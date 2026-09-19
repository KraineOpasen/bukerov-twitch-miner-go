package p4offline

import (
	"errors"
	"strconv"
	"unicode/utf8"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// SEAM 8: the separate, immutable resolution artifact.
//
// How a round settled is a fact the EVALUATORS never see: no evaluation
// function in this package accepts a [ResolutionArtifact], and the artifact is
// projected from typed evidence, never from the factset. The artifact carries
// the exact public round identity, the ordered outcome identities, the
// outcome (WINNER_KNOWN, REFUND or UNKNOWN), the evidence references, the
// availability, the projector and proof revisions, the revision of the
// obligations that were applied, and a digest over all of it.
//
// # Verification is re-derivation
//
// A digest detects CHANGE, not origin: anyone who can build the struct can
// compute its digest. So [VerifyResolutionArtifact] does not stop at the
// digest. For a WINNER_KNOWN or REFUND artifact it re-projects the artifact's
// own facts through the proof obligations and requires the result to be the
// artifact, byte for byte. An artifact that asserts a winner its own facts do
// not prove is refused, whatever its digest says. Every consumer in this
// package verifies before it reads.
//
// # Proof obligations — PROVISIONAL label, reconciled semantics
//
// The obligations enforced here are this package's own definition, versioned
// as [ResolutionObligationsRevision] and carried in every artifact. The
// owner's source-validation evidence (testdata/synthetic/work/, see
// WORK_PROVENANCE.md) has since been reconciled against them and found
// semantically identical where both speak, with one NARROWING on this side:
// the source validation admits, as a proof obligation, a derived winner from
// a unique, causally linked, platform-accepted factual action whose terminal
// says WON (or LOST on an independently proven exhaustive binary set); this
// package implements NO derived-winner path, so such evidence yields UNKNOWN
// here, never a winner. A narrowing can only withhold, never upgrade. The
// availability-before-T1 censoring the source validation names is a
// dataset-window binding: the pure source records carry no timestamps, so it
// belongs to the owner's dataset binding, not to this projector. Every
// obligation below is a rule the contract already implies by what it
// forbids:
//
//	WINNER_KNOWN requires ALL of:
//	  - the claim is WINNER_KNOWN and the basis is
//	    PLATFORM_RESOLVED_EVENT_WINNING_OUTCOME — the winning outcome id as the
//	    platform's own resolution event names it;
//	  - availability AVAILABLE;
//	  - an ordered outcome set of at least two distinct, non-empty identities;
//	  - a winner identity that occurs in that set exactly once;
//	  - at least one evidence reference, every reference naming this round,
//	    and at least one in round state RESOLVED;
//	  - a projector revision and a proof revision.
//	REFUND requires the same, with the basis PLATFORM_CANCELED_EVENT_REFUND,
//	  NO winner identity, and a reference in round state CANCELED.
//
// Every other basis is refused by name. In particular a terminal WON/LOST, a
// RESOLVED state on its own, the nearest fact in time, the same event alone
// or the same stake alone establish NOTHING here: a LOST on a non-binary
// round names no winner at all, and a LOST on a binary round would name one
// only through an inference this package does not make.
//
// What no rule here can check is whether a supplied reference EXISTS: a pure
// function cannot authenticate evidence it is handed. A reference is matched
// to the round by its event identity; its collector coordinates travel with
// it for the reader's audit and are not compared to anything.

// ResolutionOutcome is the closed resolution vocabulary.
type ResolutionOutcome string

// Resolution outcomes.
const (
	ResolutionWinnerKnown ResolutionOutcome = "WINNER_KNOWN"
	ResolutionRefund      ResolutionOutcome = "REFUND"
	ResolutionUnknown     ResolutionOutcome = "UNKNOWN"
)

// Accepted proof bases. Anything else is refused.
const (
	ProofBasisPlatformResolvedWinner = "PLATFORM_RESOLVED_EVENT_WINNING_OUTCOME"
	ProofBasisPlatformCanceledRefund = "PLATFORM_CANCELED_EVENT_REFUND"
)

// Availability is the closed availability vocabulary.
type Availability string

// Availability values.
const (
	AvailabilityAvailable Availability = "AVAILABLE"
	// AvailabilityNotRecorded is the honest answer for evidence read through
	// the P1 replay mirror, which carries no winning outcome identity.
	AvailabilityNotRecorded Availability = "NOT_RECORDED_IN_P1_MIRROR"
	AvailabilityUnavailable Availability = "UNAVAILABLE"
)

// ResolutionObligationsRevision names the obligation set applied. The label
// stays PROVISIONAL: the set has been reconciled with the owner's source-
// validation evidence as semantically identical with a documented narrowing
// (no derived-winner path), and the label changes only by an owner decision.
const ResolutionObligationsRevision = "p4offline-resolution-obligations/provisional-v1"

// Round states the obligations read, as the store spells them.
const (
	roundStateResolved = "RESOLVED"
	roundStateCanceled = "CANCELED"
)

// Resolution refusal reasons. Closed vocabulary.
const (
	ResolutionRefusalBasisNotAccepted       = "BASIS_NOT_ACCEPTED"
	ResolutionRefusalWinnerNotInOutcomeSet  = "WINNER_NOT_IN_OUTCOME_SET"
	ResolutionRefusalRefundConflict         = "REFUND_CONFLICT"
	ResolutionRefusalClaimContradiction     = "CLAIM_CONTRADICTION"
	ResolutionRefusalEvidenceMissing        = "EVIDENCE_MISSING"
	ResolutionRefusalEvidenceRoundMismatch  = "EVIDENCE_ROUND_MISMATCH"
	ResolutionRefusalEvidenceStateMismatch  = "EVIDENCE_STATE_MISMATCH"
	ResolutionRefusalNotAvailable           = "AVAILABILITY_NOT_AVAILABLE"
	ResolutionRefusalOutcomeSetInvalid      = "OUTCOME_SET_INVALID"
	ResolutionRefusalRevisionMissing        = "REVISION_MISSING"
	ResolutionRefusalClaimOutsideVocabulary = "CLAIM_OUTSIDE_VOCABULARY"
	ResolutionRefusalRoundIdentityMissing   = "ROUND_IDENTITY_MISSING"
	ResolutionRefusalAvailabilityVocabulary = "AVAILABILITY_OUTSIDE_VOCABULARY"
	// ResolutionRefusalTextNotExpressible names evidence carrying a string
	// this artifact's own encoding cannot carry unchanged. The value is DROPPED
	// rather than projected: see expressibleEvidence.
	ResolutionRefusalTextNotExpressible = "TEXT_NOT_EXPRESSIBLE"
)

// Resolution verification refusals.
var (
	// ErrResolutionDigest is an artifact whose digest, contract or
	// obligations revision does not match its fields.
	ErrResolutionDigest = errors.New("p4offline: resolution artifact digest does not verify")
	// ErrResolutionNotDerivable is an artifact whose asserted outcome is not
	// what its own facts prove under the obligations.
	ErrResolutionNotDerivable = errors.New("p4offline: resolution artifact is not the projection of its own facts")
)

// PublicRoundIdentity is the platform's identity of a prediction round.
type PublicRoundIdentity struct {
	EventID   string `json:"eventId"`
	ChannelID string `json:"channelId,omitempty"`
}

// EvidenceReference names one persisted fact without carrying its content.
type EvidenceReference struct {
	ObservationID      string `json:"observationId"`
	CollectorEpoch     int64  `json:"collectorEpoch"`
	CollectorSessionID string `json:"collectorSessionId"`
	Kind               string `json:"kind"`
	Phase              string `json:"phase"`
	RoundState         string `json:"roundState,omitempty"`
	EventID            string `json:"eventId"`
}

// ResolutionEvidence is the typed input to the projector.
type ResolutionEvidence struct {
	Round              PublicRoundIdentity `json:"round"`
	OrderedOutcomeIDs  []string            `json:"orderedOutcomeIds"`
	Claim              ResolutionOutcome   `json:"claim"`
	WinnerOutcomeID    string              `json:"winnerOutcomeId,omitempty"`
	ProofBasis         string              `json:"proofBasis,omitempty"`
	EvidenceReferences []EvidenceReference `json:"evidenceReferences,omitempty"`
	Availability       Availability        `json:"availability"`
	ProjectorRevision  string              `json:"projectorRevision"`
	ProofRevision      string              `json:"proofRevision"`
}

// ResolutionArtifact is the immutable, digested projection.
type ResolutionArtifact struct {
	ContractVersion     string              `json:"contractVersion"`
	ObligationsRevision string              `json:"obligationsRevision"`
	Round               PublicRoundIdentity `json:"round"`
	OrderedOutcomeIDs   []string            `json:"orderedOutcomeIds"`
	Outcome             ResolutionOutcome   `json:"outcome"`
	WinnerOutcomeID     string              `json:"winnerOutcomeId,omitempty"`
	// WinnerIndex is the winner's position in OrderedOutcomeIDs, or -1.
	WinnerIndex        int                 `json:"winnerIndex"`
	ProofBasis         string              `json:"proofBasis,omitempty"`
	EvidenceReferences []EvidenceReference `json:"evidenceReferences,omitempty"`
	Availability       Availability        `json:"availability"`
	ProjectorRevision  string              `json:"projectorRevision"`
	ProofRevision      string              `json:"proofRevision"`
	Refusals           []string            `json:"refusals,omitempty"`
	// ResolutionFactsDigest is the exact-byte digest both scorers share, as
	// the protocol spells it: "sha256:" followed by the 64 lower-case hex
	// digits of SHA-256 over [SerializeResolutionArtifact].
	ResolutionFactsDigest string `json:"resolutionFactsDigest"`
}

// ProjectResolution applies the proof obligations and digests the result.
// It is total: a withheld obligation yields an UNKNOWN artifact naming the
// refusal, never an error and never a winner.
func ProjectResolution(ev ResolutionEvidence) ResolutionArtifact {
	// THE PRODUCER IS GATED, not only the verifier. VerifyResolutionArtifact
	// refuses a SUPPLIED artifact carrying a string encoding/json cannot carry
	// unchanged; without this, the package's own exported projector could still
	// MINT one -- an artifact that fails its own verifier and, after a JSON
	// round trip, presents as tampering. Reproduced before this gate existed:
	// an invalid-UTF-8 Round.ChannelID yielded WINNER_KNOWN with NO refusals,
	// retaining the bad bytes, and its own verifier rejected it.
	//
	// The offending value is DROPPED rather than carried, because refusing
	// while still framing the bytes would leave the artifact exactly as
	// unserializable as before. The refusal below then forces UNKNOWN through
	// the same `len(a.Refusals) == 0` guard every other refusal uses, so this
	// artifact asserts nothing.
	ev, lossyText := expressibleEvidence(ev)
	a := ResolutionArtifact{
		ContractVersion:     ResolutionFactsDigestVersion,
		ObligationsRevision: ResolutionObligationsRevision,
		Round:               ev.Round,
		OrderedOutcomeIDs:   append([]string(nil), ev.OrderedOutcomeIDs...),
		Outcome:             ResolutionUnknown,
		WinnerIndex:         -1,
		ProofBasis:          ev.ProofBasis,
		EvidenceReferences:  append([]EvidenceReference(nil), ev.EvidenceReferences...),
		Availability:        ev.Availability,
		ProjectorRevision:   ev.ProjectorRevision,
		ProofRevision:       ev.ProofRevision,
	}
	refuse := func(reason string) { a.Refusals = appendOnce(a.Refusals, reason) }

	if lossyText {
		refuse(ResolutionRefusalTextNotExpressible)
	}
	if ev.Round.EventID == "" {
		refuse(ResolutionRefusalRoundIdentityMissing)
	}
	if ev.ProjectorRevision == "" || ev.ProofRevision == "" {
		refuse(ResolutionRefusalRevisionMissing)
	}
	switch ev.Availability {
	case AvailabilityAvailable, AvailabilityNotRecorded, AvailabilityUnavailable:
	default:
		refuse(ResolutionRefusalAvailabilityVocabulary)
	}
	winnerCount := 0
	winnerIndex := -1
	setValid := len(ev.OrderedOutcomeIDs) >= 2
	seen := map[string]bool{}
	for i, id := range ev.OrderedOutcomeIDs {
		if id == "" || seen[id] {
			setValid = false
		}
		seen[id] = true
		if id == ev.WinnerOutcomeID && id != "" {
			winnerCount++
			winnerIndex = i
		}
	}
	if !setValid {
		refuse(ResolutionRefusalOutcomeSetInvalid)
	}
	refsValid := len(ev.EvidenceReferences) > 0
	resolvedRef, canceledRef := false, false
	for _, r := range ev.EvidenceReferences {
		if r.ObservationID == "" {
			refsValid = false
		}
		if r.EventID != ev.Round.EventID {
			refuse(ResolutionRefusalEvidenceRoundMismatch)
		}
		switch r.RoundState {
		case roundStateResolved:
			resolvedRef = true
		case roundStateCanceled:
			canceledRef = true
		}
	}

	switch ev.Claim {
	case ResolutionWinnerKnown:
		if !refsValid {
			refuse(ResolutionRefusalEvidenceMissing)
		}
		if ev.Availability != AvailabilityAvailable {
			refuse(ResolutionRefusalNotAvailable)
		}
		if ev.ProofBasis != ProofBasisPlatformResolvedWinner {
			refuse(ResolutionRefusalBasisNotAccepted)
		}
		if winnerCount != 1 {
			refuse(ResolutionRefusalWinnerNotInOutcomeSet)
		}
		if refsValid && !resolvedRef {
			refuse(ResolutionRefusalEvidenceStateMismatch)
		}
		if len(a.Refusals) == 0 {
			a.Outcome = ResolutionWinnerKnown
			a.WinnerOutcomeID = ev.WinnerOutcomeID
			a.WinnerIndex = winnerIndex
		}
	case ResolutionRefund:
		if !refsValid {
			refuse(ResolutionRefusalEvidenceMissing)
		}
		if ev.Availability != AvailabilityAvailable {
			refuse(ResolutionRefusalNotAvailable)
		}
		if ev.ProofBasis != ProofBasisPlatformCanceledRefund {
			refuse(ResolutionRefusalBasisNotAccepted)
		}
		if ev.WinnerOutcomeID != "" {
			refuse(ResolutionRefusalRefundConflict)
		}
		if refsValid && !canceledRef {
			refuse(ResolutionRefusalEvidenceStateMismatch)
		}
		if len(a.Refusals) == 0 {
			a.Outcome = ResolutionRefund
		}
	case ResolutionUnknown:
		if ev.WinnerOutcomeID != "" {
			refuse(ResolutionRefusalClaimContradiction)
		}
	default:
		refuse(ResolutionRefusalClaimOutsideVocabulary)
	}
	if a.Outcome != ResolutionWinnerKnown {
		a.WinnerOutcomeID = ""
		a.WinnerIndex = -1
	}
	a.ResolutionFactsDigest = resolutionDigest(a)
	return a
}

// evidenceTextFault names the first string in the evidence that the artifact's
// own encoding cannot carry unchanged, or "" when there is none.
//
// On HONEST evidence it allocates nothing and copies nothing, which is what lets
// expressibleEvidence leave such evidence alone entirely. On the refusal path it
// allocates once for the two positions that carry an index, which a review lane
// measured and an earlier unqualified "allocates nothing" claimed away.
func evidenceTextFault(ev ResolutionEvidence) string {
	for _, f := range [...]struct{ what, v string }{
		{"round event id", ev.Round.EventID},
		{"round channel id", ev.Round.ChannelID},
		{"winner outcome id", ev.WinnerOutcomeID},
		{"proof basis", ev.ProofBasis},
		{"projector revision", ev.ProjectorRevision},
		{"proof revision", ev.ProofRevision},
		{"availability", string(ev.Availability)},
	} {
		if !utf8.ValidString(f.v) {
			return f.what
		}
	}
	for i, id := range ev.OrderedOutcomeIDs {
		if !utf8.ValidString(id) {
			return "ordered outcome id " + strconv.Itoa(i)
		}
	}
	for i, r := range ev.EvidenceReferences {
		for _, f := range [...]struct{ what, v string }{
			{"observation id", r.ObservationID}, {"collector session id", r.CollectorSessionID},
			{"kind", r.Kind}, {"phase", r.Phase},
			{"round state", r.RoundState}, {"event id", r.EventID},
		} {
			if !utf8.ValidString(f.v) {
				return "evidence reference " + strconv.Itoa(i) + " " + f.what
			}
		}
	}
	return ""
}

// expressibleEvidence returns ev with every string the artifact would frame
// replaced by "" if it is not valid UTF-8, and reports whether it replaced any.
//
// Dropping rather than keeping is the point: encoding/json writes invalid UTF-8
// WITHOUT error by substituting U+FFFD, so an artifact that merely REFUSED
// while still carrying the bytes would still fail its own digest after a round
// trip. A blanked value also makes the ordinary refusals fire on their own
// terms — a blanked round identity is a missing round identity — so the
// artifact that comes back is one this package can verify and store.
//
// It does not touch Claim: an unrepresentable claim is outside the vocabulary
// and the switch in ProjectResolution ABOVE already refuses it there. Checked rather than assumed,
// in "an unrepresentable claim is refused by the vocabulary, not carried".
//
// HONEST EVIDENCE IS RETURNED UNTOUCHED, and that is a repair rather than an
// optimisation. The first version copied both slices unconditionally, and
// ProjectResolution then copies them AGAIN into the artifact, so every honest
// projection paid two copies where it used to pay one, on a path where nothing
// is lossy. Worst of all on the verifier's re-projection, which runs only AFTER
// checkResolutionStringsExpressible has proved every string valid, so the second
// copy there was provably waste. The scan below decides first, and only lossy
// evidence is copied at all.
//
// THE SIZE OF THAT SAVING IS NOT QUOTED HERE, and the reason is this package's
// own rule catching this package out. An earlier wording gave "+12,009,584 bytes
// and +22.4% at 100,000 references" -- two figures from two DIFFERENT fixtures,
// neither written down, and a review lane showed no single fixture reaches both.
// The same commit had just retracted nine other absolute figures on exactly that
// ground, and then added more. What is true and fixture-independent: the saving
// is one copy of each slice per honest projection, and
// TestExpressibleEvidenceLeavesHonestEvidenceAlone asserts it as ALIASING rather
// than as a number.
//
// THE TWO SLICES ARE COPIED BEFORE ANYTHING IS BLANKED. ProjectResolution takes
// its evidence by value, which protects the scalars and protects nothing else:
// a slice copied by value still points at the caller's backing array, so
// blanking an element in place would silently rewrite the caller's own
// evidence. A producer that edits its input to make its output storable is a
// worse defect than the one this gate was added to fix. Pinned by the
// "caller's evidence is not rewritten" case, which fails without these copies.
func expressibleEvidence(ev ResolutionEvidence) (ResolutionEvidence, bool) {
	if evidenceTextFault(ev) == "" {
		return ev, false
	}
	keep := func(v *string) {
		if !utf8.ValidString(*v) {
			*v = ""
		}
	}
	keep(&ev.Round.EventID)
	keep(&ev.Round.ChannelID)
	keep(&ev.WinnerOutcomeID)
	keep(&ev.ProofBasis)
	keep(&ev.ProjectorRevision)
	keep(&ev.ProofRevision)
	if !utf8.ValidString(string(ev.Availability)) {
		ev.Availability = ""
	}
	ids := append([]string(nil), ev.OrderedOutcomeIDs...)
	for i := range ids {
		keep(&ids[i])
	}
	ev.OrderedOutcomeIDs = ids
	refs := append([]EvidenceReference(nil), ev.EvidenceReferences...)
	for i := range refs {
		keep(&refs[i].ObservationID)
		keep(&refs[i].CollectorSessionID)
		keep(&refs[i].Kind)
		keep(&refs[i].Phase)
		keep(&refs[i].RoundState)
		keep(&refs[i].EventID)
	}
	ev.EvidenceReferences = refs
	return ev, true
}

// SerializeResolutionArtifact renders the artifact's facts canonically:
// every field except ResolutionFactsDigest, length-prefixed, in a fixed
// order. It is exported so an auditor can recompute the digest by hand.
func SerializeResolutionArtifact(a ResolutionArtifact) []byte {
	var c canonical
	c.str(ResolutionFactsDigestVersion)
	c.str(a.ObligationsRevision)
	c.str(a.Round.EventID)
	c.str(a.Round.ChannelID)
	c.count(len(a.OrderedOutcomeIDs))
	for _, id := range a.OrderedOutcomeIDs {
		c.str(id)
	}
	c.str(string(a.Outcome))
	c.str(a.WinnerOutcomeID)
	c.i64(int64(a.WinnerIndex))
	c.str(a.ProofBasis)
	c.count(len(a.EvidenceReferences))
	for _, r := range a.EvidenceReferences {
		c.str(r.ObservationID)
		c.i64(r.CollectorEpoch)
		c.str(r.CollectorSessionID)
		c.str(r.Kind)
		c.str(r.Phase)
		c.str(r.RoundState)
		c.str(r.EventID)
	}
	c.str(string(a.Availability))
	c.str(a.ProjectorRevision)
	c.str(a.ProofRevision)
	c.count(len(a.Refusals))
	for _, r := range a.Refusals {
		c.str(r)
	}
	return c.bytes()
}

// checkResolutionStringsExpressible refuses an artifact holding a string its
// own encoding cannot carry unchanged. It covers every string
// [SerializeResolutionArtifact] frames, constant-size positions first and the
// two caller-sized slices last, for the reason checkFactsetValuesExpressible
// states: a supplier pairing one bad constant-size value with a huge slice
// would otherwise turn a constant-time refusal into work over the whole slice.
//
// It does NOT check ObligationsRevision, which the gate above holds to an exact
// constant, and it says nothing about whether a string is the one the platform
// reported. Every valid UTF-8 string is accepted exactly as before, including
// one that already contains U+FFFD.
// resolutionConstantStringsExpressible is the ONE-PER-ARTIFACT half of
// checkResolutionStringsExpressible: the eight strings an artifact carries
// exactly one of, whatever its size. The rest of that scan walks
// OrderedOutcomeIDs, EvidenceReferences and Refusals, and grows with the
// artifact.
//
// THE SPLIT EXISTS SO A PRECEDENCE CAN BE STATED ONCE. Two arms of
// VerifyResolutionArtifact decide from constant-size fields and are hoisted
// above the framing, and before the hoist every unreadable string in the
// artifact outranked their semantic claims -- because the whole scan ran
// first. The hoist took that away, and the first repair gave each arm a
// deferral on the string IT reads, which restored the precedence for two
// fields out of eight: an UNKNOWN artifact naming a readable winner beside an
// unreadable Round.EventID was still told it names a winner. A second review
// lane found that, which is this seam's third round on one rule.
//
// Deferring per-arm was the wrong shape. This is the rule: no semantic claim
// about an artifact may be made until every string the artifact carries one of
// is known to be readable. It is stated here and asked once, above both arms.
//
// IT IS NOT A CONSTANT-TIME GATE and is not claimed as one -- "constant-size"
// counts the FIELDS, not their bytes, and a caller may supply a megabyte of
// ProofBasis. What it preserves is the property the hoist was measured on:
// utf8.ValidString allocates nothing, so the eight scans add no bytes to a
// refusal the arms settle, and the O(n) walks over the three slices stay
// below them.
func resolutionConstantStringsExpressible(a ResolutionArtifact) error {
	for _, f := range [...]struct {
		what string
		v    string
	}{
		{"round event id", a.Round.EventID},
		{"round channel id", a.Round.ChannelID},
		{"outcome", string(a.Outcome)},
		{"winner outcome id", a.WinnerOutcomeID},
		{"proof basis", a.ProofBasis},
		{"availability", string(a.Availability)},
		{"projector revision", a.ProjectorRevision},
		{"proof revision", a.ProofRevision},
	} {
		if !utf8.ValidString(f.v) {
			return errors.Join(ErrResolutionDigest, errors.New("p4offline: "+f.what+
				" is not valid UTF-8, so this artifact's own encoding cannot carry it unchanged"))
		}
	}
	return nil
}

func checkResolutionStringsExpressible(a ResolutionArtifact) error {
	lossy := func(what string) error {
		return errors.Join(ErrResolutionDigest, errors.New("p4offline: "+what+
			" is not valid UTF-8, so this artifact's own encoding cannot carry it unchanged"))
	}
	if err := resolutionConstantStringsExpressible(a); err != nil {
		return err
	}
	for i, id := range a.OrderedOutcomeIDs {
		if !utf8.ValidString(id) {
			return lossy("ordered outcome id " + strconv.Itoa(i))
		}
	}
	for i, r := range a.EvidenceReferences {
		at := func(what string) error {
			return lossy("evidence reference " + strconv.Itoa(i) + " " + what)
		}
		switch {
		case !utf8.ValidString(r.ObservationID):
			return at("observation id")
		case !utf8.ValidString(r.CollectorSessionID):
			return at("collector session id")
		case !utf8.ValidString(r.Kind):
			return at("kind")
		case !utf8.ValidString(r.Phase):
			return at("phase")
		case !utf8.ValidString(r.RoundState):
			return at("round state")
		case !utf8.ValidString(r.EventID):
			return at("event id")
		}
	}
	for i, r := range a.Refusals {
		if !utf8.ValidString(r) {
			return lossy("refusal " + strconv.Itoa(i))
		}
	}
	return nil
}

func resolutionDigest(a ResolutionArtifact) string {
	return DigestReference(sha256Hex(SerializeResolutionArtifact(a)))
}

// VerifyResolutionArtifact refuses a foreign contract or obligations revision,
// then a digest that is not this package's shape, then a value the artifact's
// own encoding cannot express, then recomputes the digest -- and then it checks
// the DERIVATION: a WINNER_KNOWN or REFUND artifact must be exactly what
// projecting its own facts produces, and an UNKNOWN artifact must assert
// nothing. The value gate is ahead of the digest COMPARISON and below the
// digest's SHAPE gate; both placements carry a precedence shift, and each is
// recorded at the gate that carries it.
func VerifyResolutionArtifact(a ResolutionArtifact) error {
	// Same rule as VerifyCommonFactset's first gate: see suppliedTextExtent.
	if a.ContractVersion != ResolutionFactsDigestVersion {
		return errors.Join(ErrResolutionDigest, errors.New("p4offline: artifact contract is "+
			suppliedTextExtent(a.ContractVersion)+", this package writes only "+strconv.Quote(ResolutionFactsDigestVersion)))
	}
	if a.ObligationsRevision != ResolutionObligationsRevision {
		return errors.Join(ErrResolutionDigest, errors.New("p4offline: artifact obligations revision is "+
			suppliedTextExtent(a.ObligationsRevision)+", this package writes only "+strconv.Quote(ResolutionObligationsRevision)))
	}
	// AHEAD OF THE DIGEST COMPARISON -- and BELOW the digest's SHAPE gate,
	// which sits between this paragraph and the scan it documents. The heading
	// names the COMPARISON for that reason: only one of the two is below this
	// scan. For the reason checkFactsetValuesExpressible gives at the other
	// artifact: a string encoding/json cannot carry
	// UNCHANGED must not be hashed into a certificate. encoding/json marshals
	// invalid UTF-8 WITHOUT error by substituting U+FFFD, so the stored
	// artifact is a different value from the certified one and fails its own
	// digest when it is read back -- which is the signal this package reserves
	// for tampering. coverage_test.go round-trips this artifact through JSON in
	// the same test that round-trips a factset, so that is a supported path.
	//
	// This seam was found by a review lane AFTER the same repair had been made
	// to the factset and this file's WHAT REMAINS note had been shortened to
	// say the class was closed. It was closed for one artifact of the four this
	// package verifies. Reproduced here before repair: a poked ChannelID, a
	// consistently-poked winner id, a non-winner ordered id and an evidence
	// reference's ObservationID each CERTIFIED, marshalled without error, and
	// failed their own digest after a round trip. A poked Round.EventID did
	// not, because the re-projection below re-derives the round identity and
	// catches it first -- so the hole was four positions wide, not five.
	//
	// THE DIGEST'S SHAPE IS CHECKED BEFORE THE ARTIFACT IS FRAMED. A
	// producer-impossible digest -- "x" -- used to reach the scan BELOW and the
	// full serialization below before the mismatch was found: AT THIS COMMIT'S
	// PARENT 0b3cd2f, 44 allocations and about 10.59 MB on a 20,000-reference
	// artifact, against 44 and the same 10.59 MB for a WELL-FORMED wrong
	// digest -- a constant-size malformed field amplified by the payload's
	// size. With this gate the same call is 4 allocations and 200 bytes. The
	// well-formed wrong digest reads 49 allocations here rather than 44,
	// because the comparison below now quotes both digests; it is the one
	// figure in this paragraph that moved, and it moved in the direction the
	// gate does not govern. The megabyte is given to four significant
	// figures because the byte-exact total is a property of the toolchain that
	// measured it, not of the amplification this sentence is about. The 200 is
	// this gate's own errors.Join. THE RATIO IS THE CLAIM: four allocations
	// against forty-odd. Neither the count nor the byte total is quoted as a
	// constant -- independent lanes reading the gated path get 4 every time and
	// the ungated path in the mid-forties, varying by a count or two between
	// runs and between estimators. No triple is written here, because three
	// readings from one host are a sample and a sample quoted as a constant is
	// something a later lane on another host has to spend its time refuting.
	// resolutionDigest emits a DigestReference, so anything else is refusable
	// in O(1). This is the same rule the two gates above the factset's digest
	// already follow, applied where a review lane found it missing.
	// THE REFUSAL NAMES THE FAULT AND THE EXTENT. This package holds a digest
	// to its shape in SIX places -- this one, the factset's, the registry's,
	// p3b.go's two (the declared native digest and the declared raw hash) and
	// checkEntropyCoordinates' reference gate -- and after this change every
	// one of the six names what it refused rather than returning the bare
	// sentinel: factset.go emits "factset digest of N bytes is not this
	// package's 64 lower-case hex digits", p3b.go "declared native digest is
	// not 64 lower-case hex digits", entropy.go "common factset digest is not
	// \"sha256:\" followed by 64 lower-case hex digits".
	// suppliedTextExtent's rule holds here as there: the FIELD and the EXTENT,
	// never the caller's text.
	//
	// What naming it buys is that a caller can tell a refusal from the SHAPE
	// gate from one by the comparison below it. The gate's POSITION is a
	// separate property and is pinned by behaviour, in
	// TestADigestsSHAPEIsJudgedAboveTheScanThatReadsTheSupply, not by this
	// message and not by cost.
	if !isDigestReference(a.ResolutionFactsDigest) {
		return errors.Join(ErrResolutionDigest, errors.New("p4offline: resolution facts digest of "+
			suppliedTextExtent(a.ResolutionFactsDigest)+" is not this package's "+
			strconv.Quote(DigestReferencePrefix)+" prefix and 64 lower-case hex digits"))
	}
	// TWO ARMS OF THE SWITCH BELOW DECIDE FROM CONSTANT-SIZE FIELDS, and the
	// scan and the framing under them are thrown away when either fires. An
	// adversarial review lane measured this: an eleven-byte out-of-vocabulary
	// Outcome cost 13,436,051 B/op on a 20,000-reference artifact -- 100% of a
	// FULL honest verification of the same artifact, and 62,204x the flat
	// ContractVersion gate in this same function. The digest gate above is
	// O(1) and the two arms below are O(1); nothing between them is. This is
	// the WORK half of canonical.go's rule, at a site this package had listed
	// only under the TEXT half.
	//
	// NEITHER ARM MAY SPEAK UNTIL THE ARTIFACT IS READABLE. This is the whole
	// deferral, asked once above both of them; see
	// resolutionConstantStringsExpressible for why it is not asked per-arm and
	// what the two attempts before it got wrong. It allocates nothing, so the
	// refusals below still cost what they were measured to cost.
	//
	// WHAT IT DOES NOT RESTORE is recorded rather than absorbed: the three
	// SLICE scans -- ordered outcome ids, evidence references, refusals --
	// remain below these arms, because walking them is the O(n) work the hoist
	// exists to avoid. So an artifact that is BOTH semantically wrong AND
	// unreadable inside one of those slices is now told the semantic thing,
	// where before the hoist it was told the encoding one. That residual is in
	// the deferred register.
	if err := resolutionConstantStringsExpressible(a); err != nil {
		return err
	}
	if a.Outcome != ResolutionWinnerKnown && a.Outcome != ResolutionRefund &&
		a.Outcome != ResolutionUnknown {
		return errors.Join(ErrResolutionNotDerivable, errors.New("p4offline: outcome of "+
			suppliedTextExtent(string(a.Outcome))+" is outside the vocabulary"))
	}
	if a.Outcome == ResolutionUnknown && (a.WinnerOutcomeID != "" || a.WinnerIndex != -1) {
		return errors.Join(ErrResolutionNotDerivable, errors.New("p4offline: an UNKNOWN artifact names a winner"))
	}
	if err := checkResolutionStringsExpressible(a); err != nil {
		return err
	}
	if want := resolutionDigest(a); a.ResolutionFactsDigest != want {
		// BOTH DIGESTS ARE NAMED, on the same precondition as the two sibling
		// comparisons: the shape gate above has already proved this value a
		// DigestReference, so quoting it cannot render a caller's payload, and
		// reporting it by extent could only ever render the constant "71
		// bytes". Naming both is what lets a caller tell "you sent the wrong
		// digest" from "your artifact is not what you digested"; naming
		// neither, which this refusal used to do, says only that they differ.
		return errors.Join(ErrResolutionDigest, errors.New("p4offline: resolution facts digest "+
			strconv.Quote(a.ResolutionFactsDigest)+" does not match the artifact, which digests to "+
			strconv.Quote(want)))
	}
	switch a.Outcome {
	case ResolutionWinnerKnown, ResolutionRefund:
		re := ProjectResolution(ResolutionEvidence{
			Round:              a.Round,
			OrderedOutcomeIDs:  a.OrderedOutcomeIDs,
			Claim:              a.Outcome,
			WinnerOutcomeID:    a.WinnerOutcomeID,
			ProofBasis:         a.ProofBasis,
			EvidenceReferences: a.EvidenceReferences,
			Availability:       a.Availability,
			ProjectorRevision:  a.ProjectorRevision,
			ProofRevision:      a.ProofRevision,
		})
		if re.ResolutionFactsDigest != a.ResolutionFactsDigest {
			return errors.Join(ErrResolutionNotDerivable,
				errors.New("p4offline: re-projecting the artifact's facts yields "+string(re.Outcome)+
					" with refusals "+joinReasons(re.Refusals)))
		}
	case ResolutionUnknown:
		// UNREACHABLE BY CONSTRUCTION, and kept anyway. The gate above this
		// switch refuses the same shape before the framing, so no input
		// arrives here: a mutation campaign confirms it, because a mutant
		// adding observable work inside this arm survives the whole suite,
		// and a panic installed here leaves it green. It stays because a
		// switch on a string that falls through its cases returns nil, and a
		// fail-closed backstop is worth more than a killable mutant.
		if a.WinnerOutcomeID != "" || a.WinnerIndex != -1 {
			return errors.Join(ErrResolutionNotDerivable, errors.New("p4offline: an UNKNOWN artifact names a winner"))
		}
	default:
		// UNREACHABLE BY CONSTRUCTION, for the reason stated at the UNKNOWN
		// arm above: an out-of-vocabulary Outcome is refused by the hoisted
		// gate when it is expressible and by the expressibility scan when it
		// is not, so the two together leave this arm nothing to catch.
		return errors.Join(ErrResolutionNotDerivable, errors.New("p4offline: outcome of "+
			suppliedTextExtent(string(a.Outcome))+" is outside the vocabulary"))
	}
	return nil
}

// joinReasons renders a reason list in ONE pass.
//
// It used to accumulate with `out += r`, which copies the whole prefix on every
// iteration: quadratic in a list nothing bounds. An independent security lane
// reached it through the exported EvaluateP2Case with a factset this package's
// own verifier accepts, and measured 1,018,297,776 bytes at 1,000 reasons
// rising to 64,420,331,144 at 8,000 -- exactly 4x the allocation per 2x the
// input. Reproduced here at 4,097 reasons: 1,266,167,840 bytes to refuse
// 297,913 bytes of input.
//
// The size is computed first and the buffer allocated once. strings.Join would
// say this in one line, but `strings` is not on the purity allowlist and
// bringing it in to save four lines is not a trade this package makes; the
// dependency fence exists to be inconvenient here.
//
// This removes the quadratic factor at every call site. It does NOT bound the
// result, so a caller-controlled list would still be rendered in full -- which
// is why the one site reached with unbounded caller content reports a count and
// a total instead of calling this at all. See EvaluateP2Case.
//
// WHY THE ACCUMULATION HALF IS NOT SEPARATELY TESTABLE, and where the first
// version of this argument was wrong. Restoring the `+=` accumulation leaves
// the suite green, because no remaining caller hands this a list whose MEMBERS
// are unbounded: resolution.go's refusals and the episode exclusion reasons
// both go through appendOnce over closed vocabularies, and the COMPLETE-beside
// check passes at most six derived reasons.
//
// The first version of that list also claimed sessionRefusals' members are all
// package constants. They are: but its SESSION_P2_EXCLUSION members are emitted
// once per excluded record with a plain append, so the list is unbounded in
// COUNT even though every member is short -- an independent lane built a
// 20,000-record foreign-session dataset and got an 840,098-byte refusal. That
// is sub-proportional to the input rather than an amplification, so it is not a
// blowup; it is a refusal that renders the caller's data, which is the thing
// this package decided it does not do. That site now names the extent and the
// distinct KINDS, and the claim here is narrowed to what it can carry.
//
// The empty-list guard below is a separate matter and IS testable: without it
// `make([]byte, 0, -1)` panics. It has its own case.
//
// The quadratic is removed because a future caller should not have to
// rediscover it, not because a current one reaches it.
func joinReasons(rs []string) string {
	if len(rs) == 0 {
		return ""
	}
	n := len(rs) - 1
	for _, r := range rs {
		n += len(r)
	}
	out := make([]byte, 0, n)
	for i, r := range rs {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, r...)
	}
	return string(out)
}

// reasonListExtent names a reason list by its COUNT and its TOTAL BYTES,
// without rendering it. It is the rule rulesetIdentityFault, ValidateDrawTrace
// and bindEntropyCoordinates already follow, applied to a list instead of a
// string: a refusal names the fault, never the input it is refusing.
func reasonListExtent(rs []string) string {
	n := 0
	for _, r := range rs {
		n += len(r)
	}
	return strconv.Itoa(len(rs)) + " reasons, " + strconv.Itoa(n) + " bytes"
}

// ExtractResolutionReferences names the facts of one round a resolution could
// be established from — the settlement and lifecycle facts — as references.
// It extracts NO winner: the P1 mirror this package reads carries none.
func ExtractResolutionReferences(ds predictioneval.SourceDataset, round PublicRoundIdentity) []EvidenceReference {
	var out []EvidenceReference
	if round.EventID == "" {
		return nil
	}
	for i := range ds.Records {
		r := &ds.Records[i]
		if r.EventID != round.EventID {
			continue
		}
		if r.Kind != predictioneval.KindUserTerminal && r.Kind != kindChannelEvent {
			continue
		}
		out = append(out, EvidenceReference{
			ObservationID:      r.ObservationID,
			CollectorEpoch:     r.CollectorEpoch,
			CollectorSessionID: r.CollectorSessionID,
			Kind:               r.Kind,
			Phase:              r.Payload.Phase,
			RoundState:         r.Payload.RoundState,
			EventID:            r.EventID,
		})
	}
	return out
}

// ResolutionNotRecorded is the honest artifact for evidence that carries no
// winner: UNKNOWN, NOT_RECORDED_IN_P1_MIRROR, with the references kept.
func ResolutionNotRecorded(round PublicRoundIdentity, orderedOutcomeIDs []string, refs []EvidenceReference, projectorRevision string) ResolutionArtifact {
	return ProjectResolution(ResolutionEvidence{
		Round:              round,
		OrderedOutcomeIDs:  orderedOutcomeIDs,
		Claim:              ResolutionUnknown,
		EvidenceReferences: refs,
		Availability:       AvailabilityNotRecorded,
		ProjectorRevision:  projectorRevision,
		ProofRevision:      "none:" + string(AvailabilityNotRecorded),
	})
}
