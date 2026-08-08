package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
)

// healthPersistenceFakeProvider is a HealthProvider whose ApplyHealthSettings
// outcome the test fully controls (success or a specific error), so the
// HTTP-layer error-propagation contract of POST /api/health/settings can be
// exercised independent of any real persistence mechanism. It mirrors the
// commit-point discipline the production Miner implementation now has: on
// applyErr != nil, settings is left untouched.
type healthPersistenceFakeProvider struct {
	settings    config.HealthSettings
	applyErr    error
	applyCalls  int
	lastApplied config.HealthSettings
}

func (f *healthPersistenceFakeProvider) HealthSnapshot() health.Snapshot { return health.Snapshot{} }
func (f *healthPersistenceFakeProvider) RunCanaryNow()                   {}
func (f *healthPersistenceFakeProvider) CurrentHealthSettings() config.HealthSettings {
	return f.settings
}
func (f *healthPersistenceFakeProvider) ApplyHealthSettings(cfg config.HealthSettings) error {
	f.applyCalls++
	f.lastApplied = cfg
	if f.applyErr != nil {
		return f.applyErr
	}
	f.settings = cfg
	return nil
}

// newHealthPersistenceTestServer reuses the package's standard test server
// (handlers_statistics_test.go) and attaches the given fake HealthProvider —
// the same construction every other Health Center handler test in this
// package already relies on for base-layout fields to render correctly.
func newHealthPersistenceTestServer(t *testing.T, provider *healthPersistenceFakeProvider) *Server {
	t.Helper()
	srv := newStatsTestServer(t)
	srv.SetHealthProvider(provider)
	return srv
}

func postHealthSettingsForm(t *testing.T, srv *Server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/health/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)
	return rec
}

// TestHandleAPIHealthSettingsApplyFailureReturnsNonSuccess is the P3 web-layer
// invariant: when the provider's durable persistence fails,
// POST /api/health/settings must answer non-2xx, never the 200 the htmx
// partial render always produced before this pass (mechanism #2 of the P3
// diagnosis — the handler could not previously see ApplyHealthSettings'
// (void) outcome at all, so it always rendered "success"). This is also the
// M3 mutation shape: swallowing the propagated error back into an
// unconditional renderHealthPartial call must fail this test.
func TestHandleAPIHealthSettingsApplyFailureReturnsNonSuccess(t *testing.T) {
	provider := &healthPersistenceFakeProvider{
		settings: config.HealthSettings{CanaryChannel: "before_channel", CanaryIntervalMinutes: 120, CanaryMaxStalenessHours: 6},
		applyErr: errors.New("persist failed: simulated disk error"),
	}
	srv := newHealthPersistenceTestServer(t, provider)

	form := url.Values{}
	form.Set("section", "canary")
	form.Set("canaryEnabled", "on")
	form.Set("canaryChannel", "after_channel")
	form.Set("canaryIntervalMinutes", "180")
	form.Set("canaryMaxStalenessHours", "12")
	rec := postHealthSettingsForm(t, srv, form)

	if rec.Code >= 200 && rec.Code < 300 {
		t.Fatalf("POST /api/health/settings on apply failure = %d, want non-2xx; body=%q", rec.Code, rec.Body.String())
	}
	if provider.applyCalls != 1 {
		t.Fatalf("ApplyHealthSettings called %d times, want 1", provider.applyCalls)
	}
	// The rejected candidate must not be echoed back as CurrentHealthSettings:
	// the fake only commits on nil error, so this pins that a failed apply
	// leaves the provider's own published settings exactly as they were.
	if got := provider.CurrentHealthSettings(); got.CanaryChannel != "before_channel" {
		t.Errorf("provider settings changed despite the reported apply failure: got %+v", got)
	}
}

// TestHandleAPIHealthSettingsApplySuccessRendersPartial preserves the
// existing successful hot-apply behavior: a POST whose ApplyHealthSettings
// succeeds must still answer 200 with the re-rendered Health Center partial,
// exactly as before this pass.
func TestHandleAPIHealthSettingsApplySuccessRendersPartial(t *testing.T) {
	provider := &healthPersistenceFakeProvider{
		settings: config.HealthSettings{CanaryChannel: "before_channel", CanaryIntervalMinutes: 120, CanaryMaxStalenessHours: 6},
	}
	srv := newHealthPersistenceTestServer(t, provider)

	form := url.Values{}
	form.Set("section", "canary")
	form.Set("canaryChannel", "after_channel")
	form.Set("canaryIntervalMinutes", "180")
	form.Set("canaryMaxStalenessHours", "12")
	rec := postHealthSettingsForm(t, srv, form)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/health/settings on apply success = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if provider.applyCalls != 1 {
		t.Fatalf("ApplyHealthSettings called %d times, want 1", provider.applyCalls)
	}
	if provider.lastApplied.CanaryChannel != "after_channel" {
		t.Errorf("applied CanaryChannel = %q, want after_channel", provider.lastApplied.CanaryChannel)
	}
}

// TestHandleAPIHealthSettingsWatchdogSectionPreservesCanaryFields guards the
// canary/watchdog section patch semantics the P3 contract requires to stay
// intact: posting only the watchdog form must not clobber the current canary
// fields (each section form posts only its own fields — see
// handleAPIHealthSettings' healthFormMu doc comment).
func TestHandleAPIHealthSettingsWatchdogSectionPreservesCanaryFields(t *testing.T) {
	provider := &healthPersistenceFakeProvider{
		settings: config.HealthSettings{
			CanaryEnabled: true, CanaryChannel: "kept_channel", CanaryIntervalMinutes: 240, CanaryMaxStalenessHours: 24,
			WatchdogEnabled: false, WatchdogStallDelayMinutes: 20, WatchdogStallConfirmations: 3,
			WatchdogRecoveryCooldownMinutes: 5, WatchdogAvoidTTLMinutes: 60, WatchdogRearmHours: 6,
		},
	}
	srv := newHealthPersistenceTestServer(t, provider)

	form := url.Values{}
	form.Set("section", "watchdog")
	form.Set("watchdogEnabled", "on")
	form.Set("watchdogStallDelayMinutes", "30")
	form.Set("watchdogStallConfirmations", "4")
	form.Set("watchdogRecoveryCooldownMinutes", "10")
	form.Set("watchdogAvoidTTLMinutes", "90")
	form.Set("watchdogRearmHours", "8")
	rec := postHealthSettingsForm(t, srv, form)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	applied := provider.lastApplied
	if applied.CanaryChannel != "kept_channel" || applied.CanaryIntervalMinutes != 240 || applied.CanaryMaxStalenessHours != 24 || !applied.CanaryEnabled {
		t.Errorf("watchdog-section save must preserve canary fields untouched: got %+v", applied)
	}
	if !applied.WatchdogEnabled || applied.WatchdogStallDelayMinutes != 30 {
		t.Errorf("watchdog fields not applied from the posted form: got %+v", applied)
	}
}

// TestHandleAPIHealthSettingsNilProviderRendersPartial pins the existing
// nil-provider behavior unchanged: with no HealthProvider attached the
// endpoint renders the (empty/unavailable) partial and answers 200, exactly
// as every other nil-provider path in this file already does — this pass
// must not turn that into an error response.
func TestHandleAPIHealthSettingsNilProviderRendersPartial(t *testing.T) {
	srv := newStatsTestServer(t) // no health provider attached

	form := url.Values{}
	form.Set("section", "canary")
	rec := postHealthSettingsForm(t, srv, form)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/health/settings with no provider = %d, want 200 (unchanged nil-provider behavior); body=%q", rec.Code, rec.Body.String())
	}
}
