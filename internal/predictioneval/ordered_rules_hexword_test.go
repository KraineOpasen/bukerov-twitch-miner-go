package predictioneval_test

// THE 64-BIT WIRE CONTRACT, under falsification.
//
// Four exported fields carry an exact 64-bit pattern across a wire:
// SuppliedDrawTrace.Words, OrderedRulesSelection.ShareBits,
// OrderedRulesTraceEntry.ShareBits and OrderedRulesTraceEntry.RawWordValue.
// They travelled as JSON numbers until this file's subject was introduced, and
// a JSON number cannot carry one.
//
// The cases below are arranged so that the claim is PROVEN and not asserted:
// TestOrderedRulesNumericJSONWouldLoseTheDistinctionTheHexFormKeeps runs the
// old representation and the new one through the same IEEE-754 consumer and
// shows the first collapsing two distinguishable values into one while the
// second keeps them apart. Everything else pins the repaired contract itself —
// every boundary the audit named, every spelling that is refused, and the four
// digests, which must NOT have moved, because the transport changed and the
// hashed values did not.

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// hexWordVectors is one table for every case in this file, so a value that is
// exercised in one direction is exercised in all of them.
//
// The membership is the audit's own list: zero, values whose canonical form is
// mostly leading zeros, both sides of 2^53, the maximum, and two ShareBits one
// ulp apart — plus 1e-300's bit pattern, which is the sharpest member. It is a
// tiny double and a 58-bit integer, so it refutes "only large shares are at
// risk" without needing a large share.
var hexWordVectors = []struct {
	name string
	// value is the exact 64-bit pattern; text is its one canonical spelling.
	value uint64
	text  string
}{
	{"zero", 0, "0000000000000000"},
	{"one, fifteen leading zeros", 1, "0000000000000001"},
	{"a byte, fourteen leading zeros", 0xff, "00000000000000ff"},
	{"every nibble distinct", 0x0123456789abcdef, "0123456789abcdef"},
	{"2^53 minus one, the last exact double", 1<<53 - 1, "001fffffffffffff"},
	{"2^53, where doubles start skipping", 1 << 53, "0020000000000000"},
	{"2^53 plus one, the first integer no double holds", 1<<53 + 1, "0020000000000001"},
	{"2^63, the half-rate threshold", 1 << 63, "8000000000000000"},
	{"MaxUint64", math.MaxUint64, "ffffffffffffffff"},
	{"Float64bits(0.5)", 0x3fe0000000000000, "3fe0000000000000"},
	{"Float64bits(0.5) plus one ulp", 0x3fe0000000000001, "3fe0000000000001"},
	{"Float64bits(1e-300), a tiny double past 2^53", 0x01a56e1fc2f8f359, "01a56e1fc2f8f359"},
}

// TestOrderedRulesHexWordEncodesEveryBoundaryExactly pins both directions.
//
// Encoding and decoding are checked against the SAME table rather than against
// each other, because a round trip alone cannot distinguish a correct codec
// from two mistakes that cancel: a marshaller that dropped the high nibble and
// an unmarshaller that invented it back would pass a round-trip test and lose
// every value above 2^60 on a real wire.
func TestOrderedRulesHexWordEncodesEveryBoundaryExactly(t *testing.T) {
	for _, v := range hexWordVectors {
		t.Run(v.name, func(t *testing.T) {
			if len(v.text) != predictioneval.OrderedRulesHexWordDigits {
				t.Fatalf("the vector's own text is %d characters, not the declared %d",
					len(v.text), predictioneval.OrderedRulesHexWordDigits)
			}
			encoded, err := predictioneval.OrderedRulesHex64(v.value).MarshalText()
			if err != nil {
				t.Fatalf("encoding %#016x failed: %v", v.value, err)
			}
			if string(encoded) != v.text {
				t.Fatalf("%#016x encodes to %q, want %q", v.value, encoded, v.text)
			}
			// Set to something else first, so a decoder that never assigns is
			// not read as a decoder that assigned the right value.
			decoded := predictioneval.OrderedRulesHex64(math.MaxUint64 - 7)
			if err := decoded.UnmarshalText([]byte(v.text)); err != nil {
				t.Fatalf("decoding %q failed: %v", v.text, err)
			}
			if uint64(decoded) != v.value {
				t.Fatalf("%q decodes to %#016x, want %#016x", v.text, uint64(decoded), v.value)
			}
		})
	}
}

// TestOrderedRulesHexWordRefusesEveryNonCanonicalSpelling is the strictness the
// owner decision specified: one value, one encoding, and no repair of input.
//
// A decoder that normalizes cannot tell a caller that the artifact it read was
// the artifact that was written, which is the whole business of a package whose
// results are evidence. The decimal spellings are in the table for a second
// reason: they show that refusing the JSON-number form is not the only thing
// standing between this contract and a decimal one.
func TestOrderedRulesHexWordRefusesEveryNonCanonicalSpelling(t *testing.T) {
	for _, c := range []struct{ name, text string }{
		{"empty", ""},
		{"one digit", "f"},
		{"fifteen digits, one short", "000000000000001"},
		{"seventeen digits, one long", "00000000000000010"},
		{"upper case, right length", "0000000000000ABC"},
		{"mixed case, right length", "0123456789AbCdEf"},
		{"0x prefix, right length", "0x000000000000ff"},
		{"a non-hex letter", "00000000000000fg"},
		{"trailing space", "00000000000000f "},
		{"leading space", " 00000000000000f"},
		{"a sign", "+000000000000001"},
		{"an interior NUL", "00000000\x0000000f"},
		{"an invalid UTF-8 byte", "\xff000000000000000"},
		{"a decimal spelling of 2^63", "9223372036854775808"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got predictioneval.OrderedRulesHex64
			err := got.UnmarshalText([]byte(c.text))
			if err == nil {
				t.Fatalf("%q was accepted as %#016x; it is not what the encoder writes",
					c.text, uint64(got))
			}
			if !errors.Is(err, predictioneval.ErrOrderedRulesHexWordMalformed) {
				t.Fatalf("%q was refused with %v, which does not wrap "+
					"ErrOrderedRulesHexWordMalformed; a caller cannot classify it", c.text, err)
			}
		})
	}
}

// TestOrderedRulesHexWordReadsAnAllDigitWordAsHexAndNotAsDecimal records the
// one thing the width check above does NOT catch, because nothing could.
//
// Every decimal digit is also a hex digit, so a decimal spelling that happens
// to be exactly sixteen characters long is a well-formed hex word and is
// accepted — as its HEX value. This is not a hole in the strictness: the
// contract names one encoding, and a producer writing decimal into a field
// declared hexadecimal is writing a different number, not the same number
// differently. It is pinned so the limit of the length check is stated where
// someone reading the refusal table would otherwise infer it covers decimals.
//
// The refusal table's own decimal case is nineteen characters, so length kills
// it; that is the general outcome, and this is the exception.
func TestOrderedRulesHexWordReadsAnAllDigitWordAsHexAndNotAsDecimal(t *testing.T) {
	var got predictioneval.OrderedRulesHex64
	if err := got.UnmarshalText([]byte("0009223372036854")); err != nil {
		t.Fatalf("a sixteen-digit all-decimal word was refused: %v. Every one of those digits "+
			"is a hex digit, so refusing it would mean the decoder is not hex64's inverse", err)
	}
	if uint64(got) != 0x0009223372036854 {
		t.Fatalf("decoded %#016x, want the hexadecimal reading %#016x", uint64(got), 0x0009223372036854)
	}
	if uint64(got) == 9223372036854 {
		t.Fatal("the decoder read the decimal value, so the encoding is not the one documented")
	}
	// And the value it produces re-encodes to exactly what was read, which is
	// what makes the reading canonical rather than merely accepted.
	if out, err := got.MarshalText(); err != nil || string(out) != "0009223372036854" {
		t.Fatalf("re-encoding produced %q (%v), want the text it was decoded from", out, err)
	}
}

// TestOrderedRulesHexWordRefusalLeavesTheDestinationUntouched pins that a
// refusal is not a partial write.
//
// A decoder that assigned the digits it had read before hitting the bad one
// would hand a caller a value that is neither the old one nor any value that
// was ever on the wire, and the error is easy to ignore on a slice element.
func TestOrderedRulesHexWordRefusalLeavesTheDestinationUntouched(t *testing.T) {
	for _, text := range []string{"000000000000000g", "g000000000000000", "abc", "0123456789abcdeF"} {
		before := predictioneval.OrderedRulesHex64(0xdeadbeefdeadbeef)
		got := before
		if err := got.UnmarshalText([]byte(text)); err == nil {
			t.Fatalf("%q was accepted", text)
		}
		if got != before {
			t.Fatalf("refusing %q changed the destination from %#016x to %#016x",
				text, uint64(before), uint64(got))
		}
	}
}

// TestOrderedRulesHexWordRefusesAJSONNumberAtTheTypeLevel is why the owner
// decision needed no decimal compatibility shim.
//
// encoding/json will not hand a JSON number to a TextUnmarshaler at all, so the
// old representation is closed off by the type rather than by a check this
// package would have to keep writing. The refusal is json's own and is NOT
// wrapped in ErrOrderedRulesHexWordMalformed — asserted here rather than
// glossed, because a caller classifying malformed input must know that the
// number case arrives as a *json.UnmarshalTypeError.
func TestOrderedRulesHexWordRefusesAJSONNumberAtTheTypeLevel(t *testing.T) {
	for _, in := range []string{
		`{"shareBits":4602678819172646912}`,
		`{"shareBits":0}`,
		`{"shareBits":1.5}`,
		`{"shareBits":true}`,
		`{"shareBits":{}}`,
		`{"words":[0]}`,
		`{"words":[9007199254740993]}`,
	} {
		var into struct {
			ShareBits predictioneval.OrderedRulesHex64   `json:"shareBits"`
			Words     []predictioneval.OrderedRulesHex64 `json:"words"`
		}
		err := json.Unmarshal([]byte(in), &into)
		if err == nil {
			t.Fatalf("%s decoded without error as %#016x %v; the numeric form is the "+
				"representation this contract exists to remove", in, uint64(into.ShareBits), into.Words)
		}
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			t.Fatalf("%s was refused with %v, want a *json.UnmarshalTypeError", in, err)
		}
	}
}

// TestOrderedRulesHexWordNullIsANoOpAndNotAZero records a property this repair
// did NOT change and does not claim to.
//
// encoding/json treats a JSON null as "leave the destination alone" for every
// type, so `{"shareBits":null}` into a fresh struct yields a share of exactly
// zero with no error — the same thing it yielded when the field was a plain
// uint64. It is pinned rather than left implicit because it is the one way a
// missing 64-bit value still becomes a real one, and a reader of this file
// would otherwise reasonably assume the strictness above covers it. Repairing
// it means a presence flag or a pointer, which is a wire change outside the
// authorized scope of this one.
func TestOrderedRulesHexWordNullIsANoOpAndNotAZero(t *testing.T) {
	var fresh struct {
		ShareBits predictioneval.OrderedRulesHex64   `json:"shareBits"`
		Words     []predictioneval.OrderedRulesHex64 `json:"words"`
	}
	if err := json.Unmarshal([]byte(`{"shareBits":null,"words":null}`), &fresh); err != nil {
		t.Fatalf("a null was refused: %v. That is a stricter contract than this repair "+
			"introduced, so the doc above is now wrong rather than the code", err)
	}
	if fresh.ShareBits != 0 || fresh.Words != nil {
		t.Fatalf("a null wrote %#016x / %v", uint64(fresh.ShareBits), fresh.Words)
	}
	// And the other half of the same behaviour: null does not CLEAR a value
	// that is already there, which is what makes it a no-op rather than a zero.
	kept := struct {
		ShareBits predictioneval.OrderedRulesHex64 `json:"shareBits"`
	}{ShareBits: 0x3fe0000000000000}
	if err := json.Unmarshal([]byte(`{"shareBits":null}`), &kept); err != nil {
		t.Fatalf("a null over an existing value was refused: %v", err)
	}
	if uint64(kept.ShareBits) != 0x3fe0000000000000 {
		t.Fatalf("a null overwrote an existing value with %#016x", uint64(kept.ShareBits))
	}
}

// TestOrderedRulesAllFourAffectedFieldsRoundTripBitExact walks the four fields
// in their real carriers rather than in a stand-in.
//
// The type is exercised elsewhere; what this case pins is that each of the four
// declarations actually USES it. Reverting any one of them to a plain uint64
// stops this file compiling, which is a real guard but a weak one — it is a
// statement about Go and not about an artifact — so every subtest also asserts
// on the encoded TEXT, and TestOrderedRulesEvaluationCarriesHexOnTheWire makes
// the same assertion without naming a Go type at all.
func TestOrderedRulesAllFourAffectedFieldsRoundTripBitExact(t *testing.T) {
	const (
		lo   = uint64(1<<53 + 1)
		hi   = math.MaxUint64
		half = uint64(0x3fe0000000000000)
		ulp  = uint64(0x3fe0000000000001)
	)

	t.Run("SuppliedDrawTrace.Words", func(t *testing.T) {
		in := predictioneval.SuppliedDrawTrace{
			RunID:                   "wire-1",
			EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion,
			Words: []predictioneval.OrderedRulesHex64{
				0, 1, predictioneval.OrderedRulesHex64(lo), predictioneval.OrderedRulesHex64(hi),
			},
		}
		encoded := mustMarshal(t, in)
		const want = `"words":["0000000000000000","0000000000000001","0020000000000001","ffffffffffffffff"]`
		if !strings.Contains(encoded, want) {
			t.Fatalf("the encoded trace is %s, want it to carry %s", encoded, want)
		}
		var out predictioneval.SuppliedDrawTrace
		mustUnmarshal(t, encoded, &out)
		if len(out.Words) != len(in.Words) {
			t.Fatalf("decoded %d words, want %d", len(out.Words), len(in.Words))
		}
		for i := range in.Words {
			if out.Words[i] != in.Words[i] {
				t.Fatalf("word %d round-tripped %#016x to %#016x",
					i, uint64(in.Words[i]), uint64(out.Words[i]))
			}
		}
	})

	t.Run("OrderedRulesSelection.ShareBits", func(t *testing.T) {
		in := predictioneval.OrderedRulesSelection{
			CandidateIdentity: "c1", OutcomeIdentity: "A",
			Basis: predictioneval.SelectionDefault, RuleIndex: -1,
			ShareBits: predictioneval.OrderedRulesHex64(half),
		}
		encoded := mustMarshal(t, in)
		if !strings.Contains(encoded, `"shareBits":"3fe0000000000000"`) {
			t.Fatalf("the encoded selection is %s, want a quoted hex shareBits", encoded)
		}
		var out predictioneval.OrderedRulesSelection
		mustUnmarshal(t, encoded, &out)
		if out.ShareBits != in.ShareBits {
			t.Fatalf("shareBits round-tripped %#016x to %#016x",
				uint64(in.ShareBits), uint64(out.ShareBits))
		}
		// The adjacent value must not land on the same text, which is the
		// distinction the field was added to preserve.
		next := in
		next.ShareBits = predictioneval.OrderedRulesHex64(ulp)
		if other := mustMarshal(t, next); other == encoded {
			t.Fatalf("two shares one ulp apart both encode to %s", encoded)
		}
	})

	t.Run("OrderedRulesTraceEntry.ShareBits and .RawWordValue", func(t *testing.T) {
		in := predictioneval.OrderedRulesTraceEntry{
			Step: predictioneval.TraceStepRuleDraw, RuleIndex: 0, RawWordIndex: 0,
			ComparatorMatched: true, BernoulliEvaluated: true,
			ShareBits:    predictioneval.OrderedRulesHex64(ulp),
			RawWordValue: predictioneval.OrderedRulesHex64(hi),
		}
		encoded := mustMarshal(t, in)
		for _, want := range []string{
			`"shareBits":"3fe0000000000001"`, `"rawWordValue":"ffffffffffffffff"`,
		} {
			if !strings.Contains(encoded, want) {
				t.Fatalf("the encoded entry is %s, want it to carry %s", encoded, want)
			}
		}
		var out predictioneval.OrderedRulesTraceEntry
		mustUnmarshal(t, encoded, &out)
		if out.ShareBits != in.ShareBits || out.RawWordValue != in.RawWordValue {
			t.Fatalf("the entry round-tripped (%#016x, %#016x) to (%#016x, %#016x)",
				uint64(in.ShareBits), uint64(in.RawWordValue),
				uint64(out.ShareBits), uint64(out.RawWordValue))
		}
	})
}

// TestOrderedRulesNumericJSONWouldLoseTheDistinctionTheHexFormKeeps is the
// proof the owner decision asked for, and it is a MEASUREMENT rather than an
// argument about IEEE-754.
//
// Both representations are built here and both are read back through the same
// consumer: json.Unmarshal into an interface, which is what every JSON reader
// that has no int64 does — JavaScript, jq without a flag, Python's float path,
// and Go's own default. The numeric form is shown to destroy the distinction
// that the exported contract's own documentation promises to preserve; the
// repaired form is shown to keep it through the identical pipeline.
func TestOrderedRulesNumericJSONWouldLoseTheDistinctionTheHexFormKeeps(t *testing.T) {
	// legacyNumeric is the declaration as it stood before this repair: the same
	// field names and tags, on plain uint64. It exists only here.
	type legacyNumeric struct {
		Words     []uint64 `json:"words"`
		ShareBits uint64   `json:"shareBits"`
	}
	repaired := struct {
		Words     []predictioneval.OrderedRulesHex64 `json:"words"`
		ShareBits predictioneval.OrderedRulesHex64   `json:"shareBits"`
	}{}

	for _, c := range []struct {
		name string
		a, b uint64
	}{
		{"two shares one ulp apart", 0x3fe0000000000000, 0x3fe0000000000001},
		{"2^53 and the integer above it", 1 << 53, 1<<53 + 1},
		{"MaxUint64 and the word below it", math.MaxUint64, math.MaxUint64 - 1},
		{"two raw words the strict comparison separates", 9223372036854788153, 9223372036854788154},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.a == c.b {
				t.Fatal("the vector is two copies of one value, so nothing below can fail")
			}

			// The OLD form. Its JSON text distinguishes the two values...
			oldA := mustMarshal(t, legacyNumeric{Words: []uint64{c.a}, ShareBits: c.a})
			oldB := mustMarshal(t, legacyNumeric{Words: []uint64{c.b}, ShareBits: c.b})
			if oldA == oldB {
				t.Fatalf("the two numeric encodings are already identical (%s), so this case "+
					"would prove nothing about the READER", oldA)
			}
			// ...and an IEEE-754 reader cannot tell them apart.
			gotA, gotB := decodeThroughFloat64(t, oldA), decodeThroughFloat64(t, oldB)
			if gotA != gotB {
				t.Fatalf("the numeric form survived this consumer: %v against %v. The four "+
					"fields were changed on the claim that it does not, so either the claim "+
					"or this vector is wrong", gotA, gotB)
			}
			t.Logf("numeric: %#016x and %#016x both arrive as %v", c.a, c.b, gotA)

			// The REPAIRED form, through the identical consumer.
			repaired.Words = []predictioneval.OrderedRulesHex64{predictioneval.OrderedRulesHex64(c.a)}
			repaired.ShareBits = predictioneval.OrderedRulesHex64(c.a)
			newA := mustMarshal(t, repaired)
			repaired.Words = []predictioneval.OrderedRulesHex64{predictioneval.OrderedRulesHex64(c.b)}
			repaired.ShareBits = predictioneval.OrderedRulesHex64(c.b)
			newB := mustMarshal(t, repaired)
			keptA, keptB := decodeThroughFloat64(t, newA), decodeThroughFloat64(t, newB)
			if keptA == keptB {
				t.Fatalf("the repaired form collapsed too: %v equals %v", keptA, keptB)
			}

			// And the strict decoder recovers the exact bits, which the float
			// comparison above deliberately does not check.
			var back struct {
				Words     []predictioneval.OrderedRulesHex64 `json:"words"`
				ShareBits predictioneval.OrderedRulesHex64   `json:"shareBits"`
			}
			mustUnmarshal(t, newB, &back)
			if uint64(back.ShareBits) != c.b || len(back.Words) != 1 || uint64(back.Words[0]) != c.b {
				t.Fatalf("the repaired form decoded to %#016x / %v, want %#016x",
					uint64(back.ShareBits), back.Words, c.b)
			}
		})
	}
}

// TestOrderedRulesEvaluationCarriesHexOnTheWire closes the gap between the four
// declarations and a real result.
//
// A field can be declared correctly and still be assigned from a path that
// rounds, so this evaluates the mechanism and inspects what the EVALUATION
// encodes — every 64-bit value in it, taken from the model rather than from a
// literal.
func TestOrderedRulesEvaluationCarriesHexOnTheWire(t *testing.T) {
	ev := orWireFixture(t)
	if ev.Selected == nil || len(ev.Trace) == 0 {
		t.Fatalf("the fixture must admit and trace for this case to inspect anything: "+
			"status %q reason %q", ev.Status, ev.Reason)
	}
	encoded := mustMarshal(t, ev)
	for _, want := range []string{
		`"shareBits":"3fd999999999999a"`, `"rawWordValue":"0020000000000001"`,
	} {
		if !strings.Contains(encoded, want) {
			t.Fatalf("the encoded evaluation does not carry %s: %s", want, encoded)
		}
	}
	// No bare 64-bit decimal may appear where these fields are. The selection's
	// share as a number would be this, and finding it means a field was missed.
	if strings.Contains(encoded, "4600877379321698714") {
		t.Fatalf("the evaluation still encodes a share as a decimal number: %s", encoded)
	}
	// The assertions above name Go types. These do not: they read the artifact
	// as any other consumer would, so they survive a field being redeclared and
	// they cover a 64-bit field nobody has added yet.
	var generic any
	mustUnmarshal(t, encoded, &generic)
	for _, key := range []string{"shareBits", "rawWordValue", "words"} {
		found := 0
		walkJSON(generic, func(path string, node any) {
			if !strings.HasSuffix(path, "."+key) && !strings.HasSuffix(path, "."+key+"[]") {
				return
			}
			found++
			text, ok := node.(string)
			if !ok {
				t.Fatalf("%s is %#v, a %T; a 64-bit pattern on this wire is a string", path, node, node)
			}
			if len(text) != predictioneval.OrderedRulesHexWordDigits {
				t.Fatalf("%s is %q, which is %d characters rather than %d",
					path, text, len(text), predictioneval.OrderedRulesHexWordDigits)
			}
		})
		if key != "words" && found == 0 {
			t.Fatalf("the encoded evaluation carries no %q at all, so nothing above was checked", key)
		}
	}
	// And no number anywhere in THIS artifact is one an IEEE-754 reader would
	// round. That is a statement about this evaluation, not a property of the
	// types: the four repaired fields are strings here, and every other number
	// in this fixture is small. The model still carries int64 quantities that
	// CAN exceed 2^53, and the case below proves this very walk finds them —
	// see TestOrderedRulesInt64JSONFieldsStillCollapseAndAreOutsideThisRepair.
	walkJSON(generic, func(path string, node any) {
		n, ok := node.(float64)
		if !ok || n < 1<<53 && n > -(1<<53) {
			return
		}
		t.Fatalf("%s encodes %v as a JSON number past 2^53, so a consumer without a 64-bit "+
			"integer type cannot read it back exactly", path, n)
	})

	var back predictioneval.OrderedRulesEvaluation
	mustUnmarshal(t, encoded, &back)
	if back.Selected == nil || back.Selected.ShareBits != ev.Selected.ShareBits {
		t.Fatalf("the selection's share did not survive the round trip: %+v", back.Selected)
	}
	for i := range ev.Trace {
		if back.Trace[i].ShareBits != ev.Trace[i].ShareBits ||
			back.Trace[i].RawWordValue != ev.Trace[i].RawWordValue {
			t.Fatalf("trace entry %d did not survive: %+v against %+v", i, back.Trace[i], ev.Trace[i])
		}
	}
}

// TestOrderedRulesTransportChangeMovedNoDigest is the other half of the repair:
// the wire form changed and NOTHING the digests witness did.
//
// The four expected values were captured by running this exact fixture on
// a3187b2 — the commit before the transport change — and are pinned here rather
// than recomputed, so the case can fail. A digest that moved would mean the
// conversion at a call site was a numeric change rather than a reinterpretation,
// and every previously issued piece of evidence would stop verifying.
func TestOrderedRulesTransportChangeMovedNoDigest(t *testing.T) {
	ev := orWireFixture(t)
	// An unread refusal carries EMPTY digests, so pinning strings without
	// checking the run reached the digests would pass on four empty values.
	if ev.Status != predictioneval.StatusWouldAttempt {
		t.Fatalf("the fixture refused with %q/%q, so the digests below would not be the "+
			"ones this case means to pin", ev.Status, ev.Reason)
	}
	for _, d := range []struct{ name, want, got string }{
		{"StreamDigest", "1d70c59897450fc359532b99e3ad7100663c3ad9790d28688baafebde291cf6e", ev.StreamDigest},
		{"ConfigDigest", "2e754165964654803883c5d512ce4e56c64dd7cedbb3307372d1e1d31ab9bf07", ev.ConfigDigest},
		{"EntropyDigest", "ef5d3511ae0f1ea6df8554ebac7fb375ff582a856679a67f0a3329079b21ba0f", ev.EntropyDigest},
		{"ConsumedInputDigest", "e8ae4efe52245e2419f7ee3e73cff563fcb939ecaa0c88a3ca448379a38826df", ev.ConsumedInputDigest},
	} {
		if len(d.got) != 64 {
			t.Fatalf("%s is %q, which is not a SHA-256; pinning it would be vacuous", d.name, d.got)
		}
		if d.got != d.want {
			t.Fatalf("%s is %s, but was %s before the transport change. Changing how a word is "+
				"WRITTEN must not change what is HASHED", d.name, d.got, d.want)
		}
	}
}

// TestOrderedRulesInt64JSONFieldsStillCollapseAndAreOutsideThisRepair is the
// limit of this repair, MEASURED and pinned rather than left for a reader to
// discover.
//
// A reviewer read the header of ordered_rules_hexword.go, which claimed "every
// exported 64-bit value", and pointed out that ten exported int64 JSON fields
// are quantities that still travel as numbers. That was correct, and the header
// now says so. This case is the evidence behind it: it runs the real types
// through a consumer that has only float64 numbers and records what each path
// actually does. Two of the three paths turn out to be DETECTED rather than
// silent, and saying which is the difference between a recorded limitation and
// a vague warning.
//
// It fails if the policy is ever widened to those fields. That is deliberate:
// whoever widens it must come here and to the header, rather than leave a
// comment behind that has quietly become false again — which is exactly the
// defect this case was filed against.
func TestOrderedRulesInt64JSONFieldsStillCollapseAndAreOutsideThisRepair(t *testing.T) {
	const bigPoints = int64(1)<<53 + 1
	cfg := orConfig(nil, 0, 100, 0, 10)

	// PATH 1 — a SOURCE relayed before projection. This one is SILENT: the
	// projection hashes what it is given, so nothing downstream can tell that
	// the points it is hashing are not the points that were supplied.
	t.Run("a relayed source silently changes the recorded share", func(t *testing.T) {
		src := predictioneval.OrderedRulesSource{
			Scope: orScope(),
			Candidates: []predictioneval.OrderedRulesCandidate{
				orCandidate("c1", 10, orKnownBalance(1000),
					orOutcome("A", bigPoints), orOutcome("B", 1)),
			},
		}
		var relayed predictioneval.OrderedRulesSource
		relayThroughFloat64(t, src, &relayed)
		got := relayed.Candidates[0].Outcomes[0].Points.Value
		if got == bigPoints {
			t.Fatalf("the relay preserved %d, so either the fields were repaired after all "+
				"or this case no longer measures anything. Update this case and the header "+
				"of ordered_rules_hexword.go together", bigPoints)
		}
		if got != bigPoints-1 {
			t.Fatalf("the relay produced %d, not the expected %d", got, bigPoints-1)
		}

		exact := evaluateSource(t, src, cfg)
		after := evaluateSource(t, relayed, cfg)
		if exact.Selected == nil || after.Selected == nil {
			t.Fatalf("both runs must admit for the shares to be comparable: %q / %q",
				exact.Status, after.Status)
		}
		// Same status, same chosen outcome, DIFFERENT recorded share — and the
		// relayed one is exactly 1.0, which reports outcome A as holding the
		// whole pool when it holds all of it but one point.
		if exact.Status != after.Status || exact.Selected.OutcomeIdentity != after.Selected.OutcomeIdentity {
			t.Fatalf("the relay changed the decision, not only the share: %q/%q against %q/%q",
				exact.Status, exact.Selected.OutcomeIdentity, after.Status, after.Selected.OutcomeIdentity)
		}
		if exact.Selected.ShareBits == after.Selected.ShareBits {
			t.Fatalf("the shares agree at %#016x, so the loss did not reach the decision",
				uint64(exact.Selected.ShareBits))
		}
		if uint64(exact.Selected.ShareBits) != 0x3feffffffffffffe ||
			uint64(after.Selected.ShareBits) != 0x3ff0000000000000 {
			t.Fatalf("measured %#016x then %#016x, want 0x3feffffffffffffe then 0x3ff0000000000000",
				uint64(exact.Selected.ShareBits), uint64(after.Selected.ShareBits))
		}
		t.Logf("silent: share %#016x became %#016x, which is exactly 1.0",
			uint64(exact.Selected.ShareBits), uint64(after.Selected.ShareBits))
	})

	// PATH 2 — a projected STREAM relayed. DETECTED: the selection digest was
	// computed over the exact values and no longer matches.
	t.Run("a relayed stream is caught by its own selection digest", func(t *testing.T) {
		cs := []predictioneval.OrderedRulesCandidate{
			orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", bigPoints), orOutcome("B", 1)),
		}
		stream := orProject(t, cs, nil)
		var relayed predictioneval.OrderedRulesStream
		relayThroughFloat64(t, stream, &relayed)
		after := predictioneval.EvaluateOrderedRules(relayed, cfg, orDraws())
		if after.Status != predictioneval.StatusRefused {
			t.Fatalf("a relayed stream evaluated to %q; the binding is supposed to refuse it",
				after.Status)
		}
		if after.Reason != predictioneval.ReasonStreamDigestMismatch {
			t.Fatalf("refused with %q, want %q", after.Reason,
				predictioneval.ReasonStreamDigestMismatch)
		}
	})

	// PATH 3 — positions. DETECTED when the collapse creates a tie, because
	// strictly increasing causal order is already a projection rule. It is NOT
	// a general guarantee: a single candidate has nothing to tie with.
	t.Run("collapsed positions are caught by the strict ordering rule", func(t *testing.T) {
		scope := orScope()
		scope.IntervalToPosition = int64(1)<<53 + 100
		src := predictioneval.OrderedRulesSource{
			Scope: scope,
			Candidates: []predictioneval.OrderedRulesCandidate{
				orCandidate("c1", int64(1)<<53, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
				orCandidate("c2", int64(1)<<53+1, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
			},
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(src, orAdmission()); err != nil {
			t.Fatalf("the exact source must project: %v", err)
		}
		var relayed predictioneval.OrderedRulesSource
		relayThroughFloat64(t, src, &relayed)
		if relayed.Candidates[0].Position != relayed.Candidates[1].Position {
			t.Fatalf("the two positions did not collapse: %d and %d",
				relayed.Candidates[0].Position, relayed.Candidates[1].Position)
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(relayed, orAdmission()); err == nil {
			t.Fatal("two candidates at the same position projected without complaint")
		}
	})

	// PATH 4 — the OUTPUT side, and the reason the walk in
	// TestOrderedRulesEvaluationCarriesHexOnTheWire is not a general no-loss
	// claim. A result carries positions straight back out as numbers, so the
	// same walk over a large-position evaluation FINDS one past 2^53. This also
	// proves that walk is a live detector rather than a check that cannot fire.
	t.Run("a result re-exports a position a float64 reader would round", func(t *testing.T) {
		scope := orScope()
		scope.IntervalToPosition = int64(1)<<53 + 100
		ev := evaluateSource(t, predictioneval.OrderedRulesSource{
			Scope: scope,
			Candidates: []predictioneval.OrderedRulesCandidate{
				orCandidate("c1", int64(1)<<53+1, orKnownBalance(1000),
					orOutcome("A", 4), orOutcome("B", 6)),
			},
		}, cfg)
		var generic any
		mustUnmarshal(t, mustMarshal(t, ev), &generic)
		found := map[string]bool{}
		walkJSON(generic, func(path string, node any) {
			if n, ok := node.(float64); ok && (n >= 1<<53 || n <= -(1<<53)) {
				found[path] = true
			}
		})
		// EVERY documented path, individually. Requiring merely one would stay
		// green if three of the four were repaired, which is the opposite of
		// what this case promises: it is supposed to fail the moment the policy
		// reaches any one of them. A reviewer pointed out that it did not.
		for _, want := range []string{
			".selected.candidatePosition",
			".stoppedAtPosition",
			".trace[].candidatePosition",
			".visits[].candidatePosition",
		} {
			if !found[want] {
				t.Fatalf("%s does not carry a number past 2^53 in a result built from a "+
					"position past 2^53. Either it is now carried losslessly — in which case "+
					"update this case, the table in SPECIFICATIONS.md and the header of "+
					"ordered_rules_hexword.go together — or the walk cannot see it, which "+
					"would make the assertion in TestOrderedRulesEvaluationCarriesHexOnTheWire "+
					"vacuous. Found: %v", want, sortedSet(found))
			}
		}
		t.Logf("still numbers past 2^53 on a result: %v", sortedSet(found))
	})
}

// relayThroughFloat64 sends a value through a consumer that has no 64-bit
// integer type: encode, decode into an interface where every JSON number
// becomes a float64, re-encode, decode back. It is the standard relay — a
// JavaScript service, jq without a flag, Go's own default — and not a contrived
// one.
func relayThroughFloat64(t *testing.T, in any, out any) {
	t.Helper()
	var generic any
	mustUnmarshal(t, mustMarshal(t, in), &generic)
	mustUnmarshal(t, mustMarshal(t, generic), out)
}

func evaluateSource(t *testing.T, src predictioneval.OrderedRulesSource,
	cfg predictioneval.OrderedRulesConfig) predictioneval.OrderedRulesEvaluation {
	t.Helper()
	stream, err := predictioneval.ProjectOrderedRulesStream(src, orAdmission())
	if err != nil {
		t.Fatalf("projecting the source failed: %v", err)
	}
	return predictioneval.EvaluateOrderedRules(stream, cfg, orDraws())
}

// orWireFixture is the one fixture the two cases above share: a run that
// admits, traces, and supplies words on both sides of 2^53 plus MaxUint64, so
// the entropy digest covers exactly the values the transport change touches.
//
// THE FIRST WORD IS THE LOSSY ONE, and that ordering is the whole point of this
// helper rather than a detail of it. The first rule matches at fifty percent
// and its draw succeeds immediately, so exactly ONE word is ever consumed and
// the consumed-prefix digest covers exactly that word. An earlier version led
// with zero: the run consumed it, succeeded, and every risky word after it was
// an unconsumed suffix that only the SEPARATE entropy digest covered. A
// regression confined to orderedRulesConsumedDigest — hashing uint64(float64(w)),
// say — left the pin below green, because zero survives that round trip. A
// reviewer found it. 2^53+1 is under the half-rate threshold of 2^63, so it
// still admits, and it does not survive a float64.
func orWireFixture(t *testing.T) predictioneval.OrderedRulesEvaluation {
	t.Helper()
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 50, 0, 10),
	}, 95, 100, 0, 10)
	return predictioneval.EvaluateOrderedRules(orProject(t, cs, nil), cfg,
		orDraws(1<<53+1, 0, 0x7fffffffffffffff, math.MaxUint64, 1<<53))
}

// walkJSON visits every node of a decoded JSON document, naming each by a path
// like "trace[].rawWordValue" so a failure says WHERE rather than only what.
func walkJSON(node any, visit func(path string, node any)) {
	var walk func(string, any)
	walk = func(path string, n any) {
		visit(path, n)
		switch v := n.(type) {
		case map[string]any:
			for _, k := range sortedKeys(v) {
				walk(path+"."+k, v[k])
			}
		case []any:
			for _, e := range v {
				walk(path+"[]", e)
			}
		}
	}
	walk("", node)
}

// sortedSet names a set of paths in a stable order, so a failure reads the same
// on every run.
func sortedSet(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling %T failed: %v", v, err)
	}
	return string(out)
}

func mustUnmarshal(t *testing.T, in string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(in), into); err != nil {
		t.Fatalf("decoding %s into %T failed: %v", in, into, err)
	}
}

// decodeThroughFloat64 is the IEEE-754 consumer both representations are read
// by: encoding/json into an interface, where every JSON number becomes a
// float64. It returns the shareBits field exactly as such a reader sees it.
func decodeThroughFloat64(t *testing.T, in string) any {
	t.Helper()
	var generic map[string]any
	mustUnmarshal(t, in, &generic)
	got, ok := generic["shareBits"]
	if !ok {
		t.Fatalf("%s carries no shareBits field", in)
	}
	return got
}
