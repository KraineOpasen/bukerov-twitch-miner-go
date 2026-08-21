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
// publishes the result to the watcher (DROPS tie-break), discovery (cross-game
// ordering), and its own snapshot (UI/debug). It runs on the existing
// health-watchdog tick, so it adds no goroutine and makes no Twitch calls —
// every input is derived from already-synced state.
func (m *Miner) refreshPolicy(now time.Time) {
	if m.dropsTracker == nil {
		return
	}

	m.mu.RLock()
	mode := policy.Normalize(m.config.CampaignPolicy)
	games := m.config.DirectoryGames
	m.mu.RUnlock()

	// A COPY of the rules map, not the shared reference: buildPolicyInputs reads
	// it lock-free (below), while SetDropRule mutates m.config.DropRules under
	// the lock from another goroutine. Sharing the map reference here would be a
	// concurrent map read/write (a fatal runtime error). games is a slice whose
	// backing array is never mutated in place (writers replace the whole slice
	// under the lock), so capturing its header under RLock is safe as-is.
	rules := m.snapshotDropRules()

	campaigns := m.dropsTracker.Campaigns()
	inputs := m.buildPolicyInputs(campaigns, rules, games, now)
	decisions := policy.Rank(mode, inputs, now)

	byID := make(map[string]policy.Decision, len(decisions))
	for _, d := range decisions {
		byID[d.CampaignID] = d
	}

	if m.watcher != nil {
		m.watcher.SetCampaignScores(m.policyScoresByLogin(byID))
	}
	if m.discovery != nil {
		m.discovery.SetGameRanks(policyGameRanks(decisions, campaigns))
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
			gameID, gameName = c.Game.ID, c.Game.Name
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
			if ch.Status != "offline" && strings.EqualFold(ch.Game, c.Game.Name) {
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

// policyScoresByLogin maps each configured streamer to the best (highest) score
// among the non-excluded campaigns it carries, for the watcher's DROPS
// tie-break. Logins with no scored campaign are omitted.
func (m *Miner) policyScoresByLogin(byID map[string]policy.Decision) map[string]int {
	if m.streamers == nil {
		return nil
	}
	scores := make(map[string]int)
	for _, s := range m.streamers.All() {
		best, has := 0, false
		for _, cc := range s.Stream.GetCampaigns() {
			if d, ok := byID[cc.ID]; ok && !d.Excluded {
				if !has || d.Total > best {
					best, has = d.Total, true
				}
			}
		}
		if has {
			scores[s.GetUsername()] = best
		}
	}
	return scores
}

// policyGameRanks assigns each game its first-appearance rank in the ranked
// decision order (lower = higher priority), for the discovery cross-game
// ordering. Keyed by lowercase game name.
func policyGameRanks(decisions []policy.Decision, campaigns []*models.Campaign) map[string]int {
	gameOf := make(map[string]string, len(campaigns))
	for _, c := range campaigns {
		if c.Game != nil {
			gameOf[c.ID] = strings.ToLower(c.Game.Name)
		}
	}
	ranks := make(map[string]int)
	next := 0
	for _, d := range decisions {
		if d.Excluded {
			continue
		}
		g := gameOf[d.CampaignID]
		if g == "" {
			continue
		}
		if _, seen := ranks[g]; !seen {
			ranks[g] = next
			next++
		}
	}
	return ranks
}

// persistLocked writes the current config to disk if a path is configured,
// returning the write error so callers can fail closed on it. configPath ==
// "" is the documented library no-persist case and reports success. Caller
// holds m.mu (the deliberate SaveConfig-under-m.mu invariant — see
// cloneConfigLocked's [R7] note) and owns logging with its own mutation
// context, mirroring setAutoRedeem (rewards.go) and applyHealthSettings
// (health.go).
func (m *Miner) persistLocked() error {
	if m.configPath == "" {
		return nil
	}
	return config.SaveConfig(m.configPath, m.config)
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

// ApplyCampaignPolicy validates and applies (runtime, no restart) a new
// policy mode, durably persists it, and — only once that write has
// succeeded — keeps it and re-ranks immediately so the change is visible
// at once.
//
// Persistence is the commit point (configPath != ""): on a config.SaveConfig
// failure the previous raw value is restored inside the same m.mu critical
// section that applied the new one, so no reader — CurrentCampaignPolicy,
// refreshPolicy, a settings apply's clone, or the generation handoff's
// CurrentConfig — can ever observe the rejected mode, no snapshot is
// re-ranked from it, and a later unrelated save cannot launder it to disk.
// refreshPolicy runs only on the success path, after Unlock (it takes
// m.mu.RLock itself). configPath == "" stays the documented library
// no-persist case: a successful hot-apply that refreshes normally.
//
// A non-nil error means NOTHING was changed: ErrShuttingDown when this
// generation is no longer the authoritative mutable one (see fenced,
// miner.go), or a persistence failure on a live generation. Callers must
// not report success on a non-nil error.
func (m *Miner) ApplyCampaignPolicy(mode string) error {
	return m.fenced(func() error {
		m.mu.Lock()
		prev := m.config.CampaignPolicy
		m.config.CampaignPolicy = string(policy.Normalize(mode))
		if err := m.persistLocked(); err != nil {
			// Restore the raw stored value byte-exactly (not the mode it
			// normalizes to) before unlocking, so memory keeps matching the
			// still-valid on-disk state.
			m.config.CampaignPolicy = prev
			m.mu.Unlock()
			slog.Error("Failed to save config; campaign policy unchanged", "mode", mode, "error", err)
			return fmt.Errorf("campaign policy rejected; no changes were made: persist config: %w", err)
		}
		m.mu.Unlock()
		m.refreshPolicy(time.Now())
		return nil
	})
}

// SetDropRule sets (or, when the rule is the zero value, clears — the "Reset
// rule" control) the per-drop override for a normalized reward key, durably
// persists it, and — only once that write has succeeded — re-ranks
// immediately.
//
// A non-nil error means NOTHING was changed: ErrShuttingDown when this
// generation is no longer the authoritative mutable one (see fenced,
// miner.go), or a persistence failure on a live generation (commitDropRule
// is the commit point and rolls back exactly).
//
// The fence is entered BEFORE the empty-key check, not after. An empty or
// whitespace-only key is a no-op with nothing to refuse, so ordering it the
// other way would be defensible for this call alone — but it would let a
// retired generation answer a mutation endpoint with success, which is exactly
// the untruth this fence exists to prevent. Refusing first keeps "a retired
// generation never reports a configuration mutation as done" true without
// exception, and an empty key on a LIVE generation still returns nil as before.
func (m *Miner) SetDropRule(rewardKey string, rule config.DropRule) error {
	return m.fenced(func() error {
		rewardKey = strings.ToLower(strings.TrimSpace(rewardKey))
		if rewardKey == "" {
			return nil
		}
		return m.commitDropRule(rewardKey, rule)
	})
}

// commitDropRule performs the mutation itself. It takes m.mu (so it is NOT a
// "Locked" helper in this package's sense); it exists only so SetDropRule's
// fenced body stays a single statement.
//
// Persistence is the commit point (configPath != ""): the map entry is
// applied, then persisted under the same m.mu section; on a
// config.SaveConfig failure the exact pre-call state — value, presence, and
// map nil-ness — is restored before unlocking (the setAutoRedeem discipline,
// rewards.go), so the rejected rule is never observable to any reader, no
// snapshot is re-ranked from it, and a later unrelated save cannot launder
// it to disk. On the failure path the map is restored IN PLACE (including
// re-nilling a map this call allocated), so a rollback never changes the
// map identity a concurrent candidate aliases; on the success path a nil
// map is allocated exactly as before this change. cloneConfigLocked's
// load-bearing DropRules aliasing ([R7], miner.go) ultimately rests on
// every candidate's SaveConfig running under m.mu, which this path
// preserves; the pre-existing nil-to-allocated corner of the applySettings
// clone window (see applySettings' [M2/R6] note, miner.go) is unchanged by
// this fix. refreshPolicy runs only on the success path, after Unlock.
func (m *Miner) commitDropRule(rewardKey string, rule config.DropRule) error {
	m.mu.Lock()
	prevRule, hadPrev := m.config.DropRules[rewardKey]
	wasNil := m.config.DropRules == nil
	if wasNil {
		m.config.DropRules = map[string]config.DropRule{}
	}
	if rule == (config.DropRule{}) {
		delete(m.config.DropRules, rewardKey)
	} else {
		m.config.DropRules[rewardKey] = rule
	}
	if err := m.persistLocked(); err != nil {
		// Restore exactly — including re-nilling the map if it was nil
		// before this call — so memory keeps matching the still-valid
		// on-disk state.
		if hadPrev {
			m.config.DropRules[rewardKey] = prevRule
		} else {
			delete(m.config.DropRules, rewardKey)
		}
		if wasNil {
			m.config.DropRules = nil
		}
		m.mu.Unlock()
		slog.Error("Failed to save config; drop rule unchanged", "rewardKey", rewardKey, "error", err)
		return fmt.Errorf("drop rule rejected; no changes were made: persist config: %w", err)
	}
	m.mu.Unlock()
	m.refreshPolicy(time.Now())
	return nil
}
