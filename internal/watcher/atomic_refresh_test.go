package watcher

import (
	"fmt"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// --- Corrective pass Groups C/D/E/F: broker-side correlated refresh ---

// setSession gives a streamer a known broadcast (and thus a non-zero session
// generation) for the expected-session guards. After one Update the generation is
// 1.
func setSession(w *MinuteWatcher, idx int, broadcast string) {
	w.streamers[idx].Stream.Update(broadcast, "t", nil, nil, 0)
}

// TestBrokerExpectedGuardRejectsWithoutIO (Group D + E): a request whose concrete
// expected broadcast or generation no longer matches the live session is rejected
// as stale BEFORE any refresher I/O — no network, no session mutation, a typed
// stale outcome with a bounded redacted reason.
func TestBrokerExpectedGuardRejectsWithoutIO(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(w *MinuteWatcher)
		req        SessionRefreshRequest
		wantReason string
	}{
		{
			name:       "generation drift (expected b1/g-old, current b1/g-new)",
			setup:      func(w *MinuteWatcher) { setSession(w, 0, "b1") }, // gen 1
			req:        SessionRefreshRequest{RequestID: "r", Mode: RefreshSession, ExpectedBroadcastID: "b1", ExpectedGeneration: 99},
			wantReason: RefreshReasonGenerationMoved,
		},
		{
			name:       "concrete expected broadcast vs empty current",
			setup:      func(w *MinuteWatcher) {}, // no broadcast: current empty
			req:        SessionRefreshRequest{RequestID: "r", Mode: RefreshSession, ExpectedBroadcastID: "b1"},
			wantReason: RefreshReasonBroadcastMoved,
		},
		{
			name:       "new broadcast landed (expected b1, current b2)",
			setup:      func(w *MinuteWatcher) { setSession(w, 0, "b2") },
			req:        SessionRefreshRequest{RequestID: "r", Mode: RefreshSession, ExpectedBroadcastID: "b1"},
			wantReason: RefreshReasonBroadcastMoved,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := newTestWatcher(1)
			ref := &fakeRefresher{}
			w.refresher = ref
			tc.setup(w)
			tc.req.Login = w.streamers[0].Username

			w.RequestSessionRefresh(tc.req)
			w.executeSessionRefreshes(occupantsFor(w, 0))

			if spade, stream := ref.calls(); len(spade) != 0 || len(stream) != 0 {
				t.Fatalf("a stale-guarded request must do ZERO refresher I/O, got spade=%v stream=%v", spade, stream)
			}
			out, ok := w.LastSessionRefresh(w.streamers[0].Username)
			if !ok || !out.Stale || out.Success || out.Reason != tc.wantReason {
				t.Fatalf("expected a stale outcome (%s), got %+v", tc.wantReason, out)
			}
			for _, secret := range []string{"http://", "https://", "token", "sig="} {
				if strings.Contains(out.Detail+out.Reason, secret) {
					t.Fatalf("stale outcome leaked %q: %+v", secret, out)
				}
			}
		})
	}
}

// TestBrokerExpectedMatchRuns (Group E): when the expected broadcast and
// generation still match, the refresh runs and publishes, reporting success and
// the applied generation.
func TestBrokerExpectedMatchRuns(t *testing.T) {
	w, _ := newTestWatcher(1)
	ref := &fakeRefresher{}
	w.refresher = ref
	setSession(w, 0, "b1") // gen 1
	login := w.streamers[0].Username

	w.RequestSessionRefresh(SessionRefreshRequest{
		RequestID: "r", Login: login, Mode: RefreshSession,
		ExpectedBroadcastID: "b1", ExpectedGeneration: 1,
	})
	w.executeSessionRefreshes(occupantsFor(w, 0))

	if _, stream := ref.calls(); len(stream) != 1 {
		t.Fatalf("a matching request must run exactly once, got %v", stream)
	}
	out, _ := w.LastSessionRefresh(login)
	if !out.Success || out.Stale || out.AppliedSessionGeneration == 0 {
		t.Fatalf("expected a successful applied outcome, got %+v", out)
	}
}

// TestBrokerGenerationDriftDuringIOIsStale (Group C + E): the pre-I/O guard
// passes, but the session generation drifts DURING the refresh I/O; the atomic
// apply's expected-generation re-check then rejects it. The network ran, yet the
// outcome is stale (not a nil-success) and nothing was published.
func TestBrokerGenerationDriftDuringIOIsStale(t *testing.T) {
	w, _ := newTestWatcher(1)
	setSession(w, 0, "b1") // gen 1
	login := w.streamers[0].Username

	ref := &fakeRefresher{beforeApply: func(s *models.Streamer) {
		// A concurrent change bumps the generation while the refresh is mid-flight.
		s.Stream.SetSpadeURL("http://spade.test/drifted")
	}}
	w.refresher = ref

	w.RequestSessionRefresh(SessionRefreshRequest{
		RequestID: "r", Login: login, Mode: RefreshSession,
		ExpectedBroadcastID: "b1", ExpectedGeneration: 1,
	})
	w.executeSessionRefreshes(occupantsFor(w, 0))

	// The network ran (pre-I/O guard passed)...
	if _, stream := ref.calls(); len(stream) != 1 {
		t.Fatalf("the refresh must have run its I/O, got %v", stream)
	}
	// ...but the apply was rejected as stale, and the refreshed spade URL was NOT
	// published over the drifted session.
	out, _ := w.LastSessionRefresh(login)
	if out.Success || !out.Stale {
		t.Fatalf("a mid-I/O generation drift must yield a stale (not success) outcome, got %+v", out)
	}
	if got := w.streamers[0].Stream.GetSpadeURL(); got == "http://spade.test/refreshed" {
		t.Fatalf("a stale refresh must not publish the tuple, but spade URL was overwritten: %q", got)
	}
}

// TestBrokerOutcomeDistinguishesGenerations (CP2 outcome schema): a refresh outcome
// honestly reports expected / current / applied generation. On a pre-I/O stale
// reject the current generation is filled and applied stays zero; on success the
// applied generation is non-zero.
func TestBrokerOutcomeDistinguishesGenerations(t *testing.T) {
	t.Run("pre-I/O stale fills current, not applied", func(t *testing.T) {
		w, _ := newTestWatcher(1)
		w.refresher = &fakeRefresher{}
		setSession(w, 0, "b1") // gen 1
		login := w.streamers[0].Username

		w.RequestSessionRefresh(SessionRefreshRequest{
			RequestID: "r", Login: login, Mode: RefreshSession,
			ExpectedBroadcastID: "b1", ExpectedGeneration: 99, // mismatch
		})
		w.executeSessionRefreshes(occupantsFor(w, 0))

		out, _ := w.LastSessionRefresh(login)
		if !out.Stale {
			t.Fatalf("expected a stale outcome, got %+v", out)
		}
		if out.ExpectedSessionGeneration != 99 || out.CurrentSessionGeneration != 1 || out.AppliedSessionGeneration != 0 {
			t.Fatalf("stale outcome must carry expected=99 current=1 applied=0, got exp=%d cur=%d app=%d",
				out.ExpectedSessionGeneration, out.CurrentSessionGeneration, out.AppliedSessionGeneration)
		}
	})

	t.Run("success fills applied and current", func(t *testing.T) {
		w, _ := newTestWatcher(1)
		w.refresher = &fakeRefresher{}
		setSession(w, 0, "b1") // gen 1
		login := w.streamers[0].Username

		w.RequestSessionRefresh(SessionRefreshRequest{
			RequestID: "r", Login: login, Mode: RefreshSession,
			ExpectedBroadcastID: "b1", ExpectedGeneration: 1,
		})
		w.executeSessionRefreshes(occupantsFor(w, 0))

		out, _ := w.LastSessionRefresh(login)
		if !out.Success {
			t.Fatalf("expected a successful outcome, got %+v", out)
		}
		if out.ExpectedSessionGeneration != 1 || out.AppliedSessionGeneration == 0 || out.CurrentSessionGeneration == 0 {
			t.Fatalf("success outcome must carry expected=1, non-zero applied+current, got exp=%d cur=%d app=%d",
				out.ExpectedSessionGeneration, out.CurrentSessionGeneration, out.AppliedSessionGeneration)
		}
	})
}

// TestExecuteSessionRefreshesParallelNoRace (Group F): many slotted refreshes run
// in parallel; every worker writes only its own pre-allocated outcome slot and the
// parent never appends to the outcomes slice after launching workers. Run under
// -race (-count=100 in CI); a restored append+captured-slice pattern (M14) races.
func TestExecuteSessionRefreshesParallelNoRace(t *testing.T) {
	const n = 12
	w, _ := newTestWatcher(n)
	ref := &fakeRefresher{}
	w.refresher = ref

	idxs := make([]int, n)
	for i := 0; i < n; i++ {
		setSession(w, i, fmt.Sprintf("b%d", i))
		idxs[i] = i
		w.RequestSessionRefresh(SessionRefreshRequest{
			RequestID: fmt.Sprintf("r%d", i),
			Login:     w.streamers[i].Username,
			Mode:      RefreshSession,
		})
	}

	w.executeSessionRefreshes(occupantsFor(w, idxs...))

	// Every request produced exactly one published outcome for its own login.
	for i := 0; i < n; i++ {
		login := w.streamers[i].Username
		out, ok := w.LastSessionRefresh(login)
		if !ok || out.Login != login || out.RequestID != fmt.Sprintf("r%d", i) {
			t.Fatalf("missing/mismatched outcome for %s: ok=%v %+v", login, ok, out)
		}
	}
}
