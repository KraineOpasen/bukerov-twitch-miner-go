package journal

// Stable, redacted string codes for connection-health journal fields. They are
// deliberately decoupled from the miner's internal enum values so the journal's
// vocabulary is a stable diagnostic contract, not a mirror of private constants.
const (
	// Aggregate connection levels.
	HealthLevelHealthy  = "healthy"
	HealthLevelDegraded = "degraded"
	HealthLevelLost     = "lost"

	// API-path sub-state codes.
	APIStateUnknown  = "unknown"
	APIStateIdle     = "idle"
	APIStateOK       = "ok"
	APIStateDegraded = "degraded"
	APIStateDown     = "down"

	// Evidence quality behind a transition.
	EvidenceAuthoritative = "authoritative" // rests on positive confirmed evidence
	EvidenceDegraded      = "degraded"      // rests on degraded-tier evidence (functional failures / reconnects)
	EvidenceInconclusive  = "inconclusive"  // rests on idle/unknown (no attempts, went quiet)

	// Recovery classification.
	RecoveryNone    = "none"
	RecoveryPartial = "partial" // LOST -> DEGRADED
	RecoveryFull    = "full"    // -> HEALTHY while a lost alert was outstanding

	// Bounded transition reason codes.
	HealthReasonEnteredLost     = "entered_lost"
	HealthReasonFullRestore     = "full_restore"
	HealthReasonPartialRestore  = "partial_restore"
	HealthReasonEnteredDegraded = "entered_degraded"
	HealthReasonStabilized      = "stabilized"
	HealthReasonLevelChanged    = "level_changed"
)

// HealthEventType is the kind of connection-health record. Only meaningful
// transitions are recorded; repeated identical states are deduped (never
// appended twice) and counted in SuppressedDuplicates on the next transition.
type HealthEventType string

const (
	// HealthTransition is a change in the aggregate connection-health level.
	HealthTransition HealthEventType = "transition"
)

// HealthEvent is one connection-health transition diagnostic record. It observes
// the outputs the health state machine already computed — it never reclassifies
// and never influences notification or dashboard decisions. It contains ONLY
// value-typed, privacy-safe fields; it never carries a token, cookie, header,
// URL, error body, OAuth datum, or notification transport secret.
type HealthEvent struct {
	Type HealthEventType `json:"type"`

	// Domain names the health domain this transition belongs to (e.g.
	// "connection" for the aggregate API+PubSub link).
	Domain string `json:"domain"`

	// PrevLevel/NewLevel are the aggregate levels (healthy/degraded/lost).
	PrevLevel string `json:"prevLevel"`
	NewLevel  string `json:"newLevel"`

	// APIState is the API-path sub-state; PubSubDown/PubSubDegraded are the
	// PubSub-path verdicts that fed the aggregate decision.
	APIState       string `json:"apiState,omitempty"`
	PubSubDown     bool   `json:"pubsubDown,omitempty"`
	PubSubDegraded bool   `json:"pubsubDegraded,omitempty"`

	// Evidence is the quality of the deciding evidence
	// (authoritative/degraded/inconclusive) — information the state machine
	// computes implicitly but does not label.
	Evidence string `json:"evidence"`

	// Recovery is the recovery classification (none/partial/full).
	Recovery string `json:"recovery"`

	// Reason is a bounded transition code (entered_lost, full_restore, ...).
	Reason string `json:"reason"`

	// NotificationEmitted is true when this transition triggered an external
	// (Discord/push) connection-lost or connection-restored notification.
	NotificationEmitted bool `json:"notificationEmitted,omitempty"`

	// SuppressedDuplicates is how many identical repeated health ticks were
	// deduped since the previous recorded transition — the dedupe count.
	SuppressedDuplicates int `json:"suppressedDuplicates,omitempty"`
}
