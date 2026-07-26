package notifications

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// TestBatcherFlushLogsSendErrorWithoutSentinel is scenario (a): a conforming
// provider's send func returns a *SendError built from a sentinel-bearing
// *url.Error. Flush's log line must carry the provider attr and the
// SendError's stage/class, but none of the sentinel components anywhere.
func TestBatcherFlushLogsSendErrorWithoutSentinel(t *testing.T) {
	buf := captureLogs(t)

	sendErr := newTransportError("webhook", "send", &url.Error{
		Op: "Post", URL: sentinelWebhookURL,
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
	})
	send := func(context.Context, Message) error { return sendErr }

	cfg := BatchConfig{Enabled: true, Interval: time.Hour, MaxEntries: 20}
	b := NewBatcher("webhook", cfg, send)

	ctx := context.Background()
	if err := b.Add(ctx, BatchEvent{Type: NotificationTypeOnline, Group: "streamerA", Line: "line1"}); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	b.Flush(ctx)

	out := buf.String()
	assertNoSentinel(t, out, allSentinels...)
	if !strings.Contains(out, "provider=webhook") {
		t.Errorf("expected a provider=webhook attr in the log line: %s", out)
	}
	if !strings.Contains(out, "error.stage=transport") {
		t.Errorf("expected the SendError's stage to be logged: %s", out)
	}
	if !strings.Contains(out, "error.class=connect") {
		t.Errorf("expected the SendError's class to be logged: %s", out)
	}
}

// TestBatcherFlushLogsRegressedProviderWithoutSentinel is scenario (b): a
// regressed provider returns a raw fmt.Errorf("%w", urlErr)-wrapped error
// instead of a *SendError. safeSendErrorAttr must fail closed to the
// "unclassified provider send failure" summary plus a bare type name, and
// still leak nothing.
func TestBatcherFlushLogsRegressedProviderWithoutSentinel(t *testing.T) {
	buf := captureLogs(t)

	regressed := fmt.Errorf("webhook request failed: %w", &url.Error{
		Op: "Post", URL: sentinelWebhookURL, Err: errors.New(sentinelMain),
	})
	send := func(context.Context, Message) error { return regressed }

	cfg := BatchConfig{Enabled: true, Interval: time.Hour, MaxEntries: 20}
	b := NewBatcher("webhook", cfg, send)

	ctx := context.Background()
	if err := b.Add(ctx, BatchEvent{Type: NotificationTypeOnline, Group: "streamerA", Line: "line1"}); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	b.Flush(ctx)

	out := buf.String()
	assertNoSentinel(t, out, allSentinels...)
	if !strings.Contains(out, "unclassified provider send failure") {
		t.Errorf("expected the fail-closed summary: %s", out)
	}
	if !strings.Contains(out, "error.type=*fmt.wrapError") {
		t.Errorf("expected the bare Go type name of the regressed error: %s", out)
	}
}

// TestBatcherFlushLogNonEmptyNegativeControl is scenario (c): the negative
// control proving the assertions above aren't vacuously true because nothing
// was logged — the flush failure line is non-empty and still names the
// provider and the failed operation.
func TestBatcherFlushLogNonEmptyNegativeControl(t *testing.T) {
	buf := captureLogs(t)

	sendErr := newResponseError("gotify", "send", 500)
	send := func(context.Context, Message) error { return sendErr }

	cfg := BatchConfig{Enabled: true, Interval: time.Hour, MaxEntries: 20}
	b := NewBatcher("gotify", cfg, send)

	ctx := context.Background()
	if err := b.Add(ctx, BatchEvent{Type: NotificationTypeOnline, Group: "streamerA", Line: "line1"}); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	b.Flush(ctx)

	out := buf.String()
	if out == "" {
		t.Fatal("expected a non-empty log line on flush failure")
	}
	if !strings.Contains(out, "provider=gotify") {
		t.Errorf("expected the provider attr: %s", out)
	}
	if !strings.Contains(out, "error.op=send") {
		t.Errorf("expected the failed operation to be logged: %s", out)
	}
}

// syncWriter is a mutex-guarded io.Writer wrapping a bytes.Buffer. A plain
// bytes.Buffer is not safe for concurrent use: dispatchPush logs from a
// spawned goroutine, so a test polling the buffer's content from a different
// goroutine needs a writer whose Write and String are both synchronized.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// captureLogsConcurrentSafe mirrors captureLogs (discord_config_test.go) —
// same save/restore-slog.Default idiom — but backs the handler with a
// *syncWriter instead of a *bytes.Buffer, so it is safe to read from a
// goroutine other than the one slog writes from.
func captureLogsConcurrentSafe(t *testing.T) *syncWriter {
	t.Helper()
	w := &syncWriter{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return w
}

// TestDispatchPushLogsRegressedProviderWithoutSentinel is dispatchPush's
// counterpart to TestBatcherFlushLogsRegressedProviderWithoutSentinel: the
// same fail-closed safeSendErrorAttr call exists at manager.go's
// dispatchPush, on its own, independent code path (a goroutine per batcher,
// not Flush), and had no regression test of its own. A regressed provider
// returning a raw fmt.Errorf("%w", urlErr) instead of a *SendError must still
// log only the "unclassified provider send failure" summary plus a bare type
// name — never the sentinel-bearing URL wrapped inside it.
func TestDispatchPushLogsRegressedProviderWithoutSentinel(t *testing.T) {
	buf := captureLogsConcurrentSafe(t)

	regressed := fmt.Errorf("webhook request failed: %w", &url.Error{
		Op: "Post", URL: sentinelWebhookURL, Err: errors.New("refused"),
	})
	send := func(context.Context, Message) error { return regressed }

	m, _ := newManager(t, config.DiscordSettings{})
	// Batching disabled -> Batcher.Add sends immediately and returns the send
	// error, exercising dispatchPush's goroutine/log path rather than Flush's.
	b := NewBatcher("webhook", BatchConfig{Enabled: false}, send)
	m.batchers = map[string]*Batcher{"webhook": b}

	m.dispatchPush(NotificationTypeOnline, "streamerA", "line1")

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(buf.String(), "Failed to dispatch push notification") {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the dispatch failure to be logged; log so far: %s", buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	out := buf.String()
	if !strings.Contains(out, "provider=webhook") {
		t.Errorf("expected a provider=webhook attr in the log line: %s", out)
	}
	if !strings.Contains(out, "unclassified provider send failure") {
		t.Errorf("expected the fail-closed summary: %s", out)
	}
	assertNoSentinel(t, out, allSentinels...)
}
