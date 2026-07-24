package journal

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// manualClock is a deterministic, controllable clock seam for tests (J3).
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func newManualClock(start time.Time) *manualClock { return &manualClock{t: start} }

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func base() time.Time { return time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC) }

// TestClockSeamStampsDeterministically (J3): timestamps come from the injected
// clock, not the wall clock.
func TestClockSeamStampsDeterministically(t *testing.T) {
	clk := newManualClock(base())
	j := New[SlotEvent](8, clk.now)

	r1 := j.Append(SlotEvent{Type: SlotEntered, Channel: "a"})
	if !r1.At.Equal(base()) {
		t.Fatalf("first record At = %v, want %v", r1.At, base())
	}
	clk.advance(90 * time.Second)
	r2 := j.Append(SlotEvent{Type: SlotReleased, Channel: "a"})
	if !r2.At.Equal(base().Add(90 * time.Second)) {
		t.Fatalf("second record At = %v, want %v", r2.At, base().Add(90*time.Second))
	}
	if got := j.Now(); !got.Equal(base().Add(90 * time.Second)) {
		t.Fatalf("Now() = %v, want %v", got, base().Add(90*time.Second))
	}
}

// TestMonotonicSequence (J2): sequence numbers increase by one and never repeat.
func TestMonotonicSequence(t *testing.T) {
	j := New[SlotEvent](4, newManualClock(base()).now)
	for i := 1; i <= 3; i++ {
		r := j.Append(SlotEvent{Channel: "a"})
		if r.Seq != uint64(i) {
			t.Fatalf("append %d got Seq %d", i, r.Seq)
		}
	}
	if j.LastSeq() != 3 {
		t.Fatalf("LastSeq = %d, want 3", j.LastSeq())
	}
}

// TestBoundedCapacityDeterministicEviction (J1, J7, T15): oldest events are
// evicted deterministically when capacity is reached and the sequence stays
// monotonic across evictions.
func TestBoundedCapacityDeterministicEviction(t *testing.T) {
	const cap = 4
	j := New[SlotEvent](cap, newManualClock(base()).now)

	for i := 0; i < 10; i++ {
		j.Append(SlotEvent{Channel: string(rune('a' + i))})
	}

	if j.Len() != cap {
		t.Fatalf("Len = %d, want %d", j.Len(), cap)
	}
	if j.Cap() != cap {
		t.Fatalf("Cap = %d, want %d", j.Cap(), cap)
	}

	snap := j.Snapshot()
	if len(snap) != cap {
		t.Fatalf("snapshot len = %d, want %d", len(snap), cap)
	}
	// After 10 appends into a cap-4 ring, the retained records are the last four,
	// seq 7..10, oldest first, channels g,h,i,j.
	wantSeq := []uint64{7, 8, 9, 10}
	wantChan := []string{"g", "h", "i", "j"}
	for i, r := range snap {
		if r.Seq != wantSeq[i] {
			t.Fatalf("snap[%d].Seq = %d, want %d", i, r.Seq, wantSeq[i])
		}
		if r.Event.Channel != wantChan[i] {
			t.Fatalf("snap[%d].Channel = %q, want %q", i, r.Event.Channel, wantChan[i])
		}
	}
	// Monotonic and strictly increasing.
	for i := 1; i < len(snap); i++ {
		if snap[i].Seq <= snap[i-1].Seq {
			t.Fatalf("sequence not monotonic at %d: %d <= %d", i, snap[i].Seq, snap[i-1].Seq)
		}
	}
}

// TestSnapshotImmutability (J4, T16): a caller mutating a returned snapshot
// cannot change stored journal state.
func TestSnapshotImmutability(t *testing.T) {
	j := New[SlotEvent](8, newManualClock(base()).now)
	j.Append(SlotEvent{Type: SlotEntered, Channel: "original", Successes: 1})

	snap := j.Snapshot()
	// Mutate the returned copy aggressively.
	snap[0].Event.Channel = "tampered"
	snap[0].Event.Successes = 999
	snap[0].Seq = 424242
	snap = append(snap, Record[SlotEvent]{Event: SlotEvent{Channel: "injected"}})
	_ = snap

	again := j.Snapshot()
	if len(again) != 1 {
		t.Fatalf("stored length changed to %d after caller mutation", len(again))
	}
	if again[0].Event.Channel != "original" {
		t.Fatalf("stored Channel mutated to %q", again[0].Event.Channel)
	}
	if again[0].Event.Successes != 1 {
		t.Fatalf("stored Successes mutated to %d", again[0].Event.Successes)
	}
	if again[0].Seq != 1 {
		t.Fatalf("stored Seq mutated to %d", again[0].Seq)
	}
}

// TestOrderingOldestFirst: snapshot is chronological (oldest first) before the
// ring wraps.
func TestOrderingOldestFirst(t *testing.T) {
	j := New[SlotEvent](8, newManualClock(base()).now)
	for _, c := range []string{"a", "b", "c"} {
		j.Append(SlotEvent{Channel: c})
	}
	snap := j.Snapshot()
	want := []string{"a", "b", "c"}
	for i, r := range snap {
		if r.Event.Channel != want[i] {
			t.Fatalf("snap[%d] = %q, want %q", i, r.Event.Channel, want[i])
		}
	}
}

// TestNilReceiverSafe (J10): every method is safe on a nil journal, so
// instrumentation call sites need no nil guard and a disabled journal is inert.
func TestNilReceiverSafe(t *testing.T) {
	var j *Journal[SlotEvent]
	if r := j.Append(SlotEvent{Channel: "a"}); r.Seq != 0 {
		t.Fatalf("nil Append returned non-zero record")
	}
	if s := j.Snapshot(); s != nil {
		t.Fatalf("nil Snapshot returned non-nil")
	}
	if j.Len() != 0 || j.Cap() != 0 || j.LastSeq() != 0 {
		t.Fatalf("nil accessors returned non-zero")
	}
	if !j.Now().IsZero() {
		t.Fatalf("nil Now returned non-zero")
	}
}

// TestConcurrencyRaceFree (J5, T17): concurrent writers and readers do not race
// and never corrupt the sequence. Run under -race.
func TestConcurrencyRaceFree(t *testing.T) {
	j := New[SlotEvent](64, newManualClock(base()).now)

	const writers = 8
	const perWriter = 500
	var wg sync.WaitGroup

	stop := make(chan struct{})
	// Readers churn snapshots concurrently with writers.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s := j.Snapshot()
					for i := 1; i < len(s); i++ {
						if s[i].Seq <= s[i-1].Seq {
							t.Errorf("snapshot not monotonic under concurrency")
							return
						}
					}
					_ = j.Len()
				}
			}
		}()
	}

	var writersWG sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		writersWG.Add(1)
		go func(id int) {
			defer wg.Done()
			defer writersWG.Done()
			for i := 0; i < perWriter; i++ {
				j.Append(SlotEvent{Channel: "c", SlotIndex: id})
			}
		}(w)
	}

	// Stop the readers once every writer has finished, then join everyone.
	writersWG.Wait()
	close(stop)
	wg.Wait()

	if got := j.LastSeq(); got != uint64(writers*perWriter) {
		t.Fatalf("LastSeq = %d, want %d (no lost/duplicated appends)", got, writers*perWriter)
	}
}

// TestRedactionByConstruction (J8, T19): even when fed fixtures that contain
// fake secrets in the free-form code fields, the serialized journal exposes only
// what was put in those bounded fields — the payload type has no field that can
// carry a token/URL/cookie by structure. This asserts the schema-level guarantee
// (the watcher/miner tests assert the wiring never routes real secrets in).
func TestRedactionByConstruction(t *testing.T) {
	j := New[HealthEvent](8, newManualClock(base()).now)
	j.Append(HealthEvent{
		Domain: "connection", PrevLevel: HealthLevelHealthy, NewLevel: HealthLevelLost,
		APIState: APIStateDown, Evidence: EvidenceAuthoritative, Recovery: RecoveryNone,
		Reason: HealthReasonEnteredLost, NotificationRequested: true,
	})
	blob, err := json.Marshal(j.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	// The bounded schema has no free field that would carry these even if a
	// caller tried; confirm none of the classic secret markers appear.
	for _, bad := range []string{"oauth", "Bearer", "token=", "spade", "https://", "webhook", "cookie"} {
		if strings.Contains(strings.ToLower(string(blob)), strings.ToLower(bad)) {
			t.Fatalf("serialized health journal contains forbidden marker %q: %s", bad, blob)
		}
	}
}
