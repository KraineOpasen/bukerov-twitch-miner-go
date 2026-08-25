package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/app"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/logger"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/updater"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

var (
	configFile  = flag.String("config", "config.json", "Path to configuration file")
	debug       = flag.Bool("debug", false, "Enable debug logging")
	genConfig   = flag.Bool("generate-config", false, "Generate a sample configuration file")
	autoUpdate  = flag.Bool("auto-update", false, "Automatically download and apply new GitHub releases, then restart")
	healthcheck = flag.Bool("healthcheck", false, "Probe the running miner's dashboard and exit 0 (healthy) or 1 (unhealthy); used by the container HEALTHCHECK")
)

// main is the thin process entry point: it parses flags, handles the standalone
// healthcheck / generate-config modes, loads config, sets up logging and the
// signal-scoped context, and then delegates ALL service wiring and lifecycle to
// the internal/app composition root — Build (construct), Run (start + block),
// Shutdown (deterministic teardown). It builds no services itself.
func main() {
	flag.Parse()

	// Resolve the entire process-level runtime configuration ONCE, at the single
	// bootstrap boundary: CLI flags plus the environment (read only here, via
	// runtimeconfig.OSLookup) become one immutable, typed snapshot. No downstream
	// runtime component reads the environment for these inputs again.
	rc := runtimeconfig.Resolve(runtimeconfig.Flags{
		ConfigPath:     *configFile,
		Debug:          *debug,
		AutoUpdate:     *autoUpdate,
		GenerateConfig: *genConfig,
		Healthcheck:    *healthcheck,
	}, runtimeconfig.OSLookup)

	// A recreated stable container starts with the originally pinned image
	// binary. Before health checks, config access, database wiring, or mining,
	// replay any newer artifact that was already accepted into the persistent
	// /database cache and re-exec this same argv/env. AUTO_UPDATE=false stops
	// future discovery; it never silently downgrades an accepted runtime floor.
	if restored, err := updater.RecoverStable(version.Version, version.Channel, updater.DefaultStableCacheDir); err != nil {
		setupBasicLogger(rc.Debug)
		slog.Error("Stable update recovery failed", "executableRestored", restored, "error", err)
		os.Exit(1)
	}

	if *healthcheck {
		os.Exit(runHealthcheck(rc.ConfigPath, rc.Dashboard))
	}

	if *genConfig {
		setupBasicLogger(rc.Debug)
		generateSampleConfig()
		return
	}

	cfg, err := loadOrCreateConfig(rc.ConfigPath)
	if err != nil {
		setupBasicLogger(rc.Debug)
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	if cfg.Username == "" {
		setupBasicLogger(rc.Debug)
		slog.Error("Username is required in configuration")
		os.Exit(1)
	}

	if len(cfg.Streamers) == 0 {
		setupBasicLogger(rc.Debug)
		slog.Error("At least one streamer is required in configuration")
		os.Exit(1)
	}

	logSettings := cfg.Logger
	if rc.Debug {
		logSettings.ConsoleLevel = "DEBUG"
		logSettings.FileLevel = "DEBUG"
	}

	log, err := logger.Setup(cfg.StorageKey(), logSettings)
	if err != nil {
		setupBasicLogger(rc.Debug)
		slog.Error("Failed to setup logger", "error", err)
		os.Exit(1)
	}
	defer log.Close()

	slog.Info("Twitch Channel Points Miner", "version", version.Version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Build the whole service graph (database, analytics, web server, miner) in
	// one place from the immutable runtime snapshot. Build opens owned resources
	// and runs migrations but starts no runtime loops; a failure here has already
	// unwound anything it opened.
	application, err := app.Build(ctx, cfg, rc)
	if err != nil {
		slog.Error("Failed to build application", "error", err)
		os.Exit(1)
	}

	// Run starts the web listener and the miner, blocking until the signal
	// context is cancelled or a fatal runtime error occurs.
	runErr := application.Run(ctx)

	// Deterministic graceful teardown of the process-level resources, always on
	// a fresh context so shutdown proceeds even after the run context was
	// cancelled.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), app.DefaultShutdownTimeout)
	defer cancel()
	if serr := application.Shutdown(shutdownCtx); serr != nil {
		slog.Error("Graceful shutdown reported errors", "error", serr)
	}

	if runErr != nil {
		slog.Error("Miner error", "error", runErr)
		os.Exit(1)
	}
}

func setupBasicLogger(debug bool) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	slog.SetDefault(slog.New(handler))
}

func loadOrCreateConfig(path string) (*config.Config, error) {
	cfg, err := config.LoadConfig(path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Warn("Configuration file not found, creating sample", "path", path)
			return nil, fmt.Errorf("configuration file not found: %s. Run with -generate-config to create a sample", path)
		}
		return nil, err
	}
	return cfg, nil
}

func generateSampleConfig() {
	cfg := config.DefaultConfig()
	cfg.Username = "your_twitch_username"
	cfg.EnableAnalytics = true
	cfg.Priority = []config.Priority{
		config.PriorityStreak,
		config.PriorityDrops,
		config.PriorityOrder,
	}
	cfg.Streamers = []config.StreamerConfig{
		{
			Username: "streamer1",
		},
		{
			Username: "streamer2",
			Settings: &models.StreamerSettings{
				MakePredictions: true,
				FollowRaid:      true,
				ClaimDrops:      true,
				ClaimMoments:    true,
				WatchStreak:     true,
				CommunityGoals:  false,
				Chat:            models.ChatOnline,
				Bet: models.BetSettings{
					Strategy:      models.StrategySmart,
					Percentage:    5,
					PercentageGap: 20,
					MaxPoints:     50000,
					MinimumPoints: 0,
					StealthMode:   false,
					Delay:         6,
					DelayMode:     models.DelayModeFromEnd,
				},
			},
		},
	}

	if err := config.SaveConfig("config.sample.json", &cfg); err != nil {
		slog.Error("Failed to save sample configuration", "error", err)
		os.Exit(1)
	}

	slog.Info("Sample configuration generated", "path", "config.sample.json")
	fmt.Println("\nSample configuration saved to config.sample.json")
	fmt.Println("Rename it to config.json and update with your settings")
}
