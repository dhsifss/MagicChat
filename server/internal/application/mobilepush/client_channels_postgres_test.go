package mobilepush

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"app/internal/store"
	"github.com/google/uuid"
)

func TestPostgresClientChannelsIsolateRegistrationRenewalAndSend(t *testing.T) {
	service, db, now := newPostgresPushTestService(t)
	upstream, client := newPostgresPushContract(t, db)
	service.grants.endpoints = client
	commands := make([]RegisterGrantCommand, 0, 2)
	for index, clientID := range []string{"cloud.example.clienta", "cloud.example.clientb"} {
		channelID := strings.Repeat(string(rune('a'+index)), 32)
		upstream.clientChannels.Store(clientID+":xiaomi_push:", channelID)
		upstream.channelProviders.Store(channelID, "xiaomi_push")
		user := insertPushUser(t, db, clientID+"@example.test")
		var session store.UserSession
		if err := db.Where("user_id=?", user.ID).First(&session).Error; err != nil {
			t.Fatal(err)
		}
		cmd := RegisterGrantCommand{ClientID: clientID, UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{
			{Provider: "fcm", Token: "bundled-fcm-token"}, // No configured FCM channel.
			{Provider: "xiaomi_push", Token: "same-opaque-token-in-different-apps"},
		}}
		if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
			t.Fatalf("register %s: %v", clientID, err)
		}
		commands = append(commands, cmd)
	}
	assertBindings := func() {
		t.Helper()
		for index, cmd := range commands {
			var stored store.UserPushGrant
			if err := db.First(&stored, "installation_id=?", cmd.InstallationID).Error; err != nil {
				t.Fatal(err)
			}
			raw, err := service.cipher.Decrypt(stored.SendTokenCiphertext, []byte(stored.ID))
			if err != nil {
				t.Fatal(err)
			}
			binding, ok := decodePushBinding(raw)
			if !ok || binding.ClientID != cmd.ClientID || len(binding.EndpointIDs) != 1 {
				t.Fatalf("invalid client binding: %+v", binding)
			}
			var endpoint contractEndpoint
			if err := db.First(&endpoint, "id=?", binding.EndpointIDs[0]).Error; err != nil {
				t.Fatal(err)
			}
			if endpoint.ConfigID != strings.Repeat(string(rune('a'+index)), 32) || endpoint.Provider != "xiaomi_push" {
				t.Fatalf("cross-client endpoint: %+v", endpoint)
			}
			if err := client.Send(t.Context(), cmd.GatewayGrantID, raw, "message-for-"+cmd.ClientID, NotificationRequest{ExpiresAt: now.Add(time.Minute)}); err != nil {
				t.Fatal(err)
			}
			var submission contractSubmission
			if err := db.First(&submission, "key=?", pushReference("message-for-"+cmd.ClientID)).Error; err != nil {
				t.Fatal(err)
			}
			var request struct {
				Installations []string `json:"installation_ids"`
			}
			if json.Unmarshal([]byte(submission.Request), &request) != nil || len(request.Installations) != 1 || request.Installations[0] != endpoint.InstallationID {
				t.Fatalf("message selected another client: %s", submission.Request)
			}
		}
	}
	assertBindings()
	// A lost renewal response is recovered on the same application's endpoint.
	commands[0].ExpiresAt = now.Add(2 * time.Hour)
	commands[0].Registrations[1].Token = "renewed-client-a-token"
	upstream.failUpdateResponse.Store(true)
	if _, err := service.RegisterGrant(t.Context(), commands[0]); ErrorCodeOf(err) != "push_service_unavailable" {
		t.Fatalf("expected recoverable response loss: %v", err)
	}
	if _, err := service.RegisterGrant(t.Context(), commands[0]); err != nil {
		t.Fatal(err)
	}
	assertBindings()
	var active int64
	if err := db.Model(&contractEndpoint{}).Where("status='active'").Count(&active).Error; err != nil || active != 2 {
		t.Fatalf("renewal duplicated endpoints: %d %v", active, err)
	}
	var other contractEndpoint
	if err := db.First(&other, "config_id=?", strings.Repeat("b", 32)).Error; err != nil || other.Token != "same-opaque-token-in-different-apps" {
		t.Fatalf("renewal modified client B: %+v %v", other, err)
	}
	unknown := commands[0]
	unknown.ClientID, unknown.InstallationID, unknown.GatewayGrantID = "cloud.example.unknown", uuid.NewString(), uuid.NewString()
	if _, err := service.RegisterGrant(t.Context(), unknown); ErrorCodeOf(err) != "unsupported_provider" {
		t.Fatalf("unknown client used a legacy channel: %v", err)
	}
}

func TestPostgresClientRegistrationLossRetainsConfirmedRejectionsForCleanup(t *testing.T) {
	service, db, now := newPostgresPushTestService(t)
	upstream, client := newPostgresPushContract(t, db)
	service.grants.endpoints = client
	channelID := strings.Repeat("a", 32)
	upstream.clientChannels.Store("cloud.example.clienta:xiaomi_push:", channelID)
	upstream.channelProviders.Store(channelID, "xiaomi_push")
	user := insertPushUser(t, db, "client-create-loss@example.test")
	var session store.UserSession
	if err := db.Where("user_id=?", user.ID).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	cmd := RegisterGrantCommand{ClientID: "cloud.example.clienta", UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{
		{Provider: "fcm", Token: "unconfigured-fcm"}, {Provider: "xiaomi_push", Token: "native-token"},
	}}
	upstream.failCreateResponse.Store(true)
	if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
		t.Fatalf("expected response loss: %v", err)
	}
	var stored store.UserPushGrant
	if err := db.First(&stored, "installation_id=?", cmd.InstallationID).Error; err != nil {
		t.Fatal(err)
	}
	raw, err := service.cipher.Decrypt(stored.SendTokenCiphertext, []byte(stored.ID))
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := decodePushBinding(raw)
	if !ok || len(binding.PendingCreates) != 2 || !binding.PendingCreates[0].Rejected {
		t.Fatalf("confirmed unconfigured provider was lost: %+v", binding.PendingCreates)
	}
	if err := service.RevokeGrant(t.Context(), cmd.UserID, cmd.InstallationID, cmd.GatewayGrantID); err != nil {
		t.Fatalf("cleanup was blocked by unconfigured provider: %v", err)
	}
	var active int64
	if err := db.Model(&contractEndpoint{}).Where("status='active'").Count(&active).Error; err != nil || active != 0 {
		t.Fatalf("lost response left active endpoints: %d %v", active, err)
	}
}

func TestPostgresClientRegistrationRotatesWhenAPNsEnvironmentChanges(t *testing.T) {
	service, db, now := newPostgresPushTestService(t)
	upstream, client := newPostgresPushContract(t, db)
	service.grants.endpoints = client
	for env, id := range map[string]string{"production": strings.Repeat("a", 32), "development": strings.Repeat("b", 32)} {
		upstream.clientChannels.Store("cloud.example.ios:apns:"+env, id)
		upstream.channelProviders.Store(id, "apns")
	}
	user := insertPushUser(t, db, "apns-environment@example.test")
	var session store.UserSession
	if err := db.Where("user_id=?", user.ID).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	cmd := RegisterGrantCommand{ClientID: "cloud.example.ios", UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "ios", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "apns", Environment: "production", Token: "native-token"}}}
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	cmd.Registrations[0].Environment = "development"
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	var production, development contractEndpoint
	if err := db.First(&production, "config_id=?", strings.Repeat("a", 32)).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&development, "config_id=?", strings.Repeat("b", 32)).Error; err != nil {
		t.Fatal(err)
	}
	if production.Status != "revoked" || development.Status != "active" || production.InstallationID == development.InstallationID {
		t.Fatalf("APNs environment did not rotate: production=%+v development=%+v", production, development)
	}
}

func TestPostgresOptionalProviderRecoversUncertainRejection(t *testing.T) {
	for _, transient := range []string{"before_creation", "lost_rejection_response"} {
		for _, action := range []string{"retry", "revoke"} {
			t.Run(transient+"/"+action, func(t *testing.T) {
				service, db, now := newPostgresPushTestService(t)
				upstream, client := newPostgresPushContract(t, db)
				service.grants.endpoints = client
				channelID := strings.Repeat("a", 32)
				upstream.clientChannels.Store("cloud.example.clienta:xiaomi_push:", channelID)
				upstream.channelProviders.Store(channelID, "xiaomi_push")
				user := insertPushUser(t, db, "uncertain-provider@example.test")
				var session store.UserSession
				if err := db.Where("user_id=?", user.ID).First(&session).Error; err != nil {
					t.Fatal(err)
				}
				cmd := RegisterGrantCommand{ClientID: "cloud.example.clienta", UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{
					{Provider: "fcm", Token: "unconfigured-fcm"}, {Provider: "xiaomi_push", Token: "native-token"},
				}}
				if transient == "before_creation" {
					upstream.failCreateRequest.Store(true)
				} else {
					upstream.failRejectionResponse.Store(true)
				}
				if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
					t.Fatalf("expected uncertain first request: %v", err)
				}
				var stored store.UserPushGrant
				if err := db.First(&stored, "installation_id=?", cmd.InstallationID).Error; err != nil {
					t.Fatal(err)
				}
				raw, err := service.cipher.Decrypt(stored.SendTokenCiphertext, []byte(stored.ID))
				if err != nil {
					t.Fatal(err)
				}
				original, ok := decodePushBinding(raw)
				if !ok || len(original.PendingCreates) != 2 || original.PendingCreates[0].Rejected {
					t.Fatalf("uncertain request was discarded: %+v", original)
				}
				if action == "retry" {
					if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
						t.Fatalf("optional provider blocked configured provider: %v", err)
					}
					var endpoints []contractEndpoint
					if err := db.Where("status='active'").Find(&endpoints).Error; err != nil || len(endpoints) != 1 || endpoints[0].Provider != "xiaomi_push" {
						t.Fatalf("configured provider was not registered: %+v %v", endpoints, err)
					}
				}
				if err := service.RevokeGrant(t.Context(), cmd.UserID, cmd.InstallationID, cmd.GatewayGrantID); err != nil {
					t.Fatalf("optional provider blocked cleanup: %v", err)
				}
				// A newly configured FCM channel cannot revive the rejected key,
				// including delayed copies of the original uncertain request.
				upstream.clientChannels.Store("cloud.example.clienta:fcm:", strings.Repeat("b", 32))
				_, err = client.createEndpoint(t.Context(), original.PendingCreates[0].Request)
				var failure *GatewayError
				if !errors.As(err, &failure) || failure.Code != "client_channel_not_found" {
					t.Fatalf("late original escaped its persisted rejection: %v", err)
				}
				var active int64
				if err := db.Model(&contractEndpoint{}).Where("status='active'").Count(&active).Error; err != nil || active != 0 {
					t.Fatalf("cleanup left active endpoints: %d %v", active, err)
				}
			})
		}
	}
}
