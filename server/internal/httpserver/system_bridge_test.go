//go:build systeme2e

package httpserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"app/internal/appregistry"
	"app/internal/config"
	"app/internal/store"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// A separately running business server with production migrations, router,
// authentication, WebSocket handlers and push worker. Push uses its real URL.
func TestSystemChatBridge(t *testing.T) {
	directory := os.Getenv("SYSTEM_E2E_DIR")
	if directory == "" {
		t.Fatal("SYSTEM_E2E_DIR is required")
	}
	var upstream struct {
		URL, Credential string
		Configs         map[string]string
	}
	data, err := os.ReadFile(filepath.Join(directory, "push-ready.json"))
	if err != nil || json.Unmarshal(data, &upstream) != nil {
		t.Fatal("read live Push configuration", err)
	}
	caData, err := os.ReadFile(filepath.Join(directory, "push-ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		t.Fatal(err)
	}
	if !roots.AppendCertsFromPEM(caData) {
		t.Fatal("invalid local Push CA")
	}
	// Trust the fixture CA through the normal TLS verifier on every host OS.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	previousTransport := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = previousTransport; transport.CloseIdleConnections() }()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	base, err := store.OpenPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	basePool, _ := base.DB()
	defer basePool.Close()
	schema := "system_chat_" + uuid.NewString()[:8]
	if err := base.Exec(`CREATE SCHEMA "` + schema + `"`).Error; err != nil {
		t.Fatal(err)
	}
	defer base.Exec(`DROP SCHEMA "` + schema + `" CASCADE`)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, err := store.OpenPostgres(parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	defer pool.Close()
	if err := store.RunPostgresMigrations(db, "../../migrations"); err != nil {
		t.Fatal(err)
	}
	apps := config.AppsConfig{AIAssistantSecret: "system-fixture-assistant"}
	if _, err := appregistry.EnsureAIAssistantApp(db, apps); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Apps: apps, Admin: config.AdminConfig{Password: "system-admin-password"}, Database: config.DatabaseConfig{DSN: parsed.String()}, Server: config.ServerConfig{PublicHostname: "127.0.0.1", ClientHTTPSPort: 443, AdminHTTPSPort: 1443}, Push: config.PushConfig{
		Enabled: true, ServiceURL: upstream.URL, ServiceCredential: upstream.Credential, CredentialEncryptionKey: make([]byte, 32),
		APNSDevelopmentChannelID: upstream.Configs["apns"], FCMChannelID: upstream.Configs["fcm"], HuaweiChannelID: upstream.Configs["huawei_hms"],
		XiaomiChannelID: upstream.Configs["xiaomi_push"], OPPOChannelID: upstream.Configs["oppo_push"], VivoChannelID: upstream.Configs["vivo_push"], HonorChannelID: upstream.Configs["honor_push"], MeizuChannelID: upstream.Configs["meizu_push"],
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	router, shutdown := NewRouterWithTaskReminderWorker(ctx, db, cfg)
	server := httptest.NewTLSServer(router)
	defer func() {
		server.CloseClientConnections()
		server.Close()
		cancel()
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := shutdown(shutdownCtx); err != nil {
			t.Error(err)
		}
	}()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(directory, "chat-ca.pem"), ca, 0600); err != nil {
		t.Fatal(err)
	}
	// Hash once; the fixture creates independent real user rows per scenario.
	prototype := insertTestUser(t, db, "system-alice@example.test", "Alice", store.UserStatusActive, time.Now().UTC())
	done := make(chan struct{})
	var once sync.Once
	var seedMutex sync.Mutex
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/stop":
			once.Do(func() { close(done) })
			json.NewEncoder(w).Encode(map[string]bool{"stopped": true})
		case "/scenario":
			seedMutex.Lock()
			defer seedMutex.Unlock()
			receiver := prototype
			receiver.ID = uuid.NewString()
			receiver.Email = "bob-" + receiver.ID + "@example.test"
			receiver.Name = "Bob"
			var conversation store.Conversation
			err := db.Transaction(func(tx *gorm.DB) error {
				if err := tx.Create(&receiver).Error; err != nil {
					return err
				}
				low, high := orderTestUserIDs(prototype.ID, receiver.ID)
				if err := tx.Create(&store.UserFriendship{UserIDLow: low, UserIDHigh: high, CreatedAt: time.Now().UTC()}).Error; err != nil {
					return err
				}
				conversation = store.Conversation{ID: uuid.NewString(), Kind: store.ConversationKindDirect, Name: "System flow", CreatedByUserID: prototype.ID, Status: store.ConversationStatusActive, PostingPolicy: store.ConversationPostingPolicyOpen, Visibility: store.ConversationVisibilityPrivate, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
				if err := tx.Create(&conversation).Error; err != nil {
					return err
				}
				if err := tx.Create(&store.DirectConversation{ConversationID: conversation.ID, UserLowID: low, UserHighID: high}).Error; err != nil {
					return err
				}
				members := []store.ConversationMember{}
				for _, id := range []string{prototype.ID, receiver.ID} {
					members = append(members, store.ConversationMember{ConversationID: conversation.ID, MemberType: store.ConversationMemberTypeUser, MemberID: id, Role: store.ConversationMemberRoleMember, JoinedAt: time.Now().UTC(), HistoryVisibleFromSeq: 1})
				}
				return tx.Create(&members).Error
			})
			if err != nil {
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"conversationId": conversation.ID, "receiver": map[string]string{"id": receiver.ID, "email": receiver.Email}})
		case "/state":
			var jobs []store.MobilePushJob
			if err := db.Order("created_at").Find(&jobs).Error; err != nil {
				w.WriteHeader(500)
				return
			}
			views := []map[string]any{}
			for _, job := range jobs {
				views = append(views, map[string]any{"id": job.ID, "messageId": job.MessageID, "conversationId": job.ConversationID, "grantId": job.GrantID, "status": job.Status, "attempts": job.Attempts, "reason": job.LastErrorCode})
			}
			json.NewEncoder(w).Encode(map[string]any{"jobs": views})
		default:
			w.WriteHeader(404)
		}
	}))
	defer control.Close()
	ready := map[string]any{"url": server.URL, "control": control.URL, "ca": filepath.Join(directory, "chat-ca.pem"), "sender": map[string]string{"id": prototype.ID, "email": prototype.Email}, "password": "test-password"}
	data, _ = json.Marshal(ready)
	if err := os.WriteFile(filepath.Join(directory, "chat-ready.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(8 * time.Minute):
		t.Error("live business fixture timed out")
	}
}
