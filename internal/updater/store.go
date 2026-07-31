package updater

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// storeModuleName is this package's internal/database.Module name -
// "не пересекается" with miner_lifecycle, streamerlifecycle, or any other
// module's schema_versions row (each module owns exactly its own row; see
// internal/database.RegisterModule).
const storeModuleName = "miner_updater"

// handoffDDLV1 persists AT MOST one row (id=1) describing the single
// in-flight-or-just-finished self-update apply, so a restart mid-swap (or a
// supervisor that never restarts the process at all) can be classified at
// the NEXT boot by internal/app (ConsumeHandoff + ClassifyBoot). updated_at
// is epoch seconds via strftime('%s','now'), matching the
// schema_versions/lifecycle convention used elsewhere in this database.
const handoffDDLV1 = `
CREATE TABLE updater_apply_handoff(
	id INTEGER PRIMARY KEY CHECK(id=1),
	from_version TEXT NOT NULL,
	to_version TEXT NOT NULL,
	phase TEXT NOT NULL CHECK(phase IN ('applying','applied')),
	release_url TEXT,
	updated_at INTEGER NOT NULL
)`

// HandoffApplying/HandoffApplied are the two legal values of the phase
// column. Named distinctly from snapshot.go's Phase type (PhaseIdle,
// PhaseDownloading, ...) - which describes the IN-PROCESS updater's live
// state machine, a different concept from this DURABLE handoff row's own
// two-phase lifecycle - so the two never collide even though both packages
// use the word "phase".
const (
	HandoffApplying = "applying"
	HandoffApplied  = "applied"
)

// handoffWriteTimeout bounds every Store write method's own derived
// context. Deliberately BELOW internal/lifecycle's 3s updaterJoinTimeout
// (worker.go:52, "how long Run waits for the process-level updater loop to
// actually return after its ctx is cancelled"): a handoff write that could
// still be blocking for the full updater-join window would starve that
// timeout entirely, so this budget must always leave headroom under it.
// Package-level so tests can shrink it.
var handoffWriteTimeout = 2 * time.Second

// Store is the durable updater-apply-handoff persistence backed by the
// shared *database.DB singleton (module miner_updater), following the same
// convention as internal/lifecycle.Store: registered from within this
// package via db.RegisterModule, never by touching internal/database
// itself.
type Store struct {
	db *database.DB
}

// NewStore registers the miner_updater module's migrations against db and
// returns a ready-to-use Store. Safe to call once per process per db.
func NewStore(db *database.DB) (*Store, error) {
	if err := db.RegisterModule(storeModule{}); err != nil {
		return nil, fmt.Errorf("updater: register %s module: %w", storeModuleName, err)
	}
	return &Store{db: db}, nil
}

// storeModule adapts Store's DDL to database.Module.
type storeModule struct{}

func (storeModule) Name() string { return storeModuleName }

func (storeModule) Migrations() []database.Migration {
	return []database.Migration{
		{Version: 1, Description: "create updater_apply_handoff", SQL: handoffDDLV1},
	}
}

// HandoffRecord is one persisted apply-handoff row, as read back by
// ConsumeHandoff.
type HandoffRecord struct {
	FromVersion string
	ToVersion   string
	Phase       string
	ReleaseURL  string
	UpdatedAt   time.Time
}

// writeCtx derives the bounded, un-cancelable context every write method
// uses: context.WithoutCancel because on every concurrent-exit path the
// updater's OWN ctx (the one checkAndMaybeUpdate was called with) is already
// cancelled by the time the post-swap tail (RecordApplied on success, or
// Clear on any applyUpdate failure) runs - process shutdown races the very
// cycle that is trying to terminalize its own handoff row - and a cancelled
// ctx would fail BeginTx instantly even though the database handle itself is
// still perfectly open. handoffWriteTimeout still caps how long the write
// may block waiting for the single shared SQLite connection, so a write
// started too late still settles (success or a typed database.ErrClosed)
// rather than hanging.
func writeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), handoffWriteTimeout)
}

// RecordApplying UPSERTs the single row (id=1) to phase 'applying',
// overwriting EVERY column of any stale row left over from an earlier,
// never-cleared cycle - the belt-and-braces terminalization that guarantees
// this call always leaves exactly one row describing THIS attempt, never a
// merge of two attempts. Called immediately before the first download byte
// of an apply, so a crash anywhere in applyUpdate (download, verify, swap)
// is trivially "the from/to/release_url this row already names".
func (s *Store) RecordApplying(ctx context.Context, from, to, releaseURL string) error {
	wctx, cancel := writeCtx(ctx)
	defer cancel()

	err := s.db.WithTx(wctx, func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(wctx, `
			INSERT INTO updater_apply_handoff (id, from_version, to_version, phase, release_url, updated_at)
			VALUES (1, ?, ?, 'applying', ?, strftime('%s', 'now'))
			ON CONFLICT(id) DO UPDATE SET
				from_version = excluded.from_version,
				to_version = excluded.to_version,
				phase = excluded.phase,
				release_url = excluded.release_url,
				updated_at = excluded.updated_at
		`, from, to, releaseURL)
		return execErr
	})
	if err != nil {
		return fmt.Errorf("updater: record applying handoff: %w", err)
	}
	return nil
}

// RecordApplied upgrades the row RecordApplying already wrote to phase
// 'applied' - the successful-swap half of the two-write invariant
// applyUpdate always issues in order. The release_url column is
// deliberately left OUT of the ON CONFLICT SET clause (SQLite upsert: a
// column absent from SET keeps its existing value), so the release page URL
// the preceding RecordApplying recorded survives the phase flip unchanged;
// choosing to overwrite it to "" instead was rejected because ConsumeHandoff
// hands ReleaseURL to internal/app's boot-classification logging (the
// BootSucceeded slog line), and losing it there would be a pure regression
// for no benefit. The bare INSERT branch (no prior row) has no release_url
// to preserve and stores "" - reachable only if RecordApplied is ever called
// without a preceding RecordApplying, which production code
// (checkAndMaybeUpdate) never does.
func (s *Store) RecordApplied(ctx context.Context, from, to string) error {
	wctx, cancel := writeCtx(ctx)
	defer cancel()

	err := s.db.WithTx(wctx, func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(wctx, `
			INSERT INTO updater_apply_handoff (id, from_version, to_version, phase, release_url, updated_at)
			VALUES (1, ?, ?, 'applied', '', strftime('%s', 'now'))
			ON CONFLICT(id) DO UPDATE SET
				from_version = excluded.from_version,
				to_version = excluded.to_version,
				phase = excluded.phase,
				updated_at = excluded.updated_at
		`, from, to)
		return execErr
	})
	if err != nil {
		return fmt.Errorf("updater: record applied handoff: %w", err)
	}
	return nil
}

// Clear deletes the single handoff row. Called on EVERY applyUpdate failure
// class (mandatory terminalization: no stale 'applying' row may survive the
// cycle it was written for). A missing row - or a missing table entirely,
// e.g. a Store built directly without NewStore/RegisterModule - is success,
// not an error: Clear's contract is "no row exists after this call", which
// is already true in both cases.
func (s *Store) Clear(ctx context.Context) error {
	wctx, cancel := writeCtx(ctx)
	defer cancel()

	err := s.db.WithTx(wctx, func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(wctx, `DELETE FROM updater_apply_handoff WHERE id = 1`)
		if execErr != nil && isNoSuchTableError(execErr) {
			return nil
		}
		return execErr
	})
	if err != nil {
		return fmt.Errorf("updater: clear handoff: %w", err)
	}
	return nil
}

// ConsumeHandoff reads AND deletes the single row in ONE transaction
// (consume-and-clear, at most once per boot): internal/app calls this
// exactly once during Build, strictly before the lifecycle controller (and
// therefore any new updater cycle that could write a fresh row) exists, so
// there is no concurrent writer to race. A missing table/row is tolerated
// exactly like internal/lifecycle's isNoSuchTableError convention: (zero
// value, found=false, nil error) rather than an error, since "no pending
// handoff" is by far the most common boot (every boot not immediately
// preceded by a self-update).
func (s *Store) ConsumeHandoff(ctx context.Context) (HandoffRecord, bool, error) {
	wctx, cancel := writeCtx(ctx)
	defer cancel()

	var rec HandoffRecord
	var found bool
	var updatedAt int64

	err := s.db.WithTx(wctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(wctx, `
			SELECT from_version, to_version, phase, release_url, updated_at
			FROM updater_apply_handoff WHERE id = 1`)
		scanErr := row.Scan(&rec.FromVersion, &rec.ToVersion, &rec.Phase, &rec.ReleaseURL, &updatedAt)
		if scanErr == sql.ErrNoRows {
			return nil
		}
		if scanErr != nil {
			if isNoSuchTableError(scanErr) {
				return nil
			}
			return scanErr
		}
		found = true
		if _, execErr := tx.ExecContext(wctx, `DELETE FROM updater_apply_handoff WHERE id = 1`); execErr != nil {
			return execErr
		}
		return nil
	})
	if err != nil {
		return HandoffRecord{}, false, fmt.Errorf("updater: consume handoff: %w", err)
	}
	if !found {
		return HandoffRecord{}, false, nil
	}
	rec.UpdatedAt = time.Unix(updatedAt, 0)
	return rec, true, nil
}

// isNoSuchTableError recognizes the modernc.org/sqlite driver's "no such
// table" error without importing its (unstable, internal) error type - a
// deliberate duplicate of internal/lifecycle's helper of the same name
// (that package is never imported from here; the two stay independent, tiny
// copies rather than sharing a dependency for one string check).
func isNoSuchTableError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "no such table")
}

// BootOutcome classifies a consumed durable handoff against the version the
// CURRENT process is actually running (see ClassifyBoot).
type BootOutcome string

const (
	// BootSucceeded: the running version equals the handoff's ToVersion -
	// PHASE-AGNOSTIC (applying OR applied). The completed swap and the
	// process now running on the new binary is stronger ground truth than
	// the best-effort 'applying'->'applied' upgrade a crash between the
	// swap and RecordApplied could have prevented; treating that as a
	// failure would be a false alarm for an update that plainly worked.
	BootSucceeded BootOutcome = "succeeded"
	// BootInterrupted: running == FromVersion and phase is still 'applying'
	// - the process restarted (or crashed) before the swap ever completed.
	BootInterrupted BootOutcome = "interrupted"
	// BootNotEffective: running == FromVersion but phase reached 'applied' -
	// the swap reported success, yet the process that came back up is
	// running the OLD version anyway (e.g. a supervisor that restored a
	// backup, or a restart that never actually happened).
	BootNotEffective BootOutcome = "not_effective"
	// BootAnomalous: running is neither FromVersion nor ToVersion - a state
	// this package's own boot classification cannot explain (manual binary
	// replacement, a handoff row from an unrelated install, ...).
	BootAnomalous BootOutcome = "anomalous"
)

// ClassifyBoot classifies rec (a just-consumed handoff row) against
// runningVersion (internal/version.Version at the call site) into one of the
// four BootOutcome cases documented above. VersionsEqual - never raw string
// equality - is used throughout, since runningVersion (ldflags value, e.g.
// "1.2.3") and rec's From/ToVersion (GitHub release tags, e.g. "v1.2.3") use
// different "v"-prefix conventions for the same release.
func ClassifyBoot(rec HandoffRecord, runningVersion string) BootOutcome {
	switch {
	case VersionsEqual(runningVersion, rec.ToVersion):
		return BootSucceeded
	case VersionsEqual(runningVersion, rec.FromVersion) && rec.Phase == HandoffApplying:
		return BootInterrupted
	case VersionsEqual(runningVersion, rec.FromVersion) && rec.Phase == HandoffApplied:
		return BootNotEffective
	default:
		return BootAnomalous
	}
}
