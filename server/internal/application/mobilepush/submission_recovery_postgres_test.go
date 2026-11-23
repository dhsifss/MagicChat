package mobilepush

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"app/internal/store"
	"github.com/google/uuid"
)

func TestPostgresCleanupFencesLateEndpointCreation(t *testing.T) {
	for _, mode := range []string{"explicit_revoke", "logout_cleanup_retry"} {
		t.Run(mode, func(t *testing.T) {
			service, db, now := newPostgresPushTestService(t)
			upstream, originalClient := newPostgresPushContract(t, db)
			client, err := NewPushServiceClient(originalClient.options, &http.Client{Timeout: 150 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			service.grants.endpoints = client
			user := insertPushUser(t, db, "late-create@example.test")
			var session store.UserSession
			if err := db.Where("user_id = ?", user.ID).First(&session).Error; err != nil {
				t.Fatal(err)
			}
			cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "late-create-token"}}}
			started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			var releaseOnce, finishOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			upstream.beforeCreate = func() {
				if calls.Add(1) == 1 {
					close(started)
					<-release
				}
			}
			upstream.afterCreate = func() {
				select {
				case <-release:
					finishOnce.Do(func() { close(finished) })
				default:
				}
			}
			if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
				t.Fatalf("timed-out creation: %v", err)
			}
			select {
			case <-started:
			default:
				t.Fatal("POST did not start")
			}
			var pending store.UserPushGrant
			if err := db.First(&pending, "installation_id = ?", cmd.InstallationID).Error; err != nil {
				t.Fatal(err)
			}
			raw, err := service.cipher.Decrypt(pending.SendTokenCiphertext, []byte(pending.ID))
			if err != nil {
				t.Fatal(err)
			}
			binding, ok := decodePushBinding(raw)
			if !ok || len(binding.PendingCreates) != 1 {
				t.Fatal("creation request not durably saved before POST")
			}
			// Logout and its durable cleanup complete while the original POST is still
			// in flight. Cleanup must settle that key before it discards its reference.
			service.grants.endpoints = originalClient
			if mode == "explicit_revoke" {
				if err := service.RevokeGrant(t.Context(), user.ID, cmd.InstallationID, cmd.GatewayGrantID); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := db.Delete(&session).Error; err != nil {
					t.Fatal(err)
				}
				upstream.failCreateRequest.Store(true)
				service.now = func() time.Time { return now.Add(time.Minute) }
				if _, err := service.ProcessEndpointRevocations(t.Context(), 25); err == nil {
					t.Fatal("failed registration replay reported cleanup complete")
				}
				var count int64
				if err := db.Model(&store.MobilePushEndpointRevocation{}).Count(&count).Error; err != nil || count != 1 {
					t.Fatalf("failed replay discarded cleanup record: %d %v", count, err)
				}
				session = store.UserSession{ID: uuid.NewString(), UserID: user.ID, TokenHash: uuid.NewString(), ExpiresAt: now.Add(time.Hour), CreatedAt: now, LastSeenAt: now}
				if err := db.Create(&session).Error; err != nil {
					t.Fatal(err)
				}
				cmd.SessionID = session.ID
			}
			service.now = func() time.Time { return now.Add(2 * time.Minute) }
			if _, err := service.ProcessEndpointRevocations(t.Context(), 25); err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := db.Model(&store.MobilePushEndpointRevocation{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("cleanup incomplete: %d %v", count, err)
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("late POST did not finish")
			}
			if err := db.Model(&contractEndpoint{}).Where("status = 'active'").Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("late creation left an active orphan: %d %v", count, err)
			}
			cmd.GatewayGrantID = uuid.NewString()
			if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
				t.Fatalf("fresh registration conflicts with late endpoint: %v", err)
			}
		})
	}
}

func TestPostgresMessageRetryKeepsSubmissionAcrossBindingRotation(t *testing.T) {
	service, db, now := newPostgresPushTestService(t)
	upstream, client := newPostgresPushContract(t, db)
	service.grants.endpoints, service.gateway = client, client
	user := insertPushUser(t, db, "retry-target@example.test")
	sender := insertPushUser(t, db, "retry-sender@example.test")
	conversation := insertPushConversation(t, db, user, sender, now)
	var session store.UserSession
	if err := db.Where("user_id = ?", user.ID).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "message-retry-token"}}}
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	insertPostgresPushMessage(t, db, conversation.ID, sender.ID, 1, now)
	if _, err := service.ExpandEventBatch(t.Context(), 25); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now.Add(10 * time.Second) }
	upstream.failMessageResponse.Store(true)
	if _, err := service.DispatchBatch(t.Context(), 25); err != nil {
		t.Fatal(err)
	}
	var job store.MobilePushJob
	if err := db.First(&job).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != JobStatusRetry || len(job.SubmissionTokenCiphertext) == 0 {
		t.Fatalf("lost submission did not retain target: %+v", job)
	}
	submitted, err := service.cipher.Decrypt(job.SubmissionTokenCiphertext, []byte(job.ID))
	if err != nil {
		t.Fatal(err)
	}
	oldBinding, ok := decodePushBinding(submitted)
	if !ok {
		t.Fatal("invalid submission target")
	}
	client.options.FCMChannelID = strings.Repeat("4", 32)
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	var grant store.UserPushGrant
	if err := db.First(&grant, "installation_id = ?", cmd.InstallationID).Error; err != nil {
		t.Fatal(err)
	}
	current, err := service.cipher.Decrypt(grant.SendTokenCiphertext, []byte(grant.ID))
	if err != nil {
		t.Fatal(err)
	}
	newBinding, _ := decodePushBinding(current)
	if oldBinding.InstallationID == newBinding.InstallationID {
		t.Fatal("test did not rotate installation")
	}
	// Recreate the worker to ensure the retry target comes from the database.
	restarted, err := NewService(Dependencies{DB: db, Cipher: service.cipher, Gateway: client, Enabled: true, Now: func() time.Time { return now.Add(20 * time.Second) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.DispatchBatch(context.Background(), 25); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&job, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != JobStatusSent {
		t.Fatalf("retry changed idempotent payload: status=%s code=%s", job.Status, job.LastErrorCode)
	}
	var submissions []contractSubmission
	if err := db.Find(&submissions).Error; err != nil || len(submissions) != 1 {
		t.Fatalf("duplicate upstream submissions: %d %v", len(submissions), err)
	}
	var body struct {
		InstallationIDs []string `json:"installation_ids"`
	}
	if err := json.Unmarshal([]byte(submissions[0].Request), &body); err != nil || len(body.InstallationIDs) != 1 || body.InstallationIDs[0] != oldBinding.InstallationID {
		t.Fatalf("original submission changed: %+v %v", body, err)
	}
}

func TestPostgresExpiredUncommittedRegistrationCanRenew(t *testing.T) {
	service, db, now := newPostgresPushTestService(t)
	_, client := newPostgresPushContract(t, db)
	service.grants.endpoints = client
	user := insertPushUser(t, db, "expired-uncommitted@example.test")
	var session store.UserSession
	if err := db.Where("user_id = ?", user.ID).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "uncommitted-token"}}}
	if _, err := service.grants.register(t.Context(), cmd, true); err != nil {
		t.Fatal(err)
	}
	var stored store.UserPushGrant
	if err := db.Where("installation_id = ?", cmd.InstallationID).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	raw, err := service.cipher.Decrypt(stored.SendTokenCiphertext, []byte(stored.ID))
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := decodePushBinding(raw)
	if !ok || len(binding.PendingCreates) != 1 {
		t.Fatal("missing persisted creation")
	}
	// Model an offline installation returning after its unsubmitted lease ended.
	binding.PendingCreates[0].Request.ExpiresAt = now.Add(-time.Hour).Truncate(time.Microsecond).Format(time.RFC3339Nano)
	rawJSON, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := service.cipher.Encrypt(string(rawJSON), []byte(stored.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&stored).Update("send_token_ciphertext", ciphertext).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatalf("expired uncommitted plan blocked renewal: %v", err)
	}
	if err := db.First(&stored, "id = ?", stored.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != GrantStatusActive || stored.GatewayGrantID != cmd.GatewayGrantID {
		t.Fatal("renewal changed grant identity")
	}
	var count int64
	if err := db.Model(&contractEndpoint{}).Where("status = 'active'").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("renewal did not create one active endpoint: %d %v", count, err)
	}
}
