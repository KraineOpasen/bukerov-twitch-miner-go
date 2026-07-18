package config

import (
	"path/filepath"
	"testing"
)

// §8.26: the logger `colored` setting round-trips through save/load.
func TestLoggerColoredRoundTripsSaveLoad(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Logger.Colored = true
	path := filepath.Join(t.TempDir(), "config.json")
	if err := SaveConfig(path, &cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !loaded.Logger.Colored {
		t.Error("logger.colored must survive save/load")
	}
}

// §8.34 + §8.35: a pre-hotfix config carrying "less": true still loads, and the
// field is retained (for backward compatibility) but ignored — it never alters
// the configured log levels.
func TestOldConfigWithLessStillLoads(t *testing.T) {
	path := writeTestConfig(t, `{"logger":{"save":true,"less":true,"consoleLevel":"INFO","fileLevel":"DEBUG","colored":false}}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("old config with less:true must still load: %v", err)
	}
	if !cfg.Logger.Less {
		t.Error("the Less field must be retained for backward compatibility")
	}
	if cfg.Logger.ConsoleLevel != "INFO" || cfg.Logger.FileLevel != "DEBUG" {
		t.Errorf("less=true must not change log levels, got console=%q file=%q", cfg.Logger.ConsoleLevel, cfg.Logger.FileLevel)
	}
}
