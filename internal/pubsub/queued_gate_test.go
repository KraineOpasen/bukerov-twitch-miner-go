package pubsub

import (
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// fakeChannelActor records every per-channel Twitch mutation the handlers
// attempt, so the queued-event gates can assert both that a disabled setting
// blocks the action and that an enabled one still performs it.
type fakeChannelActor struct {
	mu       sync.Mutex
	raids    int
	moments  int
	goals    int
	bonuses  int
	lastGoal int
}

func (a *fakeChannelActor) ClaimBonus(*models.Streamer, string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bonuses++
	return nil
}

func (a *fakeChannelActor) JoinRaid(*models.Streamer, *models.Raid) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.raids++
	return nil
}

func (a *fakeChannelActor) ClaimMoment(*models.Streamer, string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.moments++
	return nil
}

func (a *fakeChannelActor) ContributeToCommunityGoal(_ *models.Streamer, _, _ string, amount int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.goals++
	a.lastGoal = amount
	return nil
}

func (a *fakeChannelActor) counts() (raids, moments, goals, bonuses int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.raids, a.moments, a.goals, a.bonuses
}

func setStreamerFlag(s *models.Streamer, mutate func(*models.StreamerSettings)) {
	next := s.GetSettings()
	mutate(&next)
	s.SetSettings(next)
}

func raidMsg() *PubSubMessage {
	return &PubSubMessage{
		Type: "raid_update_v2",
		Message: map[string]interface{}{
			"raid": map[string]interface{}{
				"id":           "raid-1",
				"target_login": "target",
			},
		},
	}
}

func momentMsg() *PubSubMessage {
	return &PubSubMessage{
		Type: "active",
		Data: map[string]interface{}{"moment_id": "moment-1"},
	}
}

func goalMsg(msgType string) *PubSubMessage {
	return &PubSubMessage{
		Type: msgType,
		Data: map[string]interface{}{
			"community_goal": map[string]interface{}{
				"id":          "goal-1",
				"title":       "Goal",
				"status":      "STARTED",
				"is_in_stock": true,
				"goal_amount": float64(1000),
			},
		},
	}
}

// TestQueuedRaidGateReChecksCurrentSettings: a raid frame that was queued
// before FollowRaid was switched off must not join the raid; with the flag on
// the same frame does.
func TestQueuedRaidGateReChecksCurrentSettings(t *testing.T) {
	actor := &fakeChannelActor{}
	pool := &WebSocketPool{actor: actor}
	s := newTestStreamer(1000)

	setStreamerFlag(s, func(st *models.StreamerSettings) { st.FollowRaid = false })
	pool.handleRaid(raidMsg(), s)
	if raids, _, _, _ := actor.counts(); raids != 0 {
		t.Fatalf("raid joined despite FollowRaid=false: %d calls", raids)
	}

	setStreamerFlag(s, func(st *models.StreamerSettings) { st.FollowRaid = true })
	pool.handleRaid(raidMsg(), s)
	if raids, _, _, _ := actor.counts(); raids != 1 {
		t.Fatalf("raid not joined with FollowRaid=true: %d calls, want 1", raids)
	}
}

// TestQueuedMomentGateReChecksCurrentSettings mirrors the raid gate for
// ClaimMoments.
func TestQueuedMomentGateReChecksCurrentSettings(t *testing.T) {
	actor := &fakeChannelActor{}
	pool := &WebSocketPool{actor: actor}
	s := newTestStreamer(1000)

	setStreamerFlag(s, func(st *models.StreamerSettings) { st.ClaimMoments = false })
	pool.handleMoment(momentMsg(), s)
	if _, moments, _, _ := actor.counts(); moments != 0 {
		t.Fatalf("moment claimed despite ClaimMoments=false: %d calls", moments)
	}

	setStreamerFlag(s, func(st *models.StreamerSettings) { st.ClaimMoments = true })
	pool.handleMoment(momentMsg(), s)
	if _, moments, _, _ := actor.counts(); moments != 1 {
		t.Fatalf("moment not claimed with ClaimMoments=true: %d calls, want 1", moments)
	}
}

// TestQueuedPredictionEventGateReChecksCurrentSettings: a queued event-created
// frame delivered after MakePredictions was switched off must not schedule an
// auto-bet round.
func TestQueuedPredictionEventGateReChecksCurrentSettings(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newTestStreamer(1000)

	setStreamerFlag(s, func(st *models.StreamerSettings) { st.MakePredictions = false })
	pool.handlePredictionChannel(eventCreatedMsg("e-disabled"), s)

	pool.mu.RLock()
	_, tracked := pool.predictions["e-disabled"]
	pool.mu.RUnlock()
	if tracked {
		t.Fatal("prediction round scheduled despite MakePredictions=false")
	}

	setStreamerFlag(s, func(st *models.StreamerSettings) { st.MakePredictions = true })
	pool.handlePredictionChannel(eventCreatedMsg("e-enabled"), s)

	pool.mu.RLock()
	_, tracked = pool.predictions["e-enabled"]
	pool.mu.RUnlock()
	if !tracked {
		t.Fatal("prediction round not scheduled with MakePredictions=true")
	}
}

// TestQueuedCommunityGoalGateReChecksCurrentSettings: a queued goal frame
// delivered after CommunityGoals was switched off must not contribute points.
func TestQueuedCommunityGoalGateReChecksCurrentSettings(t *testing.T) {
	actor := &fakeChannelActor{}
	pool := &WebSocketPool{actor: actor}
	s := newTestStreamer(5000)

	setStreamerFlag(s, func(st *models.StreamerSettings) {
		st.CommunityGoals = false
		st.CommunityGoalsMaxPercent = 0
		st.CommunityGoalsMaxAmount = 0
	})
	pool.handleCommunityPointsChannel(goalMsg("community-goal-updated"), s)
	if _, _, goals, _ := actor.counts(); goals != 0 {
		t.Fatalf("goal contribution despite CommunityGoals=false: %d calls", goals)
	}

	setStreamerFlag(s, func(st *models.StreamerSettings) { st.CommunityGoals = true })
	pool.handleCommunityPointsChannel(goalMsg("community-goal-updated"), s)
	if _, _, goals, _ := actor.counts(); goals != 1 {
		t.Fatalf("goal contribution missing with CommunityGoals=true: %d calls, want 1", goals)
	}
}
