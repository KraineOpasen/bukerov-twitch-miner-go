package miner

import (
	"net/http"
	"net/url"
	"os"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
)

// These tests pin the persistence commit point of the two runtime policy
// mutations, ApplyCampaignPolicy and SetDropRule, on a LIVE authoritative
// generation. The stale-generation fence (stale_generation_fence_test.go)
// already guarantees a RETIRED generation refuses these mutations outright;
// what is pinned here is the other transactional half: on a live generation
// with a configured config path, config.SaveConfig succeeding IS the commit
// point. A failed save must leave the method returning a non-nil error, the
// live config at the prior committed state, the published policy snapshot
// unrefreshed from the rejected value, and config.json untouched — never a
// false HTTP success over a change that silently exists only in memory.
//
// The persistence failure is induced with breakConfigPathForNextSave
// (cp1_c2_matrix_test.go) — the deterministic rename-onto-a-directory seam
// every other commit-point test in this package already uses.

// TestPolicyModePersistFailureOverHTTPFailsClosed is the primary operator-
// facing regression, crossing the REAL seam the defect lives on: a real HTTP
// POST to /api/policy/mode on the real process-level web.Server, served by
// the real PolicyProvider a LIVE generation registered in setupComponents.
//
// On the unfixed base the request is answered 200 with a re-rendered Drops
// list, the live in-memory CampaignPolicy is changed to the rejected value,
// and refreshPolicy publishes a policy snapshot ranked under that rejected
// value — while config.json was never written. All of that is asserted
// against here.
func TestPolicyModePersistFailureOverHTTPFailsClosed(t *testing.T) {
	m, configPath := newFenceMiner(t)
	baseURL, retire := startLiveGeneration(t, m)
	defer retire()

	before := diskPolicy(t, configPath)
	if got, want := before, string(policy.ModeGameOrder); got != want {
		t.Fatalf("seeded on-disk CampaignPolicy = %q, want %q", got, want)
	}

	breakConfigPathForNextSave(t, configPath)

	resp := postForm(t, baseURL, "/api/policy/mode", url.Values{
		"mode": {string(policy.ModeSmart)},
	})

	if resp.StatusCode == http.StatusOK {
		t.Errorf("POST /api/policy/mode = %d OK after config.SaveConfig failed; "+
			"a mutation that never became durable must not be acknowledged", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d: a persistence failure on a LIVE generation is a "+
			"server fault (500), not the fence's retryable 503 and not the lifecycle 409",
			resp.StatusCode, http.StatusInternalServerError)
	}

	// The live config must still be at the prior committed value: the rejected
	// mode must not survive in memory after its persistence failed.
	if live, _ := m.CurrentCampaignPolicy(); live != string(policy.ModeGameOrder) {
		t.Errorf("live CampaignPolicy = %q after a FAILED persist, want the prior committed %q",
			live, policy.ModeGameOrder)
	}

	// No dependent side effect from the rejected value: refreshPolicy must run
	// only past the commit point, so the published snapshot must never carry
	// the rejected mode. (On the unfixed base the failed mutation still calls
	// refreshPolicy, which stores a snapshot ranked under the rejected mode.)
	if mode, _ := m.PolicySnapshot(); mode == policy.ModeSmart {
		t.Errorf("PolicySnapshot mode = %q; the policy engine re-ranked from a value whose "+
			"persistence FAILED", mode)
	}

	// Nothing may have reached disk: the directory breakConfigPathForNextSave
	// installed must still BE that directory (WriteFileAtomic's failing
	// os.Rename is the only step that could have replaced it with a file).
	if info, err := os.Stat(configPath); err != nil || !info.IsDir() {
		t.Fatalf("configPath must still be the untouched directory (nothing was written): stat=%v err=%v", info, err)
	}
}

// TestDropRulePersistFailureOverHTTPFailsClosed pins the same commit point on
// the second policy mutation, over the same real HTTP seam. SetDropRule
// writes a MAP entry inside the live config; a failed persist must restore
// the map to its exact prior state — including nil-ness, which CurrentConfig
// deliberately preserves (current_config_test.go) — rather than leaving the
// rejected rule live and acknowledged.
func TestDropRulePersistFailureOverHTTPFailsClosed(t *testing.T) {
	m, configPath := newFenceMiner(t)
	baseURL, retire := startLiveGeneration(t, m)
	defer retire()

	const rewardKey = "doomed-rule"
	rulesWereNil := m.CurrentConfig().DropRules == nil

	breakConfigPathForNextSave(t, configPath)

	resp := postForm(t, baseURL, "/api/policy/drop-rule", url.Values{
		"rewardKey": {rewardKey},
		"skip":      {"on"},
	})

	if resp.StatusCode == http.StatusOK {
		t.Errorf("POST /api/policy/drop-rule = %d OK after config.SaveConfig failed; "+
			"a mutation that never became durable must not be acknowledged", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d: a persistence failure on a LIVE generation is a "+
			"server fault (500), not the fence's retryable 503 and not the lifecycle 409",
			resp.StatusCode, http.StatusInternalServerError)
	}

	_, rules := m.CurrentCampaignPolicy()
	if _, ok := rules[rewardKey]; ok {
		t.Errorf("live DropRules still carry %q after its persistence FAILED; "+
			"the rejected rule must be rolled back exactly", rewardKey)
	}

	// nil-vs-empty is a meaningful distinction in this package (CurrentConfig
	// snapshots keep nil maps nil); a rollback that leaves behind an allocated
	// empty map would change what every later snapshot reports.
	if rulesWereNil && m.CurrentConfig().DropRules != nil {
		t.Errorf("DropRules became a non-nil map after a rolled-back mutation; " +
			"the rollback must restore nil-ness exactly")
	}

	if info, err := os.Stat(configPath); err != nil || !info.IsDir() {
		t.Fatalf("configPath must still be the untouched directory (nothing was written): stat=%v err=%v", info, err)
	}
}

// TestFailedDropRulePersistCannotBeLaunderedByLaterSave pins the laundering
// half of the transactional contract, at the provider-method seam (the HTTP
// crossing is already covered above; what matters here is the causal chain).
//
// config.SaveConfig is whole-document persistence: it marshals the ENTIRE
// live config. On the unfixed base a rejected drop rule stays live after its
// own save failed, so the NEXT successful save — here an ordinary, unrelated
// ApplyCampaignPolicy — writes the rejected rule to disk incidentally. The
// rejected value gets "laundered" into config.json by a mutation the operator
// never re-issued. Fail-closed rollback is what makes this impossible: the
// rejected rule is gone from the live config before any later save can
// marshal it.
func TestFailedDropRulePersistCannotBeLaunderedByLaterSave(t *testing.T) {
	m, configPath := newFenceMiner(t)
	_, retire := startLiveGeneration(t, m)
	defer retire()

	const (
		keptRule     = "kept-rule"
		launderedKey = "laundered-rule"
	)

	// A successfully committed rule, so the fix can be shown not to throw away
	// committed state while rolling back rejected state.
	if err := m.SetDropRule(keptRule, config.DropRule{HighPriority: true}); err != nil {
		t.Fatalf("SetDropRule(%q) on a live generation = %v, want nil", keptRule, err)
	}

	breakConfigPathForNextSave(t, configPath)
	if err := m.SetDropRule(launderedKey, config.DropRule{Skip: true}); err == nil {
		t.Errorf("SetDropRule(%q) = nil after config.SaveConfig failed; "+
			"a mutation that never became durable must not be acknowledged", launderedKey)
	}

	// Repair the path, then perform an ordinary successful whole-document save.
	if err := os.Remove(configPath); err != nil {
		t.Fatalf("repair config path: %v", err)
	}
	if err := m.ApplyCampaignPolicy(string(policy.ModeEndingSoonest)); err != nil {
		t.Fatalf("ApplyCampaignPolicy after repairing the path = %v, want nil", err)
	}

	onDisk, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("re-read config.json: %v", err)
	}
	if _, ok := onDisk.DropRules[launderedKey]; ok {
		t.Errorf("config.json carries %q: a drop rule whose OWN persistence failed was "+
			"laundered to disk by a later unrelated successful save", launderedKey)
	}
	if _, ok := onDisk.DropRules[keptRule]; !ok {
		t.Errorf("config.json lost %q: rolling back a rejected mutation must not discard "+
			"previously committed state", keptRule)
	}
	if got := onDisk.CampaignPolicy; got != string(policy.ModeEndingSoonest) {
		t.Errorf("config.json CampaignPolicy = %q, want %q (the later save itself must commit)",
			got, policy.ModeEndingSoonest)
	}
}

// TestFailedPolicyModePersistCannotBeLaunderedByLaterSave mirrors the
// laundering proof above for the CampaignPolicy field: a rejected mode
// followed by an unrelated successful whole-document save (a SetDropRule)
// must leave config.json at the prior committed mode, never the rejected
// one. Same commit-point mechanism, pinned symmetrically for the value-field
// writer.
func TestFailedPolicyModePersistCannotBeLaunderedByLaterSave(t *testing.T) {
	m, configPath := newFenceMiner(t)
	_, retire := startLiveGeneration(t, m)
	defer retire()

	breakConfigPathForNextSave(t, configPath)
	if err := m.ApplyCampaignPolicy(string(policy.ModeSmart)); err == nil {
		t.Errorf("ApplyCampaignPolicy = nil after config.SaveConfig failed; " +
			"a mutation that never became durable must not be acknowledged")
	}

	if err := os.Remove(configPath); err != nil {
		t.Fatalf("repair config path: %v", err)
	}
	if err := m.SetDropRule("unrelated-rule", config.DropRule{HighPriority: true}); err != nil {
		t.Fatalf("SetDropRule after repairing the path = %v, want nil", err)
	}

	onDisk, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("re-read config.json: %v", err)
	}
	if got := onDisk.CampaignPolicy; got == string(policy.ModeSmart) {
		t.Errorf("config.json CampaignPolicy = %q: a mode whose OWN persistence failed was "+
			"laundered to disk by a later unrelated successful save", got)
	}
	if got := onDisk.CampaignPolicy; got != string(policy.ModeGameOrder) {
		t.Errorf("config.json CampaignPolicy = %q, want the prior committed %q", got, policy.ModeGameOrder)
	}
	if !onDisk.DropRules["unrelated-rule"].HighPriority {
		t.Errorf("config.json DropRules = %v, want the later successful mutation committed", onDisk.DropRules)
	}
}
