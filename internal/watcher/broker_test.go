package watcher

import (
	"context"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// discoveryStreamer builds an ephemeral, drops-enabled streamer standing in for
// a directory-discovery candidate (online, with an assigned campaign so
// DropsCondition holds).
func discoveryStreamer(login string, restricted bool) *models.Streamer {
	s := models.NewStreamer(login, models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "ch-" + login
	s.IsOnline = true
	s.OnlineAt = time.Now().Add(-time.Minute)
	s.Stream.CampaignIDs = []string{"camp-" + login}
	if restricted {
		s.Stream.Campaigns = []*models.Campaign{{ID: "camp-" + login, Channels: []string{s.ChannelID}}}
	}
	return s
}

// staticSource is a candidate source returning a fixed proposal list.
type staticSource struct {
	name string
	cand []Candidate
}

func (s *staticSource) SourceName() string           { return s.name }
func (s *staticSource) WatchCandidates() []Candidate { return s.cand }

func loginsOf(slots []slotOccupant) map[string]bool {
	m := make(map[string]bool, len(slots))
	for _, s := range slots {
		m[s.streamer.Username] = true
	}
	return m
}

// TestArbitrateNeverExceedsMaxSlots is the core cap guarantee: no matter how
// many configured picks and external candidates are offered, arbitration never
// returns more than constants.MaxSimultaneousStreams occupied slots.
func TestArbitrateNeverExceedsMaxSlots(t *testing.T) {
	w, _ := newTestWatcher(2)
	extra := []Candidate{
		{Streamer: discoveryStreamer("disco1", true), Origin: OriginDiscovery},
		{Streamer: discoveryStreamer("disco2", true), Origin: OriginDiscovery},
	}
	slots, _ := w.arbitrate([]int{0, 1}, extra, time.Now())
	if len(slots) > constants.MaxSimultaneousStreams {
		t.Fatalf("arbitration exceeded the slot cap: got %d, max %d", len(slots), constants.MaxSimultaneousStreams)
	}
}

// TestArbitrateFillsFreeSlotWithDiscovery: one configured pick leaves a free
// slot, which a discovered channel fills — the overnight/idle case, now capped
// at two instead of being an un-arbitrated third slot.
func TestArbitrateFillsFreeSlotWithDiscovery(t *testing.T) {
	w, _ := newTestWatcher(1)
	disco := discoveryStreamer("disco", false)
	extra := []Candidate{{Streamer: disco, Origin: OriginDiscovery}}

	slots, _ := w.arbitrate([]int{0}, extra, time.Now())
	if len(slots) != 2 {
		t.Fatalf("expected the free slot filled by discovery, got %d slots", len(slots))
	}
	if !loginsOf(slots)["disco"] {
		t.Errorf("expected the discovered channel to fill the free slot, got %v", loginsOf(slots))
	}
}

// TestArbitrateRestrictedDiscoveryDisplacesPlainConfigured covers acceptance
// criterion #4: a channel-restricted discovery drop can displace a plain
// (points/rotation) configured channel when both slots are full.
func TestArbitrateRestrictedDiscoveryDisplacesPlainConfigured(t *testing.T) {
	w, _ := newTestWatcher(2) // two plain configured picks, both slots full
	restricted := discoveryStreamer("restricted_disco", true)
	extra := []Candidate{{Streamer: restricted, Origin: OriginDiscovery}}

	slots, waiting := w.arbitrate([]int{0, 1}, extra, time.Now())
	if len(slots) != constants.MaxSimultaneousStreams {
		t.Fatalf("expected slots to stay at the cap, got %d", len(slots))
	}
	if !loginsOf(slots)["restricted_disco"] {
		t.Fatalf("expected the channel-restricted discovery drop to take a slot, got %v", loginsOf(slots))
	}
	// Exactly one configured channel should remain; the other is now waiting.
	configuredKept := 0
	for _, s := range slots {
		if s.origin == OriginConfigured {
			configuredKept++
		}
	}
	if configuredKept != 1 {
		t.Errorf("expected exactly one configured channel kept, got %d", configuredKept)
	}
	if len(waiting) != 1 {
		t.Errorf("expected one displaced channel reported as waiting, got %d", len(waiting))
	}
}

// TestArbitrateDoesNotDisplaceNearStreakCompletion covers acceptance criterion
// #5: a channel within minutes of finishing its watch streak is never
// displaced, even by a channel-restricted discovery drop.
func TestArbitrateProtectsNearStreakCompletion(t *testing.T) {
	w, _ := newTestWatcher(2)
	// Both configured picks are seconds from completing a watch streak.
	for _, s := range w.streamers {
		s.Settings.WatchStreak = true
		s.Stream.WatchStreakMissing = true
		s.Stream.MinuteWatched = 6.5
	}
	restricted := discoveryStreamer("restricted_disco", true)
	extra := []Candidate{{Streamer: restricted, Origin: OriginDiscovery}}

	slots, _ := w.arbitrate([]int{0, 1}, extra, time.Now())
	if loginsOf(slots)["restricted_disco"] {
		t.Fatalf("a near-complete watch streak must not be displaced, got %v", loginsOf(slots))
	}
}

// TestArbitrateNoDuplicateSlot covers acceptance criterion #7: a channel that
// is both configured and proposed by a source never occupies two slots.
func TestArbitrateNoDuplicateSlot(t *testing.T) {
	w, _ := newTestWatcher(1)
	// A source proposes the very same streamer object that is configured.
	dup := Candidate{Streamer: w.streamers[0], Origin: OriginDiscovery}
	slots, _ := w.arbitrate([]int{0}, []Candidate{dup}, time.Now())
	if len(slots) != 1 {
		t.Fatalf("expected the duplicate proposal to be ignored, got %d slots", len(slots))
	}
}

// TestArbitratePassThroughWithoutSources guarantees the configured-only path is
// byte-for-byte the old behavior when no source proposes anything.
func TestArbitratePassThroughWithoutSources(t *testing.T) {
	w, _ := newTestWatcher(2)
	slots, waiting := w.arbitrate([]int{0, 1}, nil, time.Now())
	if len(slots) != 2 || len(waiting) != 0 {
		t.Fatalf("expected the two configured picks passed through unchanged, got %d slots %d waiting", len(slots), len(waiting))
	}
	if slots[0].idx != 0 || slots[1].idx != 1 {
		t.Errorf("expected configured indexes preserved, got %d,%d", slots[0].idx, slots[1].idx)
	}
}

// TestGatherCandidatesDropsConfiguredLogins: the broker is the single owner of
// duplicate prevention — a source proposing a channel that is on the configured
// list is dropped before arbitration.
func TestGatherCandidatesDropsConfiguredLogins(t *testing.T) {
	w, _ := newTestWatcher(1) // streamer "streamera"
	src := &staticSource{name: "discovery", cand: []Candidate{
		{Streamer: w.streamers[0]},                          // duplicate of a configured login
		{Streamer: discoveryStreamer("fresh_disco", false)}, // genuinely new
	}}
	got := w.gatherCandidates([]CandidateSource{src})
	if len(got) != 1 || got[0].Streamer.Username != "fresh_disco" {
		t.Fatalf("expected only the non-configured candidate, got %v", got)
	}
	if got[0].Origin != "discovery" {
		t.Errorf("expected origin defaulted to the source name, got %q", got[0].Origin)
	}
}

// TestArbitrateDirectModeVictimStableAcrossTicks is the regression for the
// non-deterministic displacement victim: with two equal-rank configured
// channels in direct mode and one strictly-higher-rank external candidate, the
// surviving configured channel must be identical on every tick even though
// selectByPriority returns configuredWatch in a randomized (map-iteration)
// order. Before the fix this flipped tick-to-tick.
func TestArbitrateDirectModeVictimStableAcrossTicks(t *testing.T) {
	w, _ := newTestWatcher(2)
	for _, s := range w.streamers {
		s.Settings.WatchStreak = false // isolate from streak protection
	}
	disco := discoveryStreamer("disco", false) // active_drop, rank 2 > configured rank 1
	extra := []Candidate{{Streamer: disco, Origin: OriginDiscovery}}

	survivor := ""
	for tick := 0; tick < 3000; tick++ {
		w.selectionReasons = make(map[int]string)
		w.selectionMode = ModeIdle
		cw := w.selectStreamersToWatch([]int{0, 1}) // direct mode: map-order varies
		slots, _ := w.arbitrate(cw, extra, time.Now())

		got, discoSlotted := "", false
		for _, s := range slots {
			if s.origin == OriginConfigured {
				got = s.streamer.Username
			}
			if s.streamer.Username == "disco" {
				discoSlotted = true
			}
		}
		if !discoSlotted {
			t.Fatalf("tick %d: the rank-2 discovery drop must always hold a slot", tick)
		}
		if tick == 0 {
			survivor = got
			continue
		}
		if got != survivor {
			t.Fatalf("tick %d: surviving configured channel flipped from %q to %q — victim is not deterministic", tick, survivor, got)
		}
	}
}

// TestArbitrateDirectModeColdStartVictimByIndex documents the pure-direct-mode
// cold-start behavior: when the bot never enters rotation (configured streamers
// are always <=2 online), w.rotation.lastWatched is never populated, so the
// equal-rank tie-break always falls through to the streamer-index fallback.
// The victim is then deterministically the SAME configured streamer (lowest
// index) on every displacement for the whole process uptime — stable, no flips,
// but also no alternation between the two. This is intended, documented
// behavior of this focused fix, not a bug (see the PR discussion for the note
// on when an alternating fallback would be warranted).
func TestArbitrateDirectModeColdStartVictimByIndex(t *testing.T) {
	w, _ := newTestWatcher(2)
	for _, s := range w.streamers {
		s.Settings.WatchStreak = false
	}
	if len(w.rotation.lastWatched) != 0 {
		t.Fatalf("precondition: cold start must have empty rotation recency, got %d entries", len(w.rotation.lastWatched))
	}

	disco := discoveryStreamer("disco", false) // active_drop, rank 2
	extra := []Candidate{{Streamer: disco, Origin: OriginDiscovery}}

	victimIdx0, survivorIdx1 := w.streamers[0].Username, w.streamers[1].Username
	for tick := 0; tick < 2000; tick++ {
		w.selectionReasons = make(map[int]string)
		w.selectionMode = ModeIdle
		cw := w.selectStreamersToWatch([]int{0, 1}) // direct mode, order varies
		slots, _ := w.arbitrate(cw, extra, time.Now())

		survivor := ""
		for _, s := range slots {
			if s.origin == OriginConfigured {
				survivor = s.streamer.Username
			}
		}
		// With both recencies zero, the lower-index streamer (0) is always the
		// victim, so the higher-index one (1) always survives — every tick.
		if survivor != survivorIdx1 {
			t.Fatalf("tick %d: cold-start victim must be the by-index fallback (streamer 0 = %q evicted, streamer 1 = %q kept), but %q was evicted",
				tick, victimIdx0, survivorIdx1, survivorIdx1)
		}
	}

	// Direct mode must never have populated rotation recency (that is what keeps
	// the choice stable rather than alternating).
	if len(w.rotation.lastWatched) != 0 {
		t.Errorf("direct mode must not populate rotation recency, got %d entries", len(w.rotation.lastWatched))
	}
}

// TestArbitrateEvictsMostRecentlyWatchedAmongEqualRank pins the tie-break rule
// itself: among equal-rank configured occupants the most-recently-watched is
// evicted (so the least-recently-watched, most "owed" a turn, keeps its slot),
// independent of the configuredWatch order.
func TestArbitrateEvictsMostRecentlyWatchedAmongEqualRank(t *testing.T) {
	w, _ := newTestWatcher(2)
	for _, s := range w.streamers {
		s.Settings.WatchStreak = false
	}
	now := time.Now()
	// streamer 0 was watched most recently; streamer 1 longer ago.
	w.rotation.lastWatched = map[int]time.Time{
		0: now,
		1: now.Add(-time.Hour),
	}
	disco := discoveryStreamer("disco", false)
	extra := []Candidate{{Streamer: disco, Origin: OriginDiscovery}}

	// Both possible configuredWatch orders must evict the same (most-recent)
	// channel and keep streamer 1.
	for _, cw := range [][]int{{0, 1}, {1, 0}} {
		slots, _ := w.arbitrate(cw, extra, now)
		survivor := ""
		for _, s := range slots {
			if s.origin == OriginConfigured {
				survivor = s.streamer.Username
			}
		}
		if survivor != w.streamers[1].Username {
			t.Fatalf("cw=%v: expected the least-recently-watched channel %q to keep its slot, got %q",
				cw, w.streamers[1].Username, survivor)
		}
	}
}

func TestSlotRankOrdering(t *testing.T) {
	restricted := slotRank(ReasonRestrictedDrop)
	streak := slotRank(ReasonStreak)
	drop := slotRank(ReasonActiveDrop)
	fair := slotRank(ReasonFairRotation)
	if restricted <= streak || streak <= drop || drop <= fair {
		t.Fatalf("reason-code ranking must be restricted > streak > active drop > fair rotation, got %d,%d,%d,%d",
			restricted, streak, drop, fair)
	}
}

// staticChecker/staticSender are fakes for exercising the full send loop
// without real HTTP.
type staticChecker struct{ checked chan string }

func (c *staticChecker) CheckStreamerOnline(s *models.Streamer) {
	select {
	case c.checked <- s.Username:
	default:
	}
}

type countingSender struct {
	sent chan string
	err  error
}

func (s *countingSender) Send(streamer *models.Streamer) (error, error) {
	select {
	case s.sent <- streamer.Username:
	default:
	}
	return nil, s.err
}

// newLoopWatcher builds a broker wired with fakes so the real loop() can run
// without touching the network.
func newLoopWatcher(n int, sender minuteReporter, checker onlineChecker) (*MinuteWatcher, []*models.Streamer) {
	streamers := make([]*models.Streamer, n)
	for i := range streamers {
		streamers[i] = models.NewStreamer("streamer"+string(rune('a'+i)), models.DefaultStreamerSettings())
		streamers[i].SetOnline()
		streamers[i].OnlineAt = time.Now().Add(-time.Minute)
	}
	w := &MinuteWatcher{
		client:     checker,
		streamers:  streamers,
		priorities: []config.Priority{config.PriorityOrder},
		settings: config.RateLimitSettings{
			MinuteWatchedInterval:      1,
			RotationIntervalMinMinutes: 1,
			RotationIntervalMaxMinutes: 1,
		},
		sender: sender,
		// No real inter-send pauses in tests.
		pacer: func(time.Duration) bool { return true },
	}
	return w, streamers
}

// TestProcessWatchingNeverSendsMoreThanMaxSlots verifies the whole-tick
// guarantee: with more streamers online than slots and a discovery candidate
// competing, a single processWatching pass sends at most
// constants.MaxSimultaneousStreams minute-watched reports, and never twice to
// one channel.
func TestProcessWatchingNeverSendsMoreThanMaxSlots(t *testing.T) {
	sender := &countingSender{sent: make(chan string, 16)}
	checker := &staticChecker{checked: make(chan string, 16)}
	w, _ := newLoopWatcher(4, sender, checker)

	// A discovery source also competing for a slot.
	w.AddSource(&staticSource{name: "discovery", cand: []Candidate{
		{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery},
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.ctx = ctx

	w.processWatching()

	close(sender.sent)
	seen := map[string]bool{}
	count := 0
	for name := range sender.sent {
		if seen[name] {
			t.Fatalf("channel %q was sent minute-watched twice in one tick", name)
		}
		seen[name] = true
		count++
	}
	if count > constants.MaxSimultaneousStreams {
		t.Fatalf("sent %d minute-watched reports in one tick, cap is %d", count, constants.MaxSimultaneousStreams)
	}
	if count == 0 {
		t.Fatal("expected at least one minute-watched report")
	}
}

// TestBrokerSnapshotReflectsAllocation checks the explainable snapshot is
// published and includes the discovery-occupied slot.
func TestBrokerSnapshotReflectsAllocation(t *testing.T) {
	sender := &countingSender{sent: make(chan string, 16)}
	checker := &staticChecker{checked: make(chan string, 16)}
	w, _ := newLoopWatcher(1, sender, checker)
	w.AddSource(&staticSource{name: "discovery", cand: []Candidate{
		{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery},
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.ctx = ctx
	w.processWatching()

	snap := w.BrokerSnapshot()
	if snap.MaxSlots != constants.MaxSimultaneousStreams {
		t.Errorf("expected MaxSlots=%d, got %d", constants.MaxSimultaneousStreams, snap.MaxSlots)
	}
	var haveDisco bool
	for _, s := range snap.Slots {
		if s.Channel == "disco" {
			haveDisco = true
			if s.Origin != OriginDiscovery {
				t.Errorf("expected discovery origin, got %q", s.Origin)
			}
			if s.ReasonCode != ReasonRestrictedDrop {
				t.Errorf("expected restricted_drop reason code, got %q", s.ReasonCode)
			}
		}
	}
	if !haveDisco {
		t.Errorf("expected the discovered channel in the broker snapshot slots, got %+v", snap.Slots)
	}
	if !w.IsWatching("disco") {
		t.Error("IsWatching should report the slotted discovery channel as watched")
	}
}

// TestProcessWatchingContextCancelStopsSends: a cancelled context aborts the
// send loop between slots.
func TestProcessWatchingContextCancelStopsSends(t *testing.T) {
	sender := &countingSender{sent: make(chan string, 16)}
	checker := &staticChecker{checked: make(chan string, 16)}
	w, _ := newLoopWatcher(2, sender, checker)

	ctx, cancel := context.WithCancel(context.Background())
	w.ctx = ctx
	cancel()
	// Simulate the pacing wait observing the cancelled context: the send loop
	// must stop after the first send instead of pacing through every slot.
	w.pacer = func(time.Duration) bool { return false }

	w.processWatching() // must not hang or panic

	close(sender.sent)
	count := 0
	for range sender.sent {
		count++
	}
	if count > 1 {
		t.Fatalf("expected the send loop to stop after the first send on cancellation, got %d sends", count)
	}
}

// TestUpdateSettingsAppliedNextTick confirms staged runtime settings are picked
// up by the loop without restart, and that priorities/settings stay loop-owned.
func TestUpdateSettingsAppliedNextTick(t *testing.T) {
	sender := &countingSender{sent: make(chan string, 16)}
	checker := &staticChecker{checked: make(chan string, 16)}
	w, _ := newLoopWatcher(2, sender, checker)

	w.UpdateSettings([]config.Priority{config.PriorityPointsDescending}, config.RateLimitSettings{
		MinuteWatchedInterval:      2,
		RotationIntervalMinMinutes: 1,
		RotationIntervalMaxMinutes: 1,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.ctx = ctx
	w.processWatching() // applies pending settings at the start of the tick

	if w.settings.MinuteWatchedInterval != 2 {
		t.Errorf("expected the staged interval applied, got %d", w.settings.MinuteWatchedInterval)
	}
	if len(w.priorities) != 1 || w.priorities[0] != config.PriorityPointsDescending {
		t.Errorf("expected the staged priorities applied, got %v", w.priorities)
	}
}
