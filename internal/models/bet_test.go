package models

import "testing"

// TestClampStakeGates exercises the stateless size gates (reserve, max-stake
// percent, and the absolute MaxPoints cap) and their fixed reason priority
// (reserve > percent > absolute; tightest cap wins, ties break by priority).
// Cases A-O.
func TestClampStakeGates(t *testing.T) {
	cases := []struct {
		name              string
		proposed, balance int
		percent, reserve  int
		maxPoints         int
		wantAllowed       int
		wantReason        GateReason
		wantLimit         int
	}{
		// A: no gates configured -> proposed unchanged.
		{"A_no_gates", 500, 10000, 0, 0, 50000, 500, GateNone, 0},
		// B: percent cap binds (10% of 10000 = 1000).
		{"B_percent_binds", 5000, 10000, 10, 0, 50000, 1000, GatePercent, 1000},
		// C: percent configured but above the proposed stake -> no change.
		{"C_percent_slack", 500, 10000, 50, 0, 50000, 500, GateNone, 0},
		// D: reserve floor binds (10000 - 2000 = 8000).
		{"D_reserve_binds", 9000, 10000, 0, 2000, 50000, 8000, GateReserve, 8000},
		// E: reserve configured but leaves enough headroom -> no change.
		{"E_reserve_slack", 500, 10000, 0, 2000, 50000, 500, GateNone, 0},
		// F: reserve exceeds balance -> floor clamps to 0.
		{"F_reserve_over_balance", 500, 1000, 0, 5000, 50000, 0, GateReserve, 0},
		// G: both set, percent is the tighter cap -> percent wins.
		{"G_percent_tighter", 9000, 10000, 10, 2000, 50000, 1000, GatePercent, 1000},
		// H: both set, reserve is the tighter cap -> reserve wins.
		{"H_reserve_tighter", 9000, 10000, 90, 2000, 50000, 8000, GateReserve, 8000},
		// I: percent cap == reserve cap (both 2000) -> tie breaks to reserve.
		{"I_percent_reserve_tie", 9000, 10000, 20, 8000, 50000, 2000, GateReserve, 2000},
		// J: absolute MaxPoints cap binds (standalone).
		{"J_absolute_binds", 60000, 100000, 0, 0, 50000, 50000, GateAbsolute, 50000},
		// K: percent > 100 is clamped to 100 (cap == balance).
		{"K_percent_over_100", 20000, 10000, 150, 0, 50000, 10000, GatePercent, 10000},
		// L: percent cap == absolute cap (both 50000) -> tie breaks to percent.
		{"L_percent_absolute_tie", 60000, 100000, 50, 0, 50000, 50000, GatePercent, 50000},
		// M: reserve cap == absolute cap (both 50000) -> tie breaks to reserve.
		{"M_reserve_absolute_tie", 60000, 100000, 0, 50000, 50000, 50000, GateReserve, 50000},
		// N: all three set, reserve is the tightest (30000).
		{"N_all_three_reserve_tightest", 40000, 100000, 40, 70000, 50000, 30000, GateReserve, 30000},
		// O: zero balance -> percent cap is 0, which binds.
		{"O_zero_balance", 500, 0, 10, 0, 50000, 0, GatePercent, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := BetSettings{MaxStakePercent: c.percent, ReservePoints: c.reserve, MaxPoints: c.maxPoints}
			allowed, reason, limit := s.ClampStake(c.proposed, c.balance)
			if allowed != c.wantAllowed || reason != c.wantReason || limit != c.wantLimit {
				t.Errorf("ClampStake(proposed=%d, balance=%d) pct=%d reserve=%d max=%d = (allowed=%d, reason=%q, limit=%d), want (%d, %q, %d)",
					c.proposed, c.balance, c.percent, c.reserve, c.maxPoints,
					allowed, reason, limit, c.wantAllowed, c.wantReason, c.wantLimit)
			}
		})
	}
}
