package predictioneval

// THE ORDERED-RULES CORE: types.
//
// This file adds a SECOND, SEPARATE pure model beside the current-configured
// replay the rest of this package performs. The two never mix:
//
//   - the existing four seams (MaterializePairedKnowledge → ProjectDecisionCase
//     → Evaluate → Score) reconstruct THE STRATEGY EACH RECORDED DECISION WAS
//     ACTUALLY RUNNING UNDER, from facts this miner persisted;
//   - the ordered-rules core here re-derives a DIFFERENT, DONOR mechanism —
//     ordered odds rules with a per-rule participation rate and a percentage
//     stake — over data the CALLER supplies explicitly.
//
// Nothing here reads the store, and nothing here feeds the baseline. A result
// produced by this model carries [OrderedRulesEvidenceLabel] and means exactly
// what that label says: the mechanism was evaluated over the declared common
// admitted data, and over nothing else.
//
// # SUPPLIED is not PROVEN
//
// Every value that reaches this model does so because a caller passed it in.
// A pure function cannot authenticate a number it is handed, so it does not
// pretend to: provenance travels with each field, an absent value stays
// absent, and a fixture never becomes a production fact by being evaluated.
// That is the whole reason presence is a first-class part of these types
// rather than a nil pointer someone can read as a zero.
//
// # What this model deliberately is not
//
// It is NOT a faithful replay of the donor's full runtime policy. The donor
// branches on lock/end timestamps, refreshes its balance against a live API,
// re-enters on every round update until a placement succeeds, and retries
// after a failed call. None of those transitions is established by anything
// this package can read, so none of them is modelled and none is guessed.
// See [OrderedRulesEvidenceLabel].

// Contract identities for the ordered-rules core.
//
// These are deliberately INDEPENDENT of [ModelVersion], [CommonInputDigestVersion]
// and [SupportedProducerRevision]. Those name the baseline replay and the
// producer that wrote this miner's own rows; this model reads neither, so
// reusing their versions would attach a claim about persisted facts to a
// result computed entirely from supplied values.
const (
	// OrderedRulesModelVersion is this model's own version, moving
	// independently of the baseline's [ModelVersion].
	OrderedRulesModelVersion = "predictioneval-orderedrules/v1"

	// OrderedRulesStreamContractVersion versions the shape of the supplied
	// source and the projection this package derives from it.
	OrderedRulesStreamContractVersion = "pe-ors/v1"

	// OrderedRulesConfigBasisVersion names the basis the exported config is
	// accepted in: RAW percentages in 0..100, normalized privately exactly
	// once. An already-normalized config is NOT an accepted input, because the
	// two are indistinguishable by inspection and normalizing twice turns a
	// 50% participation rate into 0.5%.
	OrderedRulesConfigBasisVersion = "pe-orc-raw/v1"

	// OrderedRulesEntropySemanticsVersion names the exact Bernoulli semantics
	// the supplied raw words are consumed under. A trace declaring anything
	// else is refused rather than reinterpreted.
	OrderedRulesEntropySemanticsVersion = "rand-0.8.5-bernoulli/v1"

	// OrderedRulesEvidenceLabel is the ONLY claim a result of this model
	// carries: the mechanism, evaluated over the declared common-admitted
	// data. It is not a claim about the donor's full runtime policy, about
	// this miner's production data, or about profitability.
	OrderedRulesEvidenceLabel = "CORE_MECHANISM_COMMON_ADMITTED_DATA"

	// OrderedRulesDonorRevision pins the mechanism being modelled, exactly.
	// The donor's own tree hash is recorded beside it because a commit can be
	// re-pointed by a force push while a tree cannot.
	OrderedRulesDonorRevision = "t348575/twitch-points-miner@ae9d39d3d10ff8240f24e2529fe7cf6dd701db66"
	// OrderedRulesDonorTree is that commit's tree.
	OrderedRulesDonorTree = "5e781ae8b1e4f28427dcd8ca790c8e9a7afbcf49"

	// OrderedRulesEntropyRevision pins the source the Bernoulli semantics were
	// read from: rand 0.8.5, at the immutable commit its tag resolves to.
	OrderedRulesEntropyRevision = "rust-random/rand@937320cbfeebd4352a23086d9c6e68f067f74644"
)

// Offline resource bounds.
//
// These are THIS PACKAGE'S limits on what it will evaluate in one call. They
// are not Twitch limits, they are not the donor's limits, and they describe no
// property of any real round: they exist so a supplied input cannot make a
// pure function allocate or spin without bound.
//
// Every one of them is a REFUSAL boundary, never a truncation boundary. An
// input past a limit is rejected whole — see
// TestOrderedRulesOverBudgetInputIsRefusedWithoutTruncation — because a
// silently shortened candidate list changes which attempt opportunities exist
// and would present a partial traversal as a complete one.
const (
	// MaxOrderedRulesCandidates bounds the supplied candidate stream.
	MaxOrderedRulesCandidates = 128
	// MaxOrderedRulesOutcomes bounds one candidate's outcome vector. It equals
	// [PinnedMaxOutcomes] by coincidence of value, not by derivation: this one
	// is an offline evaluation limit, that one mirrors the store's frozen
	// ceiling, and they are free to move apart.
	MaxOrderedRulesOutcomes = 64
	// MaxOrderedRulesRules bounds the ordered detailed rule list.
	MaxOrderedRulesRules = 128
	// MaxOrderedRulesInterventions bounds the supplied factual boundary markers.
	MaxOrderedRulesInterventions = 1024
	// MaxOrderedRulesQualifications bounds a projected stream's qualification
	// list. The projection itself emits at most five, so this is headroom
	// rather than a limit anything real approaches — it exists because
	// [EvaluateOrderedRules] is exported and must bound a stream it did not
	// produce.
	MaxOrderedRulesQualifications = 64
	// MaxOrderedRulesSourceReferences bounds the admission manifest's source
	// list, which the projection retains and copies.
	//
	// Every other supplied collection is bounded by COUNT before its bytes are
	// charged; this one was bounded only by the aggregate byte budget, and that
	// budget charges payload. Empty strings have no payload, so any number of
	// them passed — while each still costs a string header to retain, copy and
	// digest. The bound restores the file's own rule rather than adding a new
	// one, and with a count this small the uncharged header cost is a few tens
	// of kilobytes against [MaxOrderedRulesAggregateBytes].
	MaxOrderedRulesSourceReferences = 1024
	// MaxOrderedRulesIdentifierBytes bounds one supplied identifier, and every
	// other free-text string the projection retains — a presence reason, a
	// provenance note, a coverage detail. The caller chooses those freely, so
	// leaving them unbounded would leave the aggregate budget below
	// unenforceable in exactly the field most under a caller's control.
	MaxOrderedRulesIdentifierBytes = 4096
	// MaxOrderedRulesDrawWords bounds the supplied raw entropy trace.
	MaxOrderedRulesDrawWords = 1 << 20
	// MaxOrderedRulesWork bounds predicate and draw slots actually evaluated in
	// one traversal. The other bounds together permit more work than this, so
	// this is a live counter rather than a precomputable product: a run that
	// stops early is never charged for work it did not do.
	//
	// It is 2^18 rather than the 2^20 an offline slot ceiling alone would
	// allow, and the difference is not arbitrary: EVERY evaluated slot also
	// appends one [OrderedRulesTraceEntry], which is 120 bytes, so the slot
	// ceiling and the trace length are the same quantity measured twice. At
	// 2^20 a single call would retain 120 MiB of trace — plus the discarded
	// arrays append leaves behind on the way there — from an input sitting
	// inside every other declared bound and consuming no entropy at all. That
	// is exactly the unbounded allocation these limits exist to prevent, so
	// the ceiling is narrowed until the trace fits the budget below with room
	// for append's doubling. Narrowing a bound is always allowed; widening one
	// past what [MaxOrderedRulesAggregateBytes] permits is not.
	MaxOrderedRulesWork = 1 << 18
	// MaxOrderedRulesAggregateBytes bounds the supplied identifier bytes of one
	// source, matching the reader's existing aggregate ceiling rather than
	// inventing a looser one. It also fixes [MaxOrderedRulesWork], since the
	// trace a traversal retains is one entry per evaluated slot.
	MaxOrderedRulesAggregateBytes = 128 << 20
)

// SuppliedPresence separates a value that was supplied from one that was not.
//
// The distinction is load-bearing and cannot be carried by a zero: every
// numeric field in this model has a legitimate zero. A pool outcome really can
// hold zero points, and a balance really can be zero — so a missing value read
// as a zero would be a fabricated input, not a conservative one.
type SuppliedPresence string

const (
	// SuppliedKnown is a value the caller supplied together with its provenance.
	SuppliedKnown SuppliedPresence = "KNOWN"
	// SuppliedMissing is a value the caller declared it does not have. It is
	// never back-filled from a neighbouring candidate, from a later fact, or
	// from a default.
	SuppliedMissing SuppliedPresence = "MISSING"
	// SuppliedInvalid is a value the caller supplied and declared unusable —
	// out of domain, undecodable, or contradicted. It is distinct from MISSING
	// because "we know it was wrong" and "we do not have it" are different
	// pieces of evidence.
	SuppliedInvalid SuppliedPresence = "INVALID"
)

// SuppliedInt64 is one supplied integer with its own presence and provenance.
//
// AvailableAtPosition is the causal position at which the value became
// knowable. It exists to make back-dating checkable rather than trusted: a
// value that only became available AFTER the candidate it is attached to
// cannot have been read when that candidate was current, and the projection
// refuses it instead of quietly using it. See
// TestOrderedRulesLaterBalanceCannotRepairEarlierCandidate.
type SuppliedInt64 struct {
	Presence SuppliedPresence `json:"presence"`
	Value    int64            `json:"value"`
	// Reason is the caller's closed explanation of a non-KNOWN presence. It is
	// carried into the result verbatim so a stop can say WHY, not just that.
	Reason string `json:"reason,omitempty"`
	// Provenance is where a KNOWN value came from, in the caller's own terms.
	Provenance string `json:"provenance,omitempty"`
	// AvailableAtPosition is required for a KNOWN value, and
	// HasAvailableAtPosition is how the caller says it supplied one.
	//
	// The flag is not ceremony. Position zero is a legitimate causal position,
	// so a bare int64 cannot separate "available from the very start" from
	// "never supplied" — and an omitted field decoded as zero would pass the
	// back-dating check for every candidate at position zero or later, which
	// is the whole interval in any stream that starts at zero. That is exactly
	// the fabricated-input failure [SuppliedPresence] exists to prevent, one
	// level down. A KNOWN value that does not set the flag is refused, the same
	// way a KNOWN value carrying no provenance is.
	AvailableAtPosition    int64 `json:"availableAtPosition"`
	HasAvailableAtPosition bool  `json:"hasAvailableAtPosition"`
}

// SuppliedUint32 is a stake-shaped result value with its own presence.
//
// The donor computes stakes in u32. This model keeps that width rather than
// widening to Go's int, so a stake computed here means the same number the
// donor would have produced on any machine.
type SuppliedUint32 struct {
	Presence SuppliedPresence `json:"presence"`
	Value    uint32           `json:"value"`
	Reason   string           `json:"reason,omitempty"`
}

// OrderedRulesCoverage is the caller's declaration of how complete the supplied
// interval is.
//
// It is not cosmetic. A gap or a lost prefix can hide an EARLIER intervention,
// and an earlier intervention would have cut the stream shorter — so an
// incomplete interval can never be read as proof that no intervention
// happened. The qualification therefore travels all the way into the result.
type OrderedRulesCoverage string

const (
	// CoverageCompleteDeclared is the caller declaring the interval carries
	// every relevant fact it contained. This is a declaration, not a proof.
	CoverageCompleteDeclared OrderedRulesCoverage = "COMPLETE_DECLARED"
	// CoverageGapsPresent is a declared interval with known holes.
	CoverageGapsPresent OrderedRulesCoverage = "GAPS_PRESENT"
	// CoverageTruncatedPrefix is a declared interval whose beginning is missing
	// — the shape most able to hide an earlier intervention.
	CoverageTruncatedPrefix OrderedRulesCoverage = "TRUNCATED_PREFIX"
	// CoverageUnknown is the caller declining to characterise coverage at all.
	// It is REFUSED: an unstated coverage is exactly the case where "no
	// intervention was supplied" cannot be distinguished from "the
	// intervention was not collected".
	CoverageUnknown OrderedRulesCoverage = "UNKNOWN"
)

// OrderedRulesSourceKind names what a candidate is a snapshot of.
type OrderedRulesSourceKind string

const (
	// SourceKindChannelUpdate is a candidate derived from an observed channel
	// frame — a round created or updated.
	SourceKindChannelUpdate OrderedRulesSourceKind = "CHANNEL_UPDATE"
	// SourceKindCalculateSnapshot is a candidate derived from a decision-time
	// model snapshot. Such a candidate is the local model's view, NOT a proven
	// wire frame, and a stream built only from these says so through
	// [ViewCalculateOnly] rather than calling itself a complete update stream.
	SourceKindCalculateSnapshot OrderedRulesSourceKind = "CALCULATE_SNAPSHOT"
)

// OrderedRulesMembership is whether a candidate's membership in the declared
// episode was PROVEN by the caller or merely assumed.
//
// Assumed membership is refused. Joining rounds across accounts, pools or
// reconnections by a shared event id or a nearby timestamp is exactly the
// error that would silently manufacture attempt opportunities; see
// TestOrderedRulesRoundAndSourceEpisodeCannotBeJoinedByEventIDAlone.
type OrderedRulesMembership string

const (
	// MembershipProven is the caller asserting a demonstrated association.
	MembershipProven OrderedRulesMembership = "PROVEN"
	// MembershipUnknown is refused by the projection.
	MembershipUnknown OrderedRulesMembership = "UNKNOWN"
)

// OrderedRulesInterventionKind is the sort of factual intervention that bounds
// the stream.
//
// Both kinds cut. A manual placement carries no attempt discriminator and may
// carry a different or empty round incarnation, so a boundary search that
// looked only for automatic attempts would miss a real intervention and let
// the model keep evaluating a world that had already been acted on. See
// TestOrderedRulesManualCallStartedWithoutAttemptIDCutsTheStream.
type OrderedRulesInterventionKind string

const (
	// InterventionAutoCallStarted is an automatic placement call having started.
	InterventionAutoCallStarted OrderedRulesInterventionKind = "AUTO_CALL_STARTED"
	// InterventionManualCallStarted is a manual placement call having started.
	InterventionManualCallStarted OrderedRulesInterventionKind = "MANUAL_CALL_STARTED"
)

// OrderedRulesRelevance is whether an intervention was PROVEN to belong to the
// declared episode.
type OrderedRulesRelevance string

const (
	// RelevanceProven is a demonstrated association: this call acted on this
	// episode.
	RelevanceProven OrderedRulesRelevance = "PROVEN_RELEVANT"
	// RelevanceAmbiguous is an intervention whose association could not be
	// established either way. It still cuts, at its own position, because the
	// conservative boundary is the earlier one — choosing the later, more
	// convenient boundary is how a model keeps evaluating past a real
	// intervention.
	RelevanceAmbiguous OrderedRulesRelevance = "AMBIGUOUS_ASSOCIATION"
	// RelevanceExcluded is an intervention PROVEN to belong elsewhere.
	RelevanceExcluded OrderedRulesRelevance = "PROVEN_UNRELATED"
)

// OrderedRulesScope is the declared identity and extent of a supplied source.
//
// AccountContext is the caller's own description of the account context it
// established the association under. It is deliberately a caller-supplied
// STRING and this package never derives, collects or persists an account
// identity of its own: nothing in this model needs to know whose account it is,
// only that the caller proved the candidates and the calls came from the same
// one.
type OrderedRulesScope struct {
	Namespace             string               `json:"namespace"`
	EpisodeID             string               `json:"episodeId"`
	AccountContext        string               `json:"accountContext"`
	AssociationEvidence   string               `json:"associationEvidence"`
	SourceContractVersion string               `json:"sourceContractVersion"`
	Coverage              OrderedRulesCoverage `json:"coverage"`
	CoverageDetail        string               `json:"coverageDetail,omitempty"`
	// IntervalFromPosition / IntervalToPosition bound the declared interval,
	// inclusive. Every supplied candidate and intervention must lie inside it:
	// a fact outside the interval the caller declared is a contradiction in the
	// input, not a fact to silently include.
	IntervalFromPosition int64 `json:"intervalFromPosition"`
	IntervalToPosition   int64 `json:"intervalToPosition"`
}

// OrderedRulesOutcome is one entry of a candidate's ORDERED outcome vector.
//
// Order is the vector's own order and is never re-derived. The donor iterates
// the outcome vector exactly as it arrived, so sorting by odds, by colour or by
// id would change which outcome wins the traversal.
type OrderedRulesOutcome struct {
	Identity string        `json:"identity"`
	Points   SuppliedInt64 `json:"points"`
}

// OrderedRulesCandidate is one supplied opportunity: the pool as it stood, the
// balance if the caller has it, and where in the episode it sat.
type OrderedRulesCandidate struct {
	// Identity is unique within the source. Two candidates with the same
	// identity are an input conflict and are refused rather than deduplicated:
	// silently dropping one removes an attempt opportunity, silently keeping
	// both invents one. See TestOrderedRulesDuplicateIdentityIsRejected.
	Identity string `json:"identity"`
	// Position is the strict causal position. Positions must be strictly
	// increasing across the supplied slice; equal positions mean the caller
	// does not actually know the order, and an arbitrary tie-break would hide
	// that. See TestOrderedRulesAmbiguousCausalPositionsAreNotSortedAway.
	Position          int64                  `json:"position"`
	SourceKind        OrderedRulesSourceKind `json:"sourceKind"`
	EpisodeMembership OrderedRulesMembership `json:"episodeMembership"`
	// OutcomesPresence declares whether the caller RECOVERED this candidate's
	// ordered outcome vector whole.
	//
	// It exists because every scalar in this model carries a presence and the
	// vector did not, which made two completely different things identical: a
	// pool the donor genuinely declines because it holds fewer than two
	// outcomes, and a pool whose vector the caller could not recover. The first
	// is a decision and the traversal walks on; the second is an unknown, and
	// walking past it hands every LATER candidate an attempt opportunity that
	// exists only because this one was skipped.
	//
	// A short vector is only the donor's decline when this says KNOWN. Anything
	// else stops the traversal. A partially recovered vector must be declared
	// INVALID rather than passed off as complete: the pool total feeds every
	// share, so a missing entry moves all of them.
	OutcomesPresence SuppliedPresence `json:"outcomesPresence"`
	// OutcomesReason is the caller's closed explanation of a non-KNOWN vector.
	OutcomesReason string                `json:"outcomesReason,omitempty"`
	Outcomes       []OrderedRulesOutcome `json:"outcomes"`
	// Balance is the balance bound TO THIS CANDIDATE. A balance is never
	// forward-filled from an earlier candidate, borrowed from a later one, or
	// taken from a neighbouring record because the timestamps looked close.
	Balance    SuppliedInt64 `json:"balance"`
	Provenance string        `json:"provenance,omitempty"`
}

// OrderedRulesIntervention is a factual placement call having STARTED.
//
// It carries no result, and that absence is deliberate. Whether the call
// succeeded, what it staked and what it returned are facts about the world that
// actually happened; the model is evaluating a world in which it had not yet
// acted, and after a real action that world no longer exists. Even a call that
// FAILED is a boundary — see TestOrderedRulesFailedFactualCallStillCutsOff.
type OrderedRulesIntervention struct {
	Identity  string                       `json:"identity"`
	Position  int64                        `json:"position"`
	Kind      OrderedRulesInterventionKind `json:"kind"`
	Relevance OrderedRulesRelevance        `json:"relevance"`
	Detail    string                       `json:"detail,omitempty"`
}

// OrderedRulesSource is the explicitly supplied input to the projection.
//
// It is DATA, not a collector, a reader or a callback. Handing this package a
// source is how the database, the network and the clock stay on the other side
// of the dependency fence.
type OrderedRulesSource struct {
	Scope         OrderedRulesScope          `json:"scope"`
	Candidates    []OrderedRulesCandidate    `json:"candidates"`
	Interventions []OrderedRulesIntervention `json:"interventions"`
}

// OrderedRulesViewKind names which view of the common-admitted population the
// caller selected.
type OrderedRulesViewKind string

const (
	// ViewChannelCandidateStream is a stream of observed channel candidates.
	ViewChannelCandidateStream OrderedRulesViewKind = "CHANNEL_CANDIDATE_STREAM"
	// ViewCalculateOnly is a stream assembled only from decision-time model
	// snapshots. It is a legitimate, explicitly named slice — and it is NOT a
	// recovered create/update stream. Labelling it as one would silently narrow
	// a promised full stream to the subset that happens to be recoverable.
	ViewCalculateOnly OrderedRulesViewKind = "CALCULATE_ONLY"
)

// CommonAdmission is the caller's declarative manifest of the common-admitted
// population, the view taken of it, and the order it was taken in.
//
// It is a MANIFEST — plain data. It is deliberately not a callback, a scheduler
// or a registry, because anything executable here would let admission depend on
// what the model went on to decide. Admission is fixed before evaluation and
// cannot be chosen for producing a better answer.
type CommonAdmission struct {
	ManifestID       string               `json:"manifestId"`
	ViewKind         OrderedRulesViewKind `json:"viewKind"`
	Population       string               `json:"population"`
	OrderBasis       string               `json:"orderBasis"`
	SourceReferences []string             `json:"sourceReferences,omitempty"`
}

// OrderedRuleComparator is the donor's two-valued odds comparison.
type OrderedRuleComparator string

const (
	// ComparatorLe admits a pool share at or BELOW the threshold: p <= t.
	ComparatorLe OrderedRuleComparator = "Le"
	// ComparatorGe admits a pool share at or ABOVE the threshold: p >= t.
	ComparatorGe OrderedRuleComparator = "Ge"
)

// OrderedRulesPoints is the donor's stake sizing for one rule.
//
// MaxValue is a POINT CAP in the donor's u32 stake domain, not a percentage.
// MaxValue == 0 means "no cap" — it does not mean "stake nothing".
type OrderedRulesPoints struct {
	MaxValue uint32 `json:"maxValue"`
	// RawPercent is a raw percentage in 0..100.
	RawPercent float64 `json:"rawPercent"`
}

// OrderedRule is one entry of the ORDERED detailed rule list.
type OrderedRule struct {
	Comparator OrderedRuleComparator `json:"comparator"`
	// RawThresholdPercent is a raw percentage in 0..100.
	RawThresholdPercent float64 `json:"rawThresholdPercent"`
	// RawAttemptRatePercent is a raw percentage in 0..100 and controls
	// PARTICIPATION ONLY. It is not stake sizing: a rule can admit at 5% and
	// still stake the full percentage when it does admit. Raw 1.0 means one
	// percent, not one hundred.
	RawAttemptRatePercent float64            `json:"rawAttemptRatePercent"`
	Points                OrderedRulesPoints `json:"points"`
}

// OrderedRulesDefault is the per-outcome fallback the donor checks when no
// detailed rule admitted THAT outcome.
//
// Its bounds are inclusive on both ends and are NOT reordered when min > max.
// The donor validates the two fields independently and never swaps them, so a
// min above a max is a config that admits nothing — which is a real, expressible
// configuration and not a mistake for this model to correct.
type OrderedRulesDefault struct {
	// RawMinPercent / RawMaxPercent are raw percentages in 0..100.
	//
	// An explicit zero is an explicit zero. The donor's 40/60 come from
	// per-field serde defaults that fill in a MISSING key while deserializing;
	// they are not this model's business, because this model accepts a FULLY
	// RESOLVED config in which every field is already present. Substituting
	// 40/60 for a supplied 0 would overwrite the caller's actual configuration.
	// See TestOrderedRulesDefaultBoundsVersusExplicitZero.
	RawMinPercent float64            `json:"rawMinPercent"`
	RawMaxPercent float64            `json:"rawMaxPercent"`
	Points        OrderedRulesPoints `json:"points"`
}

// OrderedRulesConfig is a FULLY RESOLVED donor ordered-rules configuration in
// RAW percentages.
//
// There is no YAML parser here and no defaulting: resolving a configuration
// file into these values is the caller's job, and a fixture records the
// correspondence rather than this package re-deriving it.
type OrderedRulesConfig struct {
	ConfigID string `json:"configId"`
	// Detailed is the ordered rule list. Absent, null and empty all mean the
	// same thing to the donor — no detailed rules — so they mean the same thing
	// here.
	Detailed []OrderedRule `json:"detailed,omitempty"`
	// Default is MANDATORY. The donor's Detailed strategy always carries one.
	Default OrderedRulesDefault `json:"default"`
}

// SuppliedDrawTrace is the bounded, explicitly supplied entropy.
//
// It is a fixed list of raw 64-bit words, not a generator and not a callback.
// The donor drew from a thread-local RNG whose realization was never recorded,
// so the historical entropy is UNAVAILABLE and no amount of modelling recovers
// it. What this model offers instead is exact, reproducible CONSUMPTION: given
// the same words it consumes the same prefix in the same order.
//
// The baseline replay's ObservedRealization is NOT a source for this. That value
// is a recorded stealth draw from a different mechanism, conditioned on a
// decision that already happened; reusing it would leak an observed outcome into
// a counterfactual. See
// TestOrderedRulesSuppliedDrawTraceIsIndependentOfObservedStealth.
type SuppliedDrawTrace struct {
	RunID string `json:"runId"`
	// EntropySemanticsVersion must equal [OrderedRulesEntropySemanticsVersion].
	// A trace declaring other semantics is refused rather than reinterpreted
	// under semantics it was not produced for.
	EntropySemanticsVersion string `json:"entropySemanticsVersion"`
	// Words are consumed strictly in order, once each, across the whole run.
	// The sequence does not restart on a new candidate.
	Words []uint64 `json:"words"`
}

// OrderedRulesCutoffBasis says HOW the stream's boundary was established.
type OrderedRulesCutoffBasis string

const (
	// CutoffFactualIntervention is a boundary at a PROVEN relevant call.
	CutoffFactualIntervention OrderedRulesCutoffBasis = "FACTUAL_INTERVENTION"
	// CutoffConservativeAmbiguous is a boundary taken at an intervention whose
	// association could not be established. The earlier, conservative boundary
	// is chosen deliberately and is reported as such.
	CutoffConservativeAmbiguous OrderedRulesCutoffBasis = "CONSERVATIVE_AMBIGUOUS_ASSOCIATION"
	// CutoffNoInterventionDeclared is the declared interval containing no
	// relevant intervention. Under anything but [CoverageCompleteDeclared] this
	// is NOT evidence that none occurred, and the stream says so through its
	// qualifications.
	CutoffNoInterventionDeclared OrderedRulesCutoffBasis = "NO_INTERVENTION_IN_DECLARED_INTERVAL"
)

// OrderedRulesCutoff is the factual boundary the projection established.
type OrderedRulesCutoff struct {
	// Established is whether a boundary position exists at all.
	Established bool `json:"established"`
	// Position is EXCLUSIVE: candidates strictly before it survive, candidates
	// at or after it are not part of the stream at all.
	Position int64                        `json:"position"`
	Kind     OrderedRulesInterventionKind `json:"kind,omitempty"`
	Identity string                       `json:"identity,omitempty"`
	Basis    OrderedRulesCutoffBasis      `json:"basis"`
	// DroppedAtOrAfter is how many supplied candidates the boundary excluded.
	// It is reported rather than hidden: a stream that lost most of its
	// candidates to an early intervention is a different piece of evidence from
	// one that lost none.
	DroppedAtOrAfter int `json:"droppedAtOrAfter"`
}

// OrderedRulesStream is the projected, DETACHED input the model evaluates.
//
// Detached means deep-copied: nothing here aliases the caller's slices, so a
// caller mutating its own source after projecting cannot change what the model
// reads or what the digests witness. See
// TestOrderedRulesInputsAndTraceDoNotAliasCallerValues.
type OrderedRulesStream struct {
	ContractVersion string                  `json:"contractVersion"`
	Scope           OrderedRulesScope       `json:"scope"`
	Admission       CommonAdmission         `json:"admission"`
	Candidates      []OrderedRulesCandidate `json:"candidates"`
	Cutoff          OrderedRulesCutoff      `json:"cutoff"`
	// Qualifications are the caller's coverage and boundary limitations,
	// carried verbatim into every result computed from this stream.
	Qualifications []string `json:"qualifications,omitempty"`
	// SelectionDigest binds the selection — scope, admission, boundary and every
	// detached candidate value — so a result cannot be re-attached to a
	// different stream.
	SelectionDigest string `json:"selectionDigest"`
}

// OrderedRulesStatus is a traversal's terminal status.
type OrderedRulesStatus string

const (
	// StatusWouldAttempt is participation admitted AND a stake computable from
	// a known balance. A donor-requested stake of zero still lands here: zero
	// is an amount, not an abstention.
	StatusWouldAttempt OrderedRulesStatus = "WOULD_ATTEMPT"
	// StatusParticipationAdmittedStakeUnknown is participation admitted with the
	// chosen outcome KNOWN and the stake NOT computable, because this
	// candidate's balance was not supplied or was out of domain.
	//
	// It is a terminal stop, and it is deliberately not a zero: the model knows
	// it would have bet and does not know how much. Continuing past it would
	// evaluate candidates that only exist in a world where this participation
	// did not happen.
	StatusParticipationAdmittedStakeUnknown OrderedRulesStatus = "PARTICIPATION_ADMITTED_STAKE_UNKNOWN"
	// StatusNoAttemptInSuppliedPrefix is the whole supplied prefix traversed
	// with nothing admitted. It is a statement about the SUPPLIED PREFIX only.
	// It is not a full-round skip, and it is not a financial zero.
	StatusNoAttemptInSuppliedPrefix OrderedRulesStatus = "NO_ATTEMPT_IN_SUPPLIED_PREFIX"
	// StatusUnknownInput is a required input the traversal actually reached and
	// could not read. The scan stops there rather than skipping the candidate,
	// because skipping it would change every later opportunity.
	StatusUnknownInput OrderedRulesStatus = "UNKNOWN_INPUT"
	// StatusRefused is an input this model will not evaluate at all: a config
	// outside the admitted numeric domain, mismatched contract versions, or a
	// resource bound reached. Nothing partial is reported as complete.
	StatusRefused OrderedRulesStatus = "REFUSED"
)

// Closed reasons accompanying a non-WOULD_ATTEMPT status.
const (
	ReasonEntropyExhausted         = "ENTROPY_EXHAUSTED"
	ReasonEntropySemanticsMismatch = "ENTROPY_SEMANTICS_MISMATCH"
	ReasonStreamContractMismatch   = "STREAM_CONTRACT_MISMATCH"
	// ReasonOutcomeVectorNotKnown is a candidate whose ordered outcome vector
	// the caller could not recover whole. It is NOT the donor's fewer-than-two
	// decline, and the traversal stops rather than walking past it.
	ReasonOutcomeVectorNotKnown    = "OUTCOME_VECTOR_NOT_KNOWN"
	ReasonOutcomePointsNotKnown    = "OUTCOME_POINTS_NOT_KNOWN"
	ReasonOutcomePointsOutOfDomain = "OUTCOME_POINTS_OUT_OF_DOMAIN"
	ReasonPoolSumOverflow          = "POOL_SUM_OVERFLOW"
	ReasonBalanceNotSupplied       = "BALANCE_NOT_SUPPLIED"
	ReasonBalanceInvalid           = "BALANCE_INVALID"
	ReasonBalanceOutOfDomain       = "BALANCE_OUT_OF_U32_DOMAIN"
	ReasonConfigOutOfDomain        = "CONFIG_OUT_OF_ADMITTED_DOMAIN"
	ReasonAttemptRateOutOfDomain   = "ATTEMPT_RATE_OUT_OF_BERNOULLI_DOMAIN"
	ReasonRuleCountOverBound       = "RULE_COUNT_OVER_BOUND"
	ReasonDrawWordsOverBound       = "DRAW_WORDS_OVER_BOUND"
	// ReasonStreamShapeOverBound is a supplied stream whose COUNTS already
	// exceed a declared bound — candidates, one candidate's outcomes,
	// qualifications or admission source references.
	ReasonStreamShapeOverBound = "STREAM_SHAPE_OVER_BOUND"
	// ReasonStreamBytesOverBound is a supplied input whose retained text
	// exceeds [MaxOrderedRulesAggregateBytes], the same budget the projection
	// charges.
	ReasonStreamBytesOverBound = "STREAM_BYTES_OVER_BOUND"
	// ReasonStreamTextOverBound is a single supplied string past
	// [MaxOrderedRulesIdentifierBytes] in a field the projection bounds
	// individually. The aggregate budget alone does not catch it: one 64 MiB
	// provenance note sits well inside 128 MiB while being a value the
	// projection refuses outright.
	ReasonStreamTextOverBound = "STREAM_TEXT_OVER_BOUND"
	// ReasonWorkBudgetExceeded covers the evaluated-slot ceiling, which is also
	// the retained-trace ceiling: see [MaxOrderedRulesWork].
	ReasonWorkBudgetExceeded = "WORK_BUDGET_EXCEEDED"
	// ReasonBalanceNotEvaluated is not a failure. It is what the model reports
	// about a balance it never needed, because the traversal never admitted
	// participation on that candidate. Reporting it as MISSING would confuse
	// "we did not have it" with "we did not look".
	ReasonBalanceNotEvaluated = "NOT_EVALUATED"
)

// OrderedRulesSelectionBasis is which half of the mechanism admitted.
type OrderedRulesSelectionBasis string

const (
	// SelectionDetailedRule is admission by an ordered detailed rule: its
	// comparator matched AND its participation draw succeeded.
	SelectionDetailedRule OrderedRulesSelectionBasis = "DETAILED_RULE"
	// SelectionDefault is admission by the current outcome's default bounds. It
	// consumes no draw.
	SelectionDefault OrderedRulesSelectionBasis = "DEFAULT"
)

// OrderedRulesSelection is what the mechanism chose.
type OrderedRulesSelection struct {
	CandidateIdentity string                     `json:"candidateIdentity"`
	CandidatePosition int64                      `json:"candidatePosition"`
	CandidateIndex    int                        `json:"candidateIndex"`
	OutcomeIndex      int                        `json:"outcomeIndex"`
	OutcomeIdentity   string                     `json:"outcomeIdentity"`
	Basis             OrderedRulesSelectionBasis `json:"basis"`
	// RuleIndex is the admitting rule's index, or -1 for a default admission.
	RuleIndex int `json:"ruleIndex"`
	// ShareBits is math.Float64bits of the pool share the decision was made on,
	// recorded as bits so a report cannot round two distinguishable shares into
	// one printed number.
	ShareBits uint64 `json:"shareBits"`
}

// OrderedRulesTraceStep names what one trace entry records.
type OrderedRulesTraceStep string

const (
	// TraceStepRuleComparator is a comparator evaluated and NOT matched. No draw
	// is taken for it, which is the point of recording it separately.
	TraceStepRuleComparator OrderedRulesTraceStep = "RULE_COMPARATOR"
	// TraceStepRuleDraw is a comparator matched and the participation draw
	// evaluated.
	TraceStepRuleDraw OrderedRulesTraceStep = "RULE_DRAW"
	// TraceStepDefaultBounds is the current outcome's default bounds evaluated.
	TraceStepDefaultBounds OrderedRulesTraceStep = "DEFAULT_BOUNDS"
)

// OrderedRulesTraceEntry is one evaluated step.
//
// Identities are carried alongside the indices. In Go a string assignment
// shares the existing backing bytes rather than copying them, so the trace's
// footprint is the indices plus one header per entry — the aggregate identifier
// bytes are counted once, at projection, and not again per rule.
type OrderedRulesTraceEntry struct {
	Step              OrderedRulesTraceStep `json:"step"`
	CandidateIndex    int                   `json:"candidateIndex"`
	CandidateIdentity string                `json:"candidateIdentity"`
	CandidatePosition int64                 `json:"candidatePosition"`
	OutcomeIndex      int                   `json:"outcomeIndex"`
	OutcomeIdentity   string                `json:"outcomeIdentity"`
	// ShareBits is math.Float64bits of the pool share, for the same reason as
	// [OrderedRulesSelection.ShareBits].
	ShareBits uint64 `json:"shareBits"`
	// RuleIndex is the rule's index, or -1 on a default step.
	RuleIndex         int  `json:"ruleIndex"`
	ComparatorMatched bool `json:"comparatorMatched"`
	// BernoulliEvaluated separates "the draw was taken" from "the draw
	// succeeded". A comparator that did not match never reaches the draw.
	BernoulliEvaluated bool `json:"bernoulliEvaluated"`
	BernoulliResult    bool `json:"bernoulliResult"`
	// RawWordIndex is the index of the raw word consumed, or -1 when the step
	// consumed none. A rate of exactly one consumes no word and still succeeds;
	// a rate of exactly zero consumes one and still fails.
	RawWordIndex int    `json:"rawWordIndex"`
	RawWordValue uint64 `json:"rawWordValue"`
	// Admitted is whether this step admitted participation.
	Admitted bool `json:"admitted"`
}

// OrderedRulesEvaluation is the model's complete, self-describing result.
type OrderedRulesEvaluation struct {
	// EvidenceLabel is always [OrderedRulesEvidenceLabel] and bounds every
	// claim below it.
	EvidenceLabel string `json:"evidenceLabel"`
	ModelVersion  string `json:"modelVersion"`
	DonorRevision string `json:"donorRevision"`
	// EntropySemanticsVersion names the semantics the words were consumed under.
	EntropySemanticsVersion string `json:"entropySemanticsVersion"`

	Status OrderedRulesStatus `json:"status"`
	Reason string             `json:"reason,omitempty"`

	// StoppedAtCandidate / StoppedAtPosition are the causal position of the
	// stop, present whenever the traversal actually reached a candidate.
	StoppedAtCandidate string `json:"stoppedAtCandidate,omitempty"`
	StoppedAtPosition  int64  `json:"stoppedAtPosition,omitempty"`
	HasStopPosition    bool   `json:"hasStopPosition"`

	// Selected is nil unless the mechanism admitted.
	Selected *OrderedRulesSelection `json:"selected,omitempty"`
	// Participation is separate from [OrderedRulesEvaluation.Stake] on purpose:
	// the model can know participation was admitted while the stake stays
	// unknown, and collapsing the two would turn an unknown into a zero.
	Participation OrderedRulesParticipation `json:"participation"`
	Stake         SuppliedUint32            `json:"stake"`

	CandidatesConsumed   int `json:"candidatesConsumed"`
	OutcomesConsidered   int `json:"outcomesConsidered"`
	RulesConsidered      int `json:"rulesConsidered"`
	BernoulliEvaluations int `json:"bernoulliEvaluations"`
	// RawWordsConsumed counts words actually taken from the trace.
	//
	// It differs from BernoulliEvaluations only where a rate of exactly ONE was
	// evaluated, because that is the single case that returns an answer without
	// drawing. A rate of exactly zero advances BOTH counters — it builds a
	// distribution with a threshold of zero, draws a word, and only then fails
	// on the strict comparison — so the two staying equal is NOT evidence that
	// no zero-rate rule was reached.
	RawWordsConsumed int `json:"rawWordsConsumed"`

	// Cutoff is the boundary the stream was projected under, carried onto the
	// result rather than left behind on the stream.
	//
	// Without it a NO_ATTEMPT_IN_SUPPLIED_PREFIX produced by a boundary that
	// removed EVERY candidate is field-for-field identical to one over a source
	// that never held any — same status, same counters, same consumed-prefix
	// digest. Those are opposite pieces of evidence: one says a real placement
	// call had already acted on the round, the other says nothing was there.
	Cutoff OrderedRulesCutoff `json:"cutoff"`

	Trace []OrderedRulesTraceEntry `json:"trace,omitempty"`
	// Visits records what the traversal did at EACH candidate it reached, not
	// only at the one it stopped on. The run-level status describes the stop;
	// three of the four mandatory missing-balance vectors are about candidates
	// the traversal walked THROUGH, and without a per-candidate record
	// "the balance was never needed" would be indistinguishable from "the
	// balance was missing".
	Visits         []OrderedRulesCandidateVisit `json:"visits,omitempty"`
	Qualifications []string                     `json:"qualifications,omitempty"`

	// The bindings. Each names one component of the run, domain-separated, so
	// substituting any single one fails to verify as the same run. See
	// TestOrderedRulesCandidatePolicyAndDrawBindingsCannotBeRecombined.
	StreamDigest string `json:"streamDigest"`
	ConfigDigest string `json:"configDigest"`
	// EntropyDigest covers the WHOLE supplied trace; ConsumedInputDigest covers
	// only the prefix actually consumed. Both are reported because an unused
	// suffix must not change the evaluated prefix, and proving that needs the
	// two to be visibly different things.
	EntropyDigest       string `json:"entropyDigest"`
	ConsumedInputDigest string `json:"consumedInputDigest"`
}

// OrderedRulesParticipation is whether the mechanism admitted a bet at all.
type OrderedRulesParticipation string

const (
	// ParticipationNotAdmitted is no admission anywhere in the traversed prefix.
	ParticipationNotAdmitted OrderedRulesParticipation = "NOT_ADMITTED"
	// ParticipationAdmitted is an admission, whether or not the stake is known.
	ParticipationAdmitted OrderedRulesParticipation = "ADMITTED"
)
