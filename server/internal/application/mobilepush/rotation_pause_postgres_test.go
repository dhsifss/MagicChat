package mobilepush

import (
	"strings"
	"testing"
	"time"

	"app/internal/store"
	"github.com/google/uuid"
)

func TestPostgresRotationCommitsPauseBeforeRetiringBinding(t *testing.T) {
	for _, failurePoint := range []string{"device_report", "revoke", "claimed_device_report"} {
		t.Run(failurePoint, func(t *testing.T) {
			service, db, now := newPostgresPushTestService(t)
			upstream, client := newPostgresPushContract(t, db)
			service.grants.endpoints, service.gateway = client, client
			user := insertPushUser(t, db, "rotation-pause@example.test")
			sender := insertPushUser(t, db, "rotation-pause-sender@example.test")
			conversation := insertPushConversation(t, db, user, sender, now)
			var session store.UserSession
			if err := db.Where("user_id = ?", user.ID).First(&session).Error; err != nil {
				t.Fatal(err)
			}
			cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "rotation-pause-token"}}}
			if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
				t.Fatal(err)
			}
			originalGrant, originalBinding := storedRegistration(t, service, db, cmd.InstallationID)
			insertPostgresPushMessage(t, db, conversation.ID, sender.ID, 1, now)
			if _, err := service.ExpandEventBatch(t.Context(), 25); err != nil {
				t.Fatal(err)
			}
			var claimed []store.MobilePushJob
			if failurePoint == "claimed_device_report" {
				service.now = func() time.Time { return now.Add(10 * time.Second) }
				var err error
				claimed, err = service.claimJobs(t.Context(), 25)
				if err != nil || len(claimed) != 1 || claimed[0].Grant.Status != GrantStatusActive {
					t.Fatalf("claim before rotation: jobs=%d error=%v", len(claimed), err)
				}
				service.now = func() time.Time { return now }
			}
			// Use a separate database connection from the transaction performing
			// retirement: this must see the pause committed before the DELETE.
			upstream.beforeRevoke = func() {
				var grant store.UserPushGrant
				if err := db.First(&grant, "id = ?", originalGrant.ID).Error; err != nil {
					t.Errorf("read committed grant before retirement: %v", err)
					return
				}
				if grant.Status != GrantStatusRegistering {
					t.Errorf("retirement preceded committed pause: %s", grant.Status)
				}
			}
			client.options.FCMChannelID = strings.Repeat("4", 32)
			if failurePoint != "revoke" {
				upstream.failDeviceReport.Store(true)
			} else {
				upstream.failRevoke.Store(true)
			}
			if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
				t.Fatalf("rotation failure: %v", err)
			}
			grant, binding := storedRegistration(t, service, db, cmd.InstallationID)
			if grant.Status != GrantStatusRegistering || !binding.RegistrationPending || binding.InstallationID != originalBinding.InstallationID || len(binding.EndpointIDs) != 1 || binding.EndpointIDs[0] != originalBinding.EndpointIDs[0] {
				t.Fatal("rotation failure lost its committed pause or old cleanup reference")
			}
			if failurePoint != "revoke" {
				var count int64
				if err := db.Model(&contractEndpoint{}).Where("status = 'active'").Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("expected retired upstream binding: %d %v", count, err)
				}
			}
			insertPostgresPushMessage(t, db, conversation.ID, sender.ID, 2, now)
			if _, err := service.ExpandEventBatch(t.Context(), 25); err != nil {
				t.Fatal(err)
			}
			service.now = func() time.Time { return now.Add(10 * time.Second) }
			for _, job := range claimed {
				if err := service.dispatchJob(t.Context(), job); err != nil {
					t.Fatalf("dispatch pre-rotation claim: %v", err)
				}
			}
			if _, err := service.DispatchBatch(t.Context(), 25); err != nil {
				t.Fatal(err)
			}
			var jobs []store.MobilePushJob
			if err := db.Find(&jobs).Error; err != nil || len(jobs) != 2 {
				t.Fatalf("notifications lost during failed rotation: count=%d error=%v", len(jobs), err)
			}
			for _, job := range jobs {
				if job.Status != JobStatusRetry || job.LastErrorCode != "grant_registering" || job.Attempts != 0 || len(job.SubmissionTokenCiphertext) != 0 {
					t.Fatalf("job sent during failed rotation: status=%s code=%s", job.Status, job.LastErrorCode)
				}
			}
			var submissions int64
			if err := db.Model(&contractSubmission{}).Count(&submissions).Error; err != nil || submissions != 0 {
				t.Fatalf("notifications submitted to retired binding: %d %v", submissions, err)
			}
			upstream.failRevoke.Store(false)
			restarted, err := NewService(Dependencies{DB: db, Cipher: service.cipher, Gateway: client, Enabled: true, Now: func() time.Time { return now.Add(20 * time.Second) }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.RegisterGrant(t.Context(), cmd); err != nil {
				t.Fatalf("recover paused rotation after restart: %v", err)
			}
			if _, err := restarted.DispatchBatch(t.Context(), 25); err != nil {
				t.Fatal(err)
			}
			jobs = nil
			if err := db.Find(&jobs).Error; err != nil {
				t.Fatal(err)
			}
			for _, job := range jobs {
				if job.Status != JobStatusSent {
					t.Fatalf("notification did not recover: status=%s code=%s", job.Status, job.LastErrorCode)
				}
			}
			grant, binding = storedRegistration(t, restarted, db, cmd.InstallationID)
			if grant.ID != originalGrant.ID || grant.GatewayGrantID != cmd.GatewayGrantID || grant.Status != GrantStatusActive || binding.InstallationID == originalBinding.InstallationID {
				t.Fatal("rotation recovery lost grant identity or reused retired installation")
			}
		})
	}
}
