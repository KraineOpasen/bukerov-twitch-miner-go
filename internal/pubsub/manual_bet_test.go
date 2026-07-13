package pubsub

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// fakePlacer is a test double for the Twitch prediction-placement call. It
// records how many times it was invoked (to prove no double-bet ever reaches
// Twitch), can be told to fail, to add latency (to widen the auto/manual race
// window), or to block on a channel (to hold a manual bet "in flight").
type fakePlacer struct {
	mu      sync.Mutex
	calls   int
	lastID  string
	lastAmt int
	err     error
	delay   time.Duration
	block   chan struct{}
}

func (f *fakePlacer) PlacePredictionBet(event *models.EventPrediction, outcomeID string, amount int) error {
	if f.block != nil {
		<-f.block
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastID = outcomeID
	f.lastAmt = amount
	return f.err
}

func (f *fakePlacer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newTestStreamer(points int) *models.Streamer {
	s := models.NewStreamer("streamer", models.DefaultStreamerSettings())
	s.ChannelID = "chan-1"
	s.SetOnline()
	s.SetChannelPoints(points)
	return s
}

func newTestPool(placer predictionPlacer) *WebSocketPool {
	return &WebSocketPool{
		predictions: make(map[string]*models.EventPrediction),
		control:     make(map[string]*roundControl),
		placer:      placer,
	}
}

// addRound registers an ACTIVE round with two outcomes (ids o1/o2) whose window
// is well in the future so it is bettable.
func addRound(pool *WebSocketPool, streamer *models.Streamer, eventID string) *models.EventPrediction {
	return addRoundWithOutcomes(pool, streamer, eventID, "o1", "o2")
}

func addRoundWithOutcomes(pool *WebSocketPool, streamer *models.Streamer, eventID, id1, id2 string) *models.EventPrediction {
	outcomes := []interface{}{
		map[string]interface{}{"id": id1, "title": "Yes", "total_points": float64(300), "total_users": float64(3)},
		map[string]interface{}{"id": id2, "title": "No", "total_points": float64(200), "total_users": float64(2)},
	}
	ep := models.NewEventPrediction(streamer, eventID, "Will they win?", time.Now(), 3600, "ACTIVE", outcomes)
	pool.mu.Lock()
	pool.predictions[eventID] = ep
	pool.control[eventID] = &roundControl{}
	pool.mu.Unlock()
	return ep
}

func (p *WebSocketPool) controlFor(eventID string) *roundControl {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.control[eventID]
}

func TestManualBetSuccess(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	s := newTestStreamer(100000)
	ep := addRound(pool, s, "e1")

	title, err := pool.PlaceManualBet("e1", "o1", 500)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if title != "Yes" {
		t.Errorf("outcome title = %q, want Yes", title)
	}
	if placer.callCount() != 1 {
		t.Errorf("placer called %d times, want 1", placer.callCount())
	}
	if placer.lastID != "o1" || placer.lastAmt != 500 {
		t.Errorf("placer got id=%q amt=%d", placer.lastID, placer.lastAmt)
	}
	if !ep.BetPlaced {
		t.Error("BetPlaced should be true")
	}
	rc := pool.controlFor("e1")
	if !rc.manualBet || !rc.autoBetSkip {
		t.Errorf("control flags: manualBet=%v autoBetSkip=%v", rc.manualBet, rc.autoBetSkip)
	}
	if ep.Bet.Decision.ID != "o1" || ep.Bet.Decision.Amount != 500 || ep.Bet.Decision.Choice != 0 {
		t.Errorf("decision = %+v", ep.Bet.Decision)
	}
}

func TestManualBetAmountValidation(t *testing.T) {
	cases := []struct {
		name   string
		amount int
		want   error
	}{
		{"zero", 0, ErrInvalidAmount},
		{"negative", -50, ErrInvalidAmount},
		{"too low", 5, ErrAmountTooLow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			placer := &fakePlacer{}
			pool := newTestPool(placer)
			addRound(pool, newTestStreamer(100000), "e1")
			_, err := pool.PlaceManualBet("e1", "o1", tc.amount)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if placer.callCount() != 0 {
				t.Errorf("placer must not be called for invalid amount")
			}
		})
	}
}

func TestManualBetExceedsBalance(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	addRound(pool, newTestStreamer(300), "e1")

	_, err := pool.PlaceManualBet("e1", "o1", 1000)
	if !errors.Is(err, ErrInsufficientPoints) {
		t.Fatalf("err = %v, want ErrInsufficientPoints", err)
	}
	if placer.callCount() != 0 {
		t.Error("placer must not be called when balance is insufficient")
	}
}

func TestManualBetUnknownPrediction(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	_, err := pool.PlaceManualBet("missing", "o1", 500)
	if !errors.Is(err, ErrPredictionNotFound) {
		t.Fatalf("err = %v, want ErrPredictionNotFound", err)
	}
}

func TestManualBetUnknownOutcome(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	addRound(pool, newTestStreamer(100000), "e1")
	_, err := pool.PlaceManualBet("e1", "does-not-exist", 500)
	if !errors.Is(err, ErrOutcomeNotFound) {
		t.Fatalf("err = %v, want ErrOutcomeNotFound", err)
	}
}

func TestManualBetOutcomeFromAnotherRound(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newTestStreamer(100000)
	addRoundWithOutcomes(pool, s, "e1", "a1", "a2")
	addRoundWithOutcomes(pool, s, "e2", "b1", "b2")

	// b1 belongs to e2, not e1.
	_, err := pool.PlaceManualBet("e1", "b1", 500)
	if !errors.Is(err, ErrOutcomeNotFound) {
		t.Fatalf("err = %v, want ErrOutcomeNotFound (foreign outcome)", err)
	}
}

func TestManualBetClosedRound(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	ep := addRound(pool, newTestStreamer(100000), "e1")
	ep.Status = models.PredictionLocked

	_, err := pool.PlaceManualBet("e1", "o1", 500)
	if !errors.Is(err, ErrRoundClosed) {
		t.Fatalf("err = %v, want ErrRoundClosed", err)
	}
}

func TestManualBetCanceledRound(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	ep := addRound(pool, newTestStreamer(100000), "e1")
	ep.Status = models.PredictionCanceled

	_, err := pool.PlaceManualBet("e1", "o1", 500)
	if !errors.Is(err, ErrRoundClosed) {
		t.Fatalf("err = %v, want ErrRoundClosed", err)
	}
}

func TestManualBetOfflineStreamer(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newTestStreamer(100000)
	s.SetOffline()
	addRound(pool, s, "e1")

	_, err := pool.PlaceManualBet("e1", "o1", 500)
	if !errors.Is(err, ErrStreamerOffline) {
		t.Fatalf("err = %v, want ErrStreamerOffline", err)
	}
}

func TestManualBetTwitchRejection(t *testing.T) {
	placer := &fakePlacer{err: errors.New("prediction error: NOT_ENOUGH_POINTS")}
	pool := newTestPool(placer)
	ep := addRound(pool, newTestStreamer(100000), "e1")

	_, err := pool.PlaceManualBet("e1", "o1", 500)
	if err == nil {
		t.Fatal("expected an error from Twitch rejection")
	}
	if !strings.Contains(err.Error(), "not enough channel points") {
		t.Errorf("error should be humanized, got %q", err.Error())
	}
	if ep.BetPlaced {
		t.Error("BetPlaced must stay false on Twitch rejection")
	}
	rc := pool.controlFor("e1")
	if rc.manualBet || rc.autoBetSkip {
		t.Error("no bookkeeping should be applied on failure")
	}
	if rc.manualErr == "" {
		t.Error("manualErr should be recorded")
	}
}

func TestManualBetNetworkFailureIsGeneric(t *testing.T) {
	placer := &fakePlacer{err: errors.New("request failed: context deadline exceeded")}
	pool := newTestPool(placer)
	addRound(pool, newTestStreamer(100000), "e1")

	_, err := pool.PlaceManualBet("e1", "o1", 500)
	if err == nil || !strings.Contains(err.Error(), "please try again") {
		t.Fatalf("expected generic retry message, got %v", err)
	}
}

func TestManualBetUnauthorizedIsFriendly(t *testing.T) {
	placer := &fakePlacer{err: api.ErrUnauthorized}
	pool := newTestPool(placer)
	addRound(pool, newTestStreamer(100000), "e1")

	_, err := pool.PlaceManualBet("e1", "o1", 500)
	if err == nil || !strings.Contains(err.Error(), "reauthorize") {
		t.Fatalf("expected reauthorize hint, got %v", err)
	}
}

func TestAutoBetLockedAfterManualBet(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	ep := addRound(pool, newTestStreamer(100000), "e1")

	if _, err := pool.PlaceManualBet("e1", "o1", 500); err != nil {
		t.Fatalf("manual bet: %v", err)
	}
	// The scheduled auto-bet must now be a no-op for this round.
	pool.placeAutoBet("e1")

	if placer.callCount() != 1 {
		t.Errorf("placer called %d times, want exactly 1 (auto-bet must be suppressed)", placer.callCount())
	}
	if ep.Bet.Decision.ID != "o1" {
		t.Error("manual decision should be preserved (auto-bet must not overwrite it)")
	}
}

func TestManualSkipSuppressesAutoBet(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	ep := addRound(pool, newTestStreamer(100000), "e1")

	if err := pool.SetAutoBetSkip("e1", true); err != nil {
		t.Fatalf("skip: %v", err)
	}
	pool.placeAutoBet("e1")

	if placer.callCount() != 0 {
		t.Errorf("placer called %d times, want 0 (skip must suppress auto-bet)", placer.callCount())
	}
	if ep.BetPlaced {
		t.Error("no bet should be placed on a skipped round")
	}
}

func TestCancelSkipBeforeAutoBet(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	addRound(pool, newTestStreamer(100000), "e1")

	if err := pool.SetAutoBetSkip("e1", true); err != nil {
		t.Fatalf("skip: %v", err)
	}
	if err := pool.SetAutoBetSkip("e1", false); err != nil {
		t.Fatalf("unskip: %v", err)
	}
	pool.placeAutoBet("e1")

	if placer.callCount() != 1 {
		t.Errorf("placer called %d times, want 1 (auto-bet should resume after un-skip)", placer.callCount())
	}
}

func TestTransientStateClearedAfterCleanup(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	addRound(pool, newTestStreamer(100000), "e1")
	_ = pool.SetAutoBetSkip("e1", true)

	pool.removePrediction("e1")

	pool.mu.RLock()
	_, hasPred := pool.predictions["e1"]
	_, hasCtl := pool.control["e1"]
	pool.mu.RUnlock()
	if hasPred || hasCtl {
		t.Error("prediction and control state must be gone after cleanup")
	}
	if len(pool.PredictionsSnapshot()) != 0 {
		t.Error("snapshot should be empty after cleanup")
	}
	// removePrediction is idempotent.
	pool.removePrediction("e1")
}

func TestManualBetDoesNotAffectNextRound(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	s := newTestStreamer(100000)
	addRound(pool, s, "e1")
	if _, err := pool.PlaceManualBet("e1", "o1", 500); err != nil {
		t.Fatalf("manual bet: %v", err)
	}

	// A subsequent round for the same streamer is handled by normal auto-bet.
	ep2 := addRound(pool, s, "e2")
	pool.placeAutoBet("e2")

	if placer.callCount() != 2 {
		t.Errorf("placer called %d times, want 2 (manual on e1 + auto on e2)", placer.callCount())
	}
	if !ep2.BetPlaced {
		t.Error("e2 should have an auto-bet placed")
	}
	if pool.controlFor("e2").manualBet {
		t.Error("e2 must not inherit e1's manual flag")
	}
}

func TestManualBetDoesNotAffectOtherActivePrediction(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newTestStreamer(100000)
	addRound(pool, s, "e1")
	addRound(pool, s, "e2")

	if _, err := pool.PlaceManualBet("e1", "o1", 500); err != nil {
		t.Fatalf("manual bet: %v", err)
	}
	rc2 := pool.controlFor("e2")
	if rc2.autoBetSkip || rc2.manualBet {
		t.Error("betting on e1 must not touch e2's control state")
	}
}

func TestConcurrentManualAndAutoNoDoubleBet(t *testing.T) {
	placer := &fakePlacer{delay: 20 * time.Millisecond}
	pool := newTestPool(placer)
	ep := addRound(pool, newTestStreamer(100000), "e1")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = pool.PlaceManualBet("e1", "o1", 500) }()
	go func() { defer wg.Done(); pool.placeAutoBet("e1") }()
	wg.Wait()

	if placer.callCount() != 1 {
		t.Fatalf("placer called %d times, want exactly 1 (no double bet)", placer.callCount())
	}
	if !ep.BetPlaced {
		t.Error("exactly one bet should be recorded as placed")
	}
}

func TestTwoConcurrentManualBets(t *testing.T) {
	placer := &fakePlacer{delay: 20 * time.Millisecond}
	pool := newTestPool(placer)
	addRound(pool, newTestStreamer(100000), "e1")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) { defer wg.Done(); _, errs[idx] = pool.PlaceManualBet("e1", "o1", 500) }(i)
	}
	wg.Wait()

	if placer.callCount() != 1 {
		t.Fatalf("placer called %d times, want exactly 1", placer.callCount())
	}
	success, failure := 0, 0
	for _, e := range errs {
		if e == nil {
			success++
		} else {
			failure++
		}
	}
	if success != 1 || failure != 1 {
		t.Errorf("want 1 success + 1 failure, got %d/%d (errs: %v)", success, failure, errs)
	}
}

func TestDuplicateInFlightRejected(t *testing.T) {
	release := make(chan struct{})
	placer := &fakePlacer{block: release}
	pool := newTestPool(placer)
	addRound(pool, newTestStreamer(100000), "e1")

	done := make(chan error, 1)
	go func() { _, err := pool.PlaceManualBet("e1", "o1", 500); done <- err }()

	// Wait until the first request has claimed the in-flight slot.
	deadline := time.After(2 * time.Second)
	for {
		if rc := pool.controlFor("e1"); rc != nil {
			pool.mu.RLock()
			pending := rc.manualPending
			pool.mu.RUnlock()
			if pending {
				break
			}
		}
		select {
		case <-deadline:
			t.Fatal("first bet never became pending")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// A second submission while the first is in flight is rejected immediately.
	if _, err := pool.PlaceManualBet("e1", "o1", 500); !errors.Is(err, ErrManualBetInFlight) {
		t.Errorf("second in-flight bet err = %v, want ErrManualBetInFlight", err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Errorf("first bet should have succeeded, got %v", err)
	}
	if placer.callCount() != 1 {
		t.Errorf("placer called %d times, want 1", placer.callCount())
	}
}

func TestSweepStaleBoundsGrowth(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newTestStreamer(100000)

	// A pile of finished-but-never-cleaned rounds with old timestamps.
	for i := 0; i < 50; i++ {
		id := "old-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		ep := models.NewEventPrediction(s, id, "old", time.Now().Add(-3*time.Hour), 60, "RESOLVED", nil)
		pool.mu.Lock()
		pool.predictions[id] = ep
		pool.control[id] = &roundControl{}
		pool.mu.Unlock()
	}
	// One fresh round that must be retained.
	addRound(pool, s, "fresh")

	pool.mu.Lock()
	pool.sweepStaleLocked()
	remaining := len(pool.predictions)
	_, freshKept := pool.predictions["fresh"]
	ctlRemaining := len(pool.control)
	pool.mu.Unlock()

	if remaining != 1 || !freshKept {
		t.Errorf("sweep left %d predictions (fresh kept=%v), want only the fresh one", remaining, freshKept)
	}
	if ctlRemaining != 1 {
		t.Errorf("control map left %d entries, want 1", ctlRemaining)
	}
}

func TestSetAutoBetSkipUnknownRound(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	if err := pool.SetAutoBetSkip("nope", true); !errors.Is(err, ErrPredictionNotFound) {
		t.Fatalf("err = %v, want ErrPredictionNotFound", err)
	}
}

func TestSkipAfterBetRejected(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	addRound(pool, newTestStreamer(100000), "e1")
	if _, err := pool.PlaceManualBet("e1", "o1", 500); err != nil {
		t.Fatalf("manual bet: %v", err)
	}
	if err := pool.SetAutoBetSkip("e1", true); !errors.Is(err, ErrAlreadyBet) {
		t.Errorf("skip after manual bet err = %v, want ErrAlreadyBet", err)
	}
}
