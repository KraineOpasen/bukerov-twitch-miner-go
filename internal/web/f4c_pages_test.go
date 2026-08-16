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
	if !strings.Contains(body, `class="lc-spinner w-3.5 h-3.5 border-2 border-border-default border-t-interactive rounded-full animate-spin"`) {
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

// f4cPanelWithDashboard renders the lifecycle_panel partial (through the
// real GET /api/lifecycle htmx handler) against a fixed running snapshot and
// an explicit dashboard config + RemoteAddr, in the given language — the
// Ф4d seam-3 rendering helper.
func f4cPanelWithDashboard(t *testing.T, dash runtimeconfig.Dashboard, remoteAddr, lang string) string {
	t.Helper()
	s := newRenderServer(t)
	s.SetLifecycleController(&fakeLifecycleController{snap: lifecycle.Snapshot{
		Desired: lifecycle.DesiredRunning, Observed: lifecycle.ObservedRunning,
		Capabilities: lifecycle.Capabilities{CanPause: true, CanRestart: true, CanStop: true},
	}})
	s.SetDashboardConfig(dash)
	req := htmxRequest(http.MethodGet, "/api/lifecycle")
	req.RemoteAddr = remoteAddr
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

// f4cLANWording pins the LITERAL, language-specific substrings the
// trusted-LAN panel states must render (f3_pages_test.go / this file's
// idiom: hardcoded expected text, never text computed from the SAME i18n
// catalog the handler under test also reads — that would only prove the
// catalog is self-consistent, not that the rendered HTML says what it
// should). Each fragment is a stable prefix/substring of the real
// internal/i18n/locales/{en,ru}.json value, chosen to survive minor wording
// edits elsewhere in the same string.
var f4cLANWording = map[string]struct {
	allowedPrefix        string // lc.lan.allowed
	deniedPrefix         string // lc.lan.denied
	insecureDisabledFrag string // the trusted-LAN clause inside the extended lc.insecure_disabled
}{
	"en": {
		allowedPrefix:        "Lifecycle commands are allowed from your address",
		deniedPrefix:         "Lifecycle controls are disabled for your address",
		insecureDisabledFrag: "or allow your home network with DASHBOARD_TRUSTED_LAN_CIDRS",
	},
	"ru": {
		allowedPrefix:        "Команды управления жизненным циклом разрешены с вашего адреса",
		deniedPrefix:         "Управление жизненным циклом отключено для вашего адреса",
		insecureDisabledFrag: "либо разрешите вашу домашнюю сеть через DASHBOARD_TRUSTED_LAN_CIDRS",
	},
}

// TestF4cPanelTrustedLANStates is the Ф4d seam-3 tri-state panel rendering
// matrix, in both languages: trusted -> lc.lan.allowed text + Pause NOT
// disabled; denied -> lc.lan.denied text + Pause disabled; not configured ->
// the extended lc.insecure_disabled text + Pause disabled; Basic Auth mode
// (InsecureNoAuth false) -> no LAN strings at all, Pause not disabled.
func TestF4cPanelTrustedLANStates(t *testing.T) {
	pauseEnabled := `hx-post="/api/lifecycle/pause" hx-target="#lifecycle-panel" hx-swap="innerHTML" hx-disabled-elt="this" hx-sync="#lifecycle-panel:replace" >`
	pauseDisabled := `hx-post="/api/lifecycle/pause" hx-target="#lifecycle-panel" hx-swap="innerHTML" hx-disabled-elt="this" hx-sync="#lifecycle-panel:replace" disabled>`

	for _, lang := range f4cLangs {
		want := f4cLANWording[lang]

		t.Run(lang+"/trusted", func(t *testing.T) {
			dash := runtimeconfig.Dashboard{InsecureNoAuth: true, TrustedLANCIDRs: mustLANCIDRs(t, "10.0.0.0/8")}
			body := f4cPanelWithDashboard(t, dash, "10.1.2.3:5555", lang)
			if !strings.Contains(body, want.allowedPrefix) {
				t.Errorf("[%s] trusted panel missing the allowed-address text %q; body=%s", lang, want.allowedPrefix, body)
			}
			if !strings.Contains(body, pauseEnabled) {
				t.Errorf("[%s] trusted panel's Pause button must NOT be disabled; body=%s", lang, body)
			}
		})

		t.Run(lang+"/denied", func(t *testing.T) {
			dash := runtimeconfig.Dashboard{InsecureNoAuth: true, TrustedLANCIDRs: mustLANCIDRs(t, "192.168.0.0/16")}
			body := f4cPanelWithDashboard(t, dash, "10.1.2.3:5555", lang)
			if !strings.Contains(body, want.deniedPrefix) {
				t.Errorf("[%s] denied panel missing the denied-address text %q; body=%s", lang, want.deniedPrefix, body)
			}
			if !strings.Contains(body, pauseDisabled) {
				t.Errorf("[%s] denied panel's Pause button must be disabled; body=%s", lang, body)
			}
		})

		t.Run(lang+"/not_configured", func(t *testing.T) {
			dash := insecureDashboardConfig() // InsecureNoAuth:true, no CIDRs
			body := f4cPanelWithDashboard(t, dash, "10.1.2.3:5555", lang)
			if !strings.Contains(body, "DASHBOARD_TRUSTED_LAN_CIDRS") {
				t.Errorf("[%s] not-configured panel's explanation must mention the trusted-LAN alternative; body=%s", lang, body)
			}
			if !strings.Contains(body, want.insecureDisabledFrag) {
				t.Errorf("[%s] not-configured panel missing the extended-explanation fragment %q; body=%s", lang, want.insecureDisabledFrag, body)
			}
			if !strings.Contains(body, pauseDisabled) {
				t.Errorf("[%s] not-configured panel's Pause button must be disabled; body=%s", lang, body)
			}
		})

		t.Run(lang+"/basic_auth_no_lan_messaging", func(t *testing.T) {
			dash := runtimeconfig.Dashboard{Username: "admin", Password: runtimeconfig.NewSecret("hunter2")}
			body := f4cPanelWithDashboard(t, dash, "10.1.2.3:5555", lang)
			for _, unwanted := range []string{want.allowedPrefix, want.deniedPrefix, "DASHBOARD_TRUSTED_LAN_CIDRS"} {
				if strings.Contains(body, unwanted) {
					t.Errorf("[%s] Basic Auth mode must carry no LAN/insecure messaging, found %q; body=%s", lang, unwanted, body)
				}
			}
			if !strings.Contains(body, pauseEnabled) {
				t.Errorf("[%s] Basic Auth mode's Pause button must NOT be disabled; body=%s", lang, body)
			}
		})
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
