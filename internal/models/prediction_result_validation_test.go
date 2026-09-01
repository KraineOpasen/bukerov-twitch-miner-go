package models

import (
	"math"
	"testing"
)

// Direct unit coverage of the pure terminal-payload validation boundary; the
// admission-handler integration lives in internal/pubsub's payload tests.
func TestValidateTerminalResult(t *testing.T) {
	negZero := math.Copysign(0, -1)

	cases := []struct {
		name   string
		result map[string]interface{}
		valid  bool
	}{
		{"nil result", nil, false},
		{"missing type", map[string]interface{}{"points_won": float64(100)}, false},
		{"non-string type", map[string]interface{}{"type": 7.0}, false},
		{"unsupported type", map[string]interface{}{"type": "CANCELED"}, false},
		{"empty type", map[string]interface{}{"type": ""}, false},

		{"WIN valid payout", map[string]interface{}{"type": "WIN", "points_won": float64(1000)}, true},
		{"WIN zero payout", map[string]interface{}{"type": "WIN", "points_won": float64(0)}, true},
		{"WIN negative zero payout", map[string]interface{}{"type": "WIN", "points_won": negZero}, true},
		{"WIN missing payout", map[string]interface{}{"type": "WIN"}, false},
		{"WIN null payout", map[string]interface{}{"type": "WIN", "points_won": nil}, false},
		{"WIN string payout", map[string]interface{}{"type": "WIN", "points_won": "1000"}, false},
		{"WIN NaN payout", map[string]interface{}{"type": "WIN", "points_won": math.NaN()}, false},
		{"WIN +Inf payout", map[string]interface{}{"type": "WIN", "points_won": math.Inf(1)}, false},
		{"WIN -Inf payout", map[string]interface{}{"type": "WIN", "points_won": math.Inf(-1)}, false},
		{"WIN fractional payout", map[string]interface{}{"type": "WIN", "points_won": 999.5}, false},
		{"WIN exact maximum accepted", map[string]interface{}{"type": "WIN", "points_won": maxExactTerminalPayout}, true},
		{"WIN next representable above maximum", map[string]interface{}{"type": "WIN", "points_won": math.Nextafter(maxExactTerminalPayout, math.Inf(1))}, false},
		{"WIN clearly oversized", map[string]interface{}{"type": "WIN", "points_won": 1e300}, false},
		{"WIN negative payout", map[string]interface{}{"type": "WIN", "points_won": float64(-1)}, false},
		{"WIN large negative payout", map[string]interface{}{"type": "WIN", "points_won": float64(-9007199254740992)}, false},

		{"LOSE missing payout", map[string]interface{}{"type": "LOSE"}, true},
		{"LOSE null payout", map[string]interface{}{"type": "LOSE", "points_won": nil}, true},
		{"LOSE zero payout", map[string]interface{}{"type": "LOSE", "points_won": float64(0)}, true},
		{"LOSE negative zero payout", map[string]interface{}{"type": "LOSE", "points_won": negZero}, true},
		{"LOSE non-zero payout", map[string]interface{}{"type": "LOSE", "points_won": float64(500)}, false},
		{"LOSE negative non-zero payout", map[string]interface{}{"type": "LOSE", "points_won": float64(-5)}, false},
		{"LOSE string payout", map[string]interface{}{"type": "LOSE", "points_won": "0"}, false},
		{"LOSE NaN payout", map[string]interface{}{"type": "LOSE", "points_won": math.NaN()}, false},
		{"LOSE positive-infinite payout", map[string]interface{}{"type": "LOSE", "points_won": math.Inf(1)}, false},
		{"LOSE negative-infinite payout", map[string]interface{}{"type": "LOSE", "points_won": math.Inf(-1)}, false},
		{"LOSE fractional payout", map[string]interface{}{"type": "LOSE", "points_won": 0.5}, false},

		{"REFUND missing payout", map[string]interface{}{"type": "REFUND"}, true},
		{"REFUND null payout", map[string]interface{}{"type": "REFUND", "points_won": nil}, true},
		{"REFUND zero payout", map[string]interface{}{"type": "REFUND", "points_won": float64(0)}, true},
		{"REFUND negative zero payout", map[string]interface{}{"type": "REFUND", "points_won": negZero}, true},
		{"REFUND non-zero payout", map[string]interface{}{"type": "REFUND", "points_won": float64(250)}, false},
		{"REFUND negative non-zero payout", map[string]interface{}{"type": "REFUND", "points_won": float64(-250)}, false},
		{"REFUND NaN payout", map[string]interface{}{"type": "REFUND", "points_won": math.NaN()}, false},
		{"REFUND -Inf payout", map[string]interface{}{"type": "REFUND", "points_won": math.Inf(-1)}, false},
		{"REFUND +Inf payout", map[string]interface{}{"type": "REFUND", "points_won": math.Inf(1)}, false},
		{"REFUND fractional payout", map[string]interface{}{"type": "REFUND", "points_won": 0.5}, false},
		{"REFUND string payout", map[string]interface{}{"type": "REFUND", "points_won": "0"}, false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidateTerminalResult(tc.result); got != tc.valid {
				t.Fatalf("ValidateTerminalResult(%v) = %v, want %v", tc.result, got, tc.valid)
			}
		})
	}
}
