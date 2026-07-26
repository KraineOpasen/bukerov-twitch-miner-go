package miner

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// This file covers M2's evaluateAutoRedeem generation-guard, run-context
// cancellation, rename-continuity and production-seam behavior (design §7
// tests 11-15, 19-20), plus the fakeRewardsClient every test in this file
// (and some in rewards_lifecycle_test.go) shares.
//
// evaluateAutoRedeem holds NO lock across a rewardsClient call, so a
// blocking hook here cannot deadlock against a concurrent SetAutoRedeem or a
// full settings apply running on another goroutine — that is exactly what
// tests 11/12/15 rely on to deterministically interleave without
// time.Sleep.

// redeemCall records one RedeemCustomReward invocation observed by
// fakeRewardsClient.
type redeemCall struct {
	username  string
	rewardID  string
	textInput string
}

// fakeRewardsClient implements rewardsClient for tests: a canned, mutable
// reward listing plus per-call blocking hooks (beforeGet/beforeRedeem), so a
// test can deterministically interleave a concurrent SetAutoRedeem or a full
// settings apply mid-evaluation.
type fakeRewardsClient struct {
	mu       sync.Mutex
	rewards  []*models.CustomReward
	getCalls int
	redeemed []redeemCall

	// beforeGet/beforeRedeem, when non-nil, are called BEFORE the respective
	// method does anything else — and may block.
	beforeGet    func()
	beforeRedeem func()
}

func (f *fakeRewardsClient) GetCustomRewards(*models.Streamer) ([]*models.CustomReward, error) {
	if f.beforeGet != nil {
		f.beforeGet()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	return f.rewards, nil
}

func (f *fakeRewardsClient) RedeemCustomReward(s *models.Streamer, reward *models.CustomReward, textInput string) error {
	if f.beforeRedeem != nil {
		f.beforeRedeem()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redeemed = append(f.redeemed, redeemCall{username: s.GetUsername(), rewardID: reward.ID, textInput: textInput})
	return nil
}

func (f *fakeRewardsClient) redeemCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.redeemed)
}

func (f *fakeRewardsClient) redeemedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, len(f.redeemed))
	for i, rc := range f.redeemed {
		ids[i] = rc.rewardID
	}
	return ids
}

func (f *fakeRewardsClient) getCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls
}

func (f *fakeRewardsClient) setRewardAvailable(id string, avail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rewards {
		if r.ID == id {
			r.IsEnabled = avail
		}
	}
}

// blockUntilReleased returns a hook for fakeRewardsClient's beforeGet/
// beforeRedeem: it signals reached is crossed by closing it, then blocks
// until release is closed — a deterministic rendezvous point with no
// time.Sleep.
func blockUntilReleased(reached, release chan struct{}) func() {
	return func() {
		close(reached)
		<-release
	}
}

// Test 11: a stale generation (set mid-block, before the evaluator ever
// reaches the I9 gate) means zero RedeemCustomReward calls and no runtime
// state created.
func TestEvaluateAutoRedeem_StaleGenerationBeforeGate_NoRedeem(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "staleget")
	seedConfigFile(t, m)
	m.config.AutoRedeem = map[string]config.AutoRedeemConfig{
		"staleget": {Enabled: true, Budget: 1000, RewardIDs: []string{"rew-1"}},
	}
	if err := config.SaveConfig(m.configPath, m.config); err != nil {
		t.Fatalf("reseed config: %v", err)
	}

	reached := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeRewardsClient{
		rewards:   []*models.CustomReward{{ID: "rew-1", Title: "Reward", Cost: 100, IsEnabled: true, IsInStock: true}},
		beforeGet: blockUntilReleased(reached, release),
	}
	m.rewardsAPI = fake

	s := m.streamers.Get("staleget")
	if s == nil {
		t.Fatal("setup: streamer missing")
	}

	done := make(chan struct{})
	go func() {
		m.evaluateAutoRedeem(s)
		close(done)
	}()

	<-reached
	// SetAutoRedeem completes (bumps the generation) WHILE evaluateAutoRedeem
	// is blocked inside GetCustomRewards, well before it ever reaches the I9
	// gate.
	if err := m.SetAutoRedeem("staleget", config.AutoRedeemConfig{Enabled: true, Budget: 50, RewardIDs: []string{"rew-1"}}); err != nil {
		t.Fatalf("SetAutoRedeem: %v", err)
	}
	close(release)
	<-done

	if got := fake.redeemCount(); got != 0 {
		t.Errorf("RedeemCustomReward called %d times, want 0 (stale generation)", got)
	}
	if _, ok := m.autoRedeemState["staleget"]; ok {
		t.Error("runtime state was created for a stale-generation evaluation")
	}
}

// Test 12: a stale generation set WHILE RedeemCustomReward is in flight (the
// I9 gate already passed) means the wire call still happens, but the record
// is refused and a WARN is logged. Kills mutant 6 (disable the generation
// check in recordAutoRedeemed).
func TestEvaluateAutoRedeem_StaleGenerationDuringRedeem_RecordRefusedWithWarn(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "staleredeem")
	seedConfigFile(t, m)
	m.config.AutoRedeem = map[string]config.AutoRedeemConfig{
		"staleredeem": {Enabled: true, Budget: 1000, RewardIDs: []string{"rew-2"}},
	}
	if err := config.SaveConfig(m.configPath, m.config); err != nil {
		t.Fatalf("reseed config: %v", err)
	}

	reached := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeRewardsClient{
		rewards:      []*models.CustomReward{{ID: "rew-2", Title: "Reward2", Cost: 150, IsEnabled: true, IsInStock: true}},
		beforeRedeem: blockUntilReleased(reached, release),
	}
	m.rewardsAPI = fake

	s := m.streamers.Get("staleredeem")
	if s == nil {
		t.Fatal("setup: streamer missing")
	}
	s.SetChannelPoints(10000) // must clear the insufficient-points gate to reach RedeemCustomReward

	cap := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	defer slog.SetDefault(prev)

	done := make(chan struct{})
	go func() {
		m.evaluateAutoRedeem(s)
		close(done)
	}()

	<-reached
	// The I9 gate already passed (we're inside RedeemCustomReward) — bump
	// the generation while the irreversible network call is in flight.
	if err := m.SetAutoRedeem("staleredeem", config.AutoRedeemConfig{Enabled: true, Budget: 1000, RewardIDs: []string{"rew-2"}}); err != nil {
		t.Fatalf("SetAutoRedeem: %v", err)
	}
	close(release)
	<-done

	if got := fake.redeemCount(); got != 1 {
		t.Fatalf("RedeemCustomReward called %d times, want 1 (the wire call itself must still happen)", got)
	}
	if _, ok := m.autoRedeemState["staleredeem"]; ok {
		t.Errorf("runtime state must not exist after a refused stale-generation record: %+v", m.autoRedeemState["staleredeem"])
	}
	if !cap.hasSubstring("stale window") {
		t.Errorf("expected a WARN about a stale-window redemption, got: %v", cap.msgs)
	}
}

// Test 13: a budget reduction mid-evaluation does not affect the in-flight
// (stale) cycle, but the NEXT cycle sees the new, reduced budget exactly.
func TestEvaluateAutoRedeem_BudgetChangeDuringEvaluationTakesEffectNextCycle(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "budgetchange")
	seedConfigFile(t, m)
	m.config.AutoRedeem = map[string]config.AutoRedeemConfig{
		"budgetchange": {Enabled: true, Budget: 1000, RewardIDs: []string{"rew-3"}},
	}
	if err := config.SaveConfig(m.configPath, m.config); err != nil {
		t.Fatalf("reseed config: %v", err)
	}

	reached := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeRewardsClient{
		rewards:   []*models.CustomReward{{ID: "rew-3", Title: "Reward3", Cost: 100, IsEnabled: true, IsInStock: true}},
		beforeGet: blockUntilReleased(reached, release),
	}
	m.rewardsAPI = fake

	s := m.streamers.Get("budgetchange")
	if s == nil {
		t.Fatal("setup: streamer missing")
	}

	done := make(chan struct{})
	go func() {
		m.evaluateAutoRedeem(s)
		close(done)
	}()

	<-reached
	if err := m.SetAutoRedeem("budgetchange", config.AutoRedeemConfig{Enabled: true, Budget: 30, RewardIDs: []string{"rew-3"}}); err != nil {
		t.Fatalf("SetAutoRedeem: %v", err)
	}
	close(release)
	<-done

	if got := fake.redeemCount(); got != 0 {
		t.Fatalf("the stale-generation cycle must not redeem: got %d calls", got)
	}

	// A FRESH evaluation cycle, using the NEW generation, must see the NEW
	// (reduced) budget: cost 100 now exceeds remaining budget 30, so it must
	// skip rather than redeem.
	fake2 := &fakeRewardsClient{rewards: []*models.CustomReward{{ID: "rew-3", Title: "Reward3", Cost: 100, IsEnabled: true, IsInStock: true}}}
	m.rewardsAPI = fake2
	m.evaluateAutoRedeem(s)
	if got := fake2.redeemCount(); got != 0 {
		t.Errorf("the new window must have exactly the new (reduced) budget: got %d redeems, want 0", got)
	}
}

// Test 14: a cancelled run context blocks redemption even when everything
// else about the evaluation is otherwise valid. Kills mutant 9 (bypass the
// runCtx check in autoRedeemStillCurrent).
func TestEvaluateAutoRedeem_CancelledRunContextBlocksRedemption(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "ctxcancel")
	m.config.AutoRedeem = map[string]config.AutoRedeemConfig{
		"ctxcancel": {Enabled: true, Budget: 1000, RewardIDs: []string{"rew-4"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.runCtx = ctx

	fake := &fakeRewardsClient{rewards: []*models.CustomReward{{ID: "rew-4", Title: "Reward4", Cost: 50, IsEnabled: true, IsInStock: true}}}
	m.rewardsAPI = fake

	s := m.streamers.Get("ctxcancel")
	if s == nil {
		t.Fatal("setup: streamer missing")
	}
	m.evaluateAutoRedeem(s)

	if got := fake.redeemCount(); got != 0 {
		t.Errorf("RedeemCustomReward called %d times, want 0 (m.runCtx already cancelled)", got)
	}
}

// Test 15: a full rename apply (commit + CommitPlan + finishApply)
// completing WHILE a redemption is in flight preserves budget continuity
// (C4) — the record lands in the migrated new-login window rather than being
// refused as stale. This pins the gen-MIGRATION behavior: a blanket
// rename-bump (rather than a migration) would refuse this record and reopen
// the C4 overspend bug.
func TestEvaluateAutoRedeem_RenameMidEvaluationPreservesBudgetContinuity(t *testing.T) {
	client := newRenameCapableAPI()
	client.set("c4renold", "id-c4continuity")
	m, _, _ := newRenameTestMiner(t, client, "c4renold")

	m.config.AutoRedeem = map[string]config.AutoRedeemConfig{
		"c4renold": {Enabled: true, Budget: 1000, RewardIDs: []string{"rew-c4"}},
	}
	// [RR8] Seed runtime state directly (not via SetAutoRedeem, which would
	// delete it) so both old/new generations stay at their zero value,
	// matching the "destination carries no higher history" precondition
	// migrateAutoRedeemGenLocked's continuity guarantee depends on.
	m.autoRedeemState["c4renold"] = &autoRedeemRuntime{spent: 900, redeemed: map[string]bool{}}

	reached := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeRewardsClient{
		rewards:      []*models.CustomReward{{ID: "rew-c4", Title: "RewardC4", Cost: 100, IsEnabled: true, IsInStock: true}},
		beforeRedeem: blockUntilReleased(reached, release),
	}
	m.rewardsAPI = fake

	s := m.streamers.Get("c4renold")
	if s == nil {
		t.Fatal("setup: streamer missing")
	}
	s.SetChannelPoints(10000) // must clear the insufficient-points gate to reach RedeemCustomReward

	cap := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	defer slog.SetDefault(prev)

	done := make(chan struct{})
	go func() {
		m.evaluateAutoRedeem(s)
		close(done)
	}()

	<-reached
	client.set("c4rennew", "id-c4continuity") // same stable ID -> rename
	if err := m.applySettings(context.Background(), renameRuntimeStreamers(m, "c4renold", "c4rennew")); err != nil {
		t.Fatalf("rename apply failed: %v", err)
	}
	close(release)
	<-done

	if got := fake.redeemCount(); got != 1 {
		t.Fatalf("RedeemCustomReward called %d times, want 1", got)
	}
	if _, ok := m.autoRedeemState["c4renold"]; ok {
		t.Error("an orphaned runtime state was left under the old login")
	}
	rt := m.autoRedeemState["c4rennew"]
	if rt == nil {
		t.Fatal("the migrated-window record is missing under the new login")
	}
	if rt.spent != 1000 {
		t.Errorf("spent = %d, want 1000 (900 pre-existing + 100 accepted into the migrated window)", rt.spent)
	}
	if cap.hasSubstring("stale window") {
		t.Errorf("a blanket rename-bump would have refused this record as stale; the migration must accept it: %v", cap.msgs)
	}
}

// Test 19: production reward evaluation through the seam — a happy-path
// table exercising: redeem, edge-triggered re-arm, user-input skip,
// insufficient-channel-points skip, and over-budget skip, all in one
// sequence of evaluation cycles against a stateful fake.
func TestEvaluateAutoRedeem_HappyPathThroughSeam(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "seamtest")
	m.config.AutoRedeem = map[string]config.AutoRedeemConfig{
		"seamtest": {Enabled: true, Budget: 300, RewardIDs: []string{"avail", "userinput", "toobig", "overbudget"}},
	}
	s := m.streamers.Get("seamtest")
	if s == nil {
		t.Fatal("setup: streamer missing")
	}
	// Channel points are deliberately lower than "toobig"'s cost but higher
	// than "avail"'s, so "toobig" is skipped specifically by the
	// insufficient-points gate, not the budget gate.
	s.SetChannelPoints(150)

	fake := &fakeRewardsClient{rewards: []*models.CustomReward{
		{ID: "avail", Title: "Available", Cost: 100, IsEnabled: true, IsInStock: true},
		{ID: "userinput", Title: "UserInput", Cost: 10, IsEnabled: true, IsInStock: true, IsUserInputRequired: true},
		{ID: "toobig", Title: "TooExpensiveForPoints", Cost: 200, IsEnabled: true, IsInStock: true},
		{ID: "overbudget", Title: "OverBudget", Cost: 1000, IsEnabled: true, IsInStock: true},
	}}
	m.rewardsAPI = fake

	// Cycle 1: "avail" redeems (100 <= budget 300, 100 <= points 150).
	// "userinput" never redeems. "toobig" passes the budget check (remaining
	// 200 after avail >= cost 200) but fails the points check (200 > 150).
	// "overbudget" fails the budget check (remaining 200 < cost 1000).
	m.evaluateAutoRedeem(s)
	if got := fake.redeemCount(); got != 1 {
		t.Fatalf("cycle 1: redeemed %d times, want 1", got)
	}
	if ids := fake.redeemedIDs(); len(ids) != 1 || ids[0] != "avail" {
		t.Errorf("cycle 1: redeemed %v, want [avail]", ids)
	}
	if got := m.autoRedeemSpent("seamtest"); got != 100 {
		t.Errorf("spent after cycle 1 = %d, want 100", got)
	}

	// Cycle 2: "avail" is edge-triggered — already redeemed this
	// availability window, so it does NOT redeem again while unchanged.
	m.evaluateAutoRedeem(s)
	if got := fake.redeemCount(); got != 1 {
		t.Fatalf("cycle 2: redeemed %d times, want still 1 (edge-triggered, avail unchanged)", got)
	}

	// "avail" goes unavailable (re-arms it) then comes back available.
	fake.setRewardAvailable("avail", false)
	m.evaluateAutoRedeem(s) // clears the redeemed flag
	fake.setRewardAvailable("avail", true)
	m.evaluateAutoRedeem(s) // redeems again: 100+100=200 <= budget 300
	if got := fake.redeemCount(); got != 2 {
		t.Fatalf("after re-arm: redeemed %d times, want 2", got)
	}
	if got := m.autoRedeemSpent("seamtest"); got != 200 {
		t.Errorf("spent after re-arm = %d, want 200", got)
	}

	// "userinput" and "toobig" must never appear among redeemed IDs, in any
	// cycle above.
	for _, id := range []string{"userinput", "toobig", "overbudget"} {
		for _, got := range fake.redeemedIDs() {
			if got == id {
				t.Errorf("%s should never have been redeemed", id)
			}
		}
	}
}

// Test 20: refreshCandidateAutoRedeemLocked panics when called without m.mu
// held — the TryLock guard [R3]. Deterministically kills the "snapshot
// without locking" mutant (mutant 10).
func TestRefreshCandidateAutoRedeemLocked_PanicsWithoutMu(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "guardtest")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic when m.mu is not held")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "not held") {
			t.Errorf("panic value = %v, want a message mentioning m.mu not being held", r)
		}
	}()

	candidate := &config.Config{}
	m.refreshCandidateAutoRedeemLocked(candidate, nil, nil)
}
