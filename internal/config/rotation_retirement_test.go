package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRetiredRotationIntervalsLoadButAreNotSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	oldConfig := []byte(`{
		"username": "legacy-user",
		"rateLimits": {
			"minuteWatchedInterval": 45,
			"rotationInterval": 600,
			"rotationIntervalMinMinutes": 15,
			"rotationIntervalMaxMinutes": 45
		}
	}`)
	if err := os.WriteFile(path, oldConfig, 0o600); err != nil {
		t.Fatalf("write old config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("old config containing retired rotation intervals must still load: %v", err)
	}
	if cfg.Username != "legacy-user" || cfg.RateLimits.MinuteWatchedInterval != 45 {
		t.Fatalf("non-retired settings changed while loading old config: %+v", cfg.RateLimits)
	}

	savedPath := filepath.Join(t.TempDir(), "saved.json")
	if err := SaveConfig(savedPath, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	saved, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	for _, retired := range [][]byte{
		[]byte(`"rotationInterval"`),
		[]byte(`"rotationIntervalMinMinutes"`),
		[]byte(`"rotationIntervalMaxMinutes"`),
	} {
		if bytes.Contains(saved, retired) {
			t.Errorf("SaveConfig resurrected retired field %s:\n%s", retired, saved)
		}
	}
}
