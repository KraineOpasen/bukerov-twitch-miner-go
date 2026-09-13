package p4offline

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"strconv"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// THE ENTROPY ALGORITHM: p4-hmac-sha256-u64/v1.
//
// The P3b core consumes an explicitly supplied list of raw 64-bit words under
// rand 0.8.5's Bernoulli semantics. The P4 protocol fixes how those words are
// PRODUCED, so that every trajectory of every source round is reproducible
// from coordinates alone and no global generator, seed file or clock is ever
// consulted:
//
//	LP(s)   = uint64 big-endian byte length of UTF-8(s) || UTF-8(s)
//	M(k)    = LP("p4-hmac-sha256-u64/v1") || LP("p4-offline/v1")
//	          || LP(decimal trajectory) || LP(sourceRoundID)
//	          || LP(policyID) || LP(decimal k)
//	word(k) = big-endian uint64 of the FIRST 8 bytes of
//	          HMAC-SHA256(key = UTF-8(keyMaterial), M(k))
//	wire    = 16 lower-case hexadecimal digits ([predictioneval.OrderedRulesHex64])
//
// Words are consumed SEQUENTIALLY from k = 0 by the P3b traversal, and the
// traversal alone decides how many are consumed: a comparator that does not
// match consumes none, a matched rule at a rate of exactly one consumes none,
// a matched rule at a rate of exactly zero consumes ONE and fails, and the
// per-outcome default consumes none. P2 consumes no entropy at all — its
// evaluation function does not accept a trace. None of that is decided here;
// this file only produces and validates the words.
//
// The key material is a supplied string. It is not a secret in the
// cryptographic sense — the algorithm is public and deterministic — it is the
// run's root that makes two P4 runs with different roots independent. The
// message is length-prefixed so a round identity containing a delimiter
// cannot alias another coordinate.
//
// Vectors produced by an independent implementation (Python's standard
// library, cross-checked with openssl) pin every step: see
// testdata/synthetic/entropy_vectors.json and PROVENANCE.md.

// maxEntropyIdentityBytes bounds the key material and each coordinate
// identity, at the same ceiling the P3b core places on its identifiers. The
// message is allocated once per word for up to MaxOrderedRulesDrawWords
// words, so an unbounded identity would be an unbounded allocation.
const maxEntropyIdentityBytes = predictioneval.MaxOrderedRulesIdentifierBytes

// EntropyCoordinates are the deterministic coordinates of one entropy run:
// which trajectory, which source round, and which policy is drawing.
type EntropyCoordinates struct {
	// Trajectory is in [0, TrajectoryCount).
	Trajectory uint32 `json:"trajectory"`
	// SourceRoundID is the public round identity (the Twitch event id).
	SourceRoundID string `json:"sourceRoundId"`
	// PolicyID names the drawing policy, so two rulesets over the same round
	// and trajectory do not share words.
	PolicyID string `json:"policyId"`
}

// Entropy refusals.
var (
	// ErrEntropyKey is empty or over-long key material. There is no default
	// root.
	ErrEntropyKey = errors.New("p4offline: entropy key material is empty or over-long")
	// ErrEntropyCoordinates is a trajectory at or past TrajectoryCount, or an
	// empty or over-long round or policy identity.
	ErrEntropyCoordinates = errors.New("p4offline: entropy coordinates are outside the protocol")
	// ErrEntropyCount is a negative word count or one past the P3b trace
	// ceiling.
	ErrEntropyCount = errors.New("p4offline: entropy word count is outside the protocol")
	// ErrEntropyWordMismatch is a supplied word the algorithm does not
	// reproduce at its index.
	ErrEntropyWordMismatch = errors.New("p4offline: supplied entropy word does not match the algorithm")
	// ErrEntropyTrace is a draw trace whose declared semantics or run identity
	// do not match the coordinates it claims.
	ErrEntropyTrace = errors.New("p4offline: supplied draw trace does not match the algorithm")
)

// EntropyMessage is the exact framed message M(k) the MAC is computed over.
// It is exported so an independent implementation can be checked byte for
// byte rather than only through the resulting word.
func EntropyMessage(coords EntropyCoordinates, index uint64) []byte {
	return lpFrame(
		EntropyAlgorithmVersion,
		ProtocolVersion,
		strconv.FormatUint(uint64(coords.Trajectory), 10),
		coords.SourceRoundID,
		coords.PolicyID,
		strconv.FormatUint(index, 10),
	)
}

func checkEntropyInputs(key string, coords EntropyCoordinates) error {
	if key == "" || len(key) > maxEntropyIdentityBytes {
		return ErrEntropyKey
	}
	if coords.Trajectory >= TrajectoryCount {
		return errors.Join(ErrEntropyCoordinates,
			errors.New("p4offline: trajectory "+strconv.FormatUint(uint64(coords.Trajectory), 10)+
				" is not below "+strconv.Itoa(TrajectoryCount)))
	}
	if coords.SourceRoundID == "" || len(coords.SourceRoundID) > maxEntropyIdentityBytes {
		return errors.Join(ErrEntropyCoordinates, errors.New("p4offline: source round identity is empty or over-long"))
	}
	if coords.PolicyID == "" || len(coords.PolicyID) > maxEntropyIdentityBytes {
		return errors.Join(ErrEntropyCoordinates, errors.New("p4offline: policy identity is empty or over-long"))
	}
	return nil
}

func checkEntropyCount(count int) error {
	if count < 0 || count > predictioneval.MaxOrderedRulesDrawWords {
		return errors.Join(ErrEntropyCount,
			errors.New("p4offline: "+strconv.Itoa(count)+" words requested; the admitted range is 0.."+
				strconv.Itoa(predictioneval.MaxOrderedRulesDrawWords)))
	}
	return nil
}

// EntropyWord computes word k for the coordinates under the key.
func EntropyWord(key string, coords EntropyCoordinates, index uint64) (uint64, error) {
	if err := checkEntropyInputs(key, coords); err != nil {
		return 0, err
	}
	return entropyWord(key, coords, index), nil
}

// entropyWord is EntropyWord after the inputs have been checked once.
func entropyWord(key string, coords EntropyCoordinates, index uint64) uint64 {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write(EntropyMessage(coords, index))
	sum := mac.Sum(nil)
	var w uint64
	for i := 0; i < 8; i++ {
		w = w<<8 | uint64(sum[i])
	}
	return w
}

// GenerateEntropyWords produces words 0..count-1 in order.
func GenerateEntropyWords(key string, coords EntropyCoordinates, count int) ([]predictioneval.OrderedRulesHex64, error) {
	if err := checkEntropyInputs(key, coords); err != nil {
		return nil, err
	}
	if err := checkEntropyCount(count); err != nil {
		return nil, err
	}
	out := make([]predictioneval.OrderedRulesHex64, count)
	for k := 0; k < count; k++ {
		out[k] = predictioneval.OrderedRulesHex64(entropyWord(key, coords, uint64(k)))
	}
	return out, nil
}

// entropyRunID names a run by its coordinates and word count, with each
// identity quoted so a component containing a delimiter cannot read as
// another coordinate. It is the P3b trace's RunID and is bound into the
// core's entropy and consumed-prefix digests. The words themselves — HMACs
// over the length-prefixed coordinates — are what make a trace inseparable
// from its coordinates; the run identity is the human-readable name of that
// binding, not a second one.
func entropyRunID(coords EntropyCoordinates, count int) string {
	return EntropyAlgorithmVersion +
		":t=" + strconv.FormatUint(uint64(coords.Trajectory), 10) +
		":round=" + strconv.Quote(coords.SourceRoundID) +
		":policy=" + strconv.Quote(coords.PolicyID) +
		":words=" + strconv.Itoa(count)
}

// BuildDrawTrace produces the P3b-facing trace: the generated words under the
// rand-0.8.5 Bernoulli semantics the core consumes, identified by the run's
// coordinates.
func BuildDrawTrace(key string, coords EntropyCoordinates, count int) (predictioneval.SuppliedDrawTrace, error) {
	words, err := GenerateEntropyWords(key, coords, count)
	if err != nil {
		return predictioneval.SuppliedDrawTrace{}, err
	}
	return predictioneval.SuppliedDrawTrace{
		RunID:                   entropyRunID(coords, count),
		EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion,
		Words:                   words,
	}, nil
}

// ValidateEntropyWords recomputes every word and refuses the first mismatch
// by index.
func ValidateEntropyWords(key string, coords EntropyCoordinates, words []predictioneval.OrderedRulesHex64) error {
	if err := checkEntropyInputs(key, coords); err != nil {
		return err
	}
	if err := checkEntropyCount(len(words)); err != nil {
		return err
	}
	for k := range words {
		if want := entropyWord(key, coords, uint64(k)); uint64(words[k]) != want {
			return errors.Join(ErrEntropyWordMismatch,
				errors.New("p4offline: word at index "+strconv.Itoa(k)+" is "+hex16(uint64(words[k]))+
					", the algorithm produces "+hex16(want)))
		}
	}
	return nil
}

// ValidateDrawTrace checks a whole P3b trace: its declared semantics, its run
// identity and every word.
func ValidateDrawTrace(key string, coords EntropyCoordinates, trace predictioneval.SuppliedDrawTrace) error {
	if err := checkEntropyInputs(key, coords); err != nil {
		return err
	}
	if trace.EntropySemanticsVersion != predictioneval.OrderedRulesEntropySemanticsVersion {
		return errors.Join(ErrEntropyTrace,
			errors.New("p4offline: trace declares semantics "+strconv.Quote(trace.EntropySemanticsVersion)+
				", the core consumes only "+strconv.Quote(predictioneval.OrderedRulesEntropySemanticsVersion)))
	}
	if want := entropyRunID(coords, len(trace.Words)); trace.RunID != want {
		return errors.Join(ErrEntropyTrace,
			errors.New("p4offline: trace run identity "+strconv.Quote(trace.RunID)+" is not "+strconv.Quote(want)))
	}
	return ValidateEntropyWords(key, coords, trace.Words)
}
