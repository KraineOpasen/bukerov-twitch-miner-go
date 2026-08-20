package web

import (
	"regexp"
	"strings"
	"testing"
)

// Structural regression tests for the August-2 visual restoration of the
// shared Dashboard chrome and /overview.
//
// Scope note: these pin STRUCTURE, not pixels. The restoration is a layout and
// typography change, so what has to be defended is the set of load-bearing
// facts a future refactor could quietly break — that all 30 routes survive the
// nav accordion, that the accordion is an enhancement rather than a
// requirement, that the console grid added no region and reordered none, and
// that the generated stylesheet actually carries the source rules. There is
// deliberately no assertion on a specific padding, colour, or track width:
// those are design decisions, and pinning them would only make the design
// harder to keep improving.
//
// Everything here is additive. No pre-existing assertion is relaxed, replaced
// or removed anywhere in this package.

// aug2Routes are the representative routes the shared chrome is checked on:
// /overview (the page this task recomposed) plus one route from three other
// sections, so a chrome regression cannot hide behind the one page under
// active development.
var aug2Routes = []string{"/overview", "/drops/current", "/settings/streamers", "/system/status"}

// aug2CanonicalChildren is the design's full 30-destination child set (§11
// page matrix), in nav order. Every one of these must remain a rendered child
// anchor in the shared chrome, on every route, regardless of which section the
// accordion happens to expand.
var aug2CanonicalChildren = []string{
	"/overview", "/overview/queue",
	"/drops/current", "/drops/upcoming", "/drops/claims", "/drops/past",
	"/analytics/points", "/analytics/roi",
	"/events", "/events/browser", "/events/sound", "/events/discord",
	"/settings/streamers", "/settings/rotation", "/settings/drops", "/settings/predictions",
	"/settings/chat-raids", "/settings/transport", "/settings/analytics-logging",
	"/settings/events-notifications", "/settings/discord", "/settings/system",
	"/system/status", "/system/diagnostics", "/system/logs",
	"/help/getting-started", "/help/glossary", "/help/troubleshooting",
	"/help/notifications-audio", "/help/diagnostics-support",
}

// aug2NavToggleRe matches a rendered section disclosure button's opening tag.
var aug2NavToggleRe = regexp.MustCompile(`<button type="button" class="c2-nav-disclosure"[^>]*>`)

// aug2UngatedChildHideRe matches a .c2-nav-children display:none rule whose
// selector is NOT prefixed by the [data-c2-collapsible] enhancement gate. The
// leading boundary is what separates it from the gated rule, whose selector
// merely ends in the same text.
var aug2UngatedChildHideRe = regexp.MustCompile(`(^|[\n{;}])\s*\.c2-nav-children\s*\{[^}]*display:\s*none`)

func aug2InputCSS(t *testing.T) string {
	t.Helper()
	return readEmbeddedStatic(t, "static/css/input.css")
}
func aug2AppCSS(t *testing.T) string             { t.Helper(); return readEmbeddedStatic(t, "static/css/app.css") }
func aug2Template(t *testing.T, n string) string { t.Helper(); return readEmbeddedTemplate(t, n) }

// ---------------------------------------------------------------------------
// Shared chrome — the accordion must not cost a single destination
// ---------------------------------------------------------------------------

// TestAug2AccordionKeepsSevenSectionsAndThirtyDestinations is the core
// safety property of the whole restoration: collapsing the nav changed how
// many child links are VISIBLE, and must never have changed how many EXIST.
// All 30 destinations stay rendered in the shared chrome on every route, so
// none can become unreachable, un-crawlable, or invisible to a client that
// never runs the collapse script at all.
func TestAug2AccordionKeepsSevenSectionsAndThirtyDestinations(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, route := range aug2Routes {
		for _, lang := range []string{"en", "ru"} {
			body := f3GetPage(t, srv, route, lang)

			s5_2AssertSevenSections(t, lang, body)

			if n := strings.Count(body, `data-nav-parent>`); n != 7 {
				t.Errorf("[%s %s] expected 7 top-level sections, found %d", route, lang, n)
			}
			if n := strings.Count(body, `data-nav-child>`); n != 30 {
				t.Errorf("[%s %s] expected 30 child destinations, found %d", route, lang, n)
			}
			for _, href := range aug2CanonicalChildren {
				want := `href="` + href + `" class="c2-nav-child" data-nav-section=`
				if !strings.Contains(body, want) {
					t.Errorf("[%s %s] canonical destination %q is no longer a rendered child anchor", route, lang, href)
				}
			}
		}
	}
}

// TestAug2DisclosureButtonsAreInertWithoutTheScript proves the accordion is a
// pure enhancement. The server renders the toggles `hidden`, and every CSS
// rule that collapses anything is gated on [data-c2-collapsible], an attribute
// only base.html's nav script ever sets. A client that never runs that script
// therefore sees the pre-restoration nav — all 30 children visible — and never
// sees a control that does nothing.
func TestAug2DisclosureButtonsAreInertWithoutTheScript(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview", "en")

	tags := aug2NavToggleRe.FindAllString(body, -1)
	if len(tags) != 7 {
		t.Fatalf("expected exactly 7 section disclosure buttons, found %d", len(tags))
	}
	for _, tag := range tags {
		if !strings.Contains(tag, " hidden") {
			t.Errorf("disclosure button is not server-rendered hidden: %s", tag)
		}
		if !strings.Contains(tag, `aria-expanded="false"`) {
			t.Errorf("disclosure button missing its initial aria-expanded state: %s", tag)
		}
		if !strings.Contains(tag, `aria-controls="c2-nav-kids-`) {
			t.Errorf("disclosure button does not name the child list it controls: %s", tag)
		}
		if !strings.Contains(tag, `aria-labelledby="c2-nav-lbl-`) {
			t.Errorf("disclosure button has no accessible name: %s", tag)
		}
		// updateActiveNav() walks [data-nav-section] and marks matches with
		// aria-current="page". The toggle is not a destination, so it must
		// never appear in that walk.
		if strings.Contains(tag, "data-nav-section") {
			t.Errorf("disclosure button carries data-nav-section and would be marked aria-current: %s", tag)
		}
		if strings.Contains(tag, "data-nav-parent") || strings.Contains(tag, "data-nav-child") {
			t.Errorf("disclosure button carries a destination marker and corrupts the 7/30 counts: %s", tag)
		}
	}

	base := aug2Template(t, "templates/base.html")
	if !strings.Contains(base, `nav.setAttribute('data-c2-collapsible', '')`) {
		t.Error("base.html no longer stamps data-c2-collapsible — the collapse would never engage")
	}
	if !strings.Contains(base, "btn.hidden = false") {
		t.Error("base.html no longer unhides the disclosure buttons — they would render as inert controls")
	}

	// Every collapse rule must be gated. An ungated one would hide children
	// for scriptless clients too.
	css := aug2InputCSS(t)
	for _, rule := range []string{
		".c2-nav[data-c2-collapsible] .c2-nav-children { display: none; }",
		".c2-nav[data-c2-collapsible] .c2-nav-disclosure { display: inline-flex; }",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("input.css is missing the gated accordion rule %q", rule)
		}
	}
	// Anchored at a rule boundary on purpose: an unanchored search also
	// matches the tail of the GATED rule above, which is the very thing this
	// is meant to tell apart.
	if aug2UngatedChildHideRe.MatchString(css) {
		t.Error("input.css hides .c2-nav-children without the [data-c2-collapsible] gate — scriptless clients would lose every child destination")
	}
}

// TestAug2AccordionLeavesTheIconRailAlone proves the collapse is scoped to the
// two ranges that render the full sidebar. The 1024-1279.98px icon rail
// reaches its child destinations ONLY through its hover/:focus-within flyout,
// so an accordion rule leaking into that range would strand 30 routes behind a
// control the rail does not even display.
func TestAug2AccordionLeavesTheIconRailAlone(t *testing.T) {
	css := aug2InputCSS(t)

	const scope = "@media (max-width: 1023.98px), (min-width: 1280px) {"
	at := strings.Index(css, scope)
	if at < 0 {
		t.Fatal("the accordion's drawer+sidebar media scope is missing from input.css")
	}
	// The scope must exclude the rail range on both sides.
	if strings.Contains(scope, "1024px") || strings.Contains(scope, "1279") {
		t.Error("the accordion media scope reaches into the 1024-1279.98px rail range")
	}

	// The rail's own unconditional hide, and its flyout, must be untouched.
	if !strings.Contains(css, ".sidebar-brand, .now-watching, .c2-nav-label, .c2-nav-children {\n    display: none;\n  }") {
		t.Error("the rail block no longer hides .c2-nav-children — its flyout contract is broken")
	}
	rail := extractMediaBlock(t, css, "@media (min-width: 1024px) and (max-width: 1279.98px)")
	for _, want := range []string{
		".c2-nav-item--group:hover .c2-nav-children",
		".c2-nav-item--group:focus-within .c2-nav-children",
	} {
		if !strings.Contains(rail, want) {
			t.Errorf("the rail block lost its %q flyout rule — keyboard users could not reach child routes there", want)
		}
	}
	if strings.Contains(rail, "data-c2-collapsible") {
		t.Error("an accordion rule leaked into the rail block")
	}
}

// TestAug2SharedChromeStillRendersEverywhere is the blast-radius guard: the
// shell and nav are on all 30 routes, so a chrome change is checked against
// more than the page it was written for. It also pins the two-global-live-
// regions budget, which a new status/announcement region would quietly break.
func TestAug2SharedChromeStillRendersEverywhere(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, route := range aug2Routes {
		for _, lang := range []string{"en", "ru"} {
			body := f3GetPage(t, srv, route, lang)

			for _, marker := range []string{`id="app-sidebar"`, `class="c2-nav"`, `class="app-main"`, `class="skip-link"`} {
				if !strings.Contains(body, marker) {
					t.Errorf("[%s %s] shared chrome marker %q missing", route, lang, marker)
				}
			}
			if n := strings.Count(body, "<h1"); n != 1 {
				t.Errorf("[%s %s] page renders %d <h1> elements, want exactly 1", route, lang, n)
			}
			// Two is the design's global ceiling, not a per-page quota: the
			// toast stack is on every page, the lifecycle announcement region
			// only where a lifecycle surface exists. What must never happen is
			// a THIRD one appearing because a recomposition reached for its
			// own announcer.
			n := strings.Count(body, `aria-live=`)
			if n < 1 || n > 2 {
				t.Errorf("[%s %s] page renders %d aria-live regions, want 1 or 2 (global ceiling is 2)", route, lang, n)
			}
			if route == "/overview" && n != 2 {
				t.Errorf("[%s %s] /overview must keep exactly its two live regions, found %d", route, lang, n)
			}
		}
	}
}

// TestAug2ChromeIntroducedNoExternalAssetReference proves the restoration
// stayed inside the vendored, self-hosted asset universe: no CDN, no web font,
// no absolute URL anywhere in the chrome it touched. The chevron SVG in
// particular must not carry an xmlns.
func TestAug2ChromeIntroducedNoExternalAssetReference(t *testing.T) {
	for _, name := range []string{
		"templates/base.html",
		"templates/components/c2_nav.html",
		"templates/overview.html",
		"templates/partials/overview_live.html",
	} {
		src := aug2Template(t, name)
		for _, bad := range []string{"http://", "https://", "//cdn.", "@import url("} {
			if strings.Contains(src, bad) {
				t.Errorf("%s contains an external reference %q", name, bad)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// /overview — the console grid added no region and reordered none
// ---------------------------------------------------------------------------

// TestAug2ConsoleGridAddedNoRegionMarker proves the recomposition is layout
// only. The grid wrappers are plain layout classes; if any of them had reached
// for a data-ov-* attribute it would have widened the page's closed region
// vocabulary, which is exactly how a fabricated region gets in.
func TestAug2ConsoleGridAddedNoRegionMarker(t *testing.T) {
	body := s53cPage(t, "en")

	for _, wrapper := range []string{`class="ov-console"`, `class="ov-live"`} {
		if !strings.Contains(body, wrapper) {
			t.Errorf("/overview no longer renders the console wrapper %s", wrapper)
		}
	}
	// Frozen DOM order, re-asserted here against the grid: a CSS grid can
	// place cells anywhere, so the source order is the only thing that keeps
	// the page's reading sequence honest.
	ordered := []string{
		`id="lifecycle-panel"`,
		"data-ov-slots",
		"data-ov-health-summary",
		"data-ov-pred-kpi",
		"data-ov-pred-board",
	}
	prev := -1
	for _, marker := range ordered {
		at := strings.Index(body, marker)
		if at < 0 {
			t.Fatalf("/overview no longer renders region marker %q", marker)
		}
		if at <= prev {
			t.Errorf("region %q renders out of the frozen order", marker)
		}
		prev = at
	}
}

// TestAug2OverviewStillOwnsExactlyTwoSlotsAndNoRoster pins the two boundaries
// a denser Overview is most likely to erode: the slot pair is exactly two
// boxes, and neither the removed roster nor the host-resource strip came back
// under cover of "more density".
func TestAug2OverviewStillOwnsExactlyTwoSlotsAndNoRoster(t *testing.T) {
	body := s53cPage(t, "en")

	region := s53SlotPairRegion(t, body)
	if n := countC12SlotBoxes(region); n != 2 {
		t.Errorf("/overview renders %d slot boxes in its watch pair, want exactly 2", n)
	}

	for _, banned := range []string{
		"card-grid",            // the Aug-2 roster grid
		`data-ov-group=`,       // its group headings
		`id="ov-filter-input"`, // its filter toolbar
		`id="ov-sort-select"`,
		`class="rw-strip"`, // the host-resource strip
		`data-rw=`,
		`data-action="cycle-preference"`, // manual roster controls
		`data-action="toggle-watch"`,
	} {
		if strings.Contains(body, banned) {
			t.Errorf("/overview re-introduced removed roster/resource markup %q", banned)
		}
	}
}

// TestAug2PredictionsBoardCollapsedByDefaultAndSurvivesTheSwap covers both
// halves of the board's contract. Collapsed-by-default is frozen, so the
// server must never emit ` open`. But the board also lives inside the node the
// 30s poll replaces wholesale, and nothing used to restore its disclosure
// state — so an opened board snapped shut on the next tick and the page height
// jumped by its full height. Restoring it client-side is what makes "expanded"
// a state a user can actually sit in, and it is the same capture-on-request /
// reapply-on-swap shape the lifecycle panel already uses for #lc-advanced.
func TestAug2PredictionsBoardCollapsedByDefaultAndSurvivesTheSwap(t *testing.T) {
	body := s53cPage(t, "en")

	at := strings.Index(body, "data-ov-pred-board")
	if at < 0 {
		t.Fatal("/overview no longer renders the predictions board")
	}
	tagStart := strings.LastIndex(body[:at], "<details")
	tagEnd := strings.Index(body[at:], ">")
	if tagStart < 0 || tagEnd < 0 {
		t.Fatal("could not isolate the predictions board's opening tag")
	}
	if strings.Contains(body[tagStart:at+tagEnd], " open") {
		t.Error("the predictions board renders expanded — collapsed-by-default is frozen")
	}

	page := aug2Template(t, "templates/overview.html")
	if !strings.Contains(page, "function predBoard()") {
		t.Fatal("overview.html no longer restores the predictions board's disclosure state across the 30s swap")
	}
	for _, want := range []string{"htmx:beforeRequest", "htmx:afterSwap", "board.open = true"} {
		if !strings.Contains(page, want) {
			t.Errorf("the board's swap-state restore is missing %q", want)
		}
	}
	// It must find the board through the #predictions anchor, never through
	// the marker name: /overview's region inventory is pinned by counting raw
	// marker occurrences, so a second one in a script string reads as a
	// duplicated region.
	if n := strings.Count(page, "data-ov-pred-board"); n != 0 {
		t.Errorf("overview.html mentions data-ov-pred-board %d times; the marker must appear only in the partial", n)
	}
}

// TestAug2SlotsReserveFixedGeometry pins the anti-layout-shift guarantee the
// denser slot cards depend on. Almost every line inside a slot is conditional
// (game, reason badge, campaign, streak), so without a reserved height a slot
// that gains or loses one on the 30s swap shifts every region below it.
func TestAug2SlotsReserveFixedGeometry(t *testing.T) {
	css := aug2InputCSS(t)
	block := extractMediaBlock(t, css, ".ov-cell--slots .c12-slot")
	if !strings.Contains(block, "min-height:") {
		t.Errorf("/overview's slot cards no longer reserve a minimum height; block=%q", block)
	}
}

// TestAug2ContentMeasureIsBounded proves the shell finally implements the
// design's own desktop rule — a bounded, centred content measure — which is
// what stops a page stretching one row of controls across ~2250px at the
// 2560x1440 target. The two byte-pinned mobile padding rules are checked here
// too: the measure had to be added to the base rule precisely because those
// two are load-bearing literals elsewhere.
func TestAug2ContentMeasureIsBounded(t *testing.T) {
	css := aug2InputCSS(t)

	base := extractMediaBlock(t, css, ".app-main {")
	for _, want := range []string{"max-width:", "margin-inline: auto;"} {
		if !strings.Contains(base, want) {
			t.Errorf(".app-main's base rule is missing %q — the content measure is unbounded", want)
		}
	}
	for _, literal := range []string{
		".app-main { padding: 1rem; }",
		".app-main { padding: 3.25rem 1rem 1rem; }",
	} {
		if !strings.Contains(css, literal) {
			t.Errorf("the byte-pinned mobile padding rule %q was disturbed", literal)
		}
	}
}

// TestAug2NoDecorativeGradientsIntroduced holds the restoration to the design
// system's "quiet chrome" rule. The August-2 stylesheet reached for two-stop
// gradients for its nav ink and section accents; this task restored that
// hierarchy with flat tokens instead, and must keep doing so.
func TestAug2NoDecorativeGradientsIntroduced(t *testing.T) {
	css := aug2InputCSS(t)
	at := strings.Index(css, "/overview — August-2 console restoration")
	if at < 0 {
		t.Fatal("the /overview restoration CSS section is missing from input.css")
	}
	if strings.Contains(css[at:], "linear-gradient") {
		t.Error("the /overview restoration section introduced a decorative gradient")
	}
	for _, sel := range []string{".c2-nav-disclosure {", ".ov-cell-label::before {"} {
		block := extractMediaBlock(t, css, sel)
		if strings.Contains(block, "gradient") {
			t.Errorf("%s uses a gradient", sel)
		}
	}
}

// TestAug2GeneratedStylesheetCarriesTheRestoration proves app.css was actually
// regenerated from input.css rather than left behind. app.css is a build
// artifact and is never hand-edited, so a source rule that never reached it
// means the whole restoration ships invisible.
func TestAug2GeneratedStylesheetCarriesTheRestoration(t *testing.T) {
	app := aug2AppCSS(t)
	for _, want := range []string{
		"data-c2-collapsible",
		".c2-nav-disclosure",
		".ov-console",
		".ov-cell--slots",
		".ov-readout",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("generated app.css does not carry %q — it is stale against input.css", want)
		}
	}
	// PR #195's explicit source universe must survive: without source(none)
	// Tailwind resumes scanning the whole repository.
	in := aug2InputCSS(t)
	if !strings.Contains(in, `@import "tailwindcss" source(none);`) {
		t.Error(`input.css lost ` + "`" + `source(none)` + "`" + ` — whole-repo content detection would come back`)
	}
	if !strings.Contains(in, `@source "../../templates";`) {
		t.Error("input.css lost its explicit template source directive")
	}
}
