package p4offline_test

// SEAMS 1–3: session admission, first-opportunity selection without later
// substitution, the COMMON_CUTOFF boundary proof, and source-round
// deduplication.
//
// Every expectation is stated from the protocol rules, never derived from the
// implementation: which session classifications are admitted, that the FIRST
// automated opportunity is the only one an episode can offer, that C must be
// STRICTLY before F, that a missing F needs a complete no-call coverage proof,
// and that a reused public round is not evidence twice.

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

type (
	testingT    = *testing.T
	p4selection = p4offline.EvidenceSelection
	p4episode   = p4offline.EpisodeSelection
)

func mustSelect(t *testing.T, ds predictioneval.SourceDataset) p4offline.EvidenceSelection {
	t.Helper()
	sel, err := p4offline.SelectEpisodes(ds)
	if err != nil {
		t.Fatalf("SelectEpisodes: %v", err)
	}
	return sel
}

// TestSelectEpisodesAdmitsOnlyCompleteFinalizedSessions pins the session
// gate: COMPLETE + AS_FINALIZED, the supported producer revision, every fact
// present, every witness verified, nothing lost. Anything else yields no
// episodes at all, with the refusal named.
func TestSelectEpisodesAdmitsOnlyCompleteFinalizedSessions(t *testing.T) {
	build := func() *synth {
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		return s
	}
	control := mustSelect(t, build().dataset())
	if !control.SessionAdmitted || len(control.Episodes) != 1 || control.Episodes[0].Excluded {
		t.Fatalf("the control session must be admitted with one selected episode: %+v", control)
	}
	if control.ProtocolVersion != p4offline.ProtocolVersion {
		t.Fatalf("selection carries protocol %q", control.ProtocolVersion)
	}

	cases := []struct {
		name   string
		mutate func(*predictioneval.SourceDataset)
		reason string
	}{
		{"unfinalized reading", func(ds *predictioneval.SourceDataset) { ds.Source.SessionReading = "UNFINALIZED" }, "SESSION_READING_NOT_AS_FINALIZED"},
		{"incomplete close state", func(ds *predictioneval.SourceDataset) { ds.Source.CloseState = "INCOMPLETE" }, "SESSION_CLOSE_STATE_NOT_COMPLETE"},
		{"legacy producer revision", func(ds *predictioneval.SourceDataset) { ds.Source.ProducerRevision = "obs-v1" }, "PRODUCER_REVISION_UNSUPPORTED"},
		{"lost facts", func(ds *predictioneval.SourceDataset) { ds.Source.DroppedCount = 1 }, "SESSION_FACTS_LOST"},
		{"a negative counter", func(ds *predictioneval.SourceDataset) { ds.Source.DroppedCount = -5 }, "SESSION_COUNTERS_NEGATIVE"},
		{"pruned facts", func(ds *predictioneval.SourceDataset) { ds.Source.CommittedCount++ }, "SESSION_FACTS_INCOMPLETE"},
		{"unchecked witnesses", func(ds *predictioneval.SourceDataset) {
			ds.Source.WitnessesUnchecked = 1
			ds.Source.WitnessesVerified--
		}, "SESSION_WITNESSES_UNCHECKED"},
		{"no witness verified", func(ds *predictioneval.SourceDataset) { ds.Source.WitnessesVerified = 0 }, "SESSION_NO_WITNESSES_VERIFIED"},
		{"a foreign session's fact in the dataset", func(ds *predictioneval.SourceDataset) {
			foreign := ds.Records[0]
			foreign.CollectorSessionID = "someone-else"
			foreign.ObservationID = "foreign"
			foreign.CollectorSequence = ds.Records[len(ds.Records)-1].CollectorSequence + 1
			ds.Records = append(ds.Records, foreign)
		}, "SESSION_FOREIGN_FACTS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds := build().dataset()
			tc.mutate(&ds)
			sel := mustSelect(t, ds)
			if sel.SessionAdmitted {
				t.Fatalf("session must be refused")
			}
			if !containsString(sel.SessionRefusals, tc.reason) {
				t.Fatalf("refusals %v do not name %s", sel.SessionRefusals, tc.reason)
			}
			if len(sel.Episodes) != 0 {
				t.Fatalf("a refused session yields no episodes, got %d", len(sel.Episodes))
			}
		})
	}

	t.Run("records out of causal order are a caller error", func(t *testing.T) {
		ds := build().dataset()
		ds.Records[0], ds.Records[1] = ds.Records[1], ds.Records[0]
		if _, err := p4offline.SelectEpisodes(ds); err == nil {
			t.Fatal("an unordered dataset must be refused with an error, not read")
		}
	})
}

// TestFirstAutomatedOpportunityIsNeverSubstitutedByALaterOne pins seam 2.
//
// The first automatic attempt on the round is the episode's only opportunity.
// When it is unusable — here its terminal fact carries no envelope — the
// episode is excluded even though a later, perfectly usable attempt exists.
func TestFirstAutomatedOpportunityIsNeverSubstitutedByALaterOne(t *testing.T) {
	t.Run("an unusable first opportunity excludes the episode", func(t *testing.T) {
		s := newSynth()
		s.due("r1", "e1", 1)
		s.terminal("r1", "e1", 1, predictioneval.PhaseAutoSkipped, "BELOW_MINIMUM_POINTS", nil)
		s.placedAttempt("r1", "e1", 2)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if !ep.Excluded || !containsString(ep.ExclusionReasons, "FIRST_OPPORTUNITY_UNUSABLE") {
			t.Fatalf("episode must be excluded as FIRST_OPPORTUNITY_UNUSABLE: %+v", ep)
		}
		if ep.FirstOpportunity == nil || ep.FirstOpportunity.AttemptID != 1 {
			t.Fatalf("the first opportunity is attempt 1, got %+v", ep.FirstOpportunity)
		}
		if ep.FirstOpportunityUsable || ep.Attempt != nil {
			t.Fatalf("no attempt may be selected in place of the unusable first one: %+v", ep.Attempt)
		}
		if ep.LaterAttempts != 1 {
			t.Fatalf("the later attempt must be counted as refused substitution: %d", ep.LaterAttempts)
		}
		if !containsString(ep.P2Exclusions, predictioneval.ExclusionTerminalWithoutEnvelope) {
			t.Fatalf("the P2 exclusion must be carried: %v", ep.P2Exclusions)
		}
		if ep.Quality.Quality != p4offline.QualityExcluded {
			t.Fatalf("quality %q", ep.Quality.Quality)
		}
	})

	t.Run("a usable first opportunity is selected and the later one is ignored", func(t *testing.T) {
		s := newSynth()
		s.skippedAttempt("r1", "e1", 1)
		s.placedAttempt("r1", "e1", 2)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if ep.Excluded {
			t.Fatalf("episode must be selected: %v", ep.ExclusionReasons)
		}
		if ep.Attempt == nil || ep.Attempt.Key.AttemptID != 1 {
			t.Fatalf("attempt 1 must be the selected opportunity: %+v", ep.Attempt)
		}
		if ep.LaterAttempts != 1 || ep.FirstOpportunityPosition != 1 {
			t.Fatalf("later attempts: %d, first opportunity at %d", ep.LaterAttempts, ep.FirstOpportunityPosition)
		}
		// The later attempt's OWN call is a later F: C < F holds and the
		// boundary is proven by that call, not by a coverage argument.
		if !ep.Boundary.Proven || !ep.Boundary.EarliestCallPresent || ep.Boundary.EarliestCallKind != p4offline.CallKindAuto {
			t.Fatalf("boundary: %+v", ep.Boundary)
		}
		if ep.Quality.Quality != p4offline.QualityPrimaryScorable {
			t.Fatalf("a selected episode starts PRIMARY_SCORABLE, got %q (%v)", ep.Quality.Quality, ep.Quality.Reasons)
		}
	})

	t.Run("an automatic row naming no attempt is an unusable first opportunity", func(t *testing.T) {
		s := newSynth()
		s.add(s.fact(predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, "r1", "e1", 0))
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		// The row exists, so the episode HAD a first opportunity; it names no
		// attempt, so nothing can be materialized for it.
		if !ep.Excluded || !containsString(ep.ExclusionReasons, "FIRST_OPPORTUNITY_UNUSABLE") {
			t.Fatalf("%+v", ep)
		}
		if ep.FirstOpportunityPosition != 1 || ep.FirstOpportunityUsable || ep.Attempt != nil {
			t.Fatalf("its position is still evidence and nothing stands in for it: %+v", ep)
		}
	})
}

// TestManualOrAmbiguousCallsExcludeTheEpisodeOrBreakTheBoundary pins the
// manual rule and the "across incarnations" rule.
func TestManualOrAmbiguousCallsExcludeTheEpisodeOrBreakTheBoundary(t *testing.T) {
	t.Run("a manual call before the cutoff", func(t *testing.T) {
		s := newSynth()
		s.due("r1", "e1", 1)
		s.manualCall("r1", "e1", 30, 1)
		s.terminal("r1", "e1", 1, predictioneval.PhaseAutoDecided, "OK", synthPlacedEnvelope())
		s.call("r1", "e1", 1, 50, 0)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if !ep.Excluded || !containsString(ep.ExclusionReasons, "MANUAL_INTERVENTION") {
			t.Fatalf("%+v", ep)
		}
		if ep.Boundary.Proven || ep.Boundary.Reason != "C_NOT_BEFORE_F" || ep.Boundary.EarliestCallKind != p4offline.CallKindManual {
			t.Fatalf("the manual call is the earliest F and sits before C: %+v", ep.Boundary)
		}
		if len(ep.ManualSignals) == 0 {
			t.Fatal("the manual signal must be named")
		}
	})

	t.Run("a manual call after the cutoff still excludes the whole episode", func(t *testing.T) {
		s := newSynth()
		s.skippedAttempt("r1", "e1", 1)
		s.manualCall("r1", "e1", 30, 1)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if !ep.Excluded || !containsString(ep.ExclusionReasons, "MANUAL_INTERVENTION") {
			t.Fatalf("%+v", ep)
		}
		if !ep.Boundary.Proven {
			t.Fatalf("C < F holds here; the exclusion is the manual rule, not the boundary: %+v", ep.Boundary)
		}
	})

	t.Run("a manual call on another incarnation of the same event", func(t *testing.T) {
		s := newSynth()
		s.due("r1", "e1", 1)
		s.manualCall("r1-readmitted", "e1", 30, 1)
		s.terminal("r1", "e1", 1, predictioneval.PhaseAutoSkipped, "BELOW_MINIMUM_POINTS", synthSkippedEnvelope())
		sel := mustSelect(t, s.dataset())
		var ep *p4offline.EpisodeSelection
		for i := range sel.Episodes {
			if sel.Episodes[i].Episode.RoundIncarnationID == "r1" {
				ep = &sel.Episodes[i]
			}
		}
		if ep == nil {
			t.Fatalf("episode r1 missing: %+v", sel.Episodes)
		}
		if !ep.Excluded || ep.Boundary.Proven || ep.Boundary.Reason != "C_NOT_BEFORE_F" {
			t.Fatalf("a call on another incarnation of the SAME event is still an F: %+v", ep)
		}
	})

	t.Run("an undecodable placement fact breaks the boundary", func(t *testing.T) {
		s := newSynth()
		s.due("r1", "e1", 1)
		amb := s.fact(predictioneval.KindPlacement, "", "r1", "e1", 0)
		amb.PayloadUndecodable = true
		s.add(amb)
		s.terminal("r1", "e1", 1, predictioneval.PhaseAutoSkipped, "BELOW_MINIMUM_POINTS", synthSkippedEnvelope())
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if !ep.Excluded || !containsString(ep.ExclusionReasons, "BOUNDARY_NOT_PROVEN") {
			t.Fatalf("%+v", ep)
		}
		if ep.Boundary.EarliestCallKind != p4offline.CallKindAmbiguous || !ep.Boundary.EarliestCallPresent {
			t.Fatalf("an undecodable placement fact is an AMBIGUOUS call at its own position: %+v", ep.Boundary)
		}
		// Unreadable is also unreadable FOR COVERAGE, and the boundary reports
		// that before it compares positions at all.
		if ep.Boundary.Reason != "NO_CALL_COVERAGE_UNPROVEN" ||
			!containsString(ep.Boundary.NoCallCoverage.Reasons, "UNDECODABLE_FACT_ON_ROUND") {
			t.Fatalf("%+v", ep.Boundary)
		}
	})
}

// TestCommonCutoffRequiresTheCutoffStrictlyBeforeTheEarliestCall pins the
// boundary predicate itself, including C == F, which a well-formed store
// cannot produce through SelectEpisodes but the predicate must still refuse.
func TestCommonCutoffRequiresTheCutoffStrictlyBeforeTheEarliestCall(t *testing.T) {
	calls := []p4offline.CallSignal{
		{Position: 12, ObservationID: "later", Kind: p4offline.CallKindAuto},
		{Position: 10, ObservationID: "earliest", Kind: p4offline.CallKindManual},
	}
	proven := p4offline.NoCallCoverageProof{Proven: true}
	for _, tc := range []struct {
		cutoff int64
		want   bool
	}{{9, true}, {10, false}, {11, false}} {
		b := p4offline.ProveCommonCutoff(tc.cutoff, "terminal", calls, proven)
		if b.Proven != tc.want {
			t.Fatalf("cutoff %d against F=10: proven=%v, want %v (%+v)", tc.cutoff, b.Proven, tc.want, b)
		}
		if b.Rule != p4offline.BoundaryRuleCommonCutoff || !b.EarliestCallPresent || b.EarliestCallPosition != 10 ||
			b.EarliestCallObservationID != "earliest" || b.EarliestCallKind != p4offline.CallKindManual {
			t.Fatalf("F must be the EARLIEST call, whatever its kind: %+v", b)
		}
		if !tc.want && b.Reason != "C_NOT_BEFORE_F" {
			t.Fatalf("reason %q", b.Reason)
		}
	}
	// No call at all: the coverage proof decides.
	ok := p4offline.ProveCommonCutoff(9, "terminal", nil, p4offline.NoCallCoverageProof{Proven: true})
	if !ok.Proven || ok.EarliestCallPresent {
		t.Fatalf("%+v", ok)
	}
	bad := p4offline.ProveCommonCutoff(9, "terminal", nil, p4offline.NoCallCoverageProof{Proven: false, Reasons: []string{"x"}})
	if bad.Proven || bad.Reason != "NO_CALL_COVERAGE_UNPROVEN" {
		t.Fatalf("a missing F without a complete coverage proof is never proven: %+v", bad)
	}
	// Coverage is required WITH an F too: a call could sit unrecorded before
	// the recorded one, and the zero value of the proof proves nothing.
	unproven := p4offline.ProveCommonCutoff(9, "terminal", calls, p4offline.NoCallCoverageProof{})
	if unproven.Proven || unproven.Reason != "NO_CALL_COVERAGE_UNPROVEN" || !unproven.EarliestCallPresent {
		t.Fatalf("C < F with no coverage proof establishes nothing: %+v", unproven)
	}
	// The predicate does not write into the caller's proof.
	reasons := make([]string, 1, 4)
	reasons[0] = "mine"
	_ = p4offline.ProveCommonCutoff(9, "terminal", nil, p4offline.NoCallCoverageProof{Reasons: reasons})
	if reasons[:2][1] != "" {
		t.Fatalf("the caller's backing array was written: %v", reasons[:2])
	}
}

// TestMissingEarliestCallRequiresACompleteNoCallCoverageProof pins seam 1's
// coverage rule through the dataset: a skip with no call is proven only when
// nothing in the session could hide a call.
func TestMissingEarliestCallRequiresACompleteNoCallCoverageProof(t *testing.T) {
	t.Run("a complete session proves the absence of a call", func(t *testing.T) {
		s := newSynth()
		s.skippedAttempt("r1", "e1", 1)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if ep.Excluded || !ep.Boundary.Proven || ep.Boundary.EarliestCallPresent || !ep.Boundary.NoCallCoverage.Proven {
			t.Fatalf("%+v", ep)
		}
	})
	t.Run("an orphan CALL_RETURNED means a call started that was never recorded", func(t *testing.T) {
		s := newSynth()
		s.skippedAttempt("r1", "e1", 1)
		s.placement("r1", "e1", 0, predictioneval.PhaseCallReturned, 30, 1, "OK", "NONE")
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if !ep.Excluded || ep.Boundary.Proven || ep.Boundary.Reason != "NO_CALL_COVERAGE_UNPROVEN" {
			t.Fatalf("%+v", ep)
		}
	})
	t.Run("an unclassified fact on the round breaks coverage", func(t *testing.T) {
		s := newSynth()
		s.skippedAttempt("r1", "e1", 1)
		s.add(s.fact("source_unknown", "UNCLASSIFIED", "r1", "e1", 0))
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if !ep.Excluded || ep.Boundary.Proven || ep.Boundary.Reason != "NO_CALL_COVERAGE_UNPROVEN" {
			t.Fatalf("%+v", ep)
		}
	})
	t.Run("an unclassified fact naming no incarnation and a round nobody admitted breaks coverage on its pool", func(t *testing.T) {
		s := newSynth()
		s.skippedAttempt("r1", "e1", 1)
		s.add(s.fact("source_unknown", "UNCLASSIFIED", "", "e-ghost", 0))
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if !ep.Excluded || ep.Boundary.Proven || ep.Boundary.Reason != "NO_CALL_COVERAGE_UNPROVEN" {
			t.Fatalf("a fact that cannot be attributed to any admitted round cannot be proven NOT to concern this one: %+v", ep)
		}
		// The same fact naming ANOTHER admitted round bears on that round.
		s = newSynth()
		s.skippedAttempt("r1", "e1", 1)
		s.skippedAttempt("r2", "e2", 2)
		s.add(s.fact("source_unknown", "UNCLASSIFIED", "", "e2", 0))
		sel := mustSelect(t, s.dataset())
		// The count guard is not decoration: without it a regression that drops
		// an episode, or attributes the two under other event ids, makes this
		// loop run zero or one matching iteration and the subtest passes with
		// nothing asserted. The sibling loops in this file carry the same
		// guard; this one did not.
		if len(sel.Episodes) != 2 {
			t.Fatalf("both rounds must be selected for this claim to mean anything: %+v", sel.Episodes)
		}
		seen := map[string]bool{}
		for _, ep := range sel.Episodes {
			seen[ep.Episode.EventID] = true
			switch ep.Episode.EventID {
			case "e1":
				if ep.Excluded {
					t.Fatalf("e1 is untouched by a fact attributed to e2: %+v", ep)
				}
			case "e2":
				if !ep.Excluded || ep.Boundary.Reason != "NO_CALL_COVERAGE_UNPROVEN" {
					t.Fatalf("%+v", ep)
				}
			}
		}
		if !seen["e1"] || !seen["e2"] {
			t.Fatalf("both arms must be reached; the episodes were attributed as %v", seen)
		}
	})
}

// TestSourceRoundClaimsAreDerivedFromTheDataset pins that a claim is
// produced only from a dataset and the factset it derives, and that two
// sessions' claims on one public round reconcile as a conflict.
func TestSourceRoundClaimsAreDerivedFromTheDataset(t *testing.T) {
	ds, ep, fs := selectedCase(t, nil, nil)
	claim, err := p4offline.ClaimSourceRound(ds, fs)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Episode != ep.Episode || claim.Attempt != fs.Attempt || claim.FactsetDigest != fs.Digest {
		t.Fatalf("%+v", claim)
	}
	_, _, other := selectedCase(t, func(e *predictioneval.SourceDecisionEnvelope) { e.Balance = ptrI64(2000) }, nil)
	if _, err := p4offline.ClaimSourceRound(ds, other); !errors.Is(err, p4offline.ErrFactsetNotDerived) {
		t.Fatalf("a factset the dataset does not derive claims nothing: %v", err)
	}
	if _, err := p4offline.ClaimSourceRound(predictioneval.SourceDataset{}, fs); !errors.Is(err, p4offline.ErrEpisodeNotSelected) {
		t.Fatalf("got %v", err)
	}
	s := newSynth()
	s.session = "p4-synth-session-b"
	s.source.CollectorSessionID = s.session
	s.placedAttempt("r1", "e1", 1)
	dsB := s.dataset()
	fsB, err := p4offline.BuildCommonFactset(dsB, singleEpisode(t, mustSelect(t, dsB)).Episode)
	if err != nil {
		t.Fatal(err)
	}
	claimB, err := p4offline.ClaimSourceRound(dsB, fsB)
	if err != nil {
		t.Fatal(err)
	}
	reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{claim, claimB})
	if len(reg.Entries) != 1 || reg.Entries[0].Status != "CONFLICT" || reg.Entries[0].Canonical != nil {
		t.Fatalf("two sessions' claims on one public round are a conflict: %+v", reg.Entries)
	}
	twice := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{claim, claim})
	if len(twice.Entries) != 1 || twice.Entries[0].Status != "DEDUPLICATED_IDENTICAL" {
		t.Fatalf("%+v", twice.Entries)
	}
}

// TestDuplicateOrReusedSourceRoundsAreNotEvidenceTwice pins seam 3.
func TestDuplicateOrReusedSourceRoundsAreNotEvidenceTwice(t *testing.T) {
	t.Run("a re-admitted incarnation is superseded by the earlier one", func(t *testing.T) {
		s := newSynth()
		s.skippedAttempt("r1", "e1", 1)
		s.skippedAttempt("r1-readmitted", "e1", 2)
		sel := mustSelect(t, s.dataset())
		if len(sel.Episodes) != 2 {
			t.Fatalf("%+v", sel.Episodes)
		}
		var first, second p4offline.EpisodeSelection
		for _, ep := range sel.Episodes {
			if ep.Episode.RoundIncarnationID == "r1" {
				first = ep
			} else {
				second = ep
			}
		}
		if first.Excluded {
			t.Fatalf("the earliest incarnation is the round's opportunity: %v", first.ExclusionReasons)
		}
		if !second.Excluded || !containsString(second.ExclusionReasons, "SUPERSEDED_BY_EARLIER_INCARNATION") {
			t.Fatalf("%+v", second)
		}
		if !containsString(sel.DuplicateSourceRounds, "e1") {
			t.Fatalf("e1 must be reported as reused: %v", sel.DuplicateSourceRounds)
		}
	})
	t.Run("an unusable earliest incarnation excludes the later one too", func(t *testing.T) {
		s := newSynth()
		s.due("r1", "e1", 1)
		s.terminal("r1", "e1", 1, predictioneval.PhaseAutoSkipped, "BELOW_MINIMUM_POINTS", nil)
		s.skippedAttempt("r1-readmitted", "e1", 2)
		sel := mustSelect(t, s.dataset())
		if !sel.SessionAdmitted || len(sel.Episodes) != 2 {
			t.Fatalf("both incarnations must be reported: %+v", sel)
		}
		for _, ep := range sel.Episodes {
			if !ep.Excluded {
				t.Fatalf("no incarnation of a reused round may substitute for the first: %+v", ep)
			}
		}
	})
	t.Run("an episode without a public round identity is not a source round", func(t *testing.T) {
		s := newSynth()
		s.skippedAttempt("r1", "", 1)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if !ep.Excluded || !containsString(ep.ExclusionReasons, "SOURCE_ROUND_IDENTITY_MISSING") {
			t.Fatalf("%+v", ep)
		}
	})

	t.Run("global reconciliation", func(t *testing.T) {
		epA := p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "s-a", PoolInstanceID: "p", RoundIncarnationID: "r", EventID: "e1"}
		epB := epA
		epB.CollectorSessionID = "s-b"
		keyA := predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: "s-a", PoolInstanceID: "p", AttemptID: 1}
		keyB := keyA
		keyB.CollectorSessionID = "s-b"
		identical := []p4offline.SourceRoundClaim{
			{Episode: epA, Attempt: keyA, FactsetDigest: "d1"},
			{Episode: epA, Attempt: keyA, FactsetDigest: "d1"},
			{Episode: p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "s-a", PoolInstanceID: "p", RoundIncarnationID: "r2", EventID: "e2"}, Attempt: keyA, FactsetDigest: "d2"},
		}
		reg := p4offline.ReconcileSourceRounds(identical)
		if reg.Version != p4offline.SourceRoundRegistryVersion || len(reg.Entries) != 2 {
			t.Fatalf("%+v", reg)
		}
		if reg.Entries[0].EventID != "e1" || reg.Entries[0].Status != "DEDUPLICATED_IDENTICAL" || reg.Entries[0].Canonical == nil {
			t.Fatalf("two identical claims collapse to one canonical: %+v", reg.Entries[0])
		}
		if reg.Entries[1].EventID != "e2" || reg.Entries[1].Status != "UNIQUE" || reg.Entries[1].Canonical == nil {
			t.Fatalf("%+v", reg.Entries[1])
		}
		conflict := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{
			{Episode: epA, Attempt: keyA, FactsetDigest: "d1"},
			{Episode: epB, Attempt: keyB, FactsetDigest: "d9"},
		})
		if len(conflict.Entries) != 1 || conflict.Entries[0].Status != "CONFLICT" || conflict.Entries[0].Canonical != nil {
			t.Fatalf("two different claims on one round are a conflict with no canonical: %+v", conflict.Entries)
		}
		// Order-independent, deterministic identity.
		reversed := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{identical[2], identical[1], identical[0]})
		if reversed.Digest != reg.Digest || reg.Digest == "" {
			t.Fatalf("registry digest must not depend on claim order: %q vs %q", reversed.Digest, reg.Digest)
		}
		if conflict.Digest == reg.Digest {
			t.Fatal("different registries share a digest")
		}
		empty := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{{Episode: epA, Attempt: keyA}})
		if len(empty.Entries) != 1 || empty.Entries[0].Status != "INVALID" {
			t.Fatalf("a claim without a factset digest is invalid: %+v", empty.Entries)
		}
	})
}

// TestPostCutoffFactsDoNotChangeTheSelectedOpportunity pins the causal cut at
// seam 1: facts appended after the terminal envelope — the round's settlement,
// more placement traffic — leave the selected attempt, its cutoff and its
// P2 input digest untouched.
func TestPostCutoffFactsDoNotChangeTheSelectedOpportunity(t *testing.T) {
	build := func(extra bool) p4offline.EpisodeSelection {
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		if extra {
			s.userTerminal("r1", "e1", "WON", 50, 120)
			s.add(s.fact("channel_event", "ROUND_UPDATED", "r1", "e1", 0))
		}
		return singleEpisode(t, mustSelect(t, s.dataset()))
	}
	before, after := build(false), build(true)
	if before.Excluded || after.Excluded {
		t.Fatalf("%v / %v", before.ExclusionReasons, after.ExclusionReasons)
	}
	if before.Attempt.Key != after.Attempt.Key || before.Boundary.CutoffPosition != after.Boundary.CutoffPosition ||
		before.Attempt.CommonInputDigest != after.Attempt.CommonInputDigest ||
		len(before.Attempt.CommonInputSlice) != len(after.Attempt.CommonInputSlice) {
		t.Fatalf("post-cutoff facts changed the selected opportunity:\n%+v\n%+v", before.Boundary, after.Boundary)
	}
}

// TestAutomaticCallIdentityIsScopedToItsPool pins the identity an automatic
// attempt id carries: the counter restarts in every pool, so attempt 1 names
// one attempt per pool, session and epoch — never across pools. A return in
// one pool is not paired with a start in another, and another pool's
// automatic call on the same public round is an intervention on this
// episode, not its own call.
func TestAutomaticCallIdentityIsScopedToItsPool(t *testing.T) {
	t.Run("an orphan return in another pool is not paired with this pool's start", func(t *testing.T) {
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		s.pool = "pool-b"
		s.due("r2", "e2", 1)
		s.terminal("r2", "e2", 1, predictioneval.PhaseAutoDecided, "OK", synthPlacedEnvelope())
		s.placement("r2", "e2", 1, predictioneval.PhaseCallReturned, 50, 0, "OK", "NONE")
		sel := mustSelect(t, s.dataset())
		var sawA, sawB bool
		for _, ep := range sel.Episodes {
			switch ep.Episode.EventID {
			case "e1":
				sawA = true
				if ep.Excluded {
					t.Fatalf("pool-a's complete attempt is untouched by pool-b's orphan: %+v", ep)
				}
			case "e2":
				sawB = true
				if !ep.Excluded || ep.Boundary.NoCallCoverage.Proven || ep.Boundary.Reason != "NO_CALL_COVERAGE_UNPROVEN" ||
					!containsString(ep.Boundary.NoCallCoverage.Reasons, "ORPHAN_CALL_RETURNED") {
					t.Fatalf("pool-b's return without a start is an orphan whatever pool-a recorded under the same attempt id: %+v", ep)
				}
			}
		}
		if !sawA || !sawB {
			t.Fatalf("expected episodes e1 and e2: %+v", sel.Episodes)
		}
	})
	t.Run("another pool's automatic call on the same round is not this episode's own", func(t *testing.T) {
		s := newSynth()
		s.skippedAttempt("r1", "e1", 1)
		s.pool = "pool-b"
		s.placement("r1b", "e1", 1, predictioneval.PhaseCallStarted, 50, 0, "OK", "NONE")
		s.placement("r1b", "e1", 1, predictioneval.PhaseCallReturned, 50, 0, "OK", "NONE")
		sel := mustSelect(t, s.dataset())
		for _, ep := range sel.Episodes {
			if ep.Episode.EventID == "e1" && ep.Episode.PoolInstanceID == "pool-a" {
				if !ep.Excluded || !containsString(ep.ExclusionReasons, "UNATTRIBUTED_INTERVENTION") {
					t.Fatalf("a call by another pool's attempt 1 is an intervention on this episode, not its own call: %+v", ep)
				}
				return
			}
		}
		t.Fatalf("no pool-a e1 episode: %+v", sel.Episodes)
	})
	t.Run("the same pool's own attempt still pairs and attributes as before", func(t *testing.T) {
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if ep.Excluded || !ep.Boundary.Proven || !ep.Boundary.NoCallCoverage.Proven || !ep.Boundary.EarliestCallPresent {
			t.Fatalf("%+v", ep)
		}
	})
}

// framing re-frames registry parts the way the package's canonical framing
// does: every part length-prefixed by eight big-endian bytes, counts and
// booleans as their decimal and "true"/"false" strings. It exists so a
// test can present an entry list reconciliation would never produce under
// a digest that MATCHES it; the test checks first that it agrees with the
// package on an honest registry, whose digest the golden test pins to the
// independent oracle.
type framing struct{ buf []byte }

func (f *framing) str(s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	f.buf = append(append(f.buf, n[:]...), s...)
}

// restampRegistry returns reg under the digest the package's framing yields
// for its entries as they are, so a hand-altered entry list can be presented
// under a digest that matches it.
func restampRegistry(reg p4offline.SourceRoundRegistry) p4offline.SourceRoundRegistry {
	claimKey := func(c p4offline.SourceRoundClaim) string {
		var k framing
		k.str(c.Episode.String())
		k.str(strconv.FormatInt(c.Attempt.CollectorEpoch, 10))
		k.str(c.Attempt.CollectorSessionID)
		k.str(c.Attempt.PoolInstanceID)
		k.str(strconv.FormatUint(c.Attempt.AttemptID, 10))
		k.str(c.FactsetDigest)
		return hex.EncodeToString(k.buf)
	}
	var f framing
	f.str(p4offline.SourceRoundRegistryVersion)
	f.str(strconv.Itoa(len(reg.Entries)))
	for _, e := range reg.Entries {
		f.str(e.EventID)
		f.str(string(e.Status))
		f.str(strconv.Itoa(len(e.Claims)))
		for _, c := range e.Claims {
			f.str(claimKey(c))
		}
		f.str(strconv.FormatBool(e.Canonical != nil))
	}
	reg.Digest = digestOf(f.buf)
	return reg
}

// TestSourceRoundRegistryVerifierAdmitsOnlyReconciliationsOwnOutput pins the
// verifier as a FIXED POINT of reconciliation, independently of the digest:
// every shape below carries a digest that matches its entries, so only the
// re-reconciliation — the same entries, in the same order, with the same
// statuses, claims and canonical claims — stands between it and acceptance.
// What reconciliation produces is admitted, whatever it holds; what it never
// produces is refused, however its digest was made.
func TestSourceRoundRegistryVerifierAdmitsOnlyReconciliationsOwnOutput(t *testing.T) {
	ep := func(session, incarnation, event string) p4offline.EpisodeIdentity {
		return p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: session, PoolInstanceID: "p", RoundIncarnationID: incarnation, EventID: event}
	}
	key := func(session string) predictioneval.AttemptKey {
		return predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: session, PoolInstanceID: "p", AttemptID: 1}
	}
	a1 := p4offline.SourceRoundClaim{Episode: ep("s-a", "r1", "e1"), Attempt: key("s-a"), FactsetDigest: "d1"}
	b1 := p4offline.SourceRoundClaim{Episode: ep("s-b", "r1", "e1"), Attempt: key("s-b"), FactsetDigest: "d9"}
	a2 := p4offline.SourceRoundClaim{Episode: ep("s-a", "r2", "e2"), Attempt: key("s-a"), FactsetDigest: "d2"}
	undigested := p4offline.SourceRoundClaim{Episode: ep("s-a", "r3", "e3"), Attempt: key("s-a")}
	unrounded := p4offline.SourceRoundClaim{Episode: ep("s-a", "", ""), Attempt: key("s-a"), FactsetDigest: "d4"}
	honest := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{a1, a1, a2})
	conflict := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{a1, b1, a2})
	if conflict.Entries[0].Status != p4offline.SourceRoundConflict || len(conflict.Entries[0].Claims) != 2 {
		t.Fatalf("fixture: %+v", conflict.Entries)
	}

	admitted := map[string]p4offline.SourceRoundRegistry{
		"the honest registry":                         honest,
		"the empty registry":                          p4offline.ReconcileSourceRounds(nil),
		"a conflict":                                  conflict,
		"an honest INVALID entry":                     p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{a1, undigested}),
		"a round named by a valid and an INVALID one": p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{a1, {Episode: ep("s-a", "r1", "e1"), Attempt: key("s-a")}}),
	}
	for name, reg := range admitted {
		// Every status and both canonical states pass through here, so the
		// framing is proved against the package's on each before it is used
		// to re-stamp anything.
		if restampRegistry(reg).Digest != reg.Digest {
			t.Fatalf("%s: the test's framing disagrees with the package's: %q vs %q", name, restampRegistry(reg).Digest, reg.Digest)
		}
		if err := p4offline.VerifySourceRoundRegistry(reg); err != nil {
			t.Fatalf("%s is reconciliation's own output: %v", name, err)
		}
	}

	refused := map[string]func(p4offline.SourceRoundRegistry) p4offline.SourceRoundRegistry{
		"entries reordered": func(reg p4offline.SourceRoundRegistry) p4offline.SourceRoundRegistry {
			reg.Entries = []p4offline.SourceRoundEntry{reg.Entries[1], reg.Entries[0]}
			return reg
		},
		"an entry with no claims": func(reg p4offline.SourceRoundRegistry) p4offline.SourceRoundRegistry {
			reg.Entries = append(reg.Entries, p4offline.SourceRoundEntry{EventID: "e3", Status: p4offline.SourceRoundUnique})
			return reg
		},
		"UNIQUE relabelled DEDUPLICATED_IDENTICAL": func(reg p4offline.SourceRoundRegistry) p4offline.SourceRoundRegistry {
			reg.Entries[1].Status = p4offline.SourceRoundDeduplicatedIdentical
			return reg
		},
		"DEDUPLICATED_IDENTICAL relabelled UNIQUE": func(reg p4offline.SourceRoundRegistry) p4offline.SourceRoundRegistry {
			reg.Entries[0].Status = p4offline.SourceRoundUnique
			return reg
		},
		"an entry with a valid claim relabelled INVALID": func(reg p4offline.SourceRoundRegistry) p4offline.SourceRoundRegistry {
			reg.Entries[1].Status = p4offline.SourceRoundInvalid
			reg.Entries[1].Canonical = nil
			return reg
		},
		"a claim filed under another round's entry": func(reg p4offline.SourceRoundRegistry) p4offline.SourceRoundRegistry {
			reg.Entries[0].Claims = []p4offline.SourceRoundClaim{a1}
			reg.Entries[1].Claims = []p4offline.SourceRoundClaim{a2, a1}
			return reg
		},
		"an INVALID entry filed under a round its claim does not name": func(reg p4offline.SourceRoundRegistry) p4offline.SourceRoundRegistry {
			reg.Entries = append(reg.Entries, p4offline.SourceRoundEntry{EventID: "e4", Status: p4offline.SourceRoundInvalid, Claims: []p4offline.SourceRoundClaim{unrounded}})
			return reg
		},
	}
	for name, tamper := range refused {
		t.Run(name, func(t *testing.T) {
			reg := restampRegistry(tamper(cloneRegistry(honest)))
			if err := p4offline.VerifySourceRoundRegistry(reg); !errors.Is(err, p4offline.ErrSourceRoundRegistry) {
				t.Fatalf("got %v for %+v", err, reg.Entries)
			}
		})
	}
	t.Run("a CONFLICT entry given a canonical", func(t *testing.T) {
		reg := cloneRegistry(conflict)
		c := a1
		reg.Entries[0].Canonical = &c
		if err := p4offline.VerifySourceRoundRegistry(restampRegistry(reg)); !errors.Is(err, p4offline.ErrSourceRoundRegistry) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("claims reordered inside a CONFLICT entry", func(t *testing.T) {
		reg := cloneRegistry(conflict)
		cs := reg.Entries[0].Claims
		reg.Entries[0].Claims = []p4offline.SourceRoundClaim{cs[1], cs[0]}
		if err := p4offline.VerifySourceRoundRegistry(restampRegistry(reg)); !errors.Is(err, p4offline.ErrSourceRoundRegistry) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("and this position stays BARE, which is a contract and not a habit", func(t *testing.T) {
		// THE FIFTH REFUSAL POSITION. The other four name themselves, so a
		// caller distinguishes this one by the ABSENCE of a second line -- and
		// an absence nothing asserts is a convention that collapses the moment
		// a later round leaves some other position bare. A review lane raised
		// exactly that. It stays bare on purpose: the sentinel's own sentence,
		// "does not re-derive from its entries", IS this position's diagnosis,
		// so joining a second line would repeat it. This test already drives
		// the position; the assertion is one line, and it is what makes "bare
		// means re-reconciliation" checkable.
		reg := cloneRegistry(conflict)
		c := a1
		reg.Entries[0].Canonical = &c
		err := p4offline.VerifySourceRoundRegistry(restampRegistry(reg))
		if err == nil || err.Error() != p4offline.ErrSourceRoundRegistry.Error() {
			t.Fatalf("the re-reconciliation is the one position that answers with the sentinel "+
				"alone, because the sentinel already says what went wrong: %v", err)
		}
	})
}

// cloneRegistry copies a registry deep enough for a test to tamper with one
// entry without touching the fixture.
func cloneRegistry(reg p4offline.SourceRoundRegistry) p4offline.SourceRoundRegistry {
	out := reg
	out.Entries = make([]p4offline.SourceRoundEntry, len(reg.Entries))
	for i, e := range reg.Entries {
		e.Claims = append([]p4offline.SourceRoundClaim(nil), e.Claims...)
		if e.Canonical != nil {
			c := *e.Canonical
			e.Canonical = &c
		}
		out.Entries[i] = e
	}
	return out
}

// TestOneCallStartPairsAtMostOneReturn pins the producer's one-start/one-return
// shape. A recorded start settles ONE return: a second return under the same
// identity has no start of its own, and the start it lacks could precede the
// cutoff, so coverage is not proven. The check is on cardinality, not identity
// — the full-identity scoping stays exactly as it is.
func TestOneCallStartPairsAtMostOneReturn(t *testing.T) {
	t.Run("a second automatic return is orphaned when one start was recorded", func(t *testing.T) {
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		s.placement("r1", "e1", 1, predictioneval.PhaseCallReturned, 50, 0, "OK", "NONE")
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if ep.Boundary.NoCallCoverage.Proven ||
			!containsString(ep.Boundary.NoCallCoverage.Reasons, "ORPHAN_CALL_RETURNED") {
			t.Fatalf("one start cannot settle two returns: %+v", ep.Boundary)
		}
		if !ep.Excluded || !containsString(ep.ExclusionReasons, "BOUNDARY_NOT_PROVEN") {
			t.Fatalf("an unproven boundary excludes the episode: %+v", ep)
		}
	})
	t.Run("a second non-automatic return is orphaned when one start was recorded", func(t *testing.T) {
		s := newSynth()
		s.skippedAttempt("r1", "e1", 1)
		s.add(s.fact(predictioneval.KindPlacement, predictioneval.PhaseCallStarted, "r1", "e1", 0))
		s.add(s.fact(predictioneval.KindPlacement, predictioneval.PhaseCallReturned, "r1", "e1", 0))
		s.add(s.fact(predictioneval.KindPlacement, predictioneval.PhaseCallReturned, "r1", "e1", 0))
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if ep.Boundary.NoCallCoverage.Proven ||
			!containsString(ep.Boundary.NoCallCoverage.Reasons, "ORPHAN_CALL_RETURNED") {
			t.Fatalf("the non-automatic path settles one return per start too: %+v", ep.Boundary)
		}
	})
	t.Run("a return with no start at all is still orphaned", func(t *testing.T) {
		s := newSynth()
		s.skippedAttempt("r1", "e1", 1)
		s.placement("r1", "e1", 1, predictioneval.PhaseCallReturned, 50, 0, "OK", "NONE")
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if ep.Boundary.NoCallCoverage.Proven ||
			!containsString(ep.Boundary.NoCallCoverage.Reasons, "ORPHAN_CALL_RETURNED") {
			t.Fatalf("%+v", ep.Boundary)
		}
	})
	t.Run("one start and one return still pair", func(t *testing.T) {
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if ep.Excluded || !ep.Boundary.Proven || !ep.Boundary.NoCallCoverage.Proven {
			t.Fatalf("the honest one-start/one-return pair is untouched: %+v", ep)
		}
	})
	t.Run("a second start for the same attempt cannot settle a second return", func(t *testing.T) {
		// The producer writes ONE start per placement call: KindPlacement is
		// declared as "the single Twitch placement call - one fact immediately
		// before it and one immediately after. Never wraps, retries or alters
		// it", and both call sites emit exactly that pair with no retry. Two
		// starts carrying the SAME attempt discriminator on the same round are
		// therefore a shape it cannot emit - and banking the second one would
		// settle a return that has no start of its own, turning an episode the
		// evidence excludes back into a scorable one.
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		s.placement("r1", "e1", 1, predictioneval.PhaseCallStarted, 50, 0, "OK", "NONE")
		s.placement("r1", "e1", 1, predictioneval.PhaseCallReturned, 50, 0, "OK", "NONE")
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if ep.Boundary.NoCallCoverage.Proven {
			t.Fatalf("a duplicate start must not restore a proven coverage: %+v", ep.Boundary)
		}
		if !containsString(ep.Boundary.NoCallCoverage.Reasons, "UNCLASSIFIED_FACT_ON_ROUND") {
			t.Fatalf("the duplicate start is the contradicted fact: %+v", ep.Boundary)
		}
		if !ep.Excluded || !containsString(ep.ExclusionReasons, "BOUNDARY_NOT_PROVEN") {
			t.Fatalf("an unproven boundary excludes the episode: %+v", ep)
		}
	})
	t.Run("two attempts on one round do not share a start", func(t *testing.T) {
		// The round half of the slot is identical here, so only the native
		// attempt identity can keep these apart: attempt 1 holds a start
		// nothing returns, attempt 2 returns with no start of its own.
		s := newSynth()
		s.due("r1", "e1", 1)
		s.terminal("r1", "e1", 1, predictioneval.PhaseAutoDecided, "OK", synthPlacedEnvelope())
		s.placement("r1", "e1", 1, predictioneval.PhaseCallStarted, 50, 0, "OK", "NONE")
		s.due("r1", "e1", 2)
		s.terminal("r1", "e1", 2, predictioneval.PhaseAutoDecided, "OK", synthPlacedEnvelope())
		s.placement("r1", "e1", 2, predictioneval.PhaseCallReturned, 50, 0, "OK", "NONE")
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if ep.Boundary.NoCallCoverage.Proven ||
			!containsString(ep.Boundary.NoCallCoverage.Reasons, "ORPHAN_CALL_RETURNED") {
			t.Fatalf("one attempt's start cannot settle another attempt's return: %+v", ep.Boundary)
		}
	})
	t.Run("each pool's own start settles only its own return", func(t *testing.T) {
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		s.pool = "pool-b"
		s.placedAttempt("r2", "e2", 1)
		sel := mustSelect(t, s.dataset())
		if len(sel.Episodes) != 2 {
			t.Fatalf("want two episodes: %+v", sel.Episodes)
		}
		for _, ep := range sel.Episodes {
			if ep.Excluded || !ep.Boundary.NoCallCoverage.Proven {
				t.Fatalf("one pool's pair does not consume another pool's start: %+v", ep)
			}
		}
	})
}

// TestUnreadableOrUnsupportedPlacementLeavesCoverageUnproven pins the second
// half of the no-call coverage argument. A placement fact the reader could not
// decode, or one claiming a payload version this package does not support, is
// a fact about the call record that cannot be read: its phase could be
// CALL_RETURNED and the start it names could precede the cutoff. Coverage is
// therefore not proven, exactly as it is not proven for any other unreadable
// fact on the round. The fact still positions itself as an ambiguous call.
func TestUnreadableOrUnsupportedPlacementLeavesCoverageUnproven(t *testing.T) {
	// afterCutoff builds a skipping attempt and then one placement fact at a
	// LATER position, so C < F holds and coverage is the only thing left that
	// can refuse the boundary.
	afterCutoff := func(t *testing.T, mutate func(*predictioneval.SourceRecord)) p4episode {
		t.Helper()
		s := newSynth()
		s.skippedAttempt("r1", "e1", 1)
		r := s.fact(predictioneval.KindPlacement, predictioneval.PhaseCallStarted, "r1", "e1", 1)
		mutate(&r)
		s.add(r)
		return singleEpisode(t, mustSelect(t, s.dataset()))
	}

	t.Run("an undecodable placement fact after the cutoff leaves coverage unproven", func(t *testing.T) {
		ep := afterCutoff(t, func(r *predictioneval.SourceRecord) { r.PayloadUndecodable = true })
		if ep.Boundary.NoCallCoverage.Proven ||
			!containsString(ep.Boundary.NoCallCoverage.Reasons, "UNDECODABLE_FACT_ON_ROUND") {
			t.Fatalf("an unreadable placement fact cannot prove there was no call: %+v", ep.Boundary)
		}
		if ep.Boundary.Reason != "NO_CALL_COVERAGE_UNPROVEN" || !ep.Excluded {
			t.Fatalf("the boundary must fail closed on unproven coverage: %+v", ep)
		}
		if ep.Boundary.EarliestCallKind != p4offline.CallKindAmbiguous || !ep.Boundary.EarliestCallPresent {
			t.Fatalf("the fact still positions itself as an ambiguous call: %+v", ep.Boundary)
		}
	})
	t.Run("a placement fact claiming an unsupported payload version leaves coverage unproven", func(t *testing.T) {
		ep := afterCutoff(t, func(r *predictioneval.SourceRecord) {
			r.PayloadVersion = predictioneval.SupportedPayloadVersion + 1
		})
		if ep.Boundary.NoCallCoverage.Proven ||
			!containsString(ep.Boundary.NoCallCoverage.Reasons, "UNDECODABLE_FACT_ON_ROUND") {
			t.Fatalf("a placement fact in an unsupported version is not readable evidence: %+v", ep.Boundary)
		}
		if ep.Boundary.Reason != "NO_CALL_COVERAGE_UNPROVEN" || !ep.Excluded {
			t.Fatalf("the boundary must fail closed on unproven coverage: %+v", ep)
		}
	})
	t.Run("a supported, decodable placement fact still proves coverage", func(t *testing.T) {
		// The control builds its own SETTLED pair rather than reusing the
		// helper's lone CALL_STARTED. A start no return ever spends is
		// contradicted evidence in an admitted COMPLETE session, so the
		// helper's shape is no longer a readable-call control — it is the
		// unspent-start case, which has its own test.
		s := newSynth()
		s.due("r1", "e1", 1)
		env := synthPlacedEnvelope()
		s.terminal("r1", "e1", 1, predictioneval.PhaseAutoDecided, "OK", env)
		s.call("r1", "e1", 1, *env.FinalAmount, *env.ChoiceIndex)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		// All three predicates, restored: an earlier edit dropped the boundary
		// and exclusion halves while leaving the message claiming them, which
		// is the weaker-assertion-with-the-stronger-message shape this suite
		// exists to refuse.
		if !ep.Boundary.NoCallCoverage.Proven || !ep.Boundary.Proven || ep.Excluded {
			t.Fatalf("a readable, settled call after the cutoff is exactly what the boundary admits: %+v", ep)
		}
	})
}

// TestEarliestAutomaticRowWithoutAnIdentifierIsNotSubstituted pins seam 2's
// no-later-substitution rule at the one shape that used to escape it: the
// EARLIEST raw automatic row carries no usable attempt id. It is the episode's
// first opportunity and it is unusable; a later, fully materialized attempt
// must not stand in for it.
func TestEarliestAutomaticRowWithoutAnIdentifierIsNotSubstituted(t *testing.T) {
	t.Run("a later valid attempt does not substitute for an identifier-less first row", func(t *testing.T) {
		s := newSynth()
		s.add(s.fact(predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, "r1", "e1", 0))
		s.placedAttempt("r1", "e1", 2)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if !ep.Excluded || !containsString(ep.ExclusionReasons, "FIRST_OPPORTUNITY_UNUSABLE") {
			t.Fatalf("the earliest automatic row is the first opportunity and it is unusable: %+v", ep)
		}
		if ep.FirstOpportunityUsable || ep.Attempt != nil {
			t.Fatalf("no later attempt may stand in for it: %+v", ep.Attempt)
		}
		if ep.Quality.Quality == p4offline.QualityPrimaryScorable {
			t.Fatalf("a substituted episode must not reach the primary cohort: %+v", ep.Quality)
		}
	})
	t.Run("a non-positive attempt id is identifier-less too", func(t *testing.T) {
		s := newSynth()
		r := s.fact(predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, "r1", "e1", 0)
		r.Payload.Counters = map[string]int64{predictioneval.CounterAutoAttemptID: -1}
		s.add(r)
		s.placedAttempt("r1", "e1", 2)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if !ep.Excluded || !containsString(ep.ExclusionReasons, "FIRST_OPPORTUNITY_UNUSABLE") {
			t.Fatalf("a non-positive id names no attempt: %+v", ep)
		}
	})
	t.Run("an episode with no automatic row at all is not an opportunity", func(t *testing.T) {
		s := newSynth()
		s.userTerminal("r1", "e1", "OK", 0, 0)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		// The fixture carries no call, so this reason stands alone.
		if !ep.Excluded || !containsString(ep.ExclusionReasons, "NO_AUTOMATIC_OPPORTUNITY") {
			t.Fatalf("no automatic row at all is still NO_AUTOMATIC_OPPORTUNITY: %+v", ep)
		}
		if containsString(ep.ExclusionReasons, "FIRST_OPPORTUNITY_UNUSABLE") {
			t.Fatalf("the two halves of the partition are distinct: %+v", ep.ExclusionReasons)
		}
	})
	t.Run("an unusable earliest incarnation still supersedes the later one", func(t *testing.T) {
		// Seam 3 orders supersession by FirstOpportunityPosition. An episode
		// whose earliest automatic row names no attempt is still the round's
		// EARLIEST incarnation, so it must keep its place in that order and
		// the later incarnation must stay superseded — an unusable first
		// opportunity may not hand the round to a usable later one.
		s := newSynth()
		s.userTerminal("r1", "e1", "OK", 0, 0)
		s.skippedAttempt("r2", "e1", 1)
		s.add(s.fact(predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, "r1", "e1", 0))
		sel := mustSelect(t, s.dataset())
		var later *p4episode
		for i := range sel.Episodes {
			if sel.Episodes[i].Episode.RoundIncarnationID == "r2" {
				later = &sel.Episodes[i]
			}
		}
		if later == nil {
			t.Fatalf("no r2 episode: %+v", sel.Episodes)
		}
		if !later.Excluded || !containsString(later.ExclusionReasons, "SUPERSEDED_BY_EARLIER_INCARNATION") {
			t.Fatalf("the later incarnation must stay superseded: %+v", later)
		}
		if later.Quality.Quality == p4offline.QualityPrimaryScorable {
			t.Fatalf("a superseded incarnation must not reach the primary cohort: %+v", later.Quality)
		}
	})
}

// episodeOnIncarnation picks one episode out of a selection by its incarnation.
func episodeOnIncarnation(t *testing.T, sel p4offline.EvidenceSelection, incarnation string) p4episode {
	t.Helper()
	for i := range sel.Episodes {
		if sel.Episodes[i].Episode.RoundIncarnationID == incarnation {
			return sel.Episodes[i]
		}
	}
	t.Fatalf("no episode on incarnation %q: %+v", incarnation, sel.Episodes)
	return p4episode{}
}

// TestAStartCannotValidateAReturnFromAnotherRound pins the round half of call
// pairing. The attempt counter is unique within a pool, so the identity alone
// already names the attempt and the round is not there to disambiguate it: it
// is there to refuse. A start and a return that agree on the attempt but
// disagree about the incarnation or the public round are contradicted evidence,
// and the return is left with no start of its own — a start that could precede
// its own cutoff. The native AttemptKey identity is unchanged; this is an
// association check layered on top of it, not a replacement for it.
func TestAStartCannotValidateAReturnFromAnotherRound(t *testing.T) {
	t.Run("another round's start does not settle this round's return", func(t *testing.T) {
		s := newSynth()
		// Round A holds an UNSPENT start: a CALL_STARTED with no return of its
		// own. One-to-one consumption alone therefore leaves it available, and
		// only the round association can stop round B from spending it.
		s.due("rA", "eA", 1)
		s.terminal("rA", "eA", 1, predictioneval.PhaseAutoDecided, "OK", synthPlacedEnvelope())
		s.placement("rA", "eA", 1, predictioneval.PhaseCallStarted, 50, 0, "OK", "NONE")
		// Round B in the SAME pool: its own usable attempt, plus a start-less
		// return that reuses attempt id 1.
		s.skippedAttempt("rB", "eB", 2)
		s.placement("rB", "eB", 1, predictioneval.PhaseCallReturned, 50, 0, "OK", "NONE")
		var roundB *p4episode
		sel := mustSelect(t, s.dataset())
		for i := range sel.Episodes {
			if sel.Episodes[i].Episode.RoundIncarnationID == "rB" {
				roundB = &sel.Episodes[i]
			}
		}
		if roundB == nil {
			t.Fatalf("no rB episode: %+v", sel.Episodes)
		}
		if roundB.Boundary.NoCallCoverage.Proven ||
			!containsString(roundB.Boundary.NoCallCoverage.Reasons, "ORPHAN_CALL_RETURNED") {
			t.Fatalf("a return on rB has no start of its own: %+v", roundB.Boundary)
		}
	})
	t.Run("only the public event differs and the start still settles nothing", func(t *testing.T) {
		// Both facts share the pool, the incarnation and the attempt id, so the
		// EVENT component of the round half is the only thing left that can
		// keep them apart. One incarnation is one episode whatever public event
		// its facts name, so this is a single episode holding an unspent start
		// and a return the start must not settle.
		s := newSynth()
		s.due("r1", "eA", 1)
		s.terminal("r1", "eA", 1, predictioneval.PhaseAutoDecided, "OK", synthPlacedEnvelope())
		s.placement("r1", "eA", 1, predictioneval.PhaseCallStarted, 50, 0, "OK", "NONE")
		s.placement("r1", "eB", 1, predictioneval.PhaseCallReturned, 50, 0, "OK", "NONE")
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if ep.Boundary.NoCallCoverage.Proven ||
			!containsString(ep.Boundary.NoCallCoverage.Reasons, "ORPHAN_CALL_RETURNED") {
			t.Fatalf("a start recorded on another public event settles nothing here: %+v", ep.Boundary)
		}
	})
	t.Run("only the incarnation differs and the start still settles nothing", func(t *testing.T) {
		// The mirror image: one public event, two incarnations of it, so the
		// INCARNATION component is the only discriminator left.
		s := newSynth()
		s.due("rA", "e1", 1)
		s.terminal("rA", "e1", 1, predictioneval.PhaseAutoDecided, "OK", synthPlacedEnvelope())
		s.placement("rA", "e1", 1, predictioneval.PhaseCallStarted, 50, 0, "OK", "NONE")
		s.skippedAttempt("rB", "e1", 2)
		s.placement("rB", "e1", 1, predictioneval.PhaseCallReturned, 50, 0, "OK", "NONE")
		ep := episodeOnIncarnation(t, mustSelect(t, s.dataset()), "rB")
		if ep.Boundary.NoCallCoverage.Proven ||
			!containsString(ep.Boundary.NoCallCoverage.Reasons, "ORPHAN_CALL_RETURNED") {
			t.Fatalf("a start recorded on another incarnation settles nothing here: %+v", ep.Boundary)
		}
	})
	t.Run("a round's own start still settles its own return", func(t *testing.T) {
		s := newSynth()
		s.placedAttempt("rA", "eA", 1)
		s.placedAttempt("rB", "eB", 2)
		sel := mustSelect(t, s.dataset())
		if len(sel.Episodes) != 2 {
			t.Fatalf("want two episodes: %+v", sel.Episodes)
		}
		for _, ep := range sel.Episodes {
			if ep.Excluded || !ep.Boundary.NoCallCoverage.Proven {
				t.Fatalf("each round's own pair is untouched: %+v", ep)
			}
		}
	})
	t.Run("a foreign round's return no longer spends this round's start", func(t *testing.T) {
		// The direction a crafted dataset gains on: a decoy return naming
		// another incarnation used to consume this round's start and starve
		// the start's OWN return. The orphan belongs to the round that has no
		// start, not to the round that has one.
		s := newSynth()
		s.due("rA", "eA", 1)
		s.terminal("rA", "eA", 1, predictioneval.PhaseAutoDecided, "OK", synthPlacedEnvelope())
		s.placement("rA", "eA", 1, predictioneval.PhaseCallStarted, 50, 0, "OK", "NONE")
		s.skippedAttempt("rB", "eB", 2)
		s.placement("rB", "eB", 1, predictioneval.PhaseCallReturned, 50, 0, "OK", "NONE")
		s.placement("rA", "eA", 1, predictioneval.PhaseCallReturned, 50, 0, "OK", "NONE")
		sel := mustSelect(t, s.dataset())
		var a, b *p4episode
		for i := range sel.Episodes {
			switch sel.Episodes[i].Episode.RoundIncarnationID {
			case "rA":
				a = &sel.Episodes[i]
			case "rB":
				b = &sel.Episodes[i]
			}
		}
		if a == nil || b == nil {
			t.Fatalf("want both episodes: %+v", sel.Episodes)
		}
		if !a.Boundary.NoCallCoverage.Proven {
			t.Fatalf("rA has a matched pair of its own; the decoy must not starve it: %+v", a.Boundary)
		}
		if b.Boundary.NoCallCoverage.Proven ||
			!containsString(b.Boundary.NoCallCoverage.Reasons, "ORPHAN_CALL_RETURNED") {
			t.Fatalf("the orphan belongs to rB, which has no start: %+v", b.Boundary)
		}
	})
	t.Run("a start on another pool and another round settles nothing here", func(t *testing.T) {
		// Same incarnation and public round in both pools, same attempt id:
		// only the pool separates them. (The pool appears in both halves of
		// the slot, so this pins the behaviour rather than which half carries
		// it; the identity half is kept whole because it is the native
		// AttemptKey.)
		s := newSynth()
		s.due("r1", "e1", 1)
		s.terminal("r1", "e1", 1, predictioneval.PhaseAutoDecided, "OK", synthPlacedEnvelope())
		s.placement("r1", "e1", 1, predictioneval.PhaseCallStarted, 50, 0, "OK", "NONE")
		s.pool = "pool-b"
		s.skippedAttempt("r1", "e1", 2)
		s.placement("r1", "e1", 1, predictioneval.PhaseCallReturned, 50, 0, "OK", "NONE")
		sel := mustSelect(t, s.dataset())
		var other *p4episode
		for i := range sel.Episodes {
			if sel.Episodes[i].Episode.PoolInstanceID == "pool-b" {
				other = &sel.Episodes[i]
			}
		}
		if other == nil {
			t.Fatalf("no pool-b episode: %+v", sel.Episodes)
		}
		if other.Boundary.NoCallCoverage.Proven ||
			!containsString(other.Boundary.NoCallCoverage.Reasons, "ORPHAN_CALL_RETURNED") {
			t.Fatalf("another pool's start is not this return's: %+v", other.Boundary)
		}
	})
	t.Run("a start and a return disagreeing on pool and round do not pair", func(t *testing.T) {
		s := newSynth()
		s.due("r1", "e1", 1)
		s.terminal("r1", "e1", 1, predictioneval.PhaseAutoDecided, "OK", synthPlacedEnvelope())
		s.placement("r1", "e1", 1, predictioneval.PhaseCallStarted, 50, 0, "OK", "NONE")
		s.pool = "pool-b"
		s.skippedAttempt("r2", "e2", 1)
		s.placement("r2", "e2", 1, predictioneval.PhaseCallReturned, 50, 0, "OK", "NONE")
		var poolB *p4episode
		sel := mustSelect(t, s.dataset())
		for i := range sel.Episodes {
			if sel.Episodes[i].Episode.PoolInstanceID == "pool-b" {
				poolB = &sel.Episodes[i]
			}
		}
		if poolB == nil || poolB.Boundary.NoCallCoverage.Proven ||
			!containsString(poolB.Boundary.NoCallCoverage.Reasons, "ORPHAN_CALL_RETURNED") {
			t.Fatalf("a start on another pool AND another round settles nothing here: %+v", poolB)
		}
	})
}

// TestUnreadableEvidenceOnTheRoundNeverProvesNoCall covers the remaining shapes
// of the same guarantee: coverage is proven only when every fact matched to the
// episode could actually be read. A placement phase outside the closed
// vocabulary and an unsupported payload version on a non-placement fact are
// both unreadable in that sense, and neither is examined anywhere downstream.
func TestUnreadableEvidenceOnTheRoundNeverProvesNoCall(t *testing.T) {
	t.Run("a placement phase outside the vocabulary does not prove coverage", func(t *testing.T) {
		s := newSynth()
		s.skippedAttempt("r1", "e1", 1)
		r := s.fact(predictioneval.KindPlacement, "CALL_SOMETHING_ELSE", "r1", "e1", 1)
		s.add(r)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if ep.Boundary.NoCallCoverage.Proven {
			t.Fatalf("a phase the producer cannot emit is not readable evidence: %+v", ep.Boundary)
		}
		if !containsString(ep.Boundary.NoCallCoverage.Reasons, "UNCLASSIFIED_FACT_ON_ROUND") {
			t.Fatalf("an unnameable phase is an unclassified fact: %+v", ep.Boundary.NoCallCoverage)
		}
		// The diagnostic position is still reported.
		if !ep.Boundary.EarliestCallPresent || ep.Boundary.EarliestCallKind != p4offline.CallKindAmbiguous {
			t.Fatalf("the row still positions itself as an ambiguous call: %+v", ep.Boundary)
		}
	})
	t.Run("an unsupported payload version on a non-placement fact does not prove coverage", func(t *testing.T) {
		s := newSynth()
		s.skippedAttempt("r1", "e1", 1)
		r := s.fact(predictioneval.KindUserTerminal, predictioneval.PhaseTerminalAdmitted, "r1", "e1", 0)
		r.PayloadVersion = predictioneval.SupportedPayloadVersion + 1
		s.add(r)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if ep.Boundary.NoCallCoverage.Proven ||
			!containsString(ep.Boundary.NoCallCoverage.Reasons, "UNDECODABLE_FACT_ON_ROUND") {
			t.Fatalf("an unreadable terminal fact on the round cannot prove there was no call: %+v", ep.Boundary)
		}
	})
	t.Run("every kind read on the round is read the same way", func(t *testing.T) {
		// The arm lists seven kinds. KindAutoDecision is among them on
		// purpose: materialization does examine an automatic fact's version,
		// but it excludes that fact at attempt level under its observation
		// id, which is not a session refusal and says nothing about the rest
		// of the round — so the coverage argument is still this package's.
		for _, kind := range []string{
			predictioneval.KindAutoDecision, predictioneval.KindUserTerminal,
			"channel_event", "schedule_decision", "user_prediction_made",
			"round_cleanup", "manual_control",
		} {
			t.Run(kind, func(t *testing.T) {
				s := newSynth()
				s.skippedAttempt("r1", "e1", 1)
				r := s.fact(kind, "", "r1", "e1", 0)
				r.PayloadVersion = predictioneval.SupportedPayloadVersion + 1
				s.add(r)
				ep := singleEpisode(t, mustSelect(t, s.dataset()))
				if ep.Boundary.NoCallCoverage.Proven ||
					!containsString(ep.Boundary.NoCallCoverage.Reasons, "UNDECODABLE_FACT_ON_ROUND") {
					t.Fatalf("an unreadable %s on the round cannot prove there was no call: %+v", kind, ep.Boundary)
				}
			})
		}
	})
	t.Run("a known kind carrying a placement-only phase does not prove coverage", func(t *testing.T) {
		// The exploit this refuses: take a CALL_RETURNED whose start is missing
		// — an orphan, which breaks the coverage argument — and relabel its
		// KIND to one that never carries a call phase. The producer writes
		// call phases only on placement facts, so the pairing is one it cannot
		// emit; leaving it unread would let a relabel erase the orphan and
		// restore a proven boundary.
		for _, kind := range []string{
			predictioneval.KindAutoDecision, predictioneval.KindUserTerminal,
			"channel_event", "schedule_decision", "user_prediction_made",
			"round_cleanup", "manual_control",
		} {
			for _, phase := range []string{predictioneval.PhaseCallStarted, predictioneval.PhaseCallReturned} {
				t.Run(kind+"/"+phase, func(t *testing.T) {
					s := newSynth()
					s.skippedAttempt("r1", "e1", 1)
					s.add(s.fact(kind, phase, "r1", "e1", 0))
					ep := singleEpisode(t, mustSelect(t, s.dataset()))
					if ep.Boundary.NoCallCoverage.Proven ||
						!containsString(ep.Boundary.NoCallCoverage.Reasons, "UNCLASSIFIED_FACT_ON_ROUND") {
						t.Fatalf("a %s carrying %s is a pairing the producer cannot write: %+v", kind, phase, ep.Boundary)
					}
				})
				t.Run(kind+"/"+phase+"/unreadable", func(t *testing.T) {
					// The phase is judged only on a row this package can read.
					// An unreadable one is refused for being unreadable, and its
					// phase — which belongs to a vocabulary this package has not
					// pinned for that version — is not named as contradicted.
					for name, break_ := range map[string]func(*predictioneval.SourceRecord){
						"undecodable": func(r *predictioneval.SourceRecord) { r.PayloadUndecodable = true },
						"unsupported version": func(r *predictioneval.SourceRecord) {
							r.PayloadVersion = predictioneval.SupportedPayloadVersion + 1
						},
					} {
						t.Run(name, func(t *testing.T) {
							s := newSynth()
							s.skippedAttempt("r1", "e1", 1)
							r := s.fact(kind, phase, "r1", "e1", 0)
							break_(&r)
							s.add(r)
							ep := singleEpisode(t, mustSelect(t, s.dataset()))
							if ep.Boundary.NoCallCoverage.Proven {
								t.Fatalf("an unreadable row cannot prove coverage either: %+v", ep.Boundary)
							}
							if containsString(ep.Boundary.NoCallCoverage.Reasons, "UNCLASSIFIED_FACT_ON_ROUND") {
								t.Fatalf("its phase must not be judged: %+v", ep.Boundary.NoCallCoverage.Reasons)
							}
						})
					}
				})
			}
		}
	})
	t.Run("a known kind carrying an automatic-only phase does not prove coverage", func(t *testing.T) {
		// The second half of the same vocabulary, and the exploit is worse than
		// the placement one because it needs no orphan. Seam 2 reads attempt ids
		// out of auto_decision rows, so relabelling the EARLIEST automatic
		// attempt's rows onto another kind hides that attempt from selection
		// while this classifier leaves coverage proven — and a LATER attempt
		// becomes the first opportunity, which is the substitution the protocol
		// forbids.
		//
		// The ground is the same single-writer one the call phases rest on,
		// established the same way rather than assumed from the phase table's
		// grouping comments: every site that emits AUTO_DUE, AUTO_DECIDED or
		// AUTO_SKIPPED passes ObsKindAutoDecision, so no other kind can carry
		// one. The remaining phase families are NOT refused here, because their
		// writers have not been enumerated and a grouping comment is not a
		// contract.
		for _, kind := range []string{
			predictioneval.KindUserTerminal, "channel_event", "schedule_decision",
			"user_prediction_made", "round_cleanup", "manual_control",
		} {
			for _, phase := range []string{
				predictioneval.PhaseAutoDue, predictioneval.PhaseAutoDecided, predictioneval.PhaseAutoSkipped,
			} {
				t.Run(kind+"/"+phase, func(t *testing.T) {
					s := newSynth()
					s.skippedAttempt("r1", "e1", 1)
					s.add(s.fact(kind, phase, "r1", "e1", 0))
					ep := singleEpisode(t, mustSelect(t, s.dataset()))
					if ep.Boundary.NoCallCoverage.Proven ||
						!containsString(ep.Boundary.NoCallCoverage.Reasons, "UNCLASSIFIED_FACT_ON_ROUND") {
						t.Fatalf("a %s carrying %s is a pairing the producer cannot write: %+v", kind, phase, ep.Boundary)
					}
				})
			}
		}
		t.Run("the kind that does carry them is untouched", func(t *testing.T) {
			// The false-refusal control: auto_decision is where all three live.
			for _, phase := range []string{
				predictioneval.PhaseAutoDue, predictioneval.PhaseAutoDecided, predictioneval.PhaseAutoSkipped,
			} {
				t.Run(phase, func(t *testing.T) {
					s := newSynth()
					s.skippedAttempt("r1", "e1", 1)
					s.add(s.fact(predictioneval.KindAutoDecision, phase, "r1", "e1", 0))
					ep := singleEpisode(t, mustSelect(t, s.dataset()))
					if !ep.Boundary.NoCallCoverage.Proven {
						t.Fatalf("auto_decision is the kind these phases live on: %+v", ep.Boundary)
					}
				})
			}
		})
	})
	t.Run("a known kind carrying its own phase still proves coverage", func(t *testing.T) {
		// The control: the same arm must not start refusing honest rows. An
		// auto_decision carrying AUTO_DUE is exactly what the producer writes.
		s := newSynth()
		s.skippedAttempt("r1", "e1", 1)
		s.add(s.fact(predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, "r1", "e1", 0))
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if !ep.Boundary.NoCallCoverage.Proven {
			t.Fatalf("an honest auto_decision row must not break coverage: %+v", ep.Boundary)
		}
	})
	t.Run("a fact of a kind outside the vocabulary does not prove coverage", func(t *testing.T) {
		s := newSynth()
		s.skippedAttempt("r1", "e1", 1)
		s.add(s.fact("some_other_kind", "", "r1", "e1", 0))
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if ep.Boundary.NoCallCoverage.Proven ||
			!containsString(ep.Boundary.NoCallCoverage.Reasons, "UNCLASSIFIED_FACT_ON_ROUND") {
			t.Fatalf("a kind this package cannot name is unclassified evidence: %+v", ep.Boundary)
		}
	})
	t.Run("a supported, readable round still proves coverage", func(t *testing.T) {
		s := newSynth()
		s.skippedAttempt("r1", "e1", 1)
		s.userTerminal("r1", "e1", "OK", 0, 0)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if !ep.Boundary.NoCallCoverage.Proven || !ep.Boundary.Proven || ep.Excluded {
			t.Fatalf("the healthy no-call round is untouched: %+v", ep)
		}
	})
}

// TestEpisodeSignalMatchingKeepsItsOrderAndDeduplicates pins the two semantic
// properties the streaming merge has to preserve, at the public seam.
//
// The merge replaced a copy-sort-materialize per episode. Order and duplicate
// removal used to come from sort.Ints plus a neighbour check; they now come
// from advancing every posting list that holds the current minimum. Both are
// observable here rather than argued: a record reachable through TWO posting
// lists must still be seen once, and calls must still come back in ascending
// collector order.
func TestEpisodeSignalMatchingKeepsItsOrderAndDeduplicates(t *testing.T) {
	t.Run("a record matched by two indexes is still one record", func(t *testing.T) {
		// A placement fact carries the episode's EventID AND its round
		// incarnation, so it sits in byEvent and byPoolRound at once.
		//
		// This asserts only that a healthy placed attempt stays selectable. It
		// does NOT detect a dedup failure, and it used to claim it did: the
		// at-most-one-start rule lives in classifySignals, which runs once per
		// record while the index is built, so how many times a matched signal
		// is read afterwards cannot re-trigger it. The deduplication is pinned
		// differentially in evidence_internal_test.go instead, which is where
		// it can actually be observed.
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if !ep.Boundary.NoCallCoverage.Proven && containsString(ep.Boundary.NoCallCoverage.Reasons, "UNCLASSIFIED_FACT_ON_ROUND") {
			t.Fatalf("a doubly-indexed record must not look like two: %+v", ep.Boundary)
		}
		if ep.Excluded {
			t.Fatalf("a healthy placed attempt must stay selectable: %+v", ep)
		}
	})
	t.Run("the earliest call is the earliest one in collector order", func(t *testing.T) {
		// This asserts that the EARLIEST call is identified correctly. It does
		// NOT pin the merge's ordering, and an earlier version of this comment
		// claimed it did: the earliest call is selected by a minimum over
		// position and observation id, which is order-independent by
		// construction. Merge ordering is pinned differentially in
		// evidence_internal_test.go against the implementation this one
		// replaced. An independent lane proved the point with a mutant that
		// reversed the entire visit order: this test passed, that one failed.
		for _, tc := range []struct {
			name        string
			manualFirst bool
			wantKind    p4offline.CallKind
		}{
			{"a manual call before the automatic one", true, p4offline.CallKindManual},
			{"a manual call after the automatic one", false, p4offline.CallKindAuto},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := newSynth()
				s.due("r1", "e1", 1)
				if tc.manualFirst {
					s.manualCall("r1", "e1", 10, 0)
					s.placedAttempt("r1", "e1", 1)
				} else {
					s.placedAttempt("r1", "e1", 1)
					s.manualCall("r1", "e1", 10, 0)
				}
				ep := singleEpisode(t, mustSelect(t, s.dataset()))
				if !ep.Boundary.EarliestCallPresent {
					t.Fatalf("both shapes carry a call: %+v", ep.Boundary)
				}
				// Both calls are placement records; which one is EARLIEST is
				// the merge's answer, and the kind is what distinguishes them.
				if ep.Boundary.EarliestCallKind != tc.wantKind {
					t.Fatalf("earliest call is %s at %d, want %s: %+v",
						ep.Boundary.EarliestCallKind, ep.Boundary.EarliestCallPosition, tc.wantKind, ep.Boundary)
				}
			})
		}
	})
}

// TestEpisodeSelectionDoesNotRebuildTheEventBucketPerEpisode is the measured
// evidence for the colliding-EventID repair.
//
// It asserts SHAPE, not speed. A wall-clock threshold on shared CI would be
// flaky and would prove nothing about complexity, so the assertions are on
// allocated bytes, which are deterministic for this deterministic workload,
// and on RATIOS rather than absolute figures, so a machine with a different
// allocator profile does not move the verdict.
//
// Before the repair, matching copied and sorted the whole shared event bucket
// once per episode: 26 MB at 256 colliding rounds, 384 MB at 1,024 and 6.67 GB
// at 4,096 (12,288 records) — about 16x per 4x input, against 29 MB for the
// same record count with distinct event ids. The allocation was the part that
// mattered: it turns a slow parse into an OOM before the verifier can answer.
// WHAT THIS TEST DOES NOT COVER, named because it read as though it did. Its
// dataset emits only placedAttempt and skippedAttempt, so it carries ZERO manual
// signals at every size: the episodes x manual-signals product is identically
// zero here, and its allocation ratio is measured where that term does not
// exist. It also uses a DISTINCT-event dataset as its linear control, and that
// same shape is quadratic once the manual facts are unattributable. The product
// is covered by TestManualSignalsAreSharedNotRetainedPerEpisode, which scales
// both factors and drives both collision axes.
func TestEpisodeSelectionDoesNotRebuildTheEventBucketPerEpisode(t *testing.T) {
	// The fixture MUST carry call facts. An earlier version of this test built
	// skipped attempts only, so seam 1 matched no calls and the per-episode
	// accumulation the repair is about never ran — the test passed while a
	// colliding dataset with calls still allocated quadratically. An
	// independent lane found that gap by adding exactly this.
	build := func(rounds int, collide, withCalls bool) predictioneval.SourceDataset {
		s := newSynth()
		for i := 0; i < rounds; i++ {
			ev := "shared"
			if !collide {
				ev = fmt.Sprintf("e%d", i)
			}
			round := fmt.Sprintf("r%d", i)
			if withCalls {
				s.placedAttempt(round, ev, int64(i+1))
			} else {
				s.skippedAttempt(round, ev, int64(i+1))
			}
		}
		return s.dataset()
	}
	measure := func(t *testing.T, ds predictioneval.SourceDataset) uint64 {
		t.Helper()
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		if _, err := p4offline.SelectEpisodes(ds); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&b)
		return b.TotalAlloc - a.TotalAlloc
	}

	for _, shape := range []struct {
		name      string
		withCalls bool
	}{
		{"skipped attempts", false},
		{"attempts that placed a call", true},
	} {
		t.Run(shape.name, func(t *testing.T) {
			small := measure(t, build(512, true, shape.withCalls))
			large := measure(t, build(2048, true, shape.withCalls))
			distinct := measure(t, build(2048, false, shape.withCalls))
			t.Logf("colliding small=%d large=%d; distinct large=%d", small, large, distinct)

			// Quadratic growth over a 4x input is ~16x; linear is ~4x. The
			// bound is loose enough that allocator differences cannot trip it
			// and still far below the quadratic shape.
			if ratio := float64(large) / float64(small); ratio > 8 {
				t.Fatalf("allocation grew %.1fx over a 4x input, which is the superlinear shape this repair removes", ratio)
			}
			// And a shared event id must not cost materially more than
			// distinct ones at the same record count.
			if ratio := float64(large) / float64(distinct); ratio > 3 {
				t.Fatalf("colliding event ids cost %.1fx a distinct-id dataset of the same size", ratio)
			}
		})
	}
}

// TestAnUnspentAutomaticStartContradictsAnAdmittedSource enforces the owner's
// D16 disposition of PLACEMENT_NOT_RETURNED.
//
// P4 admits only COMPLETE + AS_FINALIZED source sessions. Under the pinned
// producer and lifecycle contract a factual AUTOMATIC CALL_STARTED in such a
// session cannot legitimately lack its CALL_RETURNED: the return is written
// unconditionally with any error carried into the fact, nothing in the
// producer's pubsub path recovers from a panic between the two, and a dropped
// observation prevents the session finalizing COMPLETE at all. An unspent
// automatic start beside a claim of that contract is therefore a contradiction
// in the evidence, and it fails closed HERE — before any factual placement can
// be minted from it — rather than being reported downstream as a placement
// outcome.
//
// The rule is stated of the AUTOMATIC emitter only, which passes one captured
// incarnation and event id to both halves of its pair. The manual emitter
// resolves the incarnation separately per half, so its two facts are not
// guaranteed to agree, and an episode carrying a manual call is excluded as an
// intervention regardless.
func TestAnUnspentAutomaticStartContradictsAnAdmittedSource(t *testing.T) {
	startedOnly := func() predictioneval.SourceDataset {
		s := newSynth()
		s.due("r1", "e1", 1)
		env := synthPlacedEnvelope()
		s.terminal("r1", "e1", 1, predictioneval.PhaseAutoDecided, "OK", env)
		// The start alone: no CALL_RETURNED anywhere on the round.
		s.placement("r1", "e1", 1, predictioneval.PhaseCallStarted, *env.FinalAmount, *env.ChoiceIndex, "OK", "NONE")
		return s.dataset()
	}
	t.Run("the evidence is refused rather than scored", func(t *testing.T) {
		ep := singleEpisode(t, mustSelect(t, startedOnly()))
		if ep.Boundary.NoCallCoverage.Proven {
			t.Fatalf("an unspent automatic start cannot leave coverage proven: %+v", ep.Boundary)
		}
		if !containsString(ep.Boundary.NoCallCoverage.Reasons, "UNCLASSIFIED_FACT_ON_ROUND") {
			t.Fatalf("want the start named as contradicted evidence: %+v", ep.Boundary.NoCallCoverage.Reasons)
		}
	})
	t.Run("the complete pair is untouched", func(t *testing.T) {
		// The false-refusal control. The very same attempt WITH its return is
		// the producer's normal output and must stay selectable.
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if containsString(ep.Boundary.NoCallCoverage.Reasons, "UNCLASSIFIED_FACT_ON_ROUND") {
			t.Fatalf("a settled pair is not contradicted evidence: %+v", ep.Boundary)
		}
	})
	t.Run("two complete pairs on one round are untouched", func(t *testing.T) {
		// Two attempts, each with its own start and return: every start is
		// spent, so nothing here is contradicted.
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		s.placedAttempt("r1", "e1", 2)
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if containsString(ep.Boundary.NoCallCoverage.Reasons, "UNCLASSIFIED_FACT_ON_ROUND") {
			t.Fatalf("two settled pairs are not contradicted evidence: %+v", ep.Boundary)
		}
	})
}

// TestUnspentStartDoesNotExcludeAHealthySiblingIncarnation scopes the D16 mark
// to the incarnation whose evidence actually contradicts itself.
//
// Signals are matched to episodes by EventID among other keys, so an unspent
// start recorded on one incarnation of a public round reaches every episode
// carrying that event. A comment used to assert this could only ADD A REASON
// and never flip an admission, on the ground that such a start carries an
// attempt id the sibling does not own and is therefore already excluded as an
// unattributed intervention. An independent lane falsified that: attempt ids
// are scoped to the pool, not the incarnation, so a sibling on the same pool
// CAN own the id — and then the D16 mark is the only thing excluding it.
func TestUnspentStartDoesNotExcludeAHealthySiblingIncarnation(t *testing.T) {
	byIncarnation := func(t *testing.T, sel p4selection, want string) p4episode {
		t.Helper()
		for _, ep := range sel.Episodes {
			if ep.Episode.RoundIncarnationID == want {
				return ep
			}
		}
		t.Fatalf("no episode for incarnation %q in %d episodes", want, len(sel.Episodes))
		return p4episode{}
	}
	build := func(paired bool) predictioneval.SourceDataset {
		s := newSynth()
		// The sibling: its own complete, earliest attempt, and it also owns
		// attempt id 2 on the same pool.
		s.skippedAttempt("r2", "e1", 1)
		s.due("r2", "e1", 2)
		// The contradicted incarnation: a start labelled attempt 2, with or
		// without its return.
		s.placement("r1", "e1", 2, predictioneval.PhaseCallStarted, 50, 0, "OK", "NONE")
		if paired {
			s.placement("r1", "e1", 2, predictioneval.PhaseCallReturned, 50, 0, "OK", "NONE")
		}
		return s.dataset()
	}
	pairedSel, unpairedSel := mustSelect(t, build(true)), mustSelect(t, build(false))
	paired := byIncarnation(t, pairedSel, "r2")
	unpaired := byIncarnation(t, unpairedSel, "r2")

	// The ONLY difference between the two datasets is whether r1's start was
	// spent. r2's own evidence is identical and complete in both.
	if paired.Excluded {
		t.Fatalf("the control must be admitted: %+v", paired.ExclusionReasons)
	}
	if unpaired.Excluded {
		t.Fatalf("an unspent start on a SIBLING incarnation must not exclude this one: %+v -> %+v",
			unpaired.ExclusionReasons, unpaired.Boundary)
	}
	if unpaired.Boundary.NoCallCoverage.Proven != paired.Boundary.NoCallCoverage.Proven {
		t.Fatalf("coverage differs on an episode whose own evidence is unchanged: %+v vs %+v",
			unpaired.Boundary.NoCallCoverage, paired.Boundary.NoCallCoverage)
	}

	// THE POSITIVE HALF, AND WITHOUT IT THIS TEST IS SATISFIED BY LOSING THE
	// MARK ALTOGETHER. A scope that matches nothing excuses the sibling and the
	// RECORDER alike, and the assertions above cannot tell the two apart: a
	// mutant keying the scope on the public round instead of the incarnation
	// survived them. So the incarnation that recorded the unspent start is
	// checked here, on the same two datasets.
	recorder := byIncarnation(t, unpairedSel, "r1")
	if recorder.Boundary.NoCallCoverage.Proven ||
		!containsString(recorder.Boundary.NoCallCoverage.Reasons, p4offline.CoverageUnclassifiedFact) {
		t.Fatalf("the incarnation that RECORDED the unspent start must carry the mark: %+v",
			recorder.Boundary.NoCallCoverage)
	}
	if spent := byIncarnation(t, pairedSel, "r1"); !spent.Boundary.NoCallCoverage.Proven {
		t.Fatalf("the same incarnation with its start SPENT carries no contradiction: %+v",
			spent.Boundary.NoCallCoverage)
	}
}

// TestReconcileDoesNotReframeEveryClaimPerComparison pins that reconciliation
// frames each claim's ordering key ONCE. claimKey is not an accessor: it builds
// a fresh length-prefixed framing of the whole claim and hex-encodes it, so it
// costs and allocates about twice the claim's own bytes. Computing it inside a
// sort comparator re-framed every claim about 2*log2(n) times on the shape this
// function exists to handle -- many claims on ONE public round.
//
// The assertion is the COLLIDING-versus-DISTINCT ratio, never a total, and
// never wall-clock. That choice is the point of the test: reconciliation frames
// every claim twice no matter what, because registryDigest has to frame each
// claim to digest it, and those two mandatory passes dominate the absolute
// number. A total-allocation threshold would therefore be measuring the digest,
// not the comparator. Comparisons happen only WITHIN a group, so n claims on
// one round and n claims on n distinct rounds pay the same mandatory passes and
// differ only in the comparator's share.
//
// Measured, with the per-comparison framing restored as a disposable mutant and
// then removed again: 1.73 -> 1.39 at n=8192, and 1.69 -> 1.35 at n=2048. The
// threshold sits between, with margin on both sides.
//
// NOT claimed: this does not make reconciliation cheap. An independent lane
// reported roughly 80x amplification over the claims' own bytes; that figure is
// right and mostly is NOT this defect -- it is the two mandatory framings. What
// this removes is the log-n factor on top of them, which was 20% of the
// allocation at n=8192 and grows with the group.
// TestRegistryRefusesOnItsConstantsBeforeItReconciles pins the SEVENTH-instance
// sibling in VerifySourceRoundRegistry: the two O(1) clauses of its condition
// run before the work, not after it.
//
// The whole flattening, ReconcileSourceRounds, the per-claim framing and
// registryDigest used to run as STATEMENTS above a condition whose first two
// clauses are constant comparisons -- and every clause of that condition
// returns the same sentinel, so nothing about precedence was at stake. An
// independent judge measured a one-byte-wrong Version at 8,865,720 / 19,038,200
// / 37,564,760 / 76,808,072 bytes and 8.9 / 28.9 / 55.8 / 105.1 ms for n =
// 2,000 / 4,000 / 8,000 / 16,000 claims: 59% of the cost of a VALID
// verification, to refuse on a string comparison.
//
// The assertion is an absolute budget rather than a ratio, because the repaired
// path does no work at all that grows with n -- which is exactly the property.
// TestManualSignalReportIsBoundedAndEpisodesDoNotAlias is the receipt for the
// episodes x manual-signals cross product, and for the repair that replaced the
// first attempt at fixing it.
//
// WHAT WAS WRONG. EvidenceSelection reported, and held, an episodes x
// manual-signals cross product -- exactly n*n. Review measured 250,000 /
// 1,000,000 / 4,000,000 / 16,000,000 entries at n = 500 / 1,000 / 2,000 / 4,000,
// peak RSS 2,057,468 KiB from 12,213,348 bytes of input, and at a fixed 50,000
// records under a declared 2 GiB cap the call dying with an unrecoverable
// "fatal error: runtime: out of memory" where the same size in a linear shape
// returned in 1.6 s holding 13 MB. On TWO axes: a shared EventID, and manual
// facts naming no incarnation, which route to a bucket keyed on PoolInstanceID
// alone and match every episode of the pool.
//
// THE FIRST REPAIR WAS WORSE THAN THE BOUND, and this test exists in its current
// shape because of what review found in it. It shared one backing array between
// episodes whose matched set was identical. That aliased the caller's slices --
// two episodes at len 3 cap 4, one ordinary append each, and the first
// episode's tail read the second's value -- it hashed every byte of every entry
// to decide what could be shared, costing 8x CPU on a dimension nothing
// bounded, and it counted only what was shared, so the ceiling could not see a
// report carrying 1,001,000 entries and 31 MB of JSON.
//
// SO THE BOUND IS ON WHAT IS REPORTED, and the two properties below are what
// that buys: a shape that would exceed it is refused before the memory is
// committed, and every episode owns its own slice.
func TestManualSignalReportIsBoundedAndEpisodesDoNotAlias(t *testing.T) {
	// n episodes on one pool, and n manual facts every one of them matches.
	build := func(n int, sharedEvent bool) predictioneval.SourceDataset {
		s := newSynth()
		for i := 0; i < n; i++ {
			ev := "shared"
			if !sharedEvent {
				ev = "e" + strconv.Itoa(i)
			}
			s.placedAttempt("r"+strconv.Itoa(i), ev, int64(i+1))
		}
		for j := 0; j < n; j++ {
			if sharedEvent {
				s.manualCall("r0", "shared", 50, 0)
			} else {
				// No incarnation and an event no episode carries: the
				// pool-unattributed route, which needs no event collision.
				s.manualCall("", "u"+strconv.Itoa(j), 50, 0)
			}
		}
		return s.dataset()
	}

	for _, shape := range []struct {
		name        string
		sharedEvent bool
	}{
		{"a shared event id", true},
		{"manual facts that name no incarnation", false},
	} {
		t.Run(shape.name, func(t *testing.T) {
			// Under the ceiling: selected, and the content is what it always was.
			const under = 300
			sel, err := p4offline.SelectEpisodes(build(under, shape.sharedEvent))
			if err != nil {
				t.Fatalf("n=%d is under the ceiling and must be selected: %v", under, err)
			}
			total := 0
			for i := range sel.Episodes {
				total += len(sel.Episodes[i].ManualSignals)
			}
			if total != under*under {
				t.Fatalf("every episode reports every manual signal matched to it: summed %d, want %d",
					total, under*under)
			}

			t.Run("and no episode's slice aliases another's", func(t *testing.T) {
				// THE REGRESSION CHECK. The reverted repair handed one array
				// with spare capacity to every episode, so an ordinary caller
				// append clobbered a sibling. Two appends, one per episode, must
				// not be able to see each other.
				if len(sel.Episodes) < 2 {
					t.Fatalf("need two episodes to test aliasing, got %d", len(sel.Episodes))
				}
				a := sel.Episodes[0].ManualSignals
				b := sel.Episodes[1].ManualSignals
				if len(a) == 0 || len(b) == 0 {
					t.Fatal("both episodes must report manual signals")
				}
				beforeB := b[0]
				a[0] = "MUTATED-BY-CALLER"
				if b[0] != beforeB {
					t.Fatal("writing one episode's manual signals changed another's")
				}
				// BOTH tails are checked, not one. An earlier draft appended to
				// both and inspected only the first -- which the linter caught as
				// an ineffectual assignment, and it was right about more than
				// style: with the shared array, it is the SECOND append that
				// overwrites the first, so inspecting only one of them could
				// miss the very collision this case exists for.
				a = append(a, "ANNOTATION-A")
				b = append(b, "ANNOTATION-B")
				if a[len(a)-1] != "ANNOTATION-A" || b[len(b)-1] != "ANNOTATION-B" {
					t.Fatalf("appends into two episodes' lists collided: %q and %q",
						a[len(a)-1], b[len(b)-1])
				}
			})

			t.Run("and a shape that would report past the ceiling is refused typed", func(t *testing.T) {
				// 1,100 episodes x 1,100 signals is 1,210,000 reported entries
				// against a ceiling of 1,048,576.
				_, err := p4offline.SelectEpisodes(build(1100, shape.sharedEvent))
				if !errors.Is(err, p4offline.ErrEvidenceRetention) {
					t.Fatalf("want ErrEvidenceRetention, got %v", err)
				}
				if msg := err.Error(); !strings.Contains(msg, "manual-signal attributions") {
					t.Fatalf("the refusal must name what it counted: %s", msg)
				}
			})
		})
	}

	t.Run("and an ordinary session is nowhere near the ceiling", func(t *testing.T) {
		// The false-refusal control. A ceiling that refused honest evidence
		// would be worse than the exhaustion it prevents.
		s := newSynth()
		for i := 0; i < 200; i++ {
			s.placedAttempt("r"+strconv.Itoa(i), "e"+strconv.Itoa(i), int64(i+1))
		}
		s.manualCall("r0", "e0", 50, 0)
		if _, err := p4offline.SelectEpisodes(s.dataset()); err != nil {
			t.Fatalf("an ordinary session must be selected, not refused: %v", err)
		}
	})
}

// TestManualSignalBudgetBoundaryIsTheLiteralValue pins the BUDGET ITSELF --
// 1<<20 reported entries across one selection -- at the three points that
// matter, through the public seam and with the number written out rather than
// read from the package. A test that imported the constant would agree with any
// value the constant took.
//
// The budget is an operational OUTPUT-ENTRY budget, and nothing more: not a
// rule about which datasets are valid, not a statistical exclusion, not a bound
// on process memory or on the serialized document. What it says is how many
// manual-signal attributions ONE selection will emit.
func TestManualSignalBudgetBoundaryIsTheLiteralValue(t *testing.T) {
	const budget = 1 << 20 // 1,048,576

	// episodes x facts reported entries: every episode on the pool matches
	// every manual fact that names no incarnation.
	build := func(episodes, facts int) predictioneval.SourceDataset {
		s := newSynth()
		for i := 0; i < episodes; i++ {
			s.placedAttempt("r"+strconv.Itoa(i), "e"+strconv.Itoa(i), int64(i+1))
		}
		for j := 0; j < facts; j++ {
			s.manualCall("", "u"+strconv.Itoa(j), 50, 0)
		}
		return s.dataset()
	}

	reported := func(t *testing.T, sel p4offline.EvidenceSelection) int {
		t.Helper()
		n := 0
		for i := range sel.Episodes {
			n += len(sel.Episodes[i].ManualSignals)
		}
		return n
	}

	t.Run("below the budget", func(t *testing.T) {
		sel, err := p4offline.SelectEpisodes(build(1024, 1023))
		if err != nil {
			t.Fatalf("1,047,552 entries is under the budget: %v", err)
		}
		if got := reported(t, sel); got != budget-1024 {
			t.Fatalf("reported %d entries, want %d", got, budget-1024)
		}
	})

	t.Run("at exactly the budget", func(t *testing.T) {
		// THE BOUNDARY. At the limit, otherwise-valid processing is supported:
		// the last admissible entry is emitted and the selection succeeds.
		sel, err := p4offline.SelectEpisodes(build(1024, 1024))
		if err != nil {
			t.Fatalf("exactly %d entries must still be selected: %v", budget, err)
		}
		if got := reported(t, sel); got != budget {
			t.Fatalf("reported %d entries, want exactly %d", got, budget)
		}
	})

	t.Run("one entry past the budget", func(t *testing.T) {
		sel, err := p4offline.SelectEpisodes(build(1024, 1025))
		if !errors.Is(err, p4offline.ErrEvidenceRetention) {
			t.Fatalf("want ErrEvidenceRetention, got %v", err)
		}
		if msg := err.Error(); !strings.Contains(msg, "manual-signal attributions") ||
			!strings.Contains(msg, strconv.Itoa(budget)) {
			t.Fatalf("the refusal must name what it counted and the budget: %s", msg)
		}
		// ALL OR NOTHING. A partial cohort returned as a complete one is the
		// failure this refusal exists to prevent, so the zero selection comes
		// back -- not the episodes that fitted.
		if sel.SessionAdmitted || len(sel.Episodes) != 0 || sel.ProtocolVersion != "" {
			t.Fatalf("an aborted selection must return nothing at all: %+v", sel.Source)
		}
	})
}

// collidingManualDataset builds k episodes on ONE event id and k manual facts
// that name no incarnation, so every episode matches every manual fact and the
// selection reports k*k manual-signal entries. It is the shape the output
// budget is about.
func collidingManualDataset(k int) predictioneval.SourceDataset {
	s := newSynth()
	for i := 0; i < k; i++ {
		s.placedAttempt("r"+strconv.Itoa(i), "shared", int64(i+1))
	}
	for j := 0; j < k; j++ {
		s.manualCall("", "u"+strconv.Itoa(j), 50, 0)
	}
	return s.dataset()
}

// TestSelectionRefusalIsDistinctFromEpisodeExclusion covers the downstream half
// of the resource refusal, which nothing drove before: review found
// ErrEvidenceRetention occurred at exactly ONE line in every test in the
// package, so no test carried it past the seam that produced it.
//
// AssessCaseQuality collapsed every error class from lookupEpisode into
// EPISODE_EXCLUDED, so a refusal of the WHOLE DATASET on its shape was recorded
// as evidence about this episode -- a claim about evidence that was never read.
func TestSelectionRefusalIsDistinctFromEpisodeExclusion(t *testing.T) {
	// A dataset refused on its shape: 1,100 x 1,100 reported entries against a
	// budget of 1,048,576.
	refused := collidingManualDataset(1100)
	if _, err := p4offline.SelectEpisodes(refused); !errors.Is(err, p4offline.ErrEvidenceRetention) {
		t.Fatalf("the fixture must be refused on its shape, or this proves nothing: %v", err)
	}

	_, fs := selectedFactset(t, nil, nil)
	q := p4offline.AssessCaseQuality(refused, fs, p4offline.PolicyDecision{}, p4offline.PolicyDecision{},
		p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, nil, "p"))
	if q.Quality != p4offline.QualityExcluded {
		t.Fatalf("a selection that could not run must still be fail-closed: %+v", q)
	}
	if !containsString(q.Reasons, p4offline.QualityReasonSelectionUnavailable) {
		t.Fatalf("the reason must say the selection could not RUN: %v", q.Reasons)
	}
	if containsString(q.Reasons, p4offline.QualityReasonEpisodeExcluded) {
		t.Fatalf("a dataset refused on its shape says nothing about this episode: %v", q.Reasons)
	}

	// THE MACHINE-READABLE HALF. Both outcomes are EXCLUDED and fail-closed, so
	// the Quality cannot tell an operational abort from an ordinary exclusion,
	// and a consumer reading prose reasons is a consumer that will one day
	// count one as the other.
	if q.ProcessingComplete {
		t.Fatal("a selection that could not run must not report complete processing")
	}

	// AN ABORT SURVIVES A MERGE IN EITHER ARGUMENT POSITION, and both are
	// asserted because only one of them is reachable through this package.
	// AssessCaseQuality always merges with the incomplete record as the
	// RECEIVER, so a Merge that simply discarded the other side's completeness
	// left the whole suite green -- found by an independent lane. Merge is
	// exported, and the caller this round is written for is exactly one that
	// folds many records together in whatever order it likes.
	if p4offline.NewQualityRecord().Merge(q).ProcessingComplete {
		t.Fatal("merging a complete record WITH an incomplete one must not erase the abort")
	}
	if q.Merge(p4offline.NewQualityRecord()).ProcessingComplete {
		t.Fatal("merging an incomplete record with a complete one must not erase the abort")
	}
	if !p4offline.NewQualityRecord().Merge(p4offline.NewQualityRecord()).ProcessingComplete {
		t.Fatal("two complete records merge to a complete one")
	}

	t.Run("and a dataset the selection could not READ reports the same way", func(t *testing.T) {
		// THE SECOND WAY A SELECTION CAN FAIL TO RUN, and the one an earlier
		// version of this field missed: the caller hands the records over out of
		// causal order, SelectEpisodes refuses on its contract, and nothing
		// whatever is established about any episode. An independent lane found
		// the record asserting COMPLETE processing there -- the exact fail-open
		// the field exists to close -- so the predicate now enumerates the
		// COMPLETE case and treats every other error as incomplete.
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		ds := s.dataset()
		if len(ds.Records) < 2 {
			t.Fatalf("need two records to disorder, got %d", len(ds.Records))
		}
		ds.Records[0], ds.Records[1] = ds.Records[1], ds.Records[0]
		if _, err := p4offline.SelectEpisodes(ds); err == nil ||
			errors.Is(err, p4offline.ErrEvidenceRetention) {
			t.Fatalf("the fixture must be refused on the ORDERING contract: %v", err)
		}
		q := p4offline.AssessCaseQuality(ds, fs, p4offline.PolicyDecision{}, p4offline.PolicyDecision{},
			p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, nil, "p"))
		if q.ProcessingComplete {
			t.Fatal("a dataset that was never read must not report complete processing")
		}
		if !containsString(q.Reasons, p4offline.QualityReasonSelectionUnavailable) ||
			containsString(q.Reasons, p4offline.QualityReasonEpisodeExcluded) {
			t.Fatalf("the reason must say the selection could not RUN: %v", q.Reasons)
		}
	})

	t.Run("and an ordinarily excluded episode still reports COMPLETE processing", func(t *testing.T) {
		// THE DISCRIMINATING CONTROL, and without it the assertion above would
		// pass on a field that is simply always false. Here the selection RAN:
		// it read the whole dataset and established that this episode is not
		// in it. Same Quality, same fail-closed verdict, opposite processing
		// status -- which is the entire reason the second axis exists.
		other := newSynth()
		other.placedAttempt("r9", "e9", 9)
		q := p4offline.AssessCaseQuality(other.dataset(), fs, p4offline.PolicyDecision{}, p4offline.PolicyDecision{},
			p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, nil, "p"))
		if q.Quality != p4offline.QualityExcluded {
			t.Fatalf("the control must be excluded, or it discriminates nothing: %+v", q)
		}
		if !containsString(q.Reasons, p4offline.QualityReasonEpisodeExcluded) {
			t.Fatalf("the control must be an ORDINARY exclusion: %v", q.Reasons)
		}
		if !q.ProcessingComplete {
			t.Fatalf("an ordinary exclusion is evidence that WAS produced: %+v", q)
		}
	})
}

// TestQuadraticMatchingShapeIsProcessedRatherThanRefused is the positive half
// of removing the visit ceiling this package briefly carried.
//
// THE SHAPE. One undecodable AUTO_DUE per round incarnation makes every row its
// own episode AND a signal every other episode on that event matches, so the
// matching relation is |episodes| x |signals|: 256,000,000 pairs at k = 16,000.
// The ceiling refused the dataset rather than walk them.
//
// IT IS NOT WALKED NOW. What each of those pairs established was
// episode-INDEPENDENT -- an undecodable fact breaks the coverage argument for
// every episode that matches it -- so it is established once per posting list
// and read per episode. The dataset is processed, and the answer is the same
// answer the walk gave: every episode excluded, with the undecodable-fact
// coverage reason.
func TestQuadraticMatchingShapeIsProcessedRatherThanRefused(t *testing.T) {
	build := func(k int, oneEvent bool) predictioneval.SourceDataset {
		s := newSynth()
		for i := 0; i < k; i++ {
			ev := "e" + strconv.Itoa(i)
			if oneEvent {
				ev = "e1"
			}
			r := s.fact(predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue,
				"r"+strconv.Itoa(i), ev, 1)
			r.PayloadUndecodable = true
			s.add(r)
		}
		return s.dataset()
	}

	const k = 16000
	t.Run("the quadratic shape is selected", func(t *testing.T) {
		sel, err := p4offline.SelectEpisodes(build(k, true))
		if err != nil {
			t.Fatalf("the shape must be processed, not refused: %v", err)
		}
		if len(sel.Episodes) != k {
			t.Fatalf("want %d episodes, got %d", k, len(sel.Episodes))
		}
		// And the verdict is the one the walk produced: the episodes share a
		// public round, so all but the earliest are superseded as well, but
		// every one of them carries the coverage reason the matched
		// undecodable facts establish.
		for i := range sel.Episodes {
			ep := &sel.Episodes[i]
			if !ep.Excluded {
				t.Fatalf("episode %d matched %d undecodable facts and must be excluded", i, k)
			}
			if ep.Boundary.NoCallCoverage.Proven {
				t.Fatalf("episode %d: coverage cannot be proven beside an undecodable fact", i)
			}
			if len(ep.Boundary.NoCallCoverage.Reasons) != 1 ||
				ep.Boundary.NoCallCoverage.Reasons[0] != p4offline.CoverageUndecodableFact {
				t.Fatalf("episode %d: want exactly the undecodable reason, got %v",
					i, ep.Boundary.NoCallCoverage.Reasons)
			}
		}
	})

	t.Run("and the same record count on distinct events is selected too", func(t *testing.T) {
		// The linear counterpart, kept: what the producer actually writes is
		// one incarnation per round, each on its own event.
		sel, err := p4offline.SelectEpisodes(build(k, false))
		if err != nil {
			t.Fatalf("an honest dataset of %d records must be selected: %v", k, err)
		}
		if len(sel.Episodes) != k {
			t.Fatalf("want %d episodes, got %d", k, len(sel.Episodes))
		}
	})
}

func TestRegistryRefusesOnItsConstantsBeforeItReconciles(t *testing.T) {
	const n = 4096
	claims := make([]p4offline.SourceRoundClaim, 0, n)
	for i := 0; i < n; i++ {
		claims = append(claims, p4offline.SourceRoundClaim{
			Episode: p4offline.EpisodeIdentity{
				CollectorEpoch: 1, CollectorSessionID: "session-" + strconv.Itoa(i),
				PoolInstanceID: "pool-" + strconv.Itoa(i), RoundIncarnationID: "round-" + strconv.Itoa(i),
				EventID: "e" + strconv.Itoa(i),
			},
			Attempt: predictioneval.AttemptKey{
				CollectorEpoch: 1, CollectorSessionID: "session-" + strconv.Itoa(i),
				PoolInstanceID: "pool-" + strconv.Itoa(i), AttemptID: uint64(i + 1),
			},
			FactsetDigest: fmt.Sprintf("%064x", i),
		})
	}
	valid := p4offline.ReconcileSourceRounds(claims)
	if err := p4offline.VerifySourceRoundRegistry(valid); err != nil {
		t.Fatalf("the fixture must verify, or the refusals below prove nothing: %v", err)
	}

	measure := func(reg p4offline.SourceRoundRegistry) uint64 {
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		if err := p4offline.VerifySourceRoundRegistry(reg); err == nil {
			t.Fatal("this registry must be refused")
		}
		runtime.ReadMemStats(&b)
		return b.TotalAlloc - a.TotalAlloc
	}

	// Well above anything the two comparisons legitimately need, and orders
	// below the reconciliation the defect paid for at this n.
	const budget = 64 * 1024

	t.Run("and a present but wrong digest is judged before the reconciliation", func(t *testing.T) {
		// THE SAME FUNCTION, ONE GATE LATER, and the reason this subtest exists
		// is that the repair above stopped where the finding did. Framing the
		// entries to digest them is O(n) and unavoidable; flattening every claim
		// and RE-RECONCILING them is a second, independent pass whose product is
		// thrown away the moment the digest disagrees. Judged at 24,641,243 /
		// 50,923,867 / 101,334,414 / 203,719,320 bytes for n = 2,000 / 4,000 /
		// 8,000 / 16,000 -- 100.0% of a VALID verification at every n.
		//
		// THE ASSERTION IS A RATIO, NOT THE ABSOLUTE BUDGET ABOVE, and that is
		// deliberate: registryDigest is legitimately O(n), so this refusal
		// cannot be free the way the two constant clauses are. What it must not
		// do is cost as much as verifying the registry outright.
		bad := valid
		bad.Digest = strings.Repeat("f", len(valid.Digest))
		if bad.Digest == valid.Digest {
			t.Fatal("the fixture digest must differ, or there is no mismatch to judge")
		}

		refused := measure(bad)
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		if err := p4offline.VerifySourceRoundRegistry(valid); err != nil {
			t.Fatalf("the valid registry must still verify: %v", err)
		}
		runtime.ReadMemStats(&b)
		full := b.TotalAlloc - a.TotalAlloc
		t.Logf("%d claims; refusing a wrong digest allocated %d bytes against %d for a full verification (%.2fx)",
			n, refused, full, float64(refused)/float64(full))

		if full == 0 {
			t.Fatal("a full verification must allocate something, or the ratio is meaningless")
		}
		if refused*10 > full*7 {
			t.Fatalf("refusing on a digest cost %d bytes against %d for a full verification: the registry was reconciled before its digest was judged",
				refused, full)
		}
	})
	for _, tc := range []struct {
		name string
		edit func(*p4offline.SourceRoundRegistry)
	}{
		{"a foreign registry version", func(r *p4offline.SourceRoundRegistry) { r.Version += "x" }},
		{"an absent digest", func(r *p4offline.SourceRoundRegistry) { r.Digest = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := valid
			tc.edit(&bad)
			allocated := measure(bad)
			t.Logf("%d claims; the refusing call allocated %d bytes", n, allocated)
			if allocated > budget {
				t.Fatalf("refusing on a constant allocated %d bytes over %d claims: the registry was reconciled before it was judged",
					allocated, n)
			}
		})
	}
}

func TestReconcileDoesNotReframeEveryClaimPerComparison(t *testing.T) {
	// Distinct factset digests, so a shared round is a CONFLICT rather than
	// identical duplicates: that is the shape that forces every comparison.
	build := func(n int, collide bool) []p4offline.SourceRoundClaim {
		cs := make([]p4offline.SourceRoundClaim, 0, n)
		for i := 0; i < n; i++ {
			ev := "shared"
			if !collide {
				ev = "e" + strconv.Itoa(i)
			}
			cs = append(cs, p4offline.SourceRoundClaim{
				Episode: p4offline.EpisodeIdentity{
					CollectorEpoch: 1, CollectorSessionID: "session-" + strconv.Itoa(i),
					PoolInstanceID: "pool-" + strconv.Itoa(i), RoundIncarnationID: "round-" + strconv.Itoa(i),
					EventID: ev,
				},
				Attempt: predictioneval.AttemptKey{
					CollectorEpoch: 1, CollectorSessionID: "session-" + strconv.Itoa(i),
					PoolInstanceID: "pool-" + strconv.Itoa(i), AttemptID: uint64(i + 1),
				},
				FactsetDigest: fmt.Sprintf("%064x", i),
			})
		}
		return cs
	}
	measure := func(cs []p4offline.SourceRoundClaim) uint64 {
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		p4offline.ReconcileSourceRounds(cs)
		runtime.ReadMemStats(&b)
		return b.TotalAlloc - a.TotalAlloc
	}

	const n = 8192
	colliding := measure(build(n, true))
	distinct := measure(build(n, false))
	ratio := float64(colliding) / float64(distinct)
	t.Logf("%d claims: colliding=%d distinct=%d ratio=%.2f", n, colliding, distinct, ratio)
	if ratio > 1.55 {
		t.Fatalf("one shared round cost %.2fx the same claims on distinct rounds: the ordering key is being reframed per comparison", ratio)
	}

	// WHAT THE ORDER HAS TO BE, and what it does not. registryDigest frames the
	// claims in the order they are left in, so the registry's digest -- the
	// value VerifySourceRoundRegistry re-derives and AssessDenominatorMembership
	// names on its verdict -- depends on it. What the contract needs is that the
	// order be a deterministic function of the claim SET and not of the order
	// the claims happened to arrive in. Which total order is not load-bearing:
	// a group is CONFLICT when its claims differ, and where a canonical claim
	// exists the claims are identical, so cs[0] is the same whichever
	// deterministic order is chosen.
	//
	// TWO MUTANTS SHAPED THIS TEST, and one of them corrected me. A `len(cs) < 2`
	// early return widened to `< 3` leaves a TWO-claim group in ARRIVAL order --
	// plainly the non-determinism this pins, and no case reached it. The other
	// reverses the undecorated result, and I first argued it was an equivalent
	// mutant on the reasoning above: a deterministic permutation preserves the
	// property, so which order is chosen cannot matter. That was wrong, and the
	// rotated input below is what showed it. Reversing the undecorate step is
	// NOT a permutation of the sorted result -- it maps sorted position i to
	// input position len-1-idx[i], which depends on the arrival order through
	// idx -- so the same claim set reconciles differently depending on how it
	// arrived. It is killed, not argued away. Three arrival orders and three
	// group sizes, because a single reversal is satisfiable by a symmetry.
	// INVALID CLAIMS ARE SORTED BY THE SAME CALL AND WERE PINNED BY NOTHING.
	// ReconcileSourceRounds sorts the invalid list too, and registryDigest
	// frames the INVALID entries in the order it is left in -- so their order is
	// load-bearing for the digest VerifySourceRoundRegistry re-derives and
	// AssessDenominatorMembership names on its verdict. A lane deleted that one
	// call and the whole suite stayed green, then showed two blank-round claims
	// reconciling to two DIFFERENT digests depending only on arrival order,
	// with verification returning nil for both.
	buildInvalid := func(n int) []p4offline.SourceRoundClaim {
		cs := make([]p4offline.SourceRoundClaim, 0, n)
		for i := 0; i < n; i++ {
			c := build(1, true)[0]
			c.Episode.EventID = "" // no round: an invalid claim
			c.Episode.CollectorSessionID = "session-" + strconv.Itoa(i)
			c.Attempt.AttemptID = uint64(i + 1)
			c.FactsetDigest = fmt.Sprintf("%064x", i)
			cs = append(cs, c)
		}
		return cs
	}

	for _, shape := range []struct {
		name  string
		build func(int) []p4offline.SourceRoundClaim
		want  p4offline.SourceRoundStatus
	}{
		{"claims on one round", func(n int) []p4offline.SourceRoundClaim { return build(n, true) }, p4offline.SourceRoundConflict},
		{"invalid claims", buildInvalid, p4offline.SourceRoundInvalid},
	} {
		for _, n := range []int{2, 3, 64} {
			runArrivalOrderCase(t, shape.name, n, shape.build, shape.want)
		}
	}
}

// runArrivalOrderCase asserts that a claim set reconciles to the same entries
// and the same digest however it arrived: forward, reversed and rotated. Three
// orders, because a single reversal is satisfiable by a symmetry -- which is
// exactly how a deterministic-but-arrival-dependent permutation survived the
// first version of this test.
func runArrivalOrderCase(t *testing.T, shape string, n int,
	build func(int) []p4offline.SourceRoundClaim, want p4offline.SourceRoundStatus) {
	t.Helper()
	t.Run(fmt.Sprintf("%d %s reconcile independently of arrival order", n, shape), func(t *testing.T) {
		base := build(n)
		forward := p4offline.ReconcileSourceRounds(base)

		reversed := make([]p4offline.SourceRoundClaim, n)
		for i := range base {
			reversed[i] = base[n-1-i]
		}
		rotated := append(append([]p4offline.SourceRoundClaim(nil), base[n/2:]...), base[:n/2]...)

		wantEntries := 1
		if want == p4offline.SourceRoundInvalid {
			// Every invalid claim is its own entry.
			wantEntries = n
		}
		for _, name := range []string{"reversed", "rotated"} {
			other := reversed
			if name == "rotated" {
				other = rotated
			}
			got := p4offline.ReconcileSourceRounds(other)
			if len(forward.Entries) != wantEntries || len(got.Entries) != wantEntries {
				t.Fatalf("want %d entries, got %d / %d", wantEntries, len(forward.Entries), len(got.Entries))
			}
			if forward.Entries[0].Status != want {
				t.Fatalf("want status %q, got %q", want, forward.Entries[0].Status)
			}
			if forward.Digest != got.Digest {
				t.Fatalf("the registry digest depends on the order the claims arrived in (%s)", name)
			}
			for e := range forward.Entries {
				for i := range forward.Entries[e].Claims {
					if forward.Entries[e].Claims[i] != got.Entries[e].Claims[i] {
						t.Fatalf("entry %d claim %d ordered differently from a %s input", e, i, name)
					}
				}
			}
		}
	})
}

// TestP2ExclusionsAreAttributedByObservationNotOnlyByAttemptKey covers the one
// branch of p2ExclusionsFor that no test reached. A coverage profile showed
// count 0 on all three of its blocks, and an independent lane showed that BOTH
// contradictory mutants of its predicate survived the whole suite: `!=` -> `==`
// (attribute nothing) and `==` -> `!=` (attribute every unrelated exclusion).
//
// The branch matters because P2Exclusions is what tells an operator WHY the
// first opportunity was unusable. Under-attribution loses the reason; the
// package's own factset labelling reads IncompleteReasons from the same family,
// so silence there is indistinguishable from nothing having gone wrong.
//
// The reachable shape is an automatic row that NAMES its attempt in the
// counters but whose payload the producer could not decode: seam 2 records its
// observation id against the attempt, while materialization excludes it with an
// observation id and NO key.
func TestP2ExclusionsAreAttributedByObservationNotOnlyByAttemptKey(t *testing.T) {
	s := newSynth()
	bad := s.fact(predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, "r1", "e1", 1)
	bad.PayloadUndecodable = true
	s.add(bad)
	// A second round refused for a DIFFERENT reason. The two reasons must
	// differ: with the same reason on both, appendOnce collapses an
	// over-attributing predicate back to one entry and the over-attribution
	// direction goes unseen -- which is exactly how one of the two
	// contradictory mutants survived a first draft of this test.
	other := s.fact(predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, "r2", "e2", 2)
	other.PayloadVersion = predictioneval.SupportedPayloadVersion + 1
	s.add(other)
	ds := s.dataset()

	sel := mustSelect(t, ds)
	if len(sel.Episodes) != 2 {
		t.Fatalf("both rounds must form episodes: %+v", sel.Episodes)
	}
	seen := map[string][]string{}
	for _, ep := range sel.Episodes {
		if ep.FirstOpportunityUsable {
			t.Fatalf("an undecodable first opportunity is not usable: %+v", ep)
		}
		seen[ep.Episode.EventID] = ep.P2Exclusions
	}
	for _, tc := range []struct{ id, want string }{
		{"e1", predictioneval.ExclusionPayloadUndecodable},
		{"e2", predictioneval.ExclusionUnsupportedPayloadVersion},
	} {
		got := seen[tc.id]
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("%s must carry exactly its own refusal %q, got %v", tc.id, tc.want, got)
		}
	}
}

// TestProveCommonCutoffPresupposesACutoff pins the documented limit of the
// exported boundary predicate, so it cannot drift while the owner decision
// about the seam's shape is outstanding.
//
// The predicate takes the cutoff as a bare int64 and cannot tell an ABSENT
// cutoff from one at position 0, so a caller who has none and calls it anyway
// is told BOUNDARY_PROVEN. That fails OPEN, which is why it is documented on
// BoundaryProof rather than left to be found -- and why SelectEpisodes guards
// the call externally and records CUTOFF_UNKNOWN itself.
func TestProveCommonCutoffPresupposesACutoff(t *testing.T) {
	calls := []p4offline.CallSignal{{Position: 5, ObservationID: "call-1", Kind: p4offline.CallKindAuto}}
	coverage := p4offline.NoCallCoverageProof{Proven: true}

	t.Run("a real cutoff after the call is refused", func(t *testing.T) {
		got := p4offline.ProveCommonCutoff(9, "obs-9", calls, coverage)
		if got.Proven || got.Reason != p4offline.BoundaryCutoffNotBeforeCall {
			t.Fatalf("C=9 is not before F=5: %+v", got)
		}
	})
	for _, tc := range []struct {
		name   string
		cutoff int64
		obs    string
	}{
		{"a cutoff left at its zero", 0, ""},
		{"a negative cutoff", -1, ""},
	} {
		t.Run(tc.name+" is reported as proven, which is the documented limit", func(t *testing.T) {
			got := p4offline.ProveCommonCutoff(tc.cutoff, tc.obs, calls, coverage)
			if !got.Proven || got.Reason != p4offline.BoundaryProven {
				t.Fatalf("the documented behaviour is BOUNDARY_PROVEN; if this now refuses, the seam changed and BoundaryProof's doc must change with it: %+v", got)
			}
		})
	}
	t.Run("the honest verdict exists but the predicate never returns it", func(t *testing.T) {
		// CUTOFF_UNKNOWN is the caller's to record. If ProveCommonCutoff ever
		// starts returning it, this fires and the limitation is closed.
		for _, cutoff := range []int64{-1, 0, 9} {
			if got := p4offline.ProveCommonCutoff(cutoff, "", calls, coverage); got.Reason == p4offline.BoundaryCutoffUnknown {
				t.Fatalf("the predicate now names CUTOFF_UNKNOWN at cutoff %d; update BoundaryProof's doc and drop this test", cutoff)
			}
		}
	})
}

// TestQualityRecordMethodsDoNotModifyTheReceiver pins the value semantics the
// two exported ladder methods document. Discarding either result compiles and
// `go vet` is silent, and the record then stays at the TOP of the ladder --
// the inverse of its safety property -- so the obligation to assign is stated
// on the methods and checked here.
// WHAT ACTUALLY CATCHES DRIFT HERE IS THE COMPILER, not these assertions, and
// saying so is the point. A value receiver on a fresh record with nil slices
// cannot modify its caller's copy under ANY body, so no single-token change
// makes the assertions below fire. What does fire is the fix doc.go names for
// this sharp edge: rewriting Downgrade with a pointer receiver breaks the build
// at every call site that ranges over records by value. The test is kept as the
// statement of the contract the doc comment makes, not as its discriminator.
func TestQualityRecordMethodsDoNotModifyTheReceiver(t *testing.T) {
	rec := p4offline.NewQualityRecord()
	if rec.Quality != p4offline.QualityPrimaryScorable {
		t.Fatalf("a new record starts at the top: %+v", rec)
	}
	lowered := rec.Downgrade(p4offline.QualityExcluded, "BOUNDARY_NOT_PROVEN")
	if rec.Quality != p4offline.QualityPrimaryScorable || len(rec.Reasons) != 0 {
		t.Fatalf("Downgrade modified its receiver; the doc says it does not: %+v", rec)
	}
	if lowered.Quality != p4offline.QualityExcluded {
		t.Fatalf("the RETURNED record carries the downgrade: %+v", lowered)
	}
	merged := rec.Merge(lowered)
	if rec.Quality != p4offline.QualityPrimaryScorable || len(rec.Reasons) != 0 {
		t.Fatalf("Merge modified its receiver; the doc says it does not: %+v", rec)
	}
	if merged.Quality != p4offline.QualityExcluded {
		t.Fatalf("the RETURNED record carries the lower quality: %+v", merged)
	}
}

// TestManualSignalsDoNotCostQuadraticInTheirOwnCount is the receipt for the
// second superlinear path found on this branch, and for a disposition this
// package had written down and got wrong.
//
// ManualSignals was accumulated through appendOnce, which scans the whole
// accumulated list on every call. An ObservationID is distinct per record in
// any well-formed dataset, so the scan deduplicated nothing and cost O(M^2) in
// the manual facts matched to one episode -- reached from the exported
// SelectEpisodes, and so from every exported function that takes a
// SourceDataset. The site's own comment concluded that a declared ingest
// ceiling was "the only real answer" to it. It was not.
//
// The assertion is a GROWTH ratio, never wall-clock: what distinguishes the
// defect is the exponent.
// WHAT THIS TEST DOES NOT COVER, for the same reason. It builds through
// singleEpisode(), which fails on more than one episode, so the OTHER factor is
// pinned at one: it measures the cost of building ONE episode's list and can say
// nothing about what the selection RETAINS across many. That is
// TestManualSignalsAreSharedNotRetainedPerEpisode's job.
func TestManualSignalsDoNotCostQuadraticInTheirOwnCount(t *testing.T) {
	build := func(manual int) predictioneval.SourceDataset {
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		for i := 0; i < manual; i++ {
			s.manualCall("r1", "e1", 50, 0)
		}
		return s.dataset()
	}
	measure := func(t *testing.T, ds predictioneval.SourceDataset) (uint64, int) {
		t.Helper()
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		sel, err := p4offline.SelectEpisodes(ds)
		runtime.ReadMemStats(&b)
		if err != nil {
			t.Fatal(err)
		}
		ep := singleEpisode(t, sel)
		return b.TotalAlloc - a.TotalAlloc, len(ep.ManualSignals)
	}

	smallAlloc, smallN := measure(t, build(2048))
	largeAlloc, largeN := measure(t, build(4096))
	t.Logf("2048 manual calls -> %d bytes (%d signals); 4096 -> %d bytes (%d signals)",
		smallAlloc, smallN, largeAlloc, largeN)

	if smallN != 2048 || largeN != 4096 {
		t.Fatalf("every distinct manual signal must be reported: %d / %d", smallN, largeN)
	}
	// Linear over a 2x input is ~2x; quadratic is ~4x.
	if ratio := float64(largeAlloc) / float64(smallAlloc); ratio > 3 {
		t.Fatalf("allocation grew %.1fx over a 2x input: the manual signals are being accumulated quadratically", ratio)
	}

	t.Run("and a repeated observation is still reported once", func(t *testing.T) {
		// The deduplication is the property appendOnce was there for, and it
		// has to survive the change. The producer cannot write a duplicate
		// observation id, so this is built by hand.
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		dup := s.fact(predictioneval.KindPlacement, predictioneval.PhaseCallStarted, "r1", "e1", 0)
		dup.Payload.Manual = ptrBool(true)
		dup.ObservationID = "manual-twice"
		s.add(dup)
		again := dup
		again.CollectorSequence = s.next()
		s.add(again)

		sel, err := p4offline.SelectEpisodes(s.dataset())
		if err != nil {
			t.Fatal(err)
		}
		ep := singleEpisode(t, sel)
		seen := 0
		for _, id := range ep.ManualSignals {
			if id == "manual-twice" {
				seen++
			}
		}
		if seen != 1 {
			t.Fatalf("one observation id is reported once, got %d in %v", seen, ep.ManualSignals)
		}
	})
}

// TestSessionRefusalsAreNamedByKindNotOncePerRecord pins the two properties of
// the session-refusal rendering: it must not grow with the caller's record
// count, and the kinds it names must be in a deterministic order.
//
// sessionRefusals appends SESSION_P2_EXCLUSION once per session-level
// exclusion, and materialization emits one per foreign-session record, so the
// list's LENGTH is the caller's dataset even though every member is a short
// constant. Rendering it produced an 840,098-byte refusal for 20,000 records.
// The refusal now names the extent and the distinct kinds.
func TestSessionRefusalsAreNamedByKindNotOncePerRecord(t *testing.T) {
	build := func(n int) predictioneval.SourceDataset {
		s := newSynth()
		s.placedAttempt("r1", "e1", 1)
		ds := s.dataset()
		for i := 0; i < n; i++ {
			r := s.fact(predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, "r9", "e9", 0)
			r.CollectorSessionID = "a-foreign-session"
			r.ObservationID = ""
			ds.Records = append(ds.Records, r)
		}
		return ds
	}
	errOf := func(n int) error {
		_, err := p4offline.BuildCommonFactset(build(n), p4offline.EpisodeIdentity{})
		return err
	}
	e1, e2 := errOf(64), errOf(4096)
	if e1 == nil || e2 == nil {
		t.Fatalf("a foreign-session dataset must be refused: %v / %v", e1, e2)
	}
	t.Logf("64 foreign records -> %d-byte refusal; 4096 -> %d-byte refusal", len(e1.Error()), len(e2.Error()))

	// A 64x increase in the caller's records must not grow the refusal.
	if len(e2.Error()) > len(e1.Error())+64 {
		t.Fatalf("the refusal grew from %d to %d bytes with the caller's record count: %q",
			len(e1.Error()), len(e2.Error()), e2.Error())
	}
	if !strings.Contains(e2.Error(), "reasons,") || !strings.Contains(e2.Error(), "bytes") {
		t.Fatalf("the refusal must name the extent: %v", e2)
	}
}

// TestP2ExclusionAttributionIsCompleteForEveryEpisode is the receipt for the THIRD
// superlinear accumulation found on this seam, and it is renamed because the
// name it used to carry asserted something it could not see.
//
// WHAT IT USED TO CLAIM. It was called ...IsNotQuadraticInTheDataset, and an
// auditor reads that name as proof. Review measured its own fixture: every row
// was built on the same RoundIncarnationID, so it yielded exactly ONE episode at
// k = 2,000 / 4,000 / 8,000 / 16,000 / 32,000. The episode factor was pinned at
// one by construction, and the exact pre-repair nested rescan, reinstated as a
// well-formed mutation whose reachedness its own subtest confirmed, left it and
// the whole package suite green. It asserted the negation of a fact that was
// true.
//
// WHAT THE SEAM REALLY COSTS, measured through the exported SelectEpisodes with
// one undecodable AUTO_DUE PER round incarnation, so every row is its own
// episode AND its own producer exclusion. FOUR code states, not three: rescan
// and indexed are earlier rounds' figures, "indexed" refusing at 16,000 because
// the visit ceiling was still in force then; walk and aggregates were measured
// against each other on one machine in one sitting and are the pair to read:
//
//	records     rescan     indexed    walk      aggregates
//	  2,000     0.128 s    0.069 s    0.077 s   0.011 s
//	  4,000     0.424 s    0.216 s    0.229 s   0.027 s
//	  8,000     1.621 s    0.799 s    0.883 s   0.057 s
//	 16,000     6.287 s    refused    3.481 s   0.074 s
//	 32,000    14.254 s    refused   13.939 s   0.217 s
//
// The index halved it and did not make it linear, because it was never the only
// quadratic on that shape: the matching RELATION is itself quadratic here, and
// walking it was 94.71% cumulative on a profile. The package briefly REFUSED
// the shape for that reason. It no longer does: what each of those matches
// established was episode-INDEPENDENT, so it is established once per posting
// list and read per episode. The walk's own ratios over the four doublings are
// 2.97, 3.86, 3.94 and 4.00 -- quadratic once the fixed cost stops dominating;
// the aggregates' are 2.45, 2.11, 1.30 and 2.93, which is noise around linear
// rather than a clean ladder, and is reported as such. At 32,000 the two differ
// by 64x. See TestQuadraticMatchingShapeIsProcessedRatherThanRefused.
//
// WHAT THAT IS NOT. It is one shape, and it is the shape with no
// episode-dependent work at all. On the shape whose work IS the output -- k
// episodes each REPORTING k manual entries -- the two trees measure the same,
// 0.155 s against 0.167 s at 1,048,576 entries, because nothing there was
// repeated. That work is bounded by the output budget, not removed by this.
//
// SO THIS TEST ASSERTS THE ANSWER, WHICH IS ALL IT CAN, AND IS NAMED FOR THAT.
// It was briefly renamed ...IsIndexedNotRescanned, which is the same defect the
// old name had -- a name claiming a property the body does not check. Executed:
// the pre-repair rescan, reinstated as a well-formed mutant with the index kept
// alive so nothing is optimised away, leaves this test and the whole suite
// green, because the rescan computes the SAME attribution. The cost repair is
// argued at its site with the measurements, in the convention this package uses
// for every CPU-only change; what is PINNED here is that every episode gets its
// own correct answer, which the old fixture could not check because it only ever
// produced one episode.
func TestP2ExclusionAttributionIsCompleteForEveryEpisode(t *testing.T) {
	// K automatic rows naming one attempt that never gets a terminal envelope,
	// so the first opportunity is unusable and the attribution runs; each row is
	// also producer-excluded, so both operands grow together.
	build := func(k int) predictioneval.SourceDataset {
		s := newSynth()
		for i := 0; i < k; i++ {
			// ONE EPISODE PER ROW, which is the whole correction: on the old
			// fixture every row shared "r1", so there was one episode at
			// every size and the product this seam is about never formed.
			r := s.fact(predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue,
				"r"+strconv.Itoa(i), "e1", 1)
			r.PayloadUndecodable = true
			s.add(r)
		}
		return s.dataset()
	}
	measure := func(t *testing.T, ds predictioneval.SourceDataset) uint64 {
		t.Helper()
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		if _, err := p4offline.SelectEpisodes(ds); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&b)
		return b.TotalAlloc - a.TotalAlloc
	}

	small := measure(t, build(2048))
	large := measure(t, build(4096))
	t.Logf("2048 rows -> %d bytes; 4096 -> %d bytes", small, large)
	if ratio := float64(large) / float64(small); ratio > 3 {
		t.Fatalf("allocation grew %.1fx over a 2x input: the attribution is rescanning per exclusion", ratio)
	}

	t.Run("and the attribution is unchanged, for every episode", func(t *testing.T) {
		// The index must not change WHICH reasons are attributed, and it must
		// not change them for ANY episode -- which is what the old subtest could
		// not check, because it asserted through singleEpisode() and the old
		// fixture only ever produced one. Each row here is its own episode and
		// its own producer exclusion, so each must carry exactly its own.
		const n = 8
		sel, err := p4offline.SelectEpisodes(build(n))
		if err != nil {
			t.Fatal(err)
		}
		if len(sel.Episodes) != n {
			t.Fatalf("the fixture must produce one episode per row: got %d, want %d", len(sel.Episodes), n)
		}
		for i := range sel.Episodes {
			got := sel.Episodes[i].P2Exclusions
			if len(got) != 1 || got[0] != predictioneval.ExclusionPayloadUndecodable {
				t.Fatalf("episode %d must carry exactly its own refusal, got %v", i, got)
			}
		}
	})
}

// registryStringSites walks a registry by reflection and returns a POINTER to
// it beside every string position reachable from it. The pointer is the point:
// the sites index into the value this function holds, so returning the registry
// by value would hide the mutation of every field not behind a pointer or a
// slice -- which is how an earlier draft of the factset sibling passed a
// position it had never poked.
//
// Its blind spots, written down rather than left to be discovered: no map and
// no interface is descended (SourceRoundRegistry has neither), and an EMPTY
// slice contributes nothing, so a fixture must carry the entries and claims it
// wants covered. A nil Canonical is allocated in place so the canonical claim's
// strings are reached, which is why the fixture below is UNIQUE.
func registryStringSites(reg p4offline.SourceRoundRegistry) (*p4offline.SourceRoundRegistry, []floatSite) {
	var out []floatSite
	reachableStrings(reflect.ValueOf(&reg).Elem(), "SourceRoundRegistry", &out)
	return &reg, out
}

// registryCarriesOnlyExpressibleText reports the first string position in a
// registry that its own encoding cannot carry unchanged.
//
// IT WALKS READ-ONLY, and that is not a stylistic preference. reachableStrings
// ALLOCATES a nil pointer in place so the value behind it can be poked, which
// is right for a poking test and wrong for an inspecting one: SourceRoundEntry
// is reached through a slice, so the entries are shared however the registry
// itself is passed, and an inspection built on that walker gave every INVALID
// entry a Canonical claim it had not been minted with -- the instrument
// changing the value it was measuring. The sibling trap, one round earlier,
// was a walker whose sites pointed at a COPY; both are the same mistake about
// what reflection is holding.
func registryCarriesOnlyExpressibleText(reg p4offline.SourceRoundRegistry) (string, bool) {
	var walk func(v reflect.Value, path string) (string, bool)
	walk = func(v reflect.Value, path string) (string, bool) {
		switch v.Kind() {
		case reflect.String:
			return path, utf8.ValidString(v.String())
		case reflect.Pointer:
			if v.IsNil() {
				return "", true
			}
			return walk(v.Elem(), path+".*")
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				if at, ok := walk(v.Index(i), path+"["+strconv.Itoa(i)+"]"); !ok {
					return at, false
				}
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).PkgPath != "" {
					continue
				}
				if at, ok := walk(v.Field(i), path+"."+v.Type().Field(i).Name); !ok {
					return at, false
				}
			}
		}
		return "", true
	}
	return walk(reflect.ValueOf(reg), "SourceRoundRegistry")
}

// TestReconcileSourceRoundsNeverMintsARegistryItsOwnVerifierRefuses is the
// PRODUCER half of the encoding class, and it is the third producer in this
// package to need it.
//
// The class was closed at VerifyCommonFactset first, then -- after an external
// lane pointed out that gating the verifier leaves the package's own exported
// projector free to mint what that verifier refuses -- at ProjectResolution.
// doc.go then said this registry was "closed by CONSTRUCTION", which was an
// argument and not a measurement: running it showed ReconcileSourceRounds
// minting a registry that VERIFIED, marshalled without error, and failed its
// own re-derivation after the round trip. Instance, class, and then the class
// again: the lesson is the sweep, not any one of the three repairs.
//
// The assertion is the whole chain a certificate is supposed to survive --
// verify, marshal, read back, verify again -- plus the direct statement that
// the minted registry carries no byte it cannot carry, which is the property
// the chain is evidence FOR.
func TestReconcileSourceRoundsNeverMintsARegistryItsOwnVerifierRefuses(t *testing.T) {
	const invalid = "\xff\xfe\x80"
	// A VALID multi-byte control, already containing U+FFFD, so this cannot be
	// passing by refusing the substitution it exists to prevent.
	const validWide = "ok-\u00e9-\uFFFD"

	good := p4offline.SourceRoundClaim{
		Episode:       p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", RoundIncarnationID: "r", EventID: "e1"},
		Attempt:       predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", AttemptID: 1},
		FactsetDigest: "d1",
	}
	// Every string a claim holds, named by hand rather than walked, because
	// this list is the CONTRACT the routing is written against: a field added
	// to SourceRoundClaim and not added here is a position this test stops
	// covering, and the count assertion below is what makes that visible.
	poke := map[string]func(*p4offline.SourceRoundClaim, string){
		"Episode.CollectorSessionID": func(c *p4offline.SourceRoundClaim, v string) { c.Episode.CollectorSessionID = v },
		"Episode.PoolInstanceID":     func(c *p4offline.SourceRoundClaim, v string) { c.Episode.PoolInstanceID = v },
		"Episode.RoundIncarnationID": func(c *p4offline.SourceRoundClaim, v string) { c.Episode.RoundIncarnationID = v },
		"Episode.EventID":            func(c *p4offline.SourceRoundClaim, v string) { c.Episode.EventID = v },
		"Attempt.CollectorSessionID": func(c *p4offline.SourceRoundClaim, v string) { c.Attempt.CollectorSessionID = v },
		"Attempt.PoolInstanceID":     func(c *p4offline.SourceRoundClaim, v string) { c.Attempt.PoolInstanceID = v },
		"FactsetDigest":              func(c *p4offline.SourceRoundClaim, v string) { c.FactsetDigest = v },
	}
	if _, sites := registryStringSites(p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{good})); len(sites) != 18 {
		var paths []string
		for _, site := range sites {
			paths = append(paths, site.path)
		}
		t.Fatalf("a one-claim UNIQUE registry reaches %d string positions, want 18: %s", len(sites), strings.Join(paths, ", "))
	}

	for name, set := range poke {
		t.Run(name, func(t *testing.T) {
			c := good
			set(&c, invalid)
			reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{c})
			if err := p4offline.VerifySourceRoundRegistry(reg); err != nil {
				t.Fatalf("the registry this package minted does not verify: %v", err)
			}
			if at, ok := registryCarriesOnlyExpressibleText(reg); !ok {
				t.Fatalf("the minted registry still carries text it cannot express, at %s", at)
			}
			// The claim is not dropped: it is COUNTED, as one INVALID entry
			// that still NAMES THE ROUND IT SPOKE ABOUT. A denominator-
			// disciplined package does not get to make a fact it cannot carry
			// disappear, and an earlier version of this repair erased the round
			// name here -- which let one invalid byte dissolve a CONFLICT and
			// left the registry with no trace that the round had been contested.
			wantEvent := "e1"
			if name == "Episode.EventID" {
				wantEvent = "" // the round name itself was the unrepresentable string
			}
			if len(reg.Entries) != 1 || reg.Entries[0].Status != p4offline.SourceRoundInvalid ||
				reg.Entries[0].EventID != wantEvent || len(reg.Entries[0].Claims) != 1 || reg.Entries[0].Canonical != nil {
				t.Fatalf("want one INVALID entry naming round %q and claiming nothing, got %+v", wantEvent, reg.Entries)
			}
			if reg.Entries[0].Claims[0].FactsetDigest != "" {
				t.Fatalf("the recorded claim must have lost its evidence identity: %+v", reg.Entries[0].Claims[0])
			}
			raw, err := json.Marshal(reg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var back p4offline.SourceRoundRegistry
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if err := p4offline.VerifySourceRoundRegistry(back); err != nil {
				t.Fatalf("a registry this package minted and certified failed its own re-derivation after a JSON round trip: %v", err)
			}
			if !reflect.DeepEqual(back, reg) {
				t.Fatalf("the registry did not survive its own round trip unchanged:\n before %+v\n  after %+v", reg, back)
			}
		})
	}

	// THE ORDERING, which no other case here can reach. The routing runs its
	// O(len) expressibility test AHEAD of the two O(1) ones, against this
	// package's usual rule, because the cheap clauses RECORD the claim verbatim
	// rather than merely refusing it. A claim that trips both -- no round name
	// AND text that cannot be carried -- is the only shape that tells the two
	// orders apart, and with the cheap clause first the registry keeps the
	// bytes while still calling the claim INVALID: refused and unwritable, the
	// worst of both.
	t.Run("a claim that is both unnameable and uncarriable", func(t *testing.T) {
		c := good
		c.Episode.EventID = ""
		c.Attempt.PoolInstanceID = invalid
		reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{c})
		if at, ok := registryCarriesOnlyExpressibleText(reg); !ok {
			t.Fatalf("the minted registry still carries text it cannot express, at %s", at)
		}
		if err := p4offline.VerifySourceRoundRegistry(reg); err != nil {
			t.Fatalf("the registry this package minted does not verify: %v", err)
		}
		if len(reg.Entries) != 1 || reg.Entries[0].Status != p4offline.SourceRoundInvalid {
			t.Fatalf("%+v", reg.Entries)
		}
	})

	// The control, per position: a VALID wide string at the same position is
	// reconciled as evidence, not routed INVALID. Without it this test would
	// still pass if the routing refused every claim.
	for name, set := range poke {
		t.Run("valid wide text at "+name, func(t *testing.T) {
			c := good
			set(&c, validWide)
			reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{c})
			if len(reg.Entries) != 1 || reg.Entries[0].Status != p4offline.SourceRoundUnique || reg.Entries[0].Canonical == nil {
				t.Fatalf("a valid multi-byte string is evidence: want one UNIQUE entry with a canonical claim, got %+v", reg.Entries)
			}
			if *reg.Entries[0].Canonical != c {
				t.Fatalf("the canonical claim was altered: %+v, want %+v", *reg.Entries[0].Canonical, c)
			}
			// And the VERIFIER's gate does not refuse it either. A gate that
			// refused a valid multi-byte identity would be refusing the very
			// U+FFFD substitution it exists to prevent.
			if err := p4offline.VerifySourceRoundRegistry(reg); err != nil {
				t.Fatalf("a registry carrying valid multi-byte text was refused: %v", err)
			}
		})
	}
}

// TestARegistryMixingCarriableAndUncarriableClaimsKeepsBothCounted covers the
// shapes one claim cannot: a registry holding BOTH kinds, and two different
// uncarriable claims that blank to the same value.
//
// The second is the case the repair deliberately accepts. Dropping a claim's
// text loses what distinguished it from another claim broken the same way, so
// two of them alias -- but they alias into INVALID, which stands for no round
// and carries no canonical claim, and they are still recorded one entry each.
// That is the whole reason the text is dropped rather than the claim: a
// package that reports a denominator does not get to make a fact it cannot
// write down disappear. Blanking only the OFFENDING string would have moved
// the same aliasing into the CANONICAL position instead, where it would mean
// two distinct pieces of evidence deduplicating into one.
func TestARegistryMixingCarriableAndUncarriableClaimsKeepsBothCounted(t *testing.T) {
	const invalid = "\xff\xfe\x80"
	good := p4offline.SourceRoundClaim{
		Episode:       p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", RoundIncarnationID: "r", EventID: "e1"},
		Attempt:       predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", AttemptID: 1},
		FactsetDigest: "d1",
	}
	settles := func(t *testing.T, reg p4offline.SourceRoundRegistry) {
		t.Helper()
		if err := p4offline.VerifySourceRoundRegistry(reg); err != nil {
			t.Fatalf("the registry this package minted does not verify: %v", err)
		}
		if at, ok := registryCarriesOnlyExpressibleText(reg); !ok {
			t.Fatalf("the minted registry still carries text it cannot express, at %s", at)
		}
		var back p4offline.SourceRoundRegistry
		if err := json.Unmarshal(mustMarshal(t, reg), &back); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if err := p4offline.VerifySourceRoundRegistry(back); err != nil {
			t.Fatalf("the registry failed its own re-derivation after a JSON round trip: %v", err)
		}
		if !reflect.DeepEqual(back, reg) {
			t.Fatalf("the registry did not survive its own round trip unchanged:\n before %+v\n  after %+v", reg, back)
		}
	}

	t.Run("a carriable claim beside an uncarriable one", func(t *testing.T) {
		bad := good
		bad.Episode.RoundIncarnationID = invalid
		bad.Episode.EventID = "e2"
		reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{good, bad})
		settles(t, reg)
		if len(reg.Entries) != 2 {
			t.Fatalf("both claims must be recorded: %+v", reg.Entries)
		}
		if reg.Entries[0].EventID != "e1" || reg.Entries[0].Status != p4offline.SourceRoundUnique ||
			reg.Entries[0].Canonical == nil || *reg.Entries[0].Canonical != good {
			t.Fatalf("the carriable claim is still evidence for its own round: %+v", reg.Entries[0])
		}
		if reg.Entries[1].Status != p4offline.SourceRoundInvalid || reg.Entries[1].EventID != "e2" {
			t.Fatalf("the uncarriable claim must still name the round it spoke about: %+v", reg.Entries[1])
		}
		// It named e2, and keeping that name must not let it be read as a claim
		// on e1 -- nor let it be read as EVIDENCE about e2.
		if len(reg.Entries[1].Claims) != 1 || reg.Entries[1].Claims[0].Episode.EventID != "e2" ||
			reg.Entries[1].Claims[0].FactsetDigest != "" {
			t.Fatalf("want one claim naming e2 with no evidence identity, got %+v", reg.Entries[1])
		}
	})

	t.Run("two uncarriable claims that blank to the same value", func(t *testing.T) {
		a, b := good, good
		a.Episode.EventID = invalid + "a"
		b.Episode.EventID = invalid + "b"
		if a == b {
			t.Fatal("the fixture must supply two DIFFERENT claims")
		}
		reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{a, b})
		settles(t, reg)
		if len(reg.Entries) != 2 {
			t.Fatalf("two claims, two entries, whether or not they still differ: %+v", reg.Entries)
		}
		for i, e := range reg.Entries {
			if e.Status != p4offline.SourceRoundInvalid || e.Canonical != nil || len(e.Claims) != 1 {
				t.Fatalf("entry %d: want INVALID, one claim and no canonical claim, got %+v", i, e)
			}
		}
		// They differ only in their (unrepresentable) round names, so once those
		// are dropped they alias -- and BOTH are INVALID, standing for no round
		// and carrying no canonical claim. That is the aliasing this repair
		// accepts; what it does NOT accept is aliasing in the canonical
		// position, which is why blanking only the offending string was
		// rejected.
		if reg.Entries[0].Claims[0] != reg.Entries[1].Claims[0] {
			t.Fatalf("the two claims are expected to alias once their text is dropped: %+v", reg.Entries)
		}
	})
}

// TestAClaimThisPackageCannotReconcileContestsItsRoundRatherThanLeavingIt is
// the regression test for the worst defect this branch has produced, and it is
// worth reading as a lesson rather than a case list.
//
// The repair for the encoding class routed an uncarriable claim to INVALID with
// its ROUND NAME dropped. That looked like the honest reduction -- a claim whose
// text cannot be carried names no round it can carry -- and it was a fail-open
// on the one axis this package declares fail-closed. A claim with no round name
// leaves its round's GROUP, so appending ONE invalid byte to a competing claim
// turned a CONFLICT, which has no canonical claim and counts for nobody, into a
// UNIQUE whose canonical claim was the other, honest one. The registry verified,
// round-tripped, and kept no trace that the round had ever been contested. Two
// independent review lanes reproduced it; the writer did not.
//
// The trade the first version made was fixed-point property over round
// association. It was avoidable: dropping the DIGEST instead routes the claim
// to INVALID just as well and keeps the round name, and a round some claim spoke
// about unreconcilably is then held fail-closed on its own evidence.
//
// The digestless claim takes the same path, and always could have. That case is
// covered here too because the rule is one rule: this package treats "I cannot
// read what this claim said about round R" identically however it came about.
func TestAClaimThisPackageCannotReconcileContestsItsRoundRatherThanLeavingIt(t *testing.T) {
	const invalid = "\xff\xfe\x80"
	honestA := p4offline.SourceRoundClaim{
		Episode:       p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "sA", PoolInstanceID: "pA", RoundIncarnationID: "rA", EventID: "E1"},
		Attempt:       predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: "sA", PoolInstanceID: "pA", AttemptID: 1},
		FactsetDigest: "dA",
	}
	honestB := p4offline.SourceRoundClaim{
		Episode:       p4offline.EpisodeIdentity{CollectorEpoch: 2, CollectorSessionID: "sB", PoolInstanceID: "pB", RoundIncarnationID: "rB", EventID: "E1"},
		Attempt:       predictioneval.AttemptKey{CollectorEpoch: 2, CollectorSessionID: "sB", PoolInstanceID: "pB", AttemptID: 7},
		FactsetDigest: "dB",
	}
	canonicalOf := func(reg p4offline.SourceRoundRegistry, event string) *p4offline.SourceRoundClaim {
		for _, e := range reg.Entries {
			if e.EventID == event && e.Canonical != nil {
				return e.Canonical
			}
		}
		return nil
	}

	// The baseline this must not be allowed to differ from: two honest claims
	// that disagree are a CONFLICT and nothing is canonical.
	base := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{honestA, honestB})
	if len(base.Entries) != 1 || base.Entries[0].Status != p4offline.SourceRoundConflict || canonicalOf(base, "E1") != nil {
		t.Fatalf("two disagreeing claims on one round are a CONFLICT with no canonical claim: %+v", base.Entries)
	}

	for _, tc := range []struct {
		name   string
		break_ func(p4offline.SourceRoundClaim) p4offline.SourceRoundClaim
	}{
		{"one invalid byte in the competing claim's pool instance id", func(c p4offline.SourceRoundClaim) p4offline.SourceRoundClaim {
			c.Episode.PoolInstanceID += invalid
			return c
		}},
		{"one invalid byte in the competing claim's collector session id", func(c p4offline.SourceRoundClaim) p4offline.SourceRoundClaim {
			c.Episode.CollectorSessionID += invalid
			return c
		}},
		{"one invalid byte in the competing claim's factset digest", func(c p4offline.SourceRoundClaim) p4offline.SourceRoundClaim {
			c.FactsetDigest += invalid
			return c
		}},
		{"the competing claim carries no factset digest at all", func(c p4offline.SourceRoundClaim) p4offline.SourceRoundClaim {
			c.FactsetDigest = ""
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{honestA, tc.break_(honestB)})
			if err := p4offline.VerifySourceRoundRegistry(reg); err != nil {
				t.Fatalf("the registry this package minted does not verify: %v", err)
			}
			if at, ok := registryCarriesOnlyExpressibleText(reg); !ok {
				t.Fatalf("the minted registry still carries text it cannot express, at %s", at)
			}
			// THE ASSERTION THIS TEST EXISTS FOR.
			if c := canonicalOf(reg, "E1"); c != nil {
				t.Fatalf("a claim this package could not reconcile handed E1 a canonical claim: %+v", *c)
			}
			for _, e := range reg.Entries {
				if e.EventID == "E1" && e.Status != p4offline.SourceRoundConflict && e.Status != p4offline.SourceRoundInvalid {
					t.Fatalf("E1 must stay fail-closed, got %v: %+v", e.Status, e)
				}
			}
			// And the round the unreadable claim spoke about is still visible,
			// which is what makes the suppression auditable rather than silent.
			named := false
			for _, e := range reg.Entries {
				if e.Status == p4offline.SourceRoundInvalid && e.EventID == "E1" {
					named = true
				}
			}
			if !named {
				t.Fatalf("the registry keeps no trace that E1 was contested: %+v", reg.Entries)
			}
			var back p4offline.SourceRoundRegistry
			if err := json.Unmarshal(mustMarshal(t, reg), &back); err != nil {
				t.Fatal(err)
			}
			if err := p4offline.VerifySourceRoundRegistry(back); err != nil {
				t.Fatalf("the registry failed its own re-derivation after a JSON round trip: %v", err)
			}
		})
	}

	// THE CARVE-OUT, PINNED AS AN EXCEPTION RATHER THAN LEFT TO BE FOUND. The
	// four cases above all leave the claim NAMING its round. When the unreadable
	// string is the round name ITSELF there is no round left to name, so the
	// claim contests nothing and a round whose only competitor was corrupted
	// that way keeps its canonical claim. A review lane graded that a blocker;
	// it stands, and the assertion below is the argument in executable form:
	// corrupting the round name reaches EXACTLY what withholding the claim
	// reaches, and no reconciler can detect withholding. If that ever stops
	// being true, this test fails and the carve-out gets re-decided instead of
	// quietly widening.
	t.Run("a claim whose round NAME is unreadable names no round, so it contests none", func(t *testing.T) {
		poisoned := honestB
		poisoned.Episode.EventID += invalid
		got := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{honestA, poisoned})
		withheld := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{honestA})
		if err := p4offline.VerifySourceRoundRegistry(got); err != nil {
			t.Fatalf("verify: %v", err)
		}
		if at, ok := registryCarriesOnlyExpressibleText(got); !ok {
			t.Fatalf("the minted registry still carries text it cannot express, at %s", at)
		}
		// E1 keeps its canonical claim -- the fail-open half, stated out loud.
		// The expectation is written down rather than taken from the other run:
		// comparing two calls of the same function to each other would pin only
		// that it is consistent with itself, which a review lane pointed out. The
		// equality against `withheld` is asserted too, because equivalence to
		// withholding is the ARGUMENT for the carve-out, but honestA is the
		// independent oracle.
		gotC, withheldC := canonicalOf(got, "E1"), canonicalOf(withheld, "E1")
		if gotC == nil || *gotC != honestA {
			t.Fatalf("E1 must keep exactly the claim it would have kept: got %+v, want %+v", gotC, honestA)
		}
		if withheldC == nil || *withheldC != honestA {
			t.Fatalf("the withheld-claim baseline is not what this test assumes: %+v", withheld.Entries)
		}
		// And the ONLY difference from withholding is the extra INVALID row, so
		// corruption is strictly more visible than withholding, never less.
		if len(got.Entries) != len(withheld.Entries)+1 {
			t.Fatalf("want exactly one extra entry over withholding, got %d against %d: %+v", len(got.Entries), len(withheld.Entries), got.Entries)
		}
		extra := got.Entries[len(got.Entries)-1]
		if extra.Status != p4offline.SourceRoundInvalid || extra.EventID != "" || extra.Canonical != nil {
			t.Fatalf("the extra entry must be an INVALID one naming no round: %+v", extra)
		}
	})

	// THE NO-FALSE-REFUSAL CONTROL, and it is the half that makes the rule a
	// rule rather than a blanket refusal: an unreadable claim about a DIFFERENT
	// round leaves this one alone, and an honest round on its own still gets its
	// canonical claim.
	t.Run("an unreadable claim about another round does not contest this one", func(t *testing.T) {
		// E2 CARRIES AN HONEST CLAIM OF ITS OWN, and that is the correction a
		// review lane made to this case. Without it, E2's only claim was the
		// unreadable one, so "E2 has no canonical claim" was true whatever the
		// code did -- it stayed green even with roundsWithUnreconcilableClaims
		// returning nil. A guard that holds either way pins nothing.
		honestE2 := honestA
		honestE2.Episode.EventID = "E2"
		honestE2.Episode.RoundIncarnationID = "r2"
		elsewhere := honestB
		elsewhere.Episode.EventID = "E2"
		elsewhere.Episode.PoolInstanceID += invalid
		reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{honestA, honestE2, elsewhere})
		if err := p4offline.VerifySourceRoundRegistry(reg); err != nil {
			t.Fatalf("verify: %v", err)
		}
		c := canonicalOf(reg, "E1")
		if c == nil || *c != honestA {
			t.Fatalf("E1 is uncontested and must keep its canonical claim: %+v", reg.Entries)
		}
		// E2 WOULD have a canonical claim on its honest claim alone; it must not
		// keep one once an unreadable claim names it.
		alone := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{honestE2})
		if a := canonicalOf(alone, "E2"); a == nil || *a != honestE2 {
			t.Fatalf("the fixture must give E2 a claim that WOULD be canonical: %+v", alone.Entries)
		}
		if canonicalOf(reg, "E2") != nil {
			t.Fatalf("E2 was spoken about unreadably and must lose its canonical claim: %+v", reg.Entries)
		}
	})
	t.Run("an uncontested round keeps its canonical claim", func(t *testing.T) {
		reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{honestA})
		c := canonicalOf(reg, "E1")
		if c == nil || *c != honestA {
			t.Fatalf("%+v", reg.Entries)
		}
	})
}

// TestASuppliedRegistryCarryingTextItCannotExpressIsRefused is the VERIFIER
// half, and its history is the reason it asserts what it does.
//
// The producer repair came first, and on its own it looked sufficient: with
// ReconcileSourceRounds dropping what it cannot carry, and registryDigest
// unexported, there was no way left to obtain a CONSISTENT uncarriable
// registry, and a hand-edited one was already refused at every position.
//
// AN EARLIER, WEAKER FORM OF THIS TEST -- refusal only, no named position --
// did pass before checkRegistryTextExpressible existed, and a comment here
// claimed that of the CURRENT form too. A review lane refuted it by execution:
// with the gate removed, 16 of these 18 positions fail, at the assertion that
// the refusal must name what it refused. The weaker form pinned a standing
// property; this one pins the gate.
//
// Two independent external reviews asked for the verifier gate regardless, and
// the argument against it was the weak kind this branch keeps getting caught
// by: it rested on this package's exported SURFACE (no serializer, so no
// reachable digest) rather than on the artifact. So the gate went in, and what
// this test now pins is stronger than "refused" -- it is refused, and the
// refusal NAMES the position. Without that, one constant message would satisfy
// every case here, which is exactly the hole a lane demonstrated in the
// factset's sibling by replacing all of its labels with one.
//
// EVERY POSITION IS NAMED NOW, including the two that were not. At this
// commit's PARENT 0b3cd2f Version was refused by a constant comparison that said nothing about
// itself, and Digest by the digest COMPARISON, which did the same, so a table
// skipping them cost nothing. Both name themselves here, so a skip would decline to
// assert a property the code has -- and a revert of either refusal would go
// uncaught. Both are rows.
func TestASuppliedRegistryCarryingTextItCannotExpressIsRefused(t *testing.T) {
	const invalid = "\xff\xfe\x80"
	mint := func() p4offline.SourceRoundRegistry {
		return p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{{
			Episode:       p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", RoundIncarnationID: "r", EventID: "e1"},
			Attempt:       predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", AttemptID: 1},
			FactsetDigest: "d1",
		}})
	}
	if err := p4offline.VerifySourceRoundRegistry(mint()); err != nil {
		t.Fatalf("the unpoked control must verify: %v", err)
	}
	_, found := registryStringSites(mint())
	if want := 18; len(found) != want {
		var paths []string
		for _, site := range found {
			paths = append(paths, site.path)
		}
		t.Fatalf("reached %d string positions, want %d: %s", len(found), want, strings.Join(paths, ", "))
	}
	const p = "SourceRoundRegistry.Entries[0]"
	names := map[string]string{
		// THE REGISTRY'S OWN TWO, which had no rows while their clauses
		// returned the bare sentinel. The Version clause is refused above
		// everything, so it never reaches the text scan; the digest is held to
		// 64 lower-case hex, so an invalid byte is a shape fault. Both say
		// which position they refused now, and both are asserted here.
		"SourceRoundRegistry.Version":                 "registry version is",
		"SourceRoundRegistry.Digest":                  "registry digest of",
		p + ".EventID":                                "entry 0 event id",
		p + ".Status":                                 "entry 0 status",
		p + ".Claims[0].Episode.CollectorSessionID":   "entry 0 claim 0 episode collector session id",
		p + ".Claims[0].Episode.PoolInstanceID":       "entry 0 claim 0 episode pool instance id",
		p + ".Claims[0].Episode.RoundIncarnationID":   "entry 0 claim 0 episode round incarnation id",
		p + ".Claims[0].Episode.EventID":              "entry 0 claim 0 episode event id",
		p + ".Claims[0].Attempt.CollectorSessionID":   "entry 0 claim 0 attempt collector session id",
		p + ".Claims[0].Attempt.PoolInstanceID":       "entry 0 claim 0 attempt pool instance id",
		p + ".Claims[0].FactsetDigest":                "entry 0 claim 0 factset digest",
		p + ".Canonical.*.Episode.CollectorSessionID": "entry 0 canonical claim episode collector session id",
		p + ".Canonical.*.Episode.PoolInstanceID":     "entry 0 canonical claim episode pool instance id",
		p + ".Canonical.*.Episode.RoundIncarnationID": "entry 0 canonical claim episode round incarnation id",
		p + ".Canonical.*.Episode.EventID":            "entry 0 canonical claim episode event id",
		p + ".Canonical.*.Attempt.CollectorSessionID": "entry 0 canonical claim attempt collector session id",
		p + ".Canonical.*.Attempt.PoolInstanceID":     "entry 0 canonical claim attempt pool instance id",
		p + ".Canonical.*.FactsetDigest":              "entry 0 canonical claim factset digest",
	}
	if len(names) != len(found) {
		t.Fatalf("%d positions are named, against %d reached", len(names), len(found))
	}
	// AND WHY each position is refused. Most are refused for their ENCODING.
	// The registry's own two are refused ABOVE the text scan -- Version by a
	// constant comparison and Digest by its shape -- so an invalid byte there
	// never reaches the encoding question at all. They are still refused and
	// they still name their position, which is what this test is for; asserting
	// the encoding fault for them would assert a path they do not take.
	why := map[string]string{
		"SourceRoundRegistry.Version": "this package writes only",
		"SourceRoundRegistry.Digest":  "is not this package's 64 lower-case hex digits",
	}
	for i, site := range found {
		t.Run(site.path, func(t *testing.T) {
			reg, sites := registryStringSites(mint())
			sites[i].at.SetString(invalid)
			err := p4offline.VerifySourceRoundRegistry(*reg)
			if !errors.Is(err, p4offline.ErrSourceRoundRegistry) {
				t.Fatalf("invalid UTF-8 at %s was CERTIFIED (err %v)", site.path, err)
			}
			want, named := names[site.path]
			if !named {
				t.Fatalf("%s is reached by the walker and named by no row; every position "+
					"this registry can carry must say which one it refused", site.path)
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the refusal must name the position it refused:\n got %v\nwant a mention of %q", err, want)
			}
			wantWhy, ok := why[site.path]
			if !ok {
				wantWhy = "is not valid UTF-8"
			}
			if !strings.Contains(err.Error(), wantWhy) {
				t.Fatalf("the refusal must say WHY, not merely which (%q): %v", wantWhy, err)
			}
		})
	}
}

// TestTheRefusalNamesWhichEntryAndWhichClaim closes a hole the writer left and
// no review reported, and it is the same hole a lane demonstrated one round
// earlier in the factset's sibling gate.
//
// checkRegistryTextExpressible builds its message from strconv.Itoa(i) and
// strconv.Itoa(j) -- the entry index and the claim index. Every other test of
// that gate uses a registry with ONE entry holding ONE claim, so both indices
// are 0 whatever the code computes, and replacing either with the constant 0
// would leave the whole suite green. An index nothing pins is an index that can
// point anywhere.
//
// The fixture is a registry with TWO entries, one of them a CONFLICT holding
// two claims, which also reaches three cases nothing else here covers: an entry
// past index 0, a claim past index 0, and an entry whose Canonical is nil.
func TestTheRefusalNamesWhichEntryAndWhichClaim(t *testing.T) {
	const invalid = "\xff\xfe\x80"
	base := p4offline.SourceRoundClaim{
		Episode:       p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "s-a", PoolInstanceID: "p", RoundIncarnationID: "r", EventID: "e1"},
		Attempt:       predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: "s-a", PoolInstanceID: "p", AttemptID: 1},
		FactsetDigest: "d1",
	}
	// Two DIFFERENT claims on e1 -- a CONFLICT, so entry 0 holds two claims and
	// no canonical one -- and a third on e2, which is UNIQUE and does have one.
	other := base
	other.Episode.CollectorSessionID, other.Attempt.CollectorSessionID, other.FactsetDigest = "s-b", "s-b", "d9"
	third := base
	third.Episode.EventID, third.Episode.RoundIncarnationID = "e2", "r2"
	mint := func() p4offline.SourceRoundRegistry {
		return p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{base, other, third})
	}
	reg := mint()
	if len(reg.Entries) != 2 ||
		reg.Entries[0].EventID != "e1" || reg.Entries[0].Status != p4offline.SourceRoundConflict ||
		len(reg.Entries[0].Claims) != 2 || reg.Entries[0].Canonical != nil ||
		reg.Entries[1].EventID != "e2" || reg.Entries[1].Status != p4offline.SourceRoundUnique ||
		reg.Entries[1].Canonical == nil {
		t.Fatalf("the fixture must be a two-claim CONFLICT followed by a UNIQUE: %+v", reg.Entries)
	}
	if err := p4offline.VerifySourceRoundRegistry(reg); err != nil {
		t.Fatalf("the unpoked control must verify: %v", err)
	}
	// The claims inside an entry are sorted by key, so which of the two e1
	// claims sits at index 1 is not something this test gets to assume. It is
	// read off the minted registry -- the registry is the oracle for its own
	// ordering, and the CLAIM under test is which INDEX the message reports.
	for _, tc := range []struct {
		name string
		poke func(*p4offline.SourceRoundRegistry)
		want string
	}{
		{"the second claim of the first entry", func(r *p4offline.SourceRoundRegistry) {
			r.Entries[0].Claims[1].FactsetDigest = invalid
		}, "entry 0 claim 1 factset digest"},
		{"the first claim of the second entry", func(r *p4offline.SourceRoundRegistry) {
			r.Entries[1].Claims[0].Episode.PoolInstanceID = invalid
		}, "entry 1 claim 0 episode pool instance id"},
		{"the second entry's canonical claim", func(r *p4offline.SourceRoundRegistry) {
			r.Entries[1].Canonical.Attempt.CollectorSessionID = invalid
		}, "entry 1 canonical claim attempt collector session id"},
		{"the second entry's event id", func(r *p4offline.SourceRoundRegistry) {
			r.Entries[1].EventID = invalid
		}, "entry 1 event id"},
		{"the second entry's status", func(r *p4offline.SourceRoundRegistry) {
			r.Entries[1].Status = p4offline.SourceRoundStatus(invalid)
		}, "entry 1 status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			poked := mint()
			tc.poke(&poked)
			err := p4offline.VerifySourceRoundRegistry(poked)
			if !errors.Is(err, p4offline.ErrSourceRoundRegistry) {
				t.Fatalf("invalid UTF-8 at %s was CERTIFIED (err %v)", tc.name, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal must name WHICH entry and WHICH claim:\n got %v\nwant a mention of %q", err, tc.want)
			}
		})
	}

	// And the PRODUCER side of the same shape: an uncarriable claim among
	// claims that CONFLICT must not change what the conflict is, and the
	// registry must still survive its own round trip.
	t.Run("an uncarriable claim beside a conflict", func(t *testing.T) {
		bad := base
		bad.Attempt.PoolInstanceID = invalid
		reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{base, other, third, bad})
		if at, ok := registryCarriesOnlyExpressibleText(reg); !ok {
			t.Fatalf("the minted registry still carries text it cannot express, at %s", at)
		}
		if err := p4offline.VerifySourceRoundRegistry(reg); err != nil {
			t.Fatalf("the registry this package minted does not verify: %v", err)
		}
		if len(reg.Entries) != 3 {
			t.Fatalf("four claims, two rounds and one uncarriable claim make three entries: %+v", reg.Entries)
		}
		if reg.Entries[0].Status != p4offline.SourceRoundConflict || len(reg.Entries[0].Claims) != 2 {
			t.Fatalf("the uncarriable claim must not join or dissolve the conflict: %+v", reg.Entries[0])
		}
		if reg.Entries[2].Status != p4offline.SourceRoundInvalid || reg.Entries[2].EventID != "e1" {
			t.Fatalf("the uncarriable claim must still name e1, the round it contested: %+v", reg.Entries[2])
		}
		var back p4offline.SourceRoundRegistry
		if err := json.Unmarshal(mustMarshal(t, reg), &back); err != nil {
			t.Fatal(err)
		}
		if err := p4offline.VerifySourceRoundRegistry(back); err != nil {
			t.Fatalf("the registry failed its own re-derivation after a JSON round trip: %v", err)
		}
	})
}

// TestAConstantSizeRegistryPositionIsRefusedBeforeACallerSizedSlice pins the
// ordering checkRegistryTextExpressible calls load-bearing, which nothing pinned.
//
// The gate checks the entry's two constant-size strings and its single canonical
// claim BEFORE the caller-sized Claims slice -- factset.go's rule, whose sibling
// IS pinned (TestAConstantSizePositionIsRefusedBeforeACallerSizedSlice). Here it
// was backed by a measurement and by nothing executable: a mutation swapping the
// two survived the whole suite, found independently by this round's mutation
// campaign and by a review lane. Only an entry bad in BOTH positions tells the
// orders apart, and no test had one.
//
// The assertion is which fault is NAMED, because that is the observable
// consequence; the cost argument is what the order is FOR. A lane measured the
// inverted form at roughly fifty thousand times the work on a 200,000-claim
// entry -- the ratio reproduces, the absolute figures are fixture-dependent and
// so are not asserted here.
func TestAConstantSizeRegistryPositionIsRefusedBeforeACallerSizedSlice(t *testing.T) {
	const invalid = "\xff\xfe\x80"
	reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{{
		Episode:       p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", RoundIncarnationID: "r", EventID: "e1"},
		Attempt:       predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", AttemptID: 1},
		FactsetDigest: "d1"}})
	if err := p4offline.VerifySourceRoundRegistry(reg); err != nil {
		t.Fatalf("the unpoked control must verify: %v", err)
	}
	if reg.Entries[0].Canonical == nil {
		t.Fatalf("the fixture needs a canonical claim to poke: %+v", reg.Entries[0])
	}
	reg.Entries[0].Claims[0].Episode.PoolInstanceID = invalid
	reg.Entries[0].Canonical.Attempt.CollectorSessionID = invalid
	err := p4offline.VerifySourceRoundRegistry(reg)
	if !errors.Is(err, p4offline.ErrSourceRoundRegistry) {
		t.Fatalf("an entry bad in both positions was CERTIFIED (err %v)", err)
	}
	if !strings.Contains(err.Error(), "entry 0 canonical claim attempt collector session id") {
		t.Fatalf("the constant-size position must be refused first:\n got %v\nwant a mention of the canonical claim", err)
	}
	if strings.Contains(err.Error(), "claim 0 episode pool instance id") {
		t.Fatalf("the caller-sized slice was scanned before the constant-size position: %v", err)
	}
}

// TestAnUnreconcilableClaimContestsARoundTwoSessionsAGREEDOn is the arm the
// whole fail-closed rule left untouched, and it is the one where agreement
// makes the round look settled.
//
// Every case in the sibling test leaves the contested round's group holding ONE
// claim, so only the UNIQUE arm was ever reached. DEDUPLICATED_IDENTICAL is the
// shape this package calls the ordinary one -- "what a session loaded twice
// produces" -- and a review lane showed that scoping `contested` to single-claim
// groups survived the entire suite while turning exactly this shape back into a
// canonical claim.
func TestAnUnreconcilableClaimContestsARoundTwoSessionsAGREEDOn(t *testing.T) {
	const invalid = "\xff\xfe\x80"
	agreed := p4offline.SourceRoundClaim{
		Episode:       p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", RoundIncarnationID: "r", EventID: "E1"},
		Attempt:       predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", AttemptID: 1},
		FactsetDigest: "d1"}
	// The control: loaded twice and nothing else, this round IS settled.
	twice := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{agreed, agreed})
	if len(twice.Entries) != 1 || twice.Entries[0].Status != p4offline.SourceRoundDeduplicatedIdentical ||
		twice.Entries[0].Canonical == nil || *twice.Entries[0].Canonical != agreed {
		t.Fatalf("two identical claims collapse to one canonical claim: %+v", twice.Entries)
	}
	for _, tc := range []struct {
		name  string
		third p4offline.SourceRoundClaim
	}{
		{"whose text cannot be carried", func() p4offline.SourceRoundClaim {
			c := agreed
			c.Episode.PoolInstanceID += invalid
			return c
		}()},
		{"which carries no factset digest", func() p4offline.SourceRoundClaim {
			c := agreed
			c.FactsetDigest = ""
			return c
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{agreed, agreed, tc.third})
			if err := p4offline.VerifySourceRoundRegistry(reg); err != nil {
				t.Fatalf("verify: %v", err)
			}
			for _, e := range reg.Entries {
				if e.EventID == "E1" && e.Canonical != nil {
					t.Fatalf("a claim this package could not reconcile left E1 settled: %+v", e)
				}
				if e.EventID == "E1" && e.Status == p4offline.SourceRoundDeduplicatedIdentical {
					t.Fatalf("agreement between the readable claims does not settle a round a third claim contested: %+v", e)
				}
			}
			var back p4offline.SourceRoundRegistry
			if err := json.Unmarshal(mustMarshal(t, reg), &back); err != nil {
				t.Fatal(err)
			}
			if err := p4offline.VerifySourceRoundRegistry(back); err != nil {
				t.Fatalf("the registry failed its own re-derivation after a JSON round trip: %v", err)
			}
		})
	}
}

// TestAMalformedRegistryDigestIsRefusedBeforeTheRegistryIsFramed measures ONE
// of the three gates a review lane found missing -- the registry's -- and the
// rule is the same one this package states twice and had applied unevenly. The
// artifact's is measured by its own sibling in resolution_test.go and the
// factset's by TestAMalformedFactsetDigestIsRefusedBeforeTheFactsetIsFramed.
//
// THE FACTSET NEEDED A COST TEST OF ITS OWN, because the row for it in
// TestFirstGatesDoNotMaterializeSuppliedText pins the gate's REFUSAL and never
// its cost: SerializeCommonFactset does not READ fs.Digest -- the digest is OF
// the values -- so a 1 MiB digest leaves that row's payload tiny and its budget
// unapproached. The comparison quotes the digest now, so deleting the gate does
// blow that budget, but a budget a gate's absence happens to trip is still not
// a measurement of what the gate costs.
//
// A digest is CONSTANT-SIZE. Holding it merely to being non-empty let a
// one-byte, producer-impossible value -- "x" -- through the constant-time
// clause and on into a text scan and a full framing pass before the mismatch
// was found. Measured before the repair: 11,930 allocations at 1,000 claims
// and 95,939 at 8,000 -- in each case the same as a well-formed wrong digest,
// which is the signature of the defect: a constant-size malformed field bought
// at a price proportional to the whole artifact. The resolution artifact's
// figures for the same defect are at its own gate, in resolution.go.
//
// The assertion is the RATIO between a malformed digest and an HONEST
// verification, not an absolute figure, because an absolute is a property of
// the fixture that produced it. The denominator is an honest verification and
// not a well-formed WRONG digest, which is the weaker assertion the body
// rejects two paragraphs down: comparing two refusals would pass on two
// differently-malformed inputs.
func TestAMalformedRegistryDigestIsRefusedBeforeTheRegistryIsFramed(t *testing.T) {
	claims := make([]p4offline.SourceRoundClaim, 2000)
	for i := range claims {
		id := strconv.Itoa(i)
		claims[i] = p4offline.SourceRoundClaim{
			Episode:       p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", RoundIncarnationID: "r" + id, EventID: "e" + id},
			Attempt:       predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", AttemptID: uint64(i)},
			FactsetDigest: strings.Repeat("a", 64)}
	}
	reg := p4offline.ReconcileSourceRounds(claims)
	if err := p4offline.VerifySourceRoundRegistry(reg); err != nil {
		t.Fatalf("the unpoked control must verify: %v", err)
	}
	malformed, wrong := reg, reg
	malformed.Digest = "x"
	wrong.Digest = strings.Repeat("b", 64)
	if !errors.Is(p4offline.VerifySourceRoundRegistry(malformed), p4offline.ErrSourceRoundRegistry) ||
		!errors.Is(p4offline.VerifySourceRoundRegistry(wrong), p4offline.ErrSourceRoundRegistry) {
		t.Fatal("both shapes of bad digest must be refused")
	}
	// The denominator is an HONEST verification -- what a caller with a real
	// registry pays -- rather than a well-formed wrong digest, so the assertion
	// cannot be satisfied by two differently-malformed inputs both refusing
	// early.
	cheap := testing.AllocsPerRun(5, func() { _ = p4offline.VerifySourceRoundRegistry(malformed) })
	honest := testing.AllocsPerRun(5, func() { _ = p4offline.VerifySourceRoundRegistry(reg) })
	if honest < 1000 {
		t.Fatalf("the fixture must make an honest verification expensive, got %.0f allocations", honest)
	}
	if cheap*100 > honest {
		t.Fatalf("a one-byte malformed digest costs %.0f allocations against %.0f for an honest verification: "+
			"a constant-size malformed field must not buy work proportional to the registry", cheap, honest)
	}
}

// shapeRefusal is the phrase the three digest-SHAPE gates UNDER TEST HERE share
// and no other refusal in this package carries, so a test can tell "refused for
// the digest's shape" from "refused for the digest's value". Three of SIX: this
// package also holds a digest to its shape in p3b.go twice (the declared native
// digest and the declared raw hash) and in checkEntropyCoordinates. Those three
// state their requirements in their own words, are not reachable through this
// table, and are pinned by their own tests.
//
// The distinction it draws was genuinely unavailable at ONE of the three until
// this round: VerifySourceRoundRegistry's shape gate and its digest comparison
// both returned the same bare sentinel. At the artifact the comparison already
// named itself, so only the shape half was missing there.
const shapeRefusal = "is not this package's"

// namedDigest is one digest value under test, labelled for the subtest name.
type namedDigest struct{ name, digest string }

// digestShapeGate is one exported verifier that holds a digest to its SHAPE
// before reading the artifact that digest certifies.
//
// The three gates are ONE rule written three times, and a table of ill-shaped
// digests does not automatically pin either half of it.
//
// WHICH BYTES: isCanonicalHex returns on the LENGTH check BEFORE the character
// loop runs, so a fixture that differs from a real digest only in its length
// never reaches the character class at all, whatever its bytes are. Each gate
// reduced to exactly that branch -- `!isCanonicalHex(d, 64)` rewritten as
// `len(d) != 64`, and `!isDigestReference(d)` as `len(d) != len(prefix)+64` --
// survives any table built only of length-wrong digests. Every row here that
// differs from a real digest in anything OTHER than its length kills those,
// which is most of them. NO COUNT IS QUOTED: firstGateTableRows and the census
// in fence_test.go are where this package writes a count it means to keep. The
// HELPER-level mutants -- dropping isCanonicalHex's character class, dropping
// isDigestReference's prefix test -- are killed at this commit's parent
// 0b3cd2f too (not at the branch's base bd4d2727, where this file does not
// exist yet), by
// TestEntropyRefusesOutOfProtocolInputs' "upper-case digest" and "another
// prefix of the same length" rows. This table kills them again, and that is a
// second line of defence, not the finding.
//
// WHERE: a placement argued in COST is a placement a cost instrument cannot
// see. The artifact's gate moved BELOW the scan it is supposed to precede
// leaves every byte-ratio assertion green, because the scan ALLOCATES NOTHING
// -- 0 B/op and 0 allocs/op on a 20,000-reference fixture, about a millisecond
// of CPU. What a cost test can pin is "refused before the FRAMING", and the
// cost tests are named for the framing. Both halves of the rule are pinned
// BEHAVIOURALLY here -- by which refusal comes back -- so neither depends on a
// fixture being expensive.
type digestShapeGate struct {
	name string
	// verify runs the verifier over a fixture that verifies unpoked, with the
	// supplied digest substituted and nothing else changed.
	verify func(digest string) error
	// verifyLossy runs it over the same fixture ALSO carrying a string the
	// artifact's own encoding cannot express, so WHICH refusal comes back
	// names WHICH gate ran first.
	verifyLossy func(digest string) error
	// sentinel is what every refusal on this artifact joins.
	sentinel error
	// wantRequirement is the SHAPE this gate holds the digest to, in the
	// gate's own words. Asserting only that the refusal came from the shape
	// gate leaves the gate free to state a shape the producer does not emit.
	// The artifact's refusal made to name "sha512:" is caught by this field and
	// by nothing else in the suite; the factset's and the registry's made to
	// name 32 digits are caught here first and by their own sibling tests too.
	wantRequirement string
	// wellFormed has the right SHAPE and the wrong VALUE: the shape gate must
	// hand it on to the comparison below, which refuses it for another reason.
	wellFormed string
	// badShapes are digests this package's own producer cannot emit.
	badShapes []namedDigest
}

// digestShapeGates builds the three gates over fixtures that verify unpoked.
//
// Each fixture is rebuilt per call rather than shared: the registry's Entries
// is a slice, and a lossy case that poked a shared one would change the value
// the honest control is measured against -- the same aliasing that made an
// earlier walker in this suite mint canonical claims the producer never
// emitted.
func digestShapeGates(t *testing.T) []digestShapeGate {
	t.Helper()
	const lossyText = "\xff\xfe\x80"
	hex64 := strings.Repeat("a", 64)
	upper := strings.ToUpper(hex64)

	// THE FACTSET IS DERIVED ONCE, HERE, on the parent test's goroutine.
	// selectedCase reports a broken fixture with t.Fatalf, and these closures
	// are called from inside t.Run subtests: a t.Fatalf on the PARENT's t from
	// a subtest's goroutine is a cross-goroutine FailNow, which Go reports as
	// "subtest may have called FailNow on a parent test" instead of as the
	// fixture failure it is. Latent -- the fixture does not fail today -- and
	// repaired anyway, because the day it does fail is the day the diagnostic
	// matters. The copy each closure takes is safe: every closure assigns only
	// scalar fields and nothing appends to the shared Outcomes slice. The
	// REGISTRY is deliberately not hoisted the same way, because its lossy case
	// pokes Entries, and a shared slice would carry that into the honest
	// control.
	_, _, baseFactset := selectedCase(t, nil, nil)
	newFactset := func() p4offline.CommonFactset { return baseFactset }
	newArtifact := func() p4offline.ResolutionArtifact {
		return p4offline.ResolutionNotRecorded(
			p4offline.PublicRoundIdentity{EventID: "e1", ChannelID: "c1"}, []string{"o1", "o2"}, nil, "pr")
	}
	newRegistry := func() p4offline.SourceRoundRegistry {
		return p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{{
			Episode:       p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", RoundIncarnationID: "r1", EventID: "e1"},
			Attempt:       predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", AttemptID: 1},
			FactsetDigest: hex64,
		}})
	}
	for _, c := range []struct {
		what string
		err  error
	}{
		{"factset", p4offline.VerifyCommonFactset(newFactset())},
		{"artifact", p4offline.VerifyResolutionArtifact(newArtifact())},
		{"registry", p4offline.VerifySourceRoundRegistry(newRegistry())},
	} {
		if c.err != nil {
			t.Fatalf("the %s fixture must verify unpoked: %v", c.what, c.err)
		}
	}

	// THE PLAIN SHAPE is 64 lower-case hex digits, which is what the factset
	// and the registry each declare. Both length branches are covered, and the
	// character class is covered at the front, at the end, and ONE BYTE OUTSIDE
	// EACH OF ITS FOUR EDGES. The edges are the point. A class widened by one
	// byte still refuses 'A', 'z' and '-', so a table carrying only bytes far
	// from an edge leaves every one-byte widening alive; the four that matter
	// are '/' below '0', ':' above '9', '`' below 'a' and 'g' above 'f'. A gate
	// that checks only the first byte, only the length, or a class one byte too
	// wide is what such a table lets through.
	plain := []namedDigest{
		{"empty", ""},
		{"one byte", "x"},
		{"63 hex digits", hex64[:63]},
		{"65 hex digits", hex64 + "a"},
		{"64 upper-case hex digits", upper},
		{"one upper-case digit at the end", hex64[:63] + "A"},
		{"one upper-case digit at the front", "A" + hex64[1:]},
		{"one non-hex letter at the end", hex64[:63] + "g"},
		{"one non-hex letter at the front", "g" + hex64[1:]},
		{"64 non-hex letters", strings.Repeat("z", 64)},
		{"64 characters that are not letters at all", strings.Repeat("-", 64)},
		// THE FOUR BYTES ADJACENT TO THE CLASS, which is what "both halves at
		// each end" has to mean and did not. The class is [0-9a-f]; its
		// neighbours are '/' (0x2F), ':' (0x3A), '`' (0x60) and 'g' (0x67).
		// NO EDGE WAS DRIVEN AS AN EDGE, so a one-byte widening at any of them
		// survived the whole suite -- and not as a message. Three are rows of
		// this table named for the byte they drive; the fourth, 'g', is the
		// byte the two "non-hex letter" rows above already carry, so it earns
		// no row of its own and is named here instead.
		// No position is given for them, because a pointer to a row's place in
		// a table is wrong the moment a row is inserted. isCanonicalHex is
		// what isDigestReference calls, so under each widening EntropyMAC
		// ACCEPTED an out-of-protocol CommonFactsetDigest and drew a full
		// schedule from it. Measured, one byte at a time.
		{"a slash, the byte below '0'", hex64[:63] + "/"},
		{"a colon, the byte above '9'", hex64[:63] + ":"},
		{"a backtick, the byte below 'a'", hex64[:63] + "`"},
		// THE OTHER SPELLING, and a length this package never writes. Neither
		// table had a row for either, so all three gates could be widened to
		// accept a DigestReference where a bare digest belongs -- the exact
		// confusion doc.go lists as a sharp edge -- or to accept 32 digits,
		// with the whole suite green. A review lane measured the first at
		// about 50,800x on a 2,000-claim registry, because the widened gate hands the
		// value to the framing below it.
		{"a digest reference where a bare digest belongs", p4offline.DigestReference(hex64)},
		{"32 hex digits, which is an MD5's width", hex64[:32]},
	}
	// THE ARTIFACT'S SHAPE is a DigestReference: the prefix and then the same
	// 64 digits. The prefix is a second thing to get wrong and had no case at
	// all, so the right-length-wrong-prefix and wrong-case-prefix rows below
	// are the ones a prefix-blind gate cannot survive.
	prefixed := []namedDigest{
		{"empty", ""},
		{"one byte", "x"},
		{"the right length with no prefix at all", strings.Repeat("a", len(p4offline.DigestReferencePrefix)+64)},
		{"an upper-case prefix", strings.ToUpper(p4offline.DigestReferencePrefix) + hex64},
		{"a prefix this package does not write", "sha512:" + hex64},
		{"the prefix without a separator", "sha256" + hex64},
		// THE SEPARATOR BYTE, which nothing in this package held until a review
		// lane showed it. Every other prefix row here fails on some OTHER byte
		// -- no prefix at all, an upper-case one, a foreign one, a doubled one,
		// or a wrong length -- so a gate that compared only the six letters
		// passed the whole suite. It is not a message-only gap: the same
		// isDigestReference guards EntropyMAC's coordinates, where the mutant
		// ACCEPTED an out-of-protocol digest outright.
		{"the right length with the wrong separator", "sha256_" + hex64},
		{"the right length with a NUL for the separator", "sha256\x00" + hex64},
		{"the prefix twice", p4offline.DigestReference(p4offline.DigestReference(hex64))},
		{"63 hex digits behind the prefix", p4offline.DigestReference(hex64[:63])},
		{"65 hex digits behind the prefix", p4offline.DigestReference(hex64 + "a")},
		{"64 upper-case hex digits behind the prefix", p4offline.DigestReference(upper)},
		{"one upper-case digit at the end", p4offline.DigestReference(hex64[:63] + "A")},
		{"one non-hex letter at the end", p4offline.DigestReference(hex64[:63] + "g")},
		{"one non-hex letter at the front", p4offline.DigestReference("g" + hex64[1:])},
		{"a bare digest where a reference belongs", hex64},
		{"32 hex digits behind the prefix", p4offline.DigestReference(hex64[:32])},
	}

	return []digestShapeGate{
		{
			name: "VerifyCommonFactset",
			verify: func(d string) error {
				fs := newFactset()
				fs.Digest = d
				return p4offline.VerifyCommonFactset(fs)
			},
			verifyLossy: func(d string) error {
				fs := newFactset()
				fs.ProjectorRevision = lossyText
				fs.Digest = d
				return p4offline.VerifyCommonFactset(fs)
			},
			sentinel:        p4offline.ErrFactsetDigest,
			wantRequirement: "is not this package's 64 lower-case hex digits",
			wellFormed:      strings.Repeat("b", 64),
			badShapes:       plain,
		},
		{
			name: "VerifyResolutionArtifact",
			verify: func(d string) error {
				a := newArtifact()
				a.ResolutionFactsDigest = d
				return p4offline.VerifyResolutionArtifact(a)
			},
			verifyLossy: func(d string) error {
				a := newArtifact()
				a.Round.ChannelID = lossyText
				a.ResolutionFactsDigest = d
				return p4offline.VerifyResolutionArtifact(a)
			},
			sentinel:        p4offline.ErrResolutionDigest,
			wantRequirement: `is not this package's "sha256:" prefix and 64 lower-case hex digits`,
			wellFormed:      p4offline.DigestReference(strings.Repeat("b", 64)),
			badShapes:       prefixed,
		},
		{
			name: "VerifySourceRoundRegistry",
			verify: func(d string) error {
				reg := newRegistry()
				reg.Digest = d
				return p4offline.VerifySourceRoundRegistry(reg)
			},
			verifyLossy: func(d string) error {
				reg := newRegistry()
				reg.Entries[0].EventID = lossyText
				reg.Digest = d
				return p4offline.VerifySourceRoundRegistry(reg)
			},
			sentinel:        p4offline.ErrSourceRoundRegistry,
			wantRequirement: "is not this package's 64 lower-case hex digits",
			wellFormed:      strings.Repeat("b", 64),
			badShapes:       plain,
		},
	}
}

// TestEachNamedRegistryRefusalSaysWhichPositionRefusedIt pins the repair of an
// asymmetry a review lane counted across VerifySourceRoundRegistry's refusal
// positions: of the five its clause split creates, only the text scan named
// itself and the other four returned the identical bare sentinel with identical
// text, while the sibling verifiers name every position they own. A caller
// could not tell "this registry was written by an older version of this
// package" -- benign, and a reason to re-derive -- from the tampering signal
// this function exists to raise.
//
// Three of the four are named now. The fourth, the re-reconciliation at the
// end, is left bare DELIBERATELY, because the sentinel's own sentence ("does not
// re-derive from its entries") is exactly that position's diagnosis; it is not
// asserted here because reaching it from outside the package needs a digest
// this package computes, and TestSourceRoundRegistryVerifierAdmitsOnlyReconciliationsOwnOutput
// already drives it.
//
// NAMED is in the title on purpose, and the title is now true of all four:
// the Version clause, the digest-shape gate, the digest comparison and the text
// scan. The fifth position is the bare one above, which is not a named refusal
// and is pinned as a contract where it is driven. The title says NAMED and not
// EACH for that reason: a title that promised every position would promise one
// this test does not drive.
//
// Pairwise distinctness is the load-bearing assertion. Asserting only that each
// message contains its own phrase would pass a verifier that returned all four
// phrases at every position.
func TestEachNamedRegistryRefusalSaysWhichPositionRefusedIt(t *testing.T) {
	newRegistry := func() p4offline.SourceRoundRegistry {
		return p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{{
			Episode:       p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", RoundIncarnationID: "r1", EventID: "e1"},
			Attempt:       predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", AttemptID: 1},
			FactsetDigest: strings.Repeat("a", 64),
		}})
	}
	if err := p4offline.VerifySourceRoundRegistry(newRegistry()); err != nil {
		t.Fatalf("the unpoked control must verify: %v", err)
	}
	seen := map[string]string{}
	cases := []struct {
		name string
		poke func(*p4offline.SourceRoundRegistry)
		// names is what the refusal must say for its own position.
		names []string
		// quotes is a value the refusal must carry VERBATIM, as
		// strconv.Quote renders it. It exists because "names" alone cannot
		// tell a diagnosis from a template: the comparison's refusal below
		// reads the same for every mismatch there is if the supplied digest
		// is reported by EXTENT, and reporting it by extent survived every
		// other assertion here. The digest is safe to quote at that position
		// precisely because the shape gate above it has already bounded it.
		quotes func(p4offline.SourceRoundRegistry) string
	}{
		{"a foreign version", func(r *p4offline.SourceRoundRegistry) {
			r.Version = "NOT_A_VERSION_THIS_PACKAGE_WRITES"
		}, []string{"registry version is", "33 bytes"}, nil},
		{"a digest of the wrong shape", func(r *p4offline.SourceRoundRegistry) {
			r.Digest = "x"
		}, []string{"registry digest of", "1 bytes", "is not this package's 64 lower-case hex digits"}, nil},
		{"an entry carrying text the registry cannot express", func(r *p4offline.SourceRoundRegistry) {
			// THE FOURTH NAMED POSITION, and the reason this test's name is
			// "Each Named" rather than "Each": the text scan has always named
			// itself, so a title that said EACH would over-promise by one. It
			// is driven here rather than only in
			// TestASuppliedRegistryCarryingTextItCannotExpressIsRefused,
			// because what THIS test adds is that the four named refusals are
			// mutually distinguishable, and a fourth that is never compared
			// against the other three cannot show that.
			r.Entries[0].EventID = "\xff\xfe\x80"
		}, []string{"entry 0 event id", "is not valid UTF-8"}, nil},
		{"an entry altered under a minted digest", func(r *p4offline.SourceRoundRegistry) {
			// The realistic tampering shape, and the one that reaches the
			// COMPARISON: the digest is this package's own, so it passes the
			// shape gate and then disagrees with the entries it no longer
			// describes.
			r.Entries[0].Status = p4offline.SourceRoundConflict
		}, []string{"registry digest", "does not match its entries", "which digest to"},
			func(r p4offline.SourceRoundRegistry) string { return r.Digest }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := newRegistry()
			tc.poke(&reg)
			err := p4offline.VerifySourceRoundRegistry(reg)
			if !errors.Is(err, p4offline.ErrSourceRoundRegistry) {
				t.Fatalf("want %v, got %v", p4offline.ErrSourceRoundRegistry, err)
			}
			for _, want := range tc.names {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the refusal must name the position it refused (%q): %v", want, err)
				}
			}
			// A "this is not the bare sentinel" row belongs here by symmetry and
			// is NOT written: every row's `names` carries a phrase the bare
			// sentinel does not, so the loop above fires first on any
			// regression to it: the mutant such a row would exist for is
			// already killed four lines up. This package's own rule is that a
			// row nothing can break is decoration.
			// AND THE SENTINEL LEADS, at these positions too. errors.Join
			// renders its arguments in order and errors.Is cannot see that
			// order, so swapping them moves the CLASS out of the first line
			// and leaves a reader with the diagnosis and no category. The
			// digest-shape table pins this at the three shape gates; these are
			// the three positions the same round wrote or re-worded, and the
			// swap survived at every one of them.
			if !strings.HasPrefix(err.Error(), p4offline.ErrSourceRoundRegistry.Error()) {
				t.Fatalf("the refusal must lead with the class it joins: %v", err)
			}
			if tc.quotes != nil {
				if want := strconv.Quote(tc.quotes(reg)); !strings.Contains(err.Error(), want) {
					t.Fatalf("the refusal must carry the value it refused (%s), not a rendering of "+
						"its length that reads the same for every input: %v", want, err)
				}
				if strings.Contains(err.Error(), "64 bytes") {
					t.Fatalf("a digest the gate above has already bounded must be NAMED, not "+
						"reduced to its extent: %v", err)
				}
				// AND IN THE RIGHT ORDER. Both digests are 64 hex characters,
				// so a refusal that swaps them reads perfectly and says the
				// opposite: it names this package's own digest as the caller's.
				// Containment cannot see that -- both values are present either
				// way -- so the assertion is POSITION: the supplied digest must
				// come before the phrase that introduces the derived one. This
				// is the same swap-survives-containment failure this package
				// already recorded at the binding gate.
				// `names` above already requires "which digest to", so this
				// asks only about ORDER.
				at := strings.Index(err.Error(), strconv.Quote(tc.quotes(reg)))
				if intro := strings.Index(err.Error(), "which digest to "); at > intro {
					t.Fatalf("the SUPPLIED digest must be named before the derived one: %v", err)
				}
				if !strings.Contains(err.Error(), `which digest to "`) {
					t.Fatalf("the derived digest must be quoted too: %v", err)
				}
			}
			seen[err.Error()] = tc.name
		})
	}
	// COUNTED AND READ, not probed. A membership test inside the loop above
	// cannot fail if the map is keyed by anything else -- a review lane re-keyed
	// it by row name and the whole suite stayed green, because the lookup then
	// simply never hits. Counting alone does not fix that: four row names are
	// four distinct keys too. So the keys are also READ, and a key that is not a
	// refusal of this artifact is not a refusal at all.
	if len(seen) != len(cases) {
		t.Errorf("%d rows produced %d distinct refusals; each named position must be "+
			"distinguishable from the others: %v", len(cases), len(seen), seen)
	}
	for msg, row := range seen {
		if !strings.HasPrefix(msg, p4offline.ErrSourceRoundRegistry.Error()) {
			t.Errorf("the key recorded for %q is not a refusal this verifier produced: %q", row, msg)
		}
	}
}

// TestAnUncarriableSupplyIsRefusedBeforeItIsFramed pins this round's own rule
// ONE GATE LOWER than the round pinned it, at the text scan, at all THREE
// verifiers -- a factset, a registry and a resolution artifact -- which is why
// its name says SUPPLY rather than naming one of them.
//
// The three cost tests beside this one all poke the DIGEST, so they stop at the
// shape gate and never reach the scan below it. A review lane hoisted the
// digest MATERIALIZATION above that scan at all three verifiers -- a
// materialization above a gate that can refuse without it, which is the
// sentence this whole round exists to enforce -- and every one of them survived
// the entire suite, measured without the race detector: 46,394x at the
// registry, 22,845x at the factset, 57,562x at
// the artifact -- the artifact and the factset each 100.0% of the cost of an honest
// verification. evidence.go's own comment already CLAIMED the property
// ("refusing an uncarriable registry before a full framing pass") with nothing
// asserting it, and the file two gates up calls this "the failure mode this
// package has now made twice". It was reachable a third time.
//
// The fixtures are large AND carry a string the artifact's encoding cannot
// express, with a digest of the RIGHT SHAPE, so the shape gate passes them on
// and the scan is what refuses. The denominator is an honest verification of
// the same large fixture, as at the three siblings.
func TestAnUncarriableSupplyIsRefusedBeforeItIsFramed(t *testing.T) {
	const lossy = "\xff\xfe\x80"
	hex64 := strings.Repeat("a", 64)
	_, _, base := selectedCase(t, nil, nil)

	// EVERY BUILDER TAKES ITS SIZE, so the same fixture can be made large
	// enough to dominate an honest verification and small enough that nothing
	// payload-sized can hide inside a refusal of it.
	factsetOf := func(n int, poke bool) p4offline.CommonFactset {
		fs := base
		fs.ProjectorRevision = strings.Repeat("p", n)
		fs.Digest = digestOf(p4offline.SerializeCommonFactset(fs))
		if poke {
			fs.ProjectorRevision += lossy
		}
		return fs
	}
	registryOf := func(n int, poke bool) p4offline.SourceRoundRegistry {
		claims := make([]p4offline.SourceRoundClaim, n)
		for i := range claims {
			id := strconv.Itoa(i)
			claims[i] = p4offline.SourceRoundClaim{
				Episode:       p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", RoundIncarnationID: "r" + id, EventID: "e" + id},
				Attempt:       predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", AttemptID: uint64(i)},
				FactsetDigest: hex64}
		}
		reg := p4offline.ReconcileSourceRounds(claims)
		if poke {
			reg.Entries[0].EventID = lossy
		}
		return reg
	}
	artifactOf := func(n int, poke bool) p4offline.ResolutionArtifact {
		refs := make([]p4offline.EvidenceReference, n)
		ids := make([]string, n)
		for i := range refs {
			refs[i] = p4offline.EvidenceReference{ObservationID: "obs" + strconv.Itoa(i), Kind: "k", Phase: "p", RoundState: "RESOLVED", EventID: "e1"}
			ids[i] = "o" + strconv.Itoa(i)
		}
		a := p4offline.ProjectResolution(p4offline.ResolutionEvidence{
			Round: p4offline.PublicRoundIdentity{EventID: "e1"}, OrderedOutcomeIDs: ids,
			Claim: p4offline.ResolutionUnknown, Availability: p4offline.AvailabilityNotRecorded,
			EvidenceReferences: refs, ProjectorRevision: "pr", ProofRevision: "x"})
		if poke {
			a.Round.ChannelID = lossy
		}
		return a
	}

	measure := func(f func()) uint64 {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		f()
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	// EVERY FIXTURE IS BUILT ONCE, HERE, and never inside a measured closure.
	// Built in the closure, both readings are dominated by four megabytes of
	// construction and the ratio measures nothing: the factset pair comes back
	// at about 12.60 MB against about 12.60 MB -- the fixture twice, give or
	// take a few hundred bytes run to run -- and a refusal that read the whole
	// payload would still pass. Each poked fixture is built
	// independently of its honest twin, because the registry's poke writes
	// through Entries, which a shared slice would carry into the control.
	honestFS, lossyFS, tinyFS := factsetOf(bigFactsetText, false), factsetOf(bigFactsetText, true), factsetOf(1, true)
	honestReg, lossyReg, tinyReg := registryOf(2000, false), registryOf(2000, true), registryOf(1, true)
	honestArt, lossyArt, tinyArt := artifactOf(20000, false), artifactOf(20000, true), artifactOf(1, true)
	for _, tc := range []struct {
		name     string
		honest   func() error
		refusing func() error
		// The SAME refusal over a fixture too small to hide anything in. See
		// the scale assertion below for what it is for.
		refusingTiny func() error
	}{
		{"VerifyCommonFactset",
			func() error { return p4offline.VerifyCommonFactset(honestFS) },
			func() error { return p4offline.VerifyCommonFactset(lossyFS) },
			func() error { return p4offline.VerifyCommonFactset(tinyFS) }},
		{"VerifySourceRoundRegistry",
			func() error { return p4offline.VerifySourceRoundRegistry(honestReg) },
			func() error { return p4offline.VerifySourceRoundRegistry(lossyReg) },
			func() error { return p4offline.VerifySourceRoundRegistry(tinyReg) }},
		{"VerifyResolutionArtifact",
			func() error { return p4offline.VerifyResolutionArtifact(honestArt) },
			func() error { return p4offline.VerifyResolutionArtifact(lossyArt) },
			func() error { return p4offline.VerifyResolutionArtifact(tinyArt) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.honest(); err != nil {
				t.Fatalf("the unpoked control must verify: %v", err)
			}
			for _, refusing := range []func() error{tc.refusing, tc.refusingTiny} {
				if err := refusing(); err == nil || !strings.Contains(err.Error(), "is not valid UTF-8") {
					t.Fatalf("the SCAN must be what refuses this fixture, not a gate above it: %v", err)
				}
			}
			cheap := measure(func() { _ = tc.refusing() })
			tiny := measure(func() { _ = tc.refusingTiny() })
			honest := measure(func() { _ = tc.honest() })
			t.Logf("refused for its encoding: %d bytes at size, %d at one; honest verification: %d", cheap, tiny, honest)
			if honest < 1<<20 {
				t.Fatalf("the fixture must make an honest verification expensive, got %d bytes", honest)
			}
			if cheap*4 > honest {
				t.Fatalf("an uncarriable artifact costs %d bytes against %d for an honest verification: "+
					"the digest must not be framed above the gate that refuses without it", cheap, honest)
			}
			// AND THE RATIO ALONE IS NOT THE PROPERTY. A ratio lets a refusal
			// grow with the payload as long as it stays under a QUARTER of an
			// honest verification, and a review lane walked straight through
			// that door: hoisting the registry's OTHER payload-sized
			// materialization -- the claim flattening three lines below the
			// scan -- into exactly the position this test is named for cost
			// 768,808 bytes against a true 184 and passed, because 4.0% is
			// less than 25%. This refusal reads no payload, so its cost must
			// not move with the payload's size at all. The tiny fixture is the
			// same refusal over one claim, one reference, one byte of text;
			// the slack absorbs an error string that names a field, never one
			// that carries a payload.
			if cheap > tiny+256 {
				t.Fatalf("refusing costs %d bytes at size against %d at one: a refusal that reads no "+
					"payload must not scale with it", cheap, tiny)
			}
		})
	}
}

// TestAnO1GateAboveAShapeGateStillSpeaksFirst pins the FIVE orders the three
// shape gates create and that nothing else holds.
//
// TestADigestsSHAPEIsJudgedAboveTheScanThatReadsTheSupply holds each shape
// gate ABOVE the scan beneath it. Nothing holds any of them BELOW the
// constant-time gates above them, and without this table all FIVE hoists
// survive the whole suite: the registry's Version clause trading places with
// its digest clause, the artifact's shape gate lifted over its contract gate
// and over its obligations gate, and the factset's over its contract gate and
// over its protocol gate. Every one of those gates is O(1) and
// every one of those refusals is truthful, so no hoist is a correctness hole.
// What they change is WHICH QUESTION gets answered -- and telling a caller that
// their digest is the wrong shape, when the artifact is not this package's KIND
// at all, answers a question they did not ask about an artifact this package
// will not read either way.
//
// Each case carries its converse, because "the gate above spoke" is also what
// you would see if the shape gate had stopped looking entirely.
func TestAnO1GateAboveAShapeGateStillSpeaksFirst(t *testing.T) {
	hex64 := strings.Repeat("a", 64)
	newRegistry := func() p4offline.SourceRoundRegistry {
		return p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{{
			Episode:       p4offline.EpisodeIdentity{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", RoundIncarnationID: "r1", EventID: "e1"},
			Attempt:       predictioneval.AttemptKey{CollectorEpoch: 1, CollectorSessionID: "s", PoolInstanceID: "p", AttemptID: 1},
			FactsetDigest: hex64,
		}})
	}
	newArtifact := func() p4offline.ResolutionArtifact {
		return p4offline.ResolutionNotRecorded(
			p4offline.PublicRoundIdentity{EventID: "e1", ChannelID: "c1"}, []string{"o1", "o2"}, nil, "pr")
	}
	_, _, baseFactset := selectedCase(t, nil, nil)
	newFactset := func() p4offline.CommonFactset { return baseFactset }
	for _, tc := range []struct {
		name      string
		bothBad   func() error
		shapeOnly func() error
		names     string
	}{
		{"a registry's VERSION, above its digest's shape", func() error {
			r := newRegistry()
			r.Version = "NOT_A_VERSION_THIS_PACKAGE_WRITES"
			r.Digest = "x"
			return p4offline.VerifySourceRoundRegistry(r)
		}, func() error {
			r := newRegistry()
			r.Digest = "x"
			return p4offline.VerifySourceRoundRegistry(r)
		}, "registry version is"},
		{"an artifact's CONTRACT, above its digest's shape", func() error {
			a := newArtifact()
			a.ContractVersion = "NOT_A_CONTRACT_THIS_PACKAGE_WRITES"
			a.ResolutionFactsDigest = "x"
			return p4offline.VerifyResolutionArtifact(a)
		}, func() error {
			a := newArtifact()
			a.ResolutionFactsDigest = "x"
			return p4offline.VerifyResolutionArtifact(a)
		}, "artifact contract is"},
		{"an artifact's OBLIGATIONS revision, above its digest's shape", func() error {
			a := newArtifact()
			a.ObligationsRevision = "NOT_A_REVISION_THIS_PACKAGE_WRITES"
			a.ResolutionFactsDigest = "x"
			return p4offline.VerifyResolutionArtifact(a)
		}, func() error {
			a := newArtifact()
			a.ResolutionFactsDigest = "x"
			return p4offline.VerifyResolutionArtifact(a)
		}, "artifact obligations revision is"},
		{"a factset's CONTRACT, above its digest's shape", func() error {
			fs := newFactset()
			fs.ContractVersion = "NOT_A_CONTRACT_THIS_PACKAGE_WRITES"
			fs.Digest = "x"
			return p4offline.VerifyCommonFactset(fs)
		}, func() error {
			fs := newFactset()
			fs.Digest = "x"
			return p4offline.VerifyCommonFactset(fs)
		}, "factset contract is"},
		{"a factset's PROTOCOL, above its digest's shape", func() error {
			fs := newFactset()
			fs.Protocol = "NOT_A_PROTOCOL_THIS_PACKAGE_WRITES"
			fs.Digest = "x"
			return p4offline.VerifyCommonFactset(fs)
		}, func() error {
			fs := newFactset()
			fs.Digest = "x"
			return p4offline.VerifyCommonFactset(fs)
		}, "factset protocol is"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.shapeOnly(); err == nil || !strings.Contains(err.Error(), shapeRefusal) {
				t.Fatalf("an ill-shaped digest ALONE must be named by the shape gate: %v", err)
			}
			err := tc.bothBad()
			if err == nil {
				t.Fatal("an artifact wrong in both ways must be refused")
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("the gate above must speak first, naming %q: %v", tc.names, err)
			}
			if strings.Contains(err.Error(), shapeRefusal) {
				t.Fatalf("the digest's shape was judged above a gate that sits over it: %v", err)
			}
		})
	}
}

// TestADigestIsHeldToItsDIGITSAndNotMerelyToItsLength pins WHICH BYTES each
// shape gate refuses.
//
// Asserting only that a bad digest is REFUSED proves nothing about the gate: a
// digest of the right length and the wrong characters is refused by the
// COMPARISON below the gate too, for a different reason, and every fixture the
// adding round carried was a one-byte "x" that only the length branch had to
// see. So the assertion is WHICH refusal comes back. The well-formed-wrong row
// is the converse and is not decoration: without it a gate that refused
// EVERYTHING would pass every row above.
func TestADigestIsHeldToItsDIGITSAndNotMerelyToItsLength(t *testing.T) {
	for _, g := range digestShapeGates(t) {
		t.Run(g.name, func(t *testing.T) {
			err := g.verify(g.wellFormed)
			if !errors.Is(err, g.sentinel) {
				t.Fatalf("a well-formed WRONG digest must still be refused, got %v", err)
			}
			if strings.Contains(err.Error(), shapeRefusal) {
				t.Fatalf("a digest of the right SHAPE must reach the comparison, not the shape gate: %v", err)
			}
			for _, bad := range g.badShapes {
				t.Run(bad.name, func(t *testing.T) {
					err := g.verify(bad.digest)
					if !errors.Is(err, g.sentinel) {
						t.Fatalf("want %v, got %v", g.sentinel, err)
					}
					// THE REQUIREMENT, IN THE GATE'S OWN WORDS, and only that:
					// every wantRequirement is a superstring of shapeRefusal,
					// so a separate shapeRefusal row here could not fail unless
					// this one failed first. It is a digest the producer cannot
					// emit, so the SHAPE gate must be what refuses it -- the
					// comparison below refuses it too, for a reason that is not
					// why it is wrong. shapeRefusal earns its place in the
					// NEGATIVE assertions, where nothing dominates it.
					if !strings.Contains(err.Error(), g.wantRequirement) {
						t.Fatalf("the refusal must state the shape it holds the digest to (%q): %v",
							g.wantRequirement, err)
					}
					// AND THE SENTINEL LEADS. errors.Join renders its arguments
					// in order, and errors.Is cannot see that order, so swapping
					// them moved the class out of the first line with the whole
					// suite green at all three gates.
					if !strings.HasPrefix(err.Error(), g.sentinel.Error()) {
						t.Fatalf("the refusal must lead with the class it joins: %v", err)
					}
					// A "the refusal does not quote the digest back" row belongs
					// here by symmetry and is NOT written: a one-byte digest is
					// a substring of almost any sentence -- "x" is in both
					// "prefix" and "hex" -- so the row passes or fails on the
					// wording of the message rather than on the property. That
					// property is pinned where it can be seen, over a 1 MiB
					// digest, in TestFirstGatesDoNotMaterializeSuppliedText.
				})
			}
		})
	}
}

// TestADigestsSHAPEIsJudgedAboveTheScanThatReadsTheSupply pins WHERE each
// shape gate sits, by behaviour rather than by cost.
//
// The cost tests cannot see this. The artifact's gate moved below the scan runs
// that scan to completion and leaves every cost test green -- the scan
// allocates NOTHING, 0 B/op and 0 allocs/op, so no byte ratio can see the move
// whatever its denominator.
// An artifact bad in BOTH ways settles it: with the gate above, the refusal
// names the digest's shape; with the gate below, it names the encoding fault.
func TestADigestsSHAPEIsJudgedAboveTheScanThatReadsTheSupply(t *testing.T) {
	const encodingRefusal = "is not valid UTF-8"
	for _, g := range digestShapeGates(t) {
		t.Run(g.name, func(t *testing.T) {
			// THE CONTROL FIRST: the lossy fixture must be one the scan
			// actually refuses. Without this row the case below could pass
			// because the fixture was never lossy at all.
			control := g.verifyLossy(g.wellFormed)
			if control == nil || !strings.Contains(control.Error(), encodingRefusal) {
				t.Fatalf("the lossy fixture must be refused by the scan, or the case below proves nothing: %v", control)
			}
			both := g.verifyLossy("x")
			if !errors.Is(both, g.sentinel) {
				t.Fatalf("want %v, got %v", g.sentinel, both)
			}
			if !strings.Contains(both.Error(), shapeRefusal) {
				t.Fatalf("an artifact bad in BOTH ways must be refused by the gate that costs nothing: %v", both)
			}
			if strings.Contains(both.Error(), encodingRefusal) {
				t.Fatalf("the scan ran above the shape gate: %v", both)
			}
		})
	}
}

// TestADerivedClaimIsNeverRoutedInvalidForItsText is the no-false-refusal
// control for the routing, and it is the half a narrowing owes.
//
// The gate refuses a claim for its ENCODING, so the question it has to answer
// is whether the sanctioned producer can emit one. It cannot, and the reason is
// structural rather than incidental -- but the reason is SIX of seven strings,
// not seven, which a review lane corrected by execution. ClaimSourceRound
// derives its claim from a factset; the four episode identities and the two
// attempt identities have already passed checkFactsetValuesExpressible on that
// factset, and the seventh, the factset digest, is NOT in that validator's list
// at all: what forces it expressible is the next gate, the comparison against
// this package's own 64 hex digits. This drives the real path rather than
// asserting either, because "an argument two readers accept is not a
// measurement" is this branch's oldest lesson.
func TestADerivedClaimIsNeverRoutedInvalidForItsText(t *testing.T) {
	s := newSynth()
	s.placedAttempt("r1", "e1", 1)
	ds := s.dataset()
	fs, err := p4offline.BuildCommonFactset(ds, singleEpisode(t, mustSelect(t, ds)).Episode)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := p4offline.ClaimSourceRound(ds, fs)
	if err != nil {
		t.Fatal(err)
	}
	reg := p4offline.ReconcileSourceRounds([]p4offline.SourceRoundClaim{claim})
	if len(reg.Entries) != 1 || reg.Entries[0].Status != p4offline.SourceRoundUnique {
		t.Fatalf("a derived claim is evidence for its round, not INVALID: %+v", reg.Entries)
	}
	if reg.Entries[0].Canonical == nil || *reg.Entries[0].Canonical != claim {
		t.Fatalf("the derived claim must reach the canonical position unaltered: %+v", reg.Entries[0])
	}
}

// TestASessionRefusedByItsOwnProvenanceIsNotMaterializedFirst pins the
// preflight at the top of SelectEpisodes.
//
// MaterializePairedKnowledge groups every record in the dataset. Nine of the
// session refusals read only constant-size provenance, and the foreign-facts
// scan reads two identity fields per record without allocating; none of them
// needs the materialized knowledge. A security review lane measured the old
// order: a ONE-BYTE SessionReading that refuses the whole session allocated
// 22,618,360 B/op on 16,384 attempt rows, against 16 B/op on an empty dataset,
// and every byte of it was discarded.
//
// BOTH DIRECTIONS ARE ASSERTED. A ceiling alone passes if the fixture's
// records never reach the measured path, so the admitted control must show the
// records ARE there and DO cost -- otherwise "flat" would mean "empty".
func TestASessionRefusedByItsOwnProvenanceIsNotMaterializedFirst(t *testing.T) {
	measure := func(f func()) uint64 {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		f()
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	build := func(n int, poke func(*predictioneval.SourceProvenance)) predictioneval.SourceDataset {
		s := newSynth()
		for i := 0; i < n; i++ {
			s.due("r1", "e1", int64(i+1))
		}
		ds := s.dataset()
		poke(&ds.Source)
		return ds
	}
	const rows = 16384
	refused := func(p *predictioneval.SourceProvenance) { p.SessionReading = "X" }
	admitted := func(p *predictioneval.SourceProvenance) {}

	var small, large uint64
	for _, tc := range []struct {
		name string
		n    int
		into *uint64
	}{{"an empty dataset", 0, &small}, {"16,384 attempt rows", rows, &large}} {
		ds := build(tc.n, refused)
		var sel p4offline.EvidenceSelection
		var err error
		*tc.into = measure(func() { sel, err = p4offline.SelectEpisodes(ds) })
		t.Logf("%-22s refused by one byte of provenance: %d bytes", tc.name, *tc.into)
		if err != nil {
			t.Fatalf("a refused session is not an error, it is a refusal: %v", err)
		}
		if sel.SessionAdmitted {
			t.Fatalf("this fixture must be refused, or the budget proves nothing")
		}
		if !containsString(sel.SessionRefusals, p4offline.RefusalSessionReadingNotAsFinalized) {
			t.Fatalf("the refusal must name the reading, got %v", sel.SessionRefusals)
		}
	}
	// The refusal must not grow with the dataset. The allowance is generous
	// against allocator rounding and still three orders below one
	// materialization of this fixture.
	if large > small+8192 {
		t.Fatalf("refusing on constant-size provenance allocated %d bytes on %d rows against %d on none: the dataset is still being materialized first",
			large, rows, small)
	}
	// THE CONTROL: the same records, a session the provenance admits. This
	// must cost, or the rows above were flat because the fixture was empty.
	ds := build(rows, admitted)
	var sel p4offline.EvidenceSelection
	var err error
	cost := measure(func() { sel, err = p4offline.SelectEpisodes(ds) })
	t.Logf("%-22s admitted: %d bytes", "16,384 attempt rows", cost)
	if err != nil {
		t.Fatalf("the admitted control must select: %v", err)
	}
	if !sel.SessionAdmitted {
		t.Fatalf("the control must be admitted, got refusals %v", sel.SessionRefusals)
	}
	if cost < 1<<20 {
		t.Fatalf("the admitted control allocated only %d bytes on %d rows: the fixture is not reaching materialization, so the flatness above proves nothing",
			cost, rows)
	}
}
