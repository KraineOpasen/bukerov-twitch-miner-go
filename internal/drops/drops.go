package drops

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/PatrickWalther/twitch-miner-go/internal/api"
	"github.com/PatrickWalther/twitch-miner-go/internal/config"
	"github.com/PatrickWalther/twitch-miner-go/internal/constants"
	"github.com/PatrickWalther/twitch-miner-go/internal/events"
	"github.com/PatrickWalther/twitch-miner-go/internal/models"
)

type DropsTracker struct {
	client    *api.TwitchClient
	streamers []*models.Streamer
	settings  config.RateLimitSettings

	campaigns []*models.Campaign

	ctx    context.Context
	cancel context.CancelFunc

	mu sync.RWMutex
}

func NewDropsTracker(
	client *api.TwitchClient,
	streamers []*models.Streamer,
	settings config.RateLimitSettings,
) *DropsTracker {
	return &DropsTracker{
		client:    client,
		streamers: streamers,
		settings:  settings,
	}
}

func (d *DropsTracker) Start(ctx context.Context) {
	d.mu.Lock()
	d.ctx, d.cancel = context.WithCancel(ctx)
	d.mu.Unlock()

	go d.loop()
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

	campaigns, err := d.getActiveCampaigns()
	if err != nil {
		slog.Error("Failed to get campaigns", "error", err)
		return
	}

	campaigns = d.syncWithInventory(campaigns)
	campaigns = d.applyClaimHistory(campaigns)

	d.mu.Lock()
	d.campaigns = campaigns
	d.mu.Unlock()

	d.updateStreamerCampaigns()
}

func (d *DropsTracker) getActiveCampaigns() ([]*models.Campaign, error) {
	dashboardCampaigns, err := d.getDropsDashboard("ACTIVE")
	if err != nil {
		return nil, err
	}

	var campaigns []*models.Campaign
	for _, c := range dashboardCampaigns {
		campaign := models.NewCampaignFromGQL(c)
		if campaign.DateMatch {
			campaign.ClearClaimedDrops()
			if len(campaign.Drops) > 0 {
				campaigns = append(campaigns, campaign)
			}
		}
	}

	return campaigns, nil
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

	for _, campaign := range campaigns {
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
				campaign.SyncDrops(drops, func(drop *models.Drop) bool {
					claimed, err := d.client.ClaimDrop(drop)
					if err != nil {
						slog.Error("Failed to claim drop", "drop", drop.Name, "error", err)
						return false
					}
					if claimed {
						events.Record(events.TypeDropClaimed, "", drop.Name)
					}
					return claimed
				})
			}

			campaign.ClearClaimedDrops()
			break
		}
	}

	return campaigns
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
