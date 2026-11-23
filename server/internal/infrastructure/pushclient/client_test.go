package pushclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Notify when this request starts waiting for a shared refresh, allowing the
// cancellation scenarios to synchronize without timing assumptions.
type refreshWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *refreshWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestSharedRefreshIsIndependentOfRequestCancellation(t *testing.T) {
	for _, cancelInitiator := range []bool{true, false} {
		name := "waiting_request"
		if cancelInitiator {
			name = "initiating_request"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			var exchanges, sends atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth/token" {
					if exchanges.Add(1) == 1 {
						close(started)
					}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": testCredential("mpa_"), "token_type": "Bearer", "expires_in": 900})
					return
				}
				sends.Add(1)
				w.WriteHeader(202)
			}))
			defer server.Close()
			defer releaseOnce.Do(func() { close(release) })
			client, err := NewClient(server.URL, testCredential("mpt_"), server.Client())
			if err != nil {
				t.Fatal(err)
			}
			firstCtx, cancelFirst := context.WithCancel(ctx)
			defer cancelFirst()
			secondCtx, cancelSecond := context.WithCancel(ctx)
			defer cancelSecond()
			waiterCtx := &refreshWaitContext{Context: secondCtx, waiting: make(chan struct{})}
			send := func(ctx context.Context) <-chan error {
				result := make(chan error, 1)
				go func() {
					req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/messages", strings.NewReader(`{}`))
					if err == nil {
						var response *http.Response
						response, err = client.Do(req)
						if response != nil {
							response.Body.Close()
							if response.StatusCode != 202 {
								err = errors.New("unexpected business status")
							}
						}
					}
					result <- err
				}()
				return result
			}
			first := send(firstCtx)
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("exchange did not start")
			}
			second := send(waiterCtx)
			select {
			case <-waiterCtx.waiting:
			case <-ctx.Done():
				t.Fatal("second request did not wait for refresh")
			}
			canceled, surviving := first, second
			if cancelInitiator {
				cancelFirst()
			} else {
				cancelSecond()
				canceled, surviving = second, first
			}
			select {
			case err := <-canceled:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("canceled request did not cancel its own wait", err)
				}
			case <-ctx.Done():
				t.Fatal("canceled request remained blocked on refresh")
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case err := <-surviving:
				if err != nil {
					t.Fatal("unrelated request lost shared refresh", err)
				}
			case <-ctx.Done():
				t.Fatal("unrelated request did not complete")
			}
			if exchanges.Load() != 1 || sends.Load() != 1 {
				t.Fatal("refresh was duplicated or canceled request was sent", exchanges.Load(), sends.Load())
			}
		})
	}
}

type refreshRoundTripper func(*http.Request) (*http.Response, error)

func (f refreshRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestSharedRefreshRemainsBoundedAfterAllRequestsCancel(t *testing.T) {
	started := make(chan struct{})
	transport := refreshRoundTripper(func(req *http.Request) (*http.Response, error) {
		if deadline, ok := req.Context().Deadline(); !ok || time.Until(deadline) > 5*time.Second {
			t.Error("shared exchange lacks its independent timeout")
		}
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	client, err := NewClient("https://push.example", testCredential("mpt_"), &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := client.accessToken(ctx); result <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	client.mu.Lock()
	refresh := client.refreshing
	client.mu.Unlock()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled caller remained blocked")
	}
	select {
	case <-refresh.done:
		if !errors.Is(refresh.err, ErrAuthentication) {
			t.Fatal("shared timeout lost authentication failure", refresh.err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("shared refresh outlived its bounded timeout")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.refreshing != nil || client.token != "" {
		t.Fatal("failed shared refresh retained state")
	}
}

func testCredential(prefix string) string {
	return prefix + strings.Repeat("a", 32) + "." + strings.Repeat("A", 43)
}

func TestConcurrentRequestsRefreshAndCredentialSeparation(t *testing.T) {
	var exchanges, sends atomic.Int32
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	root := testCredential("mpt_")
	first, second := testCredential("mpa_"), strings.Replace(testCredential("mpa_"), ".A", ".B", 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/token" {
			n := exchanges.Add(1)
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+root {
				t.Error("exchange did not use the long-lived credential")
			}
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["grant_type"] != "client_credentials" {
				t.Error("invalid exchange body")
			}
			token := first
			if n > 1 {
				token = second
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": 900})
			return
		}
		sends.Add(1)
		expected := first
		if exchanges.Load() > 1 {
			expected = second
		}
		if r.Header.Get("Authorization") != "Bearer "+expected {
			t.Error("business API received the wrong credential")
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, root, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	batch := func() {
		t.Helper()
		var wg sync.WaitGroup
		for range 24 {
			wg.Go(func() {
				req, _ := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/messages", strings.NewReader(`{}`))
				response, err := client.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				defer response.Body.Close()
				if response.StatusCode != 202 || req.Header.Get("Authorization") != "" {
					t.Error("request mutated or not sent")
				}
			})
		}
		wg.Wait()
	}
	batch()
	if exchanges.Load() != 1 || sends.Load() != 24 {
		t.Fatal(exchanges.Load(), sends.Load())
	}
	clock.Add(14 * 60)
	batch()
	if exchanges.Load() != 2 || sends.Load() != 48 {
		t.Fatal("refresh was not shared by concurrent requests", exchanges.Load(), sends.Load())
	}
}

func TestExchangeFailuresDoNotSendBusinessRequestOrFollowRedirect(t *testing.T) {
	for _, failure := range []string{"denied", "redirect", "bad_token", "too_long", "too_short", "oversized"} {
		t.Run(failure, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1/auth/token" {
					t.Error("business request or redirect followed after failed exchange")
				}
				switch failure {
				case "denied":
					http.Error(w, testCredential("mpt_"), 403)
				case "redirect":
					w.Header().Set("Location", "/v1/other")
					w.WriteHeader(307)
				case "oversized":
					_, _ = w.Write([]byte(strings.Repeat("x", 4097)))
				default:
					token, ttl := testCredential("mpa_"), 900
					if failure == "bad_token" {
						token = testCredential("mpt_")
					}
					if failure == "too_long" {
						ttl = 901
					}
					if failure == "too_short" {
						ttl = 0
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": ttl})
				}
			}))
			defer server.Close()
			client, err := NewClient(server.URL, testCredential("mpt_"), server.Client())
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/messages", strings.NewReader(`{}`))
			if _, err := client.Do(req); !errors.Is(err, ErrAuthentication) || calls.Load() != 1 {
				t.Fatal(err, calls.Load())
			}
		})
	}
}

func TestExpiredBusinessRequestIsNotReplayedAndRefreshesNextRequest(t *testing.T) {
	var exchanges, sends atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/token" {
			exchanges.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": testCredential("mpa_"), "token_type": "Bearer", "expires_in": 900})
			return
		}
		sends.Add(1)
		w.WriteHeader(401)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, testCredential("mpt_"), server.Client())
	for i := int32(1); i <= 2; i++ {
		req, _ := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/messages", strings.NewReader(`{}`))
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 401 || sends.Load() != i || exchanges.Load() != i {
			t.Fatal("unexpected request replay")
		}
	}
	for _, path := range []string{"https://other.example/v1/messages", server.URL + "/v1/auth/token", server.URL + "/admin"} {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", path, nil)
		if _, err := client.Do(req); err == nil {
			t.Fatal("arbitrary target accepted")
		}
	}
}
