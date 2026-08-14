package web

// S5-3 Phase 5/6/7/8/10 tests: the /overview/queue route, the full
// configured-streamer roster, C4 table semantics, C3 responsive semantics,
// shared filter/sort state, and the exactly-once C18 DPBA card.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestS5_3QueueRosterRowHasNoUnusedCampaignFields proves the Q3 MINOR-3 fix:
// queueRosterRow no longer declares HasCampaign/Campaign - fields that were
// never read by queue.html, the C4/C3 templates, or any handler (a dead,
// never-rendered pair of struct fields, not a real campaign column).
func TestS5_3QueueRosterRowHasNoUnusedCampaignFields(t *testing.T) {
	typ := reflect.TypeOf(queueRosterRow{})
	for _, name := range []string{"HasCampaign", "Campaign"} {
		if _, ok := typ.FieldByName(name); ok {
			t.Errorf("queueRosterRow must not declare %s - it was never read by any template or handler", name)
		}
	}
}

// ---- Phase 5: route ---------------------------------------------------------

// TestS5_3QueueRouteReturns200 proves /overview/queue is now a real
// direct-render route (GET and HEAD), and /overview still is too.
func TestS5_3QueueRouteReturns200(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	for _, path := range []string{"/overview", "/overview/queue"} {
		rec, body := httpGetBody(t, h, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200; body=%s", path, rec.Code, body)
		}

		recHead := httptest.NewRecorder()
		h.ServeHTTP(recHead, httptest.NewRequest(http.MethodHead, path, nil))
		if recHead.Code != http.StatusOK {
			t.Errorf("HEAD %s = %d, want 200", path, recHead.Code)
		}
	}
}

// TestS5_3RemainingDeferredRoutesStill404 re-confirmed (independently of
// TestS5_2DeferredRoutesRemain404, which S5-3 was told to update by removing
// exactly /overview/queue) that every OTHER deferred route, including
// /help/glossary and /help/troubleshooting specifically, was still an
// honest 404 - S5-3 must not have accidentally widened the route table.
// task S5-4 removed /drops/claims from this list: it became a real
// direct-render route (handlers_drops.go) - see s5_4_drops_test.go. task
// S5-7 removed /events/browser, /events/sound and /events/discord: each
// became a real direct-render route (handlers_events.go) - see
// s5_7_events_test.go. task S5-9 removed the last four entries
// (/help/glossary, /help/troubleshooting, /help/notifications-audio,
// /help/diagnostics-support): each is now a real direct-render route
// (handlers_help.go) - see s5_9_help_test.go. With the list empty, this
// test is retired (mirrors TestS5_2DeferredRoutesRemain404's own S5-9
// retirement in s5_2_redirects_test.go).

// TestS5_3QueueRouteOneH1AndNoRedirectLoop proves the page has exactly one
// h1, was never redirected, and does not capture any existing API/JSON path.
func TestS5_3QueueRouteOneH1AndNoRedirectLoop(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()

	rec, body := httpGetBody(t, h, "/overview/queue")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /overview/queue = %d, want 200", rec.Code)
	}
	if n := strings.Count(body, "<h1"); n != 1 {
		t.Errorf("expected exactly one <h1>, found %d; body=%s", n, body)
	}

	// Existing API/JSON endpoints remain reachable and unaffected.
	for _, path := range []string{"/api/settings", "/api/status", "/api/now-watching"} {
		r2, b2 := httpGetBody(t, h, path)
		if r2.Code != http.StatusOK {
			t.Errorf("existing endpoint %s = %d, want 200; body=%s", path, r2.Code, b2)
		}
	}
}

// TestS5_3QueueTitleIsLocalized proves the Q3 MINOR-1 fix: /overview/queue's
// <title> uses the existing translated queue.title key instead of a
// hardcoded English string, so it actually changes with the language cookie.
func TestS5_3QueueTitleIsLocalized(t *testing.T) {
	srv := buildF3PageServer(t)
	bodyEN := f3GetPage(t, srv, "/overview/queue", "en")
	bodyRU := f3GetPage(t, srv, "/overview/queue", "ru")

	// queue.title contains "&", so the rendered (HTML-escaped) title carries
	// "&amp;" - this proves the key is actually going through {{ t ... }}
	// (which auto-escapes), not just string-matched some other way.
	if !strings.Contains(bodyEN, "<title>Queue &amp; assignments - Twitch Points Miner</title>") {
		t.Errorf("[en] <title> must use the localized queue.title key, body=%s", bodyEN)
	}
	if !strings.Contains(bodyRU, "<title>Очередь и назначения - Twitch Points Miner</title>") {
		t.Errorf("[ru] <title> must use the localized queue.title key, body=%s", bodyRU)
	}
	if strings.Contains(bodyEN, "<title>Queue - Twitch Points Miner</title>") {
		t.Error("<title> must not be the old hardcoded, unlocalized string")
	}
}

// ---- Phase 6: full roster ---------------------------------------------------

// TestS5_3FullRosterIncludesEveryConfiguredState proves the roster is built
// from the COMPLETE configured streamer list - offline, disabled-watch,
// unknown, and watching entries all present - never a filtered subset
// (LiveCards-only, Waiting-only, eligible-only, online-only).
func TestS5_3FullRosterIncludesEveryConfiguredState(t *testing.T) {
	watchingOne := models.NewStreamer("watching_one", models.DefaultStreamerSettings())
	watchingOne.SetConfirmedOnline()

	offlineOne := models.NewStreamer("offline_one", models.DefaultStreamerSettings())
	offlineOne.SetConfirmedOffline()

	disabledSettings := models.DefaultStreamerSettings()
	disabledSettings.DisableWatch = true
	disabledOne := models.NewStreamer("disabled_one", disabledSettings)
	disabledOne.SetConfirmedOffline()

	unknownOne := models.NewStreamer("unknown_one", models.DefaultStreamerSettings())
	unknownOne.SetConfirmedOnline()
	unknownOne.SetUnknown(models.ReasonTransportError)

	streamers := []*models.Streamer{watchingOne, offlineOne, disabledOne, unknownOne}
	slots := WatchSlotsView{Watching: map[string]bool{"watching_one": true}}

	srv := &Server{}
	rows := srv.buildQueueRoster(streamers, slots, map[string]streamerStats{}, watchSlotEvidence{}, enTR(t))
	sortRosterByChannel(rows)

	if len(rows) != 4 {
		t.Fatalf("roster has %d rows, want 4 (full configured list); rows=%+v", len(rows), rows)
	}
	byName := map[string]queueRosterRow{}
	for _, r := range rows {
		byName[r.Channel] = r
	}
	if byName["watching_one"].Status != "watching" {
		t.Errorf("watching_one status = %q, want watching", byName["watching_one"].Status)
	}
	if byName["offline_one"].Status != "offline" {
		t.Errorf("offline_one status = %q, want offline", byName["offline_one"].Status)
	}
	if byName["disabled_one"].Status != "disabled" || !byName["disabled_one"].DisableWatch {
		t.Errorf("disabled_one = %+v, want status=disabled, DisableWatch=true", byName["disabled_one"])
	}
	if byName["unknown_one"].Status != "unknown" {
		t.Errorf("unknown_one status = %q, want unknown", byName["unknown_one"].Status)
	}
}

// TestS5_3PointsTodayPreservedThroughBuildQueueRoster proves the real,
// end-to-end path: a streamer with a genuine stats-map hit for today's points
// comes back from buildQueueRoster with HasPointsToday=true and both the
// formatted and raw values preserved — never gated on the stats entry merely
// existing (CodeRabbit PR152 finding), and never lost through the Name/stats
// key that buildCards and buildQueueRoster now share via a single
// GetUsername() snapshot.
func TestS5_3PointsTodayPreservedThroughBuildQueueRoster(t *testing.T) {
	st := models.NewStreamer("alpha", models.DefaultStreamerSettings())
	st.SetConfirmedOffline()
	st.SetChannelPoints(9000)
	stats := map[string]streamerStats{"alpha": {pointsToday: 4321}}

	srv := &Server{}
	rows := srv.buildQueueRoster([]*models.Streamer{st}, WatchSlotsView{}, stats, watchSlotEvidence{}, enTR(t))
	if len(rows) != 1 {
		t.Fatalf("roster has %d rows, want 1", len(rows))
	}

	row := rows[0]
	if !row.HasPointsToday {
		t.Errorf("row.HasPointsToday = false, want true: stats had a real, non-empty PointsToday hit; row=%+v", row)
	}
	if row.PointsToday != "4,321" {
		t.Errorf("row.PointsToday = %q, want formatted \"4,321\"; row=%+v", row.PointsToday, row)
	}
	if row.PointsTodayRaw != 4321 {
		t.Errorf("row.PointsTodayRaw = %d, want raw 4321; row=%+v", row.PointsTodayRaw, row)
	}
}

// TestS5_3PointsTodayAbsentThroughBuildQueueRoster proves the complementary
// no-stats case through the same real path: a streamer with no matching
// stats-map entry comes back with HasPointsToday=false and an empty
// formatted value, never a stale or defaulted value.
func TestS5_3PointsTodayAbsentThroughBuildQueueRoster(t *testing.T) {
	st := models.NewStreamer("beta", models.DefaultStreamerSettings())
	st.SetConfirmedOffline()
	st.SetChannelPoints(500)

	srv := &Server{}
	rows := srv.buildQueueRoster([]*models.Streamer{st}, WatchSlotsView{}, map[string]streamerStats{}, watchSlotEvidence{}, enTR(t))
	if len(rows) != 1 {
		t.Fatalf("roster has %d rows, want 1", len(rows))
	}

	row := rows[0]
	if row.HasPointsToday {
		t.Errorf("row.HasPointsToday = true, want false: no stats entry exists for this channel; row=%+v", row)
	}
	if row.PointsToday != "" {
		t.Errorf("row.PointsToday = %q, want empty: no stats entry exists for this channel; row=%+v", row.PointsToday, row)
	}
}

// TestS5_3BuildCardsSingleUsernameSnapshotAST proves, independent of any
// runtime race window, that buildCards' streamer loop reads st.GetUsername()
// exactly once per iteration and reuses that snapshot for every
// identity-keyed lookup in the same iteration (card.Name, the stats key,
// lastEventFor, the ticker's Streamer field, predByStreamer, and both
// WatchReason assignments) — never a second independent read that a
// concurrent RenameIfCurrent could observe differently from the first.
func TestS5_3BuildCardsSingleUsernameSnapshotAST(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "handlers_overview.go", nil, 0)
	if err != nil {
		t.Fatalf("parse handlers_overview.go: %v", err)
	}

	var buildCards *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "buildCards" {
			buildCards = fn
			break
		}
	}
	if buildCards == nil {
		t.Fatal("buildCards function declaration not found in handlers_overview.go")
	}

	var streamerLoop *ast.RangeStmt
	ast.Inspect(buildCards.Body, func(n ast.Node) bool {
		if streamerLoop != nil {
			return false
		}
		if rs, ok := n.(*ast.RangeStmt); ok {
			if ident, ok := rs.X.(*ast.Ident); ok && ident.Name == "streamers" {
				streamerLoop = rs
				return false
			}
		}
		return true
	})
	if streamerLoop == nil {
		t.Fatal("buildCards: could not find `for _, st := range streamers` loop")
	}

	count := 0
	ast.Inspect(streamerLoop.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "GetUsername" {
			count++
		}
		return true
	})

	if count != 1 {
		t.Errorf("buildCards streamer loop calls st.GetUsername() %d times, want exactly 1 (single snapshot reused for every identity-keyed read)", count)
	}
}

// TestS5_3BuildNowWatchingQueuedLoopSingleUsernameSnapshotAST extends the
// same single-snapshot invariant to buildNowWatching's queued loop, which
// carried the identical torn read: slots.Watching[st.GetUsername()] and the
// view.QueuedNames append were two independent RLock-scoped reads in one
// iteration, so a concurrent RenameIfCurrent landing between them could queue
// a name that was never the one tested against the watching set (CodeRabbit
// PR152 follow-up finding).
//
// buildNowWatching ranges over `streamers` twice - once to build the byName
// index, once for the queued names - so the loop is identified by structural
// evidence in its own body (it assigns to view.QueuedNames), never by its
// numeric position among the function's loops.
func TestS5_3BuildNowWatchingQueuedLoopSingleUsernameSnapshotAST(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "handlers_overview.go", nil, 0)
	if err != nil {
		t.Fatalf("parse handlers_overview.go: %v", err)
	}

	var buildNowWatching *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "buildNowWatching" {
			buildNowWatching = fn
			break
		}
	}
	if buildNowWatching == nil {
		t.Fatal("buildNowWatching function declaration not found in handlers_overview.go")
	}

	// Every `range streamers` loop whose body mentions view.QueuedNames. The
	// byName loop does not, so it is excluded on evidence rather than order.
	var queuedLoops []*ast.RangeStmt
	ast.Inspect(buildNowWatching.Body, func(n ast.Node) bool {
		rs, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		ident, ok := rs.X.(*ast.Ident)
		if !ok || ident.Name != "streamers" {
			return true
		}
		touchesQueuedNames := false
		ast.Inspect(rs.Body, func(inner ast.Node) bool {
			sel, ok := inner.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "QueuedNames" {
				return true
			}
			if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == "view" {
				touchesQueuedNames = true
				return false
			}
			return true
		})
		if touchesQueuedNames {
			queuedLoops = append(queuedLoops, rs)
		}
		return true
	})

	if len(queuedLoops) != 1 {
		t.Fatalf("buildNowWatching: found %d `for ... := range streamers` loops assigning view.QueuedNames, want exactly 1 (the queued loop must stay unambiguously identifiable)", len(queuedLoops))
	}

	count := 0
	ast.Inspect(queuedLoops[0].Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "GetUsername" {
			count++
		}
		return true
	})

	if count != 1 {
		t.Errorf("buildNowWatching queued loop calls st.GetUsername() %d times, want exactly 1 (one snapshot reused for both the Watching lookup and the QueuedNames append)", count)
	}
}

// TestS5_3PointsTodayMissingRendersNoDataMarker proves the C4/C3 templates
// render the existing "—" missing-data state (never a blank cell) when
// HasPointsToday is false, while the raw sort attribute still carries the
// stats-derived value for client-side sorting (task Phase 3 item H).
func TestS5_3PointsTodayMissingRendersNoDataMarker(t *testing.T) {
	row := queueRosterRow{
		Channel: "streamer_a", Status: "offline", StatusLabel: "Offline",
		// ReasonCode and Points are deliberately given real, present values
		// (HasReasonCode/HasPoints=true) so only the Today column can produce
		// the "no data" marker below - otherwise a page-wide Contains check
		// could not tell a Today-specific regression apart from the other
		// two columns' own (correct) no-data markers.
		HasReasonCode: true, ReasonCode: "priority_slot", ReasonLabel: "Priority slot",
		HasPoints: true, Points: "9,000", PointsRaw: 9000,
		HasPointsToday: false, PointsToday: "", PointsTodayRaw: 500,
	}
	data := QueuePageData{Roster: []queueRosterRow{row}}

	partials := testPartialsLang(t, i18n.LangEN)
	var buf strings.Builder
	if err := partials.ExecuteTemplate(&buf, "c4.roster_table", data); err != nil {
		t.Fatalf("render c4.roster_table: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `data-qr-today="500"`) {
		t.Errorf("C4 row must still carry the raw sort attribute from PointsTodayRaw; body=%s", out)
	}
	if !strings.Contains(out, `aria-label="no data">—<`) {
		t.Errorf("C4 row missing PointsToday must render the \"—\" no-data marker, not a blank cell; body=%s", out)
	}
}

// TestS5_3QueueEmptyStateCTATargetsSettingsStreamers proves the empty-roster
// state's action link targets the canonical streamer-roster owner route,
// /settings/streamers - matching the doc comment above QueuePageData and
// Stage 4's assignment of the streamer roster to that route - never the bare
// /settings landing page (CodeRabbit PR152 finding: code and comment
// disagreed).
func TestS5_3QueueEmptyStateCTATargetsSettingsStreamers(t *testing.T) {
	srv := buildF3PageServer(t)
	srv.AttachStreamers(nil)
	data := srv.buildQueuePageData("en")
	// This fixture server has no configured streamers, so the roster is
	// empty and the S-EMPTY state block renders.
	if !data.RosterEmpty {
		t.Fatal("expected an empty roster for a server with no configured streamers")
	}
	if data.EmptyState.ActionTarget != "/settings/streamers" {
		t.Errorf("empty-state ActionTarget = %q, want /settings/streamers", data.EmptyState.ActionTarget)
	}
}

// TestS5_3RosterNeverFabricatesQueuePosition proves queueRosterRow carries no
// position/rank field and the rendered table/cards never print a "#1"-style
// numbered position.
func TestS5_3RosterNeverFabricatesQueuePosition(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview/queue", "en")

	positionPattern := regexp.MustCompile(`#\d+\s*</`)
	if positionPattern.MatchString(body) {
		t.Errorf("rendered queue page must never print a fabricated numbered position, matched in: %s", body)
	}
}

// TestS5_3RosterFullRosterOnLivePage exercises the real page render (not
// just the unit-level builder) and proves an offline, non-watched streamer
// from the F3 fixture set still appears on /overview/queue.
func TestS5_3RosterFullRosterOnLivePage(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview/queue", "en")
	for _, want := range []string{"streamer_a", "streamer_b"} {
		if !strings.Contains(body, want) {
			t.Errorf("queue page missing configured streamer %q", want)
		}
	}
}

// ---- Phase 6/10 item 16: ReasonCode rendered, free-form Reason absent -----

// TestS5_3QueueTableRendersReasonCodeNeverFreeFormReason extends the Phase 3
// leak proof (s5_3_watch_evidence_test.go) to the queue page specifically,
// now that it exists: the safe ReasonCode surfaces, the sentinel never does.
func TestS5_3QueueTableRendersReasonCodeNeverFreeFormReason(t *testing.T) {
	srv := s53LeakTestServer(t)
	for _, lang := range []string{"en", "ru"} {
		body := f3GetPage(t, srv, "/overview/queue", lang)
		if strings.Contains(body, s53SentinelSlotReason) || strings.Contains(body, s53SentinelWaitingReason) {
			t.Errorf("[%s] /overview/queue leaked a free-form Reason sentinel", lang)
		}
	}
	// The safe ReasonCode DOES surface somewhere (localized, not the raw code).
	body := f3GetPage(t, srv, "/overview/queue", "en")
	if !strings.Contains(body, enTR(t)("queue.reason.priority")) {
		t.Error("queue page must render the safe ReasonCode evidence somewhere")
	}
}

// ---- Phase 7/10 item 17: C4 semantics ---------------------------------------

func TestS5_3C4TableSemantics(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview/queue", "en")

	if n := strings.Count(body, `<th scope="col">`); n < 2 {
		t.Errorf(`<th scope="col"> count = %d, want at least 2`, n)
	}
	for _, want := range []string{
		`data-qr-sort="channel"`, `data-qr-sort="points"`, `data-qr-sort="today"`,
		`aria-sort="none"`,
		`id="qr-sort-announce"`, `aria-live="polite"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("queue page missing C4 literal %q", want)
		}
	}
	// Sortable headers are real buttons, not divs/spans pretending to be one.
	if !regexp.MustCompile(`<button[^>]*data-qr-sort="channel"`).MatchString(body) {
		t.Error("the channel sort control must be a real <button>")
	}
	// Overflow-x lives on the table's own card wrapper only.
	if !strings.Contains(body, `class="c4-table-card"`) {
		t.Error("queue page missing the c4-table-card overflow wrapper")
	}
}

// TestS5_3C4TableCardHasVerticalScrollBoundForStickyHeader proves the C4
// table wrapper has a genuine vertical scroll boundary (max-height + a
// scrolling overflow, not just overflow-x), so the sticky <thead th> rule
// has an actual nearest scroll ancestor to stick within (CodeRabbit PR152
// finding: overflow-x alone makes overflow-y compute to auto with no height
// bound, so the container never scrolls vertically and the header can never
// separate from the rows). Horizontal scrolling and sticky headers both stay
// enabled; the page itself must never scroll horizontally.
func TestS5_3C4TableCardHasVerticalScrollBoundForStickyHeader(t *testing.T) {
	css := stripCSSComments(readEmbeddedStatic(t, "static/css/input.css"))

	ruleRe := regexp.MustCompile(`\.c4-table-card\s*\{([^}]*)\}`)
	m := ruleRe.FindStringSubmatch(css)
	if m == nil {
		t.Fatal("input.css missing the .c4-table-card rule")
	}
	rule := m[1]
	if !cssHasBoundedMaxHeight(rule) {
		t.Errorf(".c4-table-card must declare a max-height with a real value so it has a genuine vertical scroll boundary; rule=%s", rule)
	}
	if !cssHasScrollingOverflow(rule) {
		t.Errorf(".c4-table-card must set overflow (or overflow-y) to auto or scroll so the height bound actually scrolls; rule=%s", rule)
	}

	stickyRe := regexp.MustCompile(`\.c4-table thead th\s*\{([^}]*)\}`)
	sm := stickyRe.FindStringSubmatch(css)
	if sm == nil || !strings.Contains(sm[1], "position: sticky") {
		t.Error("input.css must still declare position: sticky on .c4-table thead th")
	}
}

// ---- CSS declaration matchers for the scroll-boundary contract -------------
//
// The first version of the contract above hard-coded the shipped rule's exact
// spelling: max-height had to start with a digit and overflow had to be
// literally `auto`. That rejected behaviour-equivalent CSS (`calc(...)`,
// `var(...)`, `scroll`) purely on syntax, so the helpers below match on what
// the declaration *means* for the sticky header instead - while staying
// anchored to complete property declarations, so `not-max-height`,
// `overflow-x` and commented-out declarations can never satisfy them.

// stripCSSComments removes /* ... */ blocks so a commented-out declaration
// never satisfies a contract, and a `}` inside a comment never truncates a
// rule body.
func stripCSSComments(css string) string {
	return regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, " ")
}

// cssDeclValue returns the trimmed value of the prop declaration in a rule
// body. The leading boundary group is what keeps the match anchored to a whole
// property name: Go's regexp has no lookbehind, so the character before prop is
// captured and required to be a non-identifier - that is what makes
// `not-max-height` fail a `max-height` query, and `overflow-x` fail an
// `overflow` query (there, `-x` simply is not the `:` the pattern demands).
func cssDeclValue(rule, prop string) (string, bool) {
	re := regexp.MustCompile(`(?i)(?:^|[^0-9A-Za-z_-])` + regexp.QuoteMeta(prop) + `\s*:([^;}]*)`)
	m := re.FindStringSubmatch(rule)
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

// cssHasBoundedMaxHeight reports whether the rule bounds its height at all.
// Any non-empty value counts - the sticky header only needs *a* bound, not a
// particular unit - but a present-yet-empty declaration does not.
func cssHasBoundedMaxHeight(rule string) bool {
	v, ok := cssDeclValue(rule, "max-height")
	return ok && v != ""
}

// cssHasScrollingOverflow reports whether the rule actually scrolls on the
// vertical axis. `auto` and `scroll` both create a scroll container; `hidden`
// and `visible` do not, so they must be rejected rather than merely unmatched.
func cssHasScrollingOverflow(rule string) bool {
	for _, prop := range []string{"overflow", "overflow-y"} {
		v, ok := cssDeclValue(rule, prop)
		if !ok {
			continue
		}
		switch strings.ToLower(v) {
		case "auto", "scroll":
			return true
		}
	}
	return false
}

// TestS5_3CSSMaxHeightMatcherAcceptsAnyRealBound pins the max-height matcher's
// accept/reject matrix directly, so a future tightening or loosening of the
// scroll-boundary contract fails here rather than silently changing what the
// shipped CSS is allowed to say.
func TestS5_3CSSMaxHeightMatcherAcceptsAnyRealBound(t *testing.T) {
	cases := []struct {
		name string
		rule string
		want bool
	}{
		{"shipped viewport unit", `overflow: auto; max-height: 72vh; background: red;`, true},
		{"calc expression", `overflow: auto; max-height: calc(100vh - 20rem);`, true},
		{"custom property", `overflow: auto; max-height: var(--queue-max-height);`, true},
		{"plain pixels", `max-height: 400px;`, true},
		{"empty value", `overflow: auto; max-height:;`, false},
		{"whitespace-only value", `overflow: auto; max-height:   ;`, false},
		{"declaration missing", `overflow: auto; background: red;`, false},
		{"different property with matching suffix", `overflow: auto; not-max-height: 72vh;`, false},
		{"commented out", stripCSSComments(`overflow: auto; /* max-height: 72vh; */`), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cssHasBoundedMaxHeight(c.rule); got != c.want {
				t.Errorf("cssHasBoundedMaxHeight(%q) = %v, want %v", c.rule, got, c.want)
			}
		})
	}
}

// TestS5_3CSSOverflowMatcherAcceptsOnlyScrollingValues pins the overflow half
// of the same contract: both scrolling values on both axes-properties are
// accepted, and every non-scrolling or off-axis declaration is rejected.
func TestS5_3CSSOverflowMatcherAcceptsOnlyScrollingValues(t *testing.T) {
	cases := []struct {
		name string
		rule string
		want bool
	}{
		{"overflow auto", `max-height: 72vh; overflow: auto;`, true},
		{"overflow scroll", `max-height: 72vh; overflow: scroll;`, true},
		{"overflow-y auto", `max-height: 72vh; overflow-y: auto;`, true},
		{"overflow-y scroll", `max-height: 72vh; overflow-y: scroll;`, true},
		{"overflow hidden", `max-height: 72vh; overflow: hidden;`, false},
		{"overflow-y hidden", `max-height: 72vh; overflow-y: hidden;`, false},
		{"overflow visible", `max-height: 72vh; overflow: visible;`, false},
		{"overflow-y visible", `max-height: 72vh; overflow-y: visible;`, false},
		{"empty value", `max-height: 72vh; overflow:;`, false},
		{"declaration missing", `max-height: 72vh; background: red;`, false},
		{"horizontal axis only", `max-height: 72vh; overflow-x: auto;`, false},
		{"commented out", stripCSSComments(`max-height: 72vh; /* overflow: auto; */`), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cssHasScrollingOverflow(c.rule); got != c.want {
				t.Errorf("cssHasScrollingOverflow(%q) = %v, want %v", c.rule, got, c.want)
			}
		})
	}
}

// TestS5_3QueueCountsFromWhicheverRepresentationExists is a source-contract
// test (no DOM/browser harness exists in this repository - task Phase 3 item
// G) pinning the actual counting mechanism: applyFilter must count from
// whichever of tableBody/cardsList actually exists, never hardcode
// "container === tableBody" (CodeRabbit PR152 finding: on a cards-only
// render, that guard left total/shown at 0 forever, reporting "0 of 0" and
// suppressing the no-results note even when a filter matched nothing).
func TestS5_3QueueCountsFromWhicheverRepresentationExists(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview/queue", "en")

	if !strings.Contains(body, "var countFrom = tableBody || cardsList;") {
		t.Error("queue.html must count from whichever representation exists: var countFrom = tableBody || cardsList;")
	}
	if strings.Contains(body, "var isTable = container === tableBody;") {
		t.Error("queue.html must not gate counting on container === tableBody alone (the cards-only bug)")
	}
	if !strings.Contains(body, "var counts = container === countFrom;") {
		t.Error("queue.html must gate counting on container === countFrom, not a hardcoded tableBody comparison")
	}
}

// TestS5_3AriaSortOwnedByHeaderNeverByButton proves aria-sort lives only on
// the owning <th scope="col"> (the ARIA-correct columnheader owner), never
// on the nested sort <button> (CodeRabbit PR152 finding: aria-sort on a
// button is invisible to assistive technology, which only recognizes it on
// columnheader/rowheader roles). The button remains the activation control.
func TestS5_3AriaSortOwnedByHeaderNeverByButton(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview/queue", "en")

	buttonAriaSortRe := regexp.MustCompile(`<button[^>]*aria-sort[^>]*>`)
	if m := buttonAriaSortRe.FindString(body); m != "" {
		t.Errorf("a sort <button> must never carry aria-sort itself, found: %s", m)
	}

	thAriaSortRe := regexp.MustCompile(`<th[^>]*aria-sort="[^"]*"[^>]*>`)
	tags := thAriaSortRe.FindAllString(body, -1)
	if len(tags) < 3 {
		t.Fatalf("expected aria-sort on at least the 3 sortable <th> headers (channel/points/today), found %d: %v", len(tags), tags)
	}
	for _, tag := range tags {
		if !strings.Contains(tag, `scope="col"`) {
			t.Errorf("every aria-sort-carrying header must be a <th scope=\"col\">, got: %s", tag)
		}
	}

	// Every aria-sort occurrence anywhere on the page belongs to a <th> -
	// the total count found on <th> must equal the total count on the page.
	totalAriaSort := strings.Count(body, "aria-sort=")
	if totalAriaSort != len(tags) {
		t.Errorf("aria-sort appears %d times on the page but only %d are on <th> elements - some occurrence escaped the header", totalAriaSort, len(tags))
	}
}

// ---- Phase 7/10 item 18: C3 status/evidence/metadata ordering --------------

func TestS5_3C3CardOrderingStatusEvidenceMetadata(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview/queue", "en")

	start := strings.Index(body, `id="qr-cards"`)
	if start < 0 {
		t.Fatal("queue page missing #qr-cards")
	}
	firstCardStart := strings.Index(body[start:], `class="c3-list-card"`)
	if firstCardStart < 0 {
		t.Fatal("no rendered .c3-list-card found")
	}
	region := body[start+firstCardStart:]
	end := strings.Index(region, "</li>")
	if end < 0 {
		t.Fatal("first .c3-list-card is not closed with </li>")
	}
	card := region[:end]

	statusIdx := strings.Index(card, "c3-list-card-status")
	evidenceIdx := strings.Index(card, "c3-list-card-evidence")
	metadataIdx := strings.Index(card, "c3-list-card-metadata")
	if statusIdx < 0 || evidenceIdx < 0 || metadataIdx < 0 {
		t.Fatalf("card missing one of status/evidence/metadata sections: %s", card)
	}
	if statusIdx >= evidenceIdx || evidenceIdx >= metadataIdx {
		t.Errorf("card sections out of order (want status < evidence < metadata): status=%d evidence=%d metadata=%d\n%s", statusIdx, evidenceIdx, metadataIdx, card)
	}
}

// TestS5_3C3NoFieldSilentlyDisappears proves the SAME channel/reason/points
// evidence visible in the C4 table is also present in the C3 cards - one
// logical data source, nothing dropped in the responsive transform.
func TestS5_3C3NoFieldSilentlyDisappears(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview/queue", "en")

	tableStart := strings.Index(body, `id="qr-table-body"`)
	cardsStart := strings.Index(body, `id="qr-cards"`)
	if tableStart < 0 || cardsStart < 0 {
		t.Fatal("queue page missing #qr-table-body or #qr-cards")
	}
	table := body[tableStart:cardsStart]
	cards := body[cardsStart:]

	for _, channel := range []string{"streamer_a", "streamer_b"} {
		if !strings.Contains(table, channel) {
			t.Errorf("table missing channel %q", channel)
		}
		if !strings.Contains(cards, channel) {
			t.Errorf("cards missing channel %q - field silently disappeared in the responsive transform", channel)
		}
	}
}

// ---- Phase 7/10 item 19: filter/sort state shared across representations --

func TestS5_3FilterSortStateSharedAcrossRepresentations(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview/queue", "en")

	// Exactly one filter/sort toolbar element (not duplicated per
	// representation) - anchored on the actual element's opening tag, not
	// the bare attribute name, which also appears in the JS selector string
	// below.
	if n := strings.Count(body, `<div class="ov-toolbar" data-qr-toolbar`); n != 1 {
		t.Errorf(`toolbar element count = %d, want exactly 1 (one shared toolbar)`, n)
	}
	// Both the table rows and the cards carry the SAME data-qr-channel/
	// data-qr-status markers the shared script filters/sorts on.
	if !strings.Contains(body, `data-qr-channel="streamer_a"`) {
		t.Error("rows must carry data-qr-channel for the shared filter/sort script")
	}
	// The script is a single-instance guarded IIFE operating on both
	// containers in the same pass (source-literal pin, since this is
	// client-side behavior no Go test can execute).
	for _, want := range []string{
		"window.__queueDelta", "if (window.__queueDelta) return;",
		"[tableBody, cardsList].forEach(",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("queue page missing shared filter/sort literal %q", want)
		}
	}
}

// ---- Phase 8/10 items 20-21: C18 exactly once, five controls absent -------

func TestS5_3C18AppearsExactlyOnceOnQueuePageOnly(t *testing.T) {
	srv := buildF3PageServer(t)

	queueBody := f3GetPage(t, srv, "/overview/queue", "en")
	if n := strings.Count(queueBody, `class="c18-dpba"`); n != 1 {
		t.Errorf("/overview/queue: c18-dpba count = %d, want exactly 1", n)
	}

	for _, page := range []string{"/overview", "/drops", "/statistics", "/health", "/logs", "/settings", "/notifications"} {
		body := f3GetPage(t, srv, page, "en")
		if strings.Contains(body, "c18-dpba") {
			t.Errorf("%s must never render the C18 card - it is queue-page-exclusive", page)
		}
	}
}

func TestS5_3DPBAFiveControlsAbsentNoButtonsNoMenus(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview/queue", "en")

	start := strings.Index(body, `class="c18-dpba"`)
	if start < 0 {
		t.Fatal("queue page missing the C18 card")
	}
	end := strings.Index(body[start:], "</div>")
	if end < 0 {
		t.Fatal("C18 card is not closed with </div>")
	}
	card := body[start : start+end]

	if strings.Contains(card, "<button") || strings.Contains(card, "<select") || strings.Contains(card, "<a ") {
		t.Errorf("C18 card must contain no buttons/menus/links, got: %s", card)
	}
	if strings.Contains(card, "disabled") {
		t.Errorf("C18 card must contain no disabled ghost controls, got: %s", card)
	}
	if !strings.Contains(card, enTR(t)("queue.dpba.text")) {
		t.Error("C18 card must state the deferred-pending-broker-audit text")
	}

	// The passive text names all five deferred controls (task Phase 8); the
	// card itself implements none of them.
	for _, ctrl := range []string{"favorite", "slot switching", "harvest", "override", "reorder"} {
		if !strings.Contains(strings.ToLower(card), ctrl) {
			t.Errorf("C18 text must name the deferred control %q, got: %s", ctrl, card)
		}
	}

	// The troubleshooting link is intentionally omitted (deferred route).
	if strings.Contains(card, "/help/troubleshooting") {
		t.Error("C18 must not link to the still-deferred /help/troubleshooting route")
	}
}
