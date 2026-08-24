package watcher

import (
	"log/slog"
	"time"
)

// Selection modes reported in DebugState.Mode.
const (
	// ModeIdle: no streamer is online, nothing is being watched.
	ModeIdle = "idle"
	// ModeDirect: at most constants.MaxSimultaneousStreams candidates are
	// online, so the original priority-based picker runs with no rotation.
	ModeDirect = "direct"
	// ModeRotation: more candidates than watch slots - the fair watch-pair
	// rotation decides who occupies the two slots.
	ModeRotation = "rotation"
)

// DebugState is a point-in-time view of the watcher's selection decisions,
// rebuilt once per processWatching tick and served (read-only) by the debug
// HTTP endpoint.
type DebugState struct {
	// EvaluatedAt is when the watch loop last ran a selection pass; zero
	// until the first tick completes.
	EvaluatedAt time.Time
	Mode        string

	// ActivePair/PairSince describe the current fair base pair and are only
	// populated in ModeRotation. PairSince changes only with actual membership.
	ActivePair []string
	PairSince  time.Time

	// PostponedSwapOuts lists pair members whose fairness replacement is
	// currently deferred by the explicit streak-completion deadline.
	PostponedSwapOuts []PostponedSwapOut

	// Decisions holds one entry per online streamer with a human-readable
	// reason for why it is or isn't being watched right now.
	Decisions []WatchDecision

	// WatchTimeWindowHours is the trailing window (in hours) over which
	// WatchedMinutesWindow accumulates.
	WatchTimeWindowHours float64
}

type WatchDecision struct {
	Username string
	Watching bool
	Reason   string
	// WatchedMinutesWindow is the streamer's accumulated watch minutes over
	// the trailing watchTimeWindow - the fairness metric the rotation ranks
	// by. Zero when no watch-time store is configured.
	WatchedMinutesWindow float64
}

type PostponedSwapOut struct {
	Username string
	Until    time.Time
}

// noteSelection records a human-readable explanation for why the streamer at
// idx is (not) being watched this tick. Only ever called from the loop()
// goroutine; the map is rebuilt at the start of every processWatching pass
// and published into debugState (which has its own lock) at the end.
func (w *MinuteWatcher) noteSelection(idx int, reason string) {
	if w.selectionReasons == nil {
		w.selectionReasons = make(map[int]string)
	}
	w.selectionReasons[idx] = reason
}

// noteSelectionIfEmpty records a reason only when no more specific one (a
// boost, a displacement, a postponed swap-out) was already noted this tick.
func (w *MinuteWatcher) noteSelectionIfEmpty(idx int, reason string) {
	if w.selectionReasons == nil {
		w.selectionReasons = make(map[int]string)
	}
	if _, ok := w.selectionReasons[idx]; !ok {
		w.selectionReasons[idx] = reason
	}
}

// publishDebugState snapshots this tick's selection outcome into debugState
// for the debug endpoint. watching holds the indexes actually selected; mode
// is one of the Mode* constants. Runs on the loop() goroutine.
func (w *MinuteWatcher) publishDebugState(watching []int, mode string) {
	watchingSet := make(map[int]bool, len(watching))
	for _, idx := range watching {
		watchingSet[idx] = true
	}
	if !w.rotation.deferUntil.IsZero() && !watchingSet[w.rotation.deferStreamer] {
		idx := w.rotation.deferStreamer
		w.rotation.clearActiveDeferral()
		w.noteSelection(idx, "not watched this tick: a strictly stronger broker contender displaced the deferred streak seat")
	}

	st := DebugState{
		EvaluatedAt:          time.Now(),
		Mode:                 mode,
		WatchTimeWindowHours: watchTimeWindow.Hours(),
	}

	if mode == ModeRotation && w.rotation.hasPair {
		st.ActivePair = []string{
			w.streamers[w.rotation.activePair[0]].GetUsername(),
			w.streamers[w.rotation.activePair[1]].GetUsername(),
		}
		st.PairSince = w.rotation.lastSwitch

		if !w.rotation.deferUntil.IsZero() && time.Now().Before(w.rotation.deferUntil) {
			idx := w.rotation.deferStreamer
			if idx >= 0 && idx < len(w.streamers) {
				st.PostponedSwapOuts = append(st.PostponedSwapOuts, PostponedSwapOut{
					Username: w.streamers[idx].GetUsername(),
					Until:    w.rotation.deferUntil,
				})
			}
		}
	}

	for idx, s := range w.streamers {
		reason, noted := w.selectionReasons[idx]
		if !noted && !s.GetIsOnline() {
			continue
		}
		if reason == "" {
			switch {
			case watchingSet[idx]:
				reason = "watched"
			case mode == ModeRotation:
				reason = "waiting: persisted accumulated watch time did not earn a fair slot at this broker evaluation"
			default:
				reason = "online but not matched by any configured priority - no watch slot assigned this tick"
			}
		}
		st.Decisions = append(st.Decisions, WatchDecision{
			Username: s.GetUsername(),
			Watching: watchingSet[idx],
			Reason:   reason,
		})
	}

	w.debugMu.Lock()
	w.debugState = st
	w.debugMu.Unlock()
}

// GetDebugState returns the last published selection snapshot, with each
// decision's accumulated watch-time window refreshed from the store at call
// time (the reasons themselves are only recomputed once per watch tick).
// Safe to call from any goroutine.
func (w *MinuteWatcher) GetDebugState() DebugState {
	w.debugMu.Lock()
	st := w.debugState
	st.Decisions = append([]WatchDecision(nil), st.Decisions...)
	st.ActivePair = append([]string(nil), st.ActivePair...)
	st.PostponedSwapOuts = append([]PostponedSwapOut(nil), st.PostponedSwapOuts...)
	w.debugMu.Unlock()

	if w.store != nil && len(st.Decisions) > 0 {
		usernames := make([]string, len(st.Decisions))
		for i, d := range st.Decisions {
			usernames[i] = d.Username
		}
		if minutes, err := w.store.WindowMinutes(usernames, time.Now()); err == nil {
			for i := range st.Decisions {
				st.Decisions[i].WatchedMinutesWindow = minutes[st.Decisions[i].Username]
			}
		} else {
			slog.Debug("Failed to load watch-time window for debug snapshot", "error", err)
		}
	}

	return st
}
