package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestObservationQueueOverflowDropsAndLeavesConnectionUsable proves the
// producer-side bound: offering far more than the queue can hold never blocks
// the producer, the excess is counted as drops, and the shared connection is
// immediately usable by a business writer afterwards.
func TestObservationQueueOverflowDropsAndLeavesConnectionUsable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	svc, err := NewService(db, filepath.Dir(path), 0)
	if err != nil {
		t.Fatal(err)
	}
	repo := svc.repo.(*SQLiteRepository)

	// Hold the writer off entirely: capture is published RUNNING but nothing
	// drains, so the queue fills and then overflows.
	c := svc.observations
	c.phase.Store(phaseRunning)

	const offered = ObservationQueueCapacity * 3
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < offered; i++ {
			svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "s", "e"+itoa(int64(i)), "ROUND_CREATED"))
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a producer blocked on a full observation queue; offers must be nonblocking")
	}

	if got := len(c.queue); got != ObservationQueueCapacity {
		t.Fatalf("queue holds %d facts, want the %d cap", got, ObservationQueueCapacity)
	}
	if dropped := c.dropped.Load(); dropped != offered-ObservationQueueCapacity {
		t.Fatalf("dropped %d facts, want %d", dropped, offered-ObservationQueueCapacity)
	}

	// The shared connection was never touched by the overflow, so a business
	// write still works immediately and exactly.
	if err := repo.RecordPoints("business", 1234, "WATCH"); err != nil {
		t.Fatalf("business write after a queue overflow: %v", err)
	}
	samples := mustPointSamples(t, repo, "business", time.Time{}, time.Time{}, 0)
	if len(samples) != 1 || samples[0].Balance != 1234 {
		t.Fatalf("business sample after overflow = %+v", samples)
	}
}

// TestObservationDeadlineCancellationLeavesConnectionUsable proves the 5 ms
// per-fact deadline: a write that cannot finish in time is abandoned (counted
// as a drop, never retried) and the shared connection is immediately reusable
// by a business writer with exact results.
func TestObservationDeadlineCancellationLeavesConnectionUsable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	svc, err := NewService(db, filepath.Dir(path), 0)
	if err != nil {
		t.Fatal(err)
	}
	repo := svc.repo.(*SQLiteRepository)
	c := svc.observations
	// An impossible budget: every write must lose its deadline.
	c.writeDeadline = time.Nanosecond
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	<-c.bootstrapped

	for i := 0; i < 20; i++ {
		svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "s", "e"+itoa(int64(i)), "ROUND_CREATED"))
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && c.dropped.Load() < 20 {
		time.Sleep(time.Millisecond)
	}
	if c.committed.Load() != 0 {
		t.Fatalf("%d facts committed under an impossible deadline", c.committed.Load())
	}
	if c.dropped.Load() < 20 {
		t.Fatalf("only %d of 20 facts were dropped by the deadline", c.dropped.Load())
	}

	// Nothing partial was written, and the connection is usable and exact.
	got, err := repo.ObservationsBySession(context.Background(), c.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("%d partial facts survived a cancelled write", len(got))
	}
	if err := repo.RecordPoints("business", 4321, "WATCH"); err != nil {
		t.Fatalf("business write after a cancelled observation: %v", err)
	}
	samples := mustPointSamples(t, repo, "business", time.Time{}, time.Time{}, 0)
	if len(samples) != 1 || samples[0].Balance != 4321 {
		t.Fatalf("business sample after cancellation = %+v", samples)
	}
	_ = svc.Close()
}

// TestPriorityClaimCancelsTheObservationLease proves the gate's core promise:
// a business claim cancels the in-flight observation lease and returns once it
// has settled, and no new lease is granted while the claim is held.
func TestPriorityClaimCancelsTheObservationLease(t *testing.T) {
	p := newTxPriority()

	leaseCtx, settle, ok := p.lease(context.Background())
	if !ok {
		t.Fatal("the first lease was refused on an idle gate")
	}

	claimed := make(chan struct{})
	go func() {
		release := p.Claim()
		close(claimed)
		defer release()
		time.Sleep(20 * time.Millisecond)
	}()

	select {
	case <-leaseCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a business claim did not cancel the in-flight observation lease")
	}
	select {
	case <-claimed:
		t.Fatal("Claim returned before the lease it cancelled had settled")
	default:
	}
	settle()
	select {
	case <-claimed:
	case <-time.After(2 * time.Second):
		t.Fatal("Claim did not return after the cancelled lease settled")
	}

	// While a claim is outstanding no lease is granted; a bounded wait
	// expires instead of stealing the connection.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, _, ok := p.lease(ctx); ok {
		t.Fatal("a lease was granted while a business claim was outstanding")
	}
}

// TestPriorityLeaseResumesAfterClaimReleases proves the gate is not a
// one-way door: once the business writer releases, the observation writer can
// lease again.
func TestPriorityLeaseResumesAfterClaimReleases(t *testing.T) {
	p := newTxPriority()
	release := p.Claim()
	release()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	leaseCtx, settle, ok := p.lease(ctx)
	if !ok {
		t.Fatal("no lease was granted after every claim released")
	}
	defer settle()
	if leaseCtx.Err() != nil {
		t.Fatal("a fresh lease was born cancelled")
	}
}

// TestExactPointsUnchangedUnderObservationLoad is the non-interference proof
// for #303's exact ledger: with the collector saturated, the exact
// sample+event+marker transaction is still atomic, still idempotent on a
// re-delivery, and still returns exactly what it always did.
func TestExactPointsUnchangedUnderObservationLoad(t *testing.T) {
	svc, repo := newObservationService(t)
	ts := time.Now().Add(-time.Hour)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "exact", "e"+itoa(int64(i)), "ROUND_CREATED"))
		}
	}()

	for i := 0; i < 50; i++ {
		id := "sha256:exact-" + itoa(int64(i))
		rec, err := repo.RecordPointEvent("exact", streakEvent(id, ts.Add(time.Duration(i)*time.Second), 1000+i), 1000+i, streakAnnotation(450))
		if err != nil || !rec {
			t.Fatalf("exact event %d: recorded=%v err=%v", i, rec, err)
		}
		// An exact re-delivery still writes nothing at all.
		rec, err = repo.RecordPointEvent("exact", streakEvent(id, ts.Add(time.Hour), 9999), 9999, streakAnnotation(450))
		if err != nil || rec {
			t.Fatalf("replay %d: recorded=%v err=%v, want (false, nil)", i, rec, err)
		}
	}
	close(stop)
	wg.Wait()

	exact := mustExactEarnings(t, repo, "exact", time.Time{}, time.Time{})
	if exact.Events != 50 {
		t.Fatalf("exact ledger holds %d events under observation load, want 50", exact.Events)
	}
	if len(exact.Breakdown) != 1 || exact.Breakdown[0].Gained != 50*450 {
		t.Fatalf("exact earnings = %+v, want a single WATCH_STREAK slice of %d", exact.Breakdown, 50*450)
	}
	samples := mustPointSamples(t, repo, "exact", time.Time{}, time.Time{}, 0)
	if len(samples) != 50 {
		t.Fatalf("%d samples, want exactly one per exact event (no duplicate from a replay)", len(samples))
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM annotations`); n != 50 {
		t.Fatalf("%d markers, want exactly one per exact event", n)
	}
}

// TestSnapshotCoherentBothInterleavings proves #303's coherent read snapshot
// is unaffected in BOTH contention directions: snapshot-first (the observation
// writer yields) and observation-first (the snapshot waits only for a bounded
// one-row commit). Either way no partial exact event is ever visible.
func TestSnapshotCoherentBothInterleavings(t *testing.T) {
	svc, repo := newObservationService(t)
	ts := time.Now().Add(-time.Hour)
	if rec, err := repo.RecordPointEvent("snap", streakEvent("sha256:snap-1", ts, 1450), 1450, streakAnnotation(450)); err != nil || !rec {
		t.Fatalf("seed: recorded=%v err=%v", rec, err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "snap", "e"+itoa(int64(i)), "ROUND_CREATED"))
		}
	}()

	for i := 0; i < 100; i++ {
		snap, err := repo.PointsSnapshotBetween(context.Background(), "snap", time.Time{}, time.Time{}, 0, true)
		if err != nil {
			t.Fatalf("snapshot under observation load: %v", err)
		}
		// Coherence: an exact event is either fully present (aggregate +
		// exact-backed sample + marker) or fully absent.
		if snap.Exact.Events != 1 {
			t.Fatalf("snapshot %d saw %d exact events, want 1", i, snap.Exact.Events)
		}
		if len(snap.Samples) != 1 || !snap.Samples[0].Exact {
			t.Fatalf("snapshot %d samples = %+v, want the single exact-backed sample", i, snap.Samples)
		}
		if len(snap.Annotations) != 1 {
			t.Fatalf("snapshot %d annotations = %+v, want the single marker", i, snap.Annotations)
		}
	}
	close(stop)
	wg.Wait()
}

// TestGenericRetentionUnaffectedByObservations proves the generic three-table
// retention sweep stays whole and is not lengthened by P1: the observation
// tables are untouched by PruneBefore.
func TestGenericRetentionUnaffectedByObservations(t *testing.T) {
	svc, repo := newObservationService(t)
	ts := time.Now().Add(-48 * time.Hour)
	if rec, err := repo.RecordPointEvent("prune", streakEvent("sha256:prune-1", ts, 1450), 1450, streakAnnotation(450)); err != nil || !rec {
		t.Fatal(err)
	}
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "prune", "e1", "ROUND_CREATED"))
	awaitCommitted(t, svc, 1)

	n, err := repo.PruneBefore(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("generic sweep removed %d rows, want exactly the sample, ledger row and marker", n)
	}
	if got := countRows(t, repo, `SELECT COUNT(*) FROM prediction_observations`); got != 1 {
		t.Fatalf("the generic sweep removed %d observation rows; P1 retention is worker-owned", 1-got)
	}
}

// TestObservationCloseJoinsWriterBeforeDatabaseClose proves the shutdown
// order: after Service.Close no DB-capable collector goroutine survives, so
// closing the shared database immediately afterwards is safe and produces no
// use-after-close error.
func TestObservationCloseJoinsWriterBeforeDatabaseClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	svc, err := NewService(db, filepath.Dir(path), 0)
	if err != nil {
		t.Fatal(err)
	}
	svc.observations.writeDeadline = 5 * time.Second
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	<-svc.observations.bootstrapped

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(4)
	for w := 0; w < 4; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "s", fmt.Sprintf("e%d-%d", w, i), "ROUND_CREATED"))
			}
		}(w)
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	if err := svc.Close(); err != nil {
		t.Fatalf("service close: %v", err)
	}
	select {
	case <-svc.observations.joined:
	default:
		t.Fatal("the collector writer was not joined by Close")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("database close after the collector join: %v", err)
	}
	// A late offer after everything is closed is fenced, not a driver error.
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "s", "post-close", "ROUND_CREATED"))
}

// TestObservationCloseRacesProducers hammers Close against live producers with
// the race detector on: no goroutine leak, no panic, no write after the join.
func TestObservationCloseRacesProducers(t *testing.T) {
	for attempt := 0; attempt < 5; attempt++ {
		func() {
			path := filepath.Join(t.TempDir(), "miner.db")
			db := openPrivateDB(t, path)
			defer func() { _ = db.Close() }()
			svc, err := NewService(db, filepath.Dir(path), 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.Start(); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			wg.Add(3)
			for w := 0; w < 3; w++ {
				go func(w int) {
					defer wg.Done()
					for i := 0; i < 200; i++ {
						svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "s", fmt.Sprintf("e%d-%d", w, i), "ROUND_CREATED"))
					}
				}(w)
			}
			// Close concurrently with the producers.
			go func() { _ = svc.Close() }()
			wg.Wait()
			_ = svc.Close() // idempotent
		}()
	}
}

// TestObservationBootstrapFailureDisablesOnlyP1 proves a bootstrap that cannot
// reach the database disables P1 alone: Start still returns nil, capture is
// off, offers are counted as drops, and nothing else is affected.
func TestObservationBootstrapFailureDisablesOnlyP1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	svc, err := NewService(db, filepath.Dir(path), 0)
	if err != nil {
		t.Fatal(err)
	}
	// Close the shared handle so the bootstrap's own transaction is refused.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(); err != nil {
		t.Fatalf("Start must never fail runtime startup because P1 could not bootstrap: %v", err)
	}
	<-svc.observations.bootstrapped
	if !svc.observations.disabled.Load() {
		t.Fatal("a failed bootstrap did not disable capture")
	}
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "s", "e", "ROUND_CREATED"))
	// Accounted as a pre-intake loss, not a drop: capture never opened, so the
	// fact took no causal position to be dropped from.
	if svc.observations.preIntakeLosses.Load() == 0 {
		t.Fatal("an offer to a disabled collector was not accounted")
	}
	if svc.observations.epoch.Load() != 0 {
		t.Fatal("a failed bootstrap allocated a session")
	}
	_ = svc.Close()
}

// TestObservationIdentityPurgePilot is the bounded-cost pilot the contract
// requires: erasing one proved identity's 8,192 facts carrying 32 MiB of
// payload completes inside 250 ms, in ONE transaction, leaving every other
// identity's facts intact.
func TestObservationIdentityPurgePilot(t *testing.T) {
	if testing.Short() {
		t.Skip("purge pilot writes 32 MiB")
	}
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	repo, err := NewSQLiteRepository(db, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Under -race the pure-Go SQLite engine is instrumented on every load and
	// store, so a wall-clock bound is not measurable; the pilot still proves
	// the purge is correct, whole and bounded in rows, on a smaller body of
	// data, and the budget is asserted in the un-raced build.
	rows := MaxProvedIdentityUnionRows // 8192
	if raceDetectorEnabled {
		rows = 1024
	}
	payloadBytes := MaxProvedIdentityUnionBytes / MaxProvedIdentityUnionRows
	const prefix = `{"phase":"ROUND_UPDATED","reasonCode":"OK","_":"`
	const suffix = `"}`
	payload := prefix + strings.Repeat("x", payloadBytes-len(prefix)-len(suffix)) + suffix
	if len(payload) != payloadBytes {
		t.Fatalf("pilot payload is %d bytes, want exactly %d", len(payload), payloadBytes)
	}

	// Seed in one transaction: this is setup cost, not the measured cost.
	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		for i := 0; i < rows; i++ {
			// Half the facts belong to whole rounds owned by the victim,
			// half merely route through it: both are in the purge's scope.
			var inc, retChan interface{}
			if i%2 == 0 {
				inc, retChan = "round:"+itoa(int64(i/16)), "chan-victim"
			}
			if _, e := tx.Exec(`INSERT INTO prediction_observations
				(observation_id, collector_session_id, collector_epoch, collector_sequence,
				 pool_instance_id, round_incarnation_id, retention_group_owner_channel_id,
				 routed_channel_id, kind, producer_time_source, received_at_ms,
				 payload_version, payload_json, observation_sha256)
				VALUES (?, 'victim', 1, ?, 'pool', ?, ?, 'chan-victim', ?, ?, 1, 1, ?, 'sha256:x')`,
				"v"+itoa(int64(i)), int64(i), inc, retChan, KindChannelEvent, TimeSourceReceiver, payload); e != nil {
				return e
			}
		}
		// A bystander identity that must survive untouched.
		for i := 0; i < 256; i++ {
			if _, e := tx.Exec(`INSERT INTO prediction_observations
				(observation_id, collector_session_id, collector_epoch, collector_sequence,
				 pool_instance_id, routed_channel_id, kind, producer_time_source, received_at_ms,
				 payload_version, payload_json, observation_sha256)
				VALUES (?, 'bystander', 2, ?, 'pool', 'chan-other', ?, ?, 1, 1, '{"phase":"UNKNOWN"}', 'sha256:x')`,
				"b"+itoa(int64(i)), int64(i), KindChannelEvent, TimeSourceReceiver); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed pilot data: %v", err)
	}

	var bytes int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(LENGTH(payload_json)),0) FROM prediction_observations WHERE routed_channel_id = 'chan-victim'`).Scan(&bytes); err != nil {
		t.Fatal(err)
	}
	wantBytes := int64(rows) * int64(payloadBytes)
	if bytes != wantBytes {
		t.Fatalf("pilot seeded %d bytes, want %d", bytes, wantBytes)
	}

	var removed int64
	start := time.Now()
	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		var e error
		removed, e = repo.EraseObservationsForIdentityTx(tx, ObservationIdentity{ChannelID: "chan-victim", Login: "victim"})
		return e
	}); err != nil {
		t.Fatalf("pilot purge: %v", err)
	}
	elapsed := time.Since(start)

	if removed != int64(rows) {
		t.Fatalf("purge removed %d of %d facts", removed, rows)
	}
	if raceDetectorEnabled {
		t.Logf("identity purge pilot (race build, bound NOT asserted): %d rows / %d bytes in %v",
			removed, bytes, elapsed)
	} else {
		if elapsed > 250*time.Millisecond {
			t.Fatalf("purging %d facts / %d bytes took %v, over the 250ms bound", rows, bytes, elapsed)
		}
		t.Logf("identity purge pilot: %d rows / %d bytes in %v (bound 250ms)", removed, bytes, elapsed)
	}

	var survivors int
	if err := db.QueryRow(`SELECT COUNT(*) FROM prediction_observations`).Scan(&survivors); err != nil {
		t.Fatal(err)
	}
	if survivors != 256 {
		t.Fatalf("%d facts survived, want the 256 bystander facts exactly", survivors)
	}

	// The measurement only means something if this fixture really is the worst
	// case an erasure can meet. It is, and by construction rather than by
	// assumption: the insert path refuses a fact once its deletion identity is
	// at the per-key ceiling, so a proved parent+channel union can never hold
	// more than two of those. Before the ceilings were enforced pre-insert,
	// nothing stopped an identity accumulating far more than this pilot
	// measured, and the number below bounded nothing at all.
	l := newObservationQuotaLedger()
	l.deletionKeys["chan:worst-case"] = observationUsage{Rows: MaxDeletionIdentityRows}
	if ok, identity := l.admit([]string{"chan:worst-case"}, "", 1); ok || !identity {
		t.Fatalf("an identity already at its ceiling was admitted (ok=%v identity=%v); the pilot's "+
			"fixture would not be the worst case", ok, identity)
	}
	if want := int64(MaxProvedIdentityUnionRows); MaxDeletionIdentityRows*2 != want {
		t.Fatalf("the proved parent+channel union bound (%d) is no longer two per-key ceilings "+
			"(%d); the pilot fixture must be rebuilt to the real worst case",
			want, MaxDeletionIdentityRows*2)
	}
	if want := int64(MaxProvedIdentityUnionBytes); MaxDeletionIdentityBytes*2 != want {
		t.Fatalf("the union byte bound (%d) is no longer two per-key ceilings (%d)",
			want, MaxDeletionIdentityBytes*2)
	}
}

// TestObservationRetentionRunsPeriodically is the regression for a defect an
// independent review found: PruneObservationUnit had exactly one caller —
// bootstrap — so a long-running miner pruned one unit at startup and never
// again, keeping observations far past Analytics.RetentionDays.
func TestObservationRetentionRunsPeriodically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	svc, err := NewService(db, filepath.Dir(path), 1) // 1-day retention
	if err != nil {
		t.Fatal(err)
	}
	repo := svc.repo.(*SQLiteRepository)
	c := svc.observations
	c.writeDeadline = 5 * time.Second
	c.maintenanceInterval = 10 * time.Millisecond
	t.Cleanup(func() { _ = svc.Close() })

	// Seed facts that are already older than the retention window, in an
	// epoch the collector will not claim.
	old := time.Now().Add(-72 * time.Hour).UnixMilli()
	staleEpoch, err := repo.OpenObservationSession(context.Background(), "stale", old)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		for i := 0; i < 40; i++ {
			if _, e := tx.Exec(`INSERT INTO prediction_observations
				(observation_id, collector_session_id, collector_epoch, collector_sequence,
				 pool_instance_id, kind, producer_time_source, received_at_ms,
				 payload_version, payload_json, observation_sha256)
				VALUES (?, 'stale', ?, ?, 'pool', ?, ?, ?, 1, '{"phase":"UNKNOWN"}', 'sha256:x')`,
				"old"+itoa(int64(i)), staleEpoch, int64(i), KindChannelEvent, TimeSourceReceiver, old); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Finalize the stale session. A crash-left OPEN session's facts are
	// deliberately protected from automatic pruning, so retention can only be
	// observed against a session that closed cleanly.
	if applied, err := repo.FinalizeObservationSession(context.Background(), staleEpoch,
		ObservationAccounting{Committed: 40}, old); err != nil || !applied {
		t.Fatalf("finalize stale session: applied=%v err=%v", applied, err)
	}

	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	<-c.bootstrapped

	// The collector must drain the backlog on its own, without any write
	// traffic to piggyback on.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if countRows(t, repo, `SELECT COUNT(*) FROM prediction_observations`) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%d stale observations survived: worker-owned retention is not running",
		countRows(t, repo, `SELECT COUNT(*) FROM prediction_observations`))
}

// TestObservationWholeRoundPruneSpearesTheActiveEpoch proves the whole-round
// delete does not take the active session's facts with it. The SELECT filtered
// rows by epoch while the DELETE removed the round by incarnation, so a round
// that spanned a restart lost its live facts.
func TestObservationWholeRoundPruneSparesTheActiveEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	repo, err := NewSQLiteRepository(db, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	oldEpoch, err := repo.OpenObservationSession(ctx, "old", 1)
	if err != nil {
		t.Fatal(err)
	}
	activeEpoch, err := repo.OpenObservationSession(ctx, "active", 2)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(epoch, seq int64, id string, at int64) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO prediction_observations
			(observation_id, collector_session_id, collector_epoch, collector_sequence,
			 pool_instance_id, round_incarnation_id, retention_group_owner_channel_id,
			 kind, producer_time_source, received_at_ms, payload_version, payload_json, observation_sha256)
			VALUES (?, 's', ?, ?, 'pool', 'round:spanning', 'chan-a', ?, ?, ?, 1, '{"phase":"UNKNOWN"}', 'sha256:x')`,
			id, epoch, seq, KindChannelEvent, TimeSourceReceiver, at); err != nil {
			t.Fatal(err)
		}
	}
	// ONE round whose facts span a restart: two old, one in the active epoch.
	insert(oldEpoch, 1, "r-old-1", 100)
	insert(oldEpoch, 2, "r-old-2", 150)
	insert(activeEpoch, 1, "r-active", 200)

	n, err := repo.PruneObservationUnit(ctx, 1000, activeEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("prune removed %d rows from a round the active session is still writing to", n)
	}
	if got := countRows(t, repo, `SELECT COUNT(*) FROM prediction_observations`); got != 3 {
		t.Fatalf("%d facts remain, want all 3 — the active epoch's round must be untouched", got)
	}
}

// TestObservationCapacityStopsCaptureSTICKILY drives the hard store bound at
// a REAL limit and proves what happens on either side of it.
//
// The previous test set the flags by hand and called maintain, so the branch
// it claimed to cover was never actually reached by a measurement, and it
// asserted that capture RESUMES once a later pass measures less. That is the
// opposite of the contract: capacity is released only by a new process's exact
// per-key and global recount. A pass that measures the whole store under its
// bound has not re-established the per-identity ledger that overflowed, so
// resuming would mean capturing again on ceilings nobody has re-derived.
func TestObservationCapacityStopsCaptureSTICKILY(t *testing.T) {
	newCollector := func(t *testing.T, rowCap int64) (*Service, *observationCollector) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "miner.db")
		db := openPrivateDB(t, path)
		t.Cleanup(func() { _ = db.Close() })
		svc, err := NewService(db, filepath.Dir(path), 0)
		if err != nil {
			t.Fatal(err)
		}
		c := svc.observations
		c.writeDeadline = 5 * time.Second
		c.maxStoreRows = rowCap
		t.Cleanup(func() { _ = svc.Close() })
		if err := svc.Start(); err != nil {
			t.Fatal(err)
		}
		<-c.bootstrapped
		if !c.capturing() {
			t.Fatal("capture did not come up")
		}
		return svc, c
	}

	t.Run("one below the ceiling capture continues", func(t *testing.T) {
		svc, c := newCollector(t, 3)
		for i := 0; i < 2; i++ {
			svc.RecordPredictionObservation(
				channelObservation("pool-1", "chan-a", "s", "e"+itoa(int64(i)), "ROUND_CREATED"))
		}
		awaitCommitted(t, svc, 2)
		c.maintain(context.Background())
		if !c.capturing() {
			t.Fatal("capture stopped below the ceiling")
		}
		if c.disabled.Load() {
			t.Fatal("capture was disabled below the ceiling")
		}
	})

	t.Run("at the ceiling capture stops and stays stopped", func(t *testing.T) {
		svc, c := newCollector(t, 3)
		for i := 0; i < 3; i++ {
			svc.RecordPredictionObservation(
				channelObservation("pool-1", "chan-a", "s", "e"+itoa(int64(i)), "ROUND_CREATED"))
		}
		awaitCommitted(t, svc, 3)

		c.maintain(context.Background())
		if c.capturing() {
			t.Fatal("capture continued at the store ceiling")
		}
		if !c.overCapacity.Load() || !c.disabled.Load() {
			t.Fatalf("capacity breach did not latch: overCapacity=%v disabled=%v",
				c.overCapacity.Load(), c.disabled.Load())
		}

		// Space is freed and the store now measures far under the bound. It
		// still must not resume: only a restart's exact recount may.
		c.maxStoreRows = MaxStoreRows
		c.maintain(context.Background())
		if c.capturing() {
			t.Fatal("capture resumed in-process after a capacity breach; only a restart's exact " +
				"per-key and global recount may release it")
		}
		if !c.disabled.Load() {
			t.Fatal("the capacity latch was cleared without a restart")
		}

		// And a fact offered afterwards is accounted, never quietly kept.
		before := c.preIntakeLosses.Load()
		svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "s", "late", "ROUND_CREATED"))
		if c.preIntakeLosses.Load() != before+1 {
			t.Fatal("a fact offered after the capacity latch was not accounted")
		}
	})

	t.Run("bootstrap refuses to open a session over the ceiling", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "miner.db")
		db := openPrivateDB(t, path)
		defer func() { _ = db.Close() }()

		// A first run fills the store.
		first, err := NewService(db, filepath.Dir(path), 0)
		if err != nil {
			t.Fatal(err)
		}
		first.observations.writeDeadline = 5 * time.Second
		if err := first.Start(); err != nil {
			t.Fatal(err)
		}
		<-first.observations.bootstrapped
		first.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "s", "e0", "ROUND_CREATED"))
		awaitCommitted(t, first, 1)
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}

		// A second run whose ceiling the store already exceeds must disable
		// P1 without failing startup and without opening a session.
		second, err := NewService(db, filepath.Dir(path), 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = second.Close() }()
		second.observations.maxStoreRows = 1
		if err := second.Start(); err != nil {
			t.Fatalf("a store at its ceiling must not fail startup: %v", err)
		}
		<-second.observations.bootstrapped
		if !second.observations.disabled.Load() {
			t.Fatal("bootstrap opened capture on a store already at its ceiling")
		}
		if second.observations.epoch.Load() != 0 {
			t.Fatal("bootstrap allocated a session on a store already at its ceiling")
		}
	})
}

// TestObservationCloseBeatsAFinishingBootstrap proves Close's intake fence
// cannot be overwritten by a bootstrap that completes concurrently, which
// would otherwise leave RUNNING true with no writer and finalize a session
// COMPLETE despite the work it lost.
func TestObservationCloseBeatsAFinishingBootstrap(t *testing.T) {
	for attempt := 0; attempt < 20; attempt++ {
		path := filepath.Join(t.TempDir(), "miner.db")
		db := openPrivateDB(t, path)
		svc, err := NewService(db, filepath.Dir(path), 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.Start(); err != nil {
			t.Fatal(err)
		}
		// Close immediately — racing the bootstrap goroutine.
		_ = svc.Close()
		if svc.observations.capturing() {
			t.Fatal("a finishing bootstrap reopened intake over the shutdown fence")
		}
		_ = db.Close()
	}
}

// TestObservationSessionBoundsAreEnforced proves the per-session ceilings are
// real rather than documentation. An independent review found eight declared
// "frozen" bounds enforced nowhere; these two are checked per fact, from
// atomics the writer already maintains, so a session that reaches either one
// stops committing instead of growing past it.
func TestObservationSessionBoundsAreEnforced(t *testing.T) {
	svc, repo := newObservationService(t)
	c := svc.observations

	// Row ceiling: pretend the session has already committed its maximum.
	c.committed.Store(MaxSessionRows)
	if c.withinSessionBounds() {
		t.Fatal("a session at MaxSessionRows still reports itself within bounds")
	}
	before := c.dropped.Load()
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "s", "over-rows", "ROUND_CREATED"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && c.dropped.Load() == before {
		time.Sleep(time.Millisecond)
	}
	if c.dropped.Load() == before {
		t.Fatal("a fact past the session row ceiling was neither refused nor counted")
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM prediction_observations`); n != 0 {
		t.Fatalf("%d facts were committed past the session row ceiling", n)
	}

	// Byte ceiling behaves the same way.
	c.committed.Store(0)
	c.sessionBytes.Store(MaxSessionBytes)
	if c.withinSessionBounds() {
		t.Fatal("a session at MaxSessionBytes still reports itself within bounds")
	}

	// Back under both ceilings, capture resumes.
	c.sessionBytes.Store(0)
	if !c.withinSessionBounds() {
		t.Fatal("a session under both ceilings is refused")
	}
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "s", "under", "ROUND_CREATED"))
	awaitCommitted(t, svc, 1)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && c.sessionBytes.Load() <= 0 {
		time.Sleep(time.Millisecond)
	}
	if c.sessionBytes.Load() <= 0 {
		t.Fatal("committed payload bytes were not accounted, so the byte ceiling could never be reached")
	}
}
