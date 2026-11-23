package mobilepush

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"app/internal/infrastructure/pushclient"
	"github.com/google/uuid"
)

// PushServiceOptions maps native device providers to server-controlled channels.
type PushServiceOptions struct {
	URL, Credential                                                                                string
	APNSDevelopmentChannelID, APNSProductionChannelID, FCMChannelID                                string
	HuaweiChannelID, XiaomiChannelID, OPPOChannelID, VivoChannelID, HonorChannelID, MeizuChannelID string
}

type HTTPPushServiceClient struct {
	options PushServiceOptions
	client  *pushclient.Client
}

func NewPushServiceClient(options PushServiceOptions, httpClient *http.Client) (*HTTPPushServiceClient, error) {
	client, err := pushclient.NewClient(options.URL, options.Credential, httpClient)
	if err != nil {
		return nil, err
	}
	options.URL = strings.TrimRight(options.URL, "/")
	return &HTTPPushServiceClient{options: options, client: client}, nil
}

var pushErrorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,119}$`)

var endpointIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var registrationRefPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

var errBindingRetirementNeedsPause = errors.New("binding retirement requires a committed registration pause")

func validEndpointID(id string) bool { return endpointIDPattern.MatchString(id) }
func pushReference(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// pushBinding is stored encrypted in the existing local binding record.
// InstallationID is the Push device ID used for server-side provider routing.
type pushBinding struct {
	InstallationID      string                  `json:"installation_id"`
	EndpointIDs         []string                `json:"endpoint_ids"`
	RegistrationRef     string                  `json:"registration_ref,omitempty"`
	Discover            bool                    `json:"discover,omitempty"`
	BindingRef          string                  `json:"binding_ref,omitempty"`
	PendingCreates      []pendingEndpointCreate `json:"pending_creates,omitempty"`
	RegistrationPending bool                    `json:"registration_pending,omitempty"`
	HandoffToken        string                  `json:"handoff_token,omitempty"`
}

// Persist the exact POST before sending it. Replaying the same key serializes
// with a timed-out request upstream, even when a directory snapshot is empty.
type endpointCreateRequest struct {
	IdempotencyKey  string `json:"idempotency_key"`
	InstallationRef string `json:"installation_ref"`
	BindingRef      string `json:"binding_ref"`
	ChannelID       string `json:"channel_config_id"`
	Platform        string `json:"platform"`
	Token           string `json:"token"`
	ExpiresAt       string `json:"expires_at"`
}

type pendingEndpointCreate struct {
	Provider     string                `json:"provider"`
	Request      endpointCreateRequest `json:"request"`
	FirstAttempt bool                  `json:"first_attempt,omitempty"`
	Rejected     bool                  `json:"rejected,omitempty"`
}

func decodePushBinding(raw string) (pushBinding, bool) {
	var value pushBinding
	if json.Unmarshal([]byte(raw), &value) != nil || !validEndpointID(value.InstallationID) || (!value.Discover && len(value.EndpointIDs) == 0) || len(value.EndpointIDs) > 8 || len(value.PendingCreates) > 8 {
		return value, false
	}
	seen := map[string]bool{}
	for _, id := range value.EndpointIDs {
		if !validEndpointID(id) || seen[id] {
			return value, false
		}
		seen[id] = true
	}
	if (value.RegistrationRef != "" && !registrationRefPattern.MatchString(value.RegistrationRef)) || (value.BindingRef != "" && !registrationRefPattern.MatchString(value.BindingRef)) {
		return value, false
	}
	return value, true
}
func isPushBinding(raw string) bool {
	_, ok := decodePushBinding(raw)
	return ok || validEndpointID(raw)
}
func obsoletePushBinding(previous, current string) string {
	before, ok := decodePushBinding(previous)
	if !ok {
		if validEndpointID(previous) {
			return previous
		}
		return ""
	}
	after, _ := decodePushBinding(current)
	before.Discover = false
	before.PendingCreates = nil
	before.EndpointIDs = slices.DeleteFunc(before.EndpointIDs, func(id string) bool { return slices.Contains(after.EndpointIDs, id) })
	if len(before.EndpointIDs) == 0 {
		return ""
	}
	raw, _ := json.Marshal(before)
	return string(raw)
}

func validDeviceRegistrations(cmd RegisterGrantCommand) bool {
	validText := func(s string) bool {
		return utf8.ValidString(s) && len(s) <= 128 && strings.TrimSpace(s) == s && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) })
	}
	if !validText(cmd.Manufacturer) || !validText(cmd.Model) || len(cmd.Registrations) > 8 {
		return false
	}
	seen := map[string]bool{}
	for _, r := range cmd.Registrations {
		if seen[r.Provider] || len(r.Token) == 0 || len(r.Token) > 8192 || !utf8.ValidString(r.Token) || strings.TrimSpace(r.Token) != r.Token ||
			strings.ContainsFunc(r.Token, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
			return false
		}
		seen[r.Provider] = true
		switch r.Provider {
		case "apns":
			if cmd.Platform != "ios" || (r.Environment != "development" && r.Environment != "production") {
				return false
			}
		case "fcm", "huawei_hms", "xiaomi_push", "oppo_push", "vivo_push", "honor_push", "meizu_push":
			if cmd.Platform != "android" || r.Environment != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (c *HTTPPushServiceClient) registrationChannel(r ProviderRegistration) string {
	switch r.Provider {
	case "apns":
		if r.Environment == "development" {
			return c.options.APNSDevelopmentChannelID
		}
		return c.options.APNSProductionChannelID
	case "fcm":
		return c.options.FCMChannelID
	case "huawei_hms":
		return c.options.HuaweiChannelID
	case "xiaomi_push":
		return c.options.XiaomiChannelID
	case "oppo_push":
		return c.options.OPPOChannelID
	case "vivo_push":
		return c.options.VivoChannelID
	case "honor_push":
		return c.options.HonorChannelID
	case "meizu_push":
		return c.options.MeizuChannelID
	}
	return ""
}

func (c *HTTPPushServiceClient) PrepareRegistration(ctx context.Context, cmd RegisterGrantCommand) (string, error) {
	if len(cmd.Registrations) == 0 {
		return "", &GatewayError{Kind: GatewayErrorRetry, Code: "push_device_token_unavailable"}
	}
	if !validDeviceRegistrations(cmd) {
		return "", &GatewayError{Kind: GatewayErrorInvalid, Code: "invalid_device_report"}
	}
	registrations := slices.Clone(cmd.Registrations)
	slices.SortFunc(registrations, func(a, b ProviderRegistration) int { return strings.Compare(a.Provider, b.Provider) })
	providers := make([]string, 0, len(registrations))
	configured := false
	for _, r := range registrations {
		providers = append(providers, r.Provider)
		configured = configured || c.registrationChannel(r) != ""
	}
	if !configured {
		// Reject before publishing a routing snapshot that this server cannot
		// register. The previous working device report remains authoritative.
		return "", &GatewayError{Kind: GatewayErrorInvalid, Code: "push_channel_unavailable"}
	}
	previous, hasPrevious := decodePushBinding(cmd.SendToken)
	var existing []upstreamEndpoint
	if hasPrevious {
		var retired bool
		var err error
		existing, retired, err = c.installationEndpointSnapshot(ctx, previous.InstallationID)
		if err != nil {
			return "", err
		}
		rotate := retired
		for _, pending := range previous.PendingCreates {
			if expiry, err := time.Parse(time.RFC3339Nano, pending.Request.ExpiresAt); err == nil && !expiry.After(time.Now()) {
				rotate = true
			}
			for _, registration := range registrations {
				if registration.Provider == pending.Provider && c.registrationChannel(registration) != pending.Request.ChannelID {
					rotate = true
				}
			}
		}
		for _, endpoint := range existing {
			if endpoint.Status != "active" {
				continue
			}
			if endpoint.ExpiresAt != nil && !endpoint.ExpiresAt.After(time.Now()) {
				rotate = true
			}
			// Channel IDs belong to an endpoint for its entire lifetime. Stage a
			// fresh binding before writing endpoints when server configuration changes.
			for _, registration := range registrations {
				channel := c.registrationChannel(registration)
				if endpoint.Provider == registration.Provider && channel != "" && endpoint.ConfigID != channel {
					rotate = true
				}
			}
		}
		// If the final endpoint was revoked during an interrupted rotation, its
		// installation binding is permanently closed and needs a new reference.
		if !rotate && len(existing) == 0 && len(previous.EndpointIDs) > 0 {
			rotate = true
		}
		if !rotate {
			for _, endpoint := range existing {
				if endpoint.Status == "active" {
					rotate = false
					break
				}
				rotate = true
			}
		}
		if rotate {
			if !previous.RegistrationPending {
				// Return the old cleanup reference without retiring anything yet.
				// The grant service commits this pause, then repeats preparation.
				previous.RegistrationPending = true
				raw, _ := json.Marshal(previous)
				return string(raw), errBindingRetirementNeedsPause
			}
			if err := c.RevokeEndpoint(ctx, cmd.SendToken); err != nil {
				return "", err
			}
			hasPrevious = false
			existing = nil
		}
	}
	ref := pushReference("installation\x00" + uuid.NewString())
	if hasPrevious {
		ref = previous.RegistrationRef
		if ref == "" {
			ref = pushReference("installation\x00" + cmd.GatewayGrantID)
		}
	}
	report := map[string]any{"installation_ref": ref, "manufacturer": cmd.Manufacturer, "model": cmd.Model, "platform": cmd.Platform, "push_capability": "supported", "registered_providers": providers}
	var device struct {
		ID string `json:"id"`
	}
	if err := c.request(ctx, http.MethodPost, "/v1/devices", report, http.StatusOK, &device); err != nil {
		return "", err
	}
	if !validEndpointID(device.ID) {
		return "", &GatewayError{Kind: GatewayErrorRetry, Code: "push_response_invalid"}
	}
	binding := pushBinding{InstallationID: device.ID, RegistrationRef: ref, Discover: true, BindingRef: pushReference("binding\x00" + ref), EndpointIDs: []string{}, RegistrationPending: true}
	if hasPrevious {
		binding.BindingRef = previous.BindingRef
		if binding.BindingRef == "" {
			binding.BindingRef = pushReference("binding\x00" + cmd.GatewayGrantID)
		}
	}
	if hasPrevious {
		binding.EndpointIDs = previous.EndpointIDs
		for _, pending := range previous.PendingCreates {
			if !pending.Rejected {
				// Once a plan has survived a prior registration round, its POST
				// may still be running. A later rejection cannot settle that uncertainty.
				pending.FirstAttempt = false
				binding.PendingCreates = append(binding.PendingCreates, pending)
			}
		}
	}
	for _, registration := range registrations {
		channel := c.registrationChannel(registration)
		usable := channel != "" && slices.ContainsFunc(existing, func(e upstreamEndpoint) bool {
			return e.Status == "active" && e.Provider == registration.Provider && e.ConfigID == channel && (e.ExpiresAt == nil || e.ExpiresAt.After(time.Now()))
		})
		if usable {
			binding.RegistrationPending = false
		}
		if channel == "" || usable || slices.ContainsFunc(binding.PendingCreates, func(p pendingEndpointCreate) bool { return p.Provider == registration.Provider }) {
			continue
		}
		binding.PendingCreates = append(binding.PendingCreates, pendingEndpointCreate{Provider: registration.Provider, FirstAttempt: true, Request: endpointCreateRequest{
			IdempotencyKey:  pushReference("endpoint\x00" + uuid.NewString()),
			InstallationRef: ref, BindingRef: binding.BindingRef, ChannelID: channel,
			Platform: cmd.Platform, Token: registration.Token,
			ExpiresAt: cmd.ExpiresAt.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano),
		}})
	}
	raw, _ := json.Marshal(binding)
	return string(raw), nil
}

func (c *HTTPPushServiceClient) RegisterEndpoint(ctx context.Context, cmd RegisterGrantCommand) (string, error) {
	if len(cmd.Registrations) == 0 {
		return "", &GatewayError{Kind: GatewayErrorRetry, Code: "push_device_token_unavailable"}
	}
	if !validDeviceRegistrations(cmd) {
		return "", &GatewayError{Kind: GatewayErrorInvalid, Code: "invalid_device_report"}
	}
	prepared, ok := decodePushBinding(cmd.SendToken)
	if !ok {
		return "", &GatewayError{Kind: GatewayErrorInvalid, Code: "push_registration_not_prepared"}
	}
	registrations := slices.Clone(cmd.Registrations)
	slices.SortFunc(registrations, func(a, b ProviderRegistration) int { return strings.Compare(a.Provider, b.Provider) })
	existing, err := c.installationEndpoints(ctx, prepared.InstallationID)
	if err != nil {
		return "", err
	}
	ref := prepared.RegistrationRef
	if ref == "" {
		ref = pushReference("installation\x00" + cmd.GatewayGrantID)
	}
	binding := pushBinding{InstallationID: prepared.InstallationID, RegistrationRef: ref, BindingRef: prepared.BindingRef, Discover: true, EndpointIDs: []string{}}
	for _, r := range registrations {
		channel := c.registrationChannel(r)
		if channel == "" {
			continue
		}
		endpointID := ""
		for _, endpoint := range existing {
			if endpoint.Status != "active" || endpoint.Provider != r.Provider {
				continue
			}
			if endpoint.ConfigID != channel {
				return "", &GatewayError{Kind: GatewayErrorInvalid, Code: "push_channel_changed"}
			}
			if err := c.request(ctx, http.MethodPut, "/v1/endpoints/"+endpoint.ID, map[string]any{
				"expected_version": endpoint.Version, "token": r.Token,
				"expires_at": cmd.ExpiresAt.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano),
			}, http.StatusOK, nil); err != nil {
				return "", err
			}
			endpointID = endpoint.ID
			break
		}
		if endpointID != "" {
			binding.EndpointIDs = append(binding.EndpointIDs, endpointID)
			continue
		}
		index := slices.IndexFunc(prepared.PendingCreates, func(p pendingEndpointCreate) bool { return p.Provider == r.Provider && p.Request.ChannelID == channel })
		if index < 0 {
			return "", &GatewayError{Kind: GatewayErrorRetry, Code: "push_registration_not_prepared"}
		}
		request := prepared.PendingCreates[index].Request
		id, err := c.createEndpoint(ctx, request)
		if err != nil {
			if prepared.PendingCreates[index].FirstAttempt && definitivelyRejectedCreation(err) {
				prepared.PendingCreates[index].Rejected = true
				raw, _ := json.Marshal(prepared)
				// The caller commits this settlement even though registration fails.
				return string(raw), err
			}
			return "", err
		}
		if request.Token != r.Token || request.ExpiresAt != cmd.ExpiresAt.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano) {
			// A retry may carry refreshed tokens/lease. First settle the original
			// request; the next pass finds its endpoint and updates it with CAS.
			return "", &GatewayError{Kind: GatewayErrorRetry, Code: "push_registration_changed"}
		}
		binding.EndpointIDs = append(binding.EndpointIDs, id)
	}
	// Provider removal can overtake a timed-out creation too. Settle and revoke
	// every omitted request before discarding the persisted registration plan.
	for _, pending := range prepared.PendingCreates {
		if pending.Rejected {
			continue
		}
		if slices.ContainsFunc(registrations, func(r ProviderRegistration) bool {
			return r.Provider == pending.Provider && c.registrationChannel(r) == pending.Request.ChannelID
		}) {
			continue
		}
		// Revoke only the omitted creation; other providers remain in this binding.
		if err := c.revokePendingCreation(ctx, pending.Request); err != nil {
			return "", err
		}
	}
	slices.Sort(binding.EndpointIDs)
	var handoff struct {
		Token string `json:"handoff_token"`
	}
	if err := c.request(ctx, http.MethodPost, "/v1/devices/"+binding.InstallationID+"/handoff", struct{}{}, http.StatusOK, &handoff); err != nil {
		return "", err
	}
	if !validHandoffToken(handoff.Token) {
		return "", &GatewayError{Kind: GatewayErrorRetry, Code: "push_response_invalid"}
	}
	binding.HandoffToken = handoff.Token
	raw, _ := json.Marshal(binding)
	return string(raw), nil
}

func validHandoffToken(token string) bool {
	return len(token) > 20 && len(token) <= 4096 && strings.HasPrefix(token, "mph_") && !strings.ContainsFunc(token, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	})
}

func (c *HTTPPushServiceClient) RedeemHandoffs(ctx context.Context, tokens []string) error {
	for _, token := range tokens {
		err := c.request(ctx, http.MethodPost, "/v1/device-handoffs/redeem", map[string]string{"handoff_token": token}, http.StatusOK, nil)
		var upstream *GatewayError
		if errors.As(err, &upstream) && upstream.Code == "invalid_handoff" {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func definitivelyRejectedCreation(err error) bool {
	var upstream *GatewayError
	if !errors.As(err, &upstream) {
		return false
	}
	// Only explicit admission rejections prove that this first POST created no
	// endpoint. Transport errors, 5xx responses and conflicts remain unresolved.
	return (upstream.StatusCode == http.StatusBadRequest && upstream.Code == "invalid_endpoint_token") ||
		(upstream.StatusCode == http.StatusConflict && upstream.Code == "channel_not_ready") ||
		(upstream.StatusCode == http.StatusUnprocessableEntity && (upstream.Code == "unsupported_capability" || upstream.Code == "unsupported_platform"))
}

func (c *HTTPPushServiceClient) createEndpoint(ctx context.Context, request endpointCreateRequest) (string, error) {
	var result struct {
		EndpointID string `json:"endpoint_id"`
	}
	if err := c.request(ctx, http.MethodPost, "/v1/endpoints", request, http.StatusCreated, &result); err != nil {
		return "", err
	}
	if !validEndpointID(result.EndpointID) {
		return "", &GatewayError{Kind: GatewayErrorRetry, Code: "push_response_invalid"}
	}
	return result.EndpointID, nil
}

func (c *HTTPPushServiceClient) revokePendingCreation(ctx context.Context, request endpointCreateRequest) error {
	id, err := c.createEndpoint(ctx, request)
	var upstream *GatewayError
	// These rejections are checked after the registration key lock upstream.
	// A closed binding or expired request cannot produce a later new endpoint.
	if errors.As(err, &upstream) && (upstream.Code == "binding_retired" || upstream.Code == "endpoint_expired") {
		return nil
	}
	if err != nil {
		return err
	}
	return c.RevokeEndpoint(ctx, id)
}

type upstreamEndpoint struct {
	ID             string     `json:"id"`
	InstallationID string     `json:"installation_id"`
	Provider       string     `json:"provider"`
	ConfigID       string     `json:"channel_config_id"`
	Status         string     `json:"status"`
	Version        int64      `json:"version"`
	ExpiresAt      *time.Time `json:"expires_at"`
}

// Read current versions for CAS updates and recover registrations whose response
// or local transaction was lost. The upstream owns token/provider uniqueness.
func (c *HTTPPushServiceClient) installationEndpoints(ctx context.Context, installationID string) ([]upstreamEndpoint, error) {
	endpoints, _, err := c.installationEndpointSnapshot(ctx, installationID)
	return endpoints, err
}

func (c *HTTPPushServiceClient) installationEndpointSnapshot(ctx context.Context, installationID string) ([]upstreamEndpoint, bool, error) {
	var endpoints []upstreamEndpoint
	for page := 1; page <= 100; page++ {
		var result struct {
			DeviceID   string             `json:"device_id"`
			HasMore    bool               `json:"has_more"`
			Items      []upstreamEndpoint `json:"items"`
			Generation string             `json:"installation_generation"`
		}
		if err := c.request(ctx, http.MethodGet, fmt.Sprintf("/v1/devices/%s/endpoints?page=%d", installationID, page), nil, http.StatusOK, &result); err != nil {
			return nil, false, err
		}
		if result.DeviceID != installationID {
			return nil, false, &GatewayError{Kind: GatewayErrorRetry, Code: "push_response_invalid"}
		}
		for _, endpoint := range result.Items {
			if !validEndpointID(endpoint.ID) || endpoint.InstallationID != installationID || endpoint.Version < 1 {
				return nil, false, &GatewayError{Kind: GatewayErrorRetry, Code: "push_response_invalid"}
			}
			endpoints = append(endpoints, endpoint)
		}
		if !result.HasMore {
			return endpoints, len(endpoints) == 0 && result.Generation != "" && result.Generation != "0", nil
		}
	}
	return nil, false, &GatewayError{Kind: GatewayErrorInvalid, Code: "push_endpoint_limit"}
}

func (c *HTTPPushServiceClient) RevokeEndpoint(ctx context.Context, reference string) error {
	binding, ok := decodePushBinding(reference)
	if !ok {
		if !validEndpointID(reference) {
			return &GatewayError{Kind: GatewayErrorInvalid, Code: "push_registration_required"}
		}
		binding.EndpointIDs = []string{reference}
	}
	for _, pending := range binding.PendingCreates {
		if pending.Rejected {
			continue
		}
		if err := c.revokePendingCreation(ctx, pending.Request); err != nil {
			return err
		}
	}
	if binding.Discover {
		endpoints, err := c.installationEndpoints(ctx, binding.InstallationID)
		if err != nil {
			return err
		}
		for _, endpoint := range endpoints {
			if endpoint.Status == "active" && !slices.Contains(binding.EndpointIDs, endpoint.ID) {
				binding.EndpointIDs = append(binding.EndpointIDs, endpoint.ID)
			}
		}
	}
	for _, id := range binding.EndpointIDs {
		err := c.request(ctx, http.MethodDelete, "/v1/endpoints/"+id, nil, http.StatusOK, nil)
		var failure *GatewayError
		if errors.As(err, &failure) && failure.Kind == GatewayErrorRevoked {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *HTTPPushServiceClient) Send(ctx context.Context, grantID, endpointID, idempotencyKey string, notification NotificationRequest) error {
	binding, ok := decodePushBinding(endpointID)
	if !ok || len(binding.EndpointIDs) == 0 {
		return &GatewayError{Kind: GatewayErrorRevoked, Code: "push_registration_required"}
	}
	if notification.ExpiresAt.IsZero() {
		return &GatewayError{Kind: GatewayErrorInvalid, Code: "push_expiry_required"}
	}
	body := map[string]any{
		"idempotency_key": pushReference(idempotencyKey), "installation_ids": []string{binding.InstallationID},
		"kind": "alert", "category": "transaction", "priority": "normal",
		"title": "即应", "body": "你收到一条新消息",
		"expires_at": notification.ExpiresAt.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano),
		"data":       map[string]string{"event": "message_created", "grant_id": grantID, "route_token": notification.RouteToken},
	}
	return c.request(ctx, http.MethodPost, "/v1/messages", body, http.StatusAccepted, nil)
}

func (c *HTTPPushServiceClient) request(ctx context.Context, method, path string, body any, status int, result any) error {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return &GatewayError{Kind: GatewayErrorInvalid, Code: "push_request_invalid"}
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.options.URL+path, bytes.NewReader(encoded))
	if err != nil {
		return &GatewayError{Kind: GatewayErrorInvalid, Code: "push_request_invalid"}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(req)
	if err != nil {
		return &GatewayError{Kind: GatewayErrorRetry, Code: "push_service_unavailable"}
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (128<<10)+1))
	if err != nil || len(raw) > 128<<10 {
		return &GatewayError{Kind: GatewayErrorRetry, Code: "push_response_invalid"}
	}
	if response.StatusCode == status {
		if result != nil && json.Unmarshal(raw, result) != nil {
			return &GatewayError{Kind: GatewayErrorRetry, Code: "push_response_invalid"}
		}
		return nil
	}
	var failure struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &failure)
	code := failure.Error
	if !pushErrorCodePattern.MatchString(code) {
		code = fmt.Sprintf("push_http_%d", response.StatusCode)
	}
	kind := GatewayErrorInvalid
	switch {
	case code == "endpoint_version_conflict" || code == "no_eligible_endpoint" || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 || response.StatusCode == http.StatusUnauthorized:
		kind = GatewayErrorRetry
	case response.StatusCode == http.StatusGone || code == "endpoint_not_found" || code == "endpoint_inactive" || code == "endpoint_binding_inactive":
		kind = GatewayErrorRevoked
	}
	return &GatewayError{Kind: kind, Code: code, StatusCode: response.StatusCode}
}
