package models

import "testing"

// TestEvaluateStakeGates exercises the stateless GLOBAL size gates and their
// fixed priority. Two gates only live here: the max-stake-percent clamp and the
// reserve floor. The absolute MaxPoints cap is NOT part of EvaluateStake — it is
// applied once inside Calculate (see TestCalculateAppliesMaxPointsOnce).
//
// The reserve is a FLOOR, not a cap: when placing the (already percent-capped)
// stake would push the balance below the reserve, EvaluateStake returns
// GateReserveViolation and the caller must SKIP the bet — the stake is never
// silently shrunk to fit under the reserve.
func TestEvaluateStakeGates(t *testing.T) {
	cases := []struct {
		name              string
		proposed, balance int
		percent, reserve  int
		wantAllowed       int
		wantReason        GateReason
		wantLimit         int
	}{
		// A: no gates configured -> proposed unchanged.
		{"A_no_gates", 500, 10000, 0, 0, 500, GateNone, 0},
		// B: percent cap binds (10% of 10000 = 1000).
		{"B_percent_binds", 5000, 10000, 10, 0, 1000, GatePercent, 1000},
		// C: percent configured but above the proposed stake -> no change.
		{"C_percent_slack", 500, 10000, 50, 0, 500, GateNone, 0},
		// D: reserve floor violated (10000 - 9000 = 1000 < 2000) -> SKIP. The
		// returned allowed is the un-shrunk (percent-capped) stake; the reason
		// tells the caller not to place it.
		{"D_reserve_violation", 9000, 10000, 0, 2000, 9000, GateReserveViolation, 2000},
		// E: reserve configured but leaves enough headroom -> no change.
		{"E_reserve_slack", 500, 10000, 0, 2000, 500, GateNone, 0},
		// F: reserve larger than the balance -> any stake violates it -> SKIP.
		{"F_reserve_over_balance", 500, 1000, 0, 5000, 500, GateReserveViolation, 5000},
		// G: percent binds, reserve still satisfied afterwards -> percent wins,
		// no violation (10000 - 1000 = 9000 >= 2000).
		{"G_percent_binds_reserve_ok", 9000, 10000, 10, 2000, 1000, GatePercent, 1000},
		// H: percent binds (cap 9000) AND the capped stake still violates the
		// reserve (10000 - 9000 = 1000 < 2000) -> reserve violation outranks the
		// percent clamp; allowed stays the percent-capped 9000 but the caller skips.
		{"H_percent_and_reserve_violation", 9500, 10000, 90, 2000, 9000, GateReserveViolation, 2000},
		// I: zero balance -> percent cap is 0, which binds; no reserve set.
		{"I_zero_balance_percent", 500, 0, 10, 0, 0, GatePercent, 0},
		// J: reserve boundary is exact (10000 - 8000 = 2000, NOT < 2000) -> allowed.
		{"J_reserve_exact_boundary_ok", 8000, 10000, 0, 2000, 8000, GateNone, 0},
		// K: one point past the boundary (10000 - 8001 = 1999 < 2000) -> violation.
		{"K_reserve_just_violates", 8001, 10000, 0, 2000, 8001, GateReserveViolation, 2000},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			allowed, reason, limit := EvaluateStake(c.proposed, c.balance, c.percent, c.reserve)
			if allowed != c.wantAllowed || reason != c.wantReason || limit != c.wantLimit {
				t.Errorf("EvaluateStake(proposed=%d, balance=%d, pct=%d, reserve=%d) = (allowed=%d, reason=%q, limit=%d), want (%d, %q, %d)",
					c.proposed, c.balance, c.percent, c.reserve,
					allowed, reason, limit, c.wantAllowed, c.wantReason, c.wantLimit)
			}
		})
	}
}

// TestCalculateAppliesMaxPointsOnce pins the absolute MaxPoints cap to Calculate
// (the single, per-streamer source) and asserts it is applied exactly once: the
// raw percentage stake is capped to MaxPoints when it exceeds it, and passes
// through untouched when it does not. This guards the CORRECTION that MaxPoints
// centralization is deferred to a later PR — EvaluateStake must NOT re-cap it.
func TestCalculateAppliesMaxPointsOnce(t *testing.T) {
	t.Run("raw_above_max_is_capped", func(t *testing.T) {
		bet := &Bet{
			Outcomes: []*Outcome{{ID: "o1"}, {ID: "o2"}},
			Settings: BetSettings{Strategy: StrategyNumber1, Percentage: 50, MaxPoints: 1000},
		}
		// raw = 100000 * 50% = 50000, capped to MaxPoints = 1000.
		d := bet.Calculate(100000)
		if d.Amount != 1000 {
			t.Errorf("Calculate amount = %d, want 1000 (capped to MaxPoints)", d.Amount)
		}
	})

	t.Run("raw_below_max_passes_through", func(t *testing.T) {
		bet := &Bet{
			Outcomes: []*Outcome{{ID: "o1"}, {ID: "o2"}},
			Settings: BetSettings{Strategy: StrategyNumber1, Percentage: 1, MaxPoints: 1000},
		}
		// raw = 50000 * 1% = 500, below MaxPoints -> unchanged.
		d := bet.Calculate(50000)
		if d.Amount != 500 {
			t.Errorf("Calculate amount = %d, want 500 (below MaxPoints, unchanged)", d.Amount)
		}
	})
}
