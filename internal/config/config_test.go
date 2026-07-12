package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}
	return path
}

func TestLoadConfigDefaultsRotationRange(t *testing.T) {
	path := writeTestConfig(t, `{"username": "test"}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.RateLimits.RotationIntervalMinMinutes != 30 {
		t.Errorf("expected default min 30, got %d", cfg.RateLimits.RotationIntervalMinMinutes)
	}
	if cfg.RateLimits.RotationIntervalMaxMinutes != 80 {
		t.Errorf("expected default max 80, got %d", cfg.RateLimits.RotationIntervalMaxMinutes)
	}
	if cfg.RateLimits.RotationInterval != 0 {
		t.Errorf("expected deprecated field to stay unset, got %d", cfg.RateLimits.RotationInterval)
	}
}

func TestLoadConfigMigratesLegacyRotationInterval(t *testing.T) {
	path := writeTestConfig(t, `{"username": "test", "rateLimits": {"rotationInterval": 600}}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.RateLimits.RotationIntervalMinMinutes != 10 {
		t.Errorf("expected legacy 600s to migrate to 10 min minimum, got %d", cfg.RateLimits.RotationIntervalMinMinutes)
	}
	if cfg.RateLimits.RotationIntervalMaxMinutes != 10 {
		t.Errorf("expected legacy 600s to migrate to 10 min maximum, got %d", cfg.RateLimits.RotationIntervalMaxMinutes)
	}
	if cfg.RateLimits.RotationInterval != 0 {
		t.Errorf("expected deprecated field to be cleared after migration, got %d", cfg.RateLimits.RotationInterval)
	}
}

func TestLoadConfigIgnoresLegacyFieldWhenRangeAlreadySet(t *testing.T) {
	path := writeTestConfig(t, `{
		"username": "test",
		"rateLimits": {
			"rotationInterval": 600,
			"rotationIntervalMinMinutes": 15,
			"rotationIntervalMaxMinutes": 45
		}
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.RateLimits.RotationIntervalMinMinutes != 15 {
		t.Errorf("expected explicit min 15 to be preserved, got %d", cfg.RateLimits.RotationIntervalMinMinutes)
	}
	if cfg.RateLimits.RotationIntervalMaxMinutes != 45 {
		t.Errorf("expected explicit max 45 to be preserved, got %d", cfg.RateLimits.RotationIntervalMaxMinutes)
	}
}

func TestValidateConfigClampsRotationRangeAndOrdering(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RateLimits.RotationIntervalMinMinutes = 500
	cfg.RateLimits.RotationIntervalMaxMinutes = 1
	ValidateConfig(&cfg)

	if cfg.RateLimits.RotationIntervalMinMinutes != 180 {
		t.Errorf("expected min clamped to 180, got %d", cfg.RateLimits.RotationIntervalMinMinutes)
	}
	if cfg.RateLimits.RotationIntervalMaxMinutes < cfg.RateLimits.RotationIntervalMinMinutes {
		t.Errorf("expected max (%d) to never be below min (%d) after clamping",
			cfg.RateLimits.RotationIntervalMaxMinutes, cfg.RateLimits.RotationIntervalMinMinutes)
	}
}

func TestDebugSettingsDefaults(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Debug.Enabled {
		t.Error("expected debug server disabled by default")
	}
	if cfg.Debug.Port != 5757 {
		t.Errorf("expected default debug port 5757, got %d", cfg.Debug.Port)
	}
}

func TestLoadConfigDefaultsDebugPortWhenAbsent(t *testing.T) {
	path := writeTestConfig(t, `{
		"username": "test",
		"debug": {"enabled": true}
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if !cfg.Debug.Enabled {
		t.Error("expected debug enabled from config")
	}
	if cfg.Debug.Port != 5757 {
		t.Errorf("expected absent port to fall back to 5757, got %d", cfg.Debug.Port)
	}
}

func TestValidateConfigClampsDebugPort(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Debug.Port = 99999
	ValidateConfig(&cfg)

	if cfg.Debug.Port != 5757 {
		t.Errorf("expected out-of-range port reset to 5757, got %d", cfg.Debug.Port)
	}
}

func TestLoadConfigDirectoryGamesDefaultEmpty(t *testing.T) {
	// Backward compatibility: configs written before directory discovery
	// existed must load with the subsystem fully disabled.
	path := writeTestConfig(t, `{"username": "test"}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if len(cfg.DirectoryGames) != 0 {
		t.Errorf("expected no directory games by default, got %v", cfg.DirectoryGames)
	}
}

func TestLoadConfigParsesDirectoryGames(t *testing.T) {
	path := writeTestConfig(t, `{
		"username": "test",
		"directoryGames": ["World of Tanks", "Rust"]
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if len(cfg.DirectoryGames) != 2 || cfg.DirectoryGames[0] != "World of Tanks" || cfg.DirectoryGames[1] != "Rust" {
		t.Errorf("expected configured directory games, got %v", cfg.DirectoryGames)
	}
}
