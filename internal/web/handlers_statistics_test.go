package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestMain opens the process-wide DB singleton against a durable directory
// before any test runs, so the shared handle does not end up pointing at a
// removed t.TempDir() (which causes readonly-database errors once the first
// test that opened it completes).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "web-test-*")
	if err != nil {
		panic(err)
	}
	if _, err := database.Open(dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func newStatsTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("analytics service: %v", err)
	}
	return NewServerEarly(config.AnalyticsSettings{Refresh: 5, DaysAgo: 7}, "tester", t.TempDir(), svc)
}

// TestPointsHistoryAPIFormat verifies the endpoint returns the documented shape
// with the balance series and typed annotations for a known streamer.
func TestPointsHistoryAPIFormat(t *testing.T) {
	srv := newStatsTestServer(t)
	repo := srv.analytics.Repository()
	const s = "api_fmt_streamer"

	if err := repo.RecordPoints(s, 1000, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}
	if err := repo.RecordPoints(s, 1500, "CLAIM"); err != nil {
		t.Fatalf("seed points: %v", err)
	}
	if err := repo.RecordAnnotation(s, "WATCH_STREAK", "+450 - Watch Streak", "#8b7fd1"); err != nil {
		t.Fatalf("seed annotation: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/points-history?streamer="+s+"&range=7d", nil)
	srv.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got analytics.PointsHistory
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if got.Streamer != s {
		t.Errorf("streamer = %q, want %q", got.Streamer, s)
	}
	if got.Range != "7d" {
		t.Errorf("range = %q, want 7d", got.Range)
	}
	if len(got.Points) != 2 {
		t.Fatalf("points = %d, want 2", len(got.Points))
	}
	if got.Points[0].Balance != 1000 || got.Points[1].Balance != 1500 {
		t.Errorf("point balances = %+v", got.Points)
	}
	if got.Points[1].Reason != "CLAIM" {
		t.Errorf("point reason = %q, want CLAIM", got.Points[1].Reason)
	}
	if len(got.Annotations) != 1 || got.Annotations[0].Type != "WATCH_STREAK" {
		t.Errorf("annotations = %+v", got.Annotations)
	}
}

// TestPointsHistoryBreakdown verifies the earnings-breakdown field over
// production-shaped rows: positive balance deltas grouped by CANONICAL reason
// (the stored streak form is "WATCH STREAK" — Service.RecordPoints writes
// spaces), spends ignored, low-volume reasons pooled into OTHER, sorted by
// gained descending — the data behind the Statistics donut.
func TestPointsHistoryBreakdown(t *testing.T) {
	srv := newStatsTestServer(t)
	repo := srv.analytics.Repository()
	const s = "api_breakdown_streamer"

	seed := []struct {
		balance int
		reason  string
	}{
		{1000, "WATCH"}, // baseline
		{1010, "WATCH"},
		{1510, "CLAIM"},
		{1490, "Spent"}, // negative delta: excluded from the breakdown
		{1740, "RAID"},
		{1741, "WATCH"},
		{2191, "WATCH STREAK"},   // production space form → WATCH_STREAK
		{2192, "WEEKLY REWARDS"}, // deliberately OTHER
	}
	for _, p := range seed {
		if err := repo.RecordPoints(s, p.balance, p.reason); err != nil {
			t.Fatalf("seed points: %v", err)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/points-history?streamer="+s+"&range=7d", nil)
	srv.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got analytics.PointsHistory
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	want := []analytics.ReasonShare{
		{Reason: "CLAIM", Gained: 500, Count: 1},
		{Reason: "WATCH_STREAK", Gained: 450, Count: 1},
		{Reason: "RAID", Gained: 250, Count: 1},
		{Reason: "WATCH", Gained: 11, Count: 2},
		{Reason: "OTHER", Gained: 1, Count: 1},
	}
	if len(got.Breakdown) != len(want) {
		t.Fatalf("breakdown = %+v, want %+v", got.Breakdown, want)
	}
	for i := range want {
		if got.Breakdown[i] != want[i] {
			t.Errorf("breakdown[%d] = %+v, want %+v", i, got.Breakdown[i], want[i])
		}
	}
	// Small seed: neither completeness flag may fire.
	if got.RawTruncated || got.ChartDownsampled {
		t.Errorf("flags = rawTruncated:%v chartDownsampled:%v, want false/false", got.RawTruncated, got.ChartDownsampled)
	}
}

// TestPointsHistoryDefaultRange24h: an absent range parameter must resolve to
// the page default of 24h (matching the UI's initial selection), while an
// unknown value keeps the documented 7d fallback.
func TestPointsHistoryDefaultRange24h(t *testing.T) {
	srv := newStatsTestServer(t)
	repo := srv.analytics.Repository()
	const s = "default_range_streamer"
	if err := repo.RecordPoints(s, 100, "WATCH"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/points-history?streamer="+s, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got analytics.PointsHistory
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Range != "24h" {
		t.Errorf("default range = %q, want 24h", got.Range)
	}
}

// TestStatisticsKPIsDashOnRawTruncation pins the client-side KPI honesty
// contract in the page's renderKPIs function (there is no JS test harness in
// this project, so the contract is asserted on the shipped source):
//
//  1. rawTruncated=true dashes out ALL FOUR KPI tiles — a truncated window is
//     missing its newest rows (ascending fetch + row cap), so the loaded
//     last balance is not the current balance and last-first is not the
//     full-period change;
//  2. chartDownsampled alone never hides KPIs (the function must not consult
//     that flag at all);
//  3. the normal full-response path still computes balance/net from the
//     series and accrued/events from the breakdown, unchanged.
func TestStatisticsKPIsDashOnRawTruncation(t *testing.T) {
	srv := newStatsTestServer(t)
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/statistics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	start := strings.Index(body, "function renderKPIs(data)")
	if start < 0 {
		t.Fatal("statistics page lost the renderKPIs function")
	}
	end := strings.Index(body[start:], "function renderBreakdown")
	if end < 0 {
		t.Fatal("cannot delimit renderKPIs body (renderBreakdown moved?)")
	}
	kpiFn := body[start : start+end]

	// (1) The rawTruncated guard dashes all four tiles and runs BEFORE any
	// computation on the (incomplete) series.
	guard := strings.Index(kpiFn, "data.rawTruncated")
	compute := strings.Index(kpiFn, "last - first")
	if guard < 0 || compute < 0 || guard > compute {
		t.Errorf("renderKPIs must gate on data.rawTruncated before computing last-first (guard=%d compute=%d)", guard, compute)
	}
	if !strings.Contains(kpiFn, `['kpi-balance', 'kpi-net', 'kpi-earned', 'kpi-events']`) {
		t.Errorf("rawTruncated guard must dash out all four KPI tiles, got:\n%s", kpiFn)
	}
	if !strings.Contains(kpiFn, "'—'") {
		t.Errorf("rawTruncated guard must render the dash placeholder")
	}

	// (2) chartDownsampled must never influence KPI visibility.
	if strings.Contains(kpiFn, "chartDownsampled") {
		t.Errorf("renderKPIs must not consult chartDownsampled — downsampling alone never hides KPIs")
	}

	// (3) The full-response computation is intact.
	for _, want := range []string{"share.gained", "share.count", "kpi-balance", "kpi-net", "kpi-earned", "kpi-events"} {
		if !strings.Contains(kpiFn, want) {
			t.Errorf("renderKPIs lost its normal computation marker %q", want)
		}
	}
}

// TestHistoryFlagsIndependent pins the two completeness signals as
// independent: chart downsampling alone must not read as raw truncation
// (which hides the breakdown), and vice versa.
func TestHistoryFlagsIndependent(t *testing.T) {
	cases := []struct {
		name                    string
		rawLen, pointsLen, cap_ int
		wantRaw, wantDown       bool
	}{
		{"small window: neither", 100, 100, 20000, false, false},
		{"downsampled only", 5000, 2000, 20000, false, true},
		{"raw cap hit, no downsample", 20000, 20000, 20000, true, false},
		{"both at once", 20000, 2000, 20000, true, true},
	}
	for _, c := range cases {
		gotRaw, gotDown := historyFlags(c.rawLen, c.pointsLen, c.cap_)
		if gotRaw != c.wantRaw || gotDown != c.wantDown {
			t.Errorf("%s: historyFlags(%d,%d,%d) = (%v,%v), want (%v,%v)",
				c.name, c.rawLen, c.pointsLen, c.cap_, gotRaw, gotDown, c.wantRaw, c.wantDown)
		}
	}
}

// TestPointsHistoryAPIEmpty verifies an unknown streamer yields 200 with empty
// arrays rather than an error.
func TestPointsHistoryAPIEmpty(t *testing.T) {
	srv := newStatsTestServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/points-history?streamer=nobody&range=24h", nil)
	srv.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got analytics.PointsHistory
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Range != "24h" {
		t.Errorf("range = %q, want 24h", got.Range)
	}
	if len(got.Points) != 0 || len(got.Annotations) != 0 {
		t.Errorf("want empty arrays, got %d points / %d annotations", len(got.Points), len(got.Annotations))
	}
}

// TestPointsHistoryAPIValidation covers missing param, bad method, and unknown
// range fallback.
func TestPointsHistoryAPIValidation(t *testing.T) {
	srv := newStatsTestServer(t)
	h := srv.handler()

	// Missing streamer -> 400.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/points-history?range=7d", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing streamer: status = %d, want 400", rec.Code)
	}

	// Wrong method -> 405.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/points-history?streamer=x", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status = %d, want 405", rec.Code)
	}

	// Unknown range falls back to 7d (still 200).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/points-history?streamer=x&range=bogus", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("bogus range: status = %d, want 200", rec.Code)
	}
	var got analytics.PointsHistory
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Range != "7d" {
		t.Errorf("bogus range fell back to %q, want 7d", got.Range)
	}
}

// TestPointsHistoryExport verifies the export endpoint returns data as a
// downloadable attachment with the same filters.
func TestPointsHistoryExport(t *testing.T) {
	srv := newStatsTestServer(t)
	repo := srv.analytics.Repository()
	const s = "export_streamer"
	if err := repo.RecordPoints(s, 2000, "WATCH"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/points-history/export?streamer="+s+"&range=30d", nil)
	srv.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd == "" {
		t.Errorf("missing Content-Disposition header for export")
	}
	var got analytics.PointsHistory
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Points) != 1 || got.Range != "30d" {
		t.Errorf("export payload = %+v", got)
	}
}

// TestStatisticsPageRenders verifies the page handler renders without error and
// includes the chart mount point.
func TestStatisticsPageRenders(t *testing.T) {
	srv := newStatsTestServer(t)

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/statistics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "stat-chart") || !contains(body, "/api/points-history") {
		t.Errorf("statistics page missing expected chart wiring")
	}

	// Summary widgets + breakdown/outcome donuts added with the UI refresh.
	for _, want := range []string{
		"kpi-balance", "kpi-net", "kpi-earned", "kpi-events",
		"stat-donut", "stat-breakdown-legend", "roi-donut",
		"js.stat.br.watch", "--ui-watch",
	} {
		if !contains(body, want) {
			t.Errorf("statistics page missing %q", want)
		}
	}

	// Default range is 24h everywhere the page decides it: the initially
	// active preset button and the JS starting state (which also drives the
	// first fetch and the export link).
	for _, want := range []string{
		`class="stat-range-btn is-active" data-range="24h"`,
		`currentRange = '24h'`,
	} {
		if !contains(body, want) {
			t.Errorf("statistics page missing 24h default marker %q", want)
		}
	}
	if contains(body, `class="stat-range-btn is-active" data-range="7d"`) || contains(body, `currentRange = '7d'`) {
		t.Errorf("statistics page still defaults to 7d somewhere")
	}

	// Independent completeness signals: the partial-data warning element and
	// the two distinct flags wired in the JS.
	for _, want := range []string{"stat-partial", "rawTruncated", "chartDownsampled"} {
		if !contains(body, want) {
			t.Errorf("statistics page missing completeness wiring %q", want)
		}
	}
}

// TestStatisticsPageLocalized verifies the page renders the default RU strings
// and switches to English when the lang cookie is set (it was RU-only before).
func TestStatisticsPageLocalized(t *testing.T) {
	srv := newStatsTestServer(t)

	ru := httptest.NewRecorder()
	srv.handler().ServeHTTP(ru, httptest.NewRequest(http.MethodGet, "/statistics", nil))
	if !contains(ru.Body.String(), "Статистика") {
		t.Errorf("default statistics page should render Russian heading")
	}

	en := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/statistics", nil)
	req.AddCookie(&http.Cookie{Name: "lang", Value: "en"})
	srv.handler().ServeHTTP(en, req)
	body := en.Body.String()
	if !contains(body, "Betting ROI") || !contains(body, "By streamer") {
		t.Errorf("english statistics page missing translated strings")
	}
	if contains(body, "ROI по ставкам") {
		t.Errorf("english render leaked Russian text")
	}

	// KPI labels + the explanatory note distinguishing the two metrics:
	// balance change (includes spending/bets) vs accrued (positive gains
	// only). Both languages must carry the clarified wording.
	for _, want := range []string{"Изменение баланса", "Начислено", "учитывает траты и ставки"} {
		if !contains(ru.Body.String(), want) {
			t.Errorf("RU statistics page missing KPI wording %q", want)
		}
	}
	for _, want := range []string{"Balance change", "Accrued", "includes spending and bets"} {
		if !contains(body, want) {
			t.Errorf("EN statistics page missing KPI wording %q", want)
		}
	}
}

// TestPointsHistoryAnnotationColors verifies the endpoint carries each event
// type's persisted colour in the JSON so the chart can render WATCH_STREAK in its
// own hue, distinct from RAID/WIN (and from the accent-coloured balance line).
// This is the backend half of the "WATCH_STREAK invisible on the chart" fix.
func TestPointsHistoryAnnotationColors(t *testing.T) {
	srv := newStatsTestServer(t)
	repo := srv.analytics.Repository()
	const s = "ann_color_streamer"

	seeds := []struct{ typ, text, color string }{
		{"WATCH_STREAK", "+450 - Watch Streak", "#45c1ff"},
		{"RAID", "+250 - Raid", "#d9a25c"},
		{"WIN", "+100 - Win", "#36b535"},
	}
	for _, sd := range seeds {
		if err := repo.RecordAnnotation(s, sd.typ, sd.text, sd.color); err != nil {
			t.Fatalf("seed %s: %v", sd.typ, err)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/points-history?streamer="+s+"&range=7d", nil)
	srv.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got analytics.PointsHistory
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}

	byType := map[string]string{}
	for _, a := range got.Annotations {
		byType[a.Type] = a.Color
	}
	for _, sd := range seeds {
		if byType[sd.typ] != sd.color {
			t.Errorf("annotation %s colour = %q, want %q (all: %+v)", sd.typ, byType[sd.typ], sd.color, got.Annotations)
		}
	}
	// The whole point of the fix: WATCH_STREAK must not share another type's colour.
	if byType["WATCH_STREAK"] == byType["RAID"] || byType["WATCH_STREAK"] == byType["WIN"] {
		t.Errorf("WATCH_STREAK colour collides with another type: %+v", byType)
	}
}

// TestStatisticsPageColoursAnnotationsFromRecord guards that the chart colours
// annotations from the record's own colour (a.color) instead of the removed
// hardcoded type→colour switch that made WATCH_STREAK fall back to the accent
// (the same colour as the balance line).
func TestStatisticsPageColoursAnnotationsFromRecord(t *testing.T) {
	srv := newStatsTestServer(t)

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/statistics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "a.color") {
		t.Errorf("statistics chart must colour annotations from the record's colour (a.color)")
	}
	// The removed per-type palette token must be gone, or a new event type would
	// again fall through to the accent and vanish against the line.
	if contains(body, "C.event") {
		t.Errorf("removed palette token C.event still referenced; type→colour switch not fully removed")
	}
}

// TestStatisticsDropdownUsesConfiguredRoster pins the §3 fix: the streamer
// selector is built from the configured roster, not analytics history, so a
// streamer removed from settings (whose points/annotation rows are deliberately
// retained) no longer lingers in the dropdown — and cannot silently become the
// default first <option>. The retained history must still be queryable by
// direct URL.
func TestStatisticsDropdownUsesConfiguredRoster(t *testing.T) {
	srv := newStatsTestServer(t)
	repo := srv.analytics.Repository()

	// Configured roster: alpha, beta. A removed streamer "gamma-removed" keeps
	// its history rows but is not in the roster.
	srv.AttachStreamers([]*models.Streamer{
		models.NewStreamer("alpha", models.DefaultStreamerSettings()),
		models.NewStreamer("beta", models.DefaultStreamerSettings()),
	})
	if err := repo.RecordPoints("alpha", 1000, "WATCH"); err != nil {
		t.Fatalf("seed alpha: %v", err)
	}
	if err := repo.RecordPoints("gamma-removed", 500, "WATCH"); err != nil {
		t.Fatalf("seed gamma: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/statistics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{`<option value="alpha">`, `<option value="beta">`} {
		if !contains(body, want) {
			t.Errorf("dropdown missing configured streamer option %q", want)
		}
	}
	if contains(body, `value="gamma-removed"`) {
		t.Errorf("removed streamer gamma-removed must not appear in the dropdown")
	}

	// History for the removed streamer is preserved and still queryable directly.
	rec2 := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/points-history?streamer=gamma-removed&range=7d", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("history status = %d, want 200 (history must be preserved)", rec2.Code)
	}
	var hist analytics.PointsHistory
	if err := json.Unmarshal(rec2.Body.Bytes(), &hist); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(hist.Points) != 1 {
		t.Errorf("removed streamer history points = %d, want 1 (history must not be destroyed)", len(hist.Points))
	}
}

// TestStatisticsDropdownEmptyRosterIsSafe: with no configured streamers the page
// still renders (disabled selector + empty state), never a crash or a stale
// history-derived option.
func TestStatisticsDropdownEmptyRosterIsSafe(t *testing.T) {
	srv := newStatsTestServer(t)
	repo := srv.analytics.Repository()
	// History exists for a streamer that is NOT in the (empty) roster.
	if err := repo.RecordPoints("orphan", 1000, "WATCH"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/statistics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if contains(body, `value="orphan"`) {
		t.Errorf("history-only streamer orphan must not appear with an empty roster")
	}
}

// TestPointsHistoryBetSummaryAndPredictionSlice pins the §4 fix: a prediction
// win is a first-class PREDICTION breakdown slice (not OTHER), and the response
// carries a betSummary (won/staked/refunded/net) derived from the same window's
// bets. A prediction loss must never appear as a positive breakdown slice.
func TestPointsHistoryBetSummaryAndPredictionSlice(t *testing.T) {
	srv := newStatsTestServer(t)
	repo := srv.analytics.Repository()
	const s = "bet_summary_streamer"

	// Points series with a prediction credit and a prediction loss (a spend).
	for _, p := range []struct {
		balance int
		reason  string
	}{
		{1000, "WATCH"},      // baseline
		{900, "Spent"},       // bet placed (-100): excluded from breakdown
		{1150, "PREDICTION"}, // won round (+250 gross credit)
		{1050, "Spent"},      // second bet placed (-100)
		{1050, "PREDICTION"}, // lost round: flat, no credit
	} {
		if err := repo.RecordPoints(s, p.balance, p.reason); err != nil {
			t.Fatalf("seed points: %v", err)
		}
	}
	// Matching bet records (one win, one loss) for the summary.
	if err := repo.RecordBet(analytics.BetRecord{
		EventID: "bs-win", Streamer: s, Timestamp: time.Now().UnixMilli(), Strategy: "SMART",
		ResultType: "WIN", Placed: 100, Won: 250, Gained: 150, Odds: 2.5,
	}); err != nil {
		t.Fatalf("record win: %v", err)
	}
	if err := repo.RecordBet(analytics.BetRecord{
		EventID: "bs-lose", Streamer: s, Timestamp: time.Now().UnixMilli(), Strategy: "SMART",
		ResultType: "LOSE", Placed: 100, Won: 0, Gained: -100, Odds: 1.8,
	}); err != nil {
		t.Fatalf("record lose: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/points-history?streamer="+s+"&range=7d", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got analytics.PointsHistory
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Prediction win is its own slice; no positive slice from the loss.
	var pred *analytics.ReasonShare
	for i := range got.Breakdown {
		if got.Breakdown[i].Reason == "PREDICTION" {
			pred = &got.Breakdown[i]
		}
		if got.Breakdown[i].Reason == "OTHER" {
			t.Errorf("prediction credit leaked into OTHER: %+v", got.Breakdown)
		}
	}
	if pred == nil || pred.Gained != 250 || pred.Count != 1 {
		t.Fatalf("PREDICTION slice = %+v, want gained 250 count 1", pred)
	}

	// Bet summary present and consistent: Net == Won - Staked.
	if got.BetSummary == nil {
		t.Fatal("expected a betSummary in the response")
	}
	bs := got.BetSummary
	if bs.Won != 250 || bs.Staked != 200 || bs.Net != 50 || bs.Wins != 1 || bs.Losses != 1 {
		t.Errorf("betSummary = %+v, want won 250 staked 200 net 50 wins 1 losses 1", bs)
	}
	if bs.Net != bs.Won-bs.Staked {
		t.Errorf("betSummary invariant broken: net %d != won %d - staked %d", bs.Net, bs.Won, bs.Staked)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
