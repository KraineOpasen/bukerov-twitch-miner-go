package logger

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
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

// consoleBufferLines bounds how many formatted console lines may queue for the
// background writer before new lines are dropped. Large enough that a brief
// stdout hiccup never drops anything, small enough to bound memory if the
// consumer stalls for good.
const consoleBufferLines = 4096

// consoleWriter serializes console output on a single background goroutine, so
// a slow or wedged stdout consumer (e.g. a Docker log pipe whose reader stopped
// draining) can never stall the logging goroutines. Callers enqueue a
// fully-formatted line and return immediately; the actual blocking Write happens
// only on the background goroutine, holding no lock any other goroutine needs.
// If the buffer fills (the consumer has genuinely wedged) further lines are
// dropped and counted rather than blocking the caller — the miner keeps running
// even if nothing is reading its stdout.
type consoleWriter struct {
	lines chan []byte
	// done signals run() to drain and exit. Close closes THIS, never lines:
	// closing lines would panic any write() that races shutdown (send on a
	// closed channel), which is a real risk because other goroutines may still
	// be logging when Logger.Close runs. closed is the fast-path guard so
	// post-Close writes drop instead of buffering into a queue run() no longer
	// drains.
	done      chan struct{}
	closed    atomic.Bool
	wg        sync.WaitGroup
	dropped   atomic.Uint64
	closeOnce sync.Once
}

func newConsoleWriter(w io.Writer) *consoleWriter {
	cw := &consoleWriter{
		lines: make(chan []byte, consoleBufferLines),
		done:  make(chan struct{}),
	}
	cw.wg.Add(1)
	go cw.run(w)
	return cw
}

// run drains queued lines to w in order until Close signals done, then flushes
// whatever is still buffered and exits. Being the sole writer, it needs no lock
// to keep lines from interleaving, and its blocking Writes never hold a lock the
// logging goroutines wait on. lines is deliberately never closed (see Close).
func (cw *consoleWriter) run(w io.Writer) {
	defer cw.wg.Done()
	for {
		select {
		case line := <-cw.lines:
			_, _ = w.Write(line)
		case <-cw.done:
			for {
				select {
				case line := <-cw.lines:
					_, _ = w.Write(line)
				default:
					return
				}
			}
		}
	}
}

// write enqueues a formatted line. It never blocks and never panics: once Close
// has run it drops the line (counting it); otherwise it enqueues, or drops if
// the buffer is full. It never sends on a closed channel because lines is never
// closed.
//
// Known limitation: between the closed check and the send there is a narrow
// window where a line can be enqueued just after run() has already drained and
// exited on Close. Such a line (at most one or two, only at the instant of
// shutdown) neither panics nor blocks, but goes unwritten and is not counted in
// Dropped(); the file log is unaffected. Left as-is by design, like the
// cold-start victim-by-index tie-break in internal/watcher/broker.go.
func (cw *consoleWriter) write(line []byte) {
	if cw.closed.Load() {
		cw.dropped.Add(1)
		return
	}
	select {
	case cw.lines <- line:
	default:
		cw.dropped.Add(1)
	}
}

// Close stops the background writer after draining the queued lines. Idempotent.
// The process-wide writer created by Setup is closed once, from Logger.Close, to
// flush any buffered lines on shutdown. It signals run() via the done channel
// and never closes lines, so a write() racing shutdown drops safely instead of
// panicking on a send to a closed channel.
func (cw *consoleWriter) Close() {
	cw.closeOnce.Do(func() {
		cw.closed.Store(true)
		close(cw.done)
		cw.wg.Wait()
	})
}

// Dropped reports how many console lines were dropped because the buffer was
// full (a wedged stdout consumer). Exposed for diagnostics and tests.
func (cw *consoleWriter) Dropped() uint64 {
	return cw.dropped.Load()
}

// consoleHandler formats records like slog's TextHandler but wraps each line in
// an ANSI color chosen by styleForRecord. It reuses a real TextHandler for the
// actual formatting (attribute escaping, groups, time layout) so the only thing
// it adds is the color envelope, then hands the finished line to an async
// consoleWriter so a stalled stdout consumer can't block logging.
type consoleHandler struct {
	level   slog.Level
	cw      *consoleWriter
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
		cw:    newConsoleWriter(w),
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

	// Build the final (possibly colored) line, then hand it to the async writer.
	// No lock is held and no blocking Write happens on this goroutine, so a
	// stalled stdout consumer cannot stall the caller.
	h.cw.write(h.decorate(r, buf.Bytes()))
	return nil
}

// decorate returns the final console bytes for the record as a freshly
// allocated, caller-owned slice: the plain formatted line when coloring is off
// or the record is unstyled, otherwise the line wrapped in its ANSI color with
// an emoji prefix. line is slog's TextHandler output (trailing newline
// included) and is not retained.
func (h *consoleHandler) decorate(r slog.Record, line []byte) []byte {
	// The "Colored Output" setting gates all console decoration (color + emoji);
	// with it off, stdout is the same plain text as the file.
	if !h.color {
		return append([]byte(nil), line...)
	}

	style := styleForRecord(r)
	if style.color == "" && style.emoji == "" {
		return append([]byte(nil), line...)
	}

	trimmed := bytes.TrimRight(line, "\n")

	var out bytes.Buffer
	out.Grow(len(trimmed) + len(style.color) + len(style.emoji) + len(ansiReset) + 3)
	if style.color != "" {
		out.WriteString(style.color)
	}
	if style.emoji != "" {
		out.WriteString(style.emoji)
		out.WriteByte(' ')
	}
	out.Write(trimmed)
	if style.color != "" {
		out.WriteString(ansiReset)
	}
	out.WriteByte('\n')
	return out.Bytes()
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
// Handle. Clones share the one consoleWriter (like the old shared mutex) so all
// output still serializes through a single background goroutine. The miner
// doesn't currently use slog groups/With chains, but this keeps the handler
// correct if it ever does.
func (h *consoleHandler) clone(op func(slog.Handler) slog.Handler) *consoleHandler {
	ops := make([]func(slog.Handler) slog.Handler, len(h.withOps), len(h.withOps)+1)
	copy(ops, h.withOps)
	ops = append(ops, op)
	return &consoleHandler{
		level:   h.level,
		cw:      h.cw,
		color:   h.color,
		withOps: ops,
	}
}
