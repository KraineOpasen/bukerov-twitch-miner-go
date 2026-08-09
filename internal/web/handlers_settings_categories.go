package web

import (
	"net/http"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

// S5-6 Settings category direct routes (13-22): replace the ten former
// /settings/* compatibility redirects (see handlers_chrome.go's
// compatibilityRedirects, task S5-2) with real direct-render pages, the same
// "additive direct route replaces a compatibility redirect" shape S5-4/S5-5
// already used for Drops and System. The legacy /settings mega-form
// (handlers_settings.go) is untouched and keeps rendering directly at its
// own route — it is simply no longer linked from the C2 nav (see
// c2_nav.html), exactly like /health and /logs after S5-5.
//
// Every page here is a thin, mostly-static shell: current settings are
// fetched client-side from the SAME GET /api/settings (or, for routes 20/21,
// GET /api/notifications/config and GET /api/notifications/points) every
// other settings surface already uses, and a save POSTs back a payload
// containing ONLY that category's own top-level keys — never a new
// endpoint, DTO, or backend semantic. handlers_settings.go's beginSettingsTxn
// read-modify-write already merges a partial body onto the current settings
// at any nesting depth (a JSON key absent from the body leaves the
// corresponding field, however deeply nested, at its current value), so a
// scoped payload here can never clobber a field owned by a different
// category page.
//
// Route 22 (System) is the one exception: it reuses the existing Health P3
// canary/watchdog partial (GET /api/health, POST /api/health/settings)
// verbatim, so healthFormMu/ApplyHealthSettings semantics are preserved
// exactly rather than re-routed through the C6/C7/C8 engine.
//
// requireGetOrHead is the ONLY page-route method contract in this package
// that actually rejects non-GET/HEAD requests (see handlers_drops.go's
// doc comment on why the S5-4/S5-5 pages deliberately don't) — task S5-6
// requires it explicitly, so every category route below opens with it.
func requireGetOrHead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	return false
}

// settingsCategoryPageData builds the common shell data every category page
// needs — identical to handleSettingsPage's own SettingsPageData, reused
// as-is since no category page needs additional server-rendered fields (all
// of them fetch their own scoped settings slice client-side).
func (s *Server) settingsCategoryPageData() SettingsPageData {
	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	s.mu.RUnlock()

	return SettingsPageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
	}
}

func (s *Server) handleSettingsStreamersPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	s.renderPage(w, r, "settings_streamers.html", s.settingsCategoryPageData())
}

func (s *Server) handleSettingsRotationPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	s.renderPage(w, r, "settings_rotation.html", s.settingsCategoryPageData())
}

func (s *Server) handleSettingsDropsPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	s.renderPage(w, r, "settings_drops.html", s.settingsCategoryPageData())
}

func (s *Server) handleSettingsPredictionsPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	s.renderPage(w, r, "settings_predictions.html", s.settingsCategoryPageData())
}

func (s *Server) handleSettingsChatRaidsPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	s.renderPage(w, r, "settings_chat_raids.html", s.settingsCategoryPageData())
}

func (s *Server) handleSettingsTransportPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	s.renderPage(w, r, "settings_transport.html", s.settingsCategoryPageData())
}

func (s *Server) handleSettingsAnalyticsLoggingPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	s.renderPage(w, r, "settings_analytics_logging.html", s.settingsCategoryPageData())
}

func (s *Server) handleSettingsEventsNotificationsPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}

	base := s.settingsCategoryPageData()
	s.mu.RLock()
	notifMgr := s.notificationManager
	s.mu.RUnlock()

	configValid := true
	if notifMgr != nil {
		configValid, _ = notifMgr.IsConfigValid()
	}

	data := EventsNotificationsPageData{
		Username:       base.Username,
		RefreshMinutes: base.RefreshMinutes,
		Version:        base.Version,
		DiscordEnabled: base.DiscordEnabled,
		DebugURL:       base.DebugURL,
		ConfigValid:    configValid,
	}
	s.renderPage(w, r, "settings_events_notifications.html", data)
}

func (s *Server) handleSettingsDiscordPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	s.renderPage(w, r, "settings_discord.html", s.settingsCategoryPageData())
}

func (s *Server) handleSettingsSystemPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	s.renderPage(w, r, "settings_system.html", s.settingsCategoryPageData())
}
