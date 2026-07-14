package web

import (
	"net/http"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
)

// roiPeriodWindow maps a ROI period preset to its lookback duration and canonical
// label. "lifetime" returns a zero duration, meaning an open-ended (all-time)
// query. Unknown values fall back to 30d.
func roiPeriodWindow(p string) (time.Duration, string) {
	switch p {
	case "7d":
		return 7 * 24 * time.Hour, "7d"
	case "90d":
		return 90 * 24 * time.Hour, "90d"
	case "lifetime", "all":
		return 0, "lifetime"
	case "30d", "":
		return 30 * 24 * time.Hour, "30d"
	default:
		return 30 * 24 * time.Hour, "30d"
	}
}

// roiQuery resolves the shared streamer/strategy/period filters from the request.
// An empty streamer or strategy means "all". start is zero for the lifetime
// period (open-ended).
func roiQuery(r *http.Request) (streamer, strategy string, start, end time.Time, label string) {
	streamer = r.URL.Query().Get("streamer")
	strategy = r.URL.Query().Get("strategy")
	window, label := roiPeriodWindow(r.URL.Query().Get("period"))
	end = time.Now()
	if window > 0 {
		start = end.Add(-window)
	}
	return streamer, strategy, start, end, label
}

// handleAPIPredictionsROI returns the ROI summary for the selected filters:
//
//	GET /api/predictions/roi?streamer=&strategy=&period=7d|30d|90d|lifetime
//
// streamer/strategy are optional (empty = all). The response is a fully computed
// analytics.ROISummary (counts, win rate, wagered, net profit, ROI, averages,
// max drawdown, and by-streamer/by-strategy/by-odds-bucket breakdowns). It is
// read-only and never places or changes a bet.
func (s *Server) handleAPIPredictionsROI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeNotAllowed(w)
		return
	}
	if s.analytics == nil {
		writeServiceUnavailable(w, "Analytics not available")
		return
	}

	streamer, strategy, start, end, label := roiQuery(r)

	bets, err := s.analytics.Repository().GetBets(streamer, strategy, start, end)
	if err != nil {
		writeInternalError(w, "Failed to load prediction bets")
		return
	}

	summary := analytics.ComputeROI(bets)
	summary.Streamer = streamer
	summary.Strategy = strategy
	summary.Period = label

	writeJSONOK(w, summary)
}

// handleAPIPredictionsROIExport returns the raw resolved bets for the selected
// filters as a downloadable JSON attachment, for external analysis. Same filters
// and auth as the summary endpoint.
func (s *Server) handleAPIPredictionsROIExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeNotAllowed(w)
		return
	}
	if s.analytics == nil {
		writeServiceUnavailable(w, "Analytics not available")
		return
	}

	streamer, strategy, start, end, label := roiQuery(r)

	bets, err := s.analytics.Repository().GetBets(streamer, strategy, start, end)
	if err != nil {
		writeInternalError(w, "Failed to load prediction bets")
		return
	}
	if bets == nil {
		bets = []analytics.BetRecord{}
	}

	name := streamer
	if name == "" {
		name = "all"
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"-roi-"+label+".json\"")
	writeJSONOK(w, map[string]interface{}{
		"streamer": streamer,
		"strategy": strategy,
		"period":   label,
		"bets":     bets,
	})
}
