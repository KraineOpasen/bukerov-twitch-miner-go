package miner

import (
	"errors"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/eligibility"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
)

// pointsActionGate maps each ineligible reason to a stable, user-safe error,
// keeping capability-disabled, capability-unknown, offline and status-unknown
// distinct.
func TestPointsActionGate(t *testing.T) {
	mk := func(f func(*models.Streamer)) *models.Streamer {
		s := models.NewStreamer("s", models.DefaultStreamerSettings())
		f(s)
		return s
	}
	cases := []struct {
		name string
		s    *models.Streamer
		want error
	}{
		{"enabled online -> ok", mk(func(s *models.Streamer) {
			s.SetConfirmedOnline()
			s.SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)
		}), nil},
		{"disabled -> disabled err", mk(func(s *models.Streamer) {
			s.SetConfirmedOnline()
			s.SetChannelPointsCapability(models.CapabilityDisabled, models.CapReasonConfirmedDisabled)
		}), ErrChannelPointsDisabled},
		{"unknown cap -> unknown err", mk(func(s *models.Streamer) {
			s.SetConfirmedOnline() // capability stays Unknown
		}), ErrChannelPointsUnknown},
		{"offline -> offline err", mk(func(s *models.Streamer) {
			s.SetConfirmedOnline()
			s.SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)
			s.SetConfirmedOffline()
		}), ErrStreamerOffline},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pointsActionGate(tc.s, eligibility.TaskCustomReward)
			if !errors.Is(got, tc.want) {
				t.Fatalf("gate = %v, want %v", got, tc.want)
			}
		})
	}
}

type rewardGateClient struct{}

func (rewardGateClient) GetChannelID(u string) (string, error)           { return "ch-" + u, nil }
func (rewardGateClient) LoadChannelPointsContext(*models.Streamer) error { return nil }
func (rewardGateClient) CheckStreamerOnline(*models.Streamer) models.StatusTransition {
	return models.StatusTransition{}
}

// RedeemCustomReward is blocked BEFORE any Twitch call when Channel Points are
// confirmed disabled or unknown — the user-safe reason is returned and the
// reward-list fetch never happens.
func TestRedeemCustomRewardCapabilityGate(t *testing.T) {
	mgr := streamer.NewManager(&rewardGateClient{}, models.DefaultStreamerSettings())
	if err := mgr.LoadFromConfig([]config.StreamerConfig{{Username: "cyganzor"}}, nil); err != nil {
		t.Fatalf("load streamers: %v", err)
	}
	s := mgr.Get("cyganzor")
	s.SetConfirmedOnline()
	s.SetChannelPointsCapability(models.CapabilityDisabled, models.CapReasonConfirmedDisabled)

	m := &Miner{config: &config.Config{Username: "tester"}, streamers: mgr}
	// client is nil: if the gate did not short-circuit, GetCustomRewards would
	// panic — so a clean disabled-error return proves the gate ran first.
	err := m.RedeemCustomReward("cyganzor", "reward-1", "")
	if !errors.Is(err, ErrChannelPointsDisabled) {
		t.Fatalf("expected ErrChannelPointsDisabled, got %v", err)
	}
}
