// Package pushclient authenticates backend requests to the Push service and
// refreshes short-lived access tokens using a long-lived service credential.
package pushclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

var credentialPattern = regexp.MustCompile(`^mpt_[0-9a-f]{32}\.[A-Za-z0-9_-]{43}$`)
var accessPattern = regexp.MustCompile(`^mpa_[0-9a-f]{32}\.[A-Za-z0-9_-]{43}$`)

// ErrAuthentication deliberately omits credential-bearing request/response data.
var ErrAuthentication = errors.New("push token exchange failed")

type Client struct {
	baseURL    string
	credential string
	http       *http.Client
	mu         sync.Mutex
	token      string
	expires    time.Time
	refreshing *tokenRefresh
	now        func() time.Time
}

type tokenRefresh struct {
	done  chan struct{}
	token string
	err   error
}

// NewClient accepts an origin URL and a backend-only mpt_ credential. HTTPS is
// recommended; HTTP is only suitable for a protected internal gateway hop.
// Redirects are disabled, including redirects within the original origin.
// The supplied http.Client is copied and its transport must be safe for reuse.
func NewClient(baseURL, credential string, client *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" && u.Path != "/" || !credentialPattern.MatchString(credential) {
		return nil, errors.New("invalid push origin or service credential")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), credential: credential, http: &copyClient, now: time.Now}, nil
}

// Do obtains a cached access token or refreshes it within one minute of expiry.
// Concurrent callers share one exchange. Business requests are sent once;
// failures never cause an implicit replay of registrations or messages.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || req.URL.User != nil || req.Host != "" && req.Host != req.URL.Host || req.Header.Get("Authorization") != "" || req.URL.Scheme+"://"+req.URL.Host != c.baseURL || !strings.HasPrefix(req.URL.Path, "/v1/") || req.URL.Path == "/v1/auth/token" {
		return nil, errors.New("push request must target this client's business API")
	}
	token, err := c.accessToken(req.Context())
	if err != nil {
		return nil, err
	}
	request := req.Clone(req.Context())
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.http.Do(request)
	if err != nil {
		// A custom transport error may contain request headers or credentials.
		return nil, errors.New("push business request failed")
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		c.mu.Lock()
		if c.token == token {
			c.token, c.expires = "", time.Time{}
		}
		c.mu.Unlock()
	}
	return response, nil
}

func (c *Client) accessToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	if c.token != "" && c.now().Add(time.Minute).Before(c.expires) {
		token := c.token
		c.mu.Unlock()
		return token, nil
	}
	refresh := c.refreshing
	if refresh == nil {
		refresh = &tokenRefresh{done: make(chan struct{})}
		c.refreshing = refresh
		// The shared exchange retains context values but has its own bounded
		// lifetime. Every caller, including the initiator, only owns its wait.
		go c.refresh(context.WithoutCancel(ctx), refresh)
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-refresh.done:
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return refresh.token, refresh.err
	}
}

func (c *Client) refresh(ctx context.Context, refresh *tokenRefresh) {
	token, expires, err := c.exchange(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.token, c.expires = token, expires
	}
	c.refreshing = nil
	refresh.token, refresh.err = token, err
	close(refresh.done)
}

func (c *Client) exchange(parent context.Context) (string, time.Time, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	started := c.now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/auth/token", strings.NewReader(`{"grant_type":"client_credentials"}`))
	if err != nil {
		return "", time.Time{}, ErrAuthentication
	}
	req.Header.Set("Authorization", "Bearer "+c.credential)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return "", time.Time{}, ErrAuthentication
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", time.Time{}, ErrAuthentication
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	defer clear(raw)
	if err != nil || len(raw) > 4096 {
		return "", time.Time{}, ErrAuthentication
	}
	var result struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if json.Unmarshal(raw, &result) != nil || result.TokenType != "Bearer" || !accessPattern.MatchString(result.AccessToken) || result.ExpiresIn <= 0 || result.ExpiresIn > 900 {
		return "", time.Time{}, ErrAuthentication
	}
	// Relative lifetime avoids dependence on the server/client wall-clock offset;
	// measuring from request start conservatively includes the network delay.
	expires := started.Add(time.Duration(result.ExpiresIn) * time.Second)
	if !c.now().Before(expires) {
		return "", time.Time{}, ErrAuthentication
	}
	return result.AccessToken, expires, nil
}
