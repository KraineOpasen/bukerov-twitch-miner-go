package predictioneval_test

// Two properties that came out of review rather than out of design, and that
// both concern the model claiming LESS than it might.

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// TestTheRoundsVerdictIsNotAttributedToAnAttemptItCannotBeLinkedTo pins a
// limitation that would otherwise read as a missing feature.
//
// The producer stamps the minted autoAttemptId on the due fact, the terminal
// decision and both placement calls — but NOT on the user_terminal fact that
// carries WON/LOST/REFUNDED and the payout. Those name only the round, and a
// round can carry more than one attempt, so joining on the round would credit
// one attempt with another's payout.
//
// An earlier draft of ProjectSettlementFacts had a branch that read such a
// fact. It was dead — seam 1 never groups a user_terminal fact into an
// attempt — but a dead branch that looks like a feature is worse than an
// absent one: it implies an attribution the data cannot support. This test
// requires the model to state the limitation instead.
func TestTheRoundsVerdictIsNotAttributedToAnAttemptItCannotBeLinkedTo(t *testing.T) {
	// A fully-formed attempt whose round ALSO settled, with the settlement
	// fact sitting right there in the dataset carrying a payout.
	terminal := peRecord(3, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDecided, 7)
	terminal.Payload.ReasonCode = "OK"
	terminal.Payload.DecisionEnvelope = peMinimalEnvelope(7)

	ds := predictioneval.SourceDataset{
		Source: peProvenance(),
		Records: []predictioneval.SourceRecord{
			peRecord(1, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, 7),
			terminal,
			peRecord(4, predictioneval.KindPlacement, predictioneval.PhaseCallStarted, 7),
			peRecord(5, predictioneval.KindPlacement, predictioneval.PhaseCallReturned, 7),
			// The round's verdict. It carries a payout and NO attempt id,
			// exactly as the pinned producer writes it.
			peSettlementRecord(6),
		},
	}
	// Keep the records in ascending causal order.
	ds.Records[1].CollectorSequence = 3

	pk, err := predictioneval.MaterializePairedKnowledge(ds)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(pk.Attempts) != 1 {
		t.Fatalf("materialized %d attempts, want 1 (excluded %+v)", len(pk.Attempts), pk.Excluded)
	}

	facts := predictioneval.ProjectSettlementFacts(pk.Attempts[0])

	if facts.ResolutionLinkage != predictioneval.ResolutionNotLinkableToAttempt {
		t.Errorf("resolution linkage = %q, want %q",
			facts.ResolutionLinkage, predictioneval.ResolutionNotLinkableToAttempt)
	}
	if facts.Resolution != "" || facts.Payout != nil || facts.ReturnedStake != nil {
		t.Errorf("the model attributed a round-level verdict to one attempt: "+
			"resolution=%q payout=%v returned=%v", facts.Resolution, facts.Payout, facts.ReturnedStake)
	}
	// The placement facts, which DO carry the discriminator, are still read.
	if !facts.PlacementCallStarted || !facts.PlacementCallReturned {
		t.Error("the placement calls were lost; those do carry the attempt id and must be read")
	}

	// And a scorecard built on it never claims a settlement it cannot attribute.
	dc, err := predictioneval.ProjectDecisionCase(pk.Attempts[0])
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	sc := predictioneval.Score(dc, predictioneval.Evaluate(dc.Inputs, dc.Observed), facts)
	if sc.Settlement.ROI != predictioneval.SettlementUnknown {
		t.Errorf("ROI = %q, want UNKNOWN", sc.Settlement.ROI)
	}
	if sc.Settlement.Facts.Resolution != "" {
		t.Errorf("the scorecard carried a resolution of %q", sc.Settlement.Facts.Resolution)
	}
}

// TestACausalPositionBelongsToOneCollectorRun regresses a false alarm.
//
// Every collector run numbers its facts from 1, so two runs both hold a
// sequence 1. An earlier draft keyed the duplicate-position check on the
// sequence alone AND ran it before the foreign-session filter, so simply
// reading a dataset that mentioned a second epoch raised
// DUPLICATE_CAUSAL_POSITION — an integrity anomaly manufactured by nothing
// worse than two sessions appearing together.
func TestACausalPositionBelongsToOneCollectorRun(t *testing.T) {
	own := peRecord(1, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, 7)

	// A fact from a DIFFERENT collector run that happens to reuse position 1.
	foreign := peRecord(1, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, 7)
	foreign.CollectorEpoch = peProvenance().CollectorEpoch + 1
	foreign.CollectorSessionID = "some-other-collector-session"
	foreign.ObservationID = "foreign-1"

	terminal := peRecord(2, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDecided, 7)
	terminal.Payload.ReasonCode = "OK"
	terminal.Payload.DecisionEnvelope = peMinimalEnvelope(7)

	// Ordered as the store returns them: ascending (epoch, sequence). The
	// second run's facts therefore follow this run's, and its sequence 1
	// collides with this run's sequence 1.
	pk, err := predictioneval.MaterializePairedKnowledge(predictioneval.SourceDataset{
		Source:  peProvenance(),
		Records: []predictioneval.SourceRecord{own, terminal, foreign},
	})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	for _, a := range pk.Anomalies {
		if a == predictioneval.AnomalyDuplicateCausalPosition {
			t.Fatalf("a fact from another collector run raised %q; a causal position belongs to "+
				"one run, and two runs both numbering from 1 is not a corrupt dataset",
				predictioneval.AnomalyDuplicateCausalPosition)
		}
	}
	// The foreign fact is still excluded by name — it is not silently accepted.
	foundForeign := false
	for _, e := range pk.Excluded {
		if e.Reason == predictioneval.ExclusionForeignSession && e.ObservationID == "foreign-1" {
			foundForeign = true
		}
	}
	if !foundForeign {
		t.Errorf("the foreign fact was not excluded as %q: %+v",
			predictioneval.ExclusionForeignSession, pk.Excluded)
	}
	if len(pk.Attempts) != 1 {
		t.Fatalf("materialized %d attempts, want 1", len(pk.Attempts))
	}

	// A genuine duplicate WITHIN the run is still reported, so the fix did not
	// simply disable the check.
	dup := peRecord(1, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, 7)
	dup.ObservationID = "dup-1"
	pk2, err := predictioneval.MaterializePairedKnowledge(predictioneval.SourceDataset{
		Source:  peProvenance(),
		Records: []predictioneval.SourceRecord{own, dup, terminal},
	})
	if err != nil {
		t.Fatalf("materialize duplicate: %v", err)
	}
	seen := false
	for _, a := range pk2.Anomalies {
		if a == predictioneval.AnomalyDuplicateCausalPosition {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("a real duplicate position within one run was not reported: %v", pk2.Anomalies)
	}
}

// ---- fixtures ---------------------------------------------------------------

func peProvenance() predictioneval.SourceProvenance {
	return predictioneval.SourceProvenance{
		CollectorEpoch:     11,
		CollectorSessionID: "pe-settlement-session",
		ProducerRevision:   predictioneval.SupportedProducerRevision,
		SessionReading:     "AS_FINALIZED",
		CloseState:         "COMPLETE",
		FactsPresent:       5,
		CommittedCount:     5,
		WitnessesVerified:  5,
	}
}

func peRecord(seq int64, kind, phase string, attemptID int64) predictioneval.SourceRecord {
	return predictioneval.SourceRecord{
		ObservationID:      kind + "-" + phase + "-" + itoaTest(int(seq)),
		CollectorSessionID: peProvenance().CollectorSessionID,
		CollectorEpoch:     peProvenance().CollectorEpoch,
		CollectorSequence:  seq,
		PoolInstanceID:     "pool-settlement",
		RoundIncarnationID: "round-1",
		RoundCaptureOrigin: "ACTIVE_AT_ADMISSION",
		EventID:            "event-1",
		Kind:               kind,
		PayloadVersion:     predictioneval.SupportedPayloadVersion,
		ObservationSHA256:  "witness-" + itoaTest(int(seq)),
		Payload: predictioneval.SourcePayload{
			Phase:    phase,
			Counters: map[string]int64{predictioneval.CounterAutoAttemptID: attemptID},
		},
	}
}

// peSettlementRecord is the round's verdict as the pinned producer writes it:
// a payout, and NO attempt discriminator.
func peSettlementRecord(seq int64) predictioneval.SourceRecord {
	r := peRecord(seq, predictioneval.KindUserTerminal, predictioneval.PhaseTerminalAdmitted, 0)
	r.ObservationID = "settlement-" + itoaTest(int(seq))
	r.Payload.ReasonCode = "WON"
	r.Payload.Counters = map[string]int64{
		predictioneval.CounterStake:  100,
		predictioneval.CounterPayout: 250,
	}
	return r
}

func peMinimalEnvelope(attemptID uint64) *predictioneval.SourceDecisionEnvelope {
	balance := int64(1000)
	choice := 0
	amount := int64(50)
	skip := false
	compared := 0.0
	pct, reserve := 0, 0
	allowed, limit, final := int64(50), int64(0), int64(50)
	clamp := false
	return &predictioneval.SourceDecisionEnvelope{
		AttemptID:      attemptID,
		SettingsStage:  predictioneval.StageExecuted,
		CalculateStage: predictioneval.StageExecuted,
		SkipStage:      predictioneval.StageExecuted,
		HealthStage:    predictioneval.HealthAllowed,
		StakeStage:     predictioneval.StageExecuted,
		Settings: &predictioneval.SourceBetSettings{
			Strategy: predictioneval.StrategyMostVoted, Percentage: 5,
			PercentageGap: 20, MaxPoints: 50_000, DelayMode: "FROM_END",
		},
		Balance: &balance,
		Outcomes: []predictioneval.SourceModelOutcome{
			{Slot: 0, Present: true, ID: "o1", TotalUsers: 6, TotalPoints: 600, TopPoints: 90,
				PercentageUsers: 60, Odds: 1.66, OddsPercentage: 60.24},
			{Slot: 1, Present: true, ID: "o2", TotalUsers: 4, TotalPoints: 400, TopPoints: 80,
				PercentageUsers: 40, Odds: 2.5, OddsPercentage: 40},
		},
		ChoiceIndex: &choice, ChoiceOutcomeID: "o1", ChoiceAmount: &amount,
		SkipResult: &skip, SkipCompared: &compared,
		RiskMaxStakePercent: &pct, RiskReservePoints: &reserve,
		StakeAllowed: &allowed, StakeLimit: &limit,
		ClampApplied: &clamp, FinalAmount: &final,
	}
}
