package lifecycle

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// (31) A full start/pause/resume/restart/stop cycle, repeated several
// times, leaves no goroutine leak: NumGoroutine() converges back to
// (roughly) its starting point once every Controller.Run has returned.
func TestNoGoroutineLeakAcrossFullCycles(t *testing.T) {
	runtime.GC()
	before := runtime.NumGoroutine()

	for i := 0; i < 5; i++ {
		rf := newRunnerFactory()
		c, _, cancel, done := newRunningController(t, rf)

		if res := c.Pause(context.Background()); res.Outcome != OutcomeAccepted {
			t.Fatalf("cycle %d: pause: %v (%v)", i, res.Outcome, res.Err)
		}
		waitObserved(t, c, ObservedPaused)

		if res := c.Resume(context.Background()); res.Outcome != OutcomeAccepted {
			t.Fatalf("cycle %d: resume: %v (%v)", i, res.Outcome, res.Err)
		}
		waitObserved(t, c, ObservedRunning)

		if res := c.Restart(context.Background()); res.Outcome != OutcomeAccepted {
			t.Fatalf("cycle %d: restart: %v (%v)", i, res.Outcome, res.Err)
		}
		rf.waitCount(t, 3) // resume built generation 2; restart must build a 3rd
		waitObserved(t, c, ObservedRunning)

		if res := c.Stop(context.Background()); res.Outcome != OutcomeAccepted {
			t.Fatalf("cycle %d: stop: %v (%v)", i, res.Outcome, res.Err)
		}
		waitObserved(t, c, ObservedStopped)

		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("cycle %d: Run did not return after cancel", i)
		}
	}

	// Bounded convergence poll (goroutine teardown — timer/runtime
	// bookkeeping goroutines, GC workers — can lag the observable point by
	// a few scheduler ticks even with no true leak).
	waitFor(t, 5*time.Second, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= before+2
	})
}
