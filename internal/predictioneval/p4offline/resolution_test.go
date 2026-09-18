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
	"runtime"
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

// TestARefusalBelowADigestDoesNotFrameItsSupplyAgain holds refusal branches
// that sit BELOW a digest comparison.
//
// THE EXPOSURE IS A DOUBLING, not the class the shape gates closed. Reaching
// any of these means the digest already verified, so one framing of the payload
// is paid and unavoidable; what a second framing inside the refusal buys is a
// second copy of the whole payload for a refusal that reads none of it. It is
// smaller than a constant-size field amplified by the payload, and it is the
// same rule.
//
// THE ROWS BELOW ARE WHAT IS HELD, and this comment does not claim they are the
// whole set. Each is driven to its own named refusal, so a row that stops
// reaching its branch fails rather than passing vacuously.
//
// THREE SUCH BRANCHES ARE HELD ELSEWHERE, by the `belowDigest` rows of
// TestFirstGatesDoNotMaterializeSuppliedText: checkFactsetConsistency's
// stealth-proof arm, its completeness default and the artifact's outcome
// default. Those three render a caller's EXTENT, which is what lets that table
// hold them. ELEVEN of the rows below RENDER NO EXTENT, so they cannot satisfy
// its `names` and `wantExtent` assertions, and relaxing those to admit them
// would weaken every row it has. Most of the eleven are constant text
// outright. TWO ARE NOT, and they are named because the distinction is easy to
// lose: the artifact's digest comparison quotes both digests, and its
// re-projection arm renders the outcome and the joined refusals. Neither
// renders an EXTENT, which is the property this paragraph turns on, and
// "constant text" would be the wrong test for membership of this table.
// The twelfth is the completeness default, which renders an extent like the
// other two and is therefore held in BOTH places -- by that table for what its
// message says, and here for what reaching it costs.
func TestARefusalBelowADigestDoesNotFrameItsSupplyAgain(t *testing.T) {
	big := strings.Repeat("a", 1<<20)
	measure := func(f func() error) (uint64, error) {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		err := f()
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc, err
	}
	// A FLOOR BESIDE EVERY CEILING. Each budget below is a ceiling, and a
	// ceiling alone passes when the payload is not framed on the measured path
	// at all -- and so does the second framing it exists to catch. Stopping
	// SerializeCommonFactset and SerializeResolutionArtifact from writing
	// ProjectorRevision costs the ceilings nothing: take the floors away and
	// every row here passes with that mutant applied. Put them back and every row
	// that measures a framing of `big` fails. This is that guard, expressed as
	// cost because these refusals render no extent to assert: reaching any of
	// them costs at least the framings named, less a tenth for the allocator's
	// rounding.
	//
	// THE SIBLING TABLE CANNOT SEE THAT MUTANT AT ALL, which is the reason these
	// budgets are here and not there. Its `wantExtent` rows assert the extent
	// rendered for StealthProof, Completeness and Outcome, and an unwritten
	// ProjectorRevision changes none of those messages. Tests elsewhere in the
	// package DO fail under it -- both independent goldens among them -- but no
	// row of that table does. No count is given for that set: it is whatever the
	// suite happens to contain, which is not a property this test holds.
	//
	// ONE ROW BELOW DOES NOT USE floorOf: the registry row's payload is not
	// `big`, so its floor is the lower side of its own ratio, written there.
	floorOf := func(t *testing.T, allocated uint64, framings int) {
		t.Helper()
		if want := uint64(framings) * uint64(len(big)) * 9 / 10; allocated < want {
			t.Fatalf("the refusing call allocated %d bytes, below the %d framing(s) of a %d-byte payload it must pay to REACH this branch (floor %d): the fixture's payload is not on the measured path, so the ceiling below proves nothing",
				allocated, framings, len(big), want)
		}
	}

	t.Run("an UNKNOWN artifact that names a winner", func(t *testing.T) {
		a := p4offline.ProjectResolution(p4offline.ResolutionEvidence{
			Round: p4offline.PublicRoundIdentity{EventID: "e1"}, OrderedOutcomeIDs: []string{"o1", "o2"},
			Claim: p4offline.ResolutionUnknown, Availability: p4offline.AvailabilityNotRecorded,
			ProjectorRevision: big, ProofRevision: "x"})
		if err := p4offline.VerifyResolutionArtifact(a); err != nil {
			t.Fatalf("the unpoked control must verify: %v", err)
		}
		a.WinnerOutcomeID = "o1"
		a.ResolutionFactsDigest = p4offline.DigestReference(digestOf(p4offline.SerializeResolutionArtifact(a)))
		allocated, err := measure(func() error { return p4offline.VerifyResolutionArtifact(a) })
		t.Logf("a %d-byte artifact; the refusing call allocated %d bytes", len(big), allocated)
		if !errors.Is(err, p4offline.ErrResolutionNotDerivable) || !strings.Contains(err.Error(), "names a winner") {
			t.Fatalf("this fixture must reach the winner branch, or the budget below proves nothing: %v", err)
		}
		// ONE FRAMING, NOT TWO. Reaching this branch means the digest already
		// verified, so one framing of the payload is paid and unavoidable:
		// the refusal measures 1.01x the payload. A second framing inside the
		// branch measures 2.02x. The line sits between them at 1.5x, which is
		// half the payload of margin on either side.
		floorOf(t, allocated, 1)
		if allocated > uint64(len(big))+uint64(len(big))/2 {
			t.Fatalf("the refusal allocated %d bytes on a %d-byte payload: reaching it costs one framing, and refusing must not add another",
				allocated, len(big))
		}
	})

	t.Run("an artifact whose facts do not re-project", func(t *testing.T) {
		// THE RE-PROJECTION ARM, which a review lane measured at 3.03x the
		// payload under a second framing and which nothing held.
		a := p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, nil, big)
		a.Outcome, a.WinnerOutcomeID, a.WinnerIndex = p4offline.ResolutionWinnerKnown, "o1", 0
		a.ResolutionFactsDigest = p4offline.DigestReference(digestOf(p4offline.SerializeResolutionArtifact(a)))
		allocated, err := measure(func() error { return p4offline.VerifyResolutionArtifact(a) })
		t.Logf("a %d-byte artifact; the refusing call allocated %d bytes", len(big), allocated)
		if !errors.Is(err, p4offline.ErrResolutionNotDerivable) || !strings.Contains(err.Error(), "re-projecting") {
			t.Fatalf("this fixture must reach the re-projection arm: %v", err)
		}
		// TWO FRAMINGS ARE THE FLOOR HERE, not one: reaching this arm means
		// the artifact was framed for its digest AND re-projected, and the
		// re-projection frames it again to compare. The honest reading is
		// 2.02x; a third framing inside the refusal measures 3.03x. The line
		// sits between them at 2.5x.
		floorOf(t, allocated, 2)
		if allocated > 5*uint64(len(big))/2 {
			t.Fatalf("the refusal allocated %d bytes on a %d-byte payload: reaching it costs two framings, and refusing must not add a third",
				allocated, len(big))
		}
	})

	t.Run("an artifact whose digest is well-formed and wrong", func(t *testing.T) {
		// THE COMPARISON ITSELF is a below-digest branch too -- it has paid the
		// derivation it compares against -- and it was the only one of the
		// three siblings with no budget. The factset's is held by
		// TestTheValueGateAddsNoPerOutcomeAllocation and the registry's by
		// TestRegistryRefusesOnItsConstantsBeforeItReconciles.
		a := p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, nil, big)
		a.ResolutionFactsDigest = p4offline.DigestReference(strings.Repeat("b", 64))
		allocated, err := measure(func() error { return p4offline.VerifyResolutionArtifact(a) })
		t.Logf("a %d-byte artifact; the refusing comparison allocated %d bytes", len(big), allocated)
		if !errors.Is(err, p4offline.ErrResolutionDigest) || !strings.Contains(err.Error(), "does not match the artifact") {
			t.Fatalf("this fixture must reach the digest comparison: %v", err)
		}
		floorOf(t, allocated, 1)
		if allocated > uint64(len(big))+uint64(len(big))/2 {
			t.Fatalf("the refusal allocated %d bytes on a %d-byte payload: the comparison has paid one derivation, and refusing must not pay another",
				allocated, len(big))
		}
	})

	t.Run("every arm of checkFactsetConsistency but the stealth-proof one", func(t *testing.T) {
		// ONE ROW PER ARM, each asserted by the sentence it returns, because a
		// poke that stops reaching its arm would otherwise pass this budget
		// while measuring a different refusal entirely. The stealth-proof arm
		// is the ninth and is not here: it renders the caller's extent, so
		// TestFirstGatesDoNotMaterializeSuppliedText holds it as a row.
		_, _, good := selectedCase(t, nil, nil)
		good.ProjectorRevision = big
		for _, tc := range []struct {
			name string
			poke func(*p4offline.CommonFactset)
			says string
		}{
			{"COMPLETE without a reached decision", func(fs *p4offline.CommonFactset) {
				fs.Completeness, fs.ReachedDecision, fs.IncompleteReasons = p4offline.FactsetComplete, false, nil
			}, "COMPLETE beside a decision that was not reached"},
			{"COMPLETE beside incompleteness reasons", func(fs *p4offline.CommonFactset) {
				fs.Completeness, fs.ReachedDecision = p4offline.FactsetComplete, true
				fs.IncompleteReasons = []string{predictioneval.IneligibleMissingBalance}
			}, "COMPLETE beside incompleteness reasons"},
			{"COMPLETE beside a value-derived reason", func(fs *p4offline.CommonFactset) {
				fs.Completeness, fs.ReachedDecision, fs.IncompleteReasons = p4offline.FactsetComplete, true, nil
				fs.BalancePresent = false
			}, "COMPLETE beside "},
			{"PRE_DECISION_EXIT beside a reached decision", func(fs *p4offline.CommonFactset) {
				fs.Completeness, fs.ReachedDecision = p4offline.FactsetPreDecisionExit, true
			}, "PRE_DECISION_EXIT beside a reached decision"},
			{"INCOMPLETE without a reached decision", func(fs *p4offline.CommonFactset) {
				fs.Completeness, fs.ReachedDecision = p4offline.FactsetIncomplete, false
			}, "INCOMPLETE beside a decision that was not reached"},
			{"INCOMPLETE without a reason", func(fs *p4offline.CommonFactset) {
				fs.Completeness, fs.ReachedDecision, fs.IncompleteReasons = p4offline.FactsetIncomplete, true, nil
			}, "INCOMPLETE without a reason"},
			{"INCOMPLETE without its value-derived reason", func(fs *p4offline.CommonFactset) {
				fs.Completeness, fs.ReachedDecision = p4offline.FactsetIncomplete, true
				fs.IncompleteReasons = []string{predictioneval.IneligibleMissingSettings}
				fs.BalancePresent = false
			}, "INCOMPLETE without the value-derived reason "},
			{"a completeness outside the vocabulary", func(fs *p4offline.CommonFactset) {
				fs.Completeness = "NOT_A_COMPLETENESS_THIS_PACKAGE_WRITES"
			}, "is outside the vocabulary"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fs := good
				tc.poke(&fs)
				fs.Digest = digestOf(p4offline.SerializeCommonFactset(fs))
				allocated, err := measure(func() error { return p4offline.VerifyCommonFactset(fs) })
				t.Logf("a %d-byte factset; the refusing call allocated %d bytes", len(big), allocated)
				if !errors.Is(err, p4offline.ErrFactsetInconsistent) || !strings.Contains(err.Error(), tc.says) {
					t.Fatalf("this poke must reach the arm that says %q: %v", tc.says, err)
				}
				floorOf(t, allocated, 1)
				if allocated > uint64(len(big))+uint64(len(big))/2 {
					t.Fatalf("the refusal allocated %d bytes on a %d-byte payload: reaching it costs one framing, and refusing must not add another",
						allocated, len(big))
				}
			})
		}
	})

	t.Run("a registry that does not re-derive from its entries", func(t *testing.T) {
		claims := make([]p4offline.SourceRoundClaim, 200)
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
		reg.Entries[0].Status = "INVALID"
		reg = restampRegistry(reg)
		// BOTH FIXTURES ARE BUILT HERE, never inside a measured closure: built
		// inside, the honest reading is dominated by its own construction and the
		// ratio measures that instead of the verification.
		honestReg := restampRegistry(p4offline.ReconcileSourceRounds(claims))
		allocated, err := measure(func() error { return p4offline.VerifySourceRoundRegistry(reg) })
		honest, honestErr := measure(func() error { return p4offline.VerifySourceRoundRegistry(honestReg) })
		t.Logf("the refusing call allocated %d bytes; an honest verification allocated %d", allocated, honest)
		if honestErr != nil {
			t.Fatalf("the denominator must be an honest verification: %v", honestErr)
		}
		if !errors.Is(err, p4offline.ErrSourceRoundRegistry) || strings.Contains(err.Error(), "\n") {
			t.Fatalf("this fixture must reach the BARE re-reconciliation, or the budget below proves nothing: %v", err)
		}
		// AGAINST AN HONEST VERIFICATION, because this refusal is
		// payload-proportional by construction -- it flattens and reconciles
		// before it can know the answer -- so a constant will not do. The two
		// paths do the SAME work and differ only in what they return: registryDigest
		// runs above the re-reconciliation, so the refusal pays it too, and the
		// refusal measures 1.00x an honest verification. A second registryDigest
		// inside the branch measures 1.44x. The ceiling sits between them.
		//
		// THE FLOOR IS THE OTHER SIDE OF THE SAME RATIO, and it is what this row
		// has instead of floorOf, whose unit is a framing of `big`: a refusal that
		// stopped reaching the re-reconciliation -- a short-circuit that answered
		// from a cached digest, say -- would read far BELOW an honest verification,
		// and a ceiling alone would call that a pass.
		//
		// NO MUTANT IN THIS ROUND'S CAMPAIGN MOVES THAT FLOOR, and the record is
		// here rather than in a claim that one does: both paths run the same code
		// and differ only in what they return, so lowering the numerator means
		// writing the short-circuit, not editing what is here. It guards the change
		// that has not been made yet.
		if allocated*5 > honest*6 {
			t.Fatalf("the refusal allocated %d bytes against %d for an honest verification: reaching it pays the framing once, and refusing must not pay it twice",
				allocated, honest)
		}
		if allocated*10 < honest*9 {
			t.Fatalf("the refusal allocated %d bytes against %d for an honest verification: it is not paying a whole verification's framing, so it is not on the measured path and the ceiling above proves nothing",
				allocated, honest)
		}
	})
}

// TestAMalformedResolutionDigestIsRefusedBeforeTheArtifactIsFramed is the
// resolution half of the same rule; see the registry sibling for the finding.
//
// Before the repair a producer-impossible digest was paid for at the price of
// the whole artifact: the verifier scanned every reference and serialized the
// artifact before comparing, and a well-formed wrong digest cost exactly the
// same. resolutionDigest emits a DigestReference, so anything else is
// refusable in O(1). The figures are written once, at the gate itself in
// resolution.go; what this test asserts is the RATIO, which is what survives a
// change of fixture.
//
// IT IS NAMED FOR THE FRAMING, and not for the walk, because the framing is
// what a cost instrument can prove here. The gate moved BELOW
// checkResolutionStringsExpressible leaves this test green: that scan ALLOCATES
// NOTHING -- 0 B/op, 0 allocs/op on this fixture, about a millisecond of CPU --
// so no byte ratio can see the move whatever its denominator. The order against
// the scan is pinned by behaviour instead, in
// TestADigestsSHAPEIsJudgedAboveTheScanThatReadsTheSupply.
func TestAMalformedResolutionDigestIsRefusedBeforeTheArtifactIsFramed(t *testing.T) {
	refs := make([]p4offline.EvidenceReference, 20000)
	ids := make([]string, 20000)
	for i := range refs {
		refs[i] = p4offline.EvidenceReference{ObservationID: "obs" + strconv.Itoa(i), Kind: "k", Phase: "p", RoundState: "RESOLVED", EventID: "e1"}
		ids[i] = "o" + strconv.Itoa(i)
	}
	a := p4offline.ProjectResolution(p4offline.ResolutionEvidence{
		Round: p4offline.PublicRoundIdentity{EventID: "e1"}, OrderedOutcomeIDs: ids,
		Claim: p4offline.ResolutionUnknown, Availability: p4offline.AvailabilityNotRecorded,
		EvidenceReferences: refs, ProjectorRevision: "pr", ProofRevision: "x"})
	if err := p4offline.VerifyResolutionArtifact(a); err != nil {
		t.Fatalf("the unpoked control must verify: %v", err)
	}
	malformed := a
	malformed.ResolutionFactsDigest = "x"
	// WELL-FORMED BUT WRONG, and the easy way to write it is wrong: because
	// DigestReference PREPENDS the prefix, DigestReference(prefix + hex) yields
	// a 78-character double-prefixed value the shape gate rightly refuses, and
	// the comparison below would then be between two malformed digests and
	// would pass for the wrong reason. The lengths are asserted so the fixture
	// cannot drift into that shape.
	wrong := a
	wrong.ResolutionFactsDigest = p4offline.DigestReference(strings.Repeat("b", 64))
	if len(wrong.ResolutionFactsDigest) != len(a.ResolutionFactsDigest) {
		t.Fatalf("the wrong digest must have the SHAPE of a real one: %d against %d",
			len(wrong.ResolutionFactsDigest), len(a.ResolutionFactsDigest))
	}
	// THE COMPARISON NAMES BOTH DIGESTS, on the shape gate's precondition, as
	// its two siblings do -- and in the right order, because both are
	// DigestReferences and swapping them reads perfectly while saying the
	// opposite. Containment cannot see that; position can.
	werr := p4offline.VerifyResolutionArtifact(wrong)
	if !errors.Is(werr, p4offline.ErrResolutionDigest) {
		t.Fatalf("a well-formed WRONG digest must be refused by the comparison: %v", werr)
	}
	if !strings.Contains(werr.Error(), strconv.Quote(wrong.ResolutionFactsDigest)) {
		t.Fatalf("the supplied digest is gated to a reference above and must be NAMED: %v", werr)
	}
	if strings.Contains(werr.Error(), "71 bytes") {
		t.Fatalf("a gated digest reported by extent reads the same for every mismatch: %v", werr)
	}
	if !strings.Contains(werr.Error(), strconv.Quote(a.ResolutionFactsDigest)) {
		t.Fatalf("the refusal must also name the digest the artifact produces: %v", werr)
	}
	if at, intro := strings.Index(werr.Error(), strconv.Quote(wrong.ResolutionFactsDigest)),
		strings.Index(werr.Error(), "which digests to "); intro < 0 || at > intro {
		t.Fatalf("the SUPPLIED digest must be named before the derived one: %v", werr)
	}
	if !strings.HasPrefix(werr.Error(), p4offline.ErrResolutionDigest.Error()) {
		t.Fatalf("the refusal must lead with the class it joins: %v", werr)
	}
	if !errors.Is(p4offline.VerifyResolutionArtifact(malformed), p4offline.ErrResolutionDigest) ||
		!errors.Is(p4offline.VerifyResolutionArtifact(wrong), p4offline.ErrResolutionDigest) {
		t.Fatal("both shapes of bad digest must be refused")
	}
	// MEASURED IN BYTES RATHER THAN IN ALLOCATION COUNT, for the MARGIN and not
	// because the count is blind. The count discriminates in both directions:
	// before the gate a malformed digest cost 44 allocations against an honest
	// verification's 41, and with the gate it costs 4 against the same 41. What
	// differs is how much room the assertion has. An honest verification grows
	// a few large buffers rather than many small ones, so the same pair spans a
	// factor near fifty thousand in BYTES against a factor of ten in the count
	// -- and the hundredfold threshold below is satisfiable only in bytes: 4
	// allocations against 41 would fail it while the gate is working. The
	// registry sibling counts allocations because its cost is per-claim
	// framing, which is the opposite shape.
	// MEASURED ONCE EACH, with the instrument TestFirstGatesDoNotMaterializeSuppliedText
	// already uses, rather than by benchmarking. testing.Benchmark runs each
	// function for about a second of WALL time whatever the fixture costs, so
	// the two calls it takes here cost SEVERAL SECONDS under the race detector.
	// No figure is quoted because none would mean anything: the floor is set by
	// that wall-clock target and the honest half swings with b.N. Shrinking the
	// fixture would
	// not move it either way, since the time is fixed and only the iteration
	// count changes. A single
	// pair of readings answers the same question: the gap being asserted is
	// four orders of magnitude, so a few kilobytes of bookkeeping slop cannot
	// reach it.
	measure := func(f func()) uint64 {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		f()
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	cheap := measure(func() { _ = p4offline.VerifyResolutionArtifact(malformed) })
	honest := measure(func() { _ = p4offline.VerifyResolutionArtifact(a) })
	t.Logf("a malformed digest allocated %d bytes; an honest verification allocated %d", cheap, honest)
	if honest < 1<<20 {
		t.Fatalf("the fixture must make an honest verification expensive, got %d bytes", honest)
	}
	if cheap*100 > honest {
		t.Fatalf("a one-byte malformed digest costs %d bytes against %d for an honest verification: "+
			"a constant-size malformed field must not buy work proportional to the payload", cheap, honest)
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
