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

// TestS5_6SNoBackFieldsAbsent proves every category page omits UI for a
// field the task brief explicitly scoped out (either genuinely absent from
// the backend, or deliberately carved into a different route): route 18
// (Transport) never exposes requestDelay/proxy/client-id/user-agent; route
// 20 (Events & Notifications) never exposes sound/quiet-hours/upload
// controls (NotificationConfig has no such fields); route 22 (System) never
// exposes an updater editor or a LAN-CIDR control.
func TestS5_6SNoBackFieldsAbsent(t *testing.T) {
	srv := buildF3PageServer(t)

	transport := f3GetPage(t, srv, "/settings/transport", "en")
	for _, banned := range []string{"requestDelay", "proxy", "client-id", "clientId", "user-agent", "userAgent"} {
		if strings.Contains(transport, banned) {
			t.Errorf("/settings/transport must not expose %q", banned)
		}
	}

	events := f3GetPage(t, srv, "/settings/events-notifications", "en")
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

	httpSrv := &http.Server{Addr: addr, Handler: srv.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpSrv.ListenAndServe() }()
	t.Logf("S5-6 evidence harness serving on http://%s — try /settings/streamers, /settings/rotation, /settings/drops, /settings/predictions, /settings/chat-raids, /settings/transport, /settings/analytics-logging, /settings/events-notifications, /settings/discord, /settings/system", addr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
	case <-time.After(30 * time.Minute):
	}
	_ = httpSrv.Shutdown(context.Background())
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
	canaryIdx := strings.Index(saveFn, "postSection('canary', payload.canary)")
	watchdogIdx := strings.Index(saveFn, "postSection('watchdog', payload.watchdog)")
	if canaryIdx < 0 || watchdogIdx < 0 {
		t.Fatalf("save() must post both sections via postSection(), from C6's own captured payload (R3); got:\n%s", saveFn)
	}
	if canaryIdx > watchdogIdx {
		t.Error("save() must POST canary strictly before watchdog (sequential, matching healthFormMu's own serialization)")
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
	// just-sent payload) so a LATER edit-then-Cancel cycle also restores to
	// the just-saved state, not the original page-load snapshot.
	clickHandler := s56ExtractJSFunction(t, src, "saveBtn.addEventListener('click', async function (e) {")
	if !strings.Contains(clickHandler, "canonical = Object.assign({}, canonical, payload)") {
		t.Error("save success path must refresh canonical (merge payload) before a later Cancel can rely on it")
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
	if !strings.Contains(clickHandler, "await opts.save(payload);") {
		t.Error("save handler must send the captured `payload`, not a fresh gather()")
	}
	if !strings.Contains(clickHandler, "baseline = JSON.stringify(payload);") {
		t.Error("save handler must baseline from the captured `payload`, not a fresh gather()")
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
// err.committed, and watchdog's own committed DOM must NOT be reapplied on
// full success — doing so risks clobbering a newer edit made during
// watchdog's real await window with a value that was never actually sent).
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
	if !strings.Contains(saveFn, "postSection('canary', payload.canary)") {
		t.Errorf("save() must POST canary from payload.canary (C6's own snapshot); got:\n%s", saveFn)
	}
	if !strings.Contains(saveFn, "postSection('watchdog', payload.watchdog)") {
		t.Errorf("save() must POST watchdog from payload.watchdog (C6's own snapshot), not a live re-read taken after canary's await; got:\n%s", saveFn)
	}
	if !strings.Contains(saveFn, "err.committed = { canary: payload.canary };") {
		t.Error("save() must attach the committed canary snapshot to the thrown error when watchdog fails after canary succeeds, so C6's catch can advance baseline/canonical for canary alone (R2)")
	}
	// Exactly one applyFields call (canary's, immediately after its own
	// postSection — safe, no await separates it from the payload snapshot).
	// Watchdog's committed fields must never be re-applied to its own live
	// DOM on the full-success path.
	if strings.Count(saveFn, "applyFields(") != 1 {
		t.Errorf("save() must call applyFields exactly once (for canary only) — got %d; re-applying watchdog's committed DOM on full success risks clobbering a newer edit made during its own await window with a value that was never sent, got:\n%s", strings.Count(saveFn, "applyFields("), saveFn)
	}
	if !strings.Contains(saveFn, "applyFields(sectionForm('canary')") {
		t.Error("save() must still reconcile canary's own DOM with its committed response")
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
