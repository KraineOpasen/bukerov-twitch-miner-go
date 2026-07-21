package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

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
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{"claim":{"id":"x"}}}}`)
	})
	if err := c.ClaimBonus(newTestStreamer("s"), "claim-abc"); err != nil {
		t.Fatalf("accepted claim must return nil, got %v", err)
	}
}

func TestClaimBonusNullResultIsNotSuccess(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":null}}`)
	})
	err := c.ClaimBonus(newTestStreamer("s"), "claim-abc")
	if err == nil {
		t.Fatal("null business-result node must NOT be treated as success")
	}
	if !errors.Is(err, ErrClaimNotAccepted) {
		t.Fatalf("want ErrClaimNotAccepted, got %v", err)
	}
}

// TestEmptyCommunityPointsResultIsMalformedAndRetryable pins the burden-of-proof
// decision: an empty business-result object is non-authoritative (malformed),
// not accepted, and stays retryable through the existing bounded paths.
func TestEmptyCommunityPointsResultIsMalformedAndRetryable(t *testing.T) {
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
// claimCommunityPoints object makes ClaimBonus return ErrClaimNotAccepted with no
// success log/event, and leaks neither the claim ID nor the token into logs.
func TestClaimBonusEmptyResultIsNotSuccess(t *testing.T) {
	buf := captureAPILogs(t)
	const secretClaimID = "SECRET-CLAIM-ID-empty"
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"claimCommunityPoints":{}}}`)
	})
	err := c.ClaimBonus(newTestStreamer("s"), secretClaimID)
	if err == nil {
		t.Fatal("empty business-result object must NOT be treated as success")
	}
	if !errors.Is(err, ErrClaimNotAccepted) {
		t.Fatalf("want ErrClaimNotAccepted, got %v", err)
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
	if err := c.ClaimBonus(newTestStreamer("s"), "claim-abc"); !errors.Is(err, ErrClaimNotAccepted) {
		t.Fatalf("mutation-level rejection must not be success, got %v", err)
	}
}

func TestClaimBonusUnauthorizedHTTP(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"Unauthorized"}`)
	})
	if err := c.ClaimBonus(newTestStreamer("s"), "claim-abc"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("HTTP 401 must map to ErrUnauthorized, got %v", err)
	}
}

func TestClaimBonusUnauthorizedGraphQL(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"errors":[{"message":"Unauthorized"}]}`)
	})
	if err := c.ClaimBonus(newTestStreamer("s"), "claim-abc"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("GraphQL unauthorized must map to ErrUnauthorized, got %v", err)
	}
}

// TestClaimBonusTransientExhaustedIsError covers a transient transport failure
// (HTTP 5xx) that survives the bounded retries: it must surface as an error, not
// a silent success. (Exercises the real backoff, so it is a few seconds long.)
func TestClaimBonusTransientExhaustedIsError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	err := c.ClaimBonus(newTestStreamer("s"), "claim-abc")
	if err == nil {
		t.Fatal("a transient failure that exhausts retries must return an error")
	}
	if errors.Is(err, ErrClaimNotAccepted) {
		t.Fatalf("transport failure must not be reported as a not-accepted claim, got %v", err)
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
	claimed, err := c.ClaimAvailableBonus(newTestStreamer("s"))
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
	claimed, err := c.ClaimAvailableBonus(newTestStreamer("s"))
	if claimed {
		t.Fatal("polling fallback must NOT report success when the claim returned a null result")
	}
	if !errors.Is(err, ErrClaimNotAccepted) {
		t.Fatalf("want ErrClaimNotAccepted from the fallback, got %v", err)
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
	claimed, err := c.ClaimAvailableBonus(newTestStreamer("s"))
	if claimed {
		t.Fatal("polling fallback must NOT report success when the claim returned an empty object")
	}
	if !errors.Is(err, ErrClaimNotAccepted) {
		t.Fatalf("want ErrClaimNotAccepted from the fallback, got %v", err)
	}
}

func TestClaimAvailableBonusNothingToClaim(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"community":{"channel":{"self":{"communityPoints":{"availableClaim":null}}}}}}`)
	})
	claimed, err := c.ClaimAvailableBonus(newTestStreamer("s"))
	if claimed || err != nil {
		t.Fatalf("no available claim must be (false, nil), got claimed=%v err=%v", claimed, err)
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
	_ = c.ClaimBonus(newTestStreamer("s"), secretClaimID)

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
