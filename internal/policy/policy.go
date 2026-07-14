// Package policy is the deterministic, explainable campaign-selection engine.
//
// It ranks the currently-trackable drop campaigns under a chosen mode
// (GAME_ORDER / ENDING_SOONEST / CLOSEST_TO_REWARD / LOW_AVAILABILITY / SMART)
// and, for each, produces a feasibility estimate plus a transparent scoring
// breakdown. It is pure: no I/O, no globals, no time.Now() — the caller passes
// `now` and a slice of already-assembled CampaignInput snapshots, so the whole
// engine is trivially unit-testable and its output is reproducible. It never
// makes a watch-slot decision itself; the unified slot broker stays the sole
// authority. Nothing here is an opaque model — every point in a decision is a
// named, human-readable factor.
//
// Feasibility is an ESTIMATE from current data, never a guaranteed drop.
package policy

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Mode is a campaign-selection strategy.
type Mode string

const (
	ModeGameOrder       Mode = "GAME_ORDER"
	ModeEndingSoonest   Mode = "ENDING_SOONEST"
	ModeClosestToReward Mode = "CLOSEST_TO_REWARD"
	ModeLowAvailability Mode = "LOW_AVAILABILITY"
	ModeSmart           Mode = "SMART"
)

// DefaultMode preserves the pre-policy behavior (configured game order), so
// enabling the engine changes nothing until the operator opts into another
// mode.
const DefaultMode = ModeGameOrder

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool {
	switch m {
	case ModeGameOrder, ModeEndingSoonest, ModeClosestToReward, ModeLowAvailability, ModeSmart:
		return true
	default:
		return false
	}
}

// Normalize upper-cases and validates m, falling back to DefaultMode.
func Normalize(s string) Mode {
	m := Mode(strings.ToUpper(strings.TrimSpace(s)))
	if m.Valid() {
		return m
	}
	return DefaultMode
}

// FeasStatus is the coarse feasibility verdict for a campaign.
type FeasStatus string

const (
	StatusSafe           FeasStatus = "SAFE"             // can finish the whole campaign with margin
	StatusAtRisk         FeasStatus = "AT_RISK"          // can finish all, but the margin is thin
	StatusNextRewardOnly FeasStatus = "NEXT_REWARD_ONLY" // can finish the next reward but not the chain
	StatusImpossible     FeasStatus = "IMPOSSIBLE"       // cannot even finish the next reward before it ends
)

// Scoring weights and thresholds. All are named so a decision breakdown reads
// as plain English and the numbers are auditable in one place.
const (
	// minStabilitySamples is how many delivered watch reports the current slot
	// session must have before the channel-stability factor participates at
	// all. Below it, the factor is neutral (0 points) and explicitly labeled
	// "insufficient data" — a couple of observations must never masquerade as a
	// confident 0%/100% signal (the same cold-start guard as the Stage 1
	// displacement tie-break).
	minStabilitySamples = 5

	smartHighPriority    = 200 // per-drop "High priority" rule floats a campaign up
	smartRestricted      = 100 // channel-restricted campaign (only earnable here)
	smartEndingSoonBonus = 80  // ends within endingSoonWindow
	smartScarceChannel   = 30  // exactly one eligible live channel
	smartStartedBonus    = 40  // campaign already in progress
	smartWatchingBonus   = 10  // slight stickiness for a channel already in a slot
	smartUnstablePenalty = 50  // max penalty for a fully unstable channel
	smartNextRewardOnly  = 40  // penalty when the whole chain can't be finished

	endingSoonWindow = 6 * time.Hour
	safetyReserveMin = 10 // minutes of buffer kept before a campaign's end
	atRiskMarginMin  = 30 // slack below which SAFE downgrades to AT_RISK
)

// rewardCloseness returns the SMART bonus for how close the next reward is,
// tiered so the breakdown reads in clean, explainable steps.
func rewardCloseness(mins int) int {
	switch {
	case mins <= 0:
		return 0
	case mins <= 30:
		return 60
	case mins <= 60:
		return 40
	case mins <= 120:
		return 20
	default:
		return 0
	}
}

// DropStep is one milestone in a campaign's drop chain.
type DropStep struct {
	MinutesRequired       int
	CurrentMinutesWatched int
	IsClaimed             bool
}

// CampaignInput is the caller-assembled snapshot the engine scores. Every
// field is derived from data the miner already holds (no new Twitch calls).
type CampaignInput struct {
	CampaignID string
	Name       string
	Game       string

	Restricted bool      // channel-restricted campaign
	Started    bool      // already in the account inventory / in progress
	EndAt      time.Time // campaign end (zero = unknown/none)

	Drops []DropStep // the drop chain, for feasibility

	EligibleLiveChannels int  // live channels that can currently farm this campaign
	WatchingHere         bool // currently occupies a watch slot

	// ChannelStability is 0..1 (1 = every recent watch report delivered).
	// It only participates once StabilitySamples >= minStabilitySamples.
	ChannelStability float64
	StabilitySamples int

	// GameOrderIndex is the campaign's game position in the operator's
	// configured game order (0-based; negative = not configured, ranked last).
	GameOrderIndex int

	// Per-drop rule flags (from config, keyed by normalized reward identity).
	Skip                 bool
	HighPriority         bool
	AlwaysFinishStarted  bool
	NextRewardOnly       bool
	IgnoreSubscriberOnly bool

	// SubscriberOnly is Twitch's best-effort subscriber-only flag;
	// SubscriberOnlyKnown records whether Twitch actually reported it, so the
	// "Ignore subscriber-only" control can honestly show "no effect" when the
	// data is absent.
	SubscriberOnly      bool
	SubscriberOnlyKnown bool
}

// Feasibility is the estimate (never a guarantee) of what can still be earned.
type Feasibility struct {
	TimeUntilEnd          time.Duration `json:"timeUntilEnd"`
	MinutesToNextReward   int           `json:"minutesToNextReward"`
	MinutesToCompleteAll  int           `json:"minutesToCompleteAll"`
	CanCompleteNextReward bool          `json:"canCompleteNextReward"`
	CanCompleteAll        bool          `json:"canCompleteAll"`
	SafetyReserveMinutes  int           `json:"safetyReserveMinutes"`
	Status                FeasStatus    `json:"status"`
}

// Factor is one named contribution to a decision's score.
type Factor struct {
	Label  string `json:"label"`
	Points int    `json:"points"`
}

// Decision is the engine's explainable verdict for one campaign.
type Decision struct {
	CampaignID    string      `json:"campaignId"`
	Name          string      `json:"name"`
	Mode          Mode        `json:"mode"`
	Total         int         `json:"total"`
	Factors       []Factor    `json:"factors,omitempty"`
	Feasibility   Feasibility `json:"feasibility"`
	Status        FeasStatus  `json:"status"`
	Excluded      bool        `json:"excluded,omitempty"`
	ExcludeReason string      `json:"excludeReason,omitempty"`
}

// nextReward returns the remaining watched minutes to the lowest-threshold
// unclaimed, not-yet-met drop (the next reward to unlock), and whether one
// exists.
func nextReward(drops []DropStep) (remaining int, ok bool) {
	minThresh := 0
	for _, d := range drops {
		if d.IsClaimed || d.CurrentMinutesWatched >= d.MinutesRequired {
			continue
		}
		if !ok || d.MinutesRequired < minThresh {
			minThresh = d.MinutesRequired
			remaining = d.MinutesRequired - d.CurrentMinutesWatched
			ok = true
		}
	}
	return remaining, ok
}

// completeAllRemaining returns the watched minutes still needed to finish the
// whole campaign — the furthest unclaimed milestone's remaining, matching the
// codebase's cumulative drop model (Campaign.FinalDrop / OverallProgressPercent).
func completeAllRemaining(drops []DropStep) int {
	maxRem := 0
	for _, d := range drops {
		if d.IsClaimed {
			continue
		}
		if rem := d.MinutesRequired - d.CurrentMinutesWatched; rem > maxRem {
			maxRem = rem
		}
	}
	return maxRem
}

// ComputeFeasibility estimates what the campaign can still earn before it ends.
func ComputeFeasibility(in CampaignInput, now time.Time) Feasibility {
	f := Feasibility{SafetyReserveMinutes: safetyReserveMin}
	f.TimeUntilEnd = in.EndAt.Sub(now)
	if f.TimeUntilEnd < 0 {
		f.TimeUntilEnd = 0
	}

	nr, hasNext := nextReward(in.Drops)
	f.MinutesToNextReward = nr
	f.MinutesToCompleteAll = completeAllRemaining(in.Drops)

	// The NextRewardOnly rule reduces the "finish everything" goal to just the
	// next reward, so a user who only wants the next reward reads SAFE once it
	// is reachable.
	goalAll := f.MinutesToCompleteAll
	if in.NextRewardOnly {
		goalAll = nr
	}

	availMin := int(f.TimeUntilEnd/time.Minute) - safetyReserveMin
	f.CanCompleteNextReward = hasNext && availMin >= nr
	f.CanCompleteAll = availMin >= goalAll

	switch {
	case !in.EndAt.IsZero() && !in.EndAt.After(now):
		f.Status = StatusImpossible
	case !hasNext && f.MinutesToCompleteAll == 0:
		f.Status = StatusSafe // nothing left to earn
	case !f.CanCompleteNextReward:
		f.Status = StatusImpossible
	case !f.CanCompleteAll:
		f.Status = StatusNextRewardOnly
	case availMin-goalAll < atRiskMarginMin:
		f.Status = StatusAtRisk
	default:
		f.Status = StatusSafe
	}
	return f
}

// Decide scores a single campaign under the given mode.
func Decide(mode Mode, in CampaignInput, now time.Time) Decision {
	mode = Normalize(string(mode))
	f := ComputeFeasibility(in, now)
	d := Decision{CampaignID: in.CampaignID, Name: in.Name, Mode: mode, Feasibility: f, Status: f.Status}

	if in.Skip {
		d.Excluded, d.ExcludeReason = true, "per-drop rule: Skip"
		return d
	}
	if f.Status == StatusImpossible {
		d.Excluded, d.ExcludeReason = true, "cannot finish the next reward before the campaign ends"
		return d
	}

	if mode == ModeSmart {
		return smartDecision(in, f, d)
	}
	return modeDecision(mode, in, f, d)
}

func smartDecision(in CampaignInput, f Feasibility, d Decision) Decision {
	add := func(label string, pts int) {
		if pts == 0 {
			return
		}
		d.Factors = append(d.Factors, Factor{Label: label, Points: pts})
		d.Total += pts
	}

	if in.HighPriority {
		add("per-drop rule: High priority", smartHighPriority)
	}
	if in.Restricted {
		add("channel-restricted campaign", smartRestricted)
	}
	if !in.EndAt.IsZero() && f.TimeUntilEnd > 0 && f.TimeUntilEnd < endingSoonWindow {
		add(fmt.Sprintf("ends in under %dh", int(endingSoonWindow/time.Hour)), smartEndingSoonBonus)
	}
	if pts := rewardCloseness(f.MinutesToNextReward); pts != 0 {
		add(fmt.Sprintf("next reward in %d min", f.MinutesToNextReward), pts)
	}
	if in.EligibleLiveChannels == 1 {
		add("only one eligible live channel", smartScarceChannel)
	}
	if in.Started {
		label := "campaign already started"
		if in.AlwaysFinishStarted {
			label = "started campaign + finish-started rule"
		}
		add(label, smartStartedBonus)
	}
	if in.WatchingHere {
		add("already in a watch slot", smartWatchingBonus)
	}

	// Channel-stability penalty, gated on a minimum sample size so a 1-2
	// observation window never yields a confident extreme.
	if in.StabilitySamples < minStabilitySamples {
		d.Factors = append(d.Factors, Factor{
			Label:  fmt.Sprintf("channel stability: insufficient data (%d/%d reports)", in.StabilitySamples, minStabilitySamples),
			Points: 0,
		})
	} else if in.ChannelStability < 1 {
		add(fmt.Sprintf("unstable channel (%.0f%% delivery)", in.ChannelStability*100),
			-int(math.Round(smartUnstablePenalty*(1-in.ChannelStability))))
	}

	if f.Status == StatusNextRewardOnly {
		add("cannot finish the whole campaign in time", -smartNextRewardOnly)
	}
	return d
}

func modeDecision(mode Mode, in CampaignInput, f Feasibility, d Decision) Decision {
	// High priority still floats a campaign up in every mode.
	if in.HighPriority {
		d.Factors = append(d.Factors, Factor{Label: "per-drop rule: High priority", Points: smartHighPriority})
		d.Total += smartHighPriority
	}
	switch mode {
	case ModeEndingSoonest:
		d.Factors = append(d.Factors, Factor{Label: fmt.Sprintf("ends in %s", f.TimeUntilEnd.Round(time.Minute))})
	case ModeClosestToReward:
		d.Factors = append(d.Factors, Factor{Label: fmt.Sprintf("next reward in %d min", f.MinutesToNextReward)})
	case ModeLowAvailability:
		d.Factors = append(d.Factors, Factor{Label: fmt.Sprintf("%d eligible live channel(s)", in.EligibleLiveChannels)})
	default: // ModeGameOrder
		d.Factors = append(d.Factors, Factor{Label: fmt.Sprintf("configured game order position %d", in.GameOrderIndex+1)})
	}
	return d
}

// Rank scores every input under mode and returns the decisions ordered
// best-first. Excluded campaigns (Skip / impossible) sort last. The ordering
// is deterministic: ties break on campaign ID, so identical inputs always
// produce identical output.
func Rank(mode Mode, inputs []CampaignInput, now time.Time) []Decision {
	mode = Normalize(string(mode))

	type ranked struct {
		d  Decision
		in CampaignInput
	}
	items := make([]ranked, len(inputs))
	for i, in := range inputs {
		items[i] = ranked{d: Decide(mode, in, now), in: in}
	}

	gameIdx := func(i int) int {
		if i < 0 {
			return math.MaxInt32
		}
		return i
	}

	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.d.Excluded != b.d.Excluded {
			return !a.d.Excluded // excluded last
		}
		if a.in.HighPriority != b.in.HighPriority {
			return a.in.HighPriority // high priority first, in every mode
		}
		switch mode {
		case ModeSmart:
			if a.d.Total != b.d.Total {
				return a.d.Total > b.d.Total
			}
		case ModeEndingSoonest:
			if !a.in.EndAt.Equal(b.in.EndAt) {
				return a.in.EndAt.Before(b.in.EndAt)
			}
		case ModeClosestToReward:
			if a.d.Feasibility.MinutesToNextReward != b.d.Feasibility.MinutesToNextReward {
				return a.d.Feasibility.MinutesToNextReward < b.d.Feasibility.MinutesToNextReward
			}
		case ModeLowAvailability:
			if a.in.EligibleLiveChannels != b.in.EligibleLiveChannels {
				return a.in.EligibleLiveChannels < b.in.EligibleLiveChannels
			}
		default: // ModeGameOrder
			if ai, bi := gameIdx(a.in.GameOrderIndex), gameIdx(b.in.GameOrderIndex); ai != bi {
				return ai < bi
			}
		}
		return a.in.CampaignID < b.in.CampaignID // deterministic tie-break
	})

	out := make([]Decision, len(items))
	for i := range items {
		out[i] = items[i].d
	}
	return out
}

// Breakdown renders a decision's factors as the human-readable list used in
// the UI and docs, e.g.:
//
//	+100 channel-restricted campaign
//	 +80 ends in under 6h
//	 +60 next reward in 22 min
//	 +30 only one eligible live channel
//	 -50 unstable channel (0% delivery)
//	Total: 220
func (d Decision) Breakdown() string {
	var b strings.Builder
	for _, f := range d.Factors {
		fmt.Fprintf(&b, "%+d %s\n", f.Points, f.Label)
	}
	fmt.Fprintf(&b, "Total: %d", d.Total)
	return b.String()
}
