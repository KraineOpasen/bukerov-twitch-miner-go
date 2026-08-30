package watcher

import "github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"

// RunRoutineRefresh linearizes a routine stream metadata/status refresh with
// exact-owner provisional lease admission. The callback is registered under
// observationMu before it starts, then runs with no watcher lock held. A lease
// for the same private Streamer object therefore either wins first and denies
// the refresh, or observes the in-flight registration and waits for a later
// broker tick. Other streamer objects remain independent.
//
// Registration is active even while provisional monitoring is disabled: the
// watchdog may enable monitoring while a previously-started routine refresh is
// still in flight, and that refresh must still fence a new lease. The deferred
// release also makes a callback panic incapable of leaking admission state.
func (w *MinuteWatcher) RunRoutineRefresh(streamer *models.Streamer, refresh func()) bool {
	if streamer == nil || refresh == nil {
		return false
	}

	w.observationMu.Lock()
	if w.provisionalLease != nil && w.provisionalLeaseStreamer == streamer {
		w.observationMu.Unlock()
		return false
	}
	for _, permit := range w.observationPermits {
		if permit.streamer == streamer && (permit.leaseID != 0 || permit.proofID != 0) {
			w.observationMu.Unlock()
			return false
		}
	}
	if w.routineRefreshes == nil {
		w.routineRefreshes = make(map[*models.Streamer]uint64)
	}
	w.routineRefreshes[streamer]++
	w.observationMu.Unlock()

	defer func() {
		w.observationMu.Lock()
		if count := w.routineRefreshes[streamer]; count <= 1 {
			delete(w.routineRefreshes, streamer)
		} else {
			w.routineRefreshes[streamer] = count - 1
		}
		w.observationMu.Unlock()
	}()

	refresh()
	return true
}

// routineRefreshActiveLocked reports whether an exact-owner routine refresh
// won admission before a provisional lease. The caller must hold
// observationMu.
func (w *MinuteWatcher) routineRefreshActiveLocked(streamer *models.Streamer) bool {
	return streamer != nil && w.routineRefreshes[streamer] != 0
}
