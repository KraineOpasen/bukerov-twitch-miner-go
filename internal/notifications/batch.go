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
		done:   make(chan struct{}),
	}
}

// Start launches the background flush loop, which flushes accumulated events
// every cfg.Interval and performs a final flush when ctx is cancelled. When
// batching is disabled Start is a no-op (events are sent immediately by Add).
func (b *Batcher) Start(ctx context.Context) {
	if !b.cfg.Enabled {
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
				// since ctx is already cancelled.
				flushCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				b.Flush(flushCtx)
				cancel()
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
				slog.Error("Failed to flush notification batch",
					"provider", b.name, "group", group, "error", err)
			}
		}
	}
}

// Stop performs a final synchronous flush and waits for the background loop
// (if any) to finish. It is safe to call multiple times.
func (b *Batcher) Stop(ctx context.Context) {
	b.stopOnce.Do(func() {
		b.Flush(ctx)
	})
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
