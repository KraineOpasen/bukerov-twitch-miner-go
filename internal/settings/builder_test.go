package settings

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

func TestApplyToConfigNormalizesDropBlacklist(t *testing.T) {
	cfg := config.DefaultConfig()

	ApplyToConfig(&cfg, RuntimeSettings{
		DropBlacklist: []string{" League ", "", "Skin", "   "},
	})

	if len(cfg.DropBlacklist) != 2 {
		t.Fatalf("expected blank entries trimmed away, got %v", cfg.DropBlacklist)
	}
	if cfg.DropBlacklist[0] != "League" || cfg.DropBlacklist[1] != "Skin" {
		t.Errorf("expected trimmed keywords, got %v", cfg.DropBlacklist)
	}
}

func TestApplyToConfigEmptyBlacklistBecomesNil(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DropBlacklist = []string{"stale"}

	ApplyToConfig(&cfg, RuntimeSettings{DropBlacklist: []string{"  ", ""}})

	if cfg.DropBlacklist != nil {
		t.Errorf("expected nil blacklist when all entries blank, got %v", cfg.DropBlacklist)
	}
}

func TestBuildRuntimeSettingsRoundTripsDropBlacklist(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DropBlacklist = []string{"foo", "bar"}

	rt := BuildRuntimeSettings(&cfg)
	if len(rt.DropBlacklist) != 2 || rt.DropBlacklist[0] != "foo" || rt.DropBlacklist[1] != "bar" {
		t.Errorf("expected blacklist to survive the round trip, got %v", rt.DropBlacklist)
	}
}
