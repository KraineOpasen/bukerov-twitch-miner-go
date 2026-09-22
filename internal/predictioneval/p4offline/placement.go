package p4offline

import (
	"errors"
	"strconv"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// SEAM 9: evidence-only placement, p4-placement-evidence-only/v1.
//
// The only placement that ever happened is the FACTUAL one: the selected
// attempt's own CALL_STARTED/CALL_RETURNED pair. A policy's decision may be
// attributed that placement only when the decision IS the factual decision —
// the same attempt, the same outcome, the same stake — because a different
// choice, a different stake or a different call time describes a call nobody
// made. And even an attributed call proves less than it looks: a local
// CALL_RETURNED with a nil error means the local call returned, not that the
// platform accepted the prediction. Acceptance needs a separate proof bound
// to that exact call.
//
// "Proven" here means exactly this much: a supplied proof reference was
// checked for its basis and its binding to the call and the round. A pure
// function cannot authenticate that the reference exists; the reader's audit
// can, through the observation identity the proof names.
//
// # Derived evidence carries a witness
//
// [PolicyDecision], [FactualPlacement] and [PlacementEvidence] are DERIVED
// artifacts: the first from a policy result and its factset by
// [P2CaseResult.Decision] / [P3bCaseResult.Decision], the second from the
// dataset by [ProjectFactualPlacement], the third from a decision and a
// factual placement by [DerivePlacement]. Each carries an unexported witness
// over its own fields that only its producer sets. Every consumer refuses an
// artifact without a valid witness, so a hand-built or edited artifact — and
// an artifact read back from storage — settles nothing: the trusted path is
// in-process derivation from the dataset, and a runner that stores artifacts
// re-derives them before it settles money on them. The consistency checks a
// consumer makes BEFORE the witness (binding, legality, a skip's exact zero,
// a choice's index against its identity, a stake's sign) name the specific
// contradiction of an edited artifact; an artifact that is consistent but
// not derived is refused as exactly that.
//
// Nothing here imputes success, infers a retry, or repairs a pool.

// PlacementStatus is the closed placement vocabulary.
type PlacementStatus string

// Placement statuses.
const (
	// PlacementAcceptedPlatformProven — the factual call is attributed and a
	// platform acceptance proof bound to that call was supplied.
	PlacementAcceptedPlatformProven PlacementStatus = "ACCEPTED_PLATFORM_PROVEN"
	// PlacementLocalOKPlatformUnproven — the factual call is attributed and
	// returned with a nil local error, and nothing proves the platform
	// accepted it.
	PlacementLocalOKPlatformUnproven PlacementStatus = "LOCAL_OK_PLATFORM_UNPROVEN"
	// PlacementLocalErrorPlatformUnknown — the factual call returned with a
	// local error. Whether the platform saw it is unknown.
	PlacementLocalErrorPlatformUnknown PlacementStatus = "LOCAL_ERROR_PLATFORM_UNKNOWN"
	// PlacementNotReturned — RESERVED, and NOT EMITTABLE through the D16
	// pipeline.
	//
	// It names a factual call that started and never returned. Under the
	// owner's D16 disposition that is not an admissible derived outcome: P4
	// admits only COMPLETE + AS_FINALIZED sources, and such a source cannot
	// carry a factual automatic CALL_STARTED without its CALL_RETURNED, so an
	// unspent start is a contradiction in the evidence rather than a placement
	// result. classifySignals refuses it there — before any factual placement
	// can be minted — so no trusted path reaches the branch below that names
	// this status, and the negative proof of that lives in placement_test.go.
	//
	// The constant is retained rather than deleted to avoid pre-merge API and
	// vocabulary churn, and the branch is retained as defence in depth behind
	// FactualPlacement's producer-only witness. Neither is a path: nothing here
	// invents a way to emit it.
	PlacementNotReturned PlacementStatus = "NOT_RETURNED"
	// PlacementNotRecorded — the factual decision placed and no call exists.
	PlacementNotRecorded PlacementStatus = "NOT_RECORDED"
	// PlacementIncoherent — the recorded call pair has a shape the producer
	// cannot write.
	PlacementIncoherent PlacementStatus = "INCOHERENT"
	// PlacementCounterfactualNotInheritable — the policy's decision differs
	// from the factual one in choice or stake.
	PlacementCounterfactualNotInheritable PlacementStatus = "COUNTERFACTUAL_NOT_INHERITABLE"
	// PlacementNotApplicable — a POLICY_SKIP has no placement.
	PlacementNotApplicable PlacementStatus = "NOT_APPLICABLE"
	// PlacementUnknown — nothing can be established.
	PlacementUnknown PlacementStatus = "UNKNOWN"
)

// ProofBasisPlatformPredictionConfirmed is the only accepted platform
// acceptance basis: the platform's own confirmation of the user's prediction.
// It is this package's own, narrower vocabulary (see the package
// documentation's reconciled readings), not contract text.
const ProofBasisPlatformPredictionConfirmed = "PLATFORM_PREDICTION_CONFIRMED"

// Decision refusals shared by the placement and payout seams: a decision that
// is not bound, legal and self-consistent settles nothing.
const (
	DecisionReasonPolicyBindingMismatch       = "POLICY_BINDING_MISMATCH"
	DecisionReasonCaseBindingMismatch         = "CASE_BINDING_MISMATCH"
	DecisionReasonIllegalNativeShape          = "ILLEGAL_NATIVE_SHAPE"
	DecisionReasonPolicySkipStakeNotZero      = "POLICY_SKIP_STAKE_NOT_EXACT_ZERO"
	DecisionReasonChoiceIndexIdentityMismatch = "CHOICE_INDEX_ID_MISMATCH"
	DecisionReasonStakeNegative               = "STAKE_NEGATIVE"
	DecisionReasonNotDerived                  = "DECISION_NOT_DERIVED"
	// DecisionReasonIdentityWithheldPrefix heads the one reason a refused
	// decision's artifact carries beside its category: the EXTENT of the
	// identity text that artifact does not echo.
	//
	// It is a prefix and not a closed value because an extent is a number.
	// What follows it is arithmetic over lengths -- never a byte of the text
	// itself, which is the whole of the rule and admits no second reading.
	// PlacementReasonLocalErrorClassWithheldPrefix is the same rule at the
	// local-error arm; an earlier spelling of that one carried the class's
	// TEXT, which is how this vocabulary came to hold an unbounded payload
	// while this paragraph described a bounded one.
	DecisionReasonIdentityWithheldPrefix = "IDENTITY_WITHHELD:"
)

// Placement reasons. Closed vocabulary.
const (
	PlacementReasonCaseBindingMismatch   = DecisionReasonCaseBindingMismatch
	PlacementReasonFactualNotDerived     = "FACTUAL_PLACEMENT_NOT_DERIVED"
	PlacementReasonPolicyStakeUnknown    = "POLICY_STAKE_UNKNOWN"
	PlacementReasonChoiceMissing         = "CHOICE_MISSING"
	PlacementReasonChoiceDiffers         = "CHOICE_DIFFERS"
	PlacementReasonChoiceIdentityDiffers = "CHOICE_IDENTITY_DIFFERS"
	PlacementReasonTerminalSlotDiffers   = "TERMINAL_SLOT_DIFFERS"
	PlacementReasonStakeDiffers          = "STAKE_DIFFERS"
	PlacementReasonFactualNotPlace       = "FACTUAL_DECISION_NOT_PLACE"
	PlacementReasonCallNotAfterCutoff    = "CALL_NOT_AFTER_CUTOFF"
	PlacementReasonCallContradictsRecord = "CALL_CONTRADICTS_RECORDED_DECISION"
	PlacementReasonProofBasisNotAccepted = "PROOF_BASIS_NOT_ACCEPTED"
	PlacementReasonProofNotBoundToCall   = "PROOF_NOT_BOUND_TO_CALL"
	PlacementReasonProofRoundMismatch    = "PROOF_ROUND_MISMATCH"
	PlacementReasonProofIncomplete       = "PROOF_INCOMPLETE"
	PlacementReasonNoProofSupplied       = "NO_PLATFORM_ACCEPTANCE_PROOF"
	// PlacementReasonLocalErrorClassWithheldPrefix heads the local-error arm's
	// one reason: the EXTENT of the producer's error class, never the class.
	//
	// THE OFFLINE CLASS IS SUPPLIER TEXT, EVEN THOUGH THE LIVE PRODUCER'S
	// VOCABULARY IS CLOSED. placementStatusCoherent admits any class other than
	// NONE whenever the reason code is not OK. Enforcing the producer vocabulary
	// would change which supplied artifacts are admitted. This repair preserves
	// that domain and reports every class by extent. A review lane measured what
	// echoing it costs -- the artifact copies the class into Reasons and
	// placementEvidenceWitness then frames that copy, so a hand-built record
	// turns a fail-closed placement into payload-sized allocations and diagnostic
	// text.
	//
	// SO IT IS NOT ECHOED, AND THE REPORTING LOSS IS REAL AND RECORDED. A
	// caller reading this reason learns that a local error was recorded and
	// how wide its class was, and no longer which class it was. The status,
	// PlacementLocalErrorPlatformUnknown, already carries what the verdict
	// turns on; the class was diagnostic colour, and diagnostic colour is not
	// worth an unbounded echo of text this package never verified.
	PlacementReasonLocalErrorClassWithheldPrefix = "LOCAL_ERROR_CLASS_WITHHELD:"
	PlacementReasonIdentityWithheldPrefix        = DecisionReasonIdentityWithheldPrefix
)

// ErrDecisionBinding is a policy result bound to a factset other than the one
// it is being attached to.
var ErrDecisionBinding = errors.New("p4offline: the policy result is not bound to this factset")

// ErrResultNotDerived is a policy result that no evaluator of this package
// produced as it is: a hand-built, edited or stored-and-reloaded
// [P2CaseResult] or [P3bCaseResult]. A decision is minted only from the
// evaluator's own result.
var ErrResultNotDerived = errors.New("p4offline: the policy result was not produced by this package's evaluator or was altered afterwards")

// FactualPlacement is the selected attempt's own placement evidence, bound
// to the case. It is derived from the dataset, never from a selection, and
// carries a witness only [ProjectFactualPlacement] sets.
type FactualPlacement struct {
	Attempt        predictioneval.AttemptKey `json:"attempt"`
	FactsetDigest  string                    `json:"factsetDigest"`
	EventID        string                    `json:"eventId"`
	CutoffPosition int64                     `json:"cutoffPosition"`
	// The factual DECISION, from the terminal envelope's recorded results:
	// the chosen index, the outcome identity the store recorded for it (empty
	// when the store carries none), the final amount, and the slot the
	// terminal fact ITSELF named — the pinned producer writes it on every
	// placing terminal fact, independently of the envelope's index.
	TerminalDecision        string `json:"terminalDecision"`
	RecordedChoiceIndex     *int   `json:"recordedChoiceIndex,omitempty"`
	RecordedChoiceOutcomeID string `json:"recordedChoiceOutcomeId,omitempty"`
	RecordedFinalAmount     *int64 `json:"recordedFinalAmount,omitempty"`
	RecordedTerminalSlot    *int   `json:"recordedTerminalSlot,omitempty"`
	// The factual CALL, from the post-decision facts. Coherence is the P2
	// settlement projection's own verdict; StartedOnly separates the one
	// half-present shape that is evidentially distinct — a call that started
	// and never returned — from every other incoherent shape.
	Coherence                string `json:"coherence"`
	StartedOnly              bool   `json:"startedOnly"`
	CallPresent              bool   `json:"callPresent"`
	CallStartedObservationID string `json:"callStartedObservationId,omitempty"`
	CallStartedPosition      int64  `json:"callStartedPosition"`
	Stake                    *int64 `json:"stake,omitempty"`
	Slot                     *int   `json:"slot,omitempty"`
	Returned                 bool   `json:"returned"`
	LocalReasonOK            bool   `json:"localReasonOk"`
	ErrorClass               string `json:"errorClass,omitempty"`
	witness                  string
	// framedLen is the width witness was computed over. Producer-only, like
	// the witness itself.
	framedLen int
}

// derived reports whether the value is exactly what ProjectFactualPlacement
// produced.
func (f FactualPlacement) derived() bool {
	return f.witness != "" && f.framedLen > 0 &&
		f.framedLen == factualPlacementFramedLen(f) && f.witness == factualPlacementWitness(f)
}

func factualPlacementWitness(f FactualPlacement) string {
	var c canonical
	frameFactualPlacement(&c, f)
	return c.digest()
}

// factualPlacementFramedLen is the width factualPlacementWitness frames, computed by the SAME pass with
// bytes switched off. See [P3bCaseResult.derived] for why the width is
// checked before the witness.
func factualPlacementFramedLen(f FactualPlacement) int {
	c := canonical{lenOnly: true}
	frameFactualPlacement(&c, f)
	return c.framedLen()
}

func frameFactualPlacement(c *canonical, f FactualPlacement) {
	c.str("p4offline-factual-placement-witness")
	c.i64(f.Attempt.CollectorEpoch)
	c.str(f.Attempt.CollectorSessionID)
	c.str(f.Attempt.PoolInstanceID)
	c.u64(f.Attempt.AttemptID)
	c.str(f.FactsetDigest)
	c.str(f.EventID)
	c.i64(f.CutoffPosition)
	c.str(f.TerminalDecision)
	serializeOptionalInt(c, f.RecordedChoiceIndex)
	c.str(f.RecordedChoiceOutcomeID)
	serializeOptionalInt64(c, f.RecordedFinalAmount)
	serializeOptionalInt(c, f.RecordedTerminalSlot)
	c.str(f.Coherence)
	c.boolean(f.StartedOnly)
	c.boolean(f.CallPresent)
	c.str(f.CallStartedObservationID)
	c.i64(f.CallStartedPosition)
	serializeOptionalInt64(c, f.Stake)
	serializeOptionalInt(c, f.Slot)
	c.boolean(f.Returned)
	c.boolean(f.LocalReasonOK)
	c.str(f.ErrorClass)
}

// PlatformAcceptanceProof is supplied evidence that the platform accepted a
// specific call.
type PlatformAcceptanceProof struct {
	Basis             string            `json:"basis"`
	CallObservationID string            `json:"callObservationId"`
	Reference         EvidenceReference `json:"reference"`
	ProofRevision     string            `json:"proofRevision"`
}

// PolicyDecision is what one policy decided on one case, bound to the case.
// It is produced by [P2CaseResult.Decision] and [P3bCaseResult.Decision],
// which refuse a factset other than the one the result was evaluated over
// and a result the evaluator did not produce as it is (the result carries
// its own witness), and it carries a witness only they set.
type PolicyDecision struct {
	Policy         string                    `json:"policy"`
	Attempt        predictioneval.AttemptKey `json:"attempt"`
	FactsetDigest  string                    `json:"factsetDigest"`
	EventID        string                    `json:"eventId"`
	CutoffPosition int64                     `json:"cutoffPosition"`
	// Derivation names what produced this decision, so two genuine
	// decisions of one policy on one case are distinguishable: for P2 the
	// per-case config binding digest; for P3b the verified ruleset, its
	// native config digest, the entropy run identity and the core's entropy
	// digest. It is descriptive provenance for the audit, covered by the
	// witness; it grants nothing.
	Derivation string `json:"derivation"`
	// OutcomeIDs is the factset's ordered outcome identity vector: the set
	// the choice was made from, carried so a choice's index and identity can
	// be checked against each other and against a resolution's set.
	OutcomeIDs []string      `json:"outcomeIds"`
	Action     ActionMapping `json:"action"`
	Choice     PolicyChoice  `json:"choice"`
	Stake      Int64Fact     `json:"stake"`
	witness    string
	// framedLen is the width witness was computed over. Producer-only, like
	// the witness itself.
	framedLen int
}

// derived reports whether the value is exactly what a Decision method
// produced.
func (p PolicyDecision) derived() bool {
	return p.witness != "" && p.framedLen > 0 &&
		p.framedLen == policyDecisionFramedLen(p) && p.witness == policyDecisionWitness(p)
}

func policyDecisionWitness(p PolicyDecision) string {
	var c canonical
	framePolicyDecision(&c, p)
	return c.digest()
}

// policyDecisionFramedLen is the width policyDecisionWitness frames, computed by the SAME pass with
// bytes switched off. See [P3bCaseResult.derived] for why the width is
// checked before the witness.
func policyDecisionFramedLen(p PolicyDecision) int {
	c := canonical{lenOnly: true}
	framePolicyDecision(&c, p)
	return c.framedLen()
}

func framePolicyDecision(c *canonical, p PolicyDecision) {
	c.str("p4offline-policy-decision-witness")
	c.str(p.Policy)
	c.i64(p.Attempt.CollectorEpoch)
	c.str(p.Attempt.CollectorSessionID)
	c.str(p.Attempt.PoolInstanceID)
	c.u64(p.Attempt.AttemptID)
	c.str(p.FactsetDigest)
	c.str(p.EventID)
	c.i64(p.CutoffPosition)
	c.str(p.Derivation)
	c.count(len(p.OutcomeIDs))
	for _, id := range p.OutcomeIDs {
		c.str(id)
	}
	frameAction(c, p.Action)
	frameChoice(c, p.Choice)
	frameFact(c, p.Stake)
}

// frameAction, frameChoice and frameFact frame the three values every
// policy result and decision carries, so the result witnesses and the
// decision witness cover them identically.
func frameAction(c *canonical, a ActionMapping) {
	c.str(a.MapVersion)
	c.str(a.Policy)
	c.str(a.NativeAction)
	c.str(string(a.Class))
	c.boolean(a.Legal)
	c.count(len(a.Illegality))
	for _, r := range a.Illegality {
		c.str(r)
	}
	c.str(a.SkipReason)
}

func frameChoice(c *canonical, ch PolicyChoice) {
	c.boolean(ch.Present)
	c.i64(int64(ch.Index))
	c.str(ch.OutcomeID)
}

func frameFact(c *canonical, f Int64Fact) {
	c.str(string(f.Presence))
	c.i64(f.Value)
	c.str(f.Reason)
}

// PlacementEvidence is the evidence-only placement verdict for one policy,
// bound to its case, carrying a witness only [DerivePlacement] sets.
//
// A placement verdict states no denominator membership: "would attempt" is
// not "placed bet". Membership in the PLACED_BET_WIN_RATE denominator is a
// fact about the SETTLED, platform-proven bet, stated by
// [PayoutEvidence.PlacedBetDenominatorMember] and composed with the case's
// quality by [AssessDenominatorMembership].
type PlacementEvidence struct {
	ContractVersion string                    `json:"contractVersion"`
	Policy          string                    `json:"policy"`
	Attempt         predictioneval.AttemptKey `json:"attempt"`
	FactsetDigest   string                    `json:"factsetDigest"`
	EventID         string                    `json:"eventId"`
	Status          PlacementStatus           `json:"status"`
	Reasons         []string                  `json:"reasons,omitempty"`
	// PolicyStake is the policy's OWN stake, carried verbatim whatever the
	// placement verdict: withholding a call never changes what the policy
	// would have staked, and a skip's exact zero stays an exact zero.
	//
	// ON A REFUSED DECISION IT IS UNKNOWN, naming the refusal. That is not an
	// exception to the sentence above but its precondition: there is no
	// policy stake to carry until the decision has been accepted as one this
	// package produced.
	PolicyStake                 Int64Fact `json:"policyStake"`
	AttributedOutcomeID         string    `json:"attributedOutcomeId,omitempty"`
	AttributedCallObservationID string    `json:"attributedCallObservationId,omitempty"`
	AttributedCallPosition      int64     `json:"attributedCallPosition"`
	witness                     string
	// framedLen is the width witness was computed over. Producer-only, like
	// the witness itself.
	framedLen int
}

// derived reports whether the value is exactly what DerivePlacement produced.
func (p PlacementEvidence) derived() bool {
	return p.witness != "" && p.framedLen > 0 &&
		p.framedLen == placementEvidenceFramedLen(p) && p.witness == placementEvidenceWitness(p)
}

func placementEvidenceWitness(p PlacementEvidence) string {
	var c canonical
	framePlacementEvidence(&c, p)
	return c.digest()
}

// placementEvidenceFramedLen is the width placementEvidenceWitness frames, computed by the SAME pass with
// bytes switched off. See [P3bCaseResult.derived] for why the width is
// checked before the witness.
func placementEvidenceFramedLen(p PlacementEvidence) int {
	c := canonical{lenOnly: true}
	framePlacementEvidence(&c, p)
	return c.framedLen()
}

func framePlacementEvidence(c *canonical, p PlacementEvidence) {
	c.str("p4offline-placement-evidence-witness")
	c.str(p.ContractVersion)
	c.str(p.Policy)
	c.i64(p.Attempt.CollectorEpoch)
	c.str(p.Attempt.CollectorSessionID)
	c.str(p.Attempt.PoolInstanceID)
	c.u64(p.Attempt.AttemptID)
	c.str(p.FactsetDigest)
	c.str(p.EventID)
	c.str(string(p.Status))
	c.count(len(p.Reasons))
	for _, r := range p.Reasons {
		c.str(r)
	}
	frameFact(c, p.PolicyStake)
	c.str(p.AttributedOutcomeID)
	c.str(p.AttributedCallObservationID)
	c.i64(p.AttributedCallPosition)
}

// decisionOf binds a policy result to the factset it was evaluated over.
//
// THE DERIVATION ARRIVES AS A THUNK, NOT A STRING, and that is the whole point
// of the signature. Go evaluates call arguments before the call, so a
// derivation built at the call site is fully materialized BEFORE the first gate
// below runs -- and both callers build theirs from plain exported fields of a
// caller-supplied result that nothing has length-bounded. An independent sweep
// reached it through the exported P3bCaseResult.Decision with a 1 MiB ruleset
// id and measured 3,686,696 bytes allocated on a call that then refuses.
//
// This is the FIFTH instance of the same class on this branch, and it is the
// one that says what the rule really is. It had been scoped to "the first
// statement of an exported function" and then to "any gate whose operand is
// caller-supplied": this site is neither. The scope that survives is the one
// stated on suppliedTextExtent -- materialization on a path that has not yet
// bounded what it is materializing, wherever it sits and whether or not it is
// a gate.
//
// AND THE THUNK ALONE WAS NOT THE WHOLE REPAIR. The sixth instance was the
// same call site one gate later: see the mint check below. A thunk moves a
// materialization behind the gates it is written above; it says nothing about
// the gates it is written below.
func decisionOf(policy string, fs CommonFactset, resultDigest string, action ActionMapping, choice PolicyChoice,
	stake Int64Fact, derivation func() string, minted func() bool) (PolicyDecision, error) {
	if err := VerifyCommonFactset(fs); err != nil {
		return PolicyDecision{}, err
	}
	// ONLY ONE OF THE TWO IS UNGATED, and an earlier version of this comment
	// said neither was. VerifyCommonFactset ran three lines up and returns nil
	// only after fs.Digest == commonFactsetDigest(fs), which is hexEncode of a
	// SHA-256: exactly 64 lower-case hex characters of this package's own
	// making. So the factset side IS gated, and reporting it by extent rendered
	// the constant "64 bytes" for every input the gate can see -- turning a
	// real diagnosis into a sentence that reads the same for every binding
	// mismatch there is. It is named, exactly as derivedOpportunity already
	// names it under the identical precondition. resultDigest is the ungated
	// one: a plain exported field on the supplied result.
	//
	// THE EMPTY CHECK IS DEAD AND STAYS, recorded rather than left to be
	// rediscovered: fs.Digest is 64 hex characters by the precondition above,
	// so an empty resultDigest already fails the comparison beside it and
	// deleting the clause passes the whole suite. It is kept because it states
	// the intent at the site -- an absent digest binds nothing -- and it would
	// start carrying weight the moment this gate is reached on a path where
	// fs.Digest is not already proved non-empty.
	if resultDigest == "" || resultDigest != fs.Digest {
		return PolicyDecision{}, errors.Join(ErrDecisionBinding,
			errors.New("p4offline: result was evaluated over a factset digest of "+suppliedTextExtent(resultDigest)+
				", not this factset's "+strconv.Quote(fs.Digest)))
	}
	if action.Policy != policy || action.MapVersion != NativeActionMapVersion {
		return PolicyDecision{}, errors.Join(ErrDecisionBinding, errors.New("p4offline: action mapping is not this policy's"))
	}
	// THE SIXTH INSTANCE OF THE CLASS WAS HERE, one gate later than the fifth.
	// The thunk stopped the derivation being built before the gates ABOVE ran;
	// it did not stop it being built before the check that actually refuses a
	// caller-built result. Both callers used to mint the whole decision --
	// derivation and canonical witness framing -- and only then ask whether the
	// result was theirs at all. A caller cannot set the unexported witness, so
	// any result built outside this package refuses here for certain, in ONE
	// string comparison: an independent sweep measured 75,527,248 bytes on a
	// 16 MiB ruleset id, 4.50x the input, for a refusal that costs O(1).
	//
	// The check moves in here rather than to the top of the callers, and the
	// ORDER is the reason. Both methods document that a binding contradiction
	// is named FIRST and an underived result second, and a witness test at the
	// top of the method inverts exactly that for the input this repair is
	// about. So it sits below the binding gates, above the materialization.
	if !minted() {
		return PolicyDecision{}, ErrResultNotDerived
	}
	d := PolicyDecision{
		Policy:         policy,
		Attempt:        fs.Attempt,
		FactsetDigest:  fs.Digest,
		EventID:        fs.Episode.EventID,
		CutoffPosition: fs.CutoffPosition,
		Derivation:     derivation(),
		OutcomeIDs:     outcomeIDs(fs),
		Action:         action,
		Choice:         choice,
		Stake:          stake,
	}
	d.witness, d.framedLen = policyDecisionWitness(d), policyDecisionFramedLen(d)
	return d, nil
}

// decisionRefusal names the first reason a decision settles nothing: an
// unbound policy, an illegal native shape, a POLICY_SKIP without its exact
// zero, a choice whose index and identity disagree with the outcome vector
// it was made from, a negative stake, or — last, so an edited decision is
// named by its contradiction first — a decision no Decision method
// produced. An empty string is a usable decision.
func decisionRefusal(p PolicyDecision) string {
	switch {
	case p.Policy != PolicyP2 && p.Policy != PolicyP3b:
		return DecisionReasonPolicyBindingMismatch
	case p.Action.Policy != p.Policy || p.Action.MapVersion != NativeActionMapVersion:
		return DecisionReasonPolicyBindingMismatch
	case p.FactsetDigest == "" || p.EventID == "":
		return DecisionReasonCaseBindingMismatch
	case !p.Action.Legal:
		return DecisionReasonIllegalNativeShape
	case p.Action.Class == ActionPolicySkip && (!p.Stake.Known() || p.Stake.Value != 0):
		return DecisionReasonPolicySkipStakeNotZero
	case p.Choice.Present && (p.Choice.Index < 0 || p.Choice.Index >= len(p.OutcomeIDs) ||
		p.Choice.OutcomeID == "" || p.OutcomeIDs[p.Choice.Index] != p.Choice.OutcomeID):
		return DecisionReasonChoiceIndexIdentityMismatch
	case p.Stake.Known() && p.Stake.Value < 0:
		// No genuine result carries a negative stake; this names an edited
		// decision's contradiction ahead of the witness. Defence in depth.
		return DecisionReasonStakeNegative
	case !p.derived():
		return DecisionReasonNotDerived
	}
	return ""
}

// refusedDecisionIdentity reports, BY EXTENT ONLY, the identity text that a
// refused decision's artifact does not echo.
//
// A DECISION decisionRefusal REJECTED WAS NEVER PROVED TO BE THIS PACKAGE'S,
// so every string on it is the caller's, bounded by nothing. Echoing those
// into an artifact -- and then hashing them into its witness -- turns a
// refusal decided by one comparison into work proportional to what the caller
// supplied. The artifact still has to NAME the case it refused, so the naming
// becomes arithmetic: how wide each withheld field was, and not one byte of
// it. Two refusals of different cases stay distinguishable by their extents,
// their numeric attempt key and their category, which is what keeps a refused
// case from disappearing in silence.
//
// IT NAMES THE FIELDS THE PLACEMENT ARTIFACT WOULD HAVE ECHOED. The payout
// artifact echoes one more, so it calls the wrapper below rather than
// re-spelling the list; a second spelling is a second place a future field can
// be forgotten.
func refusedDecisionIdentity(p PolicyDecision) string {
	return DecisionReasonIdentityWithheldPrefix +
		"policy " + suppliedTextExtent(p.Policy) +
		", session " + suppliedTextExtent(p.Attempt.CollectorSessionID) +
		", pool " + suppliedTextExtent(p.Attempt.PoolInstanceID) +
		", factset digest " + suppliedTextExtent(p.FactsetDigest) +
		", event " + suppliedTextExtent(p.EventID)
}

// refusedDecisionIdentityWithDerivation is the same sentence for the payout
// seam, whose artifact also echoes the decision's derivation.
func refusedDecisionIdentityWithDerivation(p PolicyDecision) string {
	return refusedDecisionIdentity(p) + ", derivation " + suppliedTextExtent(p.Derivation)
}

// namedPolicy is the policy name a refused artifact may still carry: one this
// package RECOGNIZES, and otherwise nothing.
//
// IT IS NOT AN EXCEPTION TO THE WITHHOLDING, it is the same rule read exactly.
// What a refused artifact may not echo is unverified text of unbounded extent.
// A policy name that equals PolicyP2 or PolicyP3b is neither: the comparison
// is decisionRefusal's own first clause, it admits two three-byte constants,
// and a 1 MiB policy name fails it and is withheld like everything else. What
// is carried is this package's constant matched by value, not the caller's
// string carried on trust.
//
// IT IS ALSO WHAT KEEPS A REFUSAL'S CATEGORY WHERE IT WAS. A payout compares
// the placement's policy before it compares the rest of the case, so a
// placement that named no policy at all would be refused as a POLICY binding
// mismatch -- which is true, and which hides that the placement refused its
// own decision. Carrying the recognized name lets that comparison pass and the
// case comparison below it speak, exactly as before this withholding existed.
func namedPolicy(p string) string {
	if p == PolicyP2 || p == PolicyP3b {
		return p
	}
	return ""
}

// namedAttempt is the part of an attempt key a refused artifact may still
// carry: the two fixed-width numbers, never the two caller-supplied strings.
//
// It is not a fabricated identity -- both values are the caller's own, copied
// verbatim -- and it is not a binding: nothing downstream may act on an
// attempt key whose strings are absent, because every consumer compares the
// WHOLE key and an absent string never equals a present one.
func namedAttempt(k predictioneval.AttemptKey) predictioneval.AttemptKey {
	return predictioneval.AttemptKey{CollectorEpoch: k.CollectorEpoch, AttemptID: k.AttemptID}
}

// ProjectFactualPlacement reads the selected attempt's recorded decision and
// its post-decision placement facts through the P2 projections. The factset
// must be exactly what the dataset derives for its episode.
func ProjectFactualPlacement(src PreparedDataset, fs CommonFactset) (FactualPlacement, error) {
	ep, err := derivedOpportunity(src, fs)
	if err != nil {
		return FactualPlacement{}, err
	}
	dc, err := predictioneval.ProjectDecisionCase(*ep.Attempt)
	if err != nil {
		return FactualPlacement{}, err
	}
	settle := predictioneval.ProjectSettlementFacts(*ep.Attempt)
	out := FactualPlacement{
		Attempt:          fs.Attempt,
		FactsetDigest:    fs.Digest,
		EventID:          fs.Episode.EventID,
		CutoffPosition:   fs.CutoffPosition,
		TerminalDecision: dc.Recorded.TerminalDecision,
		Coherence:        settle.PlacementCoherence,
		CallPresent:      settle.PlacementCallStarted || settle.PlacementCallReturned,
		Returned:         settle.PlacementCallReturned,
		LocalReasonOK:    settle.PlacementAccepted,
		ErrorClass:       settle.PlacementErrorClass,
		Stake:            copyInt64(settle.PlacementStake),
	}
	if settle.PlacementSlot != nil {
		slot := *settle.PlacementSlot
		out.Slot = &slot
	}
	if dc.Recorded.ChoiceIndexRecorded {
		idx := dc.Recorded.ChoiceIndex
		out.RecordedChoiceIndex = &idx
	}
	out.RecordedChoiceOutcomeID = dc.Recorded.ChoiceOutcomeID
	if dc.Recorded.TerminalOutcomeSlot != nil {
		slot := *dc.Recorded.TerminalOutcomeSlot
		out.RecordedTerminalSlot = &slot
	}
	if dc.Recorded.FinalAmountRecorded {
		final := dc.Recorded.FinalAmount
		out.RecordedFinalAmount = &final
	}
	started, returned := 0, 0
	for _, r := range ep.Attempt.PostDecision {
		if r.Kind != predictioneval.KindPlacement {
			continue
		}
		switch r.Payload.Phase {
		case predictioneval.PhaseCallStarted:
			started++
			if out.CallStartedObservationID == "" {
				out.CallStartedObservationID = r.ObservationID
				out.CallStartedPosition = r.CollectorSequence
				if out.Stake == nil {
					if v, ok := r.Payload.Counters[predictioneval.CounterStake]; ok {
						out.Stake = &v
					}
				}
				if out.Slot == nil && r.Payload.OutcomeSlot != nil {
					slot := *r.Payload.OutcomeSlot
					out.Slot = &slot
				}
			}
		case predictioneval.PhaseCallReturned:
			returned++
		}
	}
	out.StartedOnly = started == 1 && returned == 0
	out.witness, out.framedLen = factualPlacementWitness(out), factualPlacementFramedLen(out)
	return out, nil
}

// DerivePlacement is seam 9.
func DerivePlacement(policy PolicyDecision, factual FactualPlacement, proof *PlatformAcceptanceProof) PlacementEvidence {
	out := derivePlacement(policy, factual, proof)
	out.witness, out.framedLen = placementEvidenceWitness(out), placementEvidenceFramedLen(out)
	return out
}

func derivePlacement(policy PolicyDecision, factual FactualPlacement, proof *PlatformAcceptanceProof) PlacementEvidence {
	// THE REFUSAL IS ABOVE THE ARTIFACT, AND ITS ARTIFACT ECHOES NO UNVERIFIED
	// IDENTITY. Until decisionRefusal returns empty nothing has established
	// that this decision is one this package produced, so its strings are the
	// caller's and nothing bounds them. The struct literal below is O(1) -- a
	// Go string field copies a two-word header -- but the WITNESS
	// DerivePlacement takes over the returned value is not: it frames Policy,
	// the attempt's two identifiers, FactsetDigest and EventID and hashes
	// them, so a refusal decided by one comparison used to allocate 18.3 MB on
	// a decision carrying five 1 MiB strings. Withholding the text at the
	// source is what makes the witness constant; the witness itself is
	// unchanged.
	//
	// WHAT THE REFUSED ARTIFACT STILL SAYS: its category, the extent of each
	// withheld field, and the attempt's two fixed-width numbers. What it does
	// not say, it does not invent -- an absent identity reads as absent, and
	// every consumer of a placement compares the identity in WHOLE, so an
	// artifact missing one can bind to nothing.
	//
	// THE STAKE IS WITHHELD TOO, and that is a narrower statement than the
	// field's doc above. Int64Fact.Reason is an exported string on a value the
	// caller built, so carrying the stake verbatim carries unbounded text --
	// and worse, it presents a stake this package never accepted as the
	// policy's own. UNKNOWN naming the refusal is what was actually
	// established.
	if why := decisionRefusal(policy); why != "" {
		return PlacementEvidence{
			ContractVersion: PlacementEvidenceVersion,
			Policy:          namedPolicy(policy.Policy),
			Attempt:         namedAttempt(policy.Attempt),
			Status:          PlacementUnknown,
			Reasons:         []string{why, refusedDecisionIdentity(policy)},
			PolicyStake:     UnknownInt64(why),
		}
	}
	out := PlacementEvidence{
		ContractVersion: PlacementEvidenceVersion,
		Policy:          policy.Policy,
		Attempt:         policy.Attempt,
		FactsetDigest:   policy.FactsetDigest,
		EventID:         policy.EventID,
		Status:          PlacementUnknown,
		PolicyStake:     policy.Stake,
	}
	reason := func(r string) { out.Reasons = appendOnce(out.Reasons, r) }

	if !factual.derived() {
		reason(PlacementReasonFactualNotDerived)
		return out
	}
	// FOUR CLAUSES, AND ONLY THE FIRST THREE ARE DISCRIMINABLE FROM OUTSIDE
	// THIS PACKAGE. An independent lane deleted the CutoffPosition clause and
	// the whole suite stayed green; reproduced here. That is not a missing
	// test, and a test is not the right answer to it: both values are derived
	// from the SAME factset, and a FactualPlacement carries a producer-only
	// witness, so factual.derived() above refuses a hand-built one before this
	// line is reached. There is no input outside this package that skews the
	// cutoff alone -- a skewed one is refused as FACTUAL_PLACEMENT_NOT_DERIVED
	// one barrier earlier. The clause is defence in depth against a future
	// caller INSIDE the package, and manufacturing a test for it would mean
	// expanding the production API to defeat the witness, which is worse than
	// the gap. Recorded as an argument rather than left as an unexplained
	// mutation survivor.
	if policy.FactsetDigest != factual.FactsetDigest || policy.Attempt != factual.Attempt ||
		policy.EventID != factual.EventID || policy.CutoffPosition != factual.CutoffPosition {
		reason(PlacementReasonCaseBindingMismatch)
		return out
	}
	switch policy.Action.Class {
	case ActionPolicySkip:
		out.Status = PlacementNotApplicable
		return out
	case ActionWouldAttempt:
	default:
		// COVERAGE_ONLY, LEGACY_FAILURE, UNKNOWN_INPUT, REFUSED,
		// UNSUPPORTED_SHAPE, PARTICIPATION_ADMITTED_STAKE_UNKNOWN and
		// NO_ATTEMPT_IN_SUPPLIED_PREFIX: nothing was placed and nothing is a
		// zero. The class is the reason.
		reason(string(policy.Action.Class))
		return out
	}

	if !policy.Choice.Present {
		reason(PlacementReasonChoiceMissing)
		return out
	}
	if !policy.Stake.Known() {
		reason(PlacementReasonPolicyStakeUnknown)
		return out
	}
	out.AttributedOutcomeID = policy.Choice.OutcomeID

	// Is the policy's decision THE factual decision?
	inheritable := true
	if factual.TerminalDecision != "PLACE" {
		reason(PlacementReasonFactualNotPlace)
		inheritable = false
	}
	if factual.RecordedChoiceIndex == nil || *factual.RecordedChoiceIndex != policy.Choice.Index {
		reason(PlacementReasonChoiceDiffers)
		inheritable = false
	}
	// The identity the store recorded for the factual choice, when it
	// carries one, must be the policy's: a recorded decision whose index
	// agrees with the policy's but whose outcome does not names another
	// outcome, and the policy's decision is not that decision.
	if factual.RecordedChoiceOutcomeID != "" && factual.RecordedChoiceOutcomeID != policy.Choice.OutcomeID {
		reason(PlacementReasonChoiceIdentityDiffers)
		inheritable = false
	}
	// The slot the terminal fact itself named is a second, independent
	// record of the same choice (the pinned producer writes it on every
	// placing terminal fact and on no other): absent, or naming another
	// slot, the terminal fact does not name the policy's slot. On a factual
	// SKIP the slot is rightly absent and this reason joins
	// FACTUAL_DECISION_NOT_PLACE; it says the fact names no such slot, not
	// that the producer recorded a wrong one.
	if factual.RecordedTerminalSlot == nil || *factual.RecordedTerminalSlot != policy.Choice.Index {
		reason(PlacementReasonTerminalSlotDiffers)
		inheritable = false
	}
	if factual.RecordedFinalAmount == nil || *factual.RecordedFinalAmount != policy.Stake.Value {
		reason(PlacementReasonStakeDiffers)
		inheritable = false
	}
	if !inheritable {
		out.Status = PlacementCounterfactualNotInheritable
		return out
	}

	// The factual call itself.
	switch {
	case factual.Coherence == predictioneval.PlacementShapeAbsent || !factual.CallPresent:
		out.Status = PlacementNotRecorded
		return out
	case factual.Coherence == predictioneval.PlacementShapeCoherent, factual.StartedOnly:
	default:
		out.Status = PlacementIncoherent
		return out
	}
	if factual.CallStartedObservationID == "" {
		out.Status = PlacementNotRecorded
		return out
	}
	// A derived factual placement's call always follows its own terminal
	// envelope (the P2 materialization orders it so); this guard is defence
	// in depth behind the witness, not a reachable verdict.
	if factual.CallStartedPosition <= policy.CutoffPosition {
		reason(PlacementReasonCallNotAfterCutoff)
		return out
	}
	if factual.Stake == nil || factual.Slot == nil || *factual.Stake != policy.Stake.Value || *factual.Slot != policy.Choice.Index {
		reason(PlacementReasonCallContradictsRecord)
		return out
	}
	out.AttributedCallObservationID = factual.CallStartedObservationID
	out.AttributedCallPosition = factual.CallStartedPosition
	switch {
	case factual.StartedOnly || !factual.Returned:
		// Unreachable through the D16 pipeline: an unspent automatic start is
		// refused in the evidence seam, so no episode carrying one is ever
		// selected and no FactualPlacement is minted from it. Kept as defence
		// in depth behind the producer-only witness. See PlacementNotReturned.
		out.Status = PlacementNotReturned
		return out
	case !factual.LocalReasonOK:
		out.Status = PlacementLocalErrorPlatformUnknown
		if factual.ErrorClass != "" {
			reason(PlacementReasonLocalErrorClassWithheldPrefix + suppliedTextExtent(factual.ErrorClass))
		}
		return out
	}

	// Local OK. Acceptance needs a proof bound to THIS call.
	out.Status = PlacementLocalOKPlatformUnproven
	if proof == nil {
		reason(PlacementReasonNoProofSupplied)
		return out
	}
	valid := true
	if proof.Basis != ProofBasisPlatformPredictionConfirmed {
		reason(PlacementReasonProofBasisNotAccepted)
		valid = false
	}
	if proof.CallObservationID != factual.CallStartedObservationID {
		reason(PlacementReasonProofNotBoundToCall)
		valid = false
	}
	if proof.Reference.EventID != factual.EventID {
		reason(PlacementReasonProofRoundMismatch)
		valid = false
	}
	if proof.Reference.ObservationID == "" || proof.ProofRevision == "" {
		reason(PlacementReasonProofIncomplete)
		valid = false
	}
	if valid {
		out.Status = PlacementAcceptedPlatformProven
	}
	return out
}

func serializeOptionalInt(c *canonical, v *int) {
	c.boolean(v != nil)
	if v != nil {
		c.i64(int64(*v))
	}
}
