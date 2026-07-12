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

func TestApplyToConfigNormalizesDirectoryGames(t *testing.T) {
	cfg := config.DefaultConfig()

	ApplyToConfig(&cfg, RuntimeSettings{
		DirectoryGames: []string{" World of Tanks ", "", "world of tanks", "Rust", "  "},
	})

	if len(cfg.DirectoryGames) != 2 {
		t.Fatalf("expected blanks and case-insensitive duplicates removed, got %v", cfg.DirectoryGames)
	}
	if cfg.DirectoryGames[0] != "World of Tanks" || cfg.DirectoryGames[1] != "Rust" {
		t.Errorf("expected trimmed, order-preserving list, got %v", cfg.DirectoryGames)
	}
}

func TestApplyToConfigEmptyDirectoryGamesBecomesNil(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DirectoryGames = []string{"stale"}

	ApplyToConfig(&cfg, RuntimeSettings{DirectoryGames: []string{"  ", ""}})

	if cfg.DirectoryGames != nil {
		t.Errorf("expected nil directory games when all entries blank, got %v", cfg.DirectoryGames)
	}
}

func TestBuildRuntimeSettingsRoundTripsDirectoryGames(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DirectoryGames = []string{"World of Tanks", "Rust"}

	rt := BuildRuntimeSettings(&cfg)
	if len(rt.DirectoryGames) != 2 || rt.DirectoryGames[0] != "World of Tanks" || rt.DirectoryGames[1] != "Rust" {
		t.Errorf("expected directory games to survive the round trip, got %v", rt.DirectoryGames)
	}
}
