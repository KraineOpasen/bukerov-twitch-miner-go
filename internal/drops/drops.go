package drops

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// twitchClient is the slice of *api.TwitchClient the drops tracker actually
// uses, narrowed to an interface so the full campaign-sync pipeline can be
// exercised end-to-end in tests (previously only the pure buildTrackedCampaign
// helper was testable, so a regression that emptied the live sync path went
// unnoticed). Satisfied by *api.TwitchClient.
type twitchClient interface {
	PostGQL(op constants.GQLOperation) (map[string]interface{}, error)
	GetDropCampaignDetails(campaignID string) (map[string]interface{}, error)
	ClaimDrop(drop *models.Drop) (bool, error)
}

// SyncStatus is a snapshot of the most recent campaign sync. It exists so the
// debug snapshot (and any future health check) can tell whether the sync ran,
// what Twitch's dashboard returned, how many campaigns were recovered from the
// inventory's in-progress list, how many ended up tracked, and whether the last
// run errored - none of which was observable before, since every sync
// diagnostic was DEBUG-only and a production container runs without -debug.
type SyncStatus struct {
	LastSyncAt         time.Time
	Runs               int
	DashboardCampaigns int
	RecoveredCampaigns int
	TrackedCampaigns   int
	LastError          string

	// Lightweight progress-sync observability (Stage 3). ProgressLastSyncAt is
	// stamped only when an inventory read actually completed, so the progress
	// watchdog can require "a fresh successful observation" before counting a
	// no-progress interval — and an inventory outage (previously swallowed
	// silently) is now visible via ProgressLastError.
	ProgressRuns       int
	ProgressLastSyncAt time.Time
	ProgressLastError  string
}

type DropsTracker struct {
	client    twitchClient
	streamers []*models.Streamer
	settings  config.RateLimitSettings

	campaigns []*models.Campaign

	// upcomingCampaigns holds campaigns Twitch's dashboard returned that have not
	// started yet (start_at in the future). They are display-only for the Drops
	// page "Upcoming" tab and NEVER enter the active farm set — they cannot be
	// farmed before their official start. Guarded by mu.
	upcomingCampaigns []*models.Campaign

	// catalog, when set, durably records every observed campaign (current +
	// upcoming) so the "Past" tab can show campaigns that have since expired.
	// Set once at wiring time before the loops start; nil disables cataloging.
	catalog *CampaignCatalog

	// Sync bookkeeping for SyncStatus (and LastSync); all guarded by mu.
	syncRuns           int
	lastSyncAt         time.Time
	lastDashboardCount int
	lastRecoveredCount int
	lastTrackedCount   int
	lastSyncErr        string

	// Lightweight progress-sync bookkeeping (see SyncStatus); guarded by mu.
	progressRuns       int
	progressLastSyncAt time.Time
	progressLastErr    string

	// fullSyncMu serializes syncCampaigns between the background loop and
	// SyncNow (the progress watchdog's forced-resync recovery stage), so two
	// full syncs never interleave their network calls and campaign swaps.
	fullSyncMu sync.Mutex

	// dropBlacklist holds case-insensitive keywords; a campaign is skipped
	// during rotation prioritization when any of its drop/reward names matches
	// one. Guarded by mu so it can be updated at runtime from the Settings page.
	dropBlacklist []string

	// resync wakes the lightweight progress-sync loop early (buffered to 1 so
	// bursts of triggers between syncs coalesce into a single extra run). Fed by
	// TriggerProgressSync, e.g. right after a watched minute is reported so the
	// Drops page reflects the new progress within seconds instead of waiting out
	// DropProgressSyncInterval.
	resync chan struct{}

	// logMu guards the assignment/progress de-dup state below. Both the full
	// campaign sync (loop) and the lightweight progress sync (progressLoop) call
	// updateStreamerCampaigns from separate goroutines, so the maps it reads and
	// rewrites must not be touched concurrently.
	logMu sync.Mutex
	// loggedRestrictedAssignments remembers which channel-restricted assignments
	// (keyed by streamer+campaign) have already been announced at INFO, so the
	// announcement fires only on a real change -- a campaign newly assigned to a
	// streamer, or one reassigned after dropping off -- and not on every 2-minute
	// progress sync.
	loggedRestrictedAssignments map[string]struct{}
	// loggedProgressBucket remembers, per campaign, the last 5% progress
	// checkpoint already logged at INFO, so the compact progress line is throttled
	// to checkpoint crossings instead of firing on every lightweight sync.
	loggedProgressBucket map[string]int

	ctx    context.Context
	cancel context.CancelFunc
	// loopsDone is closed when BOTH loops (sync + progress) have exited;
	// Stop waits on it (bounded by stopJoinTimeout) so in-flight catalog and
	// claim-annotation writes drain before the database is closed.
	loopsDone chan struct{}

	// onDropClaimed, if set, is invoked with the drop name each time a drop is
	// successfully claimed, so a listener (the miner) can durably record the
	// claim for the daily summary without the tracker importing analytics. Set
	// once at wiring time before the loops start; read lock-free.
	onDropClaimed func(dropName string)

	// intervalUnit scales the configured sync intervals (minutes in
	// production). Set once at construction and never mutated, so both loops
	// read it without the lock; tests set it to a sub-second unit to exercise
	// the cadence without waiting real minutes.
	intervalUnit time.Duration

	mu sync.RWMutex
}

func NewDropsTracker(
	client twitchClient,
	streamers []*models.Streamer,
	settings config.RateLimitSettings,
	dropBlacklist []string,
) *DropsTracker {
	return &DropsTracker{
		client:        client,
		streamers:     streamers,
		settings:      settings,
		dropBlacklist: dropBlacklist,
		resync:        make(chan struct{}, 1),
		intervalUnit:  time.Minute,
	}
}

// UpdateBlacklist replaces the drop-name blacklist. Called when the operator
// changes it on the Settings page so the new keywords take effect on the next
// campaign sync without a restart.
func (d *DropsTracker) UpdateBlacklist(dropBlacklist []string) {
	d.mu.Lock()
	d.dropBlacklist = dropBlacklist
	d.mu.Unlock()
}

// UpdateSettings replaces the rate-limit settings at runtime (e.g. from the
// Settings page) so a changed CampaignSyncInterval/DropProgressSyncInterval
// takes effect on the next tick of the respective loop without a restart.
func (d *DropsTracker) UpdateSettings(settings config.RateLimitSettings) {
	d.mu.Lock()
	d.settings = settings
	d.mu.Unlock()
}

// stopJoinTimeout bounds how long Stop waits for the sync/progress loops to
// drain their in-flight iteration (which may be writing the drops catalog or
// claim annotations) before giving up, so a hung loop can never block
// shutdown indefinitely. Package variable so tests can shrink it.
var stopJoinTimeout = 5 * time.Second

func (d *DropsTracker) Start(ctx context.Context) {
	d.mu.Lock()
	d.ctx, d.cancel = context.WithCancel(ctx)
	done := make(chan struct{})
	d.loopsDone = done
	d.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		d.loop()
	}()
	go func() {
		defer wg.Done()
		d.progressLoop()
	}()
	go func() {
		wg.Wait()
		close(done)
	}()
}

// TriggerProgressSync asks the progress-sync loop to run an immediate
// lightweight refresh instead of waiting out DropProgressSyncInterval. It is
// non-blocking and coalescing: if a run is already queued the trigger is
// dropped. Wired to the watcher so a freshly-reported watched minute is
// reflected on the Drops page within seconds.
func (d *DropsTracker) TriggerProgressSync() {
	select {
	case d.resync <- struct{}{}:
	default:
	}
}

// SyncNow runs a single campaign sync synchronously, refreshing the tracked
// campaign pool and SyncStatus before it returns. The background loop runs the
// same logic on each tick; this exposes it so a caller (the progress
// watchdog's forced-resync recovery stage, or a test) can force an immediate
// refresh without waiting out the sync interval. Serialized against the
// background loop via fullSyncMu inside syncCampaigns.
func (d *DropsTracker) SyncNow() {
	d.syncCampaigns()
}

// Stop cancels both loops and waits (bounded by stopJoinTimeout) for them to
// finish, so in-flight catalog/annotation writes complete before the caller
// proceeds to close the database.
func (d *DropsTracker) Stop() {
	d.mu.Lock()
	if d.cancel != nil {
		d.cancel()
	}
	done := d.loopsDone
	d.mu.Unlock()

	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(stopJoinTimeout):
		slog.Warn("Drops tracker loops did not finish within the stop timeout; proceeding with shutdown", "timeout", stopJoinTimeout)
	}
}

// Campaigns returns a snapshot of the currently tracked active drop
// campaigns (a copy of the slice, safe to read concurrently). The dashboard's
// Drops page uses this to render the campaign queue.
func (d *DropsTracker) Campaigns() []*models.Campaign {
	d.mu.RLock()
	defer d.mu.RUnlock()

	campaigns := make([]*models.Campaign, len(d.campaigns))
	copy(campaigns, d.campaigns)
	return campaigns
}

// UpcomingCampaigns returns a snapshot of the campaigns Twitch's dashboard
// listed that have not started yet (display-only; never farmed).
func (d *DropsTracker) UpcomingCampaigns() []*models.Campaign {
	d.mu.RLock()
	defer d.mu.RUnlock()

	out := make([]*models.Campaign, len(d.upcomingCampaigns))
	copy(out, d.upcomingCampaigns)
	return out
}

// SetCatalog wires the durable campaign catalog. Set once before the sync loops
// start; when nil, cataloging is a no-op.
func (d *DropsTracker) SetCatalog(catalog *CampaignCatalog) {
	d.catalog = catalog
}

// LastSync reports when the campaign-sync pipeline last completed (zero if it
// has not run yet). Exposed for the debug snapshot.
func (d *DropsTracker) LastSync() time.Time {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.lastSyncAt
}

// SyncStatus returns a snapshot of the last campaign sync for the debug
// endpoint. Safe to call from any goroutine.
func (d *DropsTracker) SyncStatus() SyncStatus {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return SyncStatus{
		LastSyncAt:         d.lastSyncAt,
		Runs:               d.syncRuns,
		DashboardCampaigns: d.lastDashboardCount,
		RecoveredCampaigns: d.lastRecoveredCount,
		TrackedCampaigns:   d.lastTrackedCount,
		LastError:          d.lastSyncErr,
		ProgressRuns:       d.progressRuns,
		ProgressLastSyncAt: d.progressLastSyncAt,
		ProgressLastError:  d.progressLastErr,
	}
}

// recordSync updates the sync bookkeeping surfaced by SyncStatus.
func (d *DropsTracker) recordSync(dashboardCount, recoveredCount, trackedCount int, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.syncRuns++
	d.lastSyncAt = time.Now()
	d.lastDashboardCount = dashboardCount
	d.lastRecoveredCount = recoveredCount
	d.lastTrackedCount = trackedCount
	if err != nil {
		d.lastSyncErr = err.Error()
	} else {
		d.lastSyncErr = ""
	}
}

// recordProgressSync updates the lightweight progress-sync bookkeeping. A nil
// err means an inventory read completed (even if it reported no progress and
// nothing was republished) — the "fresh successful observation" the progress
// watchdog gates its no-progress counting on. Called only from syncProgress;
// the full sync intentionally does not stamp it, because its inventory step
// swallows errors internally and a failed read must never masquerade as a
// successful observation.
func (d *DropsTracker) recordProgressSync(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.progressRuns++
	d.progressLastSyncAt = time.Now()
	if err != nil {
		d.progressLastErr = err.Error()
	} else {
		d.progressLastErr = ""
	}
}

func (d *DropsTracker) loop() {
	d.mu.RLock()
	ctx := d.ctx
	d.mu.RUnlock()

	d.syncCampaigns()

	for {
		// Re-read the interval each cycle (fresh timer per iteration) so a
		// runtime UpdateSettings change to CampaignSyncInterval is adopted on the
		// next sync, instead of being pinned to the value read once at startup.
		// This mirrors progressLoop below and the "snapshot the current settings
		// at the start of each cycle" pattern the watcher (applyPendingSettings)
		// and the health loops already use. A fixed time.Ticker created once at
		// startup — the previous implementation — silently ignored the change,
		// contradicting UpdateSettings' documented contract.
		timer := time.NewTimer(d.campaignSyncInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			d.syncCampaigns()
		}
	}
}

// progressLoop runs the lightweight, inventory-only progress refresh on the
// (much shorter) DropProgressSyncInterval cadence, or immediately when
// TriggerProgressSync fires. It exists so the Drops page tracks Twitch's real
// drop progress within a minute or two instead of lagging up to a full
// CampaignSyncInterval behind, without paying for the full campaign-discovery
// pipeline (dashboard listing + per-campaign details fetches) every time.
func (d *DropsTracker) progressLoop() {
	d.mu.RLock()
	ctx := d.ctx
	d.mu.RUnlock()

	for {
		timer := time.NewTimer(d.progressSyncInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-d.resync:
			timer.Stop()
		}
		d.syncProgress()
	}
}

// campaignSyncInterval returns the configured full-sync cadence, guarded so a
// runtime UpdateSettings write can't race the read; it falls back to the
// built-in default when unset.
func (d *DropsTracker) campaignSyncInterval() time.Duration {
	d.mu.RLock()
	mins := d.settings.CampaignSyncInterval
	d.mu.RUnlock()
	if mins <= 0 {
		mins = 60
	}
	return time.Duration(mins) * d.intervalUnit
}

// progressSyncInterval returns the configured lightweight progress-sync
// cadence, guarded against a racing UpdateSettings write; it falls back to the
// built-in default when unset.
func (d *DropsTracker) progressSyncInterval() time.Duration {
	d.mu.RLock()
	mins := d.settings.DropProgressSyncInterval
	d.mu.RUnlock()
	if mins <= 0 {
		mins = 2
	}
	return time.Duration(mins) * d.intervalUnit
}

// syncProgress runs a lightweight, inventory-only refresh of the watched-minute
// progress of the already-tracked campaigns. Unlike syncCampaigns it issues a
// single Inventory GQL request and touches neither the ViewerDropsDashboard
// listing nor the per-campaign DropCampaignDetails calls, so it is cheap enough
// to run every couple of minutes (and on demand after a watched minute). It
// never adds, removes, or claims campaigns/drops -- discovery, claiming, and
// blacklist/claim-history filtering all stay with the full sync -- it only
// advances the progress counters of campaigns the full sync already published.
func (d *DropsTracker) syncProgress() {
	d.mu.RLock()
	existing := make([]*models.Campaign, len(d.campaigns))
	copy(existing, d.campaigns)
	d.mu.RUnlock()

	// Nothing tracked yet: the full sync hasn't populated the campaign pool, so
	// there's no progress to refresh and no reason to hit the network.
	if len(existing) == 0 {
		return
	}

	inventory, err := d.getInventory()
	if err != nil || inventory == nil {
		if err == nil {
			err = fmt.Errorf("empty inventory response")
		}
		// Previously swallowed silently — an inventory outage was invisible and
		// indistinguishable from "progress genuinely not moving". Record it so
		// the health center and the progress watchdog can tell the two apart.
		d.recordProgressSync(err)
		slog.Debug("Drops progress sync failed: could not read inventory", "error", err)
		return
	}

	inProgress, ok := inventory["dropCampaignsInProgress"].([]interface{})
	if !ok || inProgress == nil {
		// A fetched inventory without an in-progress list is a legitimate state
		// (tracked campaigns whose watching hasn't credited any minutes yet), so
		// this counts as a completed observation reporting zero progress.
		d.recordProgressSync(nil)
		return
	}

	progressByID := make(map[string][]interface{}, len(inProgress))
	for _, prog := range inProgress {
		progData, ok := prog.(map[string]interface{})
		if !ok {
			continue
		}
		id, _ := progData["id"].(string)
		if id == "" {
			continue
		}
		if drops, ok := progData["timeBasedDrops"].([]interface{}); ok {
			progressByID[id] = drops
		}
	}

	// Build a fresh slice so published campaigns stay immutable after they're
	// swapped in (the Drops page and directory discovery read them lock-free and
	// rely on that invariant). Only campaigns the inventory reports progress for
	// are cloned and advanced; the rest are carried over unchanged by pointer.
	updated := make([]*models.Campaign, len(existing))
	changed := false
	for i, c := range existing {
		drops, ok := progressByID[c.ID]
		if !ok {
			updated[i] = c
			continue
		}

		clone := c.Clone()
		// nil claim callback: this path is display-only. Claiming stays with the
		// full sync / claimAllDropsFromInventory so no network write happens on
		// the hot progress path.
		clone.SyncDrops(drops, nil)
		clone.ClearClaimedDrops()
		updated[i] = clone

		if !changed && progressDiffers(c, clone) {
			changed = true
		}
	}

	if !changed {
		// The inventory read completed and nothing moved — "checked and
		// unchanged" is exactly the observation the progress watchdog counts
		// stalls with.
		d.recordProgressSync(nil)
		return
	}

	d.mu.Lock()
	d.campaigns = updated
	d.mu.Unlock()

	// Re-point the streamers at the refreshed campaigns so watch-priority
	// decisions see the new progress too, exactly as the full sync does.
	d.updateStreamerCampaigns()

	// Stamp the observation only AFTER the refreshed campaigns are published:
	// a reader (the progress watchdog) that sees the new observation timestamp
	// must also see the new minutes, or a progress-carrying read could be
	// miscounted as a no-progress observation.
	d.recordProgressSync(nil)

	slog.Debug("Drops progress sync: refreshed tracked campaign progress from inventory",
		"campaigns", len(updated))
}

// progressDiffers reports whether the watched-minute progress (or the set of
// still-unclaimed drops) changed between the pre- and post-refresh campaign, so
// syncProgress only republishes -- and re-points streamers -- when something
// actually moved. Compared by drop ID so it is independent of ordering or of
// drops ClearClaimedDrops removed on the refreshed copy.
func progressDiffers(before, after *models.Campaign) bool {
	if len(before.Drops) != len(after.Drops) {
		return true
	}
	beforeMinutes := make(map[string]int, len(before.Drops))
	for _, drop := range before.Drops {
		beforeMinutes[drop.ID] = drop.CurrentMinutesWatched
	}
	for _, drop := range after.Drops {
		prev, ok := beforeMinutes[drop.ID]
		if !ok || prev != drop.CurrentMinutesWatched {
			return true
		}
	}
	return false
}

func (d *DropsTracker) syncCampaigns() {
	// Serialize full syncs: the background loop and SyncNow (the progress
	// watchdog's forced resync) must never interleave their network calls,
	// campaign-pool swaps, and claim attempts.
	d.fullSyncMu.Lock()
	defer d.fullSyncMu.Unlock()

	d.claimAllDropsFromInventory()

	campaigns, upcoming, dashboardCount, err := d.getActiveCampaigns()
	if err != nil {
		slog.Error("Drops sync failed: could not fetch active drop campaigns from Twitch", "error", err)
		d.recordSync(0, 0, 0, err)
		return
	}

	d.mu.Lock()
	d.upcomingCampaigns = upcoming
	d.mu.Unlock()

	// Campaigns produced by the dashboard -> DropCampaignDetails path, before
	// syncWithInventory folds in any in-progress campaign that path missed.
	fromDashboard := len(campaigns)

	campaigns = d.syncWithInventory(campaigns)
	afterInventory := len(campaigns)
	// Anything syncWithInventory added beyond the dashboard set was recovered
	// straight from the inventory's in-progress list.
	recovered := afterInventory - fromDashboard
	if recovered < 0 {
		recovered = 0
	}

	campaigns = d.applyClaimHistory(campaigns)
	afterClaimHistory := len(campaigns)

	// Record every observed campaign (claim-enriched active set + upcoming) into
	// the durable catalog for the "Past" tab. Done before the blacklist filter so
	// blacklisted-but-real campaigns are still catalogued as having existed.
	d.recordCatalog(campaigns, upcoming)

	campaigns = d.applyBlacklist(campaigns)
	afterBlacklist := len(campaigns)

	slog.Debug("Drops sync: campaign counts through the pipeline",
		"dashboardCount", dashboardCount,
		"fromDashboard", fromDashboard,
		"afterInventory", afterInventory,
		"recoveredFromInventory", recovered,
		"afterClaimHistory", afterClaimHistory,
		"afterBlacklist", afterBlacklist)

	d.mu.Lock()
	d.campaigns = campaigns
	d.mu.Unlock()

	// One concise INFO line per sync so a production deployment - which runs
	// without -debug - can confirm the sync ran and see what it found.
	// Previously every sync diagnostic was DEBUG-only, so an empty Drops page
	// was indistinguishable from a sync that never ran, silently skipped every
	// campaign, or errored. Detailed per-campaign skip reasons stay at DEBUG.
	switch {
	case len(campaigns) > 0:
		names := make([]string, 0, len(campaigns))
		for _, c := range campaigns {
			names = append(names, c.Name)
		}
		slog.Info("Drops sync complete: tracking active drop campaigns",
			"tracked", len(campaigns), "dashboardCampaigns", dashboardCount,
			"recoveredFromInventory", recovered, "campaigns", names)
	case dashboardCount == 0:
		slog.Info("Drops sync complete: Twitch reports no active drop campaigns for this account")
	default:
		slog.Info("Drops sync complete: active drop campaigns exist on Twitch but none are trackable "+
			"(all filtered out by date window, claim history, or blacklist; run with -debug for per-campaign reasons)",
			"dashboardCampaigns", dashboardCount)
	}

	d.recordSync(dashboardCount, recovered, len(campaigns), nil)

	d.updateStreamerCampaigns()
}

func (d *DropsTracker) getActiveCampaigns() (active, upcoming []*models.Campaign, dashboardTotal int, err error) {
	// Fetch every campaign the dashboard lists (a single ViewerDropsDashboard
	// call, no status filter) so not-yet-started (UPCOMING) campaigns reach the
	// date-window classification below instead of being dropped at the source;
	// the active farm set is still gated by DateMatch, so this changes nothing
	// about what gets farmed — it only additionally surfaces the upcoming ones.
	dashboardCampaigns, err := d.getDropsDashboard("")
	if err != nil {
		return nil, nil, 0, err
	}
	dashboardCount := len(dashboardCampaigns)

	slog.Debug("Drops sync: fetched active campaigns from dashboard",
		"dashboardCount", dashboardCount)

	var campaigns []*models.Campaign
	var upcomingCampaigns []*models.Campaign
	now := time.Now()
	for _, summary := range dashboardCampaigns {
		campaignID, _ := summary["id"].(string)
		summaryName, _ := summary["name"].(string)

		// The ViewerDropsDashboard listing returns campaign summaries without
		// their timeBasedDrops (and without the per-drop start/end dates that
		// ClearClaimedDrops relies on). Fetch the full campaign details so the
		// campaign actually has drops to track; without this every campaign is
		// filtered out below for having zero usable drops and the Drops page
		// stays empty even while campaigns are active.
		detail, err := d.client.GetDropCampaignDetails(campaignID)
		if err != nil {
			slog.Warn("Drops sync: failed to fetch campaign details, skipping",
				"campaign", summaryName, "campaignID", campaignID, "error", err)
			continue
		}
		if detail == nil {
			slog.Debug("Drops sync: no campaign details returned, skipping",
				"campaign", summaryName, "campaignID", campaignID)
			continue
		}

		campaign, dropsFromDetails, skip := buildTrackedCampaign(summary, detail)
		switch skip {
		case skipOutsideDateWindow:
			// A campaign outside its window is either upcoming (start in the
			// future) or already ended. Upcoming ones are kept for the display-
			// only "Upcoming" tab; they never enter the active farm set.
			if !campaign.StartAt.IsZero() && campaign.StartAt.After(now) {
				upcomingCampaigns = append(upcomingCampaigns, campaign)
				slog.Debug("Drops sync: campaign is upcoming (not yet started)",
					"campaign", campaign.Name, "campaignID", campaign.ID,
					"startAt", campaign.StartAt)
			} else {
				slog.Debug("Drops sync: skipping campaign outside its active date window",
					"campaign", campaign.Name, "campaignID", campaign.ID,
					"startAt", campaign.StartAt, "endAt", campaign.EndAt)
			}
			continue
		case skipNoActiveDrops:
			slog.Debug("Drops sync: skipping campaign with no active unclaimed drops",
				"campaign", campaign.Name, "campaignID", campaign.ID,
				"dropsFromDetails", dropsFromDetails)
			continue
		}

		campaigns = append(campaigns, campaign)
	}

	slog.Debug("Drops sync: active campaigns after detail fetch and filtering",
		"trackedCount", len(campaigns), "upcomingCount", len(upcomingCampaigns))

	return campaigns, upcomingCampaigns, dashboardCount, nil
}

// campaignSkipReason explains why buildTrackedCampaign declined to track a
// campaign (skipNone means it should be tracked).
type campaignSkipReason int

const (
	skipNone campaignSkipReason = iota
	skipOutsideDateWindow
	skipNoActiveDrops
)

// buildTrackedCampaign merges a ViewerDropsDashboard summary with its
// DropCampaignDetails response into a tracked Campaign and decides whether it
// should be tracked. The details response is authoritative (it's the only
// source of timeBasedDrops and their per-drop dates); the summary is used only
// to backfill fields details occasionally omits (id, name, game). It returns
// the built campaign, how many drops details supplied (for diagnostics), and a
// skip reason. Kept free of the API client so the merge/filter behavior can be
// unit-tested directly.
func buildTrackedCampaign(summary, detail map[string]interface{}) (*models.Campaign, int, campaignSkipReason) {
	campaign := models.NewCampaignFromGQL(detail)
	summaryCampaign := models.NewCampaignFromGQL(summary)

	if campaign.ID == "" {
		campaign.ID = summaryCampaign.ID
	}
	if campaign.Name == "" {
		campaign.Name = summaryCampaign.Name
	}
	if campaign.Game == nil && summaryCampaign.Game != nil {
		campaign.Game = summaryCampaign.Game
	}

	// Backfill the campaign-level date window from the summary when the details
	// response omits it. The ViewerDropsDashboard summary always carries the
	// campaign's startAt/endAt; a details response that doesn't would otherwise
	// leave DateMatch false and get the campaign silently skipped as "outside
	// its date window" even while it's actively running - the exact class of
	// silent filtering that leaves the Drops page empty during live farming.
	// DateMatch is then recomputed from whatever dates we end up with, so a
	// details response that genuinely places the campaign outside its window
	// (non-zero dates) is preserved rather than overridden by the summary.
	if campaign.StartAt.IsZero() && !summaryCampaign.StartAt.IsZero() {
		campaign.StartAt = summaryCampaign.StartAt
	}
	if campaign.EndAt.IsZero() && !summaryCampaign.EndAt.IsZero() {
		campaign.EndAt = summaryCampaign.EndAt
	}
	if !campaign.StartAt.IsZero() && !campaign.EndAt.IsZero() {
		now := time.Now()
		campaign.DateMatch = campaign.StartAt.Before(now) && campaign.EndAt.After(now)
	}

	dropsFromDetails := len(campaign.Drops)

	if !campaign.DateMatch {
		return campaign, dropsFromDetails, skipOutsideDateWindow
	}

	campaign.ClearClaimedDrops()
	if len(campaign.Drops) == 0 {
		return campaign, dropsFromDetails, skipNoActiveDrops
	}

	return campaign, dropsFromDetails, skipNone
}

func (d *DropsTracker) getDropsDashboard(status string) ([]map[string]interface{}, error) {
	resp, err := d.client.PostGQL(constants.ViewerDropsDashboard)
	if err != nil {
		return nil, err
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return nil, nil
	}

	currentUser, ok := data["currentUser"].(map[string]interface{})
	if !ok || currentUser == nil {
		return nil, nil
	}

	campaignsData, ok := currentUser["dropCampaigns"].([]interface{})
	if !ok || campaignsData == nil {
		return nil, nil
	}

	var result []map[string]interface{}
	for _, c := range campaignsData {
		campaign, ok := c.(map[string]interface{})
		if !ok {
			continue
		}

		if status != "" {
			if s, ok := campaign["status"].(string); ok && s != status {
				continue
			}
		}

		result = append(result, campaign)
	}

	return result, nil
}

func (d *DropsTracker) getInventory() (map[string]interface{}, error) {
	resp, err := d.client.PostGQL(constants.Inventory)
	if err != nil {
		return nil, err
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return nil, nil
	}

	currentUser, ok := data["currentUser"].(map[string]interface{})
	if !ok || currentUser == nil {
		return nil, nil
	}

	inventory, ok := currentUser["inventory"].(map[string]interface{})
	if !ok {
		return nil, nil
	}

	return inventory, nil
}

func (d *DropsTracker) syncWithInventory(campaigns []*models.Campaign) []*models.Campaign {
	inventory, err := d.getInventory()
	if err != nil || inventory == nil {
		return campaigns
	}

	inProgress, ok := inventory["dropCampaignsInProgress"].([]interface{})
	if !ok || inProgress == nil {
		return campaigns
	}

	tracked := make(map[string]bool, len(campaigns))
	for _, campaign := range campaigns {
		if campaign.ID != "" {
			tracked[campaign.ID] = true
		}

		campaign.ClearClaimedDrops()

		for _, prog := range inProgress {
			progData, ok := prog.(map[string]interface{})
			if !ok {
				continue
			}

			progID, ok := progData["id"].(string)
			if !ok || progID != campaign.ID {
				continue
			}

			campaign.InInventory = true

			if drops, ok := progData["timeBasedDrops"].([]interface{}); ok {
				campaign.SyncDrops(drops, d.claimDropFn())
			}

			campaign.ClearClaimedDrops()
			break
		}
	}

	// Recover campaigns Twitch is actively crediting (they appear in the
	// inventory's dropCampaignsInProgress with live progress) but that the
	// ViewerDropsDashboard -> DropCampaignDetails path never produced -- e.g.
	// when a per-campaign details fetch returns nothing. Without this the
	// Drops page shows "no active campaigns" even while drops are visibly
	// filling up, because the inventory (which drives farming/claiming) is a
	// separate source from the dashboard listing that populates the page.
	for _, prog := range inProgress {
		progData, ok := prog.(map[string]interface{})
		if !ok {
			continue
		}
		progID, _ := progData["id"].(string)
		if progID == "" || tracked[progID] {
			continue
		}

		recovered := d.buildInProgressCampaign(progData)
		if recovered == nil || len(recovered.Drops) == 0 {
			continue
		}

		slog.Debug("Drops sync: recovered in-progress campaign from inventory missing from dashboard/details path",
			"campaign", recovered.Name, "campaignID", recovered.ID, "drops", len(recovered.Drops))

		campaigns = append(campaigns, recovered)
		tracked[progID] = true
	}

	return campaigns
}

// claimDropFn returns the callback SyncDrops uses to claim a drop once its
// watch requirement is met, recording a claim event on success.
func (d *DropsTracker) claimDropFn() func(*models.Drop) bool {
	return func(drop *models.Drop) bool {
		claimed, err := d.client.ClaimDrop(drop)
		if err != nil {
			slog.Error("Failed to claim drop", "drop", drop.Name, "error", err)
			return false
		}
		if claimed {
			events.Record(events.TypeDropClaimed, "", drop.Name)
			d.noteDropClaimed(drop.Name)
		}
		return claimed
	}
}

// SetDropClaimedHook registers a listener invoked with the drop name on each
// successful claim (for durable daily-summary counting). Set once before the
// loops start.
func (d *DropsTracker) SetDropClaimedHook(fn func(dropName string)) {
	d.onDropClaimed = fn
}

// noteDropClaimed fires the claim hook if one is set.
func (d *DropsTracker) noteDropClaimed(dropName string) {
	if d.onDropClaimed != nil {
		d.onDropClaimed(dropName)
	}
}

// buildInProgressCampaign constructs a tracked Campaign directly from an
// inventory dropCampaignsInProgress entry, applying each drop's `self`
// progress. Unlike the dashboard/details path it does not gate on a parseable
// date window: membership in dropCampaignsInProgress already proves Twitch
// considers the campaign active, and inventory entries sometimes omit the
// per-drop start/end dates ClearClaimedDrops relies on. It keeps only
// still-unclaimed drops and returns nil for an entry with no campaign ID.
func (d *DropsTracker) buildInProgressCampaign(progData map[string]interface{}) *models.Campaign {
	campaign := models.NewCampaignFromGQL(progData)
	if campaign.ID == "" {
		return nil
	}
	campaign.InInventory = true

	if drops, ok := progData["timeBasedDrops"].([]interface{}); ok {
		campaign.SyncDrops(drops, d.claimDropFn())
	}

	kept := make([]*models.Drop, 0, len(campaign.Drops))
	for _, drop := range campaign.Drops {
		if !drop.IsClaimed {
			kept = append(kept, drop)
		}
	}
	campaign.Drops = kept

	return campaign
}

// applyClaimHistory cross-references each campaign's drops against the
// account's Twitch-wide claim history (the inventory's gameEventDrops),
// which lists rewards already granted independently of whether this exact
// campaign instance has been joined. This is what lets a recurring or
// regional variant of a campaign -- one sharing the same reward name and
// game but a different campaign/drop ID -- get recognized as already
// claimed before it's ever prioritized for watch time.
func (d *DropsTracker) applyClaimHistory(campaigns []*models.Campaign) []*models.Campaign {
	inventory, err := d.getInventory()
	if err != nil {
		slog.Error("Failed to fetch inventory for claim history check", "error", err)
		return campaigns
	}

	claimedRewards := extractClaimedRewardKeys(inventory)
	if len(claimedRewards) == 0 {
		return campaigns
	}

	for _, campaign := range campaigns {
		campaign.ApplyClaimHistory(claimedRewards)

		switch {
		case campaign.ClaimStatus == models.CampaignClaimStatusAlreadyClaimed:
			slog.Info("Skipping drop campaign: already claimed",
				"campaign", campaign.Name, "campaignID", campaign.ID,
				"alreadyClaimed", campaign.ClaimedDropNames)
		case len(campaign.ClaimedDropNames) > 0:
			slog.Info("Skipping already-claimed reward within active drop campaign",
				"campaign", campaign.Name, "campaignID", campaign.ID,
				"alreadyClaimed", campaign.ClaimedDropNames)
		}
	}

	return campaigns
}

// extractClaimedRewardKeys reads the inventory's gameEventDrops -- Twitch's
// account-wide record of rewards already granted -- and normalizes each one
// into a Drop.RewardKey-compatible identifier (game + reward name). Raw
// reward/drop IDs are intentionally not used here: they can differ (or even
// collide) between recurring/regional variants of the same campaign, while
// the reward's own name and game stay stable.
func extractClaimedRewardKeys(inventory map[string]interface{}) map[string]bool {
	claimed := make(map[string]bool)
	if inventory == nil {
		return claimed
	}

	events, ok := inventory["gameEventDrops"].([]interface{})
	if !ok || events == nil {
		return claimed
	}

	for _, e := range events {
		entry, ok := e.(map[string]interface{})
		if !ok {
			continue
		}

		name, _ := entry["name"].(string)
		if name == "" {
			if benefit, ok := entry["benefit"].(map[string]interface{}); ok {
				name, _ = benefit["name"].(string)
			}
		}
		if name == "" {
			continue
		}

		gameID, _ := entry["gameId"].(string)
		if gameID == "" {
			if game, ok := entry["game"].(map[string]interface{}); ok {
				gameID, _ = game["id"].(string)
			}
		}

		claimed[models.NormalizeRewardKey(gameID, name)] = true
	}

	return claimed
}

// applyBlacklist drops any campaign whose drop or reward name matches a
// configured blacklist keyword, so it's never prioritized for watch time. This
// mirrors the claim-history dedup as an additional exclusion condition, and
// logs each skip distinctly (with the keyword and matched name) so it's clear
// the campaign was excluded by the blacklist rather than for another reason.
func (d *DropsTracker) applyBlacklist(campaigns []*models.Campaign) []*models.Campaign {
	d.mu.RLock()
	blacklist := d.dropBlacklist
	d.mu.RUnlock()

	if len(blacklist) == 0 {
		return campaigns
	}

	kept := make([]*models.Campaign, 0, len(campaigns))
	for _, campaign := range campaigns {
		if keyword, dropName, matched := campaign.MatchesBlacklist(blacklist); matched {
			slog.Info("Skipping drop campaign: matched drop-name blacklist",
				"campaign", campaign.Name, "campaignID", campaign.ID,
				"keyword", keyword, "matchedDrop", dropName)
			continue
		}
		kept = append(kept, campaign)
	}

	return kept
}

func (d *DropsTracker) claimAllDropsFromInventory() {
	inventory, err := d.getInventory()
	if err != nil || inventory == nil {
		return
	}

	inProgress, ok := inventory["dropCampaignsInProgress"].([]interface{})
	if !ok || inProgress == nil {
		return
	}

	for _, campaign := range inProgress {
		campaignData, ok := campaign.(map[string]interface{})
		if !ok {
			continue
		}

		drops, ok := campaignData["timeBasedDrops"].([]interface{})
		if !ok || drops == nil {
			continue
		}

		for _, dropData := range drops {
			dropMap, ok := dropData.(map[string]interface{})
			if !ok {
				continue
			}

			drop := models.NewDropFromGQL(dropMap)
			if selfData, ok := dropMap["self"].(map[string]interface{}); ok {
				drop.Update(selfData)
			}

			if drop.IsClaimable {
				if claimed, err := d.client.ClaimDrop(drop); err != nil {
					slog.Error("Failed to claim drop", "drop", drop.Name, "error", err)
				} else if claimed {
					slog.Info("Claimed drop", "drop", drop.Name)
					events.Record(events.TypeDropClaimed, "", drop.Name)
					d.noteDropClaimed(drop.Name)
				}
				time.Sleep(5 * time.Second)
			}
		}
	}
}

func (d *DropsTracker) updateStreamerCampaigns() {
	d.mu.RLock()
	campaigns := d.campaigns
	d.mu.RUnlock()

	// All assignment/progress logging shares the de-dup maps below, so serialize
	// it against a concurrent updateStreamerCampaigns from the other sync
	// goroutine. The work here is cheap and runs at most every couple of minutes.
	d.logMu.Lock()
	defer d.logMu.Unlock()

	if d.loggedRestrictedAssignments == nil {
		d.loggedRestrictedAssignments = make(map[string]struct{})
	}
	if d.loggedProgressBucket == nil {
		d.loggedProgressBucket = make(map[string]int)
	}

	// currentAssignments accumulates this cycle's channel-restricted assignments;
	// it replaces loggedRestrictedAssignments at the end so a campaign that stops
	// being assigned is announced again if it later comes back.
	currentAssignments := make(map[string]struct{})

	// Remember the first eligible online streamer seen per campaign (streamer
	// slice order is stable, so this is deterministic) plus its first-seen order,
	// so the compact progress line names one farmer per campaign instead of
	// repeating an identical line for every streamer assigned the same campaign.
	farmer := make(map[string]*models.Streamer)
	var progressOrder []*models.Campaign

	for _, streamer := range d.streamers {
		if !streamer.DropsCondition() {
			continue
		}

		var streamerCampaigns []*models.Campaign
		for _, campaign := range campaigns {
			if len(campaign.Drops) == 0 {
				continue
			}

			if campaign.Game == nil || streamer.Stream.GameID() == "" {
				continue
			}

			if campaign.Game.ID != streamer.Stream.GameID() {
				continue
			}

			hasID := false
			for _, id := range streamer.Stream.GetCampaignIDs() {
				if id == campaign.ID {
					hasID = true
					break
				}
			}

			if !hasID {
				continue
			}

			if campaign.IsChannelRestricted() {
				if !campaign.AllowsChannel(streamer.ChannelID) {
					// Defensive: Twitch's per-channel CampaignIDs lookup
					// (GetCampaignIDsFromStreamer) should already exclude
					// campaigns this channel isn't eligible for, so this
					// should never trigger in practice. If it ever does,
					// make it loud instead of silently over-crediting watch
					// time Twitch won't actually count.
					slog.Warn("Withholding drop progress: channel not in campaign's allowed-channel list",
						"streamer", streamer.Username, "channelID", streamer.ChannelID,
						"campaign", campaign.Name, "campaignID", campaign.ID,
						"allowedChannels", campaign.Channels)
					continue
				}
				d.logRestrictedAssignment(streamer, campaign, currentAssignments)
			}

			streamerCampaigns = append(streamerCampaigns, campaign)

			if _, ok := farmer[campaign.ID]; !ok {
				farmer[campaign.ID] = streamer
				progressOrder = append(progressOrder, campaign)
			}
		}

		streamer.Stream.SetCampaigns(streamerCampaigns)
	}

	d.loggedRestrictedAssignments = currentAssignments

	for _, campaign := range progressOrder {
		d.logCampaignProgress(farmer[campaign.ID], campaign)
	}

	// Drop remembered progress checkpoints for campaigns no longer tracked, so a
	// long-running process doesn't accumulate stale entries. Buckets for tracked
	// but momentarily-unfarmed campaigns are kept, so a farmer returning mid-way
	// doesn't re-announce progress it already passed.
	if len(d.loggedProgressBucket) > 0 {
		tracked := make(map[string]struct{}, len(campaigns))
		for _, c := range campaigns {
			tracked[c.ID] = struct{}{}
		}
		for id := range d.loggedProgressBucket {
			if _, ok := tracked[id]; !ok {
				delete(d.loggedProgressBucket, id)
			}
		}
	}
}

// progressLogStepPercent is the progress increment (in percent) between the
// checkpoints at which a drop's progress line is (re-)logged at INFO. It also
// sizes the textual bar so one cell equals one checkpoint.
const progressLogStepPercent = 5

// progressBarWidth is the number of cells in the textual drop-progress bar.
const progressBarWidth = 100 / progressLogStepPercent

// logRestrictedAssignment records that a channel-restricted campaign is assigned
// to streamer for this cycle and announces it at INFO only the first time the
// assignment appears (a new campaign for the streamer, or one reassigned after
// dropping off). Steady-state re-assignments seen on every lightweight progress
// sync are logged at DEBUG, and the long allowed-channel list is kept off the
// INFO line entirely (available at DEBUG for troubleshooting). Callers must hold
// d.logMu.
func (d *DropsTracker) logRestrictedAssignment(
	streamer *models.Streamer, campaign *models.Campaign, current map[string]struct{},
) {
	key := streamer.Username + "\x00" + campaign.ID
	current[key] = struct{}{}

	if _, seen := d.loggedRestrictedAssignments[key]; seen {
		slog.Debug("Channel-restricted drop campaign still assigned to streamer",
			"streamer", streamer.Username, "campaign", campaign.Name)
		return
	}

	slog.Info("Channel-restricted drop campaign assigned to streamer",
		"streamer", streamer.Username, "campaign", campaign.Name)
	slog.Debug("Channel-restricted drop campaign allowed-channel list",
		"streamer", streamer.Username, "campaign", campaign.Name,
		"campaignID", campaign.ID, "allowedChannels", campaign.Channels)
}

// logCampaignProgress emits a compact, human-readable progress line for a drop
// campaign a streamer is farming, e.g.
//
//	World of Tanks [cyganzor] AMD Summer Arena Drops#2: -----------> 55%
//
// It is throttled: the line is logged at INFO only when progress crosses a new
// 5% checkpoint (which includes reaching 100% / claim-ready), so a campaign
// inching forward on every lightweight sync doesn't spam the log. Cycles that
// don't cross a checkpoint are logged at DEBUG rather than dropped, so a -debug
// run still shows every refresh. Callers must hold d.logMu.
func (d *DropsTracker) logCampaignProgress(streamer *models.Streamer, campaign *models.Campaign) {
	if streamer == nil || campaign.CurrentDrop() == nil {
		return
	}

	pct := campaign.OverallProgressPercent()
	line := formatDropProgress(streamer.Username, campaign, pct)

	bucket := pct / progressLogStepPercent
	if last, seen := d.loggedProgressBucket[campaign.ID]; !seen || bucket > last {
		d.loggedProgressBucket[campaign.ID] = bucket
		slog.Info(line)
		return
	}
	slog.Debug(line)
}

// formatDropProgress renders the compact one-line progress string used in the
// logs: the game, the streamer farming it, the campaign, a textual progress bar,
// and the percentage — deliberately without campaignID or the allowed-channel
// list, which are debugging details rather than daily-monitoring ones.
func formatDropProgress(streamer string, campaign *models.Campaign, pct int) string {
	game := ""
	if campaign.Game != nil {
		game = campaign.Game.Name
	}
	if game != "" {
		return fmt.Sprintf("%s [%s] %s: %s %d%%", game, streamer, campaign.Name, progressBar(pct), pct)
	}
	return fmt.Sprintf("[%s] %s: %s %d%%", streamer, campaign.Name, progressBar(pct), pct)
}

// progressBar renders pct (0-100) as a fixed-width arrow, e.g. "----------->"
// at ~55%, growing to a full "-------------------->" at 100%.
func progressBar(pct int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return strings.Repeat("-", pct*progressBarWidth/100) + ">"
}
