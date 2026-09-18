package p4offline_test

// SEAM 8: the separate, immutable resolution artifact.
//
// The winner is a claim that must carry its proof. The proof obligations are
// stated in resolution.go; every case below is one obligation withheld, and
// the expected answer in every such case is UNKNOWN — never a winner
// inferred from a RESOLVED state, a nearby time, the same event alone, the
// same stake alone, or a terminal WON/LOST.

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

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

// TestEveryReachableResolutionStringRefusesInvalidUTF8 is the factset test's
// sibling, and it exists because the factset repair was made first and this
// artifact was left out.
//
// The same defect lived here: a caller-supplied ResolutionArtifact carrying
// invalid UTF-8 in a framed string was CERTIFIED, marshalled without error,
// and failed its OWN digest after a JSON round trip — the signal this package
// reserves for tampering. coverage_test.go round-trips this artifact in the
// same test that round-trips a factset, so the path is supported rather than
// hypothetical.
//
// Reproduced before the repair at four positions; a poked Round.EventID was
// already caught by the re-projection, so the hole was four wide and not five.
// The gate now sits ahead of both the digest and the re-projection, so every
// position reports the encoding fault.
func TestEveryReachableResolutionStringRefusesInvalidUTF8(t *testing.T) {
	const invalid = "\xff\xfe\x80"
	const validWide = "ok-\u00e9-\uFFFD"
	const utf8Fault = "is not valid UTF-8"

	sealed := func(a p4offline.ResolutionArtifact) p4offline.ResolutionArtifact {
		a.ResolutionFactsDigest = p4offline.DigestReference(digestOf(p4offline.SerializeResolutionArtifact(a)))
		return a
	}
	sites := func() (*p4offline.ResolutionArtifact, []floatSite) {
		a := winnerArtifact("o1")
		var out []floatSite
		reachableStrings(reflect.ValueOf(&a).Elem(), "ResolutionArtifact", &out)
		return &a, out
	}
	// THE UNPOKED CONTROL MUST VERIFY, or every "was CERTIFIED" assertion below
	// passes for a reason unrelated to the poke. This baseline was briefly LOST:
	// an attempt to reach Refusals through the walker put a refusal on this
	// WINNER_KNOWN fixture and resealed it, which the package rightly refuses as
	// not the projection of its own facts ("re-projecting the artifact's facts
	// yields WINNER_KNOWN with refusals"). A review lane caught it. Refusals is
	// covered by its own case below instead, on an artifact that legitimately
	// carries one.
	control, _ := sites()
	if err := p4offline.VerifyResolutionArtifact(*control); err != nil {
		t.Fatalf("the unpoked control must verify: %v", err)
	}
	// EXACT, not a floor. `len(found) < 8` was the guard here, which could not
	// tell 19 from 20 and so could not notice that Refusals was missing --
	// reachableStrings contributes NOTHING for an empty slice, and this fixture
	// carries none.
	_, found := sites()
	if want := 19; len(found) != want {
		var paths []string
		for _, f := range found {
			paths = append(paths, f.path)
		}
		t.Fatalf("reached %d string positions, want %d: %s", len(found), want, strings.Join(paths, ", "))
	}
	for i, site := range found {
		switch site.path {
		case "ResolutionArtifact.ResolutionFactsDigest":
			continue // the seal itself, not a framed value
		}
		t.Run(site.path, func(t *testing.T) {
			a, s := sites()
			s[i].at.SetString(invalid)
			err := p4offline.VerifyResolutionArtifact(sealed(*a))
			if err == nil {
				t.Fatalf("invalid UTF-8 at %s was CERTIFIED", site.path)
			}
			// The two identifiers held to exact constants are refused by the
			// gates above this one; everything else must be refused BY THE
			// ENCODING, and naming the difference is what keeps the assertion
			// from passing on any error whatsoever.
			if site.path == "ResolutionArtifact.ContractVersion" ||
				site.path == "ResolutionArtifact.ObligationsRevision" {
				return
			}
			if !strings.Contains(err.Error(), utf8Fault) {
				t.Fatalf("invalid UTF-8 at %s = %v, want a %q refusal", site.path, err, utf8Fault)
			}
			// The control: valid multi-byte text, U+FFFD included, is never
			// refused for its encoding.
			a2, s2 := sites()
			s2[i].at.SetString(validWide)
			if err := p4offline.VerifyResolutionArtifact(sealed(*a2)); err != nil &&
				strings.Contains(err.Error(), utf8Fault) {
				t.Fatalf("valid UTF-8 at %s was refused for its encoding: %v", site.path, err)
			}
		})
	}
}

// TestAResolutionRefusalIsHeldToTheSameEncoding closes the position the walker
// cannot reach.
//
// Refusals is framed by SerializeResolutionArtifact and scanned by
// checkResolutionStringsExpressible, but reachableStrings contributes nothing
// for an EMPTY slice and the walker's fixture carries none -- so a review lane
// deleted the Refusals loop from the gate and the whole suite stayed green.
// Putting a refusal on the walker's WINNER_KNOWN fixture does not work: an
// artifact that names a winner AND carries refusals is not the projection of
// its own facts, and the package refuses it for that instead, which would
// silently retire every assertion in the walker. So this drives an artifact
// that legitimately carries a refusal -- an UNKNOWN one, which is the only kind
// that does -- and pokes the refusal itself.
func TestAResolutionRefusalIsHeldToTheSameEncoding(t *testing.T) {
	const invalid = "\xff\xfe\x80"
	ev := goodWinnerEvidence()
	ev.ProofBasis = "" // earns a refusal on its own terms, so the artifact is honest
	a := p4offline.ProjectResolution(ev)
	if a.Outcome != p4offline.ResolutionUnknown || len(a.Refusals) == 0 {
		t.Fatalf("the fixture must be an artifact that legitimately carries refusals: %+v", a)
	}
	if err := p4offline.VerifyResolutionArtifact(a); err != nil {
		t.Fatalf("the unpoked control must verify: %v", err)
	}
	a.Refusals[0] = invalid
	a.ResolutionFactsDigest = p4offline.DigestReference(digestOf(p4offline.SerializeResolutionArtifact(a)))
	err := p4offline.VerifyResolutionArtifact(a)
	if err == nil {
		t.Fatal("invalid UTF-8 in a refusal was CERTIFIED")
	}
	if !strings.Contains(err.Error(), "is not valid UTF-8") {
		t.Fatalf("want a refusal naming the encoding fault, got %v", err)
	}
	if !strings.Contains(err.Error(), "refusal 0") {
		t.Fatalf("the refusal must name WHICH refusal it refused: %v", err)
	}
}

// artifactCarriesOnlyExpressibleText reports the first string position in a
// resolution artifact that its own encoding cannot carry unchanged.
//
// It walks READ-ONLY. reachableStrings allocates a nil pointer in place so the
// value behind it can be poked, which is right for a poking test and wrong for
// an inspecting one; an inspection built on that walker would change the value
// it is measuring. Its blind spots are the walker's: no map, no interface, and
// an EMPTY slice contributes nothing -- which is why the caller must not rely on
// it to notice an empty Refusals list.
func artifactCarriesOnlyExpressibleText(a p4offline.ResolutionArtifact) (string, bool) {
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
	return walk(reflect.ValueOf(a), "ResolutionArtifact")
}

// TestProjectResolutionNeverMintsAnArtifactItsOwnVerifierRefuses closes the
// producer half of the class the verifier gate above closes.
//
// Gating only VerifyResolutionArtifact left the package's own exported projector
// able to MINT an artifact that fails its own verifier: an invalid-UTF-8
// Round.ChannelID yielded WINNER_KNOWN with NO refusals, retaining the bad
// bytes. That is the same producer/verifier contradiction the factset's
// build-path gate had already removed, and it survived one commit because that
// repair was made at the verifier only.
//
// IT WALKS THE EVIDENCE BY REFLECTION RATHER THAN LISTING ITS FIELDS, and the
// reason is a defect this test had in its first form: the list was written by
// hand and reached ONE of EvidenceReference's six strings. A review lane deleted
// each of the other five in turn and every one survived the whole suite -- with
// keep(&refs[i].Kind) removed it reproduced this commit's own defect verbatim,
// WINNER_KNOWN with no refusals and the bad bytes retained. A hand-written list
// of positions is a list of the positions someone remembered. The registry
// sibling walks; so does the verifier sibling; this one did not, and that is
// exactly where the surviving mutants were.
//
// Each position asserts the three things that together mean "asserts nothing,
// and can be stored": the projection carries the refusal, it does NOT retain the
// unrepresentable bytes, and the artifact verifies and survives a JSON round
// trip. Each also carries its own VALID-WIDE control, because a gate that
// refused legitimate non-ASCII identities would be refusing the very U+FFFD
// substitution it exists to prevent -- and a lane showed that an
// expressibleEvidence widened to do exactly that survived the entire suite.
func TestProjectResolutionNeverMintsAnArtifactItsOwnVerifierRefuses(t *testing.T) {
	const invalid = "\xff\xfe\x80"
	const validWide = "ok-\u00e9-\uFFFD"

	// A POINTER, for the reason the factset sibling records: the walker's sites
	// point into the value it was handed, so returning it by value would hide
	// the mutation of every field not behind a pointer or a slice.
	sites := func() (*p4offline.ResolutionEvidence, []floatSite) {
		ev := goodWinnerEvidence()
		var out []floatSite
		reachableStrings(reflect.ValueOf(&ev).Elem(), "ResolutionEvidence", &out)
		return &ev, out
	}
	_, found := sites()
	// EXACT, like the registry sibling's, and for the same reason: a slack
	// bound lets a position disappear without tripping. It is a property of the
	// FIXTURE together with the type -- 8 scalar positions (the round's two
	// identities, the claim, the winner, the proof basis, the availability and
	// the two revisions), the 2 ordered outcome ids, and the 6 strings of one
	// evidence reference.
	if want := 16; len(found) != want {
		var paths []string
		for _, f := range found {
			paths = append(paths, f.path)
		}
		t.Fatalf("reached %d string positions, want %d: %s", len(found), want, strings.Join(paths, ", "))
	}
	for i, site := range found {
		// Claim is the one framed-adjacent string expressibleEvidence does not
		// touch, and it needs no clause: an unrepresentable claim is outside the
		// closed vocabulary, so the projector refuses it there and never copies
		// it into the artifact. Asserted rather than assumed, below.
		if site.path == "ResolutionEvidence.Claim" {
			continue
		}
		t.Run(site.path, func(t *testing.T) {
			ev, s := sites()
			s[i].at.SetString(invalid)
			a := p4offline.ProjectResolution(*ev)

			if a.Outcome != p4offline.ResolutionUnknown {
				t.Fatalf("outcome = %v, want UNKNOWN: a refused projection must assert nothing", a.Outcome)
			}
			if !containsString(a.Refusals, p4offline.ResolutionRefusalTextNotExpressible) {
				t.Fatalf("refusals = %v, want %s", a.Refusals, p4offline.ResolutionRefusalTextNotExpressible)
			}
			if at, ok := artifactCarriesOnlyExpressibleText(a); !ok {
				t.Fatalf("the projection RETAINED unrepresentable bytes, at %s", at)
			}
			if err := p4offline.VerifyResolutionArtifact(a); err != nil {
				t.Fatalf("the package minted an artifact its own verifier refuses: %v", err)
			}
			var back p4offline.ResolutionArtifact
			if err := json.Unmarshal(mustMarshal(t, a), &back); err != nil {
				t.Fatal(err)
			}
			if err := p4offline.VerifyResolutionArtifact(back); err != nil {
				t.Fatalf("the artifact did not survive its own JSON round trip: %v", err)
			}
		})
		t.Run("valid wide text at "+site.path, func(t *testing.T) {
			ev, s := sites()
			s[i].at.SetString(validWide)
			a := p4offline.ProjectResolution(*ev)
			if containsString(a.Refusals, p4offline.ResolutionRefusalTextNotExpressible) {
				t.Fatalf("a valid multi-byte string was refused for its encoding: %v", a.Refusals)
			}
			if err := p4offline.VerifyResolutionArtifact(a); err != nil {
				t.Fatalf("verify: %v", err)
			}
		})
	}

	// Claim's carve-out, checked rather than asserted.
	t.Run("an unrepresentable claim is refused by the vocabulary, not carried", func(t *testing.T) {
		ev := goodWinnerEvidence()
		ev.Claim = p4offline.ResolutionOutcome(invalid)
		a := p4offline.ProjectResolution(ev)
		if a.Outcome != p4offline.ResolutionUnknown ||
			!containsString(a.Refusals, p4offline.ResolutionRefusalClaimOutsideVocabulary) {
			t.Fatalf("outcome=%v refusals=%v", a.Outcome, a.Refusals)
		}
		if at, ok := artifactCarriesOnlyExpressibleText(a); !ok {
			t.Fatalf("the claim's bytes reached the artifact, at %s", at)
		}
		if err := p4offline.VerifyResolutionArtifact(a); err != nil {
			t.Fatalf("verify: %v", err)
		}
	})

	// The control: valid evidence still projects a WINNER_KNOWN with no
	// refusals, so the gate above is not refusing everything.
	a := p4offline.ProjectResolution(goodWinnerEvidence())
	if a.Outcome != p4offline.ResolutionWinnerKnown || len(a.Refusals) != 0 {
		t.Fatalf("valid evidence = %v with refusals %v, want WINNER_KNOWN and none", a.Outcome, a.Refusals)
	}

	// ResolutionNotRecorded is the file's OTHER exported producer. It builds no
	// artifact of its own -- it delegates to ProjectResolution and so inherits
	// the gate -- and that delegation is exactly what wants pinning: a later
	// refactor that constructed the artifact inline here would reopen the hole
	// silently, with every test above still green.
	t.Run("ResolutionNotRecorded inherits the gate", func(t *testing.T) {
		a := p4offline.ResolutionNotRecorded(
			p4offline.PublicRoundIdentity{EventID: "e", ChannelID: invalid},
			[]string{"o1", invalid},
			[]p4offline.EvidenceReference{{ObservationID: invalid, Kind: "k"}},
			"pr")
		if a.Outcome != p4offline.ResolutionUnknown {
			t.Fatalf("outcome = %v, want UNKNOWN", a.Outcome)
		}
		if !containsString(a.Refusals, p4offline.ResolutionRefusalTextNotExpressible) {
			t.Fatalf("refusals = %v, want %s", a.Refusals, p4offline.ResolutionRefusalTextNotExpressible)
		}
		if a.Round.ChannelID == invalid || a.OrderedOutcomeIDs[1] == invalid ||
			a.EvidenceReferences[0].ObservationID == invalid {
			t.Fatalf("the projection RETAINED the unrepresentable bytes: %+v", a)
		}
		if err := p4offline.VerifyResolutionArtifact(a); err != nil {
			t.Fatalf("the package minted an artifact its own verifier refuses: %v", err)
		}
		var back p4offline.ResolutionArtifact
		if err := json.Unmarshal(mustMarshal(t, a), &back); err != nil {
			t.Fatal(err)
		}
		if err := p4offline.VerifyResolutionArtifact(back); err != nil {
			t.Fatalf("the artifact did not survive its own JSON round trip: %v", err)
		}
	})

	// AND THE CALLER'S EVIDENCE IS NOT TOUCHED. ProjectResolution takes its
	// argument by value, which protects the scalars and protects nothing else:
	// OrderedOutcomeIDs and EvidenceReferences are slices, so blanking an
	// element in place would reach through the copy and edit the caller's own
	// value. That is the harder half of "drop rather than carry" -- a producer
	// that silently rewrote its input would be a worse defect than the one it
	// was added to fix -- and it is the shape of the aliasing trap this
	// package's reflection walkers hit one round earlier, so it is asserted
	// rather than assumed.
	t.Run("the caller's evidence is not rewritten", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			place func(*p4offline.ResolutionEvidence)
		}{
			{"an ordered outcome id", func(e *p4offline.ResolutionEvidence) { e.OrderedOutcomeIDs[1] = invalid }},
			{"an evidence reference observation id",
				func(e *p4offline.ResolutionEvidence) { e.EvidenceReferences[0].ObservationID = invalid }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ev := goodWinnerEvidence()
				tc.place(&ev)
				before := p4offline.ResolutionEvidence{
					OrderedOutcomeIDs:  append([]string(nil), ev.OrderedOutcomeIDs...),
					EvidenceReferences: append([]p4offline.EvidenceReference(nil), ev.EvidenceReferences...),
				}
				_ = p4offline.ProjectResolution(ev)
				if !reflect.DeepEqual(ev.OrderedOutcomeIDs, before.OrderedOutcomeIDs) ||
					!reflect.DeepEqual(ev.EvidenceReferences, before.EvidenceReferences) {
					t.Fatalf("ProjectResolution edited the caller's evidence through a shared slice:\n before %+v / %+v\n  after %+v / %+v",
						before.OrderedOutcomeIDs, before.EvidenceReferences, ev.OrderedOutcomeIDs, ev.EvidenceReferences)
				}
			})
		}
	})
}
