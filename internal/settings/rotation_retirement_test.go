package settings

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

func TestRuntimeSettingsDoNotExposeRetiredRotationIntervals(t *testing.T) {
	runtime := BuildRuntimeSettings(ptr(config.DefaultConfig()))
	body, err := json.Marshal(runtime)
	if err != nil {
		t.Fatalf("marshal runtime settings: %v", err)
	}

	for _, retired := range [][]byte{
		[]byte(`"rotationInterval"`),
		[]byte(`"rotationIntervalMinMinutes"`),
		[]byte(`"rotationIntervalMaxMinutes"`),
	} {
		if bytes.Contains(body, retired) {
			t.Errorf("runtime settings still expose retired field %s: %s", retired, body)
		}
	}
}

func ptr[T any](value T) *T { return &value }
