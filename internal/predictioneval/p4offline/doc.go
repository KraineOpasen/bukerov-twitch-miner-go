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
// item below this list came to be denied for a round.
//
// AN ENTRY STAYS WHEN IT CLOSES, marked as closed and carrying both readings.
// Deleting it would delete the measurement that shows the repair was needed
// and the one that bounds what it bought, and a repair with neither on the
// record is indistinguishable from a claim. Every such entry states what it
// did NOT buy in the same breath, because that residue is what the next
// reader inherits:
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
//   - SOURCE-ROUND VERIFICATION WAS PER-VERDICT, SO A RUN WAS QUADRATIC IN ITS
//     OWN SIZE. Reported by a security review lane, reproduced, and CLOSED IN
//     THIS ROUND under owner authorization to change the seams it sat on.
//     WHAT THE DEFECT WAS, and it was WIDER THAN THE REPORT. The lane named
//     AssessDenominatorMembership re-verifying the whole registry for every
//     case it judged -- hashing every claim, flattening the entries and
//     reconciling them again -- and that was real. It was not the whole of it:
//     every seam that took a SourceDataset re-ran seams 1-3 over the WHOLE
//     dataset and then scanned the result linearly for one episode, so one
//     verdict selected the dataset TWICE, once inside the quality assessment
//     and once inside the canonical-claim binding, and then scanned the
//     registry's entries linearly for one canonical claim. FIVE PASSES OVER
//     THE RUN PER CASE, not one. Removing the verification alone would have
//     left the run quadratic and the entry would have read as closed.
//     THE DURABLE PART IS THE SHAPE: one verification grew linearly in N and
//     the run of N grew quadratically, so doubling N from 64 to 128 to 256
//     multiplied the run's total by about 3.8 each time. On a registry of N
//     single-attempt claims over distinct rounds that was 0.542 / 1.044 /
//     1.987 MiB per verification and 34.71 / 133.63 / 508.71 MiB for the run
//     -- 0.57 / 1.09 / 2.08 MB and 36.4 / 140.1 / 533.4 MB in units of 10^6,
//     which the nested-hex note below uses. EVERY FIGURE IN THIS BLOCK WAS
//     MEASURED WITHOUT THE RACE DETECTOR; under -race the same run reads about
//     0.4% higher, which is the detector's bookkeeping and not the
//     amplification.
//     THE UNIT IS SPELLED OUT because the same run reads 4.9% apart in MiB and
//     in MB -- 2^20/10^6 -- which is a gap large enough to pass for a fixture
//     effect and is not one. The absolutes DO move with the claim shape, which
//     is why the fixture is named: against that one, a two-byte claim digest
//     was about 20% cheaper (19% at N = 256), 16-byte session and pool ids
//     36.5% to 37.7% dearer, and 36-character ones 82% to 107% dearer. The
//     RATIO is what survives a change of shape.
//     WHAT CLOSED IT IS TWO HANDLES AND A SIGNATURE CHANGE AT FIVE SEAMS.
//     [PrepareSourceRounds] runs the same [VerifySourceRoundRegistry] any
//     caller could run, ONCE, and owns an index from round to canonical claim;
//     [PrepareDataset] runs [SelectEpisodes] ONCE and owns an index from
//     episode identity to selection. [BuildCommonFactset],
//     [ProjectFactualPlacement], [ClaimSourceRound], [AssessCaseQuality] and
//     [AssessDenominatorMembership] take those handles instead of the dataset
//     and the registry, so there is exactly one production call of
//     SelectEpisodes and one of VerifySourceRoundRegistry left in this
//     package, both inside a Prepare. A consumer cannot mix a verified
//     registry with a different one or a factset with another dataset's
//     selection, because it no longer passes either artifact: THE HANDLE IS
//     THE BINDING. Neither handle is a cache, a store or a second validator --
//     each is a value the caller holds, built by the verifier that already
//     existed.
//     MEASURED ON THIS TREE, one verdict over a run of n cases: 38,336 B/op at
//     n = 32 and 38,336 B/op at n = 256. Flat, to the byte, over eight times
//     the run. A run of N is therefore one prepare plus N of those, which is
//     linear -- TestAPreparedRunIsLinearInTheCasesItJudges.
//     WHAT IT DID NOT BUY, in two parts. FIRST, the indexes are argued and
//     only the re-derivations are measured: that test counts ALLOCATION, and
//     replacing either index with the linear scan it stands in for allocates
//     nothing, so the run would go back to O(N^2) comparisons with the test
//     green. A wall-clock assertion would catch it and this branch has retired
//     two of those for failing to reproduce across hosts, so the honest record
//     is the split rather than a figure. SECOND, a prepared dataset is a
//     SELECTION and not a factset cache: each verdict still rebuilds its own
//     episode's factset twice, once in the quality assessment and once in the
//     canonical-claim binding. That is O(the episode) and flat in the run,
//     which is why it does not appear above, and it is the next thing a reader
//     measuring this path will find.
//     THE SELECTION'S OWN FAILURE IS OWNED AND RETURNED, and getting only the
//     first half of that right is the one design decision here that could have
//     lost a case. A dataset whose selection cannot run still becomes a handle
//     carrying that error, and every seam reads it exactly as it read
//     SelectEpisodes' own return, so a case judged EXCLUDED with
//     SELECTION_UNAVAILABLE before is judged the same way now. A handle that
//     REFUSED such a dataset would have turned a case this package used to
//     judge into a case nobody can judge. The zero handle is separate and is
//     refused as [ErrSelectionNotPrepared] -- deliberately not
//     ErrEpisodeNotSelected, so selectionRan answers false and the verdict is
//     incomplete as well as fail-closed, which is what "no selection ran"
//     means.
//     PrepareDataset FIRST SHIPPED RETURNING NO ERROR AT ALL, and a Codex
//     review named what that cost: through the handle alone an aborted
//     selection was indistinguishable from a dataset with no episodes, because
//     Episodes answers 0 for both, so a runner scheduling case seams from that
//     count would call no seam, read no carried error, and drop the failed
//     dataset in silence -- the exact fail-stop this package requires on
//     incomplete processing, defeated by an unexported field. It returns the
//     error beside the usable handle now, which keeps both halves:
//     TestPrepareDatasetReturnsTheSelectionsOwnFailure asserts the caller
//     cannot hold the failure without seeing it AND that the handle it holds
//     still judges the case the way it did.
//   - A VERIFIED RULESET WAS RE-VERIFIED ON EVERY USE, SO THE PROTOCOL'S OWN
//     SCHEDULE PAID FOR IT 16,384 TIMES. Reported by a security review lane on
//     the published head, reproduced, and CLOSED IN THIS ROUND under owner
//     authorization to change the seam it sat on. The entry stays because the
//     measurement is both the evidence the repair was needed and the bound on
//     what it bought.
//     WHAT THE DEFECT WAS. VerifiedP3bRuleset.check ran at the top of both
//     EvaluateP3bCase and its sibling, and it did not merely re-check the
//     witness: it called nativeConfigDigest, which runs the native evaluator
//     over the whole config through the fixed probe. The config's identifier is
//     caller-supplied text bounded only by MaxOrderedRulesAggregateBytes, which
//     is 128 MiB, so a 1 MiB identifier is an ordinary supported value and not
//     an extreme one. MEASURED ON THE PUBLISHED HEAD, without the race
//     detector, on check alone: 8,984 B/op for a one-byte identifier, 147,671
//     B/op at 64 KiB, and 2,113,763 B/op at 1 MiB -- about 235x the one-byte
//     check, and about two allocated bytes per identifier byte, paid twice
//     over, once when the witness frames the identifier and once when the probe
//     frames the config. THE ALLOCATION COUNT WAS FLAT at about 402 across all
//     three widths, so what grew was size and not the number of operations,
//     which is what made the growth the identifier's rather than the config's
//     shape. THE CONFIG IS ONE DETAILED RULE BESIDE A DEFAULT, which belongs
//     beside the count because the count is the config's: a lane reconstructing
//     the fixture without the rule read 393 at every width and reproduced the
//     byte figures anyway. The bytes were the identifier's; the allocation
//     count was not. The lane's own reading divides to 2,113,586 bytes per
//     check and its extrapolation to about 34.6 GB for one case's
//     16,384-trajectory schedule follows from that tree's figure too.
//     ITS READING OF THAT TOTAL NEEDED ONE CORRECTION, because it changes what
//     the fix buys. Nothing there was RETAINED between calls: the allocations
//     were transient and the allocation count flat, so a schedule's 34.6 GB was
//     cumulative throughput through the allocator and the collector, and the
//     resident cost stayed one call's few megabytes. The exposure was CPU and
//     GC pressure across a long run, not a peak that exhausts a process. NO
//     WALL-CLOCK FIGURE IS QUOTED, because two on this branch failed to
//     reproduce across hosts and were retired.
//     WHAT IT COST TO CLOSE WAS AN EXPORTED FIELD. The probe used to run again
//     on purpose, for the reason stated at check itself: Config was an exported
//     field of a value type, so a caller could mutate it between verification
//     and use, and the probe was what caught that -- INCLUDING a mutation into
//     a shape the core REFUSES, which reports no digest at all. A CHEAPER
//     DIGEST WOULD HAVE CAUGHT IT TOO -- a review lane built one and showed
//     that a package-local content hash distinguishes a config mutated past
//     MaxOrderedRulesRules from the verified one, so "it would be this
//     package's digest rather than the core's verdict" was an argument about
//     authority and not about detection. The reason the handle could not simply
//     cache a digest and keep the field is the one that survived that
//     correction: ANY digest compared on every use is still work proportional
//     to the config, so caching shrinks the constant and leaves the shape.
//     SO THE REPAIR REMOVED THE REACHABILITY, NOT THE COMPARISON. The verified
//     config is an unexported field now, detached at verification and handed
//     out only as a detached copy by ConfigCopy; evaluation reads the sealed
//     value and nothing else. There is no expression left by which the
//     evaluated config can differ from the verified one, so the re-derivation
//     is UNNECESSARY rather than cheaper. The identity fields stay exported and
//     stay bound, because the witness covers them; a zero value, a forged value
//     and a deserialized one all still fail, because the witness is unexported
//     and nothing outside this package can set it.
//     TestTheSealedRulesetCannotBeRetuned drives the alias paths -- the
//     returned copy, two copies against each other, and the caller's own config
//     mutated after verification.
//     ConfigCopy REFUSES AN UNVERIFIED HANDLE rather than answering zero, and
//     it is the one accessor here that has to: every field of
//     OrderedRulesConfig has a legitimate zero -- the type's own doc says an
//     omitted default decodes to a USABLE [0,0] rule -- so a zero, forged or
//     deserialized handle would otherwise hand back a structurally complete,
//     fully-resolved-LOOKING config with nothing to say nothing verified it. A
//     Q3 lane named it. Rules, Digest, Rounds and Episodes answer zero on an
//     unverified value and are deliberately left doing so: an empty count and
//     an empty digest read as empty, where an all-zero config reads as real.
//     WHAT IT DID NOT BUY IS THREE QUARTERS OF THE COST, and saying so needs
//     one fact first: VerifyP3bRuleset REQUIRES RulesetID to equal the config's
//     ConfigID, so the ruleset's name and the config's are one caller-supplied
//     value and every framing of either pays for it. Measured here over sixteen
//     evaluations of one case at a 1 MiB identifier: 4,240,782 B/op per
//     evaluation on the published head, 3,183,766 now. An allocation profile of
//     400 evaluations splits what REMAINS into three nearly equal shares of
//     about 1,057,000 B/op -- the native EvaluateOrderedRules digesting the
//     config it is handed (1,055,076 B/op measured alone, the floor beneath
//     everything here), check framing the identifier for the witness, and
//     p3bResultWitness framing it again because a P3bCaseResult carries the
//     ruleset's name and that artifact's framing is frozen. SEALING DID NOT
//     MAKE EVALUATION O(1) IN THE CONFIG and nothing here claims it did; check
//     itself is not O(1) either, for the second of those three reasons.
//     THE ONE SHARE STILL REACHABLE IS CHECK'S, and it is not taken here. It
//     would mean unexporting RulesetID, RawSHA256 and NativeConfigDigest behind
//     accessors -- three more exported fields on an approved seam, all three
//     read to populate a frozen artifact -- to remove one third of what is
//     left, while the result witness keeps paying the identifier regardless.
//     The reported defect was the probe; the probe is what this removes.
//     THE DURABLE INVARIANT IS A PASS COUNT, AND A MUTANT IS WHY. The obvious
//     form -- P4's work as a MULTIPLE of the native floor -- was written first
//     and is blind: restoring the re-proof inside check leaves it green,
//     because at a one-byte identifier the probe's fixed cost exceeds the floor
//     and lifts both ends of the ratio together (4.04x and 3.02x before, 5.29x
//     and 4.02x after; the ceiling never trips). What a re-proof changes is how
//     many times one evaluation WALKS the identifier, so that is what
//     TestASealedRulesetIsNotReprovedOnEveryUse measures -- marginal bytes per
//     identifier byte between a one-byte and a 1 MiB fixture. The native floor
//     reads 1.00 passes, the whole path 3.01, and the surviving mutant 4.01.
//     THE PROSE ABOVE DID NOT CATCH THIS AND THE MUTANT DID, which is the same
//     lesson this file records for gate order, arriving here through a cost
//     ceiling instead.
//     ONE HALF OF THIS WAS CLOSED IN AN EARLIER ROUND and is kept here because
//     the two halves needed different repairs. The same probe was also charged
//     on a REFUSAL decided by one integer: EvaluateP3bCase ran rs.check above
//     checkEntropyCoordinates, so an out-of-protocol trajectory paid 2,113,748
//     B/op and threw the probe away. That was the WORK half of canonical.go's
//     rule and it needed a swap, not a seam: the coordinate gate is above the
//     probe at both evaluators now and that refusal measures 152 B/op flat from
//     a one-byte identifier to a 1 MiB one, pinned by
//     TestAnOutOfProtocolCoordinateIsRefusedWithoutProbingTheRuleset.
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
//   - REFUSING A RULESET DOCUMENT STILL COSTS ABOUT SEVEN TIMES THE DOCUMENT.
//     OWNER-DEFERRED for this supplied-evidence offline package: the measured
//     cost is accepted under the existing provenance and binding checks, the
//     accepted-input domain is retained, and the limit is to be revisited
//     before a real-data runner rather than inside a repair round. Two lanes
//     measured it and a third reproduced both figures through
//     VerifyP3bRuleset: a ~65.6 KB document reads 457,795 B (6.98x) and a
//     512 KiB one 3,669,099 B (7.00x).
//     THE ATTRIBUTION RECORDED HERE BEFORE WAS WRONG AND IS CORRECTED. It said
//     the cost was encoding/json decoding the document plus the mandatory
//     sha256Hex over RawBytes. Measured per stage at 512 KiB: sha256Hex is
//     128 B and FLAT at both sizes, so it is not a contributor; json.Decode
//     NEVER RUNS, because checkRulesetKeys refuses first; checkRulesetKeys is
//     3,668,713 B, which is 99.99% of it; decodeFault is 200 B flat. This
//     package's own residue is 258 B. The cost is the key walk, and it is
//     encoding/json's buffering over a buffer the caller already holds.
//     AND REFUSAL IS THE CHEAP END, WHICH THE OLD WORDING HID. Controls at
//     512 KiB: a giant number literal 7.00x, a giant string literal 5.00x, and
//     a VALID document of many small tokens 13.99x. Honest input costs about
//     twice what this refusal costs, so the figure is a property of the format
//     rather than a penalty this seam imposes on refusal. dec.UseNumber() in
//     checkRulesetKeys measures 7.00x -> 5.00x and is NOT taken: it moves the
//     refusal from Token()'s typed UnmarshalTypeError arm to Decode and changes
//     the message, and the residual 5x is stdlib buffering unreachable through
//     the public API. rulesetRawCeiling is what bounds the carrier.
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
//     claim carries a 1 MiB FactsetDigest, and pays 11,586,217 B to say so --
//     11.05x the supplied field, and the SAME figure before this round's gates
//     existed, so it is a carried item rather than a regression. (An earlier
//     "about 14.75 MB, 14.06x" recorded here is STALE and is corrected to the
//     figures in this entry.) The full ReconcileSourceRounds plus
//     PrepareSourceRounds path at one 1 MiB digest is 17,380,018 B, 16.57x,
//     against 10,224 B for a well-formed 64-hex control. The multiple by width
//     is 27.98x at 1 KiB, 17.69x at 64 KiB and 16.57x at 1 MiB -- flat to
//     falling, so this is a LINEAR CONSTANT FACTOR and not a growing
//     amplification. Per stage at 1 MiB: registryDigest 5,792,846 B across
//     three passes, claimKeyFraming 1,057,562 B (1.008x -- the streaming repair
//     did land), claimTextFault and checkRegistryTextExpressible 0. It is the
//     class the nested-hex note above describes, a constant factor over input
//     the caller has already materialized, not the class the three shape gates
//     closed.
//     OWNER-DEFERRED for this supplied-evidence offline package: the existing
//     accepted-input domain is retained, this measured processing cost is
//     accepted under the existing provenance and binding checks, and the limit
//     is to be revisited before a real-data runner rather than automatically.
//     Nothing about membership, settlement, tamper acceptance or a hidden
//     processing failure is covered by that deferral. The exposure today is to
//     a hand-built registry only: ClaimSourceRound derives the digest from a
//     VERIFIED factset and no non-test caller exists. The obvious repair is wrong, and the lane built it to find out:
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
//     together -- so it costs that fixture as well, and the independent golden
//     with it: measured, the shape gate moves golden_digests.json's
//     sourceRoundRegistry entry from cfd77538 to 9ba343ed and fails ten
//     top-level tests, eighteen counting subtests.
//     THE CLAIMED NEED FOR A VERSION BUMP IS UNPROVEN AND IS RETRACTED. This
//     entry said closing it was what SourceRoundRegistryVersion bumped.
//     Measured: the version IS framed into registryDigest as its first part, so
//     bumping it changes EVERY registry's digest, while the shape gate alone
//     changes the digest only for a registry that carries a non-64-hex claim --
//     an all-64-hex registry digests identically with and without the gate
//     (8a129a93 both ways), and the non-64-hex one moves 8d9669cc -> 47a16705.
//     The bump's blast radius is strictly WIDER than the defect, so it is a
//     compatibility-signalling CHOICE and not a mechanical necessity. The
//     golden regeneration is a necessity; the bump is not. Either way this is
//     a new refusal of supplied input -- a validity ceiling -- and not one to
//     take inside a repair round.
//   - THE EVIDENCE WITNESS FRAMED A DECISION THE REFUSAL ABOVE IT HAD ALREADY
//     REJECTED, reported by a code review lane on the published head,
//     reproduced -- with the MECHANISM the lane gave one step off, in a way
//     that moved the fix -- and CLOSED IN THIS ROUND under owner authorization
//     to change what a refusal may name.
//     WHAT THE DEFECT WAS. The lane named derivePlacement's struct literal,
//     which copied the caller's strings into PlacementEvidence before
//     decisionRefusal rejected them. In Go a string field copies a two-word
//     header and not the bytes, so that literal is O(1), and so is
//     decisionRefusal: derivePlacement measured 16 B/op and ONE allocation on
//     a decision whose five caller-controlled strings were 1 MiB each, flat
//     against the same decision at one byte. What cost was
//     placementEvidenceWitness, which DerivePlacement calls AFTER
//     derivePlacement returns -- on every path, the refused one included --
//     and which frames Policy, the attempt's collector session and pool
//     instance identifiers, FactsetDigest and EventID into the canonical
//     buffer and hashes them. The lane's CONCLUSION was right; what did it was
//     the witness and not the copy, and that was the difference between a fix
//     inside derivePlacement and a change to what a refusal may name.
//     MEASURED ON THE PUBLISHED HEAD, without the race detector, on a decision
//     refused by decisionRefusal's FIRST clause so that nothing below that
//     clause runs: 872 B/op and 8 allocations with all five strings at one
//     byte, against 18,301,274 B/op and 11 allocations with all five at 1 MiB
//     -- about 21,000x the one-byte refusal, and about 3.49x the 5,242,880
//     bytes supplied. DerivePayout was the same shape on the same fixture:
//     1,576 B/op at one byte against 18,301,246 B/op at 1 MiB. THE SPREAD
//     ACROSS REPEATS WAS UNDER 200 BYTES on 18.3 MB, so the reading was the
//     fixture's rather than one run's.
//     THE MULTIPLE OF THE INPUT WAS NOT A CONSTANT, and the fixture has to be
//     named for that reason rather than for tidiness. The same path with THREE
//     of the five strings at 1 MiB read 6,889,790 B/op and NINE allocations:
//     2.19x the 3,145,728 bytes supplied, where five strings read 3.49x. Both
//     readings were right; what moved between them was the canonical buffer's
//     growth series. It is Go's slice growth, which doubles only below 256
//     bytes and grows by about a quarter above it, with need-driven jumps: a
//     lane walked the capacities for this exact framing and read ratios of
//     2.450, 1.253, 1.563 and 1.250, not 2. So the total allocated over one
//     call was a step function of the payload rather than a multiple of it,
//     and the extra allocations at the wider fixture were extra growth steps
//     rather than doublings. The durable statement was the shape, that a
//     refusal decided in constant time framed every identity field in full,
//     and not any one ratio -- WHICH IS WHY THE TEST THAT CLOSES IT ASSERTS A
//     ONE-BYTE CONTROL rather than a budget. A budget is satisfied by shrinking
//     a multiple; only a control measured on the same fixture is not.
//     WHAT CLOSING IT COST WAS THE CONTRACT, AND THE OWNER AUTHORIZED IT.
//     A hash of an artifact is proportional to the artifact, and the refused
//     artifact carried the caller's identity fields because a refusal names the
//     case it refused; so making the path constant meant a refusal stops naming
//     its case IN TEXT. It names it in ARITHMETIC instead: both seams now
//     return a bounded artifact above decisionRefusal's verdict, carrying the
//     category, the attempt's two fixed-width numbers, and one reason under
//     the IDENTITY_WITHHELD prefix that states how wide each withheld field
//     was and not one byte of it. Nothing is invented, no refusal becomes a
//     verdict, and no consumer can bind to an artifact that names no case,
//     because every consumer compares the identity in WHOLE. MEASURED ON THIS
//     TREE: DerivePlacement 1,712 B/op at one byte against 1,792 at 1 MiB,
//     DerivePayout 1,840 against 1,952 --
//     TestARefusedDecisionNamesItsExtentAndNotItsText, which drives both seams
//     and the six mutants that reinstate each withheld field in turn.
//     THE ONE-BYTE REFUSAL GOT DEARER, about 840 bytes at the placement seam
//     and 260 at the payout seam, and it is recorded rather than absorbed: the
//     extent sentence is work a refusal naming its case in full never did.
//     AND THE WITHHOLDING CLOSED ITS OWN NEIGHBOUR ONE SEAM DOWNSTREAM, which
//     two independent Q3 lanes found before this head was published -- the
//     first time this branch's recurring failure was caught by a review rather
//     than by a mutant, and its EIGHTH occurrence. THE REPAIR FOR IT THEN DID
//     THE SAME THING ONE GATE EARLIER, which a Codex review caught on the
//     published head and which makes NINE: the new gate was placed BELOW the
//     policy switch, so it answered for a refused decision whose policy this
//     package recognizes and missed the one whose policy it does not --
//     namedPolicy withholds an unrecognized name, so the switch's default arm
//     fired first and returned the very string the gate exists to replace. It
//     is above the switch now, and the test carries the row that was missing. A refused payout names no
//     case, so AssessDenominatorMembership's case-binding gate fired for every
//     one of them and reported PAYOUT_BINDING_MISMATCH -- true, and the same
//     string a genuine cross-case splice earns, and the same string the
//     witness gate below it earns for a spliced decision. Three
//     distinguishable faults arriving as one is the regression selectionRan
//     was repaired for, one seam over. namedPolicy exists precisely to keep a
//     refusal's category where it was at the placement-to-payout seam, and
//     that reasoning was not carried the next seam along.
//     MembershipReasonPayoutRefusedDecision now names it above the binding
//     gate, on an exact discriminator rather than a heuristic: decisionRefusal
//     refuses an empty FactsetDigest, so a DERIVED payout carrying none is
//     exactly one this package refused. WHY it was refused stays on the
//     artifact's own Reasons, in the refusal's own category, which is finer
//     than the two verdicts the gate displaces rather than coarser.
//     THE PART THAT WAS CALLED WEAKER THAN THE REST IS THE PART THAT SURVIVED,
//     and not as the piecemeal repair the earlier reading of it proposed.
//     Policy is a two-value enum whose every other value decisionRefusal's
//     first clause refuses in constant time, and it was framed in full anyway
//     -- 1,057,073 B/op for a 1 MiB Policy with every other field at one byte.
//     It is not repaired by shrinking; it is repaired by the same withholding
//     as the rest, with ONE exemption stated at namedPolicy: a policy name
//     EQUAL to PolicyP2 or PolicyP3b is this package's own three-byte constant
//     matched by value, not caller text carried on trust, and a 1 MiB one
//     fails that comparison and is withheld like everything else. Carrying the
//     recognized name is also what keeps a refusal's CATEGORY where it was: a
//     payout compares the placement's policy before it compares the case, so a
//     placement naming no policy at all would be refused as a POLICY binding
//     mismatch, which is true and which hides that the placement refused its
//     own decision.
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
//   - A COLLIDING OBSERVATION ID MADE P2 EXCLUSION ATTRIBUTION QUADRATIC,
//     reported by a security review lane on the published head, reproduced,
//     and CLOSED IN THIS ROUND without refusing anything.
//     WHAT THE DEFECT WAS. p2ExclusionIndex answers one episode by copying the
//     posting list of every observation id that episode carries. The
//     observation id is the SUPPLIER'S text, so N causally ordered,
//     metadata-consistent undecodable AUTO_DUE rows on N DISTINCT round
//     incarnations -- hence N episodes -- all stamped with ONE id build a
//     single posting list of length N that all N episodes copy, sort and
//     position-dedup, and that appendOnce then collapses to the ONE reason it
//     actually carried. The work was quadratic and the answer was one string.
//     MEASURED ON THIS TREE through the exported SelectEpisodes, before the
//     repair: 40,352,320 B at 2,048 rows and 147,957,848 B at 4,096 -- a 3.7x
//     ratio over a 2x input -- against 13,671,760 B for the SAME rows with
//     distinct ids. The lane reported 12.1 / 41.5 / 150.2 MB at 1,024 / 2,048
//     / 4,096 against 3.7 / 7.9 / 15.9 MB. The two colliding figures reproduce
//     to within 3%; the distinct-id control is 14% apart, and that gap is
//     recorded rather than smoothed -- the lane's fixture is not published, so
//     the RATIO is what survives, exactly as the per-verdict entry above says.
//     WHAT CLOSED IT IS A PREAGGREGATION AND NOT A CEILING. The lane offered
//     "reject or preaggregate duplicate ObservationID values". Rejecting is a
//     new validity ceiling on supplied data: it would refuse datasets this
//     package certifies today, and this package answers a cost by doing less
//     WORK, never by admitting less EVIDENCE. So each posting list now keeps
//     one position per DISTINCT REASON. That is output-identical -- the answer
//     is the distinct reasons ordered by the smallest hit position each one
//     occupies, and that position is minimal within its own bucket too, so the
//     reduction keeps it -- and the bound comes from the producer's exclusion
//     vocabulary being CLOSED at fourteen constants, none of them caller text.
//     AFTER: 6,754,176 B at 2,048 and 13,639,056 B at 4,096, a 2.02x ratio,
//     and 0.998x what the same rows cost with distinct ids. 10.8x less at
//     4,096, and nothing refused that was not refused before.
//     THE IDENTITY IS EXECUTED, NOT ARGUED.
//     TestTheReducedPostingListsAnswerAsTheFullOnesDid runs the reduced index
//     against a VERBATIM copy of the unreduced build over 16,000 randomized
//     queries on deliberately tiny alphabets, and carries three controls: that
//     a posting list was actually reduced (10,038 were), that the answers had
//     content (12,844 did), and that a mutant keeping the LAST position per
//     reason -- which preserves the reason SET and breaks only the ORDER, the
//     half of the claim an argument is likeliest to get wrong -- is caught by
//     that same fixture.
//     THE NEIGHBOURS WERE HUNTED, AND THE HUNT'S FIRST FRAME WAS WRONG.
//     byKey takes the same reduction although no lane named it -- as DEFENCE
//     IN DEPTH and not as a closed live entrance: a byKey bucket cannot exceed
//     one today, because materializeAttempt runs once per deduped key and each
//     of its six keyed refusals returns immediately off a nil excl. A Q3 lane
//     established that by execution, and established too that reverting that
//     half left the whole suite green -- leaving a route unreduced is
//     output-identical, so only a per-route control can see it, and that is
//     what the test now carries. The package's three other
//     posting-list indexes were enumerated -- SelectEpisodes' duplicate-round
//     grouping, signalIndex, and ReconcileSourceRounds' claim groups -- and
//     each visits every group exactly once.
//     AND THEN A SECOND Q3 LANE FOUND THE ENTRANCE THAT ENUMERATION COULD NOT
//     SEE, because "posting-list index" was the wrong frame: the class is a
//     SUPPLIER-SIZED LIST RE-READ PER CONSUMER, and the second instance is not
//     an index at all. lookupEpisode runs per CASE, and on a session refused
//     by its own P2 exclusions it rebuilt the refusal summary every time --
//     scanning a list whose length is the caller's dataset, twice, for a
//     sentence that never changes. 5,615,856 B for 512 cases over 512 refusals
//     against 20,422,752 B at 1,024, a 3.64x ratio; after building it once per
//     HANDLE in PrepareDataset, 36,864 and 73,728 B, exactly 2.00x, with the
//     sentence pinned BYTE FOR BYTE against a hand-counted literal. It is the
//     same lesson one turn later than the earlier repair on that seam, which
//     stopped the list being RENDERED into the message and left the recompute
//     standing -- so BOTH instances found in this round are closed, and what
//     is recorded is that the enumeration, not the code, is where the next one
//     will hide. This branch's recurring failure now has ELEVEN occurrences,
//     every one a repair that shut the entrance it was shown and left the
//     neighbouring one open; the last two were caught before publication.
//   - THE REDUCTION BOUNDED THE BUCKET AND NOT THE NUMBER OF BUCKETS, which a
//     review lane found on the published head and which is the TWELFTH
//     occurrence of this branch's recurring failure -- the second one found
//     inside the repair that closed the eleventh.
//     WHAT WAS LEFT OPEN. One episode reads one bucket per distinct observation
//     id its first opportunity carries, and nothing limits that count. A
//     supplier that puts each of N ids on two refused rows with DIFFERENT
//     reasons keeps both positions in every bucket -- correctly; they are
//     different reasons -- so exclusionsFor gathered 2N integers and SORTED
//     them to emit two strings. Measured on this tree, 50 calls: 11,875,552 B
//     at N = 2,048 and 23,889,648 at 4,096, about 116 bytes per identifier for
//     a two-string answer, with an O(N log N) sort no allocation ratio sees.
//     THE AXIS WAS SEEN AND DISMISSED, which is the part worth recording. The
//     reasoning was that work proportional to an episode's own identifier list
//     is linear in that episode's input and so not an amplification. True of
//     the TRAVERSAL, false of the sort and the gathering: the answer is bounded
//     by the vocabulary, so anything growing with N to produce it is work the
//     answer does not need.
//     WHAT CLOSED IT is a merge that keeps the smallest position PER REASON,
//     so the running state is bounded by the vocabulary rather than by the
//     episode and the final ordering sorts at most fourteen entries. The
//     duplicate-id set went with it -- merging by minimum is idempotent, so a
//     repeated id costs one extra bucket traversal and the per-call state no
//     longer grows with the caller at all. 50 calls now read about 3,600 B at
//     BOTH 2,048 and 4,096 ids -- the sixteen bytes separating two given runs
//     are run-to-run jitter and not a size effect, confirmed across three runs,
//     so the figure is quoted as flat rather than as a contrast.
//     TestAnEpisodesOwnIdentifierCountDoesNotEnterTheAnswersCost.
//     AND THE ORACLE WAS WIDENED RATHER THAN LEFT COMPARING THE NEW CODE TO
//     ITSELF: TestTheReducedPostingListsAnswerAsTheFullOnesDid now runs the
//     ORIGINAL implementation on the UNREDUCED index against the current one on
//     the reduced index, so one comparison spans both repairs, with the merge
//     isolated on one index beside it.
//   - A LOCAL ERROR CLASS WAS ECHOED INTO A FAIL-CLOSED PLACEMENT AND FRAMED
//     AGAIN, reported by the same lane, reproduced, and CLOSED.
//     placementStatusCoherent admits ANY class other than NONE whenever the
//     reason code is not OK, so a supplied dataset can carry an otherwise
//     coherent failed CALL_RETURNED whose class is arbitrarily wide. The arm
//     copied that class into Reasons and placementEvidenceWitness framed the
//     copy, so the artifact both returned attacker-sized diagnostic text and
//     paid for it.
//     THE LANE'S MECHANISM WAS ONE FRAMING OF TWO, and the correction changes
//     what the repair can promise. The class is a field of a GENUINE
//     FactualPlacement, so factualPlacementWitness frames it to answer whether
//     that placement is derived -- work proportional to an artifact the dataset
//     really carries, which is not amplification and cannot be removed without
//     weakening the witness. Measured: 3,174,320 B/op at a 1 MiB class before,
//     3.03x the supplied text; 1,061,568 after, 1.012x. What went is every
//     framing BEYOND the honest one, which is why the test asserts ONE framing
//     against a one-byte control rather than a constant.
//     THE REPORTING LOSS IS REAL. There is no vocabulary to recognize the class
//     against -- this repository names exactly one class constant, NONE, which
//     this arm cannot see -- so unlike a policy name it cannot be
//     carried-when-recognized. It is withheld outright: a caller now learns
//     that a local error was recorded and how wide its class was, not which
//     class it was. The status already carries what the verdict turns on.
//   - AN EDITED RESULT PAID ITS OWN EDIT'S WIDTH TO BE REFUSED, reported by the
//     same lane at both result types, reproduced, and CLOSED.
//     derived() short-circuits on a missing witness, which protects a hand-BUILT
//     result: a caller cannot set the unexported field. It did not protect a
//     hand-EDITED one. A caller that COPIES a genuine result keeps its witness,
//     widens one framed field and leaves the factset digest and action binding
//     intact; the binding gates pass, and computing the witness materializes the
//     whole edit before one hash comparison refuses it. Measured: a 1 MiB edit
//     to Trace.RunID cost 1,062,368 B/op against 8,544 at one byte.
//     A PER-FIELD GATE WAS THE WRONG SHAPE, and rejecting it is this round's
//     own lesson applied before a lane had to apply it. Checking the fields the
//     lane named -- the derivable run identity, the fixed-shape digests --
//     closes the entrances named and leaves every other framed field open. What
//     is checked instead is the TOTAL FRAMED WIDTH, recorded beside the witness
//     at mint: an edit that changes the TOTAL width is refused in O(number of
//     fields), and an edit preserving that total -- including one that moves
//     bytes from one framed field into another -- pays exactly the honest cost,
//     which is the floor, because telling two values of equal total width apart
//     IS the hash's job.
//     THE WIDTH CANNOT DRIFT FROM THE WITNESS because it is not a mirror: the
//     canonical framer has a LENGTH-ONLY mode and the width is the same framing
//     function run with bytes switched off. A hand-written mirror of a
//     thirty-five-field framing is a second place a future field can be
//     forgotten, and this package has already paid for one of those.
//     TestTheRecordedFramedWidthIsTheWitnessOwn drives both modes over 400
//     randomized results at both types and over strHexOf, which no result
//     framing exercises. MEASURED AFTER: 4,048 B/op at one byte and 4,048 at
//     1 MiB, at BOTH seams -- identical to the byte.
//     AND THE REPAIR WAS NARROWER THAN ITS OWN SENTENCE SAID, which two
//     independent Q3 lanes found before publication -- the THIRTEENTH
//     occurrence, and the third in a row caught by a lane rather than by a
//     reviewer. The width was given to the two RESULT types the report named.
//     Four sibling seams carry a witness and re-frame it to answer a question
//     decided by one string comparison, each over a caller-sized value:
//     FactualPlacement, PolicyDecision, PlacementEvidence and PayoutEvidence.
//     A lane measured them at 1.008x, 1.007x, 1.008x and 1.008x the edit's own
//     width; the concrete path is a struct COPY, which carries the unexported
//     witness, widened on ErrorClass -- the very field the local-error repair
//     above was shown -- and handed to DerivePlacement, where derived() is the
//     first thing touched and every binding gate is skipped. All four carry a
//     framed width now. MEASURED AFTER, one byte against 1 MiB: 2,480 / 2,480,
//     1,728 / 1,728, 4,688 / 4,688 and 25,856 / 25,856 -- each identical to
//     the byte, as the two result types already were at 4,048.
//     THE SENTENCE THAT HID IT IS RETRACTED. This entry, and the test beside
//     it, said the framing of a wide error class "cannot be removed without
//     weakening the witness". True of a GENUINE placement, whose class is an
//     artifact the dataset carries; false of an EDITED one, where the value is
//     not the dataset's and the removal is exactly the preflight this same
//     round had just added two files over. The claim now names which case it
//     is about.
//     TWO SMALLER THINGS THE SAME LANES FOUND. A width computed WITHOUT the
//     length-only mode returns zero, and zero equals the zero a mint would then
//     have recorded, so dropping the mode made the preflight VACUOUS rather
//     than failing: every derived() now requires the width to be positive, so
//     the same slip refuses genuine values loudly instead. And
//     sessionRefusalKinds sized a bounded answer -- the distinct members of a
//     closed vocabulary -- with a capacity hint taken from the caller's list,
//     which is the posting-list defect one function over; the hint is gone.
//     THE TEST'S OWN INSTRUMENT WAS WRONG TWICE, and both are recorded because
//     a measurement this round rests on is worth no more than the way it was
//     taken. The first build constructed the 1 MiB edits INSIDE the measured
//     closure and read 2.01x on every seam -- strings.Repeat and the
//     concatenation, not the framing. The second subtracted two MemStats deltas
//     in UNSIGNED arithmetic, so a cold first reading higher than the second
//     wrapped and reported a growth of 1.76e13: an instrument failing loudly in
//     the one direction that means "no growth". Both fixed, and the helper that
//     replaces them says why in its own comment.
//     AND THE ENUMERATION ABOVE WAS ITSELF WRONG, which a Q3 lane found on the
//     published head -- the FOURTEENTH occurrence, and the first where the
//     sentence that missed the neighbour was written by the repair. It said
//     five seams carried a witness and four took a caller-sized value. The
//     package has SEVEN witness-bearing types, and the seventh,
//     VerifiedP3bRuleset, takes one: VerifyP3bRuleset requires RulesetID to
//     EQUAL the config's ConfigID, which carries no per-string length bound, so
//     check re-framed a copied-and-widened identity on every ConfigCopy, Digest
//     and EvaluateP3bCase to answer a refusal decided by one string comparison.
//     MEASURED: 856 B/op at one byte against 1,057,053 at 1 MiB, 1.0073x the
//     edit; AFTER: 0 and 16. All seven carry a width now.
//     THE OBJECTION RECORDED AGAINST CLOSING IT IS RETRACTED. check's own
//     comment called the cost "the price of leaving the identity fields
//     exported" and said removing it meant sealing those too. It does not: the
//     width is recorded at the mint and compared first, with the identity
//     fields left exported, exactly as at the other six seams.
//     THE COUNT IS NO LONGER PROSE. Three rounds enumerated these seams by
//     hand and three enumerations were short.
//     TestEveryWitnessBearingTypeCarriesAFramedWidth walks the production files
//     and fails on any witness-bearing struct without a width, naming it, with
//     its own synthetic control; a new witness type is a failing test rather
//     than a sentence someone forgets to update.
//     WHAT A WIDTH IS AND IS NOT. It is a cheap REJECTION TEST: it refuses a
//     width-changing edit in O(fields) before a byte is materialized. It is not
//     proof of identity -- two different values can frame to one width -- so an
//     equal-width alteration passes it and is refused by the witness, which is
//     the only thing here that verifies content. Nor is it an O(1) claim for
//     genuine input: a handle carrying a large VALID identity still frames it
//     once per use, because the witness must cover what the caller really
//     supplied. Both halves are driven, in the refusal test and in its
//     equal-width and wide-genuine subtests.
//     AND THE POSITIVE-WIDTH TERM IS DEFENCE IN DEPTH, RETAINED KNOWINGLY. A
//     lane showed that deleting `framedLen > 0` from every derived() leaves the
//     suite green, because `witness != ""` already excludes the zero value for
//     every producer this package currently has. That makes the mutant
//     equivalent TODAY, not the term useless: it is what makes the vacuous-mode
//     slip above fail loudly if a future mint ever records a zero. It stays,
//     and no test is added that would only pin an equivalent mutant.
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
