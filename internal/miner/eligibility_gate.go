package miner

import (
	"errors"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/eligibility"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// minerPointsEligibility is the single centralized policy the miner uses to gate
// Channel-Points actions (manual bets, custom-reward redemptions, bonus polling).
// It is stateless (system clock); there is no second, divergent capability policy.
var minerPointsEligibility = eligibility.Evaluator{}

// User-facing, privacy-safe errors for a blocked points action. They never leak
// a raw Twitch/Go error, and they keep unknown DISTINCT from disabled and from
// offline, so the operator sees an accurate, stable reason.
var (
	ErrChannelPointsDisabled   = errors.New("channel points are disabled for this channel")
	ErrChannelPointsUnknown    = errors.New("channel points availability is not confirmed yet; please try again shortly")
	ErrStreamerOffline         = errors.New("streamer is offline")
	ErrStreamerStatusUnknown   = errors.New("streamer status is not confirmed right now; please try again shortly")
	ErrPointsActionUserBlocked = errors.New("this action is disabled in the streamer's settings")
)

// pointsActionGate evaluates a points task and returns nil when it may proceed,
// or a stable, user-safe error explaining why not — distinguishing capability
// disabled, capability unknown, offline, status-unknown, and user-disabled.
func pointsActionGate(s *models.Streamer, task eligibility.Task) error {
	d := minerPointsEligibility.EvaluatePointsTask(s, task)
	if d.Eligible {
		return nil
	}
	switch d.Reason {
	case eligibility.ReasonCapabilityDisabled:
		return ErrChannelPointsDisabled
	case eligibility.ReasonCapabilityUnknown:
		return ErrChannelPointsUnknown
	case eligibility.ReasonStatusOffline:
		return ErrStreamerOffline
	case eligibility.ReasonStatusUnknown:
		return ErrStreamerStatusUnknown
	case eligibility.ReasonUserDisabled, eligibility.ReasonWatchDisabled:
		return ErrPointsActionUserBlocked
	default:
		return ErrChannelPointsUnknown
	}
}
