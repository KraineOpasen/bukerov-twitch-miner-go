package streamer

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// idFakeClient is a twitchClient fake for the ID-first reconciliation matrix:
// GetChannelID resolves from an explicit login->id map (falling back to
// "chan-"+login, matching the package's existing fakeChannelAPI/fakeStreamerAPI
// convention), and can be told to fail for specific logins to simulate a
// transient/PQNF outage (O).
type idFakeClient struct {
	ids    map[string]string
	failOn map[string]error
}

func newIDFakeClient() *idFakeClient {
	return &idFakeClient{ids: map[string]string{}, failOn: map[string]error{}}
}

func (f *idFakeClient) GetChannelID(username string) (string, error) {
	if err, ok := f.failOn[username]; ok {
		return "", err
	}
	if id, ok := f.ids[username]; ok {
		return id, nil
	}
	return "chan-" + username, nil
}
func (*idFakeClient) LoadChannelPointsContext(*models.Streamer) error { return nil }
func (*idFakeClient) CheckStreamerOnline(*models.Streamer) models.StatusTransition {
	return models.StatusTransition{}
}

// TestApplySettings_LoginReuseByDifferentID_StoredIDWins covers invariant C:
// a login that already identifies a tracked streamer is later resolved by
// Twitch to a DIFFERENT ChannelID (the login was reused by someone else).
// The already-tracked streamer's stored ChannelID must win: no rename, no
// deletion, no new streamer created under the reused login.
func TestApplySettings_LoginReuseByDifferentID_StoredIDWins(t *testing.T) {
	client := newIDFakeClient()
	client.ids["bob"] = "id-bob-original"
	m := NewManager(client, models.DefaultStreamerSettings())

	added, _, _, _ := m.ApplySettings(configsFor("bob"), models.DefaultStreamerSettings())
	if len(added) != 1 {
		t.Fatalf("seed: added=%d, want 1", len(added))
	}
	original := m.Get("bob")
	if original == nil || original.ChannelID != "id-bob-original" {
		t.Fatalf("seed streamer wrong: %+v", original)
	}

	// The login "bob" now resolves to a totally different channel.
	client.ids["bob"] = "id-bob-new-owner"
	added, removed, changed, renamed := m.ApplySettings(configsFor("bob"), models.DefaultStreamerSettings())

	if len(added) != 0 || len(removed) != 0 || len(changed) != 0 || len(renamed) != 0 {
		t.Fatalf("login reuse must mutate nothing: added=%d removed=%d changed=%d renamed=%d",
			len(added), len(removed), len(changed), len(renamed))
	}
	if got := m.Get("bob"); got != original || got.ChannelID != "id-bob-original" {
		t.Fatalf("stored ID must win: got %+v, want the original streamer unchanged", got)
	}
	if m.Count() != 1 {
		t.Fatalf("count = %d, want 1 (no phantom second streamer)", m.Count())
	}
	if s := m.GetByChannelID("id-bob-new-owner"); s != nil {
		t.Fatalf("the new owner's channel must not be tracked from a collided login: %+v", s)
	}
}

// TestApplySettings_DuplicateConfigIdenticalSettings_Coalesces covers
// invariant D: two config entries resolve to the SAME ChannelID with
// IDENTICAL effective settings — they must coalesce into exactly ONE runtime
// streamer (never two), with a single canonical login.
func TestApplySettings_DuplicateConfigIdenticalSettings_Coalesces(t *testing.T) {
	client := newIDFakeClient()
	client.ids["oldhandle"] = "id-dup-1"
	client.ids["newhandle"] = "id-dup-1"
	m := NewManager(client, models.DefaultStreamerSettings())

	added, _, _, _ := m.ApplySettings(
		[]config.StreamerConfig{{Username: "oldhandle"}, {Username: "newhandle"}},
		models.DefaultStreamerSettings())

	if len(added) != 1 {
		t.Fatalf("duplicate identical-settings entries must add exactly ONE streamer, got %d", len(added))
	}
	if m.Count() != 1 {
		t.Fatalf("count = %d, want 1 (coalesced)", m.Count())
	}
	// Canonical login is deterministic: the last-listed entry.
	if got := m.Get("newhandle"); got == nil || got.ChannelID != "id-dup-1" {
		t.Fatalf("canonical login lookup failed: %+v", got)
	}
	if m.Get("oldhandle") != nil {
		t.Fatal("the non-canonical duplicate login must not ALSO resolve to a streamer")
	}
}

// TestApplySettings_DuplicateConfigConflictingSettings_TypedConflict covers
// invariant E: two config entries resolve to the SAME ChannelID with
// DIFFERING effective settings. This is an ambiguous, unresolved conflict —
// nothing is added, nothing is merged, no partial mutation of any kind.
func TestApplySettings_DuplicateConfigConflictingSettings_TypedConflict(t *testing.T) {
	client := newIDFakeClient()
	client.ids["confa"] = "id-conflict-1"
	client.ids["confb"] = "id-conflict-1"
	m := NewManager(client, models.DefaultStreamerSettings())

	override := models.DefaultStreamerSettings()
	override.FollowRaid = false

	configs := []config.StreamerConfig{
		{Username: "confa"},
		{Username: "confb", Settings: &override},
	}
	added, removed, changed, renamed, conflicts := m.reconcile(configs, models.DefaultStreamerSettings(), nil)

	if len(added) != 0 || len(removed) != 0 || len(changed) != 0 || len(renamed) != 0 {
		t.Fatalf("conflicting duplicate entries must mutate nothing: added=%d removed=%d changed=%d renamed=%d",
			len(added), len(removed), len(changed), len(renamed))
	}
	if len(conflicts) != 1 || conflicts[0].Kind != ConflictDuplicateSettings {
		t.Fatalf("conflicts = %+v, want exactly one ConflictDuplicateSettings", conflicts)
	}
	if conflicts[0].ChannelID != "id-conflict-1" {
		t.Errorf("conflict channelID = %q, want id-conflict-1", conflicts[0].ChannelID)
	}
	if m.Count() != 0 {
		t.Fatalf("count = %d, want 0 (nothing applied on conflict)", m.Count())
	}

	// Privacy-safe: the conflict error text carries only logins + ChannelID,
	// never anything that looks like a credential or URL.
	msg := strings.ToLower(conflicts[0].Error())
	for _, forbidden := range []string{"http", "token", "bearer", "oauth"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("conflict message leaked a sensitive-looking token: %q", msg)
		}
	}
}

// TestApplySettings_CanonicalLoginCollisionDifferentID_FailsClosed covers
// invariant F: an ALREADY-TRACKED streamer's canonical (Twitch-resolved)
// login collides with a login already bound to a DIFFERENT tracked
// ChannelID. Neither streamer is mutated, deleted, or merged.
func TestApplySettings_CanonicalLoginCollisionDifferentID_FailsClosed(t *testing.T) {
	client := newIDFakeClient()
	client.ids["alice"] = "id-alice"
	client.ids["bob"] = "id-bob"
	m := NewManager(client, models.DefaultStreamerSettings())

	added, _, _, _ := m.ApplySettings(configsFor("alice", "bob"), models.DefaultStreamerSettings())
	if len(added) != 2 {
		t.Fatalf("seed: added=%d, want 2", len(added))
	}
	aliceOrig := m.Get("alice")
	bobOrig := m.Get("bob")

	// Twitch now claims "alice" resolves to bob's channel — i.e. the config
	// entry that used to mean id-bob is now (incorrectly, or due to an
	// operator typo) submitted under the login "alice".
	client.ids["alice"] = "id-bob"
	added, removed, changed, renamed := m.ApplySettings(
		[]config.StreamerConfig{{Username: "alice"}}, models.DefaultStreamerSettings())

	if len(added) != 0 || len(removed) != 0 || len(changed) != 0 || len(renamed) != 0 {
		t.Fatalf("collision must mutate nothing: added=%d removed=%d changed=%d renamed=%d",
			len(added), len(removed), len(changed), len(renamed))
	}
	if got := m.Get("alice"); got != aliceOrig || got.ChannelID != "id-alice" {
		t.Fatalf("alice's identity must be retained untouched: %+v", got)
	}
	if got := m.Get("bob"); got != bobOrig || got.ChannelID != "id-bob" {
		t.Fatalf("bob's identity must be retained untouched: %+v", got)
	}
	if m.Count() != 2 {
		t.Fatalf("count = %d, want 2 (no deletion, no overwrite)", m.Count())
	}
}

// TestReconcile_TransientResolutionFailure_NoRenameNoDelete covers invariant
// O: a transient/PersistedQueryNotFound resolution failure for an existing
// streamer's login must change NOTHING — no rename, no removal, no settings
// mutation, no conflict recorded (it's not ambiguous, just unresolved).
func TestReconcile_TransientResolutionFailure_NoRenameNoDelete(t *testing.T) {
	client := newIDFakeClient()
	client.ids["steady"] = "id-steady"
	m := NewManager(client, models.DefaultStreamerSettings())

	custom := models.DefaultStreamerSettings()
	custom.FollowRaid = false
	added, _, _, _ := m.ApplySettings(
		[]config.StreamerConfig{{Username: "steady", Settings: &custom}}, models.DefaultStreamerSettings())
	if len(added) != 1 {
		t.Fatalf("seed: added=%d, want 1", len(added))
	}
	orig := m.Get("steady")

	client.failOn["steady"] = fmt.Errorf("%w: operation GetIDFromLogin", api.ErrPersistedQueryNotFound)
	added, removed, changed, renamed, conflicts := m.reconcile(
		[]config.StreamerConfig{{Username: "steady", Settings: &custom}}, models.DefaultStreamerSettings(), nil)

	if len(added) != 0 || len(removed) != 0 || len(changed) != 0 || len(renamed) != 0 || len(conflicts) != 0 {
		t.Fatalf("transient failure must be a total no-op: added=%d removed=%d changed=%d renamed=%d conflicts=%d",
			len(added), len(removed), len(changed), len(renamed), len(conflicts))
	}
	if got := m.Get("steady"); got != orig {
		t.Fatal("transient failure must keep the SAME streamer object")
	}
	if orig.GetSettings().FollowRaid {
		t.Fatal("settings must be unchanged by a transient resolution failure (FollowRaid must stay false)")
	}

	// A later successful resolution recovers normally.
	delete(client.failOn, "steady")
	if _, _, _, _ = m.ApplySettings(configsFor("steady"), models.DefaultStreamerSettings()); m.Get("steady") != orig {
		t.Fatal("recovery apply must still be the same object")
	}
}

// TestApplySettings_RenameConcurrentWithReaders_NoDupNoDeadlock covers
// invariant N: concurrent ApplySettings calls (some of which rename the same
// streamer back and forth) racing with Get/All/GetSettings/GetUsername must
// never deadlock, never race (run with -race), and must never leave two
// runtime objects for the same ChannelID.
func TestApplySettings_RenameConcurrentWithReaders_NoDupNoDeadlock(t *testing.T) {
	client := newIDFakeClient()
	client.ids["login-a"] = "id-storm"
	client.ids["login-b"] = "id-storm"
	m := NewManager(client, models.DefaultStreamerSettings())

	if added, _, _, _ := m.ApplySettings(configsFor("login-a"), models.DefaultStreamerSettings()); len(added) != 1 {
		t.Fatalf("seed failed: added=%d", len(added))
	}

	cfgA := configsFor("login-a")
	cfgB := configsFor("login-b")

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				if (i+j)%2 == 0 {
					m.ApplySettings(cfgA, models.DefaultStreamerSettings())
				} else {
					m.ApplySettings(cfgB, models.DefaultStreamerSettings())
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				for _, s := range m.All() {
					_ = s.GetSettings()
					_ = s.GetUsername()
				}
				_ = m.Get("login-a")
				_ = m.Get("login-b")
				_ = m.GetByChannelID("id-storm")
			}
		}()
	}
	wg.Wait()

	if m.Count() != 1 {
		t.Fatalf("count after concurrent renames = %d, want 1 (no duplicate pointer for one ChannelID)", m.Count())
	}
	final := m.GetByChannelID("id-storm")
	if final == nil {
		t.Fatal("channel id-storm must still resolve to a streamer")
	}
	login := final.GetUsername()
	if login != "login-a" && login != "login-b" {
		t.Fatalf("final login = %q, want login-a or login-b", login)
	}
	if m.Get(login) != final {
		t.Fatal("byLogin index inconsistent with the final streamer")
	}
}

// TestRenameIfCurrent_StaleObservationDiscarded is the model-level unit test
// for invariant I12: a rename decision computed from an OLDER observation
// generation must be discarded (not applied) once a newer observation has
// begun, so a slow/stale reconciliation can never roll back a fresher login.
func TestRenameIfCurrent_StaleObservationDiscarded(t *testing.T) {
	s := models.NewStreamer("original", models.DefaultStreamerSettings())

	staleObs := s.BeginLoginObservation() // generation 1, captured "early"
	freshObs := s.BeginLoginObservation() // generation 2, a "newer" reconcile

	if !s.RenameIfCurrent("fresh-login", freshObs) {
		t.Fatal("the newer observation's rename must be applied")
	}
	if got := s.GetUsername(); got != "fresh-login" {
		t.Fatalf("username = %q, want fresh-login", got)
	}

	if s.RenameIfCurrent("stale-login", staleObs) {
		t.Fatal("a stale observation must NOT be able to roll back a newer rename")
	}
	if got := s.GetUsername(); got != "fresh-login" {
		t.Fatalf("username after a rejected stale rename = %q, want fresh-login (unchanged)", got)
	}
}
