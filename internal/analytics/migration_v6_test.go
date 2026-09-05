package analytics

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// seedV5Database stands up a populated analytics database at exactly v5 — the
// schema #303 shipped — with production-shaped rows in every table it owns,
// and returns a byte-for-byte snapshot of them.
func seedV5Database(t *testing.T, path string) map[string][]string {
	t.Helper()
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	repo := newV5Repository(t, db, filepath.Dir(path))

	ts := time.Now().Add(-time.Hour)
	if rec, err := repo.RecordPointEvent("v5-streamer", streakEvent("sha256:v5-1", ts, 1450), 1450, streakAnnotation(450)); err != nil || !rec {
		t.Fatalf("seed point event: recorded=%v err=%v", rec, err)
	}
	if err := repo.RecordPoints("v5-streamer", 1500, "WATCH"); err != nil {
		t.Fatalf("seed sample: %v", err)
	}
	if err := repo.RecordChatMessage("v5-streamer", ChatMessage{Username: "u", DisplayName: "U", Message: "hi"}); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	if err := repo.RecordBet(BetRecord{
		EventID: "bet-v5", Streamer: "v5-streamer", Timestamp: ts.UnixMilli(),
		Strategy: "SMART", ResultType: "WIN", Placed: 100, Won: 250, Gained: 150, Odds: 2.5,
	}); err != nil {
		t.Fatalf("seed bet: %v", err)
	}

	snapshot := map[string][]string{}
	for _, table := range []string{"streamers", "points", "point_events", "annotations", "chat_messages", "prediction_bets"} {
		snapshot[table] = dumpTable(t, db, table)
	}
	return snapshot
}

// indexNames returns every index SQLite holds for a table, sorted.
func indexNames(t *testing.T, db *database.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name=? AND name NOT LIKE 'sqlite_%' ORDER BY name`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func tableDDL(t *testing.T, db *database.DB, table string) string {
	t.Helper()
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&ddl); err != nil {
		t.Fatalf("ddl for %s: %v", table, err)
	}
	return ddl
}

// TestMigrationV6AdditiveOnPopulatedV5Database is the populated upgrade proof:
// opening a real, populated v5 database with the current module applies
// exactly the additive v6 migration. The two observation tables appear EMPTY
// (no backfill of any kind), every pre-existing row of every v1..v5 table is
// byte-for-byte untouched, and every #303 constraint and index on
// point_events survives intact.
func TestMigrationV6AdditiveOnPopulatedV5Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	before := seedV5Database(t, path)

	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()

	pointEventsDDLBefore := tableDDL(t, db, "point_events")
	pointEventsIdxBefore := indexNames(t, db, "point_events")

	repo, err := NewSQLiteRepository(db, filepath.Dir(path))
	if err != nil {
		t.Fatalf("upgrade v5 -> v6: %v", err)
	}
	if v := moduleVersion(t, db); v != 6 {
		t.Fatalf("version after upgrade = %d, want 6", v)
	}
	for _, table := range []string{"prediction_observations", "prediction_observation_sessions"} {
		if !tableExists(t, db, table) {
			t.Fatalf("%s missing after v6", table)
		}
		if n := countRows(t, repo, `SELECT COUNT(*) FROM `+table); n != 0 {
			t.Fatalf("%s has %d rows after migration, want 0 (no backfill)", table, n)
		}
	}

	// Every v1..v5 row is byte-for-byte what it was.
	for table, rows := range before {
		if after := dumpTable(t, db, table); !reflect.DeepEqual(after, rows) {
			t.Fatalf("%s rows changed by v6:\nbefore=%v\nafter=%v", table, rows, after)
		}
	}

	// #303's own schema is untouched: same DDL, same indexes, same UNIQUE.
	if after := tableDDL(t, db, "point_events"); after != pointEventsDDLBefore {
		t.Fatalf("v6 altered point_events DDL:\nbefore=%s\nafter=%s", pointEventsDDLBefore, after)
	}
	if after := indexNames(t, db, "point_events"); !reflect.DeepEqual(after, pointEventsIdxBefore) {
		t.Fatalf("v6 altered point_events indexes: before=%v after=%v", pointEventsIdxBefore, after)
	}
	if _, err := db.Exec(`INSERT INTO point_events (streamer_id, event_id, timestamp, reason_code, total_points, points_id) VALUES (1, 'sha256:v5-1', 1, 'WATCH', 1, 1)`); err == nil {
		t.Fatal("UNIQUE(event_id) on point_events was not enforced after v6")
	}

	// The v5 history still reads exactly as it did.
	samples, err := repo.GetPointSamples("v5-streamer", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || !samples[0].Exact || samples[1].Exact {
		t.Fatalf("samples after v6 = %+v, want [exact ledger sample, legacy sample]", samples)
	}
	exact := mustExactEarnings(t, repo, "v5-streamer", time.Time{}, time.Time{})
	if exact.Events != 1 {
		t.Fatalf("exact earnings after v6 = %+v, want the single v5 event", exact)
	}

	// Idempotent: re-registering applies nothing.
	if err := db.RegisterModule(&AnalyticsModule{}); err != nil {
		t.Fatalf("second registration: %v", err)
	}
	if v := moduleVersion(t, db); v != 6 {
		t.Fatalf("version after re-registration = %d, want 6", v)
	}
}

// TestMigrationV6SchemaContract pins the v6 schema's load-bearing structure:
// the exact-pair uniqueness, the identity CHECKs, the round-incarnation
// biconditional, the closed enums, the required indexes, and the two
// uniqueness constraints that deliberately do NOT exist.
func TestMigrationV6SchemaContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	if _, err := NewSQLiteRepository(db, filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}

	insert := func(cols string, args ...interface{}) error {
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")
		_, err := db.Exec(`INSERT INTO prediction_observations (`+cols+`) VALUES (`+placeholders+`)`, args...)
		return err
	}
	base := `observation_id, collector_session_id, collector_epoch, collector_sequence,
	         pool_instance_id, kind, producer_time_source, received_at_ms,
	         payload_version, payload_json, observation_sha256`
	baseArgs := func(id string, epoch, seq int64) []interface{} {
		return []interface{}{id, "s1", epoch, seq, "pool-1", KindChannelEvent, TimeSourceReceiver, int64(1), 1, `{"phase":"ROUND_CREATED"}`, "sha256:x"}
	}

	if err := insert(base, baseArgs("o1", 1, 1)...); err != nil {
		t.Fatalf("minimal valid row rejected: %v", err)
	}

	t.Run("exact pair is unique", func(t *testing.T) {
		if err := insert(base, baseArgs("o2", 1, 1)...); err == nil {
			t.Fatal("UNIQUE(collector_epoch, collector_sequence) not enforced")
		}
	})
	t.Run("observation_id is unique", func(t *testing.T) {
		if err := insert(base, baseArgs("o1", 1, 2)...); err == nil {
			t.Fatal("UNIQUE(observation_id) not enforced")
		}
	})
	t.Run("event_id is deliberately NOT unique", func(t *testing.T) {
		for i, seq := range []int64{10, 11} {
			args := append(baseArgs("evt-"+itoa(int64(i)), 1, seq), "same-event")
			if err := insert(base+", event_id", args...); err != nil {
				t.Fatalf("two facts for one round must both be storable: %v", err)
			}
		}
	})
	t.Run("source_fingerprint is deliberately NOT unique", func(t *testing.T) {
		for i, seq := range []int64{20, 21} {
			args := append(baseArgs("fp-"+itoa(int64(i)), 1, seq), "sha256:same")
			if err := insert(base+", source_fingerprint", args...); err != nil {
				t.Fatalf("a duplicate delivery is itself a fact: %v", err)
			}
		}
	})
	t.Run("a parent id requires its channel id", func(t *testing.T) {
		for _, pair := range [][2]string{
			{"routed_streamer_id", "routed_channel_id"},
			{"round_owner_streamer_id", "round_owner_channel_id"},
			{"retention_group_owner_streamer_id", "retention_group_owner_channel_id"},
		} {
			args := append(baseArgs("half-"+pair[0], 1, int64(len(pair[0]))+100), int64(7))
			if err := insert(base+", "+pair[0], args...); err == nil {
				t.Fatalf("%s without %s was accepted", pair[0], pair[1])
			}
		}
	})
	t.Run("round incarnation iff retention-group channel", func(t *testing.T) {
		args := append(baseArgs("inc-only", 1, 200), "round:abc")
		if err := insert(base+", round_incarnation_id", args...); err == nil {
			t.Fatal("round_incarnation_id without a retention-group channel was accepted")
		}
		args = append(baseArgs("chan-only", 1, 201), "chan-1")
		if err := insert(base+", retention_group_owner_channel_id", args...); err == nil {
			t.Fatal("retention-group channel without a round incarnation was accepted")
		}
		args = append(baseArgs("both", 1, 202), "round:abc", "chan-1")
		if err := insert(base+", round_incarnation_id, retention_group_owner_channel_id", args...); err != nil {
			t.Fatalf("a complete round identity was rejected: %v", err)
		}
	})
	t.Run("closed enums", func(t *testing.T) {
		args := baseArgs("bad-kind", 1, 300)
		args[5] = "not-a-kind"
		if err := insert(base, args...); err == nil {
			t.Fatal("kind outside the closed set was accepted")
		}
		args = baseArgs("bad-time", 1, 301)
		args[6] = "WHENEVER"
		if err := insert(base, args...); err == nil {
			t.Fatal("producer_time_source outside the closed set was accepted")
		}
		args = append(baseArgs("bad-topic", 1, 302), "chat-room-v1")
		if err := insert(base+", source_topic_type", args...); err == nil {
			t.Fatal("source_topic_type outside the closed set was accepted")
		}
		args = append(baseArgs("bad-msg", 1, 303), "viewcount")
		if err := insert(base+", source_message_type", args...); err == nil {
			t.Fatal("source_message_type outside the closed set was accepted")
		}
	})
	t.Run("required indexes exist", func(t *testing.T) {
		got := indexNames(t, db, "prediction_observations")
		for _, want := range []string{
			"idx_predobs_session",
			"idx_predobs_epoch",
			"idx_predobs_routed_identity",
			"idx_predobs_retention_identity",
			"idx_predobs_round",
			"idx_predobs_null_round_retention",
			"idx_predobs_fingerprint",
		} {
			found := false
			for _, g := range got {
				if g == want {
					found = true
				}
			}
			if !found {
				t.Fatalf("index %s missing; have %v", want, got)
			}
		}
	})
	t.Run("no foreign keys", func(t *testing.T) {
		for _, table := range []string{"prediction_observations", "prediction_observation_sessions"} {
			if ddl := tableDDL(t, db, table); strings.Contains(strings.ToUpper(ddl), "FOREIGN KEY") {
				t.Fatalf("%s declares a FOREIGN KEY under a database that never enables PRAGMA foreign_keys", table)
			}
		}
	})
	t.Run("session close state is closed and coherent", func(t *testing.T) {
		_, err := db.Exec(`INSERT INTO prediction_observation_sessions
			(collector_session_id, producer_revision, started_at_ms, closed_at_ms, close_state,
			 committed_count, dropped_count, unsettled_obligation_count,
			 post_fence_producer_count, producer_shutdown_uncertain_count)
			VALUES ('bad', 'r', 1, NULL, 'HALF_CLOSED', 0, 0, 0, 0, 0)`)
		if err == nil {
			t.Fatal("close_state outside the closed set was accepted")
		}
		_, err = db.Exec(`INSERT INTO prediction_observation_sessions
			(collector_session_id, producer_revision, started_at_ms, closed_at_ms, close_state,
			 committed_count, dropped_count, unsettled_obligation_count,
			 post_fence_producer_count, producer_shutdown_uncertain_count)
			VALUES ('bad2', 'r', 1, NULL, 'COMPLETE', 0, 0, 0, 0, 0)`)
		if err == nil {
			t.Fatal("a finalized session with no close time was accepted")
		}
		_, err = db.Exec(`INSERT INTO prediction_observation_sessions
			(collector_session_id, producer_revision, started_at_ms, closed_at_ms, close_state,
			 committed_count, dropped_count, unsettled_obligation_count,
			 post_fence_producer_count, producer_shutdown_uncertain_count)
			VALUES ('bad3', 'r', 1, NULL, 'OPEN', -1, 0, 0, 0, 0)`)
		if err == nil {
			t.Fatal("a negative session counter was accepted")
		}
	})
}

// TestMigrationV6SessionEpochUsesLastInsertId proves the epoch allocator is a
// committed INSERT + LastInsertId, not MAX(epoch)+1: after deleting the only
// session, the next epoch is still strictly greater, so two collector runs can
// never share an epoch.
func TestMigrationV6SessionEpochUsesLastInsertId(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	repo, err := NewSQLiteRepository(db, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	first, err := repo.OpenObservationSession(ctx, "session-a", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM prediction_observation_sessions WHERE collector_epoch = ?`, first); err != nil {
		t.Fatal(err)
	}
	second, err := repo.OpenObservationSession(ctx, "session-b", 2000)
	if err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Fatalf("epoch %d did not advance past the deleted epoch %d — AUTOINCREMENT was reset or MAX()+1 was used", second, first)
	}
}

// TestMigrationV6SessionFinalizationIsOneCAS proves a session is finalized
// exactly once: the first CAS applies, and any later attempt leaves the row
// untouched no matter what accounting it carries.
func TestMigrationV6SessionFinalizationIsOneCAS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	repo, err := NewSQLiteRepository(db, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	epoch, err := repo.OpenObservationSession(ctx, "session-cas", 1000)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := repo.FinalizeObservationSession(ctx, epoch, ObservationAccounting{Committed: 3}, 2000)
	if err != nil || !applied {
		t.Fatalf("first finalization applied=%v err=%v", applied, err)
	}
	applied, err = repo.FinalizeObservationSession(ctx, epoch, ObservationAccounting{Committed: 999, Dropped: 5}, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("a second finalization rewrote a closed session")
	}

	var state string
	var closedAt int64
	var committed, dropped int64
	if err := db.QueryRow(`SELECT close_state, closed_at_ms, committed_count, dropped_count
		FROM prediction_observation_sessions WHERE collector_epoch = ?`, epoch).
		Scan(&state, &closedAt, &committed, &dropped); err != nil {
		t.Fatal(err)
	}
	if state != SessionComplete || closedAt != 2000 || committed != 3 || dropped != 0 {
		t.Fatalf("session after a rejected second finalization = %s/%d/%d/%d, want COMPLETE/2000/3/0",
			state, closedAt, committed, dropped)
	}
}

// TestMigrationV6PreV6BinaryReadsAndWrites is the honest downgrade harness. A
// pre-v6 binary (module capped at v5) opens a v6 database: registration is a
// no-op, it never downgrades the version, and every v5-era statement still
// works because none of them references the new tables.
//
// It also pins the asymmetry that makes rolling back a POLICY decision rather
// than a safe default: the pre-v6 binary's own streamer purge removes the
// login's v5 rows and leaves the observation facts behind, so a privacy
// erasure performed by that binary is NOT complete.
func TestMigrationV6PreV6BinaryReadsAndWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	repo, err := NewSQLiteRepository(db, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordPoints("rollback6", 100, "WATCH"); err != nil {
		t.Fatal(err)
	}
	var streamerID int64
	if err := db.QueryRow(`SELECT id FROM streamers WHERE name = ?`, "rollback6").Scan(&streamerID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO prediction_observations
		(observation_id, collector_session_id, collector_epoch, collector_sequence,
		 pool_instance_id, kind, producer_time_source, received_at_ms,
		 payload_version, payload_json, observation_sha256,
		 routed_streamer_id, routed_channel_id)
		VALUES ('o-roll', 's', 1, 1, 'pool', ?, ?, 1, 1, '{"phase":"ROUND_CREATED"}', 'sha256:x', ?, 'chan-roll')`,
		KindChannelEvent, TimeSourceReceiver, streamerID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	old := openPrivateDB(t, path)
	if err := old.RegisterModule(ledgerV5Module{}); err != nil {
		t.Fatalf("pre-v6 registration on a v6 database must be a no-op, got: %v", err)
	}
	if v := moduleVersion(t, old); v != 6 {
		t.Fatalf("version after pre-v6 registration = %d, want 6 (never downgraded)", v)
	}
	// Every v5-era statement still works.
	if _, err := old.Exec(`INSERT INTO points (streamer_id, timestamp, points, event_type) VALUES (?, ?, ?, ?)`,
		streamerID, time.Now().UnixMilli(), 200, "WATCH"); err != nil {
		t.Fatalf("pre-v6 RecordPoints statement failed on a v6 schema: %v", err)
	}
	// ...and its purge leaves the observation trail behind: that is exactly
	// why a rollback below v6 cannot claim privacy-erasure completeness.
	for _, table := range []string{"points", "point_events", "annotations", "chat_messages", "prediction_bets"} {
		if _, err := old.Exec(`DELETE FROM `+table+` WHERE streamer_id = ?`, streamerID); err != nil {
			t.Fatalf("pre-v6 purge statement on %s failed: %v", table, err)
		}
	}
	if _, err := old.Exec(`DELETE FROM streamers WHERE id = ?`, streamerID); err != nil {
		t.Fatal(err)
	}
	var orphans int
	if err := old.QueryRow(`SELECT COUNT(*) FROM prediction_observations WHERE routed_channel_id = 'chan-roll'`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 1 {
		t.Fatalf("pre-v6 purge left %d observation rows, want 1 — the documented downgrade asymmetry", orphans)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	// The current binary reopens and CAN complete the erasure by channel.
	cur := openPrivateDB(t, path)
	defer func() { _ = cur.Close() }()
	repo2, err := NewSQLiteRepository(cur, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var removed int64
	if err := cur.WithTx(context.Background(), func(tx *sql.Tx) error {
		var e error
		removed, e = repo2.EraseObservationsForIdentityTx(tx, ObservationIdentity{ChannelID: "chan-roll", Login: "rollback6"})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("current binary erased %d observation rows, want 1", removed)
	}
}
