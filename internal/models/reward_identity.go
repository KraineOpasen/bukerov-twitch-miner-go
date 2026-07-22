package models

import (
	"sort"
	"strings"
	"time"
)

// Clock is an injectable time source so entitlement-window and identity-match
// decisions are deterministic in tests. A nil Clock means "use the system
// clock", so existing callers that never set one keep working unchanged.
type Clock func() time.Time

// Now returns the clock's current time, falling back to time.Now for a nil
// Clock. Using a method (rather than calling the func directly) lets every
// call site treat the zero value as the system clock.
func (c Clock) Now() time.Time {
	if c == nil {
		return time.Now()
	}
	return c()
}

// WindowSource records where an EntitlementWindow's bounds came from. It exists
// so a window derived from a strong per-drop signal is trusted over one merely
// inferred from campaign bounds, and neither is confused with mere inventory
// membership (which proves "live now" but says nothing about a historical
// occurrence's identity).
type WindowSource uint8

const (
	// WindowSourceNone means no authoritative bounds were supplied.
	WindowSourceNone WindowSource = iota
	// WindowSourceDrop means the bounds came from per-drop startAt/endAt.
	WindowSourceDrop
	// WindowSourceCampaign means the bounds came from campaign-level startAt/endAt.
	WindowSourceCampaign
	// WindowSourceInventory means the drop was seen in the inventory's
	// dropCampaignsInProgress list: server evidence it is currently live, but
	// NOT proof of any historical occurrence identity, and (per the proven
	// Twitch contract) carrying no dates of its own.
	WindowSourceInventory
)

func (s WindowSource) String() string {
	switch s {
	case WindowSourceDrop:
		return "drop"
	case WindowSourceCampaign:
		return "campaign"
	case WindowSourceInventory:
		return "inventory"
	default:
		return "none"
	}
}

// EntitlementWindow is the half-open [Start, End) occurrence a reward belongs
// to. Known reports whether authoritative bounds were actually supplied by
// Twitch; a window that is not Known (or is malformed) must never be used to
// declare a reward permanently claimed — the whole point is that "no window"
// and "an unparseable window" are honestly distinct from "expired".
type EntitlementWindow struct {
	Start  time.Time
	End    time.Time
	Source WindowSource
	Known  bool
}

// wellFormed reports whether the window has at least one usable bound and, when
// both are present, is not inverted/degenerate (End must be strictly after
// Start). A malformed/inverted window is treated as unknown, never as expired.
func (w EntitlementWindow) wellFormed() bool {
	if w.Start.IsZero() && w.End.IsZero() {
		return false
	}
	if !w.Start.IsZero() && !w.End.IsZero() && !w.End.After(w.Start) {
		return false
	}
	return true
}

// WindowState is the half-open classification of an EntitlementWindow relative
// to a clock.
type WindowState uint8

const (
	// WindowStateUnknown means there are not enough authoritative bounds to
	// classify (no window, not Known, or malformed/inverted).
	WindowStateUnknown WindowState = iota
	WindowStateUpcoming
	WindowStateActive
	WindowStateExpired
)

func (s WindowState) String() string {
	switch s {
	case WindowStateUpcoming:
		return "upcoming"
	case WindowStateActive:
		return "active"
	case WindowStateExpired:
		return "expired"
	default:
		return "unknown"
	}
}

// State classifies the window using half-open semantics: upcoming when
// now < Start, active when Start <= now < End, expired when End <= now. An
// open-ended window (only one bound known) is classified from whichever bound
// is present. A window that is not Known or is malformed is always unknown.
func (w EntitlementWindow) State(clock Clock) WindowState {
	if !w.Known || !w.wellFormed() {
		return WindowStateUnknown
	}
	now := clock.Now()
	if !w.Start.IsZero() && now.Before(w.Start) {
		return WindowStateUpcoming
	}
	if !w.End.IsZero() && !now.Before(w.End) { // now >= End
		return WindowStateExpired
	}
	return WindowStateActive
}

// Overlaps reports whether two Known, well-formed windows share any instant
// under half-open semantics. Open-ended bounds are treated as -inf/+inf. When
// either window is unknown/malformed the answer is not decidable and Overlaps
// returns false (callers must consult Decidable first when the distinction
// matters).
func (w EntitlementWindow) Overlaps(o EntitlementWindow) bool {
	if !w.Decidable() || !o.Decidable() {
		return false
	}
	// w.Start < o.End  &&  o.Start < w.End, with zero bounds meaning unbounded.
	if !w.Start.IsZero() && !o.End.IsZero() && !w.Start.Before(o.End) {
		return false
	}
	if !o.Start.IsZero() && !w.End.IsZero() && !o.Start.Before(w.End) {
		return false
	}
	return true
}

// Decidable reports whether the window carries authoritative, well-formed
// bounds that can be reasoned about (Known and not malformed).
func (w EntitlementWindow) Decidable() bool {
	return w.Known && w.wellFormed()
}

// DisjointFrom reports whether both windows are decidable and provably do not
// overlap — i.e. they are different entitlement occurrences. When either window
// is not decidable the occurrences cannot be proven distinct and DisjointFrom
// returns false (fail open: do not treat as a new occurrence without evidence).
func (w EntitlementWindow) DisjointFrom(o EntitlementWindow) bool {
	if !w.Decidable() || !o.Decidable() {
		return false
	}
	return !w.Overlaps(o)
}

// IdentityEvidence ranks how strongly a RewardIdentity is pinned to a concrete
// Twitch reward, from strongest to weakest. It is the evidence hierarchy the
// matcher uses to decide whether claim history may remove a drop.
type IdentityEvidence uint8

const (
	// EvidenceNone means there is not even a usable name — no identity at all.
	EvidenceNone IdentityEvidence = iota
	// EvidenceNameOnly is the weakest fallback: game + normalized display name,
	// with no server ID. Never sufficient on its own to confirm a claim.
	EvidenceNameOnly
	// EvidenceComposite is an exact campaign ID + drop ID for the same campaign
	// instance. Reliable within that instance/occurrence only.
	EvidenceComposite
	// EvidenceBenefit is an exact Benefit/Reward ID plus an entitlement window,
	// the strongest cross-variant identity the proven contract can carry when
	// Twitch supplies benefit IDs.
	EvidenceBenefit
	// EvidenceInstance is an exact server-minted entitlement/reward instance ID
	// (e.g. dropInstanceID) — a per-grant handle, unique when present.
	EvidenceInstance
)

func (e IdentityEvidence) String() string {
	switch e {
	case EvidenceNameOnly:
		return "name_only"
	case EvidenceComposite:
		return "campaign_drop_composite"
	case EvidenceBenefit:
		return "benefit_id"
	case EvidenceInstance:
		return "instance_id"
	default:
		return "none"
	}
}

// RewardIdentity is the evidence-aware identity of a single drop reward. It
// deliberately separates the several concepts the old name-only RewardKey
// conflated: the game, the benefit, the drop, the campaign instance, the
// display name, the watch requirement, and the entitlement window. No field is
// invented — each is populated only from data the proven Twitch contract
// actually supplies (benefit/instance IDs are captured additively when present
// and simply stay empty when Twitch omits them).
type RewardIdentity struct {
	GameID          string
	BenefitID       string
	InstanceID      string
	DropID          string
	CampaignID      string
	CanonicalName   string
	MinutesRequired int
	Window          EntitlementWindow
	Evidence        IdentityEvidence
}

// canonicalName normalizes a display name the same case/whitespace-insensitive
// way the legacy key did, so the weak name fallback behaves consistently.
func canonicalName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// canonicalGame normalizes a game ID (opaque, but trimmed/lower-cased so an
// incidental case/space difference never fabricates a cross-game mismatch).
func canonicalGame(gameID string) string {
	return strings.ToLower(strings.TrimSpace(gameID))
}

// NewRewardIdentity builds a RewardIdentity from its parts and derives the
// evidence class from which fields are actually present. It never fabricates
// evidence: an identity with only a name lands at EvidenceNameOnly.
func NewRewardIdentity(gameID, benefitID, instanceID, dropID, campaignID, name string, minutes int, window EntitlementWindow) RewardIdentity {
	id := RewardIdentity{
		GameID:          strings.TrimSpace(gameID),
		BenefitID:       strings.TrimSpace(benefitID),
		InstanceID:      strings.TrimSpace(instanceID),
		DropID:          strings.TrimSpace(dropID),
		CampaignID:      strings.TrimSpace(campaignID),
		CanonicalName:   canonicalName(name),
		MinutesRequired: minutes,
		Window:          window,
	}
	switch {
	case id.InstanceID != "":
		id.Evidence = EvidenceInstance
	case id.BenefitID != "":
		id.Evidence = EvidenceBenefit
	case id.DropID != "" && id.CampaignID != "":
		id.Evidence = EvidenceComposite
	case id.CanonicalName != "":
		id.Evidence = EvidenceNameOnly
	default:
		id.Evidence = EvidenceNone
	}
	return id
}

// IdentityMatch is the tri-state result of comparing a claimed reward against a
// candidate drop. The whole design hinges on keeping Ambiguous distinct from
// both NoMatch and Confirmed so the pipeline can fail open (retain the drop)
// without either fabricating an "already claimed" label or losing the drop.
type IdentityMatch uint8

const (
	// IdentityNoMatch: the candidate is provably a different reward and stays
	// farmable.
	IdentityNoMatch IdentityMatch = iota
	// IdentityMatchConfirmed: strong evidence the candidate is the same reward
	// already granted for the same entitlement occurrence — safe to treat as
	// claimed (for the purpose of not wasting watch time; the claim mutation is
	// still separately gated by CanClaim).
	IdentityMatchConfirmed
	// IdentityMatchAmbiguous: some signal lines up but the evidence is too weak
	// to prove sameness (name-only, missing game, unknown window). Fail open:
	// the candidate stays farmable and must never be labelled already-claimed.
	IdentityMatchAmbiguous
)

func (m IdentityMatch) String() string {
	switch m {
	case IdentityMatchConfirmed:
		return "confirmed"
	case IdentityMatchAmbiguous:
		return "ambiguous"
	default:
		return "no_match"
	}
}

// gamesConflict reports whether two identities carry different, both-present
// game IDs (a hard mismatch). A missing game on either side is not a conflict —
// it is merely non-distinguishing (and downstream weakens the match).
func gamesConflict(a, b RewardIdentity) bool {
	ga, gb := canonicalGame(a.GameID), canonicalGame(b.GameID)
	return ga != "" && gb != "" && ga != gb
}

// MatchIdentity compares a previously-claimed reward identity against a
// candidate drop identity and returns the tri-state match. It performs NO fuzzy
// matching whatsoever (no substring, Levenshtein, or translation guessing): a
// strong ID either matches exactly or it does not. The policy, strongest to
// weakest:
//
//   - Different games (both known) => NoMatch.
//   - Instance ID present on both & equal => Confirmed (unique per-grant handle).
//   - Benefit ID present on both:
//     equal + windows not provably disjoint => Confirmed;
//     equal + windows provably disjoint     => NoMatch (new occurrence);
//     unequal                               => NoMatch (different benefit,
//     even under an identical name).
//   - Composite (campaign+drop) present on both:
//     equal campaign & drop + not provably disjoint => Confirmed;
//     equal drop but different campaign             => Ambiguous (drop IDs are
//     not globally eternal);
//     otherwise                                     => NoMatch.
//   - Name fallback (no strong IDs to compare):
//     different normalized names => NoMatch (no fuzzy matching);
//     same name, provably disjoint windows => NoMatch (new occurrence);
//     same name otherwise => Ambiguous (never confirmed on name alone).
//
// The campaign-level applier layers a strict, uniqueness-aware upgrade on top of
// the Ambiguous result (see Campaign.ApplyClaimHistoryRecords); MatchIdentity
// itself never confirms on a weak name.
func MatchIdentity(claimed, candidate RewardIdentity, clock Clock) IdentityMatch {
	if gamesConflict(claimed, candidate) {
		return IdentityNoMatch
	}

	// Strongest: server-minted instance handle.
	if claimed.InstanceID != "" && candidate.InstanceID != "" {
		if claimed.InstanceID == candidate.InstanceID {
			return IdentityMatchConfirmed
		}
		// Distinct instances are distinct grants only when we cannot otherwise
		// tell (they may be different tiers). Fall through to weaker evidence.
	}

	// Benefit/Reward ID identifies a reward FAMILY, not a specific repeatable
	// entitlement occurrence. It confirms a claim ONLY together with positive
	// occurrence evidence: both windows decidable AND overlapping. Disjoint known
	// windows are a genuinely new occurrence (NoMatch); if EITHER window is
	// unknown/malformed we cannot prove the same occurrence, so we fail open to
	// Ambiguous (never confirm a repeatable reward on unknown historical window).
	if claimed.BenefitID != "" && candidate.BenefitID != "" {
		if claimed.BenefitID != candidate.BenefitID {
			return IdentityNoMatch
		}
		if !claimed.Window.Decidable() || !candidate.Window.Decidable() {
			return IdentityMatchAmbiguous
		}
		if claimed.Window.Overlaps(candidate.Window) {
			return IdentityMatchConfirmed
		}
		return IdentityNoMatch
	}

	// Composite campaign+drop identity (same instance).
	if claimed.DropID != "" && candidate.DropID != "" {
		sameDrop := claimed.DropID == candidate.DropID
		sameCampaign := claimed.CampaignID != "" && candidate.CampaignID != "" &&
			claimed.CampaignID == candidate.CampaignID
		switch {
		case sameDrop && sameCampaign:
			if claimed.Window.DisjointFrom(candidate.Window) {
				return IdentityNoMatch
			}
			return IdentityMatchConfirmed
		case sameDrop && claimed.CampaignID != "" && candidate.CampaignID != "":
			// Same drop ID under a different campaign instance: drop IDs are not
			// globally eternal, so this is not conclusive.
			return IdentityMatchAmbiguous
		case sameDrop:
			// Same drop ID with a missing campaign on one side: weak.
			return IdentityMatchAmbiguous
		default:
			return IdentityNoMatch
		}
	}

	// Weak name fallback — no strong IDs to compare.
	if claimed.CanonicalName == "" || candidate.CanonicalName == "" {
		return IdentityMatchAmbiguous
	}
	if claimed.CanonicalName != candidate.CanonicalName {
		return IdentityNoMatch
	}
	if claimed.Window.DisjointFrom(candidate.Window) {
		return IdentityNoMatch
	}
	return IdentityMatchAmbiguous
}

// ClaimedReward is one evidence-rich record from the account-wide claim history
// (Twitch's gameEventDrops). It replaces the old lossy map[string]bool: instead
// of a single game::name key it carries the full identity (with whatever IDs and
// window Twitch actually supplied) so matching can be evidence-aware and
// fail open on ambiguity.
type ClaimedReward struct {
	Identity  RewardIdentity
	ClaimedAt time.Time
}

// dedupeKey builds a deterministic key for collapsing duplicate claim-history
// rows. It uses the strongest identity component available so two rows for the
// same grant collapse, while rows for genuinely different rewards do not.
func (c ClaimedReward) dedupeKey() string {
	id := c.Identity
	switch {
	case id.InstanceID != "":
		return "i\x00" + id.InstanceID
	case id.BenefitID != "":
		return "b\x00" + id.BenefitID + "\x00" + windowKey(id.Window)
	case id.DropID != "":
		return "d\x00" + id.CampaignID + "\x00" + id.DropID
	default:
		return "n\x00" + canonicalGame(id.GameID) + "\x00" + id.CanonicalName + "\x00" + windowKey(id.Window)
	}
}

// windowKey renders a window into a stable string for dedup keys. Unknown
// windows collapse to a single sentinel so name-only rows without windows dedup
// together (rather than multiplying).
func windowKey(w EntitlementWindow) string {
	if !w.Decidable() {
		return "?"
	}
	return w.Start.UTC().Format(time.RFC3339) + "/" + w.End.UTC().Format(time.RFC3339)
}

// DedupeClaimedRewards removes duplicate claim-history records deterministically
// (stable order preserved by first occurrence), so downstream matching and any
// annotations/events are not multiplied by repeated inventory rows.
func DedupeClaimedRewards(records []ClaimedReward) []ClaimedReward {
	seen := make(map[string]struct{}, len(records))
	out := make([]ClaimedReward, 0, len(records))
	for _, r := range records {
		k := r.dedupeKey()
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, r)
	}
	return out
}

// SortClaimedRewards orders records deterministically (by dedupe key) so logs
// and tests are stable regardless of inventory iteration order.
func SortClaimedRewards(records []ClaimedReward) {
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].dedupeKey() < records[j].dedupeKey()
	})
}
