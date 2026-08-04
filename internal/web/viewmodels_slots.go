package web

import (
	"sort"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/debug"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/util"
)

// ============================================================================
// Phase 3 (OD-S5-3-1): safe watching-evidence adapter.
//
// watchSlotEvidence is the ONLY source S5-3's new UI (the C12 slot pair, the
// queue roster's reason-code column) ever reads for watch-slot/reason
// evidence. It is built by copying the EXISTING, unconditionally-wired
// Server.supportBundleSource field-by-field into a narrow, internal/web-owned
// type that structurally cannot carry a free-form Reason: watchSlotEntry and
// waitingSlotEntry simply have no field for it. debug.WatchSlot.Reason and
// debug.WaitingSlot.Reason are never read here, and the complete
// debug.Snapshot is never retained past watchSlotEvidence() itself - but the
// two fields are not equally sensitive in the CURRENT source. Only
// debug.WatchSlot.Reason (internal/watcher/broker.go's classify(), fed by
// noteSelection) can embed a dynamic private value for a configured slot - a
// channel-points balance under POINTS_ASCENDING/DESCENDING priority, or a
// subscription points-multiplier under SUBSCRIBED priority.
// debug.WaitingSlot.Reason (arbitrate()) is, as of this source, always one of
// two fixed, non-private template strings ("both watch slots are held by
// equal or higher-priority channels" / "displaced this tick by a
// higher-priority <origin> channel") - noteSelection's per-channel notes are
// never threaded into a WaitingChannel. It stays excluded here anyway,
// deliberately: nothing in this adapter's contract depends on that being
// true forever, and structurally excluding it costs nothing.
//
// This is not another provider or setter: it reuses the exact field OD-S5-3-1
// designates (Server.supportBundleSource) and the exact panic-recovery helper
// handlers_support_bundle.go already defines (safeSupportBundleSnapshot) -
// both read-only reuse within the same package, nothing new wired in.
// ============================================================================

// watchSlotEntry is the narrow, immutable copy of one occupied watch slot's
// allowlisted evidence (OD-S5-3-1 item 3).
type watchSlotEntry struct {
	Slot       int
	Channel    string
	Source     string
	ReasonCode string
	Campaign   string
}

// waitingSlotEntry is the narrow, immutable copy of one waiting/proposed
// channel's allowlisted evidence (OD-S5-3-1 item 3).
type waitingSlotEntry struct {
	Channel    string
	Source     string
	ReasonCode string
}

// watchSlotEvidence is the S5-3-owned, immutable snapshot of the watch
// broker's allocation evidence. The zero value (Mode=="", no Slots, no
// Waiting) is the honest S-NOBACK/S-UNK representation of "no evidence
// available" - never an error, never a fabricated reason.
type watchSlotEvidence struct {
	Mode        string
	EvaluatedAt time.Time
	Slots       []watchSlotEntry
	Waiting     []waitingSlotEntry
}

// slotFor returns the evidence for channel if it currently holds a slot.
func (e watchSlotEvidence) slotFor(channel string) (watchSlotEntry, bool) {
	for _, sl := range e.Slots {
		if sl.Channel == channel {
			return sl, true
		}
	}
	return watchSlotEntry{}, false
}

// waitingFor returns the evidence for channel if it is currently waiting for
// a slot.
func (e watchSlotEvidence) waitingFor(channel string) (waitingSlotEntry, bool) {
	for _, wc := range e.Waiting {
		if wc.Channel == channel {
			return wc, true
		}
	}
	return waitingSlotEntry{}, false
}

// watchSlotEvidence reads the provider pointer under s.mu, releases the lock,
// and only then calls the provider (OD-S5-3-1 item 5) - the identical locking
// shape handleSupportBundle already uses for this same field
// (handlers_support_bundle.go). A nil provider (never wired - most tests, and
// any build predating the support bundle) normalizes to the zero value; a
// panicking provider is recovered by the shared safeSupportBundleSnapshot
// helper and also normalizes to the zero value - either way this never
// propagates an error or a panic into a page render.
func (s *Server) watchSlotEvidence() watchSlotEvidence {
	s.mu.RLock()
	source := s.supportBundleSource
	s.mu.RUnlock()

	snap, err := safeSupportBundleSnapshot(source)
	if err != nil {
		return watchSlotEvidence{}
	}
	return copyWatchSlotEvidence(snap.Watching)
}

// copyWatchSlotEvidence performs the field-by-field allowlist copy
// (OD-S5-3-1 item 3). Every field NOT named there - most importantly Reason
// on both debug.WatchSlot and debug.WaitingSlot - is simply never read from w.
func copyWatchSlotEvidence(w debug.WatchingInfo) watchSlotEvidence {
	out := watchSlotEvidence{Mode: w.Mode, EvaluatedAt: w.EvaluatedAt}
	for _, sl := range w.Slots {
		out.Slots = append(out.Slots, watchSlotEntry{
			Slot:       sl.Slot,
			Channel:    sl.Channel,
			Source:     sl.Source,
			ReasonCode: sl.ReasonCode,
			Campaign:   sl.Campaign,
		})
	}
	for _, wc := range w.Waiting {
		out.Waiting = append(out.Waiting, waitingSlotEntry{
			Channel:    wc.Channel,
			Source:     wc.Source,
			ReasonCode: wc.ReasonCode,
		})
	}
	return out
}

// ============================================================================
// ReasonCode -> localized, accessible label. Closed enum only (task Phase 6:
// "render only existing closed-enum ReasonCode values"); an unrecognized or
// empty code resolves to the S-UNK "unknown reason" label, never a raw
// machine token and never invented text.
// ============================================================================

// reasonCodeKeys maps the watch broker's closed-enum ReasonCode values
// (internal/watcher/broker.go) to their i18n key.
var reasonCodeKeys = map[string]string{
	"restricted_drop": "queue.reason.restricted_drop",
	"streak":          "queue.reason.streak",
	"active_drop":     "queue.reason.active_drop",
	"fair_rotation":   "queue.reason.fair_rotation",
	"priority":        "queue.reason.priority",
	"discovery_fill":  "queue.reason.discovery_fill",
	"lower_priority":  "queue.reason.lower_priority",
}

// reasonCodeLabel resolves a closed-enum ReasonCode to localized, accessible
// text.
func reasonCodeLabel(tr func(string) string, code string) string {
	key, ok := reasonCodeKeys[code]
	if !ok {
		return tr("queue.reason.unknown")
	}
	return tr(key)
}

// ============================================================================
// Phase 4: C12 "exactly two slots".
// ============================================================================

// c12SlotData feeds the shared C12 slot component (Stage 4 §6). Fields match
// the spec's contract (Streamer?, ChannelStatus, ReasonCode, Uptime,
// PointsDelta, Active, EmptyReason?); HasX flags gate the "missing -> dash,
// never zero" rule (task Phase 4 item 7).
type c12SlotData struct {
	Occupied bool
	Channel  string

	// Unknown is true when the occupying channel's online status could not
	// be confirmed; the slot stays Occupied (never converted to Empty) - task
	// Phase 4 item 5. UnknownBadge reuses the existing C10 badge component
	// (never hand-rolled markup) for the visible "status unknown" marker.
	Unknown      bool
	UnknownBadge BadgeData

	// ReasonBadge/EmptyReasonBadge reuse the existing C10 badge component for
	// the reason chip - never linked to /help/glossary (deferred), never the
	// raw ReasonCode rendered as visible prose, only via ReasonCode's mono
	// title attribute for anyone who wants the machine value.
	HasReasonCode bool
	ReasonCode    string
	ReasonLabel   string
	ReasonBadge   BadgeData
	HasCampaign   bool
	Campaign      string

	HasUptime bool
	Uptime    string

	HasPointsDelta bool
	PointsDelta    string

	// Active is the 2px brand edge - true ONLY when the occupant's online
	// status is positively confirmed (task Phase 4 item 6).
	Active bool

	// EmptyReasonMode is "idle" (machine evidence: Watching.Mode == "idle")
	// or "unknown" (S-UNK - no positive machine reason for this specific
	// empty slot; never a Waiting channel's ReasonCode - task Phase 4 item 8).
	// Only meaningful when !Occupied.
	EmptyReasonMode string

	Link string

	// ---- sidebar variant (Q3 MAJOR-2) ----------------------------------
	// The fields below are populated only by sidebarSlotData, for the pinned
	// "Now Watching" sidebar's occupied entries - c12EntryFor (the /overview
	// and /overview/queue builder) never sets them, so they stay at their
	// zero value there and the extra markup they gate simply never renders
	// on those pages. This is what lets the sidebar keep its own richer
	// evidence (current point balance, game, streak progress, discovery
	// origin) while going through the exact same c12.slot template as every
	// other occupied/empty slot in the product, instead of a second,
	// independent rendering path.
	HasPoints bool
	Points    string
	HasGame   bool
	Game      string
	// Discovery is true for a discovery-sourced sidebar occupant (not on the
	// configured streamer list): the metrics row and streak bar are omitted
	// (no analytics history exists for it), matching the old partial's exact
	// behavior for Origin=="discovery".
	Discovery        bool
	StreakPending    bool
	StreakMinutes    int
	StreakCapMinutes int
	StreakPercent    int
}

// c12Pair always returns EXACTLY two entries (task Phase 4 items 1-4):
// occupied channels first, from evidence.Slots and capped at 2 even if a
// fake/misbehaving provider returned more, padded with explicit empty
// entries to reach 2. byName/stats provide the rich per-channel enrichment
// (uptime, points delta, confirmed-online status) from the caller's own
// already-trusted streamer snapshot - c12Pair itself reads only evidence for
// occupancy/reason, never any free-form text.
func c12Pair(evidence watchSlotEvidence, byName map[string]*models.Streamer, stats map[string]streamerStats, tr func(string) string) [2]c12SlotData {
	var pair [2]c12SlotData
	n := 0
	for _, sl := range evidence.Slots {
		if n >= 2 {
			break // never a third slot, even if a fake provider returns more
		}
		pair[n] = c12EntryFor(sl, byName[sl.Channel], stats, tr)
		n++
	}

	emptyMode := "unknown"
	if evidence.Mode == "idle" {
		emptyMode = "idle"
	}
	for ; n < 2; n++ {
		pair[n] = c12SlotData{EmptyReasonMode: emptyMode, Link: "/overview/queue"}
	}
	return pair
}

// c12PairProvenance builds the C0 provenance chip for the /overview C12 pair
// (Q3 MAJOR-1 item 6) from the same watch-slot evidence its occupants come
// from: EvaluatedAt is the watch broker's own snapshot time - real evidence,
// never the HTTP request clock - so the chip's age reflects how stale the
// underlying broker tick actually is, not just how recently the page/partial
// happened to render. Unknown is the honest S-UNK state when no evidence
// exists yet (nil provider, zero EvaluatedAt). No "Aged" threshold is
// established anywhere in the codebase for watch-slot evidence specifically
// (unlike the grid's ov-stale-badge, which derives one from
// overviewPollSeconds), so Aged is left at its honest zero value rather than
// inventing one; this only ever needs the fields ProvenanceChipData already
// declares, so C0's contract is unchanged.
func c12PairProvenance(evidence watchSlotEvidence) ProvenanceChipData {
	if evidence.EvaluatedAt.IsZero() {
		return ProvenanceChipData{Unknown: true}
	}
	return ProvenanceChipData{AgeLabel: util.FormatDuration(time.Since(evidence.EvaluatedAt))}
}

// ============================================================================
// Q3 MAJOR-2: the sidebar's occupied slots go through the same c12.slot
// component /overview and /overview/queue already use, instead of the
// legacy .watch-slot markup - one implementation, everywhere.
// ============================================================================

// sidebarSlotData converts one occupied WatchSlotView (built by
// buildNowWatching, unchanged) into the shared c12SlotData the sidebar
// renders through, registered as the "sidebarSlot" template func (server.go)
// so now_watching.html can call it inline per-slot without buildNowWatching
// itself needing to change shape - existing tests construct NowWatchingView
// by hand with []WatchSlotView, so that field/type stays exactly as is.
//
// Active is always true and Unknown always false: the sidebar has never
// distinguished a positively-confirmed-online occupant from an unconfirmed
// one (this predates that concept entirely - buildNowWatching derives
// occupancy from slots.Watching alone, no models.Streamer status check), so
// this preserves its exact existing visual meaning rather than inventing a
// new status signal. HasPointsDelta/PointsDelta reuse the exact "+N/h"
// format c12EntryFor already produces for the identical points-gain-rate
// concept (WatchSlotView.GainPerHour) - not a new field.
func sidebarSlotData(s WatchSlotView) c12SlotData {
	discovery := s.Origin == "discovery"
	entry := c12SlotData{
		Occupied:  true,
		Channel:   s.Name,
		Active:    !discovery,
		Discovery: discovery,
		Link:      "/overview/queue",
		HasGame:   s.Game != "",
		Game:      s.Game,
	}
	if !discovery {
		entry.HasPoints = s.Points != ""
		entry.Points = s.Points
		entry.HasPointsDelta = s.HasGain
		if s.HasGain {
			entry.PointsDelta = "+" + s.GainPerHour + "/h"
		}
		entry.StreakPending = s.StreakPending
		entry.StreakMinutes = s.StreakMinutes
		entry.StreakCapMinutes = s.StreakCapMinutes
		entry.StreakPercent = s.StreakPercent
	}
	return entry
}

// c12EntryFor builds one occupied slot's C12 data from safe evidence plus (if
// available) the streamer's own in-memory state.
func c12EntryFor(sl watchSlotEntry, st *models.Streamer, stats map[string]streamerStats, tr func(string) string) c12SlotData {
	entry := c12SlotData{
		Occupied: true,
		Channel:  sl.Channel,
		Link:     "/overview/queue",
	}
	entry.HasReasonCode = sl.ReasonCode != ""
	if entry.HasReasonCode {
		entry.ReasonCode = sl.ReasonCode
		entry.ReasonLabel = reasonCodeLabel(tr, sl.ReasonCode)
		entry.ReasonBadge = BadgeData{Tier: "neutral", Label: entry.ReasonLabel}
	}
	entry.HasCampaign = sl.Campaign != ""
	entry.Campaign = sl.Campaign

	if st == nil {
		// Discovery-occupied slot: the channel is not on the configured
		// streamer list, so no uptime/points evidence exists - dash, never 0.
		return entry
	}

	status := st.GetStatus()
	entry.Unknown = status == models.StatusUnknown
	if entry.Unknown {
		entry.UnknownBadge = BadgeData{Tier: "neutral", Label: tr("card.status_unknown")}
	}
	entry.Active = status == models.StatusOnline
	if status == models.StatusOnline {
		entry.HasUptime = true
		entry.Uptime = util.FormatDuration(time.Since(st.GetOnlineAt()))
	}
	if cs, ok := stats[sl.Channel]; ok && cs.hasRate {
		entry.HasPointsDelta = true
		entry.PointsDelta = "+" + util.FormatNumber(cs.pointsPerHour) + "/h"
	}
	return entry
}

// streamersByName indexes streamers by username for O(1) enrichment lookups.
func streamersByName(streamers []*models.Streamer) map[string]*models.Streamer {
	out := make(map[string]*models.Streamer, len(streamers))
	for _, st := range streamers {
		out[st.GetUsername()] = st
	}
	return out
}

// ============================================================================
// Phase 6: full roster (queue page).
// ============================================================================

// queueRosterRow is one row of the /overview/queue full-roster table/cards.
// It carries NO queue-position field (task Phase 6: "no fabricated queue
// number" - the repository does not establish a stable authoritative
// position contract); sort/filter order is client-side only.
type queueRosterRow struct {
	Channel string

	// Status/StatusLabel mirror StreamerInfo.State (watching/queued/offline/
	// disabled/unknown) - the same tri-state+disabled model buildCards
	// already produces from the COMPLETE configured streamer list, never a
	// filtered subset (live-only/waiting-only/online-only). StatusBadge reuses
	// the existing C10 badge component (icon+text+tier, never color alone);
	// "unknown" is deliberately never tier "ok" (S-UNK invariant: unknown
	// never reads as healthy).
	Status      string
	StatusLabel string
	StatusBadge BadgeData

	HasReasonCode bool
	ReasonCode    string
	ReasonLabel   string

	HasPoints      bool
	Points         string
	PointsRaw      int
	HasPointsToday bool
	PointsToday    string
	PointsTodayRaw int

	DisableWatch bool
}

// rosterStatusKeys maps buildCards' closed StreamerInfo.State set to its
// i18n key.
var rosterStatusKeys = map[string]string{
	"watching": "queue.status.watching",
	"queued":   "queue.status.queued",
	"offline":  "queue.status.offline",
	"disabled": "queue.status.disabled",
	"unknown":  "queue.status.unknown",
}

// rosterStatusTiers maps the same closed state set to a C10 badge tier.
// "unknown" is deliberately "neutral", never "ok" (S-UNK invariant).
var rosterStatusTiers = map[string]string{
	"watching": "ok",
	"queued":   "info",
	"offline":  "neutral",
	"disabled": "neutral",
	"unknown":  "neutral",
}

// rosterStatusIcons mirrors the existing card icon vocabulary (overview_live.html)
// so the roster's status badge is visually consistent with the streamer cards.
var rosterStatusIcons = map[string]string{
	"watching": "▶",
	"queued":   "◷",
	"offline":  "●",
	"disabled": "⊘",
	"unknown":  "?",
}

// buildQueueRoster flattens the COMPLETE configured streamer roster (via the
// existing buildCards live/unknown/offline groups - never LiveCards-only,
// Waiting-only, eligible-only, or online-only) into roster rows, enriched
// with the S5-3 safe adapter's ReasonCode evidence for whichever channels
// currently hold or are waiting for a slot. A channel neither occupied nor
// waiting simply has HasReasonCode=false (rendered as "-", never a
// fabricated code).
func (s *Server) buildQueueRoster(streamers []*models.Streamer, slots WatchSlotsView, stats map[string]streamerStats, evidence watchSlotEvidence, tr func(string) string) []queueRosterRow {
	live, unknown, offline, untracked, _ := s.buildCards(streamers, slots, stats, map[string]bool{}, tr)

	all := make([]StreamerInfo, 0, len(live)+len(unknown)+len(offline)+len(untracked))
	all = append(all, live...)
	all = append(all, unknown...)
	all = append(all, offline...)
	all = append(all, untracked...)

	rows := make([]queueRosterRow, 0, len(all))
	for _, info := range all {
		row := queueRosterRow{
			Channel:      info.Name,
			Status:       info.State,
			StatusLabel:  tr(rosterStatusKeys[info.State]),
			StatusBadge:  BadgeData{Tier: rosterStatusTiers[info.State], Icon: rosterStatusIcons[info.State], Label: tr(rosterStatusKeys[info.State])},
			DisableWatch: info.DisableWatch,
		}
		if sl, ok := evidence.slotFor(info.Name); ok {
			row.HasReasonCode = sl.ReasonCode != ""
			row.ReasonCode = sl.ReasonCode
			if row.HasReasonCode {
				row.ReasonLabel = reasonCodeLabel(tr, sl.ReasonCode)
			}
		} else if wc, ok := evidence.waitingFor(info.Name); ok {
			row.HasReasonCode = wc.ReasonCode != ""
			row.ReasonCode = wc.ReasonCode
			if row.HasReasonCode {
				row.ReasonLabel = reasonCodeLabel(tr, wc.ReasonCode)
			}
		}
		if info.PointsFormatted != "" {
			row.HasPoints = true
			row.Points = info.PointsFormatted
			row.PointsRaw = info.Points
		}
		if cs, ok := stats[info.Name]; ok {
			row.HasPointsToday = true
			row.PointsToday = info.PointsToday
			row.PointsTodayRaw = cs.pointsToday
		}
		rows = append(rows, row)
	}
	return rows
}

// sortRosterByChannel is used only to make roster fixtures/tests
// deterministic when a caller needs a stable order beyond buildCards' own
// (config-order) grouping; production rendering relies on client-side sort.
func sortRosterByChannel(rows []queueRosterRow) {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Channel < rows[j].Channel })
}
