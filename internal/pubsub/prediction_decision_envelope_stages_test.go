package pubsub

// Stage-level regressions for the auto-decision ENVELOPE (P1.5).
//
// Every test below drives a REAL decision through placeAutoBet (or the real
// admission path and its timer) and reads what the pool recorded. None of them
// builds an ObservationDecision by hand: an envelope this file constructed
// would only prove that this file can construct one, which is exactly the
// vacuous assertion the contract exists to rule out.

import (
	"reflect"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// envelopeFacts returns every auto_decision fact carrying a decision envelope,
// in the order the pool produced them.
func envelopeFacts(t *testing.T, sink *recordingSink) []PredictionObservation {
	t.Helper()
	var out []PredictionObservation
	for _, o := range sink.all() {
		if o.Kind == ObsKindAutoDecision && o.Payload.DecisionEnvelope != nil {
			out = append(out, o)
		}
	}
	return out
}

// terminalAutoFact returns the ONE auto fact that ended the attempt: the fact
// carrying its envelope. It fails when there is not exactly one, because an
// attempt whose envelope is on no fact cannot be replayed at all, and an
// envelope repeated across several facts leaves a reader unable to say which
// record actually ended the attempt.
func terminalAutoFact(t *testing.T, sink *recordingSink) PredictionObservation {
	t.Helper()
	got := envelopeFacts(t, sink)
	if len(got) != 1 {
		t.Fatalf("%d auto facts carry a decision envelope, want exactly 1: with none the "+
			"attempt's inputs are unrecoverable, with several no reader can tell which fact "+
			"ended it (facts recorded: %v)", len(got), sink.phases())
	}
	if phase := got[0].Payload.Phase; phase != "AUTO_DECIDED" && phase != "AUTO_SKIPPED" {
		t.Fatalf("the envelope rode a %q fact, want AUTO_DECIDED or AUTO_SKIPPED: an envelope "+
			"on a non-terminal fact describes an attempt that had not finished", phase)
	}
	return got[0]
}

// autoAttemptID reads the attempt discriminator off a fact, reporting whether
// the fact carried one at all.
func autoAttemptID(obs PredictionObservation) (int64, bool) {
	if obs.Payload.Counters == nil {
		return 0, false
	}
	v, ok := obs.Payload.Counters[obsCounterAutoAttemptID]
	return v, ok
}

// autoBetRound is the shared fixture for a single real auto decision: an
// observed pool, an admitted round, a deterministic strategy (NUMBER_1 always
// picks outcome 0) and the given balance.
func autoBetRound(t *testing.T, balance, percentage int) (*WebSocketPool, *recordingSink, *fakePlacer, *models.Streamer, *models.EventPrediction) {
	t.Helper()
	placer := &fakePlacer{}
	p, sink := observedPool(t, placer)
	s := newTestStreamer(balance)
	p.streamers = []*models.Streamer{s}
	ep := admitRound(p, s, "auto-1")
	ep.Bet.Settings = autoBetSettings(percentage)
	return p, sink, placer, s, ep
}

// TestEveryTerminalAutoPathCarriesItsDecisionEnvelope is the regression for the
// P1 contract's central gap: an auto decision recorded WHAT it decided and
// never what it decided FROM, so no exit of the auto path could be re-derived.
// The fix is only worth anything if it reaches EVERY exit — the nine below are
// the complete set of ways placeAutoBet can end.
//
// A naive implementation attaches the envelope to the happy path alone (or to
// the AUTO_DUE fact, which is emitted before any input exists) and passes a
// test that only ever places a bet. Every skip reason is therefore driven here
// through the real business path — an ineligible channel, the operator's
// per-round suppression, an already-placed bet, a closed round, the account
// health gate, the reserve floor, the minimum stake and the strategy filter —
// and each is required to end on a fact that carries an envelope with a real
// attempt id.
func TestEveryTerminalAutoPathCarriesItsDecisionEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(t *testing.T, p *WebSocketPool, s *models.Streamer, ep *models.EventPrediction)
		wantPhase  string
		wantReason string
	}{
		{
			name:       "the bet is placed",
			setup:      func(*testing.T, *WebSocketPool, *models.Streamer, *models.EventPrediction) {},
			wantPhase:  "AUTO_DECIDED",
			wantReason: "OK",
		},
		{
			name: "the channel can no longer earn points",
			setup: func(_ *testing.T, _ *WebSocketPool, s *models.Streamer, _ *models.EventPrediction) {
				s.SetChannelPointsCapability(models.CapabilityDisabled, models.CapReasonConfirmedDisabled)
			},
			wantPhase:  "AUTO_SKIPPED",
			wantReason: "NOT_ELIGIBLE",
		},
		{
			name: "the operator suppressed this round",
			setup: func(t *testing.T, p *WebSocketPool, _ *models.Streamer, _ *models.EventPrediction) {
				if err := p.SetAutoBetSkip("auto-1", true); err != nil {
					t.Fatal(err)
				}
			},
			wantPhase:  "AUTO_SKIPPED",
			wantReason: "ROUND_SUPPRESSED",
		},
		{
			name: "a bet is already placed on this round",
			setup: func(_ *testing.T, _ *WebSocketPool, _ *models.Streamer, ep *models.EventPrediction) {
				ep.BetPlaced = true
			},
			wantPhase:  "AUTO_SKIPPED",
			wantReason: "ALREADY_PLACED",
		},
		{
			name: "the round is no longer active",
			setup: func(_ *testing.T, _ *WebSocketPool, _ *models.Streamer, ep *models.EventPrediction) {
				ep.Status = models.PredictionLocked
			},
			wantPhase:  "AUTO_SKIPPED",
			wantReason: "NOT_ACTIVE",
		},
		{
			name: "the account's betting health gate is closed",
			setup: func(_ *testing.T, p *WebSocketPool, _ *models.Streamer, _ *models.EventPrediction) {
				p.SetBetHealthGate(fakeBetGate{d: blockingHealth})
				p.SetRiskSettings(config.PredictionRiskSettings{HealthGateEnabled: true})
			},
			wantPhase:  "AUTO_SKIPPED",
			wantReason: "HEALTH_GATED",
		},
		{
			name: "the bet would breach the points reserve",
			setup: func(_ *testing.T, p *WebSocketPool, _ *models.Streamer, _ *models.EventPrediction) {
				p.SetRiskSettings(config.PredictionRiskSettings{ReservePoints: 99000})
			},
			wantPhase:  "AUTO_SKIPPED",
			wantReason: "RESERVE_VIOLATION",
		},
		{
			name: "the stake is below Twitch's minimum",
			setup: func(_ *testing.T, _ *WebSocketPool, s *models.Streamer, _ *models.EventPrediction) {
				// 5% of 100 is 5, under the 10-point floor.
				s.SetChannelPoints(100)
			},
			wantPhase:  "AUTO_SKIPPED",
			wantReason: "BELOW_MINIMUM_POINTS",
		},
		{
			name: "the strategy's own filter rejects it",
			setup: func(_ *testing.T, _ *WebSocketPool, _ *models.Streamer, ep *models.EventPrediction) {
				ep.Bet.Settings.FilterCondition = &models.FilterCondition{
					By: models.OutcomeTotalUsers, Where: models.ConditionGT, Value: 1e9,
				}
			},
			wantPhase:  "AUTO_SKIPPED",
			wantReason: "FILTER_REJECTED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sink, _, s, ep := autoBetRound(t, 100000, 5)
			tc.setup(t, p, s, ep)

			p.placeAutoBet("auto-1")

			fact := terminalAutoFact(t, sink)
			if fact.Payload.Phase != tc.wantPhase || fact.Payload.ReasonCode != tc.wantReason {
				t.Fatalf("the attempt ended on %s/%s, want %s/%s: this exit is not the one the "+
					"fixture drives, so the envelope assertion below would be about a different "+
					"decision", fact.Payload.Phase, fact.Payload.ReasonCode, tc.wantPhase, tc.wantReason)
			}
			env := fact.Payload.DecisionEnvelope
			if env.AttemptID == 0 {
				t.Fatalf("the %s envelope carries attempt id 0: with no discriminator its facts "+
					"cannot be linked to the placement calls, and a re-admission of the same "+
					"Twitch event would be indistinguishable from this attempt", tc.wantReason)
			}
			// The id on the envelope is the id the attempt's other facts carry.
			due := factWithPhase(t, sink, "AUTO_DUE")
			id, ok := autoAttemptID(due)
			if !ok || id != int64(env.AttemptID) {
				t.Fatalf("AUTO_DUE reports attempt id %d (present=%v) and the envelope reports %d: "+
					"a reader cannot join the attempt's opening fact to its result",
					id, ok, env.AttemptID)
			}
		})
	}
}

// TestAPreCalculationExitRecordsEveryStageAsUnreached is the regression for the
// defect the stage vocabulary exists to prevent: an attempt that ends before
// Calculate never produces a balance, a choice, a stake or a filter verdict,
// and an envelope that reported those as 0 / false / "" would be
// indistinguishable from a decision that legitimately computed exactly those
// values from a real round.
//
// A naive implementation zero-values the envelope and passes any test that only
// checks the fields are "there". This one asserts the opposite: each stage must
// SAY it was not reached, and every result field must be absent (nil) rather
// than a fabricated zero.
func TestAPreCalculationExitRecordsEveryStageAsUnreached(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, p *WebSocketPool, s *models.Streamer, ep *models.EventPrediction)
		want  string
	}{
		{
			name: "the channel can no longer earn points",
			setup: func(_ *testing.T, _ *WebSocketPool, s *models.Streamer, _ *models.EventPrediction) {
				s.SetChannelPointsCapability(models.CapabilityDisabled, models.CapReasonConfirmedDisabled)
			},
			want: "NOT_ELIGIBLE",
		},
		{
			name: "the operator suppressed this round",
			setup: func(t *testing.T, p *WebSocketPool, _ *models.Streamer, _ *models.EventPrediction) {
				if err := p.SetAutoBetSkip("auto-1", true); err != nil {
					t.Fatal(err)
				}
			},
			want: "ROUND_SUPPRESSED",
		},
		{
			name: "a bet is already placed on this round",
			setup: func(_ *testing.T, _ *WebSocketPool, _ *models.Streamer, ep *models.EventPrediction) {
				ep.BetPlaced = true
			},
			want: "ALREADY_PLACED",
		},
		{
			name: "the round is no longer active",
			setup: func(_ *testing.T, _ *WebSocketPool, _ *models.Streamer, ep *models.EventPrediction) {
				ep.Status = models.PredictionResolved
			},
			want: "NOT_ACTIVE",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sink, placer, s, ep := autoBetRound(t, 100000, 5)
			tc.setup(t, p, s, ep)

			p.placeAutoBet("auto-1")

			fact := terminalAutoFact(t, sink)
			if fact.Payload.ReasonCode != tc.want {
				t.Fatalf("the attempt ended with reason %q, want %q", fact.Payload.ReasonCode, tc.want)
			}
			if placer.callCount() != 0 {
				t.Fatalf("a %s exit reached Twitch %d times, want 0", tc.want, placer.callCount())
			}
			env := fact.Payload.DecisionEnvelope

			for _, stage := range []struct {
				name string
				got  string
				want string
			}{
				{"CalculateStage", env.CalculateStage, ObsStageNotReached},
				{"SettingsStage", env.SettingsStage, ObsStageNotReached},
				{"SkipStage", env.SkipStage, ObsStageNotReached},
				{"StakeStage", env.StakeStage, ObsStageNotReached},
				{"HealthStage", env.HealthStage, ObsHealthNotReached},
			} {
				if stage.got != stage.want {
					t.Errorf("%s = %q on a %s exit, want %q: a stage that never ran and a stage "+
						"that ran and produced nothing are different facts, and only the state "+
						"can tell them apart", stage.name, stage.got, tc.want, stage.want)
				}
			}

			for _, field := range []struct {
				name    string
				present bool
			}{
				{"Balance", env.Balance != nil},
				{"ChoiceIndex", env.ChoiceIndex != nil},
				{"ChoiceAmount", env.ChoiceAmount != nil},
				{"SkipResult", env.SkipResult != nil},
				{"StakeAllowed", env.StakeAllowed != nil},
				{"FinalAmount", env.FinalAmount != nil},
			} {
				if field.present {
					t.Errorf("%s is present on a %s exit, which returned before the stage that "+
						"produces it: a recorded value here claims a computation that never "+
						"happened, and a zero written into it reads exactly like a decision that "+
						"legitimately computed 0", field.name, tc.want)
				}
			}
		})
	}
}

// TestTheFourHealthGateStatesAreDistinguishable is the regression for a
// collapse a boolean would force: the gate block is SKIPPED for two
// structurally different reasons — the operator turned the gate off, and the
// operator turned it on but the process supplied no gate — and when it does run
// it has two verdicts. All four are separate facts about the account's betting
// health, and "the gate did not run" is not "the gate allowed it".
//
// A naive implementation stores one bool (gated / not gated) and passes any
// test that only distinguishes DENIED from everything else. This one requires
// DISABLED and NO_GATE to be different values, so the fail-open path can be
// told apart from the switched-off path in a replay.
func TestTheFourHealthGateStatesAreDistinguishable(t *testing.T) {
	if ObsHealthDisabled == ObsHealthNoGate {
		t.Fatal("DISABLED and NO_GATE are the same value: a replay could not tell an operator " +
			"who switched the health gate off from one who switched it on while the process " +
			"could not supply a gate — a boolean would merge exactly these two")
	}

	for _, tc := range []struct {
		name       string
		gate       BetHealthGate
		enabled    bool
		wantStage  string
		wantReason string
		wantPhase  string
	}{
		{
			name:      "the operator switched the gate off",
			gate:      fakeBetGate{d: blockingHealth},
			enabled:   false,
			wantStage: ObsHealthDisabled,
			wantPhase: "AUTO_DECIDED",
		},
		{
			name:      "the gate is on but no gate was injected",
			gate:      nil,
			enabled:   true,
			wantStage: ObsHealthNoGate,
			wantPhase: "AUTO_DECIDED",
		},
		{
			name:       "the gate ran and allowed the bet",
			gate:       fakeBetGate{d: BetHealthDecision{Allowed: true, Reason: models.GateHealthGQLDegraded}},
			enabled:    true,
			wantStage:  ObsHealthAllowed,
			wantReason: string(models.GateHealthGQLDegraded),
			wantPhase:  "AUTO_DECIDED",
		},
		{
			name:       "the gate ran and denied the bet",
			gate:       fakeBetGate{d: blockingHealth},
			enabled:    true,
			wantStage:  ObsHealthDenied,
			wantReason: string(models.GateHealthGQLFailed),
			wantPhase:  "AUTO_SKIPPED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sink, _, _, _ := autoBetRound(t, 10000, 5)
			if tc.gate != nil {
				p.SetBetHealthGate(tc.gate)
			}
			p.SetRiskSettings(config.PredictionRiskSettings{HealthGateEnabled: tc.enabled})

			p.placeAutoBet("auto-1")

			fact := terminalAutoFact(t, sink)
			if fact.Payload.Phase != tc.wantPhase {
				t.Fatalf("the attempt ended on %s, want %s: the fixture is not driving the "+
					"health path it claims to", fact.Payload.Phase, tc.wantPhase)
			}
			env := fact.Payload.DecisionEnvelope
			if env.HealthStage != tc.wantStage {
				t.Fatalf("HealthStage = %q, want %q: a reader asking why the bot stopped betting "+
					"cannot separate an unhealthy transport from a switched-off gate when the "+
					"two record the same state", env.HealthStage, tc.wantStage)
			}
			if env.HealthReason != tc.wantReason {
				t.Fatalf("HealthReason = %q, want %q: the gate's own verdict is the only record "+
					"of WHY the account was judged unhealthy", env.HealthReason, tc.wantReason)
			}
		})
	}
}

// TestTheFilterRecordsBothOfItsOutcomes is the regression for a one-sided
// record: the previous contract only ever noted the filter when it REJECTED,
// so "the filter ran and let the bet through" and "the filter never ran at all"
// reached a reader as the same absence. Nothing downstream could say whether a
// placed bet had been checked against the operator's condition.
//
// A naive implementation writes SkipResult only on the rejecting branch and
// passes a test that checks rejection alone. This one asserts the passing case
// too: the stage must say EXECUTED, the result must be present and false, and
// the compared value the filter actually looked at must be recorded.
func TestTheFilterRecordsBothOfItsOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		condition    *models.FilterCondition
		wantSkip     bool
		wantCompared float64
		wantPhase    string
	}{
		{
			name: "the filter ran and passed",
			// Outcomes carry 3 and 2 users, so the compared total is 5 > 1.
			condition:    &models.FilterCondition{By: models.OutcomeTotalUsers, Where: models.ConditionGT, Value: 1},
			wantSkip:     false,
			wantCompared: 5,
			wantPhase:    "AUTO_DECIDED",
		},
		{
			name:         "the filter ran and rejected",
			condition:    &models.FilterCondition{By: models.OutcomeTotalUsers, Where: models.ConditionGT, Value: 1e9},
			wantSkip:     true,
			wantCompared: 5,
			wantPhase:    "AUTO_SKIPPED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sink, _, _, ep := autoBetRound(t, 100000, 5)
			ep.Bet.Settings.FilterCondition = tc.condition

			p.placeAutoBet("auto-1")

			fact := terminalAutoFact(t, sink)
			if fact.Payload.Phase != tc.wantPhase {
				t.Fatalf("the attempt ended on %s, want %s", fact.Payload.Phase, tc.wantPhase)
			}
			env := fact.Payload.DecisionEnvelope
			if env.SkipStage != ObsStageExecuted {
				t.Fatalf("SkipStage = %q, want %q: the filter demonstrably ran, and an envelope "+
					"that does not say so leaves a placed bet looking unchecked",
					env.SkipStage, ObsStageExecuted)
			}
			if env.SkipResult == nil {
				t.Fatal("SkipResult is absent although the filter ran: 'the filter passed' and " +
					"'the filter never ran' then reach a reader as the same absence, which is " +
					"the whole reason the result is recorded separately from the stage")
			}
			if *env.SkipResult != tc.wantSkip {
				t.Fatalf("SkipResult = %v, want %v: the recorded verdict contradicts the one the "+
					"business path acted on", *env.SkipResult, tc.wantSkip)
			}
			if env.SkipCompared == nil {
				t.Fatal("SkipCompared is absent: without the value the condition was tested " +
					"against, a rejection cannot be re-derived from the round it rejected")
			}
			if *env.SkipCompared != tc.wantCompared {
				t.Fatalf("SkipCompared = %v, want %v: the envelope reports a comparison the "+
					"filter did not make", *env.SkipCompared, tc.wantCompared)
			}
		})
	}
}

// TestThePercentClampSeparatesTheProposalFromTheFinalStake is the regression
// for the loss the clamp used to cause: the caller OVERWRITES decision.Amount
// when the percent gate binds, so an envelope that read the stake back after
// the gate would record 1000 and lose the 5000 the strategy actually proposed —
// the one quantity a replay of the strategy has to reproduce.
//
// A naive implementation records only the final stake, or derives ClampApplied
// from StakeReason, and passes a weaker test that checks a single amount. This
// one requires four distinct facts from one decision — the proposal, what
// EvaluateStake returned, whether the caller adopted it, and the stake carried
// out of the gate block — and then requires the un-clamped case to report them
// consistently the other way.
func TestThePercentClampSeparatesTheProposalFromTheFinalStake(t *testing.T) {
	t.Run("the percent gate binds", func(t *testing.T) {
		p, sink, placer, _, _ := autoBetRound(t, 10000, 50)
		// 50% of 10000 proposes 5000; a 10% cap allows 1000.
		p.SetRiskSettings(config.PredictionRiskSettings{MaxStakePercent: 10})

		p.placeAutoBet("auto-1")

		fact := terminalAutoFact(t, sink)
		if fact.Payload.Phase != "AUTO_DECIDED" {
			t.Fatalf("the clamped bet ended on %s/%s, want AUTO_DECIDED: a clamp shrinks a stake, "+
				"it never cancels the bet", fact.Payload.Phase, fact.Payload.ReasonCode)
		}
		env := fact.Payload.DecisionEnvelope
		if env.ChoiceAmount == nil || *env.ChoiceAmount != 5000 {
			t.Fatalf("ChoiceAmount = %v, want 5000: reading the stake back AFTER the clamp loses "+
				"what the strategy proposed, and the strategy can then never be replayed",
				derefInt64(env.ChoiceAmount))
		}
		if env.StakeAllowed == nil || *env.StakeAllowed != 1000 {
			t.Fatalf("StakeAllowed = %v, want 1000: this is EvaluateStake's own return, and "+
				"without it the gate's arithmetic cannot be checked", derefInt64(env.StakeAllowed))
		}
		if env.ClampApplied == nil || !*env.ClampApplied {
			t.Fatalf("ClampApplied = %v, want true: the caller's assignment is the fact, and it "+
				"is the only record that the proposal was actually replaced",
				derefBool(env.ClampApplied))
		}
		if env.FinalAmount == nil || *env.FinalAmount != 1000 {
			t.Fatalf("FinalAmount = %v, want 1000: this is the stake the minimum-stake and filter "+
				"exits are judged against, and the one that reaches Twitch",
				derefInt64(env.FinalAmount))
		}
		if env.StakeReason != string(models.GatePercent) {
			t.Fatalf("StakeReason = %q, want %q", env.StakeReason, string(models.GatePercent))
		}
		if placer.lastAmt != 1000 {
			t.Fatalf("Twitch received a stake of %d while the envelope reports a final %d: the "+
				"record and the placement have diverged", placer.lastAmt, *env.FinalAmount)
		}
	})

	t.Run("no percent gate is configured", func(t *testing.T) {
		p, sink, placer, _, _ := autoBetRound(t, 10000, 50)
		p.SetRiskSettings(config.PredictionRiskSettings{MaxStakePercent: 0})

		p.placeAutoBet("auto-1")

		fact := terminalAutoFact(t, sink)
		if fact.Payload.Phase != "AUTO_DECIDED" {
			t.Fatalf("the ungated bet ended on %s/%s, want AUTO_DECIDED",
				fact.Payload.Phase, fact.Payload.ReasonCode)
		}
		env := fact.Payload.DecisionEnvelope
		if env.ClampApplied == nil || *env.ClampApplied {
			t.Fatalf("ClampApplied = %v, want false: nothing replaced the proposal, and a record "+
				"claiming otherwise invents a gate the operator never configured",
				derefBool(env.ClampApplied))
		}
		if env.StakeReason != "" {
			t.Fatalf("StakeReason = %q, want \"\": no gate bound this stake", env.StakeReason)
		}
		if env.ChoiceAmount == nil || env.FinalAmount == nil || *env.FinalAmount != *env.ChoiceAmount {
			t.Fatalf("FinalAmount = %v and ChoiceAmount = %v, want them equal: with no clamp the "+
				"stake carried out of the gate block IS the proposal",
				derefInt64(env.FinalAmount), derefInt64(env.ChoiceAmount))
		}
		if int64(placer.lastAmt) != *env.FinalAmount {
			t.Fatalf("Twitch received %d while the envelope reports a final %d",
				placer.lastAmt, *env.FinalAmount)
		}
	})
}

// TestAGatelessNegativeProposalKeepsTheAllowanceApartFromTheStake is the
// control the whole ClampApplied/StakeAllowed split exists for.
//
// models.EvaluateStake clamps a negative allowance to 0 before returning, but
// the CALLER assigns `allowed` back onto the stake only in the GatePercent
// branch. With no percent gate and a negative proposal the two therefore
// disagree permanently: EvaluateStake returned 0, and the caller went on with a
// NEGATIVE stake. A ClampApplied derived from StakeReason, or a FinalAmount
// copied from StakeAllowed, both get this exactly wrong — and this is the only
// case that catches them, because everywhere else the two happen to agree.
//
// Stealth mode is what produces the negative proposal: the strategy proposes
// 5000, the chosen outcome's top predictor holds 0 points, so the stake becomes
// `0 - reduceAmount` (see models.Calculate) and is always in [-4, -1].
func TestAGatelessNegativeProposalKeepsTheAllowanceApartFromTheStake(t *testing.T) {
	p, sink, placer, _, ep := autoBetRound(t, 10000, 50)
	settings := ep.Bet.Settings
	settings.StealthMode = true
	ep.Bet.Settings = settings
	// The model's TopPoints is the MAX over the round's top predictors, and it
	// is only ever set from a top_predictors list — so the aggregate is pinned
	// here through the same path a real frame would take.
	ep.Bet.UpdateOutcomes([]interface{}{
		map[string]interface{}{
			"total_points": float64(300), "total_users": float64(3),
			"top_predictors": []interface{}{map[string]interface{}{"points": float64(0)}},
		},
		map[string]interface{}{"total_points": float64(200), "total_users": float64(2)},
	})
	// No percent gate and no reserve: EvaluateStake returns GateNone.
	p.SetRiskSettings(config.PredictionRiskSettings{})

	p.placeAutoBet("auto-1")

	fact := terminalAutoFact(t, sink)
	if fact.Payload.ReasonCode != "BELOW_MINIMUM_POINTS" {
		t.Fatalf("the attempt ended with reason %q, want BELOW_MINIMUM_POINTS: the fixture is "+
			"not producing the negative proposal this control depends on", fact.Payload.ReasonCode)
	}
	if placer.callCount() != 0 {
		t.Fatalf("a negative stake reached Twitch %d times, want 0", placer.callCount())
	}
	env := fact.Payload.DecisionEnvelope
	if len(env.Outcomes) == 0 || env.Outcomes[0].TopPoints != 0 {
		t.Fatalf("the chosen outcome's recorded TopPoints = %v, want 0: without it the negative "+
			"proposal below cannot be re-derived from the envelope", env.Outcomes)
	}
	if env.StakeAllowed == nil || *env.StakeAllowed != 0 {
		t.Fatalf("StakeAllowed = %v, want 0: EvaluateStake clamps a negative allowance to 0 "+
			"before returning, and the envelope must report what the function RETURNED",
			derefInt64(env.StakeAllowed))
	}
	if env.FinalAmount == nil || *env.FinalAmount >= 0 {
		t.Fatalf("FinalAmount = %v, want a negative stake: the caller adopts `allowed` only in "+
			"the percent-gate branch, so with GateNone it carries its own negative proposal out "+
			"of the gate block — copying StakeAllowed here would claim a stake of 0 that the "+
			"code never held", derefInt64(env.FinalAmount))
	}
	if env.ClampApplied == nil || *env.ClampApplied {
		t.Fatalf("ClampApplied = %v, want false: no assignment happened. A flag derived from "+
			"StakeAllowed differing from ChoiceAmount would read true here and claim a clamp "+
			"the caller never applied", derefBool(env.ClampApplied))
	}
	if env.StakeReason != "" {
		t.Fatalf("StakeReason = %q, want \"\" (GateNone)", env.StakeReason)
	}
	if env.ChoiceAmount == nil || *env.ChoiceAmount != *env.FinalAmount {
		t.Fatalf("ChoiceAmount = %v and FinalAmount = %v, want them equal: nothing replaced the "+
			"proposal on this path", derefInt64(env.ChoiceAmount), derefInt64(env.FinalAmount))
	}
}

// TestAReserveViolationRecordsNoPostGateAmount is the regression for a stage
// that would be invented rather than observed. The reserve exit returns from
// INSIDE the gate block, before the caller ever computes a post-gate stake, so
// there is no final amount in existence to record. Writing one — the proposal,
// the allowance, or 0 — would claim the attempt reached a stage it never did.
//
// A naive implementation fills FinalAmount unconditionally right after
// EvaluateStake and passes every test that only inspects a clamped or an
// ungated bet, because on those paths a final stake genuinely exists.
func TestAReserveViolationRecordsNoPostGateAmount(t *testing.T) {
	p, sink, placer, _, _ := autoBetRound(t, 10000, 50)
	// 50% of 10000 proposes 5000; placing it would leave 5000 under the 8000
	// reserve, so the bet is skipped rather than shrunk.
	p.SetRiskSettings(config.PredictionRiskSettings{ReservePoints: 8000})

	p.placeAutoBet("auto-1")

	fact := terminalAutoFact(t, sink)
	if fact.Payload.ReasonCode != "RESERVE_VIOLATION" {
		t.Fatalf("the attempt ended with reason %q, want RESERVE_VIOLATION",
			fact.Payload.ReasonCode)
	}
	if placer.callCount() != 0 {
		t.Fatalf("a reserve violation reached Twitch %d times, want 0", placer.callCount())
	}
	env := fact.Payload.DecisionEnvelope
	if env.FinalAmount != nil {
		t.Fatalf("FinalAmount = %d on a reserve violation, want absent: this exit returns from "+
			"inside the gate block, so a recorded final stake describes a stage the attempt "+
			"never reached", *env.FinalAmount)
	}
	if env.ClampApplied == nil || *env.ClampApplied {
		t.Fatalf("ClampApplied = %v, want false: the reserve is a floor, not a cap — it never "+
			"shrinks a stake", derefBool(env.ClampApplied))
	}
	if env.ChoiceAmount == nil || *env.ChoiceAmount != 5000 {
		t.Fatalf("ChoiceAmount = %v, want 5000: the proposal the reserve refused is the only "+
			"record of what the strategy wanted to stake", derefInt64(env.ChoiceAmount))
	}
	if env.StakeReason != string(models.GateReserveViolation) {
		t.Fatalf("StakeReason = %q, want %q: without it a skipped bet cannot be attributed to "+
			"the reserve floor", env.StakeReason, string(models.GateReserveViolation))
	}
	if env.StakeStage != ObsStageExecuted {
		t.Fatalf("StakeStage = %q, want %q: EvaluateStake demonstrably ran, and its inputs are "+
			"part of the refusal", env.StakeStage, ObsStageExecuted)
	}
}

// TestTheAttemptIDLinksEveryFactOfTheAttempt is the regression for a
// correlation gap: an auto attempt emitted a due fact, a decision and two
// placement calls, and the only thing they had in common was the Twitch event
// id — which a round that is cleaned up and admitted again REUSES. Two attempts
// on the same event were therefore indistinguishable, and a placement could not
// be attributed to the decision that produced its arguments.
//
// A naive implementation stamps the id on the decision fact alone and passes
// any test that inspects one fact. This one requires it on all four facts of a
// placed bet, and requires two attempts to receive different ids — which a
// value derived from the event id or a timestamp could not guarantee.
func TestTheAttemptIDLinksEveryFactOfTheAttempt(t *testing.T) {
	p, sink, placer, _, ep := autoBetRound(t, 100000, 5)

	p.placeAutoBet("auto-1")

	if placer.callCount() != 1 {
		t.Fatalf("Twitch calls = %d, want exactly 1", placer.callCount())
	}
	var first int64
	for _, phase := range []string{"AUTO_DUE", "AUTO_DECIDED", "CALL_STARTED", "CALL_RETURNED"} {
		fact := factWithPhase(t, sink, phase)
		id, ok := autoAttemptID(fact)
		if !ok {
			t.Fatalf("%s carries no %s counter: the call that actually went to Twitch cannot be "+
				"attributed to the decision that produced its arguments",
				phase, obsCounterAutoAttemptID)
		}
		if id == 0 {
			t.Fatalf("%s carries attempt id 0, which is the value an unset counter would also "+
				"produce", phase)
		}
		if first == 0 {
			first = id
		} else if id != first {
			t.Fatalf("%s carries attempt id %d while the attempt opened with %d: the attempt's "+
				"facts no longer form one linked record", phase, id, first)
		}
	}

	// A second attempt on the SAME round: the bet is already placed, so it ends
	// early — and it must still be a distinct attempt.
	sink.reset()
	if !ep.BetPlaced {
		t.Fatal("the first attempt did not mark the round as placed; the second attempt would " +
			"then place a second bet rather than being a second attempt on one round")
	}
	p.placeAutoBet("auto-1")

	second, ok := autoAttemptID(factWithPhase(t, sink, "AUTO_DUE"))
	if !ok || second == 0 {
		t.Fatalf("the second attempt opened with id %d (present=%v), want a real discriminator",
			second, ok)
	}
	if second == first {
		t.Fatalf("both attempts on this round report attempt id %d: a re-admission of the same "+
			"Twitch event would then be recorded as a continuation of the first attempt rather "+
			"than a second one", first)
	}
}

// TestTheAdmissionSnapshotIsASeparateFactFromTheDecisionSnapshot is the
// regression for a merge the contract forbids. A round is admitted with one
// settings value and decided with another, minutes later, after a runtime
// settings apply may have replaced it. Recording one snapshot and reusing it
// for both would tell a reader the two agreed instead of letting them
// establish it.
//
// A naive implementation stores the admission snapshot on the pool and hands
// the same pointer to the decision envelope; every equality-based test still
// passes, because on an undisturbed run the two values ARE equal. This test
// therefore asserts identity, not equality: two independently captured facts,
// neither aliasing the other.
func TestTheAdmissionSnapshotIsASeparateFactFromTheDecisionSnapshot(t *testing.T) {
	placer := &fakePlacer{}
	p, sink := observedPool(t, placer)
	s := newTestStreamer(100000)
	p.streamers = []*models.Streamer{s}

	baseBegun, baseSettled := sink.episodes()
	// The real admission path. The default bet delay is 6s from the end, so a
	// 7s window makes the placement due at once (the remaining fraction of a
	// second truncates to a zero sleep) — long enough to be scheduled at all,
	// short enough that the timer finishes inside this test instead of waking
	// into a later one.
	p.handlePredictionChannel(&PubSubMessage{
		Topic: NewTopic(TopicPredictionsChannel, "chan-1"), Type: "event-created",
		ChannelID: "chan-1",
		Data: map[string]interface{}{"event": map[string]interface{}{
			"id": "admit-1", "status": "ACTIVE", "created_at": time.Now().Format(time.RFC3339),
			"prediction_window_seconds": float64(7),
			"outcomes": []interface{}{
				map[string]interface{}{"id": "o1", "color": "BLUE", "total_points": float64(10)},
				map[string]interface{}{"id": "o2", "color": "PINK", "total_points": float64(20)},
			},
		}},
	}, s)

	if begun, _ := sink.episodes(); begun-baseBegun != 1 {
		t.Fatalf("the round registered %d producer episodes, want 1 for its auto-bet timer: the "+
			"admission was not accepted, so there is no decision to compare against",
			begun-baseBegun)
	}
	// Wait for the timer's own episode to settle: that is the only proof its
	// last capture attempt has returned, so nothing is still emitting when the
	// assertions below read the sink.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, settled := sink.episodes(); settled-baseSettled == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, settled := sink.episodes(); settled-baseSettled != 1 {
		t.Fatal("the auto-bet timer never settled its episode; it is still running, and any " +
			"assertion about what it recorded would be a race")
	}

	accepted := factWithPhase(t, sink, "SCHEDULE_ACCEPTED")
	if accepted.Payload.AdmissionSettings == nil {
		t.Fatal("SCHEDULE_ACCEPTED carries no admission settings: the value the round was " +
			"admitted with is then unrecoverable, and a reader cannot establish whether the " +
			"settings changed between admission and decision")
	}
	decided := terminalAutoFact(t, sink)
	if decided.Payload.DecisionEnvelope.Settings == nil {
		t.Fatal("the decision envelope carries no settings snapshot of its own")
	}
	if decided.Payload.AdmissionSettings != nil {
		t.Fatal("the auto decision fact carries admission settings: the admission snapshot " +
			"belongs to the round's admission, and repeating it here would make one fact look " +
			"like corroboration of the other")
	}
	if accepted.Payload.DecisionEnvelope != nil {
		t.Fatal("the admission fact carries a decision envelope: no decision had been taken yet")
	}
	if decided.Payload.DecisionEnvelope.Settings == accepted.Payload.AdmissionSettings {
		t.Fatal("the decision snapshot and the admission snapshot are the SAME value: a " +
			"settings change between admission and decision could then never be observed, " +
			"because the two facts are one fact reported twice")
	}
}

// TestEveryBetSettingsFieldReachesTheEnvelope is the regression for a partial
// projection. A decision cannot be replayed from settings that omit a field —
// and omitting one "because the strategy in force does not read it" makes the
// envelope's completeness depend on the very value being replayed.
//
// A naive implementation copies the fields the default SMART strategy happens
// to consult and passes any test whose fixture leaves the rest at their
// defaults, since a zero-valued copy of a default is indistinguishable from a
// real one. Every field here is therefore distinct and non-default, and the
// field counts are asserted so a newly added setting fails this test instead of
// silently going unrecorded.
func TestEveryBetSettingsFieldReachesTheEnvelope(t *testing.T) {
	if n := reflect.TypeOf(models.BetSettings{}).NumField(); n != 9 {
		t.Fatalf("models.BetSettings now has %d fields, not the 9 this test enumerates: a "+
			"setting has been added and the envelope's completeness is no longer asserted", n)
	}
	if n := reflect.TypeOf(ObservationBetSettings{}).NumField(); n != 9 {
		t.Fatalf("ObservationBetSettings now has %d fields, not 9: the projection and the "+
			"domain type have diverged", n)
	}

	p, sink, _, _, ep := autoBetRound(t, 100000, 5)
	ep.Bet.Settings = models.BetSettings{
		Strategy:      models.StrategyHighOdds,
		Percentage:    37,
		PercentageGap: 11,
		MaxPoints:     4242,
		MinimumPoints: 7,
		StealthMode:   true,
		Delay:         3.5,
		DelayMode:     models.DelayModeFromStart,
		FilterCondition: &models.FilterCondition{
			By: models.OutcomeTotalPoints, Where: models.ConditionGTE, Value: 12.5,
		},
	}

	p.placeAutoBet("auto-1")

	env := terminalAutoFact(t, sink).Payload.DecisionEnvelope
	if env.SettingsStage != ObsStageExecuted {
		t.Fatalf("SettingsStage = %q, want %q: the decision demonstrably read a settings value",
			env.SettingsStage, ObsStageExecuted)
	}
	if env.Settings == nil {
		t.Fatal("the envelope carries no settings at all; the decision cannot be replayed")
	}
	for _, f := range []struct {
		name      string
		got, want interface{}
	}{
		{"Strategy", env.Settings.Strategy, string(models.StrategyHighOdds)},
		{"Percentage", env.Settings.Percentage, 37},
		{"PercentageGap", env.Settings.PercentageGap, 11},
		{"MaxPoints", env.Settings.MaxPoints, 4242},
		{"MinimumPoints", env.Settings.MinimumPoints, 7},
		{"StealthMode", env.Settings.StealthMode, true},
		{"Delay", env.Settings.Delay, 3.5},
		{"DelayMode", env.Settings.DelayMode, string(models.DelayModeFromStart)},
	} {
		if f.got != f.want {
			t.Errorf("Settings.%s = %v, want %v: a replay reading this envelope would compute a "+
				"different decision from the one that was actually taken", f.name, f.got, f.want)
		}
	}
	if env.Settings.FilterCondition == nil {
		t.Fatal("Settings.FilterCondition is absent although the round carried one: a filtered " +
			"round then reads exactly like an unfiltered one, and Skip's verdict cannot be " +
			"re-derived")
	}
	for _, f := range []struct {
		name      string
		got, want interface{}
	}{
		{"By", env.Settings.FilterCondition.By, string(models.OutcomeTotalPoints)},
		{"Where", env.Settings.FilterCondition.Where, string(models.ConditionGTE)},
		{"Value", env.Settings.FilterCondition.Value, 12.5},
	} {
		if f.got != f.want {
			t.Errorf("Settings.FilterCondition.%s = %v, want %v: the recorded condition is not "+
				"the one the round was filtered by", f.name, f.got, f.want)
		}
	}
	// The aliasing property this used to gesture at — that a later settings
	// edit cannot reach back and rewrite a recorded fact — is not testable by
	// re-reading a value nothing has mutated, so it is not asserted here. It is
	// proved by mutating the round's real condition after the decision in
	// TestCapturedInputsDoNotFollowALaterMutation.
}

// derefInt64 / derefBool render an optional envelope field in a failure
// message without the message itself panicking on the absence it is reporting.
func derefInt64(v *int64) interface{} {
	if v == nil {
		return "absent"
	}
	return *v
}

func derefBool(v *bool) interface{} {
	if v == nil {
		return "absent"
	}
	return *v
}
