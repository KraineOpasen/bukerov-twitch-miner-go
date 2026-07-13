package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GotifyProvider delivers notifications to a self-hosted Gotify server. It is
// configured through the GOTIFY_URL (server base URL) and GOTIFY_TOKEN
// (application token) environment variables (each with the optional
// "_<USERNAME>" per-account override).
type GotifyProvider struct {
	serverURL string
	token     string
}

// NewGotifyProviderFromEnv constructs a GotifyProvider from the environment.
func NewGotifyProviderFromEnv(username string) *GotifyProvider {
	return &GotifyProvider{
		serverURL: strings.TrimRight(envForAccount("GOTIFY_URL", username), "/"),
		token:     envForAccount("GOTIFY_TOKEN", username),
	}
}

// Name returns the provider's identifier.
func (p *GotifyProvider) Name() string { return "gotify" }

// IsConfigured reports whether both the server URL and app token are set.
func (p *GotifyProvider) IsConfigured() bool {
	return p.serverURL != "" && p.token != ""
}

// Send delivers a message to the configured Gotify server.
func (p *GotifyProvider) Send(ctx context.Context, msg Message) error {
	if !p.IsConfigured() {
		return fmt.Errorf("gotify not configured")
	}

	title := msg.Title
	if title == "" {
		title = "Twitch Points Miner"
	}

	payload, err := json.Marshal(map[string]any{
		"title":    title,
		"message":  msg.Body,
		"priority": 5,
	})
	if err != nil {
		return fmt.Errorf("failed to encode gotify payload: %w", err)
	}

	url := fmt.Sprintf("%s/message?token=%s", p.serverURL, p.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to build gotify request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("gotify request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("gotify returned %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	return nil
}
