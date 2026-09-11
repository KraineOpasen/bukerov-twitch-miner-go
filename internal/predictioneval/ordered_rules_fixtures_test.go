package predictioneval_test

// Shared fixtures for the ordered-rules acceptance suite.
//
// The vectors below are the ones the readiness audit named, built from the
// concrete fixture it specified: a pool of [A:4, B:6] whose first outcome's
// donor share is exactly the double nearest 0.4, a participation rate of one
// half whose Bernoulli threshold is exactly 2^63, and two raw words chosen so
// that one admits and the other refuses at that threshold.
//
// Every probability here is binary-exact where it can be. A rate of one half
// maps to 2^63 with no rounding at all, so "the word below the threshold
// admits and the word AT the threshold does not" is a statement about the
// comparison and not about floating-point luck.

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

const (
	// orWordAdmit is below every non-zero threshold: the smallest word there is.
	orWordAdmit = uint64(0x0000000000000000)
	// orWordRefuse is exactly 2^63 — the threshold a rate of one half produces.
	// Under the donor's STRICT `raw < threshold` it refuses; under a `<=` it
	// would admit, which is what TestOrderedRulesRawThresholdStrictLess pins.
	orWordRefuse = uint64(0x8000000000000000)
)

func orOutcome(id string, points int64) predictioneval.OrderedRulesOutcome {
	return predictioneval.OrderedRulesOutcome{
		Identity: id,
		Points: predictioneval.SuppliedInt64{
			Presence:   predictioneval.SuppliedKnown,
			Value:      points,
			Provenance: "acceptance fixture",
			// Declared, not left to the zero value: the fixture interval starts
			// at position 0, so these really are available from the start — but
			// the model may not infer that from an unset field, and every
			// fixture here says so explicitly for that reason.
			HasAvailableAtPosition: true,
		},
	}
}

// orMissingPoints is an outcome whose points were NOT supplied. It is the
// shape a wire projection leaves behind when it cannot distinguish an absent
// scalar from a zero one, and the model must refuse to read it as a zero.
func orMissingPoints(id string) predictioneval.OrderedRulesOutcome {
	return predictioneval.OrderedRulesOutcome{
		Identity: id,
		Points: predictioneval.SuppliedInt64{
			Presence: predictioneval.SuppliedMissing,
			Reason:   "wire projection cannot distinguish absent from zero",
		},
	}
}

func orMissingBalance() predictioneval.SuppliedInt64 {
	return predictioneval.SuppliedInt64{
		Presence: predictioneval.SuppliedMissing,
		Reason:   "channel candidate carries no balance",
	}
}

func orKnownBalance(v int64) predictioneval.SuppliedInt64 {
	return predictioneval.SuppliedInt64{
		Presence:               predictioneval.SuppliedKnown,
		Value:                  v,
		Provenance:             "acceptance fixture",
		HasAvailableAtPosition: true,
	}
}

func orCandidate(id string, pos int64, balance predictioneval.SuppliedInt64,
	outs ...predictioneval.OrderedRulesOutcome) predictioneval.OrderedRulesCandidate {
	return predictioneval.OrderedRulesCandidate{
		Identity: id,
		Position: pos,
		// Declared, not left to the zero value: every fixture says explicitly
		// that its causal position is supplied, because an omitted one is now a
		// refusal and this suite is about what the model does ADMIT.
		HasPosition:       true,
		SourceKind:        predictioneval.SourceKindChannelUpdate,
		EpisodeMembership: predictioneval.MembershipProven,
		OutcomesPresence:  predictioneval.SuppliedKnown,
		Outcomes:          outs,
		Balance:           balance,
		Provenance:        "acceptance fixture",
	}
}

func orScope() predictioneval.OrderedRulesScope {
	return predictioneval.OrderedRulesScope{
		Namespace:             "acceptance-namespace",
		EpisodeID:             "episode-1",
		AccountContext:        "acceptance-account-context",
		AssociationEvidence:   "fixture declares one account context, one channel and one episode for every candidate and call",
		SourceContractVersion: predictioneval.OrderedRulesStreamContractVersion,
		Coverage:              predictioneval.CoverageCompleteDeclared,
		IntervalFromPosition:  0,
		IntervalToPosition:    10000,
		HasInterval:           true,
	}
}

func orAdmission() predictioneval.CommonAdmission {
	return predictioneval.CommonAdmission{
		ManifestID: "acceptance-manifest-1",
		ViewKind:   predictioneval.ViewChannelCandidateStream,
		Population: "the fixture's declared channel candidates",
		OrderBasis: "supplied causal position, ascending",
	}
}

func orDraws(words ...uint64) predictioneval.SuppliedDrawTrace {
	return predictioneval.SuppliedDrawTrace{
		RunID:                   "acceptance-run-1",
		EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion,
		Words:                   words,
	}
}

func orSource(cs []predictioneval.OrderedRulesCandidate,
	ins []predictioneval.OrderedRulesIntervention) predictioneval.OrderedRulesSource {
	return predictioneval.OrderedRulesSource{Scope: orScope(), Candidates: cs, Interventions: ins}
}

func orProject(t *testing.T, cs []predictioneval.OrderedRulesCandidate,
	ins []predictioneval.OrderedRulesIntervention) predictioneval.OrderedRulesStream {
	t.Helper()
	s, err := predictioneval.ProjectOrderedRulesStream(orSource(cs, ins), orAdmission())
	if err != nil {
		t.Fatalf("projecting the fixture source failed: %v", err)
	}
	return s
}

func orRule(cmp predictioneval.OrderedRuleComparator, threshold, rate float64,
	maxValue uint32, percent float64) predictioneval.OrderedRule {
	return predictioneval.OrderedRule{
		Comparator:            cmp,
		RawThresholdPercent:   threshold,
		RawAttemptRatePercent: rate,
		Points:                predictioneval.OrderedRulesPoints{MaxValue: maxValue, RawPercent: percent},
	}
}

// orConfig builds a fully resolved RAW config. Every percentage here is a raw
// 0..100 value, exactly as the exported contract requires.
func orConfig(detailed []predictioneval.OrderedRule,
	defMin, defMax float64, defMaxValue uint32, defPercent float64) predictioneval.OrderedRulesConfig {
	return predictioneval.OrderedRulesConfig{
		ConfigID:   "acceptance-config-1",
		Detailed:   detailed,
		HasDefault: true,
		Default: predictioneval.OrderedRulesDefault{
			RawMinPercent: defMin,
			RawMaxPercent: defMax,
			Points:        predictioneval.OrderedRulesPoints{MaxValue: defMaxValue, RawPercent: defPercent},
		},
	}
}

// orEval projects and evaluates in one step for the many tests that only care
// about the mechanism.
func orEval(t *testing.T, cs []predictioneval.OrderedRulesCandidate,
	cfg predictioneval.OrderedRulesConfig, words ...uint64) predictioneval.OrderedRulesEvaluation {
	t.Helper()
	return predictioneval.EvaluateOrderedRules(orProject(t, cs, nil), cfg, orDraws(words...))
}

func orMustAdmit(t *testing.T, ev predictioneval.OrderedRulesEvaluation) *predictioneval.OrderedRulesSelection {
	t.Helper()
	if ev.Selected == nil {
		t.Fatalf("expected an admission, got status %q reason %q", ev.Status, ev.Reason)
	}
	return ev.Selected
}
