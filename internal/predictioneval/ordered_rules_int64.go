package predictioneval

// THE ORDERED-RULES CORE: lossless transport for signed quantities.
//
// This is the SECOND of two wire rules in this package, and the split is
// deliberate rather than incidental. [OrderedRulesHex64] carries values that
// are exact BIT PATTERNS — a raw entropy word, or math.Float64bits of a share —
// and renders them as fixed-width hexadecimal, because that is the form the
// digests have always used and a bit pattern has no sign. The twelve fields
// this type carries are QUANTITIES and POSITIONS: they are signed, they are
// compared and summed as numbers, and a reader who sees one should see the
// number rather than its encoding. So they travel as canonical base-ten
// strings.
//
// One rule, one owner. Twelve hand-written serializers would be twelve places
// to keep in step, and the history of this file's sibling is a sequence of
// corrections to claims that drifted out of step with each other.
//
// WHY A STRING AT ALL, when these are ordinary integers: a JSON number is read
// by an IEEE-754 consumer as a float64, which cannot hold every int64. That is
// not hypothetical here. Measured on this package before this type existed:
// supplied points of 2^53+1 relayed through such a consumer arrive as 2^53, the
// exact int64 pool sum shifts with them, and the recorded share moves from
// 0x3feffffffffffffe to 0x3ff0000000000000 — which is exactly 1.0, reporting an
// outcome as holding the whole pool when it holds all of it but one point.
// Same status, same chosen outcome, silently different evidence.
//
// The alternative was to refuse supplied values outside the IEEE-754 safe
// range. That was rejected on purpose: the model's int64 domain is not narrowed
// to fit a consumer's limitation.

import (
	"errors"
	"strconv"
)

// ErrOrderedRulesInt64Malformed is returned for text that is not a canonical
// base-ten integer.
//
// Canonical means EXACTLY what [strconv.FormatInt] base 10 writes: an optional
// leading minus, then digits, with no leading zeros unless the value is zero
// itself. No plus sign, no padding, no "-0", no underscores, no spaces, no
// other base, no exponent, no fractional part.
//
// One value, one encoding, and every other spelling is refused rather than
// normalized — a decoder that repairs its input cannot tell a caller that the
// artifact it read was the artifact that was written.
var ErrOrderedRulesInt64Malformed = errors.New(
	"predictioneval: ordered-rules integer is not a canonical base-ten integer")

// OrderedRulesInt64 is a signed 64-bit quantity that survives JSON intact.
//
// It is an int64 in memory and a canonical base-ten STRING on the wire.
// Arithmetic converts explicitly, which is the point: the conversion is exact
// in both directions and never a numeric change, so nothing this model computes
// depends on the transport. Negative values are carried, because the fields
// that use this type have an int64 contract and this repair does not narrow it.
//
// The encoding is deliberately NOT negotiable. There is no JSON-number
// compatibility shim and no float64 fallback, because either one would preserve
// exactly the lossy representation this type exists to remove — a caller that
// silently kept working would be the caller whose value was already wrong.
type OrderedRulesInt64 int64

// MarshalText writes the canonical base-ten form.
func (v OrderedRulesInt64) MarshalText() ([]byte, error) {
	return strconv.AppendInt(nil, int64(v), 10), nil
}

// UnmarshalText is MarshalText's exact inverse, and refuses everything else.
//
// STRICTNESS IS THE FEATURE. Canonicality is not checked by a hand-written list
// of rejected spellings — it is checked by re-encoding what was parsed and
// requiring the result to be the input, byte for byte. That makes the decoder
// the encoder's inverse BY CONSTRUCTION rather than by a second set of rules
// somebody has to keep in step, which is the mistake this package has already
// paid for elsewhere.
//
// A JSON number never reaches here at all — encoding/json refuses to hand a
// number to a TextUnmarshaler — which is what closes the numeric form off at
// the type level rather than by convention.
//
// ONE EQUIVALENT MUTANT, recorded rather than left to be rediscovered.
// Replacing the base 10 above with base 0 — which would admit "0x10", "1_000"
// and leading-zero octal — changes NO observable behaviour, and a mutation run
// confirmed it kills nothing. The reason is the round trip: base 0 accepts a
// strict superset of base 10, and every spelling in that difference carries a
// prefix, an underscore or a leading zero, none of which FormatInt ever writes,
// so all of them fail canonicality instead. The base argument is therefore not
// what makes this decoder strict, and a reader should not believe it is.
func (v *OrderedRulesInt64) UnmarshalText(text []byte) error {
	parsed, err := strconv.ParseInt(string(text), 10, 64)
	if err != nil {
		return errors.Join(ErrOrderedRulesInt64Malformed, err)
	}
	// The round trip is the canonicality rule. "+1", "01", "-0" and every other
	// alternate spelling of a representable value parse successfully and then
	// fail here, which is where they are meant to fail.
	if canonical := strconv.FormatInt(parsed, 10); canonical != string(text) {
		return errors.Join(ErrOrderedRulesInt64Malformed,
			errors.New("predictioneval: integer "+strconv.Quote(string(text))+
				" is not canonical; the canonical spelling of that value is "+
				strconv.Quote(canonical)))
	}
	*v = OrderedRulesInt64(parsed)
	return nil
}
