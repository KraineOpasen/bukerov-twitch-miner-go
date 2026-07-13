package notifications

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// captureSink records every message passed to it in a goroutine-safe way.
type captureSink struct {
	mu       sync.Mutex
	messages []Message
	signal   chan struct{}
}

func newCaptureSink() *captureSink {
	return &captureSink{signal: make(chan struct{}, 64)}
}

func (c *captureSink) send(_ context.Context, msg Message) error {
	c.mu.Lock()
	c.messages = append(c.messages, msg)
	c.mu.Unlock()
	select {
	case c.signal <- struct{}{}:
	default:
	}
	return nil
}

func (c *captureSink) snapshot() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Message, len(c.messages))
	copy(out, c.messages)
	return out
}

func (c *captureSink) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.messages)
}

// waitForCount blocks until at least n messages have been captured or the
// timeout elapses.
func (c *captureSink) waitForCount(n int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		if c.count() >= n {
			return true
		}
		select {
		case <-c.signal:
		case <-deadline:
			return c.count() >= n
		}
	}
}

func TestBatcherManualFlushJoinsLines(t *testing.T) {
	sink := newCaptureSink()
	cfg := BatchConfig{Enabled: true, Interval: time.Hour, MaxEntries: 20}
	b := NewBatcher("test", cfg, sink.send)

	ctx := context.Background()
	for _, line := range []string{"line1", "line2", "line3"} {
		if err := b.Add(ctx, BatchEvent{Type: NotificationTypeOnline, Group: "streamerA", Line: line}); err != nil {
			t.Fatalf("Add returned error: %v", err)
		}
	}

	// Nothing should have been sent before the flush.
	if got := sink.count(); got != 0 {
		t.Fatalf("expected 0 messages before flush, got %d", got)
	}

	b.Flush(ctx)

	msgs := sink.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 batched message, got %d", len(msgs))
	}
	wantBody := "line1\nline2\nline3"
	if msgs[0].Body != wantBody {
		t.Fatalf("unexpected body:\n got: %q\nwant: %q", msgs[0].Body, wantBody)
	}
	if !strings.Contains(msgs[0].Title, "streamerA") {
		t.Fatalf("expected title to reference the group, got %q", msgs[0].Title)
	}
}

func TestBatcherFlushOnInterval(t *testing.T) {
	sink := newCaptureSink()
	cfg := BatchConfig{Enabled: true, Interval: 20 * time.Millisecond, MaxEntries: 20}
	b := NewBatcher("test", cfg, sink.send)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)

	if err := b.Add(ctx, BatchEvent{Type: NotificationTypeOnline, Group: "streamerA", Line: "hello"}); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}

	if !sink.waitForCount(1, time.Second) {
		t.Fatalf("expected interval flush to send 1 message, got %d", sink.count())
	}

	msgs := sink.snapshot()
	if msgs[0].Body != "hello" {
		t.Fatalf("unexpected body %q", msgs[0].Body)
	}
}

func TestBatcherSplitsOnMaxEntries(t *testing.T) {
	sink := newCaptureSink()
	cfg := BatchConfig{Enabled: true, Interval: time.Hour, MaxEntries: 2}
	b := NewBatcher("test", cfg, sink.send)

	ctx := context.Background()
	lines := []string{"a", "b", "c", "d", "e"}
	for _, line := range lines {
		if err := b.Add(ctx, BatchEvent{Type: NotificationTypeOnline, Group: "streamerA", Line: line}); err != nil {
			t.Fatalf("Add returned error: %v", err)
		}
	}

	b.Flush(ctx)

	msgs := sink.snapshot()
	// 5 lines with max 2 per message => 3 messages (2, 2, 1).
	if len(msgs) != 3 {
		t.Fatalf("expected 3 split messages, got %d", len(msgs))
	}
	wantBodies := []string{"a\nb", "c\nd", "e"}
	for i, want := range wantBodies {
		if msgs[i].Body != want {
			t.Fatalf("message %d body = %q, want %q", i, msgs[i].Body, want)
		}
	}
}

func TestBatcherImmediateEventsBypass(t *testing.T) {
	sink := newCaptureSink()
	cfg := BatchConfig{
		Enabled:         true,
		Interval:        time.Hour,
		MaxEntries:      20,
		ImmediateEvents: map[NotificationType]bool{NotificationTypeDropClaim: true},
	}
	b := NewBatcher("test", cfg, sink.send)

	ctx := context.Background()

	// A batched event is buffered (no send yet).
	if err := b.Add(ctx, BatchEvent{Type: NotificationTypeOnline, Group: "streamerA", Line: "buffered"}); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if got := sink.count(); got != 0 {
		t.Fatalf("expected buffered event not to send, got %d messages", got)
	}

	// An immediate event is delivered right away, bypassing the buffer.
	if err := b.Add(ctx, BatchEvent{Type: NotificationTypeDropClaim, Group: "streamerA", Line: "claimed a drop"}); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}

	msgs := sink.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 immediate message, got %d", len(msgs))
	}
	if msgs[0].Body != "claimed a drop" {
		t.Fatalf("unexpected immediate body %q", msgs[0].Body)
	}
	if msgs[0].Type != NotificationTypeDropClaim {
		t.Fatalf("unexpected immediate type %q", msgs[0].Type)
	}
}

func TestBatcherDisabledSendsImmediately(t *testing.T) {
	sink := newCaptureSink()
	cfg := BatchConfig{Enabled: false, Interval: time.Hour, MaxEntries: 20}
	b := NewBatcher("test", cfg, sink.send)

	ctx := context.Background()
	if err := b.Add(ctx, BatchEvent{Type: NotificationTypeOnline, Group: "streamerA", Line: "live"}); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}

	if got := sink.count(); got != 1 {
		t.Fatalf("expected immediate send when batching disabled, got %d", got)
	}
}

func TestBatcherStopFlushesPending(t *testing.T) {
	sink := newCaptureSink()
	cfg := BatchConfig{Enabled: true, Interval: time.Hour, MaxEntries: 20}
	b := NewBatcher("test", cfg, sink.send)

	ctx := context.Background()
	for _, line := range []string{"one", "two"} {
		if err := b.Add(ctx, BatchEvent{Type: NotificationTypeOffline, Group: "streamerB", Line: line}); err != nil {
			t.Fatalf("Add returned error: %v", err)
		}
	}

	b.Stop(ctx)

	msgs := sink.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 flushed message on stop, got %d", len(msgs))
	}
	if msgs[0].Body != "one\ntwo" {
		t.Fatalf("unexpected body %q", msgs[0].Body)
	}
}

func TestNewBatchConfigParsesSettings(t *testing.T) {
	cfg := NewBatchConfig(config.BatchingSettings{
		Enabled:         true,
		Interval:        "45m",
		MaxEntries:      10,
		ImmediateEvents: []string{"drop_claim", "bet_win"},
	})

	if !cfg.Enabled {
		t.Fatal("expected Enabled true")
	}
	if cfg.Interval != 45*time.Minute {
		t.Fatalf("expected 45m interval, got %v", cfg.Interval)
	}
	if cfg.MaxEntries != 10 {
		t.Fatalf("expected MaxEntries 10, got %d", cfg.MaxEntries)
	}
	if !cfg.ImmediateEvents[NotificationTypeDropClaim] || !cfg.ImmediateEvents[NotificationTypeBetWin] {
		t.Fatalf("expected immediate events to include drop_claim and bet_win, got %v", cfg.ImmediateEvents)
	}
}

func TestNewBatchConfigInvalidIntervalFallsBack(t *testing.T) {
	cfg := NewBatchConfig(config.BatchingSettings{Enabled: true, Interval: "not-a-duration"})
	if cfg.Interval != defaultBatchInterval {
		t.Fatalf("expected fallback to %v, got %v", defaultBatchInterval, cfg.Interval)
	}
}
