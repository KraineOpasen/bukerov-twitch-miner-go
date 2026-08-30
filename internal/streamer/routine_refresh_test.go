package streamer

import (
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

type routineRefreshClient struct {
	mu      sync.Mutex
	manager *Manager
	calls   []*models.Streamer
}

func (*routineRefreshClient) GetChannelID(username string) (string, error) {
	return "chan-" + username, nil
}

func (*routineRefreshClient) LoadChannelPointsContext(*models.Streamer) error { return nil }

func (c *routineRefreshClient) CheckStreamerOnline(streamer *models.Streamer) models.StatusTransition {
	// CheckOnlineStatus must release Manager.mu before entering network I/O.
	// TryLock makes that ordering assertion deterministic without a timeout.
	if !c.manager.mu.TryLock() {
		panic("Manager.mu held across CheckStreamerOnline")
	}
	c.manager.mu.Unlock()

	c.mu.Lock()
	c.calls = append(c.calls, streamer)
	c.mu.Unlock()
	return models.StatusTransition{}
}

func (c *routineRefreshClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

type managerRoutineRefreshRunner struct {
	manager *Manager
	allow   bool
	calls   []*models.Streamer
}

func (r *managerRoutineRefreshRunner) RunRoutineRefresh(streamer *models.Streamer, refresh func()) bool {
	// The roster and runner must be snapshotted before Manager.mu is released;
	// the coordinator itself never runs under that manager lock.
	if !r.manager.mu.TryLock() {
		panic("Manager.mu held across RunRoutineRefresh")
	}
	r.manager.mu.Unlock()

	r.calls = append(r.calls, streamer)
	if !r.allow {
		return false
	}
	refresh()
	return true
}

func TestCheckOnlineStatusUsesRoutineRefreshPermitAndResumes(t *testing.T) {
	client := &routineRefreshClient{}
	manager := NewManager(client, models.DefaultStreamerSettings())
	client.manager = manager
	if err := manager.LoadFromConfig([]config.StreamerConfig{{Username: "alpha"}}, nil); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	streamer := manager.Get("alpha")
	if streamer == nil {
		t.Fatal("setup: alpha not tracked")
	}

	runner := &managerRoutineRefreshRunner{manager: manager}
	manager.SetRoutineRefreshRunner(runner)

	manager.CheckOnlineStatus()
	if got := client.callCount(); got != 0 {
		t.Fatalf("owned/denied refresh reached client %d times, want 0", got)
	}
	if len(runner.calls) != 1 || runner.calls[0] != streamer {
		t.Fatalf("runner calls = %v, want exact tracked alpha pointer", runner.calls)
	}

	runner.allow = true
	manager.CheckOnlineStatus()
	if got := client.callCount(); got != 1 {
		t.Fatalf("released/unowned refresh reached client %d times, want 1", got)
	}
	if len(runner.calls) != 2 || runner.calls[1] != streamer {
		t.Fatalf("second runner call did not use exact tracked alpha pointer: %v", runner.calls)
	}
}

func TestCheckOnlineStatusStandaloneWithoutBrokerStillRefreshes(t *testing.T) {
	client := &routineRefreshClient{}
	manager := NewManager(client, models.DefaultStreamerSettings())
	client.manager = manager
	if err := manager.LoadFromConfig([]config.StreamerConfig{{Username: "standalone"}}, nil); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}

	manager.CheckOnlineStatus()
	if got := client.callCount(); got != 1 {
		t.Fatalf("standalone refresh calls = %d, want 1", got)
	}
}
