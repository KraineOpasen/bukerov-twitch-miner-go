package p4offline_test

// The frozen protocol pins, against the contract's OWN figures. Every literal
// below is transcribed from the owner's task contract (D01–D16), not from
// protocol.go, so a silent redesign of a pin is caught by name.

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
		{"placement contract", p4offline.PlacementEvidenceVersion, "p4-placement-evidence-only/v1"},
		{"payout contract", p4offline.PayoutEvidenceVersion, "p4-payout-evidence-only/v1"},
		{"resolution obligations are named and provisional", p.ResolutionObligations != "", true},
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
