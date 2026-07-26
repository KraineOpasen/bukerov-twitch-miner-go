package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

	// p.url is itself the secret here (it may carry userinfo, a secret path,
	// query, or fragment) — it must never leave this function via a returned
	// error. Request-build and transport failures below are reported through
	// SendError, which discards the underlying *url.Error (and therefore this
	// URL) entirely — see senderror.go's package doc comment.
	const op = "send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(payload))
	if err != nil {
		return newRequestError(p.Name(), op)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return newTransportError(p.Name(), op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The response body is remote/attacker-controlled and must never enter
		// an error, log, or API response — drain and discard it.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return newResponseError(p.Name(), op, resp.StatusCode)
	}

	return nil
}
