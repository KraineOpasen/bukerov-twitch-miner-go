package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- fakeRunner / runnerFactory -------------------------------------------

// fakeRunner is a controllable Runner: Run blocks until the test sends on
// finishCh (or, unless ignoreCtx, ctx is cancelled), so tests can hold a
// generation open (barrier) exactly as long as they need. It panics if Run
// is ever called twice, directly testing design v6 item 10.
type fakeRunner struct {
	id        int
	startedCh chan struct{}
	finishCh  chan error
	ignoreCtx bool
	ran       int32

	// returnOnCancel, when set, is returned instead of ctx.Err() once ctx
	// is cancelled — used to simulate a "dirty teardown" class error
	// (design v6 §5.3) without needing the test to race finish() against
	// cancellation.
	returnOnCancel error

	active *int32         // shared "currently-running generations" counter, see runnerFactory
	rec    *eventRecorder // shared ordering recorder, see runnerFactory

	// ready backs Ready() for readySignalingRunner (see runnerFactory's
	// readySignaling flag) — always allocated so markReady/isReady work
	// regardless of which type Factory() actually returned; whether it is
	// ever EXPOSED via the ReadySignaler interface is controlled entirely
	// by whether Factory() wraps this fakeRunner in readySignalingRunner.
	ready chan struct{}
}

// markReady simulates this generation finishing its own internal startup
// phase — closes the channel readySignalingRunner.Ready() returns. Safe to
// call at most once; a no-op (never observably closed) if this runner
// wasn't built with readySignaling, since nothing ever reads f.ready then.
func (f *fakeRunner) markReady() { close(f.ready) }

// readySignalingRunner wraps a *fakeRunner so it ALSO implements
// ReadySignaler. runnerFactory.readySignaling controls whether Factory()
// returns a bare *fakeRunner (no readiness signal at all — matches
// production's current Miner adapter, b3) or one wrapped this way (used by
// tests exercising design v6 §5.1's "starting" row and retry/recovery
// timing against a genuine not-ready window).
type readySignalingRunner struct{ *fakeRunner }

func (r readySignalingRunner) Ready() <-chan struct{} { return r.ready }

func (f *fakeRunner) Run(ctx context.Context) error {
	if atomic.AddInt32(&f.ran, 1) != 1 {
		panic(fmt.Sprintf("fakeRunner %d: Run called more than once", f.id))
	}
	close(f.startedCh)
	if f.rec != nil {
		f.rec.add(fmt.Sprintf("start-%d", f.id))
	}
	if f.active != nil {
		n := atomic.AddInt32(f.active, 1)
		defer atomic.AddInt32(f.active, -1)
		if n > 1 {
			panic(fmt.Sprintf("more than one active generation: %d", n))
		}
	}
	defer func() {
		if f.rec != nil {
			f.rec.add(fmt.Sprintf("return-%d", f.id))
		}
	}()
	if f.ignoreCtx {
		return <-f.finishCh
	}
	select {
	case err := <-f.finishCh:
		return err
	case <-ctx.Done():
		if f.returnOnCancel != nil {
			return f.returnOnCancel
		}
		return ctx.Err()
	}
}

// finish makes the runner's Run return err. Safe to call at most once.
func (f *fakeRunner) finish(err error) {
	f.finishCh <- err
}

// waitStarted blocks until this runner's Run has actually begun executing.
func (f *fakeRunner) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-f.startedCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("fakeRunner %d: Run never started", f.id)
	}
}

// runnerFactory hands out fresh fakeRunners and records them so a test can
// reach in and control (or inspect) each generation individually.
type runnerFactory struct {
	mu       sync.Mutex
	runners  []*fakeRunner
	next     int
	active   int32 // shared across all runners built by this factory
	ignoreCt bool  // applied to every runner this factory builds
	rec      eventRecorder

	// readySignaling, when true, makes Factory() return a
	// readySignalingRunner (implements ReadySignaler) instead of a bare
	// *fakeRunner — so the controller holds observed at "starting"/
	// "restarting" until the test calls the runner's markReady().
	readySignaling bool
}

func newRunnerFactory() *runnerFactory { return &runnerFactory{} }

func (f *runnerFactory) Factory() Runner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	r := &fakeRunner{
		id:        f.next,
		startedCh: make(chan struct{}),
		finishCh:  make(chan error, 1),
		ignoreCtx: f.ignoreCt,
		active:    &f.active,
		rec:       &f.rec,
		ready:     make(chan struct{}),
	}
	f.runners = append(f.runners, r)
	if f.readySignaling {
		return readySignalingRunner{r}
	}
	return r
}

func (f *runnerFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runners)
}

func (f *runnerFactory) at(i int) *fakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runners[i]
}

func (f *runnerFactory) latest(t *testing.T) *fakeRunner {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.runners) == 0 {
		t.Fatalf("runnerFactory: no runner built yet")
	}
	return f.runners[len(f.runners)-1]
}

// waitCount blocks (bounded convergence poll — no channel exists for "a new
// generation was built") until the factory has built at least n runners.
func (f *runnerFactory) waitCount(t *testing.T, n int) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool { return f.count() >= n })
}

// eventRecorder captures an ordered log of "runner N started"/"runner N
// returned" facts across goroutines, so tests can assert ordering (e.g. "the
// old generation returned before the new one started") without any sleep.
type eventRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *eventRecorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, s)
}

func (r *eventRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

func indexOf(events []string, s string) int {
	for i, e := range events {
		if e == s {
			return i
		}
	}
	return -1
}

// --- fakeClock / fakeTimer -------------------------------------------------

// fakeClock is the Clock seam's test double: Now() is manually advanced only
// as a side effect of NewTimer bookkeeping; timers fire only when the test
// explicitly calls FireNext, never on a wall-clock schedule, so retry-
// backoff tests are deterministic and instant.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1700000000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, fireAt: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	return t
}

// pendingCount reports how many armed (not fired, not stopped) timers exist.
func (c *fakeClock) pendingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.fired && !t.stopped {
			n++
		}
	}
	return n
}

// fireNext fires the oldest still-armed timer, reporting whether it found
// one. Advances the fake clock's Now() to that timer's scheduled fire time.
func (c *fakeClock) fireNext() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.timers {
		if !t.fired && !t.stopped {
			t.fired = true
			c.now = t.fireAt
			t.ch <- t.fireAt
			return true
		}
	}
	return false
}

type fakeTimer struct {
	c       *fakeClock
	fireAt  time.Time
	ch      chan time.Time
	fired   bool
	stopped bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.fired {
		return false
	}
	already := t.stopped
	t.stopped = true
	return !already
}

// --- fakeStatusSink ------------------------------------------------------

// statusCall records one SetStatus invocation for assertions.
type statusCall struct {
	status  string
	message string
}

// fakeStatusSink is the StatusSink seam's test double: records every
// SetStatus/SetGeneration call (ordered) so a test can assert exactly what a
// b3 web adapter would have received, without internal/lifecycle ever
// importing internal/web.
type fakeStatusSink struct {
	mu          sync.Mutex
	statuses    []statusCall
	generations []uint64
}

func (s *fakeStatusSink) SetStatus(status, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses = append(s.statuses, statusCall{status: status, message: message})
}

func (s *fakeStatusSink) SetGeneration(gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generations = append(s.generations, gen)
}

func (s *fakeStatusSink) snapshotStatuses() []statusCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]statusCall, len(s.statuses))
	copy(out, s.statuses)
	return out
}

// --- fakePersistence ---------------------------------------------------

// fakePersistence is the Persistence seam's test double: Load returns a
// fixed, mutable result; Save records every call (for ordering assertions)
// and can be made to fail on demand.
type fakePersistence struct {
	mu sync.Mutex

	loadResult LoadResult
	loadErr    error

	saveErr error
	saves   []savedCall
	onSave  func(d DesiredState, reason, cmdID string)

	// saveBarrier, when armed (see armSaveBarrier), makes Save block — after
	// being recorded and after onSave has run, but before returning — until
	// the test releases it. Used to hold Save deterministically "in flight"
	// so a test can assert what has (or, importantly, has NOT) happened yet
	// while persistence is still pending, without any sleep-based timing.
	saveBarrier chan struct{}
}

type savedCall struct {
	desired DesiredState
	reason  string
	cmdID   string
}

func newFakePersistence(initial LoadResult) *fakePersistence {
	return &fakePersistence{loadResult: initial}
}

func (p *fakePersistence) Load(context.Context) (LoadResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loadResult, p.loadErr
}

func (p *fakePersistence) Save(_ context.Context, d DesiredState, reason, cmdID string) error {
	p.mu.Lock()
	err := p.saveErr
	if err == nil {
		p.saves = append(p.saves, savedCall{d, reason, cmdID})
	}
	hook := p.onSave
	barrier := p.saveBarrier
	p.mu.Unlock()
	if hook != nil {
		hook(d, reason, cmdID)
	}
	if barrier != nil {
		<-barrier
	}
	return err
}

// armSaveBarrier makes every subsequent Save call block — after being
// recorded and after onSave runs — until the returned release func is
// called. release is idempotent (safe to call once; further calls are a
// no-op). Lets a test hold Save deterministically "in flight" for a
// controlled window, e.g. to assert the caller has not yet signaled the
// worker (design v6 §5.2: persist happens BEFORE the worker is signaled).
func (p *fakePersistence) armSaveBarrier() (release func()) {
	ch := make(chan struct{})
	p.mu.Lock()
	p.saveBarrier = ch
	p.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

func (p *fakePersistence) setSaveErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saveErr = err
}

func (p *fakePersistence) saveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.saves)
}

func (p *fakePersistence) lastSave() savedCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saves[len(p.saves)-1]
}

// --- misc test helpers ---------------------------------------------------

// waitFor polls cond (bounded convergence poll) until it is true or timeout
// elapses, failing the test otherwise. Used only where no channel/barrier
// can observe the condition directly (e.g. "the worker has published a new
// Snapshot").
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

func waitObserved(t *testing.T, c *Controller, want ObservedState) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool { return c.Snapshot().Observed == want })
}

// waitFailedWithRetryScheduled waits for BOTH observed=failed AND
// NextRetryAt to be set. classifyUnhealthyCompletion publishes the failed
// terminal (observed) and only THEN arms the retry timer (NextRetryAt) —
// two sequential, non-atomic worker-goroutine calls with no channel/lock
// in between — so a bare waitObserved(failed) can (rarely, under load)
// return in the narrow window after the first call but before the second,
// making an immediate NextRetryAt read flaky. Use this instead of
// waitObserved+a bare NextRetryAt check wherever a test expects a retry to
// already be scheduled.
func waitFailedWithRetryScheduled(t *testing.T, c *Controller) Snapshot {
	t.Helper()
	var snap Snapshot
	waitFor(t, 5*time.Second, func() bool {
		snap = c.Snapshot()
		return snap.Observed == ObservedFailed && !snap.NextRetryAt.IsZero()
	})
	return snap
}

// runInBackground starts c.Run(ctx) in a goroutine and returns a channel
// that receives its eventual result exactly once.
func runInBackground(c *Controller, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	return done
}

// shrinkRetrySchedule replaces the package-level retry backoff schedule
// with tiny durations for the duration of one test, restoring the original
// on cleanup (t.Cleanup) — the schedule only needs to be "tiny", never
// actually observed, since tests fire the fake clock's timers manually.
func shrinkRetrySchedule(t *testing.T) {
	t.Helper()
	orig := RetryBackoffSchedule
	RetryBackoffSchedule = []time.Duration{time.Millisecond}
	t.Cleanup(func() { RetryBackoffSchedule = orig })
}
