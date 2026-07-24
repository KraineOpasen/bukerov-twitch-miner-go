package journal

// SlotEventType is the kind of watch-slot lifecycle transition recorded. The
// values are stable, redacted codes safe to surface in a diagnostic snapshot.
type SlotEventType string

const (
	// SlotEntered: a streamer was newly granted one of the (at most two) Twitch
	// watch slots this tick.
	SlotEntered SlotEventType = "entered"
	// SlotReasonChanged: a streamer kept its slot but the reason it holds it
	// changed (e.g. priority -> streak pursuit). NOT a release+enter.
	SlotReasonChanged SlotEventType = "reason_changed"
	// SlotReleased: a streamer gave up its slot with no correlated replacement
	// this tick.
	SlotReleased SlotEventType = "released"
	// SlotReplaced: within one tick exactly one streamer left a slot and exactly
	// one took one — a deterministic victim -> replacement rotation/eviction,
	// correlated at the moment of the swap (not inferred by diffing snapshots).
	SlotReplaced SlotEventType = "replaced"
	// SlotContinuityReset: a slot's watch-streak/session continuity accumulator
	// was reset (e.g. the slot was lost for a tick, or a streak pursuit timed
	// out). Recorded once per reset with a bounded reason.
	SlotContinuityReset SlotEventType = "continuity_reset"
	// SlotDeliverySuccess: the FIRST successful minute-watched beacon transport
	// of this residence — proof the slot began earning at the client-transport
	// level. Subsequent successes only increment the residence counter (they are
	// not each recorded, to keep the journal to meaningful transitions, not
	// per-tick noise). A successful transport delivery is NOT a Twitch
	// points-earned confirmation; actual point credit is confirmed separately via
	// PubSub points-earned events.
	SlotDeliverySuccess SlotEventType = "delivery_success"
	// SlotDeliveryFailure: a minute-watched beacon send failed at a bounded,
	// redacted stage. Recorded per failure (failures are rare and meaningful).
	SlotDeliveryFailure SlotEventType = "delivery_failure"
)

// SlotEvent is one watch-slot lifecycle diagnostic record. It contains ONLY
// value-typed, privacy-safe fields — canonical logins, stable IDs, bounded
// reason/stage codes, counters, and a residence duration in seconds. It never
// carries a token, cookie, header, signed playback/spade URL, request payload,
// or raw error. Fields not relevant to a given Type are left zero.
type SlotEvent struct {
	Type SlotEventType `json:"type"`

	// Identity of the streamer this event concerns.
	Channel   string `json:"channel"`             // canonical login (mutable via rename)
	ChannelID string `json:"channelId,omitempty"` // stable immutable Twitch channel ID
	Broadcast string `json:"broadcast,omitempty"` // stream.id of the current broadcast
	Origin    string `json:"origin,omitempty"`    // configured / discovery
	// SlotIndex is the streamer-list arbitration index (>=0 for a configured
	// channel, -1 for a discovery channel). It is a diagnostic hint, not a fixed
	// hardware slot position (Twitch exposes only "up to two" concurrent slots).
	SlotIndex int `json:"slotIndex,omitempty"`

	// Reason transition. Reason is the current bounded reason code; PrevReason is
	// the prior code on a reason_changed event.
	Reason     string `json:"reason,omitempty"`
	PrevReason string `json:"prevReason,omitempty"`

	// Replacement correlation (Type == SlotReplaced): the victim that left the
	// slot the replacement took.
	Victim   string `json:"victim,omitempty"`   // canonical login of the displaced streamer
	VictimID string `json:"victimId,omitempty"` // stable channel ID of the victim

	// Residence accounting, carried on terminal events (released / replaced).
	ResidenceSeconds float64 `json:"residenceSeconds,omitempty"` // how long the streamer held the slot
	Successes        int     `json:"successes,omitempty"`        // successful transport deliveries during residence
	Failures         int     `json:"failures,omitempty"`         // failed deliveries during residence

	// Delivery event fields (delivery_success / delivery_failure). Stage/Status/
	// ErrorCode come straight from the already-redacted WatchFailure surface.
	Stage     string `json:"stage,omitempty"`     // bounded transport-stage code (failure)
	Status    int    `json:"status,omitempty"`    // HTTP status at a failing request, 0 if none
	ErrorCode string `json:"errorCode,omitempty"` // bounded stage+status error code (failure)

	// ResetReason is the bounded reason for a continuity_reset (e.g. "slot_lost").
	ResetReason string `json:"resetReason,omitempty"`
}
