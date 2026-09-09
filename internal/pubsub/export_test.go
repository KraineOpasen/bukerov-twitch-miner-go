package pubsub

// Test-only seam for driving a REAL automatic betting decision from outside
// this package.
//
// It exists for exactly one reason. The P1.5 capture contract has to be proved
// END TO END — the pool's producer, the miner's translation adapter, the
// analytics collector, the store and the public reader — and the miner cannot
// be imported by this package's own tests (it imports this one). The proof
// therefore lives in the external `pubsub_test` package, which can import both,
// and that package cannot reach `placeAutoBet`, `p.predictions` or
// `newRoundIncarnation` on its own.
//
// Everything here is compiled ONLY into the test binary: this file never ships,
// adds no production API, and changes no production behaviour. It deliberately
// exposes a driver rather than the internals themselves, so a test cannot
// assemble a round in a way the pool's own admission path never would.

import (
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestAutoBetHarness drives one pool through real auto-bet decisions.
type TestAutoBetHarness struct {
	pool     *WebSocketPool
	placer   *fakePlacer
	streamer *models.Streamer
}

// NewTestAutoBetHarness builds a pool with a fake placer and one online,
// points-enabled streamer holding the given balance — the same fixture the
// in-package betting tests use, so a decision driven here takes exactly the
// production path.
func NewTestAutoBetHarness(instanceID string, balance int) *TestAutoBetHarness {
	placer := &fakePlacer{}
	p := newTestPool(placer)
	p.instanceID = instanceID
	s := newTestStreamer(balance)
	p.streamers = []*models.Streamer{s}
	return &TestAutoBetHarness{pool: p, placer: placer, streamer: s}
}

// Pool exposes the pool so a caller can attach a real observation sink through
// the ordinary exported SetPredictionObservationSink.
func (h *TestAutoBetHarness) Pool() *WebSocketPool { return h.pool }

// SetRisk applies the global risk gates.
func (h *TestAutoBetHarness) SetRisk(r config.PredictionRiskSettings) { h.pool.SetRiskSettings(r) }

// AdmitRound registers an ACTIVE round the way the pool's own admission path
// does: a local admission incarnation from this pool instance, and the round's
// capture provenance frozen at the admission itself.
func (h *TestAutoBetHarness) AdmitRound(eventID string, settings models.BetSettings, outcomes []interface{}) {
	ep := models.NewEventPrediction(h.streamer, eventID, "Will they win?", time.Now(), 3600, "ACTIVE", outcomes)
	ep.Bet.Settings = settings
	h.pool.mu.Lock()
	h.pool.predictions[eventID] = ep
	control := &roundControl{incarnation: h.pool.newRoundIncarnation()}
	h.pool.control[eventID] = control
	h.pool.freezeRoundProvenance(control.incarnation, h.streamer.ChannelID, h.streamer.GetUsername())
	h.pool.mu.Unlock()
}

// UpdateOutcomes feeds a frame through the model's own accumulator, which is
// the only path that derives TopPoints from top_predictors.
func (h *TestAutoBetHarness) UpdateOutcomes(eventID string, outcomes []interface{}) {
	h.pool.mu.Lock()
	h.pool.predictions[eventID].Bet.UpdateOutcomes(outcomes)
	h.pool.mu.Unlock()
}

// DriveAutoBet runs the real scheduled auto-bet for one round.
func (h *TestAutoBetHarness) DriveAutoBet(eventID string) { h.pool.placeAutoBet(eventID) }

// RoundIncarnation is the round's local admission identity, which is how a
// reader looks its facts up in the store.
func (h *TestAutoBetHarness) RoundIncarnation(eventID string) string {
	return h.pool.roundIncarnation(eventID)
}

// PlacementCalls is how many times the fake placer was invoked — the count that
// proves observation never added, removed or retried a Twitch call.
func (h *TestAutoBetHarness) PlacementCalls() int { return h.placer.callCount() }

// LastPlacement is the outcome id and stake the placer was last called with.
func (h *TestAutoBetHarness) LastPlacement() (string, int) {
	h.placer.mu.Lock()
	defer h.placer.mu.Unlock()
	return h.placer.lastID, h.placer.lastAmt
}
