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
			if tc.cap != models.CapabilityUnknown {
				s.SetChannelPointsCapability(tc.cap, models.CapReasonConfirmedContext)
			}
			obs := s.BeginChannelPointsContextObservation()

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

// TestBonusReservationDeniesOnStateChangeBeforeReserve covers the TOCTOU the
// atomic method closes: a streamer that goes Offline / loses the capability /
// gets a newer observation between "eligible" and "reserve" is denied at
// reservation time.
func TestBonusReservationDeniesOnStateChangeBeforeReserve(t *testing.T) {
	t.Run("stream down before reserve", func(t *testing.T) {
		s := onlineEnabled(t)
		obs := s.BeginChannelPointsContextObservation()
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
		obs := s.BeginChannelPointsContextObservation()
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
		obs := s.BeginChannelPointsContextObservation()
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
		obs := s.BeginChannelPointsContextObservation()
		if !s.ReserveBonusClaimIfEligible(obs, "claim-1").Authorized {
			t.Fatal("first reservation should succeed")
		}
		r := s.ReserveBonusClaimIfEligible(obs, "claim-1")
		if r.Authorized {
			t.Fatal("duplicate claim id must not reserve twice")
		}
		if r.Reason != models.BonusReservationDuplicate {
			t.Fatalf("reason = %v, want duplicate_claim", r.Reason)
		}
	})

	t.Run("empty claim id denied", func(t *testing.T) {
		s := onlineEnabled(t)
		obs := s.BeginChannelPointsContextObservation()
		if s.ReserveBonusClaimIfEligible(obs, "").Authorized {
			t.Fatal("empty claim id must not reserve")
		}
	})

	t.Run("concurrent reservers => exactly one wins", func(t *testing.T) {
		s := onlineEnabled(t)
		obs := s.BeginChannelPointsContextObservation()
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
