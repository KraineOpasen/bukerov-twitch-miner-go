package p4offline_test

// THE ENTROPY ALGORITHM, against an INDEPENDENT oracle.
//
// p4-hmac-sha256-u64/v1 is pinned by vectors that were produced by a separate
// implementation (testdata/synthetic/gen_entropy_vectors.py, Python's
// standard-library hmac/hashlib/struct) and cross-checked against openssl —
// see testdata/synthetic/PROVENANCE.md. No expectation below was derived from
// the Go code under test.

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
	Key           string `json:"key"`
	Trajectory    uint32 `json:"trajectory"`
	SourceRoundID string `json:"sourceRoundId"`
	PolicyID      string `json:"policyId"`
	Index         uint64 `json:"index"`
	MessageHex    string `json:"messageHex"`
	MacHex        string `json:"macHex"`
	Word          string `json:"word"`
}

type entropySequence struct {
	Key           string   `json:"key"`
	Trajectory    uint32   `json:"trajectory"`
	SourceRoundID string   `json:"sourceRoundId"`
	PolicyID      string   `json:"policyId"`
	Words         []string `json:"words"`
}

type entropyVectorFile struct {
	Algorithm string            `json:"algorithm"`
	Protocol  string            `json:"protocol"`
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
	if f.Algorithm != p4offline.EntropyAlgorithmVersion || f.Protocol != p4offline.ProtocolVersion {
		t.Fatalf("the vectors were generated for %q/%q, this package is %q/%q",
			f.Algorithm, f.Protocol, p4offline.EntropyAlgorithmVersion, p4offline.ProtocolVersion)
	}
	return f
}

func coordsOf(key string, traj uint32, round, policy string) p4offline.EntropyCoordinates {
	return p4offline.EntropyCoordinates{Trajectory: traj, SourceRoundID: round, PolicyID: policy}
}

// TestEntropyMessageFramingMatchesIndependentVectors pins the exact bytes the
// MAC is computed over: u64 big-endian UTF-8 BYTE lengths, six parts, in
// order. The multi-byte vectors are what separate a byte length from a rune
// count.
func TestEntropyMessageFramingMatchesIndependentVectors(t *testing.T) {
	f := loadEntropyVectors(t)
	for _, v := range f.Vectors {
		got := hex.EncodeToString(p4offline.EntropyMessage(
			coordsOf(v.Key, v.Trajectory, v.SourceRoundID, v.PolicyID), v.Index))
		if got != v.MessageHex {
			t.Errorf("message for t=%d round=%q policy=%q k=%d:\n got  %s\n want %s",
				v.Trajectory, v.SourceRoundID, v.PolicyID, v.Index, got, v.MessageHex)
		}
	}
}

// TestEntropyWordsMatchIndependentVectors pins the word: HMAC-SHA256 under the
// UTF-8 key, first eight bytes, big-endian, rendered as sixteen lower-case
// hex digits.
func TestEntropyWordsMatchIndependentVectors(t *testing.T) {
	f := loadEntropyVectors(t)
	for _, v := range f.Vectors {
		want, err := strconv.ParseUint(v.Word, 16, 64)
		if err != nil {
			t.Fatalf("vector word %q is not hex: %v", v.Word, err)
		}
		got, err := p4offline.EntropyWord(v.Key, coordsOf(v.Key, v.Trajectory, v.SourceRoundID, v.PolicyID), v.Index)
		if err != nil {
			t.Fatalf("EntropyWord: %v", err)
		}
		if got != want {
			t.Errorf("word for t=%d round=%q policy=%q k=%d: got %016x, want %s (mac %s)",
				v.Trajectory, v.SourceRoundID, v.PolicyID, v.Index, got, v.Word, v.MacHex)
		}
		// The hex rendering is the P3b wire form, and it must be the same
		// sixteen digits the oracle wrote.
		words, err := p4offline.GenerateEntropyWords(v.Key,
			coordsOf(v.Key, v.Trajectory, v.SourceRoundID, v.PolicyID), int(v.Index)+1)
		if err != nil {
			t.Fatalf("GenerateEntropyWords: %v", err)
		}
		if len(words) != int(v.Index)+1 {
			t.Fatalf("asked for %d words, got %d", v.Index+1, len(words))
		}
		text, _ := words[v.Index].MarshalText()
		if string(text) != v.Word {
			t.Errorf("rendered word %s, want %s", text, v.Word)
		}
	}
}

// TestEntropySequencesAreSequentialFromZero pins that a run's words are the
// per-index words in order, starting at zero, and that word k does not depend
// on how many words were requested — the sequence is a prefix property.
func TestEntropySequencesAreSequentialFromZero(t *testing.T) {
	f := loadEntropyVectors(t)
	for _, s := range f.Sequences {
		coords := coordsOf(s.Key, s.Trajectory, s.SourceRoundID, s.PolicyID)
		got, err := p4offline.GenerateEntropyWords(s.Key, coords, len(s.Words))
		if err != nil {
			t.Fatalf("GenerateEntropyWords: %v", err)
		}
		if len(got) != len(s.Words) {
			t.Fatalf("got %d words, want %d", len(got), len(s.Words))
		}
		for i := range s.Words {
			text, _ := got[i].MarshalText()
			if string(text) != s.Words[i] {
				t.Errorf("word %d: got %s, want %s", i, text, s.Words[i])
			}
		}
		shorter, err := p4offline.GenerateEntropyWords(s.Key, coords, 1)
		if err != nil || len(shorter) != 1 || shorter[0] != got[0] {
			t.Errorf("word 0 changed when fewer words were requested: %v / %v (err %v)", shorter, got[:1], err)
		}
	}
	// Zero words is a legitimate request: a policy that draws nothing.
	none, err := p4offline.GenerateEntropyWords("k", coordsOf("k", 0, "r", "p"), 0)
	if err != nil || len(none) != 0 {
		t.Fatalf("zero words: %v, err %v", none, err)
	}
}

// TestEntropyRefusesOutOfProtocolInputs pins the closed domain: exactly
// TrajectoryCount trajectories, non-empty key and identities, and a word count
// inside the P3b trace ceiling. There is no global or random fallback.
func TestEntropyRefusesOutOfProtocolInputs(t *testing.T) {
	ok := coordsOf("k", 0, "round", "policy")
	cases := []struct {
		name   string
		key    string
		coords p4offline.EntropyCoordinates
		count  int
		want   error
	}{
		{"trajectory at the count is out of range", "k", coordsOf("k", p4offline.TrajectoryCount, "round", "policy"), 1, p4offline.ErrEntropyCoordinates},
		{"empty key", "", ok, 1, p4offline.ErrEntropyKey},
		{"empty round", "k", coordsOf("k", 0, "", "policy"), 1, p4offline.ErrEntropyCoordinates},
		{"empty policy", "k", coordsOf("k", 0, "round", ""), 1, p4offline.ErrEntropyCoordinates},
		{"negative count", "k", ok, -1, p4offline.ErrEntropyCount},
		{"count past the trace ceiling", "k", ok, predictioneval.MaxOrderedRulesDrawWords + 1, p4offline.ErrEntropyCount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			words, err := p4offline.GenerateEntropyWords(tc.key, tc.coords, tc.count)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got err %v (words %v), want %v", err, words, tc.want)
			}
			if words != nil {
				t.Fatalf("a refusal must return no words, got %v", words)
			}
			if _, err := p4offline.BuildDrawTrace(tc.key, tc.coords, tc.count); !errors.Is(err, tc.want) {
				t.Fatalf("BuildDrawTrace: got err %v, want %v", err, tc.want)
			}
		})
	}
	// The last trajectory is inside the protocol.
	if _, err := p4offline.GenerateEntropyWords("k", coordsOf("k", p4offline.TrajectoryCount-1, "round", "policy"), 1); err != nil {
		t.Fatalf("trajectory %d must be admitted: %v", p4offline.TrajectoryCount-1, err)
	}
}

// TestEntropyValidationDetectsTamperedWords pins validation as a full
// recomputation: a single flipped word, a word from other coordinates or a
// truncated/extended list is refused by index, and the genuine list passes.
func TestEntropyValidationDetectsTamperedWords(t *testing.T) {
	coords := coordsOf("k", 3, "round", "policy")
	words, err := p4offline.GenerateEntropyWords("secret", coords, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(words) != 4 {
		t.Fatalf("generated %d words, want 4", len(words))
	}
	if err := p4offline.ValidateEntropyWords("secret", coords, words); err != nil {
		t.Fatalf("the genuine list must validate: %v", err)
	}
	tampered := append([]predictioneval.OrderedRulesHex64(nil), words...)
	tampered[2] ^= 1
	err = p4offline.ValidateEntropyWords("secret", coords, tampered)
	if !errors.Is(err, p4offline.ErrEntropyWordMismatch) || !strings.Contains(err.Error(), "index 2") {
		t.Fatalf("a flipped low bit at index 2 must be refused by index: %v", err)
	}
	other := coordsOf("k", 4, "round", "policy")
	if err := p4offline.ValidateEntropyWords("secret", other, words); !errors.Is(err, p4offline.ErrEntropyWordMismatch) {
		t.Fatalf("words from another trajectory must not validate: %v", err)
	}
	if err := p4offline.ValidateEntropyWords("other-key", coords, words); !errors.Is(err, p4offline.ErrEntropyWordMismatch) {
		t.Fatalf("words under another key must not validate: %v", err)
	}
}

// TestBuildDrawTraceDeclaresTheP3bSemanticsAndBindsTheCoordinates pins the
// P3b-facing shape: the trace declares rand-0.8.5 Bernoulli semantics (the
// only semantics the core consumes), its run identity names the coordinates,
// and its words are exactly the generated sequence.
func TestBuildDrawTraceDeclaresTheP3bSemanticsAndBindsTheCoordinates(t *testing.T) {
	coords := coordsOf("k", 12, "event-round-0009", "ruleset-alpha")
	trace, err := p4offline.BuildDrawTrace("secret", coords, 3)
	if err != nil {
		t.Fatal(err)
	}
	if trace.EntropySemanticsVersion != predictioneval.OrderedRulesEntropySemanticsVersion {
		t.Fatalf("trace declares %q; the P3b core consumes only %q",
			trace.EntropySemanticsVersion, predictioneval.OrderedRulesEntropySemanticsVersion)
	}
	for _, part := range []string{p4offline.EntropyAlgorithmVersion, "12", "event-round-0009", "ruleset-alpha"} {
		if !strings.Contains(trace.RunID, part) {
			t.Errorf("RunID %q does not name %q", trace.RunID, part)
		}
	}
	want, _ := p4offline.GenerateEntropyWords("secret", coords, 3)
	if len(trace.Words) != 3 || trace.Words[0] != want[0] || trace.Words[1] != want[1] || trace.Words[2] != want[2] {
		t.Fatalf("trace words %v, want %v", trace.Words, want)
	}
	if err := p4offline.ValidateDrawTrace("secret", coords, trace); err != nil {
		t.Fatalf("the built trace must validate: %v", err)
	}
	wrongSemantics := trace
	wrongSemantics.EntropySemanticsVersion = "something-else/v9"
	if err := p4offline.ValidateDrawTrace("secret", coords, wrongSemantics); !errors.Is(err, p4offline.ErrEntropyTrace) {
		t.Fatalf("a trace declaring other semantics must be refused: %v", err)
	}
	wrongRun := trace
	wrongRun.RunID = "forged"
	if err := p4offline.ValidateDrawTrace("secret", coords, wrongRun); !errors.Is(err, p4offline.ErrEntropyTrace) {
		t.Fatalf("a trace whose run identity does not name the coordinates must be refused: %v", err)
	}
	// Two coordinate sets whose delimiter-joined text would coincide name
	// different runs: the identity quotes its components.
	a, _ := p4offline.BuildDrawTrace("secret", coordsOf("k", 0, "r:policy=p", "x"), 1)
	b, _ := p4offline.BuildDrawTrace("secret", coordsOf("k", 0, "r", "p:policy=x"), 1)
	if a.RunID == b.RunID {
		t.Fatalf("run identities collide: %q", a.RunID)
	}
	if err := p4offline.ValidateDrawTrace("secret", coordsOf("k", 0, "r", "p:policy=x"), a); !errors.Is(err, p4offline.ErrEntropyTrace) {
		t.Fatalf("a trace of other coordinates must be refused by identity before any word is read: %v", err)
	}
}

// TestEntropyIdentitiesAreBounded pins the identifier ceiling: an identity
// or a key past the core's identifier bound is refused, at it is admitted.
func TestEntropyIdentitiesAreBounded(t *testing.T) {
	long := strings.Repeat("x", predictioneval.MaxOrderedRulesIdentifierBytes)
	if _, err := p4offline.GenerateEntropyWords("k", coordsOf("k", 0, long, "p"), 1); err != nil {
		t.Fatalf("an identity at the bound is admitted: %v", err)
	}
	if _, err := p4offline.GenerateEntropyWords("k", coordsOf("k", 0, long+"x", "p"), 1); !errors.Is(err, p4offline.ErrEntropyCoordinates) {
		t.Fatalf("got %v", err)
	}
	if _, err := p4offline.GenerateEntropyWords("k", coordsOf("k", 0, "r", long+"x"), 1); !errors.Is(err, p4offline.ErrEntropyCoordinates) {
		t.Fatalf("got %v", err)
	}
	if _, err := p4offline.GenerateEntropyWords(long+"x", coordsOf("k", 0, "r", "p"), 1); !errors.Is(err, p4offline.ErrEntropyKey) {
		t.Fatalf("got %v", err)
	}
}
