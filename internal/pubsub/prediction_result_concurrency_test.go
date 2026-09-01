package pubsub

import (
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// R3 — concurrent same-event result deliveries must converge to exactly one
// authoritative terminal admission. The start gate makes every worker hit the
// handler together; the logical exactly-once assertion (not the race
// detector) is the oracle. Run under -race.
func TestPredictionResultConcurrentDuplicatesConvergeToOneWinner(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-r3-concurrent", "chan-a3-r3", 100000)
	confirmedRound(t, pool, s, "a3-r3-evt", 500)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			pool.handlePredictionUser(resultMsg("a3-r3-evt", "WIN", 1000), s)
		}()
	}
	close(start)
	wg.Wait()

	if got := rec.count(); got != 1 {
		t.Fatalf("%d concurrent duplicates produced %d BetResults, want exactly 1", workers, got)
	}
	if got := ringBetResults(s.GetUsername()); got != 1 {
		t.Errorf("ring recorded %d bet_result events, want exactly 1", got)
	}
	counter, amount := historyEntry(t, s, "PREDICTION")
	if counter != 0 || amount != -500 {
		t.Errorf("PREDICTION history = {%d, %d}, want single-delivery {0, -500}", counter, amount)
	}
}

// R12 (concurrent leg) — concurrent permuted duplicates of two DISTINCT
// events must each be applied exactly once, independently.
func TestPredictionResultDistinctEventsIndependentUnderConcurrency(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-r12c-distinct", "chan-a3-r12c", 100000)
	confirmedRound(t, pool, s, "a3-r12c-a", 500)
	confirmedRound(t, pool, s, "a3-r12c-b", 500)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	deliveries := []string{"a3-r12c-a", "a3-r12c-b", "a3-r12c-a", "a3-r12c-b", "a3-r12c-b", "a3-r12c-a"}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, id := range deliveries {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			pool.handlePredictionUser(resultMsg(id, "WIN", 1000), s)
		}()
	}
	close(start)
	wg.Wait()

	if got := rec.count(); got != 2 {
		t.Fatalf("concurrent duplicates of two events produced %d BetResults, want exactly 2", got)
	}
	seen := map[string]int{}
	rec.mu.Lock()
	for _, r := range rec.results {
		seen[r.EventID]++
	}
	rec.mu.Unlock()
	if seen["a3-r12c-a"] != 1 || seen["a3-r12c-b"] != 1 {
		t.Errorf("per-event emissions = %v, want exactly one per event", seen)
	}
}

// Queue same-event contenders behind the pool lock: the test holds p.mu for
// writing while all workers are launched — no contender can pass the
// handler's first lookup (let alone complete admission) until the gate
// opens, so releasing the lock lets the whole group contend for the
// authoritative admission section together. A design where the mutex merely
// serializes processing without domain once-admission fails this with N
// admissions; the repaired admission yields exactly one accepted verdict and
// one set of effects.
func TestPredictionResultQueuedContendersAdmitExactlyOne(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-queued", "chan-a3-q", 100000)
	confirmedRound(t, pool, s, "a3-q-evt", 500)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	const workers = 8
	accepted := make(chan bool, workers)
	var started, done sync.WaitGroup

	pool.mu.Lock()
	for i := 0; i < workers; i++ {
		started.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			started.Done()
			out := pool.handlePredictionUser(resultMsg("a3-q-evt", "WIN", 1000), s)
			accepted <- out.PredictionResultAccepted
		}()
	}
	// Every worker goroutine exists before the gate opens; while the write
	// lock is held none of them can get past the handler's first lookup.
	started.Wait()
	pool.mu.Unlock()
	done.Wait()
	close(accepted)

	wins := 0
	for a := range accepted {
		if a {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("%d queued contenders got %d accepted verdicts, want exactly 1", workers, wins)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("queued contenders produced %d BetResults, want exactly 1", got)
	}
	counter, amount := historyEntry(t, s, "PREDICTION")
	if counter != 0 || amount != -500 {
		t.Errorf("PREDICTION history = {%d, %d}, want single-delivery {0, -500}", counter, amount)
	}
}

// Deterministic cleanup-wins interleaving: contenders are queued at the pool
// lock, and the round is removed under that same held lock BEFORE any of
// them can look it up — zero admissions and zero effects must follow.
func TestPredictionResultCleanupBeforeAdmissionYieldsZero(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-cleanupfirst", "chan-a3-cff", 100000)
	confirmedRound(t, pool, s, "a3-cff-evt", 500)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	const workers = 4
	accepted := make(chan bool, workers)
	var started, done sync.WaitGroup

	pool.mu.Lock()
	for i := 0; i < workers; i++ {
		started.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			started.Done()
			out := pool.handlePredictionUser(resultMsg("a3-cff-evt", "WIN", 1000), s)
			accepted <- out.PredictionResultAccepted
		}()
	}
	started.Wait()
	// Retire the round while the write lock is still held — the same state
	// transition removePrediction performs, linearized before every queued
	// contender.
	delete(pool.predictions, "a3-cff-evt")
	delete(pool.control, "a3-cff-evt")
	pool.mu.Unlock()
	done.Wait()
	close(accepted)

	for a := range accepted {
		if a {
			t.Fatal("a contender was admitted after the round was retired")
		}
	}
	if got := rec.count(); got != 0 {
		t.Fatalf("retired round produced %d BetResults, want 0", got)
	}
	counter, amount := historyEntry(t, s, "PREDICTION")
	if counter != 0 || amount != 0 {
		t.Errorf("PREDICTION history = {%d, %d}, want untouched", counter, amount)
	}
}

// Stale-authority replacement: after a handler could have observed the old
// tracked object, the pool's entry is replaced by a NEW confirmed object for
// the same event_id. Admission must commit against the current authoritative
// object (re-read under the admission lock), never against the retired one.
func TestPredictionResultReplacementCommitsAgainstCurrentObject(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-replace", "chan-a3-rp", 100000)
	retired := confirmedRound(t, pool, s, "a3-rp-evt", 500)

	// Replace the tracked entry with a fresh confirmed round object under the
	// pool lock, retiring the original object.
	current := models.NewEventPrediction(s, "a3-rp-evt", "Will they win?", time.Now(), 3600, "ACTIVE", []interface{}{
		map[string]interface{}{"id": "o1", "title": "Yes", "total_points": float64(300), "total_users": float64(3)},
		map[string]interface{}{"id": "o2", "title": "No", "total_points": float64(200), "total_users": float64(2)},
	})
	current.Bet.Decision = models.Decision{Choice: 0, Amount: 500, ID: "o1"}
	current.BetPlaced = true
	current.BetConfirmed = true
	pool.mu.Lock()
	pool.predictions["a3-rp-evt"] = current
	pool.mu.Unlock()

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	out := pool.handlePredictionUser(resultMsg("a3-rp-evt", "WIN", 1000), s)
	if !out.PredictionResultAccepted {
		t.Fatal("result for the current tracked object must be admitted")
	}
	if !current.ResultAccepted || current.Result.Type != models.ResultWin {
		t.Fatalf("current object not committed: accepted=%v result=%q", current.ResultAccepted, current.Result.Type)
	}
	if retired.ResultAccepted || retired.Result.Type != "" {
		t.Fatalf("retired stale object was mutated: accepted=%v result=%q", retired.ResultAccepted, retired.Result.Type)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("replacement round emitted %d BetResults, want exactly 1", got)
	}
}

// Admission racing the round's other owners (removePrediction cleanup and the
// RESOLVED channel update): whichever interleaving the scheduler picks, there
// is never more than one terminal admission and no data race.
func TestPredictionResultAdmissionRacesCleanupSafely(t *testing.T) {
	for i := 0; i < 50; i++ {
		pool := newTestPool(&fakePlacer{})
		s := newNamedTestStreamer("a3-race-cleanup", "chan-a3-rc", 100000)
		confirmedRound(t, pool, s, "a3-rc-evt", 500)

		rec := &betResultRecorder{}
		pool.SetBetResultHandler(rec.handler)

		start := make(chan struct{})
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				pool.handlePredictionUser(resultMsg("a3-rc-evt", "WIN", 1000), s)
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			pool.handlePredictionChannel(channelEventUpdateMsg("a3-rc-evt", "RESOLVED"), s)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			pool.removePrediction("a3-rc-evt")
		}()
		close(start)
		wg.Wait()

		if got := rec.count(); got > 1 {
			t.Fatalf("iteration %d: %d admissions with concurrent cleanup, want at most 1", i, got)
		}
	}

	// Anti-vacuity, deterministic: cleanup may legitimately win every racing
	// iteration above under some schedulers, so the proof that the delivery
	// seam actually admits runs once more WITHOUT a competing remover — this
	// must always yield exactly one admission.
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-race-cleanup-final", "chan-a3-rcf", 100000)
	confirmedRound(t, pool, s, "a3-rcf-evt", 500)
	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)
	pool.handlePredictionUser(resultMsg("a3-rcf-evt", "WIN", 1000), s)
	if got := rec.count(); got != 1 {
		t.Fatalf("deterministic no-remover delivery admitted %d results, want exactly 1", got)
	}
}
