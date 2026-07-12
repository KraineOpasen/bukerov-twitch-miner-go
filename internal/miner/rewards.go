package miner

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// ListCustomRewards returns the current custom channel-points rewards for a
// tracked streamer, fetched fresh from Twitch so availability (cooldown, stock,
// paused) is up to date. Returns an error if the streamer is not tracked.
func (m *Miner) ListCustomRewards(username string) ([]*models.CustomReward, error) {
	s := m.streamers.Get(username)
	if s == nil {
		return nil, fmt.Errorf("streamer %q is not tracked", username)
	}
	return m.client.GetCustomRewards(s)
}

// RedeemCustomReward manually redeems a custom reward on behalf of the user.
// It re-fetches the reward list first so a reward that became unavailable
// between showing the list and clicking, or an insufficient balance, is caught
// and reported as a clear error rather than a raw API failure.
func (m *Miner) RedeemCustomReward(username, rewardID, textInput string) error {
	s := m.streamers.Get(username)
	if s == nil {
		return fmt.Errorf("streamer %q is not tracked", username)
	}

	rewards, err := m.client.GetCustomRewards(s)
	if err != nil {
		return fmt.Errorf("could not load rewards: %w", err)
	}

	reward := findReward(rewards, rewardID)
	if reward == nil || !reward.IsAvailable() {
		return api.ErrRewardUnavailable
	}

	if reward.IsUserInputRequired && strings.TrimSpace(textInput) == "" {
		return api.ErrRewardInputRequired
	}
	if reward.Cost > s.GetChannelPoints() {
		return api.ErrInsufficientPoints
	}

	if err := m.client.RedeemCustomReward(s, reward, textInput); err != nil {
		return err
	}

	slog.Info("Redeemed custom reward", "streamer", s.Username, "reward", reward.Title, "cost", reward.Cost)
	events.Record(events.TypeRewardRedeemed, s.Username, fmt.Sprintf("redeemed %q (-%d)", reward.Title, reward.Cost))
	return nil
}

// GetAutoRedeem returns the persisted auto-redeem configuration for a streamer
// (a disabled zero value when none is set).
func (m *Miner) GetAutoRedeem(username string) config.AutoRedeemConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config.AutoRedeem[strings.ToLower(username)]
}

// SetAutoRedeem persists the auto-redeem configuration for a streamer and
// resets its in-memory spend/window bookkeeping so the new budget takes effect
// from a clean slate. User-input rewards are stripped from the whitelist here
// as a defence-in-depth measure — they must never be auto-redeemed even if the
// UI somehow submits them.
func (m *Miner) SetAutoRedeem(username string, cfg config.AutoRedeemConfig) error {
	key := strings.ToLower(username)

	m.mu.Lock()
	if m.streamers == nil || m.streamers.Get(key) == nil {
		m.mu.Unlock()
		return fmt.Errorf("streamer %q is not tracked", username)
	}

	cfg.RewardIDs = dedupeStrings(cfg.RewardIDs)

	if m.config.AutoRedeem == nil {
		m.config.AutoRedeem = make(map[string]config.AutoRedeemConfig)
	}
	if cfg.Enabled || len(cfg.RewardIDs) > 0 || cfg.Budget > 0 {
		m.config.AutoRedeem[key] = cfg
	} else {
		delete(m.config.AutoRedeem, key)
	}

	// Fresh config → fresh budget window.
	delete(m.autoRedeemState, key)

	// Persist while holding the lock, mirroring ApplySettings, so the config
	// isn't mutated by another goroutine mid-marshal.
	var saveErr error
	if m.configPath != "" {
		saveErr = config.SaveConfig(m.configPath, m.config)
	}
	m.mu.Unlock()

	if saveErr != nil {
		slog.Error("Failed to save auto-redeem config", "streamer", username, "error", saveErr)
		return fmt.Errorf("failed to save config: %w", saveErr)
	}

	slog.Info("Updated auto-redeem config", "streamer", key, "enabled", cfg.Enabled, "budget", cfg.Budget, "rewards", len(cfg.RewardIDs))
	return nil
}

// evaluateAutoRedeem checks a streamer's whitelisted rewards and redeems any
// that are available and fit within the remaining budget. It is edge-triggered
// (a reward is redeemed once per availability window) and never touches
// user-input rewards. Called once per bonus-poll cycle for each online
// streamer.
func (m *Miner) evaluateAutoRedeem(s *models.Streamer) {
	m.mu.RLock()
	cfg, ok := m.config.AutoRedeem[s.Username]
	m.mu.RUnlock()

	if !ok || !cfg.Enabled || cfg.Budget <= 0 || len(cfg.RewardIDs) == 0 {
		return
	}

	rewards, err := m.client.GetCustomRewards(s)
	if err != nil {
		slog.Debug("Auto-redeem: failed to load rewards", "streamer", s.Username, "error", err)
		return
	}
	byID := make(map[string]*models.CustomReward, len(rewards))
	for _, r := range rewards {
		byID[r.ID] = r
	}

	for _, rewardID := range cfg.RewardIDs {
		reward := byID[rewardID]
		if reward == nil {
			continue
		}

		// A reward that is unavailable re-arms so the next time it becomes
		// available it can be redeemed again within budget.
		if !reward.IsAvailable() {
			m.clearAutoRedeemed(s.Username, rewardID)
			continue
		}

		// User-input rewards are never auto-redeemed — the bot cannot author
		// the text a human would.
		if reward.IsUserInputRequired {
			continue
		}

		if m.wasAutoRedeemed(s.Username, rewardID) {
			continue
		}

		spent := m.autoRedeemSpent(s.Username)
		remaining := cfg.Budget - spent
		if reward.Cost > remaining {
			slog.Debug("Auto-redeem: over budget, skipping",
				"streamer", s.Username, "reward", reward.Title, "cost", reward.Cost, "remaining", remaining)
			continue
		}
		if reward.Cost > s.GetChannelPoints() {
			continue
		}

		if err := m.client.RedeemCustomReward(s, reward, ""); err != nil {
			slog.Warn("Auto-redeem failed", "streamer", s.Username, "reward", reward.Title, "error", err)
			continue
		}

		newSpent := m.recordAutoRedeemed(s.Username, rewardID, reward.Cost)
		slog.Info("Auto-redeemed custom reward",
			"streamer", s.Username,
			"reward", reward.Title,
			"cost", reward.Cost,
			"spentTotal", newSpent,
			"budgetRemaining", cfg.Budget-newSpent,
		)
		events.Record(events.TypeRewardRedeemed, s.Username,
			fmt.Sprintf("auto-redeemed %q (-%d, %d/%d budget)", reward.Title, reward.Cost, newSpent, cfg.Budget))
	}
}

func (m *Miner) autoRedeemRuntimeFor(username string) *autoRedeemRuntime {
	rt := m.autoRedeemState[username]
	if rt == nil {
		rt = &autoRedeemRuntime{redeemed: make(map[string]bool)}
		m.autoRedeemState[username] = rt
	}
	return rt
}

func (m *Miner) autoRedeemSpent(username string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if rt := m.autoRedeemState[username]; rt != nil {
		return rt.spent
	}
	return 0
}

func (m *Miner) wasAutoRedeemed(username, rewardID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if rt := m.autoRedeemState[username]; rt != nil {
		return rt.redeemed[rewardID]
	}
	return false
}

func (m *Miner) clearAutoRedeemed(username, rewardID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rt := m.autoRedeemState[username]; rt != nil {
		delete(rt.redeemed, rewardID)
	}
}

// recordAutoRedeemed marks a reward as redeemed this window, adds its cost to
// the running spend, and returns the new total spent.
func (m *Miner) recordAutoRedeemed(username, rewardID string, cost int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.autoRedeemRuntimeFor(username)
	rt.redeemed[rewardID] = true
	rt.spent += cost
	return rt.spent
}

func findReward(rewards []*models.CustomReward, id string) *models.CustomReward {
	for _, r := range rewards {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
