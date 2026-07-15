package web

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/util"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	// The redesigned Overview renders its live content (header stats, ticker,
	// predictions board, streamer grid) from the /api/overview partial on
	// load; the page shell just needs the chrome data.
	data := s.buildOverviewData(s.langFromRequest(r))
	s.renderPage(w, r, "overview.html", data)
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
	debugURL := s.debugURL
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
		DebugURL:       debugURL,
	}

	s.renderPage(w, r, "streamer.html", pageData)
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
				if prog := st.ActiveCampaignProgress(); prog != nil {
					streamers[i].HasCampaign = true
					streamers[i].CampaignName = prog.CampaignName
					streamers[i].CampaignDropName = prog.DropName
					streamers[i].CampaignPercent = prog.Percent
					if prog.MinutesRequired > 0 {
						streamers[i].CampaignMinutesInfo = fmt.Sprintf("%d/%d min", prog.MinutesWatched, prog.MinutesRequired)
					}
				}
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

	s.renderPartial(w, r, "streamer_grid", gridData)
}
