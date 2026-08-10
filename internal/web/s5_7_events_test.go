package web

// Task S5-7 (Events group, routes 9-12) tests: /events becomes the real
// session-scoped event journal (five PRESENTATION groups over exactly ten
// underlying event types, S-SESS banner, S-EMPTY, manual refresh, no
// polling, no delivery evidence), and /events/browser, /events/sound,
// /events/discord become direct-render status pages (honest states,
// gesture-only permission/test actions, existing POST /api/notifications/test
// only, token never rendered). The C2 nav gains Events as its fifth parent
// group with exactly four children (5 parents / 23 children in total), and
// the Overview card events drawer (events_drawer.html) is localized (Г2).

import (
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
)

// s5_7EventsChildRoutes is the exact four-child destination set of the
// Events C2 group (task S5-7 C2/ROUTING).
var s5_7EventsChildRoutes = []string{"/events", "/events/browser", "/events/sound", "/events/discord"}

// s5_7Loc loads the embedded locale catalogs once per test.
func s5_7Loc(t *testing.T) *i18n.Localizer {
	t.Helper()
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	return loc
}

// s5_7JournalFixture returns one event of each of the exactly ten journal
// types (newest first, like events.Recent), each with a distinct streamer
// and detail so row cells can be asserted individually.
func s5_7JournalFixture() []events.Event {
	types := []events.Type{
		events.TypeStreamerOnline, events.TypeStreamerOffline,
		events.TypeBonusClaimed, events.TypePointsEarned, events.TypeRewardRedeemed,
		events.TypeBetPlaced, events.TypeBetResult,
		events.TypeDropClaimed, events.TypeMomentClaimed,
		events.TypeRaidJoined,
	}
	out := make([]events.Event, 0, len(types))
	for i, ty := range types {
		out = append(out, events.Event{
			Time:     time.Now().Add(-time.Duration(i+1) * time.Minute),
			Type:     ty,
			Streamer: "chan_" + string(ty),
			Detail:   "detail_" + string(ty),
		})
	}
	return out
}

// s5_7WantGroupByType is the OWNER-mandated five-group presentation mapping
// over the ten underlying event types (types themselves unchanged).
var s5_7WantGroupByType = map[events.Type]string{
	events.TypeStreamerOnline:  "streamer-status",
	events.TypeStreamerOffline: "streamer-status",
	events.TypeBonusClaimed:    "points-rewards",
	events.TypePointsEarned:    "points-rewards",
	events.TypeRewardRedeemed:  "points-rewards",
	events.TypeBetPlaced:       "predictions",
	events.TypeBetResult:       "predictions",
	events.TypeDropClaimed:     "drops",
	events.TypeMomentClaimed:   "drops",
	events.TypeRaidJoined:      "raids",
}

// ---------------------------------------------------------------------------
// C2 nav: Events is the fifth parent group with exactly four children.
// ---------------------------------------------------------------------------

var s5_7EventsParentTagRe = regexp.MustCompile(`<a href="/events" class="c2-nav-link" data-nav-section="events"[^>]*data-nav-parent>`)

// TestS5_7NavEventsGroupStructure proves Events renders as a C2 parent group
// (data-nav-parent) whose four children are exactly /events, /events/browser,
// /events/sound and /events/discord — in both languages.
func TestS5_7NavEventsGroupStructure(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/overview", lang)

		if !s5_7EventsParentTagRe.MatchString(body) {
			t.Errorf("[%s] Events top-level link must be a group parent (data-nav-parent)", lang)
		}
		for _, href := range s5_7EventsChildRoutes {
			want := `href="` + href + `" class="c2-nav-child" data-nav-section="events" data-nav-child`
			if !strings.Contains(body, want) {
				t.Errorf("[%s] Events group missing child destination %q", lang, href)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Routes 9-12: direct-render 200 (GET and HEAD), one h1, localized RU+EN.
// ---------------------------------------------------------------------------

// TestS5_7EventsChildRoutesRender200BothLanguages proves all four Events
// routes direct-render 200 for GET and HEAD in both languages, each with
// exactly one localized <h1>.
func TestS5_7EventsChildRoutesRender200BothLanguages(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()
	loc := s5_7Loc(t)

	wantH1 := map[string]string{
		"/events":         "events.title",
		"/events/browser": "events.browser.title",
		"/events/sound":   "events.sound.title",
		"/events/discord": "events.discord.title",
	}

	for _, path := range s5_7EventsChildRoutes {
		for _, lang := range []string{"en", "ru"} {
			body := f3GetPage(t, srv, path, lang)
			if n := strings.Count(body, "<h1"); n != 1 {
				t.Errorf("%s (lang=%s): expected exactly one <h1>, found %d", path, lang, n)
			}
			// html/template escapes the catalog text (e.g. "&" -> "&amp;"),
			// so compare against the escaped form.
			if want := html.EscapeString(loc.T(lang, wantH1[path])); !strings.Contains(body, want) {
				t.Errorf("%s (lang=%s): missing localized heading %q", path, lang, want)
			}
		}

		recHead := httptest.NewRecorder()
		h.ServeHTTP(recHead, httptest.NewRequest(http.MethodHead, path, nil))
		if recHead.Code != http.StatusOK {
			t.Errorf("HEAD %s = %d, want 200", path, recHead.Code)
		}

		recPost := httptest.NewRecorder()
		h.ServeHTTP(recPost, httptest.NewRequest(http.MethodPost, path, nil))
		if recPost.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405 (page routes are GET/HEAD-only)", path, recPost.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// Route 9: the journal's exact five-group / ten-type presentation mapping.
// ---------------------------------------------------------------------------

// TestS5_7EventsJournalGroupMapping seeds one event of each of the ten
// journal types plus two non-journal types and proves: every journal event
// renders exactly one table row and one card carrying its mandated
// presentation group, and non-journal types never appear.
func TestS5_7EventsJournalGroupMapping(t *testing.T) {
	srv := buildF3PageServer(t)
	fixture := append([]events.Event{
		{Time: time.Now(), Type: events.TypeMinerStarted, Streamer: "", Detail: "excluded_miner_started"},
		{Time: time.Now(), Type: events.TypeSlotAssigned, Streamer: "chan_x", Detail: "excluded_slot_assigned"},
	}, s5_7JournalFixture()...)
	srv.recentEvents = func(int) []events.Event { return fixture }

	body := f3GetPage(t, srv, "/events", "en")

	if n := strings.Count(body, "data-ev-row"); n != 10 {
		t.Errorf("expected exactly 10 journal table rows, found %d", n)
	}
	// Trailing space keeps the card marker from also matching the
	// data-ev-cards container attribute.
	if n := strings.Count(body, "data-ev-card "); n != 10 {
		t.Errorf("expected exactly 10 journal cards, found %d", n)
	}
	for ty, group := range s5_7WantGroupByType {
		want := `data-ev-type="` + string(ty) + `" data-ev-group="` + group + `"`
		if n := strings.Count(body, want); n != 2 { // one table row + one card
			t.Errorf("event type %s: expected exactly one row and one card with group %q (marker %q ×2), found %d", ty, group, want, n)
		}
	}
	for _, banned := range []string{"excluded_miner_started", "excluded_slot_assigned", string(events.TypeMinerStarted), string(events.TypeSlotAssigned)} {
		if strings.Contains(body, banned) {
			t.Errorf("non-journal event material %q must never appear on /events", banned)
		}
	}
	// Row cells carry streamer and detail separately (no delivery column).
	if !strings.Contains(body, "chan_"+string(events.TypeBonusClaimed)) || !strings.Contains(body, "detail_"+string(events.TypeBonusClaimed)) {
		t.Error("journal rows must render the event's streamer and detail")
	}
}

// TestS5_7EventsJournalGroupFilter proves the five group filters (plus All)
// are same-route GET links, an active group narrows the rows to exactly that
// group's types, and an unknown group value falls back to All.
func TestS5_7EventsJournalGroupFilter(t *testing.T) {
	srv := buildF3PageServer(t)
	srv.recentEvents = func(int) []events.Event { return s5_7JournalFixture() }

	body := f3GetPage(t, srv, "/events", "en")
	if n := strings.Count(body, `data-ev-filter="`); n != 6 {
		t.Errorf("expected exactly 6 filter links (All + five groups), found %d", n)
	}
	for _, key := range []string{"all", "streamer-status", "points-rewards", "predictions", "drops", "raids"} {
		if !strings.Contains(body, `data-ev-filter="`+key+`"`) {
			t.Errorf("missing filter link %q", key)
		}
	}

	filtered := f3GetPage(t, srv, "/events?group=predictions", "en")
	if n := strings.Count(filtered, "data-ev-row"); n != 2 {
		t.Errorf("?group=predictions: expected exactly 2 rows (bet_placed + bet_result), found %d", n)
	}
	for _, ty := range []events.Type{events.TypeBetPlaced, events.TypeBetResult} {
		if !strings.Contains(filtered, `data-ev-type="`+string(ty)+`"`) {
			t.Errorf("?group=predictions: missing row for %s", ty)
		}
	}
	if strings.Contains(filtered, `data-ev-type="`+string(events.TypeDropClaimed)+`"`) {
		t.Error("?group=predictions must not render drops rows")
	}
	if !strings.Contains(filtered, `data-ev-filter="predictions" data-ev-filter-active`) {
		t.Error("?group=predictions must mark the predictions filter active")
	}

	bogus := f3GetPage(t, srv, "/events?group=bogus", "en")
	if n := strings.Count(bogus, "data-ev-row"); n != 10 {
		t.Errorf("?group=bogus must fall back to All (10 rows), found %d", n)
	}
}

// ---------------------------------------------------------------------------
// Route 9: S-SESS session banner, S-EMPTY, manual refresh, honesty bans.
// ---------------------------------------------------------------------------

// TestS5_7EventsJournalSessionBanner proves the session-scoped (S-SESS)
// banner renders on /events in both languages, with and without rows.
func TestS5_7EventsJournalSessionBanner(t *testing.T) {
	srv := buildF3PageServer(t)
	loc := s5_7Loc(t)
	for _, rows := range []bool{false, true} {
		if rows {
			srv.recentEvents = func(int) []events.Event { return s5_7JournalFixture() }
		} else {
			srv.recentEvents = func(int) []events.Event { return nil }
		}
		for _, lang := range []string{"en", "ru"} {
			body := f3GetPage(t, srv, "/events", lang)
			if !strings.Contains(body, "data-events-session-note") {
				t.Errorf("[rows=%v lang=%s] /events missing the S-SESS session banner", rows, lang)
			}
			if want := loc.T(lang, "events.session_note"); !strings.Contains(body, want) {
				t.Errorf("[rows=%v lang=%s] /events missing localized session note %q", rows, lang, want)
			}
		}
	}
}

// TestS5_7EventsJournalEmptyState proves an empty session journal renders the
// honest S-EMPTY state (C1 block, localized) and no table/rows at all — and
// that a populated-but-filtered-empty group gets its own distinct message.
func TestS5_7EventsJournalEmptyState(t *testing.T) {
	srv := buildF3PageServer(t)
	loc := s5_7Loc(t)
	srv.recentEvents = func(int) []events.Event { return nil }

	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/events", lang)
		if strings.Contains(body, "data-ev-row") || strings.Contains(body, "data-ev-table") {
			t.Errorf("[%s] empty journal must render no table or rows", lang)
		}
		if !strings.Contains(body, "c1-block") {
			t.Errorf("[%s] empty journal must render the C1 S-EMPTY state block", lang)
		}
		if want := loc.T(lang, "events.empty"); !strings.Contains(body, want) {
			t.Errorf("[%s] empty journal missing localized empty message %q", lang, want)
		}
	}

	// Populated session, but the selected group has no events: the distinct
	// group-empty message renders, never the all-empty one.
	srv.recentEvents = func(int) []events.Event {
		return []events.Event{{Time: time.Now(), Type: events.TypeRaidJoined, Streamer: "chan_r", Detail: "d"}}
	}
	body := f3GetPage(t, srv, "/events?group=predictions", "en")
	if strings.Contains(body, "data-ev-row") {
		t.Error("group with no events must render no rows")
	}
	if want := loc.T("en", "events.empty_group"); !strings.Contains(body, want) {
		t.Errorf("group-empty journal missing localized message %q", want)
	}
}

// TestS5_7EventsSessionBannerCautionTierAndFlag pins the S-SESS banner's
// presentation semantics: session-scoped data is a caution-tier state carrying
// the visible C10 session flag marker (⚑, the drops_claims.html precedent),
// never a neutral info note.
func TestS5_7EventsSessionBannerCautionTierAndFlag(t *testing.T) {
	srv := buildF3PageServer(t)
	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/events", lang)
		idx := strings.Index(body, "data-events-session-note")
		if idx < 0 {
			t.Fatalf("[%s] /events missing the S-SESS session banner", lang)
		}
		// The banner's opening tag precedes the marker attribute; the icon
		// span follows it.
		open := strings.LastIndex(body[:idx], "<div")
		end := strings.Index(body[idx:], "</div>")
		if open < 0 || end < 0 {
			t.Fatalf("[%s] could not slice the S-SESS banner block", lang)
		}
		banner := body[open : idx+end]
		if !strings.Contains(banner, "c1-tier-caution") {
			t.Errorf("[%s] S-SESS banner must use the caution tier (c1-tier-caution)", lang)
		}
		if strings.Contains(banner, "c1-tier-info") {
			t.Errorf("[%s] S-SESS banner must not be a neutral info note (c1-tier-info)", lang)
		}
		if !strings.Contains(banner, "⚑") {
			t.Errorf("[%s] S-SESS banner must carry the visible C10 session flag marker ⚑", lang)
		}
	}
}

// TestS5_7EventsJournalNoDataCells proves a journal row whose event has no
// streamer or no detail renders the C4 no-data convention — the visible —
// dash wrapped in the accessible, localized queue.slot.no_data label the C4
// table and C3 card components already use — in BOTH representations: the
// desktop table cell and the mobile card row (cards render the row with the
// marker, never silently omit it). Never a bare unlabeled dash, never a
// fabricated 0.
func TestS5_7EventsJournalNoDataCells(t *testing.T) {
	srv := buildF3PageServer(t)
	loc := s5_7Loc(t)
	srv.recentEvents = func(int) []events.Event {
		return []events.Event{
			{Time: time.Now().Add(-time.Minute), Type: events.TypePointsEarned, Streamer: "", Detail: "d_present"},
			{Time: time.Now().Add(-2 * time.Minute), Type: events.TypeRaidJoined, Streamer: "c_present", Detail: ""},
		}
	}
	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/events", lang)
		want := `<span aria-label="` + html.EscapeString(loc.T(lang, "queue.slot.no_data")) + `">—</span>`
		// One missing streamer + one missing detail, each in the table AND in
		// its mobile card.
		if n := strings.Count(body, want); n != 4 {
			t.Errorf("[%s] expected exactly 4 accessible no-data markers %q (table + card × streamer + detail), found %d", lang, want, n)
		}
		// The bare, unlabeled dash cell must be gone from the table cells.
		for _, bare := range []string{">—</td>", `<td class="text-text-muted">—</td>`} {
			if strings.Contains(body, bare) {
				t.Errorf("[%s] table must not render a bare unlabeled — cell, found %q", lang, bare)
			}
		}
		// The mobile cards keep row parity with the table: every card renders
		// its Channel and Detail rows even when the value is absent.
		for _, colKey := range []string{"events.col.channel", "events.col.detail"} {
			label := html.EscapeString(loc.T(lang, colKey))
			marker := `<span class="health-card-row-label">` + label + `</span>`
			if n := strings.Count(body, marker); n != 2 {
				t.Errorf("[%s] every card must render its %s row (want %q ×2, found %d)", lang, colKey, marker, n)
			}
		}
	}
}

// TestS5_7EventsFilterAriaCurrent proves exactly the active route-9 group
// filter carries aria-current="true" — All on the unfiltered view, the
// selected group on a filtered view — and inactive filters carry none.
func TestS5_7EventsFilterAriaCurrent(t *testing.T) {
	srv := buildF3PageServer(t)
	srv.recentEvents = func(int) []events.Event { return s5_7JournalFixture() }

	cases := []struct {
		path string
		want string // filter key expected to be current
	}{
		{"/events", "all"},
		{"/events?group=predictions", "predictions"},
		{"/events?group=bogus", "all"}, // unknown group falls back to All
	}
	for _, tc := range cases {
		body := f3GetPage(t, srv, tc.path, "en")
		// Server-rendered pages carry no other aria-current ATTRIBUTE (the
		// nav sets its own client-side via setAttribute — a JS string, not
		// the attribute form counted here), so the page-wide attribute count
		// pins "exactly the active filter, no inactive one".
		if n := strings.Count(body, `aria-current="`); n != 1 {
			t.Errorf("%s: expected exactly one aria-current attribute in the rendered page, found %d", tc.path, n)
		}
		want := `aria-current="true" data-ev-filter="` + tc.want + `" data-ev-filter-active`
		if !strings.Contains(body, want) {
			t.Errorf("%s: the active filter %q must carry aria-current=\"true\" (want marker %q)", tc.path, tc.want, want)
		}
	}
}

// TestS5_7EventsUnknownSubpathNotFound pins the routing honesty regression:
// an unknown /events/* subpath is not swallowed by any Events handler — it
// falls through the mux to the "/" catch-all and 404s like any other
// unregistered path.
func TestS5_7EventsUnknownSubpathNotFound(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events/not-a-route", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /events/not-a-route = %d, want 404", rec.Code)
	}
}

// TestS5_7EventsJournalManualRefresh proves the journal's only liveness
// affordance is a same-route manual refresh link that preserves the active
// group filter.
func TestS5_7EventsJournalManualRefresh(t *testing.T) {
	srv := buildF3PageServer(t)
	srv.recentEvents = func(int) []events.Event { return s5_7JournalFixture() }

	body := f3GetPage(t, srv, "/events", "en")
	if !strings.Contains(body, `href="/events" class="btn-secondary text-sm" data-events-refresh`) {
		t.Error("/events missing the same-route manual refresh link")
	}
	filtered := f3GetPage(t, srv, "/events?group=drops", "en")
	if !strings.Contains(filtered, `href="/events?group=drops" class="btn-secondary text-sm" data-events-refresh`) {
		t.Error("/events?group=drops refresh link must preserve the active group")
	}
}

// TestS5_7EventsJournalNoDeliveryNoPolling pins route 9's honesty bans: no
// delivery column/facet (B3 has no backend), no fabricated delivery/sound/
// browser evidence, and no polling/SSE/WS transport in the template at all.
func TestS5_7EventsJournalNoDeliveryNoPolling(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/events.html")
	for _, banned := range []string{"hx-get", "hx-trigger", "hx-post", "EventSource", "WebSocket", "setInterval", "setTimeout"} {
		if strings.Contains(src, banned) {
			t.Errorf("events.html must not contain polling/SSE/WS transport, found %q", banned)
		}
	}

	srv := buildF3PageServer(t)
	srv.recentEvents = func(int) []events.Event { return s5_7JournalFixture() }
	body := f3GetPage(t, srv, "/events", "en")

	for _, banned := range []string{"delivered", "delivery status", "sound played", "browser permission"} {
		if strings.Contains(strings.ToLower(body), banned) {
			t.Errorf("/events must not fabricate delivery/sound/browser evidence, found %q", banned)
		}
	}

	// The table has exactly five columns (When/Group/Event/Channel/Detail) —
	// never a delivery column — and the filter strip has no delivery facet.
	tableIdx := strings.Index(body, "data-ev-table")
	if tableIdx < 0 {
		t.Fatal("/events with rows must render the data-ev-table table")
	}
	tableEnd := strings.Index(body[tableIdx:], "</table>")
	if tableEnd < 0 {
		t.Fatal("could not locate the end of the journal table")
	}
	table := body[tableIdx : tableIdx+tableEnd]
	if n := strings.Count(table, `<th scope="col"`); n != 5 {
		t.Errorf("journal table must have exactly 5 columns, found %d", n)
	}
	if strings.Contains(strings.ToLower(table), "delivery") {
		t.Error("journal table must not carry a delivery column")
	}
	// No nav counts: the Events nav entries never render a count badge.
	if regexp.MustCompile(`data-nav-child>[^<]*\(\d+\)`).MatchString(body) {
		t.Error("nav children must not carry event counts")
	}
}

// ---------------------------------------------------------------------------
// Route 10: /events/browser — pure Notification API, gesture-only.
// ---------------------------------------------------------------------------

// TestS5_7BrowserPageGestureSourceContract proves the browser-notifications
// page uses only the Notification API, requests permission and fires the
// test notification exclusively inside explicit click listeners, and renders
// the honest server-side initial state.
func TestS5_7BrowserPageGestureSourceContract(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/events_browser.html")

	if n := strings.Count(src, "Notification.requestPermission"); n != 1 {
		t.Fatalf("expected exactly one Notification.requestPermission call, found %d", n)
	}
	reqListener := strings.Index(src, "reqBtn.addEventListener('click'")
	reqCall := strings.Index(src, "Notification.requestPermission")
	if reqListener < 0 || reqCall < reqListener {
		t.Error("Notification.requestPermission must only run inside the request button's click listener")
	}

	if n := strings.Count(src, "new Notification("); n != 1 {
		t.Fatalf("expected exactly one new Notification(...) call, found %d", n)
	}
	testListener := strings.Index(src, "testBtn.addEventListener('click'")
	testCall := strings.Index(src, "new Notification(")
	if testListener < 0 || testCall < testListener {
		t.Error("the test notification must only fire inside the test button's click listener")
	}

	for _, banned := range []string{"hx-get", "hx-post", "EventSource", "WebSocket", "localStorage", "sessionStorage", "fetch("} {
		if strings.Contains(src, banned) {
			t.Errorf("events_browser.html must be pure Notification API — found %q", banned)
		}
	}

	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/events/browser", "en")
	if !strings.Contains(body, `data-evb-status="unknown"`) {
		t.Error("/events/browser must render the honest initial unknown status server-side")
	}
	for _, id := range []string{`id="evb-request"`, `id="evb-test"`} {
		if !strings.Contains(body, id) {
			t.Errorf("/events/browser missing control %s", id)
		}
	}
}

// TestS5_7BrowserPermissionMappingNeverDefaultsPositive pins the honest
// permission mapping: only 'granted' is READY; 'denied' is DENY; the
// not-yet-requested 'default' state falls through to BLOCK, never READY;
// a missing Notification API is UNK.
func TestS5_7BrowserPermissionMappingNeverDefaultsPositive(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/events_browser.html")

	for _, want := range []string{
		`if (!('Notification' in window)) return 'unsupported';`,
		`if (p === 'granted') return 'ready';`,
		`if (p === 'denied') return 'denied';`,
		`return 'blocked';`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("events_browser.html missing honest permission-mapping literal %q", want)
		}
	}
	if n := strings.Count(src, `return 'ready';`); n != 1 {
		t.Errorf("exactly one code path (permission === 'granted') may map to 'ready', found %d", n)
	}
}

// ---------------------------------------------------------------------------
// Route 11: /events/sound — gesture WebAudio test, fail-open, no persistence.
// ---------------------------------------------------------------------------

// TestS5_7SoundPageContract proves the sound page is a status+gesture-test
// surface only: WebAudio behind a click, honest unsupported/blocked states,
// a visible fail-open statement, and no persisted sound preference.
func TestS5_7SoundPageContract(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/events_sound.html")

	if !strings.Contains(src, "window.AudioContext || window.webkitAudioContext") {
		t.Error("events_sound.html must feature-detect Web Audio honestly")
	}
	clickIdx := strings.Index(src, "btn.addEventListener('click'")
	ctxIdx := strings.Index(src, "new AC(")
	if clickIdx < 0 || ctxIdx < clickIdx {
		t.Error("the AudioContext must only be created inside the test button's click listener (user gesture)")
	}
	for _, want := range []string{`'unsupported'`, `'blocked'`, `'played'`, "data-evs-status"} {
		if !strings.Contains(src, want) {
			t.Errorf("events_sound.html missing state literal %q", want)
		}
	}
	for _, banned := range []string{"localStorage", "sessionStorage", "fetch(", "hx-get", "hx-post", "EventSource", "WebSocket"} {
		if strings.Contains(src, banned) {
			t.Errorf("events_sound.html must not persist preferences or talk to the backend — found %q", banned)
		}
	}

	srv := buildF3PageServer(t)
	loc := s5_7Loc(t)
	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/events/sound", lang)
		if !strings.Contains(body, "data-evs-failopen") {
			t.Errorf("[%s] /events/sound missing the visible fail-open statement", lang)
		}
		if want := loc.T(lang, "events.sound.failopen"); !strings.Contains(body, want) {
			t.Errorf("[%s] /events/sound missing localized fail-open text %q", lang, want)
		}
		if !strings.Contains(body, `id="evs-test"`) {
			t.Errorf("[%s] /events/sound missing the test button", lang)
		}
	}
}

// ---------------------------------------------------------------------------
// Route 12: /events/discord — status/test only, honest states, token secrecy.
// ---------------------------------------------------------------------------

// s5_7DiscordManager builds a real notifications.Manager over a throwaway DB
// with the given Discord settings, mirroring newNotificationsTestManager.
func s5_7DiscordManager(t *testing.T, dc *config.DiscordSettings) *notifications.Manager {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	mgr, err := notifications.NewManager(dc, nil, db, nil, "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr
}

// TestS5_7DiscordPageStates proves /events/discord renders the honest
// disabled / config-invalid / ready states, only ever offering the test
// button when a test could actually be attempted.
func TestS5_7DiscordPageStates(t *testing.T) {
	loc := s5_7Loc(t)

	// Discord disabled: honest disabled state, link to the Discord settings
	// owner (route 21), no test control.
	srv := buildF3PageServer(t)
	srv.SetDiscordEnabled(false)
	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/events/discord", lang)
		if want := loc.T(lang, "events.discord.disabled"); !strings.Contains(body, want) {
			t.Errorf("[%s] disabled /events/discord missing %q", lang, want)
		}
		if !strings.Contains(body, `href="/settings/discord"`) {
			t.Errorf("[%s] disabled /events/discord must link to /settings/discord", lang)
		}
		if strings.Contains(body, `id="evd-test"`) {
			t.Errorf("[%s] disabled /events/discord must not offer a test button", lang)
		}
	}

	// Enabled but invalid config (no bot token): honest invalid state with
	// the manager's own static reason, still no test control.
	srvInvalid := buildF3PageServer(t)
	srvInvalid.SetDiscordEnabled(true)
	srvInvalid.SetNotificationManager(s5_7DiscordManager(t, &config.DiscordSettings{Enabled: true}))
	invalid := f3GetPage(t, srvInvalid, "/events/discord", "en")
	if want := loc.T("en", "events.discord.invalid"); !strings.Contains(invalid, want) {
		t.Errorf("config-invalid /events/discord missing %q", want)
	}
	if !strings.Contains(invalid, "Discord bot token is not configured") {
		t.Error("config-invalid /events/discord must surface the manager's static reason")
	}
	if strings.Contains(invalid, `id="evd-test"`) {
		t.Error("config-invalid /events/discord must not offer a test button")
	}

	// Enabled and valid: ready state with the test control.
	srvReady := buildF3PageServer(t)
	srvReady.SetDiscordEnabled(true)
	srvReady.SetNotificationManager(s5_7DiscordManager(t, &config.DiscordSettings{Enabled: true, BotToken: "s57-test-token", GuildID: "1"}))
	ready := f3GetPage(t, srvReady, "/events/discord", "en")
	if want := loc.T("en", "events.discord.ready"); !strings.Contains(ready, want) {
		t.Errorf("ready /events/discord missing %q", want)
	}
	if !strings.Contains(ready, `id="evd-test"`) {
		t.Error("ready /events/discord must offer the test button")
	}
}

// TestS5_7DiscordTestEndpointContract pins route 12's transport: the test
// button POSTs the EXISTING /api/notifications/test endpoint (never the
// broader /api/test-notification), guards against double-fire while a test
// is in flight, and fails inline (S-FAIL, role=alert) with a Retry that
// re-runs the same gesture-driven send.
func TestS5_7DiscordTestEndpointContract(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/events_discord.html")

	if n := strings.Count(src, "fetch('/api/notifications/test', { method: 'POST' })"); n != 1 {
		t.Fatalf("expected exactly one POST to /api/notifications/test, found %d", n)
	}
	if strings.Contains(src, "/api/test-notification") {
		t.Error("events_discord.html must use POST /api/notifications/test only — never /api/test-notification")
	}

	for _, want := range []string{
		"if (busy) return;", // in-flight double-fire guard
		"busy = true;",
		"busy = false;",
		`id="evd-fail"`,
		`role="alert"`,
		`id="evd-retry"`,
		"btn.addEventListener('click', send);",
		"retry.addEventListener('click', send);",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("events_discord.html missing contract literal %q", want)
		}
	}
}

// TestS5_7DiscordFailureCauseContract pins the S-FAIL cause semantics of the
// route-12 test button: an HTTP failure surfaces a localized safe cause
// carrying ONLY the numeric status code, a network/other failure surfaces the
// static localized cause, every failure stamps the current attempt time into
// the single in-place S-FAIL region, and neither the response body nor raw
// exception text can ever reach the page.
func TestS5_7DiscordFailureCauseContract(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/events_discord.html")

	for _, want := range []string{
		// HTTP failure: localized cause with the safe numeric status only.
		"t('js.evd.failed_http', { code: err.httpStatus })",
		"httpErr.httpStatus = res.status;",
		// Network/other failure: the static localized cause.
		"t('js.evd.failed_network')",
		// Every failure stamps the current attempt time, locale-aware.
		`id="evd-fail-time"`,
		"toLocaleTimeString(document.documentElement.lang || undefined)",
		"t('js.evd.failed_at', { time:",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("events_discord.html missing S-FAIL cause literal %q", want)
		}
	}

	// The failure region is a single in-place block (repeated failures update
	// it, never stack), and success hides it before/after every attempt.
	if n := strings.Count(src, `id="evd-fail"`); n != 1 {
		t.Errorf("expected exactly one S-FAIL region, found %d", n)
	}
	if !strings.Contains(src, "failEl.hidden = true;") {
		t.Error("every attempt must clear the S-FAIL region before running (success clears failure)")
	}

	// The response body and raw exception text must be unreachable: the
	// script may only read res.status off a failed response, never its body,
	// and never interpolates the caught error's own text.
	for _, banned := range []string{"res.text(", "res.statusText", "responseText", "err.message", "String(err", "+ err"} {
		if strings.Contains(src, banned) {
			t.Errorf("events_discord.html must never surface response-body/raw-error text — found %q", banned)
		}
	}
}

// TestS5_7DiscordTokenNeverRendered proves the configured bot token never
// reaches the rendered page in any state, and the template/handler never
// reference the token field at all.
func TestS5_7DiscordTokenNeverRendered(t *testing.T) {
	const sentinel = "S57-FAKE-DISCORD-TOKEN-SENTINEL"

	src := readEmbeddedTemplate(t, "templates/events_discord.html")
	for _, banned := range []string{"BotToken", "botToken"} {
		if strings.Contains(src, banned) {
			t.Errorf("events_discord.html must never reference the bot token field, found %q", banned)
		}
	}

	srv := buildF3PageServer(t)
	srv.SetDiscordEnabled(true)
	srv.SetNotificationManager(s5_7DiscordManager(t, &config.DiscordSettings{Enabled: true, BotToken: sentinel, GuildID: "42"}))
	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/events/discord", lang)
		if strings.Contains(body, sentinel) {
			t.Errorf("[%s] /events/discord rendered the bot token", lang)
		}
	}
}

// s5_7DeliveryRegion slices a rendered /events/discord body down to the
// last-delivery evidence region (the data-evd-delivery C1 block), so the
// honesty bans below scrutinize the region that would carry a delivery
// claim — not unrelated page chrome.
func s5_7DeliveryRegion(t *testing.T, body string) string {
	t.Helper()
	idx := strings.Index(body, "data-evd-delivery")
	if idx < 0 {
		t.Fatal("/events/discord missing the last-delivery S-UNK region")
	}
	end := strings.Index(body[idx:], "</div>")
	if end < 0 {
		t.Fatal("could not locate the end of the last-delivery region")
	}
	return body[idx : idx+end]
}

// TestS5_7DiscordDeliveryUnknown proves the "last delivery" evidence is an
// honest S-UNK — no delivery history backend exists (B3), and
// upcoming_campaign_notifications explicitly does not satisfy it — so the
// delivery region must never positively claim a past delivery happened, in
// either language, in any letter case. The honest "history is not recorded /
// unknown" copy is exactly what must remain.
func TestS5_7DiscordDeliveryUnknown(t *testing.T) {
	srv := buildF3PageServer(t)
	srv.SetDiscordEnabled(true)
	loc := s5_7Loc(t)

	// Positive-delivery claims, matched case-insensitively against the
	// lowercased delivery region. Chosen so a genuine claim ("Delivered",
	// "delivery succeeded", "Доставлено", "успешно доставлен...") is caught
	// while the honest EN copy ("Delivery history is not recorded…") and RU
	// copy ("История доставки не записывается…") never match: "delivery"
	// alone is legitimate, "delivered" is not; "доставки"/"доставка"
	// (history-of-delivery forms) are legitimate, the participle stem
	// "доставлен" (доставлено/доставлена/доставлены — past-delivery claims)
	// is not.
	bannedByLang := map[string][]string{
		"en": {"delivered", "delivery succeeded", "delivery successful", "delivery ok", "was sent successfully"},
		"ru": {"доставлен", "успешно"},
	}

	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/events/discord", lang)
		region := s5_7DeliveryRegion(t, body)
		if want := loc.T(lang, "events.discord.delivery_unknown"); !strings.Contains(region, want) {
			t.Errorf("[%s] delivery region missing localized delivery-unknown text %q", lang, want)
		}
		lower := strings.ToLower(region)
		for _, banned := range bannedByLang[lang] {
			if strings.Contains(lower, strings.ToLower(banned)) {
				t.Errorf("[%s] delivery region must never claim a past delivery, found %q", lang, banned)
			}
		}
	}

	// Route 9 (the journal) has no delivery evidence at all (B3 absent): a
	// positive past-delivery claim must never appear anywhere on /events, in
	// either language, in any letter case.
	journalSrv := buildF3PageServer(t)
	journalSrv.recentEvents = func(int) []events.Event { return s5_7JournalFixture() }
	journalBans := map[string][]string{
		"en": {"delivered"},
		"ru": {"доставлен", "доставк"},
	}
	for _, lang := range []string{"en", "ru"} {
		lower := strings.ToLower(f3GetPage(t, journalSrv, "/events", lang))
		for _, banned := range journalBans[lang] {
			if strings.Contains(lower, strings.ToLower(banned)) {
				t.Errorf("[%s] /events must carry no delivery evidence (B3 absent), found %q", lang, banned)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// aria-current: exactly one destination on each Events child route.
// ---------------------------------------------------------------------------

// TestS5_7AriaCurrentExactlyOnePerEventsChildRoute re-implements base.html's
// client-side updateActiveNav decision in Go (the same simulation
// TestS5_3OverviewQueueExactlyOneAriaCurrentDestination pins) against the
// rendered C2 markup of each of the four Events child routes, proving
// exactly one destination — the route's own child link — would receive
// aria-current="page".
func TestS5_7AriaCurrentExactlyOnePerEventsChildRoute(t *testing.T) {
	base := readEmbeddedTemplate(t, "templates/base.html")
	for _, want := range []string{
		`sectionMatches && a.getAttribute('href') === path`,
		`!isParent && isCurrent`,
	} {
		if !strings.Contains(base, want) {
			t.Fatalf("base.html no longer contains %q - update this simulation to match", want)
		}
	}

	srv := buildF3PageServer(t)
	for _, path := range s5_7EventsChildRoutes {
		t.Run(path, func(t *testing.T) {
			body := f3GetPage(t, srv, path, "en")
			const active = "events" // SECTION_RULES: /events and /events/* -> events

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

// ---------------------------------------------------------------------------
// Г2: the Overview events drawer is localized (RU/EN, atomically).
// ---------------------------------------------------------------------------

// TestS5_7DrawerLocalized proves events_drawer.html renders fully localized
// output in both languages and no longer hardcodes its four English strings.
func TestS5_7DrawerLocalized(t *testing.T) {
	src := readEmbeddedTemplate(t, "templates/partials/events_drawer.html")
	for _, banned := range []string{"Recent events (up to 20)", "Full page", "No events recorded for this streamer yet.", `aria-label="Close"`} {
		if strings.Contains(src, banned) {
			t.Errorf("events_drawer.html must be localized — found hardcoded %q", banned)
		}
	}
	for _, key := range []string{"drawer.recent_events", "drawer.full_page", "drawer.close", "drawer.empty"} {
		if !strings.Contains(src, `t "`+key+`"`) {
			t.Errorf("events_drawer.html missing localization call for %q", key)
		}
	}

	loc := s5_7Loc(t)
	for _, lang := range []string{i18n.LangEN, i18n.LangRU} {
		tmpl := testPartialsLang(t, lang)

		var populated strings.Builder
		if err := tmpl.ExecuteTemplate(&populated, "events_drawer", map[string]interface{}{
			"Name": "shroud",
			"Events": []struct {
				Label string
				Ago   string
			}{{Label: "evt", Ago: "2m"}},
		}); err != nil {
			t.Fatalf("render events_drawer (%s): %v", lang, err)
		}
		for _, key := range []string{"drawer.recent_events", "drawer.full_page"} {
			if want := loc.T(lang, key); !strings.Contains(populated.String(), want) {
				t.Errorf("[%s] events_drawer missing localized %q (%q)", lang, key, want)
			}
		}
		if want := loc.T(lang, "drawer.close"); !strings.Contains(populated.String(), `aria-label="`+want+`"`) {
			t.Errorf("[%s] events_drawer close button missing localized aria-label %q", lang, want)
		}

		var empty strings.Builder
		if err := tmpl.ExecuteTemplate(&empty, "events_drawer", map[string]interface{}{"Name": "shroud"}); err != nil {
			t.Fatalf("render empty events_drawer (%s): %v", lang, err)
		}
		if want := loc.T(lang, "drawer.empty"); !strings.Contains(empty.String(), want) {
			t.Errorf("[%s] empty events_drawer missing localized %q", lang, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Locale keys: present, non-empty, actually translated in both languages.
// ---------------------------------------------------------------------------

// s5_7DeliberatelyIdenticalKeys lists S5-7 keys whose EN/RU values are
// deliberately identical (brand proper nouns), mirroring the
// s56DeliberatelyIdenticalKeys precedent.
var s5_7DeliberatelyIdenticalKeys = map[string]bool{
	"events.tab.discord": true,
}

// TestS5_7LocaleKeysPresentAndTranslated guards the full S5-7 key set: each
// key resolves to a non-empty, genuinely translated string in both RU and EN.
func TestS5_7LocaleKeysPresentAndTranslated(t *testing.T) {
	loc := s5_7Loc(t)
	keys := []string{
		"events.tab.journal", "events.tab.browser", "events.tab.sound", "events.tab.discord",
		"events.session_note", "events.refresh", "events.table_caption",
		"events.filter.label", "events.filter.all",
		"events.group.streamer_status", "events.group.points_rewards",
		"events.group.predictions", "events.group.drops", "events.group.raids",
		"events.col.time", "events.col.group", "events.col.event", "events.col.channel", "events.col.detail",
		"events.empty", "events.empty_group",
		"events.browser.title", "events.browser.lead", "events.browser.initial",
		"events.browser.request", "events.browser.test",
		"events.sound.title", "events.sound.lead", "events.sound.initial",
		"events.sound.failopen", "events.sound.test",
		"events.discord.title", "events.discord.lead", "events.discord.disabled",
		"events.discord.link_settings", "events.discord.invalid", "events.discord.ready",
		"events.discord.test", "events.discord.retry",
		"events.discord.delivery_heading", "events.discord.delivery_unknown",
		"drawer.recent_events", "drawer.full_page", "drawer.close", "drawer.empty",
		"js.evb.ready", "js.evb.blocked", "js.evb.denied", "js.evb.unsupported",
		"js.evb.test_title", "js.evb.test_body",
		"js.evs.unknown", "js.evs.unsupported", "js.evs.blocked", "js.evs.played",
		"js.evd.sent", "js.evd.failed_http", "js.evd.failed_network", "js.evd.failed_at",
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
		if en == ru && !s5_7DeliberatelyIdenticalKeys[k] {
			t.Errorf("%q has identical EN/RU text %q — looks untranslated", k, en)
		}
	}
}
