// Package events keeps a small in-memory ring buffer of notable miner
// events (claims, bets, streamer online/offline transitions, ...) so the
// debug endpoint can show what the bot has been doing recently. It is a
// diagnostic aid only: nothing is persisted, and recording never blocks.
package events

import (
	"sync"
	"time"
)

type Type string

const (
	TypeMinerStarted    Type = "miner_started"
	TypeStreamerOnline  Type = "streamer_online"
	TypeStreamerOffline Type = "streamer_offline"
	TypeBonusClaimed    Type = "bonus_claimed"
	TypePointsEarned    Type = "points_earned"
	TypeBetPlaced       Type = "bet_placed"
	TypeBetResult       Type = "bet_result"
	TypeDropClaimed     Type = "drop_claimed"
	TypeMomentClaimed   Type = "moment_claimed"
	TypeRaidJoined      Type = "raid_joined"
	TypeRewardRedeemed  Type = "reward_redeemed"
	TypeUpdateAvailable Type = "update_available"
	TypeUpdateFailed    Type = "update_failed"

	// TypeDiscoverySelected/TypeDiscoverySwitched track the directory
	// discovery slot picking its first channel and auto-switching between
	// pool candidates.
	TypeDiscoverySelected Type = "discovery_selected"
	TypeDiscoverySwitched Type = "discovery_switched"

	// TypeSlotAssigned/TypeSlotReleased track the unified slot broker granting
	// or releasing one of the two Twitch watch slots (configured or discovered
	// channel), recorded only when the allocation actually changes.
	TypeSlotAssigned Type = "slot_assigned"
	TypeSlotReleased Type = "slot_released"

	// TypeDropStalled/TypeDropRecovered/TypeDropRecoveryStep track the drop
	// progress watchdog: a confirmed stall that exhausted automatic recovery,
	// a stalled/recovering drop whose progress resumed, and each executed
	// recovery-pipeline stage.
	TypeDropStalled      Type = "drop_stalled"
	TypeDropRecovered    Type = "drop_recovered"
	TypeDropRecoveryStep Type = "drop_recovery_step"
)

type Event struct {
	Time     time.Time `json:"time"`
	Type     Type      `json:"type"`
	Streamer string    `json:"streamer,omitempty"`
	Detail   string    `json:"detail,omitempty"`
}

// Log is a fixed-capacity ring buffer of events, safe for concurrent use.
// Once full, the oldest event is overwritten by each new one.
type Log struct {
	mu     sync.Mutex
	events []Event
	next   int
	filled bool
}

const defaultCapacity = 200

func NewLog(capacity int) *Log {
	if capacity <= 0 {
		capacity = defaultCapacity
	}
	return &Log{events: make([]Event, capacity)}
}

func (l *Log) Record(t Type, streamer, detail string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.events[l.next] = Event{Time: time.Now(), Type: t, Streamer: streamer, Detail: detail}
	l.next++
	if l.next == len(l.events) {
		l.next = 0
		l.filled = true
	}
}

// Recent returns up to n events, newest first.
func (l *Log) Recent(n int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()

	size := l.next
	if l.filled {
		size = len(l.events)
	}
	if n > size {
		n = size
	}
	if n <= 0 {
		return nil
	}

	out := make([]Event, 0, n)
	idx := l.next
	for i := 0; i < n; i++ {
		idx--
		if idx < 0 {
			idx = len(l.events) - 1
		}
		out = append(out, l.events[idx])
	}
	return out
}

// defaultLog is the process-wide event log. A package-level instance (like
// slog's default logger) keeps recording a one-liner at the call sites
// scattered across pubsub/drops/models instead of threading a *Log through
// every constructor for what is purely a diagnostic facility.
var defaultLog = NewLog(defaultCapacity)

// Record appends an event to the process-wide log.
func Record(t Type, streamer, detail string) {
	defaultLog.Record(t, streamer, detail)
}

// Recent returns up to n events from the process-wide log, newest first.
func Recent(n int) []Event {
	return defaultLog.Recent(n)
}
