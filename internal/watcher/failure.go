package watcher

import (
	"fmt"
	"net/http"
)

// StatusClass buckets an HTTP status into a stable, low-cardinality class for
// privacy-safe recovery signatures. It never carries anything beyond the status
// itself (already deemed safe to surface, unlike URLs/tokens/bodies).
func StatusClass(status int) string {
	switch {
	case status == 0:
		return ""
	case status == http.StatusTooManyRequests:
		return "http_429"
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	default:
		return "http_other"
	}
}

// RecoverySignature is a stable, privacy-safe fingerprint of a watch-transport
// recovery episode. It is small enough to correlate a session-refresh request
// with its published outcome and to dedup repeated identical failures, and it
// carries ONLY safe identity/classification fields — never a playback token,
// signature, spade URL, playlist/segment URL, encoded payload, cookie, OAuth
// token, request/response body, or header.
//
// Two failures with the same signature describe the same recovery need (same
// channel, same broadcast, same session generation, same failing stage and status
// class, same recovery mode), so staging one refresh for them is correct and a
// duplicate must not create duplicate work.
type RecoverySignature struct {
	Login             string
	BroadcastID       string
	SessionGeneration uint64
	Stage             ProbeStage
	StatusClass       string
	ErrorCode         string
	Mode              RefreshMode
}

// String is the stable dedup key. It is composed only of the safe fields above,
// so it can be logged and compared freely.
func (s RecoverySignature) String() string {
	return fmt.Sprintf("%s|%s|g%d|%s|%s|%s|%s",
		s.Login, s.BroadcastID, s.SessionGeneration, s.Stage, s.StatusClass, s.ErrorCode, s.Mode)
}
