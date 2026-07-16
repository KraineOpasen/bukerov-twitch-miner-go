package pubsub

import (
	"fmt"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
)

// TestHumanizeBetErrorPersistedQueryNotFound: a stale-hash outage
// (ErrPersistedQueryNotFound) must get its own dashboard message — one that
// does not leak internals ("persisted query", client IDs) and does not suggest
// retrying, since every candidate client ID was already tried and the outage
// lasts until the shipped query hashes are updated.
func TestHumanizeBetErrorPersistedQueryNotFound(t *testing.T) {
	// Wrapped the same way postGQLRequest returns it.
	err := fmt.Errorf("%w: operation MakePrediction (tried 3 client IDs)", api.ErrPersistedQueryNotFound)

	got := humanizeBetError(err)

	if strings.Contains(strings.ToLower(got), "persisted query") {
		t.Errorf("message leaks internals: %q", got)
	}
	if strings.Contains(strings.ToLower(got), "try again") {
		t.Errorf("message must not suggest retrying a stale-hash outage: %q", got)
	}
	if !strings.Contains(got, "bet was not placed") {
		t.Errorf("message should state the bet was not placed, got %q", got)
	}
}

// TestHumanizeBetErrorDefaultStillGeneric: errors without a dedicated branch
// keep the generic retry message, proving the new PQNF branch didn't swallow
// the default path.
func TestHumanizeBetErrorDefaultStillGeneric(t *testing.T) {
	got := humanizeBetError(fmt.Errorf("some transport hiccup"))
	if !strings.Contains(got, "try again") {
		t.Errorf("generic errors should keep the retry hint, got %q", got)
	}
}
