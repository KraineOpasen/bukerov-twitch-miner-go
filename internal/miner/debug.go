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

	if m.watcher != nil {
		brokerSnap := m.watcher.BrokerSnapshot()
		for _, s := range brokerSnap.Slots {
			snap.Watching.Slots = append(snap.Watching.Slots, debug.WatchSlot{
				Slot:       s.Slot,
				Channel:    s.Channel,
				Source:     s.Origin,
				ReasonCode: s.ReasonCode,
				Reason:     s.Reason,
				Campaign:   s.Campaign,
			})
		}
		for _, wc := range brokerSnap.Waiting {
			snap.Watching.Waiting = append(snap.Watching.Waiting, debug.WaitingSlot{
				Channel:    wc.Channel,
				Source:     wc.Origin,
				ReasonCode: wc.ReasonCode,
				Reason:     wc.Reason,
			})
		}
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
		status := m.dropsTracker.SyncStatus()
		info := &debug.DropsSyncInfo{
			LastSyncAt:             status.LastSyncAt,
			SyncRuns:               status.Runs,
			DashboardCampaigns:     status.DashboardCampaigns,
			RecoveredFromInventory: status.RecoveredCampaigns,
			TrackedCampaigns:       status.TrackedCampaigns,
			LastError:              status.LastError,
		}
		for _, c := range m.dropsTracker.Campaigns() {
			tc := debug.TrackedCampaignInfo{
				Name:              c.Name,
				EndAt:             c.EndAt,
				RemainingDrops:    len(c.Drops),
				OverallPercent:    c.OverallProgressPercent(),
				ClaimStatus:       string(c.ClaimStatus),
				ChannelRestricted: c.IsChannelRestricted(),
				InInventory:       c.InInventory,
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

	if m.healthCenter != nil {
		hs := m.healthCenter.Snapshot()
		info := &debug.HealthInfo{ActiveClientID: hs.ActiveClientID}
		for _, sig := range hs.Signals {
			info.Signals = append(info.Signals, debug.HealthSignal{
				Name:      sig.Name,
				Status:    sig.Status,
				CheckedAt: sig.CheckedAt,
				Stage:     sig.Stage,
				Detail:    sig.Detail,
				ErrorCode: sig.ErrorCode,
			})
		}
		snap.Health = info
	}

	// PubSub pool: one entry per connection index, so a single dead or
	// topic-less connection is visible directly (the pool-wide health signal can
	// only say "something is wrong", not which index).
	if m.wsPool != nil {
		if conns := m.wsPool.ConnSnapshot(); len(conns) > 0 {
			info := &debug.PubSubInfo{}
			for _, c := range conns {
				info.Connections = append(info.Connections, debug.PubSubConn{
					Index:        c.Index,
					Topics:       c.Topics,
					LastPong:     c.LastPong,
					Reconnecting: c.Reconnecting,
					Closed:       c.Closed,
				})
				info.TotalTopics += c.Topics
			}
			snap.PubSub = info
		}
	}

	if m.progressWatchdog != nil {
		ps := m.progressWatchdog.Snapshot()
		info := &debug.ProgressWatchdogInfo{Enabled: ps.Enabled, EvaluatedAt: ps.EvaluatedAt}
		for _, d := range ps.Drops {
			info.Drops = append(info.Drops, debug.DropProgressState{
				Campaign:             d.CampaignName,
				Drop:                 d.DropName,
				Channel:              d.Channel,
				Status:               d.Status,
				LastMinutes:          d.LastMinutes,
				LastProgressAt:       d.LastProgressAt,
				ReportsSinceProgress: d.ReportsSinceProgress,
				NoProgressObs:        d.NoProgressObs,
				RecoveryStage:        d.RecoveryStage,
				RecoveryStageName:    d.RecoveryStageName,
				LastRecoveryAt:       d.LastRecoveryAt,
				Detail:               d.Detail,
			})
		}
		for _, e := range m.progressWatchdog.AvoidEntries() {
			info.Avoided = append(info.Avoided, debug.AvoidedChannel{
				Login: e.Login, Until: e.Until, Reason: e.Reason,
			})
		}
		snap.ProgressWatchdog = info
	}

	if mode, decisions := m.PolicySnapshot(); len(decisions) > 0 {
		info := &debug.PolicyInfo{Mode: string(mode)}
		for _, d := range decisions {
			pd := debug.PolicyDecision{
				Campaign:      d.Name,
				Status:        string(d.Status),
				Total:         d.Total,
				Excluded:      d.Excluded,
				ExcludeReason: d.ExcludeReason,
				Feasibility: debug.PolicyFeasib{
					MinutesToNextReward:   d.Feasibility.MinutesToNextReward,
					MinutesToCompleteAll:  d.Feasibility.MinutesToCompleteAll,
					CanCompleteNextReward: d.Feasibility.CanCompleteNextReward,
					CanCompleteAll:        d.Feasibility.CanCompleteAll,
				},
			}
			for _, f := range d.Factors {
				pd.Factors = append(pd.Factors, debug.PolicyLine{Label: f.Label, Points: f.Points})
			}
			info.Decisions = append(info.Decisions, pd)
		}
		snap.Policy = info
	}

	snap.RecentEvents = events.Recent(snapshotRecentEvents)
	return snap
}
