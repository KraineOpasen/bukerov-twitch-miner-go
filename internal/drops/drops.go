package drops

import (
	"context"
	"log/slog"
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
}

type DropsTracker struct {
	client    twitchClient
	streamers []*models.Streamer
	settings  config.RateLimitSettings

	campaigns []*models.Campaign

	// Sync bookkeeping for SyncStatus (and LastSync); all guarded by mu.
	syncRuns           int
	lastSyncAt         time.Time
	lastDashboardCount int
	lastRecoveredCount int
	lastTrackedCount   int
	lastSyncErr        string

	// dropBlacklist holds case-insensitive keywords; a campaign is skipped
	// during rotation prioritization when any of its drop/reward names matches
	// one. Guarded by mu so it can be updated at runtime from the Settings page.
	dropBlacklist []string

	ctx    context.Context
	cancel context.CancelFunc

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

func (d *DropsTracker) Start(ctx context.Context) {
	d.mu.Lock()
	d.ctx, d.cancel = context.WithCancel(ctx)
	d.mu.Unlock()

	go d.loop()
}

// SyncNow runs a single campaign sync synchronously, refreshing the tracked
// campaign pool and SyncStatus before it returns. The background loop runs the
// same logic on each tick; this exposes it so a caller (or a test) can force an
// immediate refresh without waiting out the sync interval.
func (d *DropsTracker) SyncNow() {
	d.syncCampaigns()
}

func (d *DropsTracker) Stop() {
	d.mu.Lock()
	if d.cancel != nil {
		d.cancel()
	}
	d.mu.Unlock()
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

func (d *DropsTracker) loop() {
	syncInterval := time.Duration(d.settings.CampaignSyncInterval) * time.Minute

	d.syncCampaigns()

	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.syncCampaigns()
		}
	}
}

func (d *DropsTracker) syncCampaigns() {
	d.claimAllDropsFromInventory()

	campaigns, dashboardCount, err := d.getActiveCampaigns()
	if err != nil {
		slog.Error("Drops sync failed: could not fetch active drop campaigns from Twitch", "error", err)
		d.recordSync(0, 0, 0, err)
		return
	}

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

func (d *DropsTracker) getActiveCampaigns() ([]*models.Campaign, int, error) {
	dashboardCampaigns, err := d.getDropsDashboard("ACTIVE")
	if err != nil {
		return nil, 0, err
	}
	dashboardCount := len(dashboardCampaigns)

	slog.Debug("Drops sync: fetched active campaigns from dashboard",
		"dashboardCount", dashboardCount)

	var campaigns []*models.Campaign
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
			slog.Debug("Drops sync: skipping campaign outside its active date window",
				"campaign", campaign.Name, "campaignID", campaign.ID,
				"startAt", campaign.StartAt, "endAt", campaign.EndAt)
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
		"trackedCount", len(campaigns))

	return campaigns, dashboardCount, nil
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
		}
		return claimed
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
			for _, id := range streamer.Stream.CampaignIDs {
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
				slog.Info("Channel-restricted drop campaign assigned to streamer",
					"streamer", streamer.Username, "campaign", campaign.Name,
					"campaignID", campaign.ID, "allowedChannels", campaign.Channels)
			}

			streamerCampaigns = append(streamerCampaigns, campaign)
		}

		streamer.Stream.Campaigns = streamerCampaigns
	}
}
