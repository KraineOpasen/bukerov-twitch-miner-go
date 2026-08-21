package health

import (
	"fmt"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// deliveryEvidence is what ONE coherent watcher.ReportStats observation says
// about the farming channel's minute-watched delivery. observeProgress samples
// the broker's accounting exactly once per drop per evaluation pass and hands
// that sample here, so the report tally and the delivery verdict a single stall
// decision rests on can never come from two different broker snapshots — the
// broker republishes an immutable snapshot at the end of every watch tick, and
// one watchdog pass can block for a whole recovery stage in between.
//
// The sample is taken at the TOP of observeProgress, before the progress-advance
// branch's recovered-notification I/O. A fresh episode's baseline is therefore
// read slightly earlier than it used to be; any deliveries that land during that
// I/O fall after the baseline and count toward the new episode. The skew is
// bounded by that call and only ever makes the tally larger, which the
// currentness rules below independently guard.
type deliveryEvidence struct {
	sampled bool                // a ReportStats read succeeded on this pass
	stats   watcher.ReportStats // that read's counters and timestamps
}

// notCurrentBecause names why this evidence does NOT prove the broker is
// currently delivering minute-watched reports, or returns "" when it does. It is
// the single ladder both current and describe derive from, so the verdict and the
// published explanation cannot drift apart when a rule is added or reworded.
//
// The cumulative success counter alone cannot prove current delivery: it never
// decays, so once ReportsSinceProgress crosses stallMinReports it keeps
// satisfying that threshold even when every later send is rejected.
//
// Five rules, all fail-closed — unproven delivery is never counted as proof:
//
//   - no coherent sample this pass: nothing live backs the tally;
//   - a zero LastSuccess: nothing has ever been delivered on this tenure;
//   - a LastFailure strictly newer than LastSuccess: the most recently completed
//     counted attempt failed;
//   - the two stamped at the same instant: which landed last is genuinely
//     unknowable, and this watchdog is deliberately false-positive-averse.
//     Production cannot produce this for one login — consecutive attempts are a
//     full MinuteWatchedInterval apart — so it exists only to fail closed;
//   - a last success older than the horizon: history, not current farming.
//
// It proves the last COUNTED attempt succeeded. A send whose playback session
// moved mid-flight is reported Stale and increments neither counter, so a run of
// stale-only ticks is indistinguishable from quiet-but-healthy delivery until the
// last success ages past the horizon. Distinguishing them would need the broker
// to count a third outcome class.
//
// horizon is cfg.StallDelay: the period the watchdog already requires a drop to
// have been continuously eligible for, and the value gatesHold already uses as
// the freshness horizon for its inventory-observability evidence, with the same
// inclusive boundary (see the ProgressLastSyncAt check there) — so this reuses an
// established horizon instead of inventing a freshness constant. A zero/negative
// horizon is not production-reachable (ValidateConfig clamps the setting to
// [10, 120] minutes) and degrades fail-closed, so it needs no special case.
func (d deliveryEvidence) notCurrentBecause(now time.Time, horizon time.Duration) string {
	switch {
	case !d.sampled:
		return "delivery evidence unavailable (the broker publishes no report accounting for this channel)"
	case d.stats.LastSuccess.IsZero():
		return "no minute-watched report has been delivered yet"
	case d.stats.LastFailure.After(d.stats.LastSuccess):
		return fmt.Sprintf("delivery is failing (last success %s ago, a later send did not land)", d.successAge(now))
	case !d.stats.LastFailure.Before(d.stats.LastSuccess):
		return fmt.Sprintf("delivery is unproven (a send completed at the same instant as the last success, %s ago)", d.successAge(now))
	case now.Sub(d.stats.LastSuccess) > horizon:
		return fmt.Sprintf("delivery evidence is stale (last success %s ago, past the stall-delay window)", d.successAge(now))
	}
	return ""
}

// current reports whether the broker is demonstrably delivering minute-watched
// reports RIGHT NOW, as opposed to having delivered some at any point since the
// last progress. See notCurrentBecause for the rules and their rationale.
//
// It proves the published SNAPSHOT is fresh, not that the broker loop is alive:
// a wedged watcher that stops republishing freezes the timestamps, and this stays
// true until the frozen LastSuccess ages past the horizon. Broker liveness is a
// separate signal (BrokerSnapshot.EvaluatedAt) this deliberately does not build.
func (d deliveryEvidence) current(now time.Time, horizon time.Duration) bool {
	return d.notCurrentBecause(now, horizon) == ""
}

// describe renders the delivery half of the published detail, naming WHY
// delivery evidence is not current so a withheld confirmation stays explainable —
// the same property gatesHold gives its own gates.
func (d deliveryEvidence) describe(now time.Time, horizon time.Duration) string {
	if why := d.notCurrentBecause(now, horizon); why != "" {
		return why
	}
	return fmt.Sprintf("delivery current (last successful report %s ago)", d.successAge(now))
}

// successAge is how long ago the last delivered report landed, for display.
// evaluate fixes one clock for the whole pass while the broker stamps in real
// time, so a sample taken after a blocking recovery stage can be NEWER than that
// clock; the age is floored at zero rather than rendering as negative. The
// verdict rules above compare the raw values, where that skew is bounded by one
// recovery stage against a horizon of at least ten minutes.
func (d deliveryEvidence) successAge(now time.Time) time.Duration {
	if age := now.Sub(d.stats.LastSuccess); age > 0 {
		return age.Round(time.Second)
	}
	return 0
}
