package mobilepush

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"app/internal/store"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func TestPostgresPushSessionLockReleasesAfterCancellation(t *testing.T) {
	_, db, _ := newPostgresPushTestService(t)
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	ctx, cancel := context.WithCancel(t.Context())
	err = withDatabaseLock(ctx, db, registrationLockKey("cancel-lock-test"), func(conn *gorm.DB) error { cancel(); return ctx.Err() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation result: %v", err)
	}
	var locks int64
	if err := db.Raw("SELECT count(*) FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory'").Scan(&locks).Error; err != nil || locks != 0 {
		t.Fatalf("pooled connection retained session lock: count=%d error=%v", locks, err)
	}
	if pool.Stats().InUse != 0 {
		t.Fatal("canceled operation retained checked-out connection")
	}
}

type poolTestEndpoints struct{}

func (poolTestEndpoints) PrepareRegistration(context.Context, RegisterGrantCommand) (string, error) {
	return testBinding, nil
}
func (poolTestEndpoints) RegisterEndpoint(context.Context, RegisterGrantCommand) (string, error) {
	return testBinding, nil
}
func (poolTestEndpoints) RevokeEndpoint(context.Context, string) error { return nil }

func TestPostgresPushOperationsDoNotBorrowNestedConnections(t *testing.T) {
	for _, poolSize := range []int{1, 40} {
		t.Run(fmt.Sprint(poolSize), func(t *testing.T) {
			service, db, now := newPostgresPushTestService(t)
			service.grants.endpoints = poolTestEndpoints{}
			user := insertPushUser(t, db, "pool-concurrency@example.test")
			sender := insertPushUser(t, db, "pool-sender@example.test")
			conversation := insertPushConversation(t, db, user, sender, now)
			var session store.UserSession
			if err := db.Where("user_id=?", user.ID).First(&session).Error; err != nil {
				t.Fatal(err)
			}
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(poolSize)
			pool.SetMaxIdleConns(poolSize)
			cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "pool-token"}}}
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			start := make(chan struct{})
			results := make(chan error, 40)
			var group sync.WaitGroup
			for range 40 {
				group.Add(1)
				go func() { defer group.Done(); <-start; _, err := service.RegisterGrant(ctx, cmd); results <- err }()
			}
			close(start)
			if err := pool.PingContext(ctx); err != nil {
				t.Errorf("registration traffic blocked unrelated database work: %v", err)
			}
			group.Wait()
			close(results)
			for err := range results {
				if err != nil {
					t.Errorf("registration with %d connections: %v", poolSize, err)
				}
			}
			if ctx.Err() != nil {
				t.Fatal("registration exhausted the connection pool")
			}
			insertPostgresPushMessage(t, db, conversation.ID, sender.ID, 1, now)
			if _, err := service.ExpandEventBatch(ctx, 25); err != nil {
				t.Fatal(err)
			}
			service.now = func() time.Time { return now.Add(10 * time.Second) }
			if _, err := service.DispatchBatch(ctx, 25); err != nil {
				t.Fatal(err)
			}
			var job store.MobilePushJob
			if err := db.First(&job).Error; err != nil || job.Status != JobStatusSent {
				t.Fatalf("dispatch needs a second pool connection: status=%s error=%v", job.Status, err)
			}
			if pool.Stats().InUse != 0 {
				t.Fatal("push operation left a connection checked out")
			}
		})
	}
}

func TestPostgresTemporaryRoutingFailureRetriesUntilTTL(t *testing.T) {
	for _, outcome := range []string{"recovery", "expiry"} {
		t.Run(outcome, func(t *testing.T) {
			service, db, now := newPostgresPushTestService(t)
			upstream, client := newPostgresPushContract(t, db)
			service.grants.endpoints, service.gateway = client, client
			user := insertPushUser(t, db, "routing-recovery@example.test")
			sender := insertPushUser(t, db, "routing-sender@example.test")
			conversation := insertPushConversation(t, db, user, sender, now)
			if _, err := registerPushTestGrant(t, service, db, RegisterGrantCommand{UserID: user.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "routing-token"}}}); err != nil {
				t.Fatal(err)
			}
			insertPostgresPushMessage(t, db, conversation.ID, sender.ID, 1, now)
			if _, err := service.ExpandEventBatch(t.Context(), 25); err != nil {
				t.Fatal(err)
			}
			upstream.routingUnavailable.Store(true)
			current := now.Add(10 * time.Second)
			var job store.MobilePushJob
			for range maxJobAttempts {
				service.now = func() time.Time { return current }
				if _, err := service.DispatchBatch(t.Context(), 25); err != nil {
					t.Fatal(err)
				}
				if err := db.First(&job).Error; err != nil {
					t.Fatal(err)
				}
				if job.Status != JobStatusRetry || job.LastErrorCode != "no_eligible_endpoint" {
					t.Fatalf("temporary route failure became terminal: status=%s code=%s", job.Status, job.LastErrorCode)
				}
				current = job.NextAttemptAt
			}
			if outcome == "recovery" {
				upstream.routingUnavailable.Store(false)
			} else {
				current = job.ExpiresAt
			}
			service.now = func() time.Time { return current }
			if _, err := service.DispatchBatch(t.Context(), 25); err != nil {
				t.Fatal(err)
			}
			if err := db.First(&job).Error; err != nil {
				t.Fatal(err)
			}
			want := JobStatusSent
			if outcome == "expiry" {
				want = JobStatusExpired
			}
			if job.Status != want {
				t.Fatalf("routing %s: status=%s code=%s", outcome, job.Status, job.LastErrorCode)
			}
			var submissions int64
			if err := db.Model(&contractSubmission{}).Count(&submissions).Error; err != nil {
				t.Fatal(err)
			}
			if outcome == "recovery" && submissions != 1 {
				t.Fatal("routing retry did not keep one submission")
			}
			if strings.Contains(job.LastErrorCode, "conflict") {
				t.Fatal("routing retry changed payload")
			}
		})
	}
}
