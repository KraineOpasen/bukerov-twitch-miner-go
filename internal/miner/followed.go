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
//
// The whole read-modify-write is serialized under importMu: GetRuntimeSettings
// reads under mu.RLock and ApplySettings writes under mu.Lock as two separate
// acquisitions, so without importMu two concurrent imports could both read the
// same pre-write snapshot and the second ApplySettings (a wholesale replace of
// the streamer list) would drop the first's additions. importMu does not cover
// the broader whole-object POST /api/settings path — that pre-existing
// last-write-wins concern is out of scope here.
func (m *Miner) ImportStreamers(logins []string) (int, error) {
	m.importMu.Lock()
	defer m.importMu.Unlock()

	cur := m.GetRuntimeSettings()
	merged, added := mergeStreamerLogins(cur.Streamers, logins)
	if added == 0 {
		return 0, nil
	}
	cur.Streamers = merged

	// ApplySettings resolves channel IDs for the new logins, subscribes their
	// pubsub topics, and persists config.json — the same path the Settings page
	// uses to add a streamer. importApply is a test-only override.
	apply := m.ApplySettings
	if m.importApply != nil {
		apply = m.importApply
	}
	apply(cur)
	return added, nil
}

// mergeStreamerLogins appends logins not already present in existing (matched
// case-insensitively, and deduped within logins itself) as default-settings
// entries, returning the merged list and how many were newly added. It is pure
// so the dedup contract is unit-testable independently of the apply side effects.
func mergeStreamerLogins(existing []settings.StreamerConfig, logins []string) (merged []settings.StreamerConfig, added int) {
	seen := make(map[string]bool, len(existing))
	for _, sc := range existing {
		seen[strings.ToLower(strings.TrimSpace(sc.Username))] = true
	}

	merged = make([]settings.StreamerConfig, len(existing))
	copy(merged, existing)

	for _, login := range logins {
		l := strings.ToLower(strings.TrimSpace(login))
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		// No Settings pointer → the streamer inherits the global defaults.
		merged = append(merged, settings.StreamerConfig{Username: l})
		added++
	}
	return merged, added
}
