package analytics

// Store side of the auto-decision ENVELOPE (P1.5).
//
// P1 persisted that an automatic betting decision happened and what it chose.
// It did not persist what the decision was computed FROM, so a reader could
// see the choice and never re-derive it. This file adds the closed, sanitized
// projection of those inputs and of the original, pre-mutation results.
//
// It is an ADDITIVE optional part of ObservationPayload. Three consequences,
// each load-bearing:
//
//   - ObservationPayloadVersion does NOT move. The projection's meaning is
//     unchanged; a fact without an envelope renders byte-identical JSON to the
//     one today's code would write. The constant is also hashed into every
//     row's digest from the compile-time value while witness verification
//     re-reads the stored bytes, so bumping it would make every historical row
//     fail its own witness and read as an integrity error.
//   - ObservationProducerRevision DOES move. A reader must be able to tell a
//     session that COULD carry an envelope from one that never could, and an
//     absent envelope in an old session is a contract fact, not a decision
//     computed from nothing. A foreign revision is not an integrity failure —
//     it only tells the reader which contract's invariants apply.
//   - Every ceiling breach REFUSES THE WHOLE FACT, exactly as the outcome and
//     predictor ceilings already do. A shortened envelope would be a decision
//     record that looks complete and is not, which is the one thing this trail
//     exists to prevent.
//
// Privacy: nothing here is an erasable identity. A privacy erasure reaches a
// fact only through its COLUMNS, so an identifier placed inside payload_json
// would outlive every erasure that could apply to it. The only identifier the
// envelope carries is the outcome id — a round-scoped Twitch coordinate needed
// to link a choice to its placement call, never a channel, a login, a user or
// a predictor. Outcome display text is deliberately absent.

import "math"

// Closed vocabularies for the envelope. As everywhere else in this trail, a
// value outside its vocabulary becomes ValueUnknown rather than being stored
// as itself — the fact stays true, it just stops claiming to know a value it
// could not project.
var (
	// observationStageStates says whether a stage of the decision path ran.
	// It exists because 0, false and "" are all legitimate RESULTS of a stage
	// that ran, so absence cannot be encoded as a zero value.
	observationStageStates = []string{
		DecisionStageExecuted, DecisionStageNotReached, ValueUnknown,
	}

	// observationHealthStates distinguishes the two structurally different
	// reasons the health gate was not consulted from the two verdicts it
	// returns when it is. A boolean would merge four distinct facts into two.
	observationHealthStates = []string{
		DecisionHealthNotReached, DecisionHealthDisabled, DecisionHealthNoGate,
		DecisionHealthAllowed, DecisionHealthDenied, ValueUnknown,
	}

	// observationBetStrategies mirrors the models strategy vocabulary. It is
	// spelled out rather than imported so the STORED vocabulary is frozen
	// independently of whatever the domain happens to declare today — a value
	// outside it is recorded as UNKNOWN rather than as a strategy this build
	// cannot replay. (The package does import internal/models elsewhere; the
	// duplication is a deliberate contract boundary, not a dependency
	// constraint.) TestTheEnvelopeVocabulariesCoverEveryDomainConstant is what
	// keeps the two from drifting apart silently.
	observationBetStrategies = []string{
		"MOST_VOTED", "HIGH_ODDS", "PERCENTAGE", "SMART_MONEY", "SMART",
		"NUMBER_1", "NUMBER_2", "NUMBER_3", "NUMBER_4",
		"NUMBER_5", "NUMBER_6", "NUMBER_7", "NUMBER_8",
		ValueUnknown,
	}

	observationDelayModes = []string{"FROM_START", "FROM_END", "PERCENTAGE", ValueUnknown}

	// observationFilterKeys is the outcome-key vocabulary a filter condition
	// may compare on.
	observationFilterKeys = []string{
		"percentage_users", "odds_percentage", "odds", "top_points",
		"total_users", "total_points", "decision_users", "decision_points",
		ValueUnknown,
	}

	observationFilterComparisons = []string{"GT", "LT", "GTE", "LTE", ValueUnknown}

	// observationStakeGateReasons is the closed gate-reason vocabulary
	// EvaluateStake and the health gate share. The EMPTY string is a real
	// member — it is models.GateNone, "no gate bound this stake" — and is
	// distinguishable from absence only because the stage state says the
	// stage ran.
	observationStakeGateReasons = []string{
		"max_stake_percent", "reserve_violation",
		"health_gql_api_degraded", "health_gql_api_failed",
		"health_pubsub_degraded", "health_pubsub_failed",
		ValueUnknown,
	}
)

// Stage-state and health-state members, exported so a reader can compare
// without re-spelling the strings.
const (
	DecisionStageExecuted   = "EXECUTED"
	DecisionStageNotReached = "NOT_REACHED"

	DecisionHealthNotReached = "NOT_REACHED"
	DecisionHealthDisabled   = "DISABLED"
	DecisionHealthNoGate     = "NO_GATE"
	DecisionHealthAllowed    = "ALLOWED"
	DecisionHealthDenied     = "DENIED"
)

// ObservationBetSettings is the effective bet settings a decision consumed.
//
// All nine fields of the domain's settings are projected. None is omitted on
// the grounds that "the strategy in force did not read it": which fields a
// decision reads is a function of the very values being recorded, so a
// selective projection would make the record's completeness depend on the
// thing it is evidence for.
//
// No field uses omitempty. Every one of them has a legitimate zero — a zero
// percentage, a zero gap, a disabled stealth mode — and omitting a zero would
// encode a real value as a missing key.
type ObservationBetSettings struct {
	Strategy      string  `json:"strategy"`
	Percentage    int     `json:"percentage"`
	PercentageGap int     `json:"percentageGap"`
	MaxPoints     int     `json:"maxPoints"`
	MinimumPoints int     `json:"minimumPoints"`
	StealthMode   bool    `json:"stealthMode"`
	Delay         float64 `json:"delay"`
	DelayMode     string  `json:"delayMode"`

	// FilterCondition is the round's optional filter, or nil when it genuinely
	// had none. Nil is a value here, not an absence: Skip returns "do not
	// skip" immediately when no condition exists, so the distinction changes
	// what a replay computes.
	FilterCondition *ObservationFilterCondition `json:"filterCondition,omitempty"`
}

// ObservationFilterCondition is the filter a round carried.
type ObservationFilterCondition struct {
	By    string  `json:"by"`
	Where string  `json:"where"`
	Value float64 `json:"value"`
}

// ObservationModelOutcome is one entry of the MODEL outcome state a decision
// read, in the order the model held it.
//
// This is deliberately not ObservationOutcome. That type projects a frame as
// it arrived on the wire; this one projects what the decision actually
// consulted, including derived values the model accumulated across earlier
// frames. The two are not interchangeable, and reconstructing this one from
// the newest wire projection would be wrong: the model refreshes its derived
// values only under its own conditions, so it can legitimately be carrying
// values derived from an earlier frame than the last one observed.
type ObservationModelOutcome struct {
	// Slot is the positional index within the model's vector, re-indexed by
	// the sanitizer so a stored slot is always its true position.
	Slot int `json:"slot"`
	// Present separates an outcome the model held from a hole in its vector.
	// Every numeric field below has a legitimate zero, so a hole recorded as a
	// zero-point outcome would be a fabricated input rather than a missing
	// one. This is MODEL state, not a wire-presence claim.
	Present bool `json:"present"`
	// ID is the round-scoped outcome identifier the placement call carries. It
	// is the one identifier in the envelope, and it is not an erasable
	// identity: it names an outcome of a round, never a channel or a person.
	// The outcome's title and colour are deliberately not projected.
	ID string `json:"id"`

	TotalUsers  int64 `json:"totalUsers"`
	TotalPoints int64 `json:"totalPoints"`
	// TopPoints is the value the model had already computed: the LARGEST
	// single stake among the round's top predictors. It is one viewer's wager
	// amount, NOT a pool aggregate — say so plainly, because calling it an
	// aggregate would understate what is stored. It is kept because the
	// strategy selects on it and stealth mode reduces below it, so a decision
	// that read it cannot be replayed without it. It carries no identity, and
	// it authorizes reading or retaining nothing else about a predictor: the
	// wire projection still keeps only a COUNT of them.
	TopPoints int64 `json:"topPoints"`

	PercentageUsers float64 `json:"percentageUsers"`
	Odds            float64 `json:"odds"`
	OddsPercentage  float64 `json:"oddsPercentage"`
}

// ObservationDecisionEnvelope is the persisted projection of ONE automatic
// decision attempt: what it read, what each stage returned, and what the
// caller then did with those returns.
//
// Every optional field is a pointer whose nil means "this stage produced no
// such value", and every stage that could be skipped carries an explicit state
// saying whether it ran. A reader never has to guess whether a zero is a
// result or an absence.
type ObservationDecisionEnvelope struct {
	// AttemptID is the producer's discriminator for this attempt. It also
	// appears in Counters under autoAttemptId on every other fact of the same
	// attempt, including its placement calls, so the attempt's facts link by a
	// minted identity rather than by a timestamp or a reusable event id.
	AttemptID uint64 `json:"attemptId"`

	// The decision-time settings snapshot. This is NOT the admission-time
	// snapshot: that one is recorded separately on the schedule fact, and the
	// two are never merged or asserted equal.
	SettingsStage string                  `json:"settingsStage"`
	Settings      *ObservationBetSettings `json:"settings,omitempty"`

	// The inputs the strategy read.
	CalculateStage string                    `json:"calculateStage"`
	Balance        *int64                    `json:"balance,omitempty"`
	Outcomes       []ObservationModelOutcome `json:"outcomes,omitempty"`
	BetTotalUsers  *int64                    `json:"betTotalUsers,omitempty"`
	BetTotalPoints *int64                    `json:"betTotalPoints,omitempty"`

	// The ORIGINAL results of the strategy, captured before the caller's
	// percent clamp could overwrite the amount. Recording only the final stake
	// would lose what the strategy proposed, which is the quantity a replay
	// has to reproduce.
	ChoiceIndex     *int   `json:"choiceIndex,omitempty"`
	ChoiceOutcomeID string `json:"choiceOutcomeId,omitempty"`
	ChoiceAmount    *int64 `json:"choiceAmount,omitempty"`

	// Both results of the filter's single evaluation. A false result is a real
	// answer that the previous contract could not express at all: it recorded
	// only rejections, so "the filter ran and passed" and "the filter never
	// ran" were the same absence.
	SkipStage    string   `json:"skipStage"`
	SkipResult   *bool    `json:"skipResult,omitempty"`
	SkipCompared *float64 `json:"skipCompared,omitempty"`

	// The health gate. HealthReason is only meaningful once HealthStage says
	// the gate was evaluated.
	HealthStage  string `json:"healthStage"`
	HealthReason string `json:"healthReason,omitempty"`

	// The stake gate's inputs and its complete return. StakeAllowed is what
	// the function RETURNED; whether the caller adopted it is ClampApplied,
	// which is recorded at the assignment rather than derived from the reason.
	// The two differ whenever the binding gate is not the percent gate — and a
	// derived flag would get exactly that case wrong.
	StakeStage          string `json:"stakeStage"`
	RiskMaxStakePercent *int   `json:"riskMaxStakePercent,omitempty"`
	RiskReservePoints   *int   `json:"riskReservePoints,omitempty"`
	StakeAllowed        *int64 `json:"stakeAllowed,omitempty"`
	StakeReason         string `json:"stakeReason,omitempty"`
	StakeLimit          *int64 `json:"stakeLimit,omitempty"`

	// ClampApplied is the caller's assignment, observed at the assignment.
	// FinalAmount is the stake the caller carried OUT of the gate block, and
	// is absent when the attempt returned from inside it — a reserve violation
	// never produces a post-gate stake, and inventing one would claim a stage
	// that did not happen.
	ClampApplied *bool  `json:"clampApplied,omitempty"`
	FinalAmount  *int64 `json:"finalAmount,omitempty"`
}

// sanitizeObservationBetSettings projects a settings snapshot onto the closed
// vocabulary. ok is false when a value breaches a frozen ceiling, which
// refuses the whole fact rather than storing a settings snapshot that is
// subtly not the one the decision used.
func sanitizeObservationBetSettings(in *ObservationBetSettings) (*ObservationBetSettings, bool) {
	if in == nil {
		return nil, true
	}
	// A non-finite delay cannot be rendered as JSON at all, and substituting a
	// plausible number would make the snapshot a fabrication. The fact is
	// refused whole and counted as a loss, which is visible; a substitution
	// would not be.
	if !observationFiniteFloat(in.Delay) {
		return nil, false
	}
	out := &ObservationBetSettings{
		Strategy:      closedValue(in.Strategy, observationBetStrategies),
		Percentage:    in.Percentage,
		PercentageGap: in.PercentageGap,
		MaxPoints:     in.MaxPoints,
		MinimumPoints: in.MinimumPoints,
		StealthMode:   in.StealthMode,
		Delay:         in.Delay,
		DelayMode:     closedValue(in.DelayMode, observationDelayModes),
	}
	if in.FilterCondition != nil {
		if !observationFiniteFloat(in.FilterCondition.Value) {
			return nil, false
		}
		out.FilterCondition = &ObservationFilterCondition{
			By:    closedValue(in.FilterCondition.By, observationFilterKeys),
			Where: closedValue(in.FilterCondition.Where, observationFilterComparisons),
			Value: in.FilterCondition.Value,
		}
	}
	return out, true
}

// sanitizeObservationDecisionEnvelope projects a decision envelope onto the
// closed vocabulary. ok is false on any ceiling breach, which refuses the
// whole fact: a decision record missing an outcome, or carrying a substituted
// number, is worse than no decision record at all, because nothing downstream
// could tell it apart from a complete one.
func sanitizeObservationDecisionEnvelope(in *ObservationDecisionEnvelope) (*ObservationDecisionEnvelope, bool) {
	if in == nil {
		return nil, true
	}
	settings, ok := sanitizeObservationBetSettings(in.Settings)
	if !ok {
		return nil, false
	}
	out := &ObservationDecisionEnvelope{
		AttemptID:     in.AttemptID,
		SettingsStage: closedValue(in.SettingsStage, observationStageStates),
		Settings:      settings,

		CalculateStage: closedValue(in.CalculateStage, observationStageStates),
		Balance:        copyInt64Ptr(in.Balance),
		BetTotalUsers:  copyInt64Ptr(in.BetTotalUsers),
		BetTotalPoints: copyInt64Ptr(in.BetTotalPoints),

		ChoiceAmount: copyInt64Ptr(in.ChoiceAmount),

		SkipStage:  closedValue(in.SkipStage, observationStageStates),
		SkipResult: copyBoolPtr(in.SkipResult),

		HealthStage:  closedValue(in.HealthStage, observationHealthStates),
		HealthReason: closedOptional(in.HealthReason, observationStakeGateReasons),

		StakeStage:          closedValue(in.StakeStage, observationStageStates),
		RiskMaxStakePercent: copyIntPtr(in.RiskMaxStakePercent),
		RiskReservePoints:   copyIntPtr(in.RiskReservePoints),
		StakeAllowed:        copyInt64Ptr(in.StakeAllowed),
		StakeReason:         closedOptional(in.StakeReason, observationStakeGateReasons),
		StakeLimit:          copyInt64Ptr(in.StakeLimit),

		ClampApplied: copyBoolPtr(in.ClampApplied),
		FinalAmount:  copyInt64Ptr(in.FinalAmount),
	}

	// The chosen outcome id is stored verbatim: over the ceiling, or altered by
	// any normalization, the fact is refused rather than stored changed. A
	// shortened or trimmed id names a different outcome than the bet did.
	if id, idOK := verbatimIdentifier(in.ChoiceOutcomeID); idOK {
		out.ChoiceOutcomeID = id
	} else {
		return nil, false
	}

	// A negative choice index is the ABSENCE of a chosen outcome — Calculate's
	// own "no strategy matched" answer — not a breach; it stays absent. An
	// index past the outcome ceiling names an outcome that could never have
	// been stored, so it refuses the fact, exactly as OutcomeSlot does.
	if in.ChoiceIndex != nil {
		switch c := *in.ChoiceIndex; {
		case c >= MaxObservationOutcomes:
			return nil, false
		case c >= 0:
			choice := c
			out.ChoiceIndex = &choice
		}
	}

	if in.SkipCompared != nil {
		if !observationFiniteFloat(*in.SkipCompared) {
			return nil, false
		}
		compared := *in.SkipCompared
		out.SkipCompared = &compared
	}

	if n := len(in.Outcomes); n > 0 {
		// Over the ceiling the fact is refused. Keeping the first 64 of 70
		// would store a decision as if it had read the whole vector, and
		// nothing downstream could tell it had not.
		if n > MaxObservationOutcomes {
			return nil, false
		}
		out.Outcomes = make([]ObservationModelOutcome, 0, n)
		for i := 0; i < n; i++ {
			o := in.Outcomes[i]
			if !observationFiniteFloat(o.PercentageUsers) ||
				!observationFiniteFloat(o.Odds) ||
				!observationFiniteFloat(o.OddsPercentage) {
				return nil, false
			}
			id, idOK := verbatimIdentifier(o.ID)
			if !idOK {
				return nil, false
			}
			out.Outcomes = append(out.Outcomes, ObservationModelOutcome{
				// Positional, exactly as the wire outcomes are re-indexed: a
				// stored slot is always its true position in the vector.
				Slot:            i,
				Present:         o.Present,
				ID:              id,
				TotalUsers:      o.TotalUsers,
				TotalPoints:     o.TotalPoints,
				TopPoints:       o.TopPoints,
				PercentageUsers: o.PercentageUsers,
				Odds:            o.Odds,
				OddsPercentage:  o.OddsPercentage,
			})
		}
	}
	return out, true
}

// observationFiniteFloat reports whether v can be persisted losslessly.
//
// A NaN or an infinity has no JSON number form: encoding/json refuses it, and
// any substitute — 0, the ceiling, the previous value — would be a number the
// decision never used, stored as though it had. Refusing the whole fact costs
// one counted drop and keeps the trail's meaning; substituting costs the trail
// its meaning and reports success.
func observationFiniteFloat(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func copyInt64Ptr(v *int64) *int64 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func copyIntPtr(v *int) *int {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func copyBoolPtr(v *bool) *bool {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

// verbatimIdentifier bounds an opaque outcome id WITHOUT normalizing it.
//
// boundedIdentifier trims before it measures. That is right for the routing
// identities it was written for — a channel id, an event id, a login — because
// those are matched through comparableIdentity, which trims too, so the stored
// form and every form they are compared against agree.
//
// An outcome id is not one of those. Nothing normalizes it anywhere else: the
// model keeps whatever the frame carried and the placement mutation sends
// exactly those bytes to Twitch. So a padded id would be stored trimmed while
// the bet named the untrimmed one — a record that is hashed, witnessed and
// counted as complete while naming a different outcome than the decision chose,
// which is precisely what this file refuses to do for a truncated id. Worse,
// trimming first also lets an id that is over the frozen ceiling BECAUSE of its
// padding slip under it, turning a refusal into an acceptance.
//
// So refuse anything normalization would change, rather than storing the
// changed form. A real Twitch outcome id is an unpadded UUID, and an absent
// choice is the empty string; neither is altered by trimming, so no legitimate
// decision is refused here.
func verbatimIdentifier(v string) (string, bool) {
	bounded, ok := boundedIdentifier(v)
	return v, ok && bounded == v
}
