package mobilepush

import (
	"strings"
	"testing"
	"time"

	"app/internal/store"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func storedRegistration(t *testing.T, service *Service, db *gorm.DB, installationID string) (store.UserPushGrant, pushBinding) {
	t.Helper()
	var grant store.UserPushGrant
	if err := db.Where("installation_id = ?", installationID).First(&grant).Error; err != nil {
		t.Fatal(err)
	}
	raw, err := service.cipher.Decrypt(grant.SendTokenCiphertext, []byte(grant.ID))
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := decodePushBinding(raw)
	if !ok {
		t.Fatal("invalid stored registration")
	}
	return grant, binding
}

func TestPostgresProviderReplacementWaitsForUsableEndpoints(t *testing.T) {
	for _, mode := range []string{"replace_only_provider", "keep_working_provider", "missing_replacement_channel"} {
		t.Run(mode, func(t *testing.T) {
			service, db, now := newPostgresPushTestService(t)
			upstream, client := newPostgresPushContract(t, db)
			service.grants.endpoints, service.gateway = client, client
			client.options.HuaweiChannelID = strings.Repeat("5", 32)
			upstream.channelProviders.Store(client.options.HuaweiChannelID, "huawei_hms")
			user := insertPushUser(t, db, "provider-replacement@example.test")
			sender := insertPushUser(t, db, "provider-replacement-sender@example.test")
			conversation := insertPushConversation(t, db, user, sender, now)
			var session store.UserSession
			if err := db.Where("user_id = ?", user.ID).First(&session).Error; err != nil {
				t.Fatal(err)
			}
			cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "replacement-fcm-token"}}}
			if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
				t.Fatal(err)
			}
			_, original := storedRegistration(t, service, db, cmd.InstallationID)
			insertPostgresPushMessage(t, db, conversation.ID, sender.ID, 1, now)
			if _, err := service.ExpandEventBatch(t.Context(), 25); err != nil {
				t.Fatal(err)
			}
			hms := ProviderRegistration{Provider: "huawei_hms", Token: "replacement-hms-token"}
			if mode != "keep_working_provider" {
				cmd.Registrations = []ProviderRegistration{hms}
			} else {
				cmd.Registrations = append(cmd.Registrations, hms)
			}
			wantError := "push_service_unavailable"
			if mode == "missing_replacement_channel" {
				client.options.HuaweiChannelID = ""
				wantError = "unsupported_provider"
			} else {
				upstream.failCreateRequest.Store(true)
			}
			if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != wantError {
				t.Fatalf("creation did not pause: %v", err)
			}
			grant, prepared := storedRegistration(t, service, db, cmd.InstallationID)
			wantStatus := GrantStatusRegistering
			if mode != "replace_only_provider" {
				wantStatus = GrantStatusActive
			}
			if grant.Status != wantStatus || prepared.RegistrationPending != (mode == "replace_only_provider") {
				t.Fatalf("readiness from historical endpoints: status=%s pending=%v", grant.Status, prepared.RegistrationPending)
			}
			if len(prepared.EndpointIDs) != 1 || prepared.EndpointIDs[0] != original.EndpointIDs[0] {
				t.Fatal("old endpoint reference was lost before cleanup")
			}
			insertPostgresPushMessage(t, db, conversation.ID, sender.ID, 2, now)
			if _, err := service.ExpandEventBatch(t.Context(), 25); err != nil {
				t.Fatal(err)
			}
			service.now = func() time.Time { return now.Add(10 * time.Second) }
			if _, err := service.DispatchBatch(t.Context(), 25); err != nil {
				t.Fatal(err)
			}
			var jobs []store.MobilePushJob
			if err := db.Find(&jobs).Error; err != nil || len(jobs) != 2 {
				t.Fatalf("lost provider replacement jobs: count=%d error=%v", len(jobs), err)
			}
			for _, job := range jobs {
				if mode == "replace_only_provider" {
					if job.Status != JobStatusRetry || job.LastErrorCode != "grant_registering" || job.Attempts != 0 || len(job.SubmissionTokenCiphertext) != 0 {
						t.Fatalf("notification was submitted before HMS existed: status=%s code=%s attempts=%d", job.Status, job.LastErrorCode, job.Attempts)
					}
				} else if job.Status != JobStatusSent {
					t.Fatalf("working FCM route was needlessly paused: status=%s code=%s", job.Status, job.LastErrorCode)
				}
			}
			client.options.HuaweiChannelID = strings.Repeat("5", 32)
			if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
				t.Fatalf("complete provider replacement: %v", err)
			}
			service.now = func() time.Time { return now.Add(20 * time.Second) }
			if _, err := service.DispatchBatch(t.Context(), 25); err != nil {
				t.Fatal(err)
			}
			jobs = nil
			if err := db.Find(&jobs).Error; err != nil {
				t.Fatal(err)
			}
			for _, job := range jobs {
				if job.Status != JobStatusSent {
					t.Fatalf("replacement notification not recovered: status=%s code=%s", job.Status, job.LastErrorCode)
				}
			}
			grant, _ = storedRegistration(t, service, db, cmd.InstallationID)
			if grant.Status != GrantStatusActive || grant.GatewayGrantID != cmd.GatewayGrantID {
				t.Fatal("replacement changed grant identity or stayed pending")
			}
		})
	}
}

func TestPostgresRejectedCreationDoesNotBlockChannelRecovery(t *testing.T) {
	for _, mode := range []string{"rotate_channel", "retry_same_channel", "revoke", "logout"} {
		t.Run(mode, func(t *testing.T) {
			service, db, now := newPostgresPushTestService(t)
			upstream, client := newPostgresPushContract(t, db)
			service.grants.endpoints = client
			user := insertPushUser(t, db, "rejected-creation@example.test")
			var session store.UserSession
			if err := db.Where("user_id = ?", user.ID).First(&session).Error; err != nil {
				t.Fatal(err)
			}
			cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "rejected-channel-token"}}}
			upstream.unreadyChannel.Store(client.options.FCMChannelID)
			if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
				t.Fatalf("expected admission rejection: %v", err)
			}
			grant, plan := storedRegistration(t, service, db, cmd.InstallationID)
			if len(plan.PendingCreates) != 1 || !plan.PendingCreates[0].Rejected || grant.Status != GrantStatusRegistering {
				t.Fatal("definitive rejection was rolled back")
			}
			rejectedKey := plan.PendingCreates[0].Request.IdempotencyKey
			// Settlement must survive a service restart, including session cascades.
			restarted, err := NewService(Dependencies{DB: db, Cipher: service.cipher, Gateway: client, Enabled: true, Now: func() time.Time { return now.Add(time.Minute) }})
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "rotate_channel":
				client.options.FCMChannelID = strings.Repeat("4", 32)
			case "retry_same_channel":
				upstream.unreadyChannel.Store("")
			case "revoke":
				if err := restarted.RevokeGrant(t.Context(), user.ID, cmd.InstallationID, cmd.GatewayGrantID); err != nil {
					t.Fatalf("confirmed rejection blocked revoke: %v", err)
				}
			case "logout":
				if err := db.Delete(&session).Error; err != nil {
					t.Fatal(err)
				}
			}
			if mode == "revoke" || mode == "logout" {
				if _, err := restarted.ProcessEndpointRevocations(t.Context(), 25); err != nil {
					t.Fatalf("confirmed rejection blocked logout cleanup: %v", err)
				}
				var count int64
				if err := db.Model(&store.MobilePushEndpointRevocation{}).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("rejection cleanup retained: %d %v", count, err)
				}
				if upstream.posts.Load() != 1 {
					t.Fatal("cleanup replayed definitively rejected POST")
				}
				return
			}
			if _, err := restarted.RegisterGrant(t.Context(), cmd); err != nil {
				t.Fatalf("rejected plan blocked recovery: %v", err)
			}
			grant, _ = storedRegistration(t, restarted, db, cmd.InstallationID)
			if grant.Status != GrantStatusActive || grant.GatewayGrantID != cmd.GatewayGrantID {
				t.Fatal("recovered grant is not active")
			}
			var accepted []contractRegistration
			if err := db.Find(&accepted).Error; err != nil || len(accepted) != 1 || accepted[0].Key == rejectedKey {
				t.Fatalf("recovery reused settled request: count=%d error=%v", len(accepted), err)
			}
			if upstream.posts.Load() != 2 {
				t.Fatal("recovery replayed the old channel creation")
			}
		})
	}
}

func TestPostgresRejectionAfterUnknownOutcomeRetainsCleanup(t *testing.T) {
	for _, rejection := range []string{"channel_not_ready", "invalid_endpoint_token"} {
		t.Run(rejection, func(t *testing.T) {
			service, db, now := newPostgresPushTestService(t)
			upstream, client := newPostgresPushContract(t, db)
			service.grants.endpoints = client
			user := insertPushUser(t, db, "uncertain-rejection@example.test")
			var session store.UserSession
			if err := db.Where("user_id = ?", user.ID).First(&session).Error; err != nil {
				t.Fatal(err)
			}
			cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "uncertain-rejection-token"}}}
			upstream.failCreateRequest.Store(true)
			if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
				t.Fatal(err)
			}
			if rejection == "channel_not_ready" {
				upstream.unreadyChannel.Store(client.options.FCMChannelID)
			} else {
				upstream.rejectedToken.Store(cmd.Registrations[0].Token)
			}
			if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
				t.Fatal(err)
			}
			_, plan := storedRegistration(t, service, db, cmd.InstallationID)
			if len(plan.PendingCreates) != 1 || plan.PendingCreates[0].Rejected || plan.PendingCreates[0].FirstAttempt {
				t.Fatal("later rejection discarded an uncertain earlier request")
			}
			if err := service.RevokeGrant(t.Context(), user.ID, cmd.InstallationID, cmd.GatewayGrantID); ErrorCodeOf(err) != "push_service_unavailable" {
				t.Fatalf("unresolved revocation was marked complete: %v", err)
			}
			storedRegistration(t, service, db, cmd.InstallationID)
			upstream.unreadyChannel.Store("")
			upstream.rejectedToken.Store("")
			if err := service.RevokeGrant(t.Context(), user.ID, cmd.InstallationID, cmd.GatewayGrantID); err != nil {
				t.Fatalf("could not finish uncertain cleanup: %v", err)
			}
		})
	}
}
