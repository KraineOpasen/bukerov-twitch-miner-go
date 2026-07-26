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
//
// SRAP (Streamer Removal Admission Protocol, M1). A caller that must not
// mutate anything visible (runtime roster, config.json) until a deletion is
// DURABLY admitted uses the two-phase primitives below instead of Delete:
//
//  1. PREPARE — AdmitRemovals durably records intent (streamer_deletion_
//     admissions), all-or-nothing for a whole batch, BEFORE any runtime/config
//     mutation. AbortAdmission compensates a prepare that its caller's own
//     commit point never reached.
//  2. COMMIT — owned entirely by the caller (e.g. config.SaveConfig's atomic
//     rename); this package is not involved. Everything from here on is
//     irreversible by design (see ArbitratePrepared).
//  3. COMPLETE — CommitRemoval, called once the caller's commit has
//     succeeded: arms the fence, moves the durable record from the admissions
//     table to the pending-purge ledger (pending_streamer_deletions — its
//     ORIGINAL, unchanged meaning: "purge still owed"), then purges every
//     store and clears the ledger row atomically.
//
// A row surviving in EITHER table across a restart is resolved once, at
// startup, by ArbitratePrepared (admissions table) followed by Reconcile
// (pending table) — never re-decided per apply. See each function's doc
// comment for the exact contract.
package streamerlifecycle

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
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

// Result is the typed disposition of a Delete/CommitRemoval call.
type Result struct {
	Outcome   Outcome
	ChannelID string
	Login     string
}

// Removal identifies one streamer to admit for removal (SRAP prepare phase).
// ChannelID is the stable identity recorded for audit/logging; Login is
// canonicalized (ToLower(TrimSpace)) by AdmitRemovals before use.
type Removal struct {
	ChannelID string
	Login     string
}

// lifecycleModule owns the durable deletion ledgers:
//   - pending_streamer_deletions (v1): a row exists exactly while a streamer's
//     persisted purge has been requested but not yet confirmed complete; it is
//     deleted in the SAME transaction as the successful purge, so "row
//     present" durably means "purge still owed" across process restarts. This
//     table's meaning is UNCHANGED by SRAP.
//   - streamer_deletion_admissions (v2): a row exists exactly while a removal
//     has been durably PREPARED but its caller's own commit point has not yet
//     been confirmed reached — a row here makes no claim about whether the
//     removal will actually happen; see ArbitratePrepared for how a row that
//     outlives its process is resolved.
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
		{
			Version:     2,
			Description: "Create streamer_deletion_admissions SRAP prepare-phase ledger (M1)",
			// Additive, repeat-safe, and independent of pending_streamer_deletions
			// (a separate table, not a column on it — see the M1 design manifest
			// §3 for why a `phase` column on the existing table was rejected: an
			// old binary reading a downgraded DB would purge a merely-PREPARED
			// row, which is destructive). A v0.26 binary never SELECTs this
			// table (no `SELECT *` anywhere in this package), so a leftover
			// prepared row after a downgrade is inert (leaked, never destructive).
			SQL: `
				CREATE TABLE IF NOT EXISTS streamer_deletion_admissions (
					login        TEXT PRIMARY KEY,
					channel_id   TEXT NOT NULL,
					requested_at INTEGER NOT NULL
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

// canonicalLogin normalizes a login the same way everywhere in this package:
// trimmed and lowercased, so the ledger PKs, fence keys, and store lookups
// never disagree over case/whitespace.
func canonicalLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

// AdmitRemovals is SRAP's PREPARE phase: it durably records the INTENT to
// remove every listed streamer in ONE all-or-nothing transaction — either
// every row is written, or (on any error, including a cancelled/expired ctx
// or a closed database) none is; the caller has performed NO runtime or
// config mutation yet, so a failure here costs nothing to abandon.
//
// Every login is canonicalized and validated BEFORE the transaction opens: an
// empty login is rejected outright (never silently admitted), and the batch is
// deduped by login (a batch naming the same login twice collapses to one row
// — the ledger PK is login, so writing it twice would otherwise silently
// coalesce inside the transaction anyway; deduping here just makes that
// explicit rather than order-dependent). A row admitted here makes NO claim
// about whether the removal will actually happen — only that it was
// requested; see ArbitratePrepared for how a row that outlives its process
// (the caller crashed before reaching its own commit point, or after) is
// resolved. Calling with an empty batch is a no-op.
func (c *Coordinator) AdmitRemovals(ctx context.Context, removals []Removal) error {
	type row struct{ login, channelID string }
	seen := make(map[string]bool, len(removals))
	rows := make([]row, 0, len(removals))
	for _, r := range removals {
		login := canonicalLogin(r.Login)
		if login == "" {
			return fmt.Errorf("streamerlifecycle: empty login for channel %q in removal batch", r.ChannelID)
		}
		if seen[login] {
			continue
		}
		seen[login] = true
		rows = append(rows, row{login: login, channelID: r.ChannelID})
	}
	if len(rows) == 0 {
		return nil
	}

	now := c.now().Unix()
	return c.db.WithTx(ctx, func(tx *sql.Tx) error {
		for _, r := range rows {
			if _, err := tx.Exec(`
				INSERT INTO streamer_deletion_admissions (login, channel_id, requested_at)
				VALUES (?, ?, ?)
				ON CONFLICT(login) DO UPDATE SET channel_id = excluded.channel_id, requested_at = excluded.requested_at`,
				r.login, r.channelID, now); err != nil {
				return fmt.Errorf("admit %q (channel %q): %w", r.login, r.channelID, err)
			}
		}
		return nil
	})
}

// AbortAdmission compensates a successful AdmitRemovals whose caller never
// reached its own commit point (e.g. the config.json write that follows
// failed, or the request was cancelled first): it deletes the prepared rows
// for the given logins in ONE transaction. Unknown/never-admitted logins are
// silently no-ops. If THIS itself fails (e.g. the database closed between
// AdmitRemovals and the failed commit), the rows are simply left in place —
// ArbitratePrepared resolves them deterministically at the next startup
// (a still-configured login is aborted there too), so a failed compensation
// never leaves anything ambiguous, only durably delayed. Calling with an
// empty (or all-empty-canonicalizing) list is a no-op.
func (c *Coordinator) AbortAdmission(ctx context.Context, logins []string) error {
	canon := make([]string, 0, len(logins))
	for _, l := range logins {
		if l = canonicalLogin(l); l != "" {
			canon = append(canon, l)
		}
	}
	if len(canon) == 0 {
		return nil
	}
	return c.db.WithTx(ctx, func(tx *sql.Tx) error {
		for _, l := range canon {
			if _, err := tx.Exec(`DELETE FROM streamer_deletion_admissions WHERE login = ?`, l); err != nil {
				return fmt.Errorf("abort admission for %q: %w", l, err)
			}
		}
		return nil
	})
}

// CommitRemoval is SRAP's COMPLETE phase, called once the caller's own commit
// point (e.g. config.json's atomic rename) has already succeeded — from here
// there is no more aborting, only completing or durably retrying:
//
//  1. Tombstone the login in every store (barrier: once Tombstone returns
//     every in-flight write has committed and every later write is refused,
//     so no event can slip a row past the purge).
//  2. Move the durable record from the admissions table to the pending-purge
//     ledger — insert into pending_streamer_deletions, delete from
//     streamer_deletion_admissions — in ONE transaction, so the record is
//     NEVER durably absent from both tables at once past this point.
//  3. Purge every store AND delete the pending row in ONE transaction (as
//     Delete always did). On success the pending row is gone; on any error
//     the whole transaction rolls back, leaving the pending row in place.
//
// Failure contract. If step 2 fails, the ORIGINAL admissions row (written by
// AdmitRemovals, whose transaction already committed) remains — the caller's
// commit already happened, so at the next startup ArbitratePrepared will find
// the login absent from config (or bound to a different channel) and PROMOTE
// it into the pending table, where Reconcile purges it. If step 3 fails, the
// pending row (written by step 2, already committed) remains and Reconcile
// retries it exactly as it always has. Either way a durable row exists in
// SOME table the instant this function returns a non-nil error — so a
// caller's "durably queued to retry on the next startup" claim is
// STRUCTURALLY true from every failure branch, not just the purge one.
// Idempotent; makes no "traceless" claim while a purge is pending.
func (c *Coordinator) CommitRemoval(ctx context.Context, channelID, login string) (Result, error) {
	login = canonicalLogin(login)
	res := Result{ChannelID: channelID, Login: login}
	if login == "" {
		return res, fmt.Errorf("streamerlifecycle: empty login for channel %q", channelID)
	}

	for _, f := range c.fencers {
		f.Tombstone(login)
	}

	if err := c.movePendingTx(ctx, channelID, login); err != nil {
		return res, fmt.Errorf("streamerlifecycle: move %q (channel %q) to the pending-purge ledger: %w", login, channelID, err)
	}

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

// movePendingTx upserts the pending-purge ledger row and deletes the
// admissions row for login in ONE transaction — the record is written to
// pending_streamer_deletions before it is removed from
// streamer_deletion_admissions, so a crash mid-transaction rolls the whole
// move back (the admissions row survives untouched) rather than ever losing
// the durable record from both tables simultaneously.
func (c *Coordinator) movePendingTx(ctx context.Context, channelID, login string) error {
	now := c.now().Unix()
	return c.db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			INSERT INTO pending_streamer_deletions (login, channel_id, requested_at, attempts)
			VALUES (?, ?, ?, 0)
			ON CONFLICT(login) DO UPDATE SET channel_id = excluded.channel_id, requested_at = excluded.requested_at`,
			login, channelID, now); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM streamer_deletion_admissions WHERE login = ?`, login)
		return err
	})
}

// ArbitratePrepared resolves every row left in the admissions table by an
// unclean shutdown (a prepared removal whose caller never confirmed whether
// its own commit point was reached). It MUST run once at startup, BEFORE
// Reconcile (a row this promotes must already be in the pending table by the
// time Reconcile sweeps it) and before any watch/pubsub/event loop begins.
//
// For each prepared row, keep(login, channelID) reports whether the login is
// still configured and, if so, the config's own stored ChannelID. The
// disposition is channel-aware (the package's identity model: ChannelID is
// the primary identity, login is a lookup key that can be reused by a
// different channel) — configured AND (the row's ChannelID is empty, the
// config's stored ChannelID is empty, or the two agree) => ABORT: the
// caller's commit never happened (or happened for a DIFFERENT identity),
// so the prepared row is deleted and the fence is lifted. Otherwise =>
// PROMOTE: the caller's commit DID happen (the login is absent from config,
// or now bound to a different channel than the aborted row named), so the
// row is moved into the pending table for Reconcile to purge on this same
// startup.
//
// A prepared row for a login that is ALSO already configured to have a
// pending purge (re-added after a still-owed purge, so both the admissions
// and pending rows exist for its old identity) is not special-cased here:
// arbitration only ever inspects the admissions table, so an existing
// pending row (Reconcile's own concern) is untouched regardless of this
// row's disposition.
//
// Runs every row even if one fails, so a single stuck row can never block
// every other one's resolution. Returns how many rows were aborted/promoted
// and the FIRST error encountered (subsequent errors are logged by the
// caller via the returned count discrepancy, not swallowed silently — the
// unresolved rows themselves remain durable and are retried at the next
// startup).
func (c *Coordinator) ArbitratePrepared(ctx context.Context, keep func(login, channelID string) (configured bool, cfgChannelID string)) (aborted, promoted int, err error) {
	rows, err := c.listAdmissions(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("streamerlifecycle: list prepared removals: %w", err)
	}

	var firstErr error
	for _, rec := range rows {
		configured, cfgChannelID := keep(rec.login, rec.channelID)
		sameIdentity := rec.channelID == "" || cfgChannelID == "" || rec.channelID == cfgChannelID

		if configured && sameIdentity {
			if err := c.AbortAdmission(ctx, []string{rec.login}); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("abort prepared removal for %q: %w", rec.login, err)
				}
				continue
			}
			c.Reinstate(rec.login)
			aborted++
			slog.Info("Aborted uncommitted streamer removal recovered after unclean shutdown",
				"streamer", rec.login, "channelID", rec.channelID)
			continue
		}

		if err := c.movePendingTx(ctx, rec.channelID, rec.login); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("promote prepared removal for %q: %w", rec.login, err)
			}
			continue
		}
		promoted++
		slog.Info("Promoted a committed streamer removal recovered after unclean shutdown; purge will be retried",
			"streamer", rec.login, "channelID", rec.channelID)
	}
	return aborted, promoted, firstErr
}

// Delete is the LEGACY, single-streamer, immediate-commit path: it durably
// admits the removal AND completes it in one call, with no separate caller-
// owned commit checkpoint in between (a caller wanting SRAP's fail-closed
// two-phase protocol — durable admission BEFORE an external commit point,
// completion only AFTER it is confirmed reached — calls AdmitRemovals and
// CommitRemoval directly instead; see the miner's settings-apply pipeline,
// which no longer calls Delete). Reimplemented over AdmitRemovals +
// CommitRemoval so its observable behavior is unchanged: tombstone, durable
// intent, atomic purge, in that order, exactly as before SRAP. Retained
// because this package's own tests (and any other single-shot caller with no
// external commit point of its own to coordinate with) still use it.
func (c *Coordinator) Delete(ctx context.Context, channelID, login string) (Result, error) {
	login = canonicalLogin(login)
	res := Result{ChannelID: channelID, Login: login}
	if login == "" {
		return res, fmt.Errorf("streamerlifecycle: empty login for channel %q", channelID)
	}
	if err := c.AdmitRemovals(ctx, []Removal{{ChannelID: channelID, Login: login}}); err != nil {
		return res, fmt.Errorf("streamerlifecycle: record pending deletion for %q (channel %q): %w", login, channelID, err)
	}
	return c.CommitRemoval(ctx, channelID, login)
}

// purgeAndClearTx runs every store's purge AND deletes the durable pending
// row AND (defensively) any leftover admissions row for login in ONE
// transaction, so the login's presence in EITHER ledger disappears if and
// only if the purge commits. The admissions-row clear is defensive: by
// construction CommitRemoval's own movePendingTx already removed it before
// this ever runs, but Reconcile/ReconcileLogin call this directly against a
// row that only ever existed in the pending table, so clearing an
// already-absent admissions row here is simply a no-op DELETE, never a
// correctness requirement.
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
		if _, e := tx.Exec(`DELETE FROM streamer_deletion_admissions WHERE login = ?`, login); e != nil {
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

// Reconcile retries every unfinished deletion recorded in the durable pending
// ledger and is called ONCE at startup, AFTER ArbitratePrepared (so a
// prepared row promoted into this table by arbitration is picked up on this
// SAME pass) and before any watch/pubsub/event loop begins, so reinstating a
// login cannot race a live event. For each pending row it tombstones the
// login, purges every store and deletes the marker in one transaction, then
// lifts the fence on success — so a configured login that was mid-deletion
// when the process died is cleaned and free to record fresh, while a
// non-configured deleted login is simply cleaned. On failure the marker and
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
//
// CALLER CONTRACT: only ever call this for a login being (re-)ADDED, never
// for one that is currently tracked/configured. HasPending (which this uses)
// now also reports a merely-PREPARED row (streamer_deletion_admissions) —
// intent recorded, not yet confirmed committed — so calling this for a
// login whose prepared row leaked from an aborted or still-uncommitted apply
// would tombstone and purge a LIVE streamer's history. Today's only caller
// (applyStreamerDeletions' added-loop) satisfies this by construction.
func (c *Coordinator) ReconcileLogin(ctx context.Context, login string) (bool, error) {
	login = canonicalLogin(login)
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

// HasPending reports whether a durable removal record exists for login in
// EITHER ledger — the pending-purge table (a committed removal still owed a
// purge) OR the admissions table (a prepared removal whose caller's commit
// point has not yet been confirmed reached). Checking only one table would
// leave a same-process re-add window open across the other. Routed through
// WithTx (not a bare QueryRowContext) so a call after the database has been
// closed returns the typed database.ErrClosed instead of a raw driver error.
func (c *Coordinator) HasPending(ctx context.Context, login string) (bool, error) {
	login = canonicalLogin(login)
	if login == "" {
		return false, nil
	}
	var has bool
	err := c.db.WithTx(ctx, func(tx *sql.Tx) error {
		var one int
		e := tx.QueryRow(`
			SELECT 1 FROM pending_streamer_deletions WHERE login = ?
			UNION ALL
			SELECT 1 FROM streamer_deletion_admissions WHERE login = ?
			LIMIT 1`, login, login).Scan(&one)
		if e == sql.ErrNoRows {
			has = false
			return nil
		}
		if e != nil {
			return e
		}
		has = true
		return nil
	})
	return has, err
}

// listPending returns every row of the pending-purge ledger, oldest first.
// Routed through WithTx so a call after Close returns the typed
// database.ErrClosed.
func (c *Coordinator) listPending(ctx context.Context) ([]pendingRecord, error) {
	var out []pendingRecord
	err := c.db.WithTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT login, channel_id FROM pending_streamer_deletions ORDER BY requested_at`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var rec pendingRecord
			if err := rows.Scan(&rec.login, &rec.channelID); err != nil {
				return err
			}
			out = append(out, rec)
		}
		return rows.Err()
	})
	return out, err
}

// listAdmissions returns every row of the SRAP prepare-phase ledger, oldest
// first. Routed through WithTx so a call after Close returns the typed
// database.ErrClosed.
func (c *Coordinator) listAdmissions(ctx context.Context) ([]pendingRecord, error) {
	var out []pendingRecord
	err := c.db.WithTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT login, channel_id FROM streamer_deletion_admissions ORDER BY requested_at`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var rec pendingRecord
			if err := rows.Scan(&rec.login, &rec.channelID); err != nil {
				return err
			}
			out = append(out, rec)
		}
		return rows.Err()
	})
	return out, err
}

// bumpAttempts increments the pending-ledger retry counter for login. Routed
// through WithTx so a call after Close returns the typed database.ErrClosed
// instead of silently no-op'ing on a raw driver error.
func (c *Coordinator) bumpAttempts(ctx context.Context, login string) error {
	return c.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE pending_streamer_deletions SET attempts = attempts + 1 WHERE login = ?`, login)
		return err
	})
}

// Reinstate clears the fence for a login in every store, so re-adding the same
// login (or channel ID) after a deletion starts a clean lifecycle: the old rows
// are already gone and fresh writes are allowed again. Idempotent and safe for a
// login that was never deleted. The caller must only call this once it knows no
// purge is still pending for the login (see ReconcileLogin/HasPending).
func (c *Coordinator) Reinstate(login string) {
	login = canonicalLogin(login)
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
	oldLogin = canonicalLogin(oldLogin)
	newLogin = canonicalLogin(newLogin)
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
