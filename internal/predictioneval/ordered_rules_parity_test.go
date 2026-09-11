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
	// THE LIST IS OF MECHANISM ANSWERS, AND EVERYTHING ELSE FAILS.
	//
	// It used to be the other way round: an allowlist of the seven ingest
	// reasons known at the time, with every other refusal silently accepted. A
	// reviewer read it as what it was — "an allowlist of currently known ingest
	// reasons, not an exclusion list of mechanism outcomes as the comment
	// claims" — and named the consequence exactly: the property intended to
	// catch the NEXT divergence would ignore it, because a newly named stream
	// refusal is not in a list written before it existed. A guard that fails
	// open on the case it was built for is not a guard, and this is the fourth
	// finding on this pull request of that shape.
	//
	// So this enumerates what a PROJECTED stream may legitimately be told by
	// the MECHANISM — it ran out of entropy, the balance is unusable, the pool
	// cannot be summed, the config is outside its domain, the work budget is
	// spent — and every other StatusRefused is a failure. The config and draws
	// below are fixed and valid, so the config-and-trace refusals
	// (SUPPLIED_TEXT_NOT_ENCODABLE, DRAW_WORDS_OVER_BOUND, RULE_COUNT_OVER_BOUND)
	// are NOT reachable here and are deliberately absent: if one ever fires,
	// this case should say so rather than wave it through.
	//
	// The cost of failing closed is that a genuinely NEW mechanism answer also
	// fails here until someone adds it. That is the right way round — a test
	// that stops a new answer until it is looked at beats one that hides a new
	// divergence — and it is stated so the next person does not read the
	// failure as a false alarm.
	mechanismAnswer := map[string]bool{
		predictioneval.ReasonEntropyExhausted:         true,
		predictioneval.ReasonBalanceInvalid:           true,
		predictioneval.ReasonBalanceNotSupplied:       true,
		predictioneval.ReasonBalanceOutOfDomain:       true,
		predictioneval.ReasonOutcomeVectorNotKnown:    true,
		predictioneval.ReasonOutcomePointsNotKnown:    true,
		predictioneval.ReasonOutcomePointsOutOfDomain: true,
		predictioneval.ReasonPoolSumOverflow:          true,
		predictioneval.ReasonAttemptRateOutOfDomain:   true,
		predictioneval.ReasonConfigOutOfDomain:        true,
		predictioneval.ReasonConfigDefaultNotSupplied: true,
		predictioneval.ReasonWorkBudgetExceeded:       true,
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

	// THE TWO VALID VIEW / SOURCE-KIND PAIRS, because one of them was never
	// projected here at all.
	//
	// Every candidate came from orCandidate, a CHANNEL_UPDATE, and every
	// admission from orAdmission, a CHANNEL_CANDIDATE_STREAM — so the
	// CALCULATE_ONLY / CALCULATE_SNAPSHOT pair was never evaluated, even though
	// source-kind/view compatibility is one of the six rules the evaluator
	// copies inline rather than sharing, and is named in this package as
	// drift-prone. A reviewer put it exactly: a change rejecting every
	// legitimately projected calculate-only stream with the existing invariant
	// reason left this case green, and the separate calculate-only test only
	// PROJECTS such a stream — it never calls EvaluateOrderedRules.
	//
	// A calculate-only admission also derives qualCalculateOnlyView, so this
	// dimension exercises a qualification the old space never produced.
	//
	// It is crossed with boundFields into one flat list rather than nested as a
	// seventh loop, which keeps the body at the indentation it already had.
	type shape struct {
		bf   string
		view predictioneval.OrderedRulesViewKind
		kind predictioneval.OrderedRulesSourceKind
	}
	var shapes []shape
	for _, v := range []shape{
		{view: predictioneval.ViewChannelCandidateStream, kind: predictioneval.SourceKindChannelUpdate},
		{view: predictioneval.ViewCalculateOnly, kind: predictioneval.SourceKindCalculateSnapshot},
	} {
		for _, bf := range boundFields {
			shapes = append(shapes, shape{bf: bf, view: v.view, kind: v.kind})
		}
	}

	projected, cutoffSeen, nonKnownSeen := 0, 0, 0
	// One counter per view, so a pair that stops projecting cannot hide behind
	// the other — which is the whole reason this dimension exists.
	perView := map[string]int{}
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
						for _, sh := range shapes {
							bf := sh.bf
							name := "c" + strconv.Itoa(c.candidates) + "x" + strconv.Itoa(c.outcomes) +
								"/text" + strconv.Itoa(n) + "/bal" + strconv.Itoa(bi) +
								"/" + string(cov) + "/cutoff" + strconv.FormatBool(cut) +
								"/" + string(sh.view) + "/bound:" + bf

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
								cand.SourceKind = sh.kind
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
							adm.ViewKind = sh.view
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
							perView[string(sh.view)]++
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

							if got.Status == predictioneval.StatusRefused && !mechanismAnswer[got.Reason] {
								t.Errorf("%s: ProjectOrderedRulesStream ADMITTED this source, and the "+
									"evaluator then refused the stream it produced with %q. That reason "+
									"is not one of the mechanism answers a projected stream may "+
									"legitimately receive, so either it is an ingest-validation refusal "+
									"— a rule the two paths disagree on, since every tier that can "+
									"produce one is supposed to mirror a rule the projection already "+
									"applied — or it is a NEW mechanism answer that belongs in the list "+
									"above. Both need a person; neither may be waved through.",
									name, got.Reason)
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
	// EVERY view must have projected. Without this the calculate-only half
	// could stop projecting entirely and the case would still pass on the
	// channel half alone — which is the exact failure mode that made this
	// dimension necessary in the first place.
	for _, v := range []predictioneval.OrderedRulesViewKind{
		predictioneval.ViewChannelCandidateStream, predictioneval.ViewCalculateOnly,
	} {
		if perView[string(v)] == 0 {
			t.Fatalf("no source projected under view %s, so that view/source-kind pair was never "+
				"evaluated at all. The pair is one of the six rules the evaluator copies inline, "+
				"and a counter that only proves the OTHER pair projected is how it went untested.", v)
		}
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
		"at the per-string bound: %v; per view: %v", projected, cutoffSeen, nonKnownSeen, atBound, perView)
}
