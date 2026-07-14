package web

import (
	"encoding/json"
	"net/http"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
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
		var newSettings settings.RuntimeSettings
		if err := json.NewDecoder(r.Body).Decode(&newSettings); err != nil {
			writeBadRequest(w, "Invalid JSON: "+err.Error())
			return
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
