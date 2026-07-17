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

// TestPredictionRiskRoundTrips is the BLOCKER-4 / mandatory-#10 guard: the
// GLOBAL prediction-risk gates survive the Settings DTO round trip (config ->
// BuildRuntimeSettings -> ApplyToConfig -> config), and ApplyToConfig runs the
// same validation clamps so an out-of-range percent is stored as it is applied.
func TestPredictionRiskRoundTrips(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.PredictionRisk = config.PredictionRiskSettings{
		MaxStakePercent:   30,
		ReservePoints:     2500,
		HealthGateEnabled: false,
	}

	// config -> DTO.
	rt := BuildRuntimeSettings(&cfg)
	if rt.PredictionRisk.MaxStakePercent != 30 || rt.PredictionRisk.ReservePoints != 2500 || rt.PredictionRisk.HealthGateEnabled {
		t.Fatalf("BuildRuntimeSettings dropped the risk block, got %+v", rt.PredictionRisk)
	}

	// DTO -> config.
	out := config.DefaultConfig()
	ApplyToConfig(&out, rt)
	if out.PredictionRisk.MaxStakePercent != 30 || out.PredictionRisk.ReservePoints != 2500 || out.PredictionRisk.HealthGateEnabled {
		t.Fatalf("ApplyToConfig dropped the risk block, got %+v", out.PredictionRisk)
	}
}

// TestApplyToConfigClampsPredictionRisk proves ApplyToConfig applies the same
// validation clamps as LoadConfig: a UI that posts an out-of-range percent must
// not persist 150 while only 100 is enforced.
func TestApplyToConfigClampsPredictionRisk(t *testing.T) {
	cfg := config.DefaultConfig()
	ApplyToConfig(&cfg, RuntimeSettings{
		PredictionRisk: PredictionRiskConfig{MaxStakePercent: 150, ReservePoints: -10, HealthGateEnabled: true},
	})
	if cfg.PredictionRisk.MaxStakePercent != 100 {
		t.Errorf("percent = %d, want clamped to 100", cfg.PredictionRisk.MaxStakePercent)
	}
	if cfg.PredictionRisk.ReservePoints != 0 {
		t.Errorf("reserve = %d, want clamped to 0", cfg.PredictionRisk.ReservePoints)
	}
	if !cfg.PredictionRisk.HealthGateEnabled {
		t.Error("healthGateEnabled=true must be preserved")
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

func TestDiscoveryPreferTrackedRoundTrips(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.DiscoveryPreferTracked {
		t.Fatalf("default config must leave DiscoveryPreferTracked off (behavior-preserving)")
	}

	// Enabling via the DTO must reach the config.
	ApplyToConfig(&cfg, RuntimeSettings{DiscoveryPreferTracked: true})
	if !cfg.DiscoveryPreferTracked {
		t.Errorf("expected ApplyToConfig to enable DiscoveryPreferTracked")
	}

	// And the enabled value must survive the round trip back to the DTO.
	rt := BuildRuntimeSettings(&cfg)
	if !rt.DiscoveryPreferTracked {
		t.Errorf("expected DiscoveryPreferTracked to survive the round trip, got false")
	}

	// Disabling via the DTO must reach the config too (not a one-way latch).
	ApplyToConfig(&cfg, RuntimeSettings{DiscoveryPreferTracked: false})
	if cfg.DiscoveryPreferTracked {
		t.Errorf("expected ApplyToConfig to disable DiscoveryPreferTracked")
	}
}

func TestDiscoveryModeRoundTrips(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.DiscoveryMode != "" && cfg.DiscoveryMode != config.DiscoveryModeAll {
		t.Fatalf("default config must leave DiscoveryMode at the behavior-preserving default, got %q", cfg.DiscoveryMode)
	}

	// Selecting tracked_only via the DTO must reach the config, canonicalized.
	ApplyToConfig(&cfg, RuntimeSettings{DiscoveryMode: "tracked_only"})
	if cfg.DiscoveryMode != config.DiscoveryModeTrackedOnly {
		t.Errorf("expected ApplyToConfig to set tracked_only, got %q", cfg.DiscoveryMode)
	}

	// And it must survive the round trip back to the DTO.
	rt := BuildRuntimeSettings(&cfg)
	if rt.DiscoveryMode != "tracked_only" {
		t.Errorf("expected DiscoveryMode to survive the round trip, got %q", rt.DiscoveryMode)
	}

	// An empty/unknown DTO value normalizes back to "all" (not a one-way latch).
	ApplyToConfig(&cfg, RuntimeSettings{DiscoveryMode: ""})
	if cfg.DiscoveryMode != config.DiscoveryModeAll {
		t.Errorf("expected an empty DTO mode to normalize to all, got %q", cfg.DiscoveryMode)
	}
}

func TestDiscoveryPreferSubscribedRoundTrips(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.DiscoveryPreferSubscribed {
		t.Fatalf("default config must leave DiscoveryPreferSubscribed off (behavior-preserving)")
	}

	// Enabling via the DTO must reach the config.
	ApplyToConfig(&cfg, RuntimeSettings{DiscoveryPreferSubscribed: true})
	if !cfg.DiscoveryPreferSubscribed {
		t.Errorf("expected ApplyToConfig to enable DiscoveryPreferSubscribed")
	}

	// And the enabled value must survive the round trip back to the DTO.
	rt := BuildRuntimeSettings(&cfg)
	if !rt.DiscoveryPreferSubscribed {
		t.Errorf("expected DiscoveryPreferSubscribed to survive the round trip, got false")
	}

	// Disabling via the DTO must reach the config too (not a one-way latch).
	ApplyToConfig(&cfg, RuntimeSettings{DiscoveryPreferSubscribed: false})
	if cfg.DiscoveryPreferSubscribed {
		t.Errorf("expected ApplyToConfig to disable DiscoveryPreferSubscribed")
	}
}

// --- Env-managed Discord token semantics (Stage C hardening) ---

// With DISCORD_BOT_TOKEN managing the token, the Settings UI must never see
// the real value, and the empty value round-tripping back must not clobber it.
func TestEnvManagedBotTokenHiddenAndPreserved(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Discord.Enabled = true
	cfg.Discord.BotToken = "env-token"
	cfg.DiscordTokenFromEnv = true

	rs := BuildRuntimeSettings(&cfg)
	if rs.Discord.BotToken != "" {
		t.Errorf("UI DTO must hide the env-managed token, got %q", rs.Discord.BotToken)
	}

	// Round-trip the DTO back (as the Settings save flow does).
	ApplyToConfig(&cfg, rs)
	if cfg.Discord.BotToken != "env-token" {
		t.Errorf("ApplyToConfig clobbered the env-managed token: %q", cfg.Discord.BotToken)
	}

	// Even an explicit UI value must not override the env-managed token.
	rs.Discord.BotToken = "typed-in-ui"
	ApplyToConfig(&cfg, rs)
	if cfg.Discord.BotToken != "env-token" {
		t.Errorf("UI value overrode the env-managed token: %q", cfg.Discord.BotToken)
	}
}

// Without env management the existing behavior is unchanged: the UI sees the
// token and its value is applied.
func TestFileManagedBotTokenRoundTrips(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Discord.BotToken = "file-token"

	rs := BuildRuntimeSettings(&cfg)
	if rs.Discord.BotToken != "file-token" {
		t.Errorf("UI DTO should carry the file-managed token, got %q", rs.Discord.BotToken)
	}

	rs.Discord.BotToken = "updated-token"
	ApplyToConfig(&cfg, rs)
	if cfg.Discord.BotToken != "updated-token" {
		t.Errorf("file-managed token not applied: %q", cfg.Discord.BotToken)
	}
}
