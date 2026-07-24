package miner

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
)

// applyConfigRenames brings the persisted config in line with each confirmed
// runtime rename (streamer.Manager.ApplySettings' renamed return): the
// Username of the matching cfg.Streamers entry is updated in place —
// preserving its *models.StreamerSettings pointer untouched — and its
// resolved ChannelID is stamped. If BOTH an old-login and a new-login entry
// already exist in cfg.Streamers (the operator manually listed both, or a
// duplicate-config coalesce happened at the manager level), the old entry is
// dropped and the surviving new-login entry's own settings/ChannelID win —
// mirroring the manager's own coalesce choice, never silently merging two
// different settings blocks. AutoRedeem's per-login entry is migrated the
// same way, refusing to clobber an existing destination entry.
//
// Pure and side-effect-free besides mutating cfg; must run with the miner
// lock held (cfg is guarded by it) and BEFORE SaveConfig. Performs no I/O.
func applyConfigRenames(cfg *config.Config, renamed []streamer.RenameEvent) {
	for _, r := range renamed {
		oldIdx, newIdx := -1, -1
		for i := range cfg.Streamers {
			if strings.EqualFold(cfg.Streamers[i].Username, r.OldLogin) {
				oldIdx = i
			}
			if strings.EqualFold(cfg.Streamers[i].Username, r.NewLogin) {
				newIdx = i
			}
		}

		switch {
		case oldIdx >= 0 && newIdx >= 0 && oldIdx != newIdx:
			// Both entries exist: keep the new-login entry (its own settings),
			// drop the stale old-login one.
			cfg.Streamers[newIdx].ChannelID = r.ChannelID
			cfg.Streamers = append(cfg.Streamers[:oldIdx], cfg.Streamers[oldIdx+1:]...)
		case oldIdx >= 0:
			cfg.Streamers[oldIdx].Username = r.NewLogin
			cfg.Streamers[oldIdx].ChannelID = r.ChannelID
		case newIdx >= 0:
			// Entry already carries the new login (e.g. a repeated apply, or
			// the DTO round-trip already wrote it) — just stamp the ID.
			cfg.Streamers[newIdx].ChannelID = r.ChannelID
		}

		migrateAutoRedeem(cfg, r.OldLogin, r.NewLogin)
	}
}

// migrateAutoRedeem moves cfg.AutoRedeem[oldLogin] to [newLogin] when present.
// If the new login already has its own independently-configured entry, that
// destination entry wins (mirroring applyConfigRenames' own coalesce choice
// above: the surviving new-login entry's settings are never silently merged
// with the old one's) and oldLogin's now-orphaned entry is deleted rather than
// left behind — oldLogin no longer identifies any tracked streamer after the
// rename, so retaining its key would just be dead config. A privacy-safe
// warning is logged either way (only logins + ChannelID-free context, matching
// I13's log budget in spirit even though this specific warning is diagnostic,
// not the rename notice itself).
func migrateAutoRedeem(cfg *config.Config, oldLogin, newLogin string) {
	if cfg.AutoRedeem == nil {
		return
	}
	oldCfg, ok := cfg.AutoRedeem[oldLogin]
	if !ok {
		return
	}
	if _, clash := cfg.AutoRedeem[newLogin]; clash {
		slog.Warn("Auto-redeem config for renamed streamer was not migrated: the new login already has its own auto-redeem entry; discarding the old entry",
			"oldLogin", oldLogin, "newLogin", newLogin)
		delete(cfg.AutoRedeem, oldLogin)
		return
	}
	cfg.AutoRedeem[newLogin] = oldCfg
	delete(cfg.AutoRedeem, oldLogin)
}

// backfillChannelIDs stamps every cfg.Streamers entry's ChannelID from
// resolved, matched by CURRENT login (case-insensitive). It is best-effort
// and purely additive, and NEVER overwrites an already-non-empty ChannelID
// (BKM-006 Corrective Pass 1, C1): that field is an expected, immutable
// identity anchor once set, and a mismatch is a reconciliation conflict
// (handled entirely inside streamer.Manager) — never something this function
// silently papers over. An entry with no matching resolution (not yet
// resolved, or a genuinely unresolvable login) is left untouched. Must run
// with the miner lock held; performs no I/O.
func backfillChannelIDs(cfg *config.Config, resolved map[string]string) {
	for i := range cfg.Streamers {
		if cfg.Streamers[i].ChannelID != "" {
			continue
		}
		if id, ok := resolved[strings.ToLower(cfg.Streamers[i].Username)]; ok {
			cfg.Streamers[i].ChannelID = id
		}
	}
}

// channelIDsByLogin builds the login->ChannelID map backfillChannelIDs
// expects from a reconciled runtime roster (streamer.Manager.All()), keyed by
// each streamer's CURRENT login, lowercased. Entries with no resolved
// ChannelID yet are omitted, matching backfillChannelIDs' "leave untouched"
// contract for them.
func channelIDsByLogin(roster []*models.Streamer) map[string]string {
	out := make(map[string]string, len(roster))
	for _, s := range roster {
		if s.ChannelID == "" {
			continue
		}
		out[strings.ToLower(s.GetUsername())] = s.ChannelID
	}
	return out
}

// renameAnalyticsService is the slice of *analytics.Service the rename
// migration needs, narrowed to an interface so it is testable without a real
// database.
type renameAnalyticsService interface {
	RenameStreamer(oldName, newName string) error
}

// commitRenameTransaction performs the DURABLE half of a rename-carrying
// settings apply — analytics history migration, then config.json — entirely
// BEFORE any runtime mutation (BKM-006 Corrective Pass 1, C2). Each
// analyticsSvc.RenameStreamer call is itself a single atomic, collision-
// checking SQL transaction (internal/analytics/repository.go): a collision or
// a write failure leaves that streamer's analytics row completely untouched,
// so it doubles as both the preflight check and the commit for that one
// rename. If a LATER rename in a multi-rename batch fails after earlier ones
// already committed, or SaveConfig fails after every analytics commit
// succeeded, every already-committed analytics rename in THIS call is
// reversed (RenameStreamer called again with old/new swapped — itself
// idempotent and collision-safe) so this function is all-or-nothing from the
// caller's point of view: nil only when analytics and config.json both now
// agree with newConfig; a non-nil error leaves both durable stores at their
// pre-call state (a reversal failure is logged loudly, since it is the one
// path that can leave analytics history parked under the wrong login until
// the next successful rename apply retries it).
func commitRenameTransaction(configPath string, newConfig *config.Config, renames []streamer.RenameEvent, analyticsSvc renameAnalyticsService) error {
	var committed []streamer.RenameEvent
	rollback := func() {
		for i := len(committed) - 1; i >= 0; i-- {
			r := committed[i]
			if err := analyticsSvc.RenameStreamer(r.NewLogin, r.OldLogin); err != nil {
				slog.Error("Failed to reverse a partially committed analytics rename during a failed settings apply; analytics history may remain split across logins until the next successful rename apply",
					"oldLogin", r.OldLogin, "newLogin", r.NewLogin, "error", err)
			}
		}
	}

	if analyticsSvc != nil {
		for _, r := range renames {
			if err := analyticsSvc.RenameStreamer(r.OldLogin, r.NewLogin); err != nil {
				rollback()
				return fmt.Errorf("analytics history migration for %q -> %q: %w", r.OldLogin, r.NewLogin, err)
			}
			committed = append(committed, r)
		}
	}

	if configPath != "" {
		if err := config.SaveConfig(configPath, newConfig); err != nil {
			rollback()
			return fmt.Errorf("persisting config: %w", err)
		}
	}
	return nil
}

// migrateAutoRedeemRuntimeState moves the in-memory auto-redeem runtime
// bookkeeping (spent budget + redeemed-reward set) for each CONFIRMED rename
// from its old login key to its new one, in the SAME commit as the runtime
// rename and config.AutoRedeem migration (BKM-006 Corrective Pass 1, C4) — the
// caller runs this under m.mu, in the same locked section as the config
// swap, so no auto-redeem poll can ever observe the old key orphaned or the
// new key starting a fresh budget window. A destination collision (state
// already tracked under the new login) is merged conservatively: the
// redeemed sets are unioned (an already-redeemed reward is never re-armed by
// the merge) and spent is the MAX of the two (never increases the available
// budget = configured budget - spent; a plain max of two non-negative ints
// has no overflow to guard). Login keys are normalized to lowercase, matching
// GetUsername and every other auto-redeem lookup in this package.
func migrateAutoRedeemRuntimeState(state map[string]*autoRedeemRuntime, renamed []streamer.RenameEvent) {
	for _, r := range renamed {
		oldKey := strings.ToLower(r.OldLogin)
		newKey := strings.ToLower(r.NewLogin)
		if oldKey == newKey {
			continue
		}
		old, hadOld := state[oldKey]
		if !hadOld {
			continue
		}
		delete(state, oldKey)

		existing, hadNew := state[newKey]
		if !hadNew {
			state[newKey] = old
			continue
		}

		merged := &autoRedeemRuntime{
			spent:    maxInt(existing.spent, old.spent),
			redeemed: make(map[string]bool, len(existing.redeemed)+len(old.redeemed)),
		}
		for k := range existing.redeemed {
			merged.redeemed[k] = true
		}
		for k := range old.redeemed {
			merged.redeemed[k] = true
		}
		state[newKey] = merged
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
