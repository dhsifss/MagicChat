package mobilepush

import (
	"strings"
	"sync"
	"testing"
	"time"

	"app/internal/store"
	"github.com/google/uuid"
)

func TestPostgresDispatchLocksBindingThroughSubmission(t *testing.T) {
	service, db, now := newPostgresPushTestService(t)
	upstream, client := newPostgresPushContract(t, db)
	service.grants.endpoints, service.gateway = client, client
	user := insertPushUser(t, db, "dispatch-lock@example.test")
	sender := insertPushUser(t, db, "dispatch-lock-sender@example.test")
	conversation := insertPushConversation(t, db, user, sender, now)
	var session store.UserSession
	if err := db.Where("user_id = ?", user.ID).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "dispatch-lock-token"}}}
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	insertPostgresPushMessage(t, db, conversation.ID, sender.ID, 1, now)
	if _, err := service.ExpandEventBatch(t.Context(), 25); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now.Add(10 * time.Second) }
	claimed, err := service.claimJobs(t.Context(), 25)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: jobs=%d error=%v", len(claimed), err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseSend := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseSend()
	upstream.beforeMessage = func() {
		// The guard transaction must not delay the submission target's commit.
		var job store.MobilePushJob
		if err := db.First(&job, "id = ?", claimed[0].ID).Error; err != nil {
			t.Errorf("read pinned submission: %v", err)
		} else if len(job.SubmissionTokenCiphertext) == 0 {
			t.Error("submission target not committed before HTTP")
		}
		close(started)
		<-release
	}
	upstream.beforeRevoke = func() {
		var job store.MobilePushJob
		if err := db.First(&job, "id = ?", claimed[0].ID).Error; err != nil {
			t.Errorf("read job before retirement: %v", err)
		} else if job.Status != JobStatusSent {
			t.Errorf("retirement overtook dispatch: %s", job.Status)
		}
	}
	dispatched := make(chan struct{})
	var dispatchErr error
	go func() {
		defer close(dispatched)
		dispatchErr = service.dispatchJob(t.Context(), claimed[0])
	}()
	defer func() {
		releaseSend()
		select {
		case <-dispatched:
		case <-time.After(3 * time.Second):
			t.Error("dispatch failed to finish")
		}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch did not reach HTTP")
	}
	options := client.options
	options.FCMChannelID = strings.Repeat("4", 32)
	rotationClient, err := NewPushServiceClient(options, nil)
	if err != nil {
		t.Fatal(err)
	}
	rotator, err := NewService(Dependencies{DB: db, Cipher: service.cipher, Gateway: rotationClient, Enabled: true, Now: func() time.Time { return now.Add(10 * time.Second) }})
	if err != nil {
		t.Fatal(err)
	}
	rotated := make(chan struct{})
	var rotationErr error
	go func() {
		defer close(rotated)
		_, rotationErr = rotator.RegisterGrant(t.Context(), cmd)
	}()
	defer func() {
		releaseSend()
		select {
		case <-rotated:
			if rotationErr != nil {
				t.Errorf("rotation: %v", rotationErr)
			}
		case <-time.After(3 * time.Second):
			t.Error("rotation failed to finish")
		}
	}()
	// Observe a real blocked PostgreSQL advisory lock rather than relying on
	// the relative timing of two goroutines.
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int64
		if err := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND wait_event = 'advisory' AND pid <> pg_backend_pid()").Scan(&waiting).Error; err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case <-rotated:
			t.Fatalf("rotation did not wait for submission: %v", rotationErr)
		case <-deadline:
			t.Fatal("rotation never waited for the dispatch installation lock")
		case <-ticker.C:
		}
	}
	releaseSend()
	select {
	case <-dispatched:
		if dispatchErr != nil {
			t.Fatal(dispatchErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch did not complete")
	}
	select {
	case <-rotated:
		if rotationErr != nil {
			t.Fatal(rotationErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rotation did not resume")
	}
	grant, _ := storedRegistration(t, rotator, db, cmd.InstallationID)
	if grant.Status != GrantStatusActive {
		t.Fatal("rotation did not restore active grant")
	}
}
