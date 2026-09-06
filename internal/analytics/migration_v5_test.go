package analytics

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// preLedgerModule is the analytics module exactly as a pre-v5 binary ships it:
// the same module name with only migrations v1..v4. Registering it stands in
// for opening a database with the previous release.
type preLedgerModule struct{}

func (preLedgerModule) Name() string { return (&AnalyticsModule{}).Name() }

func (preLedgerModule) Migrations() []database.Migration {
	all := (&AnalyticsModule{}).Migrations()
	var pre []database.Migration
	for _, m := range all {
		if m.Version <= 4 {
			pre = append(pre, m)
		}
	}
	return pre
}

// ledgerV5Module is the analytics module exactly as the release that SHIPPED
// the exact point-event ledger (#303) had it: the same module name with
// migrations v1..v5 and nothing after. It exists so the assertions below keep
// testing the v5 schema itself at version 5, unchanged, now that the module
// has migrations beyond it.
type ledgerV5Module struct{}

func (ledgerV5Module) Name() string { return (&AnalyticsModule{}).Name() }

func (ledgerV5Module) Migrations() []database.Migration {
	all := (&AnalyticsModule{}).Migrations()
	var upToV5 []database.Migration
	for _, m := range all {
		if m.Version <= 5 {
			upToV5 = append(upToV5, m)
		}
	}
	return upToV5
}

// newV5Repository builds a repository over a database pinned at analytics v5 —
// the exact schema #303 shipped — so the rollback harness keeps exercising a
// genuine v5 database rather than whatever the current head happens to be.
// It registers only the v5-capped module and then constructs the repository
// directly, which is why it does not go through NewSQLiteRepository (that
// always migrates to head).
func newV5Repository(t *testing.T, db *database.DB, basePath string) *SQLiteRepository {
	t.Helper()
	if err := db.RegisterModule(ledgerV5Module{}); err != nil {
		t.Fatalf("register v5 module: %v", err)
	}
	if v := moduleVersion(t, db); v != 5 {
		t.Fatalf("pinned module version = %d, want 5", v)
	}
	return &SQLiteRepository{
		db:       db,
		basePath: basePath,
		deleted:  make(map[string]struct{}),
		priority: newTxPriority(),
	}
}

// currentAnalyticsVersion is the module's current migration head, read from
// the module itself rather than hard-coded, so adding a migration never turns
// an unrelated assertion red.
func currentAnalyticsVersion() int {
	head := 0
	for _, m := range (&AnalyticsModule{}).Migrations() {
		if m.Version > head {
			head = m.Version
		}
	}
	return head
}

func openPrivateDB(t *testing.T, path string) *database.DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	return &database.DB{DB: sqlDB}
}

func moduleVersion(t *testing.T, db *database.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("SELECT version FROM schema_versions WHERE module = ?", (&AnalyticsModule{}).Name()).Scan(&v); err != nil {
		t.Fatalf("module version: %v", err)
	}
	return v
}

func tableExists(t *testing.T, db *database.DB, table string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// dumpTable reads every row of a table, in rowid order, as a slice of
// stringified columns — a byte-for-byte witness of the pre-migration data.
func dumpTable(t *testing.T, db *database.DB, table string) []string {
	t.Helper()
	rows, err := db.Query("SELECT * FROM " + table + " ORDER BY rowid")
	if err != nil {
		t.Fatalf("dump %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		var parts []string
		for _, v := range vals {
			if v.Valid {
				parts = append(parts, v.String)
			} else {
				parts = append(parts, "<NULL>")
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dump %s: %v", table, err)
	}
	return out
}

// seedV4Database stands up a populated pre-ledger analytics database: the v4
// schema with production-shaped rows in every table.
func seedV4Database(t *testing.T, path string) map[string][]string {
	t.Helper()
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	if err := db.RegisterModule(preLedgerModule{}); err != nil {
		t.Fatalf("register pre-ledger module: %v", err)
	}
	if v := moduleVersion(t, db); v != 4 {
		t.Fatalf("pre-ledger version = %d, want 4", v)
	}
	ts := time.Now().Add(-time.Hour).UnixMilli()
	stmts := []string{
		`INSERT INTO streamers (id, name, created_at) VALUES (1, 'v4-streamer', ` + itoa(ts) + `)`,
		`INSERT INTO points (streamer_id, timestamp, points, event_type) VALUES (1, ` + itoa(ts) + `, 11310, 'WATCH')`,
		`INSERT INTO points (streamer_id, timestamp, points, event_type) VALUES (1, ` + itoa(ts+1000) + `, 11772, 'WATCH STREAK')`,
		`INSERT INTO points (streamer_id, timestamp, points, event_type) VALUES (1, ` + itoa(ts+2000) + `, 11322, 'WATCH')`,
		`INSERT INTO annotations (streamer_id, timestamp, text, color, event_type) VALUES (1, ` + itoa(ts+1000) + `, '+450 - Watch Streak', '#45c1ff', 'WATCH_STREAK')`,
		`INSERT INTO chat_messages (streamer_id, timestamp, username, display_name, message) VALUES (1, ` + itoa(ts) + `, 'u', 'U', 'hi')`,
		`INSERT INTO prediction_bets (streamer_id, event_id, timestamp, strategy, result_type, placed, won, gained, odds, manual) VALUES (1, 'bet-1', ` + itoa(ts) + `, 'SMART', 'WIN', 100, 250, 150, 2.5, 0)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
	snapshot := map[string][]string{}
	for _, table := range []string{"streamers", "points", "annotations", "chat_messages", "prediction_bets"} {
		snapshot[table] = dumpTable(t, db, table)
	}
	return snapshot
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

// TestMigrationV5AdditiveOnPopulatedV4Database: opening a populated v4
// database with the current module applies exactly the additive v5
// migration — the ledger table appears empty (no backfill of any kind) and
// every pre-existing row of every table is byte-for-byte untouched.
func TestMigrationV5AdditiveOnPopulatedV4Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	before := seedV4Database(t, path)

	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	// The v5 step itself, at exactly v5, on the populated v4 database.
	repo := newV5Repository(t, db, filepath.Dir(path))
	if v := moduleVersion(t, db); v != 5 {
		t.Fatalf("version after upgrade = %d, want 5", v)
	}
	if !tableExists(t, db, "point_events") {
		t.Fatal("point_events table missing after v5")
	}
	for table, rows := range before {
		if after := dumpTable(t, db, table); !reflect.DeepEqual(after, rows) {
			t.Fatalf("%s rows changed by migration:\nbefore=%v\nafter=%v", table, rows, after)
		}
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM point_events`); n != 0 {
		t.Fatalf("point_events has %d rows after migration, want 0 (no historical backfill)", n)
	}

	// The current module then carries the same database to its head, and the
	// v4 rows are STILL byte-for-byte what they were.
	repo, err := NewSQLiteRepository(db, filepath.Dir(path))
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if v := moduleVersion(t, db); v != currentAnalyticsVersion() {
		t.Fatalf("version after upgrade = %d, want %d", v, currentAnalyticsVersion())
	}
	for table, rows := range before {
		if after := dumpTable(t, db, table); !reflect.DeepEqual(after, rows) {
			t.Fatalf("%s rows changed by a later migration:\nbefore=%v\nafter=%v", table, rows, after)
		}
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM point_events`); n != 0 {
		t.Fatalf("point_events has %d rows after migration, want 0 (no historical backfill)", n)
	}

	// Legacy history is still served — as legacy: samples are not exact-backed
	// and the exact ledger is empty for the streamer.
	samples, err := repo.GetPointSamples("v4-streamer", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 3 {
		t.Fatalf("legacy samples = %+v, want 3", samples)
	}
	for _, sample := range samples {
		if sample.Exact {
			t.Fatalf("legacy sample %+v was marked exact after migration", sample)
		}
	}
	exact, err := repo.ExactEarningsBetween("v4-streamer", time.Time{}, time.Time{})
	if err != nil || exact.Events != 0 {
		t.Fatalf("exact earnings for legacy history = %+v err=%v, want none", exact, err)
	}
	// Idempotent: re-registering the current module applies nothing.
	if err := db.RegisterModule(&AnalyticsModule{}); err != nil {
		t.Fatalf("second registration: %v", err)
	}
	if v := moduleVersion(t, db); v != currentAnalyticsVersion() {
		t.Fatalf("version after re-registration = %d, want %d", v, currentAnalyticsVersion())
	}
}

// TestMigrationV5RestartPreservesLedgerAndUniqueness: closing and reopening
// the database keeps the ledger rows and the UNIQUE event identity — a replay
// after a restart is still rejected, both through the repository and at the
// SQL level.
func TestMigrationV5RestartPreservesLedgerAndUniqueness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	ts := time.Now().Add(-time.Hour)

	db := openPrivateDB(t, path)
	repo, err := NewSQLiteRepository(db, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if rec, err := repo.RecordPointEvent("restart-streamer", streakEvent("sha256:restart-1", ts, 1450), 1450, streakAnnotation(450)); err != nil || !rec {
		t.Fatalf("recorded=%v err=%v", rec, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2 := openPrivateDB(t, path)
	defer func() { _ = db2.Close() }()
	repo2, err := NewSQLiteRepository(db2, filepath.Dir(path))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if v := moduleVersion(t, db2); v != currentAnalyticsVersion() {
		t.Fatalf("version after reopen = %d, want %d", v, currentAnalyticsVersion())
	}
	exact, err := repo2.ExactEarningsBetween("restart-streamer", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := ExactEarnings{Breakdown: []ReasonShare{{Reason: "WATCH_STREAK", Gained: 450, Count: 1}}, Events: 1, Since: ts.UnixMilli()}
	if !reflect.DeepEqual(exact, want) {
		t.Fatalf("exact earnings after reopen = %+v, want %+v", exact, want)
	}
	if rec, err := repo2.RecordPointEvent("restart-streamer", streakEvent("sha256:restart-1", ts.Add(time.Minute), 1900), 1900, streakAnnotation(450)); err != nil || rec {
		t.Fatalf("replay after restart: recorded=%v err=%v, want (false, nil)", rec, err)
	}
	if _, err := db2.Exec(`INSERT INTO point_events (streamer_id, event_id, timestamp, reason_code, total_points, points_id) VALUES (1, 'sha256:restart-1', 1, 'WATCH', 1, 1)`); err == nil {
		t.Fatal("UNIQUE(event_id) was not enforced after reopen")
	}
	samples := mustPointSamples(t, repo2, "restart-streamer", time.Time{}, time.Time{}, 0)
	if len(samples) != 1 || !samples[0].Exact {
		t.Fatalf("samples after reopen = %+v, want the single exact sample", samples)
	}
}

// TestPreLedgerBinaryOpensV5DatabaseSafely is the rollback harness: a database
// already at analytics v5 is opened by the previous release's module (v1..v4
// only). Registration is a no-op (no error, no downgrade), the pre-ledger
// queries and writes still work because nothing in them references the new
// table, and the rows such a binary writes are later recognized by the current
// code as legacy (not exact-backed) — the gap is visible, never mislabeled.
func TestPreLedgerBinaryOpensV5DatabaseSafely(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	ts := time.Now().Add(-time.Hour)

	db := openPrivateDB(t, path)
	repo := newV5Repository(t, db, filepath.Dir(path))
	if rec, err := repo.RecordPointEvent("rollback-streamer", streakEvent("sha256:rollback-1", ts, 1450), 1450, streakAnnotation(450)); err != nil || !rec {
		t.Fatalf("recorded=%v err=%v", rec, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// --- previous release opens the v5 database ---
	old := openPrivateDB(t, path)
	if err := old.RegisterModule(preLedgerModule{}); err != nil {
		t.Fatalf("pre-ledger registration on a v5 database must be a no-op, got: %v", err)
	}
	if v := moduleVersion(t, old); v != 5 {
		t.Fatalf("version after pre-ledger registration = %d, want 5 (never downgraded)", v)
	}
	// The previous release's exact write and read statements.
	if _, err := old.Exec("INSERT INTO points (streamer_id, timestamp, points, event_type) VALUES (?, ?, ?, ?)", 1, ts.Add(time.Minute).UnixMilli(), 1462, "WATCH"); err != nil {
		t.Fatalf("pre-ledger RecordPoints statement failed on v5 schema: %v", err)
	}
	if _, err := old.Exec("INSERT INTO annotations (streamer_id, timestamp, text, color, event_type) VALUES (?, ?, ?, ?, ?)", 1, ts.Add(time.Minute).UnixMilli(), "+250 - Raid", "#d9a25c", "RAID"); err != nil {
		t.Fatalf("pre-ledger RecordAnnotation statement failed on v5 schema: %v", err)
	}
	rows, err := old.Query("SELECT timestamp, points, COALESCE(event_type, '') FROM points WHERE streamer_id = ? ORDER BY timestamp ASC", 1)
	if err != nil {
		t.Fatalf("pre-ledger GetPointSamples statement failed on v5 schema: %v", err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("pre-ledger GetPointSamples read failed on v5 schema: %v", err)
	}
	if n != 2 {
		t.Fatalf("pre-ledger read saw %d samples, want 2", n)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	// --- current release reopens: nothing lost, the rollback-era row is legacy ---
	cur := openPrivateDB(t, path)
	repo2, err := NewSQLiteRepository(cur, filepath.Dir(path))
	if err != nil {
		t.Fatalf("reopen with current release: %v", err)
	}
	samples, err := repo2.GetPointSamples("rollback-streamer", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || !samples[0].Exact || samples[1].Exact {
		t.Fatalf("samples = %+v, want [exact ledger sample, legacy rollback-era sample]", samples)
	}
	exact := mustExactEarnings(t, repo2, "rollback-streamer", time.Time{}, time.Time{})
	if exact.Events != 1 {
		t.Fatalf("exact earnings = %+v, want only the pre-rollback event", exact)
	}
	// The rollback-era earning is present as legacy evidence, so the range is
	// classified mixed — never claimed fully exact.
	_, _, acc := ComposeEarnings(exact, EstimateLegacyBreakdown(samples), false)
	if acc.Coverage != EarningsCoverageMixed {
		t.Fatalf("coverage across a rollback gap = %q, want %q", acc.Coverage, EarningsCoverageMixed)
	}
	if err := cur.Close(); err != nil {
		t.Fatal(err)
	}

	// --- previous release again: its retention sweep knows nothing about
	// point_events, so it can orphan a ledger row from its sample. Cutoff
	// between the ledger event (ts) and the rollback-era sample (ts+1m).
	old = openPrivateDB(t, path)
	if err := old.RegisterModule(preLedgerModule{}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"points", "annotations"} {
		if _, err := old.Exec("DELETE FROM "+table+" WHERE timestamp < ?", ts.Add(30*time.Second).UnixMilli()); err != nil {
			t.Fatalf("pre-ledger PruneBefore statement on %s failed on v5 schema: %v", table, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	// --- current release tolerates the orphan: the earning stays an accepted
	// fact, the timeline shows only what survived, and its own sweep removes
	// the orphan with everything else past the cutoff.
	cur = openPrivateDB(t, path)
	repo3, err := NewSQLiteRepository(cur, filepath.Dir(path))
	if err != nil {
		t.Fatalf("reopen after an old-binary prune: %v", err)
	}
	samples, err = repo3.GetPointSamples("rollback-streamer", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].Exact {
		t.Fatalf("after an old-binary prune samples = %+v, want the one legacy sample", samples)
	}
	if exact = mustExactEarnings(t, repo3, "rollback-streamer", time.Time{}, time.Time{}); exact.Events != 1 {
		t.Fatalf("orphaned ledger row lost: exact = %+v, want 1 event", exact)
	}
	if n, err := repo3.PruneBefore(ts.Add(2 * time.Hour)); err != nil || n != 3 {
		t.Fatalf("current-release sweep = %d rows, err=%v; want 3 (legacy sample, orphaned ledger row, legacy annotation)", n, err)
	}
	if n := countRows(t, repo3, `SELECT COUNT(*) FROM point_events`); n != 0 {
		t.Fatalf("%d orphaned ledger rows survived the sweep", n)
	}
	// A new ledger row for the streamer, then the previous release purges
	// the streamer with its own DeleteStreamerTx shape (which does not know
	// point_events either).
	if rec, err := repo3.RecordPointEvent("rollback-streamer", streakEvent("sha256:rollback-2", ts.Add(90*time.Minute), 1900), 1900, nil); err != nil || !rec {
		t.Fatalf("recorded=%v err=%v", rec, err)
	}
	if err := cur.Close(); err != nil {
		t.Fatal(err)
	}
	old = openPrivateDB(t, path)
	if err := old.RegisterModule(preLedgerModule{}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"points", "annotations", "chat_messages", "prediction_bets"} {
		if _, err := old.Exec("DELETE FROM "+table+" WHERE streamer_id = ?", 1); err != nil {
			t.Fatalf("pre-ledger DeleteStreamerTx statement on %s failed on v5 schema: %v", table, err)
		}
	}
	if _, err := old.Exec("DELETE FROM streamers WHERE id = ?", 1); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	// --- the orphan of a purged streamer is unreachable by name (streamer ids
	// are never reused) and is swept by retention; it can never attach to a
	// re-added streamer of the same login.
	cur = openPrivateDB(t, path)
	defer func() { _ = cur.Close() }()
	repo4, err := NewSQLiteRepository(cur, filepath.Dir(path))
	if err != nil {
		t.Fatalf("reopen after an old-binary purge: %v", err)
	}
	if exact = mustExactEarnings(t, repo4, "rollback-streamer", time.Time{}, time.Time{}); exact.Events != 0 {
		t.Fatalf("purged streamer still reports exact earnings: %+v", exact)
	}
	if err := repo4.RecordPoints("rollback-streamer", 100, "WATCH"); err != nil {
		t.Fatal(err)
	}
	if exact = mustExactEarnings(t, repo4, "rollback-streamer", time.Time{}, time.Time{}); exact.Events != 0 {
		t.Fatalf("re-added streamer inherited an orphaned ledger row: %+v", exact)
	}
	if n, err := repo4.PruneBefore(ts.Add(3 * time.Hour)); err != nil || n != 2 {
		t.Fatalf("sweep past everything = %d rows, err=%v; want 2 (the purged streamer's orphaned ledger row and the re-added streamer's sample)", n, err)
	}
	if n := countRows(t, repo4, `SELECT COUNT(*) FROM point_events`); n != 0 {
		t.Fatalf("%d orphaned ledger rows survived the sweep", n)
	}
}
