package pubsub

import "strings"

// Canonical point-gain reason codes. Every runtime decision that keys off a
// Twitch community-points reason (watch-streak grant detection, analytics
// annotation, console/web-log classification, event-ring filtering, in-memory
// History keys) compares against one of these, never against the raw payload
// string. The raw wire value is preserved separately for forensic use (see
// PointReason / analytics event_type / the reason_raw log attribute).
const (
	ReasonWatch       = "WATCH"
	ReasonWatchStreak = "WATCH_STREAK"
	ReasonClaim       = "CLAIM"
	ReasonRaid        = "RAID"
)

// canonicalReasonTable maps the trimmed, upper-cased lookup key of a raw
// reason_code to its canonical runtime form. Twitch's watch-streak reason has
// been observed in our production data in the space form ("WATCH STREAK") while
// every internal comparison expects the underscore form ("WATCH_STREAK"); both
// map to the single canonical WATCH_STREAK so the grant is detected regardless
// of which form arrives. The mapping is an EXACT table on purpose — no
// substring/prefix match — so "WATCH" can never swallow "WATCH STREAK".
var canonicalReasonTable = map[string]string{
	"WATCH STREAK": ReasonWatchStreak,
	"WATCH_STREAK": ReasonWatchStreak,
	"WATCH":        ReasonWatch,
	"CLAIM":        ReasonClaim,
	"RAID":         ReasonRaid,
}

// CanonicalReasonCode maps a raw Twitch point-gain reason_code to its canonical
// runtime form. Normalization is deliberately limited to trimming surrounding
// whitespace plus an exact, case-insensitive table lookup. A reason not in the
// table is returned trimmed but otherwise unchanged — never coerced to WATCH or
// to a synthetic OTHER — so an unknown reason flows through the runtime without
// triggering any streak side effect and still logs a diagnosable value.
func CanonicalReasonCode(raw string) string {
	if canonical, ok := canonicalReasonTable[strings.ToUpper(strings.TrimSpace(raw))]; ok {
		return canonical
	}
	return strings.TrimSpace(raw)
}

// PointReason extracts the point-gain reason from a points-earned PubSubMessage:
// the raw payload string (unchanged, for analytics/forensics), its canonical
// runtime form, and whether a reason_code was present. It is the single
// extraction+normalization point shared by the pool's own handling path and the
// miner's onMessage callback, so neither reads point_gain["reason_code"] by hand
// and both derive the canonical form identically. msg.Data is never mutated.
func PointReason(msg *PubSubMessage) (raw, canonical string, ok bool) {
	if msg == nil || msg.Data == nil {
		return "", "", false
	}
	pointGain, ok := msg.Data["point_gain"].(map[string]interface{})
	if !ok {
		return "", "", false
	}
	rc, ok := pointGain["reason_code"].(string)
	if !ok {
		return "", "", false
	}
	return rc, CanonicalReasonCode(rc), true
}
