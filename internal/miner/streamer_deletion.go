package miner

import (
	"context"
	"log/slog"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamerlifecycle"
)

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
	var renamers []streamerlifecycle.Renamer
	if m.analyticsSvc != nil {
		repo := m.analyticsSvc.Repository()
		purgers = append(purgers, repo)
		fencers = append(fencers, repo)
		renamers = append(renamers, repo)
	}
	// Prefer the live Manager for the fence (so it covers its AddPointRule); the
	// standalone repository always backs the purge and rename so notification rows
	// are handled even when Discord is disabled. Both operate on the same tables.
	if m.notifications != nil {
		purgers = append(purgers, m.notifications)
		fencers = append(fencers, m.notifications)
	} else if m.notificationsRepo != nil {
		purgers = append(purgers, m.notificationsRepo)
		fencers = append(fencers, m.notificationsRepo)
	}
	if m.notificationsRepo != nil {
		renamers = append(renamers, m.notificationsRepo)
	}
	if m.watchTimeStore != nil {
		purgers = append(purgers, m.watchTimeStore)
		fencers = append(fencers, m.watchTimeStore)
		renamers = append(renamers, m.watchTimeStore)
	}
	if len(purgers) == 0 {
		m.streamerLifecycle = nil
		return
	}
	coord, err := streamerlifecycle.New(m.db, purgers, fencers, renamers)
	if err != nil {
		slog.Error("Failed to build streamer-deletion coordinator; removals will not purge persisted history until this is fixed", "error", err)
		m.streamerLifecycle = nil
		return
	}
	m.streamerLifecycle = coord
}

// reconcilePendingStreamerDeletions retries, ONCE at startup, any streamer
// deletion whose persisted purge did not finish before the process last exited
// (a durable pending-deletion row survived). Called during initialize, BEFORE
// any watch/pubsub/event loop starts, so reinstating a cleaned login cannot race
// a live event. Never fatal: a still-failing purge stays durably queued for the
// next startup.
func (m *Miner) reconcilePendingStreamerDeletions() {
	if m.streamerLifecycle == nil {
		return
	}
	ctx := m.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	n, err := m.streamerLifecycle.Reconcile(ctx)
	if err != nil {
		slog.Error("Some pending streamer deletions could not be reconciled at startup; they remain durably queued for the next startup",
			"reconciled", n, "error", err)
		return
	}
	if n > 0 {
		slog.Info("Reconciled unfinished streamer deletions at startup", "count", n)
	}
}

// applyStreamerDeletions is the deletion half of a committed settings apply: for
// every streamer this apply REMOVED from the roster it purges all bot-owned
// persisted state (one atomic transaction) and clears the runtime streak grant,
// and for every streamer it ADDED it purges any still-pending stale history and
// then clears the resurrection fence so a re-added login/channel starts a clean
// lifecycle. Runtime capability teardown (PubSub topics, IRC presence) has
// already run in reconcileRuntimeCapabilities; the watcher's own login-keyed maps
// are pruned by applyStreamerList. Called at the end of finishApply, off the
// miner lock. Safe (and a near no-op) when nothing was added or removed.
//
// Failed purges are NOT tracked in memory — they persist as durable
// pending-deletion rows and are retried by reconcilePendingStreamerDeletions on
// the next startup (and here on a re-add), so a failure survives a restart.
func (m *Miner) applyStreamerDeletions(added, removed []*models.Streamer) {
	ctx := m.runCtx
	if ctx == nil {
		ctx = context.Background()
	}

	// Purge every streamer removed by THIS apply, and clear its streak grant.
	for _, s := range removed {
		// Streak state (persisted cache file + in-memory hydration snapshot) is
		// not a DB table; clear it alongside the transactional purge so the grant
		// cannot outlive the streamer or be re-inherited on a same-login re-add.
		m.streamers.ForgetStreak(s.GetUsername())
		m.purgeRemovedStreamer(ctx, s)
	}

	// Re-adds: if a durable purge is still owed for the login (an earlier
	// deletion whose purge failed), finish it FIRST so the re-added streamer
	// starts clean; then lift the fence so fresh events record. If that purge
	// fails again the streamer stays fenced (inert) rather than inheriting stale
	// history — the durable marker keeps it queued for the next startup.
	if m.streamerLifecycle != nil {
		for _, s := range added {
			login := s.GetUsername()
			if _, err := m.streamerLifecycle.ReconcileLogin(ctx, login); err != nil {
				slog.Error("Re-added streamer's stale-history purge failed; keeping it fenced until the deletion reconciles",
					"streamer", login, "channelID", s.ChannelID, "error", err)
				continue
			}
			m.streamerLifecycle.Reinstate(login)
		}
	}
}

// purgeRemovedStreamer runs the atomic, durably-tracked persisted purge for one
// removed streamer, keyed by its stable ChannelID (primary identity) and current
// login (the login-keyed stores' lookup key). On failure the streamer is already
// out of the runtime roster and config — inert and fenced, never half-active —
// and a DURABLE pending-deletion row remains, so the purge is retried on the
// next startup (survives a restart) rather than reported as a success.
func (m *Miner) purgeRemovedStreamer(ctx context.Context, s *models.Streamer) {
	if m.streamerLifecycle == nil {
		return
	}
	login := s.GetUsername()
	res, err := m.streamerLifecycle.Delete(ctx, s.ChannelID, login)
	if err != nil {
		slog.Error("Streamer removed from runtime but persisted-history purge failed; durably queued to retry on the next startup",
			"streamer", login, "channelID", s.ChannelID, "error", err)
		return
	}
	slog.Info("Purged deleted streamer's persisted state",
		"streamer", login, "channelID", s.ChannelID, "outcome", string(res.Outcome))
}
