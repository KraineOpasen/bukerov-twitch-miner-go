package miner

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

type fakeStreamCheckTimer struct {
	ch         chan time.Time
	selected   chan struct{}
	resets     chan time.Time
	stopped    chan struct{}
	selectOnce sync.Once
	stopOnce   sync.Once
}

func newFakeStreamCheckTimer() *fakeStreamCheckTimer {
	return &fakeStreamCheckTimer{
		ch:       make(chan time.Time, 1),
		selected: make(chan struct{}),
		resets:   make(chan time.Time, 16),
		stopped:  make(chan struct{}),
	}
}

func (f *fakeStreamCheckTimer) Chan() <-chan time.Time {
	f.selectOnce.Do(func() { close(f.selected) })
	return f.ch
}

func (f *fakeStreamCheckTimer) Reset(deadline time.Time) bool {
	// Go 1.25's Timer.Reset guarantees that a stale value from the previous
	// deadline cannot be received after Reset returns. Mirror that contract so
	// the simultaneous timer+wake regression exercises either select order.
	select {
	case <-f.ch:
	default:
	}
	f.resets <- deadline
	return true
}

func (f *fakeStreamCheckTimer) Stop() bool {
	f.stopOnce.Do(func() { close(f.stopped) })
	return true
}

func receiveStreamCheckTest[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

type uncheckedStreamCheckCall struct {
	interval time.Duration
	now      time.Time
}

func startStreamCheckLoopTest(
	t *testing.T,
	m *Miner,
	timer *fakeStreamCheckTimer,
	now time.Time,
	checkAll func(),
	checkUnchecked func(time.Duration, time.Time),
) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.runStreamCheckLoop(ctx, streamCheckLoopDeps{
			now:            func() time.Time { return now },
			newTimer:       func(time.Time) streamCheckTimer { return timer },
			checkAll:       checkAll,
			checkUnchecked: checkUnchecked,
		})
	}()
	receiveStreamCheckTest(t, timer.selected, "loop select")
	return cancel, done
}

func stopStreamCheckLoopTest(t *testing.T, cancel context.CancelFunc, done <-chan struct{}, timer *fakeStreamCheckTimer) {
	t.Helper()
	cancel()
	receiveStreamCheckTest(t, done, "loop exit")
	receiveStreamCheckTest(t, timer.stopped, "timer stop")
}

func holdStreamCheckLoopAtSweep(t *testing.T, m *Miner, timer *fakeStreamCheckTimer, now time.Time) func() {
	t.Helper()
	sweepStarted := make(chan struct{})
	releaseSweep := make(chan struct{})
	cancel, done := startStreamCheckLoopTest(t, m, timer, now, func() {}, func(time.Duration, time.Time) {
		close(sweepStarted)
		<-releaseSweep
	})
	m.triggerStreamCheck()
	receiveStreamCheckTest(t, sweepStarted, "blocking due-only sweep")

	var finishOnce sync.Once
	finish := func() {
		finishOnce.Do(func() {
			close(releaseSweep)
			stopStreamCheckLoopTest(t, cancel, done, timer)
		})
	}
	t.Cleanup(finish)
	return finish
}

func setStreamCheckIntervalForTest(m *Miner, seconds int) {
	m.mu.Lock()
	m.config.RateLimits.StreamCheckInterval = seconds
	m.mu.Unlock()
}

func TestApplySettingsStreamCheckIntervalWakesScheduler(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.RateLimits.StreamCheckInterval = 600
	timer := newFakeStreamCheckTimer()
	now := time.Date(2026, 8, 22, 19, 40, 0, 0, time.UTC)
	unchecked := make(chan uncheckedStreamCheckCall, 1)
	cancel, done := startStreamCheckLoopTest(t, m, timer, now, func() {}, func(interval time.Duration, at time.Time) {
		unchecked <- uncheckedStreamCheckCall{interval: interval, now: at}
	})

	rs := settings.BuildRuntimeSettings(m.config)
	rs.RateLimits.StreamCheckInterval = 60

	if err := m.ApplySettings(context.Background(), rs); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	if got, want := receiveStreamCheckTest(t, timer.resets, "committed interval timer reset"), now.Add(60*time.Second); !got.Equal(want) {
		t.Fatalf("reset deadline = %v, want %v", got, want)
	}
	call := receiveStreamCheckTest(t, unchecked, "committed interval due-only sweep")
	if call.interval != 60*time.Second || !call.now.Equal(now) {
		t.Fatalf("unchecked sweep = {%v %v}, want {1m0s %v}", call.interval, call.now, now)
	}

	stopStreamCheckLoopTest(t, cancel, done, timer)
}

func TestApplySettingsRenameAndStreamCheckIntervalWakesScheduler(t *testing.T) {
	client := newRenameCapableAPI()
	client.set("oldlogin", "stable-channel")
	client.set("newlogin", "stable-channel")
	m, _, _ := newRenameTestMiner(t, client, "oldlogin")
	m.config.RateLimits.StreamCheckInterval = 600
	timer := newFakeStreamCheckTimer()
	now := time.Date(2026, 8, 22, 19, 45, 0, 0, time.UTC)
	unchecked := make(chan uncheckedStreamCheckCall, 1)
	cancel, done := startStreamCheckLoopTest(t, m, timer, now, func() {}, func(interval time.Duration, at time.Time) {
		unchecked <- uncheckedStreamCheckCall{interval: interval, now: at}
	})

	rs := renameRuntimeStreamers(m, "oldlogin", "newlogin")
	rs.RateLimits.StreamCheckInterval = 60

	if err := m.ApplySettings(context.Background(), rs); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	if got, want := receiveStreamCheckTest(t, timer.resets, "rename interval timer reset"), now.Add(60*time.Second); !got.Equal(want) {
		t.Fatalf("reset deadline = %v, want %v", got, want)
	}
	call := receiveStreamCheckTest(t, unchecked, "rename interval due-only sweep")
	if call.interval != 60*time.Second || !call.now.Equal(now) {
		t.Fatalf("unchecked sweep = {%v %v}, want {1m0s %v}", call.interval, call.now, now)
	}

	stopStreamCheckLoopTest(t, cancel, done, timer)
}

func TestApplySettingsRemovalAndStreamCheckIntervalWakesScheduler(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha", "beta")
	m.config.RateLimits.StreamCheckInterval = 600
	timer := newFakeStreamCheckTimer()
	now := time.Date(2026, 8, 22, 19, 50, 0, 0, time.UTC)
	unchecked := make(chan uncheckedStreamCheckCall, 1)
	cancel, done := startStreamCheckLoopTest(t, m, timer, now, func() {}, func(interval time.Duration, at time.Time) {
		unchecked <- uncheckedStreamCheckCall{interval: interval, now: at}
	})

	rs := settings.BuildRuntimeSettings(m.config)
	rs.Streamers = rs.Streamers[:1]
	rs.RateLimits.StreamCheckInterval = 60
	if err := m.ApplySettings(context.Background(), rs); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	if got, want := receiveStreamCheckTest(t, timer.resets, "removal interval timer reset"), now.Add(60*time.Second); !got.Equal(want) {
		t.Fatalf("reset deadline = %v, want %v", got, want)
	}
	call := receiveStreamCheckTest(t, unchecked, "removal interval due-only sweep")
	if call.interval != 60*time.Second || !call.now.Equal(now) {
		t.Fatalf("unchecked sweep = {%v %v}, want {1m0s %v}", call.interval, call.now, now)
	}

	stopStreamCheckLoopTest(t, cancel, done, timer)
}

func TestApplySettingsUnchangedStreamCheckIntervalDoesNotWakeScheduler(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.RateLimits.StreamCheckInterval = 60
	timer := newFakeStreamCheckTimer()
	now := time.Date(2026, 8, 22, 19, 55, 0, 0, time.UTC)
	finish := holdStreamCheckLoopAtSweep(t, m, timer, now)
	wantDeadline := m.GetNextStreamCheck()

	rs := settings.BuildRuntimeSettings(m.config)
	if err := m.ApplySettings(context.Background(), rs); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	assertNoQueuedStreamCheckWakeOrReset(t, m, timer, "unchanged StreamCheckInterval")
	if got := m.GetNextStreamCheck(); !got.Equal(wantDeadline) {
		t.Fatalf("unchanged apply moved deadline from %v to %v", wantDeadline, got)
	}
	finish()
}

func TestApplySettingsFailedStreamCheckIntervalDoesNotWakeScheduler(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.RateLimits.StreamCheckInterval = 600
	timer := newFakeStreamCheckTimer()
	now := time.Date(2026, 8, 22, 20, 0, 0, 0, time.UTC)
	finish := holdStreamCheckLoopAtSweep(t, m, timer, now)
	configPath := filepath.Join(t.TempDir(), "config.json")
	m.configPath = configPath
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	breakConfigPathForNextSave(t, configPath)

	rs := settings.BuildRuntimeSettings(m.config)
	rs.RateLimits.StreamCheckInterval = 60
	if err := m.ApplySettings(context.Background(), rs); err == nil {
		t.Fatal("ApplySettings succeeded after config persistence failed")
	}

	if got := m.config.RateLimits.StreamCheckInterval; got != 600 {
		t.Fatalf("live StreamCheckInterval = %d after rejected apply, want 600", got)
	}
	assertNoQueuedStreamCheckWakeOrReset(t, m, timer, "rejected StreamCheckInterval change")
	if got, want := m.GetNextStreamCheck(), now.Add(600*time.Second); !got.Equal(want) {
		t.Fatalf("rejected apply moved deadline to %v, want %v", got, want)
	}
	finish()
}

func assertNoQueuedStreamCheckWakeOrReset(t *testing.T, m *Miner, timer *fakeStreamCheckTimer, what string) {
	t.Helper()
	select {
	case <-m.streamCheckTrigger:
		t.Fatalf("%s queued a scheduler wake", what)
	default:
	}
	select {
	case got := <-timer.resets:
		t.Fatalf("%s reset timer to %v", what, got)
	default:
	}
}

func TestStreamCheckLoopPublishesInitialDeadline(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.RateLimits.StreamCheckInterval = 60
	timer := newFakeStreamCheckTimer()
	created := make(chan time.Time, 1)
	now := time.Date(2026, 8, 22, 20, 5, 0, 0, time.UTC)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.runStreamCheckLoop(ctx, streamCheckLoopDeps{
			now: func() time.Time { return now },
			newTimer: func(deadline time.Time) streamCheckTimer {
				created <- deadline
				return timer
			},
			checkAll:       func() {},
			checkUnchecked: func(time.Duration, time.Time) {},
		})
	}()

	if got, want := receiveStreamCheckTest(t, created, "initial timer"), now.Add(60*time.Second); !got.Equal(want) {
		t.Fatalf("initial timer deadline = %v, want %v", got, want)
	}
	receiveStreamCheckTest(t, timer.selected, "loop select")
	if got, want := m.GetNextStreamCheck(), now.Add(60*time.Second); !got.Equal(want) {
		t.Fatalf("GetNextStreamCheck = %v, want %v", got, want)
	}

	cancel()
	receiveStreamCheckTest(t, done, "loop exit")
	receiveStreamCheckTest(t, timer.stopped, "timer stop")
}

func TestStreamCheckLoopRearmsAfterPeriodicCheck(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.RateLimits.StreamCheckInterval = 60
	timer := newFakeStreamCheckTimer()
	initial := time.Date(2026, 8, 22, 20, 7, 0, 0, time.UTC)
	afterCheck := initial.Add(17 * time.Second)
	nowSamples := make(chan time.Time, 2)
	nowSamples <- initial
	nowSamples <- afterCheck
	fullChecks := make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.runStreamCheckLoop(ctx, streamCheckLoopDeps{
			now:      func() time.Time { return <-nowSamples },
			newTimer: func(time.Time) streamCheckTimer { return timer },
			checkAll: func() { fullChecks <- struct{}{} },
			checkUnchecked: func(time.Duration, time.Time) {
			},
		})
	}()
	receiveStreamCheckTest(t, timer.selected, "loop select")

	timer.ch <- initial.Add(60 * time.Second)
	receiveStreamCheckTest(t, fullChecks, "periodic full check")
	if got, want := receiveStreamCheckTest(t, timer.resets, "periodic timer rearm"), afterCheck.Add(60*time.Second); !got.Equal(want) {
		t.Fatalf("rearmed deadline = %v, want %v", got, want)
	}
	if got, want := m.GetNextStreamCheck(), afterCheck.Add(60*time.Second); !got.Equal(want) {
		t.Fatalf("GetNextStreamCheck after periodic check = %v, want %v", got, want)
	}
	select {
	case <-fullChecks:
		t.Fatal("one timer event started more than one full check")
	default:
	}

	stopStreamCheckLoopTest(t, cancel, done, timer)
}

func TestStreamCheckLoopCancellationWinsSelectedTimer(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.RateLimits.StreamCheckInterval = 60
	timer := newFakeStreamCheckTimer()
	timer.ch = make(chan time.Time)
	now := time.Date(2026, 8, 22, 20, 8, 0, 0, time.UTC)
	fullChecks := make(chan struct{}, 1)
	intervalReadStarted := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.runStreamCheckLoop(ctx, streamCheckLoopDeps{
			now:            func() time.Time { return now },
			newTimer:       func(time.Time) streamCheckTimer { return timer },
			checkAll:       func() { fullChecks <- struct{}{} },
			checkUnchecked: func(time.Duration, time.Time) {},
			beforeSelectedIntervalRead: func() {
				close(intervalReadStarted)
			},
		})
	}()
	receiveStreamCheckTest(t, timer.selected, "loop select")

	m.mu.Lock()
	locked := true
	defer func() {
		if locked {
			m.mu.Unlock()
		}
	}()
	timerSelected := make(chan struct{})
	go func() {
		timer.ch <- now.Add(60 * time.Second)
		close(timerSelected)
	}()
	receiveStreamCheckTest(t, timerSelected, "selected timer event")
	receiveStreamCheckTest(t, intervalReadStarted, "timer interval read")
	cancel()
	m.mu.Unlock()
	locked = false

	receiveStreamCheckTest(t, done, "cancelled loop exit")
	receiveStreamCheckTest(t, timer.stopped, "cancelled timer stop")
	select {
	case <-fullChecks:
		t.Fatal("selected timer started a full check after cancellation")
	default:
	}
}

func TestStreamCheckLoopCancellationWinsSelectedWake(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.RateLimits.StreamCheckInterval = 60
	m.streamCheckTrigger = make(chan struct{})
	timer := newFakeStreamCheckTimer()
	now := time.Date(2026, 8, 22, 20, 9, 0, 0, time.UTC)
	unchecked := make(chan struct{}, 1)
	intervalReadStarted := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.runStreamCheckLoop(ctx, streamCheckLoopDeps{
			now:            func() time.Time { return now },
			newTimer:       func(time.Time) streamCheckTimer { return timer },
			checkAll:       func() {},
			checkUnchecked: func(time.Duration, time.Time) { unchecked <- struct{}{} },
			beforeSelectedIntervalRead: func() {
				close(intervalReadStarted)
			},
		})
	}()
	receiveStreamCheckTest(t, timer.selected, "loop select")

	m.mu.Lock()
	locked := true
	defer func() {
		if locked {
			m.mu.Unlock()
		}
	}()
	wakeSelected := make(chan struct{})
	go func() {
		m.streamCheckTrigger <- struct{}{}
		close(wakeSelected)
	}()
	receiveStreamCheckTest(t, wakeSelected, "selected scheduler wake")
	receiveStreamCheckTest(t, intervalReadStarted, "wake interval read")
	cancel()
	m.mu.Unlock()
	locked = false

	receiveStreamCheckTest(t, done, "cancelled loop exit")
	receiveStreamCheckTest(t, timer.stopped, "cancelled timer stop")
	select {
	case <-unchecked:
		t.Fatal("selected wake started a due-only check after cancellation")
	default:
	}
}

func TestStreamCheckLoopResetsShorterIntervalOnWake(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.RateLimits.StreamCheckInterval = 600
	timer := newFakeStreamCheckTimer()
	now := time.Date(2026, 8, 22, 20, 10, 0, 0, time.UTC)
	unchecked := make(chan uncheckedStreamCheckCall, 1)
	cancel, done := startStreamCheckLoopTest(t, m, timer, now, func() {}, func(interval time.Duration, at time.Time) {
		unchecked <- uncheckedStreamCheckCall{interval: interval, now: at}
	})

	rs := settings.BuildRuntimeSettings(m.config)
	rs.RateLimits.StreamCheckInterval = 60
	if err := m.ApplySettings(context.Background(), rs); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	if got, want := receiveStreamCheckTest(t, timer.resets, "shorter timer reset"), now.Add(60*time.Second); !got.Equal(want) {
		t.Fatalf("reset deadline = %v, want %v", got, want)
	}
	call := receiveStreamCheckTest(t, unchecked, "due-only sweep")
	if call.interval != 60*time.Second || !call.now.Equal(now) {
		t.Fatalf("unchecked sweep = {%v %v}, want {1m0s %v}", call.interval, call.now, now)
	}
	if got, want := m.GetNextStreamCheck(), now.Add(60*time.Second); !got.Equal(want) {
		t.Fatalf("GetNextStreamCheck = %v, want %v", got, want)
	}

	stopStreamCheckLoopTest(t, cancel, done, timer)
}

func TestStreamCheckLoopRosterWakeKeepsDeadline(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.RateLimits.StreamCheckInterval = 60
	timer := newFakeStreamCheckTimer()
	now := time.Date(2026, 8, 22, 20, 15, 0, 0, time.UTC)
	unchecked := make(chan uncheckedStreamCheckCall, 1)
	cancel, done := startStreamCheckLoopTest(t, m, timer, now, func() {}, func(interval time.Duration, at time.Time) {
		unchecked <- uncheckedStreamCheckCall{interval: interval, now: at}
	})
	wantDeadline := m.GetNextStreamCheck()

	m.triggerStreamCheck()
	call := receiveStreamCheckTest(t, unchecked, "roster due-only sweep")
	if call.interval != 60*time.Second || !call.now.Equal(now) {
		t.Fatalf("unchecked sweep = {%v %v}, want {1m0s %v}", call.interval, call.now, now)
	}
	select {
	case got := <-timer.resets:
		t.Fatalf("roster-only wake reset timer to %v", got)
	default:
	}
	if got := m.GetNextStreamCheck(); !got.Equal(wantDeadline) {
		t.Fatalf("roster-only wake moved deadline from %v to %v", wantDeadline, got)
	}

	stopStreamCheckLoopTest(t, cancel, done, timer)
}

func TestStreamCheckLoopSkipsObsoleteReadyTimer(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.RateLimits.StreamCheckInterval = 60
	timer := newFakeStreamCheckTimer()
	now := time.Date(2026, 8, 22, 20, 20, 0, 0, time.UTC)
	fullChecks := make(chan struct{}, 1)
	cancel, done := startStreamCheckLoopTest(t, m, timer, now, func() { fullChecks <- struct{}{} }, func(time.Duration, time.Time) {})

	setStreamCheckIntervalForTest(m, 900)
	timer.ch <- now
	if got, want := receiveStreamCheckTest(t, timer.resets, "longer timer reset"), now.Add(900*time.Second); !got.Equal(want) {
		t.Fatalf("reset deadline = %v, want %v", got, want)
	}
	select {
	case <-fullChecks:
		t.Fatal("obsolete ready timer started a full stream check")
	default:
	}
	if got, want := m.GetNextStreamCheck(), now.Add(900*time.Second); !got.Equal(want) {
		t.Fatalf("GetNextStreamCheck = %v, want %v", got, want)
	}

	stopStreamCheckLoopTest(t, cancel, done, timer)
}

func TestStreamCheckLoopTimerAndWakeReadySkipObsoleteCheck(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.RateLimits.StreamCheckInterval = 60
	timer := newFakeStreamCheckTimer()
	now := time.Date(2026, 8, 22, 20, 25, 0, 0, time.UTC)
	firstSweepStarted := make(chan struct{})
	releaseFirstSweep := make(chan struct{})
	secondSweep := make(chan struct{}, 1)
	var sweepMu sync.Mutex
	sweeps := 0
	fullChecks := make(chan struct{}, 1)
	cancel, done := startStreamCheckLoopTest(t, m, timer, now, func() { fullChecks <- struct{}{} }, func(time.Duration, time.Time) {
		sweepMu.Lock()
		sweeps++
		call := sweeps
		sweepMu.Unlock()
		if call == 1 {
			close(firstSweepStarted)
			<-releaseFirstSweep
			return
		}
		secondSweep <- struct{}{}
	})

	m.triggerStreamCheck()
	receiveStreamCheckTest(t, firstSweepStarted, "blocking due-only sweep")
	rs := settings.BuildRuntimeSettings(m.config)
	rs.RateLimits.StreamCheckInterval = 900
	if err := m.ApplySettings(context.Background(), rs); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	timer.ch <- now
	close(releaseFirstSweep)

	if got, want := receiveStreamCheckTest(t, timer.resets, "coalesced longer timer reset"), now.Add(900*time.Second); !got.Equal(want) {
		t.Fatalf("reset deadline = %v, want %v", got, want)
	}
	receiveStreamCheckTest(t, secondSweep, "coalesced due-only sweep")
	select {
	case <-fullChecks:
		t.Fatal("simultaneously ready obsolete timer started a full stream check")
	default:
	}

	stopStreamCheckLoopTest(t, cancel, done, timer)
}

func TestStreamCheckLoopAdoptsLatestIntervalAfterBlockedFullCheck(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.RateLimits.StreamCheckInterval = 60
	timer := newFakeStreamCheckTimer()
	now := time.Date(2026, 8, 22, 20, 30, 0, 0, time.UTC)
	fullCheckStarted := make(chan struct{})
	releaseFullCheck := make(chan struct{})
	fullChecks := make(chan struct{}, 2)
	unchecked := make(chan uncheckedStreamCheckCall, 1)
	cancel, done := startStreamCheckLoopTest(t, m, timer, now, func() {
		fullChecks <- struct{}{}
		close(fullCheckStarted)
		<-releaseFullCheck
	}, func(interval time.Duration, at time.Time) {
		unchecked <- uncheckedStreamCheckCall{interval: interval, now: at}
	})

	timer.ch <- now
	receiveStreamCheckTest(t, fullCheckStarted, "blocked full check")
	setStreamCheckIntervalForTest(m, 900)
	m.triggerStreamCheck()
	close(releaseFullCheck)

	if got, want := receiveStreamCheckTest(t, timer.resets, "post-check latest timer reset"), now.Add(900*time.Second); !got.Equal(want) {
		t.Fatalf("reset deadline = %v, want %v", got, want)
	}
	call := receiveStreamCheckTest(t, unchecked, "post-check coalesced sweep")
	if call.interval != 900*time.Second {
		t.Fatalf("unchecked interval = %v, want 15m0s", call.interval)
	}
	if got, want := m.GetNextStreamCheck(), now.Add(900*time.Second); !got.Equal(want) {
		t.Fatalf("GetNextStreamCheck after full check = %v, want %v", got, want)
	}
	select {
	case got := <-timer.resets:
		t.Fatalf("coalesced wake reset an already-current timer to %v", got)
	default:
	}
	receiveStreamCheckTest(t, fullChecks, "first full check record")
	select {
	case <-fullChecks:
		t.Fatal("interval change started an overlapping full check")
	default:
	}

	stopStreamCheckLoopTest(t, cancel, done, timer)
}

func TestStreamCheckLoopIntervalWakeProperty(t *testing.T) {
	const intervalDomain = 900 - 60 + 1
	property := func(oldRaw, newRaw uint16) bool {
		oldSeconds := 60 + int(oldRaw)%intervalDomain
		newSeconds := 60 + int(newRaw)%intervalDomain

		m, _, _ := newCapabilityMiner(t, "alpha")
		m.config.RateLimits.StreamCheckInterval = oldSeconds
		timer := newFakeStreamCheckTimer()
		now := time.Date(2026, 8, 22, 20, 35, 0, 0, time.UTC)
		unchecked := make(chan uncheckedStreamCheckCall, 1)
		fullChecks := make(chan struct{}, 1)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			m.runStreamCheckLoop(ctx, streamCheckLoopDeps{
				now:      func() time.Time { return now },
				newTimer: func(time.Time) streamCheckTimer { return timer },
				checkAll: func() { fullChecks <- struct{}{} },
				checkUnchecked: func(interval time.Duration, at time.Time) {
					unchecked <- uncheckedStreamCheckCall{interval: interval, now: at}
				},
			})
		}()
		defer func() {
			cancel()
			<-done
		}()

		select {
		case <-timer.selected:
		case <-time.After(2 * time.Second):
			return false
		}
		initialDeadline := m.GetNextStreamCheck()
		setStreamCheckIntervalForTest(m, newSeconds)
		m.triggerStreamCheck()

		var reset time.Time
		if oldSeconds != newSeconds {
			select {
			case reset = <-timer.resets:
			case <-time.After(2 * time.Second):
				return false
			}
		}
		var call uncheckedStreamCheckCall
		select {
		case call = <-unchecked:
		case <-time.After(2 * time.Second):
			return false
		}

		select {
		case <-fullChecks:
			return false
		default:
		}
		if call.interval != time.Duration(newSeconds)*time.Second || !call.now.Equal(now) {
			return false
		}
		if oldSeconds == newSeconds {
			select {
			case <-timer.resets:
				return false
			default:
			}
			return m.GetNextStreamCheck().Equal(initialDeadline)
		}
		return reset.Equal(now.Add(time.Duration(newSeconds)*time.Second)) &&
			m.GetNextStreamCheck().Equal(now.Add(time.Duration(newSeconds)*time.Second))
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 100}); err != nil {
		t.Fatal(err)
	}
}
