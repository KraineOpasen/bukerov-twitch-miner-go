package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// newAcceptanceManager builds a Manager (via the production NewManager
// constructor, Discord disabled) with the given push providers injected
// directly into messageProviders/batchers — same package, unexported fields,
// no environment variables needed — so each acceptance test can drive both
// the batcher/slog leg and the TestAllProviders DTO leg against the same
// provider instance.
func newAcceptanceManager(t *testing.T, providers ...MessageProvider) (*Manager, map[string]*Batcher) {
	t.Helper()
	m, _ := newManager(t, config.DiscordSettings{})
	m.messageProviders = providers
	batchers := make(map[string]*Batcher, len(providers))
	for _, p := range providers {
		batchers[p.Name()] = NewBatcher(p.Name(), BatchConfig{Enabled: true, Interval: time.Hour, MaxEntries: 20}, p.Send)
	}
	m.batchers = batchers
	return m, batchers
}

// TestAcceptanceGotifyTransportFailureNeverExposesToken
//
// Given a Gotify provider configured with a sentinel GOTIFY_TOKEN
// When the underlying HTTP transport fails (connection refused)
// Then neither the batcher's flush log line nor the TestAllProviders JSON DTO
// contains the token, in any form, while the failure is still reported as a
// classified, non-OK result.
func TestAcceptanceGotifyTransportFailureNeverExposesToken(t *testing.T) {
	withFakeHTTPClient(t, &fakeErrTransport{err: errors.New("connection refused")})
	buf := captureLogs(t)

	p := &GotifyProvider{serverURL: "http://" + sentinelMain + ".invalid", token: sentinelGotifyToken}
	m, batchers := newAcceptanceManager(t, p)

	// Leg 1: provider -> batcher -> slog (the same path a real notification
	// event takes through dispatchPush).
	ctx := context.Background()
	if err := batchers["gotify"].Add(ctx, BatchEvent{Type: NotificationTypeOnline, Group: "streamerA", Line: "line"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	batchers["gotify"].Flush(ctx)
	assertNoSentinel(t, buf.String(), sentinelGotifyToken, sentinelMain)

	// Leg 2: provider -> Manager.TestAllProviders -> DTO -> JSON (the same
	// path POST /api/test-notification serves).
	results := m.TestAllProviders(ctx)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	res := results[0]
	if res.OK {
		t.Fatal("expected OK = false for a forced transport failure")
	}
	if res.Stage == "" || res.Class == "" {
		t.Errorf("expected Stage/Class populated, got Stage=%q Class=%q", res.Stage, res.Class)
	}
	assertNoSentinel(t, res.Error, sentinelGotifyToken, sentinelMain)

	encoded, err := json.Marshal(results)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	assertNoSentinel(t, string(encoded), sentinelGotifyToken, sentinelMain)
}

// TestAcceptanceWebhookFailureNeverExposesConfiguredURL
//
// Given a Webhook provider configured with a sentinel WEBHOOK_URL that
// carries userinfo, host, path, query, and fragment secrets simultaneously
// When the underlying HTTP transport fails (connection refused)
// Then neither the batcher's flush log line nor the TestAllProviders JSON DTO
// contains any component of the URL, in any form.
func TestAcceptanceWebhookFailureNeverExposesConfiguredURL(t *testing.T) {
	withFakeHTTPClient(t, &fakeErrTransport{err: errors.New("connection refused")})
	buf := captureLogs(t)

	p := &WebhookProvider{url: sentinelWebhookURL}
	m, batchers := newAcceptanceManager(t, p)

	ctx := context.Background()
	if err := batchers["webhook"].Add(ctx, BatchEvent{Type: NotificationTypeOnline, Group: "streamerA", Line: "line"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	batchers["webhook"].Flush(ctx)
	assertNoSentinel(t, buf.String(), allSentinels...)

	results := m.TestAllProviders(ctx)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	res := results[0]
	if res.OK {
		t.Fatal("expected OK = false for a forced transport failure")
	}
	assertNoSentinel(t, res.Error, allSentinels...)

	encoded, err := json.Marshal(results)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	assertNoSentinel(t, string(encoded), allSentinels...)
}

// TestAcceptanceRemoteResponseReflectsCredential
//
// Given a Webhook provider pointed at a remote endpoint that answers with a
// non-2xx status and a body reflecting the full request URL plus an
// unrelated response-body secret
// When Send is invoked (directly, and via TestAllProviders)
// Then the resulting error/DTO reports the status code but never the
// reflected URL or response-body secret — the body is drained and discarded,
// never read into any error.
func TestAcceptanceRemoteResponseReflectsCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("error handling " + r.URL.String() + " (" + sentinelResponseBody + ")"))
	}))
	defer srv.Close()

	p := &WebhookProvider{url: srv.URL + "/" + sentinelWebhookPath + "?q=" + sentinelWebhookQuery}
	m, _ := newAcceptanceManager(t, p)

	err := p.Send(context.Background(), Message{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("expected a non-2xx error")
	}
	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("expected *SendError, got %T", err)
	}
	if se.Stage() != StageResponse || se.StatusCode() != http.StatusInternalServerError {
		t.Errorf("Stage/Status = %v/%d, want StageResponse/500", se.Stage(), se.StatusCode())
	}
	assertNoSentinel(t, err.Error(), sentinelWebhookPath, sentinelWebhookQuery, sentinelResponseBody)

	results := m.TestAllProviders(context.Background())
	if len(results) != 1 || results[0].OK {
		t.Fatalf("results = %v, want exactly one non-OK result", results)
	}
	assertNoSentinel(t, results[0].Error, sentinelWebhookPath, sentinelWebhookQuery, sentinelResponseBody)
}

// TestAcceptanceSuccessfulDeliveryRemainsUnchanged
//
// Given Gotify and Webhook providers pointed at real, healthy endpoints
// When a message is sent (directly, and via TestAllProviders)
// Then delivery succeeds exactly as before the fix: the real token/URL still
// reach the destination unchanged, and the DTO reports OK with no error text.
func TestAcceptanceSuccessfulDeliveryRemainsUnchanged(t *testing.T) {
	var gotifyQuery string
	gotifySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotifyQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer gotifySrv.Close()

	var webhookHit bool
	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webhookHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookSrv.Close()

	gp := &GotifyProvider{serverURL: gotifySrv.URL, token: sentinelGotifyToken}
	wp := &WebhookProvider{url: webhookSrv.URL + "/hook"}
	m, _ := newAcceptanceManager(t, gp, wp)

	results := m.TestAllProviders(context.Background())
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	for _, res := range results {
		if !res.OK {
			t.Errorf("provider %q: OK = false, want true; Error = %q", res.Provider, res.Error)
		}
		if res.Error != "" {
			t.Errorf("provider %q: Error = %q, want empty", res.Provider, res.Error)
		}
	}

	if !webhookHit {
		t.Error("expected the webhook target to have been hit")
	}
	if !strings.Contains(gotifyQuery, "token="+sentinelGotifyToken) {
		t.Errorf("expected the real Gotify token to still travel in the query string, got %q", gotifyQuery)
	}
}
