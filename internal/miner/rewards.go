package miner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/eligibility"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

// rewardsClient is the narrow slice of the Twitch client the rewards paths
// use; a seam so evaluation is testable without network I/O (I8). Mirrors
// this repo's topicReconciler/renameAnalyticsService seam precedent.
type rewardsClient interface {
	GetCustomRewards(s *models.Streamer) ([]*models.CustomReward, error)
	RedeemCustomReward(s *models.Streamer, reward *models.CustomReward, textInput string) error
}

var _ rewardsClient = (*twitch.TwitchClient)(nil)

// rewards returns the rewardsClient to use for Twitch reward I/O: the
// tests-only seam m.rewardsAPI when injected, otherwise the real client. No
// m.mu is ever held across a call through the returned client (true today;
// keep it that way).
func (m *Miner) rewards() rewardsClient {
	if m.rewardsAPI != nil {
		return m.rewardsAPI
	}
	return m.client
}

// ListCustomRewards returns the current custom channel-points rewards for a
// tracked streamer, fetched fresh from Twitch so availability (cooldown, stock,
// paused) is up to date. Returns an error if the streamer is not tracked.
func (m *Miner) ListCustomRewards(username string) ([]*models.CustomReward, error) {
	s := m.streamers.Get(username)
	if s == nil {
		return nil, fmt.Errorf("streamer %q is not tracked", username)
	}
	return m.rewards().GetCustomRewards(s)
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

	// Centralized capability gate: a custom-reward redemption spends channel
	// points, so it is blocked when Channel Points are confirmed disabled or not
	// yet confirmed (unknown), with a user-safe reason distinct from offline.
	if err := pointsActionGate(s, eligibility.TaskCustomReward); err != nil {
		return err
	}

	rewards, err := m.rewards().GetCustomRewards(s)
	if err != nil {
		if friendly := humanizeRewardError(err); friendly != err {
			return friendly
		}
		return fmt.Errorf("could not load rewards: %w", err)
	}

	reward := findReward(rewards, rewardID)
	if reward == nil || !reward.IsAvailable() {
		return twitch.ErrRewardUnavailable
	}

	if reward.IsUserInputRequired && strings.TrimSpace(textInput) == "" {
		return twitch.ErrRewardInputRequired
	}
	if reward.Cost > s.GetChannelPoints() {
		return twitch.ErrInsufficientPoints
	}

	if err := m.rewards().RedeemCustomReward(s, reward, textInput); err != nil {
		return humanizeRewardError(err)
	}

	slog.Info("Redeemed custom reward", "streamer", s.GetUsername(), "reward", reward.Title, "cost", reward.Cost)
	events.Record(events.TypeRewardRedeemed, s.GetUsername(), fmt.Sprintf("redeemed %q (-%d)", reward.Title, reward.Cost))
	return nil
}

// humanizeRewardError maps low-level Twitch API failures to messages safe to
// show in the rewards modal, which prints err.Error() verbatim. Only
// ErrPersistedQueryNotFound needs translating: its raw text names internals
// ("persisted query", client IDs) and the outage lasts until the shipped query
// hashes are updated, so unlike other failures a retry hint would be wrong.
// Every other error passes through unchanged — the api package already phrases
// those as user-facing sentinels (not enough points, reward unavailable, ...).
func humanizeRewardError(err error) error {
	if errors.Is(err, twitch.ErrPersistedQueryNotFound) {
		return errors.New("twitch is temporarily rejecting the miner's requests (stale query metadata) — redemption is unavailable until it recovers")
	}
	return err
}

// GetAutoRedeem returns the persisted auto-redeem configuration for a streamer
// (a disabled zero value when none is set).
func (m *Miner) GetAutoRedeem(username string) config.AutoRedeemConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config.AutoRedeem[strings.ToLower(username)]
}

// configHasStreamerLocked reports whether cfg's persisted streamer list
// contains login, case-insensitively. This is the config-presence half of
// SetAutoRedeem's admission check ([R2], I6): the runtime-roster check alone
// leaves a window — between a removal/rename apply publishing its candidate
// config and CommitPlan reconciling the runtime roster — in which the OLD
// runtime state has not caught up yet, but a dashboard SetAutoRedeem could
// otherwise write a dead old-login key nothing will ever migrate, or
// resurrect a just-removed streamer's consent on disk (D2's exact harm).
// Caller holds m.mu.
func configHasStreamerLocked(cfg *config.Config, login string) bool {
	for _, sc := range cfg.Streamers {
		if strings.EqualFold(sc.Username, login) {
			return true
		}
	}
	return false
}

// SetAutoRedeem persists the auto-redeem configuration for a streamer and
// resets its in-memory spend/window bookkeeping — and bumps its generation
// (I5) — so the new budget takes effect from a clean slate for both this
// process and any auto-redeem evaluation already in flight against the old
// window (see evaluateAutoRedeem/autoRedeemStillCurrent).
//
// Admission requires the login be BOTH in the live runtime roster AND in the
// persisted config's streamer list (case-insensitive) [R2]. The config check
// closes the post-commit/pre-CommitPlan window a bare roster check would
// leave open: without it, a SetAutoRedeem landing in that window could
// resurrect a removed streamer's consent on disk (D2) or write a dead
// old-login key nothing ever migrates. [RR3] The one accepted false-refusal
// this causes: a login-collision SURVIVOR (kept in the runtime roster by the
// reconciler's own conflict rule while ApplyToConfig replaced cfg.Streamers
// with a posted list that omits it) cannot save auto-redeem settings until
// the config lists it again — a narrow, fail-closed cost judged acceptable.
// Do NOT "fix" this by admitting on ChannelID match instead: that reopens the
// rename dead-key hole (a post-commit, pre-CommitPlan entry carries the NEW
// login but the SAME ChannelID as the old one).
//
// The config mutation below is applied, then persisted; on a SaveConfig
// failure it is restored EXACTLY — including re-nilling the map if it was
// nil before this call — before returning the error (I6, fixes D5), so
// memory and disk can never diverge. Runtime state and the generation are
// touched ONLY after a successful save.
func (m *Miner) SetAutoRedeem(username string, cfg config.AutoRedeemConfig) error {
	key := strings.ToLower(username)
	if !m.beginConfigWrite() {
		return ErrShuttingDown
	}
	defer m.endConfigWrite()
	if m.configWriteBarrier != nil {
		m.configWriteBarrier()
	}

	m.mu.Lock()
	if m.streamers == nil || m.streamers.Get(key) == nil {
		m.mu.Unlock()
		return fmt.Errorf("streamer %q is not tracked", username)
	}
	if !configHasStreamerLocked(m.config, key) {
		m.mu.Unlock()
		return fmt.Errorf("streamer %q is not in the saved streamer list", username)
	}

	cfg.RewardIDs = dedupeStrings(cfg.RewardIDs)

	prevCfg, hadPrev := m.config.AutoRedeem[key]
	wasNil := m.config.AutoRedeem == nil
	if wasNil {
		m.config.AutoRedeem = make(map[string]config.AutoRedeemConfig)
	}
	if cfg.Enabled || len(cfg.RewardIDs) > 0 || cfg.Budget > 0 {
		m.config.AutoRedeem[key] = cfg
	} else {
		delete(m.config.AutoRedeem, key)
	}

	// Persist while holding the lock, mirroring ApplySettings, so the config
	// isn't mutated by another goroutine mid-marshal.
	var saveErr error
	if m.configPath != "" {
		saveErr = m.saveConfig(m.configPath, m.config)
	}
	if saveErr != nil {
		// Restore exactly, so a failed save leaves memory matching the
		// still-valid on-disk state — runtime state and generation are left
		// untouched because they were never touched on this failing path.
		if hadPrev {
			m.config.AutoRedeem[key] = prevCfg
		} else {
			delete(m.config.AutoRedeem, key)
		}
		if wasNil {
			m.config.AutoRedeem = nil
		}
		m.mu.Unlock()
		slog.Error("Failed to save auto-redeem config", "streamer", username, "error", saveErr)
		return fmt.Errorf("failed to save config: %w", saveErr)
	}

	// Fresh config -> fresh budget window: the state delete AND the
	// generation bump are both gated on the successful save (I5/I6) — a
	// stale evaluator cycle that snapshot the OLD generation can no longer
	// record into (or re-arm a reward within) this streamer's new window.
	delete(m.autoRedeemState, key)
	m.bumpAutoRedeemGenLocked(key)
	m.mu.Unlock()

	slog.Info("Updated auto-redeem config", "streamer", key, "enabled", cfg.Enabled, "budget", cfg.Budget, "rewards", len(cfg.RewardIDs))
	return nil
}

// evaluateAutoRedeem checks a streamer's whitelisted rewards and redeems any
// that are available and fit within the remaining budget. It is edge-triggered
// (a reward is redeemed once per availability window) and never touches
// user-input rewards. Called once per bonus-poll cycle for each online
// streamer.
//
// cfg, gen and runCtx are snapshotted together under ONE RLock at cycle
// start, keyed by s.GetUsername() at that moment. Every helper call below
// (wasAutoRedeemed, autoRedeemSpent, clearAutoRedeemed, recordAutoRedeemed,
// autoRedeemStillCurrent) re-reads s.GetUsername() at call time instead of
// reusing a cached key — today's shape, unchanged — because a rename can
// repoint the SAME *models.Streamer to a new login mid-evaluation; it is the
// generation migration (migrateAutoRedeemGenLocked), not key-pinning, that
// makes comparing the snapshotted gen against a later, different-keyed
// generation valid: value-identical config, one continued window (I5/C4).
func (m *Miner) evaluateAutoRedeem(s *models.Streamer) {
	m.mu.RLock()
	cfg, ok := m.config.AutoRedeem[s.GetUsername()]
	gen := m.autoRedeemGen[s.GetUsername()]
	runCtx := m.runCtx
	m.mu.RUnlock()

	if !ok || !cfg.Enabled || cfg.Budget <= 0 || len(cfg.RewardIDs) == 0 {
		return
	}

	rewards, err := m.rewards().GetCustomRewards(s)
	if err != nil {
		slog.Debug("Auto-redeem: failed to load rewards", "streamer", s.GetUsername(), "error", err)
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
			m.clearAutoRedeemed(s.GetUsername(), rewardID, gen)
			continue
		}

		// User-input rewards are never auto-redeemed — the bot cannot author
		// the text a human would.
		if reward.IsUserInputRequired {
			continue
		}

		if m.wasAutoRedeemed(s.GetUsername(), rewardID) {
			continue
		}

		spent := m.autoRedeemSpent(s.GetUsername())
		remaining := cfg.Budget - spent
		if reward.Cost > remaining {
			slog.Debug("Auto-redeem: over budget, skipping",
				"streamer", s.GetUsername(), "reward", reward.Title, "cost", reward.Cost, "remaining", remaining)
			continue
		}
		if reward.Cost > s.GetChannelPoints() {
			continue
		}

		// I9: the LAST gate before the irreversible network call. A stale
		// snapshot — a SetAutoRedeem, a committed removal, or a rename
		// migration's clash branch invalidated this window since gen was
		// taken, or the miner is shutting down — must never spend. On a
		// stale snapshot the WHOLE cycle stops (not just this reward): the
		// snapshotted cfg itself may no longer describe this streamer.
		if !m.autoRedeemStillCurrent(s.GetUsername(), gen, runCtx) {
			return
		}

		if err := m.rewards().RedeemCustomReward(s, reward, ""); err != nil {
			slog.Warn("Auto-redeem failed", "streamer", s.GetUsername(), "reward", reward.Title, "error", err)
			continue
		}

		newSpent, recorded := m.recordAutoRedeemed(s.GetUsername(), rewardID, reward.Cost, gen)
		if !recorded {
			// The redemption already happened on the wire — Twitch spent the
			// points — but crediting it to a window it no longer belongs to
			// would either inflate a budget the operator just reset, or
			// resurrect a runtime entry for a login the commit-point cleanup
			// already deleted. Stop the cycle; this one redemption's cost is
			// simply not tracked (bounded, single-reward residue, the same
			// inherent-TOCTOU class as any invalidation racing an in-flight
			// network call).
			slog.Warn("Auto-redeem: redemption completed against a stale window; not counted toward the new budget",
				"streamer", s.GetUsername(), "reward", reward.Title, "cost", reward.Cost)
			return
		}
		slog.Info("Auto-redeemed custom reward",
			"streamer", s.GetUsername(),
			"reward", reward.Title,
			"cost", reward.Cost,
			"spentTotal", newSpent,
			"budgetRemaining", cfg.Budget-newSpent,
		)
		events.Record(events.TypeRewardRedeemed, s.GetUsername(),
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

// clearAutoRedeemed re-arms rewardID for username so it can be redeemed again
// the next time it becomes available. Gen-guarded (I5) the same way
// recordAutoRedeemed is: when the live generation for username no longer
// matches gen, this is a no-op — a stale cycle must not reach into (and
// re-arm a reward inside) a window it no longer describes.
func (m *Miner) clearAutoRedeemed(username, rewardID string, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.autoRedeemGen[username] != gen {
		return
	}
	if rt := m.autoRedeemState[username]; rt != nil {
		delete(rt.redeemed, rewardID)
	}
}

// recordAutoRedeemed marks a reward as redeemed this window, adds its cost to
// the running spend, and returns the new total spent. Gen-guarded (I5): when
// the live generation for username no longer matches gen — a SetAutoRedeem, a
// committed removal, or a rename-migration clash invalidated this window
// since evaluateAutoRedeem's snapshot was taken — the record is REFUSED (the
// second return is false) and NO runtime state is created or mutated; the
// caller must log a WARN and stop its cycle (see evaluateAutoRedeem). The
// first return on refusal is the CURRENT spent total (0 if no state exists),
// for a caller that only wants the number, not the outcome.
func (m *Miner) recordAutoRedeemed(username, rewardID string, cost int, gen uint64) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.autoRedeemGen[username] != gen {
		if rt := m.autoRedeemState[username]; rt != nil {
			return rt.spent, false
		}
		return 0, false
	}
	rt := m.autoRedeemRuntimeFor(username)
	rt.redeemed[rewardID] = true
	rt.spent += cost
	return rt.spent, true
}

// autoRedeemStillCurrent reports whether an evaluator's earlier snapshot
// (gen, taken under the SAME RLock as cfg at cycle start, and runCtx, the
// miner's run-scoped context at that same moment) is still valid for key —
// false when the run context has since been cancelled (shutdown in
// progress), or when key's live generation no longer matches gen (I5/I9).
// Checked immediately before the irreversible RedeemCustomReward network
// call; false means the caller must stop the whole evaluation cycle rather
// than attempt to spend against a window that may already be gone.
func (m *Miner) autoRedeemStillCurrent(key string, gen uint64, runCtx context.Context) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if runCtx != nil && runCtx.Err() != nil {
		return false
	}
	return m.autoRedeemGen[key] == gen
}

// bumpAutoRedeemGenLocked increments key's auto-redeem generation (I5),
// lazily allocating the map so a struct-literal test Miner (which never runs
// New()) still works. Entries are never deleted — a re-added login must not
// restart at a generation a stale evaluator still holds — and are monotonic
// per key (never decreased), so a sealed generation can never match again.
// Caller holds m.mu. Called only on success/commit paths (a SetAutoRedeem
// save succeeding, a removal committing, a rename-migration clash) — never
// on a failed path.
func (m *Miner) bumpAutoRedeemGenLocked(login string) {
	if m.autoRedeemGen == nil {
		m.autoRedeemGen = make(map[string]uint64)
	}
	m.autoRedeemGen[login]++
}

// refreshCandidateAutoRedeemLocked rebuilds candidate.AutoRedeem at the
// commit point from the CURRENT live map, so a SetAutoRedeem committed while
// this apply was doing durable I/O off the miner lock is never lost (I1). It
// then re-applies this apply's own committed intents IN ORDER: rename
// migrations first (migrateAutoRedeem, destination-wins clash semantics
// preserved), then removal deletions — so a committed removal always wins
// over any concurrently-written entry, and a fresh copy can never resurrect a
// removed streamer's consent. [RR2] A rename INTO a login removed by the same
// apply is structurally unreachable (the planner's login-collision rule,
// manager.go:517-531, refuses the rename with ConflictLoginCollision); the
// copy -> migrate -> delete ordering is retained as defence in depth and
// pinned by a direct unit test, not an ApplySettings test.
//
// Returns the ALWAYS NON-NIL [RR1] set of lowercase new-login keys whose
// migration hit the destination-wins clash branch (the caller feeds it to
// migrateAutoRedeemGenLocked, merged with migrateAutoRedeemRuntimeState's own
// clash set). Caller MUST hold m.mu — enforced by a TryLock guard [R3]:
// acquiring m.mu here must be impossible.
func (m *Miner) refreshCandidateAutoRedeemLocked(candidate *config.Config, renames []streamer.RenameEvent, removedLogins []string) map[string]bool {
	if m.mu.TryLock() {
		m.mu.Unlock()
		panic("refreshCandidateAutoRedeemLocked: m.mu not held")
	}

	fresh := make(map[string]config.AutoRedeemConfig, len(m.config.AutoRedeem))
	for k, v := range m.config.AutoRedeem {
		fresh[k] = v
	}
	candidate.AutoRedeem = fresh

	clashes := make(map[string]bool)
	for _, r := range renames {
		if migrateAutoRedeem(candidate, r.OldLogin, r.NewLogin) {
			clashes[strings.ToLower(r.NewLogin)] = true
		}
	}

	// [RR6] Case-insensitive on purpose: a hand-edited config.json can carry
	// a mixed-case key (D6 is the follow-up for full load-time
	// normalization; this makes removal cleanup unconditionally correct — I4
	// — in the meantime). The runtime-state delete at the commit points in
	// miner.go stays EXACT-key on purpose: state keys are only ever created
	// from s.GetUsername() (always the canonical lowercase login), so an
	// EqualFold scan there would just be needless O(n) work for a case
	// mismatch that can never occur — don't "fix" it to match this one for
	// consistency.
	for _, login := range removedLogins {
		for k := range candidate.AutoRedeem {
			if strings.EqualFold(k, login) {
				delete(candidate.AutoRedeem, k)
			}
		}
	}

	return clashes
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
