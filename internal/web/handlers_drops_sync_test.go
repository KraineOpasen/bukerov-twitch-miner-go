package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// fakeCampaignsProvider is a minimal CampaignsProvider for exercising the manual
// "Sync Drops now" endpoint without a live tracker.
type fakeCampaignsProvider struct {
	status drops.SyncStatus
	manual drops.ManualSyncResult
	calls  int
}

func (f *fakeCampaignsProvider) Campaigns() []*models.Campaign { return nil }
func (f *fakeCampaignsProvider) SyncStatus() drops.SyncStatus  { return f.status }
func (f *fakeCampaignsProvider) RequestManualSync() drops.ManualSyncResult {
	f.calls++
	return f.manual
}

func TestHandleAPIDropsSyncTriggered(t *testing.T) {
	srv := newStatsTestServer(t)
	prov := &fakeCampaignsProvider{
		status: drops.SyncStatus{Runs: 3, TrackedCampaigns: 1, DashboardCampaigns: 0, RecoveredCampaigns: 1, LastSyncAt: time.Now()},
		manual: drops.ManualSyncResult{Triggered: true},
	}
	prov.manual.Status = prov.status
	srv.SetCampaignsProvider(prov)

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/drops/sync", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["triggered"] != true {
		t.Errorf("triggered = %v, want true", got["triggered"])
	}
	if got["trackedCampaigns"].(float64) != 1 {
		t.Errorf("trackedCampaigns = %v, want 1", got["trackedCampaigns"])
	}
	if prov.calls != 1 {
		t.Errorf("RequestManualSync called %d times, want 1", prov.calls)
	}
}

func TestHandleAPIDropsSyncCooldownReported(t *testing.T) {
	srv := newStatsTestServer(t)
	prov := &fakeCampaignsProvider{
		manual: drops.ManualSyncResult{Triggered: false, RetryAfter: 12 * time.Second},
	}
	srv.SetCampaignsProvider(prov)

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/drops/sync", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["triggered"] != false {
		t.Errorf("triggered = %v, want false", got["triggered"])
	}
	if got["retryAfterSecs"].(float64) != 12 {
		t.Errorf("retryAfterSecs = %v, want 12", got["retryAfterSecs"])
	}
}

func TestHandleAPIDropsSyncRejectsGET(t *testing.T) {
	srv := newStatsTestServer(t)
	srv.SetCampaignsProvider(&fakeCampaignsProvider{})

	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/drops/sync", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rec.Code)
	}
}
