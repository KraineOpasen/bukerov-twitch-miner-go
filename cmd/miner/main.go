package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/logger"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/miner"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/updater"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"
)

var (
	configFile  = flag.String("config", "config.json", "Path to configuration file")
	debug       = flag.Bool("debug", false, "Enable debug logging")
	genConfig   = flag.Bool("generate-config", false, "Generate a sample configuration file")
	autoUpdate  = flag.Bool("auto-update", false, "Automatically download and apply new GitHub releases, then restart")
	healthcheck = flag.Bool("healthcheck", false, "Probe the running miner's dashboard and exit 0 (healthy) or 1 (unhealthy); used by the container HEALTHCHECK")
)

func main() {
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck(*configFile))
	}

	if *genConfig {
		setupBasicLogger(*debug)
		generateSampleConfig()
		return
	}

	cfg, err := loadOrCreateConfig(*configFile)
	if err != nil {
		setupBasicLogger(*debug)
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	if cfg.Username == "" {
		setupBasicLogger(*debug)
		slog.Error("Username is required in configuration")
		os.Exit(1)
	}

	if len(cfg.Streamers) == 0 {
		setupBasicLogger(*debug)
		slog.Error("At least one streamer is required in configuration")
		os.Exit(1)
	}

	logSettings := cfg.Logger
	if *debug {
		logSettings.ConsoleLevel = "DEBUG"
		logSettings.FileLevel = "DEBUG"
	}

	log, err := logger.Setup(cfg.Username, logSettings)
	if err != nil {
		setupBasicLogger(*debug)
		slog.Error("Failed to setup logger", "error", err)
		os.Exit(1)
	}
	defer log.Close()

	slog.Info("Twitch Channel Points Miner", "version", version.Version)

	var analyticsSvc *analytics.Service
	var webServer *web.Server
	var db *database.DB
	if cfg.EnableAnalytics {
		dbBasePath := filepath.Join("database", cfg.Username)
		if err := os.MkdirAll(dbBasePath, 0755); err != nil {
			slog.Error("Failed to create database directory", "error", err)
			os.Exit(1)
		}
		db, err = database.Open(dbBasePath)
		if err != nil {
			slog.Error("Failed to open database", "error", err)
			os.Exit(1)
		}
		defer func() { _ = db.Close() }()

		analyticsSvc, err = analytics.NewService(db, dbBasePath, cfg.Analytics.RetentionDays)
		if err != nil {
			slog.Error("Failed to create analytics service", "error", err)
			os.Exit(1)
		}

		webServer = web.NewServerEarly(cfg.Analytics, cfg.Username, dbBasePath, analyticsSvc)
		if webServer != nil {
			// Fail-closed: a non-loopback bind without credentials aborts
			// startup instead of silently serving an open dashboard.
			if err := webServer.Start(); err != nil {
				slog.Error("Refusing to start", "error", err)
				os.Exit(1)
			}
			defer webServer.Stop()
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := miner.New(cfg, *configFile)
	m.ConfigureAutoUpdate(autoUpdateEnabled(), updater.ParseCheckInterval(os.Getenv("AUTO_UPDATE_CHECK_INTERVAL")))
	if analyticsSvc != nil {
		m.SetAnalyticsService(analyticsSvc)
	}
	if webServer != nil {
		m.SetWebServer(webServer)
	}
	if err := m.Run(ctx); err != nil {
		slog.Error("Miner error", "error", err)
		os.Exit(1)
	}
}

// autoUpdateEnabled resolves whether self-update is on. The -auto-update flag
// takes precedence; otherwise the AUTO_UPDATE env var (true/1/yes) enables it,
// which is how the Docker image opts in without changing the entrypoint.
func autoUpdateEnabled() bool {
	if *autoUpdate {
		return true
	}
	if v, err := strconv.ParseBool(os.Getenv("AUTO_UPDATE")); err == nil {
		return v
	}
	return false
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
