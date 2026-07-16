package miner

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
)

// TestHumanizeRewardErrorPersistedQueryNotFound: the rewards modal prints
// err.Error() verbatim, so a stale-hash outage must be rewritten into a
// message without internals ("persisted query", client IDs) and without a
// retry hint — retrying cannot help until the shipped query hashes are updated.
func TestHumanizeRewardErrorPersistedQueryNotFound(t *testing.T) {
	err := fmt.Errorf("%w: operation ChannelPointsContext (tried 3 client IDs)", api.ErrPersistedQueryNotFound)

	got := humanizeRewardError(err)
	if got == nil {
		t.Fatal("expected a non-nil error")
	}
	msg := strings.ToLower(got.Error())
	if strings.Contains(msg, "persisted query") || strings.Contains(msg, "client id") {
		t.Errorf("message leaks internals: %q", got.Error())
	}
	if strings.Contains(msg, "try again") {
		t.Errorf("message must not suggest retrying a stale-hash outage: %q", got.Error())
	}
}

// TestHumanizeRewardErrorPassesOthersThrough: every other failure keeps its
// identity so callers (and the modal) still see the api package's friendly
// sentinels unchanged.
func TestHumanizeRewardErrorPassesOthersThrough(t *testing.T) {
	for _, err := range []error{
		api.ErrInsufficientPoints,
		api.ErrRewardUnavailable,
		errors.New("some transport hiccup"),
	} {
		if got := humanizeRewardError(err); got != err {
			t.Errorf("humanizeRewardError(%v) = %v, want the error unchanged", err, got)
		}
	}
}
