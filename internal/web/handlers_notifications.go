package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

func (s *Server) handleNotificationsPage(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	notifMgr := s.notificationManager
	s.mu.RUnlock()

	if !discordEnabled {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	var streamers []string
	for _, st := range s.streamers {
		streamers = append(streamers, st.Username)
	}

	configValid := true
	configError := ""
	if notifMgr != nil {
		configValid, configError = notifMgr.IsConfigValid()
	}

	data := NotificationsPageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
		ConfigValid:    configValid,
		ConfigError:    configError,
		Streamers:      streamers,
	}

	s.renderPage(w, "notifications.html", data)
}

func (s *Server) handleAPINotificationsConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	notifMgr := s.notificationManager
	s.mu.RUnlock()

	if notifMgr == nil {
		writeServiceUnavailable(w, "Notifications not available")
		return
	}

	if r.Method == http.MethodGet {
		cfg, err := notifMgr.GetConfig()
		if err != nil {
			writeInternalError(w, "Failed to get config")
			return
		}
		writeJSONOK(w, cfg)
		return
	}

	if r.Method == http.MethodPost {
		var cfg notifications.NotificationConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeBadRequest(w, "Invalid JSON: "+err.Error())
			return
		}

		if err := notifMgr.SaveConfig(&cfg); err != nil {
			writeInternalError(w, "Failed to save config")
			return
		}

		writeSuccess(w)
		return
	}

	writeNotAllowed(w)
}

func (s *Server) handleAPINotificationsChannels(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	notifMgr := s.notificationManager
	s.mu.RUnlock()

	if notifMgr == nil {
		writeServiceUnavailable(w, "Notifications not available")
		return
	}

	forceRefresh := r.URL.Query().Get("refresh") == "1"
	channels, err := notifMgr.GetDiscordChannels(context.Background(), forceRefresh)
	if err != nil {
		writeInternalError(w, "Failed to get channels: "+err.Error())
		return
	}

	writeJSONOK(w, channels)
}

func (s *Server) handleAPINotificationsPoints(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	notifMgr := s.notificationManager
	s.mu.RUnlock()

	if notifMgr == nil {
		writeServiceUnavailable(w, "Notifications not available")
		return
	}

	if r.Method == http.MethodGet {
		rules, err := notifMgr.GetPointRules()
		if err != nil {
			writeInternalError(w, "Failed to get rules")
			return
		}
		writeJSONOK(w, rules)
		return
	}

	if r.Method == http.MethodPost {
		var rule notifications.PointRule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			writeBadRequest(w, "Invalid JSON: "+err.Error())
			return
		}

		if err := notifMgr.AddPointRule(&rule); err != nil {
			writeInternalError(w, "Failed to add rule")
			return
		}

		writeJSONOK(w, rule)
		return
	}

	writeNotAllowed(w)
}

func (s *Server) handleAPINotificationsPointsDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeNotAllowed(w)
		return
	}

	s.mu.RLock()
	notifMgr := s.notificationManager
	s.mu.RUnlock()

	if notifMgr == nil {
		writeServiceUnavailable(w, "Notifications not available")
		return
	}

	idStr := strings.TrimPrefix(r.URL.Path, "/api/notifications/points/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeBadRequest(w, "Invalid ID")
		return
	}

	if err := notifMgr.DeletePointRule(id); err != nil {
		writeInternalError(w, "Failed to delete rule")
		return
	}

	writeSuccess(w)
}

func (s *Server) handleAPINotificationsTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}

	s.mu.RLock()
	notifMgr := s.notificationManager
	s.mu.RUnlock()

	if notifMgr == nil {
		writeServiceUnavailable(w, "Notifications not available")
		return
	}

	sent, err := notifMgr.SendTestNotifications()
	if err != nil {
		writeInternalError(w, "Failed to send test notifications: "+err.Error())
		return
	}

	writeJSONOK(w, map[string]int{"sent": sent})
}

// handleAPITestNotification sends a test message to every enabled provider
// (Discord plus all configured push providers), bypassing event filters and
// batching. It responds with a per-provider status so the caller can see which
// providers delivered successfully and which failed.
func (s *Server) handleAPITestNotification(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}

	s.mu.RLock()
	notifMgr := s.notificationManager
	s.mu.RUnlock()

	if notifMgr == nil {
		writeServiceUnavailable(w, "Notifications not available")
		return
	}

	results := notifMgr.TestAllProviders(r.Context())

	// "ok" when every provider succeeded, "partial" when at least one failed,
	// and still "ok" (with an explanatory message) when nothing is enabled.
	status := "ok"
	for _, res := range results {
		if !res.OK {
			status = "partial"
			break
		}
	}

	resp := map[string]any{
		"status":    status,
		"providers": results,
	}
	if len(results) == 0 {
		resp["message"] = "no providers enabled"
	}

	writeJSONOK(w, resp)
}
