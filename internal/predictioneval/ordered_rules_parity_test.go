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
	// FREE TEXT only — provenance, the outcomes reason, an intervention detail.
	// The per-string BOUND is delivered by boundField below instead, one field
	// class at a time, because a reviewer pointed out that padding these three
	// proved nothing about the fields that actually matter here.
	textLens := []int{0, 64}
	// WHICH FIELD CLASS CARRIES A BOUND-LENGTH STRING.
	//
	// The first version of this case padded provenance, the outcomes reason and
	// the intervention detail, and counted "at the bound" if any of them
	// reached MaxOrderedRulesIdentifierBytes. That counter was satisfied
	// without a single IDENTIFIER or admission SOURCE REFERENCE ever reaching
	// the bound — and those are the fields whose validation this round moved
	// between tiers: the two identity passes, and checkTextLength in the
	// charging loops. So the guard was green while the property went untested
	// exactly where it was most likely to break. A reviewer read the padding
	// rather than the counter.
	boundFields := []string{
		"none", "candidateIdentity", "outcomeIdentity",
		"scopeNamespace", "sourceReference", "interventionIdentity",
	}
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

	projected, cutoffSeen, nonKnownSeen := 0, 0, 0
	// One counter per field class, so a class that never projects cannot hide
	// behind another that did.
	atBound := map[string]int{}

	// atBoundIn reads the PROJECTED STREAM rather than the case name.
	//
	// Counting on the loop variable alone would have reproduced the very defect
	// this counter was added to fix, one level up: delete the line that builds
	// the bound-length identity and a name-keyed counter stays green, because it
	// only ever knew which case it was in. Asking the stream means a class is
	// counted when, and only when, a string of exactly
	// MaxOrderedRulesIdentifierBytes actually survived projection into that
	// field. Verified by mutation in both directions — see the note below.
	atBoundIn := func(s predictioneval.OrderedRulesStream, class string) bool {
		full := func(v string) bool {
			return len(v) == predictioneval.MaxOrderedRulesIdentifierBytes
		}
		switch class {
		case "candidateIdentity":
			for i := range s.Candidates {
				if full(s.Candidates[i].Identity) {
					return true
				}
			}
		case "outcomeIdentity":
			for i := range s.Candidates {
				for j := range s.Candidates[i].Outcomes {
					if full(s.Candidates[i].Outcomes[j].Identity) {
						return true
					}
				}
			}
		case "scopeNamespace":
			return full(s.Scope.Namespace)
		case "sourceReference":
			for _, ref := range s.Admission.SourceReferences {
				if full(ref) {
					return true
				}
			}
		case "interventionIdentity":
			// Only reachable through the CUTOFF, which is where a bounded
			// intervention identity lands in a projected stream.
			return s.Cutoff.Established && full(s.Cutoff.Identity)
		}
		return false
	}

	// bound builds a string of exactly MaxOrderedRulesIdentifierBytes ending in
	// a distinguishing suffix, so identities that must be UNIQUE still are at
	// the bound. A shared 4 KiB filler would be refused as a duplicate and the
	// case would silently stop testing what it names.
	bound := func(suffix string) string {
		return strings.Repeat("i", predictioneval.MaxOrderedRulesIdentifierBytes-len(suffix)) + suffix
	}

	for _, c := range counts {
		for _, n := range textLens {
			for bi, bal := range balances {
				for _, cov := range coverages {
					for _, cut := range withCutoff {
						for _, bf := range boundFields {
							name := "c" + strconv.Itoa(c.candidates) + "x" + strconv.Itoa(c.outcomes) +
								"/text" + strconv.Itoa(n) + "/bal" + strconv.Itoa(bi) +
								"/" + string(cov) + "/cutoff" + strconv.FormatBool(cut) +
								"/bound:" + bf

							cs := make([]predictioneval.OrderedRulesCandidate, 0, c.candidates)
							for i := 0; i < c.candidates; i++ {
								outs := make([]predictioneval.OrderedRulesOutcome, 0, c.outcomes)
								for j := 0; j < c.outcomes; j++ {
									id := "o" + strconv.Itoa(j)
									if bf == "outcomeIdentity" {
										id = bound(id)
									}
									o := orOutcome(id, int64(j+1))
									o.Points.Provenance = pad(n)
									outs = append(outs, o)
								}
								cid := "c" + strconv.Itoa(i)
								if bf == "candidateIdentity" {
									cid = bound(cid)
								}
								cand := orCandidate(cid, int64(i+1), bal, outs...)
								cand.Provenance = pad(n)
								cand.OutcomesReason = pad(n)
								cs = append(cs, cand)
							}
							var ins []predictioneval.OrderedRulesIntervention
							if cut {
								iid := "call-1"
								if bf == "interventionIdentity" {
									// This one becomes the stream's CUTOFF identity,
									// which is the field the bounded-text tier scans.
									iid = bound(iid)
								}
								ins = []predictioneval.OrderedRulesIntervention{
									orIntervention(iid, int64(c.candidates)+1,
										predictioneval.InterventionAutoCallStarted,
										predictioneval.RelevanceProven, pad(n))}
							}
							src := orSource(cs, ins)
							src.Scope.Coverage = cov
							if bf == "scopeNamespace" {
								src.Scope.Namespace = bound("ns")
							}
							adm := orAdmission()
							if bf == "sourceReference" {
								adm.SourceReferences = []string{bound("ref")}
							}

							stream, err := predictioneval.ProjectOrderedRulesStream(src, adm)
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
							for _, class := range boundFields {
								if class != "none" && atBoundIn(stream, class) {
									atBound[class]++
								}
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
	}
	// EVERY field class must have reached the bound in at least one projected
	// stream. One counter per class rather than one for all of them, because a
	// single counter is what let the first version of this case pass while no
	// identifier and no source reference was ever at its limit.
	//
	// MUTATION RECORD. Each counter was confirmed to discriminate before this
	// case was committed, and the results are worth keeping because two of them
	// are not what a reader would guess.
	//
	//   - Neutering the line that builds the bound-length string, one class at a
	//     time (five mutants), fails HERE and names that class. Counts on the
	//     unmutated tree: candidateIdentity 54, outcomeIdentity 54,
	//     scopeNamespace 54, sourceReference 54, interventionIdentity 27 — the
	//     last is half because only the cutoff cases carry an intervention.
	//   - Widening an EVALUATOR-ONLY bound so it refuses a value the projection
	//     admits — len(c.Identity) >= MaxOrderedRulesIdentifierBytes on the
	//     cutoff, ordered_rules.go — fails on the PROPERTY, 27 cases, all of
	//     them bound:interventionIdentity, with STREAM_INVARIANT_VIOLATED. That
	//     is the defect class this whole case exists for.
	//   - Widening the SHARED validator the same way — checkIdentifier in
	//     ordered_rules_stream.go — does NOT fail on the property. The
	//     projection refuses first, the case is skipped, and the failure lands
	//     on the candidateIdentity counter instead. That is the correct outcome
	//     and it is structural: candidate and outcome identities have no
	//     evaluator-only length bound to diverge, because both paths call the
	//     same helper. The counters are what covers those two classes; the
	//     property covers the fields that are checked twice.
	for _, bf := range boundFields {
		if bf == "none" {
			continue
		}
		if atBound[bf] == 0 {
			t.Fatalf("no projected stream carried %s at MaxOrderedRulesIdentifierBytes, so the "+
				"length and identity tiers were never exercised at their edge for that field. "+
				"This counter exists because its predecessor counted only free text and was "+
				"green while exactly these fields went untested.", bf)
		}
	}
	t.Logf("%d projected streams evaluated; %d with a boundary, %d with a non-KNOWN balance; "+
		"at the per-string bound: %v", projected, cutoffSeen, nonKnownSeen, atBound)
}
