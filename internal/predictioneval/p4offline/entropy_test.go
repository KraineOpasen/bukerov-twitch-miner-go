package p4offline_test

// THE ENTROPY ALGORITHM, against a SUPPLEMENTAL independent oracle.
//
// The primary oracle is the owner-supplied Work vector set (entropy_work_
// test.go, testdata/synthetic/work/). This file adds sequences, more indices
// and more multi-byte identities, produced by a separate implementation
// (testdata/synthetic/gen_entropy_vectors.py, Python's standard-library
// hmac/hashlib/struct) written from the approved algorithm and cross-checked
// against openssl — see testdata/synthetic/PROVENANCE.md. No expectation
// below was derived from the Go code under test.

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

type entropyVector struct {
	DatasetID           string `json:"datasetId"`
	DatasetVersion      string `json:"datasetVersion"`
	CommonFactsetDigest string `json:"commonFactsetDigest"`
	PairedOpportunityID string `json:"pairedOpportunityId"`
	Trajectory          uint32 `json:"trajectory"`
	Index               uint64 `json:"index"`
	MessageHex          string `json:"messageHex"`
	MacHex              string `json:"macHex"`
	Word                string `json:"word"`
}

type entropySequence struct {
	DatasetID           string   `json:"datasetId"`
	DatasetVersion      string   `json:"datasetVersion"`
	CommonFactsetDigest string   `json:"commonFactsetDigest"`
	PairedOpportunityID string   `json:"pairedOpportunityId"`
	Trajectory          uint32   `json:"trajectory"`
	Words               []string `json:"words"`
}

type entropyVectorFile struct {
	Algorithm string            `json:"algorithm"`
	SeedHex   string            `json:"seedHex"`
	Vectors   []entropyVector   `json:"vectors"`
	Sequences []entropySequence `json:"sequences"`
}

func loadEntropyVectors(t *testing.T) entropyVectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/synthetic/entropy_vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var f entropyVectorFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	if len(f.Vectors) < 10 || len(f.Sequences) < 2 {
		t.Fatalf("the vector file is too thin to be evidence: %d vectors, %d sequences",
			len(f.Vectors), len(f.Sequences))
	}
	if f.Algorithm != p4offline.EntropyAlgorithmVersion {
		t.Fatalf("the vectors were generated for %q, this package is %q", f.Algorithm, p4offline.EntropyAlgorithmVersion)
	}
	if seed := p4offline.EntropySeed(); hex.EncodeToString(seed[:]) != f.SeedHex {
		t.Fatalf("the vectors were generated under seed %s, this package draws under %x", f.SeedHex, seed)
	}
	return f
}

func (v entropyVector) coords() p4offline.EntropyCoordinates {
	return p4offline.EntropyCoordinates{DatasetID: v.DatasetID, DatasetVersion: v.DatasetVersion,
		CommonFactsetDigest: v.CommonFactsetDigest, PairedOpportunityID: v.PairedOpportunityID, Trajectory: v.Trajectory}
}

func (s entropySequence) coords() p4offline.EntropyCoordinates {
	return p4offline.EntropyCoordinates{DatasetID: s.DatasetID, DatasetVersion: s.DatasetVersion,
		CommonFactsetDigest: s.CommonFactsetDigest, PairedOpportunityID: s.PairedOpportunityID, Trajectory: s.Trajectory}
}

// okCoords is a set of coordinates inside the protocol, for the refusal
// tests: a syntactically valid digest reference and short identities.
func okCoords(trajectory uint32) p4offline.EntropyCoordinates {
	return p4offline.EntropyCoordinates{DatasetID: "d", DatasetVersion: "v1",
		CommonFactsetDigest: p4offline.DigestReference(strings.Repeat("ab", 32)), PairedOpportunityID: "round", Trajectory: trajectory}
}

// TestEntropyMessageFramingMatchesIndependentVectors pins the exact bytes the
// MAC is computed over: u64 big-endian UTF-8 BYTE lengths, nine parts, in
// order. The multi-byte vectors are what separate a byte length from a rune
// count.
func TestEntropyMessageFramingMatchesIndependentVectors(t *testing.T) {
	f := loadEntropyVectors(t)
	for _, v := range f.Vectors {
		got := hex.EncodeToString(p4offline.EntropyMessage(v.coords(), v.Index))
		if got != v.MessageHex {
			t.Errorf("message for run=%d opportunity=%q k=%d:\n got  %s\n want %s",
				v.Trajectory, v.PairedOpportunityID, v.Index, got, v.MessageHex)
		}
	}
}

// TestEntropyWordsMatchIndependentVectors pins the MAC and the word: HMAC-
// SHA256 under the raw seed, first eight bytes, big-endian, rendered as
// sixteen lower-case hex digits.
func TestEntropyWordsMatchIndependentVectors(t *testing.T) {
	f := loadEntropyVectors(t)
	for _, v := range f.Vectors {
		want, err := strconv.ParseUint(v.Word, 16, 64)
		if err != nil {
			t.Fatalf("vector word %q is not hex: %v", v.Word, err)
		}
		mac, err := p4offline.EntropyMAC(v.coords(), v.Index)
		if err != nil {
			t.Fatalf("EntropyMAC: %v", err)
		}
		if got := hex.EncodeToString(mac[:]); got != v.MacHex {
			t.Errorf("mac for run=%d opportunity=%q k=%d: got %s, want %s", v.Trajectory, v.PairedOpportunityID, v.Index, got, v.MacHex)
		}
		got, err := p4offline.EntropyWord(v.coords(), v.Index)
		if err != nil {
			t.Fatalf("EntropyWord: %v", err)
		}
		if got != want {
			t.Errorf("word for run=%d opportunity=%q k=%d: got %016x, want %s (mac %s)",
				v.Trajectory, v.PairedOpportunityID, v.Index, got, v.Word, v.MacHex)
		}
		// The hex rendering is the P3b wire form, and it must be the same
		// sixteen digits the oracle wrote.
		words, err := p4offline.GenerateEntropyWords(v.coords(), int(v.Index)+1)
		if err != nil {
			t.Fatalf("GenerateEntropyWords: %v", err)
		}
		if len(words) != int(v.Index)+1 {
			t.Fatalf("asked for %d words, got %d", v.Index+1, len(words))
		}
		text := mustHex16(t, words[v.Index])
		if string(text) != v.Word {
			t.Errorf("rendered word %s, want %s", text, v.Word)
		}
	}
}

// mustHex16 renders a word or fails the test. On the pinned implementation
// OrderedRulesHex64.MarshalText returns a literal nil error, so this cannot
// mask a real failure today; it is written this way so the tests below never
// read a rendering whose error was discarded, and so a future implementation
// that can fail is caught at the site instead of silently comparing "".
func mustHex16(t *testing.T, w predictioneval.OrderedRulesHex64) []byte {
	t.Helper()
	text, err := w.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText(%d): %v", uint64(w), err)
	}
	return text
}

// TestEntropySequencesAreSequentialFromZero pins that a run's words are the
// per-index words in order, starting at zero, and that word k does not depend
// on how many words were requested — the sequence is a prefix property.
func TestEntropySequencesAreSequentialFromZero(t *testing.T) {
	f := loadEntropyVectors(t)
	for _, s := range f.Sequences {
		got, err := p4offline.GenerateEntropyWords(s.coords(), len(s.Words))
		if err != nil {
			t.Fatalf("GenerateEntropyWords: %v", err)
		}
		if len(got) != len(s.Words) {
			t.Fatalf("got %d words, want %d", len(got), len(s.Words))
		}
		for i := range s.Words {
			text := mustHex16(t, got[i])
			if string(text) != s.Words[i] {
				t.Errorf("word %d: got %s, want %s", i, text, s.Words[i])
			}
		}
		shorter, err := p4offline.GenerateEntropyWords(s.coords(), 1)
		if err != nil || len(shorter) != 1 || shorter[0] != got[0] {
			t.Errorf("word 0 changed when fewer words were requested: %v / %v (err %v)", shorter, got[:1], err)
		}
	}
	// Zero words is a legitimate request: a policy that draws nothing.
	none, err := p4offline.GenerateEntropyWords(okCoords(0), 0)
	if err != nil || len(none) != 0 {
		t.Fatalf("zero words: %v, err %v", none, err)
	}
}

// TestEntropyRefusesOutOfProtocolInputs pins the closed domain: exactly
// TrajectoryCount trajectories, non-empty identities, a digest spelled as a
// "sha256:" reference to 64 lower-case hex digits, and a word count inside
// the P3b trace ceiling. There is no global or random fallback.
func TestEntropyRefusesOutOfProtocolInputs(t *testing.T) {
	with := func(edit func(*p4offline.EntropyCoordinates)) p4offline.EntropyCoordinates {
		c := okCoords(0)
		edit(&c)
		return c
	}
	cases := []struct {
		name   string
		coords p4offline.EntropyCoordinates
		count  int
		want   error
	}{
		{"trajectory at the count is out of range", okCoords(p4offline.TrajectoryCount), 1, p4offline.ErrEntropyCoordinates},
		{"empty dataset id", with(func(c *p4offline.EntropyCoordinates) { c.DatasetID = "" }), 1, p4offline.ErrEntropyCoordinates},
		{"empty dataset version", with(func(c *p4offline.EntropyCoordinates) { c.DatasetVersion = "" }), 1, p4offline.ErrEntropyCoordinates},
		{"empty opportunity", with(func(c *p4offline.EntropyCoordinates) { c.PairedOpportunityID = "" }), 1, p4offline.ErrEntropyCoordinates},
		{"bare hex digest without the sha256: prefix", with(func(c *p4offline.EntropyCoordinates) { c.CommonFactsetDigest = strings.Repeat("ab", 32) }), 1, p4offline.ErrEntropyCoordinates},
		{"another prefix of the same length", with(func(c *p4offline.EntropyCoordinates) { c.CommonFactsetDigest = "SHA256:" + strings.Repeat("ab", 32) }), 1, p4offline.ErrEntropyCoordinates},
		{"upper-case digest", with(func(c *p4offline.EntropyCoordinates) {
			c.CommonFactsetDigest = p4offline.DigestReference(strings.Repeat("AB", 32))
		}), 1, p4offline.ErrEntropyCoordinates},
		{"short digest", with(func(c *p4offline.EntropyCoordinates) {
			c.CommonFactsetDigest = p4offline.DigestReference(strings.Repeat("ab", 31))
		}), 1, p4offline.ErrEntropyCoordinates},
		{"empty digest reference", with(func(c *p4offline.EntropyCoordinates) { c.CommonFactsetDigest = p4offline.DigestReferencePrefix }), 1, p4offline.ErrEntropyCoordinates},
		{"negative count", okCoords(0), -1, p4offline.ErrEntropyCount},
		{"count past the trace ceiling", okCoords(0), predictioneval.MaxOrderedRulesDrawWords + 1, p4offline.ErrEntropyCount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			words, err := p4offline.GenerateEntropyWords(tc.coords, tc.count)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got err %v (words %v), want %v", err, words, tc.want)
			}
			if words != nil {
				t.Fatalf("a refusal must return no words, got %v", words)
			}
			if _, err := p4offline.BuildDrawTrace(tc.coords, tc.count); !errors.Is(err, tc.want) {
				t.Fatalf("BuildDrawTrace: got err %v, want %v", err, tc.want)
			}
			if tc.want == p4offline.ErrEntropyCoordinates {
				if _, err := p4offline.EntropyWord(tc.coords, 0); !errors.Is(err, tc.want) {
					t.Fatalf("EntropyWord: got err %v, want %v", err, tc.want)
				}
				if _, err := p4offline.EntropyMAC(tc.coords, 0); !errors.Is(err, tc.want) {
					t.Fatalf("EntropyMAC: got err %v, want %v", err, tc.want)
				}
			}
		})
	}
	// The last trajectory is inside the protocol.
	if _, err := p4offline.GenerateEntropyWords(okCoords(p4offline.TrajectoryCount-1), 1); err != nil {
		t.Fatalf("trajectory %d must be admitted: %v", p4offline.TrajectoryCount-1, err)
	}
}

// TestEntropyValidationDetectsTamperedWords pins validation as a full
// recomputation: a single flipped word, a word from other coordinates or a
// truncated/extended list is refused by index, and the genuine list passes.
func TestEntropyValidationDetectsTamperedWords(t *testing.T) {
	coords := okCoords(3)
	words, err := p4offline.GenerateEntropyWords(coords, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(words) != 4 {
		t.Fatalf("generated %d words, want 4", len(words))
	}
	if err := p4offline.ValidateEntropyWords(coords, words); err != nil {
		t.Fatalf("the genuine list must validate: %v", err)
	}
	tampered := append([]predictioneval.OrderedRulesHex64(nil), words...)
	tampered[2] ^= 1
	err = p4offline.ValidateEntropyWords(coords, tampered)
	if !errors.Is(err, p4offline.ErrEntropyWordMismatch) || !strings.Contains(err.Error(), "index 2") {
		t.Fatalf("a flipped low bit at index 2 must be refused by index: %v", err)
	}
	if err := p4offline.ValidateEntropyWords(okCoords(4), words); !errors.Is(err, p4offline.ErrEntropyWordMismatch) {
		t.Fatalf("words from another trajectory must not validate: %v", err)
	}
	otherVersion := coords
	otherVersion.DatasetVersion = "v2"
	if err := p4offline.ValidateEntropyWords(otherVersion, words); !errors.Is(err, p4offline.ErrEntropyWordMismatch) {
		t.Fatalf("words under another dataset version must not validate: %v", err)
	}
	otherDigest := coords
	otherDigest.CommonFactsetDigest = p4offline.DigestReference(strings.Repeat("cd", 32))
	if err := p4offline.ValidateEntropyWords(otherDigest, words); !errors.Is(err, p4offline.ErrEntropyWordMismatch) {
		t.Fatalf("words under another factset must not validate: %v", err)
	}
}

// TestBuildDrawTraceDeclaresTheP3bSemanticsAndBindsTheCoordinates pins the
// P3b-facing shape: the trace declares rand-0.8.5 Bernoulli semantics (the
// only semantics the core consumes), its run identity names the coordinates,
// and its words are exactly the generated sequence.
func TestBuildDrawTraceDeclaresTheP3bSemanticsAndBindsTheCoordinates(t *testing.T) {
	coords := okCoords(12)
	coords.PairedOpportunityID = "event-round-0009"
	trace, err := p4offline.BuildDrawTrace(coords, 3)
	if err != nil {
		t.Fatal(err)
	}
	if trace.EntropySemanticsVersion != predictioneval.OrderedRulesEntropySemanticsVersion {
		t.Fatalf("trace declares %q; the P3b core consumes only %q",
			trace.EntropySemanticsVersion, predictioneval.OrderedRulesEntropySemanticsVersion)
	}
	for _, part := range []string{p4offline.EntropyAlgorithmVersion, "run=12", "event-round-0009", coords.DatasetID,
		coords.DatasetVersion, coords.CommonFactsetDigest, p4offline.EntropyPolicyLabel, p4offline.EntropyDrawKind, "words=3"} {
		if !strings.Contains(trace.RunID, part) {
			t.Errorf("RunID %q does not name %q", trace.RunID, part)
		}
	}
	want, err := p4offline.GenerateEntropyWords(coords, 3)
	if err != nil {
		t.Fatalf("GenerateEntropyWords: %v", err)
	}
	if len(trace.Words) != 3 || trace.Words[0] != want[0] || trace.Words[1] != want[1] || trace.Words[2] != want[2] {
		t.Fatalf("trace words %v, want %v", trace.Words, want)
	}
	if err := p4offline.ValidateDrawTrace(coords, trace); err != nil {
		t.Fatalf("the built trace must validate: %v", err)
	}
	wrongSemantics := trace
	wrongSemantics.EntropySemanticsVersion = "something-else/v9"
	if err := p4offline.ValidateDrawTrace(coords, wrongSemantics); !errors.Is(err, p4offline.ErrEntropyTrace) {
		t.Fatalf("a trace declaring other semantics must be refused: %v", err)
	}
	wrongRun := trace
	wrongRun.RunID = "forged"
	if err := p4offline.ValidateDrawTrace(coords, wrongRun); !errors.Is(err, p4offline.ErrEntropyTrace) {
		t.Fatalf("a trace whose run identity does not name the coordinates must be refused: %v", err)
	}
	// Two coordinate sets whose delimiter-joined text would coincide name
	// different runs: the identity quotes its components.
	a := okCoords(0)
	a.DatasetID, a.DatasetVersion = `d":version="v1`, "x"
	b := okCoords(0)
	b.DatasetID, b.DatasetVersion = "d", `v1":version="x`
	ta := mustDrawTrace(t, a, 1)
	tb := mustDrawTrace(t, b, 1)
	if ta.RunID == tb.RunID {
		t.Fatalf("run identities collide: %q", ta.RunID)
	}
	if err := p4offline.ValidateDrawTrace(b, ta); !errors.Is(err, p4offline.ErrEntropyTrace) {
		t.Fatalf("a trace of other coordinates must be refused by identity before any word is read: %v", err)
	}
}

// TestEntropyIdentitiesAreBounded pins the identifier ceiling: an identity
// past the core's identifier bound is refused, at it is admitted.
func TestEntropyIdentitiesAreBounded(t *testing.T) {
	long := strings.Repeat("x", predictioneval.MaxOrderedRulesIdentifierBytes)
	for name, edit := range map[string]func(*p4offline.EntropyCoordinates, string){
		"dataset id":      func(c *p4offline.EntropyCoordinates, v string) { c.DatasetID = v },
		"dataset version": func(c *p4offline.EntropyCoordinates, v string) { c.DatasetVersion = v },
		"opportunity":     func(c *p4offline.EntropyCoordinates, v string) { c.PairedOpportunityID = v },
	} {
		at := okCoords(0)
		edit(&at, long)
		if _, err := p4offline.GenerateEntropyWords(at, 1); err != nil {
			t.Fatalf("%s at the bound is admitted: %v", name, err)
		}
		past := okCoords(0)
		edit(&past, long+"x")
		if _, err := p4offline.GenerateEntropyWords(past, 1); !errors.Is(err, p4offline.ErrEntropyCoordinates) {
			t.Fatalf("%s past the bound: got %v", name, err)
		}
	}
}

// TestEntropyIdentitiesMustBeEncodable pins that a coordinate identity the
// package's own declared encoding cannot carry is refused before it is MACed.
//
// The three identity fields were bounded for emptiness and byte length only.
// Go's JSON encoder replaces an invalid UTF-8 sequence with U+FFFD, and every
// exported field of a P3bCaseResult carries a JSON tag, so coordinates holding
// invalid UTF-8 are MACed over bytes their own declared encoding does not
// preserve: the stored form names a different identity than the one that drew.
// Two distinct invalid sequences also collapse onto the same encoded string, so
// the damage is not only that a result cannot be re-derived -- distinct
// identities stop being distinguishable in the representation the type
// declares. Refused at the coordinate gate, which is the one place every
// drawing function already passes through.
func TestEntropyIdentitiesMustBeEncodable(t *testing.T) {
	_, fs := selectedFactset(t, nil, nil)
	good := synthCoords(fs, 0)
	if _, err := p4offline.EntropyMAC(good, 0); err != nil {
		t.Fatalf("the honest coordinates must stay accepted: %v", err)
	}
	for _, tc := range []struct {
		name string
		edit func(*p4offline.EntropyCoordinates)
	}{
		{"dataset id", func(c *p4offline.EntropyCoordinates) { c.DatasetID = "d\xff" }},
		{"dataset version", func(c *p4offline.EntropyCoordinates) { c.DatasetVersion = "v\xff" }},
		{"paired opportunity id", func(c *p4offline.EntropyCoordinates) { c.PairedOpportunityID = "o\xff" }},
		{"a lone surrogate half", func(c *p4offline.EntropyCoordinates) { c.DatasetID = "d\xed\xa0\x80" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coords := good
			tc.edit(&coords)
			if _, err := p4offline.EntropyMAC(coords, 0); !errors.Is(err, p4offline.ErrEntropyCoordinates) {
				t.Fatalf("an identity the declared encoding cannot carry must be refused, got %v", err)
			}
			if _, err := p4offline.GenerateEntropyWords(coords, 1); !errors.Is(err, p4offline.ErrEntropyCoordinates) {
				t.Fatalf("the drawing path must refuse it too, got %v", err)
			}
		})
	}
	// The false-refusal control, over EVERY field the guard touches and BOTH
	// entry points -- an earlier version varied only the dataset id through
	// only the MAC, which left two of the three guarded fields with no
	// legitimate-input case at all. Multi-byte UTF-8 is legitimate: the oracle
	// vectors in this file already depend on it.
	for _, id := range []string{"датасет", "データ", "éte", "🎲"} {
		for name, set := range map[string]func(*p4offline.EntropyCoordinates){
			"dataset id":            func(c *p4offline.EntropyCoordinates) { c.DatasetID = id },
			"dataset version":       func(c *p4offline.EntropyCoordinates) { c.DatasetVersion = id },
			"paired opportunity id": func(c *p4offline.EntropyCoordinates) { c.PairedOpportunityID = id },
		} {
			coords := good
			set(&coords)
			if _, err := p4offline.EntropyMAC(coords, 0); err != nil {
				t.Fatalf("valid multi-byte %s %q must stay accepted by the MAC: %v", name, id, err)
			}
			if _, err := p4offline.GenerateEntropyWords(coords, 1); err != nil {
				t.Fatalf("valid multi-byte %s %q must stay accepted by the drawing path: %v", name, id, err)
			}
		}
	}
}

// TestDrawTraceRefusalDoesNotMaterializeSuppliedText is the sibling of
// TestRulesetIdentityRefusalDoesNotMaterializeSuppliedText in p3b_test.go, and
// it exists because that repair was applied at one gate and not at its
// siblings. ValidateDrawTrace reads two caller-supplied strings that NOTHING on
// this path bounds: checkEntropyCoordinates bounds the three COORDINATE
// identities, and checkEntropyCount bounds the word slice, but neither reaches
// the trace's own semantics version or run identity. Both gates quoted them
// before refusing.
//
// The budget is stated here and is not read from the code under test.
func TestDrawTraceRefusalDoesNotMaterializeSuppliedText(t *testing.T) {
	const budget = 1024
	big := strings.Repeat("a", 1<<20)

	for _, tc := range []struct {
		name  string
		trace predictioneval.SuppliedDrawTrace
	}{
		{"an over-long semantics version", predictioneval.SuppliedDrawTrace{
			EntropySemanticsVersion: big,
		}},
		{"an over-long run identity beside the right semantics", predictioneval.SuppliedDrawTrace{
			EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion,
			RunID:                   big,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := p4offline.ValidateDrawTrace(okCoords(0), tc.trace)
			if !errors.Is(err, p4offline.ErrEntropyTrace) {
				t.Fatalf("want ErrEntropyTrace, got %v", err)
			}
			if n := len(err.Error()); n > budget {
				t.Fatalf("the refusal carries %d bytes; a refusal must name the fault, not the input (budget %d)", n, budget)
			}
		})
	}

	t.Run("each refusal still names its own fault", func(t *testing.T) {
		// Declining to quote the values must not cost the diagnosis, and
		// "the two strings differ" does not establish that. A first version of
		// this subtest asserted only inequality, and an independent lane showed
		// two mutants surviving it: emitting the SEMANTICS message from the
		// run-identity gate (so a caller with a correct version and a wrong run
		// is told the wrong fault), and reporting len(trace.RunID) on BOTH
		// sides of the run-identity message (so it reads "is N bytes and is not
		// this run's, which is N bytes"). Both passed, because the two fixture
		// strings happen to differ in length. Content is asserted now.
		const foreignSemantics = "not-the-core's"
		const foreignRun = "not-this-run's"
		semantics := predictioneval.SuppliedDrawTrace{EntropySemanticsVersion: foreignSemantics}
		runID := predictioneval.SuppliedDrawTrace{
			EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion,
			RunID:                   foreignRun,
		}
		e1 := p4offline.ValidateDrawTrace(okCoords(0), semantics)
		e2 := p4offline.ValidateDrawTrace(okCoords(0), runID)
		if !errors.Is(e1, p4offline.ErrEntropyTrace) || !errors.Is(e2, p4offline.ErrEntropyTrace) {
			t.Fatalf("both must be trace faults: %v / %v", e1, e2)
		}
		// The semantics arm names itself, the supplied LENGTH, and the constant
		// the core consumes -- which is quoted because it is a constant.
		if !strings.Contains(e1.Error(), "declares semantics of "+strconv.Itoa(len(foreignSemantics))+" bytes") ||
			!strings.Contains(e1.Error(), strconv.Quote(predictioneval.OrderedRulesEntropySemanticsVersion)) {
			t.Fatalf("the semantics fault must name itself, the supplied length and the core's constant: %v", e1)
		}
		// The run-identity arm names itself and TWO DIFFERENT lengths: the one
		// supplied and the one this run actually has. Equal counts would be the
		// self-contradictory message the second mutant produced.
		if !strings.Contains(e2.Error(), "run identity is "+strconv.Itoa(len(foreignRun))+" bytes") {
			t.Fatalf("the run-identity fault must name itself and the supplied length: %v", e2)
		}
		if strings.Contains(e2.Error(), "is not this run's, which is "+strconv.Itoa(len(foreignRun))+" bytes") {
			t.Fatalf("both lengths are the supplied one; the refusal contradicts itself: %v", e2)
		}
		// And neither re-exports the text it refused.
		if strings.Contains(e1.Error(), foreignSemantics) || strings.Contains(e2.Error(), foreignRun) {
			t.Fatalf("a refusal must not carry the text it is refusing: %v / %v", e1, e2)
		}
		if e1.Error() == e2.Error() {
			t.Fatalf("the two faults must be distinguishable: %v / %v", e1, e2)
		}
	})
}

// TestValidateEntropyWordsDoesNotBindTheCount pins the documented gap between
// the two validators, so the weaker one cannot quietly be read as the stronger.
// ValidateEntropyWords validates the words SUPPLIED; only ValidateDrawTrace
// binds how many there should be, through the run identity.
func TestValidateEntropyWordsDoesNotBindTheCount(t *testing.T) {
	coords := okCoords(0)
	words, err := p4offline.GenerateEntropyWords(coords, 16)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1, 8, 15, 16} {
		if err := p4offline.ValidateEntropyWords(coords, words[:n]); err != nil {
			t.Fatalf("a correct prefix of %d words is accepted, by design: %v", n, err)
		}
	}
	if err := p4offline.ValidateEntropyWords(coords, nil); err != nil {
		t.Fatalf("a nil list is accepted, by design: %v", err)
	}

	t.Run("the whole-schedule validator does bind it", func(t *testing.T) {
		// The trace comes from the package's own exported builder, so the run
		// identity is not restated here: the production API is not widened to
		// let a test read an internal value.
		full, err := p4offline.BuildDrawTrace(coords, 16)
		if err != nil {
			t.Fatal(err)
		}
		if err := p4offline.ValidateDrawTrace(coords, full); err != nil {
			t.Fatalf("the whole schedule must validate: %v", err)
		}
		truncated := full
		truncated.Words = full.Words[:8]
		if err := p4offline.ValidateDrawTrace(coords, truncated); !errors.Is(err, p4offline.ErrEntropyTrace) {
			t.Fatalf("a truncated schedule must be refused as a run-identity fault, got %v", err)
		}
		// And the weaker validator accepts that very same truncation, which is
		// the whole point of the two existing side by side.
		if err := p4offline.ValidateEntropyWords(coords, truncated.Words); err != nil {
			t.Fatalf("the word validator accepts a correct prefix, by design: %v", err)
		}
	})
}
