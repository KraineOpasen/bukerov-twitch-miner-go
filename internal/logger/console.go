package logger

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
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

// lineStyle is the console decoration for one log line: an ANSI color that
// wraps the whole line and an emoji prefixed to it. Both come from a single
// lookup (styleForRecord) so a given message gets its color and icon from one
// place — the mapping is never duplicated. An empty field means "none".
type lineStyle struct {
	color string
	emoji string
}

// staticMsgStyles maps a fixed slog message (the msg field) to its console
// style. Only INFO/DEBUG records are looked up here — WARN and ERROR are styled
// purely by level in styleForRecord before this map is consulted. Messages that
// need to inspect record attributes (Points earned, Prediction result) are
// handled separately and are intentionally absent from this map.
//
// The palette/emoji mirror the colorama style of rdavydov/Twitch-Channel-
// Points-Miner-v2 and Guliveer/twitch-miner-go, adapted to this project's real
// msg values.
var staticMsgStyles = map[string]lineStyle{
	// Startup banner.
	"Twitch Channel Points Miner": {emoji: "🚀"},

	// Online / offline transitions.
	"Streamer is online":    {color: ansiGreen, emoji: "🟢"},
	"Streamer went offline": {color: ansiRed, emoji: "😴"},

	// Predictions.
	"Placing prediction bet": {color: ansiBlue, emoji: "🎫"},
	"Prediction confirmed":   {color: ansiSoftBlue, emoji: "✅"},

	// Bets filtered out by BetSettings filter conditions.
	"Skipping bet":       {color: ansiMagenta},
	"Bet amount too low": {color: ansiMagenta},

	// Gains — same intent as "Points earned", so the same yellow family.
	"Claiming bonus":                 {color: ansiYellow, emoji: "🎁"},
	"Claiming moment":                {color: ansiYellow, emoji: "🎮"},
	"Claiming drop":                  {color: ansiYellow, emoji: "🎮"},
	"Claimed drop":                   {color: ansiYellow, emoji: "🎮"},
	"Contributing to community goal": {color: ansiYellow},

	// Raids.
	"Joining raid": {emoji: "🚩"},

	// Fair watch-pair rotation (PR #4).
	"Rotating watch pair": {emoji: "🔄"},

	// Startup / bookkeeping noise — muted so real events stand out; a neutral
	// wrench keeps them grouped without competing with the notable events.
	"Loaded streamer":                         {color: ansiGray, emoji: "🔧"},
	"Loading streamers":                       {color: ansiGray, emoji: "🔧"},
	"Joined IRC chat":                         {color: ansiGray, emoji: "🔧"},
	"Discord notification provider connected": {color: ansiGray, emoji: "🔧"},
}

// styleForRecord picks the console color+emoji for a record. Level wins first
// (ERROR red 🔴, WARN amber ⚠️), then message category. Returns a zero lineStyle
// (no color, no emoji) for anything unmapped, which prints as the plain
// terminal default.
func styleForRecord(r slog.Record) lineStyle {
	switch {
	case r.Level >= slog.LevelError:
		return lineStyle{color: ansiRed, emoji: "🔴"}
	case r.Level >= slog.LevelWarn:
		return lineStyle{color: ansiAmber, emoji: "⚠️"}
	}

	switch r.Message {
	case "Points earned":
		return pointsEarnedStyle(r)
	case "Prediction result":
		return predictionResultStyle(r)
	}

	return staticMsgStyles[r.Message]
}

// pointsEarnedStyle varies the gain-yellow and icon by the "reason" attribute so
// CLAIM/RAID/WATCH_STREAK read differently from the constant passive WATCH
// stream. Any unknown reason falls back to the WATCH styling.
func pointsEarnedStyle(r slog.Record) lineStyle {
	switch attrString(r, "reason") {
	case "CLAIM":
		return lineStyle{color: ansiBrightYellow, emoji: "🎁"}
	case "WATCH_STREAK":
		return lineStyle{color: ansiGold, emoji: "🔥"}
	case "RAID":
		return lineStyle{color: ansiOrangeGain, emoji: "🚩"}
	default:
		return lineStyle{color: ansiYellow, emoji: "🖊️"}
	}
}

// predictionResultStyle is green 🏆 on WIN, red ❌ on LOSE, and muted (no emoji)
// for REFUND or anything else.
func predictionResultStyle(r slog.Record) lineStyle {
	switch attrString(r, "result") {
	case "WIN":
		return lineStyle{color: ansiGreen, emoji: "🏆"}
	case "LOSE":
		return lineStyle{color: ansiRed, emoji: "❌"}
	default:
		return lineStyle{color: ansiGray}
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

// newConsoleHandler builds the colored stdout handler. Coloring follows the
// explicit `colored` flag (the Logger "Colored Output" setting) and nothing
// else — deliberately no isatty/IsTerminal check on os.Stdout. Terminal
// autodetection breaks the main use case: the container's stdout is read by web
// log viewers (Portainer, Dozzle) over the Docker API with no TTY attached, so
// isatty would report "not a terminal" and strip the color the user asked for,
// even though those viewers render ANSI just fine. We trust the user's choice.
func newConsoleHandler(w io.Writer, level slog.Level, colored bool) *consoleHandler {
	return &consoleHandler{
		level: level,
		w:     w,
		mu:    &sync.Mutex{},
		color: colored,
	}
}

func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *consoleHandler) Handle(ctx context.Context, r slog.Record) error {
	var buf bytes.Buffer

	// The record is formatted unmodified — the emoji is prefixed to the output
	// line below, never to the slog message, so it never passes through slog's
	// string quoting (which would escape variation-selector emoji like ⚠️/🖊️).
	// The file handler formats the same untouched record, so the on-disk log
	// stays plain text with neither color nor emoji.
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

	// The "Colored Output" setting gates all console decoration (color + emoji);
	// with it off, stdout is the same plain text as the file.
	if !h.color {
		_, err := h.w.Write(line)
		return err
	}

	style := styleForRecord(r)
	if style.color == "" && style.emoji == "" {
		_, err := h.w.Write(line)
		return err
	}

	trimmed := bytes.TrimRight(line, "\n")

	prefix := ""
	if style.emoji != "" {
		prefix = style.emoji + " "
	}

	if style.color == "" {
		_, err := fmt.Fprintf(h.w, "%s%s\n", prefix, trimmed)
		return err
	}
	_, err := fmt.Fprintf(h.w, "%s%s%s%s\n", style.color, prefix, trimmed, ansiReset)
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
