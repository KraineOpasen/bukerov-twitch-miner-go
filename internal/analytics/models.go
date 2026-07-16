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

// PointsHistory is the response shape for the statistics points-history
// endpoint: a balance series plus event annotations over a time range.
type PointsHistory struct {
	Streamer    string             `json:"streamer"`
	Range       string             `json:"range"`
	Points      []PointSample      `json:"points"`
	Annotations []AnnotationRecord `json:"annotations"`
	// Truncated is true when the raw series exceeded the point cap and was
	// downsampled for the chart (the export endpoint returns full fidelity).
	Truncated bool `json:"truncated"`
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
