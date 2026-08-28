package twitch

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func newEligibleBonusTestStreamer(username string) *models.Streamer {
	s := newTestStreamer(username)
	s.ChannelID = "channel-1"
	s.SetConfirmedOnline()
	s.SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)
	return s
}

const bonusContextWithClaim = `{"data":{"community":{"channel":{"self":{"communityPoints":{"availableClaim":{"id":"claim-1"}}}}}}}`

type bonusNetworkErrorRoundTripper struct {
	calls atomic.Int64
}

func (rt *bonusNetworkErrorRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	rt.calls.Add(1)
	return nil, errors.New("synthetic connection reset")
}

// captureAPILogs redirects the default slog logger to a buffer for the duration
// of a test so log content (or the absence of sensitive values) can be asserted.
func captureAPILogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// gqlOperationName decodes the operationName from a GQL request body so a test
// handler can answer ChannelPointsContext and ClaimCommunityPoints distinctly.
func gqlOperationName(r *http.Request) string {
	body, _ := io.ReadAll(r.Body)
	var op struct {
		OperationName string `json:"operationName"`
	}
	_ = json.Unmarshal(body, &op)
	return op.OperationName
}

// ---- classifyCommunityPointsClaim: the Channel Points response matrix -------

func TestClassifyCommunityPointsClaim(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]interface{}
		want ClaimStatus
	}{
		{
			// Parser-policy case (synthetic shape; NOT a claim that Twitch returns
			// exactly this): a non-empty node with a business-result field and no
			// error is accepted.
			name: "accepted: non-empty node with a business-result field, no error",
			resp: map[string]interface{}{"data": map[string]interface{}{
				"claimCommunityPoints": map[string]interface{}{"claim": map[string]interface{}{"id": "x"}},
			}},
			want: ClaimStatusAccepted,
		},
		{
			// Parser-policy case: an explicit `error: null` is the family's
			// "no error" marker and is a non-empty node, so it is accepted. This is
			// deliberately NOT merged with the empty-object case below.
			name: "accepted: present node with explicit null error",
			resp: map[string]interface{}{"data": map[string]interface{}{
				"claimCommunityPoints": map[string]interface{}{"error": nil},
			}},
			want: ClaimStatusAccepted,
		},
		{
			// Burden of proof: an EMPTY business-result object carries NO positive
			// evidence of a successful claim. No fixture/selection-set/captured
			// response confirms {} is a real success (verified by an adversarial
			// evidence hunt), so it is fail-closed as malformed — never accepted on
			// the mere absence of a rejection. Mirrors classifyDropClaim, which also
			// treats a status-less claimDropRewards:{} as malformed.
			name: "malformed: empty business-result object (no positive evidence)",
			resp: map[string]interface{}{"data": map[string]interface{}{
				"claimCommunityPoints": map[string]interface{}{},
			}},
			want: ClaimStatusMalformed,
		},
		{
			// A non-null `error` of an unexpected type is malformed — fail-closed,
			// never read as success.
			name: "malformed: error is a string",
			resp: map[string]interface{}{"data": map[string]interface{}{
				"claimCommunityPoints": map[string]interface{}{"error": "unexpected"},
			}},
			want: ClaimStatusMalformed,
		},
		{
			name: "malformed: error is a bool",
			resp: map[string]interface{}{"data": map[string]interface{}{
				"claimCommunityPoints": map[string]interface{}{"error": false},
			}},
			want: ClaimStatusMalformed,
		},
		{
			name: "malformed: error is an array",
			resp: map[string]interface{}{"data": map[string]interface{}{
				"claimCommunityPoints": map[string]interface{}{"error": []interface{}{}},
			}},
			want: ClaimStatusMalformed,
		},
		{
			name: "claimCommunityPoints: null",
			resp: map[string]interface{}{"data": map[string]interface{}{"claimCommunityPoints": nil}},
			want: ClaimStatusNullResult,
		},
		{
			name: "missing data",
			resp: map[string]interface{}{},
			want: ClaimStatusMissingData,
		},
		{
			name: "data: null",
			resp: map[string]interface{}{"data": nil},
			want: ClaimStatusMissingData,
		},
		{
			name: "missing mutation node",
			resp: map[string]interface{}{"data": map[string]interface{}{"other": 1}},
			want: ClaimStatusMissingResult,
		},
		{
			name: "malformed mutation node type (string)",
			resp: map[string]interface{}{"data": map[string]interface{}{"claimCommunityPoints": "nope"}},
			want: ClaimStatusMalformed,
		},
		{
			name: "top-level graphql errors",
			resp: map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "boom"}}},
			want: ClaimStatusGraphQLError,
		},
		{
			name: "mutation-level rejection error node",
			resp: map[string]interface{}{"data": map[string]interface{}{
				"claimCommunityPoints": map[string]interface{}{"error": map[string]interface{}{"code": "SERVER_ERROR"}},
			}},
			want: ClaimStatusRejected,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCommunityPointsClaim(tc.resp)
			if got != tc.want {
				t.Fatalf("classifyCommunityPointsClaim = %q, want %q", got, tc.want)
			}
			if got.Accepted() && tc.want != ClaimStatusAccepted && tc.want != ClaimStatusAlreadyClaimed {
				t.Fatalf("outcome %q must not report Accepted()", got)
			}
		})
	}
}

// ---- classifyDropClaim: the Drops mutation response matrix ------------------

func TestClassifyDropClaim(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]interface{}
		want ClaimStatus
	}{
		{
			name: "fresh accept: ELIGIBLE_FOR_ALL",
			resp: dropClaimResp("ELIGIBLE_FOR_ALL"),
			want: ClaimStatusAccepted,
		},
		{
			name: "already claimed: DROP_INSTANCE_ALREADY_CLAIMED",
			resp: dropClaimResp("DROP_INSTANCE_ALREADY_CLAIMED"),
			want: ClaimStatusAlreadyClaimed,
		},
		{
			name: "rejected: any other status",
			resp: dropClaimResp("NOT_ELIGIBLE"),
			want: ClaimStatusRejected,
		},
		{
			name: "null claim node",
			resp: map[string]interface{}{"data": map[string]interface{}{"claimDropRewards": nil}},
			want: ClaimStatusNullResult,
		},
		{
			name: "missing claim node",
			resp: map[string]interface{}{"data": map[string]interface{}{}},
			want: ClaimStatusMissingResult,
		},
		{
			name: "malformed claim node type",
			resp: map[string]interface{}{"data": map[string]interface{}{"claimDropRewards": "nope"}},
			want: ClaimStatusMalformed,
		},
		{
			name: "malformed: missing status field",
			resp: map[string]interface{}{"data": map[string]interface{}{"claimDropRewards": map[string]interface{}{}}},
			want: ClaimStatusMalformed,
		},
		{
			name: "missing data",
			resp: map[string]interface{}{},
			want: ClaimStatusMissingData,
		},
		{
			name: "top-level graphql errors",
			resp: map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "boom"}}},
			want: ClaimStatusGraphQLError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyDropClaim(tc.resp); got != tc.want {
				t.Fatalf("classifyDropClaim = %q, want %q", got, tc.want)
			}
		})
	}
}

func dropClaimResp(status string) map[string]interface{} {
	return map[string]interface{}{"data": map[string]interface{}{
		"claimDropRewards": map[string]interface{}{"status": status},
	}}
}

// ---- ClaimStatus contract methods ------------------------------------------

func TestClaimStatusContract(t *testing.T) {
	if !ClaimStatusAccepted.Accepted() || !ClaimStatusAccepted.Fresh() {
		t.Fatal("accepted must be Accepted() and Fresh()")
	}
	if !ClaimStatusAlreadyClaimed.Accepted() || ClaimStatusAlreadyClaimed.Fresh() {
		t.Fatal("already-claimed must be Accepted() but not Fresh()")
	}
	for _, s := range []ClaimStatus{ClaimStatusRejected, ClaimStatusMissingData, ClaimStatusMissingResult,
		ClaimStatusNullResult, ClaimStatusMalformed, ClaimStatusGraphQLError} {
		if s.Accepted() {
			t.Fatalf("%q must not be Accepted()", s)
		}
	}
	// Authoritative rejection is terminal; the rest are retryable.
	if ClaimStatusRejected.Retryable() {
		t.Fatal("rejected must not be Retryable()")
	}
	for _, s := range []ClaimStatus{ClaimStatusMissingData, ClaimStatusMissingResult,
		ClaimStatusNullResult, ClaimStatusMalformed, ClaimStatusGraphQLError} {
		if !s.Retryable() {
			t.Fatalf("%q must be Retryable()", s)
		}
	}
}

// ---- ClaimBonus end-to-end (httptest) --------------------------------------

func TestClaimBonusAcceptedReturnsNil(t *testing.T) {
	var requests atomic.Int64
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{"claim":{"id":"x"}}}}`)
	})
	streamer := newEligibleBonusTestStreamer("s")
	result, err := c.ClaimBonus(streamer, "claim-abc")
	if err != nil || !result.Fresh() {
		t.Fatalf("accepted claim must return nil, got %v", err)
	}
	duplicate, err := c.ClaimBonus(streamer, "claim-abc")
	if err != nil || duplicate.Outcome != BonusClaimSuppressed || duplicate.Reason != models.BonusReservationCompleted {
		t.Fatalf("completed duplicate = %+v, err=%v", duplicate, err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("accepted duplicate sent %d mutations, want 1", got)
	}
}

func TestClaimBonusNullResultIsNotSuccess(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":null}}`)
	})
	result, err := c.ClaimBonus(newEligibleBonusTestStreamer("s"), "claim-abc")
	if err == nil {
		t.Fatal("null business-result node must NOT be treated as success")
	}
	if !errors.Is(err, ErrBonusClaimIndeterminate) || result.Outcome != BonusClaimIndeterminate {
		t.Fatalf("want explicit indeterminate outcome, got result=%+v err=%v", result, err)
	}
}

// TestEmptyCommunityPointsResultIsMalformed pins the shared parser contract.
// Retryable remains a Drops-facing classifier method; the bonus-specific owner
// quarantines malformed mutation outcomes as indeterminate and never retries.
func TestEmptyCommunityPointsResultIsMalformed(t *testing.T) {
	st := classifyCommunityPointsClaim(map[string]interface{}{"data": map[string]interface{}{
		"claimCommunityPoints": map[string]interface{}{},
	}})
	if st != ClaimStatusMalformed {
		t.Fatalf("empty {} must be malformed, got %q", st)
	}
	if st.Accepted() {
		t.Fatal("empty {} must not report Accepted()")
	}
	if !st.Retryable() {
		t.Fatal("empty {} must remain retryable")
	}
}

// TestClaimBonusEmptyResultIsNotSuccess proves the end-to-end path: an empty
// claimCommunityPoints object makes ClaimBonus return an explicit indeterminate
// outcome with no success log/event, and leaks neither ID nor token into logs.
func TestClaimBonusEmptyResultIsNotSuccess(t *testing.T) {
	buf := captureAPILogs(t)
	const secretClaimID = "SECRET-CLAIM-ID-empty"
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{}}}`)
	})
	_, err := c.ClaimBonus(newEligibleBonusTestStreamer("s"), secretClaimID)
	if err == nil {
		t.Fatal("empty business-result object must NOT be treated as success")
	}
	if !errors.Is(err, ErrBonusClaimIndeterminate) {
		t.Fatalf("want ErrBonusClaimIndeterminate, got %v", err)
	}
	logs := buf.String()
	if strings.Contains(logs, "Claimed") {
		t.Fatalf("no success must be logged for an empty result; logs: %s", logs)
	}
	if strings.Contains(logs, secretClaimID) {
		t.Fatalf("claim ID must never be logged; found it in: %s", logs)
	}
	if strings.Contains(logs, "dummy-token") {
		t.Fatalf("OAuth token must never be logged; found it in: %s", logs)
	}
}

func TestClaimBonusRejectionIsNotSuccess(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{"error":{"code":"SERVER_ERROR"}}}}`)
	})
	if _, err := c.ClaimBonus(newEligibleBonusTestStreamer("s"), "claim-abc"); !errors.Is(err, ErrClaimNotAccepted) {
		t.Fatalf("mutation-level rejection must not be success, got %v", err)
	}
}

func TestClaimBonusUnauthorizedHTTP(t *testing.T) {
	var requests atomic.Int64
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"Unauthorized"}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{"claim":{"id":"accepted"}}}}`)
	})
	recoveries := installCountingRecoverFn(c)
	result, err := c.ClaimBonus(newEligibleBonusTestStreamer("s"), "claim-abc")
	if err != nil || !result.Fresh() {
		t.Fatalf("HTTP 401 recovery replay = %+v, err=%v", result, err)
	}
	if requests.Load() != 2 || recoveries.Load() != 1 {
		t.Fatalf("HTTP 401 requests=%d recoveries=%d, want 2/1", requests.Load(), recoveries.Load())
	}
}

func TestClaimBonusUnauthorizedGraphQL(t *testing.T) {
	var requests atomic.Int64
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"errors":[{"message":"Unauthorized"}]}`)
	})
	recoveries := installCountingRecoverFn(c)
	result, err := c.ClaimBonus(newEligibleBonusTestStreamer("s"), "claim-abc")
	if !errors.Is(err, ErrBonusClaimIndeterminate) || result.Outcome != BonusClaimIndeterminate {
		t.Fatalf("GraphQL Unauthorized must fail closed: result=%+v err=%v", result, err)
	}
	if requests.Load() != 1 || recoveries.Load() != 0 {
		t.Fatalf("GraphQL Unauthorized requests=%d recoveries=%d, want 1/0", requests.Load(), recoveries.Load())
	}
}

// An HTTP 5xx may follow remote execution, so the bonus-specific transport must
// make exactly one request and quarantine the claim as indeterminate.
func TestClaimBonusAmbiguousHTTPDoesNotReplay(t *testing.T) {
	var requests atomic.Int64
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	streamer := newEligibleBonusTestStreamer("s")
	result, err := c.ClaimBonus(streamer, "claim-abc")
	if err == nil {
		t.Fatal("an ambiguous HTTP failure must return an error")
	}
	if result.Outcome != BonusClaimIndeterminate || errors.Is(err, ErrClaimNotAccepted) {
		t.Fatalf("ambiguous failure = %+v err=%v", result, err)
	}
	duplicate, duplicateErr := c.ClaimBonus(streamer, "claim-abc")
	if duplicateErr != nil || duplicate.Reason != models.BonusReservationIndeterminate {
		t.Fatalf("indeterminate duplicate = %+v err=%v", duplicate, duplicateErr)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("ambiguous mutation requests=%d, want 1", got)
	}
}

func TestBonusMutationAmbiguousStatusMatrixDoesNotReplay(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"accepted-looking 202", http.StatusAccepted, `{"data":{"claimCommunityPoints":{"claim":{"id":"x"}}}}`},
		{"redirect 307", http.StatusTemporaryRedirect, `{"data":{"claimCommunityPoints":{"claim":{"id":"x"}}}}`},
		{"rate limited 429", http.StatusTooManyRequests, `{}`},
		{"service unavailable 503", http.StatusServiceUnavailable, `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int64
			var redirectTargets atomic.Int64
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == "/redirected" {
					redirectTargets.Add(1)
					_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{"claim":{"id":"redirected"}}}}`)
					return
				}
				if test.status == http.StatusTemporaryRedirect {
					w.Header().Set("Location", "/redirected")
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			})

			result, err := c.ClaimBonus(newEligibleBonusTestStreamer("s"), "claim-abc")
			if !errors.Is(err, ErrBonusClaimIndeterminate) || result.Outcome != BonusClaimIndeterminate {
				t.Fatalf("result=%+v err=%v, want indeterminate", result, err)
			}
			if requests.Load() != 1 || redirectTargets.Load() != 0 {
				t.Fatalf("requests=%d redirectTargets=%d, want 1/0", requests.Load(), redirectTargets.Load())
			}
		})
	}
}

func TestBonusMutationNetworkErrorIsSingleIndeterminateAttempt(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) {})
	rt := &bonusNetworkErrorRoundTripper{}
	c.client.Transport = rt
	before := c.LastSuccessAt()

	result, err := c.ClaimBonus(newEligibleBonusTestStreamer("s"), "claim-abc")
	if !errors.Is(err, ErrBonusClaimIndeterminate) || result.Outcome != BonusClaimIndeterminate {
		t.Fatalf("network error = %+v err=%v, want indeterminate", result, err)
	}
	if got := rt.calls.Load(); got != 1 {
		t.Fatalf("network-error requests=%d, want 1", got)
	}
	health := c.ConnHealth(time.Now(), time.Minute)
	if !health.LastSuccess.Equal(before) || health.RecentTransportFailures != 1 || health.RecentFunctionalFailures != 0 {
		t.Fatalf("network-error health=%+v before=%v", health, before)
	}
}

func TestBonusMutationExactPQNFUsesBoundedClientIDFallback(t *testing.T) {
	var mutations atomic.Int64
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := mutations.Add(1)
		if n < int64(len(constants.GQLClientIDFallbacks)) {
			_, _ = io.WriteString(w, persistedQueryNotFoundBody)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{"claim":{"id":"accepted"}}}}`)
	})

	result, err := c.ClaimBonus(newEligibleBonusTestStreamer("s"), "claim-abc")
	if err != nil || !result.Fresh() {
		t.Fatalf("exact PQNF fallback = %+v err=%v", result, err)
	}
	if got := mutations.Load(); got != int64(len(constants.GQLClientIDFallbacks)) {
		t.Fatalf("mutation requests=%d, want %d bounded candidates", got, len(constants.GQLClientIDFallbacks))
	}
}

func TestBonusMutationFalsePQNFDoesNotReplay(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"message only", `{"errors":[{"message":"PersistedQueryNotFound"}]}`},
		{"mixed error codes", `{"errors":[{"extensions":{"code":"PERSISTED_QUERY_NOT_FOUND"}},{"extensions":{"code":"OTHER"}}]}`},
		{"singular error conflict", `{"error":"Unauthorized","errors":[{"extensions":{"code":"PERSISTED_QUERY_NOT_FOUND"}}]}`},
		{"duplicate top-level data", `{"data":{"claimCommunityPoints":{"claim":{"id":"x"}}},"data":null,"errors":[{"extensions":{"code":"PERSISTED_QUERY_NOT_FOUND"}}]}`},
		{"duplicate top-level data last success", `{"data":null,"data":{"claimCommunityPoints":{"claim":{"id":"x"}}}}`},
		{"duplicate nested code", `{"errors":[{"extensions":{"code":"OTHER","code":"PERSISTED_QUERY_NOT_FOUND"}}]}`},
		{"duplicate result error last null", `{"data":{"claimCommunityPoints":{"error":{"code":"OTHER"},"error":null}}}`},
		{"conflicting data", `{"data":{"claimCommunityPoints":{"claim":{"id":"x"}}},"errors":[{"extensions":{"code":"PERSISTED_QUERY_NOT_FOUND"}}]}`},
		{"singular error with data", `{"error":"Unauthorized","data":{"claimCommunityPoints":{"claim":{"id":"x"}}}}`},
		{"empty errors with data", `{"errors":[],"data":{"claimCommunityPoints":{"claim":{"id":"x"}}}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int64
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.WriteString(w, test.body)
			})
			result, err := c.ClaimBonus(newEligibleBonusTestStreamer("s"), "claim-abc")
			if !errors.Is(err, ErrBonusClaimIndeterminate) || result.Outcome != BonusClaimIndeterminate {
				t.Fatalf("false PQNF = %+v err=%v, want indeterminate", result, err)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("false PQNF requests=%d, want 1", got)
			}
		})
	}
}

func TestBonusClaimProvenNonExecutionRetryIsObservationBounded(t *testing.T) {
	var mutationRequests atomic.Int64
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch gqlOperationName(r) {
		case "ChannelPointsContext":
			_, _ = io.WriteString(w, bonusContextWithClaim)
		case "ClaimCommunityPoints":
			mutationRequests.Add(1)
			_, _ = io.WriteString(w, persistedQueryNotFoundBody)
		default:
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	})
	streamer := newEligibleBonusTestStreamer("s")

	first, firstErr := c.ClaimBonus(streamer, "claim-1")
	if !errors.Is(firstErr, ErrPersistedQueryNotFound) || first.Outcome != BonusClaimRetryPending {
		t.Fatalf("first proved non-execution = %+v err=%v", first, firstErr)
	}
	directRetry, directErr := c.ClaimBonus(streamer, "claim-1")
	if directErr != nil || directRetry.Reason != models.BonusReservationRetryNeedsObservation {
		t.Fatalf("direct replay = %+v err=%v", directRetry, directErr)
	}

	claimed, secondErr := c.ClaimAvailableBonus(streamer)
	if claimed || !errors.Is(secondErr, ErrPersistedQueryNotFound) {
		t.Fatalf("fresh observed retry claimed=%v err=%v", claimed, secondErr)
	}
	claimed, thirdErr := c.ClaimAvailableBonus(streamer)
	if claimed || thirdErr != nil {
		t.Fatalf("exhausted observation claimed=%v err=%v", claimed, thirdErr)
	}
	exhausted, exhaustedErr := c.ClaimBonus(streamer, "claim-1")
	if exhaustedErr != nil || exhausted.Reason != models.BonusReservationRetryExhausted {
		t.Fatalf("exhausted direct call = %+v err=%v", exhausted, exhaustedErr)
	}

	want := int64(2 * len(constants.GQLClientIDFallbacks))
	if got := mutationRequests.Load(); got != want {
		t.Fatalf("proved non-execution mutation requests=%d, want bounded %d", got, want)
	}
}

func TestBonusClaimDifferentIDsAndStreamersRemainIndependent(t *testing.T) {
	var mutations atomic.Int64
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mutations.Add(1)
		_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{"claim":{"id":"accepted"}}}}`)
	})
	firstStreamer := newEligibleBonusTestStreamer("first")
	secondStreamer := newEligibleBonusTestStreamer("second")

	for _, call := range []struct {
		streamer *models.Streamer
		claimID  string
		fresh    bool
	}{
		{firstStreamer, "claim-a", true},
		{firstStreamer, "claim-b", true},
		{firstStreamer, "claim-a", false},
		{secondStreamer, "claim-a", true},
	} {
		result, err := c.ClaimBonus(call.streamer, call.claimID)
		if err != nil || result.Fresh() != call.fresh {
			t.Fatalf("claim %q for %q = %+v err=%v", call.claimID, call.streamer.GetUsername(), result, err)
		}
	}
	if got := mutations.Load(); got != 3 {
		t.Fatalf("independent mutation count=%d, want 3", got)
	}
}

func TestBonusClaimIndependentOwnersCanMutateConcurrently(t *testing.T) {
	tests := []struct {
		name      string
		claimants func() [2]struct {
			streamer *models.Streamer
			claimID  string
		}
	}{
		{
			name: "different IDs on one streamer",
			claimants: func() [2]struct {
				streamer *models.Streamer
				claimID  string
			} {
				s := newEligibleBonusTestStreamer("one")
				return [2]struct {
					streamer *models.Streamer
					claimID  string
				}{{s, "claim-a"}, {s, "claim-b"}}
			},
		},
		{
			name: "same ID on different streamers",
			claimants: func() [2]struct {
				streamer *models.Streamer
				claimID  string
			} {
				return [2]struct {
					streamer *models.Streamer
					claimID  string
				}{{newEligibleBonusTestStreamer("one"), "claim-shared"}, {newEligibleBonusTestStreamer("two"), "claim-shared"}}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entered := make(chan struct{}, 2)
			release := make(chan struct{})
			var mutations atomic.Int64
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				mutations.Add(1)
				entered <- struct{}{}
				<-release
				_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{"claim":{"id":"accepted"}}}}`)
			})
			claimants := test.claimants()
			type claimReturn struct {
				result BonusClaimResult
				err    error
			}
			done := make(chan claimReturn, 2)
			for _, claimant := range claimants {
				claimant := claimant
				go func() {
					result, err := c.ClaimBonus(claimant.streamer, claimant.claimID)
					done <- claimReturn{result: result, err: err}
				}()
			}
			for i := 0; i < 2; i++ {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					close(release)
					t.Fatal("independent owner did not enter mutation transport")
				}
			}
			close(release)
			for i := 0; i < 2; i++ {
				select {
				case returned := <-done:
					if returned.err != nil || !returned.result.Fresh() {
						t.Fatalf("independent claim = %+v err=%v", returned.result, returned.err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("independent owner did not finish")
				}
			}
			if got := mutations.Load(); got != 2 {
				t.Fatalf("independent mutation count=%d, want 2", got)
			}
		})
	}
}

func TestBonusClaimSecondHTTP401IsBoundedAndRetryPending(t *testing.T) {
	var requests atomic.Int64
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"Unauthorized"}`)
	})
	recoveries := installCountingRecoverFn(c)
	streamer := newEligibleBonusTestStreamer("s")
	result, err := c.ClaimBonus(streamer, "claim-abc")
	if !errors.Is(err, ErrUnauthorized) || result.Outcome != BonusClaimRetryPending {
		t.Fatalf("double 401 = %+v err=%v", result, err)
	}
	if requests.Load() != 2 || recoveries.Load() != 1 {
		t.Fatalf("double 401 requests=%d recoveries=%d, want 2/1", requests.Load(), recoveries.Load())
	}
	duplicate, duplicateErr := c.ClaimBonus(streamer, "claim-abc")
	if duplicateErr != nil || duplicate.Reason != models.BonusReservationRetryNeedsObservation || requests.Load() != 2 {
		t.Fatalf("post-401 direct replay = %+v err=%v requests=%d", duplicate, duplicateErr, requests.Load())
	}
}

// ---- ClaimAvailableBonus (polling fallback) --------------------------------

func TestClaimAvailableBonusSuccessOnlyAfterAccepted(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch gqlOperationName(r) {
		case "ChannelPointsContext":
			_, _ = io.WriteString(w, `{"data":{"community":{"channel":{"self":{"communityPoints":{"availableClaim":{"id":"claim-1"}}}}}}}`)
		case "ClaimCommunityPoints":
			_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{"claim":{"id":"claim-1"}}}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	})
	claimed, err := c.ClaimAvailableBonus(newEligibleBonusTestStreamer("s"))
	if err != nil || !claimed {
		t.Fatalf("polling fallback must report success after an accepted claim: claimed=%v err=%v", claimed, err)
	}
}

func TestClaimAvailableBonusFalseAfterNullResult(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch gqlOperationName(r) {
		case "ChannelPointsContext":
			_, _ = io.WriteString(w, `{"data":{"community":{"channel":{"self":{"communityPoints":{"availableClaim":{"id":"claim-1"}}}}}}}`)
		case "ClaimCommunityPoints":
			_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":null}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	})
	claimed, err := c.ClaimAvailableBonus(newEligibleBonusTestStreamer("s"))
	if claimed {
		t.Fatal("polling fallback must NOT report success when the claim returned a null result")
	}
	if !errors.Is(err, ErrBonusClaimIndeterminate) {
		t.Fatalf("want ErrBonusClaimIndeterminate from the fallback, got %v", err)
	}
}

func TestClaimAvailableBonusFalseAfterEmptyResult(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch gqlOperationName(r) {
		case "ChannelPointsContext":
			_, _ = io.WriteString(w, `{"data":{"community":{"channel":{"self":{"communityPoints":{"availableClaim":{"id":"claim-1"}}}}}}}`)
		case "ClaimCommunityPoints":
			_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{}}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	})
	claimed, err := c.ClaimAvailableBonus(newEligibleBonusTestStreamer("s"))
	if claimed {
		t.Fatal("polling fallback must NOT report success when the claim returned an empty object")
	}
	if !errors.Is(err, ErrBonusClaimIndeterminate) {
		t.Fatalf("want ErrBonusClaimIndeterminate from the fallback, got %v", err)
	}
}

func TestClaimAvailableBonusNothingToClaim(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"community":{"channel":{"self":{"communityPoints":{"availableClaim":null}}}}}}`)
	})
	claimed, err := c.ClaimAvailableBonus(newEligibleBonusTestStreamer("s"))
	if claimed || err != nil {
		t.Fatalf("no available claim must be (false, nil), got claimed=%v err=%v", claimed, err)
	}
}

func TestClaimAvailableBonusAppliesTheFullContextSnapshot(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"community":{"channel":{"self":{"communityPoints":{"balance":777,"activeMultipliers":[{"factor":2}],"availableClaim":null}}}}}}`)
	})
	streamer := newEligibleBonusTestStreamer("s")
	streamer.SetChannelPoints(100)
	claimed, err := c.ClaimAvailableBonus(streamer)
	if claimed || err != nil {
		t.Fatalf("full fallback observation claimed=%v err=%v", claimed, err)
	}
	if points := streamer.GetChannelPoints(); points != 777 {
		t.Fatalf("poll balance=%d, want 777", points)
	}
	if multiplier := streamer.TotalPointsMultiplier(); multiplier != 2 {
		t.Fatalf("poll multiplier=%v, want 2", multiplier)
	}
}

func TestClaimAvailableBonusRejectsPartialErrorsWithData(t *testing.T) {
	var contextRequests atomic.Int64
	var mutationRequests atomic.Int64
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch gqlOperationName(r) {
		case "ChannelPointsContext":
			contextRequests.Add(1)
			_, _ = io.WriteString(w, `{"errors":[{"message":"service failure"}],"data":{"community":{"channel":{"self":{"communityPoints":{"availableClaim":{"id":"claim-1"}}}}}}}`)
		case "ClaimCommunityPoints":
			mutationRequests.Add(1)
			_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{"claim":{"id":"accepted"}}}}`)
		}
	})
	streamer := newEligibleBonusTestStreamer("s")
	claimed, err := c.ClaimAvailableBonus(streamer)
	if claimed || err == nil {
		t.Fatalf("partial errors+data claimed=%v err=%v, want fail-closed error", claimed, err)
	}
	if contextRequests.Load() != 1 || mutationRequests.Load() != 0 {
		t.Fatalf("context requests=%d mutations=%d, want 1/0", contextRequests.Load(), mutationRequests.Load())
	}
	if state := streamer.GetChannelPointsCapability(); state != models.CapabilityUnknown {
		t.Fatalf("partial error capability=%v, want unknown", state)
	}
}

func TestClaimAvailableBonusRecoversAfterInconclusiveObservation(t *testing.T) {
	var contextRequests atomic.Int64
	var mutationRequests atomic.Int64
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch gqlOperationName(r) {
		case "ChannelPointsContext":
			if contextRequests.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"errors":[{"message":"temporary failure"}]}`)
				return
			}
			_, _ = io.WriteString(w, bonusContextWithClaim)
		case "ClaimCommunityPoints":
			mutationRequests.Add(1)
			_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{"claim":{"id":"accepted"}}}}`)
		}
	})
	streamer := newEligibleBonusTestStreamer("s")

	if claimed, err := c.ClaimAvailableBonus(streamer); claimed || err == nil {
		t.Fatalf("inconclusive poll claimed=%v err=%v, want error", claimed, err)
	}
	if state := streamer.GetChannelPointsCapability(); state != models.CapabilityUnknown {
		t.Fatalf("inconclusive poll capability=%v, want unknown", state)
	}
	if claimed, err := c.ClaimAvailableBonus(streamer); !claimed || err != nil {
		t.Fatalf("recovery poll claimed=%v err=%v, want fresh success", claimed, err)
	}
	if contextRequests.Load() != 2 || mutationRequests.Load() != 1 {
		t.Fatalf("recovery context=%d mutations=%d, want 2/1", contextRequests.Load(), mutationRequests.Load())
	}
}

// ---- Privacy: sensitive values must never reach the logs -------------------

func TestClaimBonusDoesNotLogClaimIDOrPayload(t *testing.T) {
	buf := captureAPILogs(t)
	const secretClaimID = "SECRET-CLAIM-ID-9f3a"
	const secretPayloadMarker = "SECRET-PAYLOAD-MARKER"
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Reject so the WARN diagnostic path runs, and embed a marker in the body.
		_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{"error":{"code":"`+secretPayloadMarker+`"}}}}`)
	})
	_, _ = c.ClaimBonus(newEligibleBonusTestStreamer("s"), secretClaimID)

	logs := buf.String()
	if strings.Contains(logs, secretClaimID) {
		t.Fatalf("claim ID must never be logged; found it in: %s", logs)
	}
	if strings.Contains(logs, secretPayloadMarker) {
		t.Fatalf("raw response payload must never be logged; found marker in: %s", logs)
	}
	if strings.Contains(logs, "dummy-token") {
		t.Fatalf("OAuth token must never be logged; found it in: %s", logs)
	}
}

// ---- ClaimDrop end-to-end (httptest) ---------------------------------------

func TestClaimDropStatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus ClaimStatus
	}{
		{"fresh", `{"data":{"claimDropRewards":{"status":"ELIGIBLE_FOR_ALL"}}}`, ClaimStatusAccepted},
		{"already", `{"data":{"claimDropRewards":{"status":"DROP_INSTANCE_ALREADY_CLAIMED"}}}`, ClaimStatusAlreadyClaimed},
		{"rejected", `{"data":{"claimDropRewards":{"status":"WHATEVER"}}}`, ClaimStatusRejected},
		{"null node", `{"data":{"claimDropRewards":null}}`, ClaimStatusNullResult},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			})
			status, err := c.ClaimDrop(&models.Drop{Name: "Skin", DropInstanceID: "inst-9"})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != tc.wantStatus {
				t.Fatalf("ClaimDrop status = %q, want %q", status, tc.wantStatus)
			}
			if tc.wantStatus == ClaimStatusAccepted && !status.Fresh() {
				t.Fatal("ELIGIBLE_FOR_ALL must be a Fresh accept")
			}
			if tc.wantStatus == ClaimStatusAlreadyClaimed && (status.Fresh() || !status.Accepted()) {
				t.Fatal("already-claimed must be Accepted() but not Fresh()")
			}
		})
	}
}

func TestClaimDropTransientIsError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	status, err := c.ClaimDrop(&models.Drop{Name: "Skin", DropInstanceID: "inst-9"})
	if err == nil {
		t.Fatal("transient failure must return an error")
	}
	if status.Accepted() {
		t.Fatalf("a transient failure must not report an accepted status, got %q", status)
	}
}
