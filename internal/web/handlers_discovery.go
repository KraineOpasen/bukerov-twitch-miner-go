package web

import (
	"log/slog"
	"net/http"
	"sort"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/discovery"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/util"
)

// handleAPIDiscovery renders the discovered-channels partial for the Drops
// page (htmx auto-refreshes it the same way as the campaign queue).
func (s *Server) handleAPIDiscovery(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	provider := s.discoveryProvider
	s.mu.RUnlock()

	var state discovery.State
	if provider != nil {
		state = provider.State()
	}

	data := buildDiscoveryView(state)

	w.Header().Set("Content-Type", "text/html")
	tmpl := s.templates["partials"]
	if tmpl == nil {
		writeInternalError(w, "Partials not loaded")
		return
	}
	if err := tmpl.ExecuteTemplate(w, "discovery_list", data); err != nil {
		slog.Error("Failed to render discovery list", "error", err)
		writeInternalError(w, "Failed to render")
	}
}

// buildDiscoveryView orders the pool the way the slot actually picks from it
// (watching first, then available by viewer count, offline last) and formats
// the counters for display.
func buildDiscoveryView(state discovery.State) DiscoveryListData {
	data := DiscoveryListData{
		Enabled: state.Enabled,
		Games:   state.Games,
	}

	statusRank := map[string]int{"watching": 0, "available": 1, "offline": 2}

	channels := append([]discovery.ChannelState(nil), state.Channels...)
	sort.SliceStable(channels, func(i, j int) bool {
		if statusRank[channels[i].Status] != statusRank[channels[j].Status] {
			return statusRank[channels[i].Status] < statusRank[channels[j].Status]
		}
		return channels[i].Viewers > channels[j].Viewers
	})

	for _, ch := range channels {
		view := DiscoveredChannelView{
			Login:             ch.Login,
			Game:              ch.Game,
			Status:            ch.Status,
			ViewersFormatted:  util.FormatNumber(ch.Viewers),
			Watching:          ch.Status == "watching",
			Offline:           ch.Status == "offline",
			MinutesWatched:    int(ch.MinutesWatched),
			HasMinutesWatched: ch.Status == "watching" && ch.MinutesWatched >= 1,
		}
		data.Channels = append(data.Channels, view)
	}

	return data
}
