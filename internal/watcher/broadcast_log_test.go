package watcher

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// captureWatcherLogs redirects the default slogger to a buffer for the test's
// duration (mirrors the pattern in streak_test.go) and returns the buffer.
func captureWatcherLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// logLineContaining returns the first buffered log line that contains all the
// given substrings, or "" if none does.
func logLineContaining(buf *bytes.Buffer, subs ...string) string {
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		all := true
		for _, s := range subs {
			if !strings.Contains(line, s) {
				all = false
				break
			}
		}
		if all {
			return line
		}
	}
	return ""
}

// TestLogSlotChangesCarriesBroadcastID verifies each slot event logs the
// broadcast ID of the streamer that event is about — in particular a "released"
// event, whose streamer is no longer in the slots slice, must report ITS OWN
// broadcast rather than another slot's.
func TestLogSlotChangesCarriesBroadcastID(t *testing.T) {
	w, _ := newTestWatcher(2)
	w.streamers[0].Stream.Update("bc-A", "t", nil, nil, 0) // streamera
	w.streamers[1].Stream.Update("bc-B", "t", nil, nil, 0) // streamerb
	a, b := w.streamers[0].Username, w.streamers[1].Username

	buf := captureWatcherLogs(t)

	// Tick 1: both take a slot.
	w.logSlotChanges([]slotOccupant{
		{streamer: w.streamers[0], reasonCode: ReasonStreak},
		{streamer: w.streamers[1], reasonCode: ReasonActiveDrop},
	})
	if l := logLineContaining(buf, "Watch slot assigned", "channel="+a); !strings.Contains(l, "broadcastID=bc-A") {
		t.Errorf("assigned %s: want broadcastID=bc-A, got line %q", a, l)
	}
	if l := logLineContaining(buf, "Watch slot assigned", "channel="+b); !strings.Contains(l, "broadcastID=bc-B") {
		t.Errorf("assigned %s: want broadcastID=bc-B, got line %q", b, l)
	}

	// Tick 2: streamer A is released (only B keeps a slot).
	buf.Reset()
	w.logSlotChanges([]slotOccupant{
		{streamer: w.streamers[1], reasonCode: ReasonActiveDrop},
	})
	rel := logLineContaining(buf, "Watch slot released", "channel="+a)
	if rel == "" {
		t.Fatalf("no release line for %s; buffer=%q", a, buf.String())
	}
	if !strings.Contains(rel, "broadcastID=bc-A") {
		t.Errorf("released %s: want its own broadcastID=bc-A, got %q", a, rel)
	}
	if strings.Contains(rel, "bc-B") {
		t.Errorf("released %s leaked another slot's broadcast: %q", a, rel)
	}
}

// TestLogSlotChangesEmptyBroadcastID proves an empty broadcast ID (before the
// first stream-info fetch) neither panics nor perturbs the loop-owned slot
// bookkeeping — the diagnostic never gates anything on the ID.
func TestLogSlotChangesEmptyBroadcastID(t *testing.T) {
	w, _ := newTestWatcher(1) // no Stream.Update -> empty BroadcastID
	_ = captureWatcherLogs(t)

	w.logSlotChanges([]slotOccupant{{streamer: w.streamers[0], reasonCode: ReasonPriority}})
	state, ok := w.lastSlots[w.streamers[0].Username]
	if !ok {
		t.Fatal("assigned slot not recorded in lastSlots")
	}
	if state.broadcast != "" {
		t.Errorf("empty broadcast expected, got %q", state.broadcast)
	}

	// Release: must not panic and must clear the slot.
	w.logSlotChanges(nil)
	if len(w.lastSlots) != 0 {
		t.Fatalf("expected slot released, still tracking %d", len(w.lastSlots))
	}
}
