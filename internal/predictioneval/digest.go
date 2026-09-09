package predictioneval

import (
	"crypto/sha256"
	"hash"
	"strconv"
)

// The common-input digest.
//
// This is NOT the store's row digest and never replaces it. This one witnesses
// something the store cannot: that the set of facts a replay treated as an
// attempt's inputs is exactly the causally-closed prefix that existed when the
// attempt ended, and did not change when later facts were appended.
//
// It therefore hashes each fact's IDENTITY and the store's own row witness,
// rather than re-deriving the row's contents. Re-deriving would duplicate P1's
// sanitizer and digest algorithms — two implementations of one contract that
// could drift — while achieving nothing extra: if a row's bytes changed
// without its witness being updated to match, the reader has already
// established that separately.
//
// The strength of that check has a bound worth stating exactly, because
// overstating it is the same class of error this package exists to avoid. The
// store's row witness is an UNKEYED SHA-256 over data stored beside it. It
// therefore detects accidental corruption — a truncated write, a bad page, a
// partial restore — and it does NOT authenticate anything against an adversary
// with write access to the database file, who can recompute the witness after
// editing a payload and repair the session counters to match. Hashing that
// witness in here inherits exactly that property and adds no authenticity of
// its own.
//
// What this digest does hold against a hostile store is narrower and still
// worth having: it binds a scorecard to the specific fact set it was computed
// from, so a case, an evaluation and a settlement cannot be recombined across
// attempts. Detecting malicious EDITS would need a MAC or signature whose key
// lives outside the database — a producer-side change, not a reader-side one,
// and outside this module's scope. Nothing here should be read as claiming a
// row was proven genuine.
//
// Length-prefixed parts, matching the store's own convention: a separator can
// be forged by a field containing it, a length prefix cannot.

// digestPart feeds one field in length-prefixed.
//
// The eight-byte big-endian length is written by hand rather than with
// encoding/binary. That package is the ONLY thing that put reflect into this
// package's import graph, and the dependency fence's honesty depends on the
// graph containing nothing it cannot justify — eight shifts are a smaller
// price than a residue entry that needs explaining.
func digestPart(h hash.Hash, part string) {
	n := uint64(len(part))
	lenBuf := [8]byte{
		byte(n >> 56), byte(n >> 48), byte(n >> 40), byte(n >> 32),
		byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n),
	}
	_, _ = h.Write(lenBuf[:])
	_, _ = h.Write([]byte(part))
}

func digestInt(h hash.Hash, v int64) { digestPart(h, strconv.FormatInt(v, 10)) }
func digestBool(h hash.Hash, v bool) { digestPart(h, strconv.FormatBool(v)) }

// commonInputDigest hashes the causally-closed slice of facts an attempt was
// decided on.
//
// Only facts UP TO AND INCLUDING the attempt's terminal fact are passed here.
// Nothing later can reach this function, which is what makes the digest stable
// against a growing store — and what
// TestAppendingLaterFactsCannotChangeAnEarlierAttemptsInputsOrDigest falsifies if it ever stops being
// true.
func commonInputDigest(attemptID uint64, slice []SourceRecord) string {
	h := sha256.New()
	// The version goes in first, so a future change to this encoding produces
	// a visibly different digest rather than a silently incompatible one.
	digestPart(h, CommonInputDigestVersion)
	digestPart(h, ModelVersion)
	digestInt(h, int64(attemptID))
	digestInt(h, int64(len(slice)))

	for i := range slice {
		r := &slice[i]
		digestPart(h, r.ObservationID)
		digestPart(h, r.CollectorSessionID)
		digestInt(h, r.CollectorEpoch)
		digestInt(h, r.CollectorSequence)
		digestPart(h, r.PoolInstanceID)
		digestPart(h, r.RoundIncarnationID)
		digestPart(h, r.RoundCaptureOrigin)
		digestPart(h, r.RoundCaptureGapCause)
		digestPart(h, r.EventID)
		digestPart(h, r.Kind)
		digestInt(h, r.PayloadVersion)
		digestBool(h, r.PayloadUndecodable)
		// The store's own witness of the row's bytes. Covering it here binds
		// this digest to the exact content the store hashed, without this
		// package re-implementing how that content is canonicalized.
		digestPart(h, r.ObservationSHA256)
	}
	return hexEncode(h.Sum(nil))
}

// hexEncode renders a digest as lower-case hex.
//
// It is hand-rolled rather than encoding/hex because encoding/hex imports fmt,
// which imports os — and the whole point of the dependency fence is that this
// package's import graph should not contain a package that can open a file.
// Twenty lines here buys a fence that means what it says. See
// TestProductionCoreDependencyFence for what remains and why.
func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0x0f]
	}
	return string(out)
}
