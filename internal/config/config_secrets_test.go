package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func clearSecretEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DISCORD_BOT_TOKEN", "")
}

// SaveConfig must write atomically with owner-only permissions.
func TestSaveConfigOwnerOnlyPerms(t *testing.T) {
	clearSecretEnv(t)
	path := filepath.Join(t.TempDir(), "config.json")

	cfg := DefaultConfig()
	cfg.Username = "u"
	cfg.Discord.BotToken = "file-token"
	if err := SaveConfig(path, &cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("config perm = %o, want 0600", perm)
		}
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Discord.BotToken != "file-token" {
		t.Errorf("token round-trip = %q", loaded.Discord.BotToken)
	}
}

// A pre-hardening 0644 config file is tightened to 0600 on the first load,
// without waiting for the next save (migrate-on-load).
func TestLoadConfigTightensLegacyPerms(t *testing.T) {
	clearSecretEnv(t)
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"username":"u"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(path); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm after load = %o, want 0600 (migrate-on-load)", perm)
	}
}

// DISCORD_BOT_TOKEN overrides the file value at load and is flagged as
// env-managed.
func TestDiscordTokenEnvOverride(t *testing.T) {
	clearSecretEnv(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"username":"u","discord":{"enabled":true,"botToken":"file-token","guildId":"g"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DISCORD_BOT_TOKEN", "env-token")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Discord.BotToken != "env-token" {
		t.Errorf("token = %q, want env-token", cfg.Discord.BotToken)
	}
	if !cfg.DiscordTokenFromEnv {
		t.Error("DiscordTokenFromEnv must be set")
	}

	// Without the env var the file value applies and the flag stays off.
	t.Setenv("DISCORD_BOT_TOKEN", "")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Discord.BotToken != "file-token" || cfg.DiscordTokenFromEnv {
		t.Errorf("without env: token = %q, fromEnv = %v", cfg.Discord.BotToken, cfg.DiscordTokenFromEnv)
	}
}

// While the token is env-managed, SaveConfig clears the on-disk copy and
// never persists the env value (documented, deliberately irreversible).
func TestSaveConfigNeverPersistsEnvToken(t *testing.T) {
	clearSecretEnv(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"username":"u","discord":{"botToken":"file-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DISCORD_BOT_TOKEN", "env-token")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}

	// In-memory config keeps working with the env token after the save.
	if cfg.Discord.BotToken != "env-token" {
		t.Errorf("in-memory token clobbered by SaveConfig: %q", cfg.Discord.BotToken)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct {
		Discord struct {
			BotToken string `json:"botToken"`
		} `json:"discord"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Discord.BotToken != "" {
		t.Errorf("on-disk token = %q, must be cleared while env-managed", onDisk.Discord.BotToken)
	}
}
