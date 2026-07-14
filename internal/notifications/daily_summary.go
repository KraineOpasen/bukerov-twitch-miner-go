package notifications

import (
	"fmt"
	"strings"
)

// DailySummary is the once-a-day operator digest payload. The miner assembles it
// from analytics and runtime counters; the manager formats and dispatches it
// through the system channel (same gate as the other operator alerts).
//
// EarnedPoints is the NET channel-point change for the day; PredictionNet is the
// betting contribution that is ALREADY included in EarnedPoints (surfaced as a
// component, not an independent number, so the two never read as double-counted).
// ClaimedDrops and Streaks are durable counts; RecoveryIncidents and
// LostMiningMinutes are in-memory best-effort figures (a mid-day restart resets
// them), which the rendered text flags.
type DailySummary struct {
	Date              string
	EarnedPoints      int
	PredictionNet     int
	PredictionWins    int
	PredictionLosses  int
	PredictionRefunds int
	ClaimedDrops      int
	Streaks           int
	RecoveryIncidents int
	LostMiningMinutes float64
}

func signedInt(n int) string {
	if n >= 0 {
		return fmt.Sprintf("+%d", n)
	}
	return fmt.Sprintf("%d", n)
}

// Render returns the summary's title and multi-line message body.
func (s DailySummary) Render() (title, message string) {
	title = "📊 Daily summary — " + s.Date

	var b strings.Builder

	// Net points, with the prediction contribution phrased as a COMPONENT of the
	// net figure (never a separate parallel number).
	if s.PredictionNet != 0 {
		fmt.Fprintf(&b, "Net points: %s (of which %s from predictions)\n",
			signedInt(s.EarnedPoints), signedInt(s.PredictionNet))
	} else {
		fmt.Fprintf(&b, "Net points: %s\n", signedInt(s.EarnedPoints))
	}

	fmt.Fprintf(&b, "Drops claimed: %d\n", s.ClaimedDrops)
	fmt.Fprintf(&b, "Watch streaks: %d\n", s.Streaks)

	if s.PredictionWins+s.PredictionLosses+s.PredictionRefunds > 0 {
		fmt.Fprintf(&b, "Predictions: %dW / %dL", s.PredictionWins, s.PredictionLosses)
		if s.PredictionRefunds > 0 {
			fmt.Fprintf(&b, " / %d refunded", s.PredictionRefunds)
		}
		b.WriteByte('\n')
	}

	// Best-effort (in-memory, reset on restart) — labelled as such.
	fmt.Fprintf(&b, "Recovery incidents: %d\n", s.RecoveryIncidents)
	fmt.Fprintf(&b, "Lost mining time: ~%d min\n", int(s.LostMiningMinutes+0.5))
	b.WriteString("(recovery incidents and lost time are best-effort since the last restart)")

	return title, strings.TrimRight(b.String(), "\n")
}

// NotifyDailySummary renders and dispatches the daily digest through the system
// channel (push providers + Discord system channel, gated on SystemEnabled),
// reusing the same operator-alert dispatch as the health/connection/drop alerts.
func (m *Manager) NotifyDailySummary(s DailySummary) {
	title, message := s.Render()
	m.notifyDropTransition(NotificationTypeDailySummary, title, message)
}
