package lifecycle

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// persistTimeout is the budget Store.Save gives itself for one persist call
// (design v6 §8/item 15): SetMaxOpenConns(1) means a busy foreign
// transaction can delay Save's BeginTx, so a command's reply is
// "within persistTimeout", never instant. Package-level so tests can shrink
// it to make the busy-connection test (§12 test 13) fast.
var persistTimeout = 5 * time.Second

// moduleName is this package's internal/database.Module name (design v6
// §8: "модуль miner_lifecycle — не пересекается со streamerlifecycle").
const moduleName = "miner_lifecycle"

// ddlV1 is the exact schema from design v6 §8. Only desired/reason/
// command_id/updated_at are ever persisted — never a transitional observed
// state (design v6 item 15).
const ddlV1 = `
CREATE TABLE miner_lifecycle_state(
	id INTEGER PRIMARY KEY CHECK(id=1),
	desired TEXT NOT NULL CHECK(desired IN ('running','paused','stopped')),
	reason TEXT,
	command_id TEXT,
	updated_at INTEGER NOT NULL
)`

// Store is the production Persistence implementation, backed by the shared
// *database.DB singleton (design v6 §8: registered from within this
// package via db.RegisterModule — internal/database itself is never
// touched, following the streamerlifecycle/watcher/drops/notifications
// convention).
type Store struct {
	db *database.DB
}

// NewStore registers the miner_lifecycle module's migrations against db and
// returns a ready-to-use Store. Safe to call once per process per db (same
// idempotent-migration contract as every other module).
func NewStore(db *database.DB) (*Store, error) {
	if err := db.RegisterModule(storeModule{}); err != nil {
		return nil, fmt.Errorf("lifecycle: register %s module: %w", moduleName, err)
	}
	return &Store{db: db}, nil
}

// storeModule adapts Store's DDL to database.Module.
type storeModule struct{}

func (storeModule) Name() string { return moduleName }

func (storeModule) Migrations() []database.Migration {
	return []database.Migration{
		{Version: 1, Description: "create miner_lifecycle_state", SQL: ddlV1},
	}
}

// Load reads the persisted desired state. See LoadResult's doc comment and
// design v6 §5.4 for how the outcome classes are distinguished: a missing
// row (Found=false, nil error) is back-compat "running", handled by the
// CALLER, not here; an unrecognized desired value violating the CHECK
// constraint's intent (reachable only from a future schema/manual edit,
// since CHECK prevents writing one through this Store) is a
// *CorruptStateError; any other failure (I/O, database.ErrClosed) is
// returned as-is.
func (s *Store) Load(ctx context.Context) (LoadResult, error) {
	ctx, cancel := context.WithTimeout(ctx, persistTimeout)
	defer cancel()

	var raw string
	var found bool

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT desired FROM miner_lifecycle_state WHERE id = 1`)
		scanErr := row.Scan(&raw)
		if scanErr == sql.ErrNoRows {
			return nil
		}
		if scanErr != nil {
			// A missing TABLE surfaces here as a driver "no such table"
			// error, not sql.ErrNoRows — treat it identically to "no row"
			// (design v6 §5.4: "нет таблицы/строки -> running").
			if isNoSuchTableError(scanErr) {
				return nil
			}
			return scanErr
		}
		found = true
		return nil
	})
	if err != nil {
		return LoadResult{}, fmt.Errorf("lifecycle: load persisted desired state: %w", err)
	}
	if !found {
		return LoadResult{Found: false}, nil
	}

	switch DesiredState(raw) {
	case DesiredRunning, DesiredPaused, DesiredStopped:
		return LoadResult{Desired: DesiredState(raw), Found: true}, nil
	default:
		return LoadResult{}, &CorruptStateError{Raw: raw}
	}
}

// Save durably upserts the desired state plus the reason/commandID that
// caused it (design v6 §8: "persist только desired+reason+command_id+
// updated_at"). updated_at is epoch seconds, matching the
// schema_versions/strftime('%s','now') convention used elsewhere in this
// database package.
func (s *Store) Save(ctx context.Context, desired DesiredState, reason, commandID string) error {
	ctx, cancel := context.WithTimeout(ctx, persistTimeout)
	defer cancel()

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(ctx, `
			INSERT INTO miner_lifecycle_state (id, desired, reason, command_id, updated_at)
			VALUES (1, ?, ?, ?, strftime('%s', 'now'))
			ON CONFLICT(id) DO UPDATE SET
				desired = excluded.desired,
				reason = excluded.reason,
				command_id = excluded.command_id,
				updated_at = excluded.updated_at
		`, string(desired), reason, commandID)
		return execErr
	})
	if err != nil {
		return fmt.Errorf("lifecycle: save desired state: %w", err)
	}
	return nil
}

// isNoSuchTableError recognizes the modernc.org/sqlite driver's "no such
// table" error without importing its (unstable, internal) error type —
// this Store's own migration always creates the table before Load could
// ever run against production, so this branch exists mainly to make an
// out-of-band/rolled-back schema state degrade to "missing row" instead of
// a hard error.
func isNoSuchTableError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "no such table")
}

// memoryPersistence is Config's default Persistence: an in-process-only
// stand-in so a Controller built with a zero Config.Persistence is still
// usable (mainly in tests that don't care about durability). Load always
// reports "not found" (back-compat running); Save always succeeds but is
// not observable across a restart — there is no restart, it's memory.
type memoryPersistence struct{}

func newMemoryPersistence() Persistence { return memoryPersistence{} }

func (memoryPersistence) Load(context.Context) (LoadResult, error) {
	return LoadResult{Found: false}, nil
}

func (memoryPersistence) Save(context.Context, DesiredState, string, string) error {
	return nil
}
