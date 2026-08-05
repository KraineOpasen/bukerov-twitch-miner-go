package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

// ErrLifecyclePersist is the sentinel internal/app's Persistence decorator
// wraps a Save error with (errors.Join, preserving both chains) so this
// package can tell a 500-class persistence failure apart from a 409-class
// table/slot rejection without string-matching lifecycle's rejection prose.
// lifecycle.Controller.Submit wraps store errors with %w, so
// errors.Is(res.Err, ErrLifecyclePersist) survives that wrap unchanged.
var ErrLifecyclePersist = errors.New("lifecycle: persist failed")

// LifecycleController is the narrow seam this package needs into the
// lifecycle controller — internal/lifecycle's *Controller satisfies it
// directly. Kept narrow (rather than depending on the concrete type) so
// tests can inject a fake with canned Snapshot/SubmitResult values.
type LifecycleController interface {
	Snapshot() lifecycle.Snapshot
	Pause(ctx context.Context) lifecycle.SubmitResult
	Resume(ctx context.Context) lifecycle.SubmitResult
	Restart(ctx context.Context) lifecycle.SubmitResult
	Stop(ctx context.Context) lifecycle.SubmitResult
}

// LifecycleUpdateState is the best-effort auto-updater state surfaced
// alongside the lifecycle snapshot: State is one of "" (nothing observed
// yet), "available", "failed", or "applied" — Version is the version string
// associated with that state (meaningless, and left empty, when State is
// ""). Assembled by internal/app from the updater's notify callbacks; this
// package only ever reads it through the injected closure below.
type LifecycleUpdateState struct {
	State   string
	Version string
}

// SetLifecycleController wires the lifecycle command/snapshot seam. Guarded
// by s.mu like every other Set* provider in this file; nil (the default —
// every pre-Ф4c caller, and any test that doesn't opt in) is a supported,
// honest state: GET/POST /api/lifecycle* answer 503 for the JSON contract
// and the dashboard panel renders nothing.
func (s *Server) SetLifecycleController(c LifecycleController) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifecycleController = c
}

// SetLifecycleUpdateState wires the best-effort updater-state reader. nil
// (the default) means "no updater information available" — the panel's
// update line and the GET JSON's updateState field are simply omitted.
func (s *Server) SetLifecycleUpdateState(fn func() LifecycleUpdateState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifecycleUpdateState = fn
}

// SetProcessRestartRequester wires the degraded "Restart process" action.
// Calling the injected func is the one-shot, documented process-exit path
// (cancelling the app's own run scope) — this package never implements that
// itself. nil (the default) means the action is unavailable: 503 for the
// JSON contract, hidden/disabled in the panel.
func (s *Server) SetProcessRestartRequester(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processRestartRequester = fn
}

func (s *Server) lifecycleProviders() (ctrl LifecycleController, updFn func() LifecycleUpdateState, restartRequester func()) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lifecycleController, s.lifecycleUpdateState, s.processRestartRequester
}

func lifecycleUpdateStateOf(fn func() LifecycleUpdateState) LifecycleUpdateState {
	if fn == nil {
		return LifecycleUpdateState{}
	}
	return fn()
}

// lifecycleResponse is the JSON shape for GET/POST /api/lifecycle* under the
// Accept: application/json contract. Outcome/Error are populated only for
// POST responses — a GET always leaves them empty/omitted.
type lifecycleResponse struct {
	DesiredState  string `json:"desiredState"`
	ObservedState string `json:"observedState"`
	Transition    string `json:"transition"`
	Generation    uint64 `json:"generation"`
	CommandID     string `json:"commandId,omitempty"`
	Reason        string `json:"reason,omitempty"`

	// StartedAt/TransitionStartedAt/NextRetryAt are RFC3339 strings or JSON
	// null (never omitted) when the underlying time.Time is zero.
	StartedAt           *string `json:"startedAt"`
	TransitionStartedAt *string `json:"transitionStartedAt"`
	NextRetryAt         *string `json:"nextRetryAt"`

	LastError string `json:"lastError,omitempty"`

	CanPause   bool `json:"canPause"`
	CanResume  bool `json:"canResume"`
	CanRestart bool `json:"canRestart"`
	CanStop    bool `json:"canStop"`

	Override bool `json:"override"`

	// UpdateState is omitted entirely when "" (no updater information
	// wired/observed yet) rather than serialized as an explicit "none".
	UpdateState   string `json:"updateState,omitempty"`
	UpdateVersion string `json:"updateVersion,omitempty"`

	Version string `json:"version"`

	// Outcome/Error are set only on POST responses.
	Outcome string `json:"outcome,omitempty"`
	Error   string `json:"error,omitempty"`
}

func rfc3339OrNull(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func buildLifecycleResponse(snap lifecycle.Snapshot, upd LifecycleUpdateState) lifecycleResponse {
	return lifecycleResponse{
		DesiredState:        string(snap.Desired),
		ObservedState:       string(snap.Observed),
		Transition:          string(snap.Transition),
		Generation:          snap.Generation,
		CommandID:           snap.CommandID,
		Reason:              string(snap.Reason),
		StartedAt:           rfc3339OrNull(snap.StartedAt),
		TransitionStartedAt: rfc3339OrNull(snap.TransitionStartedAt),
		NextRetryAt:         rfc3339OrNull(snap.NextRetryAt),
		LastError:           snap.LastError,
		CanPause:            snap.Capabilities.CanPause,
		CanResume:           snap.Capabilities.CanResume,
		CanRestart:          snap.Capabilities.CanRestart,
		CanStop:             snap.Capabilities.CanStop,
		Override:            snap.Override,
		UpdateState:         upd.State,
		UpdateVersion:       upd.Version,
		Version:             version.Version,
	}
}

// lifecycleActionOutcome carries what a POST just did (or why a request was
// refused before even reaching the controller), so both response shapes can
// render it: "" (the zero value, used for plain GETs) means no result to
// show at all.
type lifecycleActionOutcome struct {
	kind   string // "accepted" | "idempotent" | "rejected" | "insecure" | "lan_denied" | "unknown_action" | "unavailable"
	detail string // sanitized human-readable detail; "" when kind carries its own fixed text
}

// handleAPILifecycle serves GET /api/lifecycle: the current snapshot, HTML
// partial for htmx or JSON for Accept: application/json.
func (s *Server) handleAPILifecycle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeNotAllowed(w)
		return
	}
	ctrl, _, _ := s.lifecycleProviders()
	status := http.StatusOK
	if ctrl == nil {
		status = http.StatusServiceUnavailable
	}
	s.respondLifecycle(w, r, status, lifecycleActionOutcome{})
}

// handleAPILifecycleAction serves POST /api/lifecycle/{action}: pause,
// resume, restart, stop, or restart-process. GET is never accepted here
// (I21: GET never mutates).
func (s *Server) handleAPILifecycleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeNotAllowed(w)
		return
	}

	// Security gate (design v6 §9(а); Ф4d, hoisted in the corrective pass):
	// the trigger is EXACTLY InsecureNoAuth, never !AuthEnabled() — a
	// loopback default run with no credentials configured at all must still
	// be able to mutate (§9(в)). This check is per-handler because
	// basicAuthMiddleware is skipped entirely whenever AuthEnabled() is
	// false, so nothing upstream in the middleware chain would otherwise
	// stop an unauthenticated remote command under
	// DASHBOARD_INSECURE_NO_AUTH=true. lifecycleAuthGateBlocked additionally
	// consults the DASHBOARD_TRUSTED_LAN_CIDRS allowlist. It runs FIRST —
	// above action-name validation and the restart-process dispatch below —
	// so EVERY POST under /api/lifecycle/, including an unrecognized action
	// name, passes it before anything else: the unknown_action 400 (which
	// echoes a snapshot body) is reachable only by a caller this gate has
	// already allowed.
	if s.lifecycleAuthGateBlocked(w, r) {
		return
	}

	action := strings.TrimPrefix(r.URL.Path, "/api/lifecycle/")

	if action == "restart-process" {
		s.handleLifecycleRestartProcess(w, r)
		return
	}

	if action != "pause" && action != "resume" && action != "restart" && action != "stop" {
		slog.Warn("lifecycle_command_rejected", "action", action, "outcome", "unknown_action", "remote", r.RemoteAddr)
		s.respondLifecycle(w, r, http.StatusBadRequest, lifecycleActionOutcome{kind: "unknown_action"})
		return
	}

	ctrl, _, _ := s.lifecycleProviders()
	if ctrl == nil {
		slog.Warn("lifecycle_command_rejected", "action", action, "outcome", "unavailable", "remote", r.RemoteAddr)
		s.respondLifecycle(w, r, http.StatusServiceUnavailable, lifecycleActionOutcome{kind: "unavailable"})
		return
	}

	// context.WithoutCancel: an accepted command's persist/transition must
	// survive the HTTP client disconnecting right after accept (design v6
	// §9: "client disconnect после accept — transition продолжается").
	ctx := context.WithoutCancel(r.Context())
	var res lifecycle.SubmitResult
	switch action {
	case "pause":
		res = ctrl.Pause(ctx)
	case "resume":
		res = ctrl.Resume(ctx)
	case "restart":
		res = ctrl.Restart(ctx)
	case "stop":
		res = ctrl.Stop(ctx)
	}

	logLifecycleCommand(r, action, res)

	switch res.Outcome {
	case lifecycle.OutcomeAccepted:
		s.respondLifecycle(w, r, http.StatusAccepted, lifecycleActionOutcome{kind: string(res.Outcome)})
	case lifecycle.OutcomeIdempotent:
		s.respondLifecycle(w, r, http.StatusOK, lifecycleActionOutcome{kind: string(res.Outcome)})
	case lifecycle.OutcomeRejected:
		if errors.Is(res.Err, ErrLifecyclePersist) {
			s.respondLifecycle(w, r, http.StatusInternalServerError, lifecycleActionOutcome{
				kind:   string(res.Outcome),
				detail: s.lifecycleText(r, "lc.result.persist_error"),
			})
			return
		}
		detail := ""
		if res.Err != nil {
			detail = res.Err.Error()
		}
		s.respondLifecycle(w, r, http.StatusConflict, lifecycleActionOutcome{kind: string(res.Outcome), detail: detail})
	default:
		// Unreachable with the real controller (Outcome is a closed set),
		// guarded so a future/fake outcome value degrades to a safe 500
		// rather than silently reporting success.
		s.respondLifecycle(w, r, http.StatusInternalServerError, lifecycleActionOutcome{kind: string(res.Outcome), detail: "unexpected outcome"})
	}
}

// lifecycleAuthGateBlocked is the ONE shared unauthenticated-mutation gate
// for both lifecycle POST handlers (handleAPILifecycleAction and
// handleLifecycleRestartProcess), extracted so the two call sites can never
// drift. Under DASHBOARD_INSECURE_NO_AUTH=true a mutation is refused unless
// lifecycleLANTrust explicitly trusts the connection's remote address
// against the DASHBOARD_TRUSTED_LAN_CIDRS allowlist (Ф4d). Auth-enabled and
// loopback-default runs are unaffected: this only ever fires when
// InsecureNoAuth is true. Returns true — having already written the 403
// response via respondLifecycle — when the caller must not proceed.
func (s *Server) lifecycleAuthGateBlocked(w http.ResponseWriter, r *http.Request) bool {
	dash := s.dashboardConfig()
	if !dash.InsecureNoAuth {
		return false
	}
	switch lifecycleLANTrust(dash, r.RemoteAddr) {
	case lanTrustAllowed:
		return false
	case lanTrustDenied:
		slog.Warn("lifecycle_mutation_blocked_lan_denied", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		s.respondLifecycle(w, r, http.StatusForbidden, lifecycleActionOutcome{kind: "lan_denied"})
		return true
	default: // lanTrustNotConfigured: exactly today's unconditional refusal.
		slog.Warn("lifecycle_mutation_blocked_insecure", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		s.respondLifecycle(w, r, http.StatusForbidden, lifecycleActionOutcome{kind: "insecure"})
		return true
	}
}

// handleLifecycleRestartProcess serves POST /api/lifecycle/restart-process
// (task contract D6): accepted ONLY when Observed==degraded, calling the
// injected requester exactly once (its own sync.Once makes repeats a no-op).
// This is the I31 process-exit path — cancelling the app's own run scope —
// never anything this package does directly.
func (s *Server) handleLifecycleRestartProcess(w http.ResponseWriter, r *http.Request) {
	// Reached today only via handleAPILifecycleAction's "restart-process"
	// dispatch, which already ran this same gate first (hoisted in the
	// corrective pass) — this second call is a cheap, idempotent no-op on
	// that path (dash.InsecureNoAuth/the CIDR classification haven't
	// changed between the two calls). It stays here as defense-in-depth in
	// case this handler is ever reached some other way.
	if s.lifecycleAuthGateBlocked(w, r) {
		return
	}

	ctrl, _, requester := s.lifecycleProviders()
	if ctrl == nil || requester == nil {
		slog.Warn("lifecycle_restart_process_rejected", "reason", "unavailable", "remote", r.RemoteAddr)
		s.respondLifecycle(w, r, http.StatusServiceUnavailable, lifecycleActionOutcome{kind: "unavailable"})
		return
	}

	snap := ctrl.Snapshot()
	if snap.Observed != lifecycle.ObservedDegraded {
		slog.Warn("lifecycle_restart_process_rejected", "observed", snap.Observed, "remote", r.RemoteAddr)
		s.respondLifecycle(w, r, http.StatusConflict, lifecycleActionOutcome{
			kind:   "rejected",
			detail: s.lifecycleText(r, "lc.result.restart_process_conflict"),
		})
		return
	}

	slog.Warn("lifecycle_restart_process_requested", "remote", r.RemoteAddr)
	requester()
	s.respondLifecycle(w, r, http.StatusAccepted, lifecycleActionOutcome{kind: "accepted"})
}

// logLifecycleCommand is §11's audit requirement: slog for EVERY command and
// every refusal (action, outcome, commandId, remote — never secrets).
func logLifecycleCommand(r *http.Request, action string, res lifecycle.SubmitResult) {
	if res.Outcome == lifecycle.OutcomeRejected {
		reason := ""
		if res.Err != nil {
			reason = res.Err.Error()
		}
		slog.Warn("lifecycle_command_rejected", "action", action, "outcome", string(res.Outcome), "commandId", res.CommandID, "remote", r.RemoteAddr, "reason", reason)
		return
	}
	slog.Info("lifecycle_command", "action", action, "outcome", string(res.Outcome), "commandId", res.CommandID, "remote", r.RemoteAddr)
}

// lifecycleText is the small tr() shim this file needs for the handful of
// server-localized result strings (persist error, restart-process conflict).
func (s *Server) lifecycleText(r *http.Request, key string) string {
	return s.i18n.T(s.langFromRequest(r), key)
}

// respondLifecycle is the single write path for every lifecycle GET/POST
// response (design v6 §9): an htmx request ALWAYS gets 200 + the lifecycle
// panel partial with the result rendered inside it; everything else gets
// jsonStatus + the JSON snapshot/outcome contract.
func (s *Server) respondLifecycle(w http.ResponseWriter, r *http.Request, jsonStatus int, ao lifecycleActionOutcome) {
	ctrl, updFn, restartRequester := s.lifecycleProviders()
	upd := lifecycleUpdateStateOf(updFn)
	dash := s.dashboardConfig()

	var snap lifecycle.Snapshot
	available := ctrl != nil
	if available {
		snap = ctrl.Snapshot()
	}

	if isHTMXRequest(r) {
		vm := s.buildLifecyclePanelView(available, snap, dash, upd, restartRequester != nil, ao, r)
		s.renderPartial(w, r, "lifecycle_panel", vm)
		return
	}

	resp := buildLifecycleResponse(snap, upd)
	resp.Outcome = ao.kind
	resp.Error = ao.detail
	writeJSON(w, jsonStatus, resp)
}

// LifecyclePanelView is the lifecycle_panel partial's view model.
type LifecyclePanelView struct {
	// Available is false only when no lifecycle controller is wired at all
	// (e.g. EnableAnalytics=false's no-controller-surface path, or a plain
	// pre-Ф4c test server) — the panel then renders nothing.
	Available bool

	DesiredState  string
	ObservedState string
	Transition    string
	Generation    uint64
	CommandID     string
	Reason        string
	LastError     string
	// NextRetryAt is pre-formatted "HH:MM:SS" in the dashboard's display
	// time zone; "" when no retry is scheduled.
	NextRetryAt string
	Override    bool

	IsTransitioning bool
	IsFailed        bool
	IsDegraded      bool

	// TransitionReason is the S5-3 lifecycle-honesty text shown next to the
	// button group whenever IsTransitioning is true: the pending-command slot
	// (design v6 §5.2 step 1) drives every Capabilities.Can* flag false for
	// the duration, which would otherwise render a semantically-relevant
	// button (Show*=true) as disabled with no visible explanation. Empty in
	// every steady (non-transitioning) state — never a fabricated warning.
	TransitionReason string

	CanPause   bool
	CanResume  bool
	CanRestart bool
	CanStop    bool

	// ShowX decide which primary-row buttons are semantically relevant for
	// the current Desired/Observed pair (independent of whether the command
	// happens to be accepted RIGHT NOW — that's CanX, used for the disabled
	// attribute); ShowRetry is a differently-labeled Resume for a failed
	// generation, ShowRestartProcess is the degraded-only I31 action.
	ShowPause          bool
	ShowResume         bool
	ShowRestart        bool
	ShowRetry          bool
	ShowRestartProcess bool
	// CanRestartProcess is false when no requester was wired (D1 nil-safe):
	// the button is hidden even for a degraded observed state.
	CanRestartProcess bool

	// InsecureDisabled means "unauthenticated mode AND this client may not
	// mutate": dash.InsecureNoAuth && trust != lanTrustAllowed. Buttons keep
	// using it (unchanged) for the disabled attribute — a trusted-LAN client
	// under InsecureNoAuth is NOT disabled.
	InsecureDisabled bool
	// LANState is "" whenever !dash.InsecureNoAuth (Basic Auth / loopback
	// modes carry no LAN messaging at all); otherwise one of "allowed" |
	// "not_configured" | "denied", driving which explanation block the
	// partial renders (Ф4d).
	LANState string

	UpdateState   string
	UpdateVersion string

	Version string

	// ResultKind/ResultMessage/ResultIsError render the just-happened POST's
	// outcome as a visible line inside the panel (design v6 §9's "КОНФЛИКТ —
	// видимый пользователю текст причины" requirement); ResultKind is ""
	// (nothing rendered) for a plain GET poll.
	ResultKind    string
	ResultMessage string
	ResultIsError bool

	// Announce is what the STATIC (non-swapped) aria-live node in
	// overview.html should say — write-only-on-change, decided client-side
	// by comparing against what it last wrote (the lifecycle_panel pitfall:
	// a role=status INSIDE the swapped node never announces, since every
	// swap creates a brand-new DOM node). Defaults to the current state
	// label; a just-happened POST's result message takes priority.
	Announce string
}

func (s *Server) buildLifecyclePanelView(available bool, snap lifecycle.Snapshot, dash runtimeconfig.Dashboard, upd LifecycleUpdateState, restartProcessWired bool, ao lifecycleActionOutcome, r *http.Request) LifecyclePanelView {
	vm := LifecyclePanelView{
		Available:     available,
		DesiredState:  string(snap.Desired),
		ObservedState: string(snap.Observed),
		Transition:    string(snap.Transition),
		Generation:    snap.Generation,
		CommandID:     snap.CommandID,
		Reason:        string(snap.Reason),
		LastError:     snap.LastError,
		Override:      snap.Override,
		CanPause:      snap.Capabilities.CanPause,
		CanResume:     snap.Capabilities.CanResume,
		CanRestart:    snap.Capabilities.CanRestart,
		CanStop:       snap.Capabilities.CanStop,
		UpdateState:   upd.State,
		UpdateVersion: upd.Version,
		Version:       version.Version,
	}

	// Ф4d: compute the trust classification once for THIS render, then
	// derive both InsecureDisabled (unchanged semantics for the six button
	// lines) and the new LANState (drives which explanation block renders)
	// from the single result. This is not the only lifecycleLANTrust call
	// for a gated POST: lifecycleAuthGateBlocked already ran its own,
	// separate check earlier in the request (before ever reaching here) to
	// decide whether to let the mutation through at all; this call exists
	// purely to render the panel that follows.
	trust := lifecycleLANTrust(dash, r.RemoteAddr)
	vm.InsecureDisabled = dash.InsecureNoAuth && trust != lanTrustAllowed
	if dash.InsecureNoAuth {
		switch trust {
		case lanTrustAllowed:
			vm.LANState = "allowed"
		case lanTrustDenied:
			vm.LANState = "denied"
		default:
			vm.LANState = "not_configured"
		}
	}

	if !snap.NextRetryAt.IsZero() {
		vm.NextRetryAt = snap.NextRetryAt.In(s.displayLocation()).Format("15:04:05")
	}

	vm.IsTransitioning = snap.Transition != lifecycle.TransitionNone
	vm.IsFailed = snap.Observed == lifecycle.ObservedFailed
	vm.IsDegraded = snap.Observed == lifecycle.ObservedDegraded
	if vm.IsTransitioning {
		vm.TransitionReason = s.lifecycleText(r, "lc.reason.transitioning")
	}

	vm.ShowPause = snap.Desired == lifecycle.DesiredRunning && !vm.IsDegraded
	vm.ShowResume = (snap.Desired == lifecycle.DesiredPaused || snap.Desired == lifecycle.DesiredStopped) && !vm.IsDegraded
	vm.ShowRestart = snap.Desired == lifecycle.DesiredRunning && !vm.IsDegraded && !vm.IsFailed
	vm.ShowRetry = vm.IsFailed
	vm.ShowRestartProcess = vm.IsDegraded
	vm.CanRestartProcess = restartProcessWired

	vm.ResultKind = ao.kind
	vm.ResultIsError = ao.kind == "rejected" || ao.kind == "insecure" || ao.kind == "lan_denied" || ao.kind == "unknown_action" || ao.kind == "unavailable"
	vm.ResultMessage = s.lifecycleResultMessage(r, ao)

	if available {
		vm.Announce = s.lifecycleText(r, "lc.state."+string(snap.Observed))
	}
	if vm.ResultMessage != "" {
		vm.Announce = vm.ResultMessage
	}

	return vm
}

func (s *Server) lifecycleResultMessage(r *http.Request, ao lifecycleActionOutcome) string {
	tr := func(key string) string { return s.lifecycleText(r, key) }
	switch ao.kind {
	case "":
		return ""
	case "accepted":
		return tr("lc.result.accepted")
	case "idempotent":
		return tr("lc.result.idempotent")
	case "rejected":
		if ao.detail != "" {
			return tr("lc.result.rejected") + ": " + ao.detail
		}
		return tr("lc.result.rejected")
	case "insecure":
		return tr("lc.insecure_disabled")
	case "lan_denied":
		return tr("lc.result.lan_denied")
	case "unknown_action":
		return tr("lc.result.unknown_action")
	case "unavailable":
		return tr("lc.result.unavailable")
	default:
		return ao.detail
	}
}
