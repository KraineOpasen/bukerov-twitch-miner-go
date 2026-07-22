package pubsub

import (
	"errors"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// B6 boundary: a manual bet is blocked when Channel Points are confirmed
// disabled or merely unknown, with a user-safe, distinct reason — and the placer
// is never invoked.
func TestManualBetBlockedByCapability(t *testing.T) {
	cases := []struct {
		name string
		cap  models.CapabilityState
		want error
	}{
		{"disabled", models.CapabilityDisabled, ErrChannelPointsDisabled},
		{"unknown", models.CapabilityUnknown, ErrChannelPointsUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			placer := &fakePlacer{}
			pool := newTestPool(placer)
			s := newTestStreamer(100000)
			s.SetChannelPointsCapability(tc.cap, models.CapReasonConfirmedDisabled)
			if tc.cap == models.CapabilityUnknown {
				// Reset to unknown (newTestStreamer marks Enabled).
				s.SetChannelPointsCapability(models.CapabilityUnknown, models.CapReasonTransportError)
			}
			addRound(pool, s, "e1")

			_, err := pool.PlaceManualBet("e1", "o1", 500)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if placer.callCount() != 0 {
				t.Fatalf("placer must not be called when the capability gate blocks, got %d calls", placer.callCount())
			}
		})
	}
}

// B6 boundary: community-goal contribution short-circuits (never touches the
// client) when the capability gate blocks. Uses a nil client to prove no client
// call is attempted on the disabled/unknown path.
func TestContributeToGoalsSkippedWhenNotEligible(t *testing.T) {
	for _, cap := range []models.CapabilityState{models.CapabilityDisabled, models.CapabilityUnknown} {
		pool := &WebSocketPool{} // nil client: any client access would panic
		s := models.NewStreamer("s", models.DefaultStreamerSettings())
		s.SetConfirmedOnline()
		s.SetChannelPointsCapability(cap, models.CapReasonConfirmedDisabled)
		s.AddCommunityGoal(&models.CommunityGoal{GoalID: "g1", Status: models.CommunityGoalStarted, IsInStock: true, GoalAmount: 1000})
		s.SetChannelPoints(5000)

		// Must return without calling the (nil) client — no panic.
		pool.contributeToGoals(s)
	}
}

// B6 boundary: a claim-available bonus event short-circuits (never touches the
// client) when the capability gate blocks.
func TestBonusClaimSkippedWhenNotEligible(t *testing.T) {
	for _, cap := range []models.CapabilityState{models.CapabilityDisabled, models.CapabilityUnknown} {
		pool := &WebSocketPool{} // nil client
		s := models.NewStreamer("s", models.DefaultStreamerSettings())
		s.SetConfirmedOnline()
		s.SetChannelPointsCapability(cap, models.CapReasonConfirmedDisabled)

		msg := &PubSubMessage{
			Type: "claim-available",
			Data: map[string]interface{}{"claim": map[string]interface{}{"id": "claim-1"}},
		}
		// Must return without calling the (nil) client — no panic.
		pool.handleCommunityPointsUser(msg, s)
	}
}
