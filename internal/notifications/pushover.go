package notifications

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// pushoverEndpoint is the Pushover message API URL.
const pushoverEndpoint = "https://api.pushover.net/1/messages.json"

// PushoverProvider delivers notifications through the Pushover API. It is
// configured through the PUSHOVER_TOKEN (application API token) and
// PUSHOVER_USER_KEY environment variables (each with the optional
// "_<USERNAME>" per-account override).
type PushoverProvider struct {
	token   string
	userKey string
}

// NewPushoverProviderFromEnv constructs a PushoverProvider from the environment.
func NewPushoverProviderFromEnv(username string) *PushoverProvider {
	return &PushoverProvider{
		token:   envForAccount("PUSHOVER_TOKEN", username),
		userKey: envForAccount("PUSHOVER_USER_KEY", username),
	}
}

// Name returns the provider's identifier.
func (p *PushoverProvider) Name() string { return "pushover" }

// IsConfigured reports whether both the app token and user key are set.
func (p *PushoverProvider) IsConfigured() bool {
	return p.token != "" && p.userKey != ""
}

// Send delivers a message through Pushover.
func (p *PushoverProvider) Send(ctx context.Context, msg Message) error {
	if !p.IsConfigured() {
		return fmt.Errorf("pushover not configured")
	}

	form := url.Values{}
	form.Set("token", p.token)
	form.Set("user", p.userKey)
	form.Set("message", msg.Body)
	if msg.Title != "" {
		form.Set("title", msg.Title)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pushoverEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("failed to build pushover request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("pushover request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("pushover returned %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	return nil
}
