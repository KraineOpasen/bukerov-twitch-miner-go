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
// different settings blocks. This early pass also calls migrateAutoRedeem on
// the candidate, but that call's result is DISCARDED here: the commit-point
// refreshCandidateAutoRedeemLocked (rewards.go) later replaces
// candidate.AutoRedeem wholesale from the LIVE map (I1/I2) before publish, so
// only that later call is authoritative — this one is harmless but moot.
//
// Pure and side-effect-free besides mutating cfg; performs no I/O and must
// run BEFORE SaveConfig. [R5] Despite what an earlier version of this comment
// claimed, it does NOT need the miner lock held: it operates on a private
// candidate config that only THIS goroutine can see at this point (m.mu was
// already released before applySettingsWithRename calls this — see
// miner.go). The commit-point refresh that DOES need m.mu is
// refreshCandidateAutoRedeemLocked, not this function.
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

// migrateAutoRedeem moves cfg.AutoRedeem[oldLogin] to [newLogin] when
// present, normalizing both logins to lowercase internally [R4] — matching
// its runtime twin migrateAutoRedeemRuntimeState and every other auto-redeem
// lookup in this package — so a caller may pass either login in its original
// case. If the new login already has its own independently-configured entry,
// that destination entry wins (mirroring applyConfigRenames' own coalesce
// choice above: the surviving new-login entry's settings are never silently
// merged with the old one's) and oldLogin's now-orphaned entry is deleted
// rather than left behind — oldLogin no longer identifies any tracked
// streamer after the rename, so retaining its key would just be dead config.
// A privacy-safe warning is logged either way (only logins + ChannelID-free
// context, matching I13's log budget in spirit even though this specific
// warning is diagnostic, not the rename notice itself). [R5] When this runs
// from the commit-point refresh (rewards.go), it runs under m.mu — slog.Warn
// already runs under m.mu on other paths in this package (finishApply's
// non-persisted SaveConfig branch, miner.go; policy.go via persistLocked;
// health.go), so this is not a new pattern.
//
// Returns true when the clash (destination-wins) branch fired, so a caller
// tracking commit-time AutoRedeem generations (migrateAutoRedeemGenLocked)
// knows which new-login keys had their snapshot invalidated rather than
// continued.
func migrateAutoRedeem(cfg *config.Config, oldLogin, newLogin string) bool {
	oldKey := strings.ToLower(oldLogin)
	newKey := strings.ToLower(newLogin)
	if cfg.AutoRedeem == nil {
		return false
	}
	oldCfg, ok := cfg.AutoRedeem[oldKey]
	if !ok {
		return false
	}
	if _, clash := cfg.AutoRedeem[newKey]; clash {
		slog.Warn("Auto-redeem config for renamed streamer was not migrated: the new login already has its own auto-redeem entry; discarding the old entry",
			"oldLogin", oldKey, "newLogin", newKey)
		delete(cfg.AutoRedeem, oldKey)
		return true
	}
	cfg.AutoRedeem[newKey] = oldCfg
	delete(cfg.AutoRedeem, oldKey)
	return false
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

// commitAnalyticsRenames performs the analytics half of a rename-carrying
// settings apply's durable prepare (BKM-006 Corrective Pass 1, C2; split from
// the former commitRenameTransaction by M2/I7 — the config.json write and the
// runtime publish now happen together, under m.mu, at the caller's own commit
// point in applySettingsWithRename, not here). Runs OFF m.mu — SQLite I/O
// must never run under m.mu. Each svc.RenameStreamer call is itself a single
// atomic, collision-checking SQL transaction
// (internal/analytics/repository.go): a collision or a write failure leaves
// that streamer's analytics row completely untouched, so it doubles as both
// the preflight check and the commit for that one rename. If a LATER rename
// in a multi-rename batch fails after earlier ones already committed, every
// already-committed rename in THIS call is reversed immediately (RenameStreamer
// called again with old/new swapped — itself idempotent and collision-safe)
// and the error is returned; nil error means every rename in the batch is
// durably committed.
//
// The returned rollback func additionally lets the CALLER reverse this
// batch's successful commits later, if ITS OWN subsequent step fails (the
// C2-B case: config.SaveConfig fails after analytics already committed) — a
// no-op when svc is nil or nothing was committed. Callers must invoke it
// themselves; commitAnalyticsRenames never calls it after returning.
func commitAnalyticsRenames(renames []streamer.RenameEvent, svc renameAnalyticsService) (rollback func(), err error) {
	var committed []streamer.RenameEvent
	rollback = func() {
		for i := len(committed) - 1; i >= 0; i-- {
			r := committed[i]
			if err := svc.RenameStreamer(r.NewLogin, r.OldLogin); err != nil {
				slog.Error("Failed to reverse a partially committed analytics rename during a failed settings apply; analytics history may remain split across logins until the next successful rename apply",
					"oldLogin", r.OldLogin, "newLogin", r.NewLogin, "error", err)
			}
		}
	}

	if svc == nil {
		return rollback, nil
	}

	for _, r := range renames {
		if err := svc.RenameStreamer(r.OldLogin, r.NewLogin); err != nil {
			rollback()
			return rollback, fmt.Errorf("analytics history migration for %q -> %q: %w", r.OldLogin, r.NewLogin, err)
		}
		committed = append(committed, r)
	}
	return rollback, nil
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
// has no overflow to guard). [RR5] This merge is deliberately conservative,
// pre-existing C4-B behavior (it can only over-count spend, never under-spend)
// and is kept as-is by M2 — the clash-branch generation bump
// (migrateAutoRedeemGenLocked) invalidates SNAPSHOTS taken before the clash,
// not this merge. Login keys are normalized to lowercase, matching
// GetUsername and every other auto-redeem lookup in this package.
//
// Returns the ALWAYS NON-NIL [RR1] set of lowercase new-login keys whose
// migration hit this MERGE branch (destination state already existed) — the
// rename commit point (applySettingsWithRename) unions it into
// refreshCandidateAutoRedeemLocked's own clash set before feeding both to
// migrateAutoRedeemGenLocked, so a state-only clash (no config.AutoRedeem
// entry at all — see TestApplySettings_Rename_AutoRedeemRuntimeState_
// DestinationCollision_C4B) still invalidates the destination's generation.
func migrateAutoRedeemRuntimeState(state map[string]*autoRedeemRuntime, renamed []streamer.RenameEvent) map[string]bool {
	clashes := make(map[string]bool)
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

		clashes[newKey] = true
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
	return clashes
}

// migrateAutoRedeemGenLocked runs EXACTLY ONCE per committed rename batch, at
// the rename commit point (applySettingsWithRename) — NEVER from finishApply's
// idempotent state-migration re-run: a second pass here would bump PAST the
// migrated value and sever the continuity this function exists to preserve
// (see finishApply's doc comment, [R1]). Caller holds m.mu. [Round-2 note]
// Rename chains/swaps (A->B, B->C) never reach one commit batch: the
// planner's login-collision rule (streamer/manager.go:517-531) refuses the
// second step with ConflictLoginCollision, so this never needs intra-batch
// ordering across renames.
//
// For each rename, one of two things happens to gen[newKey]:
//   - clashedNewKeys[newKey] (config OR state already had an independent
//     entry there — the union of refreshCandidateAutoRedeemLocked's and
//     migrateAutoRedeemRuntimeState's clash sets): the destination's config
//     or state was just replaced/merged, so an old snapshot no longer
//     describes it — bump(newKey), invalidating it outright.
//   - otherwise (ordinary rename, the window CONTINUES under the new login):
//     gen[newKey] = max(gen[newKey], gen[oldKey] as of just before this
//     call). A plain max, never lowered — reviving a zombie snapshot by
//     LOWERING gen[newKey] would be worse than the one-redemption residue
//     documented below.
//
// Either way, oldKey is ALWAYS sealed (bumped) at the end: any post-commit
// record/clear that still resolves the OLD login (the runtime roster only
// renames at CommitPlan, which runs AFTER this) is refused rather than
// creating an orphaned runtime entry a later finishApply merge would
// max()-away (silent budget loss).
//
// Why this is correct — the three record-landing windows for an in-flight
// evaluation, budget 1000 / spent 900 / a 100-cost redemption, snapshot gen G
// taken under oldKey:
//   - (A) record lands BEFORE this commit -> oldKey state 900->1000, then
//     migrated whole by migrateAutoRedeemRuntimeState. Correct.
//   - (B) record lands AFTER this commit but BEFORE CommitPlan -> the
//     record's helper call resolves the OLD login (the roster only renames at
//     CommitPlan); gen[old] = G+1 != G -> refused, no orphan created. This one
//     already-in-flight redemption goes unrecorded — bounded, one reward, the
//     same inherent-TOCTOU class as any invalidation racing an in-flight
//     network call.
//   - (C) record lands AFTER CommitPlan -> the helper call resolves the NEW
//     login; gen[new] = G (migrated) -> accepted into the migrated state ->
//     spent = 1000. Correct. [RR4] This continuity is EXACT precisely when
//     the destination key carried no HIGHER historical generation than
//     oldKey's (the non-clash branch's "otherwise" case above); when the
//     destination's own history is higher (e.g. remove-beta-then-rename-
//     into-beta), window C degrades to the same one-in-flight-redemption
//     residue as window B — the monotonic-max guard is deliberately
//     preferred over exactness, because lowering gen[newKey] would revive a
//     zombie snapshot.
func (m *Miner) migrateAutoRedeemGenLocked(renames []streamer.RenameEvent, clashedNewKeys map[string]bool) {
	for _, r := range renames {
		oldKey, newKey := strings.ToLower(r.OldLogin), strings.ToLower(r.NewLogin)
		if oldKey == newKey {
			continue
		}
		prevOld := m.autoRedeemGen[oldKey] // read BEFORE the seal below
		if clashedNewKeys[newKey] {
			m.bumpAutoRedeemGenLocked(newKey)
		} else if prevOld > m.autoRedeemGen[newKey] {
			if m.autoRedeemGen == nil {
				m.autoRedeemGen = make(map[string]uint64)
			}
			m.autoRedeemGen[newKey] = prevOld
		}
		// Seal the old key regardless of which branch above fired.
		m.bumpAutoRedeemGenLocked(oldKey)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
