package predictioneval

// THE ORDERED-RULES CORE: lossless 64-bit transport, for four fields.
//
// FOUR exported values this model carries across a wire are exact bit patterns
// rather than quantities — a raw entropy word, or math.Float64bits of a pool
// share. JSON numbers cannot carry one. An IEEE-754 consumer rounds anything
// past 2^53, so two words the strict Bernoulli comparison distinguishes can
// arrive equal, and two shares one ulp apart collapse into one.
//
// WHAT THIS TYPE DOES NOT COVER, said here because an earlier draft of this
// comment claimed "every exported 64-bit value" and a reviewer was right to
// call that false. The model also carries ten exported int64 JSON fields that
// are QUANTITIES — supplied point values, availability positions, the declared
// interval endpoints, candidate and intervention positions, and the positions
// re-exported on a result. They still travel as JSON numbers and they are not
// repaired here: the owner decision that authorized this change enumerated the
// four bit-pattern fields, and widening a public wire contract past what was
// authorized is not a thing a comment gets to do. The cost is measured rather
// than guessed, in
// TestOrderedRulesInt64JSONFieldsStillCollapseAndAreOutsideThisRepair.
//
// That was not hypothetical. Reproduced before this type existed:
// 9223372036854788153 and 9223372036854788154 both became 9223372036854788096
// through a float64 parser, and math.Float64bits(0.5) against the next
// representable double — 4602678819172646912 and 4602678819172646913 — became
// the same integer. The share field is the sharper case: a bit pattern of any
// normal double is past 2^53, even for a value as small as 1e-300, so EVERY
// value of it was at risk rather than only the large ones.
//
// The package already said so and already acted on it in one place: hex64's own
// documentation warns that "a raw word pushed through a double-precision JSON
// number loses its low bits silently", and the digests have used fixed-width hex
// since they were written. The wire form contradicted that. This type removes
// the contradiction rather than adding a rule — it is hex64 on the way out, and
// hex64's exact inverse on the way in.

import (
	"errors"
	"strconv"
)

// ErrOrderedRulesHexWordMalformed is returned for text that is not a canonical
// fixed-width hex word.
//
// Canonical means exactly [OrderedRulesHexWordDigits] characters drawn from
// 0-9 and lower-case a-f: no shorter form for a small value, no "0x" prefix, no
// upper case, no padding. One value, one encoding, and every other spelling is
// refused rather than normalized — a decoder that repairs its input cannot tell
// a caller that the artifact it read was the artifact that was written.
var ErrOrderedRulesHexWordMalformed = errors.New(
	"predictioneval: ordered-rules hex word is not canonical fixed-width hexadecimal")

// OrderedRulesHexWordDigits is the exact width of an encoded word: 64 bits at
// four bits per character.
const OrderedRulesHexWordDigits = 16

// OrderedRulesHex64 is a 64-bit pattern that survives JSON intact.
//
// It is a uint64 in memory and a canonical fixed-width hex STRING on the wire.
// Arithmetic converts explicitly, which is the point: the conversion is a
// reinterpretation of the same bits and never a numeric change, so nothing this
// model computes depends on the transport.
//
// The encoding is deliberately NOT negotiable. There is no decimal fallback and
// no lenient decode, because a shim that accepts JSON numbers would preserve
// exactly the lossy representation this type exists to remove — a caller that
// silently kept working would be the caller whose low bits were already gone.
type OrderedRulesHex64 uint64

// MarshalText renders the word through the package's existing convention.
//
// It calls hex64 rather than restating the format: one definition, and a change
// to the digests' rendering cannot silently diverge from the wire's.
func (v OrderedRulesHex64) MarshalText() ([]byte, error) {
	return []byte(hex64(uint64(v))), nil
}

// UnmarshalText is hex64's exact inverse, and refuses everything else.
//
// STRICTNESS IS THE FEATURE, so the two failure modes are separate and both
// say what they saw. A JSON number never reaches here at all — encoding/json
// refuses to hand a number to a TextUnmarshaler — which is what closes the
// decimal form off at the type level rather than by convention.
func (v *OrderedRulesHex64) UnmarshalText(text []byte) error {
	if len(text) != OrderedRulesHexWordDigits {
		return errors.Join(ErrOrderedRulesHexWordMalformed,
			errors.New("predictioneval: hex word is "+strconv.Itoa(len(text))+
				" characters, not "+strconv.Itoa(OrderedRulesHexWordDigits)))
	}
	var out uint64
	for _, c := range text {
		var nibble uint64
		switch {
		case c >= '0' && c <= '9':
			nibble = uint64(c - '0')
		case c >= 'a' && c <= 'f':
			nibble = uint64(c-'a') + 10
		default:
			// Upper case lands here with everything else, and that is
			// deliberate: "3FE0" and "3fe0" are the same number and two
			// different artifacts, and only one of them is what hex64 writes.
			return errors.Join(ErrOrderedRulesHexWordMalformed,
				errors.New("predictioneval: hex word contains "+strconv.Quote(string(c))+
					", which is not a lower-case hexadecimal digit"))
		}
		out = out<<4 | nibble
	}
	*v = OrderedRulesHex64(out)
	return nil
}
