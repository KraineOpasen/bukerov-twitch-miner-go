package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/gql"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/util"
)

var (
	ErrStreamerDoesNotExist = errors.New("streamer does not exist")
	ErrStreamerIsOffline    = errors.New("streamer is offline")
	// ErrPlaybackSessionStale is returned by a session refresh whose atomic apply
	// was superseded by a newer observation (or a broadcast/generation that moved
	// during the I/O). It is inconclusive — the newer session is authoritative — so
	// classifyCheck maps it to UNKNOWN and it confirms neither online nor offline.
	// It carries no URL, token, payload, or body.
	ErrPlaybackSessionStale = errors.New("playback session superseded by a newer observation")

	// ErrRewardUnavailable is returned when a custom channel-points reward is
	// not (or no longer) redeemable — it disappeared from the channel, was
	// disabled/paused, went out of stock, or is on cooldown. Distinct from a
	// transport error so callers can show a "reward no longer available"
	// message instead of a generic failure.
	ErrRewardUnavailable = errors.New("reward is not available")

	// ErrInsufficientPoints is returned when the viewer's channel-points
	// balance is below a custom reward's cost.
	ErrInsufficientPoints = errors.New("not enough channel points")

	// ErrRewardInputRequired is returned when a reward requires viewer text
	// input but none was supplied.
	ErrRewardInputRequired = errors.New("this reward requires text input")

	// ErrUnauthorized indicates the Twitch OAuth token was rejected (expired
	// or revoked). Callers should treat this as "reauthorization required"
	// rather than a transient failure.
	ErrUnauthorized = errors.New("twitch: unauthorized (token expired or revoked)")

	// ErrPersistedQueryNotFound indicates every candidate Twitch client ID
	// returned PersistedQueryNotFound for an operation — i.e. the persisted-query
	// hash the code ships (or the client metadata) is stale because Twitch
	// rotated it server-side. It is deliberately distinct from
	// ErrStreamerDoesNotExist / ErrStreamerIsOffline so a stale-hash outage is
	// never misreported as "streamer does not exist" and so callers can keep the
	// last-known state (points, campaigns, online flag) instead of wiping it on
	// what is a temporary, Twitch-side failure. Recovery is a hash update in
	// internal/constants/gql.go (see the per-operation client-ID fallback below).
	ErrPersistedQueryNotFound = errors.New("twitch: persisted query not found (stale query hash or client metadata)")

	// errAmbiguousDiagnosticJSON marks a DIAGNOSTIC response whose raw JSON
	// carries duplicate object members, so decoding it would silently pick one
	// of two conflicting readings. Unexported: only the observation raises and
	// classifies it, and no business path changes behaviour on it.
	errAmbiguousDiagnosticJSON = errors.New("twitch: ambiguous diagnostic JSON response (duplicate object members)")

	// errOversizedDiagnosticJSON marks a DIAGNOSTIC response carrying more JSON
	// values than any credible answer to a diagnostic operation. Unexported for
	// the same reason as errAmbiguousDiagnosticJSON: only the observation
	// raises and classifies it.
	errOversizedDiagnosticJSON = errors.New("twitch: oversized diagnostic JSON response (too many values)")

	// ErrClaimNotAccepted is returned only when Twitch authoritatively rejected a
	// bonus claim in the mutation's business-result node. Missing/null/malformed
	// responses use ErrBonusClaimIndeterminate and are quarantined, never retried.
	ErrClaimNotAccepted = errors.New("twitch: claim not accepted")
	// ErrBonusClaimIndeterminate means the mutation may have executed but the
	// response did not authoritatively prove acceptance or rejection. Callers
	// must fail closed and must not retry this claim ID.
	ErrBonusClaimIndeterminate = errors.New("twitch: bonus claim outcome indeterminate")

	// ErrDirectBonusMutation prevents callers from bypassing the Streamer-owned
	// bonus arbitration through the generic PostGQL/read transport.
	ErrDirectBonusMutation = errors.New("twitch: direct bonus mutation bypasses arbitration")
)

// StreamCheckError classifies a stream-status check that could NOT be resolved to
// an authoritative online/offline: a malformed/absent response, a top-level
// GraphQL error, a missing Spade URL, or another inconclusive outcome. It carries
// a compact, privacy-safe models.StatusReason (never any raw payload, token, or
// header) so CheckStreamerOnline maps it to UNKNOWN — never a false offline. It is
// deliberately distinct from ErrStreamerIsOffline, which is reserved for the ONE
// authoritative GQL offline shape: user present, "stream" key present and JSON null.
type StreamCheckError struct {
	Reason models.StatusReason
	Err    error
}

func (e *StreamCheckError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("stream check inconclusive (%s): %v", e.Reason, e.Err)
	}
	return fmt.Sprintf("stream check inconclusive (%s)", e.Reason)
}

func (e *StreamCheckError) Unwrap() error { return e.Err }

// newStreamCheckError builds a classified inconclusive-check error.
func newStreamCheckError(reason models.StatusReason, format string, args ...any) *StreamCheckError {
	return &StreamCheckError{Reason: reason, Err: fmt.Errorf(format, args...)}
}

// classifyCheck maps the error returned by a stream-status fetch (GetStreamInfo /
// UpdateStream) to the authoritative tri-state transition it justifies. This is
// the single place the "errors mean UNKNOWN, not offline" policy is enforced:
// only nil is online and only ErrStreamerIsOffline is offline — every other
// error (transport, timeout, auth, PersistedQueryNotFound, top-level GraphQL
// errors, malformed/absent structural fields, Spade failure, cancelled context)
// is UNKNOWN with a specific reason.
func classifyCheck(err error) (models.StreamerStatus, models.StatusReason) {
	switch {
	case err == nil:
		return models.StatusOnline, ""
	case errors.Is(err, ErrStreamerIsOffline):
		return models.StatusOffline, ""
	case errors.Is(err, ErrPlaybackSessionStale):
		// A superseded session apply is inconclusive — never a false online/offline.
		return models.StatusUnknown, models.ReasonSessionStale
	case errors.Is(err, ErrPersistedQueryNotFound):
		return models.StatusUnknown, models.ReasonPersistedQueryMissing
	case errors.Is(err, ErrUnauthorized):
		return models.StatusUnknown, models.ReasonUnauthorized
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return models.StatusUnknown, models.ReasonTimeout
	}
	var sce *StreamCheckError
	if errors.As(err, &sce) {
		return models.StatusUnknown, sce.Reason
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return models.StatusUnknown, models.ReasonTimeout
	}
	return models.StatusUnknown, models.ReasonTransportError
}

const (
	// gqlMaxRetries is the number of retries attempted after the initial try,
	// i.e. up to gqlMaxRetries+1 total attempts per GQL request.
	gqlMaxRetries = 4
)

type TwitchClient struct {
	auth          *auth.TwitchAuth
	deviceID      string
	clientSession string
	clientVersion string
	userAgent     string
	client        *http.Client

	// gqlURL is the GraphQL endpoint. It defaults to constants.GQLURL in
	// production and is only overridden by tests (setGQLEndpoint) to point at a
	// local httptest server.
	gqlURL string

	twilightBuildIDPattern *regexp.Regexp
	spadeURLPattern        *regexp.Regexp
	settingsURLPattern     *regexp.Regexp

	// spadeHTTP is the narrow HTTP surface Spade discovery uses (channel page +
	// settings.js fetch). It defaults to c.client in production; tests inject a
	// fake so the strict fetch/validation can be exercised without real DNS.
	// twitchBaseURL is the channel-page origin, constants.TwitchURL in production
	// and a test double otherwise.
	spadeHTTP           spadeHTTPClient
	twitchBaseURL       string
	maxChannelPageBytes int64
	maxSettingsBytes    int64

	// beforeSessionApply, when set, is invoked on the refresh goroutine AFTER all
	// fetch/parse/campaign-availability work and JUST BEFORE the single atomic
	// playback-session apply. Nil in production; tests use it as a deterministic
	// barrier to prove no partial session tuple is ever visible during the I/O.
	beforeSessionApply func()

	// afterRefreshObservation, when set, is invoked on the refresh goroutine
	// immediately AFTER the atomic observation pair is reserved and BEFORE any
	// network I/O. Nil in production; tests use it as a deterministic barrier to
	// interleave concurrent refreshes at the observation-reservation boundary.
	afterRefreshObservation func()

	authErrorHandler func()

	// recoverFn, when set (tests only), replaces the real single-flight auth
	// recovery so replay semantics can be exercised deterministically without
	// an OAuth endpoint. Nil in production (recoverAuth calls auth.Recover).
	recoverFn func(rejectedGeneration uint64) (auth.Snapshot, error)

	healthMu    sync.RWMutex
	lastSuccess time.Time

	// gqlFailures records the timestamps of GQL request cycles that exhausted
	// every retry, as a self-synchronized sliding window. It is deliberately NOT
	// folded into healthMu (which only guards lastSuccess, not the request path).
	// The connection-health watchdog reads RecentGQLFailures to raise a
	// "degraded" signal when the API repeatedly gives up short of a full blackout.
	gqlFailures eventWindow

	// connAcct tracks GQL request attempts and reachable-but-functional failures,
	// so the connection-health classifier can tell an idle client (no attempts)
	// from a failing one. Self-synchronized; see connhealth.go.
	connAcct apiConnAccount

	// clientIDMu guards the GQL client-ID fallback state below. The same
	// *TwitchClient is shared across goroutines (watcher, drops sync, discovery,
	// and the health canary all call it concurrently), so this state must be
	// synchronized rather than left as a plain map.
	//
	//   - defaultClientID is the client ID uncached operations start with and the
	//     one ActiveClientID reports. It is promoted to a working fallback only
	//     when a PersistedQueryNotFound is actually resolved by switching IDs.
	//   - opClientID caches, per operation name, the last client ID that served
	//     it without PersistedQueryNotFound, so a recovered operation tries its
	//     known-good ID first instead of re-walking the whole candidate list.
	clientIDMu      sync.RWMutex
	defaultClientID string
	opClientID      map[string]string

	// gameSlugs caches game display name (lowercased) -> directory slug
	// lookups for the discovery subsystem; slugs are stable, so caching
	// halves that subsystem's GQL calls per sync.
	slugMu    sync.Mutex
	gameSlugs map[string]string

	mu sync.RWMutex
}

func NewTwitchClient(twitchAuth *auth.TwitchAuth, deviceID string) *TwitchClient {
	c := &TwitchClient{
		auth:                   twitchAuth,
		deviceID:               deviceID,
		clientSession:          util.RandomHex(16),
		clientVersion:          constants.DefaultClientVersion,
		userAgent:              constants.TVUserAgent,
		client:                 &http.Client{Timeout: 30 * time.Second},
		gqlURL:                 constants.GQLURL,
		defaultClientID:        constants.ClientIDTV,
		opClientID:             make(map[string]string),
		twilightBuildIDPattern: regexp.MustCompile(`window\.__twilightBuildID\s*=\s*"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"`),
		spadeURLPattern:        regexp.MustCompile(`"spade_url":"(.*?)"`),
		settingsURLPattern:     regexp.MustCompile(`(https://static.twitchcdn.net/config/settings.*?js|https://assets.twitch.tv/config/settings.*?.js)`),
		lastSuccess:            time.Now(),
		twitchBaseURL:          constants.TwitchURL,
		maxChannelPageBytes:    maxChannelPageBytes,
		maxSettingsBytes:       maxSettingsBytes,
	}
	c.spadeHTTP = c.client
	return c
}

// setGQLEndpoint overrides the GraphQL endpoint URL. It exists for tests, which
// point the client at a local httptest server; production always uses
// constants.GQLURL set by NewTwitchClient.
func (c *TwitchClient) setGQLEndpoint(url string) {
	c.gqlURL = url
}

// SetAuthErrorHandler registers a callback invoked the first time (and every
// subsequent time) a request fails with ErrUnauthorized.
func (c *TwitchClient) SetAuthErrorHandler(handler func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authErrorHandler = handler
}

// LastSuccessAt returns when the client last completed a request without an
// auth or transport error. Used by the connection-health watchdog.
func (c *TwitchClient) LastSuccessAt() time.Time {
	c.healthMu.RLock()
	defer c.healthMu.RUnlock()
	return c.lastSuccess
}

func (c *TwitchClient) markSuccess() {
	c.healthMu.Lock()
	c.lastSuccess = time.Now()
	c.healthMu.Unlock()
}

// RecentGQLFailures returns how many GQL request cycles exhausted all retries
// within the trailing window. Used by the connection-health watchdog to flag a
// degraded (repeatedly failing but not fully stale) API link.
func (c *TwitchClient) RecentGQLFailures(window time.Duration) int {
	return c.gqlFailures.count(time.Now(), window)
}

func (c *TwitchClient) handleUnauthorized() {
	c.mu.RLock()
	handler := c.authErrorHandler
	c.mu.RUnlock()

	if handler != nil {
		handler()
	}
}

// isAuthError reports whether an HTTP status code or GQL response body
// indicates the OAuth token was rejected. Only HTTP 401 is an authoritative
// token rejection per the official Twitch contract; HTTP 403 is a
// permission/scope/business rejection and deliberately does NOT count — it
// must never trigger a token refresh or device flow.
//
// The 200-body checks are a strict ALLOWLIST of the two shapes evidenced in
// this repository (private GQL error shapes are not publicly documented, so
// nothing beyond repo evidence is trusted):
//   - top-level {"error":"Unauthorized", ...} — the long-standing contract
//     this client has checked since the original implementation;
//   - errors[].message EXACTLY "Unauthorized" — the fixture shape in
//     claim_test.go, matched by normalized equality, never by substring: a
//     business/permission message that merely CONTAINS the word (e.g.
//     "unauthorized entitlement") is an ordinary GQL error, and treating it
//     as a token rejection would spend a one-time refresh grant per
//     occurrence.
func isAuthError(statusCode int, result map[string]interface{}) bool {
	if statusCode == http.StatusUnauthorized {
		return true
	}

	if errMsg, ok := result["error"].(string); ok && strings.EqualFold(strings.TrimSpace(errMsg), "Unauthorized") {
		return true
	}

	if errs, ok := result["errors"].([]interface{}); ok {
		for _, e := range errs {
			if em, ok := e.(map[string]interface{}); ok {
				// Byte-exact match of the one evidenced fixture shape; even a
				// case variant is not trusted to mean a token rejection.
				if msg, ok := em["message"].(string); ok && strings.TrimSpace(msg) == "Unauthorized" {
					return true
				}
			}
		}
	}

	return false
}

// diagnosticRequestKey marks a request context as carrying a DIAGNOSTIC-ONLY
// read.
//
// This is the transport owner's distinction between a BUSINESS read — one the
// miner acts on, whose success or failure is real evidence about the Twitch
// link — and a diagnostic observation, which must be able to fail without
// telling the rest of the process anything about connectivity.
//
// It exists because the connectivity accounting below is shared, and it feeds
// the auto-bet gate: functionalFailures reaches TwitchClient.ConnHealth ->
// internal/miner.classifyAPI -> health.SignalGQLAPI = DEGRADED ->
// minerBetHealthGate blocks every automated prediction bet. A diagnostic read
// of an operation whose live acceptance is not established (see
// constants.RewardList) would otherwise pin that gate closed forever on an
// outcome that says nothing about whether Twitch is reachable.
//
// The suppression is SYMMETRIC: a diagnostic request contributes neither
// failure nor success to the accounting, and never escalates to the operator
// reauth path. It must not be able to mask a real outage any more than it can
// invent one.
//
// Scope, stated precisely rather than generously: no WARN or ERROR is raised
// for the request's own OUTCOME (stale hash, retries, exhaustion — all DEBUG),
// and the retry trace carries no raw transport error text, only its bounded
// status.
//
// The shared CLIENT-ID POOL is isolated too, and completely: a diagnostic read
// caches NOTHING. It does not promote the process-wide defaultClientID, it does
// not pin a per-operation candidate, and it never raises the WARN that
// announces the shipped persisted-query hashes are stale. rememberWorkingClientID
// promotes on any fallback answer that is not PersistedQueryNotFound — a 401
// included — so without the split a diagnostic that FAILED would steer the
// client ID a later business call goes out under, and would tell the operator to
// update internal/constants/gql.go when nothing had resolved. A BUSINESS
// rotation still promotes and still warns, unchanged.
//
// What a diagnostic read SHARES with a business read is the candidate order,
// the transient-retry schedule and gqlMaxRetries. It additionally (see
// doGQLRequestWithClientIDFallback, doGQLRequestWithRetry, doGQLOnceWithClient
// and gqlSingleRoundTrip): draws every explicit dispatch from a per-cycle
// DiagnosticAllowance, so candidates and retries stop when it is spent; never
// follows a redirect; caps the body at maxDiagnosticResponseBytes; stops the
// candidate loop and drops the body on any non-2xx; refuses oversized or
// ambiguous JSON; recognises PersistedQueryNotFound structurally; settles
// authorization on the HTTP status alone; never replays on an auth rejection;
// and can return errDiagnosticAllowanceExhausted, errAmbiguousDiagnosticJSON
// or errOversizedDiagnosticJSON, none of which a business read ever sees.
type diagnosticRequestKey struct{}

// DiagnosticAllowance caps the explicit authenticated diagnostic HTTP dispatches
// one logical cycle may make. The caller that owns the cycle creates it, hands
// it to every ObserveWatchStreakMilestone of that cycle, and lets it go when
// the cycle ends: it is per-cycle state, not a limiter on the client, and it is
// deliberately NOT safe for concurrent use, because a diagnostic stage has
// exactly one goroutine.
//
// The unit is one explicit http.Client.Do call made by this package - a first
// attempt, a retry or a client-ID fallback alike. It is an application attempt
// cap, not a wire-delivery bound: net/http may transparently replay a request
// on a dead keep-alive connection, and nothing here claims to count what
// Twitch counts.
type DiagnosticAllowance struct {
	remaining int
	spent     int
}

// NewDiagnosticAllowance returns an allowance of permits explicit dispatches.
func NewDiagnosticAllowance(permits int) *DiagnosticAllowance {
	if permits < 0 {
		permits = 0
	}
	return &DiagnosticAllowance{remaining: permits}
}

// Remaining reports how many dispatches may still be made. It consumes nothing,
// which is what lets a caller decide whether to START another target, fallback
// or retry without spending the permit that decision needs.
func (a *DiagnosticAllowance) Remaining() int { return a.remaining }

// Spent reports how many dispatches were charged.
func (a *DiagnosticAllowance) Spent() int { return a.spent }

// charge takes one permit immediately before a dispatch. It never refunds:
// a dispatch that errors, times out or is cancelled has still been made.
func (a *DiagnosticAllowance) charge() bool {
	if a.remaining == 0 {
		return false
	}
	a.remaining--
	a.spent++
	return true
}

// errDiagnosticAllowanceExhausted is the stopping reason a diagnostic read
// returns when its cycle's dispatch allowance ran out before the read could
// finish. It is a LOCAL decision, never a transport fact: it is not transient,
// it must not be retried, and a candidate traversal it cut short is incomplete
// evidence rather than proof that every client ID rejected the query.
var errDiagnosticAllowanceExhausted = errors.New("twitch: diagnostic dispatch allowance exhausted")

// diagnosticRequest is the value a diagnostic-only context carries. It is a
// pointer so the transport can count the dispatches it actually makes and the
// caller can read that count after the request returns; the count is per
// ObserveWatchStreakMilestone call, because that is where the value is created.
type diagnosticRequest struct {
	// allowance is the cycle's shared permit pool. nil means uncapped, which is
	// what non-cycle callers and tests use; the miner always passes one.
	allowance *DiagnosticAllowance
	// dispatches counts the explicit http.Client.Do calls this request made.
	dispatches int
}

// withDiagnosticRequest marks ctx as a diagnostic-only read that draws on
// allowance (nil = uncapped). Cancellation and deadlines propagate unchanged.
// The stored value is ALWAYS non-nil: the mark is the pointer's presence, not
// the allowance's.
func withDiagnosticRequest(ctx context.Context, allowance *DiagnosticAllowance) context.Context {
	return context.WithValue(ctx, diagnosticRequestKey{}, &diagnosticRequest{allowance: allowance})
}

// diagnosticRequestOf returns the diagnostic value ctx carries, or nil for an
// unmarked (business) context. Like every other context consumer in this
// package it requires a non-nil ctx.
func diagnosticRequestOf(ctx context.Context) *diagnosticRequest {
	dr, _ := ctx.Value(diagnosticRequestKey{}).(*diagnosticRequest)
	return dr
}

// isDiagnosticRequest reports whether ctx was marked diagnostic-only. A nil or
// unmarked context is a business request, so the accounting default is
// unchanged for every pre-existing caller.
func isDiagnosticRequest(ctx context.Context) bool {
	return diagnosticRequestOf(ctx) != nil
}

// PostGQL runs one GQL operation under the client's own lifetime. A caller that
// owns a cancellation scope (today: the watch generation, through the ctx-taking
// entry points below) reaches postGQLRequest directly with its context; this
// exported form keeps the background ownership every other subsystem — drops,
// pubsub, claims, predictions, discovery, chat, web — already has.
func (c *TwitchClient) PostGQL(operation constants.GQLOperation) (map[string]interface{}, error) {
	return c.postGQLRequest(context.Background(), operation)
}

func (c *TwitchClient) PostGQLBatch(operations []constants.GQLOperation) ([]map[string]interface{}, error) {
	return c.postGQLBatchRequest(context.Background(), operations)
}

func isBonusMutationOperation(operation constants.GQLOperation) bool {
	return operation.OperationName == constants.ClaimCommunityPoints.OperationName ||
		operation.Extensions.PersistedQuery.SHA256Hash == constants.ClaimCommunityPoints.Extensions.PersistedQuery.SHA256Hash
}

// gqlSingleRoundTrip performs one complete single-operation GQL cycle (client
// ID fallback + transient retry + parse) signed with the given token, and
// reports whether the outcome was an authoritative auth rejection. The
// marshaled body is reused verbatim across recovery replays, so a replayed
// request is byte-identical to the original.
func (c *TwitchClient) gqlSingleRoundTrip(ctx context.Context, body []byte, operationName, token string) (result map[string]interface{}, statusCode int, authRejected bool, err error) {
	respBody, statusCode, err := c.doGQLRequestWithClientIDFallback(ctx, body, operationName, token)
	if err != nil {
		// Includes ErrPersistedQueryNotFound when every candidate client ID
		// returned PersistedQueryNotFound. Returning it here (instead of an empty
		// map) is what stops callers from misreading a stale hash as "streamer
		// does not exist" or wiping their last-known state.
		return nil, statusCode, false, err
	}

	// HTTP 401 is the authoritative token rejection regardless of body shape.
	if statusCode == http.StatusUnauthorized {
		return nil, statusCode, true, nil
	}
	// HTTP 403 is a permission/scope/business rejection: NOT an auth
	// rejection (no refresh, no device flow) and NOT data either — its error
	// body must never reach per-operation parsers as if it were a result, nor
	// refresh the connection-health timestamp.
	if statusCode == http.StatusForbidden {
		if !isDiagnosticRequest(ctx) {
			c.connAcct.markFunctionalFailure(time.Now())
		}
		return nil, statusCode, false, fmt.Errorf("twitch GQL %s: permission denied (status 403)", operationName)
	}

	if len(bytes.TrimSpace(respBody)) == 0 {
		return nil, statusCode, false, fmt.Errorf("twitch GQL %s: empty response body", operationName)
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, statusCode, false, fmt.Errorf("failed to unmarshal %s response: %w", operationName, err)
	}

	// A diagnostic read settles authorization on the STATUS alone. A 401 was
	// answered above; a 2xx body that merely says "Unauthorized" is a string
	// the peer chose, not a token rejection, and letting it pick the recorded
	// class is exactly the peer-controlled misclassification this read
	// refuses everywhere else. isAuthError's body rule stays for business
	// reads, whose responses are not attacker-shaped text and whose token
	// rejections must still drive credential recovery.
	if isDiagnosticRequest(ctx) {
		return result, statusCode, false, nil
	}
	return result, statusCode, isAuthError(statusCode, result), nil
}

// postGQLRequest is the ordinary read entry point. It keeps its historical
// signature so every existing caller is untouched; callers that must judge the
// HTTP status themselves use postGQLRequestWithStatus.
func (c *TwitchClient) postGQLRequest(ctx context.Context, operation constants.GQLOperation) (map[string]interface{}, error) {
	result, _, err := c.postGQLRequestWithStatus(ctx, operation)
	return result, err
}

// postGQLRequestWithStatus is postGQLRequest plus the HTTP status of the
// response it parsed.
//
// The shared transport special-cases only 401 and 403. For a BUSINESS caller
// every other non-2xx body that happens to be JSON is returned verbatim as a
// result, so a caller that treats "this decoded into the shape I expected" as
// proof of success would accept a 400, a 404 or a redirect page as data. For a
// DIAGNOSTIC caller doGQLRequestWithClientIDFallback drops any non-2xx body
// instead and the round trip surfaces an error; the status is exposed so that
// caller can name the failure by status rather than by shape.
func (c *TwitchClient) postGQLRequestWithStatus(ctx context.Context, operation constants.GQLOperation) (map[string]interface{}, int, error) {
	if isBonusMutationOperation(operation) {
		return nil, 0, ErrDirectBonusMutation
	}
	body, err := json.Marshal(operation)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to marshal operation: %w", err)
	}

	// Capture the credential snapshot the request is signed with; its
	// Generation is what a recovery is keyed on, so a rejection of an
	// already-rotated token never triggers a second refresh.
	snap := c.auth.Snapshot()
	result, statusCode, authRejected, err := c.gqlSingleRoundTrip(ctx, body, operation.OperationName, snap.AccessToken)
	if err != nil {
		return nil, statusCode, err
	}

	if authRejected {
		// A diagnostic read owns no credential recovery: it neither drives a
		// token refresh nor escalates to the operator reauth path. It reports
		// ErrUnauthorized and stops. Business operations own auth recovery,
		// and an observation must not be the thing that declares the session
		// dead.
		if isDiagnosticRequest(ctx) {
			return nil, statusCode, fmt.Errorf("%w: operation %s", ErrUnauthorized, operation.OperationName)
		}
		// Serialized recovery + exactly ONE replay of the identical body. A
		// replay that is rejected again surfaces ErrUnauthorized with no
		// further recovery and no third request.
		newSnap, rerr := c.recoverAuth(ctx, snap.Generation)
		if rerr != nil {
			// Only a DEFINITIVE recovery failure escalates to the operator
			// reauth path; a transient endpoint failure or a bounded-wait
			// timeout (recovery still running) stays a retryable
			// ErrUnauthorized for this one request.
			if !isTransientRecoveryFailure(rerr) {
				c.handleUnauthorized()
			}
			return nil, statusCode, fmt.Errorf("%w: operation %s", ErrUnauthorized, operation.OperationName)
		}
		result, statusCode, authRejected, err = c.gqlSingleRoundTrip(ctx, body, operation.OperationName, newSnap.AccessToken)
		if err != nil {
			return nil, statusCode, err
		}
		if authRejected {
			c.handleUnauthorized()
			return nil, statusCode, fmt.Errorf("%w: operation %s", ErrUnauthorized, operation.OperationName)
		}
	}

	// A top-level "errors" array means Twitch rejected the operation at the GQL
	// layer (PersistedQueryNotFound after client-ID exhaustion, service error,
	// etc.) and returned no authoritative data, even when the HTTP status is
	// 200. Such a response must NOT refresh the connection-health timestamp, or
	// the watchdog/canary would read a total GQL outage as "GQL API ok".
	// Checked explicitly here rather than via isAuthError, which only covers
	// token rejection. The result is returned unchanged so per-operation
	// parsing behaves exactly as before.
	if isDiagnosticRequest(ctx) {
		// Symmetric suppression: a diagnostic read neither refreshes the
		// health timestamp nor records a functional failure. Letting it
		// refresh lastSuccess would let a diagnostic that Twitch happens to
		// answer mask a real business-path outage.
		return result, statusCode, nil
	}
	if !gql.HasTopLevelErrors(result) {
		c.markSuccess()
	} else {
		c.connAcct.markFunctionalFailure(time.Now())
	}
	return result, statusCode, nil
}

// postBonusMutation is the only transport allowed to send
// ClaimCommunityPoints. Unlike the generic read transport it never replays an
// ambiguous network/429/5xx/read/redirect outcome. It may try another client ID
// only after an exact APQ-not-found response with no data, and may replay once
// after an HTTP 401 because both outcomes prove the mutation did not execute.
func (c *TwitchClient) postBonusMutation(operation constants.GQLOperation) (map[string]interface{}, bonusMutationDelivery, error) {
	body, err := json.Marshal(operation)
	if err != nil {
		return nil, bonusMutationProvenNotExecuted, fmt.Errorf("failed to marshal operation: %w", err)
	}

	snap := c.auth.Snapshot()
	result, authRejected, delivery, err := c.bonusMutationRoundTrip(body, operation.OperationName, snap.AccessToken)
	if err != nil {
		return nil, delivery, err
	}
	if authRejected {
		newSnap, recoveryErr := c.recoverAuth(context.Background(), snap.Generation)
		if recoveryErr != nil {
			if !isTransientRecoveryFailure(recoveryErr) {
				c.handleUnauthorized()
			}
			return nil, bonusMutationProvenNotExecuted, fmt.Errorf("%w: operation %s", ErrUnauthorized, operation.OperationName)
		}
		result, authRejected, delivery, err = c.bonusMutationRoundTrip(body, operation.OperationName, newSnap.AccessToken)
		if err != nil {
			return nil, delivery, err
		}
		if authRejected {
			c.handleUnauthorized()
			return nil, bonusMutationProvenNotExecuted, fmt.Errorf("%w: operation %s", ErrUnauthorized, operation.OperationName)
		}
	}

	return result, bonusMutationResponse, nil
}

// bonusMutationRoundTrip performs one no-ambiguous-replay mutation cycle for a
// credential snapshot. HTTP-200 GraphQL "Unauthorized" is intentionally not an
// auth-replay authority here: without an exact non-execution contract it remains
// an indeterminate top-level GraphQL outcome.
func (c *TwitchClient) bonusMutationRoundTrip(body []byte, operationName, token string) (map[string]interface{}, bool, bonusMutationDelivery, error) {
	c.connAcct.markAttempt(time.Now())
	candidates := c.candidateClientIDs(operationName)

	for i, clientID := range candidates {
		respBody, statusCode, requestStarted, err := c.doGQLMutationOnce(body, operationName, clientID, token)
		if err != nil {
			if !requestStarted {
				return nil, false, bonusMutationProvenNotExecuted, err
			}
			c.gqlFailures.mark(time.Now())
			if statusCode != 0 {
				c.connAcct.markFunctionalFailure(time.Now())
			}
			slog.Warn("Bonus claim mutation outcome is indeterminate; refusing automatic replay",
				"operation", operationName,
				"status", statusCode,
				"error", err,
			)
			return nil, false, bonusMutationIndeterminate, err
		}

		// HTTP 401 is a pre-execution authentication rejection. The caller owns
		// the one existing auth-recovery cycle and may replay once with new creds.
		if statusCode == http.StatusUnauthorized {
			return nil, true, bonusMutationProvenNotExecuted, nil
		}
		if statusCode != http.StatusOK {
			c.connAcct.markFunctionalFailure(time.Now())
			return nil, false, bonusMutationIndeterminate,
				fmt.Errorf("twitch GQL %s: non-success status %d", operationName, statusCode)
		}
		if len(bytes.TrimSpace(respBody)) == 0 {
			c.connAcct.markFunctionalFailure(time.Now())
			return nil, false, bonusMutationIndeterminate,
				fmt.Errorf("twitch GQL %s: empty response body", operationName)
		}

		// Duplicate JSON object members are ambiguous for every mutation outcome,
		// not only APQ fallback. encoding/json otherwise keeps the last value, so
		// conflicting data or embedded-error members could be erased and turn an
		// unproven response into a fresh local success. Reject the raw body before
		// either retry proof or ordinary result classification.
		if !jsonObjectKeysAreUnique(respBody) {
			c.connAcct.markFunctionalFailure(time.Now())
			return nil, false, bonusMutationIndeterminate,
				fmt.Errorf("twitch GQL %s: ambiguous or malformed JSON response", operationName)
		}

		if strictPersistedQueryNotFound(respBody) {
			if i+1 < len(candidates) {
				slog.Warn("Bonus claim APQ was not found; trying the next Twitch client ID",
					"operation", operationName,
					"clientID", clientID,
					"remainingCandidates", len(candidates)-i-1,
				)
			}
			continue
		}

		var result map[string]interface{}
		if err := json.Unmarshal(respBody, &result); err != nil {
			c.connAcct.markFunctionalFailure(time.Now())
			return nil, false, bonusMutationIndeterminate,
				fmt.Errorf("failed to unmarshal %s response: %w", operationName, err)
		}

		c.rememberWorkingClientID(operationName, clientID, i > 0)
		return result, false, bonusMutationResponse, nil
	}

	c.connAcct.markFunctionalFailure(time.Now())
	return nil, false, bonusMutationProvenNotExecuted,
		fmt.Errorf("%w: operation %s (tried %d client IDs)", ErrPersistedQueryNotFound, operationName, len(candidates))
}

// strictPersistedQueryNotFound is deliberately stronger than the generic
// substring detector: mutation replay is authorized only when every top-level
// error has the exact APQ code and no usable data accompanies it.
func strictPersistedQueryNotFound(respBody []byte) bool {
	if !jsonObjectKeysAreUnique(respBody) {
		return false
	}
	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return false
	}
	if _, present := result["error"]; present {
		return false
	}
	if data, present := result["data"]; present && data != nil {
		return false
	}
	errorsList, ok := result["errors"].([]interface{})
	if !ok || len(errorsList) == 0 {
		return false
	}
	for _, rawError := range errorsList {
		errorObject, ok := rawError.(map[string]interface{})
		if !ok {
			return false
		}
		extensions, ok := errorObject["extensions"].(map[string]interface{})
		if !ok || extensions["code"] != "PERSISTED_QUERY_NOT_FOUND" {
			return false
		}
	}
	return true
}

// maxDiagnosticJSONValues bounds how many JSON values one DIAGNOSTIC response
// may contain, counted over the RAW body before anything decodes it.
//
// The byte limit and the collection limit each bound something real, and
// neither bounds this. maxDiagnosticResponseBytes bounds the wire. The
// milestone collection limit bounds what the PARSER retains. Between them sits
// encoding/json, which materialises the entire document into interface values
// before either the parser or its guard is reached: measured, a 1,048,575-byte
// body of "0," elements still cost ~97 MB and ~300 ms to refuse, because the
// refusal ran after the decode that made it expensive.
//
// A megabyte of wire is only a megabyte of memory when the values inside it are
// large. This is the bound for the opposite case.
//
// "Value" counts scalars, object keys AND opening containers, because the
// containers are exactly what gets allocated. A first version of this scan
// counted only scalars and was therefore blind to the cheapest amplification
// available: a body that is nothing but delimiters.
//
// Sized by MARGIN, not by observation. No live RewardList response has been
// captured (acceptance is PENDING) and the donor's model covers only the
// watchStreakMilestone subtree, so the value count of a real document is
// unverified; the limit is expected to sit far above it, and that expectation
// is an assumption this comment does not upgrade to a fact. It is set,
// deliberately, above the milestone collection limit, so a merely implausible
// collection is still decoded and refused with the precise
// OVERSIZED_COLLECTION evidence rather than being rejected here as an
// unreadable body.
const maxDiagnosticJSONValues = 8192

// diagnosticJSONValueVerdict is what diagnosticJSONValueCount answers. The
// over-limit case is split in two because the SIZE question and the SHAPE
// question have different owners, and collapsing them cost the bound its teeth
// once already: reporting "within limit" for an over-limit malformed body let
// the next pass walk the whole document.
type diagnosticJSONValueVerdict int

const (
	// diagnosticJSONWithinLimit: few enough values to decode.
	diagnosticJSONWithinLimit diagnosticJSONValueVerdict = iota
	// diagnosticJSONOverLimit: too many values, in a body that is otherwise
	// well formed. "Too large" is the honest class.
	diagnosticJSONOverLimit
	// diagnosticJSONOverLimitMalformed: too many values AND not well formed.
	// There is no honest size verdict to give - the decoder owns the shape one
	// - but the resource bound still applies, so no caller may run another
	// allocating pass over this body.
	diagnosticJSONOverLimitMalformed
)

// diagnosticJSONValueCount classifies body by how many JSON values it carries.
//
// It streams tokens and stops at the limit, so it holds only the decoder's
// nesting stack and never the document: the check cannot become the
// amplification it exists to prevent.
//
// Malformed JSON is NOT this function's business to CLASSIFY. Saying "too
// large" about a body that is merely broken would be a fabricated reason - and,
// worse, one a peer could choose, by moving its syntax error to either side of
// the limit boundary. But declining to name the class is not the same as
// declining to bound the resource, and an earlier version conflated the two:
// it answered a plain "within limit" for an over-limit malformed body, and the
// duplicate-member scan that runs next then boxed the entire payload. Measured:
// a 1,048,560-byte dense array with one trailing invalid byte allocated 55.2 MB
// - the exact cost this scan exists to avoid, reachable by appending a byte.
func diagnosticJSONValueCount(body []byte) diagnosticJSONValueVerdict {
	decoder := json.NewDecoder(bytes.NewReader(body))

	// UseNumber is what keeps this bound from being switchable off. Without it
	// Token converts every number to a float64, so a single unrepresentable one
	// - 1e10000 - fails the scan; and because a scan failure means "malformed,
	// not my problem", the bound then applied to NOTHING after that point. A
	// 240 KB body, well inside the byte cap, was reported within limit and cost
	// 22 MB to refuse. json.Number keeps the token a string, so counting
	// survives values the decoder could never represent, and the size question
	// stays separate from the representability one.
	decoder.UseNumber()

	values := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			// Ran out of readable tokens before the limit: too small to matter,
			// whatever its shape. The decoder reports the shape.
			return diagnosticJSONWithinLimit
		}
		// An OPENING delimiter is a value: it is the map or slice
		// encoding/json will allocate. Counting only scalars leaves the
		// cheapest amplification of all uncounted, because a container-heavy
		// body - [{},{},{},...] - is nothing BUT delimiters. Measured before
		// this line existed: 349,496 empty objects inside a just-under-1 MiB
		// body scanned as zero values and still cost ~88 MB to decode.
		//
		// Closing delimiters are skipped so a container is counted once, not
		// twice.
		if delimiter, isDelimiter := token.(json.Delim); isDelimiter {
			if delimiter != '{' && delimiter != '[' {
				continue
			}
		}
		values++
		if values > maxDiagnosticJSONValues {
			// Over the limit on the tokens seen so far - but "too large" is
			// only the honest answer for a body that is otherwise WELL FORMED.
			// A malformed one has no size verdict to give, only a shape one,
			// and the decoder that follows owns that; answering "too large"
			// here would let a peer choose between the two classes by moving
			// its syntax error to either side of this boundary.
			//
			// json.Valid is the right way to settle that and the reason this
			// is not simply "keep scanning": finishing the token loop boxes
			// every value it reads, which measured 54 MB on a 1 MiB body -
			// reintroducing the cost this scan exists to avoid. json.Valid
			// walks the bytes without materialising anything.
			if json.Valid(body) {
				return diagnosticJSONOverLimit
			}
			return diagnosticJSONOverLimitMalformed
		}
	}
}

// diagnosticJSONHasDuplicateMembers reports whether body carries duplicate
// object members, and ONLY that.
//
// jsonObjectKeysAreUnique cannot answer this question, because it folds scanner
// errors into its "not unique" answer: it returns false for a truncated body,
// for trailing bytes after the document, and for anything else the decoder
// dislikes. That is the right shape for its own callers, which want a single
// "is this body trustworthy" verdict. It is the wrong shape here, where the
// answer becomes a named failure class: malformed input reported as
// AMBIGUOUS_JSON would tell an operator that Twitch sent duplicate members
// when it sent no such thing, and would let a peer choose the recorded class
// with syntax alone.
//
// Malformed input is therefore left to the decoder that follows, which reports
// it honestly.
func diagnosticJSONHasDuplicateMembers(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	duplicate, err := scanJSONValueForDuplicateKeys(decoder)
	if err != nil {
		return false
	}
	if !duplicate {
		return false
	}

	// A duplicate found does not yet mean a duplicate REPORTED. The scan stops
	// at the first repeated key, so it has seen only a prefix - and a body
	// whose remainder the decoder cannot read is a shape problem, whatever its
	// prefix contained. Reporting "duplicate members" for it would let a peer
	// choose between this class and the transport class by moving the bad part
	// to either side of the repeated key.
	//
	// The test is DECODABILITY, not syntax, and the difference is reachable:
	// json.Valid accepts 1e10000 as a well-formed number while the decode that
	// follows fails on it, because interface{} decoding puts numbers in a
	// float64. Syntax is the wrong question here - the operative one is
	// whether the value the rest of this read works with can exist at all.
	//
	// Affordable precisely here: the value bound above has already passed, so
	// this document is at most maxDiagnosticJSONValues values. The value scan
	// itself cannot do this, because it runs before any bound is established
	// and so can only afford json.Valid.
	var decoded interface{}
	return json.Unmarshal(body, &decoded) == nil
}

// diagnosticPersistedQueryNotFound reports whether body is a STRUCTURED
// PersistedQueryNotFound rejection.
//
// gql.IsPersistedQueryNotFound answers with bytes.Contains over the whole body.
// For a business read that is defensible: the operations are ours and their
// responses are not attacker-shaped text. For a diagnostic read it is not, and
// the consequence is not a misclassification but a destroyed observation - any
// valid response with the marker in an unrelated string is resent under every
// candidate client ID and then reported as UNSUPPORTED_QUERY, discarding the
// real milestone data it carried.
//
// So the marker must be where a rejection actually puts it: a top-level errors
// array, every element an object carrying the APQ marker, with no non-null data
// member beside it (an empty data object is PRESENT, and refuses the reading;
// an explicit null is absent, as everywhere else in this read). Both attested
// spellings count - "message" and the extensions code
// - because a genuine PersistedQueryNotFound is the EXPECTED steady state for
// this operation, and failing to recognise one would replace an honest
// UNSUPPORTED_QUERY with GRAPHQL_TOP_LEVEL_ERRORS after a single dispatch: the
// unrecognised body still carries a non-empty errors array, which the reader
// refuses before it ever looks for a data node, so the stale shipped hash
// would hide among ordinary service errors.
//
// Deliberately not strictPersistedQueryNotFound: that one authorizes a mutation
// REPLAY and demands the extensions code specifically, so it would answer false
// for the message-only spelling this read must still recognise.
func diagnosticPersistedQueryNotFound(body []byte) bool {
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return false
	}
	if data, present := result["data"]; present && data != nil {
		return false
	}
	// A top-level "error" is an explicit rejection in its own right, and it
	// sits BESIDE the errors array rather than inside it - so an APQ-shaped
	// array can be presented alongside one and would otherwise be believed,
	// concealing the real rejection.
	//
	// An explicit null is ABSENT, not a rejection, which is the rule this read
	// applies to every other presence question and the rule SPECIFICATIONS.md
	// states for it. Matching strictPersistedQueryNotFound's blunter "any
	// present key" test here was wrong in the one direction that costs
	// evidence: {"error":null,"errors":[{"message":"PersistedQueryNotFound"}]}
	// was refused as APQ and recorded GRAPHQL_TOP_LEVEL_ERRORS, so a null the
	// peer costs nothing to add would hide the stale shipped hash this
	// operation exists to detect. That mutation-replay detector can afford to
	// be blunter; this one cannot.
	if raw, present := result["error"]; present && raw != nil {
		return false
	}

	list, ok := result["errors"].([]interface{})
	if !ok || len(list) == 0 {
		return false
	}
	for _, raw := range list {
		object, ok := raw.(map[string]interface{})
		if !ok {
			return false
		}

		// The two spellings are ALTERNATIVE evidence, not independent ones.
		// Where both are present the code decides, because it is the machine-
		// readable field and the message is prose beside it: an error reading
		// {"message":"PersistedQueryNotFound","extensions":{"code":"UNAUTHORIZED"}}
		// is an authorization rejection wearing an APQ message, and taking the
		// message on its own would replay the authenticated request under every
		// client ID and record the real rejection as UNSUPPORTED_QUERY.
		// Presence and SHAPE are separate questions. A single type assertion
		// answers both at once and therefore answers neither: it reports false
		// for an absent extensions member and for a present one that is a
		// string, so malformed rejection metadata reads as no metadata and
		// falls through to the message.
		//
		// An explicit null is treated as absent rather than malformed, because
		// it says "no extensions object" and refusing it would risk missing a
		// genuine PersistedQueryNotFound - the one direction where being wrong
		// hides a stale shipped hash instead of merely refusing a response.
		if rawExtensions, present := object["extensions"]; present && rawExtensions != nil {
			extensions, isObject := rawExtensions.(map[string]interface{})
			if !isObject {
				return false
			}
			//
			// The same null-is-absent rule applies to the code itself: an
			// explicit "code": null says "no code", not "a contradicting code",
			// so it falls through to the message exactly as a null extensions
			// object does. Refusing it cost the same evidence in the same
			// direction: a genuine rejection wearing a null code was recorded
			// GRAPHQL_TOP_LEVEL_ERRORS after one dispatch.
			if code, carried := extensions["code"]; carried && code != nil {
				if code != "PERSISTED_QUERY_NOT_FOUND" {
					return false
				}
				continue
			}
		}

		// No code to consult: the message is the only evidence there is.
		if message, _ := object["message"].(string); message != "PersistedQueryNotFound" {
			return false
		}
	}
	return true
}

// jsonObjectKeysAreUnique validates the raw JSON token stream before decoding
// into maps. encoding/json otherwise applies last-value-wins to duplicate
// members, which is unsafe proof for a side-effect retry: a conflicting
// successful `data` member followed by `data:null` (or a duplicate nested code)
// must invalidate PQNF authority rather than disappear during unmarshal.
func jsonObjectKeysAreUnique(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	duplicate, err := scanJSONValueForDuplicateKeys(decoder)
	if err != nil || duplicate {
		return false
	}
	_, err = decoder.Token()
	return errors.Is(err, io.EOF)
}

func scanJSONValueForDuplicateKeys(decoder *json.Decoder) (bool, error) {
	token, err := decoder.Token()
	if err != nil {
		return false, err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return false, nil
	}

	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return false, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return false, errors.New("JSON object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return true, nil
			}
			seen[key] = struct{}{}
			if duplicate, err := scanJSONValueForDuplicateKeys(decoder); err != nil || duplicate {
				return duplicate, err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return false, fmt.Errorf("invalid JSON object closing token: %v", err)
		}
	case '[':
		for decoder.More() {
			if duplicate, err := scanJSONValueForDuplicateKeys(decoder); err != nil || duplicate {
				return duplicate, err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return false, fmt.Errorf("invalid JSON array closing token: %v", err)
		}
	default:
		return false, fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return false, nil
}

// noRedirectClient returns a request-local copy of the shared HTTP client that
// refuses to follow redirects. The copy shares the transport and the timeout;
// only the copy's redirect policy changes, so the shared client - and every
// caller that follows redirects - is untouched. Both the side-effect
// mutations and the diagnostic read dispatch through it.
func (c *TwitchClient) noRedirectClient() *http.Client {
	client := *c.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client
}

func (c *TwitchClient) doGQLMutationOnce(body []byte, operationName, clientID, token string) ([]byte, int, bool, error) {
	req, err := http.NewRequest(http.MethodPost, c.gqlURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, false, fmt.Errorf("failed to create request: %w", err)
	}
	c.setGQLHeaders(req, clientID, token)

	respBody, statusCode, _, err := doGQLOnceWithClient(c.noRedirectClient(), req)
	if err != nil {
		return respBody, statusCode, true, err
	}
	slog.Debug("GQL response", "operation", operationName, "status", statusCode)
	return respBody, statusCode, true, nil
}

// authRecoveryWait bounds how long a rejected request waits for the shared
// auth recovery before giving up with ErrUnauthorized. A refresh completes
// well within it; a device-flow recovery (minutes, needs the user) keeps
// running in its owner — this caller just stops waiting, and later requests
// pick up the rotated credentials once published.
const authRecoveryWait = 60 * time.Second

// isTransientRecoveryFailure reports whether a recovery error proves nothing
// about the credentials being unrecoverable: a transient auth-endpoint
// failure, an INCONCLUSIVE outcome (undocumented refresh 400 — fail-closed,
// retryable), a backoff refusal (the auth layer's per-generation retry gate
// pacing sequential attempts — retryable by definition, zero traffic), or
// this caller's own bounded wait expiring while the shared recovery keeps
// running (device flow needs the user and takes minutes). None of these may
// escalate the operator reauth path. A definitive protocol failure (e.g. a
// malformed refresh success, which consumed the one-time refresh token) is
// NOT in this set and escalates.
func isTransientRecoveryFailure(err error) bool {
	return errors.Is(err, auth.ErrAuthTransient) ||
		errors.Is(err, auth.ErrRecoveryInconclusive) ||
		errors.Is(err, auth.ErrRecoveryBackoff) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}

// recoverAuth funnels an authoritative rejection of the given credential
// generation into the auth layer's single-flight recovery and returns the
// rotated snapshot to replay with.
// The ctx bounds THIS caller's wait only. The shared single-flight recovery
// itself runs on the auth layer's own lifecycle context (internal/auth.Recover:
// "the flight runs on the LIFECYCLE context, detached from every caller's
// context"), so a cancelled watch generation stops waiting without aborting a
// token refresh or device flow the rest of the process depends on.
func (c *TwitchClient) recoverAuth(ctx context.Context, rejectedGeneration uint64) (auth.Snapshot, error) {
	if c.recoverFn != nil {
		return c.recoverFn(rejectedGeneration)
	}
	waitCtx, cancel := context.WithTimeout(ctx, authRecoveryWait)
	defer cancel()
	return c.auth.Recover(waitCtx, rejectedGeneration)
}

// gqlBatchRoundTrip is the batch twin of gqlSingleRoundTrip: one complete
// batch GQL cycle signed with the given token, reporting an authoritative auth
// rejection (HTTP 401 or a documented Unauthorized body on any entry).
func (c *TwitchClient) gqlBatchRoundTrip(ctx context.Context, body []byte, label, token string) (result []map[string]interface{}, authRejected bool, err error) {
	respBody, statusCode, err := c.doGQLRequestWithClientIDFallback(ctx, body, label, token)
	if err != nil {
		return nil, false, err
	}

	// Status checks come BEFORE the unmarshal: a real 401/403 carries an
	// OBJECT error body that cannot unmarshal into the batch's []map shape —
	// checking after parsing would make these branches unreachable.
	if statusCode == http.StatusUnauthorized {
		return nil, true, nil
	}
	if statusCode == http.StatusForbidden {
		c.connAcct.markFunctionalFailure(time.Now())
		return nil, false, fmt.Errorf("twitch GQL %s: permission denied (status 403)", label)
	}

	if len(bytes.TrimSpace(respBody)) == 0 {
		return nil, false, fmt.Errorf("twitch GQL %s: empty response body", label)
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, false, fmt.Errorf("failed to unmarshal %s response: %w", label, err)
	}

	for _, item := range result {
		if isAuthError(statusCode, item) {
			return result, true, nil
		}
	}
	return result, false, nil
}

func (c *TwitchClient) postGQLBatchRequest(ctx context.Context, operations []constants.GQLOperation) ([]map[string]interface{}, error) {
	for _, operation := range operations {
		if isBonusMutationOperation(operation) {
			return nil, ErrDirectBonusMutation
		}
	}
	body, err := json.Marshal(operations)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal operations: %w", err)
	}

	names := make([]string, len(operations))
	for i, op := range operations {
		names[i] = op.OperationName
	}
	label := strings.Join(names, ",")

	// Same snapshot/recover/replay-once contract as postGQLRequest, applied to
	// the whole batch (the batch is one HTTP request and is replayed as one).
	snap := c.auth.Snapshot()
	result, authRejected, err := c.gqlBatchRoundTrip(ctx, body, label, snap.AccessToken)
	if err != nil {
		return nil, err
	}

	if authRejected {
		newSnap, rerr := c.recoverAuth(ctx, snap.Generation)
		if rerr != nil {
			if !isTransientRecoveryFailure(rerr) {
				c.handleUnauthorized()
			}
			return nil, fmt.Errorf("%w: batch %s", ErrUnauthorized, label)
		}
		result, authRejected, err = c.gqlBatchRoundTrip(ctx, body, label, newSnap.AccessToken)
		if err != nil {
			return nil, err
		}
		if authRejected {
			c.handleUnauthorized()
			return nil, fmt.Errorf("%w: batch %s", ErrUnauthorized, label)
		}
	}

	// Same GQL-layer-error gate as postGQLRequest: a batch entry carrying a
	// top-level "errors" array returned no authoritative data and must not
	// refresh the connection-health timestamp.
	for _, item := range result {
		if gql.HasTopLevelErrors(item) {
			c.connAcct.markFunctionalFailure(time.Now())
			return result, nil
		}
	}

	c.markSuccess()
	return result, nil
}

// candidateClientIDs returns the ordered client IDs to try for operation, most
// likely to work first: the operation's cached known-good ID (if any), then the
// promoted global default, then the remaining public IDs from
// constants.GQLClientIDFallbacks — de-duplicated. In steady state the first
// candidate already works, so no fallback requests are made.
func (c *TwitchClient) candidateClientIDs(operation string) []string {
	c.clientIDMu.RLock()
	cached := c.opClientID[operation]
	def := c.defaultClientID
	c.clientIDMu.RUnlock()

	out := make([]string, 0, len(constants.GQLClientIDFallbacks)+2)
	seen := make(map[string]bool)
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	add(cached)
	add(def)
	for _, id := range constants.GQLClientIDFallbacks {
		add(id)
	}
	return out
}

// rememberWorkingClientID records clientID as the known-good ID for operation.
// viaFallback is true when clientID was not the first candidate tried (i.e. an
// earlier candidate returned PersistedQueryNotFound and this one resolved it).
//
// The global default is promoted only when a fallback actually rotates it, and
// promoted is computed under the lock — so when many goroutines recover
// concurrently exactly one promotes and logs the WARN; the rest observe the
// already-promoted default and stay quiet. A steady state (each operation's
// cached ID works on the first try) never logs and never churns the default, so
// there is no log-spam loop. Safe for concurrent callers.
func (c *TwitchClient) rememberWorkingClientID(operation, clientID string, viaFallback bool) {
	c.clientIDMu.Lock()
	if c.opClientID == nil {
		c.opClientID = make(map[string]string)
	}
	c.opClientID[operation] = clientID
	prevDefault := c.defaultClientID
	promoted := viaFallback && clientID != prevDefault
	if promoted {
		c.defaultClientID = clientID
	}
	c.clientIDMu.Unlock()

	switch {
	case promoted:
		// The moment the global default rotates: log once at WARN so the
		// operator knows the shipped hashes are stale and should be updated.
		slog.Warn("GQL PersistedQueryNotFound resolved by switching Twitch client ID; the persisted-query hashes in internal/constants/gql.go are likely stale and should be updated",
			"operation", operation,
			"workingClientID", clientID,
			"previousDefault", prevDefault,
		)
	case viaFallback:
		// Recovered onto the already-promoted default — the rotation was already
		// logged once at WARN; keep the follow-ups at DEBUG so one rotation never
		// becomes a WARN burst.
		slog.Debug("GQL operation recovered on the promoted fallback client ID",
			"operation", operation,
			"clientID", clientID,
		)
	}
}

// doGQLRequestWithClientIDFallback sends a GQL request and, on a
// PersistedQueryNotFound response, transparently retries with the alternate
// public Twitch client IDs before giving up. This guards against the
// well-known failure where Twitch rotates or invalidates the persisted-query
// hashes tied to a hardcoded client ID, which would otherwise break every GQL
// call at once. Transient network/HTTP failures are handled one layer down by
// doGQLRequestWithRetry — this layer deals only with the stale
// client-ID/query-hash case.
//
// Candidate order is per-operation (candidateClientIDs): the operation's cached
// known-good ID first, then the promoted default, then the rest. On success a
// BUSINESS read caches the working ID for the operation
// (rememberWorkingClientID); a DIAGNOSTIC read caches nothing at all, so it
// starts from the current process-wide default - the shipped one until a
// BUSINESS rotation promotes another - followed by the remaining shipped
// candidates, and it never moves that default itself. When every
// candidate returns PersistedQueryNotFound the request has genuinely failed
// because the hash itself is stale — one ERROR is logged for a business read (a
// diagnostic read logs the same summary at DEBUG, see logStaleHashExhausted) and
// ErrPersistedQueryNotFound is returned so the caller keeps its last-known state
// instead of parsing an error body as "no data".
func (c *TwitchClient) doGQLRequestWithClientIDFallback(ctx context.Context, body []byte, operationLabel, token string) ([]byte, int, error) {
	dr := diagnosticRequestOf(ctx)
	diagnostic := dr != nil
	if !diagnostic {
		c.connAcct.markAttempt(time.Now())
	}
	candidates := c.candidateClientIDs(operationLabel)

	var (
		respBody   []byte
		statusCode int
		err        error
	)

	for i, clientID := range candidates {
		// A diagnostic read asks - WITHOUT spending anything - whether the
		// cycle can still afford another candidate before it starts one. When
		// it cannot, the traversal ends here as INCOMPLETE. That is not the
		// same fact as the exhausted-hash return after the loop: that one says
		// every candidate answered PersistedQueryNotFound, and a traversal cut
		// short by the allowance has not established it. UNSUPPORTED_QUERY
		// stays reachable only through the full walk.
		//
		// The owner is consulted FIRST, as on the retry path: a cancellation
		// or deadline that lands between one candidate's answer and the next
		// must read CANCELLED, never as an allowance stop.
		if diagnostic {
			if cerr := ctx.Err(); cerr != nil {
				return nil, 0, cerr
			}
			if dr.allowance != nil && dr.allowance.Remaining() == 0 {
				return nil, 0, fmt.Errorf("%w: after %d of %d client IDs",
					errDiagnosticAllowanceExhausted, i, len(candidates))
			}
		}

		respBody, statusCode, err = c.doGQLRequestWithRetry(ctx, body, operationLabel, clientID, token)
		if err != nil {
			return respBody, statusCode, err
		}

		// A DIAGNOSTIC read stops here on any non-2xx, before the body is
		// treated as evidence of anything.
		//
		// The structured APQ detector this path uses
		// (diagnosticPersistedQueryNotFound, below) reads the body and does
		// not consult the status, so a rejected response whose body is SHAPED
		// like a structured rejection would otherwise drive the candidate
		// loop: the authenticated request would be re-sent under every client
		// ID this project ships, and an endpoint answering each with the same
		// shape would end the read as UNSUPPORTED_QUERY - amplifying the
		// request threefold and naming the wrong cause. The status is the
		// authority on whether a body is worth reading at all.
		//
		// The BODY is dropped, not just the iteration stopped. An earlier
		// version handed it back "unchanged, so gqlSingleRoundTrip keeps
		// deciding 401 and 403" - but neither of those branches reads the body,
		// so nothing needed it, and returning it walked the response straight
		// past the pre-decode value bound (the diagnosticJSONValueCount switch
		// below) into json.Unmarshal. Measured on one 1,048,567-byte dense array: 44.9 MB
		// at HTTP 404 against 3.2 MB for the identical body at HTTP 200 - a
		// 14x amplification on the bonus poll goroutine, selected purely by the
		// peer's status code, in the one place this feature's stated order
		// (bound the resource, establish the shape, then interpret) had no
		// bound at all.
		//
		// Nothing downstream loses evidence: 401 and 403 are settled on the
		// status alone, the resulting "empty response body" transport error
		// carries no text worth keeping, and ObserveWatchStreakMilestone
		// refines it back to HTTP_STATUS. A non-2xx is not a working client ID,
		// so nothing is cached for it either. Transient statuses never arrive
		// here - doGQLRequestWithRetry has already exhausted them into an error
		// above.
		if diagnostic {
			if statusCode < 200 || statusCode > 299 {
				return nil, statusCode, nil
			}

			// Duplicate JSON members are ambiguous evidence. encoding/json
			// keeps the LAST value, so a response that carries an explicit
			// rejection followed by a benign duplicate - say an "errors" array
			// of real errors followed by an empty one - decodes with the
			// rejection erased and would be recorded as a clean observation.
			// The mutation path already refuses raw bodies like this
			// (jsonObjectKeysAreUnique); an observation whose whole purpose is
			// honest evidence has the same need, and the fail-closed checks
			// downstream cannot help, because they only ever see the lossy
			// decoded map.
			//
			// This runs BEFORE the structured APQ detector, not after, and the
			// order is the whole point: that detector decodes the body, and
			// encoding/json keeps the LAST duplicate member, so a benign errors
			// array followed by an APQ-shaped one would present as an
			// authoritative rejection, drive the candidate loop under every
			// client ID and end as UNSUPPORTED_QUERY - while the reverse member
			// order would erase a real rejection into NO_DATA_NODE - never
			// reaching this refusal at all. The peer would pick the recorded
			// class by member ORDER. An ambiguous body is not evidence of
			// anything, the marker included.
			// Size first: this scan is what keeps the two below - and the
			// decode after them - from being handed a document that makes them
			// expensive. jsonObjectKeysAreUnique builds a key set per object,
			// so it is one of the things being protected, not a peer.
			switch diagnosticJSONValueCount(respBody) {
			case diagnosticJSONOverLimit:
				return nil, statusCode, fmt.Errorf(
					"%w: operation %s", errOversizedDiagnosticJSON, operationLabel)

			case diagnosticJSONOverLimitMalformed:
				// Over the limit, and broken. It keeps its malformed class -
				// the decoder below names that, and a peer must not be able to
				// pick between the two classes with a syntax error - but the
				// duplicate scan is SKIPPED, because that scan boxes every
				// value it reads and this body is precisely the one too large
				// to hand it. Nothing is lost by skipping it: a body that does
				// not decode cannot be recorded as an observation either way.
				// The two json.Unmarshal calls that still follow - the
				// structured APQ detector's and gqlSingleRoundTrip's - are
				// safe because encoding/json validates the whole input before
				// allocating anything, so a malformed body fails without
				// materialising; TestAMalformedOversizedBodyIsStillBounded
				// pins that cost, so a decoder that does not share the
				// property cannot be substituted silently.

			default:
				if diagnosticJSONHasDuplicateMembers(respBody) {
					return nil, statusCode, fmt.Errorf(
						"%w: operation %s", errAmbiguousDiagnosticJSON, operationLabel)
				}
			}
		}

		// The two paths ask the same question of different evidence: a business
		// read trusts the marker anywhere in its own operation's response, a
		// diagnostic read requires it where a rejection actually puts it. See
		// diagnosticPersistedQueryNotFound.
		queryNotFound := gql.IsPersistedQueryNotFound(respBody)
		if diagnostic {
			queryNotFound = diagnosticPersistedQueryNotFound(respBody)
		}

		if !queryNotFound {
			// A diagnostic read pins NOTHING - not the process-wide default,
			// and not the per-operation candidate either.
			//
			// It used to pin the per-operation candidate, guarded by a predicate
			// that tried to decide HERE whether the reader would accept the
			// response. Every version of that predicate was an incomplete
			// restatement of the reader's rules, and three consecutive review
			// rounds each found a body it admitted and the reader then rejected:
			// an explicit service rejection, a non-object data node, and an
			// oversized collection. Because candidateClientIDs puts the cached
			// ID FIRST and any non-APQ response ends this loop, each of those
			// pinned a candidate permanently and the shipped default was never
			// retried.
			//
			// Predicting one layer's verdict in another is the defect, not any
			// one of those shapes, so the prediction is gone rather than
			// extended a fourth time. What it bought was at most one saved
			// request per target per cycle, and only in a state - the shipped
			// default failing while a fallback serves - that RewardList has
			// never been observed in, its acceptance being PENDING. In the
			// state it IS expected to be in, every candidate answers
			// PersistedQueryNotFound and nothing was ever cached anyway.
			//
			// This also makes the code match what this feature claims: it owns
			// no cache. A BUSINESS read is untouched and still caches and
			// promotes exactly as before.
			if !diagnostic {
				c.rememberWorkingClientID(operationLabel, clientID, i > 0)
			}
			return respBody, statusCode, nil
		}

		// A diagnostic read of an operation whose live acceptance is not
		// established fails this way BY DESIGN, for every candidate, on every
		// cycle. At WARN/ERROR it would be an unactionable operator alert; even
		// at DEBUG, one line per candidate per target per cycle is volume the
		// retained log does not need, and the exhausted summary below already
		// reports how many candidates were tried. A business read keeps the
		// full per-candidate WARN, where the specific client ID is actionable.
		if !diagnostic {
			slog.Warn("GQL request returned PersistedQueryNotFound; trying next client ID",
				"operation", operationLabel,
				"clientID", clientID,
				"remainingCandidates", len(candidates)-i-1,
			)
		}
	}

	logStaleHashExhausted(diagnostic, operationLabel, len(candidates))

	if !diagnostic {
		c.connAcct.markFunctionalFailure(time.Now())
	}
	return respBody, statusCode, fmt.Errorf("%w: operation %s (tried %d client IDs)", ErrPersistedQueryNotFound, operationLabel, len(candidates))
}

// logStaleHashExhausted reports that every candidate client ID returned
// PersistedQueryNotFound. For a business read this is the operator's signal to
// refresh the hash in internal/constants/gql.go; for a diagnostic read it is an
// expected outcome the caller records itself.
func logStaleHashExhausted(diagnostic bool, operationLabel string, clientIDsTried int) {
	if diagnostic {
		slog.Debug("Diagnostic GQL request returned PersistedQueryNotFound on all known client IDs",
			"operation", operationLabel,
			"clientIDsTried", clientIDsTried,
		)
		return
	}
	slog.Error("GQL request returned PersistedQueryNotFound on all known client IDs; persisted-query hashes are stale and need updating in internal/constants/gql.go",
		"operation", operationLabel,
		"clientIDsTried", clientIDsTried,
	)
}

// doGQLRequestWithRetry sends the given already-marshaled GQL request body
// using the supplied client ID, retrying with exponential backoff on transient
// failures: network-level errors (timeouts, connection resets) and HTTP 429/5xx
// responses. A 429's Retry-After header, when present, is honored in place of
// the computed backoff (see gql.RetryWait). Other HTTP errors (4xx auth/logic
// errors) are returned immediately since retrying them would just reproduce the
// same failure. A successful response never incurs a wait.
func (c *TwitchClient) doGQLRequestWithRetry(ctx context.Context, body []byte, operationLabel, clientID, token string) ([]byte, int, error) {
	var lastErr error
	dr := diagnosticRequestOf(ctx)
	diagnostic := dr != nil

	for attempt := 0; attempt <= gqlMaxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", c.gqlURL, bytes.NewReader(body))
		if err != nil {
			return nil, 0, fmt.Errorf("failed to create request: %w", err)
		}
		c.setGQLHeaders(req, clientID, token)

		var (
			respBody   []byte
			statusCode int
			retryAfter time.Duration
		)
		if !diagnostic {
			respBody, statusCode, retryAfter, err = c.doGQLOnce(req)
		} else {
			// The permit is charged IMMEDIATELY before the dispatch and in the
			// same breath as the dispatch count, so spent == dispatches by
			// construction; a charge refused here means no request is made.
			// A dead owner is checked first so a permit is not spent on a
			// Do that would refuse on its own context without dialling.
			if cerr := ctx.Err(); cerr != nil {
				return nil, 0, cerr
			}
			if dr.allowance != nil && !dr.allowance.charge() {
				return nil, 0, errDiagnosticAllowanceExhausted
			}
			dr.dispatches++

			// A diagnostic read never follows a redirect. The shared client
			// follows up to ten, each one another authenticated request that
			// no permit above would have counted and that the byte cap cannot
			// see. noRedirectClient is request-local and made at dispatch
			// time, as doGQLMutationOnce already does, so the shared client's
			// policy - and every business read's - is untouched. The 3xx then
			// comes back as an ordinary non-2xx and is refused on its status.
			respBody, statusCode, retryAfter, err = doGQLOnceWithClient(c.noRedirectClient(), req)
		}
		if err == nil {
			// A diagnostic read repeats for every target on every cycle, so its
			// per-attempt trace is pure volume in the retained log; the caller
			// records the outcome as its own structured fact instead. A
			// business read keeps the trace.
			if !diagnostic {
				slog.Debug("GQL response", "operation", operationLabel, "status", statusCode)
			}
			return respBody, statusCode, nil
		}

		lastErr = err

		transient := statusCode == 0 || gql.IsTransientStatus(statusCode)
		if !transient {
			return nil, statusCode, err
		}

		if attempt == gqlMaxRetries {
			break
		}

		if diagnostic {
			// Order matters and is the whole point of these two lines. The
			// owner's own cancellation is checked FIRST: an owner deadline or
			// SIGTERM that lands on the last permitted dispatch must be
			// recorded as CANCELLED, not as an allowance stop, and no switch
			// order downstream can recover that once the wrong sentinel has
			// been returned. Only then is the allowance asked - WITHOUT
			// spending - whether a retry is affordable at all; when it is not,
			// waiting out the backoff would be a wait for a request that can
			// never be sent.
			if cerr := ctx.Err(); cerr != nil {
				return nil, statusCode, cerr
			}
			if dr.allowance != nil && dr.allowance.Remaining() == 0 {
				return nil, statusCode, errDiagnosticAllowanceExhausted
			}
		}

		wait, via := gql.RetryWait(attempt, retryAfter)
		retryLog := slog.Warn
		retryAttrs := []any{
			"operation", operationLabel,
			"attempt", attempt + 1,
			"maxAttempts", gqlMaxRetries + 1,
			"waitSeconds", wait.Seconds(),
			"nextRetryVia", via,
			"status", statusCode,
		}
		if diagnostic {
			// Lowering the level is not enough on its own: FileLevel defaults
			// to DEBUG, so this record still reaches the retained log. The
			// transport error is an arbitrary, unbounded string that a hostile
			// redirect or proxy can shape - exactly what a diagnostic read
			// promises never to write. The bounded status is what makes a retry
			// diagnosable; the raw text adds nothing a class does not.
			retryLog = slog.Debug
		} else {
			retryAttrs = append(retryAttrs, "error", lastErr)
		}
		retryLog("GQL request failed, retrying", retryAttrs...)

		// The backoff belongs to the request, so it belongs to whoever owns the
		// request. gql.RetryWait still computes the same jittered delay; only
		// the waiting is interruptible now, so a cancelled owner stops instead
		// of sleeping out the rest of the schedule.
		if werr := waitForRetry(ctx, wait); werr != nil {
			return nil, statusCode, werr
		}
	}

	if !diagnostic {
		c.gqlFailures.mark(time.Now())
		slog.Error("GQL request exhausted all retries, skipping this cycle",
			"operation", operationLabel,
			"attempts", gqlMaxRetries+1,
			"error", lastErr,
		)
	} else {
		slog.Debug("Diagnostic GQL request exhausted all retries",
			"operation", operationLabel,
			"attempts", gqlMaxRetries+1,
		)
	}

	return nil, 0, fmt.Errorf("gql request failed after %d attempts: %w", gqlMaxRetries+1, lastErr)
}

// waitForRetry pauses for the computed backoff, interruptible by ctx. It
// returns ctx.Err() when the owner is cancelled mid-wait, so an abandoned
// request stops retrying instead of finishing its schedule on behalf of a dead
// generation. A background-owned caller (every non-watch subsystem) never
// observes the Done branch.
func waitForRetry(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// maxDiagnosticResponseBytes is the largest response body a DIAGNOSTIC read will
// accept. Chosen orders of magnitude above a real RewardList response so it
// cannot refuse one, and far below anything that would matter to the process.
//
// It is a limit, not a truncation point: a body past it is refused whole. The
// read pulls one byte more than the limit precisely so overflow is DETECTABLE,
// because io.LimitReader reports a truncated stream and a complete one the same
// way - both end in a clean EOF. Truncating instead would let a hostile endpoint
// send a complete document ending exactly at the limit followed by arbitrary
// bytes: the prefix would decode and be recorded as a whole observation of a
// response that was never read whole.
const maxDiagnosticResponseBytes = 1 << 20 // 1 MiB

// doGQLOnce performs a single HTTP round trip. It returns the response body on
// success, or an error with the observed status code (0 for network-level
// errors, where no HTTP response was received at all) plus any Retry-After delay
// the server asked for (0 when absent), so the caller can honor a 429 hint.
func (c *TwitchClient) doGQLOnce(req *http.Request) ([]byte, int, time.Duration, error) {
	return doGQLOnceWithClient(c.client, req)
}

func doGQLOnceWithClient(client *http.Client, req *http.Request) ([]byte, int, time.Duration, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A diagnostic read caps what it will pull into memory. The status is only
	// known after the response arrives, so a hostile endpoint or proxy could
	// otherwise answer a rejected diagnostic with an unbounded body and have it
	// read in full — on the bonus poll goroutine — before anything classifies
	// it. Business reads stay unbounded as they were; this is a property of the
	// diagnostic caller, not a change to the shared contract.
	//
	// The limit is sized by margin, not by observation: no live RewardList
	// response has been captured (acceptance is PENDING), so its real size is
	// unverified and the limit is only EXPECTED to sit far above it. A body
	// that exceeds it is refused whole and reported as a transport failure,
	// which is the honest outcome for a response this read declines to trust;
	// if a legitimate response ever exceeded it, that channel would be refused
	// on every cycle and the record could not tell it from a hostile one.
	//
	// Reading limit+1 is what makes the overflow visible - see
	// maxDiagnosticResponseBytes. This is the same shape fetchSpadeAsset uses.
	diagnostic := isDiagnosticRequest(req.Context())
	body := io.Reader(resp.Body)
	if diagnostic {
		body = io.LimitReader(resp.Body, maxDiagnosticResponseBytes+1)
	}

	respBody, err := io.ReadAll(body)
	if err != nil {
		return nil, resp.StatusCode, 0, fmt.Errorf("failed to read response: %w", err)
	}
	// A transient status keeps its Retry-After even when the body is refused
	// below, so a capped 429 is retried on the peer's schedule rather than on
	// the computed backoff. Parsed once, before the size check, so the two
	// returns that carry it cannot drift apart.
	retryAfter := time.Duration(0)
	if gql.IsTransientStatus(resp.StatusCode) {
		retryAfter = gql.ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	}
	if diagnostic && len(respBody) > maxDiagnosticResponseBytes {
		return nil, resp.StatusCode, retryAfter, fmt.Errorf(
			"response body exceeded the %d-byte diagnostic limit", maxDiagnosticResponseBytes)
	}

	if gql.IsTransientStatus(resp.StatusCode) {
		return nil, resp.StatusCode, retryAfter, fmt.Errorf("transient GQL error: status %d", resp.StatusCode)
	}

	return respBody, resp.StatusCode, 0, nil
}

func (c *TwitchClient) setGQLHeaders(req *http.Request, clientID, token string) {
	req.Header.Set("Authorization", "OAuth "+token)
	req.Header.Set("Client-Id", clientID)
	req.Header.Set("Client-Session-Id", c.clientSession)
	req.Header.Set("Client-Version", c.getClientVersion())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("X-Device-Id", c.deviceID)
}

// ActiveClientID returns a human label for the promoted default GQL client ID
// ("TV", "Browser", "Mobile", or "Unknown"), for the Health Center. The default
// can change at runtime when doGQLRequestWithClientIDFallback promotes a working
// alternate after a PersistedQueryNotFound.
func (c *TwitchClient) ActiveClientID() string {
	c.clientIDMu.RLock()
	id := c.defaultClientID
	c.clientIDMu.RUnlock()

	switch id {
	case constants.ClientIDTV:
		return "TV"
	case constants.ClientIDBrowser:
		return "Browser"
	case constants.ClientIDMobile:
		return "Mobile"
	default:
		return "Unknown"
	}
}

func (c *TwitchClient) getClientVersion() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.clientVersion
}

func (c *TwitchClient) UpdateClientVersion() string {
	resp, err := c.client.Get(constants.TwitchURL)
	if err != nil {
		return c.getClientVersion()
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return c.getClientVersion()
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.getClientVersion()
	}

	matches := c.twilightBuildIDPattern.FindSubmatch(body)
	if len(matches) < 2 {
		return c.getClientVersion()
	}

	c.mu.Lock()
	c.clientVersion = string(matches[1])
	c.mu.Unlock()

	slog.Debug("Updated client version", "version", c.clientVersion)
	return c.clientVersion
}

func (c *TwitchClient) GetChannelID(username string) (string, error) {
	op := constants.GetIDFromLogin.WithVariables(map[string]interface{}{
		"login": strings.ToLower(username),
	})

	resp, err := c.postGQLRequest(context.Background(), op)
	if err != nil {
		return "", err
	}

	// A 200 carrying a top-level "errors" array (a non-PQNF service failure
	// such as "service timeout") returned no authoritative data. Mapping it to
	// ErrStreamerDoesNotExist below would tell callers — including the startup
	// fail-fast path, which treats "does not exist" as a config typo and exits —
	// that the login is missing when Twitch merely hiccuped.
	if gql.HasTopLevelErrors(resp) {
		return "", fmt.Errorf("twitch GQL error for %s: user lookup returned no data", op.OperationName)
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return "", ErrStreamerDoesNotExist
	}

	user, ok := data["user"].(map[string]interface{})
	if !ok || user == nil {
		return "", ErrStreamerDoesNotExist
	}

	id, ok := user["id"].(string)
	if !ok {
		return "", ErrStreamerDoesNotExist
	}

	return id, nil
}

// FollowedChannel is one channel the authenticated user follows, as returned by
// GetFollowedChannels. Only the login and display name are captured — no tokens,
// ids, or other account data.
type FollowedChannel struct {
	Login       string
	DisplayName string
}

const (
	// followedPageSize is the per-request page size for the ChannelFollows query.
	followedPageSize = 100
	// maxFollowedFetch caps how many followed channels GetFollowedChannels will
	// pull, so an account following thousands can't turn one import into an
	// unbounded paginated crawl. When the cap is hit with more still available,
	// the method reports truncated=true so the UI can say the list is partial.
	maxFollowedFetch = 1000
)

// GetFollowedChannels returns the channels the authenticated user follows
// (login + display name), paginated up to maxFollowedFetch. truncated is true
// when the cap was reached while Twitch still reported more pages, so the caller
// can surface "showing first N of more" instead of silently cutting the list.
func (c *TwitchClient) GetFollowedChannels() (channels []FollowedChannel, truncated bool, err error) {
	return collectFollowedChannels(func(cursor string) (map[string]interface{}, error) {
		vars := map[string]interface{}{"limit": followedPageSize, "order": "ASC"}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		return c.postGQLRequest(context.Background(), constants.ChannelFollows.WithVariables(vars))
	})
}

// collectFollowedChannels drives the ChannelFollows pagination: it calls fetch
// with the running cursor, parses each page, dedups logins, and stops at the end
// or the maxFollowedFetch cap (reporting truncated when the cap is hit with more
// available). The network is injected as fetch so the loop is unit-testable.
func collectFollowedChannels(fetch func(cursor string) (map[string]interface{}, error)) (channels []FollowedChannel, truncated bool, err error) {
	seen := make(map[string]bool)
	cursor := ""

	for {
		resp, err := fetch(cursor)
		if err != nil {
			return nil, false, err
		}

		follows := followsNode(resp)
		if follows == nil {
			return channels, false, nil
		}
		edges, _ := follows["edges"].([]interface{})
		if len(edges) == 0 {
			return channels, false, nil
		}

		lastCursor := ""
		for _, e := range edges {
			edge, ok := e.(map[string]interface{})
			if !ok {
				continue
			}
			if cur, ok := edge["cursor"].(string); ok && cur != "" {
				lastCursor = cur
			}
			node, ok := edge["node"].(map[string]interface{})
			if !ok || node == nil {
				continue
			}
			login, _ := node["login"].(string)
			login = strings.ToLower(strings.TrimSpace(login))
			if login == "" || seen[login] {
				continue
			}
			seen[login] = true
			display, _ := node["displayName"].(string)
			channels = append(channels, FollowedChannel{Login: login, DisplayName: display})

			if len(channels) >= maxFollowedFetch {
				// Cap reached: report truncation only if more remain.
				return channels, hasNextPage(follows), nil
			}
		}

		if !hasNextPage(follows) || lastCursor == "" {
			return channels, false, nil
		}
		cursor = lastCursor
	}
}

// followsNode digs out data.user.follows from a ChannelFollows response.
func followsNode(resp map[string]interface{}) map[string]interface{} {
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return nil
	}
	user, ok := data["user"].(map[string]interface{})
	if !ok || user == nil {
		return nil
	}
	follows, _ := user["follows"].(map[string]interface{})
	return follows
}

// hasNextPage reads follows.pageInfo.hasNextPage (false when absent).
func hasNextPage(follows map[string]interface{}) bool {
	pageInfo, ok := follows["pageInfo"].(map[string]interface{})
	if !ok {
		return false
	}
	next, _ := pageInfo["hasNextPage"].(bool)
	return next
}

func (c *TwitchClient) GetStreamInfo(ctx context.Context, streamer *models.Streamer) (map[string]interface{}, error) {
	op := constants.VideoPlayerStreamInfoOverlayChannel.WithVariables(map[string]interface{}{
		"channel": streamer.GetUsername(),
	})

	resp, err := c.postGQLRequest(ctx, op)
	if err != nil {
		// Transport / auth (ErrUnauthorized) / PersistedQueryNotFound / empty body
		// / invalid JSON — all inconclusive, never offline. Propagated verbatim and
		// classified by classifyCheck at the call site.
		return nil, err
	}

	// A top-level GraphQL "errors" array means Twitch returned no authoritative
	// data (even at HTTP 200) — inconclusive, not offline.
	if gql.HasTopLevelErrors(resp) {
		return nil, newStreamCheckError(models.ReasonGraphQLError, "twitch GQL %s: top-level errors", op.OperationName)
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok || data == nil {
		return nil, newStreamCheckError(models.ReasonMalformedResponse, "twitch GQL %s: missing or malformed data", op.OperationName)
	}

	user, ok := data["user"].(map[string]interface{})
	if !ok || user == nil {
		return nil, newStreamCheckError(models.ReasonMalformedResponse, "twitch GQL %s: missing or malformed user", op.OperationName)
	}

	// Distinguish the ONE authoritative offline (stream key present and JSON null)
	// from an absent/malformed stream field (inconclusive). Map membership — not a
	// type assertion — is what separates present-null from key-absent.
	streamVal, present := user["stream"]
	switch {
	case !present:
		return nil, newStreamCheckError(models.ReasonMalformedResponse, "twitch GQL %s: stream field absent", op.OperationName)
	case streamVal == nil:
		// user present, "stream": null — authoritatively offline.
		return nil, ErrStreamerIsOffline
	}
	if _, ok := streamVal.(map[string]interface{}); !ok {
		return nil, newStreamCheckError(models.ReasonMalformedResponse, "twitch GQL %s: malformed stream field", op.OperationName)
	}

	return user, nil
}

// SessionRefreshResult is the redacted outcome of a full playback-session refresh
// (see RefreshPlaybackSession). It reports whether the session tuple was atomically
// applied, whether it was rejected as stale (a newer session/observation won), the
// stage that failed (if any), and the generation bookkeeping — never a URL, token,
// or body. NoOp is set for a gated metadata refresh that started NO observation and
// did NO I/O (so it cannot supersede a concurrent refresh).
type SessionRefreshResult struct {
	NoOp               bool
	Applied            bool
	Stale              bool
	Stage              string // "" ok; "spade"; "stream_info"; "apply"
	Reason             string // bounded reason code
	AppliedGeneration  uint64
	CurrentGeneration  uint64
	CurrentBroadcastID string
}

// playbackRefreshIntent selects how much of a session refresh does and how the gate
// is honored. It is the seam that distinguishes a due-metadata refresh (which may
// no-op) from an authoritative online confirmation (which always fetches stream
// info) and a forced recovery.
type playbackRefreshIntent struct {
	fetchSpade      bool                   // re-scrape the spade URL (bring-online / full recovery)
	forceStreamInfo bool                   // always fetch stream info, ignoring the 2-minute gate
	expected        models.ExpectedSession // optimistic broadcast/generation preconditions
}

// UpdateStream refreshes a streamer's stream info and beacon payload IF DUE (past
// the 2-minute gate) and publishes them ATOMICALLY (broadcast, metadata, payload in
// one apply). It is the manager/pubsub online-metadata path. Errors are propagated
// verbatim (authoritative offline vs inconclusive) for the caller to classify; a
// stale (superseded) apply is surfaced as ErrPlaybackSessionStale — never a silent
// nil-success. A gated no-op returns nil and touches nothing.
func (c *TwitchClient) UpdateStream(streamer *models.Streamer) error {
	return c.updateStream(context.Background(), streamer)
}

// updateStream is UpdateStream under an explicit owner. UpdateStream keeps its
// context-free signature because its production caller (internal/pubsub) owns a
// different lifecycle; only the watch-owned online check reaches this form.
func (c *TwitchClient) updateStream(ctx context.Context, streamer *models.Streamer) error {
	_, err := c.doRefreshPlaybackSession(ctx, streamer, playbackRefreshIntent{})
	return err
}

// ConfirmOnline runs the authoritative bring-online refresh: it ALWAYS fetches the
// spade URL AND fresh stream info (ignoring the 2-minute gate, so a fresh
// lastUpdate can never let liveness be confirmed on stale cached data), and
// publishes the whole session in one atomic apply. Online is confirmable only when
// this returns a valid stream object (err == nil, not stale). Returns the redacted
// result plus the underlying error for the tri-state classifier.
func (c *TwitchClient) ConfirmOnline(ctx context.Context, streamer *models.Streamer) (SessionRefreshResult, error) {
	return c.doRefreshPlaybackSession(ctx, streamer, playbackRefreshIntent{
		fetchSpade:      true,
		forceStreamInfo: true,
	})
}

// RefreshPlaybackSession is the broker-facing forced recovery refresh: it ALWAYS
// fetches stream info (optionally the spade URL) OFF the Stream lock and publishes
// the whole tuple in ONE atomic, optimistic apply guarded by the expected
// broadcast/generation. It never returns a raw error (the broker does not classify
// liveness); a network failure or a stale/superseded apply is reflected in the
// redacted result.
func (c *TwitchClient) RefreshPlaybackSession(ctx context.Context, streamer *models.Streamer, fetchSpade bool, expected models.ExpectedSession) SessionRefreshResult {
	res, err := c.doRefreshPlaybackSession(ctx, streamer, playbackRefreshIntent{
		fetchSpade:      fetchSpade,
		forceStreamInfo: true,
		expected:        expected,
	})
	if err != nil && !res.Stale {
		if res.Stage == "" {
			res.Stage = "stream_info"
		}
		if res.Reason == "" {
			res.Reason = string(classifyReason(err))
		}
	}
	return res
}

// classifyReason maps a stream-info/transport error to the bounded status reason
// code (reusing the tri-state classifier's vocabulary), for redacted outcomes.
func classifyReason(err error) models.StatusReason {
	_, reason := classifyCheck(err)
	return reason
}

// pendingCampaignAvailability is a channel-side availability observation fetched
// OFF the Stream lock and held until the playback-session apply decides whether to
// publish it — a stale/rejected refresh must not publish availability.
type pendingCampaignAvailability struct {
	obsID uint64
	known bool
	ids   []string
	at    time.Time
}

// observeCampaignAvailability fetches the availability result into a local pending
// value under a PRE-ALLOCATED observation id (obsID) WITHOUT publishing it. The
// observation is reserved by the caller ALONGSIDE the session observation, before
// any I/O, so the availability domain keeps the same relative ordering as the
// session domain (an older playback refresh never holds a newer availability
// observation). Publish happens later, only if the playback-session apply succeeded
// (see doRefreshPlaybackSession). It preserves the tri-state Known/Unknown
// contract: a resolved lookup (including a legitimately empty list) is Known; a
// failed lookup or an unresolved game is Unknown (keeping previous IDs as
// last-known).
func (c *TwitchClient) observeCampaignAvailability(ctx context.Context, streamer *models.Streamer, game *models.Game, obsID uint64) pendingCampaignAvailability {
	pend := pendingCampaignAvailability{
		obsID: obsID,
		at:    time.Now(),
	}
	if game != nil && game.Name != "" && game.ID != "" {
		if campaignIDs, err := c.GetCampaignIDsFromStreamer(ctx, streamer); err != nil {
			slog.Warn("Failed to fetch channel drop campaign IDs; availability unknown (keeping previous list as last-known)",
				"streamer", streamer.GetUsername(), "error", err)
		} else {
			pend.known, pend.ids = true, campaignIDs
		}
	}
	return pend
}

// doRefreshPlaybackSession is the shared orchestration for every playback-session
// refresh intent. It does NO work and starts NO observation for a gated no-op;
// otherwise it begins the session observation before the FIRST network I/O
// (newest-STARTED-wins), performs all fetch/parse/campaign-availability work OFF
// the Stream lock into an immutable candidate + a pending availability, publishes
// the whole tuple in ONE atomic apply, and only then publishes the availability —
// and only if the apply was applied. It returns the redacted result plus the
// underlying error (ErrStreamerIsOffline / a StreamCheckError / a spade error /
// ErrPlaybackSessionStale) for the tri-state classifier.
func (c *TwitchClient) doRefreshPlaybackSession(ctx context.Context, streamer *models.Streamer, intent playbackRefreshIntent) (SessionRefreshResult, error) {
	res := SessionRefreshResult{
		CurrentGeneration:  streamer.Stream.SessionGeneration(),
		CurrentBroadcastID: streamer.Stream.GetBroadcastID(),
	}

	// Cancellation gate BEFORE the no-op gate. A gated no-op returns (res, nil),
	// and classifyCheck maps a nil error to StatusOnline — so a cancelled
	// generation reaching the no-op gate would authoritatively confirm ONLINE
	// with zero network evidence. A cancelled refresh therefore always surfaces
	// ctx.Err(), which classifyCheck maps to UNKNOWN/ReasonTimeout.
	if err := ctx.Err(); err != nil {
		return res, err
	}

	fetchStreamInfo := intent.forceStreamInfo || streamer.Stream.UpdateRequired()

	// No-op gate FIRST, before any observation. A gated metadata refresh with
	// nothing to fetch must not begin an observation (it would spuriously supersede
	// a concurrent real refresh), must not touch sessionObs/sessionGen, and must do
	// no I/O — it returns an explicit NoOp.
	if !intent.fetchSpade && !fetchStreamInfo {
		res.NoOp = true
		return res, nil
	}

	// Begin the observation ONLY now that we will do I/O, before the FIRST network
	// call, so a concurrently-started newer refresh always supersedes this one.
	//
	// Reserve BOTH the session and the campaign-availability observation ids in a
	// SINGLE atomic step: two separate Begin* calls could be preempted between
	// them and hand out inverted (session, availability) pairs across concurrent
	// refreshes, letting a newer playback refresh's availability be rejected as
	// stale against an older one. BeginPlaybackRefreshObservation advances both
	// counters under one Stream.mu hold, so the two domains keep the same relative
	// ordering with no window in between (see PlaybackRefreshObservation).
	claimDrops := streamer.GetSettings().ClaimDrops
	observation := streamer.Stream.BeginPlaybackRefreshObservation(fetchStreamInfo && claimDrops)
	obs := observation.SessionID
	availObs := observation.CampaignAvailabilityID

	// afterRefreshObservation, when set (tests only), fires immediately after the
	// atomic observation pair is reserved and before any network I/O — a
	// deterministic barrier for interleaving concurrent refreshes at exactly the
	// point the split allocator could invert.
	if c.afterRefreshObservation != nil {
		c.afterRefreshObservation()
	}

	cand := models.PlaybackSessionCandidate{}

	if intent.fetchSpade {
		url, err := c.discoverSpadeURL(ctx, streamer)
		if err != nil {
			res.Stage, res.Reason = "spade", string(models.ReasonSpadeUnavailable)
			return res, err
		}
		cand = cand.WithSpadeURL(url)
	}

	var (
		avail     pendingCampaignAvailability
		haveAvail bool
	)
	if fetchStreamInfo {
		streamInfo, err := c.GetStreamInfo(ctx, streamer)
		if err != nil {
			// Offline (ErrStreamerIsOffline) or inconclusive — no apply either way.
			res.Stage = "stream_info"
			return res, err
		}

		stream, ok := streamInfo["stream"].(map[string]interface{})
		if !ok {
			err := newStreamCheckError(models.ReasonMalformedResponse, "twitch GQL %s: malformed stream after fetch", constants.VideoPlayerStreamInfoOverlayChannel.OperationName)
			res.Stage = "stream_info"
			return res, err
		}

		broadcastSettings, _ := streamInfo["broadcastSettings"].(map[string]interface{})
		broadcastID, _ := stream["id"].(string)
		title := ""
		if broadcastSettings != nil {
			title, _ = broadcastSettings["title"].(string)
		}

		var game *models.Game
		if broadcastSettings != nil {
			if gameData, ok := broadcastSettings["game"].(map[string]interface{}); ok && gameData != nil {
				game = &models.Game{}
				game.ID, _ = gameData["id"].(string)
				game.Name, _ = gameData["name"].(string)
				game.DisplayName, _ = gameData["displayName"].(string)
			}
		}

		var tags []models.Tag
		if tagsData, ok := stream["tags"].([]interface{}); ok {
			for _, t := range tagsData {
				if tagMap, ok := t.(map[string]interface{}); ok {
					tag := models.Tag{}
					tag.ID, _ = tagMap["id"].(string)
					tag.LocalizedName, _ = tagMap["localizedName"].(string)
					tags = append(tags, tag)
				}
			}
		}

		viewersCount := 0
		if vc, ok := stream["viewersCount"].(float64); ok {
			viewersCount = int(vc)
		}

		cand.BroadcastID = broadcastID
		cand.Title = strings.TrimSpace(title)
		cand.Game = game
		cand.Tags = tags
		cand.ViewersCount = viewersCount

		// Channel-side campaign availability uses the observation id reserved above
		// (aligned with the session observation). Its RESULT is held locally and NOT
		// published yet: a stale/rejected playback apply must not publish
		// availability derived from a superseded refresh.
		if claimDrops {
			avail = c.observeCampaignAvailability(ctx, streamer, game, availObs)
			haveAvail = true
		}

		// A payload that cannot be built (an unusable viewer identity) is NOT a
		// stream-check outcome: the channel's online state is unaffected, so this
		// deliberately does not fail the refresh or feed the tri-state classifier.
		// The faulted candidate clears the session payload instead, and the minute
		// sender then fails closed at its session-snapshot gate before any Spade
		// request — never a beacon with a coerced user_id.
		var payloadErr error
		cand, payloadErr = cand.WithPayload(streamer.ChannelID, broadcastID, c.auth.GetUserID(), streamer.GetUsername(), game, nil)
		if payloadErr != nil {
			// Loud, not silent: without a payload NOTHING can earn watch credit for
			// this channel. The bounded sentinel is logged; the offending id is not.
			slog.Warn("Minute-watched beacon payload could not be built; watch credit is impossible until the viewer identity is valid",
				"channel", streamer.GetUsername(), "error", payloadErr)
		}
	}

	if cand.IsEmpty() {
		// Nothing to publish (spade-less gated no-op past the observation begin —
		// e.g. an intent that fetched neither). No availability was observed here.
		res.CurrentGeneration = streamer.Stream.SessionGeneration()
		res.CurrentBroadcastID = streamer.Stream.GetBroadcastID()
		return res, nil
	}

	if c.beforeSessionApply != nil {
		c.beforeSessionApply()
	}

	apply := streamer.Stream.ApplyPlaybackSessionIfCurrent(obs, cand, intent.expected)
	res.Applied = apply.Applied
	res.Stale = apply.Stale
	res.AppliedGeneration = apply.Generation
	res.CurrentGeneration = apply.CurrentGeneration
	res.CurrentBroadcastID = apply.CurrentBroadcastID

	// Publish campaign availability ONLY when the playback session was applied. A
	// stale/superseded refresh publishes nothing (its own campaignAvailObs guard
	// would also drop a newer-superseded result, but the apply gate is the
	// authoritative "this whole refresh was rejected" signal).
	if haveAvail && apply.Applied {
		streamer.Stream.ApplyCampaignAvailability(avail.obsID, avail.known, avail.ids, avail.at)
	}

	if apply.Stale {
		res.Stage, res.Reason = "apply", apply.Reason
		return res, ErrPlaybackSessionStale
	}
	return res, nil
}

// CheckStreamerOnline resolves a streamer's live status and applies the resulting
// tri-state transition, returning it so the caller (PubSub viewcount, the manager
// loop, discovery, health) can act on a typed result instead of racing to read
// mutable state before and after the call. The classification is authoritative:
//   - a valid stream object  -> online
//   - a valid "stream": null -> offline
//   - EVERYTHING else (transport, timeout, auth, PersistedQueryNotFound, top-level
//     GraphQL errors, malformed/absent structural fields, a Spade fetch failure,
//     cancelled context) -> UNKNOWN, never a false offline.
//
// The result is applied under a stale-observation guard (StatusSnapshot captured
// before any I/O), so a slow result can never overwrite a newer authoritative
// PubSub stream-up/stream-down that landed while this check was in flight.
func (c *TwitchClient) CheckStreamerOnline(streamer *models.Streamer) models.StatusTransition {
	return c.CheckStreamerOnlineContext(context.Background(), streamer)
}

// CheckStreamerOnlineContext is CheckStreamerOnline under an explicit
// cancellation owner. CheckStreamerOnline keeps its context-free signature
// because four other subsystems (pubsub, the miner's stream-check loop, the
// streamer manager and the health canary) own their own lifecycles and must not
// be cancelled by a watch-generation teardown. This form is used only by work
// the watch generation owns: the broker's two call sites, and directory
// discovery's candidate re-verification, which runs on the broker's own loop
// goroutine. A cancelled check stays inconclusive:
// classifyCheck maps context.Canceled/DeadlineExceeded to UNKNOWN, never to an
// authoritative OFFLINE (and, via the gate in doRefreshPlaybackSession, never
// to a no-op-nil ONLINE either).
func (c *TwitchClient) CheckStreamerOnlineContext(ctx context.Context, streamer *models.Streamer) models.StatusTransition {
	// Rate-limit re-checks of a CONFIRMED-offline streamer (don't hammer a channel
	// just authoritatively seen offline). Unknown and online streamers are checked
	// on their normal cadence so an unknown resolves promptly — unlike the old gate
	// on OfflineAt, which unknown no longer writes.
	status, obsSeq := streamer.StatusSnapshot()
	if status == models.StatusOffline && time.Since(streamer.GetOfflineAt()) < time.Minute {
		return models.StatusTransition{Previous: status, Current: status}
	}

	streamer.SetLastChecked(time.Now())

	if status != models.StatusOnline {
		// Not currently confirmed online: run the authoritative bring-online probe
		// as ONE atomic session refresh (spade URL + FRESH stream info + payload
		// published together). ConfirmOnline always fetches stream info, so a fresh
		// lastUpdate can never let online be confirmed on stale cached data.
		res, err := c.ConfirmOnline(ctx, streamer)
		switch {
		case res.Stage == "spade":
			// A Spade fetch failure is inconclusive (UNKNOWN), NOT evidence the
			// channel is offline — never SetOffline here.
			slog.Debug("Cannot fetch Spade URL; recording status as unknown (not offline)",
				"streamer", streamer.GetUsername(), "reason", string(models.ReasonSpadeUnavailable))
			return streamer.ApplyCheckResultIfCurrent(obsSeq, models.StatusUnknown, models.ReasonSpadeUnavailable)
		case res.Stale:
			// A newer observation superseded this refresh: inconclusive. A stale
			// (old/superseded) check must NOT confirm online over the newer session.
			slog.Debug("Bring-online session superseded by a newer observation; recording unknown (not online)",
				"streamer", streamer.GetUsername(), "reason", string(models.ReasonSessionStale))
			return streamer.ApplyCheckResultIfCurrent(obsSeq, models.StatusUnknown, models.ReasonSessionStale)
		}

		next, reason := classifyCheck(err)
		tr := streamer.ApplyCheckResultIfCurrent(obsSeq, next, reason)
		if tr.OnlineConfirmed {
			// Log only on a genuine transition (not a recovery continuation), so
			// racing detectors don't print a duplicate "Streamer is online".
			slog.Info("Streamer is online",
				"streamer", streamer.GetUsername(),
				"channelID", streamer.ChannelID,
				"broadcastID", streamer.Stream.GetBroadcastID())
		} else if tr.Current == models.StatusUnknown && !tr.Stale {
			slog.Debug("Cannot confirm stream status; keeping state as unknown (not offline)",
				"streamer", streamer.GetUsername(), "reason", string(reason))
		}
		return tr
	}

	// Already confirmed online: refresh metadata. A non-authoritative refresh
	// failure (transport, PersistedQueryNotFound, top-level GraphQL errors,
	// malformed response) yields UNKNOWN — never offline — so the slot, streak and
	// last-known stream data are preserved; only an authoritative "stream": null
	// settles offline (which arrives here as ErrStreamerIsOffline) or a PubSub
	// stream-down does it directly.
	next, reason := classifyCheck(c.updateStream(ctx, streamer))
	tr := streamer.ApplyCheckResultIfCurrent(obsSeq, next, reason)
	switch {
	case tr.OfflineConfirmed:
		slog.Info("Streamer went offline",
			"streamer", streamer.GetUsername(),
			"channelID", streamer.ChannelID,
			"broadcastID", streamer.Stream.GetBroadcastID())
	case tr.Current == models.StatusUnknown && tr.Changed():
		slog.Warn("Cannot refresh stream info; status is unknown, keeping last-known state (not offline)",
			"streamer", streamer.GetUsername(), "reason", string(reason))
	}
	return tr
}

// capabilityFromError maps a transport/GQL error to the tri-state Channel
// Points capability reason. Every error outcome is UNKNOWN (never Disabled):
// a failure is not proof the feature is off.
func capabilityFromError(err error) models.CapabilityReason {
	switch {
	case errors.Is(err, ErrPersistedQueryNotFound):
		return models.CapReasonPQNF
	case errors.Is(err, ErrUnauthorized):
		return models.CapReasonUnauthorized
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return models.CapReasonCancelled
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return models.CapReasonTimeout
	}
	return models.CapReasonTransportError
}

// parseActiveMultipliers parses the activeMultipliers array with ALL-OR-NOTHING
// strictness: it returns (multipliers, true) only when EVERY element is an object
// carrying a numeric factor. A single malformed element (non-object, or a
// missing/non-numeric factor) returns (nil, false) so the caller preserves the
// prior multipliers rather than publishing a partial set. A valid empty array
// returns (empty, true) — the authoritative clear.
func parseActiveMultipliers(ms []interface{}) ([]models.Multiplier, bool) {
	out := make([]models.Multiplier, 0, len(ms))
	for _, m := range ms {
		mMap, ok := m.(map[string]interface{})
		if !ok || mMap == nil {
			return nil, false
		}
		factor, ok := mMap["factor"].(float64)
		if !ok {
			return nil, false
		}
		out = append(out, models.Multiplier{Factor: factor})
	}
	return out, true
}

// parseCommunityGoals parses the goals array with ALL-OR-NOTHING strictness: it
// returns (goals, true) only when EVERY element is an object that yields a goal
// with a non-empty id. A single malformed element returns (nil, false) so the
// caller preserves the prior goals rather than publishing a partial upsert. A
// valid empty array returns (empty, true) — but the caller's upsert semantics
// never clear on empty (goal removal is owned by the PubSub delete path).
func parseCommunityGoals(goals []interface{}) ([]*models.CommunityGoal, bool) {
	out := make([]*models.CommunityGoal, 0, len(goals))
	for _, g := range goals {
		goalMap, ok := g.(map[string]interface{})
		if !ok || goalMap == nil {
			return nil, false
		}
		goal := models.CommunityGoalFromGQL(goalMap)
		if goal == nil || goal.GoalID == "" {
			return nil, false
		}
		out = append(out, goal)
	}
	return out, true
}

type channelPointsObservation struct {
	obsID uint64
	apply models.ContextApplyResult
}

// observeChannelPointsContext is the single full-context read/apply path used
// by both hydration and the fallback poll. A poll therefore cannot supersede a
// concurrent full observation while publishing only the bonus field and
// silently discarding authoritative balance, multipliers, or goals.
func (c *TwitchClient) observeChannelPointsContext(streamer *models.Streamer) (channelPointsObservation, error) {
	// Begin a fresh observation BEFORE the I/O. Only the latest-begun observation
	// may publish, so a newer request always wins regardless of completion order
	// (capSeq alone could not order two requests that begin at the same sequence).
	obsID := streamer.BeginChannelPointsContextObservation()

	// applyUnknown publishes an inconclusive result through the observation guard:
	// capability Unknown, and every optional field absent, so LastConfirmed,
	// balance, multipliers and goals are all PRESERVED.
	applyUnknown := func(reason models.CapabilityReason) models.ContextApplyResult {
		return streamer.ApplyChannelPointsContext(obsID, models.ChannelPointsContextSnapshot{
			Capability: models.CapabilityUnknown, Reason: reason,
		})
	}

	op := constants.ChannelPointsContext.WithVariables(map[string]interface{}{
		"channelLogin": streamer.GetUsername(),
	})

	resp, err := c.postGQLRequest(context.Background(), op)
	if err != nil {
		apply := applyUnknown(capabilityFromError(err))
		return channelPointsObservation{obsID: obsID, apply: apply}, err
	}

	// A top-level GraphQL "errors" array (even at HTTP 200, and even when a
	// partially-valid data node is ALSO present) means Twitch returned no
	// authoritative context. Classify UNKNOWN and stop BEFORE parsing data — a
	// service-layer error must never be read as an Enabled capability nor update
	// balance/multipliers/goals nor trigger a bonus claim. LastConfirmed and every
	// optional field are preserved (applyUnknown writes none of them).
	if gql.HasTopLevelErrors(resp) {
		apply := applyUnknown(models.CapReasonGraphQLError)
		return channelPointsObservation{obsID: obsID, apply: apply},
			fmt.Errorf("twitch GQL %s: top-level errors", op.OperationName)
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		apply := applyUnknown(models.CapReasonMalformed)
		return channelPointsObservation{obsID: obsID, apply: apply}, ErrStreamerDoesNotExist
	}
	community, ok := data["community"].(map[string]interface{})
	if !ok || community == nil {
		apply := applyUnknown(models.CapReasonMalformed)
		return channelPointsObservation{obsID: obsID, apply: apply}, ErrStreamerDoesNotExist
	}
	channel, ok := community["channel"].(map[string]interface{})
	if !ok || channel == nil {
		apply := applyUnknown(models.CapReasonMalformed)
		return channelPointsObservation{obsID: obsID, apply: apply}, ErrStreamerDoesNotExist
	}
	self, ok := channel["self"].(map[string]interface{})
	if !ok {
		// Structurally valid channel with no self node: the feature context is
		// missing, but Twitch is NOT known to signal "disabled" by omission, so
		// UNKNOWN (never coerced to Disabled without proof).
		apply := applyUnknown(models.CapReasonMissingContext)
		return channelPointsObservation{obsID: obsID, apply: apply}, nil
	}
	communityPoints, ok := self["communityPoints"].(map[string]interface{})
	if !ok {
		apply := applyUnknown(models.CapReasonMissingContext)
		return channelPointsObservation{obsID: obsID, apply: apply}, nil
	}

	// Parse the FULL accepted context into one snapshot WITHOUT any streamer write.
	snap := models.ChannelPointsContextSnapshot{
		Capability: models.CapabilityEnabled,
		Reason:     models.CapReasonConfirmedContext,
	}
	if b, ok := communityPoints["balance"].(float64); ok {
		snap.Balance, snap.HasBalance = int(b), true
	}
	// activeMultipliers: ALL-OR-NOTHING. Absent or a wrong top-level type preserves
	// the prior value (HasMultipliers stays false). A present array is parsed
	// strictly — every element MUST be an object carrying a numeric factor. A single
	// malformed element rejects the WHOLE field (preserve prior), never a partial
	// set; a valid (possibly empty) array authoritatively replaces/clears.
	if raw, present := communityPoints["activeMultipliers"]; present {
		if ms, ok := raw.([]interface{}); ok {
			if mult, valid := parseActiveMultipliers(ms); valid {
				snap.Multipliers, snap.HasMultipliers = mult, true
			}
		}
	}
	if streamer.GetSettings().CommunityGoals {
		if settings, ok := channel["communityPointsSettings"].(map[string]interface{}); ok {
			if raw, present := settings["goals"]; present {
				if goals, ok := raw.([]interface{}); ok {
					// ALL-OR-NOTHING goals: a single malformed goal element rejects the
					// whole field (HasGoals stays false => prior goals preserved). A valid
					// list uses the upsert semantics; a valid EMPTY list does NOT clear
					// (goal removal is owned by the PubSub delete path).
					if gs, valid := parseCommunityGoals(goals); valid {
						snap.Goals, snap.HasGoals = gs, true
					}
				}
			}
		}
	}
	if availableClaim, ok := communityPoints["availableClaim"].(map[string]interface{}); ok && availableClaim != nil {
		if id, ok := availableClaim["id"].(string); ok {
			snap.AvailableClaimID = id
		}
	}

	// ONE atomic publication under the observation guard. A newer observation
	// makes this whole context (state + optional fields + bonus opportunity) stale.
	res := streamer.ApplyChannelPointsContext(obsID, snap)
	if res.Stale {
		slog.Debug("Dropping stale channel-points context (a newer observation already began)",
			"streamer", streamer.GetUsername())
		return channelPointsObservation{obsID: obsID, apply: res}, nil
	}
	return channelPointsObservation{obsID: obsID, apply: res}, nil
}

func (c *TwitchClient) LoadChannelPointsContext(streamer *models.Streamer) error {
	observation, err := c.observeChannelPointsContext(streamer)
	if err != nil {
		return err
	}
	if observation.apply.Stale || observation.apply.AvailableClaimID == "" {
		return nil
	}

	// Bonus claim: eligibility + reservation are one atomic Streamer transition;
	// network I/O begins only after the lock is released. Context hydration keeps
	// its historical error contract: a bonus failure is logged but does not turn a
	// successfully loaded full context into a load failure.
	claimResult, claimErr := c.claimObservedBonus(streamer, observation.obsID, observation.apply.AvailableClaimID)
	if claimErr != nil {
		slog.Error("Failed to claim bonus", "error", claimErr)
	} else if claimResult.Outcome == BonusClaimSuppressed {
		slog.Debug("Skipping bonus claim from context: not reserved",
			"streamer", streamer.GetUsername(), "reason", claimResult.Reason.String())
	}
	return nil
}

// ClaimAvailableBonus is the polling fallback for channel-points bonus chests.
// It re-reads the full channel-points context and arbitrates any available
// bonus through the same Streamer owner as PubSub/context hydration. It returns
// true only when this poll caller owns a fresh accepted mutation; this does not
// imply whether a PubSub event was delivered, delayed, or absent.
func (c *TwitchClient) ClaimAvailableBonus(streamer *models.Streamer) (bool, error) {
	observation, err := c.observeChannelPointsContext(streamer)
	if err != nil {
		return false, err
	}
	if observation.apply.Stale || observation.apply.AvailableClaimID == "" {
		return false, nil
	}
	result, err := c.claimObservedBonus(streamer, observation.obsID, observation.apply.AvailableClaimID)
	if err != nil {
		return false, err
	}
	return result.Fresh(), nil
}

// availableClaimID walks a ChannelPointsContext GraphQL response down to the
// currently-claimable bonus chest's claim ID, returning "" when no bonus is
// available. It defends against every level being absent so a partial or
// unexpected response is treated as "nothing to claim" rather than panicking.
func availableClaimID(resp map[string]interface{}) string {
	id, _ := availableClaimContext(resp)
	return id
}

// availableClaimContext returns authoritative=true only when the complete
// ChannelPointsContext path through communityPoints exists. An absent/null
// availableClaim is an authoritative "nothing available"; malformed enclosing
// structure is UNKNOWN and must never initiate a mutation.
func availableClaimContext(resp map[string]interface{}) (string, bool) {
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return "", false
	}
	community, ok := data["community"].(map[string]interface{})
	if !ok || community == nil {
		return "", false
	}
	channel, ok := community["channel"].(map[string]interface{})
	if !ok || channel == nil {
		return "", false
	}
	self, ok := channel["self"].(map[string]interface{})
	if !ok || self == nil {
		return "", false
	}
	communityPoints, ok := self["communityPoints"].(map[string]interface{})
	if !ok || communityPoints == nil {
		return "", false
	}
	rawClaim, present := communityPoints["availableClaim"]
	if !present || rawClaim == nil {
		return "", true
	}
	availableClaim, ok := rawClaim.(map[string]interface{})
	if !ok {
		return "", false
	}
	id, ok := availableClaim["id"].(string)
	if !ok {
		return "", false
	}
	return id, true
}

func (c *TwitchClient) ClaimBonus(streamer *models.Streamer, claimID string) (BonusClaimResult, error) {
	reservation := streamer.ReserveCurrentBonusClaimIfEligible(claimID)
	return c.claimReservedBonus(streamer, claimID, reservation)
}

func (c *TwitchClient) claimObservedBonus(streamer *models.Streamer, obsID uint64, claimID string) (BonusClaimResult, error) {
	reservation := streamer.ReserveBonusClaimIfEligible(obsID, claimID)
	return c.claimReservedBonus(streamer, claimID, reservation)
}

func (c *TwitchClient) claimReservedBonus(streamer *models.Streamer, claimID string, reservation models.BonusClaimReservation) (BonusClaimResult, error) {
	if !reservation.Authorized {
		slog.Debug("Skipping bonus claim: arbitration did not grant mutation ownership",
			"streamer", streamer.GetUsername(),
			"reason", reservation.Reason.String())
		return BonusClaimResult{Outcome: BonusClaimSuppressed, Reason: reservation.Reason}, nil
	}

	slog.Info("Claiming bonus", "streamer", streamer.GetUsername())

	op := constants.ClaimCommunityPoints.WithVariables(map[string]interface{}{
		"input": map[string]interface{}{
			"channelID": streamer.ChannelID,
			"claimID":   claimID,
		},
	})

	resp, delivery, err := c.postBonusMutation(op)
	if err != nil {
		completion := models.BonusClaimCompletionIndeterminate
		outcome := BonusClaimIndeterminate
		if delivery == bonusMutationProvenNotExecuted {
			completion = models.BonusClaimCompletionProvenNotExecuted
			outcome = BonusClaimRetryPending
		}
		if applied := streamer.CompleteBonusClaim(reservation, completion); !applied.Applied {
			return BonusClaimResult{Outcome: BonusClaimIndeterminate},
				fmt.Errorf("bonus claim completion lost arbitration ownership: %w", err)
		}
		if delivery == bonusMutationIndeterminate {
			return BonusClaimResult{Outcome: outcome}, fmt.Errorf("%w: %w", ErrBonusClaimIndeterminate, err)
		}
		return BonusClaimResult{Outcome: outcome}, err
	}

	// An HTTP 200 with no top-level GraphQL error is NOT proof of a claim: the
	// authoritative business-result node (data.claimCommunityPoints) must be
	// present and un-rejected. Previously the payload was ignored, so a null or
	// missing node was silently treated as success.
	status := classifyCommunityPointsClaim(resp)
	switch status {
	case ClaimStatusAccepted:
		c.markSuccess()
		completed := streamer.CompleteBonusClaim(reservation, models.BonusClaimCompletionSucceeded)
		if !completed.Applied || !completed.FreshSuccess {
			return BonusClaimResult{Outcome: BonusClaimIndeterminate},
				errors.New("bonus claim success completion lost arbitration ownership")
		}
		return BonusClaimResult{Outcome: BonusClaimFreshAccepted}, nil
	case ClaimStatusAlreadyClaimed:
		c.markSuccess()
		completed := streamer.CompleteBonusClaim(reservation, models.BonusClaimCompletionReconciled)
		if !completed.Applied {
			return BonusClaimResult{Outcome: BonusClaimIndeterminate},
				errors.New("bonus claim reconciliation lost arbitration ownership")
		}
		return BonusClaimResult{Outcome: BonusClaimReconciled}, nil
	case ClaimStatusRejected:
		c.markSuccess()
		streamer.CompleteBonusClaim(reservation, models.BonusClaimCompletionRejected)
		// Privacy-safe: outcome class only — never the payload, claim ID, token,
		// or headers. No success log/event is emitted for a non-accepted claim.
		slog.Warn("Channel points bonus claim not accepted by Twitch",
			"streamer", streamer.GetUsername(),
			"outcome", string(status),
			"retryable", false)
		return BonusClaimResult{Outcome: BonusClaimRejected}, fmt.Errorf("%w: %s", ErrClaimNotAccepted, status)
	default:
		c.connAcct.markFunctionalFailure(time.Now())
		streamer.CompleteBonusClaim(reservation, models.BonusClaimCompletionIndeterminate)
		slog.Warn("Channel points bonus claim outcome is indeterminate; refusing replay",
			"streamer", streamer.GetUsername(),
			"outcome", string(status),
			"retryable", false)
		return BonusClaimResult{Outcome: BonusClaimIndeterminate}, fmt.Errorf("%w: %s", ErrBonusClaimIndeterminate, status)
	}
}

func (c *TwitchClient) ClaimMoment(streamer *models.Streamer, momentID string) error {
	slog.Info("Claiming moment", "streamer", streamer.GetUsername())

	op := constants.CommunityMomentCalloutClaim.WithVariables(map[string]interface{}{
		"input": map[string]interface{}{
			"momentID": momentID,
		},
	})

	_, err := c.postGQLRequest(context.Background(), op)
	return err
}

func (c *TwitchClient) JoinRaid(streamer *models.Streamer, raid *models.Raid) error {
	if streamer.Raid != nil && streamer.Raid.RaidID == raid.RaidID {
		return nil
	}

	slog.Info("Joining raid", "from", streamer.GetUsername(), "to", raid.TargetLogin)

	op := constants.JoinRaid.WithVariables(map[string]interface{}{
		"input": map[string]interface{}{
			"raidID": raid.RaidID,
		},
	})

	resp, err := c.postGQLRequest(context.Background(), op)
	if err != nil {
		return err
	}

	// A 200 carrying a top-level "errors" array (a non-PQNF service failure)
	// means Twitch did not accept the join, same as the GetChannelID case.
	if gql.HasTopLevelErrors(resp) {
		return fmt.Errorf("twitch GQL error for %s: raid join not accepted", op.OperationName)
	}

	// Mark the raid as joined only once Twitch actually accepted it. Doing this
	// before the request (as previously) made the RaidID guard above treat a
	// FAILED join as done, so the repeated raid_update_v2 events Twitch sends
	// during the raid countdown — the natural retry channel — were silently
	// short-circuited and a failed join was never retried.
	streamer.Raid = raid
	return nil
}

// PlacePredictionBet places a single prediction bet for an explicit outcome and
// amount and interprets Twitch's response. It is the one and only Twitch entry
// point for placing a prediction — both the scheduled auto-bet and a manual
// dashboard bet go through it — so there is no second betting implementation to
// keep in sync. It deliberately does NOT mutate the event's local bet state
// (BetPlaced / Decision); the caller owns that bookkeeping under its own
// synchronization, which is what lets the pool serialize auto and manual bets
// against each other without this method knowing about locks.
//
// A returned error is either a transport/auth error from postGQLRequest or a
// "prediction error: <CODE>" carrying Twitch's own rejection code (e.g.
// NOT_ENOUGH_POINTS, EVENT_NOT_ACTIVE), so callers can surface a precise reason.
func (c *TwitchClient) PlacePredictionBet(event *models.EventPrediction, outcomeID string, amount int) error {
	op := constants.MakePrediction.WithVariables(map[string]interface{}{
		"input": map[string]interface{}{
			"eventID":       event.EventID,
			"outcomeID":     outcomeID,
			"points":        amount,
			"transactionID": util.RandomHex(16),
		},
	})

	resp, err := c.postGQLRequest(context.Background(), op)
	if err != nil {
		return err
	}

	if data, ok := resp["data"].(map[string]interface{}); ok {
		if makePrediction, ok := data["makePrediction"].(map[string]interface{}); ok {
			if errData, ok := makePrediction["error"].(map[string]interface{}); ok && errData != nil {
				if code, ok := errData["code"].(string); ok {
					return fmt.Errorf("prediction error: %s", code)
				}
				return fmt.Errorf("prediction error")
			}
		}
	}

	return nil
}

func (c *TwitchClient) GetCampaignIDsFromStreamer(ctx context.Context, streamer *models.Streamer) ([]string, error) {
	op := constants.DropsHighlightServiceAvailableDrops.WithVariables(map[string]interface{}{
		"channelID": streamer.ChannelID,
	})

	resp, err := c.postGQLRequest(ctx, op)
	if err != nil {
		return nil, err
	}

	// A top-level GraphQL "errors" array (even at HTTP 200, typically with
	// data:null) is a service-layer failure, NOT an authoritative "no campaigns
	// available here". Returning an error keeps channel-side availability UNKNOWN
	// so a transient failure never gets recorded as Known+empty (which the drops
	// assignment path would read as authoritative "No" and use to clear a valid
	// assignment mid-farm).
	if gql.HasTopLevelErrors(resp) {
		return nil, fmt.Errorf("twitch GQL %s: top-level errors", op.OperationName)
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok || data == nil {
		return nil, fmt.Errorf("twitch GQL %s: missing or malformed data", op.OperationName)
	}

	// An ABSENT/null channel node is an unresolved response, not proof of "no
	// campaigns". Treat it as inconclusive (=> availability UNKNOWN), never as an
	// authoritative empty list.
	channel, ok := data["channel"].(map[string]interface{})
	if !ok || channel == nil {
		return nil, fmt.Errorf("twitch GQL %s: missing or malformed channel", op.OperationName)
	}

	// The viewerDropCampaigns CONTAINER is discriminated via map MEMBERSHIP, not a
	// single type assertion — a bare `.([]interface{})` would conflate unavailable
	// evidence with a malformed wrong-type value. Per the proven contract:
	//   - key absent           => unavailable response => error (UNKNOWN);
	//   - explicit JSON null   => unavailable response => error (UNKNOWN);
	//   - valid empty array    => authoritative "no campaigns here" (Known + empty);
	//   - present, wrong type  => MALFORMED response => error (=> availability
	//                             UNKNOWN, previous IDs preserved; NEVER recorded as
	//                             an authoritative "No" that would clear a live
	//                             assignment).
	raw, present := channel["viewerDropCampaigns"]
	if !present {
		return nil, fmt.Errorf("twitch GQL %s: missing viewerDropCampaigns", op.OperationName)
	}
	if raw == nil {
		return nil, fmt.Errorf("twitch GQL %s: null viewerDropCampaigns", op.OperationName)
	}
	campaigns, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("twitch GQL %s: malformed viewerDropCampaigns container", op.OperationName)
	}
	if len(campaigns) == 0 {
		return nil, nil
	}

	// ALL-OR-NOTHING element parse: the list becomes an authoritative Known
	// allowlist only if EVERY element is a well-formed campaign object carrying a
	// clean, non-empty string id. A single malformed element makes the whole lookup
	// an error (=> availability UNKNOWN) — we never publish a valid SUBSET as if it
	// were the complete advertised set, and never clear the previous IDs off a
	// partially-parsed response. Channel/campaign IDs are opaque: a whitespace-only
	// id, or one with leading/trailing whitespace, is malformed and is NOT silently
	// trimmed (no case-folding, no fuzzy normalization). IDs are deduplicated and
	// returned in a deterministic (sorted) order.
	seen := make(map[string]struct{}, len(campaigns))
	for _, campaign := range campaigns {
		cm, ok := campaign.(map[string]interface{})
		if !ok || cm == nil {
			return nil, fmt.Errorf("twitch GQL %s: malformed campaign element", op.OperationName)
		}
		id, ok := cm["id"].(string)
		if !ok || id == "" || strings.TrimSpace(id) != id {
			return nil, fmt.Errorf("twitch GQL %s: malformed campaign id", op.OperationName)
		}
		seen[id] = struct{}{}
	}

	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// GetDropCampaignDetails fetches the full details for a single drop campaign,
// including its timeBasedDrops (each with its own start/end dates, required
// minutes and benefit). The ViewerDropsDashboard listing only returns campaign
// summaries without this per-drop breakdown, so the details must be fetched
// per campaign before the campaign can be tracked. Returns the raw
// `data.user.dropCampaign` map, or nil if the campaign is not found.
func (c *TwitchClient) GetDropCampaignDetails(campaignID string) (map[string]interface{}, error) {
	op := constants.DropCampaignDetails.WithVariables(map[string]interface{}{
		"dropID":       campaignID,
		"channelLogin": c.auth.GetUserID(),
	})

	resp, err := c.postGQLRequest(context.Background(), op)
	if err != nil {
		return nil, err
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return nil, nil
	}

	user, ok := data["user"].(map[string]interface{})
	if !ok || user == nil {
		return nil, nil
	}

	campaign, ok := user["dropCampaign"].(map[string]interface{})
	if !ok || campaign == nil {
		return nil, nil
	}

	return campaign, nil
}

func (c *TwitchClient) GetPlaybackAccessToken(ctx context.Context, username string) (string, string, error) {
	// platform:"web" is required by the current PlaybackAccessToken persisted
	// query (the hash and this variable set are kept in lockstep — see
	// constants.PlaybackAccessToken). Omitting it against the new hash yields an
	// empty/invalid token.
	op := constants.PlaybackAccessToken.WithVariables(map[string]interface{}{
		"login":      username,
		"isLive":     true,
		"isVod":      false,
		"vodID":      "",
		"playerType": "site",
		"platform":   "web",
	})

	resp, err := c.postGQLRequest(ctx, op)
	if err != nil {
		return "", "", err
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		slog.Debug("PlaybackAccessToken: no data", "username", username, "response", resp)
		return "", "", fmt.Errorf("no data in response")
	}

	sat, ok := data["streamPlaybackAccessToken"].(map[string]interface{})
	if !ok || sat == nil {
		sat, ok = data["streamAccessToken"].(map[string]interface{})
		if !ok || sat == nil {
			slog.Debug("PlaybackAccessToken: no token found", "username", username, "data", data)
			return "", "", fmt.Errorf("no stream access token")
		}
	}

	signature, _ := sat["signature"].(string)
	value, _ := sat["value"].(string)

	if signature == "" || value == "" {
		return "", "", fmt.Errorf("empty stream access token")
	}

	return signature, value, nil
}

// ClaimDrop submits the drop-reward claim mutation and returns the authoritative
// outcome as a ClaimStatus. The accepted statuses are preserved exactly as
// before — ELIGIBLE_FOR_ALL (fresh) and DROP_INSTANCE_ALREADY_CLAIMED (an
// idempotent already-claimed reconciliation) — but the status-parsing is now
// funneled through the shared classifyDropClaim boundary so callers can tell a
// fresh claim from a reconciliation (and thus avoid duplicate success events)
// and so the parse is unit-testable without a network round trip. On a
// transport/auth error the error is returned and the ClaimStatus is unspecified;
// callers must check the error first.
func (c *TwitchClient) ClaimDrop(drop *models.Drop) (ClaimStatus, error) {
	slog.Info("Claiming drop", "drop", drop.Name)

	op := constants.DropsPageClaimDropRewards.WithVariables(map[string]interface{}{
		"input": map[string]interface{}{
			"dropInstanceID": drop.DropInstanceID,
		},
	})

	resp, err := c.postGQLRequest(context.Background(), op)
	if err != nil {
		return ClaimStatus(""), err
	}

	return classifyDropClaim(resp), nil
}

func (c *TwitchClient) ContributeToCommunityGoal(streamer *models.Streamer, goalID, title string, amount int) error {
	op := constants.ContributeCommunityPointsCommunityGoal.WithVariables(map[string]interface{}{
		"input": map[string]interface{}{
			"amount":        amount,
			"channelID":     streamer.ChannelID,
			"goalID":        goalID,
			"transactionID": util.RandomHex(16),
		},
	})

	resp, err := c.postGQLRequest(context.Background(), op)
	if err != nil {
		return err
	}

	if data, ok := resp["data"].(map[string]interface{}); ok {
		if contribute, ok := data["contributeCommunityPointsCommunityGoal"].(map[string]interface{}); ok {
			if errData, ok := contribute["error"].(map[string]interface{}); ok && errData != nil {
				return fmt.Errorf("contribution error: %v", errData)
			}
		}
	}

	streamer.SetChannelPoints(streamer.GetChannelPoints() - amount)

	slog.Info("Contributed to community goal",
		"streamer", streamer.GetUsername(),
		"goal", title,
		"amount", amount,
		"remainingBalance", streamer.GetChannelPoints())

	return nil
}

// GetCustomRewards returns the streamer's custom channel-points rewards (the
// personal rewards a viewer can redeem with points, not Community Goals). It
// reuses the ChannelPointsContext query — the same one that carries the point
// balance and goals — and, since that response also includes the up-to-date
// balance, refreshes the streamer's cached points as a side effect so callers
// can immediately compare cost against balance.
func (c *TwitchClient) GetCustomRewards(streamer *models.Streamer) ([]*models.CustomReward, error) {
	op := constants.ChannelPointsContext.WithVariables(map[string]interface{}{
		"channelLogin": streamer.GetUsername(),
	})

	resp, err := c.postGQLRequest(context.Background(), op)
	if err != nil {
		return nil, err
	}

	channel := channelPointsChannel(resp)
	if channel == nil {
		return nil, ErrStreamerDoesNotExist
	}

	if self, ok := channel["self"].(map[string]interface{}); ok && self != nil {
		if communityPoints, ok := self["communityPoints"].(map[string]interface{}); ok {
			if balance, ok := communityPoints["balance"].(float64); ok {
				streamer.SetChannelPoints(int(balance))
			}
		}
	}

	settings, ok := channel["communityPointsSettings"].(map[string]interface{})
	if !ok || settings == nil {
		return nil, nil
	}

	rawRewards, ok := settings["customRewards"].([]interface{})
	if !ok {
		return nil, nil
	}

	rewards := make([]*models.CustomReward, 0, len(rawRewards))
	for _, r := range rawRewards {
		if rewardMap, ok := r.(map[string]interface{}); ok {
			reward := models.CustomRewardFromGQL(rewardMap)
			if reward.ID != "" {
				rewards = append(rewards, reward)
			}
		}
	}

	return rewards, nil
}

// channelPointsChannel walks a ChannelPointsContext response down to the
// community.channel object, returning nil if any level is missing so callers
// treat an unexpected shape as "no data" rather than panicking.
func channelPointsChannel(resp map[string]interface{}) map[string]interface{} {
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return nil
	}
	community, ok := data["community"].(map[string]interface{})
	if !ok || community == nil {
		return nil
	}
	channel, ok := community["channel"].(map[string]interface{})
	if !ok || channel == nil {
		return nil
	}
	return channel
}

// RedeemCustomReward spends channel points on a custom reward. The reward's
// current cost, title and prompt are echoed back in the input because Twitch
// rejects the redemption (PROPERTIES_MISMATCH) if any of them no longer match
// the server's copy — which is exactly the "reward changed between showing the
// list and clicking" race we want surfaced as a clear error. textInput carries
// the viewer's message for user-input rewards and is omitted otherwise.
//
// Errors are mapped to friendly messages: transport failures propagate as-is,
// while Twitch's own rejection codes become ErrInsufficientPoints /
// ErrRewardUnavailable or a descriptive error, never a panic. On success the
// streamer's cached balance is decremented by the cost.
func (c *TwitchClient) RedeemCustomReward(streamer *models.Streamer, reward *models.CustomReward, textInput string) error {
	slog.Info("Redeeming custom reward", "streamer", streamer.GetUsername(), "reward", reward.Title, "cost", reward.Cost)

	input := map[string]interface{}{
		"channelID":     streamer.ChannelID,
		"cost":          reward.Cost,
		"pricingType":   "POINTS",
		"prompt":        reward.Prompt,
		"rewardID":      reward.ID,
		"title":         reward.Title,
		"transactionID": util.RandomHex(16),
	}
	if reward.IsUserInputRequired && textInput != "" {
		input["textInput"] = textInput
	}

	op := constants.RedeemCustomReward.WithVariables(map[string]interface{}{
		"input": input,
	})

	resp, err := c.postGQLRequest(context.Background(), op)
	if err != nil {
		return err
	}

	if err := redeemResponseError(resp); err != nil {
		return err
	}

	streamer.SetChannelPoints(streamer.GetChannelPoints() - reward.Cost)
	return nil
}

// redeemResponseError inspects a RedeemCustomReward response and returns a
// friendly error when the redemption failed, or nil on success. It handles
// both top-level GraphQL errors and the mutation payload's own error object
// (data.redeemCommunityPointsCustomReward.error — the field keeps its old name
// even though the operation was renamed).
func redeemResponseError(resp map[string]interface{}) error {
	if errs, ok := resp["errors"].([]interface{}); ok && len(errs) > 0 {
		if em, ok := errs[0].(map[string]interface{}); ok {
			if msg, ok := em["message"].(string); ok && msg != "" {
				return fmt.Errorf("redemption rejected: %s", msg)
			}
		}
		return fmt.Errorf("redemption rejected")
	}

	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("redemption failed: unexpected response")
	}

	payload, ok := data["redeemCommunityPointsCustomReward"].(map[string]interface{})
	if !ok {
		payload, _ = data["redeemCustomReward"].(map[string]interface{})
	}
	if payload == nil {
		// No payload and no errors: treat as success rather than inventing a
		// failure, mirroring how Twitch omits the object on some success paths.
		return nil
	}

	errObj, ok := payload["error"].(map[string]interface{})
	if !ok || errObj == nil {
		return nil
	}

	code, _ := errObj["code"].(string)
	return redeemErrorForCode(code)
}

// redeemErrorForCode maps a Twitch redemption error code to a user-facing
// error, preferring the shared sentinels so callers can branch on them.
func redeemErrorForCode(code string) error {
	switch code {
	case "INSUFFICIENT_POINTS":
		return ErrInsufficientPoints
	case "NOT_AVAILABLE", "DISABLED", "OUT_OF_STOCK":
		return ErrRewardUnavailable
	case "COOLDOWN":
		return fmt.Errorf("reward is on cooldown")
	case "MAX_PER_STREAM_EXCEEDED":
		return fmt.Errorf("maximum redemptions per stream reached")
	case "MAX_PER_USER_PER_STREAM_EXCEEDED":
		return fmt.Errorf("you have already redeemed this reward this stream")
	case "PROPERTIES_MISMATCH":
		return fmt.Errorf("reward changed — refresh and try again")
	case "":
		return fmt.Errorf("redemption failed")
	default:
		return fmt.Errorf("redemption failed: %s", code)
	}
}
