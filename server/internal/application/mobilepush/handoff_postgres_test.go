package mobilepush

import (
	"errors"
	"testing"
	"time"

	"app/internal/store"
	"github.com/google/uuid"
)

func TestPostgresHandoffAcrossIndependentPrivateServers(t *testing.T) {
	source, sourceDB, now := newPostgresPushTestService(t)
	_, upstream := newPostgresPushContract(t, sourceDB)
	source.grants.endpoints = upstream
	destination, destinationDB, _ := newPostgresPushTestService(t)
	destination.grants.endpoints = upstream
	userA := insertPushUser(t, sourceDB, "source-server@example.test")
	userB := insertPushUser(t, destinationDB, "destination-server@example.test")
	var sessionA, sessionB store.UserSession
	if err := sourceDB.Where("user_id=?", userA.ID).First(&sessionA).Error; err != nil {
		t.Fatal(err)
	}
	if err := destinationDB.Where("user_id=?", userB.ID).First(&sessionB).Error; err != nil {
		t.Fatal(err)
	}
	cmdA := RegisterGrantCommand{UserID: userA.ID, SessionID: sessionA.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "shared-device-token"}}}
	grantA, err := source.RegisterGrant(t.Context(), cmdA)
	if err != nil {
		t.Fatal(err)
	}
	if !validHandoffToken(grantA.HandoffToken) {
		t.Fatal("source registration did not return a portable handoff capability")
	}
	source.grants.endpoints = &fakeEndpointClient{revokeErr: errors.New("source backend unavailable")}
	if err := source.RevokeGrant(t.Context(), userA.ID, cmdA.InstallationID, cmdA.GatewayGrantID); ErrorCodeOf(err) != "push_service_unavailable" {
		t.Fatal("test did not retain the source token")
	}
	cmdB := cmdA
	cmdB.UserID = userB.ID
	cmdB.SessionID = sessionB.ID
	cmdB.InstallationID = uuid.NewString()
	cmdB.GatewayGrantID = uuid.NewString()
	if _, err := destination.RegisterGrant(t.Context(), cmdB); ErrorCodeOf(err) != "push_service_unavailable" {
		t.Fatalf("expected shared-channel token conflict before handoff: %v", err)
	}
	cmdB.HandoffTokens = []string{grantA.HandoffToken}
	grantB, err := destination.RegisterGrant(t.Context(), cmdB)
	if err != nil {
		t.Fatalf("offline source prevented handoff: %v", err)
	}
	if !validHandoffToken(grantB.HandoffToken) || grantB.HandoffToken == grantA.HandoffToken {
		t.Fatal("destination did not receive its own binding capability")
	}
	var active []contractEndpoint
	if err := sourceDB.Where("status='active'").Find(&active).Error; err != nil || len(active) != 1 {
		t.Fatalf("handoff did not leave one owner: count=%d error=%v", len(active), err)
	}
	_, binding := storedRegistration(t, destination, destinationDB, cmdB.InstallationID)
	if active[0].InstallationID != binding.InstallationID {
		t.Fatal("native token still belongs to source server")
	}
	// Recovery of the old server and its queued cleanup must not revoke B.
	source.grants.endpoints = upstream
	if err := source.RevokeGrant(t.Context(), userA.ID, cmdA.InstallationID, cmdA.GatewayGrantID); err != nil {
		t.Fatal(err)
	}
	source.now = func() time.Time { return now.Add(time.Minute) }
	if _, err := source.ProcessEndpointRevocations(t.Context(), 25); err != nil {
		t.Fatal(err)
	}
	active = nil
	if err := sourceDB.Where("status='active'").Find(&active).Error; err != nil || len(active) != 1 || active[0].InstallationID != binding.InstallationID {
		t.Fatal("source recovery revoked the new owner's token")
	}
	// The source ticket is safe to replay after destination registration.
	if _, err := destination.RegisterGrant(t.Context(), cmdB); err != nil {
		t.Fatalf("handoff replay blocked active destination: %v", err)
	}
}
