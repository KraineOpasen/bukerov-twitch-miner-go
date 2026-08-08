package web

// S5-3c — final canonical /overview composition.
//
// Every assertion here is on RENDERED behaviour reachable through a real page
// or partial render: the frozen v3 region inventory, its DOM order, the
// evidence gates that decide whether a conditional region exists at all, and
// the honesty invariants (unknown never reads as 0, unknown never reads as
// healthy). Regions are located by the stable data-ov-* attributes they render
// -- never by counting tags -- and every lookup fails loudly when the region it
// names has disappeared.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// ---------------------------------------------------------------------------
// Fixtures / helpers
// ---------------------------------------------------------------------------

// s53cPage renders the full /overview page (base chrome + overview.html +
// the inlined overview_live partial) for the given language.
func s53cPage(t *testing.T, lang string) string {
	t.Helper()
	srv, _, _ := newOverviewTestServer(t)
	return s53cPageFor(t, srv, lang)
}

// s53cPageFor renders /overview for a caller-supplied server, so a test can
// wire its own providers (health, predictions) first.
func s53cPageFor(t *testing.T, srv *Server, lang string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/overview", nil)
	if lang != "" {
		req.AddCookie(&http.Cookie{Name: langCookieName, Value: lang})
	}
	rec := httptest.NewRecorder()
	srv.handleOverviewPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /overview = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// s53cRenderLive renders the overview_live partial from an explicit view model,
// so evidence-gated regions can be exercised in both their present and absent
// states without depending on process-wide state.
func s53cRenderLive(t *testing.T, data OverviewPageData) string {
	t.Helper()
	var buf bytes.Buffer
	if err := testPartials(t).ExecuteTemplate(&buf, "overview_live", data); err != nil {
		t.Fatalf("render overview_live: %v", err)
	}
	return buf.String()
}

// s53cMarkerAt returns the byte offset of a REQUIRED rendered region marker and
// fails the test loudly when that region is not on the page at all -- a missing
// region is never silently treated as "ordered correctly" (review finding S7).
func s53cMarkerAt(t *testing.T, body, marker string) int {
	t.Helper()
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("required Overview region marker %q is not rendered", marker)
	}
	if j := strings.Index(body[i+len(marker):], marker); j >= 0 {
		t.Fatalf("Overview region marker %q rendered more than once", marker)
	}
	return i
}

// s53cDataOvAttrs returns every distinct data-ov-* attribute name in the
// rendered output. It is what pins the region inventory exactly: a region that
// comes back (a roster grid, a filter toolbar, a per-subsystem health row)
// brings its attribute with it and immediately widens this set.
var s53cDataOvRe = regexp.MustCompile(`data-ov-[a-z0-9-]+`)

func s53cDataOvAttrs(body string) []string {
	seen := map[string]bool{}
	for _, m := range s53cDataOvRe.FindAllString(body, -1) {
		seen[m] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// s53cHealthProvider is a health provider whose snapshot the test controls.
type s53cHealthProvider struct{ snap health.Snapshot }

func (p *s53cHealthProvider) HealthSnapshot() health.Snapshot { return p.snap }
func (p *s53cHealthProvider) RunCanaryNow()                   {}
func (p *s53cHealthProvider) CurrentHealthSettings() config.HealthSettings {
	return config.HealthSettings{}
}
func (p *s53cHealthProvider) ApplyHealthSettings(_ config.HealthSettings) error { return nil }

// ---------------------------------------------------------------------------
// Region inventory and order (v3 §A/§B, owner ruling B)
// ---------------------------------------------------------------------------

// TestS5_3COverviewHasExactlyOneH1 pins the single page heading.
func TestS5_3COverviewHasExactlyOneH1(t *testing.T) {
	body := s53cPage(t, "en")
	if n := strings.Count(body, "<h1"); n != 1 {
		t.Errorf("/overview renders %d <h1> elements, want exactly 1", n)
	}
}

// TestS5_3CRegionOrder pins the frozen DOM order:
// lifecycle -> exactly two slots -> health aggregate -> predictions summary ->
// (version) -> full manual prediction board (secondary).
// Up Next and the queue preview are absent by evidence verdict, so they simply
// do not appear between health and the predictions summary.
func TestS5_3CRegionOrder(t *testing.T) {
	body := s53cPage(t, "en")

	ordered := []string{
		`id="lifecycle-panel"`,
		"data-ov-slots",
		"data-ov-health-summary",
		"data-ov-pred-kpi",
		"data-ov-pred-board",
	}
	prev := -1
	prevName := ""
	for _, marker := range ordered {
		at := s53cMarkerAt(t, body, marker)
		if at <= prev {
			t.Errorf("region %q renders at %d, before %q at %d — frozen order violated", marker, at, prevName, prev)
		}
		prev, prevName = at, marker
	}
}

// TestS5_3CNoOtherTopLevelRegions pins the region inventory to a CLOSED
// vocabulary. Any region that returns to /overview — the roster grid, its
// filter/sort toolbar, a per-subsystem health row, a fabricated Up Next, a
// roster-count line dressed up as the queue preview — renders its own
// data-ov-* attribute, which is immediately outside the vocabulary and fails
// here. Every unconditional marker must also actually be present, so the check
// can never pass by rendering nothing at all.
func TestS5_3CNoOtherTopLevelRegions(t *testing.T) {
	body := s53cPage(t, "en")

	// Rendered on every request, whatever the evidence says. There is no
	// status/summary strip in this vocabulary: the miner's own state is the
	// lifecycle panel's, and Total points / +Today / live-count are not part of
	// the frozen composition at all.
	always := []string{
		"data-ov-slots",
		"data-ov-health-summary",
		"data-ov-health-state",
		"data-ov-pred-kpi",
		"data-ov-pred-active",
		"data-ov-pred-today",
		"data-ov-pred-winrate",
		"data-ov-pred-board",
	}
	// Rendered only when its evidence exists. ALLOWED here rather than
	// required; its gate is pinned by TestS5_3CVersionRegionIsEvidenceGated.
	conditional := []string{
		"data-ov-version",
	}

	allowed := map[string]bool{}
	for _, m := range append(append([]string{}, always...), conditional...) {
		allowed[m] = true
	}

	got := s53cDataOvAttrs(body)
	for _, m := range got {
		if !allowed[m] {
			t.Errorf("/overview renders region marker %q, which is outside the canonical composition", m)
		}
	}
	for _, m := range always {
		if !strings.Contains(body, m) {
			t.Errorf("/overview is missing unconditional region marker %q", m)
		}
	}

	// The claims preview and the updater echo are not conditional regions of
	// this page — they are not regions of it at all. The closed vocabulary above
	// already rejects them; naming them gives the failure a reason rather than
	// just "outside the composition", since both were once allowed here.
	for _, gone := range []string{"data-ov-claims", "data-ov-update-state"} {
		if strings.Contains(body, gone) {
			t.Errorf("/overview renders %q: /drops owns claim history and /system/diagnostics owns the updater verdict", gone)
		}
	}
}

// ---------------------------------------------------------------------------
// Regions that must be absent (v3 §1 "НЕ ДЛЯ", owner rulings C/D/H)
// ---------------------------------------------------------------------------

// Host resources, the roster grid and its filter/sort toolbar each already have
// a single focused owner elsewhere in this package, non-vacuously proven
// against their real owner page: TestResourceWidgetsAbsentFromOverview +
// TestHostResourcesStillOwnedBySystemStatus (resources_widget_render_test.go),
// TestOverviewRendersNoStreamerCards + TestOverviewRendersNoRosterGrid and
// TestOverviewFreshnessBadgeSurvivesToolbarRemoval (overview_delta_test.go).
// They are deliberately not re-banned here.

// TestS5_3CNoQuickActions covers the one roster surface those owners do not:
// the per-row mutation controls. /overview has no roster, so it offers no
// streamer-level actions at all — /overview/queue owns both. Non-vacuous: the
// fixture's streamers are proven to still render on the queue page.
func TestS5_3CNoQuickActions(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	body := s53cPageFor(t, srv, "en")

	for _, banned := range []string{
		"data-card-streamer",
		`data-action="cycle-preference"`, `data-action="toggle-watch"`,
		"/api/streamer-action/",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("/overview still renders per-streamer quick action %q", banned)
		}
	}

	rec := httptest.NewRecorder()
	srv.handleOverviewQueuePage(rec, httptest.NewRequest(http.MethodGet, "/overview/queue", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /overview/queue = %d, want 200", rec.Code)
	}
	queue := rec.Body.String()
	for _, name := range []string{"shroud", "summit"} {
		if !strings.Contains(queue, name) {
			t.Fatalf("precondition failed: /overview/queue does not list %q, so the Overview roster ban is vacuous", name)
		}
	}
}

// TestS5_3CNoCommunityGoalsTicker: the community-goals ticker is gone from
// /overview. Non-vacuity uses the public ActiveCommunityGoals evidence — a
// streamer with a live goal is on the page's streamer list, so a ticker region
// would have content to render if it still existed.
func TestS5_3CNoCommunityGoalsTicker(t *testing.T) {
	srv, online, _ := newOverviewTestServer(t)
	online.AddCommunityGoal(&models.CommunityGoal{
		GoalID: "g1", Title: "New Emote", Status: models.CommunityGoalStarted,
		IsInStock: true, PointsContributed: 72, GoalAmount: 100,
	})
	if len(online.ActiveCommunityGoals()) == 0 {
		t.Fatal("precondition failed: fixture streamer has no active community goal, so the ticker ban is vacuous")
	}

	body := s53cPageFor(t, srv, "en")
	for _, banned := range []string{`class="ticker`, "ticker-item", "ticker-track", "ticker-sep"} {
		if strings.Contains(body, banned) {
			t.Errorf("/overview still renders the community-goals ticker (%q)", banned)
		}
	}
	if strings.Contains(body, "New Emote") {
		t.Error("/overview still renders community-goal content")
	}
}

// The canonical System links, and the absence of the legacy /health link, are
// owned by TestOverviewHealthChip in overview_delta_test.go — which also proves
// /health is still served, so that ban is about the LINK moving, never about
// the route disappearing. Not re-banned here.

// Up Next has no dedicated negative test: the evidence audit found no Up Next
// concept anywhere in the product, so there is nothing that could make one
// appear and nothing a focused test could non-vacuously prove. Its exclusion is
// carried by TestS5_3CNoOtherTopLevelRegions' closed vocabulary, which fails
// the moment any region marker outside the frozen inventory renders.

// TestS5_3CNoStatusStripOrRosterCountSubstitute pins the two surfaces the
// frozen composition does NOT contain: the old top-level status/summary strip
// (Total points / +Today / live-count / refresh) and any roster-count line
// dressed up as the queue preview. No ordered queue evidence exists, so the
// queue preview itself is S-NOBACK.
//
// Non-vacuity is established from the OTHER side, on rendered pages: the
// fixture holds two real streamers, one of them confirmed live and holding a
// watch slot, both carrying real non-zero points. /overview/queue is proven to
// render that roster, and /overview is proven to render the live one in its
// watch pair. So the counts a "1/2" substitute line would have shown are real,
// present and non-zero on this very request — their absence below is a
// composition decision, not an empty fixture.
func TestS5_3CNoStatusStripOrRosterCountSubstitute(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)

	rec := httptest.NewRecorder()
	srv.handleOverviewQueuePage(rec, httptest.NewRequest(http.MethodGet, "/overview/queue", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /overview/queue = %d, want 200", rec.Code)
	}
	// The roster owner renders one row per streamer per surface (card list +
	// table), so distinct channels — not row count — is what states the roster.
	rowRe := regexp.MustCompile(`data-qr-row data-qr-channel="([^"]+)" data-qr-status="([^"]+)" data-qr-points="([0-9]+)"`)
	roster := map[string]string{}
	watching, withPoints := 0, 0
	for _, m := range rowRe.FindAllStringSubmatch(rec.Body.String(), -1) {
		roster[m[1]] = m[2]
		if m[2] == "watching" {
			watching++
		}
		if pts, err := strconv.Atoi(m[3]); err == nil && pts > 0 {
			withPoints++
		}
	}
	// Two real streamers, one of them live, both with non-zero points: every
	// figure the deleted strip used to show is genuinely available on this
	// request, so each ban below is a composition decision, not an empty fixture.
	if len(roster) != 2 || watching == 0 || withPoints == 0 {
		t.Fatalf("precondition failed: /overview/queue reports %d streamers, %d watching rows, %d rows with non-zero points (want 2, >0, >0) — the bans below would be vacuous; roster=%v", len(roster), watching, withPoints, roster)
	}

	body := s53cPageFor(t, srv, "en")
	if !strings.Contains(body, "shroud") {
		t.Fatal("precondition failed: the live fixture streamer does not reach /overview at all, so the count bans are vacuous")
	}

	// The strip itself, each of its three figures, and its refresh control.
	// "1/2" is exactly what LiveCount/StreamerCount renders for this fixture.
	for _, banned := range []string{
		"data-ov-status",
		`title="Total channel points"`, ">total points<",
		">1/2<", ">live</div>",
		`title="Refresh now"`,
	} {
		if strings.Contains(body, banned) {
			t.Errorf("/overview still renders status-strip surface %q — it is not part of the frozen composition", banned)
		}
	}

	// ...and no region may reintroduce those counts under a queue-preview name.
	for _, banned := range []string{
		"data-ov-queue-preview", "data-ov-roster-summary", "data-ov-roster-total", "data-ov-roster-live",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("/overview renders %q — a roster count is not the frozen queue preview", banned)
		}
	}
}

// TestS5_3CNoPerSubsystemHealthRows: /overview carries ONE aggregate value.
// Non-vacuous: the per-subsystem rows are proven to still render on their
// owner page, /system/status.
func TestS5_3CNoPerSubsystemHealthRows(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	srv.SetHealthProvider(&s53cHealthProvider{snap: health.Snapshot{Signals: []health.Signal{
		{Name: health.SignalOAuth, Status: health.StatusOK},
		{Name: health.SignalGQLAPI, Status: health.StatusOK},
		{Name: health.SignalPubSub, Status: health.StatusOK},
		{Name: health.SignalDropsInventory, Status: health.StatusOK},
	}}})
	body := s53cPageFor(t, srv, "en")

	for _, banned := range []string{"data-ov-health-row", "health-card-row-label", "OAuth", "GQL", "PubSub", "Drops Inventory Sync"} {
		if strings.Contains(body, banned) {
			t.Errorf("/overview renders per-subsystem health detail %q — only the aggregate belongs here", banned)
		}
	}

	rec := httptest.NewRecorder()
	newRenderServer(t).handleSystemStatusPage(rec, httptest.NewRequest(http.MethodGet, "/system/status", nil))
	if !strings.Contains(rec.Body.String(), "OAuth") {
		t.Fatal("precondition failed: /system/status does not render subsystem rows, so the Overview ban is vacuous")
	}
}

// ---------------------------------------------------------------------------
// Health aggregate (owner ruling A, v3 checklist item 4)
// ---------------------------------------------------------------------------

// TestS5_3CCompactAggregateHealth: exactly one aggregate value, drawn from the
// {healthy, degraded, unhealthy, unknown} vocabulary, linking to /system/status.
// A failed signal is UNHEALTHY on the page, not merely degraded: the page shows
// aggregateHealth's verdict as it is, and the chip's CSS class is derived from
// that same value so a rendered class can never disagree with the state.
func TestS5_3CCompactAggregateHealth(t *testing.T) {
	for _, tc := range []struct {
		name    string
		signals []health.Signal
		want    string
	}{
		{"all ok", []health.Signal{{Name: health.SignalOAuth, Status: health.StatusOK}}, "healthy"},
		{"one degraded", []health.Signal{
			{Name: health.SignalOAuth, Status: health.StatusOK},
			{Name: health.SignalPubSub, Status: health.StatusDegraded},
		}, "degraded"},
		{"one failed stays unhealthy", []health.Signal{
			{Name: health.SignalOAuth, Status: health.StatusFailed},
		}, "unhealthy"},
		{"one unknown", []health.Signal{
			{Name: health.SignalOAuth, Status: health.StatusOK},
			{Name: health.SignalPubSub, Status: health.StatusUnknown},
		}, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newOverviewTestServer(t)
			srv.SetHealthProvider(&s53cHealthProvider{snap: health.Snapshot{Signals: tc.signals}})
			body := s53cPageFor(t, srv, "en")

			if n := strings.Count(body, "data-ov-health-state="); n != 1 {
				t.Fatalf("/overview renders %d aggregate health values, want exactly 1", n)
			}
			want := `data-ov-health-state="` + tc.want + `"`
			if !strings.Contains(body, want) {
				t.Errorf("aggregate health is not %s; body does not contain %q", tc.want, want)
			}
			// The class is derived from the SAME state, so the two are pinned
			// together: a mapping that softened the state would have to soften
			// the class with it, and both assertions would fail at once.
			if wantClass := `class="ov-health ov-health-` + tc.want + `"`; !strings.Contains(body, wantClass) {
				t.Errorf("health chip class is not derived from state %q; body does not contain %q", tc.want, wantClass)
			}
			// The chip carries readable, localized text — never colour alone
			// (I18) and never the untranslated key.
			label := s53T(t, "en", "ov.health."+tc.want)
			if label == "" || label == "ov.health."+tc.want {
				t.Fatalf("health state %q has no localized label, so the chip would render a raw key", tc.want)
			}
			if !strings.Contains(body, ">"+label+"<") {
				t.Errorf("health chip does not render the localized label %q for state %q", label, tc.want)
			}
			if !strings.Contains(body, `href="/system/status"`) {
				t.Error("aggregate health must link to its canonical owner /system/status")
			}
		})
	}
}

// TestS5_3CUnknownHealthNeverRendersHealthy pins the honesty invariant across
// every path that produces an unknown verdict: an explicitly unknown signal, an
// empty snapshot, and no health provider at all.
func TestS5_3CUnknownHealthNeverRendersHealthy(t *testing.T) {
	tr := enTR(t)
	healthyLabel := tr("ov.health.healthy")

	for _, tc := range []struct {
		name     string
		provider HealthProvider
	}{
		{"explicit unknown signal", &s53cHealthProvider{snap: health.Snapshot{Signals: []health.Signal{
			{Name: health.SignalPubSub, Status: health.StatusUnknown},
		}}}},
		{"empty snapshot", &s53cHealthProvider{snap: health.Snapshot{}}},
		{"no provider", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newOverviewTestServer(t)
			if tc.provider != nil {
				srv.SetHealthProvider(tc.provider)
			}
			body := s53cPageFor(t, srv, "en")

			if !strings.Contains(body, `data-ov-health-state="unknown"`) {
				t.Errorf("unknown health did not render as unknown")
			}
			if strings.Contains(body, `data-ov-health-state="healthy"`) {
				t.Error("unknown health rendered as healthy")
			}
			if strings.Contains(body, ">"+healthyLabel+"<") {
				t.Errorf("unknown health rendered the healthy label %q", healthyLabel)
			}
		})
	}
}

// TestS5_3CEveryHealthVerdictIsLocalized closes the honesty invariant at the
// seam rather than at one language's render: buildOverviewHealth labels a chip
// with tr("ov.health."+state), so a verdict aggregateHealth can emit but a
// catalog is missing renders the RAW KEY — a chip reading "ov.health.unhealthy"
// to the user. Every verdict is exercised through buildOverviewHealth itself
// (not a hand-written list of strings), in BOTH languages, so adding a fifth
// verdict without its locale entries fails here instead of in production.
func TestS5_3CEveryHealthVerdictIsLocalized(t *testing.T) {
	// Real inputs, one per verdict aggregateHealth can reach.
	verdicts := []struct {
		name           string
		providerAbsent bool
		signals        []health.Signal
		connectionLost bool
		want           string
	}{
		{name: "healthy", signals: []health.Signal{{Name: health.SignalOAuth, Status: health.StatusOK}}, want: "healthy"},
		{name: "degraded", signals: []health.Signal{{Name: health.SignalOAuth, Status: health.StatusDegraded}}, want: "degraded"},
		{name: "unhealthy", signals: []health.Signal{{Name: health.SignalOAuth, Status: health.StatusFailed}}, want: "unhealthy"},
		{name: "unhealthy via connection lost", signals: []health.Signal{{Name: health.SignalOAuth, Status: health.StatusOK}}, connectionLost: true, want: "unhealthy"},
		{name: "unknown", providerAbsent: true, want: "unknown"},
	}

	seen := map[string]bool{}
	for _, lang := range []string{"en", "ru"} {
		for _, v := range verdicts {
			t.Run(lang+"/"+v.name, func(t *testing.T) {
				tr := func(key string) string { return s53T(t, lang, key) }
				view := buildOverviewHealth(tr, !v.providerAbsent, health.Snapshot{Signals: v.signals}, v.connectionLost)

				if view.State != v.want {
					t.Fatalf("state = %q, want %q", view.State, v.want)
				}
				seen[view.State] = true

				// The label is REAL translated text: not empty, and not the key
				// itself, which is what i18n.T falls back to when neither the
				// requested catalog nor the default one holds the entry.
				if view.Label == "" || view.Label == "ov.health."+view.State {
					t.Errorf("[%s] verdict %q renders label %q — the catalog is missing ov.health.%s, so the chip shows a raw key", lang, view.State, view.Label, view.State)
				}
			})
		}
	}

	// Non-vacuity: the table really did exercise all four verdicts, so a future
	// edit that drops one cannot quietly shrink this test's coverage.
	for _, want := range []string{"healthy", "degraded", "unhealthy", "unknown"} {
		if !seen[want] {
			t.Errorf("no case in this test produced the %q verdict", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Predictions (v3 checklist items 5 and 6)
// ---------------------------------------------------------------------------

// TestS5_3CCompactPredictionsKPI: the KPI is a primary region carrying the
// frozen three figures, with a pointer to the ROI owner.
func TestS5_3CCompactPredictionsKPI(t *testing.T) {
	body := s53cPage(t, "en")

	s53cMarkerAt(t, body, "data-ov-pred-kpi")
	// The fixture's board is reachable and holds exactly one round, so the
	// active figure is proven evidence, not a guess.
	if !strings.Contains(body, `data-ov-pred-active="1"`) {
		t.Error("predictions KPI does not report the proven active-round count")
	}
	// No bet-history evidence exists on this request, so both remaining figures
	// stay honestly unknown rather than being fabricated.
	for _, unknown := range []string{`data-ov-pred-today="unknown"`, `data-ov-pred-winrate="unknown"`} {
		if !strings.Contains(body, unknown) {
			t.Errorf("predictions KPI missing honest unknown marker %q", unknown)
		}
	}
	if !strings.Contains(body, `href="/statistics"`) {
		t.Error("predictions KPI must point at the ROI owner (/statistics)")
	}
}

// TestS5_3CPredictionsKPIUnknownNeverBecomesZero: with no provider wired the
// board is unreachable, so the active count is UNKNOWN. It must render a dash,
// never a 0 that would read as positive evidence of "no rounds".
func TestS5_3CPredictionsKPIUnknownNeverBecomesZero(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	srv.SetOverviewProvider(nil)
	body := s53cPageFor(t, srv, "en")

	if !strings.Contains(body, `data-ov-pred-active="unknown"`) {
		t.Fatal("an unreachable predictions board must report an unknown active count")
	}
	if strings.Contains(body, `data-ov-pred-active="0"`) {
		t.Error("unknown active-round count rendered as 0")
	}
	if !strings.Contains(body, "—") {
		t.Error("unknown figures must render as an em dash")
	}
}

// s53cUnknownKPIValue extracts the full rendered element for ONE unknown
// predictions figure — the data-ov-pred-<name>="unknown" span through its own
// matching closing tag — by walking that element's <span>/</span> nesting. Its
// accessibility assertions are then made against that element alone, so no
// similar-looking markup elsewhere on the page (another figure, another dash)
// can satisfy them, and no child ordering is baked in.
func s53cUnknownKPIValue(t *testing.T, body, attr string) string {
	t.Helper()
	open := `<span class="num" data-ov-pred-` + attr + `="unknown">`
	at := strings.Index(body, open)
	if at < 0 {
		t.Fatalf("no unknown %s figure rendered (looked for %s); body=%s", attr, open, body)
	}

	depth, i := 1, at+len(open)
	for depth > 0 {
		nested := strings.Index(body[i:], "<span")
		closed := strings.Index(body[i:], "</span>")
		if closed < 0 {
			t.Fatalf("the unknown %s figure element is never closed; body=%s", attr, body[at:])
		}
		if nested >= 0 && nested < closed {
			depth++
			i += nested + len("<span")
			continue
		}
		depth--
		i += closed + len("</span>")
	}
	return body[at:i]
}

// TestS5_3CUnknownKPIValuesExposeNoDataToAssistiveTech: an unknown figure shows
// a dash to sighted readers, but a dash is not an accessible NAME — a bare span
// carries no ARIA role, so an aria-label on it is not reliably announced and the
// only text content ("—") announces as "dash" or as nothing at all. Each unknown
// figure therefore hides its dash from assistive technology and carries the
// translated "no data" wording as real text, in BOTH languages.
func TestS5_3CUnknownKPIValuesExposeNoDataToAssistiveTech(t *testing.T) {
	for _, lang := range []string{"en", "ru"} {
		t.Run(lang, func(t *testing.T) {
			srv, _, _ := newOverviewTestServer(t)
			srv.SetOverviewProvider(nil) // unreachable board: all three figures unknown
			body := s53cPageFor(t, srv, lang)
			noData := s53T(t, lang, "queue.slot.no_data")
			if noData == "" || noData == "queue.slot.no_data" {
				t.Fatalf("[%s] queue.slot.no_data must resolve to real text, got %q", lang, noData)
			}

			for _, attr := range []string{"active", "today", "winrate"} {
				el := s53cUnknownKPIValue(t, body, attr)

				// The dash stays VISIBLE — this is not a "drop the dash" fix.
				if !strings.Contains(el, "—") {
					t.Errorf("[%s] unknown %s figure no longer shows the em dash; el=%s", lang, attr, el)
				}
				// ...but it is hidden from assistive technology.
				if !strings.Contains(el, `<span aria-hidden="true">—</span>`) {
					t.Errorf("[%s] unknown %s figure must hide its em dash from assistive technology; el=%s", lang, attr, el)
				}
				// ...and the meaning is exposed as real, translated text rather
				// than as an aria-label on a role-less span.
				if !strings.Contains(el, `<span class="visually-hidden">`+noData+`</span>`) {
					t.Errorf("[%s] unknown %s figure must expose the translated %q meaning as visually-hidden text; el=%s", lang, attr, noData, el)
				}
				if strings.Contains(el, "aria-label") {
					t.Errorf("[%s] unknown %s figure still names itself with an aria-label on a bare span; el=%s", lang, attr, el)
				}
			}
		})
	}
}

// TestS5_3CPredictionsKPIProvenZeroStaysZero is the other half of the numeric
// contract: a reachable board with no rounds is a PROVEN zero and must render 0.
func TestS5_3CPredictionsKPIProvenZeroStaysZero(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)
	srv.SetOverviewProvider(&fakeOverviewProvider{
		slots: WatchSlotsView{Watching: map[string]bool{}},
	})
	body := s53cPageFor(t, srv, "en")

	if !strings.Contains(body, `data-ov-pred-active="0"`) {
		t.Error("a reachable board with zero rounds must render a proven 0, not a dash")
	}
}

// TestS5_3CFullPredictionBoardIsCollapsedSecondaryDisclosure: the legacy manual
// board is preserved IN FULL, on the same route, but demoted to a disclosure
// that is closed by default and rendered after every primary region.
func TestS5_3CFullPredictionBoardIsCollapsedSecondaryDisclosure(t *testing.T) {
	body := s53cPage(t, "en")

	at := s53cMarkerAt(t, body, "data-ov-pred-board")
	start := strings.LastIndex(body[:at], "<details")
	if start < 0 {
		t.Fatal("the full prediction board is not inside a <details> disclosure")
	}
	// The WHOLE opening tag, through its closing ">" — an "open" attribute
	// written after the data-ov-pred-board marker is just as expanded as one
	// written before it.
	end := strings.Index(body[start:], ">")
	if end < 0 {
		t.Fatal("the prediction board's <details> tag is never closed")
	}
	tag := body[start : start+end+1]
	if strings.Contains(tag, " open") {
		t.Errorf("the full prediction board must be COLLAPSED by default; tag=%s", tag)
	}
	if at < s53cMarkerAt(t, body, "data-ov-pred-kpi") {
		t.Error("the secondary board must render after the primary predictions KPI")
	}

	// The whole existing feature survives: the board, the manual-bet control,
	// the place action and the auto-bet skip toggle.
	for _, want := range []string{`id="predictions"`, "pred-board", "data-manual", "data-place", "data-skip"} {
		if !strings.Contains(body, want) {
			t.Errorf("collapsing the board dropped existing manual-bet surface %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Evidence-gated region: version
// ---------------------------------------------------------------------------

// TestS5_3CNoClaimsPreview: /overview renders no claims preview at all. A claim
// the Overview could show is one event pulled out of the in-memory ring — no
// history, no totals — and /drops owns that surface.
//
// The ban is on the claims REGION's own markers and on its retired locale keys,
// never on `href="/drops/claims"`: the sidebar nav links there on every page,
// and banning it would fail on a link that is entirely correct.
func TestS5_3CNoClaimsPreview(t *testing.T) {
	body := s53cPage(t, "en")

	// Non-vacuity: the page really did render its canonical regions, so the
	// bans below are asserted against a real Overview and not an empty body.
	for _, present := range []string{"data-ov-health-summary", "data-ov-pred-kpi", "data-ov-pred-board"} {
		if !strings.Contains(body, present) {
			t.Fatalf("precondition failed: /overview did not render %q, so the claim bans are vacuous", present)
		}
	}

	for _, gone := range []string{"data-ov-claims", "ov-claims-heading", "ov.claims"} {
		if strings.Contains(body, gone) {
			t.Errorf("/overview still renders claim surface %q", gone)
		}
	}

	// The retired keys are gone from BOTH catalogs, so neither language can
	// resurrect the region's wording — and neither can be left as an orphan.
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	for _, lang := range []string{i18n.LangEN, i18n.LangRU} {
		for _, key := range []string{"ov.claims.heading", "ov.claims.history_link"} {
			if got := loc.T(lang, key); got != key {
				t.Errorf("[%s] retired claims key %q still resolves to %q; it must be removed from the catalog", lang, key, got)
			}
		}
	}
}

// TestS5_3CVersionRegionIsEvidenceGated: the version line renders only when a
// version value actually exists, and carries no updater verdict of its own —
// /system/diagnostics owns that signal, and the Overview links to it.
func TestS5_3CVersionRegionIsEvidenceGated(t *testing.T) {
	t.Run("absent without a version value", func(t *testing.T) {
		body := s53cRenderLive(t, OverviewPageData{})
		if strings.Contains(body, "data-ov-version") {
			t.Error("the version region rendered with an empty version value")
		}
	})

	t.Run("present with a version value", func(t *testing.T) {
		body := s53cRenderLive(t, OverviewPageData{OverviewData: OverviewData{Version: "0.29.0"}})
		if !strings.Contains(body, "data-ov-version") {
			t.Fatal("the version region did not render despite a real version value")
		}
		if !strings.Contains(body, "0.29.0") {
			t.Error("version region does not render the version value")
		}
		if !strings.Contains(body, `href="/system/diagnostics"`) {
			t.Error("version region must link to its owner /system/diagnostics")
		}
		// The updater's verdict has exactly one owner, and it is not this page.
		if strings.Contains(body, "data-ov-update-state") {
			t.Error("the version region carries an update-status line; /system/diagnostics owns the updater verdict")
		}
	})
}

// ---------------------------------------------------------------------------
// Lifecycle + slots (v3 checklist items 2 and 3)
// ---------------------------------------------------------------------------

// TestS5_3CLifecycleAndExactlyTwoSlotsRemain: the lifecycle panel is the page's
// first region and its sole owner of mutation controls, and the watch pair is
// always exactly two boxes.
func TestS5_3CLifecycleAndExactlyTwoSlotsRemain(t *testing.T) {
	body := s53cPage(t, "en")

	if !strings.Contains(body, `id="lifecycle-panel"`) {
		t.Fatal("/overview lost the lifecycle panel")
	}
	// Overview never grows a second, competing lifecycle control surface.
	for _, banned := range []string{"/api/lifecycle/pause", "/api/lifecycle/stop", "/api/lifecycle/restart"} {
		if strings.Contains(body, banned) {
			t.Errorf("/overview renders its own lifecycle control %q — the panel partial is the sole owner", banned)
		}
	}

	s53cMarkerAt(t, body, "data-ov-slots")
	if !strings.Contains(body, `href="/overview/queue"`) {
		t.Error("the watch slots must link to their full owner /overview/queue")
	}

	// Counted on the partial, which is the Overview's own surface: the pinned
	// sidebar renders its own, separate pair through the same component and
	// must not be conflated with the page's.
	live := s53cRenderLive(t, sampleOverview())
	boxes := strings.Count(live, "c12-slot--occupied") + strings.Count(live, "c12-slot--empty")
	if boxes != 2 {
		t.Errorf("the Overview watch pair renders %d slot boxes, want exactly 2", boxes)
	}
}

// TestS5_3CLifecycleDangerousActionsBehindDisclosure pins the frozen action
// placement on the RENDERED /api/lifecycle panel — the exact bytes the browser
// receives — not on the embedded template source.
//
// Frozen v3: Pause is the only primarily-visible action; Restart and Stop live
// only inside the More-actions disclosure, both keeping their confirmation.
func TestS5_3CLifecycleDangerousActionsBehindDisclosure(t *testing.T) {
	// A plain running miner: every one of the three controls is semantically
	// available, so each of them genuinely renders and placement is the only
	// thing under test.
	snap := lifecycle.Snapshot{
		Desired: lifecycle.DesiredRunning, Observed: lifecycle.ObservedRunning,
		Capabilities: lifecycle.Capabilities{CanPause: true, CanRestart: true, CanStop: true},
	}
	for _, lang := range []string{"en", "ru"} {
		body := lifecyclePanelHTMX(t, snap, lang)

		disclosure := strings.Index(body, `id="lc-advanced"`)
		if disclosure < 0 {
			t.Fatalf("[%s] the lifecycle panel lost its More-actions disclosure", lang)
		}
		primary, advanced := body[:disclosure], body[disclosure:]

		if !strings.Contains(primary, `hx-post="/api/lifecycle/pause"`) {
			t.Errorf("[%s] Pause must stay the primary, always-visible action; body=%s", lang, body)
		}
		for _, dangerous := range []string{"/api/lifecycle/restart", "/api/lifecycle/stop"} {
			if strings.Contains(primary, `hx-post="`+dangerous+`"`) {
				t.Errorf("[%s] %s renders as a visible peer of Pause; it belongs inside the More-actions disclosure", lang, dangerous)
			}
			if !strings.Contains(advanced, `hx-post="`+dangerous+`"`) {
				t.Errorf("[%s] %s is not inside the More-actions disclosure at all; body=%s", lang, dangerous, body)
			}
		}

		// Both dangerous actions keep their confirmation once disclosed. The
		// check is scoped to each action's own rendered <button> tag, so one
		// button's hx-confirm can never stand in for the other's.
		for _, dangerous := range []string{"restart", "stop"} {
			re := regexp.MustCompile(`<button[^>]*hx-post="/api/lifecycle/` + dangerous + `"[^>]*>`)
			tag := re.FindString(advanced)
			if tag == "" {
				t.Errorf("[%s] no rendered %s <button> inside the More-actions disclosure", lang, dangerous)
				continue
			}
			if !strings.Contains(tag, "hx-confirm=") {
				t.Errorf("[%s] the disclosed %s action lost its confirmation; tag=%s", lang, dangerous, tag)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// P1: deterministic structural horizontal-overflow guard
// ---------------------------------------------------------------------------

// s53cOverflowMinPx is the narrowest required viewport (390px). A box pinned at
// least this wide, or a layout that cannot reflow below it, is what actually
// produces page-level horizontal overflow on mobile.
const s53cOverflowMinPx = 390

type s53cOverflow struct {
	Rule    string
	Snippet string
}

var (
	// Widths are read from rendered style="..." ATTRIBUTES only. A width inside
	// a stylesheet's @media condition is a breakpoint, not a box, and flagging
	// those would make the guard noise instead of signal.
	s53cStyleAttrRe   = regexp.MustCompile(`style="([^"]*)"`)
	s53cInlineWidthRe = regexp.MustCompile(`(?i)(min-width|width)\s*:\s*(\d+(?:\.\d+)?)\s*(px|rem|em)`)
	s53cArbWidthRe    = regexp.MustCompile(`(?:min-)?w-\[(\d+(?:\.\d+)?)(px|rem)\]`)
	s53cGridColsRe    = regexp.MustCompile(`(^|[\s"'])grid-cols-(\d+)`)
	s53cIntrinsicRe   = regexp.MustCompile(`(^|[\s"'])(min-w-max|w-max|w-screen|min-w-screen)([\s"']|$)`)
	s53cViewportRe    = regexp.MustCompile(`(?i)(min-)?width\s*:\s*(\d+(?:\.\d+)?)vw`)
)

// s53cScanOverflow reports rendered constructs that are likely to reintroduce
// page-level horizontal overflow. It is a dependency-free CI proxy for the
// browser measurement, not a replacement for it.
func s53cScanOverflow(html string) []s53cOverflow {
	var out []s53cOverflow
	add := func(rule, snippet string) {
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		out = append(out, s53cOverflow{Rule: rule, Snippet: snippet})
	}

	// 1. A box pinned to an absolute width at least as wide as the narrowest
	//    viewport cannot shrink with the page.
	styles := make([]string, 0, 8)
	for _, sm := range s53cStyleAttrRe.FindAllStringSubmatch(html, -1) {
		styles = append(styles, sm[1])
	}
	for _, style := range styles {
		for _, m := range s53cInlineWidthRe.FindAllStringSubmatch(style, -1) {
			px, err := strconv.ParseFloat(m[2], 64)
			if err != nil {
				continue
			}
			if strings.EqualFold(m[3], "rem") || strings.EqualFold(m[3], "em") {
				px *= 16
			}
			if px >= s53cOverflowMinPx {
				add("fixed-width", m[0])
			}
		}
	}
	for _, m := range s53cArbWidthRe.FindAllStringSubmatch(html, -1) {
		px, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		if m[2] == "rem" {
			px *= 16
		}
		if px >= s53cOverflowMinPx {
			add("fixed-width", m[0])
		}
	}

	// 2. A multi-column grid with no responsive prefix keeps all its columns at
	//    every viewport, including the narrowest one.
	for _, m := range s53cGridColsRe.FindAllStringSubmatch(html, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil || n < 2 {
			continue
		}
		add("unconditional-multicolumn", strings.TrimSpace(m[0]))
	}

	// 3. Content-sized / viewport-sized boxes ignore their container's width.
	for _, m := range s53cIntrinsicRe.FindAllStringSubmatch(html, -1) {
		add("intrinsic-width", strings.TrimSpace(m[0]))
	}

	// 4. A width expressed as more than a full viewport is overflow by
	//    definition.
	for _, style := range styles {
		for _, m := range s53cViewportRe.FindAllStringSubmatch(style, -1) {
			vw, err := strconv.ParseFloat(m[2], 64)
			if err == nil && vw > 100 {
				add("over-viewport-width", m[0])
			}
		}
	}

	// 5. /overview is not the owner of any tabular surface; a table is the
	//    classic un-shrinkable block on this page.
	if strings.Contains(html, "<table") {
		add("table-on-overview", "<table")
	}
	return out
}

// TestS5_3CScanOverflowDetectsKnownOffenders proves the guard is not vacuous:
// every rule must actually fire on a construct that exhibits it, and clean
// responsive markup must not trip any of them.
func TestS5_3CScanOverflowDetectsKnownOffenders(t *testing.T) {
	for _, tc := range []struct {
		name, html, rule string
	}{
		{"inline px width", `<div style="min-width:640px">x</div>`, "fixed-width"},
		{"arbitrary tailwind width", `<div class="min-w-[720px]">x</div>`, "fixed-width"},
		{"unprefixed grid", `<div class="grid grid-cols-4 gap-2">x</div>`, "unconditional-multicolumn"},
		{"intrinsic width", `<div class="min-w-max">x</div>`, "intrinsic-width"},
		{"over-viewport width", `<div style="width:140vw">x</div>`, "over-viewport-width"},
		{"table", `<table><tr><td>x</td></tr></table>`, "table-on-overview"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found := false
			for _, o := range s53cScanOverflow(tc.html) {
				if o.Rule == tc.rule {
					found = true
				}
			}
			if !found {
				t.Errorf("overflow guard failed to flag %s (rule %q)", tc.name, tc.rule)
			}
		})
	}

	clean := `<div class="grid md:grid-cols-2 gap-4"><p style="width:100%">ok</p><span class="w-full">ok</span></div>`
	if got := s53cScanOverflow(clean); len(got) != 0 {
		t.Errorf("overflow guard flagged clean responsive markup: %v", got)
	}
}

// TestS5_3CNoHorizontalOverflowConstructs runs the guard against the ACTUAL
// rendered Overview in both languages.
func TestS5_3CNoHorizontalOverflowConstructs(t *testing.T) {
	for _, lang := range []string{"en", "ru"} {
		t.Run(lang, func(t *testing.T) {
			body := s53cPage(t, lang)
			if offenders := s53cScanOverflow(body); len(offenders) > 0 {
				var b strings.Builder
				for _, o := range offenders {
					fmt.Fprintf(&b, "\n  [%s] %s", o.Rule, o.Snippet)
				}
				t.Errorf("/overview renders %d horizontal-overflow construct(s):%s", len(offenders), b.String())
			}
		})
	}
}
