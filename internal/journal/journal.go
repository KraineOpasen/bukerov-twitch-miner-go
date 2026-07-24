// Package journal is a bounded, privacy-safe, race-free in-memory diagnostic
// ring buffer. It records a fixed-capacity, monotonically-sequenced trail of
// typed events (watch-slot lifecycle transitions and connection-health
// transitions) so an operator can reconstruct WHY the miner did what it did,
// without any effect on scheduling, point earning, health classification, or
// notification decisions.
//
// Design contract (BKM-013 / BKM-014 observability):
//
//   - Fixed bounded capacity; once full the oldest record is evicted. No
//     unbounded memory growth.
//   - A monotonic sequence number is assigned to every appended record and is
//     never reused, even after eviction.
//   - Timestamps come from an injectable Clock seam, so tests are deterministic.
//   - Snapshots are immutable copy-outs: the returned records share no mutable
//     storage with the journal, so a caller can never mutate stored state.
//   - Append and read are concurrency-safe.
//   - Append is non-blocking: it performs NO network, database, or template
//     work under the lock — only a bounded slice write. A diagnostic failure can
//     therefore never stall or affect mining.
//   - Every method is safe on a nil receiver, so instrumentation call sites need
//     no nil guard and a build with journaling disabled behaves identically to
//     one with it enabled.
//
// PRIVACY. This package (like internal/debug and internal/health) is redacted
// by construction. The event payload types must contain ONLY value-typed,
// privacy-safe fields — canonical logins, stable IDs, bounded reason/stage
// codes, counters, timestamps, and durations. They must NEVER carry a token,
// cookie, header, signed/playback/spade URL, request payload, OAuth datum,
// Discord webhook, config secret, or a raw error string. Because payloads hold
// only value types, an immutable copy-out is a plain by-value copy.
package journal

import (
	"sync"
	"time"
)

// Clock returns the current time. It is injected so tests can supply
// deterministic, controllable timestamps; a nil Clock defaults to time.Now.
type Clock func() time.Time

// Record wraps one event payload E with the monotonic sequence number and the
// timestamp assigned at append time. Copy-out is by value; because payloads
// contain only value-typed fields, a returned Record shares no mutable state
// with the journal.
type Record[E any] struct {
	Seq   uint64    `json:"seq"`
	At    time.Time `json:"at"`
	Event E         `json:"event"`
}

// DefaultCapacity is used when New is given a non-positive capacity.
const DefaultCapacity = 256

// Journal is a fixed-capacity ring buffer of Records, safe for concurrent
// append and read. When full, the oldest Record is evicted deterministically.
// The sequence number is monotonic across evictions and never reused.
type Journal[E any] struct {
	mu     sync.Mutex
	buf    []Record[E]
	next   int
	filled bool
	seq    uint64
	now    Clock
}

// New returns a journal retaining at most capacity records (DefaultCapacity when
// capacity <= 0). clock supplies record timestamps (time.Now when nil).
func New[E any](capacity int, clock Clock) *Journal[E] {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	if clock == nil {
		clock = time.Now
	}
	return &Journal[E]{buf: make([]Record[E], capacity), now: clock}
}

// Now returns the journal's current time from its clock seam. It lets a caller
// stamp related state (e.g. a residence start) on the same deterministic clock
// the journal uses, so derived durations stay reproducible in tests. Safe on a
// nil receiver (returns the zero time).
func (j *Journal[E]) Now() time.Time {
	if j == nil {
		return time.Time{}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.now()
}

// Append stamps e with the next monotonic sequence number and the current time,
// stores it (evicting the oldest record when full), and returns the stored
// Record. It is non-blocking: it performs no network, database, or template work
// — only a bounded slice write under a short-lived lock. Safe on a nil receiver,
// where it returns the zero Record without recording anything.
func (j *Journal[E]) Append(e E) Record[E] {
	if j == nil {
		return Record[E]{}
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	j.seq++
	rec := Record[E]{Seq: j.seq, At: j.now(), Event: e}
	j.buf[j.next] = rec
	j.next++
	if j.next == len(j.buf) {
		j.next = 0
		j.filled = true
	}
	return rec
}

// Snapshot returns an immutable copy of the retained records, oldest first. The
// returned slice and its elements share no storage with the journal, so a caller
// mutating them cannot alter journal state. Safe on a nil receiver (returns nil).
func (j *Journal[E]) Snapshot() []Record[E] {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	size := j.next
	start := 0
	if j.filled {
		size = len(j.buf)
		start = j.next // oldest lives just past the write cursor
	}
	if size == 0 {
		return nil
	}

	out := make([]Record[E], 0, size)
	n := len(j.buf)
	for i := 0; i < size; i++ {
		out = append(out, j.buf[(start+i)%n])
	}
	return out
}

// Len returns the number of records currently retained. Safe on a nil receiver.
func (j *Journal[E]) Len() int {
	if j == nil {
		return 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.filled {
		return len(j.buf)
	}
	return j.next
}

// Cap returns the fixed capacity. Safe on a nil receiver (returns 0).
func (j *Journal[E]) Cap() int {
	if j == nil {
		return 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.buf)
}

// LastSeq returns the highest sequence number assigned so far (0 before any
// append). Safe on a nil receiver.
func (j *Journal[E]) LastSeq() uint64 {
	if j == nil {
		return 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.seq
}
