package api

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
)

// TestIsTransientGQLStatus pins the retry classification: only 429 and 5xx are
// transient; ordinary 4xx (auth/logic) and 2xx are not, so they never incur a
// backoff-and-retry storm.
func TestIsTransientGQLStatus(t *testing.T) {
	transient := []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout}
	for _, s := range transient {
		if !isTransientGQLStatus(s) {
			t.Errorf("status %d should be transient", s)
		}
	}
	notTransient := []int{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound}
	for _, s := range notTransient {
		if isTransientGQLStatus(s) {
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
			if got := parseRetryAfter(tc.in, now); got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
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
		w, via := gqlRetryWait(attempt, 0)
		if via != "backoff" {
			t.Errorf("attempt %d: via = %q, want backoff", attempt, via)
		}
		base := gqlBaseBackoff * time.Duration(1<<uint(attempt))
		if base > gqlMaxBackoff {
			base = gqlMaxBackoff
		}
		// jitter adds up to base/2+1.
		if w < base || w > base+base/2+time.Millisecond {
			t.Errorf("attempt %d: backoff %v out of [%v, %v]", attempt, w, base, base+base/2)
		}
	}

	// Retry-After present: honored (plus bounded jitter), labelled "retry-after".
	w, via := gqlRetryWait(0, 3*time.Second)
	if via != "retry-after" {
		t.Errorf("via = %q, want retry-after", via)
	}
	if w < 3*time.Second || w >= 3*time.Second+gqlBaseBackoff {
		t.Errorf("retry-after wait %v out of [3s, 3s+base)", w)
	}

	// Retry-After larger than the cap is clamped to the cap.
	w, via = gqlRetryWait(0, 10*time.Minute)
	if via != "retry-after" {
		t.Errorf("via = %q, want retry-after", via)
	}
	if w < gqlRetryAfterCap || w >= gqlRetryAfterCap+gqlBaseBackoff {
		t.Errorf("clamped retry-after wait %v out of [cap, cap+base)", w)
	}
}

// TestGQLSuccessNoRetryNoBackoff proves a normal 200 response is returned on the
// first try, with no backoff and no extra requests — the "don't add backoff to
// successful responses" guard.
func TestGQLSuccessNoRetryNoBackoff(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	})

	start := time.Now()
	if _, err := c.PostGQL(constants.Inventory); err != nil {
		t.Fatalf("PostGQL: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("request count = %d, want exactly 1 (no retry on success)", n)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Errorf("success path took %v — no backoff should be incurred", elapsed)
	}
}

// TestGQLRetriesTransientThenSucceeds proves a transient 503 is retried (with
// the bounded backoff) and the eventual 200 is returned — the request is not
// abandoned on a single transient blip.
func TestGQLRetriesTransientThenSucceeds(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	})

	resp, err := c.PostGQL(constants.Inventory)
	if err != nil {
		t.Fatalf("PostGQL after one transient failure: %v", err)
	}
	if resp["data"] == nil {
		t.Errorf("expected data on the retried success, got %+v", resp)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("request count = %d, want 2 (one transient + one success)", n)
	}
}

// TestGQLRetryAfterHonored proves a 429 with Retry-After parks the retry for at
// least the header's duration — longer than the ~500ms base backoff would — so
// the server's throttling hint is actually respected.
func TestGQLRetryAfterHonored(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-based; skipped in -short")
	}
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	})

	start := time.Now()
	if _, err := c.PostGQL(constants.Inventory); err != nil {
		t.Fatalf("PostGQL after 429+Retry-After: %v", err)
	}
	elapsed := time.Since(start)
	// The 1s Retry-After must dominate the ≤750ms base backoff, proving it was
	// honored rather than ignored in favour of the computed backoff.
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed %v < 900ms — Retry-After: 1 was not honored", elapsed)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("request count = %d, want 2", n)
	}
}
