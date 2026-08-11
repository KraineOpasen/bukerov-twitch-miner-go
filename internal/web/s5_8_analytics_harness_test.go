package web

// S5-8 permanent localhost browser-evidence harness for the Analytics group's
// two direct routes (/analytics/points, /analytics/roi).
//
// Serves the REAL handler chain — the same routing, chrome, templates and
// middleware production uses — against deterministic in-memory fixtures, so a
// browser can be driven over the full evidence matrix:
//
//	RU + EN; light + dark + a live theme switch; 1440 / 1100 / 800 / <768;
//	Points READY / EMPTY / PART / SPARSE / FAIL(+Retry); ROI READY / EMPTY /
//	FAIL(+Retry); prefers-reduced-motion; table -> cards; keyboard/focus order;
//	the exact aria-current destination; CSV download; and no horizontal overflow.
//
// s58BrowserScenarios is the standing, named catalogue of what this harness is
// FOR: each entry pins one behavior that lives inside a page's client IIFE and
// is therefore unobservable from Go, together with the exact URL that produces
// it. TestS5_8EvidenceHarness prints the catalogue on startup, and
// TestS5_8BrowserScenarioCatalogueIsServable keeps it from naming a URL the
// harness does not serve.
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
//
// "sparse" serves a SINGLE-sample points payload with no breakdown key — the
// indeterminate window in which a change and a per-reason total do not exist to
// be measured. It is a browser-observable state on purpose: whether the page
// dashes those tiles out or paints a fabricated 0 is a client-IIFE decision, so
// it cannot be settled by a Go assertion over template text.
var s58HarnessStates = []string{"empty", "part", "sparse", "fail"}

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

// s58PointSamples is how many samples streamer_a's seeded series holds.
const s58PointSamples = 140

// s58AnnotationSeed is one deterministic annotation, written at a fixed INDEX
// inside the points loop rather than after it.
//
// RecordAnnotation stamps time.Now() itself and accepts no explicit timestamp,
// so writing an annotation BETWEEN two RecordPoints calls is the only way to
// place it inside the series' x-range. Seeding all three after the loop (the
// pass-2 behavior) put every annotation strictly to the right of the last
// sample, where ApexCharts clips an xaxis annotation: /api/points-history
// dutifully returned three annotations and the browser drew none of them. An
// API-level count is therefore not evidence that an annotation is visible.
type s58AnnotationSeed struct {
	afterSample int
	eventType   string
	text        string
	// color is deliberately EMPTY on the first seed. The page resolves an
	// annotation's ink as `a.color || P.s1`, so an empty colour falls back to
	// the --chart-series-1 token, which resolves to --prim-night-purple in dark
	// and --prim-day-purple in light. That contrast against the two hex-pinned
	// seeds is what lets browser evidence show annotation ink actually
	// recolouring with the theme rather than being frozen to a stored hex.
	color string
}

// s58Annotations spread three markers across the middle of streamer_a's series,
// well inside both ends, so every one of them lands in the drawn x-range.
var s58Annotations = []s58AnnotationSeed{
	{afterSample: 35, eventType: "WATCH_STREAK", text: "+450 - Watch Streak", color: ""},
	{afterSample: 70, eventType: "WIN", text: "+1200 - Prediction WIN", color: "#39FF88"},
	{afterSample: 105, eventType: "LOSE", text: "-500 - Prediction LOSE", color: "#FF4D67"},
}

// s58SeedFixtures is the test-facing adapter over s58Seed: seeding is one
// operation, so any failed write fails setup outright.
func s58SeedFixtures(t *testing.T, repo analytics.Repository) {
	t.Helper()
	if err := s58Seed(repo); err != nil {
		t.Fatalf("seed fixtures: %v", err)
	}
}

// s58Seed records the deterministic points, annotations and settled bets both
// READY pages render. Values are fixed (no randomness, no wall-clock dependence
// beyond "now"), so two runs produce the same picture.
//
// Every write is checked. A best-effort write would let a tombstoned streamer,
// a closed database or a migration drift produce a harness that comes up
// healthy and serves a chart with silently missing data — and browser evidence
// captured against it would be worthless without looking worthless.
func s58Seed(repo analytics.Repository) error {
	// Points: a rising balance for streamer_a across every earn reason the
	// breakdown distinguishes, plus a smaller series for streamer_b so the
	// streamer selector has a real second choice.
	reasons := []string{"WATCH", "WATCH", "WATCH", "CLAIM", "RAID", "WATCH_STREAK", "PREDICTION"}
	deltas := []int{10, 10, 10, 50, 250, 450, 1200}
	balance := 20000
	for i := 0; i < s58PointSamples; i++ {
		balance += deltas[i%len(deltas)]
		if err := repo.RecordPoints("streamer_a", balance, reasons[i%len(reasons)]); err != nil {
			return fmt.Errorf("seed points streamer_a[%d]: %w", i, err)
		}
		for _, a := range s58Annotations {
			if a.afterSample != i {
				continue
			}
			if err := repo.RecordAnnotation("streamer_a", a.eventType, a.text, a.color); err != nil {
				return fmt.Errorf("seed annotation %s: %w", a.eventType, err)
			}
		}
	}
	for i := 0; i < 40; i++ {
		if err := repo.RecordPoints("streamer_b", 5000+i*20, "WATCH"); err != nil {
			return fmt.Errorf("seed points streamer_b[%d]: %w", i, err)
		}
	}

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
			return fmt.Errorf("seed bet %s/%s: %w", b.streamer, b.strategy, err)
		}
	}
	return nil
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
		case "sparse":
			if !isPoints {
				next.ServeHTTP(w, r)
				return
			}
			// One sample, and no breakdown key at all — the shape
			// /api/points-history really produces for a window holding a single
			// observation. No change EXISTS across it and nothing is attributable
			// per reason, so the derived tiles have nothing to report.
			writeJSONOK(w, analytics.PointsHistory{
				Streamer: r.URL.Query().Get("streamer"),
				Range:    "24h",
				Points:   []analytics.PointSample{{T: time.Now().UnixMilli(), Balance: 20000, Reason: "WATCH"}},
			})
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

// s58BrowserScenario is one NAMED localhost evidence scenario.
//
// The Go suite pins what the SERVER answers, what the page RENDERS and what the
// SEEDED DATA is. None of those can observe a client IIFE actually running: an
// assertion that the string "toISOString()" appears in a template proves only
// that the string appears in the template — it passes over a script that never
// executes, a helper nobody calls, and any behavior the browser really shows.
// Those behaviors are proven here instead, by driving the listed URL in a real
// browser on loopback and recording the result.
type s58BrowserScenario struct {
	// Name is how the scenario is cited in an evidence write-up.
	Name string
	// Route is the page under evidence; State selects the harness variant
	// ("" is READY).
	Route, State string
	// Proves is the behavior the scenario establishes.
	Proves string
}

// s58BrowserScenarios is the standing evidence matrix. Every entry names a
// behavior that lives inside a page's client IIFE and is therefore invisible to
// the Go suite by construction.
var s58BrowserScenarios = []s58BrowserScenario{
	{"points-stale-silent", "/analytics/points", "fail",
		"a failed TIMED refresh over retained content raises the S-STALE strip with zero live-region announcements"},
	{"points-part-silent", "/analytics/points", "part",
		"the S-PART strip re-asserted by a timed poll announces nothing, and every KPI dashes out"},
	{"points-fail-retry-stamp", "/analytics/points", "fail",
		"terminal S-FAIL shows cause + Retry + its own failure time, and each Retry failure re-stamps a strictly later datetime"},
	{"roi-fail-retry-stamp", "/analytics/roi", "fail",
		"the same terminal S-FAIL contract on ROI, including a re-stamp per failed Retry"},
	{"points-annotations-recolour", "/analytics/points", "",
		"at least one xaxis annotation is drawn, and the token-backed one changes ink between light and dark"},
	{"points-summary-announces-once", "/analytics/points", "",
		"the C14 summary announces on a user-initiated render and stays silent across timed refreshes"},
	{"points-strips-leave-with-content", "/analytics/points", "part",
		"the S-PART strip disappears when the page leaves the content state rather than captioning a failure block"},
	{"points-sparse-dashes", "/analytics/points", "sparse",
		"a single-sample window dashes out net change, earned and events instead of painting a fabricated 0"},
	{"points-empty", "/analytics/points", "empty",
		"an empty series shows S-EMPTY alone, with no chart and no stale or partial strip"},
	{"points-csv", "/analytics/points", "",
		"the CSV is produced in-browser from the already-fetched JSON, with ISO-8601 UTC times and escaped/formula-guarded cells"},
	{"roi-readonly-get-only", "/analytics/roi", "",
		"every request the page issues is a GET; no form, no mutation affordance"},
	{"roi-csv", "/analytics/roi", "",
		"the outcome CSV is generated client-side from the same summary the tiles read"},
	{"points-straight-no-interpolation", "/analytics/points", "",
		"the drawn path has vertices only at real samples — no smoothing, no gap fill, no synthetic zero"},
	{"reduced-motion", "/analytics/points", "",
		"prefers-reduced-motion disables chart animation outright rather than shortening it"},
}

// s58ScenarioURL builds the loopback URL that produces a scenario.
func (s s58BrowserScenario) s58URL(base string) string {
	if s.State == "" {
		return base + s.Route
	}
	return base + s.Route + "?state=" + s.State
}

// TestS5_8BrowserScenarioCatalogueIsServable proves the catalogue can never
// name a URL the harness does not actually serve: every scenario must target a
// real route and a state the interceptor understands, and names must be unique
// so evidence can cite one unambiguously.
func TestS5_8BrowserScenarioCatalogueIsServable(t *testing.T) {
	routes := map[string]bool{}
	for _, r := range s58Routes {
		routes[r] = true
	}
	states := map[string]bool{"": true}
	for _, s := range s58HarnessStates {
		states[s] = true
	}

	seen := map[string]bool{}
	for _, sc := range s58BrowserScenarios {
		if sc.Name == "" || sc.Proves == "" {
			t.Errorf("scenario %+v must carry both a name and the behavior it proves", sc)
		}
		if seen[sc.Name] {
			t.Errorf("duplicate scenario name %q — evidence must cite exactly one scenario", sc.Name)
		}
		seen[sc.Name] = true
		if !routes[sc.Route] {
			t.Errorf("scenario %q targets %q, which is not an S5-8 route", sc.Name, sc.Route)
		}
		if !states[sc.State] {
			t.Errorf("scenario %q selects state %q, which the harness does not serve", sc.Name, sc.State)
		}
	}

	// Every state the harness implements must be exercised by at least one
	// named scenario, or it is dead code masquerading as evidence capability.
	for _, state := range s58HarnessStates {
		used := false
		for _, sc := range s58BrowserScenarios {
			if sc.State == state {
				used = true
				break
			}
		}
		if !used {
			t.Errorf("harness state %q is served but no named scenario uses it", state)
		}
	}
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
	t.Logf("  language: set the %q cookie to en|ru", langCookieName)
	t.Logf("  named scenarios (%d):", len(s58BrowserScenarios))
	for _, sc := range s58BrowserScenarios {
		t.Logf("    %-32s %s", sc.Name, sc.s58URL(base))
		t.Logf("    %-32s   proves: %s", "", sc.Proves)
	}

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

// s58StubRepo fails one named write path and succeeds on the rest. The
// Repository interface is embedded as a nil value on purpose: s58Seed must
// only ever touch the three write methods overridden below, so any other call
// panics loudly instead of being silently tolerated.
type s58StubRepo struct {
	analytics.Repository
	failOn string
}

func (r *s58StubRepo) err(path string) error {
	if r.failOn == path {
		return fmt.Errorf("stub: %s failed", path)
	}
	return nil
}

func (r *s58StubRepo) RecordPoints(string, int, string) error { return r.err("points") }
func (r *s58StubRepo) RecordAnnotation(string, string, string, string) error {
	return r.err("annotation")
}
func (r *s58StubRepo) RecordBet(analytics.BetRecord) error { return r.err("bet") }

// TestS5_8SeedPropagatesEveryRepositoryError proves no fixture write is
// best-effort.
//
// The annotation seeds were written as `_ = repo.RecordAnnotation(...)` while
// the points and bet seeds fatally failed the test. A tombstoned streamer, a
// closed database or a migration drift therefore produced a harness that came
// up perfectly healthy and served a points chart with no annotations on it —
// and the browser evidence run that followed would have reported "annotations
// render" as unreproducible, or worse, been read as passing.
//
// Seeding is one operation: if any write fails, setup fails.
func TestS5_8SeedPropagatesEveryRepositoryError(t *testing.T) {
	for _, path := range []string{"points", "annotation", "bet"} {
		if err := s58Seed(&s58StubRepo{failOn: path}); err == nil {
			t.Errorf("s58Seed swallowed a %s write failure — a partial fixture must never reach browser evidence", path)
		}
	}
	// The control: nothing fails, so seeding reports success.
	if err := s58Seed(&s58StubRepo{}); err != nil {
		t.Errorf("s58Seed on a healthy repository = %v, want nil", err)
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
		"http://127.0.0.1:8978/analytics/points?x=1":          "",
		"http://127.0.0.1:8978/analytics/points?state=":       "",
		"http://127.0.0.1:8978/analytics/points?state=bogus":  "",
		"http://127.0.0.1:8978/analytics/points?state=empty":  "empty",
		"http://127.0.0.1:8978/analytics/points?state=part":   "part",
		"http://127.0.0.1:8978/analytics/points?state=sparse": "sparse",
		"http://127.0.0.1:8978/analytics/roi?state=fail":      "fail",
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

	// SPARSE: exactly one sample and NO breakdown key. Both halves matter — a
	// payload carrying an empty breakdown array would encode "measured nothing"
	// rather than "nothing is measurable", which is the opposite state.
	code, body = get(pointsAPI, page("/analytics/points", "sparse"))
	if code != http.StatusOK {
		t.Errorf("SPARSE points = %d, want 200", code)
	}
	if n := strings.Count(body, `"balance"`); n != 1 {
		t.Errorf("SPARSE points carries %d samples, want exactly 1: %.200s", n, body)
	}
	if strings.Contains(body, `"breakdown"`) {
		t.Errorf("SPARSE points must omit the breakdown key entirely: %.200s", body)
	}

	// FAIL.
	if code, _ = get(pointsAPI, page("/analytics/points", "fail")); code != http.StatusInternalServerError {
		t.Errorf("FAIL points = %d, want 500", code)
	}
	if code, _ = get(roiAPI, page("/analytics/roi", "fail")); code != http.StatusInternalServerError {
		t.Errorf("FAIL roi = %d, want 500", code)
	}
}
