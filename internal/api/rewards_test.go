package api

import (
	"errors"
	"testing"
)

func TestRedeemErrorForCode(t *testing.T) {
	tests := []struct {
		code    string
		wantErr error // sentinel to match with errors.Is, or nil to just require non-nil
	}{
		{"INSUFFICIENT_POINTS", ErrInsufficientPoints},
		{"NOT_AVAILABLE", ErrRewardUnavailable},
		{"DISABLED", ErrRewardUnavailable},
		{"OUT_OF_STOCK", ErrRewardUnavailable},
		{"COOLDOWN", nil},
		{"PROPERTIES_MISMATCH", nil},
		{"SOMETHING_NEW", nil},
		{"", nil},
	}

	for _, tc := range tests {
		err := redeemErrorForCode(tc.code)
		if err == nil {
			t.Fatalf("code %q: expected a non-nil error", tc.code)
		}
		if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
			t.Errorf("code %q: got %v, want errors.Is %v", tc.code, err, tc.wantErr)
		}
	}
}

func TestRedeemResponseErrorSuccess(t *testing.T) {
	// error explicitly null on the mutation payload => success (nil error).
	resp := map[string]interface{}{
		"data": map[string]interface{}{
			"redeemCommunityPointsCustomReward": map[string]interface{}{
				"error": nil,
			},
		},
	}
	if err := redeemResponseError(resp); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestRedeemResponseErrorPayloadError(t *testing.T) {
	resp := map[string]interface{}{
		"data": map[string]interface{}{
			"redeemCommunityPointsCustomReward": map[string]interface{}{
				"error": map[string]interface{}{"code": "INSUFFICIENT_POINTS"},
			},
		},
	}
	if err := redeemResponseError(resp); !errors.Is(err, ErrInsufficientPoints) {
		t.Fatalf("expected ErrInsufficientPoints, got %v", err)
	}
}

func TestRedeemResponseErrorTopLevel(t *testing.T) {
	resp := map[string]interface{}{
		"errors": []interface{}{
			map[string]interface{}{"message": "PersistedQueryNotFound"},
		},
	}
	if err := redeemResponseError(resp); err == nil {
		t.Fatal("expected an error for top-level GraphQL errors")
	}
}

func TestRedeemResponseErrorNoPayload(t *testing.T) {
	// No payload object and no errors: treated as success rather than a failure.
	resp := map[string]interface{}{"data": map[string]interface{}{}}
	if err := redeemResponseError(resp); err != nil {
		t.Fatalf("expected nil error for empty data, got %v", err)
	}
}
