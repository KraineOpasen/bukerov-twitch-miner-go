package miner

import (
	"errors"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"
)

// errPredictionsUnavailable is returned by the manual-control methods before the
// pubsub pool exists (e.g. during startup), so the dashboard shows a friendly
// message rather than a nil dereference.
var errPredictionsUnavailable = errors.New("predictions are not available yet")

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
			EventID:                 p.EventID,
			Title:                   p.Title,
			Status:                  p.Status,
			CreatedAt:               p.CreatedAt,
			PredictionWindowSeconds: p.PredictionWindowSeconds,
			BetPlaced:               p.BetPlaced,
			BetConfirmed:            p.BetConfirmed,
			BetAmount:               p.BetAmount,
			TotalPoints:             p.TotalPoints,
			Online:                  p.Online,
			Balance:                 p.Balance,
			ManualBet:               p.ManualBet,
			BetOutcomeTitle:         p.BetOutcomeTitle,
			AutoBetSkipped:          p.AutoBetSkipped,
			ManualPending:           p.ManualPending,
			ManualError:             p.ManualError,
		}
		for _, o := range p.Outcomes {
			lp.Outcomes = append(lp.Outcomes, web.LivePredictionOutcome{
				ID:              o.ID,
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

// PlaceManualBet implements web.PredictionControlProvider by delegating to the
// pubsub pool, which owns the tracked-prediction state and performs all the
// server-side revalidation and the auto-bet serialization. Returns the chosen
// outcome title on success.
func (m *Miner) PlaceManualBet(eventID, outcomeID string, amount int) (string, error) {
	m.mu.RLock()
	pool := m.wsPool
	m.mu.RUnlock()
	if pool == nil {
		return "", errPredictionsUnavailable
	}
	return pool.PlaceManualBet(eventID, outcomeID, amount)
}

// SetAutoBetSkip implements web.PredictionControlProvider by delegating to the
// pubsub pool. It suppresses (or un-suppresses) auto-bet for one round only.
func (m *Miner) SetAutoBetSkip(eventID string, skip bool) error {
	m.mu.RLock()
	pool := m.wsPool
	m.mu.RUnlock()
	if pool == nil {
		return errPredictionsUnavailable
	}
	return pool.SetAutoBetSkip(eventID, skip)
}
