package chat

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
)

// fakeRosterClient satisfies streamer.Manager's twitchClient slice with no
// network I/O (GetChannelID is a deterministic, injective function of the
// login), so these tests exercise a REAL streamer.Manager — not a hand-rolled
// stand-in — against a REAL ChatManager wired with SetRosterMembership.
type fakeRosterClient struct{}

func (fakeRosterClient) GetChannelID(username string) (string, error) {
	return "chan-" + username, nil
}
func (fakeRosterClient) LoadChannelPointsContext(*models.Streamer) error { return nil }
func (fakeRosterClient) CheckStreamerOnline(*models.Streamer) models.StatusTransition {
	return models.StatusTransition{}
}

// rosterSettings returns default streamer settings with Chat overridden, for
// building config.StreamerConfig entries fed to streamer.Manager.ApplySettings.
func rosterSettings(mode models.ChatPresence) *models.StreamerSettings {
	s := models.DefaultStreamerSettings()
	s.Chat = mode
	return &s
}

func rosterEntry(username string, mode models.ChatPresence) config.StreamerConfig {
	return config.StreamerConfig{Username: username, Settings: rosterSettings(mode)}
}

// clientPtr exposes the manager's raw *IRCClient for a login, so a test can
// prove a rejected join left an existing client instance completely
// untouched (not merely "still connected", but literally the SAME object).
func (m *ChatManager) clientPtr(username string) *IRCClient {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clients[username]
}

// newGatedManager builds a real streamer.Manager plus a runtime ChatManager
// (in-memory IRC transport, dial-counted) wired with SetRosterMembership
// using the SAME pointer-identity predicate the miner installs in production
// (Miner.chatRosterMembership in internal/miner/miner.go): the roster's
// current object for a streamer's login must be that EXACT pointer.
func newGatedManager() (*streamer.Manager, *ChatManager, *int32) {
	mgr := streamer.NewManager(fakeRosterClient{}, models.DefaultStreamerSettings())
	cm, dials := newRuntimeChatManager()
	cm.SetRosterMembership(func(s *models.Streamer) bool {
		return mgr.Get(s.GetUsername()) == s
	})
	return mgr, cm, dials
}

// TestJoinChatRejectsStaleToggleAfterLeave is the ghost-client reproduction
// (M3's confirmed defect): a *models.Streamer obtained before removal, held
// past ChatManager.Leave (the teardown removal actually performs), must be
// rejected by a later ToggleChat instead of re-joining and creating a
// persistent IRC client nothing will ever leave again.
func TestJoinChatRejectsStaleToggleAfterLeave(t *testing.T) {
	mgr, cm, dials := newGatedManager()

	added, _, _, _ := mgr.ApplySettings([]config.StreamerConfig{rosterEntry("chan", models.ChatAlways)}, models.DefaultStreamerSettings())
	if len(added) != 1 {
		t.Fatalf("setup: added = %d, want 1", len(added))
	}
	stale := mgr.Get("chan")
	if stale == nil {
		t.Fatal("setup: streamer not tracked")
	}

	cm.ToggleChat(stale)
	if !cm.hasConnection("chan") {
		t.Fatal("setup: initial ToggleChat should have joined")
	}
	if got := atomic.LoadInt32(dials); got != 1 {
		t.Fatalf("dials = %d, want 1", got)
	}

	// Remove "chan" the way a capability reconcile does: ApplySettings with
	// it no longer named, followed by the explicit Leave teardown call.
	mgr.ApplySettings(nil, models.DefaultStreamerSettings())
	cm.Leave("chan")
	if cm.hasConnection("chan") {
		t.Fatal("setup: Leave should have torn down the connection")
	}

	// The reproduction: a delayed ToggleChat with the STALE pointer (as if
	// from an older All() snapshot still being iterated by
	// checkAllStreamers/checkUncheckedStreamers, or a startMining initial
	// sweep racing the removal) must be rejected — no join, no new dial.
	cm.ToggleChat(stale)
	if cm.hasConnection("chan") {
		t.Fatal("stale ToggleChat after Leave must not re-join (ghost IRC client)")
	}
	if got := atomic.LoadInt32(dials); got != 1 {
		t.Fatalf("dials = %d after stale ToggleChat, want still 1 (no ghost dial)", got)
	}
}

// TestJoinChatPermitsCurrentRosterMemberAllModes proves the roster gate is
// invisible to a legitimate join: a streamer the manager still tracks under
// its login joins normally in every Chat mode, exactly as without the
// predicate.
func TestJoinChatPermitsCurrentRosterMemberAllModes(t *testing.T) {
	cases := []struct {
		name string
		mode models.ChatPresence
		prep func(s *models.Streamer)
	}{
		{"ALWAYS", models.ChatAlways, func(*models.Streamer) {}},
		{"ONLINE", models.ChatOnline, func(s *models.Streamer) { s.SetConfirmedOnline() }},
		{"OFFLINE", models.ChatOffline, func(s *models.Streamer) { s.SetConfirmedOffline() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, cm, dials := newGatedManager()
			added, _, _, _ := mgr.ApplySettings([]config.StreamerConfig{rosterEntry("chan", tc.mode)}, models.DefaultStreamerSettings())
			if len(added) != 1 {
				t.Fatalf("setup: added = %d, want 1", len(added))
			}
			s := mgr.Get("chan")
			tc.prep(s)

			cm.ToggleChat(s)
			if !cm.hasConnection("chan") {
				t.Fatalf("%s: current roster member must join with the predicate set", tc.name)
			}
			if got := atomic.LoadInt32(dials); got != 1 {
				t.Fatalf("%s: dials = %d, want 1", tc.name, got)
			}
		})
	}
}

// TestJoinChatSameLoginReAddRejectsOldAcceptsNew pins the identity semantics
// that a login-only membership check would get wrong: after "chan" is
// removed and re-added, the roster tracks a NEW *models.Streamer under the
// SAME login. The OLD pointer must be rejected even though its login still
// resolves to a (different) live roster member; the NEW pointer must join;
// and a stale toggle against the old pointer must not disturb the new
// pointer's already-live client.
func TestJoinChatSameLoginReAddRejectsOldAcceptsNew(t *testing.T) {
	mgr, cm, dials := newGatedManager()

	mgr.ApplySettings([]config.StreamerConfig{rosterEntry("chan", models.ChatAlways)}, models.DefaultStreamerSettings())
	ptr1 := mgr.Get("chan")
	cm.ToggleChat(ptr1)
	if !cm.hasConnection("chan") {
		t.Fatal("setup: ptr1 must join")
	}
	if got := atomic.LoadInt32(dials); got != 1 {
		t.Fatalf("dials = %d, want 1", got)
	}

	// Remove, then Leave (mirrors capability reconcile's removal sequence).
	mgr.ApplySettings(nil, models.DefaultStreamerSettings())
	cm.Leave("chan")

	// Re-add under the SAME login: a NEW streamer object.
	mgr.ApplySettings([]config.StreamerConfig{rosterEntry("chan", models.ChatAlways)}, models.DefaultStreamerSettings())
	ptr2 := mgr.Get("chan")
	if ptr2 == nil {
		t.Fatal("setup: re-add failed")
	}
	if ptr1 == ptr2 {
		t.Fatal("setup: re-add must produce a NEW streamer object, not reuse ptr1")
	}

	// The OLD pointer must be rejected: no join, no new dial.
	cm.ToggleChat(ptr1)
	if cm.hasConnection("chan") {
		t.Fatal("old pointer after same-login re-add must not join")
	}
	if got := atomic.LoadInt32(dials); got != 1 {
		t.Fatalf("dials = %d after rejected old-pointer toggle, want still 1", got)
	}

	// The NEW pointer joins normally.
	cm.ToggleChat(ptr2)
	if !cm.hasConnection("chan") {
		t.Fatal("new pointer after same-login re-add must join")
	}
	if got := atomic.LoadInt32(dials); got != 2 {
		t.Fatalf("dials = %d, want 2 (new pointer's own dial)", got)
	}
	live := cm.clientPtr("chan")

	// With ptr2's client live, a repeated ToggleChat(ptr1) must leave it
	// completely untouched: no dial, no client swap.
	cm.ToggleChat(ptr1)
	if got := atomic.LoadInt32(dials); got != 2 {
		t.Fatalf("dials = %d after repeated stale toggle with a live client, want still 2", got)
	}
	if got := cm.clientPtr("chan"); got != live {
		t.Fatal("ptr2's live client must remain the exact same object after a rejected stale toggle")
	}
}

// TestJoinChatRejectsForeignPointerWithMatchingLogin is the test that kills a
// login-only membership check: a *models.Streamer that shares "chan"'s login
// but was NEVER added to the manager (a distinct object entirely) must be
// rejected, and must not disturb the genuinely tracked streamer's live
// client.
func TestJoinChatRejectsForeignPointerWithMatchingLogin(t *testing.T) {
	mgr, cm, dials := newGatedManager()

	mgr.ApplySettings([]config.StreamerConfig{rosterEntry("chan", models.ChatAlways)}, models.DefaultStreamerSettings())
	tracked := mgr.Get("chan")
	cm.ToggleChat(tracked)
	if !cm.hasConnection("chan") {
		t.Fatal("setup: tracked streamer must join")
	}
	if got := atomic.LoadInt32(dials); got != 1 {
		t.Fatalf("dials = %d, want 1", got)
	}
	before := cm.clientPtr("chan")

	// A separate object, same login, never added to the manager.
	foreign := models.NewStreamer("chan", *rosterSettings(models.ChatAlways))
	if foreign == tracked {
		t.Fatal("setup: foreign must be a distinct pointer from the tracked object")
	}

	cm.ToggleChat(foreign)
	if got := atomic.LoadInt32(dials); got != 1 {
		t.Fatalf("dials = %d after foreign-pointer toggle, want still 1 (no join for a never-tracked pointer)", got)
	}
	if got := cm.clientPtr("chan"); got != before {
		t.Fatal("tracked streamer's live client must remain untouched by the foreign-pointer toggle")
	}
}

// TestJoinChatNilPredicatePreservesLegacyBehavior proves an unset predicate
// (the pre-M3, standalone/library-use default) permits every join exactly as
// before this change — this test's manager never calls SetRosterMembership.
func TestJoinChatNilPredicatePreservesLegacyBehavior(t *testing.T) {
	m, dials := newRuntimeChatManager()
	s := streamerWithChat("chan", models.ChatAlways)

	m.ToggleChat(s)
	if !m.hasConnection("chan") {
		t.Fatal("nil predicate must permit every join, as before M3")
	}
	if got := atomic.LoadInt32(dials); got != 1 {
		t.Fatalf("dials = %d, want 1", got)
	}
}

// TestJoinChatReconnectsStoppedClientForCurrentMember proves the predicate
// does not interfere with the existing reconnect path: a current roster
// member with a seeded, not-running client in the map still gets dialed and
// replaced by ToggleChat.
func TestJoinChatReconnectsStoppedClientForCurrentMember(t *testing.T) {
	mgr, cm, dials := newGatedManager()

	mgr.ApplySettings([]config.StreamerConfig{rosterEntry("chan", models.ChatAlways)}, models.DefaultStreamerSettings())
	s := mgr.Get("chan")

	// seedConnection (tristate_test.go) puts a client in the map that was
	// never Connect()'d, so IsRunning() is false — standing in for a
	// connection that died without leaveChat/Leave clearing the entry.
	cm.seedConnection(s)
	if got := atomic.LoadInt32(dials); got != 0 {
		t.Fatalf("seedConnection must not dial, got %d", got)
	}

	cm.ToggleChat(s)
	if !cm.hasConnection("chan") {
		t.Fatal("reconnect for a current roster member must join")
	}
	if got := atomic.LoadInt32(dials); got != 1 {
		t.Fatalf("dials = %d, want 1 (reconnect dialed exactly once)", got)
	}
}

// TestLeaveIdempotentWithRosterGateSet extends the exactly-once/idempotent
// removal coverage (runtime_toggle_test.go's
// TestToggleChatRuntimeRemovedStreamerAlwaysLeaves) to a manager with
// SetRosterMembership installed, since Leave's own switch logic is
// unchanged by M3 but is worth re-pinning under the new gated configuration.
func TestLeaveIdempotentWithRosterGateSet(t *testing.T) {
	mgr, cm, _ := newGatedManager()
	mgr.ApplySettings([]config.StreamerConfig{rosterEntry("chan", models.ChatAlways)}, models.DefaultStreamerSettings())
	s := mgr.Get("chan")
	cm.ToggleChat(s)
	if !cm.hasConnection("chan") {
		t.Fatal("setup: join failed")
	}

	cm.Leave("chan")
	if cm.hasConnection("chan") {
		t.Fatal("Leave must tear down the connection")
	}
	cm.Leave("chan") // repeated: no-op
	if cm.hasConnection("chan") {
		t.Fatal("repeated Leave must remain a no-op")
	}
	cm.Leave("never-joined") // no-op for an unknown login
}

// TestJoinChatRosterGateConcurrentHammering hammers ToggleChat against both a
// pointer that becomes stale mid-run and whatever the roster currently
// reports, racing concurrent roster remove/re-add + Leave cycles, all
// started together off one barrier channel (no sleeps anywhere). The
// -race detector is the primary assertion here; a final deterministic
// sequence, run only after every goroutine has joined, then pins the
// end-state invariants the predicate exists to guarantee regardless of how
// the storm interleaved.
func TestJoinChatRosterGateConcurrentHammering(t *testing.T) {
	mgr, cm, _ := newGatedManager()

	mgr.ApplySettings([]config.StreamerConfig{rosterEntry("chan", models.ChatAlways)}, models.DefaultStreamerSettings())
	ptr1 := mgr.Get("chan")

	const iterations = 200
	start := make(chan struct{})
	var wg sync.WaitGroup

	// g1: hammers ToggleChat with the ORIGINAL pointer, which g2 will make
	// stale partway through the run.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < iterations; i++ {
			cm.ToggleChat(ptr1)
		}
	}()

	// g2: cycles roster membership itself — remove, Leave, re-add — racing
	// g1's and g3's ToggleChat calls against the predicate.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < iterations; i++ {
			mgr.ApplySettings(nil, models.DefaultStreamerSettings())
			cm.Leave("chan")
			mgr.ApplySettings([]config.StreamerConfig{rosterEntry("chan", models.ChatAlways)}, models.DefaultStreamerSettings())
		}
	}()

	// g3: always toggles whatever the roster CURRENTLY reports for "chan"
	// (skipping the moments it observes nil, between a remove and the next
	// re-add) — the "current pointer" side of the race.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < iterations; i++ {
			if cur := mgr.Get("chan"); cur != nil {
				cm.ToggleChat(cur)
			}
		}
	}()

	close(start)
	wg.Wait()

	// Deterministic final sequence: force a known end state and verify the
	// invariants regardless of how the storm above interleaved.
	mgr.ApplySettings(nil, models.DefaultStreamerSettings())
	cm.Leave("chan")
	cm.ToggleChat(ptr1)
	if cm.hasConnection("chan") {
		t.Fatal("the removed-era pointer must never re-establish a connection after a final Leave")
	}

	mgr.ApplySettings([]config.StreamerConfig{rosterEntry("chan", models.ChatAlways)}, models.DefaultStreamerSettings())
	cur := mgr.Get("chan")
	cm.ToggleChat(cur)
	if !cm.hasConnection("chan") {
		t.Fatal("the current roster pointer must join after the final re-add")
	}
}
