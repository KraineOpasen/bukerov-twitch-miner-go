package p4offline

// The package's tests are otherwise all EXTERNAL (package p4offline_test), on
// purpose: behaviour is pinned through the public seam so the tests survive
// refactors. This one file is internal, because the property it pins cannot be
// observed from outside — that the streaming merge which replaced the old
// copy-sort-materialize returns exactly what that implementation returned.
//
// A comment in evidence.go used to assert this comparison existed when it did
// not. An independent review lane caught the claim, wrote the comparison, and
// found the equivalence itself true. Rather than delete the sentence, the
// comparison now lives here and the sentence is true.

import (
	"math/rand"
	"sort"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// matchingBeforeTheMerge is the implementation eachMatching replaced, copied
// verbatim from 7091991f. It is the oracle, so it must NOT be tidied.
func (ix signalIndex) matchingBeforeTheMerge(ep EpisodeIdentity) []rawSignal {
	var idx []int
	if ep.EventID != "" {
		idx = append(idx, ix.byEvent[ep.EventID]...)
	}
	idx = append(idx, ix.byPoolRound[poolRound{ep.PoolInstanceID, ep.RoundIncarnationID}]...)
	idx = append(idx, ix.byPoolUnattributed[ep.PoolInstanceID]...)
	sort.Ints(idx)
	out := make([]rawSignal, 0, len(idx))
	for i, k := range idx {
		if i > 0 && idx[i-1] == k {
			continue
		}
		out = append(out, ix.signals[k])
	}
	return out
}

func TestStreamingMergeMatchesTheImplementationItReplaced(t *testing.T) {
	pools := []string{"", "p0", "p1"}
	rounds := []string{"", "r0", "r1", "r2"}
	events := []string{"", "e0", "e1", "e2"}
	kinds := []string{
		predictioneval.KindPlacement, predictioneval.KindAutoDecision,
		predictioneval.KindUserTerminal, "channel_event", "source_unknown",
	}
	phases := []string{
		predictioneval.PhaseCallStarted, predictioneval.PhaseCallReturned,
		predictioneval.PhaseAutoDue, predictioneval.PhaseAutoDecided, "ROUND_CREATED",
	}
	rng := rand.New(rand.NewSource(20260916))

	for ds := 0; ds < 400; ds++ {
		// Half the datasets collide on one event id, which is the adversarial
		// shape the merge exists for; the rest spread across the vocabulary.
		collide := ds%2 == 0
		n := 1 + rng.Intn(40)
		recs := make([]predictioneval.SourceRecord, 0, n)
		known := map[string]bool{}
		for i := 0; i < n; i++ {
			ev := events[rng.Intn(len(events))]
			if collide {
				ev = "e0"
			}
			r := predictioneval.SourceRecord{
				PoolInstanceID:     pools[rng.Intn(len(pools))],
				RoundIncarnationID: rounds[rng.Intn(len(rounds))],
				EventID:            ev,
				CollectorSequence:  int64(i),
			}
			r.Kind = kinds[rng.Intn(len(kinds))]
			r.Payload.Phase = phases[rng.Intn(len(phases))]
			r.PayloadVersion = predictioneval.SupportedPayloadVersion
			recs = append(recs, r)
			if rng.Intn(2) == 0 {
				known[ev] = true
			}
		}
		ix := indexSignals(recs, known)

		// The merge's exactness rests on each posting list being strictly
		// ascending and carrying each index once. indexSignals maintains that;
		// nothing asserted it until now.
		for name, lists := range map[string][][]int{
			"byEvent":            values(ix.byEvent),
			"byPoolRound":        valuesPR(ix.byPoolRound),
			"byPoolUnattributed": values(ix.byPoolUnattributed),
		} {
			for _, l := range lists {
				for i := 1; i < len(l); i++ {
					if l[i] <= l[i-1] {
						t.Fatalf("%s posting list is not strictly ascending: %v", name, l)
					}
				}
			}
		}

		for _, p := range pools {
			for _, r := range rounds {
				for _, e := range events {
					ep := EpisodeIdentity{PoolInstanceID: p, RoundIncarnationID: r, EventID: e}
					want := ix.matchingBeforeTheMerge(ep)
					var got []rawSignal
					ix.eachMatching(ep, func(s rawSignal) { got = append(got, s) })
					if len(got) != len(want) {
						t.Fatalf("dataset %d %+v: merge returned %d signals, the old implementation %d",
							ds, ep, len(got), len(want))
					}
					for i := range want {
						if got[i].rec != want[i].rec {
							t.Fatalf("dataset %d %+v: signal %d differs", ds, ep, i)
						}
					}
				}
			}
		}
	}
}

func values(m map[string][]int) [][]int {
	out := make([][]int, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func valuesPR(m map[poolRound][]int) [][]int {
	out := make([][]int, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
