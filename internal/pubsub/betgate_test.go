package pubsub

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// fakeBetGate is a test double for the injected transport-health gate.
type fakeBetGate struct{ allowed bool }

func (g fakeBetGate) AutoBetAllowed() bool { return g.allowed }

// gateBetSettings yields a settings value that produces a valid auto-bet: with
// StrategyNumber1 the bot always picks outcome 0, and Percentage 5 of a 10000
// balance is 500 — above the 10-point minimum and below MaxPoints.
func gateBetSettings(healthGate bool, reserve int) models.BetSettings {
	return models.BetSettings{
		Strategy:          models.StrategyNumber1,
		Percentage:        5,
		MaxPoints:         50000,
		ReservePoints:     reserve,
		HealthGateEnabled: healthGate,
	}
}

// TestAutoBetHealthGateOnBlocksWhenUnhealthy — health switch test #1: with the
// gate switch ON and the link unhealthy, the auto-bet is blocked (no placement).
func TestAutoBetHealthGateOnBlocksWhenUnhealthy(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	pool.SetBetHealthGate(fakeBetGate{allowed: false})

	ep := addRound(pool, newTestStreamer(10000), "e1")
	ep.Bet.Settings = gateBetSettings(true, 0)

	pool.placeAutoBet("e1")

	if placer.callCount() != 0 {
		t.Fatalf("gate ON + unhealthy must block the auto-bet; placer called %d times", placer.callCount())
	}
}

// TestAutoBetHealthGateOffAllowsWhenUnhealthy — health switch test #2: with the
// gate switch OFF, the auto-bet proceeds even when the link is unhealthy.
func TestAutoBetHealthGateOffAllowsWhenUnhealthy(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	pool.SetBetHealthGate(fakeBetGate{allowed: false})

	ep := addRound(pool, newTestStreamer(10000), "e1")
	ep.Bet.Settings = gateBetSettings(false, 0)

	pool.placeAutoBet("e1")

	if placer.callCount() != 1 {
		t.Fatalf("gate OFF must let the auto-bet through even when unhealthy; placer called %d times", placer.callCount())
	}
	if placer.lastAmt != 500 {
		t.Errorf("stake = %d, want 500", placer.lastAmt)
	}
}

// TestAutoBetHealthGateNilFailsOpen: an absent gate (unknown health) fails open.
func TestAutoBetHealthGateNilFailsOpen(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer) // no SetBetHealthGate -> nil -> unknown -> fail-open

	ep := addRound(pool, newTestStreamer(10000), "e1")
	ep.Bet.Settings = gateBetSettings(true, 0)

	pool.placeAutoBet("e1")

	if placer.callCount() != 1 {
		t.Fatalf("nil gate (unknown health) must fail open; placer called %d times", placer.callCount())
	}
}

// TestAutoBetReserveClampsPlacedStake proves the stateless size gate clamps the
// stake actually sent to Twitch: percent 50 of 10000 proposes 5000, the reserve
// of 8000 caps it to 10000-8000 = 2000.
func TestAutoBetReserveClampsPlacedStake(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)

	ep := addRound(pool, newTestStreamer(10000), "e1")
	s := gateBetSettings(true, 8000)
	s.Percentage = 50
	ep.Bet.Settings = s

	pool.placeAutoBet("e1")

	if placer.callCount() != 1 {
		t.Fatalf("expected one bet, got %d", placer.callCount())
	}
	if placer.lastAmt != 2000 {
		t.Errorf("reserve gate: placed %d, want 2000", placer.lastAmt)
	}
}
