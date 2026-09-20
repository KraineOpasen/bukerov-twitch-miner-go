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
// enforces the first five clauses over this package's whole transitive import
// graph, and the sixth over its syntax: a goroutine needs no import, so that
// clause used to be a convention this sentence presented as a machine check.
// It is now Rule E, and Rule E has its own control.
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
// # Sharp edges on the exported surface, not repaired here
//
// An independent misuse-resistance review found four exported shapes whose
// correct use depends on reading a doc comment. The FIRST THREE are documented
// at their own declarations and carry a test that states the contract -- though
// for the ladder methods that test records that the COMPILER, and not its
// assertions, is what catches a drift. The
// fourth is recorded only here, and the distinction is one a later lane had to
// point out: what its tests pin are the three RUNTIME gates it describes, not
// the type-level hazard itself, and DigestReference's own declaration says
// nothing about it. None is REDESIGNED here, because each fix changes an
// approved public seam and that is an owner decision, not a mechanical one:
//
//   - [ProveCommonCutoff] takes the cutoff as a bare int64 with no presence
//     bit, so an absent cutoff reads as position 0 and the predicate reports
//     BOUNDARY_PROVEN. It fails OPEN. The fix is a presence-carrying cutoff
//     and a reachable [BoundaryCutoffUnknown]; it would also refuse a shape
//     nothing in the producer forbids, since an observation id is a plain
//     TEXT column;
//   - [QualityRecord.Downgrade] and [QualityRecord.Merge] return a new record
//     and a discarded result compiles silently, leaving the record at the TOP
//     of the ladder — the inverse of its safety property. The fix is a
//     pointer receiver or a value-returning name;
//   - [PayoutEvidence.PrimaryDenominatorMember] and
//     [PayoutEvidence.PlacedBetDenominatorMember] read as the denominator
//     answer and are only the payout seam's CONDITION;
//     [AssessDenominatorMembership] applies every gate its membership
//     vocabulary names, which is more than these two and is a const block
//     rather than a number written here. The cheap path is the wrong one.
//     The fix is a name that reads wrong at the call site, which would move a
//     JSON key and so the artifact's framing.
//   - THE TWO DIGEST SPELLINGS ARE BOTH `string`. This package keeps a bare
//     64-hex digest and a "sha256:"-prefixed reference deliberately distinct,
//     and [DigestReference] is a func(string) string rather than a type, so
//     every slot holding either is a plain string and the compiler cannot tell
//     them apart. Every digest a verifier CHECKS AGAINST ITS OWN DERIVATION is
//     gated at runtime -- a raw hex digest in a reference slot, a reference in
//     a raw slot, and a doubly prefixed value are all refused by name.
//     "Checks", not "reads": VerifySourceRoundRegistry READS
//     SourceRoundClaim.FactsetDigest on several paths and derives nothing from
//     it, which is the slot below. ONE SLOT IS NOT GATED, so "each reachable
//     slot" would be the wrong sentence: SourceRoundClaim.FactsetDigest is
//     held only to being non-empty and expressible (the WHAT REMAINS entry
//     below), so a reference there reconciles, the registry VERIFIES, and the
//     fault surfaces downstream as CASE_NOT_CANONICAL_SOURCE_ROUND rather than
//     as a digest fault -- a caller who spelled one digest the other way is
//     told something true about a different thing. It is listed because the
//     misuse is easy to write and hard to see:
//     DigestReference(DigestReferencePrefix + hex) reads like a well-formed
//     reference and is a 78-character double-prefixed value
//     the gates refuse, so a test built that way would compare two malformed
//     digests and pass for the wrong reason. The fix is two named string
//     types, which marshal identically so no framing moves -- and which is an
//     owner decision because it changes an approved public seam.
//
// # Multi-clause guards: what is pinned, what is argued, and what is measured
//
// Many of this package's fail-closed guards are compound -- `if A || B` or
// `s.require(A && B, TAG)` -- and a test that violates every clause at once
// cannot tell the real guard from a strictly weaker one. This is stated with a
// MEASUREMENT rather than an impression, because an earlier version of this
// paragraph named three files and was wrong about which.
//
// The sweeps below each drop exactly one top-level clause and run the whole
// package suite, with a byte-identical restore per mutant:
//
//   - THE `s.require` GUARDS IN actionmap.go WHOSE CONDITION IS COMPOUND IN
//     PLACE. There are 48 of them by the definition stated at the head of this
//     section, in which `A || B` is compound just as `A && B` is: 26 are
//     `&&`-joined and carry 66 top-level clauses, 22 are `||`-joined and carry
//     51. BOTH SETS ARE SWEPT, and the verdicts are in this commit's message
//     rather than here, because a count written into a comment is a count
//     nothing re-derives. A review lane established the 48 by an AST census,
//     which is the only way this number should ever be stated: the two sets
//     differ by their top-level operator alone, so a sweep scoped to one of
//     them leaves the other unmeasured.
//
//     "IN PLACE" IS LOAD-BEARING AND WAS NOT SAID AT FIRST. A further TWELVE
//     call sites pass a compound predicate that was bound to a NAME a line or
//     more earlier -- stealthClean, stoppedIntact, unreachedIntact,
//     healthWitnessedOpen, gateOpen, postGateNotReached, healthNotReached:
//     seven predicates carrying 24 distinct top-level clauses. The syntactic
//     definition excludes them, so 48 is not a wrong count, but the rationale
//     at the head of this section applies to them identically and neither
//     sweep has touched them. `unreachedIntact` is an eight-term conjunction
//     and is the guard rows four and five of
//     TestARefusedShapeIsNotAlsoToldArtefactsOfItsOwnRefusal reach, so it is
//     not untested -- it is unswept, which is a different thing and is said
//     here rather than left to be inferred from "48". Before the repair the
//     `&&` half was
//     **31 survivors of 66** -- 28 in the P2 exit arms and 3 in MapP3bAction's
//     own guards (the two clauses of STOP_FIELDS_WITHOUT_STOP_POSITION and the
//     count conjunct of NO_CANDIDATE_REACHED). An earlier version of this
//     paragraph said 29, all in the P2 arms: the three P3b survivors were found
//     and repaired, and then booked against the other sweep. Independent review
//     re-ran the sweep against the commit BEFORE the repair -- actionmap.go's
//     bytes are unchanged since, so only a pre-repair test suite shows the
//     survivors -- and measured 31.
//     THREE OF THE 51 ||-CLAUSE MUTANTS ARE EQUIVALENT AND ARE DECLARED AS
//     SUCH, not counted as survivors. All three drop the `stop < 0` precondition
//     from `s.require(stop < 0 || executedBefore(stop), "STAGES_NOT_RUN_BEFORE_STOP")`,
//     at the legacy-failure, indeterminate and unsupported arms.
//     `executedBefore(i)` is a conjunction of four terms each shaped
//     `i <= <stage index> || ...`, and every stage index is at least 0, so at
//     i == -1 every term holds by its left disjunct and the helper returns true
//     unconditionally. The guard cannot fire when stop < 0 with or without the
//     precondition, so no input distinguishes the two programs. This is an
//     argued equivalence and not the "I tried one mutant form" mistake that was
//     made twice on the comment fence: the OTHER form of the same guard,
//     dropping `executedBefore(stop)` and keeping the precondition, IS in the
//     campaign and IS killed, so the guard's substance is held and only its
//     dead-at-stop<0 prefix is unreachable. The remaining 48 are killed, eight
//     of them only after
//     TestARefusedShapeIsNotAlsoToldArtefactsOfItsOwnRefusal was written,
//     because the existing table asserts CONTAINMENT of the expected tag and a
//     guard naming one contradiction too many passes that.
//
//     WHICH ELEVEN SURVIVED IS NOT THE SET A READER WOULD GUESS. actionmap.go
//     has eleven `stop < 0 ||` preconditions -- three on the legacy-failure arm
//     and four on each of the indeterminate and unsupported arms -- and TWO of
//     them, the health clause of the latter two, were already held by the
//     `alone` rows of that same table, which is exactly what the comment beside
//     that map says they are for. The eleven survivors are the OTHER nine of
//     those preconditions plus two that have nothing to do with a stop: the
//     stake gate's `State != exec` before its reason vocabulary, and the
//     filter's before its applied flag. Those two fire only on a stage that did
//     not execute yet carries a value, which UNREACHED_STAGE_CARRIES_A_VALUE
//     already refuses, so the bound is the same -- an extra contradiction on an
//     already refused evaluation -- but it is a different argument and is
//     stated rather than folded into the other one. The three no-stage `alone`
//     rows sit on a fixture where all eight stages are NOT_REACHED, so
//     `unreachedAfter(-1)`, which ranges over all eight, is true there and the
//     `onlyStop` and `unreachedAfter` artefacts never appear: that is why those
//     two classes needed new rows and the health clause did not.
//
//   - Every single-line `if` condition in all thirteen production files:
//     141 mutants, 65 killed, **63 survived**, 13 that do not compile.
//     entropy.go and actionmap.go are clean; the survivors are concentrated in
//     factset.go, p3b.go, placement.go and evidence.go. THAT SWEEP PREDATES
//     the value gate below AND the three digest-shape gates added after it
//     (factset.go, resolution.go and evidence.go, plus the split of
//     evidence.go's one `||` clause into two statements), and has not been
//     re-run over any of them: they add conditions the census does not count.
//     Each of them
//     was killed individually by a disposable mutant, so the exposure has
//     almost certainly not grown -- but the number above is the old number,
//     said plainly rather than quietly reused, because this file's own
//     discipline is that a count nobody re-scanned is a count that is wrong.
//
// Those 63 are a measured COVERAGE limitation, not 63 defects. Independent
// review proved non-equivalence for two of them and both are now pinned per
// clause: the UNKNOWN-artifact winner guard in [VerifyResolutionArtifact] --
// the package's prime directive at its narrowest point -- and the unread-
// refusal guard in [MapP3bAction]. The COMPLETE factset invariant and the
// stop-fields guard were split for the same reason.
//
// Where the masking barrier is a producer-only witness, no discriminating
// input exists outside this package and the equivalence is ARGUED AT THE SITE
// rather than left unexplained: derivePlacement's case binding, sameMapping,
// and the payout attribution guard -- the last of which was checked by writing
// the test and watching it fail on PLACEMENT_NOT_DERIVED, the barrier one step
// earlier. The remainder are neither pinned nor individually argued, and that
// is the honest state: the number above is the exposure.
//
// # Framing a float by its bits, and the values that framing used to admit
//
// canonical.go frames a float by its BITS, so two values a decimal rendering
// would round together stay distinguishable. It does not follow that two
// INDISTINGUISHABLE values frame alike, and a non-finite value was the sharp
// case: a NaN has many bit patterns, the architectures this project ships for
// do not agree on which one an arithmetic NaN carries, and a factset carrying
// one PASSED [VerifyCommonFactset] while encoding/json refused to write it at
// all, though every exported field of the artifact carries a JSON tag. A
// certificate for an artifact nobody can encode is worse than a wrong one,
// because nothing downstream ever gets far enough to disagree with it.
//
// An earlier round reported and reproduced that and did NOT repair it, on two
// grounds: that the producer's reach was not established, and that refusing a
// value narrows an approved seam. A second, independent review raised it again
// on a later head. Both grounds were then SETTLED rather than restated, and
// the refusal is checkFactsetValuesExpressible, on the build path and the
// verify path, ahead of the digest on each.
//
//	Reach. This package never computes these floats: projectOutcomes copies
//	them verbatim out of the recorded envelope, so what bounds them is the
//	STORE, not the arithmetic that first produced them. Two gates close it.
//	internal/analytics refuses to persist a non-finite value at all --
//	observationFiniteFloat, applied to the delay, the filter value and the
//	three outcome ratios -- and the payload is written and read back as JSON,
//	which has no token for one. A dataset from internal/predictioneval/reader
//	therefore cannot carry a non-finite float, and the BUILD-path gate is
//	defence in depth; the VERIFY-path gate, which a hand-built factset reaches
//	directly, is the load-bearing one.
//	A FIRST DRAFT OF THIS PARAGRAPH ARGUED THE ARITHMETIC INSTEAD, and said
//	reaching a non-finite ratio "needs a negative point count from the
//	platform". That was FALSE, and two of three review lanes affirmed it
//	before a third ran it: internal/models sums the outcomes' points into an
//	int without an overflow check, so three NON-NEGATIVE counts
//	(9223372036854775296, 9223372036854775296, 1030) wrap the total to 6 and
//	drive roundFloat(total/points, 2) to exactly 0, making 100/odds +Inf; and
//	int(float64(1e30)) is -9223372036854775808 on THIS target, so a POSITIVE
//	platform number manufactures the negative count locally. (Written with the
//	explicit conversion because the constant form is a compile error, and
//	"this target" is load-bearing: an out-of-range float-to-int conversion is
//	implementation-defined, and arm64 -- which this project cross-compiles for
//	-- saturates instead. The overflow above does not depend on it.) The
//	retracted claim is recorded here because the lesson is the branch's oldest
//	one: an argument two readers accept is not a measurement.
//	Narrowing. Every FINITE value is accepted exactly as before, however large
//	or small, both zeroes included; the framing, the digest and the vocabulary
//	are untouched. What the seam refuses is a value this package would
//	otherwise have CERTIFIED and encoding/json would have rejected -- not
//	every route such a value has out of the process, since
//	[SerializeCommonFactset] is exported and ungated and still frames one
//	(1,030 bytes for the fixture carrying a +Inf).
//
// WHAT REMAINS, listed rather than summarised, because a summary is how the
// item below this list came to be denied for a round:
//
//   - Framing is still by bits, so +0.0 and -0.0 -- equal under comparison,
//     both encodable, and both round-tripping through JSON with the sign bit
//     intact -- still frame apart. That is a digest separating two spellings
//     of one fact, not a certificate for a fact that cannot be written down.
//   - A BENIGN round-trip class, recorded so it is not rediscovered as
//     a defect: IncompleteReasons = []string{} verifies, marshals, and comes
//     back as nil -- a different Go value -- which still verifies, because the
//     framing writes c.count(len(...)) and 0 is 0 either way. The digest is
//     stable by design. Outcomes, which carries no omitempty, does not behave
//     this way.
//   - BLANKING COSTS REPORTING FIDELITY IN ProjectResolution, found by a review
//     lane and REPORTED RATHER THAN REPAIRED. Two identities that blank to the
//     SAME value then compare EQUAL -- which covers one unrepresentable identity
//     beside one that was legitimately EMPTY, not only the case where both are
//     unrepresentable, as an earlier wording had it. So an artifact
//     whose evidence contradicted its round earns TEXT_NOT_EXPRESSIBLE and
//     ROUND_IDENTITY_MISSING where a readable version of the same evidence
//     would also have earned EVIDENCE_ROUND_MISMATCH. The VERDICT is unaffected
//     -- lossy text always forces UNKNOWN, and VerifyResolutionArtifact does not
//     re-project an UNKNOWN artifact, so no producer/verifier contradiction
//     arises -- but Refusals is inside the digest and is the artifact's audit
//     record, and a reader sees fewer reasons than the evidence earns.
//     Repairing it means remembering WHICH strings were blanked, which is the
//     same information the blanking exists to discard; it is recorded here
//     instead of being given a field nothing else needs.
//   - The registry's INVALID entries can alias: two claims that differ only in
//     strings neither of which can be carried blank to the same value. Neither
//     carries a canonical claim, and each is still counted as its own entry, so
//     nothing leaves the denominator -- but the two are no longer told apart.
//     (An earlier version of this bullet said they "stand for no round", which
//     the repair it shipped beside had just made false: a blanked claim KEEPS
//     its round name, and two such entries name the same round. A review lane
//     caught it. Only a claim whose round NAME was the unreadable string stands
//     for no round.) The alternative, blanking only the offending string, moves
//     that aliasing into the CANONICAL position, which is the one place this
//     package cannot afford it.
//   - SOURCE-ROUND VERIFICATION IS PER-VERDICT, SO A RUN IS QUADRATIC IN ITS
//     OWN SIZE. Reported by a security review lane, reproduced, and NOT
//     repaired. AssessDenominatorMembership re-verifies the whole registry for
//     every case it judges, and a verification hashes every claim, flattens the
//     entries and reconciles them again. The DURABLE part is the shape: one
//     verification grows linearly in N and the run of N grows quadratically, so
//     doubling N from 64 to 128 to 256 multiplies the run's total by about 3.8
//     each time. On a registry of N single-attempt claims over distinct rounds
//     that is 0.542 / 1.044 / 1.987 MiB per verification and 34.71 / 133.63 /
//     508.71 MiB for the run -- 0.57 / 1.09 / 2.08 MB and 36.4 / 140.1 /
//     533.4 MB in units of 10^6, which the nested-hex note below uses. EVERY
//     FIGURE IN THIS BLOCK WAS MEASURED WITHOUT THE RACE DETECTOR; under -race
//     the same run reads about 0.4% higher, which is the detector's bookkeeping
//     and not the amplification.
//     THE UNIT IS SPELLED OUT because the same run reads 4.9% apart in MiB and
//     in MB -- 2^20/10^6 -- which is a gap large enough to pass for a fixture
//     effect and is not one. The absolutes DO move with the claim shape, which
//     is why the fixture is named: against this one, a two-byte claim digest is
//     about 20% cheaper (19% at N = 256), 16-byte session and pool ids 36.5% to
//     37.7% dearer, and 36-character ones 82% to 107% dearer. The RATIO is what
//     survives a change of shape. The fix is the one the lane named -- verify
//     once and carry an immutable verified handle -- but it changes the
//     signature of the function where seam 12 composes with seams 3 and 10,
//     which is an approved seam rather than an implementation detail, and there
//     is no non-test caller today because the shape needs a runner this package
//     deliberately does not contain. Designing that API now would be guessing
//     at a consumer that does not exist; it belongs with the runner, and with
//     the seam re-approved. THIS PACKAGE HAS NO WORKING EXAMPLE OF THAT SHAPE
//     TO COPY: VerifiedP3bRuleset is the nearest thing to one and the entry
//     below reports that it re-verifies on every use, so whoever builds the
//     handle is building the first one here rather than following a pattern.
//   - A VERIFIED RULESET IS RE-VERIFIED ON EVERY USE, SO THE PROTOCOL'S OWN
//     SCHEDULE PAYS FOR IT 16,384 TIMES. Reported by a security review lane on
//     the published head, reproduced, and NOT repaired. VerifiedP3bRuleset.check
//     runs at the top of both EvaluateP3bCase and its sibling, and it does not
//     merely re-check the witness: it calls nativeConfigDigest, which runs the
//     native evaluator over the whole config through the fixed probe. The
//     config's identifier is caller-supplied text bounded only by
//     MaxOrderedRulesAggregateBytes, which is 128 MiB, so a 1 MiB identifier is
//     an ordinary supported value and not an extreme one.
//     MEASURED ON THIS TREE, without the race detector, on check alone: 8,984
//     B/op for a one-byte identifier, 147,671 B/op at 64 KiB, and 2,113,763
//     B/op at 1 MiB -- about 235x the one-byte check, and about two allocated
//     bytes per identifier byte, paid twice over, once when the witness frames
//     the identifier and once when the probe frames the config. THE ALLOCATION
//     COUNT IS FLAT at about 402 across all three widths, so what grows is size
//     and not the number of operations, which is what makes the growth the
//     identifier's rather than the config's shape. THE CONFIG IS ONE DETAILED
//     RULE BESIDE A DEFAULT, which belongs beside the count because the count
//     is the config's: a lane reconstructing the fixture without the rule read
//     393 at every width and reproduced the byte figures anyway. The bytes are
//     the identifier's; the allocation count is not. The lane's own reading
//     divides to 2,113,586 bytes per check and its extrapolation to about
//     34.6 GB for one case's 16,384-trajectory schedule follows from this
//     tree's figure too.
//     ITS READING OF THAT TOTAL NEEDS ONE CORRECTION, because it changes what
//     the fix buys. Nothing here is RETAINED between calls: the allocations are
//     transient and the allocation count is flat, so a schedule's 34.6 GB is
//     cumulative throughput through the allocator and the collector, and the
//     resident cost stays one call's few megabytes. The exposure is CPU and GC
//     pressure across a long run, not a peak that exhausts a process. NO
//     WALL-CLOCK FIGURE IS QUOTED, because two on this branch failed to
//     reproduce across hosts and were retired; the durable statement is that
//     each check is one full native evaluation of the config.
//     AND THE PROBE IS NOT THE ONLY CONFIG-PROPORTIONAL WORK PER EVALUATION,
//     which a fix sold as removing the cost will be measured against: the real
//     EvaluateOrderedRules over the same config measures 1,053,193 B/op at a
//     1 MiB identifier, so sealing the handle removes about two thirds of the
//     per-evaluation config cost and not all of it.
//     ONE HALF OF IT IS CLOSED IN THIS ROUND AND THE OTHER IS NOT, and the two
//     are worth separating because only one needed the redesign. The same
//     probe was also charged on a REFUSAL decided by one integer:
//     EvaluateP3bCase ran rs.check above checkEntropyCoordinates, so an
//     out-of-protocol trajectory paid 2,113,748 B/op and threw the probe away.
//     That is the WORK half of canonical.go's rule and it needed a swap, not a
//     seam: the coordinate gate is above the probe at both evaluators now and
//     that refusal measures 152 B/op flat from a one-byte identifier to a
//     1 MiB one, pinned by
//     TestAnOutOfProtocolCoordinateIsRefusedWithoutProbingTheRuleset. What
//     REMAINS is the cost on the path that does not refuse -- an evaluation
//     that proceeds still re-verifies -- and that is the part below.
//     WHAT IT COSTS TO CLOSE IS AN EXPORTED FIELD. The probe runs again on
//     purpose, for the reason stated at check itself: Config is an exported
//     field of a value type, so a caller may mutate it between verification and
//     use, and the probe is what catches that -- INCLUDING a mutation into a
//     shape the core REFUSES, which reports no digest at all. A CHEAPER DIGEST
//     WOULD CATCH IT -- a review lane built one and showed that a package-local
//     content hash distinguishes a config mutated past MaxOrderedRulesRules
//     from the verified one, so "it would be this package's digest rather than
//     the core's verdict" is an argument about authority and not about
//     detection. The reason the handle cannot simply cache a digest and keep
//     the field is the one that survives that correction: ANY digest compared
//     on every use is still work proportional to the config, so caching shrinks
//     the constant and leaves the shape. Closing it means an unexported
//     verified copy evaluated from directly, which changes an exported field on
//     an approved seam. As with the per-verdict re-verification above, there is
//     no non-test caller: the 16,384-run schedule is the protocol's, and this
//     package deliberately contains no runner to execute it.
//   - A SESSION REFUSED BY ITS OWN PROVENANCE NO LONGER CARRIES ITS ANOMALY
//     REASONS, which is what the preflight at the top of SelectEpisodes costs
//     and is recorded because a trade absorbed in silence is the defect behind
//     the defect. A security review lane measured the order it replaces:
//     MaterializePairedKnowledge ran before the session was tested, so a
//     ONE-BYTE SessionReading refusing the whole session allocated 22,618,360
//     B/op on 16,384 attempt rows against 16 B/op on an empty dataset, and all
//     of it was discarded. The preflight answers every refusal that does not
//     need the materialized knowledge -- the nine provenance arms and the
//     foreign-facts scan -- and returns before materializing.
//     WHAT IS LOST is the two arms that DO need it: a session refused by its
//     provenance no longer also reports the producer's anomalies or its
//     session-level P2 exclusions, because it is refused before those are
//     derived. Those reasons describe records in a session that is not
//     admitted, and the reason a caller acts on is still named; the cut is
//     "needs the materialized knowledge", which is statable, rather than a
//     threshold someone has to tune. It is a reporting loss all the same.
//   - REFUSING A RULESET DOCUMENT STILL COSTS ABOUT SEVEN TIMES THE DOCUMENT,
//     which the decode-fault repair does not remove and is recorded so the
//     repair is not read as closing it. Two lanes measured it: refusing a
//     ~65.6 KB document whose one literal is over-long reads about 467 KB, and
//     a 512 KiB one about 3.68 MB. That is encoding/json decoding the document
//     plus the mandatory sha256Hex over RawBytes, both proportional to a
//     buffer the caller has already materialized and one of which this package
//     cannot skip, since the raw hash is the ruleset's identity. It is the
//     nested-hex class rather than the shape-gate class: a constant factor
//     over input already in hand, not a constant-size field amplified by a
//     payload. What the repair removed is the two further copies of the
//     literal that this package itself was making.
//   - NESTED HEX IN claimKey, reported by a code review lane, reproduced at
//     19.06x the input, and now CLOSED under owner authorization by streaming
//     the representation instead of changing it. EpisodeIdentity.String()
//     hex-encodes the framed identity, claimKey frames that and hex-encodes it
//     again, and registryDigest copied the key once more. The nesting is
//     load-bearing -- the registry digest is computed over those exact bytes
//     and an INDEPENDENT golden generated by
//     testdata/synthetic/gen_golden_digests.py pins it -- so removing the
//     nesting would mean bumping SourceRoundRegistryVersion and regenerating
//     that golden. What was avoidable was never the nesting but the
//     INTERMEDIATE STRINGS: EpisodeIdentity.framed exposes the framing,
//     canonical.strHexOf writes its hex rendering straight into the
//     destination, and compareClaimKeys orders claims without building a key
//     at all. Every byte of claimKey, of registryDigest and of the goldens is
//     unchanged, proved by an ORACLE rather than by a recorded constant:
//     TestStreamingTheClaimKeyDidNotMoveOneByteOfIt keeps the pre-change
//     functions verbatim and runs both over every field shape.
//     THE FIGURE THE PREVIOUS ROUND DECLINED TO QUOTE IS HERE, and the reason
//     it declined was sound: the sign really does depend on how the one-pass
//     writer grows its buffer. What that round asserted about the direction is
//     WRONG, and is retracted -- it said appending two bytes at a time
//     measures worse and "growing the buffer to the exact width measures
//     better". Exact-width growth measures better only where nothing
//     accumulates. A canonical buffer usually accumulates, and reserving
//     exactly what each call needs re-copies everything already written, once
//     per call: with it, sixty-four claims went from 36.94x the input to
//     41.82x while the single-claim case improved -- the signature of
//     quadratic copying. Geometric growth is what works, and canonical.grow
//     keeps it. Measured on the public paths, as multiples of the identity
//     supplied: ReconcileSourceRounds 19.06x -> 14.54x, 32.72x -> 11.65x and
//     36.94x -> 12.13x at one 1 MiB claim, eight 128 KiB claims and
//     sixty-four 4 KiB claims; VerifySourceRoundRegistry, which reconciles and
//     digests again, 38.11x -> 29.08x, 65.43x -> 23.29x and 73.80x -> 24.18x.
//     WHAT IS NOT CLAIMED: the remaining multiple is not overhead to be
//     removed. The digest must read the hex rendering of the identity's
//     framing, which is about four times the identity, because that is the
//     representation. Three ordering attempts preceded the measured one, two
//     of them asserted on reasoning and both wrong; the numbers above are what
//     settled it.
//   - A SUPPLIED CLAIM'S FACTSET DIGEST IS HELD ONLY TO BEING NON-EMPTY AND
//     EXPRESSIBLE -- ReconcileSourceRounds routes it to INVALID on an empty
//     value and on invalid UTF-8, and on nothing else -- found
//     by a review lane one field below the gate this round added, and
//     reproduced: VerifySourceRoundRegistry returns nil for a registry whose
//     claim carries a 1 MiB FactsetDigest, and pays about 14.75 MB to say so --
//     14.06x the supplied field, and the SAME figure before this round's gates
//     existed, so it is a carried item rather than a regression. It is the
//     class the nested-hex note above describes, a constant factor over input
//     the caller has already materialized, not the class the three shape gates
//     closed. The obvious repair is wrong, and the lane built it to find out:
//     gating the VERIFIER alone breaks the fixed point, because the producer
//     would still route such a claim to its round, and this package's own tests
//     catch it -- a 64-hex gate over the claims fails eight of them, among them
//     TestReconcileSourceRoundsNeverMintsARegistryItsOwnVerifierRefuses,
//     because the suite's own fixtures carry two-byte digests. An exact-length
//     gate (len != 64) fails the same eight; a length CEILING (len > 64),
//     which is the minimal shape that closes the amplification, leaves the
//     whole suite green AT THE VERIFIER. It does not at the producer: routing
//     a digest past that ceiling to INVALID in ReconcileSourceRounds fails
//     TestTheExpressibilityRoutingAllocatesNothing, whose 250-claim fixture
//     carries a 4,096-byte FactsetDigest and asserts a canonical claim for
//     every entry. The figures are quoted per SIDE because they differ by
//     side, which is the part a reader planning the repair needs.
//     Closing it means routing a non-64-hex digest
//     to INVALID in ReconcileSourceRounds AND gating it in the verifier,
//     together -- so it costs that fixture as well, which
//     SourceRoundRegistryVersion bumped and the independent golden regenerated
//     -- the same shape of change as the nested hex, and not one to take inside
//     a repair round.
//   - THE EVIDENCE WITNESS FRAMES A DECISION THE REFUSAL ABOVE IT HAS ALREADY
//     REJECTED, reported by a code review lane on the published head and
//     reproduced -- with the MECHANISM the lane gave one step off, in a way
//     that moves the fix. The lane named derivePlacement's struct literal,
//     which copies the caller's strings into PlacementEvidence before
//     decisionRefusal rejects them. In Go a string field copies a two-word
//     header and not the bytes, so that literal is O(1), and so is
//     decisionRefusal: derivePlacement measures 16 B/op and ONE allocation on
//     a decision whose five caller-controlled strings are 1 MiB each, flat
//     against the same decision at one byte. What costs is
//     placementEvidenceWitness, which DerivePlacement calls AFTER
//     derivePlacement returns -- on every path, the refused one included --
//     and which frames Policy, the attempt's collector session and pool
//     instance identifiers, FactsetDigest and EventID into the canonical
//     buffer and hashes them. The lane's CONCLUSION is right; what does it is
//     the witness and not the copy, and that is the difference between a fix
//     inside derivePlacement and a change to what a refusal may name.
//     MEASURED ON THIS TREE, without the race detector, on a decision refused
//     by decisionRefusal's FIRST clause so that nothing below that clause
//     runs: 872 B/op and 8 allocations with all five strings at one byte,
//     against 18,301,274 B/op and 11 allocations with all five at 1 MiB --
//     about 21,000x the one-byte refusal, and about 3.49x the 5,242,880 bytes
//     supplied. DerivePayout is the same shape on the same fixture: 1,576
//     B/op at one byte against 18,301,246 B/op at 1 MiB. THE SPREAD ACROSS
//     REPEATS IS UNDER 200 BYTES on 18.3 MB, so the reading is the fixture's
//     rather than one run's.
//     THE MULTIPLE OF THE INPUT IS NOT A CONSTANT, and the fixture has to be
//     named for that reason rather than for tidiness. The same path with THREE
//     of the five strings at 1 MiB reads 6,889,790 B/op and NINE allocations:
//     2.19x the 3,145,728 bytes supplied, where five strings read 3.49x. Both
//     readings are right; what moves between them is the canonical buffer's
//     growth series. It is Go's slice growth, which doubles only below 256
//     bytes and grows by about a quarter above it, with need-driven jumps: a
//     lane walked the capacities for this exact framing and read ratios of
//     2.450, 1.253, 1.563 and 1.250, not 2. So the total allocated over one
//     call is a step function of the payload rather than a multiple of it,
//     and the extra allocations at the wider fixture are extra growth steps
//     rather than doublings. So
//     the durable statement is the shape, that a refusal decided in constant
//     time frames every identity field in full, and not any one ratio.
//     CLOSING IT IS A CONTRACT CHANGE RATHER THAN A REPAIR. The witness is
//     unexported, is not serialized, and no golden pins it, so its FORMAT is
//     free -- but it is a hash, and a hash of an artifact is proportional to
//     the artifact, while the refused artifact carries the caller's identity
//     fields because a refusal names the case it refused. Making that path
//     O(1) means a refusal stops naming its case, which is a change at two
//     approved seams and not an implementation detail.
//     ONE PART IS WEAKER THAN THE REST, and is named here rather than quietly
//     repaired: Policy is a two-value enum whose every other value the first
//     clause refuses in constant time, and it is framed in full anyway --
//     1,057,073 B/op for a 1 MiB Policy with every other field at one byte,
//     which is the SAME SHAPE as the three constant-size digest gates this
//     round closed. Repairing only Policy leaves EventID, FactsetDigest and
//     the attempt identity behind, so it shrinks the amplification without
//     closing the finding, and it is not taken piecemeal. What remains after
//     that is the class the nested-hex entry above describes -- a constant
//     factor over input the caller has already materialized -- rather than
//     the class the shape gates closed.
//   - AND THE FAIL-CLOSED RULE HAS ONE FAIL-OPEN CARVE-OUT, stated here because
//     this round's own lesson is that a trade absorbed in silence is the defect
//     behind the defect. A claim whose ROUND NAME is itself unreadable names no
//     round, so it contests none, and a round whose only competing claim was
//     corrupted that way keeps its canonical claim. A review lane graded that a
//     blocker. It stands because corrupting a round name reaches exactly what
//     WITHHOLDING the claim reaches -- the same registry plus one INVALID row
//     saying a claim was unreadable -- and no reconciler can detect withholding;
//     while the alternative, letting an unnameable claim contest every round,
//     would let one byte deny a whole registry and would attribute to every
//     round a claim that was about one. roundsWithUnreconcilableClaims argues
//     it at the site and a named test pins it as an exception.
//
// A SECOND CLASS WAS HERE FOR ONE ROUND AND IS NOW REPAIRED, and the way it
// was repaired is worth recording rather than quietly deleting, because the
// first repair was of the INSTANCE and not the class.
//
// Invalid UTF-8 in a hashed string verified, marshalled WITHOUT error, came
// back as U+FFFD and then failed its own digest -- silently, and presenting as
// the one signal this package reserves for tampering. A local lane found it
// because this section's first draft claimed the float repair had settled the
// whole class; it was then recorded as out of scope on the reasoning that the
// reported class was a value that cannot be ENCODED while this one encodes
// LOSSILY. An external lane ranked it P1 and named the two things that settle
// it: this package's own coverage_test.go round-trips a factset through JSON,
// so the round trip is a supported path rather than a hypothetical one, and
// checkEntropyCoordinates in this same package ALREADY required valid UTF-8 of
// three identities -- the precedent was three functions away. Being out of
// scope is a judgement, and that one was wrong.
//
// THE GATE WAS THEN ADDED TO THE FACTSET ONLY, and this paragraph was shortened
// to say the class was closed. It was closed for ONE of the four artifacts this
// package verifies, and a review lane caught the shortened sentence. The long
// version was then written, with a row per artifact -- and TWO of its four rows
// were wrong, in the same way, for the same reason: they reasoned about the
// code instead of running it. What follows is what running it showed.
//
//	CommonFactset        GATE ADDED: checkFactsetValuesExpressible, on the
//	                     build path and the verify path, ahead of the digest
//	                     on each
//	ResolutionArtifact   GATE ADDED on the verify path, and then -- one commit
//	                     later, after an external lane observed that gating a
//	                     verifier leaves this package's own exported projector
//	                     free to mint what it refuses -- expressibleEvidence on
//	                     ProjectResolution, which drops the text and refuses
//	                     with TEXT_NOT_EXPRESSIBLE
//	SourceRoundRegistry  PRODUCER REPAIRED, TWICE. The row here first read
//	                     "closed by CONSTRUCTION: it re-derives from its own
//	                     entries", which was true of a registry someone EDITED
//	                     and said nothing about the one this package mints.
//	                     ReconcileSourceRounds minted a registry that verified,
//	                     marshalled without error and then failed its own
//	                     re-derivation. THE FIRST REPAIR FOR THAT WAS WORSE THAN
//	                     THE DEFECT: it dropped the claim's ROUND NAME, which
//	                     took the claim out of its round's group, so one invalid
//	                     byte on a competing claim dissolved a CONFLICT into a
//	                     UNIQUE with a canonical claim and the case counted --
//	                     fail-OPEN on the one axis this package declares
//	                     fail-closed, with no trace left in the registry.
//	                     expressibleClaim now drops the claim's DIGEST instead,
//	                     which routes it to INVALID just as well and keeps the
//	                     round name, and roundsWithUnreconcilableClaims holds
//	                     that round fail-closed -- with one carve-out, listed
//	                     under WHAT REMAINS, where the unreadable string is the
//	                     round name itself. checkRegistryTextExpressible gates
//	                     the verifier as the other two are gated
//	P3bRuleset           CLOSED, and now with receipts rather than an argument:
//	                     all five of its string positions are refused, by three
//	                     different gates -- the two config strings against the
//	                     config decoded from the raw bytes, the ruleset id
//	                     against the config id, and the two digests against
//	                     64 lower-case hex digits. RawBytes needs no clause:
//	                     JSON carries []byte as base64
//
// One artifact was closed all along, one needed only a gate, and two needed BOTH
// a gate and their exported producer repaired. (An earlier version of this
// sentence said "two needed a gate and one needed its PRODUCER repaired", which
// contradicted the table two lines above it; a review lane caught the
// arithmetic.)
//
// The pattern worth keeping is narrower than the one stated here for a round,
// which claimed the producer "is where the hole was every time it was there at
// all" -- a quantifier this file's own account of the factset contradicts,
// since that instance was found at the VERIFIER and the build-path gate is
// recorded three hundred lines above as defence in depth. What holds is the
// weaker and still useful thing: a verifier gate is the half that gets written
// first and the half that gets mistaken for the whole repair, and in this
// package the exported producer went unchecked TWICE -- at ProjectResolution and
// at ReconcileSourceRounds. (An earlier wording said "three times running",
// which contradicted the partition sentence a dozen lines above it: the factset
// gate landed on the build path and the verify path in ONE commit, so that
// producer was never left unchecked. A review lane caught the arithmetic, which
// is the second time this file has been wrong about its own count.)
//
// AND THE REPAIR IS NOT AUTOMATICALLY SAFER THAN THE DEFECT. The registry's
// first repair round-tripped correctly and was fail-open; the defect it
// replaced was un-round-trippable and fail-closed. Two independent review lanes
// found that, and the writer did not. A repair that trades an invariant for a
// property has to say which invariant, out loud, in the commit -- absorbing it
// silently is how it happened.
//
// AND A HOIST CAN MOVE A PRECEDENCE WITHOUT TOUCHING A PREDICATE. The rule
// this round applied -- a materialization must not precede a gate that can
// refuse without it -- is about COST. It says nothing about which answer wins
// when two gates both have something to say, and at SelectEpisodes the answer
// changed: lifting the session preflight above MaterializePairedKnowledge
// lifted it above the causal-order check at that function's top, so a dataset
// with out-of-order facts AND a refusable session answered (selection, nil).
// The caller's own malformed input came back filed as a completed verdict
// about somebody's session, against the two-kinds-of-error contract stated in
// SelectEpisodes' own doc comment, which was not edited and had been true when
// it was written. A review lane found it on the published head.
//
// THE CLASS IS NARROWER THAN "HOISTS ARE RISKY", and the narrow form is what
// makes it checkable: the damage needs a gate that answers (value, nil) to be
// hoisted above a gate that answers an error. Two errors reordered change
// which fault is NAMED, which is a reporting choice; a verdict placed above an
// error changes whether the caller is told it has a bug at all. Of the five
// hoists this round made, four were error-above-error -- the coordinate and
// factset-admission gates above the ruleset probe at both P3b evaluators, the
// two constant-size arms above the resolution framing, the outcome ceiling
// above the conversion loop -- and exactly one, the session preflight, had a
// (value, nil) gate to lift. That one was the defect. The other four were
// re-checked against this shape after the finding, not before it.
//
// THE SAME ROUND'S PINS STOPPED ONE EVALUATOR SHORT, which is the pattern
// again and this time in the tests rather than the code. The coordinate gate
// and the factset admission were both hoisted at BOTH P3b evaluators, and the
// cost assertion that pins each was written against EvaluateP3bCase alone --
// while the review lane that asked for the factset gate had named
// EvaluateP3bWithTrace. Nothing failed, because deleting either gate from
// EvaluateP3bWithTrace changes no ANSWER: bindEntropyCoordinates refuses the
// same coordinate below it and ProjectP3bSingleCandidate refuses the same
// factset below that. Only the cost moves, so only a cost assertion at that
// evaluator can see it, and there was none. Both deletions were survivors
// until the two tables were driven over both entry points; they are mutants
// OR8 and OR9 now, and both die.
//
// WHAT GENERALISES IS NOT "TEST BOTH SITES" but the reason there were two: a
// gate hoisted for COST leaves the answers identical by construction, so the
// suite that proves the answers cannot notice where it went. Every hoist in
// this round is therefore pinned by an allocation assertion rather than by a
// verdict, and an allocation assertion is only ever about the call it makes.
//
// A COST GATE CHANGED A DIGESTED FIELD, and the comment above it said it had
// not. The outcome-ceiling gate was added to stop the projection converting a
// vector it was about to refuse, and it answered with this package's own
// sentinel and sentence. Two things went with that. A caller classifying on
// predictioneval.ErrOrderedRulesOverBound -- which the path below the gate
// carries up through errors.Join -- stopped matching it. And ProjectionRefusal
// is hashed into p3bResultWitness, so the sentence is not a message but a
// field: the same over-ceiling factset produced a DIFFERENT ARTIFACT depending
// on whether the gate existed. The gate now reproduces both, and the
// agreement test composes its expectation from what the native projector
// actually says about the same count rather than transcribing it.
//
// The lesson is not "preserve error text". It is that a gate justified as
// moving WHEN a refusal happens has to be checked against everything the
// refusal is an INPUT to, and a refusal string that reaches a digest is an
// input to the artifact. The comment asserting sentinel and wording were
// unchanged was written in the same commit that changed both.
//
// AND THE SAME RULE NEEDED THREE ROUNDS AT THE RESOLUTION ARMS. Hoisting two
// constant-size semantic arms above the framing took away the precedence that
// every unreadable string in the artifact outranked a semantic claim, because
// the whole expressibility scan used to run first. Round one gave the
// out-of-vocabulary arm a deferral. Round two gave the winner arm one, after a
// lane found the sibling. Round three -- this one, after another lane -- is
// the first that states the rule instead of patching an arm: no semantic claim
// may be made until every string the artifact carries ONE of is readable,
// asked once above both arms as resolutionConstantStringsExpressible. Two
// rounds of per-arm deferrals restored the precedence for two fields out of
// eight.
//
// WHAT THAT REPAIR DOES NOT RESTORE, recorded rather than absorbed: the three
// SLICE scans -- ordered outcome ids, evidence references, refusals -- stay
// below the arms, because walking them is the O(n) work the hoist exists to
// avoid. An artifact both semantically wrong and unreadable inside one of
// those slices is now told the semantic thing where before the hoist it was
// told the encoding one. Closing that residual means paying the scan the hoist
// removed, so it is a trade and not an oversight -- but it is a precedence
// this package used to offer and no longer does.
//
// AND BOTH OF THOSE REPAIRS WERE THEMSELVES DEFECTIVE, found by the next lane
// on the next head. The pattern did not stop when it was named; it recurred
// inside the answers to it.
//
// THE CEILING SHORTCUT DISPLACED A GATE ITS AUTHOR HAD ENUMERATED WRONGLY.
// ProjectOrderedRulesStream checks the SCOPE before it walks candidates, and
// the scope this projector builds carries three caller-derived strings bounded
// at MaxOrderedRulesIdentifierBytes. A factset with both an over-long episode
// identity and an over-ceiling vector was told the candidate-count sentence
// where the native path says the scope one -- a different witness, since
// ProjectionRefusal is digested. The claim that no earlier gate could fire was
// written on the review thread that asked for the gate, and it enumerated the
// PER-CANDIDATE checks only. The shortcut now answers only when the scope is
// within bound and steps aside otherwise.
//
// THE EXPRESSIBILITY PREFLIGHT BOUGHT PRECEDENCE WITH UNBOUNDED CPU. Reading
// all eight one-per-artifact strings above two constant-time arms let a
// supplier attach a multi-megabyte readable ProofBasis and charge proportional
// work to every verification. The writer's defence -- that utf8.ValidString
// allocates nothing -- treated allocation as the only cost there is, which is
// the same narrowing that produced the original amplification class this
// package spent a round closing.
//
// WHAT MAKES THAT ONE HARD IS THAT TWO FINDINGS PULL OPPOSITE WAYS. One lane
// required every constant-size string to be proven readable before any semantic
// claim; the next required the work above those claims to be bounded. A byte
// CEILING satisfies both and refuses artifacts this package certifies today,
// which breaks the producer/verifier fixed point -- the same wall the outcome
// vector's verifier-side bound is still behind. The resolution is to bound the
// ORDER rather than the VALUE: a field over resolutionPrecedenceBudget is read
// by the complete scan below the arms instead of above them, so the work above
// them is bounded by eight times a constant while no artifact's verdict moves.
// The residual -- an over-budget unreadable field yields precedence to an arm
// -- is the same kind as the slice residual beside it, and is pinned by a test
// rather than left to be rediscovered.
//
// THE REPAIR RESTATES A PREDICATE IN A SECOND PACKAGE, which is a cost the
// scope forced and not a design preference: the order check is unexported in
// predictioneval, and reaching it means calling the materializer -- which is
// the thing the preflight exists not to pay for. So causalOrderFault says the
// contract again, answers predictioneval's own sentinel, and is pinned AS a
// duplicate: TestTheLocalOrderFaultAgreesWithTheMaterializer drives it and the
// materializer over one table and one randomized sweep and fails on any
// disagreement. Three hand-mutations
// of the local predicate -- the epoch arm dropped, the strict comparison made
// non-strict, the epoch-equality guard dropped so an epoch's sequence reset
// reads as a fault -- were each killed by that test before it was trusted.
//
// Nothing here was checked against real P1/P1.5 data: no production dataset
// is proven available, no dataset window (T0/T1) or dataset binding is
// asserted, the ruleset candidates verified in ruleset_work_test.go bind no
// choice of ruleset, and the synthetic fixtures are implementation evidence
// only.
package p4offline
