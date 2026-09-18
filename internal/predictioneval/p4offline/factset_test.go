package p4offline_test

// SEAMS 4–5: the outcome-free common factset and the exact P2 per-case
// configuration binding, with the P2 plumbing they feed.

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

// TestCommonFactsetIsOutcomeFreeAndStableAgainstLaterFacts pins seam 4.
//
// The digest must move for an INPUT and must not move for a RESULT, a
// post-cutoff fact, or the store's own row witness — which is exactly why
// the native pe-cid/v1 cannot serve here: it covers the terminal row's
// witness, and that row carries the results.
func TestCommonFactsetIsOutcomeFreeAndStableAgainstLaterFacts(t *testing.T) {
	ep, base := selectedFactset(t, nil, nil)
	if base.ContractVersion != p4offline.CommonFactsetDigestVersion || base.Protocol != p4offline.ProtocolVersion {
		t.Fatalf("factset carries %q / %q", base.ContractVersion, base.Protocol)
	}
	if base.Completeness != p4offline.FactsetComplete || base.StealthProof != p4offline.StealthProofOff {
		t.Fatalf("the fixture factset must be COMPLETE with stealth proven off: %+v", base)
	}
	if base.CutoffPosition != ep.Boundary.CutoffPosition || base.Attempt != ep.Attempt.Key {
		t.Fatalf("factset identity does not match the selection: %+v", base)
	}
	if base.Digest == "" || base.Digest == ep.Attempt.CommonInputDigest {
		t.Fatalf("the P4 factset digest must exist and must not be the native pe-cid: %q", base.Digest)
	}
	if err := p4offline.VerifyCommonFactset(base); err != nil {
		t.Fatalf("a freshly built factset must verify: %v", err)
	}
	ser := p4offline.SerializeCommonFactset(base)
	if len(ser) == 0 || !bytes.Equal(ser, p4offline.SerializeCommonFactset(base)) {
		t.Fatal("serialization must be deterministic and non-empty")
	}
	if !bytes.HasPrefix(ser, append([]byte{0, 0, 0, 0, 0, 0, 0, byte(len(p4offline.CommonFactsetDigestVersion))},
		[]byte(p4offline.CommonFactsetDigestVersion)...)) {
		t.Fatal("serialization must open with the length-prefixed contract version")
	}

	same := []struct {
		name   string
		mutate func(*predictioneval.SourceDecisionEnvelope)
		extra  func(*synth)
	}{
		{"recorded choice amount", func(e *predictioneval.SourceDecisionEnvelope) { e.ChoiceAmount = ptrI64(777777) }, nil},
		{"recorded final amount", func(e *predictioneval.SourceDecisionEnvelope) {
			e.FinalAmount = ptrI64(777777)
			e.StakeAllowed = ptrI64(777777)
		}, nil},
		{"recorded choice id", func(e *predictioneval.SourceDecisionEnvelope) { e.ChoiceOutcomeID = "recorded-choice-marker" }, nil},
		{"recorded skip verdict", func(e *predictioneval.SourceDecisionEnvelope) {
			e.SkipResult = ptrBool(true)
			e.SkipCompared = ptrF64(99.5)
		}, nil},
		{"recorded clamp", func(e *predictioneval.SourceDecisionEnvelope) {
			e.ClampApplied = ptrBool(true)
			e.StakeReason = "max_stake_percent"
		}, nil},
		{"post-cutoff settlement and traffic", nil, func(s *synth) {
			s.userTerminal("r1", "e1", "WON", 50, 120)
			s.add(s.fact("channel_event", "ROUND_UPDATED", "r1", "e1", 0))
		}},
	}
	for _, tc := range same {
		t.Run("unchanged by "+tc.name, func(t *testing.T) {
			_, fs := selectedFactset(t, tc.mutate, tc.extra)
			if fs.Digest != base.Digest {
				t.Fatalf("a %s changed the outcome-free factset digest", tc.name)
			}
			ser := p4offline.SerializeCommonFactset(fs)
			for _, marker := range []string{"777777", "recorded-choice-marker", "99.5", "max_stake_percent"} {
				if bytes.Contains(ser, []byte(marker)) {
					t.Fatalf("the serialization carries the recorded value %q", marker)
				}
			}
		})
	}

	// The store's row witness moves pe-cid and must NOT move the factset.
	t.Run("unchanged by the terminal row witness", func(t *testing.T) {
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		ds := s.dataset()
		for i := range ds.Records {
			if ds.Records[i].Payload.DecisionEnvelope != nil {
				ds.Records[i].ObservationSHA256 = "edited-witness"
			}
		}
		ep2 := singleEpisode(t, mustSelect(t, ds))
		fs2, err := p4offline.BuildCommonFactset(ds, ep2.Episode)
		if err != nil {
			t.Fatal(err)
		}
		if ep2.Attempt.CommonInputDigest == ep.Attempt.CommonInputDigest {
			t.Fatal("control failed: the native pe-cid did not move with the witness")
		}
		if fs2.Digest != base.Digest {
			t.Fatal("the factset digest followed the row witness; it must hash values, not witnesses")
		}
	})

	changed := []struct {
		name   string
		mutate func(*predictioneval.SourceDecisionEnvelope)
	}{
		{"the balance", func(e *predictioneval.SourceDecisionEnvelope) { e.Balance = ptrI64(1001) }},
		{"an outcome's points", func(e *predictioneval.SourceDecisionEnvelope) { e.Outcomes[1].TotalPoints = 401 }},
		{"an outcome's odds", func(e *predictioneval.SourceDecisionEnvelope) { e.Outcomes[0].Odds = math.Nextafter(1.66, 2) }},
		{"the strategy", func(e *predictioneval.SourceDecisionEnvelope) { e.Settings.Strategy = predictioneval.StrategyHighOdds }},
		{"the percentage", func(e *predictioneval.SourceDecisionEnvelope) { e.Settings.Percentage = 6 }},
		{"the filter", func(e *predictioneval.SourceDecisionEnvelope) {
			e.Settings.FilterCondition = &predictioneval.SourceFilterCondition{By: "odds", Where: "GT", Value: 1}
		}},
		{"the health verdict", func(e *predictioneval.SourceDecisionEnvelope) { e.HealthStage = predictioneval.HealthNoGate }},
		{"the reserve gate", func(e *predictioneval.SourceDecisionEnvelope) { e.RiskReservePoints = ptrInt(1) }},
	}
	for _, tc := range changed {
		t.Run("changed by "+tc.name, func(t *testing.T) {
			_, fs := selectedFactset(t, tc.mutate, nil)
			if fs.Digest == base.Digest {
				t.Fatalf("changing %s left the factset digest unchanged", tc.name)
			}
		})
	}

	t.Run("a tampered factset does not verify", func(t *testing.T) {
		tampered := base
		tampered.Balance++
		if err := p4offline.VerifyCommonFactset(tampered); !errors.Is(err, p4offline.ErrFactsetDigest) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("an excluded episode has no factset", func(t *testing.T) {
		s := newSynth()
		s.due("r1", "e1", 1)
		s.terminal("r1", "e1", 1, predictioneval.PhaseAutoSkipped, "BELOW_MINIMUM_POINTS", nil)
		ds := s.dataset()
		ep := singleEpisode(t, mustSelect(t, ds))
		if _, err := p4offline.BuildCommonFactset(ds, ep.Episode); !errors.Is(err, p4offline.ErrEpisodeNotSelected) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("an episode the dataset does not hold has no factset", func(t *testing.T) {
		ds, ep, _ := selectedCase(t, nil, nil)
		other := ep.Episode
		other.RoundIncarnationID = "r-not-there"
		if _, err := p4offline.BuildCommonFactset(ds, other); !errors.Is(err, p4offline.ErrEpisodeNotSelected) {
			t.Fatalf("got %v", err)
		}
		if _, err := p4offline.BuildCommonFactset(predictioneval.SourceDataset{}, ep.Episode); !errors.Is(err, p4offline.ErrEpisodeNotSelected) {
			t.Fatalf("an empty dataset selects nothing: %v", err)
		}
	})
}

// relabelled re-digests a hand-edited factset so it is internally consistent
// at the digest level: this constructs a hostile INPUT, not an expectation.
func relabelled(fs p4offline.CommonFactset) p4offline.CommonFactset {
	fs.Digest = digestOf(p4offline.SerializeCommonFactset(fs))
	return fs
}

// TestFactsetLabelsAreRederivedNotTrusted pins that a matching digest is not
// enough: a factset whose labels say more than its values do is refused by
// the verifier and by every consumer, however consistent its digest.
func TestFactsetLabelsAreRederivedNotTrusted(t *testing.T) {
	ds, _, fs := selectedCase(t, nil, nil)
	cases := []struct {
		name   string
		mutate func(*p4offline.CommonFactset)
	}{
		{"stealth ON relabelled COMPLETE with an honest proof label", func(f *p4offline.CommonFactset) {
			s := *f.Settings
			s.StealthMode = true
			f.Settings = &s
			f.StealthProof = p4offline.StealthProofOn
		}},
		{"stealth ON beside a FACTUAL_STEALTH_OFF label", func(f *p4offline.CommonFactset) {
			s := *f.Settings
			s.StealthMode = true
			f.Settings = &s
		}},
		// THE TWO HALVES ARE SEPARATE CASES, and the reason is the finding
		// that made them so. The single case below used to set BOTH fields, so
		// it satisfied both disjuncts of `!fs.ReachedDecision ||
		// fs.PreDecisionExit != ""` at once and could not tell `||` from
		// `&&`. An independent lane weakened that one token and the whole
		// package suite stayed green, while a factset asserting simultaneously
		// that the policy ran AND that it exited before running verified
		// clean, evaluated to WOULD_ATTEMPT and projected into P3b -- with
		// nothing masking it. Each disjunct now has a case that violates it
		// alone.
		{"COMPLETE beside a decision that was not reached", func(f *p4offline.CommonFactset) {
			f.ReachedDecision = false
		}},
		{"COMPLETE beside a recorded pre-decision exit", func(f *p4offline.CommonFactset) {
			f.PreDecisionExit = "NOT_ELIGIBLE"
		}},
		{"a pre-decision exit relabelled COMPLETE", func(f *p4offline.CommonFactset) {
			f.ReachedDecision = false
			f.PreDecisionExit = "NOT_ELIGIBLE"
		}},
		{"a missing balance relabelled COMPLETE", func(f *p4offline.CommonFactset) {
			f.BalancePresent = false
			f.Balance = 0
		}},
		{"a missing vector relabelled COMPLETE", func(f *p4offline.CommonFactset) {
			f.OutcomesPresent = false
			f.Outcomes = nil
		}},
		{"missing settings relabelled COMPLETE with an unknown proof", func(f *p4offline.CommonFactset) {
			f.Settings = nil
			f.StealthProof = p4offline.StealthProofUnknown
		}},
		{"INCOMPLETE without its value-derived reason", func(f *p4offline.CommonFactset) {
			f.BalancePresent = false
			f.Completeness = p4offline.FactsetIncomplete
			f.IncompleteReasons = []string{"SOMETHING_ELSE"}
		}},
		{"PRE_DECISION_EXIT beside a reached decision", func(f *p4offline.CommonFactset) {
			f.Completeness = p4offline.FactsetPreDecisionExit
		}},
		{"a completeness outside the vocabulary", func(f *p4offline.CommonFactset) {
			f.Completeness = "MOSTLY_COMPLETE"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hostile := fs
			tc.mutate(&hostile)
			hostile = relabelled(hostile)
			if err := p4offline.VerifyCommonFactset(hostile); !errors.Is(err, p4offline.ErrFactsetInconsistent) {
				t.Fatalf("VerifyCommonFactset: got %v, want ErrFactsetInconsistent", err)
			}
			if _, err := p4offline.EvaluateP2Case(hostile); !errors.Is(err, p4offline.ErrFactsetInconsistent) {
				t.Fatalf("P2 evaluated a relabelled factset: %v", err)
			}
			if _, err := p4offline.ProjectP3bSingleCandidate(hostile); !errors.Is(err, p4offline.ErrFactsetInconsistent) {
				t.Fatalf("P3b projected a relabelled factset: %v", err)
			}
			if _, err := p4offline.ProjectFactualPlacement(ds, hostile); !errors.Is(err, p4offline.ErrFactsetInconsistent) {
				t.Fatalf("placement read a relabelled factset: %v", err)
			}
		})
	}
	t.Run("a consistent factset from another dataset is not this dataset's", func(t *testing.T) {
		_, _, other := selectedCase(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Balance = ptrI64(2000) }, nil)
		if err := p4offline.VerifyCommonFactset(other); err != nil {
			t.Fatalf("control: %v", err)
		}
		if _, err := p4offline.ProjectFactualPlacement(ds, other); !errors.Is(err, p4offline.ErrFactsetNotDerived) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a foreign protocol label does not verify", func(t *testing.T) {
		hostile := fs
		hostile.Protocol = "p4-offline/v0"
		hostile = relabelled(hostile)
		if err := p4offline.VerifyCommonFactset(hostile); !errors.Is(err, p4offline.ErrFactsetDigest) {
			t.Fatalf("got %v", err)
		}
	})
}

// TestStealthOnOrUnknownExcludesTheCase pins FACTUAL_STEALTH_OFF: stealth on
// is out, and settings that are missing cannot prove it off.
func TestStealthOnOrUnknownExcludesTheCase(t *testing.T) {
	_, on := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Settings.StealthMode = true }, nil)
	if on.Completeness != p4offline.FactsetIncomplete || on.StealthProof != p4offline.StealthProofOn ||
		!containsString(on.IncompleteReasons, string(p4offline.StealthProofOn)) {
		t.Fatalf("%+v", on)
	}
	if _, err := p4offline.EvaluateP2Case(on); !errors.Is(err, p4offline.ErrFactsetNotEvaluable) {
		t.Fatalf("a stealth-on case must not be evaluated: %v", err)
	}
	_, unknown := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Settings = nil }, nil)
	if unknown.Completeness != p4offline.FactsetIncomplete || unknown.StealthProof != p4offline.StealthProofUnknown ||
		!containsString(unknown.IncompleteReasons, predictioneval.IneligibleMissingSettings) {
		t.Fatalf("missing settings cannot prove stealth off: %+v", unknown)
	}
	if _, err := p4offline.BindP2Config(unknown); !errors.Is(err, p4offline.ErrConfigBindingMissing) {
		t.Fatalf("no settings, no binding: %v", err)
	}
}

// TestKnownZeroBalanceIsNotAMissingBalance pins missing != zero at the
// factset and through P2: a KNOWN zero balance evaluates (to a skip), a
// missing balance is INCOMPLETE and never becomes zero.
func TestKnownZeroBalanceIsNotAMissingBalance(t *testing.T) {
	_, zero := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) {
		e.Balance = ptrI64(0)
		e.ChoiceAmount = ptrI64(0)
		e.StakeAllowed = ptrI64(0)
		e.FinalAmount = ptrI64(0)
	}, nil)
	if zero.Completeness != p4offline.FactsetComplete || !zero.BalancePresent || zero.Balance != 0 {
		t.Fatalf("%+v", zero)
	}
	res, err := p4offline.EvaluateP2Case(zero)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action.Class != p4offline.ActionPolicySkip || res.Action.SkipReason != predictioneval.ActionBelowMinimum {
		t.Fatalf("5%% of a KNOWN zero is a stake of 0, below the minimum of 10: %+v", res.Action)
	}
	if !res.Stake.Known() || res.Stake.Value != 0 {
		t.Fatalf("a POLICY_SKIP carries an exact zero stake: %+v", res.Stake)
	}
	_, missing := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Balance = nil }, nil)
	if missing.Completeness != p4offline.FactsetIncomplete || missing.BalancePresent ||
		!containsString(missing.IncompleteReasons, predictioneval.IneligibleMissingBalance) {
		t.Fatalf("%+v", missing)
	}
	if missing.Digest == zero.Digest {
		t.Fatal("a missing balance and a known zero share a digest")
	}
	if _, err := p4offline.EvaluateP2Case(missing); !errors.Is(err, p4offline.ErrFactsetNotEvaluable) {
		t.Fatalf("a missing balance is not evaluable: %v", err)
	}
}

// TestOutcomeVectorPresenceIsCarriedNotRepaired pins that a hole in the
// model vector stays a hole (P2 reports its own legacy failure on it) and
// that an EMPTY vector is a real input rather than a missing one.
func TestOutcomeVectorPresenceIsCarriedNotRepaired(t *testing.T) {
	_, hole := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) {
		e.Outcomes[0].Present = false
	}, nil)
	if hole.Completeness != p4offline.FactsetComplete || !hole.OutcomesPresent || hole.Outcomes[0].Present {
		t.Fatalf("%+v", hole)
	}
	res, err := p4offline.EvaluateP2Case(hole)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action.Class != p4offline.ActionLegacyFailure || !res.Action.Legal {
		t.Fatalf("the pinned policy dereferences the hole; the map must say LEGACY_FAILURE: %+v", res.Action)
	}
	if res.Stake.Presence != p4offline.PresenceNotReached {
		t.Fatalf("no stake exists after a legacy failure: %+v", res.Stake)
	}
	_, empty := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) {
		e.Outcomes = nil
		e.ChoiceIndex = nil
		e.ChoiceOutcomeID = ""
	}, nil)
	if empty.Completeness != p4offline.FactsetComplete || !empty.OutcomesPresent || len(empty.Outcomes) != 0 {
		t.Fatalf("an empty vector is a real, present input: %+v", empty)
	}
	if empty.Digest == hole.Digest {
		t.Fatal("digests must separate the two vectors")
	}
}

// TestPerCaseConfigBindingFollowsEachCasesOwnSettings pins seam 5.
func TestPerCaseConfigBindingFollowsEachCasesOwnSettings(t *testing.T) {
	_, a := selectedFactset(t, nil, nil)
	_, b := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Settings.Percentage = 7 }, nil)
	_, c := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Balance = ptrI64(2000) }, nil)
	ba, err := p4offline.BindP2Config(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := p4offline.BindP2Config(b)
	if err != nil {
		t.Fatal(err)
	}
	bc, err := p4offline.BindP2Config(c)
	if err != nil {
		t.Fatal(err)
	}
	if ba.ContractVersion != p4offline.P2ConfigBindingVersion || ba.Digest == "" {
		t.Fatalf("%+v", ba)
	}
	if ba.Model != predictioneval.CurrentModelProvenance() {
		t.Fatalf("the binding must name the P2 model and policy revision: %+v", ba.Model)
	}
	if ba.Digest == bb.Digest {
		t.Fatal("two cases with different percentages share a config binding")
	}
	if ba.Digest != bc.Digest {
		t.Fatal("a different balance is not a different configuration")
	}
	if ba.Settings.Percentage != 5 || bb.Settings.Percentage != 7 || ba.MinimumStake != predictioneval.PinnedMinimumStake {
		t.Fatalf("%+v / %+v", ba.Settings, bb.Settings)
	}
	res, err := p4offline.EvaluateP2Case(b)
	if err != nil {
		t.Fatal(err)
	}
	if res.Binding.Digest != bb.Digest || res.FactsetDigest != b.Digest || res.Policy != p4offline.PolicyP2 {
		t.Fatalf("the P2 result must carry its own case's binding and factset: %+v", res)
	}
	// 7% of 1000 is 70: the evaluation used THIS case's percentage.
	if res.Action.Class != p4offline.ActionWouldAttempt || !res.Stake.Known() || res.Stake.Value != 70 ||
		!res.Choice.Present || res.Choice.OutcomeID != "o1" {
		t.Fatalf("%+v %+v %+v", res.Action, res.Stake, res.Choice)
	}
}

// TestP2ConsumesNoEntropyAndIsDeterministic pins NO_STEALTH_SIGNAL on the P2
// side: the evaluation takes no trace by signature, and two evaluations of
// one factset are identical.
func TestP2ConsumesNoEntropyAndIsDeterministic(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	a, err := p4offline.EvaluateP2Case(fs)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p4offline.EvaluateP2Case(fs)
	if err != nil {
		t.Fatal(err)
	}
	if a.Evaluation.Action != b.Evaluation.Action || a.Stake != b.Stake || a.Choice != b.Choice ||
		a.Evaluation.CommonInputDigest != fs.Digest {
		t.Fatalf("P2 must be a pure function of the factset, bound to it: %+v / %+v", a, b)
	}
	if a.Evaluation.Stealth.Outcome != predictioneval.StealthNotApplicable {
		t.Fatalf("stealth is off by construction: %+v", a.Evaluation.Stealth)
	}
}

// TestIntegerBoundariesAreRefusedNotWrapped pins the int boundary through
// P2: a balance whose percent-gate product would wrap the pinned policy's
// int arithmetic is INDETERMINATE, mapped to UNKNOWN_INPUT, never a number.
func TestIntegerBoundariesAreRefusedNotWrapped(t *testing.T) {
	_, fs := selectedFactset(t, func(e *predictioneval.SourceDecisionEnvelope) {
		e.Balance = ptrI64(math.MaxInt64)
		e.RiskMaxStakePercent = ptrInt(3)
		e.StakeReason = "max_stake_percent"
		e.ClampApplied = ptrBool(true)
	}, nil)
	if fs.Completeness != p4offline.FactsetComplete {
		t.Fatalf("%+v", fs.IncompleteReasons)
	}
	res, err := p4offline.EvaluateP2Case(fs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Evaluation.Action != predictioneval.ActionIndeterminate ||
		res.Action.Class != p4offline.ActionUnknownInput || !res.Action.Legal {
		t.Fatalf("%+v", res.Action)
	}
	if res.Stake.Known() {
		t.Fatalf("no stake may be reported from wrapped arithmetic: %+v", res.Stake)
	}
}

// TestIncompleteFactsetRefusalIsBoundedInItsOwnInput is the RED receipt for the
// quadratic refusal an independent security lane found on the exported
// EvaluateP2Case path, and the guard that keeps it closed.
//
// IncompleteReasons is a plain exported []string that this package's own
// verifier accepts without a count bound or a per-string bound -- containsID
// requires only that the DERIVED reasons are a subset of the supplied ones, so
// arbitrary padding is legal by the package's own rules. The refusal then
// concatenated every one of them with repeated `+=`, copying the whole
// accumulated prefix each time. Measured by the lane: 1,018,297,776 bytes and
// 845 ms at 1,000 reasons rising to 64,420,331,144 at 8,000 -- exactly 4x the
// allocation per 2x the input. Reproduced here at 4,097 reasons:
// 1,266,167,840 bytes allocated and a 302,086-byte error.
//
// TWO ASSERTIONS, and the second one is the one that names the defect.
//
//   - The error must be bounded. A refusal names the fault, never the input;
//     the budget is stated here and is not read from the code under test.
//   - The allocation must grow LINEARLY. It cannot be asserted below the input,
//     and pretending otherwise would be the wrong test: VerifyCommonFactset
//     runs first and has to digest the whole factset, reasons included, so a
//     constant-factor multiple of the input is the honest floor. What
//     distinguishes the defect is the EXPONENT -- doubling the list doubled the
//     work before, and quadrupled it under the defect.
//
// TestIncompleteFactsetWithNoDerivedFaultRefusesWithoutPanicking reaches
// joinReasons with an EMPTY list through the exported EvaluateP2Case.
//
// The single-pass rewrite computes its buffer as `len(rs) - 1 + sum(len)`, so
// an empty list gives a capacity of -1 and `make([]byte, 0, -1)` PANICS. The
// guard against that was added with the rewrite and was pinned by nothing: a
// mutation deleting it survived the whole suite. This is the production path
// that reaches it -- an INCOMPLETE factset whose VALUES are all present, so
// valueDerivedReasons returns nothing while IncompleteReasons is non-empty.
func TestIncompleteFactsetWithNoDerivedFaultRefusesWithoutPanicking(t *testing.T) {
	_, _, fs := selectedCase(t, nil, nil)
	fs.Completeness = p4offline.FactsetIncomplete
	fs.IncompleteReasons = []string{"SOMETHING_THE_VALUES_DO_NOT_DERIVE"}
	fs.Digest = digestOf(p4offline.SerializeCommonFactset(fs))
	if err := p4offline.VerifyCommonFactset(fs); err != nil {
		t.Fatalf("the fixture must be a factset this package accepts: %v", err)
	}
	// Reaching this line at all is the assertion: without the guard the call
	// panics with "makeslice: cap out of range" rather than returning.
	_, err := p4offline.EvaluateP2Case(fs)
	if !errors.Is(err, p4offline.ErrFactsetNotEvaluable) {
		t.Fatalf("an INCOMPLETE factset must be refused as not evaluable, got %v", err)
	}
	if !strings.Contains(err.Error(), "1 reasons") {
		t.Fatalf("the refusal must still name the supplied extent: %v", err)
	}
}

func TestIncompleteFactsetRefusalIsBoundedInItsOwnInput(t *testing.T) {
	const budget = 1024
	build := func(t *testing.T, n int) (p4offline.CommonFactset, int) {
		t.Helper()
		_, _, fs := selectedCase(t, nil, nil)
		fs.Completeness = p4offline.FactsetIncomplete
		fs.BalancePresent = false
		fs.Balance = 0
		fs.IncompleteReasons = []string{"MISSING_BALANCE"}
		for i := 0; i < n; i++ {
			fs.IncompleteReasons = append(fs.IncompleteReasons, "PAD_"+strings.Repeat("x", 64)+"_"+strconv.Itoa(i))
		}
		fs.Digest = digestOf(p4offline.SerializeCommonFactset(fs))
		if err := p4offline.VerifyCommonFactset(fs); err != nil {
			t.Fatalf("the fixture must be a factset this package accepts: %v", err)
		}
		bytes := 0
		for _, r := range fs.IncompleteReasons {
			bytes += len(r)
		}
		return fs, bytes
	}
	measure := func(t *testing.T, fs p4offline.CommonFactset) (uint64, error) {
		t.Helper()
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := p4offline.EvaluateP2Case(fs)
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc, err
	}

	small, smallBytes := build(t, 2048)
	large, largeBytes := build(t, 4096)
	smallAlloc, smallErr := measure(t, small)
	largeAlloc, err := measure(t, large)
	t.Logf("%d bytes of reasons -> %d bytes allocated; %d bytes -> %d bytes allocated; %d-byte error",
		smallBytes, smallAlloc, largeBytes, largeAlloc, len(err.Error()))

	if !errors.Is(err, p4offline.ErrFactsetNotEvaluable) || !errors.Is(smallErr, p4offline.ErrFactsetNotEvaluable) {
		t.Fatalf("an INCOMPLETE factset must be refused as not evaluable, got %v / %v", smallErr, err)
	}
	if n := len(err.Error()); n > budget {
		t.Fatalf("the refusal carries %d bytes; a refusal must name the fault, not the input (budget %d)", n, budget)
	}
	// Linear over a 2x input is ~2x; quadratic is ~4x. The bound is loose
	// enough that allocator differences cannot trip it and far below the
	// quadratic shape.
	if ratio := float64(largeAlloc) / float64(smallAlloc); ratio > 3 {
		t.Fatalf("allocation grew %.1fx over a 2x input: the reason list is being rendered quadratically", ratio)
	}

	t.Run("the refusal still names the completeness, the count and the total", func(t *testing.T) {
		if !strings.Contains(err.Error(), string(p4offline.FactsetIncomplete)) ||
			!strings.Contains(err.Error(), strconv.Itoa(len(large.IncompleteReasons))+" reasons") ||
			!strings.Contains(err.Error(), strconv.Itoa(largeBytes)+" bytes") {
			t.Fatalf("the refusal must name the label, the count and the total: %v", err)
		}
		// AND THE FAULT. Naming only the extent tells the caller nothing about
		// what was wrong, which is the other half of this package's own rule.
		// The fault is re-derived from the factset's own VALUES, so it is
		// bounded whatever the caller supplied.
		if !strings.Contains(err.Error(), "MISSING_BALANCE") {
			t.Fatalf("the refusal must name the derived fault, not only its extent: %v", err)
		}
		// And it must not carry the reasons themselves.
		if strings.Contains(err.Error(), "PAD_") {
			t.Fatalf("the refusal re-exported the reasons it refused: %v", err)
		}
	})
}

// TestFirstGatesDoNotMaterializeSuppliedText is the class test, and it exists
// because this defect has now been found in EIGHT successive review rounds: at
// VerifyP3bRuleset's identity gate; then at ValidateDrawTrace's two gates and
// bindEntropyCoordinates' round gate; then at six more, including
// VerifyCommonFactset, the first gate on every factset path; then at three MORE
// that the rule's own wording had excluded, because it was scoped to "the first
// statement of an exported function" and these sit further down the same two
// functions, reached the moment the digest verifies -- which a caller can make
// it do, since the serializers are exported and the digest is unkeyed; and then
// at a site that is not a gate at all, the derivation argument Decision builds
// before decisionOf's first statement runs.
//
// So this is a table over the gates of that shape that are reachable as a plain
// first-gate comparison through an exported verifier, and a new gate of that
// shape belongs here on the day it is written. It is a REGISTRY, not a proof of
// enumeration -- see the paragraph below, which exists because this sentence
// used to claim more than any test can.
//
// EACH ROW ASSERTS THE RENDERED EXTENT, not merely that the error is small. A
// first version asserted only the sentinel, a 1024-byte budget and the absence
// of the input -- and a gate reporting the WRONG extent (this package's own
// constant instead of the caller's string) satisfied all three. Three of five
// rows survived a field swap. The extent is the property the repair exists for,
// so it is the property that is checked.
//
// SIX SITES OF THE CLASS ARE PINNED ELSEWHERE, in the five rows below -- one
// row covers two gates -- and they are named here rather than duplicated:
//
//	VerifyP3bRuleset's identity gate  TestRulesetIdentityRefusalDoesNotMaterializeSuppliedText
//	ValidateDrawTrace's two gates     TestDrawTraceRefusalDoesNotMaterializeSuppliedText
//	bindEntropyCoordinates' round     TestEntropyBindingRefusalDoesNotMaterializeTheFactsetRound
//	Decision's derivation thunk       TestDecisionDoesNotBuildItsDerivationBeforeItsGates
//	  (BOTH callers -- P2CaseResult.Decision and P3bCaseResult.Decision -- and
//	  the mint check below them)
//	walkRulesetObject's key gate      TestRulesetKeyRefusalsDoNotMaterializeTheSuppliedKey
//
// With the nine rows below that is FIFTEEN sites: 9 + 6. Every number in this
// paragraph is from a scan of the source -- 9 table rows, 10 production call
// sites of suppliedTextExtent, of which one (walkRulesetObject's) needs a raw
// document and a raised ceiling to reach and so is pinned elsewhere. canonical.go
// carries the same inventory beside the rule.
//
// THE PREVIOUS VERSION OF THIS PARAGRAPH GOT TWO OF THOSE NUMBERS WRONG, in the
// same edit that claimed they were read off the source by a scan. It said FIVE
// sites where the rows name six, because a row was added without rescanning;
// and the new row was inserted ABOVE the indented continuation belonging to the
// Decision row, so the parenthetical about two callers and a mint check read as
// if it described walkRulesetObject, which has one caller and no mint check.
// Claiming a scan is not performing one.
//
// WHAT THIS TABLE IS NOT. It is the registry for the sites reachable as a plain
// first-gate comparison through an exported verifier. It is NOT a proof that the
// class is enumerated, and no test can be one. An earlier version said it
// covered every such gate "wherever it sits" and called its inventory whole;
// that was false while walkRulesetObject's key gate sat outside it, and an
// auditor reading it as an enumeration would have been misled about what is
// proven. The class has now been found in EIGHT successive review rounds;
// canonical.go carries the site counts, because it is where the rule lives and
// one place should own them. The honest statement is that every site KNOWN to
// this branch is listed here or named above -- not that no further one exists.
//
// The Decision row used to name one test while that test drove ONE of its two
// callers; a lane reverted the other and the whole suite passed, so the row
// promised more than it held. It names what it covers now.
func TestFirstGatesDoNotMaterializeSuppliedText(t *testing.T) {
	const budget = 1024
	big := strings.Repeat("a", 1<<20)
	wantExtent := strconv.Itoa(len(big)) + " bytes"
	_, _, good := selectedCase(t, nil, nil)
	unknownArtifact := func() p4offline.ResolutionArtifact {
		return p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, nil, "p")
	}

	for _, tc := range []struct {
		name        string
		call        func() error
		sentinel    error
		names       string
		belowDigest bool
	}{
		{"a factset contract version", func() error {
			fs := good
			fs.ContractVersion = big
			return p4offline.VerifyCommonFactset(fs)
		}, p4offline.ErrFactsetDigest, "factset contract is", false},
		{"a factset protocol version", func() error {
			fs := good
			fs.Protocol = big
			return p4offline.VerifyCommonFactset(fs)
		}, p4offline.ErrFactsetDigest, "factset protocol is", false},
		{"a factset digest", func() error {
			fs := good
			fs.Digest = big
			return p4offline.VerifyCommonFactset(fs)
		}, p4offline.ErrFactsetDigest, "factset digest of", false},
		{"a factset stealth proof, below the digest gate", func() error {
			fs := good
			fs.StealthProof = p4offline.StealthProof(big)
			fs.Digest = digestOf(p4offline.SerializeCommonFactset(fs))
			return p4offline.VerifyCommonFactset(fs)
		}, p4offline.ErrFactsetInconsistent, "stealth proof is", true},
		{"a factset completeness, below the digest gate", func() error {
			fs := good
			fs.Completeness = p4offline.FactsetCompleteness(big)
			fs.Digest = digestOf(p4offline.SerializeCommonFactset(fs))
			return p4offline.VerifyCommonFactset(fs)
		}, p4offline.ErrFactsetInconsistent, "completeness of", true},
		{"a resolution artifact contract version", func() error {
			a := unknownArtifact()
			a.ContractVersion = big
			return p4offline.VerifyResolutionArtifact(a)
		}, p4offline.ErrResolutionDigest, "artifact contract is", false},
		{"a resolution obligations revision", func() error {
			a := unknownArtifact()
			a.ObligationsRevision = big
			return p4offline.VerifyResolutionArtifact(a)
		}, p4offline.ErrResolutionDigest, "artifact obligations revision is", false},
		{"a resolution outcome, below the digest gate", func() error {
			a := unknownArtifact()
			a.Outcome = p4offline.ResolutionOutcome(big)
			a.ResolutionFactsDigest = p4offline.DigestReference(digestOf(p4offline.SerializeResolutionArtifact(a)))
			return p4offline.VerifyResolutionArtifact(a)
		}, p4offline.ErrResolutionNotDerivable, "outcome of", true},
		{"a policy result's own factset digest", func() error {
			// The SUPPLIED side is the result's own FactsetDigest, a plain
			// exported field. The factset itself must stay valid, or
			// VerifyCommonFactset refuses one gate earlier and this row
			// measures that gate instead -- which is exactly what a first
			// version of this row did, so the binding gate went unreached and
			// a swap of its two arguments survived.
			res, err := p4offline.EvaluateP2Case(good)
			if err != nil {
				t.Fatalf("fixture: %v", err)
			}
			res.FactsetDigest = big
			_, derr := res.Decision(good)
			return derr
		}, p4offline.ErrDecisionBinding, "evaluated over a factset digest of", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			err := tc.call()
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("1 MiB supplied; the refusing call allocated %d bytes", allocated)

			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("want %v, got %v", tc.sentinel, err)
			}
			if n := len(err.Error()); n > budget {
				t.Fatalf("the refusal carries %d bytes; a refusal must name the fault, not the input (budget %d)", n, budget)
			}
			if strings.Contains(err.Error(), big) {
				t.Fatalf("the refusal re-exported the string it refused")
			}
			// THE EXTENT OF THE FIELD UNDER TEST, not merely a small error: a
			// gate reporting this package's own constant instead of the
			// caller's string passes everything above and nothing here.
			if !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("the refusal must name its own fault (%q): %v", tc.names, err)
			}
			if !strings.Contains(err.Error(), wantExtent) {
				t.Fatalf("the refusal must name the SUPPLIED extent (%q): %v", wantExtent, err)
			}
			// And it must not copy the supplied string -- with a floor that
			// depends on WHERE the gate sits, stated rather than fudged. A gate
			// above the digest check refuses before anything reads the factset,
			// so its cost is constant. A gate BELOW it is only reached once the
			// digest verifies, and verifying means serializing the whole
			// factset, big string included: a small multiple of the supplied
			// bytes is the honest floor there, and what the row proves is that
			// the REFUSAL does not add another copy on top.
			budgetBytes := uint64(len(big)) / 2
			if tc.belowDigest {
				budgetBytes = 3 * uint64(len(big))
			}
			if allocated > budgetBytes {
				t.Fatalf("the refusing call allocated %d bytes against a %d-byte supplied string (budget %d)",
					allocated, len(big), budgetBytes)
			}
		})
	}

	t.Run("and a gated digest is still NAMED, not reduced to its length", func(t *testing.T) {
		// THE CONVERSE PROPERTY, and the one a blanket application of the rule
		// gets wrong. decisionOf runs VerifyCommonFactset first, which pins
		// fs.Digest to 64 hex characters of this package's own making, so
		// reporting it by extent would render "64 bytes" for every binding
		// mismatch there is. It must be NAMED.
		//
		// A first version of this subtest drove a MATCHING binding and asserted
		// only that it succeeded, so it produced no refusal message at all and
		// reverting the repair survived the whole suite. It drives a real
		// mismatch now.
		res, err := p4offline.EvaluateP2Case(good)
		if err != nil {
			t.Fatalf("fixture: %v", err)
		}
		// A second factset that is well-formed and DIFFERENT. The change has to
		// be one the factset actually carries -- it is outcome-free, so editing
		// the recorded envelope does not move its digest, which a first version
		// of this subtest discovered the hard way.
		otherFs := good
		otherFs.Balance = good.Balance + 1
		otherFs.Digest = digestOf(p4offline.SerializeCommonFactset(otherFs))
		if err := p4offline.VerifyCommonFactset(otherFs); err != nil {
			t.Fatalf("the second fixture must verify: %v", err)
		}
		if otherFs.Digest == good.Digest {
			t.Fatal("the two fixtures must differ, or there is no mismatch to report")
		}
		_, derr := res.Decision(otherFs)
		if !errors.Is(derr, p4offline.ErrDecisionBinding) {
			t.Fatalf("a foreign factset must be a binding fault: %v", derr)
		}
		if !strings.Contains(derr.Error(), otherFs.Digest) {
			t.Fatalf("the factset's own digest is gated to 64 hex and must be NAMED: %v", derr)
		}
		if !strings.Contains(derr.Error(), strconv.Itoa(len(res.FactsetDigest))+" bytes") {
			t.Fatalf("the supplied result digest must be reported by extent: %v", derr)
		}
	})

	t.Run("and each refusal still names the constant it wanted", func(t *testing.T) {
		// THE OTHER HALF OF THE RULE. Naming the package-owned constant is what
		// makes the refusal a diagnosis rather than a size report, and
		// canonical.go says so twice -- but the table above asserts only the
		// fault wording and the supplied extent, so six mutants that replace a
		// quoted constant with a literal survived it.
		fs := good
		fs.ContractVersion = big
		e1 := p4offline.VerifyCommonFactset(fs)
		fs = good
		fs.Protocol = big
		e2 := p4offline.VerifyCommonFactset(fs)
		fs = good
		fs.Digest = big
		e3 := p4offline.VerifyCommonFactset(fs)
		fs = good
		fs.StealthProof = p4offline.StealthProof(big)
		fs.Digest = digestOf(p4offline.SerializeCommonFactset(fs))
		e4 := p4offline.VerifyCommonFactset(fs)

		a := unknownArtifact()
		a.ContractVersion = big
		e5 := p4offline.VerifyResolutionArtifact(a)
		a = unknownArtifact()
		a.ObligationsRevision = big
		e6 := p4offline.VerifyResolutionArtifact(a)

		for _, tc := range []struct {
			err  error
			want string
		}{
			{e1, strconv.Quote(p4offline.CommonFactsetDigestVersion)},
			{e2, strconv.Quote(p4offline.ProtocolVersion)},
			{e4, strconv.Quote(string(p4offline.StealthProofOff))},
			{e5, strconv.Quote(p4offline.ResolutionFactsDigestVersion)},
			{e6, strconv.Quote(p4offline.ResolutionObligationsRevision)},
		} {
			if tc.err == nil || !strings.Contains(tc.err.Error(), tc.want) {
				t.Fatalf("the refusal must name the constant it wanted (%s): %v", tc.want, tc.err)
			}
		}
		// The digest gate names the digest this package computed, which is not
		// a compile-time constant but is its own 64 hex characters -- so it is
		// asserted BY VALUE, like the five rows above, and not by its shape. An
		// earlier draft of this row looked only for `which digest to "`, and a
		// mutant that keeps the quotes and loses the value -- a fixed 64-zero
		// digest reported for every mismatch there is -- survived the whole
		// suite. That is precisely the sentence-that-reads-the-same-for-every-
		// input failure this half of the rule exists to prevent, so shape is
		// not enough here either. The subtest overwrites only fs.Digest, so the
		// digest the VALUES produce is still good's own.
		if e3 == nil || !strings.Contains(e3.Error(), strconv.Quote(good.Digest)) {
			t.Fatalf("the digest fault must name the digest the values produce (%s): %v", good.Digest, e3)
		}
		if e1.Error() == e2.Error() {
			t.Fatalf("the two faults must be distinguishable: %v / %v", e1, e2)
		}
	})
}

// nonFiniteValues are one representative of each class encoding/json refuses.
// It is NOT every such float64: a NaN has many bit patterns and this list
// carries one of them (math.NaN() is 7ff8000000000001 here, while 0/0 and
// Inf-Inf both give fff8000000000000). The gate tests IS-NaN rather than a bit
// pattern, so one representative exercises it; the list is a sample, and
// saying otherwise would contradict doc.go's own point that a NaN has many
// bit patterns. A finite value always encodes, both zeroes included.
var nonFiniteValues = []struct {
	name string
	v    float64
}{
	{"NaN", math.NaN()},
	{"+Inf", math.Inf(1)},
	{"-Inf", math.Inf(-1)},
}

// withFilter gives the fixture a finite filter condition, so the two settings
// float positions are both present and both reachable.
func withFilter(e *predictioneval.SourceDecisionEnvelope) {
	e.Settings.FilterCondition = &predictioneval.SourceFilterCondition{
		By: predictioneval.OutcomePercentageUsers, Where: predictioneval.ConditionGT, Value: 25,
	}
}

// TestNonFiniteFactsetValuesAreRefusedBeforeTheyAreDigested pins the repair of
// a defect two independent reviews reported: a non-finite float made a factset
// that VerifyCommonFactset ACCEPTED and that encoding/json refuses to write, so
// this package certified an artifact no consumer could store — while the
// package's own storage-and-rederivation argument assumes it can.
//
// Each case asserts four things, because no one of them alone would fail for
// the right reason:
//
//   - the poked value really is unserializable, so the refusal has a cause
//     rather than a preference;
//   - the DATASET path refuses it, so no such factset is ever built;
//   - the VERIFY path refuses it as ErrFactsetInconsistent and NOT as
//     ErrFactsetDigest — which places the check ahead of the digest GATE. It
//     does not, by itself, place it ahead of the HASHING: an implementation
//     that hashed first and compared later would satisfy this too. The code
//     computes and compares in one statement, so the two coincide there;
//   - the artifact the finding actually described — a factset whose digest was
//     computed OVER the non-finite value, so the digest is RIGHT — is refused.
//     That case used to return nil, and no assertion above reaches it, because
//     every other poke here leaves a stale digest;
//   - the same poke with a FINITE value is refused by the digest gate instead,
//     which is the control that keeps the third assertion from passing merely
//     because a poked factset fails somehow.
func TestNonFiniteFactsetValuesAreRefusedBeforeTheyAreDigested(t *testing.T) {
	// says is the text the refusal must carry. It is asserted because the
	// index is otherwise a free variable: an independent lane showed that
	// replacing strconv.Itoa(i) with strconv.Itoa(0) leaves the whole package
	// suite green, so the restructuring that closed an allocation defect in
	// this very expression had its own correctness unpinned. Two of the five
	// positions poke outcome 1, so a constant index cannot satisfy both.
	positions := []struct {
		name  string
		says  string
		place func(*predictioneval.SourceDecisionEnvelope, float64)
		poke  func(*p4offline.CommonFactset, float64)
	}{
		{"outcome percentage users", "outcome 0 percentage users is",
			func(e *predictioneval.SourceDecisionEnvelope, v float64) { e.Outcomes[0].PercentageUsers = v },
			func(fs *p4offline.CommonFactset, v float64) { fs.Outcomes[0].PercentageUsers = v }},
		{"outcome odds", "outcome 1 odds is",
			func(e *predictioneval.SourceDecisionEnvelope, v float64) { e.Outcomes[1].Odds = v },
			func(fs *p4offline.CommonFactset, v float64) { fs.Outcomes[1].Odds = v }},
		{"outcome odds percentage", "outcome 0 odds percentage is",
			func(e *predictioneval.SourceDecisionEnvelope, v float64) { e.Outcomes[0].OddsPercentage = v },
			func(fs *p4offline.CommonFactset, v float64) { fs.Outcomes[0].OddsPercentage = v }},
		{"settings delay", "settings delay is",
			func(e *predictioneval.SourceDecisionEnvelope, v float64) { e.Settings.Delay = v },
			func(fs *p4offline.CommonFactset, v float64) { fs.Settings.Delay = v }},
		{"settings filter condition value", "settings filter condition value is",
			func(e *predictioneval.SourceDecisionEnvelope, v float64) { e.Settings.FilterCondition.Value = v },
			func(fs *p4offline.CommonFactset, v float64) { fs.Settings.FilterCondition.Value = v }},
	}

	t.Run("the finite fixture still builds, verifies and encodes", func(t *testing.T) {
		_, fs := selectedFactset(t, withFilter, nil)
		if err := p4offline.VerifyCommonFactset(fs); err != nil {
			t.Fatalf("VerifyCommonFactset: %v", err)
		}
		if _, err := json.Marshal(fs); err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if _, err := p4offline.BindP2Config(fs); err != nil {
			t.Fatalf("BindP2Config: %v", err)
		}
	})

	for _, pos := range positions {
		for _, nf := range nonFiniteValues {
			t.Run(pos.name+" "+nf.name, func(t *testing.T) {
				// The cause: this value cannot be written down at all.
				poked := func(v float64) p4offline.CommonFactset {
					_, fs := selectedFactset(t, withFilter, nil)
					pos.poke(&fs, v)
					return fs
				}
				if _, err := json.Marshal(poked(nf.v)); err == nil {
					t.Fatalf("json.Marshal accepted %v; this case proves nothing", nf.name)
				}

				// The dataset path: no such factset is built.
				s := newSynth()
				s.due("r1", "e1", 1)
				env := synthPlacedEnvelope()
				withFilter(env)
				pos.place(env, nf.v)
				s.terminal("r1", "e1", 1, predictioneval.PhaseAutoDecided, "OK", env)
				s.call("r1", "e1", 1, 50, 0)
				ds := s.dataset()
				ep := singleEpisode(t, mustSelect(t, ds))
				if _, err := p4offline.BuildCommonFactset(ds, ep.Episode); !errors.Is(err, p4offline.ErrFactsetInconsistent) {
					t.Fatalf("BuildCommonFactset = %v, want ErrFactsetInconsistent", err)
				}

				// The verify path, ahead of the digest GATE.
				err := p4offline.VerifyCommonFactset(poked(nf.v))
				if !errors.Is(err, p4offline.ErrFactsetInconsistent) {
					t.Fatalf("VerifyCommonFactset = %v, want ErrFactsetInconsistent", err)
				}
				if errors.Is(err, p4offline.ErrFactsetDigest) {
					t.Fatalf("refused by the digest gate rather than by the value: %v", err)
				}
				// The refusal must name the position it refused, index
				// included; see the table's comment for why.
				if want := pos.says + " " + nf.name; !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal does not name the position: want %q in %v", want, err)
				}

				// THE ARTIFACT THE FINDING DESCRIBED: a factset whose digest is
				// computed over the non-finite value, so the digest is RIGHT
				// and every assertion above — which all leave a stale digest —
				// misses it. This is the case that used to return nil.
				sealed := poked(nf.v)
				sealed.Digest = digestOf(p4offline.SerializeCommonFactset(sealed))
				if err := p4offline.VerifyCommonFactset(sealed); !errors.Is(err, p4offline.ErrFactsetInconsistent) {
					t.Fatalf("a self-consistent digest over %v = %v, want ErrFactsetInconsistent", nf.name, err)
				}

				// The control: the same poke with a finite value reaches the
				// digest gate, so the assertions above are about finiteness.
				if err := p4offline.VerifyCommonFactset(poked(7.5)); !errors.Is(err, p4offline.ErrFactsetDigest) {
					t.Fatalf("finite poke = %v, want ErrFactsetDigest", err)
				}
				// And the same value, re-sealed, verifies — so "sealed" above
				// refused for the value and not for the act of re-sealing.
				finite := poked(7.5)
				finite.Digest = digestOf(p4offline.SerializeCommonFactset(finite))
				if err := p4offline.VerifyCommonFactset(finite); err != nil {
					t.Fatalf("a re-sealed finite factset = %v, want nil", err)
				}
			})
		}
	}
}

// floatSite is one settable float64 inside a factset, with the path that
// reached it.
type floatSite struct {
	path string
	at   reflect.Value
}

// reachableFloats walks a factset VALUE by reflection and collects the float64
// positions it can reach through structs, pointers and slice elements. It
// derives them from the shape it is given rather than from any list this file
// maintains.
//
// WHAT IT DOES NOT REACH, stated because the test below would otherwise be
// read as a completeness guarantee it does not provide: there is no case for
// reflect.Map or reflect.Interface, so a float behind either is missed; an
// EMPTY slice contributes no position, so a float in a slice the fixture
// leaves empty is missed; and a `json:"-"` field would be collected although
// no encoder writes it. Today every type reachable from a CommonFactset is a
// plain struct, pointer or non-empty slice and no field is tagged "-", so the
// walk is exact HERE. A float added behind a map or an interface would not
// fail the test below, and the count assertion would not catch it either.
//
// It allocates a nil pointer as it goes so an optional branch is reachable.
// On the present fixture that branch never fires — Settings, FilterCondition,
// BetTotalUsers and BetTotalPoints are all non-nil — and if it ever did, the
// allocation would change serializeOptionalInt64's framing and so the digest,
// which would make the finite-value control below pass for the wrong reason.
func reachableFloats(v reflect.Value, path string, out *[]floatSite) {
	switch v.Kind() {
	case reflect.Float64, reflect.Float32:
		*out = append(*out, floatSite{path: path, at: v})
	case reflect.Pointer:
		if v.IsNil() {
			if !v.CanSet() {
				return
			}
			v.Set(reflect.New(v.Type().Elem()))
		}
		reachableFloats(v.Elem(), path+".*", out)
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			reachableFloats(v.Index(i), path+"["+strconv.Itoa(i)+"]", out)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).PkgPath != "" {
				continue // unexported: no encoder writes it and nothing can set it
			}
			reachableFloats(v.Field(i), path+"."+v.Type().Field(i).Name, out)
		}
	}
}

// TestEveryReachableFactsetFloatRefusesANonFiniteValue is the same guarantee
// stated independently of the implementation's list of fields.
//
// The test above names the five positions the canonical framing hands to
// canonical.f64. This one names none of them: it walks the fixture, sets each
// float64 it reaches to NaN in turn, and requires a refusal at each. A float
// added to the factset later — or to one of the native input types it embeds —
// fails here without anyone editing this file, PROVIDED it sits in a shape the
// walker reaches. See reachableFloats for the shapes it does not.
func TestEveryReachableFactsetFloatRefusesANonFiniteValue(t *testing.T) {
	// A POINTER, for the reason the string walk below states: every site here
	// happens to sit behind a pointer or a slice today, so a copy would work
	// by accident, and a float added as a direct field would silently stop
	// being poked.
	sites := func() (*p4offline.CommonFactset, []floatSite) {
		_, fs := selectedFactset(t, withFilter, nil)
		var out []floatSite
		reachableFloats(reflect.ValueOf(&fs).Elem(), "CommonFactset", &out)
		return &fs, out
	}
	_, found := sites()
	// Two outcomes carrying three ratios each, the settings delay and the
	// filter condition's value. This is a property of the FIXTURE — of
	// synthOutcomes returning two outcomes and of withFilter being applied —
	// not of the CommonFactset type, so changing the fixture's outcome count
	// fails here with a count mismatch rather than a coverage failure. It is
	// asserted so that a walker which silently reached nothing could not pass
	// the loop below; it does not backstop a float in a shape the walker skips.
	if want := 2*3 + 1 + 1; len(found) != want {
		var paths []string
		for _, s := range found {
			paths = append(paths, s.path)
		}
		t.Fatalf("reached %d float positions, want %d: %s", len(found), want, strings.Join(paths, ", "))
	}
	for i, site := range found {
		t.Run(site.path, func(t *testing.T) {
			fs, s := sites()
			s[i].at.SetFloat(math.NaN())
			if _, err := json.Marshal(*fs); err == nil {
				t.Fatalf("json.Marshal accepted a NaN at %s", site.path)
			}
			if err := p4offline.VerifyCommonFactset(*fs); !errors.Is(err, p4offline.ErrFactsetInconsistent) {
				t.Fatalf("VerifyCommonFactset at %s = %v, want ErrFactsetInconsistent", site.path, err)
			}
			fs2, s2 := sites()
			s2[i].at.SetFloat(11)
			if err := p4offline.VerifyCommonFactset(*fs2); !errors.Is(err, p4offline.ErrFactsetDigest) {
				t.Fatalf("finite value at %s = %v, want ErrFactsetDigest", site.path, err)
			}
		})
	}
}

// datasetWithEnvelope builds the fixture's episode with one value replaced in
// the recorded envelope. The replacement need not be non-finite: the control
// at the end of the test below uses it with a finite value to get a dataset
// that derives a DIFFERENT factset, which is the ordinary route to the same
// sentinel and is what keeps the assertions above from being satisfied by any
// dataset whatsoever.
func datasetWithEnvelope(t *testing.T, place func(*predictioneval.SourceDecisionEnvelope)) predictioneval.SourceDataset {
	t.Helper()
	s := newSynth()
	s.due("r1", "e1", 1)
	env := synthPlacedEnvelope()
	withFilter(env)
	place(env)
	s.terminal("r1", "e1", 1, predictioneval.PhaseAutoDecided, "OK", env)
	s.call("r1", "e1", 1, 50, 0)
	return s.dataset()
}

// TestADatasetThatDerivesNothingStillSaysSoAsNotDerived pins the seam the value
// gate could most easily have broken, and did break in its first form.
//
// derivedOpportunity verifies the CALLER's factset, then rebuilds from the
// DATASET and compares digests. Once the rebuild can fail on a value, the naive
// spelling returns the rebuild's own error — and that error talks about "this
// factset", while the caller's factset is fine and holds the fixture's own
// finite value — 1.66 for the odds, 6 for the delay — where the dataset holds
// the non-finite one. Two things go wrong at once: the message blames the wrong
// artifact, and errors.Is(err, ErrFactsetNotDerived) flips from true to false
// for an input class ErrFactsetNotDerived's own doc describes exactly.
//
// An independent review lane found this; no test in the package reached it,
// because every other case pins the contract with a FINITE dataset.
func TestADatasetThatDerivesNothingStillSaysSoAsNotDerived(t *testing.T) {
	_, good := selectedFactset(t, withFilter, nil)
	if err := p4offline.VerifyCommonFactset(good); err != nil {
		t.Fatalf("the caller's factset must be valid for this test to mean anything: %v", err)
	}
	for _, tc := range []struct {
		name  string
		place func(*predictioneval.SourceDecisionEnvelope)
	}{
		{"outcome odds", func(e *predictioneval.SourceDecisionEnvelope) { e.Outcomes[0].Odds = math.NaN() }},
		{"settings delay", func(e *predictioneval.SourceDecisionEnvelope) { e.Settings.Delay = math.Inf(1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := datasetWithEnvelope(t, tc.place)
			for _, call := range []struct {
				name string
				run  func() error
			}{
				{"ProjectFactualPlacement", func() error {
					_, err := p4offline.ProjectFactualPlacement(bad, good)
					return err
				}},
				{"ClaimSourceRound", func() error {
					_, err := p4offline.ClaimSourceRound(bad, good)
					return err
				}},
			} {
				err := call.run()
				if !errors.Is(err, p4offline.ErrFactsetNotDerived) {
					t.Fatalf("%s = %v, want ErrFactsetNotDerived", call.name, err)
				}
				// The cause stays READABLE...
				if !strings.Contains(err.Error(), "the dataset derives no factset") {
					t.Fatalf("%s dropped the cause: %v", call.name, err)
				}
				if !strings.Contains(err.Error(), "cannot express") {
					t.Fatalf("%s dropped the underlying reason: %v", call.name, err)
				}
				// ...and the cause's SENTINEL does not. ErrFactsetInconsistent
				// classifies a fault in a factset's own values; the caller's
				// factset verified at the top of this test, so a caller that
				// quarantines on that sentinel must not be told to quarantine
				// it. Prose cannot disambiguate a programmatic branch, which is
				// why this assertion is about errors.Is and not about text.
				if errors.Is(err, p4offline.ErrFactsetInconsistent) {
					t.Fatalf("%s exposes the caller-factset sentinel for a dataset fault: %v", call.name, err)
				}
			}
		})
	}
	// The control: with a FINITE dataset that simply derives a different
	// factset, the same seam still reports ErrFactsetNotDerived — so the
	// assertions above are not satisfied by every dataset whatsoever.
	other := datasetWithEnvelope(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Outcomes[0].Odds = 9.5 })
	if _, err := p4offline.ProjectFactualPlacement(other, good); !errors.Is(err, p4offline.ErrFactsetNotDerived) {
		t.Fatalf("a merely different finite dataset = %v, want ErrFactsetNotDerived", err)
	}
}

// TestTheValueGateAddsNoPerOutcomeAllocation pins the WORK half of
// canonical.go's rule at the one place this change could break it.
//
// The value gate walks the caller's outcome slice ahead of the digest gate.
// fs.Outcomes is an exported field nothing bounds, so a string built once per
// outcome and read only on the refusing branch is an allocation per element
// that every non-refusing element throws away. The first form of this gate did
// exactly that, and two independent review lanes measured it at 2.00x this
// function's allocation count.
//
// The guarantee is stated as a SLOPE rather than as an absolute count, because
// an absolute count would pin the canonical framing's own allocations too and
// would move for reasons that have nothing to do with this gate. Doubling the
// outcomes must add about one allocation per added outcome — the framing's —
// and not two.
func TestTheValueGateAddsNoPerOutcomeAllocation(t *testing.T) {
	refusedWith := func(n int) float64 {
		_, fs := selectedFactset(t, withFilter, nil)
		outs := make([]predictioneval.OutcomeInput, n)
		for i := range outs {
			outs[i] = predictioneval.OutcomeInput{Slot: i, Present: true, ID: "o",
				PercentageUsers: 1, Odds: 2, OddsPercentage: 3}
		}
		fs.Outcomes = outs
		fs.Digest = strings.Repeat("0", 64) // refused by the digest gate
		return testing.AllocsPerRun(3, func() { _ = p4offline.VerifyCommonFactset(fs) })
	}
	const n = 20000
	one, two := refusedWith(n), refusedWith(2*n)
	perOutcome := (two - one) / float64(n)
	t.Logf("allocs: %.0f at %d outcomes, %.0f at %d — %.2f per added outcome",
		one, n, two, 2*n, perOutcome)
	if perOutcome > 1.5 {
		t.Fatalf("the value gate allocates %.2f per outcome on the refusing path; "+
			"the framing alone is about 1, so a label is being built and discarded", perOutcome)
	}
}

// reachableStrings is reachableFloats for strings, with the same scope and the
// same blind spots — see that function's comment, which applies verbatim.
func reachableStrings(v reflect.Value, path string, out *[]floatSite) {
	switch v.Kind() {
	case reflect.String:
		*out = append(*out, floatSite{path: path, at: v})
	case reflect.Pointer:
		if v.IsNil() {
			if !v.CanSet() {
				return
			}
			v.Set(reflect.New(v.Type().Elem()))
		}
		reachableStrings(v.Elem(), path+".*", out)
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			reachableStrings(v.Index(i), path+"["+strconv.Itoa(i)+"]", out)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).PkgPath != "" {
				continue
			}
			reachableStrings(v.Field(i), path+"."+v.Type().Field(i).Name, out)
		}
	}
}

// TestEveryReachableFactsetStringRefusesInvalidUTF8 pins the SECOND half of the
// value gate, and it is the half that was recorded as a known limitation for a
// round before an external lane ranked it P1.
//
// The failure is quieter than the non-finite one and therefore worse. A NaN is
// refused by encoding/json at the point of marshalling, so whoever is storing
// the artifact learns immediately. An invalid UTF-8 byte marshals WITHOUT
// error: Go substitutes U+FFFD, the stored artifact is a different value from
// the one that was certified, and it fails its OWN digest when it is read back
// — which is the single signal this package reserves for tampering. The
// package's own coverage_test.go round-trips a factset through JSON, so that
// is a supported path here and not a hypothetical one.
//
// Each position is poked with the digest RESEALED over the poked value, which
// is the artifact the finding describes: one this package would certify. The
// control per position is a VALID multi-byte string that already contains
// U+FFFD, which must not be refused for its encoding — otherwise this gate
// would be refusing the very substitution it exists to prevent.
func TestEveryReachableFactsetStringRefusesInvalidUTF8(t *testing.T) {
	const invalid = "\xff\xfe\x80"
	const validWide = "ok-\u00e9-\uFFFD"
	const utf8Fault = "is not valid UTF-8"

	sealed := func(fs p4offline.CommonFactset) p4offline.CommonFactset {
		fs.Digest = digestOf(p4offline.SerializeCommonFactset(fs))
		return fs
	}
	// A POINTER, not a copy. The walker's sites point into the factset it was
	// handed, so returning it by value would hide the mutation of any field
	// that is not itself behind a pointer or a slice — which is most of them
	// here, and is exactly how the first draft of this test passed a position
	// it had not actually poked.
	sites := func() (*p4offline.CommonFactset, []floatSite) {
		_, fs := selectedFactset(t, withFilter, nil)
		var out []floatSite
		reachableStrings(reflect.ValueOf(&fs).Elem(), "CommonFactset", &out)
		return &fs, out
	}
	_, found := sites()
	if len(found) < 12 {
		t.Fatalf("reached only %d string positions; the walk is not covering the factset", len(found))
	}
	for i, site := range found {
		if site.path == "CommonFactset.Digest" {
			continue // the seal itself, not a framed value
		}
		t.Run(site.path, func(t *testing.T) {
			fs, s := sites()
			s[i].at.SetString(invalid)
			err := p4offline.VerifyCommonFactset(sealed(*fs))
			if err == nil {
				t.Fatalf("invalid UTF-8 at %s was CERTIFIED", site.path)
			}
			// The two contract identifiers are refused by the gates that are
			// documented as the first on every factset path — they are held to
			// exact constants, so an invalid byte cannot survive them either
			// way. Every other position must be refused BY THE ENCODING, and
			// naming that difference is what keeps this assertion honest: a
			// blanket "some error" would pass even if the value gate did
			// nothing at all.
			if site.path == "CommonFactset.ContractVersion" || site.path == "CommonFactset.Protocol" {
				if !errors.Is(err, p4offline.ErrFactsetDigest) {
					t.Fatalf("%s = %v, want the contract gate", site.path, err)
				}
				return
			}
			if !strings.Contains(err.Error(), utf8Fault) {
				t.Fatalf("invalid UTF-8 at %s = %v, want a %q refusal", site.path, err, utf8Fault)
			}
			if !errors.Is(err, p4offline.ErrFactsetInconsistent) {
				t.Fatalf("invalid UTF-8 at %s did not carry ErrFactsetInconsistent: %v", site.path, err)
			}
			// The control: a valid multi-byte string, U+FFFD included, is
			// never refused FOR ITS ENCODING. It may still be refused by a
			// closed vocabulary, which is a different gate and a different
			// message.
			fs2, s2 := sites()
			s2[i].at.SetString(validWide)
			if err := p4offline.VerifyCommonFactset(sealed(*fs2)); err != nil &&
				strings.Contains(err.Error(), utf8Fault) {
				t.Fatalf("valid UTF-8 at %s was refused for its encoding: %v", site.path, err)
			}
		})
	}
	// IncompleteReasons is empty on this fixture, so the walk above never
	// reaches it. It is framed, so it is checked, and it is poked here by hand
	// rather than left to a walker that cannot see an empty slice.
	t.Run("IncompleteReasons element", func(t *testing.T) {
		_, fs := selectedFactset(t, withFilter, nil)
		fs.IncompleteReasons = []string{invalid}
		err := p4offline.VerifyCommonFactset(sealed(fs))
		if err == nil || !strings.Contains(err.Error(), utf8Fault) {
			t.Fatalf("invalid UTF-8 reason = %v, want a %q refusal", err, utf8Fault)
		}
	})
	// And the dataset path refuses it too, not only a supplied factset.
	t.Run("the dataset path", func(t *testing.T) {
		ds := datasetWithEnvelope(t, func(e *predictioneval.SourceDecisionEnvelope) {
			e.Outcomes[0].ID = invalid
		})
		ep := singleEpisode(t, mustSelect(t, ds))
		_, err := p4offline.BuildCommonFactset(ds, ep.Episode)
		if err == nil || !strings.Contains(err.Error(), utf8Fault) {
			t.Fatalf("BuildCommonFactset = %v, want a %q refusal", err, utf8Fault)
		}
	})
}

// TestAConstantSizePositionIsRefusedBeforeACallerSizedSlice pins the check
// ORDER inside the value gate, which is a resource property stated as a
// deterministic one.
//
// fs.Outcomes and fs.IncompleteReasons are slices the CALLER sizes, and nothing
// bounds them before the digest gate. If the gate scanned them before checking
// the constant-size positions, a supplier could pair one bad settings value
// with a huge finite outcome slice and turn a constant-time refusal into work
// proportional to the whole slice — all of it discarded. An external review
// lane raised exactly that.
//
// Asserting it by TIME would be flaky, so it is asserted by WHICH FAULT WINS:
// a factset bad in both places must report the constant-size one, which it can
// only do by looking there first. The controls make each fault on its own
// reachable, so the test cannot pass because the slice fault is simply never
// detected.
func TestAConstantSizePositionIsRefusedBeforeACallerSizedSlice(t *testing.T) {
	const invalid = "\xff\xfe\x80"
	both := func(mutate func(*p4offline.CommonFactset)) p4offline.CommonFactset {
		_, fs := selectedFactset(t, withFilter, nil)
		outs := make([]predictioneval.OutcomeInput, 4096)
		for i := range outs {
			outs[i] = predictioneval.OutcomeInput{Slot: i, Present: true, ID: "o",
				PercentageUsers: 1, Odds: 2, OddsPercentage: 3}
		}
		fs.Outcomes = outs
		mutate(&fs)
		fs.Digest = digestOf(p4offline.SerializeCommonFactset(fs))
		return fs
	}
	says := func(fs p4offline.CommonFactset) string {
		err := p4offline.VerifyCommonFactset(fs)
		if err == nil {
			t.Fatalf("expected a refusal")
		}
		return err.Error()
	}

	// The slice fault alone IS detected — otherwise the assertions below would
	// pass for the wrong reason.
	if got := says(both(func(fs *p4offline.CommonFactset) {
		fs.Outcomes[4095].ID = invalid
	})); !strings.Contains(got, "outcome 4095 id") {
		t.Fatalf("the slice fault alone = %q, want it named", got)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*p4offline.CommonFactset)
		want   string
	}{
		{"settings delay beats a late outcome id", func(fs *p4offline.CommonFactset) {
			fs.Settings.Delay = math.NaN()
			fs.Outcomes[4095].ID = invalid
		}, "settings delay"},
		{"health reason beats a late outcome odds", func(fs *p4offline.CommonFactset) {
			fs.HealthReason = invalid
			fs.Outcomes[4095].Odds = math.Inf(1)
		}, "health reason"},
		{"filter value beats a late outcome id", func(fs *p4offline.CommonFactset) {
			fs.Settings.FilterCondition.Value = math.Inf(-1)
			fs.Outcomes[4095].ID = invalid
		}, "settings filter condition value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := says(both(tc.mutate)); !strings.Contains(got, tc.want) {
				t.Fatalf("= %q, want the constant-size fault %q first", got, tc.want)
			}
		})
	}
}
