package miner

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// TestDropRulesSnapshotRaceFreeAgainstSetDropRule is the regression for the
// concurrent-map read/write on m.config.DropRules: refreshPolicy hands the
// per-drop rules to buildPolicyInputs, which reads them lock-free, while
// SetDropRule mutates the same map under the write lock from another goroutine.
//
// The reader here drives the real fixed path — snapshotDropRules() then
// buildPolicyInputs() — so if snapshotDropRules returns the shared map reference
// instead of a copy (the pre-fix behavior), buildPolicyInputs reads the shared
// map lock-free while SetDropRule writes it, which `go test -race` reports as a
// data race (and the runtime as "concurrent map read and map write"). With the
// fix (a private copy) the reader never touches the shared map.
//
// buildPolicyInputs needs no DropsTracker (campaigns are passed in) and is
// nil-safe with a zero-value watcher and nil streamers/discovery.
func TestDropRulesSnapshotRaceFreeAgainstSetDropRule(t *testing.T) {
	m := &Miner{
		config:  &config.Config{DropRules: map[string]config.DropRule{"seed": {Skip: true}}},
		watcher: &watcher.MinuteWatcher{}, // streamers / discovery / dropsTracker stay nil
	}

	// A campaign with an unclaimed current drop, so buildPolicyInputs reaches the
	// rules[...] lookup.
	campaigns := []*models.Campaign{{
		ID:    "c1",
		Name:  "Campaign One",
		Game:  &models.Game{ID: "g1", Name: "Game One"},
		Drops: []*models.Drop{{Name: "reward", MinutesRequired: 60}},
	}}
	games := []string{"Game One"}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: mutate m.config.DropRules under the write lock. dropsTracker is nil,
	// so SetDropRule's trailing refreshPolicy returns immediately (no-op).
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				m.SetDropRule(fmt.Sprintf("k%d", i%16), config.DropRule{HighPriority: true})
			}
		}
	}()

	// Reader: the real refreshPolicy handoff — snapshot the rules, then read them
	// in buildPolicyInputs exactly as refreshPolicy does.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				rules := m.snapshotDropRules()
				_ = m.buildPolicyInputs(campaigns, rules, games, time.Now())
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}
