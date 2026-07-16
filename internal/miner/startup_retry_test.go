package miner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
)

// shrinkStartupBackoff replaces the real backoff schedule with a tiny one for
// the duration of a test, so retry loops complete in milliseconds.
func shrinkStartupBackoff(t *testing.T) {
	t.Helper()
	saved := startupBackoffSchedule
	startupBackoffSchedule = []time.Duration{time.Millisecond}
	t.Cleanup(func() { startupBackoffSchedule = saved })
}

// TestRetryStartupLookupRetriesPQNFUntilSuccess: a stale-hash outage
// (ErrPersistedQueryNotFound) must be retried — here it clears on the third
// attempt — and every retry must be reported through onAttempt.
func TestRetryStartupLookupRetriesPQNFUntilSuccess(t *testing.T) {
	shrinkStartupBackoff(t)

	calls := 0
	fetch := func() (string, error) {
		calls++
		if calls < 3 {
			return "", fmt.Errorf("%w: operation GetIDFromLogin (tried 3 client IDs)", api.ErrPersistedQueryNotFound)
		}
		return "user-123", nil
	}

	attempts := 0
	id, err := retryStartupLookup(context.Background(), fetch, func(attempt int, err error, next time.Duration) {
		attempts++
		if !errors.Is(err, api.ErrPersistedQueryNotFound) {
			t.Errorf("onAttempt got unexpected error: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if id != "user-123" {
		t.Fatalf("expected user-123, got %q", id)
	}
	if calls != 3 {
		t.Fatalf("expected 3 fetch calls, got %d", calls)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 onAttempt notifications, got %d", attempts)
	}
}

// TestRetryStartupLookupRetriesUnknownErrors: transport exhaustion has no
// sentinel to match on, so the loop's default posture must be to retry
// anything that is not an explicit fail-fast error.
func TestRetryStartupLookupRetriesUnknownErrors(t *testing.T) {
	shrinkStartupBackoff(t)

	calls := 0
	fetch := func() (string, error) {
		calls++
		if calls < 2 {
			return "", fmt.Errorf("gql request failed after 5 attempts: connection reset")
		}
		return "user-123", nil
	}

	id, err := retryStartupLookup(context.Background(), fetch, nil)
	if err != nil || id != "user-123" {
		t.Fatalf("expected success after retrying an unknown error, got id=%q err=%v", id, err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 fetch calls, got %d", calls)
	}
}

// TestRetryStartupLookupFailsFast: a rejected token and a genuinely unknown
// login cannot be cured by retrying — the loop must return immediately after a
// single fetch, preserving the error's identity for the caller.
func TestRetryStartupLookupFailsFast(t *testing.T) {
	for _, sentinel := range []error{api.ErrUnauthorized, api.ErrStreamerDoesNotExist} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			calls := 0
			fetch := func() (string, error) {
				calls++
				return "", fmt.Errorf("wrapped: %w", sentinel)
			}

			_, err := retryStartupLookup(context.Background(), fetch, func(int, error, time.Duration) {
				t.Error("onAttempt must not fire for a fail-fast error")
			})
			if !errors.Is(err, sentinel) {
				t.Fatalf("expected %v, got %v", sentinel, err)
			}
			if calls != 1 {
				t.Fatalf("expected exactly 1 fetch call, got %d", calls)
			}
		})
	}
}

// TestRetryStartupLookupCtxCancelAbortsWait: cancelling the context during the
// backoff wait (the real schedule starts at 5s) must abort promptly with
// ctx.Err() instead of sleeping out the timer — this is what makes SIGINT
// during a startup outage exit cleanly.
func TestRetryStartupLookupCtxCancelAbortsWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	fetch := func() (string, error) {
		return "", fmt.Errorf("%w: operation GetIDFromLogin", api.ErrPersistedQueryNotFound)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := retryStartupLookup(ctx, fetch, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancellation should abort the backoff wait promptly, took %v", elapsed)
	}
}

// TestStartupBackoffCapAndJitter: the schedule must cap at its last entry and
// jitter must stay within ±20% of the base value.
func TestStartupBackoffCapAndJitter(t *testing.T) {
	last := startupBackoffSchedule[len(startupBackoffSchedule)-1]
	for attempt := 1; attempt <= 20; attempt++ {
		got := startupBackoff(attempt)
		idx := attempt - 1
		if idx >= len(startupBackoffSchedule) {
			idx = len(startupBackoffSchedule) - 1
		}
		base := startupBackoffSchedule[idx]
		lo := time.Duration(float64(base) * 0.8)
		hi := time.Duration(float64(base) * 1.2)
		if got < lo || got > hi {
			t.Fatalf("attempt %d: backoff %v outside [%v, %v]", attempt, got, lo, hi)
		}
		if attempt > len(startupBackoffSchedule) && (got < time.Duration(float64(last)*0.8) || got > time.Duration(float64(last)*1.2)) {
			t.Fatalf("attempt %d: backoff %v not capped around %v", attempt, got, last)
		}
	}
}
