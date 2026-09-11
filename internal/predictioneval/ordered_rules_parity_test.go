package predictioneval_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// TestOrderedRulesEverythingTheProjectionAdmitsTheEvaluatorAdmits is the
// direction the refusal-cost work must never break.
//
// Thirty-five findings on this pull request moved checks between tiers, and
// almost every one of them was safe in the direction it was argued: a refusal
// got cheaper. The direction NOT argued each time is this one — that hoisting a
// check above a ceiling, a budget or a hash cannot make the evaluator refuse a
// stream ProjectOrderedRulesStream legitimately produced. Two of those moves
// already inverted a pinned precedence and were caught by existing cases; this
// asserts the property they were instances of, rather than waiting for the next
// instance to have a case of its own.
//
// It is NOT a claim that the two paths agree on everything — they do not, and
// orderedRulesStreamInvariantsBroken now says so. It is the weaker and more
// useful claim: a PROJECTED stream is never rejected by an INGEST-VALIDATION
// refusal. The mechanism may still decline to attempt, run out of entropy or
// find the pool unusable; those are results, not rejections of the stream.
//
// The space is enumerated rather than randomised, because the repository's
// deterministic-test contract bars a seeded generator whose failures cannot be
// replayed from the source alone.
func TestOrderedRulesEverythingTheProjectionAdmitsTheEvaluatorAdmits(t *testing.T) {
	// The refusals that mean "this stream is not evaluable" — the ones a
	// projected stream must never see. Reason codes about the MECHANISM
	// (entropy, balance, pool, config domain) are deliberately absent: those
	// are answers, and a projected stream may legitimately receive them.
	ingestRefusal := map[string]bool{
		predictioneval.ReasonStreamContractMismatch:   true,
		predictioneval.ReasonEntropySemanticsMismatch: true,
		predictioneval.ReasonStreamShapeOverBound:     true,
		predictioneval.ReasonStreamBytesOverBound:     true,
		predictioneval.ReasonStreamTextOverBound:      true,
		predictioneval.ReasonStreamInvariantViolated:  true,
		predictioneval.ReasonStreamDigestMismatch:     true,
	}

	pad := func(n int) string { return strings.Repeat("p", n) }

	// Dimensions chosen because this round's reorderings touched every one of
	// them: counts, per-string lengths, the presence words, the closed
	// vocabularies, and whether a boundary exists at all.
	counts := []struct{ candidates, outcomes int }{{1, 2}, {3, 2}, {2, 8}}
	textLens := []int{0, 1, 64, predictioneval.MaxOrderedRulesIdentifierBytes}
	balances := []predictioneval.SuppliedInt64{
		orKnownBalance(1000), orMissingBalance(),
		{Presence: predictioneval.SuppliedInvalid, Reason: "arrived unusable"},
	}
	coverages := []predictioneval.OrderedRulesCoverage{
		predictioneval.CoverageCompleteDeclared,
		predictioneval.CoverageGapsPresent,
		predictioneval.CoverageTruncatedPrefix,
	}
	withCutoff := []bool{false, true}

	projected, cutoffSeen, nonKnownSeen, atBoundSeen := 0, 0, 0, 0

	for _, c := range counts {
		for _, n := range textLens {
			for bi, bal := range balances {
				for _, cov := range coverages {
					for _, cut := range withCutoff {
						name := "c" + strconv.Itoa(c.candidates) + "x" + strconv.Itoa(c.outcomes) +
							"/text" + strconv.Itoa(n) + "/bal" + strconv.Itoa(bi) +
							"/" + string(cov) + "/cutoff" + strconv.FormatBool(cut)

						cs := make([]predictioneval.OrderedRulesCandidate, 0, c.candidates)
						for i := 0; i < c.candidates; i++ {
							outs := make([]predictioneval.OrderedRulesOutcome, 0, c.outcomes)
							for j := 0; j < c.outcomes; j++ {
								o := orOutcome("o"+strconv.Itoa(j), int64(j+1))
								o.Points.Provenance = pad(n)
								outs = append(outs, o)
							}
							cand := orCandidate("c"+strconv.Itoa(i), int64(i+1), bal, outs...)
							cand.Provenance = pad(n)
							cand.OutcomesReason = pad(n)
							cs = append(cs, cand)
						}
						var ins []predictioneval.OrderedRulesIntervention
						if cut {
							ins = []predictioneval.OrderedRulesIntervention{
								orIntervention("call-1", int64(c.candidates)+1,
									predictioneval.InterventionAutoCallStarted,
									predictioneval.RelevanceProven, pad(n))}
						}
						src := orSource(cs, ins)
						src.Scope.Coverage = cov

						stream, err := predictioneval.ProjectOrderedRulesStream(src, orAdmission())
						if err != nil {
							// Refused by the projection: this property says
							// nothing about it. Not every combination above is
							// admissible, and that is fine — the non-vacuity
							// counters below prove enough of them are.
							continue
						}
						projected++
						if stream.Cutoff.Established {
							cutoffSeen++
						}
						if bal.Presence != predictioneval.SuppliedKnown {
							nonKnownSeen++
						}
						if n == predictioneval.MaxOrderedRulesIdentifierBytes {
							atBoundSeen++
						}

						got := predictioneval.EvaluateOrderedRules(stream,
							orConfig([]predictioneval.OrderedRule{
								orRule(predictioneval.ComparatorLe, 40, 100, 0, 10)}, 95, 100, 0, 10),
							orDraws(orWordAdmit, orWordAdmit, orWordAdmit, orWordAdmit))

						if got.Status == predictioneval.StatusRefused && ingestRefusal[got.Reason] {
							t.Errorf("%s: ProjectOrderedRulesStream ADMITTED this source, and the "+
								"evaluator then refused the stream it produced with %q. A projected "+
								"stream must never meet an ingest-validation refusal: every tier "+
								"that can produce one is supposed to be a mirror of a rule the "+
								"projection already applied, so this is a rule the two paths "+
								"disagree on.", name, got.Reason)
						}
					}
				}
			}
		}
	}

	// NON-VACUITY. Without these the loop could `continue` past everything and
	// assert nothing, which is the failure mode that has made three cases on
	// this pull request pass for a reason they did not name.
	switch {
	case projected < 50:
		t.Fatalf("only %d sources projected; this space is too thin to be evidence", projected)
	case cutoffSeen == 0:
		t.Fatal("no projected stream established a boundary, so the cutoff tiers were never exercised")
	case nonKnownSeen == 0:
		t.Fatal("no projected stream carried a non-KNOWN balance, so the presence tiers were never exercised")
	case atBoundSeen == 0:
		t.Fatal("no projected stream carried text at the per-string bound, so the length tiers " +
			"were never exercised at their edge")
	}
	t.Logf("%d projected streams evaluated; %d with a boundary, %d with a non-KNOWN balance, "+
		"%d at the per-string bound", projected, cutoffSeen, nonKnownSeen, atBoundSeen)
}
