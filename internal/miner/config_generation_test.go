package miner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
)

type configGenerationRunner struct {
	miner   *Miner
	started chan struct{}
}

type configGenerationStatusSink struct {
	running chan<- struct{}
}

func (s configGenerationStatusSink) SetStatus(status, _ string) {
	if status == string(lifecycle.ObservedRunning) {
		s.running <- struct{}{}
	}
}

func (configGenerationStatusSink) SetGeneration(uint64) {}

func (r *configGenerationRunner) Run(ctx context.Context) error {
	r.miner.runCtx = ctx
	close(r.started)
	<-ctx.Done()
	return r.miner.stop()
}

func newConfigGenerationMiner(t *testing.T, cfg *config.Config, configPath string) *Miner {
	t.Helper()
	m := New(cfg, configPath)
	mgr := streamer.NewManager(fakeStreamerAPI{}, cfg.StreamerSettings)
	if added, _, _, _ := mgr.ApplySettings(cfg.Streamers, cfg.StreamerSettings); len(added) != len(cfg.Streamers) {
		t.Fatalf("seed generation roster: added=%d want=%d", len(added), len(cfg.Streamers))
	}
	m.streamers = mgr
	return m
}

func cloneConfigForBlockedSave(cfg *config.Config) (*config.Config, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var snapshot config.Config
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func waitTestChannel(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func TestConfigWritesQuiesceBeforeNextGenerationDropRuleCommit(t *testing.T) {
	tests := []struct {
		name   string
		write  func(*Miner) error
		verify func(*testing.T, *config.Config)
	}{
		{
			name: "campaign policy",
			write: func(m *Miner) error {
				m.ApplyCampaignPolicy(string(policy.ModeSmart))
				return nil
			},
			verify: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.CampaignPolicy != string(policy.ModeSmart) {
					t.Fatalf("campaign policy handoff = %q, want %q", cfg.CampaignPolicy, policy.ModeSmart)
				}
			},
		},
		{
			name: "health settings",
			write: func(m *Miner) error {
				settings := config.DefaultHealthSettings()
				settings.CanaryEnabled = !settings.CanaryEnabled
				m.ApplyHealthSettings(settings)
				return nil
			},
			verify: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if !cfg.Health.CanaryEnabled {
					t.Fatal("health settings commit was lost at generation handoff")
				}
			},
		},
		{
			name: "auto redeem",
			write: func(m *Miner) error {
				return m.SetAutoRedeem("generation-streamer", config.AutoRedeemConfig{
					Enabled: true,
					Budget:  100,
					RewardIDs: []string{
						"reward-1",
					},
				})
			},
			verify: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				got := cfg.AutoRedeem["generation-streamer"]
				if !got.Enabled || got.Budget != 100 || len(got.RewardIDs) != 1 || got.RewardIDs[0] != "reward-1" {
					t.Fatalf("auto-redeem handoff = %+v, want committed rule", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.json")
			oldCfg := config.DefaultConfig()
			oldCfg.Username = "generation-owner"
			oldCfg.Streamers = []config.StreamerConfig{{Username: "generation-streamer"}}
			if err := config.SaveConfig(configPath, &oldCfg); err != nil {
				t.Fatalf("seed config: %v", err)
			}

			oldMiner := newConfigGenerationMiner(t, &oldCfg, configPath)
			oldStarted := make(chan struct{})
			newStarted := make(chan struct{})
			newMinerReady := make(chan *Miner, 1)
			runningStatuses := make(chan struct{}, 2)
			var factoryMu sync.Mutex
			factoryIndex := 0
			controller := lifecycle.New(lifecycle.Config{
				StatusSink: configGenerationStatusSink{running: runningStatuses},
				Factory: func() lifecycle.Runner {
					factoryMu.Lock()
					defer factoryMu.Unlock()
					factoryIndex++
					switch factoryIndex {
					case 1:
						return &configGenerationRunner{miner: oldMiner, started: oldStarted}
					case 2:
						// This is the same handoff App's production factory performs:
						// lifecycle reaches Factory only after N.Run returned, and the
						// snapshot gives N+1 independent mutable config memory.
						newMiner := New(oldMiner.ConfigSnapshot(), configPath)
						newMinerReady <- newMiner
						return &configGenerationRunner{miner: newMiner, started: newStarted}
					default:
						panic("unexpected third config generation")
					}
				},
			})
			runCtx, cancelRun := context.WithCancel(context.Background())
			controllerDone := make(chan error, 1)
			go func() { controllerDone <- controller.Run(runCtx) }()

			releaseOldSave := make(chan struct{})
			var releaseOnce sync.Once
			releaseOld := func() { releaseOnce.Do(func() { close(releaseOldSave) }) }
			releaseOldWrite := make(chan struct{})
			var releaseOldWriteOnce sync.Once
			t.Cleanup(func() {
				releaseOldWriteOnce.Do(func() { close(releaseOldWrite) })
				releaseOld()
				cancelRun()
				select {
				case <-controllerDone:
				case <-time.After(5 * time.Second):
					t.Error("lifecycle controller did not stop")
				}
			})

			waitTestChannel(t, oldStarted, "generation N start")
			waitTestChannel(t, runningStatuses, "generation N running status")
			oldWriteEntered := make(chan struct{})
			oldMiner.configWriteBarrier = func() {
				close(oldWriteEntered)
				<-releaseOldWrite
			}
			drainStarted := make(chan struct{})
			oldMiner.configDrainStarted = func() { close(drainStarted) }
			oldSaveEntered := make(chan struct{})
			var enteredOnce sync.Once
			oldMiner.saveConfigFn = func(path string, cfg *config.Config) error {
				snapshot, err := cloneConfigForBlockedSave(cfg)
				if err != nil {
					return fmt.Errorf("snapshot config before blocked save: %w", err)
				}
				enteredOnce.Do(func() { close(oldSaveEntered) })
				<-releaseOldSave
				return config.SaveConfig(path, snapshot)
			}

			oldWriteDone := make(chan error, 1)
			go func() { oldWriteDone <- tt.write(oldMiner) }()
			waitTestChannel(t, oldWriteEntered, "generation N writer entry")

			if result := controller.Restart(context.Background()); result.Outcome != lifecycle.OutcomeAccepted {
				t.Fatalf("restart outcome=%s err=%v, want accepted", result.Outcome, result.Err)
			}
			waitTestChannel(t, drainStarted, "generation N config-write drain")
			select {
			case <-newStarted:
				t.Fatal("generation N+1 started before the admitted N writer was released")
			default:
			}

			newRule := config.DropRule{Skip: true, HighPriority: true}
			releaseOldWriteOnce.Do(func() { close(releaseOldWrite) })
			waitTestChannel(t, oldSaveEntered, "generation N admitted save")
			select {
			case <-newStarted:
				t.Fatal("generation N+1 started while the admitted N save could still commit")
			default:
			}
			releaseOld()

			if err := <-oldWriteDone; err != nil {
				t.Fatalf("generation N writer: %v", err)
			}
			waitTestChannel(t, newStarted, "generation N+1 start after N quiesced")
			newMiner := <-newMinerReady
			tt.verify(t, newMiner.ConfigSnapshot())
			if err := newMiner.SetDropRule("game::newest", newRule); err != nil {
				t.Fatalf("generation N+1 SetDropRule: %v", err)
			}
			onDisk, err := config.LoadConfig(configPath)
			if err != nil {
				t.Fatalf("reload final config: %v", err)
			}
			if got := onDisk.DropRules["game::newest"]; got != newRule {
				t.Fatalf("generation N stale writer replaced N+1 rule: disk=%+v want=%+v", got, newRule)
			}
			tt.verify(t, onDisk)
		})
	}
}

func TestRetiredGenerationConfigWritersCannotCommit(t *testing.T) {
	tests := []struct {
		name         string
		write        func(*Miner) error
		wantShutdown bool
	}{
		{
			name: "campaign policy",
			write: func(m *Miner) error {
				m.ApplyCampaignPolicy("priority")
				return nil
			},
		},
		{
			name: "health settings",
			write: func(m *Miner) error {
				s := m.CurrentHealthSettings()
				s.CanaryEnabled = !s.CanaryEnabled
				m.ApplyHealthSettings(s)
				return nil
			},
		},
		{
			name: "auto redeem",
			write: func(m *Miner) error {
				return m.SetAutoRedeem("generation-streamer", config.AutoRedeemConfig{
					Enabled: true,
					Budget:  100,
				})
			},
			wantShutdown: true,
		},
		{
			name: "drop rule",
			write: func(m *Miner) error {
				return m.SetDropRule("game::retired", config.DropRule{Skip: true})
			},
			wantShutdown: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.json")
			cfg := config.DefaultConfig()
			cfg.Username = "generation-owner"
			cfg.Streamers = []config.StreamerConfig{{Username: "generation-streamer"}}
			if err := config.SaveConfig(configPath, &cfg); err != nil {
				t.Fatalf("seed config: %v", err)
			}
			beforeDisk, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("read seed config: %v", err)
			}
			beforeLive, err := json.Marshal(&cfg)
			if err != nil {
				t.Fatalf("snapshot live config: %v", err)
			}

			m := newConfigGenerationMiner(t, &cfg, configPath)
			m.applyMu.Lock()
			m.applyDraining = true
			m.applyMu.Unlock()

			err = tt.write(m)
			if tt.wantShutdown && !errors.Is(err, ErrShuttingDown) {
				t.Fatalf("retired writer error = %v, want ErrShuttingDown", err)
			}
			if !tt.wantShutdown && err != nil {
				t.Fatalf("retired void writer adapter error = %v", err)
			}
			afterLive, err := json.Marshal(m.config)
			if err != nil {
				t.Fatalf("snapshot config after retired write: %v", err)
			}
			afterDisk, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("read config after retired write: %v", err)
			}
			if !bytes.Equal(afterLive, beforeLive) {
				t.Fatal("retired generation mutated live config")
			}
			if !bytes.Equal(afterDisk, beforeDisk) {
				t.Fatal("retired generation replaced durable config")
			}
		})
	}
}
