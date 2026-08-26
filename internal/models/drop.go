package models

import (
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Claimability is the authoritative, server-derived claim state of a drop. It
// is deliberately tri-state so a drop is only ever claimed on an explicit
// authoritative "yes" from Twitch — never inferred from locally-counted watch
// minutes, a 100% progress bar, or the mere presence of a campaign.
type Claimability int

const (
	// ClaimabilityUnknown means Twitch has not (yet) provided an authoritative
	// claimable signal for this drop. Treated as NOT claimable (fail-closed).
	ClaimabilityUnknown Claimability = iota
	// ClaimabilityKnownFalse means Twitch authoritatively indicates the drop is
	// not claimable: it is already claimed, an explicit isClaimable==false, or
	// hasPreconditionsMet==false.
	ClaimabilityKnownFalse
	// ClaimabilityKnownTrue means Twitch authoritatively indicates the drop is
	// claimable: an explicit isClaimable==true, or Twitch has minted the drop
	// instance (dropInstanceID present) for an unclaimed, un-blocked drop.
	ClaimabilityKnownTrue
)

type Drop struct {
	ID   string
	Name string
	// Benefit is the reward's display name (benefitEdges[0].benefit.name).
	Benefit string
	// BenefitID is the reward's stable Benefit/Reward ID
	// (benefitEdges[0].benefit.id) when Twitch supplies it. The proven contract
	// (this repo's fixtures) does NOT currently carry it, so it is parsed
	// additively and is frequently empty; identity/claim-matching treats it as
	// strong evidence only when present, and never fabricates it.
	BenefitID string
	// BenefitType is the typed classification of the reward's benefit, decoded
	// from benefitEdges[0].benefit.distributionType (BADGE / EMOTE /
	// DIRECT_ENTITLEMENT). It drives the account-link eligibility decision:
	// only a direct entitlement requires a linked publisher account. An
	// absent/unknown type is BenefitTypeUnknown and fails open.
	BenefitType           BenefitType
	ImageURL              string
	MinutesRequired       int
	CurrentMinutesWatched int
	PercentageProgress    int
	HasPreconditionsMet   *bool
	DropInstanceID        string
	// IsClaimable mirrors Claimability == ClaimabilityKnownTrue for backward
	// compatibility with existing readers. It is a derived convenience only;
	// Claimability is the source of truth and CanClaim gates the actual claim.
	IsClaimable bool
	IsClaimed   bool
	// Claimability is the authoritative, server-derived tri-state claim status.
	// It is computed by Update from the inventory `self` data and never from the
	// locally-counted watch minutes.
	Claimability Claimability
	StartAt      time.Time
	EndAt        time.Time

	// SubscriberOnly is Twitch's best-effort subscriber-only flag for the drop;
	// SubscriberOnlyKnown records whether Twitch actually reported it. The
	// campaign policy's "Ignore subscriber-only" control honestly shows "no
	// effect" when the flag is not known, rather than pretending to gate on
	// data the API never provided.
	SubscriberOnly      bool
	SubscriberOnlyKnown bool
}

func NewDropFromGQL(data map[string]interface{}) *Drop {
	drop := &Drop{}

	if id, ok := data["id"].(string); ok {
		drop.ID = id
	}
	if name, ok := data["name"].(string); ok {
		drop.Name = name
	}

	if benefitEdges, ok := data["benefitEdges"].([]interface{}); ok && len(benefitEdges) > 0 {
		// Typed benefit classification (distributionType) is read from the same
		// first edge as the fields below; a missing/malformed value yields
		// BenefitTypeUnknown, which fails open (never treated as requiring a link).
		drop.BenefitType = parseDropBenefitType(benefitEdges)
		if edge, ok := benefitEdges[0].(map[string]interface{}); ok {
			if benefit, ok := edge["benefit"].(map[string]interface{}); ok {
				if name, ok := benefit["name"].(string); ok {
					drop.Benefit = name
				}
				// Additive: capture the Benefit/Reward ID when Twitch supplies
				// it. It is the strongest cross-variant reward identity, but the
				// current proven response shape omits it, so this is best-effort
				// and stays empty rather than being invented.
				if id, ok := benefit["id"].(string); ok {
					drop.BenefitID = id
				}
				// imageAssetURL is the reward's own artwork; the Drops-page
				// modal uses it as each drop's icon, falling back to the
				// campaign box art when Twitch omits it.
				if img, ok := benefit["imageAssetURL"].(string); ok {
					drop.ImageURL = img
				}
			}
		}
	}

	if mins, ok := data["requiredMinutesWatched"].(float64); ok {
		drop.MinutesRequired = int(mins)
	}

	if startAt, ok := data["startAt"].(string); ok {
		if t, err := time.Parse(time.RFC3339, startAt); err == nil {
			drop.StartAt = t
		}
	}
	if endAt, ok := data["endAt"].(string); ok {
		if t, err := time.Parse(time.RFC3339, endAt); err == nil {
			drop.EndAt = t
		}
	}

	// Best-effort subscriber-only flag: Twitch does not reliably expose it on
	// time-based drops, so try a couple of plausible keys and only mark it
	// "known" when one is actually present.
	for _, key := range []string{"isSubscriberOnly", "isSubscriptionOnly", "subscriberOnly"} {
		if v, ok := data[key].(bool); ok {
			drop.SubscriberOnly = v
			drop.SubscriberOnlyKnown = true
			break
		}
	}

	return drop
}

func (d *Drop) Update(selfData map[string]interface{}) {
	if mins, ok := selfData["currentMinutesWatched"].(float64); ok {
		d.CurrentMinutesWatched = int(mins)
	}
	if hasPre, ok := selfData["hasPreconditionsMet"].(bool); ok {
		d.HasPreconditionsMet = &hasPre
	}
	if instanceID, ok := selfData["dropInstanceID"].(string); ok {
		d.DropInstanceID = instanceID
	}
	if claimed, ok := selfData["isClaimed"].(bool); ok {
		d.IsClaimed = claimed
	}

	if d.MinutesRequired > 0 {
		d.PercentageProgress = (d.CurrentMinutesWatched * 100) / d.MinutesRequired
	}

	// Claimability is authoritative and server-derived only — it is NEVER
	// inferred from the locally-counted minutes above (that local heuristic was
	// the integrity bug: reaching the required minutes locally is not proof that
	// Twitch has authorized the claim). IsClaimable mirrors the known-true state
	// so existing readers keep working.
	d.Claimability = d.deriveClaimability(selfData)
	d.IsClaimable = d.Claimability == ClaimabilityKnownTrue
}

// deriveClaimability computes the authoritative, server-derived claim state from
// an inventory `self` object. Local watched minutes are never an input — they
// drive progress/UI only. Precedence (fail-closed):
//
//  1. Already claimed                       -> known false (nothing to claim).
//  2. hasPreconditionsMet == false          -> known false (authoritative block,
//     even if a stray isClaimable==true were also present).
//  3. explicit server isClaimable, if sent  -> its boolean value.
//  4. a Twitch-minted dropInstanceID         -> known true (the "ready to claim"
//     signal Twitch actually provides on the inventory shape, for an unclaimed,
//     un-blocked drop).
//  5. otherwise                             -> unknown (never claim).
//
// Note: the inventory `self` shape confirmed by this repository's fixtures does
// NOT carry an isClaimable field; step 3 exists so that if Twitch ever supplies
// one it is honored as the top authority, without inventing it when absent.
func (d *Drop) deriveClaimability(selfData map[string]interface{}) Claimability {
	if d.IsClaimed {
		return ClaimabilityKnownFalse
	}
	if d.HasPreconditionsMet != nil && !*d.HasPreconditionsMet {
		return ClaimabilityKnownFalse
	}
	if v, ok := selfData["isClaimable"].(bool); ok {
		if v {
			return ClaimabilityKnownTrue
		}
		return ClaimabilityKnownFalse
	}
	if d.DropInstanceID != "" {
		return ClaimabilityKnownTrue
	}
	return ClaimabilityUnknown
}

// CanClaim reports whether this drop may be submitted to the claim mutation:
// Twitch authoritatively marks it claimable (ClaimabilityKnownTrue) AND has
// minted the dropInstanceID the mutation requires AND it is not already claimed.
// Local watched minutes are never consulted, so a drop can never be claimed on
// locally-counted progress or a full progress bar alone.
func (d *Drop) CanClaim() bool {
	return d.Claimability == ClaimabilityKnownTrue && d.DropInstanceID != "" && !d.IsClaimed
}

// RequiresPublisherLink reports whether this drop's reward can only be earned
// with a linked publisher account (a direct in-game entitlement). Badge/emote
// and unknown/absent benefit types fail open (false), so such rewards are never
// account-link-blocked. Combined with the owning campaign's AccountConnection,
// this is the sole additional input to the account-link eligibility decision.
func (d *Drop) RequiresPublisherLink() bool {
	return d.BenefitType.RequiresPublisherLink()
}

func (d *Drop) DateTimeMatch() bool {
	now := time.Now()
	return d.StartAt.Before(now) && d.EndAt.After(now)
}

// HasDateWindow reports whether Twitch supplied a per-drop date window (either
// bound present). Inventory dropCampaignsInProgress entries frequently omit
// per-drop startAt/endAt, so a drop can be legitimately live with no window.
func (d *Drop) HasDateWindow() bool {
	return !d.StartAt.IsZero() || !d.EndAt.IsZero()
}

// InActiveWindow reports whether the drop should be treated as currently active.
// A drop whose window Twitch actually supplied must be inside it; a drop with NO
// window (both bounds zero — the common inventory-recovery shape) is treated as
// active. Membership in the inventory's in-progress list already proves Twitch
// considers the drop live, so discarding it merely for a missing window is the
// silent loss that made inventory-recovered campaigns flap in and out of the
// tracked set between the full sync (which keeps them) and the lightweight
// progress sync (which used to strip them via ClearClaimedDrops).
func (d *Drop) InActiveWindow() bool {
	if !d.HasDateWindow() {
		return true
	}
	return d.DateTimeMatch()
}

// ClampedProgress returns this drop's watch progress as a 0-100 percentage,
// clamped so an over-watched drop (Twitch occasionally reports more minutes
// than required) never renders past a full bar.
func (d *Drop) ClampedProgress() int {
	if d.MinutesRequired <= 0 {
		return 0
	}
	pct := (d.CurrentMinutesWatched * 100) / d.MinutesRequired
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

// MinutesRemaining reports how many more watched minutes this drop needs
// before its reward unlocks (never negative).
func (d *Drop) MinutesRemaining() int {
	remaining := d.MinutesRequired - d.CurrentMinutesWatched
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (d *Drop) IsPrintable() bool {
	return !d.IsClaimed && d.CurrentMinutesWatched > 0 && d.CurrentMinutesWatched < d.MinutesRequired
}

// Window returns the drop's entitlement window from its own per-drop
// startAt/endAt when Twitch supplied them (WindowSourceDrop). Inventory
// in-progress drops legitimately carry no dates — that yields a not-Known
// window (never mistaken for expired), which callers layer a campaign-level or
// inventory source onto. The window is deliberately NOT anchored to time.Now
// here; classification uses an injectable clock via EntitlementWindow.State.
func (d *Drop) Window() EntitlementWindow {
	if d.StartAt.IsZero() && d.EndAt.IsZero() {
		return EntitlementWindow{Source: WindowSourceNone, Known: false}
	}
	return EntitlementWindow{
		Start:  d.StartAt,
		End:    d.EndAt,
		Source: WindowSourceDrop,
		Known:  true,
	}
}

// Identity builds this drop's evidence-aware RewardIdentity within a campaign
// instance. gameID and campaignID come from the owning Campaign; window is the
// drop's own when known, otherwise the caller-supplied fallback (campaign-level
// or inventory) so an in-progress drop still carries the best available
// occurrence evidence. Evidence class is derived from which IDs are present.
func (d *Drop) Identity(gameID, campaignID string, fallback EntitlementWindow) RewardIdentity {
	w := d.Window()
	if !w.Known && fallback.Known {
		w = fallback
	}
	// DropInstanceID is the server-minted per-grant handle (strongest evidence
	// when present on BOTH sides). It is passed through so an instance-level
	// comparison is possible; MatchIdentity only confirms on an exact match of a
	// non-empty instance ID on both sides, so a one-sided instance never
	// fabricates cross-occurrence sameness.
	return NewRewardIdentity(gameID, d.BenefitID, d.DropInstanceID, d.ID, campaignID, d.Name, d.MinutesRequired, w)
}

// RewardKey is a NON-AUTHORITATIVE display/grouping key (game + normalized drop
// name). It is retained only for catalog grouping and legacy display, where a
// coarse, human-recognizable bucket is wanted. It MUST NOT be used as the sole
// evidence for claim deduplication — that path now runs through the
// evidence-aware RewardIdentity/MatchIdentity machinery, which keeps distinct
// tiers, benefits, and entitlement occurrences apart. See ClaimedReward and
// Campaign.ApplyClaimHistoryRecords.
func (d *Drop) RewardKey(gameID string) string {
	return NormalizeRewardKey(gameID, d.Name)
}

// NormalizeRewardKey builds a stable, case/whitespace-insensitive grouping
// identifier for a drop reward from its game and display name. It is a
// non-authoritative bucket (see Drop.RewardKey) — never a claim-history proof.
func NormalizeRewardKey(gameID, name string) string {
	return strings.ToLower(strings.TrimSpace(gameID)) + "::" + strings.ToLower(strings.TrimSpace(name))
}

// ErrInvalidRewardKey reports a composite reward key that cannot be produced
// by NormalizeRewardKey. Callers must reject it instead of persisting an
// unreachable alternate key.
var ErrInvalidRewardKey = errors.New("invalid reward key")

// maxRewardKeyInputBytes matches net/http's default application/x-www-form-
// urlencoded body ceiling. It bounds direct callers too, so an HTTP-controlled
// key cannot consume unbounded normalization work even if the handler's form
// parsing limit changes independently.
const maxRewardKeyInputBytes = 10 << 20

// NormalizeRewardKeyInput normalizes the case and outer whitespace accepted by
// the Drop Policy API, then verifies that the result belongs to the exact output
// space of NormalizeRewardKey. The producer permits empty components and
// unescaped "::" within either component. A delimiter is a valid component
// boundary exactly when neither adjacent component has trim-space at that
// boundary; checking that condition in one byte scan and invoking the owner once
// keeps work O(len(input)) while preserving every legitimately producible key.
func NormalizeRewardKeyInput(input string) (string, error) {
	if len(input) > maxRewardKeyInputBytes {
		return "", ErrInvalidRewardKey
	}
	candidate := strings.ToLower(strings.TrimSpace(input))
	for i := 0; i+1 < len(candidate); i++ {
		if candidate[i:i+2] != "::" {
			continue
		}
		if i > 0 {
			leftLast, _ := utf8.DecodeLastRuneInString(candidate[:i])
			if unicode.IsSpace(leftLast) {
				continue
			}
		}
		if i+2 < len(candidate) {
			rightFirst, _ := utf8.DecodeRuneInString(candidate[i+2:])
			if unicode.IsSpace(rightFirst) {
				continue
			}
		}
		canonical := NormalizeRewardKey(candidate[:i], candidate[i+2:])
		if canonical == candidate {
			return canonical, nil
		}
	}
	return "", ErrInvalidRewardKey
}
