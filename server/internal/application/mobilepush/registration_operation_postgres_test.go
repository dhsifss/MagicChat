package mobilepush

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"app/internal/store"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type observedRegistrationClient struct {
	*HTTPPushServiceClient
	prepared  func()
	completed func()
}

func (c *observedRegistrationClient) PrepareRegistration(ctx context.Context, cmd RegisterGrantCommand) (string, error) {
	ref, err := c.HTTPPushServiceClient.PrepareRegistration(ctx, cmd)
	if err == nil {
		c.prepared()
	}
	return ref, err
}
func (c *observedRegistrationClient) RegisterEndpoint(ctx context.Context, cmd RegisterGrantCommand) (string, error) {
	ref, err := c.HTTPPushServiceClient.RegisterEndpoint(ctx, cmd)
	if err == nil {
		c.completed()
	}
	return ref, err
}

func TestPostgresOverlappingProviderSnapshotsStayConsistent(t *testing.T) {
	service, db, now := newPostgresPushTestService(t)
	upstream, client := newPostgresPushContract(t, db)
	client.options.HuaweiChannelID = strings.Repeat("5", 32)
	upstream.channelProviders.Store(client.options.HuaweiChannelID, "huawei_hms")
	service.grants.endpoints, service.gateway = client, client
	user := insertPushUser(t, db, "overlapping-snapshots@example.test")
	var session store.UserSession
	if err := db.Where("user_id = ?", user.ID).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	fcm := ProviderRegistration{Provider: "fcm", Token: "overlapping-fcm-token"}
	hms := ProviderRegistration{Provider: "huawei_hms", Token: "overlapping-hms-token"}
	cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{fcm, hms}}
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("registration-operation:" + cmd.InstallationID))
	operationKey := int64(binary.BigEndian.Uint64(digest[:8]))
	assertOperationLocked := func() {
		t.Helper()
		if err := db.Transaction(func(tx *gorm.DB) error {
			var acquired bool
			if err := tx.Raw("SELECT pg_try_advisory_xact_lock(?)", operationKey).Scan(&acquired).Error; err != nil {
				return err
			}
			if acquired {
				t.Error("registration operation lock missing across preparation/completion")
			}
			return nil
		}); err != nil {
			t.Error(err)
		}
	}
	prepared, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	observed := &observedRegistrationClient{HTTPPushServiceClient: client, prepared: func() { assertOperationLocked(); close(prepared); <-release }, completed: assertOperationLocked}
	service.grants.endpoints = observed
	first := cmd
	first.Registrations = []ProviderRegistration{fcm}
	second := cmd
	second.Registrations = []ProviderRegistration{hms}
	secondService, err := NewService(Dependencies{DB: db, Cipher: service.cipher, Gateway: client, Enabled: true, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	var reports atomic.Int32
	upstream.beforeDeviceReport = func() { reports.Add(1) }
	firstDone := make(chan struct{})
	var firstErr error
	go func() { defer close(firstDone); _, firstErr = service.RegisterGrant(t.Context(), first) }()
	defer func() {
		unblock()
		select {
		case <-firstDone:
			if firstErr != nil {
				t.Error(firstErr)
			}
		case <-time.After(3 * time.Second):
			t.Error("first registration did not finish")
		}
	}()
	select {
	case <-prepared:
	case <-time.After(3 * time.Second):
		t.Fatal("first preparation did not finish")
	}
	secondDone := make(chan struct{})
	var secondErr error
	go func() { defer close(secondDone); _, secondErr = secondService.RegisterGrant(t.Context(), second) }()
	defer func() {
		unblock()
		select {
		case <-secondDone:
			if secondErr != nil {
				t.Error(secondErr)
			}
		case <-time.After(3 * time.Second):
			t.Error("second registration did not finish")
		}
	}()
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int64
		if err := db.Raw("SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND wait_event='advisory'").Scan(&waiting).Error; err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case <-secondDone:
			t.Fatalf("second snapshot was not serialized: %v", secondErr)
		case <-deadline:
			t.Fatal("second registration did not wait")
		case <-ticker.C:
		}
	}
	if reports.Load() != 1 {
		t.Fatal("second device snapshot published before first completion")
	}
	unblock()
	select {
	case <-firstDone:
		if firstErr != nil {
			t.Fatal(firstErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first registration stalled")
	}
	select {
	case <-secondDone:
		if secondErr != nil {
			t.Fatal(secondErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second registration stalled")
	}
	grant, binding := storedRegistration(t, service, db, cmd.InstallationID)
	if grant.Status != GrantStatusActive {
		t.Fatal("final snapshot did not become active")
	}
	var device contractDevice
	if err := db.First(&device, "id = ?", binding.InstallationID).Error; err != nil {
		t.Fatal(err)
	}
	if device.Providers != `["huawei_hms"]` {
		t.Fatalf("device has obsolete provider snapshot: %s", device.Providers)
	}
	var endpoints []contractEndpoint
	if err := db.Where("status = 'active'").Find(&endpoints).Error; err != nil || len(endpoints) != 1 || endpoints[0].Provider != "huawei_hms" {
		t.Fatalf("endpoints do not match latest device snapshot: count=%d error=%v", len(endpoints), err)
	}
	raw, err := service.cipher.Decrypt(grant.SendTokenCiphertext, []byte(grant.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Send(t.Context(), cmd.GatewayGrantID, raw, "after-overlapping-updates", NotificationRequest{ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatalf("overlapping snapshots left no usable route: %v", err)
	}
}

func TestPostgresTokenRejectionsRecover(t *testing.T) {
	for _, outcome := range []string{"correct", "revoke", "logout"} {
		t.Run(outcome, func(t *testing.T) {
			service, db, now := newPostgresPushTestService(t)
			upstream, client := newPostgresPushContract(t, db)
			service.grants.endpoints = client
			user := insertPushUser(t, db, "token-rejection@example.test")
			var session store.UserSession
			if err := db.Where("user_id = ?", user.ID).First(&session).Error; err != nil {
				t.Fatal(err)
			}
			cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "vendor-invalid-token"}}}
			upstream.rejectedToken.Store(cmd.Registrations[0].Token)
			if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
				t.Fatalf("expected upstream token rejection: %v", err)
			}
			_, plan := storedRegistration(t, service, db, cmd.InstallationID)
			if len(plan.PendingCreates) != 1 || !plan.PendingCreates[0].Rejected {
				t.Fatal("first token validation rejection not settled")
			}
			switch outcome {
			case "correct":
				cmd.Registrations[0].Token = "corrected-token"
				if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
					t.Fatalf("corrected token still replayed rejected request: %v", err)
				}
				grant, _ := storedRegistration(t, service, db, cmd.InstallationID)
				if grant.Status != GrantStatusActive {
					t.Fatal("corrected token did not activate grant")
				}
			case "revoke":
				if err := service.RevokeGrant(t.Context(), user.ID, cmd.InstallationID, cmd.GatewayGrantID); err != nil {
					t.Fatal(err)
				}
			case "logout":
				if err := db.Delete(&session).Error; err != nil {
					t.Fatal(err)
				}
			}
			service.now = func() time.Time { return now.Add(time.Minute) }
			if _, err := service.ProcessEndpointRevocations(t.Context(), 25); err != nil {
				t.Fatalf("token rejection blocked cleanup: %v", err)
			}
			var count int64
			if err := db.Model(&store.MobilePushEndpointRevocation{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("token rejection retained cleanup: %d %v", count, err)
			}
		})
	}
}

func TestPostgresInvalidTokenShapeIsRejectedBeforePreparation(t *testing.T) {
	service, db, now := newPostgresPushTestService(t)
	upstream, client := newPostgresPushContract(t, db)
	service.grants.endpoints = client
	user := insertPushUser(t, db, "invalid-token-shape@example.test")
	var session store.UserSession
	if err := db.Where("user_id = ?", user.ID).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour)}
	for _, token := range []string{" token", "token ", "tok\nen", "tok\x00en", "tok\u200ben", string([]byte{0xff}), strings.Repeat("x", 8193)} {
		cmd.Registrations = []ProviderRegistration{{Provider: "fcm", Token: token}}
		if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "invalid_request" {
			t.Fatalf("invalid token shape accepted: %v", err)
		}
	}
	var count int64
	if err := db.Model(&store.UserPushGrant{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("invalid token persisted registration plan: %d %v", count, err)
	}
	if err := db.Model(&contractDevice{}).Count(&count).Error; err != nil || count != 0 || upstream.posts.Load() != 0 {
		t.Fatal("invalid token reached upstream")
	}
	cmd.Registrations = []ProviderRegistration{{Provider: "fcm", Token: "valid-token"}}
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatalf("corrected token could not register: %v", err)
	}
}
