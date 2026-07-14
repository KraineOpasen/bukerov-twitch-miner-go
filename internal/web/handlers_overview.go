package web

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/util"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

// statsTTL bounds how often the Overview recomputes per-streamer
// analytics-derived figures (points-today / points-per-hour) from SQLite, so
// the 30s live poll doesn't add DB load beyond what the old dashboard did.
const statsTTL = 60 * time.Second

// handleAPIOverview renders the live Overview content partial (header stats,
// events ticker, live-predictions board and the streamer grid) plus an
// out-of-band update for the sidebar "Now Watching" block. Everything is built
// from in-memory state; the only SQLite reads (points-today / per-hour) are
// memoised behind statsTTL.
func (s *Server) handleAPIOverview(w http.ResponseWriter, r *http.Request) {
	data := s.buildOverviewData()

	s.renderPartial(w, r, "overview_live", data)
}

// handleAPINowWatching renders just the pinned sidebar "Now Watching" block.
// It's polled from every page's sidebar, so it's kept cheap: in-memory watch
// state plus the memoised per-streamer stats, no fresh SQLite reads beyond the
// shared cache.
func (s *Server) handleAPINowWatching(w http.ResponseWriter, r *http.Request) {
	streamers := s.snapshotStreamers()
	stats, _, _ := s.ensureStats(streamers)

	s.mu.RLock()
	provider := s.overviewProvider
	s.mu.RUnlock()

	status := s.status.GetStatus()
	var slots WatchSlotsView
	if provider != nil {
		slots = provider.WatchSlots()
	}
	if slots.Watching == nil {
		slots.Watching = map[string]bool{}
	}
	view := s.buildNowWatching(streamers, slots, stats, status.ConnectionLost)

	s.renderPartial(w, r, "now_watching", view)
}

// snapshotStreamers returns a stable slice of the tracked streamers under the
// read lock, so the rest of the assembly runs lock-free.
func (s *Server) snapshotStreamers() []*models.Streamer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*models.Streamer, len(s.streamers))
	copy(out, s.streamers)
	return out
}

// ensureStats refreshes the cached per-streamer analytics figures if the cache
// is older than statsTTL. Returns the cache map, total points and today total.
func (s *Server) ensureStats(streamers []*models.Streamer) (map[string]streamerStats, int, int) {
	s.mu.RLock()
	fresh := s.statsCache != nil && time.Since(s.statsAt) < statsTTL
	cache := s.statsCache
	s.mu.RUnlock()

	total, today := 0, 0
	if fresh {
		for _, st := range streamers {
			if cs, ok := cache[st.Username]; ok {
				today += cs.pointsToday
			}
			total += st.GetChannelPoints()
		}
		return cache, total, today
	}

	repo := s.analytics.Repository()
	newCache := make(map[string]streamerStats, len(streamers))
	todayStart := time.Now().Truncate(24 * time.Hour).UnixMilli()
	hoursToday := time.Since(time.Now().Truncate(24 * time.Hour)).Hours()

	for _, st := range streamers {
		points := st.GetChannelPoints()
		total += points

		cs := streamerStats{}
		data, err := repo.GetStreamerData(st.Username)
		if err == nil && len(data.Series) > 0 {
			base := -1
			for i := len(data.Series) - 1; i >= 0; i-- {
				if data.Series[i].X < todayStart {
					base = data.Series[i].Y
					break
				}
			}
			if base < 0 {
				base = data.Series[0].Y // all points are from today
			}
			cs.pointsToday = points - base
			if cs.pointsToday < 0 {
				cs.pointsToday = 0
			}
			if hoursToday >= 0.5 {
				cs.pointsPerHour = int(float64(cs.pointsToday) / hoursToday)
				cs.hasRate = true
			}
		}
		newCache[st.Username] = cs
		today += cs.pointsToday
	}

	s.mu.Lock()
	s.statsCache = newCache
	s.statsAt = time.Now()
	s.mu.Unlock()

	return newCache, total, today
}

// buildOverviewData assembles the full Overview view model from in-memory
// state (streamers, watcher slots, pool predictions, the events ring) plus the
// memoised analytics figures.
func (s *Server) buildOverviewData() OverviewData {
	streamers := s.snapshotStreamers()
	stats, total, today := s.ensureStats(streamers)

	s.mu.RLock()
	refresh := s.refresh
	discordEnabled := s.discordEnabled
	debugURL := s.debugURL
	provider := s.overviewProvider
	s.mu.RUnlock()

	status := s.status.GetStatus()

	var slots WatchSlotsView
	var predictions []LivePrediction
	if provider != nil {
		slots = provider.WatchSlots()
		predictions = provider.LivePredictions()
	}
	if slots.Watching == nil {
		slots.Watching = map[string]bool{}
	}

	// Which streamers have a live prediction (for the card marker).
	predByStreamer := make(map[string]bool, len(predictions))
	for _, p := range predictions {
		predByStreamer[p.Streamer] = true
	}

	live, offline, untracked, ticker := s.buildCards(streamers, slots, stats, predByStreamer)

	data := OverviewData{
		Username:       s.username,
		RefreshMinutes: refresh,
		Version:        version.Version,
		DiscordEnabled: discordEnabled,
		DebugURL:       debugURL,
		BotStatus:      string(status.Status),
		BotStatusLabel: botStatusLabel(status.Status),
		Connected:      status.Status == StatusRunning && !status.ConnectionLost && !status.ReauthRequired,
		Stale:          status.ConnectionLost,
		ReauthRequired: status.ReauthRequired,
		ConnectionLost: status.ConnectionLost,
		TotalPoints:    util.FormatNumber(total),
		StreamerCount:  len(streamers),
		LiveCount:      len(live),
		PointsToday:    util.FormatNumber(today),
		Ticker:         ticker,
		Predictions:    buildPredictionViews(predictions),
		NowWatching:    s.buildNowWatching(streamers, slots, stats, status.ConnectionLost),
		TrackedLive:    live,
		TrackedOffline: offline,
		Untracked:      untracked,
		GeneratedUnix:  time.Now().Unix(),
	}
	return data
}

func botStatusLabel(status MinerStatus) string {
	switch status {
	case StatusRunning:
		return "Running"
	case StatusError:
		return "Error"
	case StatusAuthRequired, StatusAuthWaiting:
		return "Login required"
	case StatusLoadingStreamers:
		return "Loading"
	default:
		return "Starting"
	}
}

// buildCards enriches every tracked/untracked streamer into an Overview card
// and collects ticker items (active community goals). Ordering mirrors the old
// dashboard: live first (config order), then offline, then untracked.
func (s *Server) buildCards(
	streamers []*models.Streamer,
	slots WatchSlotsView,
	stats map[string]streamerStats,
	predByStreamer map[string]bool,
) (live, offline, untracked []StreamerInfo, ticker []TickerItem) {
	watching := slots.Watching

	for _, st := range streamers {
		settings := st.GetSettings()
		online := st.GetIsOnline()
		card := StreamerInfo{
			Name:            st.Username,
			Points:          st.GetChannelPoints(),
			PointsFormatted: util.FormatNumber(st.GetChannelPoints()),
			IsLive:          online,
			Preference:      string(settings.Preference),
			DisableWatch:    settings.DisableWatch,
		}

		if cs, ok := stats[st.Username]; ok {
			card.PointsToday = util.FormatNumber(cs.pointsToday)
			if cs.hasRate {
				card.PointsPerHour = util.FormatNumber(cs.pointsPerHour)
			}
		}

		// Last notable event for this streamer from the in-memory ring.
		if text, ago := lastEventFor(st.Username); text != "" {
			card.LastEventText = text
			card.LastEventAgo = ago
		}

		// Active community goal (also feeds the ticker).
		if goals := st.ActiveCommunityGoals(); len(goals) > 0 {
			g := goals[0]
			card.HasGoal = true
			card.GoalTitle = g.Title
			card.GoalPercent = g.Percent
			ticker = append(ticker, TickerItem{
				Streamer: st.Username,
				Kind:     "goal",
				Label:    g.Title,
				Percent:  g.Percent,
				HasPct:   true,
			})
		}

		card.HasActivePrediction = predByStreamer[st.Username]

		if online {
			card.LiveDuration = util.FormatDuration(time.Since(st.GetOnlineAt()))
			card.GameName = st.Stream.GameName()
			card.Title = st.Stream.GetTitle()
			card.ViewersCount = st.Stream.GetViewersCount()
			card.ViewersCountFormatted = util.FormatNumber(card.ViewersCount)
			card.ChannelRestrictedDrop = st.HasChannelRestrictedCampaign()

			// Watch-streak progress (0..7 min) toward the streak bonus.
			if settings.WatchStreak && st.Stream.GetWatchStreakMissing() {
				mins := int(st.Stream.GetMinuteWatched())
				card.StreakPending = true
				card.StreakMinutes = mins
				pct := (mins * 100) / watchStreakThresholdMinutes
				if pct > 100 {
					pct = 100
				}
				card.StreakPercent = pct
			}

			// Drop progress: only when farming (active campaign + measurable).
			if settings.ClaimDrops {
				if prog := st.ActiveCampaignProgress(); prog != nil {
					card.HasCampaign = true
					card.CampaignName = prog.CampaignName
					card.CampaignDropName = prog.DropName
					card.CampaignPercent = prog.Percent
					if prog.MinutesRequired > 0 {
						card.CampaignMinutesInfo = fmt.Sprintf("%d/%d min", prog.MinutesWatched, prog.MinutesRequired)
					}
				}
			}

			card.WatchReason = slots.Reason[st.Username]
			switch {
			case settings.DisableWatch:
				card.State = "disabled"
			case watching[st.Username]:
				card.State = "watching"
				card.Watching = true
			default:
				card.State = "queued"
				card.Queued = true
			}
			live = append(live, card)
		} else {
			if off := st.GetOfflineAt(); !off.IsZero() {
				card.OfflineDuration = util.FormatDuration(time.Since(off))
			}
			if settings.DisableWatch {
				card.State = "disabled"
			} else {
				card.State = "offline"
			}
			offline = append(offline, card)
		}
	}

	// Sort ticker by completion desc so the most interesting goals lead.
	sort.SliceStable(ticker, func(i, j int) bool { return ticker[i].Percent > ticker[j].Percent })
	return live, offline, untracked, ticker
}

const watchStreakThresholdMinutes = 7

// manualMinBet is Twitch's minimum prediction stake, mirrored on the dashboard
// so the UI never offers a bet the backend would reject. Kept in sync with the
// pool's minPredictionBet.
const manualMinBet = 10

// fmtSeconds renders a seconds count as m:ss for prediction countdowns.
func fmtSeconds(sec int) string {
	if sec < 0 {
		sec = 0
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

// lastEventFor returns a short human summary + relative time of the most recent
// event recorded for the given streamer, or ("","") if none.
func lastEventFor(username string) (text, ago string) {
	for _, e := range events.Recent(200) {
		if !strings.EqualFold(e.Streamer, username) {
			continue
		}
		return eventLabel(e), util.FormatDuration(time.Since(e.Time)) + " ago"
	}
	return "", ""
}

// eventLabel maps an event to a compact card label.
func eventLabel(e events.Event) string {
	label := map[events.Type]string{
		events.TypeStreamerOnline:  "Went live",
		events.TypeStreamerOffline: "Went offline",
		events.TypeBonusClaimed:    "Bonus claimed",
		events.TypePointsEarned:    "Points earned",
		events.TypeBetPlaced:       "Bet placed",
		events.TypeBetResult:       "Bet result",
		events.TypeDropClaimed:     "Drop claimed",
		events.TypeMomentClaimed:   "Moment claimed",
		events.TypeRaidJoined:      "Raid joined",
		events.TypeRewardRedeemed:  "Reward redeemed",
	}[e.Type]
	if label == "" {
		label = string(e.Type)
	}
	if e.Detail != "" {
		return label + " · " + e.Detail
	}
	return label
}

// buildNowWatching builds the pinned sidebar "Now Watching" block from the
// active watch slots.
func (s *Server) buildNowWatching(
	streamers []*models.Streamer,
	slots WatchSlotsView,
	stats map[string]streamerStats,
	stale bool,
) NowWatchingView {
	byName := make(map[string]*models.Streamer, len(streamers))
	for _, st := range streamers {
		byName[st.Username] = st
	}

	view := NowWatchingView{Mode: slots.Mode, Stale: stale}

	// Watched streamers first (stable by ActivePair order, then any extras).
	seen := map[string]bool{}
	order := append([]string(nil), slots.ActivePair...)
	for name := range slots.Watching {
		found := false
		for _, n := range order {
			if n == name {
				found = true
				break
			}
		}
		if !found {
			order = append(order, name)
		}
	}

	for _, name := range order {
		if !slots.Watching[name] {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		slot := WatchSlotView{Name: name, Origin: slots.Origin[name]}

		st := byName[name]
		if st == nil {
			// A discovery-occupied slot: the channel is not on the configured
			// streamer list, so only the name, its game, and the discovery
			// badge are available (no points/streak/gain history).
			slot.Game = slots.Games[name]
			view.Slots = append(view.Slots, slot)
			continue
		}

		slot.Points = util.FormatNumber(st.GetChannelPoints())
		slot.Game = st.Stream.GameName()
		if st.GetSettings().WatchStreak && st.Stream.GetWatchStreakMissing() {
			mins := int(st.Stream.GetMinuteWatched())
			slot.StreakPending = true
			slot.StreakMinutes = mins
			pct := (mins * 100) / watchStreakThresholdMinutes
			if pct > 100 {
				pct = 100
			}
			slot.StreakPercent = pct
		}
		if cs, ok := stats[name]; ok && cs.hasRate {
			slot.HasGain = true
			slot.GainPerHour = util.FormatNumber(cs.pointsPerHour)
		}
		view.Slots = append(view.Slots, slot)
	}

	// Queued = online, not watched, not disabled.
	for _, st := range streamers {
		if !st.GetIsOnline() || slots.Watching[st.Username] || st.GetSettings().DisableWatch {
			continue
		}
		view.QueuedNames = append(view.QueuedNames, st.Username)
	}

	if !slots.NextRotationAt.IsZero() && len(view.Slots) > 0 {
		view.HasNextRotation = true
		view.NextRotationUnix = slots.NextRotationAt.Unix()
	}
	return view
}

// buildPredictionViews maps live predictions to board view models.
func buildPredictionViews(preds []LivePrediction) []PredictionView {
	views := make([]PredictionView, 0, len(preds))
	now := time.Now()
	for _, p := range preds {
		secondsLeft := int(p.PredictionWindowSeconds - now.Sub(p.CreatedAt).Seconds())
		if secondsLeft < 0 {
			secondsLeft = 0
		}
		locked := p.Status == string(models.PredictionLocked)
		// A manual bet is offered only when the round is genuinely bettable:
		// open, still within its window, the streamer online, no bet placed yet,
		// and the balance covers the minimum stake. This is a UI gate only — the
		// backend re-checks all of it before actually placing.
		manualAllowed := !locked &&
			p.Status == string(models.PredictionActive) &&
			!p.BetPlaced &&
			p.Online &&
			secondsLeft > 0 &&
			p.Balance >= manualMinBet
		pv := PredictionView{
			Streamer:         p.Streamer,
			Title:            p.Title,
			Status:           p.Status,
			Locked:           locked,
			SecondsLeft:      secondsLeft,
			SecondsLeftLabel: fmtSeconds(secondsLeft),
			BetPlaced:        p.BetPlaced,
			BetConfirmed:     p.BetConfirmed,
			WindowEndUnix:    p.CreatedAt.Add(time.Duration(p.PredictionWindowSeconds) * time.Second).Unix(),
			EventID:          p.EventID,
			ManualAllowed:    manualAllowed,
			ManualBet:        p.ManualBet,
			BetOutcomeTitle:  p.BetOutcomeTitle,
			AutoBetSkipped:   p.AutoBetSkipped,
			SkipUndoable:     p.AutoBetSkipped && !p.BetPlaced && p.Status == string(models.PredictionActive) && secondsLeft > 0,
			ManualPending:    p.ManualPending,
			ManualError:      p.ManualError,
			Balance:          p.Balance,
			BalanceLabel:     util.FormatNumber(p.Balance),
			MinBet:           manualMinBet,
		}
		if p.BetPlaced && p.BetAmount > 0 {
			pv.BetAmount = util.FormatNumber(p.BetAmount)
		}
		if p.TotalPoints > 0 {
			pv.PoolLabel = util.FormatNumber(p.TotalPoints)
		}
		for _, o := range p.Outcomes {
			ov := PredictionOutcomeView{
				ID:         o.ID,
				Title:      o.Title,
				Color:      o.Color,
				Percent:    int(o.PercentageUsers + 0.5),
				Chosen:     o.Chosen,
				Selectable: manualAllowed && o.ID != "",
			}
			if o.Odds > 0 {
				ov.Odds = fmt.Sprintf("%.2fx", o.Odds)
			}
			if o.TotalPoints > 0 {
				ov.PointsLabel = util.FormatNumber(o.TotalPoints)
			}
			pv.Outcomes = append(pv.Outcomes, ov)
		}
		views = append(views, pv)
	}
	// Soonest-closing first.
	sort.SliceStable(views, func(i, j int) bool { return views[i].SecondsLeft < views[j].SecondsLeft })
	return views
}

// handleAPIOverviewEvents returns the recent events for one streamer, rendered
// as the card drawer partial. Reads the in-memory ring buffer only.
func (s *Server) handleAPIOverviewEvents(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/overview/events/")
	if name == "" {
		writeBadRequest(w, "Streamer not specified")
		return
	}

	type eventRow struct {
		Label string
		Ago   string
	}
	var rows []eventRow
	for _, e := range events.Recent(200) {
		if !strings.EqualFold(e.Streamer, name) {
			continue
		}
		rows = append(rows, eventRow{Label: eventLabel(e), Ago: util.FormatDuration(time.Since(e.Time)) + " ago"})
		if len(rows) >= 20 {
			break
		}
	}

	s.renderPartial(w, r, "events_drawer", map[string]interface{}{
		"Name":   name,
		"Events": rows,
	})
}
