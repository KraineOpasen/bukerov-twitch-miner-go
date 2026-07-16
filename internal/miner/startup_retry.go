package miner

import (
	"context"
	"errors"
	"math/rand"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
)

// startupBackoffSchedule is the base delay before retry N of the startup
// user-ID lookup; past the last entry every retry waits the final (capped)
// value. Package-level so tests can shrink it.
var startupBackoffSchedule = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
	60 * time.Second,
}

// retryStartupLookup runs fetch until it succeeds, retrying indefinitely with
// capped, jittered backoff. It exists for the one Twitch call the miner cannot
// start without — resolving the account's own user ID: exiting on a temporary
// Twitch-side outage (stale persisted-query hashes, a long 5xx spell) would
// just crash-loop the container, killing the dashboard with the process, so
// the loop keeps the process alive and observable instead.
//
// Fail-fast is reserved for the errors retrying cannot cure: a rejected token
// (api.ErrUnauthorized — needs reauthorization) and a login Twitch reports as
// nonexistent (api.ErrStreamerDoesNotExist — a config typo). Everything else,
// including api.ErrPersistedQueryNotFound and transport exhaustion (which has
// no sentinel to match on), is retried: an unknown failure is far more likely
// transient than permanent, and an endless retry is observable via onAttempt
// while a wrong exit is not.
//
// onAttempt (optional) is invoked before each wait with the 1-based attempt
// number, the error, and the upcoming delay. Cancelling ctx aborts the wait
// immediately and returns ctx.Err().
func retryStartupLookup(ctx context.Context, fetch func() (string, error), onAttempt func(attempt int, err error, next time.Duration)) (string, error) {
	for attempt := 1; ; attempt++ {
		id, err := fetch()
		if err == nil {
			return id, nil
		}
		if errors.Is(err, api.ErrUnauthorized) || errors.Is(err, api.ErrStreamerDoesNotExist) {
			return "", err
		}

		wait := startupBackoff(attempt)
		if onAttempt != nil {
			onAttempt(attempt, err, wait)
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}

// startupBackoff returns the jittered delay before retry number attempt
// (1-based): the schedule's entry for that attempt, capped at its last value,
// with ±20% jitter so restarts across many deployments don't hit Twitch in
// lockstep.
func startupBackoff(attempt int) time.Duration {
	idx := attempt - 1
	if idx >= len(startupBackoffSchedule) {
		idx = len(startupBackoffSchedule) - 1
	}
	base := startupBackoffSchedule[idx]
	jitter := (rand.Float64() - 0.5) * 0.4
	return time.Duration(float64(base) * (1 + jitter))
}
