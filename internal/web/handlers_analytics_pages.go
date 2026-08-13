package web

// Task S5-8: the Analytics group's two direct-render routes (7-8).
//
//   - /analytics/points (route 7) — the canonical points-history page.
//   - /analytics/roi    (route 8) — the canonical prediction-ROI page.
//
// Both replace their former /analytics/* -> /statistics compatibility
// redirects (see handlers_chrome.go's compatibilityRedirects, now holding
// only the unrelated /help entry). /analytics itself is deliberately NOT
// registered: the Analytics root is not a page, so it keeps falling through
// to the "/" catch-all and 404s honestly, exactly like any other unbuilt
// path.
//
// These are deliberately THIN handlers. Routes 7-8 add no backend surface
// whatsoever: no new API endpoint, no new provider, no new transport, and no
// new viewmodel — both pages reuse the existing StatisticsPageData that
// handleStatisticsPage already builds, and both read the existing, unchanged
// JSON endpoints client-side (/api/points-history and /api/predictions/roi,
// plus their full-fidelity JSON exports). The visible CSV each page offers is
// generated in the browser from that same authoritative JSON (see the shared
// c14.chart component), never from a second server-side representation that
// could disagree with it.
//
// The split between the two pages follows the seams that already existed in
// statistics.html — it shipped two entirely independent client IIFEs, one per
// concern — so the canonical presentation could be built without editing that
// template at all. Legacy /statistics keeps rendering directly and byte-for-
// byte unchanged, exactly like /health, /logs and /settings after S5-5/S5-6;
// it is simply no longer the destination the C2 nav points Analytics at.

import (
	"net/http"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

// analyticsPageData builds the shared page data for routes 7-8. It is the
// SAME StatisticsPageData the legacy page uses, populated from the same two
// sources: the current configured roster (never analytics.ListStreamers, so a
// removed streamer's retained history never lingers in a picker — see
// handleStatisticsPage) and the strategies that actually appear in recorded
// bets. A bet-strategy read failure degrades to an empty strategy filter
// rather than failing the page.
func (s *Server) analyticsPageData() StatisticsPageData {
	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	s.mu.RUnlock()

	var strategies []string
	if s.analytics != nil {
		if strats, err := s.analytics.Repository().DistinctBetStrategies(); err == nil {
			strategies = strats
		}
	}

	return StatisticsPageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
		Streamers:      s.configuredStreamerNames(),
		BetStrategies:  strategies,
	}
}

// handleAnalyticsPointsPage renders /analytics/points (route 7).
//
// The page requires a CONCRETE streamer: the points series is per-streamer
// and /api/points-history rejects an empty streamer with 400, so no "All
// streamers" facet is offered — an aggregate option could only be honoured by
// inventing a cross-streamer series the miner never recorded.
func (s *Server) handleAnalyticsPointsPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	s.renderPage(w, r, "analytics_points.html", s.analyticsPageData())
}

// handleAnalyticsROIPage renders /analytics/roi (route 8).
//
// Strictly read-only: the page renders prediction outcomes and never places,
// skips or configures a bet. /settings/predictions remains the sole owner of
// betting behavior and is linked from the page rather than duplicated on it.
func (s *Server) handleAnalyticsROIPage(w http.ResponseWriter, r *http.Request) {
	if !requireGetOrHead(w, r) {
		return
	}
	s.renderPage(w, r, "analytics_roi.html", s.analyticsPageData())
}
