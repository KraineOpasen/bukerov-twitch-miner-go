package watcher

import (
	"sort"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/journal"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// slotResidence is the loop-owned bookkeeping for one channel while it holds a
// watch slot: where/why it entered and its running transport-delivery counters.
// It exists only to enrich terminal journal events (released/replaced) with a
// residence duration and success/failure totals, and never affects scheduling.
type slotResidence struct {
	channelID string
	broadcast string
	origin    string
	reason    string
	idx       int
	enteredAt time.Time
	successes int
	failures  int
}

// SetSlotJournal injects the bounded slot-lifecycle diagnostic journal. Passing
// nil (the default) disables slot journaling entirely — every hook becomes a
// no-op, so selection and sending are identical to a build without journaling.
// Call once at construction, before Start.
func (w *MinuteWatcher) SetSlotJournal(j *journal.Journal[journal.SlotEvent]) {
	w.slotJournal = j
}

// SlotJournalSnapshot returns an immutable, oldest-first copy of the slot
// journal (nil when journaling is disabled). Safe to call from any goroutine.
func (w *MinuteWatcher) SlotJournalSnapshot() []journal.Record[journal.SlotEvent] {
	return w.slotJournal.Snapshot()
}

// journalSlotTransitions records this tick's slot lifecycle transitions
// (entered / reason_changed / released / replaced) into the diagnostic journal.
// It correlates a single-out/single-in change within the same tick as ONE
// "replaced" event (victim -> replacement) at the moment of the swap — it does
// not infer correlation by diffing external snapshots. It runs once per tick on
// the loop goroutine, right after the existing slot-change logging, reads only
// already-computed state, and never affects arbitration. No-op when journaling
// is disabled.
func (w *MinuteWatcher) journalSlotTransitions(slots []slotOccupant) {
	if w.slotJournal == nil {
		return
	}
	if w.slotResidence == nil {
		w.slotResidence = make(map[string]*slotResidence)
	}

	// Current occupants keyed by canonical login (matching logSlotChanges).
	current := make(map[string]slotOccupant, len(slots))
	for _, s := range slots {
		current[s.streamer.GetUsername()] = s
	}

	// Partition this tick's change into disjoint sets; sort each for a
	// deterministic event sequence regardless of Go map iteration order.
	var entered, released, reasonChanged []string
	for login, occ := range current {
		res, held := w.slotResidence[login]
		if !held {
			entered = append(entered, login)
			continue
		}
		if res.reason != occ.reasonCode {
			reasonChanged = append(reasonChanged, login)
		}
	}
	for login := range w.slotResidence {
		if _, held := current[login]; !held {
			released = append(released, login)
		}
	}
	sort.Strings(entered)
	sort.Strings(released)
	sort.Strings(reasonChanged)

	// 1) reason_changed for channels that kept their slot across the tick.
	for _, login := range reasonChanged {
		occ := current[login]
		res := w.slotResidence[login]
		w.slotJournal.Append(journal.SlotEvent{
			Type:       journal.SlotReasonChanged,
			Channel:    login,
			ChannelID:  occ.streamer.ChannelID,
			Broadcast:  occ.streamer.Stream.GetBroadcastID(),
			Origin:     occ.origin,
			SlotIndex:  occ.idx,
			PrevReason: res.reason,
			Reason:     occ.reasonCode,
		})
		res.reason = occ.reasonCode
		res.broadcast = occ.streamer.Stream.GetBroadcastID()
	}

	// 2) A single channel leaving while a single channel enters, within the same
	// tick, is a deterministic rotation/eviction: record it as one correlated
	// "replaced" event. Any other shape (0/2 out, 0/2 in) is recorded as
	// independent released/entered events to avoid fabricating a correlation.
	switch {
	case len(entered) == 1 && len(released) == 1:
		w.journalReplaced(released[0], current[entered[0]])
	default:
		for _, login := range released {
			w.journalReleased(login)
		}
		for _, login := range entered {
			w.journalEntered(current[login])
		}
	}
}

// journalEntered records a slot assignment and opens a fresh residence.
func (w *MinuteWatcher) journalEntered(occ slotOccupant) {
	login := occ.streamer.GetUsername()
	broadcast := occ.streamer.Stream.GetBroadcastID()
	rec := w.slotJournal.Append(journal.SlotEvent{
		Type:      journal.SlotEntered,
		Channel:   login,
		ChannelID: occ.streamer.ChannelID,
		Broadcast: broadcast,
		Origin:    occ.origin,
		SlotIndex: occ.idx,
		Reason:    occ.reasonCode,
	})
	w.slotResidence[login] = &slotResidence{
		channelID: occ.streamer.ChannelID,
		broadcast: broadcast,
		origin:    occ.origin,
		reason:    occ.reasonCode,
		idx:       occ.idx,
		enteredAt: rec.At,
	}
}

// journalReleased records a slot release with the residence's final accounting
// and closes the residence.
func (w *MinuteWatcher) journalReleased(login string) {
	res := w.slotResidence[login]
	if res == nil {
		return
	}
	w.slotJournal.Append(journal.SlotEvent{
		Type:             journal.SlotReleased,
		Channel:          login,
		ChannelID:        res.channelID,
		Broadcast:        res.broadcast,
		Origin:           res.origin,
		SlotIndex:        res.idx,
		Reason:           res.reason,
		ResidenceSeconds: w.slotJournal.Now().Sub(res.enteredAt).Seconds(),
		Successes:        res.successes,
		Failures:         res.failures,
	})
	delete(w.slotResidence, login)
}

// journalReplaced records a correlated victim -> replacement swap as a single
// event and hands the slot's residence over to the replacement. The residence
// accounting on the event (ResidenceSeconds/Successes/Failures) describes the
// VICTIM's just-ended residence.
func (w *MinuteWatcher) journalReplaced(victimLogin string, incoming slotOccupant) {
	victim := w.slotResidence[victimLogin]
	inLogin := incoming.streamer.GetUsername()
	broadcast := incoming.streamer.Stream.GetBroadcastID()

	ev := journal.SlotEvent{
		Type:      journal.SlotReplaced,
		Channel:   inLogin,
		ChannelID: incoming.streamer.ChannelID,
		Broadcast: broadcast,
		Origin:    incoming.origin,
		SlotIndex: incoming.idx,
		Reason:    incoming.reasonCode,
		Victim:    victimLogin,
	}
	if victim != nil {
		ev.VictimID = victim.channelID
		ev.ResidenceSeconds = w.slotJournal.Now().Sub(victim.enteredAt).Seconds()
		ev.Successes = victim.successes
		ev.Failures = victim.failures
	}
	rec := w.slotJournal.Append(ev)

	delete(w.slotResidence, victimLogin)
	w.slotResidence[inLogin] = &slotResidence{
		channelID: incoming.streamer.ChannelID,
		broadcast: broadcast,
		origin:    incoming.origin,
		reason:    incoming.reasonCode,
		idx:       incoming.idx,
		enteredAt: rec.At,
	}
}

// recordSlotDelivery updates the current residence's transport-delivery counters
// and journals the FIRST success and EVERY failure. A stale result changes
// nothing (no minute delivered; not a failure, not an offline signal). Runs on
// the loop goroutine after each send. No-op when journaling is disabled or the
// streamer holds no tracked slot. It records ONLY client-transport delivery — a
// success is NOT a Twitch points-earned confirmation (those flow via PubSub).
func (w *MinuteWatcher) recordSlotDelivery(streamer *models.Streamer, res SendResult) {
	if w.slotJournal == nil {
		return
	}
	r := w.slotResidence[streamer.GetUsername()]
	if r == nil {
		return
	}
	switch {
	case res.Delivered:
		r.successes++
		if r.successes == 1 {
			w.slotJournal.Append(journal.SlotEvent{
				Type:      journal.SlotDeliverySuccess,
				Channel:   streamer.GetUsername(),
				ChannelID: r.channelID,
				Broadcast: streamer.Stream.GetBroadcastID(),
				Origin:    r.origin,
				SlotIndex: r.idx,
				Successes: r.successes,
			})
		}
	case res.Failure != nil:
		r.failures++
		w.slotJournal.Append(journal.SlotEvent{
			Type:      journal.SlotDeliveryFailure,
			Channel:   streamer.GetUsername(),
			ChannelID: r.channelID,
			Broadcast: streamer.Stream.GetBroadcastID(),
			Origin:    r.origin,
			SlotIndex: r.idx,
			Stage:     string(res.Failure.Stage),
			Status:    res.Failure.Status,
			ErrorCode: res.Failure.ErrorCode,
			Failures:  r.failures,
		})
		// res.Stale intentionally falls through to no-op.
	}
}

// journalContinuityReset records that a channel's watch-streak/session
// continuity accumulator was reset, with a bounded reason. Called from the loop
// goroutine (resetLostSlotContinuity). No-op when journaling is disabled.
func (w *MinuteWatcher) journalContinuityReset(streamer *models.Streamer, reason string) {
	if w.slotJournal == nil {
		return
	}
	w.slotJournal.Append(journal.SlotEvent{
		Type:        journal.SlotContinuityReset,
		Channel:     streamer.GetUsername(),
		ChannelID:   streamer.ChannelID,
		Broadcast:   streamer.Stream.GetBroadcastID(),
		ResetReason: reason,
	})
}
