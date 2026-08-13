package web

// Task S5-8: the Analytics group's two direct-render routes (7-8).
//
//   - /analytics/points (route 7) — canonical points-history page.
//   - /analytics/roi    (route 8) — canonical prediction-ROI page.
//
// Both replace their former /analytics/* -> /statistics compatibility
// redirects (compatibilityRedirects shrinks 3 -> 1, leaving only /help), and
// both are pure presentation over the EXISTING, unchanged JSON endpoints
// (/api/points-history, /api/predictions/roi and their exports). No new
// viewmodel, API, provider or transport is introduced; legacy /statistics and
// its template stay exactly as they were.
//
// This file is the source-of-truth regression pin for the owner decisions:
// direct routes (never redirects) with /analytics itself still 404; the
// Analytics nav becoming a parent + two children (5 -> 6 parents, 23 -> 25
// children) while the top-level section count stays at seven and exactly one
// destination carries aria-current; C14 pairing every visual chart with a
// localized assistive-technology summary AND a data table AND a client-side
// CSV; Points requiring a concrete streamer (no "All streamers" facet); ROI
// showing eight KPIs and three C4 breakdown tables while staying strictly
// read-only; S-FAIL carrying role="alert" with a Retry control; no-data
// rendering an em dash rather than a fabricated zero; RU/EN parity; and the
// existing AnalyticsSettings.Refresh cadence being reused verbatim.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
)

// s58Routes are the two direct-render routes this slice adds.
var s58Routes = []string{"/analytics/points", "/analytics/roi"}

// s58ReadTemplate reads a page/component template out of the embedded FS.
// Deliberately templatesFS rather than os.ReadFile: buildF3PageServer calls
// t.Chdir into a temp dir, so a relative on-disk path would not resolve.
func s58ReadTemplate(t *testing.T, name string) string {
	t.Helper()
	b, err := templatesFS.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// ---------------------------------------------------------------------
// 1. Direct routes, method contract, and the /analytics root staying 404.
// ---------------------------------------------------------------------

// TestS5_8DirectRoutesGetHeadOK proves both routes render directly with 200
// on GET and HEAD — never a 30x. A redirect (the pre-S5-8 behavior) fails
// here, which is the mutation probe for "restore the analytics redirect".
func TestS5_8DirectRoutesGetHeadOK(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	for _, route := range s58Routes {
		t.Run(route, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(method, route, nil))
				if rec.Code != http.StatusOK {
					t.Errorf("%s %s = %d, want 200 direct (never a redirect)", method, route, rec.Code)
				}
				if loc := rec.Header().Get("Location"); loc != "" {
					t.Errorf("%s %s set Location=%q — must render directly, not redirect", method, route, loc)
				}
			}
		})
	}
}

// TestS5_8DirectRoutesRejectOtherMethods proves both routes are GET/HEAD-only
// and answer 405 with an exact Allow header for everything else — the same
// explicit method gating S5-6's category routes established.
func TestS5_8DirectRoutesRejectOtherMethods(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	for _, route := range s58Routes {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, route, nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405", method, route, rec.Code)
			}
			if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
				t.Errorf("%s %s Allow = %q, want %q", method, route, allow, "GET, HEAD")
			}
		}
	}
}

// TestS5_8AnalyticsRootStays404 proves /analytics itself is never registered:
// it falls through to the "/" catch-all and 404s, exactly like any other
// unbuilt path. Registering it (a "helpful" index or redirect) is a mutation
// this test rejects.
func TestS5_8AnalyticsRootStays404(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	for _, path := range []string{"/analytics", "/analytics/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (the Analytics root is not a page)", path, rec.Code)
		}
	}
}

// TestS5_8RedirectMapShrunkToOne proves compatibilityRedirects lost exactly
// the two /analytics/* entries (3 -> 1) and now holds only the unrelated
// /help entry — and that the survivor still 302s to its unchanged target.
func TestS5_8RedirectMapShrunkToOne(t *testing.T) {
	if len(compatibilityRedirects) != 1 {
		t.Fatalf("len(compatibilityRedirects) = %d, want 1 (only /help survives S5-8)", len(compatibilityRedirects))
	}
	for _, route := range s58Routes {
		if target, ok := compatibilityRedirects[route]; ok {
			t.Errorf("compatibilityRedirects must no longer contain %q (still maps to %q)", route, target)
		}
	}
	if target, ok := compatibilityRedirects["/help"]; !ok || target != "/help/getting-started" {
		t.Errorf("compatibilityRedirects[/help] = %q,%v; want %q,true", target, ok, "/help/getting-started")
	}

	srv := buildF3PageServer(t)
	h := srv.handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/help", nil))
	if rec.Code != http.StatusFound {
		t.Errorf("GET /help = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/help/getting-started" {
		t.Errorf("GET /help Location = %q, want %q", loc, "/help/getting-started")
	}
}

// ---------------------------------------------------------------------
// 2. Nav: Analytics becomes the sixth group; seven sections survive.
// ---------------------------------------------------------------------

// TestS5_8NavAnalyticsGroup proves the Analytics nav entry is now a group of
// the same shape as Overview/Drops/Events/Settings/System: one parent link
// (href = first child, the established convention) plus exactly two children
// with their own distinct hrefs, taking the totals from 5 -> 6 parents and
// 23 -> 25 children. Removing the parent or either child fails here.
func TestS5_8NavAnalyticsGroup(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview", "en")

	if n := strings.Count(body, `data-nav-parent>`); n != 6 {
		t.Errorf("expected exactly six data-nav-parent groups (Overview, Drops, Analytics, Events, Settings, System), found %d", n)
	}
	if n := strings.Count(body, `data-nav-child>`); n != 25 {
		t.Errorf("expected exactly twenty-five data-nav-child destinations, found %d", n)
	}
	if !strings.Contains(body, `href="/analytics/points" class="c2-nav-link" data-nav-section="analytics"`) {
		t.Error("Analytics parent link must point at its first child (/analytics/points)")
	}
	for _, href := range s58Routes {
		want := `href="` + href + `" class="c2-nav-child" data-nav-section="analytics" data-nav-child`
		if !strings.Contains(body, want) {
			t.Errorf("Analytics group missing child destination %q", href)
		}
	}
	// The legacy page is no longer linked from the nav (exactly like /health,
	// /logs and /settings after S5-5/S5-6) — but it must still render.
	if strings.Contains(body, `href="/statistics" class="c2-nav-link"`) {
		t.Error("the C2 nav must no longer link /statistics — Analytics now points at its two canonical children")
	}
}

// TestS5_8SevenSectionsSurvive proves promoting Analytics to a group did not
// change the top-level section count: still exactly seven, in both languages.
func TestS5_8SevenSectionsSurvive(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, lang := range []string{"en", "ru"} {
		for _, route := range s58Routes {
			body := f3GetPage(t, srv, route, lang)
			s5_2AssertSevenSections(t, lang, body)
		}
	}
}

// s58NavAnchorRe matches every rendered C2 nav destination anchor.
var s58NavAnchorRe = regexp.MustCompile(`<a href="([^"]*)" class="c2-nav-(?:link|child)"[^>]*>`)

// TestS5_8ExactlyOneAriaCurrentDestination proves the nav-activation contract
// still yields at most one aria-current="page" destination on the two new
// routes: children are disambiguated by href, and a group's own parent link
// is only ever visually highlighted. Evaluated in Go against the same rules
// base.html's script implements, since the template ships no server-rendered
// aria-current.
func TestS5_8ExactlyOneAriaCurrentDestination(t *testing.T) {
	srv := buildF3PageServer(t)

	for _, path := range s58Routes {
		body := f3GetPage(t, srv, path, "en")

		// The section rule base.html applies for these paths.
		if !strings.Contains(body, `p.indexOf('/analytics/') === 0`) {
			t.Fatal("base.html nav activation must still resolve /analytics/* to the analytics section")
		}

		current := []string{}
		for _, m := range s58NavAnchorRe.FindAllStringSubmatch(body, -1) {
			tag, href := m[0], m[1]
			isParent := strings.Contains(tag, "data-nav-parent")
			isChild := strings.Contains(tag, "data-nav-child")
			sectionMatches := strings.Contains(tag, `data-nav-section="analytics"`)
			isCurrent := sectionMatches
			if isChild {
				isCurrent = sectionMatches && href == path
			}
			if !isParent && isCurrent {
				current = append(current, href)
			}
		}
		if len(current) != 1 {
			t.Errorf("path %s: expected exactly one aria-current destination, got %d: %v", path, len(current), current)
		} else if current[0] != path {
			t.Errorf("path %s: aria-current landed on %q, want the page's own destination", path, current[0])
		}
	}
}

// ---------------------------------------------------------------------
// 3. C14: every visual chart is paired with an AT summary, a data table
//    and a client-generated CSV.
// ---------------------------------------------------------------------

// TestS5_8C14IsOneSharedComponent proves the chart block is a genuinely shared
// component rather than markup copy-pasted into each page: the two pages'
// rendered C14 blocks are byte-identical once each one's own id prefix is
// normalized away. Copy-paste drifts; a shared component cannot.
//
// Asserted on rendered output. The previous version read the component's source
// and looked for `{{define "c14.chart"}}`, which proves only that the string is
// in the file — it would pass just as happily if neither page used it.
func TestS5_8C14IsOneSharedComponent(t *testing.T) {
	srv := buildF3PageServer(t)

	normalized := map[string]string{}
	for route, prefix := range map[string]string{
		"/analytics/points": "ap-c14",
		"/analytics/roi":    "ar-c14",
	} {
		block, ok := s58DivSubtreeAt(f3GetPage(t, srv, route, "en"), `data-c14-block="`+prefix+`"`)
		if !ok {
			t.Fatalf("%s: no C14 block rendered", route)
		}
		normalized[route] = strings.ReplaceAll(block, prefix, "c14")
	}
	if normalized["/analytics/points"] != normalized["/analytics/roi"] {
		t.Errorf("the two pages render DIFFERENT C14 chart hosts — the component is not actually shared:\n points: %s\n roi:    %s",
			normalized["/analytics/points"], normalized["/analytics/roi"])
	}
}

// TestS5_8ChartsPairVisualWithSummaryAndTable proves both rendered pages
// carry, for every chart they show, the C14 triple: the chart host, a
// localized assistive-technology summary region, and a data table. Removing
// the summary or the table is a mutation this test rejects.
func TestS5_8ChartsPairVisualWithSummaryAndTable(t *testing.T) {
	srv := buildF3PageServer(t)

	for _, route := range s58Routes {
		for _, lang := range []string{"en", "ru"} {
			body := f3GetPage(t, srv, route, lang)
			if !strings.Contains(body, `data-c14-chart`) {
				t.Errorf("[%s %s] no C14 chart host rendered", lang, route)
			}
			nChart := strings.Count(body, `data-c14-chart`)
			nSummary := strings.Count(body, `data-c14-summary`)
			nTable := strings.Count(body, `data-c14-table`)
			if nSummary != nChart || nTable != nChart {
				t.Errorf("[%s %s] C14 triple mismatch: %d charts, %d summaries, %d tables — every chart needs both",
					lang, route, nChart, nSummary, nTable)
			}
			// The summary is announceable on a user-initiated render, not
			// decorative text. Scoped to the summary's own tag: the shared
			// c17.toast_stack renders role="status" on EVERY page, so a
			// page-wide search would pass even with the summary's role deleted.
			block, ok := s58DivSubtreeAt(body, `data-c14-block=`)
			if !ok {
				t.Errorf("[%s %s] no C14 block rendered", lang, route)
				continue
			}
			if !strings.Contains(block, `role="status"`) {
				t.Errorf("[%s %s] the C14 summary must carry role=\"status\": %s", lang, route, block)
			}
			// The data table is a real semantic table with a caption.
			if !strings.Contains(block, `<caption`) {
				t.Errorf("[%s %s] the C14 data table must carry a <caption>", lang, route)
			}
		}
	}
}

// TestS5_8CSVAddsNoServerEndpoint proves the CSV affordance is rendered on both
// pages and that this slice introduced no server-side CSV route: the download
// is produced in the browser from JSON the page already holds, so there is no
// second source of truth and no new backend contract.
//
// The two pre-existing JSON export endpoints are asserted to still answer,
// unchanged. What the CSV actually CONTAINS — ISO-8601 UTC timestamps, quote
// doubling, comma and CR/LF quoting, and the =/+/-/@ formula-injection guard —
// is client-side behavior and is proven by the named browser scenarios
// "points-csv" and "roi-csv"; a Go assertion that the string "toISOString()"
// appears in a template would pass over a script that never runs.
func TestS5_8CSVAddsNoServerEndpoint(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	for _, route := range s58Routes {
		for _, lang := range []string{"en", "ru"} {
			if body := f3GetPage(t, srv, route, lang); !strings.Contains(body, "data-c14-csv") {
				t.Errorf("[%s %s] no CSV control rendered", lang, route)
			}
		}
	}

	// The existing export contracts still answer exactly as before.
	for _, path := range []string{
		"/api/points-history/export?streamer=streamer_a&range=24h",
		"/api/predictions/roi/export?period=30d",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, s58GET(path))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (the existing export contract must be untouched)", path, rec.Code)
		}
	}

	// No new server-side CSV route was invented for the C14 control.
	for _, path := range []string{
		"/api/points-history/csv", "/api/predictions/roi/csv",
		"/analytics/points/export.csv", "/analytics/roi/export.csv",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, s58GET(path))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 — the CSV is built in the browser, not served", path, rec.Code)
		}
	}
}

// ---------------------------------------------------------------------
// 4. Points (route 7): a concrete streamer, no invented data.
// ---------------------------------------------------------------------

// TestS5_8PointsRequiresConcreteStreamer proves the Points page never offers
// an "All streamers" facet: the series is per-streamer and the backend
// requires a streamer parameter, so an aggregate option would either 400 or
// invite a fabricated cross-streamer series. Adding the option is a mutation
// this test rejects.
func TestS5_8PointsRequiresConcreteStreamer(t *testing.T) {
	srv := buildF3PageServer(t)

	// The backend fact the page is obeying: an empty streamer is rejected, so
	// an aggregate facet could only ever be served by inventing a
	// cross-streamer series in the browser.
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, s58GET("/api/points-history?streamer=&range=24h"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("GET points-history with an empty streamer = %d, want 400 — the no-aggregate rule rests on this", rec.Code)
	}

	// The fixture roster is non-empty, so the selector must offer only real
	// streamers: an empty-valued <option> IS the aggregate facet.
	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/analytics/points", lang)
		start := strings.Index(body, `id="ap-streamer"`)
		if start < 0 {
			t.Fatalf("[%s] Points page has no streamer selector", lang)
		}
		sel := body[start:]
		if end := strings.Index(sel, "</select>"); end > 0 {
			sel = sel[:end]
		}
		if strings.Contains(sel, `<option value=""`) {
			t.Errorf("[%s] Points streamer selector carries an empty-valued (All streamers) option: %s", lang, sel)
		}
		if !strings.Contains(sel, `value="streamer_a"`) {
			t.Errorf("[%s] Points streamer selector missing the concrete roster entries", lang)
		}
	}
}

// TestS5_8PointsSeriesIsExactlyWhatWasRecorded proves the series the page draws
// is the series the miner actually recorded: every sample the endpoint returns
// corresponds to a stored observation, with nothing invented between them.
//
// Asserted at the served-data seam by comparing the endpoint's response against
// the repository's own rows. Whether the CHART then smooths, interpolates or
// zero-fills those samples is client rendering, proven by the named browser
// scenario "points-straight-no-interpolation".
//
// Repeat-safe: compares set membership against the stored rows rather than
// asserting a row count, so accumulation across -count runs cannot break it.
func TestS5_8PointsSeriesIsExactlyWhatWasRecorded(t *testing.T) {
	srv := buildF3PageServer(t)
	s58SeedFixtures(t, srv.analytics.Repository())

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, s58GET("/api/points-history?streamer=streamer_a&range=24h"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET points-history = %d, want 200", rec.Code)
	}
	var got analytics.PointsHistory
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode points-history: %v", err)
	}
	if len(got.Points) < 2 {
		t.Fatalf("served series has %d samples, want a real series", len(got.Points))
	}

	stored, err := srv.analytics.Repository().GetPointSamples("streamer_a", time.Now().Add(-24*time.Hour), time.Now(), maxHistoryRows)
	if err != nil {
		t.Fatalf("read stored samples: %v", err)
	}
	// Keyed on the (timestamp, balance) PAIR, never on the timestamp alone:
	// RecordPoints stamps whole milliseconds, so several observations routinely
	// share one T. A timestamp-keyed map would silently keep the last of them
	// and report every other real sample as invented.
	type sample struct {
		t       int64
		balance int
	}
	recorded := make(map[sample]bool, len(stored))
	for _, s := range stored {
		recorded[sample{s.T, s.Balance}] = true
	}
	for _, p := range got.Points {
		if !recorded[sample{p.T, p.Balance}] {
			t.Errorf("served sample (t=%d, balance=%d) was never recorded — the endpoint invented a point", p.T, p.Balance)
		}
	}
}

// ---------------------------------------------------------------------
// 5. ROI (route 8): eight KPIs, three tables, strictly read-only.
// ---------------------------------------------------------------------

// s58ROIKPIs are the eight KPI tiles the ROI page must render, keyed by the
// element id each one binds to.
var s58ROIKPIs = []string{
	"ar-kpi-count", "ar-kpi-winrate", "ar-kpi-net", "ar-kpi-roi",
	"ar-kpi-wagered", "ar-kpi-avgwager", "ar-kpi-avgwin", "ar-kpi-drawdown",
}

// TestS5_8ROIHasEightKPIsAndThreeTables proves the ROI page renders exactly
// the eight KPI tiles and the three C4 breakdown tables (byStreamer,
// byStrategy, byOddsBucket) the owner mandated — no more, no fewer.
func TestS5_8ROIHasEightKPIsAndThreeTables(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/analytics/roi", "en")

	for _, id := range s58ROIKPIs {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("ROI page missing KPI tile %q", id)
		}
	}
	if n := strings.Count(body, "stat-tile"); n != len(s58ROIKPIs) {
		t.Errorf("ROI page renders %d KPI tiles, want exactly %d", n, len(s58ROIKPIs))
	}
	for _, id := range []string{"ar-by-streamer", "ar-by-strategy", "ar-by-odds"} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("ROI page missing breakdown table %q", id)
		}
	}
	// The three tables are the C4 component's tables, not bespoke markup.
	if n := strings.Count(body, "c4-table"); n < 3 {
		t.Errorf("ROI breakdowns must use the C4 table styling, found %d occurrences", n)
	}
}

// TestS5_8ROISummaryCarriesEveryFigureThePageShows proves the ROI page never
// NEEDS to reconstruct a financial outcome: every KPI and every breakdown
// column it presents already exists as a field of the authoritative
// /api/predictions/roi response, so a second client-side computation — a second
// source of truth that can silently disagree with the miner's own ledger —
// would be gratuitous.
//
// Asserted against the real response over seeded bets, not against template
// text: this pins the CONTRACT the page depends on, which is what would
// actually break if the endpoint ever dropped a field.
func TestS5_8ROISummaryCarriesEveryFigureThePageShows(t *testing.T) {
	srv := buildF3PageServer(t)
	s58SeedFixtures(t, srv.analytics.Repository())

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, s58GET("/api/predictions/roi?period=30d"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET predictions/roi = %d, want 200", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode roi summary: %v", err)
	}

	// The eight KPI tiles and the outcome donut, each a straight field read.
	for _, field := range []string{
		"count", "winRate", "netProfit", "roi", "totalWagered", "avgWager", "avgWin", "maxDrawdown",
		"wins", "losses", "refunds",
	} {
		if _, ok := raw[field]; !ok {
			t.Errorf("authoritative ROI summary has no %q — the page would have to reconstruct it", field)
		}
	}

	// The three C4 breakdown tables, each with the columns the page renders.
	for _, field := range []string{"byStreamer", "byStrategy", "byOddsBucket"} {
		body, ok := raw[field]
		if !ok {
			t.Errorf("authoritative ROI summary has no %q breakdown", field)
			continue
		}
		var rows []map[string]json.RawMessage
		if err := json.Unmarshal(body, &rows); err != nil {
			t.Errorf("%s is not a list of rows: %v", field, err)
			continue
		}
		if len(rows) == 0 {
			t.Errorf("%s came back empty — the seeded bets must produce real breakdown rows", field)
			continue
		}
		for _, col := range []string{"key", "count", "netProfit", "roi"} {
			if _, ok := rows[0][col]; !ok {
				t.Errorf("%s rows have no %q column, but the page renders it", field, col)
			}
		}
	}
}

// TestS5_8ROIIsStrictlyReadOnly proves route 8 ships no mutation affordance:
// no bet/skip control, no form, no non-GET fetch, and no reference to any
// state-changing endpoint. Betting behavior is owned by /settings/predictions,
// which the page links to instead. Adding a mutation control fails here.
// Asserted on the RENDERED page (a mutation affordance the browser never
// receives cannot be used) plus the route's own method contract. That every
// request the page ISSUES is a GET is client behavior, proven by the named
// browser scenario "roi-readonly-get-only".
func TestS5_8ROIIsStrictlyReadOnly(t *testing.T) {
	srv := buildF3PageServer(t)

	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/analytics/roi", lang)
		own, ok := s58PageOwnRegion(body)
		if !ok {
			t.Fatalf("[%s] could not isolate the ROI page's own rendered region", lang)
		}
		for _, banned := range []string{
			"/api/prediction/bet", "/api/prediction/skip",
			"<form", "hx-post", "hx-put", "hx-delete",
		} {
			if strings.Contains(own, banned) {
				t.Errorf("[%s] ROI page must be strictly read-only — rendered a mutation affordance %q", lang, banned)
			}
		}
		// The read-only stance is stated to the operator, and the owner of the
		// behavior is linked rather than duplicated. Matched on the page's OWN
		// anchor, never on the C2 nav's /settings/predictions child, which every
		// page carries and which would make this assertion vacuous.
		if !strings.Contains(body, `data-ar-owner-link href="/settings/predictions"`) {
			t.Errorf("[%s] ROI page must carry its own owner link pointing at /settings/predictions", lang)
		}
	}

	// The route itself refuses every state-changing method.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := httptest.NewRecorder()
		srv.handler().ServeHTTP(rec, httptest.NewRequest(method, "/analytics/roi", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /analytics/roi = %d, want 405 — route 8 is read-only", method, rec.Code)
		}
	}
}

// TestS5_8BackendContractsUntouched proves this slice changed no backend
// contract: the four existing JSON endpoints still answer exactly as before,
// and the two pages call those same paths.
func TestS5_8BackendContractsUntouched(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	for _, path := range []string{
		"/api/points-history?streamer=streamer_a&range=24h",
		"/api/points-history/export?streamer=streamer_a&range=24h",
		"/api/predictions/roi?period=30d",
		"/api/predictions/roi/export?period=30d",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (existing contract must be untouched)", path, rec.Code)
		}
	}

	// Each page ships pointing at the existing endpoint — asserted on what the
	// browser actually receives.
	for route, endpoint := range map[string]string{
		"/analytics/points": "/api/points-history",
		"/analytics/roi":    "/api/predictions/roi",
	} {
		if body := f3GetPage(t, srv, route, "en"); !strings.Contains(body, endpoint) {
			t.Errorf("rendered %s does not reference the existing %s endpoint", route, endpoint)
		}
	}
}

// ---------------------------------------------------------------------
// 6. State honesty: S-FAIL alerts with Retry, and no fabricated zeros.
// ---------------------------------------------------------------------

// TestS5_8FailStatesAreAlertsWithRetry proves each page's failure state is an
// inline role="alert" carrying an explicit Retry control. Dropping role="alert"
// is a mutation this test rejects.
//
// Scoped to the S-FAIL block itself: a page-wide search for role="alert" would
// be satisfied by any alert anywhere in the chrome. That failures render inline
// rather than as a toast is observable behavior, proven by the named browser
// scenarios "points-fail-retry-stamp" and "roi-fail-retry-stamp".
func TestS5_8FailStatesAreAlertsWithRetry(t *testing.T) {
	srv := buildF3PageServer(t)

	for route, r := range s58FailureRegions {
		for _, lang := range []string{"en", "ru"} {
			block, ok := s58DivSubtree(f3GetPage(t, srv, route, lang), r.block)
			if !ok {
				t.Errorf("[%s %s] rendered page has no S-FAIL block", lang, route)
				continue
			}
			if !strings.Contains(block, `role="alert"`) {
				t.Errorf("[%s %s] the S-FAIL block must carry role=\"alert\": %s", lang, route, block)
			}
			if !strings.Contains(block, `id="`+r.retry+`"`) {
				t.Errorf("[%s %s] the S-FAIL block must offer a Retry control", lang, route)
			}
		}
	}
}

// s58ClassAttrRe extracts every class="..." value from a template.
var s58ClassAttrRe = regexp.MustCompile(`class="([^"]*)"`)

// TestS5_8HiddenActuallyHides is a cascade regression pin, caught in browser
// evidence: the compiled stylesheet emits `.c1-block{display:flex}` AFTER
// `.hidden{display:none}`, and both are single-class selectors, so an element
// carrying BOTH classes loses to .c1-block and stays visible. A page that put
// `hidden` directly on a state block would therefore render its stale, partial,
// empty and failure blocks all at once, on top of a working chart.
//
// The rule these pages follow instead: the element JS toggles is a plain
// wrapper, and the .c1-block lives inside it. This test pins that no element
// anywhere in the two pages combines the two classes.
func TestS5_8HiddenActuallyHides(t *testing.T) {
	// Guard the premise: if the stylesheet order ever changes, this test's
	// reason to exist changes with it.
	css := readEmbeddedStatic(t, "static/css/app.css")
	hiddenAt := strings.Index(css, ".hidden{display:none}")
	blockAt := strings.Index(css, ".c1-block{")
	if hiddenAt < 0 || blockAt < 0 {
		t.Fatalf("could not locate the .hidden / .c1-block rules in app.css (%d, %d)", hiddenAt, blockAt)
	}
	if blockAt < hiddenAt {
		t.Skip(".c1-block now precedes .hidden in the cascade; the conflict this test guards no longer applies")
	}

	for _, name := range []string{
		"templates/analytics_points.html",
		"templates/analytics_roi.html",
		"templates/components/c14_chart.html",
	} {
		src := s58ReadTemplate(t, name)
		for _, m := range s58ClassAttrRe.FindAllStringSubmatch(src, -1) {
			classes := strings.Fields(m[1])
			var hasHidden, hasBlock bool
			for _, c := range classes {
				if c == "hidden" {
					hasHidden = true
				}
				if c == "c1-block" {
					hasBlock = true
				}
			}
			if hasHidden && hasBlock {
				t.Errorf("%s: class=%q combines .hidden with .c1-block — .c1-block's display:flex wins the cascade, so this element would never hide", name, m[1])
			}
		}
	}
}

// TestS5_8StateRegionsStartHidden proves every non-default state region on both
// pages ships hidden, so a freshly served page shows the loading skeleton
// alone — never a failure or empty block stacked on top of real content.
func TestS5_8StateRegionsStartHidden(t *testing.T) {
	srv := buildF3PageServer(t)
	regions := map[string][]string{
		"/analytics/points": {"ap-stale", "ap-partial", "ap-empty", "ap-error", "ap-content"},
		"/analytics/roi":    {"ar-stale", "ar-empty", "ar-error", "ar-content"},
	}

	for route, ids := range regions {
		body := f3GetPage(t, srv, route, "en")
		for _, id := range ids {
			idx := strings.Index(body, `id="`+id+`"`)
			if idx < 0 {
				t.Errorf("%s: state region %q not rendered", route, id)
				continue
			}
			// Inspect the opening tag that carries the id.
			start := strings.LastIndex(body[:idx], "<")
			end := strings.Index(body[idx:], ">")
			if start < 0 || end < 0 {
				t.Errorf("%s: could not isolate the tag for %q", route, id)
				continue
			}
			tag := body[start : idx+end+1]
			if !strings.Contains(tag, "hidden") {
				t.Errorf("%s: state region %q does not start hidden: %s", route, id, tag)
			}
		}
	}
}

// TestS5_8NoDataLabelIsLocalizedAndShipped proves the accessible no-data label
// the em dash carries actually reaches the browser, localized, on both pages —
// the label is what tells a screen reader that a dash means "not measured"
// rather than being read as punctuation or, worse, silence.
//
// WHICH values dash out is a client-side decision made against a live payload,
// so it is proven by the named browser scenarios "points-sparse-dashes" (a
// single-sample window dashes net change, earned and events instead of painting
// a fabricated 0) and "points-part-silent" (the row cap dashes every KPI).
func TestS5_8NoDataLabelIsLocalizedAndShipped(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	srv := buildF3PageServer(t)

	for _, route := range s58Routes {
		seen := map[string]string{}
		for _, lang := range []string{"en", "ru"} {
			label := loc.T(lang, "analytics.no_data")
			if label == "" || label == "analytics.no_data" {
				t.Fatalf("[%s] the shared no-data label is not translated", lang)
			}
			body := f3GetPage(t, srv, route, lang)
			if !strings.Contains(body, label) {
				t.Errorf("[%s %s] the localized no-data label %q never reaches the browser, so a dash would be unlabelled", lang, route, label)
			}
			if !strings.Contains(body, "—") {
				t.Errorf("[%s %s] the em-dash no-data glyph is not shipped", lang, route)
			}
			seen[lang] = label
		}
		if seen["en"] == seen["ru"] {
			t.Errorf("%s: the no-data label is identical in EN and RU (%q) — it is not actually localized", route, seen["en"])
		}
	}
}

// ---------------------------------------------------------------------
// 7. Cadence, localization and legacy parity.
// ---------------------------------------------------------------------

// TestS5_8ReusesExistingRefreshCadence proves both pages derive their refresh
// interval from the existing AnalyticsSettings.Refresh value threaded through
// RefreshMinutes — never a hardcoded cadence, and never a new polling, SSE,
// WebSocket or manual-refresh contract. Hardcoding a cadence fails here.
func TestS5_8ReusesExistingRefreshCadence(t *testing.T) {
	// The cadence is genuinely data-driven: the fixture's Refresh value must
	// reach the rendered page. Matched loosely because html/template pads a
	// numeric substitution with spaces in a JS context. A hardcoded cadence
	// would not track the fixture and fails here.
	srv := buildF3PageServer(t)
	cadence := regexp.MustCompile(`REFRESH_MS\s*=\s*5\s*\*\s*60\s*\*\s*1000`)
	for _, route := range s58Routes {
		body := f3GetPage(t, srv, route, "en")
		if !cadence.MatchString(body) {
			t.Errorf("rendered %s did not carry the fixture's Refresh=5 cadence", route)
		}
		// No new polling, streaming or push transport reaches the browser from
		// the PAGE. Scoped to the page's own region: base.html's chrome has
		// carried the global miner-status EventSource since long before this
		// slice, and a page-wide scan would be flagging that instead.
		own, ok := s58PageOwnRegion(body)
		if !ok {
			t.Fatalf("could not isolate %s's own rendered region", route)
		}
		for _, banned := range []string{"EventSource(", "WebSocket(", `hx-trigger="every`, "/api/miner-status/stream"} {
			if strings.Contains(own, banned) {
				t.Errorf("rendered %s introduces a new %q transport", route, banned)
			}
		}
	}
}

// TestS5_8LocalizationParity proves both pages render fully localized in RU
// and EN, with no raw i18n key leaking into the output and no untranslated
// difference between the two.
func TestS5_8LocalizationParity(t *testing.T) {
	srv := buildF3PageServer(t)
	rawKey := regexp.MustCompile(`>\s*(analytics|stat|js)\.[a-z0-9_.]+\s*<`)

	for _, route := range s58Routes {
		bodies := map[string]string{}
		for _, lang := range []string{"en", "ru"} {
			body := f3GetPage(t, srv, route, lang)
			if m := rawKey.FindString(body); m != "" {
				t.Errorf("[%s %s] raw i18n key leaked into the page: %q", lang, route, strings.TrimSpace(m))
			}
			bodies[lang] = body
		}
		if bodies["en"] == bodies["ru"] {
			t.Errorf("%s: EN and RU renders are byte-identical — the page is not actually localized", route)
		}
	}
}

// TestS5_8LegacyStatisticsUntouched proves this slice edited neither
// statistics.html nor help.html: both templates are pinned to their exact
// pre-S5-8 content hash. Editing statistics.html — explicitly forbidden, since
// the canonical pages were split along its client seams rather than by
// modifying it — fails here.
func TestS5_8LegacyStatisticsUntouched(t *testing.T) {
	pinned := map[string]string{
		"templates/statistics.html": "f842e1e335b339ee89eff87a7026c8c610c810a39d97da80a4b9f660882087a3",
		"templates/help.html":       "0aefd78068f22a6ec716acdf7aa5e77137a8c6dd01b05c6179f40d56bc769830",
	}
	for name, want := range pinned {
		sum := sha256.Sum256([]byte(s58ReadTemplate(t, name)))
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s was modified (sha256 %s, want %s) — S5-8 must leave it byte-identical", name, got, want)
		}
	}
}

// TestS5_8LegacyStatisticsStillRendersDirectly proves /statistics keeps
// rendering directly (200, never redirected) and still carries both of its
// own client sections, unchanged — the canonical pages are additive, they
// never disable the legacy page.
func TestS5_8LegacyStatisticsStillRendersDirectly(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/statistics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /statistics = %d, want 200 direct", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("GET /statistics set Location=%q — the legacy page must never be redirected", loc)
	}
	body := rec.Body.String()
	for _, want := range []string{`id="stat-chart"`, `id="roi-content"`, "/api/points-history", "/api/predictions/roi"} {
		if !strings.Contains(body, want) {
			t.Errorf("/statistics lost its own content %q", want)
		}
	}
}

// ---------------------------------------------------------------------
// 8. Q3 corrective pass — regressions found by independent review.
//
// Each test below pins a defect that the pre-existing suite could not see,
// because its assertions were scoped to functions the defect did not live in.
// ---------------------------------------------------------------------

// TestS5_8IndeterminateWindowIsDistinguishableFromZero pins the DATA half of
// Q3 MAJOR-1: the false-zero the Points KPIs must not paint is only avoidable
// because the payload can tell "nothing is measurable" apart from "we measured
// nothing", and this proves it still can.
//
// /api/points-history omits "breakdown" entirely (`json:"breakdown,omitempty"`)
// for BOTH facts: BreakdownFromSamples returns nil when the raw window holds
// fewer than two samples — no delta EXISTS to attribute — and an empty slice
// when the window genuinely earned nothing. The two are separable without any
// new backend field, from chartDownsampled plus the served series length. If
// the endpoint ever stopped emitting chartDownsampled, or started sending an
// empty breakdown array for the indeterminate case, the distinction would
// collapse and a fabricated 0 would become unavoidable.
//
// Which tiles then dash out is client rendering, proven by the named browser
// scenario "points-sparse-dashes".
func TestS5_8IndeterminateWindowIsDistinguishableFromZero(t *testing.T) {
	srv := buildF3PageServer(t)
	s58SeedFixtures(t, srv.analytics.Repository())
	h := s58StateInterceptor(srv.handler())

	get := func(referer string) map[string]json.RawMessage {
		t.Helper()
		req := s58GET("/api/points-history?streamer=streamer_a&range=24h")
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("points-history = %d, want 200", rec.Code)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("decode points-history: %v", err)
		}
		return raw
	}

	// The completeness signal the disambiguation rests on is always present.
	ready := get("")
	if _, ok := ready["chartDownsampled"]; !ok {
		t.Error("the payload no longer carries chartDownsampled — an indeterminate window becomes indistinguishable from a measured zero")
	}
	if _, ok := ready["rawTruncated"]; !ok {
		t.Error("the payload no longer carries rawTruncated")
	}

	// A single-sample window: breakdown is ABSENT (not an empty array), and the
	// series is not downsampled, so the page can establish that fewer than two
	// raw samples existed and nothing is attributable.
	sparse := get("http://127.0.0.1:8978/analytics/points?state=sparse")
	if _, ok := sparse["breakdown"]; ok {
		t.Error("a single-sample window must OMIT breakdown; an empty array would encode a measured zero instead")
	}
	var points []analytics.PointSample
	if err := json.Unmarshal(sparse["points"], &points); err != nil {
		t.Fatalf("decode sparse points: %v", err)
	}
	if len(points) != 1 {
		t.Errorf("the sparse window served %d samples, want exactly 1", len(points))
	}
	if string(sparse["chartDownsampled"]) != "false" {
		t.Errorf("chartDownsampled = %s on a one-sample window, want false — otherwise the raw series cannot be proven short", sparse["chartDownsampled"])
	}
}

// TestS5_8SummaryNeverAnnouncesOnTimedRefresh pins Q3 MINOR-1.
//
// The C14 summary carries role="status", whose IMPLICIT live value is polite,
// so rewriting its text on the timed refresh made a screen reader re-read the
// same sentence every AnalyticsSettings.Refresh minutes. Measured in a browser
// (page interval compressed): five refreshes produced five announcements of
// identical text. Handoff §9 "routine SSE/poll refreshes never announce" and
// §10 "timed polls ... never announce".
func TestS5_8SummaryNeverAnnouncesOnTimedRefresh(t *testing.T) {
	// Asserted on the RENDERED page, not on template text: what reaches the
	// browser is what decides whether the region speaks. A summary that ships
	// live would announce on the very first timed refresh.
	srv := buildF3PageServer(t)
	for _, tc := range []struct{ route, id string }{
		{"/analytics/points", "ap-c14-summary"},
		{"/analytics/roi", "ar-c14-summary"},
	} {
		for _, lang := range []string{"en", "ru"} {
			tag, ok := s58TagWithID(f3GetPage(t, srv, tc.route, lang), tc.id)
			if !ok {
				t.Errorf("[%s %s] no C14 summary rendered", lang, tc.route)
				continue
			}
			if !strings.Contains(tag, `aria-live="off"`) {
				t.Errorf("[%s %s] the C14 summary must render aria-live=\"off\"; role=\"status\" alone is implicitly polite: %s", lang, tc.route, tag)
			}
			if !strings.Contains(tag, `role="status"`) {
				t.Errorf("[%s %s] the C14 summary must keep role=\"status\" so a user-initiated render can still announce: %s", lang, tc.route, tag)
			}
		}
	}
	// That the region then goes polite for a USER-initiated render and stays
	// off across every timed refresh is client behavior — an aria-live value
	// rewritten per render, which no static read of the shipped markup can
	// observe. It is proven by the named browser scenario
	// "points-summary-announces-once".
}

// Q3 MINOR-2 — the S-PART and S-STALE strips leaving with the content region
// they describe — is a state-transition behavior inside each page's show():
// after a failed load the page used to show "part of the data is unavailable"
// above a failure block displaying no data at all. A transition cannot be
// observed in shipped markup, so it is proven by the named browser scenario
// "points-strips-leave-with-content". What the Go suite still pins here is the
// markup those strips must ship as: hidden, plain wrappers, and (see
// TestS5_8TimedPollStripsAreNeverLiveRegions) never live regions.

// TestS5_8StripsStartHiddenAndArePlainWrappers re-asserts, after the show()
// change, that the strips are still plain wrappers (never .c1-block carrying
// .hidden — see TestS5_8HiddenActuallyHides) and still ship hidden. Without
// this, the MINOR-2 fix could be "satisfied" by an element that never hid.
func TestS5_8StripsStartHiddenAndArePlainWrappers(t *testing.T) {
	srv := buildF3PageServer(t)
	for route, ids := range map[string][]string{
		"/analytics/points": {"ap-partial", "ap-stale"},
		"/analytics/roi":    {"ar-stale"},
	} {
		body := f3GetPage(t, srv, route, "en")
		for _, id := range ids {
			tag, ok := s58TagWithID(body, id)
			if !ok {
				t.Errorf("%s: strip %q not rendered", route, id)
				continue
			}
			classes := s58ClassSet(tag)
			// Exact class-token membership, never a substring of the whole
			// tag: `strings.Contains(tag, "hidden")` would also be satisfied
			// by an unrelated attribute (data-hidden="false") or by a class
			// that merely contains the word (not-hidden), so it could pass
			// over an element that renders perfectly visible.
			if !classes["hidden"] {
				t.Errorf("%s: strip %q must ship with the `hidden` CLASS, got class set %v in %s", route, id, s58SortedKeys(classes), tag)
			}
			if classes["c1-block"] {
				t.Errorf("%s: strip %q must be a plain wrapper, not the .c1-block itself: %s", route, id, tag)
			}
		}
	}
}

// s58TagWithID returns the opening tag carrying id="<id>".
func s58TagWithID(body, id string) (string, bool) {
	idx := strings.Index(body, `id="`+id+`"`)
	if idx < 0 {
		return "", false
	}
	start := strings.LastIndex(body[:idx], "<")
	end := strings.Index(body[idx:], ">")
	if start < 0 || end < 0 {
		return "", false
	}
	return body[start : idx+end+1], true
}

// s58ClassSet parses an opening tag's class attribute into a token set, so
// membership is checked exactly rather than by substring.
func s58ClassSet(tag string) map[string]bool {
	out := map[string]bool{}
	m := s58ClassAttrRe.FindStringSubmatch(tag)
	if m == nil {
		return out
	}
	for _, c := range strings.Fields(m[1]) {
		out[c] = true
	}
	return out
}

func s58SortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestS5_8NewTemplatesUseSemanticTokensOnly pins Q3 MINOR-3. Handoff §5: new
// and migrated templates use semantic utilities exclusively, and F1's legacy
// re-pointed aliases are deleted in S5-10 once a grep proves zero template
// references. Two fresh `border-neutral-700` references would have made that
// grep fail.
func TestS5_8NewTemplatesUseSemanticTokensOnly(t *testing.T) {
	legacy := regexp.MustCompile(`\b(?:bg|text|border|ring|from|to|via)-(?:neutral|purple|gray|slate|zinc)-\d{2,3}\b`)
	primitive := regexp.MustCompile(`--prim-[a-z0-9-]+`)

	for _, name := range []string{
		"templates/analytics_points.html",
		"templates/analytics_roi.html",
		"templates/components/c14_chart.html",
	} {
		src := s58ReadTemplate(t, name)
		if m := legacy.FindAllString(src, -1); len(m) > 0 {
			t.Errorf("%s: uses F1 legacy alias(es) %v — new templates take semantic utilities/tokens only (§5)", name, m)
		}
		if m := primitive.FindAllString(src, -1); len(m) > 0 {
			t.Errorf("%s: references primitive token(s) %v — primitives never appear in templates (§4)", name, m)
		}
	}
}

// TestS5_8ROIMarksG4AsInterpretation pins Q3 MINOR-4. Handoff §17 requires the
// open gap Г4 (the ROI "three tables" composition) to be marked [INT] in a
// template comment. This slice ships byStreamer/byStrategy/byOddsBucket rather
// than the report's стример/стратегия/исход, so the marker is what keeps the
// substitution visible as an unresolved interpretation instead of silently
// reading as settled parity.
func TestS5_8ROIMarksG4AsInterpretation(t *testing.T) {
	src := s58ReadTemplate(t, "templates/analytics_roi.html")
	for _, want := range []string{"[INT]", "Г4"} {
		if !strings.Contains(src, want) {
			t.Errorf("the ROI template comment must carry the %s marker for the open table-composition gap (§17)", want)
		}
	}
	// The marker has to sit in a template comment, not in rendered output.
	srv := buildF3PageServer(t)
	for _, lang := range []string{"en", "ru"} {
		if body := f3GetPage(t, srv, "/analytics/roi", lang); strings.Contains(body, "[INT]") {
			t.Errorf("[%s] the Г4 marker leaked into the rendered page — it is a template comment", lang)
		}
	}
}

// ---------------------------------------------------------------------
// 9. Corrective pass 3 — defects the re-Q3 review found in pass 2.
//
// Every assertion below is a RENDERED, SERVED or PARSED invariant: what the
// browser actually receives, what the real handler actually answers, or what
// the test sources structurally are. Client-IIFE behavior is not asserted from
// template text here — it is proven by the named localhost browser scenarios
// catalogued in s5_8_analytics_harness_test.go.
// ---------------------------------------------------------------------

// s58CautionStrips are the content-scoped caution strips each page can reveal
// WITHOUT user action: the S-STALE strip is raised only by a failed timed
// refresh, and S-PART is re-asserted on every timed poll that reports the
// backend row cap.
var s58CautionStrips = map[string][]string{
	"/analytics/points": {"ap-stale", "ap-partial"},
	"/analytics/roi":    {"ar-stale"},
}

// s58ImplicitLiveRoles are the ARIA roles that make an element a live region
// by IMPLICATION — each one carries a non-off implicit aria-live value, so
// merely revealing the element speaks.
var s58ImplicitLiveRoles = []string{"status", "alert", "log", "marquee", "timer"}

// TestS5_8TimedPollStripsAreNeverLiveRegions pins corrective-pass-3 defect 1.
//
// Both caution strips are revealed by the TIMED POLL, never by a user action:
// S-STALE only ever appears from a failed background refresh, and S-PART is
// re-toggled from every poll's rawTruncated. Carrying role="status" made them
// IMPLICIT live regions (role=status's implicit aria-live is "polite"), so a
// screen reader announced the strip the moment the poll revealed it — exactly
// the "timed polls never announce" rule (§9/§10) the C14 summary already obeys.
//
// They must therefore be plain content: no implicit live role, and an EXPLICIT
// aria-live="off" so a later role addition cannot silently make them speak
// again. Asserted on the rendered tag in both languages, because what reaches
// the browser is what decides whether the region talks.
func TestS5_8TimedPollStripsAreNeverLiveRegions(t *testing.T) {
	srv := buildF3PageServer(t)

	for route, ids := range s58CautionStrips {
		for _, lang := range []string{"en", "ru"} {
			body := f3GetPage(t, srv, route, lang)
			for _, id := range ids {
				tag, ok := s58TagWithID(body, id)
				if !ok {
					t.Errorf("[%s %s] caution strip %q not rendered", lang, route, id)
					continue
				}
				for _, role := range s58ImplicitLiveRoles {
					if strings.Contains(tag, `role="`+role+`"`) {
						t.Errorf("[%s %s] strip %q carries role=%q, an IMPLICIT live region — the timed poll that reveals it would announce: %s",
							lang, route, id, role, tag)
					}
				}
				if !strings.Contains(tag, `aria-live="off"`) {
					t.Errorf("[%s %s] strip %q must render an explicit aria-live=\"off\" so a poll reveal stays silent: %s",
						lang, route, id, tag)
				}
			}
		}
	}
}

// s58FailureRegions maps each page to its S-FAIL block, that block's dedicated
// failure-time element, its Retry control, its cause element, and the
// LAST-SUCCESS clock that must never stand in for a failure timestamp.
var s58FailureRegions = map[string]struct {
	block, failTime, retry, cause, lastSuccess string
}{
	"/analytics/points": {"ap-error", "ap-error-time", "ap-retry", "ap-error-msg", "ap-updated"},
	"/analytics/roi":    {"ar-error", "ar-error-time", "ar-retry", "ar-error-msg", "ar-updated"},
}

// TestS5_8TerminalFailureCarriesItsOwnFreshTimestamp pins corrective-pass-3
// defect 2.
//
// A terminal (user-visible) S-FAIL showed a cause and a Retry, but no time at
// all: the only clock on screen was the LAST-SUCCESS one, which a failure
// leaves frozen at the last good load. An operator reading "Updated 10:04:11"
// above a failure block cannot tell whether the failure just happened or has
// been there for an hour.
//
// Each page therefore renders a dedicated failure-time element INSIDE its own
// role="alert" block, as a <time> carrying a machine-readable datetime so the
// stamp is provably re-written (millisecond resolution) on each terminal
// failure rather than reused. The last-success clock stays where it is, OUTSIDE
// the failure block — it reports a different fact.
func TestS5_8TerminalFailureCarriesItsOwnFreshTimestamp(t *testing.T) {
	srv := buildF3PageServer(t)

	for route, r := range s58FailureRegions {
		for _, lang := range []string{"en", "ru"} {
			body := f3GetPage(t, srv, route, lang)

			block, ok := s58DivSubtree(body, r.block)
			if !ok {
				t.Errorf("[%s %s] S-FAIL block %q not rendered", lang, route, r.block)
				continue
			}
			if !strings.Contains(block, `role="alert"`) {
				t.Errorf("[%s %s] the S-FAIL block must stay a role=\"alert\" region", lang, route)
			}

			// Cause and Retry keep sharing the alert region with the stamp, so
			// one announcement carries all three.
			for _, id := range []string{r.cause, r.retry} {
				if !strings.Contains(block, `id="`+id+`"`) {
					t.Errorf("[%s %s] S-FAIL block is missing %q — a failure must state its cause and offer Retry", lang, route, id)
				}
			}

			// The failure stamp lives inside the alert block, not beside it.
			if !strings.Contains(block, `id="`+r.failTime+`"`) {
				t.Errorf("[%s %s] S-FAIL block has no dedicated failure-time element %q — the page can only be showing the last-success clock",
					lang, route, r.failTime)
				continue
			}
			tag, ok := s58TagWithID(block, r.failTime)
			if !ok {
				t.Errorf("[%s %s] could not isolate %q", lang, route, r.failTime)
				continue
			}
			if !strings.HasPrefix(tag, "<time") {
				t.Errorf("[%s %s] %q must be a <time> element so the stamp is machine-readable: %s", lang, route, r.failTime, tag)
			}
			if !strings.Contains(tag, "datetime=") {
				t.Errorf("[%s %s] %q must carry a datetime attribute, the millisecond-resolution proof that each terminal failure re-stamps: %s",
					lang, route, r.failTime, tag)
			}

			// The last-success clock must NOT be inside the failure block — a
			// failure that reused it would be reporting the time of the last
			// thing that WORKED.
			if strings.Contains(block, `id="`+r.lastSuccess+`"`) {
				t.Errorf("[%s %s] the last-success clock %q is inside the S-FAIL block — a failure must never present it as its own timestamp",
					lang, route, r.lastSuccess)
			}
			if !strings.Contains(body, `id="`+r.lastSuccess+`"`) {
				t.Errorf("[%s %s] the last-success clock %q disappeared from the page", lang, route, r.lastSuccess)
			}
		}
	}
}

// TestS5_8TerminalFailureIsSilentUntilExplicitlyAnnounced pins the STATIC half
// of the timed-S-FAIL defect.
//
// role="alert" carries an implicit aria-live of "assertive", so ANY mutation of
// the block's contents while it is on screen is spoken. A page already sitting
// in terminal S-FAIL keeps polling, and every failed poll rewrote the cause and
// the failure stamp inside that block — so a screen-reader user was interrupted
// with the same failure once per refresh cadence, forever, without ever having
// asked for anything. §9/§10: a timed poll never announces.
//
// The container is therefore SILENT BY DEFAULT — an explicit aria-live="off"
// overrides the role's implicit assertive — and the page's own setter raises it
// to "assertive" for the renders that are allowed to speak (initial load,
// Retry, any other user-initiated attempt) BEFORE it mutates anything. Written
// explicitly in the markup rather than only from script, so a page whose script
// never runs cannot announce either, and so the default cannot be lost by an
// edit that only touches the JS.
//
// The runtime half — that a TIMED poll really passes announce=false while a
// Retry really passes true — is client-IIFE behavior and is proven by the named
// browser scenarios, not by reading template text.
func TestS5_8TerminalFailureIsSilentUntilExplicitlyAnnounced(t *testing.T) {
	srv := buildF3PageServer(t)

	for route, r := range s58FailureRegions {
		for _, lang := range []string{"en", "ru"} {
			body := f3GetPage(t, srv, route, lang)
			tag, ok := s58TagWithID(body, r.block)
			if !ok {
				t.Errorf("[%s %s] S-FAIL block %q not rendered", lang, route, r.block)
				continue
			}
			if !strings.Contains(tag, `role="alert"`) {
				t.Errorf("[%s %s] %q must stay a role=\"alert\" region: %s", lang, route, r.block, tag)
			}
			if !strings.Contains(tag, `aria-live="off"`) {
				t.Errorf("[%s %s] %q must render an explicit aria-live=\"off\" default; without it role=\"alert\" is implicitly assertive and every failed TIMED poll re-announces the same failure: %s",
					lang, route, r.block, tag)
			}
		}
	}
}

// s58DivSubtree returns the full outer HTML of the <div> carrying id="<id>".
func s58DivSubtree(body, id string) (string, bool) {
	return s58DivSubtreeAt(body, `id="`+id+`"`)
}

// s58DivSubtreeAt returns the full outer HTML of the <div> whose opening tag
// contains marker, found by balancing <div>/</div> from that tag. Used to scope
// an assertion to one region instead of to the whole page, where an unrelated
// element elsewhere would satisfy it.
func s58DivSubtreeAt(body, marker string) (string, bool) {
	idx := strings.Index(body, marker)
	if idx < 0 {
		return "", false
	}
	start := strings.LastIndex(body[:idx], "<")
	if start < 0 || !strings.HasPrefix(body[start:], "<div") {
		return "", false
	}
	depth, i := 0, start
	for i < len(body) {
		next := strings.IndexAny(body[i:], "<")
		if next < 0 {
			return "", false
		}
		i += next
		switch {
		case strings.HasPrefix(body[i:], "<div"):
			depth++
			i += len("<div")
		case strings.HasPrefix(body[i:], "</div>"):
			depth--
			i += len("</div>")
			if depth == 0 {
				return body[start:i], true
			}
		default:
			i++
		}
	}
	return "", false
}

// s58PageOwnRegion narrows a rendered page to the part this slice OWNS: the
// content block plus the page's own {{block "scripts"}} output. It starts at
// the #main-content anchor and ends where base.html's trailing chrome script
// begins.
//
// Necessary because base.html legitimately ships things a page must not: an
// hx-post language switcher and the global miner-status EventSource. A
// page-wide scan for those tokens reports the chrome, on every page, forever —
// an assertion that can only ever be a false positive is worse than none.
// The end marker is an IDENTIFIER from base.html's chrome script, not a
// comment in it: html/template strips JS comments while rendering a <script>,
// so a comment-based delimiter silently never matches.
func s58PageOwnRegion(body string) (string, bool) {
	start := strings.Index(body, `id="main-content"`)
	// base.html's own trailing nav-activation script.
	end := strings.Index(body, "SECTION_RULES")
	if start < 0 || end < 0 || end <= start {
		return "", false
	}
	return body[start:end], true
}

// s58JSTCallKey matches a client-side t('key') call in a rendered page.
var s58JSTCallKey = regexp.MustCompile(`\bt\(\s*'([a-zA-Z0-9_.]+)'`)

// TestS5_8RenderedScriptKeysResolveInTheClientCatalog proves every message key
// the two pages' scripts resolve at runtime actually exists in window.I18N for
// every supported language.
//
// window.I18N is populated from i18n.JSMessages, which carries ONLY "js."-
// prefixed keys; any other key silently renders as the literal key string in
// the browser, with no compile-time and no template-render-time signal. This is
// a rendered + catalog invariant, so it covers whatever keys the pages use —
// including the failure-stamp label — without naming any of them.
func TestS5_8RenderedScriptKeysResolveInTheClientCatalog(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	srv := buildF3PageServer(t)

	for _, route := range s58Routes {
		body := f3GetPage(t, srv, route, "en")
		matches := s58JSTCallKey.FindAllStringSubmatch(body, -1)
		if len(matches) == 0 {
			t.Fatalf("%s: no client-side t(...) calls found in the rendered page — the pattern is stale", route)
		}
		seen := map[string]bool{}
		for _, m := range matches {
			seen[m[1]] = true
		}
		for key := range seen {
			if !strings.HasPrefix(key, "js.") {
				t.Errorf("%s: script resolves t(%q), which is not \"js.\"-prefixed — window.I18N only carries js.* keys, so it renders as the literal key", route, key)
				continue
			}
			for _, lang := range i18n.SupportedLangs() {
				if _, ok := loc.JSMessages(lang)[key]; !ok {
					t.Errorf("%s [%s]: client catalog has no %q — the browser would print the raw key", route, lang, key)
				}
			}
		}
	}
}

// TestS5_8SeededAnnotationsAreRenderableEvidence pins corrective-pass-3
// defect 3, at the SERVED-DATA seam: what the real /api/points-history handler
// hands the chart.
//
// The harness seeded its annotations AFTER the whole points loop, so every
// annotation timestamp sat strictly to the right of the last sample. ApexCharts
// clips an xaxis annotation outside the series' x-range, so the browser drew
// none of them — while an API-level "we returned three annotations" check
// passed happily. Counting the API rows is therefore NOT evidence that an
// annotation is visible.
//
// The invariant: every annotation streamer_a serves falls inside the x-range of
// the series served alongside it, and at least one is TOKEN-backed (empty
// colour), so its ink comes from --chart-series-1 and demonstrably recolours
// with the theme instead of being frozen to a seeded hex.
//
// Repeat-safe: database.Open is a process-wide singleton, so streamer_a's rows
// accumulate across -count runs. Every assertion here is therefore a
// containment or a "at least one" property, never an exact row count.
func TestS5_8SeededAnnotationsAreRenderableEvidence(t *testing.T) {
	srv := buildF3PageServer(t)
	s58SeedFixtures(t, srv.analytics.Repository())

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, s58GET("/api/points-history?streamer=streamer_a&range=24h"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET points-history = %d, want 200", rec.Code)
	}
	var got analytics.PointsHistory
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode points-history: %v", err)
	}

	if len(got.Points) < 2 {
		t.Fatalf("seeded series has %d samples; the fixture must draw a real chart", len(got.Points))
	}
	if len(got.Annotations) == 0 {
		t.Fatal("seeded fixtures produced no annotations — RecordAnnotation failures are being swallowed")
	}

	first, last := got.Points[0].T, got.Points[len(got.Points)-1].T
	for _, a := range got.Annotations {
		if a.T < first || a.T > last {
			t.Errorf("annotation %q/%q at t=%d falls outside the served chart x-range [%d, %d] — ApexCharts clips it, so the browser renders nothing",
				a.Type, a.Reason, a.T, first, last)
		}
	}

	tokenBacked := 0
	for _, a := range got.Annotations {
		if a.Color == "" {
			tokenBacked++
		}
	}
	if tokenBacked == 0 {
		t.Error("every seeded annotation carries a hardcoded colour — at least one must be token-backed (empty colour) so the browser can prove annotation ink recolours from --chart-series-1")
	}
}

// s58TemplateSourceReaders is the allow-list of S5-8 tests that may read
// template SOURCE. Each reads it for a STATIC property of the file — a content
// hash, a class token, a style token, a comment marker — never to infer what
// the client script does at runtime.
var s58TemplateSourceReaders = map[string]string{
	"s58ReadTemplate":                           "the reader primitive itself; its CALLERS are what this list governs",
	"TestS5_8LegacyStatisticsUntouched":         "pins two templates to a content hash",
	"TestS5_8HiddenActuallyHides":               "parses class attributes for exact class-token membership",
	"TestS5_8NewTemplatesUseSemanticTokensOnly": "lints for legacy alias / primitive style tokens",
	"TestS5_8ROIMarksG4AsInterpretation":        "checks for the Г4 [INT] template comment",
}

// TestS5_8TestsPinBehaviorAtRealSeamsNotInTemplateText pins corrective-pass-3
// defect 4, structurally.
//
// An assertion like `strings.Contains(src, "toISOString()")` proves only that a
// string appears in a file. It passes over a script that never runs, over a
// helper whose only caller ignores it, and over any behavior the browser
// actually exhibits — while reading like a behavioral guarantee. Every such
// assertion is a false witness, and the pass-2 suite was full of them.
//
// The rule this enforces: S5-8's Go tests pin what the SERVER answers, what the
// page RENDERS, and what the SEEDED DATA is. Client-IIFE behavior is proven by
// the named localhost browser scenarios in the harness file. Reading template
// source is allowed only for the static-property checks listed above.
//
// Enforced over the AST rather than by substring, so the guard cannot match its
// own text and a rename cannot slip past it.
func TestS5_8TestsPinBehaviorAtRealSeamsNotInTemplateText(t *testing.T) {
	fset := token.NewFileSet()
	for _, name := range []string{"s5_8_analytics_test.go", "s5_8_analytics_harness_test.go"} {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			// A JS-function-body extractor exists for exactly one purpose:
			// asserting on client-script implementation text.
			if fn.Name.Name == "s58FuncBody" {
				t.Errorf("%s: %s extracts a client JS function body — client-IIFE behavior belongs to named browser evidence, not to a Go substring assertion",
					name, fn.Name.Name)
			}
		}
		for _, v := range s58TemplateSourceViolations(file) {
			t.Errorf("%s: %s %s. Only the static-property checks %v may read template source; pin this at the rendered page, the served API, or named browser evidence instead",
				name, v.Func, v.Reason, s58SortedKeys(s58AllowedReaderSet()))
		}
	}
}

// s58ReadViolation names one function that reaches template source outside the
// allow-list, and how.
type s58ReadViolation struct {
	Func   string
	Reason string
}

// s58TemplateSourceViolations reports every function in file that reads
// template source without being on the s58TemplateSourceReaders allow-list.
//
// Three routes reach those bytes and all three are covered:
//
//	s58ReadTemplate        — the package's own reader primitive
//	templatesFS.ReadFile   — the embedded FS underneath it
//	os.ReadFile            — the same bytes straight off disk
//
// os.ReadFile is judged by its ARGUMENT, allow-list style rather than
// deny-list: a bare "*.go" literal with no directory is the package's own
// source and is fine, and everything else — a template path, or any computed
// expression, which is the shape every wrapper takes — is a read this guard
// cannot prove is safe, so it is reported. Judging the argument rather than the
// callee is what makes hiding the read inside a helper useless: the helper is
// itself a function in this file, so the helper is what gets named.
//
// It is a decision over the AST rather than a substring scan, so the guard
// cannot match its own text and a rename cannot slip past it.
func s58TemplateSourceViolations(file *ast.File) []s58ReadViolation {
	var out []s58ReadViolation
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if _, allowed := s58TemplateSourceReaders[fn.Name.Name]; allowed {
			continue
		}
		reported := map[string]bool{}
		report := func(reason string) {
			if reported[reason] {
				return
			}
			reported[reason] = true
			out = append(out, s58ReadViolation{Func: fn.Name.Name, Reason: reason})
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := call.Fun.(type) {
			case *ast.Ident:
				if f.Name == "s58ReadTemplate" {
					report("reads template source")
				}
			case *ast.SelectorExpr:
				id, ok := f.X.(*ast.Ident)
				if !ok {
					return true
				}
				if id.Name == "templatesFS" && f.Sel.Name == "ReadFile" {
					report("reads template source directly from templatesFS, bypassing the allow-list")
				}
				if id.Name == "os" && f.Sel.Name == "ReadFile" && !s58IsOwnGoSourceArg(call) {
					report("calls os.ReadFile on a path that is not this package's own Go source, which reads template bytes straight off disk and bypasses the allow-list")
				}
			}
			return true
		})
	}
	return out
}

// s58IsOwnGoSourceArg reports whether a call's single argument is a literal
// naming a Go file in this package directory — the only os.ReadFile target
// these two files have a reason to touch. A path with a directory, a non-Go
// extension, or any non-literal expression is not one.
func s58IsOwnGoSourceArg(call *ast.CallExpr) bool {
	if len(call.Args) != 1 {
		return false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	path, err := strconv.Unquote(lit.Value)
	if err != nil {
		return false
	}
	return strings.HasSuffix(path, ".go") && !strings.ContainsAny(path, `/\`)
}

// s58AllowedReaderSet adapts the allow-list to the shared key-sorting helper.
func s58AllowedReaderSet() map[string]bool {
	out := make(map[string]bool, len(s58TemplateSourceReaders))
	for k := range s58TemplateSourceReaders {
		out[k] = true
	}
	return out
}

// TestS5_8TemplateSourceGuardHasNoOSReadFileBypass is the focused regression
// pin for the guard itself.
//
// The guard knew two ways to reach template source — s58ReadTemplate and
// templatesFS.ReadFile — and neither is the only one. os.ReadFile reads the
// very same bytes off disk and went straight past the allow-list, so the whole
// policy could be reinstated verbatim by swapping one call. A wrapper made it
// worse: hide the read behind a helper and even a future name-based check would
// miss it.
//
// Closed narrowly. Inside these two files os.ReadFile may read only the
// package's OWN Go source — a bare "*.go" literal, no directory — which is what
// TestS5_8HarnessDeadlineBeatsDocumentedTimeout legitimately does when it
// checks its own usage comment. Any other path, and any COMPUTED path (a
// variable, a concatenation, a parameter — the shapes a wrapper uses), is a
// read the guard cannot prove is not template source, so it is prohibited and
// the function performing it is named. That deliberately stops at os.ReadFile:
// this is a policy pin for two files, not a general-purpose file-access
// analyzer.
func TestS5_8TemplateSourceGuardHasNoOSReadFileBypass(t *testing.T) {
	const preamble = "package web\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n"

	cases := []struct {
		name string
		src  string
		want []string // functions the guard must name; empty = must find nothing
	}{
		{
			name: "direct os.ReadFile of template source is caught",
			src:  "func TestSneaky(t *testing.T) {\n\tb, _ := os.ReadFile(\"templates/analytics_points.html\")\n\t_ = b\n}\n",
			want: []string{"TestSneaky"},
		},
		{
			name: "a wrapper hiding the read is caught at the wrapper",
			src: "func s58SneakyRead(name string) []byte {\n\tb, _ := os.ReadFile(\"templates/\" + name)\n\treturn b\n}\n\n" +
				"func TestViaWrapper(t *testing.T) { _ = s58SneakyRead(\"analytics_roi.html\") }\n",
			want: []string{"s58SneakyRead"},
		},
		{
			name: "a forwarding helper with a computed path is caught",
			src:  "func s58Forward(p string) []byte {\n\tb, _ := os.ReadFile(p)\n\treturn b\n}\n",
			want: []string{"s58Forward"},
		},
		{
			name: "reading the package's own Go source stays allowed",
			src:  "func TestOwnSource(t *testing.T) {\n\tb, _ := os.ReadFile(\"s5_8_analytics_harness_test.go\")\n\t_ = b\n}\n",
			want: nil,
		},
		{
			name: "an allow-listed static-property check may read template source",
			src:  "func TestS5_8HiddenActuallyHides(t *testing.T) { _ = s58ReadTemplate(t, \"analytics_points.html\") }\n",
			want: nil,
		},
		{
			name: "a non-allow-listed s58ReadTemplate caller is still caught",
			src:  "func TestSomethingElse(t *testing.T) { _ = s58ReadTemplate(t, \"analytics_points.html\") }\n",
			want: []string{"TestSomethingElse"},
		},
		{
			name: "the templatesFS bypass is still caught",
			src:  "func TestViaFS(t *testing.T) {\n\tb, _ := templatesFS.ReadFile(\"analytics_roi.html\")\n\t_ = b\n}\n",
			want: []string{"TestViaFS"},
		},
		{
			name: "a clean file reports nothing",
			src:  "func TestClean(t *testing.T) {\n\tif 1 != 1 {\n\t\tt.Fatal(\"no\")\n\t}\n}\n",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture_test.go", preamble+tc.src, 0)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			var got []string
			for _, v := range s58TemplateSourceViolations(file) {
				got = append(got, v.Func)
			}
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("guard reported %v, want %v", got, want)
			}
		})
	}
}
