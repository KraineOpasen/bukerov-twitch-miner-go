package pubsub

import (
	"sync"
	"testing"
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

// Admission racing the round's other owners (removePrediction cleanup and the
// RESOLVED channel update): whichever interleaving the scheduler picks, there
// is never more than one terminal admission and no data race.
func TestPredictionResultAdmissionRacesCleanupSafely(t *testing.T) {
	totalAdmissions := 0
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
		totalAdmissions += rec.count()
	}
	// Anti-vacuity: removePrediction may win individual iterations, but if no
	// iteration ever admitted, the race was not actually exercised.
	if totalAdmissions == 0 {
		t.Fatal("no iteration admitted a result; the admission/cleanup race was not exercised")
	}
}
