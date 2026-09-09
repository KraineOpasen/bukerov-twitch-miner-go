package predictioneval

// Pure mirrors of the persisted observation facts.
//
// These types deliberately RE-DECLARE what internal/analytics already
// declares. That is not accidental duplication: a Go package is atomic, so
// importing the store's types would drag database/sql, the SQLite driver and
// the whole write path into every replay, and the purity fence this package
// exists to hold would be gone on the first import line.
//
// The cost of the mirror is drift, and it is paid for explicitly: the reader
// package's TestSourceRecordMirrorsEveryStoredEnvelopeField walks the store's
// envelope with reflection and fails if the store grows a field this mirror
// does not carry. A silent field loss is exactly the failure a replay must
// not have, so it is a compile-and-test-time error rather than a runtime one.
//
// Everything here is a VALUE. Nothing aliases live model state, nothing is a
// pointer into the store, and no method mutates a receiver.

// SourceDataset is the bounded, coherent slice of persisted facts a reader
// acquired, together with the session classification the store applied to it.
//
// It is the ONLY input to [MaterializePairedKnowledge]. Handing this package a
// dataset is how the database stays on the other side of the fence.
type SourceDataset struct {
	// Source is the session provenance the store reported, carried verbatim.
	Source SourceProvenance `json:"source"`
	// Records are the session's facts in the causal order the store returned
	// them (ascending collector sequence). Order is load-bearing: it is what
	// makes an attempt's prefix a prefix.
	Records []SourceRecord `json:"records"`
}

// SourceRecord is one persisted fact, projected as a pure value.
type SourceRecord struct {
	ObservationID      string `json:"observationId"`
	CollectorSessionID string `json:"collectorSessionId"`
	CollectorEpoch     int64  `json:"collectorEpoch"`
	CollectorSequence  int64  `json:"collectorSequence"`

	PoolInstanceID     string `json:"poolInstanceId"`
	RoundIncarnationID string `json:"roundIncarnationId"`
	// RoundCaptureOrigin / RoundCaptureGapCause are the round's frozen
	// admission provenance. A round admitted while capture was incomplete is
	// not a round with a short history — it is a round whose history this
	// build cannot claim to know — so both travel with every case and are
	// reported rather than ignored.
	RoundCaptureOrigin   string `json:"roundCaptureOrigin"`
	RoundCaptureGapCause string `json:"roundCaptureGapCause"`

	EventID string `json:"eventId"`
	Kind    string `json:"kind"`

	PayloadVersion int64 `json:"payloadVersion"`
	// PayloadUndecodable marks a row whose stored payload could not be
	// decoded. Its columns are intact and its digest still witnesses them, but
	// its payload must not be read — and this model never treats such a row as
	// carrying an absent value.
	PayloadUndecodable bool   `json:"payloadUndecodable"`
	ObservationSHA256  string `json:"observationSha256"`

	Payload SourcePayload `json:"payload"`
}

// SourcePayload is the closed typed projection of one fact.
type SourcePayload struct {
	Phase       string           `json:"phase"`
	RoundState  string           `json:"roundState,omitempty"`
	Decision    string           `json:"decision,omitempty"`
	ReasonCode  string           `json:"reasonCode,omitempty"`
	ErrorClass  string           `json:"errorClass,omitempty"`
	Manual      *bool            `json:"manual,omitempty"`
	OutcomeSlot *int             `json:"outcomeSlot,omitempty"`
	Counters    map[string]int64 `json:"counters,omitempty"`

	// DecisionEnvelope is the inputs and original results of ONE automatic
	// decision attempt. Nil on every other kind of fact and on every fact
	// written under the pre-envelope contract.
	DecisionEnvelope *SourceDecisionEnvelope `json:"decisionEnvelope,omitempty"`
	// AdmissionSettings is the settings the round was ADMITTED with. It is a
	// separate fact from the decision-time snapshot and the two are never
	// merged: a reader may establish that they agree, and is never told to
	// assume it.
	AdmissionSettings *SourceBetSettings `json:"admissionSettings,omitempty"`
}

// SourceBetSettings is the effective bet settings a decision consumed.
type SourceBetSettings struct {
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

// SourceFilterCondition is the optional filter the round carried. Nil is a
// value, not an absence: the pinned policy returns "do not skip" immediately
// when there is no condition, so nil changes what a replay computes.
type SourceFilterCondition struct {
	By    string  `json:"by"`
	Where string  `json:"where"`
	Value float64 `json:"value"`
}

// SourceModelOutcome is one entry of the MODEL outcome vector a decision read.
//
// It is not the wire projection. The wire projection describes a frame as it
// arrived; this one describes what the strategy actually consulted, including
// derived values the model had accumulated over earlier frames. Recomputing
// these from the newest wire totals would be wrong, because the model
// refreshes them only under its own conditions.
type SourceModelOutcome struct {
	Slot int `json:"slot"`
	// Present separates an outcome the model held from a HOLE in its vector.
	// Every numeric field below has a legitimate zero, so a hole read as a
	// zero-point outcome would be a fabricated input.
	Present bool   `json:"present"`
	ID      string `json:"id"`

	TotalUsers  int64 `json:"totalUsers"`
	TotalPoints int64 `json:"totalPoints"`
	// TopPoints is the LARGEST SINGLE stake among the round's top predictors —
	// one viewer's wager, not a pool aggregate. The strategy selects on it and
	// stealth mode reduces below it, so a decision that read it cannot be
	// replayed without it. It carries no identity and authorizes reading
	// nothing else about any predictor.
	TopPoints int64 `json:"topPoints"`

	PercentageUsers float64 `json:"percentageUsers"`
	Odds            float64 `json:"odds"`
	OddsPercentage  float64 `json:"oddsPercentage"`
}

// SourceDecisionEnvelope is the persisted projection of ONE automatic decision
// attempt: what it read, what each stage returned, and what the caller then
// did with those returns.
//
// It holds BOTH halves of the causal cut — inputs and results — because the
// producer persists them in one terminal envelope. Splitting them is
// [ProjectDecisionCase]'s job and nothing before it may blur the two.
type SourceDecisionEnvelope struct {
	AttemptID uint64 `json:"attemptId"`

	SettingsStage string             `json:"settingsStage"`
	Settings      *SourceBetSettings `json:"settings,omitempty"`

	CalculateStage string               `json:"calculateStage"`
	Balance        *int64               `json:"balance,omitempty"`
	Outcomes       []SourceModelOutcome `json:"outcomes,omitempty"`
	BetTotalUsers  *int64               `json:"betTotalUsers,omitempty"`
	BetTotalPoints *int64               `json:"betTotalPoints,omitempty"`

	// The ORIGINAL results of the strategy, before the caller's percent clamp
	// could overwrite the amount.
	//
	// ChoiceIndex nil while CalculateStage is EXECUTED is not missing data: the
	// store drops a NEGATIVE index because a negative index is the policy's own
	// "no outcome was chosen" answer. The pair therefore reconstructs the
	// policy's -1 exactly, and [ProjectDecisionCase] says so rather than
	// treating the nil as absence.
	ChoiceIndex     *int   `json:"choiceIndex,omitempty"`
	ChoiceOutcomeID string `json:"choiceOutcomeId,omitempty"`
	ChoiceAmount    *int64 `json:"choiceAmount,omitempty"`

	SkipStage    string   `json:"skipStage"`
	SkipResult   *bool    `json:"skipResult,omitempty"`
	SkipCompared *float64 `json:"skipCompared,omitempty"`

	HealthStage  string `json:"healthStage"`
	HealthReason string `json:"healthReason,omitempty"`

	StakeStage          string `json:"stakeStage"`
	RiskMaxStakePercent *int   `json:"riskMaxStakePercent,omitempty"`
	RiskReservePoints   *int   `json:"riskReservePoints,omitempty"`
	StakeAllowed        *int64 `json:"stakeAllowed,omitempty"`
	StakeReason         string `json:"stakeReason,omitempty"`
	StakeLimit          *int64 `json:"stakeLimit,omitempty"`

	// ClampApplied is the caller's ASSIGNMENT, observed at the assignment.
	// FinalAmount is the stake the caller carried OUT of the gate block and is
	// absent when the attempt returned from inside it.
	ClampApplied *bool  `json:"clampApplied,omitempty"`
	FinalAmount  *int64 `json:"finalAmount,omitempty"`
}

// Fact kinds and phases this model reads. They are re-declared for the same
// reason the types are, and pinned against the store by the reader's
// vocabulary test.
const (
	KindAutoDecision = "auto_decision"
	KindPlacement    = "placement"
	KindUserTerminal = "user_terminal"

	PhaseAutoDue     = "AUTO_DUE"
	PhaseAutoDecided = "AUTO_DECIDED"
	PhaseAutoSkipped = "AUTO_SKIPPED"

	PhaseCallStarted  = "CALL_STARTED"
	PhaseCallReturned = "CALL_RETURNED"

	PhaseTerminalDelivered = "TERMINAL_DELIVERED"
	PhaseTerminalAdmitted  = "TERMINAL_ADMITTED"

	// CounterAutoAttemptID is the key carrying the attempt discriminator. It
	// is what links an attempt's facts together — a minted identity, never a
	// timestamp and never an event id a later admission could reuse.
	CounterAutoAttemptID = "autoAttemptId"
	CounterStake         = "stake"
	CounterBalance       = "balance"
	CounterPayout        = "payout"
	CounterReturnedStake = "returnedStake"

	// StageExecuted / StageNotReached are the store's stage-state vocabulary.
	StageExecuted   = "EXECUTED"
	StageNotReached = "NOT_REACHED"

	HealthNotReached = "NOT_REACHED"
	HealthDisabled   = "DISABLED"
	HealthNoGate     = "NO_GATE"
	HealthAllowed    = "ALLOWED"
	HealthDenied     = "DENIED"

	// ValueUnknown is the store's marker for a value that fell outside its
	// closed vocabulary. It is never treated as absence, and never silently
	// replaced by a plausible member of the vocabulary.
	ValueUnknown = "UNKNOWN"
)
