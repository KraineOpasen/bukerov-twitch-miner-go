package analytics

type SeriesPoint struct {
	X int64  `json:"x"`
	Y int    `json:"y"`
	Z string `json:"z,omitempty"`
}

type Annotation struct {
	X           int64           `json:"x"`
	BorderColor string          `json:"borderColor"`
	Label       AnnotationLabel `json:"label"`
}

type AnnotationLabel struct {
	Style map[string]string `json:"style"`
	Text  string            `json:"text"`
}

type StreamerData struct {
	Series      []SeriesPoint `json:"series"`
	Annotations []Annotation  `json:"annotations"`
}

// PointSample is one balance-over-time reading returned by the points-history
// endpoint: T is a Unix-millis timestamp, Balance is the absolute channel-point
// balance snapshot recorded at that moment, Reason is the event that caused the
// change (e.g. "WATCH", "CLAIM") shown in the chart tooltip. Exact marks a
// sample that an exact point event produced (see PointEvent): its balance
// delta is accounted for by that event's event-local amount, so the legacy
// estimator never attributes it a second time. Samples recorded before the
// exact ledger existed, by a pre-ledger binary, or for a points-spent frame
// are not Exact.
type PointSample struct {
	T       int64  `json:"t"`
	Balance int    `json:"balance"`
	Reason  string `json:"reason,omitempty"`
	Exact   bool   `json:"exact,omitempty"`
}

// PointEvent is the immutable, event-local snapshot of ONE accepted Twitch
// points-earned event — the accounting fact the exact earnings ledger stores.
// Every value comes from the same PubSub frame that carried the event; none
// of it is re-read from the shared, mutable Streamer after admission:
//
//   - EventID is the exact event identity (the PubSub EventFingerprint: a
//     SHA-256 of topic + canonical inner message). UNIQUE in the ledger, so an
//     exact re-delivery of the same event is idempotent.
//   - ReasonCode is the raw Twitch point_gain.reason_code ("WATCH", "CLAIM",
//     "WATCH_STREAK", "RAID", "PREDICTION", ...), canonicalized only at
//     aggregation time.
//   - TotalPoints is the event-local point_gain.total_points — the amount
//     Twitch says THIS event granted. It is the only earning authority; a
//     balance delta between two snapshots never is.
//   - BalanceAfter is the balance.balance Twitch reported in the same frame,
//     when it carried one (BalanceKnown); it is informational and never used
//     to derive an earning.
//   - Timestamp is the Unix-millis time the event was accepted into the
//     ledger; zero lets the Service stamp its clock.
type PointEvent struct {
	EventID      string
	Timestamp    int64
	ReasonCode   string
	TotalPoints  int
	BalanceAfter int
	BalanceKnown bool
}

// PointEventAnnotation is the chart marker of a WATCH_STREAK or RAID grant,
// built from the event-local amount. For a ledger event RecordPointEvent
// writes it in the SAME transaction as the PointEvent, so the marker text and
// the ledger row always carry the identical amount and a duplicate event can
// never leave a second marker behind; for a frame the ledger could not admit
// RecordPointMarker writes it on its own, through the same fence and close
// barrier.
type PointEventAnnotation struct {
	EventType string
	Text      string
	Color     string
}

// ExactEarnings is the exact point-event aggregation over one streamer and
// time range, computed in SQL from the ledger (never from balance samples,
// never subject to the chart's raw-series row cap).
type ExactEarnings struct {
	// Breakdown sums each canonical reason's positive event-local amounts
	// (Gained) and counts its positive events (Count); sorted like the legacy
	// estimate for rendering. Nil when no positive event is in range.
	Breakdown []ReasonShare
	// Events is the number of ledger rows in range, positive or not.
	Events int
	// Since is the Unix-millis timestamp of the earliest ledger row in range
	// (0 when Events == 0): the point from which exact accounting exists in
	// the selected period. It is not a guarantee of continuous coverage —
	// samples a pre-ledger binary wrote after it (a rollback gap) still show
	// up as legacy, and the coverage classification says so.
	Since int64
}

// LegacyEstimate is the balance-delta ESTIMATE over the samples of a range
// that no exact event backs. It is an estimate by construction (a delta
// between two mutable balance snapshots, attributed to the later snapshot's
// reason), kept only so history recorded before the exact ledger existed
// stays visible; it is never added to exact figures.
type LegacyEstimate struct {
	// Breakdown attributes each positive delta into a legacy earning sample
	// to that sample's canonical reason. Nil when nothing could be attributed.
	Breakdown []ReasonShare
	// Samples counts the legacy EARNING samples in the window (not Exact, not
	// a points-spent snapshot): evidence that pre-ledger history is present
	// even when no positive delta could be attributed (e.g. a lone baseline).
	Samples int
}

// Earnings coverage values reported by EarningsAccounting.Coverage.
const (
	// EarningsCoverageExact: every earning in range is an exact ledger event.
	EarningsCoverageExact = "exact"
	// EarningsCoverageLegacy: no exact event in range; only the balance-delta
	// estimate is available.
	EarningsCoverageLegacy = "legacy"
	// EarningsCoverageMixed: exact events AND legacy history (or a legacy part
	// whose estimate is unavailable) share the range; the two are reported
	// separately and never summed.
	EarningsCoverageMixed = "mixed"
	// EarningsCoverageNone: no earning of either kind in range.
	EarningsCoverageNone = "none"
	// EarningsCoverageUnavailable: no exact event in range and the legacy
	// estimate could not be computed (raw series truncated).
	EarningsCoverageUnavailable = "unavailable"
)

// Legacy-estimate status values reported by EarningsAccounting.LegacyStatus.
const (
	LegacyStatusNone        = "none"        // no legacy earning sample in range
	LegacyStatusEstimated   = "estimated"   // LegacyBreakdown is a balance-delta estimate
	LegacyStatusUnavailable = "unavailable" // raw series truncated: the estimate cannot be computed
)

// EarningsAccounting is the additive points-history metadata that tells a
// consumer which accounting produced Breakdown and whether a separately
// estimated legacy part exists, so exact and estimated figures are never
// mistaken for one another — and never summed.
type EarningsAccounting struct {
	// Coverage is one of the EarningsCoverage* values.
	Coverage string `json:"coverage"`
	// Exact is true when Breakdown is the exact ledger aggregation.
	Exact bool `json:"exact"`
	// ExactSince is the Unix-millis timestamp of the earliest exact event in
	// range (omitted when there is none): where exact accounting starts, not
	// a promise that everything after it is exact — Coverage carries that.
	ExactSince int64 `json:"exactSince,omitempty"`
	// LegacyStatus is one of the LegacyStatus* values and qualifies
	// LegacyBreakdown.
	LegacyStatus string `json:"legacyStatus"`
}

// AnnotationRecord is a machine-readable event marker for the points-history
// endpoint: T is a Unix-millis timestamp, Type is the event type (e.g.
// WATCH_STREAK, WIN, LOSE), Reason is the human-readable description, and Color
// is the per-type marker colour persisted alongside the annotation (see
// analytics.RecordAnnotation). Carrying the colour lets the chart give every
// event type — WATCH_STREAK included — a hue distinct from the balance line
// without the template duplicating the type→colour map, so a new event type
// needs no front-end change.
type AnnotationRecord struct {
	T      int64  `json:"t"`
	Type   string `json:"type"`
	Reason string `json:"reason"`
	Color  string `json:"color"`
}

// ReasonShare is one slice of the earnings breakdown for the points-history
// endpoint: how many points arrived under one canonical reason (WATCH, CLAIM,
// RAID, WATCH_STREAK, PREDICTION, OTHER) within the requested range, and how
// many earning events (exact) or positive balance changes (legacy estimate)
// carried that reason.
type ReasonShare struct {
	Reason string `json:"reason"`
	Gained int    `json:"gained"`
	Count  int    `json:"count"`
}

// PointsHistory is the response shape for the statistics points-history
// endpoint: a balance series plus event annotations over a time range.
type PointsHistory struct {
	Streamer    string             `json:"streamer"`
	Range       string             `json:"range"`
	Points      []PointSample      `json:"points"`
	Annotations []AnnotationRecord `json:"annotations"`
	// Breakdown is the range's primary earnings attribution by canonical
	// reason (WATCH/CLAIM/RAID/WATCH_STREAK/PREDICTION/OTHER). When the range
	// holds exact point events it is the EXACT ledger aggregation
	// (Earnings.Exact == true) — event-local Twitch amounts, computed in SQL,
	// unaffected by RawTruncated; the legacy part of a mixed range then lives
	// only in LegacyBreakdown. When the range holds no exact event it is the
	// legacy balance-delta ESTIMATE (Earnings.Exact == false). Omitted when
	// there is nothing earned in range. The two accountings are never summed.
	Breakdown []ReasonShare `json:"breakdown,omitempty"`
	// LegacyBreakdown is the balance-delta ESTIMATE over the samples of the
	// range that no exact event backs (history recorded before the exact
	// ledger). Present only for a MIXED range (Earnings.Exact == true and
	// Earnings.LegacyStatus == "estimated") where something could be
	// attributed; it is reported beside the exact Breakdown, never added to
	// it. For a legacy-only range the estimate is Breakdown itself and is not
	// repeated here, so no consumer can double-count by summing the two.
	LegacyBreakdown []ReasonShare `json:"legacyBreakdown,omitempty"`
	// Earnings qualifies Breakdown/LegacyBreakdown: which accounting produced
	// them, from when the range is exactly covered, and whether the legacy
	// estimate exists, is unavailable (truncated), or is not needed.
	Earnings EarningsAccounting `json:"earnings"`
	// BetSummary is the prediction-betting accounting (won/staked/refunded/net)
	// for the same streamer and window as the series, shown next to the earnings
	// donut so the PREDICTION slice's origin is explicit. Nil/omitted when there
	// are no bets in range. Derived from BetRecords, so it agrees with the ROI
	// section for an equivalent window (only the window differs).
	BetSummary *BetSummary `json:"betSummary,omitempty"`
	// RawTruncated is true when the raw series hit the backend row cap, so
	// the balance window is incomplete: balance-derived KPIs and the legacy
	// estimate (Earnings.LegacyStatus == "unavailable") must not be presented
	// as full-period results. An exact Breakdown is unaffected — it is
	// aggregated from the ledger, not from the truncated series.
	RawTruncated bool `json:"rawTruncated"`
	// ChartDownsampled is true when Points was thinned for display only; the
	// raw series (and therefore the legacy estimate) is still complete.
	// Deliberately a separate flag from RawTruncated: downsampling alone must
	// never hide the breakdown or trigger a partial-data warning.
	ChartDownsampled bool `json:"chartDownsampled"`
}

type StreamerInfo struct {
	Name                  string `json:"name"`
	Points                int    `json:"points"`
	PointsFormatted       string `json:"points_formatted"`
	LastActivity          int64  `json:"last_activity"`
	LastActivityFormatted string `json:"last_activity_formatted"`
	IsLive                bool   `json:"is_live"`
	LiveDuration          string `json:"live_duration,omitempty"`
	OfflineDuration       string `json:"offline_duration,omitempty"`
}

type ChatMessage struct {
	ID          int64  `json:"id"`
	Timestamp   int64  `json:"timestamp"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Message     string `json:"message"`
	Emotes      string `json:"emotes,omitempty"`
	Badges      string `json:"badges,omitempty"`
	Color       string `json:"color,omitempty"`
}

type ChatLogData struct {
	Messages   []ChatMessage `json:"messages"`
	TotalCount int           `json:"total_count"`
	HasMore    bool          `json:"has_more"`
}

// BetRecord is one resolved prediction bet, persisted so ROI analytics survive
// restarts (the in-memory streamer.History and the lossy WIN/LOSE annotation
// string are not enough — neither carries the stake, payout, strategy, or
// odds). One row per event_id; a reconnect that re-delivers the same
// prediction-result must not double-count it.
//
// Placed is the raw stake put on the round (kept even for REFUND, where the
// stake is returned); Won is the payout (0 for LOSE/REFUND); Gained is the net
// (Won-Placed for WIN/LOSE, 0 for REFUND). Odds is the chosen outcome's odds at
// resolution. Manual marks a bet placed via the dashboard rather than auto-bet.
type BetRecord struct {
	EventID    string  `json:"eventId"`
	Streamer   string  `json:"streamer"`
	Timestamp  int64   `json:"t"`
	Strategy   string  `json:"strategy"`
	ResultType string  `json:"result"` // WIN | LOSE | REFUND
	Placed     int     `json:"placed"`
	Won        int     `json:"won"`
	Gained     int     `json:"gained"`
	Odds       float64 `json:"odds"`
	Manual     bool    `json:"manual"`
}

// BetSummary is a compact, sign-separated accounting of prediction betting over
// one selection, shown next to the earnings-breakdown donut so the "Prediction
// wins" slice (a gross positive channel-point credit) is never mistaken for an
// unexplained gain: it pairs the winnings with the stake put at risk, the stake
// refunded, and the net result. Derived from the same BetRecords as the ROI
// section (SummarizeBets), so the two can only differ by window, never
// contradict.
//
// Invariant: Net == Won - Staked == Σ BetRecord.Gained. Won is the GROSS payout
// credited on wins (stake*odds, which is exactly the positive balance delta the
// donut's PREDICTION slice sums), Staked is the stake on settled bets (WIN+LOSE;
// a refunded stake was returned, so it is reported separately, not staked). A
// prediction LOSS only ever reduces Net — it is never a positive figure.
type BetSummary struct {
	Wins     int `json:"wins"`
	Losses   int `json:"losses"`
	Refunds  int `json:"refunds"`
	Won      int `json:"won"`      // gross payout credited on wins (Σ Won over WIN)
	Staked   int `json:"staked"`   // stake risked on settled bets (Σ Placed over WIN+LOSE)
	Refunded int `json:"refunded"` // stake returned on refunds (Σ Placed over REFUND)
	Net      int `json:"net"`      // net betting result (Σ Gained; == Won - Staked)
	// Empty is true when the selection has no bet records, so the UI can hide
	// the betting summary entirely rather than render a row of zeros.
	Empty bool `json:"empty"`
}
