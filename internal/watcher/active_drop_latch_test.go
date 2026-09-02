package watcher

import (
	"sort"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
)

func streamersByLogin(streamers []*models.Streamer) map[string]*models.Streamer {
	byLogin := make(map[string]*models.Streamer, len(streamers))
	for _, streamer := range streamers {
		byLogin[streamer.GetUsername()] = streamer
	}
	return byLogin
}

func assertBrokerMatchesFairPair(t *testing.T, w *MinuteWatcher, want []string) {
	t.Helper()
	snap := w.BrokerSnapshot()
	if len(snap.Slots) != constants.MaxSimultaneousStreams {
		t.Fatalf("broker allocated %d slots, want cap %d: %v", len(snap.Slots), constants.MaxSimultaneousStreams, brokerChannels(snap))
	}
	got := brokerChannels(snap)
	sort.Strings(got)
	want = append([]string(nil), want...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("broker channels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("broker channels = %v, want %v", got, want)
		}
	}
	base := sortedPair(w.GetDebugState().ActivePair)
	if len(base) != len(want) {
		t.Fatalf("fair base = %v, want %v", base, want)
	}
	for i := range want {
		if base[i] != want[i] {
			t.Fatalf("final slots did not converge to fair base: base=%v final=%v want=%v", base, got, want)
		}
	}
}

func TestProcessWatchingStreakTerminalReleasesUnrestrictedActiveDropLatch(t *testing.T) {
	terminalCases := []struct {
		name      string
		terminal  models.WatchStreakState
		terminate func(*testing.T, *models.Streamer)
	}{
		{
			name:     "bound grant",
			terminal: models.WatchStreakGranted,
			terminate: func(t *testing.T, streamer *models.Streamer) {
				acceptBoundStreakForWatcherTest(t, streamer.Stream, "grant-"+streamer.GetUsername(), streamer.Stream.GetBroadcastID())
			},
		},
		{
			name:     "bounded timeout",
			terminal: models.WatchStreakTimedOutUnknown,
			terminate: func(_ *testing.T, streamer *models.Streamer) {
				streamer.Stream.MinuteWatched = models.WatchStreakPursuitCapMinutes
			},
		},
	}
	permutations := [][]int{
		{0, 1, 2, 3},
		{3, 2, 1, 0},
		{1, 3, 0, 2},
		{2, 0, 3, 1},
	}

	for _, terminalCase := range terminalCases {
		for permutationIndex, permutation := range permutations {
			t.Run(terminalCase.name+"/permutation-"+string(rune('a'+permutationIndex)), func(t *testing.T) {
				w := newPolicyBrokerWatcher(t)
				original := append([]*models.Streamer(nil), w.streamers...)
				for i, originalIndex := range permutation {
					w.streamers[i] = original[originalIndex]
				}
				byLogin := streamersByLogin(w.streamers)
				w.SetCampaignSemanticClasses(map[string]policy.SemanticClass{
					"streamera": 1,
					"streamerb": 1,
					"streamerc": 1,
					"streamerd": 1,
				})
				for _, login := range []string{"streamerc", "streamerd"} {
					streamer := byLogin[login]
					streamer.Settings.WatchStreak = true
					streamer.Stream.Update("broadcast-"+login, "", nil, nil, 1)
					streamer.Stream.MinuteWatched = 5
				}
				seedPolicyBrokerWeightsByLogin(t, w, time.Now(), map[string]float64{
					"streamera": 0,
					"streamerb": 1,
					"streamerc": 10,
					"streamerd": 20,
				})

				w.processWatching(tickCtx(w))
				first := w.BrokerSnapshot()
				if len(first.Slots) != constants.MaxSimultaneousStreams || !brokerHasChannel(first, "streamerc") {
					t.Fatalf("initial streak target was not latched at cap %d: %v", constants.MaxSimultaneousStreams, brokerChannels(first))
				}
				if !w.rotation.boostLatched || w.streamers[w.rotation.boostTarget].GetUsername() != "streamerc" {
					t.Fatalf("initial boost latch = latched:%v target:%d, want streamerc", w.rotation.boostLatched, w.rotation.boostTarget)
				}

				for _, login := range []string{"streamera", "streamerb", "streamerc"} {
					if err := w.store.RecordMinutes(login, 100, time.Now()); err != nil {
						t.Fatalf("advance fairness for %s: %v", login, err)
					}
				}
				w.processWatching(tickCtx(w))
				pursuingBase := sortedPair(w.GetDebugState().ActivePair)
				if len(pursuingBase) != 2 || pursuingBase[0] != "streamera" || pursuingBase[1] != "streamerd" {
					t.Fatalf("test did not replace the fair pair while streak pursued: %v", pursuingBase)
				}
				pursuing := w.BrokerSnapshot()
				if len(pursuing.Slots) != constants.MaxSimultaneousStreams || !brokerHasChannel(pursuing, "streamerc") || !brokerHasChannel(pursuing, "streamerd") {
					t.Fatalf("PURSUING continuity did not survive fair-pair replacement: base=%v final=%v", pursuingBase, brokerChannels(pursuing))
				}

				for _, login := range []string{"streamerc", "streamerd"} {
					terminalCase.terminate(t, byLogin[login])
				}
				w.processWatching(tickCtx(w))
				for _, login := range []string{"streamerc", "streamerd"} {
					streamer := byLogin[login]
					decision := streamer.Stream.EvaluateWatchStreak(time.Now())
					if decision.State != terminalCase.terminal {
						t.Fatalf("%s state = %s, want %s", login, decision.State, terminalCase.terminal)
					}
					if !streamer.DropsCondition() {
						t.Fatalf("%s no longer has the unrestricted active drop needed by the regression", login)
					}
				}
				assertBrokerMatchesFairPair(t, w, []string{"streamera", "streamerd"})

				for repeat := 0; repeat < 3; repeat++ {
					w.processWatching(tickCtx(w))
					assertBrokerMatchesFairPair(t, w, []string{"streamera", "streamerd"})
				}
			})
		}
	}
}

func TestBoundedSecondaryUtilityDoesNotCreatePermanentDropIncumbency(t *testing.T) {
	w := newPolicyBrokerWatcher(t)
	w.SetCampaignSemanticPolicy(map[string]policy.SemanticUtility{
		"streamera": {SemanticClass: 1, PrimaryCampaignID: "campaign-a"},
		"streamerb": {SemanticClass: 1, PrimaryCampaignID: "campaign-b"},
		"streamerc": {
			SemanticClass:          0,
			SecondarySemanticClass: 2,
			HasSecondary:           true,
			PrimaryCampaignID:      "campaign-c",
			SecondaryCampaignID:    "campaign-c-secondary",
		},
		"streamerd": {SemanticClass: 0, PrimaryCampaignID: "campaign-d"},
	}, nil, nil)
	seedPolicyBrokerWeightsByLogin(t, w, time.Now(), map[string]float64{
		"streamera": 0,
		"streamerb": 1,
		"streamerc": 10,
		"streamerd": 5,
	})

	w.processWatching(tickCtx(w))
	first := w.BrokerSnapshot()
	if len(first.Slots) != constants.MaxSimultaneousStreams || !brokerHasChannel(first, "streamerc") {
		t.Fatalf("bounded secondary utility did not break the primary tie: %v", brokerChannels(first))
	}
	if reason := brokerReason(first, "streamerc"); reason == "" {
		t.Fatal("bounded secondary winner has no broker reason")
	}

	w.SetCampaignSemanticPolicy(map[string]policy.SemanticUtility{
		"streamera": {SemanticClass: 1, PrimaryCampaignID: "campaign-a"},
		"streamerb": {SemanticClass: 1, PrimaryCampaignID: "campaign-b"},
		"streamerc": {SemanticClass: 0, PrimaryCampaignID: "campaign-c"},
		"streamerd": {SemanticClass: 0, PrimaryCampaignID: "campaign-d"},
	}, nil, nil)
	w.processWatching(tickCtx(w))
	second := w.BrokerSnapshot()
	if len(second.Slots) != constants.MaxSimultaneousStreams || !brokerHasChannel(second, "streamerd") || brokerHasChannel(second, "streamerc") {
		t.Fatalf("removed secondary utility created permanent incumbency: %v", brokerChannels(second))
	}
}
