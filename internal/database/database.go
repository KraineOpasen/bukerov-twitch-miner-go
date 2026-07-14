package database

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
	mu sync.RWMutex
}

type Module interface {
	Name() string
	Migrations() []Migration
}

type Migration struct {
	Version     int
	Description string
	// SQL is the migration body. It may contain multiple statements; the
	// whole migration (body + version bump) is applied in one transaction.
	SQL string
	// Run, when set, is executed instead of SQL inside the migration's
	// transaction. Use it for migrations that need per-statement guards
	// (e.g. AddColumnIfMissing) so a historically half-applied schema can
	// self-heal instead of failing on re-run.
	Run func(tx *sql.Tx) error
}

var (
	// instance/instancePath implement the process-wide singleton: the first
	// successful Open wins and later calls return the same handle. Guarded
	// by instanceMu (not sync.Once) so a FAILED initialization is retryable
	// instead of poisoning every later call — with sync.Once and a local
	// error variable, calls after a failed first attempt used to return
	// (nil, nil): a silent nil *DB that panicked on first use.
	instance     *DB
	instancePath string
	instanceMu   sync.Mutex
)

func Open(basePath string) (*DB, error) {
	instanceMu.Lock()
	defer instanceMu.Unlock()

	if instance != nil {
		if basePath != instancePath {
			// The singleton ignores later basePath arguments by design; make
			// that visible instead of silently returning a DB rooted elsewhere.
			slog.Warn("database.Open called with a different basePath; returning the already-open instance",
				"requested", basePath, "active", instancePath)
		}
		return instance, nil
	}

	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	dbPath := filepath.Join(basePath, "miner.db")
	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	sqlDB.SetMaxOpenConns(1)

	instance = &DB{DB: sqlDB}
	instancePath = basePath
	return instance, nil
}

func (db *DB) RegisterModule(module Module) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	moduleName := module.Name()
	currentVersion, err := db.getModuleVersion(moduleName)
	if err != nil {
		return fmt.Errorf("failed to get module version for %s: %w", moduleName, err)
	}

	migrations := module.Migrations()
	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})

	for _, m := range migrations {
		if m.Version <= currentVersion {
			continue
		}

		slog.Debug("Applying migration",
			"module", moduleName,
			"version", m.Version,
			"description", m.Description,
		)

		if err := db.applyMigration(moduleName, m); err != nil {
			return err
		}
	}

	return nil
}

// applyMigration runs one migration's body AND its schema_versions bump in a
// single transaction, so a crash or failure can never leave the migration
// applied with a stale version (or, for multi-statement bodies, applied
// halfway): either everything commits or everything rolls back. SQLite DDL
// (CREATE/ALTER/DROP) is fully transactional, and the modernc driver executes
// a multi-statement Exec on the transaction's connection, so migration
// bodies need no splitting. The version upsert stays a separate Exec because
// it carries bind parameters (the driver binds one args slice to every
// statement of a multi-statement string).
func (db *DB) applyMigration(moduleName string, m Migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin migration transaction for %s v%d: %w", moduleName, m.Version, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	if m.Run != nil {
		err = m.Run(tx)
	} else {
		_, err = tx.Exec(m.SQL)
	}
	if err != nil {
		return fmt.Errorf("failed to apply migration %s v%d (%s): %w",
			moduleName, m.Version, m.Description, err)
	}

	if _, err := tx.Exec(`
		INSERT INTO schema_versions (module, version, updated_at)
		VALUES (?, ?, strftime('%s', 'now'))
		ON CONFLICT(module) DO UPDATE SET version = excluded.version, updated_at = excluded.updated_at
	`, moduleName, m.Version); err != nil {
		return fmt.Errorf("failed to update module version for %s: %w", moduleName, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit migration %s v%d: %w", moduleName, m.Version, err)
	}
	return nil
}

// AddColumnIfMissing adds a column to a table unless it already exists. It is
// the per-statement guard for ALTER TABLE ADD COLUMN migrations (SQLite has
// no ADD COLUMN IF NOT EXISTS): each column is checked independently against
// pragma_table_info, so a historically half-applied migration (some columns
// present, version stale) heals by adding only what is missing.
func AddColumnIfMissing(tx *sql.Tx, table, column, definition string) error {
	exists, err := columnExists(tx, table, column)
	if err != nil {
		return fmt.Errorf("check column %s.%s: %w", table, column, err)
	}
	if exists {
		slog.Info("Migration column already present, skipping (self-heal)", "table", table, "column", column)
		return nil
	}
	if _, err := tx.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)); err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

func columnExists(tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (db *DB) getModuleVersion(moduleName string) (int, error) {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_versions (
			module TEXT PRIMARY KEY,
			version INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL
		)
	`)
	if err != nil {
		return 0, err
	}

	var version int
	err = db.QueryRow("SELECT version FROM schema_versions WHERE module = ?", moduleName).Scan(&version)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return version, err
}

func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.DB.Close()
}

func (db *DB) RLock() {
	db.mu.RLock()
}

func (db *DB) RUnlock() {
	db.mu.RUnlock()
}

func (db *DB) Lock() {
	db.mu.Lock()
}

func (db *DB) Unlock() {
	db.mu.Unlock()
}
