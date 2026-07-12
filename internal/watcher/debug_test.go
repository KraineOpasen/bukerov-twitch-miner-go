package watcher

import (
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// runSelectionTick mimics the selection portion of processWatching: it
// resets the per-tick scratch state, runs selection, and publishes the
// debug state - without the network side effects of the full tick.
func runSelectionTick(w *MinuteWatcher, online []int) {
	w.selectionReasons = make(map[int]string)
	w.selectionMode = ModeIdle
	watching := w.selectStreamersToWatch(online)
	w.publishDebugState(watching, w.selectionMode)
}

func TestDebugStateReportsRotationDecisions(t *testing.T) {
	w, online := newTestWatcher(4)
	for _, s := range w.streamers {
		s.SetOnline()
	}

	runSelectionTick(w, online)
	st := w.GetDebugState()

	if st.Mode != ModeRotation {
		t.Fatalf("expected rotation mode with 4 online streamers, got %q", st.Mode)
	}
	if len(st.ActivePair) != 2 {
		t.Fatalf("expected an active pair of 2, got %v", st.ActivePair)
	}
	if st.NextRotationAt.Before(st.PairSince) {
		t.Errorf("next rotation (%v) should not precede pair since (%v)", st.NextRotationAt, st.PairSince)
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
		s.SetOnline()
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
		s.SetOnline()
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
