package miner

import (
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

// FollowedChannels implements web.FollowedProvider: the channels the
// authenticated user follows (login + display name), paginated up to the API's
// cap. truncated is true when the cap was hit with more available.
func (m *Miner) FollowedChannels() ([]api.FollowedChannel, bool, error) {
	m.mu.RLock()
	client := m.client
	m.mu.RUnlock()
	if client == nil {
		return nil, false, nil
	}
	return client.GetFollowedChannels()
}

// TrackedUsernames implements web.FollowedProvider: the lowercase logins already
// in the configured streamer list, so the import picker can mark and skip them.
func (m *Miner) TrackedUsernames() []string {
	cur := m.GetRuntimeSettings()
	out := make([]string, 0, len(cur.Streamers))
	for _, sc := range cur.Streamers {
		if login := strings.ToLower(strings.TrimSpace(sc.Username)); login != "" {
			out = append(out, login)
		}
	}
	return out
}

// ImportStreamers implements web.FollowedProvider: append the given logins to
// the tracked streamer list with default settings (no per-streamer overrides),
// skipping any already tracked, then run the standard settings-apply path so the
// new channels are subscribed and persisted to config.json. Returns how many
// new entries were added.
func (m *Miner) ImportStreamers(logins []string) (int, error) {
	cur := m.GetRuntimeSettings()

	existing := make(map[string]bool, len(cur.Streamers))
	for _, sc := range cur.Streamers {
		existing[strings.ToLower(strings.TrimSpace(sc.Username))] = true
	}

	added := 0
	for _, login := range logins {
		l := strings.ToLower(strings.TrimSpace(login))
		if l == "" || existing[l] {
			continue
		}
		existing[l] = true
		// No Settings pointer → the streamer inherits the global defaults.
		cur.Streamers = append(cur.Streamers, settings.StreamerConfig{Username: l})
		added++
	}

	if added == 0 {
		return 0, nil
	}

	// ApplySettings resolves channel IDs for the new logins, subscribes their
	// pubsub topics, and persists config.json — the same path the Settings page
	// uses to add a streamer.
	m.ApplySettings(cur)
	return added, nil
}
