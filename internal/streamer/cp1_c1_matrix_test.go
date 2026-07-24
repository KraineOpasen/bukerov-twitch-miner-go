package streamer

import (
	"fmt"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestLoadFromConfig_StoredChannelIDMismatch_TransientFailure_ColdRestart_C1B
// covers BKM-006 Corrective Pass 1 test matrix item C1-B: a config entry
// carries a non-empty, persisted ChannelID, but its login cannot be resolved
// this cycle (ErrStreamerDoesNotExist — the classic "typo'd/removed login"
// shape) on a COLD restart (byID/byLogin both empty). The stored identity
// must not be silently dropped, fabricated, or deleted: nothing is created
// for this entry, and a later successful resolution recovers normally.
func TestLoadFromConfig_StoredChannelIDMismatch_TransientFailure_ColdRestart_C1B(t *testing.T) {
	client := newIDFakeClient()
	client.failOn["oldlogin"] = fmt.Errorf("%w: operation GetIDFromLogin", api.ErrStreamerDoesNotExist)
	m := NewManager(client, models.DefaultStreamerSettings())

	err := m.LoadFromConfig([]config.StreamerConfig{
		{Username: "oldlogin", ChannelID: "123"},
	}, nil)
	if err == nil {
		t.Fatal("LoadFromConfig with zero resolvable streamers should report an error")
	}

	if s := m.GetByChannelID("123"); s != nil {
		t.Fatalf("stored ChannelID 123 must not be fabricated into a tracked streamer: %+v", s)
	}
	if s := m.Get("oldlogin"); s != nil {
		t.Fatalf("unresolved login must not be tracked: %+v", s)
	}
	if m.Count() != 0 {
		t.Fatalf("count = %d, want 0 (fail-closed: nothing fabricated)", m.Count())
	}

	// A later successful resolution recovers normally (first-bind path: the
	// stored id now matches what Twitch reports).
	client.ids["oldlogin"] = "123"
	delete(client.failOn, "oldlogin")
	added, _, _, _, conflicts := m.reconcile(
		[]config.StreamerConfig{{Username: "oldlogin", ChannelID: "123"}}, models.DefaultStreamerSettings(), nil)
	if len(conflicts) != 0 {
		t.Fatalf("recovery apply reported unexpected conflicts: %+v", conflicts)
	}
	if len(added) != 1 || m.Get("oldlogin") == nil {
		t.Fatalf("recovery apply did not bind the streamer once the login resolved to the stored id: added=%d", len(added))
	}
}

// TestApplySettings_StoredChannelIDMismatch_TransientFailure_Warm_C1B covers
// the IN-PROCESS counterpart of C1-B: a streamer is already tracked (under
// its persisted stored id), and a LATER apply's resolution for its login
// fails transiently. The already-tracked streamer must survive untouched —
// neither deleted (it would otherwise look "unresolved this cycle" and fall
// out of the survivor set) nor renamed/overwritten.
func TestApplySettings_StoredChannelIDMismatch_TransientFailure_Warm_C1B(t *testing.T) {
	client := newIDFakeClient()
	client.ids["steady"] = "id-steady"
	m := NewManager(client, models.DefaultStreamerSettings())

	added, _, _, _ := m.ApplySettings(
		[]config.StreamerConfig{{Username: "steady", ChannelID: "id-steady"}}, models.DefaultStreamerSettings())
	if len(added) != 1 {
		t.Fatalf("seed: added=%d, want 1", len(added))
	}
	orig := m.Get("steady")

	// Twitch becomes transiently unreachable for this login (network
	// hiccup) — NOT a genuine rename to a different channel.
	client.failOn["steady"] = fmt.Errorf("%w: operation GetIDFromLogin", api.ErrPersistedQueryNotFound)
	_, removed, changed, renamed, conflicts := m.reconcile(
		[]config.StreamerConfig{{Username: "steady", ChannelID: "id-steady"}}, models.DefaultStreamerSettings(), nil)

	if len(removed) != 0 || len(changed) != 0 || len(renamed) != 0 || len(conflicts) != 0 {
		t.Fatalf("transient failure with a stored id must be a total no-op: removed=%d changed=%d renamed=%d conflicts=%d",
			len(removed), len(changed), len(renamed), len(conflicts))
	}
	if got := m.Get("steady"); got != orig {
		t.Fatal("the already-tracked streamer must survive a transient resolution failure untouched")
	}
	if m.GetByChannelID("id-steady") != orig {
		t.Fatal("stored identity must remain resolvable by ChannelID")
	}
}

// TestApplySettings_EmptyStoredChannelID_FirstBindThenIdempotent_C1D covers
// BKM-006 Corrective Pass 1 test matrix item C1-D: an EMPTY persisted
// ChannelID is the first-bind path — no expected-identity protection yet, so
// the entry binds normally to whatever Twitch resolves — and repeating the
// SAME apply (still empty, as an un-backfilled config would be) is a
// idempotent no-op. A THIRD apply that now carries the backfilled id (as a
// real config.json would after the first successful bind) is likewise a
// no-op, proving the expected-id path and the first-bind path converge to
// the same steady state.
func TestApplySettings_EmptyStoredChannelID_FirstBindThenIdempotent_C1D(t *testing.T) {
	client := newIDFakeClient()
	client.ids["fresh"] = "id-fresh"
	m := NewManager(client, models.DefaultStreamerSettings())

	added, removed, changed, renamed := m.ApplySettings(
		[]config.StreamerConfig{{Username: "fresh", ChannelID: ""}}, models.DefaultStreamerSettings())
	if len(added) != 1 || len(removed) != 0 || len(changed) != 0 || len(renamed) != 0 {
		t.Fatalf("first bind: added=%d removed=%d changed=%d renamed=%d, want 1/0/0/0",
			len(added), len(removed), len(changed), len(renamed))
	}
	first := m.Get("fresh")
	if first == nil || first.ChannelID != "id-fresh" {
		t.Fatalf("first bind did not resolve the streamer: %+v", first)
	}

	// Repeat with the SAME (still-empty) stored id: idempotent no-op.
	added, removed, changed, renamed = m.ApplySettings(
		[]config.StreamerConfig{{Username: "fresh", ChannelID: ""}}, models.DefaultStreamerSettings())
	if len(added) != 0 || len(removed) != 0 || len(changed) != 0 || len(renamed) != 0 {
		t.Fatalf("repeat with empty stored id: added=%d removed=%d changed=%d renamed=%d, want all 0",
			len(added), len(removed), len(changed), len(renamed))
	}
	if m.Get("fresh") != first {
		t.Fatal("repeat apply must retain the SAME streamer object")
	}

	// A THIRD apply now carrying the backfilled (matching) stored id is also
	// a no-op — the expected-id path and the first-bind path converge.
	added, removed, changed, renamed = m.ApplySettings(
		[]config.StreamerConfig{{Username: "fresh", ChannelID: "id-fresh"}}, models.DefaultStreamerSettings())
	if len(added) != 0 || len(removed) != 0 || len(changed) != 0 || len(renamed) != 0 {
		t.Fatalf("repeat with matching stored id: added=%d removed=%d changed=%d renamed=%d, want all 0",
			len(added), len(removed), len(changed), len(renamed))
	}
	if m.Get("fresh") != first {
		t.Fatal("apply with the now-matching stored id must retain the SAME streamer object")
	}
}
