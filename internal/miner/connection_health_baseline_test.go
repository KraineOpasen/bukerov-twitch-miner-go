package miner

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
)

// TestBaselineIdleAPIIsNotFailed reproduces the confirmed production false
// positive at the Health-Center layer using only pre-existing symbols and the
// existing `now` seam of refreshHealthCenter.
//
// A freshly-constructed API client that has attempted NO requests (pure idle)
// must never be reported as a FAILED GQL API signal just because its
// constructor-stamped lastSuccess has aged past the connection threshold. On the
// unmodified base this test is RED (the signal is StatusFailed), which is the
// exact idle-blackout misclassification that also drives the false
// "Connection lost - harvesting paused" Discord alerts.
func TestBaselineIdleAPIIsNotFailed(t *testing.T) {
	client := api.NewTwitchClient(auth.NewTwitchAuth("tester", "device"), "device")
	center := health.NewCenter()

	m := &Miner{
		config: &config.Config{
			Username:   "tester",
			RateLimits: config.RateLimitSettings{ConnectionTimeoutMinutes: 5},
		},
		client:       client,
		healthCenter: center,
	}

	// No API request has been attempted. Advance the observation clock well past
	// the 5-minute threshold relative to the client's construction time: pure
	// idle, zero request failures.
	now := time.Now().Add(6 * time.Minute)
	m.refreshHealthCenter(now)

	sig, ok := center.Snapshot().Signal(health.SignalGQLAPI)
	if !ok {
		t.Fatal("expected a gql_api signal to be recorded")
	}
	if sig.Status == health.StatusFailed {
		t.Fatalf("idle API (no request attempts) must not be classified as FAILED; "+
			"got status=%q detail=%q — this is the idle-blackout false positive", sig.Status, sig.Detail)
	}
	if sig.Healthy() != true && sig.Status != health.StatusDegraded {
		t.Fatalf("idle API should read healthy/idle (or at most degraded), got status=%q", sig.Status)
	}
}
