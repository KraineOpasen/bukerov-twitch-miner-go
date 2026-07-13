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
// WATCH_STREAK, WIN, LOSE), Reason is the human-readable description.
type AnnotationRecord struct {
	T      int64  `json:"t"`
	Type   string `json:"type"`
	Reason string `json:"reason"`
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
