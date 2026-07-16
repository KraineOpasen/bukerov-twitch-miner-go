package api

import (
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestJoinRaidFailureStaysRetryable covers the "failed join is never retried"
// regression: JoinRaid used to set streamer.Raid BEFORE the network call, so
// its RaidID guard treated a failed join as done and short-circuited the
// repeated raid_update_v2 events Twitch sends during the raid countdown — the
// natural retry channel. The raid must be marked joined only on real success.
func TestJoinRaidFailureStaysRetryable(t *testing.T) {
	var requests atomic.Int64
	var failing atomic.Bool
	failing.Store(true)

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		if failing.Load() {
			// Every candidate client ID gets PersistedQueryNotFound: the whole
			// join attempt fails with ErrPersistedQueryNotFound.
			_, _ = io.WriteString(w, persistedQueryNotFoundBody)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"joinRaid":{"raidID":"raid-1"}}}`)
	})

	s := newTestStreamer("somestreamer")
	raid := &models.Raid{RaidID: "raid-1", TargetLogin: "target"}

	// Phase A: Twitch is down (stale hashes). The join fails and must NOT be
	// recorded as done.
	err := c.JoinRaid(s, raid)
	if !errors.Is(err, ErrPersistedQueryNotFound) {
		t.Fatalf("expected ErrPersistedQueryNotFound, got %v", err)
	}
	if s.Raid != nil {
		t.Fatal("streamer.Raid must stay nil after a FAILED join, or the next raid_update_v2 can never retry it")
	}
	afterFailure := requests.Load()
	if afterFailure == 0 {
		t.Fatal("expected the failed join to have hit the network")
	}

	// Phase B: the next raid_update_v2 event for the SAME raid arrives after
	// Twitch recovered. The guard must not short-circuit — the join must reach
	// the network again and succeed.
	failing.Store(false)
	if err := c.JoinRaid(s, raid); err != nil {
		t.Fatalf("expected the retried join to succeed, got %v", err)
	}
	if requests.Load() == afterFailure {
		t.Fatal("retried join never reached the network: the RaidID guard short-circuited a failed join")
	}
	if s.Raid == nil || s.Raid.RaidID != raid.RaidID {
		t.Fatalf("streamer.Raid must record the successfully joined raid, got %+v", s.Raid)
	}

	// Phase C: yet another raid_update_v2 for the same raid. Now the guard is
	// legitimate: no extra network call.
	afterSuccess := requests.Load()
	if err := c.JoinRaid(s, raid); err != nil {
		t.Fatalf("expected duplicate join of a joined raid to be a no-op, got %v", err)
	}
	if requests.Load() != afterSuccess {
		t.Fatal("a raid already joined successfully must not be re-sent to Twitch")
	}
}

// TestJoinRaidServiceErrorNotMarkedJoined: an HTTP-200 response carrying a
// top-level "errors" array (a non-PQNF service failure) is not an accepted
// join — it must return an error and leave the raid unmarked so the next
// raid_update_v2 event retries it.
func TestJoinRaidServiceErrorNotMarkedJoined(t *testing.T) {
	var requests atomic.Int64
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"errors":[{"message":"service error"}],"data":null}`)
	})

	s := newTestStreamer("somestreamer")
	raid := &models.Raid{RaidID: "raid-2", TargetLogin: "target"}

	if err := c.JoinRaid(s, raid); err == nil {
		t.Fatal("expected an error for a service-error response")
	}
	if s.Raid != nil {
		t.Fatal("streamer.Raid must stay nil when Twitch did not accept the join")
	}

	// The same raid must still be retryable.
	before := requests.Load()
	if err := c.JoinRaid(s, raid); err == nil {
		t.Fatal("expected the retry to fail again against the still-broken server")
	}
	if requests.Load() == before {
		t.Fatal("retry after a service error never reached the network")
	}
}
