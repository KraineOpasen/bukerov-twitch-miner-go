package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
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
	if view.State != "unknown" || view.Detail != tr("ov.health.detail.noprovider") {
		t.Errorf("nil provider = %+v", view)
	}

	view = buildOverviewHealth(tr, true, health.Snapshot{}, false)
	if view.State != "unknown" || view.Detail != tr("ov.health.detail.nosignals") {
		t.Errorf("empty snapshot = %+v", view)
	}

	view = buildOverviewHealth(tr, true, health.Snapshot{Signals: []health.Signal{sig(health.SignalOAuth, health.StatusFailed)}}, false)
	wantDetail := tr("ov.health.detail.signal") + ": OAuth"
	if view.State != "unhealthy" || view.Detail != wantDetail {
		t.Errorf("failed signal = %+v, want detail %q", view, wantDetail)
	}

	// connectionLost forces unhealthy + its own detail even over a healthy snapshot.
	view = buildOverviewHealth(tr, true, health.Snapshot{Signals: []health.Signal{sig(health.SignalOAuth, health.StatusOK)}}, true)
	if view.State != "unhealthy" || view.Detail != tr("ov.health.detail.connlost") {
		t.Errorf("connection lost = %+v", view)
	}

	view = buildOverviewHealth(tr, true, health.Snapshot{Signals: []health.Signal{sig(health.SignalOAuth, health.StatusOK)}}, false)
	if view.State != "healthy" || view.Detail != "" {
		t.Errorf("healthy = %+v, want empty detail", view)
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
		{"active with rounds on the board", true, true, 2, "active"},
		{"idle, provider present, nothing on the board", true, true, 0, "idle"},
		{"unavailable, no provider", false, true, 0, "unavailable"},
		{"unavailable, no provider even with a stale count", false, true, 3, "unavailable"},
		{"unavailable, miner not running", true, false, 0, "unavailable"},
		{"unavailable, miner not running even with rounds", true, false, 5, "unavailable"},
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
// F2.4 avatar fallback: deterministic initial + color bucket, zero network.
// ---------------------------------------------------------------------------

func TestAvatarInitial(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"shroud", "S"},
		{"  bob", "B"},
		{"", "?"},
		{"   ", "?"},
		{"ёжик", "Ё"},
		{"ñandú", "Ñ"},
		{"123abc", "1"},
	}
	for _, c := range cases {
		si := StreamerInfo{Name: c.name}
		if got := si.AvatarInitial(); got != c.want {
			t.Errorf("AvatarInitial(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestAvatarBucketRangeAndDeterminism(t *testing.T) {
	names := []string{"shroud", "pokimane", "ёжик", "Ñandú", "", "  ", "a", "zzzzzzzzzzzzzzzzzzzz"}
	for _, name := range names {
		si := StreamerInfo{Name: name}
		b1 := si.AvatarBucket()
		b2 := si.AvatarBucket()
		if b1 != b2 {
			t.Errorf("AvatarBucket(%q) not deterministic: %d vs %d", name, b1, b2)
		}
		if b1 < 0 || b1 > 5 {
			t.Errorf("AvatarBucket(%q) = %d, want 0..5", name, b1)
		}
	}

	if got := (StreamerInfo{Name: ""}).AvatarBucket(); got != 0 {
		t.Errorf("empty name bucket = %d, want 0", got)
	}

	// Case/whitespace-insensitive: same identity, same bucket.
	a := StreamerInfo{Name: "Shroud"}.AvatarBucket()
	b := StreamerInfo{Name: "  shroud  "}.AvatarBucket()
	if a != b {
		t.Errorf("bucket should be case/whitespace-insensitive: %d vs %d", a, b)
	}
}

// ---------------------------------------------------------------------------
// PointsTodayRaw plumbing (toCardViews): raw ints only, never parsed back out
// of a formatted string (I13).
// ---------------------------------------------------------------------------

func TestToCardViewsPointsTodayRaw(t *testing.T) {
	infos := []StreamerInfo{{Name: "shroud", Points: 100000}, {Name: "summit", Points: 5000}}
	stats := map[string]streamerStats{"shroud": {pointsToday: 1234}}

	cards := toCardViews(infos, stats)
	if len(cards) != 2 {
		t.Fatalf("len(cards) = %d, want 2", len(cards))
	}
	if !cards[0].HasTodayRaw || cards[0].PointsTodayRaw != 1234 {
		t.Errorf("shroud card = %+v, want HasTodayRaw=true PointsTodayRaw=1234", cards[0])
	}
	if cards[1].HasTodayRaw || cards[1].PointsTodayRaw != 0 {
		t.Errorf("summit card (no stats entry) = %+v, want HasTodayRaw=false PointsTodayRaw=0", cards[1])
	}

	// nil stats map must not panic and must leave every card HasTodayRaw=false.
	nilStatsCards := toCardViews(infos, nil)
	for _, c := range nilStatsCards {
		if c.HasTodayRaw {
			t.Errorf("nil stats map: %+v should have HasTodayRaw=false", c)
		}
	}

	// Empty input returns nil, so an empty group renders no heading.
	if got := toCardViews(nil, stats); got != nil {
		t.Errorf("toCardViews(nil, ...) = %v, want nil", got)
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
	if data.Health.State != "unknown" {
		t.Errorf("Health.State = %q, want unknown (no health provider wired)", data.Health.State)
	}
	// The fake overview provider + StatusRunning + 1 live prediction: active.
	if data.PredictionsState != "active" {
		t.Errorf("PredictionsState = %q, want active", data.PredictionsState)
	}
	// shroud has a recorded points delta -> PointsTodayRaw should be plumbed
	// through onto its live card.
	found := false
	for _, c := range data.LiveCards {
		if c.Name == "shroud" {
			found = true
			if !c.HasTodayRaw {
				t.Errorf("shroud live card missing HasTodayRaw")
			}
		}
	}
	if !found {
		t.Fatal("shroud not found in LiveCards")
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

// TestOverviewToolbarSemantics covers F2.1/F2.2: the toolbar renders a
// labeled filter input, a clear button, and a sort select with all four
// values — all OUTSIDE #overview-live.
func TestOverviewToolbarSemantics(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	toolbarIdx := strings.Index(body, `data-ov-toolbar`)
	liveIdx := strings.Index(body, `id="overview-live"`)
	if toolbarIdx < 0 || liveIdx < 0 {
		t.Fatalf("missing toolbar or #overview-live marker in rendered page")
	}
	if toolbarIdx >= liveIdx {
		t.Errorf("toolbar (offset %d) must render BEFORE #overview-live (offset %d), so it survives the htmx swap", toolbarIdx, liveIdx)
	}

	for _, want := range []string{
		`id="ov-filter-input"`,
		`id="ov-filter-clear"`,
		`id="ov-sort-select"`,
		`<option value="default">`,
		`<option value="points">`,
		`<option value="today">`,
		`<option value="name">`,
		`id="ov-filter-count"`,
		`aria-live="polite"`,
		"Filter streamers", "Filter by name", "Clear filter", "Sort",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
}

// TestOverviewCardDataAttributes covers F2.2/F2.3: cards carry the raw
// integer data attributes the sort comparators read (never a formatted
// string), plus the F2.4 avatar span. data-ov-today pins I13 for the "today"
// sort specifically: newOverviewTestServer seeds shroud with two same-day
// RecordPoints calls (95000 then 100000), so points-today resolves to the
// exact raw delta 5000 — never a formatted "5,000" string.
func TestOverviewCardDataAttributes(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	for _, want := range []string{
		`data-ov-name="shroud"`,
		`data-ov-points="100000"`,
		`data-ov-today="5000"`,
		`class="s-avatar s-avatar-b`,
		`aria-hidden="true">S</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing %q", want)
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

// TestOverviewHealthChip covers F2.6: the aggregated health chip links to
// /health and carries an icon + localized text (never color alone).
func TestOverviewHealthChip(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	if !strings.Contains(body, `href="/health"`) {
		t.Error("health chip must link to /health")
	}
	if !strings.Contains(body, `class="ov-health ov-health-unknown"`) {
		t.Errorf("health chip should render state 'unknown' (no health provider wired)")
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
// content for the documented client-side contracts: the exact localStorage
// key names (I24), the __ovDelta single-instance guard, the performance.now
// test seam, and no newly-introduced external resource URLs (I26).
func TestOverviewScriptContracts(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	for _, want := range []string{
		"'miner-ov-filter'",
		"'miner-ov-sort'",
		"window.__ovDelta",
		"window.__OV_CLOCK",
		"performance.now()",
		"htmx:afterSwap",
		"js.ov.shown_count",
		"js.ov.sort_applied",
		// Regression guard for the F2.5 bug: the stale-clock key-building
		// literal must be the js.-prefixed "js.ov.fresh_" (+ state), never
		// the bare server-only "ov.fresh." that doesn't resolve client-side.
		"t('js.ov.fresh_' + state)",
		"t('js.ov.fresh_aria')",
		// Regression guard for the Q3 W1/W2 findings: per-second role="status"
		// re-announcement spam from tickStale() rewriting the DOM every tick
		// even when the state hadn't changed (W1), and the aria-live filter
		// count being rewritten (and thus re-announced) on every 30s swap
		// even when unchanged (W2). These literals pin the change-gating that
		// fixed both, plus the data-generated clamp entry point (W3) that
		// must stay wired in.
		"lastStaleState",
		"if (state === lastStaleState) return;",
		"resetStaleBaseline",
		"countEl.textContent !== newCount",
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

// TestOverviewNoResultsElementPresent covers F2.1: the global empty-filter
// state element exists in the grid, hidden by default.
func TestOverviewNoResultsElementPresent(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	if !strings.Contains(body, `data-ov-noresults hidden`) {
		t.Error("rendered page missing the hidden global no-results element")
	}
	if !strings.Contains(body, "No streamers match the filter.") {
		t.Error("rendered page missing the localized no-results copy")
	}
}

// TestOverviewGroupSections covers F2.1/F2.2: each card group is wrapped in
// a data-ov-group section so the filter/sort JS can hide/scope per group.
func TestOverviewGroupSections(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := renderDashboardEN(t, srv)

	if !strings.Contains(body, `data-ov-group="live"`) {
		t.Error("rendered page missing the live group's data-ov-group wrapper")
	}
	if !strings.Contains(body, `data-ov-group="offline"`) {
		t.Error("rendered page missing the offline group's data-ov-group wrapper")
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
		"js.ov.shown_count", "js.ov.sort_applied",
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
