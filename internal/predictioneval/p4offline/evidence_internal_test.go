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
	"encoding/json"
	"errors"
	"math/rand"
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
		if got, want := hexEncode(claimKeyFraming(nil, cs[i])), claimKey(cs[i]); got != want {
			t.Fatalf("claim %d: the framing does not hex-encode to the key", i)
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
