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

// PastCampaigns implements web.DropCatalogProvider: expired campaigns from the
// durable catalog, ordered for grouped rendering.
func (m *Miner) PastCampaigns() ([]drops.CatalogRecord, error) {
	if m.dropCatalog == nil {
		return nil, nil
	}
	return m.dropCatalog.Past()
}
