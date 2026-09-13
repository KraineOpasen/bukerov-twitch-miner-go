package p4offline

import (
	"errors"

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
	// PlacementNotReturned — the factual call started and never returned.
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
)

// Placement reasons. Closed vocabulary.
const (
	PlacementReasonCaseBindingMismatch   = DecisionReasonCaseBindingMismatch
	PlacementReasonFactualNotDerived     = "FACTUAL_PLACEMENT_NOT_DERIVED"
	PlacementReasonPolicyStakeUnknown    = "POLICY_STAKE_UNKNOWN"
	PlacementReasonChoiceMissing         = "CHOICE_MISSING"
	PlacementReasonChoiceDiffers         = "CHOICE_DIFFERS"
	PlacementReasonStakeDiffers          = "STAKE_DIFFERS"
	PlacementReasonFactualNotPlace       = "FACTUAL_DECISION_NOT_PLACE"
	PlacementReasonCallNotAfterCutoff    = "CALL_NOT_AFTER_CUTOFF"
	PlacementReasonCallContradictsRecord = "CALL_CONTRADICTS_RECORDED_DECISION"
	PlacementReasonProofBasisNotAccepted = "PROOF_BASIS_NOT_ACCEPTED"
	PlacementReasonProofNotBoundToCall   = "PROOF_NOT_BOUND_TO_CALL"
	PlacementReasonProofRoundMismatch    = "PROOF_ROUND_MISMATCH"
	PlacementReasonProofIncomplete       = "PROOF_INCOMPLETE"
	PlacementReasonNoProofSupplied       = "NO_PLATFORM_ACCEPTANCE_PROOF"
	PlacementReasonLocalErrorClassPrefix = "LOCAL_ERROR_CLASS:"
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
	// The factual DECISION, from the terminal envelope's recorded results.
	TerminalDecision    string `json:"terminalDecision"`
	RecordedChoiceIndex *int   `json:"recordedChoiceIndex,omitempty"`
	RecordedFinalAmount *int64 `json:"recordedFinalAmount,omitempty"`
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
}

// derived reports whether the value is exactly what ProjectFactualPlacement
// produced.
func (f FactualPlacement) derived() bool {
	return f.witness != "" && f.witness == factualPlacementWitness(f)
}

func factualPlacementWitness(f FactualPlacement) string {
	var c canonical
	c.str("p4offline-factual-placement-witness")
	c.i64(f.Attempt.CollectorEpoch)
	c.str(f.Attempt.CollectorSessionID)
	c.str(f.Attempt.PoolInstanceID)
	c.u64(f.Attempt.AttemptID)
	c.str(f.FactsetDigest)
	c.str(f.EventID)
	c.i64(f.CutoffPosition)
	c.str(f.TerminalDecision)
	serializeOptionalInt(&c, f.RecordedChoiceIndex)
	serializeOptionalInt64(&c, f.RecordedFinalAmount)
	c.str(f.Coherence)
	c.boolean(f.StartedOnly)
	c.boolean(f.CallPresent)
	c.str(f.CallStartedObservationID)
	c.i64(f.CallStartedPosition)
	serializeOptionalInt64(&c, f.Stake)
	serializeOptionalInt(&c, f.Slot)
	c.boolean(f.Returned)
	c.boolean(f.LocalReasonOK)
	c.str(f.ErrorClass)
	return c.digest()
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
}

// derived reports whether the value is exactly what a Decision method
// produced.
func (p PolicyDecision) derived() bool {
	return p.witness != "" && p.witness == policyDecisionWitness(p)
}

func policyDecisionWitness(p PolicyDecision) string {
	var c canonical
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
	frameAction(&c, p.Action)
	frameChoice(&c, p.Choice)
	frameFact(&c, p.Stake)
	return c.digest()
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
	PolicyStake                 Int64Fact `json:"policyStake"`
	AttributedOutcomeID         string    `json:"attributedOutcomeId,omitempty"`
	AttributedCallObservationID string    `json:"attributedCallObservationId,omitempty"`
	AttributedCallPosition      int64     `json:"attributedCallPosition"`
	witness                     string
}

// derived reports whether the value is exactly what DerivePlacement produced.
func (p PlacementEvidence) derived() bool {
	return p.witness != "" && p.witness == placementEvidenceWitness(p)
}

func placementEvidenceWitness(p PlacementEvidence) string {
	var c canonical
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
	frameFact(&c, p.PolicyStake)
	c.str(p.AttributedOutcomeID)
	c.str(p.AttributedCallObservationID)
	c.i64(p.AttributedCallPosition)
	return c.digest()
}

// decisionOf binds a policy result to the factset it was evaluated over.
func decisionOf(policy string, fs CommonFactset, resultDigest string, action ActionMapping, choice PolicyChoice,
	stake Int64Fact, derivation string) (PolicyDecision, error) {
	if err := VerifyCommonFactset(fs); err != nil {
		return PolicyDecision{}, err
	}
	if resultDigest == "" || resultDigest != fs.Digest {
		return PolicyDecision{}, errors.Join(ErrDecisionBinding,
			errors.New("p4offline: result was evaluated over factset "+resultDigest+", not "+fs.Digest))
	}
	if action.Policy != policy || action.MapVersion != NativeActionMapVersion {
		return PolicyDecision{}, errors.Join(ErrDecisionBinding, errors.New("p4offline: action mapping is not this policy's"))
	}
	d := PolicyDecision{
		Policy:         policy,
		Attempt:        fs.Attempt,
		FactsetDigest:  fs.Digest,
		EventID:        fs.Episode.EventID,
		CutoffPosition: fs.CutoffPosition,
		Derivation:     derivation,
		OutcomeIDs:     outcomeIDs(fs),
		Action:         action,
		Choice:         choice,
		Stake:          stake,
	}
	d.witness = policyDecisionWitness(d)
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
	case p.Action.Class == ActionPolicySkip && !(p.Stake.Known() && p.Stake.Value == 0):
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

// ProjectFactualPlacement reads the selected attempt's recorded decision and
// its post-decision placement facts through the P2 projections. The factset
// must be exactly what the dataset derives for its episode.
func ProjectFactualPlacement(ds predictioneval.SourceDataset, fs CommonFactset) (FactualPlacement, error) {
	ep, err := derivedOpportunity(ds, fs)
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
	out.witness = factualPlacementWitness(out)
	return out, nil
}

// DerivePlacement is seam 9.
func DerivePlacement(policy PolicyDecision, factual FactualPlacement, proof *PlatformAcceptanceProof) PlacementEvidence {
	out := derivePlacement(policy, factual, proof)
	out.witness = placementEvidenceWitness(out)
	return out
}

func derivePlacement(policy PolicyDecision, factual FactualPlacement, proof *PlatformAcceptanceProof) PlacementEvidence {
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

	if why := decisionRefusal(policy); why != "" {
		reason(why)
		return out
	}
	if !factual.derived() {
		reason(PlacementReasonFactualNotDerived)
		return out
	}
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
		out.Status = PlacementNotReturned
		return out
	case !factual.LocalReasonOK:
		out.Status = PlacementLocalErrorPlatformUnknown
		if factual.ErrorClass != "" {
			reason(PlacementReasonLocalErrorClassPrefix + factual.ErrorClass)
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
