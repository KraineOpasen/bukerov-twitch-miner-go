package notifications

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// openRawNotifDB opens a PRIVATE, non-singleton database over its own file
// (mirrors internal/miner/srap_test.go's openRawMinerDB): unlike
// database.Open (a process-wide singleton — see testDBHandle's own doc
// comment), this handle can be closed by a single test without breaking
// every other test in this package that shares testDBHandle.
func openRawNotifDB(t *testing.T, path string) *database.DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	return &database.DB{DB: sqlDB}
}

// --- T13: Batcher loop lifecycle (A4) ---

// TestBatcherStopJoinsRunningLoopBeforeFinalFlush proves Stop's ordering
// contract: with a long interval (so the ticker never fires during the
// test), Stop must still deliver exactly the pending lines in ONE flush —
// its own, performed after joining the loop — never a second, concurrent
// loop-driven flush splitting the batch.
func TestBatcherStopJoinsRunningLoopBeforeFinalFlush(t *testing.T) {
	sink := newCaptureSink()
	cfg := BatchConfig{Enabled: true, Interval: time.Hour, MaxEntries: 20}
	b := NewBatcher("test", cfg, sink.send)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)

	for _, line := range []string{"one", "two"} {
		if err := b.Add(ctx, BatchEvent{Type: NotificationTypeOffline, Group: "streamerB", Line: line}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	b.Stop(context.Background())

	msgs := sink.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 flushed message from Stop, got %d: %v", len(msgs), msgs)
	}
	if msgs[0].Body != "one\ntwo" {
		t.Fatalf("unexpected flush body %q", msgs[0].Body)
	}

	// The flush-content assertion above is not, by itself, proof that Stop
	// actually joined the loop (a flush-only Stop, with no stopCh signal and
	// no join, would produce the exact same flushed content here — the loop
	// just keeps running in the background afterward). Assert the loop
	// actually exited: b.done is only closed when the goroutine's own defer
	// runs, on its way out of the select loop.
	select {
	case <-b.done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not exit after Stop")
	}

	// Calling Stop again must be safe and instant: the loop already exited
	// (done already closed), so this just re-joins immediately and re-flushes
	// an now-empty buffer — no new message, no hang.
	b.Stop(context.Background())
	if got := sink.count(); got != 1 {
		t.Fatalf("second Stop must not send anything new, got %d messages", got)
	}
}

// TestBatcherDoubleStartIsNoop proves the double-Start guard with two
// deterministic sub-cases (no time.Sleep-as-synchronization anywhere):
//
//   - "disabled": a disabled Batcher's Start closes b.done synchronously, in
//     the calling goroutine, with no loop to join — a naive implementation
//     missing the `started` guard would close(b.done) again on the second
//     call and panic immediately and deterministically (no scheduling
//     luck required; the recover below turns that panic into a clean
//     t.Fatalf instead of an opaque crash).
//   - "enabled": after the interval flush is observed (waitForCount, not a
//     sleep), b.Stop is called to JOIN the loop before asserting the final
//     count. Stop's join is itself a strong secondary check: closing stopCh
//     is seen by every listener on that channel, so an unguarded second
//     loop goroutine would ALSO race to `defer close(b.done)` and panic on
//     the double-close the instant Stop tries to join — deterministically,
//     not depending on a fixed sleep window ever being long enough.
func TestBatcherDoubleStartIsNoop(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("second Start on a disabled Batcher panicked (missing double-close(b.done) guard): %v", r)
			}
		}()

		cfg := BatchConfig{Enabled: false}
		b := NewBatcher("test", cfg, func(context.Context, Message) error { return nil })
		b.Start(context.Background())
		b.Start(context.Background()) // must be a no-op, not a second close(b.done)
	})

	t.Run("enabled", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Stop after a duplicate Start panicked (an unguarded second loop goroutine racing to close(b.done)): %v", r)
			}
		}()

		sink := newCaptureSink()
		cfg := BatchConfig{Enabled: true, Interval: 15 * time.Millisecond, MaxEntries: 20}
		b := NewBatcher("test", cfg, sink.send)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		b.Start(ctx)
		b.Start(ctx) // must be a no-op

		if err := b.Add(ctx, BatchEvent{Type: NotificationTypeOnline, Group: "g", Line: "x"}); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if !sink.waitForCount(1, time.Second) {
			t.Fatalf("expected the interval flush to still fire exactly once, got %d", sink.count())
		}

		// Join, don't sleep: by the time Stop returns, every loop goroutine
		// that could possibly exist (one with the guard intact, or more
		// without it) has either exited or made Stop itself panic above.
		b.Stop(ctx)

		if got := sink.count(); got != 1 {
			t.Fatalf("expected exactly 1 message even with a duplicate Start, got %d", got)
		}
	})
}

// TestBatcherCtxCancelExitsLoopAndFlushes proves the OTHER exit path (a
// plain context cancellation, no explicit Stop call): the loop must still
// exit and perform its own final flush.
func TestBatcherCtxCancelExitsLoopAndFlushes(t *testing.T) {
	sink := newCaptureSink()
	cfg := BatchConfig{Enabled: true, Interval: time.Hour, MaxEntries: 20}
	b := NewBatcher("test", cfg, sink.send)

	ctx, cancel := context.WithCancel(context.Background())
	b.Start(ctx)

	if err := b.Add(ctx, BatchEvent{Type: NotificationTypeOnline, Group: "g", Line: "pending"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	cancel()
	select {
	case <-b.done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not exit after ctx cancellation")
	}

	if !sink.waitForCount(1, time.Second) {
		t.Fatalf("expected the ctx-cancellation final flush, got %d messages", sink.count())
	}
}

// TestBatcherStopWithoutStartNeverHangs pins the never-started case
// explicitly (batch_test.go's TestBatcherStopFlushesPending already covers
// the same shape as a regression for the flush behavior; this test names
// the specific hang risk the `started` guard exists for): Stop on an
// enabled Batcher whose Start was never called must return promptly rather
// than blocking on channels nothing will ever close.
func TestBatcherStopWithoutStartNeverHangs(t *testing.T) {
	sink := newCaptureSink()
	cfg := BatchConfig{Enabled: true, Interval: time.Hour, MaxEntries: 20}
	b := NewBatcher("test", cfg, sink.send)

	if err := b.Add(context.Background(), BatchEvent{Type: NotificationTypeOnline, Group: "g", Line: "x"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	done := make(chan struct{})
	go func() {
		b.Stop(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop hung on a Batcher whose Start was never called")
	}
	if got := sink.count(); got != 1 {
		t.Fatalf("expected Stop to flush the pending line, got %d messages", got)
	}
}

// --- Manager.Start: batchers independent of Discord connect outcome (A3/6.5) ---

// TestManagerStartStartsBatchersDespiteDiscordConnectFailure pins the fix at
// manager.go's Start: before M4, a Discord Connect failure returned early,
// BEFORE the batcher-start loop ever ran, so every push provider's batcher
// stayed un-started for the rest of the process — any batched (non-
// immediate) event handed to Add would then buffer forever with nothing
// ever flushing it. Proven here by an interval-triggered flush: that can
// only fire if the loop goroutine was actually launched by Start.
func TestManagerStartStartsBatchersDespiteDiscordConnectFailure(t *testing.T) {
	m, _ := newManager(t, config.DiscordSettings{Enabled: true, BotToken: "tok", GuildID: "guild"})
	fake := &fakeDiscord{connected: false, botToken: "tok", guildID: "guild"}
	fake.connectErrs = []error{errors.New("boom")}
	m.discord = fake

	sink := newCaptureSink()
	b := NewBatcher("push", BatchConfig{Enabled: true, Interval: 20 * time.Millisecond, MaxEntries: 20}, sink.send)
	m.batchers = map[string]*Batcher{"push": b}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := m.Start(ctx); err == nil {
		t.Fatal("expected Start to surface the Discord connect error")
	}

	if err := b.Add(ctx, BatchEvent{Type: NotificationTypeOnline, Group: "g", Line: "x"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !sink.waitForCount(1, time.Second) {
		t.Fatalf("expected the batcher's interval-flush loop to be running despite the Discord connect failure, got %d messages", sink.count())
	}
}

// --- Manager.Stop idempotency (I5) ---

// TestManagerStopIsIdempotent proves Stop's stopOnce guard: a second call
// must not re-run Disconnect (or anything else) — the whole body only ever
// executes once.
func TestManagerStopIsIdempotent(t *testing.T) {
	m, _ := newManager(t, config.DiscordSettings{Enabled: true, BotToken: "tok", GuildID: "guild"})
	fake := &fakeDiscord{connected: true, botToken: "tok", guildID: "guild"}
	m.discord = fake

	m.Stop()
	m.Stop()
	m.Stop()

	_, disconnect, _, connected := fake.counts()
	if disconnect != 1 {
		t.Fatalf("expected exactly 1 Disconnect across 3 Stop calls, got %d", disconnect)
	}
	if connected {
		t.Fatal("provider should report not connected after Stop")
	}
}

// --- notifConfig deep copy (I7) ---

// TestNewManagerDeepCopiesNotificationsConfig proves NewManager never
// retains the caller's *config.NotificationsSettings (nor its
// ProviderBatching map, nor any entry's ImmediateEvents slice): mutating the
// caller's struct AFTER construction must not change what
// resolveBatchingSettings already resolved.
func TestNewManagerDeepCopiesNotificationsConfig(t *testing.T) {
	notifCfg := &config.NotificationsSettings{
		Batching: config.BatchingSettings{Enabled: true, Interval: "10m", MaxEntries: 5, ImmediateEvents: []string{"drop_claim"}},
		ProviderBatching: map[string]config.BatchingSettings{
			"webhook": {Enabled: false, Interval: "1h", ImmediateEvents: []string{"bet_win"}},
		},
	}
	m, err := NewManager(&config.DiscordSettings{}, notifCfg, testDBHandle, nil, "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Mutate the caller's config in place AFTER construction — a Manager
	// that aliased any of this must observe the change; one that deep-copied
	// must not.
	notifCfg.Batching.Interval = "999h"
	notifCfg.Batching.ImmediateEvents[0] = "mutated"
	notifCfg.ProviderBatching["webhook"] = config.BatchingSettings{Enabled: true, Interval: "mutated"}
	delete(notifCfg.ProviderBatching, "webhook")

	got := m.resolveBatchingSettings("some-other-provider")
	if got.Interval != "10m" {
		t.Fatalf("global Batching.Interval = %q, want unaffected %q", got.Interval, "10m")
	}
	if len(got.ImmediateEvents) != 1 || got.ImmediateEvents[0] != "drop_claim" {
		t.Fatalf("global Batching.ImmediateEvents = %v, want unaffected [\"drop_claim\"]", got.ImmediateEvents)
	}

	gotOverride := m.resolveBatchingSettings("webhook")
	if gotOverride.Interval != "1h" {
		t.Fatalf("provider override Interval = %q, want unaffected %q (the caller's post-construction delete/overwrite must not be observed)", gotOverride.Interval, "1h")
	}
}

// TestNewManagerNilNotificationsConfigFallsBackToDefaults preserves the
// pre-M4 behavior for a nil notifCfg (some tests, and any future caller that
// has no batching config to supply): resolveBatchingSettings must still
// return the built-in defaults, not a zero-value BatchConfig.
func TestNewManagerNilNotificationsConfigFallsBackToDefaults(t *testing.T) {
	m, err := NewManager(&config.DiscordSettings{}, nil, testDBHandle, nil, "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	got := m.resolveBatchingSettings("anyprovider")
	want := config.DefaultBatchingSettings()
	if got.Interval != want.Interval || got.MaxEntries != want.MaxEntries || got.Enabled != want.Enabled {
		t.Fatalf("resolveBatchingSettings with nil notifCfg = %v, want defaults %v", got, want)
	}
}

// --- NotifyOnline/NotifyOffline inert guard (A8) ---

// TestNotifyOnlineOfflineInertGuardSkipsRepoWhenNothingConfigured proves the
// A8 guard structurally: with Discord disabled and no push providers, the
// underlying DB is closed AFTER construction so any repo.GetConfig() call
// would fail and log "Failed to get notification config" — the guard's
// whole point is that this call never happens when nobody could possibly be
// notified.
func TestNotifyOnlineOfflineInertGuardSkipsRepoWhenNothingConfigured(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "inert.db")
	db := openRawNotifDB(t, dbPath)

	m, err := NewManager(&config.DiscordSettings{}, nil, db, nil, "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	buf := captureLogs(t)
	m.NotifyOnline("streamerA")
	m.NotifyOffline("streamerA")

	if strings.Contains(buf.String(), "Failed to get notification config") {
		t.Fatalf("inert guard did not skip repo.GetConfig with nothing configured: %s", buf.String())
	}
}

// TestNotifyOnlineStillReadsConfigWhenDiscordEnabled is the negative control
// for the guard above: with Discord ENABLED (even though unconnected), the
// normal path must still run and hit the (now-closed) repo, proving the
// guard only fires when truly nothing is configured, not whenever the DB
// happens to be unavailable.
func TestNotifyOnlineStillReadsConfigWhenDiscordEnabled(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "control.db")
	db := openRawNotifDB(t, dbPath)

	m, err := NewManager(&config.DiscordSettings{Enabled: true, BotToken: "tok", GuildID: "guild"}, nil, db, nil, "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	buf := captureLogs(t)
	m.NotifyOnline("streamerA")

	if !strings.Contains(buf.String(), "Failed to get notification config") {
		t.Fatalf("expected the normal (non-guarded) path to still read the config and fail loudly on a closed DB, got: %s", buf.String())
	}
}
