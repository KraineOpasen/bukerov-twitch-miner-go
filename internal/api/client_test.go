package api

import "testing"

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
