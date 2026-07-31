package web

import (
	"encoding/json"
	"testing"
)

// TestSetGenerationPreservesFieldsAndBroadcasts is part of Ф4c: SetGeneration
// must behave like SetConnectionDegraded — set only its own field, leave
// every other published field untouched, and push the update to subscribers.
func TestSetGenerationPreservesFieldsAndBroadcasts(t *testing.T) {
	b := NewStatusBroadcaster()
	b.SetStatus(StatusRunning, "hello")
	b.SetReauthRequired(true, "reauth please")
	b.SetConnectionLost(true, "lost")

	sub := b.Subscribe()
	<-sub // initial push on subscribe

	b.SetGeneration(3)

	got := b.GetStatus()
	if got.Generation != 3 {
		t.Fatalf("Generation = %d, want 3", got.Generation)
	}
	if got.Status != StatusRunning || got.Message != "hello" {
		t.Errorf("SetGeneration mutated Status/Message: %+v", got)
	}
	if !got.ReauthRequired || got.ReauthMessage != "reauth please" {
		t.Errorf("SetGeneration mutated ReauthRequired fields: %+v", got)
	}
	if !got.ConnectionLost || got.ConnectionMessage != "lost" {
		t.Errorf("SetGeneration mutated ConnectionLost fields: %+v", got)
	}

	select {
	case broadcast := <-sub:
		if broadcast.Generation != 3 {
			t.Errorf("broadcast Generation = %d, want 3", broadcast.Generation)
		}
	default:
		t.Error("SetGeneration did not broadcast to subscribers")
	}
}

// TestStatusInfoGenerationOmitEmpty locks the "absent means 1" client
// contract (design v6 §10): a StatusInfo with the zero Generation must
// serialize WITHOUT a "generation" key at all, and a non-zero one must
// serialize with the literal number.
func TestStatusInfoGenerationOmitEmpty(t *testing.T) {
	zero := StatusInfo{Status: StatusRunning}
	b, err := json.Marshal(zero)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := raw["generation"]; present {
		t.Errorf("generation key present for zero value: %s", b)
	}

	nonZero := StatusInfo{Status: StatusRunning, Generation: 2}
	b2, err := json.Marshal(nonZero)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(b2, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(raw["generation"]) != "2" {
		t.Errorf("generation = %s, want 2", raw["generation"])
	}
}
