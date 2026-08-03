package web

import (
	"net/http"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

// compatibilityRedirects lists the additive, temporary 302 compatibility
// routes for S5-2 (task §E): a not-yet-migrated wireframe route that
// redirects to the existing canonical page it maps onto today. None of these
// are new pages — every target already renders directly via its own
// existing route. This is a one-way, forward-only map (wireframe -> canon);
// S5-2 never redirects a legacy canonical route anywhere.
var compatibilityRedirects = map[string]string{
	"/drops/current":                 "/drops",
	"/drops/upcoming":                "/drops",
	"/drops/past":                    "/drops",
	"/analytics/points":              "/statistics",
	"/analytics/roi":                 "/statistics",
	"/settings/streamers":            "/settings",
	"/settings/rotation":             "/settings",
	"/settings/drops":                "/settings",
	"/settings/predictions":          "/settings",
	"/settings/chat-raids":           "/settings",
	"/settings/transport":            "/settings",
	"/settings/analytics-logging":    "/settings",
	"/settings/events-notifications": "/settings",
	"/settings/discord":              "/settings",
	"/settings/system":               "/settings",
	"/system/status":                 "/health",
	"/system/diagnostics":            "/health",
	"/system/logs":                   "/logs",
	"/help":                          "/help/getting-started",
}

// handleOverviewPage renders the additive /overview compatibility route
// through the exact same Overview rendering pipeline as the legacy "/" root
// (handlers_dashboard.go's handleDashboard). Duplicated here rather than
// factored out of that file, since handlers_dashboard.go sits outside this
// slice's allowed paths and its own "/" behavior must stay untouched.
func (s *Server) handleOverviewPage(w http.ResponseWriter, r *http.Request) {
	data := s.buildOverviewData(s.langFromRequest(r))
	s.renderPage(w, r, "overview.html", data)
}

// handleEventsPage renders the minimal /events compatibility landing
// (OD-S5-2-1 item 2): always reachable as one of the seven top-level
// sections regardless of Discord configuration, but its content honestly
// varies by Discord availability rather than implementing the future event
// journal (deferred to S5-7).
func (s *Server) handleEventsPage(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	s.mu.RUnlock()

	data := EventsPageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
	}
	if !discordEnabled {
		lang := s.langFromRequest(r)
		data.DisabledState = StateBlockData{
			State:        "EMPTY",
			Variant:      "block",
			Message:      s.i18n.T(lang, "events.disabled_text"),
			ActionLabel:  s.i18n.T(lang, "events.link_settings"),
			ActionTarget: "/settings",
		}
	}
	s.renderPage(w, r, "events.html", data)
}

// handleHelpGettingStarted renders the minimal /help/getting-started
// compatibility landing (OD-S5-2-1 item 1): orientation to the seven live
// sections only, no glossary or troubleshooting content (deferred to S5-9).
func (s *Server) handleHelpGettingStarted(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	s.mu.RUnlock()

	data := HelpPageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
	}
	s.renderPage(w, r, "help.html", data)
}

// redirectCompat returns a handler that 302-redirects GET/HEAD requests to
// target, preserving the request's raw query string exactly; any other
// method is rejected with 405 (Allow: GET, HEAD). Used only for the additive
// temporary compatibility routes in compatibilityRedirects.
func redirectCompat(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		loc := target
		if r.URL.RawQuery != "" {
			loc += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, loc, http.StatusFound)
	}
}
