package watcher

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// This file owns the incident-shaped regression for the committed ordinary
// watch-slot cohort. Both cases drive the REAL processWatching pipeline —
// selection -> boost overlay -> Broker arbitration -> final proof
// reconciliation -> committed grants -> resetLostSlotContinuity -> delivery ->
// Stream.UpdateMinuteWatched -> WatchTimeStore — with the package's existing
// loop fakes. Nothing here models the scheduler: every assertion reads the
// published BrokerSnapshot and the persisted watch-time store, which is why an
// oscillation that the pure selector tests cannot see shows up as both a
// membership fact and a delivered-service fact.

// residenceFixture is one incident-shaped watcher plus the store its fairness
// ranking and its delivered-service assertions both read.
type residenceFixture struct {
	w      *MinuteWatcher
	store  *WatchTimeStore
	sender *countingSender
	src    *mutableSource
}

// newResidenceFixture wires n configured channels onto the existing loop
// harness (real loop, faked transport) with a real on-disk watch-time store, so
// RecordMinutes and WindowMinutes run for real.
func newResidenceFixture(t *testing.T, n int) *residenceFixture {
	t.Helper()

	sender := &countingSender{sent: make(chan string, 64)}
	w, streamers := newLoopWatcher(n, sender, &staticChecker{checked: make(chan string, 64)})
	for _, s := range streamers {
		s.ChannelID = "ch-" + s.GetUsername()
		// An ordinary configured channel: no drops, no watch streak, so it is
		// never boost-eligible and can only ever hold an ORDINARY seat.
		s.Settings.ClaimDrops = false
		s.Settings.WatchStreak = false
		s.Stream.Update("broadcast-"+s.GetUsername(), "", nil, nil, 1)
	}

	// The loop harness uses a 1s minute-watched interval, which makes the
	// continuity gap (2 x interval) only two seconds. These fixtures run several
	// real ticks back to back and assert on continuity, so a loaded runner
	// pausing between two of them would look like a slot loss. Use the
	// production-shaped 60s interval: the pacer is stubbed, so this changes no
	// timing, only the gap the continuity check is measured against.
	w.settings.MinuteWatchedInterval = 60

	store, db := openWatchTimeStore(t, filepath.Join(t.TempDir(), "watch.db"))
	t.Cleanup(func() { _ = db.Close() })
	w.store = store

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w.ctx = ctx

	return &residenceFixture{w: w, store: store, sender: sender}
}

// seedWeights writes real trailing-window watch minutes, the same rows the
// production fairness ranking reads. It seeds HISTORY, never service credit for
// a channel this test is about to watch.
func (f *residenceFixture) seedWeights(t *testing.T, at time.Time, minutes map[string]float64) {
	t.Helper()
	for login, m := range minutes {
		if m == 0 {
			continue
		}
		if err := f.store.RecordMinutes(login, m, at); err != nil {
			t.Fatalf("seed watch-time history for %s: %v", login, err)
		}
	}
}

// windowMinutes reads the persisted trailing-window service for login.
func (f *residenceFixture) windowMinutes(t *testing.T, login string) float64 {
	t.Helper()
	got, err := f.store.WindowMinutes([]string{login}, time.Now())
	if err != nil {
		t.Fatalf("read persisted watch minutes for %s: %v", login, err)
	}
	return got[login]
}

// makeDropCandidate turns a configured streamer into a candidate the EXISTING
// stronger admission policy admits off-pair: an assigned campaign with real
// remaining work and no watch-streak pursuit, so it carries no continuity latch.
func makeDropCandidate(s *models.Streamer, restricted bool) {
	s.Settings.ClaimDrops = true
	s.Settings.WatchStreak = false
	s.Stream.SetCampaignIDs([]string{"camp-" + s.GetUsername()})
	s.Stream.SetCampaigns([]*models.Campaign{
		watchSlotTestCampaign("camp-"+s.GetUsername(), s.ChannelID, restricted),
	})
}

// committedOrdinary returns the logins of the committed slots that are NOT the
// named stronger occupant — the ordinary cohort as the published snapshot saw
// it. Reading the snapshot (not the selector) is what makes this an end-to-end
// oracle.
func committedOrdinary(snap BrokerSnapshot, stronger string) []string {
	var out []string
	for _, slot := range snap.Slots {
		if slot.Channel == stronger {
			continue
		}
		out = append(out, slot.Channel)
	}
	sort.Strings(out)
	return out
}

// distinct counts how many different cohorts appeared across the observed ticks.
func distinct(cohorts [][]string) map[string]int {
	seen := make(map[string]int, len(cohorts))
	for _, c := range cohorts {
		key := ""
		for i, login := range c {
			if i > 0 {
				key += "+"
			}
			key += login
		}
		seen[key]++
	}
	return seen
}

// TestCommittedOrdinaryResidenceHoldsUnderConfiguredDropBoost is incident path
// one: a configured channel-restricted drop with NO pursuing-streak latch takes
// one of the two slots every tick, and the residual ordinary seat must stay
// with ONE committed channel for the minimum residence instead of being
// re-picked from ordinary deficit/recency ranking on every evaluation.
//
// The 15-minute anchor of the pre-overlay base pair does not cover this: the
// pair {a,b} never changes, so its guard never fires, while the seat that is
// actually granted alternates underneath it. That is exactly why the oracle is
// the committed cohort and the persisted delivered minutes, not the pair.
func TestCommittedOrdinaryResidenceHoldsUnderConfiguredDropBoost(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w

	// streamerd is the stronger occupant: assigned restricted campaign, no
	// streak, and enough banked history to keep it OUT of the fair base pair —
	// so it can only enter through the off-pair boost overlay.
	byLogin := streamersByLogin(w.streamers)
	makeDropCandidate(byLogin["streamerd"], true)

	// streamera and streamerb carry the SAME persisted history on purpose: the
	// ordinary ranking cannot separate them, so the committed-cohort residence is
	// the only thing that can keep one seat still. With unequal history the
	// deficit tie-break would settle it and this fixture would stop testing the
	// guard at all.
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamerc": 30,
		"streamerd": 90,
	})

	const ticks = 6
	cohorts := make([][]string, 0, ticks)
	for i := 0; i < ticks; i++ {
		w.processWatching(tickCtx(w))
		snap := w.BrokerSnapshot()
		if len(snap.Slots) != constants.MaxSimultaneousStreams {
			t.Fatalf("tick %d committed %d slots, want the full cap %d: %v",
				i, len(snap.Slots), constants.MaxSimultaneousStreams, brokerChannels(snap))
		}
		if !brokerHasChannel(snap, "streamerd") {
			t.Fatalf("tick %d: the stronger restricted-drop occupant lost its slot: %v", i, brokerChannels(snap))
		}
		cohorts = append(cohorts, committedOrdinary(snap, "streamerd"))
	}

	if got := distinct(cohorts); len(got) != 1 {
		t.Fatalf("the committed ordinary cohort changed %d times inside one residence window: %v (per-tick: %v)",
			len(got)-1, got, cohorts)
	}

	// The delivered-time consequence. A cohort that survives consecutive ticks
	// keeps its continuity, so the second and later successful reports return a
	// positive delta and reach WatchTimeStore. An alternating cohort loses its
	// slot every tick, resetLostSlotContinuity zeroes the accumulator, and every
	// report re-anchors at delta 0 — no persisted service at all.
	resident := cohorts[0][0]
	if got := f.windowMinutes(t, resident); got <= 0 {
		t.Fatalf("the committed ordinary channel %s delivered no persisted watch time across %d successful ticks (window=%v): "+
			"a cohort that is replaced every evaluation never banks a continuous minute", resident, ticks, got)
	}
}

// TestCommittedOrdinaryResidenceDeliversPersistedWatchTime isolates the
// delivered-time consequence of the oscillation from the membership fact, so
// each is proved on its own.
//
// Nothing here credits watch time synthetically: the only writer is the
// production chain (successful send -> Stream.UpdateMinuteWatched -> a positive
// delta -> WatchTimeStore.RecordMinutes). A channel that keeps its committed
// seat across consecutive evaluations banks a positive delta on its second and
// later reports. A channel that loses the seat every evaluation has its
// accumulator zeroed by resetLostSlotContinuity, so every one of its reports
// re-anchors and returns delta 0 — the sends succeed and nothing is ever
// persisted. Both ordinary competitors ending at exactly zero after this many
// successful ticks is the loss the incident produced.
func TestCommittedOrdinaryResidenceDeliversPersistedWatchTime(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w

	byLogin := streamersByLogin(w.streamers)
	makeDropCandidate(byLogin["streamerd"], true)

	// streamera and streamerb carry the SAME persisted history on purpose: the
	// ordinary ranking cannot separate them, so the committed-cohort residence is
	// the only thing that can keep one seat still. With unequal history the
	// deficit tie-break would settle it and this fixture would stop testing the
	// guard at all.
	f.seedWeights(t, time.Now(), map[string]float64{
		"streamerc": 30,
		"streamerd": 90,
	})

	// Baseline the SEEDED history so the oracle measures newly delivered
	// service only. Comparing absolute window minutes would let seeded history
	// masquerade as service.
	logins := []string{"streamera", "streamerb", "streamerc"}
	before := make(map[string]float64, len(logins))
	for _, login := range logins {
		before[login] = f.windowMinutes(t, login)
	}

	const ticks = 6
	for i := 0; i < ticks; i++ {
		w.processWatching(tickCtx(w))
	}

	// Every tick delivered its beacons: this is not a transport failure.
	if len(f.sender.sent) == 0 {
		t.Fatal("no minute-watched report was delivered at all; the fixture is not exercising the send path")
	}

	served := 0
	delivered := map[string]float64{}
	for _, login := range logins {
		delivered[login] = f.windowMinutes(t, login) - before[login]
		if login != "streamerc" && delivered[login] > 0 {
			served++
		}
	}
	// A channel that never held a slot must never be credited.
	if delivered["streamerc"] != 0 {
		t.Fatalf("streamerc never held a committed slot yet was credited %v persisted minutes", delivered["streamerc"])
	}
	if served == 0 {
		t.Fatalf("after %d successful ticks no ordinary competitor banked ANY newly delivered watch time (%v): "+
			"an ordinary seat that changes hands on every evaluation resets continuity before a single "+
			"continuous minute can be credited", ticks, delivered)
	}
}

// TestCommittedOrdinaryResidenceHoldsUnderExternalStrongerDiscovery is incident
// path two: two configured channels in DIRECT mode (at or below the slot cap,
// so ordinary fair rotation never runs) plus one external discovery candidate
// that strictly outranks them. The cross-source Broker picks the displacement
// victim on its own, and the cold-start alternation branch swaps it on every
// tick — a second, independent path to the same oscillation, which is why a fix
// confined to the configured boost seam is not enough.
func TestCommittedOrdinaryResidenceHoldsUnderExternalStrongerDiscovery(t *testing.T) {
	f := newResidenceFixture(t, 2)
	w := f.w
	w.priorities = []config.Priority{config.PriorityOrder}

	w.AddSource(&staticSource{name: OriginDiscovery, cand: []Candidate{
		{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery},
	}})

	// No seeding: both configured channels start with NO persisted history, which
	// is what this fixture needs. The ordinary ranking cannot separate them, so
	// the committed-cohort residence is the only thing that can hold one seat
	// still. (seedWeights would skip zero-valued entries anyway.)

	const ticks = 6
	cohorts := make([][]string, 0, ticks)
	for i := 0; i < ticks; i++ {
		w.processWatching(tickCtx(w))
		snap := w.BrokerSnapshot()
		if len(snap.Slots) != constants.MaxSimultaneousStreams {
			t.Fatalf("tick %d committed %d slots, want the full cap %d: %v",
				i, len(snap.Slots), constants.MaxSimultaneousStreams, brokerChannels(snap))
		}
		if !brokerHasChannel(snap, "disco") {
			t.Fatalf("tick %d: the stronger discovery occupant lost its slot: %v", i, brokerChannels(snap))
		}
		cohorts = append(cohorts, committedOrdinary(snap, "disco"))
	}

	if got := distinct(cohorts); len(got) != 1 {
		t.Fatalf("the committed ordinary cohort changed %d times inside one residence window: %v (per-tick: %v)",
			len(got)-1, got, cohorts)
	}

	resident := cohorts[0][0]
	if got := f.windowMinutes(t, resident); got <= 0 {
		t.Fatalf("the committed ordinary channel %s delivered no persisted watch time across %d successful ticks (window=%v): "+
			"a cohort that is replaced every evaluation never banks a continuous minute", resident, ticks, got)
	}
}
