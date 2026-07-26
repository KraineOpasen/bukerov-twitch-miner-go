package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
)

// Sentinels mirror internal/notifications' M5/M6 test sentinels exactly (test
// identifiers never cross a package boundary, even when exported, so this
// package restates them) — never real credentials.
const (
	sentinelMain            = "bkm-m5-m6-secret-sentinel-never-print"
	sentinelGotifyToken     = "bkm-gotify-token-never-print"
	sentinelWebhookUserInfo = "bkm-webhook-userinfo-never-print"
	sentinelWebhookPath     = "bkm-webhook-path-never-print"
	sentinelWebhookQuery    = "bkm-webhook-query-never-print"
	sentinelWebhookFragment = "bkm-webhook-fragment-never-print"
)

// allSentinels lists every sentinel string for exhaustive leak checks.
var allSentinels = []string{
	sentinelMain, sentinelGotifyToken, sentinelWebhookUserInfo,
	sentinelWebhookPath, sentinelWebhookQuery, sentinelWebhookFragment,
}

// assertNoSentinelInString fails the test if s contains any sentinel.
func assertNoSentinelInString(t *testing.T, s string) {
	t.Helper()
	for _, sentinel := range allSentinels {
		if strings.Contains(s, sentinel) {
			t.Errorf("leaked sentinel %q in: %s", sentinel, s)
		}
	}
}

// walkNoSentinel recursively walks a decoded JSON value (the shapes
// encoding/json produces: map[string]any, []any, string, and scalars) and
// fails the test if any string leaf contains a sentinel. This catches a leak
// nested at any depth, not just at fields the test happens to name.
func walkNoSentinel(t *testing.T, v any) {
	t.Helper()
	switch val := v.(type) {
	case string:
		assertNoSentinelInString(t, val)
	case map[string]any:
		for _, vv := range val {
			walkNoSentinel(t, vv)
		}
	case []any:
		for _, vv := range val {
			walkNoSentinel(t, vv)
		}
	}
}

// newNotificationsTestManager wires a *notifications.Manager against the
// shared test DB singleton (see TestMain in handlers_statistics_test.go) with
// Discord disabled, so only the push providers configured by the calling
// test's environment variables are active.
func newNotificationsTestManager(t *testing.T) *notifications.Manager {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	mgr, err := notifications.NewManager(nil, nil, db, nil, "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr
}

// TestHandleAPITestNotificationNeverLeaksSecrets drives the real
// GOTIFY_URL/GOTIFY_TOKEN/WEBHOOK_URL environment-configuration path (the
// same one production uses) into a guaranteed-fast local failure (port 1 is
// never listening), through the full handler, and asserts neither the raw
// response body nor any string anywhere in the decoded JSON contains a
// sentinel — while still naming both providers.
func TestHandleAPITestNotificationNeverLeaksSecrets(t *testing.T) {
	t.Setenv("GOTIFY_URL", "http://127.0.0.1:1")
	t.Setenv("GOTIFY_TOKEN", sentinelGotifyToken)
	webhookURL := "http://" + sentinelWebhookUserInfo + ":pw@127.0.0.1:1/" +
		sentinelWebhookPath + "?q=" + sentinelWebhookQuery + "#" + sentinelWebhookFragment
	t.Setenv("WEBHOOK_URL", webhookURL)

	mgr := newNotificationsTestManager(t)
	s := newRenderServer(t)
	s.SetNotificationManager(mgr)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/test-notification", nil)
	s.handleAPITestNotification(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	assertNoSentinelInString(t, body)

	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	walkNoSentinel(t, decoded)

	providers, ok := decoded["providers"].([]any)
	if !ok || len(providers) == 0 {
		t.Fatalf("expected a non-empty providers array, got %v", decoded["providers"])
	}
	var sawGotify, sawWebhook bool
	for _, raw := range providers {
		p, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("provider entry is not an object: %v", raw)
		}
		name, _ := p["provider"].(string)
		switch name {
		case "gotify":
			sawGotify = true
		case "webhook":
			sawWebhook = true
		}
		if errText, ok := p["error"].(string); ok && errText != "" {
			if errText != "delivery failed" && !strings.Contains(errText, name+" send failed") {
				t.Errorf("provider %q: unexpected error text shape: %q", name, errText)
			}
		}
	}
	if !sawGotify || !sawWebhook {
		t.Errorf("expected both gotify and webhook in the providers list, got %v", providers)
	}
}

// TestHandleAPITestNotificationSuccessPath is the positive control: pointing
// GOTIFY_URL/WEBHOOK_URL at real, healthy httptest servers must still report
// success with no error fields — the sanitization added for M5/M6 must not
// break the happy path.
func TestHandleAPITestNotificationSuccessPath(t *testing.T) {
	gotifySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer gotifySrv.Close()
	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookSrv.Close()

	t.Setenv("GOTIFY_URL", gotifySrv.URL)
	t.Setenv("GOTIFY_TOKEN", "irrelevant-for-this-test")
	t.Setenv("WEBHOOK_URL", webhookSrv.URL+"/hook")

	mgr := newNotificationsTestManager(t)
	s := newRenderServer(t)
	s.SetNotificationManager(mgr)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/test-notification", nil)
	s.handleAPITestNotification(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var decoded struct {
		Status    string                             `json:"status"`
		Providers []notifications.ProviderTestResult `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if decoded.Status != "ok" {
		t.Errorf("status = %q, want %q", decoded.Status, "ok")
	}
	if len(decoded.Providers) == 0 {
		t.Fatal("expected at least one provider result")
	}
	for _, p := range decoded.Providers {
		if !p.OK {
			t.Errorf("provider %q: OK = false, want true", p.Provider)
		}
		if p.Error != "" {
			t.Errorf("provider %q: Error = %q, want empty", p.Provider, p.Error)
		}
	}
}

// TestHandleAPINotificationsTestStaticError checks the Discord-only
// SendTestNotifications path: with no Discord provider connected, the
// handler must return a static 500 message — never err.Error() text, and
// certainly no sentinel.
func TestHandleAPINotificationsTestStaticError(t *testing.T) {
	mgr := newNotificationsTestManager(t) // Discord disabled -> discord == nil
	s := newRenderServer(t)
	s.SetNotificationManager(mgr)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/test", nil)
	s.handleAPINotificationsTest(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Failed to send test notifications") {
		t.Errorf("expected the static message, got: %s", body)
	}
	if strings.Contains(body, "discord not connected") {
		t.Errorf("expected err.Error() text to be absent, got: %s", body)
	}
	assertNoSentinelInString(t, body)
}

// TestSanitizeProviderTestResults is the table test for the fail-closed
// backstop: safe-looking messages (a static string, or a SendError's fixed
// grammar) pass through unchanged; anything that still looks URL- or
// credential-shaped is replaced.
func TestSanitizeProviderTestResults(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantEqual bool // true: Error should be unchanged; false: replaced with "delivery failed"
	}{
		{"static safe message", "no Discord channel configured", true},
		{"SendError response grammar", "gotify send failed: endpoint returned HTTP 500", true},
		{"SendError transport grammar", "webhook send failed: connect error", true},
		{"empty error", "", true},
		{"scheme separator", "webhook send failed: http://leaked.example/hook", false},
		{"token= query", "gotify send failed: token=abc123", false},
		{"percent-encoded TOKEN case-insensitive", "gotify send failed: TOKEN%3Dabc123", false},
		{"userinfo at-sign", "webhook send failed: user@leaked.example", false},
		{"percent-encoded path segment", "webhook send failed: %2Fsecret%2Fpath", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []notifications.ProviderTestResult{{Provider: "p", OK: false, Error: tc.in, Stage: "transport", Class: "connect"}}
			out := sanitizeProviderTestResults(in)
			if len(out) != 1 {
				t.Fatalf("len(out) = %d, want 1", len(out))
			}
			if tc.wantEqual {
				if out[0].Error != tc.in {
					t.Errorf("Error = %q, want unchanged %q", out[0].Error, tc.in)
				}
			} else {
				if out[0].Error != "delivery failed" {
					t.Errorf("Error = %q, want %q", out[0].Error, "delivery failed")
				}
			}
			// Whitelisted fields always pass through untouched.
			if out[0].Provider != "p" || out[0].Stage != "transport" || out[0].Class != "connect" {
				t.Errorf("whitelisted fields altered: %+v", out[0])
			}
		})
	}
}
