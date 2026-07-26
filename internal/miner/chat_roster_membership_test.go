package miner

import (
	"context"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

// TestChatRosterMembershipSemantics pins chatRosterMembership's production
// contract (M3): it is a POINTER IDENTITY check against the live roster's
// CURRENT object for a streamer's login, never a login-equality check.
func TestChatRosterMembershipSemantics(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	_ = m.ApplySettings(context.Background(), m.GetRuntimeSettings())

	added := m.streamers.Get("alpha")
	if added == nil {
		t.Fatal("setup: alpha not tracked")
	}
	if !m.chatRosterMembership(added) {
		t.Fatal("(a) a currently-tracked member must report true")
	}

	// Remove it entirely.
	rs := m.GetRuntimeSettings()
	rs.Streamers = nil
	if err := m.ApplySettings(context.Background(), rs); err != nil {
		t.Fatalf("ApplySettings (remove): %v", err)
	}
	if m.streamers.Get("alpha") != nil {
		t.Fatal("setup: alpha still tracked after removal")
	}
	if m.chatRosterMembership(added) {
		t.Fatal("(b) a stale pointer surviving removal must report false")
	}

	// Re-add under the SAME login: the manager creates a NEW object.
	rs2 := m.GetRuntimeSettings()
	rs2.Streamers = []settings.StreamerConfig{{Username: "alpha"}}
	if err := m.ApplySettings(context.Background(), rs2); err != nil {
		t.Fatalf("ApplySettings (re-add): %v", err)
	}
	reAdded := m.streamers.Get("alpha")
	if reAdded == nil {
		t.Fatal("setup: re-add failed")
	}
	if reAdded == added {
		t.Fatal("setup: re-add must produce a NEW streamer object, not reuse the old pointer")
	}
	if m.chatRosterMembership(added) {
		t.Fatal("(c) the OLD pointer after a same-login re-add must report false")
	}
	if !m.chatRosterMembership(reAdded) {
		t.Fatal("(c) the NEW pointer after a same-login re-add must report true")
	}

	// A foreign pointer that shares a tracked member's login but was never
	// added to the manager at all.
	foreign := models.NewStreamer("alpha", models.DefaultStreamerSettings())
	if m.chatRosterMembership(foreign) {
		t.Fatal("(d) a never-tracked pointer sharing a member's login must report false")
	}

	// A struct-literal Miner (as built directly in many package tests, which
	// never run New()) has a nil m.streamers; the predicate must degrade to
	// false rather than panic.
	var bare Miner
	other := models.NewStreamer("someone", models.DefaultStreamerSettings())
	if bare.chatRosterMembership(other) {
		t.Fatal("(e) a nil m.streamers must report false, not panic")
	}
}

// TestApplySettingsSweepSkipsRemovedStaleStreamer covers the whole-roster
// sweep ApplySettings drives (reconcileRuntimeCapabilities ->
// buildCapabilityPlan -> executeCapabilityPlan), the same shape
// checkAllStreamers/checkUncheckedStreamers run in production: it must
// toggle every CURRENT roster member and never the just-removed one, Leave
// the removed streamer's login exactly once, and stay idempotent (no second
// Leave) on a repeated identical apply. It also cross-checks the production
// predicate directly: chatRosterMembership rejects the stale pointer while
// accepting the surviving current member.
func TestApplySettingsSweepSkipsRemovedStaleStreamer(t *testing.T) {
	m, _, chatRec := newCapabilityMiner(t, "alpha", "bravo")
	_ = m.ApplySettings(context.Background(), m.GetRuntimeSettings()) // seed

	stale := m.streamers.Get("bravo")
	if stale == nil {
		t.Fatal("setup: bravo not tracked")
	}
	// bravo was toggled by the seed apply above while it was still a member;
	// what matters is that the REMOVAL sweep below adds no further toggle.
	bravoTogglesBeforeRemoval := chatRec.toggleCount("bravo")

	// Remove bravo, keep alpha.
	rs := m.GetRuntimeSettings()
	var keep []settings.StreamerConfig
	for _, sc := range rs.Streamers {
		if sc.Username != "bravo" {
			keep = append(keep, sc)
		}
	}
	rs.Streamers = keep
	if err := m.ApplySettings(context.Background(), rs); err != nil {
		t.Fatalf("ApplySettings (remove bravo): %v", err)
	}

	if chatRec.toggleCount("alpha") == 0 {
		t.Fatal("the sweep must toggle the current roster member")
	}
	if got := chatRec.toggleCount("bravo"); got != bravoTogglesBeforeRemoval {
		t.Fatalf("the removal sweep must never toggle the just-removed streamer: toggles went from %d to %d",
			bravoTogglesBeforeRemoval, got)
	}
	if got := chatRec.leaveCount("bravo"); got != 1 {
		t.Fatalf("leaveCount(bravo) = %d, want exactly 1", got)
	}

	// Repeat the identical apply (bravo already gone, nothing new to
	// remove): no second Leave.
	if err := m.ApplySettings(context.Background(), rs); err != nil {
		t.Fatalf("ApplySettings (repeat): %v", err)
	}
	if got := chatRec.leaveCount("bravo"); got != 1 {
		t.Fatalf("leaveCount(bravo) after repeated apply = %d, want still 1 (idempotent)", got)
	}

	// The production predicate: reject the stale pointer, accept the current
	// member.
	if m.chatRosterMembership(stale) {
		t.Fatal("chatRosterMembership must reject the removed stale pointer")
	}
	current := m.streamers.Get("alpha")
	if current == nil {
		t.Fatal("setup: alpha missing after the apply")
	}
	if !m.chatRosterMembership(current) {
		t.Fatal("chatRosterMembership must accept the surviving current member")
	}
}
