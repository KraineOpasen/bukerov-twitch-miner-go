package models

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchStreakOwnerBoundaryTable(t *testing.T) {
	for _, minutes := range []float64{0, 7, 8, 15, 19} {
		t.Run(fmt.Sprintf("%.0fm", minutes), func(t *testing.T) {
			s := NewStream()
			s.Update("b1", "t", nil, nil, 1)
			s.MinuteWatched = minutes
			decision := s.EvaluateWatchStreak(time.Unix(100, 0))
			want := WatchStreakPursuing
			if minutes == 0 {
				want = WatchStreakEligible
			}
			if decision.State != want || !decision.PursuitEligible || decision.Transitioned {
				t.Fatalf("decision=%+v, want %s eligible without transition", decision, want)
			}
			if s.StreakPursuitTimedOut() {
				t.Fatal("sub-cap decision latched timeout")
			}
		})
	}

	s := NewStream()
	s.Update("b1", "t", nil, nil, 1)
	s.MinuteWatched = WatchStreakPursuitCapMinutes
	first := s.EvaluateWatchStreak(time.Unix(200, 0))
	if first.State != WatchStreakTimedOutUnknown || first.PursuitEligible || !first.Transitioned {
		t.Fatalf("first cap decision=%+v, want transitioned TIMED_OUT_UNKNOWN", first)
	}
	if first.Persistence.Revision != 1 || first.Persistence.Timeout == nil || first.Persistence.Timeout.BroadcastID != "b1" {
		t.Fatalf("timeout persistence=%+v, want revision 1 bound to b1", first.Persistence)
	}
	second := s.EvaluateWatchStreak(time.Unix(300, 0))
	if second.State != WatchStreakTimedOutUnknown || second.Transitioned {
		t.Fatalf("repeated cap decision=%+v, want inert TIMED_OUT_UNKNOWN", second)
	}
}

func TestWatchStreakBoundGrantBeforeAtAndAfterCap(t *testing.T) {
	tests := []struct {
		name            string
		minutes         float64
		timeoutFirst    bool
		wantTimeoutFact bool
	}{
		{name: "before cap", minutes: 19},
		{name: "at boundary grant first", minutes: 20},
		{name: "after timeout", minutes: 20, timeoutFirst: true, wantTimeoutFact: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStream()
			s.Update("b1", "t", nil, nil, 1)
			s.MinuteWatched = tc.minutes
			if tc.timeoutFirst {
				if got := s.EvaluateWatchStreak(time.Unix(100, 0)).State; got != WatchStreakTimedOutUnknown {
					t.Fatalf("setup state=%s, want timeout", got)
				}
			}
			result := s.AcceptWatchStreakGrant(WatchStreakGrantEvent{
				EventID: "grant-b1", AcceptedAt: time.Unix(101, 0), ProvenBroadcastID: "b1",
			})
			if result.Admission != WatchStreakGrantNewBound || result.Decision.State != WatchStreakGranted || result.Decision.PursuitEligible {
				t.Fatalf("grant result=%+v, want NEW_BOUND/GRANTED", result)
			}
			if (result.Persistence.Timeout != nil) != tc.wantTimeoutFact {
				t.Fatalf("timeout fact=%+v, want present=%v", result.Persistence.Timeout, tc.wantTimeoutFact)
			}
			if later := s.EvaluateWatchStreak(time.Unix(102, 0)); later.State != WatchStreakGranted || later.PursuitEligible || later.Transitioned {
				t.Fatalf("post-grant state=%+v, grant must dominate timeout forever", later)
			}
			if replay := s.AcceptWatchStreakGrant(WatchStreakGrantEvent{EventID: "grant-b1", AcceptedAt: time.Unix(103, 0), ProvenBroadcastID: "b1"}); replay.Admission != WatchStreakGrantDuplicate {
				t.Fatalf("replay admission=%s, want DUPLICATE", replay.Admission)
			}
		})
	}
}

func TestWatchStreakUnboundGrantNeverMutatesCurrentPursuit(t *testing.T) {
	s := NewStream()
	s.Update("new-broadcast", "t", nil, nil, 1)
	s.MinuteWatched = 10
	before := s.EvaluateWatchStreak(time.Unix(100, 0))
	result := s.AcceptWatchStreakGrant(WatchStreakGrantEvent{
		EventID: "delayed-unbound", AcceptedAt: time.Unix(101, 0),
	})
	if result.Admission != WatchStreakGrantNewUnbound || result.Decision.State != before.State || !result.Decision.PursuitEligible {
		t.Fatalf("unbound result=%+v, before=%+v", result, before)
	}
	if len(result.Persistence.Grants) != 1 || result.Persistence.Grants[0].Binding != WatchStreakGrantUnbound || result.Persistence.Grants[0].BroadcastID != "" {
		t.Fatalf("unbound ledger=%+v", result.Persistence.Grants)
	}
	if bid, _ := s.StreakEarnedGrant(); bid != "" {
		t.Fatalf("unbound grant acquired invented BroadcastID %q", bid)
	}
	if got := s.EvaluateWatchStreak(time.Unix(102, 0)); got.State != WatchStreakPursuing || !got.PursuitEligible {
		t.Fatalf("unbound grant mutated current pursuit: %+v", got)
	}
}

func TestWatchStreakTimeoutSurvivesSlotLossAndNewBroadcastRearms(t *testing.T) {
	s := NewStream()
	s.Update("b1", "t", nil, nil, 1)
	s.MinuteWatched = 20
	if got := s.EvaluateWatchStreak(time.Unix(100, 0)); got.State != WatchStreakTimedOutUnknown {
		t.Fatalf("setup=%+v", got)
	}
	s.ResetWatchContinuity()
	if got := s.EvaluateWatchStreak(time.Unix(101, 0)); got.State != WatchStreakTimedOutUnknown || got.PursuitEligible {
		t.Fatalf("slot loss reopened same broadcast: %+v", got)
	}
	s.Update("b1", "same", nil, nil, 1)
	if got := s.EvaluateWatchStreak(time.Unix(102, 0)); got.State != WatchStreakTimedOutUnknown {
		t.Fatalf("same BroadcastID update cleared timeout: %+v", got)
	}
	s.Update("b2", "new", nil, nil, 1)
	if got := s.EvaluateWatchStreak(time.Unix(103, 0)); got.State != WatchStreakEligible || !got.PursuitEligible || got.ContinuousMinutes != 0 {
		t.Fatalf("new BroadcastID did not re-arm: %+v", got)
	}
}

func TestWatchStreakHydrationPreservesTerminalFactsAndReplayLedger(t *testing.T) {
	source := NewStream()
	source.Update("b1", "t", nil, nil, 1)
	source.MinuteWatched = 20
	source.EvaluateWatchStreak(time.Unix(100, 0))
	source.AcceptWatchStreakGrant(WatchStreakGrantEvent{EventID: "unbound-1", AcceptedAt: time.Unix(101, 0)})
	snapshot := source.WatchStreakPersistence()

	hydrated := NewStream()
	hydrated.HydrateWatchStreak(snapshot)
	hydrated.Update("b1", "t", nil, nil, 1)
	if got := hydrated.EvaluateWatchStreak(time.Unix(102, 0)); got.State != WatchStreakTimedOutUnknown || got.PursuitEligible {
		t.Fatalf("hydrated state=%+v, want timeout", got)
	}
	if replay := hydrated.AcceptWatchStreakGrant(WatchStreakGrantEvent{EventID: "unbound-1", AcceptedAt: time.Unix(103, 0)}); replay.Admission != WatchStreakGrantDuplicate {
		t.Fatalf("hydrated replay admission=%s, want DUPLICATE", replay.Admission)
	}
}

func TestWatchStreakConcurrentGrantTimeoutAndBroadcastTransitionsLinearize(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		s := NewStream()
		s.Update("b1", "t", nil, nil, 1)
		s.MinuteWatched = 20
		var accepted atomic.Int64
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(4)
		go func() {
			defer wg.Done()
			<-start
			s.EvaluateWatchStreak(time.Unix(100, 0))
		}()
		for i := 0; i < 2; i++ {
			go func() {
				defer wg.Done()
				<-start
				if s.AcceptWatchStreakGrant(WatchStreakGrantEvent{
					EventID: "grant-b1", AcceptedAt: time.Unix(101, 0), ProvenBroadcastID: "b1",
				}).NewlyAccepted() {
					accepted.Add(1)
				}
			}()
		}
		go func() {
			defer wg.Done()
			<-start
			s.Update("b2", "new", nil, nil, 1)
		}()
		close(start)
		wg.Wait()

		if accepted.Load() != 1 {
			t.Fatalf("iteration %d: accepted=%d, want exactly one", iteration, accepted.Load())
		}
		decision := s.EvaluateWatchStreak(time.Unix(102, 0))
		if decision.BroadcastID != "b2" || decision.State != WatchStreakEligible || !decision.PursuitEligible {
			t.Fatalf("iteration %d: final current decision=%+v, old b1 transition corrupted b2", iteration, decision)
		}
		persisted := s.WatchStreakPersistence()
		if len(persisted.Grants) != 1 || persisted.Grants[0].BroadcastID != "b1" {
			t.Fatalf("iteration %d: grant ledger=%+v, want one fact bound only to b1", iteration, persisted.Grants)
		}
	}
}
