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
//     [ClaimSourceRound], [ReconcileSourceRounds].
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
//  11. Deterministic entropy: [GenerateEntropyWords], [ValidateEntropyWords],
//     [BuildDrawTrace], [ValidateDrawTrace], [EntropyAlgorithmVersion].
//  12. Typed quality / UNKNOWN / exclusion handling: [Quality],
//     [QualityRecord], [Int64Fact], [AssessCaseQuality].
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
// ([VerifyCommonFactset]); a WINNER_KNOWN or REFUND resolution is re-projected
// from its own facts ([VerifyResolutionArtifact]); a factset that must be the
// dataset's is rebuilt from the dataset ([ProjectFactualPlacement],
// [AssessCaseQuality]); a policy result is bound to the factset it was
// evaluated over before it becomes a [PolicyDecision]; a placement and a
// payout carry the case they settle and are checked against the decision;
// a [VerifiedP3bRuleset], a [PolicyDecision], a [FactualPlacement] and a
// [PlacementEvidence] each carry an unexported witness only their producer
// can set, so a hand-built, edited or stored-and-reloaded one settles
// nothing (an edited one is named by its specific contradiction first, a
// consistent but underived one as exactly that); a source-
// round claim is derived from the dataset by [ClaimSourceRound] before
// [ReconcileSourceRounds] reconciles it; and the entropy coordinates must
// name the factset's own round. The trusted path is
// therefore IN-PROCESS derivation from the dataset: a runner that stores
// artifacts re-derives them before it settles anything on them, and
// [AssessCaseQuality] is the gate that decides whether a case counts. What
// no pure function can do is authenticate that a supplied reference EXISTS:
// "proven" always means "the supplied proof was checked for its binding",
// and the observation identities it names are what the reader's audit
// follows.
//
// # Purity
//
// Every function is a pure function of its arguments: no database, no
// network, no clock, no environment, no global RNG, no goroutines. The only
// first-party import is the predictioneval package itself, whose own fence
// already excludes every capability package. TestP4OfflineDependencyFence
// enforces this package's fence over its whole transitive import graph.
//
// # Assumptions stated for the owner
//
// The following readings could not be settled from the contract text this
// session had. Each is applied conservatively, named where it bites, and is
// this package's PROVISIONAL definition rather than contract fact:
//
//   - the resolution proof obligations, versioned as
//     [ResolutionObligationsRevision] and carried in every artifact, so that
//     aligning them with the contract's own text is a visible identity
//     change;
//   - a case in which either policy made no choice — a POLICY_SKIP or a
//     NO_ATTEMPT_IN_SUPPLIED_PREFIX — is DESCRIPTIVE_ONLY for the primary
//     POLICY_CHOICE_ACCURACY metric ([AssessCaseQuality]);
//   - the one accepted platform-acceptance basis for a placement,
//     [ProofBasisPlatformPredictionConfirmed], and the one accepted linkage
//     basis for a payout record, [LinkageBasisAttemptLinkedUserTerminal]:
//     the package checks a supplied proof for its basis and binding and
//     names no other basis;
//   - a bet-only denominator counts exactly the legal WOULD_ATTEMPT
//     decisions ([PlacementEvidence]): the contract fixes only that a
//     POLICY_SKIP contributes nothing to it;
//   - the attribution of raw facts to an episode ([SelectEpisodes]): a fact
//     bears on an episode when it names the same public round in any
//     incarnation, the same incarnation, or — conservatively — no
//     incarnation and no round any episode of the session carries (admitted
//     or excluded), on the same pool. The last rule can only exclude an
//     episode or break its boundary, never admit one.
//
// # Honest source limitations
//
// The owner's task contract names a separate engineering-contract document
// and a set of independent entropy vectors that were NOT present in the
// repository or the session. Where this package needed their exact text — the
// resolution proof obligations, the entropy framing — it defines the rule
// precisely in the doc comment of the function that applies it, and the
// vectors it is tested against were generated by an independent
// implementation (see testdata/synthetic/PROVENANCE.md). Nothing here was
// checked against real P1/P1.5 data: no production dataset is proven
// available, and the synthetic fixtures are implementation evidence only.
package p4offline
