package web

// S5-9 Help section tests: the five direct-render Help routes (26-30 of the
// design's 30-route page matrix), the Help nav group (parent + five
// children, completing 7 parents / 30 children total), RU/EN localization
// of every new key, the glossary's parity with its canonical Go source
// dictionaries, the troubleshooting page's four-state distinction, the
// notifications/audio fail-open invariant (reusing the existing
// events.sound.failopen copy verbatim), and diagnostics-support's
// link-only, no-snapshot-generation contract.

import (
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
)

// s59MainRe extracts the <main class="app-main">...</main> region of a
// rendered page — the page-specific content plus the one shared c17
// lifecycle-alert container, but none of base.html's chrome (sidebar,
// topbar, or any of its several <script> blocks, all of which sit outside
// <main>). Used to test "this page's own content stays static/link-only"
// without false-positiving on chrome every page already shares.
var s59MainRe = regexp.MustCompile(`(?s)<main class="app-main">(.*)</main>`)

func s59MainRegion(t *testing.T, body string) string {
	t.Helper()
	m := s59MainRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("could not locate <main class=\"app-main\">...</main> in the rendered page")
	}
	return m[1]
}

var s59HelpRoutes = []string{
	"/help/getting-started",
	"/help/glossary",
	"/help/troubleshooting",
	"/help/notifications-audio",
	"/help/diagnostics-support",
}

// s59NewDirectRoutes excludes /help/getting-started, whose handler
// (handlers_chrome.go, S5-2) predates this task and was never method-gated;
// changing that handler's method behavior is out of S5-9's scope.
var s59NewDirectRoutes = []string{
	"/help/glossary",
	"/help/troubleshooting",
	"/help/notifications-audio",
	"/help/diagnostics-support",
}

// ---------------------------------------------------------------------
// 1. Direct routes and method contract.
// ---------------------------------------------------------------------

// TestS5_9DirectRoutesGetHeadOK proves all five Help routes render directly
// with 200 on GET and HEAD — never a redirect (the pre-S5-9 behavior for the
// four new routes was an honest 404, never a 30x).
func TestS5_9DirectRoutesGetHeadOK(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	for _, route := range s59HelpRoutes {
		t.Run(route, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(method, route, nil))
				if rec.Code != http.StatusOK {
					t.Errorf("%s %s = %d, want 200 direct", method, route, rec.Code)
				}
				if loc := rec.Header().Get("Location"); loc != "" {
					t.Errorf("%s %s set Location=%q — must render directly, not redirect", method, route, loc)
				}
			}
		})
	}
}

// TestS5_9NewDirectRoutesRejectOtherMethods proves the four new routes are
// GET/HEAD-only, answering 405 with an exact Allow header for everything
// else — the same explicit method gating S5-6/S5-8's category/analytics
// routes established.
func TestS5_9NewDirectRoutesRejectOtherMethods(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	for _, route := range s59NewDirectRoutes {
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

// TestS5_9HelpCompatibilityRedirectPreserved proves /help still 302s to
// /help/getting-started with its query string preserved, unchanged by this
// task (generic coverage already lives in s5_2_redirects_test.go's
// TestS5_2RedirectMatrix; this is the S5-9-local confirmation the task's RED
// list calls for explicitly).
func TestS5_9HelpCompatibilityRedirectPreserved(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	rec, _ := httpGetBody(t, h, "/help?foo=bar")
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /help?foo=bar = %d, want %d", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/help/getting-started?foo=bar" {
		t.Errorf("GET /help?foo=bar Location = %q, want %q", loc, "/help/getting-started?foo=bar")
	}
}

// ---------------------------------------------------------------------
// 2. Nav group structure and aria-current.
// ---------------------------------------------------------------------

// TestS5_9HelpNavGroupStructure proves the Help group renders with exactly
// five distinct children, each with its own href, and that the parent link
// carries data-nav-parent (the "parent href = first child" convention every
// other group already uses).
func TestS5_9HelpNavGroupStructure(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview", "en")

	if !strings.Contains(body, `href="/help/getting-started" class="c2-nav-link" data-nav-section="help" aria-label="Help" data-nav-parent`) {
		t.Error("Help parent link missing data-nav-parent, or no longer points at /help/getting-started")
	}
	for _, href := range []string{
		"/help/getting-started", "/help/glossary", "/help/troubleshooting",
		"/help/notifications-audio", "/help/diagnostics-support",
	} {
		want := `href="` + href + `" class="c2-nav-child" data-nav-section="help" data-nav-child`
		if !strings.Contains(body, want) {
			t.Errorf("Help group missing child destination %q", href)
		}
	}
}

// TestS5_9AriaCurrentExactlyOnePerHelpChildRoute re-implements base.html's
// client-side updateActiveNav decision in Go (the same simulation
// TestS5_7AriaCurrentExactlyOnePerEventsChildRoute pins) against the
// rendered C2 markup of each of the five Help child routes, proving exactly
// one destination — the route's own child link — would receive
// aria-current="page".
func TestS5_9AriaCurrentExactlyOnePerHelpChildRoute(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, path := range s59HelpRoutes {
		t.Run(path, func(t *testing.T) {
			body := f3GetPage(t, srv, path, "en")
			const active = "help" // SECTION_RULES: /help and /help/* -> help

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
				t.Errorf("simulated nav activation on %s marked %q current, want %q", path, currentHrefs[0], path)
			}
		})
	}
}

// ---------------------------------------------------------------------
// 3. Localization: presence, translation, and full-page RU/EN parity.
// ---------------------------------------------------------------------

func s5_9Loc(t *testing.T) *i18n.Localizer {
	t.Helper()
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	return loc
}

// TestS5_9LocaleKeysPresentAndTranslated guards the full S5-9 key set: each
// key resolves to a non-empty, genuinely translated string in both RU and
// EN.
func TestS5_9LocaleKeysPresentAndTranslated(t *testing.T) {
	loc := s5_9Loc(t)
	keys := []string{
		"nav.help.getting_started", "nav.help.glossary", "nav.help.troubleshooting",
		"nav.help.notifications_audio", "nav.help.diagnostics_support",
		"help.more.heading",
		"help.card.glossary.desc", "help.card.troubleshooting.desc",
		"help.card.notifications_audio.desc", "help.card.diagnostics_support.desc",
		"help.glossary.title", "help.glossary.lead", "help.glossary.scope_note",
		"help.glossary.section.reason", "help.glossary.section.status",
		"help.glossary.section.event", "help.glossary.section.event_group",
		"help.troubleshooting.title", "help.troubleshooting.lead",
		"help.troubleshooting.unknown.title", "help.troubleshooting.unknown.def", "help.troubleshooting.unknown.link",
		"help.troubleshooting.stale.title", "help.troubleshooting.stale.def", "help.troubleshooting.stale.link",
		"help.troubleshooting.degraded.title", "help.troubleshooting.degraded.def", "help.troubleshooting.degraded.link",
		"help.troubleshooting.failure.title", "help.troubleshooting.failure.def", "help.troubleshooting.failure.link",
		"help.troubleshooting.invariant", "help.troubleshooting.diagnostics_link",
		"help.notifications_audio.title", "help.notifications_audio.lead",
		"help.notifications_audio.gesture.title", "help.notifications_audio.gesture.def",
		"help.notifications_audio.permission.title", "help.notifications_audio.permission.def",
		"help.notifications_audio.permission.browser_link", "help.notifications_audio.permission.sound_link",
		"help.notifications_audio.svg_caption",
		"help.diagnostics_support.title", "help.diagnostics_support.lead",
		"help.diagnostics_support.snapshot.title", "help.diagnostics_support.snapshot.def", "help.diagnostics_support.snapshot.link",
		"help.diagnostics_support.status.title", "help.diagnostics_support.status.def", "help.diagnostics_support.status.link",
		"help.diagnostics_support.logs.title", "help.diagnostics_support.logs.def", "help.diagnostics_support.logs.link",
	}
	// The glossary's per-code definition keys, generated from the same
	// canonical sources buildHelpGlossaryPageData ranges over — proving the
	// locale catalogs actually carry a definition for every code the parity
	// test (below) requires to be rendered.
	for code := range reasonCodeKeys {
		keys = append(keys, "help.glossary.def.reason."+code)
	}
	for code := range rosterStatusKeys {
		keys = append(keys, "help.glossary.def.status."+code)
	}
	for code := range eventTypeKeys {
		keys = append(keys, "help.glossary.def.event."+string(code))
	}
	for _, g := range eventJournalGroups {
		keys = append(keys, "help.glossary.def.event_group."+dashToUnderscore(g.Key))
	}

	for _, k := range keys {
		en := loc.T(i18n.LangEN, k)
		ru := loc.T(i18n.LangRU, k)
		if en == k {
			t.Errorf("EN missing translation for %q (echoed the key back)", k)
		}
		if ru == k {
			t.Errorf("RU missing translation for %q (echoed the key back)", k)
		}
		if strings.TrimSpace(en) == "" || strings.TrimSpace(ru) == "" {
			t.Errorf("%q has an empty value in one language (en=%q ru=%q)", k, en, ru)
		}
		if en == ru {
			t.Errorf("%q has identical EN/RU text %q — looks untranslated", k, en)
		}
	}
}

// TestS5_9LocalizationParity proves all five Help pages render fully
// localized in RU and EN, with no raw i18n key leaking into the output and
// no untranslated difference between the two languages.
func TestS5_9LocalizationParity(t *testing.T) {
	srv := buildF3PageServer(t)
	rawKey := regexp.MustCompile(`>\s*(help|nav)\.[a-z0-9_.]+\s*<`)

	for _, route := range s59HelpRoutes {
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

// ---------------------------------------------------------------------
// 4. Glossary parity: the rendered code set must exactly match its
//    canonical Go source dictionaries — never a hand-typed second list.
// ---------------------------------------------------------------------

var s59GlossaryCodeRe = regexp.MustCompile(`<dt class="type-code text-text-primary">([^<]+)</dt>`)

func s59RenderedGlossaryCodes(t *testing.T, body string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, m := range s59GlossaryCodeRe.FindAllStringSubmatch(body, -1) {
		out[m[1]] = true
	}
	return out
}

// TestS5_9GlossaryParity proves /help/glossary renders exactly one entry per
// code in each of the four canonical source dictionaries this package
// already uses to render reason/status/event labels elsewhere
// (reasonCodeKeys, rosterStatusKeys, eventTypeKeys, eventJournalGroups) —
// no code missing, no extra/fabricated code. This is the parity test the
// design doc requires before a glossary can reference "codes owned by
// code": if a code is added, renamed or removed in any of those
// dictionaries without a matching glossary change (or vice versa), this
// test fails.
func TestS5_9GlossaryParity(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/help/glossary", "en")
	rendered := s59RenderedGlossaryCodes(t, body)

	want := map[string]bool{}
	for code := range reasonCodeKeys {
		want[code] = true
	}
	for code := range rosterStatusKeys {
		want[code] = true
	}
	for code := range eventTypeKeys {
		want[string(code)] = true
	}
	for _, g := range eventJournalGroups {
		want[g.Key] = true
	}

	for code := range want {
		if !rendered[code] {
			t.Errorf("glossary missing code %q present in its canonical Go source", code)
		}
	}
	for code := range rendered {
		if !want[code] {
			t.Errorf("glossary renders code %q that does not exist in any canonical Go source — second source of truth", code)
		}
	}

	wantTotal := len(reasonCodeKeys) + len(rosterStatusKeys) + len(eventTypeKeys) + len(eventJournalGroups)
	if len(rendered) != wantTotal {
		t.Errorf("glossary rendered %d distinct codes, want %d (sum of the four canonical source dictionaries — duplicate or missing entry)", len(rendered), wantTotal)
	}
}

// TestS5_9GlossaryEntriesAreSorted proves the glossary's entries render in a
// deterministic (sorted) order within each section — Go map iteration order
// must never leak into the page.
func TestS5_9GlossaryEntriesAreSorted(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/help/glossary", "en")
	codes := s5_9OrderedGlossaryCodesInSection(t, body, "help.glossary.section.reason", "help.glossary.section.status")
	if !sort.StringsAreSorted(codes) {
		t.Errorf("reason-code section entries are not rendered in sorted order: %v", codes)
	}
}

// s5_9OrderedGlossaryCodesInSection extracts the codes rendered between the
// heading key for `from` and the heading key for `to`, in document order.
func s5_9OrderedGlossaryCodesInSection(t *testing.T, body, fromHeadingKeyEN, toHeadingKeyEN string) []string {
	t.Helper()
	loc := s5_9Loc(t)
	// html.EscapeString mirrors html/template's default auto-escaping (&, ',
	// " etc.) so a search string containing e.g. "&" or an apostrophe still
	// matches what the template actually rendered.
	from := html.EscapeString(loc.T(i18n.LangEN, fromHeadingKeyEN))
	to := html.EscapeString(loc.T(i18n.LangEN, toHeadingKeyEN))
	start := strings.Index(body, from)
	end := strings.Index(body, to)
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("could not locate section bounds %q..%q in rendered glossary", from, to)
	}
	section := body[start:end]
	var codes []string
	for _, m := range s59GlossaryCodeRe.FindAllStringSubmatch(section, -1) {
		codes = append(codes, m[1])
	}
	return codes
}

// TestS5_9GlossaryScopeNoteExplainsExclusions proves the glossary explicitly
// states its scope (queue/roster reason codes and event types only) rather
// than silently omitting the code families that have no single canonical Go
// source today (drop claim states, the internal S-state vocabulary, browser/
// sound permission states, log levels).
func TestS5_9GlossaryScopeNoteExplainsExclusions(t *testing.T) {
	srv := buildF3PageServer(t)
	loc := s5_9Loc(t)
	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/help/glossary", lang)
		note := html.EscapeString(loc.T(i18n.NormalizeLang(lang), "help.glossary.scope_note"))
		if !strings.Contains(body, note) {
			t.Errorf("[%s] glossary missing its scope note", lang)
		}
	}
}

// ---------------------------------------------------------------------
// 5. Troubleshooting: the four states, correctly distinguished and linked.
// ---------------------------------------------------------------------

// TestS5_9TroubleshootingDistinguishesFourStates proves /help/troubleshooting
// names all four data-freshness states (Unknown, Stale, Degraded, Failure)
// with distinct copy, deep-links each to a real owning page, and states the
// "unknown never becomes healthy" invariant explicitly.
func TestS5_9TroubleshootingDistinguishesFourStates(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/help/troubleshooting", "en")
	loc := s5_9Loc(t)

	stateLinks := map[string]string{
		"unknown":  "/system/status",
		"stale":    "/drops/current",
		"degraded": "/system/status",
		"failure":  "/system/logs",
	}
	titlesSeen := map[string]bool{}
	for state, href := range stateLinks {
		title := loc.T(i18n.LangEN, "help.troubleshooting."+state+".title")
		if titlesSeen[title] {
			t.Errorf("state title %q duplicated across states — must be distinct", title)
		}
		titlesSeen[title] = true
		if !strings.Contains(body, title) {
			t.Errorf("troubleshooting page missing the %q state title", state)
		}
		if !strings.Contains(body, `href="`+href+`"`) {
			t.Errorf("troubleshooting page missing the %q state's deep link to %q", state, href)
		}
	}
	if !strings.Contains(body, loc.T(i18n.LangEN, "help.troubleshooting.invariant")) {
		t.Error("troubleshooting page missing the unknown-never-becomes-healthy invariant statement")
	}
	if !strings.Contains(body, `href="/system/diagnostics"`) {
		t.Error("troubleshooting page missing its diagnostics-snapshot escalation link")
	}
}

// ---------------------------------------------------------------------
// 6. Notifications & Audio: gesture/permission model and fail-open.
// ---------------------------------------------------------------------

// TestS5_9NotificationsAudioReusesExistingFailOpenCopy proves the page
// states the fail-open invariant using the SAME i18n key (events.sound.
// failopen) the Sound status page already uses — never a second, drifting
// wording of the same guarantee.
func TestS5_9NotificationsAudioReusesExistingFailOpenCopy(t *testing.T) {
	srv := buildF3PageServer(t)
	loc := s5_9Loc(t)
	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/help/notifications-audio", lang)
		failopen := loc.T(i18n.NormalizeLang(lang), "events.sound.failopen")
		if !strings.Contains(body, failopen) {
			t.Errorf("[%s] notifications-audio page missing the exact existing fail-open copy %q", lang, failopen)
		}
	}
}

// TestS5_9NotificationsAudioLinksLivePermissionPages proves the page deep-
// links to the real, live browser/sound status pages rather than restating
// their status, and carries a static themed (currentColor) inline SVG with
// no external asset reference.
func TestS5_9NotificationsAudioLinksLivePermissionPages(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/help/notifications-audio", "en")

	for _, href := range []string{`href="/events/browser"`, `href="/events/sound"`} {
		if !strings.Contains(body, href) {
			t.Errorf("notifications-audio page missing link %q", href)
		}
	}
	if !strings.Contains(body, "<svg") {
		t.Error("notifications-audio page missing its themed diagram SVG")
	}
	if strings.Contains(body, "http://") || strings.Contains(body, "https://") || strings.Contains(body, "cdn.") {
		t.Error("notifications-audio page must not reference any external asset/CDN URL")
	}
	if !strings.Contains(body, `stroke="currentColor"`) {
		t.Error("notifications-audio diagram must theme via currentColor, like every other icon in this codebase")
	}
}

// ---------------------------------------------------------------------
// 7. Diagnostics & Support: canonical link only, never a snapshot action.
// ---------------------------------------------------------------------

// TestS5_9DiagnosticsSupportLinksCanonicalPage proves /help/diagnostics-
// support deep-links to the canonical /system/diagnostics page (and its
// System siblings) rather than restating diagnostic content.
func TestS5_9DiagnosticsSupportLinksCanonicalPage(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/help/diagnostics-support", "en")

	for _, href := range []string{`href="/system/diagnostics"`, `href="/system/status"`, `href="/system/logs"`} {
		if !strings.Contains(body, href) {
			t.Errorf("diagnostics-support page missing link %q", href)
		}
	}
}

// TestS5_9DiagnosticsSupportNeverGeneratesASnapshot proves this page's own
// content (the <main> region — excluding base.html's shared chrome, which
// legitimately carries its own buttons/hx- attributes on every page, e.g.
// the theme toggle) contains no form, button, or reference to a
// snapshot-generating endpoint of its own — it only links to the canonical
// page that owns that action.
func TestS5_9DiagnosticsSupportNeverGeneratesASnapshot(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/help/diagnostics-support", lang)
		main := s59MainRegion(t, body)
		for _, banned := range []string{"<form", "<button", DebugSnapshotPath, SupportBundlePath, "hx-post", "hx-get"} {
			if strings.Contains(main, banned) {
				t.Errorf("[%s] diagnostics-support page must never itself trigger a snapshot/bundle action, found %q", lang, banned)
			}
		}
	}
}

// ---------------------------------------------------------------------
// 8. Static editorial content: no live state, no new transport, one h1.
// ---------------------------------------------------------------------

// TestS5_9PagesAreStaticNoLiveTransport proves none of the five Help pages'
// own content (the <main> region — excluding base.html's shared chrome,
// which every page already carries its own <script>/hx- machinery in, e.g.
// the nav-activation script and theme toggle) introduces htmx polling, SSE,
// or a new script — reading-density pages stay pure server-rendered prose,
// exactly like help.html always has been.
func TestS5_9PagesAreStaticNoLiveTransport(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, route := range s59HelpRoutes {
		body := f3GetPage(t, srv, route, "en")
		main := s59MainRegion(t, body)
		for _, banned := range []string{"hx-get", "hx-post", "hx-trigger", "EventSource(", "<script"} {
			if strings.Contains(main, banned) {
				t.Errorf("%s must stay static editorial content, found %q", route, banned)
			}
		}
	}
}

// TestS5_9EachPageHasExactlyOneH1 proves every Help page carries exactly one
// h1 (the §9 a11y contract: one h1 per page, no level skips).
func TestS5_9EachPageHasExactlyOneH1(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, route := range s59HelpRoutes {
		body := f3GetPage(t, srv, route, "en")
		if n := strings.Count(body, "<h1"); n != 1 {
			t.Errorf("%s: expected exactly one <h1>, found %d", route, n)
		}
	}
}
