package models

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHasChannelRestrictedCampaign(t *testing.T) {
	s := NewStreamer("teststreamer", DefaultStreamerSettings())

	if s.HasChannelRestrictedCampaign() {
		t.Error("streamer with no campaigns should not have a channel-restricted campaign")
	}

	s.Stream.Campaigns = []*Campaign{{ID: "unrestricted"}}
	if s.HasChannelRestrictedCampaign() {
		t.Error("streamer with only unrestricted campaigns should not report a channel-restricted campaign")
	}

	s.Stream.Campaigns = append(s.Stream.Campaigns, &Campaign{ID: "restricted", Channels: []string{"channel-1"}})
	if !s.HasChannelRestrictedCampaign() {
		t.Error("streamer with a channel-restricted campaign should report it")
	}
}

func TestActiveCampaignProgressPicksFurthestAlong(t *testing.T) {
	s := NewStreamer("teststreamer", DefaultStreamerSettings())

	if s.ActiveCampaignProgress() != nil {
		t.Error("streamer with no campaigns should report no active campaign progress")
	}

	s.Stream.Campaigns = []*Campaign{
		{
			Name: "Behind",
			Game: &Game{Name: "Game A"},
			Drops: []*Drop{
				{Name: "Reward A", MinutesRequired: 100, CurrentMinutesWatched: 10},
			},
		},
		{
			Name: "Ahead",
			Game: &Game{Name: "Game B"},
			Drops: []*Drop{
				{Name: "Reward B", MinutesRequired: 100, CurrentMinutesWatched: 80},
			},
		},
	}

	prog := s.ActiveCampaignProgress()
	if prog == nil {
		t.Fatal("expected active campaign progress")
	}
	if prog.CampaignName != "Ahead" {
		t.Errorf("expected the furthest-along campaign (Ahead), got %q", prog.CampaignName)
	}
	if prog.Percent != 80 {
		t.Errorf("expected 80%% progress, got %d", prog.Percent)
	}
	if prog.DropName != "Reward B" || prog.MinutesRequired != 100 {
		t.Errorf("unexpected drop details: %+v", prog)
	}
}

// TestSetOnlineGracePreservesStreakOnBlip is the grace case (B): a short offline
// blip (an online-detection flap) within watchStreakContinuityGrace must NOT
// reset watch-streak progress — it is the same continuous broadcast.
func TestSetOnlineGracePreservesStreakOnBlip(t *testing.T) {
	s := NewStreamer("blipper", DefaultStreamerSettings())

	if !s.SetOnline() {
		t.Fatal("first SetOnline should report an offline→online transition")
	}
	// Progress banked on this broadcast: 5 watched minutes, streak already earned.
	s.Stream.MinuteWatched = 5
	s.Stream.WatchStreakMissing = false

	// Brief flap: offline then immediately back online (OfflineAt ≈ now).
	s.SetOffline()
	if !s.SetOnline() {
		t.Fatal("re-online after a blip is still a real transition")
	}

	if got := s.Stream.GetMinuteWatched(); got != 5 {
		t.Errorf("blip within grace must preserve MinuteWatched, got %v (want 5)", got)
	}
	if s.Stream.GetWatchStreakMissing() {
		t.Error("blip within grace must preserve the earned streak (WatchStreakMissing stays false)")
	}
}

// TestSetOnlineReArmsStreakOnNewBroadcast is the non-grace case (B): after a
// genuinely long offline gap (a real new broadcast) the streak re-arms exactly
// as before — MinuteWatched back to 0 and the streak marked missing.
func TestSetOnlineReArmsStreakOnNewBroadcast(t *testing.T) {
	s := NewStreamer("returner", DefaultStreamerSettings())

	if !s.SetOnline() {
		t.Fatal("first SetOnline should report a transition")
	}
	s.Stream.MinuteWatched = 6
	s.Stream.WatchStreakMissing = false

	s.SetOffline()
	// Simulate a new broadcast: offline well beyond the grace window.
	s.OfflineAt = time.Now().Add(-watchStreakContinuityGrace - time.Minute)

	if !s.SetOnline() {
		t.Fatal("re-online should report a transition")
	}
	if got := s.Stream.GetMinuteWatched(); got != 0 {
		t.Errorf("a new broadcast must re-arm the streak (MinuteWatched=0), got %v", got)
	}
	if !s.Stream.GetWatchStreakMissing() {
		t.Error("a new broadcast must re-arm the streak (WatchStreakMissing=true)")
	}
}

// TestSetOnlineReportsTransitionOnlyOnce is the log-dedup case (A): many
// concurrent online detections racing on the same offline streamer must yield
// exactly ONE reported transition, so the "Streamer is online" log fires once.
// Run under -race.
func TestSetOnlineReportsTransitionOnlyOnce(t *testing.T) {
	s := NewStreamer("racer", DefaultStreamerSettings()) // starts offline

	const n = 32
	var transitions atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if s.SetOnline() {
				transitions.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := transitions.Load(); got != 1 {
		t.Fatalf("concurrent SetOnline must report exactly one transition (log fires once), got %d", got)
	}
	if !s.GetIsOnline() {
		t.Error("streamer must be online after the race")
	}
}
