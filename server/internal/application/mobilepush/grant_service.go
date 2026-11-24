package mobilepush

import (
	"context"
	"errors"
	"strings"
	"time"

	"app/internal/store"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type grantService struct {
	endpoints EndpointClient
	db        *gorm.DB
	cipher    *TokenCipher
	enabled   bool
	now       func() time.Time
}

func (s *grantService) Register(ctx context.Context, cmd RegisterGrantCommand) (Grant, error) {
	if s.db.Dialector.Name() != "postgres" {
		return s.registerOperation(ctx, cmd)
	}
	installationID, err := normalizeUUID(cmd.InstallationID)
	if err != nil {
		return Grant{}, err
	}
	var grant Grant
	err = withDatabaseLock(ctx, s.db, registrationLockKey("registration-operation:"+installationID), func(conn *gorm.DB) error {
		operation := *s
		operation.db = conn
		var err error
		grant, err = operation.registerOperation(ctx, cmd)
		return err
	})
	return grant, err
}

func (s *grantService) registerOperation(ctx context.Context, cmd RegisterGrantCommand) (Grant, error) {
	if len(cmd.HandoffTokens) > 10 {
		return Grant{}, failure("invalid_request", "推送转移信息格式错误")
	}
	for _, token := range cmd.HandoffTokens {
		if !validHandoffToken(token) {
			return Grant{}, failure("invalid_request", "推送转移信息格式错误")
		}
	}
	if len(cmd.HandoffTokens) > 0 {
		if !s.enabled {
			return Grant{}, failure("push_disabled", "当前服务未启用手机推送")
		}
		var session store.UserSession
		if err := s.db.WithContext(ctx).Where("id=? AND user_id=? AND expires_at>?", cmd.SessionID, cmd.UserID, s.now().UTC()).First(&session).Error; err != nil {
			return Grant{}, failure("unauthorized", "登录状态已失效")
		}
		client, ok := s.endpoints.(EndpointHandoffClient)
		if !ok {
			return Grant{}, failure("push_service_unavailable", "推送转移暂不可用")
		}
		if err := client.RedeemHandoffs(ctx, cmd.HandoffTokens); err != nil {
			return Grant{}, failure("push_service_unavailable", "推送转移暂不可用")
		}
	}
	// Commit a recoverable device reference before any endpoint mutation. If the
	// second transaction fails, revoke/logout can still discover every endpoint.
	if _, ok := s.endpoints.(EndpointRegistrationPreparer); ok {
		if _, err := s.register(ctx, cmd, true); err != nil {
			return Grant{}, err
		}
	}
	return s.register(ctx, cmd, false)
}

func (s *grantService) register(ctx context.Context, cmd RegisterGrantCommand, prepare bool) (Grant, error) {
	if !s.enabled {
		return Grant{}, failure("push_disabled", "当前服务未启用手机推送")
	}
	userID, err := normalizeUUID(cmd.UserID)
	if err != nil {
		return Grant{}, err
	}
	installationID, err := normalizeUUID(cmd.InstallationID)
	if err != nil {
		return Grant{}, err
	}
	gatewayGrantID, err := normalizeUUID(cmd.GatewayGrantID)
	if err != nil {
		return Grant{}, err
	}
	sessionID := ""
	if strings.TrimSpace(cmd.SessionID) != "" {
		sessionID, err = normalizeUUID(cmd.SessionID)
		if err != nil {
			return Grant{}, err
		}
	}
	cmd.SendToken = strings.TrimSpace(cmd.SendToken)
	cmd.Platform = strings.TrimSpace(cmd.Platform)
	now := s.now().UTC()
	if (s.endpoints == nil && (len(cmd.SendToken) < minSendToken || len(cmd.SendToken) > maxSendToken)) ||
		(s.endpoints != nil && !validDeviceRegistrations(cmd)) ||
		(cmd.Platform != "android" && cmd.Platform != "ios") ||
		!cmd.ExpiresAt.After(now) || cmd.ExpiresAt.After(now.Add(maxGrantLease)) {
		return Grant{}, failure("invalid_request", "推送授权格式错误")
	}

	if cmd.SessionID == "" {
		return Grant{}, failure("unauthorized", "登录状态已失效")
	}
	var stored store.UserPushGrant
	var registrationErr error
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockRegistrationKey(tx, "user:"+userID); err != nil {
			return err
		}
		if err := store.LockMobilePushInstallation(tx, installationID); err != nil {
			return err
		}
		if err := lockRegistrationKey(tx, "grant:"+gatewayGrantID); err != nil {
			return err
		}
		var session store.UserSession
		if err := tx.Where(
			"id = ? AND user_id = ? AND expires_at > ?", sessionID, userID, now,
		).First(&session).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return failure("unauthorized", "登录状态已失效")
			}
			return err
		}
		queryErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("installation_id = ?", installationID).First(&stored).Error
		switch {
		case queryErr == nil:
		case errors.Is(queryErr, gorm.ErrRecordNotFound):
			stored.ID = uuid.NewString()
			stored.CreatedAt = now
		default:
			return queryErr
		}
		var duplicateCount int64
		if err := tx.Model(&store.UserPushGrant{}).
			Where("gateway_grant_id = ? AND id <> ?", gatewayGrantID, stored.ID).
			Count(&duplicateCount).Error; err != nil {
			return err
		}
		if duplicateCount > 0 {
			return failure("grant_conflict", "推送授权已被其他安装实例使用")
		}
		wasActiveSlot := stored.ID != "" && stored.UserID == userID &&
			(stored.Status == GrantStatusActive || stored.Status == GrantStatusRegistering) && stored.ExpiresAt.After(now)
		if !wasActiveSlot {
			var userGrantCount int64
			if err := tx.Model(&store.UserPushGrant{}).
				Where("user_id = ? AND id <> ? AND status IN ? AND expires_at > ?", userID, stored.ID, []string{GrantStatusActive, GrantStatusRegistering}, now).
				Count(&userGrantCount).Error; err != nil {
				return err
			}
			if userGrantCount >= maxGrantsPerUser {
				return failure("grant_limit_reached", "当前账号绑定的推送设备数量已达上限")
			}
		}
		if s.endpoints != nil {
			if err := s.drainInstallationRevocations(ctx, tx, installationID); err != nil {
				return failure("push_service_unavailable", "推送设备清理暂不可用")
			}
			previous := ""
			if len(stored.SendTokenCiphertext) > 0 {
				previous, err = s.cipher.Decrypt(stored.SendTokenCiphertext, []byte(stored.ID))
				if err != nil {
					return err
				}
			}
			sameBinding := stored.UserID == userID && stored.GatewayGrantID == gatewayGrantID
			if _, staged := s.endpoints.(EndpointRegistrationPreparer); staged && !prepare && !sameBinding {
				return failure("grant_conflict", "推送绑定已更换，请重新同步")
			}
			if previous != "" && !sameBinding && isPushBinding(previous) {
				if err := s.endpoints.RevokeEndpoint(ctx, previous); err != nil {
					return failure("push_service_unavailable", "推送设备更新失败")
				}
			}
			cmd.SendToken = ""
			if sameBinding {
				cmd.SendToken = previous
			}
			var endpointID string
			if prepare {
				endpointID, err = s.endpoints.(EndpointRegistrationPreparer).PrepareRegistration(ctx, cmd)
			} else {
				endpointID, err = s.endpoints.RegisterEndpoint(ctx, cmd)
			}
			if err != nil {
				if prepare && errors.Is(err, errBindingRetirementNeedsPause) {
					prepared, ok := decodePushBinding(endpointID)
					if !sameBinding || !ok || !prepared.RegistrationPending {
						return failure("push_service_unavailable", "推送设备登记失败")
					}
					ciphertext, encryptErr := s.cipher.Encrypt(endpointID, []byte(stored.ID))
					if encryptErr != nil {
						return encryptErr
					}
					registrationErr = errBindingRetirementNeedsPause
					return tx.Model(&stored).Updates(map[string]any{
						"status": GrantStatusRegistering, "send_token_ciphertext": ciphertext, "updated_at": now,
					}).Error
				}
				var upstream *GatewayError
				registrationErr = failure("push_service_unavailable", "推送设备登记失败")
				if errors.As(err, &upstream) && upstream.Code == "push_channel_unavailable" {
					registrationErr = failure("unsupported_provider", "当前服务器未配置此设备的推送通道")
				}
				if errors.As(err, &upstream) && upstream.Code == "client_channel_ambiguous" {
					registrationErr = failure("push_channel_ambiguous", "推送平台存在多个匹配当前客户端的厂商配置，请联系管理员")
				}
				if prepared, ok := decodePushBinding(endpointID); !prepare && ok {
					// Commit recovery decisions before returning the error: confirmed
					// rejections and identity-change retirement must survive retries.
					ciphertext, encryptErr := s.cipher.Encrypt(endpointID, []byte(stored.ID))
					if encryptErr != nil {
						return encryptErr
					}
					updates := map[string]any{"send_token_ciphertext": ciphertext, "updated_at": now}
					if prepared.RetirementPending {
						updates["status"] = GrantStatusRegistering
					}
					return tx.Model(&stored).Updates(updates).Error
				}
				return registrationErr
			}
			if previous != "" && sameBinding {
				// Legacy grant credentials cannot identify a Push endpoint.
				if obsolete := obsoletePushBinding(previous, endpointID); obsolete != "" {
					if err := s.endpoints.RevokeEndpoint(ctx, obsolete); err != nil {
						return failure("push_service_unavailable", "推送设备更新失败")
					}
				}
			}
			cmd.SendToken = endpointID
		}
		if stored.ID != "" && (stored.UserID != userID || stored.GatewayGrantID != gatewayGrantID) {
			if err := tx.Where("grant_id = ?", stored.ID).Delete(&store.MobilePushJob{}).Error; err != nil {
				return err
			}
			if err := tx.Delete(&stored).Error; err != nil {
				return err
			}
			stored = store.UserPushGrant{ID: uuid.NewString(), CreatedAt: now}
		}
		ciphertext, err := s.cipher.Encrypt(cmd.SendToken, []byte(stored.ID))
		if err != nil {
			return err
		}
		stored.UserID = userID
		stored.SessionID = sessionID
		stored.InstallationID = installationID
		stored.GatewayGrantID = gatewayGrantID
		stored.SendTokenCiphertext = ciphertext
		stored.Platform = cmd.Platform
		sessionExpiry := session.ExpiresAt.UTC()
		if session.RefreshTokenHash != nil && session.RefreshExpiresAt != nil && session.RefreshAbsoluteExpiresAt != nil {
			sessionExpiry = minTime(*session.RefreshExpiresAt, *session.RefreshAbsoluteExpiresAt)
		}
		stored.ExpiresAt = minTime(cmd.ExpiresAt.UTC(), sessionExpiry)
		if !prepare {
			stored.Status = GrantStatusActive
		} else if prepared, ok := decodePushBinding(cmd.SendToken); stored.Status != GrantStatusActive || (ok && (prepared.RegistrationPending || len(prepared.EndpointIDs) == 0)) {
			stored.Status = GrantStatusRegistering
		}
		stored.LastSeenAt = now
		stored.UpdatedAt = now
		return tx.Save(&stored).Error
	})
	if err != nil {
		return Grant{}, err
	}
	if registrationErr != nil {
		if errors.Is(registrationErr, errBindingRetirementNeedsPause) {
			// The pause and old cleanup reference are now durable. Reacquire all
			// registration locks before any upstream retirement or device report.
			return s.register(ctx, cmd, prepare)
		}
		return Grant{}, registrationErr
	}
	result := Grant{
		InstallationID: stored.InstallationID, GatewayGrantID: stored.GatewayGrantID,
		Platform: stored.Platform, ExpiresAt: stored.ExpiresAt,
	}
	if raw, err := s.cipher.Decrypt(stored.SendTokenCiphertext, []byte(stored.ID)); err == nil {
		if binding, ok := decodePushBinding(raw); ok {
			result.HandoffToken = binding.HandoffToken
		}
	}
	return result, nil
}

func (s *grantService) Revoke(ctx context.Context, userID, installationID, gatewayGrantID string) error {
	userID, err := normalizeUUID(userID)
	if err != nil {
		return err
	}
	installationID, err = normalizeUUID(installationID)
	if err != nil {
		return err
	}
	gatewayGrantID, err = normalizeUUID(gatewayGrantID)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := store.LockMobilePushInstallation(tx, installationID); err != nil {
			return err
		}
		if s.endpoints != nil {
			var stored store.UserPushGrant
			err := tx.Where("user_id = ? AND installation_id = ? AND gateway_grant_id = ?", userID, installationID, gatewayGrantID).First(&stored).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			endpointID, err := s.cipher.Decrypt(stored.SendTokenCiphertext, []byte(stored.ID))
			if err != nil {
				return err
			}
			if isPushBinding(endpointID) {
				if err := s.endpoints.RevokeEndpoint(ctx, endpointID); err != nil {
					return failure("push_service_unavailable", "推送设备注销失败")
				}
			}
		}
		return tx.Where(
			"user_id = ? AND installation_id = ? AND gateway_grant_id = ?",
			userID, installationID, gatewayGrantID,
		).Delete(&store.UserPushGrant{}).Error
	})
}

func minTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}
	return second
}
