package mobilepush

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"app/internal/store"
	"github.com/google/uuid"
)

type blockedCleanupClient struct {
	started chan struct{}
	once    sync.Once
}

func (c *blockedCleanupClient) RegisterEndpoint(context.Context, RegisterGrantCommand) (string, error) {
	panic("unexpected registration")
}

func (c *blockedCleanupClient) RevokeEndpoint(ctx context.Context, _ string) error {
	c.once.Do(func() { close(c.started) })
	<-ctx.Done()
	return ctx.Err()
}

type notifyingGateway struct{ sent chan struct{} }

func (g *notifyingGateway) Send(context.Context, string, string, string, NotificationRequest) error {
	g.sent <- struct{}{}
	return nil
}

func TestPostgresSlowEndpointCleanupDoesNotBlockNotifications(t *testing.T) {
	service, db, now := newPostgresPushTestService(t)
	user := insertPushUser(t, db, "cleanup-recipient@example.test")
	sender := insertPushUser(t, db, "cleanup-sender@example.test")
	conversation := insertPushConversation(t, db, user, sender, now)
	if _, err := registerPushTestGrant(t, service, db, RegisterGrantCommand{
		UserID: user.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(),
		SendToken: strings.Repeat("s", 43), Platform: "ios", ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	insertPostgresPushMessage(t, db, conversation.ID, sender.ID, 1, now.Add(-time.Minute))
	if _, err := service.ExpandEventBatch(t.Context(), 25); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&store.MobilePushJob{}).Where("user_id = ?", user.ID).Update("next_attempt_at", now.Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	insertPostgresPushMessage(t, db, conversation.ID, sender.ID, 2, now)
	for range workerBatchSize {
		id := uuid.NewString()
		ciphertext, err := service.cipher.Encrypt(testBinding, []byte(id))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&store.MobilePushEndpointRevocation{
			ID: id, InstallationID: uuid.NewString(), SendTokenCiphertext: ciphertext,
			NextAttemptAt: now.Add(-time.Second), CreatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	cleanup := &blockedCleanupClient{started: make(chan struct{})}
	service.grants.endpoints = cleanup
	gateway := &notifyingGateway{sent: make(chan struct{}, 1)}
	service.gateway = gateway
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); service.RunWorker(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("worker did not cancel and join blocked cleanup")
		}
	}()
	select {
	case <-cleanup.started:
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup never started")
	}
	select {
	case <-gateway.sent:
	case <-time.After(3 * time.Second):
		t.Fatal("blocked cleanup prevented notification dispatch")
	}
	var count int64
	if err := db.Model(&store.MobilePushJob{}).Where("user_id = ?", user.ID).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("blocked cleanup prevented message expansion: jobs=%d error=%v", count, err)
	}
}

func TestPostgresRegisteringGrantRetainsNotifications(t *testing.T) {
	for _, outcome := range []string{"complete", "ttl_expired", "disabled"} {
		t.Run(outcome, func(t *testing.T) {
			service, db, now := newPostgresPushTestService(t)
			_, client := newPostgresPushContract(t, db)
			service.grants.endpoints = client
			service.gateway = client
			user := insertPushUser(t, db, "registering-recipient@example.test")
			sender := insertPushUser(t, db, "registering-sender@example.test")
			conversation := insertPushConversation(t, db, user, sender, now)
			var session store.UserSession
			if err := db.Where("user_id = ?", user.ID).First(&session).Error; err != nil {
				t.Fatal(err)
			}
			cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "registering-device-token"}}}
			if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
				t.Fatal(err)
			}
			// Queue one message before rotation and another while registration pauses.
			insertPostgresPushMessage(t, db, conversation.ID, sender.ID, 1, now)
			if _, err := service.ExpandEventBatch(t.Context(), 25); err != nil {
				t.Fatal(err)
			}
			client.options.FCMChannelID = strings.Repeat("4", 32)
			if _, err := service.grants.register(t.Context(), cmd, true); err != nil {
				t.Fatal(err)
			}
			insertPostgresPushMessage(t, db, conversation.ID, sender.ID, 2, now)
			if _, err := service.ExpandEventBatch(t.Context(), 25); err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := db.Model(&store.MobilePushJob{}).Count(&count).Error; err != nil || count != 2 {
				t.Fatalf("fanout lost notifications during registration: count=%d error=%v", count, err)
			}
			// Waiting longer than the normal retry allowance must not fail jobs.
			for attempt := range maxJobAttempts + 2 {
				current := now.Add(pushDeliveryDelay + time.Duration(attempt)*workerPollInterval)
				service.now = func() time.Time { return current }
				if processed, err := service.DispatchBatch(t.Context(), 25); err != nil || processed != 2 {
					t.Fatalf("waiting dispatch: processed=%d error=%v", processed, err)
				}
			}
			var jobs []store.MobilePushJob
			if err := db.Find(&jobs).Error; err != nil {
				t.Fatal(err)
			}
			for _, job := range jobs {
				if job.Status != JobStatusRetry || job.Attempts != 0 || job.LastErrorCode != "grant_registering" || job.LockedAt != nil || job.LockToken != "" {
					t.Fatalf("registration wait consumed or locked notification: %+v", job)
				}
			}
			wantStatus, wantCode := JobStatusSent, ""
			switch outcome {
			case "complete":
				if _, err := service.grants.register(t.Context(), cmd, false); err != nil {
					t.Fatal(err)
				}
				service.now = func() time.Time { return now.Add(time.Minute) }
			case "ttl_expired":
				service.now = func() time.Time { return now.Add(pushJobTTL + time.Second) }
				wantStatus, wantCode = JobStatusExpired, "ttl_expired"
			case "disabled":
				if err := db.Model(&store.UserPushGrant{}).Where("installation_id = ?", cmd.InstallationID).Update("status", GrantStatusDisabled).Error; err != nil {
					t.Fatal(err)
				}
				service.now = func() time.Time { return now.Add(time.Minute) }
				wantStatus, wantCode = JobStatusFailed, "grant_inactive"
			}
			if _, err := service.DispatchBatch(t.Context(), 25); err != nil {
				t.Fatal(err)
			}
			jobs = nil
			if err := db.Find(&jobs).Error; err != nil {
				t.Fatal(err)
			}
			for _, job := range jobs {
				if job.Status != wantStatus || job.LastErrorCode != wantCode {
					t.Fatalf("notification after %s: status=%s code=%s", outcome, job.Status, job.LastErrorCode)
				}
			}
		})
	}
}
