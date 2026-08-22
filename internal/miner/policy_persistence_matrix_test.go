package miner

import (
	"os"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
)

// State-matrix coverage for the policy writers' persistence commit point, at
// the exported-method seam. A freshly built, never-run miner is ADMITTED by
// the mutation fence (beginApply refuses only draining/cancelled miners —
// internal/app's TestGenerationConfigHandsOverAnIsolatedSnapshot leans on the
// same property), so these cases exercise the commit point without paying for
// a full HTTP generation each; the HTTP crossing is pinned separately in
// policy_persistence_fail_closed_test.go.
//
// refreshPolicy no-ops on these miners (no dropsTracker), which is fine here:
// the refresh-ordering observable is pinned at the HTTP seam, and what this
// matrix owns is rollback exactness — value, map entry, and nil-ness.

// TestSetDropRulePersistFailureMatrix drives SetDropRule into a failing
// persist from every distinct prior map state and asserts the exact pre-call
// state — value, structure, and nil-ness — survives.
func TestSetDropRulePersistFailureMatrix(t *testing.T) {
	const key = "matrix-key"
	committed := config.DropRule{HighPriority: true}
	replacement := config.DropRule{Skip: true}

	cases := []struct {
		name string
		// seed prepares the live map (through the public API where a
		// committed prior state is wanted, directly for the raw shapes).
		seed func(t *testing.T, m *Miner)
		// mutate is the failing call under test.
		mutate func(m *Miner) error
		// check asserts the exact pre-call state survived the rollback.
		check func(t *testing.T, m *Miner)
	}{
		{
			name: "nil map, add",
			seed: func(t *testing.T, m *Miner) {},
			mutate: func(m *Miner) error {
				return m.SetDropRule(key, replacement)
			},
			check: func(t *testing.T, m *Miner) {
				if m.CurrentConfig().DropRules != nil {
					t.Error("DropRules must be re-nilled after a rolled-back add on a nil map")
				}
			},
		},
		{
			name: "empty non-nil map, add",
			seed: func(t *testing.T, m *Miner) {
				m.mu.Lock()
				m.config.DropRules = map[string]config.DropRule{}
				m.mu.Unlock()
			},
			mutate: func(m *Miner) error {
				return m.SetDropRule(key, replacement)
			},
			check: func(t *testing.T, m *Miner) {
				snap := m.CurrentConfig().DropRules
				if snap == nil {
					t.Error("DropRules must stay a non-nil (empty) map: the rollback must not re-nil a map that was allocated before the call")
				}
				if len(snap) != 0 {
					t.Errorf("DropRules = %v, want empty after rollback", snap)
				}
			},
		},
		{
			name: "key present, replace",
			seed: func(t *testing.T, m *Miner) {
				if err := m.SetDropRule(key, committed); err != nil {
					t.Fatalf("seed committed rule: %v", err)
				}
			},
			mutate: func(m *Miner) error {
				return m.SetDropRule(key, replacement)
			},
			check: func(t *testing.T, m *Miner) {
				_, rules := m.CurrentCampaignPolicy()
				if got := rules[key]; got != committed {
					t.Errorf("rule %q = %+v after rolled-back replace, want the committed %+v", key, got, committed)
				}
			},
		},
		{
			name: "key present, reset (zero rule)",
			seed: func(t *testing.T, m *Miner) {
				if err := m.SetDropRule(key, committed); err != nil {
					t.Fatalf("seed committed rule: %v", err)
				}
			},
			mutate: func(m *Miner) error {
				return m.SetDropRule(key, config.DropRule{})
			},
			check: func(t *testing.T, m *Miner) {
				_, rules := m.CurrentCampaignPolicy()
				if got, ok := rules[key]; !ok || got != committed {
					t.Errorf("rule %q = %+v (present=%v) after rolled-back reset, want the committed %+v restored", key, got, ok, committed)
				}
			},
		},
		{
			name: "key absent, reset (zero rule)",
			seed: func(t *testing.T, m *Miner) {
				if err := m.SetDropRule("other-key", committed); err != nil {
					t.Fatalf("seed unrelated rule: %v", err)
				}
			},
			mutate: func(m *Miner) error {
				return m.SetDropRule(key, config.DropRule{})
			},
			check: func(t *testing.T, m *Miner) {
				_, rules := m.CurrentCampaignPolicy()
				if _, ok := rules[key]; ok {
					t.Errorf("rule %q appeared out of a rolled-back reset of a nonexistent key", key)
				}
				if got := rules["other-key"]; got != committed {
					t.Errorf("unrelated rule = %+v after rollback, want %+v untouched", got, committed)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, configPath := newFenceMiner(t)
			tc.seed(t, m)
			breakConfigPathForNextSave(t, configPath)

			if err := tc.mutate(m); err == nil {
				t.Error("SetDropRule = nil after config.SaveConfig failed; the mutation must fail closed")
			}
			tc.check(t, m)

			if info, err := os.Stat(configPath); err != nil || !info.IsDir() {
				t.Fatalf("configPath must still be the untouched directory (nothing was written): stat=%v err=%v", info, err)
			}
		})
	}
}

// TestSetDropRuleFailureKeepsEarlierCommittedValue is the deterministic core
// of acceptance criterion A17: a failed mutation's rollback must restore the
// value the LAST SUCCESSFUL mutation committed — never an older one — because
// the rollback runs inside the same m.mu critical section that performed the
// failed mutation, and every competing writer is serialized by that lock.
func TestSetDropRuleFailureKeepsEarlierCommittedValue(t *testing.T) {
	m, configPath := newFenceMiner(t)
	const key = "a17-key"

	first := config.DropRule{HighPriority: true}
	second := config.DropRule{HighPriority: true, NextRewardOnly: true}

	if err := m.SetDropRule(key, first); err != nil {
		t.Fatalf("first committed SetDropRule: %v", err)
	}
	if err := m.SetDropRule(key, second); err != nil {
		t.Fatalf("second committed SetDropRule: %v", err)
	}

	breakConfigPathForNextSave(t, configPath)
	if err := m.SetDropRule(key, config.DropRule{Skip: true}); err == nil {
		t.Fatal("SetDropRule = nil after config.SaveConfig failed")
	}

	_, rules := m.CurrentCampaignPolicy()
	if got := rules[key]; got != second {
		t.Errorf("rule %q = %+v after rollback, want the LAST committed %+v (a rollback must never erase a prior successful mutation)", key, got, second)
	}
}

// TestSetDropRuleEmptyKeyStaysNoOpSuccess pins the documented live semantics
// of a whitespace/empty reward key: a no-op nil return on a live generation,
// even when the config path could not be written — no persist is attempted
// for a mutation that does not exist.
func TestSetDropRuleEmptyKeyStaysNoOpSuccess(t *testing.T) {
	m, configPath := newFenceMiner(t)
	breakConfigPathForNextSave(t, configPath)

	for _, key := range []string{"", "   ", "\t"} {
		if err := m.SetDropRule(key, config.DropRule{Skip: true}); err != nil {
			t.Errorf("SetDropRule(%q) = %v, want nil (empty key is a documented no-op success)", key, err)
		}
	}
	if rules := m.CurrentConfig().DropRules; rules != nil {
		t.Errorf("DropRules = %v, want nil untouched by no-op calls", rules)
	}
}

// TestApplyCampaignPolicyPersistFailureMatrix covers the mode writer: a
// failed persist restores the RAW stored value byte-exactly (including a
// value that only normalization would turn into a valid mode), and a
// subsequent successful mutation still works.
func TestApplyCampaignPolicyPersistFailureMatrix(t *testing.T) {
	t.Run("valid committed mode survives", func(t *testing.T) {
		m, configPath := newFenceMiner(t)
		if err := m.ApplyCampaignPolicy(string(policy.ModeEndingSoonest)); err != nil {
			t.Fatalf("seed committed mode: %v", err)
		}

		breakConfigPathForNextSave(t, configPath)
		if err := m.ApplyCampaignPolicy(string(policy.ModeSmart)); err == nil {
			t.Fatal("ApplyCampaignPolicy = nil after config.SaveConfig failed; the mutation must fail closed")
		}
		if live, _ := m.CurrentCampaignPolicy(); live != string(policy.ModeEndingSoonest) {
			t.Errorf("live CampaignPolicy = %q, want the committed %q", live, policy.ModeEndingSoonest)
		}
	})

	t.Run("raw stored value restored byte-exactly", func(t *testing.T) {
		m, configPath := newFenceMiner(t)
		// A raw, non-normalized stored value (as a hand-edited config.json can
		// carry): the rollback must restore exactly these bytes, not the
		// normalized mode they map to.
		m.mu.Lock()
		m.config.CampaignPolicy = "hand-edited-raw-value"
		m.mu.Unlock()

		breakConfigPathForNextSave(t, configPath)
		if err := m.ApplyCampaignPolicy(string(policy.ModeSmart)); err == nil {
			t.Fatal("ApplyCampaignPolicy = nil after config.SaveConfig failed")
		}
		if got := m.CurrentConfig().CampaignPolicy; got != "hand-edited-raw-value" {
			t.Errorf("stored CampaignPolicy = %q after rollback, want the raw pre-call value restored byte-exactly", got)
		}
	})

	t.Run("subsequent successful mutation works", func(t *testing.T) {
		m, configPath := newFenceMiner(t)
		breakConfigPathForNextSave(t, configPath)
		if err := m.ApplyCampaignPolicy(string(policy.ModeSmart)); err == nil {
			t.Fatal("ApplyCampaignPolicy = nil after config.SaveConfig failed")
		}
		if err := os.Remove(configPath); err != nil {
			t.Fatalf("repair config path: %v", err)
		}
		if err := m.ApplyCampaignPolicy(string(policy.ModeClosestToReward)); err != nil {
			t.Fatalf("ApplyCampaignPolicy after repairing the path = %v, want nil", err)
		}
		if got := diskPolicy(t, configPath); got != string(policy.ModeClosestToReward) {
			t.Errorf("config.json CampaignPolicy = %q, want %q", got, policy.ModeClosestToReward)
		}
	})

	t.Run("alias input still normalizes on success", func(t *testing.T) {
		m, configPath := newFenceMiner(t)
		if err := m.ApplyCampaignPolicy("smart"); err != nil {
			t.Fatalf("ApplyCampaignPolicy(lowercase): %v", err)
		}
		if live, _ := m.CurrentCampaignPolicy(); live != string(policy.ModeSmart) {
			t.Errorf("live CampaignPolicy = %q, want normalized %q", live, policy.ModeSmart)
		}
		if got := diskPolicy(t, configPath); got != string(policy.ModeSmart) {
			t.Errorf("config.json CampaignPolicy = %q, want normalized %q", got, policy.ModeSmart)
		}
	})
}

// TestPolicyMutationsWithoutConfigPathStaySuccessful pins the documented
// library semantics: with no config file configured there is nothing to
// persist, and both policy mutations remain plain hot-apply successes —
// making persistence the commit point must not turn "no config file" into a
// failure. Mirrors TestApplySettingsNoRenameWithoutConfigPathStaysSuccessful.
func TestPolicyMutationsWithoutConfigPathStaySuccessful(t *testing.T) {
	m, _ := newFenceMiner(t)
	m.configPath = ""

	if err := m.ApplyCampaignPolicy(string(policy.ModeSmart)); err != nil {
		t.Fatalf("ApplyCampaignPolicy with no config path = %v, want nil", err)
	}
	if live, _ := m.CurrentCampaignPolicy(); live != string(policy.ModeSmart) {
		t.Errorf("live CampaignPolicy = %q, want %q (hot-apply must still happen)", live, policy.ModeSmart)
	}

	if err := m.SetDropRule("library-rule", config.DropRule{Skip: true}); err != nil {
		t.Fatalf("SetDropRule with no config path = %v, want nil", err)
	}
	if _, rules := m.CurrentCampaignPolicy(); !rules["library-rule"].Skip {
		t.Errorf("DropRules = %v, want the hot-applied rule present", rules)
	}
}

// TestConcurrentPolicyMutationsStayConsistent is the -race stress half of
// A17/A18, in three phases: concurrent writers against a WORKING path;
// concurrent FAILING writers (broken path) racing snapshot readers — every
// rollback racing every other; then a repaired path with a final successful
// mutation. Memory and disk must be equivalent at the end, and the failing
// phase must not have erased any phase-1 committed value. The deterministic
// rollback-ordering half is TestSetDropRuleFailureKeepsEarlierCommittedValue
// above.
func TestConcurrentPolicyMutationsStayConsistent(t *testing.T) {
	m, configPath := newFenceMiner(t)

	keys := []string{"k1", "k2", "k3", "k4"}
	modes := []string{
		string(policy.ModeSmart),
		string(policy.ModeEndingSoonest),
		string(policy.ModeClosestToReward),
	}
	committed := config.DropRule{HighPriority: true}

	// Phase 1: concurrent successful writers + readers.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			if err := m.SetDropRule(keys[i%len(keys)], committed); err != nil {
				t.Errorf("concurrent SetDropRule: %v", err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if err := m.ApplyCampaignPolicy(modes[i%len(modes)]); err != nil {
				t.Errorf("concurrent ApplyCampaignPolicy: %v", err)
			}
		}(i)
		go func() {
			defer wg.Done()
			_, _ = m.CurrentCampaignPolicy()
			_ = m.CurrentConfig()
		}()
	}
	wg.Wait()

	// Phase 2: break the path and race FAILING mutations (each performing a
	// rollback) against each other and against snapshot readers. Every write
	// must fail closed; no rollback may erase a phase-1 committed rule.
	breakConfigPathForNextSave(t, configPath)
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			if err := m.SetDropRule(keys[i%len(keys)], config.DropRule{Skip: true}); err == nil {
				t.Error("SetDropRule = nil while the config path is broken; must fail closed")
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if err := m.ApplyCampaignPolicy(modes[(i+1)%len(modes)]); err == nil {
				t.Error("ApplyCampaignPolicy = nil while the config path is broken; must fail closed")
			}
		}(i)
		go func() {
			defer wg.Done()
			_, _ = m.CurrentCampaignPolicy()
			_ = m.CurrentConfig()
		}()
	}
	wg.Wait()

	for _, k := range keys {
		if _, liveRules := m.CurrentCampaignPolicy(); liveRules[k] != committed {
			t.Errorf("rule %q = %+v after the failing phase, want the phase-1 committed %+v (a racing rollback erased a successful mutation)", k, liveRules[k], committed)
		}
	}

	// Phase 3: repair the path; a subsequent mutation commits normally.
	if err := os.Remove(configPath); err != nil {
		t.Fatalf("repair config path: %v", err)
	}
	if err := m.ApplyCampaignPolicy(string(policy.ModeGameOrder)); err != nil {
		t.Fatalf("ApplyCampaignPolicy after repairing the path = %v, want nil", err)
	}

	// Post-condition: what the miner reports live is exactly what the last
	// committed save left on disk.
	liveMode, liveRules := m.CurrentCampaignPolicy()
	onDisk, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("re-read config.json: %v", err)
	}
	if got := string(policy.Normalize(onDisk.CampaignPolicy)); got != liveMode {
		t.Errorf("disk CampaignPolicy = %q, live = %q; memory and disk diverged", got, liveMode)
	}
	if len(onDisk.DropRules) != len(liveRules) {
		t.Errorf("disk DropRules = %v, live = %v; memory and disk diverged", onDisk.DropRules, liveRules)
	}
	for k, v := range liveRules {
		if onDisk.DropRules[k] != v {
			t.Errorf("disk rule %q = %+v, live = %+v", k, onDisk.DropRules[k], v)
		}
	}
}
