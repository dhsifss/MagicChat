package mobilepush

import (
	"strings"
	"testing"
	"time"

	"app/internal/store"
	"github.com/google/uuid"
)

func TestPostgresIdentityChangeRenewalReplacesBindingAfterRestart(t *testing.T) {
	for _, mode := range []string{"fixed_channel", "client_matching"} {
		for _, failurePoint := range []string{"none", "revoke", "device_report", "endpoint_create"} {
			t.Run(mode+"/"+failurePoint, func(t *testing.T) {
				service, db, now := newPostgresPushTestService(t)
				upstream, client := newPostgresPushContract(t, db)
				service.grants.endpoints, service.gateway = client, client
				user := insertPushUser(t, db, "identity-renewal@example.test")
				sender := insertPushUser(t, db, "identity-renewal-sender@example.test")
				conversation := insertPushConversation(t, db, user, sender, now)
				var session store.UserSession
				if err := db.Where("user_id=?", user.ID).First(&session).Error; err != nil {
					t.Fatal(err)
				}
				cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "identity-renewal-token"}}}
				if mode == "client_matching" {
					cmd.ClientID = "cloud.example.clienta"
					upstream.clientChannels.Store(cmd.ClientID+":fcm:", client.options.FCMChannelID)
				}
				if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
					t.Fatal(err)
				}
				originalGrant, original := storedRegistration(t, service, db, cmd.InstallationID)
				originalEndpointID := original.EndpointIDs[0]
				upstream.renewalErrors.Store(originalEndpointID, "configuration_identity_changed")
				cmd.ExpiresAt = now.Add(2 * time.Hour)
				if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
					t.Fatalf("expected recoverable identity rejection: %v", err)
				}
				paused, retiring := storedRegistration(t, service, db, cmd.InstallationID)
				if paused.Status != GrantStatusRegistering || !retiring.RegistrationPending || !retiring.RetirementPending || retiring.InstallationID != original.InstallationID || retiring.EndpointIDs[0] != originalEndpointID {
					t.Fatalf("identity rejection did not preserve a paused retirement: status=%s binding=%+v", paused.Status, retiring)
				}
				var old contractEndpoint
				if err := db.First(&old, "id=?", originalEndpointID).Error; err != nil || old.Status != "active" {
					t.Fatalf("binding was retired before its recovery state committed: %+v %v", old, err)
				}
				upstream.beforeRevoke = func() {
					grant, binding := storedRegistration(t, service, db, cmd.InstallationID)
					if grant.Status != GrantStatusRegistering || !binding.RetirementPending {
						t.Errorf("retirement preceded committed identity-change pause: status=%s binding=%+v", grant.Status, binding)
					}
				}
				insertPostgresPushMessage(t, db, conversation.ID, sender.ID, 1, now)
				if _, err := service.ExpandEventBatch(t.Context(), 25); err != nil {
					t.Fatal(err)
				}
				service.now = func() time.Time { return now.Add(10 * time.Second) }
				if _, err := service.DispatchBatch(t.Context(), 25); err != nil {
					t.Fatal(err)
				}
				var waiting store.MobilePushJob
				if err := db.Take(&waiting).Error; err != nil || waiting.Status != JobStatusRetry || waiting.LastErrorCode != "grant_registering" || waiting.Attempts != 0 || len(waiting.SubmissionTokenCiphertext) != 0 {
					t.Fatalf("notification was not held during retirement: %+v %v", waiting, err)
				}
				switch failurePoint {
				case "revoke":
					upstream.failRevoke.Store(true)
				case "device_report":
					upstream.failDeviceReport.Store(true)
				case "endpoint_create":
					upstream.failCreateRequest.Store(true)
				}
				if failurePoint != "none" {
					if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
						t.Fatalf("expected interrupted replacement: %v", err)
					}
					grant, binding := storedRegistration(t, service, db, cmd.InstallationID)
					if grant.Status != GrantStatusRegistering || !binding.RegistrationPending {
						t.Fatalf("interrupted replacement lost its pause: status=%s binding=%+v", grant.Status, binding)
					}
					if failurePoint != "endpoint_create" && (!binding.RetirementPending || binding.InstallationID != original.InstallationID) {
						t.Fatalf("interrupted retirement lost its cleanup reference: %+v", binding)
					}
				}
				upstream.failRevoke.Store(false)
				restarted, err := NewService(Dependencies{DB: db, Cipher: service.cipher, Gateway: client, Enabled: true, Now: func() time.Time { return now.Add(20 * time.Second) }})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := restarted.RegisterGrant(t.Context(), cmd); err != nil {
					t.Fatalf("replacement did not recover after restart: %v", err)
				}
				grant, current := storedRegistration(t, restarted, db, cmd.InstallationID)
				if grant.ID != originalGrant.ID || grant.GatewayGrantID != cmd.GatewayGrantID || grant.Status != GrantStatusActive || current.RegistrationPending || current.RetirementPending || current.InstallationID == original.InstallationID || len(current.EndpointIDs) != 1 || current.EndpointIDs[0] == originalEndpointID {
					t.Fatalf("identity replacement did not activate a fresh endpoint: status=%s binding=%+v", grant.Status, current)
				}
				var fresh contractEndpoint
				if err := db.First(&fresh, "id=?", current.EndpointIDs[0]).Error; err != nil || fresh.ConfigID != client.options.FCMChannelID || fresh.Status != "active" {
					t.Fatalf("replacement changed the configured channel: %+v %v", fresh, err)
				}
				if err := db.First(&old, "id=?", originalEndpointID).Error; err != nil || old.Status != "revoked" {
					t.Fatalf("old identity remained active: %+v %v", old, err)
				}
				if _, err := restarted.DispatchBatch(t.Context(), 25); err != nil {
					t.Fatal(err)
				}
				var sent store.MobilePushJob
				if err := db.First(&sent, "id=?", waiting.ID).Error; err != nil || sent.Status != JobStatusSent || !sent.ExpiresAt.Equal(waiting.ExpiresAt) {
					t.Fatalf("queued notification did not recover within its original TTL: %+v %v", sent, err)
				}
				// A later normal renewal must reuse the replacement, not retire again.
				upstream.beforeRevoke = nil
				cmd.ExpiresAt = now.Add(3 * time.Hour)
				if _, err := restarted.RegisterGrant(t.Context(), cmd); err != nil {
					t.Fatal(err)
				}
				_, renewed := storedRegistration(t, restarted, db, cmd.InstallationID)
				if renewed.InstallationID != current.InstallationID || renewed.EndpointIDs[0] != current.EndpointIDs[0] || upstream.puts.Load() != 2 {
					t.Fatal("normal renewal replaced the recovered binding or retried the rejected endpoint")
				}
			})
		}
	}
}

func TestPostgresTemporaryRenewalConflictKeepsExistingBinding(t *testing.T) {
	service, db, now := newPostgresPushTestService(t)
	upstream, client := newPostgresPushContract(t, db)
	service.grants.endpoints = client
	user := insertPushUser(t, db, "renewal-conflict@example.test")
	var session store.UserSession
	if err := db.Where("user_id=?", user.ID).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: strings.Repeat("t", 32)}}}
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	_, original := storedRegistration(t, service, db, cmd.InstallationID)
	upstream.renewalErrors.Store(original.EndpointIDs[0], "endpoint_version_conflict")
	if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
		t.Fatal(err)
	}
	grant, binding := storedRegistration(t, service, db, cmd.InstallationID)
	if grant.Status != GrantStatusActive || binding.RetirementPending || binding.RegistrationPending {
		t.Fatalf("temporary version conflict retired a usable binding: status=%s binding=%+v", grant.Status, binding)
	}
	upstream.renewalErrors.Delete(original.EndpointIDs[0])
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	_, renewed := storedRegistration(t, service, db, cmd.InstallationID)
	if renewed.InstallationID != original.InstallationID || renewed.EndpointIDs[0] != original.EndpointIDs[0] {
		t.Fatal("temporary conflict unnecessarily replaced an endpoint")
	}
}

func TestPostgresIdentityRetirementRequiresCommittedPauseAndSupportsRevoke(t *testing.T) {
	service, db, now := newPostgresPushTestService(t)
	upstream, client := newPostgresPushContract(t, db)
	service.grants.endpoints = client
	user := insertPushUser(t, db, "identity-pause-commit@example.test")
	var session store.UserSession
	if err := db.Where("user_id=?", user.ID).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "identity-pause-token"}}}
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	_, original := storedRegistration(t, service, db, cmd.InstallationID)
	upstream.renewalErrors.Store(original.EndpointIDs[0], "configuration_identity_changed")
	if err := db.Exec(`CREATE FUNCTION reject_identity_pause() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'pause storage unavailable'; END $$;
 CREATE TRIGGER reject_identity_pause BEFORE UPDATE ON user_push_grants FOR EACH ROW
 WHEN (OLD.status='active' AND NEW.status='registering') EXECUTE FUNCTION reject_identity_pause()`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterGrant(t.Context(), cmd); err == nil {
		t.Fatal("pause persistence failure was hidden")
	}
	grant, unchanged := storedRegistration(t, service, db, cmd.InstallationID)
	if grant.Status != GrantStatusActive || unchanged.RetirementPending || unchanged.InstallationID != original.InstallationID {
		t.Fatalf("failed pause left a partial retirement: status=%s binding=%+v", grant.Status, unchanged)
	}
	var endpoint contractEndpoint
	if err := db.First(&endpoint, "id=?", original.EndpointIDs[0]).Error; err != nil || endpoint.Status != "active" {
		t.Fatalf("uncommitted pause retired an endpoint: %+v %v", endpoint, err)
	}
	if err := db.Exec(`DROP TRIGGER reject_identity_pause ON user_push_grants`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
		t.Fatalf("identity rejection did not become recoverable: %v", err)
	}
	grant, retiring := storedRegistration(t, service, db, cmd.InstallationID)
	if grant.Status != GrantStatusRegistering || !retiring.RetirementPending {
		t.Fatal("identity retirement was not persisted after storage recovered")
	}
	if err := service.RevokeGrant(t.Context(), cmd.UserID, cmd.InstallationID, cmd.GatewayGrantID); err != nil {
		t.Fatalf("a pending identity replacement blocked logout/revocation: %v", err)
	}
	var active int64
	if err := db.Model(&contractEndpoint{}).Where("status='active'").Count(&active).Error; err != nil || active != 0 {
		t.Fatalf("revocation left active endpoints: %d %v", active, err)
	}
}
