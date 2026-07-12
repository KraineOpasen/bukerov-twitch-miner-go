package models

import "testing"

func TestNewDropFromGQLExtractsBenefitAndImage(t *testing.T) {
	data := map[string]interface{}{
		"id":                     "drop-1",
		"name":                   "Legendary Skin",
		"requiredMinutesWatched": float64(120),
		"benefitEdges": []interface{}{
			map[string]interface{}{
				"benefit": map[string]interface{}{
					"name":          "Legendary Weapon Skin",
					"imageAssetURL": "https://example.com/skin.png",
				},
			},
		},
	}

	drop := NewDropFromGQL(data)
	if drop.Name != "Legendary Skin" {
		t.Errorf("unexpected name: %q", drop.Name)
	}
	if drop.Benefit != "Legendary Weapon Skin" {
		t.Errorf("unexpected benefit: %q", drop.Benefit)
	}
	if drop.ImageURL != "https://example.com/skin.png" {
		t.Errorf("unexpected image URL: %q", drop.ImageURL)
	}
	if drop.MinutesRequired != 120 {
		t.Errorf("unexpected minutes required: %d", drop.MinutesRequired)
	}
}

func TestNewDropFromGQLMissingImageIsEmpty(t *testing.T) {
	data := map[string]interface{}{
		"name": "No Image Drop",
		"benefitEdges": []interface{}{
			map[string]interface{}{
				"benefit": map[string]interface{}{"name": "Some Reward"},
			},
		},
	}

	drop := NewDropFromGQL(data)
	if drop.ImageURL != "" {
		t.Errorf("expected empty image URL when absent, got %q", drop.ImageURL)
	}
}
