package predictioneval

// The one claim the wire contract rests on that only an internal test can make:
// the encoding is the package's EXISTING convention and not a second one.

import "testing"

// TestOrderedRulesHexWordIsExactlyTheDigestRendering pins the identity the
// owner decision required — the same fixed-width hex the digests have always
// used, reused rather than restated.
//
// MarshalText calls hex64, so this cannot fail today; that is the point. It
// fails the moment someone gives the wire its own formatter, which is the
// change that would let the two drift and make a digest and the artifact it
// witnesses disagree about what a word looks like.
func TestOrderedRulesHexWordIsExactlyTheDigestRendering(t *testing.T) {
	if got := len(hex64(0)); got != OrderedRulesHexWordDigits {
		t.Fatalf("hex64 renders %d characters but the wire declares %d",
			got, OrderedRulesHexWordDigits)
	}
	for _, v := range hexWordSweep() {
		encoded, err := OrderedRulesHex64(v).MarshalText()
		if err != nil {
			t.Fatalf("encoding %#016x failed: %v", v, err)
		}
		if string(encoded) != hex64(v) {
			t.Fatalf("%#016x encodes to %q on the wire and %q in a digest",
				v, encoded, hex64(v))
		}
	}
}

// TestOrderedRulesHexWordDecodeInvertsHex64 checks the inverse over a sweep
// wide enough that a per-nibble fault cannot hide in it.
//
// Every single-bit value is included, so a decoder that mishandled any one of
// the sixty-four bit positions — a shift of the wrong width, a nibble table off
// by one, an accumulator that is not sixty-four bits — fails on that bit rather
// than on whichever value a hand-picked table happened to contain.
func TestOrderedRulesHexWordDecodeInvertsHex64(t *testing.T) {
	sweep := hexWordSweep()
	if len(sweep) < 64 {
		t.Fatalf("the sweep is %d values, too few to cover the bit positions", len(sweep))
	}
	for _, v := range sweep {
		got := OrderedRulesHex64(0xa5a5a5a5a5a5a5a5)
		if err := got.UnmarshalText([]byte(hex64(v))); err != nil {
			t.Fatalf("decoding hex64(%#016x) = %q failed: %v", v, hex64(v), err)
		}
		if uint64(got) != v {
			t.Fatalf("hex64(%#016x) decodes back to %#016x", v, uint64(got))
		}
	}
}

// hexWordSweep is every single-bit value, every nibble-aligned run, and a
// deterministic spread — no randomness, because a test that samples a different
// space on each run reports a different thing on each run.
func hexWordSweep() []uint64 {
	vs := []uint64{0, 1<<53 - 1, 1 << 53, 1<<53 + 1, ^uint64(0)}
	for b := 0; b < 64; b++ {
		vs = append(vs, uint64(1)<<b, ^(uint64(1) << b))
	}
	for n := 0; n < 16; n++ {
		// A single nibble held at each of the sixteen values, at each of the
		// sixteen positions, so a digit table that is wrong for exactly one
		// digit is caught wherever that digit lands.
		for pos := 0; pos < 16; pos++ {
			vs = append(vs, uint64(n)<<(4*pos))
		}
	}
	// A deterministic spread: a fixed multiplier walked across the space.
	v := uint64(0x9e3779b97f4a7c15)
	for i := 0; i < 64; i++ {
		v = v*6364136223846793005 + 1442695040888963407
		vs = append(vs, v)
	}
	return vs
}
