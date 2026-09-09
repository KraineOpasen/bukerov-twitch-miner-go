package predictioneval

import "math"

// Seam 4 of 4: comparison, and the only stage that may look forward.
//
// Score is handed what [Evaluate] computed, what the producer recorded, and —
// for the first time in the pipeline — what happened AFTERWARDS: whether the
// placement call was made, whether Twitch accepted it, how the round resolved.
// It cannot influence the evaluation, because the evaluation already ran and
// is passed in by value.
//
// The load-bearing thing this stage does is REFUSE TO ADD UP comparisons that
// mean different things. A replay that conditioned its stake on the recorded
// stake and then "verified" that stake against the record has proven nothing,
// and counting that as a pass would make the whole scorecard dishonest. Every
// comparison therefore carries a basis, and the tallies are kept apart.

// Comparison bases.
const (
	// BasisIndependent — this model computed the value from inputs alone. An
	// agreement here is real evidence.
	BasisIndependent = "INDEPENDENT"
	// BasisConditioned — the value sits downstream of the conditioned stealth
	// stage, so it inherits an observed quantity. An agreement is consistency,
	// not proof.
	BasisConditioned = "CONDITIONED"
	// BasisCircular — the value IS the observed quantity the model was
	// conditioned on. Comparing it against the record compares it against
	// itself; it is reported so it is visible, and never counted.
	BasisCircular = "CIRCULAR"
	// BasisWitnessed — an external verdict echoed from the record. Agreement
	// is guaranteed by construction and proves nothing about the hidden state.
	BasisWitnessed = "WITNESSED"
	// BasisUnavailable — one of the two sides does not exist.
	BasisUnavailable = "UNAVAILABLE"
)

// Comparison verdicts.
const (
	VerdictAgree       = "AGREE"
	VerdictDisagree    = "DISAGREE"
	VerdictUnavailable = "UNAVAILABLE"
)

// Settlement assessments.
const (
	// SettlementUnknown — nothing about the settlement can be attributed to
	// the replayed decision.
	SettlementUnknown = "UNKNOWN"
	// SettlementAppliesToReplay — the replay independently reproduced the
	// recorded decision AND the placement it produced was accepted, so the
	// recorded PLACEMENT describes the replayed decision too.
	//
	// It says nothing about how the round resolved: that verdict is not
	// attributable to an attempt at this producer revision (see
	// SettlementFacts.ResolutionLinkage), so payout and profitability stay
	// UNKNOWN even here.
	SettlementAppliesToReplay = "APPLIES_TO_REPLAY"
	// SettlementConditional — as above, but the stake depended on the
	// conditioned stealth realization, so the correspondence is conditional on
	// that reconstruction.
	SettlementConditional = "CONDITIONAL_ON_STEALTH_REALIZATION"
	// SettlementNotApplicable — the replay says no placement would have been
	// attempted, so a recorded settlement is not the replayed decision's.
	SettlementNotApplicable = "NOT_APPLICABLE"
)

// Scorecard limitations.
const (
	LimitationNoROIComputed = "ROI_BANKROLL_AND_PROFITABILITY_NOT_COMPUTED"
	LimitationCaseExcluded  = "CASE_NOT_EVALUABLE"
	LimitationCaptureGap    = "ROUND_ADMITTED_WITH_INCOMPLETE_CAPTURE"
	LimitationNoDueFact     = "ATTEMPT_OPENING_FACT_ABSENT_FROM_SLICE"
	LimitationPolicyNotRun  = "ATTEMPT_EXITED_BEFORE_THE_POLICY_RAN"
	// LimitationEvaluationCaseMismatch marks a scorecard whose evaluation was
	// produced from DIFFERENT evidence than the case it is scored against.
	LimitationEvaluationCaseMismatch = "EVALUATION_DOES_NOT_BELONG_TO_THIS_CASE"
)

// Comparison is one recorded value set against one computed value.
type Comparison struct {
	Field    string `json:"field"`
	Verdict  string `json:"verdict"`
	Basis    string `json:"basis"`
	Recorded string `json:"recorded,omitempty"`
	Computed string `json:"computed,omitempty"`
	// Note explains an UNAVAILABLE or a non-obvious basis.
	Note string `json:"note,omitempty"`
}

// SettlementFacts are the post-decision facts of an attempt. They exist only
// to reach this stage; nothing upstream is given them.
type SettlementFacts struct {
	PlacementCallStarted  bool   `json:"placementCallStarted"`
	PlacementCallReturned bool   `json:"placementCallReturned"`
	PlacementAccepted     bool   `json:"placementAccepted"`
	PlacementErrorClass   string `json:"placementErrorClass,omitempty"`
	// PlacementStake is the stake the placement call actually carried.
	PlacementStake *int64 `json:"placementStake,omitempty"`
	PlacementSlot  *int   `json:"placementSlot,omitempty"`

	// ResolutionLinkage says whether how the round SETTLED can be attributed
	// to this attempt at all.
	//
	// Under the pinned producer it cannot, and saying so is the point of the
	// field. An attempt is reassembled by the minted autoAttemptId, and the
	// producer puts that counter on the due fact, the terminal decision and
	// both placement calls — but NOT on the user_terminal fact that carries the
	// win/loss verdict and the payout. Those facts name only the round, and a
	// round can carry more than one attempt, so joining on the round would
	// attribute one attempt's payout to another. That is exactly the kind of
	// quietly wrong number this package exists not to produce.
	//
	// Resolution, Payout and ReturnedStake therefore stay unset and the
	// settlement stays UNKNOWN, rather than being guessed from a round-level
	// fact. Making them attributable is a PRODUCER change — carry the
	// discriminator on the terminal fact — not a reader change, and it is
	// outside this module's scope.
	ResolutionLinkage string `json:"resolutionLinkage"`
	Resolution        string `json:"resolution,omitempty"`
	Payout            *int64 `json:"payout,omitempty"`
	ReturnedStake     *int64 `json:"returnedStake,omitempty"`

	PostDecisionFacts int `json:"postDecisionFacts"`
}

// ResolutionNotLinkableToAttempt is the only value ResolutionLinkage takes
// under the pinned producer revision.
const ResolutionNotLinkableToAttempt = "NOT_LINKABLE_TO_ATTEMPT_AT_PINNED_PRODUCER"

// SettlementAssessment is what the settlement says about the REPLAYED
// decision, as opposed to the original one.
type SettlementAssessment struct {
	Assessment string `json:"assessment"`
	// Facts are the settlement facts, carried verbatim.
	Facts SettlementFacts `json:"facts"`
	// ROI is deliberately always UNKNOWN. Bankroll and profitability effects
	// need a portfolio model, a bankroll trajectory and counterfactual
	// settlement, none of which this package has or invents.
	ROI string `json:"roi"`
}

// Scorecard is the deterministic, versioned result for one decision case.
type Scorecard struct {
	Model  ModelProvenance  `json:"model"`
	Source SourceProvenance `json:"source"`
	// Anomalies are the qualifications on the SESSION this case was read from
	// — truncated, unfinalized, unwitnessed, lossy, foreign revision. They ride
	// on the scorecard because the scorecard is what gets stored: without them
	// a result from a seven-facts-dropped, zero-witness session is byte-identical
	// to one from a fully verified AS_FINALIZED session.
	Anomalies         []string   `json:"anomalies,omitempty"`
	Key               AttemptKey `json:"key"`
	CommonInputDigest string     `json:"commonInputDigest"`

	RoundIncarnationID string `json:"roundIncarnationId"`
	EventID            string `json:"eventId"`

	Eligibility CaseEligibility `json:"eligibility"`

	// ReplayedAction is what the replay says the miner would have done;
	// RecordedTerminal is what it recorded doing.
	ReplayedAction   string `json:"replayedAction"`
	RecordedTerminal string `json:"recordedTerminal"`

	Comparisons []Comparison `json:"comparisons"`

	// The tallies are kept apart on purpose: an independent agreement and a
	// conditioned one are not the same evidence and must never be summed.
	IndependentAgree    int `json:"independentAgree"`
	IndependentDisagree int `json:"independentDisagree"`
	ConditionedAgree    int `json:"conditionedAgree"`
	ConditionedDisagree int `json:"conditionedDisagree"`
	WitnessedEchoes     int `json:"witnessedEchoes"`
	CircularComparisons int `json:"circularComparisons"`
	UnavailablePairs    int `json:"unavailablePairs"`

	Settlement SettlementAssessment `json:"settlement"`

	Evaluation Evaluation `json:"evaluation"`

	Limitations []string `json:"limitations,omitempty"`
}

// Score is seam 4: it compares one replayed decision against its record.
//
// It is pure and total: an ineligible case produces a scorecard saying so,
// never an error and never a silent pass.
func Score(c DecisionCase, ev Evaluation, s SettlementFacts) Scorecard {
	sc := Scorecard{
		Model:              CurrentModelProvenance(),
		Source:             c.Source,
		Anomalies:          append([]string(nil), c.Anomalies...),
		Key:                c.Key,
		CommonInputDigest:  c.CommonInputDigest,
		RoundIncarnationID: c.RoundIncarnationID,
		EventID:            c.EventID,
		Eligibility:        c.Eligibility,
		ReplayedAction:     ev.Action,
		RecordedTerminal:   c.Recorded.TerminalReason,
		Evaluation:         ev,
		Limitations:        append([]string(nil), ev.Limitations...),
	}
	sc.Limitations = appendOnce(sc.Limitations, LimitationNoROIComputed)

	// A batch caller can pair the wrong evaluation with a case. Both carry the
	// common-input digest of the slice they came from, so the mismatch is
	// detectable — and worth detecting, because if the two decisions happen to
	// produce the same values every comparison would be counted as an
	// agreement and a settlement declared applicable, on evidence that belongs
	// to something else. The scorecard is emitted with no comparisons rather
	// than with confident ones.
	if ev.CommonInputDigest != c.CommonInputDigest {
		sc.Limitations = appendOnce(sc.Limitations, LimitationEvaluationCaseMismatch)
		sc.Settlement = SettlementAssessment{
			Facts: s, ROI: SettlementUnknown, Assessment: SettlementUnknown,
		}
		return sc
	}
	if !c.Eligibility.Eligible {
		sc.Limitations = appendOnce(sc.Limitations, LimitationCaseExcluded)
	}
	if !c.Eligibility.ExercisesPolicy {
		sc.Limitations = appendOnce(sc.Limitations, LimitationPolicyNotRun)
	}
	if c.RoundCaptureGapCause != "" {
		sc.Limitations = appendOnce(sc.Limitations, LimitationCaptureGap)
	}
	if !c.SawDueFact {
		sc.Limitations = appendOnce(sc.Limitations, LimitationNoDueFact)
	}

	// Downstream of a conditioned stealth stage every derived quantity
	// inherits the observed realization.
	conditioned := ev.Stealth.Outcome == StealthConditionedOnObservedRealization
	derived := BasisIndependent
	if conditioned {
		derived = BasisConditioned
	}

	rec := c.Recorded
	add := func(cmp Comparison) { sc.Comparisons = append(sc.Comparisons, cmp) }

	// ---- The choice. The index and the id are functions of the outcome
	// vector and the strategy alone, so they stay INDEPENDENT even under
	// stealth.
	if ev.Choice.State == StageStateExecuted {
		recIdx, recHas := recordedChoiceIndex(rec)
		add(compareInt("choiceIndex", recIdx, ev.Choice.Index, recHas, BasisIndependent,
			"a negative recorded index is stored as an absence and reconstructed here"))
		add(compareString("choiceOutcomeId", rec.ChoiceOutcomeID, ev.Choice.OutcomeID, true, BasisIndependent, ""))
	}

	// ---- The strategy's proposed stake.
	if ev.PolicyAmountKnown && ev.Choice.State == StageStateExecuted {
		basis := derived
		note := ""
		if conditioned {
			// This is the very value the stealth stage was conditioned on.
			basis, note = BasisCircular, "compared against the observed realization it was conditioned on"
		}
		add(compareInt64("choiceAmount", rec.ChoiceAmount, int64(ev.PolicyAmount),
			rec.ChoiceAmountRecorded, basis, note))
	}

	// ---- The filter. Independent of the stake in every case: Skip reads the
	// outcome vector and the chosen index, never an amount.
	if ev.Filter.State == StageStateExecuted {
		add(compareBool("skipResult", rec.SkipResult, ev.Filter.Skip, rec.SkipResultRecorded,
			BasisIndependent, ""))
		if rec.SkipCompared != nil {
			add(compareFloat("skipCompared", *rec.SkipCompared, ev.Filter.Compared, true,
				BasisIndependent, ""))
		} else {
			add(Comparison{Field: "skipCompared", Verdict: VerdictUnavailable, Basis: BasisUnavailable})
		}
	}

	// ---- The health gate is echoed, so its agreement is guaranteed and
	// counted apart from anything this model derived.
	if ev.Health.State == StageStateWitnessed {
		add(Comparison{
			Field: "healthStage", Verdict: VerdictAgree, Basis: BasisWitnessed,
			Recorded: rec.HealthStage, Computed: ev.Health.Verdict,
			Note: "external verdict echoed from the record, not reconstructed",
		})
	}

	// ---- The stake gate and the clamp.
	if ev.StakeGate.State == StageStateExecuted {
		add(compareInt64("stakeAllowed", rec.StakeAllowed, int64(ev.StakeGate.Allowed),
			rec.StakeAllowedRecorded, derived, ""))
		add(compareString("stakeReason", rec.StakeReason, ev.StakeGate.Reason, true, derived, ""))
		add(compareInt64("stakeLimit", rec.StakeLimit, int64(ev.StakeGate.Limit),
			rec.StakeLimitRecorded, derived, ""))
	}
	if ev.Clamp.State == StageStateExecuted {
		add(compareBool("clampApplied", rec.ClampApplied, ev.Clamp.Applied,
			rec.ClampAppliedRecorded, derived, ""))
		add(compareInt64("finalAmount", rec.FinalAmount, int64(ev.Clamp.FinalAmount),
			rec.FinalAmountRecorded, derived, ""))
	} else if ev.Action == ActionReserveViolation {
		// The pinned caller produces no post-gate stake on this exit, so the
		// record must not carry one either. Agreement here is a real check.
		add(Comparison{
			Field:    "finalAmount",
			Verdict:  verdictOf(!rec.FinalAmountRecorded),
			Basis:    derived,
			Recorded: boolText(rec.FinalAmountRecorded, "present", "absent"),
			Computed: "absent",
			Note:     "a reserve violation returns from inside the gate block, so no post-gate stake exists",
		})
	}

	// ---- The terminal action.
	//
	// A legacy failure, an indeterminate stage and an unsupported input have no
	// producer counterpart at all: the pinned producer either crashed or this
	// model could not get far enough to predict an ending. Comparing them
	// against an empty expected reason manufactured an INDEPENDENT
	// disagreement out of a case where the model correctly declined to
	// predict — which then suppressed the settlement too. The test is on the
	// ACTION, not on the expected string being empty, because a pre-decision
	// exit can legitimately carry an empty reason.
	switch ev.Action {
	case ActionLegacyFailure, ActionIndeterminate, ActionUnsupported:
		add(Comparison{
			Field: "terminalReason", Verdict: VerdictUnavailable, Basis: BasisUnavailable,
			Recorded: rec.TerminalReason,
			Note:     "the replayed action has no producer terminal reason to compare against",
		})
	default:
		add(compareString("terminalReason", rec.TerminalReason, expectedTerminalReason(ev), true,
			terminalBasis(ev, derived), ""))
	}

	for _, cmp := range sc.Comparisons {
		switch {
		case cmp.Verdict == VerdictUnavailable:
			sc.UnavailablePairs++
		case cmp.Basis == BasisCircular:
			sc.CircularComparisons++
		case cmp.Basis == BasisWitnessed:
			sc.WitnessedEchoes++
		case cmp.Basis == BasisConditioned:
			if cmp.Verdict == VerdictAgree {
				sc.ConditionedAgree++
			} else {
				sc.ConditionedDisagree++
			}
		case cmp.Basis == BasisIndependent:
			if cmp.Verdict == VerdictAgree {
				sc.IndependentAgree++
			} else {
				sc.IndependentDisagree++
			}
		}
	}

	sc.Settlement = assessSettlement(sc, ev, conditioned, s)
	return sc
}

// ProjectSettlementFacts reads an attempt's POST-DECISION facts.
//
// It is deliberately a separate call from [ProjectDecisionCase]: these facts
// exist on the far side of the causal cut, and the only stage entitled to them
// is [Score]. Nothing here can reach [Evaluate], because [Evaluate] does not
// accept this type.
func ProjectSettlementFacts(a AttemptKnowledge) SettlementFacts {
	out := SettlementFacts{
		PostDecisionFacts: len(a.PostDecision),
		// There is deliberately no branch below reading a round-level terminal
		// fact into an attempt: the pinned producer does not put the attempt
		// discriminator on one, so no attempt's post-decision facts can carry
		// it. See ResolutionLinkage.
		ResolutionLinkage: ResolutionNotLinkableToAttempt,
	}
	for _, r := range a.PostDecision {
		switch {
		case r.Kind == KindPlacement && r.Payload.Phase == PhaseCallStarted:
			out.PlacementCallStarted = true
			if v, ok := r.Payload.Counters[CounterStake]; ok {
				stake := v
				out.PlacementStake = &stake
			}
			if r.Payload.OutcomeSlot != nil {
				slot := *r.Payload.OutcomeSlot
				out.PlacementSlot = &slot
			}
		case r.Kind == KindPlacement && r.Payload.Phase == PhaseCallReturned:
			out.PlacementCallReturned = true
			out.PlacementAccepted = r.Payload.ReasonCode == "OK"
			out.PlacementErrorClass = r.Payload.ErrorClass
		}
	}
	return out
}

// assessSettlement decides what the recorded settlement says about the
// REPLAYED decision — which is not the same question as what happened.
func assessSettlement(sc Scorecard, ev Evaluation, conditioned bool, s SettlementFacts) SettlementAssessment {
	out := SettlementAssessment{Facts: s, ROI: SettlementUnknown, Assessment: SettlementUnknown}
	switch {
	case sc.IndependentDisagree > 0 || sc.ConditionedDisagree > 0:
		// The replay did not reproduce the recorded decision, so the recorded
		// settlement is the settlement of a DIFFERENT decision.
		out.Assessment = SettlementUnknown
	case ev.Action != ActionWouldAttemptPlacement:
		out.Assessment = SettlementNotApplicable
	case !s.PlacementCallReturned || !s.PlacementAccepted:
		// A call that Twitch REJECTED settled nothing. Reporting the recorded
		// settlement as describing the replayed decision in that case would
		// attribute an outcome to a bet that was never taken.
		out.Assessment = SettlementUnknown
	case conditioned:
		out.Assessment = SettlementConditional
	default:
		out.Assessment = SettlementAppliesToReplay
	}
	return out
}

// expectedTerminalReason maps a replayed action onto the reason code the
// pinned producer records for it.
func expectedTerminalReason(ev Evaluation) string {
	switch ev.Action {
	case ActionPreDecisionExit:
		return ev.ActionReason
	case ActionHealthGated:
		return "HEALTH_GATED"
	case ActionReserveViolation:
		return "RESERVE_VIOLATION"
	case ActionBelowMinimum:
		return "BELOW_MINIMUM_POINTS"
	case ActionFilterRejected:
		return "FILTER_REJECTED"
	case ActionWouldAttemptPlacement:
		return "OK"
	default:
		// LEGACY_FAILURE, INDETERMINATE and UNSUPPORTED have no producer
		// counterpart: the pinned producer either crashed or the model could
		// not get far enough to predict an ending.
		return ""
	}
}

// terminalBasis keeps a witnessed pre-decision exit out of the independent
// tally: this model echoed that reason rather than deriving it.
func terminalBasis(ev Evaluation, derived string) string {
	if ev.Action == ActionPreDecisionExit {
		return BasisWitnessed
	}
	return derived
}

func recordedChoiceIndex(rec RecordedResults) (int, bool) {
	if rec.ChoiceIndexRecorded {
		return rec.ChoiceIndex, true
	}
	if rec.ChoiceWasNegative {
		// The store drops a negative index, and under an EXECUTED stage that
		// absence reconstructs the policy's -1 exactly.
		return -1, true
	}
	return 0, false
}

func compareInt(field string, recorded, computed int, has bool, basis, note string) Comparison {
	if !has {
		return Comparison{Field: field, Verdict: VerdictUnavailable, Basis: BasisUnavailable,
			Computed: itoa(int64(computed)), Note: note}
	}
	return Comparison{
		Field: field, Verdict: verdictOf(recorded == computed), Basis: basis,
		Recorded: itoa(int64(recorded)), Computed: itoa(int64(computed)), Note: note,
	}
}

func compareInt64(field string, recorded, computed int64, has bool, basis, note string) Comparison {
	if !has {
		return Comparison{Field: field, Verdict: VerdictUnavailable, Basis: BasisUnavailable,
			Computed: itoa(computed), Note: note}
	}
	return Comparison{
		Field: field, Verdict: verdictOf(recorded == computed), Basis: basis,
		Recorded: itoa(recorded), Computed: itoa(computed), Note: note,
	}
}

func compareString(field, recorded, computed string, has bool, basis, note string) Comparison {
	if !has {
		return Comparison{Field: field, Verdict: VerdictUnavailable, Basis: BasisUnavailable, Note: note}
	}
	return Comparison{
		Field: field, Verdict: verdictOf(recorded == computed), Basis: basis,
		Recorded: recorded, Computed: computed, Note: note,
	}
}

func compareBool(field string, recorded, computed, has bool, basis, note string) Comparison {
	if !has {
		return Comparison{Field: field, Verdict: VerdictUnavailable, Basis: BasisUnavailable, Note: note}
	}
	return Comparison{
		Field: field, Verdict: verdictOf(recorded == computed), Basis: basis,
		Recorded: boolText(recorded, "true", "false"), Computed: boolText(computed, "true", "false"),
		Note: note,
	}
}

// compareFloat compares with BIT equality rather than a tolerance.
//
// The recorded value came out of the same float64 arithmetic this model
// performs, so an exact match is the correct expectation and a tolerance would
// hide precisely the drift worth finding. NaN is handled explicitly because it
// is not equal to itself: two NaNs are the same recorded fact.
func compareFloat(field string, recorded, computed float64, has bool, basis, note string) Comparison {
	if !has {
		return Comparison{Field: field, Verdict: VerdictUnavailable, Basis: BasisUnavailable, Note: note}
	}
	equal := recorded == computed ||
		(math.IsNaN(recorded) && math.IsNaN(computed)) ||
		math.Float64bits(recorded) == math.Float64bits(computed)
	return Comparison{
		Field: field, Verdict: verdictOf(equal), Basis: basis,
		Recorded: ftoa(recorded), Computed: ftoa(computed), Note: note,
	}
}

func verdictOf(ok bool) string {
	if ok {
		return VerdictAgree
	}
	return VerdictDisagree
}

func boolText(v bool, yes, no string) string {
	if v {
		return yes
	}
	return no
}
