package health

import (
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// --- Group F: delivery currentness ---
//
// stallMinReports promises "we are demonstrably watching, Twitch is demonstrably
// not crediting". The success counter it reads is cumulative and never decays, so
// on its own it proves only that delivery worked at SOME point since the last
// progress. These tests pin the missing half: at the moment a stall confirms,
// delivery evidence must also be CURRENT — the most recently completed counted
// send succeeded, and it is no older than the stall-evidence horizon.
//
// The failure family this closes is beacon/spade rejection on a channel that
// stays online, slotted and eligible: no OAuth/GQL/PubSub/inventory gate fires,
// and watch_transport is recorded only by the canary against its own configured
// channel, so nothing else stops a false confirmation.

// TestWatchdogStaleDeliveryEvidenceDoesNotConfirmStall: five successful reports
// followed by nothing but failed sends. On the pinned base the frozen counter
// keeps satisfying the report threshold and a stall confirms on evidence that is
// already minutes old. It must not — and, just as importantly, the failure must
// not destroy the evidence window either: a transient rejected send is not a gate
// failure, so a genuine stall still confirms as soon as delivery is current
// again, without rebuilding a whole fresh window.
func TestWatchdogStaleDeliveryEvidenceDoesNotConfirmStall(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.evaluate(h.now) // baselines; the evidence window opens here

	// Two observed ticks of real farming: 20m elapsed, 2 clean observations,
	// 6 delivered reports (past stallMinReports).
	h.tick(10*time.Minute, true, 3)
	h.tick(10*time.Minute, true, 3)

	// Delivery now stops working while the channel keeps its slot, its game and
	// its campaign assignment. This third observed tick completes every
	// pre-existing threshold — 30m >= 20m, 3 >= 3 observations, 6 >= 5 reports —
	// so on the base it confirms a stall on ten-minute-old successes.
	h.tickFailing(10*time.Minute, true, 3)
	assertNoRecovery(t, h, "delivery is failing")

	st := h.state(t)
	if st.NoProgressObs != 3 {
		t.Fatalf("a failed send is not a gate failure and must not reset the observation count, got %+v", st)
	}
	if st.ReportsSinceProgress != 6 {
		t.Fatalf("a failed send must not discard the accumulated report count, got %+v", st)
	}
	if st.Status != ProgressHealthy {
		t.Fatalf("a blocked confirmation leaves the drop healthy, got %+v", st)
	}

	// Case B: the failures continue for another half hour. Stale successes must
	// stay incapable of authorizing recovery for as long as delivery is down.
	for i := 0; i < 3; i++ {
		h.tickFailing(10*time.Minute, true, 3)
	}
	assertNoRecovery(t, h, "delivery is failing")
	if st := h.state(t); st.NoProgressObs != 6 {
		t.Fatalf("the evidence window must keep accruing across the outage, got %+v", st)
	}

	// Delivery recovers. Every other threshold has been met for an hour, so the
	// genuine stall must confirm on this very pass — the repair must not have
	// turned a false positive into a false negative.
	h.tick(10*time.Minute, true, 3)
	if _, triggered := h.drops.counts(); triggered != 1 {
		t.Fatalf("a genuine stall must confirm as soon as delivery is current again, got triggered=%d", triggered)
	}
}

// TestWatchdogExpiredDeliveryEvidenceDoesNotConfirmStall isolates the recency
// clause from the last-outcome clause: the reports were delivered, no later send
// FAILED (delivery simply went quiet — the third outcome class, a stale send,
// moves neither counter), and only the age of the last success disqualifies it.
// A mutant that drops only the freshness check must fail here.
func TestWatchdogExpiredDeliveryEvidenceDoesNotConfirmStall(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.evaluate(h.now)

	// The whole report quota is delivered up front, one minute in.
	h.tick(time.Minute, true, 5)

	// Then delivery goes quiet: no successes and no failures, so the most recent
	// counted outcome is still that success. Only its age changes.
	h.tick(21*time.Minute, true, 0) // 22m elapsed, 2 observations, success 21m old
	h.tick(5*time.Minute, true, 0)  // 27m elapsed, 3 observations — thresholds met

	assertNoRecovery(t, h, "delivery evidence is stale")
	if st := h.state(t); st.ReportsSinceProgress != 5 {
		t.Fatalf("an expired success is still a counted report; the tally must stand, got %+v", st)
	}

	// Control: one fresh delivery makes the evidence current and the same
	// otherwise-unchanged stall confirms.
	h.tick(time.Minute, true, 1)
	if _, triggered := h.drops.counts(); triggered != 1 {
		t.Fatalf("a fresh delivery must restore currentness and confirm the stall, got triggered=%d", triggered)
	}
}

// TestWatchdogSlotBlipDoesNotFreezeReportsBaseline: the broker's per-slot
// counters restart when a login misses one slot allocation, and the login is
// unchanged, so nothing re-baselines the watchdog on its behalf. On the base the
// `n >= 0` guard reads the decrease as a stale sample and silently keeps a report
// count earned in the PREVIOUS counter tenure. The new tenure must instead be
// adopted as the baseline.
func TestWatchdogSlotBlipDoesNotFreezeReportsBaseline(t *testing.T) {
	h := newWatchdogHarness(t)
	// A non-zero baseline: the channel had been delivering long before this drop
	// episode started.
	h.watch.addSuccesses("chan", 40)
	h.w.evaluate(h.now)
	if st := h.state(t); st.ReportsSinceProgress != 0 {
		t.Fatalf("the first sighting baselines against the live counter, got %+v", st)
	}

	h.tick(10*time.Minute, true, 3)
	h.tick(10*time.Minute, true, 3)
	if st := h.state(t); st.ReportsSinceProgress != 6 {
		t.Fatalf("expected 6 reports past the baseline, got %+v", st)
	}

	// The counter tenure restarts far below the watchdog's baseline, with the
	// farming channel unchanged.
	h.watch.restartStatsTenure("chan", 2)
	h.tick(10*time.Minute, true, 0)

	st := h.state(t)
	if st.ReportsSinceProgress != 0 {
		t.Fatalf("a restarted counter must re-baseline, not keep the previous tenure's count, got %+v", st)
	}
	if st.Channel != "chan" {
		t.Fatalf("the farming channel is unchanged throughout, got %+v", st)
	}
	// A counter restart is not a gate failure: the evidence window survives it.
	if st.NoProgressObs != 3 {
		t.Fatalf("re-baselining the counter must not discard the evidence window, got %+v", st)
	}

	// From the adopted baseline, only genuinely new successes accrue.
	h.tick(10*time.Minute, true, 3)
	if st := h.state(t); st.ReportsSinceProgress != 3 {
		t.Fatalf("expected 3 reports past the adopted baseline, got %+v", st)
	}
}

// TestWatchdogMissingReportStatsCannotConfirmStall: when the broker publishes no
// accounting for the farming channel, the watchdog has no live evidence that
// delivery is happening. A previously cached report tally must not stand in for
// one — but a single missed sample must not wipe the episode either; the next
// valid sample decides normally.
func TestWatchdogMissingReportStatsCannotConfirmStall(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.evaluate(h.now)
	h.tick(10*time.Minute, true, 3)
	h.tick(10*time.Minute, true, 3)

	// The published accounting for the channel disappears on the pass that would
	// otherwise confirm.
	h.watch.dropStats("chan")
	h.tick(10*time.Minute, true, 0)
	assertNoRecovery(t, h, "delivery evidence unavailable")
	if st := h.state(t); st.NoProgressObs != 3 {
		t.Fatalf("one missed sample must not discard the evidence window, got %+v", st)
	}

	// The accounting comes back the only way the broker can produce it: an entry
	// reappears because a counted outcome re-created it, so the counter restarts
	// low. The tally therefore re-baselines rather than resuming at 6...
	h.watch.restartStatsTenure("chan", 1)
	h.tick(10*time.Minute, true, 0)
	// The stale tally of 6 does not survive: what stands is the new tenure's own
	// single delivery. This episode's baseline is 0, so the adopt condition is
	// not what corrects it — simply resampling the restarted counter is.
	if st := h.state(t); st.ReportsSinceProgress != 1 {
		t.Fatalf("a reappearing accounting is a new tenure; the stale tally must not stand, got %+v", st)
	}
	assertNoRecovery(t, h, "")

	// ...and confirmation follows once the new tenure delivers enough itself.
	h.tick(10*time.Minute, true, stallMinReports)
	if _, triggered := h.drops.counts(); triggered != 1 {
		t.Fatalf("the new tenure's own deliveries must confirm normally, got triggered=%d", triggered)
	}
}

// TestWatchdogDeliveryOutageMidPipelinePausesWithoutLosingStage mirrors
// TestWatchdogTransientGateBlipPausesButKeepsStage for the delivery threshold.
// A gate blip discards the evidence window, so the pipeline must wait out a full
// fresh one; a delivery blip must NOT, because currentness is a threshold. The
// reached stage survives either way.
func TestWatchdogDeliveryOutageMidPipelinePausesWithoutLosingStage(t *testing.T) {
	h := newWatchdogHarness(t)
	h.driveToStall(t) // stage 1 executed

	before := h.state(t)
	h.tickFailing(10*time.Minute, true, 3)
	st := h.state(t)
	if st.RecoveryStage != 1 {
		t.Fatalf("a delivery outage must not reset the recovery stage, got %+v", st)
	}
	if st.NoProgressObs <= before.NoProgressObs || st.ReportsSinceProgress != before.ReportsSinceProgress {
		t.Fatalf("the evidence window must keep accruing across a delivery outage, got %+v (was %+v)", st, before)
	}
	if syncNow, _ := h.drops.counts(); syncNow != 0 {
		t.Fatalf("no new stage may run while delivery is down, got syncNow=%d", syncNow)
	}

	// One delivered report and the NEXT stage runs immediately — no fresh
	// evidence window is required, unlike the gate-blip case.
	h.tick(10*time.Minute, true, 3)
	if syncNow, _ := h.drops.counts(); syncNow != 1 {
		t.Fatalf("restored delivery must resume the pipeline at once, got syncNow=%d", syncNow)
	}
	if st := h.state(t); st.RecoveryStage != 2 {
		t.Fatalf("the pipeline must resume from stage 2, got %+v", st)
	}
}

// TestWatchdogDeliveryHorizonFollowsStallDelay pins that the freshness horizon is
// cfg.StallDelay and not a baked-in constant: at a 60-minute StallDelay a
// 40-minute-old success is still current, where the harness's 20-minute default
// would long since have expired it.
func TestWatchdogDeliveryHorizonFollowsStallDelay(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.UpdateSettings(WatchdogConfig{
		Enabled: true, StallDelay: 60 * time.Minute, StallConfirmations: 3,
		RecoveryCooldown: 0, AvoidTTL: time.Hour, Rearm: 6 * time.Hour,
	})
	h.w.evaluate(h.now)

	// The quota lands 21 minutes in, then delivery goes quiet. At the confirming
	// pass the last success is 40 minutes old: long past the 20m horizon the
	// harness default would impose, comfortably inside this config's 60m one and
	// nowhere near its boundary (the unit table owns the exact edge).
	h.tick(21*time.Minute, true, stallMinReports) // t+21m: 1 observation
	h.tick(20*time.Minute, true, 0)               // t+41m: 2 observations, success 20m old
	h.tick(20*time.Minute, true, 0)               // t+61m >= 60m: 3 observations, success 40m old
	if _, triggered := h.drops.counts(); triggered != 1 {
		t.Fatalf("a success well inside the configured horizon must still confirm, got triggered=%d", triggered)
	}
}

// TestWatchdogSamplesReportStatsOncePerPass pins the change's headline structural
// claim: the report tally and the currentness verdict come from ONE ReportStats
// observation, so a broker republish partway through a pass cannot make them
// describe two different counter tenures.
func TestWatchdogSamplesReportStatsOncePerPass(t *testing.T) {
	h := newWatchdogHarness(t)

	// Steady state: same channel, no progress.
	before := h.watch.statsReadCount("chan")
	h.w.evaluate(h.now)
	if got := h.watch.statsReadCount("chan") - before; got != 1 {
		t.Fatalf("one evaluation pass must take exactly one delivery sample, got %d", got)
	}

	before = h.watch.statsReadCount("chan")
	h.tick(10*time.Minute, true, 3)
	if got := h.watch.statsReadCount("chan") - before; got != 1 {
		t.Fatalf("a report-carrying pass must still take exactly one sample, got %d", got)
	}

	// The progress-advanced branch replaces the whole episode and re-baselines;
	// it must reuse the same sample rather than taking a second one.
	before = h.watch.statsReadCount("chan")
	h.campaign.Drops[0].CurrentMinutesWatched += 10
	h.tick(10*time.Minute, true, 3)
	if got := h.watch.statsReadCount("chan") - before; got != 1 {
		t.Fatalf("the progress-advance re-baseline must reuse the pass's sample, got %d reads", got)
	}

	// Finally a CONFIRMING pass. The delivery term is the last conjunct of the
	// stall predicate, so every pass above short-circuits before it; only a pass
	// where the three accrued thresholds already hold actually evaluates it, and
	// only there would a read taken at the predicate — instead of reusing the
	// pass's sample — show up.
	h2 := newWatchdogHarness(t)
	h2.w.evaluate(h2.now)
	h2.tick(10*time.Minute, true, 3)
	h2.tick(10*time.Minute, true, 3)
	before = h2.watch.statsReadCount("chan")
	h2.tick(10*time.Minute, true, 3)
	if _, triggered := h2.drops.counts(); triggered != 1 {
		t.Fatalf("setup: the third tick must confirm the stall, got triggered=%d", triggered)
	}
	if got := h2.watch.statsReadCount("chan") - before; got != 1 {
		t.Fatalf("the confirming pass must still take exactly one sample, got %d reads", got)
	}
}

// TestWatchdogFreshDeliveryConfirmsTrueStall is the true-positive control for
// the whole group: uninterrupted successful delivery through a complete evidence
// window, with no drop progress, still confirms at the existing thresholds and
// runs the existing first recovery stage. It fails any change that makes
// delivery currentness unsatisfiable.
func TestWatchdogFreshDeliveryConfirmsTrueStall(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.evaluate(h.now)

	// Before any threshold is met the drop is healthy and the published detail
	// says delivery is current — the positive branch of describe.
	h.tick(10*time.Minute, true, 3)
	if d := h.state(t).Detail; !strings.Contains(d, "delivery current") {
		t.Fatalf("a delivering channel must be described as current, got %q", d)
	}

	h.tick(10*time.Minute, true, 3)
	h.tick(10*time.Minute, true, 3)
	if _, triggered := h.drops.counts(); triggered != 1 {
		t.Fatalf("continuous fresh delivery with no progress must still confirm a stall, got triggered=%d", triggered)
	}
	if st := h.state(t); st.Status != ProgressRecovering || st.RecoveryStage != 1 {
		t.Fatalf("expected the first recovery stage, got %+v", st)
	}
}

// TestWatchdogNeverDeliveredIsExplained pins describe's remaining branch: a
// channel that has not delivered anything yet is named as such, not reported as
// a stale or failing one. The harness reaches it through its seeded empty entry;
// production reaches the same branch whenever a channel's only counted outcomes
// have been failures, since the zero-LastSuccess rule is checked first.
func TestWatchdogNeverDeliveredIsExplained(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.evaluate(h.now)
	if d := h.state(t).Detail; !strings.Contains(d, "no minute-watched report has been delivered yet") {
		t.Fatalf("a channel with no delivery yet must be named as such, got %q", d)
	}
}

// TestDeliveryEvidenceDescribe pins the wording of every outcome, including the
// equal-timestamp case (which must NOT claim a later send failed — which send
// landed last is unknowable there) and a sample stamped after the pass clock,
// whose age must render as zero rather than negative.
func TestDeliveryEvidenceDescribe(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	const horizon = 20 * time.Minute

	cases := []struct {
		name string
		ev   deliveryEvidence
		want string
	}{
		{
			name: "no sample",
			ev:   deliveryEvidence{},
			want: "delivery evidence unavailable (the broker publishes no report accounting for this channel)",
		},
		{
			name: "never delivered",
			ev:   deliveryEvidence{sampled: true},
			want: "no minute-watched report has been delivered yet",
		},
		{
			name: "later failure",
			ev: deliveryEvidence{sampled: true, stats: watcher.ReportStats{
				Successes: 5, LastSuccess: now.Add(-5 * time.Minute),
				Failures: 1, LastFailure: now.Add(-time.Minute),
			}},
			want: "delivery is failing (last success 5m0s ago, a later send did not land)",
		},
		{
			name: "equal timestamps are unproven, not failing",
			ev: deliveryEvidence{sampled: true, stats: watcher.ReportStats{
				Successes: 5, LastSuccess: now.Add(-time.Minute),
				Failures: 1, LastFailure: now.Add(-time.Minute),
			}},
			want: "delivery is unproven (a send completed at the same instant as the last success, 1m0s ago)",
		},
		{
			name: "expired success",
			ev:   deliveryEvidence{sampled: true, stats: watcher.ReportStats{Successes: 5, LastSuccess: now.Add(-time.Hour)}},
			want: "delivery evidence is stale (last success 1h0m0s ago, past the stall-delay window)",
		},
		{
			name: "current",
			ev:   deliveryEvidence{sampled: true, stats: watcher.ReportStats{Successes: 5, LastSuccess: now.Add(-90 * time.Second)}},
			want: "delivery current (last successful report 1m30s ago)",
		},
		{
			name: "a sample newer than the pass clock renders as zero, never negative",
			ev:   deliveryEvidence{sampled: true, stats: watcher.ReportStats{Successes: 5, LastSuccess: now.Add(45 * time.Second)}},
			want: "delivery current (last successful report 0s ago)",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.ev.describe(now, horizon); got != c.want {
				t.Fatalf("describe() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestDeliveryEvidenceCurrent pins the exact semantics of the currentness
// predicate, including every boundary the state-machine tests above cannot reach
// deterministically: no sample, zero timestamps, equal timestamps, and the exact
// horizon edge.
func TestDeliveryEvidenceCurrent(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	const horizon = 20 * time.Minute

	cases := []struct {
		name    string
		ev      deliveryEvidence
		horizon time.Duration
		want    bool
	}{
		{
			name:    "no sample this pass",
			ev:      deliveryEvidence{sampled: false, stats: watcher.ReportStats{Successes: 99, LastSuccess: now}},
			horizon: horizon,
			want:    false,
		},
		{
			name:    "sampled but nothing ever delivered",
			ev:      deliveryEvidence{sampled: true},
			horizon: horizon,
			want:    false,
		},
		{
			name:    "zero last success with a real failure",
			ev:      deliveryEvidence{sampled: true, stats: watcher.ReportStats{Failures: 3, LastFailure: now.Add(-time.Minute)}},
			horizon: horizon,
			want:    false,
		},
		{
			name:    "fresh success, nothing ever failed",
			ev:      deliveryEvidence{sampled: true, stats: watcher.ReportStats{Successes: 5, LastSuccess: now.Add(-time.Minute)}},
			horizon: horizon,
			want:    true,
		},
		{
			name: "fresh success newer than an older failure",
			ev: deliveryEvidence{sampled: true, stats: watcher.ReportStats{
				Successes: 5, LastSuccess: now.Add(-time.Minute),
				Failures: 2, LastFailure: now.Add(-5 * time.Minute),
			}},
			horizon: horizon,
			want:    true,
		},
		{
			name: "a later failure invalidates a fresh success",
			ev: deliveryEvidence{sampled: true, stats: watcher.ReportStats{
				Successes: 5, LastSuccess: now.Add(-5 * time.Minute),
				Failures: 2, LastFailure: now.Add(-time.Minute),
			}},
			horizon: horizon,
			want:    false,
		},
		{
			name: "equal timestamps are not proof the last attempt succeeded",
			ev: deliveryEvidence{sampled: true, stats: watcher.ReportStats{
				Successes: 5, LastSuccess: now.Add(-time.Minute),
				Failures: 2, LastFailure: now.Add(-time.Minute),
			}},
			horizon: horizon,
			want:    false,
		},
		{
			name:    "success exactly at the horizon is still current",
			ev:      deliveryEvidence{sampled: true, stats: watcher.ReportStats{Successes: 5, LastSuccess: now.Add(-horizon)}},
			horizon: horizon,
			want:    true,
		},
		{
			name:    "one nanosecond past the horizon is not",
			ev:      deliveryEvidence{sampled: true, stats: watcher.ReportStats{Successes: 5, LastSuccess: now.Add(-horizon - time.Nanosecond)}},
			horizon: horizon,
			want:    false,
		},
		{
			name:    "expired success with no later failure",
			ev:      deliveryEvidence{sampled: true, stats: watcher.ReportStats{Successes: 5, LastSuccess: now.Add(-time.Hour)}},
			horizon: horizon,
			want:    false,
		},
		{
			name:    "a zero horizon accepts only a success at this instant",
			ev:      deliveryEvidence{sampled: true, stats: watcher.ReportStats{Successes: 5, LastSuccess: now}},
			horizon: 0,
			want:    true,
		},
		{
			name:    "a zero horizon rejects anything older",
			ev:      deliveryEvidence{sampled: true, stats: watcher.ReportStats{Successes: 5, LastSuccess: now.Add(-time.Nanosecond)}},
			horizon: 0,
			want:    false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.ev.current(now, c.horizon); got != c.want {
				t.Fatalf("current(horizon=%s) = %v, want %v", c.horizon, got, c.want)
			}
			// describe and current derive from one ladder and must never
			// disagree about the verdict.
			d := c.ev.describe(now, c.horizon)
			if got := strings.HasPrefix(d, "delivery current"); got != c.want {
				t.Fatalf("describe() = %q, which does not match current() = %v", d, c.want)
			}
		})
	}
}
