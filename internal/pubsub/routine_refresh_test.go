package pubsub

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

type poolRoutineRefreshRunner struct {
	allow bool
	calls []*models.Streamer
}

func (r *poolRoutineRefreshRunner) RunRoutineRefresh(streamer *models.Streamer, refresh func()) bool {
	r.calls = append(r.calls, streamer)
	if !r.allow {
		return false
	}
	refresh()
	return true
}

func TestViewcountUsesRoutineRefreshPermitAndResumes(t *testing.T) {
	checker := &fakeChecker{checkApplies: true}
	pool, statusEvents := newStatusTestPool(checker)
	runner := &poolRoutineRefreshRunner{}
	pool.SetRoutineRefreshRunner(runner)
	streamer := models.NewStreamer("permit-viewcount", models.DefaultStreamerSettings())

	pool.handleVideoPlayback(&PubSubMessage{Type: "viewcount"}, streamer)
	if checker.checkCalls != 0 {
		t.Fatalf("owned/denied viewcount reached checker %d times, want 0", checker.checkCalls)
	}
	if streamer.GetStatus() == models.StatusOnline || len(*statusEvents) != 0 {
		t.Fatalf("denied viewcount mutated status=%v events=%v", streamer.GetStatus(), *statusEvents)
	}

	runner.allow = true
	pool.handleVideoPlayback(&PubSubMessage{Type: "viewcount"}, streamer)
	if checker.checkCalls != 1 {
		t.Fatalf("released/unowned viewcount reached checker %d times, want 1", checker.checkCalls)
	}
	if len(runner.calls) != 2 || runner.calls[0] != streamer || runner.calls[1] != streamer {
		t.Fatalf("runner did not receive exact streamer twice: %v", runner.calls)
	}
	if streamer.GetStatus() != models.StatusOnline || len(*statusEvents) != 1 {
		t.Fatalf("allowed viewcount status=%v events=%v, want online + one event", streamer.GetStatus(), *statusEvents)
	}
}

func TestStreamUpLifecycleEvidenceBypassesRoutineRefreshPermit(t *testing.T) {
	checker := &fakeChecker{}
	pool, statusEvents := newStatusTestPool(checker)
	runner := &poolRoutineRefreshRunner{}
	pool.SetRoutineRefreshRunner(runner)
	streamer := models.NewStreamer("lifecycle-stream-up", models.DefaultStreamerSettings())

	pool.handleVideoPlayback(&PubSubMessage{Type: "stream-up"}, streamer)

	if len(runner.calls) != 0 {
		t.Fatalf("stream-up consulted routine permit %d times, want 0", len(runner.calls))
	}
	if checker.updateCalls != 1 || streamer.GetStatus() != models.StatusOnline || len(*statusEvents) != 1 {
		t.Fatalf("stream-up lifecycle path updateCalls=%d status=%v events=%v", checker.updateCalls, streamer.GetStatus(), *statusEvents)
	}
}

func TestStreamDownLifecycleEvidenceBypassesRoutineRefreshPermit(t *testing.T) {
	checker := &fakeChecker{}
	pool, statusEvents := newStatusTestPool(checker)
	runner := &poolRoutineRefreshRunner{}
	pool.SetRoutineRefreshRunner(runner)
	streamer := models.NewStreamer("lifecycle-stream-down", models.DefaultStreamerSettings())
	streamer.SetConfirmedOnline()

	pool.handleVideoPlayback(&PubSubMessage{Type: "stream-down"}, streamer)

	if len(runner.calls) != 0 {
		t.Fatalf("stream-down consulted routine permit %d times, want 0", len(runner.calls))
	}
	if checker.checkCalls != 0 || checker.updateCalls != 0 ||
		streamer.GetStatus() != models.StatusOffline || len(*statusEvents) != 1 ||
		(*statusEvents)[0].status != models.StatusOffline {
		t.Fatalf(
			"stream-down lifecycle path checkCalls=%d updateCalls=%d status=%v events=%v",
			checker.checkCalls, checker.updateCalls, streamer.GetStatus(), *statusEvents,
		)
	}
}
