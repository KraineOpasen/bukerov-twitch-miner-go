package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// ---------------------------------------------------------------------------
// F2.6 aggregateHealth: every precedence branch (design.md §1/§3 I14).
// ---------------------------------------------------------------------------

func sig(name, status string) health.Signal { return health.Signal{Name: name, Status: status} }

func TestAggregateHealth(t *testing.T) {
	cases := []struct {
		name            string
		providerPresent bool
		snap            health.Snapshot
		connectionLost  bool
		wantState       string
		wantReason      string
		wantOffending   []string
	}{
		{
			name:       "nil provider is unknown, never a false healthy",
			wantState:  "unknown",
			wantReason: "noprovider",
		},
		{
			name:            "empty snapshot is unknown",
			providerPresent: true,
			snap:            health.Snapshot{},
			wantState:       "unknown",
			wantReason:      "nosignals",
		},
		{
			name:            "all ok is healthy",
			providerPresent: true,
			snap:            health.Snapshot{Signals: []health.Signal{sig(health.SignalOAuth, health.StatusOK), sig(health.SignalPubSub, health.StatusIdle)}},
			wantState:       "healthy",
			wantReason:      "",
		},
		{
			name:            "one degraded signal",
			providerPresent: true,
			snap:            health.Snapshot{Signals: []health.Signal{sig(health.SignalOAuth, health.StatusOK), sig(health.SignalPubSub, health.StatusDegraded)}},
			wantState:       "degraded",
			wantReason:      "signal",
			wantOffending:   []string{"PubSub"},
		},
		{
			name:            "failed signal wins over degraded",
			providerPresent: true,
			snap: health.Snapshot{Signals: []health.Signal{
				sig(health.SignalPubSub, health.StatusDegraded),
				sig(health.SignalGQLAPI, health.StatusFailed),
			}},
			wantState:     "unhealthy",
			wantReason:    "signal",
			wantOffending: []string{"GQL API"},
		},
		{
			name:            "stalled signal is unhealthy",
			providerPresent: true,
			snap:            health.Snapshot{Signals: []health.Signal{sig(health.SignalDropsProgress, health.StatusStalled)}},
			wantState:       "unhealthy",
			wantReason:      "signal",
			wantOffending:   []string{"Drops Progress"},
		},
		{
			name:            "unknown-status signal, no failure",
			providerPresent: true,
			snap:            health.Snapshot{Signals: []health.Signal{sig(health.SignalOAuth, health.StatusUnknown)}},
			wantState:       "unknown",
			wantReason:      "signal",
			wantOffending:   []string{"OAuth"},
		},
		{
			name:            "connectionLost wins over an otherwise-healthy snapshot",
			providerPresent: true,
			snap:            health.Snapshot{Signals: []health.Signal{sig(health.SignalOAuth, health.StatusOK)}},
			connectionLost:  true,
			wantState:       "unhealthy",
			wantReason:      "connlost",
		},
		{
			name:           "connectionLost wins even with no provider",
			connectionLost: true,
			wantState:      "unhealthy",
			wantReason:     "connlost",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, reason, offending := aggregateHealth(c.providerPresent, c.snap, c.connectionLost)
			if state != c.wantState {
				t.Errorf("state = %q, want %q", state, c.wantState)
			}
			if reason != c.wantReason {
				t.Errorf("reason = %q, want %q", reason, c.wantReason)
			}
			if len(offending) != len(c.wantOffending) {
				t.Fatalf("offending = %v, want %v", offending, c.wantOffending)
			}
			for i := range offending {
				if offending[i] != c.wantOffending[i] {
					t.Errorf("offending[%d] = %q, want %q", i, offending[i], c.wantOffending[i])
				}
			}
		})
	}
}

// TestBuildOverviewHealthDetail pins the localized Detail text per branch —
// the tooltip basis a human reads to understand *why* the chip says what it
// says (I18: never color alone).
func TestBuildOverviewHealthDetail(t *testing.T) {
	tr := enTR(t)

	view := buildOverviewHealth(tr, false, health.Snapshot{}, false)
	if view.State != overviewHealthUnknown || view.Detail != tr("ov.health.detail.noprovider") {
		t.Errorf("nil provider = %+v", view)
	}

	view = buildOverviewHealth(tr, true, health.Snapshot{}, false)
	if view.State != overviewHealthUnknown || view.Detail != tr("ov.health.detail.nosignals") {
		t.Errorf("empty snapshot = %+v", view)
	}

	view = buildOverviewHealth(tr, true, health.Snapshot{Signals: []health.Signal{sig(health.SignalOAuth, health.StatusFailed)}}, false)
	wantDetail := tr("ov.health.detail.signal") + ": OAuth"
	if view.State != overviewHealthDegraded || view.Detail != wantDetail {
		t.Errorf("failed signal = %+v, want detail %q", view, wantDetail)
	}

	// connectionLost forces a not-OK verdict + its own detail even over a
	// healthy snapshot.
	view = buildOverviewHealth(tr, true, health.Snapshot{Signals: []health.Signal{sig(health.SignalOAuth, health.StatusOK)}}, true)
	if view.State != overviewHealthDegraded || view.Detail != tr("ov.health.detail.connlost") {
		t.Errorf("connection lost = %+v", view)
	}

	view = buildOverviewHealth(tr, true, health.Snapshot{Signals: []health.Signal{sig(health.SignalOAuth, health.StatusOK)}}, false)
	if view.State != overviewHealthOK || view.Detail != "" {
		t.Errorf("healthy = %+v, want empty detail", view)
	}

	// Every frozen value maps to a real localized label and a built CSS class —
	// never an empty string that would render a blank chip.
	for _, state := range []string{overviewHealthOK, overviewHealthDegraded, overviewHealthUnknown} {
		if key := overviewHealthLabelKeys[state]; key == "" || tr(key) == "" || tr(key) == key {
			t.Errorf("health value %q has no localized label", state)
		}
		if overviewHealthClasses[state] == "" {
			t.Errorf("health value %q has no CSS class", state)
		}
	}
}

// ---------------------------------------------------------------------------
// F2.7 predictionsState: active/idle/unavailable, including a non-running
// miner (I3/I4-adjacent: this never touches manualUI/__predBusy).
// ---------------------------------------------------------------------------

func TestPredictionsState(t *testing.T) {
	cases := []struct {
		name            string
		providerPresent bool
		minerRunning    bool
		count           int
		want            string
	}{
		{"active with rounds on the board", true, true, 2, predictionsStateActive},
		{"idle, provider present, nothing on the board", true, true, 0, predictionsStateIdle},
		{"unavailable, no provider", false, true, 0, predictionsStateUnavailable},
		{"unavailable, no provider even with a stale count", false, true, 3, predictionsStateUnavailable},
		{"unavailable, miner not running", true, false, 0, predictionsStateUnavailable},
		{"unavailable, miner not running even with rounds", true, false, 5, predictionsStateUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := predictionsState(c.providerPresent, c.minerRunning, c.count); got != c.want {
				t.Errorf("predictionsState(%v,%v,%d) = %q, want %q", c.providerPresent, c.minerRunning, c.count, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Live-count derivation. The Overview no longer renders per-streamer cards, so
// the only thing it still reads out of buildCards is the CONFIRMED-online
// count: a streamer merely holding a slot through a transient unknown is not
// live.
// ---------------------------------------------------------------------------

func TestOverviewLiveCountCountsConfirmedOnlineOnly(t *testing.T) {
	online := models.NewStreamer("liveone", models.DefaultStreamerSettings())
	online.SetConfirmedOnline()

	slotted := models.NewStreamer("unsure", models.DefaultStreamerSettings())
	slotted.SetConfirmedOnline()
	slotted.SetUnknown(models.ReasonTransportError) // unknown, but still holds a slot

	offline := models.NewStreamer("gone", models.DefaultStreamerSettings())
	offline.SetConfirmedOffline()

	srv := &Server{}
	slots := WatchSlotsView{Watching: map[string]bool{"unsure": true}}
	live, _, _, _ := srv.buildCards(
		[]*models.Streamer{online, slotted, offline},
		slots, map[string]streamerStats{}, map[string]bool{}, echoTr,
	)

	if len(live) != 2 {
		t.Fatalf("live group = %d entries, want 2 (confirmed-online + slotted-unconfirmed)", len(live))
	}
	confirmed := 0
	for _, c := range live {
		if c.IsLive {
			confirmed++
		}
	}
	if confirmed != 1 {
		t.Errorf("confirmed-online count = %d, want 1 (the unconfirmed slot holder must not count as live)", confirmed)
	}
}

// ---------------------------------------------------------------------------
// F2.5 single-source poll interval + GeneratedUnix plumbing (I1).
// ---------------------------------------------------------------------------

func TestOverviewPollSecondsIs30(t *testing.T) {
	if overviewPollSeconds != 30 {
		t.Fatalf("overviewPollSeconds = %d, want 30", overviewPollSeconds)
	}
}

func TestBuildOverviewDataGeneratedUnixAndPollSeconds(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)

	data := srv.buildOverviewData(i18n.LangEN)
	if data.GeneratedUnix <= 0 {
		t.Errorf("GeneratedUnix = %d, want > 0", data.GeneratedUnix)
	}
	if data.PollSeconds != overviewPollSeconds {
		t.Errorf("PollSeconds = %d, want %d", data.PollSeconds, overviewPollSeconds)
	}
	// newOverviewTestServer wires no health provider: aggregate health must
	// read "unknown", never a false-positive "healthy".
	if data.Health.State != overviewHealthUnknown {
		t.Errorf("Health.State = %q, want unknown (no health provider wired)", data.Health.State)
	}
	// The fake overview provider + StatusRunning + 1 live prediction: active,
	// reachable, and therefore a PROVEN round count.
	if data.PredictionsKPI.State != "active" {
		t.Errorf("PredictionsKPI.State = %q, want active", data.PredictionsKPI.State)
	}
	if !data.PredictionsKPI.Available || data.PredictionsKPI.ActiveCount != 1 {
		t.Errorf("PredictionsKPI = %+v, want Available=true ActiveCount=1", data.PredictionsKPI)
	}
	// shroud is confirmed online, so it is counted as live.
	if data.LiveCount != 1 {
		t.Errorf("LiveCount = %d, want 1", data.LiveCount)
	}
}

// ---------------------------------------------------------------------------
// Template / static-scan coverage (ST) — rendered via the real handler, the
// same path production traffic takes.
// ---------------------------------------------------------------------------

// renderDashboardEN renders the full Overview page (base.html + overview.html
// + partials) in English via the real handler, exactly as handleDashboard is
// wired in production.
func renderDashboardEN(t *testing.T, srv *Server) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: langCookieName, Value: i18n.LangEN})
	rec := httptest.NewRecorder()
	srv.handleDashboard(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handleDashboard status = %d, body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestOverviewFreshnessBadgeSurvivesToolbarRemoval pins what actually has to
// stay true after the roster controls left the page: the freshness badge is
// still rendered, still BEFORE #overview-live (so the 30s htmx swap can never
// destroy the role="status" node mid-announcement), and the filter/sort
// controls it used to sit beside are gone.
func TestOverviewFreshnessBadgeSurvivesToolbarRemoval(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	badgeIdx := strings.Index(body, `id="ov-stale-badge"`)
	liveIdx := strings.Index(body, `id="overview-live"`)
	if badgeIdx < 0 {
		t.Fatal("the freshness badge must survive the toolbar removal")
	}
	if liveIdx < 0 {
		t.Fatal("missing #overview-live marker in rendered page")
	}
	if badgeIdx >= liveIdx {
		t.Errorf("freshness badge (offset %d) must render BEFORE #overview-live (offset %d), so it survives the htmx swap", badgeIdx, liveIdx)
	}

	for _, gone := range []string{
		`data-ov-toolbar`, `id="ov-filter-input"`, `id="ov-filter-clear"`,
		`id="ov-sort-select"`, `id="ov-filter-count"`, `<option value="points">`,
	} {
		if strings.Contains(body, gone) {
			t.Errorf("roster control %q is still rendered on /overview", gone)
		}
	}
}

// TestOverviewRendersNoStreamerCards is the counterpart to the removed card
// data-attribute test: the raw sort attributes existed only for the roster
// grid, which /overview/queue now owns. Non-vacuous — the same fixture
// streamer is proven to still be reachable through the page's data.
func TestOverviewRendersNoStreamerCards(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	if srv.buildOverviewData(i18n.LangEN).StreamerCount == 0 {
		t.Fatal("precondition failed: no streamers in the fixture, so the assertions below are vacuous")
	}
	for _, gone := range []string{
		`data-ov-name="shroud"`, `data-ov-points="100000"`, `data-ov-today="5000"`,
		`class="s-avatar s-avatar-b`, `class="s-card`,
	} {
		if strings.Contains(body, gone) {
			t.Errorf("streamer-card surface %q is still rendered on /overview", gone)
		}
	}
}

// TestOverviewHxTriggerPollInterval pins I1 end-to-end: the RENDERED page's
// #overview-live carries the exact "every 30s, refresh" hx-trigger, sourced
// from the same overviewPollSeconds constant the F2.5 stale-clock thresholds
// are derived from (not a separately-hardcoded "30" that could drift).
func TestOverviewHxTriggerPollInterval(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	if overviewPollSeconds != 30 {
		t.Fatalf("overviewPollSeconds = %d, want 30 (this test's literal assertion below assumes 30)", overviewPollSeconds)
	}
	if !strings.Contains(body, `hx-trigger="every 30s, refresh"`) {
		t.Error(`rendered page missing hx-trigger="every 30s, refresh" on #overview-live`)
	}
}

// TestOverviewMetaMarker covers F2.5: the hidden #ov-meta marker carries the
// generation timestamp inside the swapped partial.
func TestOverviewMetaMarker(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	if !strings.Contains(body, `id="ov-meta"`) {
		t.Fatal("rendered page missing #ov-meta")
	}
	if !strings.Contains(body, `data-generated="`) {
		t.Error("rendered page missing #ov-meta's data-generated attribute")
	}
	if !strings.Contains(body, `data-connection-lost="0"`) {
		t.Error("rendered page should show data-connection-lost=\"0\" for a healthy connection")
	}
}

// TestOverviewHealthChip covers the compact aggregate: it links to the
// canonical System owner (never the legacy /health route) and carries an icon
// plus localized text, never colour alone. This is the single owner of the
// Overview's health-link invariants.
func TestOverviewHealthChip(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	if !strings.Contains(body, `href="/system/status"`) {
		t.Error("health chip must link to /system/status")
	}
	if strings.Contains(body, `href="/health"`) {
		t.Error("health chip must not link to the legacy /health route")
	}
	// ...and that ban is about the LINK, not the route: /health itself is still
	// served (its retirement is a separate, later step), so the check above can
	// never be satisfied by the endpoint having quietly disappeared.
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /health = %d, want 200 — only the Overview link moved, the route stays served", rec.Code)
	}
	if !strings.Contains(body, `data-ov-health-state="unknown"`) {
		t.Error("health chip should render the aggregate 'unknown' (no health provider wired)")
	}
	if !strings.Contains(body, `class="ov-health ov-health-unknown"`) {
		t.Error("health chip missing its built CSS class for the unknown state")
	}
	if !strings.Contains(body, "<svg") {
		t.Error("health chip should render an icon, not text alone")
	}
	if !strings.Contains(body, "Unknown") {
		t.Error("health chip missing localized label text")
	}
}

// TestOverviewPredictionsChip covers F2.7: the technical predictions-board
// status chip renders near pred.heading. Per W8, it must NOT carry
// role="status": the chip lives inside #overview-live, so it's a brand new
// DOM node on every 30s innerHTML swap and would never fire a live-region
// announcement — only false-promise one. Its icon + visible text stay in the
// accessibility tree regardless (normal document reading order).
func TestOverviewPredictionsChip(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	chipStart := strings.Index(body, `class="ov-pred-chip ov-pred-chip-active"`)
	if chipStart < 0 {
		t.Fatal("predictions chip should render state 'active' (a live round is on the board)")
	}
	chipEnd := strings.Index(body[chipStart:], "</span>")
	if chipEnd < 0 {
		t.Fatal("could not find the predictions chip's closing tag")
	}
	chipMarkup := body[chipStart : chipStart+chipEnd]
	if strings.Contains(chipMarkup, `role="status"`) {
		t.Error(`predictions chip must NOT carry role="status" (W8: it's a fresh DOM node every swap, so it never actually announces)`)
	}

	// role="status" still exists elsewhere on the page (the F2.5 stale
	// badge) — dropping it from the pred chip specifically shouldn't be
	// confused with dropping it everywhere.
	if !strings.Contains(body, `id="ov-stale-badge"`) || !strings.Contains(body, `role="status"`) {
		t.Error("the stale badge should still carry role=\"status\" elsewhere on the page")
	}
}

// TestUpdaterSlotInert covers F2.8: the placeholder is hidden, aria-hidden,
// and referenced by NO script in the page — not a fetch, not a link.
func TestUpdaterSlotInert(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	if !strings.Contains(body, `<div id="updater-widget-slot" hidden aria-hidden="true" data-updater-slot></div>`) {
		t.Fatal("updater-widget-slot must render hidden + aria-hidden, with no other attributes")
	}
	// The only two allowed occurrences of "updater" are inside that one inert
	// div's id and data attribute — never inside a <script> block (getElementById
	// call, querySelector, fetch target, etc).
	if got, want := strings.Count(body, "updater"), 2; got != want {
		t.Errorf(`the word "updater" appears %d times, want exactly %d (the inert placeholder's id + data attribute only)`, got, want)
	}
	if strings.Contains(body, "/releases") {
		t.Error("rendered page must not reference a releases/update-check endpoint")
	}
}

// TestOverviewScriptContracts is a static scan of the rendered page's script
// content, limited to the client-side names that are genuinely CONTRACTS with
// something outside the script: the __ovDelta / __OV_CLOCK globals other code
// and tests attach to, the performance.now test seam, the htmx swap hook, the
// js.-prefixed i18n keys window.I18N must actually contain, and
// resetStaleBaseline — the entry point overview_live.html's own #ov-meta
// comment documents as the data-generated clamp. Plus no newly-introduced
// external resource URLs (I26).
//
// Internal implementation details of the stale clock are deliberately NOT
// pinned here. They have no rendered proxy and no cross-file contract, so a
// byte-exact source assertion on them would only forbid refactoring.
func TestOverviewScriptContracts(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	for _, want := range []string{
		"window.__ovDelta",
		"window.__OV_CLOCK",
		"performance.now()",
		"htmx:afterSwap",
		// Regression guard for the F2.5 bug: the stale-clock key-building
		// literal must be the js.-prefixed "js.ov.fresh_" (+ state), never
		// the bare server-only "ov.fresh." that doesn't resolve client-side.
		"t('js.ov.fresh_' + state)",
		"t('js.ov.fresh_aria')",
		// The data-generated clamp entry point (W3): overview_live.html's
		// #ov-meta comment names resetStaleBaseline() as the function its
		// data-generated attribute exists to feed, so the two files have a
		// real contract on this name.
		"resetStaleBaseline",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page/script missing %q", want)
		}
	}
	if strings.Contains(body, "'ov.fresh.") {
		t.Error("rendered script still references the old, non-js.-prefixed 'ov.fresh.*' keys — window.I18N only ever contains js.* keys, so these would render literally in the browser")
	}

	// No new external resource loads: the only http(s):// occurrences allowed
	// are pre-existing ones (there are none on this page — everything is
	// vendored/local), so this is an absolute assertion.
	if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
		t.Error("rendered Overview page must not load any external http(s) resource")
	}
}

// ovScriptFunctionBody extracts the body of `function <name>(...) { ... }` from
// the RENDERED page by matching braces from the function's own opening one, so
// an assertion can be scoped to that function instead of to the whole page.
// Brace matching (rather than an indentation or line pattern) is what keeps this
// off the script's formatting: reflowing the file must never change a contract.
// Brace counting is deliberately naive about braces inside string literals —
// there are none in the functions this is used on — so callers must sanity-check
// the returned slice against landmarks it has to contain, and fail loudly on a
// mis-extraction rather than let a negative assertion pass on garbage.
func ovScriptFunctionBody(t *testing.T, body, name string) string {
	t.Helper()
	sig := regexp.MustCompile(`function\s+` + regexp.QuoteMeta(name) + `\s*\([^)]*\)\s*\{`)
	loc := sig.FindStringIndex(body)
	if loc == nil {
		t.Fatalf("rendered page declares no function %s(...)", name)
	}
	if sig.FindStringIndex(body[loc[1]:]) != nil {
		t.Fatalf("rendered page declares function %s(...) more than once — extraction is ambiguous", name)
	}
	depth := 1
	for i := loc[1]; i < len(body); i++ {
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return body[loc[1]:i]
			}
		}
	}
	t.Fatalf("function %s(...) is never closed in the rendered page", name)
	return ""
}

// ovStaleDedupGuardRe matches an early return guarded by an equality comparison
// of two identifiers — `if (a === b) return;` — capturing BOTH so a test can
// work out which one is the previous-state cache from what the code does with
// them, instead of hard-coding the cache's name.
var ovStaleDedupGuardRe = regexp.MustCompile(`if\s*\(\s*([A-Za-z_$][A-Za-z0-9_$]*)\s*===\s*([A-Za-z_$][A-Za-z0-9_$]*)\s*\)\s*return\s*;`)

// TestOverviewStaleClockSkipsRewriteWhenStateUnchanged is the W1 regression
// guard. tickStale() runs once a second, but #ov-stale-badge is a role="status"
// live region: rewriting its classes/text/aria-label on a tick where the
// computed freshness state did NOT change re-announces the very same state to
// assistive technology every second, which is exactly the announcement spam
// role="status" exists to avoid.
//
// The contract asserted is semantic, not textual: tickStale() keeps a
// previous-state cache OUTSIDE itself, returns early when the newly computed
// state equals it, updates it only once past that guard, and performs every DOM
// write after it. The cache's identifier is captured from the code rather than
// spelled out here, and no whitespace or formatting is part of the contract.
func TestOverviewStaleClockSkipsRewriteWhenStateUnchanged(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	tick := ovScriptFunctionBody(t, body, "tickStale")
	// Sanity-check the extraction FIRST: these are the freshness clock's own
	// landmarks, so their absence means the slice is not tickStale()'s body and
	// every assertion below would be meaningless. This also keeps the
	// js.ov.fresh_* client-key usage pinned inside the function that resolves it.
	for _, landmark := range []string{
		"staleBadge", "POLL_MS",
		"t('js.ov.fresh_' + state)", "t('js.ov.fresh_aria')",
	} {
		if !strings.Contains(tick, landmark) {
			t.Fatalf("extracted tickStale() body is missing landmark %q — either the extraction is wrong or the freshness clock was rewritten; body=%s", landmark, tick)
		}
	}

	guard := ovStaleDedupGuardRe.FindStringSubmatchIndex(tick)
	if guard == nil {
		t.Fatalf("tickStale() has no `if (<state> === <cache>) return;` early return, so an unchanged freshness state still rewrites the role=\"status\" badge on every one-second tick; body=%s", tick)
	}
	guardEnd := guard[1]
	lhs, rhs := tick[guard[2]:guard[3]], tick[guard[4]:guard[5]]

	// Which operand is the CACHE is decided by what the code does, not by its
	// name: the cache is the one the freshly computed state gets written into.
	var cache, state string
	assignAt := -1
	for _, pair := range [2][2]string{{lhs, rhs}, {rhs, lhs}} {
		assign := regexp.MustCompile(`\b` + regexp.QuoteMeta(pair[0]) + `\s*=\s*` + regexp.QuoteMeta(pair[1]) + `\s*;`)
		if at := assign.FindStringIndex(tick); at != nil {
			cache, state, assignAt = pair[0], pair[1], at[0]
			break
		}
	}
	if cache == "" {
		t.Fatalf("neither operand of tickStale()'s `%s === %s` guard is ever assigned the other, so nothing remembers the state last written and the guard can never be true; body=%s", lhs, rhs, tick)
	}

	// The cache is updated only PAST the guard. A write before it would compare
	// the newly computed state against itself, and the early return would never
	// fire on a genuinely unchanged state.
	if assignAt < guardEnd {
		t.Errorf("tickStale() writes `%s = %s` at offset %d, before its own de-dup guard ends at %d — the same-state comparison can then never be true", cache, state, assignAt, guardEnd)
	}

	// ...and so is every DOM write the guard exists to skip.
	for _, write := range []string{"classList", "textContent", "setAttribute"} {
		at := strings.Index(tick, write)
		if at < 0 {
			t.Errorf("tickStale() performs no %q DOM write at all — the de-dup guard has nothing left to protect", write)
			continue
		}
		if at < guardEnd {
			t.Errorf("tickStale() performs a %q DOM write at offset %d, before the same-state early return at %d — an unchanged state still rewrites the badge", write, at, guardEnd)
		}
	}

	// The cache must live OUTSIDE tickStale(): a per-call declaration is reset
	// on every tick, so the comparison would always be against a fresh initial
	// value and the guard would never fire however correct it looks.
	decl := regexp.MustCompile(`\b(?:var|let|const)\s+` + regexp.QuoteMeta(cache) + `\b`)
	if decl.MatchString(tick) {
		t.Errorf("the previous-state cache %s is declared INSIDE tickStale(), so it is discarded on every tick and the de-dup guard can never fire", cache)
	}
	if !decl.MatchString(body) {
		t.Errorf("the previous-state cache %s is never declared in the rendered script", cache)
	}

	// Non-vacuity: the element the guard protects really is a live region — that
	// is the entire reason an unchanged state must not be rewritten — and the
	// baseline anchor that feeds this clock is still wired in.
	badge := regexp.MustCompile(`<[a-z]+[^>]*id="ov-stale-badge"[^>]*>`).FindString(body)
	if badge == "" {
		t.Fatal("#ov-stale-badge is not rendered, so tickStale() writes to nothing and this guard is vacuous")
	}
	if !strings.Contains(badge, `role="status"`) {
		t.Errorf(`#ov-stale-badge must still carry role="status" — without a live region there is nothing for the de-dup guard to protect; tag=%s`, badge)
	}
	if !strings.Contains(body, "resetStaleBaseline") {
		t.Error("resetStaleBaseline — the data-generated clamp that anchors this clock — is no longer in the rendered script")
	}
}

// TestOverviewRendersNoRosterGrid replaces the two grid-scoping tests (the
// no-results element and the per-group wrappers): both existed only to support
// the client-side filter over the card grid, and /overview/queue owns that
// roster now. Non-vacuous — the fixture's groups are proven non-empty.
func TestOverviewRendersNoRosterGrid(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	live, _, offline, _ := srv.buildCards(srv.snapshotStreamers(), WatchSlotsView{Watching: map[string]bool{}}, nil, map[string]bool{}, echoTr)
	if len(live) == 0 || len(offline) == 0 {
		t.Fatalf("precondition failed: fixture has %d live / %d offline streamers, so the grid assertions are vacuous", len(live), len(offline))
	}

	for _, gone := range []string{
		`data-ov-noresults`, `data-ov-group="live"`, `data-ov-group="offline"`,
		`id="grid"`, `class="card-grid`,
	} {
		if strings.Contains(body, gone) {
			t.Errorf("roster-grid surface %q is still rendered on /overview", gone)
		}
	}
}

// jsTCallKeyPattern matches every client-side `t('...'` / `t("..."` call in
// overview.html's <script> blocks, capturing the leading string literal — the
// full key for a direct call, or the literal key PREFIX when the script
// builds the key via concatenation (e.g. t('js.ov.fresh_' + state)).
var jsTCallKeyPattern = regexp.MustCompile(`\bt\(\s*['"]([a-zA-Z0-9_.]+)['"]`)

// TestOverviewScriptOnlyUsesJSPrefixedKeys is the regression guard for the F2.5
// bug where the stale-badge script called window.t() with server-only "ov.*"
// keys. The client catalog (window.I18N, populated from i18n.JSMessages) only
// ever contains keys under the "js." prefix (internal/i18n/i18n.go's
// jsKeyPrefix) — any other key silently renders as the literal key string in
// the browser, with no compile-time or template-render-time signal. This
// statically scans every t(...) call in overview.html and fails if any
// literal key/prefix isn't "js."-prefixed, then confirms the concrete
// freshness-clock keys actually resolve in BOTH locale's client catalogs.
func TestOverviewScriptOnlyUsesJSPrefixedKeys(t *testing.T) {
	raw, err := templatesFS.ReadFile("templates/overview.html")
	if err != nil {
		t.Fatalf("read overview.html: %v", err)
	}
	src := string(raw)

	matches := jsTCallKeyPattern.FindAllStringSubmatch(src, -1)
	if len(matches) == 0 {
		t.Fatal("no client-side t(...) calls found in overview.html — pattern is stale, update it")
	}
	for _, m := range matches {
		key := m[1]
		if !strings.HasPrefix(key, "js.") {
			t.Errorf("script calls t(%q...) with a non-\"js.\"-prefixed key/prefix — window.I18N only ever contains js.* keys (i18n.JSMessages), so this renders as the literal key string in the browser", key)
		}
	}

	// The concrete keys the freshness clock resolves at runtime (the
	// 'js.ov.fresh_' concatenation prefix + each state, plus the aria label
	// key) must exist in the CLIENT catalog for every supported language —
	// not merely somewhere in the server-side catalog.
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	wantClientKeys := []string{
		"js.ov.fresh_fresh", "js.ov.fresh_delayed", "js.ov.fresh_stale", "js.ov.fresh_lost", "js.ov.fresh_aria",
	}
	for _, lang := range i18n.SupportedLangs() {
		msgs := loc.JSMessages(lang)
		for _, key := range wantClientKeys {
			if _, ok := msgs[key]; !ok {
				t.Errorf("lang %q: client catalog (JSMessages) missing %q — the __ovDelta script cannot resolve it in the browser", lang, key)
			}
		}
	}

	// The old, buggy server-only keys must be gone entirely (no duplicate
	// unprefixed copies left behind after the rename).
	for _, lang := range i18n.SupportedLangs() {
		for _, oldKey := range []string{"ov.fresh.fresh", "ov.fresh.delayed", "ov.fresh.stale", "ov.fresh.lost", "ov.fresh.aria"} {
			for _, k := range loc.Keys(lang) {
				if k == oldKey {
					t.Errorf("lang %q: locale still defines the old key %q — it must be renamed to a js.*-prefixed key, not duplicated", lang, oldKey)
				}
			}
		}
	}
}

// TestServerSideTResolvesJSPrefixedKeys proves the server-side t() helper
// (unlike window.t in the browser) resolves ANY key, including js.*-prefixed
// ones — so seeding the initial, server-rendered freshness badge text from
// "js.ov.fresh_fresh" is safe and renders real text, not the literal key.
func TestServerSideTResolvesJSPrefixedKeys(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	if !strings.Contains(body, `<span id="ov-stale-text">Live</span>`) {
		t.Error(`initial server-rendered stale badge must show the resolved English text "Live", not a raw key`)
	}
	if strings.Contains(body, "js.ov.fresh_fresh<") || strings.Contains(body, ">ov.fresh.fresh<") {
		t.Error("stale badge rendered a raw/unresolved i18n key instead of translated text")
	}
}
