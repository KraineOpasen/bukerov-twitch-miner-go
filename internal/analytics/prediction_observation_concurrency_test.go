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

	// The claim is held until this test says so, not for a fixed duration: a
	// sleep would decide by scheduling whether the assertions below run while
	// the claim is still held, which is the only state they are about.
	claimed := make(chan struct{})
	releaseClaim := make(chan struct{})
	go func() {
		release := p.Claim()
		close(claimed)
		defer release()
		<-releaseClaim
	}()
	defer close(releaseClaim)

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
	// Every producer signals that it has actually offered a fact, and the
	// window closes once all four have. A fixed sleep decided by scheduling
	// how much work -- possibly none -- had happened before Close, so on a
	// loaded machine this could assert the shutdown path against an idle
	// collector and still pass.
	stop := make(chan struct{})
	offered := make(chan struct{}, 4)
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
				if i == 0 {
					offered <- struct{}{}
				}
			}
		}(w)
	}
	for i := 0; i < 4; i++ {
		select {
		case <-offered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of 4 producers reached the collector", i)
		}
	}
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
		removed, e = repo.EraseObservationsForIdentityTx(context.Background(), tx, ObservationIdentity{ChannelID: "chan-victim", Login: "victim"})
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
		c.maxStoreRows.Store(rowCap)
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
		//
		// The ceiling is an atomic precisely so this can be raised while the
		// collector's own maintenance goroutine is reading it; a plain field
		// here was a data race the detector would report only when a tick
		// landed in the window.
		c.maxStoreRows.Store(MaxStoreRows)
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
		second.observations.maxStoreRows.Store(1)
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

	// The SESSION ceiling is the third term of the same check and the only one
	// that had no oracle: deleting it from both the bootstrap refusal and the
	// runtime latch left the whole package green. It is also the term that
	// bounds how many rows a store may accumulate that nothing else can
	// reclaim, so it is the one a store hits after a run of unclean shutdowns.
	t.Run("bootstrap refuses to open a session over the SESSION ceiling", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "miner.db")
		db := openPrivateDB(t, path)
		defer func() { _ = db.Close() }()

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

		// One session row exists, and its facts keep it from being swept.
		// One below the ceiling still opens; at the ceiling refuses.
		under, err := NewService(db, filepath.Dir(path), 0)
		if err != nil {
			t.Fatal(err)
		}
		under.observations.writeDeadline = 5 * time.Second
		under.observations.maxStoreSessions.Store(2)
		if err := under.Start(); err != nil {
			t.Fatal(err)
		}
		<-under.observations.bootstrapped
		if under.observations.disabled.Load() || under.observations.epoch.Load() == 0 {
			t.Fatal("a store one session under its ceiling refused to open a session")
		}
		if err := under.Close(); err != nil {
			t.Fatal(err)
		}

		at, err := NewService(db, filepath.Dir(path), 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = at.Close() }()
		at.observations.maxStoreSessions.Store(2)
		if err := at.Start(); err != nil {
			t.Fatalf("a store at its session ceiling must not fail startup: %v", err)
		}
		<-at.observations.bootstrapped
		if !at.observations.disabled.Load() {
			t.Fatal("bootstrap opened capture on a store already at its SESSION ceiling")
		}
		if at.observations.epoch.Load() != 0 {
			t.Fatal("bootstrap allocated a session on a store already at its SESSION ceiling")
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

	// One payload's worth of bytes, the unit both ceilings are tested at.
	payload := int64(len(payloadJSONOf(mustSanitize(t,
		channelObservation("pool-1", "chan-a", "s", "sizer", "ROUND_CREATED")))))
	if payload <= 0 {
		t.Fatal("the fixture renders no payload; the byte cases below would prove nothing")
	}

	// Row ceiling: one below admits, at the ceiling refuses. A row is always
	// one row, so the pair is exact.
	c.committed.Store(MaxSessionRows - 1)
	if !c.withinSessionBounds(payload) {
		t.Fatal("a session one row under MaxSessionRows refuses the fact that would reach it")
	}
	c.committed.Store(MaxSessionRows)
	if c.withinSessionBounds(payload) {
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

	// Byte ceiling, as a would-exceed pair. The fact that lands EXACTLY on
	// the ceiling is admitted; the one that would take the session a single
	// byte past it is refused. Asking only whether the session had already
	// exceeded the ceiling let a whole further payload land beyond a bound
	// that is supposed to be a promise.
	c.committed.Store(0)
	c.sessionBytes.Store(MaxSessionBytes - payload)
	if !c.withinSessionBounds(payload) {
		t.Fatal("the fact that lands exactly on MaxSessionBytes was refused")
	}
	c.sessionBytes.Store(MaxSessionBytes - payload + 1)
	if c.withinSessionBounds(payload) {
		t.Fatalf("a fact that would take the session one byte past MaxSessionBytes was admitted; "+
			"the ceiling may be overshot by a whole payload (%d bytes)", payload)
	}
	c.sessionBytes.Store(MaxSessionBytes)
	if c.withinSessionBounds(payload) {
		t.Fatal("a session at MaxSessionBytes still reports itself within bounds")
	}

	// Back under both ceilings, capture resumes.
	c.sessionBytes.Store(0)
	if !c.withinSessionBounds(payload) {
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

// TestAnErasureThatLandsWhileTheWriteWaitsForTheLeaseStillHolds closes a test
// gap an independent acceptance review found by mutation: deleting the entire
// re-check that write performs AFTER acquiring the gate lease left the whole
// analytics suite green, though its own comment calls it "the difference
// between an erasure that holds and one that only looks like it does".
//
// The window is real and is not small. Acquiring the lease waits for whatever
// business writer currently holds the single connection, and an operator's
// erasure runs on one of exactly those paths. A fact admitted by the checks
// before the lease, and committed after the erasure released it, is an
// observation of an erased identity written after the erasure — the one
// outcome the fence exists to prevent.
//
// Driving that requires the erasure to land while the write is ALREADY past
// its pre-lease checks, so this test proves the writer is blocked before it
// erases: the only blocking point in write is the lease acquisition, so a
// writer that has not returned is a writer holding the pre-lease verdict. The
// fence alone is armed and the generation is deliberately left untouched, so
// the generation check cannot be what refuses the fact either.
func TestAnErasureThatLandsWhileTheWriteWaitsForTheLeaseStillHolds(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()
	c := svc.observations

	fact := channelObservation("pool-1", "chan-doomed", "doomed", "event-1", "ROUND_CREATED")
	fact.sequence = c.sequence.Add(1)
	fact.generation = c.generation.Load()

	// It passes every check write makes BEFORE the lease.
	sanitized, ok := sanitizeObservation(fact, c.now().UnixMilli())
	if !ok {
		t.Fatal("the fixture does not sanitize; it would be dropped before the lease")
	}
	if c.disabled.Load() || c.isFenced(sanitized) {
		t.Fatal("the fixture is already refused before the lease; this test would prove nothing")
	}

	// A business writer takes the gate and holds it.
	claimed := make(chan struct{})
	releaseClaim := make(chan struct{})
	go func() {
		release := c.gate.Claim()
		close(claimed)
		defer release()
		<-releaseClaim
	}()
	<-claimed

	written := make(chan struct{})
	go func() {
		defer close(written)
		c.write(ctx, fact)
	}()

	// The writer must be INSIDE the lease wait before the erasure lands,
	// otherwise the pre-lease check would be what refuses the fact and this
	// test would pass for the wrong reason. Everything write does before the
	// lease is non-blocking, so a writer that has not returned is blocked
	// exactly there.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-written:
		t.Fatal("the write completed while a business claim was held; it never waited for the " +
			"lease, so the window this test is about was never entered")
	default:
	}

	// Only the fence is armed. The generation is left alone on purpose: if it
	// advanced, the generation half of the re-check could refuse the fact and
	// the fence half would stay unproven.
	c.fence("chan-doomed", "doomed")
	repo.Tombstone("doomed")

	close(releaseClaim)
	select {
	case <-written:
	case <-time.After(5 * time.Second):
		t.Fatal("the write never completed after the claim was released")
	}

	got, err := repo.ObservationsBySession(ctx, c.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range got {
		if rec.EventID == "event-1" {
			t.Fatalf("an observation of an erased identity was written AFTER the erasure: %+v. "+
				"Every check before the lease passed, so only the re-check after it can "+
				"refuse this fact", rec)
		}
	}
}

// TestAReservedPositionThatNeverReachesTheWriterIsSettledAsDropped closes a
// second gap the same review found by mutation: removing accounting's gap
// settlement left the suite green.
//
// A position is reserved when a producer captures a fact and is only later
// either committed or dropped. A producer preempted in between leaves a
// position that reached neither counter, and reporting that verbatim finalizes
// a session whose own numbers do not add up: committed + dropped below
// last_assigned_sequence is exactly the counter-form violation the reader
// classifies as INTEGRITY_ERROR. Settling the shortfall as dropped is both
// true — the fact was captured and never stored — and what keeps the session
// honestly INCOMPLETE instead of self-contradicting.
func TestAReservedPositionThatNeverReachesTheWriterIsSettledAsDropped(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()
	c := svc.observations

	svc.RecordPredictionObservation(
		channelObservation("pool-1", "chan-a", "streamer-a", "event-1", "ROUND_CREATED"))
	awaitCommitted(t, svc, 1)

	// A producer reserves its position and is then preempted: the fact never
	// reaches the writer, so neither counter ever moves for it.
	c.sequence.Add(1)

	acc := c.accounting()
	if acc.LastAssignedSequence != 2 {
		t.Fatalf("last assigned = %d, want 2", acc.LastAssignedSequence)
	}
	if acc.Committed+acc.Dropped != acc.LastAssignedSequence {
		t.Fatalf("accounting = %d committed + %d dropped against %d reserved: a session whose "+
			"counters do not account for every position it handed out reads INTEGRITY_ERROR, "+
			"and nothing may be concluded from it at all",
			acc.Committed, acc.Dropped, acc.LastAssignedSequence)
	}
	if acc.Whole() {
		t.Fatal("a session that lost a captured fact reports itself whole")
	}

	// And the finalized session really does read coherently.
	c.Close()
	reading, found, err := repo.ReadObservationSession(ctx, c.epoch.Load())
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if reading.Reading == ReadingIntegrityError {
		t.Fatalf("the finalized session reads INTEGRITY_ERROR (%s)", reading.Detail)
	}
	if reading.Session.CloseState != SessionIncomplete {
		t.Fatalf("close state = %q, want %q", reading.Session.CloseState, SessionIncomplete)
	}
}

// TestABusinessWriterPreemptsTheCollectorsOwnMaintenance closes a test gap an
// independent acceptance review found by mutation: replacing leased's body with
// a bare `return fn(leaseCtx)` — removing the gate from the retention pass, the
// store measurement, the session open, the bootstrap recount, the startup
// reconciliation and the finalize — left the whole analytics suite green.
//
// That gate is the entire reason a P1 maintenance transaction is allowed to
// hold the single shared connection for up to its budget: a business writer can
// take it back. Proven only at the txPriority unit level and for the per-fact
// write path, the collector's OWN transactions could have stopped participating
// without anything noticing.
//
// This asserts the behaviour end to end through leased: a maintenance
// transaction in flight has its context cancelled by a business claim, and the
// business writer gets the connection rather than waiting out the budget.
func TestABusinessWriterPreemptsTheCollectorsOwnMaintenance(t *testing.T) {
	svc, repo := newObservationService(t)
	c := svc.observations

	// The budget is deliberately far larger than production's, and far larger
	// than the window this test allows. A lease that ends because its budget
	// expired proves nothing about the gate — the two causes are otherwise
	// indistinguishable — so the budget is put out of reach and only a
	// business claim can explain a prompt cancellation.
	const budget = 30 * time.Second
	const promptly = 3 * time.Second

	inside := make(chan struct{})
	cancelled := make(chan time.Duration, 1)
	done := make(chan error, 1)
	var heldFrom time.Time
	go func() {
		done <- c.leased(context.Background(), budget,
			func(lease context.Context) error {
				heldFrom = time.Now()
				close(inside)
				select {
				case <-lease.Done():
					cancelled <- time.Since(heldFrom)
				case <-time.After(budget):
				}
				return nil
			})
	}()
	<-inside

	// A business write on a gated repository path. It must not wait out the
	// maintenance budget.
	claimReturned := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_ = repo.RecordPoints("streamer-a", 100, "WATCH")
		claimReturned <- time.Since(start)
	}()

	select {
	case held := <-cancelled:
		if held >= promptly {
			t.Fatalf("the maintenance transaction was cancelled only after %v; a business "+
				"claim must preempt it, not the budget eventually expiring", held)
		}
	case <-time.After(promptly):
		t.Fatal("a business writer's claim did not cancel the collector's in-flight maintenance " +
			"transaction: the collector's own transactions are not participating in the gate, " +
			"so a business write can be stuck behind one for its whole budget")
	}
	select {
	case <-claimReturned:
	case <-time.After(promptly):
		t.Fatal("the business write never completed after preempting the maintenance transaction")
	}
	if err := <-done; err != nil {
		t.Fatalf("the preempted maintenance transaction returned %v", err)
	}
}

// TestAProducerDescheduledAcrossAReAddCannotResurrectTheErasedLife is the
// regression for a defect an independent acceptance review found in the
// erasure boundary, and it is the one interleaving the boundary's own
// reasoning got wrong.
//
// A fact carries two values that say when it was captured: its causal position
// and the capture generation. They were NOT taken together — offer read the
// generation first and reserved the position second — so a producer could be
// descheduled between them. Park it there across a whole erase-and-re-add and
// it emerges with a generation loaded AFTER the erasure (so the generation
// check passes), no live fence left to meet, and a position taken AFTER the
// re-add raised the watermark (so the causal check passes too). Every check
// agrees, and a fact of the ERASED life is written against the re-added
// streamer's row.
//
// Reserving the position first closes it, because every watermark is a value
// of that same counter: a position taken before an erasure can never be above
// the boundary that erasure leaves.
//
// afterReserve occupies exactly that gap, so the race is a fixture rather than
// a hope.
func TestAProducerDescheduledAcrossAReAddCannotResurrectTheErasedLife(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()
	c := svc.observations

	// The erasure has already happened when the producer reaches offer: the
	// generation is bumped, the fence armed, the rows purged. A producer
	// loading the generation now loads the POST-erasure one, so the
	// generation check cannot be what refuses this fact.
	c.invalidateGeneration()
	c.fence("chan-doomed", "doomed")
	repo.Tombstone("doomed")

	// The re-add lands while the producer is inside offer, between taking its
	// position and reading its generation.
	var once sync.Once
	c.afterReserve = func() {
		once.Do(func() { repo.Reinstate("doomed") })
	}
	t.Cleanup(func() { c.afterReserve = nil })

	c.offer(channelObservation("pool-1", "chan-doomed", "doomed", "old-life", "ROUND_CREATED"))

	// Wait for the writer to REACH a verdict, whichever way it went, so the
	// assertion below reports what actually happened to the fact rather than
	// timing out waiting for the verdict this test wants.
	deadline := time.Now().Add(5 * time.Second)
	for c.committed.Load()+c.dropped.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the writer never reached a verdict on the offered fact")
		}
		time.Sleep(time.Millisecond)
	}

	stored, err := repo.ObservationsBySession(ctx, c.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range stored {
		if rec.EventID == "old-life" {
			t.Fatalf("a fact of the erased life was written against the re-added streamer: %+v. "+
				"The producer was descheduled across the re-add, so it carries the erasure's "+
				"own generation and meets no live fence — only a position taken BEFORE the "+
				"re-add raised the watermark can refuse it", rec)
		}
	}

	if c.dropped.Load() != 1 {
		t.Fatalf("the fact was not counted as a drop: committed=%d dropped=%d",
			c.committed.Load(), c.dropped.Load())
	}

	// And the re-added streamer's own new life is still fully observable.
	c.afterReserve = nil
	svc.RecordPredictionObservation(
		channelObservation("pool-1", "chan-doomed", "doomed", "new-life", "ROUND_CREATED"))
	awaitCommitted(t, svc, 1)
}
