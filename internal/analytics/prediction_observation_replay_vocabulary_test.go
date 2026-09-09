package analytics

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// This file is the producer-side half of the replay's vocabulary contract.
//
// internal/predictioneval deliberately RE-DECLARES the phases and counter keys
// it reads, because importing this package would drag database/sql and the
// whole write path across its purity fence. The reader's own vocabulary test
// pins the exported values — kinds, stage states, health states, readings — but
// the phases and counter keys live in UNEXPORTED allowlists here and cannot be
// reached from outside the package at all. They were therefore unpinned, and
// they are the values whose drift is hardest to notice.
//
// The failure mode is specific and quiet. If the producer renamed AUTO_DECIDED
// or autoAttemptId, the replay would not crash and would not report a mismatch:
// it would find no terminal fact and no attempt discriminator, exclude every
// fact as NO_TERMINAL_FACT or NO_ATTEMPT_ID, and hand back an empty result that
// looks exactly like a session in which nothing happened.
//
// This is an in-package test because that is the only place the allowlists are
// visible. It creates no import cycle: internal/predictioneval imports no
// first-party package, which its dependency fence enforces.

// TestTheReplayVocabularyIsAdmittedByTheProducersAllowlists pins every phase
// and counter key the replay re-declares against the store's closed
// vocabularies.
func TestTheReplayVocabularyIsAdmittedByTheProducersAllowlists(t *testing.T) {
	phases := map[string]bool{}
	for _, p := range observationPhases {
		phases[p] = true
	}
	if len(phases) < 20 {
		t.Fatalf("only %d phases in the store's allowlist; this check is not reading it",
			len(phases))
	}

	for _, tc := range []struct{ what, value string }{
		{"the due phase", predictioneval.PhaseAutoDue},
		{"the decided terminal phase", predictioneval.PhaseAutoDecided},
		{"the skipped terminal phase", predictioneval.PhaseAutoSkipped},
		{"the placement call start", predictioneval.PhaseCallStarted},
		{"the placement call return", predictioneval.PhaseCallReturned},
		{"the delivered terminal phase", predictioneval.PhaseTerminalDelivered},
		{"the admitted terminal phase", predictioneval.PhaseTerminalAdmitted},
	} {
		if !phases[tc.value] {
			t.Errorf("%s: the replay reads %q, which the producer's closed phase vocabulary "+
				"does not contain. The replay would not fail loudly — it would find no fact "+
				"with that phase and report an empty session, which is indistinguishable from "+
				"a session in which nothing happened.", tc.what, tc.value)
		}
	}

	counters := map[string]bool{}
	for _, k := range observationCounterKeys {
		counters[k] = true
	}
	if len(counters) < 10 {
		t.Fatalf("only %d counter keys in the store's allowlist; this check is not reading it",
			len(counters))
	}

	for _, tc := range []struct{ what, value string }{
		// The discriminator is the load-bearing one: it is what links an
		// attempt's facts to each other.
		{"the attempt discriminator", predictioneval.CounterAutoAttemptID},
		{"the stake", predictioneval.CounterStake},
		{"the balance", predictioneval.CounterBalance},
		{"the payout", predictioneval.CounterPayout},
		{"the returned stake", predictioneval.CounterReturnedStake},
	} {
		if !counters[tc.value] {
			t.Errorf("%s: the replay reads the counter %q, which the producer's closed key "+
				"vocabulary does not contain. Every fact would then be excluded as carrying no "+
				"attempt id, and the replay would report nothing rather than a mismatch.",
				tc.what, tc.value)
		}
	}
}

// TestTheProducerStillWritesTheDecisionKindsTheReplayGroups pins the fact
// kinds the replay groups on, from the producer's side.
//
// The reader's test pins these too, by comparing exported constants. This one
// checks the other property: that they are members of the store's own closed
// KIND vocabulary, so a kind that was renamed in the allowlist but left as a
// stale exported constant is still caught.
func TestTheProducerStillWritesTheDecisionKindsTheReplayGroups(t *testing.T) {
	kinds := map[string]bool{}
	for _, k := range observationKinds {
		kinds[k] = true
	}
	if len(kinds) < 5 {
		t.Fatalf("only %d kinds in the store's allowlist; this check is not reading it",
			len(kinds))
	}
	for _, tc := range []struct{ what, value string }{
		{"the automatic decision", predictioneval.KindAutoDecision},
		{"the placement call", predictioneval.KindPlacement},
		{"the round verdict", predictioneval.KindUserTerminal},
	} {
		if !kinds[tc.value] {
			t.Errorf("%s: the replay groups on kind %q, which the producer's closed kind "+
				"vocabulary does not contain", tc.what, tc.value)
		}
	}
}
