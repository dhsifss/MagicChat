package mobilepush

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountapp "app/internal/application/account"
	"app/internal/auth"
	"app/internal/store"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// This HTTP fixture stores endpoint state in PostgreSQL with the Push service's
// two active-endpoint uniqueness constraints. Requests use its POST/PUT/CAS and
// complete-device-snapshot semantics, so a duplicate POST cannot mimic renewal.
type postgresPushContract struct {
	db                  *gorm.DB
	failUpdateResponse  atomic.Bool
	failCreateResponse  atomic.Bool
	failCreateRequest   atomic.Bool
	failRevoke          atomic.Bool
	failMessageResponse atomic.Bool
	failDeviceReport    atomic.Bool
	routingUnavailable  atomic.Bool
	posts               atomic.Int32
	puts                atomic.Int32
	beforeCreate        func()
	afterCreate         func()
	beforeRevoke        func()
	beforeMessage       func()
	beforeDeviceReport  func()
	unreadyChannel      atomic.Value
	rejectedToken       atomic.Value
	channelProviders    sync.Map
}
type contractEndpoint struct {
	ID             string `gorm:"primaryKey"`
	InstallationID string
	Provider       string
	ConfigID       string
	Token          string
	Version        int64
	ExpiresAt      time.Time
	Status         string
}

func (contractEndpoint) TableName() string { return "review_upstream_endpoints" }

type contractDevice struct {
	ID        string `gorm:"primaryKey"`
	Ref       string
	Providers string
}

func (contractDevice) TableName() string { return "review_upstream_devices" }

type contractRegistration struct {
	Key        string `gorm:"primaryKey"`
	Request    string
	EndpointID string
}

func (contractRegistration) TableName() string { return "review_upstream_registrations" }

type contractSubmission struct {
	Key     string `gorm:"primaryKey"`
	Request string
}

func (contractSubmission) TableName() string { return "review_upstream_submissions" }

type contractHandoff struct {
	Token          string `gorm:"primaryKey"`
	InstallationID string
}

func (contractHandoff) TableName() string { return "review_upstream_handoffs" }

func newPostgresPushContract(t *testing.T, db *gorm.DB) (*postgresPushContract, *HTTPPushServiceClient) {
	t.Helper()
	if err := db.Exec(`CREATE TABLE review_upstream_devices(id TEXT PRIMARY KEY, ref TEXT UNIQUE NOT NULL, providers TEXT NOT NULL);
 CREATE TABLE review_upstream_endpoints(id TEXT PRIMARY KEY, installation_id TEXT NOT NULL, provider TEXT NOT NULL, config_id TEXT NOT NULL, token TEXT NOT NULL, version BIGINT NOT NULL, expires_at TIMESTAMPTZ NOT NULL, status TEXT NOT NULL);
 CREATE UNIQUE INDEX review_active_token ON review_upstream_endpoints(config_id,provider,token) WHERE status='active';
 CREATE UNIQUE INDEX review_active_provider ON review_upstream_endpoints(installation_id,provider) WHERE status='active';
 CREATE TABLE review_upstream_registrations(key TEXT PRIMARY KEY, request TEXT NOT NULL, endpoint_id TEXT NOT NULL);
 CREATE TABLE review_upstream_submissions(key TEXT PRIMARY KEY, request TEXT NOT NULL);
 CREATE TABLE review_upstream_handoffs(token TEXT PRIMARY KEY,installation_id TEXT NOT NULL);`).Error; err != nil {
		t.Fatal(err)
	}
	fixture := &postgresPushContract{db: db}
	server := httptest.NewServer(http.HandlerFunc(fixture.serve))
	t.Cleanup(server.Close)
	client, err := NewPushServiceClient(PushServiceOptions{URL: server.URL, Credential: testServiceCredential, FCMChannelID: strings.Repeat("3", 32)}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return fixture, client
}
func (f *postgresPushContract) serve(w http.ResponseWriter, r *http.Request) {
	reject := func(status int, code string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": code})
	}
	write := func(status int, value any) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(value) }
	if r.URL.Path == "/v1/auth/token" {
		if r.Header.Get("Authorization") != "Bearer "+testServiceCredential {
			reject(403, "access_denied")
			return
		}
		write(200, map[string]any{"access_token": testAccessToken, "token_type": "Bearer", "expires_in": 900})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
		reject(403, "access_denied")
		return
	}
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/devices/") && strings.HasSuffix(r.URL.Path, "/handoff") {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/devices/"), "/handoff")
		item := contractHandoff{Token: "mph_" + pushReference("handoff:"+id), InstallationID: id}
		if f.db.Where("token=?", item.Token).FirstOrCreate(&item).Error != nil {
			reject(503, "db_error")
			return
		}
		write(200, map[string]string{"handoff_token": item.Token})
		return
	}
	if r.URL.Path == "/v1/device-handoffs/redeem" {
		var input struct {
			Token string `json:"handoff_token"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			reject(400, "invalid_handoff")
			return
		}
		var item contractHandoff
		if f.db.Where("token=?", input.Token).First(&item).Error != nil {
			reject(400, "invalid_handoff")
			return
		}
		if f.db.Model(&contractEndpoint{}).Where("installation_id=? AND status='active'", item.InstallationID).Update("status", "revoked").Error != nil {
			reject(503, "db_error")
			return
		}
		write(200, map[string]bool{"retired": true})
		return
	}
	if r.URL.Path == "/v1/devices" {
		if f.beforeDeviceReport != nil {
			f.beforeDeviceReport()
		}
		if f.failDeviceReport.Swap(false) {
			reject(503, "device_report_unavailable")
			return
		}
		var input struct {
			Ref       string   `json:"installation_ref"`
			Providers []string `json:"registered_providers"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			reject(400, "invalid_request")
			return
		}
		var device contractDevice
		err := f.db.Where("ref = ?", input.Ref).First(&device).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			device.ID = strings.ReplaceAll(uuid.NewString(), "-", "")
			device.Ref = input.Ref
		} else if err != nil {
			reject(503, "db_error")
			return
		}
		raw, _ := json.Marshal(input.Providers)
		device.Providers = string(raw)
		if f.db.Save(&device).Error != nil {
			reject(503, "db_error")
			return
		}
		write(200, map[string]string{"id": device.ID})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/devices/") && r.Method == http.MethodGet {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/devices/"), "/endpoints")
		var rows []contractEndpoint
		if f.db.Where("installation_id = ?", id).Find(&rows).Error != nil {
			reject(503, "db_error")
			return
		}
		items := []upstreamEndpoint{}
		active := false
		for _, row := range rows {
			if row.Status == "active" {
				active = true
			}
		}
		for _, row := range rows {
			if !active {
				continue
			}
			items = append(items, upstreamEndpoint{ID: row.ID, InstallationID: row.InstallationID, Provider: row.Provider, ConfigID: row.ConfigID, Status: row.Status, Version: row.Version, ExpiresAt: &row.ExpiresAt})
		}
		generation := "0"
		if len(rows) > 0 {
			generation = "1"
		}
		write(200, map[string]any{"device_id": id, "items": items, "has_more": false, "installation_generation": generation})
		return
	}
	if r.URL.Path == "/v1/endpoints" && r.Method == http.MethodPost {
		if f.failCreateRequest.Swap(false) {
			reject(503, "registration_unavailable")
			return
		}
		if f.afterCreate != nil {
			defer f.afterCreate()
		}
		f.posts.Add(1)
		if f.beforeCreate != nil {
			f.beforeCreate()
		}
		var input endpointCreateRequest
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			reject(400, "invalid_request")
			return
		}
		if rejected, _ := f.rejectedToken.Load().(string); rejected != "" && input.Token == rejected {
			reject(400, "invalid_endpoint_token")
			return
		}
		raw, _ := json.Marshal(input)
		endpointID := ""
		err := f.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?,0))", input.IdempotencyKey).Error; err != nil {
				return err
			}
			var original contractRegistration
			if err := tx.First(&original, "key = ?", input.IdempotencyKey).Error; err == nil {
				if original.Request != string(raw) {
					return errors.New("idempotency_conflict")
				}
				endpointID = original.EndpointID
				return nil
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if unready, _ := f.unreadyChannel.Load().(string); unready != "" && input.ChannelID == unready {
				return errors.New("channel_not_ready")
			}
			var device contractDevice
			if err := tx.Where("ref = ?", input.InstallationRef).First(&device).Error; err != nil {
				return err
			}
			var total, active int64
			if err := tx.Model(&contractEndpoint{}).Where("installation_id = ?", device.ID).Count(&total).Error; err != nil {
				return err
			}
			if err := tx.Model(&contractEndpoint{}).Where("installation_id = ? AND status = 'active'", device.ID).Count(&active).Error; err != nil {
				return err
			}
			if total > 0 && active == 0 {
				return errors.New("binding_retired")
			}
			expiry, err := time.Parse(time.RFC3339Nano, input.ExpiresAt)
			if err != nil {
				return err
			}
			if !expiry.After(time.Now()) {
				return errors.New("endpoint_expired")
			}
			provider := "fcm"
			if configured, ok := f.channelProviders.Load(input.ChannelID); ok {
				provider = configured.(string)
			}
			endpoint := contractEndpoint{ID: strings.ReplaceAll(uuid.NewString(), "-", ""), InstallationID: device.ID, Provider: provider, ConfigID: input.ChannelID, Token: input.Token, Version: 1, ExpiresAt: expiry, Status: "active"}
			if err := tx.Create(&endpoint).Error; err != nil {
				return err
			}
			endpointID = endpoint.ID
			return tx.Create(&contractRegistration{Key: input.IdempotencyKey, Request: string(raw), EndpointID: endpoint.ID}).Error
		})
		if err != nil {
			code := "endpoint_conflict"
			if err.Error() == "binding_retired" || err.Error() == "idempotency_conflict" || err.Error() == "endpoint_expired" || err.Error() == "channel_not_ready" {
				code = err.Error()
			}
			reject(409, code)
			return
		}
		if f.failCreateResponse.Swap(false) {
			reject(503, "response_lost")
			return
		}
		write(201, map[string]any{"endpoint_id": endpointID, "version": 1})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/endpoints/") {
		var endpoint contractEndpoint
		if f.db.First(&endpoint, "id = ?", strings.TrimPrefix(r.URL.Path, "/v1/endpoints/")).Error != nil {
			reject(404, "endpoint_not_found")
			return
		}
		if r.Method == http.MethodDelete {
			if f.beforeRevoke != nil {
				f.beforeRevoke()
			}
			if f.failRevoke.Load() {
				reject(503, "storage_unavailable")
				return
			}
			if f.db.Model(&endpoint).Update("status", "revoked").Error != nil {
				reject(503, "db_error")
				return
			}
			write(200, endpoint)
			return
		}
		if r.Method == http.MethodPut {
			f.puts.Add(1)
			var input struct {
				Version   int64     `json:"expected_version"`
				Token     string    `json:"token"`
				ExpiresAt time.Time `json:"expires_at"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil {
				reject(400, "invalid_request")
				return
			}
			if input.Version != endpoint.Version {
				reject(409, "endpoint_version_conflict")
				return
			}
			if !endpoint.ExpiresAt.After(time.Now()) {
				reject(410, "endpoint_expired")
				return
			}
			if endpoint.Status != "active" {
				reject(410, "endpoint_inactive")
				return
			}
			if input.Token != endpoint.Token || !input.ExpiresAt.Equal(endpoint.ExpiresAt) {
				result := f.db.Model(&endpoint).Where("version = ?", input.Version).Updates(map[string]any{"token": input.Token, "expires_at": input.ExpiresAt, "version": input.Version + 1})
				if result.Error != nil {
					reject(409, "endpoint_conflict")
					return
				}
			}
			if f.failUpdateResponse.Swap(false) {
				reject(503, "response_lost")
				return
			}
			write(200, endpoint)
			return
		}
	}
	if r.URL.Path == "/v1/messages" {
		if f.beforeMessage != nil {
			f.beforeMessage()
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			reject(400, "invalid_request")
			return
		}
		raw, _ := json.Marshal(body)
		key, _ := body["idempotency_key"].(string)
		var original contractSubmission
		if err := f.db.First(&original, "key = ?", key).Error; err == nil {
			if original.Request != string(raw) {
				reject(409, "idempotency_conflict")
				return
			}
			write(202, map[string]any{})
			return
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			reject(503, "db_error")
			return
		}
		if f.routingUnavailable.Load() {
			reject(409, "no_eligible_endpoint")
			return
		}
		var input struct {
			IDs []string `json:"installation_ids"`
		}
		if json.Unmarshal(raw, &input) != nil || len(input.IDs) != 1 {
			reject(400, "invalid_request")
			return
		}
		var device contractDevice
		if f.db.First(&device, "id = ?", input.IDs[0]).Error != nil {
			reject(404, "target_not_found")
			return
		}
		if device.Providers == "[]" {
			reject(409, "no_eligible_endpoint")
			return
		}
		var count int64
		var providers []string
		if json.Unmarshal([]byte(device.Providers), &providers) != nil {
			reject(503, "invalid_device")
			return
		}
		if f.db.Model(&contractEndpoint{}).Where("installation_id = ? AND status = 'active' AND expires_at > ? AND provider IN ?", device.ID, time.Now(), providers).Count(&count).Error != nil || count == 0 {
			reject(409, "no_eligible_endpoint")
			return
		}
		if f.db.Create(&contractSubmission{Key: key, Request: string(raw)}).Error != nil {
			reject(503, "db_error")
			return
		}
		if f.failMessageResponse.Swap(false) {
			reject(503, "response_lost")
			return
		}
		write(202, map[string]any{})
		return
	}
	reject(404, "unknown_path")
}

func TestPostgresPushRenewalUpdatesEndpointAndKeepsRoutesDuringTokenRecovery(t *testing.T) {
	service, db, now := newPostgresPushTestService(t)
	upstream, client := newPostgresPushContract(t, db)
	service.grants.endpoints = client
	user := insertPushUser(t, db, "renewal-contract@example.test")
	var session store.UserSession
	db.Where("user_id = ?", user.ID).First(&session)
	cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "unchanged-native-token"}}}
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	var initial store.UserPushGrant
	db.First(&initial, "installation_id = ?", cmd.InstallationID)
	original, _ := service.cipher.Decrypt(initial.SendTokenCiphertext, []byte(initial.ID))
	cmd.ExpiresAt = now.Add(24 * time.Hour)
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatalf("renewal: %v", err)
	}
	upstream.failUpdateResponse.Store(true)
	cmd.ExpiresAt = now.Add(48 * time.Hour)
	if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
		t.Fatalf("lost update response: %v", err)
	}
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatalf("recover committed update: %v", err)
	}
	if upstream.posts.Load() != 1 || upstream.puts.Load() != 3 {
		t.Fatalf("renewal used POST: posts=%d puts=%d", upstream.posts.Load(), upstream.puts.Load())
	}
	var current store.UserPushGrant
	db.First(&current, "id = ?", initial.ID)
	reference, _ := service.cipher.Decrypt(current.SendTokenCiphertext, []byte(current.ID))
	oldBinding, _ := decodePushBinding(original)
	newBinding, _ := decodePushBinding(reference)
	if !reflect.DeepEqual(oldBinding, newBinding) || current.GatewayGrantID != initial.GatewayGrantID || !current.ExpiresAt.Equal(cmd.ExpiresAt) {
		t.Fatal("renewal changed binding or lost expiry")
	}
	var endpoint contractEndpoint
	db.First(&endpoint)
	if endpoint.Version != 3 || !endpoint.ExpiresAt.Equal(cmd.ExpiresAt) {
		t.Fatalf("upstream renewal = %+v", endpoint)
	}
	cmd.Registrations = nil
	if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
		t.Fatalf("temporary token error: %v", err)
	}
	var device contractDevice
	db.First(&device)
	if device.Providers != `["fcm"]` {
		t.Fatalf("empty tokens cleared route: %s", device.Providers)
	}
	if err := client.Send(t.Context(), cmd.GatewayGrantID, reference, "job-after-empty-token", NotificationRequest{ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatalf("working route stopped: %v", err)
	}
}

func TestPostgresLogoutRetainsEndpointCleanupAndReloginDrainsIt(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%t", native), func(t *testing.T) {
			service, db, now := newPostgresPushTestService(t)
			upstream, client := newPostgresPushContract(t, db)
			service.grants.endpoints = client
			user := insertPushUser(t, db, "logout-contract@example.test")
			var session store.UserSession
			db.Where("user_id = ?", user.ID).First(&session)
			refreshHash := auth.HashSessionToken("refresh-token")
			db.Model(&session).Updates(map[string]any{"token_hash": auth.HashSessionToken("access-token"), "refresh_token_hash": refreshHash})
			cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "same-token-after-login"}}}
			if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
				t.Fatal(err)
			}
			accounts := accountapp.NewService(accountapp.Dependencies{DB: db})
			var err error
			if native {
				err = accounts.RevokeNativeSession(t.Context(), "refresh-token")
			} else {
				err = accounts.Logout(t.Context(), accountapp.LogoutCommand{Token: "access-token", InstallationID: cmd.InstallationID})
			}
			if err != nil {
				t.Fatal(err)
			}
			var pending []store.MobilePushEndpointRevocation
			if err := db.Find(&pending).Error; err != nil || len(pending) != 1 {
				t.Fatalf("cleanup lost: %v %+v", err, pending)
			}
			var count int64
			db.Model(&store.UserPushGrant{}).Count(&count)
			if count != 0 {
				t.Fatal("logout retained local grant")
			}
			// A restarted worker uses only the durable snapshot and the persisted key.
			restarted, err := NewService(Dependencies{DB: db, Cipher: service.cipher, Gateway: client, Enabled: true, Now: time.Now})
			if err != nil {
				t.Fatal(err)
			}
			service = restarted
			upstream.failRevoke.Store(true)
			service.now = time.Now
			if _, err := service.ProcessEndpointRevocations(t.Context(), 25); err == nil {
				t.Fatal("cleanup outage ignored")
			}
			db.Model(&store.MobilePushEndpointRevocation{}).Count(&count)
			if count != 1 {
				t.Fatal("failed cleanup lost durable reference")
			}
			next := store.UserSession{ID: uuid.NewString(), UserID: user.ID, TokenHash: uuid.NewString(), ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now, LastSeenAt: now}
			if err := db.Create(&next).Error; err != nil {
				t.Fatal(err)
			}
			cmd.SessionID = next.ID
			cmd.GatewayGrantID = uuid.NewString()
			if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
				t.Fatalf("relogin bypassed pending cleanup: %v", err)
			}
			upstream.failRevoke.Store(false)
			if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
				t.Fatalf("relogin native token conflict: %v", err)
			}
			db.Model(&store.MobilePushEndpointRevocation{}).Count(&count)
			if count != 0 {
				t.Fatal("relogin left cleanup pending")
			}
			var active int64
			db.Model(&contractEndpoint{}).Where("status='active'").Count(&active)
			if active != 1 {
				t.Fatalf("active endpoints=%d", active)
			}
			// Direct grant deletion also keeps a durable snapshot for the background worker.
			if err := db.Where("session_id = ?", next.ID).Delete(&store.UserPushGrant{}).Error; err != nil {
				t.Fatal(err)
			}
			service.now = time.Now
			if _, err := service.ProcessEndpointRevocations(t.Context(), 25); err != nil {
				t.Fatalf("worker cleanup: %v", err)
			}
			db.Model(&contractEndpoint{}).Where("status='active'").Count(&active)
			if active != 0 {
				t.Fatal("worker retained upstream endpoint")
			}
		})
	}
}

func TestPostgresEndpointCleanupIsAtomicWithGrantDeletion(t *testing.T) {
	service, db, now := newPostgresPushTestService(t)
	_, client := newPostgresPushContract(t, db)
	service.grants.endpoints = client
	user := insertPushUser(t, db, "cleanup-atomic@example.test")
	var session store.UserSession
	db.Where("user_id = ?", user.ID).First(&session)
	cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "atomic-token"}}}
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("rollback session revocation")
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&session).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&store.MobilePushEndpointRevocation{}).Count(&count).Error; err != nil {
			return err
		}
		if count != 1 {
			t.Fatal("cascade did not retain snapshot in transaction")
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	var count int64
	db.Model(&store.UserPushGrant{}).Count(&count)
	if count != 1 {
		t.Fatal("rollback lost original binding")
	}
	db.Model(&store.MobilePushEndpointRevocation{}).Count(&count)
	if count != 0 {
		t.Fatal("rollback committed cleanup for live binding")
	}
	// User deletion also cascades through sessions/grants; cleanup has no FK to
	// either and retains the original ciphertext association after the account is gone.
	if err := db.Delete(&user).Error; err != nil {
		t.Fatal(err)
	}
	service.now = time.Now
	if _, err := service.ProcessEndpointRevocations(t.Context(), 25); err != nil {
		t.Fatal(err)
	}
	db.Model(&contractEndpoint{}).Where("status = 'active'").Count(&count)
	if count != 0 {
		t.Fatal("user cascade orphaned upstream endpoint")
	}
}

func TestPostgresPushRegistrationRecoversCommittedCreate(t *testing.T) {
	service, db, now := newPostgresPushTestService(t)
	upstream, client := newPostgresPushContract(t, db)
	service.grants.endpoints = client
	user := insertPushUser(t, db, "registration-lost-response@example.test")
	var session store.UserSession
	db.Where("user_id = ?", user.ID).First(&session)
	cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "lost-create-token"}}}
	upstream.beforeCreate = func() {
		var persisted store.UserPushGrant
		if err := db.Where("installation_id = ?", cmd.InstallationID).First(&persisted).Error; err != nil || persisted.Status != GrantStatusRegistering {
			t.Errorf("registration reference not committed before upstream creation: %v", err)
		}
	}
	upstream.failCreateResponse.Store(true)
	if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
		t.Fatalf("lost create response: %v", err)
	}
	var count int64
	db.Model(&store.UserPushGrant{}).Where("status = ?", GrantStatusRegistering).Count(&count)
	if count != 1 {
		t.Fatal("lost response did not retain recovery grant")
	}
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatalf("recover created endpoint: %v", err)
	}
	if upstream.posts.Load() != 1 || upstream.puts.Load() != 1 {
		t.Fatal("recovery created duplicate endpoint")
	}
}

func TestPostgresExpiredEndpointRotatesUpstreamInstallation(t *testing.T) {
	for _, mode := range []string{"expired", "interrupted_expired", "interrupted_retired"} {
		t.Run(mode, func(t *testing.T) {
			service, db, now := newPostgresPushTestService(t)
			upstream, client := newPostgresPushContract(t, db)
			service.grants.endpoints = client
			user := insertPushUser(t, db, "expired-device@example.test")
			var session store.UserSession
			db.Where("user_id = ?", user.ID).First(&session)
			cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "expired-returning-device-token"}}}
			if mode != "expired" {
				upstream.failCreateResponse.Store(true)
			}
			if _, err := service.RegisterGrant(t.Context(), cmd); (mode == "expired" && err != nil) || (mode != "expired" && ErrorCodeOf(err) != "push_service_unavailable") {
				t.Fatal(err)
			}
			var old contractEndpoint
			db.First(&old)
			if err := db.Model(&old).Update("expires_at", time.Now().Add(-24*time.Hour)).Error; err != nil {
				t.Fatal(err)
			}
			if mode == "interrupted_retired" {
				if err := db.Model(&old).Update("status", "revoked").Error; err != nil {
					t.Fatal(err)
				}
			}
			cmd.ExpiresAt = now.Add(30 * 24 * time.Hour)
			if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
				t.Fatalf("expired endpoint recovery: %v", err)
			}
			var rows []contractEndpoint
			db.Order("id").Find(&rows)
			if len(rows) != 2 {
				t.Fatalf("endpoints=%d", len(rows))
			}
			var active contractEndpoint
			if err := db.Where("status='active'").First(&active).Error; err != nil {
				t.Fatal(err)
			}
			if active.InstallationID == old.InstallationID || active.Token != old.Token {
				t.Fatal("expired upstream binding reused")
			}
			db.First(&old, "id = ?", old.ID)
			if old.Status != "revoked" {
				t.Fatal("expired token still occupies active-token index")
			}
			if upstream.puts.Load() != 0 {
				t.Fatal("attempted to update expired endpoint")
			}
			var local store.UserPushGrant
			db.First(&local, "installation_id = ?", cmd.InstallationID)
			if local.GatewayGrantID != cmd.GatewayGrantID || local.Status != GrantStatusActive {
				t.Fatal("recovery lost local grant identity")
			}
		})
	}
}

func TestPostgresInterruptedRegistrationCanBeAbandoned(t *testing.T) {
	for _, mode := range []string{"revoke", "switch_account", "session_logout"} {
		t.Run(mode, func(t *testing.T) {
			service, db, now := newPostgresPushTestService(t)
			upstream, client := newPostgresPushContract(t, db)
			service.grants.endpoints = client
			user := insertPushUser(t, db, "abandoned-registration@example.test")
			var session store.UserSession
			db.Where("user_id = ?", user.ID).First(&session)
			cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "interrupted-creation-token"}}}
			upstream.failCreateResponse.Store(true)
			if _, err := service.RegisterGrant(t.Context(), cmd); ErrorCodeOf(err) != "push_service_unavailable" {
				t.Fatalf("lost response: %v", err)
			}
			var recovery store.UserPushGrant
			if err := db.Where("installation_id = ?", cmd.InstallationID).First(&recovery).Error; err != nil {
				t.Fatalf("recovery record not persisted before write: %v", err)
			}
			raw, err := service.cipher.Decrypt(recovery.SendTokenCiphertext, []byte(recovery.ID))
			if err != nil {
				t.Fatal(err)
			}
			binding, ok := decodePushBinding(raw)
			if !ok || !binding.Discover || len(binding.EndpointIDs) != 0 || recovery.Status != GrantStatusRegistering {
				t.Fatalf("missing discovery reference: %s", raw)
			}
			switch mode {
			case "revoke":
				if err := service.RevokeGrant(t.Context(), user.ID, cmd.InstallationID, cmd.GatewayGrantID); err != nil {
					t.Fatalf("abandon pending grant: %v", err)
				}
			case "switch_account":
				nextUser := insertPushUser(t, db, "replacement-account@example.test")
				session = store.UserSession{}
				db.Where("user_id = ?", nextUser.ID).First(&session)
				cmd.UserID = nextUser.ID
				cmd.SessionID = session.ID
			case "session_logout":
				if err := db.Delete(&session).Error; err != nil {
					t.Fatal(err)
				}
				next := store.UserSession{ID: uuid.NewString(), UserID: user.ID, TokenHash: uuid.NewString(), ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now, LastSeenAt: now}
				if err := db.Create(&next).Error; err != nil {
					t.Fatal(err)
				}
				cmd.SessionID = next.ID
			}
			cmd.GatewayGrantID = uuid.NewString()
			if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
				t.Fatalf("new registration blocked by orphan: %v", err)
			}
			var active int64
			db.Model(&contractEndpoint{}).Where("status='active'").Count(&active)
			if active != 1 {
				t.Fatalf("active orphan remains: %d", active)
			}
		})
	}
}

func TestPostgresChangedChannelRotatesBinding(t *testing.T) {
	for _, mode := range []string{"normal", "lost_create_response", "failed_revoke"} {
		t.Run(mode, func(t *testing.T) {
			service, db, now := newPostgresPushTestService(t)
			upstream, client := newPostgresPushContract(t, db)
			service.grants.endpoints = client
			user := insertPushUser(t, db, "channel-rotation@example.test")
			var session store.UserSession
			if err := db.Where("user_id = ?", user.ID).First(&session).Error; err != nil {
				t.Fatal(err)
			}
			cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "channel-rotation-token"}}}
			if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
				t.Fatal(err)
			}
			var old contractEndpoint
			if err := db.First(&old).Error; err != nil {
				t.Fatal(err)
			}
			client.options.FCMChannelID = strings.Repeat("4", 32)
			switch mode {
			case "lost_create_response":
				upstream.failCreateResponse.Store(true)
			case "failed_revoke":
				upstream.failRevoke.Store(true)
			}
			if _, err := service.RegisterGrant(t.Context(), cmd); mode == "normal" {
				if err != nil {
					t.Fatalf("channel rotation: %v", err)
				}
			} else {
				if ErrorCodeOf(err) != "push_service_unavailable" {
					t.Fatalf("expected recoverable interruption, got %v", err)
				}
				upstream.failRevoke.Store(false)
				if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
					t.Fatalf("retry channel rotation: %v", err)
				}
			}
			if err := db.First(&old, "id = ?", old.ID).Error; err != nil || old.Status != "revoked" {
				t.Fatalf("old channel endpoint not revoked: %v", err)
			}
			var active []contractEndpoint
			if err := db.Where("status = 'active'").Find(&active).Error; err != nil || len(active) != 1 {
				t.Fatalf("active endpoints: %v %v", active, err)
			}
			if active[0].ConfigID != client.options.FCMChannelID || active[0].InstallationID == old.InstallationID || active[0].Token != old.Token {
				t.Fatal("new channel did not receive a fresh binding with the same token")
			}
			if upstream.posts.Load() != 2 {
				t.Fatal("recovery created duplicate endpoints")
			}
			var local store.UserPushGrant
			if err := db.Where("installation_id = ?", cmd.InstallationID).First(&local).Error; err != nil {
				t.Fatal(err)
			}
			if local.GatewayGrantID != cmd.GatewayGrantID || local.Status != GrantStatusActive {
				t.Fatal("channel rotation changed local grant identity")
			}
			reference, err := service.cipher.Decrypt(local.SendTokenCiphertext, []byte(local.ID))
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Send(t.Context(), cmd.GatewayGrantID, reference, "after-channel-rotation", NotificationRequest{ExpiresAt: now.Add(time.Minute)}); err != nil {
				t.Fatalf("new channel route unavailable: %v", err)
			}
		})
	}
}

func TestPostgresCleanupSchedulesFailuresAndAdvancesBeyondFullBatch(t *testing.T) {
	service, db, now := newPostgresPushTestService(t)
	_, client := newPostgresPushContract(t, db)
	service.grants.endpoints = client
	user := insertPushUser(t, db, "fair-cleanup@example.test")
	var session store.UserSession
	db.Where("user_id = ?", user.ID).First(&session)
	cmd := RegisterGrantCommand{UserID: user.ID, SessionID: session.ID, InstallationID: uuid.NewString(), GatewayGrantID: uuid.NewString(), Platform: "android", ExpiresAt: now.Add(time.Hour), Registrations: []ProviderRegistration{{Provider: "fcm", Token: "cleanup-fairness-token"}}}
	if _, err := service.RegisterGrant(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	for range 25 {
		row := store.MobilePushEndpointRevocation{ID: uuid.NewString(), InstallationID: uuid.NewString(), SendTokenCiphertext: []byte("unreadable-old-credential"), CreatedAt: now.Add(-time.Hour), NextAttemptAt: now.Add(-time.Hour)}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Where("session_id = ?", session.ID).Delete(&store.UserPushGrant{}).Error; err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now.Add(time.Minute) }
	if processed, err := service.ProcessEndpointRevocations(t.Context(), 25); err == nil || processed != 0 {
		t.Fatalf("old failures: %d %v", processed, err)
	}
	var scheduled int64
	db.Model(&store.MobilePushEndpointRevocation{}).Where("attempts=1 AND next_attempt_at > ?", service.now()).Count(&scheduled)
	if scheduled != 25 {
		t.Fatalf("failed rows not rescheduled: %d", scheduled)
	}
	if processed, err := service.ProcessEndpointRevocations(t.Context(), 25); err != nil || processed != 1 {
		t.Fatalf("new cleanup starved: %d %v", processed, err)
	}
	var active int64
	db.Model(&contractEndpoint{}).Where("status='active'").Count(&active)
	if active != 0 {
		t.Fatal("new logout endpoint not revoked")
	}
	service.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := service.ProcessEndpointRevocations(t.Context(), 25); err == nil {
		t.Fatal("old failures not retried when due")
	}
	db.Model(&store.MobilePushEndpointRevocation{}).Where("attempts=2").Count(&scheduled)
	if scheduled != 25 {
		t.Fatal("backoff rows did not retry")
	}
}
