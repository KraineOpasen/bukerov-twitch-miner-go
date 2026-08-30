package miner

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

type minerRoutineRefreshRunner struct {
	allow bool
	calls int
}

func (r *minerRoutineRefreshRunner) RunRoutineRefresh(_ *models.Streamer, refresh func()) bool {
	r.calls++
	if !r.allow {
		return false
	}
	refresh()
	return true
}

func TestMinerRoutineRefreshPermitDeniesThenResumes(t *testing.T) {
	runner := &minerRoutineRefreshRunner{}
	miner := &Miner{routineRefresh: runner}
	streamer := models.NewStreamer("miner-permit", models.DefaultStreamerSettings())
	refreshCalls := 0

	if miner.runRoutineRefresh(streamer, func() { refreshCalls++ }) {
		t.Fatal("owned refresh reported execution")
	}
	if refreshCalls != 0 || runner.calls != 1 {
		t.Fatalf("denied refresh calls=%d runner calls=%d, want 0/1", refreshCalls, runner.calls)
	}

	runner.allow = true
	if !miner.runRoutineRefresh(streamer, func() { refreshCalls++ }) {
		t.Fatal("released refresh was denied")
	}
	if refreshCalls != 1 || runner.calls != 2 {
		t.Fatalf("allowed refresh calls=%d runner calls=%d, want 1/2", refreshCalls, runner.calls)
	}
}
