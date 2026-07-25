package twitch

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
)

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
