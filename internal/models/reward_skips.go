package models

// RewardSkips is the effective farming-exclusion decision derived from the
// operator's per-drop rules (config.DropRules entries with Skip=true). It is
// the ONE shared interpretation consumed by every side-effect boundary that
// must honor "Skip excludes the reward from farming entirely": the drops
// tracker's broker-facing assignment views, discovery proposals, watcher slot
// admission, and both automatic drop-claim sites. Duplicating the rule lookup
// in those consumers instead would let their interpretations drift.
//
// Keys are stored verbatim, exactly as they appear in the config map, and
// looked up by NormalizeRewardKey output — the same semantics as the policy
// ranker's rules[NormalizeRewardKey(gameID, drop.Name)] lookup, so a legacy
// non-canonical config key stays inert here precisely where it is inert
// there. A nil *RewardSkips excludes nothing; the value is immutable after
// construction and therefore safe to share across goroutines without locks.
type RewardSkips struct {
	keys map[string]struct{}
}

// NewRewardSkips builds an immutable skip set from the operator's rule keys,
// stored verbatim (canonical NormalizeRewardKey output in the normal case).
// The slice is copied; nil or empty input yields a decision that excludes
// nothing (equivalent to a nil *RewardSkips).
func NewRewardSkips(keys []string) *RewardSkips {
	if len(keys) == 0 {
		return &RewardSkips{}
	}
	set := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		set[key] = struct{}{}
	}
	return &RewardSkips{keys: set}
}

// SkipsKey reports whether the exact key is farming-excluded. Nil-safe.
func (s *RewardSkips) SkipsKey(key string) bool {
	if s == nil || len(s.keys) == 0 {
		return false
	}
	_, ok := s.keys[key]
	return ok
}

// SkipsReward reports whether the reward identified by (gameID, dropName) is
// farming-excluded, using the canonical normalized reward identity. Nil-safe.
func (s *RewardSkips) SkipsReward(gameID, dropName string) bool {
	if s == nil || len(s.keys) == 0 {
		return false
	}
	return s.SkipsKey(NormalizeRewardKey(gameID, dropName))
}

// SkipsProvisionalCandidate reports whether a provisional observation
// candidate's reward is farming-excluded. The candidate is minted from its
// campaign's CURRENT drop, so this is the same current-drop interpretation
// the other boundaries use, expressed on the candidate tuple the watcher
// actually holds. The lookup keys on whatever label the candidate carries in
// Drop — a non-name label (the minting side may fall back to an ID when
// Twitch reports an empty name) can never match a name-keyed rule and fails
// open, exactly like every other name-keyed gate. Nil-safe.
func (s *RewardSkips) SkipsProvisionalCandidate(c ProvisionalDropCandidate) bool {
	return s.SkipsReward(c.GameID, c.Drop)
}

// SkipsCampaignCurrentDrop reports whether the campaign is farming-excluded
// right now: true exactly when its CurrentDrop's reward identity carries a
// Skip rule. This mirrors the policy ranker's campaign-level interpretation
// (the rule is keyed by the campaign's current drop only), so a campaign
// whose skipped drop is finished or claimed — CurrentDrop has moved on —
// becomes farmable again and never blocks unrelated later rewards. Nil-safe
// on both receiver and campaign; a campaign with no current drop is not
// excluded by this decision (it has nothing left to farm anyway).
func (s *RewardSkips) SkipsCampaignCurrentDrop(c *Campaign) bool {
	if s == nil || len(s.keys) == 0 || c == nil {
		return false
	}
	drop := c.CurrentDrop()
	if drop == nil {
		return false
	}
	var gameID string
	if c.Game != nil {
		gameID = c.Game.ID
	}
	return s.SkipsReward(gameID, drop.Name)
}
