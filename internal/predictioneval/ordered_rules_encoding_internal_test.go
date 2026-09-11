package predictioneval

// The one claim the budget rests on, checked against the encoder itself.

import (
	"encoding/json"
	"testing"
)

// TestOrderedRulesNoByteEncodesWiderThanTheChargedExpansion proves the constant
// the whole aggregate budget now depends on.
//
// chargedWidth multiplies by orderedRulesMaxJSONExpansion instead of decoding
// UTF-8, which is only sound while no input byte can encode wider than that. It
// is asserted against encoding/json rather than reasoned about, because the
// escaping rules are the encoder's and not this package's to restate — this is
// a test file, so importing json here costs the production fence nothing.
func TestOrderedRulesNoByteEncodesWiderThanTheChargedExpansion(t *testing.T) {
	widest := 0
	var widestByte byte
	for i := 0; i < 256; i++ {
		b := byte(i)
		encoded, err := json.Marshal(string([]byte{b}))
		if err != nil {
			t.Fatalf("marshalling byte %#02x failed: %v", b, err)
		}
		// Minus the two surrounding quotes.
		w := len(encoded) - 2
		if w > orderedRulesMaxJSONExpansion {
			t.Fatalf("byte %#02x encodes to %d bytes, past the charged expansion of %d; every "+
				"budget in this package would then admit a source that breaks its own ceiling",
				b, w, orderedRulesMaxJSONExpansion)
		}
		if w > widest {
			widest, widestByte = w, b
		}
	}
	if widest != orderedRulesMaxJSONExpansion {
		t.Fatalf("the widest byte encodes to %d, but the charged expansion is %d; a bound looser "+
			"than the worst case narrows the budget for nothing",
			widest, orderedRulesMaxJSONExpansion)
	}
	t.Logf("widest single byte is %#02x at %d encoded bytes", widestByte, widest)

	// A multi-byte rune must not beat it either: valid UTF-8 passes through at
	// its own width, and the line separators escape to six.
	for _, s := range []string{" ", " ", "\U0001F600", "é", "\xff\xfe"} {
		encoded, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshalling %q failed: %v", s, err)
		}
		if got, max := len(encoded)-2, chargedWidth(len(s)); int64(got) > max {
			t.Fatalf("%q is %d raw bytes charged as %d, but encodes to %d", s, len(s), max, got)
		}
	}
}

// TestOrderedRulesStructuralOverheadFitsItsReserve keeps the reserve honest.
//
// The reserve exists because JSON syntax is not charged to anyone: quotes,
// field names, braces and commas land on top of a budget the caller has already
// filled with text. It was sized from a measurement, and a measurement goes
// stale the moment a field is added — at which point an admitted stream would
// quietly encode past the ceiling again, which is exactly the defect the
// reserve was introduced to fix. So the measurement is re-taken here.
func TestOrderedRulesStructuralOverheadFitsItsReserve(t *testing.T) {
	scope := OrderedRulesScope{
		Namespace: "n", EpisodeID: "e", AccountContext: "a", AssociationEvidence: "v",
		SourceContractVersion: OrderedRulesStreamContractVersion,
		Coverage:              CoverageCompleteDeclared,
		IntervalToPosition:    1 << 40,
		HasInterval:           true,
	}
	admission := CommonAdmission{
		ManifestID: "m", ViewKind: ViewChannelCandidateStream, Population: "p", OrderBasis: "o",
	}
	// The widest shape the counts allow, carrying the least text they allow, so
	// what is left over IS the structure.
	text := 0
	count := func(s string) string { text += len(s); return s }
	cs := make([]OrderedRulesCandidate, 0, MaxOrderedRulesCandidates)
	for i := 0; i < MaxOrderedRulesCandidates; i++ {
		outs := make([]OrderedRulesOutcome, 0, MaxOrderedRulesOutcomes)
		for j := 0; j < MaxOrderedRulesOutcomes; j++ {
			outs = append(outs, OrderedRulesOutcome{
				// Unique within the candidate, as the model now requires. The
				// extra bytes are COUNTED, so they leave the structure figure
				// this case measures untouched.
				Identity: count("o" + itoaInternal(j)),
				Points: SuppliedInt64{
					Presence: SuppliedKnown, Value: int64(j + 1),
					Provenance: count("p"), HasAvailableAtPosition: true,
				},
			})
			text += len(SuppliedKnown)
		}
		cs = append(cs, OrderedRulesCandidate{
			Identity: count("c" + itoaInternal(i)), Position: int64(i + 1), HasPosition: true,
			SourceKind: SourceKindChannelUpdate, EpisodeMembership: MembershipProven,
			OutcomesPresence: SuppliedKnown, Outcomes: outs, Provenance: count("p"),
			Balance: SuppliedInt64{
				Presence: SuppliedKnown, Value: 1, Provenance: count("p"), HasAvailableAtPosition: true,
			},
		})
		text += len(SuppliedKnown)
	}

	stream, err := ProjectOrderedRulesStream(
		OrderedRulesSource{Scope: scope, Candidates: cs}, admission)
	if err != nil {
		t.Fatalf("the widest shape must project: %v", err)
	}
	encoded, err := json.Marshal(stream)
	if err != nil {
		t.Fatalf("marshalling the projected stream failed: %v", err)
	}

	structure := len(encoded) - text
	t.Logf("structure = %d bytes against a reserve of %d (%.1f%% used)",
		structure, orderedRulesStructuralReserveBytes,
		100*float64(structure)/float64(orderedRulesStructuralReserveBytes))
	if structure > orderedRulesStructuralReserveBytes {
		t.Fatalf("JSON structure is %d bytes at the widest shape, past the %d reserved for it. An "+
			"admitted stream can now encode past MaxOrderedRulesAggregateBytes: raise the reserve "+
			"or stop claiming the ceiling covers the encoded form.",
			structure, orderedRulesStructuralReserveBytes)
	}
}

func itoaInternal(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
