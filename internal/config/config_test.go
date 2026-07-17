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

func TestNormalizeDailySummaryTime(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"09:00", "09:00", true},
		{"9:5", "09:05", true}, // zero-padded canonical form
		{"23:59", "23:59", true},
		{" 08:30 ", "08:30", true}, // trimmed
		{"", "09:00", false},
		{"25:00", "09:00", false},
		{"nonsense", "09:00", false},
	}
	for _, tc := range cases {
		got, ok := NormalizeDailySummaryTime(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("NormalizeDailySummaryTime(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestValidateConfigCanonicalizesDailySummaryTime(t *testing.T) {
	c := &Config{DailySummary: DailySummarySettings{Enabled: true, Time: "7:5"}}
	ValidateConfig(c)
	if c.DailySummary.Time != "07:05" {
		t.Errorf("daily summary time = %q, want 07:05", c.DailySummary.Time)
	}

	c = &Config{DailySummary: DailySummarySettings{Enabled: true, Time: "banana"}}
	ValidateConfig(c)
	if c.DailySummary.Time != "09:00" {
		t.Errorf("invalid time must fall back to 09:00, got %q", c.DailySummary.Time)
	}
}

func TestDefaultDailySummaryIsOptIn(t *testing.T) {
	d := DefaultDailySummarySettings()
	if d.Enabled {
		t.Error("daily summary must default to disabled (opt-in)")
	}
	if d.Time != "09:00" {
		t.Errorf("default time = %q, want 09:00", d.Time)
	}
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

func TestValidateConfigClampsHealthStalenessToInterval(t *testing.T) {
	// The forced-probe staleness threshold must cover at least one opportunistic
	// interval; otherwise the force condition fires first and the hybrid
	// schedule degenerates into "always force a real beacon every interval".
	cfg := DefaultConfig()
	cfg.Health.CanaryIntervalMinutes = 360 // 6h
	cfg.Health.CanaryMaxStalenessHours = 2 // below the interval
	ValidateConfig(&cfg)

	if cfg.Health.CanaryMaxStalenessHours != 6 {
		t.Errorf("expected staleness clamped up to the 6h interval, got %d", cfg.Health.CanaryMaxStalenessHours)
	}
}

func TestValidateConfigRoundsStalenessClampUpToWholeHours(t *testing.T) {
	// A sub-hour remainder in the interval must round the staleness floor up so
	// it still fully covers the interval (90 min -> 2h, never 1h).
	cfg := DefaultConfig()
	cfg.Health.CanaryIntervalMinutes = 90
	cfg.Health.CanaryMaxStalenessHours = 1
	ValidateConfig(&cfg)

	if cfg.Health.CanaryMaxStalenessHours != 2 {
		t.Errorf("expected 90-min interval to raise staleness floor to 2h, got %d", cfg.Health.CanaryMaxStalenessHours)
	}
}

func TestValidateConfigPreservesStalenessWhenItCoversInterval(t *testing.T) {
	// When staleness already exceeds the interval the cross-check must leave it
	// untouched — the hybrid schedule stays intact.
	cfg := DefaultConfig()
	cfg.Health.CanaryIntervalMinutes = 60
	cfg.Health.CanaryMaxStalenessHours = 48
	ValidateConfig(&cfg)

	if cfg.Health.CanaryMaxStalenessHours != 48 {
		t.Errorf("expected staleness of 48h to be preserved, got %d", cfg.Health.CanaryMaxStalenessHours)
	}
}

func TestValidateConfigClampsWatchdogSettings(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Health.WatchdogStallDelayMinutes = 1       // below 10
	cfg.Health.WatchdogStallConfirmations = 99     // above 10
	cfg.Health.WatchdogRecoveryCooldownMinutes = 0 // below 1
	cfg.Health.WatchdogAvoidTTLMinutes = 5000      // above 360
	cfg.Health.WatchdogRearmHours = 0              // below 1
	ValidateConfig(&cfg)

	if cfg.Health.WatchdogStallDelayMinutes != 10 {
		t.Errorf("expected stall delay clamped to 10, got %d", cfg.Health.WatchdogStallDelayMinutes)
	}
	if cfg.Health.WatchdogStallConfirmations != 10 {
		t.Errorf("expected confirmations clamped to 10, got %d", cfg.Health.WatchdogStallConfirmations)
	}
	if cfg.Health.WatchdogRecoveryCooldownMinutes != 1 {
		t.Errorf("expected cooldown clamped to 1, got %d", cfg.Health.WatchdogRecoveryCooldownMinutes)
	}
	if cfg.Health.WatchdogAvoidTTLMinutes != 360 {
		t.Errorf("expected avoid TTL clamped to 360, got %d", cfg.Health.WatchdogAvoidTTLMinutes)
	}
	if cfg.Health.WatchdogRearmHours != 1 {
		t.Errorf("expected rearm clamped to 1, got %d", cfg.Health.WatchdogRearmHours)
	}
}

// TestLoadConfigWatchdogDefaultsOnForExistingConfigs guards the opt-out
// default: a pre-Stage-3 config.json — even one that already carries a canary
// health block — must load with the watchdog enabled and its conservative
// thresholds populated, while an explicit "watchdogEnabled": false is honored.
func TestLoadConfigWatchdogDefaultsOnForExistingConfigs(t *testing.T) {
	path := writeTestConfig(t, `{
		"username": "test",
		"health": {"canaryEnabled": true, "canaryChannel": "somechan"}
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if !cfg.Health.WatchdogEnabled {
		t.Error("expected the watchdog enabled by default for configs without the field")
	}
	if cfg.Health.WatchdogStallDelayMinutes != 20 || cfg.Health.WatchdogStallConfirmations != 3 {
		t.Errorf("expected conservative defaults 20/3, got %d/%d",
			cfg.Health.WatchdogStallDelayMinutes, cfg.Health.WatchdogStallConfirmations)
	}
	if !cfg.Health.CanaryEnabled || cfg.Health.CanaryChannel != "somechan" {
		t.Error("expected the existing canary settings to be preserved")
	}

	pathOff := writeTestConfig(t, `{
		"username": "test",
		"health": {"watchdogEnabled": false}
	}`)
	cfgOff, err := LoadConfig(pathOff)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfgOff.Health.WatchdogEnabled {
		t.Error("expected an explicit watchdogEnabled=false to be honored")
	}
}

func TestValidateConfigNormalizesCampaignPolicy(t *testing.T) {
	cases := map[string]string{
		"":                  "GAME_ORDER", // unset → behavior-preserving default
		"smart":             "SMART",      // lower-cased normalizes
		"  ending_soonest ": "ENDING_SOONEST",
		"nonsense":          "GAME_ORDER", // unknown falls back
	}
	for in, want := range cases {
		cfg := DefaultConfig()
		cfg.CampaignPolicy = in
		ValidateConfig(&cfg)
		if cfg.CampaignPolicy != want {
			t.Errorf("policy %q normalized to %q, want %q", in, cfg.CampaignPolicy, want)
		}
	}
}

func TestValidateConfigNormalizesDiscoveryMode(t *testing.T) {
	cases := map[string]DiscoveryMode{
		"":              DiscoveryModeAll,         // unset → behavior-preserving default
		"all":           DiscoveryModeAll,         // canonical
		"TRACKED_ONLY":  DiscoveryModeTrackedOnly, // upper-cased normalizes
		" tracked_only": DiscoveryModeTrackedOnly, // trimmed
		"nonsense":      DiscoveryModeAll,         // unknown falls back
	}
	for in, want := range cases {
		cfg := DefaultConfig()
		cfg.DiscoveryMode = DiscoveryMode(in)
		ValidateConfig(&cfg)
		if cfg.DiscoveryMode != want {
			t.Errorf("mode %q normalized to %q, want %q", in, cfg.DiscoveryMode, want)
		}
	}
}

func TestLoadConfigParsesDropRules(t *testing.T) {
	path := writeTestConfig(t, `{
		"username": "test",
		"campaignPolicy": "SMART",
		"dropRules": {
			"g1::cool skin": {"skip": true},
			"g2::rare emote": {"highPriority": true, "nextRewardOnly": true}
		}
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.CampaignPolicy != "SMART" {
		t.Errorf("expected SMART policy preserved, got %q", cfg.CampaignPolicy)
	}
	if len(cfg.DropRules) != 2 {
		t.Fatalf("expected 2 drop rules, got %d", len(cfg.DropRules))
	}
	if !cfg.DropRules["g1::cool skin"].Skip {
		t.Error("expected the skip rule to parse")
	}
	r := cfg.DropRules["g2::rare emote"]
	if !r.HighPriority || !r.NextRewardOnly || r.Skip {
		t.Errorf("unexpected rule parse: %+v", r)
	}
}

func TestLoadConfigDropRulesDefaultEmpty(t *testing.T) {
	// Backward compatibility: a config written before per-drop rules existed
	// loads with no rules and the behavior-preserving default policy.
	path := writeTestConfig(t, `{"username": "test"}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if len(cfg.DropRules) != 0 {
		t.Errorf("expected no drop rules by default, got %v", cfg.DropRules)
	}
	if cfg.CampaignPolicy != "GAME_ORDER" {
		t.Errorf("expected default GAME_ORDER policy, got %q", cfg.CampaignPolicy)
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

// TestLoadConfigPredictionRiskDefaultsOnForExistingConfigs is the BLOCKER-4
// guard: the GLOBAL prediction-risk block defaults to the health gate ON for
// configs written before it existed. Crucially it is TOP-LEVEL, so per-streamer
// settings blocks (which wholesale-replace the defaults) can never silently
// disable it — the failure mode the reviewer found in production, where every
// streamer carried a settings block and a per-streamer-scoped opt-out bool would
// have loaded false everywhere. An explicit healthGateEnabled=false is honored.
func TestLoadConfigPredictionRiskDefaultsOnForExistingConfigs(t *testing.T) {
	// Old config: has per-streamer settings blocks, no predictionRisk key.
	path := writeTestConfig(t, `{
		"username": "test",
		"streamers": [
			{"username": "alice", "settings": {"makePredictions": true}},
			{"username": "bob", "settings": {"makePredictions": false}}
		]
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if !cfg.PredictionRisk.HealthGateEnabled {
		t.Error("health gate must default ON for existing configs, even with per-streamer settings blocks")
	}
	if cfg.PredictionRisk.MaxStakePercent != 0 || cfg.PredictionRisk.ReservePoints != 0 {
		t.Errorf("size gates must default OFF (0/0), got pct=%d reserve=%d",
			cfg.PredictionRisk.MaxStakePercent, cfg.PredictionRisk.ReservePoints)
	}

	// An explicit opt-out is honored.
	pathOff := writeTestConfig(t, `{
		"username": "test",
		"predictionRisk": {"healthGateEnabled": false, "maxStakePercent": 25, "reservePoints": 1000}
	}`)
	cfgOff, err := LoadConfig(pathOff)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfgOff.PredictionRisk.HealthGateEnabled {
		t.Error("an explicit healthGateEnabled=false must be honored")
	}
	if cfgOff.PredictionRisk.MaxStakePercent != 25 || cfgOff.PredictionRisk.ReservePoints != 1000 {
		t.Errorf("explicit size gates must be parsed, got pct=%d reserve=%d",
			cfgOff.PredictionRisk.MaxStakePercent, cfgOff.PredictionRisk.ReservePoints)
	}
}

// TestValidateConfigClampsPredictionRisk pins the VALIDATION rule: store exactly
// what is applied. MaxStakePercent is clamped to 0..100 (0 = off), never stored
// as 150 while 100 is applied; ReservePoints is clamped to >= 0 so a negative
// never masquerades as "off".
func TestValidateConfigClampsPredictionRisk(t *testing.T) {
	cases := []struct {
		name              string
		inPct, inReserve  int
		wantPct, wantResv int
	}{
		{"in_range_unchanged", 25, 1000, 25, 1000},
		{"percent_over_100_clamped", 150, 0, 100, 0},
		{"percent_negative_zeroed", -5, 0, 0, 0},
		{"percent_boundary_100", 100, 0, 100, 0},
		{"reserve_negative_zeroed", 10, -1, 10, 0},
		{"both_off", 0, 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.PredictionRisk.MaxStakePercent = c.inPct
			cfg.PredictionRisk.ReservePoints = c.inReserve
			ValidateConfig(&cfg)
			if cfg.PredictionRisk.MaxStakePercent != c.wantPct || cfg.PredictionRisk.ReservePoints != c.wantResv {
				t.Errorf("clamp(pct=%d, reserve=%d) = (%d, %d), want (%d, %d)",
					c.inPct, c.inReserve,
					cfg.PredictionRisk.MaxStakePercent, cfg.PredictionRisk.ReservePoints,
					c.wantPct, c.wantResv)
			}
		})
	}
}
