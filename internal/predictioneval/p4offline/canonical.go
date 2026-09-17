package p4offline

import (
	"crypto/sha256"
	"math"
	"strconv"
)

// Canonical serialization.
//
// Every digest and every serialization in this package uses ONE framing:
// length-prefixed parts, where the prefix is the part's byte length as an
// unsigned 64-bit big-endian integer and the part is the UTF-8 bytes of a
// string. A separator can be forged by a field that contains it; a length
// prefix cannot. Numbers are rendered as canonical text — base-ten for
// quantities, fixed-width lower-case hexadecimal for bit patterns — so the
// framed bytes are readable as well as hashable.
//
// The framing is written by hand (eight shifts) rather than through
// encoding/binary, for the same reason the predictioneval package does it:
// encoding/binary is the import that would drag reflect into the closure of a
// package that has no other use for it.

// canonical accumulates framed parts and can hash them.
type canonical struct {
	buf []byte
}

// lpPrefix appends the eight-byte big-endian length of n.
func (c *canonical) lpPrefix(n uint64) {
	c.buf = append(c.buf,
		byte(n>>56), byte(n>>48), byte(n>>40), byte(n>>32),
		byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}

// str frames one string part.
func (c *canonical) str(s string) {
	c.lpPrefix(uint64(len(s)))
	c.buf = append(c.buf, s...)
}

func (c *canonical) i64(v int64)    { c.str(strconv.FormatInt(v, 10)) }
func (c *canonical) u64(v uint64)   { c.str(strconv.FormatUint(v, 10)) }
func (c *canonical) boolean(v bool) { c.str(strconv.FormatBool(v)) }

// f64 frames a float by its BITS, as fixed-width hex, never by a decimal
// rendering that could round two distinguishable values together.
func (c *canonical) f64(v float64) { c.str(hex16(math.Float64bits(v))) }

// count frames a collection length ahead of its elements so an element
// boundary cannot be moved by shifting bytes between neighbours.
func (c *canonical) count(n int) { c.i64(int64(n)) }

// bytes returns the framed bytes.
func (c *canonical) bytes() []byte { return c.buf }

// digest returns the lower-case hex SHA-256 of the framed bytes.
func (c *canonical) digest() string {
	sum := sha256.Sum256(c.buf)
	return hexEncode(sum[:])
}

// suppliedTextExtent names a caller-supplied string by its LENGTH, for a
// refusal message.
//
// THE RULE THIS ENFORCES, written down once because it has now been rediscovered
// FOUR times on this branch -- and stated on the fourth attempt in the only
// terms that have survived review.
//
// THE RULE: no expression may MATERIALIZE caller-supplied text on a path that
// has not already bounded that text. Where the text must be spoken about, name
// the FAULT and the EXTENT instead.
//
// The scope has been wrong twice and each wording is recorded, because each one
// is how the next round was missed:
//
//   - "the FIRST statement of an exported function" missed three arms sitting
//     further down the same two functions -- CommonFactset's stealth-proof and
//     completeness arms and ResolutionArtifact's outcome arm -- reached the
//     moment the digest verifies, which a caller can make it do, because the
//     serializers are exported and the digest is an unkeyed SHA-256 that
//     detects change and not origin.
//   - "any GATE whose operand is caller-supplied" then missed the fifth
//     instance, which is not a gate at all: the derivation argument that
//     P3bCaseResult.Decision builds for decisionOf. Go evaluates arguments
//     before the call, so it was fully built before decisionOf's first gate
//     ran -- 3,686,696 bytes on a 1 MiB ruleset id, for a call that then
//     refuses. decisionOf now takes that derivation as a thunk.
//
// So the scope is neither position nor gate-ness. It is MATERIALIZATION ON AN
// UNGATED PATH: a concatenation, a quote, a conversion, a join, an argument
// expression -- anywhere, gate or not. What matters is whether the bound has
// already run on the path that reaches it.
//
// Measured on a 64 MiB supplied string: 134,234,440 bytes allocated and a
// 67,108,944-byte error, to report that a string differs from a 20-byte
// compile-time constant. On the three that the position-based wording missed:
// 201,352,984 bytes for the completeness arm and 201,353,096 for the stealth
// arm, each returning a 67 MB error.
//
// A constant this package OWNS is quoted, deliberately: it is compile-time and
// short, and naming it is the whole diagnosis. So is a digest this package has
// just computed, which is its own 64 hex characters whatever the caller sent.
//
// THE INVENTORY, counted once so the next reader does not have to re-derive it.
// THIRTEEN sites are repaired under this rule:
//
//	4 predate this helper and have their own tests --
//	    VerifyP3bRuleset's identity gate (rulesetIdentityFault),
//	    ValidateDrawTrace's two gates,
//	    bindEntropyCoordinates' round gate.
//	9 call this helper --
//	    3 in VerifyCommonFactset (contract, protocol, digest),
//	    1 in VerifyCommonFactset's stealth-proof arm,
//	    1 in checkFactsetConsistency's completeness arm,
//	    2 in VerifyResolutionArtifact (contract, obligations),
//	    1 in its outcome arm,
//	    1 in decisionOf.
//
// A fourteenth site is repaired under the same rule WITHOUT this helper,
// because it is not a gate and there is nothing to name: decisionOf takes its
// derivation as a thunk so the caller's text is never built on the refusing
// path. That is the fifth instance, and the reason the rule above is scoped to
// materialization rather than to gates.
//
// TestFirstGatesDoNotMaterializeSuppliedText is the registry for the nine
// reachable through an exported verifier. It names where the other five are
// pinned rather than duplicating them.
func suppliedTextExtent(s string) string {
	return strconv.Itoa(len(s)) + " bytes"
}

// lpFrame frames the parts in order with the u64 big-endian length prefix and
// returns the framed bytes. It is the exact framing [EntropyAlgorithmVersion]
// hashes, exported through [EntropyMessage] so an independent implementation
// can be checked byte for byte.
func lpFrame(parts ...string) []byte {
	var c canonical
	for _, p := range parts {
		c.str(p)
	}
	return c.buf
}

// hexEncode renders bytes as lower-case hex without encoding/hex (which
// imports fmt, which imports os).
func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0x0f]
	}
	return string(out)
}

// hex16 renders a uint64 as exactly sixteen lower-case hex digits.
func hex16(v uint64) string {
	const digits = "0123456789abcdef"
	var out [16]byte
	for i := 15; i >= 0; i-- {
		out[i] = digits[v&0x0f]
		v >>= 4
	}
	return string(out[:])
}

// isCanonicalHex reports whether s is exactly n lower-case hex digits.
func isCanonicalHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// sha256Hex is the lower-case hex SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hexEncode(sum[:])
}
