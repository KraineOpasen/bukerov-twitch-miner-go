package web

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	s.mu.RUnlock()

	data := SettingsPageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
	}
	s.renderPage(w, r, "settings.html", data)
}

func (s *Server) handleAPISettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if s.settingsProvider == nil {
			writeServiceUnavailable(w, "Settings not available")
			return
		}
		currentSettings := s.settingsProvider.GetRuntimeSettings()
		writeJSONOK(w, currentSettings)
		return
	}

	if r.Method == http.MethodPost {
		if s.settingsProvider == nil {
			writeServiceUnavailable(w, "Settings not available")
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeBadRequest(w, "Failed to read request body: "+err.Error())
			return
		}

		// Decode ONTO the current settings, not onto a zero value: the apply
		// path (settings.ApplyToConfig) replaces the config wholesale, so a
		// partial body used to zero every omitted field — dropping all
		// streamers when "streamers" was absent, resetting logger/analytics
		// blocks, and letting ValidateConfig silently clamp zeroed intervals.
		// Seeding gives merge semantics: an absent key keeps its current
		// value, a present key (including an explicit empty list) replaces it.
		newSettings := s.settingsProvider.GetRuntimeSettings()

		// One exception: a slice of structs must not be decoded "over" its
		// seeded elements. encoding/json resets the slice length to zero and
		// appends, which reuses the retained backing array — a posted element
		// would then inherit leftover fields (including the per-streamer
		// Settings pointer) from whatever previously sat at its index. Clear
		// the seed and restore it only when the key was genuinely absent.
		seededStreamers := newSettings.Streamers
		newSettings.Streamers = nil

		if err := json.Unmarshal(body, &newSettings); err != nil {
			writeBadRequest(w, "Invalid JSON: "+err.Error())
			return
		}

		// The struct unmarshal above succeeded, so the body is a JSON object
		// and this probe cannot fail.
		var probe map[string]json.RawMessage
		_ = json.Unmarshal(body, &probe)
		if _, present := probe["streamers"]; !present {
			newSettings.Streamers = seededStreamers
		}

		if s.onSettingsUpdate != nil {
			s.onSettingsUpdate(newSettings)
		}

		s.mu.Lock()
		s.refresh = newSettings.Analytics.Refresh
		s.daysAgo = newSettings.Analytics.DaysAgo
		s.mu.Unlock()

		writeSuccess(w)
		return
	}

	writeNotAllowed(w)
}

func (s *Server) handleAPISettingsReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}

	if s.settingsProvider == nil {
		writeServiceUnavailable(w, "Settings not available")
		return
	}

	defaults := s.settingsProvider.GetDefaultSettings()

	if s.onSettingsUpdate != nil {
		s.onSettingsUpdate(defaults)
	}

	s.mu.Lock()
	s.refresh = defaults.Analytics.Refresh
	s.daysAgo = defaults.Analytics.DaysAgo
	s.mu.Unlock()

	writeJSONOK(w, defaults)
}
