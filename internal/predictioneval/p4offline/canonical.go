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
//
// IT HAS A LENGTH-ONLY MODE, and the mode exists so that ONE framing pass can
// answer two questions. A derivation check asks whether a value is the one an
// evaluator produced; the answer is a hash comparison, and computing the hash
// materializes every framed field -- so an edited result used to pay its own
// edit's width to be refused by one comparison. Recording the framed WIDTH
// beside the witness turns an edit that changes the TOTAL framed width into an
// O(number of fields) refusal.
//
// THE MODE IS WHY THAT WIDTH CANNOT DRIFT FROM THE WITNESS. The widest of these
// framings makes thirty-one direct part calls and three helper calls, and a
// hand-written mirror of it is a second place a future field can be forgotten;
// this package has already paid for one such mirror. Here the width is computed
// by running the SAME framing function with bytes switched off, so the two
// cannot disagree about which fields are framed.
type canonical struct {
	buf []byte
	// lenOnly switches every writer from appending bytes to accumulating the
	// width those bytes would have had, in n.
	lenOnly bool
	n       int
}

// framedLen is the width the framing would have had, valid only in lenOnly
// mode. It is a WIDTH and never a digest: two different values can frame to the
// same width, so this answers "could this be the value that was framed" and
// never "is it".
func (c *canonical) framedLen() int { return c.n }

// lpPrefix appends the eight-byte big-endian length of n.
func (c *canonical) lpPrefix(n uint64) {
	if c.lenOnly {
		c.n += 8
		return
	}
	c.buf = append(c.buf,
		byte(n>>56), byte(n>>48), byte(n>>40), byte(n>>32),
		byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}

// str frames one string part.
func (c *canonical) str(s string) {
	c.lpPrefix(uint64(len(s)))
	if c.lenOnly {
		c.n += len(s)
		return
	}
	c.buf = append(c.buf, s...)
}

// strHexOf frames the HEX RENDERING of raw without ever materializing that
// rendering. The bytes it appends are exactly the bytes c.str(hexEncode(raw))
// appends: the same eight-byte length prefix, which is twice len(raw) because
// hex is two digits per byte, followed by the same digits in the same order.
//
// IT EXISTS TO STREAM A REPRESENTATION, NOT TO CHANGE ONE. This package frames
// a hex rendering of a framing in claimKey, and that nesting is load-bearing:
// the registry digest is computed over those exact bytes and an independent
// golden pins it. So the nesting stays and only its INTERMEDIATE STRINGS go --
// which is the whole of the repair, and the reason the digest is unchanged.
func (c *canonical) strHexOf(raw []byte) {
	const digits = "0123456789abcdef"
	c.lpPrefix(uint64(len(raw)) * 2)
	if c.lenOnly {
		c.n += 2 * len(raw)
		return
	}
	// ONE EXACT EXTENSION, THEN FILL BY INDEX. Appending two digits at a time
	// into a growing slice re-allocates on the way up and costs MORE than the
	// single sized make inside hexEncode that this replaces -- measured, on
	// the first attempt at this function: one claim carrying a 1 MiB
	// identifier went from 19.06x the input to 31.24x. The saving is only real
	// when the destination is grown once.
	c.grow(len(raw) * 2)
	at := len(c.buf)
	c.buf = c.buf[:at+len(raw)*2]
	out := c.buf[at:]
	for i, v := range raw {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0x0f]
	}
}

// grow reserves room for n more bytes in one allocation, leaving the length
// unchanged.
//
// IT KEEPS GEOMETRIC GROWTH, and that is not an optimization detail -- it is
// the difference between linear and quadratic. A canonical buffer is usually
// ACCUMULATING: registryDigest appends every claim's key into one buffer, so a
// grow that reserves exactly what this call needs re-copies everything already
// written, once per call. Measured, on the first version of this helper, which
// did exactly that: 64 claims carrying 4 KiB identifiers went from 36.94x the
// input to 41.82x, while the single-claim case improved -- the signature of
// quadratic copying that only shows up once there is something to re-copy.
func (c *canonical) grow(n int) {
	if cap(c.buf)-len(c.buf) >= n {
		return
	}
	size := cap(c.buf) * 2
	if need := len(c.buf) + n; size < need {
		size = need
	}
	grown := make([]byte, len(c.buf), size)
	copy(grown, c.buf)
	c.buf = grown
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
// THE RULE THIS ENFORCES is written down once, here, because a sweep for its
// instances has to start from a statement of the rule rather than from a count
// of how many have been found.
//
// THE RULE: no expression may MATERIALIZE caller-supplied text on a path that
// has not already bounded that text. Where the text must be spoken about, name
// the FAULT and the EXTENT instead.
//
// TWO SCOPES NARROWER THAN THIS ONE DO NOT HOLD, and each is recorded because
// each is a way of reading the rule that lets an instance through:
//
//   - SCOPING IT TO THE FIRST STATEMENT of an exported function misses three
//     arms sitting further down the same two functions -- CommonFactset's stealth-proof and
//     completeness arms and ResolutionArtifact's outcome arm -- reached the
//     moment the digest verifies, which a caller can make it do, because the
//     serializers are exported and the digest is an unkeyed SHA-256 that
//     detects change and not origin.
//     THE SAME THREE ARMS WERE AN INSTANCE OF THE WORK HALF and were listed
//     only here, under the TEXT half. An adversarial review lane measured them:
//     an eleven-byte out-of-vocabulary Outcome cost 13,436,051 B/op on a
//     20,000-reference artifact and a seventeen-byte Completeness 16,885,191
//     B/op on a 20,000-outcome factset -- in both cases 100% of a FULL honest
//     verification of the same payload, for a verdict that reads one
//     constant-size field. Fixing their MESSAGES left their COST untouched,
//     which is the reading this note exists to stop: an instance can belong to
//     both halves, and repairing the half you noticed is not repairing the
//     site. The two arms whose verdict does not depend on the digest -- the
//     outcome vocabulary with the UNKNOWN winner check, and the completeness
//     vocabulary -- are now gated above the scan and the framing and refuse in
//     72 and 168 bytes on a 1 MiB payload. The stealth-proof arm is NOT
//     hoisted: it compares against a value derived from the settings, so it
//     genuinely needs what is above it.
//   - SCOPING IT TO GATES misses a further instance, which is not a gate at
//     all: the derivation argument that
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
// ONE INSTANCE DOES NOT BREAK THAT SCOPE AT ALL. It breaks the way the scope
// is APPLIED, which is worth separating, because the two above are scopes
// narrower than the rule and this one is not. It sits at the same call site as
// the one above, one gate later: the thunk moved the derivation behind
// decisionOf's binding gates, and asking "has a bound run on the path that
// reaches it?" gave the answer "yes, the gates above" -- true, and beside the
// point, because a LATER check refuses every caller-built result in one string
// comparison. So the question the rule asks is sharpened rather than rescoped:
// a materialization must not precede any gate that can refuse WITHOUT it.
// Measured: 75,527,248 bytes on a 16 MiB ruleset id, 4.50x the input, for a
// refusal that costs O(1).
//
// A LATER ROUND FOUND TWO MORE, BOTH OF THE SECOND KIND, and both in code
// this branch had repaired one gate earlier -- which is now the single most
// common way an instance survives: the repair stops at the gate that was
// reported. VerifyP3bRuleset checked its declared NATIVE digest for shape, an
// O(1) test of 64 characters, BELOW the full-buffer SHA-256, the key walk, the
// decode, the trailing scan and configsEqual: 167,771,494 bytes and 302 ms at a
// 16 MiB declared identity, exactly 10.00x it, for a 137-byte error; hoisted,
// 72 bytes in 3 allocations (312 with the message rendered), flat at 1 MiB and
// at 16 MiB. And VerifySourceRoundRegistry -- repaired one round
// earlier for its two CONSTANT clauses -- still flattened and re-reconciled
// every claim above its DIGEST comparison: 203,719,320 bytes and 192.8 ms at
// 16,000 claims, 100.0% of the cost of a valid verification, to refuse on 64
// characters.
//
// A DIFFERENT CLASS ENTIRELY, recorded here because round after round of reviewing
// THIS one walked straight past it, and so did two mechanical censuses -- 150
// functions and 251 early returns, 539 materialization nodes. walkRulesetValue
// recurses on a JSON array and nothing bounded the recursion, so a document
// costing two bytes a level drove the walk to the runtime's 1 GB stack limit
// and killed the process. It is not a cost defect: the resource is the STACK,
// the amplifier is structural rather than a byte count, and `fatal error: stack
// overflow` cannot be recovered, so it defeats a host's recover() and takes
// every other verification in flight with it. Bounded at rulesetMaxNesting, and
// the lesson is that "how expensive is this refusal" and "can this input reach
// an unrecoverable state" are different questions asked of the same code.
//
// THE DISCRIMINATOR, which an independent judge supplied by clearing a site
// this rule would otherwise have condemned: ask whether the product of the
// materialization is DISCARDED on the refusing path. At both sites above it is.
// At AssessDenominatorMembership the re-derived quality record costs the same
// 100% of its own computation on a refusal -- but that record IS the answer the
// refusal returns, which its own doc makes the contract. Work that the refusal
// consumes is not this class; work the refusal throws away is.
//
// THE SEVENTH WAS CLEARED BY NAME BY A SWEEP, which is a failure mode worth
// recording separately from a scope being too narrow. walkRulesetObject's
// unknown-key gate re-exported the caller's JSON key; a sweep declared it
// bounded because VerifyP3bRuleset applies rulesetRawCeiling before the walk.
// True, and not a bound: that ceiling is rulesetStructuralAllowance +
// 6*len(ConfigID), which the CALLER raises by declaring a large ConfigID, to
// roughly 769 MiB. A bound the input chooses is not a bound. Measured through
// the exported verifier: 159,406,880 bytes and a 16,777,374-byte error on a
// 16 MiB key, to say the key is misspelled.
//
// TWO SIBLINGS OF THE SAME ROUND ARE THE COST SHAPE WITHOUT THE TEXT, and they
// are repaired under the sharpened question rather than this helper, because
// there is no text to name: VerifySourceRoundRegistry reconciled a whole
// registry above a condition whose first two clauses are constant comparisons
// (76,808,072 bytes and 105 ms at 16,000 claims, to refuse on a string
// comparison), and evaluateProjected copied a caller's whole word array above
// two O(1) identity gates (8,407,200 bytes at 2^20 words, against 232 flat).
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
// And so is a caller's string that an EARLIER gate has already restricted to
// one of this package's own spellings: walkRulesetObject's duplicate-key arm
// keeps its quote, because a key reaches it only after passing the
// contract-spelling check one line below, and a review that read the two arms
// as the same defect was wrong about which one the caller controls.
//
// THE INVENTORY IS DERIVED, NOT WRITTEN. It lives in fence_test.go as
// suppliedTextExtentCensus, a map from function to call-site count that
// TestTheSuppliedTextExtentCensusMatchesAScan holds against an AST walk of this
// package's production files; the class table's row count lives beside it as
// firstGateTableRows, and the table asserts its own length against it.
//
// WHY IT MOVED. A count maintained by hand is a count that will be wrong, and a
// scan a comment merely suggests is a scan nobody runs. This package learned
// that once already as Rule E, where a convention presented as a machine check
// was made into one; the census is the same repair applied to a census.
//
// AND A RULE THIS PACKAGE NOW HOLDS ITSELF TO: a comment may state what the
// CODE used to do -- a sentinel that moved, a gate that was absent -- because a
// reader can check that against the history and a caller can act on it. It may
// not state what a COMMENT used to say. That kind of sentence cites review
// iterations this repository never recorded, so nobody reading it can audit it
// -- and a claim nobody can audit is a claim nothing can correct, which is how
// a wrong one survives a round that was looking for it.
//
// THE TEXT HALF -- do not materialize caller-supplied text on a path that has
// not bounded it -- covers every site in that census plus four that predate the
// helper and have their own tests: VerifyP3bRuleset's identity gate
// (rulesetIdentityFault), ValidateDrawTrace's two gates, and
// bindEntropyCoordinates' round gate.
//
// THE WORK HALF -- do not do work above a gate that can refuse without it, when
// the refusing path throws that work away -- is a list rather than a count, for
// the same reason: decisionOf's mint check (one seam, both Decision callers);
// VerifySourceRoundRegistry's constant Version clause, its digest-shape gate
// and, one round later, its digest comparison; the factset's and the resolution
// artifact's own digest-shape gates, each above a full framing pass;
// evaluateProjected's word-array copy above the two identity gates; and
// VerifyP3bRuleset's TWO declared-digest shape gates, each above the
// full-buffer hash of the carrier.
//
// One further text site is repaired under the same rule WITHOUT this helper,
// because it is not a gate and there is nothing to name: decisionOf takes its
// derivation as a thunk, so the caller's text is not built before the gates
// ABOVE that thunk. That is the instance the rule above is scoped to
// materialization for, rather than to gates. It is one seam with TWO
// production callers -- P2CaseResult.Decision and P3bCaseResult.Decision --
// and its test drives both, because a repair that lives at three places and is
// pinned at one leaves the other free to regress in silence. It did: an
// independent lane reverted the P2 caller alone and the whole suite passed.
//
// "NEVER BUILT ON THE REFUSING PATH" WOULD BE FALSE HERE, for a whole refusing
// path -- the underived one. A
// caller cannot set the unexported witness, so every result built outside this
// package refuses at the mint check; that check ran AFTER the derivation and
// after the decision was framed, and three independent probes measured
// 4,748,224 / 18,903,904 / 75,527,120 bytes for a 1 / 4 / 16 MiB ruleset id, on
// a 99-byte refusal. That is the instance that sharpened the QUESTION rather
// than the scope. It is repaired at the seam
// rather than at the fields: decisionOf takes the mint check as a predicate and
// runs it below its own gates and above the materialization, so every field
// feeding either derivation is covered at once -- the ruleset id measured here,
// and equally RulesetRawSHA256, NativeConfigDigest, the two trace identities
// and P2's Binding.Digest, none of which any gate bounds either.
//
// TestFirstGatesDoNotMaterializeSuppliedText is the registry for those of them
// reachable through an exported verifier -- most as a plain first-gate
// comparison, three from below a digest gate, kept in one table because they
// share the refusal RULE rather than the position. Its length is asserted
// against firstGateTableRows, and the sites it does NOT carry are named in its
// own header rather than counted here; walkRulesetObject's needs a raw document
// and a raised ceiling to reach, so it has its own test.
//
// A SECOND CROSS-ARTIFACT REGISTRY sits beside it, and is named here because
// nothing else points at it: digestShapeGates in evidence_test.go carries
// THREE OF THE SIX digest-SHAPE gates -- which bytes each refuses, and what
// its refusal states. The other three are not in it and are covered where they
// live: p3b.go's two by TestBothDeclaredDigestsAreHeldToTheirShapeByTheirOwnGates and
// TestNativeDigestShapeIsJudgedBeforeTheDocumentIsRead, and
// checkEntropyCoordinates' by TestEntropyRefusesOutOfProtocolInputs. A reader
// who takes that table for the whole class will miss the other three -- and at
// this commit's PARENT, 0b3cd2f, two of those three were held only to a LENGTH,
// which is the reading this pointer exists to head off. The parent is named
// rather than "the base commit": this branch's base is bd4d2727, where none of
// these files exist yet, so a reader sent there would find nothing to check.
//
// THE ORDER OF A GATE against the O(1) gates above it is a separate property
// again, and it is held for five of the six: the three cross-artifact ones by
// TestAnO1GateAboveAShapeGateStillSpeaksFirst, which builds its own rows rather
// than reading that table, and p3b.go's two by their own tests. The sixth,
// checkEntropyCoordinates' reference gate, is NOT held: hoisting it above the
// trajectory gate or sinking it below the paired-opportunity gates leaves the
// whole suite green. Every clause there is O(1) and every one returns
// ErrEntropyCoordinates, so no input and no sentinel moves -- what moves is
// which fault a caller is told about for coordinates wrong in more than one
// way, which is exactly the property the five pinned orders exist for.
//
// BOTH OF THOSE TABLES LIVE IN evidence_test.go and cover factset.go and
// resolution.go from there, which the repository's "tests belong
// next to the code they cover" rule does not fit, because no one file holds a
// class that spans three. It is left where it is; this pointer is the smaller
// change.
func suppliedTextExtent(s string) string {
	return suppliedExtent(len(s))
}

// suppliedExtent is the same sentence for an extent that is COMPUTED rather
// than measured off a string in hand.
//
// IT EXISTS BECAUSE ONE EXTENT CANNOT BE MEASURED WITHOUT BUILDING THE VERY
// TEXT THE CEILING EXCLUDES. decodeFault must report how wide a decoder's
// sentence is without rendering it, and the sentence's width is the width of
// the same sentence with its caller-supplied carrier blanked, plus the
// carrier's own length -- an identity, because the carrier is concatenated in
// whole. So the number is arithmetic and the string is never built.
//
// BOTH SPELLINGS ARE ONE CENSUS TARGET, because a second way to write an
// extent is a second place a future edit can write one that nothing counts.
// TestTheSuppliedTextExtentCensusMatchesAScan walks call sites of both names.
func suppliedExtent(n int) string {
	return strconv.Itoa(n) + " bytes"
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
