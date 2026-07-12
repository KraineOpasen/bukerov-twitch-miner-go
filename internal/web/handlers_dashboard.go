package web

import (
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/PatrickWalther/twitch-miner-go/internal/models"
	"github.com/PatrickWalther/twitch-miner-go/internal/util"
	"github.com/PatrickWalther/twitch-miner-go/internal/version"
)

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	repo := s.analytics.Repository()
	streamers, err := repo.ListStreamers()
	if err != nil {
		slog.Error("Failed to list streamers", "error", err)
		writeInternalError(w, "Internal error")
		return
	}

	totalPoints := 0
	pointsToday := 0
	todayStart := time.Now().Truncate(24 * time.Hour)

	for _, info := range streamers {
		totalPoints += info.Points

		data, err := repo.GetStreamerData(info.Name)
		if err != nil {
			continue
		}
		todayStartMS := todayStart.UnixMilli()
		for i := len(data.Series) - 1; i >= 0; i-- {
			if data.Series[i].X < todayStartMS {
				if i+1 < len(data.Series) {
					pointsToday += info.Points - data.Series[i+1].Y
				}
				break
			}
			if i == 0 && len(data.Series) > 0 {
				pointsToday += info.Points - data.Series[0].Y
			}
		}
	}

	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	s.mu.RUnlock()

	data := DashboardData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		TotalPoints:    util.FormatNumber(totalPoints),
		StreamerCount:  len(streamers),
		PointsToday:    util.FormatNumber(pointsToday),
		DiscordEnabled: discordEnabled,
	}

	s.renderPage(w, "dashboard.html", data)
}

func (s *Server) handleStreamerPage(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/streamer/")
	if name == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	repo := s.analytics.Repository()
	data, err := repo.GetStreamerData(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	currentPoints := 0
	if len(data.Series) > 0 {
		currentPoints = data.Series[len(data.Series)-1].Y
	}

	s.mu.RLock()
	refresh := s.refresh
	daysAgo := s.daysAgo
	discordEnabled := s.discordEnabled
	s.mu.RUnlock()

	startTS := time.Now().AddDate(0, 0, -daysAgo).UnixMilli()
	pointsGained := 0
	for i, p := range data.Series {
		if p.X >= startTS {
			if i > 0 {
				pointsGained = currentPoints - data.Series[i-1].Y
			} else {
				pointsGained = currentPoints - p.Y
			}
			break
		}
	}

	pageData := StreamerPageData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		Streamer: StreamerInfo{
			Name:            name,
			Points:          currentPoints,
			PointsFormatted: util.FormatNumber(currentPoints),
		},
		PointsGained:   util.FormatNumber(pointsGained),
		DataPoints:     len(data.Series),
		DaysAgo:        daysAgo,
		DiscordEnabled: discordEnabled,
	}

	s.renderPage(w, "streamer.html", pageData)
}

func (s *Server) handleAPIStreamers(w http.ResponseWriter, r *http.Request) {
	repo := s.analytics.Repository()
	repoStreamers, err := repo.ListStreamers()
	if err != nil {
		writeInternalError(w, "Failed to list streamers")
		return
	}

	streamers := convertStreamerInfoList(repoStreamers)

	streamerMap := make(map[string]*models.Streamer)
	configOrder := make(map[string]int)
	for i, st := range s.streamers {
		streamerMap[st.Username] = st
		configOrder[st.Username] = i
	}

	var trackedLive, trackedOffline, untracked []StreamerInfo

	for i := range streamers {
		if st, ok := streamerMap[streamers[i].Name]; ok {
			streamers[i].IsLive = st.GetIsOnline()
			streamers[i].Preference = string(st.GetSettings().Preference)
			if streamers[i].IsLive {
				streamers[i].LiveDuration = util.FormatDuration(time.Since(st.GetOnlineAt()))
				streamers[i].GameName = st.Stream.GameName()
				streamers[i].Title = st.Stream.GetTitle()
				streamers[i].ViewersCount = st.Stream.GetViewersCount()
				streamers[i].ViewersCountFormatted = util.FormatNumber(streamers[i].ViewersCount)
				streamers[i].ChannelRestrictedDrop = st.HasChannelRestrictedCampaign()
				trackedLive = append(trackedLive, streamers[i])
			} else {
				offlineAt := st.GetOfflineAt()
				if !offlineAt.IsZero() {
					streamers[i].OfflineDuration = util.FormatDuration(time.Since(offlineAt))
				}
				trackedOffline = append(trackedOffline, streamers[i])
			}
		} else {
			untracked = append(untracked, streamers[i])
		}
	}

	sort.Slice(trackedLive, func(i, j int) bool {
		return configOrder[trackedLive[i].Name] < configOrder[trackedLive[j].Name]
	})
	sort.Slice(trackedOffline, func(i, j int) bool {
		return configOrder[trackedOffline[i].Name] < configOrder[trackedOffline[j].Name]
	})
	sort.Slice(untracked, func(i, j int) bool {
		return untracked[i].Name < untracked[j].Name
	})

	gridData := StreamerGridData{
		TrackedLive:    trackedLive,
		TrackedOffline: trackedOffline,
		Untracked:      untracked,
	}

	w.Header().Set("Content-Type", "text/html")
	tmpl := s.templates["partials"]
	if tmpl == nil {
		writeInternalError(w, "Partials not loaded")
		return
	}
	if err := tmpl.ExecuteTemplate(w, "streamer_grid", gridData); err != nil {
		slog.Error("Failed to render streamer grid", "error", err)
		writeInternalError(w, "Failed to render")
	}
}
