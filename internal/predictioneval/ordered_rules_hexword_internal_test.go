package predictioneval

// The one claim the wire contract rests on that only an internal test can make:
// the encoding is the package's EXISTING convention and not a second one.

import (
	"crypto/sha256"
	"hash"
	"math"
	"strconv"
	"testing"
)

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

// TestOrderedRulesInt64IsExactlyTheDigestRendering pins for the signed wire
// what the case above pins for the hexadecimal one: one rendering, two
// consumers, and no way for them to drift apart unnoticed.
//
// It deliberately does NOT compare MarshalText against strconv.FormatInt.
// Both ARE strconv.FormatInt, so such a check would restate the implementation
// twice and could not fail. Instead it runs the two real code paths — the
// digest's own digestInt, and digestPart over whatever the wire produced — and
// requires the resulting hashes to agree. That fails the moment either side
// changes its format, which is the only thing worth catching here.
func TestOrderedRulesInt64IsExactlyTheDigestRendering(t *testing.T) {
	sweep := int64DigestSweep()
	if len(sweep) < 64 {
		t.Fatalf("the sweep is %d values, too few to cover the bit positions", len(sweep))
	}
	for _, v := range sweep {
		encoded, err := OrderedRulesInt64(v).MarshalText()
		if err != nil {
			t.Fatalf("encoding %d failed: %v", v, err)
		}

		viaDigest := sha256.New()
		digestInt(viaDigest, v)
		viaWire := sha256.New()
		digestPart(viaWire, string(encoded))

		if got, want := hexOfSum(viaWire), hexOfSum(viaDigest); got != want {
			t.Fatalf("%d hashes to %s through the wire form %q and to %s through digestInt; "+
				"the two renderings have diverged", v, got, encoded, want)
		}

		// And the decoder inverts the digest's own rendering, so the round trip
		// is anchored to that convention rather than to the encoder beside it.
		decoded := OrderedRulesInt64(0x5a5a5a5a5a5a5a5)
		if err := decoded.UnmarshalText([]byte(strconv.FormatInt(v, 10))); err != nil {
			t.Fatalf("decoding the digest rendering of %d failed: %v", v, err)
		}
		if int64(decoded) != v {
			t.Fatalf("the digest rendering of %d decodes back to %d", v, int64(decoded))
		}
	}
}

func hexOfSum(h hash.Hash) string {
	sum := h.Sum(nil)
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(sum)*2)
	for _, b := range sum {
		out = append(out, digits[b>>4], digits[b&0x0f])
	}
	return string(out)
}

// int64DigestSweep is every power of two and its neighbours in both signs, plus
// the extremes — deterministic, so a failure reproduces exactly.
func int64DigestSweep() []int64 {
	vs := []int64{0, 1, -1, math.MinInt64, math.MaxInt64, 1<<53 - 1, 1 << 53, 1<<53 + 1,
		-(1 << 53), -(1 << 53) - 1}
	for b := 0; b < 63; b++ {
		p := int64(1) << b
		vs = append(vs, p, p-1, p+1, -p, -p+1, -p-1)
	}
	return vs
}
