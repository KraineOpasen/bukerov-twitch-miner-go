package p4offline

import (
	"errors"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// P2 PLUMBING: one factset, one native P2 evaluation, one mapped action.
//
// This is not a runner. It evaluates ONE case under the P2 baseline exactly as
// the native evaluator defines it, bound to the case's own configuration and
// to the outcome-free factset digest. It accepts no entropy, no resolution, no
// placement and no recorded result: none of those has a parameter here, which
// is how NO_STEALTH_SIGNAL and "the winner is unavailable to the evaluators"
// are enforced rather than promised.

// PolicyChoice is what a policy would have bet on: the outcome's index in the
// factset's ordered vector AND its identity. The two are checked against
// each other wherever a choice is consumed.
type PolicyChoice struct {
	Present   bool   `json:"present"`
	Index     int    `json:"index"`
	OutcomeID string `json:"outcomeId,omitempty"`
}

// P2CaseResult is the P2 side of one case.
type P2CaseResult struct {
	Policy        string                    `json:"policy"`
	FactsetDigest string                    `json:"factsetDigest"`
	Binding       P2ConfigBinding           `json:"binding"`
	Evaluation    predictioneval.Evaluation `json:"evaluation"`
	Action        ActionMapping             `json:"action"`
	Choice        PolicyChoice              `json:"choice"`
	// Stake is the policy's stake: exact zero for a POLICY_SKIP, the
	// post-clamp final for an attempt, otherwise not a number.
	Stake   Int64Fact `json:"stake"`
	witness string
	// framedLen is the width witness was computed over. Producer-only, like
	// the witness itself.
	framedLen int
}

// derived reports whether the value is what EvaluateP2Case produced OVER THE
// FIELDS p2ResultWitness FRAMES. That is not the whole value, and the
// difference is load-bearing: the witness leaves Binding.Model, Binding.Settings,
// the Risk fields, MinimumStake and all of Evaluation but CommonInputDigest,
// Action and ActionReason unframed, so an edit to any of those is ACCEPTED
// here. No such edit can move a verdict, because Decision reads framed fields
// only -- that is why the gap is tolerable, not why it is absent.
// p2ResultWitness enumerates what is not framed and why; read it before
// trusting this answer about a field added later.
func (r P2CaseResult) derived() bool {
	return r.witness != "" && r.framedLen > 0 &&
		r.framedLen == p2ResultFramedLen(r) && r.witness == p2ResultWitness(r)
}

// p2ResultWitness frames every field a decision is minted from — the action,
// the choice, the stake — and the policy, the factset digest, the binding's
// contract version and digest, and the native evaluation's common-input
// digest, action and action reason.
//
// NOT framed, exactly: the binding's Model, Settings, RiskPresent,
// RiskMaxStakePercent, RiskReservePoints and MinimumStake (Binding.Digest,
// which BindP2Config computes over all of them, is framed; it is not
// recomputed here) and, of the native evaluation, Model, Choice, BaseStake,
// Stealth, Filter, Health, StakeGate, Clamp, Minimum, PolicyAmount,
// PolicyAmountKnown, LegacyFailure and Limitations. The common-input digest
// binds the run's INPUTS, not its stages; the stages are bound only through
// the framed action, choice and stake, so an edit confined to the unframed
// fields is not detected here. A stored result is re-evaluated, never
// trusted.
func p2ResultWitness(r P2CaseResult) string {
	var c canonical
	frameP2Result(&c, r)
	return c.digest()
}

// p2ResultFramedLen is the width p2ResultWitness frames, computed by the SAME
// pass with bytes switched off. See P3bCaseResult.derived for why the width is
// checked first.
func p2ResultFramedLen(r P2CaseResult) int {
	c := canonical{lenOnly: true}
	frameP2Result(&c, r)
	return c.framedLen()
}

func frameP2Result(c *canonical, r P2CaseResult) {
	c.str("p4offline-p2-result-witness")
	c.str(r.Policy)
	c.str(r.FactsetDigest)
	c.str(r.Binding.ContractVersion)
	c.str(r.Binding.Digest)
	c.str(r.Evaluation.CommonInputDigest)
	c.str(r.Evaluation.Action)
	c.str(r.Evaluation.ActionReason)
	frameAction(c, r.Action)
	frameChoice(c, r.Choice)
	frameFact(c, r.Stake)
}

// p2Derivation is the P2 decision's derivation: the per-case config binding.
func p2Derivation(bindingDigest string) string {
	return PolicyP2 + ":binding=" + bindingDigest
}

// Stake reasons.
const (
	StakeReasonPolicySkipExactZero = "POLICY_SKIP_EXACT_ZERO"
	StakeReasonNoFinalStake        = "NO_FINAL_STAKE"
	StakeReasonCoverageOnly        = "COVERAGE_ONLY"
	StakeReasonUnsupportedShape    = "UNSUPPORTED_SHAPE"
)

// EvaluateP2Case evaluates one COMPLETE factset under the pinned P2 policy.
func EvaluateP2Case(fs CommonFactset) (P2CaseResult, error) {
	if err := VerifyCommonFactset(fs); err != nil {
		return P2CaseResult{}, err
	}
	if fs.Completeness != FactsetComplete || !fs.ReachedDecision {
		// The reasons are NAMED, not rendered. IncompleteReasons is a plain
		// exported slice that checkFactsetConsistency bounds neither in count
		// nor per string -- containsID asks only that the derived reasons are a
		// SUBSET of the supplied ones, so arbitrary padding is legal by this
		// package's own rules -- and this is the exported entry point a caller
		// reaches it through.
		// THE FAULT AND THE EXTENT, which is the whole rule and not half of it.
		// A first version reported only the extent, so an ordinary INCOMPLETE
		// factset refused with "1 reasons, 15 bytes" and the caller lost the
		// diagnosis entirely. valueDerivedReasons is the bounded answer: it is
		// re-derived from the factset's own VALUES, returns at most six members
		// drawn from closed vocabularies, and never touches the caller's
		// IncompleteReasons -- which is the unbounded list, named by extent.
		return P2CaseResult{}, errors.Join(ErrFactsetNotEvaluable,
			errors.New("p4offline: factset is "+string(fs.Completeness)+": "+
				joinReasons(valueDerivedReasons(fs))+" ("+reasonListExtent(fs.IncompleteReasons)+" supplied)"))
	}
	binding, err := BindP2Config(fs)
	if err != nil {
		return P2CaseResult{}, err
	}
	in := predictioneval.DecisionInputs{
		// The P2 evaluation is bound to the P4 factset, not to pe-cid.
		CommonInputDigest: fs.Digest,
		// The factset's OWN answer, never assumed: a factset that verifies
		// as COMPLETE has already proven it reached the decision.
		ReachedDecision:     fs.ReachedDecision,
		PreDecisionExit:     fs.PreDecisionExit,
		Settings:            copySettings(fs.Settings),
		Balance:             int(fs.Balance),
		BalancePresent:      fs.BalancePresent,
		Outcomes:            append([]predictioneval.OutcomeInput(nil), fs.Outcomes...),
		OutcomesPresent:     fs.OutcomesPresent,
		BetTotalUsers:       copyInt64(fs.BetTotalUsers),
		BetTotalPoints:      copyInt64(fs.BetTotalPoints),
		RiskPresent:         fs.RiskPresent,
		RiskMaxStakePercent: fs.RiskMaxStakePercent,
		RiskReservePoints:   fs.RiskReservePoints,
		HealthState:         fs.HealthState,
		HealthReason:        fs.HealthReason,
		MinimumStake:        fs.MinimumStake,
	}
	if int64(in.Balance) != fs.Balance {
		return P2CaseResult{}, errors.Join(ErrFactsetNotEvaluable,
			errors.New("p4offline: balance is not representable at this int width"))
	}
	// No observed realization: stealth is proven off, and the stage that
	// would read one reports NOT_APPLICABLE without consulting it.
	ev := predictioneval.Evaluate(in, predictioneval.ObservedRealization{})
	action := MapP2Action(ev)
	res := P2CaseResult{
		Policy:        PolicyP2,
		FactsetDigest: fs.Digest,
		Binding:       binding,
		Evaluation:    ev,
		Action:        action,
	}
	if ev.Choice.State == predictioneval.StageStateExecuted && ev.Choice.Selected {
		res.Choice = PolicyChoice{Present: true, Index: ev.Choice.Index, OutcomeID: ev.Choice.OutcomeID}
	}
	switch action.Class {
	case ActionWouldAttempt:
		if ev.Clamp.HasFinal {
			res.Stake = KnownInt64(int64(ev.Clamp.FinalAmount))
		} else {
			res.Stake = UnknownInt64(StakeReasonNoFinalStake)
		}
	case ActionPolicySkip:
		res.Stake = Int64Fact{Presence: PresenceKnown, Value: 0, Reason: StakeReasonPolicySkipExactZero}
	case ActionLegacyFailure:
		res.Stake = Int64Fact{Presence: PresenceNotReached, Reason: string(ActionLegacyFailure)}
	case ActionCoverageOnly:
		res.Stake = NotApplicableInt64(StakeReasonCoverageOnly)
	case ActionUnknownInput:
		res.Stake = UnknownInt64(ev.Action)
	default:
		res.Stake = UnknownInt64(StakeReasonUnsupportedShape)
	}
	res.witness, res.framedLen = p2ResultWitness(res), p2ResultFramedLen(res)
	return res, nil
}

// Decision binds a P2 result to its case as a policy decision. The factset
// must be the one the result was evaluated over, and the result must be
// exactly what [EvaluateP2Case] produced: a binding contradiction is named
// first, an underived result second.
func (r P2CaseResult) Decision(fs CommonFactset) (PolicyDecision, error) {
	d, err := decisionOf(PolicyP2, fs, r.FactsetDigest, r.Action, r.Choice, r.Stake,
		func() string { return p2Derivation(r.Binding.Digest) }, r.derived)
	if err != nil {
		return PolicyDecision{}, err
	}
	return d, nil
}
