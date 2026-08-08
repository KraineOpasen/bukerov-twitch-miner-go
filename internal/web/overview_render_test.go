package web

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// sampleOverview builds an OverviewPageData exercising every region the
// canonical Overview renders: the lifecycle echo, the two watch slots, the
// health aggregate, the compact predictions summary, the evidence-gated
// version region and the secondary manual board with its branch variants.
func sampleOverview() OverviewPageData {
	data := OverviewData{
		Username:       "tester",
		RefreshMinutes: 5,
		Version:        "0.29.0",
		Predictions: []PredictionView{
			{
				Streamer: "shroud", Title: "Will they win?", Status: "ACTIVE",
				SecondsLeftLabel: "0:42", WindowEndUnix: 1000, PoolLabel: "50,000",
				BetPlaced: true, BetConfirmed: true, BetAmount: "5,000",
				Outcomes: []PredictionOutcomeView{
					{Title: "Yes", Percent: 61, Odds: "1.60x", PointsLabel: "30,000", Chosen: true},
					{Title: "No", Percent: 39, Odds: "2.50x", PointsLabel: "20,000"},
				},
			},
			{Streamer: "ninja", Title: "Locked one", Status: "LOCKED", Locked: true},
		},
		GeneratedUnix: 1700000000,
	}

	return OverviewPageData{
		OverviewData:          data,
		Health:                OverviewHealthView{State: "healthy", Label: "Healthy"},
		PredictionsState:      predictionsStateActive,
		PredictionsStateLabel: "Active",
		PollSeconds:           overviewPollSeconds,
		SlotPair: [2]c12SlotData{
			{Occupied: true, Channel: "shroud", Active: true, Link: "/overview/queue"},
			{EmptyReasonMode: "idle", Link: "/overview/queue"},
		},
	}
}

func TestRenderOverviewTemplates(t *testing.T) {
	partials := testPartials(t)

	var buf bytes.Buffer
	if err := partials.ExecuteTemplate(&buf, "overview_live", sampleOverview()); err != nil {
		t.Fatalf("render overview_live: %v", err)
	}
	out := buf.String()

	// Localized partial renders the default language (RU) via testPartials.
	for _, want := range []string{
		// Every canonical region, in its rendered form.
		"data-ov-slots", "data-ov-health", "data-ov-pred-kpi",
		"data-ov-version", "data-ov-pred-board",
		// The two watch slots and their owner link.
		"shroud", `href="/overview/queue"`,
		// Health aggregate + canonical owner. The chip's class is DERIVED from
		// the same State the data attribute carries, so the two cannot drift.
		`data-ov-health-state="healthy"`, `class="ov-health ov-health-healthy"`,
		`href="/system/status"`,
		// Predictions: the count comes from Predictions itself, so the compact
		// figure and the board below can never describe different boards.
		`data-ov-pred-active="2"`, `data-ov-pred-today="unknown"`,
		`data-ov-pred-winrate="unknown"`, `href="/statistics"`,
		// Evidence-gated version region.
		"0.29.0", `href="/system/diagnostics"`,
		// The full manual board, preserved inside the collapsed disclosure.
		"Активные предикшены", "Will they win?", "Закрыто",
		"data-window-end", `id="predictions"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("overview_live output missing %q", want)
		}
	}

	// The Overview owns no claims preview and no updater verdict: /drops and
	// /system/diagnostics do. Banned here rather than only in the page-level
	// test, so a region re-added straight to the partial cannot slip through.
	for _, banned := range []string{
		"data-ov-claims", `href="/drops/claims"`, "ov.claims",
		"data-ov-update-state", "lc.update_available",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("overview_live still renders removed surface %q", banned)
		}
	}
}

// TestOverviewPredictionCardEscapesHostileText proves the escaping guarantee on
// a surface that is actually RENDERED: prediction titles and outcome names come
// straight from Twitch, so they are the hostile-input path on this page.
func TestOverviewPredictionCardEscapesHostileText(t *testing.T) {
	partials := testPartials(t)
	data := sampleOverview()
	data.Predictions[0].Title = `<script>alert(1)</script> round`
	data.Predictions[0].Outcomes[0].Title = `<img src=x onerror=alert(2)>`

	var buf bytes.Buffer
	if err := partials.ExecuteTemplate(&buf, "overview_live", data); err != nil {
		t.Fatalf("render overview_live: %v", err)
	}
	out := buf.String()
	for _, banned := range []string{"<script>alert(1)</script>", "<img src=x"} {
		if strings.Contains(out, banned) {
			t.Errorf("prediction card rendered unescaped HTML %q", banned)
		}
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Error("prediction title should render as escaped text")
	}
}

// The claim-detail escaping test that used to live here went with the claims
// preview itself: the Overview no longer renders a Twitch-supplied drop name,
// so there is no such surface left on this page to escape. The escaping
// guarantee is still covered on the surfaces that DO carry Twitch text —
// prediction titles and outcome names, above.

// TestSidebarOmitsDuplicateOverviewTabs pins the information-architecture fix:
// the «Стримеры» (/#grid) and «Предикшены» (/#predictions) sidebar tabs offered
// no destination distinct from Overview (they were in-page anchors into
// sections Overview already renders), so they were removed from the sidebar.
// The section anchors themselves must survive in the live partial so a
// bookmarked /#grid or /#predictions URL still lands on the right section (no
// redirect needed), and the generic hashchange highlighting must remain.
func TestSidebarOmitsDuplicateOverviewTabs(t *testing.T) {
	base, err := templatesFS.ReadFile("templates/base.html")
	if err != nil {
		t.Fatalf("read base.html: %v", err)
	}
	// The duplicate sidebar tabs are gone.
	for _, banned := range []string{`data-nav="#grid"`, `data-nav="#predictions"`, `href="/#grid"`, `href="/#predictions"`} {
		if strings.Contains(string(base), banned) {
			t.Errorf("base.html still contains removed duplicate sidebar tab %q", banned)
		}
	}
	// Generic hash-based active-nav highlighting stays (harmless, and still used
	// by any deep link into a section).
	if !strings.Contains(string(base), "hashchange") {
		t.Errorf("base.html missing hashchange handling")
	}

	// The section anchors themselves still exist, so a direct /#grid or
	// /#predictions URL scrolls to the right Overview section.
	partial, err := templatesFS.ReadFile("templates/partials/overview_live.html")
	if err != nil {
		t.Fatalf("read overview_live.html: %v", err)
	}
	// #grid went with the roster it anchored (/overview/queue owns that now);
	// #predictions must survive so a bookmarked /#predictions still lands on
	// the board inside its disclosure.
	if !strings.Contains(string(partial), `id="predictions"`) {
		t.Error(`overview_live.html missing section anchor id="predictions"`)
	}
	if strings.Contains(string(partial), `id="grid"`) {
		t.Error(`overview_live.html still carries the removed id="grid" roster anchor`)
	}

	// F1 theme-token consolidation moved overview.html's inline <style> block
	// (card-state visuals, stream title/tag rules, section accents) into
	// input.css, so those selectors are checked there now instead of in the
	// template; overview.html itself must no longer carry the old block.
	css, err := staticFS.ReadFile("static/css/input.css")
	if err != nil {
		t.Fatalf("read input.css: %v", err)
	}
	for _, want := range []string{".s-title", ".s-tag", "scroll-margin-top", ".sec-accent"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("input.css missing %q", want)
		}
	}

	overview, err := templatesFS.ReadFile("templates/overview.html")
	if err != nil {
		t.Fatalf("read overview.html: %v", err)
	}
	if strings.Contains(string(overview), "<style>") {
		t.Error("overview.html must no longer carry an inline <style> block — those rules now live in input.css")
	}

	// The shared semantic palette every page (and the statistics charts)
	// reads is now declared once in input.css (F1), not inline in base.html.
	for _, wantVar := range []string{"--ui-watching", "--ui-online", "--ui-gain", "--ui-roi-pos", "--ui-roi-neg", "--ui-watch", "--ui-claim", "--ui-raid", "--ui-streak"} {
		if !strings.Contains(string(css), wantVar) {
			t.Errorf("input.css missing semantic palette variable %q", wantVar)
		}
		if strings.Contains(string(base), wantVar+":") {
			t.Errorf("base.html must not define %q inline anymore — it now lives only in input.css", wantVar)
		}
	}
}

// TestRenderOverviewTemplatesEnglish renders the same fixture in English to
// prove every region localizes both ways (not just the default RU).
func TestRenderOverviewTemplatesEnglish(t *testing.T) {
	partials := testPartialsLang(t, i18n.LangEN)
	var buf bytes.Buffer
	if err := partials.ExecuteTemplate(&buf, "overview_live", sampleOverview()); err != nil {
		t.Fatalf("render overview_live (en): %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Live Predictions", "Locked", "System Status",
		"active", "today", "win rate", "ROI in Analytics",
		// The health chip's own label, which localizes through the same
		// ov.health.<state> key the restored four-state vocabulary uses.
		"Healthy",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("english overview_live missing %q", want)
		}
	}
	if strings.Contains(out, "Исправно") || strings.Contains(out, "Состояние системы") {
		t.Errorf("english render leaked Russian text")
	}
	// Two of the keys above cannot be proven by the loop alone: "ov.pred.active"
	// contains the substring "active" and "ov.today" contains "today", and the
	// rendered page carries data-ov-pred-active / data-ov-pred-today attributes
	// that contain them too. So neither an unresolved key nor a missing English
	// entry can be detected by looking for the expected English words — each has
	// to be banned in the form it would actually take:
	//
	//   - missing from BOTH catalogs -> i18n.T's last resort renders the raw key;
	//   - missing from en.json only  -> i18n.T falls back lang -> default, so the
	//     page renders the RUSSIAN text in an English render.
	//
	// The Russian value is read from the catalog rather than spelled out here, so
	// this stays correct when a translation is reworded.
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	for _, key := range []string{"ov.pred.active", "ov.today"} {
		if strings.Contains(out, key) {
			t.Errorf("english overview_live rendered the unresolved key %q instead of its translation", key)
		}
		ru := loc.T(i18n.LangRU, key)
		if ru != key && strings.Contains(out, ru) {
			t.Errorf("english overview_live rendered the Russian text %q for %q — the English catalog entry is missing and i18n.T fell back to the default language", ru, key)
		}
	}
}

func TestRenderNowWatching(t *testing.T) {
	partials := testPartials(t)
	view := NowWatchingView{
		Slots: []WatchSlotView{
			{Name: "shroud", Points: "100,000", Game: "VALORANT", HasGain: true, GainPerHour: "1,200", StreakPending: true, StreakMinutes: 5, StreakCapMinutes: 20, StreakPercent: 25},
		},
		QueuedNames:      []string{"pokimane", "ninja"},
		HasNextRotation:  true,
		NextRotationUnix: 1234567890,
	}
	var buf bytes.Buffer
	if err := partials.ExecuteTemplate(&buf, "now_watching", view); err != nil {
		t.Fatalf("render now_watching: %v", err)
	}
	out := buf.String()
	// now_watching is localized (PR 0); testPartials renders the default language (RU).
	for _, want := range []string{"shroud", "VALORANT", "1,200/h", "pokimane", "Следующая ротация", "data-countdown-to"} {
		if !strings.Contains(out, want) {
			t.Errorf("now_watching output missing %q", want)
		}
	}
}

func TestRenderNowWatchingEmpty(t *testing.T) {
	partials := testPartials(t)
	var buf bytes.Buffer
	if err := partials.ExecuteTemplate(&buf, "now_watching", NowWatchingView{Stale: true}); err != nil {
		t.Fatalf("render empty now_watching: %v", err)
	}
	if !strings.Contains(buf.String(), "Сейчас ничего не смотрим") {
		t.Error("empty now_watching should show empty-state text")
	}
}

func TestRenderEventsDrawer(t *testing.T) {
	partials := testPartials(t)
	var buf bytes.Buffer
	data := map[string]interface{}{
		"Name": "shroud",
		"Events": []struct {
			Label string
			Ago   string
		}{{Label: "Bonus claimed", Ago: "2m ago"}},
	}
	if err := partials.ExecuteTemplate(&buf, "events_drawer", data); err != nil {
		t.Fatalf("render events_drawer: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"shroud", "Bonus claimed", "2m ago", "Full page"} {
		if !strings.Contains(out, want) {
			t.Errorf("events_drawer output missing %q", want)
		}
	}
}

func TestCardTags(t *testing.T) {
	tags := func(names ...string) []models.Tag {
		out := make([]models.Tag, len(names))
		for i, n := range names {
			out[i] = models.Tag{ID: n, LocalizedName: n}
		}
		return out
	}

	if got := cardTags(nil); got != nil {
		t.Errorf("cardTags(nil) = %v, want nil", got)
	}
	if got := cardTags(tags("English", "FPS")); len(got) != 2 || got[0] != "English" {
		t.Errorf("cardTags two = %v", got)
	}
	// Capped at maxCardTags so tag-heavy channels can't overflow the card.
	if got := cardTags(tags("a", "b", "c", "d", "e")); len(got) != maxCardTags {
		t.Errorf("cardTags cap = %v, want %d entries", got, maxCardTags)
	}
	// Unnamed tags are skipped, not rendered as empty chips.
	mixed := []models.Tag{{ID: "1"}, {ID: "2", LocalizedName: "Drops"}}
	if got := cardTags(mixed); len(got) != 1 || got[0] != "Drops" {
		t.Errorf("cardTags mixed = %v, want [Drops]", got)
	}
}

func TestNextPreferenceCycle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "prefer"},
		{"prefer", "avoid"},
		{"avoid", ""},
		{"bogus", "prefer"},
	}
	for _, c := range cases {
		if got := nextPreference(c.in); got != c.want {
			t.Errorf("nextPreference(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFmtSeconds(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{{0, "0:00"}, {5, "0:05"}, {65, "1:05"}, {-3, "0:00"}, {600, "10:00"}}
	for _, c := range cases {
		if got := fmtSeconds(c.in); got != c.want {
			t.Errorf("fmtSeconds(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildPredictionViewsSortedAndMapped(t *testing.T) {
	now := time.Now()
	preds := []LivePrediction{
		{Streamer: "a", Title: "slow", Status: "ACTIVE", CreatedAt: now, PredictionWindowSeconds: 600, TotalPoints: 100,
			Outcomes: []LivePredictionOutcome{{Title: "X", PercentageUsers: 60.4, Odds: 1.6, TotalPoints: 60, Chosen: true}}},
		{Streamer: "b", Title: "fast", Status: "LOCKED", CreatedAt: now, PredictionWindowSeconds: 0},
	}
	views := buildPredictionViews(preds)
	if len(views) != 2 {
		t.Fatalf("expected 2 views, got %d", len(views))
	}
	// LOCKED/0s should sort first (soonest closing).
	if views[0].Streamer != "b" || !views[0].Locked {
		t.Errorf("expected locked 'b' first, got %+v", views[0])
	}
	var a *PredictionView
	for i := range views {
		if views[i].Streamer == "a" {
			a = &views[i]
		}
	}
	if a == nil || len(a.Outcomes) != 1 || a.Outcomes[0].Percent != 60 || a.Outcomes[0].Odds != "1.60x" || !a.Outcomes[0].Chosen {
		t.Errorf("outcome mapping wrong: %+v", a)
	}
	if a.PoolLabel != "100" {
		t.Errorf("pool label = %q, want 100", a.PoolLabel)
	}
}

func TestNetState(t *testing.T) {
	cases := []struct {
		name   string
		status StatusInfo
		want   string
	}{
		{"running clean", StatusInfo{Status: StatusRunning}, "ok"},
		{"degraded", StatusInfo{Status: StatusRunning, ConnectionDegraded: true}, "degraded"},
		{"lost wins over degraded", StatusInfo{Status: StatusRunning, ConnectionLost: true, ConnectionDegraded: true}, "lost"},
		{"not running is lost", StatusInfo{Status: StatusInitializing}, "lost"},
		{"error is lost", StatusInfo{Status: StatusError}, "lost"},
	}
	for _, c := range cases {
		if got := netState(c.status); got != c.want {
			t.Errorf("%s: netState = %q, want %q", c.name, got, c.want)
		}
	}
}

// The network indicator that used to render .NetState lived in the Overview's
// top status strip, which is not part of the frozen composition and has been
// removed; /overview has no surface for it any more. netState's own
// classification stays covered by TestNetState above.

func TestBotStatusLabel(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n: %v", err)
	}
	tr := func(k string) string { return loc.T(i18n.LangEN, k) }
	if got := botStatusLabel(tr, StatusRunning); got != "Running" {
		t.Errorf("running label = %q, want Running", got)
	}
	if got := botStatusLabel(tr, StatusAuthRequired); got != "Login required" {
		t.Errorf("auth label = %q, want Login required", got)
	}
}
