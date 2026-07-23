package auth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Corrective Pass 3: candidate revalidation survives a long /validate outage ---
//
// A granted (possibly already-spent) token pair must remain a private candidate
// under revalidation for as long as the lifecycle context is alive — the DELAY
// between attempts is bounded (capped exponential backoff), the NUMBER of
// attempts is not. Only an authoritative outcome, a stale generation, or
// context cancellation ends the loop. A finite retry budget that aborts startup
// (HEAD 9ea6dca: candidateValidateRetries=4) loses the memory-only candidate.

// manualTimer is a deterministic stand-in for a.timerAfter: it records the
// requested delays and only fires a parked wait when the test calls fireNext,
// so pacing can be asserted without any wall-clock sleep.
type manualTimer struct {
	mu      sync.Mutex
	waits   []time.Duration
	pending []chan time.Time
}

func newManualTimer() *manualTimer { return &manualTimer{} }

func (m *manualTimer) after(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	m.mu.Lock()
	m.waits = append(m.waits, d)
	m.pending = append(m.pending, ch)
	m.mu.Unlock()
	return ch
}

func (m *manualTimer) waitCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.waits)
}

func (m *manualTimer) durations() []time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]time.Duration(nil), m.waits...)
}

func (m *manualTimer) fireNext() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) == 0 {
		return false
	}
	ch := m.pending[0]
	m.pending = m.pending[1:]
	ch <- time.Time{}
	return true
}

// C: a permanently transient /validate followed by context cancellation ends
// the loop with a context error — the candidate never becomes active, is never
// persisted, fires no callback/Completed, and no second refresh/device grant is
// spent. (On 9ea6dca the ctx.Done branch instead stages and returns
// ErrRecoveryFailed, so this FAILs there.)
func TestCandidateRevalidationStopsOnCancellation(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	mt := newManualTimer()
	a.timerAfter = mt.after
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	// The recovery owner runs on the lifecycle context; cancelling it is the
	// process-shutdown signal. The refresh grant is a single HTTP call (no
	// timer), so the FIRST paced wait is the candidate revalidation wait.
	lifeCtx, cancel := context.WithCancel(context.Background())
	a.SetLifecycleContext(lifeCtx)

	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }
	f.mu.Unlock()

	var completed atomic.Int64
	a.SetEventCallback(func(ev AuthEvent) {
		if ev.Type == AuthEventCompleted {
			completed.Add(1)
		}
	})
	var callbacks atomic.Int64
	a.SetRotationCallback(func(uint64) { callbacks.Add(1) })

	errCh := make(chan error, 1)
	go func() {
		_, err := a.Recover(context.Background(), 0)
		errCh <- err
	}()

	// Let the revalidation loop park on at least one paced wait, then cancel
	// the lifecycle context.
	waitCond(t, "revalidation to park on a paced wait", func() bool { return mt.waitCount() >= 1 })
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("cancellation was ignored (loop did not exit)")
	}

	if a.GetAuthToken() != "test-access-1" || a.Generation() != 0 {
		t.Fatalf("cancelled candidate became active credentials")
	}
	if _, err := os.Stat(a.cookiesPath()); !os.IsNotExist(err) {
		t.Fatalf("cancelled candidate was persisted")
	}
	if completed.Load() != 0 || callbacks.Load() != 0 {
		t.Fatalf("cancelled candidate emitted Completed/rotation callback")
	}
	if _, _, refresh, _ := f.counts(); refresh != 1 {
		t.Fatalf("cancellation re-spent a refresh grant: refresh=%d, want exactly 1", refresh)
	}
}

// After the owner flight is cancelled mid-outage, the candidate stays privately
// staged; a fresh recovery must REVALIDATE that same staged candidate — never
// spend a second refresh grant (the one-time refresh token is already
// consumed). Exercises the pending-candidate priority in Recover.
func TestPendingCandidateRevalidatedWithoutRefreshAfterCancel(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	mt := newManualTimer()
	a.timerAfter = mt.after
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	lifeCtx, cancel := context.WithCancel(context.Background())
	a.SetLifecycleContext(lifeCtx)

	var down atomic.Bool
	down.Store(true)
	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(503)
			return
		}
		validFor(f, a, w)
	}
	f.mu.Unlock()

	// Owner 1: one refresh grant, stage the candidate, park on a paced wait,
	// then cancel the lifecycle context.
	errCh := make(chan error, 1)
	go func() {
		_, err := a.Recover(context.Background(), 0)
		errCh <- err
	}()
	waitCond(t, "candidate staged + parked", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.pendingCandidate != nil && mt.waitCount() >= 1
	})
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("owner 1 cancellation error = %v, want context.Canceled", err)
	}
	a.mu.Lock()
	stillStaged := a.pendingCandidate != nil
	a.mu.Unlock()
	if !stillStaged {
		t.Fatalf("the cancelled owner discarded the pending candidate")
	}

	// Validate recovers; a fresh recovery revalidates the staged candidate.
	down.Store(false)
	a.SetLifecycleContext(context.Background())
	snap, err := a.Recover(context.Background(), 0)
	if err != nil {
		t.Fatalf("owner 2: %v", err)
	}
	if snap.AccessToken != "test-access-2" || a.Generation() != 1 {
		t.Fatalf("staged candidate not promoted by the fresh recovery: %+v", snap)
	}
	if _, _, refresh, _ := f.counts(); refresh != 1 {
		t.Fatalf("refresh grants = %d, want exactly 1 (staged candidate must be revalidated, not re-fetched)", refresh)
	}
}

// D: an external complete-set replacement landing mid-revalidation ends the
// loop at the next iteration (stale generation) — the stale candidate publishes
// nothing and the loop stops promptly instead of running the rest of a budget.
// (On 9ea6dca staleness is checked only once at entry, so all 5 budgeted
// validates run; this asserts exactly 3.)
func TestStaleGenerationEndsCandidateRevalidation(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f) // immediateTimer: the loop paces instantly
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	var validates atomic.Int64
	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		if validates.Add(1) == 3 {
			// A complete-set replacement lands before this response is read,
			// bumping the generation the candidate targeted.
			a.ReplaceCredentials(TokenResponse{AccessToken: "external-access"})
		}
		w.WriteHeader(503)
	}
	f.mu.Unlock()

	var callbacks atomic.Int64
	a.SetRotationCallback(func(uint64) { callbacks.Add(1) })

	_, _ = a.Recover(context.Background(), 0)

	if got := validates.Load(); got != 3 {
		t.Fatalf("revalidation did not stop at the stale generation: %d validate calls, want 3", got)
	}
	if a.GetAuthToken() != "external-access" {
		t.Fatalf("stale revalidation published over the external replacement: %q", a.GetAuthToken())
	}
	if callbacks.Load() != 0 {
		t.Fatalf("stale revalidation fired the rotation callback")
	}
}

// E: revalidation delay is paced and capped — no network call happens without a
// timer fire, and the delay grows then plateaus at the cap. (On 9ea6dca the
// delay is a fixed 2s and the loop stops after 4 waits, so both the growth and
// the continuation assertions FAIL.)
func TestCandidateRevalidationPacingIsCapped(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	mt := newManualTimer()
	a.timerAfter = mt.after
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	var validates atomic.Int64
	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		if authHeaderToken(r) == "test-access-2" && validates.Add(1) <= 6 {
			w.WriteHeader(503)
			return
		}
		validFor(f, a, w)
	}
	f.mu.Unlock()

	done := make(chan struct{})
	go func() { _, _ = a.Recover(context.Background(), 0); close(done) }()

	// After each parked wait, exactly one validate has run — no network without
	// a fire. Drive six paced attempts, then the seventh validate succeeds.
	for k := 1; k <= 6; k++ {
		waitCond(t, "paced wait to be requested", func() bool { return mt.waitCount() >= k })
		if got := validates.Load(); got != int64(k) {
			t.Fatalf("attempt %d: %d validate calls before the timer fired (network without pacing)", k, got)
		}
		mt.fireNext()
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("revalidation did not converge after the outage cleared")
	}

	waits := mt.durations()
	if len(waits) < 6 {
		t.Fatalf("only %d paced waits observed, want >= 6 (loop gave up early)", len(waits))
	}
	capDelay := 60 * time.Second
	if waits[1] <= waits[0] {
		t.Fatalf("delay did not grow: %v", waits)
	}
	for i, d := range waits {
		if d > capDelay {
			t.Fatalf("delay[%d]=%v exceeds the cap %v", i, d, capDelay)
		}
		if i > 0 && d < waits[i-1] {
			t.Fatalf("delay decreased at %d: %v", i, waits)
		}
	}
	if waits[len(waits)-1] != capDelay {
		t.Fatalf("delay never reached the cap: last=%v cap=%v (%v)", waits[len(waits)-1], capDelay, waits)
	}
}

// F: across a long outage with many revalidation attempts, exactly one refresh
// grant is spent and the old one-time refresh token is presented exactly once —
// the pending candidate is revalidated, never re-fetched. (On 9ea6dca recovery
// fails after the finite budget, so the promotion assertion FAILs.)
func TestNoSecondRefreshAcrossLongOutage(t *testing.T) {
	f := newFakeOAuth(t)
	a := newLifecycleAuth(t, f)
	a.token = "test-access-1"
	a.refreshToken = "test-refresh-1"

	var validates atomic.Int64
	f.mu.Lock()
	f.validateHandler = func(w http.ResponseWriter, r *http.Request) {
		if authHeaderToken(r) == "test-access-2" && validates.Add(1) <= 20 {
			w.WriteHeader(503)
			return
		}
		validFor(f, a, w)
	}
	f.mu.Unlock()

	snap, err := a.Recover(context.Background(), 0)
	if err != nil {
		t.Fatalf("a long validate outage must not fail recovery: %v", err)
	}
	if snap.AccessToken != "test-access-2" || a.Generation() != 1 {
		t.Fatalf("candidate not promoted after the outage cleared: %+v", snap)
	}
	if _, _, refresh, _ := f.counts(); refresh != 1 {
		t.Fatalf("refresh grants = %d across the outage, want exactly 1", refresh)
	}
	f.mu.Lock()
	seen := append([]string(nil), f.refreshTokensSeen...)
	f.mu.Unlock()
	if len(seen) != 1 || seen[0] != "test-refresh-1" {
		t.Fatalf("old one-time refresh token presented %d times: %v", len(seen), seen)
	}
}
