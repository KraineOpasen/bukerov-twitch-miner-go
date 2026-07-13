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

// WebhookProvider delivers notifications as a JSON POST to an arbitrary URL. It
// is configured through the WEBHOOK_URL environment variable (with the optional
// "_<USERNAME>" per-account override). The posted JSON carries the event type,
// title and body so downstream consumers can route or format as they wish.
type WebhookProvider struct {
	url string
}

// NewWebhookProviderFromEnv constructs a WebhookProvider from the environment.
func NewWebhookProviderFromEnv(username string) *WebhookProvider {
	return &WebhookProvider{
		url: envForAccount("WEBHOOK_URL", username),
	}
}

// Name returns the provider's identifier.
func (p *WebhookProvider) Name() string { return "webhook" }

// IsConfigured reports whether a target URL is set.
func (p *WebhookProvider) IsConfigured() bool {
	return p.url != ""
}

// webhookPayload is the JSON body posted to the configured webhook URL.
type webhookPayload struct {
	Type    NotificationType `json:"type"`
	Title   string           `json:"title"`
	Message string           `json:"message"`
}

// Send posts the message to the configured webhook URL as JSON.
func (p *WebhookProvider) Send(ctx context.Context, msg Message) error {
	if !p.IsConfigured() {
		return fmt.Errorf("webhook not configured")
	}

	payload, err := json.Marshal(webhookPayload{
		Type:    msg.Type,
		Title:   msg.Title,
		Message: msg.Body,
	})
	if err != nil {
		return fmt.Errorf("failed to encode webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("webhook request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("webhook returned %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	return nil
}
