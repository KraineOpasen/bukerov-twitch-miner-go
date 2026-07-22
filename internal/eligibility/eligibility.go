// Package eligibility centralizes the task-scoped decision of whether the miner
// should perform a given activity on a given streamer. It replaces the scattered
// boolean chains previously duplicated across the watcher, drops, and pubsub
// paths with one documented gate order per task, so capability, liveness, ACL,
// entitlement, and user-setting concerns are each evaluated exactly once and in
// a consistent priority.
//
// The evaluator is deliberately pure and clock-injectable: every decision is a
// function of its explicit inputs plus an injectable clock, with no hidden
// time.Now, no network, and no locks held across it.
package eligibility

import (
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// Task identifies a distinct thing the miner can do on a channel. Only tasks
// that actually exist in this codebase are enumerated — there is no weekly task
// subsystem, so no TaskWeekly is defined.
type Task string

const (
	// TaskDrops: farm an active drop entitlement. NOT gated by Channel Points
	// capability.
	TaskDrops Task = "drops"
	// TaskChannelPoints: occupy a watch slot primarily to accrue channel points
	// (points-only priority). Gated by Channel Points capability.
	TaskChannelPoints Task = "channel_points"
	// TaskWatchStreak: earn the per-stream watch-streak bonus. Points-gated.
	TaskWatchStreak Task = "watch_streak"
	// TaskPrediction: place an automated prediction bet. Points-gated.
	TaskPrediction Task = "prediction"
	// TaskBonusClaim: claim a channel-points bonus chest. Points-gated.
	TaskBonusClaim Task = "bonus_claim"
	// TaskCommunityGoal: contribute points to a community goal. Points-gated.
	TaskCommunityGoal Task = "community_goal"
	// TaskCustomReward: redeem a custom channel-points reward. Points-gated.
	TaskCustomReward Task = "custom_reward"
)

// needsWatchSlot reports whether performing the task occupies one of the two
// Twitch watch slots (and is therefore additionally gated by DisableWatch).
func (t Task) needsWatchSlot() bool {
	switch t {
	case TaskDrops, TaskChannelPoints, TaskWatchStreak:
		return true
	default:
		// Prediction, bonus-claim, community-goal and custom-reward act on a
		// channel already being watched/tracked; they do not themselves claim a
		// new watch slot.
		return false
	}
}

// pointsGated reports whether the task requires the Channel Points capability.
// Drops is the sole watch task that is NOT points-gated.
func (t Task) pointsGated() bool {
	return t != TaskDrops
}

// State is the tri-state outcome of an eligibility evaluation.
type State uint8

const (
	// StateEligible: proceed.
	StateEligible State = iota
	// StateIneligible: an authoritative gate blocks the task.
	StateIneligible
	// StateUnknown: a required signal (liveness, capability, ACL) is not
	// authoritatively known; do not start a new risky action.
	StateUnknown
)

func (s State) String() string {
	switch s {
	case StateEligible:
		return "eligible"
	case StateUnknown:
		return "unknown"
	default:
		return "ineligible"
	}
}

// Reason is a stable, privacy-safe code explaining a decision. It never carries
// a channel name, token, or claim identifier.
type Reason string

const (
	ReasonEligible                 Reason = "eligible"
	ReasonUserDisabled             Reason = "user_disabled"
	ReasonWatchDisabled            Reason = "watch_disabled"
	ReasonStatusOffline            Reason = "status_offline"
	ReasonStatusUnknown            Reason = "status_unknown"
	ReasonCapabilityDisabled       Reason = "capability_disabled"
	ReasonCapabilityUnknown        Reason = "capability_unknown"
	ReasonNoActiveEntitlement      Reason = "no_active_entitlement"
	ReasonRewardAlreadyClaimed     Reason = "reward_already_claimed"
	ReasonEntitlementExpired       Reason = "entitlement_expired"
	ReasonChannelNotAllowed        Reason = "channel_not_allowed"
	ReasonACLUnknown               Reason = "acl_unknown"
	ReasonCampaignNotAvailable     Reason = "campaign_not_available_on_channel"
	ReasonImpossibleBeforeDeadline Reason = "impossible_before_deadline"
	ReasonGameMismatch             Reason = "game_mismatch"
	ReasonNoWatchableTask          Reason = "no_watchable_task"
)

// Decision is the immutable result of an eligibility evaluation.
type Decision struct {
	Eligible bool
	State    State
	Reason   Reason
}

func eligible() Decision {
	return Decision{Eligible: true, State: StateEligible, Reason: ReasonEligible}
}
func blocked(r Reason) Decision { return Decision{Eligible: false, State: StateIneligible, Reason: r} }
func unknown(r Reason) Decision { return Decision{Eligible: false, State: StateUnknown, Reason: r} }

// Availability is the tri-state result of the channel-side campaign availability
// lookup (GetCampaignIDsFromStreamer). It is deliberately distinct from a plain
// bool so a transient lookup failure (Unknown) is never conflated with an
// authoritative "not advertised here" (No).
type Availability uint8

const (
	AvailabilityUnknown Availability = iota
	AvailabilityYes
	AvailabilityNo
)

// Evaluator holds the injectable clock. The zero value uses the system clock.
type Evaluator struct {
	Clock models.Clock
}

func (e Evaluator) now() time.Time { return e.Clock.Now() }

// PointsTaskUserEnabled reports whether the operator's per-streamer settings
// permit a points-gated task, independent of capability. Tasks without a
// dedicated toggle default to enabled (the miner's core always-on behaviors).
func PointsTaskUserEnabled(settings models.StreamerSettings, task Task) bool {
	switch task {
	case TaskWatchStreak:
		return settings.WatchStreak
	case TaskPrediction:
		return settings.MakePredictions
	case TaskCommunityGoal:
		return settings.CommunityGoals
	case TaskChannelPoints, TaskBonusClaim, TaskCustomReward:
		return true
	default:
		return true
	}
}

// EvaluatePointsTask applies the documented gate order for a Channel-Points
// gated task:
//  1. user setting;
//  2. DisableWatch (only for watch-slot tasks);
//  3. confirmed online;
//  4. Channel Points capability enabled;
//
// then the caller layers any task-specific prerequisites and broker policy.
//
// Capability is the crux: a confirmed-Disabled capability blocks every points
// task; an Unknown capability blocks starting a NEW points action (it is never
// coerced to enabled), while leaving Drops — evaluated separately — untouched.
func (e Evaluator) EvaluatePointsTask(s *models.Streamer, task Task) Decision {
	settings := s.GetSettings()

	if !PointsTaskUserEnabled(settings, task) {
		return blocked(ReasonUserDisabled)
	}
	if task.needsWatchSlot() && settings.DisableWatch {
		return blocked(ReasonWatchDisabled)
	}

	switch s.GetStatus() {
	case models.StatusOffline:
		return blocked(ReasonStatusOffline)
	case models.StatusUnknown:
		return unknown(ReasonStatusUnknown)
	}

	if task.pointsGated() {
		switch s.GetChannelPointsCapability() {
		case models.CapabilityDisabled:
			return blocked(ReasonCapabilityDisabled)
		case models.CapabilityUnknown:
			return unknown(ReasonCapabilityUnknown)
		}
	}

	return eligible()
}

// EvaluateDrops applies the documented Drops gate order for a specific campaign
// drop on a specific channel. Channel Points capability is intentionally NOT a
// gate here — disabled/unknown points never block an otherwise-eligible drop.
//
// Gate order:
//  1. ClaimDrops user setting;
//  2. DisableWatch;
//  3. confirmed online (for a new slot);
//  4. entitlement active (window);
//  5. reward not authoritatively claimed;
//  6. entitlement feasibility (deadline);
//  7. game identity match;
//  8. ACL state (unknown fails closed);
//  9. exact ChannelID allowed;
//
// 10. channel-side campaign availability (unknown != no).
func (e Evaluator) EvaluateDrops(s *models.Streamer, campaign *models.Campaign, drop *models.Drop, avail Availability) Decision {
	settings := s.GetSettings()

	if !settings.ClaimDrops {
		return blocked(ReasonUserDisabled)
	}
	if settings.DisableWatch {
		return blocked(ReasonWatchDisabled)
	}

	switch s.GetStatus() {
	case models.StatusOffline:
		return blocked(ReasonStatusOffline)
	case models.StatusUnknown:
		return unknown(ReasonStatusUnknown)
	}

	if campaign == nil || drop == nil {
		return blocked(ReasonNoActiveEntitlement)
	}

	// 4. Entitlement window.
	window := drop.Window()
	if !window.Known {
		window = campaignWindow(campaign)
	}
	switch window.State(e.Clock) {
	case models.WindowStateExpired:
		return blocked(ReasonEntitlementExpired)
	case models.WindowStateUpcoming:
		return blocked(ReasonNoActiveEntitlement)
	}
	// WindowStateActive and WindowStateUnknown both proceed: a date-less
	// in-progress drop (unknown window) is treated as live (it is in the
	// inventory), never as expired.

	// 5. Reward not authoritatively claimed (instance-local signal only; the
	// account-wide claim-history dedup already ran upstream and any survivor is
	// by definition not confirmed-claimed).
	if drop.IsClaimed {
		return blocked(ReasonRewardAlreadyClaimed)
	}

	// 6. Deadline feasibility.
	if feasible, decided := e.DropDeadlineFeasible(drop, window); decided && !feasible {
		return blocked(ReasonImpossibleBeforeDeadline)
	}

	// 7. Game identity match.
	if streamerGame := s.Stream.GameID(); streamerGame != "" && campaign.Game != nil &&
		campaign.Game.ID != "" && campaign.Game.ID != streamerGame {
		return blocked(ReasonGameMismatch)
	}

	// 8/9. ACL state + exact channel membership.
	switch campaign.ACLState() {
	case models.ACLUnknown:
		return unknown(ReasonACLUnknown)
	case models.ACLRestricted:
		if !campaign.AllowsChannel(s.ChannelID) {
			return blocked(ReasonChannelNotAllowed)
		}
	}

	// 10. Channel-side availability: unknown is distinct from no. Only an
	// authoritative "not advertised here" blocks; a transient lookup failure
	// does not.
	if avail == AvailabilityNo {
		return blocked(ReasonCampaignNotAvailable)
	}

	return eligible()
}

// SlotCandidateEligible reports whether a streamer has at least one
// confirmed-useful task justifying a NEW watch slot: an active eligible Drops
// entitlement, OR a points task that is confirmed permitted (capability Enabled
// and the user setting on). A points-only streamer whose Channel Points
// capability is Disabled OR merely Unknown yields false — an unknown capability
// is never a basis to grant a slot, and never coerced to enabled. Drops
// eligibility is evaluated independently, so a disabled/unknown-points channel
// with a live drop still qualifies.
//
// hasActiveDrops is supplied by the caller (it already knows the streamer's
// assigned campaigns / DropsCondition), keeping this function free of the drops
// pipeline. reason explains the outcome for diagnostics.
func (e Evaluator) SlotCandidateEligible(s *models.Streamer, hasActiveDrops bool) (bool, Reason) {
	settings := s.GetSettings()
	if settings.DisableWatch {
		return false, ReasonWatchDisabled
	}

	// Drops path — not points-gated.
	if hasActiveDrops && settings.ClaimDrops {
		return true, ReasonEligible
	}

	// Points path — requires a confirmed-enabled capability.
	switch s.GetChannelPointsCapability() {
	case models.CapabilityEnabled:
		return true, ReasonEligible
	case models.CapabilityDisabled:
		return false, ReasonCapabilityDisabled
	default:
		return false, ReasonCapabilityUnknown
	}
}

// DropDeadlineFeasible reports whether the drop's remaining watch requirement can
// still be met before its entitlement window closes. It uses the server-reported
// CurrentMinutesWatched (never locally-estimated progress) and adds NO safety
// margin. decided is false when the end time is unknown — an unknown deadline is
// never treated as impossible.
func (e Evaluator) DropDeadlineFeasible(drop *models.Drop, window models.EntitlementWindow) (feasible, decided bool) {
	need := drop.MinutesRemaining()
	if need <= 0 {
		return true, true
	}
	if !window.Decidable() || window.End.IsZero() {
		return true, false
	}
	remaining := window.End.Sub(e.now())
	if remaining <= 0 {
		return false, true
	}
	availableMinutes := int(remaining / time.Minute)
	return availableMinutes >= need, true
}

// campaignWindow mirrors Campaign.campaignWindow for the eligibility layer
// (which cannot reach the unexported method). It is the campaign-level occurrence
// fallback for drops carrying no per-drop dates.
func campaignWindow(c *models.Campaign) models.EntitlementWindow {
	if c.StartAt.IsZero() && c.EndAt.IsZero() {
		src := models.WindowSourceNone
		if c.InInventory {
			src = models.WindowSourceInventory
		}
		return models.EntitlementWindow{Source: src, Known: false}
	}
	return models.EntitlementWindow{
		Start:  c.StartAt,
		End:    c.EndAt,
		Source: models.WindowSourceCampaign,
		Known:  true,
	}
}
