package miner

import (
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

// backfillChannelIDs stamps every cfg.Streamers entry's ChannelID from the
// reconciled runtime roster, matched by CURRENT login (case-insensitive). It
// is best-effort and purely additive: an entry with no matching runtime
// streamer (not yet resolved, or a genuinely unresolvable login) is left
// untouched. Must run with the miner lock held; performs no I/O.
func backfillChannelIDs(cfg *config.Config, roster []*models.Streamer) {
	byLogin := make(map[string]string, len(roster))
	for _, s := range roster {
		if s.ChannelID == "" {
			continue
		}
		byLogin[strings.ToLower(s.GetUsername())] = s.ChannelID
	}
	for i := range cfg.Streamers {
		if id, ok := byLogin[strings.ToLower(cfg.Streamers[i].Username)]; ok {
			cfg.Streamers[i].ChannelID = id
		}
	}
}

// migrateRenamesToPersistence performs every I/O side effect a confirmed
// rename requires OUTSIDE the miner lock: migrating the streamer's analytics
// history to the new login (fail-closed on a genuine name collision — never a
// silent merge) and emitting exactly one privacy-safe log line (old login,
// new login, ChannelID only — no tokens/URLs/headers/payloads) per event.
// Chat presence (exactly one Leave of the old IRC channel) is handled
// separately by reconcileRuntimeCapabilities, which also runs unlocked.
func (m *Miner) migrateRenamesToPersistence(renamed []streamer.RenameEvent, analyticsSvc renameAnalyticsService) {
	for _, r := range renamed {
		if analyticsSvc != nil {
			if err := analyticsSvc.RenameStreamer(r.OldLogin, r.NewLogin); err != nil {
				slog.Warn("Streamer analytics history was not migrated to the new login (conflict); history remains recorded under the old login",
					"channelID", r.ChannelID, "error", err)
			}
		}
		slog.Info("Reconciled streamer rename by stable Twitch channel ID",
			"oldLogin", r.OldLogin, "newLogin", r.NewLogin, "channelID", r.ChannelID)
	}
}

// renameAnalyticsService is the slice of *analytics.Service the rename
// migration needs, narrowed to an interface so it is testable without a real
// database.
type renameAnalyticsService interface {
	RenameStreamer(oldName, newName string) error
}
