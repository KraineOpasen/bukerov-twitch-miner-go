package miner

import (
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
)

// PastCampaigns implements web.DropCatalogProvider: expired campaigns from the
// durable catalog, ordered for grouped rendering.
func (m *Miner) PastCampaigns() ([]drops.CatalogRecord, error) {
	if m.dropCatalog == nil {
		return nil, nil
	}
	return m.dropCatalog.Past()
}
