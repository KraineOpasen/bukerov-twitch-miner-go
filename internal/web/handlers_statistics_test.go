package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
