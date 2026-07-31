package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

// settingsLifecycleTestServer builds a fully-wired settings server (real
// i18n via newRenderServer) plus a recording settings callback, so the 409
// guard tests can assert BOTH the response and that the callback was never
// invoked.
func settingsLifecycleTestServer(t *testing.T, observed lifecycle.ObservedState, transition lifecycle.Transition) (*Server, *fakeLifecycleController, *[]settings.RuntimeSettings) {
	t.Helper()
	s := newRenderServer(t)
	applied := &[]settings.RuntimeSettings{}
	s.SetSettingsProvider(&fakeSettingsProvider{rt: settings.RuntimeSettings{
		Streamers: []settings.StreamerConfig{{Username: "alpha"}},
	}})
	s.SetSettingsUpdateCallback(func(_ context.Context, rt settings.RuntimeSettings) error {
		*applied = append(*applied, rt)
		return nil
	})
	ctrl := &fakeLifecycleController{snap: lifecycle.Snapshot{Observed: observed, Transition: transition}}
	s.SetLifecycleController(ctrl)
	return s, ctrl, applied
}

func TestSettingsPostBlockedWhilePaused(t *testing.T) {
	s, _, applied := settingsLifecycleTestServer(t, lifecycle.ObservedPaused, lifecycle.TransitionNone)

	rec := postSettings(t, s, `{"streamers":[{"username":"alpha"}]}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(*applied) != 0 {
		t.Errorf("settings callback must not be invoked while paused, got %d calls", len(*applied))
	}
	if rec.Body.Len() == 0 {
		t.Error("409 body must carry a localized explanation")
	}
}

func TestSettingsPostBlockedWhileStopped(t *testing.T) {
	s, _, applied := settingsLifecycleTestServer(t, lifecycle.ObservedStopped, lifecycle.TransitionNone)
	rec := postSettings(t, s, `{}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if len(*applied) != 0 {
		t.Error("callback must not be invoked while stopped")
	}
}

func TestSettingsPostBlockedDuringTransition(t *testing.T) {
	s, _, applied := settingsLifecycleTestServer(t, lifecycle.ObservedPausing, lifecycle.TransitionPause)
	rec := postSettings(t, s, `{}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if len(*applied) != 0 {
		t.Error("callback must not be invoked mid-transition")
	}
}

func TestSettingsPostNotBlockedWhileRunning(t *testing.T) {
	s, _, applied := settingsLifecycleTestServer(t, lifecycle.ObservedRunning, lifecycle.TransitionNone)
	rec := postSettings(t, s, `{"streamers":[{"username":"alpha"}]}`)
	if rec.Code == http.StatusConflict {
		t.Fatalf("running must not be blocked, status = %d", rec.Code)
	}
	if len(*applied) != 1 {
		t.Errorf("callback should have been invoked once, got %d", len(*applied))
	}
}

func TestSettingsPostNotBlockedWhileDegraded(t *testing.T) {
	s, _, applied := settingsLifecycleTestServer(t, lifecycle.ObservedDegraded, lifecycle.TransitionNone)
	rec := postSettings(t, s, `{"streamers":[{"username":"alpha"}]}`)
	if rec.Code == http.StatusConflict {
		t.Fatalf("degraded must not be blocked by this guard, status = %d", rec.Code)
	}
	if len(*applied) != 1 {
		t.Errorf("callback should have been invoked once, got %d", len(*applied))
	}
}

func TestSettingsGetNotBlockedWhilePaused(t *testing.T) {
	s, _, _ := settingsLifecycleTestServer(t, lifecycle.ObservedPaused, lifecycle.TransitionNone)
	rec := httptest.NewRecorder()
	s.handleAPISettings(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET must remain unaffected by the paused guard, status = %d", rec.Code)
	}
}

func TestSettingsResetBlockedWhilePaused(t *testing.T) {
	s, _, applied := settingsLifecycleTestServer(t, lifecycle.ObservedPaused, lifecycle.TransitionNone)
	rec := httptest.NewRecorder()
	s.handleAPISettingsReset(rec, httptest.NewRequest(http.MethodPost, "/api/settings/reset", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if len(*applied) != 0 {
		t.Error("reset callback must not be invoked while paused")
	}
}

func TestQuickActionBlockedWhilePaused(t *testing.T) {
	s, _, applied := settingsLifecycleTestServer(t, lifecycle.ObservedPaused, lifecycle.TransitionNone)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/streamer-action/alpha", strings.NewReader(`{"action":"toggle-watch"}`))
	s.handleAPIStreamerQuickAction(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if len(*applied) != 0 {
		t.Error("quickaction callback must not be invoked while paused")
	}
}

// TestSettingsLifecycleGuardNilControllerUnaffected locks the nil-safety
// invariant that the dozens of existing bare-&Server{} settings tests depend
// on: no lifecycle controller wired at all means no guard, exactly today's
// behavior.
func TestSettingsLifecycleGuardNilControllerUnaffected(t *testing.T) {
	applied := 0
	srv := &Server{
		settingsProvider: &fakeSettingsProvider{rt: settings.RuntimeSettings{}},
		onSettingsUpdate: func(context.Context, settings.RuntimeSettings) error {
			applied++
			return nil
		},
	}
	rec := postSettings(t, srv, `{}`)
	if rec.Code == http.StatusConflict {
		t.Fatalf("no lifecycle controller wired must never produce a 409, status = %d", rec.Code)
	}
	if applied != 1 {
		t.Errorf("callback should have run once, got %d", applied)
	}
}

// TestSettingsPostBlockedFullChain exercises the guard through s.handler()
// (the real middleware chain), confirming a 409 still passes CSRF and lands
// as the paused-conflict body, not a generic settings-not-available message.
func TestSettingsPostBlockedFullChain(t *testing.T) {
	s, _, applied := settingsLifecycleTestServer(t, lifecycle.ObservedPaused, lifecycle.TransitionNone)
	handler := s.handler()

	req := httptest.NewRequest(http.MethodPost, "http://10.0.0.5:5000/api/settings", strings.NewReader(`{}`))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(*applied) != 0 {
		t.Error("callback must not be invoked")
	}
}
