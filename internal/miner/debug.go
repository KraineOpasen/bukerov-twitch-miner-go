package miner

import (
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/debug"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// snapshotRecentEvents bounds how many ring-buffer events one snapshot
// carries; the buffer itself may hold more.
const snapshotRecentEvents = 100

// BuildDebugSnapshot assembles the /debug/snapshot document from every
// component the miner orchestrates. Called by the debug HTTP server on each
// request; only runs after startMining, so the component fields it reads
// (watcher, wsPool, streamers) are fully wired and never reassigned.
func (m *Miner) BuildDebugSnapshot() debug.Snapshot {
	m.mu.RLock()
	startedAt := m.startedAt
	reauth := m.reauthRequired
	lost := m.connectionLost
	lostDetail := m.connectionDetail
	m.mu.RUnlock()

	snap := debug.Snapshot{
		GeneratedAt: time.Now(),
		Version:     version.Version,
		Username:    m.config.Username,
	}
	if !startedAt.IsZero() {
		snap.UptimeSeconds = int64(time.Since(startedAt).Seconds())
	}

	switch {
	case reauth:
		snap.Status = debug.StatusAuthError
		snap.StatusDetail = "Twitch authorization expired or was revoked - restart the miner and log in again"
	case lost:
		snap.Status = debug.StatusPaused
		snap.StatusDetail = lostDetail
	default:
		snap.Status = debug.StatusRunning
	}

	var watcherState watcher.DebugState
	if m.watcher != nil {
		watcherState = m.watcher.GetDebugState()
	}

	decisions := make(map[string]watcher.WatchDecision, len(watcherState.Decisions))
	for _, d := range watcherState.Decisions {
		decisions[d.Username] = d
	}

	snap.Watching = debug.WatchingInfo{
		Mode:                 watcherState.Mode,
		EvaluatedAt:          watcherState.EvaluatedAt,
		ActivePair:           watcherState.ActivePair,
		PairSince:            watcherState.PairSince,
		NextRotationAt:       watcherState.NextRotationAt,
		WatchTimeWindowHours: watcherState.WatchTimeWindowHours,
	}
	if snap.Watching.Mode == "" {
		snap.Watching.Mode = watcher.ModeIdle
	}
	for _, p := range watcherState.PostponedSwapOuts {
		snap.Watching.PostponedSwapOuts = append(snap.Watching.PostponedSwapOuts, debug.PostponedSwapOut{
			Username: p.Username,
			Until:    p.Until,
		})
	}

	// Predictions the pool still tracks in a bettable state, keyed by
	// streamer. Resolved/cancelled ones surface via recentEvents instead.
	activePredictions := make(map[string]*debug.PredictionInfo)
	if m.wsPool != nil {
		for _, p := range m.wsPool.PredictionsSnapshot() {
			if p.Status != string(models.PredictionActive) && p.Status != string(models.PredictionLocked) {
				continue
			}
			activePredictions[p.Streamer] = &debug.PredictionInfo{
				Title:        p.Title,
				Status:       p.Status,
				CreatedAt:    p.CreatedAt,
				BetPlaced:    p.BetPlaced,
				BetConfirmed: p.BetConfirmed,
				BetAmount:    p.BetAmount,
			}
		}
	}

	if m.streamers != nil {
		for _, s := range m.streamers.All() {
			st := debug.StreamerState{
				Username:      s.Username,
				Online:        s.GetIsOnline(),
				ChannelPoints: s.GetChannelPoints(),
				Preference:    string(s.GetSettings().Preference),
			}

			if d, ok := decisions[s.Username]; ok {
				st.Watching = d.Watching
				st.Reason = d.Reason
				st.WatchedMinutesWindow = d.WatchedMinutesWindow
			}

			if st.Online {
				st.OnlineSince = s.GetOnlineAt()
				st.Game = s.Stream.GameName()
				st.Title = s.Stream.GetTitle()
				if st.Reason == "" {
					st.Reason = "online - awaiting the next watch-selection tick"
				}

				for _, c := range s.ActiveCampaignsSummary() {
					st.DropCampaigns = append(st.DropCampaigns, debug.DropCampaignInfo{
						Name:              c.Name,
						Game:              c.Game,
						EndAt:             c.EndAt,
						ChannelRestricted: c.ChannelRestricted,
						RemainingDrops:    c.RemainingDrops,
					})
				}

				if s.GetSettings().WatchStreak && s.Stream.GetWatchStreakMissing() {
					st.WatchStreak = &debug.WatchStreakInfo{
						Pending:        true,
						MinutesWatched: s.Stream.GetMinuteWatched(),
					}
				}
			} else {
				st.OfflineSince = s.GetOfflineAt()
				if st.Reason == "" {
					st.Reason = "offline"
				}
			}

			st.ActivePrediction = activePredictions[s.Username]
			snap.Streamers = append(snap.Streamers, st)
		}
	}

	if m.dropsTracker != nil {
		campaigns := m.dropsTracker.Campaigns()
		info := &debug.DropsSyncInfo{LastSyncAt: m.dropsTracker.LastSync()}
		for _, c := range campaigns {
			tc := debug.TrackedCampaignInfo{
				RemainingDrops:    len(c.Drops),
				OverallPercent:    c.OverallProgressPercent(),
				ClaimStatus:       string(c.ClaimStatus),
				ChannelRestricted: c.IsChannelRestricted(),
				InInventory:       c.InInventory,
				Name:              c.Name,
			}
			if c.Game != nil {
				tc.Game = c.Game.Name
			}
			info.Campaigns = append(info.Campaigns, tc)
		}
		snap.Drops = info
	}

	if m.discovery != nil {
		if st := m.discovery.State(); st.Enabled {
			info := &debug.DiscoveryInfo{
				Games:      st.Games,
				Watching:   st.Watching,
				LastSyncAt: st.LastSync,
			}
			for _, ch := range st.Channels {
				info.Channels = append(info.Channels, debug.DiscoveryChannel{
					Login:          ch.Login,
					Game:           ch.Game,
					Viewers:        ch.Viewers,
					Status:         ch.Status,
					MinutesWatched: ch.MinutesWatched,
				})
			}
			snap.Discovery = info
		}
	}

	snap.RecentEvents = events.Recent(snapshotRecentEvents)
	return snap
}
