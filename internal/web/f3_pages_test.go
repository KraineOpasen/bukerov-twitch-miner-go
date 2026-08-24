package web

// F3 "Pages Evolution" regression tests: the six evolved pages (drops,
// statistics, health, logs, settings, notifications) render correctly in
// both languages, keep the polling and API contracts expected by each task,
// and carry the accessibility/behavior additions the design mandated. Fixtures
// are the shared f3_harness_test.go fakes so this file
// stays a thin assertion layer over the same deterministic data the browser
// harness serves.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

const f3PageUsername = "f3pagetester"

// buildF3PageServer wires a Server against the shared f3 fakes (defined in
// f3_harness_test.go) plus a Notifications manager whose Discord config is
// deliberately invalid (Enabled with no bot token), so both the healthy and
// the config-invalid banner paths are exercisable from one fixture set.
func buildF3PageServer(t *testing.T) *Server {
	t.Helper()

	workDir := t.TempDir()
	t.Chdir(workDir)
	f3WriteLogFixture(t, workDir, f3PageUsername, 40)

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("analytics service: %v", err)
	}

	streamers := []*models.Streamer{
		models.NewStreamer("streamer_a", models.StreamerSettings{}),
		models.NewStreamer("streamer_b", models.StreamerSettings{}),
	}

	cfg := config.DefaultConfig()
	cfg.Streamers = []config.StreamerConfig{{Username: "streamer_a"}, {Username: "streamer_b"}}
	rt := settings.BuildRuntimeSettings(&cfg)

	srv := NewServer(config.AnalyticsSettings{Host: "127.0.0.1", Port: 0, Refresh: 5, DaysAgo: 30}, f3PageUsername, workDir, svc, streamers)
	srv.SetCampaignsProvider(&f3Campaigns{campaigns: f3BuildCampaigns()})
	srv.SetDropCatalogProvider(&f3Catalog{past: f3BuildPast()})
	srv.SetDiscoveryProvider(f3Discovery{})
	srv.SetHealthProvider(&f3Health{settings: config.HealthSettings{
		CanaryEnabled: true, CanaryChannel: "canary_chan", CanaryIntervalMinutes: 120, CanaryMaxStalenessHours: 12,
		WatchdogEnabled: true, WatchdogStallDelayMinutes: 30, WatchdogStallConfirmations: 3,
		WatchdogRecoveryCooldownMinutes: 10, WatchdogAvoidTTLMinutes: 60, WatchdogRearmHours: 6,
	}})
	srv.SetDropProgressProvider(f3Progress{})
	srv.SetPolicyProvider(&f3Policy{
		mode:  "smart",
		rules: map[string]config.DropRule{},
		decisions: []policy.Decision{
			{CampaignID: "c1", Name: "Anniversary Drops", Total: 42, Status: policy.StatusSafe,
				Feasibility: policy.Feasibility{TimeUntilEnd: 30 * time.Hour, MinutesToNextReward: 60, CanCompleteNextReward: true, CanCompleteAll: true, Status: policy.StatusSafe},
				Factors:     []policy.Factor{{Label: "ending soon", Points: 20}, {Label: "close to reward", Points: 22}}},
		},
	})
	srv.SetSettingsProvider(&f3Settings{rt: rt})
	srv.SetSettingsUpdateCallback(func(_ context.Context, _ settings.RuntimeSettings) error { return nil })
	srv.SetFollowedProvider(f3Followed{})

	// Discord enabled but unconfigured (no bot token) => IsConfigValid()
	// returns false, exercising the config-invalid banner on /notifications.
	mgr, err := notifications.NewManager(&config.DiscordSettings{Enabled: true}, nil, db, []string{"streamer_a", "streamer_b"}, f3PageUsername)
	if err != nil {
		t.Fatalf("notifications.NewManager: %v", err)
	}
	srv.SetNotificationManager(mgr)
	srv.SetDiscordEnabled(true)

	return srv
}

// f3GetPage issues a GET against srv's full handler chain (routing,
// language cookie, base-layout render included) and fails the test on a
// non-200 response.
func f3GetPage(t *testing.T, srv *Server, path, lang string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if lang != "" {
		req.AddCookie(&http.Cookie{Name: langCookieName, Value: lang})
	}
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s (lang=%s) = %d, want 200; body=%s", path, lang, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

var f3Pages = []string{"/drops", "/statistics", "/health", "/logs", "/settings", "/notifications"}
var f3Langs = []string{"en", "ru"}

// TestF3SixPagesRenderBothLanguages is the baseline smoke test every other
// F3 assertion builds on: all six evolved pages render 200 OK in both
// languages through the real base.html layout.
func TestF3SixPagesRenderBothLanguages(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, page := range f3Pages {
		for _, lang := range f3Langs {
			body := f3GetPage(t, srv, page, lang)
			if !strings.Contains(body, "app-sidebar") {
				t.Errorf("%s (lang=%s) did not render through base.html (missing app-sidebar marker)", page, lang)
			}
		}
	}
}

// TestF3PollIntervalLiteralsUnchanged pins every htmx polling interval this
// task was forbidden from touching.
func TestF3PollIntervalLiteralsUnchanged(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, lang := range f3Langs {
		logs := f3GetPage(t, srv, "/logs", lang)
		if !strings.Contains(logs, "every 10s") {
			t.Errorf("logs page (lang=%s) missing the unchanged \"every 10s\" poll literal", lang)
		}

		health := f3GetPage(t, srv, "/health", lang)
		if !strings.Contains(health, "load, every 15s") {
			t.Errorf("health page (lang=%s) missing the unchanged \"load, every 15s\" poll literal", lang)
		}

		drops := f3GetPage(t, srv, "/drops", lang)
		if !strings.Contains(drops, "load, every 30s") {
			t.Errorf("drops page (lang=%s) missing the unchanged \"load, every 30s\" poll literal", lang)
		}
		if n := strings.Count(drops, "every 1m"); n != 1 {
			t.Errorf("drops page (lang=%s) has %d occurrences of \"every 1m\", want exactly 1 (discovery)", lang, n)
		}
	}
}

// TestF3PreservedEndpointLiterals pins the API endpoints outside the removed
// product surface, wherever they appear (a bare page load, or a partial only
// reachable via its own htmx poll).
func TestF3PreservedEndpointLiterals(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, lang := range f3Langs {
		drops := f3GetPage(t, srv, "/drops", lang)
		for _, want := range []string{
			"/api/drops", "/api/drops/sync", "/api/drops/past",
			"/api/discovery", "/api/policy/mode",
		} {
			if !strings.Contains(drops, want) {
				t.Errorf("drops page (lang=%s) missing endpoint literal %q", lang, want)
			}
		}

		apiDrops := f3GetPage(t, srv, "/api/drops", lang)
		if !strings.Contains(apiDrops, "/api/policy/drop-rule") {
			t.Errorf("GET /api/drops (lang=%s) missing endpoint literal \"/api/policy/drop-rule\"", lang)
		}

		health := f3GetPage(t, srv, "/health", lang)
		if !strings.Contains(health, "/api/health") {
			t.Errorf("health page (lang=%s) missing endpoint literal \"/api/health\"", lang)
		}
		apiHealth := f3GetPage(t, srv, "/api/health", lang)
		for _, want := range []string{"/api/health/settings", "/api/health/canary/run"} {
			if !strings.Contains(apiHealth, want) {
				t.Errorf("GET /api/health (lang=%s) missing endpoint literal %q", lang, want)
			}
		}

		logs := f3GetPage(t, srv, "/logs", lang)
		if !strings.Contains(logs, "/api/logs") {
			t.Errorf("logs page (lang=%s) missing endpoint literal \"/api/logs\"", lang)
		}

		stats := f3GetPage(t, srv, "/statistics", lang)
		for _, want := range []string{"/api/points-history", "/api/predictions/roi"} {
			if !strings.Contains(stats, want) {
				t.Errorf("statistics page (lang=%s) missing endpoint literal %q", lang, want)
			}
		}

		set := f3GetPage(t, srv, "/settings", lang)
		for _, want := range []string{"/api/settings", "/api/settings/reset"} {
			if !strings.Contains(set, want) {
				t.Errorf("settings page (lang=%s) missing endpoint literal %q", lang, want)
			}
		}

		notif := f3GetPage(t, srv, "/notifications", lang)
		for _, want := range []string{
			"/api/notifications/config", "/api/notifications/channels",
			"/api/notifications/points", "/api/notifications/test",
		} {
			if !strings.Contains(notif, want) {
				t.Errorf("notifications page (lang=%s) missing endpoint literal %q", lang, want)
			}
		}
	}
}

// TestF3NoCDNOrExternalFontReferences scans every bare page render for a
// hardcoded CDN/font-host reference. Twitch box-art URLs
// (static-cdn.jtvnw.net) only ever arrive via a partial's viewmodel data
// (GET /api/drops et al.), never via a hardcoded template string, so this is
// scoped to the bare page shells and never fetches those partials.
func TestF3NoCDNOrExternalFontReferences(t *testing.T) {
	srv := buildF3PageServer(t)
	banned := []string{"cdn.", "fonts.googleapis", "unpkg", "jsdelivr"}
	for _, page := range f3Pages {
		for _, lang := range f3Langs {
			body := f3GetPage(t, srv, page, lang)
			for _, b := range banned {
				if strings.Contains(body, b) {
					t.Errorf("%s (lang=%s) contains banned external reference %q", page, lang, b)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------
// Logs
// ---------------------------------------------------------------------

// TestF3LogsToolbarAndController asserts the new filter toolbar controls and
// the __logsCtl guard are present, and that the old inline <style> block is
// gone (moved to input.css — see TestLogsPageHasNoInlineLogStyle for the
// template-level version of this same guarantee).
func TestF3LogsToolbarAndController(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/logs", "en")

	for _, want := range []string{
		`id="logs-filter-level"`, `id="logs-filter-subsystem"`, `id="logs-filter-search"`,
		`id="logs-filter-reconnect"`, `id="logs-copy-btn"`, `id="logs-copy-status"`,
		`id="logs-count"`, `id="logs-no-match"`, `id="logs-new-indicator"`,
		"window.__logsCtl", "if (window.__logsCtl) return;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("logs page missing toolbar/controller literal %q", want)
		}
	}
	if strings.Contains(body, "<style>") {
		t.Error("logs page must not render an inline <style> block anymore")
	}
}

// TestF3LogsCountIsVisualOnlyNoMatchIsAnnounced pins FIX-2: #logs-count
// rewrites on every keystroke and every 10s poll, so it must NOT be a live
// region; only the #logs-no-match transition is announced (role=status,
// change-gated in JS — see setNoMatchAnnouncement).
func TestF3LogsCountIsVisualOnlyNoMatchIsAnnounced(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/logs", "en")

	if strings.Contains(body, `id="logs-count" class="logs-count" aria-live`) {
		t.Error("#logs-count must not be a live region (rewrites on every keystroke/poll)")
	}
	if !strings.Contains(body, `<span id="logs-count" class="logs-count"></span>`) {
		t.Error("#logs-count must render as a plain, non-live span")
	}
	if !strings.Contains(body, `id="logs-no-match" class="visually-hidden" role="status"`) {
		t.Error("#logs-no-match must be the announced (role=status) no-match transition region")
	}
	if !strings.Contains(body, "setNoMatchAnnouncement") {
		t.Error("logs page missing the change-gated setNoMatchAnnouncement helper")
	}
}

// TestF3LogsIndicatorIsContentSignatureGated pins FIX-3: showIndicator() on an
// unpinned afterSwap must only fire when the tail content actually changed.
func TestF3LogsIndicatorIsContentSignatureGated(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/logs", "en")

	for _, want := range []string{
		"function computeSignature", "lastSignature", "const changed = sig !== lastSignature;",
		"} else if (changed) {",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("logs page missing content-signature literal %q", want)
		}
	}
}

// TestF3LogsCopyStatusClearedAndAutoCleared pins FIX-4: the copy-status
// region is cleared before every attempt (so an identical repeated result
// still re-announces) and auto-clears after ~4s via one owned, re-armed timer.
func TestF3LogsCopyStatusClearedAndAutoCleared(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/logs", "en")

	for _, want := range []string{
		"let copyStatusTimer = null;",
		"if (copyStatusTimer) clearTimeout(copyStatusTimer);",
		"}, 4000);",
		"if (copyStatus) copyStatus.textContent = '';",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("logs page missing copy-status clear/timer literal %q", want)
		}
	}
}

// TestF3LogsScrollRegionIsKeyboardAccessible pins FIX-8: #logs-scroll must be
// keyboard-focusable (tabindex=0) and labeled as a region — but role="log"
// specifically must NOT be used, since its implicit aria-live would
// re-introduce the exact 10s-poll announcement spam FIX-2 removed.
func TestF3LogsScrollRegionIsKeyboardAccessible(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/logs", "en")

	if !strings.Contains(body, `id="logs-scroll" class="log-scroll log-view" tabindex="0" role="region" aria-label="Log output"`) {
		t.Error(`#logs-scroll must carry tabindex="0" role="region" aria-label="Log output"`)
	}
	if strings.Contains(body, `role="log"`) {
		t.Error(`#logs-scroll must not use role="log" (implicit aria-live would spam every 10s poll)`)
	}
}

// TestF3LogsCardIsRelativeClassNotInlineStyle pins FIX-9d: the log card's
// positioning context comes from a Tailwind class, not an inline style.
func TestF3LogsCardIsRelativeClassNotInlineStyle(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/logs", "en")

	if strings.Contains(body, `style="position: relative;"`) {
		t.Error("logs page must not use an inline position:relative style anymore")
	}
	if !strings.Contains(body, `class="card relative"`) {
		t.Error(`logs page's card wrapper must use class="card relative"`)
	}
}

// TestF3LogsSubsystemMappingCoversEveryClass parses the rendered logs.html
// script for the SUBSYS class->subsystem lookup table and asserts every
// class allLogLineClasses() can emit appears there exactly once — so no log
// line can silently escape every subsystem filter bucket.
func TestF3LogsSubsystemMappingCoversEveryClass(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/logs", "en")

	start := strings.Index(body, "const SUBSYS = {")
	if start < 0 {
		t.Fatal("rendered logs.html has no \"const SUBSYS = {\" mapping literal")
	}
	end := strings.Index(body[start:], "};")
	if end < 0 {
		t.Fatal("rendered logs.html's SUBSYS mapping literal is not terminated with \"};\"")
	}
	table := body[start : start+end]

	for _, class := range allLogLineClasses() {
		needle := "'" + class + "':"
		if n := strings.Count(table, needle); n != 1 {
			t.Errorf("SUBSYS mapping: class %q appears %d times in the rendered table, want exactly 1", class, n)
		}
	}
}

// TestF3LogsLevelMappingLiteral pins the class->level branches (error/warning,
// everything else info) that back the level filter.
func TestF3LogsLevelMappingLiteral(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/logs", "en")
	for _, want := range []string{
		"if (cls === 'log-error') return 'error';",
		"if (cls === 'log-warn') return 'warning';",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("logs page missing level-mapping literal %q", want)
		}
	}
}

// ---------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------

// TestF3HealthCardsRenderStageDetailErrorCodeAndSeverity exercises the real
// GET /api/health partial (f3Health's fixture Signals cover OK/degraded/
// failed/idle/unknown) and asserts the new Stage/Detail/ErrorCode rows
// render, and that severity classes distinguish ok from everything else —
// idle and unknown must never be marked "ok".
func TestF3HealthCardsRenderStageDetailErrorCodeAndSeverity(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/api/health", "en")

	for _, want := range []string{
		"validate", "token valid", // OAuth: OK, Stage+Detail
		"query", "2 retries in last window", // GQLAPI: degraded
		"connect", "websocket dial timed out", "WS_TIMEOUT", // PubSub: failed, ErrorCode
		"beacon", "minute-watched accepted", // WatchTransport: OK
		"no active drops", // DropsInventory: idle
	} {
		if !strings.Contains(body, want) {
			t.Errorf("health partial missing expected fixture text %q", want)
		}
	}

	// Exactly the two StatusOK signals (OAuth, WatchTransport) are marked ok;
	// StatusDegraded/StatusFailed/StatusIdle/StatusUnknown must not be.
	if n := strings.Count(body, "health-sev-ok"); n != 2 {
		t.Errorf("health-sev-ok count = %d, want 2 (OAuth + WatchTransport only)", n)
	}
	if !strings.Contains(body, "health-sev-bad") {
		t.Error("health partial missing health-sev-bad (PubSub is StatusFailed)")
	}
	if !strings.Contains(body, "health-sev-warn") {
		t.Error("health partial missing health-sev-warn (GQLAPI is StatusDegraded)")
	}
	// Idle (DropsInventory) and Unknown (DropsProgress) both fall into the
	// neutral bucket — neither is "ok".
	if n := strings.Count(body, "health-sev-neutral"); n < 2 {
		t.Errorf("health-sev-neutral count = %d, want >= 2 (idle + unknown)", n)
	}
}

// TestF3HealthGuardScriptPresent asserts the active-form guard IIFE ships on
// the health page with its re-init guard and its scoping to #health-center.
func TestF3HealthGuardScriptPresent(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/health", "en")
	for _, want := range []string{
		"window.__healthGuard", "if (window.__healthGuard) return;",
		"htmx:beforeRequest", "elt !== center",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("health page missing guard-script literal %q", want)
		}
	}
}

// TestF3HealthGuardActiveElementRegexExcludesButton pins the FIX-1
// regression: a BUTTON must never count as "actively editing" for the
// beforeRequest guard — it keeps focus after a click (e.g. "Run now"), so
// including it would defer the 15s poll indefinitely with no visible cue.
func TestF3HealthGuardActiveElementRegexExcludesButton(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/health", "en")

	want := "/^(INPUT|SELECT|TEXTAREA)$/.test(active.tagName)"
	if !strings.Contains(body, want) {
		t.Errorf("health page missing the BUTTON-excluding active-element regex %q", want)
	}
	if strings.Contains(body, "/^(INPUT|SELECT|TEXTAREA|BUTTON)$/") {
		t.Error("health page's active-element regex must not include BUTTON")
	}
}

// ---------------------------------------------------------------------
// Drops
// ---------------------------------------------------------------------

// TestF3DropsTabsHaveRovingTabindexAndAria asserts the tablist/tabpanel ARIA
// wiring and roving tabindex the design mandated.
func TestF3DropsTabsHaveRovingTabindexAndAria(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/drops", "en")

	if !strings.Contains(body, `role="tablist"`) {
		t.Error("drops page missing role=\"tablist\"")
	}
	if n := strings.Count(body, `role="tab"`); n != 2 {
		t.Errorf(`role="tab"`+" count = %d, want 2", n)
	}
	if n := strings.Count(body, `role="tabpanel"`); n != 2 {
		t.Errorf(`role="tabpanel"`+" count = %d, want 2", n)
	}
	for _, want := range []string{
		`aria-controls="tab-current"`, `aria-controls="tab-past"`,
		`aria-labelledby="drops-tab-current"`, `aria-labelledby="drops-tab-past"`,
		`tabindex="0"`, `tabindex="-1"`,
		"window.__dropsTabs", "ArrowRight", "ArrowLeft",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("drops page missing tab a11y literal %q", want)
		}
	}
	for _, removed := range []string{
		`data-tab="upcoming"`, `aria-controls="tab-upcoming"`,
		`aria-labelledby="drops-tab-upcoming"`, `/api/drops/upcoming`,
	} {
		if strings.Contains(body, removed) {
			t.Errorf("drops page retained removed Upcoming surface %q", removed)
		}
	}

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/drops/upcoming", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/drops/upcoming = %d, want 404 after product-surface removal", rec.Code)
	}
}

func TestF3NotificationsOmitsRetiredDropsSetting(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, lang := range f3Langs {
		body := f3GetPage(t, srv, "/notifications", lang)
		for _, removed := range []string{"notif-drops", "upcoming-drops-enabled", "upcomingDropsEnabled", "notif.drops."} {
			if strings.Contains(body, removed) {
				t.Errorf("notifications page (lang=%s) retained removed Drops setting %q", lang, removed)
			}
		}
	}
}

// TestF3DropsPastClaimedVsExpiredTextDiffers exercises GET /api/drops/past
// (f3Catalog's fixture has one claimed and two unclaimed instances) and
// asserts the claimed and not-claimed labels are textually distinct in both
// languages, not just color-coded.
func TestF3DropsPastClaimedVsExpiredTextDiffers(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, tc := range []struct {
		lang, claimed, notClaimed string
	}{
		{"en", "Claimed", "Not claimed"},
		{"ru", "Получено", "Не получено"},
	} {
		body := f3GetPage(t, srv, "/api/drops/past", tc.lang)
		if !strings.Contains(body, tc.claimed) {
			t.Errorf("drops/past (lang=%s) missing claimed label %q", tc.lang, tc.claimed)
		}
		if !strings.Contains(body, tc.notClaimed) {
			t.Errorf("drops/past (lang=%s) missing not-claimed label %q", tc.lang, tc.notClaimed)
		}
		if tc.claimed == tc.notClaimed {
			t.Fatalf("test fixture bug: claimed/notClaimed labels must differ")
		}
	}
}

// TestF3DropsPolicyEndsInSurfaced asserts the new "ends in …" line renders
// from the existing Policy.TimeUntilEnd field when present and non-zero.
func TestF3DropsPolicyEndsInSurfaced(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/api/drops", "en")
	if !strings.Contains(body, "30h0m0s") {
		t.Error("drops queue missing the policy fixture's TimeUntilEnd value (30h0m0s)")
	}
}

// TestF3DropsEndsInHumanizedClientSide pins FIX-6: the server keeps rendering
// the honest raw Go-duration fallback (data-ends-in + visible text), and
// __dropsTabs humanizes it client-side via the new js.drops.ends_in_hm /
// js.drops.ends_in_m keys, which must exist (and be non-empty) in both
// locales.
func TestF3DropsEndsInHumanizedClientSide(t *testing.T) {
	srv := buildF3PageServer(t)
	apiDrops := f3GetPage(t, srv, "/api/drops", "en")
	if !strings.Contains(apiDrops, `class="text-neutral-500 js-ends-in" data-ends-in="30h0m0s"`) {
		t.Error("drops queue missing the raw data-ends-in fallback attribute/value")
	}

	dropsPage := f3GetPage(t, srv, "/drops", "en")
	for _, want := range []string{
		"function parseEndsIn", "function humanizeEndsIn", "function humanizeAllEndsIn",
		"js.drops.ends_in_hm", "js.drops.ends_in_m",
		"if (e.target && e.target.id === 'drops-queue') humanizeAllEndsIn();",
	} {
		if !strings.Contains(dropsPage, want) {
			t.Errorf("drops page missing ends-in humanization literal %q", want)
		}
	}

	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	for _, lang := range []string{i18n.LangEN, i18n.LangRU} {
		for _, key := range []string{"js.drops.ends_in_hm", "js.drops.ends_in_m"} {
			got := loc.T(lang, key)
			if got == key {
				t.Errorf("locale %q is missing key %q", lang, key)
			}
			if !strings.Contains(got, "{h}") && !strings.Contains(got, "{m}") {
				t.Errorf("locale %q key %q = %q, want a placeholder-bearing string", lang, key, got)
			}
		}
	}
}

// ---------------------------------------------------------------------
// Statistics
// ---------------------------------------------------------------------

// TestF3StatisticsAccessibilityTweaks asserts the aria-live removal on the
// two "updated" timestamps and the scope=col addition survive template
// rendering (the scope=col itself is added client-side by renderTable, so
// this only pins the JS source literal — see also handlers_statistics_test.go
// for the endpoint/format coverage).
func TestF3StatisticsAccessibilityTweaks(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/statistics", "en")

	if strings.Contains(body, `id="stat-updated" class="text-xs text-neutral-400 num" aria-live`) {
		t.Error("#stat-updated must not carry aria-live anymore")
	}
	if strings.Contains(body, `id="roi-updated" class="text-xs text-neutral-400 num" aria-live`) {
		t.Error("#roi-updated must not carry aria-live anymore")
	}
	if !strings.Contains(body, `id="stat-updated"`) || !strings.Contains(body, `id="roi-updated"`) {
		t.Error("statistics page lost the #stat-updated/#roi-updated regions")
	}
	if !strings.Contains(body, `<th scope="col">`) {
		t.Error("statistics page's renderTable must add scope=\"col\" to <th> elements")
	}
	if strings.Contains(body, "<style>") {
		t.Error("statistics page must not carry its old inline <style> block anymore (moved to input.css)")
	}
}

// ---------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------

// TestF3SettingsPredictionRiskSection asserts the new PredictionRisk details
// section renders with its three control ids, and that gatherFormData wires
// them into the save payload.
func TestF3SettingsPredictionRiskSection(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/settings", "en")

	for _, want := range []string{
		`id="predictionRiskMaxStakePercent"`, `id="predictionRiskReservePoints"`, `id="predictionRiskHealthGate"`,
		"predictionRisk:",
		// FIX-9c: an emptied numeric field must post an explicit 0, not NaN.
		"parseInt(document.getElementById('predictionRiskMaxStakePercent').value) || 0",
		"parseInt(document.getElementById('predictionRiskReservePoints').value) || 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing PredictionRisk literal %q", want)
		}
	}
}

// TestF3SettingsToastContainerIsAnnounced asserts the toast region is an
// announced live status region.
func TestF3SettingsToastContainerIsAnnounced(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/settings", "en")
	if !strings.Contains(body, `id="toast-container" role="status" aria-live="polite"`) {
		t.Error("settings page's #toast-container must be role=status aria-live=polite")
	}
}

// ---------------------------------------------------------------------
// Notifications
// ---------------------------------------------------------------------

// TestF3NotificationsConfigInvalidBannerIsAlert exercises the real
// config-invalid path (Discord enabled, no bot token configured) and asserts
// the banner carries role="alert", plus the hero subtitle and reload-button
// aria-labels are present.
func TestF3NotificationsConfigInvalidBannerIsAlert(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/notifications", "en")

	if !strings.Contains(body, `role="alert"`) {
		t.Error("notifications page's config-invalid banner must carry role=\"alert\"")
	}
	if !strings.Contains(body, "bot token is not configured") {
		t.Error("notifications page did not surface the real config error text")
	}
	if n := strings.Count(body, `aria-label="Reload channels"`); n < 5 {
		t.Errorf(`aria-label="Reload channels"`+" count = %d, want >= 5 (one per channel row)", n)
	}
	if !strings.Contains(body, `role="status" aria-live="polite"`) {
		t.Error("notifications page's toast container must be role=status aria-live=polite")
	}
	if !strings.Contains(body, `<div class="overflow-x-auto">`) {
		t.Error("notifications page's #point-rules-table must be wrapped in an overflow-x-auto container")
	}
	if !strings.Contains(body, `<th scope="col">`) {
		t.Error("notifications page's point-rules table headers must carry scope=\"col\"")
	}
}

// ---------------------------------------------------------------------
// FIX batch (Q3 review) — remaining note-level items
// ---------------------------------------------------------------------

// TestF3DropsDialogFocusRestoreReLooksUpLiveOpener pins FIX-9a: the dialog
// close handler must re-look-up the opener by its data-drop-modal key in the
// live DOM (the 30s auto-refresh can replace/detach the original opener node
// while the dialog is still open) and only fall back to the stored node
// reference when no live match exists.
func TestF3DropsDialogFocusRestoreReLooksUpLiveOpener(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/drops", "en")

	for _, want := range []string{
		"let dialogOpenerKey = null;",
		"let dialogOpenerNode = null;",
		`document.querySelector('[data-drop-modal="' + dialogOpenerKey + '"]');`,
		"document.body.contains(dialogOpenerNode)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("drops page missing dialog-focus-restore literal %q", want)
		}
	}
}

// TestF3HealthSeverityComputedOnce pins FIX-9b: severity + icon are derived
// from StatusColor in ONE four-way conditional per card and reused for both
// the card class and the glyph, instead of duplicating the hex comparison.
func TestF3HealthSeverityComputedOnce(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/api/health", "en")

	// Each StatusColor hex literal must appear at most once per card in the
	// rendered output's conditional-derivation trace: check the source
	// template instead, since the rendered HTML no longer contains the hex
	// values at all (only class names/glyphs do).
	tmpl := readEmbeddedTemplate(t, "templates/partials/health.html")
	if n := strings.Count(tmpl, `eq .StatusColor "#22c55e"`); n != 1 {
		t.Errorf(`template must compare .StatusColor to "#22c55e" exactly once (single conditional), found %d`, n)
	}
	if n := strings.Count(tmpl, `eq .StatusColor "#ef4444"`); n != 1 {
		t.Errorf(`template must compare .StatusColor to "#ef4444" exactly once (single conditional), found %d`, n)
	}
	if n := strings.Count(tmpl, `eq .StatusColor "#d9a25c"`); n != 1 {
		t.Errorf(`template must compare .StatusColor to "#d9a25c" exactly once (single conditional), found %d`, n)
	}
	if !strings.Contains(tmpl, "$sev") || !strings.Contains(tmpl, "$icon") {
		t.Error("template must derive $sev/$icon once and reuse them for both the card class and the glyph")
	}

	// Behavior is unchanged: the rendered cards still carry the right
	// severity classes and glyphs.
	if !strings.Contains(body, "health-sev-ok") || !strings.Contains(body, "✓") {
		t.Error("health partial lost its ok severity class/glyph")
	}
}

// TestF3HarnessReportsRunningStatus pins FIX-9e: the browser-evidence harness
// must mark the miner status "running" so the status overlay never covers
// the pages during an evidence run. The harness itself only runs under
// MINER_F3_HARNESS=1, so this checks the source literal directly.
func TestF3HarnessReportsRunningStatus(t *testing.T) {
	src, err := os.ReadFile("f3_harness_test.go")
	if err != nil {
		t.Fatalf("read f3_harness_test.go: %v", err)
	}
	if !strings.Contains(string(src), "srv.GetStatusBroadcaster().SetStatus(StatusRunning, \"\")") {
		t.Error("f3_harness_test.go must set the status broadcaster to StatusRunning before serving")
	}
}
