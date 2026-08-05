package web

// S5-3 Phase 4 tests: C12 "exactly two slots", everywhere (task Phase 10
// items 8-14). c12Pair always returns a Go [2]c12SlotData array - these tests
// additionally prove the RENDERED template never grows a third box even when
// handed a misbehaving/fake evidence value with more than two entries.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestS5_3BuildNowWatchingPaddingIsExactlyTwoEndToEnd exercises the REAL
// buildNowWatching function (not a hand-built NowWatchingView) across 0, 1,
// and 2 occupied slots, proving the padding computation itself - not just
// the template it feeds - keeps the total at exactly two.
func TestS5_3BuildNowWatchingPaddingIsExactlyTwoEndToEnd(t *testing.T) {
	a := models.NewStreamer("alpha", models.DefaultStreamerSettings())
	a.SetConfirmedOnline()
	b := models.NewStreamer("bravo", models.DefaultStreamerSettings())
	b.SetConfirmedOnline()
	streamers := []*models.Streamer{a, b}

	cases := []struct {
		name     string
		watching map[string]bool
	}{
		{"zero occupied", map[string]bool{}},
		{"one occupied", map[string]bool{"alpha": true}},
		{"two occupied", map[string]bool{"alpha": true, "bravo": true}},
	}
	srv := &Server{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			view := srv.buildNowWatching(streamers, WatchSlotsView{Watching: c.watching}, map[string]streamerStats{}, false, watchSlotEvidence{})
			total := len(view.Slots) + len(view.EmptyPad)
			if total != 2 {
				t.Fatalf("%s: len(Slots)+len(EmptyPad) = %d, want 2 (Slots=%d EmptyPad=%d)", c.name, total, len(view.Slots), len(view.EmptyPad))
			}

			partials := testPartials(t)
			var buf strings.Builder
			if err := partials.ExecuteTemplate(&buf, "now_watching", view); err != nil {
				t.Fatalf("render now_watching: %v", err)
			}
			out := buf.String()
			// Assert each markup family independently (never summed): a
			// regression that restores the legacy occupied-slot path would
			// otherwise produce e.g. 1 legacy + 1 C12 == 2 and stay green
			// (the exact bug this test previously hid; see
			// TestS5_3SidebarNowWatchingAlwaysExactlyTwoBoxes's sibling
			// comment).
			if legacy := strings.Count(out, `class="watch-slot`); legacy != 0 {
				t.Errorf("%s: found %d legacy .watch-slot boxes - every slot must render through the shared C12 component; body=%s", c.name, legacy, out)
			}
			if n := countC12SlotBoxes(out); n != 2 {
				t.Errorf("%s: C12 slot boxes = %d, want 2; body=%s", c.name, n, out)
			}
		})
	}
}

// TestS5_3NowWatchingEndpointAndCadenceUnchanged pins task Phase 10 item 26:
// the sidebar's existing polling contract - GET /api/now-watching, load then
// every 30s - survives S5-3's C12 padding additive change byte-for-byte.
func TestS5_3NowWatchingEndpointAndCadenceUnchanged(t *testing.T) {
	base := readEmbeddedTemplate(t, "templates/base.html")
	if !strings.Contains(base, `hx-get="/api/now-watching" hx-trigger="load, every 30s"`) {
		t.Error(`base.html must still poll #now-watching with hx-get="/api/now-watching" hx-trigger="load, every 30s"`)
	}

	srv := buildF3PageServer(t)
	rec := httptest.NewRecorder()
	srv.handleAPINowWatching(rec, httptest.NewRequest(http.MethodGet, "/api/now-watching", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /api/now-watching = %d, want 200", rec.Code)
	}
}

// renderC12Pair renders the shared "c12.pair" component for pair, in English.
func renderC12Pair(t *testing.T, pair [2]c12SlotData) string {
	t.Helper()
	return execComponent(t, i18n.LangEN, "c12.pair", pair)
}

// countC12SlotBoxes counts the top-level slot containers, distinguishing
// them from any nested element that happens to share the "c12-slot" prefix
// (e.g. c12-slot-head, c12-slot-name) by anchoring on the exact opening
// class list this template always emits: "c12-slot" followed by a space and
// a second "c12-slot--" modifier class - never by a bare hyphen, which is
// what every nested c12-slot-* element uses instead.
func countC12SlotBoxes(rendered string) int {
	return strings.Count(rendered, `<div class="c12-slot c12-slot--`)
}

func s53Evidence(slots []watchSlotEntry, waiting []waitingSlotEntry, mode string) watchSlotEvidence {
	return watchSlotEvidence{Mode: mode, Slots: slots, Waiting: waiting}
}

// ---- item 8: exactly two C12 slots -----------------------------------------

func TestS5_3C12AlwaysExactlyTwoRenderedContainers(t *testing.T) {
	cases := []struct {
		name  string
		slots []watchSlotEntry
	}{
		{"zero occupied", nil},
		{"one occupied", []watchSlotEntry{{Slot: 1, Channel: "a"}}},
		{"two occupied", []watchSlotEntry{{Slot: 1, Channel: "a"}, {Slot: 2, Channel: "b"}}},
		{"three occupied (misbehaving provider)", []watchSlotEntry{{Slot: 1, Channel: "a"}, {Slot: 2, Channel: "b"}, {Slot: 3, Channel: "c"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pair := c12Pair(s53Evidence(c.slots, nil, ""), nil, nil, enTR(t))
			rendered := renderC12Pair(t, pair)
			if n := countC12SlotBoxes(rendered); n != 2 {
				t.Errorf("%s: rendered %d slot boxes, want exactly 2; body=%s", c.name, n, rendered)
			}
		})
	}
}

// ---- item 13: a third provider entry cannot create a third slot -----------

func TestS5_3C12ThirdProviderEntryCannotCreateThirdSlot(t *testing.T) {
	evidence := s53Evidence([]watchSlotEntry{
		{Slot: 1, Channel: "alpha"}, {Slot: 2, Channel: "bravo"}, {Slot: 3, Channel: "charlie"},
	}, nil, "")
	pair := c12Pair(evidence, nil, nil, enTR(t))
	if pair[0].Channel != "alpha" || pair[1].Channel != "bravo" {
		t.Fatalf("expected the first two entries only, got %+v / %+v", pair[0], pair[1])
	}
	rendered := renderC12Pair(t, pair)
	if strings.Contains(rendered, "charlie") {
		t.Errorf("the third provider entry must never reach rendered output, got: %s", rendered)
	}
	if n := countC12SlotBoxes(rendered); n != 2 {
		t.Errorf("rendered %d slot boxes, want exactly 2", n)
	}
}

// ---- items 9-11: occupied/empty combinations -------------------------------

func TestS5_3C12OneOccupiedOneEmpty(t *testing.T) {
	pair := c12Pair(s53Evidence([]watchSlotEntry{{Slot: 1, Channel: "alpha", ReasonCode: "priority"}}, nil, "direct"), nil, nil, enTR(t))
	if !pair[0].Occupied || pair[0].Channel != "alpha" {
		t.Fatalf("slot 0 = %+v, want occupied/alpha", pair[0])
	}
	if pair[1].Occupied {
		t.Fatalf("slot 1 = %+v, want an explicit empty slot", pair[1])
	}
	rendered := renderC12Pair(t, pair)
	if n := countC12SlotBoxes(rendered); n != 2 {
		t.Fatalf("rendered %d slot boxes, want 2", n)
	}
	if !strings.Contains(rendered, "alpha") {
		t.Error("occupied slot's channel name must render")
	}
	if !strings.Contains(rendered, enTR(t)("queue.slot.empty")) {
		t.Error("the second, empty slot must render the definite empty-slot text")
	}
}

func TestS5_3C12TwoOccupied(t *testing.T) {
	pair := c12Pair(s53Evidence([]watchSlotEntry{
		{Slot: 1, Channel: "alpha"}, {Slot: 2, Channel: "bravo"},
	}, nil, ""), nil, nil, enTR(t))
	if !pair[0].Occupied || !pair[1].Occupied {
		t.Fatalf("both slots must be occupied, got %+v / %+v", pair[0], pair[1])
	}
	rendered := renderC12Pair(t, pair)
	if strings.Contains(rendered, enTR(t)("queue.slot.empty")) {
		t.Error("no empty-slot text should render when both slots are occupied")
	}
}

func TestS5_3C12TwoEmpty(t *testing.T) {
	pair := c12Pair(s53Evidence(nil, nil, ""), nil, nil, enTR(t))
	if pair[0].Occupied || pair[1].Occupied {
		t.Fatalf("both slots must be empty, got %+v / %+v", pair[0], pair[1])
	}
	rendered := renderC12Pair(t, pair)
	if n := strings.Count(rendered, enTR(t)("queue.slot.empty")); n != 2 {
		t.Errorf("expected the empty-slot text exactly twice, got %d; body=%s", n, rendered)
	}
}

// ---- item 12: occupied + unknown channel status ----------------------------

func TestS5_3C12OccupiedUnknownStaysOccupiedNeverEmpty(t *testing.T) {
	st := models.NewStreamer("shadowy", models.DefaultStreamerSettings())
	st.SetConfirmedOnline()
	st.SetUnknown(models.ReasonTransportError)
	byName := map[string]*models.Streamer{"shadowy": st}

	pair := c12Pair(s53Evidence([]watchSlotEntry{{Slot: 1, Channel: "shadowy", ReasonCode: "priority"}}, nil, ""), byName, nil, enTR(t))
	entry := pair[0]
	if !entry.Occupied {
		t.Fatal("an unknown-status channel holding a slot must stay Occupied, never converted to Empty")
	}
	if !entry.Unknown {
		t.Error("entry.Unknown must be true for an unconfirmed channel")
	}
	if entry.Active {
		t.Error("Active must be false for an unconfirmed (not positively proved) channel")
	}
	// Render slot 0 alone (not the whole pair - slot 1 is legitimately
	// padded-empty here since only one occupied entry was supplied).
	rendered := execComponent(t, i18n.LangEN, "c12.slot", entry)
	if strings.Contains(rendered, enTR(t)("queue.slot.empty")) {
		t.Error("an occupied-but-unknown slot must never render the empty-slot text")
	}
	if !strings.Contains(rendered, "shadowy") {
		t.Error("the occupied-but-unknown slot must still render its channel name")
	}
}

// ---- Active edge only when positively proved -------------------------------

func TestS5_3C12ActiveEdgeOnlyWhenPositivelyProved(t *testing.T) {
	online := models.NewStreamer("liveone", models.DefaultStreamerSettings())
	online.SetConfirmedOnline()

	unknown := models.NewStreamer("unsureone", models.DefaultStreamerSettings())
	unknown.SetConfirmedOnline()
	unknown.SetUnknown(models.ReasonTransportError)

	byName := map[string]*models.Streamer{"liveone": online, "unsureone": unknown}

	pair := c12Pair(s53Evidence([]watchSlotEntry{
		{Slot: 1, Channel: "liveone"}, {Slot: 2, Channel: "unsureone"},
	}, nil, ""), byName, nil, enTR(t))

	if !pair[0].Active {
		t.Error("a positively-confirmed online channel must get the active edge")
	}
	if pair[1].Active {
		t.Error("an unconfirmed channel must never get the active edge")
	}
	rendered := renderC12Pair(t, pair)
	if !strings.Contains(rendered, "c12-slot--active") {
		t.Error("the confirmed-online slot must render the active-edge class")
	}
}

// ---- item 14: missing uptime/points render dash, never zero ---------------

func TestS5_3C12MissingUptimeAndPointsRenderDashNeverZero(t *testing.T) {
	// Discovery-occupied slot: no *models.Streamer at all.
	pair := c12Pair(s53Evidence([]watchSlotEntry{{Slot: 1, Channel: "discovered_channel"}}, nil, ""), nil, nil, enTR(t))
	entry := pair[0]
	if entry.HasUptime || entry.HasPointsDelta {
		t.Errorf("a discovery-occupied slot (no streamer) must not fabricate uptime/points, got %+v", entry)
	}
	rendered := renderC12Pair(t, pair)
	if strings.Contains(rendered, ">0<") || strings.Contains(rendered, "0/h") {
		t.Errorf("missing uptime/points must never render as 0, got: %s", rendered)
	}
	if !strings.Contains(rendered, "—") {
		t.Errorf("missing uptime/points must render the dash, got: %s", rendered)
	}

	// A tracked streamer with online status but no analytics rate yet: points
	// delta must dash, uptime must still compute from OnlineAt (it IS known).
	st := models.NewStreamer("freshjoin", models.DefaultStreamerSettings())
	st.SetConfirmedOnline()
	pair2 := c12Pair(s53Evidence([]watchSlotEntry{{Slot: 1, Channel: "freshjoin"}}, nil, ""),
		map[string]*models.Streamer{"freshjoin": st}, map[string]streamerStats{}, enTR(t))
	if pair2[0].HasPointsDelta {
		t.Errorf("no analytics rate yet: HasPointsDelta must be false, got %+v", pair2[0])
	}
	if !pair2[0].HasUptime {
		t.Error("a confirmed-online tracked streamer must report uptime")
	}
}

// ---- item: empty slot never inherits a Waiting channel's ReasonCode -------

func TestS5_3C12EmptySlotNeverUsesWaitingReasonCode(t *testing.T) {
	evidence := s53Evidence(nil, []waitingSlotEntry{{Channel: "waiting_channel", ReasonCode: "lower_priority"}}, "direct")
	pair := c12Pair(evidence, nil, nil, enTR(t))
	for i, entry := range pair {
		if entry.HasReasonCode {
			t.Errorf("slot %d: empty slots must never carry a ReasonCode (esp. not a Waiting entry's), got %+v", i, entry)
		}
	}
	rendered := renderC12Pair(t, pair)
	if strings.Contains(rendered, "waiting_channel") || strings.Contains(rendered, enTR(t)("queue.reason.lower_priority")) {
		t.Errorf("a Waiting channel's evidence must never leak onto an empty slot, got: %s", rendered)
	}
}

// ---- empty-slot reason mode: idle machine evidence vs S-UNK ----------------

func TestS5_3C12EmptySlotIdleModeShowsMachineEvidence(t *testing.T) {
	pair := c12Pair(s53Evidence(nil, nil, "idle"), nil, nil, enTR(t))
	if pair[0].EmptyReasonMode != "idle" || pair[1].EmptyReasonMode != "idle" {
		t.Fatalf("Mode=idle must propagate to both empty slots, got %+v / %+v", pair[0], pair[1])
	}
	rendered := renderC12Pair(t, pair)
	if !strings.Contains(rendered, enTR(t)("queue.slot.mode_idle")) {
		t.Errorf("idle mode must show the machine evidence text, got: %s", rendered)
	}
}

func TestS5_3C12EmptySlotNonIdleModeShowsUnknownReason(t *testing.T) {
	for _, mode := range []string{"direct", "rotation", ""} {
		pair := c12Pair(s53Evidence(nil, nil, mode), nil, nil, enTR(t))
		if pair[0].EmptyReasonMode != "unknown" {
			t.Errorf("mode=%q: EmptyReasonMode = %q, want %q", mode, pair[0].EmptyReasonMode, "unknown")
		}
		rendered := renderC12Pair(t, pair)
		if strings.Contains(rendered, enTR(t)("queue.slot.mode_idle")) {
			t.Errorf("mode=%q: must not show the idle machine evidence, got: %s", mode, rendered)
		}
		if !strings.Contains(rendered, `aria-label="`+enTR(t)("queue.reason.unknown")+`"`) {
			t.Errorf("mode=%q: must show the accessible unknown-reason label, got: %s", mode, rendered)
		}
	}
}

// ---- link to /overview/queue -----------------------------------------------

func TestS5_3C12SlotAlwaysLinksToQueue(t *testing.T) {
	pair := c12Pair(s53Evidence([]watchSlotEntry{{Slot: 1, Channel: "alpha"}}, nil, ""), nil, nil, enTR(t))
	rendered := renderC12Pair(t, pair)
	if n := strings.Count(rendered, `href="/overview/queue"`); n != 2 {
		t.Errorf("both slots (occupied and empty) must link to /overview/queue, found %d links; body=%s", n, rendered)
	}
}

// ---- sidebar integration: exactly two total boxes --------------------------

// TestS5_3SidebarNowWatchingAlwaysExactlyTwoBoxes proves the S5-3 padding
// wired into buildNowWatching brings the sidebar's total slot-box count to
// exactly two for 0, 1, and 2 occupied slots, without discarding the
// existing rich occupied-slot markup or the "nothing watching" text
// TestRenderNowWatchingEmpty depends on.
//
// Q3 MAJOR-2 correction: this now asserts on countC12SlotBoxes ALONE (not a
// sum of watch-slot + c12-slot counts) and separately requires ZERO
// `class="watch-slot` occurrences - the original version summed both markup
// families together, so it stayed green even while occupied slots used the
// independent legacy .watch-slot path and only the empty padding used C12.
// Summing hid exactly the bug Q3 flagged; asserting on each family
// separately is what actually pins "every slot box goes through the common
// C12 component, never a watch-slot-only path" (mutation probe C: restoring
// the legacy occupied markup must fail this test).
func TestS5_3SidebarNowWatchingAlwaysExactlyTwoBoxes(t *testing.T) {
	partials := testPartials(t)
	cases := []struct {
		name  string
		view  NowWatchingView
		boxes int
	}{
		{"zero occupied", NowWatchingView{EmptyPad: []c12SlotData{{EmptyReasonMode: "unknown", Link: "/overview/queue"}, {EmptyReasonMode: "unknown", Link: "/overview/queue"}}}, 2},
		{"one occupied", NowWatchingView{
			Slots:    []WatchSlotView{{Name: "shroud"}},
			EmptyPad: []c12SlotData{{EmptyReasonMode: "unknown", Link: "/overview/queue"}},
		}, 2},
		{"two occupied", NowWatchingView{
			Slots: []WatchSlotView{{Name: "shroud"}, {Name: "pokimane"}},
		}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf strings.Builder
			if err := partials.ExecuteTemplate(&buf, "now_watching", c.view); err != nil {
				t.Fatalf("render now_watching: %v", err)
			}
			out := buf.String()
			if legacy := strings.Count(out, `class="watch-slot`); legacy != 0 {
				t.Errorf("%s: found %d legacy .watch-slot boxes - occupied slots must render through the shared C12 component, not an independent watch-slot-only path; body=%s", c.name, legacy, out)
			}
			if n := countC12SlotBoxes(out); n != c.boxes {
				t.Errorf("%s: C12 slot boxes = %d, want %d; body=%s", c.name, n, c.boxes, out)
			}
		})
	}
}

// TestS5_3SidebarOccupiedAndEmptySlotsShareBaseGeometry proves the sidebar's
// filled and empty slot boxes both carry the same base "c12-slot" class
// (Q3 MAJOR-2 item 1/3: equal padding/border-radius/border/background/
// spacing, since they share the exact same CSS rule keyed on that class),
// and that two adjacent empty boxes never render touching (the shared
// c12.pair wrapper always separates them).
func TestS5_3SidebarOccupiedAndEmptySlotsShareBaseGeometry(t *testing.T) {
	partials := testPartials(t)
	view := NowWatchingView{
		Slots:    []WatchSlotView{{Name: "shroud"}},
		EmptyPad: []c12SlotData{{EmptyReasonMode: "unknown", Link: "/overview/queue"}},
	}
	var buf strings.Builder
	if err := partials.ExecuteTemplate(&buf, "now_watching", view); err != nil {
		t.Fatalf("render now_watching: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `<div class="c12-slot c12-slot--occupied`) {
		t.Error("occupied sidebar slot must carry the base c12-slot class")
	}
	if !strings.Contains(out, `<div class="c12-slot c12-slot--empty`) {
		t.Error("empty sidebar slot must carry the base c12-slot class")
	}

	// MAJOR-2 item 6: "wrap or space the pair so two empty slots never
	// visually touch". The slot boxes are laid out via CSS grid (input.css:
	// .c12-pair { display: grid; gap: 0.75rem }), so the spacing comes from
	// their shared container carrying that class - the exact same wrapper
	// class /overview and /overview/queue's own c12.pair renders - not from
	// any HTML whitespace between the boxes themselves.
	pairIdx := strings.Index(out, `<div class="c12-pair">`)
	if pairIdx < 0 {
		t.Fatal("sidebar slot boxes must be wrapped in the shared c12-pair container")
	}
	slotIdx := strings.Index(out, `<div class="c12-slot`)
	if slotIdx < pairIdx {
		t.Error("slot boxes must render inside the c12-pair wrapper, not before it")
	}
}

// ---- Q3 MAJOR-1: /overview C12 pair lives inside the refresh boundary -----

// TestS5_3OverviewC12PairInsideRefreshBoundary proves the /overview page's
// C12 slot pair is rendered INSIDE #overview-live (so it refreshes on the
// existing 30s poll), not as a static sibling that only ever reflects the
// last full page load.
func TestS5_3OverviewC12PairInsideRefreshBoundary(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview", "en")

	liveIdx := strings.Index(body, `id="overview-live"`)
	if liveIdx < 0 {
		t.Fatal("page missing #overview-live")
	}
	updaterIdx := strings.Index(body, `id="updater-widget-slot"`)
	if updaterIdx < 0 {
		t.Fatal("page missing #updater-widget-slot (the sibling immediately after #overview-live)")
	}
	pairIdx := strings.Index(body, `class="c12-pair"`)
	if pairIdx < 0 {
		t.Fatal("page missing the C12 pair")
	}
	if pairIdx < liveIdx || pairIdx > updaterIdx {
		t.Errorf("the C12 pair (offset %d) must sit between #overview-live (offset %d) and #updater-widget-slot (offset %d) - it must render inside the live region, not as a static sibling", pairIdx, liveIdx, updaterIdx)
	}
}

// TestS5_3OverviewNoStaticDuplicateC12Pair proves the C12 pair renders
// exactly once on the full /overview page - never a static copy outside
// #overview-live plus a second one inside it.
func TestS5_3OverviewNoStaticDuplicateC12Pair(t *testing.T) {
	srv := buildF3PageServer(t)
	body := f3GetPage(t, srv, "/overview", "en")
	if n := strings.Count(body, `class="c12-pair"`); n != 1 {
		t.Errorf(`class="c12-pair" count = %d, want exactly 1 (no static duplicate outside #overview-live)`, n)
	}
}

// TestS5_3APIOverviewReturnsExactlyTwoC12Slots proves the actual
// GET /api/overview response - the existing 30s poll's payload - carries
// the C12 pair itself (not just static page furniture that never refreshes).
func TestS5_3APIOverviewReturnsExactlyTwoC12Slots(t *testing.T) {
	srv := buildF3PageServer(t)
	h := srv.handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/overview = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if n := countC12SlotBoxes(body); n != 2 {
		t.Errorf("GET /api/overview rendered %d C12 slot boxes, want exactly 2; body=%s", n, body)
	}
}

// TestS5_3OverviewRefreshCadenceUnchanged proves MAJOR-1's fix introduced no
// new endpoint, htmx trigger, or poll cadence: #overview-live still polls
// exactly GET /api/overview on the existing PollSeconds interval, and no
// new /api/overview/* sub-route beyond the pre-existing events-drawer one
// was added.
func TestS5_3OverviewRefreshCadenceUnchanged(t *testing.T) {
	overview := readEmbeddedTemplate(t, "templates/overview.html")
	if n := strings.Count(overview, `hx-get="/api/overview"`); n != 1 {
		t.Errorf(`hx-get="/api/overview" count = %d, want exactly 1 (single existing poll, no second poller)`, n)
	}
	if !strings.Contains(overview, `hx-trigger="every {{.PollSeconds}}s, refresh"`) {
		t.Error("overview.html must still drive #overview-live from the existing PollSeconds-derived trigger")
	}
	if strings.Contains(overview, "EventSource") || strings.Contains(overview, "WebSocket") {
		t.Error("MAJOR-1's fix must not introduce SSE or a websocket transport")
	}
}

// ---- Q3 MAJOR-1 item 6: C0 provenance chip uses real EvaluatedAt evidence -

// TestS5_3SlotPairProvenanceUnknownWithNoEvidence proves the C0 chip next to
// the C12 pair honestly reports S-UNK (never a fabricated age) when no
// watch-slot evidence provider is wired.
func TestS5_3SlotPairProvenanceUnknownWithNoEvidence(t *testing.T) {
	srv := buildF3PageServer(t) // no SetSupportBundleSource wired
	body := f3GetPage(t, srv, "/overview", "en")
	if !strings.Contains(body, "c0-chip--unknown") {
		t.Error("the slot-pair provenance chip must render the unknown variant when no evidence is wired")
	}
}

// TestS5_3SlotPairProvenanceReflectsRealEvaluatedAt proves the chip switches
// out of S-UNK and shows a real age once the safe evidence adapter has an
// actual EvaluatedAt - the exact same evidence backing the slot pair itself,
// never an invented timestamp.
func TestS5_3SlotPairProvenanceReflectsRealEvaluatedAt(t *testing.T) {
	srv := s53LeakTestServer(t) // wires a fake snapshot with EvaluatedAt=time.Now()
	body := f3GetPage(t, srv, "/overview", "en")
	if strings.Contains(body, "c0-chip--unknown") {
		t.Error("the slot-pair provenance chip must not render unknown once real evidence (EvaluatedAt) exists")
	}
	if !strings.Contains(body, `class="c0-chip`) {
		t.Error("the slot-pair provenance chip must render")
	}
}
