package notifications

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
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
