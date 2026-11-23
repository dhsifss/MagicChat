package mobilepush

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"app/internal/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Cleanup has its own loop so slow upstream revocations cannot delay messages.
// RunWorker cancels and joins this loop during shutdown.
func (s *Service) runEndpointRevocationWorker(ctx context.Context) {
	ticker := time.NewTicker(workerPollInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if _, err := s.ProcessEndpointRevocations(ctx, workerBatchSize); err != nil && ctx.Err() == nil {
			slog.Error("revoke mobile push endpoints", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// drainInstallationRevocations runs under the same installation lock as local
// registration. A fresh login cannot race cleanup of an earlier binding.
func (s *grantService) drainInstallationRevocations(ctx context.Context, tx *gorm.DB, installationID string) error {
	if s.endpoints == nil {
		return nil
	}
	var pending []store.MobilePushEndpointRevocation
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("installation_id = ?", installationID).Order("created_at, id").Find(&pending).Error; err != nil {
		return err
	}
	for _, item := range pending {
		reference, err := s.cipher.Decrypt(item.SendTokenCiphertext, []byte(item.ID))
		if err != nil {
			return err
		}
		if isPushBinding(reference) {
			if err := s.endpoints.RevokeEndpoint(ctx, reference); err != nil {
				return err
			}
		}
		if err := tx.Delete(&item).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) ProcessEndpointRevocations(ctx context.Context, limit int) (int, error) {
	if !s.enabled || s.grants.endpoints == nil || limit <= 0 {
		return 0, nil
	}
	now := s.now().UTC()
	var pending []store.MobilePushEndpointRevocation
	if err := s.db.WithContext(ctx).Where("next_attempt_at <= ?", now).Order("next_attempt_at, created_at, id").Limit(limit).Find(&pending).Error; err != nil {
		return 0, err
	}
	processed := 0
	var failures error
	for _, candidate := range pending {
		var attemptErr error
		completed := false
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := store.LockMobilePushInstallation(tx, candidate.InstallationID); err != nil {
				return err
			}
			var item store.MobilePushEndpointRevocation
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND next_attempt_at <= ?", candidate.ID, now).First(&item).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			reference, err := s.cipher.Decrypt(item.SendTokenCiphertext, []byte(item.ID))
			if err == nil && isPushBinding(reference) {
				attemptCtx, cancel := context.WithTimeout(ctx, gatewaySendTimeout)
				err = s.grants.endpoints.RevokeEndpoint(attemptCtx, reference)
				cancel()
			}
			if err != nil {
				attemptErr = err
				// Persist retry scheduling even when decryption or the upstream fails.
				// Old broken rows leave the due set, allowing later cleanup to advance.
				backoff := time.Duration(1<<min(item.Attempts, 10)) * 30 * time.Second
				return tx.Model(&item).Updates(map[string]any{"attempts": item.Attempts + 1, "next_attempt_at": now.Add(backoff)}).Error
			}
			completed = true
			return tx.Delete(&item).Error
		})
		failures = errors.Join(failures, err, attemptErr)
		if ctx.Err() != nil {
			return processed, failures
		}
		if err == nil && completed {
			processed++
		}
	}
	return processed, failures
}
