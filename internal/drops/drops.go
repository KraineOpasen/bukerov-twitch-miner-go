package drops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
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
	ClaimDrop(drop *models.Drop) (api.ClaimStatus, error)
}

// SyncStatus is a snapshot of the most recent campaign sync. It exists so the
// debug snapshot (and any future health check) can tell whether the sync ran,
// what Twitch's dashboard returned, how many campaigns were recovered from the
// inventory's in-progress list, how many ended up tracked, and whether the last
// run errored - none of which was observable before, since every sync
// diagnostic was DEBUG-only and a production container runs without -debug.
type SyncStatus struct {
	// LastSyncAt is the last full-sync ATTEMPT (stamped on success and failure);
	// LastSuccessAt is the last full sync that completed without error. Keeping
	// them distinct lets the Status Center show "attempted 20s ago, but the last
	// SUCCESSFUL discovery was 40m ago" instead of masking a failing sync.
	LastSyncAt    time.Time
	LastSuccessAt time.Time
	// LastDuration is how long the last full sync took (dashboard + details +
	// inventory), for the Status Center's "duration" field.
	LastDuration time.Duration
	Runs         int
	// IntervalMinutes is the configured full campaign-sync cadence, so the Status
	// Center can compute the next sync and the worst-case new-campaign delay
	// without duplicating the settings lookup.
	IntervalMinutes    int
	DashboardCampaigns int
	RecoveredCampaigns int
	TrackedCampaigns   int
	// FilteredByBlacklist / FilteredByGame are how many candidates the last sync
	// dropped for each reason, surfaced so the Status Center can explain an empty
	// tracked set without -debug.
	FilteredByBlacklist int
	FilteredByGame      int
	LastError           string

	// Revision is a monotonic counter bumped every time the published campaign
	// pool changes (full sync OR lightweight progress sync). Overview and Drops
	// both read it, so identical revisions prove they are rendering the very same
	// backend snapshot; a changed revision means new confirmed data landed.
	Revision uint64
	// BackendUpdatedAt is when the campaign pool was last (re)published, and
	// UpdateSource says what published it (full_sync / light_sync / manual_sync).
	// Together with Revision they are the diagnostic freshness fields the Drops
	// and Overview pages surface. No secrets are ever carried here.
	BackendUpdatedAt time.Time
	UpdateSource     string

	// Lightweight progress-sync observability (Stage 3). ProgressLastSyncAt is
	// stamped only when an inventory read actually completed, so the progress
	// watchdog can require "a fresh successful observation" before counting a
	// no-progress interval — and an inventory outage (previously swallowed
	// silently) is now visible via ProgressLastError.
	ProgressRuns       int
	ProgressLastSyncAt time.Time
	ProgressLastError  string
}

// Update sources for SyncStatus.UpdateSource / the diagnostic freshness fields.
const (
	updateSourceFullSync   = "full_sync"
	updateSourceLightSync  = "light_sync"
	updateSourceManualSync = "manual_sync"
)

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
	syncRuns              int
	lastSyncAt            time.Time
	lastSuccessAt         time.Time
	lastDuration          time.Duration
	lastDashboardCount    int
	lastRecoveredCount    int
	lastTrackedCount      int
	lastFilteredBlacklist int
	lastFilteredGame      int
	lastSyncErr           string

	// lastSummaryFingerprint is a deterministic semantic signature of the last
	// "Drops sync complete" summary published at INFO (sorted tracked campaign
	// IDs + the salient counts + outcome; no timestamps/pointers/map order). It
	// lets a repeated sync that produced the SAME semantic result publish its
	// summary at DEBUG instead of spamming an identical INFO block every cycle.
	// An INFO summary is (re)published only on the first sync, on a genuine
	// change of this fingerprint, or on recovery from a real sync error. Guarded
	// by mu; only ever written from the fullSyncMu-serialized syncCampaigns.
	lastSummaryFingerprint string
	// lastGameFilterFailOpenKey remembers the name-only, nothing-resolved
	// fail-open condition already warned about (a signature of the unresolved
	// configured game names), so the honest transition WARN fires once instead of
	// every sync; it re-fires when the unresolved set changes and clears (with one
	// INFO transition) when resolution recovers. Guarded by mu.
	lastGameFilterFailOpenKey string

	// revision is bumped every time the published campaign pool changes (full or
	// progress sync); backendUpdatedAt/lastUpdateSource record when and by what.
	// All guarded by mu. Overview and Drops read these so they can prove they are
	// showing the same snapshot (see SyncStatus.Revision).
	revision         uint64
	backendUpdatedAt time.Time
	lastUpdateSource string

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

	// filterGameIDs / filterGameNames are the operator's drop-campaign game
	// filter (config.DropCampaignGameIDs / DropCampaignGames). filterGameIDs is a
	// strict, candidate-independent allowlist of exact opaque Twitch game IDs;
	// filterGameNames is a best-effort list of game names/displayNames resolved
	// to IDs against each sync's own campaign candidates. Both empty = track
	// every game (the backward-compatible default). Guarded by mu so they can be
	// updated at runtime from the Settings page.
	filterGameIDs   []string
	filterGameNames []string

	// resync wakes the lightweight progress-sync loop early (buffered to 1 so
	// bursts of triggers between syncs coalesce into a single extra run). Fed by
	// TriggerProgressSync, e.g. right after a watched minute is reported so the
	// Drops page reflects the new progress within seconds instead of waiting out
	// DropProgressSyncInterval.
	resync chan struct{}

	// campaignResync wakes the FULL campaign-sync loop early (buffered to 1 so
	// bursts coalesce into one extra run). Fed by triggerCampaignResync when the
	// operator changes a Drops filter/blacklist (so a re-filtered campaign set
	// takes effect within seconds instead of waiting out CampaignSyncInterval)
	// and by the manual "Sync Drops now" action. Unlike the progress resync this
	// runs the full discovery pipeline, so it is the only path that can surface a
	// campaign the current tracked set is missing without waiting a full interval.
	campaignResync chan struct{}

	// lastManualSyncAt gates the manual "Sync Drops now" action so it can't be
	// spammed into a request storm; guarded by mu.
	lastManualSyncAt time.Time

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

	// upcomingNotifier, when set, is alerted once per newly-observed RELEVANT
	// upcoming campaign on the edge of a SUCCESSFUL full sync (a display/opt-in
	// concern; the notifier owns durable dedupe). Set once at wiring time before
	// the loops start; read lock-free. nil disables upcoming-campaign alerts.
	upcomingNotifier UpcomingNotifier

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
		client:         client,
		streamers:      streamers,
		settings:       settings,
		dropBlacklist:  dropBlacklist,
		resync:         make(chan struct{}, 1),
		campaignResync: make(chan struct{}, 1),
		intervalUnit:   time.Minute,
	}
}

// UpdateBlacklist replaces the drop-name blacklist. Called when the operator
// changes it on the Settings page so the new keywords take effect on the next
// campaign sync without a restart.
//
// It deliberately does NOT trigger a campaign resync itself: its sole caller,
// Miner.ApplySettings, always calls UpdateGameFilter immediately afterwards for
// the same logical settings save, and that call issues the single resync that
// re-filters the tracked set (blacklist included, since syncCampaigns reads the
// blacklist fresh). Triggering here as well made one settings save fan out into
// two full syncs — the buffered-to-1 campaignResync channel does not coalesce
// the two back-to-back triggers when the loop drains the first before the second
// is enqueued — which is the duplicate-INFO-block regression this avoids.
func (d *DropsTracker) UpdateBlacklist(dropBlacklist []string) {
	d.mu.Lock()
	d.dropBlacklist = dropBlacklist
	d.mu.Unlock()
}

// UpdateGameFilter replaces the drop-campaign game filter (the strict game-ID
// allowlist and the best-effort game-name list). Called at construction and
// whenever the operator saves the Settings page, so a change takes effect on
// the next full campaign sync without a restart: stale foreign assignments drop
// off when updateStreamerCampaigns re-runs against the refiltered tracked set.
// The two lists are the operator's inputs verbatim (already normalized by the
// config/Settings layer); applyGameFilter does the trimming/resolution.
func (d *DropsTracker) UpdateGameFilter(gameIDs, gameNames []string) {
	d.mu.Lock()
	d.filterGameIDs = gameIDs
	d.filterGameNames = gameNames
	d.mu.Unlock()
	// A game-filter change alters which campaigns are trackable; run a full
	// resync now (re-fetching the dashboard so a game newly allowed can also be
	// discovered, not just re-filtered) instead of waiting out the interval.
	d.triggerCampaignResync()
}

// UpdateStreamers replaces the tracked streamer list at runtime (Settings-page
// add/remove), so campaign assignment follows the current roster without a
// restart: an added streamer starts receiving campaigns on the next sync pass,
// a removed one stops. The slice is copied so later caller-side mutations
// cannot reach tracker state; readers snapshot it under mu (see
// updateStreamerCampaigns), keeping both sync goroutines race-free against
// a concurrent replacement.
func (d *DropsTracker) UpdateStreamers(streamers []*models.Streamer) {
	list := append([]*models.Streamer(nil), streamers...)
	d.mu.Lock()
	d.streamers = list
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

// UpcomingNotifier is the drops tracker's hook for alerting on a newly-observed
// RELEVANT upcoming campaign. It is invoked once per relevant upcoming campaign
// on the edge of a SUCCESSFUL full sync, OUTSIDE the tracker's state mutex, so
// an implementation may perform bounded network I/O and durable dedupe without
// stalling readers or the active-drops pipeline. Implementations MUST be
// idempotent per campaign (durable dedupe) so repeated syncs, restarts, and
// reordered responses never re-notify. nil disables upcoming-campaign alerts.
type UpcomingNotifier interface {
	NotifyUpcomingCampaign(ctx context.Context, c *models.Campaign)
}

// SetUpcomingNotifier wires the upcoming-campaign alert hook. Set once before
// the sync loops start (read lock-free); nil disables the alert.
func (d *DropsTracker) SetUpcomingNotifier(n UpcomingNotifier) {
	d.upcomingNotifier = n
}

// filterRelevantUpcoming keeps only the upcoming campaigns whose game passes the
// operator's game filter, reusing the exact active game-identity contract
// (resolveAllowedGameIDs + gameIDAllowed). Fail-open exactly like the active
// filter: nothing configured, or a name-only filter that resolved to no IDs this
// cycle, keeps everything; a campaign with no game ID is kept. Pure and silent —
// upcoming relevance is a display/notification concern that never touches active
// counters, blacklist, claim history, or the active-filter logs.
func (d *DropsTracker) filterRelevantUpcoming(up []*models.Campaign) []*models.Campaign {
	allowed, configured := d.resolveAllowedGameIDs(up)
	if !configured || len(allowed) == 0 {
		return up
	}
	out := make([]*models.Campaign, 0, len(up))
	for _, c := range up {
		if gameIDAllowed(allowed, c) {
			out = append(out, c)
		}
	}
	return out
}

// RelevantUpcomingCampaigns returns the upcoming campaigns that pass the
// operator's game filter — the display-only relevance the Drops "Upcoming" tab
// and the upcoming-campaign notification use. It never enters the active farm
// set and never changes active filtering behavior, counters, or logs.
func (d *DropsTracker) RelevantUpcomingCampaigns() []*models.Campaign {
	return d.filterRelevantUpcoming(d.UpcomingCampaigns())
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
		LastSyncAt:          d.lastSyncAt,
		LastSuccessAt:       d.lastSuccessAt,
		LastDuration:        d.lastDuration,
		Runs:                d.syncRuns,
		IntervalMinutes:     d.settings.CampaignSyncInterval,
		DashboardCampaigns:  d.lastDashboardCount,
		RecoveredCampaigns:  d.lastRecoveredCount,
		TrackedCampaigns:    d.lastTrackedCount,
		FilteredByBlacklist: d.lastFilteredBlacklist,
		FilteredByGame:      d.lastFilteredGame,
		LastError:           d.lastSyncErr,
		Revision:            d.revision,
		BackendUpdatedAt:    d.backendUpdatedAt,
		UpdateSource:        d.lastUpdateSource,
		ProgressRuns:        d.progressRuns,
		ProgressLastSyncAt:  d.progressLastSyncAt,
		ProgressLastError:   d.progressLastErr,
	}
}

// Revision returns the current campaign-pool revision (bumped on every published
// change, full or progress sync). Overview and Drops read it to prove they are
// rendering the same backend snapshot. Lock-free-safe via the RLock.
func (d *DropsTracker) Revision() uint64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.revision
}

// bumpRevision records that the published campaign pool just changed: it
// increments the revision and stamps when/by-what. Caller must hold d.mu.
func (d *DropsTracker) bumpRevisionLocked(source string) {
	d.revision++
	d.backendUpdatedAt = time.Now()
	d.lastUpdateSource = source
}

// recordSync updates the sync bookkeeping surfaced by SyncStatus. duration is
// the wall time of the full sync; filteredByBlacklist/filteredByGame are how
// many candidates it dropped for each reason. lastSyncAt is stamped on every
// attempt; lastSuccessAt only when err is nil.
func (d *DropsTracker) recordSync(dashboardCount, recoveredCount, trackedCount, filteredByBlacklist, filteredByGame int, duration time.Duration, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.syncRuns++
	now := time.Now()
	d.lastSyncAt = now
	d.lastDuration = duration
	d.lastDashboardCount = dashboardCount
	d.lastRecoveredCount = recoveredCount
	d.lastTrackedCount = trackedCount
	d.lastFilteredBlacklist = filteredByBlacklist
	d.lastFilteredGame = filteredByGame
	if err != nil {
		d.lastSyncErr = err.Error()
	} else {
		d.lastSyncErr = ""
		d.lastSuccessAt = now
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

	// A campaignResync buffered before the loop started (the construction-time
	// UpdateGameFilter seed at miner setup) is already reflected by the initial
	// full sync below, which reads the seeded filter fresh. Drop that pre-Start
	// trigger so startup runs exactly one full sync instead of immediately firing
	// a second, byte-identical one on the loop's first select iteration. A real
	// resync requested after this point (a runtime filter change) still buffers
	// and runs normally via the :campaignResync case below.
	select {
	case <-d.campaignResync:
	default:
	}

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
		case <-d.campaignResync:
			// Early wake: a Drops filter/blacklist change or a manual "Sync now".
			// Stop the pending timer so the next iteration starts a fresh one at
			// the current interval (a resync must not shorten the steady cadence).
			timer.Stop()
			d.syncCampaigns()
		}
	}
}

// triggerCampaignResync asks the full-sync loop to run an immediate campaign
// sync instead of waiting out CampaignSyncInterval. Non-blocking and coalescing
// (buffered-to-1 channel): concurrent triggers collapse into a single extra run,
// and syncCampaigns' fullSyncMu still serializes it against the scheduled sync,
// so this never launches a parallel sync. A trigger sent before Start is
// buffered and consumed once the loop begins.
func (d *DropsTracker) triggerCampaignResync() {
	select {
	case d.campaignResync <- struct{}{}:
	default:
	}
}

// manualSyncCooldown bounds how often the manual "Sync Drops now" action may
// force a resync, so a user (or a stuck auto-refresh) cannot turn the button
// into a request storm. Package variable so tests can shrink it.
var manualSyncCooldown = 30 * time.Second

// ManualSyncResult reports the outcome of a manual "Sync Drops now" request.
type ManualSyncResult struct {
	// Triggered is true when a resync was scheduled; false when the call was
	// within the cooldown window (RetryAfter then reports how long to wait).
	Triggered bool
	// RetryAfter is the remaining cooldown when Triggered is false (else 0).
	RetryAfter time.Duration
	// Status is a snapshot of the sync bookkeeping at call time, so the caller
	// can report the last completed sync and its counts without a secret in sight.
	Status SyncStatus
}

// RequestManualSync schedules an immediate campaign resync on behalf of the
// dashboard's "Sync Drops now" action. It is race-safe and non-blocking: the
// resync runs on the full-sync loop (serialized by fullSyncMu, so never a
// parallel sync), and a cooldown prevents spamming. Completion is observed via
// SyncStatus (LastSyncAt), which the Drops/Status pages already poll — so the
// result carries the current status rather than blocking for the network sync.
func (d *DropsTracker) RequestManualSync() ManualSyncResult {
	now := time.Now()

	d.mu.Lock()
	elapsed := now.Sub(d.lastManualSyncAt)
	if !d.lastManualSyncAt.IsZero() && elapsed < manualSyncCooldown {
		remaining := manualSyncCooldown - elapsed
		d.mu.Unlock()
		return ManualSyncResult{Triggered: false, RetryAfter: remaining, Status: d.SyncStatus()}
	}
	d.lastManualSyncAt = now
	d.mu.Unlock()

	d.triggerCampaignResync()
	return ManualSyncResult{Triggered: true, Status: d.SyncStatus()}
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
	d.bumpRevisionLocked(updateSourceLightSync)
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

	start := time.Now()

	d.claimAllDropsFromInventory()

	campaigns, upcoming, dashboardCount, err := d.getActiveCampaigns()
	if err != nil {
		slog.Error("Drops sync failed: could not fetch active drop campaigns from Twitch; keeping previously tracked campaigns", "error", err)
		// dashboardCount is non-zero when the dashboard listing succeeded and
		// the failure happened later (campaign details), so SyncStatus shows
		// what Twitch reported even for a failed run.
		d.recordSync(dashboardCount, 0, 0, 0, 0, time.Since(start), err)
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
	filteredByBlacklist := afterClaimHistory - afterBlacklist

	// Single filtering point: the dashboard and inventory-recovered candidates
	// are already merged into `campaigns` here, so one applyGameFilter pass gates
	// both. Placed after the blacklist so a blacklisted campaign is attributed to
	// the blacklist (not the game filter), and after recordCatalog so a foreign
	// campaign still survives in the durable "Past" catalog.
	campaigns, filteredByGame := d.applyGameFilter(campaigns)
	afterGameFilter := len(campaigns)

	slog.Debug("Drops sync: campaign counts through the pipeline",
		"dashboardCount", dashboardCount,
		"fromDashboard", fromDashboard,
		"afterInventory", afterInventory,
		"recoveredFromInventory", recovered,
		"afterClaimHistory", afterClaimHistory,
		"afterBlacklist", afterBlacklist,
		"filteredByBlacklist", filteredByBlacklist,
		"afterGameFilter", afterGameFilter,
		"filteredByGame", filteredByGame)

	d.mu.Lock()
	d.campaigns = campaigns
	d.bumpRevisionLocked(updateSourceFullSync)
	d.mu.Unlock()

	// One concise summary per sync so a production deployment - which runs without
	// -debug - can confirm the sync ran and see what it found. Detailed
	// per-campaign skip reasons stay at DEBUG. To stop the identical block
	// repeating on every cycle (and on any redundant back-to-back sync), the
	// summary is published at INFO only when it says something new: the first
	// completed sync, a genuine change in the semantic result (fingerprint below),
	// or recovery from a real sync error. An identical successful no-op result is
	// published at DEBUG instead.
	var (
		summaryMsg  string
		summaryArgs []any
	)
	switch {
	case len(campaigns) > 0:
		names := make([]string, 0, len(campaigns))
		for _, c := range campaigns {
			names = append(names, c.Name)
		}
		summaryMsg = "Drops sync complete: tracking active drop campaigns"
		summaryArgs = []any{
			"tracked", len(campaigns), "dashboardCampaigns", dashboardCount,
			"recoveredFromInventory", recovered, "filteredByBlacklist", filteredByBlacklist,
			"filteredByGame", filteredByGame, "campaigns", names,
		}
	case dashboardCount == 0 && recovered == 0:
		summaryMsg = "Drops sync complete: Twitch reports no active drop campaigns for this account"
	default:
		summaryMsg = "Drops sync complete: active drop campaigns exist on Twitch but none are trackable " +
			"(all filtered out by date window, claim history, blacklist, or game filter; run with -debug for per-campaign reasons)"
		summaryArgs = []any{
			"dashboardCampaigns", dashboardCount, "recoveredFromInventory", recovered,
			"filteredByBlacklist", filteredByBlacklist, "filteredByGame", filteredByGame,
		}
	}

	fingerprint := syncSummaryFingerprint(campaigns, dashboardCount, recovered, filteredByBlacklist, filteredByGame)
	d.mu.Lock()
	changed := fingerprint != d.lastSummaryFingerprint
	recovering := d.lastSyncErr != ""
	d.lastSummaryFingerprint = fingerprint
	d.mu.Unlock()

	if changed || recovering {
		slog.Info(summaryMsg, summaryArgs...)
	} else {
		slog.Debug(summaryMsg, summaryArgs...)
	}

	d.recordSync(dashboardCount, recovered, len(campaigns), filteredByBlacklist, filteredByGame, time.Since(start), nil)

	d.updateStreamerCampaigns()

	// Edge effect of a SUCCESSFUL full sync only: alert on any newly-announced
	// relevant upcoming campaign. Runs last (active farming is already set up),
	// outside d.mu, and is bounded — a notification never rolls back the
	// published snapshot and never stalls the loop. The lightweight progress
	// sync never reaches here, so it can never notify.
	d.notifyUpcoming(upcoming)
}

// upcomingNotifyTimeout bounds the per-sync upcoming-campaign notification pass
// so a slow/failed notifier can never stall the full-sync loop for long.
// Package variable so tests can shrink it.
var upcomingNotifyTimeout = 15 * time.Second

// notifyUpcoming fires the opt-in upcoming-campaign alert for each RELEVANT
// upcoming campaign. The notifier owns durable dedupe, so calling it every sync
// is safe — only a genuinely new relevant campaign produces an alert. Runs
// OUTSIDE d.mu with a bounded context, so a slow or failing Discord send can
// neither block campaign readers nor stall the sync loop, and never mutates the
// published snapshot. A nil notifier (or no relevant upcoming campaigns) is a
// no-op.
func (d *DropsTracker) notifyUpcoming(upcoming []*models.Campaign) {
	notifier := d.upcomingNotifier
	if notifier == nil || len(upcoming) == 0 {
		return
	}
	relevant := d.filterRelevantUpcoming(upcoming)
	if len(relevant) == 0 {
		return
	}

	base := context.Background()
	d.mu.RLock()
	if d.ctx != nil {
		base = d.ctx
	}
	d.mu.RUnlock()

	ctx, cancel := context.WithTimeout(base, upcomingNotifyTimeout)
	defer cancel()
	for _, c := range relevant {
		notifier.NotifyUpcomingCampaign(ctx, c)
	}
}

// syncSummaryFingerprint is a deterministic semantic signature of a completed
// full sync's result, used to suppress republishing an identical "Drops sync
// complete" INFO summary every cycle. It includes only stable, order-independent
// facts: the sorted tracked campaign IDs and the salient pipeline counts. It
// excludes timestamps, map iteration order, pointer addresses, request IDs,
// volatile progress values and raw payloads, so two syncs that reached the same
// outcome produce the same fingerprint.
func syncSummaryFingerprint(campaigns []*models.Campaign, dashboardCount, recovered, filteredByBlacklist, filteredByGame int) string {
	ids := make([]string, 0, len(campaigns))
	for _, c := range campaigns {
		ids = append(ids, c.ID)
	}
	sort.Strings(ids)
	return fmt.Sprintf("tracked=%d[%s]|dashboard=%d|recovered=%d|blacklist=%d|game=%d",
		len(ids), strings.Join(ids, ","), dashboardCount, recovered, filteredByBlacklist, filteredByGame)
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
			// A stale persisted-query hash breaks this operation for EVERY
			// campaign (the hash is per-operation), so skipping per campaign
			// would collect an empty set, swap out d.campaigns, and stop drops
			// farming for the whole outage — while also wasting a full client-ID
			// walk per campaign. Abort the sync instead: the caller keeps the
			// previously tracked campaigns, exactly like a dashboard-level error.
			if errors.Is(err, api.ErrPersistedQueryNotFound) {
				return nil, nil, dashboardCount, fmt.Errorf("campaign details unavailable (stale Twitch query metadata): %w", err)
			}
			// Any other error is campaign-specific (campaign gone, malformed
			// response) — skip just this one and keep syncing the rest.
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
		status, err := d.client.ClaimDrop(drop)
		if err != nil {
			slog.Error("Failed to claim drop", "drop", drop.Name, "error", err)
			return false
		}
		if !status.Accepted() {
			// Rejected / missing / null / malformed: leave the drop unclaimed so a
			// later bounded sync can retry. No success event is recorded.
			slog.Warn("Drop claim not accepted by Twitch",
				"drop", drop.Name, "outcome", string(status), "retryable", status.Retryable())
			return false
		}
		if status.Fresh() {
			// Fresh authoritative claim: emit the user-facing success event once.
			events.Record(events.TypeDropClaimed, "", drop.Name)
			d.noteDropClaimed(drop.Name)
		} else {
			// Authoritative already-claimed: reconcile local state to claimed
			// WITHOUT emitting a duplicate user-facing success event.
			slog.Debug("Drop already claimed on Twitch; reconciling state without duplicate event",
				"drop", drop.Name)
		}
		return true
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

	claimed := extractClaimedRewards(inventory)
	if len(claimed) == 0 {
		return campaigns
	}

	for _, campaign := range campaigns {
		// nil clock = system clock; window classification is deterministic in
		// tests via the domain APIs, which accept an injectable clock.
		outcome := campaign.ApplyClaimHistoryRecords(claimed, nil)

		switch {
		case campaign.ClaimStatus == models.CampaignClaimStatusAlreadyClaimed:
			slog.Debug("Skipping drop campaign: already claimed (evidence-confirmed)",
				"campaign", campaign.Name, "campaignID", campaign.ID,
				"confirmed", outcome.ConfirmedNames)
		case len(outcome.ConfirmedNames) > 0:
			slog.Debug("Skipping already-claimed reward within active drop campaign",
				"campaign", campaign.Name, "campaignID", campaign.ID,
				"confirmed", outcome.ConfirmedNames)
		}
		if len(outcome.AmbiguousNames) > 0 {
			// Fail-open diagnostic: the reward is KEPT farmable because claim
			// history could not authoritatively prove a match for this
			// entitlement occurrence. The claim mutation is still gated by
			// Drop.CanClaim, so retaining it can never double-claim.
			slog.Debug("Retaining reward despite weak claim-history signal (kept farmable)",
				"campaign", campaign.Name, "campaignID", campaign.ID,
				"ambiguous", outcome.AmbiguousNames)
		}
	}

	return campaigns
}

// extractClaimedRewards reads the inventory's gameEventDrops -- Twitch's
// account-wide record of rewards already granted -- into evidence-rich
// ClaimedReward records. It captures the strongest identity each entry actually
// supplies: the Benefit/Reward ID when Twitch provides it (additive; the proven
// contract omits it), otherwise game + reward name only. The proven
// gameEventDrops shape carries no entitlement window, so a name-only record
// yields an ambiguous (fail-open, retained) match downstream rather than a false
// already-claimed — a genuinely new occurrence of a repeatable reward is never
// silently skipped. Records are deduplicated deterministically so repeated rows
// do not multiply annotations.
func extractClaimedRewards(inventory map[string]interface{}) []models.ClaimedReward {
	if inventory == nil {
		return nil
	}

	events, ok := inventory["gameEventDrops"].([]interface{})
	if !ok || events == nil {
		return nil
	}

	records := make([]models.ClaimedReward, 0, len(events))
	for _, e := range events {
		entry, ok := e.(map[string]interface{})
		if !ok {
			continue
		}

		name, _ := entry["name"].(string)
		benefitID := ""
		if benefit, ok := entry["benefit"].(map[string]interface{}); ok {
			if name == "" {
				name, _ = benefit["name"].(string)
			}
			// Additive: the Benefit/Reward ID is the strongest cross-variant
			// identity, used to confirm a claim without relying on the display
			// name. Usually absent in the current contract.
			benefitID, _ = benefit["id"].(string)
		}
		if name == "" && benefitID == "" {
			continue
		}

		gameID, _ := entry["gameId"].(string)
		if gameID == "" {
			if game, ok := entry["game"].(map[string]interface{}); ok {
				gameID, _ = game["id"].(string)
			}
		}

		var claimedAt time.Time
		if ts, ok := entry["lastAwardedAt"].(string); ok {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				claimedAt = t
			}
		}

		// No DropID/CampaignID: a gameEventDrops entry's id is the per-user event
		// id, NOT the campaign's timeBasedDrop id, so mapping it to DropID would
		// never match (or worse, collide). Window is left unknown (the proven
		// contract carries none). Evidence therefore resolves to Benefit (when an
		// id is present) or name-only.
		id := models.NewRewardIdentity(gameID, benefitID, "", "", "", name, 0, models.EntitlementWindow{})
		records = append(records, models.ClaimedReward{Identity: id, ClaimedAt: claimedAt})
	}

	return models.DedupeClaimedRewards(records)
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
			slog.Debug("Skipping drop campaign: matched drop-name blacklist",
				"campaign", campaign.Name, "campaignID", campaign.ID,
				"keyword", keyword, "matchedDrop", dropName)
			continue
		}
		kept = append(kept, campaign)
	}

	return kept
}

// resolveAllowedGameIDs computes the set of Twitch game IDs whose campaigns may
// be tracked this sync, from the two operator lists. Strict IDs
// (filterGameIDs) are candidate-independent and compared as exact, opaque,
// case-sensitive values. Names (filterGameNames) are best-effort: each is
// resolved to game ID(s) via the games actually present among this sync's
// campaign candidates, matched with TrimSpace+case-fold on game name AND
// displayName (never Contains/HasPrefix, never collapsing inner whitespace), so
// two distinct game IDs are never merged by a coincidental name. configured
// reports whether either list was non-empty (so the caller can distinguish "no
// filter" from "filter resolved to nothing").
func (d *DropsTracker) resolveAllowedGameIDs(candidates []*models.Campaign) (allowed map[string]struct{}, configured bool) {
	d.mu.RLock()
	ids := d.filterGameIDs
	names := d.filterGameNames
	d.mu.RUnlock()

	configured = len(ids) > 0 || len(names) > 0
	allowed = make(map[string]struct{}, len(ids)+len(names))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			allowed[id] = struct{}{} // exact, case-sensitive, no other normalization
		}
	}
	if len(names) == 0 {
		return allowed, configured
	}

	// alias(case-folded name/displayName) -> set of game IDs seen this sync.
	alias := make(map[string]map[string]struct{})
	note := func(k, id string) {
		if k = strings.ToLower(strings.TrimSpace(k)); k == "" {
			return
		}
		if alias[k] == nil {
			alias[k] = make(map[string]struct{})
		}
		alias[k][id] = struct{}{}
	}
	for _, c := range candidates {
		if c.Game == nil || c.Game.ID == "" {
			continue
		}
		note(c.Game.Name, c.Game.ID)
		note(c.Game.DisplayName, c.Game.ID)
	}

	for _, name := range names {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		switch hits := alias[key]; {
		case len(hits) == 0:
			// Unresolved this sync: never a reason to drop a campaign (fail-open).
			// This is a benign, expected per-name diagnostic — not an operational
			// failure — because a configured strict game ID (dropCampaignGameIDs)
			// already filters correctly on its own, independent of whether the
			// best-effort NAME resolves against this sync's candidates. So it stays
			// at DEBUG. The genuine name-only-nothing-resolved transition is warned
			// (once) in applyGameFilter's fail-open branch instead.
			slog.Debug("Drops game filter: configured game name did not resolve to a game ID this sync",
				"name", name, "reason", "game_name_unresolved")
		case len(hits) > 1:
			// Ambiguous: keep ALL matching IDs (fail-open for the affected games),
			// never pick one. Not a global track-all.
			resolved := sortedKeys(hits)
			slog.Warn("Drops game filter: game name is ambiguous; allowing every matching game ID",
				"name", name, "gameIDs", resolved, "reason", "game_name_ambiguous")
			for id := range hits {
				allowed[id] = struct{}{}
			}
		default:
			for id := range hits {
				allowed[id] = struct{}{}
			}
		}
	}
	return allowed, configured
}

// applyGameFilter is the single filtering point for the merged (dashboard +
// inventory-recovered) campaign set: it keeps only campaigns whose game ID is in
// the operator's allowed set, and returns how many it dropped. Fail-open by
// design: nothing configured -> track all; a name-only config that resolved to
// no IDs -> track all this cycle with one WARN; a campaign with no game ID ->
// kept (its identity is unknown, never proof it is foreign). It never touches
// the raw-inventory claim sweep, which runs earlier in syncCampaigns.
func (d *DropsTracker) applyGameFilter(campaigns []*models.Campaign) (kept []*models.Campaign, filtered int) {
	allowed, configured := d.resolveAllowedGameIDs(campaigns)
	failOpen := configured && len(allowed) == 0
	d.noteGameFilterResolution(failOpen)
	if !configured {
		return campaigns, 0 // F1: no filter configured -> unchanged (track all).
	}
	if len(allowed) == 0 {
		// F4: names configured but none resolved, and no strict IDs -> track all
		// this cycle rather than blindly dropping everything. The honest WARN is
		// emitted once by noteGameFilterResolution above (and re-emitted only when
		// the unresolved name set changes) so it doesn't repeat every sync.
		return campaigns, 0
	}

	d.mu.RLock()
	cfgIDs := d.filterGameIDs
	cfgNames := d.filterGameNames
	d.mu.RUnlock()

	kept = make([]*models.Campaign, 0, len(campaigns))
	for _, c := range campaigns {
		var gameID, gameName string
		if c.Game != nil {
			gameID, gameName = c.Game.ID, c.Game.Name
		}
		if !gameIDAllowed(allowed, c) {
			// Per-campaign filter decisions stay at DEBUG (PR #103) so a routine
			// sync does not spam INFO; the shared gameIDAllowed helper (PR #104) is
			// the single game-identity decision reused by the upcoming/notification
			// relevance path.
			slog.Debug("Skipping drop campaign: game not in configured drop-campaign game list",
				"campaign", c.Name, "campaignID", c.ID, "gameID", gameID, "gameName", gameName,
				"reason", "game_not_allowed", "configuredGameIDs", cfgIDs, "configuredGameNames", cfgNames)
			filtered++
			continue
		}
		if gameID == "" {
			// F2: unknown game identity -> keep; a name is never proof it's foreign.
			slog.Debug("Drops game filter: keeping campaign with no game ID",
				"campaign", c.Name, "campaignID", c.ID, "reason", "missing_game_id")
		}
		kept = append(kept, c)
	}
	return kept, filtered
}

// gameIDAllowed reports whether a campaign passes the operator's game filter for
// an ALREADY-resolved allowed-ID set: a campaign with no game ID is kept (its
// identity is unknown, never proof it is foreign — the fail-open policy),
// otherwise it is kept iff its exact, opaque game ID is in allowed. It is the
// single per-campaign game-identity decision, shared verbatim by the active
// filter (applyGameFilter) and the display-only upcoming/notification relevance
// path so both honor one contract — strict opaque ID equality, never substring,
// regex, or numeric coercion. Set-level cases (nothing configured, a name-only
// filter that resolved to no IDs) are the caller's to handle.
func gameIDAllowed(allowed map[string]struct{}, c *models.Campaign) bool {
	if c == nil || c.Game == nil || c.Game.ID == "" {
		return true
	}
	_, ok := allowed[c.Game.ID]
	return ok
}

// noteGameFilterResolution manages the once-per-transition WARN for the
// name-only, nothing-resolved fail-open condition (the F4 branch in
// applyGameFilter). failOpen is true when the operator configured game names,
// none resolved to a game ID this sync, and no strict game ID covers the set —
// so the filter falls open to tracking every game. The honest WARN is published
// only when this condition first appears or when the unresolved configured-name
// set changes; while it persists unchanged the note drops to DEBUG so it does not
// spam every sync; and when it clears (a name resolved, a strict ID was added, or
// the filter was removed) a single INFO transition is published. State guarded by
// mu; only ever driven from the fullSyncMu-serialized syncCampaigns pipeline.
func (d *DropsTracker) noteGameFilterResolution(failOpen bool) {
	var key string
	if failOpen {
		d.mu.RLock()
		names := append([]string(nil), d.filterGameNames...)
		d.mu.RUnlock()
		sort.Strings(names)
		// A non-empty, deterministic signature of the unresolved name set; the
		// leading marker keeps it distinct from the empty "resolved" state.
		key = "unresolved:" + strings.Join(names, "\x00")
	}

	d.mu.Lock()
	prev := d.lastGameFilterFailOpenKey
	d.lastGameFilterFailOpenKey = key
	d.mu.Unlock()

	switch {
	case key != "" && key != prev:
		// Newly fail-open, or the unresolved configured-name set changed.
		slog.Warn("Drops game filter configured but no game names resolved to an ID this sync; " +
			"tracking all games this cycle (set a strict Twitch game ID for candidate-independent filtering)")
	case key != "" && key == prev:
		// Same unresolved condition as the previous sync — don't repeat the WARN.
		slog.Debug("Drops game filter still resolving no configured game name to an ID; " +
			"tracking all games this cycle (unchanged since last sync)")
	case key == "" && prev != "":
		// Recovered: a name now resolves, a strict ID was added, or the filter was
		// removed. One INFO transition, then silence.
		slog.Info("Drops game filter: unresolved game-name fail-open condition cleared")
	}
}

// sortedKeys returns the map keys sorted, for deterministic log/test output.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (d *DropsTracker) claimAllDropsFromInventory() {
	inventory, err := d.getInventory()
	if err != nil {
		// Previously swallowed silently: during an outage the auto-claim of
		// ready drops just stopped with no trace. One WARN per sync interval.
		slog.Warn("Drops: cannot check inventory for claimable drops; skipping this cycle", "error", err)
		return
	}
	if inventory == nil {
		// A well-formed response without inventory data: nothing to claim.
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

			// Claim only on the authoritative server signal (CanClaim), never on
			// locally-counted watch minutes.
			if drop.CanClaim() {
				status, err := d.client.ClaimDrop(drop)
				switch {
				case err != nil:
					slog.Error("Failed to claim drop", "drop", drop.Name, "error", err)
				case status.Fresh():
					// Fresh authoritative claim: one success log + one event.
					slog.Info("Claimed drop", "drop", drop.Name)
					events.Record(events.TypeDropClaimed, "", drop.Name)
					d.noteDropClaimed(drop.Name)
				case status.Accepted():
					// Already-claimed reconciliation — no duplicate success event.
					slog.Debug("Drop already claimed on Twitch; no duplicate success event",
						"drop", drop.Name)
				default:
					slog.Warn("Drop claim not accepted by Twitch",
						"drop", drop.Name, "outcome", string(status), "retryable", status.Retryable())
				}
				time.Sleep(5 * time.Second)
			}
		}
	}
}

func (d *DropsTracker) updateStreamerCampaigns() {
	// Snapshot the streamer list together with the campaigns: it can be
	// replaced at runtime by UpdateStreamers, and this runs on both sync
	// goroutines.
	d.mu.RLock()
	campaigns := d.campaigns
	streamers := d.streamers
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

	for _, streamer := range streamers {
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

			// Single crediting gate: AllowsChannel returns true for unrestricted
			// campaigns, membership for restricted ones, and FALSE for an
			// ACLUnknown campaign (fail closed — an unresolved allowlist never
			// widens eligibility). This deliberately also covers the ACLUnknown
			// case, which IsChannelRestricted() reports as not-restricted; gating
			// on IsChannelRestricted alone would silently over-credit an unknown
			// ACL on any channel.
			if !campaign.AllowsChannel(streamer.ChannelID) {
				if campaign.IsChannelRestricted() {
					// Defensive: Twitch's per-channel CampaignIDs lookup should
					// already exclude ineligible campaigns; make a real exclusion
					// loud (privacy-safe: count, not the channel list).
					slog.Warn("Withholding drop progress: channel not in campaign's allowed-channel list",
						"streamer", streamer.Username, "channelID", streamer.ChannelID,
						"campaign", campaign.Name, "campaignID", campaign.ID,
						"allowedChannelCount", campaign.AllowedChannelCount())
				} else {
					slog.Debug("Withholding drop progress: campaign ACL unresolved (fail closed)",
						"streamer", streamer.Username,
						"campaign", campaign.Name, "campaignID", campaign.ID,
						"aclState", campaign.ACLState().String())
				}
				continue
			}

			if campaign.IsChannelRestricted() {
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
