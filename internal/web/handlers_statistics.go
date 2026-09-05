package web

import (
	"net/http"
	"sort"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

const (
	// maxHistoryRows caps how many raw point rows the history endpoint fetches
	// per request (memory/timeout guard); the series is downsampled below this
	// for the chart. The export endpoint uses maxExportRows for full fidelity.
	maxHistoryRows = 20000
	maxExportRows  = 200000

	// maxChartPoints bounds the number of samples returned to the chart so wide
	// ranges stay responsive; the raw series is uniformly downsampled to this.
	maxChartPoints = 2000
)

// rangeWindow maps a range preset to its lookback duration and canonical label.
// An absent range means the page default — 24h, matching the UI's initial
// selection. Unknown values fall back to 7d. maxWindow (30d) bounds any query.
func rangeWindow(r string) (time.Duration, string) {
	switch r {
	case "24h", "1d", "":
		return 24 * time.Hour, "24h"
	case "30d":
		return 30 * 24 * time.Hour, "30d"
	case "7d":
		return 7 * 24 * time.Hour, "7d"
	default:
		return 7 * 24 * time.Hour, "7d"
	}
}

// handleStatisticsPage renders the dedicated Statistics page: a full-width
// points-history chart with a streamer selector and range presets. The streamer
// list is sourced from the analytics repo (persisted history), sorted by name.
func (s *Server) handleStatisticsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/statistics" {
		http.NotFound(w, r)
		return
	}

	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	s.mu.RUnlock()

	// The streamer selector lists only the current configured roster, never the
	// analytics history. A streamer removed from settings keeps its persisted
	// points/bet rows (history is never destroyed), so ListStreamers — which
	// returns every channel that ever recorded a point — would keep the removed
	// name in the dropdown, and, because the first <option> is the browser
	// default with no `selected`, could even make it the active selection. The
	// roster is what the operator currently tracks and is updated at runtime via
	// AttachStreamers, so add/remove takes effect without a restart. Removed
	// streamers' history stays queryable and exportable by direct URL/streamer
	// parameter; it is only dropped from the picker.
	names := s.configuredStreamerNames()

	var strategies []string
	if s.analytics != nil {
		if strats, err := s.analytics.Repository().DistinctBetStrategies(); err == nil {
			strategies = strats
		}
	}

	data := StatisticsPageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
		Streamers:      names,
		BetStrategies:  strategies,
	}
	s.renderPage(w, r, "statistics.html", data)
}

// configuredStreamerNames returns the current configured roster's usernames,
// sorted and de-duplicated, for the statistics/ROI streamer selectors. It reads
// the roster under the same lock AttachStreamers writes it, so a runtime
// add/remove is reflected without a restart. Deliberately sourced from the
// roster, not analytics.ListStreamers, so a removed streamer's retained history
// never lingers in the picker (see handleStatisticsPage).
func (s *Server) configuredStreamerNames() []string {
	s.mu.RLock()
	roster := s.streamers
	s.mu.RUnlock()

	seen := make(map[string]struct{}, len(roster))
	names := make([]string, 0, len(roster))
	for _, st := range roster {
		if st == nil || st.GetUsername() == "" {
			continue
		}
		if _, dup := seen[st.GetUsername()]; dup {
			continue
		}
		seen[st.GetUsername()] = struct{}{}
		names = append(names, st.GetUsername())
	}
	sort.Strings(names)
	return names
}

// handleAPIPointsHistory returns the balance series + event annotations for one
// streamer over a range preset (24h/7d/30d). Response:
//
//	{ streamer, range, points:[{t,balance,reason,exact}], annotations:[{t,type,reason}],
//	  breakdown:[{reason,gained,count}], exactBreakdown:[...], legacyBreakdown:[...],
//	  earnings:{coverage,exact,exactSince,legacyStatus}, rawTruncated, chartDownsampled }
//
// exactBreakdown is the authoritative accounting (the exact point-event
// aggregation, present when the window holds a positive exact event;
// earnings.exact is true for any exact event, positive or not);
// legacyBreakdown is the explicit balance-delta estimate for the history no
// exact event covers, never added to the exact figures (see
// analytics.ComposeEarnings). breakdown is the compatibility attribution of
// the first release (analytics.BreakdownFromSamples over the raw series),
// kept unchanged for existing consumers and not canonical accounting. The
// series is downsampled to maxChartPoints for display; use the export
// endpoint for full fidelity. Auth is inherited from the global middleware.
func (s *Server) handleAPIPointsHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeNotAllowed(w)
		return
	}
	if s.analytics == nil {
		writeServiceUnavailable(w, "Analytics not available")
		return
	}

	streamer := r.URL.Query().Get("streamer")
	if streamer == "" {
		writeBadRequest(w, "streamer is required")
		return
	}

	window, label := rangeWindow(r.URL.Query().Get("range"))
	end := time.Now()
	start := end.Add(-window)

	// Everything the response presents together — the balance series, its
	// markers, the exact ledger aggregate and the bets behind the PREDICTION
	// slice — comes from ONE database snapshot, so a point event committing
	// during the request is in every component or in none. The transaction
	// is over before anything below runs; encoding holds no database state.
	repo := s.analytics.Repository()
	rowCap := rowCapOrDefault(s.historyRowCap, maxHistoryRows)
	snap, err := repo.PointsSnapshotBetween(r.Context(), streamer, start, end, rowCap, true)
	if err != nil {
		writeInternalError(w, "Failed to load points history")
		return
	}
	raw := snap.Samples
	points := analytics.Downsample(raw, maxChartPoints)

	// Betting summary for the SAME streamer/window as the series, so the
	// earnings donut's PREDICTION slice (a gross positive credit) can be shown
	// beside the stake risked, refunded, and net result — making the origin of
	// the positive prediction points explicit instead of an unexplained "Other".
	// Best-effort: a bet-history read failure must not fail the whole page, it
	// just omits the summary (the snapshot leaves Bets nil in that case).
	var betSummary *analytics.BetSummary
	if len(snap.Bets) > 0 {
		bs := analytics.SummarizeBets(snap.Bets)
		betSummary = &bs
	}

	// Exact earnings come from the point-event ledger, aggregated in SQL over
	// the same window: the event-local amounts Twitch granted, independent of
	// the raw balance series and of its row cap. The legacy balance-delta
	// estimate covers only the samples no exact event backs, and is
	// unavailable — never silently zero — when the raw series was truncated.
	// The compatibility breakdown is the first release's attribution over the
	// same raw series (truncated or not), independent of both accountings.
	rawTruncated, chartDownsampled := historyFlags(len(raw), len(points), rowCap)
	exactBreakdown, legacyBreakdown, earnings := analytics.ComposeEarnings(snap.Exact, analytics.EstimateLegacyBreakdown(raw), rawTruncated)

	writeJSONOK(w, analytics.PointsHistory{
		Streamer:         streamer,
		Range:            label,
		Points:           points,
		Annotations:      snap.Annotations,
		Breakdown:        analytics.BreakdownFromSamples(raw),
		ExactBreakdown:   exactBreakdown,
		LegacyBreakdown:  legacyBreakdown,
		Earnings:         earnings,
		BetSummary:       betSummary,
		RawTruncated:     rawTruncated,
		ChartDownsampled: chartDownsampled,
	})
}

// historyFlags derives the two independent completeness signals for a
// points-history response: rawTruncated means the raw series hit the backend
// row cap, so the balance-derived KPIs and the legacy estimate are
// unavailable (the exact breakdown is aggregated from the ledger in SQL and
// does not depend on the cap); chartDownsampled means the display series was
// merely thinned while the raw series remains complete. They are deliberately
// separate: only rawTruncated may withhold figures or raise a partial-data
// warning.
func historyFlags(rawLen, pointsLen, rawCap int) (rawTruncated, chartDownsampled bool) {
	return rawLen >= rawCap, pointsLen < rawLen
}

// rowCapOrDefault normalises a per-Server row cap: the zero value (a Server
// built without NewServer/NewServerEarly) falls back to the production cap
// rather than meaning "unlimited" to the query and "always truncated" to the
// completeness flags.
func rowCapOrDefault(rowCap, fallback int) int {
	if rowCap <= 0 {
		return fallback
	}
	return rowCap
}

// handleAPIPointsHistoryExport returns the same data as handleAPIPointsHistory
// but at full fidelity (no downsampling) and as a downloadable attachment, for
// external tools (Grafana/Plotly). Same filters and auth.
func (s *Server) handleAPIPointsHistoryExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeNotAllowed(w)
		return
	}
	if s.analytics == nil {
		writeServiceUnavailable(w, "Analytics not available")
		return
	}

	streamer := r.URL.Query().Get("streamer")
	if streamer == "" {
		writeBadRequest(w, "streamer is required")
		return
	}

	window, label := rangeWindow(r.URL.Query().Get("range"))
	end := time.Now()
	start := end.Add(-window)

	// One database snapshot for the series, its markers and the exact
	// aggregate, exactly as the history endpoint: the export is a coherent
	// point in time, and the transaction is over before encoding starts.
	repo := s.analytics.Repository()
	rowCap := rowCapOrDefault(s.exportRowCap, maxExportRows)
	snap, err := repo.PointsSnapshotBetween(r.Context(), streamer, start, end, rowCap, false)
	if err != nil {
		writeInternalError(w, "Failed to load points history")
		return
	}
	points := snap.Samples

	// The export carries the same earnings accounting as the history
	// endpoint so an external consumer never sees an empty accounting block:
	// the exact aggregation from the ledger, the legacy estimate over the
	// exported samples, the same coverage metadata, and the same
	// compatibility breakdown over the exported series (additive on this
	// endpoint — the first release's export carried no breakdown — so it is
	// parity with the history endpoint, not a preserved figure). The export
	// is full-fidelity (never downsampled), so only the raw row cap can make
	// it incomplete.
	rawTruncated := len(points) >= rowCap
	exactBreakdown, legacyBreakdown, earnings := analytics.ComposeEarnings(snap.Exact, analytics.EstimateLegacyBreakdown(points), rawTruncated)

	w.Header().Set("Content-Disposition", "attachment; filename=\""+streamer+"-points-"+label+".json\"")
	writeJSONOK(w, analytics.PointsHistory{
		Streamer:        streamer,
		Range:           label,
		Points:          points,
		Annotations:     snap.Annotations,
		Breakdown:       analytics.BreakdownFromSamples(points),
		ExactBreakdown:  exactBreakdown,
		LegacyBreakdown: legacyBreakdown,
		Earnings:        earnings,
		RawTruncated:    rawTruncated,
	})
}
