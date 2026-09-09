package pubsub

// Regression tests for the two properties the P1.5 review found unguarded:
// which round a decision's facts are filed under, and whether the captured
// inputs are really immune to later mutation.
//
// Both run under -race. That matters: the fully integrated end-to-end proof
// skips under the race detector (the collector's 5ms production write deadline
// is unmeetable there), so a property proved only in that file has no coverage
// in CI. These two do not need the store, so they close that gap at the
// producer seam, where both defects would actually originate.

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestAnAttemptsFactsStayBoundToTheRoundItResolved regresses a round-identity
// defect that only appears under a concurrent cleanup.
//
// Every fact of an auto attempt used to be filed under a FRESH lookup of the
// event id. A round can be cleaned up and the same Twitch event admitted again
// while an attempt is in flight, and the lookup then returns the NEW round's
// incarnation — so the decision envelope, with the settings, outcomes, balance
// and gates of the OLD round, gets filed against a round it was never about.
//
// The attempt already holds the incarnation it resolved, so the fix is to use
// it. A test that merely drives an attempt and reads the incarnation back
// cannot see this: it only appears when the round is replaced BETWEEN the
// attempt's first and last fact, which the sink hook does here.
func TestAnAttemptsFactsStayBoundToTheRoundItResolved(t *testing.T) {
	placer := &fakePlacer{}
	p, sink := observedPool(t, placer)
	s := newTestStreamer(100000)
	ep := admitRound(p, s, "bind-1")
	ep.Bet.Settings = autoBetSettings(5)

	p.mu.Lock()
	original := p.control["bind-1"].incarnation
	p.mu.Unlock()

	// The moment the attempt files its first fact, replace the round with a
	// fresh admission of the same Twitch event — exactly what a cleanup plus a
	// re-admission does while the timer is mid-flight.
	var replaced string
	sink.mu.Lock()
	sink.hook = func(obs PredictionObservation) {
		if obs.Payload.Phase != "AUTO_DUE" {
			return
		}
		p.mu.Lock()
		if p.control["bind-1"].incarnation == original {
			rc := &roundControl{incarnation: p.newRoundIncarnation()}
			p.control["bind-1"] = rc
			replaced = rc.incarnation
		}
		p.mu.Unlock()
	}
	sink.mu.Unlock()

	p.placeAutoBet("bind-1")

	if replaced == "" || replaced == original {
		t.Fatalf("the fixture did not actually replace the round: original=%q replaced=%q",
			original, replaced)
	}

	var checked int
	for _, o := range sink.all() {
		if o.Kind != ObsKindAutoDecision && o.Kind != ObsKindPlacement {
			continue
		}
		checked++
		if o.RoundIncarnationID != original {
			t.Fatalf("fact %q/%q was filed under incarnation %q, but the attempt resolved %q. "+
				"A decision's inputs recorded against a round it was never about are worse than "+
				"no record: nothing downstream can tell the two rounds apart",
				o.Kind, o.Payload.Phase, o.RoundIncarnationID, original)
		}
	}
	if checked < 4 {
		t.Fatalf("only %d attempt facts were checked; the due fact, the decision and both "+
			"placement calls all belong to the attempt", checked)
	}
}

// TestCapturedInputsDoNotFollowALaterMutation regresses the aliasing defect:
// capturing a pointer or a slice by reference rather than by value, so that a
// settings edit or an outcome update after the decision silently rewrites a
// fact that was already recorded.
//
// The equivalent end-to-end assertion skips under -race, and the only aliasing
// check that did run there asserted a value nothing had mutated — so it could
// not fail. This one mutates the round's real filter condition and its real
// outcome vector after the decision and before reading the captured fact back.
func TestCapturedInputsDoNotFollowALaterMutation(t *testing.T) {
	placer := &fakePlacer{}
	p, sink := observedPool(t, placer)
	s := newTestStreamer(100000)
	ep := admitRound(p, s, "alias-1")

	filter := &models.FilterCondition{
		By: models.OutcomeTotalUsers, Where: models.ConditionGT, Value: 1,
	}
	ep.Bet.Settings = models.BetSettings{
		Strategy: models.StrategyNumber1, Percentage: 5, PercentageGap: 20,
		MaxPoints: 50000, FilterCondition: filter,
		Delay: 6, DelayMode: models.DelayModeFromEnd,
	}
	p.SetRiskSettings(config.PredictionRiskSettings{HealthGateEnabled: false})

	p.placeAutoBet("alias-1")

	// Mutate everything the decision read, through the very pointers and slices
	// the round still holds.
	filter.Value = 999999
	filter.By = models.OutcomeTopPoints
	filter.Where = models.ConditionLTE
	p.mu.Lock()
	ep.Bet.UpdateOutcomes([]interface{}{
		map[string]interface{}{"id": "o1", "total_points": float64(999999), "total_users": float64(4242),
			"top_predictors": []interface{}{map[string]interface{}{"points": float64(777)}}},
		map[string]interface{}{"id": "o2", "total_points": float64(1), "total_users": float64(1)},
	})
	p.mu.Unlock()

	var env *ObservationDecision
	for _, o := range sink.all() {
		if o.Payload.DecisionEnvelope != nil {
			env = o.Payload.DecisionEnvelope
		}
	}
	if env == nil {
		t.Fatal("the attempt filed no decision envelope")
	}

	fc := env.Settings.FilterCondition
	if fc == nil {
		t.Fatal("the recorded filter condition vanished")
	}
	if fc.Value != 1 || string(fc.By) != "total_users" || string(fc.Where) != "GT" {
		t.Fatalf("the recorded filter followed a later edit: %+v — the envelope aliased the "+
			"round's live pointer instead of copying the value the decision used", fc)
	}
	if len(env.Outcomes) < 2 {
		t.Fatalf("outcomes = %d, want 2", len(env.Outcomes))
	}
	if env.Outcomes[0].TotalPoints != 300 || env.Outcomes[0].TotalUsers != 3 {
		t.Fatalf("the recorded outcome state followed a later frame: %+v — the envelope kept a "+
			"live model reference instead of copying the values it read", env.Outcomes[0])
	}
	if env.Outcomes[0].TopPoints != 0 {
		t.Fatalf("TopPoints = %d, want 0: the decision ran before any top_predictors arrived, "+
			"and a later frame must not backfill an input the decision never had",
			env.Outcomes[0].TopPoints)
	}
}

// TestCaptureCostsNothingWhenNobodyIsObserving regresses a "pure observer"
// leak: the envelope work ran unconditionally, so a deployment with analytics
// switched off still minted an attempt id, allocated an envelope and a counter
// map, and wrote envelope fields on every gate — some of it inside the pool
// lock that serializes all inbound PubSub handling.
//
// Every other observation call site is short-circuited by p.observing(); this
// state was not. An earlier version of this test asserted only the placement
// result, which cannot detect any of that — it passed while the leak was
// present, so it did not test its own name.
//
// The attempt counter is the precise witness: it is incremented once per
// attempt and ONLY as part of building the observation state, so a pool that
// never observed and still advanced it has done work nobody can read.
func TestCaptureCostsNothingWhenNobodyIsObserving(t *testing.T) {
	placer := &fakePlacer{}
	p := newTestPool(placer)
	s := newTestStreamer(100000)
	ep := addRound(p, s, "off-1")
	ep.Bet.Settings = autoBetSettings(5)

	if p.observing() {
		t.Fatal("a pool with no sink reports that it is observing")
	}
	p.placeAutoBet("off-1")

	if got := p.autoAttempts.Load(); got != 0 {
		t.Fatalf("the attempt counter advanced to %d with no sink wired: the attempt id, the "+
			"envelope and its counter map were built for a reader that does not exist, so "+
			"observation is not free when nobody observes", got)
	}
	if placer.callCount() != 1 {
		t.Fatalf("Twitch calls = %d, want 1: the decision must be identical with capture off",
			placer.callCount())
	}
	if ep.Bet.Decision.Amount != 5000 {
		t.Fatalf("decision amount = %d, want 5000 with capture off", ep.Bet.Decision.Amount)
	}

	// And with a sink wired the same round DOES build the state, so the check
	// above is a real gate rather than a counter that never moves.
	p2, _ := observedPool(t, &fakePlacer{})
	s2 := newTestStreamer(100000)
	ep2 := admitRound(p2, s2, "on-1")
	ep2.Bet.Settings = autoBetSettings(5)
	p2.placeAutoBet("on-1")
	if got := p2.autoAttempts.Load(); got != 1 {
		t.Fatalf("attempt counter = %d with a sink wired, want 1", got)
	}
}
