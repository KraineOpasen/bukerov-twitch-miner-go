package watcher

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
)

type campaignPolicySwapSource struct {
	w       *MinuteWatcher
	classes map[string]policy.SemanticClass
}

func (s *campaignPolicySwapSource) SourceName() string { return "policy-swap-test" }
func (s *campaignPolicySwapSource) WatchCandidates() []Candidate {
	s.w.SetCampaignSemanticClasses(s.classes)
	return nil
}

type discoveryPolicySwapSource struct {
	w              *MinuteWatcher
	candidate      Candidate
	facts          CandidateCampaignPolicy
	nextByLogin    map[string]policy.SemanticClass
	nextByCampaign map[string]policy.SemanticClass
}

func (s *discoveryPolicySwapSource) SourceName() string { return OriginDiscovery }
func (s *discoveryPolicySwapSource) WatchCandidates() []Candidate {
	s.w.SetDiscoveryCandidatePolicy(s.candidate.Streamer.GetUsername(), s.facts)
	s.w.SetCampaignSemanticPolicy(s.nextByLogin, s.nextByCampaign, nil)
	return []Candidate{s.candidate}
}

func TestAllCampaignPolicyModesReachBrokerAllocation(t *testing.T) {
	for _, mode := range []policy.Mode{
		policy.ModeGameOrder,
		policy.ModeEndingSoonest,
		policy.ModeClosestToReward,
		policy.ModeLowAvailability,
		policy.ModeSmart,
	} {
		t.Run(string(mode), func(t *testing.T) {
			w := newPolicyBrokerWatcher(t)
			now := time.Now()
			decisions := policy.Rank(mode, policyBrokerInputs(mode, now), now)
			if got := decisions[0].CampaignID; got != "campaign-a" {
				t.Fatalf("mode %s test setup winner = %s, want campaign-a", mode, got)
			}
			w.SetCampaignSemanticClasses(policyClassesForWatcher(w, decisions))
			seedPolicyBrokerWeights(t, w, now, []float64{100, 0, 1, 200})
			w.rotation.lastWatched = map[int]time.Time{0: now.Add(-time.Hour), 3: now.Add(-2 * time.Hour)}

			w.processWatching()
			snap := w.BrokerSnapshot()
			if len(snap.Slots) != 2 {
				t.Fatalf("mode %s allocated %d slots, want hard cap allocation of 2", mode, len(snap.Slots))
			}
			if !brokerHasChannel(snap, w.streamers[0].GetUsername()) {
				t.Fatalf("mode %s semantic winner missing from broker allocation %v", mode, brokerChannels(snap))
			}
			for _, slot := range snap.Slots {
				if slot.Channel == w.streamers[0].GetUsername() && !strings.Contains(slot.Reason, "semantic class 0") {
					t.Fatalf("mode %s winner reason does not expose broker semantic class: %q", mode, slot.Reason)
				}
			}
		})
	}
}

func TestCampaignPolicyHighPriorityReachesBrokerInEveryMode(t *testing.T) {
	for _, mode := range []policy.Mode{
		policy.ModeGameOrder,
		policy.ModeEndingSoonest,
		policy.ModeClosestToReward,
		policy.ModeLowAvailability,
		policy.ModeSmart,
	} {
		t.Run(string(mode), func(t *testing.T) {
			w := newPolicyBrokerWatcher(t)
			now := time.Now()
			inputs := policyBrokerInputs(mode, now)
			inputs[1].HighPriority = true
			decisions := policy.Rank(mode, inputs, now)
			if decisions[0].CampaignID != "campaign-b" {
				t.Fatalf("mode %s HighPriority winner = %s, want campaign-b", mode, decisions[0].CampaignID)
			}
			w.SetCampaignSemanticClasses(policyClassesForWatcher(w, decisions))
			seedPolicyBrokerWeights(t, w, now, []float64{0, 200, 1, 2})
			w.processWatching()
			if snap := w.BrokerSnapshot(); !brokerHasChannel(snap, w.streamers[1].GetUsername()) {
				t.Fatalf("mode %s HighPriority winner missing from broker allocation %v", mode, brokerChannels(snap))
			}
		})
	}
}

func TestCampaignPolicyUnknownDeadlineNeverBecomesEarliestAtBroker(t *testing.T) {
	w := newPolicyBrokerWatcher(t)
	now := time.Now()
	inputs := []policy.CampaignInput{
		{CampaignID: "campaign-a", Drops: []policy.DropStep{{MinutesRequired: 30, CurrentMinutesWatched: 29}}},
		{CampaignID: "campaign-b", EndAt: now.Add(2 * time.Hour), Drops: []policy.DropStep{{MinutesRequired: 30, CurrentMinutesWatched: 29}}},
		{CampaignID: "campaign-c", EndAt: now.Add(4 * time.Hour), Drops: []policy.DropStep{{MinutesRequired: 30, CurrentMinutesWatched: 29}}},
		{CampaignID: "campaign-d", EndAt: now.Add(6 * time.Hour), Drops: []policy.DropStep{{MinutesRequired: 30, CurrentMinutesWatched: 29}}},
	}
	decisions := policy.Rank(policy.ModeEndingSoonest, inputs, now)
	if decisions[0].CampaignID != "campaign-b" || decisions[len(decisions)-1].CampaignID != "campaign-a" {
		t.Fatalf("known-before-unknown policy order = %v", campaignDecisionIDs(decisions))
	}
	w.SetCampaignSemanticClasses(policyClassesForWatcher(w, decisions))
	seedPolicyBrokerWeights(t, w, now, []float64{0, 200, 1, 2})
	w.processWatching()
	if snap := w.BrokerSnapshot(); !brokerHasChannel(snap, w.streamers[1].GetUsername()) {
		t.Fatalf("known ENDING_SOONEST winner missing from broker allocation %v", brokerChannels(snap))
	}
}

func TestCampaignPolicyRestrictedHardPrecedenceAtBroker(t *testing.T) {
	w := newPolicyBrokerWatcher(t)
	now := time.Now()
	restricted := w.streamers[3]
	restricted.Stream.SetCampaigns([]*models.Campaign{{
		ID:       "campaign-d",
		Channels: []string{"only-this-channel"},
		Drops:    []*models.Drop{{ID: "drop-d", Name: "Reward", MinutesRequired: 30, PercentageProgress: 10}},
	}})
	w.SetCampaignSemanticClasses(map[string]policy.SemanticClass{
		w.streamers[0].GetUsername(): 0,
		w.streamers[1].GetUsername(): 1,
		w.streamers[2].GetUsername(): 2,
		w.streamers[3].GetUsername(): 9,
	})
	seedPolicyBrokerWeights(t, w, now, []float64{100, 0, 1, 200})
	w.processWatching()
	if snap := w.BrokerSnapshot(); !brokerHasChannel(snap, restricted.GetUsername()) {
		t.Fatalf("restricted hard-priority channel missing from broker allocation %v", brokerChannels(snap))
	}
}

func TestCampaignPolicyOfflineWinnerCannotTakeBrokerSlot(t *testing.T) {
	w := newPolicyBrokerWatcher(t)
	now := time.Now()
	decisions := policy.Rank(policy.ModeEndingSoonest, policyBrokerInputs(policy.ModeEndingSoonest, now), now)
	w.SetCampaignSemanticClasses(policyClassesForWatcher(w, decisions))
	w.streamers[0].SetConfirmedOffline()
	seedPolicyBrokerWeights(t, w, now, []float64{100, 0, 1, 200})
	w.processWatching()
	if snap := w.BrokerSnapshot(); brokerHasChannel(snap, w.streamers[0].GetUsername()) {
		t.Fatalf("offline semantic winner took a broker slot: %v", brokerChannels(snap))
	}
}

func TestCampaignPolicyBrokerPermutationInvariant(t *testing.T) {
	orders := [][]int{
		{0, 1, 2, 3},
		{3, 2, 1, 0},
		{1, 3, 0, 2},
		{2, 0, 3, 1},
	}
	want := []string{"streamera", "streamerb"}
	for run, order := range orders {
		w := newPolicyBrokerWatcher(t)
		original := append([]*models.Streamer(nil), w.streamers...)
		for i, oldIdx := range order {
			w.streamers[i] = original[oldIdx]
		}
		now := time.Now()
		inputs := policyBrokerInputs(policy.ModeEndingSoonest, now)
		permuted := make([]policy.CampaignInput, len(inputs))
		for i, oldIdx := range order {
			permuted[i] = inputs[oldIdx]
		}
		decisions := policy.Rank(policy.ModeEndingSoonest, permuted, now)
		w.SetCampaignSemanticClasses(policyClassesForWatcher(w, decisions))
		seedPolicyBrokerWeightsByLogin(t, w, now, map[string]float64{
			"streamera": 100, "streamerb": 0, "streamerc": 1, "streamerd": 200,
		})
		w.rotation.lastWatched = make(map[int]time.Time)
		for idx, s := range w.streamers {
			switch s.GetUsername() {
			case "streamera":
				w.rotation.lastWatched[idx] = now.Add(-time.Hour)
			case "streamerb":
				w.rotation.lastWatched[idx] = now.Add(-10 * time.Minute)
			case "streamerc":
				w.rotation.lastWatched[idx] = now.Add(-20 * time.Minute)
			case "streamerd":
				w.rotation.lastWatched[idx] = now.Add(-2 * time.Hour)
			}
		}
		w.processWatching()
		got := brokerChannels(w.BrokerSnapshot())
		sort.Strings(got)
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("permutation %d order=%v allocation=%v, want %v", run, order, got, want)
		}
	}
}

func TestCampaignPolicyExactTiePermutationInvariant(t *testing.T) {
	orders := [][]int{
		{0, 1, 2, 3},
		{3, 2, 1, 0},
		{1, 3, 0, 2},
		{2, 0, 3, 1},
	}
	want := []string{"streamera", "streamerb"}
	for run, order := range orders {
		w := newPolicyBrokerWatcher(t)
		original := append([]*models.Streamer(nil), w.streamers...)
		for i, oldIdx := range order {
			w.streamers[i] = original[oldIdx]
		}
		classes := make(map[string]policy.SemanticClass, len(w.streamers))
		for _, s := range w.streamers {
			classes[s.GetUsername()] = 0
		}
		w.SetCampaignSemanticClasses(classes)
		w.processWatching()
		got := brokerChannels(w.BrokerSnapshot())
		sort.Strings(got)
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("exact-tie permutation %d order=%v allocation=%v, want login-deterministic %v", run, order, got, want)
		}
	}
}

func TestCampaignPolicyEqualClassBrokerFairnessSurvivesRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "watch.db")
	now := time.Now()
	first, firstDB := openWatchTimeStore(t, dbPath)
	for login, minutes := range map[string]float64{"streamera": 100, "streamerc": 1, "streamerd": 200} {
		if err := first.RecordMinutes(login, minutes, now); err != nil {
			t.Fatalf("first process record %s: %v", login, err)
		}
	}
	if err := firstDB.Close(); err != nil {
		t.Fatalf("close first process store: %v", err)
	}

	second, secondDB := openWatchTimeStore(t, dbPath)
	t.Cleanup(func() { _ = secondDB.Close() })
	w := newPolicyBrokerWatcher(t)
	w.store = second
	classes := make(map[string]policy.SemanticClass, len(w.streamers))
	for _, s := range w.streamers {
		classes[s.GetUsername()] = 0
	}
	w.SetCampaignSemanticClasses(classes)
	w.processWatching()
	snap := w.BrokerSnapshot()
	for _, login := range []string{"streamerb", "streamerc"} {
		if !brokerHasChannel(snap, login) {
			t.Fatalf("restart allocation %v lost persisted-deficit winner %s", brokerChannels(snap), login)
		}
	}
}

func TestCampaignPolicySemanticClassArbitratesConfiguredAndDiscovery(t *testing.T) {
	sender := &countingSender{sent: make(chan string, 16)}
	w, _ := newLoopWatcher(2, sender, &staticChecker{checked: make(chan string, 16)})
	for i, s := range w.streamers {
		s.Settings.ClaimDrops = true
		s.Settings.WatchStreak = false
		id := "campaign-" + string(rune('a'+i))
		s.Stream.SetCampaignIDs([]string{id})
		s.Stream.SetCampaigns([]*models.Campaign{{ID: id, Drops: []*models.Drop{{ID: "drop", MinutesRequired: 30}}}})
	}
	disco := discoveryStreamer("disco", false)
	w.AddSource(&staticSource{name: OriginDiscovery, cand: []Candidate{{Streamer: disco, Origin: OriginDiscovery}}})
	w.SetCampaignSemanticClasses(map[string]policy.SemanticClass{
		"streamera": 1,
		"streamerb": 1,
		"disco":     0,
	})
	seedPolicyBrokerWeightsByLogin(t, w, time.Now(), map[string]float64{
		"streamera": 0,
		"streamerb": 10,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w.ctx = ctx
	w.processWatching()
	snap := w.BrokerSnapshot()
	if !brokerHasChannel(snap, "disco") || !brokerHasChannel(snap, "streamera") || brokerHasChannel(snap, "streamerb") {
		t.Fatalf("cross-source semantic allocation = %v, want [disco streamera]", brokerChannels(snap))
	}
}

// TestCampaignPolicyBrokerTickUsesOneSemanticSnapshot forces a policy refresh
// at the real production boundary between configured Phase A and candidate
// arbitration. One broker allocation must be wholly explained by the snapshot
// it began with; it must never reclassify that already-selected pair with a
// different epoch halfway through the tick.
func TestCampaignPolicyBrokerTickUsesOneSemanticSnapshot(t *testing.T) {
	w := newPolicyBrokerWatcher(t)
	now := time.Now()
	initial := map[string]policy.SemanticClass{
		"streamera": 0,
		"streamerb": 1,
		"streamerc": 2,
		"streamerd": 3,
	}
	next := map[string]policy.SemanticClass{
		"streamera": 3,
		"streamerb": 2,
		"streamerc": 1,
		"streamerd": 0,
	}
	w.SetCampaignSemanticClasses(initial)
	seedPolicyBrokerWeights(t, w, now, []float64{100, 0, 1, 200})
	w.AddSource(&campaignPolicySwapSource{w: w, classes: next})

	w.processWatching()
	snap := w.BrokerSnapshot()
	if !brokerHasChannel(snap, "streamera") {
		t.Fatalf("initial semantic winner missing from broker allocation %v", brokerChannels(snap))
	}
	for _, slot := range snap.Slots {
		if slot.Channel == "streamera" && !strings.Contains(slot.Reason, "semantic class 0") {
			t.Fatalf("one broker tick mixed policy snapshots: streamera reason %q, want initial class 0", slot.Reason)
		}
	}
}

// TestCampaignPolicyBrokerTickUsesOneSnapshotAcrossDiscovery proves the
// late-candidate seam at the actual broker boundary. Discovery publishes exact
// eligible campaign IDs and a concurrent policy refresh replaces the global
// mapping before arbitration. The in-flight tick must still resolve both
// configured and discovery ordinals from its initial immutable snapshot; the
// next tick must then use the replacement snapshot in full.
func TestCampaignPolicyBrokerTickUsesOneSnapshotAcrossDiscovery(t *testing.T) {
	sender := &countingSender{sent: make(chan string, 16)}
	w, _ := newLoopWatcher(2, sender, &staticChecker{checked: make(chan string, 16)})
	w.priorities = []config.Priority{config.PriorityDrops}
	for i, s := range w.streamers {
		s.Settings.ClaimDrops = true
		s.Settings.WatchStreak = false
		id := "configured-" + string(rune('a'+i))
		s.Stream.SetCampaignIDs([]string{id})
		s.Stream.SetCampaigns([]*models.Campaign{{ID: id, Drops: []*models.Drop{{ID: "drop", MinutesRequired: 30}}}})
	}

	disco := discoveryStreamer("same_tick_discovery", false)
	discoCampaignID := "camp-" + disco.GetUsername()
	initialByLogin := map[string]policy.SemanticClass{
		"streamera": 1,
		"streamerb": 2,
	}
	initialByCampaign := map[string]policy.SemanticClass{discoCampaignID: 0}
	nextByLogin := map[string]policy.SemanticClass{
		"streamera": 0,
		"streamerb": 1,
	}
	nextByCampaign := map[string]policy.SemanticClass{discoCampaignID: 3}
	w.SetCampaignSemanticPolicy(initialByLogin, initialByCampaign, nil)
	w.AddSource(&discoveryPolicySwapSource{
		w:         w,
		candidate: Candidate{Streamer: disco, Origin: OriginDiscovery},
		facts: CandidateCampaignPolicy{
			SemanticClass: 0,
			Ranked:        true,
			CampaignIDs:   []string{discoCampaignID},
		},
		nextByLogin:    nextByLogin,
		nextByCampaign: nextByCampaign,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w.ctx = ctx

	w.processWatching()
	first := w.BrokerSnapshot()
	if !brokerHasChannel(first, disco.GetUsername()) || brokerHasChannel(first, "streamerb") {
		t.Fatalf("in-flight snapshot allocation = %v, want discovery class 0 to displace configured class 2", brokerChannels(first))
	}
	for _, slot := range first.Slots {
		if slot.Channel == disco.GetUsername() && !strings.Contains(slot.Reason, "semantic class 0") {
			t.Fatalf("in-flight discovery reason = %q, want initial semantic class 0", slot.Reason)
		}
	}

	w.processWatching()
	second := w.BrokerSnapshot()
	if brokerHasChannel(second, disco.GetUsername()) || !brokerHasChannel(second, "streamera") || !brokerHasChannel(second, "streamerb") {
		t.Fatalf("next-tick snapshot allocation = %v, want replacement policy to keep both configured channels", brokerChannels(second))
	}
}

// TestCampaignPolicyDiscoveryProposalChangeReachesActualBrokerAllocation is
// the broker half of discovery's production integration seam. The discovery
// package regression proves SetGameRanks changes WatchCandidates from the
// stale current to the strictly stronger channel; this test proves that exact
// CandidateSource proposal change reaches a real processWatching allocation
// with three contenders and never exceeds the global two-slot cap.
func TestCampaignPolicyDiscoveryProposalChangeReachesActualBrokerAllocation(t *testing.T) {
	sender := &countingSender{sent: make(chan string, 16)}
	w, _ := newLoopWatcher(2, sender, &staticChecker{checked: make(chan string, 16)})
	w.priorities = []config.Priority{config.PriorityDrops}
	for i, s := range w.streamers {
		s.Settings.ClaimDrops = true
		s.Settings.WatchStreak = false
		id := "campaign-" + string(rune('a'+i))
		s.Stream.SetCampaignIDs([]string{id})
	}

	weak := discoveryStreamer("discovery_game_one", false)
	strong := discoveryStreamer("discovery_game_two", false)
	source := &mutableSource{}
	source.set([]Candidate{{Streamer: weak, Origin: OriginDiscovery}})
	w.SetDiscoveryCandidatePolicy(weak.GetUsername(), CandidateCampaignPolicy{SemanticClass: 3, Ranked: true})
	w.AddSource(source)
	w.SetCampaignSemanticClasses(map[string]policy.SemanticClass{
		"streamera": 1,
		"streamerb": 2,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w.ctx = ctx

	w.processWatching()
	initial := w.BrokerSnapshot()
	if len(initial.Slots) != 2 || brokerHasChannel(initial, weak.GetUsername()) {
		t.Fatalf("weak initial discovery proposal allocation = %v, want two configured slots", brokerChannels(initial))
	}

	source.set([]Candidate{{Streamer: strong, Origin: OriginDiscovery}})
	w.SetDiscoveryCandidatePolicy(strong.GetUsername(), CandidateCampaignPolicy{SemanticClass: 0, Ranked: true})
	w.processWatching()
	final := w.BrokerSnapshot()
	if len(final.Slots) != 2 {
		t.Fatalf("proposal change allocated %d slots, want hard cap 2: %v", len(final.Slots), brokerChannels(final))
	}
	if !brokerHasChannel(final, strong.GetUsername()) || brokerHasChannel(final, weak.GetUsername()) {
		t.Fatalf("strong discovery proposal did not reach broker allocation: %v", brokerChannels(final))
	}
}

func TestDiscoveryCandidateRestrictedFactKeepsHardBrokerPrecedence(t *testing.T) {
	w := newPolicyBrokerWatcher(t)
	for i, s := range w.streamers {
		s.Stream.SetCampaigns([]*models.Campaign{{
			ID:    "configured-" + string(rune('a'+i)),
			Drops: []*models.Drop{{ID: "drop", MinutesRequired: 30}},
		}})
	}
	w.SetCampaignSemanticClasses(map[string]policy.SemanticClass{
		w.streamers[0].GetUsername(): 0,
		w.streamers[1].GetUsername(): 1,
	})
	restricted := discoveryStreamer("restricted_discovery", false)
	w.SetDiscoveryCandidatePolicy(restricted.GetUsername(), CandidateCampaignPolicy{
		SemanticClass: 9,
		Ranked:        true,
		Restricted:    true,
		Campaign:      "Allowed restricted campaign",
	})
	slots, _ := w.arbitrate([]int{0, 1}, []Candidate{{Streamer: restricted, Origin: OriginDiscovery}}, time.Now())
	if len(slots) != 2 || !loginsOf(slots)[restricted.GetUsername()] {
		t.Fatalf("restricted discovery hard fact did not displace active drop: %v", loginsOf(slots))
	}
	for _, slot := range slots {
		if slot.streamer == restricted && slot.reasonCode != ReasonRestrictedDrop {
			t.Fatalf("restricted discovery reason = %q, want %q", slot.reasonCode, ReasonRestrictedDrop)
		}
	}
}

func TestCampaignPolicyPreservesFreshStreakBoostAgainstActiveDrop(t *testing.T) {
	w, online := newTestWatcher(3)
	for _, s := range w.streamers {
		s.Settings.ClaimDrops = false
		s.Settings.WatchStreak = false
	}
	activeDrop := w.streamers[0]
	activeDrop.Settings.ClaimDrops = true
	activeDrop.SetConfirmedOnline()
	activeDrop.Stream.SetCampaignIDs([]string{"campaign-active"})
	freshStreak := w.streamers[2]
	freshStreak.Settings.WatchStreak = true
	freshStreak.Stream.WatchStreakMissing = true
	freshStreak.Stream.MinuteWatched = 0
	w.rotation.lastWatched = map[int]time.Time{
		0: time.Now(),
		2: time.Now().Add(-time.Hour),
	}
	if got := w.selectBoostTarget([2]int{0, 1}, online); got != 2 {
		t.Fatalf("active-drop base member suppressed the pre-existing fresh-streak boost: got target %d, want 2", got)
	}
}

func TestCampaignPolicyDeficitEvidenceRefreshesAcrossRenameAndRemoval(t *testing.T) {
	w := newPolicyBrokerWatcher(t)
	now := time.Now()
	seedPolicyBrokerWeights(t, w, now, []float64{0, 7, 0, 0})
	oldLogin := w.streamers[1].GetUsername()
	w.refreshDeficitMinutes([]int{0, 1, 2, 3}, now)
	if got := w.rotation.deficitMinutes[oldLogin]; got != 7 {
		t.Fatalf("initial persisted deficit = %v, want 7", got)
	}

	newLogin := "renamed-login"
	if err := w.store.RenameStreamer(oldLogin, newLogin); err != nil {
		t.Fatalf("rename persisted watch-time evidence: %v", err)
	}
	obs := w.streamers[1].BeginLoginObservation()
	if !w.streamers[1].RenameIfCurrent(newLogin, obs) {
		t.Fatal("rename streamer fixture")
	}
	w.refreshDeficitMinutes([]int{0, 1, 2, 3}, now)
	if _, stale := w.rotation.deficitMinutes[oldLogin]; stale {
		t.Fatalf("old-login deficit survived refresh after rename: %v", w.rotation.deficitMinutes)
	}
	if got := w.rotation.deficitMinutes[newLogin]; got != 7 {
		t.Fatalf("renamed persisted deficit = %v, want 7", got)
	}

	newList := append([]*models.Streamer(nil), w.streamers[:1]...)
	newList = append(newList, w.streamers[2:]...)
	w.applyStreamerList(newList)
	if len(w.rotation.deficitMinutes) != 0 {
		t.Fatalf("roster removal retained per-allocation deficit evidence: %v", w.rotation.deficitMinutes)
	}
}

func TestCampaignPolicyKeepsFairSeatProgressingAcrossLowerClasses(t *testing.T) {
	w := newPolicyBrokerWatcher(t)
	classes := map[string]policy.SemanticClass{w.streamers[0].GetUsername(): 0}
	for _, s := range w.streamers[1:] {
		classes[s.GetUsername()] = 1
	}
	w.SetCampaignSemanticClasses(classes)
	seen := make(map[string]int)
	for tick := 0; tick < 40; tick++ {
		forceRotate(w)
		pair := w.selectRotating([]int{0, 1, 2, 3})
		if len(pair) != 2 {
			t.Fatalf("tick %d allocated %d configured slots, want 2", tick, len(pair))
		}
		for _, idx := range pair {
			seen[w.streamers[idx].GetUsername()]++
		}
		if !contains(pair, 0) {
			t.Fatalf("tick %d lost strongest semantic campaign from pair %v", tick, pair)
		}
	}
	for _, s := range w.streamers[1:] {
		if seen[s.GetUsername()] == 0 {
			t.Fatalf("lower semantic class %s starved while the second deficit-fair seat rotated: %v", s.GetUsername(), seen)
		}
	}
}

func policyBrokerInputs(mode policy.Mode, now time.Time) []policy.CampaignInput {
	inputs := make([]policy.CampaignInput, 4)
	for i := range inputs {
		inputs[i] = policy.CampaignInput{
			CampaignID:           "campaign-" + string(rune('a'+i)),
			GameOrderIndex:       i,
			EndAt:                now.Add(48 * time.Hour),
			Drops:                []policy.DropStep{{MinutesRequired: 30}},
			EligibleLiveChannels: 4,
			ChannelStability:     1,
			StabilitySamples:     20,
		}
	}
	switch mode {
	case policy.ModeEndingSoonest:
		for i := range inputs {
			inputs[i].EndAt = now.Add(time.Duration(1+i*4) * time.Hour)
		}
	case policy.ModeClosestToReward:
		for i, watched := range []int{29, 20, 10, 0} {
			inputs[i].Drops[0].CurrentMinutesWatched = watched
		}
	case policy.ModeLowAvailability:
		for i := range inputs {
			inputs[i].EligibleLiveChannels = i + 1
		}
	case policy.ModeSmart:
		inputs[0].EndAt = now.Add(time.Hour)
		inputs[0].Drops[0].CurrentMinutesWatched = 29
		inputs[0].EligibleLiveChannels = 1
	}
	return inputs
}

func policyClassesForWatcher(w *MinuteWatcher, decisions []policy.Decision) map[string]policy.SemanticClass {
	classes := make(map[string]policy.SemanticClass, len(decisions))
	for _, d := range decisions {
		if d.Excluded {
			continue
		}
		for _, s := range w.streamers {
			for _, campaignID := range s.Stream.GetCampaignIDs() {
				if campaignID == d.CampaignID {
					classes[s.GetUsername()] = d.SemanticClass
				}
			}
		}
	}
	return classes
}

func campaignDecisionIDs(decisions []policy.Decision) []string {
	ids := make([]string, len(decisions))
	for i, d := range decisions {
		ids[i] = d.CampaignID
	}
	return ids
}

func seedPolicyBrokerWeightsByLogin(t *testing.T, w *MinuteWatcher, now time.Time, weights map[string]float64) {
	t.Helper()
	store, db := openWatchTimeStore(t, filepath.Join(t.TempDir(), "watch.db"))
	t.Cleanup(func() { _ = db.Close() })
	w.store = store
	for login, minutes := range weights {
		if minutes == 0 {
			continue
		}
		if err := store.RecordMinutes(login, minutes, now); err != nil {
			t.Fatalf("seed watch-time deficit for %s: %v", login, err)
		}
	}
}
