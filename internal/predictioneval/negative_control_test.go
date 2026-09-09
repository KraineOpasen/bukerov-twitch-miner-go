package predictioneval_test

// NEGATIVE CONTROLS.
//
// An independent test-honesty review found that no test in this repository
// ever made Score emit a DISAGREE. Replacing verdictOf's false arm with a
// constant AGREE survived the entire suite — every existing assertion was of
// the form "IndependentDisagree == 0" or "Verdict == AGREE", i.e. every one of
// them asserted the ABSENCE of the thing that was never produced.
//
// That is the worst shape a test suite can have for this package. The
// deliverable IS a comparison; a Score that structurally could not report a
// mismatch would have shipped green, and a comparator wired to the wrong
// recorded field would have certified the replay against nothing.
//
// So this file drives the disagreeing half on purpose: recorded results that
// deliberately contradict the evaluation, at every basis, and asserts that the
// mismatch is caught, counted in the RIGHT tally, and propagated to the
// settlement. The same review found several refusal paths reachable only
// through inputs no fixture supplied; those are here too.

import (
	"math"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// ncOutcomes is the ordinary two-outcome round the goldens use.
func ncOutcomes() []predictioneval.OutcomeInput {
	return []predictioneval.OutcomeInput{
		go1(0, "o1", 6, 600, 90, 60, 1.66, 60.24),
		go1(1, "o2", 4, 400, 80, 40, 2.5, 40),
	}
}

// ncAgreeingCase is a case whose recorded results match what Evaluate derives:
// MOST_VOTED picks slot 0, 5% of 1000 = 50, no gates, 50 >= 10 so it places.
func ncAgreeingCase() (predictioneval.DecisionCase, predictioneval.Evaluation) {
	in := gi(predictioneval.StrategyMostVoted, 5, 20, 50_000, 1000, 0, 0, ncOutcomes(), nil, false)
	in.CommonInputDigest = "nc-digest"
	ev := predictioneval.Evaluate(in, predictioneval.ObservedRealization{})

	compared := 0.0
	c := predictioneval.DecisionCase{
		// Model provenance is now part of what Score checks: a case and an
		// evaluation from a different build must not be scored together.
		Model:             predictioneval.CurrentModelProvenance(),
		CommonInputDigest: "nc-digest",
		Eligibility:       predictioneval.CaseEligibility{Eligible: true, ExercisesPolicy: true},
		Recorded: predictioneval.RecordedResults{
			ChoiceIndex: 0, ChoiceIndexRecorded: true,
			ChoiceOutcomeID:      "o1",
			ChoiceAmount:         50,
			ChoiceAmountRecorded: true,
			SkipResult:           false, SkipResultRecorded: true,
			SkipCompared: &compared,
			StakeAllowed: 50, StakeAllowedRecorded: true,
			StakeReason: predictioneval.GateNone,
			StakeLimit:  0, StakeLimitRecorded: true,
			ClampApplied: false, ClampAppliedRecorded: true,
			FinalAmount: 50, FinalAmountRecorded: true,
			TerminalReason: "OK",
			// The terminal ACTION, not only its reason. Both are compared
			// unconditionally now: a blank decision beside a matching phase
			// and reason used to raise no disagreement at all, which is how an
			// incomplete record reached an affirmative settlement.
			TerminalPhase:    predictioneval.PhaseAutoDecided,
			TerminalDecision: "PLACE",
			HealthStage:      predictioneval.HealthAllowed,
		},
	}
	return c, ev
}

// ncBoundFacts stamps hand-built settlement facts with the case's identity and
// the one placement shape the pinned producer writes.
//
// Score refuses to make an affirmative assessment from facts that carry
// neither, because a stake and a two-option slot are low-cardinality enough
// that a DIFFERENT attempt on the same round can carry the same pair.
func ncBoundFacts(c predictioneval.DecisionCase, s predictioneval.SettlementFacts) predictioneval.SettlementFacts {
	key := c.Key
	s.Attempt = &key
	s.CommonInputDigest = c.CommonInputDigest
	if s.PlacementCoherence == "" {
		s.PlacementCoherence = predictioneval.PlacementShapeCoherent
	}
	return s
}

// TestScoreActuallyReportsADisagreementWhenTheRecordContradictsTheReplay is the
// negative control the suite was missing.
func TestScoreActuallyReportsADisagreementWhenTheRecordContradictsTheReplay(t *testing.T) {
	base, ev := ncAgreeingCase()

	// Sanity: the unmodified case must AGREE, or the mutations below prove
	// nothing about which field caused the disagreement.
	agreed := predictioneval.Score(base, ev, predictioneval.SettlementFacts{})
	if agreed.IndependentDisagree != 0 {
		t.Fatalf("the control case disagreed on %d comparison(s): %+v",
			agreed.IndependentDisagree, disagreementsOf(agreed))
	}
	if agreed.IndependentAgree == 0 {
		t.Fatal("the control case produced no independent agreement at all")
	}

	wrongCompared := 999.0
	tests := []struct {
		name   string
		field  string
		mutate func(*predictioneval.RecordedResults)
	}{
		{"a different chosen index", "choiceIndex", func(r *predictioneval.RecordedResults) { r.ChoiceIndex = 1 }},
		{"a different chosen outcome id", "choiceOutcomeId", func(r *predictioneval.RecordedResults) { r.ChoiceOutcomeID = "o2" }},
		{"a different proposed stake", "choiceAmount", func(r *predictioneval.RecordedResults) { r.ChoiceAmount = 51 }},
		{"the opposite filter answer", "skipResult", func(r *predictioneval.RecordedResults) { r.SkipResult = true }},
		{"a different compared value", "skipCompared", func(r *predictioneval.RecordedResults) { r.SkipCompared = &wrongCompared }},
		{"a different gate allowance", "stakeAllowed", func(r *predictioneval.RecordedResults) { r.StakeAllowed = 49 }},
		{"a different gate reason", "stakeReason", func(r *predictioneval.RecordedResults) { r.StakeReason = predictioneval.GatePercent }},
		{"a different gate limit", "stakeLimit", func(r *predictioneval.RecordedResults) { r.StakeLimit = 7 }},
		{"the opposite clamp decision", "clampApplied", func(r *predictioneval.RecordedResults) { r.ClampApplied = true }},
		{"a different post-gate stake", "finalAmount", func(r *predictioneval.RecordedResults) { r.FinalAmount = 49 }},
		{"a different terminal reason", "terminalReason", func(r *predictioneval.RecordedResults) { r.TerminalReason = "FILTER_REJECTED" }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			c.Recorded = base.Recorded
			tc.mutate(&c.Recorded)

			sc := predictioneval.Score(c, ev, predictioneval.SettlementFacts{
				PlacementCallStarted: true, PlacementCallReturned: true, PlacementAccepted: true,
			})

			cmp := findComparison(t, sc, tc.field)
			if cmp.Verdict != predictioneval.VerdictDisagree {
				t.Fatalf("%s = %s, want DISAGREE (recorded %q vs computed %q). A comparator that "+
					"cannot report a mismatch certifies the replay against nothing",
					tc.field, cmp.Verdict, cmp.Recorded, cmp.Computed)
			}
			if cmp.Basis != predictioneval.BasisIndependent {
				t.Errorf("%s basis = %s, want INDEPENDENT for a non-stealth case", tc.field, cmp.Basis)
			}
			if sc.IndependentDisagree == 0 {
				t.Errorf("the disagreement was not counted: independentDisagree = 0")
			}
			if sc.ConditionedDisagree != 0 {
				t.Errorf("a non-stealth disagreement was counted as conditioned (%d)", sc.ConditionedDisagree)
			}
			// The settlement guard: a replay that did not reproduce the decision
			// must not claim the original bet's outcome as its own.
			if sc.Settlement.Assessment != predictioneval.SettlementUnknown {
				t.Errorf("settlement = %q despite a disagreement, want UNKNOWN: the recorded "+
					"settlement belongs to a DIFFERENT decision", sc.Settlement.Assessment)
			}
		})
	}
}

// TestTheFloatComparisonIsBitExactAndNotATolerance pins a documented property
// that was previously unfalsifiable.
//
// compareFloat's doc comment says it compares bit-for-bit precisely so that a
// tolerance cannot hide the drift worth finding — and substituting a tolerance
// of 1e6 survived the whole suite, because no test ever reached the comparator
// with a disagreeing pair. skipCompared is the only float the scorecard
// compares and it is the output of the filter arithmetic: the one place where a
// re-implementation would drift by a ULP.
func TestTheFloatComparisonIsBitExactAndNotATolerance(t *testing.T) {
	base, ev := ncAgreeingCase()
	if ev.Filter.Compared != 0 {
		t.Fatalf("fixture compared = %v, want 0", ev.Filter.Compared)
	}

	for _, tc := range []struct {
		name     string
		recorded float64
		want     string
	}{
		{"the smallest representable difference disagrees", math.SmallestNonzeroFloat64, predictioneval.VerdictDisagree},
		{"one ULP above the computed value disagrees", math.Nextafter(0, 1), predictioneval.VerdictDisagree},
		{"an exact match agrees", 0, predictioneval.VerdictAgree},
		{"negative zero agrees with zero", math.Copysign(0, -1), predictioneval.VerdictAgree},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			c.Recorded = base.Recorded
			v := tc.recorded
			c.Recorded.SkipCompared = &v

			cmp := findComparison(t, predictioneval.Score(c, ev, predictioneval.SettlementFacts{}), "skipCompared")
			if cmp.Verdict != tc.want {
				t.Fatalf("skipCompared(%v vs %v) = %s, want %s. A tolerance here would report "+
					"genuine arithmetic drift as agreement", tc.recorded, ev.Filter.Compared,
					cmp.Verdict, tc.want)
			}
		})
	}

	// Two NaNs are the same recorded fact and must not be reported as a
	// disagreement just because NaN != NaN.
	c := base
	c.Recorded = base.Recorded
	nan := math.NaN()
	c.Recorded.SkipCompared = &nan
	nanEv := ev
	nanEv.Filter.Compared = math.NaN()
	cmp := findComparison(t, predictioneval.Score(c, nanEv, predictioneval.SettlementFacts{}), "skipCompared")
	if cmp.Verdict != predictioneval.VerdictAgree {
		t.Errorf("two NaNs compared as %s, want AGREE: they are the same recorded fact", cmp.Verdict)
	}
}

// TestStealthAppliesWhenTheStakeExactlyEqualsTheTopStake pins the boundary that
// decides whether a value is independent evidence or conditioned reconstruction.
//
// The pinned policy's condition is `amount >= TopPoints`. Mutating the model's
// `>=` to `>` survived the whole suite, because every stealth fixture sat
// strictly above the top stake. At the boundary the real policy DOES draw and
// reduce; a model answering NOT_APPLICABLE there would present a stake it never
// reproduced as independently derived, and — since the recorded stake is
// topPoints minus 1..4 — emit a false INDEPENDENT disagreement against a
// decision the miner made correctly.
func TestStealthAppliesWhenTheStakeExactlyEqualsTheTopStake(t *testing.T) {
	const balance, topPoints = 2000, 1000 // 50% of 2000 == 1000 == topPoints

	// First establish, against the REAL policy, that the boundary really does
	// reduce. If internal/models ever changed to a strict >, this fails here
	// and the model's mirror must be revisited rather than quietly diverging.
	realBet := &models.Bet{
		Outcomes: []*models.Outcome{
			mkOutcome("s1", 9, 900, topPoints, 60, 1.66, 60.24),
			mkOutcome("s2", 1, 100, 10, 40, 2.5, 40),
		},
		Settings: models.BetSettings{
			Strategy: models.StrategySmartMoney, Percentage: 50, PercentageGap: 20,
			MaxPoints: 50_000, StealthMode: true,
		},
	}
	decision := realBet.Calculate(balance)
	if decision.Amount >= topPoints {
		t.Fatalf("the real policy did NOT reduce at amount == topPoints (got %d); its stealth "+
			"condition is no longer >=, and this model's mirror is now wrong", decision.Amount)
	}
	reduction := topPoints - decision.Amount
	if reduction < 1 || reduction > 4 {
		t.Fatalf("the real policy reduced by %d at the boundary, outside 1..4", reduction)
	}

	// Now the model, at the same boundary.
	in := gi(predictioneval.StrategySmartMoney, 50, 20, 50_000, balance, 0, 0,
		[]predictioneval.OutcomeInput{
			go1(0, "s1", 9, 900, topPoints, 60, 1.66, 60.24),
			go1(1, "s2", 1, 100, 10, 40, 2.5, 40),
		}, nil, true)

	ev := predictioneval.Evaluate(in, predictioneval.ObservedRealization{
		StealthAmount: int64Ptr(int64(decision.Amount)),
	})
	if !ev.Stealth.Applies {
		t.Fatalf("the model says stealth does not apply at base == topPoints (%d), but the real "+
			"policy reduced to %d. This is the condition that decides whether the stake is "+
			"independent evidence or conditioned reconstruction", topPoints, decision.Amount)
	}
	if ev.Stealth.Outcome != predictioneval.StealthConditionedOnObservedRealization {
		t.Fatalf("stealth outcome = %q at the boundary, want CONDITIONED_ON_OBSERVED_REALIZATION",
			ev.Stealth.Outcome)
	}
	if ev.Stealth.Realized != decision.Amount || ev.Stealth.Reduction != reduction {
		t.Errorf("realized = %d / reduction = %d, real policy produced %d / %d",
			ev.Stealth.Realized, ev.Stealth.Reduction, decision.Amount, reduction)
	}
	if !containsString(ev.Limitations, predictioneval.LimitationStealthConditioned) {
		t.Errorf("the boundary case did not carry the stealth-conditioning limitation: %v",
			ev.Limitations)
	}

	// One below the boundary must still be independent, so the assertion above
	// is about the boundary and not about stealth mode in general.
	below := gi(predictioneval.StrategySmartMoney, 50, 20, 50_000, balance, 0, 0,
		[]predictioneval.OutcomeInput{
			go1(0, "s1", 9, 900, topPoints+1, 60, 1.66, 60.24),
			go1(1, "s2", 1, 100, 10, 40, 2.5, 40),
		}, nil, true)
	belowEv := predictioneval.Evaluate(below, predictioneval.ObservedRealization{})
	if belowEv.Stealth.Applies {
		t.Errorf("stealth applied at base(%d) < topPoints(%d)", topPoints, topPoints+1)
	}
}

// TestARefusalPathIsReachableAndReportsItself covers the "refuse rather than
// truncate" arithmetic the specification advertises for hostile input. All
// three paths were reachable through the public API and exercised by nothing.
func TestARefusalPathIsReachableAndReportsItself(t *testing.T) {
	t.Run("a stake the policy's int cannot represent is INDETERMINATE", func(t *testing.T) {
		// 200% of MaxInt64 exceeds what int can hold, so the conversion the
		// pinned policy performs is not defined for it.
		in := gi(predictioneval.StrategyMostVoted, 200, 20, math.MaxInt, math.MaxInt, 0, 0,
			ncOutcomes(), nil, false)
		ev := predictioneval.Evaluate(in, predictioneval.ObservedRealization{})

		if ev.Action != predictioneval.ActionIndeterminate {
			t.Fatalf("action = %q, want INDETERMINATE", ev.Action)
		}
		if ev.BaseStake.State != predictioneval.StageStateIndeterminate {
			t.Errorf("base stake state = %q, want INDETERMINATE", ev.BaseStake.State)
		}
		if !containsString(ev.Limitations, predictioneval.LimitationUnrepresentableStake) {
			t.Errorf("limitations = %v, missing %q", ev.Limitations,
				predictioneval.LimitationUnrepresentableStake)
		}
		if ev.PolicyAmountKnown {
			t.Error("the model claimed to know a stake it just called unrepresentable")
		}
	})

	t.Run("stake-gate arithmetic that would wrap is INDETERMINATE", func(t *testing.T) {
		// balance * maxStakePercent overflows int, which the pinned policy
		// computes without a guard.
		in := gi(predictioneval.StrategyMostVoted, 5, 20, 50_000, math.MaxInt, 100, 0,
			ncOutcomes(), nil, false)
		ev := predictioneval.Evaluate(in, predictioneval.ObservedRealization{})

		if ev.Action != predictioneval.ActionIndeterminate {
			t.Fatalf("action = %q, want INDETERMINATE (base stake %d, state %q)",
				ev.Action, ev.BaseStake.Amount, ev.BaseStake.State)
		}
		if !containsString(ev.Limitations, predictioneval.LimitationArchDependentOverflow) &&
			!containsString(ev.Limitations, predictioneval.LimitationUnrepresentableStake) {
			t.Errorf("limitations = %v, want an explicit arithmetic limitation", ev.Limitations)
		}
	})

	t.Run("a missing risk input is UNSUPPORTED, never a zero gate", func(t *testing.T) {
		in := gi(predictioneval.StrategyMostVoted, 5, 20, 50_000, 1000, 0, 0, ncOutcomes(), nil, false)
		in.RiskPresent = false

		ev := predictioneval.Evaluate(in, predictioneval.ObservedRealization{})
		if ev.Action != predictioneval.ActionUnsupported {
			t.Fatalf("action = %q, want UNSUPPORTED", ev.Action)
		}
		if ev.StakeGate.State != predictioneval.StageStateUnsupported {
			t.Errorf("stake gate = %q, want UNSUPPORTED — an absent gate configuration must not "+
				"become a gate of zero", ev.StakeGate.State)
		}
		if ev.Clamp.HasFinal {
			t.Error("a post-gate stake was produced without the gate's inputs")
		}
	})
}

// TestARejectedPlacementIsReadAsRejected covers ProjectSettlementFacts itself.
//
// An earlier test asserted the settlement consequence by handing Score a
// hand-built SettlementFacts, which skipped the projection entirely: mutating
// `PlacementAccepted = ReasonCode == "OK"` to a constant true survived.
func TestARejectedPlacementIsReadAsRejected(t *testing.T) {
	for _, tc := range []struct {
		reason     string
		errorClass string
		want       bool
	}{
		{"OK", "NONE", true},
		{"REJECTED", "REJECTED_BY_TWITCH", false},
		{"REJECTED", "NOT_ENOUGH_POINTS", false},
	} {
		t.Run(tc.reason+"/"+tc.errorClass, func(t *testing.T) {
			terminal := peRecord(2, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDecided, 7)
			terminal.Payload.ReasonCode = "OK"
			terminal.Payload.DecisionEnvelope = peMinimalEnvelope(7)

			pk, err := predictioneval.MaterializePairedKnowledge(predictioneval.SourceDataset{
				Source: peProvenance(),
				Records: []predictioneval.SourceRecord{
					peRecord(1, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, 7),
					terminal,
					pePlacement(3, 7, predictioneval.PhaseCallStarted, 50, 0, "OK", "NONE"),
					pePlacement(4, 7, predictioneval.PhaseCallReturned, 50, 0, tc.reason, tc.errorClass),
				},
			})
			if err != nil || len(pk.Attempts) != 1 {
				t.Fatalf("materialize: %v (%d attempts)", err, len(pk.Attempts))
			}

			facts := predictioneval.ProjectSettlementFacts(pk.Attempts[0])
			if !facts.PlacementCallReturned {
				t.Fatal("the placement return was not read at all")
			}
			if facts.PlacementAccepted != tc.want {
				t.Errorf("PlacementAccepted = %v, want %v for reason %q. A Twitch-rejected "+
					"placement read as accepted would let a scorecard claim a settlement for a "+
					"bet that was never taken", facts.PlacementAccepted, tc.want, tc.reason)
			}
			if facts.PlacementErrorClass != tc.errorClass {
				t.Errorf("error class = %q, want %q", facts.PlacementErrorClass, tc.errorClass)
			}
		})
	}
}

// TestTheCommonInputDigestCoversTheWholeEncoding pins the digest against a
// golden.
//
// The only previous sensitivity test moved the row witness, so dropping the
// encoding version, the attempt id or the causal sequence from the hash all
// survived — two of the digest's three stated jobs had no oracle. A golden
// covers every field at once: any change to what is hashed, or to the order it
// is hashed in, moves this constant.
func TestTheCommonInputDigestCoversTheWholeEncoding(t *testing.T) {
	pk, err := predictioneval.MaterializePairedKnowledge(peDataset(peProvenance()))
	if err != nil || len(pk.Attempts) != 1 {
		t.Fatalf("materialize: %v", err)
	}
	got := pk.Attempts[0].CommonInputDigest

	// The golden, pinned. It was previously left empty behind an `if want !=
	// ""` guard, which made this assertion UNREACHABLE while the comment above
	// claimed it covered every field at once — the sensitivity cases below
	// were doing all the work, and a field-ORDER change moves none of them.
	const want = "b80d21dc33fb537d778b8b45571102486b6567c36b8d3acb5381e221cf677d13"
	if got != want {
		t.Fatalf("common-input digest = %q, want %q.\n"+
			"Every field this digest hashes, and the order it hashes them in, is pinned by "+
			"this constant. If the encoding changed deliberately, bump CommonInputDigestVersion "+
			"in the same commit — a digest that changes silently makes two builds' scorecards "+
			"incomparable while both claim the same version.", got, want)
	}
	if len(got) != 64 {
		t.Fatalf("digest %q is not a sha256 hex string", got)
	}

	// Sensitivity, field by field: each of these must move the digest.
	for _, tc := range []struct {
		name   string
		mutate func(*predictioneval.SourceDataset)
	}{
		{"the attempt id", func(ds *predictioneval.SourceDataset) {
			for i := range ds.Records {
				ds.Records[i].Payload.Counters[predictioneval.CounterAutoAttemptID] = 8
				if env := ds.Records[i].Payload.DecisionEnvelope; env != nil {
					env.AttemptID = 8
				}
			}
		}},
		{"a causal sequence", func(ds *predictioneval.SourceDataset) {
			ds.Records[0].CollectorSequence = 99
			ds.Records[1].CollectorSequence = 100
		}},
		{"the collector epoch", func(ds *predictioneval.SourceDataset) {
			ds.Source.CollectorEpoch = 4242
			for i := range ds.Records {
				ds.Records[i].CollectorEpoch = 4242
			}
		}},
		{"the pool instance", func(ds *predictioneval.SourceDataset) {
			for i := range ds.Records {
				ds.Records[i].PoolInstanceID = "another-pool"
			}
		}},
		{"the round incarnation", func(ds *predictioneval.SourceDataset) {
			for i := range ds.Records {
				ds.Records[i].RoundIncarnationID = "another-round"
			}
		}},
		{"the row witness", func(ds *predictioneval.SourceDataset) {
			ds.Records[1].ObservationSHA256 = "a-different-witness"
		}},
		{"the capture gap cause", func(ds *predictioneval.SourceDataset) {
			ds.Records[1].RoundCaptureGapCause = "COLLECTOR_NOT_RUNNING"
		}},
	} {
		t.Run(tc.name+" changes the digest", func(t *testing.T) {
			ds := peDataset(peProvenance())
			tc.mutate(&ds)
			mutated, err := predictioneval.MaterializePairedKnowledge(ds)
			if err != nil || len(mutated.Attempts) != 1 {
				t.Fatalf("materialize mutated: %v (%d attempts, excluded %+v)",
					err, len(mutated.Attempts), mutated.Excluded)
			}
			if mutated.Attempts[0].CommonInputDigest == got {
				t.Errorf("changing %s left the digest unchanged (%s); the digest does not "+
					"witness that field, so a replay could read different inputs under the "+
					"same provenance", tc.name, got)
			}
		})
	}
}

// TestAnExplicitZeroAttemptCounterIsNotAnAttemptID pins a boundary the producer
// cannot emit but a corrupt row can.
//
// The counter starts at 1, so a stored zero means the key was absent — or the
// row is corrupt. Accepting it would mint AttemptID 0 and group unrelated facts
// under one identity.
func TestAnExplicitZeroAttemptCounterIsNotAnAttemptID(t *testing.T) {
	for _, id := range []int64{0, -1} {
		ds := peDataset(peProvenance())
		for i := range ds.Records {
			ds.Records[i].Payload.Counters[predictioneval.CounterAutoAttemptID] = id
		}
		pk, err := predictioneval.MaterializePairedKnowledge(ds)
		if err != nil {
			t.Fatalf("materialize: %v", err)
		}
		if len(pk.Attempts) != 0 {
			t.Fatalf("an attempt counter of %d produced %d attempts, want 0 (key %+v)",
				id, len(pk.Attempts), pk.Attempts[0].Key)
		}
		found := false
		for _, e := range pk.Excluded {
			if e.Reason == predictioneval.ExclusionNoAttemptID {
				found = true
			}
		}
		if !found {
			t.Errorf("an attempt counter of %d was not excluded as %q: %+v",
				id, predictioneval.ExclusionNoAttemptID, pk.Excluded)
		}
	}
}

func disagreementsOf(sc predictioneval.Scorecard) []predictioneval.Comparison {
	var out []predictioneval.Comparison
	for _, c := range sc.Comparisons {
		if c.Verdict == predictioneval.VerdictDisagree {
			out = append(out, c)
		}
	}
	return out
}

// The second Codex code review found six more ways the model could claim more
// than it proved. Each is reproduced here as the case that would otherwise slip
// through.

// TestAnEvaluationFromAnotherBuildIsNotScored closes the gap the digest check
// left open.
//
// A case and an evaluation serialized by an OLDER build carry the same old
// common-input digest as each other, so the digest check passes — while the
// scorecard is stamped with THIS build's provenance and its comparisons are
// read as this model's work.
func TestAnEvaluationFromAnotherBuildIsNotScored(t *testing.T) {
	c, ev := ncAgreeingCase()

	for _, tc := range []struct {
		name   string
		mutate func(*predictioneval.DecisionCase, *predictioneval.Evaluation)
	}{
		{"the case was produced by another model version", func(dc *predictioneval.DecisionCase, _ *predictioneval.Evaluation) {
			dc.Model.ModelVersion = "predictioneval/v0"
		}},
		{"the evaluation was produced by another model version", func(_ *predictioneval.DecisionCase, e *predictioneval.Evaluation) {
			e.Model.ModelVersion = "predictioneval/v0"
		}},
		{"the evaluation replays another policy revision", func(_ *predictioneval.DecisionCase, e *predictioneval.Evaluation) {
			e.Model.PolicyRevision = "policy-0000000000000000000000000000000000000000"
		}},
		{"the case was read under another producer contract", func(dc *predictioneval.DecisionCase, _ *predictioneval.Evaluation) {
			dc.Model.SupportedProducerRevision = "obs-v3|policy-whatever"
		}},
		{"the evaluation came from a different int width", func(_ *predictioneval.DecisionCase, e *predictioneval.Evaluation) {
			e.Model.PlatformIntBits = 32
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dc, e := c, ev
			tc.mutate(&dc, &e)

			sc := predictioneval.Score(dc, e, predictioneval.SettlementFacts{
				PlacementCallStarted: true, PlacementCallReturned: true, PlacementAccepted: true,
			})
			if !containsString(sc.Limitations, predictioneval.LimitationModelProvenanceMismatch) {
				t.Fatalf("scored without the %q limitation: %v",
					predictioneval.LimitationModelProvenanceMismatch, sc.Limitations)
			}
			if sc.IndependentAgree != 0 || sc.ConditionedAgree != 0 {
				t.Errorf("counted %d independent and %d conditioned agreements across builds",
					sc.IndependentAgree, sc.ConditionedAgree)
			}
			if sc.Settlement.Assessment != predictioneval.SettlementUnknown {
				t.Errorf("settlement = %q, want UNKNOWN", sc.Settlement.Assessment)
			}
		})
	}

	// The matching pair still scores, so the guard did not refuse everything.
	good := predictioneval.Score(c, ev, predictioneval.SettlementFacts{})
	if containsString(good.Limitations, predictioneval.LimitationModelProvenanceMismatch) {
		t.Error("a matching case and evaluation were flagged as a provenance mismatch")
	}
	if good.IndependentAgree == 0 {
		t.Error("the matching pair produced no independent agreement")
	}
}

// TestAnIneligibleCaseNeverClaimsAnAffirmativeSettlement closes a
// self-contradiction.
//
// An ineligible case has an input the model could not use. Scoring it anyway
// produced UNAVAILABLE comparisons — which are not disagreements — so an
// accepted placement could carry it all the way to APPLIES_TO_REPLAY. A
// scorecard calling a case unevaluable while affirmatively claiming its
// settlement is contradicting itself in the same document.
func TestAnIneligibleCaseNeverClaimsAnAffirmativeSettlement(t *testing.T) {
	c, ev := ncAgreeingCase()
	c.Eligibility = predictioneval.CaseEligibility{
		Eligible:        false,
		Reasons:         []string{predictioneval.IneligibleInconsistentStageStates},
		ExercisesPolicy: true,
	}
	if ev.Action != predictioneval.ActionWouldAttemptPlacement {
		t.Fatalf("fixture action = %q; the point of this test is an ineligible case whose "+
			"inputs still replay to a placement", ev.Action)
	}

	slot := ev.Choice.Index
	stake := int64(ev.Clamp.FinalAmount)
	sc := predictioneval.Score(c, ev, predictioneval.SettlementFacts{
		PlacementCallStarted: true, PlacementCallReturned: true, PlacementAccepted: true,
		PlacementStake: &stake, PlacementSlot: &slot,
	})

	if sc.Settlement.Assessment != predictioneval.SettlementUnknown {
		t.Fatalf("an ineligible case produced settlement %q, want UNKNOWN",
			sc.Settlement.Assessment)
	}
	if !containsString(sc.Limitations, predictioneval.LimitationCaseExcluded) {
		t.Errorf("the scorecard did not say the case was unevaluable: %v", sc.Limitations)
	}
	if sc.IndependentAgree != 0 {
		t.Errorf("an unevaluable case produced %d independent agreements", sc.IndependentAgree)
	}
}

// TestASettlementNeedsThePlacementThisReplayDerived closes attribution by
// acceptance alone.
//
// SettlementFacts carries no attempt key of its own, so batch code can hand
// over another attempt's facts, and a corrupt post-decision slice can hold an
// accepted call with the wrong stake or slot. Acceptance is not attribution.
func TestASettlementNeedsThePlacementThisReplayDerived(t *testing.T) {
	c, ev := ncAgreeingCase()
	rightSlot := ev.Choice.Index
	rightStake := int64(ev.Clamp.FinalAmount)
	wrongSlot := rightSlot + 1
	wrongStake := rightStake + 1

	for _, tc := range []struct {
		name  string
		facts predictioneval.SettlementFacts
		want  string
	}{
		{"the recorded stake is not the replayed one", predictioneval.SettlementFacts{
			PlacementCallReturned: true, PlacementAccepted: true,
			PlacementStake: &wrongStake, PlacementSlot: &rightSlot,
		}, predictioneval.SettlementUnknown},
		{"the recorded slot is not the replayed one", predictioneval.SettlementFacts{
			PlacementCallReturned: true, PlacementAccepted: true,
			PlacementStake: &rightStake, PlacementSlot: &wrongSlot,
		}, predictioneval.SettlementUnknown},
		{"the facts carry no arguments at all", predictioneval.SettlementFacts{
			PlacementCallReturned: true, PlacementAccepted: true,
		}, predictioneval.SettlementUnknown},
		{"the arguments are the replayed ones", predictioneval.SettlementFacts{
			PlacementCallReturned: true, PlacementAccepted: true,
			PlacementStake: &rightStake, PlacementSlot: &rightSlot,
		}, predictioneval.SettlementAppliesToReplay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := predictioneval.Score(c, ev, ncBoundFacts(c, tc.facts))
			if sc.Settlement.Assessment != tc.want {
				t.Errorf("settlement = %q, want %q. Acceptance alone attributes nothing: the "+
					"recorded call has to be the one this replay derived",
					sc.Settlement.Assessment, tc.want)
			}
		})
	}

	// Matching arguments are not attribution on their own. The same stake and
	// slot belonging to a DIFFERENT attempt — a routine collision on a
	// two-option round — must not produce an affirmative settlement.
	right := predictioneval.SettlementFacts{
		PlacementCallStarted: true, PlacementCallReturned: true, PlacementAccepted: true,
		PlacementStake: &rightStake, PlacementSlot: &rightSlot,
	}
	for _, tc := range []struct {
		name string
		mut  func(predictioneval.SettlementFacts) predictioneval.SettlementFacts
		want string
	}{
		{"facts carrying no identity at all", func(s predictioneval.SettlementFacts) predictioneval.SettlementFacts {
			s.PlacementCoherence = predictioneval.PlacementShapeCoherent
			return s
		}, predictioneval.SettlementUnknown},
		{"facts stamped with another attempt", func(s predictioneval.SettlementFacts) predictioneval.SettlementFacts {
			s = ncBoundFacts(c, s)
			other := *s.Attempt
			other.AttemptID++
			s.Attempt = &other
			return s
		}, predictioneval.SettlementUnknown},
		{"facts stamped with another slice's digest", func(s predictioneval.SettlementFacts) predictioneval.SettlementFacts {
			s = ncBoundFacts(c, s)
			s.CommonInputDigest = "some-other-slice"
			return s
		}, predictioneval.SettlementUnknown},
		{"an incoherent placement shape", func(s predictioneval.SettlementFacts) predictioneval.SettlementFacts {
			s = ncBoundFacts(c, s)
			s.PlacementCoherence = predictioneval.PlacementShapeIncoherent
			return s
		}, predictioneval.SettlementUnknown},
		{"this attempt's own coherent facts", func(s predictioneval.SettlementFacts) predictioneval.SettlementFacts {
			return ncBoundFacts(c, s)
		}, predictioneval.SettlementAppliesToReplay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := predictioneval.Score(c, ev, tc.mut(right))
			if sc.Settlement.Assessment != tc.want {
				t.Errorf("settlement = %q, want %q", sc.Settlement.Assessment, tc.want)
			}
		})
	}
}

// TestARecordedTerminalActionIsComparedNotJustItsReason closes a hole where the
// reason code agreed and the action did not.
func TestARecordedTerminalActionIsComparedNotJustItsReason(t *testing.T) {
	c, ev := ncAgreeingCase()
	if ev.Action != predictioneval.ActionWouldAttemptPlacement {
		t.Fatalf("fixture action = %q, want WOULD_ATTEMPT_PLACEMENT", ev.Action)
	}

	// A record whose reason still says OK while its phase and decision say the
	// attempt was skipped. Before the phase was compared, this counted as
	// agreement.
	c.Recorded.TerminalPhase = predictioneval.PhaseAutoSkipped
	c.Recorded.TerminalDecision = "SKIP"

	sc := predictioneval.Score(c, ev, predictioneval.SettlementFacts{})

	phase := findComparison(t, sc, "terminalPhase")
	if phase.Verdict != predictioneval.VerdictDisagree {
		t.Errorf("terminalPhase = %s (recorded %q, computed %q), want DISAGREE",
			phase.Verdict, phase.Recorded, phase.Computed)
	}
	decision := findComparison(t, sc, "terminalDecision")
	if decision.Verdict != predictioneval.VerdictDisagree {
		t.Errorf("terminalDecision = %s, want DISAGREE", decision.Verdict)
	}
	if sc.IndependentDisagree == 0 {
		t.Error("a record whose action contradicts the replay produced no disagreement")
	}
	if sc.Settlement.Assessment != predictioneval.SettlementUnknown {
		t.Errorf("settlement = %q despite a contradicted action", sc.Settlement.Assessment)
	}
}

// TestAFutureRevisionIsNotMistakenForTheLegacyOne closes a string-arithmetic
// hole in the revision binding.
//
// "obs-v10|…" and "obs-v11|…" begin with "obs-v1". A byte-prefix test therefore
// classified them as the one KNOWN readable exception and let them through the
// binding, to be replayed under obs-v2 invariants.
func TestAFutureRevisionIsNotMistakenForTheLegacyOne(t *testing.T) {
	for _, tc := range []struct {
		revision string
		legacy   bool
	}{
		{"obs-v1|policy-deadbeef", true},
		{"obs-v1", true},
		{"obs-v10|policy-deadbeef", false},
		{"obs-v11|policy-deadbeef", false},
		{"obs-v1x", false},
		{"obs-v2|policy-378d05d6ccc7d2a914730a1e1d023ff754bcf873", false}, // the supported one
	} {
		t.Run(tc.revision, func(t *testing.T) {
			src := peProvenance()
			src.ProducerRevision = tc.revision

			// The legacy leg gets a dataset the legacy producer could
			// actually have written. Handing it the shared obs-v2 fixture
			// would ask what this model does with facts that cannot exist,
			// and would answer with a full replay of an envelope the
			// pre-envelope contract never wrote.
			ds := peDataset(src)
			if tc.legacy {
				ds = peLegacyDataset(src)
			}

			pk, err := predictioneval.MaterializePairedKnowledge(ds)
			if err != nil {
				t.Fatalf("materialize: %v", err)
			}

			supported := tc.revision == predictioneval.SupportedProducerRevision
			switch {
			case supported:
				if len(pk.Attempts) != 1 {
					t.Fatalf("the supported revision yielded %d attempts, want 1", len(pk.Attempts))
				}
				return
			case tc.legacy:
				// Readable: the facts are refused by NAME, not as an unknown
				// contract, and never as an unsupported revision.
				named := false
				for _, e := range pk.Excluded {
					if e.Reason == predictioneval.ExclusionUnsupportedProducerRevision {
						t.Fatalf("the KNOWN pre-envelope contract was refused as unsupported: %+v", e)
					}
					if e.Reason == predictioneval.ExclusionLegacyProducerNoEnvelope {
						named = true
					}
				}
				if !named {
					t.Fatalf("the pre-envelope contract produced no %q exclusion: %+v",
						predictioneval.ExclusionLegacyProducerNoEnvelope, pk.Excluded)
				}
			default:
				found := false
				for _, e := range pk.Excluded {
					if e.Reason == predictioneval.ExclusionUnsupportedProducerRevision {
						found = true
					}
				}
				if !found {
					t.Fatalf("%q was not refused as an unsupported revision: %+v. A revision that "+
						"merely starts with the legacy one may have changed what a field means",
						tc.revision, pk.Excluded)
				}
			}
			if len(pk.Attempts) != 0 {
				t.Fatalf("%q yielded %d attempts, want 0", tc.revision, len(pk.Attempts))
			}
		})
	}
}

// TestTwoTerminalPhasesAreTwoEndingsEvenWhenOneCarriesNoEnvelope closes a
// counting hole.
//
// isTerminalFact requires an envelope, so an ending WITHOUT one followed by an
// ending WITH one counted as a single terminal: the attempt materialized, and
// the first ending stayed silently inside the second one's input prefix — an
// extra claimed ending, read as an input.
func TestTwoTerminalPhasesAreTwoEndingsEvenWhenOneCarriesNoEnvelope(t *testing.T) {
	// An ending with no envelope, then the real one.
	envelopeless := peRecord(2, predictioneval.KindAutoDecision, predictioneval.PhaseAutoSkipped, 7)
	envelopeless.Payload.ReasonCode = "NOT_ELIGIBLE"

	terminal := peRecord(3, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDecided, 7)
	terminal.Payload.ReasonCode = "OK"
	terminal.Payload.DecisionEnvelope = peMinimalEnvelope(7)

	pk, err := predictioneval.MaterializePairedKnowledge(predictioneval.SourceDataset{
		Source: peProvenance(),
		Records: []predictioneval.SourceRecord{
			peRecord(1, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, 7),
			envelopeless,
			terminal,
		},
	})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(pk.Attempts) != 0 {
		t.Fatalf("an attempt with two endings materialized (%d); the envelope-less ending would "+
			"have been read as an input to the other one. Slice: %v",
			len(pk.Attempts), pk.Attempts[0].CommonInputSlice)
	}
	found := false
	for _, e := range pk.Excluded {
		if e.Reason == predictioneval.ExclusionMultipleTerminalFacts {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %q exclusion: %+v", predictioneval.ExclusionMultipleTerminalFacts, pk.Excluded)
	}
}

// TestPlacementFactsMustBeOneCoherentPair closes an attribution hole where two
// unrelated placement facts combined into one affirmative settlement.
//
// ProjectSettlementFacts folded facts in as it walked them: the stake and slot
// came from any CALL_STARTED, acceptance came from any CALL_RETURNED, and
// neither the count, the order, nor the arguments recorded on the RETURNED
// fact were examined. So a corrupt slice holding a CALL_STARTED with the
// replay's arguments plus an unrelated accepted CALL_RETURNED passed every
// check and reached APPLIES_TO_REPLAY, although no single observed call
// supported it.
//
// The producer emits exactly one CALL_STARTED immediately before the one
// existing call and exactly one CALL_RETURNED immediately after it, passing
// the SAME stake and outcome slot to both. Anything else is a shape it cannot
// have written.
func TestPlacementFactsMustBeOneCoherentPair(t *testing.T) {
	const attempt = 7
	terminal := peRecord(2, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDecided, attempt)
	terminal.Payload.ReasonCode = "OK"
	terminal.Payload.Decision = "PLACE"
	terminal.Payload.DecisionEnvelope = peMinimalEnvelope(attempt)

	started := func(seq int64, stake int64, slot int) predictioneval.SourceRecord {
		return pePlacement(seq, attempt, predictioneval.PhaseCallStarted, stake, slot, "OK", "NONE")
	}
	returned := func(seq int64, stake int64, slot int) predictioneval.SourceRecord {
		return pePlacement(seq, attempt, predictioneval.PhaseCallReturned, stake, slot, "OK", "NONE")
	}

	for _, tc := range []struct {
		name  string
		post  []predictioneval.SourceRecord
		want  string
		bound bool // the pair is the one the producer writes
	}{
		{"the pair the producer writes", []predictioneval.SourceRecord{
			started(3, 50, 0), returned(4, 50, 0),
		}, predictioneval.PlacementShapeCoherent, true},

		{"no placement at all", nil, predictioneval.PlacementShapeAbsent, false},

		{"two calls started", []predictioneval.SourceRecord{
			started(3, 50, 0), started(4, 90, 1), returned(5, 90, 1),
		}, predictioneval.PlacementShapeIncoherent, false},

		{"two calls returned", []predictioneval.SourceRecord{
			started(3, 50, 0), returned(4, 50, 0), returned(5, 50, 0),
		}, predictioneval.PlacementShapeIncoherent, false},

		{"a return with no start", []predictioneval.SourceRecord{
			returned(3, 50, 0),
		}, predictioneval.PlacementShapeIncoherent, false},

		{"a start with no return", []predictioneval.SourceRecord{
			started(3, 50, 0),
		}, predictioneval.PlacementShapeIncoherent, false},

		{"the return precedes the start", []predictioneval.SourceRecord{
			returned(3, 50, 0), started(4, 50, 0),
		}, predictioneval.PlacementShapeIncoherent, false},

		// The one the old fold could not see: both facts present, in order,
		// but describing DIFFERENT calls.
		{"the two facts disagree on the stake", []predictioneval.SourceRecord{
			started(3, 50, 0), returned(4, 90, 0),
		}, predictioneval.PlacementShapeIncoherent, false},

		{"the two facts disagree on the slot", []predictioneval.SourceRecord{
			started(3, 50, 0), returned(4, 50, 1),
		}, predictioneval.PlacementShapeIncoherent, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := []predictioneval.SourceRecord{
				peRecord(1, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, attempt),
				terminal,
			}
			records = append(records, tc.post...)

			pk, err := predictioneval.MaterializePairedKnowledge(predictioneval.SourceDataset{
				Source: peProvenance(), Records: records,
			})
			if err != nil || len(pk.Attempts) != 1 {
				t.Fatalf("materialize: %v (%d attempts)", err, len(pk.Attempts))
			}

			facts := predictioneval.ProjectSettlementFacts(pk.Attempts[0])
			if facts.PlacementCoherence != tc.want {
				t.Fatalf("placement coherence = %q, want %q (facts %+v)",
					facts.PlacementCoherence, tc.want, facts)
			}

			// An incoherent or absent shape must never carry arguments
			// downstream, because arguments are what an affirmative settlement
			// is built from.
			if !tc.bound && (facts.PlacementStake != nil || facts.PlacementSlot != nil ||
				facts.PlacementAccepted) {
				t.Fatalf("a %s shape still carried placement arguments or acceptance: %+v",
					tc.want, facts)
			}

			dc, err := predictioneval.ProjectDecisionCase(pk.Attempts[0])
			if err != nil {
				t.Fatalf("project: %v", err)
			}
			ev := predictioneval.Evaluate(dc.Inputs, dc.Observed)
			sc := predictioneval.Score(dc, ev, facts)

			want := predictioneval.SettlementUnknown
			if tc.bound && ev.Action == predictioneval.ActionWouldAttemptPlacement {
				want = predictioneval.SettlementAppliesToReplay
			}
			if sc.Settlement.Assessment != want {
				t.Fatalf("settlement = %q, want %q for a %s placement shape",
					sc.Settlement.Assessment, want, tc.want)
			}
		})
	}
}

// TestATerminalFactThatNamesNoActionIsRefused closes a hole where a blank
// value agreed with everything.
//
// terminalDecision was compared only when the recorded value was non-empty, so
// a terminal fact carrying a matching phase and reason but NO decision produced
// no comparison at all — and therefore no disagreement. A placement case could
// reach an affirmative settlement on a record that never said what it did.
//
// The producer writes SKIP or PLACE on every terminal auto fact, at both of its
// terminal write sites, so a blank decision is not an absence to tolerate.
func TestATerminalFactThatNamesNoActionIsRefused(t *testing.T) {
	// Leg 1: the projection refuses such a record outright.
	terminal := peRecord(2, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDecided, 7)
	terminal.Payload.ReasonCode = "OK"
	terminal.Payload.Decision = "" // the producer never writes this
	terminal.Payload.DecisionEnvelope = peMinimalEnvelope(7)

	pk, err := predictioneval.MaterializePairedKnowledge(predictioneval.SourceDataset{
		Source: peProvenance(),
		Records: []predictioneval.SourceRecord{
			peRecord(1, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, 7),
			terminal,
		},
	})
	if err != nil || len(pk.Attempts) != 1 {
		t.Fatalf("materialize: %v (%d attempts)", err, len(pk.Attempts))
	}
	dc, err := predictioneval.ProjectDecisionCase(pk.Attempts[0])
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if dc.Eligibility.Eligible {
		t.Fatal("a terminal fact naming no action produced an eligible case")
	}
	if !containsString(dc.Eligibility.Reasons, predictioneval.IneligibleIncompleteTerminalRecord) {
		t.Fatalf("eligibility reasons = %v, want %q",
			dc.Eligibility.Reasons, predictioneval.IneligibleIncompleteTerminalRecord)
	}

	// Leg 2: even handed straight to Score as an eligible case — which is how
	// a caller assembling its own case would reach it — the blank decision
	// DISAGREES rather than being skipped.
	c, ev := ncAgreeingCase()
	if ev.Action != predictioneval.ActionWouldAttemptPlacement {
		t.Fatalf("fixture action = %q, want WOULD_ATTEMPT_PLACEMENT", ev.Action)
	}
	c.Recorded.TerminalDecision = ""

	slot := ev.Choice.Index
	stake := int64(ev.Clamp.FinalAmount)
	sc := predictioneval.Score(c, ev, ncBoundFacts(c, predictioneval.SettlementFacts{
		PlacementCallStarted: true, PlacementCallReturned: true, PlacementAccepted: true,
		PlacementStake: &stake, PlacementSlot: &slot,
	}))

	cmp := findComparison(t, sc, "terminalDecision")
	if cmp.Verdict != predictioneval.VerdictDisagree {
		t.Fatalf("terminalDecision verdict = %q, want DISAGREE. A blank recorded action that "+
			"produces no comparison agrees with every replayed action", cmp.Verdict)
	}
	if sc.Settlement.Assessment != predictioneval.SettlementUnknown {
		t.Fatalf("settlement = %q, want UNKNOWN: the record never said what it did",
			sc.Settlement.Assessment)
	}
}

// TestAnUnavailableComparisonNeverBecomesAnAffirmativeSettlement closes a hole
// where missing evidence read as agreement.
//
// assessSettlement blocked only DISAGREEMENTS, and an UNAVAILABLE comparison is
// not one. So a case whose recorded choice amount was never recorded — or any
// other field a caller left unset — produced no disagreement, and an accepted
// placement carried it all the way to APPLIES_TO_REPLAY. An affirmative
// settlement asserts that the recorded settlement describes the replayed
// decision, and that claim cannot rest on a field nobody could check.
func TestAnUnavailableComparisonNeverBecomesAnAffirmativeSettlement(t *testing.T) {
	c, ev := ncAgreeingCase()
	if ev.Action != predictioneval.ActionWouldAttemptPlacement {
		t.Fatalf("fixture action = %q, want WOULD_ATTEMPT_PLACEMENT", ev.Action)
	}
	slot := ev.Choice.Index
	stake := int64(ev.Clamp.FinalAmount)
	facts := func(c predictioneval.DecisionCase) predictioneval.SettlementFacts {
		return ncBoundFacts(c, predictioneval.SettlementFacts{
			PlacementCallStarted: true, PlacementCallReturned: true, PlacementAccepted: true,
			PlacementStake: &stake, PlacementSlot: &slot,
		})
	}

	// The control: everything recorded, everything agrees, settlement applies.
	base := predictioneval.Score(c, ev, facts(c))
	if base.UnavailablePairs != 0 {
		t.Fatalf("the control case already has %d unavailable comparison(s): %+v",
			base.UnavailablePairs, base.Comparisons)
	}
	if base.Settlement.Assessment != predictioneval.SettlementAppliesToReplay {
		t.Fatalf("the control settlement = %q, want APPLIES_TO_REPLAY. The mutations below "+
			"prove nothing if the control does not reach it", base.Settlement.Assessment)
	}

	for _, tc := range []struct {
		name  string
		field string
		blank func(*predictioneval.RecordedResults)
	}{
		{"the recorded stake the strategy proposed", "choiceAmount",
			func(r *predictioneval.RecordedResults) { r.ChoiceAmountRecorded = false }},
		{"the recorded filter result", "skipResult",
			func(r *predictioneval.RecordedResults) { r.SkipResultRecorded = false }},
		{"the recorded gate allowance", "stakeAllowed",
			func(r *predictioneval.RecordedResults) { r.StakeAllowedRecorded = false }},
		{"the recorded final stake", "finalAmount",
			func(r *predictioneval.RecordedResults) { r.FinalAmountRecorded = false }},
		{"the recorded filter comparison", "skipCompared",
			func(r *predictioneval.RecordedResults) { r.SkipCompared = nil }},
	} {
		t.Run(tc.name+" is missing", func(t *testing.T) {
			mut := c
			mut.Recorded = c.Recorded
			tc.blank(&mut.Recorded)

			sc := predictioneval.Score(mut, ev, facts(mut))

			cmp := findComparison(t, sc, tc.field)
			if cmp.Verdict != predictioneval.VerdictUnavailable {
				t.Fatalf("%s verdict = %q, want UNAVAILABLE; this case is not exercising "+
					"missing evidence at all", tc.field, cmp.Verdict)
			}
			if sc.IndependentDisagree != 0 || sc.ConditionedDisagree != 0 {
				t.Fatalf("blanking %s produced a disagreement, so the disagreement guard "+
					"would have caught it and this test proves nothing about UNAVAILABLE",
					tc.field)
			}
			if sc.Settlement.Assessment != predictioneval.SettlementUnknown {
				t.Fatalf("settlement = %q with %s unavailable, want UNKNOWN. Missing evidence "+
					"is not agreement", sc.Settlement.Assessment, tc.field)
			}
		})
	}
}

// TestAPostDecisionFactFromAnotherAdmissionIsRefused closes the half of the
// round-incarnation check that fed the settlement.
//
// The agreement check walked the input prefix only. A placement fact carrying
// this attempt's counter but a DIFFERENT RoundIncarnationID therefore landed in
// PostDecision, reached ProjectSettlementFacts, and could supply the stake and
// slot for a settlement about another admission of the same round.
func TestAPostDecisionFactFromAnotherAdmissionIsRefused(t *testing.T) {
	const attempt = 7
	terminal := peRecord(2, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDecided, attempt)
	terminal.Payload.ReasonCode = "OK"
	terminal.Payload.Decision = "PLACE"
	terminal.Payload.DecisionEnvelope = peMinimalEnvelope(attempt)

	started := pePlacement(3, attempt, predictioneval.PhaseCallStarted, 50, 0, "OK", "NONE")
	returned := pePlacement(4, attempt, predictioneval.PhaseCallReturned, 50, 0, "OK", "NONE")
	// The call belongs to a different admission of the round.
	returned.RoundIncarnationID = "round-2"

	pk, err := predictioneval.MaterializePairedKnowledge(predictioneval.SourceDataset{
		Source: peProvenance(),
		Records: []predictioneval.SourceRecord{
			peRecord(1, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, attempt),
			terminal, started, returned,
		},
	})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(pk.Attempts) != 0 {
		t.Fatalf("an attempt materialized with a post-decision fact from another admission "+
			"(%d attempts). Its placement facts would have supplied the stake and slot for a "+
			"settlement about a different round admission", len(pk.Attempts))
	}
	found := false
	for _, e := range pk.Excluded {
		if e.Reason == predictioneval.ExclusionInconsistentRoundIncarnation {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %q exclusion: %+v",
			predictioneval.ExclusionInconsistentRoundIncarnation, pk.Excluded)
	}

	// The same shape with a consistent incarnation still materializes, so the
	// check did not simply refuse every attempt carrying placement facts.
	returned.RoundIncarnationID = started.RoundIncarnationID
	ok, err := predictioneval.MaterializePairedKnowledge(predictioneval.SourceDataset{
		Source: peProvenance(),
		Records: []predictioneval.SourceRecord{
			peRecord(1, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, attempt),
			terminal, started, returned,
		},
	})
	if err != nil || len(ok.Attempts) != 1 {
		t.Fatalf("a consistent attempt failed to materialize: %v (%d attempts, excluded %+v)",
			err, len(ok.Attempts), ok.Excluded)
	}
}
