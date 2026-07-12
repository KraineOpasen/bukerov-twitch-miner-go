package api

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
)

func channelPointsResponse(claimID string) map[string]interface{} {
	availableClaim := interface{}(nil)
	if claimID != "" {
		availableClaim = map[string]interface{}{"id": claimID}
	}
	return map[string]interface{}{
		"data": map[string]interface{}{
			"community": map[string]interface{}{
				"channel": map[string]interface{}{
					"self": map[string]interface{}{
						"communityPoints": map[string]interface{}{
							"availableClaim": availableClaim,
						},
					},
				},
			},
		},
	}
}

func TestAvailableClaimIDFound(t *testing.T) {
	if got := availableClaimID(channelPointsResponse("claim-123")); got != "claim-123" {
		t.Errorf("expected claim-123, got %q", got)
	}
}

func TestAvailableClaimIDNoneAvailable(t *testing.T) {
	if got := availableClaimID(channelPointsResponse("")); got != "" {
		t.Errorf("expected empty claim ID when none available, got %q", got)
	}
}

func TestAvailableClaimIDHandlesMalformedResponses(t *testing.T) {
	cases := []map[string]interface{}{
		nil,
		{},
		{"data": map[string]interface{}{}},
		{"data": map[string]interface{}{"community": nil}},
		{"data": map[string]interface{}{"community": map[string]interface{}{"channel": nil}}},
	}
	for i, resp := range cases {
		if got := availableClaimID(resp); got != "" {
			t.Errorf("case %d: expected empty string for malformed response, got %q", i, got)
		}
	}
}

func TestIsPersistedQueryNotFound(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "single response errors[].message",
			body: `{"errors":[{"message":"PersistedQueryNotFound"}]}`,
			want: true,
		},
		{
			name: "errorType field",
			body: `[{"errors":[{"message":"...","extensions":{"code":"PERSISTED_QUERY_NOT_FOUND"}}],"errorType":"PersistedQueryNotFound"}]`,
			want: true,
		},
		{
			name: "batched response with one failing operation",
			body: `[{"data":{"user":{"id":"1"}}},{"errors":[{"message":"PersistedQueryNotFound"}]}]`,
			want: true,
		},
		{
			name: "normal success",
			body: `{"data":{"user":{"id":"123"}}}`,
			want: false,
		},
		{
			name: "unrelated error",
			body: `{"errors":[{"message":"service unavailable"}]}`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPersistedQueryNotFound([]byte(tc.body)); got != tc.want {
				t.Errorf("isPersistedQueryNotFound(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestClientIDCandidatesStartsWithActive(t *testing.T) {
	active := constants.ClientIDBrowser
	got := clientIDCandidates(active)

	if len(got) == 0 || got[0] != active {
		t.Fatalf("expected active client ID %q first, got %v", active, got)
	}

	// The active ID must not be duplicated, and every known fallback must be
	// present exactly once.
	seen := map[string]int{}
	for _, id := range got {
		seen[id]++
	}
	for _, id := range constants.GQLClientIDFallbacks {
		if seen[id] != 1 {
			t.Errorf("client ID %q appears %d times, want exactly 1 (candidates: %v)", id, seen[id], got)
		}
	}
	if len(got) != len(constants.GQLClientIDFallbacks) {
		t.Errorf("expected %d candidates, got %d (%v)", len(constants.GQLClientIDFallbacks), len(got), got)
	}
}

func TestClientIDCandidatesUnknownActive(t *testing.T) {
	// An active client ID not present in the fallback list should still be
	// tried first, followed by every known fallback.
	active := "some-unknown-client-id"
	got := clientIDCandidates(active)

	if got[0] != active {
		t.Fatalf("expected active client ID %q first, got %v", active, got)
	}
	if len(got) != len(constants.GQLClientIDFallbacks)+1 {
		t.Errorf("expected %d candidates, got %d (%v)", len(constants.GQLClientIDFallbacks)+1, len(got), got)
	}
}
