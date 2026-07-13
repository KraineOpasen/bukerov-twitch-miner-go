package notifications

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"
)

// Additional event types used by the push providers and the batcher. They live
// alongside the Discord-oriented types in provider.go and are what the
// batching "immediate events" list is matched against.
const (
	NotificationTypeDropClaim NotificationType = "drop_claim"
	NotificationTypeBetWin    NotificationType = "bet_win"
	NotificationTypeBetLose   NotificationType = "bet_lose"
)

// Message is a provider-agnostic notification payload. Unlike Notification
// (which carries Discord-specific routing such as ChannelID), Message targets
// the "push" providers that deliver plain title/body text to a single
// preconfigured destination.
type Message struct {
	// Type identifies the event and is used for batching decisions.
	Type NotificationType

	// Title is the short headline of the message.
	Title string

	// Body is the message text. It may contain several newline-joined lines
	// when produced by the batcher.
	Body string
}

// MessageProvider is the interface implemented by the push notification
// providers (Matrix, Pushover, Gotify, generic webhook). It is deliberately
// simpler than Provider: these services take a title/body and deliver it to a
// single destination configured entirely through environment variables, so
// there is no channel discovery or long-lived connection to manage.
type MessageProvider interface {
	// Name returns the provider's identifier (e.g. "matrix").
	Name() string

	// IsConfigured reports whether the provider has all required settings.
	IsConfigured() bool

	// Send delivers a single message to the provider's configured destination.
	Send(ctx context.Context, msg Message) error
}

// httpClient is the shared HTTP client used by the push providers. The timeout
// keeps a slow or unreachable endpoint from blocking a notification goroutine
// indefinitely.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// envForAccount reads an environment variable following the project's
// per-account override convention: it first looks for "<base>_<USERNAME>"
// (uppercased) and falls back to the global "<base>" when the account-specific
// variable is unset. An empty username skips the override lookup.
func envForAccount(base, username string) string {
	if username != "" {
		key := base + "_" + strings.ToUpper(username)
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return strings.TrimSpace(os.Getenv(base))
}

// AnyMessageProviderConfigured reports whether at least one push provider is
// configured through the environment for the given account. It lets callers
// decide whether to spin up the notification manager even when Discord is
// disabled.
func AnyMessageProviderConfigured(username string) bool {
	for _, p := range NewMessageProvidersFromEnv(username) {
		if p.IsConfigured() {
			return true
		}
	}
	return false
}

// NewMessageProvidersFromEnv builds every push provider from the environment,
// using username for the per-account override fallback. Providers that are not
// configured are still returned (IsConfigured reports false); callers filter
// them via IsConfigured so that enabling a provider is purely a matter of
// setting its environment variables.
func NewMessageProvidersFromEnv(username string) []MessageProvider {
	return []MessageProvider{
		NewMatrixProviderFromEnv(username),
		NewPushoverProviderFromEnv(username),
		NewGotifyProviderFromEnv(username),
		NewWebhookProviderFromEnv(username),
	}
}
