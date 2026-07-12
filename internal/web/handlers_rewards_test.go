package web

import (
	"testing"
	"time"

	"github.com/PatrickWalther/twitch-miner-go/internal/config"
	"github.com/PatrickWalther/twitch-miner-go/internal/models"
)

func TestBuildRewardViews(t *testing.T) {
	rewards := []*models.CustomReward{
		{ID: "a", Title: "Available", Cost: 100, IsEnabled: true, IsInStock: true},
		{ID: "b", Title: "Paused", Cost: 200, IsEnabled: true, IsPaused: true, IsInStock: true},
		{ID: "c", Title: "Input", Cost: 300, IsEnabled: true, IsInStock: true, IsUserInputRequired: true},
	}

	views := buildRewardViews(rewards)
	if len(views) != 3 {
		t.Fatalf("expected 3 views, got %d", len(views))
	}
	if !views[0].Available || views[0].Unavailable != "" {
		t.Errorf("reward a should be available with no reason: %+v", views[0])
	}
	if views[1].Available || views[1].Unavailable != "Paused" {
		t.Errorf("reward b should be unavailable (Paused): %+v", views[1])
	}
	if !views[2].RequiresInput {
		t.Errorf("reward c should require input")
	}
}

func TestRewardUnavailableReason(t *testing.T) {
	future := time.Now().Add(time.Hour)
	tests := []struct {
		reward *models.CustomReward
		want   string
	}{
		{&models.CustomReward{IsEnabled: false}, "Disabled"},
		{&models.CustomReward{IsEnabled: true, IsPaused: true}, "Paused"},
		{&models.CustomReward{IsEnabled: true, IsInStock: false}, "Out of stock"},
		{&models.CustomReward{IsEnabled: true, IsInStock: true, CooldownExpiresAt: future}, "On cooldown"},
	}
	for _, tc := range tests {
		if got := rewardUnavailableReason(tc.reward); got != tc.want {
			t.Errorf("rewardUnavailableReason(%+v) = %q, want %q", tc.reward, got, tc.want)
		}
	}
}

func TestAutoRedeemView(t *testing.T) {
	// nil RewardIDs must serialize as an empty slice, not null.
	v := autoRedeemView(config.AutoRedeemConfig{Enabled: true, Budget: 5000})
	if v.RewardIDs == nil {
		t.Error("RewardIDs should be non-nil empty slice")
	}
	if !v.Enabled || v.Budget != 5000 {
		t.Errorf("unexpected view: %+v", v)
	}
}
