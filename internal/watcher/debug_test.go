package watcher

import (
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// runSelectionTick mimics the selection portion of processWatching: it
// resets the per-tick scratch state, runs selection, settles the committed
// ordinary residence and rotation recency against what the selection granted,
// and publishes the debug state - without the network side effects of the full
// tick.
//
// Every caller drives configured channels with no competing source, so the
// selection IS the commitment for these fixtures. Settling residence and
// recency here is therefore faithful rather than convenient: both belong to
// committed grants, so a mimic that skipped them would model a scheduler this
// package does not have.
func runSelectionTick(w *MinuteWatcher, online []int) {
	runSelectionTickAt(w, online, time.Now())
}

// runSelectionTickAt is runSelectionTick against an explicit instant, for tests
// that place an evaluation at a controlled offset from the residence deadline.
func runSelectionTickAt(w *MinuteWatcher, online []int, now time.Time) {
	w.selectionReasons = make(map[int]string)
	w.selectionMode = ModeIdle
	watching := w.selectStreamersToWatch(online, now)
	w.commitSelectedOrdinary(watching, now)
	w.publishDebugState(watching, w.selectionMode)
}

// selectAndCommitRotating runs one ordinary rotation evaluation and commits the
// grant it produced, the way processWatching does after arbitration.
//
// A test that models CONSECUTIVE broker evaluations has to commit: rotation
// recency and ordinary residence are both properties of the grants that were
// actually made, so a loop that only ever proposes would leave both frozen and
// would no longer model the loop it is standing in for.
func selectAndCommitRotating(w *MinuteWatcher, online []int) []int {
	w.selectionMode = ModeRotation
	pair := w.selectRotating(online, time.Now())
	w.commitSelectedOrdinary(pair, time.Now())
	return pair
}

// commitSelectedOrdinary settles residence and recency for a selection that
// received every slot it asked for, through the same production entry points
// processWatching uses on its final allocation.
func (w *MinuteWatcher) commitSelectedOrdinary(watching []int, now time.Time) {
	slots := make([]slotOccupant, 0, len(watching))
	for _, idx := range watching {
		slots = append(slots, slotOccupant{
			streamer: w.streamers[idx], origin: OriginConfigured, idx: idx, selectedAt: now,
		})
	}
	w.commitOrdinaryResidence(slots, now)
	w.noteCommittedRecency(slots, now)
}

func TestDebugStateReportsRotationDecisions(t *testing.T) {
	w, online := newTestWatcher(4)
	for _, s := range w.streamers {
		s.SetConfirmedOnline()
	}

	runSelectionTick(w, online)
	st := w.GetDebugState()

	if st.Mode != ModeRotation {
		t.Fatalf("expected rotation mode with 4 online streamers, got %q", st.Mode)
	}
	if len(st.ActivePair) != 2 {
		t.Fatalf("expected an active pair of 2, got %v", st.ActivePair)
	}
	if st.PairSince.IsZero() {
		t.Error("rotation mode must report when the actual pair membership began")
	}
	if len(st.Decisions) != 4 {
		t.Fatalf("expected a decision per online streamer, got %d", len(st.Decisions))
	}

	watched := 0
	for _, d := range st.Decisions {
		if d.Reason == "" {
			t.Errorf("decision for %s has no reason", d.Username)
		}
		if d.Watching {
			watched++
		}
	}
	if watched != 2 {
		t.Fatalf("expected exactly 2 streamers watched, got %d", watched)
	}
}

func TestDebugStateExplainsAvoidedStreamer(t *testing.T) {
	w, online := newTestWatcher(4)
	for _, s := range w.streamers {
		s.SetConfirmedOnline()
	}
	w.streamers[1].Settings.Preference = models.PreferenceAvoid

	runSelectionTick(w, online)
	st := w.GetDebugState()

	var found bool
	for _, d := range st.Decisions {
		if d.Username != w.streamers[1].Username {
			continue
		}
		found = true
		if d.Watching {
			t.Error("avoided streamer must not be watched while others are online")
		}
		if !strings.Contains(d.Reason, "avoid") {
			t.Errorf("expected the reason to mention the avoid preference, got %q", d.Reason)
		}
	}
	if !found {
		t.Fatal("no decision recorded for the avoided streamer")
	}
}

func TestDebugStateDirectModeReportsPriorityReason(t *testing.T) {
	w, online := newTestWatcher(2)
	for _, s := range w.streamers {
		s.SetConfirmedOnline()
	}

	runSelectionTick(w, online)
	st := w.GetDebugState()

	if st.Mode != ModeDirect {
		t.Fatalf("expected direct mode with 2 online streamers, got %q", st.Mode)
	}
	for _, d := range st.Decisions {
		if !d.Watching {
			t.Errorf("expected %s to be watched with only 2 online", d.Username)
		}
		if !strings.Contains(d.Reason, "ORDER") {
			t.Errorf("expected ORDER priority reason, got %q", d.Reason)
		}
	}
}
