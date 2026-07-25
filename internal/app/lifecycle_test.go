package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests exercise the generic lifecycle mechanism (Run start ordering,
// reverse-order shutdown, idempotency, concurrency, context handling, and the
// single-use guards) against fake steps and a fake runner, so ordering and
// safety are verified without the miner's network/auth dependencies. Build's
// real-resource wiring is covered in build_test.go.

var errBoom = errors.New("boom")

// recorder captures the ordered sequence of lifecycle events across goroutines.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.events = append(r.events, s)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

// step builds a lifecycleStep that records start:<name> / stop:<name> and
// returns the supplied errors. A nil start models a resource that is live once
// constructed (database, analytics service).
func step(rec *recorder, name string, startErr, stopErr error) lifecycleStep {
	s := lifecycleStep{
		name: name,
		stop: func(context.Context) error {
			rec.add("stop:" + name)
			return stopErr
		},
	}
	if startErr != errSkipStart {
		s.start = func(context.Context) error {
			rec.add("start:" + name)
			return startErr
		}
	}
	return s
}

// errSkipStart is a sentinel meaning "this step has no start step at all"
// (distinct from a start that succeeds with nil).
var errSkipStart = errors.New("no start")

// fakeRunner is a stand-in for *miner.Miner.
type fakeRunner struct {
	rec     *recorder
	err     error
	block   bool
	started chan struct{}
}

func (f *fakeRunner) Run(ctx context.Context) error {
	if f.rec != nil {
		f.rec.add("run")
	}
	if f.started != nil {
		close(f.started)
	}
	if f.block {
		<-ctx.Done()
	}
	return f.err
}

func mustContain(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event mismatch:\n got=%v\nwant=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d]=%q want %q (full got=%v want=%v)", i, got[i], want[i], got, want)
		}
	}
}

// C6 — Run starts services in construction order, then runs the runner.
func TestRunStartsStepsInOrder(t *testing.T) {
	rec := &recorder{}
	a := &App{
		runner: &fakeRunner{rec: rec},
		steps: []lifecycleStep{
			step(rec, "database", errSkipStart, nil),
			step(rec, "analytics", errSkipStart, nil),
			step(rec, "web", nil, nil),
		},
	}
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// database/analytics have no start; only web starts, then the runner runs.
	mustContain(t, rec.snapshot(), []string{"start:web", "run"})
}

// C7 — a start-step failure unwinds every opened/started step in reverse order
// and never runs the runner.
func TestRunStartFailureUnwindsInReverse(t *testing.T) {
	rec := &recorder{}
	a := &App{
		runner: &fakeRunner{rec: rec},
		steps: []lifecycleStep{
			step(rec, "database", errSkipStart, nil),
			step(rec, "first", nil, nil),
			step(rec, "second", errBoom, nil),
		},
	}
	err := a.Run(context.Background())
	if !errors.Is(err, errBoom) {
		t.Fatalf("want errBoom, got %v", err)
	}
	got := rec.snapshot()
	// Started first+second (second failed), then reverse-stop every step; the
	// runner must not have run.
	for _, e := range got {
		if e == "run" {
			t.Fatalf("runner ran despite start failure: %v", got)
		}
	}
	mustContain(t, got, []string{"start:first", "start:second", "stop:second", "stop:first", "stop:database"})
	// Explicit reverse-order assertion on the stop half.
	assertOrder(t, got, "stop:second", "stop:first")
	assertOrder(t, got, "stop:first", "stop:database")
}

// C8 — Shutdown stops steps in strict reverse construction order.
func TestShutdownReverseOrder(t *testing.T) {
	rec := &recorder{}
	a := &App{steps: []lifecycleStep{
		step(rec, "database", errSkipStart, nil),
		step(rec, "analytics", errSkipStart, nil),
		step(rec, "web", errSkipStart, nil),
	}}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	mustContain(t, rec.snapshot(), []string{"stop:web", "stop:analytics", "stop:database"})
}

// C9 — Shutdown is idempotent: a second call is a no-op and returns the same
// error.
func TestShutdownIdempotent(t *testing.T) {
	rec := &recorder{}
	a := &App{steps: []lifecycleStep{step(rec, "database", errSkipStart, nil)}}
	first := a.Shutdown(context.Background())
	second := a.Shutdown(context.Background())
	if first != nil || second != nil {
		t.Fatalf("errors: first=%v second=%v", first, second)
	}
	mustContain(t, rec.snapshot(), []string{"stop:database"})
}

// C10 — concurrent Shutdown callers each observe a single, complete teardown.
func TestShutdownConcurrent(t *testing.T) {
	var counts sync.Map
	mk := func(name string) lifecycleStep {
		return lifecycleStep{name: name, stop: func(context.Context) error {
			v, _ := counts.LoadOrStore(name, new(int))
			// serialize increments via the sync.Once inside Shutdown; only one
			// goroutine ever runs the body, so no atomic needed, but keep it
			// race-clean.
			p := v.(*int)
			*p++
			return nil
		}}
	}
	a := &App{steps: []lifecycleStep{mk("database"), mk("analytics"), mk("web")}}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.Shutdown(context.Background())
		}()
	}
	wg.Wait()

	for _, name := range []string{"database", "analytics", "web"} {
		v, _ := counts.Load(name)
		if v == nil || *(v.(*int)) != 1 {
			t.Fatalf("step %s stopped %v times, want 1", name, v)
		}
	}
}

// C11 — Shutdown surfaces a context deadline while still attempting every stop.
func TestShutdownContextTimeout(t *testing.T) {
	rec := &recorder{}
	a := &App{steps: []lifecycleStep{
		{name: "slow", stop: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}},
		{name: "fast", stop: func(context.Context) error {
			rec.add("stop:fast")
			return nil
		}},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := a.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("Shutdown blocked too long")
	}
	// "fast" (constructed first, stopped last) still ran.
	mustContain(t, rec.snapshot(), []string{"stop:fast"})
}

// C16 — Run is single-use: a second Run returns ErrAlreadyRun.
func TestRunTwiceReturnsError(t *testing.T) {
	a := &App{runner: &fakeRunner{}}
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := a.Run(context.Background()); !errors.Is(err, ErrAlreadyRun) {
		t.Fatalf("second Run: want ErrAlreadyRun, got %v", err)
	}
}

// C17 / C18 — Shutdown before Run is safe; Run after Shutdown returns
// ErrShutDown.
func TestRunAfterShutdown(t *testing.T) {
	rec := &recorder{}
	a := &App{
		runner: &fakeRunner{rec: rec},
		steps:  []lifecycleStep{step(rec, "database", errSkipStart, nil)},
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Run: %v", err)
	}
	if err := a.Run(context.Background()); !errors.Is(err, ErrShutDown) {
		t.Fatalf("Run after Shutdown: want ErrShutDown, got %v", err)
	}
	// The runner must never have run.
	for _, e := range rec.snapshot() {
		if e == "run" {
			t.Fatalf("runner ran after shutdown")
		}
	}
}

// C19 — a runtime fatal error from the runner propagates out of Run unchanged
// (once; App does not wrap or log it).
func TestRunPropagatesRunnerError(t *testing.T) {
	a := &App{runner: &fakeRunner{err: errBoom}}
	err := a.Run(context.Background())
	if !errors.Is(err, errBoom) {
		t.Fatalf("want errBoom, got %v", err)
	}
}

// I2 / I10 — the full happy path: Run starts the web step and blocks in the
// runner until ctx is cancelled, then a graceful Shutdown tears the step down.
func TestRunThenCancelThenShutdown(t *testing.T) {
	rec := &recorder{}
	started := make(chan struct{})
	a := &App{
		runner: &fakeRunner{rec: rec, block: true, started: started},
		steps:  []lifecycleStep{step(rec, "web", nil, nil)},
	}
	ctx, cancel := context.WithCancel(context.Background())

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- a.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner never started")
	}
	cancel()
	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	got := rec.snapshot()
	assertOrder(t, got, "start:web", "run")
	assertOrder(t, got, "run", "stop:web")
}

// C5 (mechanism) — stopSteps tears down every step exactly once, in reverse.
func TestStopStepsReverseExactlyOnce(t *testing.T) {
	rec := &recorder{}
	counts := map[string]int{}
	mk := func(name string) lifecycleStep {
		return lifecycleStep{name: name, stop: func(context.Context) error {
			rec.add(name)
			counts[name]++
			return nil
		}}
	}
	a := &App{steps: []lifecycleStep{mk("database"), mk("analytics"), mk("web")}}
	if err := a.stopSteps(context.Background(), len(a.steps)); err != nil {
		t.Fatalf("stopSteps: %v", err)
	}
	mustContain(t, rec.snapshot(), []string{"web", "analytics", "database"})
	for name, n := range counts {
		if n != 1 {
			t.Fatalf("step %s stopped %d times, want 1", name, n)
		}
	}
}

// stopSteps aggregates errors from every step without hiding any.
func TestStopStepsAggregatesErrors(t *testing.T) {
	rec := &recorder{}
	a := &App{steps: []lifecycleStep{
		{name: "database", stop: func(context.Context) error { rec.add("database"); return errBoom }},
		{name: "web", stop: func(context.Context) error { rec.add("web"); return errors.New("web-fail") }},
	}}
	err := a.stopSteps(context.Background(), len(a.steps))
	if err == nil || !errors.Is(err, errBoom) {
		t.Fatalf("want aggregated error wrapping errBoom, got %v", err)
	}
	if !strings.Contains(err.Error(), "web-fail") {
		t.Fatalf("aggregate missing web error: %v", err)
	}
	// Both stops still ran despite the first error.
	mustContain(t, rec.snapshot(), []string{"web", "database"})
}

// assertOrder asserts that a appears before b in the event slice.
func assertOrder(t *testing.T, events []string, a, b string) {
	t.Helper()
	ai, bi := -1, -1
	for i, e := range events {
		if e == a && ai == -1 {
			ai = i
		}
		if e == b {
			bi = i
		}
	}
	if ai == -1 {
		t.Fatalf("event %q not found in %v", a, events)
	}
	if bi == -1 {
		t.Fatalf("event %q not found in %v", b, events)
	}
	if ai >= bi {
		t.Fatalf("expected %q before %q, got order %v", a, b, events)
	}
}
