package p4offline

import (
	"errors"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// SEAM 10: evidence-only payout, p4-payout-evidence-only/v1.
//
// The payout seam answers two separate questions and keeps them separate:
//
//   - was the policy's CHOICE correct? That needs only the resolution
//     artifact and the choice. It is the primary metric's input, and it is
//     scorable even when no bet was ever placed.
//   - what did the policy's BET settle to? That needs an ATTRIBUTED,
//     PLATFORM-PROVEN placement and, for a win or a refund, the recorded
//     payout or returned stake linked to that exact call. Nothing is scaled,
//     floored, fee-adjusted, retried or repaired: a WIN whose payout is not
//     recorded has an UNKNOWN payout and an UNKNOWN net.
//
// The three handwritten rules:
//
//	WIN    (attributed, proven, choice correct):   payout = recorded; net = payout - stake
//	LOSE   (attributed, proven, choice incorrect): payout = 0;        net = -stake
//	REFUND (attributed, proven, round canceled):   payout = 0;        net = 0 iff the
//	                                               returned stake is recorded and equals the stake
//
// A POLICY_SKIP carries its own exact zero — the decision's stake, which
// must be KNOWN 0 for the decision to be usable at all — and contributes to
// no bet-only denominator. NO_ATTEMPT_IN_SUPPLIED_PREFIX is not a skip: its
// stake and net are UNKNOWN.
//
// Every artifact handed in is checked for its binding to the decision before
// it is read: the placement must be this policy's on this case and must carry
// the witness only [DerivePlacement] sets, and the resolution must be this
// round's over the outcome set the choice was made from. A payout record and
// its linkage are SUPPLIED evidence in this package's PROVISIONAL vocabulary
// (see the package documentation's stated assumptions): the package checks
// the linkage it is handed and cannot authenticate that the record exists.

// PayoutOutcome is the closed settlement vocabulary.
type PayoutOutcome string

// Payout outcomes.
const (
	PayoutWin           PayoutOutcome = "WIN"
	PayoutLose          PayoutOutcome = "LOSE"
	PayoutRefund        PayoutOutcome = "REFUND"
	PayoutUnknown       PayoutOutcome = "UNKNOWN"
	PayoutNotApplicable PayoutOutcome = "NOT_APPLICABLE"
)

// ChoiceVerdict is the closed choice-accuracy vocabulary: the primary
// metric's per-case input.
type ChoiceVerdict string

// Choice verdicts.
const (
	ChoiceCorrect       ChoiceVerdict = "CORRECT"
	ChoiceIncorrect     ChoiceVerdict = "INCORRECT"
	ChoiceUnknown       ChoiceVerdict = "UNKNOWN"
	ChoiceNotApplicable ChoiceVerdict = "NOT_APPLICABLE"
)

// LinkageBasisAttemptLinkedUserTerminal is the only accepted linkage between
// a payout record and a call: the settlement fact was linked to the attempt
// by a proof outside this package (the pinned producer writes no such link).
const LinkageBasisAttemptLinkedUserTerminal = "ATTEMPT_LINKED_USER_TERMINAL"

// Payout reasons. Closed vocabulary.
const (
	PayoutReasonPolicyBindingMismatch        = DecisionReasonPolicyBindingMismatch
	PayoutReasonPlacementNotDerived          = "PLACEMENT_NOT_DERIVED"
	PayoutReasonPlacementBindingMismatch     = "PLACEMENT_BINDING_MISMATCH"
	PayoutReasonResolutionDigestMismatch     = "RESOLUTION_DIGEST_MISMATCH"
	PayoutReasonResolutionNotDerivable       = "RESOLUTION_NOT_DERIVABLE"
	PayoutReasonResolutionRoundMismatch      = "RESOLUTION_ROUND_MISMATCH"
	PayoutReasonResolutionOutcomeSetMismatch = "RESOLUTION_OUTCOME_SET_MISMATCH"
	PayoutReasonResolutionUnknown            = "RESOLUTION_UNKNOWN"
	PayoutReasonChoiceMissing                = "CHOICE_MISSING"
	PayoutReasonChoiceNotInOutcomeSet        = "CHOICE_NOT_IN_RESOLUTION_OUTCOME_SET"
	PayoutReasonStakeUnknown                 = "POLICY_STAKE_UNKNOWN"
	PayoutReasonStakeNegative                = DecisionReasonStakeNegative
	PayoutReasonPlacementNotProven           = "PLACEMENT_NOT_PROVEN"
	PayoutReasonPlacementContradicts         = "PLACEMENT_CONTRADICTS_DECISION"
	PayoutReasonPayoutNotRecorded            = "PAYOUT_NOT_RECORDED"
	PayoutReasonPayoutNegative               = "PAYOUT_NEGATIVE"
	PayoutReasonRefundReturnNotRecorded      = "REFUND_RETURN_NOT_RECORDED"
	PayoutReasonReturnedStakeMismatch        = "RETURNED_STAKE_MISMATCH"
	PayoutReasonRecordLinkageNotAccepted     = "RECORD_LINKAGE_NOT_ACCEPTED"
	PayoutReasonRecordNotLinked              = "RECORD_NOT_LINKED"
	PayoutReasonRecordRoundMismatch          = "RECORD_ROUND_MISMATCH"
	PayoutReasonRecordIncomplete             = "RECORD_INCOMPLETE"
	PayoutReasonLoseByResolution             = "LOSE_BY_RESOLUTION"
	PayoutReasonRefundByResolution           = "REFUND_BY_RESOLUTION"
	PayoutReasonNotEvaluated                 = "NOT_EVALUATED"
)

// PayoutRecord is supplied evidence of what the platform paid on a specific
// call.
type PayoutRecord struct {
	LinkageBasis            string            `json:"linkageBasis"`
	LinkedCallObservationID string            `json:"linkedCallObservationId"`
	Reference               EvidenceReference `json:"reference"`
	Payout                  Int64Fact         `json:"payout"`
	ReturnedStake           Int64Fact         `json:"returnedStake"`
	ProofRevision           string            `json:"proofRevision"`
}

// PayoutEvidence is the evidence-only settlement of one policy on one case,
// bound to its case.
type PayoutEvidence struct {
	ContractVersion string                    `json:"contractVersion"`
	Policy          string                    `json:"policy"`
	Attempt         predictioneval.AttemptKey `json:"attempt"`
	FactsetDigest   string                    `json:"factsetDigest"`
	EventID         string                    `json:"eventId"`
	// Outcome is the settlement of the ATTRIBUTED bet, or UNKNOWN /
	// NOT_APPLICABLE.
	Outcome PayoutOutcome `json:"outcome"`
	// ChoiceCorrect is the primary metric's input for this case and policy.
	ChoiceCorrect         ChoiceVerdict `json:"choiceCorrect"`
	Stake                 Int64Fact     `json:"stake"`
	Payout                Int64Fact     `json:"payout"`
	Net                   Int64Fact     `json:"net"`
	Reasons               []string      `json:"reasons,omitempty"`
	ResolutionFactsDigest string        `json:"resolutionFactsDigest,omitempty"`
}

// DerivePayout is seam 10.
func DerivePayout(policy PolicyDecision, placement PlacementEvidence, res ResolutionArtifact, record *PayoutRecord) PayoutEvidence {
	out := PayoutEvidence{
		ContractVersion: PayoutEvidenceVersion,
		Policy:          policy.Policy,
		Attempt:         policy.Attempt,
		FactsetDigest:   policy.FactsetDigest,
		EventID:         policy.EventID,
		Outcome:         PayoutUnknown,
		ChoiceCorrect:   ChoiceUnknown,
		Stake:           UnknownInt64(PayoutReasonNotEvaluated),
		Payout:          UnknownInt64(PayoutReasonNotEvaluated),
		Net:             UnknownInt64(PayoutReasonNotEvaluated),
	}
	reason := func(r string) { out.Reasons = appendOnce(out.Reasons, r) }

	if why := decisionRefusal(policy); why != "" {
		reason(why)
		return out
	}
	if !placement.derived() {
		reason(PayoutReasonPlacementNotDerived)
		return out
	}
	if placement.ContractVersion != PlacementEvidenceVersion || placement.Policy != policy.Policy {
		reason(PayoutReasonPolicyBindingMismatch)
		return out
	}
	if placement.Attempt != policy.Attempt || placement.FactsetDigest != policy.FactsetDigest ||
		placement.EventID != policy.EventID || placement.PolicyStake != policy.Stake {
		reason(PayoutReasonPlacementBindingMismatch)
		return out
	}
	if err := VerifyResolutionArtifact(res); err != nil {
		if errors.Is(err, ErrResolutionNotDerivable) {
			reason(PayoutReasonResolutionNotDerivable)
		} else {
			reason(PayoutReasonResolutionDigestMismatch)
		}
		return out
	}
	out.ResolutionFactsDigest = res.ResolutionFactsDigest
	if res.Round.EventID != policy.EventID {
		reason(PayoutReasonResolutionRoundMismatch)
		return out
	}
	if !sameIDs(policy.OutcomeIDs, res.OrderedOutcomeIDs) {
		reason(PayoutReasonResolutionOutcomeSetMismatch)
		return out
	}

	switch policy.Action.Class {
	case ActionPolicySkip:
		// The decision's own exact zero (decisionRefusal proved it KNOWN 0).
		out.Outcome = PayoutNotApplicable
		out.ChoiceCorrect = ChoiceNotApplicable
		out.Stake = policy.Stake
		out.Payout = NotApplicableInt64(string(ActionPolicySkip))
		out.Net = Int64Fact{Presence: PresenceKnown, Value: 0, Reason: StakeReasonPolicySkipExactZero}
		reason(string(ActionPolicySkip))
		return out
	case ActionNoAttemptInSuppliedPrefix:
		out.ChoiceCorrect = ChoiceNotApplicable
		out.Stake = UnknownInt64(string(ActionNoAttemptInSuppliedPrefix))
		out.Payout = UnknownInt64(string(ActionNoAttemptInSuppliedPrefix))
		out.Net = UnknownInt64(string(ActionNoAttemptInSuppliedPrefix))
		reason(string(ActionNoAttemptInSuppliedPrefix))
		return out
	case ActionWouldAttempt, ActionParticipationAdmittedStakeUnknown:
	default:
		out.Stake = policy.Stake
		reason(string(policy.Action.Class))
		return out
	}

	// ---- The choice, against the resolution alone. --------------------
	if !policy.Choice.Present {
		reason(PayoutReasonChoiceMissing)
		return out
	}
	switch res.Outcome {
	case ResolutionWinnerKnown:
		if !containsID(res.OrderedOutcomeIDs, policy.Choice.OutcomeID) {
			reason(PayoutReasonChoiceNotInOutcomeSet)
		} else if policy.Choice.OutcomeID == res.WinnerOutcomeID {
			out.ChoiceCorrect = ChoiceCorrect
		} else {
			out.ChoiceCorrect = ChoiceIncorrect
		}
	case ResolutionRefund:
		out.ChoiceCorrect = ChoiceNotApplicable
		reason(PayoutReasonRefundByResolution)
	default:
		reason(PayoutReasonResolutionUnknown)
	}

	// ---- The stake (its sign was checked with the decision). --------------
	out.Stake = policy.Stake
	if !policy.Stake.Known() {
		reason(PayoutReasonStakeUnknown)
		return out
	}

	// ---- The bet: only an attributed, platform-proven placement settles. --
	if placement.Status != PlacementAcceptedPlatformProven {
		reason(PayoutReasonPlacementNotProven)
		reason(string(placement.Status))
		return out
	}
	if placement.AttributedOutcomeID != policy.Choice.OutcomeID || placement.AttributedCallObservationID == "" {
		reason(PayoutReasonPlacementContradicts)
		return out
	}

	switch res.Outcome {
	case ResolutionWinnerKnown:
		switch out.ChoiceCorrect {
		case ChoiceCorrect:
			out.Outcome = PayoutWin
			rec, why := linkedRecord(record, placement, res)
			if why != "" {
				reason(why)
				return out
			}
			if !rec.Payout.Known() {
				reason(PayoutReasonPayoutNotRecorded)
				return out
			}
			if rec.Payout.Value < 0 {
				reason(PayoutReasonPayoutNegative)
				return out
			}
			out.Payout = KnownInt64(rec.Payout.Value)
			// Both operands are non-negative int64, so the difference cannot
			// leave the int64 range.
			out.Net = KnownInt64(rec.Payout.Value - policy.Stake.Value)
		case ChoiceIncorrect:
			out.Outcome = PayoutLose
			out.Payout = Int64Fact{Presence: PresenceKnown, Value: 0, Reason: PayoutReasonLoseByResolution}
			out.Net = Int64Fact{Presence: PresenceKnown, Value: -policy.Stake.Value, Reason: PayoutReasonLoseByResolution}
		default:
			return out
		}
	case ResolutionRefund:
		out.Outcome = PayoutRefund
		out.Payout = Int64Fact{Presence: PresenceKnown, Value: 0, Reason: PayoutReasonRefundByResolution}
		rec, why := linkedRecord(record, placement, res)
		if why != "" {
			if why == PayoutReasonPayoutNotRecorded {
				why = PayoutReasonRefundReturnNotRecorded
			}
			reason(why)
			return out
		}
		if !rec.ReturnedStake.Known() {
			reason(PayoutReasonRefundReturnNotRecorded)
			return out
		}
		if rec.ReturnedStake.Value != policy.Stake.Value {
			reason(PayoutReasonReturnedStakeMismatch)
			return out
		}
		out.Net = Int64Fact{Presence: PresenceKnown, Value: 0, Reason: PayoutReasonRefundByResolution}
	default:
		return out
	}
	return out
}

// linkedRecord verifies a payout record's linkage to the attributed call and
// the round. It returns the record, or the reason it cannot be read.
func linkedRecord(record *PayoutRecord, placement PlacementEvidence, res ResolutionArtifact) (*PayoutRecord, string) {
	switch {
	case record == nil:
		return nil, PayoutReasonPayoutNotRecorded
	case record.LinkageBasis != LinkageBasisAttemptLinkedUserTerminal:
		return nil, PayoutReasonRecordLinkageNotAccepted
	case record.LinkedCallObservationID != placement.AttributedCallObservationID:
		return nil, PayoutReasonRecordNotLinked
	case record.Reference.EventID != res.Round.EventID:
		return nil, PayoutReasonRecordRoundMismatch
	case record.Reference.ObservationID == "" || record.ProofRevision == "":
		return nil, PayoutReasonRecordIncomplete
	}
	return record, ""
}
