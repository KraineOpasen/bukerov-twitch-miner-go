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
		for _, ep := range sel.Episodes {
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
		// record while the index is built, so how many times eachMatching
		// visits a signal afterwards cannot re-trigger it. The deduplication is
		// pinned differentially in evidence_internal_test.go instead, which is
		// where it can actually be observed.
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
	paired := byIncarnation(t, mustSelect(t, build(true)), "r2")
	unpaired := byIncarnation(t, mustSelect(t, build(false)), "r2")

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
}
