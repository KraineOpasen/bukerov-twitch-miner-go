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
			flip := "0"
			if x.ResolutionFactsDigest[0] == '0' {
				flip = "1"
			}
			x.ResolutionFactsDigest = flip + x.ResolutionFactsDigest[1:]
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
	a.ResolutionFactsDigest = digestOf(p4offline.SerializeResolutionArtifact(a))
	return a
}

// TestResolutionVerificationRederivesTheOutcome pins that a matching digest
// is not enough: a WINNER_KNOWN or REFUND artifact must be exactly what
// projecting its own facts produces.
func TestResolutionVerificationRederivesTheOutcome(t *testing.T) {
	forged := forgedWinnerArtifact("o2")
	if got := digestOf(p4offline.SerializeResolutionArtifact(forged)); got != forged.ResolutionFactsDigest {
		t.Fatalf("fixture: the forged digest must be consistent")
	}
	if err := p4offline.VerifyResolutionArtifact(forged); !errors.Is(err, p4offline.ErrResolutionNotDerivable) {
		t.Fatalf("got %v", err)
	}
	// A genuine artifact serializes to the bytes its digest covers.
	a := p4offline.ProjectResolution(goodWinnerEvidence())
	if digestOf(p4offline.SerializeResolutionArtifact(a)) != a.ResolutionFactsDigest {
		t.Fatal("the exported serialization is not what the digest covers")
	}
	// A REFUND asserted without its proof is refused the same way.
	refund := p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, nil, "p")
	refund.Outcome = p4offline.ResolutionRefund
	refund.Refusals = nil
	refund.ResolutionFactsDigest = digestOf(p4offline.SerializeResolutionArtifact(refund))
	if err := p4offline.VerifyResolutionArtifact(refund); !errors.Is(err, p4offline.ErrResolutionNotDerivable) {
		t.Fatalf("got %v", err)
	}
	// An UNKNOWN artifact that names a winner is not an UNKNOWN artifact.
	unknown := p4offline.ResolutionNotRecorded(p4offline.PublicRoundIdentity{EventID: "e1"}, []string{"o1", "o2"}, nil, "p")
	unknown.WinnerOutcomeID = "o1"
	unknown.WinnerIndex = 0
	unknown.ResolutionFactsDigest = digestOf(p4offline.SerializeResolutionArtifact(unknown))
	if err := p4offline.VerifyResolutionArtifact(unknown); !errors.Is(err, p4offline.ErrResolutionNotDerivable) {
		t.Fatalf("got %v", err)
	}
	// An outcome outside the vocabulary is refused whatever its digest.
	odd := a
	odd.Outcome = "PROBABLY_O2"
	odd.ResolutionFactsDigest = digestOf(p4offline.SerializeResolutionArtifact(odd))
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
