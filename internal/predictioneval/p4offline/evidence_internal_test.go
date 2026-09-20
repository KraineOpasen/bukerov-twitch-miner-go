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
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
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

// eachMatching is the streaming three-way merge that visits the signals
// matched to one episode, in index order and without duplicates. It was this
// package's production path until the per-posting-list aggregates replaced it,
// and it is kept HERE, verbatim, as the oracle the aggregates are compared
// against — the same role matchingBeforeTheMerge plays for it. Like that one,
// it must NOT be tidied.
func (ix signalIndex) eachMatching(ep EpisodeIdentity, visit func(rawSignal)) {
	var a []int
	if ep.EventID != "" {
		a = ix.byEvent[ep.EventID]
	}
	b := ix.byPoolRound[poolRound{ep.PoolInstanceID, ep.RoundIncarnationID}]
	c := ix.byPoolUnattributed[ep.PoolInstanceID]
	for i, j, k := 0, 0, 0; i < len(a) || j < len(b) || k < len(c); {
		// The minimum of the live heads, tracked with a FOUND flag rather than
		// a sentinel value. A sentinel would be a value a posting list could in
		// principle hold, and if it ever did, no head would compare below it,
		// no pointer would advance and this loop would not terminate. Indices
		// come from ranging over the signals so that cannot happen today; the
		// flag costs nothing and removes the "today" from that sentence.
		n, found := 0, false
		if i < len(a) && (!found || a[i] < n) {
			n, found = a[i], true
		}
		if j < len(b) && (!found || b[j] < n) {
			n, found = b[j], true
		}
		if k < len(c) && (!found || c[k] < n) {
			n, found = c[k], true
		}
		if !found {
			return
		}
		// Every list holding the minimum advances past it, so an index carried
		// by two lists is visited once and the next minimum is strictly
		// greater.
		if i < len(a) && a[i] == n {
			i++
		}
		if j < len(b) && b[j] == n {
			j++
		}
		if k < len(c) && c[k] == n {
			k++
		}
		visit(ix.signals[n])
	}
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

// ---------------------------------------------------------------------------
// The aggregate path, differentially.
//
// episodeSignals answers, from the per-posting-list aggregates, what the visit
// body used to answer by walking every match. The two must agree on everything
// the selection emits: WHICH manual entries, in which ORDER, after which
// deduplication; WHICH call is the earliest, including the tie-breaks; whether
// the episode collected an unattributed intervention; and the coverage
// reasons IN THE ORDER a walk appended them.
//
// The order is not a detail. The reasons are an exported list built by
// appendOnce during an ascending walk, so a reordering is an output change
// that no equality-of-sets assertion would catch. That is why the oracle below
// is the walk itself, copied verbatim, rather than a re-derivation.
// ---------------------------------------------------------------------------

// episodeOutcome is the four things one episode's matched signals establish,
// in the form the selection emits them.
type episodeOutcome struct {
	manual       []string
	call         CallSignal
	haveCall     bool
	intervention bool
	coverage     NoCallCoverageProof
}

// outcomeByWalk is the visit body episodeSignals replaced, copied verbatim
// from 5551a23 and changed only to record its results instead of writing them
// into an EpisodeSelection. It is the oracle, so its comparisons and their
// ORDER must NOT be tidied.
func (ix signalIndex) outcomeByWalk(ep EpisodeIdentity, attemptIDs map[int64]bool) episodeOutcome {
	out := episodeOutcome{coverage: NoCallCoverageProof{Proven: true}}
	seenManual := map[string]bool{}
	var earliest CallSignal
	haveCall := false
	ix.eachMatching(ep, func(sig rawSignal) {
		if sig.manual && !seenManual[sig.rec.ObservationID] {
			seenManual[sig.rec.ObservationID] = true
			out.manual = append(out.manual, sig.rec.ObservationID)
		}
		if sig.call {
			c := CallSignal{
				Position: sig.rec.CollectorSequence, ObservationID: sig.rec.ObservationID, Kind: sig.callKind,
			}
			if !haveCall || c.Position < earliest.Position ||
				(c.Position == earliest.Position && c.ObservationID < earliest.ObservationID) {
				earliest, haveCall = c, true
			}
			if sig.callKind == CallKindAuto {
				if id := sig.attemptID; !attemptIDs[id] || !sameAttemptScopeRec(sig.rec, ep) {
					out.intervention = true
				}
			} else if !sig.manual {
				out.intervention = true
			}
		}
		if sig.orphanReturn {
			out.coverage.Proven = false
			out.coverage.Reasons = appendOnce(out.coverage.Reasons, CoverageOrphanCallReturned)
		}
		if sig.unclassified {
			out.coverage.Proven = false
			out.coverage.Reasons = appendOnce(out.coverage.Reasons, CoverageUnclassifiedFact)
		}
		if sig.unspentStart &&
			sig.rec.PoolInstanceID == ep.PoolInstanceID &&
			sig.rec.RoundIncarnationID == ep.RoundIncarnationID {
			out.coverage.Proven = false
			out.coverage.Reasons = appendOnce(out.coverage.Reasons, CoverageUnclassifiedFact)
		}
		if sig.undecodable {
			out.coverage.Proven = false
			out.coverage.Reasons = appendOnce(out.coverage.Reasons, CoverageUndecodableFact)
		}
	})
	out.call, out.haveCall = earliest, haveCall
	return out
}

// sameAttemptScopeRec is the RECORD-shaped scope predicate the walk used,
// before the aggregates reduced the matched automatic calls to their distinct
// attempt identities. It belongs to the oracle and must not be tidied either.
func sameAttemptScopeRec(r *predictioneval.SourceRecord, ep EpisodeIdentity) bool {
	return r.CollectorEpoch == ep.CollectorEpoch && r.CollectorSessionID == ep.CollectorSessionID &&
		r.PoolInstanceID == ep.PoolInstanceID
}

// outcomeByAggregate is what the selection now does: read the summary, then
// emit the manual entries through the same deduplication the caller applies.
func (ix signalIndex) outcomeByAggregate(ep EpisodeIdentity, attemptIDs map[int64]bool) episodeOutcome {
	sum := ix.episodeSignals(ep, attemptIDs)
	out := episodeOutcome{intervention: sum.intervention, coverage: sum.coverage}
	seen := map[string]bool{}
	sum.eachManualIndex(func(n int) bool {
		obs := ix.signals[n].rec.ObservationID
		if !seen[obs] {
			seen[obs] = true
			out.manual = append(out.manual, obs)
		}
		return true
	})
	if sum.call >= 0 {
		sig := ix.signals[sum.call]
		out.call = CallSignal{
			Position: sig.rec.CollectorSequence, ObservationID: sig.rec.ObservationID, Kind: sig.callKind,
		}
		out.haveCall = true
	}
	return out
}

// randomSignalDataset builds one adversarial record set. The vocabularies are
// deliberately tiny so collisions are the common case rather than the rare one:
// repeated observation ids drive the deduplication and the call tie-break,
// repeated collector sequences drive the minimum's second and third
// comparisons, and one shared event id drives the shape the aggregates exist
// for.
func randomSignalDataset(rng *rand.Rand, collide bool) ([]predictioneval.SourceRecord, map[string]bool) {
	pools := []string{"", "p0", "p1"}
	rounds := []string{"", "r0", "r1"}
	events := []string{"", "e0", "e1"}
	kinds := []string{
		predictioneval.KindPlacement, predictioneval.KindAutoDecision,
		predictioneval.KindUserTerminal, kindManualControl, "channel_event", "source_unknown",
	}
	phases := []string{
		predictioneval.PhaseCallStarted, predictioneval.PhaseCallReturned,
		predictioneval.PhaseAutoDue, predictioneval.PhaseAutoDecided,
		predictioneval.PhaseAutoSkipped, "ROUND_CREATED",
	}
	yes, no := true, false
	n := 1 + rng.Intn(30)
	recs := make([]predictioneval.SourceRecord, 0, n)
	known := map[string]bool{}
	for i := 0; i < n; i++ {
		ev := events[rng.Intn(len(events))]
		if collide {
			ev = "e0"
		}
		r := predictioneval.SourceRecord{
			CollectorEpoch:     int64(1 + rng.Intn(2)),
			CollectorSessionID: []string{"s0", "s1"}[rng.Intn(2)],
			PoolInstanceID:     pools[rng.Intn(len(pools))],
			RoundIncarnationID: rounds[rng.Intn(len(rounds))],
			EventID:            ev,
			// Sequences and ids repeat on purpose.
			CollectorSequence:  int64(rng.Intn(5)),
			ObservationID:      "o" + strconv.Itoa(rng.Intn(6)),
			Kind:               kinds[rng.Intn(len(kinds))],
			PayloadUndecodable: rng.Intn(6) == 0,
		}
		r.Payload.Phase = phases[rng.Intn(len(phases))]
		r.PayloadVersion = predictioneval.SupportedPayloadVersion
		if rng.Intn(8) == 0 {
			r.PayloadVersion = predictioneval.SupportedPayloadVersion + 1
		}
		switch rng.Intn(3) {
		case 0:
			r.Payload.Manual = &yes
		case 1:
			r.Payload.Manual = &no
		}
		if id := int64(rng.Intn(4)); id > 0 {
			r.Payload.Counters = map[string]int64{predictioneval.CounterAutoAttemptID: id}
		}
		recs = append(recs, r)
		if rng.Intn(2) == 0 {
			known[ev] = true
		}
	}
	return recs, known
}

func TestAggregateSummaryMatchesTheWalkItReplaced(t *testing.T) {
	rng := rand.New(rand.NewSource(20260918))
	pools := []string{"", "p0", "p1"}
	rounds := []string{"", "r0", "r1"}
	events := []string{"", "e0", "e1"}
	// Every subset of the attempt ids the generator emits, so the one
	// episode-dependent branch is driven both ways on the same dataset.
	attemptSets := []map[int64]bool{
		{},
		{1: true},
		{2: true},
		{1: true, 2: true, 3: true},
	}
	sawManual, sawCall, sawIntervention, sawCoverage := false, false, false, false
	for ds := 0; ds < 600; ds++ {
		recs, known := randomSignalDataset(rng, ds%2 == 0)
		ix := indexSignals(recs, known)
		for _, p := range pools {
			for _, r := range rounds {
				for _, e := range events {
					for _, epoch := range []int64{1, 2} {
						for _, sess := range []string{"s0", "s1"} {
							for _, ids := range attemptSets {
								ep := EpisodeIdentity{
									CollectorEpoch: epoch, CollectorSessionID: sess,
									PoolInstanceID: p, RoundIncarnationID: r, EventID: e,
								}
								want := ix.outcomeByWalk(ep, ids)
								got := ix.outcomeByAggregate(ep, ids)
								if !sameOutcome(want, got) {
									t.Fatalf("dataset %d %+v ids=%v:\n walk      %+v\n aggregate %+v",
										ds, ep, ids, want, got)
								}
								sawManual = sawManual || len(want.manual) > 0
								sawCall = sawCall || want.haveCall
								sawIntervention = sawIntervention || want.intervention
								sawCoverage = sawCoverage || len(want.coverage.Reasons) > 1
							}
						}
					}
				}
			}
		}
	}
	// A differential test that drove none of the branches proves nothing, and
	// the generator is random: assert it reached them.
	if !sawManual || !sawCall || !sawIntervention || !sawCoverage {
		t.Fatalf("the generator did not reach every branch: manual=%v call=%v intervention=%v multi-reason coverage=%v",
			sawManual, sawCall, sawIntervention, sawCoverage)
	}
}

func sameOutcome(a, b episodeOutcome) bool {
	if a.haveCall != b.haveCall || a.call != b.call || a.intervention != b.intervention {
		return false
	}
	if a.coverage.Proven != b.coverage.Proven || len(a.coverage.Reasons) != len(b.coverage.Reasons) {
		return false
	}
	for i := range a.coverage.Reasons {
		if a.coverage.Reasons[i] != b.coverage.Reasons[i] {
			return false
		}
	}
	if len(a.manual) != len(b.manual) {
		return false
	}
	for i := range a.manual {
		if a.manual[i] != b.manual[i] {
			return false
		}
	}
	return true
}

// TestAggregatePathDoesNotRepeatEpisodeIndependentWork is the WORK half of the
// same change, and it is counted rather than timed: a wall-clock threshold on
// shared CI is flaky and proves nothing about complexity.
//
// The two numbers are the matches a walk would visit, and the items the
// aggregate path actually inspects per episode. On the shape that motivated the
// removed visit ceiling -- one undecodable AUTO_DUE per incarnation, all on one
// event id -- every episode matches every record, so the walk is |episodes| x
// |records| and NONE of those visits reaches an episode-dependent branch. The
// aggregate path inspects nothing there.
//
// The second shape is the honest counterpart: manual facts ARE reported, so the
// work that remains is the output, one inspection per reported entry. That is
// what the output budget bounds, and it is stated here rather than claimed
// away.
//
// WHAT THE COUNTER IS, precisely, because "inspects 0" reads as "does nothing":
// it counts the items whose number is proportional to POSTING-LIST LENGTH --
// merge visits and distinct attempt identities. The fixed per-episode work
// inside episodeSignals (three map lookups, the scope lookup, the four keeps,
// a sort over at most three marks) is not counted and is not meant to be; it
// does not grow with the matched set, which is the property under test. The
// identity count is also a conservative UPPER bound: production settles on the
// first failing identity and this sums them all.
func TestAggregatePathDoesNotRepeatEpisodeIndependentWork(t *testing.T) {
	// visits counts what a walk would have done; inspected counts what the
	// aggregate path does.
	measure := func(recs []predictioneval.SourceRecord, eps []EpisodeIdentity, ids map[int64]bool) (visits, inspected int) {
		known := map[string]bool{}
		for i := range recs {
			if recs[i].EventID != "" {
				known[recs[i].EventID] = true
			}
		}
		ix := indexSignals(recs, known)
		for _, ep := range eps {
			ix.eachMatching(ep, func(rawSignal) { visits++ })
			sum := ix.episodeSignals(ep, ids)
			sum.eachManualIndex(func(int) bool { inspected++; return true })
			for _, l := range sum.manual {
				_ = l
			}
			inspected += ix.autoCallIdentitiesFor(ep)
		}
		return visits, inspected
	}

	undecodable := func(k int) ([]predictioneval.SourceRecord, []EpisodeIdentity) {
		recs := make([]predictioneval.SourceRecord, 0, k)
		eps := make([]EpisodeIdentity, 0, k)
		for i := 0; i < k; i++ {
			r := predictioneval.SourceRecord{
				PoolInstanceID: "p0", RoundIncarnationID: "r" + strconv.Itoa(i), EventID: "e1",
				CollectorSequence: int64(i), ObservationID: "o" + strconv.Itoa(i),
				Kind: predictioneval.KindAutoDecision, PayloadUndecodable: true,
				PayloadVersion: predictioneval.SupportedPayloadVersion,
			}
			r.Payload.Phase = predictioneval.PhaseAutoDue
			recs = append(recs, r)
			eps = append(eps, EpisodeIdentity{PoolInstanceID: "p0", RoundIncarnationID: "r" + strconv.Itoa(i), EventID: "e1"})
		}
		return recs, eps
	}

	for _, k := range []int{200, 400} {
		recs, eps := undecodable(k)
		visits, inspected := measure(recs, eps, map[int64]bool{})
		if visits != k*k {
			t.Fatalf("k=%d: a walk visits every episode-record pair: got %d, want %d", k, visits, k*k)
		}
		if inspected != 0 {
			t.Fatalf("k=%d: no episode-dependent item exists on this shape, yet %d were inspected", k, inspected)
		}
	}

	// The output-dependent counterpart: k episodes each reporting k manual
	// entries. The walk still visits k*k; the aggregate path inspects exactly
	// the k*k entries it REPORTS, and not one more.
	manual := func(k int) ([]predictioneval.SourceRecord, []EpisodeIdentity) {
		recs := make([]predictioneval.SourceRecord, 0, 2*k)
		eps := make([]EpisodeIdentity, 0, k)
		yes := true
		for i := 0; i < k; i++ {
			r := predictioneval.SourceRecord{
				PoolInstanceID: "p0", RoundIncarnationID: "r" + strconv.Itoa(i), EventID: "e1",
				CollectorSequence: int64(i), ObservationID: "a" + strconv.Itoa(i),
				Kind: predictioneval.KindAutoDecision, PayloadVersion: predictioneval.SupportedPayloadVersion,
			}
			r.Payload.Phase = predictioneval.PhaseAutoDue
			recs = append(recs, r)
			eps = append(eps, EpisodeIdentity{PoolInstanceID: "p0", RoundIncarnationID: "r" + strconv.Itoa(i), EventID: "e1"})
		}
		for i := 0; i < k; i++ {
			m := predictioneval.SourceRecord{
				PoolInstanceID: "p0", EventID: "e1",
				CollectorSequence: int64(k + i), ObservationID: "m" + strconv.Itoa(i),
				Kind: kindManualControl, PayloadVersion: predictioneval.SupportedPayloadVersion,
			}
			m.Payload.Phase = predictioneval.PhaseCallStarted
			m.Payload.Manual = &yes
			recs = append(recs, m)
		}
		return recs, eps
	}
	for _, k := range []int{200, 400} {
		recs, eps := manual(k)
		visits, inspected := measure(recs, eps, map[int64]bool{})
		if visits != k*2*k {
			t.Fatalf("k=%d: walk visits %d, want %d", k, visits, k*2*k)
		}
		if inspected != k*k {
			t.Fatalf("k=%d: the work that remains must be the output: inspected %d, reported %d", k, inspected, k*k)
		}
	}
}

// autoCallIdentitiesFor counts the DISTINCT automatic attempt identities the
// episode's aggregates make it test — the one episode-dependent inspection the
// aggregates compress rather than remove.
func (ix signalIndex) autoCallIdentitiesFor(ep EpisodeIdentity) int {
	n := 0
	if ep.EventID != "" {
		if a := ix.eventAggregate(ep.EventID); a != nil {
			n += len(a.autoCalls)
		}
	}
	if a := ix.poolRoundAggregate(poolRound{ep.PoolInstanceID, ep.RoundIncarnationID}); a != nil {
		n += len(a.autoCalls)
	}
	if a := ix.poolUnattributedAggregate(ep.PoolInstanceID); a != nil {
		n += len(a.autoCalls)
	}
	return n
}

// ---------------------------------------------------------------------------
// The output budget's CHECK POINT.
//
// The external suite pins the literal budget (below it, exactly at it, past
// it) through SelectEpisodes. What it cannot pin there is WHEN the check
// happens, because demonstrating that through the public seam would need a
// single episode matching more than 1,048,576 manual facts -- a million source
// records, which is the very allocation the budget exists to avoid making in
// CI as much as in production. The budget is a parameter of emitManualSignals
// for exactly that reason, so the timing can be demonstrated at any size.
// ---------------------------------------------------------------------------

// manualFactsOnOnePool builds n manual facts that name no incarnation, so they
// are matched to every episode on the pool, plus the index over them.
func manualFactsOnOnePool(n int, dupes bool) (signalIndex, episodeSignals) {
	yes := true
	recs := make([]predictioneval.SourceRecord, 0, n)
	for i := 0; i < n; i++ {
		id := "m" + strconv.Itoa(i)
		if dupes {
			id = "m" + strconv.Itoa(i/2)
		}
		r := predictioneval.SourceRecord{
			PoolInstanceID: "p0", CollectorSequence: int64(i), ObservationID: id,
			Kind: kindManualControl, PayloadVersion: predictioneval.SupportedPayloadVersion,
		}
		r.Payload.Phase = predictioneval.PhaseCallStarted
		r.Payload.Manual = &yes
		recs = append(recs, r)
	}
	ix := indexSignals(recs, map[string]bool{})
	ep := EpisodeIdentity{PoolInstanceID: "p0", RoundIncarnationID: "r0"}
	return ix, ix.episodeSignals(ep, map[int64]bool{})
}

func TestManualBudgetStopsBeforeTheEntryThatWouldExceedIt(t *testing.T) {
	t.Run("under the budget every entry is reported", func(t *testing.T) {
		ix, sum := manualFactsOnOnePool(10, false)
		got, total, over := ix.emitManualSignals(sum, nil, 0, 16)
		if over || total != 10 || len(got) != 10 {
			t.Fatalf("want 10 entries and no overrun, got %d/%d over=%v", len(got), total, over)
		}
	})

	t.Run("at exactly the budget processing still succeeds", func(t *testing.T) {
		// THE BOUNDARY, and the sense of the comparison is the whole test: a
		// check written one off would refuse the last admissible entry.
		ix, sum := manualFactsOnOnePool(16, false)
		got, total, over := ix.emitManualSignals(sum, nil, 0, 16)
		if over {
			t.Fatal("at the exact budget, otherwise-valid processing must be supported")
		}
		if total != 16 || len(got) != 16 {
			t.Fatalf("want the 16th entry reported, got %d/%d", len(got), total)
		}
	})

	t.Run("one past the budget stops on that entry", func(t *testing.T) {
		ix, sum := manualFactsOnOnePool(17, false)
		got, total, over := ix.emitManualSignals(sum, nil, 0, 16)
		if !over {
			t.Fatal("the 17th entry must not be reported against a budget of 16")
		}
		if total != 16 || len(got) != 16 {
			t.Fatalf("nothing past the budget may be appended: %d/%d", len(got), total)
		}
	})

	t.Run("duplicates do not consume budget", func(t *testing.T) {
		// 32 facts carrying 16 distinct observation ids. The budget counts
		// what is REPORTED, so this is exactly at the budget, not twice it.
		ix, sum := manualFactsOnOnePool(32, true)
		got, total, over := ix.emitManualSignals(sum, nil, 0, 16)
		if over {
			t.Fatalf("a repeated observation id is reported once and must not be charged twice: %d", total)
		}
		if len(got) != 16 {
			t.Fatalf("want 16 distinct entries, got %d", len(got))
		}
	})

	t.Run("the running total carries across episodes", func(t *testing.T) {
		// The accumulation half: a second episode's entries are charged to the
		// same budget, and the refusal can fall in the middle of one.
		ix, sum := manualFactsOnOnePool(10, false)
		got, total, over := ix.emitManualSignals(sum, nil, 12, 16)
		if !over {
			t.Fatal("12 + 10 exceeds 16 and must be refused")
		}
		if total != 16 || len(got) != 4 {
			t.Fatalf("want 4 more entries to exactly fill the budget, got %d/%d", len(got), total)
		}
	})
}

// TestOversizedEpisodeIsRefusedWithoutMaterializingItsList is the correction
// this round was given by name: "a single large episode must not materialize
// its entire over-budget list before discovering that it exceeds the budget".
//
// The old check added len(ep.ManualSignals) to the running total AFTER the
// episode's list was built, so the list had to exist first. Here ONE episode
// matches 200,000 manual facts against a budget of 16, and what the emission
// allocates is measured: if the list were built first it would cost at least
// 200,000 string headers, 3.2 MB.
func TestOversizedEpisodeIsRefusedWithoutMaterializingItsList(t *testing.T) {
	const facts, budget = 200000, 16
	ix, sum := manualFactsOnOnePool(facts, false)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, total, over := ix.emitManualSignals(sum, nil, 0, budget)
	runtime.ReadMemStats(&after)

	if !over || total != budget || len(got) != budget {
		t.Fatalf("want a refusal at exactly the budget, got %d/%d over=%v", len(got), total, over)
	}
	grew := after.TotalAlloc - before.TotalAlloc
	// The whole list would be 200,000 * 16 bytes of headers alone. A generous
	// ceiling well under that says the list was never built: what this must
	// allocate is a handful of entries and one small map.
	const ceiling = 64 << 10
	if grew > ceiling {
		t.Fatalf("refusing an over-budget episode allocated %d bytes; the budgeted entries need far less, "+
			"so the over-budget list was materialized", grew)
	}
}

// TestManualMergeStopsWhenTheCallerSaysStop pins the mechanism the budget uses
// to refuse without materializing: the merge must ABANDON the walk at the first
// false, not run it out and discard the rest.
//
// It is here rather than at emitManualSignals because that is where it is
// observable: the caller counts what it was handed, and a merge that ignored
// the answer would hand it the whole list.
func TestManualMergeStopsWhenTheCallerSaysStop(t *testing.T) {
	_, sum := manualFactsOnOnePool(500, false)
	seen := 0
	sum.eachManualIndex(func(int) bool {
		seen++
		return seen < 4
	})
	if seen != 4 {
		t.Fatalf("the merge must stop on the visit that returned false: it made %d visits of a possible 500", seen)
	}
	// And the control: when nothing says stop, every matched manual signal is
	// visited exactly once.
	all := 0
	sum.eachManualIndex(func(int) bool { all++; return true })
	if all != 500 {
		t.Fatalf("an unstopped merge visits every matched manual signal once: %d", all)
	}
}

// TestRepeatedObservationIdsDoNotMakeTheEmissionQuadratic pins the correction an
// independent lane forced: the work that remains after the aggregates must be
// proportional to what is REPORTED, and a duplicate is not reported.
//
// A duplicate observation id is deduplicated and never consumes budget, so it
// cannot stop the merge either. Before the per-posting-list deduplication, a
// dataset repeating one id therefore made every episode walk the whole matched
// manual set to report a single entry -- the lane measured 4,000,000 merge
// visits for 2,000 reported entries, on a shape the removed visit ceiling used
// to refuse. This asserts the shape of that work, counted rather than timed.
func TestRepeatedObservationIdsDoNotMakeTheEmissionQuadratic(t *testing.T) {
	// k episodes on one event id, and k manual facts every one of them matches.
	build := func(k int, oneID bool) signalIndex {
		yes := true
		recs := make([]predictioneval.SourceRecord, 0, 2*k)
		for i := 0; i < k; i++ {
			r := predictioneval.SourceRecord{
				PoolInstanceID: "p0", RoundIncarnationID: "r" + strconv.Itoa(i), EventID: "e1",
				CollectorSequence: int64(i), ObservationID: "a" + strconv.Itoa(i),
				Kind: predictioneval.KindAutoDecision, PayloadVersion: predictioneval.SupportedPayloadVersion,
			}
			r.Payload.Phase = predictioneval.PhaseAutoDue
			recs = append(recs, r)
		}
		for j := 0; j < k; j++ {
			id := "m" + strconv.Itoa(j)
			if oneID {
				id = "m0"
			}
			m := predictioneval.SourceRecord{
				PoolInstanceID: "p0", EventID: "e1",
				CollectorSequence: int64(k + j), ObservationID: id,
				Kind: kindManualControl, PayloadVersion: predictioneval.SupportedPayloadVersion,
			}
			m.Payload.Phase = predictioneval.PhaseCallStarted
			m.Payload.Manual = &yes
			recs = append(recs, m)
		}
		return indexSignals(recs, map[string]bool{"e1": true})
	}
	count := func(ix signalIndex, k int) (visits, reported int) {
		for i := 0; i < k; i++ {
			ep := EpisodeIdentity{PoolInstanceID: "p0", RoundIncarnationID: "r" + strconv.Itoa(i), EventID: "e1"}
			sum := ix.episodeSignals(ep, map[int64]bool{})
			seen := map[string]bool{}
			sum.eachManualIndex(func(n int) bool {
				visits++
				if obs := ix.signals[n].rec.ObservationID; !seen[obs] {
					seen[obs] = true
					reported++
				}
				return true
			})
		}
		return visits, reported
	}

	for _, k := range []int{200, 400} {
		visits, reported := count(build(k, true), k)
		if reported != k {
			t.Fatalf("k=%d: every episode reports the one distinct id once: %d", k, reported)
		}
		// The whole point: k, not k*k.
		if visits != k {
			t.Fatalf("k=%d: %d merge visits to report %d entries -- the emission is walking the "+
				"matched set instead of the report", k, visits, reported)
		}
	}
	// The control: with DISTINCT ids every visit is a reported entry, so the
	// work is the output and the output budget is what bounds it.
	for _, k := range []int{200, 400} {
		visits, reported := count(build(k, false), k)
		if reported != k*k || visits != reported {
			t.Fatalf("k=%d: want %d reported and the same number of visits, got %d/%d",
				k, k*k, reported, visits)
		}
	}
}

// TestManualMergeVisitsASharedIndexOnce pins the property the merge's own
// comment rests on, on a fixture that can actually observe it.
//
// A record carrying BOTH a known event id AND an incarnation sits in byEvent
// and byPoolRound at once, so an episode matching both lists meets its index
// twice unless the merge consumes every list that holds the current minimum.
// The other fixtures here put every record in exactly ONE list, where "visited
// once" and "visited once per list" are the same number -- an independent lane
// showed a mutant that duplicates a shared index leaving the whole suite green.
func TestManualMergeVisitsASharedIndexOnce(t *testing.T) {
	const n = 5
	yes := true
	recs := make([]predictioneval.SourceRecord, 0, n)
	for i := 0; i < n; i++ {
		r := predictioneval.SourceRecord{
			PoolInstanceID: "p0", RoundIncarnationID: "r0", EventID: "e1",
			CollectorSequence: int64(i), ObservationID: "m" + strconv.Itoa(i),
			Kind: kindManualControl, PayloadVersion: predictioneval.SupportedPayloadVersion,
		}
		r.Payload.Phase = predictioneval.PhaseCallStarted
		r.Payload.Manual = &yes
		recs = append(recs, r)
	}
	ix := indexSignals(recs, map[string]bool{"e1": true})
	ep := EpisodeIdentity{PoolInstanceID: "p0", RoundIncarnationID: "r0", EventID: "e1"}

	// The fixture must really share indices, or it proves nothing.
	shared := 0
	byRound := map[int]bool{}
	for _, i := range ix.byPoolRound[poolRound{"p0", "r0"}] {
		byRound[i] = true
	}
	for _, i := range ix.byEvent["e1"] {
		if byRound[i] {
			shared++
		}
	}
	if shared != n {
		t.Fatalf("the fixture must put every index in two posting lists: %d of %d", shared, n)
	}

	var got []int
	ix.episodeSignals(ep, map[int64]bool{}).eachManualIndex(func(i int) bool {
		got = append(got, i)
		return true
	})
	if len(got) != n {
		t.Fatalf("an index carried by two lists must be visited once: %v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("the merged order must be strictly ascending: %v", got)
		}
	}
}

// TestOneObservationIdFromTwoListsIsReportedOnce covers the case the
// per-posting-list deduplication does NOT cover, and the reason the per-episode
// seen-set is still there.
//
// Deduplicating each list removes a repeat WITHIN a list. The same observation
// id can still reach one episode from two DIFFERENT lists, at two different
// indices, and then only the emission's own set can tell that it is one entry.
// The fixture below is built for exactly that: one manual fact carries the
// episode's event and no incarnation, the other carries the episode's
// incarnation and no event, and they share an observation id. Without this
// case, disabling the emission's set changes nothing observable — an earlier
// version of this suite had only within-list repeats, and the mutant survived.
func TestOneObservationIdFromTwoListsIsReportedOnce(t *testing.T) {
	yes := true
	manual := func(seq int64, round, event, obs string) predictioneval.SourceRecord {
		r := predictioneval.SourceRecord{
			PoolInstanceID: "p0", RoundIncarnationID: round, EventID: event,
			CollectorSequence: seq, ObservationID: obs,
			Kind: kindManualControl, PayloadVersion: predictioneval.SupportedPayloadVersion,
		}
		r.Payload.Phase = predictioneval.PhaseCallStarted
		r.Payload.Manual = &yes
		return r
	}
	recs := []predictioneval.SourceRecord{
		manual(1, "", "e1", "dup"),  // byEvent only
		manual(2, "r0", "", "dup"),  // byPoolRound only
		manual(3, "r0", "", "solo"), // byPoolRound only
	}
	ix := indexSignals(recs, map[string]bool{"e1": true})
	ep := EpisodeIdentity{PoolInstanceID: "p0", RoundIncarnationID: "r0", EventID: "e1"}
	sum := ix.episodeSignals(ep, map[int64]bool{})

	// The fixture must really deliver the id from two lists, or it proves nothing.
	seen, lists := map[string]int{}, 0
	for _, l := range sum.manual {
		if len(l) == 0 {
			continue
		}
		lists++
		for _, n := range l {
			seen[ix.signals[n].rec.ObservationID]++
		}
	}
	if lists != 2 || seen["dup"] != 2 {
		t.Fatalf("the fixture must carry one id in two lists: %d lists, %v", lists, seen)
	}

	got, total, over := ix.emitManualSignals(sum, nil, 0, 16)
	if over {
		t.Fatalf("nothing here is near the budget: %d", total)
	}
	if len(got) != 2 || got[0] != "dup" || got[1] != "solo" {
		t.Fatalf("the shared id is reported ONCE, in merged order: %v", got)
	}
	if total != 2 {
		t.Fatalf("and it is charged once: %d", total)
	}

	// AND THE SKIP MUST HAPPEN BEFORE THE BUDGET IS CONSULTED, which is only
	// observable at the boundary: with room for exactly one entry, the second
	// arrival of the same id must be skipped and the selection must SUCCEED.
	// Checking the budget first would abort on it -- a dataset refused for
	// reporting an entry it does not report.
	// Just the two shared-id entries: byEvent holds "dup" alone, byPoolRound
	// holds "dup" then "solo", so the second list is cut to its first entry.
	only := episodeSignals{call: -1, manual: [3][]int{sum.manual[0], sum.manual[1][:1], nil}}
	if len(only.manual[0]) != 1 || len(only.manual[1]) != 1 ||
		ix.signals[only.manual[0][0]].rec.ObservationID != "dup" ||
		ix.signals[only.manual[1][0]].rec.ObservationID != "dup" {
		t.Fatalf("this case needs exactly the two shared-id entries: %v", only.manual)
	}
	got, total, over = ix.emitManualSignals(only, nil, 0, 1)
	if over || total != 1 || len(got) != 1 || got[0] != "dup" {
		t.Fatalf("a duplicate at the exact budget is skipped, not refused: %v total=%d over=%v",
			got, total, over)
	}
}

// TestUnspentStartKeepsTheFIRSTIndexOnItsScope pins the rule the coverage-reason
// ORDER rests on, which the differential oracle cannot reach.
//
// An aggregate records the first index per (pool, incarnation) that carried an
// unspent start, because UNCLASSIFIED_FACT_ON_ROUND is appended at the position
// a walk would have met it and that position orders it against the other
// reasons. Keeping the LAST index instead flips an exported list -- and the
// randomized generator never builds the shape that shows it: two unspent
// automatic starts on one incarnation with another mark-producing fact strictly
// between them. An independent lane found that gap; this is the shape.
func TestUnspentStartKeepsTheFIRSTIndexOnItsScope(t *testing.T) {
	start := func(seq int64, attempt int64) predictioneval.SourceRecord {
		r := predictioneval.SourceRecord{
			PoolInstanceID: "p0", RoundIncarnationID: "r0", EventID: "e0",
			CollectorSequence: seq, ObservationID: "s" + strconv.FormatInt(attempt, 10),
			Kind: predictioneval.KindPlacement, PayloadVersion: predictioneval.SupportedPayloadVersion,
		}
		r.Payload.Phase = predictioneval.PhaseCallStarted
		r.Payload.Counters = map[string]int64{predictioneval.CounterAutoAttemptID: attempt}
		return r
	}
	bad := predictioneval.SourceRecord{
		PoolInstanceID: "p0", RoundIncarnationID: "r0", EventID: "e0",
		CollectorSequence: 2, ObservationID: "u1",
		Kind: "channel_event", PayloadUndecodable: true,
		PayloadVersion: predictioneval.SupportedPayloadVersion,
	}
	// start(1) ... undecodable(2) ... start(3): the unspent-start mark belongs
	// to index 0 and must be appended BEFORE the undecodable one at index 1.
	ix := indexSignals([]predictioneval.SourceRecord{start(1, 1), bad, start(3, 2)},
		map[string]bool{"e0": true})
	ep := EpisodeIdentity{PoolInstanceID: "p0", RoundIncarnationID: "r0", EventID: "e0"}

	agg := ix.poolRoundAggregate(poolRound{"p0", "r0"})
	if agg == nil {
		t.Fatal("the fixture must build a pool-round list")
	}
	if got, ok := agg.unspentStarts[poolRound{"p0", "r0"}]; !ok || got != 0 {
		t.Fatalf("the FIRST unspent start on the scope is index 0, got %d (present=%v)", got, ok)
	}

	sum := ix.episodeSignals(ep, map[int64]bool{})
	want := []string{CoverageUnclassifiedFact, CoverageUndecodableFact}
	if sum.coverage.Proven || len(sum.coverage.Reasons) != len(want) {
		t.Fatalf("want %v, got %+v", want, sum.coverage)
	}
	for i := range want {
		if sum.coverage.Reasons[i] != want[i] {
			t.Fatalf("the reason order is output: want %v, got %v", want, sum.coverage.Reasons)
		}
	}
	// And the oracle agrees, on this shape the generator does not build.
	if !sameOutcome(ix.outcomeByWalk(ep, map[int64]bool{}), ix.outcomeByAggregate(ep, map[int64]bool{})) {
		t.Fatal("the aggregate path and the walk disagree on this shape")
	}
}

// TestTheExpressibilityRoutingAllocatesNothing pins the cost the routing and the
// verifier gate claim, and its FIRST form is the reason it is written this way.
//
// It asserted zero allocations for checkRegistryTextExpressible on a ONE-ENTRY
// registry. The gate built its index label -- "entry " + strconv.Itoa(i) + " "
// -- once per entry, above every check, and threw it away on every entry that
// did not refuse. strconv.Itoa returns a cached string for 0..99, so a
// one-entry fixture measures exactly the range where the defect is invisible.
// Two independent review lanes measured what it actually cost: one allocation
// for every entry past index 99, on the HONEST path, above the digest -- 1,900
// on a clean 2,000-entry registry. The label is now built inside the refusal,
// which is canonical.go's WORK half and the same correction
// checkFactsetValuesExpressible already carries, and the fixture here crosses
// the cache boundary so the assertion can see it.
func TestTheExpressibilityRoutingAllocatesNothing(t *testing.T) {
	wide := strings.Repeat("w", 4096)
	good := SourceRoundClaim{
		Episode: EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: wide, PoolInstanceID: wide,
			RoundIncarnationID: wide, EventID: wide},
		Attempt:       predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: wide, PoolInstanceID: wide, AttemptID: 1},
		FactsetDigest: wide,
	}
	bad := good
	bad.Attempt.PoolInstanceID = "\xff\xfe\x80"

	// PAST THE SMALL-INTEGER CACHE, deliberately: at 250 entries the indices run
	// well beyond 99, where strconv.Itoa stops returning a cached string.
	claims := make([]SourceRoundClaim, 0, 250)
	for i := 0; i < 250; i++ {
		c := good
		c.Episode.EventID = "e" + strconv.Itoa(i)
		claims = append(claims, c)
	}
	many := ReconcileSourceRounds(claims)
	if len(many.Entries) != 250 {
		t.Fatalf("fixture: %d entries, want 250", len(many.Entries))
	}
	// A canonical claim on every entry, so the gate's canonical branch is walked
	// too rather than skipped.
	for _, e := range many.Entries {
		if e.Canonical == nil {
			t.Fatalf("fixture: entry %q has no canonical claim", e.EventID)
		}
	}

	for _, tc := range []struct {
		name string
		run  func()
	}{
		{"claimTextFault on a claim it accepts", func() { _ = claimTextFault(good) }},
		{"claimTextFault on a claim it refuses", func() { _ = claimTextFault(bad) }},
		{"expressibleClaim", func() { _ = expressibleClaim(bad) }},
		{"checkRegistryTextExpressible on a 250-entry registry it accepts", func() { _ = checkRegistryTextExpressible(many) }},
	} {
		if got := testing.AllocsPerRun(200, tc.run); got != 0 {
			t.Errorf("%s allocates %.0f times, want 0", tc.name, got)
		}
	}
}

// TestExpressibleEvidenceLeavesHonestEvidenceAlone pins the other half of the
// same discipline, one file over.
//
// expressibleEvidence copied both of the evidence's slices unconditionally, and
// ProjectResolution copies them AGAIN into the artifact, so every honest
// projection paid two copies where it used to pay one, on a path where nothing
// is lossy, and worst on the verifier's re-projection, which runs only after
// every string has already been proved valid. (An earlier wording quoted a
// percentage here; see expressibleEvidence for why the figures are gone.) Honest evidence is now returned untouched, which is
// asserted here as ALIASING rather than as a count: the returned slices must be
// the caller's own, not copies of them.
func TestExpressibleEvidenceLeavesHonestEvidenceAlone(t *testing.T) {
	ev := ResolutionEvidence{
		Round:              PublicRoundIdentity{EventID: "e1"},
		OrderedOutcomeIDs:  []string{"o1", "o2"},
		Claim:              ResolutionWinnerKnown,
		WinnerOutcomeID:    "o2",
		ProofBasis:         ProofBasisPlatformResolvedWinner,
		EvidenceReferences: []EvidenceReference{{ObservationID: "obs", Kind: "k", Phase: "p", RoundState: "RESOLVED", EventID: "e1"}},
		Availability:       AvailabilityAvailable,
		ProjectorRevision:  "pr",
		ProofRevision:      "x",
	}
	out, lossy := expressibleEvidence(ev)
	if lossy {
		t.Fatal("honest evidence must not be reported lossy")
	}
	if &out.OrderedOutcomeIDs[0] != &ev.OrderedOutcomeIDs[0] {
		t.Error("honest evidence's outcome ids were copied; the artifact copies them again, so this copy is pure waste")
	}
	if &out.EvidenceReferences[0] != &ev.EvidenceReferences[0] {
		t.Error("honest evidence's references were copied; the artifact copies them again, so this copy is pure waste")
	}
	if got := testing.AllocsPerRun(200, func() { _ = evidenceTextFault(ev) }); got != 0 {
		t.Errorf("evidenceTextFault allocates %.0f times, want 0", got)
	}
	// And the lossy path still copies, or blanking would rewrite the caller's
	// own evidence -- the defect the copies exist to prevent.
	bad := ev
	bad.OrderedOutcomeIDs = []string{"o1", "\xff\xfe\x80"}
	lossyOut, wasLossy := expressibleEvidence(bad)
	if !wasLossy {
		t.Fatal("evidence carrying invalid UTF-8 must be reported lossy")
	}
	if &lossyOut.OrderedOutcomeIDs[0] == &bad.OrderedOutcomeIDs[0] {
		t.Fatal("lossy evidence was blanked IN PLACE, rewriting the caller's own value")
	}
	if bad.OrderedOutcomeIDs[1] != "\xff\xfe\x80" {
		t.Fatalf("the caller's evidence was modified: %q", bad.OrderedOutcomeIDs[1])
	}
}

// TestExpressibleClaimIsIdempotentAndRoutesBackToInvalid pins the property the
// registry's whole self-re-derivation rests on, and it is the one this repair
// got WRONG the first time.
//
// VerifySourceRoundRegistry flattens a registry's entries and reconciles them
// AGAIN, so every claim ReconcileSourceRounds records has to land where it
// landed the first time. A blanked claim therefore has to satisfy two things at
// once: it must be expressible, and it must still reach the INVALID bucket.
//
// THE FIRST VERSION SATISFIED BOTH BY DROPPING THE ROUND NAME, and that was a
// fail-open: a claim that no longer names its round leaves that round's group,
// so one invalid byte on a competing claim dissolved a CONFLICT. The digest is
// the right field to drop -- it routes the claim to INVALID just as well, and
// the round name it keeps is what lets roundsWithUnreconcilableClaims hold the
// round fail-closed. So this now pins the OPPOSITE of what it used to: the
// round name SURVIVES wherever it was expressible.
func TestExpressibleClaimIsIdempotentAndRoutesBackToInvalid(t *testing.T) {
	const invalid = "\xff\xfe\x80"
	full := SourceRoundClaim{
		Episode: EpisodeIdentity{CollectorEpoch: 3, CollectorSessionID: "s", PoolInstanceID: "p",
			RoundIncarnationID: "r", EventID: "e"},
		Attempt:       predictioneval.AttemptKey{CollectorEpoch: 3, CollectorSessionID: "as", PoolInstanceID: "ap", AttemptID: 9},
		FactsetDigest: "d",
	}
	// Position i of this list is bit i of the mask, and for i in 0..5 also index
	// i of the kept list below. The seventh, FactsetDigest, has no entry there:
	// it is dropped unconditionally and is checked on its own.
	set := []func(*SourceRoundClaim){
		func(c *SourceRoundClaim) { c.Episode.CollectorSessionID = invalid },
		func(c *SourceRoundClaim) { c.Episode.PoolInstanceID = invalid },
		func(c *SourceRoundClaim) { c.Episode.RoundIncarnationID = invalid },
		func(c *SourceRoundClaim) { c.Episode.EventID = invalid },
		func(c *SourceRoundClaim) { c.Attempt.CollectorSessionID = invalid },
		func(c *SourceRoundClaim) { c.Attempt.PoolInstanceID = invalid },
		func(c *SourceRoundClaim) { c.FactsetDigest = invalid },
	}
	// Every SUBSET of the seven, so a claim broken at several positions at once
	// is covered as well as one broken at a single position.
	for mask := 1; mask < 1<<len(set); mask++ {
		c := full
		for i := range set {
			if mask&(1<<i) != 0 {
				set[i](&c)
			}
		}
		if claimTextFault(c) == "" {
			t.Fatalf("mask %d: a claim carrying invalid UTF-8 was called expressible", mask)
		}
		once := expressibleClaim(c)
		if what := claimTextFault(once); what != "" {
			t.Fatalf("mask %d: expressibleClaim left text it cannot express at %s: %+v", mask, what, once)
		}
		// The routing lever. Without this the blanked claim would be grouped as
		// evidence on the second pass and the registry would not re-derive.
		if once.FactsetDigest != "" {
			t.Fatalf("mask %d: a blanked claim kept its evidence identity, so it would reconcile as evidence on the second pass: %+v", mask, once)
		}
		if twice := expressibleClaim(once); twice != once {
			t.Fatalf("mask %d: expressibleClaim is not idempotent: %+v then %+v", mask, once, twice)
		}
		// The numbers are expressible and are kept; so is every identity
		// component that was valid -- INCLUDING the round name, which is what
		// keeps the round the claim contested visible in the registry.
		if once.Episode.CollectorEpoch != full.Episode.CollectorEpoch ||
			once.Attempt.CollectorEpoch != full.Attempt.CollectorEpoch ||
			once.Attempt.AttemptID != full.Attempt.AttemptID {
			t.Fatalf("mask %d: an expressible number was dropped: %+v", mask, once)
		}
		for i, kept := range []struct{ got, want string }{
			{once.Episode.CollectorSessionID, full.Episode.CollectorSessionID},
			{once.Episode.PoolInstanceID, full.Episode.PoolInstanceID},
			{once.Episode.RoundIncarnationID, full.Episode.RoundIncarnationID},
			{once.Episode.EventID, full.Episode.EventID},
			{once.Attempt.CollectorSessionID, full.Attempt.CollectorSessionID},
			{once.Attempt.PoolInstanceID, full.Attempt.PoolInstanceID},
		} {
			if mask&(1<<i) == 0 && kept.got != kept.want {
				t.Fatalf("mask %d: a VALID component at position %d was dropped: %q, want %q", mask, i, kept.got, kept.want)
			}
			if mask&(1<<i) != 0 && kept.got != "" {
				t.Fatalf("mask %d: an unrepresentable component at position %d was kept: %q", mask, i, kept.got)
			}
		}
	}
}

// TestADecoderFaultIsBoundedBeforeItIsRendered holds the half of the rule the
// extent ceiling does NOT hold: that producing the bounded refusal is itself
// bounded.
//
// decodeFault used to judge the fault by `err.Error()`, and rendering an
// *encoding/json.UnmarshalTypeError CONCATENATES the offending literal into a
// NEW string — so the refusal that LEFT VerifyP3bRuleset was 154 bytes, of
// which this function's own sentence is 75 and the joined sentinel the rest,
// while producing it had already copied the caller's own digits. A Codex
// review lane reported it on the published head 11a16b87; measured there
// through VerifyP3bRuleset, refusing a document whose literal is 64 KiB cost
// about 540.6 KB against about 8.3 KB for a 1 KiB one — a DIFFERENCE of about
// 532 KB, which is the figure worth quoting only once it is said which of the
// two it is. Roughly 72 KB of that difference was this function rendering what
// it was about to refuse to carry; the rest is the decoder reading a document
// 64 KiB larger, and it is still there.
//
// THAT IS THE ROUND'S OWN RULE FAILING IN THE CODE THAT STATES IT: a
// materialization must not precede a gate that can refuse without it. This
// gate can refuse without it, because the unbounded carrier is a FIELD of a
// typed error and its length is available without formatting anything.
//
// IT IS MEASURED HERE AND NOT THROUGH THE VERIFIER, because decoding a
// document 64 KiB larger costs 64 KiB more inside encoding/json whatever this
// function does; that cost is the decoder's and is not removable. Driving
// decodeFault directly is the only way to measure what decodeFault adds.
func TestADecoderFaultIsBoundedBeforeItIsRendered(t *testing.T) {
	fault := func(digits int) error {
		return &json.UnmarshalTypeError{
			Value: "number " + strings.Repeat("9", digits),
			Type:  reflect.TypeOf(float64(0)),
			Field: "default.rawMinPercent",
		}
	}
	small, large := fault(1<<10), fault(1<<16)
	for _, err := range []error{small, large} {
		if got := decodeFault(err); len(got.Error()) > rulesetDecodeFaultCeiling {
			t.Fatalf("a fault past the ceiling must be reported by its extent, got %d bytes", len(got.Error()))
		}
	}
	measure := func(err error) uint64 {
		res := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = decodeFault(err)
			}
		})
		return uint64(res.AllocedBytesPerOp())
	}
	cheap, dear := measure(small), measure(large)
	t.Logf("bounding a 1 KiB carrier costs %d bytes; a 64 KiB carrier costs %d", cheap, dear)
	if grew := int64(dear) - int64(cheap); grew > 1<<10 {
		t.Fatalf("bounding a 64 KiB carrier allocates %d bytes more than bounding a 1 KiB one: "+
			"the fault is being RENDERED before its extent is judged, so the bounded refusal "+
			"costs a copy of the very text it refuses to carry", grew)
	}
}

// TestADecoderFaultReportsTheSentenceAndNotItsCarrier pins the NUMBER a
// decode-fault refusal reports, which nothing pinned before.
//
// THE BOUNDARY TEST CANNOT SEE THIS. It asserts where the ceiling begins, how
// wide the widest literal carried verbatim is, and that the sentence it holds
// is 512 bytes -- three true facts, none of which observes the extent printed
// ABOVE the ceiling. So a change to what that extent MEASURES passes it: an
// arm reporting the carrier's width rather than the sentence's leaves every
// row of that test green while every over-ceiling refusal on this branch
// quietly reports a different number, and reports one that DECREASES as the
// input grows, because the two quantities cross at the ceiling.
//
// WHAT IS PINNED is the identity the repaired arm relies on: the sentence's
// width is the width of the same sentence with its carrier blanked, plus the
// carrier's length, so the number reported without rendering is the number
// rendering would have produced. Monotonicity follows and is asserted too,
// because a refusal whose extent falls as its input grows is not a measure.
func TestADecoderFaultReportsTheSentenceAndNotItsCarrier(t *testing.T) {
	widths := []int{0, 100, 300, 309, 428, 429, 430, 452, 453, 504, 505, 506, 600, 1000, 65536}
	for _, field := range []string{"", "default.rawMinPercent", "detailed[3].rawAttemptRatePercent"} {
		last := -1
		for _, n := range widths {
			err := &json.UnmarshalTypeError{
				Value: "number " + strings.Repeat("9", n),
				Type:  reflect.TypeOf(float64(0)),
				Field: field,
			}
			sentence := len(err.Error())
			got := decodeFault(err).Error()
			if sentence <= rulesetDecodeFaultCeiling {
				if got != err.Error() {
					t.Fatalf("field=%q n=%d: a fault whose sentence is %d bytes is under the ceiling and must pass through as written, got %q",
						field, n, sentence, got)
				}
				continue
			}
			want := "p4offline: the decoder refused the raw document with a fault of " +
				strconv.Itoa(sentence) + " bytes"
			if got != want {
				t.Fatalf("field=%q n=%d: the refusal must report the SENTENCE's width\n got %q\nwant %q",
					field, n, got, want)
			}
			if last >= 0 && sentence < last {
				t.Fatalf("field=%q n=%d: the fixture itself is not monotonic", field, n)
			}
			last = sentence
		}
	}
}

// TestTheRenderedArmsCeilingIsHeldByAFaultThatIsNotTyped pins the FALLBACK,
// which the typed arm quietly took out of reach.
//
// decodeFault has two gates on one ceiling: the typed arm, which computes an
// UnmarshalTypeError's sentence width without building it, and the rendered
// arm below it for every other fault. Every over-ceiling fault encoding/json
// produces on this decode target today is an UnmarshalTypeError -- an
// adversarial lane fuzzed 1.4 million documents looking for one that is not
// and found none -- so after the typed arm went in, NOTHING reached the
// rendered arm with anything long enough to test its ceiling. A mutation
// campaign found it: raising that ceiling to 1<<30 left the whole suite green.
//
// The rendered arm is precisely the part that covers a fault type the library
// adds tomorrow, so leaving its ceiling unpinned would retire the guard that
// exists for the unknown case. This drives it with a fault that is not typed
// at all.
func TestTheRenderedArmsCeilingIsHeldByAFaultThatIsNotTyped(t *testing.T) {
	// One byte under, on the ceiling, one byte over: the ceiling cannot be
	// raised, lowered or moved without a named failure here either.
	const prefix = "p4offline: the decoder refused the raw document with a fault of "
	for _, n := range []int{rulesetDecodeFaultCeiling - 1, rulesetDecodeFaultCeiling, rulesetDecodeFaultCeiling + 1} {
		plain := errors.New(strings.Repeat("z", n))
		if _, typed := plain.(*json.UnmarshalTypeError); typed {
			t.Fatalf("the fixture must NOT be the type the arm above handles")
		}
		got := decodeFault(plain).Error()
		if n <= rulesetDecodeFaultCeiling {
			if got != plain.Error() {
				t.Fatalf("a %d-byte untyped fault is at or under the ceiling and must pass through as written, got %d bytes",
					n, len(got))
			}
			continue
		}
		want := prefix + strconv.Itoa(n) + " bytes"
		if got != want {
			t.Fatalf("a %d-byte untyped fault must be reported by extent\n got %q\nwant %q", n, got, want)
		}
	}
}

// TestTheLocalOrderFaultAgreesWithTheMaterializer pins the one cost of
// restating the causal-order contract inside this package.
//
// SelectEpisodes must refuse an out-of-order dataset BEFORE the session
// preflight speaks, and the preflight exists so the materializer is not paid
// for — so the predicate cannot be reached through the materializer and is
// restated instead. What that buys is the ordering; what it risks is two
// statements of one contract drifting apart, silently, in different packages.
//
// This is internal because it must drive the unexported predicate directly
// against the exported seam that owns the original. It compares the two over a
// fixed table of the shapes that decide the rule's edges and then over a
// randomized sweep, so a change on either side fails here rather than showing
// up as a disagreement nobody looked for.
func TestTheLocalOrderFaultAgreesWithTheMaterializer(t *testing.T) {
	rec := func(epoch, seq int64) predictioneval.SourceRecord {
		return predictioneval.SourceRecord{
			ObservationID:      "o" + strconv.FormatInt(epoch, 10) + "-" + strconv.FormatInt(seq, 10),
			CollectorSessionID: "s", CollectorEpoch: epoch, CollectorSequence: seq,
		}
	}
	run := func(t *testing.T, name string, recs []predictioneval.SourceRecord) {
		t.Helper()
		ds := predictioneval.SourceDataset{
			Source: predictioneval.SourceProvenance{
				CollectorEpoch: 1, CollectorSessionID: "s",
				ProducerRevision: predictioneval.SupportedProducerRevision,
			},
			Records: recs,
		}
		local := causalOrderFault(ds.Records)
		_, matErr := predictioneval.MaterializePairedKnowledge(ds)
		gotLocal := local != nil
		gotMat := errors.Is(matErr, predictioneval.ErrRecordsOutOfOrder)
		if gotLocal != gotMat {
			t.Fatalf("%s: causalOrderFault refused=%v (%v) but the materializer refused=%v (%v); "+
				"the two statements of the causal-order contract have drifted",
				name, gotLocal, local, gotMat, matErr)
		}
		if local != nil && !errors.Is(local, predictioneval.ErrRecordsOutOfOrder) {
			t.Fatalf("%s: causalOrderFault answered %v, not the shared sentinel", name, local)
		}
	}

	table := []struct {
		name string
		recs []predictioneval.SourceRecord
	}{
		{"no facts", nil},
		{"one fact", []predictioneval.SourceRecord{rec(1, 7)}},
		{"ascending within an epoch", []predictioneval.SourceRecord{rec(1, 1), rec(1, 2), rec(1, 3)}},
		// Equal positions are not a fault: the rule is strict.
		{"a repeated position", []predictioneval.SourceRecord{rec(1, 1), rec(1, 1), rec(1, 2)}},
		{"a sequence that goes backwards", []predictioneval.SourceRecord{rec(1, 1), rec(1, 3), rec(1, 2)}},
		{"the fault in the first pair", []predictioneval.SourceRecord{rec(1, 9), rec(1, 8), rec(1, 10)}},
		{"the fault in the last pair", []predictioneval.SourceRecord{rec(1, 1), rec(1, 2), rec(1, 0)}},
		{"an epoch that goes backwards", []predictioneval.SourceRecord{rec(2, 1), rec(1, 2)}},
		{"an epoch that advances", []predictioneval.SourceRecord{rec(1, 5), rec(2, 6)}},
		// A new epoch RESETS the sequence, so a lower sequence across an epoch
		// boundary is ordered. This is the edge a careless restatement loses.
		{"a sequence reset by a new epoch", []predictioneval.SourceRecord{rec(1, 900), rec(2, 1), rec(2, 2)}},
		{"equal epochs after an advance", []predictioneval.SourceRecord{rec(1, 1), rec(2, 1), rec(2, 1)}},
	}
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) { run(t, tc.name, tc.recs) })
	}

	// The sweep: small alphabets on both coordinates, so faults and near-misses
	// (repeats, epoch resets) are dense rather than incidental.
	t.Run("a randomized sweep", func(t *testing.T) {
		rng := rand.New(rand.NewSource(20260919))
		for i := 0; i < 4000; i++ {
			n := rng.Intn(6)
			recs := make([]predictioneval.SourceRecord, n)
			for j := range recs {
				recs[j] = rec(int64(rng.Intn(3)), int64(rng.Intn(4)))
			}
			run(t, "sweep #"+strconv.Itoa(i), recs)
		}
	})
}

// claimKeyBeforeStreaming is claimKey as it stood before the framing was
// streamed, copied verbatim from de848c8. It is the oracle for obligation A,
// so it must NOT be tidied: it builds the episode's hex rendering as a string,
// frames that string, and hex-encodes the result.
func claimKeyBeforeStreaming(c SourceRoundClaim) string {
	var k canonical
	k.str(c.Episode.String())
	k.i64(c.Attempt.CollectorEpoch)
	k.str(c.Attempt.CollectorSessionID)
	k.str(c.Attempt.PoolInstanceID)
	k.str(strconv.FormatUint(c.Attempt.AttemptID, 10))
	k.str(c.FactsetDigest)
	return hexEncode(k.bytes())
}

// registryDigestBeforeStreaming is registryDigest as it stood before the same
// change, likewise verbatim and likewise not to be tidied.
func registryDigestBeforeStreaming(entries []SourceRoundEntry) string {
	var c canonical
	c.str(SourceRoundRegistryVersion)
	c.count(len(entries))
	for _, e := range entries {
		c.str(e.EventID)
		c.str(string(e.Status))
		c.count(len(e.Claims))
		for _, cl := range e.Claims {
			c.str(claimKeyBeforeStreaming(cl))
		}
		c.boolean(e.Canonical != nil)
	}
	return c.digest()
}

// claimShapesForFramingOracle spans the field shapes the framing distinguishes:
// empty and non-empty strings, lengths that collide and lengths that do not,
// negative and multi-digit integers, and bytes no encoding may reinterpret.
func claimShapesForFramingOracle() []SourceRoundClaim {
	vals := []string{"", "a", "b", "aa", "ab", "ba", "zzz", "\xff\xfe", "shared", "round-10"}
	epochs := []int64{-1, 0, 1, 9, 10, 100}
	ids := []uint64{0, 1, 9, 10}
	var cs []SourceRoundClaim
	for _, v := range vals {
		for _, e := range epochs {
			for _, id := range ids {
				cs = append(cs, SourceRoundClaim{
					Episode: EpisodeIdentity{
						CollectorEpoch: e, CollectorSessionID: v, PoolInstanceID: v + "p",
						RoundIncarnationID: v, EventID: v,
					},
					Attempt: predictioneval.AttemptKey{
						CollectorEpoch: e, CollectorSessionID: v, PoolInstanceID: v, AttemptID: id,
					},
					FactsetDigest: v + "d",
				})
			}
		}
	}
	// AND A SECOND FAMILY IN WHICH NO TWO FIELDS OF ONE CLAIM ARE EQUAL.
	//
	// THE FAMILY ABOVE CANNOT SEE A REORDERING, which a Q3 lane demonstrated
	// rather than argued: it gives a claim's session id, incarnation id and
	// event id the same bytes, and its attempt's session and pool the same
	// again, so swapping two of those clauses in compareEpisodeFraming leaves
	// all 57,600 pairs agreeing. The lane's mutant reordered a real registry
	// and this file stayed green.
	//
	// BOTH FAMILIES ARE KEPT because they prove different things. The uniform
	// one is where the LENGTH PREFIX earns its place: claims built from "a"
	// and "aa" put equal bytes in different positions, which is the aliasing
	// a bare concatenation would lose. The distinct one is where a POSITION
	// earns its place. Replacing the first with the second would trade one
	// blind spot for another.
	// EACH FIELD VARIES INDEPENDENTLY, which a suffix scheme does NOT give:
	// deriving every field from one value makes them all move together, so a
	// pair whose session order and event order DISAGREE never occurs and the
	// reordering stays invisible. These are a product over one-character
	// values, all of equal length, so the framed lengths tie and the position
	// of a clause is the only thing left to decide the order.
	two := []string{"a", "b"}
	for _, es := range two {
		for _, ep := range two {
			for _, er := range two {
				for _, ee := range two {
					for _, as := range two {
						for _, ap := range two {
							cs = append(cs, SourceRoundClaim{
								Episode: EpisodeIdentity{
									CollectorEpoch: 7, CollectorSessionID: es, PoolInstanceID: ep,
									RoundIncarnationID: er, EventID: ee,
								},
								Attempt: predictioneval.AttemptKey{
									CollectorEpoch: 7, CollectorSessionID: as, PoolInstanceID: ap, AttemptID: 3,
								},
								FactsetDigest: es + ee,
							})
						}
					}
				}
			}
		}
	}
	return cs
}

// TestStreamingTheClaimKeyDidNotMoveOneByteOfIt is obligation A's compatibility
// proof, and it is an ORACLE rather than a recorded constant: the pre-change
// functions are kept verbatim above and every claim is put through both.
//
// The nesting claimKey frames -- a hex rendering of a framing, framed and
// hex-encoded again -- is the representation an independent golden pins, so it
// had to survive exactly. What changed is only that the intermediate strings
// are no longer built. A recorded digest could not tell those two apart; this
// can.
func TestStreamingTheClaimKeyDidNotMoveOneByteOfIt(t *testing.T) {
	cs := claimShapesForFramingOracle()
	for i := range cs {
		if got, want := claimKey(cs[i]), claimKeyBeforeStreaming(cs[i]); got != want {
			t.Fatalf("claim %d: claimKey moved\n got: %s\nwant: %s", i, got, want)
		}
		// THE APPEND CONTRACT, which registryDigest depends on and which the
		// line above USED to assert nothing about: an earlier form of this
		// row compared hexEncode(claimKeyFraming(nil, c)) to claimKey(c),
		// and claimKey is DEFINED as exactly that, so it could not fail for
		// any implementation of either. What is worth pinning is that
		// framing into a non-empty buffer appends rather than overwrites,
		// because registryDigest reuses one scratch buffer across every
		// claim.
		if got := claimKeyFraming([]byte("prefix"), cs[i]); string(got) != "prefix"+string(claimKeyFraming(nil, cs[i])) {
			t.Fatalf("claim %d: framing into a non-empty buffer did not append", i)
		}
		if got, want := cs[i].Episode.framedLen(), len(cs[i].Episode.framed()); got != want {
			t.Fatalf("claim %d: framedLen says %d, framed() is %d bytes", i, got, want)
		}
	}
	// AND THE REGISTRY DIGEST, over reconciliations of real shapes rather than
	// over loose claims: unique rounds, a conflict, and exact duplicates.
	for _, tc := range []struct {
		name  string
		build func() []SourceRoundClaim
	}{
		{"distinct rounds", func() []SourceRoundClaim { return cs[:12] }},
		{"one shared round", func() []SourceRoundClaim {
			out := append([]SourceRoundClaim(nil), cs[:8]...)
			for i := range out {
				out[i].Episode.EventID = "shared"
			}
			return out
		}},
		{"exact duplicates", func() []SourceRoundClaim {
			return []SourceRoundClaim{cs[3], cs[3], cs[3]}
		}},
		{"every shape at once", func() []SourceRoundClaim { return cs }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := ReconcileSourceRounds(tc.build())
			if got, want := reg.Digest, registryDigestBeforeStreaming(reg.Entries); got != want {
				t.Fatalf("registry digest moved\n got: %s\nwant: %s", got, want)
			}
			if err := VerifySourceRoundRegistry(reg); err != nil {
				t.Fatalf("the reconciliation must verify: %v", err)
			}
		})
	}
}

// TestTheAllocationFreeClaimOrderIsTheFramingsOwnOrder pins the one risk the
// ordering repair introduces: compareClaimKeys mirrors a framing written
// elsewhere, so the two could drift apart silently and reorder a registry.
//
// Every ordered pair of the shape fixture is compared both ways. A comparator
// that agreed on the fixtures a sort happens to visit, and disagreed elsewhere,
// would still pass a test that only sorted -- so this compares pairs directly.
func TestTheAllocationFreeClaimOrderIsTheFramingsOwnOrder(t *testing.T) {
	cs := claimShapesForFramingOracle()
	sign := func(n int) int {
		switch {
		case n < 0:
			return -1
		case n > 0:
			return 1
		}
		return 0
	}
	framings := make([][]byte, len(cs))
	for i := range cs {
		framings[i] = claimKeyFraming(nil, cs[i])
	}
	pairs, disagreed := 0, 0
	for i := range cs {
		for j := range cs {
			pairs++
			want := sign(compareFramingBytes(framings[i], framings[j]))
			got := sign(compareClaimKeys(cs[i], cs[j]))
			if got != want {
				disagreed++
				if disagreed <= 3 {
					t.Errorf("pair (%d,%d): comparator says %d, the framing says %d", i, j, got, want)
				}
			}
		}
	}
	if pairs < 10000 {
		t.Fatalf("the fixture must exercise a wide pair set, got %d pairs", pairs)
	}
	if disagreed != 0 {
		t.Fatalf("%d of %d pairs disagree with the framing's own order", disagreed, pairs)
	}
}

// compareFramingBytes is the byte comparison the sort used to perform on
// materialized keys, kept here as the order oracle.
func compareFramingBytes(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return int(a[i]) - int(b[i])
		}
	}
	return len(a) - len(b)
}

// TestTheClaimComparatorAllocatesNothing pins a claim that was FALSE when it
// was written, and that nothing in this package noticed.
//
// `sortClaimsByKey`'s comment said the comparator allocates nothing. It
// formatted three integers per comparison with strconv.FormatInt/FormatUint,
// and `framedLen` formatted a fourth just to take its length. strconv returns
// a cached string only for values below 100, so a nine-digit collector epoch
// allocated 8 bytes per operand — 16 bytes per comparison from framedLen
// alone, over the O(N log N) comparisons of a sort. A Codex review caught the
// claim; a measurement confirmed it at 16 B/call.
//
// The replacement appends into stack buffers, which measures 0. What makes
// that safe is the part worth stating: the framing compares the LENGTH of a
// decimal rendering first, so a hand-rolled digit count would have had to get
// MinInt64 and the minus sign right to reproduce the ORDER the registry digest
// is pinned to. AppendInt is the same formatting family at the same base, so
// it reproduces the rendering by construction — and the first row below holds
// framedLen to FormatInt's own answer across the values a digit count gets
// wrong.
func TestTheClaimComparatorAllocatesNothing(t *testing.T) {
	for _, v := range []int64{0, 1, -1, 9, -9, 10, -10, 99, -99, 100, -100,
		20260901, -20260901, 1 << 62, math.MaxInt64, math.MinInt64} {
		e := EpisodeIdentity{CollectorEpoch: v}
		if got, want := e.framedLen(), 5*8+len(strconv.FormatInt(v, 10)); got != want {
			t.Fatalf("framedLen(%d) reports %d, the framing writes %d", v, got, want)
		}
	}
	// The comparator over a pair that ties until its last part, so every
	// numeric comparison is reached.
	a := SourceRoundClaim{
		Episode: EpisodeIdentity{CollectorEpoch: 20260901, CollectorSessionID: "session",
			PoolInstanceID: "pool", RoundIncarnationID: "round", EventID: "event"},
		Attempt: predictioneval.AttemptKey{CollectorEpoch: 20260901, CollectorSessionID: "session",
			PoolInstanceID: "pool", AttemptID: 123456},
		FactsetDigest: "digest",
	}
	b := a
	b.FactsetDigest = "digesu"
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	const reps = 200000
	sink := 0
	for i := 0; i < reps; i++ {
		sink += compareClaimKeys(a, b)
	}
	runtime.ReadMemStats(&after)
	if sink == 0 {
		t.Fatal("the fixture must not compare equal, or the numeric parts are never reached")
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 0 {
		t.Fatalf("the comparator allocated %d bytes over %d comparisons; the sort calls it O(N log N) times",
			grew, reps)
	}
}

// buildP2ExclusionIndexBeforeTheReduction is the build that
// appendFirstPositionPerReason replaced, copied verbatim from cef68f7. It is the
// oracle, so it must NOT be tidied.
func buildP2ExclusionIndexBeforeTheReduction(excluded []predictioneval.Exclusion) *p2ExclusionIndex {
	ix := &p2ExclusionIndex{
		byKey: make(map[predictioneval.AttemptKey][]int, len(excluded)),
		byObs: make(map[string][]int, len(excluded)),
		all:   excluded,
	}
	for i, e := range excluded {
		if e.Key != nil {
			ix.byKey[*e.Key] = append(ix.byKey[*e.Key], i)
		}
		if e.ObservationID != "" {
			ix.byObs[e.ObservationID] = append(ix.byObs[e.ObservationID], i)
		}
	}
	return ix
}

// exclusionsForBeforeTheMerge is the implementation the bounded merge replaced,
// copied verbatim from 65e9249. It is the oracle, so it must NOT be tidied.
func (ix *p2ExclusionIndex) exclusionsForBeforeTheMerge(key predictioneval.AttemptKey, obs []string) []string {
	var hits []int
	hits = append(hits, ix.byKey[key]...)
	seenObs := make(map[string]bool, len(obs))
	for _, id := range obs {
		if id == "" || seenObs[id] {
			continue
		}
		seenObs[id] = true
		hits = append(hits, ix.byObs[id]...)
	}
	if len(hits) == 0 {
		return nil
	}
	sort.Ints(hits)
	var out []string
	prev := -1
	for _, i := range hits {
		if i == prev {
			continue
		}
		prev = i
		out = appendOnce(out, ix.all[i].Reason)
	}
	return out
}

// TestTheReducedPostingListsAnswerAsTheFullOnesDid executes the identity that
// the repair at buildP2ExclusionIndex rests on, rather than arguing it.
//
// THE CLAIM UNDER TEST. Keeping only the first position of each distinct reason
// in each posting list cannot move any answer, because the answer is the
// distinct reasons ordered by the smallest hit position each occupies, and that
// smallest position is minimal within its own bucket too, so the reduction
// keeps it.
//
// WHY RANDOMIZED AND NOT A TABLE. The argument is about the INTERACTION of two
// routes, duplicate reasons inside one bucket, and positions that interleave
// across buckets. A table encodes the cases its author thought of; the previous
// three defects on this seam were each a case nobody had thought of. The
// alphabets are deliberately tiny -- three observation ids, three attempt keys,
// four reasons -- so collisions are the common case rather than a rare draw.
//
// THE THREE CONTROLS matter as much as the comparison. Without them an oracle
// that never reached a reduced bucket, or never produced a non-empty answer,
// would pass while proving nothing: the test therefore requires that the
// reduction actually FIRED, that the answers actually had content, and that a
// deliberately broken reduction -- keeping the LAST position per reason instead
// of the first -- is caught by this same fixture.
func TestTheReducedPostingListsAnswerAsTheFullOnesDid(t *testing.T) {
	reasons := []string{
		predictioneval.ExclusionPayloadUndecodable,
		predictioneval.ExclusionNoTerminalFact,
		predictioneval.ExclusionForeignSession,
		predictioneval.ExclusionMultipleTerminalFacts,
	}
	obsIDs := []string{"", "a", "b"}
	// "c" is never stamped on an exclusion, so the obs route is also exercised
	// with an id the index has never heard of -- a shape a Q3 lane pointed out
	// the first draft reached only on the key route.
	queryIDs := []string{"", "a", "b", "c"}
	keys := []predictioneval.AttemptKey{
		{CollectorEpoch: 7, CollectorSessionID: "s", PoolInstanceID: "p", AttemptID: 1},
		{CollectorEpoch: 7, CollectorSessionID: "s", PoolInstanceID: "p", AttemptID: 2},
		{CollectorEpoch: 7, CollectorSessionID: "s", PoolInstanceID: "q", AttemptID: 1},
	}
	// absent is a key no exclusion ever carries, so the byKey route contributes
	// nothing on some queries and the obs route has to answer alone.
	absent := predictioneval.AttemptKey{CollectorEpoch: 9, CollectorSessionID: "z", PoolInstanceID: "z", AttemptID: 9}

	// distinctReasons is the bound the repair claims, computed independently of
	// the code that enforces it.
	distinctReasons := func(all []predictioneval.Exclusion, list []int) int {
		var seen []string
		for _, i := range list {
			seen = appendOnce(seen, all[i].Reason)
		}
		return len(seen)
	}

	rng := rand.New(rand.NewSource(20260920))
	var compared, nonEmpty, reducedObs, reducedKey int
	for round := 0; round < 4000; round++ {
		excluded := make([]predictioneval.Exclusion, rng.Intn(24))
		for i := range excluded {
			e := predictioneval.Exclusion{
				ObservationID: obsIDs[rng.Intn(len(obsIDs))],
				Reason:        reasons[rng.Intn(len(reasons))],
			}
			if rng.Intn(3) != 0 {
				k := keys[rng.Intn(len(keys))]
				e.Key = &k
			}
			excluded[i] = e
		}

		full := buildP2ExclusionIndexBeforeTheReduction(excluded)
		cut := buildP2ExclusionIndex(excluded)

		for id, list := range full.byObs {
			if n := distinctReasons(excluded, list); len(cut.byObs[id]) != n {
				t.Fatalf("round %d: byObs[%q] reduced to %d positions, want one per distinct reason (%d)",
					round, id, len(cut.byObs[id]), n)
			}
			if len(cut.byObs[id]) < len(list) {
				reducedObs++
			}
		}
		for k, list := range full.byKey {
			if n := distinctReasons(excluded, list); len(cut.byKey[k]) != n {
				t.Fatalf("round %d: byKey[%+v] reduced to %d positions, want one per distinct reason (%d)",
					round, k, len(cut.byKey[k]), n)
			}
			if len(cut.byKey[k]) < len(list) {
				reducedKey++
			}
		}

		for q := 0; q < 4; q++ {
			key := absent
			if rng.Intn(4) != 0 {
				key = keys[rng.Intn(len(keys))]
			}
			obs := make([]string, rng.Intn(4))
			for i := range obs {
				obs[i] = queryIDs[rng.Intn(len(queryIDs))]
			}
			// THE ORACLE SPANS BOTH REPAIRS: the ORIGINAL implementation on the
			// UNREDUCED index against the current one on the reduced index. A
			// comparison of the current implementation against itself on two
			// indexes would have stopped proving anything about the merge.
			want := full.exclusionsForBeforeTheMerge(key, obs)
			got := cut.exclusionsFor(key, obs)
			compared++
			if len(want) > 0 {
				nonEmpty++
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round %d query %d: current answered %v, the pre-repair implementation answered %v\nexclusions: %+v\nkey: %+v obs: %v",
					round, q, got, want, excluded, key, obs)
			}
			// And the merge alone, isolated from the reduction, on one index.
			if iso := cut.exclusionsForBeforeTheMerge(key, obs); !reflect.DeepEqual(cut.exclusionsFor(key, obs), iso) {
				t.Fatalf("round %d query %d: the merge alone moved an answer on one index: %v against %v",
					round, q, cut.exclusionsFor(key, obs), iso)
			}
		}
	}
	t.Logf("%d comparisons, %d with a non-empty answer, %d obs and %d key buckets actually reduced",
		compared, nonEmpty, reducedObs, reducedKey)
	// THE TWO COUNTERS ARE SEPARATE BECAUSE ONE SUM HID A MUTANT. A Q3 lane
	// reverted the byKey half of the repair to a plain append and the whole
	// suite stayed green: byObs alone kept a combined counter above zero, and
	// the comparison above cannot see it, because leaving a route unreduced is
	// output-IDENTICAL by construction. byKey buckets are length one on every
	// input the materializer can produce, so nothing else in this package can
	// pin that half at all.
	if reducedObs == 0 {
		t.Fatal("no observation-id posting list was ever reduced: the fixture never reached the repair")
	}
	if reducedKey == 0 {
		t.Fatal("no attempt-key posting list was ever reduced: the byKey half of the repair is unpinned")
	}
	if nonEmpty*4 < compared {
		t.Fatalf("only %d of %d answers had content: the fixture is mostly measuring the empty case", nonEmpty, compared)
	}

	t.Run("and the same fixture catches a reduction that keeps the wrong position", func(t *testing.T) {
		// Keeping the LAST position per reason preserves the reason SET and
		// breaks only the ORDER, which is exactly the half of the claim an
		// argument is likeliest to get wrong.
		//
		// WHAT THIS SUBTEST IS, STATED PRECISELY. A production mutant that kept
		// the last position would already be caught by the comparison above,
		// because the oracle is the UNREDUCED index and the order would move.
		// So this is not that mutant: it is evidence that the FIXTURE reaches
		// order-sensitive cases at all. Without it, a fixture whose buckets
		// never held two positions with the same reason would pass the
		// comparison while proving only set equality, and the distinction
		// between the two would be invisible.
		lastPerReason := func(all []predictioneval.Exclusion, list []int) []int {
			var out []int
			for _, i := range list {
				replaced := false
				for n, j := range out {
					if all[j].Reason == all[i].Reason {
						out[n], replaced = i, true
						break
					}
				}
				if !replaced {
					out = append(out, i)
				}
			}
			return out
		}
		rng := rand.New(rand.NewSource(20260920))
		caught := false
		for round := 0; round < 4000 && !caught; round++ {
			excluded := make([]predictioneval.Exclusion, rng.Intn(24))
			for i := range excluded {
				e := predictioneval.Exclusion{
					ObservationID: obsIDs[rng.Intn(len(obsIDs))],
					Reason:        reasons[rng.Intn(len(reasons))],
				}
				if rng.Intn(3) != 0 {
					k := keys[rng.Intn(len(keys))]
					e.Key = &k
				}
				excluded[i] = e
			}
			full := buildP2ExclusionIndexBeforeTheReduction(excluded)
			mutant := buildP2ExclusionIndexBeforeTheReduction(excluded)
			for id, list := range mutant.byObs {
				mutant.byObs[id] = lastPerReason(excluded, list)
			}
			for k, list := range mutant.byKey {
				mutant.byKey[k] = lastPerReason(excluded, list)
			}
			for q := 0; q < 4 && !caught; q++ {
				key := absent
				if rng.Intn(4) != 0 {
					key = keys[rng.Intn(len(keys))]
				}
				obs := make([]string, rng.Intn(4))
				for i := range obs {
					obs[i] = queryIDs[rng.Intn(len(queryIDs))]
				}
				if !reflect.DeepEqual(mutant.exclusionsFor(key, obs), full.exclusionsFor(key, obs)) {
					caught = true
				}
			}
		}
		if !caught {
			t.Fatal("the last-position mutant survived this fixture: the comparison above proves less than it claims")
		}
	})
}

// TestAnEpisodesOwnIdentifierCountDoesNotEnterTheAnswersCost is the receipt for
// the TWELFTH occurrence of this branch's recurring failure, and the second one
// found INSIDE the repair that closed the eleventh.
//
// THE HOLE THE PREVIOUS REPAIR LEFT. Reducing the posting lists bounded each
// BUCKET by the producer's exclusion vocabulary. It said nothing about how many
// buckets ONE EPISODE READS, which is the count of distinct observation ids its
// first opportunity carries and which no ceiling limits. A supplier that puts
// each of N ids on two refused rows with DIFFERENT reasons keeps both positions
// in every bucket -- correctly, they are different reasons -- so the old form
// gathered 2N integers and sorted them to emit TWO strings.
//
// I SAW THIS AXIS AND DISMISSED IT, which is the part worth recording. The
// reasoning was that work proportional to an episode's own identifier list is
// linear in that episode's input and therefore not an amplification. That is
// true of the TRAVERSAL and false of the SORT and the gathering: the answer is
// bounded by the vocabulary, so anything that grows with N to produce it is
// work the answer does not need. A review lane named it on the published head.
//
// WHAT IS ASSERTED IS FLATNESS, NOT LINEARITY. A linear-in-N assertion is what
// the previous test made, and this shape satisfies it -- 11,875,552 B at N =
// 2,048 against 23,889,648 at 4,096 over 50 calls is a clean 2.01x. Only the
// per-call state being INDEPENDENT of N distinguishes the repair, so that is
// the assertion: the same number of calls must cost the same at both sizes.
func TestAnEpisodesOwnIdentifierCountDoesNotEnterTheAnswersCost(t *testing.T) {
	// Each id carries PAYLOAD_UNDECODABLE early and NO_TERMINAL_FACT late, the
	// late ones laid out in REVERSE, so no bucket's two positions are adjacent
	// and a position-gathering form has to sort them.
	build := func(n int) (*p2ExclusionIndex, []string) {
		excl := make([]predictioneval.Exclusion, 0, 2*n)
		ids := make([]string, n)
		for i := 0; i < n; i++ {
			ids[i] = "o" + strconv.Itoa(i)
			excl = append(excl, predictioneval.Exclusion{
				ObservationID: ids[i], Reason: predictioneval.ExclusionPayloadUndecodable,
			})
		}
		for i := n - 1; i >= 0; i-- {
			excl = append(excl, predictioneval.Exclusion{
				ObservationID: ids[i], Reason: predictioneval.ExclusionNoTerminalFact,
			})
		}
		return buildP2ExclusionIndex(excl), ids
	}
	const calls = 50
	measure := func(t *testing.T, n int) uint64 {
		t.Helper()
		ix, ids := build(n)
		var key predictioneval.AttemptKey
		// The ANSWER is checked on the same fixture that is measured, so a
		// version that got cheap by answering less would fail here first.
		want := []string{predictioneval.ExclusionPayloadUndecodable, predictioneval.ExclusionNoTerminalFact}
		if got := ix.exclusionsFor(key, ids); !reflect.DeepEqual(got, want) {
			t.Fatalf("n=%d: answered %v, want %v", n, got, want)
		}
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		for r := 0; r < calls; r++ {
			_ = ix.exclusionsFor(key, ids)
		}
		runtime.ReadMemStats(&b)
		return b.TotalAlloc - a.TotalAlloc
	}

	small := measure(t, 2048)
	large := measure(t, 4096)
	t.Logf("%d calls: 2048 ids -> %d bytes; 4096 ids -> %d bytes", calls, small, large)
	if ratio := float64(large) / float64(small); ratio > 1.25 {
		t.Fatalf("the same %d calls cost %.2fx more over twice the identifiers: the episode's own list is entering a bounded answer's cost",
			calls, ratio)
	}

	// AND THE ORDER IS THE FIXTURE'S, not the vocabulary's. PAYLOAD_UNDECODABLE
	// occupies position 0 and NO_TERMINAL_FACT's earliest is position n, so a
	// merge that kept the LAST position per reason, or that ordered by anything
	// other than the minimum, would swap these two.
	t.Run("and the two reasons come out in first-position order", func(t *testing.T) {
		ix, ids := build(8)
		got := ix.exclusionsFor(predictioneval.AttemptKey{}, ids)
		want := []string{predictioneval.ExclusionPayloadUndecodable, predictioneval.ExclusionNoTerminalFact}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("answered %v, want %v", got, want)
		}
		// Reversing the query order must not move the answer: the order is the
		// EXCLUSIONS' own, never the caller's.
		rev := make([]string, len(ids))
		for i := range ids {
			rev[i] = ids[len(ids)-1-i]
		}
		if got := ix.exclusionsFor(predictioneval.AttemptKey{}, rev); !reflect.DeepEqual(got, want) {
			t.Fatalf("a reversed query answered %v, want %v", got, want)
		}
	})
}

// TestTheRecordedFramedWidthIsTheWitnessOwn pins the one claim the derivation
// preflight rests on: the width a result records is the width its witness
// actually framed.
//
// IT IS INTERNAL BECAUSE THE PROPERTY IS. From outside, a width that disagreed
// with the framing would present as a genuine result refusing itself -- loud,
// but only in whatever test happened to mint one. This says it directly, and it
// says it on values that are NOT genuine, because the question is about the two
// framing modes and not about derivation: any P3bCaseResult, however built,
// must frame to the width the length-only pass reports for it.
//
// THE MODE IS WHY THERE IS ONE FUNCTION TO PIN. A hand-written mirror of the
// thirty-one direct part calls and three helper calls would need this test to
// enumerate the fields too, and an enumeration checked against an enumeration is
// two places to forget the same field. Here both sides run frameP3bResult.
// checkEveryFramingWidth drives every witness framing this package has for one
// randomized round and returns the names of the types it checked.
//
// IT RETURNS THE NAMES SO THE CENSUS CAN BIND TO THEM. The census below
// finds the witness-bearing types from the source; this returns
// the ones actually DRIVEN here. A Q3 lane showed the two could drift: an
// eighth witness-bearing type with both fields, whose framing nothing pinned,
// passed the census and this test together. Comparing the two sets is what
// makes "every framing is covered" a checked statement rather than a second
// hand-written enumeration replacing the one in doc.go.
func checkEveryFramingWidth(t *testing.T, rng *rand.Rand, round int) map[string]bool {
	t.Helper()
	// THE SET IS BUILT BY THE DRIVE, NOT DECLARED BESIDE IT. A first version
	// returned a hand-written literal of the seven names, and a Q3 lane deleted
	// a whole framing's driving block while leaving its name in the literal:
	// the suite stayed green, so an undriven framing passed quietly -- which is
	// the one thing this return value exists to prevent. Each check below
	// records its own name as it runs, so the name cannot outlive the drive.
	driven := map[string]bool{}
	// IT TAKES THE FRAMER, NOT TWO INTEGERS. A lane neutered one of these by
	// passing the same expression on both sides: the width assertion became a
	// tautology while the name stayed in the driven set and the census stayed
	// content. With the framing passed in, the bytes side cannot be spelled as
	// the width side.
	// THE DIFFERENTIAL IS RUN HERE, not assembled by the caller. A lane
	// neutered an earlier form three ways from the call site: spelling the
	// width side as the bytes side using a local the caller still had lying
	// around, passing a zero width with a framing closure that did nothing,
	// and passing a zero-value fixture so the randomized one never reached the
	// framer. Driving the SAME closure in both modes here removes all three
	// spellings, and an empty framing is refused outright.
	// THE SENTINEL PROVES THE ROUND'S FIXTURE REACHED THE FRAMER. An earlier
	// form refused an EMPTY framing, which is dead code: every framer opens
	// with a non-empty domain tag, so a zero-value fixture still frames ~66
	// bytes. A lane replaced the randomized fixture with the type's zero value
	// on both sides of all seven calls and the suite stayed green -- 400
	// randomized rounds became the same fixed check 400 times, and a genuine
	// length-only accounting bug in canonical.boolean then went undetected.
	// A value no zero and no constant can carry is planted in one string field
	// of every fixture instead.
	sentinel := "\x01p4-round-" + strconv.Itoa(round) + "-" + strconv.Itoa(rng.Intn(1<<30)) + "\x01"
	checkWidth := func(name string, width int, frame func(*canonical)) {
		t.Helper()
		driven[name] = true
		var b canonical
		frame(&b)
		if !bytes.Contains(b.bytes(), []byte(sentinel)) {
			t.Fatalf("round %d: %s did not frame this round's fixture: a drive that does not reach the randomized value checks nothing it claims to",
				round, name)
		}
		l := canonical{lenOnly: true}
		frame(&l)
		if l.framedLen() != len(b.bytes()) {
			t.Fatalf("round %d: %s length-only reports %d, the framing writes %d bytes",
				round, name, l.framedLen(), len(b.bytes()))
		}
		if width != len(b.bytes()) {
			t.Fatalf("round %d: %s width %d, framing %d bytes", round, name, width, len(b.bytes()))
		}
	}
	// A presence that varies, for the two result fixtures. UNKNOWN is a live
	// production value and these two were the framings the widening skipped.
	presence := func() Presence {
		if rng.Intn(2) == 0 {
			return PresenceUnknown
		}
		return PresenceKnown
	}
	text := func() string {
		n := rng.Intn(40)
		b := make([]byte, n)
		for i := range b {
			// Multi-byte runes included on purpose: the framing counts BYTES,
			// and a width computed over runes would pass an ASCII-only fixture.
			b[i] = byte("ab\xc3\xa9\x00 z"[rng.Intn(7)])
		}
		return string(b)
	}
	p3b := P3bCaseResult{
		Policy: text(), FactsetDigest: text(), RulesetID: text(),
		RulesetRawSHA256: text(), NativeConfigDigest: text(), ProjectionRefusal: text(),
		Trace: EntropyTraceBinding{
			Coordinates: EntropyCoordinates{
				DatasetID: text(), DatasetVersion: text(),
				CommonFactsetDigest: text(), PairedOpportunityID: text(),
				Trajectory: uint32(rng.Intn(1 << 20)),
			},
			RunID: text(), WordsSupplied: rng.Intn(1 << 20),
			WordsConsumed: rng.Intn(1 << 20), EntropyDigest: text(),
		},
		Projection: P3bProjection{
			Mode: text(), FactsetDigest: text(), EventID: text(),
			CutoffPosition: int64(rng.Intn(1 << 30)), CandidateIdentity: text(),
			OutcomeCount: rng.Intn(1000),
		},
		Choice: PolicyChoice{Present: rng.Intn(2) == 0, Index: rng.Intn(8), OutcomeID: text()},
		Stake:  Int64Fact{Presence: presence(), Value: int64(rng.Intn(1 << 30)), Reason: text()},
	}
	p3b.Projection.Stream.SelectionDigest = text()
	// Status is framed and was the ONE field this fixture always left
	// empty, so a mode-forgetting writer on it agreed in both modes at
	// length zero and survived the whole suite.
	p3b.Evaluation.Status = predictioneval.OrderedRulesStatus(text())
	p3b.Evaluation.Reason = text()
	p3b.Evaluation.StreamDigest = text()
	p3b.Evaluation.ConfigDigest = text()
	p3b.Evaluation.EntropyDigest = text()
	p3b.Evaluation.ConsumedInputDigest = text() + sentinel
	// Framed integers a first fixture left at zero, next to the Status field a
	// lane found empty: a length-only arm that mishandled their rendering
	// agreed with itself at "0".
	p3b.Evaluation.RawWordsConsumed = rng.Intn(1 << 20)
	p3b.Evaluation.BernoulliEvaluations = rng.Intn(1 << 20)
	p3b.Action = ActionMapping{MapVersion: text(), Policy: text(), NativeAction: text(),
		Class: ActionClass(text()), Legal: rng.Intn(2) == 0,
		Illegality: strs(rng, text), SkipReason: text()}

	checkWidth("P3bCaseResult", p3bResultFramedLen(p3b), func(c *canonical) { frameP3bResult(c, p3b) })

	p2 := P2CaseResult{
		Policy: text(), FactsetDigest: text(),
		Action: ActionMapping{MapVersion: text(), Policy: text(), NativeAction: text(),
			Class: ActionClass(text()), Legal: rng.Intn(2) == 0,
			Illegality: strs(rng, text), SkipReason: text()},
		Choice: PolicyChoice{Present: rng.Intn(2) == 0, Index: rng.Intn(8), OutcomeID: text()},
		Stake:  Int64Fact{Presence: presence(), Value: int64(rng.Intn(1 << 30)), Reason: text()},
	}
	p2.Binding.Digest = text()
	p2.Binding.ContractVersion = text()
	p2.Evaluation.CommonInputDigest = text()
	p2.Evaluation.Action = text()
	p2.Evaluation.ActionReason = text() + sentinel

	checkWidth("P2CaseResult", p2ResultFramedLen(p2), func(c *canonical) { frameP2Result(c, p2) })

	// AND THE OTHER FIVE FRAMINGS, for the reason the two above were not
	// enough. A Q3 mutant that made framePayoutEvidence's
	// ResolutionFactsDigest writer forget the length-only mode left the
	// whole suite green while the recorded width under-counted: two of the
	// seven framings were pinned and five were not, so the preflight could
	// go silently vacuous on any of them. Every framing this package has is
	// driven here, against the bytes its own writer produces.
	attempt := predictioneval.AttemptKey{
		CollectorEpoch: int64(rng.Intn(1 << 30)), CollectorSessionID: text(),
		PoolInstanceID: text(), AttemptID: uint64(rng.Intn(1 << 30)),
	}
	action := ActionMapping{MapVersion: text(), Policy: text(), NativeAction: text(),
		Class: ActionClass(text()), Legal: rng.Intn(2) == 0,
		Illegality: strs(rng, text), SkipReason: text()}
	choice := PolicyChoice{Present: rng.Intn(2) == 0, Index: rng.Intn(8), OutcomeID: text()}
	fact := func() Int64Fact {
		// UNKNOWN is a live production value -- derivePayout's refusal arm
		// sets Stake, Payout and Net to it -- and a fixture pinned to KNOWN
		// framed one string length every round.
		p := PresenceKnown
		if rng.Intn(2) == 0 {
			p = PresenceUnknown
		}
		return Int64Fact{Presence: p, Value: int64(rng.Intn(1 << 30)), Reason: text()}
	}
	oi, of := rng.Intn(1<<20), int64(rng.Intn(1<<30))

	fp := FactualPlacement{
		Attempt: attempt, FactsetDigest: text(), EventID: text(),
		CutoffPosition: int64(rng.Intn(1 << 30)), TerminalDecision: text(),
		RecordedChoiceIndex: &oi, RecordedChoiceOutcomeID: text(),
		RecordedFinalAmount: &of, RecordedTerminalSlot: &oi,
		Coherence: text(), StartedOnly: rng.Intn(2) == 0, CallPresent: rng.Intn(2) == 0,
		CallStartedObservationID: text(), CallStartedPosition: int64(rng.Intn(1 << 30)),
		Stake: &of, Slot: &oi, Returned: rng.Intn(2) == 0,
		LocalReasonOK: rng.Intn(2) == 0, ErrorClass: text() + sentinel,
	}
	checkWidth("FactualPlacement", factualPlacementFramedLen(fp), func(c *canonical) { frameFactualPlacement(c, fp) })

	dec := PolicyDecision{
		Policy: text(), Attempt: attempt, FactsetDigest: text(), EventID: text(),
		CutoffPosition: int64(rng.Intn(1 << 30)), Derivation: text() + sentinel,
		OutcomeIDs: strs(rng, text), Action: action, Choice: choice, Stake: fact(),
	}
	checkWidth("PolicyDecision", policyDecisionFramedLen(dec), func(c *canonical) { framePolicyDecision(c, dec) })

	pl := PlacementEvidence{
		ContractVersion: text(), Policy: text(), Attempt: attempt,
		FactsetDigest: text(), EventID: text(), Status: PlacementStatus(text()),
		Reasons: strs(rng, text), PolicyStake: fact(), AttributedOutcomeID: text(),
		AttributedCallObservationID: text() + sentinel,
		AttributedCallPosition:      int64(rng.Intn(1 << 30)),
	}
	checkWidth("PlacementEvidence", placementEvidenceFramedLen(pl), func(c *canonical) { framePlacementEvidence(c, pl) })

	po := PayoutEvidence{
		ContractVersion: text(), Policy: text(), Attempt: attempt,
		FactsetDigest: text(), EventID: text(), Outcome: PayoutOutcome(text()),
		ChoiceCorrect:            ChoiceVerdict(text()),
		PrimaryDenominatorMember: rng.Intn(2) == 0, PlacedBetDenominatorMember: rng.Intn(2) == 0,
		Stake: fact(), Payout: fact(), Net: fact(), Reasons: strs(rng, text),
		ResolutionFactsDigest: text(), Derivation: text() + sentinel,
	}
	// decisionWitness is framed and is unexported, so only an internal test
	// can vary it -- and it must be varied, or the last writer in the
	// framing is pinned against a constant.
	po.decisionWitness = text()
	checkWidth("PayoutEvidence", payoutEvidenceFramedLen(po), func(c *canonical) { framePayoutEvidence(c, po) })

	// THE RULESET FRAMING IS THE SEVENTH, and it is the one successive
	// hand-written enumerations of these seams kept leaving out. It carries
	// a width now like the other six.
	rid, rsha, rnat := text()+sentinel, text(), text()
	checkWidth("VerifiedP3bRuleset", rulesetFramedLen(rid, rsha, rnat), func(c *canonical) { frameRuleset(c, rid, rsha, rnat) })
	return driven
}

func TestTheRecordedFramedWidthIsTheWitnessOwn(t *testing.T) {
	rng := rand.New(rand.NewSource(20260920))
	for round := 0; round < 400; round++ {
		checkEveryFramingWidth(t, rng, round)
	}

	// AND THE LENGTH-ONLY MODE MUST COUNT THE HEX WRITER TOO, which no result
	// framing exercises -- so a mode that silently dropped strHexOf would pass
	// everything above while breaking claimKey's width if one were ever taken.
	for _, raw := range [][]byte{nil, {0}, []byte("hello"), make([]byte, 1000)} {
		var b canonical
		b.strHexOf(raw)
		l := canonical{lenOnly: true}
		l.strHexOf(raw)
		if l.framedLen() != len(b.bytes()) {
			t.Fatalf("strHexOf over %d bytes: width %d, framing %d", len(raw), l.framedLen(), len(b.bytes()))
		}
	}
}

// strs builds a random slice of framed strings, including the empty slice, so
// the count prefix and the per-element widths are both exercised.
func strs(rng *rand.Rand, text func() string) []string {
	// PAST ONE DIGIT ON PURPOSE. The count itself is framed as a decimal
	// string, so its own width grows at ten; a generator capped at three
	// pinned every count call site in its single-digit range only.
	n := rng.Intn(14)
	if n == 0 {
		return nil
	}
	out := make([]string, n)
	for i := range out {
		out[i] = text()
	}
	return out
}

// TestABoundedRefusalAnswerIsNotSizedOnTheCallersList is the receipt for the
// capacity hint sessionRefusalKinds no longer takes.
//
// THE SHAPE. The answer is the DISTINCT refusal kinds, which the closed
// refusal, anomaly and exclusion vocabularies bound at a few dozen. The list
// it reads is the caller's dataset. A hint of make([]string, 0, len(rs)) made
// that bounded answer allocate a caller-sized slice -- the posting-list defect
// one function over, and a Q3 lane showed that removing the hint again left
// the whole suite green, so the removal had no receipt.
//
// IT MEASURES THE ANSWER AGAINST THE INPUT, NOT A CONSTANT. Growing the input
// by 64x must not grow the allocation, because the vocabulary it collapses to
// has not changed.
func TestABoundedRefusalAnswerIsNotSizedOnTheCallersList(t *testing.T) {
	// A CLOSED SET OF KINDS, REPEATED. The distinct answer is 4 members at
	// every size, so any growth is the list's and not the answer's.
	kinds := []string{"A_REFUSAL", "B_REFUSAL", "C_REFUSAL", "D_REFUSAL"}
	build := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = kinds[i%len(kinds)]
		}
		return out
	}
	measure := func(rs []string) uint64 {
		// The input is built OUTSIDE the measured closure, and one untimed
		// call warms the path: both are lessons this round already paid for.
		_ = sessionRefusalKinds(rs)
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		got := sessionRefusalKinds(rs)
		runtime.ReadMemStats(&b)
		if len(got) != len(kinds) {
			t.Fatalf("the answer must be the %d distinct kinds, got %d", len(kinds), len(got))
		}
		return b.TotalAlloc - a.TotalAlloc
	}

	const small, large = 1 << 10, 1 << 16
	narrow, wide := build(small), build(large)
	narrowCost, wideCost := measure(narrow), measure(wide)
	t.Logf("%d refusals -> %d B; %d refusals -> %d B", small, narrowCost, large, wideCost)

	// THE ASSERTION IS A RATIO AGAINST THE INPUT'S GROWTH. A slice sized on the
	// caller's list grows with it; an answer sized on the vocabulary does not.
	// The map the dedup keeps is bounded by the distinct set too, so the honest
	// floor here is flat.
	if grown := float64(wideCost) - float64(narrowCost); grown > float64(large-small) {
		t.Fatalf("a %dx longer list of the SAME %d kinds cost %.0f more bytes (%d against %d): the answer is sized on the caller's list",
			large/small, len(kinds), grown, wideCost, narrowCost)
	}
}

// TestARecordedWidthIsRequiredAndNotJustRecorded pins the ruleset preflight
// behaviourally, which its cost test cannot.
//
// WHY A SECOND TEST. TestAnEditedRulesetIdentityIsRefusedWithoutFramingTheEdit
// is an allocation ratio over runtime.MemStats, and a Q3 lane pointed out that
// removing both width terms from check leaves every FUNCTIONAL assertion in the
// package green -- only that ratio moves. A ratio is a soft instrument to rest
// a gate on. The shape below is the one input that separates the repaired gate
// from the unrepaired one by its ANSWER rather than by its cost: a handle whose
// witness is correct and whose recorded width is not.
//
// IT IS INTERNAL BECAUSE ONLY THIS PACKAGE CAN MINT ONE. The external tests
// cannot set an unexported field, so from outside there is no way to hold a
// value that has a right witness and a wrong width.
func TestARecordedWidthIsRequiredAndNotJustRecorded(t *testing.T) {
	const id, raw, nat = "ruleset-id", "raw-sha", "native-digest"
	good := VerifiedP3bRuleset{
		RulesetID: id, RawSHA256: raw, NativeConfigDigest: nat,
		witness:   rulesetWitness(id, raw, nat),
		framedLen: rulesetFramedLen(id, raw, nat),
	}
	if err := good.check(); err != nil {
		t.Fatalf("a correctly built handle must pass: %v", err)
	}

	for _, tc := range []struct {
		name  string
		width int
	}{
		// Zero is the width an un-set field holds, and it is also what a width
		// computed WITHOUT the length-only mode returns -- the slip that made
		// an earlier form of this preflight vacuous.
		{"an unrecorded width", 0},
		{"a negative width", -1},
		{"a width one byte short", good.framedLen - 1},
		{"a width one byte long", good.framedLen + 1},
	} {
		t.Run(tc.name+" is refused, although the witness is right", func(t *testing.T) {
			bad := good
			bad.framedLen = tc.width
			if bad.witness != rulesetWitness(bad.RulesetID, bad.RawSHA256, bad.NativeConfigDigest) {
				t.Fatal("the fixture must keep a CORRECT witness, or it proves nothing about the width")
			}
			if err := bad.check(); !errors.Is(err, ErrRulesetNotVerified) {
				t.Fatalf("check accepted a handle whose recorded width is %d against a framing of %d: got %v",
					tc.width, good.framedLen, err)
			}
		})
	}

	// AND THE WITNESS IS STILL REQUIRED, so this test cannot be satisfied by a
	// gate that checks the width alone -- the failure a lane demonstrated on a
	// synthetic eighth seam.
	t.Run("and a right width with a wrong witness is refused too", func(t *testing.T) {
		bad := good
		bad.witness = rulesetWitness(id+"x", raw, nat)
		if bad.framedLen != rulesetFramedLen(bad.RulesetID, bad.RawSHA256, bad.NativeConfigDigest) {
			t.Fatal("the fixture must keep a CORRECT width, or it proves nothing about the witness")
		}
		if err := bad.check(); !errors.Is(err, ErrRulesetNotVerified) {
			t.Fatalf("check accepted a handle with a wrong witness: %v", err)
		}
	})

	// AND SO DO THE OTHER SIX, which rested on an allocation ratio alone. A
	// lane removed the width clause from all six derived() methods at once:
	// 211 top-level tests passed and the ONLY failure was a B/op comparison in
	// TestAnEditedResultIsRefusedWithoutFramingTheEdit. Removing it from one
	// type, with that ratio test excluded, survived outright. A ratio is a soft
	// instrument to rest a gate on -- it reports how much a refusal cost, not
	// whether it happened -- so every type is held here to an ANSWER instead:
	// a CORRECT witness beside a wrong width must still be refused.
	//
	// WHAT THESE ROWS DO NOT PROVE, stated so the scope is not read wider than
	// it is: each computes its expected width with the very ...FramedLen helper
	// it then drives, so an off-by-one in the helper moves both sides together
	// and passes here. What holds the helper against the framed bytes is
	// checkWidth, and what holds checkWidth against a namesake is
	// TestTheWidthAndPartsOraclesCannotBeSpelledFromTheFramer. These rows hold
	// the CLAUSE in derived(), not the helper. The fixtures are zero values, so
	// they are also blind to a width fault that depends on a field's value.
	for _, tc := range []struct {
		name    string
		good    int
		atWidth func(int) bool
	}{
		{"P2CaseResult", p2ResultFramedLen(P2CaseResult{}), func(w int) bool {
			v := P2CaseResult{}
			v.witness, v.framedLen = p2ResultWitness(P2CaseResult{}), w
			return v.derived()
		}},
		{"P3bCaseResult", p3bResultFramedLen(P3bCaseResult{}), func(w int) bool {
			v := P3bCaseResult{}
			v.witness, v.framedLen = p3bResultWitness(P3bCaseResult{}), w
			return v.derived()
		}},
		{"PayoutEvidence", payoutEvidenceFramedLen(PayoutEvidence{}), func(w int) bool {
			v := PayoutEvidence{}
			v.witness, v.framedLen = payoutEvidenceWitness(PayoutEvidence{}), w
			return v.derived()
		}},
		{"FactualPlacement", factualPlacementFramedLen(FactualPlacement{}), func(w int) bool {
			v := FactualPlacement{}
			v.witness, v.framedLen = factualPlacementWitness(FactualPlacement{}), w
			return v.derived()
		}},
		{"PolicyDecision", policyDecisionFramedLen(PolicyDecision{}), func(w int) bool {
			v := PolicyDecision{}
			v.witness, v.framedLen = policyDecisionWitness(PolicyDecision{}), w
			return v.derived()
		}},
		{"PlacementEvidence", placementEvidenceFramedLen(PlacementEvidence{}), func(w int) bool {
			v := PlacementEvidence{}
			v.witness, v.framedLen = placementEvidenceWitness(PlacementEvidence{}), w
			return v.derived()
		}},
	} {
		t.Run(tc.name+" requires its recorded width, not just a witness", func(t *testing.T) {
			if tc.good <= 0 {
				t.Fatalf("the fixture frames %d bytes: a width test needs a positive width", tc.good)
			}
			if !tc.atWidth(tc.good) {
				t.Fatalf("a correctly built %s must be derived at its own width %d", tc.name, tc.good)
			}
			for _, w := range []int{0, -1, tc.good - 1, tc.good + 1} {
				if tc.atWidth(w) {
					t.Errorf("%s accepted a recorded width of %d against a framing of %d, although the witness is CORRECT: the width clause is not load-bearing",
						tc.name, w, tc.good)
				}
			}
		})
	}
}

// framedByAnIndependentOracle builds the documented framing from the contract
// rather than from canonical: each part is an eight-byte big-endian length
// followed by the part's bytes.
//
// IT EXISTS BECAUSE THE OTHER WIDTH TEST IS A DIFFERENTIAL, NOT AN ORACLE.
// TestTheRecordedFramedWidthIsTheWitnessOwn compares one framer run with bytes
// on against the same framer run with bytes off, so it is sensitive to exactly
// one thing: the two modes disagreeing. It is blind by construction to WHICH
// fields a framer enumerates and to HOW it delimits them, because both sides
// lose a field, or a boundary, together. A Q3 lane demonstrated both: dropping
// a field from frameRuleset, and replacing its three length-prefixed parts with
// one prefix over their concatenation, each left the whole suite green.
func framedByAnIndependentOracle(parts ...string) []byte {
	var out []byte
	for _, p := range parts {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		out = append(out, n[:]...)
		out = append(out, p...)
	}
	return out
}

// TestTheRulesetFramingIsTheOneTheContractDescribes holds frameRuleset to an
// independently constructed framing: the domain tag and the three identity
// fields, each length-prefixed, in that order.
//
// THIS IS THE FIELD SET AND THE BOUNDARIES, which the differential cannot see.
// check's own doc says "changing an identity field after verification also
// fails, because the witness covers all three"; that sentence is a claim about
// the field set, and this is what makes it a receipt.
func TestTheRulesetFramingIsTheOneTheContractDescribes(t *testing.T) {
	rng := rand.New(rand.NewSource(20260921))
	text := func() string {
		n := rng.Intn(40)
		b := make([]byte, n)
		for i := range b {
			b[i] = byte("ab\xc3\xa9\x00 z"[rng.Intn(7)])
		}
		return string(b)
	}
	const tag = "p4offline-verified-ruleset-witness"
	for round := 0; round < 200; round++ {
		id, raw, nat := text(), text(), text()
		want := framedByAnIndependentOracle(tag, id, raw, nat)

		var c canonical
		frameRuleset(&c, id, raw, nat)
		if got := c.bytes(); !bytes.Equal(got, want) {
			t.Fatalf("round %d: frameRuleset wrote %d bytes, the contract's framing is %d",
				round, len(got), len(want))
		}
		if got := rulesetFramedLen(id, raw, nat); got != len(want) {
			t.Fatalf("round %d: the recorded width is %d, the contract's framing is %d bytes",
				round, got, len(want))
		}

		// AND EACH FIELD MUST MOVE THE FRAMING ON ITS OWN. A framer that
		// dropped one field would still agree with a three-field oracle for
		// every value of the field it dropped, so the oracle above is paired
		// with a per-field sensitivity check.
		for i, alt := range [][3]string{
			{id + "z", raw, nat},
			{id, raw + "z", nat},
			{id, raw, nat + "z"},
		} {
			var d canonical
			frameRuleset(&d, alt[0], alt[1], alt[2])
			if bytes.Equal(d.bytes(), c.bytes()) {
				t.Fatalf("round %d: changing identity field %d did not change the framing: the witness does not cover it",
					round, i)
			}
		}
	}
}

// TestTheWidthIsHonestOnValuesTheFixturesNeverBuild covers shapes the
// randomized width fixtures do not reach, which a Q3 lane censused: every
// framed integer they draw is NON-NEGATIVE and every optional pointer they set
// is NON-NIL, so neither the minus sign nor the absent arm is framed anywhere
// else. A writer that mishandled the length-only mode on either agreed with
// itself everywhere the fixtures looked.
//
// THE COLLECTION ROW IS DIFFERENT AND IS KEPT ANYWAY. The generator was capped
// below ten when that row was written; it reaches past ten now, so the row is
// no longer the only thing covering a multi-digit count. It stays as the fixed
// case beside a randomized one, because a generator can be narrowed again.
func TestTheWidthIsHonestOnValuesTheFixturesNeverBuild(t *testing.T) {
	t.Run("a negative framed integer", func(t *testing.T) {
		// Reachable in production: a losing payout sets Net to the negated
		// stake, and frameFact frames it through i64.
		po := PayoutEvidence{
			ContractVersion: "v", Policy: "p", FactsetDigest: "d", EventID: "e",
			Net: Int64Fact{Presence: PresenceKnown, Value: -12345, Reason: "loss"},
		}
		var c canonical
		framePayoutEvidence(&c, po)
		if got, want := payoutEvidenceFramedLen(po), len(c.bytes()); got != want {
			t.Fatalf("width %d, framing %d bytes: the minus sign is not counted", got, want)
		}
	})

	t.Run("a collection past one digit", func(t *testing.T) {
		// The count itself is framed as a decimal string, so its OWN width
		// grows at ten, and this row fixes one case there.
		po := PayoutEvidence{ContractVersion: "v", Reasons: make([]string, 12)}
		var c canonical
		framePayoutEvidence(&c, po)
		if got, want := payoutEvidenceFramedLen(po), len(c.bytes()); got != want {
			t.Fatalf("width %d, framing %d bytes: the count's own width is wrong past one digit", got, want)
		}
	})

	t.Run("an absent optional", func(t *testing.T) {
		// Every optional pointer the placement fixture sets is non-nil, so the
		// absent arm of serializeOptionalInt/Int64 is never framed either way.
		var fp FactualPlacement
		var c canonical
		frameFactualPlacement(&c, fp)
		if got, want := factualPlacementFramedLen(fp), len(c.bytes()); got != want {
			t.Fatalf("width %d, framing %d bytes: the absent arm is counted differently in the two modes", got, want)
		}
	})
}

// witnessSite is one struct that carries a producer-only witness.
type witnessSite struct{ name, pos string }

// witnessComparedIn reports every type whose methods compare a `.witness`
// selector against a RECOMPUTATION -- a call expression, which is what
// re-framing the value looks like.
//
// THE "AGAINST A CALL" PART IS THE WHOLE PREDICATE, and a first draft that
// accepted any comparison was vacuous: `f.witness != ""` is a comparison, it
// appears in every one of these methods, and a type whose check is
// `witness != "" && framedLen == …FramedLen(f)` satisfied it while accepting
// every equal-width forgery. That is exactly the mutant this predicate exists
// to catch, so a non-empty test does not count and a literal on the other side
// does not count.
func witnessComparedIn(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]bool{}
	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}

	// A CALLEE THAT READS THE STORED WITNESS RECOMPUTES NOTHING, so the names
	// of the ones that do are collected before the walk and struck off it. A
	// lane defeated the form above with `v.witness == v.recordedWitness()`,
	// whose callee returns the stored field: the name ends in Witness and the
	// call is handed the receiver, so both of the other clauses hold, and the
	// comparison is `w == w` -- true for every forgery. None of this package's
	// seven recomputations reads a stored witness; each frames the identity
	// fields and digests them, so the rule costs nothing here. Names are not
	// qualified by receiver, so a collision strikes off an honest namesake
	// too -- that direction reports a MISSING comparison, which is the safe
	// one. The residual in the accepting direction is a callee declared
	// OUTSIDE these files, whose body this walk cannot see; all seven of this
	// package's are package-level functions in this directory.
	// AND A CALLEE THAT NEVER READS WHAT IT IS HANDED recomputes nothing
	// either. `witness != emptyWitness(v)` passes every other clause -- the
	// name ends in Witness, the receiver is handed in, no stored witness is
	// read -- and returns a constant, which is the `witness != hexEncode(nil)`
	// defeat respelled with the suffix the recognizer looks for. A
	// recomputation must at minimum READ its own input, so one that mentions
	// neither a parameter nor its receiver anywhere in its body is struck off.
	doesNotFrame := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || !strings.HasSuffix(fd.Name.Name, "Witness") {
				continue
			}
			given := map[string]bool{}
			lists := []*ast.FieldList{fd.Type.Params}
			if fd.Recv != nil {
				lists = append(lists, fd.Recv)
			}
			for _, fl := range lists {
				if fl == nil {
					continue
				}
				for _, field := range fl.List {
					for _, n := range field.Names {
						given[n.Name] = true
					}
				}
			}
			// AND IT MUST REACH THE FRAMING. Reading the input is not
			// enough: `_ = v.A; return ""` reads it and returns a constant.
			// Every recomputation in this package builds a canonical and
			// digests it, so a callee that mentions neither is not one.
			used := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				if strings.HasPrefix(id.Name, "frame") || id.Name == "canonical" {
					used = true
				}
				return true
			})
			if !used {
				doesNotFrame[fd.Name.Name] = true
			}
		}
	}

	readsStoredWitness := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "witness" {
					readsStoredWitness[fd.Name.Name] = true
				}
				return true
			})
		}
	}

	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 || fd.Body == nil {
				continue
			}
			recv := fd.Recv.List[0].Type
			if star, ok := recv.(*ast.StarExpr); ok {
				recv = star.X
			}
			id, ok := recv.(*ast.Ident)
			if !ok {
				continue
			}
			// ONLY WHERE THE ANSWER IS USED. A comparison assigned to a
			// variable that is then discarded -- the shape a "temporarily
			// disabled" gate takes -- is not a verification, so the walk
			// starts from the expressions a decision is actually made on.
			recvName := ""
			if names := fd.Recv.List[0].Names; len(names) == 1 {
				recvName = names[0].Name
			}
			// MENTIONS THE RECEIVER, somewhere in the call's arguments. A
			// recomputation of THIS value's witness has to be handed this
			// value; a lane defeated an earlier form with a call on a foreign
			// type's zero value, which recomputes nothing about the receiver.
			mentionsRecv := func(e ast.Expr) bool {
				found := false
				ast.Inspect(e, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok && recvName != "" && id.Name == recvName {
						found = true
					}
					return true
				})
				return found
			}
			decide := func(n ast.Node) {
				ast.Inspect(n, func(n ast.Node) bool {
					// A DISCARDED CLOSURE IS NOT A DECISION. Its body holds a
					// return of its own, which an earlier form of this walk
					// took for the method's.
					if _, ok := n.(*ast.FuncLit); ok {
						return false
					}
					// || IS NOT SKIPPED, and that is deliberate: check is a
					// REFUSAL-shaped disjunction, so its witness comparison is
					// a disjunct and skipping them would blind this walk to
					// the seam it exists for. The cost is that a comparison
					// placed behind an always-true || operand is not told
					// apart from a reachable one; doc.go records that.
					be, ok := n.(*ast.BinaryExpr)
					if !ok || (be.Op != token.EQL && be.Op != token.NEQ) {
						return true
					}
					for i, side := range []ast.Expr{be.X, be.Y} {
						sel, ok := side.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "witness" {
							continue
						}
						other := be.Y
						if i == 1 {
							other = be.X
						}
						call, ok := other.(*ast.CallExpr)
						if !ok {
							continue
						}
						// AND AGAINST A RECOMPUTATION OF THE WITNESS, not any
						// call at all. A lane defeated an earlier form of this
						// with `witness != hexEncode(nil)`, which is the empty
						// string spelled as a call -- literally the mutant the
						// predicate exists to catch.
						var name string
						switch fn := call.Fun.(type) {
						case *ast.Ident:
							name = fn.Name
						case *ast.SelectorExpr:
							name = fn.Sel.Name
						}
						if strings.HasSuffix(name, "Witness") && mentionsRecv(call) &&
							!readsStoredWitness[name] && !doesNotFrame[name] {
							out[id.Name] = true
						}
					}
					return true
				})
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if _, ok := n.(*ast.FuncLit); ok {
					return false
				}
				// AND THE STATEMENT MUST CONSUME THE ANSWER. A lane put the
				// real comparison in an `if` whose body was a discard and in
				// an empty `switch` tag, on all seven production types, and
				// the walk counted both -- the "temporarily disabled gate"
				// this predicate names, in the two spellings it did not cover.
				consumes := func(b *ast.BlockStmt) bool {
					if b == nil {
						return false
					}
					found := false
					ast.Inspect(b, func(n ast.Node) bool {
						// NOT INTO A CLOSURE. A lane put `_ = func() bool {
						// return true }` in the body: its return is not this
						// statement's, and this predicate is the one place the
						// outer walk's own FuncLit skip did not reach.
						if _, ok := n.(*ast.FuncLit); ok {
							return false
						}
						switch x := n.(type) {
						case *ast.ReturnStmt, *ast.BranchStmt:
							found = true
						case *ast.AssignStmt:
							// `_ = v.witness` is the discard spelling a lane
							// used; an assignment counts only where something
							// other than the blank identifier receives it.
							for _, lhs := range x.Lhs {
								if id, ok := lhs.(*ast.Ident); !ok || id.Name != "_" {
									found = true
								}
							}
						}
						return true
					})
					return found
				}
				switch x := n.(type) {
				case *ast.ReturnStmt:
					for _, r := range x.Results {
						decide(r)
					}
				case *ast.IfStmt:
					if x.Cond == nil {
						return true
					}
					// AN `else if` IS NOT A CONSUMER BY ITSELF. Counting any
					// non-block else as one accepted a gate every branch of
					// which discards -- the disabled-gate shape exactly one
					// `else if` away from the one already covered here.
					var elseConsumes func(ast.Stmt) bool
					elseConsumes = func(e ast.Stmt) bool {
						switch y := e.(type) {
						case *ast.BlockStmt:
							return consumes(y)
						case *ast.IfStmt:
							return consumes(y.Body) || elseConsumes(y.Else)
						default:
							return false
						}
					}
					if consumes(x.Body) || elseConsumes(x.Else) {
						decide(x.Cond)
					}
				case *ast.SwitchStmt:
					// A case clause with an empty body decides nothing, and a
					// switch of nothing but those is the empty-tag shape under
					// another spelling.
					live := false
					if x.Body != nil {
						for _, cl := range x.Body.List {
							if cc, ok := cl.(*ast.CaseClause); ok && len(cc.Body) > 0 {
								live = true
							}
						}
					}
					if x.Tag != nil && live {
						decide(x.Tag)
					}
				}
				return true
			})
		}
	}
	return out
}

// witnessCensusOfDir walks the production files of dir and reports every struct
// carrying an unexported `witness` field, and which of those carry no
// `framedLen int` beside it.
//
// IT IS A FUNCTION SO THAT ITS CONTROL CAN DRIVE THE SAME WALK. The sibling
// census twenty lines up factors censusOfDir out for exactly that reason, and
// the rule it states applies here word for word: a machine check whose own
// mechanisms nothing asserts is a convention again. A control that
// re-implemented this walk would pass unchanged while this one went blind --
// which a Q3 lane demonstrated against the first draft of this census, by
// deleting its reporting arm and watching both tests stay green.
//
// THE TWO MATCHES ARE DELIBERATELY ASYMMETRIC, and the asymmetry is the whole
// safety argument. What TRIGGERS the requirement is matched WIDELY: any field
// named `witness`, whatever its type is spelled as. What SATISFIES it is
// matched NARROWLY: a field named `framedLen` whose type is the identifier
// `int`. Both directions therefore fail towards reporting. A first draft
// required the witness to be spelled `string`, and a lane put
// `type q3Digest string` -- this package's own idiom, the one ActionClass,
// PayoutOutcome, ChoiceVerdict and PlacementStatus already use -- into
// production with the whole suite green.
//
// IT WALKS EVERY STRUCT TYPE, not only the named ones, so a witness moved
// inside an anonymous `seal struct{...}` is still seen. Anonymous structs are
// named by their position, which is what a reader needs to find them.
func witnessCensusOfDir(t *testing.T, dir string) (withWitness, missing []witnessSite) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		// Named structs are labelled by their type name; every other struct
		// literal type is labelled by where it is.
		named := map[*ast.StructType]string{}
		ast.Inspect(f, func(n ast.Node) bool {
			if ts, ok := n.(*ast.TypeSpec); ok {
				if st, ok := ts.Type.(*ast.StructType); ok {
					named[st] = ts.Name.Name
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			var witness, width bool
			for _, fld := range st.Fields.List {
				for _, id := range fld.Names {
					switch id.Name {
					case "witness":
						// ANY type. See the asymmetry note above.
						witness = true
					case "framedLen":
						if x, ok := fld.Type.(*ast.Ident); ok && x.Name == "int" {
							width = true
						}
					}
				}
			}
			if !witness {
				return true
			}
			label, ok := named[st]
			if !ok {
				label = "anonymous struct"
			}
			s := witnessSite{label, fset.Position(st.Pos()).String()}
			withWitness = append(withWitness, s)
			if !width {
				missing = append(missing, s)
			}
			return true
		})
	}
	return withWitness, missing
}

// TestEveryWitnessBearingTypeCarriesAFramedWidth is the mechanical answer to
// the way the framed-width preflight was rolled out: by enumeration.
//
// ENUMERATING THEM IN PROSE KEPT FAILING. Three successive hand-written
// enumerations of these seams were short, each missing a neighbour that took a
// caller-sized value -- most recently VerifiedP3bRuleset, measured at 1.0073x
// the caller's edit on every use. A sentence cannot stop that recurring; a
// walk over the source can.
//
// THE RULE. A type that carries an unexported `witness` answers a question by
// re-framing its own fields, so it must also carry `framedLen int` and compare
// it first -- a rejection test that refuses a width-changing edit before any
// byte is materialized. A NEW witness type is therefore a failing test, not a
// prose omission.
//
// IT DOES NOT CHECK THAT THE WIDTH IS USED WELL. That is
// TestTheRecordedFramedWidthIsTheWitnessOwn's job, and the two are deliberately
// separate: this one answers "is there one", that one answers "is it right".
func TestEveryWitnessBearingTypeCarriesAFramedWidth(t *testing.T) {
	withWitness, missing := witnessCensusOfDir(t, ".")

	if len(missing) > 0 {
		for _, m := range missing {
			t.Errorf("%s (%s) carries a witness with no framedLen: it re-frames its own fields to answer a question, so a caller-sized edit is paid for before it is refused",
				m.name, m.pos)
		}
		t.Fatalf("%d of %d witness-bearing types carry no framed width", len(missing), len(withWitness))
	}

	// NOTHING HERE PINS A COUNT, and a census that stopped finding types would
	// satisfy every assertion above it by finding nothing -- which is exactly
	// the failure mode the prose enumeration had. What stops that is the
	// driven/found comparison at the END of this test: checkEveryFramingWidth
	// names the framings it drove without consulting the census, so a walk gone
	// blind is reported as seven stale drives instead of as silence. A lane
	// renamed the field on all seven production types to check that, and got
	// the seven. What stays invisible is a NEW seam spelling its witness
	// something else: the walk cannot recognise it and no drive names it, so
	// neither side of that comparison knows it exists.
	//
	// This test catches a type the walk RECOGNISES; the control below is what
	// holds the recognizer itself.
	// AND A WIDTH IS NOT A VERIFICATION. The field census alone enforces
	// "declares a width", which a Q3 lane showed is the wrong lesson to teach:
	// it added an eighth type whose check compared the WIDTH ONLY, bumped the
	// count this test prints, and the suite went green while an equal-width
	// forgery was accepted. The width refuses a width-changing edit cheaply;
	// only the witness verifies CONTENT. So a witness-bearing type must also
	// COMPARE its witness somewhere in its own methods.
	compared := witnessComparedIn(t, ".")
	for _, s := range withWitness {
		if s.name == "anonymous struct" {
			t.Errorf("a witness lives in an anonymous struct at %s: give it a named type so its comparison can be checked", s.pos)
			continue
		}
		if !compared[s.name] {
			t.Errorf("%s (%s) carries a witness that none of its methods compares: a width refuses a width-changing edit, but only the witness verifies content, so this type accepts any equal-width forgery",
				s.name, s.pos)
		}
	}

	// AND EVERY ONE MUST BE DRIVEN BY THE WIDTH TEST. Replacing doc.go's prose
	// enumeration with this census would be no gain if the width test kept a
	// second hand-written enumeration of its own -- and a Q3 lane showed it
	// did: an eighth witness-bearing type, carrying both fields but framed by
	// nothing the width test drives, passed this census and that test
	// together. The two sets are compared instead of counted.
	driven := checkEveryFramingWidth(t, rand.New(rand.NewSource(1)), 0)
	found := map[string]bool{}
	for _, s := range withWitness {
		found[s.name] = true
	}
	for name := range found {
		if !driven[name] {
			t.Errorf("%s carries a witness but TestTheRecordedFramedWidthIsTheWitnessOwn does not drive its framing: a width nothing checks is a width that can go vacuous",
				name)
		}
	}
	for name := range driven {
		if !found[name] {
			t.Errorf("the width test drives %s, which the census does not find as witness-bearing: one of the two is stale", name)
		}
	}
}

// TestTheWitnessCensusSeesWhatItClaimsTo holds the census's own walk to
// synthetic sources, by calling THE SAME FUNCTION over a temporary directory.
//
// EVERY EVASION BELOW WAS FOUND IN PRODUCTION BY A Q3 LANE, against a first
// draft that matched the witness only when its type was spelled `string` and
// only on named struct types. Each row is that lane's construct, kept as a
// regression: a future narrowing of the recognizer fails here rather than in a
// release.
func TestTheWitnessCensusSeesWhatItClaimsTo(t *testing.T) {
	dir := t.TempDir()
	const src = `package p

type q3Digest string
type aliasString = string

type plain struct {
	A         string
	witness   string
	framedLen int
}

type widthless struct {
	B       string
	witness string
}

type namedStringWitness struct {
	witness q3Digest
}

type aliasWitness struct {
	witness aliasString
}

type pointerWitness struct {
	witness *string
}

type parenthesizedWitness struct {
	witness (string)
}

type nested struct {
	seal struct {
		witness string
	}
}

type wrongWidthType struct {
	witness   string
	framedLen int64
}

type unrelated struct {
	C string
}

type comparesAgainstACall struct {
	A         string
	witness   string
	framedLen int
}

func frameSynthetic(s string) string { return s + "w" }

func comparesAgainstACallWitness(v comparesAgainstACall) string { return frameSynthetic(v.A) }

func (v comparesAgainstACall) derived() bool {
	return v.witness != "" && v.witness == comparesAgainstACallWitness(v)
}

type comparesInAPointerMethod struct {
	A         string
	witness   string
	framedLen int
}

func comparesInAPointerMethodWitness(v *comparesInAPointerMethod) string { return frameSynthetic(v.A) }

func (v *comparesInAPointerMethod) check() bool {
	return v.witness != comparesInAPointerMethodWitness(v)
}

type comparesAgainstAZeroValueWitness struct {
	A         string
	witness   string
	framedLen int
}

func (v comparesAgainstAZeroValueWitness) derived() bool {
	// A recomputation that is handed no receiver recomputes nothing.
	return v.witness == comparesAgainstACallWitness(comparesAgainstACall{})
}

type comparesInsideADiscardedClosure struct {
	A         string
	witness   string
	framedLen int
}

func (v comparesInsideADiscardedClosure) derived() bool {
	verify := func() bool { return v.witness == comparesAgainstACallWitness(comparesAgainstACall{A: v.A}) }
	_ = verify
	return v.framedLen > 0
}

type comparesBehindAnAlwaysTrueOr struct {
	A         string
	witness   string
	framedLen int
}

func (v comparesBehindAnAlwaysTrueOr) derived() bool {
	return v.framedLen >= 0 || v.witness == comparesAgainstACallWitness(comparesAgainstACall{A: v.A})
}

type comparesAgainstAnotherNonWitnessCall struct {
	A         string
	witness   string
	framedLen int
}

type comparesInAnIfWithADeadBody struct {
	A         string
	witness   string
	framedLen int
}

func (v comparesInAnIfWithADeadBody) derived() bool {
	if v.witness == comparesAgainstACallWitness(comparesAgainstACall{A: v.A}) {
		_ = v.witness
	}
	return v.framedLen > 0
}

type comparesAsAnEmptySwitchTag struct {
	A         string
	witness   string
	framedLen int
}

func (v comparesAsAnEmptySwitchTag) derived() bool {
	switch v.witness == comparesAgainstACallWitness(comparesAgainstACall{A: v.A}) {
	}
	return v.framedLen > 0
}

type comparesViaAZeroArgReceiverMethod struct {
	A         string
	witness   string
	framedLen int
}

func (v comparesViaAZeroArgReceiverMethod) recomputedWitness() string { return frameSynthetic(v.A) }

func (v comparesViaAZeroArgReceiverMethod) derived() bool {
	// The most natural spelling of "recompute my own witness"; an arity clause
	// in the recognizer rejected it, which is a false positive on honest code.
	return v.witness == v.recomputedWitness()
}

type comparesAgainstItsOwnStoredWitness struct {
	A         string
	witness   string
	framedLen int
}

// And this is the shape that separates the two: a callee whose name ends in
// Witness, handed the receiver, returning the STORED field rather than
// recomputing anything. It satisfies every other clause the recognizer has.
func (v comparesAgainstItsOwnStoredWitness) recordedWitness() string { return v.witness }

func (v comparesAgainstItsOwnStoredWitness) derived() bool {
	// w == w. True for a forgery, true for a zero value, true for anything.
	return v.witness == v.recordedWitness() && v.framedLen > 0
}

type comparesAgainstAReadAndDiscardWitness struct {
	A         string
	witness   string
	framedLen int
}

// It READS its input and throws it away, so a rule asking only that the input
// be mentioned accepts it. Nothing is framed, so nothing is recomputed.
func readAndDiscardWitness(v comparesAgainstAReadAndDiscardWitness) string {
	_ = v.A
	return ""
}

func (v comparesAgainstAReadAndDiscardWitness) derived() bool {
	return v.witness != readAndDiscardWitness(v) && v.framedLen > 0
}

type comparesAgainstAConstantWitness struct {
	A         string
	witness   string
	framedLen int
}

// It is handed the receiver and never looks at it: the empty string spelled as
// a call, wearing the suffix the recognizer looks for.
func emptyWitness(v comparesAgainstAConstantWitness) string { return "" }

func (v comparesAgainstAConstantWitness) derived() bool {
	return v.witness != emptyWitness(v) && v.framedLen > 0
}

type gateDisabledViaElseIf struct {
	A         string
	witness   string
	framedLen int
}

// comparesInAnIfWithADeadBody plus one else if: every branch discards, so the
// gate decides nothing at all.
func (v gateDisabledViaElseIf) derived() bool {
	if v.witness == comparesAgainstACallWitness(comparesAgainstACall{A: v.A}) {
		_ = v.witness
	} else if v.framedLen > 0 {
		_ = v.framedLen
	}
	return true
}

type comparesInASwitchCaseWithAnEmptyBody struct {
	A         string
	witness   string
	framedLen int
}

// The switch HAS a case clause, unlike comparesAsAnEmptySwitchTag; the clause
// has no body, which is what the live-clause requirement is actually for.
func (v comparesInASwitchCaseWithAnEmptyBody) derived() bool {
	switch v.witness == comparesAgainstACallWitness(comparesAgainstACall{A: v.A}) {
	case true:
	}
	return v.framedLen > 0
}

type comparesWithOnlyAClosureInTheBody struct {
	A         string
	witness   string
	framedLen int
}

// The if body holds nothing but a discarded closure whose return is its own.
func (v comparesWithOnlyAClosureInTheBody) derived() bool {
	if v.witness == comparesAgainstACallWitness(comparesAgainstACall{A: v.A}) {
		_ = func() bool { return true }
	}
	return v.framedLen > 0
}

type comparesInsideAClosureCalledInline struct {
	A         string
	witness   string
	framedLen int
}

// Behind an always-true operand AND inside a closure, so the || residual this
// walk accepts cannot be what carries it.
func (v comparesInsideAClosureCalledInline) derived() bool {
	return v.framedLen >= 0 || func() bool {
		return v.witness == comparesAgainstACallWitness(comparesAgainstACall{A: v.A})
	}()
}

func (v comparesAgainstAnotherNonWitnessCall) derived() bool {
	// It is not hexEncode, so a recognizer that special-cases that one name
	// fails here; and it DOES mention the receiver, so the receiver rule
	// cannot mask the name rule and leave this control vacuous.
	return v.witness != strconv.Itoa(len(v.A)) && v.framedLen > 0
}

type comparesAgainstALiteral struct {
	witness   string
	framedLen int
}

func (v comparesAgainstALiteral) derived() bool {
	// A non-empty test is a comparison and is NOT a verification.
	return v.witness != "" && v.framedLen > 0
}

type comparesNothing struct {
	witness   string
	framedLen int
}

func (v comparesNothing) derived() bool { return v.framedLen > 0 }

type comparesAgainstANonWitnessCall struct {
	witness   string
	framedLen int
}

func hexEncode(b []byte) string { return "" }

func (v comparesAgainstANonWitnessCall) derived() bool {
	// The empty string spelled as a call: a lane used exactly this to defeat
	// an earlier form of the predicate.
	return v.witness != hexEncode(nil) && v.framedLen > 0
}

type comparesButDiscardsTheAnswer struct {
	A         string
	witness   string
	framedLen int
}

func (v comparesButDiscardsTheAnswer) derived() bool {
	// THE RECEIVER IS MENTIONED, deliberately: without it mentionsRecv already
	// rejected this row and the statement-kind restriction it exists for had
	// no control at all.
	content := v.witness == comparesAgainstACallWitness(comparesAgainstACall{A: v.A})
	_ = content
	return v.framedLen > 0
}
`
	// A _test.go file must be SKIPPED by the walk, so one is written too.
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	decoy := "package p\n\ntype inATestFile struct{ witness string }\n"
	if err := os.WriteFile(filepath.Join(dir, "a_test.go"), []byte(decoy), 0o600); err != nil {
		t.Fatal(err)
	}

	withWitness, missing := witnessCensusOfDir(t, dir)
	seen := map[string]bool{}
	for _, s := range withWitness {
		seen[s.name] = true
	}
	for _, want := range []string{
		"plain", "widthless", "namedStringWitness", "aliasWitness",
		"pointerWitness", "parenthesizedWitness", "wrongWidthType",
	} {
		if !seen[want] {
			t.Errorf("the walk did not see %s as witness-bearing", want)
		}
	}
	if seen["unrelated"] {
		t.Error("the walk saw a struct with no witness as witness-bearing")
	}
	if seen["inATestFile"] {
		t.Error("the walk read a _test.go file; it must read production files only")
	}
	// The nested anonymous struct is reported at its position, not by the name
	// of the type that encloses it, so it is matched on count rather than name.
	anon := 0
	for _, s := range withWitness {
		if s.name == "anonymous struct" {
			anon++
		}
	}
	if anon != 1 {
		t.Errorf("the walk saw %d anonymous witness-bearing structs, want 1 (the one inside `nested`)", anon)
	}
	if len(withWitness) != 28 {
		names := make([]string, 0, len(withWitness))
		for _, s := range withWitness {
			names = append(names, s.name)
		}
		sort.Strings(names)
		t.Errorf("the walk saw %d witness-bearing structs, want 28: %v", len(withWitness), names)
	}

	// AND THE COMPARISON RECOGNIZER NEEDS ITS OWN CONTROL, by the same rule:
	// a Q3 lane replaced witnessComparedIn's call-expression requirement with
	// an unconditional true -- making it accept `witness != ""`, which its own
	// comment calls the mutant it exists to catch -- and the suite stayed
	// green, because nothing drove it over sources whose answer is known.
	compared := witnessComparedIn(t, dir)
	// comparesBehindAnAlwaysTrueOr is deliberately on the POSITIVE side: the
	// walk does not skip ||, because check is a refusal-shaped disjunction and
	// skipping them would blind it to the seam it exists for. This row records
	// that residual as a checked fact rather than leaving it to be discovered.
	for _, want := range []string{
		"comparesAgainstACall", "comparesInAPointerMethod", "comparesBehindAnAlwaysTrueOr",
		"comparesViaAZeroArgReceiverMethod",
	} {
		if !compared[want] {
			t.Errorf("the comparison recognizer did not see %s comparing its witness against a recomputation", want)
		}
	}
	for _, notWant := range []string{
		"comparesAgainstALiteral", "comparesNothing", "plain",
		// Every one of these was built by a Q3 lane to defeat the predicate
		// and is kept as a regression: a call that is not a witness
		// recomputation; a second one under a different name, so a recognizer
		// that special-cases the first still fails; a real comparison whose
		// answer is thrown away; a recomputation handed no receiver; one
		// inside a discarded closure; and one behind an always-true ||.
		"comparesAgainstANonWitnessCall", "comparesAgainstAnotherNonWitnessCall",
		"comparesButDiscardsTheAnswer", "comparesAgainstAZeroValueWitness",
		"comparesInsideADiscardedClosure", "comparesInAnIfWithADeadBody",
		"comparesAsAnEmptySwitchTag", "comparesAgainstItsOwnStoredWitness",
		// And five more a sixth lane drove through or found uncontrolled: a
		// callee that ignores what it is handed; a gate whose every branch
		// discards, one else if past the covered shape; a switch whose case
		// clause has no body; an if body holding nothing but a discarded
		// closure; and a comparison inside a closure called inline.
		"comparesAgainstAConstantWitness", "gateDisabledViaElseIf",
		"comparesInASwitchCaseWithAnEmptyBody", "comparesWithOnlyAClosureInTheBody",
		"comparesInsideAClosureCalledInline", "comparesAgainstAReadAndDiscardWitness",
	} {
		if compared[notWant] {
			t.Errorf("the comparison recognizer counted %s, whose witness is never compared against a recomputation", notWant)
		}
	}

	// AND IT MUST REPORT EXACTLY THE ONES WITH NO USABLE WIDTH -- everything
	// above except `plain`, including `wrongWidthType`, whose framedLen is not
	// an int and therefore does not satisfy the rule.
	got := map[string]bool{}
	for _, m := range missing {
		got[m.name] = true
	}
	if got["plain"] {
		t.Error("the walk reported a struct that DOES carry framedLen int as missing a width")
	}
	for _, want := range []string{
		"widthless", "namedStringWitness", "aliasWitness", "pointerWitness",
		"parenthesizedWitness", "wrongWidthType",
	} {
		if !got[want] {
			t.Errorf("the walk did not report %s as missing a width", want)
		}
	}
	if len(missing) != 7 {
		t.Errorf("the walk reported %d missing widths, want 7 (six named plus the nested anonymous one)", len(missing))
	}
}

// splitFramedParts parses a framing back into its length-prefixed parts with an
// independent reader: eight big-endian bytes of length, then that many bytes.
// It refuses anything it cannot consume exactly, so a malformed framing is a
// failure rather than a short read.
func splitFramedParts(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	for len(b) > 0 {
		if len(b) < 8 {
			t.Fatalf("a framing ended mid-prefix with %d bytes left", len(b))
		}
		n := binary.BigEndian.Uint64(b[:8])
		b = b[8:]
		if uint64(len(b)) < n {
			t.Fatalf("a framing declares a %d-byte part with %d bytes left", n, len(b))
		}
		out = append(out, string(b[:n]))
		b = b[n:]
	}
	return out
}

// TestEveryWitnessFramingHasItsOwnTagAndItsOwnParts is the structural receipt
// for all seven framings, and it exists because the width test cannot be one.
//
// WHAT THE WIDTH TEST CANNOT SEE. Running a framer with bytes on against the
// same framer with bytes off catches the two modes disagreeing, and nothing
// else: drop a field, collapse two length prefixes into one, or give a framing
// another framing's domain tag, and BOTH sides change together. Q3 lanes
// demonstrated all three against six of the seven framings with the whole suite
// green -- including giving PlacementEvidence the PolicyDecision tag, which is
// the cross-artifact separation the tags exist for, and dropping
// PlacementEvidence's three attribution fields, which is what DerivePayout
// reads.
//
// WHAT THIS DRIVES. One randomized round: it builds each of the seven framings'
// values, parses the framing back into parts with an independent reader, and
// compares those parts against an ordered list DECLARED HERE from the framer's
// field list -- the domain tag, then every field in order, each rendered as the
// contract renders it. It returns the framings it checked, so the caller can
// hold the table to the same binding the width drive has.
//
// IT IS A FIELD-BY-FIELD ORACLE, which it was not when it only counted parts.
// A dropped field, a collapsed delimiter, a borrowed tag, a swapped pair and a
// field written as a constant each move a part. What it still cannot see is a
// field the TYPE has and the framer never writes: the list mirrors the framer,
// not the struct, and TestAWitnessTypeCannotGrowAFieldQuietly is the tripwire
// for that.
func framingPartsRound(t *testing.T, rng *rand.Rand, round int) (map[string]bool, map[string][]string) {
	t.Helper()
	text := func() string {
		n := rng.Intn(40) + 1
		b := make([]byte, n)
		for i := range b {
			b[i] = byte("ab\xc3\xa9\x00 z"[rng.Intn(7)])
		}
		return string(b)
	}
	strsOf := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = text()
		}
		return out
	}
	num := int64(100)
	rb := func() bool { return rng.Intn(2) == 0 }
	// DRAWN, NOT STEPPED. A fixed step from a fixed start gives every round the
	// same 107, 114, 121 ..., so a framer writing one of those literals in place
	// of the field agreed with the table in all 64 rounds. Seven production
	// mutants survived the whole suite on that, and one of them was shown to
	// accept a forged FactualPlacement whose Attempt.CollectorEpoch had been
	// edited after minting. The step is drawn so a literal cannot agree twice.
	nz := func() int64 { num += 7 + int64(rng.Intn(1<<20)); return num }
	// EVERY FRAMED FIELD GETS A NON-ZERO, DISTINCT VALUE. A field left at its
	// zero makes the mutant that replaces it with that zero EQUIVALENT for this
	// fixture, which is how a first build of this table could not see a blanked
	// bool: the value list and the framer agreed on "false" either way.
	fact := func() Int64Fact {
		p := PresenceKnown
		if rb() {
			p = PresenceUnknown
		}
		return Int64Fact{Presence: p, Value: nz(), Reason: text()}
	}
	attemptOne := predictioneval.AttemptKey{
		CollectorEpoch: nz(), CollectorSessionID: text(),
		PoolInstanceID: text(), AttemptID: uint64(nz()),
	}
	// THE CARDINALITIES ARE DRAWN TOO. Fixed lengths pinned every c.count site
	// to one literal, which is the same defect as a field pinned to one value
	// and left all four surviving. The ranges reach 0 and past 9, where the
	// count's own framed width grows.
	illeg, outcomes, reasons := strsOf(rng.Intn(4)), strsOf(rng.Intn(6)), strsOf(rng.Intn(14))
	// frameAction writes four strings, a bool, a count, one string per
	// illegality and a skip reason; frameChoice and frameFact write three each.
	const actionFixed, choiceParts, factParts = 7, 3, 3
	action := ActionMapping{MapVersion: text(), Policy: text(), NativeAction: text(),
		Class: ActionClass(text()), Legal: rb(), Illegality: illeg, SkipReason: text()}
	choice := PolicyChoice{Present: rb(), Index: int(nz()) % 97, OutcomeID: text()}
	// DISTINCT BACKING VALUES per optional, so a swap between two of them is
	// visible; sharing one variable made three of them interchangeable.
	oi, of := int(nz()), nz()
	oi2, of2 := int(nz()), nz()

	p2 := P2CaseResult{Policy: text(), FactsetDigest: text(), Action: action, Choice: choice, Stake: fact()}
	p2.Binding.ContractVersion, p2.Binding.Digest = text(), text()
	p2.Evaluation.CommonInputDigest, p2.Evaluation.Action, p2.Evaluation.ActionReason = text(), text(), text()

	p3b := P3bCaseResult{Policy: text(), FactsetDigest: text(), RulesetID: text(),
		RulesetRawSHA256: text(), NativeConfigDigest: text(), ProjectionRefusal: text(),
		Action: action, Choice: choice, Stake: fact()}
	p3b.Trace.Coordinates = EntropyCoordinates{DatasetID: text(), DatasetVersion: text(),
		CommonFactsetDigest: text(), PairedOpportunityID: text(), Trajectory: uint32(nz())}
	p3b.Trace.RunID, p3b.Trace.EntropyDigest = text(), text()
	p3b.Trace.WordsSupplied, p3b.Trace.WordsConsumed = int(nz()), int(nz())
	p3b.Projection = P3bProjection{Mode: text(), FactsetDigest: text(), EventID: text(),
		CutoffPosition: nz(), CandidateIdentity: text(), OutcomeCount: int(nz())}
	p3b.Projection.Stream.SelectionDigest = text()
	p3b.Evaluation.Status = predictioneval.OrderedRulesStatus(text())
	p3b.Evaluation.Reason, p3b.Evaluation.StreamDigest = text(), text()
	p3b.Evaluation.ConfigDigest, p3b.Evaluation.EntropyDigest = text(), text()
	p3b.Evaluation.ConsumedInputDigest = text()
	p3b.Evaluation.RawWordsConsumed, p3b.Evaluation.BernoulliEvaluations = int(nz()), int(nz())

	fp := FactualPlacement{Attempt: attemptOne, FactsetDigest: text(), EventID: text(),
		CutoffPosition: nz(), TerminalDecision: text(),
		RecordedChoiceIndex: &oi, RecordedChoiceOutcomeID: text(),
		RecordedFinalAmount: &of, RecordedTerminalSlot: &oi2, Coherence: text(),
		StartedOnly: rb(), CallPresent: rb(),
		CallStartedObservationID: text(), CallStartedPosition: nz(),
		Stake: &of2, Slot: &oi2, Returned: rb(), LocalReasonOK: rb(), ErrorClass: text()}
	// The absent-optional row is deliberately the OTHER polarity on every bool,
	// so between the two rows each boolean is framed both ways.
	fpNil := FactualPlacement{Attempt: attemptOne, FactsetDigest: text(), EventID: text(),
		CutoffPosition: nz(), TerminalDecision: text(), RecordedChoiceOutcomeID: text(),
		Coherence: text(), CallStartedObservationID: text(), CallStartedPosition: nz(),
		ErrorClass: text()}

	dec := PolicyDecision{Policy: text(), Attempt: attemptOne, FactsetDigest: text(),
		EventID: text(), CutoffPosition: nz(), Derivation: text(), OutcomeIDs: outcomes,
		Action: action, Choice: choice, Stake: fact()}
	pl := PlacementEvidence{ContractVersion: text(), Policy: text(), Attempt: attemptOne,
		FactsetDigest: text(), EventID: text(), Status: PlacementStatus(text()), Reasons: reasons,
		PolicyStake: fact(), AttributedOutcomeID: text(), AttributedCallObservationID: text(),
		AttributedCallPosition: nz()}
	po := PayoutEvidence{ContractVersion: text(), Policy: text(), Attempt: attemptOne,
		FactsetDigest: text(), EventID: text(), Outcome: PayoutOutcome(text()),
		ChoiceCorrect:            ChoiceVerdict(text()),
		PrimaryDenominatorMember: rb(), PlacedBetDenominatorMember: rb(),
		Stake: fact(), Payout: fact(), Net: fact(),
		Reasons: reasons, ResolutionFactsDigest: text(), Derivation: text()}
	po.decisionWitness = text()
	rid, rsha, rnat := text(), text(), text()

	// THE RENDERINGS ARE THE CONTRACT'S, written out here rather than called
	// from canonical: i64, u64, boolean and count all funnel through str, so
	// each is exactly one length-prefixed part carrying its decimal or literal
	// spelling.
	i := func(v int64) string { return strconv.FormatInt(v, 10) }
	u := func(v uint64) string { return strconv.FormatUint(v, 10) }
	bl := func(v bool) string { return strconv.FormatBool(v) }
	cnt := func(n int) string { return strconv.FormatInt(int64(n), 10) }
	optI := func(v *int) []string {
		if v == nil {
			return []string{bl(false)}
		}
		return []string{bl(true), i(int64(*v))}
	}
	optI64 := func(v *int64) []string {
		if v == nil {
			return []string{bl(false)}
		}
		return []string{bl(true), i(*v)}
	}
	actionOf := func(a ActionMapping) []string {
		p := []string{a.MapVersion, a.Policy, a.NativeAction, string(a.Class),
			bl(a.Legal), cnt(len(a.Illegality))}
		p = append(p, a.Illegality...)
		return append(p, a.SkipReason)
	}
	choiceOf := func(ch PolicyChoice) []string {
		return []string{bl(ch.Present), i(int64(ch.Index)), ch.OutcomeID}
	}
	factOf := func(f Int64Fact) []string {
		return []string{string(f.Presence), i(f.Value), f.Reason}
	}
	attemptOf := func(a predictioneval.AttemptKey) []string {
		return []string{i(a.CollectorEpoch), a.CollectorSessionID, a.PoolInstanceID, u(a.AttemptID)}
	}
	join := func(groups ...[]string) []string {
		var out []string
		for _, g := range groups {
			out = append(out, g...)
		}
		return out
	}

	checked := map[string]bool{}
	byRow := map[string][]string{}
	// framing is the TYPE whose framing a row drives; name distinguishes rows
	// that drive the same framing with different shapes. The set compared
	// against the width drive is the framing set, not the row set.
	// parts is a SECOND, independent literal. A lane kept a row and derived its
	// want from the framer itself -- the binding below only compares name sets,
	// so the row stayed and asserted nothing, and a real dropped field then
	// went undetected. A want computed from the framer cannot agree with a
	// number written by hand.
	for _, tc := range []struct {
		name    string
		framing string
		parts   int
		want    []string
		frame   func(*canonical)
	}{
		{"P2CaseResult", "P2CaseResult", 8 + actionFixed + len(illeg) + choiceParts + factParts, join(
			[]string{"p4offline-p2-result-witness", p2.Policy, p2.FactsetDigest,
				p2.Binding.ContractVersion, p2.Binding.Digest,
				p2.Evaluation.CommonInputDigest, p2.Evaluation.Action, p2.Evaluation.ActionReason},
			actionOf(p2.Action), choiceOf(p2.Choice), factOf(p2.Stake)),
			func(c *canonical) { frameP2Result(c, p2) }},

		{"P3bCaseResult", "P3bCaseResult", 31 + actionFixed + len(illeg) + choiceParts + factParts, join(
			[]string{"p4offline-p3b-result-witness", p3b.Policy, p3b.FactsetDigest,
				p3b.RulesetID, p3b.RulesetRawSHA256, p3b.NativeConfigDigest, p3b.ProjectionRefusal,
				p3b.Trace.Coordinates.DatasetID, p3b.Trace.Coordinates.DatasetVersion,
				p3b.Trace.Coordinates.CommonFactsetDigest, p3b.Trace.Coordinates.PairedOpportunityID,
				u(uint64(p3b.Trace.Coordinates.Trajectory)), p3b.Trace.RunID,
				i(int64(p3b.Trace.WordsSupplied)), i(int64(p3b.Trace.WordsConsumed)),
				p3b.Trace.EntropyDigest,
				p3b.Projection.Mode, p3b.Projection.FactsetDigest, p3b.Projection.EventID,
				i(p3b.Projection.CutoffPosition), p3b.Projection.CandidateIdentity,
				i(int64(p3b.Projection.OutcomeCount)), p3b.Projection.Stream.SelectionDigest,
				string(p3b.Evaluation.Status), p3b.Evaluation.Reason, p3b.Evaluation.StreamDigest,
				p3b.Evaluation.ConfigDigest, p3b.Evaluation.EntropyDigest,
				p3b.Evaluation.ConsumedInputDigest,
				i(int64(p3b.Evaluation.RawWordsConsumed)), i(int64(p3b.Evaluation.BernoulliEvaluations))},
			actionOf(p3b.Action), choiceOf(p3b.Choice), factOf(p3b.Stake)),
			func(c *canonical) { frameP3bResult(c, p3b) }},

		{"FactualPlacement", "FactualPlacement", 18 + 2*5, join(
			[]string{"p4offline-factual-placement-witness"}, attemptOf(fp.Attempt),
			[]string{fp.FactsetDigest, fp.EventID, i(fp.CutoffPosition), fp.TerminalDecision},
			optI(fp.RecordedChoiceIndex), []string{fp.RecordedChoiceOutcomeID},
			optI64(fp.RecordedFinalAmount), optI(fp.RecordedTerminalSlot),
			[]string{fp.Coherence, bl(fp.StartedOnly), bl(fp.CallPresent),
				fp.CallStartedObservationID, i(fp.CallStartedPosition)},
			optI64(fp.Stake), optI(fp.Slot),
			[]string{bl(fp.Returned), bl(fp.LocalReasonOK), fp.ErrorClass}),
			func(c *canonical) { frameFactualPlacement(c, fp) }},

		{"FactualPlacement, optionals absent", "FactualPlacement", 18 + 5, join(
			[]string{"p4offline-factual-placement-witness"}, attemptOf(fpNil.Attempt),
			[]string{fpNil.FactsetDigest, fpNil.EventID, i(fpNil.CutoffPosition), fpNil.TerminalDecision},
			optI(fpNil.RecordedChoiceIndex), []string{fpNil.RecordedChoiceOutcomeID},
			optI64(fpNil.RecordedFinalAmount), optI(fpNil.RecordedTerminalSlot),
			[]string{fpNil.Coherence, bl(fpNil.StartedOnly), bl(fpNil.CallPresent),
				fpNil.CallStartedObservationID, i(fpNil.CallStartedPosition)},
			optI64(fpNil.Stake), optI(fpNil.Slot),
			[]string{bl(fpNil.Returned), bl(fpNil.LocalReasonOK), fpNil.ErrorClass}),
			func(c *canonical) { frameFactualPlacement(c, fpNil) }},

		{"PolicyDecision", "PolicyDecision", 11 + len(outcomes) + actionFixed + len(illeg) + choiceParts + factParts, join(
			[]string{"p4offline-policy-decision-witness", dec.Policy}, attemptOf(dec.Attempt),
			[]string{dec.FactsetDigest, dec.EventID, i(dec.CutoffPosition), dec.Derivation,
				cnt(len(dec.OutcomeIDs))}, dec.OutcomeIDs,
			actionOf(dec.Action), choiceOf(dec.Choice), factOf(dec.Stake)),
			func(c *canonical) { framePolicyDecision(c, dec) }},

		{"PlacementEvidence", "PlacementEvidence", 11 + len(reasons) + factParts + 3, join(
			[]string{"p4offline-placement-evidence-witness", pl.ContractVersion, pl.Policy},
			attemptOf(pl.Attempt),
			[]string{pl.FactsetDigest, pl.EventID, string(pl.Status), cnt(len(pl.Reasons))},
			pl.Reasons, factOf(pl.PolicyStake),
			[]string{pl.AttributedOutcomeID, pl.AttributedCallObservationID,
				i(pl.AttributedCallPosition)}),
			func(c *canonical) { framePlacementEvidence(c, pl) }},

		{"PayoutEvidence", "PayoutEvidence", 13 + 3*factParts + 1 + len(reasons) + 3, join(
			[]string{"p4offline-payout-evidence-witness", po.ContractVersion, po.Policy},
			attemptOf(po.Attempt),
			[]string{po.FactsetDigest, po.EventID, string(po.Outcome), string(po.ChoiceCorrect),
				bl(po.PrimaryDenominatorMember), bl(po.PlacedBetDenominatorMember)},
			factOf(po.Stake), factOf(po.Payout), factOf(po.Net),
			[]string{cnt(len(po.Reasons))}, po.Reasons,
			[]string{po.ResolutionFactsDigest, po.Derivation, po.decisionWitness}),
			func(c *canonical) { framePayoutEvidence(c, po) }},

		{"VerifiedP3bRuleset", "VerifiedP3bRuleset", 4,
			[]string{"p4offline-verified-ruleset-witness", rid, rsha, rnat},
			func(c *canonical) { frameRuleset(c, rid, rsha, rnat) }},
	} {
		checked[tc.framing] = true
		byRow[tc.name] = tc.want
		// THE COUNT IS DECLARED TWICE, from the field list and as the length of
		// the value list. A want derived from the framer cannot satisfy both.
		if len(tc.want) != tc.parts {
			t.Fatalf("round %d, %s: the value list has %d entries, the field list requires %d",
				round, tc.name, len(tc.want), tc.parts)
		}
		var c canonical
		tc.frame(&c)
		// AND THE WIDTH IS CHECKED HERE -- a SECOND barrier, currently masked.
		// Deleting these five lines leaves the whole suite green, because
		// checkEveryFramingWidth drives the same two modes over the same seven
		// framings. It is kept because that drive's fixtures are held only to
		// carrying one sentinel string, so they can be neutered without failing
		// it, and this row cannot be neutered without failing the value
		// assertions below. Masked is not dead: nothing protects it today.
		//
		// The width is checked here, where every field of the fixture is
		// drawn and asserted part by part. checkEveryFramingWidth drives the
		// two modes too, but its fixtures are only held to carrying one
		// sentinel string, so replacing them with zero values leaves it
		// satisfied -- and a genuine length-only accounting fault in
		// canonical.boolean then goes undetected. This row cannot be neutered
		// that way without failing the value assertions below it first.
		lo := canonical{lenOnly: true}
		tc.frame(&lo)
		if lo.framedLen() != len(c.bytes()) {
			t.Fatalf("round %d, %s: length-only reports %d, the framing writes %d bytes",
				round, tc.name, lo.framedLen(), len(c.bytes()))
		}
		got := splitFramedParts(t, c.bytes())
		if len(got) == 0 || got[0] != tc.want[0] {
			first := ""
			if len(got) > 0 {
				first = got[0]
			}
			t.Fatalf("round %d, %s: the framing opens with %q, its own domain tag is %q",
				round, tc.name, first, tc.want[0])
		}
		if len(got) != len(tc.want) {
			t.Fatalf("round %d, %s: the framing wrote %d parts, its fields require %d: a part missing is a field the witness no longer covers, a part too few is two fields sharing one length prefix",
				round, tc.name, len(got), len(tc.want))
		}
		for k := range tc.want {
			if got[k] != tc.want[k] {
				t.Fatalf("round %d, %s: part %d is %q, the field list puts %q there: a part carrying the wrong value is a field the witness does not actually cover",
					round, tc.name, k, got[k], tc.want[k])
			}
		}
	}
	return checked, byRow
}

// TestEveryWitnessFramingHasItsOwnTagAndItsOwnParts is the structural and
// field-level receipt for all seven framings, and it exists because the width
// test cannot be either.
//
// WHAT THE WIDTH TEST CANNOT SEE. Running a framer with bytes on against the
// same framer with bytes off catches the two modes disagreeing, and nothing
// else: drop a field, collapse two length prefixes into one, give a framing
// another framing's domain tag, or write a field's source as a constant, and
// BOTH sides change together. Q3 lanes demonstrated every one of those against
// six of the seven framings with the whole suite green -- including giving
// PlacementEvidence the PolicyDecision tag, which is the cross-artifact
// separation the tags exist for, and dropping PlacementEvidence's three
// attribution fields, which is what DerivePayout reads.
//
// WHAT THIS ASSERTS. Each framing is parsed back into parts by an independent
// reader and compared against an ORDERED LIST OF PART VALUES declared from the
// framer's field list: the domain tag, then every field in order, each rendered
// as the contract renders it. A dropped field, a collapsed delimiter, a
// borrowed tag, a swapped pair and a field written as a constant each move one
// of those parts.
//
// IT RUNS OVER RANDOMIZED ROUNDS, and that is not decoration. A fixture that
// pins a field to one value makes the mutant that writes THAT value a constant
// equivalent, which is how three of these survived a census that only ever
// substituted the zero: the fixture said Present: true, Index: 3,
// Presence: KNOWN, so writing those literals was invisible. Every value is
// drawn per round now, booleans included and independently of each other, so a
// constant and a swap both have to agree with the draw every round.
func TestEveryWitnessFramingHasItsOwnTagAndItsOwnParts(t *testing.T) {
	rng := rand.New(rand.NewSource(20260922))
	var checked map[string]bool
	// AND THE DRAW ITSELF IS HELD, part by part. Replacing the boolean draw
	// with a constant left the whole suite green, and a framer then writing
	// that same constant in place of the field survived too -- the fixture and
	// the mutant agreed because neither ever moved. Every part that carries one
	// value in all 64 rounds is reported here, so a draw that stops drawing
	// fails rather than quietly weakening every row beneath it.
	const rounds = 64
	seen := map[string][]map[string]bool{}
	occ := map[string][]int{}
	for round := 0; round < rounds; round++ {
		var byRow map[string][]string
		checked, byRow = framingPartsRound(t, rng, round)
		for name, want := range byRow {
			for len(seen[name]) < len(want) {
				seen[name] = append(seen[name], map[string]bool{})
				occ[name] = append(occ[name], 0)
			}
			for k, v := range want {
				seen[name][k][v] = true
				occ[name][k]++
			}
		}
	}
	// ONLY WHERE A SLOT IS THE SAME FIELD IN EVERY ROUND. Rows carry
	// variable-length runs, so a slot past the first of them holds different
	// fields in different rounds, and a slot reached in ONE round would be
	// reported as carrying one value "in all 64 rounds" -- which is what
	// happened: at seed 1 this test failed on a PolicyDecision slot reached
	// three times at the seed below and once at that one. So the slot must be
	// occupied in every round to be judged, and the number of slots that
	// clears that bar is pinned, or the held region could shrink in silence.
	// What the bar leaves out -- every slot past a row's first variable-length
	// run -- is held instead by
	// TestEverySharedFramingHelperIsSensitiveToEveryFieldItFrames, which does
	// not depend on a slot at all.
	held := 0
	for name, parts := range seen {
		for k, vals := range parts {
			if k == 0 || occ[name][k] != rounds {
				continue
			}
			if constantPartsByDesign[name+"/"+strconv.Itoa(k)] {
				continue
			}
			held++
			if len(vals) > 1 {
				continue
			}
			only := ""
			for v := range vals {
				only = v
			}
			t.Errorf("%s part %d carries %q in every one of the %d rounds: a part pinned to one value makes the mutant that writes THAT value equivalent, which is the defect this table's draw exists to stop",
				name, k, only, rounds)
		}
	}
	if held < heldSlotFloor {
		t.Errorf("the distinctness ratchet judges %d slots, the floor is %d: a row that grew a variable-length run earlier, or a fixture that stopped reaching a slot, silently narrows what this test holds",
			held, heldSlotFloor)
	}

	// AND THE TABLE IS HELD TO THE SAME BINDING AS THE WIDTH DRIVE. A lane
	// deleted a whole row from it and the suite stayed green, which is the
	// failure checkEveryFramingWidth's driven set was invented to stop. The two
	// sets are compared, so a row cannot leave quietly.
	driven := checkEveryFramingWidth(t, rand.New(rand.NewSource(2)), 0)
	for name := range driven {
		if !checked[name] {
			t.Errorf("%s is driven for its width but no row here checks its parts: a framing with no value list is a framing whose fields nothing covers", name)
		}
	}
	for name := range checked {
		if !driven[name] {
			t.Errorf("this table checks %s, which the width drive does not know: one of the two is stale", name)
		}
	}
}

// TestAWitnessTypeCannotGrowAFieldQuietly is the tripwire for the one thing
// neither the census nor the parts table can see.
//
// BOTH OF THOSE ARE MIRRORS OF THE FRAMER, not of the type. A lane added an
// exported field to PayoutEvidence with no framing for it and the whole suite
// stayed green: the framer did not write it, so the declared part list did not
// expect it, and the two agreed about a field neither covered. doc.go's own
// argument against mirrors -- "a second place a future field can be forgotten"
// -- applies to the repair, and this is what it costs to answer it without a
// third enumeration.
//
// IT IS A RATCHET, NOT AN ORACLE. It cannot say a new field SHOULD be framed;
// some deliberately are not, and the witness docs say which. What it can do is
// make adding one a decision somebody records here, rather than a silence.
func TestAWitnessTypeCannotGrowAFieldQuietly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields int
		v      any
	}{
		{"P2CaseResult", 9, P2CaseResult{}},
		{"VerifiedP3bRuleset", 6, VerifiedP3bRuleset{}},
		{"P3bCaseResult", 14, P3bCaseResult{}},
		{"PayoutEvidence", 18, PayoutEvidence{}},
		{"FactualPlacement", 21, FactualPlacement{}},
		{"PolicyDecision", 12, PolicyDecision{}},
		{"PlacementEvidence", 13, PlacementEvidence{}},
		// AND THE TYPES THE FRAMINGS REACH THROUGH. frameFact, frameAction and
		// frameChoice are shared by up to seven framings, so a field added to
		// one of these leaves every one of their witnesses at once -- and a
		// lane showed all four grow a field with the suite green.
		{"Int64Fact", 3, Int64Fact{}},
		{"ActionMapping", 7, ActionMapping{}},
		{"PolicyChoice", 3, PolicyChoice{}},
		{"AttemptKey", 4, predictioneval.AttemptKey{}},
	} {
		if got := reflect.TypeOf(tc.v).NumField(); got != tc.fields {
			t.Errorf("%s now has %d fields, this table says %d: if the field is framed, add it to its framer AND to the value list in TestEveryWitnessFramingHasItsOwnTagAndItsOwnParts; if it is deliberately unframed, say so at the witness and update the count here",
				tc.name, got, tc.fields)
		}
	}

	// AND THE REACHED-THROUGH TYPES ARE COUNTED TRANSITIVELY, not listed. The
	// four rows above are a hand-written enumeration with nothing binding it,
	// and the framings reach through more than four: a lane grew a field on
	// EntropyCoordinates (exported and unexported), EntropyTraceBinding,
	// P3bProjection and P2ConfigBinding, and every one survived the whole
	// suite. A transitive count trips on ANY nested growth and needs no second
	// list to keep in step with the first.
	for _, tc := range []struct {
		name  string
		total int
		v     any
	}{
		{"P2CaseResult", 95, P2CaseResult{}},
		{"VerifiedP3bRuleset", 19, VerifiedP3bRuleset{}},
		{"P3bCaseResult", 146, P3bCaseResult{}},
		{"PayoutEvidence", 25, PayoutEvidence{}},
		{"FactualPlacement", 25, FactualPlacement{}},
		{"PolicyDecision", 29, PolicyDecision{}},
		{"PlacementEvidence", 20, PlacementEvidence{}},
	} {
		if w := opaqueFieldUnder(reflect.TypeOf(tc.v), map[reflect.Type]bool{}); w != "" {
			t.Errorf("%s reaches %s, whose contents this count cannot walk: the message below would be false, so decide how that field is covered before pinning a number",
				tc.name, w)
		}
		if got := transitiveFieldCount(reflect.TypeOf(tc.v), map[reflect.Type]bool{}); got != tc.total {
			t.Errorf("%s reaches %d fields transitively, this table says %d: a field added ANYWHERE under a witness type is a field the witness can stop covering, so decide whether it is framed and then update this number",
				tc.name, got, tc.total)
		}
	}

	// AND THE SET OF TYPES IS THE CENSUS'S, so a new witness-bearing type
	// cannot skip this table either.
	withWitness, _ := witnessCensusOfDir(t, ".")
	named := map[string]bool{}
	for _, s := range withWitness {
		named[s.name] = true
	}
	// The cross-check runs one way only: every WITNESS-BEARING type must be
	// pinned here. The reverse would forbid the four rows above, which are
	// pinned precisely because they are NOT witness-bearing and so nothing
	// else watches them.
	for _, tc := range []string{
		"P2CaseResult", "VerifiedP3bRuleset", "P3bCaseResult", "PayoutEvidence",
		"FactualPlacement", "PolicyDecision", "PlacementEvidence",
	} {
		delete(named, tc)
	}
	for left := range named {
		if left == "anonymous struct" {
			continue
		}
		t.Errorf("%s carries a witness and is not pinned here: a witness type that can grow a field quietly is a field the witness can stop covering quietly", left)
	}
}

// transitiveFieldCount counts every struct field reachable from t, through
// pointers, slices and arrays, counting each struct type once per walk so the
// number is stable under a type appearing twice.
func transitiveFieldCount(t reflect.Type, seen map[reflect.Type]bool) int {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	// A MAP IS WALKED THROUGH, both halves. Stopping at one made five fields
	// under a map value cost the total exactly one, and they were then
	// invisible for good.
	if t.Kind() == reflect.Map {
		return transitiveFieldCount(t.Key(), seen) + transitiveFieldCount(t.Elem(), seen)
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return 0
	}
	seen[t] = true
	n := t.NumField()
	for i := 0; i < t.NumField(); i++ {
		n += transitiveFieldCount(t.Field(i).Type, seen)
	}
	return n
}

// opaqueFieldUnder reports a field whose contents this count cannot see at all.
// An interface, a func or a channel has no field list to walk, so a witness
// type reaching one would make the ratchet's message false.
func opaqueFieldUnder(t reflect.Type, seen map[reflect.Type]bool) string {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Interface, reflect.Func, reflect.Chan:
		return t.Kind().String()
	case reflect.Map:
		if w := opaqueFieldUnder(t.Key(), seen); w != "" {
			return w
		}
		return opaqueFieldUnder(t.Elem(), seen)
	case reflect.Struct:
		if seen[t] {
			return ""
		}
		seen[t] = true
		for i := 0; i < t.NumField(); i++ {
			if w := opaqueFieldUnder(t.Field(i).Type, seen); w != "" {
				return t.Field(i).Name + " (" + w + ")"
			}
		}
	}
	return ""
}

// constantPartsByDesign names the parts that carry one value in every round
// BECAUSE THE FIXTURE MEANS THEM TO. Each is a field the row fixes on purpose;
// anything else constant is a draw that stopped drawing.
var constantPartsByDesign = map[string]bool{
	// The "optionals absent" FactualPlacement row exists to frame the absent
	// arm, so every presence flag in it is false by design. Its sibling row
	// carries the same parts drawn, and both run in every round.
	"FactualPlacement, optionals absent/9":  true,
	"FactualPlacement, optionals absent/11": true,
	"FactualPlacement, optionals absent/12": true,
	"FactualPlacement, optionals absent/14": true,
	"FactualPlacement, optionals absent/15": true,
	"FactualPlacement, optionals absent/18": true,
	"FactualPlacement, optionals absent/19": true,
	"FactualPlacement, optionals absent/20": true,
	"FactualPlacement, optionals absent/21": true,
	// And the sibling row carries the same flags TRUE, for the same reason.
	// The pair is what covers both polarities: a framer writing a constant
	// true fails the absent row and a constant false fails this one, so
	// neither is the equivalent-mutant hazard this check exists for.
	"FactualPlacement/9":  true,
	"FactualPlacement/12": true,
	"FactualPlacement/14": true,
	"FactualPlacement/21": true,
	"FactualPlacement/23": true,
}

// TestTheWidthAndPartsOraclesCannotBeSpelledFromTheFramer holds the two
// oracles to their own shape, in the same idiom witnessComparedIn uses on
// production.
//
// BOTH CAN BE NEUTERED FROM THEIR CALL SITE WITHOUT TOUCHING THEIR BODY, which
// is the failure this package keeps meeting: the gate is locked and the key is
// left beside it. A lane passed checkWidth an inline closure returning
// len(bytes) instead of the production ...FramedLen helper -- the assertion
// becomes len == len, a tautology, and an off-by-one in the real helper then
// survives. And a lane replaced one parts row's value list with
// splitFramedParts over the framing it is meant to hold -- the values then
// agree by construction, and a swapped pair and a borrowed domain tag both
// survive, because only a change in the part COUNT still disagrees.
//
// Neither is visible to any value assertion, so both are checked structurally.
func TestTheWidthAndPartsOraclesCannotBeSpelledFromTheFramer(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		// INTERNAL TEST FILES ONLY, and EVERY one of them -- a second file in
		// this package would host a namesake or a helper with nothing looking
		// at it, which is how both defeats below were built.
		if f.Name.Name == "p4offline" {
			files = append(files, f)
		}
	}

	widths := 0
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			// A ...FramedLen NAMESAKE DECLARED HERE defeats the suffix rule
			// below: `bytesFramedLen(func(*canonical))` returning len(bytes)
			// satisfies it and makes the assertion len == len, after which an
			// off-by-one in the production helper survives. The seven real
			// helpers are declared in production, never here.
			var declared []*ast.Ident
			switch x := n.(type) {
			case *ast.FuncDecl:
				declared = append(declared, x.Name)
			case *ast.ValueSpec:
				declared = append(declared, x.Names...)
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						declared = append(declared, id)
					}
				}
			}
			for _, id := range declared {
				if strings.HasSuffix(id.Name, "FramedLen") {
					t.Errorf("%s at %s is declared in a test file: the width oracle resolves its argument by NAME, so a namesake makes it a tautology",
						id.Name, fset.Position(id.Pos()))
				}
			}

			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != "checkWidth" || len(call.Args) < 2 {
				return true
			}
			widths++
			inner, ok := call.Args[1].(*ast.CallExpr)
			if !ok {
				t.Errorf("checkWidth at %s: the width argument is not a call at all, so nothing says it came from the production width helper",
					fset.Position(call.Pos()))
				return true
			}
			fn, ok := inner.Fun.(*ast.Ident)
			if !ok || !strings.HasSuffix(fn.Name, "FramedLen") {
				t.Errorf("checkWidth at %s: the width argument is not a ...FramedLen helper, so the assertion can be a tautology over the framing's own bytes",
					fset.Position(call.Pos()))
			}
			return true
		})
	}
	if widths != 7 {
		t.Errorf("%d checkWidth calls, want 7 -- one per witness framing: a drive that left is a width nothing holds", widths)
	}

	// AN ALLOW-LIST, NOT A DENY-LIST. A three-name deny-list is defeated by a
	// helper named none of the three: a lane declared a closure above the table
	// that framed the value and split it back into parts, and three real faults
	// in framePlacementEvidence -- a swapped pair, a borrowed domain tag and a
	// dropped field -- then left the whole suite green. The part COUNT is
	// checked too, because the same closure can spell it.
	allowed := map[string]bool{
		"join": true, "actionOf": true, "choiceOf": true, "factOf": true,
		"attemptOf": true, "optI": true, "optI64": true, "i": true, "u": true,
		"bl": true, "cnt": true, "len": true, "string": true, "int64": true,
		"uint64": true,
	}
	rows := 0
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "framingPartsRound" || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				rng, ok := n.(*ast.RangeStmt)
				if !ok {
					return true
				}
				table, ok := rng.X.(*ast.CompositeLit)
				if !ok {
					return true
				}
				for _, elt := range table.Elts {
					row, ok := elt.(*ast.CompositeLit)
					if !ok || len(row.Elts) < 4 {
						continue
					}
					rows++
					for _, idx := range []int{2, 3} {
						ast.Inspect(row.Elts[idx], func(n ast.Node) bool {
							call, ok := n.(*ast.CallExpr)
							if !ok {
								return true
							}
							name := "a method or conversion"
							if id, ok := call.Fun.(*ast.Ident); ok {
								name = id.Name
								if allowed[name] {
									return true
								}
							}
							t.Errorf("parts row %d at %s calls %q: its count and value list may only be built from the declared field helpers, never from anything that can reach the framing it is meant to hold",
								rows, fset.Position(row.Pos()), name)
							return true
						})
					}
				}
				return false
			})
		}
	}
	if rows != 8 {
		t.Errorf("%d parts rows, want 8: a row that left is a framing whose values nothing checks", rows)
	}
}

// heldSlotFloor is how many parts the distinctness ratchet judges: the slots
// occupied in EVERY round, minus the ones constant by design. IT IS A FLOOR AND
// NOT A TARGET -- raise it when a row gains a fixed-position field, and treat a
// fall as the finding it is.
const heldSlotFloor = 165

// TestEverySharedFramingHelperIsSensitiveToEveryFieldItFrames holds the fields
// the parts table cannot hold at a fixed slot.
//
// A SLOT IS NOT A FIELD PAST A VARIABLE-LENGTH RUN. Three of the framings reach
// through shared helpers that sit after one, so frameAction, frameChoice and
// frameFact as PolicyDecision sees them -- and PlacementEvidence's three
// attribution fields and PayoutEvidence's three tail fields -- land at a
// different slot in every round and cannot be ratcheted by position. A lane
// pinned choice.OutcomeID in the fixture, wrote that same literal into
// frameChoice, and the whole suite stayed green.
//
// This asks the question directly instead: two values differing in ONE field
// must frame to different bytes. It needs no fixture, no slot and no round.
func TestEverySharedFramingHelperIsSensitiveToEveryFieldItFrames(t *testing.T) {
	bytesOf := func(f func(*canonical)) string {
		var c canonical
		f(&c)
		return string(c.bytes())
	}
	action := func(m ActionMapping) func(*canonical) {
		return func(c *canonical) { frameAction(c, m) }
	}
	choice := func(ch PolicyChoice) func(*canonical) {
		return func(c *canonical) { frameChoice(c, ch) }
	}
	f64 := func(f Int64Fact) func(*canonical) {
		return func(c *canonical) { frameFact(c, f) }
	}
	pl := func(p PlacementEvidence) func(*canonical) {
		return func(c *canonical) { framePlacementEvidence(c, p) }
	}
	po := func(p PayoutEvidence) func(*canonical) {
		return func(c *canonical) { framePayoutEvidence(c, p) }
	}
	for _, tc := range []struct {
		field string
		a, b  func(*canonical)
	}{
		{"ActionMapping.MapVersion", action(ActionMapping{MapVersion: "a"}), action(ActionMapping{MapVersion: "b"})},
		{"ActionMapping.Policy", action(ActionMapping{Policy: "a"}), action(ActionMapping{Policy: "b"})},
		{"ActionMapping.NativeAction", action(ActionMapping{NativeAction: "a"}), action(ActionMapping{NativeAction: "b"})},
		{"ActionMapping.Class", action(ActionMapping{Class: "a"}), action(ActionMapping{Class: "b"})},
		{"ActionMapping.Legal", action(ActionMapping{Legal: false}), action(ActionMapping{Legal: true})},
		{"ActionMapping.Illegality length", action(ActionMapping{Illegality: []string{"x"}}), action(ActionMapping{Illegality: []string{"x", "y"}})},
		{"ActionMapping.Illegality content", action(ActionMapping{Illegality: []string{"x"}}), action(ActionMapping{Illegality: []string{"y"}})},
		{"ActionMapping.SkipReason", action(ActionMapping{SkipReason: "a"}), action(ActionMapping{SkipReason: "b"})},

		{"PolicyChoice.Present", choice(PolicyChoice{Present: false}), choice(PolicyChoice{Present: true})},
		{"PolicyChoice.Index", choice(PolicyChoice{Index: 1}), choice(PolicyChoice{Index: 2})},
		{"PolicyChoice.OutcomeID", choice(PolicyChoice{OutcomeID: "a"}), choice(PolicyChoice{OutcomeID: "b"})},

		{"Int64Fact.Presence", f64(Int64Fact{Presence: "a"}), f64(Int64Fact{Presence: "b"})},
		{"Int64Fact.Value", f64(Int64Fact{Value: 1}), f64(Int64Fact{Value: 2})},
		{"Int64Fact.Reason", f64(Int64Fact{Reason: "a"}), f64(Int64Fact{Reason: "b"})},

		{"PlacementEvidence.AttributedOutcomeID", pl(PlacementEvidence{AttributedOutcomeID: "a"}), pl(PlacementEvidence{AttributedOutcomeID: "b"})},
		{"PlacementEvidence.AttributedCallObservationID", pl(PlacementEvidence{AttributedCallObservationID: "a"}), pl(PlacementEvidence{AttributedCallObservationID: "b"})},
		{"PlacementEvidence.AttributedCallPosition", pl(PlacementEvidence{AttributedCallPosition: 1}), pl(PlacementEvidence{AttributedCallPosition: 2})},

		{"PayoutEvidence.ResolutionFactsDigest", po(PayoutEvidence{ResolutionFactsDigest: "a"}), po(PayoutEvidence{ResolutionFactsDigest: "b"})},
		{"PayoutEvidence.Derivation", po(PayoutEvidence{Derivation: "a"}), po(PayoutEvidence{Derivation: "b"})},
		{"PayoutEvidence.decisionWitness", po(PayoutEvidence{decisionWitness: "a"}), po(PayoutEvidence{decisionWitness: "b"})},
	} {
		if bytesOf(tc.a) == bytesOf(tc.b) {
			t.Errorf("%s: two values differing only in this field frame to the SAME bytes, so the witness does not cover it",
				tc.field)
		}
	}
}
