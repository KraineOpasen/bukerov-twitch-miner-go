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
//
// task S5-4 removed /drops/current, /drops/upcoming and /drops/past from
// this map: each is now its own direct-render route registered in server.go
// (handlers_drops.go), not a redirect to /drops. /drops itself remains a
// compatibility alias for Current through the same handler — see
// handleDropsPage — but that alias is not a 302 redirect, so it was never a
// member of this map.
//
// task S5-5 removed /system/status, /system/diagnostics and /system/logs
// from this map: each is now its own direct-render route registered in
// server.go (handlers_system.go), not a redirect to /health or /logs.
//
// task S5-6 removed all ten /settings/* entries from this map: each is now
// its own direct-render route registered in server.go
// (handlers_settings_categories.go), not a redirect to /settings. Legacy
// /settings (the mega-form) keeps rendering directly, unchanged — it is
// simply no longer linked from the C2 nav.
//
// task S5-8 removed the last two entries that were not /help: /analytics/points
// and /analytics/roi are now their own direct-render routes registered in
// server.go (handlers_analytics_pages.go), not redirects to /statistics.
// Legacy /statistics keeps rendering directly, byte-for-byte unchanged — it is
// simply no longer the destination the C2 nav points Analytics at. The map is
// now down to its final single entry.
var compatibilityRedirects = map[string]string{
	"/help": "/help/getting-started",
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

// handleEventsPage moved to handlers_events.go in task S5-7: /events is no
// longer the minimal S5-2 compatibility landing but the real session-scoped
// event journal, joined by the /events/browser, /events/sound and
// /events/discord direct routes registered in server.go.

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
