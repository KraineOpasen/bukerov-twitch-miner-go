package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The dashboard must bind to loopback unless the user explicitly requests
// otherwise; this pins the fail-safe default (see internal/web/security.go).
func TestDefaultAnalyticsHostIsLoopback(t *testing.T) {
	if got := DefaultAnalyticsSettings().Host; got != "127.0.0.1" {
		t.Fatalf("default analytics host = %q, want loopback 127.0.0.1", got)
	}
	if got := DefaultConfig().Analytics.Host; got != "127.0.0.1" {
		t.Fatalf("DefaultConfig analytics host = %q, want loopback 127.0.0.1", got)
	}
}

// An explicit host in config.json must survive LoadConfig (the loopback
// default only applies when the key is absent), and an absent key must
// inherit the loopback default.
func TestAnalyticsHostLoadBehavior(t *testing.T) {
	dir := t.TempDir()

	explicit := filepath.Join(dir, "explicit.json")
	if err := os.WriteFile(explicit, []byte(`{"username":"u","analytics":{"host":"0.0.0.0"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Analytics.Host != "0.0.0.0" {
		t.Fatalf("explicit host = %q, want 0.0.0.0 preserved", cfg.Analytics.Host)
	}

	absent := filepath.Join(dir, "absent.json")
	if err := os.WriteFile(absent, []byte(`{"username":"u"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(absent)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Analytics.Host != "127.0.0.1" {
		t.Fatalf("defaulted host = %q, want 127.0.0.1", cfg.Analytics.Host)
	}
}
