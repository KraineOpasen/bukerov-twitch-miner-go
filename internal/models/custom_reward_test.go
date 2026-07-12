package models

import (
	"testing"
	"time"
)

func TestCustomRewardFromGQL(t *testing.T) {
	data := map[string]interface{}{
		"id":                  "reward-1",
		"title":               "Hydrate!",
		"prompt":              "Make the streamer drink water",
		"cost":                float64(500),
		"isUserInputRequired": false,
		"isEnabled":           true,
		"isPaused":            false,
		"isInStock":           true,
		"isSubOnly":           false,
		"backgroundColor":     "#FF0000",
		"defaultImage": map[string]interface{}{
			"url1x": "https://img/1x.png",
			"url2x": "https://img/2x.png",
		},
	}

	r := CustomRewardFromGQL(data)
	if r.ID != "reward-1" || r.Title != "Hydrate!" || r.Cost != 500 {
		t.Fatalf("unexpected reward: %+v", r)
	}
	if r.Prompt != "Make the streamer drink water" {
		t.Errorf("prompt not parsed: %q", r.Prompt)
	}
	if r.ImageURL != "https://img/2x.png" {
		t.Errorf("expected 2x image URL, got %q", r.ImageURL)
	}
	if !r.IsAvailable() {
		t.Errorf("reward should be available")
	}
}

func TestCustomRewardImagePrefersCustomImage(t *testing.T) {
	data := map[string]interface{}{
		"id": "r",
		"image": map[string]interface{}{
			"url2x": "https://custom/2x.png",
		},
		"defaultImage": map[string]interface{}{
			"url2x": "https://default/2x.png",
		},
	}
	if got := CustomRewardFromGQL(data).ImageURL; got != "https://custom/2x.png" {
		t.Errorf("expected custom image preferred, got %q", got)
	}
}

func TestCustomRewardIsAvailable(t *testing.T) {
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).Format(time.RFC3339)

	tests := []struct {
		name string
		data map[string]interface{}
		want bool
	}{
		{"enabled in stock", map[string]interface{}{"id": "a", "isEnabled": true, "isInStock": true}, true},
		{"disabled", map[string]interface{}{"id": "a", "isEnabled": false, "isInStock": true}, false},
		{"paused", map[string]interface{}{"id": "a", "isEnabled": true, "isPaused": true, "isInStock": true}, false},
		{"out of stock", map[string]interface{}{"id": "a", "isEnabled": true, "isInStock": false}, false},
		{"on cooldown", map[string]interface{}{"id": "a", "isEnabled": true, "isInStock": true, "cooldownExpiresAt": future}, false},
		{"cooldown expired", map[string]interface{}{"id": "a", "isEnabled": true, "isInStock": true, "cooldownExpiresAt": past}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CustomRewardFromGQL(tc.data).IsAvailable(); got != tc.want {
				t.Errorf("IsAvailable() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCustomRewardFromGQLPartial(t *testing.T) {
	// A malformed/partial payload must not panic and yields a zero-ish reward.
	r := CustomRewardFromGQL(map[string]interface{}{"id": "only-id"})
	if r.ID != "only-id" || r.Cost != 0 || r.IsAvailable() {
		t.Errorf("unexpected reward from partial data: %+v", r)
	}
}
