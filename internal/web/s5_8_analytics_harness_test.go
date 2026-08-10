package web

// S5-8 permanent localhost browser-evidence harness for the Analytics group's
// two direct routes (/analytics/points, /analytics/roi).
//
// Serves the REAL handler chain — the same routing, chrome, templates and
// middleware production uses — against deterministic in-memory fixtures, so a
// browser can be driven over the full evidence matrix:
//
//	RU + EN; light + dark + a live theme switch; 1440 / 1100 / 800 / <768;
//	Points READY / EMPTY / PART / FAIL(+Retry); ROI READY / EMPTY / FAIL(+Retry);
//	prefers-reduced-motion; table -> cards; keyboard/focus order; the exact
//	aria-current destination; CSV download; and no horizontal overflow.
//
// It binds 127.0.0.1 only and never talks to Twitch, Discord, or any network.
// Env-gated: skipped unless MINER_S5_8_HARNESS=1.
//
// Usage:
//
//	MINER_S5_8_HARNESS=1 MINER_S5_8_HARNESS_ADDR=127.0.0.1:8978 \
//	  go test -run TestS5_8EvidenceHarness -timeout 1800s ./internal/web/
//
// The harness shuts itself down after s58HarnessDeadline, which is
// deliberately SHORTER than the -timeout above: if the two were equal, go's
// own test timeout could win the race and kill the run with a goroutine-dump
// panic instead of the clean shutdown the deferred stop() performs. See
// TestS5_8HarnessDeadlineBeatsDocumentedTimeout.
//
// State selection. The two pages fetch their data from the real JSON
// endpoints, so the non-READY states are produced by intercepting those two
// endpoints in front of the production mux — never by adding a state
// parameter to production code. The selected state travels on the PAGE's
// query string (e.g. /analytics/points?state=part) and is recovered from the
// fetch's Referer header, which same-origin requests carry in full under this
// app's own "Referrer-Policy: same-origin" header. READY is the default and
// is served by the untouched production handler over the seeded fixtures.

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
)

// s58GET builds a plain GET request against the harness handler.
func s58GET(path string) *http.Request {
	return httptest.NewRequest(http.MethodGet, path, nil)
}

// s58HarnessDeadline bounds an unattended evidence run. It must stay strictly
// below the -timeout this file documents, so the harness always ends through
// its own clean shutdown path rather than go's test-timeout panic.
const s58HarnessDeadline = 25 * time.Minute

// s58DocumentedTimeout is the -timeout value the usage comment above tells
// operators to pass.
const s58DocumentedTimeout = 1800 * time.Second

// s58HarnessStates are the state names the harness understands on a page's
// ?state= query string. Anything else (including its absence) is READY.
var s58HarnessStates = []string{"empty", "part", "fail"}

// s58StateFromReferer recovers the selected harness state from an API
// request's Referer. Returns "" for READY.
func s58StateFromReferer(r *http.Request) string {
	ref := r.Header.Get("Referer")
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	got := u.Query().Get("state")
	for _, s := range s58HarnessStates {
		if got == s {
			return s
		}
	}
	return ""
}

// s58SeedFixtures records the deterministic points, annotations and settled
// bets both READY pages render. Values are fixed (no randomness, no wall-clock
// dependence beyond "now"), so two runs produce the same picture.
func s58SeedFixtures(t *testing.T, repo analytics.Repository) {
	t.Helper()

	// Points: a rising balance for streamer_a across every earn reason the
	// breakdown distinguishes, plus a smaller series for streamer_b so the
	// streamer selector has a real second choice.
	reasons := []string{"WATCH", "WATCH", "WATCH", "CLAIM", "RAID", "WATCH_STREAK", "PREDICTION"}
	deltas := []int{10, 10, 10, 50, 250, 450, 1200}
	balance := 20000
	for i := 0; i < 140; i++ {
		balance += deltas[i%len(deltas)]
		if err := repo.RecordPoints("streamer_a", balance, reasons[i%len(reasons)]); err != nil {
			t.Fatalf("seed points: %v", err)
		}
	}
	for i := 0; i < 40; i++ {
		if err := repo.RecordPoints("streamer_b", 5000+i*20, "WATCH"); err != nil {
			t.Fatalf("seed points: %v", err)
		}
	}
	_ = repo.RecordAnnotation("streamer_a", "WATCH_STREAK", "+450 - Watch Streak", "#B6FF3B")
	_ = repo.RecordAnnotation("streamer_a", "WIN", "+1200 - Prediction WIN", "#39FF88")
	_ = repo.RecordAnnotation("streamer_a", "LOSE", "-500 - Prediction LOSE", "#FF4D67")

	// Bets: enough settled outcomes across two streamers, three strategies and
	// a spread of odds that all three ROI breakdown tables have several rows,
	// with a genuinely mixed win/loss/refund split.
	type seedBet struct {
		streamer string
		strategy string
		result   string
		placed   int
		won      int
		gained   int
		odds     float64
	}
	bets := []seedBet{
		{"streamer_a", "SMART", "WIN", 500, 1400, 900, 2.8},
		{"streamer_a", "SMART", "LOSE", 400, 0, -400, 1.9},
		{"streamer_a", "SMART", "WIN", 300, 690, 390, 2.3},
		{"streamer_a", "HIGH_ODDS", "LOSE", 800, 0, -800, 4.5},
		{"streamer_a", "HIGH_ODDS", "WIN", 250, 1375, 1125, 5.5},
		{"streamer_a", "MOST_VOTED", "REFUND", 600, 600, 0, 1.0},
		{"streamer_b", "SMART", "WIN", 700, 1190, 490, 1.7},
		{"streamer_b", "SMART", "LOSE", 550, 0, -550, 2.1},
		{"streamer_b", "MOST_VOTED", "WIN", 450, 900, 450, 2.0},
		{"streamer_b", "MOST_VOTED", "LOSE", 350, 0, -350, 3.2},
		{"streamer_b", "HIGH_ODDS", "LOSE", 900, 0, -900, 6.1},
		{"streamer_b", "SMART", "REFUND", 200, 200, 0, 1.0},
	}
	for i, b := range bets {
		if err := repo.RecordBet(analytics.BetRecord{
			EventID:    fmt.Sprintf("s58-seed-%d", i),
			Streamer:   b.streamer,
			Timestamp:  time.Now().Add(-time.Duration(i+1) * time.Hour).UnixMilli(),
			Strategy:   b.strategy,
			ResultType: b.result,
			Placed:     b.placed,
			Won:        b.won,
			Gained:     b.gained,
			Odds:       b.odds,
		}); err != nil {
			t.Fatalf("seed bet: %v", err)
		}
	}
}

// s58StateInterceptor wraps the production handler, serving the EMPTY / PART /
// FAIL variants of the two JSON endpoints the pages read. READY (and every
// other request, including every page render) falls through untouched, so the
// evidence is captured against the real handler chain.
func s58StateInterceptor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := s58StateFromReferer(r)
		if state == "" {
			next.ServeHTTP(w, r)
			return
		}
		isPoints := strings.HasPrefix(r.URL.Path, "/api/points-history")
		isROI := strings.HasPrefix(r.URL.Path, "/api/predictions/roi")
		if !isPoints && !isROI {
			next.ServeHTTP(w, r)
			return
		}

		switch state {
		case "fail":
			// S-FAIL: the page must show its inline role="alert" block with a
			// working Retry, never a toast and never a blank chart.
			writeInternalError(w, "harness: forced failure")
			return
		case "empty":
			if isPoints {
				writeJSONOK(w, analytics.PointsHistory{Streamer: r.URL.Query().Get("streamer"), Range: "24h", Points: nil})
			} else {
				writeJSONOK(w, analytics.ROISummary{Period: "30d", Empty: true})
			}
			return
		case "part":
			if !isPoints {
				// PART is a points-only state: it models the backend row cap,
				// which the ROI summary has no equivalent of.
				next.ServeHTTP(w, r)
				return
			}
			rec := &s58Capture{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			rec.flushWithRawTruncated(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// s58Capture buffers the production JSON response so the PART variant can be
// produced by re-marshalling the real payload with rawTruncated set — the page
// then dashes out every KPI, which is exactly the behavior under evidence.
type s58Capture struct {
	http.ResponseWriter
	body   []byte
	status int
}

func (c *s58Capture) WriteHeader(status int) { c.status = status }
func (c *s58Capture) Write(b []byte) (int, error) {
	c.body = append(c.body, b...)
	return len(b), nil
}

func (c *s58Capture) flushWithRawTruncated(w http.ResponseWriter) {
	if c.status != 0 && c.status != http.StatusOK {
		w.WriteHeader(c.status)
		_, _ = w.Write(c.body)
		return
	}
	patched := strings.Replace(string(c.body), `"rawTruncated":false`, `"rawTruncated":true`, 1)
	if patched == string(c.body) {
		// The field is omitted or already true; append it defensively so the
		// harness never silently serves a READY payload as PART.
		if strings.HasSuffix(patched, "}") {
			patched = patched[:len(patched)-1] + `,"rawTruncated":true}`
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(patched))
}

// TestS5_8EvidenceHarness serves the two Analytics pages for browser evidence.
func TestS5_8EvidenceHarness(t *testing.T) {
	if os.Getenv("MINER_S5_8_HARNESS") != "1" {
		t.Skip("harness disabled (set MINER_S5_8_HARNESS=1)")
	}
	addr := os.Getenv("MINER_S5_8_HARNESS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8978"
	}
	if !s58IsLoopback(addr) {
		t.Fatalf("refusing to bind %q: the evidence harness is localhost-only", addr)
	}

	srv := buildF3PageServer(t)
	s58SeedFixtures(t, srv.analytics.Repository())
	// Report "running" so the status overlay never covers the pages: every
	// provider is a deterministic fake, so there is no startup sequence to
	// reflect.
	srv.GetStatusBroadcaster().SetStatus(StatusRunning, "")

	handle, err := startS5_6EvidenceHarness(s58StateInterceptor(srv.handler()), addr)
	if err != nil {
		t.Fatalf("evidence harness failed to start: %v", err)
	}
	defer func() {
		if err := handle.stop(); err != nil {
			t.Errorf("evidence harness shutdown: %v", err)
		}
	}()

	base := "http://" + handle.Addr.String()
	t.Logf("S5-8 evidence harness serving on %s", base)
	t.Logf("  READY: %s/analytics/points   %s/analytics/roi", base, base)
	t.Logf("  EMPTY: %s/analytics/points?state=empty   %s/analytics/roi?state=empty", base, base)
	t.Logf("  PART:  %s/analytics/points?state=part", base)
	t.Logf("  FAIL:  %s/analytics/points?state=fail   %s/analytics/roi?state=fail", base, base)
	t.Logf("  language: set the %q cookie to en|ru", langCookieName)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)
	select {
	case <-sig:
	case serveErr := <-handle.errCh:
		t.Fatalf("evidence harness stopped serving before it was asked to: %v", serveErr)
	case <-time.After(s58HarnessDeadline):
	}
}

// s58IsLoopback reports whether addr's host is a loopback address. The
// evidence harness serves unauthenticated fixtures, so it must never be
// reachable off-host.
func s58IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------------
// Harness self-checks — these run in the normal suite (no env gate), so a
// harness that would serve the wrong thing fails CI rather than quietly
// producing misleading browser evidence.
// ---------------------------------------------------------------------

// TestS5_8HarnessDeadlineBeatsDocumentedTimeout proves the harness always ends
// through its own shutdown path. Found the hard way: with both set to 30
// minutes, go's -timeout won the race and killed an evidence run with a
// goroutine dump instead of a clean stop.
func TestS5_8HarnessDeadlineBeatsDocumentedTimeout(t *testing.T) {
	if s58HarnessDeadline >= s58DocumentedTimeout {
		t.Fatalf("harness deadline %v must be strictly below the documented -timeout %v, or go's test timeout panics the run",
			s58HarnessDeadline, s58DocumentedTimeout)
	}
	// A margin large enough that a slow shutdown still lands inside -timeout.
	if margin := s58DocumentedTimeout - s58HarnessDeadline; margin < time.Minute {
		t.Errorf("only %v of margin between the harness deadline and -timeout; want at least a minute", margin)
	}
	src, err := os.ReadFile("s5_8_analytics_harness_test.go")
	if err != nil {
		t.Fatalf("read own source: %v", err)
	}
	if !strings.Contains(string(src), "-timeout 1800s") {
		t.Error("the usage comment no longer documents the -timeout s58DocumentedTimeout describes")
	}
}

// TestS5_8HarnessRefusesNonLoopbackBind proves the localhost-only guard is
// real: only loopback hosts are accepted.
func TestS5_8HarnessRefusesNonLoopbackBind(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8978", "localhost:8978", "[::1]:8978"} {
		if !s58IsLoopback(addr) {
			t.Errorf("%s should be accepted as loopback", addr)
		}
	}
	for _, addr := range []string{"0.0.0.0:8978", "192.168.1.10:8978", "example.com:8978", "8978"} {
		if s58IsLoopback(addr) {
			t.Errorf("%s must be refused — the harness is localhost-only", addr)
		}
	}
}

// TestS5_8HarnessStateSelection proves the state is recovered only from a
// same-origin page Referer carrying a KNOWN state, so a stray query parameter
// can never silently turn a READY evidence run into a fabricated one.
func TestS5_8HarnessStateSelection(t *testing.T) {
	mk := func(ref string) *http.Request {
		r := s58GET("/api/points-history?streamer=streamer_a")
		if ref != "" {
			r.Header.Set("Referer", ref)
		}
		return r
	}
	cases := map[string]string{
		"":                                       "",
		"http://127.0.0.1:8978/analytics/points": "",
		"http://127.0.0.1:8978/analytics/points?x=1":         "",
		"http://127.0.0.1:8978/analytics/points?state=":      "",
		"http://127.0.0.1:8978/analytics/points?state=bogus": "",
		"http://127.0.0.1:8978/analytics/points?state=empty": "empty",
		"http://127.0.0.1:8978/analytics/points?state=part":  "part",
		"http://127.0.0.1:8978/analytics/roi?state=fail":     "fail",
	}
	for ref, want := range cases {
		if got := s58StateFromReferer(mk(ref)); got != want {
			t.Errorf("Referer %q => state %q, want %q", ref, got, want)
		}
	}
}

// TestS5_8HarnessServesEveryState proves each state actually reaches the
// browser as the shape the evidence claims: READY renders data, EMPTY an empty
// payload, PART the truncation flag, FAIL a 5xx — and that the PAGE renders
// 200 in every case, so the state is always presented by the page's own inline
// block rather than by a broken navigation.
func TestS5_8HarnessServesEveryState(t *testing.T) {
	srv := buildF3PageServer(t)
	s58SeedFixtures(t, srv.analytics.Repository())
	h := s58StateInterceptor(srv.handler())

	get := func(path, referer string) (int, string) {
		req := s58GET(path)
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	// Every page URL renders 200 regardless of the state it selects.
	for _, route := range s58Routes {
		for _, state := range append([]string{""}, s58HarnessStates...) {
			path := route
			if state != "" {
				path += "?state=" + state
			}
			if code, _ := get(path, ""); code != http.StatusOK {
				t.Errorf("GET %s = %d, want 200", path, code)
			}
		}
	}

	const pointsAPI = "/api/points-history?streamer=streamer_a&range=24h"
	const roiAPI = "/api/predictions/roi?period=30d"
	page := func(route, state string) string {
		return "http://127.0.0.1:8978" + route + "?state=" + state
	}

	// READY: real seeded data, not truncated.
	code, body := get(pointsAPI, "http://127.0.0.1:8978/analytics/points")
	if code != http.StatusOK || !strings.Contains(body, `"balance"`) {
		t.Errorf("READY points = %d, body=%.120s", code, body)
	}
	code, body = get(roiAPI, "http://127.0.0.1:8978/analytics/roi")
	if code != http.StatusOK || strings.Contains(body, `"empty":true`) {
		t.Errorf("READY roi = %d, body=%.160s — the seeded bets must produce a non-empty summary", code, body)
	}
	if !strings.Contains(body, `"byOddsBucket"`) {
		t.Error("READY roi payload has no byOddsBucket breakdown — the odds seed is too narrow for evidence")
	}

	// EMPTY.
	if code, body = get(pointsAPI, page("/analytics/points", "empty")); code != http.StatusOK || !strings.Contains(body, `"points":null`) {
		t.Errorf("EMPTY points = %d, body=%.120s", code, body)
	}
	if code, body = get(roiAPI, page("/analytics/roi", "empty")); code != http.StatusOK || !strings.Contains(body, `"empty":true`) {
		t.Errorf("EMPTY roi = %d, body=%.120s", code, body)
	}

	// PART: the truncation flag the page turns into dashed-out KPIs.
	if code, body = get(pointsAPI, page("/analytics/points", "part")); code != http.StatusOK || !strings.Contains(body, `"rawTruncated":true`) {
		t.Errorf("PART points = %d, body=%.200s", code, body)
	}

	// FAIL.
	if code, _ = get(pointsAPI, page("/analytics/points", "fail")); code != http.StatusInternalServerError {
		t.Errorf("FAIL points = %d, want 500", code)
	}
	if code, _ = get(roiAPI, page("/analytics/roi", "fail")); code != http.StatusInternalServerError {
		t.Errorf("FAIL roi = %d, want 500", code)
	}
}
