package pubsub

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// fakeBetGate is a test double for the injected transport-health gate. It
// returns a fixed BetHealthDecision.
type fakeBetGate struct{ d BetHealthDecision }

func (g fakeBetGate) AutoBetDecision() BetHealthDecision { return g.d }

// autoBetSettings yields a bet-settings value that produces a deterministic
// auto-bet: with StrategyNumber1 the bot always picks outcome 0, and the given
// percentage of the balance is the proposed stake (MaxPoints high enough not to
// bind). The GLOBAL risk gates (percent / reserve / health switch) live on the
// pool, not here — set them via pool.SetRiskSettings.
func autoBetSettings(percentage int) models.BetSettings {
	return models.BetSettings{
		Strategy:   models.StrategyNumber1,
		Percentage: percentage,
		MaxPoints:  50000,
	}
}

// blockingHealth is a decision that blocks the auto-bet.
var blockingHealth = BetHealthDecision{Allowed: false, Reason: models.GateHealthGQLFailed}

// TestAutoBetHealthGateOnBlocksWhenUnhealthy — health switch #1: switch ON and
// the link unhealthy blocks the auto-bet entirely (no placement).
func TestAutoBetHealthGateOnBlocksWhenUnhealthy(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	pool.SetBetHealthGate(fakeBetGate{d: blockingHealth})
	pool.SetRiskSettings(config.PredictionRiskSettings{HealthGateEnabled: true})

	ep := addRound(pool, newTestStreamer(10000), "e1")
	ep.Bet.Settings = autoBetSettings(5)

	pool.placeAutoBet("e1")

	if placer.callCount() != 0 {
		t.Fatalf("gate ON + unhealthy must block the auto-bet; placer called %d times", placer.callCount())
	}
	if ep.BetPlaced {
		t.Error("no bet should be placed when the health gate blocks")
	}
}

// TestAutoBetHealthGateOffAllowsWhenUnhealthy — health switch #2: with the switch
// OFF the auto-bet proceeds even when the link is unhealthy, at the full stake.
func TestAutoBetHealthGateOffAllowsWhenUnhealthy(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	pool.SetBetHealthGate(fakeBetGate{d: blockingHealth})
	pool.SetRiskSettings(config.PredictionRiskSettings{HealthGateEnabled: false})

	ep := addRound(pool, newTestStreamer(10000), "e1")
	ep.Bet.Settings = autoBetSettings(5)

	pool.placeAutoBet("e1")

	if placer.callCount() != 1 {
		t.Fatalf("gate OFF must let the auto-bet through even when unhealthy; placer called %d times", placer.callCount())
	}
	if placer.lastAmt != 500 {
		t.Errorf("stake = %d, want 500", placer.lastAmt)
	}
}

// TestAutoBetHealthGateOnAllowsWhenHealthy: switch ON but a healthy link lets the
// bet through unchanged.
func TestAutoBetHealthGateOnAllowsWhenHealthy(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	pool.SetBetHealthGate(fakeBetGate{d: BetHealthDecision{Allowed: true}})
	pool.SetRiskSettings(config.PredictionRiskSettings{HealthGateEnabled: true})

	ep := addRound(pool, newTestStreamer(10000), "e1")
	ep.Bet.Settings = autoBetSettings(5)

	pool.placeAutoBet("e1")

	if placer.callCount() != 1 {
		t.Fatalf("healthy link must allow the auto-bet; placer called %d times", placer.callCount())
	}
}

// TestAutoBetHealthGateNilFailsOpen: with the switch ON but no gate injected
// (unknown health), the auto-bet fails open.
func TestAutoBetHealthGateNilFailsOpen(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer) // no SetBetHealthGate -> nil -> unknown -> fail-open
	pool.SetRiskSettings(config.PredictionRiskSettings{HealthGateEnabled: true})

	ep := addRound(pool, newTestStreamer(10000), "e1")
	ep.Bet.Settings = autoBetSettings(5)

	pool.placeAutoBet("e1")

	if placer.callCount() != 1 {
		t.Fatalf("nil gate (unknown health) must fail open; placer called %d times", placer.callCount())
	}
}

// TestAutoBetReserveViolationSkips is the BLOCKER-1 regression: the reserve is a
// FLOOR, not a cap. Percent is off; the strategy proposes 5000 (50% of 10000),
// and a reserve of 8000 means placing it would leave 5000 < 8000, so the bet is
// SKIPPED entirely — the shrunk 2000 stake must never reach Twitch.
func TestAutoBetReserveViolationSkips(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	pool.SetRiskSettings(config.PredictionRiskSettings{ReservePoints: 8000, HealthGateEnabled: false})

	ep := addRound(pool, newTestStreamer(10000), "e1")
	ep.Bet.Settings = autoBetSettings(50)

	pool.placeAutoBet("e1")

	if placer.callCount() != 0 {
		t.Fatalf("reserve violation must skip the bet entirely; placer called %d times", placer.callCount())
	}
	if placer.lastAmt != 0 {
		t.Errorf("no stake should reach Twitch on a reserve violation, got lastAmt=%d", placer.lastAmt)
	}
	if ep.BetPlaced {
		t.Error("no bet should be marked placed on a reserve violation")
	}
}

// TestAutoBetReserveSlackAllows: when the (percent-capped) stake still leaves the
// balance at or above the reserve, the bet proceeds normally.
func TestAutoBetReserveSlackAllows(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	// 5% of 10000 = 500; reserve 2000 leaves 9500 >= 2000 -> allowed.
	pool.SetRiskSettings(config.PredictionRiskSettings{ReservePoints: 2000, HealthGateEnabled: false})

	ep := addRound(pool, newTestStreamer(10000), "e1")
	ep.Bet.Settings = autoBetSettings(5)

	pool.placeAutoBet("e1")

	if placer.callCount() != 1 {
		t.Fatalf("reserve slack must allow the bet; placer called %d times", placer.callCount())
	}
	if placer.lastAmt != 500 {
		t.Errorf("stake = %d, want 500 (unshrunk)", placer.lastAmt)
	}
}

// TestAutoBetClampedStakeReachesDecisionAndROI is the BLOCKER-2 regression: the
// FINAL (percent-clamped) stake must become the round's decision, so it flows to
// the placer AND to the settled BetResult.Placed read by ROI analytics — not
// Calculate's pre-gate proposal. Fails on HEAD, where the clamp only touched a
// local copy and Decision.Amount stayed 5000.
func TestAutoBetClampedStakeReachesDecisionAndROI(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	// 50% of 10000 = 5000 proposed; percent gate 10% -> capped to 1000.
	pool.SetRiskSettings(config.PredictionRiskSettings{MaxStakePercent: 10, HealthGateEnabled: false})

	s := newTestStreamer(10000)
	ep := addRound(pool, s, "e1")
	ep.Bet.Settings = autoBetSettings(50)

	var got BetResult
	var fired int
	pool.SetBetResultHandler(func(r BetResult) { got = r; fired++ })

	pool.placeAutoBet("e1")

	if placer.callCount() != 1 {
		t.Fatalf("expected one bet, got %d", placer.callCount())
	}
	if placer.lastAmt != 1000 {
		t.Errorf("placer stake = %d, want 1000 (percent-clamped)", placer.lastAmt)
	}
	// The clamped stake must be written back as the round's decision.
	if ep.Bet.Decision.Amount != 1000 {
		t.Errorf("Decision.Amount = %d, want 1000 (clamped stake written back, not pre-gate 5000)", ep.Bet.Decision.Amount)
	}

	// Drive the settled-bet path: BetResult.Placed reads Decision.Amount, so it
	// must report the actually-placed 1000, not 5000.
	ep.BetConfirmed = true
	pool.handlePredictionUser(resultMsg("e1", "WIN", 3000), s)
	if fired != 1 {
		t.Fatalf("bet result handler fired %d times, want 1", fired)
	}
	if got.Placed != 1000 {
		t.Errorf("settled BetResult.Placed = %d, want 1000 (clamped stake reaches ROI)", got.Placed)
	}
}

// TestManualBetIgnoresRiskGates — mandatory #9: the manual path is never gated.
// Even with an unhealthy link, the switch ON, an aggressive percent cap and an
// impossible reserve, a manual bet goes through at exactly the requested stake.
func TestManualBetIgnoresRiskGates(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	pool.SetBetHealthGate(fakeBetGate{d: blockingHealth})
	pool.SetRiskSettings(config.PredictionRiskSettings{
		MaxStakePercent:   1,
		ReservePoints:     100000,
		HealthGateEnabled: true,
	})

	s := newTestStreamer(100000)
	addRound(pool, s, "e1")

	title, err := pool.PlaceManualBet("e1", "o1", 5000)
	if err != nil {
		t.Fatalf("manual bet must ignore risk gates, got error: %v", err)
	}
	if title != "Yes" {
		t.Errorf("outcome title = %q, want Yes", title)
	}
	if placer.callCount() != 1 {
		t.Fatalf("manual bet must be placed; placer called %d times", placer.callCount())
	}
	if placer.lastAmt != 5000 {
		t.Errorf("manual stake = %d, want 5000 (ungated)", placer.lastAmt)
	}
}
