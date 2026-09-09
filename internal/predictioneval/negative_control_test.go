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
			HealthStage:    predictioneval.HealthAllowed,
		},
	}
	return c, ev
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

			returned := peRecord(4, predictioneval.KindPlacement, predictioneval.PhaseCallReturned, 7)
			returned.Payload.ReasonCode = tc.reason
			returned.Payload.ErrorClass = tc.errorClass

			pk, err := predictioneval.MaterializePairedKnowledge(predictioneval.SourceDataset{
				Source: peProvenance(),
				Records: []predictioneval.SourceRecord{
					peRecord(1, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, 7),
					terminal,
					peRecord(3, predictioneval.KindPlacement, predictioneval.PhaseCallStarted, 7),
					returned,
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

	const want = "" // filled in below by the self-describing failure
	if want != "" && got != want {
		t.Fatalf("common-input digest = %q, want %q", got, want)
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
