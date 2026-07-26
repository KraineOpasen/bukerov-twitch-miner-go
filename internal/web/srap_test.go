package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

// errBoom is a generic, non-sentinel apply failure — the "durable admission
// or persistence step itself failed" case, mapped to 500.
var errBoom = errors.New("boom: injected apply failure")

// TestSettingsPostFailingCallbackNo2xxCacheUnchanged pins the fail-closed
// POST /api/settings contract (M1 §6): a non-nil callback error must produce
// a non-2xx response with NO success body, and must NOT touch the server's
// refresh/daysAgo display cache — the previous behavior updated the cache
// and wrote {"status":"ok"} unconditionally, regardless of the callback's
// (nonexistent, pre-M1) outcome.
func TestSettingsPostFailingCallbackNo2xxCacheUnchanged(t *testing.T) {
	current := settings.RuntimeSettings{Analytics: settings.AnalyticsUIConfig{Refresh: 5, DaysAgo: 7}}
	srv := &Server{
		settingsProvider: &fakeSettingsProvider{rt: current},
		onSettingsUpdate: func(ctx context.Context, rt settings.RuntimeSettings) error { return errBoom },
		refresh:          5,
		daysAgo:          7,
	}

	rec := postSettings(t, srv, `{"analytics":{"refresh":999,"daysAgo":999}}`)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want non-2xx on a failing callback", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("success body leaked on a failing callback: %s", rec.Body.String())
	}
	srv.mu.RLock()
	gotRefresh, gotDaysAgo := srv.refresh, srv.daysAgo
	srv.mu.RUnlock()
	if gotRefresh != 5 || gotDaysAgo != 7 {
		t.Errorf("refresh/daysAgo cache changed on a failing callback: refresh=%d daysAgo=%d, want 5,7", gotRefresh, gotDaysAgo)
	}
}

// TestSettingsPostShuttingDownCallbackReturns503 pins the specific status
// mapping for settings.ErrShuttingDown (and, by the same code path,
// database.ErrClosed): both mean "retry is safe, nothing changed" -> 503,
// distinct from a hard apply failure's 500.
func TestSettingsPostShuttingDownCallbackReturns503(t *testing.T) {
	srv := &Server{
		settingsProvider: &fakeSettingsProvider{rt: settings.RuntimeSettings{}},
		onSettingsUpdate: func(ctx context.Context, rt settings.RuntimeSettings) error { return settings.ErrShuttingDown },
	}

	rec := postSettings(t, srv, `{}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for ErrShuttingDown", rec.Code)
	}
}

// TestSettingsPostGenericFailureReturns500 pins the non-shutdown mapping.
func TestSettingsPostGenericFailureReturns500(t *testing.T) {
	srv := &Server{
		settingsProvider: &fakeSettingsProvider{rt: settings.RuntimeSettings{}},
		onSettingsUpdate: func(ctx context.Context, rt settings.RuntimeSettings) error { return errBoom },
	}

	rec := postSettings(t, srv, `{}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for a generic apply failure", rec.Code)
	}
}

// TestSettingsPostNilCallbackReturns503 closes G22/G23: a provider wired
// without a callback (or a callback not yet wired) must refuse loudly, not
// silently do nothing and report 200.
func TestSettingsPostNilCallbackReturns503(t *testing.T) {
	srv := &Server{settingsProvider: &fakeSettingsProvider{rt: settings.RuntimeSettings{}}}

	rec := postSettings(t, srv, `{}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when onSettingsUpdate is nil", rec.Code)
	}
}

// TestSettingsPostSuccessBodyByteIdentical pins the success-path contract
// explicitly: on a nil callback error the response is still exactly
// {"status":"ok"} (writeSuccess), byte for byte, and the cache IS updated.
func TestSettingsPostSuccessBodyByteIdentical(t *testing.T) {
	srv := &Server{
		settingsProvider: &fakeSettingsProvider{rt: settings.RuntimeSettings{}},
		onSettingsUpdate: func(ctx context.Context, rt settings.RuntimeSettings) error { return nil },
	}

	rec := postSettings(t, srv, `{"analytics":{"refresh":11,"daysAgo":22}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "{\"status\":\"ok\"}\n" {
		t.Fatalf("success body = %q, want the unchanged {\"status\":\"ok\"} shape", got)
	}
	srv.mu.RLock()
	gotRefresh, gotDaysAgo := srv.refresh, srv.daysAgo
	srv.mu.RUnlock()
	if gotRefresh != 11 || gotDaysAgo != 22 {
		t.Errorf("cache not updated on success: refresh=%d daysAgo=%d, want 11,22", gotRefresh, gotDaysAgo)
	}
}

// TestSettingsResetFailingCallbackNon2xxCacheUnchanged pins the reset
// endpoint's fail-closed contract: a failing callback must NOT update the
// cache and must NOT echo the defaults back as if they were applied (the
// previous behavior always returned 200 + the defaults, unconditionally).
func TestSettingsResetFailingCallbackNon2xxCacheUnchanged(t *testing.T) {
	srv := &Server{
		settingsProvider: resetSettingsProvider{streamers: []config.StreamerConfig{{Username: "alice"}}},
		onSettingsUpdate: func(ctx context.Context, rt settings.RuntimeSettings) error { return errBoom },
		refresh:          5,
		daysAgo:          7,
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/settings/reset", nil)
	srv.handleAPISettingsReset(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want non-2xx on a failing reset callback", rec.Code)
	}
	srv.mu.RLock()
	gotRefresh, gotDaysAgo := srv.refresh, srv.daysAgo
	srv.mu.RUnlock()
	if gotRefresh != 5 || gotDaysAgo != 7 {
		t.Errorf("refresh/daysAgo cache changed on a failing reset callback: refresh=%d daysAgo=%d, want 5,7", gotRefresh, gotDaysAgo)
	}
}

// TestSettingsResetNilCallbackReturns503 mirrors the POST nil-callback guard
// for the reset endpoint.
func TestSettingsResetNilCallbackReturns503(t *testing.T) {
	srv := &Server{settingsProvider: resetSettingsProvider{}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/settings/reset", nil)
	srv.handleAPISettingsReset(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when onSettingsUpdate is nil", rec.Code)
	}
}

// quickActionTestServer builds a bare Server satisfying
// handleAPIStreamerQuickAction's requirements: one tracked streamer
// ("shroud") with valid default settings to materialize an override from.
func quickActionTestServer(callback settings.SettingsUpdateCallback) *Server {
	return &Server{
		settingsProvider: &fakeSettingsProvider{rt: settings.RuntimeSettings{
			Streamers:       []settings.StreamerConfig{{Username: "shroud"}},
			DefaultSettings: settings.StreamerSettingsToDTO(models.DefaultStreamerSettings()),
		}},
		onSettingsUpdate: callback,
	}
}

// TestQuickActionFailingCallbackNon2xx pins the quick-action endpoint's
// fail-closed contract: a failing callback must not report success.
func TestQuickActionFailingCallbackNon2xx(t *testing.T) {
	srv := quickActionTestServer(func(ctx context.Context, rt settings.RuntimeSettings) error { return errBoom })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/streamer-action/shroud", strings.NewReader(`{"action":"toggle-watch"}`))
	srv.handleAPIStreamerQuickAction(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want non-2xx on a failing callback", rec.Code)
	}
}

// TestQuickActionNilCallbackReturns503 mirrors the nil-callback guard for
// the quick-action endpoint.
func TestQuickActionNilCallbackReturns503(t *testing.T) {
	srv := quickActionTestServer(nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/streamer-action/shroud", strings.NewReader(`{"action":"toggle-watch"}`))
	srv.handleAPIStreamerQuickAction(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when onSettingsUpdate is nil", rec.Code)
	}
}

// TestQuickActionSuccessUnchanged pins the success path stays exactly as
// before: 200 with the success/preference/disableWatch body.
func TestQuickActionSuccessUnchanged(t *testing.T) {
	var applied settings.RuntimeSettings
	srv := quickActionTestServer(func(ctx context.Context, rt settings.RuntimeSettings) error {
		applied = rt
		return nil
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/streamer-action/shroud", strings.NewReader(`{"action":"toggle-watch"}`))
	srv.handleAPIStreamerQuickAction(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"success":true`) {
		t.Fatalf("success body missing success:true: %s", rec.Body.String())
	}
	if len(applied.Streamers) != 1 || applied.Streamers[0].Settings == nil || applied.Streamers[0].Settings.DisableWatch == nil || !*applied.Streamers[0].Settings.DisableWatch {
		t.Fatalf("callback did not receive the toggled setting: %+v", applied)
	}
}
