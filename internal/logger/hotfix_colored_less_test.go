package logger

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// This file pins the v0.13.7 logger hotfix (§7): `colored` gates ONLY the ANSI
// color envelope on stdout (emoji stay regardless, the file stays plain), and the
// dead `less` setting never changes what reaches the console.

// captureStdout redirects os.Stdout to a pipe for the duration of fn (which must
// Setup, log, and Close its logger so the async console writer flushes), then
// returns everything written to stdout. It saves/restores slog.Default too.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	prev := slog.Default()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old; slog.SetDefault(prev) }()

	fn()

	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String()
}

// setupAndLog runs Setup (file logging off) with s, emits records, flushes, and
// returns the captured stdout bytes.
func setupAndLog(t *testing.T, s config.LoggerSettings, records func()) string {
	t.Helper()
	return captureStdout(t, func() {
		l, err := Setup("hotfixtester", s)
		if err != nil {
			t.Errorf("Setup: %v", err)
			return
		}
		records()
		l.Close()
	})
}

// §8.27: colored=true stdout carries ANSI escape sequences (and the emoji).
func TestColoredStdoutHasANSI(t *testing.T) {
	out := setupAndLog(t, config.LoggerSettings{ConsoleLevel: "INFO", Colored: true}, func() {
		slog.Info("Streamer is online", "streamer", "shroud")
	})
	if !strings.Contains(out, "\033[") {
		t.Errorf("colored=true stdout must contain ANSI escapes, got %q", out)
	}
	if !strings.Contains(out, "🟢") {
		t.Errorf("colored=true stdout must contain the mapped emoji, got %q", out)
	}
}

// §8.28 + §8.31: colored=false stdout has NO ANSI, but KEEPS the emoji and never
// loses the message/attributes.
func TestUncoloredStdoutHasNoANSIKeepsEmoji(t *testing.T) {
	out := setupAndLog(t, config.LoggerSettings{ConsoleLevel: "INFO", Colored: false}, func() {
		slog.Info("Streamer is online", "streamer", "shroud")
	})
	if strings.Contains(out, "\033[") {
		t.Errorf("colored=false stdout must contain no ANSI escapes, got %q", out)
	}
	if !strings.Contains(out, "🟢") {
		t.Errorf("colored=false stdout must still carry the emoji (independent of ANSI), got %q", out)
	}
	if !strings.Contains(out, "streamer=shroud") {
		t.Errorf("colored=false stdout must preserve message attributes, got %q", out)
	}
}

// §8.30: WARN/ERROR records keep their level, message and attributes at both
// color settings.
func TestWarnErrorAttrsPreserved(t *testing.T) {
	for _, colored := range []bool{true, false} {
		var sink syncBuffer
		h := newConsoleHandler(&sink, slog.LevelDebug, colored)
		rec := slog.NewRecord(time.Now(), slog.LevelWarn, "rate limited", 0)
		rec.Add("streamer", "shroud", "status", 429)
		_ = h.Handle(context.Background(), rec)
		h.cw.Close()

		out := sink.String()
		for _, want := range []string{"level=WARN", "rate limited", "streamer=shroud", "status=429", "⚠️"} {
			if !strings.Contains(out, want) {
				t.Errorf("colored=%v: WARN output missing %q, got %q", colored, want, out)
			}
		}
	}
}

// §8.35: the retained-but-ignored `less` field never changes what levels reach
// stdout. less=true and less=false produce identical level gating: an INFO line
// is suppressed at ConsoleLevel=WARN either way, and the WARN line is emitted
// either way.
func TestLessDoesNotChangeLevels(t *testing.T) {
	run := func(less bool) string {
		return setupAndLog(t, config.LoggerSettings{ConsoleLevel: "WARN", Less: less}, func() {
			slog.Info("suppressed info line")
			slog.Warn("kept warn line")
		})
	}
	withLess := run(true)
	withoutLess := run(false)

	for label, out := range map[string]string{"less=true": withLess, "less=false": withoutLess} {
		if strings.Contains(out, "suppressed info line") {
			t.Errorf("%s: ConsoleLevel=WARN must suppress INFO regardless of `less`, got %q", label, out)
		}
		if !strings.Contains(out, "kept warn line") {
			t.Errorf("%s: ConsoleLevel=WARN must keep WARN regardless of `less`, got %q", label, out)
		}
	}
}
