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
	StatusUnknown        FeasStatus = "UNKNOWN"          // deadline absent; feasibility cannot be decided
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
	// DeadlineKnown must be checked before treating TimeUntilEnd or either
	// CanComplete boolean as a deadline-derived fact.
	DeadlineKnown         bool          `json:"deadlineKnown"`
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

// SemanticClass is the ordinal policy class assigned by Rank. Lower classes
// are preferred. Decisions with identical policy facts share one class even
// though CampaignID still provides a deterministic presentation order inside
// that class. The value is intentionally unitless: it must never be added to
// watch minutes or any other broker fairness measure.
type SemanticClass uint32

// CampaignSemantic is the immutable per-campaign fact published to channel
// projections. SemanticClass remains the existing primary policy ordinal.
// SecondaryEligible is deliberately narrower: only a campaign with known
// feasible remaining work may contribute as the one bounded overlap fact.
type CampaignSemantic struct {
	SemanticClass     SemanticClass
	SecondaryEligible bool
}

// SemanticUtility is the bounded lexicographic projection for one channel.
// SemanticClass is the existing primary class. At most one distinct campaign
// contributes SecondarySemanticClass; campaign IDs make the projection
// explainable and deterministic but never participate in cross-channel
// preference.
type SemanticUtility struct {
	SemanticClass          SemanticClass
	SecondarySemanticClass SemanticClass
	HasSecondary           bool
	PrimaryCampaignID      string
	SecondaryCampaignID    string
}

// Decision is the engine's explainable verdict for one campaign.
type Decision struct {
	CampaignID    string        `json:"campaignId"`
	Name          string        `json:"name"`
	Mode          Mode          `json:"mode"`
	Total         int           `json:"total"`
	SemanticClass SemanticClass `json:"semanticClass"`
	Factors       []Factor      `json:"factors,omitempty"`
	Feasibility   Feasibility   `json:"feasibility"`
	Status        FeasStatus    `json:"status"`
	Excluded      bool          `json:"excluded,omitempty"`
	ExcludeReason string        `json:"excludeReason,omitempty"`
}

// CampaignSemanticFromDecision converts a ranked policy decision into the
// richer fact needed by channel projections. Excluded and identity-less
// decisions are not published at all. UNKNOWN and completed campaigns retain
// their existing primary semantics but fail closed for secondary utility;
// IMPOSSIBLE fails closed even if a malformed caller omitted Excluded.
func CampaignSemanticFromDecision(d Decision) (CampaignSemantic, bool) {
	if d.CampaignID == "" || d.Excluded || d.Status == StatusImpossible {
		return CampaignSemantic{}, false
	}
	secondaryEligible := d.Feasibility.MinutesToNextReward > 0
	if secondaryEligible {
		switch d.Status {
		case StatusSafe, StatusAtRisk, StatusNextRewardOnly:
		default:
			secondaryEligible = false
		}
	}
	return CampaignSemantic{
		SemanticClass:     d.SemanticClass,
		SecondaryEligible: secondaryEligible,
	}, true
}

// BuildSemanticUtility projects the exact campaign IDs carried by a channel
// into one primary class plus at most the best one distinct eligible secondary
// class. IDs are deduplicated before selection; absent and empty IDs provide no
// fact. If primary-class campaigns tie, an eligible one is consumed as primary
// first so an UNKNOWN/completed peer cannot manufacture overlap utility.
func BuildSemanticUtility(campaignIDs []string, facts map[string]CampaignSemantic) (SemanticUtility, bool) {
	unique := make(map[string]CampaignSemantic, len(campaignIDs))
	for _, id := range campaignIDs {
		if id == "" {
			continue
		}
		fact, ok := facts[id]
		if !ok {
			continue
		}
		unique[id] = fact
	}

	var utility SemanticUtility
	found := false
	primaryEligible := false
	for id, fact := range unique {
		if !found || fact.SemanticClass < utility.SemanticClass ||
			(fact.SemanticClass == utility.SemanticClass && fact.SecondaryEligible && !primaryEligible) ||
			(fact.SemanticClass == utility.SemanticClass && fact.SecondaryEligible == primaryEligible && id < utility.PrimaryCampaignID) {
			utility.SemanticClass = fact.SemanticClass
			utility.PrimaryCampaignID = id
			primaryEligible = fact.SecondaryEligible
			found = true
		}
	}
	if !found {
		return SemanticUtility{}, false
	}

	for id, fact := range unique {
		if id == utility.PrimaryCampaignID || !fact.SecondaryEligible {
			continue
		}
		if !utility.HasSecondary || fact.SemanticClass < utility.SecondarySemanticClass ||
			(fact.SemanticClass == utility.SecondarySemanticClass && id < utility.SecondaryCampaignID) {
			utility.SecondarySemanticClass = fact.SemanticClass
			utility.SecondaryCampaignID = id
			utility.HasSecondary = true
		}
	}
	return utility, true
}

// BuildSemanticUtilityWithRemainingWork intersects a previously published
// policy decision set with current per-channel progress evidence. Primary
// semantics stay authoritative, while a campaign may contribute secondary
// utility only when its exact CampaignID is present in remainingWorkCampaignIDs.
// The final projection still delegates to BuildSemanticUtility, preserving its
// CampaignID deduplication and one-secondary bound.
func BuildSemanticUtilityWithRemainingWork(
	campaignIDs []string,
	remainingWorkCampaignIDs []string,
	facts map[string]CampaignSemantic,
) (SemanticUtility, bool) {
	remaining := make(map[string]bool, len(remainingWorkCampaignIDs))
	for _, id := range remainingWorkCampaignIDs {
		if id != "" {
			remaining[id] = true
		}
	}

	currentFacts := make(map[string]CampaignSemantic, len(campaignIDs))
	for _, id := range campaignIDs {
		fact, ok := facts[id]
		if !ok {
			continue
		}
		if !remaining[id] {
			fact.SecondaryEligible = false
		}
		currentFacts[id] = fact
	}
	return BuildSemanticUtility(campaignIDs, currentFacts)
}

// CompareSemanticUtility returns positive when a is preferred to b. Primary
// policy semantics always dominate; only equal primaries consult secondary
// presence and then the single best secondary class. Campaign counts and IDs
// are intentionally absent from the comparator.
func CompareSemanticUtility(a, b SemanticUtility) int {
	if a.SemanticClass != b.SemanticClass {
		if a.SemanticClass < b.SemanticClass {
			return 1
		}
		return -1
	}
	if a.HasSecondary != b.HasSecondary {
		if a.HasSecondary {
			return 1
		}
		return -1
	}
	if !a.HasSecondary || a.SecondarySemanticClass == b.SecondarySemanticClass {
		return 0
	}
	if a.SecondarySemanticClass < b.SecondarySemanticClass {
		return 1
	}
	return -1
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
	f := Feasibility{
		DeadlineKnown:        !in.EndAt.IsZero(),
		SafetyReserveMinutes: safetyReserveMin,
	}
	if f.DeadlineKnown {
		f.TimeUntilEnd = in.EndAt.Sub(now)
		if f.TimeUntilEnd < 0 {
			f.TimeUntilEnd = 0
		}
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

	availMin := 0
	if f.DeadlineKnown {
		availMin = int(f.TimeUntilEnd/time.Minute) - safetyReserveMin
		f.CanCompleteNextReward = hasNext && availMin >= nr
		f.CanCompleteAll = availMin >= goalAll
	}

	switch {
	case f.DeadlineKnown && !in.EndAt.After(now):
		f.Status = StatusImpossible
	case !hasNext && f.MinutesToCompleteAll == 0:
		f.Status = StatusSafe // nothing left to earn
	case !f.DeadlineKnown:
		f.Status = StatusUnknown
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
	if !f.DeadlineKnown {
		d.Factors = append(d.Factors, Factor{Label: "campaign deadline unknown"})
	} else if f.TimeUntilEnd > 0 && f.TimeUntilEnd < endingSoonWindow {
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
		label := "campaign deadline unknown"
		if f.DeadlineKnown {
			label = fmt.Sprintf("ends in %s", f.TimeUntilEnd.Round(time.Minute))
		}
		d.Factors = append(d.Factors, Factor{Label: label})
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
// best-first. It also assigns SemanticClass: equal policy facts share a class,
// while CampaignID only orders presentation inside that class. Excluded
// campaigns (Skip / impossible) sort last. The ordering is deterministic, so
// identical inputs always produce identical output.
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

	// compareSemantics compares only policy facts. CampaignID is deliberately
	// excluded: it is a deterministic tie-break, not a semantic preference.
	compareSemantics := func(a, b ranked) int {
		if a.d.Excluded != b.d.Excluded {
			if !a.d.Excluded {
				return -1 // excluded last
			}
			return 1
		}
		if a.in.HighPriority != b.in.HighPriority {
			if a.in.HighPriority {
				return -1 // high priority first, in every mode
			}
			return 1
		}
		switch mode {
		case ModeSmart:
			if a.d.Total != b.d.Total {
				if a.d.Total > b.d.Total {
					return -1
				}
				return 1
			}
		case ModeEndingSoonest:
			if a.d.Feasibility.DeadlineKnown != b.d.Feasibility.DeadlineKnown {
				if a.d.Feasibility.DeadlineKnown {
					return -1 // real deadlines before unknown ones
				}
				return 1
			}
			if a.d.Feasibility.DeadlineKnown && !a.in.EndAt.Equal(b.in.EndAt) {
				if a.in.EndAt.Before(b.in.EndAt) {
					return -1
				}
				return 1
			}
		case ModeClosestToReward:
			if a.d.Feasibility.MinutesToNextReward != b.d.Feasibility.MinutesToNextReward {
				if a.d.Feasibility.MinutesToNextReward < b.d.Feasibility.MinutesToNextReward {
					return -1
				}
				return 1
			}
		case ModeLowAvailability:
			if a.in.EligibleLiveChannels != b.in.EligibleLiveChannels {
				if a.in.EligibleLiveChannels < b.in.EligibleLiveChannels {
					return -1
				}
				return 1
			}
		default: // ModeGameOrder
			if ai, bi := gameIdx(a.in.GameOrderIndex), gameIdx(b.in.GameOrderIndex); ai != bi {
				if ai < bi {
					return -1
				}
				return 1
			}
		}
		return 0
	}

	sort.SliceStable(items, func(i, j int) bool {
		if cmp := compareSemantics(items[i], items[j]); cmp != 0 {
			return cmp < 0
		}
		return items[i].in.CampaignID < items[j].in.CampaignID // deterministic tie-break
	})

	out := make([]Decision, len(items))
	var class SemanticClass
	for i := range items {
		if i > 0 && compareSemantics(items[i-1], items[i]) != 0 {
			class++
		}
		items[i].d.SemanticClass = class
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
