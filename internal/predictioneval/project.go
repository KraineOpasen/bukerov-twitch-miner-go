package predictioneval

import (
	"errors"
	"strconv"
)

// Seam 2 of 4: the causal cut.
//
// The producer persists an attempt's inputs and its results in ONE terminal
// envelope, because that is the only fact guaranteed to exist for every exit.
// Convenient for the writer; dangerous for a reader, because a single struct
// holding both halves lets a replay reach for a result the moment an input is
// awkward to reconstruct.
//
// This stage is where that stops being possible. It splits the envelope into
// [DecisionInputs] and [RecordedResults], and [Evaluate] takes only the first.
// The separation is enforced by the type system rather than by discipline: the
// evaluator cannot read a recorded choice, a later outcome or a post-placement
// state, because it is never handed one.
//
// Two things deserve naming because they look like inputs and are not:
//
//   - the health verdict and the pre-decision exit are WITNESSED EXTERNAL
//     INPUTS. The replay echoes them; it does not reconstruct the transport
//     health or the eligibility state that produced them, and a scorecard that
//     "agrees" about them has proven nothing about that hidden state.
//   - the recorded pre-risk stake is NOT an input. It reaches [Evaluate] only
//     as [ObservedRealization], only to constrain a random draw that cannot be
//     reproduced, and the stage it feeds is labelled accordingly.

// Ineligibility reasons. A case that cannot be evaluated says which input is
// missing or unusable — never by falling back to a zero, a false, a default
// strategy or a skip.
const (
	IneligibleMissingSettings         = "MISSING_SETTINGS"
	IneligibleMissingBalance          = "MISSING_BALANCE"
	IneligibleMissingChoiceAmount     = "MISSING_CHOICE_AMOUNT"
	IneligibleMissingSkipResult       = "MISSING_SKIP_RESULT"
	IneligibleMissingRiskInputs       = "MISSING_RISK_INPUTS"
	IneligibleBalanceUnrepresentable  = "BALANCE_UNREPRESENTABLE"
	IneligibleOutcomeUnrepresentable  = "OUTCOME_VALUE_UNREPRESENTABLE"
	IneligibleRiskUnrepresentable     = "RISK_VALUE_UNREPRESENTABLE"
	IneligibleInconsistentStageStates = "INCONSISTENT_STAGE_STATES"
	IneligibleUnknownStageState       = "UNKNOWN_STAGE_STATE"
	IneligibleNoEnvelope              = "NO_DECISION_ENVELOPE"
	// IneligibleIncompleteTerminalRecord — the terminal fact does not name the
	// action it recorded. The pinned producer writes a decision of SKIP or
	// PLACE on every terminal auto fact it emits, so a blank one is not an
	// absence to tolerate: it is a record that cannot have come from the
	// producer this model is bound to.
	IneligibleIncompleteTerminalRecord = "INCOMPLETE_TERMINAL_RECORD"
)

// DecisionInputs is everything the pinned policy READ, and nothing it produced.
//
// [Evaluate] receives this and only this. If a field is absent here, the
// corresponding stage is unevaluable — it never becomes a zero.
type DecisionInputs struct {
	// CommonInputDigest ties these inputs to the exact causally-closed slice
	// they were projected from.
	CommonInputDigest string `json:"commonInputDigest"`

	// ReachedDecision is the producer's witness that the policy ran at all.
	// False means the attempt exited before Calculate, and PreDecisionExit
	// says which exit.
	ReachedDecision bool `json:"reachedDecision"`
	// PreDecisionExit is the witnessed reason an attempt ended before the
	// policy ran (NOT_ELIGIBLE, ROUND_SUPPRESSED, ALREADY_PLACED, NOT_ACTIVE).
	// It is an EXTERNAL input: this model does not re-derive eligibility or
	// round state, and never claims to have.
	PreDecisionExit string `json:"preDecisionExit,omitempty"`

	Settings *BetSettingsInput `json:"settings,omitempty"`

	// Balance is the channel-point balance the policy computed the stake from,
	// already narrowed to the width the policy computes in.
	Balance        int  `json:"balance"`
	BalancePresent bool `json:"balancePresent"`

	// Outcomes is the MODEL vector the policy consulted, in the order it held
	// it, carrying the derived values the model had accumulated — never
	// recomputed from the newest wire totals.
	Outcomes        []OutcomeInput `json:"outcomes"`
	OutcomesPresent bool           `json:"outcomesPresent"`

	// BetTotalUsers / BetTotalPoints are the model's aggregate counters. The
	// pinned decision path does not read them; they are carried because they
	// were captured as inputs and a reader may want to check them against the
	// vector.
	BetTotalUsers  *int64 `json:"betTotalUsers,omitempty"`
	BetTotalPoints *int64 `json:"betTotalPoints,omitempty"`

	// The stake gate's configuration inputs.
	RiskPresent         bool `json:"riskPresent"`
	RiskMaxStakePercent int  `json:"riskMaxStakePercent"`
	RiskReservePoints   int  `json:"riskReservePoints"`

	// HealthState / HealthReason are the WITNESSED verdict of the account-wide
	// betting health gate. The gate consults live transport health this model
	// has no access to, so the verdict is echoed, never recomputed.
	HealthState  string `json:"healthState"`
	HealthReason string `json:"healthReason,omitempty"`

	// MinimumStake is the pinned floor the caller applies after the gates.
	MinimumStake int `json:"minimumStake"`
}

// BetSettingsInput is the decision-time settings snapshot.
type BetSettingsInput struct {
	Strategy      string  `json:"strategy"`
	Percentage    int     `json:"percentage"`
	PercentageGap int     `json:"percentageGap"`
	MaxPoints     int     `json:"maxPoints"`
	MinimumPoints int     `json:"minimumPoints"`
	StealthMode   bool    `json:"stealthMode"`
	Delay         float64 `json:"delay"`
	DelayMode     string  `json:"delayMode"`

	FilterCondition *SourceFilterCondition `json:"filterCondition,omitempty"`
}

// OutcomeInput is one model outcome, narrowed to the policy's width.
type OutcomeInput struct {
	Slot    int    `json:"slot"`
	Present bool   `json:"present"`
	ID      string `json:"id"`

	TotalUsers  int `json:"totalUsers"`
	TotalPoints int `json:"totalPoints"`
	TopPoints   int `json:"topPoints"`

	PercentageUsers float64 `json:"percentageUsers"`
	Odds            float64 `json:"odds"`
	OddsPercentage  float64 `json:"oddsPercentage"`
}

// ObservedRealization is the single observed value [Evaluate] may read.
//
// It exists for one stage and one stage only. When stealth mode reduces a
// stake it consumes a random draw, and no offline replay can reproduce that
// draw: the seed was never recorded, and inventing one would be presenting a
// new random number as the original. So the model establishes the choice, the
// base stake and whether stealth applies INDEPENDENTLY, and then uses the
// recorded pre-risk stake only to pin down which of the four possible integer
// reductions actually happened.
//
// Anything derived from that stage is as-operated reconstruction, not
// independent proof, and is reported separately in the [Scorecard].
type ObservedRealization struct {
	// StealthAmount is the ORIGINAL pre-risk stake the producer recorded.
	StealthAmount *int64 `json:"stealthAmount,omitempty"`
}

// RecordedResults is what the producer recorded the policy as having produced.
// These are COMPARISON TARGETS. Nothing here may be used to compute anything.
type RecordedResults struct {
	// ChoiceIndexRecorded is false when the store carries no index. Under a
	// stage state of EXECUTED that is not missing data: the store drops a
	// NEGATIVE index because negative is the policy's own "chose nothing",
	// which ChoiceWasNegative records explicitly.
	ChoiceIndex         int  `json:"choiceIndex"`
	ChoiceIndexRecorded bool `json:"choiceIndexRecorded"`
	ChoiceWasNegative   bool `json:"choiceWasNegative"`

	ChoiceOutcomeID string `json:"choiceOutcomeId,omitempty"`

	ChoiceAmount         int64 `json:"choiceAmount"`
	ChoiceAmountRecorded bool  `json:"choiceAmountRecorded"`

	SkipResult         bool     `json:"skipResult"`
	SkipResultRecorded bool     `json:"skipResultRecorded"`
	SkipCompared       *float64 `json:"skipCompared,omitempty"`

	StakeAllowed         int64  `json:"stakeAllowed"`
	StakeAllowedRecorded bool   `json:"stakeAllowedRecorded"`
	StakeReason          string `json:"stakeReason"`
	StakeLimit           int64  `json:"stakeLimit"`
	StakeLimitRecorded   bool   `json:"stakeLimitRecorded"`

	ClampApplied         bool `json:"clampApplied"`
	ClampAppliedRecorded bool `json:"clampAppliedRecorded"`

	FinalAmount         int64 `json:"finalAmount"`
	FinalAmountRecorded bool  `json:"finalAmountRecorded"`

	// TerminalPhase / TerminalDecision / TerminalReason are the producer's own
	// account of how the attempt ended.
	TerminalPhase    string `json:"terminalPhase"`
	TerminalDecision string `json:"terminalDecision,omitempty"`
	TerminalReason   string `json:"terminalReason,omitempty"`
	// TerminalOutcomeSlot is the slot the producer named on a placing
	// decision. The pinned producer writes it on the placing path ALONE, so
	// its presence on a record that says SKIP is itself a disagreement.
	TerminalOutcomeSlot *int `json:"terminalOutcomeSlot,omitempty"`
	// TerminalStake is the stake counter the terminal fact carries.
	//
	// On the placing path the producer writes exactly the amount it is about
	// to send, which is the post-clamp final. Every skip path writes a stake
	// too, but which stage's amount it holds varies by exit — so this is
	// projected for every ending and compared only where its meaning is
	// pinned.
	TerminalStake         int64 `json:"terminalStake,omitempty"`
	TerminalStakeRecorded bool  `json:"terminalStakeRecorded"`

	// Stage states, carried so a comparison can say which stages the producer
	// claims to have run.
	SettingsStage  string `json:"settingsStage"`
	CalculateStage string `json:"calculateStage"`
	SkipStage      string `json:"skipStage"`
	HealthStage    string `json:"healthStage"`
	StakeStage     string `json:"stakeStage"`
}

// CaseEligibility says whether a case can be evaluated, and why not.
type CaseEligibility struct {
	Eligible bool     `json:"eligible"`
	Reasons  []string `json:"reasons,omitempty"`
	// ExercisesPolicy is true only when the producer's own stage states say
	// the policy actually ran. A case that exited before the policy is
	// perfectly readable and contributes NO evidence about the policy, so the
	// two are counted apart.
	ExercisesPolicy bool `json:"exercisesPolicy"`
}

// DecisionCase is one attempt after the causal cut.
type DecisionCase struct {
	Key   AttemptKey      `json:"key"`
	Model ModelProvenance `json:"model"`

	// Source is the session the case was read under, and Anomalies are the
	// qualifications on that reading. They travel with the case so the
	// scorecard can name its own evidence instead of implying a clean one.
	Source    SourceProvenance `json:"source"`
	Anomalies []string         `json:"anomalies,omitempty"`

	RoundIncarnationID   string `json:"roundIncarnationId"`
	EventID              string `json:"eventId"`
	RoundCaptureOrigin   string `json:"roundCaptureOrigin"`
	RoundCaptureGapCause string `json:"roundCaptureGapCause"`
	CommonInputDigest    string `json:"commonInputDigest"`
	SawDueFact           bool   `json:"sawDueFact"`
	DueReason            string `json:"dueReason,omitempty"`

	Eligibility CaseEligibility `json:"eligibility"`

	Inputs   DecisionInputs      `json:"inputs"`
	Recorded RecordedResults     `json:"recorded"`
	Observed ObservedRealization `json:"observed"`
}

// ProjectDecisionCase is seam 2: it splits one attempt's common-knowledge
// slice into inputs and results.
//
// It is pure, and it reads NOTHING outside the slice it is given — not the
// post-decision facts, not the current configuration, not a neighbouring
// attempt.
func ProjectDecisionCase(a AttemptKnowledge) (DecisionCase, error) {
	out := DecisionCase{
		Key:                  a.Key,
		Model:                CurrentModelProvenance(),
		Source:               a.Source,
		Anomalies:            append([]string(nil), a.Anomalies...),
		RoundIncarnationID:   a.RoundIncarnationID,
		EventID:              a.EventID,
		RoundCaptureOrigin:   a.RoundCaptureOrigin,
		RoundCaptureGapCause: a.RoundCaptureGapCause,
		CommonInputDigest:    a.CommonInputDigest,
		SawDueFact:           a.SawDueFact,
		DueReason:            a.DueReason,
	}
	if a.TerminalIndex < 0 || a.TerminalIndex >= len(a.CommonInputSlice) {
		return DecisionCase{}, errors.New("predictioneval: attempt " +
			strconv.FormatUint(a.Key.AttemptID, 10) + " has no terminal fact in its slice")
	}
	terminal := a.CommonInputSlice[a.TerminalIndex]
	env := terminal.Payload.DecisionEnvelope
	if env == nil {
		out.Eligibility = CaseEligibility{Reasons: []string{IneligibleNoEnvelope}}
		return out, nil
	}

	out.Recorded = projectRecorded(terminal, env)
	out.Inputs.CommonInputDigest = a.CommonInputDigest
	out.Inputs.MinimumStake = PinnedMinimumStake

	var reasons []string

	// Stage states first: an unknown state is not a state, and inconsistent
	// states mean the snapshot cannot be read as one coherent attempt.
	if !isKnownStageState(env.SettingsStage) || !isKnownStageState(env.CalculateStage) ||
		!isKnownStageState(env.SkipStage) || !isKnownStageState(env.StakeStage) ||
		!isKnownHealthState(env.HealthStage) {
		reasons = append(reasons, IneligibleUnknownStageState)
	}
	if inconsistentStages(env) {
		reasons = append(reasons, IneligibleInconsistentStageStates)
	}
	// The terminal fact has to name the action it recorded. Without it the
	// comparison against the replayed action has nothing to compare, and a
	// blank value silently agreeing with everything is how an incomplete
	// record reaches an affirmative settlement.
	if out.Recorded.TerminalPhase == "" || out.Recorded.TerminalDecision == "" {
		reasons = append(reasons, IneligibleIncompleteTerminalRecord)
	}

	out.Inputs.HealthState = env.HealthStage
	out.Inputs.HealthReason = env.HealthReason
	out.Inputs.ReachedDecision = env.CalculateStage == StageExecuted

	if !out.Inputs.ReachedDecision {
		// The attempt exited before the policy. The terminal reason IS the
		// witnessed exit; there is nothing else to project and nothing to
		// invent.
		out.Inputs.PreDecisionExit = terminal.Payload.ReasonCode
		out.Eligibility = CaseEligibility{
			Eligible:        len(reasons) == 0,
			Reasons:         reasons,
			ExercisesPolicy: false,
		}
		return out, nil
	}

	// Settings.
	if env.Settings == nil || env.SettingsStage != StageExecuted {
		reasons = append(reasons, IneligibleMissingSettings)
	} else {
		s := *env.Settings
		in := BetSettingsInput{
			Strategy:      s.Strategy,
			Percentage:    s.Percentage,
			PercentageGap: s.PercentageGap,
			MaxPoints:     s.MaxPoints,
			MinimumPoints: s.MinimumPoints,
			StealthMode:   s.StealthMode,
			Delay:         s.Delay,
			DelayMode:     s.DelayMode,
		}
		if s.FilterCondition != nil {
			fc := *s.FilterCondition
			in.FilterCondition = &fc
		}
		out.Inputs.Settings = &in
	}

	// Balance.
	if env.Balance == nil {
		reasons = append(reasons, IneligibleMissingBalance)
	} else if n, ok := intFromStored(*env.Balance); !ok {
		reasons = append(reasons, IneligibleBalanceUnrepresentable)
	} else {
		out.Inputs.Balance, out.Inputs.BalancePresent = n, true
	}

	// Outcome vector. An empty vector is a REAL input — the policy answers on
	// it — so absence is recorded as an empty-but-present slice rather than as
	// a missing one.
	outs, ok := projectOutcomes(env.Outcomes)
	if !ok {
		reasons = append(reasons, IneligibleOutcomeUnrepresentable)
	} else {
		out.Inputs.Outcomes, out.Inputs.OutcomesPresent = outs, true
	}

	out.Inputs.BetTotalUsers = copyInt64(env.BetTotalUsers)
	out.Inputs.BetTotalPoints = copyInt64(env.BetTotalPoints)

	// Risk inputs exist exactly when the stake gate ran.
	if env.StakeStage == StageExecuted {
		switch {
		case env.RiskMaxStakePercent == nil || env.RiskReservePoints == nil:
			reasons = append(reasons, IneligibleMissingRiskInputs)
		default:
			out.Inputs.RiskMaxStakePercent = *env.RiskMaxStakePercent
			out.Inputs.RiskReservePoints = *env.RiskReservePoints
			out.Inputs.RiskPresent = true
		}
	}

	// Results that must exist once the policy ran. Their absence is an
	// inconsistent snapshot, not a value.
	if env.ChoiceAmount == nil {
		reasons = append(reasons, IneligibleMissingChoiceAmount)
	}
	if env.SkipResult == nil || env.SkipStage != StageExecuted {
		reasons = append(reasons, IneligibleMissingSkipResult)
	}

	// The ONE observed value, and only when stealth could have consumed a
	// draw. Handing it over unconditionally would make it available to stages
	// that must not see it; the evaluator additionally ignores it wherever
	// stealth does not apply, which is pinned by
	// TestObservedRealizationCannotInfluenceANonStealthCase.
	if out.Inputs.Settings != nil && out.Inputs.Settings.StealthMode && env.ChoiceAmount != nil {
		amount := *env.ChoiceAmount
		out.Observed.StealthAmount = &amount
	}

	out.Eligibility = CaseEligibility{
		Eligible:        len(reasons) == 0,
		Reasons:         reasons,
		ExercisesPolicy: true,
	}
	return out, nil
}

// projectRecorded copies the comparison targets out of the envelope.
func projectRecorded(terminal SourceRecord, env *SourceDecisionEnvelope) RecordedResults {
	r := RecordedResults{
		ChoiceOutcomeID:  env.ChoiceOutcomeID,
		StakeReason:      env.StakeReason,
		TerminalPhase:    terminal.Payload.Phase,
		TerminalDecision: terminal.Payload.Decision,
		TerminalReason:   terminal.Payload.ReasonCode,
		SettingsStage:    env.SettingsStage,
		CalculateStage:   env.CalculateStage,
		SkipStage:        env.SkipStage,
		HealthStage:      env.HealthStage,
		StakeStage:       env.StakeStage,
	}
	if terminal.Payload.OutcomeSlot != nil {
		slot := *terminal.Payload.OutcomeSlot
		r.TerminalOutcomeSlot = &slot
	}
	if v, ok := terminal.Payload.Counters[CounterStake]; ok {
		r.TerminalStake, r.TerminalStakeRecorded = v, true
	}
	if env.ChoiceIndex != nil {
		r.ChoiceIndex, r.ChoiceIndexRecorded = *env.ChoiceIndex, true
	} else if env.CalculateStage == StageExecuted {
		// The store drops a negative index rather than storing it, so under an
		// EXECUTED stage its absence reconstructs the policy's -1 exactly.
		r.ChoiceWasNegative = true
	}
	if env.ChoiceAmount != nil {
		r.ChoiceAmount, r.ChoiceAmountRecorded = *env.ChoiceAmount, true
	}
	if env.SkipResult != nil {
		r.SkipResult, r.SkipResultRecorded = *env.SkipResult, true
	}
	if env.SkipCompared != nil {
		v := *env.SkipCompared
		r.SkipCompared = &v
	}
	if env.StakeAllowed != nil {
		r.StakeAllowed, r.StakeAllowedRecorded = *env.StakeAllowed, true
	}
	if env.StakeLimit != nil {
		r.StakeLimit, r.StakeLimitRecorded = *env.StakeLimit, true
	}
	if env.ClampApplied != nil {
		r.ClampApplied, r.ClampAppliedRecorded = *env.ClampApplied, true
	}
	if env.FinalAmount != nil {
		r.FinalAmount, r.FinalAmountRecorded = *env.FinalAmount, true
	}
	return r
}

// projectOutcomes narrows the stored vector to the policy's width. A single
// unrepresentable count refuses the whole vector: replaying a decision against
// a silently truncated outcome would produce a choice the policy never made.
func projectOutcomes(src []SourceModelOutcome) ([]OutcomeInput, bool) {
	out := make([]OutcomeInput, 0, len(src))
	for _, o := range src {
		users, ok1 := intFromStored(o.TotalUsers)
		points, ok2 := intFromStored(o.TotalPoints)
		top, ok3 := intFromStored(o.TopPoints)
		if !ok1 || !ok2 || !ok3 {
			return nil, false
		}
		out = append(out, OutcomeInput{
			Slot:            o.Slot,
			Present:         o.Present,
			ID:              o.ID,
			TotalUsers:      users,
			TotalPoints:     points,
			TopPoints:       top,
			PercentageUsers: o.PercentageUsers,
			Odds:            o.Odds,
			OddsPercentage:  o.OddsPercentage,
		})
	}
	return out, true
}

// inconsistentStages checks the structural invariants the pinned producer
// guarantees. A snapshot that breaks one of them did not come from the code
// this model replays, so it is refused rather than read under assumptions that
// provably do not hold for it.
//
// The invariants, all read directly off the pinned decision path:
//
//	settings, calculate and skip are promoted together, in one block;
//	calculate NOT_REACHED implies the health gate and stake gate were never
//	    reached either;
//	health DENIED returns immediately, so the stake gate cannot have run;
//	any other health state falls through to the stake gate, so it must have;
//	an executed stake gate always records its inputs and its complete return;
//	a reserve violation returns from INSIDE the gate block, so there is no
//	    post-gate stake — and every other executed path has one;
//	the caller only assigns the clamp in the percent-gate branch.
func inconsistentStages(env *SourceDecisionEnvelope) bool {
	if env.SettingsStage != env.CalculateStage || env.CalculateStage != env.SkipStage {
		return true
	}
	if env.CalculateStage != StageExecuted {
		return env.HealthStage != HealthNotReached || env.StakeStage != StageNotReached
	}
	switch env.HealthStage {
	case HealthNotReached:
		return true
	case HealthDenied:
		if env.StakeStage != StageNotReached {
			return true
		}
		return false
	default:
		if env.StakeStage != StageExecuted {
			return true
		}
	}
	if env.RiskMaxStakePercent == nil || env.RiskReservePoints == nil ||
		env.StakeAllowed == nil || env.StakeLimit == nil || env.ClampApplied == nil {
		return true
	}
	if env.StakeReason == GateReserveViolation {
		if env.FinalAmount != nil {
			return true
		}
	} else if env.FinalAmount == nil {
		return true
	}
	if *env.ClampApplied && env.StakeReason != GatePercent {
		return true
	}
	return false
}

func isKnownStageState(v string) bool {
	return v == StageExecuted || v == StageNotReached
}

func isKnownHealthState(v string) bool {
	switch v {
	case HealthNotReached, HealthDisabled, HealthNoGate, HealthAllowed, HealthDenied:
		return true
	}
	return false
}

func copyInt64(v *int64) *int64 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
