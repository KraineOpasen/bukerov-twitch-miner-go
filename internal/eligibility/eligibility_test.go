package eligibility

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

var testNow = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

func fixedClock(t time.Time) models.Clock { return func() time.Time { return t } }

func onlineStreamer(cap models.CapabilityState) *models.Streamer {
	s := models.NewStreamer("bob", models.DefaultStreamerSettings())
	s.ChannelID = "chan-bob"
	s.SetConfirmedOnline()
	if cap != models.CapabilityUnknown {
		reason := models.CapReasonConfirmedContext
		if cap == models.CapabilityDisabled {
			reason = models.CapReasonConfirmedDisabled
		}
		s.SetChannelPointsCapability(cap, reason)
	}
	return s
}

// --- Capability matrix for points tasks (BKM-015 16-20, 13) --------------

func TestPointsTaskCapabilityMatrix(t *testing.T) {
	e := Evaluator{Clock: fixedClock(testNow)}
	pointsTasks := []Task{TaskWatchStreak, TaskPrediction, TaskBonusClaim, TaskCommunityGoal, TaskCustomReward, TaskChannelPoints}

	for _, task := range pointsTasks {
		// Enable the relevant user setting so capability is the deciding gate.
		s := onlineStreamer(models.CapabilityDisabled)
		st := s.GetSettings()
		st.WatchStreak, st.MakePredictions, st.CommunityGoals = true, true, true
		s.SetSettings(st)

		if d := e.EvaluatePointsTask(s, task); d.Eligible || d.Reason != ReasonCapabilityDisabled {
			t.Errorf("task %s disabled: got %+v, want blocked/capability_disabled", task, d)
		}

		// 15: unknown capability blocks a new points action (but not coerced).
		s2 := onlineStreamer(models.CapabilityUnknown)
		s2.SetSettings(st)
		if d := e.EvaluatePointsTask(s2, task); d.Eligible || d.State != StateUnknown || d.Reason != ReasonCapabilityUnknown {
			t.Errorf("task %s unknown: got %+v, want unknown/capability_unknown", task, d)
		}

		// 13: enabled capability + user setting on -> eligible.
		s3 := onlineStreamer(models.CapabilityEnabled)
		s3.SetSettings(st)
		if d := e.EvaluatePointsTask(s3, task); !d.Eligible {
			t.Errorf("task %s enabled: got %+v, want eligible", task, d)
		}
	}
}

// 20: a user-disabled feature stays blocked even when capability is enabled.
func TestUserDisabledOverridesCapability(t *testing.T) {
	e := Evaluator{Clock: fixedClock(testNow)}
	s := onlineStreamer(models.CapabilityEnabled)
	st := s.GetSettings()
	st.WatchStreak = false
	s.SetSettings(st)
	if d := e.EvaluatePointsTask(s, TaskWatchStreak); d.Eligible || d.Reason != ReasonUserDisabled {
		t.Fatalf("user-disabled must stay blocked: %+v", d)
	}
}

// 21: DisableWatch is a hard opt-out for watch-slot tasks.
func TestDisableWatchBlocksWatchTasks(t *testing.T) {
	e := Evaluator{Clock: fixedClock(testNow)}
	s := onlineStreamer(models.CapabilityEnabled)
	st := s.GetSettings()
	st.DisableWatch = true
	s.SetSettings(st)
	if d := e.EvaluatePointsTask(s, TaskWatchStreak); d.Eligible || d.Reason != ReasonWatchDisabled {
		t.Fatalf("DisableWatch must block a watch task: %+v", d)
	}
	// A non-watch task (prediction) is not gated by DisableWatch.
	if d := e.EvaluatePointsTask(s, TaskPrediction); d.Reason == ReasonWatchDisabled {
		t.Fatal("DisableWatch must not gate a non-watch task")
	}
}

// 22: capability unknown is never coerced to enabled/disabled by the adapter.
func TestCapabilityUnknownNotCoerced(t *testing.T) {
	e := Evaluator{Clock: fixedClock(testNow)}
	s := onlineStreamer(models.CapabilityUnknown)
	d := e.EvaluatePointsTask(s, TaskBonusClaim)
	if d.State != StateUnknown {
		t.Fatalf("unknown capability must yield StateUnknown, not %v", d.State)
	}
}

// --- Drops eligibility (BKM-015 14,15; ACL; feasibility) ------------------

func dropsStreamer(cap models.CapabilityState, gameID string) *models.Streamer {
	s := onlineStreamer(cap)
	s.Stream.Update("bc-1", "title", &models.Game{ID: gameID}, nil, 0)
	return s
}

func activeCampaign(gameID, channelID string, restricted bool) (*models.Campaign, *models.Drop) {
	drop := &models.Drop{ID: "d1", Name: "Skin", MinutesRequired: 60, CurrentMinutesWatched: 10}
	c := &models.Campaign{
		ID:      "camp-1",
		Game:    &models.Game{ID: gameID},
		StartAt: testNow.Add(-time.Hour),
		EndAt:   testNow.Add(10 * time.Hour),
		Drops:   []*models.Drop{drop},
	}
	if restricted {
		c.ACL = models.CampaignACL{State: models.ACLRestricted, ChannelIDs: []string{channelID}, Complete: true, Source: models.ACLSourceCampaignDetails}
	} else {
		c.ACL = models.CampaignACL{State: models.ACLUnrestricted, Complete: true, Source: models.ACLSourceCampaignDetails}
	}
	return c, drop
}

// 14 & 15: disabled/unknown Channel Points do NOT block an eligible drop.
func TestDropsNotBlockedByChannelPoints(t *testing.T) {
	e := Evaluator{Clock: fixedClock(testNow)}
	for _, cap := range []models.CapabilityState{models.CapabilityDisabled, models.CapabilityUnknown} {
		s := dropsStreamer(cap, "g1")
		c, drop := activeCampaign("g1", s.ChannelID, false)
		if d := e.EvaluateDrops(s, c, drop, AvailabilityYes); !d.Eligible {
			t.Errorf("cap=%v: drops must stay eligible, got %+v", cap, d)
		}
	}
}

func TestDropsACLGates(t *testing.T) {
	e := Evaluator{Clock: fixedClock(testNow)}
	s := dropsStreamer(models.CapabilityEnabled, "g1")

	// Restricted, channel in ACL -> eligible.
	c, drop := activeCampaign("g1", s.ChannelID, true)
	if d := e.EvaluateDrops(s, c, drop, AvailabilityYes); !d.Eligible {
		t.Fatalf("in-ACL channel should be eligible: %+v", d)
	}

	// Restricted, channel NOT in ACL -> blocked.
	c2, drop2 := activeCampaign("g1", "other-chan", true)
	if d := e.EvaluateDrops(s, c2, drop2, AvailabilityYes); d.Eligible || d.Reason != ReasonChannelNotAllowed {
		t.Fatalf("out-of-ACL channel should be blocked: %+v", d)
	}

	// ACL unknown -> unknown (fail closed, never widen).
	c3, drop3 := activeCampaign("g1", s.ChannelID, true)
	c3.ACL = models.CampaignACL{State: models.ACLUnknown, Source: models.ACLSourceCampaignDetails}
	if d := e.EvaluateDrops(s, c3, drop3, AvailabilityYes); d.Eligible || d.Reason != ReasonACLUnknown {
		t.Fatalf("unknown ACL should be unknown/blocked: %+v", d)
	}
}

// 23 (BKM-010): channel-side availability false vs unknown are distinct.
func TestDropsChannelAvailabilityFalseVsUnknown(t *testing.T) {
	e := Evaluator{Clock: fixedClock(testNow)}
	s := dropsStreamer(models.CapabilityEnabled, "g1")
	c, drop := activeCampaign("g1", s.ChannelID, false)

	if d := e.EvaluateDrops(s, c, drop, AvailabilityNo); d.Eligible || d.Reason != ReasonCampaignNotAvailable {
		t.Fatalf("authoritative not-available must block: %+v", d)
	}
	if d := e.EvaluateDrops(s, c, drop, AvailabilityUnknown); !d.Eligible {
		t.Fatalf("unknown availability must not block (distinct from false): %+v", d)
	}
}

func TestDropsGameMismatch(t *testing.T) {
	e := Evaluator{Clock: fixedClock(testNow)}
	s := dropsStreamer(models.CapabilityEnabled, "other-game")
	c, drop := activeCampaign("g1", s.ChannelID, false)
	if d := e.EvaluateDrops(s, c, drop, AvailabilityYes); d.Eligible || d.Reason != ReasonGameMismatch {
		t.Fatalf("game mismatch must block: %+v", d)
	}
}

func TestDropsExpiredEntitlement(t *testing.T) {
	e := Evaluator{Clock: fixedClock(testNow)}
	s := dropsStreamer(models.CapabilityEnabled, "g1")
	c, drop := activeCampaign("g1", s.ChannelID, false)
	c.StartAt = testNow.Add(-10 * time.Hour)
	c.EndAt = testNow.Add(-time.Hour)
	drop.StartAt = c.StartAt
	drop.EndAt = c.EndAt
	if d := e.EvaluateDrops(s, c, drop, AvailabilityYes); d.Eligible || d.Reason != ReasonEntitlementExpired {
		t.Fatalf("expired entitlement must block: %+v", d)
	}
}

// Deadline feasibility: impossible-before-deadline is deterministic, no margin.
func TestDropDeadlineFeasible(t *testing.T) {
	e := Evaluator{Clock: fixedClock(testNow)}

	// Needs 50 min, only 30 min of window left -> infeasible.
	drop := &models.Drop{MinutesRequired: 60, CurrentMinutesWatched: 10}
	win := models.EntitlementWindow{Start: testNow.Add(-time.Hour), End: testNow.Add(30 * time.Minute), Known: true, Source: models.WindowSourceDrop}
	if feasible, decided := e.DropDeadlineFeasible(drop, win); !decided || feasible {
		t.Fatalf("expected decided-infeasible, got feasible=%v decided=%v", feasible, decided)
	}

	// Needs 50 min, 2h left -> feasible.
	win2 := models.EntitlementWindow{Start: testNow.Add(-time.Hour), End: testNow.Add(2 * time.Hour), Known: true, Source: models.WindowSourceDrop}
	if feasible, decided := e.DropDeadlineFeasible(drop, win2); !decided || !feasible {
		t.Fatalf("expected decided-feasible, got feasible=%v decided=%v", feasible, decided)
	}

	// Unknown end -> not decided (never treated as impossible).
	if _, decided := e.DropDeadlineFeasible(drop, models.EntitlementWindow{}); decided {
		t.Fatal("unknown end must not be a decided deadline")
	}
}

func TestDropsImpossibleBeforeDeadline(t *testing.T) {
	e := Evaluator{Clock: fixedClock(testNow)}
	s := dropsStreamer(models.CapabilityEnabled, "g1")
	drop := &models.Drop{ID: "d1", Name: "Skin", MinutesRequired: 600, CurrentMinutesWatched: 0,
		StartAt: testNow.Add(-time.Hour), EndAt: testNow.Add(30 * time.Minute)}
	c := &models.Campaign{ID: "c", Game: &models.Game{ID: "g1"}, Drops: []*models.Drop{drop},
		StartAt: drop.StartAt, EndAt: drop.EndAt,
		ACL: models.CampaignACL{State: models.ACLUnrestricted, Complete: true, Source: models.ACLSourceCampaignDetails}}
	if d := e.EvaluateDrops(s, c, drop, AvailabilityYes); d.Eligible || d.Reason != ReasonImpossibleBeforeDeadline {
		t.Fatalf("impossible drop must block with impossible_before_deadline: %+v", d)
	}
}

// --- Slot candidacy (BKM-015 11,12,13,24,25) -----------------------------

func TestSlotCandidateEligible(t *testing.T) {
	e := Evaluator{Clock: fixedClock(testNow)}

	// 11: disabled points-only -> no slot.
	if ok, reason := e.SlotCandidateEligible(onlineStreamer(models.CapabilityDisabled), false); ok || reason != ReasonCapabilityDisabled {
		t.Errorf("disabled points-only should get no slot, got ok=%v reason=%v", ok, reason)
	}
	// 12: unknown points-only -> no new slot.
	if ok, reason := e.SlotCandidateEligible(onlineStreamer(models.CapabilityUnknown), false); ok || reason != ReasonCapabilityUnknown {
		t.Errorf("unknown points-only should get no new slot, got ok=%v reason=%v", ok, reason)
	}
	// 13: enabled points -> slot.
	if ok, _ := e.SlotCandidateEligible(onlineStreamer(models.CapabilityEnabled), false); !ok {
		t.Error("enabled points streamer should qualify for a slot")
	}
	// 24: a Drops-only candidate qualifies WITHOUT assuming points enabled.
	if ok, _ := e.SlotCandidateEligible(onlineStreamer(models.CapabilityUnknown), true); !ok {
		t.Error("active-drops candidate should qualify even with unknown points capability")
	}
	// disabled points but active drops -> still qualifies (drops independent).
	if ok, _ := e.SlotCandidateEligible(onlineStreamer(models.CapabilityDisabled), true); !ok {
		t.Error("disabled points + active drops should still qualify")
	}
	// DisableWatch -> never a slot candidate.
	s := onlineStreamer(models.CapabilityEnabled)
	st := s.GetSettings()
	st.DisableWatch = true
	s.SetSettings(st)
	if ok, reason := e.SlotCandidateEligible(s, true); ok || reason != ReasonWatchDisabled {
		t.Errorf("DisableWatch must veto slot candidacy, got ok=%v reason=%v", ok, reason)
	}
}

// 7 (cross-cutting): liveness online->unknown does not change capability/ACL and
// EvaluatePointsTask reports status_unknown (no new slot from unknown status).
func TestLivenessUnknownStatusReason(t *testing.T) {
	e := Evaluator{Clock: fixedClock(testNow)}
	s := onlineStreamer(models.CapabilityEnabled)
	s.SetUnknown(models.ReasonTransportError)
	d := e.EvaluatePointsTask(s, TaskBonusClaim)
	if d.State != StateUnknown || d.Reason != ReasonStatusUnknown {
		t.Fatalf("online->unknown should yield status_unknown: %+v", d)
	}
	if s.GetChannelPointsCapability() != models.CapabilityEnabled {
		t.Fatal("liveness unknown must leave capability unchanged")
	}
}

func TestOfflineStatusReason(t *testing.T) {
	e := Evaluator{Clock: fixedClock(testNow)}
	s := onlineStreamer(models.CapabilityEnabled)
	s.SetConfirmedOffline()
	if d := e.EvaluatePointsTask(s, TaskBonusClaim); d.Reason != ReasonStatusOffline {
		t.Fatalf("offline should yield status_offline: %+v", d)
	}
}
