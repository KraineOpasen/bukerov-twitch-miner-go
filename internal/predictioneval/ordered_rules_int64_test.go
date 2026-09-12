package predictioneval_test

// THE SIGNED-QUANTITY WIRE CONTRACT, under falsification.
//
// Twelve exported fields carry signed quantities and causal positions. They
// travelled as JSON numbers until [predictioneval.OrderedRulesInt64] was
// introduced, and a JSON number is read by an IEEE-754 consumer as a float64,
// which cannot hold every int64.
//
// The strongest property this codec supports is a round trip, so that is what
// is asserted — but a round trip alone cannot distinguish a correct codec from
// two mistakes that cancel, and decode() re-encodes internally to check
// canonicality, which would make decode(encode(x)) == x partly self-referential.
// So the ENCODER is constrained separately, against hand-written literals in
// int64Vectors, and the DECODER against the same literals in the other
// direction. The round trip then adds domain coverage on top of two
// independently pinned directions rather than standing in for them.
//
// There is no property-based-testing library here on purpose: this PR commits
// to leaving go.mod untouched, so the sweeps below are deterministic and wide
// rather than generated. int64Sweep is every power of two and its neighbours in
// both signs, which is where a shift, a sign bug or an off-by-one lives.

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// int64Vectors is the audit's own list, plus the two extremes.
//
// The 2^53 neighbourhood is the whole point: below it a float64 holds every
// integer exactly, at and above it they start being skipped, and the negative
// side is included because this contract carries signed values and a codec that
// dropped the sign would pass a positive-only table.
var int64Vectors = []struct {
	name  string
	value int64
	text  string
}{
	{"MinInt64", math.MinInt64, "-9223372036854775808"},
	{"minus 2^53 minus one", -(1 << 53) - 1, "-9007199254740993"},
	{"minus 2^53", -(1 << 53), "-9007199254740992"},
	{"minus 2^53 plus one", -(1 << 53) + 1, "-9007199254740991"},
	{"minus one", -1, "-1"},
	{"zero", 0, "0"},
	{"one", 1, "1"},
	{"2^53 minus one, the last exact double", 1<<53 - 1, "9007199254740991"},
	{"2^53, where doubles start skipping", 1 << 53, "9007199254740992"},
	{"2^53 plus one, the first integer no double holds", 1<<53 + 1, "9007199254740993"},
	{"MaxInt64", math.MaxInt64, "9223372036854775807"},
}

// TestOrderedRulesInt64EncodesEveryBoundaryExactly pins both directions against
// literals, so neither side is checked only by the other.
func TestOrderedRulesInt64EncodesEveryBoundaryExactly(t *testing.T) {
	for _, v := range int64Vectors {
		t.Run(v.name, func(t *testing.T) {
			encoded, err := predictioneval.OrderedRulesInt64(v.value).MarshalText()
			if err != nil {
				t.Fatalf("encoding %d failed: %v", v.value, err)
			}
			if string(encoded) != v.text {
				t.Fatalf("%d encodes to %q, want %q", v.value, encoded, v.text)
			}
			// Set to something else first, so a decoder that never assigns is
			// not read as a decoder that assigned the right value.
			decoded := predictioneval.OrderedRulesInt64(math.MinInt64 + 7)
			if err := decoded.UnmarshalText([]byte(v.text)); err != nil {
				t.Fatalf("decoding %q failed: %v", v.text, err)
			}
			if int64(decoded) != v.value {
				t.Fatalf("%q decodes to %d, want %d", v.text, int64(decoded), v.value)
			}
		})
	}
}

// TestOrderedRulesInt64RoundTripsTheWholeSweep is the domain-coverage half.
//
// Every power of two and its neighbours, in both signs, plus a deterministic
// spread. A codec that mishandled any one bit position, or the sign, or the
// two-ended asymmetry of int64 — MinInt64 has no positive counterpart — fails
// on that value rather than on whichever value a hand-picked table contained.
func TestOrderedRulesInt64RoundTripsTheWholeSweep(t *testing.T) {
	sweep := int64Sweep()
	if len(sweep) < 128 {
		t.Fatalf("the sweep is %d values, too few to cover the bit positions", len(sweep))
	}
	for _, v := range sweep {
		encoded, err := predictioneval.OrderedRulesInt64(v).MarshalText()
		if err != nil {
			t.Fatalf("encoding %d failed: %v", v, err)
		}
		// The encoder is constrained here by strconv rather than by the
		// decoder, which keeps this from restating the implementation twice.
		if want := strconv.FormatInt(v, 10); string(encoded) != want {
			t.Fatalf("%d encodes to %q, want %q", v, encoded, want)
		}
		got := predictioneval.OrderedRulesInt64(math.MaxInt64)
		if err := got.UnmarshalText(encoded); err != nil {
			t.Fatalf("decoding %q failed: %v", encoded, err)
		}
		if int64(got) != v {
			t.Fatalf("%d round-tripped to %d", v, int64(got))
		}
	}
}

// int64Sweep is deterministic on purpose: a test that samples a different space
// on each run reports a different thing on each run.
func int64Sweep() []int64 {
	vs := []int64{0, 1, -1, math.MinInt64, math.MaxInt64, math.MinInt64 + 1, math.MaxInt64 - 1}
	for b := 0; b < 63; b++ {
		p := int64(1) << b
		vs = append(vs, p, p-1, p+1, -p, -p-1, -p+1)
	}
	v := int64(0x5deece66d)
	for i := 0; i < 128; i++ {
		v = v*6364136223846793005 + 1442695040888963407
		vs = append(vs, v)
	}
	return vs
}

// TestOrderedRulesInt64RefusesEveryNonCanonicalSpelling is the strictness the
// owner decision specified: one value, one encoding, and no repair of input.
//
// Note what the rows share: every one of them PARSES to a number a human would
// call correct. They are refused anyway, because the contract names a spelling
// and not a value — a decoder that repairs its input cannot tell a caller that
// the artifact it read was the artifact that was written.
func TestOrderedRulesInt64RefusesEveryNonCanonicalSpelling(t *testing.T) {
	for _, c := range []struct{ name, text string }{
		{"empty", ""},
		{"a lone minus", "-"},
		{"a plus sign", "+1"},
		{"one leading zero", "01"},
		{"many leading zeros", "0000000001"},
		{"negative zero", "-0"},
		{"negative with a leading zero", "-01"},
		{"leading space", " 1"},
		{"trailing space", "1 "},
		{"a trailing newline", "1\n"},
		{"an underscore separator", "1_000"},
		{"hexadecimal", "0x10"},
		{"exponent form", "1e3"},
		{"a decimal point", "1.0"},
		{"a trailing decimal point", "1."},
		{"thousands separators", "1,000"},
		{"one past MaxInt64", "9223372036854775808"},
		{"one below MinInt64", "-9223372036854775809"},
		{"far out of range", "99999999999999999999999999"},
		{"non-ASCII digits", "١٢٣"},
		{"an interior NUL", "1\x002"},
		{"a quoted number", "\"1\""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got predictioneval.OrderedRulesInt64
			err := got.UnmarshalText([]byte(c.text))
			if err == nil {
				t.Fatalf("%q was accepted as %d; it is not what the encoder writes",
					c.text, int64(got))
			}
			if !errors.Is(err, predictioneval.ErrOrderedRulesInt64Malformed) {
				t.Fatalf("%q was refused with %v, which does not wrap "+
					"ErrOrderedRulesInt64Malformed; a caller cannot classify it", c.text, err)
			}
		})
	}
}

// TestOrderedRulesInt64RefusalLeavesTheDestinationUntouched pins that a refusal
// is not a partial write.
func TestOrderedRulesInt64RefusalLeavesTheDestinationUntouched(t *testing.T) {
	for _, text := range []string{"01", "-0", "+7", "1_0", "9223372036854775808", ""} {
		before := predictioneval.OrderedRulesInt64(-4242424242424242)
		got := before
		if err := got.UnmarshalText([]byte(text)); err == nil {
			t.Fatalf("%q was accepted", text)
		}
		if got != before {
			t.Fatalf("refusing %q changed the destination from %d to %d",
				text, int64(before), int64(got))
		}
	}
}

// TestOrderedRulesInt64RefusesAJSONNumberAtTheTypeLevel is why the owner
// decision needed no numeric compatibility shim.
//
// encoding/json will not hand a JSON number to a TextUnmarshaler at all, so the
// old representation is closed off by the type rather than by a check this
// package would have to keep writing. The refusal is json's own and is NOT
// wrapped in ErrOrderedRulesInt64Malformed, which is asserted rather than
// glossed: a caller classifying malformed input has to know the number case
// arrives as a *json.UnmarshalTypeError.
func TestOrderedRulesInt64RefusesAJSONNumberAtTheTypeLevel(t *testing.T) {
	for _, in := range []string{
		`{"value":9007199254740993}`,
		`{"value":0}`,
		`{"value":-1}`,
		`{"value":1.5}`,
		`{"value":true}`,
		`{"value":[]}`,
	} {
		var into struct {
			Value predictioneval.OrderedRulesInt64 `json:"value"`
		}
		err := json.Unmarshal([]byte(in), &into)
		if err == nil {
			t.Fatalf("%s decoded without error as %d; the numeric form is the representation "+
				"this contract exists to remove", in, int64(into.Value))
		}
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			t.Fatalf("%s was refused with %v, want a *json.UnmarshalTypeError", in, err)
		}
	}
}

// TestOrderedRulesInt64NullIsANoOpAndNotAZero records a property this repair
// did NOT change and does not claim to, exactly as its sibling does for hex.
func TestOrderedRulesInt64NullIsANoOpAndNotAZero(t *testing.T) {
	kept := struct {
		Value predictioneval.OrderedRulesInt64 `json:"value"`
	}{Value: 9007199254740993}
	if err := json.Unmarshal([]byte(`{"value":null}`), &kept); err != nil {
		t.Fatalf("a null was refused: %v. That is a stricter contract than this repair "+
			"introduced, so the doc is now wrong rather than the code", err)
	}
	if int64(kept.Value) != 9007199254740993 {
		t.Fatalf("a null overwrote an existing value with %d", int64(kept.Value))
	}
}

// int64Fields is the twelve, each with the smallest carrier that holds it.
//
// They are listed rather than reached by reflection because this table is what
// the header of ordered_rules_int64.go and the specification both claim, and a
// reflective loop would agree with the code no matter which fields the code
// covered. TestOrderedRulesExportedGraphCarriesNoBare64BitJSONField reaches
// them reflectively and is the other half: this one says "these twelve are
// covered", that one says "and no thirteenth was missed".
var int64Fields = []struct {
	name      string
	key       string
	omitEmpty bool
	roundTrip func(t *testing.T, v predictioneval.OrderedRulesInt64) (string, predictioneval.OrderedRulesInt64)
}{
	{"SuppliedInt64.Value", "value", false,
		func(t *testing.T, v predictioneval.OrderedRulesInt64) (string, predictioneval.OrderedRulesInt64) {
			enc := mustMarshal(t, predictioneval.SuppliedInt64{Value: v})
			var out predictioneval.SuppliedInt64
			mustUnmarshal(t, enc, &out)
			return enc, out.Value
		}},
	{"SuppliedInt64.AvailableAtPosition", "availableAtPosition", false,
		func(t *testing.T, v predictioneval.OrderedRulesInt64) (string, predictioneval.OrderedRulesInt64) {
			enc := mustMarshal(t, predictioneval.SuppliedInt64{AvailableAtPosition: v})
			var out predictioneval.SuppliedInt64
			mustUnmarshal(t, enc, &out)
			return enc, out.AvailableAtPosition
		}},
	{"OrderedRulesScope.IntervalFromPosition", "intervalFromPosition", false,
		func(t *testing.T, v predictioneval.OrderedRulesInt64) (string, predictioneval.OrderedRulesInt64) {
			enc := mustMarshal(t, predictioneval.OrderedRulesScope{IntervalFromPosition: v})
			var out predictioneval.OrderedRulesScope
			mustUnmarshal(t, enc, &out)
			return enc, out.IntervalFromPosition
		}},
	{"OrderedRulesScope.IntervalToPosition", "intervalToPosition", false,
		func(t *testing.T, v predictioneval.OrderedRulesInt64) (string, predictioneval.OrderedRulesInt64) {
			enc := mustMarshal(t, predictioneval.OrderedRulesScope{IntervalToPosition: v})
			var out predictioneval.OrderedRulesScope
			mustUnmarshal(t, enc, &out)
			return enc, out.IntervalToPosition
		}},
	{"OrderedRulesCandidate.Position", "position", false,
		func(t *testing.T, v predictioneval.OrderedRulesInt64) (string, predictioneval.OrderedRulesInt64) {
			enc := mustMarshal(t, predictioneval.OrderedRulesCandidate{Position: v})
			var out predictioneval.OrderedRulesCandidate
			mustUnmarshal(t, enc, &out)
			return enc, out.Position
		}},
	{"OrderedRulesIntervention.Position", "position", false,
		func(t *testing.T, v predictioneval.OrderedRulesInt64) (string, predictioneval.OrderedRulesInt64) {
			enc := mustMarshal(t, predictioneval.OrderedRulesIntervention{Position: v})
			var out predictioneval.OrderedRulesIntervention
			mustUnmarshal(t, enc, &out)
			return enc, out.Position
		}},
	{"OrderedRulesCutoff.Position", "position", false,
		func(t *testing.T, v predictioneval.OrderedRulesInt64) (string, predictioneval.OrderedRulesInt64) {
			enc := mustMarshal(t, predictioneval.OrderedRulesCutoff{Position: v})
			var out predictioneval.OrderedRulesCutoff
			mustUnmarshal(t, enc, &out)
			return enc, out.Position
		}},
	{"OrderedRulesSelection.CandidatePosition", "candidatePosition", false,
		func(t *testing.T, v predictioneval.OrderedRulesInt64) (string, predictioneval.OrderedRulesInt64) {
			enc := mustMarshal(t, predictioneval.OrderedRulesSelection{CandidatePosition: v})
			var out predictioneval.OrderedRulesSelection
			mustUnmarshal(t, enc, &out)
			return enc, out.CandidatePosition
		}},
	{"OrderedRulesTraceEntry.CandidatePosition", "candidatePosition", false,
		func(t *testing.T, v predictioneval.OrderedRulesInt64) (string, predictioneval.OrderedRulesInt64) {
			enc := mustMarshal(t, predictioneval.OrderedRulesTraceEntry{CandidatePosition: v})
			var out predictioneval.OrderedRulesTraceEntry
			mustUnmarshal(t, enc, &out)
			return enc, out.CandidatePosition
		}},
	{"OrderedRulesEvaluation.StoppedAtPosition", "stoppedAtPosition", true,
		func(t *testing.T, v predictioneval.OrderedRulesInt64) (string, predictioneval.OrderedRulesInt64) {
			enc := mustMarshal(t, predictioneval.OrderedRulesEvaluation{StoppedAtPosition: v})
			var out predictioneval.OrderedRulesEvaluation
			mustUnmarshal(t, enc, &out)
			return enc, out.StoppedAtPosition
		}},
	{"OrderedRulesCandidateVisit.CandidatePosition", "candidatePosition", false,
		func(t *testing.T, v predictioneval.OrderedRulesInt64) (string, predictioneval.OrderedRulesInt64) {
			enc := mustMarshal(t, predictioneval.OrderedRulesCandidateVisit{CandidatePosition: v})
			var out predictioneval.OrderedRulesCandidateVisit
			mustUnmarshal(t, enc, &out)
			return enc, out.CandidatePosition
		}},
	{"OrderedRulesCandidateVisit.PoolTotal", "poolTotal", false,
		func(t *testing.T, v predictioneval.OrderedRulesInt64) (string, predictioneval.OrderedRulesInt64) {
			enc := mustMarshal(t, predictioneval.OrderedRulesCandidateVisit{PoolTotal: v})
			var out predictioneval.OrderedRulesCandidateVisit
			mustUnmarshal(t, enc, &out)
			return enc, out.PoolTotal
		}},
}

// TestOrderedRulesAllTwelveInt64FieldsRoundTripExactly walks every field at
// every boundary, in its real carrier rather than in a stand-in.
//
// The type is exercised elsewhere; what this pins is that each of the twelve
// declarations actually USES it. Each case asserts the encoded TEXT as well as
// the decoded value, because a field left as a plain int64 would still round
// trip through Go and still lose its value on a wire.
func TestOrderedRulesAllTwelveInt64FieldsRoundTripExactly(t *testing.T) {
	if len(int64Fields) != 12 {
		t.Fatalf("this case covers %d fields; the contract names twelve", len(int64Fields))
	}
	for _, f := range int64Fields {
		t.Run(f.name, func(t *testing.T) {
			for _, v := range int64Vectors {
				encoded, got := f.roundTrip(t, predictioneval.OrderedRulesInt64(v.value))
				if int64(got) != v.value {
					t.Fatalf("%s round-tripped %d to %d via %s", f.name, v.value, int64(got), encoded)
				}
				quoted := `"` + f.key + `":"` + v.text + `"`
				if f.omitEmpty && v.value == 0 {
					// omitempty is evaluated on the value's kind before any
					// marshaler runs, so a zero is omitted exactly as it was
					// when the field was a plain int64. Pinned, not assumed.
					if strings.Contains(encoded, `"`+f.key+`"`) {
						t.Fatalf("%s carries %q at zero despite omitempty: %s", f.name, f.key, encoded)
					}
					continue
				}
				if !strings.Contains(encoded, quoted) {
					t.Fatalf("%s encoded %d as %s, want it to carry %s", f.name, v.value, encoded, quoted)
				}
				// And never as a bare number, which is the representation the
				// change exists to remove.
				if bare := `"` + f.key + `":` + v.text; strings.Contains(encoded, bare) {
					t.Fatalf("%s still encodes %d as a JSON number: %s", f.name, v.value, encoded)
				}
			}
		})
	}
}

// TestOrderedRulesExportedGraphCarriesNoBare64BitJSONField is the audit the
// owner decision asked for, and it reaches the fields reflectively so it cannot
// agree with a list that is itself wrong.
//
// It walks every exported ordered-rules type reachable from the five roots of
// the public surface and requires that any JSON-tagged 64-bit field be one of
// the two wire types. A thirteenth field added later, of either kind, fails
// here rather than being discovered by a reviewer.
func TestOrderedRulesExportedGraphCarriesNoBare64BitJSONField(t *testing.T) {
	roots := []any{
		predictioneval.OrderedRulesSource{},
		predictioneval.OrderedRulesStream{},
		predictioneval.OrderedRulesConfig{},
		predictioneval.SuppliedDrawTrace{},
		predictioneval.OrderedRulesEvaluation{},
	}
	hexType := reflect.TypeOf(predictioneval.OrderedRulesHex64(0))
	intType := reflect.TypeOf(predictioneval.OrderedRulesInt64(0))

	seen := map[reflect.Type]bool{}
	covered, ints := 0, []string{}
	var walk func(reflect.Type, string)
	walk = func(rt reflect.Type, path string) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.PkgPath != "" {
				continue // unexported: not on the wire at all
			}
			tag, tagged := f.Tag.Lookup("json")
			where := path + "." + f.Name
			ft := f.Type
			for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice || ft.Kind() == reflect.Array {
				ft = ft.Elem()
			}
			switch ft.Kind() {
			case reflect.Int64, reflect.Uint64:
				if !tagged || tag == "-" {
					continue
				}
				if ft != hexType && ft != intType {
					t.Errorf("%s is a bare %s on the wire (json:%q). Every exported 64-bit "+
						"field in this graph must travel as OrderedRulesInt64 or "+
						"OrderedRulesHex64; a JSON number cannot carry one exactly.",
						where, ft.Kind(), tag)
					continue
				}
				covered++
			case reflect.Int, reflect.Uint, reflect.Uintptr:
				if tagged && tag != "-" {
					ints = append(ints, where)
				}
			case reflect.Struct:
				walk(f.Type, where)
			}
		}
	}
	for _, r := range roots {
		rt := reflect.TypeOf(r)
		walk(rt, rt.Name())
	}
	if covered < 16 {
		t.Fatalf("the walk found only %d covered 64-bit fields; it is not reaching the graph, "+
			"so its silence would mean nothing", covered)
	}
	// The `int` fields are reported rather than failed. They are not int64 and
	// the owner decision named twelve int64 fields, so converting them is not
	// this change's to make — but they ARE 64 bits wide on this platform, so
	// leaving them unnamed would be the same silence this audit exists to break.
	// TestOrderedRulesIntWidthFieldsAreStructurallyBounded is what keeps them
	// honest.
	t.Logf("covered 64-bit wire fields: %d", covered)
	t.Logf("platform-width int fields on the wire (see the bounded-ness case): %v", sortedStrings(ints))
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TestOrderedRulesAFloat64RelayCanNoLongerCorruptASource is the proof the owner
// decision asked for, stated as the exact negation of what was measured before.
//
// The previous head demonstrated this concretely: a SOURCE relayed through a
// consumer that has only float64 numbers had its supplied points of 2^53+1
// arrive as 2^53, the exact int64 pool sum shifted with them, and the recorded
// share moved from 0x3feffffffffffffe to 0x3ff0000000000000 — exactly 1.0,
// reporting an outcome as holding the whole pool when it holds all but one
// point. Same status, same chosen outcome, silently different evidence.
//
// THE CONTROL IS THE POINT OF THE FIRST SUBTEST. A relay that had quietly
// become lossless would make every assertion below pass while proving nothing,
// so the same helper is first shown to still destroy a plain int64. Only then
// is the repaired carrier put through it.
func TestOrderedRulesAFloat64RelayCanNoLongerCorruptASource(t *testing.T) {
	const bigPoints = int64(1)<<53 + 1

	t.Run("the relay still destroys a plain int64, so it is a real relay", func(t *testing.T) {
		var out struct {
			N int64 `json:"n"`
		}
		relayThroughFloat64(t, struct {
			N int64 `json:"n"`
		}{N: bigPoints}, &out)
		if out.N == bigPoints {
			t.Fatalf("the relay preserved %d on a bare int64. It is no longer a float64 "+
				"consumer, so nothing below measures anything", bigPoints)
		}
		if out.N != bigPoints-1 {
			t.Fatalf("the relay produced %d, not the expected %d", out.N, bigPoints-1)
		}
	})

	source := func() predictioneval.OrderedRulesSource {
		return predictioneval.OrderedRulesSource{
			Scope: orScope(),
			Candidates: []predictioneval.OrderedRulesCandidate{
				orCandidate("c1", 10, orKnownBalance(1000),
					orOutcome("A", bigPoints), orOutcome("B", 1)),
			},
		}
	}
	cfg := orConfig(nil, 0, 100, 0, 10)

	t.Run("a relayed source keeps its points, its pool sum and its share", func(t *testing.T) {
		var relayed predictioneval.OrderedRulesSource
		relayThroughFloat64(t, source(), &relayed)
		if got := int64(relayed.Candidates[0].Outcomes[0].Points.Value); got != bigPoints {
			t.Fatalf("the relay changed supplied points from %d to %d", bigPoints, got)
		}

		exact := evaluateSource(t, source(), cfg)
		after := evaluateSource(t, relayed, cfg)
		if exact.Selected == nil || after.Selected == nil {
			t.Fatalf("both runs must admit: %q / %q", exact.Status, after.Status)
		}
		if exact.Selected.ShareBits != after.Selected.ShareBits {
			t.Fatalf("the recorded share moved across the relay: %#016x then %#016x",
				uint64(exact.Selected.ShareBits), uint64(after.Selected.ShareBits))
		}
		// The value the old transport produced, named so a regression that
		// reintroduces it fails here with the number rather than a generic
		// mismatch. 0x3ff0000000000000 is exactly 1.0.
		if uint64(after.Selected.ShareBits) == 0x3ff0000000000000 {
			t.Fatalf("the share is exactly 1.0 again, which is the corruption this "+
				"transport was introduced to remove (was %#016x)", uint64(exact.Selected.ShareBits))
		}
		if len(after.Visits) == 0 || after.Visits[0].PoolTotal != exact.Visits[0].PoolTotal {
			t.Fatalf("the pool total moved across the relay: %v", after.Visits)
		}
		if int64(exact.Visits[0].PoolTotal) != bigPoints+1 {
			t.Fatalf("the fixture's pool total is %d, not the %d this case means to carry; "+
				"it would no longer be past 2^53", int64(exact.Visits[0].PoolTotal), bigPoints+1)
		}
		// Every digest too: the relay is now a no-op on this document.
		for _, d := range []struct{ name, a, b string }{
			{"StreamDigest", exact.StreamDigest, after.StreamDigest},
			{"ConfigDigest", exact.ConfigDigest, after.ConfigDigest},
			{"EntropyDigest", exact.EntropyDigest, after.EntropyDigest},
			{"ConsumedInputDigest", exact.ConsumedInputDigest, after.ConsumedInputDigest},
		} {
			if d.a != d.b {
				t.Fatalf("%s moved across the relay: %s then %s", d.name, d.a, d.b)
			}
		}
	})

	t.Run("a relayed stream still verifies against its own selection digest", func(t *testing.T) {
		stream, err := predictioneval.ProjectOrderedRulesStream(source(), orAdmission())
		if err != nil {
			t.Fatalf("project: %v", err)
		}
		var relayed predictioneval.OrderedRulesStream
		relayThroughFloat64(t, stream, &relayed)
		after := predictioneval.EvaluateOrderedRules(relayed, cfg, orDraws())
		if after.Status == predictioneval.StatusRefused &&
			after.Reason == predictioneval.ReasonStreamDigestMismatch {
			t.Fatal("a relayed stream is still refused for a digest mismatch, so the " +
				"transport did not make the round trip exact")
		}
		if after.Status != predictioneval.StatusWouldAttempt {
			t.Fatalf("the relayed stream evaluated to %q / %q", after.Status, after.Reason)
		}
	})

	t.Run("positions one apart no longer collapse", func(t *testing.T) {
		scope := orScope()
		scope.IntervalToPosition = predictioneval.OrderedRulesInt64(int64(1)<<53 + 100)
		src := predictioneval.OrderedRulesSource{
			Scope: scope,
			Candidates: []predictioneval.OrderedRulesCandidate{
				orCandidate("c1", int64(1)<<53, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
				orCandidate("c2", int64(1)<<53+1, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
			},
		}
		var relayed predictioneval.OrderedRulesSource
		relayThroughFloat64(t, src, &relayed)
		a, b := relayed.Candidates[0].Position, relayed.Candidates[1].Position
		if a == b {
			t.Fatalf("the two positions still collapsed, both to %d", int64(a))
		}
		if int64(a) != int64(1)<<53 || int64(b) != int64(1)<<53+1 {
			t.Fatalf("the relayed positions are %d and %d", int64(a), int64(b))
		}
		if _, err := predictioneval.ProjectOrderedRulesStream(relayed, orAdmission()); err != nil {
			t.Fatalf("the relayed source must still project: %v", err)
		}
	})
}

// TestOrderedRulesResultCarriesNoNumberAFloat64ReaderWouldRound is the
// result-side half, over every path that used to carry one.
//
// The four position paths and PoolTotal are named individually rather than
// checked in aggregate, because requiring merely "no large number anywhere"
// would stay green if a path stopped being emitted at all. Each must be present
// AND be a string AND decode back to the exact value.
func TestOrderedRulesResultCarriesNoNumberAFloat64ReaderWouldRound(t *testing.T) {
	const pos = int64(1)<<53 + 1
	scope := orScope()
	scope.IntervalToPosition = predictioneval.OrderedRulesInt64(int64(1)<<53 + 100)
	ev := evaluateSource(t, predictioneval.OrderedRulesSource{
		Scope: scope,
		Candidates: []predictioneval.OrderedRulesCandidate{
			orCandidate("c1", pos, orKnownBalance(1000),
				orOutcome("A", int64(1)<<53+1), orOutcome("B", 1)),
		},
	}, orConfig(nil, 0, 100, 0, 10))
	if ev.Selected == nil || len(ev.Trace) == 0 || len(ev.Visits) == 0 {
		t.Fatalf("the fixture must admit, trace and visit for this case to inspect anything: "+
			"%q / %q", ev.Status, ev.Reason)
	}

	encoded := mustMarshal(t, ev)
	var generic any
	mustUnmarshal(t, encoded, &generic)

	want := map[string]string{
		".selected.candidatePosition": strconv.FormatInt(pos, 10),
		".stoppedAtPosition":          strconv.FormatInt(pos, 10),
		".trace[].candidatePosition":  strconv.FormatInt(pos, 10),
		".visits[].candidatePosition": strconv.FormatInt(pos, 10),
		".visits[].poolTotal":         strconv.FormatInt(int64(1)<<53+2, 10),
	}
	found := map[string]bool{}
	walkJSON(generic, func(path string, node any) {
		text, ok := want[path]
		if !ok {
			return
		}
		found[path] = true
		got, isString := node.(string)
		if !isString {
			t.Fatalf("%s is %#v, a %T; it must be a string on this wire", path, node, node)
		}
		if got != text {
			t.Fatalf("%s is %q, want %q", path, got, text)
		}
	})
	for path := range want {
		if !found[path] {
			t.Fatalf("%s does not appear in the encoded result at all, so this case checked "+
				"nothing about it. Encoded: %s", path, encoded)
		}
	}

	// And nothing anywhere in the document is a number a float64 reader would
	// round — including a field nobody has added yet.
	walkJSON(generic, func(path string, node any) {
		if n, ok := node.(float64); ok && (n >= 1<<53 || n <= -(1<<53)) {
			t.Fatalf("%s encodes %v as a JSON number past 2^53", path, n)
		}
	})

	// The whole result survives a relay unchanged, which is the property all of
	// the above adds up to.
	var relayed predictioneval.OrderedRulesEvaluation
	relayThroughFloat64(t, ev, &relayed)
	if relayed.Selected.CandidatePosition != ev.Selected.CandidatePosition ||
		relayed.StoppedAtPosition != ev.StoppedAtPosition ||
		relayed.Visits[0].PoolTotal != ev.Visits[0].PoolTotal ||
		relayed.Trace[0].CandidatePosition != ev.Trace[0].CandidatePosition {
		t.Fatalf("the result did not survive a float64 relay: %+v", relayed.Selected)
	}
}

// TestOrderedRulesIntWidthFieldsAreStructurallyBounded closes the gap the
// reflective audit reports rather than leaving it as a log line.
//
// Fifteen wire fields are platform-width `int`, which is 64 bits here. They are
// NOT part of the owner-approved twelve, and converting them was not
// authorized — so the claim that they are safe has to be a measurement instead.
// Every one of them is either an index or a counter bounded by a declared
// ceiling, or a caller-supplied count the invariant pass refuses above the
// candidate ceiling. The one that is caller-supplied is probed directly.
func TestOrderedRulesIntWidthFieldsAreStructurallyBounded(t *testing.T) {
	cs := []predictioneval.OrderedRulesCandidate{
		orCandidate("c1", 10, orKnownBalance(1000), orOutcome("A", 4), orOutcome("B", 6)),
	}
	cfg := orConfig(nil, 0, 100, 0, 10)

	// DroppedAtOrAfter is the only one a caller writes. A forged value past the
	// candidate ceiling is refused, and the refusal does not re-export it.
	for _, dropped := range []int{1 << 53, 1 << 60, int(^uint(0) >> 1)} {
		stream := orProject(t, cs, nil)
		stream.Cutoff.DroppedAtOrAfter = dropped
		ev := predictioneval.EvaluateOrderedRules(stream, cfg, orDraws())
		if ev.Status != predictioneval.StatusRefused ||
			ev.Reason != predictioneval.ReasonStreamInvariantViolated {
			t.Fatalf("a stream claiming %d removals evaluated to %q / %q; it must be refused",
				dropped, ev.Status, ev.Reason)
		}
		if ev.Cutoff.DroppedAtOrAfter != 0 {
			t.Fatalf("the refusal re-exported the forged count %d", ev.Cutoff.DroppedAtOrAfter)
		}
		var generic any
		mustUnmarshal(t, mustMarshal(t, ev), &generic)
		walkJSON(generic, func(path string, node any) {
			if n, ok := node.(float64); ok && (n >= 1<<53 || n <= -(1<<53)) {
				t.Fatalf("a refusal for a forged count of %d still encodes %s as %v", dropped, path, n)
			}
		})
	}

	// And an ordinary admitted run: every counter and index it reports is far
	// inside the exact-integer range, because each is bounded by a ceiling this
	// package declares.
	ev := evaluateSource(t, predictioneval.OrderedRulesSource{
		Scope: orScope(), Candidates: cs,
	}, cfg)
	var generic any
	mustUnmarshal(t, mustMarshal(t, ev), &generic)
	seen := 0
	walkJSON(generic, func(path string, node any) {
		n, ok := node.(float64)
		if !ok {
			return
		}
		seen++
		if n >= 1<<53 || n <= -(1<<53) {
			t.Fatalf("%s encodes %v as a JSON number past 2^53", path, n)
		}
	})
	if seen == 0 {
		t.Fatal("the result carries no JSON numbers at all, so this case checked nothing")
	}
	t.Logf("%d JSON numbers in an admitted result, all inside the exact-integer range", seen)
}

// orInt64WireFixture is a run whose POSITIONS and POINTS are past 2^53 in both
// signs, so the digests below witness exactly the values this change touches.
func orInt64WireFixture(t *testing.T) predictioneval.OrderedRulesEvaluation {
	t.Helper()
	scope := orScope()
	scope.IntervalFromPosition = predictioneval.OrderedRulesInt64(-(int64(1) << 53) - 1)
	scope.IntervalToPosition = math.MaxInt64
	candidate := predictioneval.OrderedRulesCandidate{
		Identity: "c1", Position: predictioneval.OrderedRulesInt64(int64(1)<<53 + 1),
		HasPosition:       true,
		SourceKind:        predictioneval.SourceKindChannelUpdate,
		EpisodeMembership: predictioneval.MembershipProven,
		OutcomesPresence:  predictioneval.SuppliedKnown,
		Provenance:        "acceptance fixture",
		Balance: predictioneval.SuppliedInt64{
			Presence: predictioneval.SuppliedKnown, Value: 1000,
			Provenance: "acceptance fixture", HasAvailableAtPosition: true,
			AvailableAtPosition: predictioneval.OrderedRulesInt64(-(int64(1) << 53)),
		},
		Outcomes: []predictioneval.OrderedRulesOutcome{
			{Identity: "A", Points: predictioneval.SuppliedInt64{
				Presence:   predictioneval.SuppliedKnown,
				Value:      predictioneval.OrderedRulesInt64(int64(1)<<53 + 1),
				Provenance: "acceptance fixture", HasAvailableAtPosition: true,
				AvailableAtPosition: predictioneval.OrderedRulesInt64(int64(1)<<53 + 1),
			}},
			{Identity: "B", Points: predictioneval.SuppliedInt64{
				Presence: predictioneval.SuppliedKnown, Value: 1,
				Provenance: "acceptance fixture", HasAvailableAtPosition: true,
			}},
		},
	}
	stream, err := predictioneval.ProjectOrderedRulesStream(
		predictioneval.OrderedRulesSource{Scope: scope,
			Candidates: []predictioneval.OrderedRulesCandidate{candidate}}, orAdmission())
	if err != nil {
		t.Fatalf("the fixture must project: %v", err)
	}
	cfg := orConfig([]predictioneval.OrderedRule{
		orRule(predictioneval.ComparatorLe, 50, 50, 0, 10),
	}, 95, 100, 0, 10)
	return predictioneval.EvaluateOrderedRules(stream, cfg, orDraws(1<<53+1, 0, math.MaxUint64))
}

// TestOrderedRulesInt64TransportMovedNoDigest is the other half of the repair:
// the wire form of twelve fields changed and NOTHING the digests witness did.
//
// The expected values were captured by running this exact fixture on 38d0ff5 —
// the commit before the int64 transport change — in a detached worktree, and
// are pinned here rather than recomputed, so the case can fail. A digest that
// moved would mean a conversion at some call site was a numeric change rather
// than a reinterpretation, and every previously issued piece of evidence would
// stop verifying.
func TestOrderedRulesInt64TransportMovedNoDigest(t *testing.T) {
	ev := orInt64WireFixture(t)
	// An unread refusal carries EMPTY digests, so pinning strings without
	// checking the run reached them would pass on four empty values.
	if ev.Status != predictioneval.StatusWouldAttempt {
		t.Fatalf("the fixture refused with %q / %q, so the digests below would not be the "+
			"ones this case means to pin", ev.Status, ev.Reason)
	}
	for _, d := range []struct{ name, want, got string }{
		{"StreamDigest", "01e34dc2bd7b0615a693fec7a30b99d04ba8232139805a7a91a95def152ba52d", ev.StreamDigest},
		{"ConfigDigest", "2e754165964654803883c5d512ce4e56c64dd7cedbb3307372d1e1d31ab9bf07", ev.ConfigDigest},
		{"EntropyDigest", "cd65d7000db8715ea48ffdb698c1c2b1b3dd80028845fe7e8920ce8b088b591c", ev.EntropyDigest},
		{"ConsumedInputDigest", "fd5e601a2bfb4b6d884158f12d07898ed4547fd49fa99b9a0d297127bfcb57b0", ev.ConsumedInputDigest},
	} {
		if len(d.got) != 64 {
			t.Fatalf("%s is %q, which is not a SHA-256; pinning it would be vacuous", d.name, d.got)
		}
		if d.got != d.want {
			t.Fatalf("%s is %s, but was %s before the int64 transport change. Changing how a "+
				"quantity is WRITTEN must not change what is HASHED", d.name, d.got, d.want)
		}
	}
	// The two values that made this fixture worth capturing, pinned so a future
	// edit cannot quietly shrink it back inside the exact-integer range.
	if ev.Selected == nil || int64(ev.Selected.CandidatePosition) != int64(1)<<53+1 {
		t.Fatalf("the fixture's selected position is not past 2^53: %+v", ev.Selected)
	}
	if len(ev.Visits) == 0 || int64(ev.Visits[0].PoolTotal) != int64(1)<<53+2 {
		t.Fatalf("the fixture's pool total is not past 2^53: %v", ev.Visits)
	}
}
