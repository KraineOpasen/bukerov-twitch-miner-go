package p4offline_test

// SEAMS 4–5: the outcome-free common factset and the exact P2 per-case
// configuration binding, with the P2 plumbing they feed.

import (
	"bytes"
	"errors"
	"math"
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
// because this defect has now been found on this branch FIVE times: at
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
// So this is a table over every gate that compares a caller-supplied string
// that no earlier gate has length-bounded, wherever it sits. A new gate of that
// shape belongs here on the day it is written.
//
// EACH ROW ASSERTS THE RENDERED EXTENT, not merely that the error is small. A
// first version asserted only the sentinel, a 1024-byte budget and the absence
// of the input -- and a gate reporting the WRONG extent (this package's own
// constant instead of the caller's string) satisfied all three. Three of five
// rows survived a field swap. The extent is the property the repair exists for,
// so it is the property that is checked.
//
// FIVE SITES OF THE CLASS ARE PINNED ELSEWHERE and are named here rather than
// duplicated, so this registry reports its whole inventory:
//
//	VerifyP3bRuleset's identity gate  TestRulesetIdentityRefusalDoesNotMaterializeSuppliedText
//	ValidateDrawTrace's two gates     TestDrawTraceRefusalDoesNotMaterializeSuppliedText
//	bindEntropyCoordinates' round     TestEntropyBindingRefusalDoesNotMaterializeTheFactsetRound
//	Decision's derivation thunk       TestDecisionDoesNotBuildItsDerivationBeforeItsGates
//
// With the nine rows below that is fourteen sites. canonical.go carries the
// same inventory beside the rule.
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
		// a compile-time constant but is its own 64 hex characters.
		if e3 == nil || !strings.Contains(e3.Error(), "which digest to \"") {
			t.Fatalf("the digest fault must name the digest the values produce: %v", e3)
		}
		if e1.Error() == e2.Error() {
			t.Fatalf("the two faults must be distinguishable: %v / %v", e1, e2)
		}
	})
}
