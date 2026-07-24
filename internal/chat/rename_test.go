package chat

import (
	"sync/atomic"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestChatManager_Rename_LeaveOldJoinNew is the chat half of BKM-006
// invariant K: the miner's reconciliation for a config-driven rename leaves
// the OLD login's IRC channel exactly once and joins the NEW login exactly
// once — never a lingering connection under a login nothing will ever
// address again, never a duplicate join.
func TestChatManager_Rename_LeaveOldJoinNew(t *testing.T) {
	m, dials := newRuntimeChatManager()
	s := streamerWithChat("oldlogin", models.ChatAlways)

	m.ToggleChat(s)
	if !m.hasConnection("oldlogin") {
		t.Fatal("setup: ALWAYS must join the old login")
	}
	if got := atomic.LoadInt32(dials); got != 1 {
		t.Fatalf("setup dials = %d, want 1", got)
	}

	// The streamer.Manager reconciles the rename in place (same pointer,
	// login updated); the miner then does exactly what
	// reconcileRuntimeCapabilities does: Leave(oldLogin) once, then
	// ToggleChat(streamer) to reconcile the (now-renamed) roster member.
	obs := s.BeginLoginObservation()
	if !s.RenameIfCurrent("newlogin", obs) {
		t.Fatal("rename setup failed")
	}
	m.Leave("oldlogin")
	m.ToggleChat(s)

	if m.hasConnection("oldlogin") {
		t.Fatal("the old login's IRC connection must be gone after the rename")
	}
	if !m.hasConnection("newlogin") {
		t.Fatal("the new login must be joined after the rename")
	}
	if got := atomic.LoadInt32(dials); got != 2 {
		t.Fatalf("dials after rename = %d, want 2 (one per login, no duplicate)", got)
	}

	// A repeated reconcile with NO further rename (the miner only calls Leave
	// for logins it actually renamed this apply) must be a no-op: no new
	// dial, connection stays exactly where it is.
	m.ToggleChat(s)
	if got := atomic.LoadInt32(dials); got != 2 {
		t.Fatalf("repeated identical reconcile re-dialed: dials = %d, want 2", got)
	}
	if !m.hasConnection("newlogin") {
		t.Fatal("repeated reconcile must keep the new login joined")
	}

	// Even an extra, redundant Leave("oldlogin") call (idempotent by
	// contract) must not disturb the established newlogin connection.
	m.Leave("oldlogin")
	if !m.hasConnection("newlogin") {
		t.Fatal("a redundant Leave(oldLogin) must not affect the new login's connection")
	}
}

// TestChatManager_Rename_DisabledChatNeverJoins covers the other half of K:
// when the streamer's Chat setting is NEVER, a rename must not cause the new
// login to be joined — Leave(oldLogin) is harmless (nothing was joined) and
// ToggleChat correctly does nothing for NEVER.
func TestChatManager_Rename_DisabledChatNeverJoins(t *testing.T) {
	m, dials := newRuntimeChatManager()
	s := streamerWithChat("oldlogin", models.ChatNever)

	m.ToggleChat(s)
	if m.hasConnection("oldlogin") {
		t.Fatal("setup: NEVER must not join")
	}

	obs := s.BeginLoginObservation()
	if !s.RenameIfCurrent("newlogin", obs) {
		t.Fatal("rename setup failed")
	}
	m.Leave("oldlogin")
	m.ToggleChat(s)

	if m.hasConnection("oldlogin") || m.hasConnection("newlogin") {
		t.Fatal("NEVER must not join either login after a rename")
	}
	if got := atomic.LoadInt32(dials); got != 0 {
		t.Fatalf("dials = %d, want 0 (chat disabled)", got)
	}
}
