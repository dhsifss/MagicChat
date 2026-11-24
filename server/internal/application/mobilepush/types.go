package mobilepush

import (
	"context"
	"time"
)

const (
	GrantStatusActive      = "active"
	GrantStatusRegistering = "registering"
	GrantStatusDisabled    = "disabled"

	EventStatusQueued    = "queued"
	EventStatusExpanding = "expanding"
	EventStatusRetry     = "retry"
	EventStatusExpired   = "expired"

	JobStatusQueued     = "queued"
	JobStatusSending    = "sending"
	JobStatusRetry      = "retry"
	JobStatusSent       = "sent"
	JobStatusFailed     = "failed"
	JobStatusExpired    = "expired"
	JobStatusSuppressed = "suppressed"
)

type ProviderRegistration struct {
	Provider    string `json:"provider"`
	Token       string `json:"token"`
	Environment string `json:"environment,omitempty"`
}

type RegisterGrantCommand struct {
	ClientID       string
	Manufacturer   string
	Model          string
	Registrations  []ProviderRegistration
	UserID         string
	SessionID      string
	InstallationID string
	GatewayGrantID string
	SendToken      string
	Platform       string
	ExpiresAt      time.Time
	HandoffTokens  []string
}

type Grant struct {
	InstallationID string
	GatewayGrantID string
	Platform       string
	ExpiresAt      time.Time
	HandoffToken   string
}

type EndpointHandoffClient interface {
	RedeemHandoffs(context.Context, []string) error
}

type Route struct {
	ConversationID string `json:"conversation_id"`
	MessageID      string `json:"message_id"`
}

type NotificationRequest struct {
	Event       string    `json:"event"`
	RouteToken  string    `json:"route_token"`
	CollapseKey string    `json:"collapse_key,omitempty"`
	TTLSeconds  int       `json:"ttl_seconds"`
	ExpiresAt   time.Time `json:"-"`
}

type GatewayErrorKind string

const (
	GatewayErrorRetry   GatewayErrorKind = "retry"
	GatewayErrorRevoked GatewayErrorKind = "revoked"
	GatewayErrorInvalid GatewayErrorKind = "invalid"
)

type GatewayError struct {
	Kind       GatewayErrorKind
	Code       string
	StatusCode int
	Err        error
}

func (e *GatewayError) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Err.Error()
	}
	return e.Code
}

func (e *GatewayError) Unwrap() error { return e.Err }

type EndpointRegistrationPreparer interface {
	PrepareRegistration(context.Context, RegisterGrantCommand) (string, error)
}

type EndpointClient interface {
	// On failure, a nonempty reference contains registration settlement that
	// must be committed before the error is returned to the client.
	RegisterEndpoint(context.Context, RegisterGrantCommand) (string, error)
	RevokeEndpoint(context.Context, string) error
}

type GatewayClient interface {
	Send(context.Context, string, string, string, NotificationRequest) error
}

type PushPreferences struct {
	PauseWhenDesktopActive bool `json:"pause_when_desktop_active"`
}

type PresenceCommand struct {
	UserID     string
	SessionID  string
	Platform   string
	Foreground bool
}

type PushJobStatus struct {
	Status    string    `json:"status"`
	Reason    string    `json:"reason"`
	UpdatedAt time.Time `json:"updated_at"`
}

type PushStatus struct {
	Enabled       bool           `json:"enabled"`
	Registered    bool           `json:"registered"`
	Foreground    bool           `json:"foreground"`
	DesktopActive bool           `json:"desktop_active"`
	DelaySeconds  int            `json:"delay_seconds"`
	LastJob       *PushJobStatus `json:"last_job"`
}

type ClientService interface {
	RegisterGrant(context.Context, RegisterGrantCommand) (Grant, error)
	RevokeGrant(context.Context, string, string, string) error
	ResolveRoute(context.Context, string, string) (Route, error)
	ReportPresence(context.Context, PresenceCommand) error
	GetPreferences(context.Context, string) (PushPreferences, error)
	SetPreferences(context.Context, string, PushPreferences) (PushPreferences, error)
	GetStatus(context.Context, string, string) (PushStatus, error)
}
