// Package streamerlifecycle owns the one high-level operation that fully removes
// a streamer's bot-owned PERSISTED state from SQLite: a single atomic
// transaction that purges every login-keyed store (analytics history,
// notification rules + config lists, watch-time rows), an in-memory
// resurrection fence that stops a late event from recreating any of it, and a
// DURABLE pending-deletion ledger so a purge that fails is retried on the next
// startup instead of being forgotten.
//
// Identity model. The stable Twitch channel ID is the PRIMARY deletion identity
// — the caller resolves it from the runtime streamer registry (which is
// ID-first) and passes it here for the durable record + logging. The current
// login is the resolved LOOKUP key for the persisted stores, which are all
// login-keyed (no channel_id column exists in any table). A config-driven rename
// repoints EVERY login-keyed store to the new login in ONE atomic transaction
// (RenameStreamer), so the runtime login always matches the persisted login and
// deleting by the current login is complete for a renamed streamer — there is no
// old-login orphan to chase, and no login is ever purged without going through
// this identity-scoped path (which protects a login later reused by a different
// channel).
//
// Boundary. This package deletes only bot-owned MUTABLE persisted state. It does
// NOT touch shared/global data (the drop-campaign catalog, campaign-keyed
// notification dedupe), already-sent Discord messages, or external/immutable
// logs. Runtime teardown (PubSub topics, IRC presence, watcher maps, streak
// cache) is the caller's responsibility and is coordinated around this call.
package streamerlifecycle

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// Purger deletes every row of one lowercase login within a caller-supplied
// transaction and reports whether anything existed for that login in this store.
// It must operate only on the passed *sql.Tx (never commit/rollback it) so the
// coordinator can span several stores in one atomic transaction.
type Purger interface {
	DeleteStreamerTx(tx *sql.Tx, login string) (bool, error)
}

// Fencer arms and clears a store's in-memory resurrection fence for a login.
// While a login is tombstoned, that store's write paths refuse to recreate the
// streamer's rows, so a late event that lost the race with deletion cannot
// resurrect it.
type Fencer interface {
	Tombstone(login string)
	Reinstate(login string)
}

// Renamer repoints one login's rows to a new login within the caller's
// transaction, so several stores can rename atomically. Same fail-closed /
// idempotent contract each store already documents.
type Renamer interface {
	RenameStreamerTx(tx *sql.Tx, oldLogin, newLogin string) error
}

// Outcome classifies a completed deletion.
type Outcome string

const (
	// OutcomeDeleted: at least one store held rows for the login and they were
	// removed.
	OutcomeDeleted Outcome = "deleted"
	// OutcomeAlreadyAbsent: no store held anything for the login (a repeated or
	// never-recorded deletion) — a deterministic idempotent success.
	OutcomeAlreadyAbsent Outcome = "already_absent"
)

// Result is the typed disposition of a Delete call.
type Result struct {
	Outcome   Outcome
	ChannelID string
	Login     string
}

// lifecycleModule owns the durable pending-deletion ledger. A row exists exactly
// while a streamer's persisted purge has been requested but not yet confirmed
// complete; it is deleted in the SAME transaction as the successful purge, so
// "row present" durably means "purge still owed" across process restarts.
type lifecycleModule struct{}

func (lifecycleModule) Name() string { return "streamer_lifecycle" }

func (lifecycleModule) Migrations() []database.Migration {
	return []database.Migration{
		{
			Version:     1,
			Description: "Create pending_streamer_deletions reconciliation ledger",
			// Additive, repeat-safe (CREATE TABLE IF NOT EXISTS) and transactional
			// via the migration runner. login is the PK (one pending purge per
			// login); channel_id records the stable identity for logging/audit.
			SQL: `
				CREATE TABLE IF NOT EXISTS pending_streamer_deletions (
					login        TEXT PRIMARY KEY,
					channel_id   TEXT NOT NULL,
					requested_at INTEGER NOT NULL,
					attempts     INTEGER NOT NULL DEFAULT 0
				);
			`,
		},
	}
}

// Coordinator runs the atomic persisted purge across a fixed set of stores and
// owns the durable pending-deletion ledger.
type Coordinator struct {
	db       *database.DB
	purgers  []Purger
	fencers  []Fencer
	renamers []Renamer
	now      func() time.Time
}

// New builds a coordinator over the shared handle and the stores to
// purge/fence/rename, registering the durable ledger's schema. purgers, fencers
// and renamers are typically the same objects (each store implements what it
// can); nil entries are ignored so a coordinator degrades gracefully when a
// subsystem (e.g. analytics) is disabled.
func New(db *database.DB, purgers []Purger, fencers []Fencer, renamers []Renamer) (*Coordinator, error) {
	if err := db.RegisterModule(lifecycleModule{}); err != nil {
		return nil, fmt.Errorf("streamerlifecycle: register ledger: %w", err)
	}
	return &Coordinator{
		db:       db,
		purgers:  compactPurgers(purgers),
		fencers:  compactFencers(fencers),
		renamers: compactRenamers(renamers),
		now:      time.Now,
	}, nil
}

func compactPurgers(in []Purger) []Purger {
	out := make([]Purger, 0, len(in))
	for _, p := range in {
		if p != nil {
			out = append(out, p)
		}
	}
	return out
}

func compactFencers(in []Fencer) []Fencer {
	out := make([]Fencer, 0, len(in))
	for _, f := range in {
		if f != nil {
			out = append(out, f)
		}
	}
	return out
}

func compactRenamers(in []Renamer) []Renamer {
	out := make([]Renamer, 0, len(in))
	for _, r := range in {
		if r != nil {
			out = append(out, r)
		}
	}
	return out
}

// Delete removes all of one streamer's persisted bot-owned state, durably.
//
// Ordering:
//  1. Tombstone the login in every store (barrier: once Tombstone returns every
//     in-flight write has committed and every later write is refused, so no
//     event can slip a row past the purge).
//  2. Record a DURABLE pending-deletion row (committed on its own) BEFORE the
//     purge, so the intent survives a crash or a rolled-back purge.
//  3. Purge every store AND delete the pending row in ONE transaction. On
//     success the pending row is gone; on any error the whole transaction rolls
//     back, leaving the pending row (and the fence) in place.
//
// Failure contract. On a purge error nothing is deleted (atomic rollback), the
// durable pending row REMAINS, and the fence stays armed. The caller has already
// removed the streamer from the runtime roster and config, so it is inert
// (not watched, earning nothing, sending nothing) and cannot be resurrected —
// "removed but its history purge is pending", never a false "fully deleted".
// The pending row is retried by Reconcile on the next startup (and by
// ReconcileLogin if the same login is re-added). Idempotent; makes no
// "traceless" claim while a purge is pending.
func (c *Coordinator) Delete(ctx context.Context, channelID, login string) (Result, error) {
	login = strings.ToLower(strings.TrimSpace(login))
	res := Result{ChannelID: channelID, Login: login}
	if login == "" {
		return res, fmt.Errorf("streamerlifecycle: empty login for channel %q", channelID)
	}

	// 1. Fence first (barrier).
	for _, f := range c.fencers {
		f.Tombstone(login)
	}

	// 2. Durable intent, committed before the purge.
	if err := c.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`
			INSERT INTO pending_streamer_deletions (login, channel_id, requested_at, attempts)
			VALUES (?, ?, ?, 0)
			ON CONFLICT(login) DO UPDATE SET channel_id = excluded.channel_id, requested_at = excluded.requested_at`,
			login, channelID, c.now().Unix())
		return e
	}); err != nil {
		return res, fmt.Errorf("streamerlifecycle: record pending deletion for %q (channel %q): %w", login, channelID, err)
	}

	// 3. Atomic purge + clear the durable marker.
	existed, err := c.purgeAndClearTx(ctx, login)
	if err != nil {
		return res, fmt.Errorf("streamerlifecycle: purge streamer %q (channel %q): %w", login, channelID, err)
	}
	if existed {
		res.Outcome = OutcomeDeleted
	} else {
		res.Outcome = OutcomeAlreadyAbsent
	}
	return res, nil
}

// purgeAndClearTx runs every store's purge AND deletes the durable pending row
// in ONE transaction, so the marker disappears if and only if the purge commits.
func (c *Coordinator) purgeAndClearTx(ctx context.Context, login string) (bool, error) {
	existed := false
	err := c.db.WithTx(ctx, func(tx *sql.Tx) error {
		for _, p := range c.purgers {
			e, perr := p.DeleteStreamerTx(tx, login)
			if perr != nil {
				return perr
			}
			existed = existed || e
		}
		if _, e := tx.Exec(`DELETE FROM pending_streamer_deletions WHERE login = ?`, login); e != nil {
			return e
		}
		return nil
	})
	return existed, err
}

type pendingRecord struct {
	login     string
	channelID string
}

// Reconcile retries every unfinished deletion recorded in the durable ledger and
// is called ONCE at startup (before any watch/pubsub/event loop begins, so
// reinstating a login cannot race a live event). For each pending row it
// tombstones the login, purges every store and deletes the marker in one
// transaction, then lifts the fence on success — so a configured login that was
// mid-deletion when the process died is cleaned and free to record fresh, while
// a non-configured deleted login is simply cleaned. On failure the marker and
// fence stay, its attempt counter is bumped, and it is retried on the next
// startup. Returns the number reconciled and the first error encountered.
func (c *Coordinator) Reconcile(ctx context.Context) (int, error) {
	pending, err := c.listPending(ctx)
	if err != nil {
		return 0, fmt.Errorf("streamerlifecycle: list pending deletions: %w", err)
	}
	reconciled := 0
	var firstErr error
	for _, rec := range pending {
		for _, f := range c.fencers {
			f.Tombstone(rec.login)
		}
		if _, err := c.purgeAndClearTx(ctx, rec.login); err != nil {
			_ = c.bumpAttempts(ctx, rec.login)
			if firstErr == nil {
				firstErr = fmt.Errorf("reconcile %q (channel %q): %w", rec.login, rec.channelID, err)
			}
			continue
		}
		for _, f := range c.fencers {
			f.Reinstate(rec.login)
		}
		reconciled++
	}
	return reconciled, firstErr
}

// ReconcileLogin retries a single login's unfinished deletion if one is pending,
// used on a re-add so a streamer added back before its earlier purge finished
// starts clean (its stale rows are purged before it can record). Returns
// (hadPending, err): (false, nil) when nothing was owed; (true, nil) when the
// stale rows were purged; (true, err) when the purge failed again (the marker
// and fence remain, so the re-added streamer stays inert rather than inheriting
// stale history). It does NOT lift the fence — the caller decides that once it
// knows the purge succeeded.
func (c *Coordinator) ReconcileLogin(ctx context.Context, login string) (bool, error) {
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" {
		return false, nil
	}
	has, err := c.HasPending(ctx, login)
	if err != nil {
		return false, err
	}
	if !has {
		return false, nil
	}
	for _, f := range c.fencers {
		f.Tombstone(login)
	}
	if _, err := c.purgeAndClearTx(ctx, login); err != nil {
		_ = c.bumpAttempts(ctx, login)
		return true, fmt.Errorf("streamerlifecycle: reconcile re-added %q: %w", login, err)
	}
	return true, nil
}

// HasPending reports whether a durable pending-deletion row exists for login.
func (c *Coordinator) HasPending(ctx context.Context, login string) (bool, error) {
	login = strings.ToLower(strings.TrimSpace(login))
	var one int
	err := c.db.QueryRowContext(ctx, `SELECT 1 FROM pending_streamer_deletions WHERE login = ?`, login).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (c *Coordinator) listPending(ctx context.Context) ([]pendingRecord, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT login, channel_id FROM pending_streamer_deletions ORDER BY requested_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []pendingRecord
	for rows.Next() {
		var rec pendingRecord
		if err := rows.Scan(&rec.login, &rec.channelID); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (c *Coordinator) bumpAttempts(ctx context.Context, login string) error {
	_, err := c.db.ExecContext(ctx, `UPDATE pending_streamer_deletions SET attempts = attempts + 1 WHERE login = ?`, login)
	return err
}

// Reinstate clears the fence for a login in every store, so re-adding the same
// login (or channel ID) after a deletion starts a clean lifecycle: the old rows
// are already gone and fresh writes are allowed again. Idempotent and safe for a
// login that was never deleted. The caller must only call this once it knows no
// purge is still pending for the login (see ReconcileLogin/HasPending).
func (c *Coordinator) Reinstate(login string) {
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" {
		return
	}
	for _, f := range c.fencers {
		f.Reinstate(login)
	}
}

// RenameStreamer repoints one streamer's rows across EVERY login-keyed store
// from oldLogin to newLogin in ONE atomic transaction: analytics (collision-
// checked, fail-closed), notifications and watch-time all move together or none
// do. It satisfies the miner rename coordinator's renamer contract
// (RenameStreamer(old,new) error), so a persisted rename either fully succeeds
// or leaves every store on the old login (the caller then aborts the settings
// apply fail-closed). Idempotent for an unknown login; returns the analytics
// *StreamerRenameConflictError unchanged when both logins already have
// independent history. No-op when the logins match.
func (c *Coordinator) RenameStreamer(oldLogin, newLogin string) error {
	oldLogin = strings.ToLower(strings.TrimSpace(oldLogin))
	newLogin = strings.ToLower(strings.TrimSpace(newLogin))
	if oldLogin == "" || newLogin == "" || oldLogin == newLogin {
		return nil
	}
	return c.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		for _, r := range c.renamers {
			if err := r.RenameStreamerTx(tx, oldLogin, newLogin); err != nil {
				return err
			}
		}
		return nil
	})
}
