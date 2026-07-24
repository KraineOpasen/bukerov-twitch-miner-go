package miner

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// fakeTopicReconciler mimics the pool's EnsureTopic desired-state contract: a
// set of subscribed topics, LISTEN transitions counted only absent->present,
// removals only present->absent, plus per-topic one-shot injected failures —
// so miner-level tests observe exactly the actions a real pool would take.
type fakeTopicReconciler struct {
	mu       sync.Mutex
	topics   map[string]bool
	listens  map[string]int
	removes  map[string]int
	failOnce map[string]error
	touched  map[string]bool // every topic key ever named, desired true or false
}

func newFakeTopicReconciler() *fakeTopicReconciler {
	return &fakeTopicReconciler{
		topics:   make(map[string]bool),
		listens:  make(map[string]int),
		removes:  make(map[string]int),
		failOnce: make(map[string]error),
		touched:  make(map[string]bool),
	}
}

func (f *fakeTopicReconciler) EnsureTopic(t pubsub.Topic, desired bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := t.String()
	f.touched[key] = true
	if desired {
		if err, ok := f.failOnce[key]; ok {
			delete(f.failOnce, key)
			return err
		}
		if !f.topics[key] {
			f.topics[key] = true
			f.listens[key]++
		}
		return nil
	}
	if f.topics[key] {
		delete(f.topics, key)
		f.removes[key]++
	}
	return nil
}

func (f *fakeTopicReconciler) has(t pubsub.Topic) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.topics[t.String()]
}

func (f *fakeTopicReconciler) listenCount(t pubsub.Topic) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listens[t.String()]
}

func (f *fakeTopicReconciler) setFailOnce(t pubsub.Topic, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOnce[t.String()] = err
}

func (f *fakeTopicReconciler) touchedUserTopic() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.touched {
		if strings.HasPrefix(key, string(pubsub.TopicCommunityPointsUser)+".") ||
			strings.HasPrefix(key, string(pubsub.TopicPredictionsUser)+".") {
			return true
		}
	}
	return false
}

// fakeChatReconciler records the chat-presence reconciliation calls.
type fakeChatReconciler struct {
	mu      sync.Mutex
	toggles map[string]int
	leaves  map[string]int
}

func newFakeChatReconciler() *fakeChatReconciler {
	return &fakeChatReconciler{toggles: make(map[string]int), leaves: make(map[string]int)}
}

func (f *fakeChatReconciler) ToggleChat(s *models.Streamer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toggles[s.Username]++
}

func (f *fakeChatReconciler) Leave(username string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaves[username]++
}

func (f *fakeChatReconciler) toggleCount(username string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.toggles[username]
}

func (f *fakeChatReconciler) leaveCount(username string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leaves[username]
}

func boolPtr(b bool) *bool { return &b }

// newCapabilityMiner builds a miner over a seeded manager with the runtime
// reconciliation seams injected — no network, no pool, no IRC.
func newCapabilityMiner(t *testing.T, usernames ...string) (*Miner, *fakeTopicReconciler, *fakeChatReconciler) {
	t.Helper()
	cfg := &config.Config{
		Username:         "tester",
		StreamerSettings: models.DefaultStreamerSettings(),
		Priority:         []config.Priority{config.PriorityOrder},
		RateLimits:       config.RateLimitSettings{MinuteWatchedInterval: 60},
	}
	for _, u := range usernames {
		cfg.Streamers = append(cfg.Streamers, config.StreamerConfig{Username: u})
	}

	mgr := streamer.NewManager(fakeStreamerAPI{}, cfg.StreamerSettings)
	if added, _, _, _ := mgr.ApplySettings(cfg.Streamers, cfg.StreamerSettings); len(added) != len(usernames) {
		t.Fatalf("seeding failed: added=%d, want %d", len(added), len(usernames))
	}

	topics := newFakeTopicReconciler()
	chatRec := newFakeChatReconciler()
	m := &Miner{
		config:             cfg,
		streamers:          mgr,
		capabilityTopics:   topics,
		chatPresence:       chatRec,
		streamCheckTrigger: make(chan struct{}, 1),
		autoRedeemState:    make(map[string]*autoRedeemRuntime),
	}
	return m, topics, chatRec
}

func overrideStreamer(rs *settings.RuntimeSettings, username string, mutate func(*settings.StreamerSettingsConfig)) {
	for i := range rs.Streamers {
		if rs.Streamers[i].Username == username {
			if rs.Streamers[i].Settings == nil {
				rs.Streamers[i].Settings = &settings.StreamerSettingsConfig{}
			}
			mutate(rs.Streamers[i].Settings)
			return
		}
	}
}

func channelTopic(tt pubsub.TopicType, username string) pubsub.Topic {
	return pubsub.NewTopic(tt, "chan-"+username)
}

// TestApplySettingsCapabilityTogglesReconcileExisting covers the runtime
// desired-state mapping for an EXISTING streamer, without restart: each
// optional topic follows its flag, the playback topic never leaves, the global
// user topics are never named, and a repeated identical apply is idempotent.
func TestApplySettingsCapabilityTogglesReconcileExisting(t *testing.T) {
	m, topics, _ := newCapabilityMiner(t, "alpha")

	// Initial apply: defaults (FollowRaid/MakePredictions/ClaimMoments on,
	// CommunityGoals off).
	m.ApplySettings(m.GetRuntimeSettings())

	for _, tt := range []pubsub.TopicType{pubsub.TopicVideoPlaybackByID, pubsub.TopicRaid, pubsub.TopicPredictionsChannel, pubsub.TopicCommunityMomentsChannel} {
		if !topics.has(channelTopic(tt, "alpha")) {
			t.Fatalf("topic %s missing after initial apply", tt)
		}
	}
	if topics.has(channelTopic(pubsub.TopicCommunityPointsChannel, "alpha")) {
		t.Fatal("community-points-channel present although CommunityGoals=false")
	}

	// Toggle several flags in ONE apply: FollowRaid off, CommunityGoals on,
	// ClaimMoments off, MakePredictions stays on.
	rs := m.GetRuntimeSettings()
	overrideStreamer(&rs, "alpha", func(sc *settings.StreamerSettingsConfig) {
		sc.FollowRaid = boolPtr(false)
		sc.CommunityGoals = boolPtr(true)
		sc.ClaimMoments = boolPtr(false)
	})
	m.ApplySettings(rs)

	if topics.has(channelTopic(pubsub.TopicRaid, "alpha")) {
		t.Fatal("raid topic still present after FollowRaid=false")
	}
	if topics.has(channelTopic(pubsub.TopicCommunityMomentsChannel, "alpha")) {
		t.Fatal("moments topic still present after ClaimMoments=false")
	}
	if !topics.has(channelTopic(pubsub.TopicCommunityPointsChannel, "alpha")) {
		t.Fatal("community-points-channel missing after CommunityGoals=true")
	}
	if !topics.has(channelTopic(pubsub.TopicPredictionsChannel, "alpha")) {
		t.Fatal("predictions topic lost although MakePredictions stayed true")
	}
	if !topics.has(channelTopic(pubsub.TopicVideoPlaybackByID, "alpha")) {
		t.Fatal("always-on playback topic was removed by a capability toggle")
	}
	if got := topics.listenCount(channelTopic(pubsub.TopicVideoPlaybackByID, "alpha")); got != 1 {
		t.Fatalf("playback LISTEN transitions = %d, want 1 (kept, not re-subscribed)", got)
	}
	if got := topics.listenCount(channelTopic(pubsub.TopicCommunityPointsChannel, "alpha")); got != 1 {
		t.Fatalf("goals topic appeared %d times, want exactly once", got)
	}

	// Repeated identical apply: no new LISTEN transitions, nothing lost.
	m.ApplySettings(rs)
	if got := topics.listenCount(channelTopic(pubsub.TopicCommunityPointsChannel, "alpha")); got != 1 {
		t.Fatalf("identical re-apply produced a duplicate LISTEN: transitions = %d", got)
	}
	if !topics.has(channelTopic(pubsub.TopicVideoPlaybackByID, "alpha")) {
		t.Fatal("identical re-apply dropped the playback topic")
	}

	if topics.touchedUserTopic() {
		t.Fatal("per-streamer reconciliation must never name the global user topics")
	}
}

// TestApplySettingsCapabilityFlagCycles: each optional flag reconciles cleanly
// through off->on->off without restart.
func TestApplySettingsCapabilityFlagCycles(t *testing.T) {
	cases := []struct {
		name  string
		topic pubsub.TopicType
		set   func(*settings.StreamerSettingsConfig, bool)
	}{
		{"FollowRaid", pubsub.TopicRaid, func(sc *settings.StreamerSettingsConfig, v bool) { sc.FollowRaid = boolPtr(v) }},
		{"MakePredictions", pubsub.TopicPredictionsChannel, func(sc *settings.StreamerSettingsConfig, v bool) { sc.MakePredictions = boolPtr(v) }},
		{"ClaimMoments", pubsub.TopicCommunityMomentsChannel, func(sc *settings.StreamerSettingsConfig, v bool) { sc.ClaimMoments = boolPtr(v) }},
		{"CommunityGoals", pubsub.TopicCommunityPointsChannel, func(sc *settings.StreamerSettingsConfig, v bool) { sc.CommunityGoals = boolPtr(v) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, topics, _ := newCapabilityMiner(t, "alpha")
			topic := channelTopic(tc.topic, "alpha")

			rs := m.GetRuntimeSettings()
			overrideStreamer(&rs, "alpha", func(sc *settings.StreamerSettingsConfig) { tc.set(sc, false) })
			m.ApplySettings(rs)
			if topics.has(topic) {
				t.Fatalf("%s=false: topic present", tc.name)
			}

			rs = m.GetRuntimeSettings()
			overrideStreamer(&rs, "alpha", func(sc *settings.StreamerSettingsConfig) { tc.set(sc, true) })
			m.ApplySettings(rs)
			if !topics.has(topic) {
				t.Fatalf("%s=true: topic missing", tc.name)
			}
			if got := topics.listenCount(topic); got != 1 {
				t.Fatalf("%s enable produced %d LISTEN transitions, want 1", tc.name, got)
			}

			rs = m.GetRuntimeSettings()
			overrideStreamer(&rs, "alpha", func(sc *settings.StreamerSettingsConfig) { tc.set(sc, false) })
			m.ApplySettings(rs)
			if topics.has(topic) {
				t.Fatalf("%s=false (again): topic present", tc.name)
			}
			if !topics.has(channelTopic(pubsub.TopicVideoPlaybackByID, "alpha")) {
				t.Fatalf("%s cycle removed the always-on playback topic", tc.name)
			}
		})
	}
}

// TestApplySettingsPartialFailureRetriesOnIdenticalApply is the healing
// contract: a failed raid subscription does not roll back the saved settings,
// does not block the other capabilities of the SAME apply, and the identical
// second apply re-attempts exactly the missing subscription.
func TestApplySettingsPartialFailureRetriesOnIdenticalApply(t *testing.T) {
	m, topics, chatRec := newCapabilityMiner(t, "alpha")
	raid := channelTopic(pubsub.TopicRaid, "alpha")

	// Seed with FollowRaid off, then enable it while the first subscription
	// attempt fails.
	rs := m.GetRuntimeSettings()
	overrideStreamer(&rs, "alpha", func(sc *settings.StreamerSettingsConfig) { sc.FollowRaid = boolPtr(false) })
	m.ApplySettings(rs)

	topics.setFailOnce(raid, context.DeadlineExceeded)
	rs = m.GetRuntimeSettings()
	overrideStreamer(&rs, "alpha", func(sc *settings.StreamerSettingsConfig) { sc.FollowRaid = boolPtr(true) })
	m.ApplySettings(rs)

	if topics.has(raid) {
		t.Fatal("setup: first raid subscription attempt should have failed")
	}
	// Settings stay enabled — no rollback on a transient subscribe failure.
	if got := m.streamers.Get("alpha").GetSettings().FollowRaid; !got {
		t.Fatal("FollowRaid was rolled back by the failed subscription")
	}
	// The other capabilities of the same apply were not skipped.
	if !topics.has(channelTopic(pubsub.TopicPredictionsChannel, "alpha")) ||
		!topics.has(channelTopic(pubsub.TopicVideoPlaybackByID, "alpha")) {
		t.Fatal("independent capability actions were skipped because one failed")
	}
	if chatRec.toggleCount("alpha") == 0 {
		t.Fatal("chat reconciliation was skipped because a topic action failed")
	}

	// Identical second apply heals the drift.
	m.ApplySettings(rs)
	if !topics.has(raid) {
		t.Fatal("identical re-apply did not retry the failed subscription")
	}
	if got := topics.listenCount(raid); got != 1 {
		t.Fatalf("raid topic exists %d times after healing, want exactly once", got)
	}
}

// TestApplySettingsAddRemoveAndTogglesOneApply — one apply simultaneously adds
// delta, removes bravo, and changes charlie's capabilities; roster, topics and
// chat must each reconcile exactly once, with no ghost of the removed
// streamer.
func TestApplySettingsAddRemoveAndTogglesOneApply(t *testing.T) {
	m, topics, chatRec := newCapabilityMiner(t, "alpha", "bravo", "charlie")
	m.ApplySettings(m.GetRuntimeSettings()) // seed the runtime topic state

	if !topics.has(channelTopic(pubsub.TopicVideoPlaybackByID, "bravo")) {
		t.Fatal("setup: bravo playback topic missing")
	}

	rs := m.GetRuntimeSettings()
	var keep []settings.StreamerConfig
	for _, sc := range rs.Streamers {
		if sc.Username != "bravo" {
			keep = append(keep, sc)
		}
	}
	rs.Streamers = append(keep, settings.StreamerConfig{Username: "delta"})
	overrideStreamer(&rs, "charlie", func(sc *settings.StreamerSettingsConfig) {
		sc.FollowRaid = boolPtr(false)
		sc.CommunityGoals = boolPtr(true)
	})
	m.ApplySettings(rs)

	// Roster.
	if m.streamers.Get("bravo") != nil {
		t.Fatal("removed streamer still in roster")
	}
	if m.streamers.Get("delta") == nil {
		t.Fatal("added streamer missing from roster")
	}

	// Removed: every per-channel topic gone (playback included), chat left.
	for _, tt := range []pubsub.TopicType{pubsub.TopicVideoPlaybackByID, pubsub.TopicRaid, pubsub.TopicPredictionsChannel, pubsub.TopicCommunityMomentsChannel, pubsub.TopicCommunityPointsChannel} {
		if topics.has(channelTopic(tt, "bravo")) {
			t.Fatalf("ghost topic %s for removed streamer", tt)
		}
	}
	if chatRec.leaveCount("bravo") == 0 {
		t.Fatal("removed streamer did not leave chat")
	}

	// Added: full desired state.
	if !topics.has(channelTopic(pubsub.TopicVideoPlaybackByID, "delta")) ||
		!topics.has(channelTopic(pubsub.TopicRaid, "delta")) {
		t.Fatal("added streamer's topics missing")
	}
	if chatRec.toggleCount("delta") == 0 {
		t.Fatal("added streamer's chat presence was not reconciled")
	}

	// Changed: toggles applied exactly once.
	if topics.has(channelTopic(pubsub.TopicRaid, "charlie")) {
		t.Fatal("charlie raid topic still present after FollowRaid=false")
	}
	if !topics.has(channelTopic(pubsub.TopicCommunityPointsChannel, "charlie")) {
		t.Fatal("charlie goals topic missing after CommunityGoals=true")
	}
	if got := topics.listenCount(channelTopic(pubsub.TopicCommunityPointsChannel, "charlie")); got != 1 {
		t.Fatalf("charlie goals topic appeared %d times, want exactly once", got)
	}
	// Unchanged alpha is untouched but still fully subscribed.
	if !topics.has(channelTopic(pubsub.TopicVideoPlaybackByID, "alpha")) {
		t.Fatal("unchanged streamer lost its playback topic")
	}
}

// TestApplySettingsImmediateChatReconcile: a Chat-mode change on an existing
// streamer reaches ChatManager.ToggleChat inside the SAME ApplySettings call —
// not on the next stream-check tick.
func TestApplySettingsImmediateChatReconcile(t *testing.T) {
	m, _, chatRec := newCapabilityMiner(t, "alpha")

	rs := m.GetRuntimeSettings()
	chatAlways := string(models.ChatAlways)
	overrideStreamer(&rs, "alpha", func(sc *settings.StreamerSettingsConfig) { sc.Chat = &chatAlways })
	m.ApplySettings(rs)

	if chatRec.toggleCount("alpha") == 0 {
		t.Fatal("chat mode change was not reconciled immediately within ApplySettings")
	}
	if got := m.streamers.Get("alpha").GetSettings().Chat; got != models.ChatAlways {
		t.Fatalf("chat setting = %q, want ALWAYS", got)
	}

	// Repeated identical apply stays idempotent (ToggleChat is idempotent by
	// contract; here we just prove it is invoked each apply, never skipped).
	before := chatRec.toggleCount("alpha")
	m.ApplySettings(rs)
	if chatRec.toggleCount("alpha") <= before {
		t.Fatal("identical re-apply skipped the chat reconciliation sweep")
	}
}

// TestApplySettingsRuntimeConcurrentStorm — concurrency harness: repeated
// concurrent applies of two setting variants, roster/settings readers, and a
// live watch loop; must be race-free, deadlock-free, converge to the final
// desired topic state, and never exceed two watch slots.
func TestApplySettingsRuntimeConcurrentStorm(t *testing.T) {
	m, topics, _ := newCapabilityMiner(t, "alpha", "bravo", "charlie")

	w := watcher.NewMinuteWatcher(nil, m.streamers.All(), m.config.Priority, m.config.RateLimits, nil)
	m.watcher = w
	for _, s := range m.streamers.All() {
		s.SetConfirmedOnline()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	defer w.Stop()

	// Build both variants up front (BuildRuntimeSettings reads the live config,
	// which concurrent applies mutate under the miner lock).
	rsOn := m.GetRuntimeSettings()
	overrideStreamer(&rsOn, "alpha", func(sc *settings.StreamerSettingsConfig) { sc.FollowRaid = boolPtr(true) })
	rsOff := m.GetRuntimeSettings()
	overrideStreamer(&rsOff, "alpha", func(sc *settings.StreamerSettingsConfig) { sc.FollowRaid = boolPtr(false) })

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				if (g+i)%2 == 0 {
					m.ApplySettings(rsOn)
				} else {
					m.ApplySettings(rsOff)
				}
			}
		}(g)
	}
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				for _, s := range m.streamers.All() {
					_ = s.GetSettings()
					_ = s.DropsCondition()
				}
				_ = m.GetRuntimeSettings()
			}
		}()
	}
	wg.Wait()

	// Converge deterministically and assert the final desired state.
	m.ApplySettings(rsOff)
	if topics.has(channelTopic(pubsub.TopicRaid, "alpha")) {
		t.Fatal("raid topic present after final FollowRaid=false")
	}
	if !topics.has(channelTopic(pubsub.TopicVideoPlaybackByID, "alpha")) {
		t.Fatal("playback topic lost during the storm")
	}
	m.ApplySettings(rsOn)
	if !topics.has(channelTopic(pubsub.TopicRaid, "alpha")) {
		t.Fatal("raid topic missing after final FollowRaid=true")
	}

	watched := 0
	for _, d := range w.GetDebugState().Decisions {
		if d.Watching {
			watched++
		}
	}
	if watched > 2 {
		t.Fatalf("watch slots exceeded two: %d streamers watched", watched)
	}
}
