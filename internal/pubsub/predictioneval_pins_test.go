package pubsub

// Pins for the constants the offline replay had to duplicate.
//
// internal/predictioneval re-declares two values this package owns, because it
// may not import the pool: the Twitch minimum stake and the store's outcome
// ceiling. A duplicated constant that drifts is worse than no constant at all —
// the replay would keep reporting confident verdicts computed against a
// threshold the miner stopped using — so the duplication is held to the
// original here, in the only package that can see both.
//
// This file is in-package on purpose: minPredictionBet and maxObservedOutcomes
// are unexported, so an external test could not reach them. It adds no
// production API and ships nothing.

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

func TestPinnedMinimumStakeMatchesTheProductionConstant(t *testing.T) {
	if predictioneval.PinnedMinimumStake != minPredictionBet {
		t.Fatalf("predictioneval.PinnedMinimumStake = %d but the pool applies %d.\n"+
			"The offline replay decides BELOW_MINIMUM_POINTS against its own copy of this "+
			"threshold, so a drift here makes every replayed decision near the floor wrong. "+
			"Update the replay's constant deliberately, not by silencing this test.",
			predictioneval.PinnedMinimumStake, minPredictionBet)
	}
}

func TestPinnedOutcomeCeilingMatchesTheProducerCeiling(t *testing.T) {
	if predictioneval.PinnedMaxOutcomes != maxObservedOutcomes {
		t.Fatalf("predictioneval.PinnedMaxOutcomes = %d but the producer's ceiling is %d.\n"+
			"The replay refuses an outcome vector past the ceiling because the store could "+
			"never have persisted it whole; a drift would either refuse valid rounds or "+
			"replay a truncated one as if it were complete.",
			predictioneval.PinnedMaxOutcomes, maxObservedOutcomes)
	}
}
