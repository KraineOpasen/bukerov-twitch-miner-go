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
	// LimitationEvaluationShapeInconsistent — the evaluation's terminal action
	// claims stages it does not carry as EXECUTED.
	LimitationEvaluationShapeInconsistent = "EVALUATION_SHAPE_INCONSISTENT_WITH_ACTION"
	LimitationNoDueFact                   = "ATTEMPT_OPENING_FACT_ABSENT_FROM_SLICE"
	LimitationPolicyNotRun                = "ATTEMPT_EXITED_BEFORE_THE_POLICY_RAN"
	// LimitationEvaluationCaseMismatch marks a scorecard whose evaluation was
	// produced from DIFFERENT evidence than the case it is scored against.
	LimitationEvaluationCaseMismatch = "EVALUATION_DOES_NOT_BELONG_TO_THIS_CASE"
	// LimitationModelProvenanceMismatch marks a scorecard whose case and
	// evaluation were produced by different builds of this model, or by a build
	// other than the one scoring them.
	LimitationModelProvenanceMismatch = "MODEL_PROVENANCE_DOES_NOT_MATCH"
	// LimitationPlacementNotAttributable marks a settlement whose recorded
	// placement arguments do not match what the replay derived.
	LimitationPlacementNotAttributable = "PLACEMENT_ARGUMENTS_DO_NOT_MATCH_THE_REPLAY"
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

	// PlacementCoherence says whether the attempt's placement facts have the
	// SHAPE the pinned producer writes: exactly one CALL_STARTED followed by
	// exactly one CALL_RETURNED, both carrying a stake and an outcome slot,
	// and both carrying the SAME ones.
	//
	// The producer emits the pair around the one existing call and passes the
	// same arguments to both, so anything else is a corrupt or edited slice.
	// Reading the stake from any started fact and acceptance from any returned
	// fact would combine two unrelated calls into one affirmative settlement.
	PlacementCoherence string `json:"placementCoherence"`

	// Attempt and CommonInputDigest bind these facts to the case they were
	// projected from.
	//
	// Matching arguments are NOT attribution. A stake and a two-option slot are
	// low-cardinality enough that a different attempt on the same round can
	// carry the same pair by coincidence, so a caller handing Score another
	// attempt's facts would otherwise pass every argument check. The binding is
	// stamped by ProjectSettlementFacts and verified by Score; facts built by
	// hand carry none and can never be affirmative.
	Attempt           *AttemptKey `json:"attempt,omitempty"`
	CommonInputDigest string      `json:"commonInputDigest,omitempty"`

	PostDecisionFacts int `json:"postDecisionFacts"`
}

// Placement-shape verdicts.
const (
	// PlacementShapeCoherent is the one shape the pinned producer writes.
	PlacementShapeCoherent = "COHERENT"
	// PlacementShapeAbsent is an attempt with no placement facts at all — an
	// ordinary state for every exit that did not reach a call.
	PlacementShapeAbsent = "ABSENT"
	// PlacementShapeIncoherent is any other shape: duplicated, unordered,
	// half-present, missing arguments, or arguments that disagree between the
	// two facts of one call.
	PlacementShapeIncoherent = "INCOHERENT"
)

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
			Facts: detachSettlementFacts(s), ROI: SettlementUnknown, Assessment: SettlementUnknown,
		}
		return sc
	}

	// The digest alone is not enough. A case and an evaluation serialized by an
	// OLDER build carry the same old digest as each other, so the check above
	// passes — while the scorecard is stamped with this build's provenance and
	// its comparisons are read as this model's work. Everything scored here has
	// to have been produced by the model doing the scoring.
	current := CurrentModelProvenance()
	if c.Model != current || ev.Model != current {
		sc.Limitations = appendOnce(sc.Limitations, LimitationModelProvenanceMismatch)
		sc.Settlement = SettlementAssessment{
			Facts: detachSettlementFacts(s), ROI: SettlementUnknown, Assessment: SettlementUnknown,
		}
		return sc
	}

	// An ineligible case has, by definition, an input this model could not use.
	// Scoring it anyway produced UNAVAILABLE comparisons — which are not
	// disagreements — so an accepted placement could still carry it all the way
	// to APPLIES_TO_REPLAY. A scorecard that calls a case unevaluable and then
	// affirmatively claims its settlement is contradicting itself.
	if !c.Eligibility.Eligible {
		sc.Limitations = appendOnce(sc.Limitations, LimitationCaseExcluded)
	}
	if !c.Eligibility.ExercisesPolicy {
		sc.Limitations = appendOnce(sc.Limitations, LimitationPolicyNotRun)
	}
	// The GAP CAUSE is not the completeness predicate — the ORIGIN is. The
	// schema admits UNKNOWN and PREFIX_UNOBSERVED_AT_ADMISSION with a NULL gap
	// cause, so keying the limitation on a non-empty cause let a round whose
	// history this build cannot claim to know pass as fully captured, with both
	// capture fields dropped from the scorecard. Anything but
	// ACTIVE_AT_ADMISSION is qualified, and so is a missing origin.
	if c.RoundCaptureOrigin != RoundOriginActiveAtAdmission || c.RoundCaptureGapCause != "" {
		sc.Limitations = appendOnce(sc.Limitations, LimitationCaptureGap)
	}
	if !c.SawDueFact {
		sc.Limitations = appendOnce(sc.Limitations, LimitationNoDueFact)
	}
	// An action is a claim about which stages ran. WOULD_ATTEMPT_PLACEMENT says
	// the policy went all the way through, so the choice, the filter and the
	// clamp must each be EXECUTED. A partially decoded or caller-edited
	// evaluation can carry that action with those stages empty or NOT_REACHED,
	// and the per-stage `if ... == StageStateExecuted` guards below then emit
	// NO comparison at all for them — not an UNAVAILABLE one. UnavailablePairs
	// therefore stays zero, and a scorecard could report APPLIES_TO_REPLAY
	// having checked almost nothing the evaluation purported to contain.
	if ev.Action == ActionWouldAttemptPlacement && !stagesMatchAction(ev) {
		sc.Limitations = appendOnce(sc.Limitations, LimitationEvaluationShapeInconsistent)
		sc.Settlement = SettlementAssessment{
			Facts: detachSettlementFacts(s), ROI: SettlementUnknown, Assessment: SettlementUnknown,
		}
		return sc
	}

	// The refusal is placed AFTER the remaining limitations are recorded and
	// BEFORE any comparison is emitted. An unevaluable case is still worth
	// describing — a capture gap and a missing AUTO_DUE are why an operator
	// would look at it — but nothing it could compare may be counted, and no
	// settlement may be affirmed on comparisons that were never made.
	if !c.Eligibility.Eligible {
		sc.Settlement = SettlementAssessment{
			Facts: detachSettlementFacts(s), ROI: SettlementUnknown, Assessment: SettlementUnknown,
		}
		return sc
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
		add(Comparison{
			Field: "terminalPhase", Verdict: VerdictUnavailable, Basis: BasisUnavailable,
			Recorded: rec.TerminalPhase,
		})
	default:
		add(compareString("terminalReason", rec.TerminalReason, expectedTerminalReason(ev), true,
			terminalBasis(ev, derived), ""))
		// The reason alone is not the action. A record whose phase says
		// AUTO_SKIPPED/SKIP while its envelope replays to a placement is
		// contradicting itself, and comparing only the reason code would count
		// that as agreement — the phase and the decision were projected and
		// then never looked at.
		//
		// Both are compared UNCONDITIONALLY. Guarding on a non-empty recorded
		// value made an empty one produce no comparison at all, so a terminal
		// fact with a blank decision but a matching phase and reason raised no
		// disagreement and could carry a placement case to an affirmative
		// settlement. The producer writes SKIP or PLACE on every terminal auto
		// fact it emits, so blank is not an absence to be tolerated — it is a
		// record that cannot have come from it, and a disagreement is the
		// honest reading. ProjectDecisionCase also refuses such a case
		// outright; this is the second of the two.
		phase, decision := expectedTerminalShape(ev)
		add(compareString("terminalPhase", rec.TerminalPhase, phase, true,
			terminalBasis(ev, derived), ""))
		add(compareString("terminalDecision", rec.TerminalDecision, decision, true,
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
	key := a.Key
	out.Attempt = &key
	out.CommonInputDigest = a.CommonInputDigest

	// Collect the two placement facts rather than folding them in as they go
	// past. Folding is what allowed a stake read from one call and an
	// acceptance read from another to become a single affirmative settlement.
	var started, returned []SourceRecord
	startedFirst := true
	seenReturned := false
	for _, r := range a.PostDecision {
		if r.Kind != KindPlacement {
			continue
		}
		switch r.Payload.Phase {
		case PhaseCallStarted:
			if seenReturned {
				// A start after a return is not this call's start.
				startedFirst = false
			}
			started = append(started, r)
		case PhaseCallReturned:
			seenReturned = true
			returned = append(returned, r)
		}
	}

	if len(started) == 0 && len(returned) == 0 {
		out.PlacementCoherence = PlacementShapeAbsent
		return out
	}

	out.PlacementCallStarted = len(started) > 0
	out.PlacementCallReturned = len(returned) > 0

	if len(started) != 1 || len(returned) != 1 || !startedFirst {
		// Duplicated, unordered or half-present. Report the shape and stop:
		// there is no single call here whose arguments could be read.
		out.PlacementCoherence = PlacementShapeIncoherent
		return out
	}

	sStake, sHasStake := started[0].Payload.Counters[CounterStake]
	rStake, rHasStake := returned[0].Payload.Counters[CounterStake]
	sSlot, rSlot := started[0].Payload.OutcomeSlot, returned[0].Payload.OutcomeSlot

	// The producer passes the SAME stake and slot to both facts of one call, so
	// arguments that disagree between them describe no call it made. Both are
	// required: reading only the started fact left the returned fact's
	// arguments — which the producer does write — unexamined.
	if !sHasStake || !rHasStake || sStake != rStake ||
		sSlot == nil || rSlot == nil || *sSlot != *rSlot {
		out.PlacementCoherence = PlacementShapeIncoherent
		return out
	}

	out.PlacementCoherence = PlacementShapeCoherent
	stake, slot := sStake, *sSlot
	out.PlacementStake = &stake
	out.PlacementSlot = &slot
	out.PlacementAccepted = returned[0].Payload.ReasonCode == "OK"
	out.PlacementErrorClass = returned[0].Payload.ErrorClass
	return out
}

// assessSettlement decides what the recorded settlement says about the
// REPLAYED decision — which is not the same question as what happened.
func assessSettlement(sc Scorecard, ev Evaluation, conditioned bool, s SettlementFacts) SettlementAssessment {
	out := SettlementAssessment{
		Facts: detachSettlementFacts(s), ROI: SettlementUnknown, Assessment: SettlementUnknown,
	}
	switch {
	case sc.IndependentDisagree > 0 || sc.ConditionedDisagree > 0:
		// The replay did not reproduce the recorded decision, so the recorded
		// settlement is the settlement of a DIFFERENT decision.
		out.Assessment = SettlementUnknown
	case ev.Action != ActionWouldAttemptPlacement:
		out.Assessment = SettlementNotApplicable
	case sc.UnavailablePairs > 0:
		// An UNAVAILABLE comparison is not a disagreement, so the disagreement
		// guard above lets it through. That is the wrong reading: a comparison
		// that could not be made is EVIDENCE THAT IS MISSING, and an
		// affirmative settlement asserts that the recorded settlement
		// describes the replayed decision — a claim that cannot rest on a
		// field nobody could check.
		//
		// The narrow case that prompted this was a blank terminal decision,
		// now compared unconditionally; the guard is kept general because the
		// hole is general. A caller assembling its own case can leave any
		// recorded field unset, and only counting disagreements would call
		// that agreement.
		out.Assessment = SettlementUnknown
	case s.PlacementCoherence != PlacementShapeCoherent:
		// No single coherent call is recorded, so nothing here describes the
		// one this replay derived. ABSENT and INCOHERENT are different facts
		// about the store and the same answer about the settlement.
		out.Assessment = SettlementUnknown
	case s.Attempt == nil || *s.Attempt != sc.Key || s.CommonInputDigest != sc.CommonInputDigest:
		// Matching arguments are not attribution: a stake and a two-option
		// slot are low-cardinality enough for a DIFFERENT attempt on the same
		// round to carry the same pair. Only facts stamped by
		// ProjectSettlementFacts with this case's identity may be affirmative.
		out.Assessment = SettlementUnknown
	case !s.PlacementCallReturned || !s.PlacementAccepted:
		// A call that Twitch REJECTED settled nothing. Reporting the recorded
		// settlement as describing the replayed decision in that case would
		// attribute an outcome to a bet that was never taken.
		out.Assessment = SettlementUnknown
	case !placementMatchesReplay(ev, s):
		// Acceptance alone is not attribution. SettlementFacts carries no
		// attempt key of its own, so a batch caller can hand over another
		// attempt's facts — and a corrupt post-decision slice can hold an
		// accepted call with the wrong stake or slot. Requiring the recorded
		// call's ARGUMENTS to be the ones the replay derived is what ties the
		// settlement to this decision rather than to some accepted bet.
		out.Assessment = SettlementUnknown
	case conditioned:
		out.Assessment = SettlementConditional
	default:
		out.Assessment = SettlementAppliesToReplay
	}
	return out
}

// placementMatchesReplay reports whether the recorded placement call carried
// the arguments this replay derived.
//
// A missing stake or slot is not a match: the facts then say nothing about
// which decision they belong to, and "nothing" must not read as "yes".
func placementMatchesReplay(ev Evaluation, s SettlementFacts) bool {
	if s.PlacementStake == nil || s.PlacementSlot == nil {
		return false
	}
	if !ev.Clamp.HasFinal || *s.PlacementStake != int64(ev.Clamp.FinalAmount) {
		return false
	}
	return *s.PlacementSlot == ev.Choice.Index
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

// expectedTerminalShape is the phase and decision the pinned producer records
// for a replayed action. AUTO_DECIDED/PLACE is the only shape that means a bet
// was reached; every exit is AUTO_SKIPPED/SKIP.
func expectedTerminalShape(ev Evaluation) (phase, decision string) {
	if ev.Action == ActionWouldAttemptPlacement {
		return PhaseAutoDecided, "PLACE"
	}
	return PhaseAutoSkipped, "SKIP"
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

// RoundOriginActiveAtAdmission is the one capture origin that means the round's
// history is fully known. Every other value, and an absent one, qualifies the
// case rather than passing as complete.
const RoundOriginActiveAtAdmission = "ACTIVE_AT_ADMISSION"

// stagesMatchAction reports whether the evaluation carries the stages its own
// terminal action claims to have run.
//
// It is not a redundant check on this package's own evaluator, which cannot
// produce a mismatch. It is a check on the VALUE Score was handed: Evaluation
// is an exported struct a caller can build, serialize, partially decode or
// edit, and Score's other guards (digest, provenance) all pass for a value that
// is internally inconsistent in this particular way.
//
// EVERY stage a placement runs is listed, not the ones whose absence happens to
// be conspicuous. A first version checked only the choice, the filter and the
// clamp, which left the STAKE GATE unvalidated — the risk-gate result that
// determines the final stake. An evaluation could then claim a placement with
// StakeGate NOT_REACHED, Score would skip the stakeAllowed / stakeReason /
// stakeLimit comparisons entirely (skipped, not UNAVAILABLE), and an
// affirmative settlement could follow without the gate ever being checked. The
// health stage was missing for the same reason.
//
// TestTheRequiredPlacementShapeIsTheOneRealEvaluationsProduce derives the
// expected shape from real evaluations rather than from this list, so the two
// cannot drift apart again.
func stagesMatchAction(ev Evaluation) bool {
	return ev.Choice.State == StageStateExecuted &&
		ev.BaseStake.State == StageStateExecuted &&
		ev.Filter.State == StageStateExecuted &&
		ev.Health.State == StageStateWitnessed &&
		ev.StakeGate.State == StageStateExecuted &&
		ev.Clamp.State == StageStateExecuted &&
		ev.Minimum.State == StageStateExecuted &&
		ev.Clamp.HasFinal
}

// detachSettlementFacts deep-copies the reference fields of the settlement
// facts before they are retained in a scorecard.
//
// Without it the scorecard shares the caller's pointers: mutating
// *Attempt, *PlacementStake or *PlacementSlot after Score returned would change
// the stored artifact while its assessment still reflected the validation that
// ran against the ORIGINAL values — a machine-readable result that disagrees
// with the check that produced it.
func detachSettlementFacts(s SettlementFacts) SettlementFacts {
	if s.Attempt != nil {
		key := *s.Attempt
		s.Attempt = &key
	}
	if s.PlacementStake != nil {
		stake := *s.PlacementStake
		s.PlacementStake = &stake
	}
	if s.PlacementSlot != nil {
		slot := *s.PlacementSlot
		s.PlacementSlot = &slot
	}
	if s.Payout != nil {
		payout := *s.Payout
		s.Payout = &payout
	}
	if s.ReturnedStake != nil {
		returned := *s.ReturnedStake
		s.ReturnedStake = &returned
	}
	return s
}
