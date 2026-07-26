package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
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
		streamers = append(streamers, st.GetUsername())
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

	s.renderPage(w, r, "notifications.html", data)
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
		// Discord-only path; err here is a static/DB-derived message with no
		// M5/M6 material, but it is still not for the network response — log
		// server-side and return a static message.
		slog.Error("Failed to send test notifications", "error", err)
		writeInternalError(w, "Failed to send test notifications")
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

	results := sanitizeProviderTestResults(notifMgr.TestAllProviders(r.Context()))

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

// percentEscapePattern matches any %-escape triplet (e.g. a URL-encoded
// secret component like %3A or %2F), so the fail-closed check below also
// catches encoded forms that slip past a plain substring match.
var percentEscapePattern = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)

// sanitizeProviderTestResults is the LAST barrier before provider test
// results reach the network. internal/notifications already returns fully
// classified, URL-free errors (see notifications.SendError), but this
// endpoint is reachable WITHOUT authentication whenever Basic Auth is
// unconfigured (server.go's basicAuthMiddleware wiring) and without a CSRF
// token for header-less requests (security.go's checkSameOrigin allowance),
// so it does not simply trust the DTO: it copies ONLY whitelisted fields and,
// as a fail-closed backstop, replaces any Error text that still looks
// URL/credential-shaped. Do not remove this as "redundant" with the
// notifications-package sanitization — it is an intentionally independent
// second layer for exactly the case where that layer regresses.
func sanitizeProviderTestResults(in []notifications.ProviderTestResult) []notifications.ProviderTestResult {
	out := make([]notifications.ProviderTestResult, len(in))
	for i, r := range in {
		out[i] = notifications.ProviderTestResult{
			Provider: r.Provider,
			OK:       r.OK,
			Error:    r.Error,
			Stage:    r.Stage,
			Class:    r.Class,
			Status:   r.Status,
		}
		if looksLikeLeakedCredential(out[i].Error) {
			out[i].Error = "delivery failed"
		}
	}
	return out
}

// looksLikeLeakedCredential fails closed: any of these substrings/patterns in
// a provider-test error message is treated as still possibly URL- or
// credential-bearing, even though a conforming provider should never produce
// one. Matching is intentionally broad (it may reject a legitimate-looking
// message) because the cost of a false positive here is a generic "delivery
// failed" string, while the cost of a false negative is a leaked secret.
func looksLikeLeakedCredential(s string) bool {
	lower := strings.ToLower(s)
	if strings.Contains(lower, "://") ||
		strings.Contains(lower, "token=") ||
		strings.Contains(lower, "token%3d") ||
		strings.Contains(s, "@") {
		return true
	}
	return percentEscapePattern.MatchString(s)
}
