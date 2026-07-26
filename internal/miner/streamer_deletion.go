package miner

import (
	"context"
	"log/slog"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamerlifecycle"
)

// buildStreamerLifecycle constructs the persisted-deletion coordinator over the
// login-keyed stores that currently exist (analytics, notifications, watch-time).
// Each store is BOTH a purger and a fencer. Called ONCE, from setupComponents,
// after those stores (and, since M4, the write-once notifications manager) are
// created; a nil subsystem is simply not covered (e.g. no notifications manager
// means there are no notification rows to purge). Before M4 this was also
// re-invoked at runtime, from finishApply, whenever Discord first flipped on —
// that rebuild is gone: the manager now exists from startup whenever a database
// exists, so the startup call above already covers it and no later rebuild is
// needed. Leaves m.streamerLifecycle nil when no persisted store exists, in
// which case removal still tears down runtime state — there is just nothing
// persisted to purge.
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
	// Read via the shared accessor (I2), never m.notifications directly — this
	// function still runs exactly once, at startup, AFTER initNotificationManager
	// has already published the write-once manager (see setupComponents'
	// ordering), so notifMgr is nil here only when the miner has no database OR
	// when NewManager itself failed at startup (initNotificationManager logs
	// and leaves m.notifications unpublished in that case) — the
	// m.notificationsRepo fallback branch immediately below exists precisely
	// to cover that second case, so a removal's notification-row purge still
	// runs even without a live Manager.
	if notifMgr := m.notificationManager(); notifMgr != nil {
		purgers = append(purgers, notifMgr)
		fencers = append(fencers, notifMgr)
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

// reconcilePendingStreamerDeletions runs ONCE at startup, BEFORE any
// watch/pubsub/event loop starts, so reinstating a cleaned/aborted login
// cannot race a live event:
//
//  1. ArbitratePrepared resolves every row left in the SRAP prepare-phase
//     ledger by an unclean shutdown (M1) — a removal whose caller never
//     confirmed whether its own commit point (config.json) was reached.
//     The keep predicate is built from m.config.Streamers AS LOADED FROM
//     DISK (loadStreamers already ran before this — see Run), matched by
//     canonical login with the config entry's own stored ChannelID for the
//     channel-aware identity check (package streamerlifecycle's doc
//     comment).
//  2. Reconcile retries every row left in the pending-purge ledger (its
//     original, unchanged meaning) — including any row ArbitratePrepared
//     just promoted into it on THIS pass.
//
// Uses context.Background()+startupReconcileBudget rather than m.runCtx: a
// SIGTERM arriving mid-pass must not abort it halfway (an idempotent pass
// that completes as a unit is easier to reason about than one that stops
// with some rows resolved and others not — see the M1 design manifest).
// Never fatal: anything unresolved stays durably queued for the next
// startup.
func (m *Miner) reconcilePendingStreamerDeletions() {
	if m.streamerLifecycle == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), startupReconcileBudget)
	defer cancel()

	aborted, promoted, err := m.streamerLifecycle.ArbitratePrepared(ctx, m.arbitrationKeepFunc())
	switch {
	case err != nil:
		slog.Error("Some prepared streamer removals could not be arbitrated at startup; they remain durably queued for the next startup",
			"aborted", aborted, "promoted", promoted, "error", err)
	case aborted > 0 || promoted > 0:
		slog.Info("Arbitrated streamer removals recovered after an unclean shutdown", "aborted", aborted, "promoted", promoted)
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

// arbitrationKeepFunc builds ArbitratePrepared's keep predicate from the
// on-disk config loaded for this run (m.config.Streamers, as loadStreamers
// left it — read here with no lock since arbitration runs before any other
// goroutine can touch m.config): a canonical (ToLower(TrimSpace)) login ->
// stored ChannelID map, matching the exact identity anchor
// applySettingsWithRemovals/applySettingsWithRename admit against.
func (m *Miner) arbitrationKeepFunc() func(login, channelID string) (configured bool, cfgChannelID string) {
	byLogin := make(map[string]string, len(m.config.Streamers))
	for _, sc := range m.config.Streamers {
		login := strings.ToLower(strings.TrimSpace(sc.Username))
		if login == "" {
			continue
		}
		byLogin[login] = sc.ChannelID
	}
	return func(login, _ string) (bool, string) {
		cfgChannelID, configured := byLogin[login]
		return configured, cfgChannelID
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
// miner lock, on the SAME ctx/coord the caller resolved for this one apply
// (never re-reads m.runCtx/m.streamerLifecycle — see finishApply's doc
// comment). Safe (and a near no-op) when nothing was added or removed, or
// coord is nil (no persisted store exists).
//
// Failed purges are NOT tracked in memory — they persist as durable
// admission/pending-deletion rows and are retried by
// reconcilePendingStreamerDeletions on the next startup (and here on a
// re-add), so a failure survives a restart.
func (m *Miner) applyStreamerDeletions(ctx context.Context, coord *streamerlifecycle.Coordinator, added, removed []*models.Streamer) {
	// Purge every streamer removed by THIS apply, and clear its streak grant.
	for _, s := range removed {
		// Streak state (persisted cache file + in-memory hydration snapshot) is
		// not a DB table; clear it alongside the transactional purge so the grant
		// cannot outlive the streamer or be re-inherited on a same-login re-add.
		m.streamers.ForgetStreak(s.GetUsername())
		m.purgeRemovedStreamer(ctx, coord, s)
	}

	// Re-adds: if a durable purge is still owed for the login (an earlier
	// deletion whose purge failed), finish it FIRST so the re-added streamer
	// starts clean; then lift the fence so fresh events record. If that purge
	// fails again the streamer stays fenced (inert) rather than inheriting stale
	// history — the durable marker keeps it queued for the next startup.
	if coord != nil {
		for _, s := range added {
			login := s.GetUsername()
			if _, err := coord.ReconcileLogin(ctx, login); err != nil {
				slog.Error("Re-added streamer's stale-history purge failed; keeping it fenced until the deletion reconciles",
					"streamer", login, "channelID", s.ChannelID, "error", err)
				continue
			}
			coord.Reinstate(login)
		}
	}
}

// purgeRemovedStreamer runs SRAP's COMPLETE phase (CommitRemoval) for one
// streamer whose removal has ALREADY been durably admitted and committed
// (config.json persisted) by the caller — this is only ever invoked after
// that commit point, in every apply path (legacy no-removal applies never
// reach here with a real removed streamer; SRAP and rename-with-removal
// applies both call AdmitRemovals before their own commit). That is what
// makes the failure log below truthful in every branch: a durable row
// (admissions, if only the move-to-pending step failed, or pending, if only
// the purge itself failed) provably exists the instant CommitRemoval returns
// an error — see that function's doc comment. A completion failure is
// logged, never returned: "committed, purge still owed" is success for the
// user's intent (the streamer IS removed), not a failed apply.
func (m *Miner) purgeRemovedStreamer(ctx context.Context, coord *streamerlifecycle.Coordinator, s *models.Streamer) {
	if coord == nil {
		return
	}
	login := s.GetUsername()
	res, err := coord.CommitRemoval(ctx, s.ChannelID, login)
	if err != nil {
		slog.Error("Streamer removed and removal committed, but persisted-history purge failed; durably queued to retry on the next startup",
			"streamer", login, "channelID", s.ChannelID, "error", err)
		return
	}
	slog.Info("Purged deleted streamer's persisted state",
		"streamer", login, "channelID", s.ChannelID, "outcome", string(res.Outcome))
}
