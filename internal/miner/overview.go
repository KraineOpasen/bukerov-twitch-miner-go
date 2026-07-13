package miner

import (
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"
)

// WatchSlots implements web.OverviewProvider. It exposes the watcher's live
// selection state (which streamers occupy the two watch slots, why, and when
// the next rotation is due) as a web view type. Read-only snapshot of
// in-memory state - no Twitch calls.
func (m *Miner) WatchSlots() web.WatchSlotsView {
	view := web.WatchSlotsView{
		Watching: make(map[string]bool),
		Reason:   make(map[string]string),
	}
	if m.watcher == nil {
		return view
	}

	st := m.watcher.GetDebugState()
	view.Mode = st.Mode
	view.NextRotationAt = st.NextRotationAt
	view.ActivePair = append([]string(nil), st.ActivePair...)
	for _, d := range st.Decisions {
		if d.Watching {
			view.Watching[d.Username] = true
		}
		if d.Reason != "" {
			view.Reason[d.Username] = d.Reason
		}
	}
	return view
}

// LivePredictions implements web.OverviewProvider. It maps the pubsub pool's
// prediction snapshot to web view types, keeping only the events that are
// still bettable/visible (ACTIVE or LOCKED); resolved/cancelled ones surface
// through the events feed instead.
func (m *Miner) LivePredictions() []web.LivePrediction {
	if m.wsPool == nil {
		return nil
	}

	var out []web.LivePrediction
	for _, p := range m.wsPool.PredictionsSnapshot() {
		if p.Status != string(models.PredictionActive) && p.Status != string(models.PredictionLocked) {
			continue
		}
		lp := web.LivePrediction{
			Streamer:                p.Streamer,
			Title:                   p.Title,
			Status:                  p.Status,
			CreatedAt:               p.CreatedAt,
			PredictionWindowSeconds: p.PredictionWindowSeconds,
			BetPlaced:               p.BetPlaced,
			BetConfirmed:            p.BetConfirmed,
			BetAmount:               p.BetAmount,
			TotalPoints:             p.TotalPoints,
		}
		for _, o := range p.Outcomes {
			lp.Outcomes = append(lp.Outcomes, web.LivePredictionOutcome{
				Title:           o.Title,
				Color:           o.Color,
				PercentageUsers: o.PercentageUsers,
				Odds:            o.Odds,
				TotalPoints:     o.TotalPoints,
				Chosen:          o.Chosen,
			})
		}
		out = append(out, lp)
	}
	return out
}
