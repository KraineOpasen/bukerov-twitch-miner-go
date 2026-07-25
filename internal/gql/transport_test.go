package gql

import (
	"net/http"
	"testing"
	"time"
)

// TestIsTransientGQLStatus pins the retry classification: only 429 and 5xx are
// transient; ordinary 4xx (auth/logic) and 2xx are not, so they never incur a
// backoff-and-retry storm.
func TestIsTransientGQLStatus(t *testing.T) {
	transient := []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout}
	for _, s := range transient {
		if !IsTransientStatus(s) {
			t.Errorf("status %d should be transient", s)
		}
	}
	notTransient := []int{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound}
	for _, s := range notTransient {
		if IsTransientStatus(s) {
			t.Errorf("status %d should NOT be transient (no retry storm on non-transient errors)", s)
		}
	}
}

// TestParseRetryAfter covers the Retry-After header forms: integer seconds, an
// HTTP-date in the future, and every shape that must fall back to 0 (absent,
// blank, malformed, non-positive, a date in the past).
func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"seconds", "5", 5 * time.Second},
		{"seconds with padding", "  30 ", 30 * time.Second},
		{"zero seconds", "0", 0},
		{"negative seconds", "-3", 0},
		{"absent", "", 0},
		{"malformed", "soon", 0},
		{"future http-date", now.Add(10 * time.Second).UTC().Format(http.TimeFormat), 10 * time.Second},
		{"past http-date", now.Add(-10 * time.Second).UTC().Format(http.TimeFormat), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseRetryAfter(tc.in, now); got != tc.want {
				t.Errorf("ParseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestGQLRetryWait proves the wait selection: a server Retry-After wins over the
// computed backoff and is clamped to the cap, while its absence falls back to
// bounded exponential backoff. Jitter is bounded, so the assertions use ranges.
func TestGQLRetryWait(t *testing.T) {
	// No Retry-After: exponential backoff, labelled "backoff".
	for attempt := 0; attempt <= 4; attempt++ {
		w, via := RetryWait(attempt, 0)
		if via != "backoff" {
			t.Errorf("attempt %d: via = %q, want backoff", attempt, via)
		}
		base := BaseBackoff * time.Duration(1<<uint(attempt))
		if base > MaxBackoff {
			base = MaxBackoff
		}
		// jitter adds up to base/2+1.
		if w < base || w > base+base/2+time.Millisecond {
			t.Errorf("attempt %d: backoff %v out of [%v, %v]", attempt, w, base, base+base/2)
		}
	}

	// Retry-After present: honored (plus bounded jitter), labelled "retry-after".
	w, via := RetryWait(0, 3*time.Second)
	if via != "retry-after" {
		t.Errorf("via = %q, want retry-after", via)
	}
	if w < 3*time.Second || w >= 3*time.Second+BaseBackoff {
		t.Errorf("retry-after wait %v out of [3s, 3s+base)", w)
	}

	// Retry-After larger than the cap is clamped to the cap.
	w, via = RetryWait(0, 10*time.Minute)
	if via != "retry-after" {
		t.Errorf("via = %q, want retry-after", via)
	}
	if w < RetryAfterCap || w >= RetryAfterCap+BaseBackoff {
		t.Errorf("clamped retry-after wait %v out of [cap, cap+base)", w)
	}
}

// TestIsPersistedQueryNotFound covers the single- and batched-response error
// shapes that must be recognized as a stale persisted-query hash, and the
// success/unrelated-error shapes that must not.
func TestIsPersistedQueryNotFound(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "single response errors[].message",
			body: `{"errors":[{"message":"PersistedQueryNotFound"}]}`,
			want: true,
		},
		{
			name: "errorType field",
			body: `[{"errors":[{"message":"...","extensions":{"code":"PERSISTED_QUERY_NOT_FOUND"}}],"errorType":"PersistedQueryNotFound"}]`,
			want: true,
		},
		{
			name: "batched response with one failing operation",
			body: `[{"data":{"user":{"id":"1"}}},{"errors":[{"message":"PersistedQueryNotFound"}]}]`,
			want: true,
		},
		{
			name: "normal success",
			body: `{"data":{"user":{"id":"123"}}}`,
			want: false,
		},
		{
			name: "unrelated error",
			body: `{"errors":[{"message":"service unavailable"}]}`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPersistedQueryNotFound([]byte(tc.body)); got != tc.want {
				t.Errorf("IsPersistedQueryNotFound(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
