// Package health tracks the miner's operational health signals — OAuth, GQL
// API, PubSub, watch transport, and drops sync/progress — and drives the
// watch-transport accrual canary. It is data/logic only (no HTTP): the web
// dashboard and the debug endpoint render Center.Snapshot(); the miner feeds
// the non-canary signals from its existing providers, and the Canary records
// the watch-transport signal.
//
// Nothing security-sensitive is ever stored: no OAuth token, cookies, signed
// playback/spade URL (which embeds sig/token), or authorization header. Signals
// carry only a status, timestamp, duration, stage, a short human detail, and a
// stable error code.
package health

import (
	"sync"
	"sync/atomic"
	"time"
)

// Signal names.
const (
	SignalOAuth          = "oauth"
	SignalGQLAPI         = "gql_api"
	SignalPubSub         = "pubsub"
	SignalWatchTransport = "watch_transport"
	SignalDropsInventory = "drops_inventory"
	SignalDropsProgress  = "drops_progress"
)

// Status values a signal can report.
const (
	StatusOK      = "ok"      // last check succeeded
	StatusFailed  = "failed"  // last check failed
	StatusIdle    = "idle"    // nothing to check right now (e.g. no active drops)
	StatusStalled = "stalled" // reserved for the Stage 3 drop-progress watchdog
	StatusUnknown = "unknown" // never checked yet
)

// signalOrder is the stable display order for the Health Center.
var signalOrder = []string{
	SignalOAuth,
	SignalGQLAPI,
	SignalPubSub,
	SignalWatchTransport,
	SignalDropsInventory,
	SignalDropsProgress,
}

// Signal is one health signal's last-known state. It deliberately holds no
// sensitive data (no tokens, cookies, signed URLs, or headers).
type Signal struct {
	Name      string        `json:"name"`
	Status    string        `json:"status"`
	CheckedAt time.Time     `json:"checkedAt,omitzero"`
	Duration  time.Duration `json:"duration,omitempty"`
	Stage     string        `json:"stage,omitempty"`
	Detail    string        `json:"detail,omitempty"`
	ErrorCode string        `json:"errorCode,omitempty"`
}

// Healthy reports whether the signal is in a good state (OK or IDLE — IDLE means
// "nothing to check", not "broken"). Used for transition detection.
func (s Signal) Healthy() bool {
	return s.Status == StatusOK || s.Status == StatusIdle
}

// Snapshot is the immutable, published view of all health signals plus the
// active GQL client label. Read lock-free by the dashboard and debug endpoint.
type Snapshot struct {
	ActiveClientID string   `json:"activeClientId,omitempty"`
	Signals        []Signal `json:"signals"`
}

// Signal returns the named signal from the snapshot, or a zero Signal + false.
func (s Snapshot) Signal(name string) (Signal, bool) {
	for _, sig := range s.Signals {
		if sig.Name == name {
			return sig, true
		}
	}
	return Signal{}, false
}

// Center is the passive aggregator of health signals. Producers (the miner's
// health loop and the canary) call Record/SetActiveClientID; consumers call
// Snapshot(). It rebuilds an immutable snapshot under a mutex on every update so
// readers never take a lock.
type Center struct {
	mu       sync.Mutex
	signals  map[string]Signal
	clientID string
	snap     atomic.Pointer[Snapshot]
}

func NewCenter() *Center {
	c := &Center{signals: make(map[string]Signal)}
	c.publishLocked()
	return c
}

// Record stores (or replaces) a signal's state and republishes the snapshot.
func (c *Center) Record(sig Signal) {
	c.mu.Lock()
	c.signals[sig.Name] = sig
	c.publishLocked()
	c.mu.Unlock()
}

// SetActiveClientID records the GQL client label (TV/Browser/Mobile).
func (c *Center) SetActiveClientID(label string) {
	c.mu.Lock()
	c.clientID = label
	c.publishLocked()
	c.mu.Unlock()
}

// Signal returns the current state of the named signal, or false if unset.
func (c *Center) Signal(name string) (Signal, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.signals[name]
	return s, ok
}

// Snapshot returns the last published view with an independent copy of the
// signal slice, so a caller cannot corrupt the shared snapshot. Safe from any
// goroutine.
func (c *Center) Snapshot() Snapshot {
	s := c.snap.Load()
	if s == nil {
		return Snapshot{}
	}
	out := *s
	out.Signals = append([]Signal(nil), s.Signals...)
	return out
}

// publishLocked rebuilds the immutable snapshot in the stable signal order.
// Caller holds mu.
func (c *Center) publishLocked() {
	snap := &Snapshot{
		ActiveClientID: c.clientID,
		Signals:        make([]Signal, 0, len(c.signals)),
	}
	for _, name := range signalOrder {
		if s, ok := c.signals[name]; ok {
			snap.Signals = append(snap.Signals, s)
		}
	}
	c.snap.Store(snap)
}
