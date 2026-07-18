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
// change (e.g. "WATCH", "CLAIM") shown in the chart tooltip.
type PointSample struct {
	T       int64  `json:"t"`
	Balance int    `json:"balance"`
	Reason  string `json:"reason,omitempty"`
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
// endpoint: how many points arrived under one reason code (WATCH, CLAIM,
// RAID, WATCH_STREAK, ...) within the requested range, and how many positive
// balance changes carried that reason.
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
	// Breakdown aggregates the range's positive balance changes by canonical
	// reason so the dashboard can chart WATCH/CLAIM/RAID/WATCH_STREAK/PREDICTION
	// shares. Computed from the raw (pre-downsampling) series; omitted when there
	// is nothing earned in range.
	Breakdown []ReasonShare `json:"breakdown,omitempty"`
	// BetSummary is the prediction-betting accounting (won/staked/refunded/net)
	// for the same streamer and window as the series, shown next to the earnings
	// donut so the PREDICTION slice's origin is explicit. Nil/omitted when there
	// are no bets in range. Derived from BetRecords, so it agrees with the ROI
	// section for an equivalent window (only the window differs).
	BetSummary *BetSummary `json:"betSummary,omitempty"`
	// RawTruncated is true when the raw series hit the backend row cap, so
	// the window is incomplete and Breakdown (and any KPI derived from it)
	// must not be presented as a full-period result.
	RawTruncated bool `json:"rawTruncated"`
	// ChartDownsampled is true when Points was thinned for display only; the
	// raw series (and therefore Breakdown) is still complete. Deliberately a
	// separate flag from RawTruncated: downsampling alone must never hide the
	// breakdown or trigger a partial-data warning.
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
