// Package gql holds generic GraphQL/HTTP transport primitives shared by the
// Twitch client: transient-status classification, persisted-query and
// top-level-error detection, and the retry/backoff timing helpers. It is
// deliberately Twitch-agnostic — it knows nothing about Twitch endpoints,
// client IDs, headers, auth, or domain operations — so it depends only on the
// standard library. The Twitch client (internal/twitch) imports this package;
// this package never imports it.
package gql

import (
	"bytes"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// BaseBackoff is the base delay of the exponential backoff between GQL
	// request retries.
	BaseBackoff = 500 * time.Millisecond
	// MaxBackoff caps the exponential backoff delay.
	MaxBackoff = 8 * time.Second

	// RetryAfterCap bounds how long a server-supplied Retry-After header is
	// honored, so a pathological or hostile value can never park a request for
	// minutes. When Twitch returns 429 with Retry-After, that hint is authoritative
	// and used in place of the computed exponential backoff (clamped to this cap);
	// a small jitter is still added so a fleet of requests doesn't resume in lockstep.
	RetryAfterCap = 30 * time.Second
)

// IsTransientStatus reports whether an HTTP status code represents a
// transient failure worth retrying (rate limiting or server-side errors).
// 4xx errors other than 429 (bad auth, bad request, etc.) are not retried
// since retrying them would just repeat the same failure.
func IsTransientStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError
}

// HasTopLevelErrors reports whether a decoded GQL response carries a
// non-empty top-level "errors" array — the GraphQL signal that the operation
// failed at the GQL layer (including PersistedQueryNotFound) and returned no
// authoritative data, regardless of HTTP status.
func HasTopLevelErrors(result map[string]interface{}) bool {
	errs, ok := result["errors"].([]interface{})
	return ok && len(errs) > 0
}

// IsPersistedQueryNotFound reports whether a GQL response body carries a
// PersistedQueryNotFound error. Twitch returns this (typically with HTTP 200)
// when the persisted-query sha256 hash it has on record for the given Client-Id
// no longer matches — usually because Twitch rotated/invalidated the hashes or
// the client ID itself. It is detected via a raw substring match so it works
// for both single and batched responses regardless of the exact error shape
// (errors[].message vs errorType).
func IsPersistedQueryNotFound(respBody []byte) bool {
	return bytes.Contains(respBody, []byte("PersistedQueryNotFound"))
}

// ParseRetryAfter interprets an HTTP Retry-After header value, which is either a
// non-negative integer number of seconds or an HTTP-date. It returns the delay
// (never negative), or 0 when the header is absent, malformed, or in the past —
// so a bad value simply falls back to the computed backoff rather than skewing
// the wait.
func ParseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// RetryWait picks the delay before the next retry and a label for why. A
// server-supplied Retry-After (from a 429) is authoritative and wins over the
// computed backoff: it is clamped to RetryAfterCap (so a hostile/huge value
// can't park the request) and given a small jitter so a fleet doesn't resume in
// lockstep. Without one, it falls back to bounded exponential backoff with
// jitter. Extracted (and jitter-bounded) so the selection is unit-testable
// without sleeping.
func RetryWait(attempt int, retryAfter time.Duration) (time.Duration, string) {
	if retryAfter > 0 {
		if retryAfter > RetryAfterCap {
			retryAfter = RetryAfterCap
		}
		return retryAfter + time.Duration(rand.Int63n(int64(BaseBackoff))), "retry-after"
	}
	return BackoffDuration(attempt), "backoff"
}

// BackoffDuration returns the exponential backoff delay (with jitter) for
// the given zero-based retry attempt, capped at MaxBackoff.
func BackoffDuration(attempt int) time.Duration {
	backoff := BaseBackoff * time.Duration(1<<uint(attempt))
	if backoff > MaxBackoff {
		backoff = MaxBackoff
	}
	jitter := time.Duration(rand.Int63n(int64(backoff)/2 + 1))
	return backoff + jitter
}
