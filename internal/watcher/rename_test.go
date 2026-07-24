package watcher

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestApplyStreamerList_RenameInPlacePreservesRotationState is the watcher
// half of BKM-006 invariant I (slot/watch continuity): a config-driven
// rename reconciles the SAME *models.Streamer object in place (only its
// Username changes), so applyStreamerList's index-keyed rotation/fairness
// state (activePair, lastWatched, deferredFor, boost latch) must translate
// cleanly by the streamer's CURRENT login — proving no slot release/
// reacquire happens, even in the defensive case where UpdateStreamers IS
// invoked across a rename (the miner normally never calls it for a PURE
// rename, since added/removed are both empty, but this guards the remap
// logic itself, which the race-safety GetUsername() conversion depends on).
func TestApplyStreamerList_RenameInPlacePreservesRotationState(t *testing.T) {
	w, online := newTestWatcher(3)
	for _, i := range online {
		w.streamers[i].SetConfirmedOnline()
		w.streamers[i].OnlineAt = time.Now().Add(-time.Minute)
	}

	// Seed rotation/fairness state referencing indexes by hand (mirrors what
	// a real tick would have accumulated).
	w.rotation.activePair = [2]int{0, 1}
	w.rotation.hasPair = true
	w.rotation.lastWatched = map[int]time.Time{0: time.Now().Add(-time.Minute), 1: time.Now().Add(-2 * time.Minute)}
	w.rotation.deferredFor = map[int]bool{2: true}
	w.rotation.boostLatched = true
	w.rotation.boostTarget = 2
	w.rotation.boostVictim = 1

	renamedStreamer := w.streamers[1]
	renamedOldLogin := renamedStreamer.GetUsername()
	obs := renamedStreamer.BeginLoginObservation()
	if !renamedStreamer.RenameIfCurrent("renamed-login", obs) {
		t.Fatal("rename setup failed")
	}
	if got := renamedStreamer.GetUsername(); got == renamedOldLogin {
		t.Fatal("rename setup did not change the login")
	}

	// Build the "new" list exactly as the manager would hand it to
	// UpdateStreamers: the SAME pointers, in the SAME order — index 1's
	// pointer now reports the new login.
	newList := append([]*models.Streamer(nil), w.streamers...)
	w.UpdateStreamers(newList)
	w.applyPendingSettings()

	// Same pointer, same index: the manager's rename is retained verbatim,
	// nothing was dropped or reordered.
	if w.streamers[1] != renamedStreamer {
		t.Fatal("applyStreamerList lost the renamed streamer's identity/position")
	}
	if got := w.streamers[1].GetUsername(); got != "renamed-login" {
		t.Fatalf("streamer at index 1 login = %q, want renamed-login", got)
	}

	// Rotation/fairness state keyed by index 1 must have survived the remap
	// (translated by the CURRENT login, which is race-safe via GetUsername).
	if w.rotation.activePair != [2]int{0, 1} {
		t.Fatalf("activePair = %v, want unchanged [0 1] (no slot release/reacquire)", w.rotation.activePair)
	}
	if !w.rotation.hasPair {
		t.Fatal("hasPair was reset by a pure rename — the pair should never be dropped")
	}
	if _, ok := w.rotation.lastWatched[1]; !ok {
		t.Fatal("lastWatched fairness state for the renamed streamer's index was lost")
	}
	if !w.rotation.deferredFor[2] {
		t.Fatal("unrelated deferredFor state for another index was lost by the rename's remap")
	}
	if !w.rotation.boostLatched || w.rotation.boostTarget != 2 || w.rotation.boostVictim != 1 {
		t.Fatalf("boost latch state corrupted by the rename: latched=%v target=%d victim=%d",
			w.rotation.boostLatched, w.rotation.boostTarget, w.rotation.boostVictim)
	}

	// The old login no longer identifies anything in the loop's own roster.
	for _, s := range w.streamers {
		if s.GetUsername() == renamedOldLogin {
			t.Fatal("the old login must not resolve to any streamer in the loop's roster after the rename")
		}
	}
}
