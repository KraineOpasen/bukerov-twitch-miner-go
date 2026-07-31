package web

// Ф4c "Pages Evolution" regression tests: the lifecycle panel anchor and
// base.html's generation-gated overlay/banner rules, pinned as httptest
// literal assertions (f3_pages_test.go style, both languages) — the
// CI-enforced regression layer for what the browser evidence (D14) proves
// interactively.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
)

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return tm
}

func insecureDashboardConfig() runtimeconfig.Dashboard {
	return runtimeconfig.Dashboard{InsecureNoAuth: true}
}

var f4cLangs = []string{"en", "ru"}

// TestF4cOverviewContainsLifecyclePanelAnchor: the panel anchor sits on the
// Overview page (not the dead legacy dashboard.html) with the exact poll
// contract design v6 §9 mandates, plus the STATIC aria-live/staleness
// siblings outside the swap target.
func TestF4cOverviewContainsLifecyclePanelAnchor(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, lang := range f4cLangs {
		body := f3GetPage(t, srv, "/", lang)
		for _, want := range []string{
			`id="lifecycle-panel"`,
			`hx-get="/api/lifecycle"`,
			`hx-trigger="load, every 2s"`,
			`hx-swap="innerHTML"`,
			`id="lc-live"`,
			`aria-live="polite"`,
			`id="lc-conn-lost"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("[%s] overview page missing %q", lang, want)
			}
		}
	}
}

// TestF4cBaseHTMLContainsGenerationGateAndAuthBanner: base.html's status
// script computes the (status.generation || 1) <= 1 discriminator and
// carries the new non-blocking auth banner element, a sibling of
// #health-banner.
func TestF4cBaseHTMLContainsGenerationGateAndAuthBanner(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, lang := range f4cLangs {
		body := f3GetPage(t, srv, "/", lang)
		for _, want := range []string{
			`status.generation || 1`,
			`id="lifecycle-auth-banner"`,
			`LIFECYCLE_STATUSES`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("[%s] base.html missing %q", lang, want)
			}
		}
	}
}

// lifecyclePanelHTMX renders the lifecycle_panel partial (via the real
// GET /api/lifecycle handler, HX-Request path) against a fake controller
// snapshot, in the given language.
func lifecyclePanelHTMX(t *testing.T, snap lifecycle.Snapshot, lang string) string {
	t.Helper()
	s := newRenderServer(t)
	s.SetLifecycleController(&fakeLifecycleController{snap: snap})
	req := htmxRequest(http.MethodGet, "/api/lifecycle")
	if lang != "" {
		req.AddCookie(&http.Cookie{Name: langCookieName, Value: lang})
	}
	rec := httptest.NewRecorder()
	s.handleAPILifecycle(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// TestF4cPanelRunningStateButtonsAndAttributes pins the first-use htmx
// attribute patterns (hx-disabled-elt, hx-sync, hx-confirm) and the
// canX-derived disabled state, plus the Stop button living inside a
// collapsed "Advanced actions" <details>.
func TestF4cPanelRunningStateButtonsAndAttributes(t *testing.T) {
	snap := lifecycle.Snapshot{
		Desired:    lifecycle.DesiredRunning,
		Observed:   lifecycle.ObservedRunning,
		Transition: lifecycle.TransitionNone,
		Capabilities: lifecycle.Capabilities{
			CanPause: true, CanRestart: true, CanStop: true,
		},
	}
	for _, lang := range f4cLangs {
		body := lifecyclePanelHTMX(t, snap, lang)
		for _, want := range []string{
			`hx-disabled-elt="this"`,
			`hx-sync="#lifecycle-panel:replace"`,
			`hx-confirm=`,
			`<details class="details-panel mt-2" id="lc-advanced">`,
			`<summary`,
			`hx-post="/api/lifecycle/pause"`,
			`hx-post="/api/lifecycle/restart"`,
			`hx-post="/api/lifecycle/stop"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("[%s] running panel missing %q; body=%s", lang, want, body)
			}
		}
		// Resume/Retry/Restart-process must NOT be shown while running.
		if strings.Contains(body, `hx-post="/api/lifecycle/resume"`) {
			t.Errorf("[%s] running panel must not offer Resume", lang)
		}
		if strings.Contains(body, `hx-post="/api/lifecycle/restart-process"`) {
			t.Errorf("[%s] running panel must not offer Restart process", lang)
		}
		if strings.Contains(body, "animate-spin") {
			t.Errorf("[%s] a steady running state (Transition=none) must not show the transition spinner", lang)
		}
	}
}

// TestF4cPanelDisabledAttributeFollowsCapabilities: when a capability is
// false (e.g. mid-transition, slot occupied), the corresponding button
// carries the disabled attribute.
func TestF4cPanelDisabledAttributeFollowsCapabilities(t *testing.T) {
	snap := lifecycle.Snapshot{
		Desired:      lifecycle.DesiredRunning,
		Observed:     lifecycle.ObservedPausing,
		Transition:   lifecycle.TransitionPause,
		Capabilities: lifecycle.Capabilities{}, // all false: slot occupied
	}
	body := lifecyclePanelHTMX(t, snap, "en")
	if !strings.Contains(body, `hx-post="/api/lifecycle/pause" hx-target="#lifecycle-panel" hx-swap="innerHTML" hx-disabled-elt="this" hx-sync="#lifecycle-panel:replace" disabled>`) {
		t.Errorf("pause button must carry disabled while CanPause is false; body=%s", body)
	}
	if !strings.Contains(body, `class="lc-spinner w-3.5 h-3.5 border-2 border-neutral-700 border-t-purple-500 rounded-full animate-spin"`) {
		t.Error("transitioning panel must show the spinner")
	}
}

// TestF4cPanelFailedShowsRetryAndNextRetry: a failed observed state shows
// the Retry button (POSTs resume) and the formatted NextRetryAt line.
func TestF4cPanelFailedShowsRetryAndNextRetry(t *testing.T) {
	snap := lifecycle.Snapshot{
		Desired:      lifecycle.DesiredRunning,
		Observed:     lifecycle.ObservedFailed,
		LastError:    "connection refused",
		NextRetryAt:  mustParseTime(t, "2026-07-31T12:00:00Z"),
		Capabilities: lifecycle.Capabilities{CanResume: true, CanPause: true, CanStop: true},
	}
	body := lifecyclePanelHTMX(t, snap, "en")
	if !strings.Contains(body, `hx-post="/api/lifecycle/resume"`) {
		t.Errorf("failed panel must offer Retry (POSTs resume); body=%s", body)
	}
	if !strings.Contains(body, "connection refused") {
		t.Error("failed panel must show LastError")
	}
	if !strings.Contains(body, "12:00:00") {
		t.Errorf("failed panel must show formatted NextRetryAt; body=%s", body)
	}
}

// TestF4cPanelDegradedShowsRestartProcessAndHint: a degraded observed state
// hides Pause/Resume/Restart and shows the Restart-process action + hint.
func TestF4cPanelDegradedShowsRestartProcessAndHint(t *testing.T) {
	snap := lifecycle.Snapshot{
		Desired:  lifecycle.DesiredPaused,
		Observed: lifecycle.ObservedDegraded,
	}
	s := newRenderServer(t)
	s.SetLifecycleController(&fakeLifecycleController{snap: snap})
	s.SetProcessRestartRequester(func() {})
	rec := httptest.NewRecorder()
	s.handleAPILifecycle(rec, htmxRequest("GET", "/api/lifecycle"))
	body := rec.Body.String()

	if !strings.Contains(body, `hx-post="/api/lifecycle/restart-process"`) {
		t.Errorf("degraded panel must offer Restart process; body=%s", body)
	}
	if strings.Contains(body, `hx-post="/api/lifecycle/pause"`) || strings.Contains(body, `hx-post="/api/lifecycle/restart"`) {
		t.Errorf("degraded panel must not offer Pause/Restart; body=%s", body)
	}
}

// TestF4cPanelInsecureDisabledShowsExplanation: InsecureNoAuth renders every
// control disabled with the explanation text, even though a GET is never
// gated (I21) so the panel still renders at all.
func TestF4cPanelInsecureDisabledShowsExplanation(t *testing.T) {
	s := newRenderServer(t)
	s.SetLifecycleController(&fakeLifecycleController{snap: lifecycle.Snapshot{
		Desired: lifecycle.DesiredRunning, Observed: lifecycle.ObservedRunning,
		Capabilities: lifecycle.Capabilities{CanPause: true, CanRestart: true, CanStop: true},
	}})
	s.SetDashboardConfig(insecureDashboardConfig())
	rec := httptest.NewRecorder()
	s.handleAPILifecycle(rec, htmxRequest("GET", "/api/lifecycle"))
	body := rec.Body.String()
	if !strings.Contains(body, "DASHBOARD_INSECURE_NO_AUTH") {
		t.Errorf("insecure panel must explain DASHBOARD_INSECURE_NO_AUTH; body=%s", body)
	}
	if !strings.Contains(body, `hx-post="/api/lifecycle/pause" hx-target="#lifecycle-panel" hx-swap="innerHTML" hx-disabled-elt="this" hx-sync="#lifecycle-panel:replace" disabled>`) {
		t.Errorf("insecure panel must render the pause button disabled; body=%s", body)
	}
}

// TestF4cPanelUnavailableRendersNothing: no controller wired at all renders
// an empty panel (hidden, per D1's "unset ⇒ hidden/disabled in panel").
func TestF4cPanelUnavailableRendersNothing(t *testing.T) {
	s := newRenderServer(t)
	rec := httptest.NewRecorder()
	s.handleAPILifecycle(rec, htmxRequest("GET", "/api/lifecycle"))
	if rec.Code != 200 {
		t.Fatalf("htmx GET with no controller: status = %d, want 200", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "" {
		t.Errorf("panel with no controller should render empty, got: %q", rec.Body.String())
	}
}
