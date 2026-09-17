package p4offline_test

// The frozen protocol pins. The table below states every pinned value a SECOND
// time, independently of protocol.go, so that a one-sided edit is caught by
// name rather than silently shipped.
//
// The literals are of two kinds, and conflating them is how one of them came to
// be pinned by nothing at all:
//
//   - The CONTRACT FIGURES are transcribed from the owner's task contract
//     (D01–D16): the policies, COMMON_CUTOFF, CALCULATE_INPUT_SINGLE_CANDIDATE,
//     FACTUAL_STEALTH_OFF, NO_STEALTH_SIGNAL, POLICY_CHOICE_ACCURACY, the eight
//     weeks, the 200 rounds, the 16,384 trajectories, the 0.1 pp ceiling,
//     DESCRIPTIVE_ONLY, MANUAL_EPISODES_EXCLUDED, COMPLETE, AS_FINALIZED,
//     EVIDENCE_ONLY, OUT_OF_SCOPE and HOLD. For these the contract is the
//     oracle and the code is what is being checked.
//
//   - The CONTRACT IDENTITIES are this package's own versioned domain-separation
//     strings. No external document fixes their spelling; what fixes them is
//     that changing one has to be a deliberate, visible act in two places. Four
//     of them are additionally pinned by the independent Python goldens in
//     testdata/synthetic, which digest the artifacts they separate.
//
// P3bProjectionManifestID was in neither list, and the one place it appeared in
// a test compared the constant to itself through the same symbol -- an
// assertion that cannot fail for the value it appears to guard. Replacing it
// with "MUTANT-p4-projection/v9" left the whole package suite green. It is
// pinned below with its siblings.

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

func TestFrozenProtocolMatchesTheContract(t *testing.T) {
	p := p4offline.FrozenProtocol()
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"policies", len(p.Policies) == 2 && p.Policies[0] == "P2" && p.Policies[1] == "P3B", true},
		{"boundary rule", p.BoundaryRule, "COMMON_CUTOFF"},
		{"P3b projection mode", p.P3bProjectionMode, "CALCULATE_INPUT_SINGLE_CANDIDATE"},
		{"stealth rule", p.StealthRule, "FACTUAL_STEALTH_OFF"},
		{"entropy signal rule", p.EntropySignalRule, "NO_STEALTH_SIGNAL"},
		{"primary metric", p.PrimaryMetric, "POLICY_CHOICE_ACCURACY"},
		{"complete UTC weeks", p.RequiredCompleteUTCWeeks, 8},
		{"minimum primary-scorable unique rounds", p.MinimumPrimaryScorableRounds, 200},
		{"trajectories", p.TrajectoryCount, 16384},
		{"MCSE ceiling in basis points (0.1 pp)", p.MCSEToleranceBasisPoints, 10},
		{"evidence mode", p.EvidenceMode, "DESCRIPTIVE_ONLY"},
		{"manual episodes", p.ManualEpisodes, "MANUAL_EPISODES_EXCLUDED"},
		{"admitted close state", p.AdmittedSessionCloseState, "COMPLETE"},
		{"admitted reading", p.AdmittedSessionReading, "AS_FINALIZED"},
		{"placement/payout mode", p.PlacementPayoutMode, "EVIDENCE_ONLY"},
		{"bankroll/path metrics", p.BankrollPathMetrics, "OUT_OF_SCOPE"},
		{"faithful full policy", p.FaithfulFullPolicy, "HOLD"},
		{"entropy algorithm", p.EntropyAlgorithm, "p4-hmac-sha256-u64/v1"},
		{"protocol", p.Protocol, "p4-offline/v1"},
		{"action map", p4offline.NativeActionMapVersion, "p4-native-action-map/v1"},
		{"P3b projection manifest", p4offline.P3bProjectionManifestID, "p4-calculate-input-single-candidate/v1"},
		{"common factset digest", p4offline.CommonFactsetDigestVersion, "p4-common-factset/v1"},
		{"P2 config binding", p4offline.P2ConfigBindingVersion, "p4-p2-config-binding/v1"},
		{"resolution facts digest", p4offline.ResolutionFactsDigestVersion, "p4-resolution-facts/v1"},
		{"source round registry", p4offline.SourceRoundRegistryVersion, "p4-source-round-registry/v1"},
		{"entropy algorithm identity", p4offline.EntropyAlgorithmVersion, "p4-hmac-sha256-u64/v1"},
		{"placement contract", p4offline.PlacementEvidenceVersion, "p4-placement-evidence-only/v1"},
		{"payout contract", p4offline.PayoutEvidenceVersion, "p4-payout-evidence-only/v1"},
		{"resolution obligations (provisional label, changed only by an owner decision)", p.ResolutionObligations, "p4offline-resolution-obligations/provisional-v1"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
	// The pins are a fresh value each call: a caller cannot alter them.
	p.Policies[0] = "P9"
	if p4offline.FrozenProtocol().Policies[0] != "P2" {
		t.Fatal("the frozen protocol was altered through a returned value")
	}
}
