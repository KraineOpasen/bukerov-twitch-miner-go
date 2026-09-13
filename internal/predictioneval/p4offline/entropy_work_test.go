package p4offline_test

// THE ENTROPY ALGORITHM against the OWNER-SUPPLIED Work vectors.
//
// testdata/synthetic/work/ holds byte-identical copies of the vectors the
// owner's P4 data-instantiation readiness package delivered
// (entropy_python_vectors.json from Python's standard library, and
// entropy_node_vectors_and_controls.json from Node's crypto module, cross-
// checked PASS in ENTROPY_CROSSCHECK.json; see work/WORK_PROVENANCE.md for
// the package hash and the per-file hashes). They pin the approved
// p4-hmac-sha256-u64/v1 framing end to end: the public seed, the exact
// length-prefixed message bytes and their UTF-8 byte lengths, the SHA-256 of
// the message, the full HMAC, and the first-eight-byte big-endian sixteen-
// digit word. No expectation below was derived from the Go code under test.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/p4offline"
)

// workPythonVectors is the shape of entropy_python_vectors.json.
type workPythonVectors struct {
	SeedHex string `json:"seed_hex"`
	Vectors []struct {
		Fixture     string `json:"fixture"`
		Run         string `json:"run"`
		Word        string `json:"word"`
		LPHex       string `json:"lp_hex"`
		LPSHA256    string `json:"lp_sha256"`
		FullHMACHex string `json:"full_hmac_hex"`
		Hex16Word   string `json:"hex16_word"`
	} `json:"vectors"`
}

// workNodeVectors is the shape of entropy_node_vectors_and_controls.json.
type workNodeVectors struct {
	SeedUTF8     string `json:"seed_utf8"`
	SeedHex      string `json:"seed_hex"`
	FixtureCount int    `json:"fixture_count"`
	Vectors      []struct {
		Fixture             string `json:"fixture"`
		DatasetID           string `json:"dataset_id"`
		DatasetVersion      string `json:"dataset_version"`
		CommonFactsetDigest string `json:"common_factset_digest"`
		PairedOpportunityID string `json:"paired_opportunity_id"`
		RunIndex            string `json:"run_index"`
		WordIndex           string `json:"word_index"`
		LPByteLength        string `json:"lp_byte_length"`
		LPSHA256            string `json:"lp_sha256"`
		FullHMACHex         string `json:"full_hmac_hex"`
		Hex16Word           string `json:"hex16_word"`
	} `json:"vectors"`
	Controls struct {
		UTF8ByteLength struct {
			Input            string `json:"input"`
			ActualByteLength string `json:"actual_byte_length"`
			HealthyLPHex     string `json:"healthy_lp_hex"`
		} `json:"utf8_byte_length"`
		First8BigEndian struct {
			HealthyHex16              string `json:"healthy_hex16"`
			NegativeLittleEndianHex16 string `json:"negative_little_endian_hex16"`
		} `json:"first8_big_endian"`
		FixedWidthZero struct {
			HealthyZero string `json:"healthy_zero"`
		} `json:"fixed_width_zero"`
	} `json:"controls"`
}

// The owner's package hashes of the three entropy files, as recorded in
// work/WORK_PROVENANCE.md and the package's SHA256SUMS.txt. The tests read
// the files only after the bytes are proven to be the owner's.
const (
	workPythonVectorsSHA256 = "e0f7b5422e22b4ecb6522f8822ca7b23f0ec5bdf9fce41fb932d704a6c5a75f2"
	workNodeVectorsSHA256   = "df666aaea0b5852770b38f9f3348edfb012fc7e49ed4332d3866aafe7255f2b8"
	workCrosscheckSHA256    = "d2a0564438fda469eb7438fd1876967a531a86a3c13d44378f0f2f4aceecec48"
)

// readWorkFile reads one owner file and refuses bytes other than the
// recorded ones.
func readWorkFile(t *testing.T, name, wantSHA256 string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/synthetic/work/" + name)
	if err != nil {
		t.Fatalf("read Work file %s: %v", name, err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != wantSHA256 {
		t.Fatalf("%s hashes to %s; the owner's package recorded %s — these are not the Work bytes", name, got, wantSHA256)
	}
	return raw
}

func loadWorkVectors(t *testing.T) (workPythonVectors, workNodeVectors) {
	t.Helper()
	var py workPythonVectors
	if err := json.Unmarshal(readWorkFile(t, "entropy_python_vectors.json", workPythonVectorsSHA256), &py); err != nil {
		t.Fatalf("decode Work Python vectors: %v", err)
	}
	var node workNodeVectors
	if err := json.Unmarshal(readWorkFile(t, "entropy_node_vectors_and_controls.json", workNodeVectorsSHA256), &node); err != nil {
		t.Fatalf("decode Work Node vectors: %v", err)
	}
	var cross struct {
		Status      string `json:"status"`
		VectorCount int    `json:"vector_count"`
		SeedEqual   bool   `json:"seed_equal"`
	}
	if err := json.Unmarshal(readWorkFile(t, "ENTROPY_CROSSCHECK.json", workCrosscheckSHA256), &cross); err != nil {
		t.Fatalf("decode Work crosscheck: %v", err)
	}
	if cross.Status != "PASS" || cross.VectorCount != 7 || !cross.SeedEqual {
		t.Fatalf("the owner's Python/Node crosscheck is not a PASS over seven vectors: %+v", cross)
	}
	if len(py.Vectors) != 7 || len(node.Vectors) != 7 || node.FixtureCount != 7 {
		t.Fatalf("the Work package delivers exactly seven vector points: python %d, node %d (%s)",
			len(py.Vectors), len(node.Vectors), strconv.Itoa(node.FixtureCount))
	}
	// The two files are paired by position; the identities must agree at
	// every position, so a reordered file cannot pair a Python expectation
	// with another Node vector's coordinates.
	for i := range node.Vectors {
		if py.Vectors[i].Fixture != node.Vectors[i].Fixture || py.Vectors[i].Run != node.Vectors[i].RunIndex ||
			py.Vectors[i].Word != node.Vectors[i].WordIndex {
			t.Fatalf("vector %d: Python names %s/%s/%s, Node names %s/%s/%s", i,
				py.Vectors[i].Fixture, py.Vectors[i].Run, py.Vectors[i].Word,
				node.Vectors[i].Fixture, node.Vectors[i].RunIndex, node.Vectors[i].WordIndex)
		}
	}
	return py, node
}

// workVector picks the Work vector with the named identity, so a test names
// the case it runs rather than a position.
func workVector(t *testing.T, node workNodeVectors, fixture, run, word string) int {
	t.Helper()
	for i, v := range node.Vectors {
		if v.Fixture == fixture && v.RunIndex == run && v.WordIndex == word {
			return i
		}
	}
	t.Fatalf("no Work vector %s/%s/%s", fixture, run, word)
	return -1
}

// workCoordinates builds the coordinates of one Work vector through the JSON
// wire form, so the vector's own field names are the binding.
func workCoordinates(t *testing.T, datasetID, datasetVersion, digest, opportunity, runIndex string) p4offline.EntropyCoordinates {
	t.Helper()
	run, err := strconv.ParseUint(runIndex, 10, 32)
	if err != nil {
		t.Fatalf("run index %q: %v", runIndex, err)
	}
	wire, err := json.Marshal(map[string]any{
		"datasetId":           datasetID,
		"datasetVersion":      datasetVersion,
		"commonFactsetDigest": digest,
		"pairedOpportunityId": opportunity,
		"trajectory":          run,
	})
	if err != nil {
		t.Fatal(err)
	}
	var coords p4offline.EntropyCoordinates
	if err := json.Unmarshal(wire, &coords); err != nil {
		t.Fatal(err)
	}
	return coords
}

// TestWorkEntropyMessageBytesMatchTheApprovedFraming pins the exact framed
// message of every Work vector: nine length-prefixed UTF-8 parts —
// algorithm, dataset id, dataset version, the sha256:-prefixed common
// factset digest, the paired opportunity id, P3B_ORDERED_RULES, the decimal
// run index, bernoulli_raw, the decimal word index — with u64 big-endian
// BYTE lengths (fixture B's identities are multi-byte UTF-8).
func TestWorkEntropyMessageBytesMatchTheApprovedFraming(t *testing.T) {
	py, node := loadWorkVectors(t)
	for i, v := range node.Vectors {
		coords := workCoordinates(t, v.DatasetID, v.DatasetVersion, v.CommonFactsetDigest, v.PairedOpportunityID, v.RunIndex)
		word, err := strconv.ParseUint(v.WordIndex, 10, 64)
		if err != nil {
			t.Fatalf("word index %q: %v", v.WordIndex, err)
		}
		msg := p4offline.EntropyMessage(coords, word)
		if got, want := hex.EncodeToString(msg), py.Vectors[i].LPHex; got != want {
			t.Errorf("fixture %s run %s word %s: message bytes\n got  %s\n want %s", v.Fixture, v.RunIndex, v.WordIndex, got, want)
		}
		if got := strconv.Itoa(len(msg)); got != v.LPByteLength {
			t.Errorf("fixture %s run %s word %s: message length %s, want %s", v.Fixture, v.RunIndex, v.WordIndex, got, v.LPByteLength)
		}
		sum := sha256.Sum256(msg)
		if got := hex.EncodeToString(sum[:]); got != v.LPSHA256 || got != py.Vectors[i].LPSHA256 {
			t.Errorf("fixture %s run %s word %s: message sha256 %s, want %s", v.Fixture, v.RunIndex, v.WordIndex, got, v.LPSHA256)
		}
	}
	// The UTF-8 byte-length control: the framed dataset identity of fixture
	// B is the healthy 25-byte frame, never an 18-code-unit one.
	ctl := node.Controls.UTF8ByteLength
	healthy, err := hex.DecodeString(ctl.HealthyLPHex)
	if err != nil {
		t.Fatal(err)
	}
	if ctl.ActualByteLength != strconv.Itoa(len([]byte(ctl.Input))) {
		t.Fatalf("control input %q is %d bytes, the control says %s", ctl.Input, len(ctl.Input), ctl.ActualByteLength)
	}
	b := node.Vectors[workVector(t, node, "B", "0", "0")]
	msg := p4offline.EntropyMessage(workCoordinates(t, b.DatasetID, b.DatasetVersion, b.CommonFactsetDigest, b.PairedOpportunityID, b.RunIndex), 0)
	if !bytes.Contains(msg, healthy) {
		t.Errorf("fixture B's message does not carry the healthy byte-length frame of %q", ctl.Input)
	}
}

// TestWorkEntropySeedIsTheApprovedPublicSeed pins the seed: SHA-256 of the
// UTF-8 text BUKEROV|P4|COMMON_CUTOFF|v1, used raw as the HMAC key.
func TestWorkEntropySeedIsTheApprovedPublicSeed(t *testing.T) {
	py, node := loadWorkVectors(t)
	if py.SeedHex != node.SeedHex {
		t.Fatalf("the two Work implementations disagree on the seed: %s / %s", py.SeedHex, node.SeedHex)
	}
	if p4offline.EntropySeedText != node.SeedUTF8 {
		t.Fatalf("seed text %q, the Work package says %q", p4offline.EntropySeedText, node.SeedUTF8)
	}
	seed := p4offline.EntropySeed()
	if got := hex.EncodeToString(seed[:]); got != node.SeedHex {
		t.Fatalf("seed %s, want %s", got, node.SeedHex)
	}
	independent := sha256.Sum256([]byte("BUKEROV|P4|COMMON_CUTOFF|v1"))
	if hex.EncodeToString(independent[:]) != node.SeedHex {
		t.Fatalf("the Work seed is not SHA-256 of the seed text; the vectors cannot be read")
	}
}

// TestWorkEntropyWordsMatchTheFullHMACAndHex16 pins the MAC and the word of
// every Work vector: HMAC-SHA256 under the raw seed bytes, the first eight
// bytes read big-endian, rendered as exactly sixteen lower-case hex digits.
func TestWorkEntropyWordsMatchTheFullHMACAndHex16(t *testing.T) {
	py, node := loadWorkVectors(t)
	for i, v := range node.Vectors {
		coords := workCoordinates(t, v.DatasetID, v.DatasetVersion, v.CommonFactsetDigest, v.PairedOpportunityID, v.RunIndex)
		word, err := strconv.ParseUint(v.WordIndex, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		mac, err := p4offline.EntropyMAC(coords, word)
		if err != nil {
			t.Fatalf("fixture %s run %s word %s: %v", v.Fixture, v.RunIndex, v.WordIndex, err)
		}
		if got := hex.EncodeToString(mac[:]); got != v.FullHMACHex || got != py.Vectors[i].FullHMACHex {
			t.Errorf("fixture %s run %s word %s: HMAC %s, want %s", v.Fixture, v.RunIndex, v.WordIndex, got, v.FullHMACHex)
		}
		w, err := p4offline.EntropyWord(coords, word)
		if err != nil {
			t.Fatal(err)
		}
		text, _ := predictioneval.OrderedRulesHex64(w).MarshalText()
		if string(text) != v.Hex16Word || string(text) != py.Vectors[i].Hex16Word {
			t.Errorf("fixture %s run %s word %s: word %s, want %s", v.Fixture, v.RunIndex, v.WordIndex, text, v.Hex16Word)
		}
		// The word must also be what the P3b-facing sequence carries at
		// that index, and a word at index k must not depend on the count.
		words, err := p4offline.GenerateEntropyWords(coords, int(word)+1)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := words[word].MarshalText(); string(got) != v.Hex16Word {
			t.Errorf("fixture %s run %s: sequence word %s is %s, want %s", v.Fixture, v.RunIndex, v.WordIndex, got, v.Hex16Word)
		}
	}
	// Controls: big-endian first eight bytes, never little-endian and never
	// the last eight; a zero word renders at fixed width. The little-endian
	// control is load-bearing: the same eight MAC bytes read the other way
	// must be exactly the control's negative value, which proves both that
	// these are the control's bytes and that the word reads them big-endian.
	ctl := node.Controls.First8BigEndian
	a := node.Vectors[workVector(t, node, "A", "0", "0")]
	coordsA := workCoordinates(t, a.DatasetID, a.DatasetVersion, a.CommonFactsetDigest, a.PairedOpportunityID, a.RunIndex)
	w, err := p4offline.EntropyWord(coordsA, 0)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := predictioneval.OrderedRulesHex64(w).MarshalText()
	if string(text) != ctl.HealthyHex16 {
		t.Errorf("first-eight-bytes control: %s, healthy %s", text, ctl.HealthyHex16)
	}
	mac, err := p4offline.EntropyMAC(coordsA, 0)
	if err != nil {
		t.Fatal(err)
	}
	var le uint64
	for i := 7; i >= 0; i-- {
		le = le<<8 | uint64(mac[i])
	}
	if leText, _ := predictioneval.OrderedRulesHex64(le).MarshalText(); string(leText) != ctl.NegativeLittleEndianHex16 || string(leText) == string(text) {
		t.Errorf("little-endian control: the same bytes read the other way render %s, the control says %s", leText, ctl.NegativeLittleEndianHex16)
	}
	zero, _ := predictioneval.OrderedRulesHex64(0).MarshalText()
	if string(zero) != node.Controls.FixedWidthZero.HealthyZero {
		t.Errorf("zero renders as %q, want %q", zero, node.Controls.FixedWidthZero.HealthyZero)
	}
}

// TestWorkEntropyTraceAndValidationUseTheApprovedWords pins that the P3b-
// facing trace and the validators are the same algorithm: a trace built
// for fixture A run 16383 carries the Work word at index 0, validates, and
// a trace with fixture B's word in its place is refused by index.
func TestWorkEntropyTraceAndValidationUseTheApprovedWords(t *testing.T) {
	_, node := loadWorkVectors(t)
	a := node.Vectors[workVector(t, node, "A", "16383", "0")]
	coords := workCoordinates(t, a.DatasetID, a.DatasetVersion, a.CommonFactsetDigest, a.PairedOpportunityID, a.RunIndex)
	trace, err := p4offline.BuildDrawTrace(coords, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := trace.Words[0].MarshalText(); string(got) != a.Hex16Word {
		t.Fatalf("trace word 0 is %s, the Work vector says %s", got, a.Hex16Word)
	}
	if err := p4offline.ValidateDrawTrace(coords, trace); err != nil {
		t.Fatalf("the built trace must validate: %v", err)
	}
	b := node.Vectors[workVector(t, node, "B", "0", "0")]
	var foreign predictioneval.OrderedRulesHex64
	if err := foreign.UnmarshalText([]byte(b.Hex16Word)); err != nil {
		t.Fatal(err)
	}
	if err := p4offline.ValidateEntropyWords(coords, []predictioneval.OrderedRulesHex64{foreign}); err == nil {
		t.Fatalf("fixture B's word must not validate under fixture A's coordinates")
	}
}
