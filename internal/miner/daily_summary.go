package miner

import (
	"context"
	"log/slog"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
)

// dropClaimedColor is the annotation colour reused for durable DROP_CLAIMED
// markers (matches the existing drops/raid palette entry).
const dropClaimedColor = "#d9a25c"

// recordDropClaimed durably records a drop claim as a DROP_CLAIMED annotation
// under the hidden analytics bucket, so the daily summary counts claims across
// restarts (the in-memory event log alone would lose them on restart). Errors
// are logged, never propagated — a failed analytics write must not disturb the
// claim path.
func (m *Miner) recordDropClaimed(dropName string) {
	if m.analyticsSvc == nil {
		return
	}
	if err := m.analyticsSvc.Repository().RecordAnnotation(
		analytics.DropsBucket, "DROP_CLAIMED", dropName, dropClaimedColor,
	); err != nil {
		slog.Error("Failed to record drop-claimed annotation", "drop", dropName, "error", err)
	}
}

// nextDailySummaryTime returns the next occurrence of the configured local
// "HH:MM" wall-clock time strictly after `now`. An unparseable time falls back
// to 09:00 (ValidateConfig already canonicalizes it, so this is defensive).
func nextDailySummaryTime(now time.Time, hhmm string) time.Time {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		t, _ = time.Parse("15:04", "09:00")
	}
	loc := now.Location()
	next := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// previousLocalDay returns the [start, end) bounds of the full local calendar
// day immediately before fire's local day. For a 09:00 fire on July 14 (local),
// the window is July 13 00:00 → July 14 00:00 local.
func previousLocalDay(fire time.Time) (start, end time.Time) {
	loc := fire.Location()
	end = time.Date(fire.Year(), fire.Month(), fire.Day(), 0, 0, 0, 0, loc)
	start = end.AddDate(0, 0, -1)
	return start, end
}

// dailySummaryLoop sends the operator digest once per day at the configured
// local time. It recomputes the next fire on every iteration (so it survives DST
// and clock changes), fires for the previous full local day, and exits cleanly
// on ctx cancellation. It never fires for a partial current day on startup.
func (m *Miner) dailySummaryLoop(ctx context.Context) {
	slog.Info("Daily summary enabled", "time", m.config.DailySummary.Time)

	var lastSentDate string // guards against a double-fire within the same day
	for {
		next := nextDailySummaryTime(time.Now(), m.config.DailySummary.Time)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		start, end := previousLocalDay(next)
		dateKey := start.Format("2006-01-02")
		if dateKey == lastSentDate {
			continue // already sent this day's summary; wait for the next one
		}
		lastSentDate = dateKey

		m.sendDailySummary(start, end)
	}
}

// sendDailySummary assembles the digest for [start, end] and dispatches it.
// Assembly drains the watcher's lost-mining accumulator, so it runs exactly
// once per scheduled send.
func (m *Miner) sendDailySummary(start, end time.Time) {
	summary := m.assembleDailySummary(start, end)
	if m.notifications != nil {
		m.notifications.NotifyDailySummary(summary)
	}
	slog.Info("Daily summary sent",
		"date", summary.Date,
		"netPoints", summary.EarnedPoints,
		"predictionNet", summary.PredictionNet,
		"dropsClaimed", summary.ClaimedDrops,
		"streaks", summary.Streaks,
		"recoveryIncidents", summary.RecoveryIncidents,
		"lostMiningMinutes", int(summary.LostMiningMinutes+0.5),
	)
}

// assembleDailySummary gathers every metric from its source. Durable metrics
// (earned points, streaks, claimed drops, prediction net) come from SQLite;
// best-effort ones (recovery incidents, lost mining time) come from the
// in-memory event log and the watcher accumulator.
func (m *Miner) assembleDailySummary(start, end time.Time) notifications.DailySummary {
	repo := m.analyticsSvc.Repository()

	earned, err := repo.EarnedPointsBetween(start, end)
	if err != nil {
		slog.Error("Daily summary: failed to read earned points", "error", err)
	}
	streaks, err := repo.CountAnnotationsByType("WATCH_STREAK", start, end)
	if err != nil {
		slog.Error("Daily summary: failed to count streaks", "error", err)
	}
	claimed, err := repo.CountAnnotationsByType("DROP_CLAIMED", start, end)
	if err != nil {
		slog.Error("Daily summary: failed to count claimed drops", "error", err)
	}

	var roi analytics.ROISummary
	if bets, err := repo.GetBets("", "", start, end); err != nil {
		slog.Error("Daily summary: failed to read prediction bets", "error", err)
	} else {
		roi = analytics.ComputeROI(bets)
	}

	lost := 0.0
	if m.watcher != nil {
		lost = m.watcher.LostMiningMinutes()
	}

	return notifications.DailySummary{
		Date:              start.Format("2006-01-02"),
		EarnedPoints:      earned,
		PredictionNet:     roi.NetProfit,
		PredictionWins:    roi.Wins,
		PredictionLosses:  roi.Losses,
		PredictionRefunds: roi.Refunds,
		ClaimedDrops:      claimed,
		Streaks:           streaks,
		RecoveryIncidents: countRecoveryEvents(start, end),
		LostMiningMinutes: lost,
	}
}

// recoveryEventTypes are the drop-progress watchdog events counted as recovery
// incidents in the daily summary.
var recoveryEventTypes = map[events.Type]bool{
	events.TypeDropStalled:      true,
	events.TypeDropRecovered:    true,
	events.TypeDropRecoveryStep: true,
}

// countRecoveryEvents counts recovery-pipeline events within [start, end] from
// the in-memory ring buffer. Best-effort: the buffer holds a bounded history and
// resets on restart, so a busy day or a mid-day restart can undercount.
func countRecoveryEvents(start, end time.Time) int {
	count := 0
	// A cap far above the ring buffer's capacity returns its full contents;
	// Recent() clamps to what's actually stored.
	for _, e := range events.Recent(1 << 20) {
		if e.Time.Before(start) || e.Time.After(end) {
			continue
		}
		if recoveryEventTypes[e.Type] {
			count++
		}
	}
	return count
}
