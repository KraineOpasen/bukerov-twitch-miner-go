package p4offline_test

// SEAM 8: the separate, immutable resolution artifact.
//
// The winner is a claim that must carry its proof. The proof obligations are
// stated in resolution.go; every case below is one obligation withheld, and
// the expected answer in every such case is UNKNOWN — never a winner
// inferred from a RESOLVED state, a nearby time, the same event alone, the
// same stake alone, or a terminal WON/LOST.

import (
	"errors"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

func resolvedRef(id, event string) p4offline.EvidenceReference {
	return p4offline.EvidenceReference{ObservationID: id, CollectorEpoch: synthEpoch, CollectorSessionID: synthSession,
		Kind: "channel_event", Phase: "ROUND_UPDATED", RoundState: "RESOLVED", EventID: event}
}

func goodWinnerEvidence() p4offline.ResolutionEvidence {
	return p4offline.ResolutionEvidence{
		Round:              p4offline.PublicRoundIdentity{EventID: "e1"},
		OrderedOutcomeIDs:  []string{"o1", "o2"},
		Claim:              p4offline.ResolutionWinnerKnown,
		WinnerOutcomeID:    "o2",
		ProofBasis:         p4offline.ProofBasisPlatformResolvedWinner,
		EvidenceReferences: []p4offline.EvidenceReference{resolvedRef("resolved-1", "e1")},
		Availability:       p4offline.AvailabilityAvailable,
		ProjectorRevision:  "test-projector/v1",
		ProofRevision:      "test-proof/v1",
	}
}

// TestResolutionArtifactIsTypedImmutableAndDigested pins the happy path and
// the digest.
func TestResolutionArtifactIsTypedImmutableAndDigested(t *testing.T) {
	a := p4offline.ProjectResolution(goodWinnerEvidence())
	if a.ContractVersion != p4offline.ResolutionFactsDigestVersion || a.Outcome != p4offline.ResolutionWinnerKnown ||
		a.WinnerOutcomeID != "o2" || a.WinnerIndex != 1 || len(a.Refusals) != 0 || a.ResolutionFactsDigest == "" {
		t.Fatalf("%+v", a)
	}
	if err := p4offline.VerifyResolutionArtifact(a); err != nil {
		t.Fatalf("a freshly projected artifact must verify: %v", err)
	}
	b := p4offline.ProjectResolution(goodWinnerEvidence())
	if b.ResolutionFactsDigest != a.ResolutionFactsDigest {
		t.Fatal("the digest must be deterministic")
	}
	for name, mutate := range map[string]func(*p4offline.ResolutionArtifact){
		"the winner":       func(x *p4offline.ResolutionArtifact) { x.WinnerOutcomeID = "o1"; x.WinnerIndex = 0 },
		"the outcome":      func(x *p4offline.ResolutionArtifact) { x.Outcome = p4offline.ResolutionRefund },
		"the round":        func(x *p4offline.ResolutionArtifact) { x.Round.EventID = "e2" },
		"the outcome set":  func(x *p4offline.ResolutionArtifact) { x.OrderedOutcomeIDs = []string{"o2", "o1"} },
		"the evidence":     func(x *p4offline.ResolutionArtifact) { x.EvidenceReferences[0].ObservationID = "other" },
		"the availability": func(x *p4offline.ResolutionArtifact) { x.Availability = p4offline.AvailabilityUnavailable },
		"the revision":     func(x *p4offline.ResolutionArtifact) { x.ProofRevision = "other" },
		"the digest": func(x *p4offline.ResolutionArtifact) {
			// Flip the first hex DIGIT after the "sha256:" prefix.
			i := len(p4offline.DigestReferencePrefix)
			flip := "0"
			if x.ResolutionFactsDigest[i] == '0' {
				flip = "1"
			}
			x.ResolutionFactsDigest = x.ResolutionFactsDigest[:i] + flip + x.ResolutionFactsDigest[i+1:]
		},
		"the digest spelling": func(x *p4offline.ResolutionArtifact) {
			// The bare hex is not a second spelling of the same digest.
			x.ResolutionFactsDigest = x.ResolutionFactsDigest[len(p4offline.DigestReferencePrefix):]
		},
	} {
		t.Run("tampering "+name, func(t *testing.T) {
			x := p4offline.ProjectResolution(goodWinnerEvidence())
			x.EvidenceReferences = append([]p4offline.EvidenceReference(nil), x.EvidenceReferences...)
			mutate(&x)
			if err := p4offline.VerifyResolutionArtifact(x); !errors.Is(err, p4offline.ErrResolutionDigest) {
				t.Fatalf("got %v", err)
			}
		})
	}
	// The evidence handed in cannot reach into the artifact afterwards.
	ev := goodWinnerEvidence()
	c := p4offline.ProjectResolution(ev)
	ev.OrderedOutcomeIDs[1] = "changed"
	ev.EvidenceReferences[0].ObservationID = "changed"
	if err := p4offline.VerifyResolutionArtifact(c); err != nil || c.OrderedOutcomeIDs[1] != "o2" {
		t.Fatalf("the artifact aliases its evidence: %v %+v", err, c.OrderedOutcomeIDs)
	}
}

// forgedWinnerArtifact is a hostile INPUT: an artifact whose fields carry
// none of the winner's proof obligations but which asserts WINNER_KNOWN, with
// a digest the test recomputes itself over the documented framing so it is
// consistent at the digest level.
func forgedWinnerArtifact(winner string) p4offline.ResolutionArtifact {
	a := p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, nil, "p")
	a.Outcome = p4offline.ResolutionWinnerKnown
	a.WinnerOutcomeID = winner
	a.WinnerIndex = 0
	if winner == "o2" {
		a.WinnerIndex = 1
	}
	a.Refusals = nil
	a.ResolutionFactsDigest = p4offline.DigestReference(digestOf(p4offline.SerializeResolutionArtifact(a)))
	return a
}

// TestResolutionVerificationRederivesTheOutcome pins that a matching digest
// is not enough: a WINNER_KNOWN or REFUND artifact must be exactly what
// projecting its own facts produces.
func TestResolutionVerificationRederivesTheOutcome(t *testing.T) {
	// THE FIXTURE GUARD, and what it is and is not. forgedWinnerArtifact sets
	// ResolutionFactsDigest from SerializeResolutionArtifact, and that field is
	// not itself serialized, so re-running the same pure function over the same
	// value is f(x) == f(x): it holds for ANY implementation of the serializer,
	// including one whose whole body is `return []byte("x")`. An independent
	// lane executed that mutant and neither this line nor its sibling below
	// fired. SIX tests did, and an earlier draft of this comment named the
	// wrong one: TestResolutionArtifactDigestMatchesTheIndependentGolden (in
	// golden_test.go, not this file), TestResolutionArtifactIsTypedImmutableAndDigested,
	// TestResolutionVerificationRederivesTheOutcome -- the very function this
	// comment sits in, at its VerifyResolutionArtifact assertion, not at either
	// guard -- TestHandwrittenWinLoseRefundScoring,
	// TestAssessCaseQualityIsTheMinimumOverEverySeam and
	// TestDenominatorMembershipIsComposedWithTheCaseQuality. So the guard is
	// stated for what it actually is -- internal consistency of the fixture,
	// not a check on the framing -- and the framing is covered in several
	// places, the independent Python golden among them.
	forged := forgedWinnerArtifact("o2")
	if got := p4offline.DigestReference(digestOf(p4offline.SerializeResolutionArtifact(forged))); got != forged.ResolutionFactsDigest {
		t.Fatalf("fixture: the forged artifact must be internally consistent")
	}
	if err := p4offline.VerifyResolutionArtifact(forged); !errors.Is(err, p4offline.ErrResolutionNotDerivable) {
		t.Fatalf("got %v", err)
	}
	// A genuine artifact serializes to the bytes its digest covers. Same shape
	// as the fixture guard above and the same limit: resolutionDigest IS
	// DigestReference(sha256Hex(SerializeResolutionArtifact(a))), so this
	// cannot disagree with the code. It is kept as a regression guard on the
	// two being WIRED together -- if ProjectResolution ever stopped setting the
	// field from the serializer, this fires -- and the framing itself is
	// pinned elsewhere -- by golden_test.go's independent Python golden, and by
	// five further assertions including the VerifyResolutionArtifact check
	// twelve lines below this one.
	a := p4offline.ProjectResolution(goodWinnerEvidence())
	if p4offline.DigestReference(digestOf(p4offline.SerializeResolutionArtifact(a))) != a.ResolutionFactsDigest {
		t.Fatal("ProjectResolution no longer digests what SerializeResolutionArtifact frames")
	}
	// A REFUND asserted without its proof is refused the same way.
	refund := p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, nil, "p")
	refund.Outcome = p4offline.ResolutionRefund
	refund.Refusals = nil
	refund.ResolutionFactsDigest = p4offline.DigestReference(digestOf(p4offline.SerializeResolutionArtifact(refund)))
	if err := p4offline.VerifyResolutionArtifact(refund); !errors.Is(err, p4offline.ErrResolutionNotDerivable) {
		t.Fatalf("got %v", err)
	}
	// An UNKNOWN artifact that names a winner is not an UNKNOWN artifact.
	//
	// ONE CLAUSE AT A TIME, and this one matters more than most: the guard is
	// the package's prime directive at its narrowest point -- an artifact that
	// admits it does not know the outcome must not name one. It used to be
	// tested by a single case setting BOTH the identity and the index, so
	// either conjunct could be deleted with the whole suite green, and an
	// artifact naming a winner by index alone (or by identity alone) would
	// have verified. A discriminating input exists and needs only exported
	// symbols, which is why this is split rather than argued away.
	unknownNaming := func(t *testing.T, edit func(*p4offline.ResolutionArtifact)) error {
		t.Helper()
		u := p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, nil, "p")
		if u.Outcome != p4offline.ResolutionUnknown {
			t.Fatalf("fixture must be UNKNOWN, got %q", u.Outcome)
		}
		edit(&u)
		u.ResolutionFactsDigest = p4offline.DigestReference(digestOf(p4offline.SerializeResolutionArtifact(u)))
		return p4offline.VerifyResolutionArtifact(u)
	}
	for _, tc := range []struct {
		name string
		edit func(*p4offline.ResolutionArtifact)
	}{
		{"by identity alone", func(u *p4offline.ResolutionArtifact) { u.WinnerOutcomeID = "o1" }},
		{"by index alone", func(u *p4offline.ResolutionArtifact) { u.WinnerIndex = 0 }},
		{"by both", func(u *p4offline.ResolutionArtifact) { u.WinnerOutcomeID = "o1"; u.WinnerIndex = 0 }},
	} {
		if err := unknownNaming(t, tc.edit); !errors.Is(err, p4offline.ErrResolutionNotDerivable) {
			t.Fatalf("an UNKNOWN artifact naming a winner %s must be refused, got %v", tc.name, err)
		}
	}
	// And the honest control: an UNKNOWN artifact that names nothing verifies.
	if err := unknownNaming(t, func(*p4offline.ResolutionArtifact) {}); err != nil {
		t.Fatalf("an honest UNKNOWN artifact must verify: %v", err)
	}
	// An outcome outside the vocabulary is refused whatever its digest.
	odd := a
	odd.Outcome = "PROBABLY_O2"
	odd.ResolutionFactsDigest = p4offline.DigestReference(digestOf(p4offline.SerializeResolutionArtifact(odd)))
	if err := p4offline.VerifyResolutionArtifact(odd); !errors.Is(err, p4offline.ErrResolutionNotDerivable) {
		t.Fatalf("got %v", err)
	}
}

// TestResolutionWithheldProofIsUnknown pins every withheld obligation.
func TestResolutionWithheldProofIsUnknown(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*p4offline.ResolutionEvidence)
		refusal string
	}{
		{"terminal WON/LOST is not a winner proof (binary)", func(e *p4offline.ResolutionEvidence) {
			e.ProofBasis = "USER_TERMINAL_WON_LOST"
		}, "BASIS_NOT_ACCEPTED"},
		{"terminal LOST on a non-binary round", func(e *p4offline.ResolutionEvidence) {
			e.OrderedOutcomeIDs = []string{"o1", "o2", "o3"}
			e.ProofBasis = "USER_TERMINAL_WON_LOST"
		}, "BASIS_NOT_ACCEPTED"},
		{"RESOLVED state alone", func(e *p4offline.ResolutionEvidence) { e.ProofBasis = "ROUND_STATE_RESOLVED_ONLY" }, "BASIS_NOT_ACCEPTED"},
		{"nearest time", func(e *p4offline.ResolutionEvidence) { e.ProofBasis = "NEAREST_TIME" }, "BASIS_NOT_ACCEPTED"},
		{"same event alone", func(e *p4offline.ResolutionEvidence) { e.ProofBasis = "SAME_EVENT_ONLY" }, "BASIS_NOT_ACCEPTED"},
		{"same stake alone", func(e *p4offline.ResolutionEvidence) { e.ProofBasis = "SAME_STAKE_ONLY" }, "BASIS_NOT_ACCEPTED"},
		{"empty basis", func(e *p4offline.ResolutionEvidence) { e.ProofBasis = "" }, "BASIS_NOT_ACCEPTED"},
		{"winner outside the outcome set", func(e *p4offline.ResolutionEvidence) { e.WinnerOutcomeID = "o9" }, "WINNER_NOT_IN_OUTCOME_SET"},
		{"winner empty", func(e *p4offline.ResolutionEvidence) { e.WinnerOutcomeID = "" }, "WINNER_NOT_IN_OUTCOME_SET"},
		{"winner ambiguous in a duplicated set", func(e *p4offline.ResolutionEvidence) {
			e.OrderedOutcomeIDs = []string{"o1", "o2", "o2"}
		}, "OUTCOME_SET_INVALID"},
		{"single-outcome set", func(e *p4offline.ResolutionEvidence) { e.OrderedOutcomeIDs = []string{"o2"} }, "OUTCOME_SET_INVALID"},
		{"refund naming a winner", func(e *p4offline.ResolutionEvidence) {
			e.Claim = p4offline.ResolutionRefund
			e.ProofBasis = p4offline.ProofBasisPlatformCanceledRefund
			e.EvidenceReferences[0].RoundState = "CANCELED"
		}, "REFUND_CONFLICT"},
		{"winner claim on a refund basis", func(e *p4offline.ResolutionEvidence) {
			e.ProofBasis = p4offline.ProofBasisPlatformCanceledRefund
		}, "BASIS_NOT_ACCEPTED"},
		{"no evidence reference", func(e *p4offline.ResolutionEvidence) { e.EvidenceReferences = nil }, "EVIDENCE_MISSING"},
		{"evidence from another round", func(e *p4offline.ResolutionEvidence) { e.EvidenceReferences[0].EventID = "e2" }, "EVIDENCE_ROUND_MISMATCH"},
		{"evidence not resolved", func(e *p4offline.ResolutionEvidence) { e.EvidenceReferences[0].RoundState = "LOCKED" }, "EVIDENCE_STATE_MISMATCH"},
		{"not available", func(e *p4offline.ResolutionEvidence) { e.Availability = p4offline.AvailabilityNotRecorded }, "AVAILABILITY_NOT_AVAILABLE"},
		{"no proof revision", func(e *p4offline.ResolutionEvidence) { e.ProofRevision = "" }, "REVISION_MISSING"},
		{"claim outside the vocabulary", func(e *p4offline.ResolutionEvidence) { e.Claim = "PROBABLY_O2" }, "CLAIM_OUTSIDE_VOCABULARY"},
		{"availability outside the vocabulary", func(e *p4offline.ResolutionEvidence) { e.Availability = "DEFINITELY_AVAILABLE_TRUST_ME" }, "AVAILABILITY_OUTSIDE_VOCABULARY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := goodWinnerEvidence()
			ev.EvidenceReferences = append([]p4offline.EvidenceReference(nil), ev.EvidenceReferences...)
			tc.mutate(&ev)
			a := p4offline.ProjectResolution(ev)
			if a.Outcome != p4offline.ResolutionUnknown || a.WinnerOutcomeID != "" || a.WinnerIndex != -1 {
				t.Fatalf("a withheld obligation is UNKNOWN, never a winner: %+v", a)
			}
			if !containsString(a.Refusals, tc.refusal) {
				t.Fatalf("refusals %v do not name %s", a.Refusals, tc.refusal)
			}
			if err := p4offline.VerifyResolutionArtifact(a); err != nil {
				t.Fatalf("an UNKNOWN artifact is still digested: %v", err)
			}
		})
	}
	t.Run("a proven refund is REFUND with no winner", func(t *testing.T) {
		ev := goodWinnerEvidence()
		ev.Claim = p4offline.ResolutionRefund
		ev.WinnerOutcomeID = ""
		ev.ProofBasis = p4offline.ProofBasisPlatformCanceledRefund
		ev.EvidenceReferences = []p4offline.EvidenceReference{{ObservationID: "canceled-1", Kind: "channel_event",
			Phase: "ROUND_UPDATED", RoundState: "CANCELED", EventID: "e1"}}
		a := p4offline.ProjectResolution(ev)
		if a.Outcome != p4offline.ResolutionRefund || a.WinnerIndex != -1 || len(a.Refusals) != 0 {
			t.Fatalf("%+v", a)
		}
	})
	t.Run("an UNKNOWN claim still names an availability inside the vocabulary", func(t *testing.T) {
		ev := goodWinnerEvidence()
		ev.Claim = p4offline.ResolutionUnknown
		ev.WinnerOutcomeID = ""
		ev.ProofBasis = ""
		ev.Availability = "DEFINITELY_AVAILABLE_TRUST_ME"
		a := p4offline.ProjectResolution(ev)
		if a.Outcome != p4offline.ResolutionUnknown || !containsString(a.Refusals, "AVAILABILITY_OUTSIDE_VOCABULARY") {
			t.Fatalf("%+v", a)
		}
		if err := p4offline.VerifyResolutionArtifact(a); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("an UNKNOWN claim is an honest unknown, not a refusal", func(t *testing.T) {
		ev := goodWinnerEvidence()
		ev.Claim = p4offline.ResolutionUnknown
		ev.WinnerOutcomeID = ""
		ev.ProofBasis = ""
		ev.Availability = p4offline.AvailabilityNotRecorded
		a := p4offline.ProjectResolution(ev)
		if a.Outcome != p4offline.ResolutionUnknown || len(a.Refusals) != 0 {
			t.Fatalf("%+v", a)
		}
	})
}

// TestResolutionCannotBeReadFromTheP1Mirror pins the availability rule: the
// mirror this package reads carries no winning outcome id, so an extraction
// from a dataset yields references and an UNKNOWN — even beside a user
// terminal fact that says WON.
func TestResolutionCannotBeReadFromTheP1Mirror(t *testing.T) {
	s := newSynth()
	s.placedAttempt("r1", "e1", 1)
	s.userTerminal("r1", "e1", "WON", 50, 120)
	s.add(s.fact("channel_event", "ROUND_UPDATED", "r1", "e1", 0))
	ds := s.dataset()
	ds.Records[len(ds.Records)-1].Payload.RoundState = "RESOLVED"
	refs := p4offline.ExtractResolutionReferences(ds, p4offline.PublicRoundIdentity{EventID: "e1"})
	if len(refs) != 2 {
		t.Fatalf("the settlement fact and the resolved frame are references: %+v", refs)
	}
	for _, r := range refs {
		if r.EventID != "e1" || r.ObservationID == "" || r.Kind == "" {
			t.Fatalf("%+v", r)
		}
	}
	a := p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, refs, predictioneval.ModelVersion)
	if a.Outcome != p4offline.ResolutionUnknown || a.Availability != p4offline.AvailabilityNotRecorded || a.WinnerIndex != -1 {
		t.Fatalf("a WON terminal is not a winner proof: %+v", a)
	}
	if err := p4offline.VerifyResolutionArtifact(a); err != nil {
		t.Fatal(err)
	}
	if len(p4offline.ExtractResolutionReferences(ds, p4offline.PublicRoundIdentity{EventID: "other"})) != 0 {
		t.Fatal("references are per round")
	}
}

// TestJoinReasonsHandlesTheEmptyAndSingletonLists pins the one line of the
// single-pass rewrite that IS separately testable, and that the site's own
// "not separately testable" note over-scoped to cover.
//
// The rewrite computes the buffer size as `len(rs) - 1 + sum(len)`, so an empty
// list gives n == -1 and `make([]byte, 0, -1)` PANICS. The empty case is
// reached in production: VerifyResolutionArtifact's re-projection arm renders
// re.Refusals, which can be empty for an internally consistent artifact whose
// re-projection yields a different digest. Deleting the guard left the whole
// suite green, and a one-line probe against that mutant panicked with
// "makeslice: cap out of range".
//
// The expected values are the join's definition, stated here: comma-separated,
// no leading or trailing comma, empty members preserved.
func TestJoinReasonsHandlesTheEmptyAndSingletonLists(t *testing.T) {
	// Reached through the exported seam rather than by calling the unexported
	// helper: a REFUND artifact asserted without its proof re-projects to a
	// different digest and renders its (empty) refusal list.
	refund := p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, nil, "p")
	refund.Outcome = p4offline.ResolutionRefund
	refund.Refusals = nil
	refund.ResolutionFactsDigest = p4offline.DigestReference(digestOf(p4offline.SerializeResolutionArtifact(refund)))
	err := p4offline.VerifyResolutionArtifact(refund)
	if !errors.Is(err, p4offline.ErrResolutionNotDerivable) {
		t.Fatalf("a REFUND asserted without its proof must be refused: %v", err)
	}
	// Reaching this line at all is the assertion: without the guard the call
	// above panics rather than returning.
	if err.Error() == "" {
		t.Fatal("the refusal must say something")
	}

	t.Run("an empty refusal list renders as nothing, not as a comma", func(t *testing.T) {
		if strings.Contains(err.Error(), "refusals ,") || strings.HasSuffix(err.Error(), ",") {
			t.Fatalf("an empty list must render empty: %q", err.Error())
		}
	})
	t.Run("a single refusal renders without a separator", func(t *testing.T) {
		one := p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, nil, "p")
		one.Outcome = p4offline.ResolutionRefund
		one.Refusals = []string{"ONE_REASON"}
		one.ResolutionFactsDigest = p4offline.DigestReference(digestOf(p4offline.SerializeResolutionArtifact(one)))
		e := p4offline.VerifyResolutionArtifact(one)
		if !errors.Is(e, p4offline.ErrResolutionNotDerivable) {
			t.Fatalf("got %v", e)
		}
		if strings.Contains(e.Error(), ",ONE_REASON") || strings.Contains(e.Error(), "ONE_REASON,") {
			t.Fatalf("a single member carries no separator: %q", e.Error())
		}
	})
}
