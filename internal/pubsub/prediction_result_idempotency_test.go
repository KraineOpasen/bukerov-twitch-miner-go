package pubsub

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

var a3StreamerSequence atomic.Uint64

// newNamedTestStreamer is newTestStreamer with a per-invocation unique
// username, so filtering the process-wide events ring stays hermetic even
// under `go test -count=N` (the ring is never reset between repetitions).
func newNamedTestStreamer(name, channelID string, points int) *models.Streamer {
	s := models.NewStreamer(fmt.Sprintf("%s-%d", name, a3StreamerSequence.Add(1)), models.DefaultStreamerSettings())
	s.ChannelID = channelID
	s.SetConfirmedOnline()
	s.SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)
	s.SetChannelPoints(points)
	return s
}

// betResultRecorder counts BetResult emissions, safe for concurrent handlers.
type betResultRecorder struct {
	mu      sync.Mutex
	results []BetResult
}

func (r *betResultRecorder) handler(res BetResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, res)
}

func (r *betResultRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.results)
}

func (r *betResultRecorder) first() BetResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.results) == 0 {
		return BetResult{}
	}
	return r.results[0]
}

// ringBetResults counts TypeBetResult ring records for one streamer name.
func ringBetResults(streamer string) int {
	n := 0
	for _, e := range events.Recent(200) {
		if e.Type == events.TypeBetResult && e.Streamer == streamer {
			n++
		}
	}
	return n
}

func historyEntry(t *testing.T, s *models.Streamer, reason string) (counter, amount int) {
	t.Helper()
	e := s.History[reason]
	if e == nil {
		return 0, 0
	}
	return e.Counter, e.Amount
}

// R1/R4 — a sequential same-event duplicate WIN must apply terminal effects
// exactly once: one BetResult, one ring record, single-delivery History.
func TestPredictionResultSequentialDuplicateWinAppliedOnce(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-r1-win", "chan-a3-r1", 100000)
	ep := confirmedRound(t, pool, s, "a3-r1-evt", 500)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	pool.handlePredictionUser(resultMsg("a3-r1-evt", "WIN", 1000), s)
	pool.handlePredictionUser(resultMsg("a3-r1-evt", "WIN", 1000), s)

	if got := rec.count(); got != 1 {
		t.Fatalf("BetResult emitted %d times for a duplicate WIN, want exactly 1", got)
	}
	if got := ringBetResults(s.GetUsername()); got != 1 {
		t.Errorf("ring recorded %d bet_result events, want exactly 1", got)
	}
	// Single WIN delivery: UpdateHistory(PREDICTION, +500) then
	// UpdateHistoryWithCounter(PREDICTION, -1000, -1) => {0, -500}.
	counter, amount := historyEntry(t, s, "PREDICTION")
	if counter != 0 || amount != -500 {
		t.Errorf("PREDICTION history = {%d, %d}, want single-delivery {0, -500}", counter, amount)
	}
	if ep.Result.Type != models.ResultWin {
		t.Errorf("event result type = %q, want WIN", ep.Result.Type)
	}
}

// R5 — a sequential duplicate LOSE must apply terminal accounting once.
func TestPredictionResultSequentialDuplicateLoseAppliedOnce(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-r5-lose", "chan-a3-r5", 100000)
	confirmedRound(t, pool, s, "a3-r5-evt", 500)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	pool.handlePredictionUser(resultMsg("a3-r5-evt", "LOSE", 0), s)
	pool.handlePredictionUser(resultMsg("a3-r5-evt", "LOSE", 0), s)

	if got := rec.count(); got != 1 {
		t.Fatalf("BetResult emitted %d times for a duplicate LOSE, want exactly 1", got)
	}
	// Single LOSE delivery: UpdateHistory(PREDICTION, -500) => {1, -500}.
	counter, amount := historyEntry(t, s, "PREDICTION")
	if counter != 1 || amount != -500 {
		t.Errorf("PREDICTION history = {%d, %d}, want single-delivery {1, -500}", counter, amount)
	}
	if got := ringBetResults(s.GetUsername()); got != 1 {
		t.Errorf("ring recorded %d bet_result events, want exactly 1", got)
	}
}

// R6 — a sequential duplicate REFUND must apply the raw-stake/refund
// accounting contract exactly once.
func TestPredictionResultSequentialDuplicateRefundAppliedOnce(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-r6-refund", "chan-a3-r6", 100000)
	confirmedRound(t, pool, s, "a3-r6-evt", 750)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	pool.handlePredictionUser(resultMsg("a3-r6-evt", "REFUND", 0), s)
	pool.handlePredictionUser(resultMsg("a3-r6-evt", "REFUND", 0), s)

	if got := rec.count(); got != 1 {
		t.Fatalf("BetResult emitted %d times for a duplicate REFUND, want exactly 1", got)
	}
	// The live raw-stake contract: the emitted record keeps the stake even
	// though ParseResult zeroes `placed` for a REFUND.
	if got := rec.first(); got.ResultType != "REFUND" || got.Placed != 750 || got.Gained != 0 {
		t.Errorf("refund record = %+v, want REFUND with raw stake 750 and gained 0", got)
	}
	// Single REFUND delivery: UpdateHistory(PREDICTION, 0) => {1, 0};
	// UpdateHistoryWithCounter(REFUND, 0, -1) => {-1, 0}.
	pCounter, pAmount := historyEntry(t, s, "PREDICTION")
	if pCounter != 1 || pAmount != 0 {
		t.Errorf("PREDICTION history = {%d, %d}, want single-delivery {1, 0}", pCounter, pAmount)
	}
	rCounter, rAmount := historyEntry(t, s, "REFUND")
	if rCounter != -1 || rAmount != 0 {
		t.Errorf("REFUND history = {%d, %d}, want single-delivery {-1, 0}", rCounter, rAmount)
	}
}

// predictionResultWire builds a wire-level predictions-user frame whose inner
// payload carries a harmless timestamp distinction, so two domain-equivalent
// results can have distinct transport fingerprints.
func predictionResultWire(eventID, channelID, resultType string, pointsWon float64, timestamp string) WSMessage {
	return WSMessage{Type: "MESSAGE", Data: &WSData{
		Topic: "predictions-user-v1.42",
		Message: fmt.Sprintf(
			`{"type":"prediction-result","data":{"timestamp":%q,"prediction":{"event_id":%q,"channel_id":%q,"result":{"type":%q,"points_won":%v}}}}`,
			timestamp, eventID, channelID, resultType, pointsWon),
	}}
}

// R2 — a semantically equivalent result with a DIFFERENT transport fingerprint
// bypasses the per-connection replay window by design; the domain layer must
// still apply terminal effects exactly once. Delivered through the normal
// websocket -> pool.handleMessage route.
func TestPredictionResultTransportDistinctDuplicateAppliedOnce(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-r2-transport", "chan-a3-r2", 100000)
	pool.streamers = []*models.Streamer{s}
	confirmedRound(t, pool, s, "a3-r2-evt", 500)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	ws := NewWebSocketClient(0, nil, 3600, 1, pool.handleMessage, nil)
	ws.handleMessage(predictionResultWire("a3-r2-evt", "chan-a3-r2", "WIN", 1000, "2026-08-25T10:00:00Z"))
	ws.handleMessage(predictionResultWire("a3-r2-evt", "chan-a3-r2", "WIN", 1000, "2026-08-25T10:00:00.100Z"))

	if got := rec.count(); got != 1 {
		t.Fatalf("BetResult emitted %d times for a transport-distinct duplicate, want exactly 1", got)
	}
	if got := ringBetResults(s.GetUsername()); got != 1 {
		t.Errorf("ring recorded %d bet_result events, want exactly 1", got)
	}
}

// R7 — a valid-looking terminal result before the bet is confirmed is
// rejected AND must not consume the round: after confirmation the next valid
// result is accepted, exactly once.
func TestPredictionResultUnconfirmedRejectionDoesNotPoison(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-r7-unconf", "chan-a3-r7", 100000)
	ep := confirmedRound(t, pool, s, "a3-r7-evt", 500)
	ep.BetConfirmed = false

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	pool.handlePredictionUser(resultMsg("a3-r7-evt", "WIN", 1000), s)
	if got := rec.count(); got != 0 {
		t.Fatalf("unconfirmed result emitted %d BetResults, want 0", got)
	}

	ep.BetConfirmed = true
	pool.handlePredictionUser(resultMsg("a3-r7-evt", "WIN", 1000), s)
	pool.handlePredictionUser(resultMsg("a3-r7-evt", "WIN", 1000), s)

	if got := rec.count(); got != 1 {
		t.Fatalf("post-confirmation result emitted %d BetResults, want exactly 1", got)
	}
}

// R8 — malformed input (missing result object; then missing result type) is
// rejected without terminal effects and must not consume the round.
func TestPredictionResultMalformedInputDoesNotPoison(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-r8-malformed", "chan-a3-r8", 100000)
	confirmedRound(t, pool, s, "a3-r8-evt", 500)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	// Missing result object entirely.
	pool.handlePredictionUser(&PubSubMessage{
		Type: "prediction-result",
		Data: map[string]interface{}{
			"prediction": map[string]interface{}{"event_id": "a3-r8-evt"},
		},
	}, s)
	if got := rec.count(); got != 0 {
		t.Fatalf("missing-result message emitted %d BetResults, want 0", got)
	}

	// Result object present but without a type: must be rejected, not parsed
	// through ParseResult's default branch.
	pool.handlePredictionUser(&PubSubMessage{
		Type: "prediction-result",
		Data: map[string]interface{}{
			"prediction": map[string]interface{}{
				"event_id": "a3-r8-evt",
				"result":   map[string]interface{}{"points_won": float64(1000)},
			},
		},
	}, s)
	if got := rec.count(); got != 0 {
		t.Fatalf("missing-type message emitted %d BetResults, want 0", got)
	}

	// A later valid terminal result must still win, exactly once.
	pool.handlePredictionUser(resultMsg("a3-r8-evt", "WIN", 1000), s)
	pool.handlePredictionUser(resultMsg("a3-r8-evt", "WIN", 1000), s)
	if got := rec.count(); got != 1 {
		t.Fatalf("valid result after malformed input emitted %d BetResults, want exactly 1", got)
	}
	if got := rec.first(); got.ResultType != "WIN" {
		t.Errorf("accepted result type = %q, want WIN", got.ResultType)
	}
}

// R9 — an unsupported terminal result type is rejected without effects and
// must not consume the round; a later supported result is accepted once.
func TestPredictionResultUnsupportedTypeDoesNotPoison(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-r9-unsupported", "chan-a3-r9", 100000)
	confirmedRound(t, pool, s, "a3-r9-evt", 500)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	pool.handlePredictionUser(resultMsg("a3-r9-evt", "CANCELED", 0), s)
	if got := rec.count(); got != 0 {
		t.Fatalf("unsupported result type emitted %d BetResults, want 0", got)
	}
	counter, amount := historyEntry(t, s, "PREDICTION")
	if counter != 0 || amount != 0 {
		t.Fatalf("unsupported result type wrote PREDICTION history {%d, %d}, want none", counter, amount)
	}

	pool.handlePredictionUser(resultMsg("a3-r9-evt", "LOSE", 0), s)
	pool.handlePredictionUser(resultMsg("a3-r9-evt", "LOSE", 0), s)
	if got := rec.count(); got != 1 {
		t.Fatalf("valid LOSE after unsupported type emitted %d BetResults, want exactly 1", got)
	}
	if got := rec.first(); got.ResultType != "LOSE" {
		t.Errorf("accepted result type = %q, want LOSE", got.ResultType)
	}
}

// R10 (pool half) — after the tracked round is cleaned up, a re-delivered
// result through the normal websocket route produces no pool terminal effect.
func TestPredictionResultAfterCleanupProducesNoPoolEffect(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-r10-cleanup", "chan-a3-r10", 100000)
	pool.streamers = []*models.Streamer{s}
	confirmedRound(t, pool, s, "a3-r10-evt", 500)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	ws := NewWebSocketClient(0, nil, 3600, 1, pool.handleMessage, nil)
	ws.handleMessage(predictionResultWire("a3-r10-evt", "chan-a3-r10", "WIN", 1000, "2026-08-25T10:00:00Z"))
	if got := rec.count(); got != 1 {
		t.Fatalf("first result emitted %d BetResults, want 1", got)
	}

	pool.removePrediction("a3-r10-evt")

	ws.handleMessage(predictionResultWire("a3-r10-evt", "chan-a3-r10", "WIN", 1000, "2026-08-25T10:00:05Z"))
	if got := rec.count(); got != 1 {
		t.Fatalf("post-cleanup result emitted %d total BetResults, want still 1", got)
	}
	counter, amount := historyEntry(t, s, "PREDICTION")
	if counter != 0 || amount != -500 {
		t.Errorf("PREDICTION history = {%d, %d}, want single-delivery {0, -500}", counter, amount)
	}
}

// channelEventUpdateMsg builds a predictions-channel event-updated message.
func channelEventUpdateMsg(eventID, status string) *PubSubMessage {
	return &PubSubMessage{
		Type: "event-updated",
		Data: map[string]interface{}{
			"event": map[string]interface{}{"id": eventID, "status": status},
		},
	}
}

// R11 — the RESOLVED channel update schedules grace cleanup but must not
// prevent the legitimate predictions-user result from being accepted once
// within the grace window; duplicates inside the window stay inert.
func TestPredictionResultAcceptedOnceDuringTerminalGrace(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-r11-grace", "chan-a3-r11", 100000)
	confirmedRound(t, pool, s, "a3-r11-evt", 500)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	pool.handlePredictionChannel(channelEventUpdateMsg("a3-r11-evt", "RESOLVED"), s)

	pool.handlePredictionUser(resultMsg("a3-r11-evt", "WIN", 1000), s)
	if got := rec.count(); got != 1 {
		t.Fatalf("result during grace emitted %d BetResults, want 1", got)
	}
	pool.handlePredictionUser(resultMsg("a3-r11-evt", "WIN", 1000), s)
	if got := rec.count(); got != 1 {
		t.Fatalf("duplicate during grace emitted %d total BetResults, want still 1", got)
	}
}

// R12 — two distinct event IDs are independent: duplicates of each, delivered
// permuted, are each applied exactly once.
func TestPredictionResultDistinctEventsIndependent(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-r12-distinct", "chan-a3-r12", 100000)
	confirmedRound(t, pool, s, "a3-r12-a", 500)
	confirmedRound(t, pool, s, "a3-r12-b", 500)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	pool.handlePredictionUser(resultMsg("a3-r12-a", "WIN", 1000), s)
	pool.handlePredictionUser(resultMsg("a3-r12-b", "LOSE", 0), s)
	pool.handlePredictionUser(resultMsg("a3-r12-b", "LOSE", 0), s)
	pool.handlePredictionUser(resultMsg("a3-r12-a", "WIN", 1000), s)

	if got := rec.count(); got != 2 {
		t.Fatalf("two distinct events emitted %d BetResults, want exactly 2", got)
	}
	seen := map[string]int{}
	rec.mu.Lock()
	for _, r := range rec.results {
		seen[r.EventID]++
	}
	rec.mu.Unlock()
	if seen["a3-r12-a"] != 1 || seen["a3-r12-b"] != 1 {
		t.Errorf("per-event emissions = %v, want exactly one per event", seen)
	}
}

// R13 — a conflicting second terminal result (LOSE after an accepted WIN) is
// inert: the first accepted result stays authoritative and its snapshot is
// never overwritten.
func TestPredictionResultConflictingSecondResultInert(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-r13-conflict", "chan-a3-r13", 100000)
	ep := confirmedRound(t, pool, s, "a3-r13-evt", 500)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	pool.handlePredictionUser(resultMsg("a3-r13-evt", "WIN", 1000), s)
	pool.handlePredictionUser(resultMsg("a3-r13-evt", "LOSE", 0), s)

	if got := rec.count(); got != 1 {
		t.Fatalf("conflicting second result emitted %d total BetResults, want 1", got)
	}
	if got := rec.first(); got.ResultType != "WIN" {
		t.Errorf("emitted result type = %q, want the first accepted WIN", got.ResultType)
	}
	if ep.Result.Type != models.ResultWin {
		t.Errorf("event result snapshot = %q, want WIN preserved", ep.Result.Type)
	}
	counter, amount := historyEntry(t, s, "PREDICTION")
	if counter != 0 || amount != -500 {
		t.Errorf("PREDICTION history = {%d, %d}, want single-WIN {0, -500}", counter, amount)
	}
}

// R14 — an empty/missing event_id is rejected outright: it must not collide
// with a (pathologically) tracked empty-keyed round, and a later valid event
// still resolves exactly once.
func TestPredictionResultEmptyEventIDRejected(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-r14-empty", "chan-a3-r14", 100000)
	confirmedRound(t, pool, s, "", 500)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	// No event_id key at all: extraction yields "" and must be rejected, not
	// matched against the empty-keyed round.
	pool.handlePredictionUser(&PubSubMessage{
		Type: "prediction-result",
		Data: map[string]interface{}{
			"prediction": map[string]interface{}{
				"result": map[string]interface{}{"type": "WIN", "points_won": float64(1000)},
			},
		},
	}, s)
	if got := rec.count(); got != 0 {
		t.Fatalf("missing event_id emitted %d BetResults, want 0", got)
	}

	// Explicit empty event_id: same rejection.
	pool.handlePredictionUser(resultMsg("", "WIN", 1000), s)
	if got := rec.count(); got != 0 {
		t.Fatalf("empty event_id emitted %d BetResults, want 0", got)
	}

	// A real round is unaffected by the empty-ID rejections.
	confirmedRound(t, pool, s, "a3-r14-real", 500)
	pool.handlePredictionUser(resultMsg("a3-r14-real", "WIN", 1000), s)
	pool.handlePredictionUser(resultMsg("a3-r14-real", "WIN", 1000), s)
	if got := rec.count(); got != 1 {
		t.Fatalf("real event after empty-ID rejections emitted %d BetResults, want exactly 1", got)
	}
}
