package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// TestTestAllProvidersNeverLeaksSentinels injects sentinel-configured Gotify
// and Webhook providers directly into a Manager's messageProviders (same
// package, unexported field — no environment variables needed) and forces
// both to fail via the fake transport. TestAllProviders' results — the exact
// values that cross the JSON API boundary in
// internal/web/handlers_notifications.go — must contain neither sentinel, in
// any form, while still reporting a populated Stage/Class per provider.
func TestTestAllProvidersNeverLeaksSentinels(t *testing.T) {
	withFakeHTTPClient(t, &fakeErrTransport{err: errors.New("connection refused")})

	m, _ := newManager(t, config.DiscordSettings{}) // Discord disabled -> m.discord stays nil
	m.messageProviders = []MessageProvider{
		&GotifyProvider{serverURL: "http://" + sentinelMain + ".invalid", token: sentinelGotifyToken},
		&WebhookProvider{url: sentinelWebhookURL},
	}

	results := m.TestAllProviders(context.Background())
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}

	for _, res := range results {
		if res.OK {
			t.Errorf("provider %q: OK = true, want false (transport is forced to fail)", res.Provider)
		}
		if res.Stage == "" {
			t.Errorf("provider %q: Stage is empty, want it populated", res.Provider)
		}
		if res.Class == "" {
			t.Errorf("provider %q: Class is empty, want it populated", res.Provider)
		}
		assertNoSentinel(t, res.Error, allSentinels...)
		assertNoSentinel(t, res.Provider, allSentinels...)
	}

	encoded, err := json.Marshal(results)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	assertNoSentinel(t, string(encoded), allSentinels...)
}

// TestNewProviderTestFailureCopiesSendErrorFields checks the happy path: a
// *SendError's already-classified, safe fields are copied verbatim.
func TestNewProviderTestFailureCopiesSendErrorFields(t *testing.T) {
	se := newTransportError("webhook", "send", &net.OpError{Op: "dial", Err: errors.New("refused")})
	res := newProviderTestFailure("webhook", se)

	if res.Provider != "webhook" {
		t.Errorf("Provider = %q, want %q", res.Provider, "webhook")
	}
	if res.Error != se.Error() {
		t.Errorf("Error = %q, want %q", res.Error, se.Error())
	}
	if res.Stage != string(StageTransport) {
		t.Errorf("Stage = %q, want %q", res.Stage, StageTransport)
	}
	if res.Class != string(ClassConnect) {
		t.Errorf("Class = %q, want %q", res.Class, ClassConnect)
	}
	if res.Status != 0 {
		t.Errorf("Status = %d, want 0", res.Status)
	}
}

// TestNewProviderTestFailureFailsClosedForNonSendError is the fail-closed
// branch: a provider that returns anything other than a *SendError (a
// regression) must never have err.Error() read — the result is a fixed
// message, and no other field is populated from the raw error.
func TestNewProviderTestFailureFailsClosedForNonSendError(t *testing.T) {
	plain := errors.New("some raw provider error: " + sentinelWebhookURL)
	res := newProviderTestFailure("webhook", plain)

	if res.Provider != "webhook" {
		t.Errorf("Provider = %q, want %q", res.Provider, "webhook")
	}
	if res.Error != "delivery failed" {
		t.Errorf("Error = %q, want %q", res.Error, "delivery failed")
	}
	if res.Stage != "" || res.Class != "" || res.Status != 0 {
		t.Errorf("expected Stage/Class/Status to stay zero-valued, got %+q/%+q/%d", res.Stage, res.Class, res.Status)
	}
	assertNoSentinel(t, res.Error, allSentinels...)

	// Also cover a wrapped *url.Error specifically, since that's the exact
	// regression shape this helper exists to defend against.
	wrapped := &url.Error{Op: "Post", URL: sentinelWebhookURL, Err: errors.New("boom")}
	res2 := newProviderTestFailure("webhook", wrapped)
	if res2.Error != "delivery failed" {
		t.Errorf("Error = %q, want %q", res2.Error, "delivery failed")
	}
}
