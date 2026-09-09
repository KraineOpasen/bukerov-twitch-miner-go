package pubsub

// Producer side of the auto-decision ENVELOPE (P1.5).
//
// P1 recorded that an automatic betting decision happened and what it decided.
// It did not record what the decision was computed FROM, so the decision could
// be read but never re-derived: the effective bet settings, the model outcome
// state actually consulted, the balance, the risk gates and the original
// pre-mutation results were all absent. This file adds exactly those facts.
//
// It obeys the same three rules as every other observation call site in this
// package, and two more that are specific to an envelope:
//
//   - It performs no I/O, no JSON work and no wait. Every value it stores was
//     already computed by the business path; building the envelope is a bounded
//     set of field copies.
//   - It never changes control flow, the number of calls, or their order.
//     Calculate, Skip, the eligibility check, the health gate and EvaluateStake
//     are each still invoked exactly once, in the same place, and the envelope
//     records the value each ALREADY returned.
//   - It never re-reads a business source to fill a gap. A stage that did not
//     run has no value, and says so with an explicit state — never a zero, a
//     false or an empty string, all of which are legitimate results of a stage
//     that DID run.
//   - It retains no live model reference. Pointers, slices and the optional
//     filter condition are deep-copied under the lock that owns them, so a
//     later settings edit, outcome update or round replacement cannot reach
//     back and change a fact that was already recorded.
//
// The envelope is a value carried out of the locked region and attached to the
// ONE terminal fact of the attempt. The pool lock is never held across an
// emission.

import "github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"

// Stage states. A stage's result is only meaningful once its state says the
// stage ran: 0, false and "" are all values a stage that ran can legitimately
// produce, so absence needs its own vocabulary rather than a zero value.
const (
	// ObsStageExecuted — the business path ran this stage and the envelope
	// carries what it returned.
	ObsStageExecuted = "EXECUTED"
	// ObsStageNotReached — the attempt ended before this stage. Its inputs
	// and results do not exist, and no value may be inferred for them.
	ObsStageNotReached = "NOT_REACHED"
)

// Health-gate states. The gate is skipped for two structurally different
// reasons that a bare boolean would merge, and an evaluated gate has two
// outcomes; all four are distinct facts.
const (
	// ObsHealthNotReached — the attempt ended before the gate block.
	ObsHealthNotReached = "NOT_REACHED"
	// ObsHealthDisabled — the gate block ran with HealthGateEnabled false, so
	// AutoBetDecision was never called.
	ObsHealthDisabled = "DISABLED"
	// ObsHealthNoGate — the gate was enabled but no gate was injected, so
	// AutoBetDecision was never called. Distinct from DISABLED: the operator
	// asked for the gate and the process could not supply one.
	ObsHealthNoGate = "NO_GATE"
	// ObsHealthAllowed / ObsHealthDenied — AutoBetDecision WAS called and
	// returned this verdict.
	ObsHealthAllowed = "ALLOWED"
	ObsHealthDenied  = "DENIED"
)

// obsCounterAutoAttemptID is the counter key carrying the auto-attempt
// discriminator. It is the automatic counterpart of manualActionId, and it
// appears on every fact of an attempt including its placement calls.
const obsCounterAutoAttemptID = "autoAttemptId"

// maxEnvelopeOutcomes mirrors maxObservedOutcomes: the store's frozen ceiling
// on how many outcomes one fact may describe. The envelope projects ONE past
// it for the same reason projectOutcomes does — an over-long round must be
// visible to the store as over-long, so the store can drop a fact that would
// otherwise look complete while silently describing only the first 64.
const maxEnvelopeOutcomes = maxObservedOutcomes

// ObservationBetSettings is the effective per-round bet settings the business
// path actually consumed. Every field of models.BetSettings is present: a
// replay that is missing one of them cannot reproduce the decision, and a
// field omitted "because the strategy in force did not read it" would make the
// envelope's completeness depend on the very value being replayed.
//
// The projection is plain types rather than the domain types themselves, so a
// stored fact cannot alias a live model value and the store's closed
// vocabulary cannot drift into the domain's. The conversion is captureBetSettings
// below.
type ObservationBetSettings struct {
	Strategy      string
	Percentage    int
	PercentageGap int
	MaxPoints     int
	MinimumPoints int
	StealthMode   bool
	Delay         float64
	DelayMode     string

	// FilterCondition is a DEEP COPY of the optional filter, or nil when the
	// round genuinely had none. Nil is a real value here, not an absence:
	// Skip returns "do not skip" immediately when there is no condition.
	FilterCondition *ObservationFilterCondition
}

// ObservationFilterCondition is the deep-copied filter the round carried.
type ObservationFilterCondition struct {
	By    string
	Where string
	Value float64
}

// ObservationModelOutcome is one outcome of the MODEL state the decision read,
// in the order the model held it. This is deliberately NOT the wire projection
// in ObservationOutcome: that one describes a frame as it arrived, this one
// describes what Calculate consulted, including the derived values the model
// had accumulated over every earlier frame.
//
// TopPoints is the value the model had already computed: the LARGEST single
// stake among the round's top predictors. It is one viewer's wager amount, not
// a pool aggregate — the strategy selects on it and stealth mode reduces below
// it, so a replay cannot reproduce either without it. It carries no identity,
// and recording it authorizes reading or retaining nothing else about a
// predictor: the wire projection still keeps only a COUNT of them.
type ObservationModelOutcome struct {
	Slot int
	// Present distinguishes an outcome the model actually held from a hole in
	// its vector. A hole is structurally impossible today, but recording one
	// as a zero-point outcome would be a fabricated input rather than a
	// missing one — and every numeric field below has a legitimate zero.
	//
	// This is MODEL state, not a wire-presence claim: it says what the decision
	// read, not what a frame carried.
	Present bool
	// ID is the outcome identifier the placement call uses. It is a bounded
	// opaque identifier needed to link a choice to a placement, never display
	// text: the outcome's title is deliberately not projected.
	ID string

	TotalUsers  int64
	TotalPoints int64
	TopPoints   int64

	// The derived values, exactly as the model held them. They are recorded
	// rather than recomputed because UpdateOutcomes only refreshes them under
	// its own conditions, so the model can legitimately be carrying values
	// derived from an earlier frame than the last one observed.
	PercentageUsers float64
	Odds            float64
	OddsPercentage  float64
}

// ObservationDecision is the envelope for ONE automatic decision attempt.
//
// Every stage of the existing business path contributes the value it already
// produced, at the point it produced it. Nothing here is recomputed, and no
// field is derived from another purely to make the record look complete: where
// a value is provably the same variable at two points with no mutation between
// them, it is recorded once.
type ObservationDecision struct {
	// AttemptID is the observer-owned discriminator for this attempt. It
	// appears on every fact of the attempt — including its placement calls —
	// so facts are linked by an identity the observer minted, never by a
	// timestamp or an event id that a replaced round could reuse.
	AttemptID uint64

	// SettingsStage / Settings — the effective settings Calculate and Skip
	// consumed, read from the value the business path already held. This is
	// the DECISION-time snapshot; the admission-time snapshot is a separate
	// fact on the schedule decision, and the two are deliberately not merged
	// or asserted equal.
	SettingsStage string
	Settings      *ObservationBetSettings

	// CalculateStage — whether Calculate ran at all. An attempt that exits on
	// eligibility or round state never reaches it, and then every field below
	// that describes an input or a result of Calculate is absent.
	CalculateStage string

	// The inputs Calculate read, captured under the lock that owns them,
	// immediately before the call.
	Balance        *int64
	Outcomes       []ObservationModelOutcome
	BetTotalUsers  *int64
	BetTotalPoints *int64

	// The ORIGINAL return of Calculate, captured before any caller mutation.
	// The caller overwrites decision.Amount when the percent gate clamps it,
	// so reading the final stake back would lose what the strategy actually
	// proposed — which is the whole quantity a replay has to match.
	ChoiceIndex     *int
	ChoiceOutcomeID string
	ChoiceAmount    *int64

	// Skip's two results from its ONE existing call. SkipResult false is a
	// real answer ("the filter did not reject"), which P1 could not express
	// at all: it only ever recorded the rejection.
	SkipStage    string
	SkipResult   *bool
	SkipCompared *float64

	// The health gate. HealthReason is the closed models.GateReason the gate
	// returned, and is only meaningful when the state says the gate was
	// evaluated.
	HealthStage  string
	HealthReason string

	// EvaluateStake's inputs and its complete return. StakeAllowed is what the
	// function RETURNED; whether the caller adopted it is ClampApplied below,
	// because the caller assigns it only in the percent-gate branch.
	StakeStage          string
	RiskMaxStakePercent *int
	RiskReservePoints   *int
	StakeAllowed        *int64
	StakeReason         string
	StakeLimit          *int64

	// ClampApplied records the caller's ASSIGNMENT, observed at the assignment
	// itself rather than inferred from StakeReason. FinalAmount is the stake
	// the caller carries out of the gate block — equal to the original
	// proposal unless the clamp was applied, and the value the minimum-stake
	// and filter exits are then judged against.
	ClampApplied *bool
	FinalAmount  *int64
}

// newAutoAttemptID mints the discriminator for one auto-decision attempt. It
// is observer-owned state on the pool, exactly like the manual correlation
// token, so no business state owner gains a field for observability's sake.
func (p *WebSocketPool) newAutoAttemptID() uint64 { return p.autoAttempts.Add(1) }

// newDecisionEnvelope starts an envelope for one attempt with every stage
// explicitly unreached. Stages are promoted as the business path passes them,
// so a field left alone is honestly reported as never produced.
func newDecisionEnvelope(attemptID uint64) *ObservationDecision {
	return &ObservationDecision{
		AttemptID:      attemptID,
		SettingsStage:  ObsStageNotReached,
		CalculateStage: ObsStageNotReached,
		SkipStage:      ObsStageNotReached,
		HealthStage:    ObsHealthNotReached,
		StakeStage:     ObsStageNotReached,
	}
}

// captureBetSettings deep-copies the settings the decision consumed.
//
// The caller passes the value the business path already read; nothing here
// consults a streamer, a config or the settings service. The optional filter
// condition is a POINTER the round keeps sharing, so its pointee is copied
// rather than aliased: a later settings replacement must not be able to reach
// a fact that was already recorded. A nil filter stays nil, because "this
// round had no filter" is a real value that Skip acts on.
func captureBetSettings(s models.BetSettings) *ObservationBetSettings {
	out := &ObservationBetSettings{
		Strategy:      string(s.Strategy),
		Percentage:    s.Percentage,
		PercentageGap: s.PercentageGap,
		MaxPoints:     s.MaxPoints,
		MinimumPoints: s.MinimumPoints,
		StealthMode:   s.StealthMode,
		Delay:         s.Delay,
		DelayMode:     string(s.DelayMode),
	}
	if s.FilterCondition != nil {
		out.FilterCondition = &ObservationFilterCondition{
			By:    string(s.FilterCondition.By),
			Where: string(s.FilterCondition.Where),
			Value: s.FilterCondition.Value,
		}
	}
	return out
}

// captureModelOutcomes deep-copies the model's outcome vector, in order, into
// plain values. No model pointer survives the call, so a later UpdateOutcomes
// or round replacement cannot alter what was recorded.
//
// Like projectOutcomes it projects ONE past the store's ceiling: the extra
// entry is not data the store keeps, it is what lets the store SEE that the
// round exceeded the bound and reject a fact that would otherwise look
// complete while describing only a prefix of the outcomes.
//
// Must be called under the lock that owns the outcomes (p.mu).
func captureModelOutcomes(src []*models.Outcome) []ObservationModelOutcome {
	n := len(src)
	if n == 0 {
		return nil
	}
	if n > maxEnvelopeOutcomes+1 {
		n = maxEnvelopeOutcomes + 1
	}
	out := make([]ObservationModelOutcome, 0, n)
	for i := 0; i < n; i++ {
		o := src[i]
		if o == nil {
			// A nil slot is a real hole in the model's vector, not an outcome
			// with zero points. It keeps its index and says nothing else.
			out = append(out, ObservationModelOutcome{Slot: i})
			continue
		}
		out = append(out, ObservationModelOutcome{
			Slot:            i,
			Present:         true,
			ID:              o.ID,
			TotalUsers:      int64(o.TotalUsers),
			TotalPoints:     int64(o.TotalPoints),
			TopPoints:       int64(o.TopPoints),
			PercentageUsers: o.PercentageUsers,
			Odds:            o.Odds,
			OddsPercentage:  o.OddsPercentage,
		})
	}
	return out
}

// Small typed constructors, alongside the package's existing boolPtr/intPtr.
// They exist so a call site never writes a bare &value for a field whose
// absence is meaningful.
func int64Ptr(v int64) *int64     { return &v }
func floatPtr(v float64) *float64 { return &v }
