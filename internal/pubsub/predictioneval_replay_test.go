package pubsub_test

// INTEGRATED PROOF: the replay reproduces what the REAL pool actually did.
//
// Every other P2 test drives the replay over data a test constructed. That
// proves the arithmetic and the seams, and it cannot prove the one thing that
// matters most here: that the replay walks the stages in the SAME ORDER the
// production caller walks them. An ordering error is invisible to a test that
// builds its own envelope, because such a test necessarily encodes the
// ordering it is trying to check.
//
// So this file drives a REAL automatic betting decision through the REAL
// production path — the real pool, the real models.Calculate/Skip, the real
// models.EvaluateStake, the real risk gates, the real health gate and the real
// placement call — captures the envelope the producer emits, and then requires
// the replay to reach the same terminal action, the same stake and the same
// outcome as the Twitch call the pool actually made.
//
// WHY IT RUNS UNDER -race, when the P1.5 end-to-end capture test cannot.
// That test is blocked by the analytics COLLECTOR's hard 5 ms per-fact write
// deadline, which an instrumented SQLite insert cannot meet. This file touches
// no database at all: it captures at the producer's own sink, which is the
// seam the pool writes to synchronously. So it carries no timing budget, adds
// no skip, and executes in full under the repository's `go test -race ./...`.
//
// What this file does NOT prove: the adapter/store/reader legs. Those are
// covered by the reader package's own persisted-dataset tests and by #325's
// existing end-to-end test. Read this as the ORDERING and POLICY-FIDELITY leg.

import (
	"reflect"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
)

// replayCapturingSink collects the facts a real decision emits.
//
// It is the producer's own sink interface, so the pool hands it exactly what it
// hands the miner's adapter in production. It performs no I/O and never blocks,
// which is the sink contract.
// The mutex is not needed by the paths this harness drives — DriveAutoBet is a
// direct synchronous call and AdmitRound schedules no timer — but the sink
// interface is a producer seam, and the production scheduling path emits from a
// timer goroutine. Guarding it costs nothing and keeps the footgun from being
// armed by a later change that routes this through the real event path.
type replayCapturingSink struct {
	mu    sync.Mutex
	facts []pubsub.PredictionObservation
}

func (s *replayCapturingSink) RecordPredictionObservation(o pubsub.PredictionObservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.facts = append(s.facts, o)
}

// snapshot returns a copy of what was captured, taken under the lock.
func (s *replayCapturingSink) snapshot() []pubsub.PredictionObservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]pubsub.PredictionObservation(nil), s.facts...)
}
func (s *replayCapturingSink) BeginPredictionProducerEpisode() func()    { return func() {} }
func (s *replayCapturingSink) PredictionCaptureState(_, _ string) string { return "" }
func (s *replayCapturingSink) NotePredictionProducerShutdownUncertain()  {}

// gateAlways is a fixed health verdict.
type gateAlways struct{ d pubsub.BetHealthDecision }

func (g gateAlways) AutoBetDecision() pubsub.BetHealthDecision { return g.d }

// drivenDecision is one real decision plus what the replay made of it.
type drivenDecision struct {
	harness   *pubsub.TestAutoBetHarness
	dataset   predictioneval.SourceDataset
	knowledge predictioneval.PairedKnowledge
	decision  predictioneval.DecisionCase
	evaluated predictioneval.Evaluation
	scored    predictioneval.Scorecard
}

// driveAndReplay runs one real auto-bet and replays it end to end.
func driveAndReplay(
	t *testing.T,
	balance int,
	risk config.PredictionRiskSettings,
	settings models.BetSettings,
	outcomes []interface{},
	update []interface{},
	gate pubsub.BetHealthGate,
) drivenDecision {
	t.Helper()

	sink := &replayCapturingSink{}
	h := pubsub.NewTestAutoBetHarness("pool-p2-replay", balance)
	if gate != nil {
		h.Pool().SetBetHealthGate(gate)
	}
	h.SetRisk(risk)
	settle := h.Pool().SetPredictionObservationSink(sink)

	h.AdmitRound("p2-1", settings, outcomes)
	if len(update) > 0 {
		h.UpdateOutcomes("p2-1", update)
	}
	h.DriveAutoBet("p2-1")
	settle(nil)

	captured := sink.snapshot()
	if len(captured) == 0 {
		t.Fatal("the real decision emitted no facts at all; the producer seam is broken")
	}

	ds := datasetFromCapturedFacts(t, captured)
	pk, err := predictioneval.MaterializePairedKnowledge(ds)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(pk.Attempts) != 1 {
		t.Fatalf("materialized %d attempts from one real decision, want exactly 1 "+
			"(excluded: %+v)", len(pk.Attempts), pk.Excluded)
	}
	dc, err := predictioneval.ProjectDecisionCase(pk.Attempts[0])
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if !dc.Eligibility.Eligible {
		t.Fatalf("a case produced by the REAL pool was judged ineligible: %v", dc.Eligibility.Reasons)
	}
	ev := predictioneval.Evaluate(dc.Inputs, dc.Observed)
	sc := predictioneval.Score(dc, ev, predictioneval.ProjectSettlementFacts(pk.Attempts[0]))

	return drivenDecision{
		harness: h, dataset: ds, knowledge: pk, decision: dc, evaluated: ev, scored: sc,
	}
}

// datasetFromCapturedFacts turns producer-side facts into the reader-side value
// the replay consumes.
//
// The collector assigns the causal coordinates in production; here they are
// assigned in CAPTURE ORDER, which is the same order the producer reserved them
// in. Nothing else is invented: every payload field is the one the pool emitted.
//
// The store's sanitizer is not run, because it is unexported. Every value these
// fixtures produce already lies inside the store's closed vocabularies, so
// sanitization would be the identity — and the reader package's persisted-data
// tests cover the sanitized path for real.
func datasetFromCapturedFacts(t *testing.T, facts []pubsub.PredictionObservation) predictioneval.SourceDataset {
	t.Helper()
	const (
		epoch     = int64(1)
		sessionID = "p2-replay-session"
	)
	ds := predictioneval.SourceDataset{
		Source: predictioneval.SourceProvenance{
			CollectorEpoch:     epoch,
			CollectorSessionID: sessionID,
			ProducerRevision:   predictioneval.SupportedProducerRevision,
			SessionReading:     "AS_FINALIZED",
			CloseState:         "COMPLETE",
			FactsPresent:       int64(len(facts)),
			CommittedCount:     int64(len(facts)),
			WitnessesVerified:  int64(len(facts)),
		},
	}
	for i, f := range facts {
		ds.Records = append(ds.Records, predictioneval.SourceRecord{
			ObservationID:        sessionID + ":" + itoaPubsubTest(i+1),
			CollectorSessionID:   sessionID,
			CollectorEpoch:       epoch,
			CollectorSequence:    int64(i + 1),
			PoolInstanceID:       f.PoolInstanceID,
			RoundIncarnationID:   f.RoundIncarnationID,
			RoundCaptureOrigin:   f.RoundCaptureOrigin,
			RoundCaptureGapCause: f.RoundCaptureGapCause,
			EventID:              f.EventID,
			Kind:                 f.Kind,
			PayloadVersion:       predictioneval.SupportedPayloadVersion,
			ObservationSHA256:    "test-witness-" + itoaPubsubTest(i+1),
			Payload:              payloadFromProducer(f.Payload),
		})
	}
	return ds
}

func payloadFromProducer(p pubsub.ObservationPayload) predictioneval.SourcePayload {
	out := predictioneval.SourcePayload{
		Phase:      p.Phase,
		RoundState: p.RoundState,
		Decision:   p.Decision,
		ReasonCode: p.ReasonCode,
		ErrorClass: p.ErrorClass,
	}
	if p.Manual != nil {
		v := *p.Manual
		out.Manual = &v
	}
	if p.OutcomeSlot != nil {
		v := *p.OutcomeSlot
		out.OutcomeSlot = &v
	}
	if len(p.Counters) > 0 {
		out.Counters = make(map[string]int64, len(p.Counters))
		for k, v := range p.Counters {
			out.Counters[k] = v
		}
	}
	if e := p.DecisionEnvelope; e != nil {
		env := &predictioneval.SourceDecisionEnvelope{
			AttemptID:       e.AttemptID,
			SettingsStage:   e.SettingsStage,
			CalculateStage:  e.CalculateStage,
			Balance:         e.Balance,
			BetTotalUsers:   e.BetTotalUsers,
			BetTotalPoints:  e.BetTotalPoints,
			ChoiceIndex:     e.ChoiceIndex,
			ChoiceOutcomeID: e.ChoiceOutcomeID,
			ChoiceAmount:    e.ChoiceAmount,
			SkipStage:       e.SkipStage,
			SkipResult:      e.SkipResult,
			SkipCompared:    e.SkipCompared,
			HealthStage:     e.HealthStage,
			HealthReason:    e.HealthReason,

			StakeStage:          e.StakeStage,
			RiskMaxStakePercent: e.RiskMaxStakePercent,
			RiskReservePoints:   e.RiskReservePoints,
			StakeAllowed:        e.StakeAllowed,
			StakeReason:         e.StakeReason,
			StakeLimit:          e.StakeLimit,
			ClampApplied:        e.ClampApplied,
			FinalAmount:         e.FinalAmount,
		}
		// The store drops a NEGATIVE choice index, because negative is the
		// policy's own "chose nothing". Reproducing that here keeps this
		// fixture faithful to what a reader would actually see.
		if env.ChoiceIndex != nil && *env.ChoiceIndex < 0 {
			env.ChoiceIndex = nil
		}
		if e.Settings != nil {
			env.Settings = &predictioneval.SourceBetSettings{
				Strategy:      e.Settings.Strategy,
				Percentage:    e.Settings.Percentage,
				PercentageGap: e.Settings.PercentageGap,
				MaxPoints:     e.Settings.MaxPoints,
				MinimumPoints: e.Settings.MinimumPoints,
				StealthMode:   e.Settings.StealthMode,
				Delay:         e.Settings.Delay,
				DelayMode:     e.Settings.DelayMode,
			}
			if fc := e.Settings.FilterCondition; fc != nil {
				env.Settings.FilterCondition = &predictioneval.SourceFilterCondition{
					By: fc.By, Where: fc.Where, Value: fc.Value,
				}
			}
		}
		for _, o := range e.Outcomes {
			env.Outcomes = append(env.Outcomes, predictioneval.SourceModelOutcome{
				Slot: o.Slot, Present: o.Present, ID: o.ID,
				TotalUsers: o.TotalUsers, TotalPoints: o.TotalPoints, TopPoints: o.TopPoints,
				PercentageUsers: o.PercentageUsers, Odds: o.Odds, OddsPercentage: o.OddsPercentage,
			})
		}
		out.DecisionEnvelope = env
	}
	return out
}

// TestTheProducerEnvelopeAndTheReplayMirrorHaveTheSameShape guards the
// translation above.
//
// The translation is hand-written, so a field the producer gains and it forgets
// would silently drop an input from every replay. Comparing the two structs by
// field NAME turns that into a compile-time-adjacent failure instead.
func TestTheProducerEnvelopeAndTheReplayMirrorHaveTheSameShape(t *testing.T) {
	for _, pair := range []struct {
		name              string
		producer, replica reflect.Type
	}{
		{"envelope", reflect.TypeOf(pubsub.ObservationDecision{}), reflect.TypeOf(predictioneval.SourceDecisionEnvelope{})},
		{"settings", reflect.TypeOf(pubsub.ObservationBetSettings{}), reflect.TypeOf(predictioneval.SourceBetSettings{})},
		{"filter", reflect.TypeOf(pubsub.ObservationFilterCondition{}), reflect.TypeOf(predictioneval.SourceFilterCondition{})},
		{"outcome", reflect.TypeOf(pubsub.ObservationModelOutcome{}), reflect.TypeOf(predictioneval.SourceModelOutcome{})},
	} {
		replicaFields := map[string]bool{}
		for i := 0; i < pair.replica.NumField(); i++ {
			replicaFields[pair.replica.Field(i).Name] = true
		}
		for i := 0; i < pair.producer.NumField(); i++ {
			name := pair.producer.Field(i).Name
			if !replicaFields[name] {
				t.Errorf("%s: the producer records %q but the replay mirror has no such field, "+
					"so every replay would silently drop that input", pair.name, name)
			}
		}
		if pair.producer.NumField() != pair.replica.NumField() {
			t.Errorf("%s: producer has %d fields, replay mirror has %d",
				pair.name, pair.producer.NumField(), pair.replica.NumField())
		}
	}
}

// TestTheReplayReachesTheSameTwitchCallTheRealPoolMade is the headline case.
//
// The pool actually calls its placer with a stake and an outcome id. The replay
// sees only the recorded inputs, and must arrive at the same two values.
func TestTheReplayReachesTheSameTwitchCallTheRealPoolMade(t *testing.T) {
	settings := models.BetSettings{
		Strategy:      models.StrategySmartMoney,
		Percentage:    10,
		PercentageGap: 20,
		MaxPoints:     50_000,
	}
	d := driveAndReplay(t, 10_000,
		config.PredictionRiskSettings{HealthGateEnabled: true},
		settings, twoOutcomes(), topPredictorFrame(), gateAlways{pubsub.BetHealthDecision{Allowed: true}})

	if d.harness.PlacementCalls() != 1 {
		t.Fatalf("the real pool made %d placement calls, want exactly 1", d.harness.PlacementCalls())
	}
	wantID, wantStake := d.harness.LastPlacement()

	if d.evaluated.Action != predictioneval.ActionWouldAttemptPlacement {
		t.Fatalf("replay action = %q, but the pool really did place a bet",
			d.evaluated.Action)
	}
	if d.evaluated.Choice.OutcomeID != wantID {
		t.Errorf("replay chose outcome %q; the real Twitch call named %q",
			d.evaluated.Choice.OutcomeID, wantID)
	}
	if !d.evaluated.Clamp.HasFinal || d.evaluated.Clamp.FinalAmount != wantStake {
		t.Errorf("replay final stake = %d (present=%v); the real Twitch call carried %d",
			d.evaluated.Clamp.FinalAmount, d.evaluated.Clamp.HasFinal, wantStake)
	}
	if d.scored.IndependentDisagree != 0 {
		t.Errorf("the replay disagreed with the record on %d independent comparison(s): %+v",
			d.scored.IndependentDisagree, disagreements(d.scored))
	}
	if d.scored.IndependentAgree == 0 {
		t.Fatal("the scorecard recorded no independent agreement at all, so it proves nothing")
	}

	// The placement facts are POST-decision: they must never have entered the
	// common-input slice.
	for _, r := range d.knowledge.Attempts[0].CommonInputSlice {
		if r.Kind == predictioneval.KindPlacement {
			t.Fatalf("a placement fact (%s) reached the common-input slice; the bet's own "+
				"call would be feeding the decision that made it", r.Payload.Phase)
		}
	}
	if len(d.knowledge.Attempts[0].PostDecision) == 0 {
		t.Fatal("the placement facts vanished instead of landing in PostDecision")
	}
	if !d.scored.Settlement.Facts.PlacementCallReturned {
		t.Error("the settlement facts did not see the placement call return")
	}
}

// TestTheReplayReproducesEveryRealTerminalExit walks the pinned caller's exits
// through the REAL pool and requires the replay to reach each one.
//
// This is the ordering proof. In particular the below-minimum case is also
// filter-rejecting, which only reports BELOW_MINIMUM_POINTS if the filter is
// acted on AFTER the minimum-stake check — exactly as the pinned caller does.
func TestTheReplayReproducesEveryRealTerminalExit(t *testing.T) {
	filterAlwaysRejects := &models.FilterCondition{
		By: models.OutcomeTotalUsers, Where: models.ConditionGT, Value: 1e9,
	}

	tests := []struct {
		name     string
		balance  int
		risk     config.PredictionRiskSettings
		settings models.BetSettings
		gate     pubsub.BetHealthGate
		want     string
		wantCall bool
	}{
		{
			name:    "health gate denies the bet",
			balance: 10_000,
			risk:    config.PredictionRiskSettings{HealthGateEnabled: true},
			settings: models.BetSettings{
				Strategy: models.StrategyMostVoted, Percentage: 10, PercentageGap: 20, MaxPoints: 50_000,
			},
			gate: gateAlways{pubsub.BetHealthDecision{Allowed: false, Reason: models.GateHealthPubSubFailed}},
			want: predictioneval.ActionHealthGated,
		},
		{
			name:    "the reserve floor abandons the bet",
			balance: 10_000,
			risk:    config.PredictionRiskSettings{ReservePoints: 9_999_999, HealthGateEnabled: true},
			settings: models.BetSettings{
				Strategy: models.StrategyMostVoted, Percentage: 10, PercentageGap: 20, MaxPoints: 50_000,
			},
			gate: gateAlways{pubsub.BetHealthDecision{Allowed: true}},
			want: predictioneval.ActionReserveViolation,
		},
		{
			name:    "the stake falls under the pinned minimum, and the filter also rejects",
			balance: 100,
			risk:    config.PredictionRiskSettings{HealthGateEnabled: true},
			settings: models.BetSettings{
				Strategy: models.StrategyMostVoted, Percentage: 1, PercentageGap: 20,
				MaxPoints: 50_000, FilterCondition: filterAlwaysRejects,
			},
			gate: gateAlways{pubsub.BetHealthDecision{Allowed: true}},
			want: predictioneval.ActionBelowMinimum,
		},
		{
			name:    "the filter rejects a stake that clears the minimum",
			balance: 10_000,
			risk:    config.PredictionRiskSettings{HealthGateEnabled: true},
			settings: models.BetSettings{
				Strategy: models.StrategyMostVoted, Percentage: 10, PercentageGap: 20,
				MaxPoints: 50_000, FilterCondition: filterAlwaysRejects,
			},
			gate: gateAlways{pubsub.BetHealthDecision{Allowed: true}},
			want: predictioneval.ActionFilterRejected,
		},
		{
			name:    "the percent gate clamps and the bet still goes out",
			balance: 10_000,
			risk:    config.PredictionRiskSettings{MaxStakePercent: 1, HealthGateEnabled: true},
			settings: models.BetSettings{
				Strategy: models.StrategyMostVoted, Percentage: 50, PercentageGap: 20, MaxPoints: 50_000,
			},
			gate:     gateAlways{pubsub.BetHealthDecision{Allowed: true}},
			want:     predictioneval.ActionWouldAttemptPlacement,
			wantCall: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := driveAndReplay(t, tc.balance, tc.risk, tc.settings,
				twoOutcomes(), topPredictorFrame(), tc.gate)

			if d.evaluated.Action != tc.want {
				t.Fatalf("replay action = %q, want %q (recorded terminal reason %q)",
					d.evaluated.Action, tc.want, d.decision.Recorded.TerminalReason)
			}
			// The producer's own reason code must agree with the replay's, or
			// one of the two is walking the stages in a different order.
			if d.scored.IndependentDisagree != 0 || d.scored.ConditionedDisagree != 0 {
				t.Fatalf("replay disagreed with the record: %+v", disagreements(d.scored))
			}
			calls := d.harness.PlacementCalls()
			if tc.wantCall && calls != 1 {
				t.Errorf("the pool made %d placement calls, want 1", calls)
			}
			if !tc.wantCall && calls != 0 {
				t.Errorf("the pool made %d placement calls, want 0", calls)
			}
			if tc.want == predictioneval.ActionReserveViolation && d.evaluated.Clamp.HasFinal {
				t.Error("a reserve violation produced a post-gate stake; the pinned caller " +
					"returns from inside the gate block and never reaches one")
			}
		})
	}
}

// TestARealStealthDecisionIsReplayedAsConditionedNotProven closes the loop on
// the one stage that cannot be independently reproduced.
//
// The pool really does consume the global RNG here. The replay must reconstruct
// the stake the pool actually sent, and must label the stage as conditioned
// rather than claiming it derived the draw.
func TestARealStealthDecisionIsReplayedAsConditionedNotProven(t *testing.T) {
	settings := models.BetSettings{
		Strategy:      models.StrategySmartMoney,
		Percentage:    50,
		PercentageGap: 20,
		MaxPoints:     50_000,
		StealthMode:   true,
	}
	d := driveAndReplay(t, 10_000,
		config.PredictionRiskSettings{HealthGateEnabled: true},
		settings, twoOutcomes(), topPredictorFrame(),
		gateAlways{pubsub.BetHealthDecision{Allowed: true}})

	if !d.evaluated.Stealth.Applies {
		t.Fatal("this fixture was built so the real policy's stealth branch fires; it did not")
	}
	if d.evaluated.Stealth.Outcome != predictioneval.StealthConditionedOnObservedRealization {
		t.Fatalf("stealth outcome = %q, want CONDITIONED_ON_OBSERVED_REALIZATION",
			d.evaluated.Stealth.Outcome)
	}
	// Require the placement rather than tolerating its absence: guarding the
	// comparison with `if calls == 1` meant a zero- or multi-placement
	// regression would silently skip the one assertion this test exists for
	// while everything around it still passed.
	if calls := d.harness.PlacementCalls(); calls != 1 {
		t.Fatalf("the stealth fixture made %d placement calls, want exactly 1; the stake "+
			"reconstruction below would otherwise not be checked at all", calls)
	}
	if _, stake := d.harness.LastPlacement(); d.evaluated.Clamp.FinalAmount != stake {
		t.Errorf("replay reconstructed stake %d; the real Twitch call carried %d",
			d.evaluated.Clamp.FinalAmount, stake)
	}
	// The reduction must be one the policy could actually have drawn.
	if d.evaluated.Stealth.Reduction < 1 || d.evaluated.Stealth.Reduction > 4 {
		t.Errorf("derived reduction %d is outside the policy's 1..4 range",
			d.evaluated.Stealth.Reduction)
	}
	// And the scorecard must not present this as independent proof.
	if d.scored.CircularComparisons == 0 {
		t.Error("the stake comparison was not marked circular, so the scorecard would present " +
			"a tautology as evidence")
	}
	found := false
	for _, l := range d.scored.Limitations {
		if l == predictioneval.LimitationStealthConditioned {
			found = true
		}
	}
	if !found {
		t.Errorf("the scorecard did not carry the stealth-conditioning limitation: %v",
			d.scored.Limitations)
	}
}

// topPredictorFrame is the update frame that makes the model derive TopPoints.
// Only UpdateOutcomes produces TopPoints, and SMART_MONEY selects on it.
func topPredictorFrame() []interface{} {
	return []interface{}{
		map[string]interface{}{
			"id": "o1", "total_points": float64(300), "total_users": float64(3),
			"top_predictors": []interface{}{
				map[string]interface{}{"points": float64(120)},
				map[string]interface{}{"points": float64(400)},
			},
		},
		map[string]interface{}{
			"id": "o2", "total_points": float64(200), "total_users": float64(2),
			"top_predictors": []interface{}{
				map[string]interface{}{"points": float64(50)},
			},
		},
	}
}

func disagreements(sc predictioneval.Scorecard) []predictioneval.Comparison {
	var out []predictioneval.Comparison
	for _, c := range sc.Comparisons {
		if c.Verdict == predictioneval.VerdictDisagree {
			out = append(out, c)
		}
	}
	return out
}

func itoaPubsubTest(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
