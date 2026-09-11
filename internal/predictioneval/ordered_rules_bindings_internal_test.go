package predictioneval

// Digest-binding guards, INTERNAL on purpose.
//
// These ask one question: does orderedRulesStreamDigest bind this field? The
// external versions asked it through EvaluateOrderedRules, taking the digest
// off the returned result — which made them depend on the evaluator computing
// the digest before anything else could refuse the probe. That dependency was
// not part of the property, and it was load-bearing enough to block a repair:
// deferring the digest behind the stream's structural tier made every
// structural field refuse first, and the walk could no longer tell "the digest
// binds this field" from "the structure refused it".
//
// Calling the digest directly removes the coupling. The probe does not have to
// be a stream anything would admit; it only has to differ in one field.

import (
	"reflect"
	"testing"
)

func internalProbeBalance(value int64) SuppliedInt64 {
	return SuppliedInt64{
		Presence:               SuppliedKnown,
		Value:                  value,
		Provenance:             "binding probe",
		HasAvailableAtPosition: true,
	}
}

func internalProbeStream(balance SuppliedInt64) OrderedRulesStream {
	return OrderedRulesStream{
		ContractVersion: OrderedRulesStreamContractVersion,
		Candidates: []OrderedRulesCandidate{{
			Identity:          "c1",
			Position:          10,
			HasPosition:       true,
			SourceKind:        SourceKindChannelUpdate,
			EpisodeMembership: MembershipProven,
			OutcomesPresence:  SuppliedKnown,
			Provenance:        "binding probe",
			Balance:           balance,
			Outcomes: []OrderedRulesOutcome{
				{Identity: "A", Points: SuppliedInt64{Presence: SuppliedKnown, Value: 4,
					Provenance: "binding probe", HasAvailableAtPosition: true}},
				{Identity: "B", Points: SuppliedInt64{Presence: SuppliedKnown, Value: 6,
					Provenance: "binding probe", HasAvailableAtPosition: true}},
			},
		}},
	}
}

// TestSuppliedDigestBindsEveryFieldThatCouldChangeADecision walks SuppliedInt64
// by reflection so a field added later cannot go unbound quietly.
//
// This exists because that is exactly what happened: HasAvailableAtPosition was
// added to the type and not to the digest, and every hand-written binding test
// kept passing because none of them knew the field existed. A field-by-field
// walk does not depend on anyone remembering.
//
// Exemptions are listed with the reason they are exempt, and the reason is
// always the same question: can the field change what the model is allowed to
// do? A leftover Reason on a KNOWN value cannot — it is the explanation slot
// for an absence that did not happen — so two streams differing only there
// carry the same evidence. The availability declaration CAN, which is why it is
// not on this list. The list is checked in BOTH directions: an exemption that
// turns out to be false fails here too.
func TestSuppliedDigestBindsEveryFieldThatCouldChangeADecision(t *testing.T) {
	exempt := map[string]string{
		"Reason": "a KNOWN value's Reason explains an absence that did not happen; it cannot " +
			"change what the model may do, and the MISSING branch binds it where it can",
	}

	base := internalProbeBalance(1000)
	base.Reason = "a leftover explanation"
	baseline := orderedRulesStreamDigest(internalProbeStream(base))
	if baseline == "" {
		t.Fatal("the probe digest is empty, so this test compares nothing")
	}

	typ := reflect.TypeOf(base)
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		t.Run(f.Name, func(t *testing.T) {
			// PRESENCE is probed differently, and the reason is the defect
			// this case had on the head before. digestSupplied hashes the
			// presence word and then BRANCHES on it: a KNOWN value binds the
			// value, provenance and availability, and a non-KNOWN one binds
			// the reason instead. Appending to "KNOWN" produces a word that is
			// not KNOWN, so it crosses that branch and the digest moves
			// because a different set of fields is hashed — which it does even
			// with the presence binding deleted. Verified: removing
			// digestPart(h, string(v.Presence)) left this whole walk green.
			//
			// MISSING against INVALID stays on one side of the branch, with
			// the reason held equal, so the only thing that can move the
			// digest is the word itself. The two are a real distinction: they
			// produce different balance results downstream.
			if f.Name == "Presence" {
				same := "the same explanation either way"
				missing := SuppliedInt64{Presence: SuppliedMissing, Reason: same}
				invalid := SuppliedInt64{Presence: SuppliedInvalid, Reason: same}
				if orderedRulesStreamDigest(internalProbeStream(missing)) ==
					orderedRulesStreamDigest(internalProbeStream(invalid)) {
					t.Fatal("MISSING and INVALID digest identically with the same reason, so the " +
						"presence word itself is not bound. The two are different evidence — a " +
						"value nobody supplied against one that arrived unusable — and a stream " +
						"can be moved between them without its digest noticing.")
				}
				return
			}

			mutated := base
			v := reflect.ValueOf(&mutated).Elem().Field(i)
			switch v.Kind() {
			case reflect.String:
				v.SetString(v.String() + "-changed")
			case reflect.Int64:
				v.SetInt(v.Int() + 1)
			case reflect.Bool:
				v.SetBool(!v.Bool())
			default:
				t.Fatalf("field %s has kind %s, which this walk does not know how to change; "+
					"teach it rather than leaving the field unchecked", f.Name, v.Kind())
			}

			moved := orderedRulesStreamDigest(internalProbeStream(mutated)) != baseline
			why, isExempt := exempt[f.Name]
			switch {
			case moved && isExempt:
				t.Fatalf("%s is listed as exempt (%s) but the digest DOES bind it; remove the "+
					"exemption rather than leaving a false one", f.Name, why)
			case !moved && !isExempt:
				t.Fatalf("changing %s left the stream digest identical, so nothing binds it. Either "+
					"hash it, or add it to the exemption list with the reason it cannot change a "+
					"decision.", f.Name)
			}
		})
	}
}

// TestDigestBindsTheAvailabilityDeclaration is the named case behind the walk
// above, kept because it is the one the P1 was actually about.
//
// Inside a projected stream the flag is always set, because the projection
// refuses a KNOWN value without it. That is precisely what made leaving it
// unhashed look like decoration — and it was not. EvaluateOrderedRules takes
// the stream BY VALUE, so the digest is what stands between it and a stream the
// projection never produced. A flag the digest ignores can be stripped from a
// projected stream, after which the evaluator reads a balance whose
// availability was never declared while the digest still matches.
func TestDigestBindsTheAvailabilityDeclaration(t *testing.T) {
	declared := internalProbeBalance(1000)
	stripped := declared
	stripped.HasAvailableAtPosition = false

	if orderedRulesStreamDigest(internalProbeStream(declared)) ==
		orderedRulesStreamDigest(internalProbeStream(stripped)) {
		t.Fatal("stripping the availability declaration left the stream digest identical, so a " +
			"projected stream can be stripped of it and still verify — which is the causal " +
			"guard undone at the one seam it was supposed to survive")
	}
}

// TestDigestsBindTheMandatoryFieldDeclarations covers the three declarations
// that make a previously unconditional field refusable: a candidate's
// HasPosition, a scope's HasInterval and a config's HasDefault.
//
// It is INTERNAL for the same reason the walk above is, and the external
// version it replaces is why the reason matters. That one asserted through
// EvaluateOrderedRules and compared the StreamDigest it returned — sound while
// the digest was computed before the invariants, and vacuous the moment the
// structural tier moved ahead of it, because a stripped declaration is then
// refused with an EMPTY digest and "" differs from the baseline no matter what
// the digest binds. Verified: with all three bindings deleted, every case of
// the external version passed.
//
// Calling the digest helpers directly removes the dependency. Nothing about
// these three properties ever needed an evaluation.
func TestDigestsBindTheMandatoryFieldDeclarations(t *testing.T) {
	scoped := func(s OrderedRulesStream) OrderedRulesStream {
		s.Scope = OrderedRulesScope{
			Namespace:             "binding probe",
			EpisodeID:             "episode-1",
			AccountContext:        "account",
			AssociationEvidence:   "evidence",
			SourceContractVersion: OrderedRulesStreamContractVersion,
			Coverage:              CoverageCompleteDeclared,
			IntervalFromPosition:  0,
			IntervalToPosition:    100,
			HasInterval:           true,
		}
		return s
	}

	t.Run("candidate position declaration", func(t *testing.T) {
		declared := scoped(internalProbeStream(internalProbeBalance(1000)))
		stripped := scoped(internalProbeStream(internalProbeBalance(1000)))
		stripped.Candidates[0].HasPosition = false
		if orderedRulesStreamDigest(declared) == orderedRulesStreamDigest(stripped) {
			t.Fatal("stripping HasPosition left the stream digest unchanged, so a projected " +
				"stream can lose its position declaration and still verify")
		}
	})

	t.Run("scope interval declaration", func(t *testing.T) {
		declared := scoped(internalProbeStream(internalProbeBalance(1000)))
		stripped := scoped(internalProbeStream(internalProbeBalance(1000)))
		stripped.Scope.HasInterval = false
		if orderedRulesStreamDigest(declared) == orderedRulesStreamDigest(stripped) {
			t.Fatal("stripping HasInterval left the stream digest unchanged, so a projected " +
				"stream can lose its interval declaration and still verify")
		}
	})

	t.Run("config default declaration", func(t *testing.T) {
		declared := OrderedRulesConfig{ConfigID: "binding probe", HasDefault: true,
			Default: OrderedRulesDefault{RawMinPercent: 95, RawMaxPercent: 100}}
		stripped := declared
		stripped.HasDefault = false
		if orderedRulesConfigDigest(declared) == orderedRulesConfigDigest(stripped) {
			t.Fatal("stripping HasDefault left the config digest unchanged, so a config can lose " +
				"the declaration that its default exists and still verify")
		}
	})
}
