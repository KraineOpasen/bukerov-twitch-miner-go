package models_test

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/eligibility"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestBonusReservationParityWithEvaluatePointsTask locks the invariant that the
// atomic reservation's liveness/capability prerequisites match
// EvaluatePointsTask(TaskBonusClaim) — there is exactly ONE eligibility policy,
// not a second divergent one. For each status/capability combination, given a
// current observation and a fresh claim id, the reservation is authorized iff the
// evaluator reports eligible.
func TestBonusReservationParityWithEvaluatePointsTask(t *testing.T) {
	cases := []struct {
		name   string
		status models.StreamerStatus
		cap    models.CapabilityState
	}{
		{"online+enabled", models.StatusOnline, models.CapabilityEnabled},
		{"online+unknown", models.StatusOnline, models.CapabilityUnknown},
		{"online+disabled", models.StatusOnline, models.CapabilityDisabled},
		{"offline+enabled", models.StatusOffline, models.CapabilityEnabled},
		{"status-unknown+enabled", models.StatusUnknown, models.CapabilityEnabled},
	}
	ev := eligibility.Evaluator{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := models.NewStreamer("chan", models.DefaultStreamerSettings())
			s.Status = tc.status
			obs := s.BeginChannelPointsContextObservation()
			reason := models.CapReasonConfirmedContext
			if tc.cap == models.CapabilityUnknown {
				reason = models.CapReasonTimeout
			}
			s.ApplyChannelPointsContext(obs, models.ChannelPointsContextSnapshot{
				Capability:       tc.cap,
				Reason:           reason,
				AvailableClaimID: "claim-1",
			})

			evalEligible := ev.EvaluatePointsTask(s, eligibility.TaskBonusClaim).Eligible
			reserved := s.ReserveBonusClaimIfEligible(obs, "claim-1").Authorized
			if evalEligible != reserved {
				t.Fatalf("parity broken for %s: EvaluatePointsTask.Eligible=%v, reservation.Authorized=%v",
					tc.name, evalEligible, reserved)
			}
		})
	}
}

// online+enabled is the only combo that should actually reserve.
func onlineEnabled(t *testing.T) *models.Streamer {
	t.Helper()
	s := models.NewStreamer("chan", models.DefaultStreamerSettings())
	s.Status = models.StatusOnline
	s.SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)
	return s
}

func observeBonusClaim(t *testing.T, s *models.Streamer, claimID string) uint64 {
	t.Helper()
	obs := s.BeginChannelPointsContextObservation()
	result := s.ApplyChannelPointsContext(obs, models.ChannelPointsContextSnapshot{
		Capability:       models.CapabilityEnabled,
		Reason:           models.CapReasonConfirmedContext,
		AvailableClaimID: claimID,
	})
	if !result.Applied {
		t.Fatal("bonus observation was not applied")
	}
	return obs
}

// TestBonusReservationDeniesOnStateChangeBeforeReserve covers the TOCTOU the
// atomic method closes: a streamer that goes Offline / loses the capability /
// gets a newer observation between "eligible" and "reserve" is denied at
// reservation time.
func TestBonusReservationDeniesOnStateChangeBeforeReserve(t *testing.T) {
	t.Run("stream down before reserve", func(t *testing.T) {
		s := onlineEnabled(t)
		obs := observeBonusClaim(t, s, "claim-1")
		// eligible now — but the streamer goes offline before the reservation.
		s.Status = models.StatusOffline
		r := s.ReserveBonusClaimIfEligible(obs, "claim-1")
		if r.Authorized {
			t.Fatal("reservation must be denied once the streamer is offline")
		}
		if r.Reason != models.BonusReservationOffline {
			t.Fatalf("reason = %v, want offline", r.Reason)
		}
	})

	t.Run("capability unknown before reserve", func(t *testing.T) {
		s := onlineEnabled(t)
		obs := observeBonusClaim(t, s, "claim-1")
		s.SetChannelPointsCapability(models.CapabilityUnknown, models.CapReasonTimeout)
		r := s.ReserveBonusClaimIfEligible(obs, "claim-1")
		if r.Authorized {
			t.Fatal("reservation must be denied once the capability is Unknown")
		}
		if r.Reason != models.BonusReservationCapabilityUnknown {
			t.Fatalf("reason = %v, want capability_unknown", r.Reason)
		}
	})

	t.Run("newer observation before reserve", func(t *testing.T) {
		s := onlineEnabled(t)
		obs := observeBonusClaim(t, s, "claim-1")
		s.BeginChannelPointsContextObservation() // a newer context begins
		r := s.ReserveBonusClaimIfEligible(obs, "claim-1")
		if r.Authorized {
			t.Fatal("reservation must be denied once a newer observation has begun")
		}
		if r.Reason != models.BonusReservationStaleObservation {
			t.Fatalf("reason = %v, want stale_observation", r.Reason)
		}
	})
}

// TestBonusReservationDedup: at most one reservation per claim id, including under
// concurrency (race-detector clean).
func TestBonusReservationDedup(t *testing.T) {
	t.Run("sequential duplicate denied", func(t *testing.T) {
		s := onlineEnabled(t)
		obs := observeBonusClaim(t, s, "claim-1")
		if !s.ReserveBonusClaimIfEligible(obs, "claim-1").Authorized {
			t.Fatal("first reservation should succeed")
		}
		r := s.ReserveBonusClaimIfEligible(obs, "claim-1")
		if r.Authorized {
			t.Fatal("duplicate claim id must not reserve twice")
		}
		if r.Reason != models.BonusReservationInFlight {
			t.Fatalf("reason = %v, want claim_in_flight", r.Reason)
		}
	})

	t.Run("empty claim id denied", func(t *testing.T) {
		s := onlineEnabled(t)
		obs := observeBonusClaim(t, s, "")
		if s.ReserveBonusClaimIfEligible(obs, "").Authorized {
			t.Fatal("empty claim id must not reserve")
		}
	})

	t.Run("concurrent reservers => exactly one wins", func(t *testing.T) {
		s := onlineEnabled(t)
		obs := observeBonusClaim(t, s, "claim-1")
		const n = 32
		var wins int32
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				if s.ReserveBonusClaimIfEligible(obs, "claim-1").Authorized {
					atomic.AddInt32(&wins, 1)
				}
			}()
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("exactly one reservation must win per claim id, got %d", wins)
		}
	})
}

func TestBonusClaimLedgerKeepsExactCompletedTombstones(t *testing.T) {
	s := onlineEnabled(t)

	obsA := observeBonusClaim(t, s, "claim-a")
	claimA := s.ReserveBonusClaimIfEligible(obsA, "claim-a")
	if !claimA.Authorized {
		t.Fatal("claim A should reserve")
	}
	completedA := s.CompleteBonusClaim(claimA, models.BonusClaimCompletionSucceeded)
	if !completedA.Applied || !completedA.FreshSuccess {
		t.Fatalf("claim A completion = %+v, want applied fresh success", completedA)
	}

	obsB := observeBonusClaim(t, s, "claim-b")
	claimB := s.ReserveBonusClaimIfEligible(obsB, "claim-b")
	if !claimB.Authorized {
		t.Fatal("claim B should reserve independently")
	}
	s.CompleteBonusClaim(claimB, models.BonusClaimCompletionSucceeded)

	delayedA := observeBonusClaim(t, s, "claim-a")
	reservation := s.ReserveBonusClaimIfEligible(delayedA, "claim-a")
	if reservation.Authorized || reservation.Reason != models.BonusReservationCompleted {
		t.Fatalf("delayed A after A/B completion = %+v, want completed tombstone", reservation)
	}
	if duplicate := s.CompleteBonusClaim(claimA, models.BonusClaimCompletionSucceeded); duplicate.Applied || duplicate.FreshSuccess {
		t.Fatalf("duplicate completion = %+v, want ignored", duplicate)
	}
}

func TestBonusClaimLedgerDoesNotCollapseDifferentIDsOrStreamers(t *testing.T) {
	t.Run("different IDs on one streamer", func(t *testing.T) {
		s := onlineEnabled(t)
		first := s.ReserveCurrentBonusClaimIfEligible("claim-a")
		second := s.ReserveCurrentBonusClaimIfEligible("claim-b")
		if !first.Authorized || !second.Authorized {
			t.Fatalf("different IDs must reserve independently: first=%+v second=%+v", first, second)
		}
	})

	t.Run("same ID on different streamers", func(t *testing.T) {
		firstStreamer := onlineEnabled(t)
		secondStreamer := onlineEnabled(t)
		first := firstStreamer.ReserveCurrentBonusClaimIfEligible("claim-shared")
		second := secondStreamer.ReserveCurrentBonusClaimIfEligible("claim-shared")
		if !first.Authorized || !second.Authorized {
			t.Fatalf("streamer ledgers must be isolated: first=%+v second=%+v", first, second)
		}
	})
}

func TestBonusClaimRetryRequiresNewAuthoritativeObservationAndIsBounded(t *testing.T) {
	s := onlineEnabled(t)
	firstObservation := observeBonusClaim(t, s, "claim-1")
	first := s.ReserveBonusClaimIfEligible(firstObservation, "claim-1")
	if !first.Authorized {
		t.Fatal("first attempt should reserve")
	}
	if result := s.CompleteBonusClaim(first, models.BonusClaimCompletionProvenNotExecuted); !result.Applied || result.FreshSuccess {
		t.Fatalf("proved non-execution completion = %+v", result)
	}

	if retry := s.ReserveBonusClaimIfEligible(firstObservation, "claim-1"); retry.Authorized || retry.Reason != models.BonusReservationRetryNeedsObservation {
		t.Fatalf("same observation retry = %+v, want a newer observation", retry)
	}
	if retry := s.ReserveCurrentBonusClaimIfEligible("claim-1"); retry.Authorized || retry.Reason != models.BonusReservationRetryNeedsObservation {
		t.Fatalf("PubSub/direct retry = %+v, want a newer observation", retry)
	}

	secondObservation := observeBonusClaim(t, s, "claim-1")
	second := s.ReserveBonusClaimIfEligible(secondObservation, "claim-1")
	if !second.Authorized {
		t.Fatalf("fresh authoritative observation should arm the one retry: %+v", second)
	}
	s.CompleteBonusClaim(second, models.BonusClaimCompletionProvenNotExecuted)

	thirdObservation := observeBonusClaim(t, s, "claim-1")
	third := s.ReserveBonusClaimIfEligible(thirdObservation, "claim-1")
	if third.Authorized || third.Reason != models.BonusReservationRetryExhausted {
		t.Fatalf("third attempt = %+v, want exhausted terminal state", third)
	}
}

func TestBonusClaimTerminalOutcomesFailClosed(t *testing.T) {
	tests := []struct {
		name       string
		completion models.BonusClaimCompletion
		wantReason models.BonusReservationReason
	}{
		{"zero value", models.BonusClaimCompletionInvalid, models.BonusReservationIndeterminate},
		{"rejected", models.BonusClaimCompletionRejected, models.BonusReservationTerminalRejected},
		{"indeterminate", models.BonusClaimCompletionIndeterminate, models.BonusReservationIndeterminate},
		{"reconciled", models.BonusClaimCompletionReconciled, models.BonusReservationCompleted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := onlineEnabled(t)
			reservation := s.ReserveCurrentBonusClaimIfEligible("claim-1")
			if !reservation.Authorized {
				t.Fatal("initial attempt should reserve")
			}
			completion := s.CompleteBonusClaim(reservation, test.completion)
			if !completion.Applied || completion.FreshSuccess {
				t.Fatalf("completion = %+v, want applied without fresh event", completion)
			}
			duplicate := s.ReserveCurrentBonusClaimIfEligible("claim-1")
			if duplicate.Authorized || duplicate.Reason != test.wantReason {
				t.Fatalf("terminal duplicate = %+v, want reason %v", duplicate, test.wantReason)
			}
		})
	}
}

func TestObservedBonusReservationRequiresAdvertisedClaimID(t *testing.T) {
	s := onlineEnabled(t)
	obs := observeBonusClaim(t, s, "claim-a")
	reservation := s.ReserveBonusClaimIfEligible(obs, "claim-b")
	if reservation.Authorized || reservation.Reason != models.BonusReservationClaimNotObserved {
		t.Fatalf("unobserved claim = %+v, want claim_not_observed", reservation)
	}
}
