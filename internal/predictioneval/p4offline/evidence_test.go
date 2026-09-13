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
	"errors"
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

	t.Run("an episode with no automatic attempt is not an opportunity", func(t *testing.T) {
		s := newSynth()
		s.add(s.fact(predictioneval.KindAutoDecision, predictioneval.PhaseAutoDue, "r1", "e1", 0))
		ep := singleEpisode(t, mustSelect(t, s.dataset()))
		if !ep.Excluded || !containsString(ep.ExclusionReasons, "NO_AUTOMATIC_OPPORTUNITY") {
			t.Fatalf("%+v", ep)
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

	t.Run("an ambiguous placement fact before the cutoff breaks the boundary", func(t *testing.T) {
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
		if ep.Boundary.EarliestCallKind != p4offline.CallKindAmbiguous || ep.Boundary.Reason != "C_NOT_BEFORE_F" {
			t.Fatalf("an undecodable placement fact is an AMBIGUOUS call at its own position: %+v", ep.Boundary)
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
