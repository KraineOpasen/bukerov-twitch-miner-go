package miner

import (
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// UpcomingCampaigns implements web.DropCatalogProvider: the not-yet-started
// campaigns Twitch's dashboard listed (display-only, never farmed).
func (m *Miner) UpcomingCampaigns() []*models.Campaign {
	if m.dropsTracker == nil {
		return nil
	}
	return m.dropsTracker.UpcomingCampaigns()
}

// RelevantUpcomingCampaigns implements web.DropCatalogProvider: the not-yet-
// started campaigns Twitch listed, filtered to the operator's game filter
// (display-only relevance). Foreign upcoming campaigns are hidden from the tab.
func (m *Miner) RelevantUpcomingCampaigns() []*models.Campaign {
	if m.dropsTracker == nil {
		return nil
	}
	return m.dropsTracker.RelevantUpcomingCampaigns()
}

// CampaignSyncStatus implements web.DropCatalogProvider: the last full-sync
// bookkeeping the Upcoming tab reads to render honest never-synced / empty /
// stale states.
func (m *Miner) CampaignSyncStatus() drops.SyncStatus {
	if m.dropsTracker == nil {
		return drops.SyncStatus{}
	}
	return m.dropsTracker.SyncStatus()
}

// PastCampaigns implements web.DropCatalogProvider: expired campaigns from the
// durable catalog, ordered for grouped rendering.
func (m *Miner) PastCampaigns() ([]drops.CatalogRecord, error) {
	if m.dropCatalog == nil {
		return nil, nil
	}
	return m.dropCatalog.Past()
}
