package web

// S5-2 seven-section chrome tests (task §K.1, K.2, K.3, K.7, K.10, K.11,
// K.12): exactly seven top-level nav sections render in both languages
// regardless of Discord configuration, the minimal Events/Help landings
// behave per OD-S5-2-1 items 1-2, the responsive sidebar/rail/drawer source
// contract (toggle/scrim/focus-trap/Escape/return-focus/scroll-lock) is
// present, chrome carries exactly the two mandated global live regions
// (never a page-local duplicate toast container), and no error path calls
// the global toast function anymore.

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// s5_2NavSectionRe extracts every data-nav-section="..." value from a
// rendered page or the raw base.html source.
var s5_2NavSectionRe = regexp.MustCompile(`data-nav-section="([a-z]+)"`)

func s5_2DistinctSections(body string) map[string]bool {
	set := map[string]bool{}
	for _, m := range s5_2NavSectionRe.FindAllStringSubmatch(body, -1) {
		set[m[1]] = true
	}
	return set
}

var s5_2WantSections = []string{"overview", "drops", "analytics", "events", "settings", "system", "help"}

func s5_2AssertSevenSections(t *testing.T, lang, body string) {
	t.Helper()
	set := s5_2DistinctSections(body)
	if len(set) != 7 {
		t.Errorf("[%s] expected exactly 7 distinct nav sections, got %d: %v", lang, len(set), set)
	}
	for _, want := range s5_2WantSections {
		if !set[want] {
			t.Errorf("[%s] missing nav section %q", lang, want)
		}
	}
	// The Events destination itself must always be reachable — never hidden
	// by Discord configuration (OD-S5-2-1 item 2).
	if !strings.Contains(body, `href="/events"`) {
		t.Errorf("[%s] Events & Notifications destination (/events) missing from nav", lang)
	}
}

// TestS5_2SevenSectionsDiscordEnabled proves the seven-section chrome
// renders identically (all seven present, Events included) in both
// languages when Discord is enabled.
func TestS5_2SevenSectionsDiscordEnabled(t *testing.T) {
	srv := buildF3PageServer(t)
	srv.SetDiscordEnabled(true)
	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/overview", lang)
		s5_2AssertSevenSections(t, lang, body)
	}
}

// TestS5_2SevenSectionsDiscordDisabled proves the same seven sections still
// render — Events & Notifications is never removed or hidden just because
// Discord is unconfigured; only /events' own CONTENT varies.
func TestS5_2SevenSectionsDiscordDisabled(t *testing.T) {
	srv := buildF3PageServer(t)
	srv.SetDiscordEnabled(false)
	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/overview", lang)
		s5_2AssertSevenSections(t, lang, body)
	}
}

// TestS5_2NavChildDisambiguation pins BOTH group destinations' structure
// (task Q3 BLOCKER-1): the System group (one parent, two children — Health/
// Logs) AND the Overview group (one parent, two children — Overview itself/
// Queue), for a combined two parent links (data-nav-parent) and four
// children (data-nav-child), each with its own distinct href.
func TestS5_2NavChildDisambiguation(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview", "en")

	// Matched with the trailing ">" so the JS source's own string-literal
	// references to these attribute names (e.g. hasAttribute('data-nav-parent'))
	// aren't miscounted as HTML occurrences.
	if n := strings.Count(body, `data-nav-parent>`); n != 2 {
		t.Errorf("expected exactly two data-nav-parent groups (Overview, System), found %d", n)
	}
	if n := strings.Count(body, `data-nav-child>`); n != 4 {
		t.Errorf("expected exactly four data-nav-child destinations (Overview, Queue, Health, Logs), found %d", n)
	}
	if !strings.Contains(body, `href="/health" class="c2-nav-child" data-nav-section="system" data-nav-child`) {
		t.Error("System group missing the Health child destination")
	}
	if !strings.Contains(body, `href="/logs" class="c2-nav-child" data-nav-section="system" data-nav-child`) {
		t.Error("System group missing the Logs child destination")
	}
	if !strings.Contains(body, `href="/overview" class="c2-nav-child" data-nav-section="overview" data-nav-child`) {
		t.Error("Overview group missing the Overview child destination")
	}
	if !strings.Contains(body, `href="/overview/queue" class="c2-nav-child" data-nav-section="overview" data-nav-child`) {
		t.Error("Overview group missing the Queue child destination")
	}
}

// s5_3NavAnchorTagRe matches a rendered C2 nav anchor's full opening tag —
// either a top-level .c2-nav-link or a group's .c2-nav-child — so
// TestS5_3OverviewQueueExactlyOneAriaCurrentDestination can inspect every
// candidate destination regardless of which group it belongs to.
var s5_3NavAnchorTagRe = regexp.MustCompile(`<a href="[^"]*" class="c2-nav-(?:link|child)"[^>]*>`)
var s5_3HrefAttrRe = regexp.MustCompile(`href="([^"]*)"`)
var s5_3NavSectionAttrRe = regexp.MustCompile(`data-nav-section="([a-z]+)"`)

// TestS5_3OverviewQueueExactlyOneAriaCurrentDestination proves BLOCKER-1's
// fix end to end: it re-implements base.html's client-side updateActiveNav
// decision (isCurrent = isChild ? sectionMatches && href===path :
// sectionMatches; aria-current only when !isParent && isCurrent) in Go
// against the ACTUAL rendered C2 markup for GET /overview/queue, proving
// exactly one destination (the Queue child, never the Overview group's own
// parent link and never its Overview child) would receive aria-current —
// without requiring a browser to execute the real script.
func TestS5_3OverviewQueueExactlyOneAriaCurrentDestination(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview/queue", "en")

	const path = "/overview/queue"
	const active = "overview" // SECTION_RULES: every /overview/* path -> overview

	tags := s5_3NavAnchorTagRe.FindAllString(body, -1)
	if len(tags) == 0 {
		t.Fatal("no C2 nav destination anchors found in the rendered page")
	}

	var currentHrefs []string
	for _, tag := range tags {
		href := ""
		if m := s5_3HrefAttrRe.FindStringSubmatch(tag); m != nil {
			href = m[1]
		}
		section := ""
		if m := s5_3NavSectionAttrRe.FindStringSubmatch(tag); m != nil {
			section = m[1]
		}
		isParent := strings.Contains(tag, "data-nav-parent")
		isChild := strings.Contains(tag, "data-nav-child")
		sectionMatches := section == active
		isCurrent := sectionMatches
		if isChild {
			isCurrent = sectionMatches && href == path
		}
		if !isParent && isCurrent {
			currentHrefs = append(currentHrefs, href)
		}
	}
	if len(currentHrefs) != 1 {
		t.Errorf("simulated nav activation on %s must mark exactly one destination current, got %d: %v", path, len(currentHrefs), currentHrefs)
	} else if currentHrefs[0] != path {
		t.Errorf("the one current destination must be %s itself, got %s", path, currentHrefs[0])
	}
}

// TestS5_2DrawerSourceContract checks base.html's source for the required
// accessibility behaviors of the responsive drawer: the toggle references
// the sidebar via aria-controls/aria-expanded, a scrim exists and closes on
// click, Escape closes, focus is trapped (Tab/Shift+Tab cycling) while open,
// initial focus lands on the first nav destination, focus returns to the
// toggle on close, and body scrolling is locked while open.
func TestS5_2DrawerSourceContract(t *testing.T) {
	base := readEmbeddedTemplate(t, "templates/base.html")

	for _, want := range []string{
		`aria-controls="app-sidebar"`,
		`aria-expanded="false"`,
		"data-chrome-scrim",
		"getElementById('sidebar-toggle')",
		"e.key === 'Escape'",
		"e.key !== 'Tab'",
		"shiftKey",
		"chrome-scroll-lock",
		".c2-nav-link, .c2-nav-child", // initial-focus target on open
		"toggle.focus()",              // return focus to the invoker on close
		"data-drawer-close",
	} {
		if !strings.Contains(base, want) {
			t.Errorf("base.html missing drawer contract literal %q", want)
		}
	}
}

// TestS5_2DrawerResizeSafeFocusTrapSourceContract (MAJOR) proves the Tab
// focus trap only runs while the viewport is genuinely inside the drawer
// range, that a single matchMedia authority backs both the trap guard and
// the breakpoint cleanup (never a second, independently-declared listener),
// that transitioning past 1024px while open clears the drawer's stale
// is-open/scrim/aria-expanded/scroll-lock state via the same setOpen used
// everywhere else, and that this cleanup path never forces focus onto the
// now-hidden drawer toggle.
func TestS5_2DrawerResizeSafeFocusTrapSourceContract(t *testing.T) {
	base := readEmbeddedTemplate(t, "templates/base.html")

	start := strings.Index(base, "const toggle = document.getElementById('sidebar-toggle');")
	if start < 0 {
		t.Fatal("could not locate the responsive-drawer script body in base.html")
	}
	relEnd := strings.Index(base[start:], "})();")
	if relEnd < 0 {
		t.Fatal("could not locate the end of the responsive-drawer IIFE in base.html")
	}
	fn := base[start : start+relEnd]

	// Exactly one drawer media-query authority — a second, independently
	// declared matchMedia('(max-width: 1023.98px)') would risk the trap
	// guard and the cleanup listener silently drifting apart.
	if n := strings.Count(fn, "matchMedia('(max-width: 1023.98px)')"); n != 1 {
		t.Fatalf("expected exactly one drawer matchMedia authority in the drawer script, found %d", n)
	}
	if !strings.Contains(fn, "const drawerMedia = window.matchMedia('(max-width: 1023.98px)');") {
		t.Error("expected a single named drawerMedia authority declared once")
	}

	// The Tab trap must be guarded by drawerMedia.matches, not .is-open
	// alone, so it can never fire once the drawer is no longer an off-canvas
	// overlay (rail/sidebar layouts at >=1024px).
	trapIdx := strings.Index(fn, "if (e.key !== 'Tab'")
	if trapIdx < 0 {
		t.Fatal("could not locate the Tab-trap guard")
	}
	trapLineEnd := strings.Index(fn[trapIdx:], "\n")
	if trapLineEnd < 0 {
		t.Fatal("could not locate the end of the Tab-trap guard line")
	}
	trapLine := fn[trapIdx : trapIdx+trapLineEnd]
	if !strings.Contains(trapLine, "drawerMedia.matches") {
		t.Error("the Tab focus trap guard must also check drawerMedia.matches, not just .is-open, so it never runs at >=1024px")
	}

	// A drawerMedia change listener must exist and clear stale drawer state
	// (via the shared setOpen) only when the viewport has left the drawer
	// range.
	changeIdx := strings.Index(fn, "drawerMedia.addEventListener('change'")
	if changeIdx < 0 {
		t.Fatal("expected a drawerMedia change listener that cleans up stale drawer state on breakpoint transition")
	}
	relChangeEnd := strings.Index(fn[changeIdx:], "});")
	if relChangeEnd < 0 {
		t.Fatal("could not locate the end of the drawerMedia change listener")
	}
	changeBlock := fn[changeIdx : changeIdx+relChangeEnd+len("});")]
	if !strings.Contains(changeBlock, "!e.matches") {
		t.Error("the breakpoint cleanup must only run once the viewport has left the drawer range (!e.matches)")
	}
	if !strings.Contains(changeBlock, "setOpen(false, false)") {
		t.Error("the breakpoint cleanup must clear drawer state via setOpen(false, false) — the restoreFocus=false form")
	}

	// setOpen itself must gate toggle.focus() behind the restoreFocus flag,
	// so the false form used by breakpoint cleanup structurally cannot move
	// focus onto the (now CSS-hidden) drawer toggle.
	setOpenIdx := strings.Index(fn, "const setOpen = (open, restoreFocus)")
	if setOpenIdx < 0 {
		t.Fatal("could not locate the setOpen(open, restoreFocus) definition")
	}
	relSetOpenEnd := strings.Index(fn[setOpenIdx:], "};")
	if relSetOpenEnd < 0 {
		t.Fatal("could not locate the end of the setOpen definition")
	}
	setOpenBody := fn[setOpenIdx : setOpenIdx+relSetOpenEnd+2]
	if !strings.Contains(setOpenBody, "} else if (restoreFocus) {") {
		t.Error("setOpen must only call toggle.focus() when restoreFocus is true, so breakpoint cleanup can never force focus onto the hidden toggle")
	}
	if !strings.Contains(setOpenBody, "toggle.focus();") {
		t.Error("setOpen must still call toggle.focus() on the user-triggered (restoreFocus=true) close path")
	}
}

// TestS5_2AriaCurrentSourceContract checks base.html's nav-activation script
// actually sets and clears aria-current="page" — the mechanism behind "set
// aria-current on exactly one active destination, remove it from inactive
// links" (task §B).
func TestS5_2AriaCurrentSourceContract(t *testing.T) {
	base := readEmbeddedTemplate(t, "templates/base.html")
	for _, want := range []string{
		`a.setAttribute('aria-current', 'page')`,
		`a.removeAttribute('aria-current')`,
		"data-nav-section",
	} {
		if !strings.Contains(base, want) {
			t.Errorf("base.html missing aria-current activation literal %q", want)
		}
	}
}

// TestS5_2RailAndSidebarResponsiveTokens confirms the new breakpoint layer
// exists in input.css: the <1024px drawer extension, the 1024-1279px icon
// rail (using --z-rail), and the >=1280px 260px sidebar — additive to (never
// replacing) the pinned <=900px drawer rules.
func TestS5_2RailAndSidebarResponsiveTokens(t *testing.T) {
	css := readEmbeddedStatic(t, "static/css/input.css")
	for _, want := range []string{
		"@media (min-width: 901px) and (max-width: 1023.98px)",
		"@media (min-width: 1024px) and (max-width: 1279.98px)",
		"@media (min-width: 1280px)",
		"var(--z-rail)",
		"grid-template-columns: 56px minmax(0, 1fr);",
		"grid-template-columns: 260px minmax(0, 1fr);",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("input.css missing responsive-layering literal %q", want)
		}
	}
	// The pinned <=900px drawer block (F1 hotfix) must remain untouched.
	if !strings.Contains(css, "@media (max-width: 900px)") {
		t.Error("input.css must keep the pinned <=900px drawer breakpoint")
	}
}

// TestS5_2GlobalLiveRegionsExactlyTwo pins OD-S5-2-1 item 3: base.html calls
// each C17 global region exactly once, so every page gets exactly one
// polite toast stack and exactly one lifecycle alert wrapper — never zero,
// never duplicated.
func TestS5_2GlobalLiveRegionsExactlyTwo(t *testing.T) {
	base := readEmbeddedTemplate(t, "templates/base.html")

	if n := strings.Count(base, `{{template "c17.toast_stack"`); n != 1 {
		t.Errorf("expected exactly one c17.toast_stack call in base.html, found %d", n)
	}
	if n := strings.Count(base, `{{template "c17.lifecycle_alert"`); n != 1 {
		t.Errorf("expected exactly one c17.lifecycle_alert call in base.html, found %d", n)
	}

	// Child banner ids are preserved but no longer carry their own alert role
	// — that markup now lives in the c17.lifecycle_alert component, not
	// inline in base.html.
	c17 := readEmbeddedTemplate(t, "templates/components/c17_toast_region.html")
	if !strings.Contains(c17, `id="health-banner" class="hidden">`) {
		t.Error("#health-banner id must be preserved, without its own role=alert")
	}
	if strings.Contains(c17, `id="health-banner" class="hidden" role="alert"`) {
		t.Error("#health-banner must no longer carry its own role=alert (the wrapper owns it now)")
	}
	if !strings.Contains(c17, `id="lifecycle-auth-banner" class="hidden"></div>`) {
		t.Error("#lifecycle-auth-banner id must be preserved, without its own role=alert")
	}
	if strings.Contains(c17, `id="lifecycle-auth-banner" class="hidden" role="alert"`) {
		t.Error("#lifecycle-auth-banner must no longer carry its own role=alert (the wrapper owns it now)")
	}

	// Rendered output: the wrapper itself is the sole alert region, and the
	// regional Overview live region (#lc-live) is untouched.
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview", "en")
	if n := strings.Count(body, `id="lifecycle-alert-region"`); n != 1 {
		t.Errorf("expected exactly one #lifecycle-alert-region, found %d", n)
	}
	if !strings.Contains(body, `id="lifecycle-alert-region" class="c17-lifecycle-alert" role="alert"`) {
		t.Error("#lifecycle-alert-region must carry role=alert")
	}
	if !strings.Contains(body, `<div id="lc-live" class="visually-hidden" aria-live="polite" role="status"></div>`) {
		t.Error("Overview's regional #lc-live live region must remain present and unchanged")
	}
}

// TestS5_2NoPageLocalDuplicateToastContainer proves overview/notifications/
// settings each render exactly one #toast-container (the global chrome one
// from base.html) — no leftover page-local instance.
func TestS5_2NoPageLocalDuplicateToastContainer(t *testing.T) {
	srv := buildF3PageServer(t)
	cases := []string{"/overview", "/notifications", "/settings"}
	for _, path := range cases {
		body := f3GetPage(t, srv, path, "en")
		if n := strings.Count(body, `id="toast-container"`); n != 1 {
			t.Errorf("%s: expected exactly one #toast-container, found %d", path, n)
		}
	}
}

// s5_2ErrorToastRe matches a showToast(...) call whose arguments include the
// literal 'error' variant marker — the exact pattern this slice eliminated.
var s5_2ErrorToastRe = regexp.MustCompile(`showToast\([^)]*'error'`)

// TestS5_2NoErrorPathCallsGlobalToast scans the three converted templates'
// raw source for any remaining error-variant showToast call. Every failure
// site must now use the inline C1-compatible showInlineFailure/showLiveError
// mechanism instead.
func TestS5_2NoErrorPathCallsGlobalToast(t *testing.T) {
	for _, name := range []string{"templates/overview.html", "templates/notifications.html", "templates/settings.html"} {
		src := readEmbeddedTemplate(t, name)
		if s5_2ErrorToastRe.MatchString(src) {
			t.Errorf("%s still calls showToast with an 'error' variant — must be an inline C1 failure region instead", name)
		}
		if strings.Contains(src, "function showToast(") {
			t.Errorf("%s must not redefine showToast locally anymore — it is shared from base.html", name)
		}
	}
}

// TestS5_2EventsPageDiscordEnabled: /events direct-renders 200, links to the
// existing Notifications page, and states no fabricated journal content.
func TestS5_2EventsPageDiscordEnabled(t *testing.T) {
	srv := buildF3PageServer(t)
	srv.SetDiscordEnabled(true)
	body := f3GetPage(t, srv, "/events", "en")

	if !strings.Contains(body, `href="/notifications"`) {
		t.Error("Discord-enabled /events must link to /notifications")
	}
	// /settings is always present in the sidebar nav, so the disabled-state
	// CONTENT link is distinguished by its localized text, not the bare href.
	if strings.Contains(body, "Go to Settings") {
		t.Error("Discord-enabled /events must not show the disabled-state content (Go to Settings)")
	}
	for _, banned := range []string{"delivered", "delivery status", "sound played", "browser permission"} {
		if strings.Contains(strings.ToLower(body), banned) {
			t.Errorf("/events must not fabricate delivery/sound/browser evidence, found %q", banned)
		}
	}
}

// TestS5_2EventsPageDiscordDisabled: /events shows the neutral/empty
// C1-compatible state and links to /settings instead.
func TestS5_2EventsPageDiscordDisabled(t *testing.T) {
	srv := buildF3PageServer(t)
	srv.SetDiscordEnabled(false)
	body := f3GetPage(t, srv, "/events", "en")

	if !strings.Contains(body, `href="/settings"`) {
		t.Error("Discord-disabled /events must link to /settings")
	}
	if strings.Contains(body, `href="/notifications"`) {
		t.Error("Discord-disabled /events must not show the enabled-state /notifications link")
	}
	if !strings.Contains(body, "c1-block") {
		t.Error("Discord-disabled /events must render its state via the C1 state-block markup")
	}
}

// TestS5_2HelpPageContract: /help/getting-started direct-renders 200, has
// exactly one h1, links to all seven live section landings, and states
// honestly that the full Help center arrives later.
func TestS5_2HelpPageContract(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/help/getting-started", "en")

	if n := strings.Count(body, "<h1"); n != 1 {
		t.Errorf("expected exactly one <h1> on the Help landing, found %d", n)
	}
	for _, href := range []string{`href="/overview"`, `href="/drops"`, `href="/statistics"`, `href="/events"`, `href="/settings"`, `href="/health"`, `href="/help/getting-started"`} {
		if !strings.Contains(body, href) {
			t.Errorf("Help landing missing link %q", href)
		}
	}
	// Honestly NAMING what's deferred (in the "arrives later" note) is
	// required, not banned — what must be absent is actual glossary/
	// troubleshooting/diagnostics CONTENT (definition lists, numbered
	// procedures, a build/version block), which this minimal landing never
	// renders in the first place.
	if !strings.Contains(body, "arrives in a later update") {
		t.Error("Help landing missing the honest deferred-content note")
	}
	for _, banned := range []string{"<dl", "<ol", "Diagnostic Snapshot"} {
		if strings.Contains(body, banned) {
			t.Errorf("Help landing must not render actual glossary/troubleshooting/diagnostics content, found %q", banned)
		}
	}
}

// httpGetBody is a small local helper for direct handler.ServeHTTP calls that
// don't need the full f3 fixture set (kept separate from f3GetPage, which
// always expects a 200).
func httpGetBody(t *testing.T, h http.Handler, path string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, rec.Body.String()
}

// ---------------------------------------------------------------------------
// Q3 corrective tests (MAJOR-1, MAJOR-2, MINOR-1, MINOR-2, MINOR-3, MINOR-4)
// ---------------------------------------------------------------------------

// s5_2NavLinkTagRe matches a rendered top-level .c2-nav-link anchor's full
// opening tag, so its attributes (including any aria-label) can be inspected.
var s5_2NavLinkTagRe = regexp.MustCompile(`<a href="[^"]*" class="c2-nav-link"[^>]*>`)

// s5_2AriaLabelAttrRe extracts an aria-label attribute's value from a tag.
var s5_2AriaLabelAttrRe = regexp.MustCompile(`aria-label="([^"]*)"`)

// TestS5_2NavLinksHaveAccessibleNames (MAJOR-1) proves all seven top-level
// .c2-nav-link anchors carry a non-empty aria-label in both languages, so
// they still have an accessible name once .c2-nav-label is display:none in
// rail mode. Child Health/Logs links are plain visible-text links and are
// deliberately not part of this contract.
func TestS5_2NavLinksHaveAccessibleNames(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/overview", lang)
		tags := s5_2NavLinkTagRe.FindAllString(body, -1)
		if len(tags) != 7 {
			t.Fatalf("[%s] expected exactly 7 top-level .c2-nav-link anchors, found %d", lang, len(tags))
		}
		for _, tag := range tags {
			m := s5_2AriaLabelAttrRe.FindStringSubmatch(tag)
			if m == nil || strings.TrimSpace(m[1]) == "" {
				t.Errorf("[%s] top-level nav link missing a non-empty aria-label: %s", lang, tag)
			}
		}
	}
}

// s5_2DrawerCloseVisibleBlock is the exact .chrome-drawer-close visible rule
// shared by both drawer ranges (<=900px and 901-1023.98px) in input.css.
const s5_2DrawerCloseVisibleBlock = "  .chrome-drawer-close {\n    display: inline-flex;\n    position: absolute;\n    top: 0.75rem;\n    right: 0.75rem;\n    align-items: center;\n    justify-content: center;\n    width: 2rem;\n    height: 2rem;\n    border: none;\n    border-radius: var(--ds-radius-sm);\n    background: transparent;\n    color: var(--text-secondary);\n  }"

// TestS5_2DrawerCloseVisibilitySourceContract (MAJOR-2, MAJOR-A) proves
// .chrome-drawer-close is hidden at base scope, shown identically inside
// both drawer ranges (<=900px and 901-1023.98px), never styled visible by
// the >=1024px rail or >=1280px sidebar rules, and — MAJOR-A — that the
// base hidden rule actually WINS the cascade at base scope: it must occur
// BEFORE both responsive visible overrides in source order in both
// input.css and the generated app.css, or the later-cascading hidden rule
// wins by source order regardless of the visible override's own presence,
// hiding the close button across the full <=900px (and 901-1023.98px)
// drawer range.
func TestS5_2DrawerCloseVisibilitySourceContract(t *testing.T) {
	css := readEmbeddedStatic(t, "static/css/input.css")

	baseRule := ".chrome-drawer-close { display: none; }"
	if n := strings.Count(css, baseRule); n != 1 {
		t.Fatalf("expected exactly one base .chrome-drawer-close hidden rule, found %d", n)
	}
	// This file's convention: top-level rules sit at column 0, rules nested
	// inside a @media block are indented. A zero-indent occurrence on its
	// own line proves the base rule isn't nested inside any media block.
	if !strings.Contains(css, "\n"+baseRule+"\n") {
		t.Error(".chrome-drawer-close base display:none must be a top-level (non-indented, non-nested) rule")
	}

	if n := strings.Count(css, s5_2DrawerCloseVisibleBlock); n != 2 {
		t.Fatalf("expected the identical .chrome-drawer-close visible rule inside both drawer media blocks, found %d", n)
	}

	idxRail := strings.Index(css, "@media (min-width: 1024px) and (max-width: 1279.98px)")
	idxSidebar := strings.Index(css, "@media (min-width: 1280px)")
	if idxRail < 0 || idxSidebar < 0 || idxSidebar <= idxRail {
		t.Fatal("expected rail/sidebar breakpoints not found")
	}
	if strings.Contains(css[idxRail:idxSidebar], ".chrome-drawer-close") {
		t.Error("the 1024-1279.98px rail must not style .chrome-drawer-close visible")
	}
	if strings.Contains(css[idxSidebar:], ".chrome-drawer-close") {
		t.Error("the >=1280px sidebar must not style .chrome-drawer-close visible")
	}

	// MAJOR-A: effective cascade order in input.css. The unscoped hidden
	// rule must precede both drawer-range visible overrides in source
	// order — source-order cascade, not mere rule presence, decides which
	// one actually renders at each breakpoint.
	hiddenIdx := strings.Index(css, baseRule)
	media900Idx := strings.Index(css, "@media (max-width: 900px)")
	media901Idx := strings.Index(css, "@media (min-width: 901px) and (max-width: 1023.98px)")
	if hiddenIdx < 0 || media900Idx < 0 || media901Idx < 0 {
		t.Fatal("expected base rule and both drawer media queries to be present in input.css")
	}
	if hiddenIdx > media900Idx {
		t.Error("input.css: unscoped .chrome-drawer-close hidden rule occurs AFTER the <=900px visible override and therefore wins the cascade, hiding the close button across the full <=900px drawer range")
	}
	if hiddenIdx > media901Idx {
		t.Error("input.css: unscoped .chrome-drawer-close hidden rule occurs AFTER the 901-1023.98px visible override and therefore wins the cascade, hiding the close button in that range")
	}

	// MAJOR-A: the same effective cascade order must survive the Tailwind
	// build into the generated (minified) app.css.
	appCSS := readEmbeddedStatic(t, "static/css/app.css")
	hiddenIdxApp := strings.Index(appCSS, ".chrome-drawer-close{display:none}")
	media900IdxApp := strings.Index(appCSS, "@media (max-width:900px)")
	media901IdxApp := strings.Index(appCSS, "@media (min-width:901px) and (max-width:1023.98px)")
	if hiddenIdxApp < 0 || media900IdxApp < 0 || media901IdxApp < 0 {
		t.Fatal("expected base rule and both drawer media queries to be present in generated app.css")
	}
	if hiddenIdxApp > media900IdxApp {
		t.Error("app.css: unscoped .chrome-drawer-close hidden rule occurs AFTER the <=900px visible override and therefore wins the cascade")
	}
	if hiddenIdxApp > media901IdxApp {
		t.Error("app.css: unscoped .chrome-drawer-close hidden rule occurs AFTER the 901-1023.98px visible override and therefore wins the cascade")
	}
}

// TestS5_2SendSkipClearsStaleFailureBeforeSuccessToast (MINOR-1, MAJOR-C)
// proves the sendSkip success path clears the SAME failure region the
// failure path would have written to — the card's own [data-live-error] via
// showLiveError when the retry came from a card checkbox, or the page-level
// #ov-action-error via clearInlineFailure for the non-card (Undo) path —
// before showing the success toast, without touching the request, endpoint,
// refresh, checkbox-revert or failure-fallback behavior.
func TestS5_2SendSkipClearsStaleFailureBeforeSuccessToast(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/overview.html")

	start := strings.Index(src, "async function sendSkip(")
	end := strings.Index(src, "function updateBusy()")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("could not locate the sendSkip function body in overview.html")
	}
	fn := src[start:end]

	// The manual card must be resolved exactly once, before the try block —
	// the same lookup backs both the success and failure region selection,
	// so they can never diverge.
	if n := strings.Count(fn, "input.closest('[data-manual]')"); n != 1 {
		t.Fatalf("expected exactly one input.closest('[data-manual]') resolution in sendSkip, found %d", n)
	}
	manualIdx := strings.Index(fn, "input.closest('[data-manual]')")
	tryIdx := strings.Index(fn, "try {")
	if manualIdx < 0 || tryIdx < 0 || manualIdx > tryIdx {
		t.Error("the manual card must be resolved before the try block, so the success and failure paths share the same lookup")
	}

	// Success must clear the manual card's region when present, and fall
	// back to the page-level region otherwise — the identical branch shape
	// the failure path already uses.
	wantSuccessClear := "if (manual) { showLiveError(manual, ''); } else { clearInlineFailure('ov-action-error'); }"
	if n := strings.Count(fn, wantSuccessClear); n != 1 {
		t.Fatalf("expected exactly one matching-region success clear %q in sendSkip, found %d", wantSuccessClear, n)
	}

	clearIdx := strings.Index(fn, wantSuccessClear)
	toastIdx := strings.Index(fn, "showToast(data.message")
	if toastIdx < 0 {
		t.Fatal("sendSkip success path must still show the success toast")
	}
	if clearIdx > toastIdx {
		t.Error("the matching-region success clear must run before the success toast in sendSkip")
	}

	// Failure behavior — including the manual-vs-fallback region choice,
	// the revert and the request itself — must remain untouched.
	if !strings.Contains(fn, `if (manual) { showLiveError(manual, msg); } else { showInlineFailure('ov-action-error', msg); }`) {
		t.Error("sendSkip must retain its existing manual-vs-fallback failure region selection")
	}
	if !strings.Contains(fn, `input.checked = !skip; // revert`) {
		t.Error("sendSkip must still revert the checkbox on failure")
	}
	if !strings.Contains(fn, `fetch('/api/prediction/skip'`) {
		t.Error("sendSkip must still POST to /api/prediction/skip")
	}
	if !strings.Contains(fn, `body: JSON.stringify({ eventId: eventId, skip: skip })`) {
		t.Error("sendSkip must still send the same request body")
	}
	if !strings.Contains(fn, `htmx.trigger('#overview-live', 'refresh')`) {
		t.Error("sendSkip must still trigger the overview-live HTMX refresh on success")
	}
}

// TestS5_2SystemFlyoutLabelAndChildrenDoNotShareAnchor (MINOR-2) proves the
// rail-mode System group label (vertically centered on the parent item) and
// its Health/Logs children flyout no longer anchor to the same top offset,
// while both keep using the existing --z-rail tier.
func TestS5_2SystemFlyoutLabelAndChildrenDoNotShareAnchor(t *testing.T) {
	css := readEmbeddedStatic(t, "static/css/input.css")

	railIdx := strings.Index(css, "@media (min-width: 1024px) and (max-width: 1279.98px)")
	sidebarIdx := strings.Index(css, "@media (min-width: 1280px)")
	if railIdx < 0 || sidebarIdx < 0 || sidebarIdx <= railIdx {
		t.Fatal("expected rail breakpoint block not found")
	}
	rail := css[railIdx:sidebarIdx]

	// (MAJOR) The rail is the only breakpoint that paints the label/children
	// flyouts outside the sidebar's own box (via left: 100%); its base
	// .app-sidebar rule clips to that box (overflow: hidden), so only the
	// rail block may remove the clipping ancestor — the drawer ranges and
	// the >=1280px expanded sidebar must never widen along with it.
	if !strings.Contains(rail, "overflow: visible;") {
		t.Error("the 1024-1279.98px rail block must set .app-sidebar overflow: visible so the label/children flyouts are not clipped by the sidebar's own box")
	}
	if strings.Contains(css[sidebarIdx:], "overflow: visible;") {
		t.Error("the >=1280px expanded sidebar must not carry the rail-only overflow: visible override")
	}

	labelIdx := strings.Index(rail, ".c2-nav-item:hover .c2-nav-label")
	childrenIdx := strings.Index(rail, ".c2-nav-item--group:hover .c2-nav-children")
	if labelIdx < 0 || childrenIdx < 0 {
		t.Fatal("expected rail flyout label/children rules not found")
	}
	labelEnd := labelIdx + strings.Index(rail[labelIdx:], "}")
	labelBlock := rail[labelIdx:labelEnd]
	childrenEnd := childrenIdx + strings.Index(rail[childrenIdx:], "}")
	childrenBlock := rail[childrenIdx:childrenEnd]

	if !strings.Contains(labelBlock, "top: 50%;") {
		t.Error("the rail flyout label must stay vertically centered on the parent item (top: 50%)")
	}
	if strings.Contains(childrenBlock, "top: 0;") {
		t.Error("the rail children flyout must not anchor at top: 0 — it collides with the vertically-centered label")
	}
	if !strings.Contains(childrenBlock, "top: 100%;") {
		t.Error("the rail children flyout must anchor below the parent item (top: 100%) to avoid overlapping the label")
	}
	if strings.Count(rail, "z-index: var(--z-rail);") < 2 {
		t.Error("both the label and children flyout must keep using the existing --z-rail tier")
	}
}

// s5_2LegacyNeutralUtilityRe matches any legacy neutral-scale color utility
// (text-neutral-*, bg-neutral-*, border-neutral-*, ...).
var s5_2LegacyNeutralUtilityRe = regexp.MustCompile(`[a-z]+-neutral-\d+`)

// TestS5_2NewPagesUseSemanticTextUtilities (MINOR-3) proves help.html,
// events.html and the new S5-2 component templates use the S5-1 semantic
// text-text-primary/secondary/muted utilities instead of legacy
// text-neutral-100/300/400 (or any other neutral-scale color utility).
func TestS5_2NewPagesUseSemanticTextUtilities(t *testing.T) {
	for _, name := range []string{
		"templates/help.html",
		"templates/events.html",
		"templates/components/c0_provenance_chip.html",
		"templates/components/c1_state_block.html",
		"templates/components/c2_nav.html",
		"templates/components/c10_badge.html",
		"templates/components/c11_progress.html",
		"templates/components/c17_toast_region.html",
	} {
		src := readEmbeddedTemplate(t, name)
		if m := s5_2LegacyNeutralUtilityRe.FindAllString(src, -1); len(m) > 0 {
			t.Errorf("%s must use semantic text-text-* utilities, found legacy neutral-scale utilities: %v", name, m)
		}
	}
}

// extractMediaBlock returns the exact body of the single CSS at-rule whose
// header text is header (e.g. "@media (max-width: 900px)" or its minified
// form "@media (max-width:900px)"), located deterministically by finding
// header's first occurrence and then walking forward from its opening "{"
// with balanced-brace counting to the matching closing "}". This is
// (MINOR-A) — a plain, dependency-free replacement for slicing from one
// media-query header to the start of some other, unrelated header, which
// can silently pull in everything in between (including hundreds of
// unrelated lines) and let required literals be satisfied by CSS that
// isn't actually inside the target block. It fails the test clearly if the
// header is missing or the braces never balance.
func extractMediaBlock(t *testing.T, css, header string) string {
	t.Helper()
	headerIdx := strings.Index(css, header)
	if headerIdx < 0 {
		t.Fatalf("media query header %q not found in CSS", header)
	}
	rel := strings.IndexByte(css[headerIdx:], '{')
	if rel < 0 {
		t.Fatalf("no opening brace found after media query header %q", header)
	}
	openIdx := headerIdx + rel

	depth := 0
	for i := openIdx; i < len(css); i++ {
		switch css[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return css[openIdx+1 : i]
			}
		}
	}
	t.Fatalf("unbalanced braces: no matching closing brace found for media query %q", header)
	return ""
}

// TestS5_2DrawerMechanicsUnifiedAcrossBothRanges (MINOR-4, MINOR-A) proves
// the <=900px and 901-1023.98px drawer ranges implement the identical
// Stage 4 treatment (260px width, --motion-slow/--motion-easing
// transition, --z-drawer, same open transform, drawer-close visible rule)
// and no longer diverge on the old 240px / 0.2s ease / numeric z-index
// values. MINOR-A: each block is extracted via exact balanced-brace
// parsing (extractMediaBlock), not a broad slice to the next unrelated
// media-query header, so a required literal sitting anywhere else in the
// stylesheet cannot satisfy this test.
func TestS5_2DrawerMechanicsUnifiedAcrossBothRanges(t *testing.T) {
	css := readEmbeddedStatic(t, "static/css/input.css")

	block900 := extractMediaBlock(t, css, "@media (max-width: 900px)")
	block901 := extractMediaBlock(t, css, "@media (min-width: 901px) and (max-width: 1023.98px)")

	for _, want := range []string{
		"width: 260px;",
		"transition: transform var(--motion-slow) var(--motion-easing);",
		"z-index: var(--z-drawer);",
		"transform: translateX(-100%);",
		".is-open { transform: translateX(0); }",
		s5_2DrawerCloseVisibleBlock,
	} {
		if !strings.Contains(block900, want) {
			t.Errorf("<=900px drawer block missing unified literal %q", want)
		}
		if !strings.Contains(block901, want) {
			t.Errorf("901-1023.98px drawer block missing unified literal %q", want)
		}
	}

	for _, banned := range []string{"width: 240px;", "transition: transform 0.2s ease;", "z-index: 60;"} {
		if strings.Contains(block900, banned) {
			t.Errorf("<=900px drawer block must not keep the old divergent literal %q", banned)
		}
		if strings.Contains(block901, banned) {
			t.Errorf("901-1023.98px drawer block must not keep the old divergent literal %q", banned)
		}
	}
}

// TestS5_2ScrimAndScrollLockResizeSafe (MINOR-B) proves the drawer scrim's
// active (.is-open) rule and the body scroll lock only take effect inside
// a media block scoped below 1024px — so resizing to >=1024px (rail/
// sidebar layouts) leaves any stale .is-open/.chrome-scroll-lock class in
// the DOM with no visual or scroll effect — and that the generated
// app.css preserves the same effective media scoping. Uses balanced-brace
// media-block extraction (extractMediaBlock), not a broad substring.
func TestS5_2ScrimAndScrollLockResizeSafe(t *testing.T) {
	css := readEmbeddedStatic(t, "static/css/input.css")

	scrimActive := ".chrome-scrim.is-open { display: block; }"
	scrollLockActive := "body.chrome-scroll-lock { overflow: hidden; }"

	// Exactly one occurrence of each active rule anywhere in the file: if a
	// stray unconditional copy existed alongside the properly scoped one,
	// this count would be 2, and no equivalent unconditional active rule
	// can remain undetected.
	if n := strings.Count(css, scrimActive); n != 1 {
		t.Fatalf("expected exactly one .chrome-scrim.is-open active rule, found %d", n)
	}
	if n := strings.Count(css, scrollLockActive); n != 1 {
		t.Fatalf("expected exactly one body.chrome-scroll-lock active rule, found %d", n)
	}
	// A zero-indent, non-nested occurrence on its own line would mean the
	// rule is unconditionally active at every viewport width, including
	// >=1024px — the same top-level-vs-nested convention used elsewhere in
	// this file to prove a rule sits inside, not outside, a @media block.
	if strings.Contains(css, "\n"+scrimActive+"\n") {
		t.Error(".chrome-scrim.is-open must not be an unconditional top-level rule — it must be scoped to a <1024px media block")
	}
	if strings.Contains(css, "\n"+scrollLockActive+"\n") {
		t.Error("body.chrome-scroll-lock must not be an unconditional top-level rule — it must be scoped to a <1024px media block")
	}

	block := extractMediaBlock(t, css, "@media (max-width: 1023.98px)")
	if !strings.Contains(block, scrimActive) {
		t.Error("input.css: the <1024px media block must contain the .chrome-scrim.is-open active rule")
	}
	if !strings.Contains(block, scrollLockActive) {
		t.Error("input.css: the <1024px media block must contain the body.chrome-scroll-lock active rule")
	}

	// The same effective scoping must survive the Tailwind build into the
	// generated (minified) app.css.
	appCSS := readEmbeddedStatic(t, "static/css/app.css")
	scrimActiveMin := ".chrome-scrim.is-open{display:block}"
	scrollLockActiveMin := "body.chrome-scroll-lock{overflow:hidden}"

	if n := strings.Count(appCSS, scrimActiveMin); n != 1 {
		t.Fatalf("generated app.css: expected exactly one .chrome-scrim.is-open active rule, found %d", n)
	}
	if n := strings.Count(appCSS, scrollLockActiveMin); n != 1 {
		t.Fatalf("generated app.css: expected exactly one body.chrome-scroll-lock active rule, found %d", n)
	}
	appBlock := extractMediaBlock(t, appCSS, "@media (max-width:1023.98px)")
	if !strings.Contains(appBlock, scrimActiveMin) {
		t.Error("generated app.css: the <1024px media block must contain the .chrome-scrim.is-open active rule")
	}
	if !strings.Contains(appBlock, scrollLockActiveMin) {
		t.Error("generated app.css: the <1024px media block must contain the body.chrome-scroll-lock active rule")
	}
}

// TestS5_2RailContainsNonFlyoutRows (MAJOR-1) proves the rail-scoped
// .app-sidebar overflow:visible override — required so the label/children
// flyouts (positioned via left:100%) escape the sidebar's own box — does
// not also let the two ordinary (non-flyout) rows it contains, the brand
// row and the username/version footer, paint outside the 56px rail. Each
// row carries a stable semantic class and is contained only inside the
// rail's own media block (1024-1279.98px); the drawer ranges and the
// >=1280px expanded sidebar must never see either rule.
func TestS5_2RailContainsNonFlyoutRows(t *testing.T) {
	base := readEmbeddedTemplate(t, "templates/base.html")

	if n := strings.Count(base, "sidebar-brand-row"); n != 1 {
		t.Fatalf("expected exactly one sidebar-brand-row class in base.html, found %d", n)
	}
	if n := strings.Count(base, "sidebar-footer"); n != 1 {
		t.Fatalf("expected exactly one sidebar-footer class in base.html, found %d", n)
	}

	css := readEmbeddedStatic(t, "static/css/input.css")

	// Exactly one occurrence of each containment rule anywhere in the file —
	// if either existed a second time outside the rail block, that copy
	// could satisfy the assertions below without the rail block itself
	// carrying the fix.
	if n := strings.Count(css, ".sidebar-brand-row {"); n != 1 {
		t.Fatalf("expected exactly one .sidebar-brand-row rule in input.css, found %d", n)
	}
	if n := strings.Count(css, ".sidebar-footer {"); n != 1 {
		t.Fatalf("expected exactly one .sidebar-footer rule in input.css, found %d", n)
	}

	rail := extractMediaBlock(t, css, "@media (min-width: 1024px) and (max-width: 1279.98px)")

	if !strings.Contains(rail, ".app-sidebar { width: 56px; overflow: visible; }") {
		t.Error("rail media block must keep .app-sidebar overflow:visible — required by the label/children flyouts")
	}

	brandRule := extractMediaBlock(t, rail, ".sidebar-brand-row")
	for _, want := range []string{"justify-content: center;", "padding-left: 0;", "padding-right: 0;", "overflow: hidden;"} {
		if !strings.Contains(brandRule, want) {
			t.Errorf("rail .sidebar-brand-row rule missing containment literal %q", want)
		}
	}

	footerRule := extractMediaBlock(t, rail, ".sidebar-footer")
	if strings.TrimSpace(footerRule) != "display: none;" {
		t.Errorf("rail .sidebar-footer rule must be exactly `display: none;`, got %q", strings.TrimSpace(footerRule))
	}

	// The drawer ranges (<=900px, 901-1023.98px) and the >=1280px expanded
	// sidebar must never hide the footer or narrow/center the brand row —
	// those rows must render normally there, with the row's own content
	// (the already-hidden .sidebar-brand span) untouched.
	sidebarIdx := strings.Index(css, "@media (min-width: 1280px)")
	if sidebarIdx < 0 {
		t.Fatal("expected >=1280px sidebar block not found")
	}
	if strings.Contains(css[sidebarIdx:], "sidebar-footer") || strings.Contains(css[sidebarIdx:], "sidebar-brand-row") {
		t.Error(">=1280px expanded sidebar must not hide .sidebar-footer or contain .sidebar-brand-row")
	}
	drawer900 := extractMediaBlock(t, css, "@media (max-width: 900px)")
	drawer901 := extractMediaBlock(t, css, "@media (min-width: 901px) and (max-width: 1023.98px)")
	for _, block := range []string{drawer900, drawer901} {
		if strings.Contains(block, "sidebar-footer") || strings.Contains(block, "sidebar-brand-row") {
			t.Error("drawer media blocks must not hide .sidebar-footer or contain .sidebar-brand-row")
		}
	}
	if !strings.Contains(css, ".sidebar-brand, .now-watching, .c2-nav-label, .c2-nav-children {\n    display: none;\n  }") {
		t.Error("rail block must still hide .sidebar-brand text inside the (now contained) brand row")
	}
}

// TestS5_2LegacyFixedToastUtilityRemoved (MAJOR-2) proves the legacy
// @utility toast/toast-success/toast-error declarations — which generated
// fixed-position rules, offset from the viewport's own edges, on a raw
// z-index, that took every toast out of the C17 flex stack's flow and
// ignored its desktop/mobile positioning and --z-toast tier — are gone
// from the source, while the unlayered C17 stack and its .toast/
// .toast-success visual rules (border/background/shadow/padding,
// success/neutral-only behaviour) are untouched, and the same holds in
// the generated app.css.
//
// Note on literals below: this repo's Tailwind build auto-scans the whole
// module for class candidates (this package's own _test.go files are not
// .gitignore'd out of that scan), so an assertion string equal to an
// actual Tailwind spacing/z-index utility name would itself get
// regenerated into app.css. The checks here instead match the effective
// *computed* CSS text those utilities expand to (e.g. the calc() form of
// a spacing scale value), which is not itself a utility-candidate token.
func TestS5_2LegacyFixedToastUtilityRemoved(t *testing.T) {
	css := readEmbeddedStatic(t, "static/css/input.css")

	for _, banned := range []string{"@utility toast {", "@utility toast-success {", "@utility toast-error {"} {
		if strings.Contains(css, banned) {
			t.Errorf("input.css must not define the legacy fixed-position utility %q", banned)
		}
	}

	if !strings.Contains(css, ".c17-toast-stack {") {
		t.Fatal("input.css must still define .c17-toast-stack")
	}
	stack := extractMediaBlock(t, css, ".c17-toast-stack")
	for _, want := range []string{"position: fixed;", "right: 1rem;", "bottom: 1rem;", "z-index: var(--z-toast);", "display: flex;", "flex-direction: column;", "gap: 0.5rem;"} {
		if !strings.Contains(stack, want) {
			t.Errorf(".c17-toast-stack missing literal %q", want)
		}
	}
	if !strings.Contains(css, ".c17-toast-stack { left: 0.75rem; right: 0.75rem; bottom: 0.75rem; max-width: none; }") {
		t.Error("input.css must keep the mobile .c17-toast-stack placement override")
	}

	toastRule := extractMediaBlock(t, css, ".toast {")
	if strings.Contains(toastRule, "position") {
		t.Errorf(".toast must remain a normal flex-flow item, found a position declaration: %q", toastRule)
	}
	for _, want := range []string{"padding: 0.6rem 0.9rem;", "border-radius: var(--ds-radius-sm);", "box-shadow: var(--shadow-1);", "border: 1px solid var(--state-ok);", "background: var(--state-ok-bg);"} {
		if !strings.Contains(toastRule, want) {
			t.Errorf(".toast missing visual literal %q", want)
		}
	}
	if !strings.Contains(css, ".toast-success { border-color: var(--state-ok); background: var(--state-ok-bg); }") {
		t.Error("input.css must keep .toast-success success-only styling")
	}

	// The same effective behaviour must survive the Tailwind build: no
	// generated rule for the .toast/.toast-success/.toast-error selectors
	// may carry the fixed-position/offset/raw-z-index computed values the
	// legacy utilities used to inject — scoped to those selectors only,
	// since unrelated pre-existing overlays may legitimately use the same
	// computed values elsewhere in the generated stylesheet.
	appCSS := readEmbeddedStatic(t, "static/css/app.css")
	toastSelectorRe := regexp.MustCompile(`\.toast(?:-success|-error)?\{[^}]*\}`)
	for _, m := range toastSelectorRe.FindAllString(appCSS, -1) {
		for _, banned := range []string{"position:fixed", "right:calc(var(--spacing)*8)", "bottom:calc(var(--spacing)*8)", "z-index:1000"} {
			if strings.Contains(m, banned) {
				t.Errorf("generated app.css: toast rule %q must not carry legacy fixed-position literal %q", m, banned)
			}
		}
	}
	if !strings.Contains(appCSS, ".c17-toast-stack{") {
		t.Error("generated app.css must still define .c17-toast-stack")
	}
}
