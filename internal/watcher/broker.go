package watcher

import (
	"log/slog"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// Reason codes explaining why a channel holds (or is waiting for) a watch
// slot. They are stable identifiers for the dashboard/debug snapshot; the
// human-readable Reason string carries the detail.
const (
	ReasonRestrictedDrop = "restricted_drop" // channel-restricted campaign, only progresses here
	ReasonStreak         = "streak"          // watch streak in progress
	ReasonActiveDrop     = "active_drop"     // active (unrestricted) drop campaign
	ReasonFairRotation   = "fair_rotation"   // holds a fair-rotation slot
	ReasonPriority       = "priority"        // matched a configured priority (ORDER/POINTS/SUBSCRIBED)
	ReasonDiscoveryFill  = "discovery_fill"  // discovered channel filling an otherwise-idle slot
	ReasonLowerPriority  = "lower_priority"  // waiting: both slots held by equal/higher priority
)

// slotRank orders reason codes for cross-source arbitration: a higher rank may
// displace a strictly lower-ranked configured slot occupant (subject to the
// watch-streak protection in arbitrate). Discovery and configured channels are
// ranked by the same scale so a channel-restricted drop always beats a plain
// points/rotation pick regardless of which source proposed it.
func slotRank(reasonCode string) int {
	switch reasonCode {
	case ReasonRestrictedDrop:
		return 4
	case ReasonStreak:
		return 3
	case ReasonActiveDrop:
		return 2
	default:
		return 1
	}
}

// SlotAssignment is one occupied watch slot in the explainable broker
// snapshot: which channel holds it, where it came from, and why.
type SlotAssignment struct {
	Slot       int       `json:"slot"`
	Channel    string    `json:"channel"`
	Origin     string    `json:"source"`
	ReasonCode string    `json:"reasonCode"`
	Reason     string    `json:"reason"`
	Campaign   string    `json:"campaign,omitempty"`
	SelectedAt time.Time `json:"selectedAt,omitzero"`
}

// WaitingChannel is a channel that was proposed for a slot but did not get one
// this tick, with the reason it is waiting.
type WaitingChannel struct {
	Channel    string `json:"channel"`
	Origin     string `json:"source"`
	ReasonCode string `json:"reasonCode"`
	Reason     string `json:"reason"`
}

// BrokerSnapshot is the immutable, explainable view of the watch-slot
// allocation published once per tick. The dashboard, the debug endpoint, and
// directory discovery all read it (never the broker's internal state) so there
// is a single, race-free source of truth for "who is being watched and why".
type BrokerSnapshot struct {
	EvaluatedAt time.Time        `json:"evaluatedAt"`
	MaxSlots    int              `json:"maxSlots"`
	Slots       []SlotAssignment `json:"slots"`
	Waiting     []WaitingChannel `json:"waiting,omitempty"`
}

// slotOccupant is the broker's internal working representation of a filled
// slot during arbitration. idx is the index into w.streamers for configured
// channels, or -1 for external (discovery) candidates.
type slotOccupant struct {
	streamer   *models.Streamer
	origin     string
	idx        int
	reasonCode string
	reason     string
	campaign   string
	selectedAt time.Time
}

// classify assigns a reason code, human-readable reason, and best-effort
// campaign name to a candidate. For configured channels it prefers the
// per-tick reason already noted by the priority/rotation selection, falling
// back to a generic one; discovery channels get a discovery-specific reason.
// idx is the streamer's index for configured channels, or -1.
func (w *MinuteWatcher) classify(s *models.Streamer, origin string, idx int) (reasonCode, reason, campaign string) {
	restricted := s.HasChannelRestrictedCampaign()
	switch {
	case restricted:
		reasonCode = ReasonRestrictedDrop
	case origin == OriginConfigured && idx >= 0 && w.streakInProgress(idx):
		reasonCode = ReasonStreak
	case s.DropsCondition():
		reasonCode = ReasonActiveDrop
	case origin == OriginDiscovery:
		reasonCode = ReasonDiscoveryFill
	default:
		reasonCode = ReasonPriority
	}

	campaign = campaignName(s, restricted)

	if origin == OriginConfigured && idx >= 0 {
		if noted := w.selectionReasons[idx]; noted != "" {
			reason = noted
		}
	}
	if reason == "" {
		reason = defaultReason(reasonCode, origin)
	}
	return reasonCode, reason, campaign
}

// campaignName returns the name of the campaign driving a drop-based slot: the
// channel-restricted one when restricted is set, otherwise the first active
// campaign. Empty when the streamer has no assigned campaigns.
func campaignName(s *models.Streamer, restricted bool) string {
	summaries := s.ActiveCampaignsSummary()
	if restricted {
		for _, c := range summaries {
			if c.ChannelRestricted {
				return c.Name
			}
		}
	}
	if len(summaries) > 0 {
		return summaries[0].Name
	}
	return ""
}

func defaultReason(reasonCode, origin string) string {
	switch reasonCode {
	case ReasonRestrictedDrop:
		return "channel-restricted drop campaign only progresses on this exact channel"
	case ReasonStreak:
		return "watch streak not yet earned this stream"
	case ReasonActiveDrop:
		if origin == OriginDiscovery {
			return "discovered channel with an active drop campaign for a configured game"
		}
		return "active drop campaign"
	case ReasonDiscoveryFill:
		return "discovered channel filling an otherwise-idle watch slot"
	default:
		return "holds a watch slot"
	}
}

// arbitrate is the cross-source allocation (phase B). It starts from the
// configured channels the priority/rotation selection already chose (phase A,
// unchanged) and layers external candidates on top, enforcing the global
// constants.MaxSimultaneousStreams cap:
//
//   - a candidate fills any free slot;
//   - otherwise it may displace a configured occupant it strictly outranks,
//     picking the eviction target by betterDisplaceVictim (lowest rank, then
//     most-recently-watched among equals — a stable, order-independent choice),
//     except one within minutes of completing a watch streak (which is never
//     interrupted — mirrors applyPriorityBoost);
//   - a channel already occupying a slot is never given a second one.
//
// With no external candidates this is a pure pass-through of the configured
// selection, so all existing single-list behavior is preserved exactly.
func (w *MinuteWatcher) arbitrate(configuredWatch []int, extra []Candidate, now time.Time) ([]slotOccupant, []WaitingChannel) {
	slots := make([]slotOccupant, 0, constants.MaxSimultaneousStreams)
	seen := make(map[string]bool, constants.MaxSimultaneousStreams)

	for _, idx := range configuredWatch {
		s := w.streamers[idx]
		rc, reason, camp := w.classify(s, OriginConfigured, idx)
		slots = append(slots, slotOccupant{
			streamer: s, origin: OriginConfigured, idx: idx,
			reasonCode: rc, reason: reason, campaign: camp, selectedAt: now,
		})
		seen[s.Username] = true
	}

	var waiting []WaitingChannel
	for _, c := range extra {
		if c.Streamer == nil {
			continue
		}
		login := c.Streamer.Username
		if seen[login] {
			// One channel can never occupy two slots (e.g. a discovered
			// channel that is also on the configured list).
			continue
		}
		rc, reason, camp := w.classify(c.Streamer, c.Origin, -1)
		incoming := slotOccupant{
			streamer: c.Streamer, origin: c.Origin, idx: -1,
			reasonCode: rc, reason: reason, campaign: camp, selectedAt: now,
		}

		if len(slots) < constants.MaxSimultaneousStreams {
			slots = append(slots, incoming)
			seen[login] = true
			continue
		}

		victim := w.pickDisplaceable(slots, incoming)
		if victim < 0 {
			waiting = append(waiting, WaitingChannel{
				Channel:    login,
				Origin:     c.Origin,
				ReasonCode: ReasonLowerPriority,
				Reason:     "both watch slots are held by equal or higher-priority channels",
			})
			continue
		}

		evicted := slots[victim]
		waiting = append(waiting, WaitingChannel{
			Channel:    evicted.streamer.Username,
			Origin:     evicted.origin,
			ReasonCode: ReasonLowerPriority,
			Reason:     "displaced this tick by a higher-priority " + c.Origin + " channel",
		})
		if evicted.idx >= 0 {
			w.noteSelection(evicted.idx, "not watched this tick: displaced by a higher-priority "+c.Origin+" channel ("+reason+")")
		}
		delete(seen, evicted.streamer.Username)
		slots[victim] = incoming
		seen[login] = true
	}

	return slots, waiting
}

// pickDisplaceable returns the index into slots of the configured occupant the
// incoming candidate may displace: the one it strictly outranks that is the
// best eviction target per betterDisplaceVictim, and is not protected by a
// near-complete watch streak. Returns -1 if none. Discovery occupants are never
// displaced (there is at most one, and it never out-prioritizes a configured
// slot into a swap war).
func (w *MinuteWatcher) pickDisplaceable(slots []slotOccupant, incoming slotOccupant) int {
	incomingRank := slotRank(incoming.reasonCode)
	victim := -1
	for i, s := range slots {
		if s.origin != OriginConfigured {
			continue
		}
		if s.idx >= 0 && w.nearStreakCompletion(s.idx) {
			continue
		}
		if incomingRank <= slotRank(s.reasonCode) {
			continue
		}
		if victim < 0 || w.betterDisplaceVictim(s, slots[victim]) {
			victim = i
		}
	}
	if victim >= 0 && w.coldStartTie(slots, victim) {
		// The victim was chosen purely by the cold-start alternation branch of
		// betterDisplaceVictim; advance the parity so the next such displacement
		// evicts the other channel instead of pinning one for the whole uptime.
		w.displaceParity++
	}
	return victim
}

// coldStartTie reports whether the chosen victim was decided by the cold-start
// alternation branch: it has no rotation recency and at least one other
// eligible configured occupant shares its rank and (zero) recency. Only then is
// the choice an alternation decision worth advancing; a rank- or recency-decided
// victim, or a lone eligible occupant, must not perturb the parity.
func (w *MinuteWatcher) coldStartTie(slots []slotOccupant, victim int) bool {
	v := slots[victim]
	if !w.rotation.lastWatched[v.idx].IsZero() {
		return false
	}
	tied := 0
	for _, s := range slots {
		if s.origin != OriginConfigured {
			continue
		}
		if s.idx >= 0 && w.nearStreakCompletion(s.idx) {
			continue
		}
		if slotRank(s.reasonCode) != slotRank(v.reasonCode) {
			continue
		}
		if !w.rotation.lastWatched[s.idx].IsZero() {
			continue
		}
		tied++
	}
	return tied >= 2
}

// betterDisplaceVictim reports whether configured occupant a is a better
// eviction target than the current best b. The choice must depend only on
// channel identity (and the loop-owned alternation parity), never on the
// occupants' order in the slots slice: that order is inherited from
// configuredWatch, which in direct mode comes from selectByPriority's map
// iteration and is therefore non-deterministic across ticks. Ranking,
// most-evictable first:
//
//  1. lowest rank — evict the least-valuable pick (unchanged cross-rank rule);
//  2. among equal ranks with real rotation recency, most-recently-watched — so
//     the least-recently-watched keeps its slot, mirroring applyPriorityBoost's
//     fair-rotation victim rule (rotation recency lives in
//     w.rotation.lastWatched);
//  3. cold-start tie (equal rank, no recorded recency at all — e.g. the bot
//     never enters rotation because configured streamers are always ≤2 online):
//     alternate the victim between the tied channels across displacements via
//     displaceParity, so neither is pinned for the whole uptime. Within a single
//     selection pass the parity is constant, so this stays a consistent total
//     order; pickDisplaceable advances it only after a cold-start-tie eviction.
//
// Rotation mode always has real (equal, non-zero) recency for the pair, so it
// takes branch 2 and never alternates — its behaviour is unchanged.
func (w *MinuteWatcher) betterDisplaceVictim(a, b slotOccupant) bool {
	if ra, rb := slotRank(a.reasonCode), slotRank(b.reasonCode); ra != rb {
		return ra < rb
	}
	la, lb := w.rotation.lastWatched[a.idx], w.rotation.lastWatched[b.idx]
	if !la.Equal(lb) {
		return la.After(lb)
	}
	if !la.IsZero() {
		// Equal, real recency (rotation mode): deterministic, no alternation.
		return a.idx < b.idx
	}
	// Cold-start tie: alternate the evicted index across displacements.
	if w.displaceParity%2 == 0 {
		return a.idx < b.idx
	}
	return a.idx > b.idx
}

// publishBrokerSnapshot stores the immutable slot allocation for the dashboard,
// the debug endpoint, and directory discovery to read. Runs on the loop
// goroutine.
func (w *MinuteWatcher) publishBrokerSnapshot(slots []slotOccupant, waiting []WaitingChannel, evaluatedAt time.Time) {
	snap := &BrokerSnapshot{
		EvaluatedAt: evaluatedAt,
		MaxSlots:    constants.MaxSimultaneousStreams,
	}
	watching := make(map[string]bool, len(slots))
	for i, s := range slots {
		snap.Slots = append(snap.Slots, SlotAssignment{
			Slot:       i + 1,
			Channel:    s.streamer.Username,
			Origin:     s.origin,
			ReasonCode: s.reasonCode,
			Reason:     s.reason,
			Campaign:   s.campaign,
			SelectedAt: s.selectedAt,
		})
		watching[s.streamer.Username] = true
	}
	snap.Waiting = waiting

	w.brokerSnapshot.Store(snap)
	w.watchingLogins.Store(&watching)
}

// BrokerSnapshot returns the last published watch-slot allocation. Safe to
// call from any goroutine; returns an empty snapshot before the first tick.
func (w *MinuteWatcher) BrokerSnapshot() BrokerSnapshot {
	if snap := w.brokerSnapshot.Load(); snap != nil {
		return *snap
	}
	return BrokerSnapshot{MaxSlots: constants.MaxSimultaneousStreams}
}

// FreeSlots reports how many of the constants.MaxSimultaneousStreams watch
// slots are currently unoccupied, for opportunistic health-canary scheduling.
// Reads the published snapshot, so it takes no broker lock.
func (w *MinuteWatcher) FreeSlots() int {
	free := constants.MaxSimultaneousStreams - len(w.BrokerSnapshot().Slots)
	if free < 0 {
		return 0
	}
	return free
}

// IsWatching reports whether the channel currently holds a watch slot. It reads
// the published snapshot, so directory discovery can tell whether its proposed
// channel actually got a slot without reaching into broker internals. Satisfies
// the discovery subsystem's slot-status interface.
func (w *MinuteWatcher) IsWatching(login string) bool {
	m := w.watchingLogins.Load()
	if m == nil {
		return false
	}
	return (*m)[login]
}

// logSlotChanges emits an INFO log and a recent-events entry only when the
// allocation actually changes — a channel takes or leaves a slot, or its
// reason changes — so a steady state does not log the same decision every
// minute. Runs on the loop goroutine; lastSlots is loop-owned.
func (w *MinuteWatcher) logSlotChanges(slots []slotOccupant) {
	current := make(map[string]string, len(slots))
	for _, s := range slots {
		current[s.streamer.Username] = s.reasonCode
	}

	for login, code := range current {
		prev, held := w.lastSlots[login]
		switch {
		case !held:
			slog.Info("Watch slot assigned", "channel", login, "reason", code)
			events.Record(events.TypeSlotAssigned, login, code)
		case prev != code:
			slog.Info("Watch slot reason changed", "channel", login, "from", prev, "to", code)
			events.Record(events.TypeSlotAssigned, login, code)
		}
	}
	for login := range w.lastSlots {
		if _, held := current[login]; !held {
			slog.Info("Watch slot released", "channel", login)
			events.Record(events.TypeSlotReleased, login, "")
		}
	}

	w.lastSlots = current
}

// configuredWatchedIndexes returns the w.streamers indexes of the configured
// channels that actually kept a slot after arbitration, for the per-streamer
// debug snapshot to reflect reality (a configured pick displaced by a
// higher-priority discovery drop is reported as not watched).
func configuredWatchedIndexes(slots []slotOccupant) []int {
	var out []int
	for _, s := range slots {
		if s.idx >= 0 {
			out = append(out, s.idx)
		}
	}
	return out
}
