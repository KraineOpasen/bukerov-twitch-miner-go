package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

type recordingPolicyProvider struct {
	setErr       error
	setCalls     int
	readCalls    int
	rewardKey    string
	rule         config.DropRule
	currentRules map[string]config.DropRule
}

func (p *recordingPolicyProvider) PolicySnapshot() (policy.Mode, []policy.Decision) {
	p.readCalls++
	return policy.DefaultMode, nil
}

func (p *recordingPolicyProvider) CurrentCampaignPolicy() (string, map[string]config.DropRule) {
	p.readCalls++
	return string(policy.DefaultMode), p.currentRules
}

func (*recordingPolicyProvider) ApplyCampaignPolicy(string) {}

func (p *recordingPolicyProvider) SetDropRule(rewardKey string, rule config.DropRule) error {
	p.setCalls++
	p.rewardKey = rewardKey
	p.rule = rule
	if p.setErr != nil {
		return p.setErr
	}
	if p.currentRules == nil {
		p.currentRules = make(map[string]config.DropRule)
	}
	if rule == (config.DropRule{}) {
		delete(p.currentRules, rewardKey)
	} else {
		p.currentRules[rewardKey] = rule
	}
	return nil
}

func postPolicyDropRule(t *testing.T, srv *Server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/policy/drop-rule", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.handleAPIPolicyDropRule(rec, req)
	return rec
}

func TestHandleAPIPolicyDropRuleSuccessPreservesRenderAndAllFields(t *testing.T) {
	srv := newRenderServer(t)
	provider := &recordingPolicyProvider{}
	srv.SetPolicyProvider(provider)

	rec := postPolicyDropRule(t, srv, url.Values{
		"rewardKey":            {"g1::cool skin"},
		"skip":                 {"on"},
		"highPriority":         {"true"},
		"alwaysFinishStarted":  {"on"},
		"nextRewardOnly":       {"true"},
		"ignoreSubscriberOnly": {"on"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	want := config.DropRule{
		Skip:                 true,
		HighPriority:         true,
		AlwaysFinishStarted:  true,
		NextRewardOnly:       true,
		IgnoreSubscriberOnly: true,
	}
	if provider.setCalls != 1 || provider.rewardKey != "g1::cool skin" || provider.rule != want {
		t.Fatalf("SetDropRule calls=%d key=%q rule=%+v, want one exact call with %+v", provider.setCalls, provider.rewardKey, provider.rule, want)
	}
	if provider.readCalls != 2 {
		t.Fatalf("successful handler read calls = %d, want 2 for immediate refreshed render", provider.readCalls)
	}
}

func TestHandleAPIPolicyDropRuleResetUsesZeroValue(t *testing.T) {
	srv := newRenderServer(t)
	provider := &recordingPolicyProvider{currentRules: map[string]config.DropRule{
		"g1::cool skin": {Skip: true},
	}}
	srv.SetPolicyProvider(provider)

	rec := postPolicyDropRule(t, srv, url.Values{
		"rewardKey": {"g1::cool skin"},
		"reset":     {"true"},
		"skip":      {"on"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if provider.rule != (config.DropRule{}) {
		t.Fatalf("reset rule = %+v, want exact zero value", provider.rule)
	}
	if _, ok := provider.currentRules["g1::cool skin"]; ok {
		t.Fatal("reset did not remove the rule before successful render")
	}
}

func TestHandleAPIPolicyDropRuleFailuresDoNotRenderOrLeak(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		err        error
		wantStatus int
	}{
		{name: "validation", key: "G1 :: Cool Skin", err: models.ErrInvalidRewardKey, wantStatus: http.StatusBadRequest},
		{name: "missing key", key: "", err: models.ErrInvalidRewardKey, wantStatus: http.StatusBadRequest},
		{name: "persistence", key: "g1::cool skin", err: fmt.Errorf("rename /private/config.json: secret-token"), wantStatus: http.StatusInternalServerError},
		{name: "shutdown", key: "g1::cool skin", err: settings.ErrShuttingDown, wantStatus: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newRenderServer(t)
			provider := &recordingPolicyProvider{setErr: tt.err}
			srv.SetPolicyProvider(provider)

			rec := postPolicyDropRule(t, srv, url.Values{"rewardKey": {tt.key}, "skip": {"on"}})
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if provider.setCalls != 1 {
				t.Fatalf("SetDropRule calls = %d, want 1", provider.setCalls)
			}
			if provider.readCalls != 0 {
				t.Fatalf("failed handler rendered/read back misleading state %d time(s)", provider.readCalls)
			}
			if body := rec.Body.String(); strings.Contains(body, "/private/config.json") || strings.Contains(body, "secret-token") {
				t.Fatalf("failure body leaked internal persistence detail: %q", body)
			}
		})
	}
}

func TestHandleAPIPolicyDropRuleNilProviderRemainsCompatible(t *testing.T) {
	srv := newRenderServer(t)
	rec := postPolicyDropRule(t, srv, url.Values{"rewardKey": {"g1::cool skin"}, "skip": {"on"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("nil-provider status = %d, want existing 200 behavior", rec.Code)
	}
}

func TestHandleAPIPolicyDropRuleRejectsNonPOST(t *testing.T) {
	srv := newRenderServer(t)
	rec := httptest.NewRecorder()
	srv.handleAPIPolicyDropRule(rec, httptest.NewRequest(http.MethodGet, "/api/policy/drop-rule", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
