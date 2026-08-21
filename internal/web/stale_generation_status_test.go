package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

// TestRetiredGenerationRefusalsMapToServiceUnavailable pins the HTTP half of
// the stale-generation mutation fence, in the package that owns it.
//
// internal/web cannot import internal/miner (miner imports web), so it cannot
// exercise a real generation. What it CAN pin — and what the miner-side tests
// cannot — is the status mapping: when a provider fails closed with
// settings.ErrShuttingDown, every configuration-mutation route must answer 503
// (retry is safe, nothing changed) rather than 200, 500, or 400.
func TestRetiredGenerationRefusalsMapToServiceUnavailable(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		body    string
		ctype   string
		install func(*Server)
	}{
		{
			name:  "policy mode",
			path:  "/api/policy/mode",
			body:  "mode=SMART",
			ctype: "application/x-www-form-urlencoded",
			install: func(s *Server) {
				s.SetPolicyProvider(&f3Policy{rules: map[string]config.DropRule{}, err: settings.ErrShuttingDown})
			},
		},
		{
			name:  "drop rule",
			path:  "/api/policy/drop-rule",
			body:  "rewardKey=some-key&skip=on",
			ctype: "application/x-www-form-urlencoded",
			install: func(s *Server) {
				s.SetPolicyProvider(&f3Policy{rules: map[string]config.DropRule{}, err: settings.ErrShuttingDown})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(config.AnalyticsSettings{Host: "127.0.0.1", Port: 0, Refresh: 5, DaysAgo: 30}, "tester", t.TempDir(), nil, nil)
			tc.install(srv)

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.ctype)
			rec := httptest.NewRecorder()
			srv.handler().ServeHTTP(rec, req)

			if rec.Code == http.StatusOK {
				t.Errorf("POST %s with a fail-closed provider = 200 OK; a refused mutation "+
					"must never be reported as success", tc.path)
			}
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("POST %s = %d, want %d", tc.path, rec.Code, http.StatusServiceUnavailable)
			}
		})
	}
}

// TestPausedLifecycleAnswersConflictNotUnavailable pins the distinction the
// change draws between the two refusing states.
//
// Paused/stopped is a lifecycle CONFLICT: the operator can fix it by resuming,
// and this repo already answers that with a localized 409 on the settings
// routes. The replacement/startup gap is a transient UNAVAILABILITY, answered
// 503. Both refuse; reporting one as the other would be truthful about the
// outcome and wrong about the cause.
func TestPausedLifecycleAnswersConflictNotUnavailable(t *testing.T) {
	for _, observed := range []lifecycle.ObservedState{lifecycle.ObservedPaused, lifecycle.ObservedStopped} {
		t.Run(string(observed), func(t *testing.T) {
			srv := NewServer(config.AnalyticsSettings{Host: "127.0.0.1", Port: 0, Refresh: 5, DaysAgo: 30}, "tester", t.TempDir(), nil, nil)
			srv.SetPolicyProvider(&f3Policy{rules: map[string]config.DropRule{}})
			srv.SetLifecycleController(&fakeLifecycleController{snap: lifecycle.Snapshot{Observed: observed}})

			req := httptest.NewRequest(http.MethodPost, "/api/policy/mode", strings.NewReader("mode=SMART"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			srv.handler().ServeHTTP(rec, req)

			if rec.Code == http.StatusOK {
				t.Errorf("POST /api/policy/mode while %s = 200 OK; a mutation against a "+
					"torn-down generation must not report success", observed)
			}
			if rec.Code != http.StatusConflict {
				t.Errorf("POST /api/policy/mode while %s = %d, want %d (lifecycle conflict, "+
					"not transient unavailability)", observed, rec.Code, http.StatusConflict)
			}
		})
	}
}
