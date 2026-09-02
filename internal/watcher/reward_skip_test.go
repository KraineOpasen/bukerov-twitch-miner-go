package watcher

// Regression tests for the watch-side fail-safe: slot admission applies the
// operator's farming exclusions to the assigned campaigns itself, so even an
// unfiltered upstream assignment never turns a Skip-ruled reward into the
// sole justification for a watch slot — while independently-justified
// watching (confirmed points capability, other wanted campaigns) continues
// exactly as the repository's blacklist/ClaimDrops contracts already promise
// (the minute-watched beacon carries no per-campaign selector, so incidental
// server-side progress is protocol-unavoidable and out of scope here).

import (
	"context"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func skippedRewardCampaign() *models.Campaign {
	return &models.Campaign{
		ID:          "camp-skip",
		Name:        "Skipped Campaign",
		Game:        &models.Game{ID: "game-skip"},
		ClaimStatus: models.CampaignClaimStatusInProgress,
		Drops: []*models.Drop{{
			ID: "d1", Name: "Skipped Reward", MinutesRequired: 60, CurrentMinutesWatched: 10,
		}},
	}
}

func newSkipRuleWatcher(s *models.Streamer, sender minuteReporter) *MinuteWatcher {
	return &MinuteWatcher{
		client:     &staticChecker{checked: make(chan string, 16)},
		streamers:  []*models.Streamer{s},
		priorities: []config.Priority{config.PriorityOrder},
		settings:   config.RateLimitSettings{MinuteWatchedInterval: 1},
		sender:     sender,
		pacer:      func(time.Duration) bool { return true },
	}
}

func drainSent(sender *countingSender) []string {
	close(sender.sent)
	var sent []string
	for name := range sender.sent {
		sent = append(sent, name)
	}
	return sent
}

// Fail-safe: a channel whose ONLY justification is an assigned campaign with
// a Skip-ruled current drop (points capability unknown) earns no slot and
// never reaches the send boundary — even though the assignment writer did not
// pre-filter. This is the exact base-defect scenario inverted.
func TestSlotAdmissionRefusesSkippedOnlyDropJustification(t *testing.T) {
	s := models.NewStreamer("skiponly", models.StreamerSettings{ClaimDrops: true})
	s.SetConfirmedOnline()
	s.OnlineAt = time.Now().Add(-time.Minute)
	s.Stream.SetCampaigns([]*models.Campaign{skippedRewardCampaign()})

	sender := &countingSender{sent: make(chan string, 16)}
	w := newSkipRuleWatcher(s, sender)
	w.SetRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("game-skip", "Skipped Reward"),
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.ctx = ctx
	w.processWatching(tickCtx(w))

	if sent := drainSent(sender); len(sent) != 0 {
		t.Fatalf("skipped-only drop justification must not reach the send boundary, got %v", sent)
	}
}

// Mixed-conflict contract (repository resolution: intent suppression, not
// watch prohibition): a points-confirmed channel keeps its independently
// justified slot and heartbeat even while carrying a skipped assignment.
func TestSlotAdmissionKeepsPointsJustifiedChannel(t *testing.T) {
	s := models.NewStreamer("pointschan", models.StreamerSettings{ClaimDrops: true})
	s.SetConfirmedOnline()
	s.OnlineAt = time.Now().Add(-time.Minute)
	s.SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)
	s.Stream.SetCampaigns([]*models.Campaign{skippedRewardCampaign()})

	sender := &countingSender{sent: make(chan string, 16)}
	w := newSkipRuleWatcher(s, sender)
	w.SetRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("game-skip", "Skipped Reward"),
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.ctx = ctx
	w.processWatching(tickCtx(w))

	if sent := drainSent(sender); len(sent) != 1 || sent[0] != "pointschan" {
		t.Fatalf("points-justified watching must continue (mixed-conflict contract), got %v", sent)
	}
}

// Control: with the skip set published, an UNSKIPPED drop-only justification
// still earns its slot — the fail-safe suppresses exactly the skipped reward,
// nothing else.
func TestSlotAdmissionKeepsUnskippedDropJustification(t *testing.T) {
	s := models.NewStreamer("dropchan", models.StreamerSettings{ClaimDrops: true})
	s.SetConfirmedOnline()
	s.OnlineAt = time.Now().Add(-time.Minute)
	wanted := skippedRewardCampaign()
	wanted.ID = "camp-want"
	wanted.Drops[0].Name = "Wanted Reward"
	s.Stream.SetCampaigns([]*models.Campaign{wanted})

	sender := &countingSender{sent: make(chan string, 16)}
	w := newSkipRuleWatcher(s, sender)
	w.SetRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("game-skip", "Skipped Reward"),
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.ctx = ctx
	w.processWatching(tickCtx(w))

	if sent := drainSent(sender); len(sent) != 1 || sent[0] != "dropchan" {
		t.Fatalf("unskipped drop justification must keep its slot, got %v", sent)
	}
}

// Publishing the decision concurrently with broker ticks is race-free (the
// watcher stores an immutable snapshot atomically) and converges: once the
// rule is published, the drop-only channel stops being sent.
func TestSetRewardSkipsConcurrentWithBrokerTicks(t *testing.T) {
	s := models.NewStreamer("skiponly", models.StreamerSettings{ClaimDrops: true})
	s.SetConfirmedOnline()
	s.OnlineAt = time.Now().Add(-time.Minute)
	s.Stream.SetCampaigns([]*models.Campaign{skippedRewardCampaign()})

	sender := &countingSender{sent: make(chan string, 256)}
	w := newSkipRuleWatcher(s, sender)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.ctx = ctx

	skips := models.NewRewardSkips([]string{
		models.NormalizeRewardKey("game-skip", "Skipped Reward"),
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			w.SetRewardSkips(skips)
		}
	}()
	for i := 0; i < 20; i++ {
		w.processWatching(tickCtx(w))
	}
	<-done

	// Deterministic post-condition: with the rule published, a tick sends
	// nothing for the skipped-only channel.
	for len(sender.sent) > 0 {
		<-sender.sent
	}
	w.processWatching(tickCtx(w))
	if got := len(sender.sent); got != 0 {
		t.Fatalf("after publication the skipped-only channel must not be sent, got %d sends", got)
	}
}

// A wanted sibling campaign on the same channel keeps the drop justification
// (intra-channel mixed case): the slot survives because of the wanted reward.
func TestSlotAdmissionKeepsSiblingWantedCampaign(t *testing.T) {
	s := models.NewStreamer("mixedchan", models.StreamerSettings{ClaimDrops: true})
	s.SetConfirmedOnline()
	s.OnlineAt = time.Now().Add(-time.Minute)
	wanted := skippedRewardCampaign()
	wanted.ID = "camp-want"
	wanted.Drops[0].Name = "Wanted Reward"
	s.Stream.SetCampaigns([]*models.Campaign{skippedRewardCampaign(), wanted})

	sender := &countingSender{sent: make(chan string, 16)}
	w := newSkipRuleWatcher(s, sender)
	w.SetRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("game-skip", "Skipped Reward"),
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.ctx = ctx
	w.processWatching(tickCtx(w))

	if sent := drainSent(sender); len(sent) != 1 || sent[0] != "mixedchan" {
		t.Fatalf("the wanted sibling must keep the drop-justified slot, got %v", sent)
	}
}
