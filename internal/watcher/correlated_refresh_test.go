package watcher

import (
	"errors"
	"strings"
	"testing"
)

// --- Group D: recovery signature & correlated refresh staging ---

// TestRecoverySignatureDistinctPerStage (D5): two signatures differing only in
// the failing stage are distinct, so a later stage never dedups against an
// earlier one.
func TestRecoverySignatureDistinctPerStage(t *testing.T) {
	base := RecoverySignature{Login: "chan", BroadcastID: "b1", SessionGeneration: 3, Mode: RefreshSession}
	a := base
	a.Stage = StagePlaylist
	b := base
	b.Stage = StageBeacon
	if a.String() == b.String() {
		t.Fatalf("distinct stages must yield distinct signatures, got %q", a.String())
	}
	// Identical inputs are stable/deterministic.
	a2 := base
	a2.Stage = StagePlaylist
	if a.String() != a2.String() {
		t.Fatal("signature must be deterministic for identical inputs")
	}
}

// TestRecoverySignatureRedacted (D6): a signature is built only from safe
// identity/classification fields; it never carries a URL, token, or payload.
func TestRecoverySignatureRedacted(t *testing.T) {
	sig := RecoverySignature{Login: "chan", BroadcastID: "b1", SessionGeneration: 3, Stage: StageBeacon, StatusClass: "4xx", ErrorCode: "beacon_http_403", Mode: RefreshSession}
	for _, secret := range []string{"http://", "https://", "token", "sig=", "spade"} {
		if strings.Contains(sig.String(), secret) {
			t.Fatalf("signature leaked %q: %q", secret, sig.String())
		}
	}
}

// TestRefreshCoalesceDedupBySignature (D1): two requests with the SAME signature
// collapse to a single staged refresh — a repeated identical failure never queues
// duplicate work.
func TestRefreshCoalesceDedupBySignature(t *testing.T) {
	w, _ := newTestWatcher(1)
	ref := &fakeRefresher{}
	w.refresher = ref
	login := w.streamers[0].Username

	// Same signature, unspecified expected session (the streamer has no broadcast/
	// generation yet), so the dedup — not the pre-I/O guard — is what's under test.
	sig := RecoverySignature{Login: login, BroadcastID: "b1", SessionGeneration: 1, Stage: StageBeacon, Mode: RefreshStreamInfo}.String()
	w.RequestSessionRefresh(SessionRefreshRequest{RequestID: "r1", Login: login, Mode: RefreshStreamInfo, Signature: sig})
	w.RequestSessionRefresh(SessionRefreshRequest{RequestID: "r2", Login: login, Mode: RefreshStreamInfo, Signature: sig})
	w.executeSessionRefreshes(occupantsFor(w, 0))

	if _, stream := ref.calls(); len(stream) != 1 {
		t.Fatalf("identical-signature requests must coalesce to one refresh, got %v", stream)
	}
	// The first request's identity wins (dedup keeps the staged one).
	if out, _ := w.LastSessionRefresh(login); out.RequestID != "r1" {
		t.Fatalf("dedup must keep the first staged request, got %q", out.RequestID)
	}
}

// TestRefreshNewerSessionSupersedes (D3): a request for a newer session (higher
// expected generation) replaces an older staged one.
func TestRefreshNewerSessionSupersedes(t *testing.T) {
	w, _ := newTestWatcher(1)
	ref := &fakeRefresher{}
	w.refresher = ref
	login := w.streamers[0].Username

	w.RequestSessionRefresh(SessionRefreshRequest{RequestID: "old", Login: login, Mode: RefreshStreamInfo, ExpectedGeneration: 1, Signature: "old"})
	w.RequestSessionRefresh(SessionRefreshRequest{RequestID: "new", Login: login, Mode: RefreshStreamInfo, ExpectedGeneration: 2, Signature: "new"})
	w.executeSessionRefreshes(occupantsFor(w, 0))

	out, _ := w.LastSessionRefresh(login)
	if out.RequestID != "new" || out.ExpectedSessionGeneration != 2 {
		t.Fatalf("a newer-session request must supersede the older one, got %+v", out)
	}
}

// TestRefreshBroadcastMismatchIsStale (Part 7 / B6): a request staged against a
// broadcast that has since changed is rejected as stale WITHOUT any I/O.
func TestRefreshBroadcastMismatchIsStale(t *testing.T) {
	w, _ := newTestWatcher(1)
	ref := &fakeRefresher{}
	w.refresher = ref
	login := w.streamers[0].Username
	w.streamers[0].Stream.Update("current-broadcast", "t", nil, nil, 1)

	w.RequestSessionRefresh(SessionRefreshRequest{
		RequestID: "r1", Login: login, Mode: RefreshSession,
		ExpectedBroadcastID: "old-broadcast", Signature: "s",
	})
	w.executeSessionRefreshes(occupantsFor(w, 0))

	if spade, stream := ref.calls(); len(spade) != 0 || len(stream) != 0 {
		t.Fatalf("a broadcast-mismatch refresh must do no I/O, got spade=%v stream=%v", spade, stream)
	}
	out, ok := w.LastSessionRefresh(login)
	if !ok || !out.Stale || out.Success || out.Reason != RefreshReasonBroadcastMoved {
		t.Fatalf("expected a stale broadcast-changed outcome, got %+v", out)
	}
}

// TestRefreshOutcomeRedacted (D6): even when the refresher's raw error embeds a
// URL/token, the published outcome (reason + detail + signature) carries none of
// it.
func TestRefreshOutcomeRedacted(t *testing.T) {
	w, _ := newTestWatcher(1)
	w.refresher = &fakeRefresher{spadeErr: errors.New("boom https://spade.evil/track?token=SECRET sig=abc")}
	login := w.streamers[0].Username

	w.RequestSessionRefresh(SessionRefreshRequest{RequestID: "r1", Login: login, Mode: RefreshSession, Signature: "s"})
	w.executeSessionRefreshes(occupantsFor(w, 0))

	out, _ := w.LastSessionRefresh(login)
	blob := out.Reason + " " + out.Detail + " " + out.Signature
	for _, secret := range []string{"https://", "token=", "SECRET", "sig=", "spade.evil"} {
		if strings.Contains(blob, secret) {
			t.Fatalf("refresh outcome leaked %q: %q", secret, blob)
		}
	}
	if out.Success {
		t.Fatal("a spade failure must not report success")
	}
}
