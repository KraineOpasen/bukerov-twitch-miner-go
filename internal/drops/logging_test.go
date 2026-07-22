package drops

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// captureLogs redirects the default slog logger to an in-memory buffer at DEBUG
// level for the duration of fn, then restores the previous logger. It returns
// everything logged so a test can assert on levels and messages.
func captureLogs(t *testing.T, fn func()) string {
	t.Helper()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	fn()
	return buf.String()
}

func countINFO(logs string) int {
	return strings.Count(logs, "level=INFO")
}

// restrictedTracker wires a single online streamer to a single channel-restricted
// campaign whose current drop sits at the given watched/required minutes, so
// updateStreamerCampaigns assigns the campaign and can log its progress.
func restrictedTracker(watched, required int) *DropsTracker {
	game := &models.Game{ID: "game-wot", Name: "World of Tanks"}

	streamer := models.NewStreamer("cyganzor", models.StreamerSettings{ClaimDrops: true})
	streamer.ChannelID = "chan-1"
	streamer.SetConfirmedOnline()
	streamer.Stream.Game = game
	streamer.Stream.CampaignIDs = []string{"campaign-amd"}

	campaign := &models.Campaign{
		ID:          "campaign-amd",
		Name:        "AMD Summer Arena Drops#2",
		Game:        game,
		Channels:    []string{"chan-1"},
		ClaimStatus: models.CampaignClaimStatusInProgress,
		Drops: []*models.Drop{
			{ID: "drop-1", Name: "Garage Slot", MinutesRequired: required, CurrentMinutesWatched: watched},
		},
	}

	return &DropsTracker{
		streamers: []*models.Streamer{streamer},
		campaigns: []*models.Campaign{campaign},
	}
}

// setWatched advances the tracked campaign's current drop to a new watched-minute
// value, mimicking what a progress sync would fold in from the inventory.
func setWatched(d *DropsTracker, watched int) {
	d.campaigns[0].Drops[0].CurrentMinutesWatched = watched
}

// TestUpdateStreamerCampaignsThrottlesRepeatLogs is the regression guard: a
// second updateStreamerCampaigns pass that neither changes the assignment nor
// crosses a 5% progress checkpoint must not re-emit the INFO assignment or
// progress lines — they belong at DEBUG instead. This is what stopped the
// per-2-minute progress-sync spam.
func TestUpdateStreamerCampaignsThrottlesRepeatLogs(t *testing.T) {
	d := restrictedTracker(55, 100) // 55%, checkpoint bucket 11

	first := captureLogs(t, d.updateStreamerCampaigns)
	// First pass: the assignment announcement plus the progress line, both INFO.
	if got := countINFO(first); got != 2 {
		t.Fatalf("expected 2 INFO lines on first pass (assignment + progress), got %d:\n%s", got, first)
	}
	if !strings.Contains(first, "Channel-restricted drop campaign assigned to streamer") {
		t.Errorf("expected the assignment announcement on the first pass:\n%s", first)
	}
	if !strings.Contains(first, "[cyganzor] AMD Summer Arena Drops#2") {
		t.Errorf("expected a compact progress line naming the streamer and campaign:\n%s", first)
	}
	// The long allowed-channel list must stay off the INFO announcement.
	if strings.Contains(firstINFOLines(first), "allowedChannels") {
		t.Errorf("allowedChannels must not appear on an INFO line:\n%s", first)
	}

	second := captureLogs(t, d.updateStreamerCampaigns)
	// Nothing changed: no INFO at all, but the events are still logged at DEBUG.
	if got := countINFO(second); got != 0 {
		t.Fatalf("expected no repeat INFO lines on an unchanged pass, got %d:\n%s", got, second)
	}
	if !strings.Contains(second, "level=DEBUG") {
		t.Errorf("expected the unchanged events to be logged at DEBUG, not dropped:\n%s", second)
	}
}

// TestUpdateStreamerCampaignsLogsOnCheckpointCross verifies the throttle still
// lets a genuine 5% progress advance through at INFO, while the (unchanged)
// assignment announcement stays quiet.
func TestUpdateStreamerCampaignsLogsOnCheckpointCross(t *testing.T) {
	d := restrictedTracker(55, 100) // bucket 11
	_ = captureLogs(t, d.updateStreamerCampaigns)

	// Advance past the next 5% checkpoint (60% -> bucket 12).
	setWatched(d, 60)
	logs := captureLogs(t, d.updateStreamerCampaigns)

	if got := countINFO(logs); got != 1 {
		t.Fatalf("expected exactly 1 INFO line (progress crossed a checkpoint), got %d:\n%s", got, logs)
	}
	if !strings.Contains(logs, "60%") {
		t.Errorf("expected the crossed progress line at 60%%:\n%s", logs)
	}
	if strings.Contains(firstINFOLines(logs), "assigned to streamer") {
		t.Errorf("the unchanged assignment must not re-log at INFO:\n%s", logs)
	}
}

// TestUpdateStreamerCampaignsLogsCompletion checks the required 100% / claim-ready
// checkpoint is always logged at INFO even when it is reached in a single jump.
func TestUpdateStreamerCampaignsLogsCompletion(t *testing.T) {
	d := restrictedTracker(55, 100)
	_ = captureLogs(t, d.updateStreamerCampaigns)

	setWatched(d, 100) // 100%
	logs := captureLogs(t, d.updateStreamerCampaigns)

	if !strings.Contains(logs, "level=INFO") || !strings.Contains(logs, "100%") {
		t.Fatalf("expected the 100%% progress line at INFO, got:\n%s", logs)
	}
}

// firstINFOLines returns only the INFO lines of a captured log blob, so a test
// can assert on what did (or did not) reach INFO without matching DEBUG lines
// that legitimately carry the same text.
func firstINFOLines(logs string) string {
	var b strings.Builder
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "level=INFO") {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return b.String()
}
