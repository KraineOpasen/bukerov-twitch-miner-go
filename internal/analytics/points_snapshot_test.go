package analytics

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database/dbtest"
)

// snapshotRig opens a private database through the hooked driver so a test
// can act at the statement boundaries INSIDE PointsSnapshotBetween's read
// transaction, plus a plain second handle on the same file that plays a
// concurrent writer with its own commit point.
type snapshotRig struct {
	t        *testing.T
	repo     *SQLiteRepository
	db       *database.DB
	writer   *SQLiteRepository
	writerDB *database.DB
	login    string

	mu       sync.Mutex
	armed    bool
	events   []dbtest.Event
	onRead   func(sql string) // runs before every read statement while armed
	onCommit func()           // runs before the transaction commits while armed
}

func newSnapshotRig(t *testing.T) *snapshotRig {
	t.Helper()
	rig := &snapshotRig{t: t, login: uniqueName("snap")}
	dir := t.TempDir()
	path := filepath.Join(dir, "miner.db")
	db := dbtest.OpenHooked(path, rig.observe)
	rig.db = db
	t.Cleanup(func() { _ = db.Close() })
	repo, err := NewSQLiteRepository(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	rig.repo = repo
	plain, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	plain.SetMaxOpenConns(1)
	rig.writerDB = &database.DB{DB: plain}
	t.Cleanup(func() { _ = rig.writerDB.Close() })
	if rig.writer, err = NewSQLiteRepository(rig.writerDB, dir); err != nil {
		t.Fatal(err)
	}
	return rig
}

func (r *snapshotRig) observe(e dbtest.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.armed {
		return
	}
	r.events = append(r.events, e)
	if e.Kind == "query" && r.onRead != nil {
		r.onRead(e.SQL)
	}
	if e.Kind == "commit" && r.onCommit != nil {
		r.onCommit()
	}
}

func (r *snapshotRig) arm(onRead func(sql string)) {
	r.mu.Lock()
	r.armed, r.events, r.onRead, r.onCommit = true, nil, onRead, nil
	r.mu.Unlock()
}

func (r *snapshotRig) disarm() []dbtest.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.armed = false
	return r.events
}

// commitThroughWriter tries to commit ev on the writer's own connection and
// returns the error, rolling the writer's SQLite transaction back when the
// commit was refused (SQLITE_BUSY leaves it open).
func (r *snapshotRig) commitThroughWriter(ev PointEvent) error {
	rec, err := r.writer.RecordPointEvent(r.login, ev, ev.BalanceAfter, streakAnnotation(ev.TotalPoints))
	if err == nil && !rec {
		err = errors.New("duplicate")
	}
	if err != nil {
		_, _ = r.writerDB.Exec("ROLLBACK")
	}
	return err
}

// ctxMarker keys a value the tests attach to the caller's context, so an
// event proves it ran under THAT context (or one derived from it), not
// merely under some cancellable context of the repository's own making.
type ctxMarker struct{}

// carriesMarker reports whether the event's context descends from the
// caller's context marked with token.
func carriesMarker(e dbtest.Event, token string) bool {
	return e.Ctx != nil && e.Ctx.Value(ctxMarker{}) == token
}

func kinds(events []dbtest.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		if e.Kind == "query" {
			switch {
			case strings.Contains(e.SQL, "FROM streamers"):
				out = append(out, "streamers")
			case strings.Contains(e.SQL, "FROM points p"):
				out = append(out, "points")
			case strings.Contains(e.SQL, "FROM annotations"):
				out = append(out, "annotations")
			case strings.Contains(e.SQL, "FROM point_events"):
				out = append(out, "point_events")
			case strings.Contains(e.SQL, "FROM prediction_bets"):
				out = append(out, "prediction_bets")
			default:
				out = append(out, "query")
			}
			continue
		}
		out = append(out, e.Kind)
	}
	return out
}

// TestPointsSnapshotBetweenIsOneReadTransaction pins the snapshot boundary at
// the repository: every read runs between one BEGIN and one COMMIT, a commit
// on another connection is refused at every split point while the
// transaction is open, and it lands as soon as the method has returned — the
// transaction is released before the caller sees the value. Every statement
// of the snapshot carries the caller's context, so an abandoned request can
// interrupt the read it is on.
func TestPointsSnapshotBetweenIsOneReadTransaction(t *testing.T) {
	rig := newSnapshotRig(t)
	base := time.Now().Add(-time.Hour)
	first := streakEvent("sha256:"+rig.login+"-1", base, 1450)
	if rec, err := rig.repo.RecordPointEvent(rig.login, first, 1450, streakAnnotation(450)); err != nil || !rec {
		t.Fatalf("seed: recorded=%v err=%v", rec, err)
	}
	if err := rig.repo.RecordBet(BetRecord{Streamer: rig.login, EventID: "bet-" + rig.login, Timestamp: base.Add(time.Minute).UnixMilli(), Strategy: "SMART", ResultType: "WIN", Placed: 100, Won: 150, Gained: 50, Odds: 1.5}); err != nil {
		t.Fatal(err)
	}
	second := streakEvent("sha256:"+rig.login+"-2", base.Add(2*time.Minute), 1900)

	var attempts []error
	rig.arm(func(sql string) {
		// Split points: every read after the samples read.
		if strings.Contains(sql, "FROM annotations") || (strings.Contains(sql, "FROM point_events") && !strings.Contains(sql, "FROM points p")) || strings.Contains(sql, "FROM prediction_bets") {
			attempts = append(attempts, rig.commitThroughWriter(second))
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	token := "caller-" + rig.login
	ctx = context.WithValue(ctx, ctxMarker{}, token)
	snap, err := rig.repo.PointsSnapshotBetween(ctx, rig.login, time.Time{}, time.Time{}, 0, true)
	events := rig.disarm()
	if err != nil {
		t.Fatal(err)
	}

	got := kinds(events)
	want := []string{"begin", "streamers", "points", "annotations", "point_events", "prediction_bets", "commit"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statement sequence = %v, want one transaction around every read: %v", got, want)
	}
	for _, e := range events {
		if (e.Kind == "begin" || e.Kind == "query") && (!e.Cancellable || !carriesMarker(e, token)) {
			t.Fatalf("%s ran without the caller's context (cancellable=%v, caller's lineage=%v): %q", e.Kind, e.Cancellable, carriesMarker(e, token), e.SQL)
		}
	}
	if len(attempts) != 3 {
		t.Fatalf("writer attempts = %d, want one per split point", len(attempts))
	}
	for i, err := range attempts {
		if err == nil {
			t.Fatalf("attempt %d committed while the snapshot transaction was open", i)
		}
	}
	if len(snap.Samples) != 1 || !snap.Samples[0].Exact || len(snap.Annotations) != 1 || snap.Exact.Events != 1 || len(snap.Bets) != 1 {
		t.Fatalf("snapshot = samples %d / annotations %d / events %d / bets %d, want the seeded state 1/1/1/1", len(snap.Samples), len(snap.Annotations), snap.Exact.Events, len(snap.Bets))
	}
	// Released: the same writer commits at once after the return.
	if err := rig.commitThroughWriter(second); err != nil {
		t.Fatalf("writer could not commit after the snapshot returned: %v", err)
	}
	// The other direction, on a proven-complete window: under a caller
	// without a cancellable context every statement is reported as neither
	// cancellable nor of the caller's lineage — the probe is pinned both
	// ways, and the snapshot adds no deadline or cancellation of its own:
	// the request context is its only bound.
	rig.arm(nil)
	after, err := rig.repo.PointsSnapshotBetween(context.Background(), rig.login, time.Time{}, time.Time{}, 0, true)
	plain := rig.disarm()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Samples) != 2 || len(after.Annotations) != 2 || after.Exact.Events != 2 {
		t.Fatalf("after the commit: samples %d / annotations %d / events %d, want 2/2/2", len(after.Samples), len(after.Annotations), after.Exact.Events)
	}
	if got := kinds(plain); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("statement sequence under context.Background() = %v, want %v", got, want)
	}
	for _, e := range plain {
		if (e.Kind == "begin" || e.Kind == "query") && (e.Cancellable || carriesMarker(e, token)) {
			t.Fatalf("%s under context.Background() ran under a cancellable context (the snapshot must not add a bound of its own) or a foreign lineage (cancellable=%v, lineage=%v): %q", e.Kind, e.Cancellable, carriesMarker(e, token), e.SQL)
		}
	}
}

// TestPointsSnapshotBetweenMatchesStandaloneReads: the snapshot returns
// exactly what the three standalone reads return on a quiescent database,
// honouring the sample cap and the window on every component, with bets only
// when asked for.
func TestPointsSnapshotBetweenMatchesStandaloneReads(t *testing.T) {
	r := newTestRepo(t)
	s := uniqueName("snap-parity")
	base := time.Now().Add(-2 * time.Hour)
	for i, amount := range []int{450, 12, 450} {
		ev := PointEvent{EventID: "sha256:" + s + "-" + string(rune('a'+i)), Timestamp: base.Add(time.Duration(i) * time.Minute).UnixMilli(), ReasonCode: "WATCH_STREAK", TotalPoints: amount, BalanceAfter: 1000 + amount*(i+1), BalanceKnown: true}
		if rec, err := r.RecordPointEvent(s, ev, ev.BalanceAfter, streakAnnotation(amount)); err != nil || !rec {
			t.Fatalf("seed %d: recorded=%v err=%v", i, rec, err)
		}
	}
	if err := r.RecordPoints(s, 999, "Spent"); err != nil {
		t.Fatal(err)
	}
	start, end := base.Add(30*time.Second), base.Add(90*time.Second)

	snap, err := r.PointsSnapshotBetween(context.Background(), s, start, end, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	samples, _ := r.GetPointSamples(s, start, end, 0)
	anns, _ := r.GetAnnotationRecords(s, start, end)
	exact, _ := r.ExactEarningsBetween(s, start, end)
	if len(snap.Samples) != len(samples) || len(samples) != 1 || snap.Samples[0] != samples[0] {
		t.Fatalf("snapshot samples = %+v, standalone = %+v", snap.Samples, samples)
	}
	if len(snap.Annotations) != len(anns) || len(anns) != 1 || snap.Annotations[0] != anns[0] {
		t.Fatalf("snapshot annotations = %+v, standalone = %+v", snap.Annotations, anns)
	}
	if snap.Exact.Events != exact.Events || exact.Events != 1 || len(snap.Exact.Breakdown) != 1 || snap.Exact.Breakdown[0] != exact.Breakdown[0] {
		t.Fatalf("snapshot exact = %+v, standalone = %+v", snap.Exact, exact)
	}
	if snap.Bets != nil {
		t.Fatalf("bets were read without being asked for: %+v", snap.Bets)
	}

	// The cap bounds the samples only; the exact aggregate is never capped.
	capped, err := r.PointsSnapshotBetween(context.Background(), s, time.Time{}, time.Time{}, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped.Samples) != 2 || capped.Exact.Events != 3 || len(capped.Annotations) != 3 {
		t.Fatalf("capped snapshot = samples %d / events %d / annotations %d, want 2 / 3 / 3", len(capped.Samples), capped.Exact.Events, len(capped.Annotations))
	}

	// An unknown streamer is an empty snapshot, not an error.
	empty, err := r.PointsSnapshotBetween(context.Background(), uniqueName("snap-nobody"), time.Time{}, time.Time{}, 0, true)
	if err != nil || empty.Samples != nil || empty.Annotations != nil || empty.Exact.Events != 0 || empty.Bets != nil {
		t.Fatalf("unknown streamer = %+v err=%v, want empty", empty, err)
	}
}

// TestPointsSnapshotBetweenAfterCloseIsRefusedTyped: the snapshot takes the
// same close barrier as every transactional path.
func TestPointsSnapshotBetweenAfterCloseIsRefusedTyped(t *testing.T) {
	r, db := openPrivateRepo(t, t.TempDir())
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PointsSnapshotBetween(context.Background(), "snap-closed", time.Time{}, time.Time{}, 0, true); !errors.Is(err, database.ErrClosed) {
		t.Fatalf("snapshot after close: err=%v, want database.ErrClosed", err)
	}
}

// TestPointsSnapshotBetweenCompletesBeforeClose: a Close that arrives while a
// snapshot is reading waits for it. The snapshot still commits and returns
// the complete value, Close has not returned by the time the read
// transaction commits, and the handle is closed for the next caller. The
// commit-time check is one-sided — it can only ever fail on a regression,
// never on correct code — and the barrier itself is pinned structurally in
// the database package (TestWithTxHoldsCloseUntilCommit).
func TestPointsSnapshotBetweenCompletesBeforeClose(t *testing.T) {
	rig := newSnapshotRig(t)
	base := time.Now().Add(-time.Hour)
	if rec, err := rig.repo.RecordPointEvent(rig.login, streakEvent("sha256:"+rig.login+"-1", base, 1450), 1450, streakAnnotation(450)); err != nil || !rec {
		t.Fatalf("seed: recorded=%v err=%v", rec, err)
	}

	closeCalled := make(chan struct{})
	closeDone := make(chan error, 1)
	var launch sync.Once
	rig.arm(func(sql string) {
		if !strings.Contains(sql, "FROM annotations") {
			return
		}
		// Close arrives between two reads of the same snapshot.
		launch.Do(func() {
			go func() {
				close(closeCalled)
				closeDone <- rig.db.Close()
			}()
			<-closeCalled
		})
	})
	var closedBeforeCommit bool
	rig.mu.Lock()
	rig.onCommit = func() {
		select {
		case err := <-closeDone:
			closedBeforeCommit = true
			closeDone <- err
		default:
		}
	}
	rig.mu.Unlock()
	snap, err := rig.repo.PointsSnapshotBetween(context.Background(), rig.login, time.Time{}, time.Time{}, 0, true)
	events := rig.disarm()
	if err != nil {
		t.Fatalf("snapshot with Close in flight: %v", err)
	}
	if closedBeforeCommit {
		t.Fatal("Close returned before the snapshot transaction committed")
	}
	if got := kinds(events); got[len(got)-1] != "commit" {
		t.Fatalf("statement sequence = %v, want the snapshot to commit", got)
	}
	if len(snap.Samples) != 1 || len(snap.Annotations) != 1 || snap.Exact.Events != 1 {
		t.Fatalf("snapshot = samples %d / annotations %d / events %d, want the complete seeded state 1/1/1", len(snap.Samples), len(snap.Annotations), snap.Exact.Events)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close after the snapshot: %v", err)
	}
	if _, err := rig.repo.PointsSnapshotBetween(context.Background(), rig.login, time.Time{}, time.Time{}, 0, true); !errors.Is(err, database.ErrClosed) {
		t.Fatalf("snapshot after Close: err=%v, want database.ErrClosed", err)
	}
}

// TestPointsSnapshotBetweenReleasesConnectionWhenCancelledMidRead: a request
// abandoned between two reads of its snapshot does not finish the read. The
// statement it is on runs under the caller's own context (pinned through
// the driver), so it receives the cancellation and refuses to run; the
// method returns the context's error and a zero snapshot — never a partial
// one with a nil error — and the single connection is released, so the
// next write and read on the same handle proceed.
func TestPointsSnapshotBetweenReleasesConnectionWhenCancelledMidRead(t *testing.T) {
	rig := newSnapshotRig(t)
	base := time.Now().Add(-time.Hour)
	if rec, err := rig.repo.RecordPointEvent(rig.login, streakEvent("sha256:"+rig.login+"-1", base, 1450), 1450, streakAnnotation(450)); err != nil || !rec {
		t.Fatalf("seed: recorded=%v err=%v", rec, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	token := "caller-" + rig.login
	ctx = context.WithValue(ctx, ctxMarker{}, token)
	rig.arm(func(sql string) {
		if strings.Contains(sql, "FROM annotations") {
			cancel() // the client goes away between the samples and the annotations
		}
	})
	snap, err := rig.repo.PointsSnapshotBetween(ctx, rig.login, time.Time{}, time.Time{}, 0, true)
	events := rig.disarm()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("snapshot cancelled mid-read: err=%v snapshot=%+v, want context.Canceled", err, snap)
	}
	var sawAnnotations bool
	for _, e := range events {
		if e.Kind == "query" && strings.Contains(e.SQL, "FROM annotations") {
			sawAnnotations = true
			if !carriesMarker(e, token) {
				t.Fatalf("the annotations read ran without the caller's context: %+v", e)
			}
		}
	}
	if !sawAnnotations {
		t.Fatalf("the annotations read was never issued (events=%v)", kinds(events))
	}
	if snap.Samples != nil || snap.Annotations != nil || snap.Exact.Events != 0 || snap.Bets != nil {
		t.Fatalf("cancelled snapshot = %+v, want the zero value", snap)
	}
	// Released: the same handle writes and reads at once.
	if rec, err := rig.repo.RecordPointEvent(rig.login, streakEvent("sha256:"+rig.login+"-2", base.Add(time.Minute), 1900), 1900, streakAnnotation(450)); err != nil || !rec {
		t.Fatalf("write after a cancelled snapshot: recorded=%v err=%v", rec, err)
	}
	after, err := rig.repo.PointsSnapshotBetween(context.Background(), rig.login, time.Time{}, time.Time{}, 0, true)
	if err != nil || len(after.Samples) != 2 || len(after.Annotations) != 2 || after.Exact.Events != 2 {
		t.Fatalf("snapshot after the cancelled one = samples %d / annotations %d / events %d err=%v, want 2/2/2", len(after.Samples), len(after.Annotations), after.Exact.Events, err)
	}
}

// TestPointsSnapshotBetweenHonoursCancelledContext: a request abandoned by
// its client does not run a read nobody will receive — the snapshot returns
// the context's error and holds nothing.
func TestPointsSnapshotBetweenHonoursCancelledContext(t *testing.T) {
	r := newTestRepo(t)
	s := uniqueName("snap-cancel")
	if rec, err := r.RecordPointEvent(s, streakEvent("sha256:"+s, time.Now(), 1450), 1450, nil); err != nil || !rec {
		t.Fatalf("seed: recorded=%v err=%v", rec, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.PointsSnapshotBetween(ctx, s, time.Time{}, time.Time{}, 0, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled snapshot: err=%v, want context.Canceled", err)
	}
	// Nothing is held: the next write and read proceed at once.
	if rec, err := r.RecordPointEvent(s, streakEvent("sha256:"+s+"-2", time.Now(), 1900), 1900, nil); err != nil || !rec {
		t.Fatalf("write after a cancelled snapshot: recorded=%v err=%v", rec, err)
	}
	if snap, err := r.PointsSnapshotBetween(context.Background(), s, time.Time{}, time.Time{}, 0, false); err != nil || snap.Exact.Events != 2 {
		t.Fatalf("snapshot after a cancelled one = %+v err=%v, want 2 events", snap.Exact, err)
	}
}
