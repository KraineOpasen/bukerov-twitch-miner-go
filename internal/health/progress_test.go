package health

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// --- fakes ---

type fakeDropsView struct {
	mu        sync.Mutex
	campaigns []*models.Campaign
	status    drops.SyncStatus
	syncNow   int
	triggered int
}

func (f *fakeDropsView) Campaigns() []*models.Campaign {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*models.Campaign(nil), f.campaigns...)
}
func (f *fakeDropsView) SyncStatus() drops.SyncStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}
func (f *fakeDropsView) SyncNow()             { f.mu.Lock(); f.syncNow++; f.mu.Unlock() }
func (f *fakeDropsView) TriggerProgressSync() { f.mu.Lock(); f.triggered++; f.mu.Unlock() }
func (f *fakeDropsView) counts() (syncNow, triggered int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncNow, f.triggered
}
func (f *fakeDropsView) observe(at time.Time, errText string) {
	f.mu.Lock()
	f.status.ProgressRuns++
	f.status.ProgressLastSyncAt = at
	f.status.ProgressLastError = errText
	f.mu.Unlock()
}

type refreshCall struct {
	login string
	mode  watcher.RefreshMode
}

type fakeWatchView struct {
	mu        sync.Mutex
	slots     []string
	watching  map[string]bool
	successes map[string]int
	refreshes []refreshCall
}

func (f *fakeWatchView) BrokerSnapshot() watcher.BrokerSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap := watcher.BrokerSnapshot{MaxSlots: 2}
	for i, ch := range f.slots {
		snap.Slots = append(snap.Slots, watcher.SlotAssignment{Slot: i + 1, Channel: ch})
	}
	return snap
}
func (f *fakeWatchView) IsWatching(login string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.watching != nil {
		return f.watching[login]
	}
	for _, ch := range f.slots {
		if ch == login {
			return true
		}
	}
	return false
}
func (f *fakeWatchView) ReportStats(login string) (watcher.ReportStats, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.successes[login]
	if !ok {
		return watcher.ReportStats{}, false
	}
	return watcher.ReportStats{Successes: n}, true
}
func (f *fakeWatchView) RequestSessionRefresh(login string, mode watcher.RefreshMode) {
	f.mu.Lock()
	f.refreshes = append(f.refreshes, refreshCall{login, mode})
	f.mu.Unlock()
}
func (f *fakeWatchView) LastSessionRefresh(string) (watcher.SessionRefreshOutcome, bool) {
	return watcher.SessionRefreshOutcome{}, false
}
func (f *fakeWatchView) addSuccesses(login string, n int) {
	f.mu.Lock()
	if f.successes == nil {
		f.successes = make(map[string]int)
	}
	f.successes[login] += n
	f.mu.Unlock()
}
func (f *fakeWatchView) refreshCalls() []refreshCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]refreshCall(nil), f.refreshes...)
}

type dropTransition struct {
	kind            string // "stalled" | "recovered"
	campaign, drop  string
	channel, detail string
}

type fakeDropNotifier struct {
	mu    sync.Mutex
	calls []dropTransition
}

func (f *fakeDropNotifier) NotifyDropStalled(campaign, drop, channel, detail string) {
	f.mu.Lock()
	f.calls = append(f.calls, dropTransition{"stalled", campaign, drop, channel, detail})
	f.mu.Unlock()
}
func (f *fakeDropNotifier) NotifyDropRecovered(campaign, drop, channel, detail string) {
	f.mu.Lock()
	f.calls = append(f.calls, dropTransition{"recovered", campaign, drop, channel, detail})
	f.mu.Unlock()
}
func (f *fakeDropNotifier) byKind(kind string) []dropTransition {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []dropTransition
	for _, c := range f.calls {
		if c.kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// --- harness ---

type watchdogHarness struct {
	w        *ProgressWatchdog
	drops    *fakeDropsView
	watch    *fakeWatchView
	prober   *fakeProber
	notifier *fakeDropNotifier
	center   *Center
	campaign *models.Campaign
	streamer *models.Streamer
	now      time.Time
}

// newWatchdogHarness builds a fully-healthy farming setup: one active
// campaign with one 240-minute drop at 100 minutes, farmed by channel "chan"
// which holds a slot, matches the game, and has the campaign assigned.
func newWatchdogHarness(t *testing.T) *watchdogHarness {
	t.Helper()
	// Anchored to real time because Drop.DateTimeMatch() consults time.Now()
	// internally; the watchdog itself runs on the injected h.now clock, which
	// only ever advances a few hours — well inside the ±48h windows below.
	base := time.Now()
	game := &models.Game{ID: "g1", Name: "Game One"}
	campaign := &models.Campaign{
		ID:      "camp-1",
		Name:    "Campaign One",
		Game:    game,
		Status:  models.CampaignActive,
		StartAt: base.Add(-24 * time.Hour),
		EndAt:   base.Add(48 * time.Hour),
		Drops: []*models.Drop{{
			ID: "drop-1", Name: "Drop One",
			MinutesRequired: 240, CurrentMinutesWatched: 100,
			StartAt: base.Add(-24 * time.Hour), EndAt: base.Add(48 * time.Hour),
		}},
	}

	streamer := models.NewStreamer("chan", models.StreamerSettings{ClaimDrops: true})
	streamer.ChannelID = "cid-chan"
	streamer.Stream.Update("b1", "title", game, nil, 100)
	streamer.Stream.SetCampaigns([]*models.Campaign{campaign})

	h := &watchdogHarness{
		drops:    &fakeDropsView{campaigns: []*models.Campaign{campaign}},
		watch:    &fakeWatchView{slots: []string{"chan"}},
		prober:   &fakeProber{res: watcher.ProbeResult{OK: true}},
		notifier: &fakeDropNotifier{},
		center:   NewCenter(),
		campaign: campaign,
		streamer: streamer,
		now:      base,
	}
	h.watch.addSuccesses("chan", 0)

	resolver := func(login string) *models.Streamer {
		if login == "chan" {
			return h.streamer
		}
		return nil
	}
	h.w = NewProgressWatchdog(h.center, h.drops, h.watch, h.prober, h.notifier, NewAvoidList(), resolver, WatchdogConfig{
		Enabled:            true,
		StallDelay:         20 * time.Minute,
		StallConfirmations: 3,
		RecoveryCooldown:   0,
		AvoidTTL:           time.Hour,
		Rearm:              6 * time.Hour,
	})
	h.w.now = func() time.Time { return h.now }
	return h
}

// tick advances time, optionally records a clean observation and delivered
// reports, and runs one evaluation.
func (h *watchdogHarness) tick(advance time.Duration, observe bool, reports int) {
	h.now = h.now.Add(advance)
	if observe {
		h.drops.observe(h.now, "")
	}
	if reports > 0 {
		h.watch.addSuccesses("chan", reports)
	}
	h.w.evaluate(h.now)
}

// driveToStall runs enough observed, report-carrying, progress-free ticks to
// satisfy every stall threshold and returns after the FIRST recovery stage
// has executed.
func (h *watchdogHarness) driveToStall(t *testing.T) {
	t.Helper()
	h.w.evaluate(h.now) // initial sighting: baselines established
	// Three observed ticks: 30m elapsed (≥20m), 3 clean observations (≥3),
	// 9 delivered reports (≥5) — the third tick confirms and runs stage 1.
	for i := 0; i < 3; i++ {
		h.tick(10*time.Minute, true, 3)
	}
	st := h.state(t)
	if st.Status != ProgressRecovering || st.RecoveryStage != 1 {
		t.Fatalf("expected the first recovery stage after stall confirmation, got %+v", st)
	}
}

func (h *watchdogHarness) state(t *testing.T) DropProgress {
	t.Helper()
	snap := h.w.Snapshot()
	if len(snap.Drops) != 1 {
		t.Fatalf("expected exactly one tracked drop, got %+v", snap.Drops)
	}
	return snap.Drops[0]
}

// --- detection & state machine ---

func TestWatchdogHealthyWhileProgressAdvances(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.evaluate(h.now)

	for i := 0; i < 5; i++ {
		h.campaign.Drops[0].CurrentMinutesWatched += 10 // Twitch credits minutes
		h.tick(10*time.Minute, true, 5)
		if st := h.state(t); st.Status != ProgressHealthy {
			t.Fatalf("advancing progress must stay healthy, got %+v", st)
		}
	}
	if syncNow, triggered := h.drops.counts(); syncNow != 0 || triggered != 0 {
		t.Fatalf("no recovery work while healthy, got syncNow=%d triggered=%d", syncNow, triggered)
	}
}

// TestWatchdogRecoveryStageOrder drives a confirmed stall through the whole
// pipeline and pins the exact stage order and the lever each stage pulls.
func TestWatchdogRecoveryStageOrder(t *testing.T) {
	h := newWatchdogHarness(t)
	h.driveToStall(t) // stage 1: forced lightweight inventory sync

	if _, triggered := h.drops.counts(); triggered != 1 {
		t.Fatalf("stage 1 must force a lightweight inventory sync, got %d", triggered)
	}

	h.tick(10*time.Minute, true, 3) // stage 2
	if syncNow, _ := h.drops.counts(); syncNow != 1 {
		t.Fatalf("stage 2 must force a full resync, got %d", syncNow)
	}

	h.tick(10*time.Minute, true, 3) // stage 3
	calls := h.watch.refreshCalls()
	if len(calls) != 1 || calls[0].mode != watcher.RefreshStreamInfo || calls[0].login != "chan" {
		t.Fatalf("stage 3 must stage a stream-info refresh into the broker, got %+v", calls)
	}

	h.tick(10*time.Minute, true, 3) // stage 4
	if got := h.prober.callCount(); got != 1 {
		t.Fatalf("stage 4 must run the transport probe, got %d", got)
	}

	h.tick(10*time.Minute, true, 3) // stage 5
	calls = h.watch.refreshCalls()
	if len(calls) != 2 || calls[1].mode != watcher.RefreshSession {
		t.Fatalf("stage 5 must stage a full session recreate, got %+v", calls)
	}

	h.tick(10*time.Minute, true, 3) // stage 6: channel switch via avoid list
	if !h.w.avoid.IsAvoided("chan") {
		t.Fatal("stage 6 must temporarily exclude the farming channel")
	}
	if st := h.state(t); st.Status != ProgressRecovering {
		t.Fatalf("mid-pipeline status must be recovering, got %+v", st)
	}

	h.tick(10*time.Minute, true, 3) // stage 7: terminal notification
	st := h.state(t)
	if st.Status != ProgressStalled {
		t.Fatalf("exhausted pipeline must end STALLED, got %+v", st)
	}
	if stalled := h.notifier.byKind("stalled"); len(stalled) != 1 {
		t.Fatalf("exactly one critical notification per episode, got %+v", stalled)
	}

	// Further ticks: no more stages, no repeat notification (finite pipeline).
	h.tick(10*time.Minute, true, 3)
	h.tick(10*time.Minute, true, 3)
	if stalled := h.notifier.byKind("stalled"); len(stalled) != 1 {
		t.Fatalf("stalled notification must not repeat, got %d", len(stalled))
	}
	if syncNow, triggered := h.drops.counts(); syncNow != 1 || triggered != 1 {
		t.Fatalf("no recovery work after exhaustion, got syncNow=%d triggered=%d", syncNow, triggered)
	}
}

// TestWatchdogProgressResumeResetsAndNotifiesRecovered: resumed minutes reset
// the episode from any depth, clear the avoid entry, and — because the stalled
// notification fired — send exactly one recovered notification.
func TestWatchdogProgressResumeResetsAndNotifiesRecovered(t *testing.T) {
	h := newWatchdogHarness(t)
	h.driveToStall(t)
	for i := 0; i < 6; i++ { // run the pipeline to exhaustion
		h.tick(10*time.Minute, true, 3)
	}
	if st := h.state(t); st.Status != ProgressStalled || !h.w.avoid.IsAvoided("chan") {
		t.Fatalf("setup: expected exhausted stall with avoided channel, got %+v", st)
	}

	h.campaign.Drops[0].CurrentMinutesWatched = 130
	h.tick(5*time.Minute, true, 1)

	st := h.state(t)
	if st.Status != ProgressHealthy || st.RecoveryStage != 0 {
		t.Fatalf("resumed progress must fully reset the episode, got %+v", st)
	}
	if h.w.avoid.IsAvoided("chan") {
		t.Fatal("resumed progress must clear the avoid entry")
	}
	if rec := h.notifier.byKind("recovered"); len(rec) != 1 {
		t.Fatalf("expected exactly one recovered notification, got %+v", rec)
	}

	// A later healthy advance must NOT re-notify recovered.
	h.campaign.Drops[0].CurrentMinutesWatched = 140
	h.tick(5*time.Minute, true, 1)
	if rec := h.notifier.byKind("recovered"); len(rec) != 1 {
		t.Fatalf("recovered notification must fire only on the transition, got %d", len(rec))
	}
}

func TestWatchdogRecoveryCooldownBlocksStages(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.UpdateSettings(WatchdogConfig{
		Enabled: true, StallDelay: 20 * time.Minute, StallConfirmations: 3,
		RecoveryCooldown: 30 * time.Minute, AvoidTTL: time.Hour, Rearm: 6 * time.Hour,
	})
	h.driveToStall(t)

	// 10 minutes later: cooldown (30m) still active — stage 2 must NOT run.
	h.tick(10*time.Minute, true, 3)
	if syncNow, _ := h.drops.counts(); syncNow != 0 {
		t.Fatalf("cooldown must block the next stage, got syncNow=%d", syncNow)
	}
	// 25 more minutes: cooldown elapsed — stage 2 runs.
	h.tick(25*time.Minute, true, 3)
	if syncNow, _ := h.drops.counts(); syncNow != 1 {
		t.Fatalf("expected stage 2 after the cooldown, got syncNow=%d", syncNow)
	}
}

func TestWatchdogRearmRestartsPipelineWithoutRenotifying(t *testing.T) {
	h := newWatchdogHarness(t)
	h.driveToStall(t)
	for i := 0; i < 6; i++ {
		h.tick(10*time.Minute, true, 3)
	}
	if _, triggered := h.drops.counts(); triggered != 1 {
		t.Fatal("setup: expected one full pipeline pass")
	}

	// Past the rearm window the pipeline restarts from stage 1...
	h.tick(7*time.Hour, true, 3)
	if _, triggered := h.drops.counts(); triggered != 2 {
		t.Fatalf("expected a re-armed stage 1 after the rearm window, got %d", triggered)
	}
	// ...but the state never left stalled, so no second critical notification
	// even after the second pass exhausts.
	for i := 0; i < 7; i++ {
		h.tick(10*time.Minute, true, 3)
	}
	if stalled := h.notifier.byKind("stalled"); len(stalled) != 1 {
		t.Fatalf("re-armed exhaustion must not re-notify (no transition), got %d", len(stalled))
	}
}

// TestWatchdogOneStagePerPass: with two independently stalled drops, one
// evaluation pass executes at most one recovery stage (API-load bound).
func TestWatchdogOneStagePerPass(t *testing.T) {
	h := newWatchdogHarness(t)

	game2 := &models.Game{ID: "g2", Name: "Game Two"}
	campaign2 := &models.Campaign{
		ID: "camp-2", Name: "Campaign Two", Game: game2,
		Status:  models.CampaignActive,
		StartAt: h.now.Add(-24 * time.Hour), EndAt: h.now.Add(48 * time.Hour),
		Drops: []*models.Drop{{
			ID: "drop-2", Name: "Drop Two",
			MinutesRequired: 240, CurrentMinutesWatched: 50,
			StartAt: h.now.Add(-24 * time.Hour), EndAt: h.now.Add(48 * time.Hour),
		}},
	}
	streamer2 := models.NewStreamer("chan2", models.StreamerSettings{ClaimDrops: true})
	streamer2.ChannelID = "cid-chan2"
	streamer2.Stream.Update("b2", "t", game2, nil, 50)
	streamer2.Stream.SetCampaigns([]*models.Campaign{campaign2})

	h.drops.mu.Lock()
	h.drops.campaigns = append(h.drops.campaigns, campaign2)
	h.drops.mu.Unlock()
	h.watch.mu.Lock()
	h.watch.slots = []string{"chan", "chan2"}
	h.watch.mu.Unlock()
	h.watch.addSuccesses("chan2", 0)

	prevResolver := h.w.resolver
	h.w.resolver = func(login string) *models.Streamer {
		if login == "chan2" {
			return streamer2
		}
		return prevResolver(login)
	}

	h.w.evaluate(h.now)
	for i := 0; i < 4; i++ {
		h.now = h.now.Add(10 * time.Minute)
		h.drops.observe(h.now, "")
		h.watch.addSuccesses("chan", 3)
		h.watch.addSuccesses("chan2", 3)
		h.w.evaluate(h.now)
	}

	// Both drops are stall-eligible, but only ONE stage may have run in the
	// pass that confirmed them.
	if _, triggered := h.drops.counts(); triggered != 1 {
		t.Fatalf("one evaluation pass must execute at most one recovery stage, got %d", triggered)
	}
}

// --- per-gate negatives: each unmet condition alone must block confirmation ---

// stallReady drives the harness to the brink: every threshold met, so the
// NEXT evaluation would start recovery unless the gate under test blocks it.
func stallReady(t *testing.T, h *watchdogHarness) {
	t.Helper()
	h.w.evaluate(h.now)
	// Two observed ticks: delay (20m) and reports (6) already suffice, but only
	// 2 of the 3 required observations — the very next observed tick confirms
	// unless the gate under test blocks it.
	for i := 0; i < 2; i++ {
		h.tick(10*time.Minute, true, 3)
	}
	st := h.state(t)
	if st.Status != ProgressHealthy {
		t.Fatalf("setup must stop just short of recovery, got %+v", st)
	}
}

func assertNoRecovery(t *testing.T, h *watchdogHarness, wantDetail string) {
	t.Helper()
	if syncNow, triggered := h.drops.counts(); syncNow != 0 || triggered != 0 {
		t.Fatalf("gate must block all recovery work, got syncNow=%d triggered=%d", syncNow, triggered)
	}
	if len(h.watch.refreshCalls()) != 0 {
		t.Fatal("gate must block session refreshes")
	}
	if wantDetail != "" {
		snap := h.w.Snapshot()
		if len(snap.Drops) == 1 && !strings.Contains(snap.Drops[0].Detail, wantDetail) {
			t.Fatalf("expected the failing gate to be explained (want %q), got %q", wantDetail, snap.Drops[0].Detail)
		}
	}
}

func TestWatchdogGateTwitchOutage(t *testing.T) {
	h := newWatchdogHarness(t)
	stallReady(t, h)
	h.center.Record(Signal{Name: SignalGQLAPI, Status: StatusFailed})
	h.tick(10*time.Minute, true, 3)
	assertNoRecovery(t, h, "connectivity is degraded")
}

func TestWatchdogGateNoFarmingChannel(t *testing.T) {
	h := newWatchdogHarness(t)
	stallReady(t, h)
	h.watch.mu.Lock()
	h.watch.slots = nil // channel lost its slot (rotation/offline)
	h.watch.mu.Unlock()
	h.tick(10*time.Minute, true, 3)
	assertNoRecovery(t, h, "no slotted channel")
}

func TestWatchdogGateNotWatching(t *testing.T) {
	h := newWatchdogHarness(t)
	stallReady(t, h)
	h.watch.mu.Lock()
	h.watch.watching = map[string]bool{"chan": false} // snapshot lists it, but it is not held
	h.watch.mu.Unlock()
	h.tick(10*time.Minute, true, 3)
	assertNoRecovery(t, h, "does not hold a watch slot")
}

func TestWatchdogGateGameSwitched(t *testing.T) {
	h := newWatchdogHarness(t)
	stallReady(t, h)
	h.streamer.Stream.Update("b1", "t", &models.Game{ID: "other", Name: "Other Game"}, nil, 100)
	h.tick(10*time.Minute, true, 3)
	assertNoRecovery(t, h, "switched away")
}

func TestWatchdogGateEligibilityLost(t *testing.T) {
	h := newWatchdogHarness(t)
	stallReady(t, h)
	h.streamer.Stream.SetCampaigns(nil) // intersection no longer assigns the campaign
	h.tick(10*time.Minute, true, 3)
	assertNoRecovery(t, h, "no longer assigned")
}

func TestWatchdogGatePreconditionsNotMet(t *testing.T) {
	h := newWatchdogHarness(t)
	stallReady(t, h)
	notMet := false
	h.campaign.Drops[0].HasPreconditionsMet = &notMet
	h.tick(10*time.Minute, true, 3)
	assertNoRecovery(t, h, "preconditions not met")
}

func TestWatchdogSkipsClaimableClaimedAndEnded(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.evaluate(h.now)

	// Claimable: fully progressed — the claim flow's job, never a stall.
	h.campaign.Drops[0].IsClaimable = true
	h.tick(10*time.Minute, true, 3)
	if snap := h.w.Snapshot(); len(snap.Drops) != 0 {
		t.Fatalf("claimable drop must not be tracked, got %+v", snap.Drops)
	}
	h.campaign.Drops[0].IsClaimable = false

	// Claimed.
	h.campaign.Drops[0].IsClaimed = true
	h.tick(10*time.Minute, true, 3)
	if snap := h.w.Snapshot(); len(snap.Drops) != 0 {
		t.Fatalf("claimed drop must not be tracked, got %+v", snap.Drops)
	}
	h.campaign.Drops[0].IsClaimed = false

	// Campaign ended.
	h.campaign.EndAt = h.now.Add(-time.Minute)
	h.tick(10*time.Minute, true, 3)
	if snap := h.w.Snapshot(); len(snap.Drops) != 0 {
		t.Fatalf("ended campaign must not be tracked, got %+v", snap.Drops)
	}
}

// TestWatchdogObservationGating: without NEW successful inventory
// observations the no-progress counter must not advance — neither on a stale
// timestamp nor on a failed observation — so a stall can never confirm on
// "could not check".
func TestWatchdogObservationGating(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.evaluate(h.now)

	// Plenty of time and reports, but zero completed observations.
	for i := 0; i < 5; i++ {
		h.tick(10*time.Minute, false, 3)
	}
	assertNoRecovery(t, h, "")

	// Failed observations do not count either.
	for i := 0; i < 5; i++ {
		h.now = h.now.Add(10 * time.Minute)
		h.drops.observe(h.now, "inventory 502")
		h.watch.addSuccesses("chan", 3)
		h.w.evaluate(h.now)
	}
	assertNoRecovery(t, h, "")

	if st := h.state(t); st.NoProgressObs != 0 {
		t.Fatalf("no-progress observations must stay 0 without clean syncs, got %d", st.NoProgressObs)
	}
}

// TestWatchdogReportThresholdGates: without enough delivered minute-watched
// reports (we cannot prove we were watching) a stall must not confirm.
func TestWatchdogReportThresholdGates(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.evaluate(h.now)
	for i := 0; i < 5; i++ {
		h.tick(10*time.Minute, true, 0) // observations but no delivered reports
	}
	assertNoRecovery(t, h, "")
}

// TestWatchdogStallDelayGates: observations and reports alone are not enough
// before the wall-clock stall delay has passed.
func TestWatchdogStallDelayGates(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.evaluate(h.now)
	// Three clean observations with reports, but only 15 total minutes (< 20m).
	for i := 0; i < 3; i++ {
		h.tick(5*time.Minute, true, 3)
	}
	assertNoRecovery(t, h, "")
}

func TestWatchdogDisabledClearsState(t *testing.T) {
	h := newWatchdogHarness(t)
	h.driveToStall(t)

	h.w.UpdateSettings(WatchdogConfig{Enabled: false})
	h.tick(time.Minute, false, 0)

	snap := h.w.Snapshot()
	if snap.Enabled || len(snap.Drops) != 0 {
		t.Fatalf("disabling must clear and mark the snapshot disabled, got %+v", snap)
	}
}

// TestWatchdogChannelSwitchRebaselines: after the farming channel changes the
// delivery accounting re-baselines against the new channel.
func TestWatchdogChannelSwitchRebaselines(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.evaluate(h.now)
	h.tick(10*time.Minute, true, 5)
	if st := h.state(t); st.ReportsSinceProgress != 5 {
		t.Fatalf("expected 5 reports accounted, got %+v", st)
	}

	// The campaign moves to a different channel.
	streamer2 := models.NewStreamer("chan2", models.StreamerSettings{ClaimDrops: true})
	streamer2.ChannelID = "cid-chan2"
	streamer2.Stream.Update("b2", "t", h.campaign.Game, nil, 10)
	streamer2.Stream.SetCampaigns([]*models.Campaign{h.campaign})
	h.watch.mu.Lock()
	h.watch.slots = []string{"chan2"}
	h.watch.mu.Unlock()
	h.watch.addSuccesses("chan2", 100) // pre-existing counter on the new channel
	prev := h.w.resolver
	h.w.resolver = func(login string) *models.Streamer {
		if login == "chan2" {
			return streamer2
		}
		return prev(login)
	}

	h.tick(10*time.Minute, true, 0)
	st := h.state(t)
	if st.Channel != "chan2" || st.ReportsSinceProgress != 0 {
		t.Fatalf("expected a re-baselined channel switch, got %+v", st)
	}
}

// TestWatchdogRebaselineDefersUntilStatsAvailable: when the farming channel
// changes but the watcher has not published stats for the new channel yet, the
// re-baseline read misses. Pre-fix the baseline was fixed at 0, so a later tick
// whose read succeeded counted the channel's entire lifetime success count as
// progress-since-baseline (ReportsSinceProgress jumping to the full counter).
// The baseline must instead be adopted from the first successful read.
func TestWatchdogRebaselineDefersUntilStatsAvailable(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.evaluate(h.now)
	h.tick(10*time.Minute, true, 5)
	if st := h.state(t); st.ReportsSinceProgress != 5 {
		t.Fatalf("expected 5 reports accounted, got %+v", st)
	}

	// The campaign moves to chan2, but no stats have been published for it yet:
	// ReportStats("chan2") misses on this tick.
	streamer2 := models.NewStreamer("chan2", models.StreamerSettings{ClaimDrops: true})
	streamer2.ChannelID = "cid-chan2"
	streamer2.Stream.Update("b2", "t", h.campaign.Game, nil, 10)
	streamer2.Stream.SetCampaigns([]*models.Campaign{h.campaign})
	h.watch.mu.Lock()
	h.watch.slots = []string{"chan2"}
	h.watch.mu.Unlock()
	prev := h.w.resolver
	h.w.resolver = func(login string) *models.Streamer {
		if login == "chan2" {
			return streamer2
		}
		return prev(login)
	}

	h.tick(10*time.Minute, true, 0)
	if st := h.state(t); st.Channel != "chan2" || st.ReportsSinceProgress != 0 {
		t.Fatalf("expected re-baseline reset with the read still missing, got %+v", st)
	}

	// The watcher now surfaces chan2 with a large pre-existing lifetime counter.
	// That whole counter must NOT be counted as progress-since-baseline.
	h.watch.addSuccesses("chan2", 100)
	h.tick(10*time.Minute, true, 0)
	if st := h.state(t); st.ReportsSinceProgress != 0 {
		t.Fatalf("first successful read must set the baseline, not count lifetime successes, got %+v", st)
	}

	// From the adopted baseline, only genuinely new successes accrue.
	h.watch.addSuccesses("chan2", 4)
	h.tick(10*time.Minute, true, 0)
	if st := h.state(t); st.ReportsSinceProgress != 4 {
		t.Fatalf("expected 4 reports past the adopted baseline, got %+v", st)
	}
}

// --- stall-evidence window (adversarial-review findings) ---

// TestWatchdogGateFailureResetsStallEvidence reproduces review scenario A: the
// farming channel is offline for 40 minutes while clean inventory syncs keep
// completing. Pre-fix, the accrued observations and stale delay clock survived
// the gap and a stall confirmed ~5 minutes after farming resumed — inside
// Twitch's ~15-minute crediting batch. The evidence window must instead
// restart on resume, and a genuine stall must still confirm once a FULL fresh
// window (delay + observations + reports) accrues.
func TestWatchdogGateFailureResetsStallEvidence(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.evaluate(h.now)

	// 40 offline minutes: gates fail, clean observations keep completing.
	h.watch.mu.Lock()
	h.watch.slots = nil
	h.watch.mu.Unlock()
	for i := 0; i < 4; i++ {
		h.tick(10*time.Minute, true, 0)
	}

	// The slot returns and five reports land almost immediately — the exact
	// pre-fix false-positive moment.
	h.watch.mu.Lock()
	h.watch.slots = []string{"chan"}
	h.watch.mu.Unlock()
	h.tick(5*time.Minute, true, 5)
	assertNoRecovery(t, h, "")
	if st := h.state(t); st.Status != ProgressHealthy || st.NoProgressObs != 0 {
		t.Fatalf("evidence must restart on farming resume, got %+v", st)
	}

	// Fresh evidence accrues: two more observed ticks are still short of the
	// confirmation count...
	h.tick(10*time.Minute, true, 3)
	h.tick(10*time.Minute, true, 3)
	assertNoRecovery(t, h, "")
	// ...and the third completes delay+observations+reports: the true positive
	// is preserved, just measured from the resume.
	h.tick(10*time.Minute, true, 3)
	if _, triggered := h.drops.counts(); triggered != 1 {
		t.Fatalf("a genuine stall with fresh evidence must still confirm, got triggered=%d", triggered)
	}
}

// TestWatchdogIneligibleWindowEvidenceDiscarded reproduces review scenario B
// (the sticky-channel variant): the SAME channel keeps its slot while playing
// another game for 40 minutes with reports flowing. Pre-fix the delivery
// counter never re-baselined (channel unchanged), so a stall confirmed on the
// first pass after eligibility returned with zero minutes actually farmed.
func TestWatchdogIneligibleWindowEvidenceDiscarded(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.evaluate(h.now)

	game := h.campaign.Game
	h.streamer.Stream.Update("b1", "t", &models.Game{ID: "other", Name: "Other Game"}, nil, 100)
	for i := 0; i < 4; i++ {
		h.tick(10*time.Minute, true, 3) // 12 reports delivered while ineligible
	}

	// The game returns: the first eligible pass must not confirm, and the
	// reports delivered during the ineligible window must be discarded.
	h.streamer.Stream.Update("b1", "t", game, nil, 100)
	h.tick(10*time.Minute, true, 3)
	assertNoRecovery(t, h, "")
	if st := h.state(t); st.ReportsSinceProgress != 0 {
		t.Fatalf("reports accrued while ineligible must not survive, got %+v", st)
	}
}

// TestWatchdogObservationCursorSeeding pins the phantom-observation fixes: a
// sync completed before tracking began, the sync whose data showed the last
// progress, and an unchanged sync timestamp must each count zero times.
func TestWatchdogObservationCursorSeeding(t *testing.T) {
	h := newWatchdogHarness(t)

	// A sync completed BEFORE tracking began must not become the first
	// no-progress observation.
	h.drops.observe(h.now, "")
	h.w.evaluate(h.now)
	h.tick(time.Minute, false, 1)
	if st := h.state(t); st.NoProgressObs != 0 {
		t.Fatalf("pre-tracking sync counted as an observation: %+v", st)
	}

	// Dedup: the SAME sync timestamp seen on several passes counts once.
	h.tick(time.Minute, true, 1)
	h.tick(time.Minute, false, 1)
	h.tick(time.Minute, false, 1)
	if st := h.state(t); st.NoProgressObs != 1 {
		t.Fatalf("one sync must count exactly once, got %+v", st)
	}

	// After a progress reset, the sync that SHOWED the progress must not be
	// re-counted against the fresh episode.
	h.campaign.Drops[0].CurrentMinutesWatched = 130
	h.tick(time.Minute, true, 1) // progress observed and reset
	h.tick(time.Minute, false, 1)
	if st := h.state(t); st.NoProgressObs != 0 {
		t.Fatalf("the progress-showing sync was re-counted after the reset: %+v", st)
	}
}

// TestWatchdogGateInventoryObservability: while inventory reads fail — or none
// complete within the stall-delay window — the drop's progress is unobservable
// and a stall must not confirm no matter how much earlier evidence existed.
func TestWatchdogGateInventoryObservability(t *testing.T) {
	t.Run("failing reads", func(t *testing.T) {
		h := newWatchdogHarness(t)
		h.w.evaluate(h.now)
		for i := 0; i < 3; i++ {
			h.tick(4*time.Minute, true, 2) // 3 clean observations early
		}
		for i := 0; i < 4; i++ { // then the inventory starts erroring
			h.now = h.now.Add(5 * time.Minute)
			h.drops.observe(h.now, "inventory 502")
			h.watch.addSuccesses("chan", 3)
			h.w.evaluate(h.now)
		}
		assertNoRecovery(t, h, "unobservable")
	})

	t.Run("stale reads", func(t *testing.T) {
		h := newWatchdogHarness(t)
		h.w.evaluate(h.now)
		h.tick(2*time.Minute, true, 2)
		for i := 0; i < 5; i++ { // syncs stop entirely while time passes the delay
			h.tick(10*time.Minute, false, 3)
		}
		assertNoRecovery(t, h, "no inventory observation")
	})
}

// TestWatchdogTransientGateBlipPausesButKeepsStage: a one-tick gate failure
// mid-recovery must pause the pipeline (stage survives — no restart from
// stage 1) while discarding the evidence, so the next stage runs only after a
// full fresh window.
func TestWatchdogTransientGateBlipPausesButKeepsStage(t *testing.T) {
	h := newWatchdogHarness(t)
	h.driveToStall(t) // stage 1 executed

	h.watch.mu.Lock()
	h.watch.watching = map[string]bool{"chan": false}
	h.watch.mu.Unlock()
	h.tick(10*time.Minute, true, 3) // gate blip
	if st := h.state(t); st.RecoveryStage != 1 {
		t.Fatalf("a gate blip must not reset the recovery stage, got %+v", st)
	}

	h.watch.mu.Lock()
	h.watch.watching = nil
	h.watch.mu.Unlock()

	// Evidence restarts on the first restored pass (its sync seeds the cursor,
	// counting as observation zero); stage 2 must wait out the full fresh
	// window: three further observed ticks (delay 30m ≥ 20m, obs 3, reports 9).
	h.tick(10*time.Minute, true, 3) // evidence window starts
	h.tick(10*time.Minute, true, 3) // obs=1
	h.tick(10*time.Minute, true, 3) // obs=2, delay 20m — still short
	if syncNow, _ := h.drops.counts(); syncNow != 0 {
		t.Fatalf("stage 2 must wait for fresh evidence after a blip, got syncNow=%d", syncNow)
	}
	h.tick(10*time.Minute, true, 3) // obs=3 — window complete
	if syncNow, _ := h.drops.counts(); syncNow != 1 {
		t.Fatalf("stage 2 must run once fresh evidence accrues, got syncNow=%d", syncNow)
	}
	if st := h.state(t); st.RecoveryStage != 2 {
		t.Fatalf("pipeline must resume from stage 2, got %+v", st)
	}
}

// TestWatchdogCleanupClosesEscalatedEpisode: an escalated episode whose drop
// leaves the tracked set (e.g. Twitch credited invisibly and it jumped to
// claimable) must not leave dangling effects — the avoided channel is
// unblocked and the standing critical alert is closed exactly once.
func TestWatchdogCleanupClosesEscalatedEpisode(t *testing.T) {
	h := newWatchdogHarness(t)
	h.driveToStall(t)
	for i := 0; i < 6; i++ {
		h.tick(10*time.Minute, true, 3) // exhaust: stalled notified, channel avoided
	}
	if !h.w.avoid.IsAvoided("chan") || len(h.notifier.byKind("stalled")) != 1 {
		t.Fatal("setup: expected an exhausted, notified, avoided episode")
	}

	h.campaign.Drops[0].CurrentMinutesWatched = 240
	h.campaign.Drops[0].IsClaimable = true
	h.tick(time.Minute, true, 0)

	if snap := h.w.Snapshot(); len(snap.Drops) != 0 {
		t.Fatalf("claimable drop must leave the tracked set, got %+v", snap.Drops)
	}
	if h.w.avoid.IsAvoided("chan") {
		t.Fatal("the avoid entry must be cleared when the episode closes")
	}
	if rec := h.notifier.byKind("recovered"); len(rec) != 1 {
		t.Fatalf("the standing stall alert must be closed exactly once, got %+v", rec)
	}
	h.tick(time.Minute, true, 0)
	if rec := h.notifier.byKind("recovered"); len(rec) != 1 {
		t.Fatalf("the close notification must not repeat, got %d", len(rec))
	}
}

// TestWatchdogGateTwitchOutageAllSignals: every outage input — OAuth, GQL,
// PubSub, watch transport — must individually block confirmation.
func TestWatchdogGateTwitchOutageAllSignals(t *testing.T) {
	for _, signal := range []string{SignalOAuth, SignalGQLAPI, SignalPubSub, SignalWatchTransport} {
		t.Run(signal, func(t *testing.T) {
			h := newWatchdogHarness(t)
			stallReady(t, h)
			h.center.Record(Signal{Name: signal, Status: StatusFailed})
			h.tick(10*time.Minute, true, 3)
			assertNoRecovery(t, h, "connectivity is degraded")
		})
	}
}

// TestWatchdogSkipsInactiveStatusAndClosedDropWindow: a campaign whose status
// is not ACTIVE (even with a future end date) and a drop outside its own time
// window must not be tracked at all.
func TestWatchdogSkipsInactiveStatusAndClosedDropWindow(t *testing.T) {
	h := newWatchdogHarness(t)

	h.campaign.Status = models.CampaignExpired // EndAt still in the future
	h.tick(time.Minute, true, 3)
	if snap := h.w.Snapshot(); len(snap.Drops) != 0 {
		t.Fatalf("non-ACTIVE campaign must not be tracked, got %+v", snap.Drops)
	}
	h.campaign.Status = models.CampaignActive

	h.campaign.Drops[0].StartAt = h.now.Add(time.Hour) // window not open yet
	h.tick(time.Minute, true, 3)
	if snap := h.w.Snapshot(); len(snap.Drops) != 0 {
		t.Fatalf("a drop outside its time window must not be tracked, got %+v", snap.Drops)
	}
}

// --- signal composition ---

func TestWatchdogProgressSignal(t *testing.T) {
	h := newWatchdogHarness(t)

	// Nothing tracked yet -> idle.
	h.w.publish(ProgressSnapshot{Enabled: true, EvaluatedAt: h.now})
	if sig := h.w.ProgressSignal(h.now); sig.Status != StatusIdle {
		t.Fatalf("expected idle with nothing tracked, got %+v", sig)
	}

	// Healthy drops -> ok.
	h.w.evaluate(h.now)
	if sig := h.w.ProgressSignal(h.now); sig.Status != StatusOK {
		t.Fatalf("expected ok while healthy, got %+v", sig)
	}

	// Recovering -> ok with a recovering stage marker.
	h.driveToStall(t)
	sig := h.w.ProgressSignal(h.now)
	if sig.Status != StatusOK || !strings.HasPrefix(sig.Stage, "recovering:") {
		t.Fatalf("expected ok+recovering stage, got %+v", sig)
	}

	// Exhausted -> stalled.
	for i := 0; i < 6; i++ {
		h.tick(10*time.Minute, true, 3)
	}
	sig = h.w.ProgressSignal(h.now)
	if sig.Status != StatusStalled || sig.ErrorCode != "drop_progress_stalled" {
		t.Fatalf("expected the stalled signal, got %+v", sig)
	}
	for _, secret := range []string{"http://", "https://", "sig=", "token="} {
		if strings.Contains(sig.Detail, secret) {
			t.Fatalf("signal detail leaked %q: %q", secret, sig.Detail)
		}
	}
}

// --- avoid list ---

func TestAvoidListExpiryAndExtension(t *testing.T) {
	base := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	now := base
	a := NewAvoidList()
	a.now = func() time.Time { return now }

	a.Avoid("chan", base.Add(time.Hour), "stalled")
	if !a.IsAvoided("chan") {
		t.Fatal("expected the channel to be avoided")
	}

	// A shorter re-avoid must not shrink the window.
	a.Avoid("chan", base.Add(time.Minute), "again")
	now = base.Add(30 * time.Minute)
	if !a.IsAvoided("chan") {
		t.Fatal("a shorter re-avoid must not shorten the exclusion")
	}

	// Expiry.
	now = base.Add(2 * time.Hour)
	if a.IsAvoided("chan") {
		t.Fatal("expected the exclusion to expire")
	}
	if entries := a.Entries(); len(entries) != 0 {
		t.Fatalf("expired entries must be pruned, got %+v", entries)
	}

	// Clear.
	a.Avoid("chan", now.Add(time.Hour), "stalled")
	a.Clear("chan")
	if a.IsAvoided("chan") {
		t.Fatal("expected Clear to lift the exclusion")
	}
}
