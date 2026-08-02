package miner

import (
	"log/slog"
	"testing"
)

// R7 F1: claimDropsOnStartup is a deprecated compatibility no-op (see the
// doc comment on config.Config.ClaimDropsOnStartup). setupComponents no
// longer reads it at all, so component setup must complete identically and
// emit no drop-claim-related log for either value. Drop claiming during the
// first full sync is unconditional and already covered by internal/drops's
// own full-sync tests (e.g. TestStartupRunsSingleFullSync,
// TestFirstFullSyncPublishesOneInfoSummary) -- this seam only proves the
// retired flag no longer gates or announces any startup behavior; it does
// not re-prove unconditional claiming itself.
func TestClaimDropsOnStartupTrueIsNoOp(t *testing.T) {
	assertClaimDropsOnStartupIsNoOp(t, true)
}

func TestClaimDropsOnStartupFalseIsNoOp(t *testing.T) {
	assertClaimDropsOnStartupIsNoOp(t, false)
}

func assertClaimDropsOnStartupIsNoOp(t *testing.T, claim bool) {
	t.Helper()

	logCap := &captureHandler{}
	prevLog := slog.Default()
	slog.SetDefault(slog.New(logCap))
	defer slog.SetDefault(prevLog)

	m, _ := newStartupCleanupMiner(t)
	m.config.ClaimDropsOnStartup = claim
	runToNormalCompletion(t, m)

	requireComponentSetupCompleted(t, m)

	if logCap.has("Claiming all drops from inventory on startup") {
		t.Error("claimDropsOnStartup must no longer produce a log message implying it initiated or enabled claiming")
	}
}
