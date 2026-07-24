package watcher

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/journal"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// --- deterministic clock seam for slot-journal tests ---

type jClock struct {
	mu sync.Mutex
	t  time.Time
}

func newJClock() *jClock {
	return &jClock{t: time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)}
}
func (c *jClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *jClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// journaledWatcher builds a bare watcher wired with a manual-clock slot journal,
// ready to have journalSlotTransitions / recordSlotDelivery driven directly.
func journaledWatcher(clk *jClock) *MinuteWatcher {
	w := &MinuteWatcher{}
	w.SetSlotJournal(journal.New[journal.SlotEvent](64, clk.now))
	return w
}

func jStreamer(login, id string) *models.Streamer {
	s := models.NewStreamer(login, models.DefaultStreamerSettings())
	s.ChannelID = id
	s.SetConfirmedOnline()
	s.OnlineAt = time.Now().Add(-time.Minute)
	s.SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)
	return s
}

func occ(s *models.Streamer, idx int, origin, reasonCode string) slotOccupant {
	return slotOccupant{streamer: s, origin: origin, idx: idx, reasonCode: reasonCode, reason: reasonCode}
}

func slotEvents(w *MinuteWatcher) []journal.SlotEvent {
	recs := w.SlotJournalSnapshot()
	out := make([]journal.SlotEvent, len(recs))
	for i, r := range recs {
		out[i] = r.Event
	}
	return out
}

func onlyOfType(evs []journal.SlotEvent, t journal.SlotEventType) []journal.SlotEvent {
	var out []journal.SlotEvent
	for _, e := range evs {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

// T1 — initial assignment: a slot entered once with correct reason, identities
// and enteredAt.
func TestJournalInitialAssignment(t *testing.T) {
	clk := newJClock()
	w := journaledWatcher(clk)
	s := jStreamer("alice", "id-alice")

	w.journalSlotTransitions([]slotOccupant{occ(s, 0, OriginConfigured, "priority")})

	entered := onlyOfType(slotEvents(w), journal.SlotEntered)
	if len(entered) != 1 {
		t.Fatalf("expected exactly one entered event, got %d (%+v)", len(entered), slotEvents(w))
	}
	e := entered[0]
	if e.Channel != "alice" || e.ChannelID != "id-alice" || e.Origin != OriginConfigured || e.Reason != "priority" {
		t.Fatalf("entered event identities/reason wrong: %+v", e)
	}
	// enteredAt is captured on the journal clock, and the residence records it.
	if got := w.slotResidence["alice"]; got == nil || !got.enteredAt.Equal(clk.now()) {
		t.Fatalf("residence enteredAt not stamped from journal clock: %+v", got)
	}
}

// T2 — repeated same assignment: no duplicate entered event.
func TestJournalRepeatedAssignmentNoDuplicate(t *testing.T) {
	clk := newJClock()
	w := journaledWatcher(clk)
	s := jStreamer("alice", "id-alice")
	slots := []slotOccupant{occ(s, 0, OriginConfigured, "priority")}

	w.journalSlotTransitions(slots)
	clk.advance(time.Minute)
	w.journalSlotTransitions(slots) // steady state
	clk.advance(time.Minute)
	w.journalSlotTransitions(slots)

	if n := len(onlyOfType(slotEvents(w), journal.SlotEntered)); n != 1 {
		t.Fatalf("steady state produced %d entered events, want exactly 1", n)
	}
	if n := len(slotEvents(w)); n != 1 {
		t.Fatalf("steady state produced %d total events, want 1", n)
	}
}

// T3 — reason change: same slot/streamer emits reason_changed, not release+enter.
func TestJournalReasonChange(t *testing.T) {
	clk := newJClock()
	w := journaledWatcher(clk)
	s := jStreamer("alice", "id-alice")

	w.journalSlotTransitions([]slotOccupant{occ(s, 0, OriginConfigured, "priority")})
	clk.advance(time.Minute)
	w.journalSlotTransitions([]slotOccupant{occ(s, 0, OriginConfigured, "streak")})

	evs := slotEvents(w)
	if n := len(onlyOfType(evs, journal.SlotReleased)); n != 0 {
		t.Fatalf("reason change must not emit a release, got %d", n)
	}
	if n := len(onlyOfType(evs, journal.SlotEntered)); n != 1 {
		t.Fatalf("reason change must not re-enter; entered count = %d", n)
	}
	rc := onlyOfType(evs, journal.SlotReasonChanged)
	if len(rc) != 1 {
		t.Fatalf("expected one reason_changed, got %d", len(rc))
	}
	if rc[0].PrevReason != "priority" || rc[0].Reason != "streak" || rc[0].Channel != "alice" {
		t.Fatalf("reason_changed fields wrong: %+v", rc[0])
	}
}

// T4 — normal release: leftAt and residence duration are correct.
func TestJournalNormalRelease(t *testing.T) {
	clk := newJClock()
	w := journaledWatcher(clk)
	s := jStreamer("alice", "id-alice")

	w.journalSlotTransitions([]slotOccupant{occ(s, 0, OriginConfigured, "priority")})
	clk.advance(5 * time.Minute)
	w.journalSlotTransitions(nil) // all slots released

	rel := onlyOfType(slotEvents(w), journal.SlotReleased)
	if len(rel) != 1 {
		t.Fatalf("expected one release, got %d", len(rel))
	}
	if rel[0].Channel != "alice" || rel[0].ResidenceSeconds != 300 {
		t.Fatalf("release residence wrong: %+v (want 300s)", rel[0])
	}
	if _, still := w.slotResidence["alice"]; still {
		t.Fatal("residence not closed after release")
	}
}

// T5 — replacement: victim and replacement are correlated without ambiguous
// ordering (one out + one in this tick == one replaced event).
func TestJournalReplacement(t *testing.T) {
	clk := newJClock()
	w := journaledWatcher(clk)
	a := jStreamer("alice", "id-alice")
	b := jStreamer("bob", "id-bob")

	w.journalSlotTransitions([]slotOccupant{occ(a, 0, OriginConfigured, "priority")})
	clk.advance(2 * time.Minute)
	w.journalSlotTransitions([]slotOccupant{occ(b, 1, OriginConfigured, "priority")})

	evs := slotEvents(w)
	rep := onlyOfType(evs, journal.SlotReplaced)
	if len(rep) != 1 {
		t.Fatalf("expected exactly one replaced event, got %d (%+v)", len(rep), evs)
	}
	if rep[0].Victim != "alice" || rep[0].VictimID != "id-alice" || rep[0].Channel != "bob" {
		t.Fatalf("replacement correlation wrong: %+v", rep[0])
	}
	if rep[0].ResidenceSeconds != 120 {
		t.Fatalf("replaced event should carry the victim's 120s residence, got %v", rep[0].ResidenceSeconds)
	}
	// No standalone released/entered when a swap was correlated.
	if len(onlyOfType(evs, journal.SlotReleased)) != 0 || len(onlyOfType(evs, journal.SlotEntered)) != 1 {
		t.Fatalf("correlated swap should not also emit standalone release; events=%+v", evs)
	}
	// The replacement now owns the slot; exactly one active residence.
	if len(w.slotResidence) != 1 {
		t.Fatalf("expected one active residence after swap, got %d", len(w.slotResidence))
	}
}

// T6 — pair rotation: a two-slot rotation produces a deterministic event
// sequence and never reports more than two active earning slots.
func TestJournalPairRotationDeterministic(t *testing.T) {
	clk := newJClock()
	w := journaledWatcher(clk)
	a := jStreamer("alice", "id-a")
	b := jStreamer("bob", "id-b")
	c := jStreamer("carol", "id-c")
	d := jStreamer("dave", "id-d")

	// Tick 1: pair {alice,bob}. Tick 2: rotate one seat -> {alice,carol}.
	// Tick 3: rotate the other -> {carol,dave}.
	w.journalSlotTransitions([]slotOccupant{occ(a, 0, OriginConfigured, "fair_rotation"), occ(b, 1, OriginConfigured, "fair_rotation")})
	assertMaxTwoActive(t, w)
	clk.advance(time.Minute)
	w.journalSlotTransitions([]slotOccupant{occ(a, 0, OriginConfigured, "fair_rotation"), occ(c, 2, OriginConfigured, "fair_rotation")})
	assertMaxTwoActive(t, w)
	clk.advance(time.Minute)
	w.journalSlotTransitions([]slotOccupant{occ(c, 2, OriginConfigured, "fair_rotation"), occ(d, 3, OriginConfigured, "fair_rotation")})
	assertMaxTwoActive(t, w)

	evs := slotEvents(w)
	// Tick1: two enters (bob, then... sorted: alice, bob). Tick2: one swap
	// (bob->carol). Tick3: one swap (alice->dave). Deterministic given sorting.
	var seq []string
	for _, e := range evs {
		seq = append(seq, string(e.Type)+":"+e.Channel)
	}
	want := []string{
		"entered:alice", "entered:bob",
		"replaced:carol", // bob -> carol
		"replaced:dave",  // alice -> dave
	}
	if strings.Join(seq, ",") != strings.Join(want, ",") {
		t.Fatalf("non-deterministic/incorrect rotation sequence:\n got=%v\nwant=%v", seq, want)
	}
	// Verify victims on the swaps.
	reps := onlyOfType(evs, journal.SlotReplaced)
	if reps[0].Victim != "bob" || reps[1].Victim != "alice" {
		t.Fatalf("swap victims wrong: %q then %q", reps[0].Victim, reps[1].Victim)
	}
}

func assertMaxTwoActive(t *testing.T, w *MinuteWatcher) {
	t.Helper()
	if len(w.slotResidence) > 2 {
		t.Fatalf("more than two active earning slots: %d", len(w.slotResidence))
	}
}

// T7 — continuity reset: a bounded reason is recorded once.
func TestJournalContinuityReset(t *testing.T) {
	clk := newJClock()
	w := journaledWatcher(clk)
	s := jStreamer("alice", "id-alice")

	w.journalContinuityReset(s, "slot_lost")

	cr := onlyOfType(slotEvents(w), journal.SlotContinuityReset)
	if len(cr) != 1 {
		t.Fatalf("expected one continuity_reset, got %d", len(cr))
	}
	if cr[0].ResetReason != "slot_lost" || cr[0].Channel != "alice" || cr[0].ChannelID != "id-alice" {
		t.Fatalf("continuity_reset fields wrong: %+v", cr[0])
	}
}

// T8 — successful minute delivery increments residence success accounting
// without claiming server-side point credit, and journals only the first.
func TestJournalDeliverySuccessAccounting(t *testing.T) {
	clk := newJClock()
	w := journaledWatcher(clk)
	s := jStreamer("alice", "id-alice")
	w.journalSlotTransitions([]slotOccupant{occ(s, 0, OriginConfigured, "priority")})

	w.recordSlotDelivery(s, SendResult{Delivered: true})
	w.recordSlotDelivery(s, SendResult{Delivered: true})
	w.recordSlotDelivery(s, SendResult{Delivered: true})

	if got := w.slotResidence["alice"].successes; got != 3 {
		t.Fatalf("residence successes = %d, want 3", got)
	}
	ds := onlyOfType(slotEvents(w), journal.SlotDeliverySuccess)
	if len(ds) != 1 {
		t.Fatalf("expected exactly one delivery_success event (first only), got %d", len(ds))
	}
	// Release and confirm the terminal event carries the full count, not a
	// fabricated points-credit claim.
	clk.advance(time.Minute)
	w.journalSlotTransitions(nil)
	rel := onlyOfType(slotEvents(w), journal.SlotReleased)[0]
	if rel.Successes != 3 {
		t.Fatalf("released event successes = %d, want 3", rel.Successes)
	}
}

// T9 — delivery failure records a stable stage/reason; raw URL/error body absent.
func TestJournalDeliveryFailureRedacted(t *testing.T) {
	clk := newJClock()
	w := journaledWatcher(clk)
	s := jStreamer("alice", "id-alice")
	w.journalSlotTransitions([]slotOccupant{occ(s, 0, OriginConfigured, "priority")})

	w.recordSlotDelivery(s, SendResult{Failure: &WatchFailure{Stage: StageBeacon, Status: 403, ErrorCode: "beacon_http_403"}})

	df := onlyOfType(slotEvents(w), journal.SlotDeliveryFailure)
	if len(df) != 1 {
		t.Fatalf("expected one delivery_failure, got %d", len(df))
	}
	if df[0].Stage != string(StageBeacon) || df[0].Status != 403 || df[0].ErrorCode != "beacon_http_403" {
		t.Fatalf("delivery_failure fields wrong: %+v", df[0])
	}
	if w.slotResidence["alice"].failures != 1 {
		t.Fatalf("failure counter not incremented")
	}
	if w.slotResidence["alice"].successes != 0 {
		t.Fatalf("a failed delivery must not increment the success counter, got %d", w.slotResidence["alice"].successes)
	}
}

// TestJournalResidenceTimerStableAcrossSteadyTicks (M4 guard): a harmless
// same-state observation must NOT reset the residence timer, so the release
// duration reflects the WHOLE residence, not just the time since the last tick.
func TestJournalResidenceTimerStableAcrossSteadyTicks(t *testing.T) {
	clk := newJClock()
	w := journaledWatcher(clk)
	s := jStreamer("alice", "id-alice")
	slots := []slotOccupant{occ(s, 0, OriginConfigured, "priority")}

	w.journalSlotTransitions(slots) // enter
	clk.advance(2 * time.Minute)
	w.journalSlotTransitions(slots) // steady tick — must not reset the timer
	clk.advance(3 * time.Minute)
	w.journalSlotTransitions(nil) // release after 5 minutes total

	rel := onlyOfType(slotEvents(w), journal.SlotReleased)
	if len(rel) != 1 {
		t.Fatalf("expected one release, got %d", len(rel))
	}
	if rel[0].ResidenceSeconds != 300 {
		t.Fatalf("residence timer was reset by a steady tick: got %vs, want 300s", rel[0].ResidenceSeconds)
	}
}

// T10 — stale session: the diagnostic hook mutates nothing (no event, no counter
// change) and never classifies the streamer offline.
func TestJournalStaleIsInert(t *testing.T) {
	clk := newJClock()
	w := journaledWatcher(clk)
	s := jStreamer("alice", "id-alice")
	w.journalSlotTransitions([]slotOccupant{occ(s, 0, OriginConfigured, "priority")})
	before := len(slotEvents(w))

	w.recordSlotDelivery(s, SendResult{Stale: true})

	if got := len(slotEvents(w)); got != before {
		t.Fatalf("stale send appended %d events, want none", got-before)
	}
	r := w.slotResidence["alice"]
	if r.successes != 0 || r.failures != 0 {
		t.Fatalf("stale send changed counters: successes=%d failures=%d", r.successes, r.failures)
	}
	if !s.GetIsOnline() {
		t.Fatal("stale send must not classify the streamer offline")
	}
}

// T19 — secret scan: fixtures with fake token/URL/cookie values prove the journal
// omits them. The watch-transport failure surface is redacted by construction,
// so even with secrets planted on the streamer's session the journal never
// carries them.
func TestJournalSecretScan(t *testing.T) {
	clk := newJClock()
	w := journaledWatcher(clk)
	s := jStreamer("alice", "id-alice")
	// Plant an obvious secret on the streamer's session; the journal must ignore
	// it and carry only the redacted WatchFailure surface.
	s.Stream.SetSpadeURL("https://spade.twitch.example/track?token=SECRETSPADE")

	w.journalSlotTransitions([]slotOccupant{occ(s, 0, OriginConfigured, "priority")})
	w.recordSlotDelivery(s, SendResult{Failure: &WatchFailure{Stage: StageBeacon, Status: 403, ErrorCode: "beacon_http_403"}})
	w.journalContinuityReset(s, "slot_lost")

	blob, err := json.Marshal(w.SlotJournalSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"SECRETSPADE", "spade", "https://", "token=", "Bearer", "cookie", "oauth"} {
		if strings.Contains(strings.ToLower(string(blob)), strings.ToLower(bad)) {
			t.Fatalf("slot journal leaked forbidden marker %q: %s", bad, blob)
		}
	}
}

// T20 — behavior parity: with the journal enabled or absent, the selected slots
// are identical.
func TestJournalBehaviorParity(t *testing.T) {
	run := func(withJournal bool) []string {
		sender := &countingSender{sent: make(chan string, 16)}
		checker := &staticChecker{checked: make(chan string, 16)}
		w, _ := newLoopWatcher(2, sender, checker)
		if withJournal {
			w.SetSlotJournal(journal.New[journal.SlotEvent](64, newJClock().now))
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w.ctx = ctx
		w.processWatching()

		var logins []string
		for _, sl := range w.BrokerSnapshot().Slots {
			logins = append(logins, sl.Channel)
		}
		sort.Strings(logins)
		return logins
	}

	with := run(true)
	without := run(false)
	if strings.Join(with, ",") != strings.Join(without, ",") {
		t.Fatalf("journal changed selection: with=%v without=%v", with, without)
	}
	if len(with) == 0 {
		t.Fatal("expected some slots selected")
	}
}

// T18 — shutdown: the loop stops cleanly and no post-shutdown journal append
// happens from abandoned workers.
func TestJournalNoPostShutdownAppend(t *testing.T) {
	sender := &countingSender{sent: make(chan string, 64)}
	checker := &staticChecker{checked: make(chan string, 64)}
	w, _ := newLoopWatcher(2, sender, checker)
	w.SetSlotJournal(journal.New[journal.SlotEvent](128, newJClock().now))

	ctx, cancel := context.WithCancel(context.Background())
	w.ctx = ctx

	done := make(chan struct{})
	go func() { w.loop(); close(done) }()

	// Let a few ticks run, then stop.
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not exit after context cancel (goroutine leak)")
	}

	seqAfterStop := w.slotJournal.LastSeq()
	time.Sleep(30 * time.Millisecond)
	if got := w.slotJournal.LastSeq(); got != seqAfterStop {
		t.Fatalf("journal appended after shutdown: %d -> %d", seqAfterStop, got)
	}
}
