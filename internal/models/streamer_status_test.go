package models

import (
	"sync"
	"testing"
	"time"
)

// TestNewStreamerStartsUnknown pins the initial-state contract: a freshly created
// streamer is UNKNOWN (zero value), never a false offline, and stamps no OfflineAt.
func TestNewStreamerStartsUnknown(t *testing.T) {
	s := NewStreamer("fresh", DefaultStreamerSettings())

	if s.GetStatus() != StatusUnknown {
		t.Fatalf("new streamer status = %v, want unknown", s.GetStatus())
	}
	if s.GetLastConfirmedStatus() != StatusUnknown {
		t.Errorf("new streamer last-confirmed = %v, want unknown (never confirmed)", s.GetLastConfirmedStatus())
	}
	if s.GetStatusReason() != ReasonInitial {
		t.Errorf("new streamer reason = %q, want %q", s.GetStatusReason(), ReasonInitial)
	}
	if s.GetIsOnline() {
		t.Error("GetIsOnline() must be false for an unknown streamer")
	}
	if !s.GetOfflineAt().IsZero() {
		t.Error("a new streamer must not have an OfflineAt stamped")
	}
}

// TestTransitionMatrix table-drives the required 10-case transition matrix plus
// the no-op cases, asserting the resulting status, the confirmed-transition
// signals (which drive exactly-once events and notifications), and OfflineAt.
func TestTransitionMatrix(t *testing.T) {
	// setup returns a streamer in the requested starting state.
	newUnknownInitial := func() *Streamer { return NewStreamer("s", DefaultStreamerSettings()) }
	newOnline := func() *Streamer {
		s := NewStreamer("s", DefaultStreamerSettings())
		s.SetConfirmedOnline()
		return s
	}
	newOffline := func() *Streamer {
		s := NewStreamer("s", DefaultStreamerSettings())
		s.SetConfirmedOffline()
		return s
	}
	newUnknownAfterOnline := func() *Streamer {
		s := newOnline()
		s.SetUnknown(ReasonTransportError)
		return s
	}
	newUnknownAfterOffline := func() *Streamer {
		s := newOffline()
		s.SetUnknown(ReasonTransportError)
		return s
	}

	tests := []struct {
		name             string
		setup            func() *Streamer
		apply            func(*Streamer) StatusTransition
		wantStatus       StreamerStatus
		wantOnlineConf   bool
		wantOfflineConf  bool
		wantOfflineAtSet bool // OfflineAt should be non-zero after the transition
	}{
		{"initial unknown -> online", newUnknownInitial, func(s *Streamer) StatusTransition { return s.SetConfirmedOnline() }, StatusOnline, true, false, false},
		{"initial unknown -> offline", newUnknownInitial, func(s *Streamer) StatusTransition { return s.SetConfirmedOffline() }, StatusOffline, false, false, true},
		{"online -> online noop", newOnline, func(s *Streamer) StatusTransition { return s.SetConfirmedOnline() }, StatusOnline, false, false, false},
		{"offline -> offline noop", newOffline, func(s *Streamer) StatusTransition { return s.SetConfirmedOffline() }, StatusOffline, false, false, true},
		{"online -> unknown", newOnline, func(s *Streamer) StatusTransition { return s.SetUnknown(ReasonTransportError) }, StatusUnknown, false, false, false},
		{"unknown(online) -> online recovery", newUnknownAfterOnline, func(s *Streamer) StatusTransition { return s.SetConfirmedOnline() }, StatusOnline, false, false, false},
		{"unknown(online) -> offline", newUnknownAfterOnline, func(s *Streamer) StatusTransition { return s.SetConfirmedOffline() }, StatusOffline, false, true, true},
		{"offline -> unknown", newOffline, func(s *Streamer) StatusTransition { return s.SetUnknown(ReasonTransportError) }, StatusUnknown, false, false, true},
		{"unknown(offline) -> offline", newUnknownAfterOffline, func(s *Streamer) StatusTransition { return s.SetConfirmedOffline() }, StatusOffline, false, false, true},
		{"offline -> online", newOffline, func(s *Streamer) StatusTransition { return s.SetConfirmedOnline() }, StatusOnline, true, false, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.setup()
			tr := tc.apply(s)
			if s.GetStatus() != tc.wantStatus {
				t.Errorf("status = %v, want %v", s.GetStatus(), tc.wantStatus)
			}
			if tr.OnlineConfirmed != tc.wantOnlineConf {
				t.Errorf("OnlineConfirmed = %v, want %v", tr.OnlineConfirmed, tc.wantOnlineConf)
			}
			if tr.OfflineConfirmed != tc.wantOfflineConf {
				t.Errorf("OfflineConfirmed = %v, want %v", tr.OfflineConfirmed, tc.wantOfflineConf)
			}
			if got := !s.GetOfflineAt().IsZero(); got != tc.wantOfflineAtSet {
				t.Errorf("OfflineAt set = %v, want %v", got, tc.wantOfflineAtSet)
			}
		})
	}
}

// TestOnlineToUnknownPreservesEverything covers case 5: online→unknown must not
// set OfflineAt, must not emit an offline transition, must not reset the streak,
// and must preserve the stream data and last-confirmed=online.
func TestOnlineToUnknownPreservesEverything(t *testing.T) {
	s := NewStreamer("keeper", DefaultStreamerSettings())
	s.SetConfirmedOnline()
	s.Stream.Update("bid-9", "title", &Game{ID: "g", Name: "G"}, nil, 100)
	s.Stream.MinuteWatched = 7
	s.Stream.WatchStreakMissing = false
	onlineAt := s.GetOnlineAt()

	tr := s.SetUnknown(ReasonTimeout)

	if tr.OfflineConfirmed {
		t.Error("online->unknown must not report a confirmed-offline transition")
	}
	if !s.GetOfflineAt().IsZero() {
		t.Error("online->unknown must not stamp OfflineAt")
	}
	if s.GetLastConfirmedStatus() != StatusOnline {
		t.Errorf("last-confirmed after online->unknown = %v, want online", s.GetLastConfirmedStatus())
	}
	if s.GetOnlineAt() != onlineAt {
		t.Error("online->unknown must not change OnlineAt")
	}
	if got := s.Stream.GetMinuteWatched(); got != 7 {
		t.Errorf("online->unknown must preserve MinuteWatched, got %v", got)
	}
	if s.Stream.GetWatchStreakMissing() {
		t.Error("online->unknown must preserve the earned streak")
	}
	if s.Stream.GetBroadcastID() != "bid-9" {
		t.Error("online->unknown must preserve the broadcast ID")
	}
	if s.GetStatusReason() != ReasonTimeout {
		t.Errorf("reason = %q, want timeout", s.GetStatusReason())
	}
}

// TestUnknownRecoveryDoesNotResetStreak covers case 6: unknown(last online)->online
// is a recovery continuation — no duplicate online transition, no streak reset.
func TestUnknownRecoveryDoesNotResetStreak(t *testing.T) {
	s := NewStreamer("recover", DefaultStreamerSettings())
	s.SetConfirmedOnline()
	s.Stream.Update("bid-1", "t", &Game{ID: "g", Name: "G"}, nil, 10)
	s.Stream.MinuteWatched = 4
	s.Stream.WatchStreakMissing = false

	s.SetUnknown(ReasonTransportError)
	tr := s.SetConfirmedOnline() // recovery on the same broadcast

	if tr.OnlineConfirmed {
		t.Error("recovery unknown(online)->online must NOT report a new online transition")
	}
	if got := s.Stream.GetMinuteWatched(); got != 4 {
		t.Errorf("recovery must not reset the streak accumulator, got %v", got)
	}
	if s.Stream.GetWatchStreakMissing() {
		t.Error("recovery must not re-arm the streak")
	}
}

// TestOfflineToUnknownPreservesOfflineAt covers case 8: offline->unknown stays
// non-online, keeps last-confirmed=offline and preserves OfflineAt.
func TestOfflineToUnknownPreservesOfflineAt(t *testing.T) {
	s := NewStreamer("off", DefaultStreamerSettings())
	s.SetConfirmedOffline()
	offlineAt := s.GetOfflineAt()
	if offlineAt.IsZero() {
		t.Fatal("precondition: offline should stamp OfflineAt")
	}

	s.SetUnknown(ReasonTransportError)

	if s.GetIsOnline() {
		t.Error("offline->unknown must not be online")
	}
	if s.GetOfflineAt() != offlineAt {
		t.Error("offline->unknown must preserve OfflineAt")
	}
	if s.GetLastConfirmedStatus() != StatusOffline {
		t.Errorf("last-confirmed = %v, want offline", s.GetLastConfirmedStatus())
	}
}

// TestSeqGuardDiscardsStaleCheck covers the stale-observation guard: a check that
// snapshotted an older sequence must be discarded once a newer authoritative
// transition has bumped the sequence, so it cannot overwrite the newer state.
func TestSeqGuardDiscardsStaleCheck(t *testing.T) {
	s := NewStreamer("guard", DefaultStreamerSettings())

	_, staleSeq := s.StatusSnapshot()
	// A newer authoritative transition lands (bumps the sequence).
	s.SetConfirmedOnline()

	// The stale check tries to mark unknown using the OLD sequence.
	tr := s.ApplyCheckResultIfCurrent(staleSeq, StatusUnknown, ReasonTransportError)

	if !tr.Stale {
		t.Error("a stale check result must be flagged Stale")
	}
	if tr.Changed() {
		t.Error("a stale check must not change the status")
	}
	if s.GetStatus() != StatusOnline {
		t.Errorf("status = %v, want online (stale unknown discarded)", s.GetStatus())
	}
}

// TestSeqUnknownDoesNotBlockConfirm proves unknown does NOT bump the sequence, so
// a genuine confirm captured at the same sequence still applies after a concurrent
// unknown — a successful confirm always beats a concurrent inconclusive check.
func TestSeqUnknownDoesNotBlockConfirm(t *testing.T) {
	s := NewStreamer("seq", DefaultStreamerSettings())
	s.SetConfirmedOnline()

	_, seq := s.StatusSnapshot()
	// An inconclusive check marks unknown (must not bump the sequence).
	s.SetUnknown(ReasonTransportError)
	// A confirm captured at the same sequence still applies.
	tr := s.ApplyCheckResultIfCurrent(seq, StatusOnline, "")

	if tr.Stale {
		t.Error("a confirm at the pre-unknown sequence must not be discarded (unknown must not bump the sequence)")
	}
	if s.GetStatus() != StatusOnline {
		t.Errorf("status = %v, want online", s.GetStatus())
	}
}

// TestRacingTransitionsDeterministic runs a storm of confirmed-online,
// confirmed-offline and unknown transitions concurrently. Under -race it proves
// there is no data race and that the terminal state is a valid, self-consistent
// tri-state (with OfflineAt never stamped while the terminal state is unknown-
// from-online... the important invariant: a confirmed status is internally
// consistent). Exactly-once is covered elsewhere; here we assert no torn state.
func TestRacingTransitionsDeterministic(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		s := NewStreamer("racer", DefaultStreamerSettings())
		const n = 24
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				switch i % 3 {
				case 0:
					s.SetConfirmedOnline()
				case 1:
					s.SetConfirmedOffline()
				default:
					s.SetUnknown(ReasonTransportError)
				}
			}(i)
		}
		close(start)
		wg.Wait()

		st := s.GetStatus()
		if st != StatusOnline && st != StatusOffline && st != StatusUnknown {
			t.Fatalf("terminal status is not a valid tri-state: %v", st)
		}
		// A confirmed status must carry a matching last-confirmed value.
		if st == StatusOnline && s.GetLastConfirmedStatus() != StatusOnline {
			t.Fatalf("online terminal but last-confirmed=%v", s.GetLastConfirmedStatus())
		}
		if st == StatusOffline && s.GetLastConfirmedStatus() != StatusOffline {
			t.Fatalf("offline terminal but last-confirmed=%v", s.GetLastConfirmedStatus())
		}
	}
}

// TestOfflineToOnlineWithinAndBeyondGrace covers cases 11 and 12 at the domain
// level using the BroadcastID=="" fallback heuristic (no Stream.Update here).
func TestOfflineToOnlineWithinAndBeyondGrace(t *testing.T) {
	t.Run("within grace preserves streak", func(t *testing.T) {
		s := NewStreamer("g1", DefaultStreamerSettings())
		s.SetConfirmedOnline()
		s.Stream.MinuteWatched = 5
		s.Stream.WatchStreakMissing = false
		s.SetConfirmedOffline() // OfflineAt ~ now (within grace)
		s.SetConfirmedOnline()
		if got := s.Stream.GetMinuteWatched(); got != 5 {
			t.Errorf("within-grace re-online must preserve streak, got %v", got)
		}
	})
	t.Run("beyond grace re-arms streak", func(t *testing.T) {
		s := NewStreamer("g2", DefaultStreamerSettings())
		s.SetConfirmedOnline()
		s.Stream.MinuteWatched = 5
		s.Stream.WatchStreakMissing = false
		s.SetConfirmedOffline()
		s.OfflineAt = time.Now().Add(-watchStreakContinuityGrace - time.Minute)
		s.SetConfirmedOnline()
		if got := s.Stream.GetMinuteWatched(); got != 0 {
			t.Errorf("beyond-grace re-online must re-arm streak (0), got %v", got)
		}
	})
}
