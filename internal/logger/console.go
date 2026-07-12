package logger

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
)

// ANSI color codes used for the console output. Only the console handler ever
// emits these — the file handler writes plain text so /debug/log stays clean.
const (
	ansiReset = "\033[0m"

	ansiRed      = "\033[31m"       // errors, offline, lost bets
	ansiGreen    = "\033[32m"       // streamer online, won bets
	ansiYellow   = "\033[33m"       // gains: points earned / claiming bonus
	ansiBlue     = "\033[34m"       // placing prediction bet
	ansiMagenta  = "\033[35m"       // bets filtered / skipped
	ansiGray     = "\033[90m"       // muted startup / bookkeeping events
	ansiSoftBlue = "\033[38;5;110m" // prediction confirmed (muted blue)
	ansiAmber    = "\033[38;5;214m" // WARN level (retries, transient failures)

	// Slight reason-based variations of the "gain" yellow for Points earned.
	ansiBrightYellow = "\033[93m"       // CLAIM
	ansiGold         = "\033[38;5;178m" // WATCH_STREAK
	ansiOrangeGain   = "\033[38;5;214m" // RAID
)

// staticMsgColors maps a fixed slog message (the msg field) to a color. Only
// INFO/DEBUG records are looked up here — WARN and ERROR are colored purely by
// level in colorForRecord before this map is consulted. Messages that need to
// inspect record attributes (Points earned, Prediction result) are handled
// separately and are intentionally absent from this map.
var staticMsgColors = map[string]string{
	// Online / offline transitions.
	"Streamer is online":    ansiGreen,
	"Streamer went offline": ansiRed,

	// Predictions.
	"Placing prediction bet": ansiBlue,
	"Prediction confirmed":   ansiSoftBlue,

	// Bets filtered out by BetSettings filter conditions.
	"Skipping bet":       ansiMagenta,
	"Bet amount too low": ansiMagenta,

	// Gains — same intent as "Points earned", so the same yellow family.
	"Claiming bonus":                 ansiYellow,
	"Claiming moment":                ansiYellow,
	"Claiming drop":                  ansiYellow,
	"Claimed drop":                   ansiYellow,
	"Contributing to community goal": ansiYellow,

	// Startup / bookkeeping noise — muted so real events stand out.
	"Loaded streamer":                         ansiGray,
	"Loading streamers":                       ansiGray,
	"Joined IRC chat":                         ansiGray,
	"Discord notification provider connected": ansiGray,
}

// colorForRecord picks the ANSI color prefix for a record, or "" for the
// terminal default. Level wins first (ERROR red, WARN amber), then message
// category. This mirrors the colorama palette used by the upstream Python
// miner, adapted to this project's actual msg values.
func colorForRecord(r slog.Record) string {
	switch {
	case r.Level >= slog.LevelError:
		return ansiRed
	case r.Level >= slog.LevelWarn:
		return ansiAmber
	}

	switch r.Message {
	case "Points earned":
		return pointsEarnedColor(r)
	case "Prediction result":
		return predictionResultColor(r)
	}

	if c, ok := staticMsgColors[r.Message]; ok {
		return c
	}
	return ""
}

// pointsEarnedColor varies the gain-yellow slightly by the "reason" attribute
// so CLAIM/RAID/WATCH_STREAK pops read differently from the constant passive
// WATCH stream. Any unknown reason falls back to plain yellow.
func pointsEarnedColor(r slog.Record) string {
	switch attrString(r, "reason") {
	case "CLAIM":
		return ansiBrightYellow
	case "WATCH_STREAK":
		return ansiGold
	case "RAID":
		return ansiOrangeGain
	default:
		return ansiYellow
	}
}

// predictionResultColor is green on WIN, red on LOSE, and muted for REFUND or
// anything else.
func predictionResultColor(r slog.Record) string {
	switch attrString(r, "result") {
	case "WIN":
		return ansiGreen
	case "LOSE":
		return ansiRed
	default:
		return ansiGray
	}
}

// attrString returns the string form of the first top-level attribute with the
// given key, or "" if absent. The miner never nests these attributes under a
// group, so a flat scan is sufficient.
func attrString(r slog.Record, key string) string {
	var out string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out = a.Value.String()
			return false
		}
		return true
	})
	return out
}

// consoleHandler formats records like slog's TextHandler but wraps each line in
// an ANSI color chosen by colorForRecord. It reuses a real TextHandler for the
// actual formatting (attribute escaping, groups, time layout) so the only thing
// it adds is the color envelope.
type consoleHandler struct {
	level   slog.Level
	w       io.Writer
	mu      *sync.Mutex
	color   bool
	withOps []func(slog.Handler) slog.Handler
}

func newConsoleHandler(w io.Writer, level slog.Level) *consoleHandler {
	return &consoleHandler{
		level: level,
		w:     w,
		mu:    &sync.Mutex{},
		color: colorEnabled(),
	}
}

// colorEnabled honors the de-facto NO_COLOR standard (https://no-color.org) so
// the output can be forced plain, but otherwise always colors — Docker/Portainer
// render ANSI even though stdout is not a TTY, which is the whole point here.
func colorEnabled() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	return true
}

func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *consoleHandler) Handle(ctx context.Context, r slog.Record) error {
	var buf bytes.Buffer

	var inner slog.Handler = slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: h.level})
	for _, op := range h.withOps {
		inner = op(inner)
	}
	if err := inner.Handle(ctx, r); err != nil {
		return err
	}

	line := buf.Bytes()

	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.color {
		_, err := h.w.Write(line)
		return err
	}

	color := colorForRecord(r)
	if color == "" {
		_, err := h.w.Write(line)
		return err
	}

	trimmed := bytes.TrimRight(line, "\n")
	_, err := fmt.Fprintf(h.w, "%s%s%s\n", color, trimmed, ansiReset)
	return err
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.clone(func(inner slog.Handler) slog.Handler {
		return inner.WithAttrs(attrs)
	})
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	return h.clone(func(inner slog.Handler) slog.Handler {
		return inner.WithGroup(name)
	})
}

// clone returns a copy of h with op appended, replaying the accumulated
// WithAttrs/WithGroup calls (in order) onto a fresh TextHandler on every
// Handle. The miner doesn't currently use slog groups/With chains, but this
// keeps the handler correct if it ever does.
func (h *consoleHandler) clone(op func(slog.Handler) slog.Handler) *consoleHandler {
	ops := make([]func(slog.Handler) slog.Handler, len(h.withOps), len(h.withOps)+1)
	copy(ops, h.withOps)
	ops = append(ops, op)
	return &consoleHandler{
		level:   h.level,
		w:       h.w,
		mu:      h.mu,
		color:   h.color,
		withOps: ops,
	}
}
