package miner

import (
	"context"
	"log/slog"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamerlifecycle"
)

// syncRenamedLoginKeyedStores best-effort repoints notification and watch-time
// rows for each config-driven rename onto the streamer's NEW login, so those
// login-keyed stores stay attached to the current identity (analytics + config
// were already renamed in the fail-closed commitRenameTransaction). This keeps a
// later deletion — which purges by the CURRENT login — complete for a renamed
// streamer, so a rename never leaves old-login notification/watch-time records
// behind (BKM-018A D15/D16). Best-effort: a failure only leaves rows under the
// old login (the pre-existing behavior) and never blocks the apply.
func (m *Miner) syncRenamedLoginKeyedStores(renamed []streamer.RenameEvent) {
	for _, r := range renamed {
		if m.notificationsRepo != nil {
			if err := m.notificationsRepo.RenameStreamer(r.OldLogin, r.NewLogin); err != nil {
				slog.Warn("Failed to move notification state to the streamer's new login; old-login rows remain until re-saved",
					"oldLogin", r.OldLogin, "newLogin", r.NewLogin, "error", err)
			}
		}
		if m.watchTimeStore != nil {
			if err := m.watchTimeStore.RenameStreamer(r.OldLogin, r.NewLogin); err != nil {
				slog.Warn("Failed to move watch-time rows to the streamer's new login",
					"oldLogin", r.OldLogin, "newLogin", r.NewLogin, "error", err)
			}
		}
	}
}

// buildStreamerLifecycle constructs the persisted-deletion coordinator over the
// login-keyed stores that currently exist (analytics, notifications, watch-time).
// Each store is BOTH a purger and a fencer. Called once during initialize after
// those stores are created; a nil subsystem is simply not covered (e.g. no
// notifications manager means there are no notification rows to purge). Leaves
// m.streamerLifecycle nil when no persisted store exists, in which case removal
// still tears down runtime state — there is just nothing persisted to purge.
func (m *Miner) buildStreamerLifecycle() {
	if m.db == nil {
		return
	}
	var purgers []streamerlifecycle.Purger
	var fencers []streamerlifecycle.Fencer
	if m.analyticsSvc != nil {
		repo := m.analyticsSvc.Repository()
		purgers = append(purgers, repo)
		fencers = append(fencers, repo)
	}
	// Prefer the live Manager (so the fence also covers its AddPointRule); fall
	// back to the standalone repository so notification rows are still purged when
	// Discord is disabled. Both operate on the same tables.
	if m.notifications != nil {
		purgers = append(purgers, m.notifications)
		fencers = append(fencers, m.notifications)
	} else if m.notificationsRepo != nil {
		purgers = append(purgers, m.notificationsRepo)
		fencers = append(fencers, m.notificationsRepo)
	}
	if m.watchTimeStore != nil {
		purgers = append(purgers, m.watchTimeStore)
		fencers = append(fencers, m.watchTimeStore)
	}
	if len(purgers) == 0 {
		return
	}
	m.streamerLifecycle = streamerlifecycle.New(m.db, purgers, fencers)
}

// applyStreamerDeletions is the deletion half of a committed settings apply: for
// every streamer this apply REMOVED from the roster it purges all bot-owned
// persisted state (one atomic transaction) and clears the runtime streak grant,
// and for every streamer it ADDED it clears any resurrection fence so a re-added
// login/channel starts a clean lifecycle. Runtime capability teardown (PubSub
// topics, IRC presence) has already run in reconcileRuntimeCapabilities; the
// watcher's own login-keyed maps are pruned by applyStreamerList. Called at the
// end of finishApply, off the miner lock. Safe (and a near no-op) when nothing
// was added or removed.
func (m *Miner) applyStreamerDeletions(added, removed []*models.Streamer) {
	ctx := m.runCtx
	if ctx == nil {
		ctx = context.Background()
	}

	// 1. Drain any previously-failed purges (rare) so a later apply reconciles
	// them. Retrying BEFORE the re-add reinstate below means a channel that is
	// re-added after an earlier failed purge still gets a clean start — its stale
	// rows are purged here, before any fresh event can write, honoring the
	// clean-re-add contract even across a purge failure.
	m.purgeMu.Lock()
	retry := m.pendingPurges
	m.pendingPurges = nil
	m.purgeMu.Unlock()
	for _, s := range retry {
		m.purgeRemovedStreamer(ctx, s)
	}

	// 2. Purge every streamer removed by THIS apply, and clear its streak grant.
	for _, s := range removed {
		// Streak state (persisted cache file + in-memory hydration snapshot) is
		// not a DB table; clear it alongside the transactional purge so the grant
		// cannot outlive the streamer or be re-inherited on a same-login re-add.
		m.streamers.ForgetStreak(s.GetUsername())
		m.purgeRemovedStreamer(ctx, s)
	}

	// 3. Re-adds: lift the fence so fresh events record again, and drop any
	// still-pending purge entry for the channel. A re-added streamer is active,
	// so it must NEVER be purged out from under its fresh history by a later
	// backlog drain; the only residue this can leave is rows a twice-failed purge
	// could not remove, which are the streamer's OWN history (present again), not
	// orphans.
	for _, s := range added {
		if m.streamerLifecycle != nil {
			m.streamerLifecycle.Reinstate(s.GetUsername())
		}
		m.purgeMu.Lock()
		delete(m.pendingPurges, s.ChannelID)
		m.purgeMu.Unlock()
	}
}

// purgeRemovedStreamer runs the atomic persisted purge for one removed streamer,
// keyed by its stable ChannelID (primary identity) and current login (the
// login-keyed stores' lookup key). On failure the streamer is already out of the
// runtime roster and config — inert and fenced, never half-active — so the error
// is logged and the streamer is queued for retry on the next settings apply
// rather than reported as a success.
func (m *Miner) purgeRemovedStreamer(ctx context.Context, s *models.Streamer) {
	if m.streamerLifecycle == nil {
		return
	}
	login := s.GetUsername()
	res, err := m.streamerLifecycle.Delete(ctx, s.ChannelID, login)
	if err != nil {
		slog.Error("Streamer removed from runtime but persisted-history purge failed; queued to retry on the next settings apply",
			"streamer", login, "channelID", s.ChannelID, "error", err)
		m.purgeMu.Lock()
		if m.pendingPurges == nil {
			m.pendingPurges = make(map[string]*models.Streamer)
		}
		m.pendingPurges[s.ChannelID] = s
		m.purgeMu.Unlock()
		return
	}
	slog.Info("Purged deleted streamer's persisted state",
		"streamer", login, "channelID", s.ChannelID, "outcome", string(res.Outcome))
}
