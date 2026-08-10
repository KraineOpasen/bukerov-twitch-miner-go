package web

// S5-6 Settings category tests: the ten new direct-render category routes
// (13-22, handlers_settings_categories.go) replacing the former /settings/*
// compatibility redirects — GET/HEAD 200 + non-GET method contract, exactly
// one aria-current nav destination per route, the shared C6/C7/C8
// dirty-boundary engine's source contract (dirty tracking, sticky save bar,
// discard dialog focus-trap/least-destructive-default/nav-interception),
// payload isolation (no category outside route 13 ever posts "streamers"),
// all 13 backend prediction strategies, Discord GuildID read/write vs
// BotToken write-only, point_rules id/triggered never editable, S-NOBACK
// field absences, legacy /settings untouched, and the new locale keys.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
)

// s56StreamersPayloadKeyRe matches the JSON object key "streamers" (a
// colon-anchored, lowercase, word-bounded match) — deliberately NOT a bare
// substring check, since several routes legitimately use unrelated
// identifiers/string-literals that happen to CONTAIN "streamers" as a
// substring without ever sending it as a top-level payload key: route 20's
// own mentionsStreamers/onlineStreamers/offlineStreamers fields (real
// NotificationConfig fields, camelCase so case-sensitively distinct from
// "streamers"), and DOM element id string literals like
// selectedFrom('mentions-streamers').
var s56StreamersPayloadKeyRe = regexp.MustCompile(`\bstreamers\s*:`)

// s56CategoryRoutes is the full set of ten S5-6 direct-render routes.
var s56CategoryRoutes = []string{
	"/settings/streamers", "/settings/rotation", "/settings/drops", "/settings/predictions",
	"/settings/chat-raids", "/settings/transport", "/settings/analytics-logging",
	"/settings/events-notifications", "/settings/discord", "/settings/system",
}

// s56CategoryTemplates maps each route to its embedded template file, for
// tests that need to inspect page-specific source rather than rendered
// output (httptest never executes JS, so client-side logic — e.g. which
// strategy values populate a <select> — can only be pinned at the source
// level, the same approach TestS5_2DrawerSourceContract and its siblings
// already use throughout this package).
var s56CategoryTemplates = map[string]string{
	"/settings/streamers":            "templates/settings_streamers.html",
	"/settings/rotation":             "templates/settings_rotation.html",
	"/settings/drops":                "templates/settings_drops.html",
	"/settings/predictions":          "templates/settings_predictions.html",
	"/settings/chat-raids":           "templates/settings_chat_raids.html",
	"/settings/transport":            "templates/settings_transport.html",
	"/settings/analytics-logging":    "templates/settings_analytics_logging.html",
	"/settings/events-notifications": "templates/settings_events_notifications.html",
	"/settings/discord":              "templates/settings_discord.html",
	"/settings/system":               "templates/settings_system.html",
}

// ---------------------------------------------------------------------
// 1. Ten direct GET/HEAD 200 + non-GET method contract.
// ---------------------------------------------------------------------

// TestS5_6DirectRoutesGetHeadOKMethodContract proves every category route
// answers GET and HEAD with 200 and rejects every other method with 405
// (Allow: GET, HEAD) — the explicit method-gating contract task S5-6
// requires (unlike the S5-4/S5-5 page precedent, which deliberately skips
// it — see requireGetOrHead's doc comment).
func TestS5_6DirectRoutesGetHeadOKMethodContract(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	for _, route := range s56CategoryRoutes {
		t.Run(route, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(method, route, nil))
				if rec.Code != http.StatusOK {
					t.Errorf("%s %s = %d, want 200", method, route, rec.Code)
				}
			}
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(method, route, nil))
				if rec.Code != http.StatusMethodNotAllowed {
					t.Errorf("%s %s = %d, want 405", method, route, rec.Code)
				}
				if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
					t.Errorf("%s %s Allow header = %q, want %q", method, route, allow, "GET, HEAD")
				}
			}
		})
	}
}

// ---------------------------------------------------------------------
// 2. Exactly one aria-current nav destination per category route.
// ---------------------------------------------------------------------

// TestS5_6EachCategoryExactlyOneAriaCurrent mirrors
// TestS5_3OverviewQueueExactlyOneAriaCurrentDestination /
// TestS5_5SystemActiveChildPerRoute's approach for the ten new Settings
// children: re-implements base.html's client-side updateActiveNav rule in Go
// against the rendered C2 nav, proving each route marks exactly one
// destination current, and it is the right one.
func TestS5_6EachCategoryExactlyOneAriaCurrent(t *testing.T) {
	srv := buildF3PageServer(t)

	for _, path := range s56CategoryRoutes {
		t.Run(path, func(t *testing.T) {
			body := f3GetPage(t, srv, path, "en")
			const active = "settings"

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
		})
	}
}

// ---------------------------------------------------------------------
// 3. C6/C7/C8 shared engine source contract.
// ---------------------------------------------------------------------

// TestS5_6C6EngineSourceContract pins the shared dirty-boundary engine's
// required behaviors at the source level: dirty detection via gather()
// comparison, the Tab/Shift+Tab focus trap inside the discard dialog,
// Escape/"keep editing" as the least-destructive default (the dialog's
// native 'cancel' event is handled as a no-op, never as discard),
// nav-interception on internal link clicks, and the beforeunload guard for
// real navigation/tab-close.
func TestS5_6C6EngineSourceContract(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/components/c6_form.html")
	for _, want := range []string{
		"JSON.stringify(opts.gather())",
		// The Tab/Shift+Tab focus-trap block, specifically — not just the
		// bare "e.key !== 'Tab'"/"shiftKey" tokens, which also appear
		// (unrelatedly, guarding modified clicks) in the nav-interception
		// block below, so a mutant that deletes ONLY the focus trap while
		// leaving nav-interception intact must still fail this check.
		"dialog.addEventListener('keydown', function (e) {\n                if (e.key !== 'Tab') return;",
		"dialog.querySelectorAll('button, [href], input, select, textarea",
		"if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }",
		"dialog.addEventListener('cancel', function () {})",
		"document.addEventListener('click', function (e) {",
		"}, true);",
		"window.addEventListener('beforeunload', function (e) {",
		"e.preventDefault();\n            e.returnValue = '';",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("c6_form.html missing engine contract literal %q", want)
		}
	}
}

// TestS5_6C8LeastDestructiveDefaultSourceContract proves the nav-discard
// dialog's "keep editing" button carries autofocus and is declared BEFORE
// "discard changes" in source order, so a dialog opened by any means always
// lands focus on the non-destructive choice by default.
func TestS5_6C8LeastDestructiveDefaultSourceContract(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/components/c8_dialog.html")
	keepIdx := strings.Index(src, "data-c8-keep autofocus")
	discardIdx := strings.Index(src, "data-c8-discard")
	if keepIdx < 0 {
		t.Fatal("c8_dialog.html: \"keep editing\" button missing the autofocus attribute")
	}
	if discardIdx < 0 {
		t.Fatal("c8_dialog.html: \"discard\" button not found")
	}
	if keepIdx > discardIdx {
		t.Error("c8_dialog.html: the autofocused \"keep editing\" button must come before \"discard\" in source order")
	}
}

// TestS5_6C7SaveBarHiddenWhileClean proves the C7 save bar renders hidden
// (data-state="clean", the c7-savebar--hidden class) by default in the
// static server-render, before any client-side dirty state exists. Route 22
// (System) also carries a C7 bar (B2 corrective pass): it drives the
// existing Health partial through a custom C6 adapter instead of a JSON
// payload, but participates in the same single dirty-boundary UI.
func TestS5_6C7SaveBarHiddenWhileClean(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, path := range s56CategoryRoutes {
		body := f3GetPage(t, srv, path, "en")
		if !strings.Contains(body, `class="c7-savebar c7-savebar--hidden" data-state="clean"`) {
			t.Errorf("%s: C7 save bar must render hidden/clean by default", path)
		}
		if !strings.Contains(body, "data-c7-save") || !strings.Contains(body, "data-c7-cancel") {
			t.Errorf("%s: C7 save bar missing Save/Cancel controls", path)
		}
	}
}

// s56ExtractJSFunction returns the balanced-brace body of a JS function
// declared as `functionName() {` inside src, using the same brace-balancing
// approach as extractMediaBlock (s5_2_chrome_test.go) so the excerpt cannot
// accidentally swallow unrelated later code.
func s56ExtractJSFunction(t *testing.T, src, signature string) string {
	t.Helper()
	idx := strings.Index(src, signature)
	if idx < 0 {
		t.Fatalf("could not locate %q", signature)
	}
	rel := strings.IndexByte(src[idx:], '{')
	if rel < 0 {
		t.Fatalf("no opening brace found after %q", signature)
	}
	openIdx := idx + rel
	depth := 0
	for i := openIdx; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[openIdx : i+1]
			}
		}
	}
	t.Fatalf("unbalanced braces locating the end of %q", signature)
	return ""
}

// ---------------------------------------------------------------------
// 4. Payload isolation: outside route 13, gather() never sends "streamers".
// ---------------------------------------------------------------------

// TestS5_6PayloadIsolationNoStreamersOutsideRoute13 proves every category
// page's own gather() function — the object actually POSTed to /api/settings
// — never includes a "streamers" key, for every route except /settings/streamers
// itself. Scoped to the gather() function body specifically (not the whole
// page source), since routes 16/predictions and 17/chat-raids legitimately
// READ data.streamers in populate() to render a read-only override summary —
// that is not a payload isolation violation, only WRITING it would be.
func TestS5_6PayloadIsolationNoStreamersOutsideRoute13(t *testing.T) {
	for path, tmpl := range s56CategoryTemplates {
		if path == "/settings/streamers" || path == "/settings/system" {
			continue // route 13 legitimately owns "streamers"; route 22 has no gather().
		}
		t.Run(path, func(t *testing.T) {
			src := readEmbeddedTemplate(t, tmpl)
			fn := s56ExtractJSFunction(t, src, "function gather()")
			if s56StreamersPayloadKeyRe.MatchString(fn) {
				t.Errorf("%s: gather() must never send a \"streamers\" payload key (outside route 13), got:\n%s", path, fn)
			}
		})
	}
}

// TestS5_6StreamersPageOwnsStreamersPayload is the positive control for the
// isolation test above: /settings/streamers' OWN gather() legitimately
// includes "streamers" — proving the isolation test isn't vacuously true
// because no page ever references the token at all.
func TestS5_6StreamersPageOwnsStreamersPayload(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/settings_streamers.html")
	fn := s56ExtractJSFunction(t, src, "function gather()")
	if !strings.Contains(fn, "streamers: gatherStreamers()") {
		t.Error("/settings/streamers gather() must own the streamers payload key")
	}
}

// ---------------------------------------------------------------------
// 5. All 13 backend prediction strategies rendered.
// ---------------------------------------------------------------------

// TestS5_6AllThirteenPredictionStrategies proves settings_predictions.html's
// strategyOptions literal carries all 13 internal/models/bet.go Strategy
// values (SMART, MOST_VOTED, HIGH_ODDS, PERCENTAGE, SMART_MONEY, NUMBER_1..8)
// — pinned at the source level since the <select>'s <option> elements are
// built client-side and httptest never executes JS.
func TestS5_6AllThirteenPredictionStrategies(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/settings_predictions.html")
	want := []string{
		"SMART", "MOST_VOTED", "HIGH_ODDS", "PERCENTAGE", "SMART_MONEY",
		"NUMBER_1", "NUMBER_2", "NUMBER_3", "NUMBER_4", "NUMBER_5", "NUMBER_6", "NUMBER_7", "NUMBER_8",
	}
	for _, strategy := range want {
		if !strings.Contains(src, "value: '"+strategy+"'") {
			t.Errorf("settings_predictions.html missing strategy option %q", strategy)
		}
	}
	// bet.filterCondition is a real backend field (internal/models/bet.go)
	// deliberately never exposed here (S-NOBACK).
	if strings.Contains(src, "filterCondition") {
		t.Error("settings_predictions.html must not expose bet.filterCondition (S-NOBACK)")
	}
}

// ---------------------------------------------------------------------
// 6. Discord: GuildID read/write, BotToken write-only.
// ---------------------------------------------------------------------

// TestS5_6DiscordGuildIdReadWriteBotTokenWriteOnly proves settings_discord.html
// populates guildId from the loaded settings but ALWAYS clears the bot-token
// field on load — the same write-only-secret contract dto.go's
// DiscordUIConfig.BotToken documents (P2), never a readback of the stored
// value.
func TestS5_6DiscordGuildIdReadWriteBotTokenWriteOnly(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/settings_discord.html")
	fn := s56ExtractJSFunction(t, src, "function populate(data)")
	if !strings.Contains(fn, "document.getElementById('discordGuildId').value = d.guildId || '';") {
		t.Error("populate() must read guildId back from the loaded settings")
	}
	if !strings.Contains(fn, "document.getElementById('discordBotToken').value = '';") {
		t.Error("populate() must always clear the bot-token field, never populate it from the response")
	}
	if strings.Contains(fn, "d.botToken") {
		t.Error("populate() must never reference d.botToken — the field is write-only and never returned by the API")
	}
}

// ---------------------------------------------------------------------
// 7. point_rules: id/triggered are never editable inputs.
// ---------------------------------------------------------------------

// TestS5_6PointRuleIdTriggeredNeverEditable proves settings_events_notifications.html
// only ever uses a point rule's id (as the DELETE URL segment) and triggered
// (as read-only display text) — never binds either to an editable form
// control.
func TestS5_6PointRuleIdTriggeredNeverEditable(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/settings_events_notifications.html")
	for _, banned := range []string{
		`data-field="id"`, `data-field="triggered"`,
		`id="point-rule-id"`, `id="point-rule-triggered"`,
	} {
		if strings.Contains(src, banned) {
			t.Errorf("settings_events_notifications.html must never expose an editable %q control", banned)
		}
	}
	if !strings.Contains(src, "'/api/notifications/points/' + rule.id") {
		t.Error("expected rule.id used only as the DELETE URL segment")
	}
	if !strings.Contains(src, "rule.triggered ? t('js.notif.triggered') : t('js.notif.waiting')") {
		t.Error("expected rule.triggered rendered only as read-only status text")
	}
}

// ---------------------------------------------------------------------
// 8. S-NOBACK: fields with no backend counterpart stay absent.
// ---------------------------------------------------------------------

// s5_6MainContent slices a rendered page body down to its <main> content
// region, excluding the shared chrome (sidebar/nav/topbar). Introduced by
// task S5-7 and used ONLY where the chrome forced it: the C2 nav's Events
// group legitimately carries a "Sound" child label on EVERY page, so route
// 20's sound/quiet-hours/upload bans must be scoped to the category page's
// own content/form — which is exactly what B4/B5/B9 always meant (route 20
// exposes no sound/quiet-hours/upload CONTROLS). The transport and system
// bans below need no such carve-out and stay full-body.
func s5_6MainContent(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<main class="app-main">`)
	end := strings.Index(body, "</main>")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("could not locate the <main> content region in the rendered page")
	}
	return body[start:end]
}

// TestS5_6SNoBackFieldsAbsent proves every category page omits UI for a
// field the task brief explicitly scoped out (either genuinely absent from
// the backend, or deliberately carved into a different route): route 18
// (Transport) never exposes requestDelay/proxy/client-id/user-agent; route
// 20 (Events & Notifications) never exposes sound/quiet-hours/upload
// controls (NotificationConfig has no such fields); route 22 (System) never
// exposes an updater editor or a LAN-CIDR control. The transport and system
// bans hold over the ENTIRE rendered body — nothing in the shared chrome
// legitimately carries those terms. Only route 20's bans are scoped to its
// own <main> content region (see s5_6MainContent): the Events nav group's
// "Sound" child label is legitimate chrome on every page.
func TestS5_6SNoBackFieldsAbsent(t *testing.T) {
	srv := buildF3PageServer(t)

	transport := f3GetPage(t, srv, "/settings/transport", "en")
	for _, banned := range []string{"requestDelay", "proxy", "client-id", "clientId", "user-agent", "userAgent"} {
		if strings.Contains(transport, banned) {
			t.Errorf("/settings/transport must not expose %q", banned)
		}
	}

	events := s5_6MainContent(t, f3GetPage(t, srv, "/settings/events-notifications", "en"))
	for _, banned := range []string{"sound", "Sound", "quiet hour", "quietHour", "upload", "Upload"} {
		if strings.Contains(events, banned) {
			t.Errorf("/settings/events-notifications must not expose %q (B4/B5/B9, S-NOBACK)", banned)
		}
	}

	system := f3GetPage(t, srv, "/settings/system", "en")
	for _, banned := range []string{"updater", "Updater", "CIDR", "cidr"} {
		if strings.Contains(system, banned) {
			t.Errorf("/settings/system must not expose %q", banned)
		}
	}
}

// ---------------------------------------------------------------------
// 9. Legacy /settings, /api/settings, /api/settings/reset untouched.
// ---------------------------------------------------------------------

// TestS5_6LegacySettingsPageAndAPIsStillDirect200 proves the legacy
// /settings mega-form still renders directly (never redirected), and its
// GET /api/settings + POST /api/settings/reset endpoints are unchanged —
// task S5-6 explicitly keeps legacy /api/settings/reset "/settings-only
// until S5-10".
func TestS5_6LegacySettingsPageAndAPIsStillDirect200(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /settings = %d, want 200 (direct, not redirected)", rec.Code)
	}

	recAPI := httptest.NewRecorder()
	h.ServeHTTP(recAPI, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if recAPI.Code != http.StatusOK {
		t.Errorf("GET /api/settings = %d, want 200", recAPI.Code)
	}

	recReset := httptest.NewRecorder()
	h.ServeHTTP(recReset, httptest.NewRequest(http.MethodPost, "/api/settings/reset", nil))
	if recReset.Code != http.StatusOK {
		t.Errorf("POST /api/settings/reset = %d, want 200", recReset.Code)
	}
}

// ---------------------------------------------------------------------
// 10. New locale keys: present, non-empty, translated in both languages.
// ---------------------------------------------------------------------

// s56DeliberatelyIdenticalKeys lists S5-6 keys whose EN/RU values are
// deliberately identical (the brand proper noun "Discord"), mirroring the
// s53/s55DeliberatelyIdenticalKeys precedent for this task's own key set.
var s56DeliberatelyIdenticalKeys = map[string]bool{
	"nav.set.discord":      true,
	"setcat.discord.title": true,
}

func TestS5_6LocaleKeysPresentAndTranslated(t *testing.T) {
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	keys := []string{
		"nav.set.streamers", "nav.set.rotation", "nav.set.drops", "nav.set.predictions",
		"nav.set.chatraids", "nav.set.transport", "nav.set.analyticslogging",
		"nav.set.eventsnotifications", "nav.set.discord", "nav.set.system",
		"setcat.streamers.title", "setcat.streamers.subtitle",
		"setcat.rotation.title", "setcat.rotation.subtitle",
		"setcat.drops.title", "setcat.drops.subtitle",
		"setcat.predictions.title", "setcat.predictions.subtitle",
		"setcat.chatraids.title", "setcat.chatraids.subtitle",
		"setcat.transport.title", "setcat.transport.subtitle",
		"setcat.analyticslogging.title", "setcat.analyticslogging.subtitle",
		"setcat.eventsnotifications.title", "setcat.eventsnotifications.subtitle",
		"setcat.discord.title", "setcat.discord.subtitle",
		"setcat.system.title", "setcat.system.subtitle",
		"js.setcat.dirty", "js.setcat.saving", "js.setcat.saved", "js.setcat.error",
		"js.setcat.load_error", "js.setcat.blocked",
		"setcat.save", "setcat.cancel",
		"setcat.discard_title", "setcat.discard_body", "setcat.discard_confirm", "setcat.discard_keep",
		"setcat.streamers.delete_title", "js.setcat.streamers.delete_body",
		"setcat.streamers.delete_confirm", "setcat.streamers.delete_keep",
		"setcat.predictions.overrides_heading", "js.setcat.predictions.overrides_empty", "js.setcat.predictions.overrides_link",
		"setcat.chatraids.overrides_heading", "js.setcat.chatraids.overrides_empty", "js.setcat.chatraids.overrides_link",
		"js.set.opt.strategy.number_3", "js.set.opt.strategy.number_4", "js.set.opt.strategy.number_5",
		"js.set.opt.strategy.number_6", "js.set.opt.strategy.number_7", "js.set.opt.strategy.number_8",
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
		if en == ru && !s56DeliberatelyIdenticalKeys[k] {
			t.Errorf("%q has identical EN/RU text %q — looks untranslated", k, en)
		}
	}
}

// ---------------------------------------------------------------------
// 11. Redirect map: exactly 3 remain, none of the ten settings routes.
// ---------------------------------------------------------------------

// TestS5_6GeneratedCSSContainsC6C7C8Classes proves the Tailwind build
// actually picked up the new C6/C7/C8 selectors from input.css into the
// generated (minified) app.css — the same input.css/app.css sync check
// TestS5_2DrawerCloseVisibilitySourceContract's siblings already run for
// other slices.
func TestS5_6GeneratedCSSContainsC6C7C8Classes(t *testing.T) {
	css := readEmbeddedStatic(t, "static/css/input.css")
	appCSS := readEmbeddedStatic(t, "static/css/app.css")
	for _, want := range []string{".c7-savebar", ".c7-savebar--hidden", ".c8-dialog", ".c8-dialog-actions"} {
		if !strings.Contains(css, want) {
			t.Errorf("input.css missing %q", want)
		}
		if !strings.Contains(appCSS, want+"{") {
			t.Errorf("generated app.css missing selector %q — run `make tailwind` to regenerate it from input.css", want)
		}
	}
}

// ---------------------------------------------------------------------
// S5-6 browser-evidence harness: mirrors the TestF3EvidenceHarness/
// TestS5_5EvidenceHarness precedent (f3_harness_test.go — READ-ONLY
// reference, never edited here) but reuses the already-wired
// buildF3PageServer fixture directly, since none of the ten category pages
// need a bespoke provider contrast — they all read/write through the SAME
// GET/POST /api/settings and /api/notifications/* endpoints every other
// F3/S5-x page already exercises. Env-gated: skipped unless
// MINER_S5_6_HARNESS=1. Never talks to Twitch, Discord, or any real network.
//
// Usage:
//
//	MINER_S5_6_HARNESS=1 MINER_S5_6_HARNESS_ADDR=127.0.0.1:8975 \
//	  go test -run TestS5_6EvidenceHarness -timeout 1800s ./internal/web/
//
// The server stops when the harness receives SIGINT/SIGTERM or 30 minutes
// elapse.
func TestS5_6EvidenceHarness(t *testing.T) {
	if os.Getenv("MINER_S5_6_HARNESS") != "1" {
		t.Skip("harness disabled (set MINER_S5_6_HARNESS=1)")
	}
	addr := os.Getenv("MINER_S5_6_HARNESS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8975"
	}

	srv := buildF3PageServer(t)
	// Report "running" so the status overlay never covers the pages during
	// browser evidence runs — buildF3PageServer wires every provider these
	// pages read from with deterministic fakes, so there is no real startup
	// sequence for it to reflect.
	srv.GetStatusBroadcaster().SetStatus(StatusRunning, "")

	handle, err := startS5_6EvidenceHarness(srv.handler(), addr)
	if err != nil {
		t.Fatalf("evidence harness failed to start: %v", err)
	}
	// Checked cleanup, and it still runs if the select below Fatalf's.
	defer func() {
		if err := handle.stop(); err != nil {
			t.Errorf("evidence harness shutdown: %v", err)
		}
	}()
	t.Logf("S5-6 evidence harness serving on http://%s — try /settings/streamers, /settings/rotation, /settings/drops, /settings/predictions, /settings/chat-raids, /settings/transport, /settings/analytics-logging, /settings/events-notifications, /settings/discord, /settings/system", handle.Addr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)
	select {
	case <-sig:
	case serveErr := <-handle.errCh:
		// The server stopped on its own — never a successful evidence run,
		// however long it had been up.
		t.Fatalf("evidence harness stopped serving before it was asked to: %v", serveErr)
	case <-time.After(30 * time.Minute):
	}
}

// s56HarnessHandle is a started evidence-harness server: the address it is
// actually listening on, the channel carrying the serve goroutine's terminal
// error, and a checked stop function.
type s56HarnessHandle struct {
	Addr net.Addr
	// errCh receives exactly one value — the serve goroutine's terminal
	// error, nil for a clean http.ErrServerClosed shutdown — and is then
	// closed, so both the harness's select and stop() can receive from it
	// without either one starving the other.
	errCh <-chan error
	stop  func() error
}

// startS5_6EvidenceHarness binds addr and serves handler on it.
//
// The bind happens SYNCHRONOUSLY, so a port collision (or any other listen
// failure) comes back to the caller as an error instead of being discarded
// inside a goroutine. That is the whole point: a harness that swallows its
// startup error goes on to log "serving on ..." and let a browser-evidence
// run claim readiness — and then success — against a server that never came
// up. Readiness needs no sleep either, since the listener is already bound
// when this returns. After startup, a non-http.ErrServerClosed serve error is
// surfaced on the handle's channel rather than dropped, and stop() reports
// both the Shutdown error and the serve goroutine's own verdict.
func startS5_6EvidenceHarness(handler http.Handler, addr string) (*s56HarnessHandle, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("evidence harness could not bind %s: %w", addr, err)
	}
	httpSrv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		serveErr := httpSrv.Serve(ln)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		errCh <- serveErr
		close(errCh)
	}()
	stop := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr := httpSrv.Shutdown(ctx)
		serveErr := <-errCh
		if shutdownErr != nil {
			return shutdownErr
		}
		return serveErr
	}
	return &s56HarnessHandle{Addr: ln.Addr(), errCh: errCh, stop: stop}, nil
}

// TestS5_6EvidenceHarnessSurfacesStartupFailure proves the harness reports a
// failed start instead of discarding it. Deterministic by construction: the
// address handed to the harness is one this test is already listening on, so
// the bind cannot succeed, and no timing window is involved.
func TestS5_6EvidenceHarnessSurfacesStartupFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not occupy a port for the collision: %v", err)
	}
	defer func() { _ = occupied.Close() }()
	addr := occupied.Addr().String()

	srv := buildF3PageServer(t)
	handle, err := startS5_6EvidenceHarness(srv.handler(), addr)
	if err == nil {
		if handle != nil {
			_ = handle.stop()
		}
		t.Fatalf("startS5_6EvidenceHarness(%s) reported success, but that address is already in use — a discarded startup error lets an evidence run claim readiness against a server that never came up", addr)
	}
	if handle != nil {
		t.Errorf("startS5_6EvidenceHarness must return a nil handle when it could not start, got %+v", handle)
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("the startup error must name the address it failed to bind, got %v", err)
	}
}

// TestS5_6EvidenceHarnessServesAndShutsDownCleanly is the other half of the
// contract: a harness that started really is serving (no sleep-based
// readiness guess — the listener is bound before startS5_6EvidenceHarness
// returns), reports nothing on errCh while healthy, and its checked shutdown
// reports no error.
func TestS5_6EvidenceHarnessServesAndShutsDownCleanly(t *testing.T) {
	srv := buildF3PageServer(t)
	handle, err := startS5_6EvidenceHarness(srv.handler(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startS5_6EvidenceHarness: %v", err)
	}
	// Registered immediately, BEFORE any assertion that can Fatal: every
	// t.Fatalf below returns from the test through runtime.Goexit, and the
	// explicit checked shutdown at the end would simply never run — leaving the
	// listener bound and the serve goroutine alive for the rest of the
	// package's tests. The explicit shutdown stays the assertion (it is what
	// this test is about), so this cleanup only covers the paths that never
	// reach it; `stopped` keeps the healthy path reporting exactly once, and
	// stop() is idempotent regardless (see
	// TestS5_6EvidenceHarnessStopIsIdempotent).
	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		if err := handle.stop(); err != nil {
			t.Errorf("checked shutdown of the harness during cleanup must report no error, got %v", err)
		}
	})

	// Proxy explicitly disabled: the harness is loopback-only and must never
	// depend on the ambient environment's proxy settings.
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + handle.Addr.String() + "/settings/events-notifications")
	if err != nil {
		t.Fatalf("GET against the started harness failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("harness GET /settings/events-notifications = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	select {
	case serveErr := <-handle.errCh:
		t.Fatalf("harness reported a serve error while it was healthy: %v", serveErr)
	default:
	}

	if err := handle.stop(); err != nil {
		t.Errorf("checked shutdown of a healthy harness must report no error, got %v", err)
	}
	stopped = true
}

// TestS5_6EvidenceHarnessStopIsIdempotent pins what makes the registered
// cleanup above safe to pair with an explicit checked shutdown: stop() may run
// twice and the second run must be a silent no-op, not a hang and not a
// spurious failure. Shutdown on an already-shut-down server returns nil, and
// errCh is closed after its single value so a second receive yields nil
// immediately — no timing window, no sleep, and no ordering assumption between
// the explicit call and the cleanup.
func TestS5_6EvidenceHarnessStopIsIdempotent(t *testing.T) {
	srv := buildF3PageServer(t)
	handle, err := startS5_6EvidenceHarness(srv.handler(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startS5_6EvidenceHarness: %v", err)
	}
	// Registered immediately, BEFORE the first explicit stop assertion below:
	// that assertion is a t.Fatalf, and a Fatal there returns from the test
	// through runtime.Goexit, so the second stop() — the only other call that
	// would shut this server down — would never run, stranding the listener
	// and the serve goroutine for the rest of the package's tests. Safe
	// precisely because of what this test proves: stop() is idempotent, so on
	// the healthy path this is a silent third no-op, and both explicit
	// assertions below remain exactly as strong as they were.
	t.Cleanup(func() { _ = handle.stop() })
	if err := handle.stop(); err != nil {
		t.Fatalf("first checked shutdown of a healthy harness must report no error, got %v", err)
	}
	if err := handle.stop(); err != nil {
		t.Errorf("a second checked shutdown must be a no-op reporting no error, got %v — a cleanup that runs after the explicit shutdown would otherwise fail an otherwise-passing test", err)
	}
}

// TestS5_6EvidenceHarnessIdempotencyCleanupRegisteredBeforeFirstStop pins the
// safety net inside the test above, which is the one harness test that has no
// other way to release its listener.
//
// The first explicit shutdown there is a t.Fatalf assertion: if it ever fires,
// the test returns through runtime.Goexit and the second stop() — the only
// other call that would shut the server down — never runs, leaving the
// listener bound and the serve goroutine alive for the remainder of the
// package's tests. Its sibling, TestS5_6EvidenceHarnessServesAndShutsDownCleanly,
// already guards exactly this with an immediately-registered t.Cleanup.
//
// So: the cleanup must be registered as soon as the harness has started, ahead
// of the first assertion that can Fatal. That is safe precisely because of
// what the test proves — stop() is idempotent — so on the healthy path the
// cleanup is a silent third no-op, and both explicit assertions stay exactly
// as strong as they were. Checked at the source level because a stranded
// listener has no in-process signal to assert on from the test that stranded
// it, the same approach TestF3HarnessReportsRunningStatus already uses.
func TestS5_6EvidenceHarnessIdempotencyCleanupRegisteredBeforeFirstStop(t *testing.T) {
	raw, err := os.ReadFile("s5_6_settings_test.go")
	if err != nil {
		t.Fatalf("read s5_6_settings_test.go: %v", err)
	}
	src := string(raw)

	// Scope every literal below to that one test's body — the next top-level
	// declaration ends it — so nothing here can be satisfied by a match
	// somewhere else in the file (including inside this test).
	const sig = "func TestS5_6EvidenceHarnessStopIsIdempotent(t *testing.T) {"
	start := strings.Index(src, sig)
	if start < 0 {
		t.Fatalf("could not locate %q", sig)
	}
	body := src[start:]
	if end := strings.Index(body[1:], "\nfunc "); end >= 0 {
		body = body[:end+1]
	}

	const cleanup = "t.Cleanup(func() { _ = handle.stop() })"
	cleanupIdx := strings.Index(body, cleanup)
	if cleanupIdx < 0 {
		t.Fatalf("TestS5_6EvidenceHarnessStopIsIdempotent must register %q right after a successful start, or a Fatal from its first explicit stop strands the listener and the serve goroutine; got:\n%s", cleanup, body)
	}

	const firstStop = "if err := handle.stop(); err != nil {"
	firstStopIdx := strings.Index(body, firstStop)
	if firstStopIdx < 0 {
		t.Fatalf("could not locate the explicit stop assertions in:\n%s", body)
	}
	if cleanupIdx > firstStopIdx {
		t.Errorf("the cleanup must be registered BEFORE the first explicit stop assertion — registered after it, it cannot run when that assertion Fatals, which is the only case it exists for; got:\n%s", body)
	}

	// The cleanup is an addition, never a replacement: both explicit
	// assertions — and the start check that makes handle safe to close over
	// — must survive verbatim, and no fourth stop() call may creep in.
	for _, kept := range []string{
		`t.Fatalf("startS5_6EvidenceHarness: %v", err)`,
		`t.Fatalf("first checked shutdown of a healthy harness must report no error, got %v", err)`,
		`t.Errorf("a second checked shutdown must be a no-op reporting no error, got %v`,
	} {
		if !strings.Contains(body, kept) {
			t.Errorf("the cleanup must not weaken the test's own assertions: missing %q; got:\n%s", kept, body)
		}
	}
	if n := strings.Count(body, "handle.stop()"); n != 3 {
		t.Errorf("TestS5_6EvidenceHarnessStopIsIdempotent must call handle.stop() exactly three times (cleanup + the two explicit assertions), got %d; body:\n%s", n, body)
	}
}

// TestS5_6RedirectMapNoLongerContainsSettingsRoutes proves none of the ten
// category routes remain in compatibilityRedirects (they would otherwise
// collide with their own direct-route registration — see server.go's
// registration-before-redirect-loop ordering — and panic srv.handler()).
func TestS5_6RedirectMapNoLongerContainsSettingsRoutes(t *testing.T) {
	if len(compatibilityRedirects) != 3 {
		t.Fatalf("len(compatibilityRedirects) = %d, want 3", len(compatibilityRedirects))
	}
	for _, route := range s56CategoryRoutes {
		if _, ok := compatibilityRedirects[route]; ok {
			t.Errorf("compatibilityRedirects must no longer contain %q", route)
		}
	}
	// Building the full mux is itself part of the proof: a route left in
	// both this map and its own direct registration panics srv.handler()
	// with "pattern already registered" before any assertion below runs.
	srv := buildF3PageServer(t)
	_ = srv.handler()
}

// ---------------------------------------------------------------------
// S5-6 Q3 corrective pass: B1-B3, M1-M2 source-level regression pins.
// (M3 has its own dedicated section further below; browser evidence for
// M3 was gathered separately — see the corrective pass's evidence report —
// since a rendered-layout overflow cannot be proven from HTML source alone.)
// ---------------------------------------------------------------------

// s56Q3ClientJSOverrideKeySites pairs each page whose client-side JS reads a
// setcat.* override/destructive-confirm string with the js.setcat.* key it
// must use instead of the bare setcat.* one server-side {{t "setcat.*"}}
// calls legitimately keep (B1: window.I18N only ever exports js.* keys, so
// any other key silently falls back to its own raw name in the browser).
var s56Q3ClientJSOverrideKeySites = map[string][]string{
	"templates/settings_predictions.html": {"js.setcat.predictions.overrides_empty", "js.setcat.predictions.overrides_link"},
	"templates/settings_chat_raids.html":  {"js.setcat.chatraids.overrides_empty", "js.setcat.chatraids.overrides_link"},
	"templates/settings_streamers.html":   {"js.setcat.streamers.delete_body"},
}

// TestS5_6Q3B1ClientJSNeverCallsNonJSSetcatKey proves every client-side t()
// call in the three pages that read override/destructive-confirm copy uses
// the js.setcat.* form, and that the bare (non-js) setcat.* form never
// appears inside a t('...') call anywhere in the ten category templates —
// the exact bug: a key absent from window.I18N (js.* only, see base.html)
// falls back to echoing its own raw name to the user.
func TestS5_6Q3B1ClientJSNeverCallsNonJSSetcatKey(t *testing.T) {
	rawSetcatInTCall := regexp.MustCompile(`t\('setcat\.[a-zA-Z0-9_.]+'`)
	for path, tmpl := range s56CategoryTemplates {
		t.Run(path, func(t *testing.T) {
			src := readEmbeddedTemplate(t, tmpl)
			if m := rawSetcatInTCall.FindString(src); m != "" {
				t.Errorf("%s: client-side t() call uses a non-js setcat.* key (%s) — window.I18N only exports js.* keys, so this falls back to the literal key name", path, m)
			}
		})
	}
	for tmpl, keys := range s56Q3ClientJSOverrideKeySites {
		src := readEmbeddedTemplate(t, tmpl)
		for _, key := range keys {
			if !strings.Contains(src, "t('"+key+"'") {
				t.Errorf("%s: expected client-side call t('%s', ...)", tmpl, key)
			}
		}
	}
}

// TestS5_6Q3B2Route22SubmitInterceptedAndSavesSequentially pins route 22's
// corrective-pass contract at the source level: the two canary/watchdog
// forms' native hx-post submit is intercepted (capture phase, before htmx's
// own bubble-phase listener can act) so neither section's own submit can
// ever whole-partial-swap #health-center and erase the sibling section's
// unsaved DOM, canary is POSTed strictly before watchdog inside save(), and
// route 22 now renders the same C7/C8 shell every other category page does.
func TestS5_6Q3B2Route22SubmitInterceptedAndSavesSequentially(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/settings_system.html")

	submitGuard := `document.addEventListener('submit', function (e) {
            if (e.target.closest && e.target.closest('#health-center form[hx-post="/api/health/settings"]')) {
                e.preventDefault();
                e.stopPropagation();
            }
        }, true);`
	if !strings.Contains(src, submitGuard) {
		t.Error("settings_system.html missing the capture-phase submit interceptor for the canary/watchdog forms")
	}

	saveFn := s56ExtractJSFunction(t, src, "save: async function (payload) {")
	canaryIdx := strings.Index(saveFn, "commitSection('canary', payload.canary)")
	watchdogIdx := strings.Index(saveFn, "commitSection('watchdog', payload.watchdog)")
	if canaryIdx < 0 || watchdogIdx < 0 {
		t.Fatalf("save() must commit both sections via commitSection(), from C6's own captured payload (R3); got:\n%s", saveFn)
	}
	if canaryIdx > watchdogIdx {
		t.Error("save() must POST canary strictly before watchdog (sequential, matching healthFormMu's own serialization)")
	}

	// commitSection itself must post from the exact snapshot, never a live
	// DOM re-read (R3's invariant, now centralized in one helper both
	// sections funnel through).
	commitFn := s56ExtractJSFunction(t, src, "async function commitSection(section, snapshot) {")
	if !strings.Contains(commitFn, "postSection(section, snapshot)") {
		t.Errorf("commitSection must POST the exact snapshot passed to it; got:\n%s", commitFn)
	}

	if !strings.Contains(src, `{{template "c7.savebar" .}}`) || !strings.Contains(src, `{{template "c8.dialog" .}}`) {
		t.Error("settings_system.html must render the C7 save bar and C8 discard dialog like every other category page")
	}

	// applyFields() only ever writes fields found on the section's OWN form
	// (via form.querySelector inside the section's own live form element),
	// never a whole-container innerHTML/replaceChildren — the structural
	// half of "saving one section must never erase sibling unsaved DOM".
	if strings.Contains(src, "health-center').innerHTML =") || strings.Contains(src, "health-center\").innerHTML =") {
		t.Error("settings_system.html's own script must never directly replace #health-center's innerHTML (that erases the sibling section's unsaved DOM)")
	}
}

// TestS5_6Q3B3CancelRestoresCanonicalNotScopedBaseline pins the c6 engine's
// canonical/baseline split: revert() (wired to Cancel) must populate from
// the full canonical data opts.load() returned, not from
// JSON.parse(baseline) — baseline is only the JSON snapshot of gather()'s
// narrower, page-owned shape, which routes 16/17 deliberately don't include
// data.streamers in, so restoring from it would wipe the read-only overrides
// summary on every Cancel even though nothing about streamers changed.
func TestS5_6Q3B3CancelRestoresCanonicalNotScopedBaseline(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/components/c6_form.html")

	revertFn := s56ExtractJSFunction(t, src, "function revert()")
	if !strings.Contains(revertFn, "opts.populate(canonical)") {
		t.Errorf("revert() must populate from canonical, got:\n%s", revertFn)
	}
	if strings.Contains(revertFn, "JSON.parse(baseline)") {
		t.Error("revert() must not restore from JSON.parse(baseline) — that's gather()'s narrower shape, not the full canonical data")
	}

	loadFn := s56ExtractJSFunction(t, src, "async function load()")
	if !strings.Contains(loadFn, "canonical = data") {
		t.Error("load() must capture the full loaded data into canonical before populate()")
	}

	// After a successful save, canonical must be refreshed (merged with the
	// actual committed truth — R6: opts.save()'s return value when it
	// provides one, else payload) so a LATER edit-then-Cancel cycle also
	// restores to the just-saved state, not the original page-load snapshot.
	clickHandler := s56ExtractJSFunction(t, src, "saveBtn.addEventListener('click', async function (e) {")
	if !strings.Contains(clickHandler, "canonical = Object.assign({}, canonical, truth)") {
		t.Error("save success path must refresh canonical (merge truth) before a later Cancel can rely on it")
	}
}

// TestS5_6Q3M1SaveGathersOnceBeforeAwait pins the exact bug and its fix:
// the click handler must gather() ONCE into a local variable, pass that same
// value to both opts.save() and the post-save baseline assignment — never a
// second, post-await opts.gather() call, which would silently fold whatever
// the operator typed while the request was in flight into "saved". A
// `saving` re-entrancy guard must also block a second Save while one is
// already in flight (recheck() must not re-enable it mid-request).
func TestS5_6Q3M1SaveGathersOnceBeforeAwait(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/components/c6_form.html")
	clickHandler := s56ExtractJSFunction(t, src, "saveBtn.addEventListener('click', async function (e) {")

	if !strings.Contains(clickHandler, "const payload = opts.gather();") {
		t.Fatalf("save handler must gather() once into a local `payload` before awaiting save(); got:\n%s", clickHandler)
	}
	gatherCalls := strings.Count(clickHandler, "opts.gather()")
	if gatherCalls != 1 {
		t.Errorf("save handler must call opts.gather() exactly once (got %d) — a second post-await call would re-baseline any in-flight edit as saved", gatherCalls)
	}
	if !strings.Contains(clickHandler, "const committed = await opts.save(payload);") {
		t.Error("save handler must send the captured `payload`, not a fresh gather()")
	}
	// R6: opts.save() may return the actual committed state (route 22
	// does); when it doesn't (every ordinary category), `payload` — the
	// same snapshot invariant as before this pass — remains truth.
	if !strings.Contains(clickHandler, "const truth = committed !== undefined && committed !== null ? committed : payload;") {
		t.Error("save handler must fall back to `payload` as truth when opts.save() returns nothing, preserving ordinary categories' exact prior behavior")
	}
	if !strings.Contains(clickHandler, "baseline = JSON.stringify(truth);") {
		t.Error("save handler must baseline from `truth` (committed state if opts.save() provided one, else the captured payload) — never a fresh gather()")
	}
	if !strings.Contains(clickHandler, "if (!isDirty() || saving) return;") {
		t.Error("save handler must refuse a second, overlapping save while one is already in flight")
	}

	recheckFn := s56ExtractJSFunction(t, src, "function recheck() {")
	if !strings.Contains(recheckFn, "if (saving) return;") {
		t.Error("recheck() must not flip the save bar back to dirty/re-enable Save while a save is in flight")
	}
}

// TestS5_6Q3M2EmptyRequiredNumericBlocksSaveEverywhere pins the c6 engine's
// blanket numeric-emptiness guard: an empty <input type="number"> anywhere
// in the form must block Save (validation-blocked, inline error, no
// opts.save()/POST call) — checked generically via
// form.querySelectorAll('input[type="number"]') so every category page's
// numeric fields are covered structurally, including per-streamer override
// inputs added dynamically at runtime (settings_streamers.html), not just
// the rotation/transport/predictions fields named in the bug report. An
// explicit "0" must NOT be treated as empty.
func TestS5_6Q3M2EmptyRequiredNumericBlocksSaveEverywhere(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/components/c6_form.html")

	guardFn := s56ExtractJSFunction(t, src, "function firstEmptyRequiredNumeric() {")
	if !strings.Contains(guardFn, `form.querySelectorAll('input[type="number"]')`) {
		t.Fatalf("firstEmptyRequiredNumeric() must scan every numeric input in the form, got:\n%s", guardFn)
	}
	if !strings.Contains(guardFn, "inputs[i].value.trim() === ''") {
		t.Error("firstEmptyRequiredNumeric() must treat only the empty string as blocking — an explicit \"0\" must pass")
	}

	clickHandler := s56ExtractJSFunction(t, src, "saveBtn.addEventListener('click', async function (e) {")
	guardIdx := strings.Index(clickHandler, "firstEmptyRequiredNumeric()")
	saveIdx := strings.Index(clickHandler, "opts.save(payload)")
	if guardIdx < 0 {
		t.Fatal("save handler must call firstEmptyRequiredNumeric()")
	}
	if saveIdx >= 0 && guardIdx > saveIdx {
		t.Error("the empty-numeric guard must run BEFORE opts.save() — no POST may ever be sent for a blocked field")
	}
	if !strings.Contains(clickHandler, "setState('blocked', t('js.setcat.blocked'));") {
		t.Error("save handler must render the blocked state with an inline error message")
	}
	if !strings.Contains(clickHandler, "emptyNumeric.focus();") {
		t.Error("save handler must focus the offending empty field")
	}
}

// ---------------------------------------------------------------------
// M3: structural overflow guard (complements the mandatory browser
// evidence — a rendered-layout overflow cannot be proven from HTML source
// alone, but the specific missing-min-w-0 root cause CAN be pinned so a
// future regression that removes it fails CI immediately).
// ---------------------------------------------------------------------

// s56Q3OverflowFixSites lists every <input class="input-field flex-1"> that
// must also carry min-w-0: a flex item that's a replaced element (<input>)
// has an automatic minimum size equal to its OWN intrinsic width unless
// min-width is explicitly zeroed, so flex-1 alone never lets it shrink below
// ~20ch in a narrow flex row — exactly the reproduced M3 overflow.
var s56Q3OverflowFixSites = map[string]string{
	"templates/settings_streamers.html": `id="new-streamer-input" placeholder="{{ t "set.streamers.input_placeholder" }}" class="input-field flex-1 min-w-0"`,
	"templates/settings_drops.html":     `id="gameIdLookupInput" class="input-field flex-1 min-w-0"`,
}

func TestS5_6Q3M3FlexInputsHaveMinWidthZero(t *testing.T) {
	for tmpl, want := range s56Q3OverflowFixSites {
		src := readEmbeddedTemplate(t, tmpl)
		if !strings.Contains(src, want) {
			t.Errorf("%s: expected %q (a flex-1 <input> without min-w-0 refuses to shrink below its own intrinsic width, overflowing the row)", tmpl, want)
		}
	}
	streamersSrc := readEmbeddedTemplate(t, "templates/settings_streamers.html")
	dropsSrc := readEmbeddedTemplate(t, "templates/settings_drops.html")
	if !strings.Contains(dropsSrc, `id="new-directory-game-input" placeholder="{{ t "set.directory.input_placeholder" }}" class="input-field flex-1 min-w-0"`) {
		t.Error(`settings_drops.html: expected id="new-directory-game-input" to also carry min-w-0`)
	}
	if !strings.Contains(streamersSrc, `<div class="flex flex-wrap gap-3 mb-4">`) {
		t.Error("settings_streamers.html: the add-streamer/import-followed button row needs flex-wrap too — two whitespace-nowrap buttons plus the input still overflow a narrow viewport with only min-w-0 on the input")
	}
}

// ---------------------------------------------------------------------
// S5-6 Q3 residual corrective pass: R1-R4 source-level regression pins.
// Live browser evidence for each was gathered separately (localhost-only
// disposable harnesses under /tmp, never against a tracked branch) — see
// the corrective pass's evidence report; a rendered dirty-guard/partial-
// commit/toast timing bug cannot be proven from HTML source alone, but the
// specific fix CAN be pinned so a future regression fails CI immediately.
// ---------------------------------------------------------------------

// TestS5_6Q3R1RunNowGuardedWhileDirty pins the R1 fix: the
// htmx:beforeRequest dirty-guard in settings_system.html must cover any
// request that would swap #health-center's innerHTML while the operator is
// mid-edit — not just requests whose own htmx element IS #health-center
// (the periodic poll), but also descendants of it, e.g. the "Run now"
// button, whose own hx-post lives on the button itself. Before this fix the
// guard's elt !== center check let a dirty Run-now click straight through,
// silently discarding the edit via the same whole-partial innerHTML swap
// B2 already fixed for the two forms' own submit.
func TestS5_6Q3R1RunNowGuardedWhileDirty(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/settings_system.html")
	guardFn := s56ExtractJSFunction(t, src, "document.addEventListener('htmx:beforeRequest', function (evt) {")
	if !strings.Contains(guardFn, "(elt !== center && !center.contains(elt))") {
		t.Errorf("settings_system.html: htmx:beforeRequest guard must also cover elements INSIDE #health-center (e.g. the Run now button), not just center itself; got:\n%s", guardFn)
	}
	if strings.Contains(guardFn, "elt !== center)") && !strings.Contains(guardFn, "!center.contains(elt)") {
		t.Error(`settings_system.html: guard regressed to the center-only check ("elt !== center)" with no descendant coverage)`)
	}
}

// TestS5_6Q3R2R3Route22PostsSnapshotAndPropagatesPartialCommit pins the R3
// fix (route 22's save() must use C6's own captured payload snapshot for
// both sections, never a live re-read of the section DOM inside
// postSection — the exact values C6 will baseline against must be the
// exact values sent) and the route-22 half of the R2 fix (a canary-success/
// watchdog-failure partial commit must be reported back to C6 as
// err.committed). commitSection's own reconcile-or-preserve and
// actual-committed-truth behavior is pinned separately by
// TestS5_6Q3R5SectionReconciliationPreservesNewerEdit and
// TestS5_6Q3R6CommittedTruthComesFromResponseNotPayload.
func TestS5_6Q3R2R3Route22PostsSnapshotAndPropagatesPartialCommit(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/settings_system.html")

	postSectionFn := s56ExtractJSFunction(t, src, "async function postSection(section, values) {")
	if !strings.Contains(postSectionFn, "Object.assign({ section: section }, values)") {
		t.Errorf("postSection must build its POST body from the `values` parameter, not a live DOM read; got:\n%s", postSectionFn)
	}
	if strings.Contains(postSectionFn, "fieldsOf(form)") || strings.Contains(postSectionFn, "fieldsOf(sectionForm") {
		t.Error("postSection must not re-read the live section DOM (fieldsOf) for the values it sends — that reintroduces the M1-style stale-snapshot bug for route 22 specifically")
	}

	saveFn := s56ExtractJSFunction(t, src, "save: async function (payload) {")
	if !strings.Contains(saveFn, "commitSection('canary', payload.canary)") {
		t.Errorf("save() must commit canary from payload.canary (C6's own snapshot); got:\n%s", saveFn)
	}
	if !strings.Contains(saveFn, "commitSection('watchdog', payload.watchdog)") {
		t.Errorf("save() must commit watchdog from payload.watchdog (C6's own snapshot), not a live re-read taken after canary's await; got:\n%s", saveFn)
	}
	if !strings.Contains(saveFn, "err.committed = { canary: canaryCommitted };") {
		t.Error("save() must attach the ACTUAL committed canary value (from canary's own response, not payload.canary — R6) to the thrown error when watchdog fails after canary succeeds, so C6's catch can advance baseline/canonical for canary alone (R2)")
	}
	if !strings.Contains(saveFn, "return { canary: canaryCommitted, watchdog: watchdogCommitted };") {
		t.Error("save() must return the ACTUAL committed state for both sections on full success, for C6 to baseline against (R6)")
	}
}

// TestS5_6Q3R2GenericCatchAdvancesPartialCommit pins the generic (c6_form.html)
// half of the R2 fix: when opts.save() throws an error carrying a
// `committed` property (a partial object in gather()'s own shape), the
// catch block must merge exactly that portion into both baseline and
// canonical before rendering the error — never the whole payload (only the
// genuinely committed part), and never nothing at all (the pre-fix bug,
// which left an already-committed section falsely dirty and let a
// subsequent Cancel restore stale, pre-save UI instead of the real
// committed state).
func TestS5_6Q3R2GenericCatchAdvancesPartialCommit(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/components/c6_form.html")
	clickHandler := s56ExtractJSFunction(t, src, "saveBtn.addEventListener('click', async function (e) {")

	catchIdx := strings.Index(clickHandler, "} catch (err) {")
	if catchIdx < 0 {
		t.Fatalf("could not locate the save handler's catch block; got:\n%s", clickHandler)
	}
	catchBlock := clickHandler[catchIdx:]

	for _, want := range []string{
		"if (err && err.committed) {",
		"const committedBaseline = JSON.parse(baseline);",
		"Object.assign(committedBaseline, err.committed);",
		"baseline = JSON.stringify(committedBaseline);",
		"canonical = Object.assign({}, canonical, err.committed);",
	} {
		if !strings.Contains(catchBlock, want) {
			t.Errorf("save handler's catch block missing %q; got:\n%s", want, catchBlock)
		}
	}
}

// TestS5_6Q3R4ToastOnlyWhenNotDirty pins the R4 fix: the generic engine's
// success path must not announce "Saved" unconditionally — recheck() may
// have just landed on 'dirty' (an edit made while the save request was in
// flight, never sent — M1), and telling the operator their CURRENT,
// still-unsaved edit was persisted is exactly the false claim R4 forbids.
func TestS5_6Q3R4ToastOnlyWhenNotDirty(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/components/c6_form.html")
	clickHandler := s56ExtractJSFunction(t, src, "saveBtn.addEventListener('click', async function (e) {")

	if !strings.Contains(clickHandler, "if (!isDirty() && window.showToast) window.showToast(t('js.setcat.saved'));") {
		t.Errorf("save handler must only call showToast('Saved') when !isDirty(); got:\n%s", clickHandler)
	}
	if strings.Contains(clickHandler, "if (window.showToast) window.showToast(t('js.setcat.saved'));") {
		t.Error("save handler regressed to an unconditional showToast('Saved') call, ignoring recheck()'s dirty verdict")
	}
	// The toast call must come strictly after recheck() so it reflects
	// recheck()'s just-computed state, not a stale one.
	recheckIdx := strings.Index(clickHandler, "recheck();")
	toastIdx := strings.Index(clickHandler, "showToast(t('js.setcat.saved'))")
	if recheckIdx < 0 || toastIdx < 0 || recheckIdx > toastIdx {
		t.Error("save handler must call recheck() before deciding whether to show the Saved toast")
	}
}

// ---------------------------------------------------------------------
// S5-6 Route22 final reconciliation: R5-R6 source-level regression pins.
// Live browser evidence gathered separately (localhost-only disposable
// harnesses under /tmp, never against a tracked branch).
// ---------------------------------------------------------------------

// TestS5_6Q3R5SectionReconciliationPreservesNewerEdit pins the R5 fix:
// commitSection must NEVER unconditionally applyFields() a section's
// response onto its own DOM. A section's own POST is never instantaneous
// (its own await is a real window — canary included, not just watchdog),
// so an edit made to that SAME section while its own request is in flight
// must survive; the response may only be written back if the section's
// live values still match the exact snapshot that was sent.
func TestS5_6Q3R5SectionReconciliationPreservesNewerEdit(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/settings_system.html")

	unchangedFn := s56ExtractJSFunction(t, src, "function sectionUnchangedSince(section, snapshot) {")
	if !strings.Contains(unchangedFn, "JSON.stringify(fieldsOf(sectionForm(section))) === JSON.stringify(snapshot)") {
		t.Errorf("sectionUnchangedSince must compare the section's CURRENT live DOM against the exact snapshot that was sent; got:\n%s", unchangedFn)
	}

	commitFn := s56ExtractJSFunction(t, src, "async function commitSection(section, snapshot) {")
	ifIdx := strings.Index(commitFn, "if (sectionUnchangedSince(section, snapshot)) {")
	applyIdx := strings.Index(commitFn, "applyFields(sectionForm(section), committed);")
	if ifIdx < 0 || applyIdx < 0 || ifIdx > applyIdx {
		t.Fatalf("commitSection must only applyFields() INSIDE an if (sectionUnchangedSince(...)) guard — an unconditional call would silently overwrite a newer edit made to that section during its own in-flight request; got:\n%s", commitFn)
	}
	// The guard must wrap the assignment, not merely precede it — reject a
	// mutant that keeps both lines present but un-nests the applyFields
	// call from the if-block.
	guardedBody := commitFn[ifIdx:]
	closeBraceIdx := strings.Index(guardedBody, "}")
	if closeBraceIdx < 0 || !strings.Contains(guardedBody[:closeBraceIdx], "applyFields(sectionForm(section), committed);") {
		t.Errorf("applyFields() must be nested inside the sectionUnchangedSince if-block's body, not merely follow it; got:\n%s", commitFn)
	}
	// commitSection must return the committed values regardless of whether
	// reconciliation happened — the return statement must sit OUTSIDE (after)
	// the if-block, unconditional.
	returnIdx := strings.LastIndex(commitFn, "return committed;")
	if returnIdx < 0 || returnIdx < closeBraceIdx+ifIdx {
		t.Errorf("commitSection must unconditionally return the committed values (R6), regardless of whether the DOM was reconciled; got:\n%s", commitFn)
	}
}

// TestS5_6Q3R6CommittedTruthComesFromResponseNotPayload pins the R6 fix:
// ApplyHealthSettings (internal/miner/health.go) runs config.ValidateConfig
// on the candidate and publishes THAT, so the request payload is never
// authoritative committed state. commitSection must parse the actual
// committed values from the response (committedFields), never assume the
// request payload equals what got committed — and save() must propagate
// THOSE parsed values, never payload.canary/payload.watchdog directly, both
// on a canary-success/watchdog-failure partial commit (err.committed) and
// on full success (the return value C6 baselines against — R6's other
// requirement, that on full success baseline/canonical represent the
// ACTUAL committed Canary+Watchdog state).
func TestS5_6Q3R6CommittedTruthComesFromResponseNotPayload(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/settings_system.html")

	commitFn := s56ExtractJSFunction(t, src, "async function commitSection(section, snapshot) {")
	if !strings.Contains(commitFn, "const committed = committedFields(html, section);") {
		t.Errorf("commitSection must parse the ACTUAL committed values out of the response via committedFields — never assume the request snapshot equals what was committed; got:\n%s", commitFn)
	}

	saveFn := s56ExtractJSFunction(t, src, "save: async function (payload) {")
	// The exact literal that would regress R6: reporting the REQUEST
	// payload as committed truth instead of the parsed response.
	for _, regressed := range []string{
		"err.committed = { canary: payload.canary };",
		"return { canary: payload.canary, watchdog: payload.watchdog };",
	} {
		if strings.Contains(saveFn, regressed) {
			t.Errorf("save() must never report the request payload as committed truth (%q) — ApplyHealthSettings can validate/normalize a candidate before persisting it, so only the response's own parsed values are authoritative; got:\n%s", regressed, saveFn)
		}
	}
	if !strings.Contains(saveFn, "const canaryCommitted = await commitSection('canary', payload.canary);") {
		t.Errorf("save() must capture commitSection's returned (actual, response-derived) canary value; got:\n%s", saveFn)
	}
	if !strings.Contains(saveFn, "const watchdogCommitted = await commitSection('watchdog', payload.watchdog);") {
		t.Errorf("save() must capture commitSection's returned (actual, response-derived) watchdog value; got:\n%s", saveFn)
	}

	// Generic engine: the optional-return-value contract itself, and that
	// ordinary categories (whose save() never returns anything) are
	// unaffected — the fallback to `payload` when opts.save() returns
	// undefined must remain intact (already asserted in
	// TestS5_6Q3M1SaveGathersOnceBeforeAwait; re-asserted narrowly here so
	// a mutant that removes JUST the fallback, leaving the rest of that
	// test's other assertions passing, still fails).
	c6Src := readEmbeddedTemplate(t, "templates/components/c6_form.html")
	clickHandler := s56ExtractJSFunction(t, c6Src, "saveBtn.addEventListener('click', async function (e) {")
	if !strings.Contains(clickHandler, "committed !== undefined && committed !== null ? committed : payload") {
		t.Error("c6_form.html: the generic engine must fall back to `payload` when opts.save() returns no committed-truth value, so ordinary (non-route-22) categories keep their exact prior baseline/canonical behavior")
	}
}

// ---------------------------------------------------------------------
// R7: an UNKNOWN streamer roster must never be persisted as an EMPTY one.
// ---------------------------------------------------------------------

// TestS5_6Q3R7RosterLoadFailureCannotBlankStreamerAllowLists pins route 20's
// load seam against a silent-data-loss path.
//
// Route 20's three allow-lists (mentionsStreamers/onlineStreamers/
// offlineStreamers) are gathered from checkbox containers that
// streamerCheckboxRow() builds by iterating `streamerUsernames`, and that
// roster comes from GET /api/settings. POST /api/notifications/config is a
// FULL-ROW write (this route's sole-ownership rationale). So when the roster
// fetch fails and the page carries on regardless, the containers render zero
// checkboxes, selectedFrom() returns [] for all three lists, C6 baselines
// that [] as truth, and a later edit to an entirely unrelated field (say the
// system-notifications toggle) posts those [] values — silently blanking the
// operator's persisted allow-lists. An empty roster and an unknown roster are
// indistinguishable once rendered, which is precisely the bug.
//
// The invariant: UNKNOWN roster must never become EMPTY roster. A failed
// /api/settings response is therefore load-bearing — it must throw out of
// opts.load() BEFORE C6 records a baseline. That leaves baseline null, which
// keeps isDirty() false, which makes the save handler's own early return fire
// first, so no POST can be produced from that failed-load state at all.
func TestS5_6Q3R7RosterLoadFailureCannotBlankStreamerAllowLists(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/settings_events_notifications.html")
	loadFn := s56ExtractJSFunction(t, src, "load: async function () {")

	// The exact pre-fix literal: a bare ok-check that silently tolerates a
	// failed roster fetch and falls through with streamerUsernames still [].
	if strings.Contains(loadFn, "if (settingsResp.ok) {") {
		t.Errorf("load() must not treat a failed /api/settings as merely \"no streamers\" — an unknown roster then serializes as an empty allow-list and a later unrelated save blanks it; got:\n%s", loadFn)
	}
	const guard = "if (!settingsResp.ok) throw new Error(t('js.setcat.load_error'));"
	if !strings.Contains(loadFn, guard) {
		t.Fatalf("load() must treat a failed /api/settings as a load-bearing failure via %q — the same existing error machinery the /api/notifications/config fetch below it already uses; got:\n%s", guard, loadFn)
	}

	// Ordering: the guard must precede every use of the roster and the
	// function's own successful return, so no editable state is ever
	// baselined from an unknown roster.
	guardIdx := strings.Index(loadFn, guard)
	for _, after := range []string{
		"streamerUsernames = (settings.streamers || [])",
		"return resp.json();",
	} {
		idx := strings.Index(loadFn, after)
		if idx < 0 {
			t.Errorf("load() no longer contains %q", after)
			continue
		}
		if guardIdx > idx {
			t.Errorf("the failed-/api/settings guard must run BEFORE %q — otherwise the page still reaches an editable, save-capable state built on an unknown roster", after)
		}
	}

	// Healthy route 20 behavior must be unchanged by the corrective seam.
	for _, kept := range []string{
		"const settings = await settingsResp.json();",
		"streamerUsernames = (settings.streamers || []).map(function (s) { return s.username; });",
		"const sel = document.getElementById('point-rule-streamer');",
		"await loadChannels();",
		"const resp = await fetch('/api/notifications/config');",
		"if (!resp.ok) throw new Error(t('js.setcat.load_error'));",
	} {
		if !strings.Contains(loadFn, kept) {
			t.Errorf("healthy-path behavior must be unchanged: load() must still contain %q; got:\n%s", kept, loadFn)
		}
	}

	// Why the roster is load-bearing at all: gather() reads the three
	// allow-lists out of containers streamerCheckboxRow() fills from
	// streamerUsernames. If either half of that coupling moves, this pin
	// must be revisited rather than silently passing.
	gatherFn := s56ExtractJSFunction(t, src, "function gather() {")
	for _, field := range []string{
		"mentionsStreamers: selectedFrom('mentions-streamers'),",
		"onlineStreamers: selectedFrom('online-streamers'),",
		"offlineStreamers: selectedFrom('offline-streamers'),",
	} {
		if !strings.Contains(gatherFn, field) {
			t.Errorf("gather() must still serialize %q — this pin assumes the allow-lists come from the roster-built checkboxes", field)
		}
	}
	rowFn := s56ExtractJSFunction(t, src, "function streamerCheckboxRow(containerId, values) {")
	if !strings.Contains(rowFn, "streamerUsernames.forEach(") {
		t.Error("streamerCheckboxRow() must still build its checkboxes from streamerUsernames — that coupling is what makes a failed roster fetch data-loss-bearing")
	}

	// The consequence, in the shared engine: a throw from opts.load() leaves
	// baseline null, and both isDirty() and the save handler refuse to act on
	// a null baseline. That is what makes "no POST from a failed load" true
	// rather than merely intended.
	c6 := readEmbeddedTemplate(t, "templates/components/c6_form.html")
	c6Load := s56ExtractJSFunction(t, c6, "async function load() {")
	awaitIdx := strings.Index(c6Load, "const data = await opts.load();")
	baselineIdx := strings.Index(c6Load, "baseline = JSON.stringify(opts.gather());")
	if awaitIdx < 0 || baselineIdx < 0 {
		t.Fatalf("c6 load() no longer awaits opts.load() before baselining; got:\n%s", c6Load)
	}
	if awaitIdx > baselineIdx {
		t.Error("c6 load() must await opts.load() BEFORE baselining, so a load failure cannot produce a baseline at all")
	}
	isDirtyFn := s56ExtractJSFunction(t, c6, "function isDirty() {")
	if !strings.Contains(isDirtyFn, "baseline !== null") {
		t.Error("isDirty() must stay false while baseline is null — that is what prevents any save from a failed-load state")
	}
	clickHandler := s56ExtractJSFunction(t, c6, "saveBtn.addEventListener('click', async function (e) {")
	if !strings.Contains(clickHandler, "if (!isDirty() || saving) return;") {
		t.Error("the save handler must return before opts.save() when isDirty() is false, so a failed load can never produce a POST")
	}
}

// TestS5_6Q3R8UnknownChannelDiscoveryCannotBlankPersistedChannelIds pins route
// 20's other unknown-vs-empty seam — the sibling of R7's roster fix, on the
// five Discord channel ids.
//
// The five channel <select>s are server-rendered carrying a single
// <option value=""> placeholder; every real option is added at runtime by
// loadChannels(). A <select> silently refuses a value no option carries — the
// assignment deselects everything and the empty placeholder wins the reset, so
// sel.value reads back "". When discovery adds no options, populate()'s
// persisted ids therefore evaporate on assignment: gather() reads "" for all
// five, C6 baselines those blanks as truth, and POST /api/notifications/config
// is a FULL-ROW write — so a later save of an entirely unrelated field (say the
// system-notifications toggle) writes five empty channel ids over the
// operator's persisted ones.
//
// Discovery legitimately adds nothing on two routinely reachable paths, and
// neither is "the operator has no channels configured": configValid=false
// (Discord enabled but not yet validly configured — the request is
// deliberately never made) and a request that was made and failed.
//
// The invariant: UNKNOWN/UNAVAILABLE channel discovery must never become EMPTY
// persisted channel ids. A SUCCESSFUL discovery stays authoritative and
// unchanged — an id the server no longer lists still clears exactly as before.
func TestS5_6Q3R8UnknownChannelDiscoveryCannotBlankPersistedChannelIds(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/settings_events_notifications.html")
	populateFn := s56ExtractJSFunction(t, src, "function populate(cfg) {")

	// The exact pre-fix literals: a raw assignment onto a select that may
	// carry no matching option at all.
	for _, id := range []string{"mentions", "points", "online", "offline", "system"} {
		clobber := "document.getElementById('" + id + "-channel').value = cfg."
		if strings.Contains(populateFn, clobber) {
			t.Errorf("populate() must not assign the persisted %s-channel id straight onto the select — with no matching <option> the value lands as \"\" and gather() then full-row-writes that blank over the stored id; got:\n%s", id, populateFn)
		}
	}

	// Every one of the five must go through the preservation seam instead.
	for _, want := range []string{
		"setChannel('mentions-channel', cfg.mentionsChannelId);",
		"setChannel('points-channel', cfg.pointsChannelId);",
		"setChannel('online-channel', cfg.onlineChannelId);",
		"setChannel('offline-channel', cfg.offlineChannelId);",
		"setChannel('system-channel', cfg.systemChannelId);",
	} {
		if !strings.Contains(populateFn, want) {
			t.Errorf("populate() must route the persisted channel id through the preservation seam: missing %q; got:\n%s", want, populateFn)
		}
	}

	setFn := s56ExtractJSFunction(t, src, "function setChannel(id, value) {")

	// The seam itself: while the channel list is unknown, carry the persisted
	// id in as its own option so the assignment has something to match.
	const unknownGuard = "if (v !== '' && !channelsKnown) {"
	if !strings.Contains(setFn, unknownGuard) {
		t.Fatalf("setChannel() must preserve a non-empty persisted id only while the channel list is unknown (%q) — that scoping is what leaves a SUCCESSFUL discovery authoritative; got:\n%s", unknownGuard, setFn)
	}
	// Idempotent across repeated populate() calls (C6 revert() re-populates
	// from canonical), and it never shadows a real discovered option.
	const dedupe = "Array.prototype.some.call(sel.options, function (o) { return o.value === v; })"
	if !strings.Contains(setFn, dedupe) {
		t.Errorf("setChannel() must add its carrier option only when the id is not already an option (%q) — otherwise a Cancel/re-populate cycle stacks duplicates; got:\n%s", dedupe, setFn)
	}
	appendIdx := strings.Index(setFn, "sel.appendChild(opt);")
	assignIdx := strings.Index(setFn, "sel.value = v;")
	if appendIdx < 0 || assignIdx < 0 {
		t.Fatalf("setChannel() must append the carrier option and then assign the value; got:\n%s", setFn)
	}
	if appendIdx > assignIdx {
		t.Error("setChannel() must append the carrier option BEFORE assigning the value — the other order assigns against a select that still has no matching option, which is the clobber itself")
	}

	loadChannelsFn := s56ExtractJSFunction(t, src, "async function loadChannels() {")

	// configValid=false skips discovery entirely — no request, so no failure
	// to report — and the seam above is what keeps the stored ids intact.
	const skip = "if (!configValid) return;"
	skipIdx := strings.Index(loadChannelsFn, skip)
	if skipIdx < 0 {
		t.Fatalf("loadChannels() must still skip discovery entirely when configValid is false (%q); got:\n%s", skip, loadChannelsFn)
	}

	// Only a list that actually came back makes the channel list known.
	const known = "channelsKnown = true;"
	if n := strings.Count(loadChannelsFn, known); n != 1 {
		t.Fatalf("loadChannels() must mark the channel list known exactly once, on the success path only; found %d occurrences of %q in:\n%s", n, known, loadChannelsFn)
	}
	knownIdx := strings.Index(loadChannelsFn, known)
	jsonIdx := strings.Index(loadChannelsFn, "const channels = await resp.json();")
	if jsonIdx < 0 {
		t.Fatalf("loadChannels() no longer parses the channel list; got:\n%s", loadChannelsFn)
	}
	if knownIdx < jsonIdx {
		t.Error("loadChannels() must only mark the channel list known AFTER a real list came back and was applied — marking it known any earlier hands populate() a false \"discovery succeeded\" and re-opens the clobber")
	}
	// Monotonic: nothing may un-know an already-discovered list (the sole
	// `= false` is the declaration itself). A failed refresh leaves the last
	// known options in the DOM untouched, so it does not make them unknown.
	if n := strings.Count(src, "channelsKnown = false"); n != 1 {
		t.Errorf("channelsKnown must only ever be initialized to false (its declaration), never reset — found %d occurrences of \"channelsKnown = false\"", n)
	}

	// A discovery that was attempted and failed must say so, deterministically,
	// on both failure paths — reusing the page's existing inline region and an
	// existing js.* catalog key (window.I18N only ever carries js.* keys).
	const chanFail = "showInlineFailure('settings-cat-load-error', t('js.setcat.load_error'));"
	if strings.Contains(loadChannelsFn, "if (!resp.ok) return;") {
		t.Errorf("loadChannels() must not drop a failed channel fetch silently — an operator cannot tell an unavailable channel list from an empty one; got:\n%s", loadChannelsFn)
	}
	if n := strings.Count(loadChannelsFn, chanFail); n != 2 {
		t.Errorf("loadChannels() must surface %q on BOTH attempted-and-failed paths (non-OK response and a rejected fetch); found %d; got:\n%s", chanFail, n, loadChannelsFn)
	}
	if !strings.Contains(loadChannelsFn, "clearInlineFailure('settings-cat-load-error');") {
		t.Errorf("loadChannels() must clear the inline failure once discovery succeeds, so a recovered reload does not leave a stale error on screen; got:\n%s", loadChannelsFn)
	}
	// The skip path reports nothing: no request was made to fail.
	if firstFail := strings.Index(loadChannelsFn, "showInlineFailure("); firstFail >= 0 && firstFail < skipIdx {
		t.Error("the configValid=false skip must come BEFORE any failure surface — discovery that was never attempted has not failed")
	}
	if !strings.Contains(src, `<div id="settings-cat-load-error" class="c1-inline-fail" role="alert" hidden></div>`) {
		t.Error("the page must keep its existing #settings-cat-load-error inline region — the channel-load failure surface reuses it rather than adding new UI")
	}

	// Why this is data-loss-bearing at all: gather() reads the five ids
	// straight off those selects, and route 20 posts the FULL object. If
	// either half moves, this pin must be revisited rather than silently pass.
	gatherFn := s56ExtractJSFunction(t, src, "function gather() {")
	for _, field := range []string{
		"mentionsChannelId: document.getElementById('mentions-channel').value,",
		"pointsChannelId: document.getElementById('points-channel').value,",
		"onlineChannelId: document.getElementById('online-channel').value,",
		"offlineChannelId: document.getElementById('offline-channel').value,",
		"systemChannelId: document.getElementById('system-channel').value,",
	} {
		if !strings.Contains(gatherFn, field) {
			t.Errorf("gather() must still serialize %q — this pin assumes the posted ids come from those selects", field)
		}
	}

	// Healthy discovery behavior must be unchanged by the corrective seam.
	for _, kept := range []string{
		"const resp = await fetch('/api/notifications/channels');",
		"const current = sel.value;",
		`Array.from(sel.querySelectorAll('option[value]:not([value=""])')).forEach(function (o) { o.remove(); });`,
		"sel.value = current;",
	} {
		if !strings.Contains(loadChannelsFn, kept) {
			t.Errorf("healthy-path behavior must be unchanged: loadChannels() must still contain %q; got:\n%s", kept, loadChannelsFn)
		}
	}
}

// TestS5_6Q3R9PointRuleLoadFailureIsVisible pins route 20's point-rules list
// against a silent failure.
//
// loadRules() renders the Point Goals table from GET /api/notifications/points.
// Returning on a non-OK response leaves the table exactly as it was — empty on
// first load — with no signal at all, so "the request failed" and "you have no
// point rules" look identical. The rules themselves are never lost (add/delete
// are immediate, separate actions against their own endpoints), but an
// operator reading an empty table has no way to know the list is stale, and
// may re-add a rule that already exists.
//
// The failure must be visible through the page's existing #notif-rules-error
// inline region — the same surface the add and delete handlers already use. No
// schema, API, or backend change: the endpoint and the healthy render path are
// untouched.
func TestS5_6Q3R9PointRuleLoadFailureIsVisible(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/settings_events_notifications.html")
	loadRulesFn := s56ExtractJSFunction(t, src, "async function loadRules() {")

	// The exact pre-fix literal: a bare ok-check that renders nothing and
	// reports nothing.
	if strings.Contains(loadRulesFn, "if (!resp.ok) return;") {
		t.Errorf("loadRules() must not return silently on a failed load — an empty table then means both \"no rules\" and \"could not load\"; got:\n%s", loadRulesFn)
	}
	const rulesFail = "showInlineFailure('notif-rules-error', t('js.setcat.load_error'));"
	if !strings.Contains(loadRulesFn, rulesFail) {
		t.Fatalf("loadRules() must surface the failure via %q — the existing inline region the add/delete handlers already use, with an existing js.* catalog key; got:\n%s", rulesFail, loadRulesFn)
	}
	if !strings.Contains(loadRulesFn, "clearInlineFailure('notif-rules-error');") {
		t.Errorf("loadRules() must clear the inline failure once the list loads, so a recovered reload does not leave a stale error on screen; got:\n%s", loadRulesFn)
	}
	if !strings.Contains(src, `<div id="notif-rules-error" class="c1-inline-fail" role="alert" hidden></div>`) {
		t.Error("the page must keep its existing #notif-rules-error inline region — the load failure reuses it rather than adding new UI")
	}

	// Healthy load behavior unchanged: same endpoint, same render call.
	for _, kept := range []string{
		"await fetch('/api/notifications/points')",
		"renderRules(await resp.json());",
	} {
		if !strings.Contains(loadRulesFn, kept) {
			t.Errorf("healthy-path behavior must be unchanged: loadRules() must still contain %q; got:\n%s", kept, loadRulesFn)
		}
	}

	// Add and delete are outside this seam and must be untouched, including
	// their own distinct failure messages.
	addHandler := s56ExtractJSFunction(t, src, "document.getElementById('add-point-rule-btn').addEventListener('click', async function () {")
	for _, kept := range []string{
		"showInlineFailure('notif-rules-error', t('js.notif.invalid_rule'));",
		"clearInlineFailure('notif-rules-error');",
		"await loadRules();",
		"showInlineFailure('notif-rules-error', t('js.notif.rule_add_failed'));",
	} {
		if !strings.Contains(addHandler, kept) {
			t.Errorf("the add-rule handler must be unchanged: missing %q; got:\n%s", kept, addHandler)
		}
	}
	renderFn := s56ExtractJSFunction(t, src, "function renderRules(rules) {")
	for _, kept := range []string{
		"await fetch('/api/notifications/points/' + rule.id, { method: 'DELETE' });",
		"showInlineFailure('notif-rules-error', t('js.notif.rule_delete_failed'));",
	} {
		if !strings.Contains(renderFn, kept) {
			t.Errorf("the delete-rule handler must be unchanged: missing %q; got:\n%s", kept, renderFn)
		}
	}
}

// ---------------------------------------------------------------------
// R10: Cancel before the first successful load is a harmless no-op.
// ---------------------------------------------------------------------

// TestS5_6Q3R10CancelBeforeCanonicalIsHarmlessNoOp pins the c6 engine's
// pre-canonical Cancel seam.
//
// canonical starts null and is only assigned once opts.load() resolves, but
// revert() — the function Cancel is wired to — called opts.populate(canonical)
// unconditionally. Cancel pressed while the category load is still in flight,
// or after it rejected, therefore reached populate(null). Every category
// page's populate() dereferences the data object it is handed, so that throws
// a TypeError out of the click handler; and on any page that happens to
// tolerate it, the setState-to-clean immediately after would hide the save
// bar and erase the very load error the operator is looking at. Either way the
// stable load-error lifecycle — the one surface telling the operator the page
// never loaded — is destroyed by a button that had nothing to restore.
//
// The invariant: Cancel/revert before the first successful load is a harmless
// no-op. It must not call populate, must not clear or replace the existing
// error state, must not change baseline, and must not create a
// dirty/save-capable state. The narrow seam is a canonical-null guard at the
// very top of revert(); everything after it — the healthy
// populate/onLoaded/clean sequence — is unchanged, and no other part of the
// state machine moves.
func TestS5_6Q3R10CancelBeforeCanonicalIsHarmlessNoOp(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/components/c6_form.html")

	// The precondition the guard reads: canonical is null until a load
	// resolves, which is what makes "no successful load yet" detectable.
	if !strings.Contains(src, "let canonical = null;") {
		t.Fatal("c6_form.html: canonical must start null — the guard below has no other way to detect that no load has succeeded yet")
	}

	revertFn := s56ExtractJSFunction(t, src, "function revert()")
	const guard = "if (canonical === null) return;"
	guardIdx := strings.Index(revertFn, guard)
	if guardIdx < 0 {
		t.Fatalf("revert() must return early while canonical is still null — Cancel during a pending load, or after a rejected one, otherwise runs the whole restore path against null; got:\n%s", revertFn)
	}

	// Scenarios 1 and 2 (pending load + Cancel, rejected load + Cancel),
	// structurally: the guard has to precede EVERY effect revert() has, or a
	// pre-canonical Cancel still performs one of them.
	for _, effect := range []string{
		// populate(null) — the throw itself.
		"opts.populate(canonical);",
		// the page's own post-populate hook, re-run against null.
		"opts.onLoaded(canonical)",
		// the state stomp: hides the bar and clears the visible load error.
		"setState('clean', '');",
	} {
		effectIdx := strings.Index(revertFn, effect)
		if effectIdx < 0 {
			t.Errorf("revert() lost its healthy-path effect %q; got:\n%s", effect, revertFn)
			continue
		}
		if guardIdx > effectIdx {
			t.Errorf("the canonical-null guard must come BEFORE %q in revert(), or a pre-canonical Cancel still runs it; got:\n%s", effect, revertFn)
		}
	}

	// A no-op really is a no-op: revert() must not have grown its own
	// error rendering or baseline handling, so whatever the load().catch
	// handler put on screen stays exactly as it was.
	if strings.Contains(revertFn, "baseline =") {
		t.Errorf("revert() must not assign baseline — a pre-canonical Cancel must leave the (still null) baseline alone; got:\n%s", revertFn)
	}
	if strings.Contains(revertFn, "setState('error',") {
		t.Errorf("revert() must not re-render the error state — it leaves whatever load().catch set untouched; got:\n%s", revertFn)
	}

	// Scenario 3, and the guard's own release condition: Cancel is still
	// wired straight to revert(), and a load that succeeds later still
	// assigns canonical — so an early Cancel costs nothing and every Cancel
	// after that first success behaves exactly as before.
	cancelHandler := s56ExtractJSFunction(t, src, "cancelBtn.addEventListener('click', function (e) {")
	if !strings.Contains(cancelHandler, "revert();") {
		t.Errorf("Cancel must still be wired to revert(); got:\n%s", cancelHandler)
	}
	loadFn := s56ExtractJSFunction(t, src, "async function load()")
	if !strings.Contains(loadFn, "canonical = data;") {
		t.Error("load() must still assign canonical, so a load that succeeds after an early Cancel opens the guard for every later Cancel")
	}

	// Save stays impossible across the whole pre-canonical window: baseline
	// is still null, isDirty() is false for that reason alone, and the save
	// handler refuses a click that is not dirty. A pre-canonical Cancel
	// changes none of it, because it now changes nothing at all.
	isDirtyFn := s56ExtractJSFunction(t, src, "function isDirty()")
	if !strings.Contains(isDirtyFn, "baseline !== null") {
		t.Error("isDirty() must stay false while baseline is null, so Save is impossible before the first successful load")
	}
	if !strings.Contains(src, "if (!isDirty() || saving) return;") {
		t.Error("the save handler must still refuse a click while the form is not dirty")
	}
}
