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
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"

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
