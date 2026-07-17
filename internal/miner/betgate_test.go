package miner

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// healthSig is a minimal named health signal for the adapter tests.
func healthSig(name, status string) health.Signal {
	return health.Signal{Name: name, Status: status}
}

// gateDecisionFor builds the miner-side health gate over a REAL health.Center
// seeded with the given signals and returns its auto-bet decision. Using the
// real Center (not a nil interface or a stub) is deliberate: the "unknown"
// fail-open path must be exercised through the same snapshot machinery
// production uses.
func gateDecisionFor(signals ...health.Signal) (bool, models.GateReason) {
	center := health.NewCenter()
	for _, s := range signals {
		center.Record(s)
	}
	m := &Miner{healthCenter: center}
	d := minerBetHealthGate{m}.AutoBetDecision()
	return d.Allowed, d.Reason
}

// TestMinerBetHealthGate covers the BLOCKER-3 scope: gql_api / pubsub
// degraded|failed block with a stable, signal-specific reason (GQL outranks
// PubSub, failed outranks degraded); everything else — OK, idle, stalled,
// unknown, or a signal not yet recorded — fails open.
func TestMinerBetHealthGate(t *testing.T) {
	cases := []struct {
		name        string
		signals     []health.Signal
		wantAllowed bool
		wantReason  models.GateReason
	}{
		{"gql_failed_blocks",
			[]health.Signal{healthSig(health.SignalGQLAPI, health.StatusFailed)},
			false, models.GateHealthGQLFailed},
		{"gql_degraded_blocks",
			[]health.Signal{healthSig(health.SignalGQLAPI, health.StatusDegraded)},
			false, models.GateHealthGQLDegraded},
		{"pubsub_failed_blocks",
			[]health.Signal{healthSig(health.SignalPubSub, health.StatusFailed)},
			false, models.GateHealthPubSubFailed},
		{"pubsub_degraded_blocks",
			[]health.Signal{healthSig(health.SignalPubSub, health.StatusDegraded)},
			false, models.GateHealthPubSubDegraded},
		{"both_ok_allows",
			[]health.Signal{healthSig(health.SignalGQLAPI, health.StatusOK), healthSig(health.SignalPubSub, health.StatusOK)},
			true, models.GateNone},
		// GQL outranks PubSub; a GQL failure names the GQL reason even when PubSub
		// is also impaired.
		{"gql_failed_outranks_pubsub_degraded",
			[]health.Signal{healthSig(health.SignalGQLAPI, health.StatusFailed), healthSig(health.SignalPubSub, health.StatusDegraded)},
			false, models.GateHealthGQLFailed},
		// StatusStalled is out of scope (degraded/failed only): it must NOT block.
		{"pubsub_stalled_allows",
			[]health.Signal{healthSig(health.SignalPubSub, health.StatusStalled)},
			true, models.GateNone},
		{"idle_allows",
			[]health.Signal{healthSig(health.SignalGQLAPI, health.StatusIdle), healthSig(health.SignalPubSub, health.StatusIdle)},
			true, models.GateNone},
		// No signals recorded yet: unknown -> fail-open, via the REAL Center.
		{"unknown_fails_open", nil, true, models.GateNone},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			allowed, reason := gateDecisionFor(c.signals...)
			if allowed != c.wantAllowed || reason != c.wantReason {
				t.Errorf("AutoBetDecision() = (allowed=%v, reason=%q), want (%v, %q)",
					allowed, reason, c.wantAllowed, c.wantReason)
			}
		})
	}
}

// TestMinerBetHealthGateNilCenterFailsOpen: a miner without a health center
// (early startup / health disabled) fails open.
func TestMinerBetHealthGateNilCenterFailsOpen(t *testing.T) {
	m := &Miner{}
	d := minerBetHealthGate{m}.AutoBetDecision()
	if !d.Allowed || d.Reason != models.GateNone {
		t.Errorf("nil health center must fail open, got %+v", d)
	}
}
