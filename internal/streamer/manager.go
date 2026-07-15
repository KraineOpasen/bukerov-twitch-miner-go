package streamer

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// ProgressCallback is called during loading to report progress.
type ProgressCallback func(current, total int, username string)

// Manager handles loading, storing, and updating streamers.
type Manager struct {
	client   *api.TwitchClient
	defaults models.StreamerSettings

	streamers []*models.Streamer
	mu        sync.RWMutex
}

// NewManager creates a new streamer manager.
func NewManager(client *api.TwitchClient, defaults models.StreamerSettings) *Manager {
	return &Manager{
		client:   client,
		defaults: defaults,
	}
}

// LoadFromConfig loads streamers from configuration.
// Returns an error if no valid streamers are found.
func (m *Manager) LoadFromConfig(configs []config.StreamerConfig, onProgress ProgressCallback) error {
	slog.Info("Loading streamers", "count", len(configs))

	total := len(configs)
	for i, sc := range configs {
		if onProgress != nil {
			onProgress(i+1, total, sc.Username)
		}

		settings := m.defaults
		if sc.Settings != nil {
			settings = *sc.Settings
		}

		streamer := models.NewStreamer(strings.ToLower(sc.Username), settings)

		channelID, err := m.client.GetChannelID(streamer.Username)
		if err != nil {
			// A stale persisted-query hash (PersistedQueryNotFound) is a
			// temporary Twitch-side outage, not a missing channel — don't call it
			// "not found".
			if errors.Is(err, api.ErrPersistedQueryNotFound) {
				slog.Warn("Could not resolve channel ID: stale Twitch query metadata (not a missing channel); skipping for now",
					"username", sc.Username, "error", err)
			} else {
				slog.Warn("Streamer not found, skipping", "username", sc.Username, "error", err)
			}
			continue
		}
		streamer.ChannelID = channelID

		if err := m.client.LoadChannelPointsContext(streamer); err != nil {
			// The streamer is kept regardless of this error, and its points are
			// left untouched (LoadChannelPointsContext only writes points on a
			// successful read). A PersistedQueryNotFound here must never be
			// reported as "streamer does not exist".
			if errors.Is(err, api.ErrPersistedQueryNotFound) {
				slog.Warn("Channel points context unavailable (stale Twitch query metadata); keeping streamer with last-known points",
					"streamer", streamer.Username, "error", err)
			} else {
				slog.Warn("Failed to load channel points", "streamer", streamer.Username, "error", err)
			}
		}

		m.mu.Lock()
		m.streamers = append(m.streamers, streamer)
		m.mu.Unlock()

		slog.Info("Loaded streamer",
			"username", streamer.Username,
			"channelID", streamer.ChannelID,
			"points", streamer.GetChannelPoints(),
		)
	}

	if len(m.streamers) == 0 {
		return fmt.Errorf("no valid streamers found")
	}

	return nil
}

// All returns all loaded streamers.
func (m *Manager) All() []*models.Streamer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.streamers
}

// Count returns the number of loaded streamers.
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.streamers)
}

// Get returns a streamer by username (case-insensitive).
func (m *Manager) Get(username string) *models.Streamer {
	m.mu.RLock()
	defer m.mu.RUnlock()

	lower := strings.ToLower(username)
	for _, s := range m.streamers {
		if s.Username == lower {
			return s
		}
	}
	return nil
}

// Names returns a list of all streamer usernames.
func (m *Manager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, len(m.streamers))
	for i, s := range m.streamers {
		names[i] = s.Username
	}
	return names
}

// PointsMap returns a map of streamer usernames to their current points.
func (m *Manager) PointsMap() map[string]int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	points := make(map[string]int, len(m.streamers))
	for _, s := range m.streamers {
		points[s.Username] = s.GetChannelPoints()
	}
	return points
}

// ApplySettings updates settings for streamers based on config.
// Returns lists of added and removed streamers.
func (m *Manager) ApplySettings(configs []config.StreamerConfig, defaults models.StreamerSettings) (added, removed []*models.Streamer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.defaults = defaults

	configMap := make(map[string]config.StreamerConfig)
	for _, sc := range configs {
		configMap[strings.ToLower(sc.Username)] = sc
	}

	existingMap := make(map[string]*models.Streamer)
	for _, s := range m.streamers {
		existingMap[s.Username] = s
	}

	for _, streamer := range m.streamers {
		if sc, ok := configMap[streamer.Username]; ok {
			if sc.Settings != nil {
				streamer.SetSettings(*sc.Settings)
			} else {
				streamer.SetSettings(defaults)
			}
		}
	}

	for username := range configMap {
		if _, exists := existingMap[username]; !exists {
			sc := configMap[username]
			settings := defaults
			if sc.Settings != nil {
				settings = *sc.Settings
			}

			streamer := models.NewStreamer(username, settings)
			channelID, err := m.client.GetChannelID(streamer.Username)
			if err != nil {
				if errors.Is(err, api.ErrPersistedQueryNotFound) {
					slog.Warn("Could not add streamer yet: stale Twitch query metadata (not a missing channel); will retry on next settings apply",
						"username", username, "error", err)
				} else {
					slog.Warn("Failed to add streamer", "username", username, "error", err)
				}
				continue
			}
			streamer.ChannelID = channelID

			if err := m.client.LoadChannelPointsContext(streamer); err != nil {
				if errors.Is(err, api.ErrPersistedQueryNotFound) {
					slog.Warn("Channel points context unavailable for new streamer (stale Twitch query metadata); keeping streamer",
						"streamer", username, "error", err)
				} else {
					slog.Warn("Failed to load channel points for new streamer", "streamer", username, "error", err)
				}
			}

			m.streamers = append(m.streamers, streamer)
			added = append(added, streamer)
			slog.Info("Added new streamer", "username", username, "channelID", channelID)
		}
	}

	var remaining []*models.Streamer
	for _, streamer := range m.streamers {
		if _, ok := configMap[streamer.Username]; ok {
			remaining = append(remaining, streamer)
		} else {
			removed = append(removed, streamer)
			slog.Info("Removed streamer", "username", streamer.Username)
		}
	}
	m.streamers = remaining

	return added, removed
}

// CheckOnlineStatus checks the online status for all streamers.
func (m *Manager) CheckOnlineStatus() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, streamer := range m.streamers {
		m.client.CheckStreamerOnline(streamer)
	}
}

// PrintReport logs a session report for all streamers.
func (m *Manager) PrintReport() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	slog.Info("=== Session Report ===")

	for _, streamer := range m.streamers {
		slog.Info("Streamer stats",
			"username", streamer.Username,
			"points", streamer.GetChannelPoints(),
		)

		for reason, entry := range streamer.History {
			if entry.Counter > 0 || entry.Amount != 0 {
				slog.Info("  History",
					"reason", reason,
					"count", entry.Counter,
					"amount", entry.Amount,
				)
			}
		}
	}
}
