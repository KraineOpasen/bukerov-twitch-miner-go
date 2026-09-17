package p4offline

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"strconv"
	"unicode/utf8"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// THE ENTROPY ALGORITHM: p4-hmac-sha256-u64/v1, exactly as the approved P4
// preregistration protocol (section 8) fixes it and as the owner-supplied
// Work vectors pin it (testdata/synthetic/work/, see WORK_PROVENANCE.md).
//
// The P3b core consumes an explicitly supplied list of raw 64-bit words under
// rand 0.8.5's Bernoulli semantics. The protocol fixes how those words are
// PRODUCED, so that every trajectory of every paired opportunity is
// reproducible from public coordinates alone and no global generator, seed
// file, clock or observed realization is ever consulted:
//
//	seed    = SHA-256(UTF-8("BUKEROV|P4|COMMON_CUTOFF|v1"))  — 32 raw bytes, PUBLIC
//	LP(s)   = uint64 big-endian byte length of UTF-8(s) || UTF-8(s)
//	M(k)    = LP("p4-hmac-sha256-u64/v1") || LP(dataset_id) || LP(dataset_version)
//	          || LP("sha256:" + common_factset_digest) || LP(paired_opportunity_id)
//	          || LP("P3B_ORDERED_RULES") || LP(decimal run_index)
//	          || LP("bernoulli_raw") || LP(decimal k)
//	word(k) = big-endian uint64 of the FIRST 8 bytes of HMAC-SHA256(seed, M(k))
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
// The seed is not a secret — the algorithm is public and deterministic — it
// is the protocol's fixed root. Two P4 runs over the same dataset identity,
// factset and opportunity draw the same words at the same run index, which
// is what makes the 16,384 trajectories a fixed, auditable schedule. The
// message is length-prefixed so an identity containing a delimiter cannot
// alias another coordinate, and the lengths are BYTE lengths of UTF-8, so a
// multi-byte identity frames as the Work vectors frame it.
//
// The common factset digest enters the message as the protocol spells it —
// "sha256:" followed by the 64 lower-case hex digits of
// [CommonFactset.Digest] — through [DigestReference].

// Fixed message parts of the approved framing.
const (
	// EntropySeedText is the public seed text; [EntropySeed] is its SHA-256.
	EntropySeedText = "BUKEROV|P4|COMMON_CUTOFF|v1"
	// EntropyPolicyLabel names the only policy that draws: P3b consumes the
	// words, P2 consumes none.
	EntropyPolicyLabel = "P3B_ORDERED_RULES"
	// EntropyDrawKind names what the words are: raw Bernoulli words under
	// the core's declared semantics.
	EntropyDrawKind = "bernoulli_raw"
	// DigestReferencePrefix is how the protocol spells a SHA-256 reference.
	DigestReferencePrefix = "sha256:"
)

// maxEntropyIdentityBytes bounds each coordinate identity, at the same
// ceiling the P3b core places on its identifiers. The message is allocated
// once per word for up to MaxOrderedRulesDrawWords words, so an unbounded
// identity would be an unbounded allocation.
const maxEntropyIdentityBytes = predictioneval.MaxOrderedRulesIdentifierBytes

// EntropySeed is the protocol's public seed: SHA-256 of [EntropySeedText],
// used raw as the HMAC key. It is recomputed on every call; nothing caches
// a mutable copy.
func EntropySeed() [32]byte {
	return sha256.Sum256([]byte(EntropySeedText))
}

// DigestReference spells a 64-hex-digit digest as the protocol references
// it: "sha256:" followed by the digits. It does not validate its argument;
// [EntropyCoordinates] are validated where they are used.
func DigestReference(hexDigest string) string {
	return DigestReferencePrefix + hexDigest
}

// isDigestReference reports whether s is exactly "sha256:" followed by 64
// lower-case hex digits.
func isDigestReference(s string) bool {
	return len(s) == len(DigestReferencePrefix)+64 &&
		s[:len(DigestReferencePrefix)] == DigestReferencePrefix &&
		isCanonicalHex(s[len(DigestReferencePrefix):], 64)
}

// EntropyCoordinates are the public coordinates of one entropy run, in the
// protocol's own vocabulary. The dataset identity and version are the
// owner's dataset binding, supplied by the caller and carried verbatim: this
// package holds no dataset registry and cannot verify them. The factset
// digest and the paired opportunity are bound to the factset being evaluated
// by [EvaluateP3bCase] and [EvaluateP3bWithTrace].
//
// AUDIT RULE for the two unverifiable coordinates: because the seed is public
// and the ruleset is not a coordinate, the whole 16,384-trajectory schedule
// of every opportunity is a function of (DatasetID, DatasetVersion) and the
// bound facts. Relabelling either string redraws every word of every
// trajectory — a free resample of the whole comparison. The two strings must
// therefore be exactly the preregistered identity of the frozen dataset (the
// owner's binding, PENDING at the time of writing), fixed before any
// evaluation; every [P3bCaseResult] records the coordinates and the run
// identity it was produced under (for a refused projection, the identity of
// the empty trace for those coordinates), and an audit compares them to that
// binding. This package can make the choice visible; only the binding can
// make it right.
type EntropyCoordinates struct {
	// DatasetID is the owner's identity of the frozen dataset.
	DatasetID string `json:"datasetId"`
	// DatasetVersion is the owner's version of that dataset.
	DatasetVersion string `json:"datasetVersion"`
	// CommonFactsetDigest is [DigestReference] of the outcome-free common
	// factset digest ([CommonFactset.Digest]) of the paired opportunity.
	CommonFactsetDigest string `json:"commonFactsetDigest"`
	// PairedOpportunityID names the paired opportunity — the one selected
	// first automated opportunity of a reconciled source round. In this
	// package it is the round's public event identity: seam 3 admits exactly
	// one canonical opportunity per source round of a dataset, so the event
	// identity names it uniquely.
	PairedOpportunityID string `json:"pairedOpportunityId"`
	// Trajectory is the run index, in [0, TrajectoryCount).
	Trajectory uint32 `json:"trajectory"`
}

// Entropy refusals.
var (
	// ErrEntropyCoordinates is a trajectory at or past TrajectoryCount, an
	// empty, over-long or not-valid-UTF-8 identity, or a factset digest not
	// spelled as a "sha256:" reference.
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
// byte rather than only through the resulting word. It frames whatever it
// is given; the coordinates are validated by every function that draws.
func EntropyMessage(coords EntropyCoordinates, index uint64) []byte {
	return lpFrame(
		EntropyAlgorithmVersion,
		coords.DatasetID,
		coords.DatasetVersion,
		coords.CommonFactsetDigest,
		coords.PairedOpportunityID,
		EntropyPolicyLabel,
		strconv.FormatUint(uint64(coords.Trajectory), 10),
		EntropyDrawKind,
		strconv.FormatUint(index, 10),
	)
}

// checkEntropyCoordinates is the one gate every drawing function passes
// through, so it is where an identity that cannot be carried is refused.
//
// The UTF-8 test is not decoration. Every exported field of a [P3bCaseResult]
// carries a JSON tag, and Go's encoder replaces an invalid sequence with
// U+FFFD, so coordinates holding invalid UTF-8 are MACed over bytes their own
// declared encoding does not preserve: the stored form names a different
// identity than the one that drew, and two distinct invalid sequences collapse
// onto the same encoded string. The digest coordinate needs no such test --
// it is already held to lower-case hex -- and the trajectory is numeric.
//
// WHERE THE DERIVED IDENTITY COMES FROM, and what that does and does not imply.
// Two of the three identities are supplied by the caller; PairedOpportunityID is
// DERIVED -- bindEntropyCoordinates requires it to equal the episode's EventID,
// which this package carries verbatim from supplied records and validates for
// encoding nowhere. EventID is not a payload field: it is a SQL TEXT column, and
// SQLite does not hold TEXT to UTF-8, so the JSON-decoding argument that covers
// the payload does not cover it.
//
// That is as far as it goes, and an earlier version of this comment went
// further in both directions. Both extra steps were wrong:
//
//   - It is NOT reachable through this build's collector. The only production
//     writer takes the value off a JSON-decoded frame, where Go has already
//     replaced any invalid sequence with U+FFFD, and it refuses an identifier
//     over the same 4096-byte bound this gate applies rather than truncating it.
//     Editing a written row closes itself: the observation digest covers
//     EventID, a mismatch forces the session to INTEGRITY_ERROR, the reader then
//     returns no records at all, and the session is refused outright. The class
//     is reachable only from a store this build's collector did not write.
//   - An error here is NOT a silent drop from the comparison. AssessCaseQuality
//     is given the DATASET, not the evaluation, and has its own branch for a
//     factset that never reached a decision: with both policy slots empty it
//     still records an enumerated quality and reason. The same seam already
//     returns a plain error for the two commonest evidential facts there are --
//     a missing balance and a stage never reached -- so there is no
//     errors-are-only-for-caller-faults rule here for this one to breach.
//
// The disposition therefore stays an error, and stays deliberate. Converting it
// into a recorded refusal would trade a loud refusal of a store this build did
// not write for a quiet non-counting record of one, which is the wrong
// direction. What would justify revisiting it is a non-collector record source
// being admitted on purpose -- an owner decision, not a mechanical one.
func checkEntropyCoordinates(coords EntropyCoordinates) error {
	if coords.Trajectory >= TrajectoryCount {
		return errors.Join(ErrEntropyCoordinates,
			errors.New("p4offline: trajectory "+strconv.FormatUint(uint64(coords.Trajectory), 10)+
				" is not below "+strconv.Itoa(TrajectoryCount)))
	}
	if coords.DatasetID == "" || len(coords.DatasetID) > maxEntropyIdentityBytes {
		return errors.Join(ErrEntropyCoordinates, errors.New("p4offline: dataset identity is empty or over-long"))
	}
	if !utf8.ValidString(coords.DatasetID) {
		return errors.Join(ErrEntropyCoordinates, errors.New("p4offline: dataset identity is not valid UTF-8"))
	}
	if coords.DatasetVersion == "" || len(coords.DatasetVersion) > maxEntropyIdentityBytes {
		return errors.Join(ErrEntropyCoordinates, errors.New("p4offline: dataset version is empty or over-long"))
	}
	if !utf8.ValidString(coords.DatasetVersion) {
		return errors.Join(ErrEntropyCoordinates, errors.New("p4offline: dataset version is not valid UTF-8"))
	}
	if !isDigestReference(coords.CommonFactsetDigest) {
		return errors.Join(ErrEntropyCoordinates,
			errors.New("p4offline: common factset digest is not \"sha256:\" followed by 64 lower-case hex digits"))
	}
	if coords.PairedOpportunityID == "" || len(coords.PairedOpportunityID) > maxEntropyIdentityBytes {
		return errors.Join(ErrEntropyCoordinates, errors.New("p4offline: paired opportunity identity is empty or over-long"))
	}
	if !utf8.ValidString(coords.PairedOpportunityID) {
		return errors.Join(ErrEntropyCoordinates, errors.New("p4offline: paired opportunity identity is not valid UTF-8"))
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

// EntropyMAC is the full 32-byte HMAC-SHA256 for word k under the seed. It is
// exported so an independent implementation's full MAC — not only its first
// eight bytes — can be compared.
func EntropyMAC(coords EntropyCoordinates, index uint64) ([32]byte, error) {
	if err := checkEntropyCoordinates(coords); err != nil {
		return [32]byte{}, err
	}
	return entropyMAC(coords, index), nil
}

// entropyMAC is EntropyMAC after the coordinates have been checked once.
func entropyMAC(coords EntropyCoordinates, index uint64) [32]byte {
	seed := EntropySeed()
	mac := hmac.New(sha256.New, seed[:])
	_, _ = mac.Write(EntropyMessage(coords, index))
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

// EntropyWord computes word k for the coordinates.
func EntropyWord(coords EntropyCoordinates, index uint64) (uint64, error) {
	if err := checkEntropyCoordinates(coords); err != nil {
		return 0, err
	}
	return entropyWord(coords, index), nil
}

// entropyWord is EntropyWord after the coordinates have been checked once:
// the FIRST eight MAC bytes, most significant first.
func entropyWord(coords EntropyCoordinates, index uint64) uint64 {
	sum := entropyMAC(coords, index)
	var w uint64
	for i := 0; i < 8; i++ {
		w = w<<8 | uint64(sum[i])
	}
	return w
}

// GenerateEntropyWords produces words 0..count-1 in order.
func GenerateEntropyWords(coords EntropyCoordinates, count int) ([]predictioneval.OrderedRulesHex64, error) {
	if err := checkEntropyCoordinates(coords); err != nil {
		return nil, err
	}
	if err := checkEntropyCount(count); err != nil {
		return nil, err
	}
	out := make([]predictioneval.OrderedRulesHex64, count)
	for k := 0; k < count; k++ {
		out[k] = predictioneval.OrderedRulesHex64(entropyWord(coords, uint64(k)))
	}
	return out, nil
}

// entropyRunID names a run by its coordinates and word count, with each
// identity quoted so a component containing a delimiter cannot read as
// another coordinate. It is the P3b trace's RunID and is bound into the
// core's entropy and consumed-prefix digests. The words themselves — MACs
// over the length-prefixed coordinates — are what make a trace inseparable
// from its coordinates; the run identity is the human-readable name of that
// binding, not a second one.
func entropyRunID(coords EntropyCoordinates, count int) string {
	return EntropyAlgorithmVersion +
		":dataset=" + strconv.Quote(coords.DatasetID) +
		":version=" + strconv.Quote(coords.DatasetVersion) +
		":factset=" + strconv.Quote(coords.CommonFactsetDigest) +
		":opportunity=" + strconv.Quote(coords.PairedOpportunityID) +
		":policy=" + EntropyPolicyLabel +
		":run=" + strconv.FormatUint(uint64(coords.Trajectory), 10) +
		":draw=" + EntropyDrawKind +
		":words=" + strconv.Itoa(count)
}

// BuildDrawTrace produces the P3b-facing trace: the generated words under the
// rand-0.8.5 Bernoulli semantics the core consumes, identified by the run's
// coordinates.
func BuildDrawTrace(coords EntropyCoordinates, count int) (predictioneval.SuppliedDrawTrace, error) {
	words, err := GenerateEntropyWords(coords, count)
	if err != nil {
		return predictioneval.SuppliedDrawTrace{}, err
	}
	return predictioneval.SuppliedDrawTrace{
		RunID:                   entropyRunID(coords, count),
		EntropySemanticsVersion: predictioneval.OrderedRulesEntropySemanticsVersion,
		Words:                   words,
	}, nil
}

// ValidateEntropyWords recomputes every word SUPPLIED, at its index, and
// refuses the first mismatch.
//
// IT DOES NOT BIND THE COUNT, and the difference decides what a caller may
// conclude. Any correct PREFIX validates, including an empty list and a nil
// one: for a truncated schedule the loop simply runs fewer times and returns
// nil, so "the 16-word schedule verified" is not a conclusion this function
// supports.
//
// [ValidateDrawTrace] is stronger but NOT that strong, and the difference is
// worth stating because an earlier version of this paragraph got it wrong. Its
// run identity is computed from the trace's OWN word count, so it refuses a
// trace whose Words were truncated AFTER it was drawn — the RunID then names a
// length the words no longer have. It does not establish that the schedule is
// the length the caller wanted: a correctly drawn 8-word trace validates
// cleanly under coordinates for which the caller needed sixteen. A caller who
// needs that must compare len(trace.Words) against the count the traversal
// requires. The package's own path does, and degrades honestly when it is
// short — the core returns UNKNOWN_INPUT / ENTROPY_EXHAUSTED rather than
// inventing a decision.
func ValidateEntropyWords(coords EntropyCoordinates, words []predictioneval.OrderedRulesHex64) error {
	if err := checkEntropyCoordinates(coords); err != nil {
		return err
	}
	if err := checkEntropyCount(len(words)); err != nil {
		return err
	}
	for k := range words {
		if want := entropyWord(coords, uint64(k)); uint64(words[k]) != want {
			return errors.Join(ErrEntropyWordMismatch,
				errors.New("p4offline: word at index "+strconv.Itoa(k)+" is "+hex16(uint64(words[k]))+
					", the algorithm produces "+hex16(want)))
		}
	}
	return nil
}

// ValidateDrawTrace checks a whole P3b trace: its declared semantics, its run
// identity and every word.
func ValidateDrawTrace(coords EntropyCoordinates, trace predictioneval.SuppliedDrawTrace) error {
	if err := checkEntropyCoordinates(coords); err != nil {
		return err
	}
	// NEITHER of these two fields is bounded anywhere on this path, and that is
	// why neither is quoted. checkEntropyCoordinates above bounds the three
	// COORDINATE identities and ValidateEntropyWords below bounds the word
	// slice; the trace's own semantics version and run identity are plain
	// caller-supplied strings that nothing reaches. Quoting them cost 1.67 GB
	// of allocation and a 268 MB error on a 64 MiB input -- to report that two
	// version strings differ. The rule is the one rulesetIdentityFault records
	// and the native producer wrote down before either of us: a refusal names
	// the fault and the LENGTHS, never the text it is refusing. The constant
	// the core consumes IS quoted, because it is a compile-time constant and
	// naming it is the whole diagnosis.
	if trace.EntropySemanticsVersion != predictioneval.OrderedRulesEntropySemanticsVersion {
		return errors.Join(ErrEntropyTrace,
			errors.New("p4offline: trace declares semantics of "+strconv.Itoa(len(trace.EntropySemanticsVersion))+
				" bytes, the core consumes only "+strconv.Quote(predictioneval.OrderedRulesEntropySemanticsVersion)))
	}
	if want := entropyRunID(coords, len(trace.Words)); trace.RunID != want {
		return errors.Join(ErrEntropyTrace,
			errors.New("p4offline: trace run identity is "+strconv.Itoa(len(trace.RunID))+
				" bytes and is not this run's, which is "+strconv.Itoa(len(want))+" bytes"))
	}
	return ValidateEntropyWords(coords, trace.Words)
}
