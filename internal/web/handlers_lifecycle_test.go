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
