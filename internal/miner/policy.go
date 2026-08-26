package miner

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
)

// policySnapshot is the published campaign-policy view: the ranked decisions
// (for the Drops page and debug snapshot) plus a by-ID index.
type policySnapshot struct {
	Mode      policy.Mode
	Decisions []policy.Decision
	byID      map[string]policy.Decision
}

// refreshPolicy re-ranks the tracked campaigns under the configured mode and
// publishes the result to the watcher (bounded semantic utility before
// persisted fairness), discovery (cross-game ordering), and its own snapshot
// (UI/debug).
// It runs on the existing
// health-watchdog tick, so it adds no goroutine and makes no Twitch calls —
// every input is derived from already-synced state. m.mu serializes the config
// snapshot through the three local publications: an ApplyCampaignPolicy /
// SetDropRule refresh can therefore never be overwritten by an older concurrent
// watchdog refresh. No network or persistence I/O occurs while this lock is
// held.
func (m *Miner) refreshPolicy(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshPolicyLocked(now)
}

// refreshPolicyLocked publishes one coherent policy evaluation from the
// caller's already-serialized config state. Caller holds m.mu.
func (m *Miner) refreshPolicyLocked(now time.Time) {
	if m.dropsTracker == nil {
		return
	}

	mode := policy.Normalize(m.config.CampaignPolicy)
	games := append([]string(nil), m.config.DirectoryGames...)

	// Private copies keep the pure ranker independent of later caller-side slice
	// or map replacement and document that no shared config reference escapes the
	// serialized refresh.
	rules := make(map[string]config.DropRule, len(m.config.DropRules))
	for key, rule := range m.config.DropRules {
		rules[key] = rule
	}

	campaigns := m.dropsTracker.Campaigns()
	inputs := m.buildPolicyInputs(campaigns, rules, games, now)
	decisions := policy.Rank(mode, inputs, now)

	byID := make(map[string]policy.Decision, len(decisions))
	for _, d := range decisions {
		byID[d.CampaignID] = d
	}
	gameRanks := policyGameRanks(decisions, campaigns)
	campaignSemantics := policyCampaignSemantics(decisions)

	if m.watcher != nil {
		m.watcher.SetCampaignSemanticPolicy(m.policyUtilitiesByLogin(campaignSemantics, campaigns), campaignSemantics, gameRanks)
	}
	if m.discovery != nil {
		m.discovery.SetCampaignPolicy(gameRanks, campaignSemantics)
	}
	m.policySnap.Store(&policySnapshot{Mode: mode, Decisions: decisions, byID: byID})
}

// buildPolicyInputs assembles one CampaignInput per trackable campaign from
// existing state (no Twitch calls). Campaigns with no current drop are skipped
// (nothing left to farm).
func (m *Miner) buildPolicyInputs(campaigns []*models.Campaign, rules map[string]config.DropRule, games []string, _ time.Time) []policy.CampaignInput {
	gameIndex := make(map[string]int, len(games))
	for i, g := range games {
		gameIndex[strings.ToLower(g)] = i
	}
	broker := m.watcher.BrokerSnapshot()

	inputs := make([]policy.CampaignInput, 0, len(campaigns))
	for _, c := range campaigns {
		drop := c.CurrentDrop()
		if drop == nil {
			continue
		}
		var gameID, gameName string
		if c.Game != nil {
			gameID = c.Game.ID
			gameName = campaignPolicyGameName(c.Game, gameIndex)
		}

		in := policy.CampaignInput{
			CampaignID:           c.ID,
			Name:                 c.Name,
			Game:                 gameName,
			Restricted:           c.IsChannelRestricted(),
			Started:              c.InInventory,
			EndAt:                c.EndAt,
			EligibleLiveChannels: m.eligibleLiveChannels(c),
			GameOrderIndex:       gameOrderIndex(gameIndex, gameName),
			SubscriberOnly:       drop.SubscriberOnly,
			SubscriberOnlyKnown:  drop.SubscriberOnlyKnown,
		}
		for _, d := range c.Drops {
			in.Drops = append(in.Drops, policy.DropStep{
				MinutesRequired:       d.MinutesRequired,
				CurrentMinutesWatched: d.CurrentMinutesWatched,
				IsClaimed:             d.IsClaimed,
			})
		}
		if r, ok := rules[models.NormalizeRewardKey(gameID, drop.Name)]; ok {
			in.Skip = r.Skip
			in.HighPriority = r.HighPriority
			in.AlwaysFinishStarted = r.AlwaysFinishStarted
			in.NextRewardOnly = r.NextRewardOnly
			in.IgnoreSubscriberOnly = r.IgnoreSubscriberOnly
		}

		// Farming channel (slotted + carries the campaign) → stability + stickiness.
		for _, slot := range broker.Slots {
			s := m.resolveStreamer(slot.Channel)
			if s == nil || !streamerCarriesCampaign(s, c.ID) {
				continue
			}
			in.WatchingHere = true
			in.ChannelStability, in.StabilitySamples = m.channelStability(slot.Channel)
			break
		}
		inputs = append(inputs, in)
	}
	return inputs
}

// eligibleLiveChannels estimates how many live channels can currently farm the
// campaign: configured online streamers carrying it, plus live directory
// channels of its game.
func (m *Miner) eligibleLiveChannels(c *models.Campaign) int {
	n := 0
	if m.streamers != nil {
		for _, s := range m.streamers.All() {
			if s.GetIsOnline() && streamerCarriesCampaign(s, c.ID) {
				n++
			}
		}
	}
	if m.discovery != nil && c.Game != nil {
		for _, ch := range m.discovery.State().Channels {
			if ch.Status == "offline" || !campaignMatchesGame(c.Game, ch.Game) {
				continue
			}
			s := m.discovery.StreamerFor(ch.Login)
			// Directory-level DROPS_ENABLED is game-wide evidence, not proof
			// that this exact channel advertises this exact campaign. Count
			// only the verified ID+ACL intersection discovery publishes on its
			// ephemeral Streamer; unknown candidates do not fabricate
			// LOW_AVAILABILITY capacity.
			if s != nil && streamerCarriesCampaign(s, c.ID) {
				n++
			}
		}
	}
	return n
}

// channelStability derives a 0..1 delivery-reliability score and its sample
// size from the watcher's per-slot report accounting. The policy engine gates
// the factor on a minimum sample size, so a fresh channel (few samples) is
// neutral rather than a confident extreme.
func (m *Miner) channelStability(login string) (stability float64, samples int) {
	if m.watcher == nil {
		return 1, 0
	}
	stats, ok := m.watcher.ReportStats(login)
	if !ok {
		return 1, 0
	}
	samples = stats.Successes + stats.Failures
	if samples == 0 {
		return 1, 0
	}
	return float64(stats.Successes) / float64(samples), samples
}

// policyUtilitiesByLogin projects Campaign Policy's bounded semantic utility
// onto each
// candidate using the eligibility evidence owned by that source. Configured
// streamers use tracker-assigned campaigns (the authoritative eligible set),
// while discovery uses its exact advertised campaign IDs plus the same channel
// ACL check as discovery.channelCarriesActiveCampaign. A game's best campaign
// is never assigned wholesale to every channel in that game.
func (m *Miner) policyUtilitiesByLogin(facts map[string]policy.CampaignSemantic, campaigns []*models.Campaign) map[string]policy.SemanticUtility {
	utilities := make(map[string]policy.SemanticUtility)
	configured := make(map[string]bool)
	if m.streamers != nil {
		for _, s := range m.streamers.All() {
			login := s.GetUsername()
			configured[strings.ToLower(login)] = true
			if utility, ok := bestAssignedPolicyUtility(s.Stream.GetCampaigns(), facts); ok {
				utilities[login] = utility
			}
		}
	}
	if m.discovery == nil {
		return utilities
	}

	campaignByID := make(map[string]*models.Campaign, len(campaigns))
	for _, c := range campaigns {
		if c == nil {
			continue
		}
		campaignByID[c.ID] = c
	}
	for _, ch := range m.discovery.State().Channels {
		if ch.Status == "offline" || configured[strings.ToLower(ch.Login)] {
			continue
		}
		s := m.discovery.StreamerFor(ch.Login)
		if s == nil {
			continue
		}
		if utility, ok := bestDiscoveredPolicyUtility(s, facts, campaignByID); ok {
			utilities[ch.Login] = utility
		}
	}
	return utilities
}

func bestAssignedPolicyUtility(campaigns []*models.Campaign, facts map[string]policy.CampaignSemantic) (policy.SemanticUtility, bool) {
	ids := make([]string, 0, len(campaigns))
	for _, c := range campaigns {
		if c == nil || c.CurrentDrop() == nil {
			continue
		}
		ids = append(ids, c.ID)
	}
	return policy.BuildSemanticUtility(ids, facts)
}

func bestDiscoveredPolicyUtility(s *models.Streamer, facts map[string]policy.CampaignSemantic, campaigns map[string]*models.Campaign) (policy.SemanticUtility, bool) {
	ids := make([]string, 0, len(s.Stream.GetCampaignIDs()))
	for _, id := range s.Stream.GetCampaignIDs() {
		c := campaigns[id]
		if c == nil || len(c.Drops) == 0 || c.ClaimStatus == models.CampaignClaimStatusAlreadyClaimed {
			continue
		}
		if !c.AllowsChannel(s.ChannelID) {
			continue
		}
		ids = append(ids, id)
	}
	return policy.BuildSemanticUtility(ids, facts)
}

// policyGameRanks maps each game to its best campaign semantic class (lower =
// higher priority) for discovery's directory/check pre-order. It is never the
// final class of every channel in that game: verified candidate selection uses
// policyCampaignSemantics plus exact advertised IDs and ACL. Campaign-ID ties do
// not manufacture different game ranks.
func policyGameRanks(decisions []policy.Decision, campaigns []*models.Campaign) map[string]int {
	gameOf := make(map[string][]string, len(campaigns))
	for _, c := range campaigns {
		for _, name := range campaignGameNames(c.Game) {
			gameOf[c.ID] = append(gameOf[c.ID], strings.ToLower(name))
		}
	}
	ranks := make(map[string]int)
	for _, d := range decisions {
		if d.Excluded {
			continue
		}
		for _, game := range gameOf[d.CampaignID] {
			rank := int(d.SemanticClass)
			if old, seen := ranks[game]; !seen || rank < old {
				ranks[game] = rank
			}
		}
	}
	return ranks
}

// policyCampaignSemantics is the exact campaign-ID semantic publication for
// configured and discovery channel projections. Excluded decisions remain
// valid presentation records but never gain semantic preference; unknown and
// completed decisions retain primary semantics while failing closed as a
// secondary.
func policyCampaignSemantics(decisions []policy.Decision) map[string]policy.CampaignSemantic {
	facts := make(map[string]policy.CampaignSemantic, len(decisions))
	for _, d := range decisions {
		if fact, ok := policy.CampaignSemanticFromDecision(d); ok {
			facts[d.CampaignID] = fact
		}
	}
	return facts
}

// campaignGameNames returns every Twitch name by which discovery may know a
// campaign's game. DisplayName-only campaigns are production-reachable (the
// drops tracker and discovery already accept them), while Name and DisplayName
// may also be distinct aliases for the same game.
func campaignGameNames(game *models.Game) []string {
	if game == nil {
		return nil
	}
	names := make([]string, 0, 2)
	for _, name := range []string{game.Name, game.DisplayName} {
		if name == "" {
			continue
		}
		duplicate := false
		for _, existing := range names {
			if strings.EqualFold(existing, name) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			names = append(names, name)
		}
	}
	return names
}

func campaignPolicyGameName(game *models.Game, configured map[string]int) string {
	names := campaignGameNames(game)
	for _, name := range names {
		if _, ok := configured[strings.ToLower(name)]; ok {
			return name
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

func campaignMatchesGame(game *models.Game, candidate string) bool {
	for _, name := range campaignGameNames(game) {
		if strings.EqualFold(name, candidate) {
			return true
		}
	}
	return false
}

// persistLocked writes the current config to disk if a path is configured.
// Caller holds m.mu.
func (m *Miner) persistLocked() error {
	if m.configPath != "" {
		if err := m.saveConfig(m.configPath, m.config); err != nil {
			slog.Error("Failed to save config", "error", err)
			return err
		}
	}
	return nil
}

func streamerCarriesCampaign(s *models.Streamer, campaignID string) bool {
	for _, cc := range s.Stream.GetCampaigns() {
		if cc.ID == campaignID {
			return true
		}
	}
	return false
}

func gameOrderIndex(index map[string]int, game string) int {
	if i, ok := index[strings.ToLower(game)]; ok {
		return i
	}
	return -1
}

// PolicySnapshot exposes the current ranked decisions for the Drops page and
// debug snapshot (web.PolicyProvider). Returns an empty snapshot before the
// first refresh.
func (m *Miner) PolicySnapshot() (policy.Mode, []policy.Decision) {
	s := m.policySnap.Load()
	if s == nil {
		return policy.DefaultMode, nil
	}
	return s.Mode, s.Decisions
}

// snapshotDropRules returns a private copy of the per-drop rules taken under the
// read lock, so callers can read it without holding m.mu while SetDropRule
// mutates the shared map under the write lock from another goroutine. Handing
// out the shared reference instead would be a concurrent map read/write — a
// fatal runtime error.
func (m *Miner) snapshotDropRules() map[string]config.DropRule {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]config.DropRule, len(m.config.DropRules))
	for k, v := range m.config.DropRules {
		out[k] = v
	}
	return out
}

// CurrentCampaignPolicy returns the active mode and a copy of the per-drop
// rules for the Drops-page controls.
func (m *Miner) CurrentCampaignPolicy() (string, map[string]config.DropRule) {
	m.mu.RLock()
	mode := string(policy.Normalize(m.config.CampaignPolicy))
	m.mu.RUnlock()
	return mode, m.snapshotDropRules()
}

// ApplyCampaignPolicy validates, applies (runtime, no restart), and persists a
// new policy mode, then re-ranks immediately so the change is visible at once.
func (m *Miner) ApplyCampaignPolicy(mode string) {
	if !m.beginConfigWrite() {
		return
	}
	defer m.endConfigWrite()
	if m.configWriteBarrier != nil {
		m.configWriteBarrier()
	}
	m.coordinatorMu.Lock()
	m.mu.Lock()
	m.config.CampaignPolicy = string(policy.Normalize(mode))
	_ = m.persistLocked()
	m.mu.Unlock()
	m.coordinatorMu.Unlock()
	m.refreshPolicy(time.Now())
}

// SetDropRule sets (or, when the rule is the zero value, clears — the "Reset
// rule" control) the per-drop override for a normalized reward key, persists,
// and re-ranks immediately.
func (m *Miner) SetDropRule(rewardKey string, rule config.DropRule) error {
	rewardKey, err := models.NormalizeRewardKeyInput(rewardKey)
	if err != nil {
		return err
	}
	if !m.beginConfigWrite() {
		return ErrShuttingDown
	}
	defer m.endConfigWrite()
	if m.configWriteBarrier != nil {
		m.configWriteBarrier()
	}

	// Settings candidates deliberately share the live DropRules map. Joining
	// the existing coordinator prevents a first write to a nil map from being
	// detached by a candidate that was snapshotted earlier, while m.mu remains
	// the common serializer for every config mutation and SaveConfig call.
	m.coordinatorMu.Lock()
	defer m.coordinatorMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()

	previous, hadPrevious := m.config.DropRules[rewardKey]
	wasNil := m.config.DropRules == nil
	if m.config.DropRules == nil {
		m.config.DropRules = map[string]config.DropRule{}
	}
	if rule == (config.DropRule{}) {
		delete(m.config.DropRules, rewardKey)
	} else {
		m.config.DropRules[rewardKey] = rule
	}
	if err := m.persistLocked(); err != nil {
		if hadPrevious {
			m.config.DropRules[rewardKey] = previous
		} else {
			delete(m.config.DropRules, rewardKey)
		}
		if wasNil {
			m.config.DropRules = nil
		}
		return fmt.Errorf("persist drop rule: %w", err)
	}

	// The durable rename is the transaction's commit point. Publish the exact
	// committed state before another config writer can acquire m.mu.
	m.refreshPolicyLocked(time.Now())
	return nil
}
