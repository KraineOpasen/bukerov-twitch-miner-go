package notifications

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// defaultBatchInterval is used when a batch config specifies an invalid or
// empty interval.
const defaultBatchInterval = 30 * time.Minute

// BatchConfig is the resolved (parsed) batching configuration for a single
// provider. It is derived from config.BatchingSettings via NewBatchConfig.
type BatchConfig struct {
	Enabled         bool
	Interval        time.Duration
	MaxEntries      int
	ImmediateEvents map[NotificationType]bool
}

// NewBatchConfig converts the JSON-facing config.BatchingSettings into a
// resolved BatchConfig, parsing the interval and normalizing the immediate
// events into a lookup set.
func NewBatchConfig(s config.BatchingSettings) BatchConfig {
	interval := defaultBatchInterval
	if s.Interval != "" {
		if d, err := time.ParseDuration(s.Interval); err == nil && d > 0 {
			interval = d
		} else {
			slog.Warn("Invalid notification batching interval, using default",
				"interval", s.Interval, "default", defaultBatchInterval)
		}
	}

	immediate := make(map[NotificationType]bool, len(s.ImmediateEvents))
	for _, e := range s.ImmediateEvents {
		immediate[NotificationType(strings.TrimSpace(e))] = true
	}

	return BatchConfig{
		Enabled:         s.Enabled,
		Interval:        interval,
		MaxEntries:      s.MaxEntries,
		ImmediateEvents: immediate,
	}
}

// BatchEvent is a single event fed into a Batcher.
type BatchEvent struct {
	// Type identifies the event; it decides whether the event bypasses
	// batching (see BatchConfig.ImmediateEvents).
	Type NotificationType

	// Group is the streamer or campaign the event belongs to. Events are
	// accumulated and flushed per group.
	Group string

	// Line is the human-readable text added to the batched message.
	Line string
}

// sendFunc delivers a fully-formed message to the underlying provider.
type sendFunc func(ctx context.Context, msg Message) error

// Batcher accumulates events for a single provider and flushes them, grouped by
// streamer/campaign, either on a fixed interval or when explicitly flushed.
// Events whose type is listed in the config's immediate set bypass buffering
// and are sent right away. A Batcher is safe for concurrent use.
type Batcher struct {
	name string
	cfg  BatchConfig
	send sendFunc

	mu     sync.Mutex
	groups map[string][]string
	order  []string

	// started is set to true, under mu, BEFORE Start does anything else: it is
	// set FIRST, and only THEN — after mu is released — does Start either
	// close done directly (disabled config, no loop to launch) or spawn the
	// background loop goroutine (enabled config); it is not set "at the same
	// moment" as either of those, just strictly before both. Stop consults it
	// to decide whether there is anything to join: Stop is safe to call on a
	// Batcher whose Start was never invoked (see TestBatcherStopFlushesPending
	// — an enabled batcher driven purely through Add/Stop) precisely because
	// it skips the join in that case rather than waiting on channels nothing
	// will ever close.
	started bool

	// stopCh signals the background loop to exit WITHOUT performing its own
	// flush. Stop always performs the final flush itself, AFTER joining the
	// loop (see Stop's doc comment), so the loop's stopCh branch and Stop's
	// own Flush can never run CONCURRENTLY and split one logical batch across
	// two partial sends. ctx.Done() is the OTHER way the loop exits, and that
	// path DOES flush from inside the loop, since a plain context
	// cancellation with no explicit Stop call still needs a final flush on
	// its way out. The two signals can legitimately race in production (e.g.
	// the miner's shutdown cancels the run ctx around the same time
	// Manager.Stop calls this Stop): if the loop happens to take the
	// ctx.Done branch and flush first, Stop's own join simply waits for done
	// to close (from EITHER branch) and then still calls Flush itself
	// afterward — a harmless SECOND, sequential flush of an already-drained
	// (now empty) buffer, never a concurrent one, because the join is what
	// orders it strictly after the loop has already exited.
	stopCh   chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// NewBatcher creates a Batcher for the named provider. The send function is
// invoked for every outgoing message (both immediate and flushed).
func NewBatcher(name string, cfg BatchConfig, send sendFunc) *Batcher {
	return &Batcher{
		name:   name,
		cfg:    cfg,
		send:   send,
		groups: make(map[string][]string),
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// Start launches the background flush loop, which flushes accumulated events
// every cfg.Interval and exits either on ctx cancellation (performing its own
// final flush) or on Stop's signal (stopCh — Stop performs the final flush
// itself, AFTER joining, so the two can never race). When batching is
// disabled Start is a no-op aside from immediately closing done (there is no
// loop to join). Calling Start more than once is a no-op after the first
// call: a second call would otherwise double-close done and panic, since
// this repo's channel-close idiom is close-once.
func (b *Batcher) Start(ctx context.Context) {
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return
	}
	b.started = true
	enabled := b.cfg.Enabled
	b.mu.Unlock()

	if !enabled {
		close(b.done)
		return
	}

	go func() {
		defer close(b.done)
		ticker := time.NewTicker(b.cfg.Interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				// Final flush on shutdown using a fresh, short-lived context
				// since ctx is already cancelled. Only reached when the loop
				// exits via a plain ctx cancellation rather than an explicit
				// Stop call (Stop signals stopCh instead — see below — and
				// performs the final flush itself after joining).
				flushCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				b.Flush(flushCtx)
				cancel()
				return
			case <-b.stopCh:
				// Stop() already signalled and is waiting to join before
				// performing the final flush itself; exit WITHOUT flushing
				// here so the two flushes can never race.
				return
			case <-ticker.C:
				b.Flush(ctx)
			}
		}
	}()
}

// Add feeds an event into the batcher. Immediate events (or any event when
// batching is disabled) are sent right away and their send error is returned;
// buffered events are stored and Add returns nil.
func (b *Batcher) Add(ctx context.Context, ev BatchEvent) error {
	if !b.cfg.Enabled || b.cfg.ImmediateEvents[ev.Type] {
		return b.send(ctx, Message{
			Type:  ev.Type,
			Title: titleForGroup(ev.Group),
			Body:  ev.Line,
		})
	}

	b.mu.Lock()
	if _, ok := b.groups[ev.Group]; !ok {
		b.order = append(b.order, ev.Group)
	}
	b.groups[ev.Group] = append(b.groups[ev.Group], ev.Line)
	b.mu.Unlock()
	return nil
}

// Flush delivers all accumulated events and clears the buffer. Within each
// group the lines are joined with newlines; groups larger than cfg.MaxEntries
// are split across several messages. Send errors are logged but do not stop the
// remaining groups from being flushed.
func (b *Batcher) Flush(ctx context.Context) {
	b.mu.Lock()
	groups := b.groups
	order := b.order
	b.groups = make(map[string][]string)
	b.order = nil
	b.mu.Unlock()

	for _, group := range order {
		lines := groups[group]
		if len(lines) == 0 {
			continue
		}

		for _, chunk := range chunkLines(lines, b.cfg.MaxEntries) {
			msg := Message{
				Title: titleForGroup(group),
				Body:  strings.Join(chunk, "\n"),
			}
			if err := b.send(ctx, msg); err != nil {
				// safeSendErrorAttr is the only way a send error may be logged
				// here: b.send is always a provider's Send (see NewBatcher's
				// caller, manager.go), and a provider must never have its raw
				// error text (which may be a regressed *url.Error) printed.
				slog.Error("Failed to flush notification batch",
					"provider", b.name, "group", group, safeSendErrorAttr(err))
			}
		}
	}
}

// Stop signals the background loop (if one was ever started) to exit, JOINS
// it — bounded by ctx, never an unbounded wait, since Manager.Stop calls this
// while holding discordLifecycleMu and a wedged loop must not wedge shutdown
// forever — and only THEN performs the final flush itself, so it is provably
// the LAST flush and can never race a concurrent loop-driven one (see
// Start's stopCh branch). Safe to call multiple times: the second call finds
// stopCh and done already closed, so it joins instantly and just re-flushes
// an already-empty buffer. Safe to call when Start was never invoked (an
// enabled batcher driven purely through Add/Flush): with no goroutine to
// signal, the join is skipped entirely rather than blocking on channels
// nothing will ever close.
func (b *Batcher) Stop(ctx context.Context) {
	b.stopOnce.Do(func() {
		close(b.stopCh)
	})

	b.mu.Lock()
	started := b.started
	b.mu.Unlock()

	if started {
		select {
		case <-b.done:
		case <-ctx.Done():
		}
	}

	b.Flush(ctx)
}

// titleForGroup builds the message title for a group of events.
func titleForGroup(group string) string {
	if group == "" {
		return "Twitch Points Miner"
	}
	return fmt.Sprintf("Twitch Points Miner — %s", group)
}

// chunkLines splits lines into slices of at most maxEntries elements. A
// non-positive maxEntries means no limit (a single chunk).
func chunkLines(lines []string, maxEntries int) [][]string {
	if maxEntries <= 0 || len(lines) <= maxEntries {
		return [][]string{lines}
	}

	var chunks [][]string
	for i := 0; i < len(lines); i += maxEntries {
		end := i + maxEntries
		if end > len(lines) {
			end = len(lines)
		}
		chunks = append(chunks, lines[i:end])
	}
	return chunks
}
