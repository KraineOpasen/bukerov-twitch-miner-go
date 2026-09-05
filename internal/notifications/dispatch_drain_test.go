package notifications

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// blockingDiscord is a network-free discordProvider whose Send blocks until
// the test releases it, simulating an in-flight (and, unless told otherwise,
// non-cancellable) network delivery. sendStarted receives one value per Send
// entry so a test can synchronize on "the dispatch goroutine is inside the
// network call" without sleeping.
type blockingDiscord struct {
	sendStarted chan struct{}
	release     chan struct{}
	// honorCtx makes Send return ctx.Err() as soon as its context is
	// cancelled, modelling a provider that supports cancellation.
	honorCtx  bool
	sendCount atomic.Int32
	ctxErrs   atomic.Int32
}

func newBlockingDiscord() *blockingDiscord {
	return &blockingDiscord{
		sendStarted: make(chan struct{}, 8),
		release:     make(chan struct{}),
	}
}

func (b *blockingDiscord) Connect(context.Context) error { return nil }
func (b *blockingDiscord) Disconnect() error             { return nil }
func (b *blockingDiscord) UpdateConfig(_, _ string)      {}
func (b *blockingDiscord) IsConnected() bool             { return true }
func (b *blockingDiscord) GetChannels(context.Context, bool) ([]Channel, error) {
	return nil, nil
}

func (b *blockingDiscord) Send(ctx context.Context, _ Notification) error {
	b.sendCount.Add(1)
	b.sendStarted <- struct{}{}
	if b.honorCtx {
		select {
		case <-b.release:
			return nil
		case <-ctx.Done():
			b.ctxErrs.Add(1)
			return ctx.Err()
		}
	}
	<-b.release
	return nil
}

// newDrainTestManager builds a Manager over a private, non-singleton SQLite
// file with the blocking fake installed as the Discord provider, a points
// channel configured, and one un-triggered point rule at the given threshold.
func newDrainTestManager(t *testing.T, fake *blockingDiscord, rules ...*PointRule) *Manager {
	t.Helper()

	db := openRawNotifDB(t, filepath.Join(t.TempDir(), "drain.db"))
	m, err := NewManager(&config.DiscordSettings{Enabled: true, BotToken: "tok", GuildID: "g"}, nil, db, nil, "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.mu.Lock()
	m.discord = fake
	m.mu.Unlock()

	if err := m.SaveConfig(&NotificationConfig{PointsChannelID: "points-channel"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	for _, r := range rules {
		if err := m.AddPointRule(r); err != nil {
			t.Fatalf("AddPointRule: %v", err)
		}
	}
	return m
}

// crossThreshold drives NotifyPointsReached twice so the second call crosses
// the rule threshold and spawns the dispatch writer (send, then the
// point_rule.triggered persistence).
func crossThreshold(m *Manager, streamer string, threshold int) {
	m.NotifyPointsReached(streamer, threshold-1)
	m.NotifyPointsReached(streamer, threshold+1)
}

// mustFindRule returns the rule with the given ID, failing the test when it
// is absent.
func mustFindRule(t *testing.T, m *Manager, id int64) PointRule {
	t.Helper()
	rules, err := m.GetPointRules()
	if err != nil {
		t.Fatalf("GetPointRules: %v", err)
	}
	for _, r := range rules {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("rule %d not found", id)
	return PointRule{}
}

// TestStopWaitsForAdmittedPointRuleWrite is the S1 flagship ordering
// regression. The point-goal dispatch goroutine performs a network send and
// THEN persists point_rule.triggered; Manager.Stop returning is what lets
// Miner.stop() — and therefore Miner.Run — return, after which App.Shutdown
// closes the shared SQLite handle. If Stop returns while an admitted
// dispatch is still in flight, that persistence races the database close:
// the write lands on a closed handle ("sql: database is closed") and the
// triggered state is lost.
//
// The test admits one writer (Send held open by the fake), starts Stop, and
// proves ordering: Stop must still be draining while the writer is in
// flight, and once the writer is released Stop must return only after the
// triggered mark is durably persisted.
func TestStopWaitsForAdmittedPointRuleWrite(t *testing.T) {
	fake := newBlockingDiscord()
	rule := &PointRule{Streamer: "streamerx", Threshold: 100}
	m := newDrainTestManager(t, fake, rule)

	crossThreshold(m, "streamerx", 100)
	<-fake.sendStarted // the admitted writer is inside the network send

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- m.Stop()
	}()

	// Negative assertion, repo stop_join_test precedent: give a
	// non-draining Stop ample time to expose itself. A draining Stop can
	// never resolve stopDone here because the writer is still blocked in
	// Send (the fake ignores cancellation, modelling non-cancellable I/O).
	select {
	case <-stopDone:
		t.Fatal("Stop returned while an admitted point-rule dispatch was still in flight — its point_rule.triggered write can land after the shared SQLite handle closes")
	case <-time.After(300 * time.Millisecond):
	}

	close(fake.release)

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop after a completed drain = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the admitted dispatch completed")
	}

	// Stop returned only after the admitted writer finished: the triggered
	// mark is already durable, before any owner could close the database.
	if got := mustFindRule(t, m, rule.ID); !got.Triggered {
		t.Fatalf("rule %d not marked triggered after Stop returned; the persistence was left in flight", rule.ID)
	}
	if n := fake.sendCount.Load(); n != 1 {
		t.Fatalf("sendCount = %d, want 1", n)
	}
}

// TestStopDrainsMultipleAdmittedWriters proves every admitted dispatch is
// accounted for: two rules on two streamers both cross their thresholds, both
// writers are held inside Send, and after release Stop returns only once both
// point_rule.triggered marks are durable.
func TestStopDrainsMultipleAdmittedWriters(t *testing.T) {
	fake := newBlockingDiscord()
	ruleA := &PointRule{Streamer: "streamera", Threshold: 100}
	ruleB := &PointRule{Streamer: "streamerb", Threshold: 200}
	m := newDrainTestManager(t, fake, ruleA, ruleB)

	crossThreshold(m, "streamera", 100)
	crossThreshold(m, "streamerb", 200)
	<-fake.sendStarted
	<-fake.sendStarted // both admitted writers are inside their sends

	close(fake.release)
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := mustFindRule(t, m, ruleA.ID); !got.Triggered {
		t.Fatalf("rule %d (streamera) not triggered after Stop", ruleA.ID)
	}
	if got := mustFindRule(t, m, ruleB.ID); !got.Triggered {
		t.Fatalf("rule %d (streamerb) not triggered after Stop", ruleB.ID)
	}
}

// TestNotifyAfterStopSpawnsNoDispatch proves admission closes when draining
// begins: a points event arriving after Stop must not schedule a dispatch
// goroutine (goDispatch refuses synchronously), so nothing can reach the
// repository once the owner proceeds to close the database.
func TestNotifyAfterStopSpawnsNoDispatch(t *testing.T) {
	fake := newBlockingDiscord()
	close(fake.release) // nothing should ever block; releases defensively
	rule := &PointRule{Streamer: "streamerx", Threshold: 100}
	m := newDrainTestManager(t, fake, rule)

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	crossThreshold(m, "streamerx", 100)

	if n := fake.sendCount.Load(); n != 0 {
		t.Fatalf("sendCount = %d after Stop, want 0 — a dispatch was scheduled after draining began", n)
	}
	if got := mustFindRule(t, m, rule.ID); got.Triggered {
		t.Fatalf("rule %d was marked triggered by a post-Stop dispatch", rule.ID)
	}
}

// TestStopReturnsDespiteHungDispatch proves the drain is bounded: a provider
// that ignores its context and never returns cannot hold shutdown open past
// dispatchDrainTimeout (shrunk via the package var, the stop_join_test
// precedent).
func TestStopReturnsDespiteHungDispatch(t *testing.T) {
	old := dispatchDrainTimeout
	dispatchDrainTimeout = 100 * time.Millisecond
	defer func() { dispatchDrainTimeout = old }()

	fake := newBlockingDiscord() // release never closed: the send hangs forever
	rule := &PointRule{Streamer: "streamerx", Threshold: 100}
	m := newDrainTestManager(t, fake, rule)

	crossThreshold(m, "streamerx", 100)
	<-fake.sendStarted

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- m.Stop()
	}()

	select {
	case err := <-done:
		if elapsed := time.Since(start); elapsed < dispatchDrainTimeout {
			t.Fatalf("Stop returned before the drain timeout (%v) — did it wait at all?", elapsed)
		}
		if !errors.Is(err, errDispatchDrainTimeout) {
			t.Fatalf("Stop on a hung writer = %v, want errDispatchDrainTimeout — the timeout must be an explicit shutdown error", err)
		}
		// A repeated Stop reports the same explicit outcome.
		if err2 := m.Stop(); !errors.Is(err2, errDispatchDrainTimeout) {
			t.Fatalf("repeated Stop = %v, want the first call's drain error", err2)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked far beyond the drain timeout — hung-writer protection missing")
	}

	// Unblock the leaked goroutine so the race detector sees it exit cleanly.
	close(fake.release)
}

// TestStopCancelsInFlightSend proves the cancel-before-wait ordering: a
// provider that honors its context returns as soon as the drain starts, so
// Stop completes well inside the drain budget instead of consuming it.
func TestStopCancelsInFlightSend(t *testing.T) {
	fake := newBlockingDiscord()
	fake.honorCtx = true // release stays open: only cancellation can free Send
	rule := &PointRule{Streamer: "streamerx", Threshold: 100}
	m := newDrainTestManager(t, fake, rule)

	crossThreshold(m, "streamerx", 100)
	<-fake.sendStarted

	// Must not hang: drainDispatch cancels dispatchCtx before waiting, so
	// the ctx-honoring send returns and the drain completes cleanly.
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if n := fake.ctxErrs.Load(); n != 1 {
		t.Fatalf("cancelled sends = %d, want 1 — dispatchCtx was not cancelled before the wait", n)
	}
	// A cancelled send delivered nothing, so the rule must stay untriggered
	// (at-least-once semantics: it re-fires after the next start).
	if got := mustFindRule(t, m, rule.ID); got.Triggered {
		t.Fatalf("rule %d marked triggered although its send was cancelled", rule.ID)
	}
}

// TestStopIdempotentUnderConcurrentCallers proves repeated shutdown stays
// safe: concurrent and repeated Stops all return, the drain runs once, and no
// panic or double-close occurs.
func TestStopIdempotentUnderConcurrentCallers(t *testing.T) {
	fake := newBlockingDiscord()
	close(fake.release)
	m := newDrainTestManager(t, fake)

	done := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			done <- m.Stop()
		}()
	}
	for i := 0; i < 3; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("concurrent Stop = %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent Stop did not return")
		}
	}
	if err := m.Stop(); err != nil { // a later repeat is a defined no-op
		t.Fatalf("repeated Stop = %v, want nil", err)
	}
}

// TestGracefulShutdownPersistsBeforeDatabaseClose is the composed S1
// sequence at this seam: admitted writer → release → Stop (drain) → database
// Close. The write must have landed (triggered persisted, no
// "sql: database is closed" failure path taken) and the close must succeed
// exactly once after the drain.
func TestGracefulShutdownPersistsBeforeDatabaseClose(t *testing.T) {
	fake := newBlockingDiscord()
	rule := &PointRule{Streamer: "streamerx", Threshold: 100}

	db := openRawNotifDB(t, filepath.Join(t.TempDir(), "drain-close.db"))
	m, err := NewManager(&config.DiscordSettings{Enabled: true, BotToken: "tok", GuildID: "g"}, nil, db, nil, "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.mu.Lock()
	m.discord = fake
	m.mu.Unlock()
	if err := m.SaveConfig(&NotificationConfig{PointsChannelID: "points-channel"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if err := m.AddPointRule(rule); err != nil {
		t.Fatalf("AddPointRule: %v", err)
	}

	// Capture logging for the whole controlled shutdown sequence: a
	// graceful drain must never produce the "database is closed" failure
	// this task exists to eliminate.
	var logBuf syncLogBuffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	crossThreshold(m, "streamerx", 100)
	<-fake.sendStarted
	close(fake.release)

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The drain completed, so the persistence is durable BEFORE the handle
	// closes — the ordering App.Shutdown relies on.
	if got := mustFindRule(t, m, rule.ID); !got.Triggered {
		t.Fatalf("rule %d not triggered before database close", rule.ID)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close after drain: %v", err)
	}

	if logged := logBuf.String(); strings.Contains(logged, "database is closed") {
		t.Fatalf("controlled graceful shutdown produced a 'database is closed' failure:\n%s", logged)
	}
}

// syncLogBuffer is a concurrency-safe io.Writer for capturing slog output
// from dispatch goroutines (the pubsub lockedBuffer precedent).
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
