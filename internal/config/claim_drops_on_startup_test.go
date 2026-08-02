package config

import "testing"

// R7 F1: claimDropsOnStartup is a deprecated compatibility no-op (see the
// doc comment on Config.ClaimDropsOnStartup and SPECIFICATIONS.md's "Drop
// Claiming Flow"). internal/miner no longer has any runtime reader of this
// field at all, so these two cases only pin the parser-facing contract:
// existing config.json files carrying either value must keep loading
// without error, and the value must still round-trip through LoadConfig.

func TestLoadConfigAcceptsClaimDropsOnStartupTrue(t *testing.T) {
	path := writeTestConfig(t, `{"claimDropsOnStartup": true}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("config with claimDropsOnStartup:true must still load: %v", err)
	}
	if !cfg.ClaimDropsOnStartup {
		t.Error("claimDropsOnStartup:true must round-trip through LoadConfig for legacy compatibility")
	}
}

func TestLoadConfigAcceptsClaimDropsOnStartupFalse(t *testing.T) {
	path := writeTestConfig(t, `{"claimDropsOnStartup": false}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("config with claimDropsOnStartup:false must still load: %v", err)
	}
	if cfg.ClaimDropsOnStartup {
		t.Error("claimDropsOnStartup:false must round-trip through LoadConfig")
	}
}
