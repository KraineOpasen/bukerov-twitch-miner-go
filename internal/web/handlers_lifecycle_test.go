package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
)

// fakeLifecycleController is the canned test double for LifecycleController:
// Snapshot is fixed per test, and every command call records itself and
// returns the configured SubmitResult, so tests can assert both the HTTP
// mapping AND that the controller was (or wasn't) actually invoked.
type fakeLifecycleController struct {
	mu     sync.Mutex
	snap   lifecycle.Snapshot
	result lifecycle.SubmitResult
	calls  []string
}

func (f *fakeLifecycleController) Snapshot() lifecycle.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeLifecycleController) call(name string) lifecycle.SubmitResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	return f.result
}

func (f *fakeLifecycleController) Pause(context.Context) lifecycle.SubmitResult {
	return f.call("pause")
}
func (f *fakeLifecycleController) Resume(context.Context) lifecycle.SubmitResult {
	return f.call("resume")
}
func (f *fakeLifecycleController) Restart(context.Context) lifecycle.SubmitResult {
	return f.call("restart")
}
func (f *fakeLifecycleController) Stop(context.Context) lifecycle.SubmitResult { return f.call("stop") }

func (f *fakeLifecycleController) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeLifecycleController) lastCall() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

func jsonRequest(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Accept", "application/json")
	return req
}

func htmxRequest(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("HX-Request", "true")
	return req
}

// ---- GET /api/lifecycle -----------------------------------------------

func TestHandleAPILifecycleGETJSON(t *testing.T) {
	s := newRenderServer(t)
	snap := lifecycle.Snapshot{
		Desired:    lifecycle.DesiredRunning,
		Observed:   lifecycle.ObservedRunning,
		Transition: lifecycle.TransitionNone,
		Generation: 3,
		CommandID:  "cmd-1",
		Reason:     lifecycle.ReasonUser,
		StartedAt:  time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC),
		Capabilities: lifecycle.Capabilities{
			CanPause: true, CanResume: false, CanRestart: true, CanStop: true,
		},
		Override: true,
	}
	ctrl := &fakeLifecycleController{snap: snap}
	s.SetLifecycleController(ctrl)
	s.SetLifecycleUpdateState(func() LifecycleUpdateState {
		return LifecycleUpdateState{State: "available", Version: "v9.9.9"}
	})

	rec := httptest.NewRecorder()
	s.handleAPILifecycle(rec, jsonRequest(http.MethodGet, "/api/lifecycle"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp lifecycleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}

	if resp.DesiredState != "running" || resp.ObservedState != "running" || resp.Transition != "none" {
		t.Errorf("state fields wrong: %+v", resp)
	}
	if resp.Generation != 3 || resp.CommandID != "cmd-1" || resp.Reason != "user" {
		t.Errorf("identity fields wrong: %+v", resp)
	}
	if resp.StartedAt == nil || *resp.StartedAt == "" {
		t.Errorf("startedAt should be populated: %+v", resp)
	}
	if !resp.CanPause || resp.CanResume || !resp.CanRestart || !resp.CanStop {
		t.Errorf("capability fields wrong: %+v", resp)
	}
	if !resp.Override {
		t.Error("override should be true")
	}
	if resp.UpdateState != "available" || resp.UpdateVersion != "v9.9.9" {
		t.Errorf("update state wrong: %+v", resp)
	}
	if resp.Version == "" {
		t.Error("version must always be populated")
	}
	if resp.Outcome != "" || resp.Error != "" {
		t.Errorf("GET must not carry outcome/error: %+v", resp)
	}

	// nextRetryAt must be JSON null (not omitted, not a string) when zero.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("raw decode: %v", err)
	}
	if v, ok := raw["nextRetryAt"]; !ok {
		t.Error("nextRetryAt key must always be present")
	} else if string(v) != "null" {
		t.Errorf("nextRetryAt = %s, want null for zero time", v)
	}
}

func TestHandleAPILifecycleGETNextRetryAtValue(t *testing.T) {
	s := newRenderServer(t)
	retry := time.Date(2026, 7, 31, 11, 30, 0, 0, time.UTC)
	ctrl := &fakeLifecycleController{snap: lifecycle.Snapshot{
		Observed:    lifecycle.ObservedFailed,
		NextRetryAt: retry,
	}}
	s.SetLifecycleController(ctrl)

	rec := httptest.NewRecorder()
	s.handleAPILifecycle(rec, jsonRequest(http.MethodGet, "/api/lifecycle"))

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var got string
	if err := json.Unmarshal(raw["nextRetryAt"], &got); err != nil {
		t.Fatalf("nextRetryAt not a string: %s", raw["nextRetryAt"])
	}
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Errorf("nextRetryAt %q is not RFC3339: %v", got, err)
	}
}

func TestHandleAPILifecycleGETNilControllerUnavailable(t *testing.T) {
	s := newRenderServer(t)
	rec := httptest.NewRecorder()
	s.handleAPILifecycle(rec, jsonRequest(http.MethodGet, "/api/lifecycle"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHandleAPILifecycleGETWrongMethod(t *testing.T) {
	s := newRenderServer(t)
	rec := httptest.NewRecorder()
	s.handleAPILifecycle(rec, httptest.NewRequest(http.MethodPost, "/api/lifecycle", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// ---- POST /api/lifecycle/{action} outcome mapping -----------------------

func TestHandleAPILifecycleActionOutcomeMapping(t *testing.T) {
	cases := []struct {
		name        string
		result      lifecycle.SubmitResult
		wantStatus  int
		wantOutcome string
	}{
		{"accepted", lifecycle.SubmitResult{Outcome: lifecycle.OutcomeAccepted, CommandID: "c1"}, http.StatusAccepted, "accepted"},
		{"idempotent", lifecycle.SubmitResult{Outcome: lifecycle.OutcomeIdempotent, CommandID: "c1"}, http.StatusOK, "idempotent"},
		{"rejected", lifecycle.SubmitResult{Outcome: lifecycle.OutcomeRejected, Err: errors.New("process restart required")}, http.StatusConflict, "rejected"},
		{"persist-sentinel", lifecycle.SubmitResult{Outcome: lifecycle.OutcomeRejected, Err: errors.Join(ErrLifecyclePersist, errors.New("db is busy"))}, http.StatusInternalServerError, "rejected"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRenderServer(t)
			ctrl := &fakeLifecycleController{result: tc.result}
			s.SetLifecycleController(ctrl)

			rec := httptest.NewRecorder()
			s.handleAPILifecycleAction(rec, jsonRequest(http.MethodPost, "/api/lifecycle/pause"))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var resp lifecycleResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Outcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", resp.Outcome, tc.wantOutcome)
			}
			if ctrl.callCount() != 1 || ctrl.lastCall() != "pause" {
				t.Errorf("expected exactly one pause call, got calls=%v", ctrl.calls)
			}
		})
	}
}

func TestHandleAPILifecycleActionPersistSentinelDoesNotLeakRawError(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{result: lifecycle.SubmitResult{
		Outcome: lifecycle.OutcomeRejected,
		Err:     errors.Join(ErrLifecyclePersist, errors.New("sqlite: database is locked, path=/secret/db")),
	}}
	s.SetLifecycleController(ctrl)

	rec := httptest.NewRecorder()
	s.handleAPILifecycleAction(rec, jsonRequest(http.MethodPost, "/api/lifecycle/pause"))

	if strings.Contains(rec.Body.String(), "sqlite") || strings.Contains(rec.Body.String(), "/secret/db") {
		t.Errorf("persist error must be sanitized, got body: %s", rec.Body.String())
	}
}

func TestHandleAPILifecycleActionUnknownAction(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{}
	s.SetLifecycleController(ctrl)

	rec := httptest.NewRecorder()
	s.handleAPILifecycleAction(rec, jsonRequest(http.MethodPost, "/api/lifecycle/nonsense"))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if ctrl.callCount() != 0 {
		t.Errorf("controller must not be called for an unknown action, calls=%v", ctrl.calls)
	}
}

// TestHandleAPILifecycleActionUnknownActionGatedByTrustFirst is the A1
// corrective-pass regression: the auth/trust gate runs BEFORE action-name
// validation, so an unrecognized action from a denied trusted-LAN address
// is refused with lan_denied (403), never reaching the unknown_action logic
// at all — and the SAME unrecognized action from an ALLOWED address still
// gets today's unknown_action 400 (the gate lets it through, then the
// action-name check rejects it, exactly as before this pass).
func TestHandleAPILifecycleActionUnknownActionGatedByTrustFirst(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{}
	s.SetLifecycleController(ctrl)
	s.SetDashboardConfig(runtimeconfig.Dashboard{InsecureNoAuth: true, TrustedLANCIDRs: mustLANCIDRs(t, "10.0.0.0/8")})
	handler := s.handler()

	// Denied address: the gate blocks before the unknown action name is
	// ever inspected.
	req := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/zzz")
	req.RemoteAddr = "192.168.1.5:5555"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unknown action from a denied address: status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp lifecycleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Outcome != "lan_denied" {
		t.Errorf("outcome = %q, want lan_denied", resp.Outcome)
	}
	if ctrl.callCount() != 0 {
		t.Errorf("controller must not be called, calls=%v", ctrl.calls)
	}

	// Control: the SAME unknown action from an ALLOWED address still gets
	// today's unknown_action 400 — the gate lets it through, and the
	// existing action-name validation takes over from there.
	req2 := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/zzz")
	req2.RemoteAddr = "10.1.2.3:5555"
	req2.Header.Set("Sec-Fetch-Site", "same-origin")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("unknown action from an allowed address: status = %d, want 400 (body=%s)", rec2.Code, rec2.Body.String())
	}
	var resp2 lifecycleResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp2.Outcome != "unknown_action" {
		t.Errorf("outcome = %q, want unknown_action", resp2.Outcome)
	}
	if ctrl.callCount() != 0 {
		t.Errorf("controller must not be called for an unknown action either way, calls=%v", ctrl.calls)
	}
}

func TestHandleAPILifecycleActionNilControllerUnavailable(t *testing.T) {
	s := newRenderServer(t)
	rec := httptest.NewRecorder()
	s.handleAPILifecycleAction(rec, jsonRequest(http.MethodPost, "/api/lifecycle/pause"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHandleAPILifecycleActionWrongMethod(t *testing.T) {
	s := newRenderServer(t)
	rec := httptest.NewRecorder()
	s.handleAPILifecycleAction(rec, httptest.NewRequest(http.MethodGet, "/api/lifecycle/pause", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// ---- htmx dual response contract ----------------------------------------

func TestHandleAPILifecycleHTMXAlways200(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{
		snap:   lifecycle.Snapshot{Observed: lifecycle.ObservedRunning},
		result: lifecycle.SubmitResult{Outcome: lifecycle.OutcomeRejected, Err: errors.New("process restart required")},
	}
	s.SetLifecycleController(ctrl)

	// GET via htmx.
	rec := httptest.NewRecorder()
	s.handleAPILifecycle(rec, htmxRequest(http.MethodGet, "/api/lifecycle"))
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx GET status = %d, want 200", rec.Code)
	}

	// POST that would be a 409 over JSON must still be htmx 200, with the
	// conflict reason VISIBLE in the rendered partial (design v6 §9 / test
	// 15's httptest twin).
	rec = httptest.NewRecorder()
	s.handleAPILifecycleAction(rec, htmxRequest(http.MethodPost, "/api/lifecycle/restart"))
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx POST (rejected outcome) status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "process restart required") {
		t.Errorf("conflict reason not visible in htmx partial body: %s", rec.Body.String())
	}
}

func TestHandleAPILifecycleHTMXUnknownActionShowsMessage(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{}
	s.SetLifecycleController(ctrl)

	req := htmxRequest(http.MethodPost, "/api/lifecycle/bogus")
	req.AddCookie(&http.Cookie{Name: langCookieName, Value: "en"})
	rec := httptest.NewRecorder()
	s.handleAPILifecycleAction(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx unknown action status = %d, want 200", rec.Code)
	}
	wantMsg := enTR(t)("lc.result.unknown_action")
	if !strings.Contains(rec.Body.String(), wantMsg) {
		t.Errorf("htmx partial must show the unknown-action message %q; body=%s", wantMsg, rec.Body.String())
	}
}

// ---- restart-process ------------------------------------------------------

func TestHandleLifecycleRestartProcessDegradedAccepted(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{snap: lifecycle.Snapshot{Observed: lifecycle.ObservedDegraded}}
	s.SetLifecycleController(ctrl)
	var calls int
	s.SetProcessRestartRequester(func() { calls++ })

	rec := httptest.NewRecorder()
	s.handleAPILifecycleAction(rec, jsonRequest(http.MethodPost, "/api/lifecycle/restart-process"))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("requester called %d times, want 1", calls)
	}

	// Idempotent-ish: a second POST while still degraded calls the
	// requester again from the handler's point of view (the sync.Once
	// idempotency contract lives in the injected closure itself, per D6) —
	// this only asserts the handler invokes it, not the once-semantics.
	rec2 := httptest.NewRecorder()
	s.handleAPILifecycleAction(rec2, jsonRequest(http.MethodPost, "/api/lifecycle/restart-process"))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("second call status = %d, want 202", rec2.Code)
	}
}

func TestHandleLifecycleRestartProcessNonDegradedRejected(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{snap: lifecycle.Snapshot{Observed: lifecycle.ObservedRunning}}
	s.SetLifecycleController(ctrl)
	var calls int
	s.SetProcessRestartRequester(func() { calls++ })

	rec := httptest.NewRecorder()
	s.handleAPILifecycleAction(rec, jsonRequest(http.MethodPost, "/api/lifecycle/restart-process"))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if calls != 0 {
		t.Errorf("requester must not be called when not degraded, calls=%d", calls)
	}
}

func TestHandleLifecycleRestartProcessNilRequesterUnavailable(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{snap: lifecycle.Snapshot{Observed: lifecycle.ObservedDegraded}}
	s.SetLifecycleController(ctrl)
	// No SetProcessRestartRequester call at all.

	rec := httptest.NewRecorder()
	s.handleAPILifecycleAction(rec, jsonRequest(http.MethodPost, "/api/lifecycle/restart-process"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// ---- security matrix (through s.handler(), full middleware chain) --------

func TestLifecycleSecurityAuthConfiguredRequiresCredentials(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{result: lifecycle.SubmitResult{Outcome: lifecycle.OutcomeAccepted}}
	s.SetLifecycleController(ctrl)
	s.SetDashboardConfig(runtimeconfig.Dashboard{Username: "admin", Password: runtimeconfig.NewSecret("hunter2")})
	handler := s.handler()

	req := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/pause")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials: status = %d, want 401", rec.Code)
	}
	if ctrl.callCount() != 0 {
		t.Errorf("controller must not be called without credentials, calls=%v", ctrl.calls)
	}

	req2 := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/pause")
	req2.Header.Set("Sec-Fetch-Site", "same-origin")
	req2.SetBasicAuth("admin", "hunter2")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("with credentials: status = %d, want 202 (body=%s)", rec2.Code, rec2.Body.String())
	}
}

func TestLifecycleSecurityInsecureNoAuthBlocksMutations(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{
		snap:   lifecycle.Snapshot{Observed: lifecycle.ObservedRunning},
		result: lifecycle.SubmitResult{Outcome: lifecycle.OutcomeAccepted},
	}
	s.SetLifecycleController(ctrl)
	s.SetDashboardConfig(runtimeconfig.Dashboard{InsecureNoAuth: true})
	handler := s.handler()

	// GET is never gated (I21).
	getReq := jsonRequest(http.MethodGet, "http://10.0.0.5:5000/api/lifecycle")
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET under InsecureNoAuth: status = %d, want 200", getRec.Code)
	}

	// POST over JSON: 403, controller never called.
	req := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/pause")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST under InsecureNoAuth: status = %d, want 403", rec.Code)
	}
	if ctrl.callCount() != 0 {
		t.Errorf("controller must not be called under InsecureNoAuth, calls=%v", ctrl.calls)
	}

	// POST over htmx: 200 + partial with the disabled explanation,
	// controller still never called.
	hreq := htmxRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/pause")
	hreq.Header.Set("Sec-Fetch-Site", "same-origin")
	hrec := httptest.NewRecorder()
	handler.ServeHTTP(hrec, hreq)
	if hrec.Code != http.StatusOK {
		t.Fatalf("htmx POST under InsecureNoAuth: status = %d, want 200", hrec.Code)
	}
	if ctrl.callCount() != 0 {
		t.Errorf("controller must not be called under InsecureNoAuth (htmx path), calls=%v", ctrl.calls)
	}
	if !strings.Contains(hrec.Body.String(), "DASHBOARD_INSECURE_NO_AUTH") {
		t.Errorf("htmx partial must explain the insecure-disabled state, body: %s", hrec.Body.String())
	}
}

func TestLifecycleSecurityLoopbackNoAuthAllowsMutations(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{result: lifecycle.SubmitResult{Outcome: lifecycle.OutcomeAccepted}}
	s.SetLifecycleController(ctrl)
	// No SetDashboardConfig at all: the zero value is "no auth, no insecure
	// flag" — exactly a default loopback run.
	handler := s.handler()

	req := jsonRequest(http.MethodPost, "http://127.0.0.1:5000/api/lifecycle/pause")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("loopback no-auth POST: status = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}
	if ctrl.callCount() != 1 {
		t.Errorf("controller should have been called once, calls=%v", ctrl.calls)
	}
}

// ---- trusted-LAN allowlist gate (Ф4d) -------------------------------------

// TestLifecycleSecurityTrustedLANAllowsIPv4 covers row 4 of the Ф4d behavior
// matrix: InsecureNoAuth + a CIDR containing RemoteAddr allows the mutation
// over both the JSON and htmx contracts.
func TestLifecycleSecurityTrustedLANAllowsIPv4(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{result: lifecycle.SubmitResult{Outcome: lifecycle.OutcomeAccepted}}
	s.SetLifecycleController(ctrl)
	s.SetDashboardConfig(runtimeconfig.Dashboard{InsecureNoAuth: true, TrustedLANCIDRs: mustLANCIDRs(t, "10.0.0.0/8")})
	handler := s.handler()

	req := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/pause")
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("JSON POST from trusted LAN: status = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}
	if ctrl.callCount() != 1 {
		t.Errorf("controller should have been called once, calls=%v", ctrl.calls)
	}

	hreq := htmxRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/pause")
	hreq.RemoteAddr = "10.1.2.3:5555"
	hreq.Header.Set("Sec-Fetch-Site", "same-origin")
	hreq.AddCookie(&http.Cookie{Name: langCookieName, Value: "en"})
	hrec := httptest.NewRecorder()
	handler.ServeHTTP(hrec, hreq)
	if hrec.Code != http.StatusOK {
		t.Fatalf("htmx POST from trusted LAN: status = %d, want 200 (body=%s)", hrec.Code, hrec.Body.String())
	}
	if !strings.Contains(hrec.Body.String(), enTR(t)("lc.result.accepted")) {
		t.Errorf("htmx partial should show the accepted message, body=%s", hrec.Body.String())
	}
	if ctrl.callCount() != 2 {
		t.Errorf("controller should have been called twice total, calls=%v", ctrl.calls)
	}
}

// TestLifecycleSecurityTrustedLANDeniesIPv4Outside covers row 5: RemoteAddr
// outside the configured CIDR is refused with the new lan_denied outcome,
// the controller is never called, and the htmx body shows the denial text.
func TestLifecycleSecurityTrustedLANDeniesIPv4Outside(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{result: lifecycle.SubmitResult{Outcome: lifecycle.OutcomeAccepted}}
	s.SetLifecycleController(ctrl)
	s.SetDashboardConfig(runtimeconfig.Dashboard{InsecureNoAuth: true, TrustedLANCIDRs: mustLANCIDRs(t, "10.0.0.0/8")})
	handler := s.handler()

	req := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/pause")
	req.RemoteAddr = "192.168.1.5:5555"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("JSON POST outside trusted LAN: status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp lifecycleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Outcome != "lan_denied" {
		t.Errorf("outcome = %q, want lan_denied", resp.Outcome)
	}
	if ctrl.callCount() != 0 {
		t.Errorf("controller must not be called, calls=%v", ctrl.calls)
	}

	hreq := htmxRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/pause")
	hreq.RemoteAddr = "192.168.1.5:5555"
	hreq.Header.Set("Sec-Fetch-Site", "same-origin")
	hreq.AddCookie(&http.Cookie{Name: langCookieName, Value: "en"})
	hrec := httptest.NewRecorder()
	handler.ServeHTTP(hrec, hreq)
	if hrec.Code != http.StatusOK {
		t.Fatalf("htmx POST outside trusted LAN: status = %d, want 200 (body=%s)", hrec.Code, hrec.Body.String())
	}
	if !strings.Contains(hrec.Body.String(), enTR(t)("lc.result.lan_denied")) {
		t.Errorf("htmx partial should show the lan_denied message, body=%s", hrec.Body.String())
	}
	if ctrl.callCount() != 0 {
		t.Errorf("controller must not be called (htmx path), calls=%v", ctrl.calls)
	}
}

// TestLifecycleSecurityTrustedLANIPv6 covers IPv6 allow/deny.
func TestLifecycleSecurityTrustedLANIPv6(t *testing.T) {
	cases := []struct {
		name       string
		cidrs      string
		remoteAddr string
		wantStatus int
		wantCalls  int
	}{
		{"allowed inside fd00::/8", "fd00::/8", "[fd12::1]:4242", http.StatusAccepted, 1},
		{"denied outside fd00::/8", "fd00::/8", "[2001:db8::1]:4242", http.StatusForbidden, 0},
		// A4: an IPv4-mapped-IPv6 RemoteAddr (as net/http reports for a
		// dual-stack listener accepting an IPv4 peer) must still match a
		// plain IPv4 CIDR end to end - proves the lifecycleLANTrust
		// Unmap() step, not just the classifier's own unit test.
		{"IPv4-mapped-IPv6 RemoteAddr unmapped against an IPv4 CIDR", "10.0.0.0/8", "[::ffff:10.1.2.3]:5555", http.StatusAccepted, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRenderServer(t)
			ctrl := &fakeLifecycleController{result: lifecycle.SubmitResult{Outcome: lifecycle.OutcomeAccepted}}
			s.SetLifecycleController(ctrl)
			s.SetDashboardConfig(runtimeconfig.Dashboard{InsecureNoAuth: true, TrustedLANCIDRs: mustLANCIDRs(t, tc.cidrs)})
			handler := s.handler()

			req := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/pause")
			req.RemoteAddr = tc.remoteAddr
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if ctrl.callCount() != tc.wantCalls {
				t.Errorf("controller called %d times, want %d, calls=%v", ctrl.callCount(), tc.wantCalls, ctrl.calls)
			}
		})
	}
}

// TestLifecycleSecurityTrustedLANMultipleCIDRsMatchSecond covers matching
// against the SECOND of several configured CIDRs.
func TestLifecycleSecurityTrustedLANMultipleCIDRsMatchSecond(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{result: lifecycle.SubmitResult{Outcome: lifecycle.OutcomeAccepted}}
	s.SetLifecycleController(ctrl)
	s.SetDashboardConfig(runtimeconfig.Dashboard{InsecureNoAuth: true, TrustedLANCIDRs: mustLANCIDRs(t, "10.0.0.0/8,192.168.0.0/16")})
	handler := s.handler()

	req := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/pause")
	req.RemoteAddr = "192.168.5.5:1111"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}
	if ctrl.callCount() != 1 {
		t.Errorf("controller should have been called once, calls=%v", ctrl.calls)
	}
}

// TestLifecycleSecurityTrustedLANIgnoresSpoofedHeaders proves the allowlist
// is checked against r.RemoteAddr ONLY: a denied RemoteAddr stays denied
// even when Forwarded/X-Forwarded-For/X-Real-IP all claim an allowed IP.
func TestLifecycleSecurityTrustedLANIgnoresSpoofedHeaders(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{result: lifecycle.SubmitResult{Outcome: lifecycle.OutcomeAccepted}}
	s.SetLifecycleController(ctrl)
	s.SetDashboardConfig(runtimeconfig.Dashboard{InsecureNoAuth: true, TrustedLANCIDRs: mustLANCIDRs(t, "10.0.0.0/8")})
	handler := s.handler()

	req := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/pause")
	req.RemoteAddr = "203.0.113.5:5555" // outside 10.0.0.0/8
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("X-Forwarded-For", "10.1.2.3")
	req.Header.Set("X-Real-IP", "10.1.2.3")
	req.Header.Set("Forwarded", `for="10.1.2.3"`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("spoofed proxy headers must not bypass the gate: status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if ctrl.callCount() != 0 {
		t.Errorf("controller must not be called, calls=%v", ctrl.calls)
	}
}

// TestLifecycleSecurityBasicAuthNeverBypassedByTrustedLAN covers row 1 of
// the behavior matrix: even with a trusted-LAN allowlist configured that
// matches the RemoteAddr, Basic Auth (when credentials are configured) is
// still enforced by the outer middleware — the allowlist never substitutes
// for it.
func TestLifecycleSecurityBasicAuthNeverBypassedByTrustedLAN(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{result: lifecycle.SubmitResult{Outcome: lifecycle.OutcomeAccepted}}
	s.SetLifecycleController(ctrl)
	s.SetDashboardConfig(runtimeconfig.Dashboard{
		Username:        "admin",
		Password:        runtimeconfig.NewSecret("hunter2"),
		InsecureNoAuth:  true, // an operator could set both; auth must still win
		TrustedLANCIDRs: mustLANCIDRs(t, "10.0.0.0/8"),
	})
	handler := s.handler()

	req := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/pause")
	req.RemoteAddr = "10.1.2.3:5555" // inside the allowlist
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials, even from a trusted LAN address: status = %d, want 401", rec.Code)
	}
	if ctrl.callCount() != 0 {
		t.Errorf("controller must not be called without credentials, calls=%v", ctrl.calls)
	}

	req2 := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/pause")
	req2.RemoteAddr = "10.1.2.3:5555"
	req2.Header.Set("Sec-Fetch-Site", "same-origin")
	req2.SetBasicAuth("admin", "hunter2")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("with credentials from a trusted LAN address: status = %d, want 202 (body=%s)", rec2.Code, rec2.Body.String())
	}
	if ctrl.callCount() != 1 {
		t.Errorf("controller should have been called once, calls=%v", ctrl.calls)
	}
}

// TestLifecycleSecurityTrustedLANCSRFStillApplies proves the trust gate does
// not weaken CSRF protection: a cross-origin POST from an ALLOWED RemoteAddr
// is still blocked by the CSRF layer, before the trust gate is ever reached.
func TestLifecycleSecurityTrustedLANCSRFStillApplies(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{result: lifecycle.SubmitResult{Outcome: lifecycle.OutcomeAccepted}}
	s.SetLifecycleController(ctrl)
	s.SetDashboardConfig(runtimeconfig.Dashboard{InsecureNoAuth: true, TrustedLANCIDRs: mustLANCIDRs(t, "10.0.0.0/8")})
	handler := s.handler()

	req := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/pause")
	req.RemoteAddr = "10.1.2.3:5555" // inside the allowlist
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST (even from a trusted address): status = %d, want 403", rec.Code)
	}
	if ctrl.callCount() != 0 {
		t.Errorf("controller must not be called for a CSRF-blocked request, calls=%v", ctrl.calls)
	}
	if strings.Contains(rec.Body.String(), "lan_denied") {
		t.Error("this 403 must come from the CSRF layer, not the lifecycle trust gate")
	}
}

// TestLifecycleGETNeverGatedAcrossTrustStates: GET /api/lifecycle stays
// read-only and ungated in all three trust states (I21).
func TestLifecycleGETNeverGatedAcrossTrustStates(t *testing.T) {
	cases := []struct {
		name string
		dash runtimeconfig.Dashboard
	}{
		{"not configured", runtimeconfig.Dashboard{InsecureNoAuth: true}},
		{"allowed", runtimeconfig.Dashboard{InsecureNoAuth: true, TrustedLANCIDRs: mustLANCIDRs(t, "10.0.0.0/8")}},
		{"denied", runtimeconfig.Dashboard{InsecureNoAuth: true, TrustedLANCIDRs: mustLANCIDRs(t, "192.168.0.0/16")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRenderServer(t)
			s.SetLifecycleController(&fakeLifecycleController{snap: lifecycle.Snapshot{Observed: lifecycle.ObservedRunning}})
			s.SetDashboardConfig(tc.dash)
			handler := s.handler()

			req := jsonRequest(http.MethodGet, "http://10.0.0.5:5000/api/lifecycle")
			req.RemoteAddr = "10.1.2.3:5555"
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestHandleLifecycleRestartProcessTrustedLANGate proves restart-process
// honors the same gate as the other mutations: denied outside the CIDR
// (403, requester not called), accepted inside it while degraded.
func TestHandleLifecycleRestartProcessTrustedLANGate(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{snap: lifecycle.Snapshot{Observed: lifecycle.ObservedDegraded}}
	s.SetLifecycleController(ctrl)
	var calls int
	s.SetProcessRestartRequester(func() { calls++ })
	s.SetDashboardConfig(runtimeconfig.Dashboard{InsecureNoAuth: true, TrustedLANCIDRs: mustLANCIDRs(t, "10.0.0.0/8")})
	handler := s.handler()

	// Outside the CIDR: denied, requester not called.
	req := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/restart-process")
	req.RemoteAddr = "192.168.1.5:5555"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("outside CIDR: status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if calls != 0 {
		t.Errorf("requester must not be called when denied, calls=%d", calls)
	}

	// Inside the CIDR, still degraded: accepted.
	req2 := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/restart-process")
	req2.RemoteAddr = "10.1.2.3:5555"
	req2.Header.Set("Sec-Fetch-Site", "same-origin")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("inside CIDR: status = %d, want 202 (body=%s)", rec2.Code, rec2.Body.String())
	}
	if calls != 1 {
		t.Errorf("requester should have been called once, calls=%d", calls)
	}
}

func TestLifecycleSecurityCSRF(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{result: lifecycle.SubmitResult{Outcome: lifecycle.OutcomeAccepted}}
	s.SetLifecycleController(ctrl)
	handler := s.handler()

	// Cross-origin POST: blocked by the CSRF layer before the handler runs.
	req := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/pause")
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST: status = %d, want 403", rec.Code)
	}
	if ctrl.callCount() != 0 {
		t.Errorf("controller must not be called for a blocked cross-origin request, calls=%v", ctrl.calls)
	}

	// No provenance headers at all (non-browser client): passes CSRF.
	req2 := jsonRequest(http.MethodPost, "http://10.0.0.5:5000/api/lifecycle/pause")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusForbidden {
		t.Fatal("a request with no provenance headers must not be blocked by CSRF")
	}
}
