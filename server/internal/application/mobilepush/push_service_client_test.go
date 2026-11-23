package mobilepush

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"app/internal/store"
	"github.com/google/uuid"
)

const testServiceCredential = "mpt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const testAccessToken = "mpa_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.ccccccccccccccccccccccccccccccccccccccccccc"
const testInstallationID = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
const testBinding = `{"installation_id":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","endpoint_ids":["dddddddddddddddddddddddddddddddd"]}`

const testEndpointID = "dddddddddddddddddddddddddddddddd"

func newTestPushClient(t *testing.T, handler http.HandlerFunc) *HTTPPushServiceClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/handoff") {
			_ = json.NewEncoder(w).Encode(map[string]string{"handoff_token": "mph_" + strings.Repeat("h", 43)})
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := NewPushServiceClient(PushServiceOptions{URL: server.URL, Credential: testServiceCredential,
		APNSDevelopmentChannelID: strings.Repeat("1", 32), APNSProductionChannelID: strings.Repeat("2", 32), FCMChannelID: strings.Repeat("3", 32)}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func exchangeTestToken(t *testing.T, w http.ResponseWriter, r *http.Request) bool {
	t.Helper()
	if r.URL.Path != "/v1/auth/token" {
		return false
	}
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+testServiceCredential {
		t.Error("invalid token exchange")
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": testAccessToken, "token_type": "Bearer", "expires_in": 900})
	return true
}

func registerPreparedClient(t *testing.T, client *HTTPPushServiceClient, cmd RegisterGrantCommand) (string, error) {
	t.Helper()
	// The service persists this value between the preparation and endpoint write.
	reference, err := client.PrepareRegistration(t.Context(), cmd)
	if err != nil {
		return "", err
	}
	cmd.SendToken = reference
	return client.RegisterEndpoint(t.Context(), cmd)
}

func TestPushServiceSettlesOmittedPendingCreation(t *testing.T) {
	hmsID := strings.Repeat("f", 32)
	var created []string
	var deleted []string
	client := newTestPushClient(t, func(w http.ResponseWriter, r *http.Request) {
		if exchangeTestToken(t, w, r) {
			return
		}
		switch {
		case r.URL.Path == "/v1/devices":
			_, _ = w.Write([]byte(`{"id":"` + testInstallationID + `"}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"device_id":"` + testInstallationID + `","items":[],"has_more":false}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/endpoints":
			var request endpointCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			created = append(created, request.ChannelID)
			id := testEndpointID
			if request.ChannelID == strings.Repeat("4", 32) {
				id = hmsID
			}
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"endpoint_id":"` + id + `"}`))
		case r.Method == http.MethodDelete:
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/v1/endpoints/"))
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	client.options.HuaweiChannelID = strings.Repeat("4", 32)
	cmd := RegisterGrantCommand{GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: time.Now().Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "fcm-token"}, {Provider: "huawei_hms", Token: "hms-token"}}}
	var err error
	cmd.SendToken, err = client.PrepareRegistration(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Registrations = cmd.Registrations[:1]
	raw, err := client.RegisterEndpoint(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := decodePushBinding(raw)
	if !ok || len(binding.PendingCreates) != 0 || !reflect.DeepEqual(binding.EndpointIDs, []string{testEndpointID}) {
		t.Fatalf("registration lost recovery state: %s", raw)
	}
	if !reflect.DeepEqual(created, []string{strings.Repeat("3", 32), strings.Repeat("4", 32)}) || !reflect.DeepEqual(deleted, []string{hmsID}) {
		t.Fatalf("omitted provider creation was not fenced and revoked: created=%v deleted=%v", created, deleted)
	}
}

func TestPushServiceReportsDeviceAndRegistersAllNativeTokens(t *testing.T) {
	var reports, bodies []map[string]any
	exchanges := 0
	client := newTestPushClient(t, func(w http.ResponseWriter, r *http.Request) {
		if exchangeTestToken(t, w, r) {
			exchanges++
			return
		}
		if r.URL.Path == "/v1/devices/"+testInstallationID+"/endpoints" {
			_, _ = w.Write([]byte(`{"device_id":"` + testInstallationID + `","items":[],"has_more":false}`))
			return
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+testAccessToken {
			t.Error("invalid backend request")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		switch r.URL.Path {
		case "/v1/devices":
			reports = append(reports, body)
			_, _ = w.Write([]byte(`{"id":"` + testInstallationID + `"}`))
		case "/v1/endpoints":
			bodies = append(bodies, body)
			if len(body) != 7 {
				t.Errorf("registration fields: %v", body)
			}
			w.WriteHeader(http.StatusCreated)
			id := testEndpointID
			if body["channel_config_id"] == strings.Repeat("4", 32) {
				id = strings.Repeat("f", 32)
			}
			_, _ = w.Write([]byte(`{"endpoint_id":"` + id + `","version":1}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	})
	client.options.HuaweiChannelID = strings.Repeat("4", 32)
	cmd := RegisterGrantCommand{GatewayGrantID: uuid.NewString(), Platform: "android", Manufacturer: "HONOR", Model: "test phone", ExpiresAt: time.Now().Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "huawei_hms", Token: "hms-token"}, {Provider: "fcm", Token: "fcm-token"}}}
	var original string
	var persistedPreparation string
	for range 2 {
		if persistedPreparation == "" {
			persistedPreparation, _ = client.PrepareRegistration(t.Context(), cmd)
		}
		cmd.SendToken = persistedPreparation
		reference, err := client.RegisterEndpoint(t.Context(), cmd)
		binding, ok := decodePushBinding(reference)
		if err != nil || !ok || binding.InstallationID != testInstallationID || len(binding.EndpointIDs) != 2 {
			t.Fatalf("registration: %s %v", reference, err)
		}
		if original != "" && original != reference {
			t.Fatal("replay changed binding")
		}
		original = reference
	}
	if exchanges != 1 || len(bodies) != 4 || !reflect.DeepEqual(bodies[0], bodies[2]) || !reflect.DeepEqual(bodies[1], bodies[3]) {
		t.Fatal("registration replay changed")
	}
	if bodies[0]["installation_ref"] != bodies[1]["installation_ref"] || bodies[0]["binding_ref"] != bodies[1]["binding_ref"] {
		t.Fatal("tokens registered under different installations")
	}
	if reports[0]["installation_ref"] != bodies[0]["installation_ref"] || reports[0]["manufacturer"] != "HONOR" {
		t.Fatal("device context lost")
	}
	if !reflect.DeepEqual(reports[0]["registered_providers"], []any{"fcm", "huawei_hms"}) {
		t.Fatalf("registered capabilities: %v", reports[0])
	}
	// Registration order is metadata, never a client-defined routing priority.
	cmd.Registrations[0], cmd.Registrations[1] = cmd.Registrations[1], cmd.Registrations[0]
	if reference, err := client.RegisterEndpoint(t.Context(), cmd); err != nil || reference != original {
		t.Fatalf("order changed binding: %v", err)
	}
	cmd.Registrations = append(cmd.Registrations, cmd.Registrations[0])
	before := len(reports)
	if _, err := registerPreparedClient(t, client, cmd); err == nil || len(reports) != before {
		t.Fatal("duplicate capabilities reached API")
	}
}

func TestPushServiceAPNsEnvironmentMapsRegistrationChannel(t *testing.T) {
	var channels []string
	client := newTestPushClient(t, func(w http.ResponseWriter, r *http.Request) {
		if exchangeTestToken(t, w, r) {
			return
		}
		if r.URL.Path == "/v1/devices" {
			_, _ = w.Write([]byte(`{"id":"` + testInstallationID + `"}`))
			return
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"device_id":"` + testInstallationID + `","items":[],"has_more":false}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		channels = append(channels, body["channel_config_id"].(string))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"endpoint_id":"` + testEndpointID + `"}`))
	})
	for _, environment := range []string{"development", "production"} {
		_, err := registerPreparedClient(t, client, RegisterGrantCommand{Platform: "ios", GatewayGrantID: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "apns", Token: "apns-token", Environment: environment}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(channels, []string{strings.Repeat("1", 32), strings.Repeat("2", 32)}) {
		t.Fatalf("APNs channel: %v", channels)
	}
}

func TestPushServiceMessageRetriesPreserveBodyAndExpiry(t *testing.T) {
	var bodies []string
	client := newTestPushClient(t, func(w http.ResponseWriter, r *http.Request) {
		if exchangeTestToken(t, w, r) {
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer "+testAccessToken {
			t.Error("invalid message request")
		}
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if _, ok := body["endpoint_ids"]; ok {
			t.Error("send bypassed automatic provider routing")
		}
		if !reflect.DeepEqual(body["installation_ids"], []any{testInstallationID}) {
			t.Error("missing installation target")
		}
		if _, ok := body["collapse_key"]; ok {
			t.Error("unsupported collapse capability")
		}
		data := body["data"].(map[string]any)
		if data["event"] != "message_created" || data["route_token"] != strings.Repeat("r", 43) {
			t.Error("invalid opaque notification data")
		}
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"storage_unavailable","retryable":true}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	n := NotificationRequest{Event: "message.created", RouteToken: strings.Repeat("r", 43), CollapseKey: "conversation", TTLSeconds: 300, ExpiresAt: time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond)}
	err := client.Send(t.Context(), uuid.NewString(), testBinding, "job", n)
	var failure *GatewayError
	if !errors.As(err, &failure) || failure.Kind != GatewayErrorRetry {
		t.Fatalf("retry: %v", err)
	}
	grantID := "00000000-0000-0000-0000-000000000001"
	// Repeat the same persisted job with a smaller remaining TTL.
	bodies = nil
	n.TTLSeconds = 10
	_ = client.Send(t.Context(), grantID, testBinding, "stable-job", n)
	n.TTLSeconds = 9
	if err := client.Send(t.Context(), grantID, testBinding, "stable-job", n); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("replay changed request: %v", bodies)
	}
}

func TestPushServiceClassifiesAuthenticationAndEndpointFailures(t *testing.T) {
	for _, test := range []struct {
		status int
		code   string
		kind   GatewayErrorKind
	}{
		{401, "access_token_expired", GatewayErrorRetry}, {403, "authorization_denied", GatewayErrorInvalid},
		{404, "endpoint_not_found", GatewayErrorRevoked}, {409, "endpoint_inactive", GatewayErrorRevoked},
		{429, "request_rate_limited", GatewayErrorRetry}, {503, "storage_unavailable", GatewayErrorRetry},
		{409, "idempotency_conflict", GatewayErrorInvalid},
		{409, "endpoint_version_conflict", GatewayErrorRetry}, {500, "provider included secret!", GatewayErrorRetry},
		{409, "no_eligible_endpoint", GatewayErrorRetry},
	} {
		t.Run(test.code, func(t *testing.T) {
			client := newTestPushClient(t, func(w http.ResponseWriter, r *http.Request) {
				if exchangeTestToken(t, w, r) {
					return
				}
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": test.code})
			})
			err := client.Send(t.Context(), uuid.NewString(), testBinding, "job", NotificationRequest{ExpiresAt: time.Now().Add(time.Minute)})
			var failure *GatewayError
			if !errors.As(err, &failure) || failure.Kind != test.kind {
				t.Fatalf("classification: %v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("unsafe upstream error echoed")
			}
		})
	}
}

func TestPushServiceRevokeUsesBackendCredentials(t *testing.T) {
	client := newTestPushClient(t, func(w http.ResponseWriter, r *http.Request) {
		if exchangeTestToken(t, w, r) {
			return
		}
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/endpoints/"+testEndpointID || r.Header.Get("Authorization") != "Bearer "+testAccessToken {
			t.Error("invalid revoke request")
		}
		_, _ = w.Write([]byte(`{}`))
	})
	if err := client.RevokeEndpoint(t.Context(), testEndpointID); err != nil {
		t.Fatal(err)
	}
}

type fakeEndpointClient struct {
	*fakeGateway
	registered, revoked    []string
	registerErr, revokeErr error
}

func (f *fakeEndpointClient) RegisterEndpoint(_ context.Context, cmd RegisterGrantCommand) (string, error) {
	f.registered = append(f.registered, cmd.Registrations[0].Token)
	return testBinding, f.registerErr
}
func (f *fakeEndpointClient) RevokeEndpoint(_ context.Context, id string) error {
	f.revoked = append(f.revoked, id)
	return f.revokeErr
}

func TestNativeGrantRegistrationPersistsEndpointAndRevokeFailureKeepsLocalBinding(t *testing.T) {
	service, db, gateway, now := newPushTestService(t)
	endpoints := &fakeEndpointClient{fakeGateway: gateway}
	service.grants.endpoints = endpoints
	user := insertPushUser(t, db, "native@example.com")
	var session store.UserSession
	if err := db.Where("user_id = ?", user.ID).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "ios", Registrations: []ProviderRegistration{{Provider: "apns", Token: "native-token", Environment: "production"}}, ExpiresAt: now.Add(time.Hour)}
	invalid := cmd
	invalid.SessionID = uuid.NewString()
	if _, err := service.RegisterGrant(t.Context(), invalid); ErrorCodeOf(err) != "unauthorized" || len(endpoints.registered) != 0 {
		t.Fatalf("session validation: %v", err)
	}
	grant, err := service.RegisterGrant(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	var stored store.UserPushGrant
	if err := db.First(&stored, "installation_id = ?", grant.InstallationID).Error; err != nil {
		t.Fatal(err)
	}
	endpointID, err := service.cipher.Decrypt(stored.SendTokenCiphertext, []byte(stored.ID))
	if err != nil || endpointID != testBinding || strings.Contains(string(stored.SendTokenCiphertext), testEndpointID) {
		t.Fatal("endpoint not encrypted")
	}
	endpoints.revokeErr = errors.New("offline")
	if err := service.RevokeGrant(t.Context(), user.ID, cmd.InstallationID, cmd.GatewayGrantID); err == nil {
		t.Fatal("revoke failure ignored")
	}
	var count int64
	db.Model(&store.UserPushGrant{}).Count(&count)
	if count != 1 {
		t.Fatal("failed revoke lost local binding")
	}
	endpoints.revokeErr = nil
	if err := service.RevokeGrant(t.Context(), user.ID, cmd.InstallationID, cmd.GatewayGrantID); err != nil {
		t.Fatal(err)
	}
	db.Model(&store.UserPushGrant{}).Count(&count)
	if count != 0 {
		t.Fatal("binding not removed")
	}
}

func TestPushBindingReplacementOnlyRevokesRemovedEndpoints(t *testing.T) {
	before := `{"installation_id":"` + testInstallationID + `","endpoint_ids":["` + testEndpointID + `","ffffffffffffffffffffffffffffffff"]}`
	obsolete := obsoletePushBinding(before, testBinding)
	binding, ok := decodePushBinding(obsolete)
	if !ok || !reflect.DeepEqual(binding.EndpointIDs, []string{strings.Repeat("f", 32)}) {
		t.Fatalf("revoked shared endpoint: %s", obsolete)
	}
	if obsoletePushBinding(testBinding, before) != "" {
		t.Fatal("revoked retained endpoint")
	}
}

func TestPushServiceRevokeSnapshotProcessesEveryEndpoint(t *testing.T) {
	var paths []string
	client := newTestPushClient(t, func(w http.ResponseWriter, r *http.Request) {
		if exchangeTestToken(t, w, r) {
			return
		}
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, testEndpointID) {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"endpoint_not_found"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	reference := `{"installation_id":"` + testInstallationID + `","endpoint_ids":["` + testEndpointID + `","ffffffffffffffffffffffffffffffff"]}`
	if err := client.RevokeEndpoint(t.Context(), reference); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"/v1/endpoints/" + testEndpointID, "/v1/endpoints/ffffffffffffffffffffffffffffffff"}) {
		t.Fatalf("revoke paths: %v", paths)
	}
}

func TestPushServiceKeepsUpstreamSnapshotWhenTokensAreTemporarilyUnavailable(t *testing.T) {
	requests := 0
	client := newTestPushClient(t, func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(500) })
	_, err := registerPreparedClient(t, client, RegisterGrantCommand{GatewayGrantID: uuid.NewString(), Platform: "android", Registrations: []ProviderRegistration{}})
	var failure *GatewayError
	if requests != 0 || !errors.As(err, &failure) || failure.Code != "push_device_token_unavailable" || failure.Kind != GatewayErrorRetry {
		t.Fatalf("empty SDK request changed upstream: requests=%d err=%v", requests, err)
	}
}

func TestNativeGrantRegistrationDistinguishesMissingTokensAndMissingChannels(t *testing.T) {
	service, db, _, now := newPushTestService(t)
	client := newTestPushClient(t, func(w http.ResponseWriter, r *http.Request) {
		if exchangeTestToken(t, w, r) {
			return
		}
		if r.URL.Path != "/v1/devices" {
			t.Errorf("unexpected endpoint registration: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"` + testInstallationID + `"}`))
	})
	service.grants.endpoints = client
	user := insertPushUser(t, db, "registration-recovery@example.com")
	var session store.UserSession
	if err := db.Where("user_id = ?", user.ID).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour)}
	for _, test := range []struct {
		name          string
		registrations []ProviderRegistration
		code          string
	}{
		{name: "SDK tokens pending", registrations: []ProviderRegistration{}, code: "push_service_unavailable"},
		{name: "server channel missing", registrations: []ProviderRegistration{{Provider: "huawei_hms", Token: "hms-token"}}, code: "unsupported_provider"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd.Registrations = test.registrations
			if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != test.code {
				t.Fatalf("registration error = %v, want %s", err, test.code)
			}
		})
	}
}
