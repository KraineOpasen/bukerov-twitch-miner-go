package pubsub

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func TestComputeGoalContribution(t *testing.T) {
	tests := []struct {
		name       string
		goal       *models.CommunityGoal
		points     int
		maxPercent int
		maxAmount  int
		want       int
	}{
		{
			name:   "no limits contributes up to amount left",
			goal:   &models.CommunityGoal{GoalAmount: 1000, PointsContributed: 400},
			points: 10000,
			want:   600,
		},
		{
			name:   "no limits capped by balance",
			goal:   &models.CommunityGoal{GoalAmount: 100000, PointsContributed: 0},
			points: 500,
			want:   500,
		},
		{
			name:       "percentage cap applies",
			goal:       &models.CommunityGoal{GoalAmount: 100000, PointsContributed: 0},
			points:     10000,
			maxPercent: 10,
			want:       1000,
		},
		{
			name:      "absolute cap applies",
			goal:      &models.CommunityGoal{GoalAmount: 100000, PointsContributed: 0},
			points:    10000,
			maxAmount: 250,
			want:      250,
		},
		{
			name:       "lower of percentage and absolute wins (absolute)",
			goal:       &models.CommunityGoal{GoalAmount: 100000, PointsContributed: 0},
			points:     10000,
			maxPercent: 10,  // -> 1000
			maxAmount:  250, // -> 250
			want:       250,
		},
		{
			name:       "lower of percentage and absolute wins (percentage)",
			goal:       &models.CommunityGoal{GoalAmount: 100000, PointsContributed: 0},
			points:     10000,
			maxPercent: 2,    // -> 200
			maxAmount:  5000, // -> 5000
			want:       200,
		},
		{
			name:   "server per-stream cap honored",
			goal:   &models.CommunityGoal{GoalAmount: 100000, PointsContributed: 0, PerStreamUserMaxContribution: 300},
			points: 10000,
			want:   300,
		},
		{
			name:       "percentage rounds down to zero for tiny balance",
			goal:       &models.CommunityGoal{GoalAmount: 100000, PointsContributed: 0},
			points:     5,
			maxPercent: 10, // 5*10/100 = 0
			want:       0,
		},
		{
			name:       "percentage of 100 is treated as no cap",
			goal:       &models.CommunityGoal{GoalAmount: 100000, PointsContributed: 0},
			points:     10000,
			maxPercent: 100,
			want:       10000,
		},
		{
			name:       "large balance does not overflow",
			goal:       &models.CommunityGoal{GoalAmount: 1000000000, PointsContributed: 0},
			points:     50000000,
			maxPercent: 99,
			want:       49500000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := models.StreamerSettings{
				CommunityGoalsMaxPercent: tt.maxPercent,
				CommunityGoalsMaxAmount:  tt.maxAmount,
			}
			got := computeGoalContribution(tt.goal, tt.points, settings)
			if got != tt.want {
				t.Errorf("computeGoalContribution() = %d, want %d", got, tt.want)
			}
		})
	}
}
