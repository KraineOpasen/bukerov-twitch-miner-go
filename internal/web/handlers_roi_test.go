package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
)

func seedBet(t *testing.T, srv *Server, b analytics.BetRecord) {
	t.Helper()
	if err := srv.analytics.Repository().RecordBet(b); err != nil {
		t.Fatalf("seed bet: %v", err)
	}
}

func TestROIAPIComputesSummary(t *testing.T) {
	srv := newStatsTestServer(t)
	now := time.Now()
	s := "roi_api_streamer"
	// 1 win (+150), 1 loss (-100), 1 refund (0). wagered 200, net +50.
	seedBet(t, srv, analytics.BetRecord{EventID: "ra-1", Streamer: s, Timestamp: now.UnixMilli(), Strategy: "SMART", ResultType: "WIN", Placed: 100, Won: 250, Gained: 150, Odds: 2.5})
	seedBet(t, srv, analytics.BetRecord{EventID: "ra-2", Streamer: s, Timestamp: now.UnixMilli(), Strategy: "SMART", ResultType: "LOSE", Placed: 100, Won: 0, Gained: -100, Odds: 1.8})
	seedBet(t, srv, analytics.BetRecord{EventID: "ra-3", Streamer: s, Timestamp: now.UnixMilli(), Strategy: "SMART", ResultType: "REFUND", Placed: 100, Won: 0, Gained: 0, Odds: 2.0})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/predictions/roi?streamer="+s+"&period=30d", nil)
	srv.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got analytics.ROISummary
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if got.Period != "30d" || got.Streamer != s {
		t.Errorf("labels wrong: %+v", got)
	}
	if got.Count != 3 || got.Wins != 1 || got.Losses != 1 || got.Refunds != 1 {
		t.Errorf("counts wrong: %+v", got)
	}
	if got.TotalWagered != 200 || got.NetProfit != 50 {
		t.Errorf("wagered=%d net=%d, want 200/50", got.TotalWagered, got.NetProfit)
	}
	if got.Empty {
		t.Error("summary must not be empty")
	}
}

func TestROIAPIEmptyState(t *testing.T) {
	srv := newStatsTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/predictions/roi?streamer=nobody&period=lifetime", nil)
	srv.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got analytics.ROISummary
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Empty || got.Count != 0 {
		t.Errorf("expected empty summary, got %+v", got)
	}
	if got.Period != "lifetime" {
		t.Errorf("period = %q, want lifetime", got.Period)
	}
}

func TestROIAPIPeriodFiltersOldBets(t *testing.T) {
	srv := newStatsTestServer(t)
	now := time.Now()
	s := "roi_period_streamer"
	// One recent bet, one 60 days old.
	seedBet(t, srv, analytics.BetRecord{EventID: "rp-1", Streamer: s, Timestamp: now.UnixMilli(), Strategy: "SMART", ResultType: "WIN", Placed: 100, Won: 200, Gained: 100, Odds: 2.0})
	seedBet(t, srv, analytics.BetRecord{EventID: "rp-2", Streamer: s, Timestamp: now.Add(-60 * 24 * time.Hour).UnixMilli(), Strategy: "SMART", ResultType: "WIN", Placed: 100, Won: 200, Gained: 100, Odds: 2.0})

	// 7d window sees only the recent one.
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/predictions/roi?streamer="+s+"&period=7d", nil))
	var got analytics.ROISummary
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Count != 1 {
		t.Errorf("7d count = %d, want 1", got.Count)
	}

	// lifetime sees both.
	rec = httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/predictions/roi?streamer="+s+"&period=lifetime", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Count != 2 {
		t.Errorf("lifetime count = %d, want 2", got.Count)
	}
}

func TestROIExportSetsAttachment(t *testing.T) {
	srv := newStatsTestServer(t)
	seedBet(t, srv, analytics.BetRecord{EventID: "re-1", Streamer: "roi_exp", Timestamp: time.Now().UnixMilli(), Strategy: "SMART", ResultType: "WIN", Placed: 100, Won: 200, Gained: 100, Odds: 2.0})

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/predictions/roi/export?streamer=roi_exp&period=90d", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd == "" {
		t.Error("export must set Content-Disposition attachment header")
	}
}

func TestROIAPIRejectsPost(t *testing.T) {
	srv := newStatsTestServer(t)
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/predictions/roi", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
