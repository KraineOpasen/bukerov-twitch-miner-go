package web

// S5-3 lifecycle honesty tests (task Phase 2/10 items 1-7). These are written
// BEFORE the implementation and must fail until handlers_lifecycle.go,
// lifecycle_panel.html and overview.html's client-side stale gating are
// updated. No new ObservedState is introduced anywhere here — both cases
// below are real, existing UI-uncertainty signals:
//
//   A. Capabilities unavailable / transition pending: Snapshot.Transition !=
//      TransitionNone, which the existing Capabilities doc comment
//      (internal/lifecycle/lifecycle.go) says drives every capability false
//      for as long as the pending-command slot is held.
//   B. The lifecycle HTMX poll (every 2s) goes stale/lost — an existing
//      client-side clock (#lc-conn-lost, STALE_AFTER_MS) already detects
//      this; S5-3 extends it to also gate the buttons, never a state-machine
//      change.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
)

// s53T resolves a single locale key for lang, for tests in this file that
// need RU text (enTR in render_helpers_test.go only ever returns EN).
func s53T(t *testing.T, lang, key string) string {
	t.Helper()
	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	return loc.T(lang, key)
}

// ---- Case A: transition-pending / capabilities-unavailable -----------------

// lcPauseButtonRe/lcStopButtonRe match the actual rendered opening tag of the
// Pause and Stop buttons by their hx-post route, so assertions can target
// that specific element instead of a page-wide "disabled" substring any
// other button (e.g. the always-collapsed Stop control) could equally
// satisfy (CodeRabbit PR152 finding: the page-wide check at the old line 60
// passed only because Stop happened to be disabled, never proving Pause
// was).
var (
	lcPauseButtonRe = regexp.MustCompile(`<button[^>]*hx-post="/api/lifecycle/pause"[^>]*>`)
	lcStopButtonRe  = regexp.MustCompile(`<button[^>]*hx-post="/api/lifecycle/stop"[^>]*>`)
)

// TestS5_3LifecycleTransitionReasonVisibleNearButtons proves that whenever the
// snapshot reports a transition in progress (capabilities unavailable for as
// long as the pending-command slot is held), Pause and Stop are EACH
// independently disabled on their own rendered element, and the reason text
// is visible within their own associated action group - the primary button
// row for Pause, the collapsed Advanced/Stop block for Stop - never merely
// "somewhere in the complete page body", in both languages.
func TestS5_3LifecycleTransitionReasonVisibleNearButtons(t *testing.T) {
	snap := lifecycle.Snapshot{
		Desired:      lifecycle.DesiredRunning,
		Observed:     lifecycle.ObservedPausing,
		Transition:   lifecycle.TransitionPause,
		Capabilities: lifecycle.Capabilities{}, // all false: slot occupied by the pending command
	}
	for _, lang := range []string{"en", "ru"} {
		body := lifecyclePanelHTMX(t, snap, lang)
		want := s53T(t, lang, "lc.reason.transitioning")

		groupStart := strings.Index(body, `role="group" aria-label="`)
		reasonStart := strings.Index(body, `id="lc-transition-reason"`)
		advancedStart := strings.Index(body, `id="lc-advanced"`)
		if groupStart < 0 || reasonStart < 0 || advancedStart < 0 {
			t.Fatalf("[%s] lifecycle_panel.html missing the expected primary group / transition-reason / advanced structure; body=%s", lang, body)
		}

		// ---- Pause: its own <button> element must carry disabled, and the
		// reason must be visible within the primary action group's own
		// region (the group through the reason element immediately
		// following it).
		primaryGroup := body[groupStart:advancedStart]
		pauseTag := lcPauseButtonRe.FindString(primaryGroup)
		if pauseTag == "" {
			t.Fatalf("[%s] expected a Pause <button hx-post=\"/api/lifecycle/pause\"> in the primary action group; body=%s", lang, body)
		}
		if !strings.Contains(pauseTag, "disabled") {
			t.Errorf("[%s] Pause button element itself must carry disabled while transitioning; tag=%s", lang, pauseTag)
		}
		if !strings.Contains(primaryGroup, want) {
			t.Errorf("[%s] transition reason %q must be visible within the Pause action group, not merely somewhere in the page; region=%s", lang, want, primaryGroup)
		}

		// ---- Stop: its own <button> element must carry disabled, scoped to
		// the Advanced action group that contains it; the shared reason
		// explanation renders immediately before that group.
		stopTag := lcStopButtonRe.FindString(body[advancedStart:])
		if stopTag == "" {
			t.Fatalf("[%s] expected a Stop <button hx-post=\"/api/lifecycle/stop\"> in the Advanced action group; body=%s", lang, body)
		}
		if !strings.Contains(stopTag, "disabled") {
			t.Errorf("[%s] Stop button element itself must carry disabled while transitioning; tag=%s", lang, stopTag)
		}
		reasonBeforeStop := body[reasonStart:advancedStart]
		if !strings.Contains(reasonBeforeStop, want) {
			t.Errorf("[%s] transition reason %q must be visible in the region associated with the Stop action group; region=%s", lang, want, reasonBeforeStop)
		}
	}
}

// TestS5_3LifecyclePendingTransitionNeverBareDisabled sweeps every
// Transition != none value and proves none of them render a disabled button
// with zero adjacent reason text.
func TestS5_3LifecyclePendingTransitionNeverBareDisabled(t *testing.T) {
	transitions := []lifecycle.Transition{
		lifecycle.TransitionPending, lifecycle.TransitionStart, lifecycle.TransitionPause,
		lifecycle.TransitionStop, lifecycle.TransitionRestart,
	}
	reason := enTR(t)("lc.reason.transitioning")
	if reason == "" || reason == "lc.reason.transitioning" {
		t.Fatalf("lc.reason.transitioning must resolve to real text, got %q", reason)
	}
	for _, tr := range transitions {
		snap := lifecycle.Snapshot{
			Desired: lifecycle.DesiredRunning, Observed: lifecycle.ObservedRunning,
			Transition: tr, Capabilities: lifecycle.Capabilities{},
		}
		body := lifecyclePanelHTMX(t, snap, "en")
		if strings.Contains(body, "disabled") && !strings.Contains(body, reason) {
			t.Errorf("transition=%s: disabled control(s) rendered with no visible reason text; body=%s", tr, body)
		}
	}
}

// TestS5_3LifecycleSteadyStateNoTransitionReason proves the new reason text
// is exclusive to an actual transition — a plain running/paused/stopped
// steady state must not show it (it would be a fabricated, un-caused warning).
func TestS5_3LifecycleSteadyStateNoTransitionReason(t *testing.T) {
	reason := enTR(t)("lc.reason.transitioning")
	steady := []lifecycle.ObservedState{lifecycle.ObservedRunning, lifecycle.ObservedPaused, lifecycle.ObservedStopped}
	for _, observed := range steady {
		snap := lifecycle.Snapshot{
			Desired: lifecycle.DesiredRunning, Observed: observed, Transition: lifecycle.TransitionNone,
			Capabilities: lifecycle.Capabilities{CanPause: true, CanResume: true, CanRestart: true, CanStop: true},
		}
		body := lifecyclePanelHTMX(t, snap, "en")
		if strings.Contains(body, reason) {
			t.Errorf("observed=%s (no transition): must not show the transitioning reason text; body=%s", observed, body)
		}
	}
}

// ---- Case B: stale lifecycle polling (client-side, source-literal pinned) --

// lcPanelScriptBlock returns the source of the __lcPanel IIFE (the last
// <script> block in overview.html), so assertions about it can't accidentally
// match an unrelated script elsewhere on the page.
func lcPanelScriptBlock(t *testing.T) string {
	t.Helper()
	src := readEmbeddedTemplate(t, "templates/overview.html")
	start := strings.Index(src, "if (window.__lcPanel) return;")
	if start < 0 {
		t.Fatal("overview.html missing the __lcPanel guard - lifecycle stale-gating script not found")
	}
	return src[start:]
}

// TestS5_3LifecycleStaleGatingDisablesButtons proves the client-side stale
// clock disables every lifecycle action button once the poll is judged lost,
// via the shared helper (not scattered per-button code), scoped to
// #lifecycle-panel only.
//
// No DOM/browser test harness exists in this repository (no goja/jsdom/
// Node/Playwright dependency, and adding one is out of scope for this
// corrective pass - task Phase 3 item C), so this remains a source-contract
// test. It is strengthened past mere syntax presence (function declared,
// querySelectorAll called) to require the ACTUAL disabling assignment
// itself: "btn.disabled = disabled" (CodeRabbit's Deterministic Test
// Contract warning on PR152 - a mutant that emptied the forEach callback
// body would previously still pass, since the old assertions only checked
// the function/selector literals, never the assignment that does the work).
func TestS5_3LifecycleStaleGatingDisablesButtons(t *testing.T) {
	block := lcPanelScriptBlock(t)
	for _, want := range []string{
		"function setLifecycleButtonsDisabled(",
		"panel.querySelectorAll('button')",
		"btn.disabled = disabled;",
		"setLifecycleButtonsDisabled(true)",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("__lcPanel script missing stale-gating literal %q", want)
		}
	}
}

// TestS5_3LifecycleStaleGatingStatesUnconfirmedNeverToast proves the stale
// path writes a "state cannot be confirmed" message (never a bare "connection
// lost" retry line masquerading as confirmed unavailability) and never calls
// the global success/neutral toast.
func TestS5_3LifecycleStaleGatingStatesUnconfirmedNeverToast(t *testing.T) {
	block := lcPanelScriptBlock(t)
	if !strings.Contains(block, "js.lc.state_unconfirmed") {
		t.Error("__lcPanel script must render the js.lc.state_unconfirmed message when stale")
	}
	if strings.Contains(block, "showToast") {
		t.Error("__lcPanel script must never call showToast - lifecycle staleness is inline, not a toast")
	}

	for _, lang := range []string{"en", "ru"} {
		got := s53T(t, lang, "js.lc.state_unconfirmed")
		if got == "" || got == "js.lc.state_unconfirmed" {
			t.Errorf("[%s] js.lc.state_unconfirmed must resolve to real text, got %q", lang, got)
		}
	}
}

// TestS5_3LifecycleStaleGatingDiagnosticsLinkUsesExistingRoute proves the
// stale-state diagnostics link targets an already-registered, non-deferred
// route (never a fabricated one, never the deferred /help/troubleshooting).
func TestS5_3LifecycleStaleGatingDiagnosticsLinkUsesExistingRoute(t *testing.T) {
	body := readEmbeddedTemplate(t, "templates/overview.html")
	if !strings.Contains(body, `id="lc-conn-lost-diag"`) {
		t.Fatal("overview.html missing the #lc-conn-lost-diag diagnostics link")
	}
	if !strings.Contains(body, `href="/health"`) {
		t.Error("#lc-conn-lost-diag must link to the existing /health route")
	}
	if strings.Contains(body, `href="/help/troubleshooting"`) {
		t.Error("must not link to the deferred /help/troubleshooting route")
	}

	// The target route must actually be live (200), not a fabricated path.
	srv := newRenderServer(t)
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /health = %d, want 200 (the diagnostics link must point at a live route)", rec.Code)
	}
}

// TestS5_3LifecycleSuccessfulSwapStillClearsConnLost pins the EXISTING
// restore mechanism this task relies on and must not remove: a real
// #lifecycle-panel swap unconditionally re-hides #lc-conn-lost, so the next
// authoritative server render (with its real Can*/Show* flags) is what
// actually un-disables the buttons - never bespoke client-side "re-enable"
// logic that could drift from the server's truth.
func TestS5_3LifecycleSuccessfulSwapStillClearsConnLost(t *testing.T) {
	block := lcPanelScriptBlock(t)
	if !strings.Contains(block, "if (connLost) connLost.hidden = true;") {
		t.Error("a successful #lifecycle-panel swap must still unconditionally re-hide #lc-conn-lost")
	}
}

// ---- server-side safety is unaffected by client staleness ------------------

// TestS5_3ForgedStalePOSTStillRejectedServerSide proves a POST that a stale
// client should never have been able to send (client-side gating bypassed —
// e.g. curl, or a race) is still validated and rejected by the existing
// Controller.Submit path exactly as before S5-3: no new bypass, no new
// leniency.
func TestS5_3ForgedStalePOSTStillRejectedServerSide(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{result: lifecycle.SubmitResult{
		Outcome: lifecycle.OutcomeRejected,
		Err:     errors.New("process restart required"),
	}}
	s.SetLifecycleController(ctrl)

	rec := httptest.NewRecorder()
	s.handleAPILifecycleAction(rec, jsonRequest(http.MethodPost, "/api/lifecycle/restart"))

	if rec.Code != http.StatusConflict {
		t.Fatalf("forged/stale POST: status = %d, want 409 (rejection must still happen server-side)", rec.Code)
	}
	if ctrl.callCount() != 1 {
		t.Errorf("Controller must still have been asked to validate the command, calls=%v", ctrl.calls)
	}
}

// ---- confirmations / non-confirmations, re-pinned under S5-3 ---------------

// TestS5_3RestartStopConfirmPauseResumeDoNot re-confirms (task Phase 10 item
// 6) that S5-3's lifecycle changes did not touch the existing confirmation
// policy: Restart/Stop keep hx-confirm, Pause/Resume never gain one.
func TestS5_3RestartStopConfirmPauseResumeDoNot(t *testing.T) {
	snap := lifecycle.Snapshot{
		Desired: lifecycle.DesiredRunning, Observed: lifecycle.ObservedRunning,
		Capabilities: lifecycle.Capabilities{CanPause: true, CanResume: true, CanRestart: true, CanStop: true},
	}
	body := lifecyclePanelHTMX(t, snap, "en")

	pauseIdx := strings.Index(body, `hx-post="/api/lifecycle/pause"`)
	restartIdx := strings.Index(body, `hx-post="/api/lifecycle/restart"`)
	stopIdx := strings.Index(body, `hx-post="/api/lifecycle/stop"`)
	if pauseIdx < 0 || restartIdx < 0 || stopIdx < 0 {
		t.Fatalf("expected pause/restart/stop buttons all present; body=%s", body)
	}
	pauseTag := body[pauseIdx : pauseIdx+strings.Index(body[pauseIdx:], ">")]
	restartTag := body[restartIdx : restartIdx+strings.Index(body[restartIdx:], ">")]
	stopTag := body[stopIdx : stopIdx+strings.Index(body[stopIdx:], ">")]

	if strings.Contains(pauseTag, "hx-confirm") {
		t.Error("Pause must remain without confirmation")
	}
	if !strings.Contains(restartTag, "hx-confirm") {
		t.Error("Restart must keep its confirmation")
	}
	if !strings.Contains(stopTag, "hx-confirm") {
		t.Error("Stop must keep its confirmation")
	}
}

// TestS5_3LifecycleFailureRemainsInlineNotToast re-confirms (task Phase 10
// item 7) that a rejected htmx command still renders its reason INSIDE the
// lifecycle partial (role=alert content, not a global toast).
func TestS5_3LifecycleFailureRemainsInlineNotToast(t *testing.T) {
	s := newRenderServer(t)
	ctrl := &fakeLifecycleController{result: lifecycle.SubmitResult{
		Outcome: lifecycle.OutcomeRejected, Err: errors.New("process restart required"),
	}}
	s.SetLifecycleController(ctrl)

	rec := httptest.NewRecorder()
	s.handleAPILifecycleAction(rec, htmxRequest(http.MethodPost, "/api/lifecycle/restart"))
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx POST status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "process restart required") {
		t.Errorf("rejection reason must render inline in the partial; body=%s", body)
	}
	if !strings.Contains(body, `role="alert"`) {
		t.Errorf("rejection must be marked role=alert inline, not a toast; body=%s", body)
	}
}

// ---- existing IDs untouched -------------------------------------------------

// TestS5_3LifecycleExistingIDsPreserved is a cheap regression guard for task
// requirement §9: the four load-bearing IDs must survive S5-3's changes.
func TestS5_3LifecycleExistingIDsPreserved(t *testing.T) {
	body := readEmbeddedTemplate(t, "templates/overview.html")
	for _, id := range []string{`id="lc-live"`, `id="lifecycle-panel"`, `id="lc-conn-lost"`} {
		if !strings.Contains(body, id) {
			t.Errorf("overview.html missing existing id %q", id)
		}
	}
	panelPartial := readEmbeddedTemplate(t, "templates/partials/lifecycle_panel.html")
	if !strings.Contains(panelPartial, `id="lc-advanced"`) {
		t.Error("lifecycle_panel.html missing existing id \"lc-advanced\"")
	}
}
