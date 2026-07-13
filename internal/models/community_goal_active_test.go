package models

import "testing"

func TestActiveCommunityGoals(t *testing.T) {
	s := NewStreamer("streamer", DefaultStreamerSettings())

	// In-progress, in-stock goal at 25%.
	s.AddCommunityGoal(&CommunityGoal{GoalID: "g1", Title: "Emote", Status: CommunityGoalStarted, IsInStock: true, PointsContributed: 250, GoalAmount: 1000})
	// Further-along goal at 90% (should sort first).
	s.AddCommunityGoal(&CommunityGoal{GoalID: "g2", Title: "Badge", Status: CommunityGoalStarted, IsInStock: true, PointsContributed: 900, GoalAmount: 1000})
	// Ended goal — must be excluded.
	s.AddCommunityGoal(&CommunityGoal{GoalID: "g3", Title: "Old", Status: CommunityGoalEnded, IsInStock: true, PointsContributed: 500, GoalAmount: 1000})
	// Out of stock — must be excluded.
	s.AddCommunityGoal(&CommunityGoal{GoalID: "g4", Title: "Gone", Status: CommunityGoalStarted, IsInStock: false, PointsContributed: 500, GoalAmount: 1000})

	goals := s.ActiveCommunityGoals()
	if len(goals) != 2 {
		t.Fatalf("expected 2 active goals, got %d", len(goals))
	}
	if goals[0].GoalID != "g2" || goals[0].Percent != 90 {
		t.Errorf("expected g2 at 90%% first, got %+v", goals[0])
	}
	if goals[1].GoalID != "g1" || goals[1].Percent != 25 {
		t.Errorf("expected g1 at 25%% second, got %+v", goals[1])
	}
}

func TestActiveCommunityGoalsNone(t *testing.T) {
	s := NewStreamer("streamer", DefaultStreamerSettings())
	if goals := s.ActiveCommunityGoals(); goals != nil {
		t.Errorf("expected nil for no goals, got %v", goals)
	}
}
