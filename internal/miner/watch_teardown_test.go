package miner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// B2 — the watch generation's dirty teardown must reach the lifecycle owner.
//
// internal/lifecycle decides whether a generation may be retired by classifying
// Miner.Run's error through miner.IsJoinTimeoutError (wired in internal/app as
// lifecycle.IsDirtyTeardownError). Before B2 the watcher's bounded join could
// expire with the watch loop still live — still sending beacons, still writing
// watch_time rows into a database the shutdown was about to close — and the only
// record was a log line, so the controller retired the generation as though it
// were gone. These tests pin the wiring that closes that hole.

// stuckSource is a non-cooperative CandidateSource: it blocks inside
// WatchCandidates and deliberately ignores the watch generation's context, which
// is exactly the case the bounded join exists for. It is driven through the
// watcher's exported AddSource seam — no test-only production API is added.
type stuckSource struct {
	entered chan struct{}
	release chan struct{}
	once    bool
}

func (s *stuckSource) SourceName() string { return "stuck" }

func (s *stuckSource) WatchCandidates(context.Context) []watcher.Candidate {
	if !s.once {
		s.once = true
		close(s.entered)
	}
	<-s.release
	return nil
}

// newHungWatchGeneration returns a started watch generation whose loop goroutine
// is parked in a dependency that ignores cancellation, plus the release channel
// that lets it finish.
func newHungWatchGeneration(t *testing.T) (*watcher.MinuteWatcher, chan struct{}) {
	t.Helper()

	src := &stuckSource{entered: make(chan struct{}), release: make(chan struct{})}
	w := watcher.NewMinuteWatcher(nil, nil,
		[]config.Priority{config.PriorityOrder},
		config.RateLimitSettings{MinuteWatchedInterval: 1}, nil)
	w.AddSource(src)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := w.Start(ctx); err != nil {
		t.Fatalf("the watch generation must start: %v", err)
	}
	select {
	case <-src.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the watch loop never entered the stuck dependency")
	}
	cancel()
	return w, src.release
}

// Cost note: this test waits out the watcher's real stopJoinTimeout (5s).
// stopJoinTimeout is package-scoped in internal/watcher, so only tests in that
// package can shrink it, and exporting a test-only setter would widen a
// production API for test convenience. One fixed 5s buys the only end-to-end
// proof that a dirty watch teardown reaches the lifecycle owner.
//
// TestWatcherDirtyTeardownReachesTheDrainError proves the wiring end to end: a
// watcher whose bounded join expires contributes an errLoopJoinTimeout-class
// error to the shutdown drain, so IsJoinTimeoutError — the function
// internal/lifecycle installs as its dirty-teardown classifier — recognises it.
func TestWatcherDirtyTeardownReachesTheDrainError(t *testing.T) {
	w, release := newHungWatchGeneration(t)
	defer close(release)

	m := &Miner{watcher: w}
	err := m.stop()
	if err == nil {
		t.Fatal("a dirty watcher teardown must surface in the shutdown drain error, not only in a log line")
	}
	if !IsJoinTimeoutError(err) {
		t.Fatalf("a dirty watcher teardown must classify as a join-timeout-class failure "+
			"(what internal/lifecycle keys its dirty-teardown handling on), got %v", err)
	}
	if !errors.Is(err, watcher.ErrStopJoinTimeout) {
		t.Fatalf("the drain error must preserve the watcher's own sentinel for diagnosis, got %v", err)
	}
}

// TestCleanWatcherTeardownIsNotAJoinTimeout is the inverse guard: a watcher that
// quiesced must not be reported as a dirty teardown, or every clean shutdown
// would be misclassified as an orphaned generation.
func TestCleanWatcherTeardownIsNotAJoinTimeout(t *testing.T) {
	w := watcher.NewMinuteWatcher(nil, nil,
		[]config.Priority{config.PriorityOrder},
		config.RateLimitSettings{MinuteWatchedInterval: 1}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("the watch generation must start: %v", err)
	}

	m := &Miner{watcher: w}
	if err := m.stop(); err != nil {
		t.Fatalf("a clean watcher teardown must contribute no drain error, got %v", err)
	}
}
