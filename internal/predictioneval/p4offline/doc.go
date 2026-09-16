// Package p4offline is the OFFLINE, PURE adapter and evidence-only scorer for
// the approved P4 comparison protocol between the P2 baseline replay and the
// P3b ordered-rules core that both live in
// [github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval].
//
// It makes the already-approved protocol mechanically enforceable over
// SUPPLIED evidence. It does not run the comparison. There is no trajectory
// runner, no leaderboard, no metric table, no delta, no ROI comparison, no
// Monte-Carlo standard error, no tuning and no ruleset selection here — those
// are a separate, separately authorized execution concern. What this package
// does is narrower and prior to all of that: it decides, per supplied
// evidence, WHAT may be evaluated, OVER WHICH bytes, UNDER WHICH bindings, and
// WHAT the evidence proves afterwards — refusing, typed and closed, wherever
// the evidence does not carry the proof.
//
// # The twelve seams
//
//  1. Raw evidence, session, episode and common-boundary validation:
//     [SelectEpisodes], [ProveCommonCutoff].
//  2. First automated opportunity selection WITHOUT later substitution: part
//     of [SelectEpisodes]; see [EpisodeSelection].
//  3. Globally reconciled source-round identity and deduplication:
//     [ClaimSourceRound], [ReconcileSourceRounds], [VerifySourceRoundRegistry].
//  4. Outcome-free common factset serialization and digest:
//     [BuildCommonFactset], [SerializeCommonFactset], [VerifyCommonFactset],
//     [CommonFactsetDigestVersion].
//  5. Exact P2 per-case configuration binding: [BindP2Config].
//  6. One-candidate P3b common-data projection: [ProjectP3bSingleCandidate],
//     consumed through [EvaluateP3bCase] and [EvaluateP3bWithTrace].
//  7. Exhaustive native action map: [MapP2Action], [MapP3bAction],
//     [NativeActionMapVersion].
//  8. Separate immutable resolution artifact and digest: [ProjectResolution],
//     [SerializeResolutionArtifact], [VerifyResolutionArtifact].
//  9. Evidence-only placement: [ProjectFactualPlacement], [DerivePlacement].
//  10. Evidence-only payout: [DerivePayout].
//  11. Deterministic entropy under the approved framing and public seed:
//     [EntropySeed], [EntropyMessage], [EntropyMAC], [GenerateEntropyWords],
//     [ValidateEntropyWords], [BuildDrawTrace], [ValidateDrawTrace],
//     [EntropyAlgorithmVersion].
//  12. Typed quality / UNKNOWN / exclusion handling and the composed
//     denominator verdict: [Quality], [QualityRecord], [Int64Fact],
//     [AssessCaseQuality], [AssessDenominatorMembership],
//     [DenominatorMembership].
//
// # Epistemic rules this package enforces mechanically
//
//   - missing is not zero; UNKNOWN is not skip; NOT_REACHED is not zero;
//   - the P3b status NO_ATTEMPT_IN_SUPPLIED_PREFIX is not POLICY_SKIP and
//     carries no financial zero;
//   - a known zero stake may still be WOULD_ATTEMPT;
//   - the winner and the resolution are unavailable to the evaluators — no
//     evaluation function in this package accepts a [ResolutionArtifact];
//   - the native P2 common-input digest (pe-cid/v1) is never reused as the
//     outcome-free common factset digest; [CommonFactsetDigestVersion] hashes
//     the factset VALUES and nothing recorded after the decision;
//   - a recorded choice, result, placement, payout or observed stealth
//     realization never reaches a common policy input or the entropy;
//   - denominator membership is a per-policy fact, never inferred from a
//     class name: the primary POLICY_CHOICE_ACCURACY denominator is the
//     resolved WOULD_ATTEMPT decisions of the PRIMARY_SCORABLE cases, the
//     PLACED_BET_WIN_RATE denominator is their platform-proven bets that
//     settled WIN or LOSE, and a POLICY_SKIP or NO_ATTEMPT_IN_SUPPLIED_PREFIX
//     is a visible non-member that is never counted wrong. The payout seam
//     states its half ([PayoutEvidence]); the case's half is re-derived from
//     the dataset; only [AssessDenominatorMembership] composes them, and
//     only for the canonical claim of the round in the source-round
//     registry the caller reconciled;
//   - no later fact repairs an earlier missing value;
//   - a different counterfactual choice or stake cannot inherit the factual
//     placement;
//   - a local nil error or a local CALL_RETURNED does not prove platform
//     acceptance;
//   - a financial value without its proof stays UNKNOWN;
//   - nothing here is a full-policy claim: FAITHFUL_FULL_POLICY stays on HOLD.
//
// # Digests detect change, not origin
//
// Every digest here is an unkeyed SHA-256 over a documented framing; anyone
// who can build an artifact can compute its digest. So no consumer stops at
// a matching digest. A factset's labels are re-derived from its values
// ([VerifyCommonFactset]); a WINNER_KNOWN or REFUND resolution is
// re-projected from its own facts ([VerifyResolutionArtifact]); a factset
// that must be the dataset's is rebuilt from the dataset
// ([ProjectFactualPlacement], [AssessCaseQuality]); a policy result is bound
// to the factset it was evaluated over before it becomes a [PolicyDecision];
// a placement and a payout carry the case they settle and are checked
// against the decision; a [VerifiedP3bRuleset], a [P2CaseResult], a
// [P3bCaseResult], a [PolicyDecision], a [FactualPlacement], a
// [PlacementEvidence] and a [PayoutEvidence] each carry an unexported
// witness only their producer can set, so a hand-built or
// stored-and-reloaded one settles nothing, and neither does one edited in
// anything a decision is minted from or an audit reads (identities, digests,
// statuses, actions, choices, stakes — the native evaluation structures are
// bound through the digests of their INPUTS and the framed action, choice
// and stake, not framed whole; the two policy-result witnesses' docs list
// exactly what they leave unframed, and every other witness frames every
// exported field of its artifact — the verified ruleset's Config through the
// native re-digest at every evaluation); a decision is minted only from the
// evaluator's own result (an edited one is named by its specific
// contradiction first, a consistent but underived one as exactly that); a
// source-round claim is derived from the dataset by [ClaimSourceRound]
// before [ReconcileSourceRounds] reconciles it; and the entropy coordinates
// must name the factset's own digest and round. The trusted path is
// therefore IN-PROCESS derivation from the dataset: a runner that stores
// artifacts re-derives them before it settles anything on them,
// [AssessCaseQuality] is the gate that decides whether a case is scorable,
// and [AssessDenominatorMembership] is the only place a decision is
// counted — as the canonical claim of its round in a registry that
// re-derives from its own entries, whose digest the verdict names. Whether
// that registry was reconciled over every dataset of the run is the
// runner's record, not a pure function's proof; and no pure function can
// authenticate that a supplied reference EXISTS: "proven" always means
// "the supplied proof was checked for its binding", and the observation
// identities it names are what the reader's audit follows.
//
// # The source trust boundary
//
// This package authenticates NOTHING about where a [predictioneval.SourceDataset]
// came from, and nothing in it should be read as doing so.
//
// The approved chain is: an owner-pinned source snapshot and its provenance,
// then the EXISTING P1 ingest path's verification of that snapshot — the row
// digests and session witnesses are recomputed by the STORE, inside the
// transaction that reads them, and the reader consumes that verdict rather than
// re-deriving it — producing a SourceDataset, and only then the pure validation
// and evaluation in this package. Every guarantee here is
// conditional on the step before it. P4 requires those row witnesses intact;
// this package is not a second P1 storage reader and does not re-verify them.
//
// So a hand-built SourceDataset that simply lies about its provenance is NOT
// authenticated here and must not be described as an authenticated source. It
// will be validated — shapes the producer cannot write are refused, and the
// refusals are typed — but validation is not authentication. A supplier who
// fabricates a self-consistent dataset gets a self-consistent answer.
//
// The distinction the digests do and do not carry is the same one. A hash binds
// BYTES: it proves an artifact was not altered after it was digested, and it is
// what makes a stored artifact re-derivable and an edited one detectable. It
// proves nothing about the TRUTH of what those bytes assert. ObservationSHA256
// rides on every source row for the reader's audit; this package verifies no
// record digest, and could not make a fabricated row true by verifying one.
//
// What this package does contribute to that chain is SHAPE. SOME relations the
// pinned producer cannot violate — established by enumerating its actual
// writers, not read off the grouping comments in its vocabulary tables — are
// enforced. Not all of them are; the ones that are were chosen because they
// are load-bearing for P4's own semantics: which
// episode is the first automatic opportunity, where the common cutoff falls,
// whether an intervention occurred, and whether a native evaluation is a shape
// its evaluator could have produced. Those refusals narrow what a fabricated
// source can claim without contradicting itself. They do not turn it into a
// verified one.
//
// Two limits on that, stated rather than left to be found. The enforced
// relations are a chosen subset: other phase families LOOK equally
// single-writer on inspection and are deliberately not transcribed, because a
// grouping comment is not a contract and enumerating every writer is what earns
// a refusal — and that enumeration was done for the call and automatic phases
// only. And no producer-side VALIDATOR pins the relations that are enforced:
// the analytics layer checks a phase against one flat vocabulary with no
// binding to the kind. Some producer flow tests do compare exact kind/phase
// sequences, so a new emitter inside those flows would fail them, but one on
// another path would not — and this package would then begin refusing honest
// rows: fail-closed, and wrong.
//
// # Digest spellings
//
// Two spellings of a SHA-256 exist here, on purpose and in fixed places. The
// package's own artifact digests — [CommonFactset.Digest], the P2 config
// binding, the source-round registry, every witness — are the bare 64
// lower-case hex digits over the documented framing. The two digests the
// protocol names as cross-artifact facts are spelled as the protocol spells
// them, "sha256:" followed by those digits ([DigestReference]): the common
// factset digest inside [EntropyCoordinates], and the resolution artifact's
// [ResolutionArtifact.ResolutionFactsDigest] that both scorers share. A
// consumer joining a payout to its trace coordinates compares
// [DigestReference] of the payout's factset digest, never the two strings
// raw. No artifact was persisted under any other spelling or shape (the
// decision and payout artifacts gained their derivation fields in the same
// reconciliation): the package has never run against data, nothing outside
// it imports it, and its artifact versions name the contract, not a wire
// history.
//
// # Purity
//
// Every function is a pure function of its arguments: no database, no
// network, no clock, no environment, no global RNG, no goroutines. The only
// first-party import is the predictioneval package itself, whose own fence
// already excludes every capability package. TestP4OfflineDependencyFence
// enforces this package's fence over its whole transitive import graph.
//
// # Reconciled readings and remaining implementation limitations
//
// The owner's P4 data-instantiation readiness package (its source
// validation, entropy vectors and ruleset candidates; see
// testdata/synthetic/work/WORK_PROVENANCE.md) has been reconciled against
// this package. What follows is the exact disposition of every reading that
// was provisional before, and the one reading the package says nothing
// about and which therefore remains a stated assumption:
//
//   - the entropy framing and seed are the approved ones, pinned by the
//     owner's vectors (entropy_work_test.go); the dataset identity and
//     version in [EntropyCoordinates] are the owner's dataset binding,
//     carried verbatim and not verifiable here — see the audit rule on
//     [EntropyCoordinates]: relabelling them resamples the whole schedule,
//     so the frozen binding must pin them before any evaluation;
//   - the resolution proof obligations, still versioned as
//     [ResolutionObligationsRevision], are semantically identical to the
//     source validation's where both speak, and NARROWER in one respect —
//     no derived-winner path exists here, so evidence that would need one
//     yields UNKNOWN. The label changes only by an owner decision;
//   - denominator membership follows the approved semantics per policy,
//     composed with the case quality by [AssessDenominatorMembership] (the
//     payout seam's conditions are [PayoutEvidence.PrimaryDenominatorMember]
//     and [PayoutEvidence.PlacedBetDenominatorMember]); the earlier readings
//     that dropped a skip's case to DESCRIPTIVE_ONLY and counted every legal
//     WOULD_ATTEMPT as a bet are withdrawn. One NARROWING of this package's
//     own is labelled as such: the PLACED_BET_WIN_RATE denominator is also
//     gated on the case being PRIMARY_SCORABLE (so on the counterpart's
//     determinacy), which the approved "proven accepted WIN+LOSE" rule does
//     not require; it can only withhold, and the reasons say why;
//   - the one accepted platform-acceptance basis for a placement,
//     [ProofBasisPlatformPredictionConfirmed], and the one accepted linkage
//     basis for a payout record, [LinkageBasisAttemptLinkedUserTerminal],
//     remain this package's vocabulary — an IMPLEMENTATION limitation, not
//     protocol fact: the package checks a supplied proof for its basis and
//     binding, names no other basis, and so is a narrower, fail-closed
//     reading of the evidence-only rule that can never upgrade an UNKNOWN
//     or make an unsupported action accepted;
//   - STILL AN ASSUMPTION, addressed by nothing in the readiness package:
//     the attribution of raw facts to an episode ([SelectEpisodes]) — a fact
//     bears on an episode when it names the same public round in any
//     incarnation, the same incarnation, or — conservatively — no
//     incarnation and no round any episode of the session carries (admitted
//     or excluded), on the same pool. The last rule can only exclude an
//     episode or break its boundary, never admit one.
//
// Nothing here was checked against real P1/P1.5 data: no production dataset
// is proven available, no dataset window (T0/T1) or dataset binding is
// asserted, the ruleset candidates verified in ruleset_work_test.go bind no
// choice of ruleset, and the synthetic fixtures are implementation evidence
// only.
package p4offline
