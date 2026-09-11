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
