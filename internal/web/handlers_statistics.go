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
//	{ streamer, range, points:[{t,balance}], annotations:[{t,type,reason}],
//	  breakdown:[{reason,gained,count}], rawTruncated, chartDownsampled }
//
// The series is downsampled to maxChartPoints for display; use the export
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

	repo := s.analytics.Repository()
	raw, err := repo.GetPointSamples(streamer, start, end, maxHistoryRows)
	if err != nil {
		writeInternalError(w, "Failed to load points history")
		return
	}
	points := analytics.Downsample(raw, maxChartPoints)

	annotations, err := repo.GetAnnotationRecords(streamer, start, end)
	if err != nil {
		writeInternalError(w, "Failed to load annotations")
		return
	}

	// Betting summary for the SAME streamer/window as the series, so the
	// earnings donut's PREDICTION slice (a gross positive credit) can be shown
	// beside the stake risked, refunded, and net result — making the origin of
	// the positive prediction points explicit instead of an unexplained "Other".
	// Best-effort: a bet-history read failure must not fail the whole page, it
	// just omits the summary.
	var betSummary *analytics.BetSummary
	if bets, betErr := repo.GetBets(streamer, "", start, end); betErr == nil && len(bets) > 0 {
		bs := analytics.SummarizeBets(bets)
		betSummary = &bs
	}

	rawTruncated, chartDownsampled := historyFlags(len(raw), len(points), maxHistoryRows)
	writeJSONOK(w, analytics.PointsHistory{
		Streamer:         streamer,
		Range:            label,
		Points:           points,
		Annotations:      annotations,
		Breakdown:        analytics.BreakdownFromSamples(raw),
		BetSummary:       betSummary,
		RawTruncated:     rawTruncated,
		ChartDownsampled: chartDownsampled,
	})
}

// historyFlags derives the two independent completeness signals for a
// points-history response: rawTruncated means the raw series hit the backend
// row cap (the window — and thus the breakdown — is incomplete);
// chartDownsampled means the display series was merely thinned while the raw
// series, and the breakdown built from it, remain complete. They are
// deliberately separate: only rawTruncated may hide the breakdown or raise a
// partial-data warning.
func historyFlags(rawLen, pointsLen, rawCap int) (rawTruncated, chartDownsampled bool) {
	return rawLen >= rawCap, pointsLen < rawLen
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

	repo := s.analytics.Repository()
	points, err := repo.GetPointSamples(streamer, start, end, maxExportRows)
	if err != nil {
		writeInternalError(w, "Failed to load points history")
		return
	}
	annotations, err := repo.GetAnnotationRecords(streamer, start, end)
	if err != nil {
		writeInternalError(w, "Failed to load annotations")
		return
	}

	w.Header().Set("Content-Disposition", "attachment; filename=\""+streamer+"-points-"+label+".json\"")
	// The export is full-fidelity (never downsampled), so only the raw row
	// cap can make it incomplete.
	writeJSONOK(w, analytics.PointsHistory{
		Streamer:     streamer,
		Range:        label,
		Points:       points,
		Annotations:  annotations,
		RawTruncated: len(points) >= maxExportRows,
	})
}
