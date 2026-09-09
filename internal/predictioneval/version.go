package predictioneval

// Pinned contract identities.
//
// Three DIFFERENT revisions meet in this package and must never be collapsed
// into one another:
//
//   - the PRODUCER revision, which names the contract the persisted facts were
//     written under. It belongs to the store, it is already committed to disk,
//     and this package only ever compares against it. It is never restamped
//     with the SHA of whatever commit happens to be replaying — doing that
//     would relabel historical rows with a build that did not write them.
//   - the POLICY revision, which names the betting code this model
//     reimplements. It is the second half of the producer revision because
//     that is exactly what the producer pinned.
//   - the MODEL version, which is this package's OWN. It moves when the replay
//     model changes, independently of both of the above, so a stored scorecard
//     says which model produced it.
const (
	// ModelVersion is this replay model's own version. It appears on every
	// [Evaluation] and [Scorecard] so a result can be attributed to the model
	// that produced it rather than to the data it read.
	ModelVersion = "predictioneval/v1"

	// SupportedPayloadVersion is the ONLY payload version whose decision
	// envelope this model reads. The store hashes this constant into every
	// row's digest from its compile-time value, so it does not move when a
	// field is appended; a payload claiming any other version is refused
	// rather than read under assumptions that may not hold for it.
	SupportedPayloadVersion = 1

	// SupportedProducerRevision is the producer contract this model replays:
	// the first revision that records what a decision was computed FROM.
	//
	// It is written as a LITERAL rather than taken from the analytics package,
	// even though the two must agree. Importing the constant would make this
	// reader follow a producer bump silently and start replaying facts written
	// under a contract nobody checked. The literal turns that into a loud test
	// failure instead — see the reader package's pinned-contract-identity test — which
	// is a decision for a human, not a default.
	SupportedProducerRevision = "obs-v2|policy-378d05d6ccc7d2a914730a1e1d023ff754bcf873"

	// PolicyRevision names the pinned betting policy this model reimplements:
	// internal/models/bet.go as of that commit. It is the producer revision's
	// policy half, and it is what a [Scorecard] means by "the configured
	// policy".
	PolicyRevision = "policy-378d05d6ccc7d2a914730a1e1d023ff754bcf873"

	// LegacyProducerRevisionPrefix identifies the pre-envelope contract. Facts
	// written under it stay READABLE — that is the whole point of a trail that
	// survives an upgrade — but they carry no decision envelope, and this
	// model never manufactures one for them from defaults, from the current
	// configuration, or from a neighbouring fact.
	LegacyProducerRevisionPrefix = "obs-v1"

	// CommonInputDigestVersion versions the canonical encoding used for a
	// case's common-input digest. It is this package's own artifact and is
	// deliberately NOT the store's row digest: that one witnesses a row
	// against tampering, this one witnesses that a replay's inputs did not
	// change when later facts were appended.
	CommonInputDigestVersion = "pe-cid/v1"

	// PinnedMinimumStake is Twitch's minimum accepted prediction stake as the
	// pinned policy applied it: internal/pubsub/pool.go's minPredictionBet.
	//
	// It is duplicated here because the production constant is unexported and
	// this package may not import the pubsub pool. The duplication is held to
	// the original by TestPinnedMinimumStakeMatchesTheProductionConstant, which
	// lives in the pubsub test binary — the only place that can see both.
	PinnedMinimumStake = 10

	// PinnedMaxOutcomes mirrors the store's frozen ceiling on how many
	// outcomes one fact may describe (analytics.MaxObservationOutcomes). A
	// case whose vector exceeds it could never have been stored whole, so it
	// is refused rather than replayed as if it were complete.
	PinnedMaxOutcomes = 64
)

// SourceProvenance is where a replayed case came from, carried through every
// stage so a [Scorecard] can name its own evidence.
//
// It is deliberately separate from [ModelProvenance]: one describes the DATA,
// the other describes the CODE that read it, and conflating them is how a
// result ends up claiming a guarantee neither of them gives.
type SourceProvenance struct {
	// CollectorEpoch / CollectorSessionID identify the collector run that
	// wrote the facts.
	CollectorEpoch     int64  `json:"collectorEpoch"`
	CollectorSessionID string `json:"collectorSessionId"`
	// ProducerRevision is the contract the facts were written under, verbatim
	// as stored. A revision this model does not support does not become an
	// error here — it becomes an explicit exclusion reason downstream.
	ProducerRevision string `json:"producerRevision"`
	// SessionReading is the store's own classification of the session
	// (AS_FINALIZED, UNFINALIZED, INTEGRITY_ERROR, ADMINISTRATIVELY_TRUNCATED),
	// carried verbatim. This model reuses that verdict rather than re-deriving
	// one, and never treats AS_FINALIZED as proof that any individual decision
	// case is complete.
	SessionReading string `json:"sessionReading"`
	// SessionDetail is the store's closed explanation of a non-AS_FINALIZED
	// reading.
	SessionDetail string `json:"sessionDetail,omitempty"`
	// CloseState is the session's stored close state.
	CloseState string `json:"closeState"`
	// WitnessesVerified / WitnessesUnchecked are how many of the session's
	// surviving facts actually had their stored digest recomputed. An
	// unchecked remainder is reported, never rounded up to "verified".
	WitnessesVerified  int64 `json:"witnessesVerified"`
	WitnessesUnchecked int64 `json:"witnessesUnchecked"`
	// FactsPresent / CommittedCount let a reader see truncation directly
	// rather than inferring it.
	FactsPresent   int64 `json:"factsPresent"`
	CommittedCount int64 `json:"committedCount"`
	DroppedCount   int64 `json:"droppedCount"`
}

// ModelProvenance is the code identity behind a result.
type ModelProvenance struct {
	ModelVersion   string `json:"modelVersion"`
	PolicyRevision string `json:"policyRevision"`
	// SupportedProducerRevision is what this build binds to, recorded so a
	// stored result can be compared against the data it claims to explain.
	SupportedProducerRevision string `json:"supportedProducerRevision"`
	// PlatformIntBits is the width of Go's int on the machine that produced
	// this result. The pinned policy computes stakes in int, so a result is
	// only portable between machines of the same width — saying so is the
	// point of recording it.
	PlatformIntBits int `json:"platformIntBits"`
}

// platformIntBits is 32 or 64 without importing anything: ^uint(0)>>63 is 1
// exactly when uint is 64 bits wide.
const platformIntBits = 32 << (^uint(0) >> 63)

// CurrentModelProvenance returns the code identity of this build.
func CurrentModelProvenance() ModelProvenance {
	return ModelProvenance{
		ModelVersion:              ModelVersion,
		PolicyRevision:            PolicyRevision,
		SupportedProducerRevision: SupportedProducerRevision,
		PlatformIntBits:           platformIntBits,
	}
}
