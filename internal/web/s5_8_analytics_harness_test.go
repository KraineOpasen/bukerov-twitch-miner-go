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
	"sync"
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
//
// "stale" and "partready" are SEQUENCED (see s58SequencedStates): they answer
// differently on the first call of a page visit than on the ones the timed poll
// makes, because the states they exist to demonstrate are TRANSITIONS and a
// single fixed response cannot reach either of them.
var s58HarnessStates = []string{"empty", "part", "sparse", "fail", "stale", "partready"}

// s58SequencedStates are the states whose answer depends on WHICH call of a
// page visit is being served. A state that answers the same thing every time
// can only ever demonstrate a resting state; a transition needs two different
// answers in a fixed order:
//
//	stale     — call 1 succeeds (the page retains content), every later call
//	            fails. Only a failed refresh OVER RETAINED CONTENT raises
//	            S-STALE; a state that fails from the start sends the page
//	            terminal instead, which is a different state entirely.
//	partready — call 1 reports the backend row cap (S-PART), every later call
//	            is the real, untruncated payload. Leaving S-PART is the whole
//	            point, and a permanently truncated state can never leave it.
var s58SequencedStates = map[string]bool{"stale": true, "partready": true}

// s58StateFromReferer recovers the selected harness state from an API
// request's Referer. Returns "" for READY.
func s58StateFromReferer(r *http.Request) string {
	got := s58RefererQuery(r, "state")
	for _, s := range s58HarnessStates {
		if got == s {
			return s
		}
	}
	return ""
}

// s58RefererQuery reads one query parameter off an API request's Referer, which
// same-origin requests carry in full under this app's own
// "Referrer-Policy: same-origin" header.
func s58RefererQuery(r *http.Request, key string) string {
	ref := r.Header.Get("Referer")
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	return u.Query().Get(key)
}

// s58Sequencer hands out the call index within one page visit.
//
// Deliberately a value OWNED BY ONE INTERCEPTOR rather than a package variable:
// the Go suite builds several interceptors in one process and go test -count=N
// runs the same test repeatedly, so a package-level counter would leak a
// half-consumed sequence from one test into the next and make the second run
// answer differently from the first. The mutex is not optional either — the
// harness is served by net/http, which runs every request on its own goroutine.
type s58Sequencer struct {
	mu    sync.Mutex
	calls map[string]int
}

// next returns 1 for the first call under key, 2 for the second, and so on.
func (s *s58Sequencer) next(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[key]++
	return s.calls[key]
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
// SPARSE / FAIL / STALE / PARTREADY variants of the two JSON endpoints the
// pages read. READY (and every other request, including every page render)
// falls through untouched, so the evidence is captured against the real handler
// chain.
//
// The sequence counter is created HERE, per interceptor, so it is scoped to one
// harness rather than to the package.
func s58StateInterceptor(next http.Handler) http.Handler {
	seq := &s58Sequencer{calls: map[string]int{}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := s58StateFromReferer(r)
		if state == "" {
			next.ServeHTTP(w, r)
			return
		}
		// Exact match, not prefix: /api/points-history/export and
		// /api/predictions/roi/export are real sibling routes (server.go)
		// that answer a different request. A prefix match would catch them
		// too and let them silently consume a step of a sequenced state
		// meant for the page's own fetch (see
		// TestS5_8ExportSiblingDoesNotAdvanceSequence).
		isPoints := r.URL.Path == "/api/points-history"
		isROI := r.URL.Path == "/api/predictions/roi"
		if !isPoints && !isROI {
			next.ServeHTTP(w, r)
			return
		}

		// Sequenced states count their calls per page VISIT. The visit is
		// identified by the page's own ?seq= value, so navigating again — a new
		// language, a re-run of the same scenario — restarts the sequence
		// instead of resuming a half-consumed one.
		call := 0
		if s58SequencedStates[state] {
			kind := "roi"
			if isPoints {
				kind = "points"
			}
			call = seq.next(state + "|" + kind + "|" + s58RefererQuery(r, "seq"))
		}

		switch state {
		case "stale":
			// A failed TIMED refresh raises S-STALE only over content the page
			// already has, so call 1 must be a real payload and every call after
			// it must fail.
			if !isPoints {
				next.ServeHTTP(w, r)
				return
			}
			if call == 1 {
				next.ServeHTTP(w, r)
				return
			}
			writeInternalError(w, "harness: forced refresh failure")
			return
		case "partready":
			// S-PART first, then the untruncated payload the timed poll gets, so
			// the strip can be watched LEAVING while the content stays.
			if !isPoints {
				next.ServeHTTP(w, r)
				return
			}
			if call == 1 {
				rec := &s58Capture{ResponseWriter: w}
				next.ServeHTTP(rec, r)
				rec.flushWithRawTruncated(w)
				return
			}
			next.ServeHTTP(w, r)
			return
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
	{"points-stale-silent", "/analytics/points", "stale",
		"a first load retains content, then a failed TIMED refresh raises the S-STALE strip over it with zero live-region announcements, no S-FAIL and no failure stamp"},
	{"points-part-silent", "/analytics/points", "part",
		"the S-PART strip re-asserted by a timed poll announces nothing, and every KPI dashes out"},
	{"points-fail-retry-stamp", "/analytics/points", "fail",
		"terminal S-FAIL shows cause + Retry + its own failure time, and each Retry failure re-stamps a strictly later datetime"},
	{"roi-fail-retry-stamp", "/analytics/roi", "fail",
		"the same terminal S-FAIL contract on ROI, including a re-stamp per failed Retry"},
	{"points-fail-timed-silent", "/analytics/points", "fail",
		"a page ALREADY in terminal S-FAIL keeps re-stamping its failure time on every timed poll while announcing nothing, and a user Retry on the same page still announces"},
	{"roi-fail-timed-silent", "/analytics/roi", "fail",
		"the same silent-restamp contract on ROI: timed polls advance the stamp without announcing, user Retry announces"},
	{"points-annotations-recolour", "/analytics/points", "",
		"at least one xaxis annotation is drawn, and the token-backed one changes ink between light and dark"},
	{"points-summary-announces-once", "/analytics/points", "",
		"the C14 summary announces on a user-initiated render and stays silent across timed refreshes"},
	{"points-strips-leave-with-content", "/analytics/points", "partready",
		"the S-PART strip is raised by a truncated load and then LEAVES on the untruncated timed poll, while the content and its real KPIs stay and no failure state appears"},
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

// s58URL builds the loopback URL that produces a scenario.
func (s s58BrowserScenario) s58URL(base string) string {
	return s.s58URLSeq(base, "")
}

// s58URLSeq builds the loopback URL for one VISIT of a scenario. A sequenced
// state answers by call index, so a distinct visit id makes each navigation an
// independent run of the sequence instead of a continuation of the last one.
func (s s58BrowserScenario) s58URLSeq(base, visit string) string {
	u := base + s.Route
	q := url.Values{}
	if s.State != "" {
		q.Set("state", s.State)
	}
	if visit != "" {
		q.Set("seq", visit)
	}
	if len(q) == 0 {
		return u
	}
	return u + "?" + q.Encode()
}

// s58CatalogueURL builds the URL TestS5_8EvidenceHarness prints for one
// scenario. A SEQUENCED scenario gets its own name as the visit id — unique
// and non-empty, since TestS5_8BrowserScenarioCatalogueIsServable already
// requires every scenario name to be both — so the one link the catalogue
// documents is isolated from any other visit ever made to it within this
// harness process, instead of collapsing into the shared, empty-seq
// sequence line (see TestS5_8CatalogueSequencedURLsCarryUniqueSeq). A
// non-sequenced scenario answers the same thing on every call, so it has
// nothing to isolate and keeps the plain URL.
func (s s58BrowserScenario) s58CatalogueURL(base string) string {
	if s58SequencedStates[s.State] {
		return s.s58URLSeq(base, s.Name)
	}
	return s.s58URL(base)
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

// TestS5_8CatalogueSequencedURLsCarryUniqueSeq proves every catalogue URL for
// a SEQUENCED scenario — the one TestS5_8EvidenceHarness actually prints —
// carries its own non-empty seq. Without one, a real browser visit to that
// printed link answers under the SAME sequencer key ("state|kind|", the
// empty-seq line) as any other visit ever made to it within this harness
// process, silently consuming a step of the sequence before a human ever
// sees it.
func TestS5_8CatalogueSequencedURLsCarryUniqueSeq(t *testing.T) {
	seen := map[string]string{}
	sequencedCount := 0
	for _, sc := range s58BrowserScenarios {
		if !s58SequencedStates[sc.State] {
			continue
		}
		sequencedCount++
		raw := sc.s58CatalogueURL("http://127.0.0.1:8978")
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("%s: catalogue URL %q does not parse: %v", sc.Name, raw, err)
		}
		seq := u.Query().Get("seq")
		if seq == "" {
			t.Errorf("%s: printed catalogue URL %s carries no seq — a real visit collapses into the shared, empty-seq sequence line", sc.Name, raw)
			continue
		}
		if prior, ok := seen[seq]; ok {
			t.Errorf("%s and %s share seq %q in their catalogue URLs — their visits are not isolated", sc.Name, prior, seq)
		}
		seen[seq] = sc.Name
	}
	if sequencedCount == 0 {
		t.Fatal("no sequenced scenario in the catalogue — this test would pass vacuously")
	}
}

// s58ScenarioByName looks a catalogue entry up, failing the test if evidence
// cites a scenario that does not exist.
func s58ScenarioByName(t *testing.T, name string) s58BrowserScenario {
	t.Helper()
	for _, sc := range s58BrowserScenarios {
		if sc.Name == name {
			return sc
		}
	}
	t.Fatalf("no browser scenario named %q", name)
	return s58BrowserScenario{}
}

// TestS5_8SequencedScenariosReallyReachTheirState is the SEMANTIC reachability
// check the catalogue was missing.
//
// TestS5_8BrowserScenarioCatalogueIsServable proves a scenario names a real
// route and a state the interceptor understands. That is syntax. It says
// nothing about whether driving that URL can ever produce the state the
// scenario CLAIMS to prove, and two entries could not:
//
//   - points-stale-silent selected "fail", which fails the very first request.
//     S-STALE is only reachable over RETAINED content — with nothing ever
//     loaded the page goes terminal instead, so the scenario could only ever
//     have shown S-FAIL while claiming to show S-STALE.
//   - points-strips-leave-with-content selected "part", which reports the row
//     cap on every request. The page can therefore never LEAVE S-PART, which is
//     precisely the transition the scenario is named for.
//
// Both now select a SEQUENCED state, and this test drives the sequence for
// real: it issues the requests a page visit would issue and asserts what each
// one answers, so a scenario that silently stops reaching its state fails here
// rather than in a browser run nobody re-reads.
func TestS5_8SequencedScenariosReallyReachTheirState(t *testing.T) {
	srv := buildF3PageServer(t)
	s58SeedFixtures(t, srv.analytics.Repository())
	h := s58StateInterceptor(srv.handler())

	// call issues the request the PAGE would issue, carrying the Referer that
	// selects the scenario's state. visit distinguishes one page visit from the
	// next, so a sequence always starts from its first step.
	call := func(sc s58BrowserScenario, visit string) (int, string) {
		req := s58GET("/api/points-history?streamer=streamer_a&range=24h")
		req.Header.Set("Referer", "http://127.0.0.1:8978"+sc.s58URLSeq("", visit))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	t.Run("points-stale-silent", func(t *testing.T) {
		sc := s58ScenarioByName(t, "points-stale-silent")

		// Step 1 — the load that gives the page something to retain. Without a
		// working first response there is no content to go stale.
		code, body := call(sc, "visit-1")
		if code != http.StatusOK || !strings.Contains(body, `"balance"`) {
			t.Fatalf("first response = %d, body=%.160s; S-STALE needs a SUCCESSFUL first load or the page goes terminal instead", code, body)
		}

		// Step 2 — the timed refresh that fails over that retained content. This
		// is the only thing that raises S-STALE.
		if code, body = call(sc, "visit-1"); code != http.StatusInternalServerError {
			t.Fatalf("second response = %d, body=%.160s; the timed refresh must FAIL or the strip never appears", code, body)
		}

		// A fresh page visit restarts the sequence, so the scenario is
		// repeatable within one harness process instead of being a one-shot.
		if code, _ = call(sc, "visit-2"); code != http.StatusOK {
			t.Fatalf("a new page visit started at step %d, not step 1 — the sequence must be scoped per visit", code)
		}
	})

	t.Run("points-strips-leave-with-content", func(t *testing.T) {
		sc := s58ScenarioByName(t, "points-strips-leave-with-content")

		// Step 1 — the truncated load that raises S-PART.
		code, body := call(sc, "visit-1")
		if code != http.StatusOK || !strings.Contains(body, `"rawTruncated":true`) {
			t.Fatalf("first response = %d, body=%.200s; the strip must be RAISED before it can be proven to leave", code, body)
		}

		// Step 2 — the timed poll that is no longer truncated. The strip must be
		// able to go while the page STAYS on content: still 200, still carrying
		// samples, and no longer reporting the cap.
		code, body = call(sc, "visit-1")
		if code != http.StatusOK {
			t.Fatalf("second response = %d — leaving S-PART must not take the page into S-FAIL", code)
		}
		if strings.Contains(body, `"rawTruncated":true`) {
			t.Fatalf("second response still reports the row cap, so the page can never leave S-PART: %.200s", body)
		}
		if !strings.Contains(body, `"balance"`) {
			t.Fatalf("second response carries no samples, so the strip would leave WITH the content instead of on its own: %.200s", body)
		}

		if code, body = call(sc, "visit-2"); code != http.StatusOK || !strings.Contains(body, `"rawTruncated":true`) {
			t.Fatalf("a new page visit did not restart at the truncated step: %d %.200s", code, body)
		}
	})
}

// TestS5_8SequencedStateIsScopedToItsInterceptor proves the sequence counter is
// per-instance, not package state: two harnesses built in the same process do
// not consume each other's steps, so one test can never leave another mid-way
// through a sequence (and -count=N stays deterministic).
func TestS5_8SequencedStateIsScopedToItsInterceptor(t *testing.T) {
	srv := buildF3PageServer(t)
	s58SeedFixtures(t, srv.analytics.Repository())

	first := func(h http.Handler) int {
		req := s58GET("/api/points-history?streamer=streamer_a&range=24h")
		req.Header.Set("Referer", "http://127.0.0.1:8978/analytics/points?state=stale&seq=shared")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	a := s58StateInterceptor(srv.handler())
	b := s58StateInterceptor(srv.handler())
	if code := first(a); code != http.StatusOK {
		t.Fatalf("interceptor A step 1 = %d, want 200", code)
	}
	if code := first(b); code != http.StatusOK {
		t.Fatalf("interceptor B step 1 = %d, want 200 — B inherited A's position, so the counter is shared state", code)
	}
	if code := first(a); code != http.StatusInternalServerError {
		t.Fatalf("interceptor A step 2 = %d, want 500", code)
	}
}

// TestS5_8SequencedStateIsRaceSafe drives one sequenced state concurrently and
// proves each step is handed out exactly once: the harness is served by
// net/http, which runs every request on its own goroutine.
func TestS5_8SequencedStateIsRaceSafe(t *testing.T) {
	srv := buildF3PageServer(t)
	s58SeedFixtures(t, srv.analytics.Repository())
	h := s58StateInterceptor(srv.handler())

	const n = 24
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := s58GET("/api/points-history?streamer=streamer_a&range=24h")
			req.Header.Set("Referer", "http://127.0.0.1:8978/analytics/points?state=stale&seq=race")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		}
	}
	if ok != 1 {
		t.Errorf("%d of %d concurrent requests got step 1, want exactly 1 — the counter is not serialized", ok, n)
	}
}

// TestS5_8ExportSiblingDoesNotAdvanceSequence proves /api/points-history/export
// — a real, registered route (server.go) that shares a path PREFIX with
// /api/points-history but answers a different request — can never consume a
// step of a sequenced state meant for the page's own fetch. A prefix match
// on the interceptor's route check would let it.
func TestS5_8ExportSiblingDoesNotAdvanceSequence(t *testing.T) {
	srv := buildF3PageServer(t)
	s58SeedFixtures(t, srv.analytics.Repository())
	h := s58StateInterceptor(srv.handler())

	const referer = "http://127.0.0.1:8978/analytics/points?state=stale&seq=export-probe"
	hitExport := func() {
		req := s58GET("/api/points-history/export?streamer=streamer_a&range=24h")
		req.Header.Set("Referer", referer)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	hitReal := func() (int, string) {
		req := s58GET("/api/points-history?streamer=streamer_a&range=24h")
		req.Header.Set("Referer", referer)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	// Hit the export sibling first, several times over. If the interceptor's
	// route match still catches it, each of these silently consumes one step
	// of the "stale" sequence under this same seq id.
	for i := 0; i < 3; i++ {
		hitExport()
	}

	// The PAGE's own first call must still land on step 1 (success) — proving
	// the export hits above never advanced the counter.
	code, body := hitReal()
	if code != http.StatusOK || !strings.Contains(body, `"balance"`) {
		t.Fatalf("real points-history first call = %d, body=%.160s; export siblings must not consume sequence state", code, body)
	}
}

// s58HarnessGeneration is any value >1. s58ReportHarnessRunning publishes it
// before StatusRunning so the client's firstBoot discriminator
// ((status.generation || 1) <= 1, base.html) reads false: every provider
// behind this harness is a deterministic fake already serving on the first
// request, so there is no real boot sequence for the one-shot
// reload-on-running to catch up with, and that reload's actual effect — a
// second document load that can silently consume the NEXT sequenced
// response before a human ever sees the first (see s58Sequencer) — would
// make S-STALE read as terminal S-FAIL and leave S-PART unreachable.
const s58HarnessGeneration = 2

// s58ReportHarnessRunning publishes the running status the evidence harness
// serves for its whole lifetime. Generation goes out FIRST — mirroring
// SetGeneration's own contract of preceding the generation's Run — so no
// subscriber ever observes StatusRunning still paired with generation<=1.
func s58ReportHarnessRunning(b *StatusBroadcaster) {
	b.SetGeneration(s58HarnessGeneration)
	b.SetStatus(StatusRunning, "")
}

// TestS5_8EvidenceHarnessReportsPastFirstBoot proves s58ReportHarnessRunning
// — what TestS5_8EvidenceHarness actually calls — leaves the client's
// firstBoot discriminator false, so base.html's one-shot reload-on-running
// (design v6 §10 rule 4) never fires against this fixture. Runs
// unconditionally: it only touches a bare *StatusBroadcaster, never binds a
// port, so it needs no MINER_S5_8_HARNESS gate.
func TestS5_8EvidenceHarnessReportsPastFirstBoot(t *testing.T) {
	b := NewStatusBroadcaster()
	s58ReportHarnessRunning(b)
	got := b.GetStatus()
	if got.Status != StatusRunning {
		t.Fatalf("status = %q, want %q", got.Status, StatusRunning)
	}
	// Mirrors base.html verbatim: const firstBoot = (status.generation || 1) <= 1.
	if got.Generation <= 1 {
		t.Fatalf("generation = %d, want >1 — the client reads (generation || 1) <= 1 as firstBoot and reloads the page", got.Generation)
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
	// Report "running" PAST firstBoot so the status overlay never covers the
	// pages AND base.html's one-shot reload-on-running never fires: every
	// provider is a deterministic fake, so there is no startup sequence to
	// reflect, and letting the reload through would silently burn a step of
	// whatever sequenced state the visited scenario selected before a human
	// ever saw it (see s58ReportHarnessRunning).
	s58ReportHarnessRunning(srv.GetStatusBroadcaster())

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
		t.Logf("    %-32s %s", sc.Name, sc.s58CatalogueURL(base))
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
