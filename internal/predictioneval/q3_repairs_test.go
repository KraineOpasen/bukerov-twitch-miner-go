package predictioneval_test

// Regressions for the defects an independent review found on this change.
//
// Each of these passed review as "the code says it does this"; none of them was
// actually true. They are kept as named tests rather than folded into the
// larger suites because each names a specific promise that was once broken.

import (
	"encoding/json"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// TestAScorecardNamesTheSessionItWasReadFrom regresses the worst defect in the
// original draft.
//
// SourceProvenance was computed correctly by the reader, carried correctly
// through MaterializePairedKnowledge — and then stranded there. Score never
// filled Scorecard.Source, so every stored result shipped an all-zero source
// block. A scorecard from a truncated, zero-witness, facts-dropped session was
// byte-identical in provenance to one from a fully verified session, and a
// consumer reading `model.supportedProducerRevision` beside an empty
// `source.producerRevision` could easily read the model's pin as the data's.
func TestAScorecardNamesTheSessionItWasReadFrom(t *testing.T) {
	src := peProvenance()
	src.SessionReading = "ADMINISTRATIVELY_TRUNCATED"
	src.SessionDetail = "facts were removed after finalization"
	src.WitnessesVerified = 0
	src.WitnessesUnchecked = 2
	src.DroppedCount = 7

	sc := peScoreOneCase(t, src)

	if sc.Source.CollectorSessionID != src.CollectorSessionID {
		t.Errorf("scorecard source session = %q, want %q", sc.Source.CollectorSessionID, src.CollectorSessionID)
	}
	if sc.Source.CollectorEpoch != src.CollectorEpoch {
		t.Errorf("scorecard source epoch = %d, want %d", sc.Source.CollectorEpoch, src.CollectorEpoch)
	}
	if sc.Source.ProducerRevision != src.ProducerRevision {
		t.Errorf("scorecard producer revision = %q, want %q. An empty value here is trivially "+
			"misread as the model's own pin", sc.Source.ProducerRevision, src.ProducerRevision)
	}
	if sc.Source.SessionReading != src.SessionReading {
		t.Errorf("scorecard session reading = %q, want %q", sc.Source.SessionReading, src.SessionReading)
	}
	if sc.Source.WitnessesVerified != 0 || sc.Source.WitnessesUnchecked != 2 {
		t.Errorf("witness counts = %d verified / %d unchecked, want 0/2: a digest nobody "+
			"recomputed witnesses nothing, and the scorecard has to say so",
			sc.Source.WitnessesVerified, sc.Source.WitnessesUnchecked)
	}
	if sc.Source.DroppedCount != 7 {
		t.Errorf("dropped count = %d, want 7", sc.Source.DroppedCount)
	}

	// The session's qualifications must reach the result too, not just the
	// enclosing reading.
	for _, want := range []string{
		predictioneval.AnomalySessionTruncated,
		predictioneval.AnomalyWitnessesUnchecked,
		predictioneval.AnomalyNoWitnessesVerified,
		predictioneval.AnomalySessionLostFacts,
	} {
		if !containsString(sc.Anomalies, want) {
			t.Errorf("scorecard anomalies = %v, missing %q", sc.Anomalies, want)
		}
	}

	// And it has to survive serialization, because the scorecard is the stored
	// artifact. Nothing in the original suite ever marshalled one, which is
	// exactly why the zero block went unnoticed.
	blob, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("a scorecard could not be marshalled: %v", err)
	}
	var decoded struct {
		Source struct {
			ProducerRevision string `json:"producerRevision"`
			SessionReading   string `json:"sessionReading"`
		} `json:"source"`
		Anomalies []string `json:"anomalies"`
	}
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("the serialized scorecard does not decode: %v", err)
	}
	if decoded.Source.ProducerRevision == "" || decoded.Source.SessionReading == "" {
		t.Fatalf("the serialized scorecard carries an empty source block: %s", blob)
	}
	if len(decoded.Anomalies) == 0 {
		t.Fatalf("the serialized scorecard carries no anomalies despite a truncated session: %s", blob)
	}
}

// TestAnUnsupportedProducerRevisionYieldsNoCases regresses a refusal that was
// declared and never performed.
//
// ExclusionUnsupportedProducerRevision existed as a constant with a doc comment
// describing a refusal, and no code path emitted it: a session stamped with an
// unknown revision was replayed in full under obs-v2 invariants. A future
// obs-v3 that changed what a field MEANS would then produce confident verdicts
// against a contract nobody re-read.
func TestAnUnsupportedProducerRevisionYieldsNoCases(t *testing.T) {
	src := peProvenance()
	src.ProducerRevision = "obs-v3|policy-deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	pk, err := predictioneval.MaterializePairedKnowledge(peDataset(src))
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(pk.Attempts) != 0 {
		t.Fatalf("an unknown producer revision yielded %d attempts, want 0: its envelope may not "+
			"mean what this model assumes", len(pk.Attempts))
	}
	found := false
	for _, e := range pk.Excluded {
		if e.Reason == predictioneval.ExclusionUnsupportedProducerRevision {
			found = true
			if e.Detail != src.ProducerRevision {
				t.Errorf("the exclusion does not name the revision it refused: %q", e.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("no %q exclusion was produced: %+v",
			predictioneval.ExclusionUnsupportedProducerRevision, pk.Excluded)
	}

	// The supported revision still works, so the binding did not simply refuse
	// everything.
	ok, err := predictioneval.MaterializePairedKnowledge(peDataset(peProvenance()))
	if err != nil {
		t.Fatalf("materialize supported: %v", err)
	}
	if len(ok.Attempts) != 1 {
		t.Fatalf("the supported revision yielded %d attempts, want 1", len(ok.Attempts))
	}
}

// TestMaterializedInputsDoNotAliasTheCallersDataset regresses a shallow copy.
//
// The common-input slice was copied at the slice level only, so the Counters
// map and the DecisionEnvelope pointer inside each payload stayed aliased to
// the caller's dataset. Because the common-input digest deliberately does not
// hash payload CONTENT — it delegates that to the store's row witness, which is
// checked before materialization — a post-materialize edit could change what a
// replay read while its digest stayed byte-identical. That is precisely the
// property this package sells.
func TestMaterializedInputsDoNotAliasTheCallersDataset(t *testing.T) {
	ds := peDataset(peProvenance())

	pk, err := predictioneval.MaterializePairedKnowledge(ds)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(pk.Attempts) != 1 {
		t.Fatalf("materialized %d attempts, want 1", len(pk.Attempts))
	}
	before, err := predictioneval.ProjectDecisionCase(pk.Attempts[0])
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	digestBefore := pk.Attempts[0].CommonInputDigest

	// Mutate the CALLER's dataset through the reference types.
	for i := range ds.Records {
		if env := ds.Records[i].Payload.DecisionEnvelope; env != nil {
			env.ChoiceOutcomeID = "MUTATED-AFTER-MATERIALIZE"
			if env.Settings != nil {
				env.Settings.Strategy = "MUTATED"
			}
			if len(env.Outcomes) > 0 {
				env.Outcomes[0].ID = "MUTATED"
			}
		}
		if ds.Records[i].Payload.Counters != nil {
			ds.Records[i].Payload.Counters[predictioneval.CounterAutoAttemptID] = 999999
		}
	}

	after, err := predictioneval.ProjectDecisionCase(pk.Attempts[0])
	if err != nil {
		t.Fatalf("re-project: %v", err)
	}

	if after.Recorded.ChoiceOutcomeID != before.Recorded.ChoiceOutcomeID {
		t.Errorf("editing the caller's dataset changed the materialized case: outcome id %q -> %q",
			before.Recorded.ChoiceOutcomeID, after.Recorded.ChoiceOutcomeID)
	}
	if after.Inputs.Settings == nil || before.Inputs.Settings == nil ||
		after.Inputs.Settings.Strategy != before.Inputs.Settings.Strategy {
		t.Errorf("editing the caller's dataset changed the replayed strategy")
	}
	if len(after.Inputs.Outcomes) > 0 && len(before.Inputs.Outcomes) > 0 &&
		after.Inputs.Outcomes[0].ID != before.Inputs.Outcomes[0].ID {
		t.Errorf("editing the caller's dataset changed a model outcome id")
	}
	if pk.Attempts[0].Key.AttemptID == 999999 {
		t.Error("editing the caller's counters changed the attempt identity")
	}
	// The digest was already stable; the point is that it stayed stable while
	// the CONTENT also stayed stable, rather than hiding a change.
	if pk.Attempts[0].CommonInputDigest != digestBefore {
		t.Errorf("the digest moved on its own: %q -> %q", digestBefore, pk.Attempts[0].CommonInputDigest)
	}
}

// TestCappedMeansMaxPointsActuallyBoundTheStake regresses an overstated flag.
//
// Capped was computed as `base == MaxPoints`, while the pinned policy caps on a
// strict `>`. A percentage that landed exactly on the cap was therefore
// reported as capped, and an operator reading that would go and raise a setting
// that was never binding.
func TestCappedMeansMaxPointsActuallyBoundTheStake(t *testing.T) {
	outs := []predictioneval.OutcomeInput{
		go1(0, "o1", 6, 600, 90, 60, 1.66, 60.24),
		go1(1, "o2", 4, 400, 80, 40, 2.5, 40),
	}
	for _, tc := range []struct {
		name       string
		balance    int
		percentage int
		maxPoints  int
		wantAmount int
		wantCapped bool
	}{
		// 5% of 20000 = 1000, exactly MaxPoints. The strict > never fires.
		{"exactly at the cap is not capped", 20_000, 5, 1000, 1000, false},
		// 5% of 20001 = 1000 (truncated), still not above the cap.
		{"truncation landing on the cap is not capped", 20_001, 5, 1000, 1000, false},
		// 10% of 20000 = 2000 > 1000, so the cap binds.
		{"above the cap is capped", 20_000, 10, 1000, 1000, true},
		{"far below the cap is not capped", 1000, 5, 50_000, 50, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := predictioneval.Evaluate(
				gi(predictioneval.StrategyMostVoted, tc.percentage, 20, tc.maxPoints,
					tc.balance, 0, 0, outs, nil, false),
				predictioneval.ObservedRealization{})
			if ev.BaseStake.Amount != tc.wantAmount {
				t.Fatalf("base stake = %d, want %d", ev.BaseStake.Amount, tc.wantAmount)
			}
			if ev.BaseStake.Capped != tc.wantCapped {
				t.Errorf("capped = %v, want %v: the pinned policy caps on a STRICT >, so a stake "+
					"that merely equals MaxPoints was never constrained by it",
					ev.BaseStake.Capped, tc.wantCapped)
			}
		})
	}
}

// ---- shared fixture --------------------------------------------------------

// peDataset is one clean, internally consistent attempt under the given
// session provenance.
func peDataset(src predictioneval.SourceProvenance) predictioneval.SourceDataset {
	terminal := peRecord(2, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDecided, 7)
	terminal.Payload.ReasonCode = "OK"
	terminal.Payload.DecisionEnvelope = peMinimalEnvelope(7)

	ds := predictioneval.SourceDataset{
		Source: src,
		Records: []predictioneval.SourceRecord{
			peRecord(1, predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, 7),
			terminal,
		},
	}
	for i := range ds.Records {
		ds.Records[i].CollectorEpoch = src.CollectorEpoch
		ds.Records[i].CollectorSessionID = src.CollectorSessionID
	}
	return ds
}

func peScoreOneCase(t *testing.T, src predictioneval.SourceProvenance) predictioneval.Scorecard {
	t.Helper()
	pk, err := predictioneval.MaterializePairedKnowledge(peDataset(src))
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(pk.Attempts) != 1 {
		t.Fatalf("materialized %d attempts, want 1 (excluded %+v)", len(pk.Attempts), pk.Excluded)
	}
	dc, err := predictioneval.ProjectDecisionCase(pk.Attempts[0])
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	return predictioneval.Score(dc, predictioneval.Evaluate(dc.Inputs, dc.Observed),
		predictioneval.ProjectSettlementFacts(pk.Attempts[0]))
}
