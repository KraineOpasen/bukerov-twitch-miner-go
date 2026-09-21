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
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unsafe"

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
// THE FOUR CONTROLS matter as much as the comparison -- the two reduction
// counters are asserted APART, because one combined counter once stayed above
// zero while half the reduction was reverted. Without them an oracle
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
		if fault := framingWidthFault(name, width, len(b.bytes())); fault != "" {
			t.Fatalf("round %d: %s", round, fault)
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
		// THE WHOLE VOCABULARY, not two of its four members. UNKNOWN is a live
		// production value -- derivePayout's refusal arm sets Stake, Payout and
		// Net to it -- and a fixture pinned to KNOWN framed one string length
		// every round. But NOT_REACHED and NOT_APPLICABLE are live too (p2.go
		// sets NOT_REACHED on a legacy failure, and NotApplicableInt64 is used
		// at the coverage-only arm and in derivePayout), and drawing only two
		// members left a framer that maps one in-vocabulary value onto another
		// invisible: a lane collapsed NOT_REACHED to NOT_APPLICABLE inside
		// frameFact and the WHOLE SUITE stayed green, with the two facts
		// framing to the same 40 bytes AND the same width, so the width
		// pre-check could not see it either. frameFact is reached by SIX
		// framings, so that is one edit against six witnesses.
		p := []Presence{PresenceKnown, PresenceUnknown, PresenceNotReached, PresenceNotApplicable}[rng.Intn(4)]
		// AND THE SIGN IS DRAWN. Every value here was non-negative, so no
		// framing in this table ever rendered a negative int64 -- and
		// derivePayout sets Net to -Stake.Value on EVERY LOSE, so the byte
		// rendering of the commonest payout in the package was pinned by
		// nothing: c.i64 could be swapped for c.u64(uint64(...)) at five sites
		// with the whole suite green.
		v := int64(rng.Intn(1 << 30))
		if rng.Intn(2) == 0 {
			v = -v
		}
		return Int64Fact{Presence: p, Value: v, Reason: text()}
	}
	// FIVE OPTIONALS, FIVE BACKINGS -- here as well as in the parts fixture.
	// This one gave &oi to three of them and &of to two, so it agreed with the
	// parts fixture's own defect rather than covering it. A swap between two
	// same-typed optionals does not change the WIDTH, so this fixture could not
	// catch one either way; distinct backings are kept so that neither fixture
	// misleads a reader into thinking it does.
	oi, of := rng.Intn(1<<20), int64(rng.Intn(1<<30))
	oi2, of2 := rng.Intn(1<<20)+1, int64(rng.Intn(1<<30))+1
	oi3 := rng.Intn(1<<20) + 2

	fp := FactualPlacement{
		Attempt: attempt, FactsetDigest: text(), EventID: text(),
		CutoffPosition: int64(rng.Intn(1 << 30)), TerminalDecision: text(),
		RecordedChoiceIndex: &oi, RecordedChoiceOutcomeID: text(),
		RecordedFinalAmount: &of, RecordedTerminalSlot: &oi2,
		Coherence: text(), StartedOnly: rng.Intn(2) == 0, CallPresent: rng.Intn(2) == 0,
		CallStartedObservationID: text(), CallStartedPosition: int64(rng.Intn(1 << 30)),
		Stake: &of2, Slot: &oi3, Returned: rng.Intn(2) == 0,
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
	// defeat respelled with the suffix the recognizer looks for. Reading the
	// input is not enough either -- `_ = v.A; return ""` reads it and returns
	// a constant -- so it must REACH THE FRAMING too. Every recomputation in
	// this package builds a canonical and digests it.
	//
	// BOTH CLAUSES, AND-ED. Replacing the first with the second let
	// `return frameSynthetic("")` back in: it frames, and reads nothing.
	// A HELPER THAT FRAMES IS A FRAMING WHATEVER IT IS CALLED. The rule below
	// keyed on the callee's NAME, so the honest refactor
	// `return digestOfPlacement(p)` -- which frames exactly as before, one call
	// deeper -- read as "recomputes nothing" and reported PlacementEvidence as
	// a type whose witness nothing compares. A guard that fails CORRECT code
	// costs more than one that misses a defeat: it forbids a refactor that
	// changes nothing, with no adversary present. The question is whether the
	// callee REACHES a framing, and that is answered by walking its body.
	framesInside := map[string]bool{}
	for pass := 0; pass < 3; pass++ {
		for _, f := range files {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.Ident:
						if x.Name == "canonical" || strings.HasPrefix(x.Name, "frame") {
							framesInside[fd.Name.Name] = true
						}
					case *ast.CallExpr:
						if id, ok := x.Fun.(*ast.Ident); ok && framesInside[id.Name] {
							framesInside[fd.Name.Name] = true
						}
					}
					return true
				})
			}
		}
	}
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
			// THE FRAMING MUST BE REACHED *FROM* THE INPUT, not merely beside
			// it. Two independent mentions are satisfied by
			// `_ = v.A; return frameSynthetic("")`, which is the previous
			// defeat plus one line.
			// AND ITS VALUE MUST REACH THE RETURN. Handing the input to the
			// framing and throwing the result away -- `_ = frameSynthetic(v.A);
			// return ""` -- satisfies "framed" and "read" and recomputes
			// nothing; it is the previous defeat with the discard moved.
			carriers := map[string]bool{}
			handed := func(call *ast.CallExpr) bool {
				fn, ok := call.Fun.(*ast.Ident)
				if !ok || (!strings.HasPrefix(fn.Name, "frame") && fn.Name != "canonical" &&
					!framesInside[fn.Name]) {
					return false
				}
				hit := false
				for _, arg := range call.Args {
					ast.Inspect(arg, func(n ast.Node) bool {
						if id, ok := n.(*ast.Ident); ok && (given[id.Name] || carriers[id.Name]) {
							hit = true
						}
						return true
					})
				}
				return hit
			}
			// A FRAMING WRITES THROUGH A POINTER. Every recomputation here is
			// `var c canonical; frameX(&c, v); return c.digest()` -- the call's
			// own value is nothing, and what carries to the return is the
			// buffer it was handed. So an &ident passed to a framing that was
			// also handed the input makes that ident a carrier.
			for pass := 0; pass < 3; pass++ {
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok && handed(c) {
						for _, a := range c.Args {
							u, ok := a.(*ast.UnaryExpr)
							if !ok || u.Op != token.AND {
								continue
							}
							if id, ok := u.X.(*ast.Ident); ok {
								carriers[id.Name] = true
							}
						}
					}
					as, ok := n.(*ast.AssignStmt)
					if !ok || len(as.Rhs) != 1 {
						return true
					}
					reaches := false
					ast.Inspect(as.Rhs[0], func(n ast.Node) bool {
						if c, ok := n.(*ast.CallExpr); ok && handed(c) {
							reaches = true
						}
						if id, ok := n.(*ast.Ident); ok && carriers[id.Name] {
							reaches = true
						}
						return true
					})
					if !reaches {
						return true
					}
					for _, lhs := range as.Lhs {
						if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" {
							carriers[id.Name] = true
						}
					}
					return true
				})
			}
			used := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				ret, ok := n.(*ast.ReturnStmt)
				if !ok {
					return true
				}
				for _, r := range ret.Results {
					ast.Inspect(r, func(n ast.Node) bool {
						// A VALUE-DESTROYING OPERATION OVER THE CARRIER IS NOT
						// THE CARRIER. `return w[:0]` spells the carrier's name
						// and returns a constant, and the walk counted the
						// mention. Slices and indexes are not descended into;
						// `c.digest()`, which is how every honest recomputation
						// here reaches its return, is a call on the carrier and
						// is unaffected. This is narrow by construction -- it
						// closes the two shapes that destroy a value while
						// naming it, not every such shape -- and the behavioural
						// oracles are what actually stop a witness that returns
						// a constant: result_witness_test.go edits eleven fields
						// of a genuine result and requires ErrResultNotDerived,
						// and the placement, payout and factset suites carry the
						// same shape.
						// A CARRIER SLICED TO NOTHING IS NOT THE CARRIER --
						// but a carrier sliced to SOMETHING still is. The
						// first draft skipped EVERY slice and index in a
						// return, which refused `return digestOfSynthetic(v.A)[:32]`:
						// a genuine recomputation over the input, truncated,
						// and correct code. That is the same mistake as the
						// width pin's, made the same week: a rule written over
						// the shape in front of it (`w[:0]`) refusing its
						// honest neighbour. What makes `w[:0]` a laundering
						// route is that it is CONSTANT, not that it is a slice.
						if sl, ok := n.(*ast.SliceExpr); ok {
							id, isIdent := sl.X.(*ast.Ident)
							if isIdent && carriers[id.Name] && constantEmptySlice(sl) {
								return false
							}
						}
						if c, ok := n.(*ast.CallExpr); ok && handed(c) {
							used = true
						}
						if id, ok := n.(*ast.Ident); ok && carriers[id.Name] {
							used = true
						}
						return true
					})
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

type comparesAgainstAFramedConstantWitness struct {
	A         string
	witness   string
	framedLen int
}

// It FRAMES and reads nothing: the shape a rule that only asks "does it reach
// the framing" lets through.
func framedConstantWitness(v comparesAgainstAFramedConstantWitness) string {
	return frameSynthetic("")
}

func (v comparesAgainstAFramedConstantWitness) derived() bool {
	return v.witness != framedConstantWitness(v) && v.framedLen > 0
}

type comparesFramesAndReadsSeparately struct {
	A         string
	witness   string
	framedLen int
}

// It FRAMES and it READS -- just not the one from the other. Two independent
// mention-predicates AND-ed accept it; requiring the framing call to be handed
// the input does not.
func framedConstantThatAlsoReadsWitness(v comparesFramesAndReadsSeparately) string {
	_ = v.A
	return frameSynthetic("")
}

func (v comparesFramesAndReadsSeparately) derived() bool {
	return v.witness != framedConstantThatAlsoReadsWitness(v) && v.framedLen > 0
}

type comparesViaASlicedCarrier struct {
	A         string
	witness   string
	framedLen int
}

// It frames the input, binds the carrier, and returns a CONSTANT spelled with
// the carrier's name. w[:0] is the empty string however w was computed, so
// every value of this type compares equal and the witness verifies nothing --
// while a walk that counts a MENTION of the carrier in the return sees a
// genuine recomputation. Any value-destroying operation over the carrier does
// the same; this row holds the slice form, which is the one a lane wrote.
func slicedCarrierWitness(v comparesViaASlicedCarrier) string {
	w := frameSynthetic(v.A)
	return w[:0]
}

func (v comparesViaASlicedCarrier) derived() bool {
	return v.witness != slicedCarrierWitness(v) && v.framedLen > 0
}

// AND THE HONEST SHAPE THE SAME REPAIR MUST NOT REFUSE. The framing is one
// call deeper, behind a helper named nothing in particular -- the refactor the
// name-keyed rule used to report as "recomputes nothing".
type comparesViaANamelessHelper struct {
	A         string
	witness   string
	framedLen int
}

func namelessHelperWitness(v comparesViaANamelessHelper) string {
	return digestOfSynthetic(v.A)
}

func digestOfSynthetic(a string) string { return frameSynthetic(a) }

func (v comparesViaANamelessHelper) derived() bool {
	return v.witness != namelessHelperWitness(v) && v.framedLen > 0
}

type comparesFramedAndDiscarded struct {
	A         string
	witness   string
	framedLen int
}

// It hands the input to the framing and throws the result away: "framed" and
// "read" both hold, and nothing recomputed reaches the return.
func framedAndDiscardedWitness(v comparesFramedAndDiscarded) string {
	_ = frameSynthetic(v.A)
	return ""
}

func (v comparesFramedAndDiscarded) derived() bool {
	return v.witness != framedAndDiscardedWitness(v) && v.framedLen > 0
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
	if len(withWitness) != 33 {
		names := make([]string, 0, len(withWitness))
		for _, s := range withWitness {
			names = append(names, s.name)
		}
		sort.Strings(names)
		t.Errorf("the walk saw %d witness-bearing structs, want 33: %v", len(withWitness), names)
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
		// AND THE HONEST INDIRECTION, on the positive side because the rule
		// that missed it was a SPELLING rule: it keyed on the callee's name,
		// so moving a framing behind a helper named anything else read as
		// "recomputes nothing" and reported a genuine production witness as
		// unverified. Without this row the repair has no receipt, and a guard
		// that fails correct code is a defect that only correct code pays.
		"comparesViaANamelessHelper",
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
		"comparesAgainstAFramedConstantWitness", "comparesFramesAndReadsSeparately",
		"comparesFramedAndDiscarded",
		// And the one a tenth lane wrote: a carrier named in the return and
		// destroyed in the same expression.
		"comparesViaASlicedCarrier",
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
func framingPartsRound(t *testing.T, rng *rand.Rand, round int) (map[string]bool, map[string][]string, [3]int) {
	t.Helper()
	// THE EMPTY STRING IS DRAWN. Every framed string here used to be at least
	// one byte, so a framer that ELIDED an empty part agreed with the fixture in
	// all 64 rounds -- and eliding is not a cosmetic change, it breaks
	// injectivity outright: with it, ("", "x") and ("x", "") both frame to the
	// single part "x", which is two distinct values sharing a witness. Two
	// mutants survived the WHOLE SUITE on that, in frameFactualPlacement's
	// ErrorClass and in frameAction's SkipReason -- and frameAction is a SHARED
	// helper, so the second reaches six framings.
	//
	// ONE DRAW IN EIGHT, not every draw, and the ratio is the point. A field
	// empty in every round would revive the mutant this fixture's "every framed
	// field gets a non-zero, distinct value" rule exists to kill: a framer
	// writing the empty literal in that field's place would agree every time.
	// Drawn sometimes, it disagrees in seven rounds out of eight.
	text := func() string {
		if rng.Intn(8) == 0 {
			return ""
		}
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
	//
	// AND IT REACHES PAST INT32. num rises by at most a megabyte per draw and is
	// reset each round, so no framed integer ever left int32's range and a
	// framer that TRUNCATED its field to int32 agreed with the table in all 64
	// rounds -- framePolicyDecision's CollectorEpoch survived the whole suite on
	// exactly that. One draw in four is lifted past 2^34 so the truncation is
	// visible. The lift cannot collide with an unlifted draw: num rises by at
	// most 2^20 per call and a round makes fewer than a hundred, so num stays
	// below 2^27 and no unlifted value can reach a lifted one.
	nzStrict := func() int64 {
		num += 7 + int64(rng.Intn(1<<20))
		v := num
		if rng.Intn(4) == 0 {
			v += 1 << 34
		}
		if rng.Intn(2) == 0 {
			return -v
		}
		return v
	}
	// AND ZERO IS DRAWN, which the "non-zero" rule above had excluded outright
	// -- and zero is the commonest integer this package actually frames. The
	// type's own contract at quality.go mandates it: every producer sets Value
	// to zero beside a non-KNOWN presence, and five sites build a KNOWN fact
	// whose Value is zero -- Payout on every LOSE, Payout and Net on every
	// REFUND, and the exact-zero POLICY_SKIP stake. With no zero in the
	// fixture, a framer that wrote a field ONLY when it was non-zero agreed in
	// all 64 rounds, and a lane exhibited two materially different
	// PayoutEvidence values sharing one digest under exactly that mutant: a
	// framing whose PART COUNT depends on a value is forgeable against any
	// variable-length neighbour, and PayoutEvidence has one.
	//
	// ONE DRAW IN EIGHT, for the reason the empty string is drawn at the same
	// rate: a field zero in EVERY round would make the mutant that writes the
	// zero literal in its place agree every time, which is the defect the
	// non-zero rule was written for. Seven rounds in eight it still disagrees.
	//
	// THE FIVE OPTIONAL BACKINGS KEEP THE STRICT DRAW. Their whole purpose is
	// that a swap between two of them is visible, and two backings that both
	// drew zero in the same round would be interchangeable in that round.
	nz := func() int64 {
		if rng.Intn(8) == 0 {
			return 0
		}
		return nzStrict()
	}
	// EVERY FRAMED FIELD GETS A NON-ZERO, DISTINCT VALUE. A field left at its
	// zero makes the mutant that replaces it with that zero EQUIVALENT for this
	// fixture, which is how a first build of this table could not see a blanked
	// bool: the value list and the framer agreed on "false" either way.
	// PRESENCE AND VALUE ARE DRAWN INDEPENDENTLY, and that is load-bearing in a
	// way worth naming. It makes the fixture emit {UNKNOWN, non-zero}, which NO
	// PRODUCER IN THIS PACKAGE CAN BUILD -- quality.go requires a zero Value
	// beside a non-KNOWN presence -- and that impossible combination is the only
	// thing that kills a framer writing Value only when the presence is KNOWN,
	// a change the type's own doc invites. So a later "make the fixture
	// production-realistic" edit that ties Value to Presence would turn that
	// kill into a silent survivor. If that edit is ever made, the conditional
	// framing needs its own row first.
	fact := func() Int64Fact {
		// ALL FOUR MEMBERS. See the sibling fixture for why two was not enough:
		// a framer collapsing one in-vocabulary Presence onto another survived
		// the whole suite, at the same width, through six framings.
		p := []Presence{PresenceKnown, PresenceUnknown, PresenceNotReached, PresenceNotApplicable}[rng.Intn(4)]
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
	//
	// AND THAT REPAIR WAS APPLIED TO THE AMOUNTS AND NOT TO THE SLOTS, which is
	// this branch's recurring failure one FIELD over rather than one file over.
	// FactualPlacement carries five optionals and this fixture gave them four
	// backings: RecordedTerminalSlot and Slot both pointed at oi2, so a framer
	// writing one where the other belongs was invisible, and a lane's mutant
	// doing exactly that survived the whole suite. Five optionals, five
	// backings; the control that swaps two DISTINCT ones still fails by name.
	oi, of := int(nzStrict()), nzStrict()
	oi2, of2 := int(nzStrict()), nzStrict()
	oi3 := int(nzStrict())

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
		Stake: &of2, Slot: &oi3, Returned: rb(), LocalReasonOK: rb(), ErrorClass: text()}
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
	return checked, byRow, [3]int{len(illeg), len(outcomes), len(reasons)}
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
	// AND THE RUNS MUST ACTUALLY REACH EMPTY. The draw's ranges start at zero
	// so the count's own framed width is exercised at one digit and at none;
	// raising the minimums keeps every part varying, raises the judged total
	// and passes the floor, so neither the floor nor the per-slot pin sees it.
	// This does.
	minRun := [3]int{1 << 30, 1 << 30, 1 << 30}
	seen := map[string][]map[string]bool{}
	occ := map[string][]int{}
	for round := 0; round < rounds; round++ {
		var byRow map[string][]string
		var runs [3]int
		checked, byRow, runs = framingPartsRound(t, rng, round)
		for i, n := range runs {
			if n < minRun[i] {
				minRun[i] = n
			}
		}
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
	// ONLY WHERE A SLOT IS OCCUPIED IN EVERY ROUND -- which is WEAKER than "is
	// the same field in every round", and the difference is written here
	// rather than left to be found. Rows carry variable-length runs, so a slot
	// reached in ONE round would otherwise be reported as carrying one value
	// "in all 64 rounds": at seed 1 this test failed on a PolicyDecision slot
	// reached three times at the seed below and once at that one. Occupancy
	// removes that. It does NOT make a slot one field -- a lane measured 36 of
	// the 165 judged slots holding different fields in different rounds,
	// because a run longer than its minimum shifts everything after it.
	// Those 36, and everything past a row's first variable-length run, are
	// held by TestEverySharedFramingHelperIsSensitiveToEveryFieldItFrames,
	// which does not depend on a slot at all.
	//
	// AND THE FLOOR IS ONE-SIDED. It fires when the judged region SHRINKS,
	// which is where a neutered fixture moves it; a fixture that RAISES the
	// run minimums raises the count and passes it. The per-slot pin catches
	// such a raise only where it stops the parts VARYING -- a raise that keeps
	// them varying passes both. What catches that is the empty-run assertion
	// above, and neither of these two numbers.
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
	for i, n := range minRun {
		if n != 0 {
			t.Errorf("run %d is never empty over the %d rounds (shortest %d): the draw reaches zero so the count's own width is framed at no digits as well as one, and a raised minimum removes that silently",
				i, rounds, n)
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
	case reflect.Interface, reflect.Func, reflect.Chan, reflect.UnsafePointer:
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

	// EXACTLY ONE framingWidthFault MAY EXIST -- and ZERO is the reachable half.
	// A previous wording here said "two leave it resolving the wrong one"; two
	// plain functions of one name in one package DO NOT COMPILE, so that arm
	// cannot fire in any package that builds. It is kept because the check is
	// one comparison and an unreachable arm costs nothing, but the defence it
	// provides is against zero: converting the sole FuncDecl to a var, or an
	// alias, removes it and this fires.
	widthFaultDecls := 0
	widths := 0
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			// A ...FramedLen NAMESAKE DECLARED HERE defeats the suffix rule
			// below: `bytesFramedLen(func(*canonical))` returning len(bytes)
			// satisfies it and makes the assertion len == len, after which an
			// off-by-one in the production helper survives. The seven real
			// helpers are declared in production, never here.
			var declared []*ast.Ident
			topLevelFunc := false
			switch x := n.(type) {
			case *ast.FuncDecl:
				// A METHOD IS A FuncDecl TOO, and it cannot shadow anything: a
				// bare call never resolves to one. Counting it inflated the tally
				// and failed a package that was fine, and skipping it entirely is
				// correct rather than merely convenient -- there is nothing for
				// this clause to say about a method namesake.
				if x.Recv != nil {
					return true
				}
				declared = append(declared, x.Name)
				// A PLAIN FuncDecl IS TOP LEVEL in Go -- a nested function is a
				// FuncLit, not a declaration -- so this is the genuine helper and
				// every OTHER declaring position for the same name shadows it.
				topLevelFunc = true
			case *ast.ValueSpec:
				declared = append(declared, x.Names...)
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						declared = append(declared, id)
					}
				}
			// A PARAMETER, A NAMED RESULT, A STRUCT FIELD and a range key are
			// declaring positions too, and a lane used the first of them.
			case *ast.Field:
				declared = append(declared, x.Names...)
			case *ast.RangeStmt:
				for _, e := range []ast.Expr{x.Key, x.Value} {
					if id, ok := e.(*ast.Ident); ok {
						declared = append(declared, id)
					}
				}
			}
			for _, id := range declared {
				if strings.HasSuffix(id.Name, "FramedLen") {
					t.Errorf("%s at %s is declared in a test file: the width oracle resolves its argument by NAME, so a namesake makes it a tautology",
						id.Name, fset.Position(id.Pos()))
				}
				// AND THE COMPARISON HELPER IS RESOLVED BY NAME TOO, which the
				// clause above covers for the width helpers and did not cover
				// for this one. A lane declared
				// `framingWidthFault := func(...) string { return "" }`
				// immediately above checkWidth: the pin matched the shadowing
				// identifier, never resolved it, and with that ONE LINE plus
				// p3bResultFramedLen returning c.framedLen()+1 -- the exact
				// production fault this guard exists for -- the WHOLE SUITE
				// shipped green. The sibling parts oracle defends against this
				// shape with taint tracking; the asymmetry was the finding.
				if id.Name == "framingWidthFault" {
					if topLevelFunc {
						widthFaultDecls++
						continue
					}
					t.Errorf("framingWidthFault at %s is declared somewhere other than as a plain function: "+
						"the width pin resolves it by NAME, so any such declaration -- whether it shadows "+
						"the real one or replaces it -- can make the comparison return nothing while a "+
						"width helper that disagrees with its own framing ships green",
						fset.Position(id.Pos()))
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
			// PINNED TO ITS OWN HELPER BY NAME, not to the SUFFIX. A suffix
			// rule accepts any namesake: a parameter called xFramedLen, or a
			// bytesFramedLen declared in a PRODUCTION file, both make the
			// assertion len == len and let an off-by-one in the real helper
			// survive. The pairing below is what the drive is for.
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok {
				t.Errorf("checkWidth at %s: the framing name is not a literal, so it cannot be paired with its helper",
					fset.Position(call.Pos()))
				return true
			}
			name := strings.Trim(lit.Value, `"`)
			want, known := widthHelperFor[name]
			if !known {
				t.Errorf("checkWidth at %s drives %q, which widthHelperFor does not name: a new framing needs its helper pinned here",
					fset.Position(call.Pos()), name)
				return true
			}
			inner, ok := call.Args[1].(*ast.CallExpr)
			if !ok {
				t.Errorf("checkWidth at %s: the width argument is not a call at all, so nothing says it came from the production width helper",
					fset.Position(call.Pos()))
				return true
			}
			fn, ok := inner.Fun.(*ast.Ident)
			if !ok || fn.Name != want {
				got := "a non-identifier"
				if ok {
					got = fn.Name
				}
				t.Errorf("checkWidth at %s drives %q with %s, want %s: anything else can be a tautology over the framing's own bytes",
					fset.Position(call.Pos()), name, got, want)
			}
			return true
		})
	}
	if widths != 7 {
		t.Errorf("%d checkWidth calls, want 7 -- one per witness framing: a drive that left is a width nothing holds", widths)
	}

	// AND checkWidth MUST ACTUALLY COMPARE. Pinning which helper is passed in
	// says nothing about the callee using it: a lane replaced the comparison
	// with `_ = width` and the whole suite stayed green, which made this pin
	// decorative. The comparison lives in framingWidthFault, TestTheWidth-
	// ComparisonReports drives it, and this requires checkWidth to call it.
	compares := 0
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range as.Lhs {
				if id, ok := lhs.(*ast.Ident); !ok || id.Name != "checkWidth" {
					return true
				}
			}
			ast.Inspect(as, func(n ast.Node) bool {
				// THE CONDITION IS READ, NOT JUST THE INIT -- and the call may
				// live anywhere in checkWidth. The previous shape required the
				// call to be an if-statement's INIT and never looked at
				// ifs.Cond, so `if fault := framingWidthFault(...); false {`
				// satisfied it; under that spelling a lane shipped
				// p3bResultFramedLen returning c.framedLen()+1 -- a width
				// helper that disagrees with its own framing, which is the
				// exact fault this guard names -- with the WHOLE SUITE GREEN.
				// It also REFUSED the honest two-line form (`fault := ...`
				// then `if fault != "" {`), which costs more than the defeat
				// does: a guard that fails correct code charges a toll with no
				// adversary present.
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				fn, ok := call.Fun.(*ast.Ident)
				if !ok || fn.Name != "framingWidthFault" {
					return true
				}
				compares++
				if len(call.Args) != 3 {
					t.Errorf("framingWidthFault at %s takes %d arguments, want 3",
						fset.Position(call.Pos()), len(call.Args))
					return true
				}
				// DISTINCTNESS IS NOT TEXTUAL. `(width)`, `int(width)` and
				// `width+0` all print differently and mean the same thing, so
				// the tautology the check exists to stop was one character
				// away. Parentheses, numeric conversions and a +0/-0 term are
				// stripped before the comparison; `len(b.bytes())` is NOT a
				// conversion and survives stripping, which is the case that
				// matters.
				seen := map[string]bool{}
				for _, a := range call.Args {
					var b bytes.Buffer
					if err := printer.Fprint(&b, fset, widthArgCore(a)); err != nil {
						t.Fatalf("print argument: %v", err)
					}
					if seen[b.String()] {
						t.Errorf("framingWidthFault at %s is handed %q twice: comparing a value with itself is a tautology",
							fset.Position(call.Pos()), b.String())
					}
					seen[b.String()] = true
				}
				// THE ANSWER MUST BE TESTED FOR BEING NON-EMPTY, and the branch
				// that fires on a fault must fail UNCONDITIONALLY.
				//
				// THE FIRST DRAFT DEMANDED ONE SPELLING -- a != against an
				// empty string literal -- and so refused `if len(fault) != 0`
				// and `switch { case fault != "": }`, both of which are correct
				// Go doing exactly the right thing. That is the second time
				// this guard has been rewritten to stop failing honest code,
				// and the lesson is the one this package keeps relearning: a
				// rule written over the spelling in front of it refuses the
				// neighbour. What matters is that the condition ASKS WHETHER
				// THERE IS A FAULT, however it is written.
				//
				// AND THE FAILURE MUST NOT BE NESTED. A lane put the t.Fatalf
				// under `if round < 0 {` inside the body; the walk found the
				// selector somewhere below and was satisfied, while the
				// production fault the guard exists for -- a width helper
				// disagreeing with its own framing -- shipped green.
				name := ""
				ast.Inspect(as, func(m ast.Node) bool {
					if x, ok := m.(*ast.AssignStmt); ok &&
						len(x.Lhs) == 1 && len(x.Rhs) == 1 && x.Rhs[0] == ast.Expr(call) {
						if id, ok := x.Lhs[0].(*ast.Ident); ok {
							name = id.Name
						}
					}
					return true
				})
				asksForAFault := func(cond ast.Expr) bool {
					bin, ok := cond.(*ast.BinaryExpr)
					if !ok {
						return false
					}
					isResult := func(e ast.Expr) bool {
						if id, ok := e.(*ast.Ident); ok && name != "" && id.Name == name {
							return true
						}
						return e == ast.Expr(call)
					}
					isLenOfResult := func(e ast.Expr) bool {
						c, ok := e.(*ast.CallExpr)
						if !ok || len(c.Args) != 1 {
							return false
						}
						id, ok := c.Fun.(*ast.Ident)
						return ok && id.Name == "len" && isResult(c.Args[0])
					}
					switch bin.Op {
					case token.NEQ:
						// x != "" / "" != x / len(x) != 0 / 0 != len(x)
						if isResult(bin.X) && isEmptyStringLit(bin.Y) ||
							isResult(bin.Y) && isEmptyStringLit(bin.X) {
							return true
						}
						return isLenOfResult(bin.X) && isZeroLit(bin.Y) ||
							isLenOfResult(bin.Y) && isZeroLit(bin.X)
					case token.GTR:
						return isLenOfResult(bin.X) && isZeroLit(bin.Y)
					case token.LSS:
						return isZeroLit(bin.X) && isLenOfResult(bin.Y)
					}
					return false
				}
				failsRightHere := func(body []ast.Stmt) bool {
					for _, st := range body {
						es, ok := st.(*ast.ExprStmt)
						if !ok {
							continue
						}
						c, ok := es.X.(*ast.CallExpr)
						if !ok {
							continue
						}
						if sel, ok := c.Fun.(*ast.SelectorExpr); ok &&
							(sel.Sel.Name == "Fatalf" || sel.Sel.Name == "Errorf" || sel.Sel.Name == "Fatal") {
							return true
						}
					}
					return false
				}
				tested := false
				ast.Inspect(as, func(m ast.Node) bool {
					switch x := m.(type) {
					case *ast.IfStmt:
						if x.Cond != nil && asksForAFault(x.Cond) && failsRightHere(x.Body.List) {
							tested = true
						}
					case *ast.CaseClause:
						for _, cond := range x.List {
							if asksForAFault(cond) && failsRightHere(x.Body) {
								tested = true
							}
						}
					}
					return true
				})
				if !tested {
					t.Errorf("framingWidthFault at %s: its answer is never tested for being non-empty in a "+
						"branch that fails right there, so the comparison decides nothing",
						fset.Position(call.Pos()))
				}
				return true
			})
			return true
		})
	}
	if widthFaultDecls != 1 {
		t.Errorf("framingWidthFault is declared %d times as a plain function, want 1: the width pin "+
			"resolves it by name, so zero leaves the pin resolving nothing. Two cannot compile, which "+
			"is why zero is the case this guards",
			widthFaultDecls)
	}
	if compares != 1 {
		t.Errorf("checkWidth calls framingWidthFault %d times, want 1: the width it is handed must be COMPARED, not merely passed",
			compares)
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
			// TAINT, NOT SPELLING. A name-based rule is beaten by respelling:
			// a lane hoisted the framed-and-resplit want into a plain
			// identifier, and separately redefined an ALLOW-LISTED helper to
			// frame and split. Both are assignments whose right-hand side
			// reaches the framing, so that is what is tracked -- every
			// identifier this function binds from an expression mentioning
			// canonical, frame* or splitFramedParts is tainted, and a row's
			// count or value list may not mention one.
			// EVERY FUNCTION IN THE PACKAGE THAT REACHES THE FRAMING is
			// tainted first, so a plain helper called from the table is not a
			// laundering route: a lane wrote placementFieldList(t, pl) with an
			// ordinary := and the intra-procedural walk saw only three
			// innocent names.
			// FUNCTIONS AND VALUES ARE KEPT APART. One namespace meant 55 of
			// this package's function bodies tainted their own names --
			// check, derived, framed, String -- and any local sharing one
			// would have been treated as framer-derived whatever it held.
			taintedFuncs := map[string]bool{}
			for _, ff := range files {
				for _, dd := range ff.Decls {
					fn, ok := dd.(*ast.FuncDecl)
					if !ok || fn.Body == nil {
						continue
					}
					ast.Inspect(fn.Body, func(n ast.Node) bool {
						if id, ok := n.(*ast.Ident); ok &&
							(id.Name == "canonical" || id.Name == "splitFramedParts" ||
								strings.HasPrefix(id.Name, "frame")) {
							taintedFuncs[fn.Name.Name] = true
						}
						return true
					})
				}
			}
			tainted := map[string]bool{}
			reachesFramer := func(e ast.Expr) bool {
				bad := false
				ast.Inspect(e, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.CallExpr:
						if id, ok := x.Fun.(*ast.Ident); ok && taintedFuncs[id.Name] {
							bad = true
						}
					case *ast.Ident:
						if x.Name == "canonical" || x.Name == "splitFramedParts" ||
							strings.HasPrefix(x.Name, "frame") || tainted[x.Name] {
							bad = true
						}
					}
					return true
				})
				return bad
			}
			// A TAINTED FUNCTION BOUND TO A NAME IS STILL A FRAMER, and it
			// belongs in the FUNCTION namespace and not the value one.
			// `mk := placementFieldList` binds a framer-reaching function to a
			// value name: reachesFramer's call arm consults taintedFuncs and
			// its Ident arm consults tainted, and after the namespace split
			// NEITHER held mk -- so `plWant := mk(t, pl)` laundered the framer
			// into a row, and under that laundering a lane swapped two writers
			// in framePlacementEvidence with the WHOLE SUITE GREEN. That is a
			// regression this round's own repair introduced.
			// PUTTING THE NAME IN taintedFuncs CLOSES IT WITHOUT REVIVING THE
			// OVER-APPROXIMATION the split removed: a local that merely SHARES
			// a name with a tainted function is not BOUND FROM one, so it stays
			// clean, which is the whole reason the two namespaces exist.
			// AND A NAME BOUND THAT WAY IS RECORDED SEPARATELY. taintedFuncs
			// starts out holding every function in the package whose body
			// reaches the framing, which is the right over-approximation for a
			// CALL but the wrong one for a row mention. What a row must refuse
			// is a name this function REBOUND from a framer -- and because
			// namesTaintedFunc is the first arm below, such a name landed in
			// taintedFuncs and never in tainted, so an ALLOW-LISTED spelling
			// (`factOf := factPartsFromFramer`) walked past the row check
			// entirely. With it, two swapped writers in frameFact left the
			// WHOLE SUITE green, while the same swap unlaundered is caught by
			// name. The allow-list answers "is this helper permitted"; it
			// cannot also answer "is this helper still what it was".
			taintedByBinding := map[string]bool{}
			namesTaintedFunc := func(e ast.Expr) bool {
				id, ok := e.(*ast.Ident)
				if !ok {
					return false
				}
				return taintedFuncs[id.Name] || id.Name == "canonical" ||
					id.Name == "splitFramedParts" || strings.HasPrefix(id.Name, "frame")
			}
			// AND THE LEFT-HAND SIDE IS ROOTED. `m["k"] = ...`, `box.p = ...`
			// and `*p = ...` are not Idents, so each laundered the framer into
			// a row untracked; the root of the expression is what binds.
			rootOf := func(e ast.Expr) string {
				for {
					switch x := e.(type) {
					case *ast.Ident:
						return x.Name
					case *ast.IndexExpr:
						e = x.X
					case *ast.SelectorExpr:
						e = x.X
					case *ast.StarExpr:
						e = x.X
					case *ast.ParenExpr:
						e = x.X
					default:
						return ""
					}
				}
			}
			// AND THE BINDING FORMS ARE NOT JUST `=`. A `var` is a ValueSpec,
			// a range binds without one, and `a, b := f()` has ONE right-hand
			// side for two names -- a lane used all three, the last of which
			// was an index bug in this loop.
			taintNames := func(lhs []ast.Expr, rhs []ast.Expr) {
				for i, e := range lhs {
					name := rootOf(e)
					if name == "" || name == "_" {
						continue
					}
					var src ast.Expr
					switch {
					case len(rhs) == 1:
						src = rhs[0]
					case i < len(rhs):
						src = rhs[i]
					default:
						continue
					}
					switch {
					case namesTaintedFunc(src):
						taintedFuncs[name] = true
						taintedByBinding[name] = true
					case reachesFramer(src):
						tainted[name] = true
					}
				}
			}
			// OVER EVERY FILE, NOT JUST THIS FUNCTION, and repeated to a fixed
			// point. A package-level var filled by a separate function is a
			// binding this walk must see, and taint that appears late in
			// source order must still reach a row declared earlier.
			bind := func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.AssignStmt:
					taintNames(x.Lhs, x.Rhs)
				case *ast.ValueSpec:
					vals := x.Values
					names := make([]ast.Expr, 0, len(x.Names))
					for _, nm := range x.Names {
						names = append(names, nm)
					}
					taintNames(names, vals)
				case *ast.RangeStmt:
					if x.X != nil && reachesFramer(x.X) {
						for _, e := range []ast.Expr{x.Key, x.Value} {
							if id, ok := e.(*ast.Ident); ok && id.Name != "_" {
								tainted[id.Name] = true
							}
						}
					}
				}
				return true
			}
			// LOCALS ARE THIS FUNCTION'S; PACKAGE-LEVEL VARS ARE EVERYONE'S.
			// Walking every body into one map merges locals that merely share
			// a name across functions, which fails innocent rows. So the
			// binding walk runs here, and separately every package-level var
			// name is tainted if ANY body assigns it from something reaching
			// the framing -- the route a lane used with a var filled by a
			// helper called before the table.
			globals := map[string]bool{}
			for _, ff := range files {
				for _, dd := range ff.Decls {
					gd, ok := dd.(*ast.GenDecl)
					if !ok || gd.Tok != token.VAR {
						continue
					}
					for _, sp := range gd.Specs {
						vs, ok := sp.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for _, nm := range vs.Names {
							globals[nm.Name] = true
						}
					}
				}
			}
			// THE FIXED POINT COUNTS BOTH NAMESPACES. A pass that only grew
			// taintedFuncs used to look like no progress and stopped the loop
			// one step before the value it fed.
			for pass := 0; pass < 4; pass++ {
				before := len(tainted) + len(taintedFuncs)
				ast.Inspect(fd.Body, bind)
				for _, ff := range files {
					ast.Inspect(ff, func(n ast.Node) bool {
						as, ok := n.(*ast.AssignStmt)
						if !ok {
							return true
						}
						for i, e := range as.Lhs {
							name := rootOf(e)
							if !globals[name] {
								continue
							}
							var src ast.Expr
							switch {
							case len(as.Rhs) == 1:
								src = as.Rhs[0]
							case i < len(as.Rhs):
								src = as.Rhs[i]
							default:
								continue
							}
							switch {
							case namesTaintedFunc(src):
								taintedFuncs[name] = true
								taintedByBinding[name] = true
							case reachesFramer(src):
								tainted[name] = true
							}
						}
						return true
					})
				}
				if len(tainted)+len(taintedFuncs) == before {
					break
				}
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
					// RESOLVED BY FIELD IN BOTH BRANCHES. A keyed literal moves
					// what sits at elements 2 and 3, and so does adding a
					// field to the row struct -- a lane used each in turn, the
					// second against a guard that had just fixed the first.
					checkMe := []ast.Expr{}
					keyed := false
					for _, e := range row.Elts {
						kv, ok := e.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						keyed = true
						if k, ok := kv.Key.(*ast.Ident); ok && (k.Name == "parts" || k.Name == "want") {
							checkMe = append(checkMe, kv.Value)
						}
					}
					if !keyed {
						for _, want := range []string{"parts", "want"} {
							at := fieldIndexOf(table.Type, want)
							if at < 0 || at >= len(row.Elts) {
								t.Errorf("parts row %d at %s: the row struct has no %q the guard can reach",
									rows, fset.Position(row.Pos()), want)
								continue
							}
							checkMe = append(checkMe, row.Elts[at])
						}
					}
					if len(checkMe) != 2 {
						t.Errorf("parts row %d at %s does not present both parts and want: the guard cannot find what to hold",
							rows, fset.Position(row.Pos()))
					}
					for _, expr := range checkMe {
						ast.Inspect(expr, func(n ast.Node) bool {
							if id, ok := n.(*ast.Ident); ok && (tainted[id.Name] || taintedByBinding[id.Name]) {
								t.Errorf("parts row %d at %s mentions %q, which this function binds from something that reaches the framing: the value list would agree with the framer by construction",
									rows, fset.Position(row.Pos()), id.Name)
								return true
							}
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
	dec := func(d PolicyDecision) func(*canonical) {
		return func(c *canonical) { framePolicyDecision(c, d) }
	}
	rows := 0
	table := []struct {
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

		// THE LIST CONTENTS, which no row's slot can hold: their position
		// moves with the run's length, so the parts table cannot ratchet them
		// and only this table can. A lane pinned all three in the fixture,
		// wrote the same literals into the three framers, and every one
		// survived the whole suite.
		{"PolicyDecision.OutcomeIDs content", dec(PolicyDecision{OutcomeIDs: []string{"x"}}), dec(PolicyDecision{OutcomeIDs: []string{"y"}})},
		{"PolicyDecision.OutcomeIDs length", dec(PolicyDecision{OutcomeIDs: []string{"x"}}), dec(PolicyDecision{OutcomeIDs: []string{"x", "y"}})},
		{"PlacementEvidence.Reasons content", pl(PlacementEvidence{Reasons: []string{"x"}}), pl(PlacementEvidence{Reasons: []string{"y"}})},
		{"PlacementEvidence.Reasons length", pl(PlacementEvidence{Reasons: []string{"x"}}), pl(PlacementEvidence{Reasons: []string{"x", "y"}})},
		{"PayoutEvidence.Reasons content", po(PayoutEvidence{Reasons: []string{"x"}}), po(PayoutEvidence{Reasons: []string{"y"}})},
		{"PayoutEvidence.Reasons length", po(PayoutEvidence{Reasons: []string{"x"}}), po(PayoutEvidence{Reasons: []string{"x", "y"}})},
	}
	for _, tc := range table {
		if bytesOf(tc.a) == bytesOf(tc.b) {
			t.Errorf("%s: two values differing only in this field frame to the SAME bytes, so the witness does not cover it",
				tc.field)
		}
		rows++
	}
	// AND THE ROW COUNT IS PINNED, like every other enumeration in this file.
	// This was the one without a pin, and it is the SOLE non-fixture holder of
	// the three list contents: deleting six rows let all three framings go
	// blind to their lists with the suite green.
	if rows != sensitivityRows {
		t.Errorf("the sensitivity table drives %d rows, want %d: a row that left is a field nothing else holds",
			rows, sensitivityRows)
	}
	// AND NO TWO ROWS MAY DRIVE THE SAME PAIR. The count holds arity only: a
	// row can keep its label, borrow another field's two values, still frame
	// differently and still pass -- which lets the field it NAMES go
	// uncovered while the table reports 26.
	// THE KEY IS QUOTED, because framed parts contain NUL: joining two raw byte
	// strings with a NUL made two different pairs collide into one key, which
	// is a false POSITIVE waiting to happen as well as a missed duplicate.
	//
	// AND WHAT THIS CLAUSE DOES NOT CATCH, recorded rather than left to be
	// found: it compares the two values BYTE FOR BYTE, so it sees a row that
	// reuses another row's exact pair and NOT a row relabelled onto a
	// neighbouring field with fresh literals. Closing that means building each
	// row from (base, field, valueA, valueB) and setting the field by
	// reflection so the label cannot drift from the field it names -- a rebuild
	// of all 26 rows, outside this round's repair scope. The cost today is nil
	// and that is checked, not assumed: every field in this table is also
	// value-compared by the parts table, which resolves rows by FIELD, so this
	// table is a redundancy backstop here and not a sole cover.
	pairs := map[string]string{}
	for _, tc := range table {
		key := strconv.Quote(bytesOf(tc.a)) + "\x00" + strconv.Quote(bytesOf(tc.b))
		if first, dup := pairs[key]; dup {
			t.Errorf("%s drives the same two values as %s: a borrowed pair leaves the field this row names uncovered",
				tc.field, first)
		}
		pairs[key] = tc.field
	}
}

// sensitivityRows is how many fields TestEverySharedFramingHelper... drives.
// IT IS A PIN AND NOT A TARGET: raise it with a row, and treat a fall as the
// finding it is.
const sensitivityRows = 26

// widthArgCore strips the spellings that mean the same value, so the
// distinctness check above cannot be defeated by punctuation. Parentheses, a
// numeric conversion and a +0/-0 term are removed; a call like len(b.bytes())
// is left alone, which is the argument the check actually has to see.
func widthArgCore(e ast.Expr) ast.Expr {
	numeric := map[string]bool{"int": true, "int8": true, "int16": true, "int32": true,
		"int64": true, "uint": true, "uint8": true, "uint16": true, "uint32": true,
		"uint64": true, "uintptr": true}
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
		case *ast.CallExpr:
			id, ok := x.Fun.(*ast.Ident)
			if !ok || len(x.Args) != 1 || !numeric[id.Name] {
				return e
			}
			e = x.Args[0]
		case *ast.UnaryExpr:
			// A UNARY PLUS IS NOT A BINARY EXPRESSION, so `+width` printed
			// differently and meant the same thing.
			if x.Op != token.ADD {
				return e
			}
			e = x.X
		case *ast.BinaryExpr:
			// AND THE IDENTITY IS NOT ONLY ADDITIVE. `width*1` and `width/1`
			// print differently too, and a lane used the first.
			switch x.Op {
			case token.ADD, token.SUB:
				switch {
				case isZeroLit(x.Y):
					e = x.X
				case isZeroLit(x.X) && x.Op == token.ADD:
					e = x.Y
				default:
					return e
				}
			case token.MUL, token.QUO:
				switch {
				case isOneLit(x.Y):
					e = x.X
				case isOneLit(x.X) && x.Op == token.MUL:
					e = x.Y
				default:
					return e
				}
			default:
				return e
			}
		default:
			return e
		}
	}
}

func isZeroLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "0"
}

func isOneLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "1"
}

func isEmptyStringLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
}

// framingWidthFault reports why a recorded width disagrees with the bytes its
// framing wrote, or "" when they agree.
//
// IT IS A NAMED FUNCTION SO THAT SOMETHING CAN DRIVE IT. Inside checkWidth the
// comparison was a bare if, and replacing it with `_ = width` left the whole
// suite green -- which made the helper-name pin decorative, since the pin says
// which width is PASSED IN and said nothing about it being compared.
func framingWidthFault(name string, width, framed int) string {
	if width == framed {
		return ""
	}
	return name + " width " + strconv.Itoa(width) + ", framing " + strconv.Itoa(framed) + " bytes"
}

// TestTheWidthComparisonReports drives the comparison checkWidth rests on.
func TestTheWidthComparisonReports(t *testing.T) {
	if got := framingWidthFault("X", 10, 10); got != "" {
		t.Errorf("equal widths reported %q, want silence", got)
	}
	for _, w := range []int{9, 11, 0, -1} {
		if framingWidthFault("X", w, 10) == "" {
			t.Errorf("a recorded width of %d against a framing of 10 reported nothing", w)
		}
	}
}

// TestTheTransitiveCountWalksWhatItClaimsTo drives the two helpers over
// synthetic types, because on this package's own types they are VACUOUS.
//
// NO WITNESS TYPE REACHES A MAP OR AN OPAQUE FIELD TODAY. A lane neutered both
// -- the map branch to return 0 and opaqueFieldUnder to return "" -- and the
// whole suite stayed green, which makes them exactly what this file's own
// prose keeps condemning: a machine check whose mechanism nothing asserts. The
// sibling census and the comparison recognizer are both driven over synthetic
// sources for that reason; these two were not.
func TestTheTransitiveCountWalksWhatItClaimsTo(t *testing.T) {
	type leaf struct{ A, B, C, D, E string }
	fresh := func() map[reflect.Type]bool { return map[reflect.Type]bool{} }

	for _, tc := range []struct {
		name string
		v    any
		want int
	}{
		{"a plain struct counts its own fields", struct{ A, B string }{}, 2},
		{"a map VALUE is walked", struct {
			M map[string]leaf
		}{}, 1 + 5},
		{"a map KEY is walked", struct {
			M map[leaf]string
		}{}, 1 + 5},
		{"a slice of pointers is walked", struct{ S []*leaf }{}, 1 + 5},
		{"an array is walked", struct{ A [2]leaf }{}, 1 + 5},
		{"a type reached twice counts once", struct{ X, Y leaf }{}, 2 + 5},
	} {
		if got := transitiveFieldCount(reflect.TypeOf(tc.v), fresh()); got != tc.want {
			t.Errorf("%s: counted %d, want %d", tc.name, got, tc.want)
		}
	}

	for _, tc := range []struct {
		name string
		v    any
		want bool
		kind string
	}{
		{"an interface field is opaque", struct{ I any }{}, true, "interface"},
		{"a func field is opaque", struct{ F func() }{}, true, "func"},
		{"a chan field is opaque", struct{ C chan int }{}, true, "chan"},
		{"an opaque field under a slice is reported", struct{ S []struct{ I any } }{}, true, "interface"},
		{"an opaque field under a map value is reported", struct {
			M map[string]struct{ F func() }
		}{}, true, "func"},
		{"an unsafe.Pointer field is opaque", struct{ P unsafe.Pointer }{}, true, "unsafe.Pointer"},
		{"an opaque field under a pointer is reported", struct{ P *struct{ I any } }{}, true, "interface"},
		{"an opaque field under an array is reported", struct{ A [2]struct{ F func() } }{}, true, "func"},
		{"an opaque map KEY is reported", struct {
			M map[any]string
		}{}, true, "interface"},
		{"a plain struct is not opaque", struct{ A string }{}, false, ""},
		{"a struct of structs is not opaque", struct{ L leaf }{}, false, ""},
	} {
		msg := opaqueFieldUnder(reflect.TypeOf(tc.v), fresh())
		if got := msg != ""; got != tc.want {
			t.Errorf("%s: opaque=%v, want %v", tc.name, got, tc.want)
		}
		// AND THE MESSAGE MUST NAME THE KIND, because it is what the census
		// prints when a witness type grows a field this count cannot walk.
		if tc.want && !strings.Contains(msg, tc.kind) {
			t.Errorf("%s: reported %q, which does not name %q", tc.name, msg, tc.kind)
		}
	}
}

// widthHelperFor pairs each framing with THE production helper whose width
// checkEveryFramingWidth must drive. A suffix rule accepts a namesake; this
// does not.
var widthHelperFor = map[string]string{
	"P2CaseResult":       "p2ResultFramedLen",
	"P3bCaseResult":      "p3bResultFramedLen",
	"FactualPlacement":   "factualPlacementFramedLen",
	"PolicyDecision":     "policyDecisionFramedLen",
	"PlacementEvidence":  "placementEvidenceFramedLen",
	"PayoutEvidence":     "payoutEvidenceFramedLen",
	"VerifiedP3bRuleset": "rulesetFramedLen",
}

// fieldIndexOf reports the position of a named field in a composite literal's
// struct type, so an unkeyed row is read BY FIELD and not by a number that
// moves the moment the struct gains one.
func fieldIndexOf(typ ast.Expr, name string) int {
	at, ok := typ.(*ast.ArrayType)
	if !ok {
		return -1
	}
	st, ok := at.Elt.(*ast.StructType)
	if !ok || st.Fields == nil {
		return -1
	}
	i := 0
	for _, f := range st.Fields.List {
		// AN EMBEDDED FIELD HAS NO NAME AND STILL COSTS THE LITERAL AN
		// ELEMENT. Skipping it without advancing shifted every index after it
		// by one, which is how a row with one embedded field put its value
		// list where this walk never looked.
		if len(f.Names) == 0 {
			i++
			continue
		}
		for _, n := range f.Names {
			if n.Name == name {
				return i
			}
			i++
		}
	}
	return -1
}

// A REGISTER OF FIGURES WRITTEN IN PROSE STOPS BEING TRUE ONE COMMIT LATER.
// doc.go states a rule about this package's measured figures -- a single
// runtime.MemStats.TotalAlloc delta is always a multiple of eight, so a figure
// that is not one is either an average whose basis was recorded or a number
// nothing can classify -- and then ENUMERATES the figures in that state. The
// enumeration was written over the figures in doc.go and read as a statement
// about the package: eight allocation figures in canonical.go, evidence.go,
// factset.go, p3b.go and resolution.go were in exactly that state and named
// nowhere, while a sentence added to p3b.go pointed a reader at the register
// for "the other figures in that state". That is this branch's recurring
// failure in its purest form -- an enumeration that was true where it was
// written and false one file over -- and the package's own answer to it is
// stated in canonical.go: THE INVENTORY IS DERIVED, NOT WRITTEN.
//
// This is that derivation. It scans every comma-grouped integer in every
// production comment, keeps the ones that are NOT multiples of eight -- the
// set the rule is about -- and requires each to be CLASSIFIED below. A figure
// added to a production comment in that form fails this test until someone
// says what it is.
//
// THE CLASSIFICATION IS THE BARRIER, not the inventory. A figure classified
// figAllocBytes is subject to the rule, so it must be quoted in doc.go, where
// the rule is stated and each figure's admissibility is argued. That clause is
// what the eight failed, and it is what keeps the register from falling behind
// the tree again.
//
// WHAT THIS DOES NOT DO, written here because the last four rounds were lost
// to guards that promised more than they checked:
//
//   - It does not check that a classification is CORRECT. Calling an
//     allocation figure a count moves it out of the register's reach, and only
//     a reader of the surrounding sentence can catch that. The kinds are named
//     rather than numbered so that a wrong one is at least legible.
//   - It does not reach a figure written without commas, as a range, in words,
//     or in a test file. It is an inventory over ONE written form, and about
//     thirty decimal-magnitude figures in production comments -- "1.67 GB",
//     "136.34 MB", "about 1.06 MB" -- are outside it, INCLUDING the retracted
//     sixteenth figure this register itself discusses. A bare 16777374 would
//     escape too. The trailing word-boundary escape is closed; these are not.
//   - It does not hold a figure that IS a multiple of eight, so 203,719,320 --
//     which the register quotes as the admissible member of its own series --
//     has no file set held here and could go stale at its measurement site.
//   - It does not check that a quoted figure is a correct MEASUREMENT. Nothing
//     here re-measures anything; the rule is about admissibility, not accuracy.
type figKind string

const (
	// figAllocBytes is a measured allocation in BYTES. The multiple-of-eight
	// rule reaches exactly these, so each one must be quoted in doc.go.
	figAllocBytes figKind = "allocation bytes"
	// figAllocCount is a measured allocation COUNT. A raw TotalAlloc delta
	// does not produce one, so the rule says nothing about it.
	figAllocCount figKind = "allocation count"
	// figInputSize is a size the CALLER chose: a document, an identifier, a
	// field, a declared ceiling.
	figInputSize figKind = "input size"
	// figOutputSize is a size this package EMITTED: an error, a refusal, a
	// serialization.
	figOutputSize figKind = "output size"
	// figPlainCount is anything else counted: entries, records, reasons,
	// rounds, hits, misses, posting lists.
	figPlainCount figKind = "count"
	// figRatio is a multiple, not a quantity -- "62,204x the flat gate". The
	// multiple-of-eight rule is about a byte count and does not reach one.
	figRatio figKind = "ratio"
)

// figEntry is what this inventory records about one figure.
type figEntry struct {
	kind figKind
	// basis is true where the figure RECORDS how it was averaged. The rule the
	// register states has two halves -- a non-multiple of eight is not a single
	// delta, and one whose averaging basis was never written down cannot be told
	// from a slip -- and only the second half decides whether a figure is left
	// standing as unexplained. doc.go says FIFTEEN are in that state; that
	// number lived in prose alone, so a sixteenth would have passed this test
	// while the sentence went stale. The split is held below.
	basis bool
	// files is every production file whose comments quote the figure, sorted
	// and comma-joined. THE FILE SET IS HELD, NOT JUST THE FIGURE, and that is
	// the clause the first draft of this test was missing. Once a figure is
	// quoted in doc.go's register, editing it at its MEASUREMENT site leaves it
	// still quoted -- in the register alone, now describing nothing -- and a
	// figure-set check passes. That is this branch's own recurring failure
	// exactly: a correction applied in one file and not its sibling one file
	// over. Holding the file set fails it.
	files string
}

// figureInventory classifies every comma-grouped integer in a production
// comment that is not a multiple of eight, and records where it is quoted. The
// figures AND their homes are held exactly: an addition, a removal and a move
// all fail.
var figureInventory = map[int]figEntry{
	1_001:       {figPlainCount, false, "evidence.go"},       // entries counted against a ceiling
	1_030:       {figOutputSize, false, "doc.go,factset.go"}, // a serialized factset carrying a +Inf
	1_900:       {figAllocCount, false, "evidence.go"},
	4_097:       {figPlainCount, false, "resolution.go"}, // reasons
	4_782:       {figPlainCount, false, "doc.go"},        // observation-id posting lists
	11_930:      {figAllocCount, false, "evidence.go"},
	12_298:      {figPlainCount, false, "doc.go"},      // answers with content
	17_883:      {figPlainCount, false, "evidence.go"}, // accessor misses
	65_671:      {figInputSize, false, "p3b.go"},       // a ruleset document
	65_675:      {figOutputSize, false, "p3b.go"},      // the refusal it produced
	95_939:      {figAllocCount, false, "evidence.go"},
	62_204:      {figRatio, false, "doc.go,resolution.go"}, // an amplification multiple
	147_671:     {figAllocBytes, false, "doc.go"},
	111_087:     {figRatio, false, "doc.go,factset.go"},              // an amplification multiple
	200_001:     {figPlainCount, false, "evidence.go"},               // records
	297_913:     {figInputSize, false, "resolution.go"},              // reasons, in bytes
	840_098:     {figOutputSize, false, "evidence.go,resolution.go"}, // a refusal naming a foreign-session dataset
	999_975:     {figAllocCount, false, "factset.go"},
	1_055_076:   {figAllocBytes, true, "doc.go"},
	1_057_073:   {figAllocBytes, false, "doc.go"},
	1_999_875:   {figAllocCount, false, "factset.go"},
	2_113_586:   {figAllocBytes, true, "doc.go"},
	2_113_748:   {figAllocBytes, false, "doc.go,p3b.go"},
	2_113_763:   {figAllocBytes, false, "doc.go"},
	3_183_766:   {figAllocBytes, true, "doc.go"},
	4_240_782:   {figAllocBytes, true, "doc.go"},
	5_588_220:   {figPlainCount, false, "evidence.go"}, // guard hits over the whole suite
	6_889_790:   {figAllocBytes, false, "doc.go"},
	9_120_039:   {figInputSize, false, "p3b.go"}, // a document that overflows the stack
	10_485_107:  {figAllocBytes, false, "doc.go,p3b.go"},
	13_436_051:  {figAllocBytes, false, "canonical.go,doc.go,resolution.go"},
	16_777_374:  {figOutputSize, false, "canonical.go,p3b.go"}, // an error naming a 16 MiB key
	16_885_191:  {figAllocBytes, false, "canonical.go,doc.go,factset.go"},
	18_301_246:  {figAllocBytes, false, "doc.go"},
	18_301_274:  {figAllocBytes, false, "doc.go"},
	24_641_243:  {figAllocBytes, false, "doc.go,evidence.go"},
	41_942_377:  {figAllocBytes, false, "doc.go,p3b.go"},
	50_923_867:  {figAllocBytes, false, "doc.go,evidence.go"},
	101_334_414: {figAllocBytes, false, "doc.go,evidence.go"},
	167_771_494: {figAllocBytes, false, "canonical.go,doc.go,p3b.go"},
	805_306_353: {figInputSize, false, "p3b.go"}, // a legal document written entirely as escapes
}

// commaGroupedFigure matches the ONE written form this inventory covers.
//
// THERE ARE NO WORD BOUNDARIES, and a draft that had them is why this comment
// exists. A trailing `\b` drops any figure ABUTTING a word character, so
// "62,204x" in resolution.go and "111,087x" in factset.go were invisible to a
// scan whose own sentence said it read every one; both happen to be ratios, so
// nothing was hidden -- but "16,885,191B/op" would have been, and that is an
// allocation figure the rule reaches. A leading `\b` has the mirror hole.
// What a boundary was there to do -- stop the TAIL of a longer number matching
// -- is done exactly by figureStartsHere instead, which RE2 cannot express as a
// lookbehind.
var commaGroupedFigure = regexp.MustCompile(`[0-9]{1,3}(?:,[0-9]{3})+`)

// figureStartsHere reports whether a match begins a figure rather than
// continuing one: the byte before it must not be a digit or a comma.
func figureStartsHere(text string, at int) bool {
	if at == 0 {
		return true
	}
	c := text[at-1]
	return c != ',' && (c < '0' || c > '9')
}

func TestTheFigureRegisterNamesEveryFigureItMustName(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	// site -> where each figure was found, so a failure names the line rather
	// than the number alone.
	sites := map[int][]string{}
	inDoc := map[int]bool{}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		for _, g := range f.Comments {
			for _, cm := range g.List {
				for _, loc := range commaGroupedFigure.FindAllStringIndex(cm.Text, -1) {
					if !figureStartsHere(cm.Text, loc[0]) {
						continue
					}
					n, err := strconv.Atoi(strings.ReplaceAll(cm.Text[loc[0]:loc[1]], ",", ""))
					if err != nil || n%8 == 0 {
						continue
					}
					line := fset.Position(cm.Pos()).Line
					sites[n] = append(sites[n], fmt.Sprintf("%s:%d", name, line))
					if name == "doc.go" {
						inDoc[n] = true
					}
				}
			}
		}
	}
	// A SCAN THAT READ NOTHING WOULD PASS EVERY CLAUSE BELOW, so the reach is
	// asserted before the answers are.
	if scanned < 10 {
		t.Fatalf("scanned only %d production files; the walk is not reaching the package", scanned)
	}
	if len(sites) == 0 {
		t.Fatal("no comma-grouped figure found in any production comment; the scan is vacuous")
	}

	for n, where := range sites {
		if _, ok := figureInventory[n]; !ok {
			t.Errorf("%s: %s is not a multiple of eight and is not classified in figureInventory; "+
				"say what it is before quoting it", strings.Join(where, ", "), withCommas(n))
		}
	}
	for n, want := range figureInventory {
		where, ok := sites[n]
		if !ok {
			t.Errorf("figureInventory classifies %s, which no production comment quotes any more; "+
				"remove the row", withCommas(n))
			continue
		}
		if got := joinFiles(where); got != want.files {
			t.Errorf("%s is quoted in %s; figureInventory records %s. A figure edited at one of its "+
				"sites and left standing at another is the defect this clause exists for",
				withCommas(n), got, want.files)
		}
	}

	// THE CLAUSE THE EIGHT FAILED. doc.go states the rule; a figure the rule
	// reaches that doc.go does not quote is a figure the register cannot have
	// argued about.
	//
	// IT IS SUBSUMED FOR EVERY ROW PRESENT TODAY, and saying so is the honest
	// version. Each figAllocBytes row now carries doc.go in its file set, so
	// the exact-file-set check above already implies this one for all of them;
	// what this clause still binds is a FUTURE row -- an allocation figure
	// added in some other file and classified here without being quoted in the
	// register. That is the case it was written for, and it is the only one it
	// now decides.
	unexplained := 0
	for n, entry := range figureInventory {
		if entry.kind != figAllocBytes {
			continue
		}
		if !entry.basis {
			unexplained++
		}
		if !inDoc[n] {
			t.Errorf("%s is an allocation figure that is not a multiple of eight and doc.go does not quote it "+
				"(found at %s); the register cannot argue a figure it does not name",
				withCommas(n), strings.Join(sites[n], ", "))
		}
	}
	// AND THE SPLIT THE REGISTER'S SENTENCE TURNS ON. doc.go says FIFTEEN
	// figures are in that state -- an allocation in bytes, not a multiple of
	// eight, and with no averaging basis recorded. That number was prose and
	// nothing held it: a sixteenth such figure quoted in doc.go would have
	// satisfied every clause above while the sentence quietly went stale, which
	// is the same defect this whole test exists for, one level up.
	if unexplained != figuresWithoutARecordedBasis {
		t.Errorf("%d allocation figures are not multiples of eight and record no averaging basis, "+
			"want %d: doc.go's register states that number in words, so move them together",
			unexplained, figuresWithoutARecordedBasis)
	}
}

// figuresWithoutARecordedBasis is how many allocation figures the register
// leaves standing as unexplained. IT IS A PIN AND NOT A TARGET: it moves when a
// figure is added, retracted, or given a basis, and doc.go's sentence moves with
// it.
const figuresWithoutARecordedBasis = 15

// joinFiles renders the distinct files a figure was found in, sorted, in the
// form figureInventory records.
func joinFiles(sites []string) string {
	seen := map[string]bool{}
	var files []string
	for _, s := range sites {
		f := s[:strings.LastIndex(s, ":")]
		if !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}
	sort.Strings(files)
	return strings.Join(files, ",")
}

// withCommas renders a figure the way the comments do, so a failure can be
// pasted straight into the register.
func withCommas(n int) string {
	s := strconv.Itoa(n)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// THE WIDEST FRAMING'S SHAPE IS COUNTED HERE, because three files state it and
// nothing checked it. canonical.go, doc.go and this file each say the widest
// witness framing makes thirty-one direct part calls and three helper calls,
// and doc.go said "an AST census confirms" it. No census existed: the figure
// was hand-maintained in three files, which is precisely the "second place a
// future field can be forgotten" the sentence is warning about. The previous
// wording ("a thirty-five-field framing") was wrong and claimed nothing; the
// correction was right and claimed a machine. This is the machine.
//
// AND ITS SCOPE IS MACHINE-STATED, NOT PROSE-STATED. A first draft counted only
// top-level funcs named frame..., which is the witness framings -- and "the
// widest of THESE framings" then rests on a reader knowing which set "these"
// is. SerializeCommonFactset is wider and the draft could not see it, so the
// sentence was true and unfalsifiable at once. Both sets are counted now and
// both maxima are pinned, so the claim names the set it is about.
//
// WHAT IT COUNTS. In every function that holds a canonical value -- as a
// *canonical parameter or as its own local -- a DIRECT part call is a call on
// that value, and a HELPER call is a call to another function handed it. The
// conversions inside a part call, c.i64(int64(x)) and c.str(string(x)), are
// neither: the canonical value is not among their arguments. A loop body counts
// once, which is why these are call-site counts and not field counts.
//
// WHAT IT DOES NOT DO. It does not say a framing frames every field of its
// type: that is TestAWitnessTypeCannotGrowAFieldQuietly's job, and the two are
// independent on purpose.
func TestTheWidestFramingIsTheShapeTheRegisterStates(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	type shape struct {
		direct, helper int
		where          string
	}
	shapes := map[string]shape{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			// THE PRIMITIVES THEMSELVES ARE NOT FRAMINGS. A method ON
			// *canonical -- str, i64, count -- writes parts by definition and
			// would otherwise dominate the maximum with its own internals.
			if fd.Recv != nil {
				continue
			}
			recv := canonicalHolderOf(fd)
			if recv == "" {
				continue
			}
			var sh shape
			sh.where = fmt.Sprintf("%s:%d", name, fset.Position(fd.Pos()).Line)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					// A READER IS NOT A PART WRITE. bytes, digest, framedLen and
					// grow are calls ON the canonical that write no part, and
					// counting them inflated SerializeCommonFactset by its own
					// `return c.bytes()` -- 36 where doc.go says 35. The widest
					// witness framing happens to call none of them, so the
					// inflation was invisible where it was checked and real
					// where it was not.
					if id, ok := fun.X.(*ast.Ident); ok && id.Name == recv &&
						!canonicalReaders[fun.Sel.Name] {
						sh.direct++
					}
				case *ast.Ident:
					for _, a := range call.Args {
						switch x := a.(type) {
						case *ast.Ident:
							if x.Name == recv {
								sh.helper++
							}
						case *ast.UnaryExpr:
							if id, ok := x.X.(*ast.Ident); ok && x.Op == token.AND && id.Name == recv {
								sh.helper++
							}
						}
					}
				}
				return true
			})
			shapes[fd.Name.Name] = sh
		}
	}
	// THE WALK MUST HAVE REACHED THEM, or every answer below is vacuously true.
	if len(shapes) < 10 {
		t.Fatalf("found only %d functions holding a canonical; the walk is not reaching them", len(shapes))
	}
	// NAMES ARE SORTED BEFORE EACH MAXIMUM IS TAKEN, so a tie resolves the same
	// way on every run rather than however the map happened to iterate.
	names := make([]string, 0, len(shapes))
	for name := range shapes {
		names = append(names, name)
	}
	sort.Strings(names)
	widest := func(only func(string) bool) (string, shape) {
		best := ""
		for _, name := range names {
			if !only(name) {
				continue
			}
			if best == "" || shapes[name].direct+shapes[name].helper > shapes[best].direct+shapes[best].helper {
				best = name
			}
		}
		return best, shapes[best]
	}
	all := func() string {
		var out []string
		for _, name := range names {
			out = append(out, fmt.Sprintf("%s=%d+%d", name, shapes[name].direct, shapes[name].helper))
		}
		return strings.Join(out, " ")
	}
	isWitnessFraming := func(name string) bool { return strings.HasPrefix(name, "frame") }

	// THE CLAIM THE THREE FILES MAKE, about the witness framings.
	name, got := widest(isWitnessFraming)
	if name != widestWitnessFraming || got.direct != widestWitnessDirect || got.helper != widestWitnessHelper {
		t.Errorf("the widest WITNESS framing is %s at %s with %d direct part calls and %d helper calls; "+
			"canonical.go, doc.go and this file state %s at %d and %d. All: %s",
			name, got.where, got.direct, got.helper,
			widestWitnessFraming, widestWitnessDirect, widestWitnessHelper, all())
	}
	// AND THE SET THE CLAIM IS NOT ABOUT, pinned so "these framings" cannot
	// quietly come to mean all of them.
	//
	// BOTH MAXIMA ARE HELD IN PROSE, not just in these constants. doc.go names
	// thirty-one for the witness framings and thirty-five for
	// SerializeCommonFactset, and the clause below requires both sentences to
	// be there: a figure pinned in one place and written in another is a figure
	// that drifts, which is the whole reason this census exists.
	name, got = widest(func(string) bool { return true })
	if name != widestFramingOverall || got.direct != widestOverallDirect || got.helper != widestOverallHelper {
		t.Errorf("the widest framing of ANY kind is %s at %s with %d direct part calls and %d helper calls; "+
			"this file states %s at %d and %d. All: %s",
			name, got.where, got.direct, got.helper,
			widestFramingOverall, widestOverallDirect, widestOverallHelper, all())
	}
	if isWitnessFraming(widestFramingOverall) {
		t.Errorf("%s is pinned as the widest framing overall and is also a witness framing; "+
			"the two pins exist because the sets differ", widestFramingOverall)
	}

	// AND THE PROSE THE CENSUS EXISTS FOR IS HELD TO THE SAME NUMBERS. Pinning
	// 31 and 3 as Go constants does nothing for the two HAND-WRITTEN copies of
	// them in canonical.go and doc.go -- a lane rewrote one to "forty-two ...
	// nine" and the whole suite stayed green. That is this branch's recurring
	// shape one layer up: the census was built to stop a figure drifting, and
	// the figure it was built for could still drift in the files that quote it.
	spelled := map[int]string{3: "three", 31: "thirty-one", 35: "thirty-five"}
	dw, hw := spelled[widestWitnessDirect], spelled[widestWitnessHelper]
	if dw == "" || hw == "" {
		t.Fatalf("no spelling for %d or %d: raise the pins and add the words, or this clause "+
			"stops holding the prose", widestWitnessDirect, widestWitnessHelper)
	}
	ow, ohw := spelled[widestOverallDirect], spelled[widestOverallHelper]
	if ow == "" || ohw == "" {
		t.Fatalf("no spelling for %d or %d", widestOverallDirect, widestOverallHelper)
	}
	// THE OVERALL SENTENCE NAMES ITS SUBJECT, and the witness one does not need
	// to. Both phrases were once "<spelling> direct part calls and three helper
	// calls", naming nothing -- so if the two maxima's direct counts ever
	// coincided, the overall clause would be satisfied by the WITNESS sentence
	// and neither would be held. What kept the sets apart was the separate name
	// assertion below, not this clause, while the paragraph it guards claimed
	// this clause did it.
	phrase := dw + " direct part calls and " + hw + " helper calls"
	overall := widestFramingOverall + " makes " + ow + " direct part calls and " + ohw + " helper calls"
	if strings.Contains(phrase, overall) || strings.Contains(overall, phrase) {
		t.Fatalf("the two pinned sentences are not distinguishable (%q vs %q): one could satisfy "+
			"the other's clause", phrase, overall)
	}
	if !saysInOneParagraph(t, "doc.go", overall) {
		t.Errorf("doc.go does not contain %q: the overall maximum is pinned as a constant here and "+
			"the sentence that names it must move with it", overall)
	}
	for _, name := range []string{"canonical.go", "doc.go"} {
		if !saysInOneParagraph(t, name, phrase) {
			t.Errorf("%s does not contain %q: the census pins the numbers and this clause pins the "+
				"sentences that quote them, because a figure held in one place and written in two "+
				"is a figure that drifts", name, phrase)
		}
	}
}

// constantEmptySlice reports whether a slice expression is empty whatever it
// slices: `x[:0]`, or `x[k:k]` for one literal k. Those carry none of the
// value's bytes, so naming a carrier inside one recomputes nothing.
//
// WHAT IT DOES NOT CATCH, and the behavioural oracles are why that is
// tolerable: any other value-destroying expression over a carrier -- a
// hash of a constant slice of it, a length, a comparison -- still reads as a
// mention. A witness that returns a constant is caught behaviourally, by
// result_witness_test.go editing eleven fields of a genuine result and
// requiring ErrResultNotDerived, and by the placement, payout and factset
// suites carrying the same shape. This walk is defence in depth, not the gate.
func constantEmptySlice(sl *ast.SliceExpr) bool {
	if sl.Slice3 {
		return false
	}
	if isZeroLit(sl.High) {
		return true
	}
	lo, loOK := sl.Low.(*ast.BasicLit)
	hi, hiOK := sl.High.(*ast.BasicLit)
	return loOK && hiOK && lo.Kind == token.INT && hi.Kind == token.INT && lo.Value == hi.Value
}

// flatParagraphs returns each PARAGRAPH of a production file's comments
// flattened to one line: runs of whitespace collapse to single spaces, so a
// pinned sentence is held wherever its AUTHOR wrapped it and cannot be matched
// line by line.
//
// NOT "WHEREVER gofmt WRAPS IT", which is what a previous justification said.
// gofmt does not wrap comment prose at all -- a 194-character line in this
// file's own comment shape survives it byte-identical -- so that sentence
// asserted a mechanism that does not exist. The wrapping this defends against
// is hand-wrapping, which is real and is why the flattening stays.
//
// PER PARAGRAPH, NOT PER FILE, and that distinction is the whole of this helper.
// A first version flattened the WHOLE FILE, so a pinned sentence could be
// assembled across a paragraph boundary out of two unrelated clauses while the
// real claim was gone, and the clause still passed. A lane demonstrated it on
// both pinned sentences, including the one that predates this helper.
//
// AND A COMMENT GROUP IS NOT A PARAGRAPH, which the second version got wrong.
// go/ast splits a group on a line that is not a comment at all; an EMPTY `//`
// line -- which is how every paragraph break in this package is written -- stays
// inside the group, and g.Text() hands it back as a blank line in the middle.
// So grouping alone rejoined exactly the boundary it was meant to respect, and
// the defeat still passed. The blank lines are what separate paragraphs, so they
// are what this splits on.
func flatParagraphs(t *testing.T, name string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	if len(f.Comments) == 0 {
		t.Fatalf("%s has no comments; every clause reading it would be vacuous", name)
	}
	var out []string
	for _, g := range f.Comments {
		for _, para := range strings.Split(g.Text(), "\n\n") {
			if flat := strings.Join(strings.Fields(para), " "); flat != "" {
				out = append(out, flat)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s flattened to no paragraphs; every clause reading it would be vacuous", name)
	}
	return out
}

// saysInOneParagraph reports whether any single paragraph carries the sentence.
func saysInOneParagraph(t *testing.T, name, phrase string) bool {
	t.Helper()
	for _, para := range flatParagraphs(t, name) {
		if strings.Contains(para, phrase) {
			return true
		}
	}
	return false
}

// canonicalReaders are the methods on a canonical that write no part, so a
// call to one is not a part call however it is spelled.
var canonicalReaders = map[string]bool{
	"bytes": true, "digest": true, "framedLen": true, "grow": true,
}

// The two maxima, pinned. RAISE THEM WITH THE CODE, and treat either moving on
// its own as the finding it is: the witness pair is quoted in canonical.go and
// doc.go, and the overall pair is what keeps "the widest of these framings"
// from being read as a claim about the package.
const (
	widestWitnessFraming = "frameP3bResult"
	widestWitnessDirect  = 31
	widestWitnessHelper  = 3

	widestFramingOverall = "SerializeCommonFactset"
	widestOverallDirect  = 35
	widestOverallHelper  = 3
)

// canonicalHolderOf returns the name a function gives the canonical value it
// writes through -- a *canonical parameter, or a local declared as `var c
// canonical` or `c := canonical{...}`. A framing that builds its own buffer is
// still a framing, and the first draft of this census, which looked only at
// parameters, could not see the widest one in the package.
func canonicalHolderOf(fd *ast.FuncDecl) string {
	if fd.Type.Params != nil {
		for _, p := range fd.Type.Params.List {
			star, ok := p.Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if id, ok := star.X.(*ast.Ident); ok && id.Name == "canonical" && len(p.Names) == 1 {
				return p.Names[0].Name
			}
		}
	}
	found := ""
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		switch x := n.(type) {
		case *ast.ValueSpec:
			if id, ok := x.Type.(*ast.Ident); ok && id.Name == "canonical" && len(x.Names) == 1 {
				found = x.Names[0].Name
			}
		case *ast.AssignStmt:
			if len(x.Lhs) != 1 || len(x.Rhs) != 1 {
				return true
			}
			lit, ok := x.Rhs[0].(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "canonical" {
				if nm, ok := x.Lhs[0].(*ast.Ident); ok {
					found = nm.Name
				}
			}
		}
		return true
	})
	return found
}

// SIGNED ZERO AND A NaN PAYLOAD ARE WHERE THE BIT RENDERING EARNS ITS KEEP,
// and until now nothing drove either. canonical.go renders a float's BITS
// precisely so that two distinguishable values cannot round together, and
// doc.go records "±0.0 still frame apart" as a checked property. It was not
// checked: a lane wrote `c.f64(o.Odds + 0)` -- which collapses -0.0 to +0.0 and
// quiets a NaN payload, and is otherwise the identity -- and it SURVIVED the
// whole suite. A wholesale rendering change (FormatFloat instead of the bits)
// is caught by the factset golden; this one edge is not, because no fixture in
// the package produces either value.
//
// WHAT THIS DOES NOT CLAIM. It does not claim any producer here emits -0.0 or
// a NaN: VerifyCommonFactset refuses non-finite odds, so the class is reachable
// through the FRAMER rather than through a verified factset. The rendering is a
// stated property of the framer, and this is that property's receipt.
func TestTheFramerKeepsSignedZeroAndNaNPayloadsApart(t *testing.T) {
	framed := func(v float64) string {
		var c canonical
		c.f64(v)
		return string(c.bytes())
	}
	negZero := math.Copysign(0, -1)
	// A VACUOUS CASE WOULD PASS EVERY ASSERTION BELOW, so each input class is
	// confirmed to be the class it is named for before it is used.
	if !math.Signbit(negZero) || math.Signbit(0) {
		t.Fatal("the fixture did not produce a negative zero; every case below would be vacuous")
	}
	if negZero != 0 {
		t.Fatal("negative zero does not compare equal to zero; this test is about values == cannot tell apart")
	}
	if framed(negZero) == framed(0) {
		t.Error("-0.0 and +0.0 frame identically: the bit rendering exists so that two values " +
			"== cannot distinguish do not round together in the digest")
	}
	// TWO NaNs WITH DIFFERENT PAYLOADS. Every comparison on a NaN is false, so
	// these are confirmed by their bits and not by ==.
	a := math.Float64frombits(0x7FF8000000000001)
	b := math.Float64frombits(0x7FF8000000000002)
	if !math.IsNaN(a) || !math.IsNaN(b) || math.Float64bits(a) == math.Float64bits(b) {
		t.Fatal("the fixture did not produce two distinct NaNs; the case below would be vacuous")
	}
	if framed(a) == framed(b) {
		t.Error("two NaNs with different payloads frame identically")
	}
	// AND BOTH MODES MUST AGREE ON THE WIDTH for every one of these, which is
	// what every other framing in this package is held to.
	for _, v := range []float64{negZero, 0, a, b, math.Inf(1), math.Inf(-1)} {
		l := canonical{lenOnly: true}
		l.f64(v)
		var c canonical
		c.f64(v)
		if l.framedLen() != len(c.bytes()) {
			t.Errorf("f64(%x): length-only reports %d, the framing writes %d bytes",
				math.Float64bits(v), l.framedLen(), len(c.bytes()))
		}
	}
}

// AND THE FIVE CALL SITES, not just the primitive. The receipt above holds
// canonical.f64 itself; it does NOT hold the five places this package hands a
// float to it, and a lane's surviving mutant was at one of those -- c.f64(o.Odds
// + 0) -- not at the framer. A test that kills a fault at the primitive and
// leaves it alive one call up is the "repair stops at the gate that was
// reported" failure this package names in canonical.go, so both are held.
//
// THE CLASS IS ACCEPTED INPUT, which is why this is worth a test rather than a
// note: finiteFloat refuses only Inf and NaN, so a factset carrying a NEGATIVE
// ZERO verifies, and SerializeCommonFactset is exported and ungated. Two
// factsets that == cannot tell apart must not share a serialization.
//
// NO GOLDEN IS INVOLVED. The two sides are compared against each other, so this
// adds no pinned bytes and changes none.
func TestEveryFloatTheFactsetFramesKeepsSignedZeroApart(t *testing.T) {
	neg := math.Copysign(0, -1)
	if !math.Signbit(neg) || neg != 0 {
		t.Fatal("the fixture did not produce a negative zero; every case below would be vacuous")
	}
	build := func() CommonFactset {
		return CommonFactset{
			Outcomes: []predictioneval.OutcomeInput{{}},
			Settings: &predictioneval.BetSettingsInput{
				// FilterCondition is a POINTER and serializeSettings frames its
				// Value only when it is set, so a nil here would make the fifth
				// row vacuous rather than failing it.
				FilterCondition: &predictioneval.SourceFilterCondition{},
			},
		}
	}
	for _, tc := range []struct {
		field string
		set   func(*CommonFactset, float64)
	}{
		{"Outcomes[0].PercentageUsers", func(f *CommonFactset, v float64) { f.Outcomes[0].PercentageUsers = v }},
		{"Outcomes[0].Odds", func(f *CommonFactset, v float64) { f.Outcomes[0].Odds = v }},
		{"Outcomes[0].OddsPercentage", func(f *CommonFactset, v float64) { f.Outcomes[0].OddsPercentage = v }},
		{"Settings.Delay", func(f *CommonFactset, v float64) { f.Settings.Delay = v }},
		{"Settings.FilterCondition.Value", func(f *CommonFactset, v float64) { f.Settings.FilterCondition.Value = v }},
	} {
		pos, negf := build(), build()
		tc.set(&pos, 0)
		tc.set(&negf, neg)
		a, b := SerializeCommonFactset(pos), SerializeCommonFactset(negf)
		if len(a) == 0 || len(b) == 0 {
			t.Fatalf("%s: the framing produced nothing; the comparison below would be vacuous", tc.field)
		}
		if string(a) == string(b) {
			t.Errorf("%s: +0.0 and -0.0 serialize identically, so this site does not render the bits",
				tc.field)
		}
	}
}

// fieldIndexOf HAS A CONTROL NOW, because its repair had no receipt. The
// embedded-field branch it gained -- an embedded field has no name and still
// costs the literal an element -- is dead against the current source: the row
// struct it walks has no embedded field, so removing the i++ again left the
// WHOLE SUITE GREEN. A repair nothing would notice regressing is a repair on
// trust, and this package's own discipline is a control table per recognizer.
//
// The source below is synthetic and carries the construct the repair is for.
// WHAT IT DOES NOT DO: it does not check any production row; it checks the
// WALK, over a shape the production rows do not currently have.
func TestFieldIndexOfCountsAnEmbeddedField(t *testing.T) {
	const src = `package p

type emb struct{ E int }

var rows = []struct {
	emb
	name  string
	parts int
	want  []string
}{}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	var typ ast.Expr
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.CompositeLit); ok && typ == nil {
			typ = lit.Type
		}
		return true
	})
	if typ == nil {
		t.Fatal("no composite literal in the synthetic source; every case below would be vacuous")
	}
	// THE EMBEDDED FIELD IS ELEMENT 0, so every named field sits one later
	// than its position among the NAMES. That off-by-one is the whole defect.
	for _, tc := range []struct {
		name string
		want int
	}{
		{"name", 1},
		{"parts", 2},
		{"want", 3},
		{"absent", -1},
	} {
		if got := fieldIndexOf(typ, tc.name); got != tc.want {
			t.Errorf("fieldIndexOf(%q) = %d, want %d: an embedded field costs the literal an "+
				"element, so skipping it without advancing shifts every index after it",
				tc.name, got, tc.want)
		}
	}
}

// THE LENGTH PREFIX IS EIGHT BYTES AND ONLY TWO OF THEM WERE EVER EXERCISED.
// canonical.lpPrefix writes a big-endian uint64 before every part, and that
// prefix is what makes the framing self-delimiting -- it is the whole reason
// two adjacent parts cannot be re-cut into a different pair. But no fixture in
// this package ever framed a part long enough for byte 2 and above to be
// non-zero, so zeroing them changed nothing any test could see: a lane zeroed
// `byte(n>>16)` and the WHOLE SUITE stayed green, then showed two DISTINCT
// VerifiedP3bRuleset identities of 65,536 bytes framing to the same 65,602
// bytes under it. A length prefix that drops its high bytes is a framing that
// is no longer injective, and `RulesetID` is caller-supplied and unbounded --
// rulesetRawCeiling bounds RawBytes, not the identity -- with a 16 MiB ruleset
// id quoted two files over as reachable through the exported verifier.
//
// THE ORACLE IS INDEPENDENT, which is the point: the expected prefix comes from
// encoding/binary, not from lpPrefix or from a recorded golden. A prefix
// checked against itself would agree with any of these mutants.
//
// WHAT IT DOES NOT REACH: bytes 4 through 7 need a part of 4 GiB or more. They
// are unreachable within this process's own memory, so a mutant zeroing one is
// equivalent over the reachable domain rather than a gap -- stated here because
// "the test covers the prefix" would otherwise be read as covering all eight.
func TestTheLengthPrefixRendersEveryByteItCanReach(t *testing.T) {
	framedPrefix := func(n int) []byte {
		var c canonical
		c.str(strings.Repeat("a", n))
		if len(c.bytes()) < 8 {
			t.Fatalf("a part of %d bytes framed %d bytes; the case below would be vacuous", n, len(c.bytes()))
		}
		return c.bytes()[:8]
	}
	// Each length is chosen to put a NON-ZERO in one byte of the prefix that a
	// shorter part leaves at zero: 1<<8 reaches byte 6, 1<<16 byte 5, 1<<24
	// byte 4 (counting from the most significant).
	for _, n := range []int{0, 1, 255, 1 << 8, 1<<16 - 1, 1 << 16, 1<<16 | 1<<8 | 1, 1 << 24} {
		var want [8]byte
		binary.BigEndian.PutUint64(want[:], uint64(n))
		got := framedPrefix(n)
		if !bytes.Equal(got, want[:]) {
			t.Errorf("a part of %d bytes is prefixed %x, want %x: the prefix is what makes the "+
				"framing self-delimiting, so a dropped byte is a framing that is not injective",
				n, got, want)
		}
	}
	// AND THE CONSEQUENCE, stated as a value rather than as arithmetic: two
	// identities differing only above the 16-bit boundary must not frame alike.
	big, bigger := strings.Repeat("a", 1<<16), strings.Repeat("a", 2<<16)
	var a, b canonical
	a.str(big)
	b.str(bigger)
	if string(a.bytes()[:8]) == string(b.bytes()[:8]) {
		t.Error("parts of 65,536 and 131,072 bytes carry the same length prefix: two identities " +
			"differing only in a high length byte would share a witness")
	}
	// BOTH MODES MUST STILL AGREE at these widths, which is the property every
	// other framing in this package is held to.
	for _, n := range []int{1 << 16, 1 << 24} {
		l := canonical{lenOnly: true}
		l.str(strings.Repeat("a", n))
		var c canonical
		c.str(strings.Repeat("a", n))
		if l.framedLen() != len(c.bytes()) {
			t.Errorf("a part of %d bytes: length-only reports %d, the framing writes %d",
				n, l.framedLen(), len(c.bytes()))
		}
	}
}

// A CLOSED VOCABULARY NEEDS EVERY MEMBER DIFFERENTIATED, not just one member
// present. The parts fixtures draw enum-valued fields through text(), which
// produces arbitrary bytes and so distinguishes any two values -- but three
// fields are set from a CLOSED vocabulary the fixture never varies, and a lane
// showed each of them collapsing one in-vocabulary member onto another with the
// whole suite green, the pinned wire goldens included. A golden pins ONE value
// per field; it cannot see a framer that maps a second value onto the first.
//
//   - FactsetCompleteness at factset.go:587. BuildCommonFactset produces both
//     PRE_DECISION_EXIT and INCOMPLETE, and both reach SerializeCommonFactset.
//     Collapsed, two materially different factsets share the FactsetDigest that
//     every downstream record binds itself to.
//   - ResolutionOutcome at resolution.go:462. Collapsed, two distinct artifacts
//     share a ResolutionFactsDigest.
//
// THE ORACLE IS A DIFFERENTIAL, not a golden: the two sides are compared with
// each other, so this pins no bytes and changes none. It asks the only question
// a golden cannot -- are these two members distinguishable at all.
//
// WHAT IT DOES NOT DO: it does not check that a member is rendered as its
// documented STRING. That is the golden's job, and the golden does it for the
// one member it carries.
func TestEveryClosedVocabularyIsDifferentiatedByItsFraming(t *testing.T) {
	factset := func(c FactsetCompleteness) string {
		return string(SerializeCommonFactset(CommonFactset{Completeness: c}))
	}
	for _, pair := range [][2]FactsetCompleteness{
		{FactsetComplete, FactsetIncomplete},
		{FactsetComplete, FactsetPreDecisionExit},
		{FactsetPreDecisionExit, FactsetIncomplete},
	} {
		a, b := factset(pair[0]), factset(pair[1])
		if len(a) == 0 {
			t.Fatalf("the factset framing produced nothing; every case here would be vacuous")
		}
		if a == b {
			t.Errorf("Completeness %q and %q serialize identically: two factsets that differ only "+
				"here would share the digest every downstream record binds to", pair[0], pair[1])
		}
	}
	artifact := func(o ResolutionOutcome) string {
		return string(SerializeResolutionArtifact(ResolutionArtifact{Outcome: o, WinnerIndex: -1}))
	}
	for _, pair := range [][2]ResolutionOutcome{
		{ResolutionWinnerKnown, ResolutionRefund},
		{ResolutionWinnerKnown, ResolutionUnknown},
		{ResolutionRefund, ResolutionUnknown},
	} {
		a, b := artifact(pair[0]), artifact(pair[1])
		if len(a) == 0 {
			t.Fatalf("the artifact framing produced nothing; every case here would be vacuous")
		}
		if a == b {
			t.Errorf("Outcome %q and %q serialize identically: two artifacts that differ only here "+
				"would share a ResolutionFactsDigest", pair[0], pair[1])
		}
	}
}

// THE CLAIM COMPARATOR'S LENGTH CLAUSE WAS HELD BY NOTHING. lpCompare orders
// two strings the way canonical.str frames them -- the eight-byte length first,
// the bytes only when the lengths are equal -- and that order is what
// sortClaimsByKey feeds into registryDigest. Deleting the clause left the WHOLE
// SUITE GREEN, the registry verifier and the registry golden included, while
// producing a different registryDigest for the same registry.
//
// THE FIXTURE COULD NOT SEE IT, and the reason is the shape this branch keeps
// finding. Every claim shape it draws either differs in Episode.framedLen -- so
// compareClaimKeys returns before lpCompare is reached -- or uses one-character
// values, so every operand pair is equal-length. lpCompare was never called
// with unequal-length operands at all. A previous round closed the POSITION
// half of that blind spot by adding the one-character family; this is the
// LENGTH half it left open.
//
// THE ORACLE IS THE FRAMING ITSELF, not a restatement of the comparator: the
// expected order is a byte comparison of what canonical.str actually writes. A
// comparator checked against its own arithmetic would agree with any mutant of
// it. The three Attempt sites sit behind no length-tie constraint, so
// unequal-length operands reach lpCompare on ordinary input.
func TestTheClaimComparatorOrdersUnequalLengthsAsTheFramingDoes(t *testing.T) {
	framed := func(s string) string {
		var c canonical
		c.str(s)
		return string(c.bytes())
	}
	sign := func(n int) int {
		switch {
		case n < 0:
			return -1
		case n > 0:
			return 1
		}
		return 0
	}
	disagreements := 0
	for _, pair := range [][2]string{
		// Unequal lengths where byte order and FRAMING order disagree: "aa"
		// sorts before "b" by bytes and after it by framing, because the
		// framing compares the length first.
		{"b", "aa"}, {"aa", "b"},
		{"z", "ab"}, {"ab", "z"},
		{"9", "10"}, {"10", "9"},
		{"~", "!!"},
		// The empty string is a length the framing distinguishes and the byte
		// loop cannot reach.
		{"", "a"}, {"a", ""}, {"", ""},
		// Across the 255/256 boundary, where the length's own rendering grows.
		{strings.Repeat("a", 255), strings.Repeat("b", 256)},
		{strings.Repeat("b", 256), strings.Repeat("a", 255)},
		// Equal lengths, so the byte loop is what decides.
		{"aa", "ab"}, {"ab", "aa"}, {"aa", "aa"},
	} {
		a, b := pair[0], pair[1]
		want := sign(strings.Compare(framed(a), framed(b)))
		if got := sign(lpCompare(a, b)); got != want {
			t.Errorf("lpCompare(%q, %q) = %d, the framing orders them %d: the comparator must "+
				"reproduce the order registryDigest is computed over", a, b, got, want)
		}
		if len(a) != len(b) && sign(strings.Compare(a, b)) != want {
			disagreements++
		}
	}
	// NON-VACUITY. At least one row must be a pair whose BYTE order differs
	// from its FRAMING order, or every row above would pass for a comparator
	// with no length clause at all -- which is exactly the mutant that survived.
	if disagreements == 0 {
		t.Fatal("no row distinguishes framing order from byte order; every case above would " +
			"pass for a comparator that ignored length entirely")
	}
}
