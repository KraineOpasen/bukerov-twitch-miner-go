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
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// s58Routes are the two direct-render routes this slice adds.
var s58Routes = []string{"/analytics/points", "/analytics/roi"}

// s58Templates maps each route to the page template it renders.
var s58Templates = map[string]string{
	"/analytics/points": "templates/analytics_points.html",
	"/analytics/roi":    "templates/analytics_roi.html",
}

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

// TestS5_8C14ComponentExists proves the shared chart component is a real
// component template (parsed from templates/components/) rather than markup
// copy-pasted into each page.
func TestS5_8C14ComponentExists(t *testing.T) {
	src := s58ReadTemplate(t, "templates/components/c14_chart.html")
	if !strings.Contains(src, `{{define "c14.chart"}}`) {
		t.Error("c14_chart.html must define the shared c14.chart component")
	}
	// The visual is never the only representation: the component itself
	// carries the AT summary and the data table.
	for _, want := range []string{"c14-summary", "c14-table", "c14-csv"} {
		if !strings.Contains(src, want) {
			t.Errorf("c14.chart missing required %q hook — a chart must always ship its text alternative", want)
		}
	}
	// Reduced motion disables animation rather than merely shortening it.
	if !strings.Contains(src, "prefers-reduced-motion") {
		t.Error("c14.chart must consult prefers-reduced-motion")
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
			// The summary is a live region announced to AT, not decorative text.
			if !strings.Contains(body, `role="status"`) {
				t.Errorf("[%s %s] C14 summary must be an announced live region", lang, route)
			}
			// The data table is a real semantic table with a caption.
			if !strings.Contains(body, `<caption`) {
				t.Errorf("[%s %s] C14 data table must carry a <caption>", lang, route)
			}
		}
	}
}

// TestS5_8CSVIsClientGeneratedFromAuthoritativeJSON proves the visible CSV
// control is generated in the browser from the SAME authoritative JSON the
// page already fetched — never a new server endpoint, and never a second
// source of truth. The existing JSON export endpoints stay untouched.
func TestS5_8CSVIsClientGeneratedFromAuthoritativeJSON(t *testing.T) {
	// The download itself is built in-browser from an object URL — one shared
	// implementation, never a server round-trip.
	c14 := s58ReadTemplate(t, "templates/components/c14_chart.html")
	if !strings.Contains(c14, "Blob(") || !strings.Contains(c14, "createObjectURL") {
		t.Error("c14.chart must build the CSV client-side from an in-memory Blob")
	}

	for _, name := range []string{"templates/analytics_points.html", "templates/analytics_roi.html"} {
		src := s58ReadTemplate(t, name)
		if !strings.Contains(src, "data-c14-csv") {
			t.Errorf("%s: no CSV control", name)
		}
		// The page feeds the shared helper from the JSON it already fetched.
		if !strings.Contains(src, "c14DownloadCSV(") {
			t.Errorf("%s: CSV must go through the shared client-side converter", name)
		}
		if strings.Contains(src, "export.csv") || strings.Contains(src, "format=csv") {
			t.Errorf("%s: must not invent a server-side CSV endpoint", name)
		}
	}
}

// TestS5_8CSVTimestampsAreLocaleIndependent proves the Points CSV emits
// ISO-8601 UTC timestamps rather than the locale-formatted string the
// on-screen table shows or the raw epoch integer the API returns. A CSV is a
// file that outlives the session and gets parsed elsewhere, so its timestamps
// must not depend on the reader's locale or timezone — and must still be
// human-checkable.
func TestS5_8CSVTimestampsAreLocaleIndependent(t *testing.T) {
	src := s58ReadTemplate(t, "templates/analytics_points.html")
	if !strings.Contains(src, "toISOString()") {
		t.Error("Points CSV must emit ISO-8601 UTC timestamps")
	}
	// The CSV path must go through the ISO conversion, not the raw rows the
	// visible table renders.
	if !strings.Contains(src, "csvRows(lastData)") {
		t.Error("the CSV download must use the ISO-converted rows, not the raw table rows")
	}
	if strings.Contains(src, "tableRows(lastData)") {
		t.Error("the CSV download must not be fed the raw epoch table rows directly")
	}
}

// TestS5_8CSVEscapingIsDeterministicAndSafe proves the shared CSV conversion
// handles the four dangerous cell shapes: embedded commas, embedded double
// quotes, embedded newlines, and formula-injection prefixes (=, +, -, @)
// that spreadsheet software would otherwise execute.
func TestS5_8CSVEscapingIsDeterministicAndSafe(t *testing.T) {
	src := s58ReadTemplate(t, "templates/components/c14_chart.html")
	if !strings.Contains(src, "c14CSVCell") {
		t.Fatal("c14.chart must expose a single shared CSV cell-escaping helper (c14CSVCell)")
	}
	for _, want := range []string{
		`'"'`,     // quote doubling / quoting
		`\n`,      // newline handling
		`','`,     // comma handling
		"'='",     // formula-injection prefix guard
		"'\\t' +", // neutralizing prefix
		"\\r",     // CR handling
	} {
		if !strings.Contains(src, want) {
			t.Errorf("c14CSVCell missing handling for %s", want)
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
	src := s58ReadTemplate(t, "templates/analytics_points.html")

	if strings.Contains(src, "all_streamers") {
		t.Error("the Points page must not offer an All-streamers facet — the series is per-streamer")
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

// TestS5_8PointsInventsNoSamplingSemantics proves the Points page never
// fabricates data the backend does not provide: no sampling-gap threshold, no
// synthetic zero-filling, and no claim that the line is smoothed or
// interpolated. It renders exactly the samples /api/points-history returns.
func TestS5_8PointsInventsNoSamplingSemantics(t *testing.T) {
	src := s58ReadTemplate(t, "templates/analytics_points.html")
	for _, banned := range []string{
		"interpolat", "GAP_THRESHOLD", "gapThreshold", "fillZero", "zeroFill", "synthetic",
	} {
		if strings.Contains(src, banned) {
			t.Errorf("Points page must not invent sampling semantics — found %q", banned)
		}
	}
	// The straight-line rendering is honest ('straight'), never sold as a
	// smoothed curve over sparse samples.
	if strings.Contains(src, "curve: 'smooth'") {
		t.Error("Points chart must not present a smoothed curve — sparse samples would read as interpolated data")
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

// TestS5_8ROIReadsOnlyItsOwnJSONFields proves the ROI page never reconstructs
// financial outcome data client-side: every figure it shows comes straight
// from a field of the authoritative /api/predictions/roi response.
func TestS5_8ROIReadsOnlyItsOwnJSONFields(t *testing.T) {
	src := s58ReadTemplate(t, "templates/analytics_roi.html")
	for _, want := range []string{"netProfit", "totalWagered", "winRate", "maxDrawdown", "byStreamer", "byStrategy", "byOddsBucket"} {
		if !strings.Contains(src, want) {
			t.Errorf("ROI page does not read authoritative field %q", want)
		}
	}
	// Recomputing an outcome from parts is exactly the reconstruction the
	// owner forbade.
	for _, banned := range []string{"wins / (wins", "wins/(wins", "* 100 / totalWagered", "netProfit =", "recompute"} {
		if strings.Contains(src, banned) {
			t.Errorf("ROI page must never reconstruct financial outcomes — found %q", banned)
		}
	}
}

// TestS5_8ROIIsStrictlyReadOnly proves route 8 ships no mutation affordance:
// no bet/skip control, no form, no non-GET fetch, and no reference to any
// state-changing endpoint. Betting behavior is owned by /settings/predictions,
// which the page links to instead. Adding a mutation control fails here.
func TestS5_8ROIIsStrictlyReadOnly(t *testing.T) {
	srv := buildF3PageServer(t)
	src := s58ReadTemplate(t, "templates/analytics_roi.html")
	body := f3GetPage(t, srv, "/analytics/roi", "en")

	for _, banned := range []string{
		"/api/prediction/bet", "/api/prediction/skip", "/api/settings",
		"method: 'POST'", `method: "POST"`, "<form", "hx-post", "hx-put", "hx-delete",
	} {
		if strings.Contains(src, banned) {
			t.Errorf("ROI page must be strictly read-only — found mutation affordance %q", banned)
		}
	}
	// The read-only stance is stated to the operator, and the owner of the
	// behavior is linked rather than duplicated. Matched on the page's OWN
	// anchor, never on the C2 nav's /settings/predictions child, which every
	// page carries and which would make this assertion vacuous.
	if !strings.Contains(body, `data-ar-owner-link href="/settings/predictions"`) {
		t.Error("ROI page must carry its own owner link pointing at /settings/predictions")
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

	pointsSrc := s58ReadTemplate(t, "templates/analytics_points.html")
	if !strings.Contains(pointsSrc, "/api/points-history") {
		t.Error("Points page must read the existing /api/points-history endpoint")
	}
	roiSrc := s58ReadTemplate(t, "templates/analytics_roi.html")
	if !strings.Contains(roiSrc, "/api/predictions/roi") {
		t.Error("ROI page must read the existing /api/predictions/roi endpoint")
	}
}

// ---------------------------------------------------------------------
// 6. State honesty: S-FAIL alerts with Retry, and no fabricated zeros.
// ---------------------------------------------------------------------

// TestS5_8FailStatesAreAlertsWithRetry proves each page's failure state is an
// inline role="alert" (never a toast) and offers an explicit Retry control.
// Dropping role="alert" is a mutation this test rejects.
func TestS5_8FailStatesAreAlertsWithRetry(t *testing.T) {
	srv := buildF3PageServer(t)

	for _, route := range s58Routes {
		src := s58ReadTemplate(t, s58Templates[route])
		if !strings.Contains(src, `role="alert"`) {
			t.Errorf("%s: the S-FAIL block must carry role=\"alert\"", route)
		}
		for _, lang := range []string{"en", "ru"} {
			body := f3GetPage(t, srv, route, lang)
			if !strings.Contains(body, `role="alert"`) {
				t.Errorf("[%s %s] rendered page has no role=\"alert\" failure region", lang, route)
			}
			if !strings.Contains(body, "-retry") {
				t.Errorf("[%s %s] failure state must offer a Retry control", lang, route)
			}
		}
		// Failures are inline, never routed through the global toast region.
		if strings.Contains(src, "showToast") || strings.Contains(src, "minerToast") {
			t.Errorf("%s: failures must render inline, never as a toast", route)
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

// TestS5_8NoFalseZeros proves missing data renders an em dash with an
// accessible no-data label rather than a fabricated 0 — the same guard C4
// already applies on the queue roster. Replacing the dash with 0 is a
// mutation this test rejects.
func TestS5_8NoFalseZeros(t *testing.T) {
	for _, route := range s58Routes {
		src := s58ReadTemplate(t, s58Templates[route])
		if !strings.Contains(src, "—") {
			t.Errorf("%s: no em-dash no-data rendering found", route)
		}
		if !strings.Contains(src, "no_data") {
			t.Errorf("%s: the no-data dash must carry an accessible label", route)
		}
	}
	c14 := s58ReadTemplate(t, "templates/components/c14_chart.html")
	if !strings.Contains(c14, "c14NoData") {
		t.Fatal("c14.chart must expose a single shared no-data formatter (c14NoData)")
	}
	// The formatter must distinguish absent from zero — returning 0 for a
	// null/undefined input is exactly the false-zero the owner forbade.
	if !strings.Contains(c14, "== null") && !strings.Contains(c14, "=== null") {
		t.Error("c14NoData must test for an absent value, not merely a falsy one (0 is a real value)")
	}

	// c14Cell is what actually PAINTS an absent value, and it is the function a
	// "just render 0" regression would land in — c14NoData can stay perfectly
	// correct while its only caller throws the answer away. Assert on the cell
	// renderer's own body: an absent value must produce the em dash carrying an
	// accessible no-data label, never a substituted numeral. (This gap was found
	// by the no-data-dash-becomes-zero mutation probe, which the c14NoData-only
	// assertions above did not catch.)
	body := s58FuncBody(t, c14, "c14Cell")
	if !strings.Contains(body, "—") {
		t.Error("c14Cell must render an em dash for an absent value")
	}
	if !strings.Contains(body, "aria-label") {
		t.Error("c14Cell's no-data dash must carry an accessible label")
	}
	if !strings.Contains(body, "c14NoData(") {
		t.Error("c14Cell must route through the shared absent-vs-zero formatter")
	}
	for _, banned := range []string{"return '0'", `return "0"`, "return 0;"} {
		if strings.Contains(body, banned) {
			t.Errorf("c14Cell substitutes a fabricated zero for missing data (%s)", banned)
		}
	}
}

// s58FuncBody extracts one top-level JS function's body from a template, so a
// behavioral assertion can be scoped to the function that owns the behavior
// rather than to the whole file (where an unrelated occurrence of the same
// literal would mask a regression).
func s58FuncBody(t *testing.T, src, fn string) string {
	t.Helper()
	start := strings.Index(src, "function "+fn+"(")
	if start < 0 {
		t.Fatalf("function %s not found", fn)
	}
	open := strings.Index(src[start:], "{")
	if open < 0 {
		t.Fatalf("function %s has no body", fn)
	}
	depth, i := 0, start+open
	for ; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start+open : i+1]
			}
		}
	}
	t.Fatalf("function %s body is unbalanced", fn)
	return ""
}

// ---------------------------------------------------------------------
// 7. Cadence, localization and legacy parity.
// ---------------------------------------------------------------------

// TestS5_8ReusesExistingRefreshCadence proves both pages derive their refresh
// interval from the existing AnalyticsSettings.Refresh value threaded through
// RefreshMinutes — never a hardcoded cadence, and never a new polling, SSE,
// WebSocket or manual-refresh contract. Hardcoding a cadence fails here.
func TestS5_8ReusesExistingRefreshCadence(t *testing.T) {
	for _, route := range s58Routes {
		src := s58ReadTemplate(t, s58Templates[route])
		if !strings.Contains(src, "{{.RefreshMinutes}} * 60 * 1000") {
			t.Errorf("%s: must reuse the existing RefreshMinutes cadence verbatim", route)
		}
		for _, banned := range []string{"EventSource(", "WebSocket(", "hx-trigger=\"every", "/api/miner-status/stream"} {
			if strings.Contains(src, banned) {
				t.Errorf("%s: must not introduce a new %q transport", route, banned)
			}
		}
	}

	// The cadence is genuinely data-driven: the fixture's Refresh value must
	// reach the rendered page. Matched loosely because html/template pads a
	// numeric substitution with spaces in a JS context.
	srv := buildF3PageServer(t)
	cadence := regexp.MustCompile(`REFRESH_MS\s*=\s*5\s*\*\s*60\s*\*\s*1000`)
	for _, route := range s58Routes {
		body := f3GetPage(t, srv, route, "en")
		if !cadence.MatchString(body) {
			t.Errorf("rendered %s did not carry the fixture's Refresh=5 cadence", route)
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

// TestS5_8EarnedEventsNeverFabricateZeroWhenBreakdownAbsent pins Q3 MAJOR-1.
//
// /api/points-history omits "breakdown" entirely (`json:"breakdown,omitempty"`,
// analytics/models.go) for TWO different facts: BreakdownFromSamples returns
// nil when the raw window holds fewer than two samples — no delta EXISTS to
// attribute — and an empty slice when the window genuinely earned nothing.
// Both arrive as a missing key, so `for (const share of data.breakdown || [])`
// produced `earned = 0, events = 0` for both, painting "+0"/"0" (with no
// no-data label) over an INDETERMINATE window. Reproduced in a browser against
// a real single-sample payload before the fix.
//
// The fix disambiguates the two cases from the payload the page already has,
// inventing no backend field: chartDownsampled is true only when points was
// thinned (raw > points >= 1, hence raw >= 2); when it is false, points IS the
// raw series.
func TestS5_8EarnedEventsNeverFabricateZeroWhenBreakdownAbsent(t *testing.T) {
	src := s58ReadTemplate(t, "templates/analytics_points.html")
	body := s58FuncBody(t, src, "renderKPIs")

	// The absent-breakdown guard must exist, and must be decided by the raw
	// series length rather than by a falsy test on breakdown alone.
	if !strings.Contains(body, "chartDownsampled === true") {
		t.Error("renderKPIs must use chartDownsampled to establish that the raw series held at least two samples")
	}
	if !strings.Contains(body, "pts.length >= 2") {
		t.Error("renderKPIs must fall back to the points length when the series was not downsampled")
	}
	if !strings.Contains(body, "!data.breakdown") {
		t.Error("renderKPIs must branch on breakdown being ABSENT — an omitempty field is missing, not falsy-zero")
	}

	// The guard must precede the summation, or it cannot prevent anything.
	guard := strings.Index(body, "!data.breakdown")
	sum := strings.Index(body, "for (const share of data.breakdown")
	if guard < 0 || sum < 0 || guard > sum {
		t.Errorf("the absent-breakdown guard must run BEFORE the breakdown summation (guard=%d, sum=%d)", guard, sum)
	}

	// An indeterminate window dashes out both derived tiles; setKPI attaches
	// the accessible no-data label whenever the text is the em dash.
	for _, want := range []string{`setKPI('ap-kpi-earned', '—')`, `setKPI('ap-kpi-events', '—')`} {
		if !strings.Contains(body, want) {
			t.Errorf("renderKPIs must dash out the derived tile when the window is indeterminate: missing %q", want)
		}
	}
}

// TestS5_8NetChangeRequiresTwoSamples pins the same false-zero class on the
// net-change tile: with a single sample last-first is 0 by CONSTRUCTION, not by
// measurement, so reporting it asserts the balance held steady across a window
// that was never observed.
func TestS5_8NetChangeRequiresTwoSamples(t *testing.T) {
	src := s58ReadTemplate(t, "templates/analytics_points.html")
	body := s58FuncBody(t, src, "renderKPIs")

	if !strings.Contains(body, "pts.length < 2") {
		t.Error("the net-change tile must dash out below two samples — a change needs two observations")
	}
	netGuard := strings.Index(body, "pts.length < 2")
	netDash := strings.Index(body, `setKPI('ap-kpi-net', '—')`)
	if netGuard < 0 || netDash < 0 || netGuard > netDash {
		t.Errorf("the two-sample guard must gate the net tile's dash (guard=%d, dash=%d)", netGuard, netDash)
	}
	// Balance is a single observation and stays renderable from one sample.
	if !strings.Contains(body, "ap-kpi-balance") {
		t.Error("the balance tile must still render from a single sample — it is one observation, not a delta")
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
	c14 := s58ReadTemplate(t, "templates/components/c14_chart.html")

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
	if strings.Contains(c14, `aria-live="polite"`) {
		t.Error("the C14 summary markup must not hardcode aria-live=\"polite\" — the announcement is per-render")
	}

	// The setter decides liveness from its caller, and does so BEFORE the text
	// mutation, so the mutation is classified by the requested value.
	body := s58FuncBody(t, c14, "c14SetSummary")
	if !strings.Contains(body, "announce ? 'polite' : 'off'") {
		t.Error("c14SetSummary must set aria-live from its announce argument")
	}
	setAt := strings.Index(body, "setAttribute('aria-live'")
	textAt := strings.Index(body, "textContent")
	if setAt < 0 || textAt < 0 || setAt > textAt {
		t.Errorf("aria-live must be set BEFORE the text changes (attr=%d, text=%d)", setAt, textAt)
	}

	// Both pages thread the flag from load(): user-initiated renders announce,
	// the timed refresh does not.
	for _, name := range []string{"templates/analytics_points.html", "templates/analytics_roi.html"} {
		src := s58ReadTemplate(t, name)
		if !strings.Contains(src, "!isRefresh") {
			t.Errorf("%s: render() must be told whether this load was user-initiated", name)
		}
		if !strings.Contains(src, "function render(") {
			t.Fatalf("%s: no render() to thread the flag through", name)
		}
		if !strings.Contains(s58FuncBody(t, src, "render"), "announce") {
			t.Errorf("%s: render() must pass the announce flag down to the C14 summary", name)
		}
	}
}

// TestS5_8CautionStripsLeaveWithTheContentTheyDescribe pins Q3 MINOR-2.
//
// show() toggled only loading/empty/error/content, so the S-PART and S-STALE
// strips — which describe the CONTENT region — survived a transition into the
// failure state. Reproduced in a browser: after a failed load the page showed
// "Partial data: the selection hit the backend row limit" above a failure block
// that was displaying no data at all. Handoff §7 defines the S-PART strip as
// sitting above RETAINED content.
func TestS5_8CautionStripsLeaveWithTheContentTheyDescribe(t *testing.T) {
	cases := map[string][]string{
		"templates/analytics_points.html": {"els.partial.classList.add('hidden')", "els.stale.classList.add('hidden')"},
		"templates/analytics_roi.html":    {"els.stale.classList.add('hidden')"},
	}
	for name, wants := range cases {
		src := s58ReadTemplate(t, name)
		body := s58FuncBody(t, src, "show")
		if !strings.Contains(body, "state !== 'content'") {
			t.Errorf("%s: show() must clear the content-scoped strips whenever content is not the rendered state", name)
		}
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s: show() must clear %q when leaving the content state", name, want)
			}
		}
	}
}

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
