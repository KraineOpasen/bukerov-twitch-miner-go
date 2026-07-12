package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

type stubRewardsProvider struct {
	rewards    []*models.CustomReward
	listErr    error
	redeemErr  error
	lastRedeem [3]string // username, rewardID, textInput
	auto       config.AutoRedeemConfig
	setAuto    config.AutoRedeemConfig
	setAutoErr error
}

func (s *stubRewardsProvider) ListCustomRewards(u string) ([]*models.CustomReward, error) {
	return s.rewards, s.listErr
}
func (s *stubRewardsProvider) RedeemCustomReward(u, id, text string) error {
	s.lastRedeem = [3]string{u, id, text}
	return s.redeemErr
}
func (s *stubRewardsProvider) GetAutoRedeem(u string) config.AutoRedeemConfig { return s.auto }
func (s *stubRewardsProvider) SetAutoRedeem(u string, c config.AutoRedeemConfig) error {
	s.setAuto = c
	return s.setAutoErr
}

func newTestServer(p RewardsProvider) *Server {
	s := &Server{status: NewStatusBroadcaster()}
	s.rewardsProvider = p
	return s
}

func TestHandleListRewardsHTTP(t *testing.T) {
	p := &stubRewardsProvider{
		rewards: []*models.CustomReward{
			{ID: "a", Title: "Hydrate", Cost: 100, IsEnabled: true, IsInStock: true},
		},
		auto: config.AutoRedeemConfig{Enabled: true, Budget: 5000, RewardIDs: []string{"a"}},
	}
	s := newTestServer(p)

	req := httptest.NewRequest(http.MethodGet, "/api/streamer/foo/rewards", nil)
	rr := httptest.NewRecorder()
	s.handleAPIStreamerRewards(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp rewardsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(resp.Rewards) != 1 || resp.Rewards[0].ID != "a" || !resp.Rewards[0].Available {
		t.Errorf("unexpected rewards: %+v", resp.Rewards)
	}
	if !resp.AutoRedeem.Enabled || resp.AutoRedeem.Budget != 5000 {
		t.Errorf("unexpected auto config: %+v", resp.AutoRedeem)
	}
}

func TestHandleRedeemFailureIsHTTP200(t *testing.T) {
	p := &stubRewardsProvider{redeemErr: errors.New("not enough channel points")}
	s := newTestServer(p)

	body := strings.NewReader(`{"rewardId":"a","textInput":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/streamer/foo/redeem", body)
	rr := httptest.NewRecorder()
	s.handleAPIStreamerRewards(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("failed redemptions should return 200, got %d", rr.Code)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["success"] != false {
		t.Errorf("expected success=false, got %v", resp["success"])
	}
	if resp["message"] != "not enough channel points" {
		t.Errorf("expected error message passed through, got %v", resp["message"])
	}
	if p.lastRedeem != [3]string{"foo", "a", "hi"} {
		t.Errorf("provider not called with expected args: %v", p.lastRedeem)
	}
}

func TestHandleRedeemSuccess(t *testing.T) {
	p := &stubRewardsProvider{}
	s := newTestServer(p)

	req := httptest.NewRequest(http.MethodPost, "/api/streamer/foo/redeem", strings.NewReader(`{"rewardId":"a"}`))
	rr := httptest.NewRecorder()
	s.handleAPIStreamerRewards(rr, req)

	var resp map[string]interface{}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["success"] != true {
		t.Errorf("expected success=true, got %v", resp["success"])
	}
}

func TestHandleAutoRedeemSave(t *testing.T) {
	p := &stubRewardsProvider{}
	s := newTestServer(p)

	body := strings.NewReader(`{"enabled":true,"budget":3000,"rewardIds":["a","b"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/streamer/foo/auto-redeem", body)
	rr := httptest.NewRecorder()
	s.handleAPIStreamerRewards(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !p.setAuto.Enabled || p.setAuto.Budget != 3000 || len(p.setAuto.RewardIDs) != 2 {
		t.Errorf("SetAutoRedeem got %+v", p.setAuto)
	}
}

func TestHandleRewardsUnknownAction(t *testing.T) {
	s := newTestServer(&stubRewardsProvider{})
	req := httptest.NewRequest(http.MethodGet, "/api/streamer/foo/bogus", nil)
	rr := httptest.NewRecorder()
	s.handleAPIStreamerRewards(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown action, got %d", rr.Code)
	}
}

func TestHandleRewardsNoProvider(t *testing.T) {
	s := &Server{status: NewStatusBroadcaster()}
	req := httptest.NewRequest(http.MethodGet, "/api/streamer/foo/rewards", nil)
	rr := httptest.NewRecorder()
	s.handleAPIStreamerRewards(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 without provider, got %d", rr.Code)
	}
}
