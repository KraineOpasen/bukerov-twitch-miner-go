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

func TestCandidateClientIDsDefaultOrder(t *testing.T) {
	// With no per-operation cache entry, candidates start with the promoted
	// default and cover every known fallback exactly once.
	c := &TwitchClient{defaultClientID: constants.ClientIDTV, opClientID: map[string]string{}}
	got := c.candidateClientIDs("ChannelPointsContext")

	if len(got) == 0 || got[0] != constants.ClientIDTV {
		t.Fatalf("expected default client ID %q first, got %v", constants.ClientIDTV, got)
	}
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

func TestCandidateClientIDsCachedFirst(t *testing.T) {
	// A per-operation cached ID is tried first, ahead of the default, and never
	// duplicated.
	c := &TwitchClient{
		defaultClientID: constants.ClientIDTV,
		opClientID:      map[string]string{"ChannelPointsContext": constants.ClientIDMobile},
	}
	got := c.candidateClientIDs("ChannelPointsContext")

	if got[0] != constants.ClientIDMobile {
		t.Fatalf("expected cached client ID %q first, got %v", constants.ClientIDMobile, got)
	}
	seen := map[string]int{}
	for _, id := range got {
		seen[id]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("client ID %q appears %d times, want exactly 1 (candidates: %v)", id, n, got)
		}
	}
	// A different operation with no cache entry still starts from the default.
	other := c.candidateClientIDs("Inventory")
	if other[0] != constants.ClientIDTV {
		t.Errorf("uncached operation should start from the default %q, got %v", constants.ClientIDTV, other)
	}
}

func TestRememberWorkingClientIDPromotesOnlyViaFallback(t *testing.T) {
	c := &TwitchClient{defaultClientID: constants.ClientIDTV, opClientID: map[string]string{}}

	// Success on the first candidate (viaFallback=false) caches per-op but does
	// NOT promote the global default.
	c.rememberWorkingClientID("ChannelPointsContext", constants.ClientIDTV, false)
	if got := c.ActiveClientID(); got != "TV" {
		t.Errorf("default should stay TV after a first-candidate success, got %q", got)
	}
	if c.opClientID["ChannelPointsContext"] != constants.ClientIDTV {
		t.Errorf("expected per-op cache to record TV")
	}

	// A fallback success (viaFallback=true) promotes the default so uncached
	// operations follow the rotation, and the health label reflects it.
	c.rememberWorkingClientID("ChannelPointsContext", constants.ClientIDBrowser, true)
	if got := c.ActiveClientID(); got != "Browser" {
		t.Errorf("default should be promoted to Browser after a fallback success, got %q", got)
	}
	if c.opClientID["ChannelPointsContext"] != constants.ClientIDBrowser {
		t.Errorf("expected per-op cache to record Browser after fallback")
	}
}
