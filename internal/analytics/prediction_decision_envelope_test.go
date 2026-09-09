package analytics

// P1.5 — the auto-decision ENVELOPE, from the producer's struct to the stored
// bytes and back.
//
// Everything here defends one property: an envelope is EVIDENCE, so a reader
// must never be able to confuse a value the decision actually used with a value
// this build could not project. The two ways that goes wrong are a zero
// masquerading as an absence (or the reverse) and a silently shortened record
// that still looks complete — so the tests below always assert BOTH the value
// and its distinguishability, and always assert that a ceiling breach loses the
// whole fact rather than part of it.

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

// envelopeAttemptID is the attempt discriminator every fixture below shares. It
// is held apart from the envelope's own AttemptID so a test that mutates the
// envelope moves exactly one thing: the counters map stays byte-identical, and
// a digest that changes can only have changed because of the envelope.
const envelopeAttemptID uint64 = 0xFEEDFACE

func envInt(v int) *int           { return &v }
func envInt64(v int64) *int64     { return &v }
func envBool(v bool) *bool        { return &v }
func envFloat(v float64) *float64 { return &v }

// fullBetSettings is a settings snapshot with every one of the nine fields set
// to a distinctive, non-zero, in-vocabulary value, so a field that is dropped,
// defaulted or swapped with its neighbour is visible rather than plausible.
func fullBetSettings() *ObservationBetSettings {
	return &ObservationBetSettings{
		Strategy:      "SMART",
		Percentage:    37,
		PercentageGap: 12,
		MaxPoints:     50_000,
		MinimumPoints: 250,
		StealthMode:   true,
		Delay:         6.5,
		DelayMode:     "FROM_END",
		FilterCondition: &ObservationFilterCondition{
			By:    "percentage_users",
			Where: "GT",
			Value: 62.5,
		},
	}
}

// fullDecisionEnvelope is an envelope in which every stage ran and every
// optional pointer is present. The model outcomes deliberately arrive with a
// WRONG Slot so the sanitizer's positional re-indexing is exercised rather than
// accidentally satisfied by the input.
func fullDecisionEnvelope() *ObservationDecisionEnvelope {
	return &ObservationDecisionEnvelope{
		AttemptID:     envelopeAttemptID,
		SettingsStage: DecisionStageExecuted,
		Settings:      fullBetSettings(),

		CalculateStage: DecisionStageExecuted,
		Balance:        envInt64(123_456),
		Outcomes: []ObservationModelOutcome{
			{
				Slot: 9, Present: true, ID: "outcome-blue",
				TotalUsers: 41, TotalPoints: 9_100, TopPoints: 7_500,
				PercentageUsers: 61.25, Odds: 1.6, OddsPercentage: 62.5,
			},
			{
				Slot: 9, Present: false, ID: "outcome-pink",
				TotalUsers: 12, TotalPoints: 3_400, TopPoints: 2_000,
				PercentageUsers: 38.75, Odds: 4.25, OddsPercentage: 23.5,
			},
		},
		BetTotalUsers:  envInt64(53),
		BetTotalPoints: envInt64(12_500),

		ChoiceIndex:     envInt(1),
		ChoiceOutcomeID: "outcome-pink",
		ChoiceAmount:    envInt64(4_500),

		SkipStage:    DecisionStageExecuted,
		SkipResult:   envBool(true),
		SkipCompared: envFloat(38.75),

		HealthStage:  DecisionHealthDenied,
		HealthReason: "health_pubsub_degraded",

		StakeStage:          DecisionStageExecuted,
		RiskMaxStakePercent: envInt(25),
		RiskReservePoints:   envInt(1_000),
		StakeAllowed:        envInt64(3_000),
		StakeReason:         "max_stake_percent",
		StakeLimit:          envInt64(3_000),

		ClampApplied: envBool(true),
		FinalAmount:  envInt64(3_000),
	}
}

// envelopeFact wraps an envelope in the auto_decision fact a producer would
// actually offer, so the envelope is exercised through the same path a real
// decision takes rather than through the sanitizer alone.
func envelopeFact(env *ObservationDecisionEnvelope, admission *ObservationBetSettings) PredictionObservation {
	obs := channelObservation("pool-1", "chan-a", "streamer-a", "event-1", "AUTO_DECIDED")
	obs.Kind = KindAutoDecision
	obs.Payload = ObservationPayload{
		Phase:      "AUTO_DECIDED",
		RoundState: "ACTIVE",
		Decision:   "PLACE",
		ReasonCode: "OK",
		// The attempt discriminator P1.5 added to the closed counter key set.
		// It is what links this fact to the placement calls of the same
		// attempt, so a fact that lost it is an unlinkable decision record.
		Counters:          map[string]int64{"autoAttemptId": int64(envelopeAttemptID)},
		DecisionEnvelope:  env,
		AdmissionSettings: admission,
	}
	return obs
}

// sanitizedEnvelope runs one envelope through the REAL payload sanitizer and
// fails the test if the fact was refused. Tests that are about a surviving
// value must never silently receive a refusal instead.
func sanitizedEnvelope(t *testing.T, env *ObservationDecisionEnvelope) *ObservationDecisionEnvelope {
	t.Helper()
	out, ok := sanitizeObservationPayload(envelopeFact(env, nil).Payload)
	if !ok {
		t.Fatal("a sanitizable envelope was refused; the value under test never reached the store")
	}
	return out.DecisionEnvelope
}

// renderEnvelopePayload sanitizes and renders a fact's payload exactly as the
// collector would, returning the bytes that would be stored and hashed.
func renderEnvelopePayload(t *testing.T, in ObservationPayload) string {
	t.Helper()
	out, ok := sanitizeObservationPayload(in)
	if !ok {
		t.Fatal("payload was refused by sanitization; nothing would be stored to inspect")
	}
	rendered, ok := marshalObservationPayload(out)
	if !ok {
		t.Fatal("sanitized payload could not be rendered; the fact would be dropped")
	}
	return rendered
}

// readOneEnvelopeFact offers exactly one fact through the real Service, waits
// for the collector to commit it, and reads it back out of SQLite.
func readOneEnvelopeFact(t *testing.T, svc *Service, repo *SQLiteRepository, fact PredictionObservation) ObservationRecord {
	t.Helper()
	svc.RecordPredictionObservation(fact)
	awaitCommitted(t, svc, 1)
	got, err := repo.ObservationsBySession(context.Background(), svc.observations.sessionID, 0)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read back %d facts, want 1: the decision record did not reach the store", len(got))
	}
	if got[0].PayloadUndecodable {
		t.Fatal("the stored payload could not be decoded: the decision record is present but unreadable, " +
			"which is the one state a reader cannot recover from")
	}
	return got[0]
}

// TestAFullyPopulatedDecisionEnvelopeSurvivesTheRealStore regresses the defect
// where an envelope field is added to the struct but lost somewhere between the
// producer and the reader — dropped by the sanitizer, omitted by the JSON tag,
// or truncated by the payload ceiling — leaving a decision record that looks
// complete and is not.
//
// A naive implementation passes a sanitizer-only test: sanitizeObservation
// returns the value it was handed, so the loss only appears after the value has
// been rendered to JSON, written to SQLite and decoded back. This test therefore
// goes through Service.RecordPredictionObservation and
// SQLiteRepository.ObservationsBySession rather than calling the sanitizer.
func TestAFullyPopulatedDecisionEnvelopeSurvivesTheRealStore(t *testing.T) {
	svc, repo := newObservationService(t)

	admission := fullBetSettings()
	// Deliberately DIFFERENT from the decision-time snapshot: the two are
	// separate facts and must never be merged or inferred from one another.
	admission.Strategy = "MOST_VOTED"
	admission.Percentage = 5
	admission.StealthMode = false
	admission.FilterCondition = &ObservationFilterCondition{By: "odds", Where: "LTE", Value: 2.25}

	fact := envelopeFact(fullDecisionEnvelope(), admission)
	rec := readOneEnvelopeFact(t, svc, repo, fact)

	env := rec.Payload.DecisionEnvelope
	if env == nil {
		t.Fatal("the decision envelope is gone after a store round trip: the fact records a choice " +
			"nobody can re-derive")
	}

	if env.AttemptID != envelopeAttemptID {
		t.Fatalf("attempt id came back %d, want %d: the attempt's other facts (its placement calls "+
			"included) can no longer be linked to this decision", env.AttemptID, envelopeAttemptID)
	}
	if got := rec.Payload.Counters["autoAttemptId"]; got != int64(envelopeAttemptID) {
		t.Fatalf("counters autoAttemptId = %d, want %d: the closed counter key P1.5 added did not "+
			"survive, so the attempt link exists on the envelope alone", got, int64(envelopeAttemptID))
	}

	// All nine settings fields, one at a time, so a failure names the field.
	want := fullBetSettings()
	got := env.Settings
	if got == nil {
		t.Fatal("the decision-time settings snapshot is gone: a replay has no inputs to replay from")
	}
	for name, pair := range map[string][2]interface{}{
		"strategy":      {got.Strategy, want.Strategy},
		"percentage":    {got.Percentage, want.Percentage},
		"percentageGap": {got.PercentageGap, want.PercentageGap},
		"maxPoints":     {got.MaxPoints, want.MaxPoints},
		"minimumPoints": {got.MinimumPoints, want.MinimumPoints},
		"stealthMode":   {got.StealthMode, want.StealthMode},
		"delay":         {got.Delay, want.Delay},
		"delayMode":     {got.DelayMode, want.DelayMode},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("settings %s came back %v, want %v: the snapshot is not the one the decision "+
				"consumed, so a replay would compute a different bet", name, pair[0], pair[1])
		}
	}
	if got.FilterCondition == nil {
		t.Fatal("the filter condition is gone: 'the round had no filter' and 'the round had one' " +
			"now read identically")
	}
	if *got.FilterCondition != *want.FilterCondition {
		t.Fatalf("filter condition = %+v, want %+v: the recorded skip cannot be re-derived",
			*got.FilterCondition, *want.FilterCondition)
	}

	// The admission snapshot is its own fact and must not have been merged
	// with, or overwritten by, the decision-time one.
	adm := rec.Payload.AdmissionSettings
	if adm == nil {
		t.Fatal("the admission settings snapshot is gone: a reader can no longer establish whether " +
			"the round was decided under the settings it was admitted with")
	}
	if adm.Strategy != "MOST_VOTED" || adm.Percentage != 5 || adm.StealthMode {
		t.Fatalf("admission settings = %+v, want the independently recorded admission-time values; "+
			"the two snapshots have been merged", *adm)
	}
	if adm.FilterCondition == nil || adm.FilterCondition.By != "odds" || adm.FilterCondition.Where != "LTE" {
		t.Fatalf("admission filter = %+v, want the admission-time condition", adm.FilterCondition)
	}

	// The ordered outcome vector, including the derived values the model had
	// already accumulated. Slots are POSITIONAL: the fixture supplied 9 for
	// both, so a stored 9 proves the sanitizer trusted the producer's index.
	wantOutcomes := []ObservationModelOutcome{
		{Slot: 0, Present: true, ID: "outcome-blue", TotalUsers: 41, TotalPoints: 9_100,
			TopPoints: 7_500, PercentageUsers: 61.25, Odds: 1.6, OddsPercentage: 62.5},
		{Slot: 1, Present: false, ID: "outcome-pink", TotalUsers: 12, TotalPoints: 3_400,
			TopPoints: 2_000, PercentageUsers: 38.75, Odds: 4.25, OddsPercentage: 23.5},
	}
	if len(env.Outcomes) != len(wantOutcomes) {
		t.Fatalf("read back %d model outcomes, want %d: the vector the strategy read is not the "+
			"vector stored", len(env.Outcomes), len(wantOutcomes))
	}
	for i, w := range wantOutcomes {
		if env.Outcomes[i] != w {
			t.Fatalf("model outcome %d = %+v, want %+v: the strategy's inputs cannot be replayed",
				i, env.Outcomes[i], w)
		}
	}

	// Every stage state, so an unexecuted stage can never be read as a
	// zero-valued result.
	for name, pair := range map[string][2]string{
		"settingsStage":   {env.SettingsStage, DecisionStageExecuted},
		"calculateStage":  {env.CalculateStage, DecisionStageExecuted},
		"skipStage":       {env.SkipStage, DecisionStageExecuted},
		"stakeStage":      {env.StakeStage, DecisionStageExecuted},
		"healthStage":     {env.HealthStage, DecisionHealthDenied},
		"healthReason":    {env.HealthReason, "health_pubsub_degraded"},
		"stakeReason":     {env.StakeReason, "max_stake_percent"},
		"choiceOutcomeId": {env.ChoiceOutcomeID, "outcome-pink"},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("%s came back %q, want %q: the reader can no longer tell which stages ran",
				name, pair[0], pair[1])
		}
	}

	// Every optional pointer.
	for name, pair := range map[string][2]interface{}{
		"balance":             {env.Balance, int64(123_456)},
		"betTotalUsers":       {env.BetTotalUsers, int64(53)},
		"betTotalPoints":      {env.BetTotalPoints, int64(12_500)},
		"choiceAmount":        {env.ChoiceAmount, int64(4_500)},
		"stakeAllowed":        {env.StakeAllowed, int64(3_000)},
		"stakeLimit":          {env.StakeLimit, int64(3_000)},
		"finalAmount":         {env.FinalAmount, int64(3_000)},
		"riskMaxStakePercent": {env.RiskMaxStakePercent, 25},
		"riskReservePoints":   {env.RiskReservePoints, 1_000},
		"choiceIndex":         {env.ChoiceIndex, 1},
		"skipResult":          {env.SkipResult, true},
		"skipCompared":        {env.SkipCompared, 38.75},
		"clampApplied":        {env.ClampApplied, true},
	} {
		if reflect.ValueOf(pair[0]).IsNil() {
			t.Fatalf("%s came back nil, want %v: a value the decision produced now reads as a "+
				"stage that produced nothing", name, pair[1])
		}
		if deref := reflect.ValueOf(pair[0]).Elem().Interface(); deref != pair[1] {
			t.Fatalf("%s came back %v, want %v", name, deref, pair[1])
		}
	}

	// Backstop: nothing else in the payload drifted either. A field added to
	// the envelope in future is covered by this even if nobody adds it above.
	wantPayload, ok := sanitizeObservationPayload(fact.Payload)
	if !ok {
		t.Fatal("the fixture fact does not sanitize; the expectation itself is unusable")
	}
	if !reflect.DeepEqual(rec.Payload, wantPayload) {
		t.Fatalf("the stored payload is not the sanitized projection that was offered\n got: %+v\nwant: %+v",
			rec.Payload, wantPayload)
	}
}

// TestALegitimateZeroInTheEnvelopeIsNotAnAbsence regresses the exact defect an
// `omitempty` on a value field would introduce: a real 0 percentage, a real
// zero balance, a real `false` stealth mode or a real "do not skip" answer would
// be omitted from the JSON and read back as "the decision never produced one".
//
// A naive implementation passes every test that only checks non-zero values —
// which is most of them — because the loss is invisible until the value happens
// to be a zero. Here every projected number is 0, every boolean is false and the
// filter is genuinely nil, and each one is asserted to come back as ITSELF: a
// non-nil pointer to zero, not a nil pointer.
func TestALegitimateZeroInTheEnvelopeIsNotAnAbsence(t *testing.T) {
	svc, repo := newObservationService(t)

	env := &ObservationDecisionEnvelope{
		AttemptID:     envelopeAttemptID,
		SettingsStage: DecisionStageExecuted,
		Settings: &ObservationBetSettings{
			Strategy: "PERCENTAGE", Percentage: 0, PercentageGap: 0,
			MaxPoints: 0, MinimumPoints: 0, StealthMode: false,
			Delay: 0, DelayMode: "FROM_START",
			// A round that genuinely carried no filter. Skip returns "do not
			// skip" immediately in that case, so nil changes what a replay
			// computes and must survive AS nil.
			FilterCondition: nil,
		},
		CalculateStage:  DecisionStageExecuted,
		Balance:         envInt64(0),
		BetTotalUsers:   envInt64(0),
		BetTotalPoints:  envInt64(0),
		ChoiceIndex:     envInt(0),
		ChoiceOutcomeID: "outcome-blue",
		ChoiceAmount:    envInt64(0),
		SkipStage:       DecisionStageExecuted,
		SkipResult:      envBool(false),
		SkipCompared:    envFloat(0),
		// NO_GATE is a real verdict, and GateNone ("") is a real reason.
		HealthStage:         DecisionHealthNoGate,
		HealthReason:        "",
		StakeStage:          DecisionStageExecuted,
		RiskMaxStakePercent: envInt(0),
		RiskReservePoints:   envInt(0),
		StakeAllowed:        envInt64(0),
		StakeReason:         "",
		StakeLimit:          envInt64(0),
		ClampApplied:        envBool(false),
		FinalAmount:         envInt64(0),
	}

	admission := &ObservationBetSettings{Strategy: "PERCENTAGE", DelayMode: "FROM_START"}
	rec := readOneEnvelopeFact(t, svc, repo, envelopeFact(env, admission))

	got := rec.Payload.DecisionEnvelope
	if got == nil {
		t.Fatal("an all-zero envelope came back nil: a decision computed from zeros is now " +
			"indistinguishable from a decision that was never recorded")
	}
	if got.Settings == nil {
		t.Fatal("an all-zero settings snapshot came back nil")
	}
	for name, pair := range map[string][2]interface{}{
		"percentage":    {got.Settings.Percentage, 0},
		"percentageGap": {got.Settings.PercentageGap, 0},
		"maxPoints":     {got.Settings.MaxPoints, 0},
		"minimumPoints": {got.Settings.MinimumPoints, 0},
		"stealthMode":   {got.Settings.StealthMode, false},
		"delay":         {got.Settings.Delay, float64(0)},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("settings %s came back %v, want the legitimate zero %v: a real value has been "+
				"encoded as a missing key", name, pair[0], pair[1])
		}
	}
	if got.Settings.FilterCondition != nil {
		t.Fatalf("a round with NO filter came back carrying %+v: a replay would evaluate a "+
			"condition that never existed", *got.Settings.FilterCondition)
	}

	for name, ptr := range map[string]interface{}{
		"balance":             got.Balance,
		"betTotalUsers":       got.BetTotalUsers,
		"betTotalPoints":      got.BetTotalPoints,
		"choiceIndex":         got.ChoiceIndex,
		"choiceAmount":        got.ChoiceAmount,
		"skipResult":          got.SkipResult,
		"skipCompared":        got.SkipCompared,
		"riskMaxStakePercent": got.RiskMaxStakePercent,
		"riskReservePoints":   got.RiskReservePoints,
		"stakeAllowed":        got.StakeAllowed,
		"stakeLimit":          got.StakeLimit,
		"clampApplied":        got.ClampApplied,
		"finalAmount":         got.FinalAmount,
	} {
		v := reflect.ValueOf(ptr)
		if v.IsNil() {
			t.Fatalf("%s came back nil, but the stage RAN and returned a zero: an executed stage "+
				"now reads as one that never happened", name)
		}
		if !v.Elem().IsZero() {
			t.Fatalf("%s came back %v, want the zero the stage returned", name, v.Elem().Interface())
		}
	}
	if got.StakeReason != "" {
		t.Fatalf("the stake gate reason came back %q, want the empty GateNone: 'no gate bound this "+
			"stake' is a real answer and must not be promoted to a named gate", got.StakeReason)
	}
	if got.HealthReason != "" {
		t.Fatalf("the health gate reason came back %q, want the empty GateNone", got.HealthReason)
	}
	if got.ChoiceOutcomeID != "outcome-blue" {
		t.Fatalf("choice outcome id = %q; slot 0 is a real choice, not an absent one", got.ChoiceOutcomeID)
	}

	// The rendered bytes say the same thing: the zeros are present as keys.
	rendered := renderEnvelopePayload(t, envelopeFact(env, admission).Payload)
	for _, key := range []string{
		`"percentage":0`, `"stealthMode":false`, `"delay":0`,
		`"balance":0`, `"choiceIndex":0`, `"choiceAmount":0`,
		`"skipResult":false`, `"clampApplied":false`, `"finalAmount":0`,
	} {
		if !strings.Contains(rendered, key) {
			t.Fatalf("the stored payload omits %s; a real zero is being written as an absence\n%s",
				key, rendered)
		}
	}
	if strings.Contains(rendered, `"filterCondition"`) {
		t.Fatalf("a nil filter condition rendered a key anyway: %s", rendered)
	}
}

// TestAnUnexecutedStageIsDistinguishableFromOneThatReturnedZero regresses the
// defect the stage-state vocabulary exists to prevent: encoding "this stage was
// never reached" as the stage's zero result.
//
// A naive implementation stores only the values and lets nil mean both things.
// It passes any test that checks one case in isolation, because each case
// individually looks right. The property only fails when the two are compared,
// so this test asserts that the NOT_REACHED envelope and the executed-and-zero
// envelope of the test above render differently AND read back differently.
func TestAnUnexecutedStageIsDistinguishableFromOneThatReturnedZero(t *testing.T) {
	svc, repo := newObservationService(t)

	notReached := &ObservationDecisionEnvelope{
		AttemptID:     envelopeAttemptID,
		SettingsStage: DecisionStageExecuted,
		Settings:      &ObservationBetSettings{Strategy: "SMART", DelayMode: "FROM_START"},
		// Calculate was never reached, so nothing downstream of it exists.
		CalculateStage:  DecisionStageNotReached,
		Balance:         nil,
		Outcomes:        nil,
		BetTotalUsers:   nil,
		BetTotalPoints:  nil,
		ChoiceIndex:     nil,
		ChoiceOutcomeID: "",
		ChoiceAmount:    nil,
		SkipStage:       DecisionStageNotReached,
		SkipResult:      nil,
		SkipCompared:    nil,
		HealthStage:     DecisionHealthNotReached,
		StakeStage:      DecisionStageNotReached,
	}

	rec := readOneEnvelopeFact(t, svc, repo, envelopeFact(notReached, nil))
	got := rec.Payload.DecisionEnvelope
	if got == nil {
		t.Fatal("an envelope describing an abandoned decision path came back nil: the fact no longer " +
			"says how far the decision got")
	}
	if got.CalculateStage != DecisionStageNotReached {
		t.Fatalf("calculateStage = %q, want %q: a stage that never ran is being reported as one "+
			"that did", got.CalculateStage, DecisionStageNotReached)
	}
	for name, ptr := range map[string]interface{}{
		"balance":        got.Balance,
		"betTotalUsers":  got.BetTotalUsers,
		"betTotalPoints": got.BetTotalPoints,
		"choiceIndex":    got.ChoiceIndex,
		"choiceAmount":   got.ChoiceAmount,
		"skipResult":     got.SkipResult,
		"skipCompared":   got.SkipCompared,
	} {
		if !reflect.ValueOf(ptr).IsNil() {
			t.Fatalf("%s came back %v for a stage that never ran: a value has been invented for a "+
				"computation that never happened", name, reflect.ValueOf(ptr).Elem().Interface())
		}
	}
	if got.Outcomes != nil {
		t.Fatalf("the model outcome vector came back %+v for a stage that never read one", got.Outcomes)
	}
	if got.ChoiceOutcomeID != "" {
		t.Fatalf("choiceOutcomeId = %q for a decision that never chose", got.ChoiceOutcomeID)
	}

	// And the two cases are genuinely different on the wire. An executed stage
	// that returned zero writes the key; an unexecuted one does not.
	executed := &ObservationDecisionEnvelope{
		AttemptID:      envelopeAttemptID,
		SettingsStage:  DecisionStageExecuted,
		Settings:       &ObservationBetSettings{Strategy: "SMART", DelayMode: "FROM_START"},
		CalculateStage: DecisionStageExecuted,
		Balance:        envInt64(0),
		ChoiceIndex:    envInt(0),
		ChoiceAmount:   envInt64(0),
		SkipStage:      DecisionStageExecuted,
		SkipResult:     envBool(false),
		SkipCompared:   envFloat(0),
		HealthStage:    DecisionHealthNoGate,
		StakeStage:     DecisionStageExecuted,
	}
	notReachedJSON := renderEnvelopePayload(t, envelopeFact(notReached, nil).Payload)
	executedJSON := renderEnvelopePayload(t, envelopeFact(executed, nil).Payload)
	if notReachedJSON == executedJSON {
		t.Fatalf("a stage that never ran renders the same bytes as one that ran and returned zero; "+
			"the two are permanently indistinguishable to every reader:\n%s", notReachedJSON)
	}
	for _, key := range []string{`"balance"`, `"choiceIndex"`, `"skipResult"`, `"skipCompared"`} {
		if strings.Contains(notReachedJSON, key) {
			t.Fatalf("the unexecuted-stage payload carries %s: %s", key, notReachedJSON)
		}
		if !strings.Contains(executedJSON, key) {
			t.Fatalf("the executed-stage payload omits %s: %s", key, executedJSON)
		}
	}
}

// TestEveryEnvelopeVocabularyIsClosedAndRefusesRawText regresses the silent
// widening `closedValue` makes possible: an unrecognized member DEGRADES to
// UNKNOWN instead of failing, so a vocabulary that quietly starts accepting raw
// producer text still commits, still finalizes COMPLETE, and nothing in the
// suite notices.
//
// A naive implementation — storing the string it was handed — passes every
// round-trip test in this file, because a verbatim string is exactly what a
// round trip expects. The property only fails when the input is NOT a member,
// so each case here feeds a secret-bearing non-member and proves it appears
// nowhere in the stored bytes or the digest.
func TestEveryEnvelopeVocabularyIsClosedAndRefusesRawText(t *testing.T) {
	const secret = "oauth:abcdef0123456789-SECRET-TOKEN"

	skeleton := func() *ObservationDecisionEnvelope {
		return &ObservationDecisionEnvelope{
			AttemptID:     envelopeAttemptID,
			SettingsStage: DecisionStageExecuted,
			Settings: &ObservationBetSettings{
				Strategy: "SMART", DelayMode: "FROM_START",
				FilterCondition: &ObservationFilterCondition{By: "odds", Where: "GT", Value: 1.5},
			},
			CalculateStage: DecisionStageExecuted,
			SkipStage:      DecisionStageExecuted,
			HealthStage:    DecisionHealthAllowed,
			StakeStage:     DecisionStageExecuted,
		}
	}

	for _, tc := range []struct {
		name string
		// valid is an in-vocabulary member that must survive verbatim.
		valid string
		// emptyWant is what an EMPTY input becomes. The stage and enum fields
		// use closedValue, so "" is not a member and becomes UNKNOWN. The two
		// gate reasons use closedOptional, where "" is models.GateNone — a real
		// value meaning "no gate bound this" — and must stay "".
		emptyWant string
		set       func(*ObservationDecisionEnvelope, string)
		get       func(*ObservationDecisionEnvelope) string
	}{
		{
			name: "settings stage", valid: DecisionStageNotReached, emptyWant: ValueUnknown,
			set: func(e *ObservationDecisionEnvelope, v string) { e.SettingsStage = v },
			get: func(e *ObservationDecisionEnvelope) string { return e.SettingsStage },
		},
		{
			name: "calculate stage", valid: DecisionStageExecuted, emptyWant: ValueUnknown,
			set: func(e *ObservationDecisionEnvelope, v string) { e.CalculateStage = v },
			get: func(e *ObservationDecisionEnvelope) string { return e.CalculateStage },
		},
		{
			name: "skip stage", valid: DecisionStageNotReached, emptyWant: ValueUnknown,
			set: func(e *ObservationDecisionEnvelope, v string) { e.SkipStage = v },
			get: func(e *ObservationDecisionEnvelope) string { return e.SkipStage },
		},
		{
			name: "stake stage", valid: DecisionStageExecuted, emptyWant: ValueUnknown,
			set: func(e *ObservationDecisionEnvelope, v string) { e.StakeStage = v },
			get: func(e *ObservationDecisionEnvelope) string { return e.StakeStage },
		},
		{
			name: "health stage", valid: DecisionHealthDisabled, emptyWant: ValueUnknown,
			set: func(e *ObservationDecisionEnvelope, v string) { e.HealthStage = v },
			get: func(e *ObservationDecisionEnvelope) string { return e.HealthStage },
		},
		{
			name: "bet strategy", valid: "NUMBER_8", emptyWant: ValueUnknown,
			set: func(e *ObservationDecisionEnvelope, v string) { e.Settings.Strategy = v },
			get: func(e *ObservationDecisionEnvelope) string { return e.Settings.Strategy },
		},
		{
			name: "delay mode", valid: "PERCENTAGE", emptyWant: ValueUnknown,
			set: func(e *ObservationDecisionEnvelope, v string) { e.Settings.DelayMode = v },
			get: func(e *ObservationDecisionEnvelope) string { return e.Settings.DelayMode },
		},
		{
			name: "filter key", valid: "decision_points", emptyWant: ValueUnknown,
			set: func(e *ObservationDecisionEnvelope, v string) { e.Settings.FilterCondition.By = v },
			get: func(e *ObservationDecisionEnvelope) string { return e.Settings.FilterCondition.By },
		},
		{
			name: "filter comparison", valid: "LTE", emptyWant: ValueUnknown,
			set: func(e *ObservationDecisionEnvelope, v string) { e.Settings.FilterCondition.Where = v },
			get: func(e *ObservationDecisionEnvelope) string { return e.Settings.FilterCondition.Where },
		},
		{
			name: "stake gate reason", valid: "reserve_violation", emptyWant: "",
			set: func(e *ObservationDecisionEnvelope, v string) { e.StakeReason = v },
			get: func(e *ObservationDecisionEnvelope) string { return e.StakeReason },
		},
		{
			name: "health gate reason", valid: "health_gql_api_failed", emptyWant: "",
			set: func(e *ObservationDecisionEnvelope, v string) { e.HealthReason = v },
			get: func(e *ObservationDecisionEnvelope) string { return e.HealthReason },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member := skeleton()
			tc.set(member, tc.valid)
			if got := tc.get(sanitizedEnvelope(t, member)); got != tc.valid {
				t.Fatalf("the %s member %q sanitized to %q: a value this build DOES understand is "+
					"being recorded as one it does not", tc.name, tc.valid, got)
			}

			empty := skeleton()
			tc.set(empty, "")
			if got := tc.get(sanitizedEnvelope(t, empty)); got != tc.emptyWant {
				t.Fatalf("an empty %s sanitized to %q, want %q", tc.name, got, tc.emptyWant)
			}

			raw := skeleton()
			tc.set(raw, secret)
			sanitized := sanitizedEnvelope(t, raw)
			if got := tc.get(sanitized); got != ValueUnknown {
				t.Fatalf("an unrecognized %s survived as %q, want %q: raw producer text is being "+
					"persisted in a field the contract declares closed", tc.name, got, ValueUnknown)
			}
			payload := renderEnvelopePayload(t, envelopeFact(raw, nil).Payload)
			fact := mustSanitize(t, envelopeFact(raw, nil))
			blob := payload + "\x00" + observationDigest(fact, "o:1", "s", 1, 1,
				fact.RoundIncarnationID, payload, nil, nil, nil)
			if strings.Contains(blob, "SECRET") || strings.Contains(blob, "oauth") {
				t.Fatalf("the rejected %s reached the stored payload or its digest: %s", tc.name, blob)
			}
		})
	}
}

// TestAnEnvelopeCeilingBreachRefusesTheWholeFact regresses the shortening
// defect: keeping 64 of 70 model outcomes, or truncating an outcome id to fit,
// produces a decision record that is FALSE and that no reader can tell apart
// from a complete one — and it is then hashed, committed and counted as a fact
// faithfully recorded.
//
// A naive implementation clamps, and passes every test that only offers legal
// input. Each case here is therefore run twice: AT the ceiling, where the fact
// must be accepted WHOLE, and one past it, where sanitizeObservationPayload must
// return false rather than returning a shortened value with ok=true.
func TestAnEnvelopeCeilingBreachRefusesTheWholeFact(t *testing.T) {
	modelOutcomes := func(n int) []ObservationModelOutcome {
		out := make([]ObservationModelOutcome, n)
		for i := range out {
			out[i] = ObservationModelOutcome{Present: true, ID: "outcome-" + fmt.Sprint(i), TotalPoints: int64(i)}
		}
		return out
	}

	for _, tc := range []struct {
		name   string
		build  func() *ObservationDecisionEnvelope
		wantOK bool
		// verify runs only on the accepted (at-the-limit) cases.
		verify func(*testing.T, *ObservationDecisionEnvelope)
	}{
		{
			name: "model outcomes exactly at the ceiling are kept whole",
			build: func() *ObservationDecisionEnvelope {
				e := fullDecisionEnvelope()
				e.Outcomes = modelOutcomes(MaxObservationOutcomes)
				e.ChoiceIndex = envInt(MaxObservationOutcomes - 1)
				return e
			},
			wantOK: true,
			verify: func(t *testing.T, e *ObservationDecisionEnvelope) {
				if len(e.Outcomes) != MaxObservationOutcomes {
					t.Fatalf("stored %d model outcomes, want %d: the vector was shortened",
						len(e.Outcomes), MaxObservationOutcomes)
				}
				for i, o := range e.Outcomes {
					if o.Slot != i {
						t.Fatalf("model outcome %d has slot %d; a stored slot must be its true "+
							"position in the vector", i, o.Slot)
					}
				}
			},
		},
		{
			name: "one model outcome past the ceiling refuses the fact",
			build: func() *ObservationDecisionEnvelope {
				e := fullDecisionEnvelope()
				e.Outcomes = modelOutcomes(MaxObservationOutcomes + 1)
				return e
			},
		},
		{
			name: "a choice index naming the last storable slot is kept",
			build: func() *ObservationDecisionEnvelope {
				e := fullDecisionEnvelope()
				e.Outcomes = modelOutcomes(MaxObservationOutcomes)
				e.ChoiceIndex = envInt(MaxObservationOutcomes - 1)
				return e
			},
			wantOK: true,
			verify: func(t *testing.T, e *ObservationDecisionEnvelope) {
				if e.ChoiceIndex == nil || *e.ChoiceIndex != MaxObservationOutcomes-1 {
					t.Fatalf("choiceIndex = %v, want %d", e.ChoiceIndex, MaxObservationOutcomes-1)
				}
			},
		},
		{
			name: "a choice index at the outcome ceiling refuses the fact",
			build: func() *ObservationDecisionEnvelope {
				e := fullDecisionEnvelope()
				e.ChoiceIndex = envInt(MaxObservationOutcomes)
				return e
			},
		},
		{
			name: "a choice outcome id exactly at the string ceiling is kept whole",
			build: func() *ObservationDecisionEnvelope {
				e := fullDecisionEnvelope()
				e.ChoiceOutcomeID = strings.Repeat("A", MaxObservationString)
				return e
			},
			wantOK: true,
			verify: func(t *testing.T, e *ObservationDecisionEnvelope) {
				if len(e.ChoiceOutcomeID) != MaxObservationString {
					t.Fatalf("choiceOutcomeId is %d bytes, want %d: a shortened id names a "+
						"different outcome", len(e.ChoiceOutcomeID), MaxObservationString)
				}
			},
		},
		{
			name: "a choice outcome id past the string ceiling refuses the fact",
			build: func() *ObservationDecisionEnvelope {
				e := fullDecisionEnvelope()
				e.ChoiceOutcomeID = strings.Repeat("A", MaxObservationString+1)
				return e
			},
		},
		{
			name: "a model outcome id exactly at the string ceiling is kept whole",
			build: func() *ObservationDecisionEnvelope {
				e := fullDecisionEnvelope()
				e.Outcomes = []ObservationModelOutcome{{Present: true, ID: strings.Repeat("B", MaxObservationString)}}
				e.ChoiceIndex = envInt(0)
				return e
			},
			wantOK: true,
			verify: func(t *testing.T, e *ObservationDecisionEnvelope) {
				if len(e.Outcomes) != 1 || len(e.Outcomes[0].ID) != MaxObservationString {
					t.Fatalf("model outcome id survived as %d bytes, want %d",
						len(e.Outcomes[0].ID), MaxObservationString)
				}
			},
		},
		{
			name: "a model outcome id past the string ceiling refuses the fact",
			build: func() *ObservationDecisionEnvelope {
				e := fullDecisionEnvelope()
				e.Outcomes = []ObservationModelOutcome{{Present: true, ID: strings.Repeat("B", MaxObservationString+1)}}
				return e
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := sanitizeObservationPayload(envelopeFact(tc.build(), nil).Payload)
			if ok != tc.wantOK {
				if tc.wantOK {
					t.Fatal("a fact within every ceiling was refused: a legal decision record is " +
						"being lost and counted as a drop")
				}
				t.Fatal("a fact past a frozen ceiling was ACCEPTED: it will be stored, hashed and " +
					"counted as complete while silently missing part of what the decision read")
			}
			if !ok {
				if out.DecisionEnvelope != nil {
					t.Fatalf("a refused fact still produced an envelope %+v; a partial record must "+
						"never escape the sanitizer", out.DecisionEnvelope)
				}
				return
			}
			if tc.verify != nil {
				tc.verify(t, out.DecisionEnvelope)
			}
		})
	}

	// A NEGATIVE choice index is Calculate's own "no strategy matched" answer.
	// It is the ABSENCE of a choice, not a breach, and must not cost the fact.
	t.Run("a negative choice index is an absent choice, not a breach", func(t *testing.T) {
		e := fullDecisionEnvelope()
		e.ChoiceIndex = envInt(-1)
		out, ok := sanitizeObservationPayload(envelopeFact(e, nil).Payload)
		if !ok {
			t.Fatal("a decision that chose nothing was refused: the record of a strategy declining " +
				"to bet is exactly as important as the record of one that bet")
		}
		if out.DecisionEnvelope.ChoiceIndex != nil {
			t.Fatalf("an absent choice came back as slot %d: the fact now claims a choice the "+
				"strategy never made", *out.DecisionEnvelope.ChoiceIndex)
		}
	})
}

// TestANonFiniteEnvelopeNumberRefusesTheWholeFact regresses the substitution
// defect. A NaN or an infinity has no JSON number form, so the tempting fix is
// to store 0, a clamp, or the previous value — a number the decision never used,
// recorded as though it had, and then reported as a SUCCESS.
//
// A naive implementation that substitutes passes every test that checks the
// fact was stored, because the fact IS stored. The property only fails on the
// value, so each case here asserts both that sanitizeObservationPayload returns
// false and that no substituted envelope escaped it; the end-to-end half then
// proves the loss is visible in the session's own accounting.
func TestANonFiniteEnvelopeNumberRefusesTheWholeFact(t *testing.T) {
	// The payload returned beside ok=false is a discard value the callers never
	// read, exactly as it is for the outcome ceilings. What must never happen is
	// that the CONTAINER holding the offending number comes back carrying a
	// substituted one, so each case names the container to check.
	for _, field := range []struct {
		name string
		set  func(*ObservationPayload, float64)
		// admission says the offending number lives in the admission-time
		// snapshot rather than in the decision envelope.
		admission bool
	}{
		{name: "settings delay", set: func(p *ObservationPayload, v float64) { p.DecisionEnvelope.Settings.Delay = v }},
		{name: "filter condition value", set: func(p *ObservationPayload, v float64) {
			p.DecisionEnvelope.Settings.FilterCondition.Value = v
		}},
		{name: "skip compared", set: func(p *ObservationPayload, v float64) { p.DecisionEnvelope.SkipCompared = envFloat(v) }},
		{name: "outcome percentage users", set: func(p *ObservationPayload, v float64) {
			p.DecisionEnvelope.Outcomes[1].PercentageUsers = v
		}},
		{name: "outcome odds", set: func(p *ObservationPayload, v float64) { p.DecisionEnvelope.Outcomes[1].Odds = v }},
		{name: "outcome odds percentage", set: func(p *ObservationPayload, v float64) {
			p.DecisionEnvelope.Outcomes[1].OddsPercentage = v
		}},
		{name: "admission settings delay", admission: true,
			set: func(p *ObservationPayload, v float64) { p.AdmissionSettings.Delay = v }},
		{name: "admission filter condition value", admission: true, set: func(p *ObservationPayload, v float64) {
			p.AdmissionSettings.FilterCondition.Value = v
		}},
	} {
		for _, value := range []struct {
			name string
			v    float64
		}{
			{"NaN", math.NaN()},
			{"+Inf", math.Inf(1)},
			{"-Inf", math.Inf(-1)},
		} {
			t.Run(field.name+"/"+value.name, func(t *testing.T) {
				payload := envelopeFact(fullDecisionEnvelope(), fullBetSettings()).Payload
				field.set(&payload, value.v)
				out, ok := sanitizeObservationPayload(payload)
				if ok {
					rendered, renderable := marshalObservationPayload(out)
					t.Fatalf("a %s of %v was ACCEPTED (renderable=%v, %q): the number the decision "+
						"actually used has been replaced by one it never saw, and the fact reports "+
						"success", field.name, value.v, renderable, rendered)
				}
				if field.admission {
					if out.AdmissionSettings != nil {
						t.Fatalf("the refused admission snapshot came back as %+v: the number the "+
							"round was admitted with has been replaced by one that never existed",
							*out.AdmissionSettings)
					}
				} else if out.DecisionEnvelope != nil {
					t.Fatalf("the refused envelope came back as %+v: the number the decision "+
						"actually used has been replaced by a substitute", *out.DecisionEnvelope)
				}
			})
		}
	}

	// End to end: the loss must be VISIBLE. A refused fact is counted as a drop,
	// which is what stops the session finalizing COMPLETE and stops a reader
	// treating the missing decision as proof that no decision happened.
	t.Run("the loss is counted and the session reads INCOMPLETE", func(t *testing.T) {
		svc, repo := newObservationService(t)
		env := fullDecisionEnvelope()
		env.Settings.Delay = math.NaN()
		svc.RecordPredictionObservation(envelopeFact(env, nil))
		awaitDropped(t, svc, 1)

		got, err := repo.ObservationsBySession(context.Background(), svc.observations.sessionID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("a fact carrying a non-finite number was stored anyway: %+v", got)
		}
		if c := svc.observations.committed.Load(); c != 0 {
			t.Fatalf("committed = %d, want 0: a refused fact must never count as recorded", c)
		}

		svc.observations.Close()
		reading, _, err := repo.ReadObservationSession(context.Background(), svc.observations.epoch.Load())
		if err != nil {
			t.Fatal(err)
		}
		if reading.Session.CloseState != SessionIncomplete {
			t.Fatalf("close state = %q, want %q: the session lost a decision record and says it "+
				"lost nothing, so a reader would take the absence as evidence",
				reading.Session.CloseState, SessionIncomplete)
		}
	})
}

// TestAFactWithoutAnEnvelopeRendersTheBytesTheOldContractWrote regresses the
// one change P1.5 was not allowed to make. observation_sha256 hashes the exact
// payload bytes, and witness verification re-reads the STORED bytes, so any
// change to how an envelope-free fact renders — an added key, a `null`, a
// reordered field — makes every historical row fail its own witness and read as
// an integrity error.
//
// A naive implementation drops `omitempty`, or inserts the new fields anywhere
// but last. Both still round-trip perfectly, so no other test in this file would
// notice; only a byte-for-byte expectation can.
func TestAFactWithoutAnEnvelopeRendersTheBytesTheOldContractWrote(t *testing.T) {
	manual := false
	slot := 1
	payload := ObservationPayload{
		Phase:       "AUTO_DECIDED",
		RoundState:  "ACTIVE",
		Decision:    "PLACE",
		ReasonCode:  "OK",
		ErrorClass:  "NONE",
		Manual:      &manual,
		OutcomeSlot: &slot,
		Outcomes: []ObservationOutcome{{
			Slot: 0, Color: "BLUE", ColorState: PresencePresent,
			TotalPoints: 5, TotalUsers: 2,
			TopPredictorsExamined: 3, TopPredictors: PresencePresent,
		}},
		Counters: map[string]int64{"stake": 10},
		Presence: map[string]string{"event": PresencePresent},
		// The two P1.5 fields, both genuinely absent.
		DecisionEnvelope:  nil,
		AdmissionSettings: nil,
	}

	const want = `{"phase":"AUTO_DECIDED","roundState":"ACTIVE","decision":"PLACE","reasonCode":"OK",` +
		`"errorClass":"NONE","manual":false,"outcomeSlot":1,` +
		`"outcomes":[{"slot":0,"color":"BLUE","colorState":"PRESENT","totalPoints":5,"totalUsers":2,` +
		`"topPredictorsExamined":3,"topPredictors":"PRESENT"}],` +
		`"counters":{"stake":10},"presence":{"event":"PRESENT"}}`

	got := renderEnvelopePayload(t, payload)
	if got != want {
		t.Fatalf("an envelope-free fact no longer renders the bytes the previous contract wrote.\n"+
			" got: %s\nwant: %s\nEvery historical row's digest was computed over the old bytes, so "+
			"this change makes them all fail their own witness and read as INTEGRITY_ERROR.", got, want)
	}
	for _, key := range []string{"decisionEnvelope", "admissionSettings"} {
		if strings.Contains(got, key) {
			t.Fatalf("a fact with no envelope still rendered %q: %s", key, got)
		}
	}
}

// TestAnEnvelopeRendersTheSameBytesEveryTime regresses non-determinism in the
// projection. observation_sha256 is only a witness if the same projection always
// renders the same bytes; a map iterated in range order, or a slice built from
// one, would make a row's digest depend on which run wrote it.
//
// A naive implementation passes a single marshal-and-compare, because one run is
// always self-consistent. The property only fails across repetitions and across
// independently-built values, so this test does both.
func TestAnEnvelopeRendersTheSameBytesEveryTime(t *testing.T) {
	payload, ok := sanitizeObservationPayload(envelopeFact(fullDecisionEnvelope(), fullBetSettings()).Payload)
	if !ok {
		t.Fatal("the fixture does not sanitize")
	}
	first, ok := marshalObservationPayload(payload)
	if !ok {
		t.Fatal("the fixture does not render")
	}
	for i := 0; i < 200; i++ {
		again, ok := marshalObservationPayload(payload)
		if !ok {
			t.Fatalf("repetition %d failed to render a payload that rendered once", i)
		}
		if again != first {
			t.Fatalf("repetition %d rendered different bytes for the same projection:\n%s\n%s\n"+
				"observation_sha256 cannot witness anything if the same fact hashes two ways",
				i, first, again)
		}
	}

	// Two envelopes built independently from the same values must be
	// indistinguishable on the wire, otherwise an identical retry of one fact
	// would collide with its own stored digest.
	other, ok := sanitizeObservationPayload(envelopeFact(fullDecisionEnvelope(), fullBetSettings()).Payload)
	if !ok {
		t.Fatal("the second fixture does not sanitize")
	}
	rendered, ok := marshalObservationPayload(other)
	if !ok {
		t.Fatal("the second fixture does not render")
	}
	if rendered != first {
		t.Fatalf("two equal envelopes rendered different bytes:\n%s\n%s", first, rendered)
	}
}

// TestTheWitnessCoversTheDecisionEnvelope regresses a digest that hashes the
// fact's columns but not the envelope inside its payload. Such a digest verifies
// happily after the envelope has been rewritten, which turns the trail's
// integrity witness into decoration for exactly the data P1.5 exists to protect.
//
// A naive implementation passes TestObservationDigestIsStableAndCoversContent,
// which only varies the payload as a whole. Here every case changes ONE envelope
// field and nothing else — the counters map is held constant on purpose — so a
// digest that ignores the envelope produces identical hashes.
func TestTheWitnessCoversTheDecisionEnvelope(t *testing.T) {
	digestOf := func(t *testing.T, env *ObservationDecisionEnvelope) string {
		t.Helper()
		fact := mustSanitize(t, envelopeFact(env, nil))
		payload, ok := marshalObservationPayload(fact.Payload)
		if !ok {
			t.Fatal("the fixture payload could not be rendered")
		}
		return observationDigest(fact, "o:1", "s", 1, 1, fact.RoundIncarnationID, payload,
			int64(7), nil, int64(7))
	}

	base := digestOf(t, fullDecisionEnvelope())
	if base != digestOf(t, fullDecisionEnvelope()) {
		t.Fatal("the same envelope hashed two different ways; the witness is not reproducible")
	}

	for _, tc := range []struct {
		name   string
		mutate func(*ObservationDecisionEnvelope)
	}{
		{"the attempt id", func(e *ObservationDecisionEnvelope) { e.AttemptID++ }},
		{"a settings field", func(e *ObservationDecisionEnvelope) { e.Settings.Percentage++ }},
		{"the settings strategy", func(e *ObservationDecisionEnvelope) { e.Settings.Strategy = "HIGH_ODDS" }},
		{"the filter threshold", func(e *ObservationDecisionEnvelope) { e.Settings.FilterCondition.Value += 1 }},
		{"a model outcome aggregate", func(e *ObservationDecisionEnvelope) { e.Outcomes[0].TopPoints++ }},
		{"a model outcome's odds", func(e *ObservationDecisionEnvelope) { e.Outcomes[0].Odds += 0.5 }},
		{"the model outcome order", func(e *ObservationDecisionEnvelope) {
			e.Outcomes[0], e.Outcomes[1] = e.Outcomes[1], e.Outcomes[0]
		}},
		{"the chosen slot", func(e *ObservationDecisionEnvelope) { e.ChoiceIndex = envInt(0) }},
		{"the chosen outcome id", func(e *ObservationDecisionEnvelope) { e.ChoiceOutcomeID = "outcome-blue" }},
		{"the filter's answer", func(e *ObservationDecisionEnvelope) { e.SkipResult = envBool(false) }},
		{"a stage state", func(e *ObservationDecisionEnvelope) { e.StakeStage = DecisionStageNotReached }},
		{"the health verdict", func(e *ObservationDecisionEnvelope) { e.HealthStage = DecisionHealthAllowed }},
		{"the binding gate reason", func(e *ObservationDecisionEnvelope) { e.StakeReason = "reserve_violation" }},
		{"whether the clamp was applied", func(e *ObservationDecisionEnvelope) { e.ClampApplied = envBool(false) }},
		{"the final stake", func(e *ObservationDecisionEnvelope) { e.FinalAmount = envInt64(1) }},
		{"the presence of the envelope at all", func(e *ObservationDecisionEnvelope) { *e = ObservationDecisionEnvelope{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := fullDecisionEnvelope()
			tc.mutate(env)
			if digestOf(t, env) == base {
				t.Fatalf("changing %s left the witness unchanged: the envelope can be rewritten in "+
					"place and every reader will still call the row authentic", tc.name)
			}
		})
	}

	// And the stored row proves it for real: a payload edited behind the
	// store's back fails the witness written with it.
	t.Run("an envelope edited in the stored row fails its own witness", func(t *testing.T) {
		svc, repo := newObservationService(t)
		ctx := context.Background()
		epoch := svc.observations.epoch.Load()
		sessionID := svc.observations.sessionID

		fact := mustSanitize(t, envelopeFact(fullDecisionEnvelope(), fullBetSettings()))
		if err := repo.AppendObservation(ctx, fact, sessionID, epoch, 1); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if applied, err := repo.FinalizeObservationSession(ctx, epoch,
			ObservationAccounting{Committed: 1, LastAssignedSequence: 1}, 900); err != nil || !applied {
			t.Fatalf("finalize: applied=%v err=%v", applied, err)
		}
		before, ok, err := repo.ReadObservationSession(ctx, epoch)
		if err != nil || !ok {
			t.Fatalf("read: ok=%v err=%v", ok, err)
		}
		if before.Reading != ReadingAsFinalized || before.WitnessesVerified != 1 {
			t.Fatalf("an intact envelope session reads %q with %d witnesses verified, want %q and 1",
				before.Reading, before.WitnessesVerified, ReadingAsFinalized)
		}

		// Rewrite the decision's chosen slot in place — the smallest edit that
		// changes what the record claims the strategy did.
		edited := envelopeFact(fullDecisionEnvelope(), fullBetSettings()).Payload
		edited.DecisionEnvelope.ChoiceIndex = envInt(0)
		edited.DecisionEnvelope.ChoiceOutcomeID = "outcome-blue"
		if _, err := repo.db.Exec(`UPDATE prediction_observations SET payload_json = ? WHERE collector_sequence = 1`,
			renderEnvelopePayload(t, edited)); err != nil {
			t.Fatal(err)
		}
		after, ok, err := repo.ReadObservationSession(ctx, epoch)
		if err != nil || !ok {
			t.Fatalf("read after edit: ok=%v err=%v", ok, err)
		}
		if after.Reading != ReadingIntegrityError {
			t.Fatalf("a session holding a rewritten envelope reads %q (%s), want %q — the digest "+
				"witnesses nothing for the envelope if an edit passes verification",
				after.Reading, after.Detail, ReadingIntegrityError)
		}
	})
}

// TestAPreEnvelopeRowStillReadsAndStillWitnessesItself regresses the upgrade
// break. Rows written before P1.5 carry no envelope keys at all, and the new
// optional fields must not turn them into unreadable rows, into rows whose
// digest no longer verifies, or — worst — into rows that appear to carry a
// zero-valued decision the producer never made.
//
// A naive implementation makes DecisionEnvelope a VALUE rather than a pointer,
// or gives it a non-nil default. Every round-trip test above still passes, and
// the old rows silently gain a complete-looking decision computed from nothing.
func TestAPreEnvelopeRowStillReadsAndStillWitnessesItself(t *testing.T) {
	t.Run("a pre-P1.5 payload decodes cleanly and stays envelope-free", func(t *testing.T) {
		svc, repo := newObservationService(t)
		// insertObservationRow writes the pre-P1.5 payload shape verbatim:
		// a phase and nothing else, with no envelope keys.
		insertObservationRow(t, repo.db, svc.observations.epoch.Load()+1, 1,
			"o-legacy", "pool-legacy", "", "", 1_700_000_000_000)

		got, err := repo.ObservationsBySession(context.Background(), "s", 0)
		if err != nil {
			t.Fatalf("read legacy row: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("read %d legacy rows, want 1", len(got))
		}
		rec := got[0]
		if rec.PayloadUndecodable {
			t.Fatal("a row written before the envelope existed no longer decodes: the additive " +
				"change made every historical fact unreadable")
		}
		if rec.Payload.Phase != ValueUnknown {
			t.Fatalf("legacy payload phase = %q, want %q", rec.Payload.Phase, ValueUnknown)
		}
		if rec.Payload.DecisionEnvelope != nil {
			t.Fatalf("a pre-envelope row came back carrying %+v: an absent field has become a "+
				"complete-looking decision record computed from nothing",
				*rec.Payload.DecisionEnvelope)
		}
		if rec.Payload.AdmissionSettings != nil {
			t.Fatalf("a pre-envelope row came back carrying admission settings %+v",
				*rec.Payload.AdmissionSettings)
		}
	})

	t.Run("an envelope-free fact still verifies its own witness", func(t *testing.T) {
		svc, repo := newObservationService(t)
		ctx := context.Background()
		epoch := svc.observations.epoch.Load()

		// The digest hashes ObservationPayloadVersion from the COMPILE-TIME
		// constant while verification re-reads the stored bytes, so a bumped
		// version would make every historical row fail its own witness. P1.5 is
		// additive and must not have moved it.
		if ObservationPayloadVersion != 1 {
			t.Fatalf("ObservationPayloadVersion = %d, want 1: bumping it for an additive optional "+
				"field makes every row written under version 1 read as an integrity error",
				ObservationPayloadVersion)
		}

		fact := mustSanitize(t, channelObservation("pool-1", "chan-a", "streamer-a", "event-1", "ROUND_CREATED"))
		if fact.Payload.DecisionEnvelope != nil {
			t.Fatal("the legacy-shaped fixture grew an envelope; it no longer represents an old row")
		}
		if err := repo.AppendObservation(ctx, fact, svc.observations.sessionID, epoch, 1); err != nil {
			t.Fatalf("seed: %v", err)
		}
		var stored string
		if err := repo.db.QueryRow(
			`SELECT payload_json FROM prediction_observations WHERE collector_sequence = 1`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"decisionEnvelope", "admissionSettings"} {
			if strings.Contains(stored, key) {
				t.Fatalf("a fact with no envelope stored %q: %s", key, stored)
			}
		}
		if applied, err := repo.FinalizeObservationSession(ctx, epoch,
			ObservationAccounting{Committed: 1, LastAssignedSequence: 1}, 900); err != nil || !applied {
			t.Fatalf("finalize: applied=%v err=%v", applied, err)
		}
		reading, ok, err := repo.ReadObservationSession(ctx, epoch)
		if err != nil || !ok {
			t.Fatalf("read: ok=%v err=%v", ok, err)
		}
		if reading.Reading != ReadingAsFinalized {
			t.Fatalf("an envelope-free session reads %q (%s), want %q: adding the optional fields "+
				"broke the witness of facts that do not use them",
				reading.Reading, reading.Detail, ReadingAsFinalized)
		}
		if reading.WitnessesVerified != 1 {
			t.Fatalf("verified %d witnesses, want 1: the reading proved nothing about the "+
				"envelope-free row", reading.WitnessesVerified)
		}
	})
}

// TestTheEnvelopeCarriesNoIdentityBeyondTheOutcomeID regresses the privacy
// defect that would outlive every erasure that could fix it: an identifier
// placed inside payload_json is reachable by no privacy erasure, because an
// erasure selects on COLUMNS.
//
// A naive implementation stores the raw strings it was handed — which every
// round-trip test in this file rewards — so the leak only shows when the input
// is a non-member. Every string field of the source envelope therefore carries a
// secret here except the two round-scoped outcome ids, which are legitimately
// retained, and the secret is proved absent from the rendered payload.
func TestTheEnvelopeCarriesNoIdentityBeyondTheOutcomeID(t *testing.T) {
	const secret = "chan-133217130:mystreamer:user-77:oauth-SECRET"

	env := &ObservationDecisionEnvelope{
		AttemptID:     envelopeAttemptID,
		SettingsStage: secret,
		Settings: &ObservationBetSettings{
			Strategy:        secret,
			DelayMode:       secret,
			FilterCondition: &ObservationFilterCondition{By: secret, Where: secret, Value: 1},
		},
		CalculateStage: secret,
		// Round-scoped outcome ids are the ONE identifier the envelope may
		// keep: they name an outcome of a round, never a channel or a person.
		Outcomes: []ObservationModelOutcome{
			{Present: true, ID: "outcome-blue"},
			{Present: true, ID: "outcome-pink"},
		},
		ChoiceIndex:     envInt(0),
		ChoiceOutcomeID: "outcome-blue",
		SkipStage:       secret,
		HealthStage:     secret,
		HealthReason:    secret,
		StakeStage:      secret,
		StakeReason:     secret,
	}
	admission := &ObservationBetSettings{
		Strategy:        secret,
		DelayMode:       secret,
		FilterCondition: &ObservationFilterCondition{By: secret, Where: secret, Value: 2},
	}

	fact := mustSanitize(t, envelopeFact(env, admission))
	payload, ok := marshalObservationPayload(fact.Payload)
	if !ok {
		t.Fatal("the sanitized payload could not be rendered")
	}
	blob := strings.Join([]string{
		payload,
		fact.SourceFingerprint,
		observationDigest(fact, "o:1", "s", 1, 1, fact.RoundIncarnationID, payload, nil, nil, nil),
	}, "\x00")
	for _, leak := range []string{"SECRET", "oauth", "mystreamer", "user-77", "133217130"} {
		if strings.Contains(blob, leak) {
			t.Fatalf("%q reached the persisted decision record: an identifier inside payload_json "+
				"outlives every privacy erasure, which reach a fact only through its columns\n%s",
				leak, blob)
		}
	}

	got := fact.Payload.DecisionEnvelope
	for name, value := range map[string]string{
		"settingsStage":  got.SettingsStage,
		"calculateStage": got.CalculateStage,
		"skipStage":      got.SkipStage,
		"stakeStage":     got.StakeStage,
		"healthStage":    got.HealthStage,
		"healthReason":   got.HealthReason,
		"stakeReason":    got.StakeReason,
		"strategy":       got.Settings.Strategy,
		"delayMode":      got.Settings.DelayMode,
		"filterBy":       got.Settings.FilterCondition.By,
		"filterWhere":    got.Settings.FilterCondition.Where,
	} {
		if value != ValueUnknown {
			t.Fatalf("%s survived as %q, want %q: a field the contract declares closed accepted "+
				"free text", name, value, ValueUnknown)
		}
	}
	if got.ChoiceOutcomeID != "outcome-blue" || got.Outcomes[1].ID != "outcome-pink" {
		t.Fatalf("the round-scoped outcome ids were dropped (%q / %q): the choice can no longer be "+
			"linked to the placement call that carried it", got.ChoiceOutcomeID, got.Outcomes[1].ID)
	}

	// Structural: no field of the envelope may even NAME an erasable identity,
	// so a future producer cannot fill one in without this failing.
	for _, typ := range []reflect.Type{
		reflect.TypeOf(ObservationDecisionEnvelope{}),
		reflect.TypeOf(ObservationBetSettings{}),
		reflect.TypeOf(ObservationFilterCondition{}),
		reflect.TypeOf(ObservationModelOutcome{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name := strings.ToLower(f.Name + " " + f.Tag.Get("json"))
			for _, forbidden := range []string{"login", "channel", "streamer", "predictor", "token", "userid"} {
				if strings.Contains(name, forbidden) {
					t.Fatalf("%s.%s names an erasable identity (%q): a channel, login, user or "+
						"predictor stored inside payload_json is unreachable by every privacy "+
						"erasure", typ.Name(), f.Name, forbidden)
				}
			}
		}
	}
}

// TestAWorstCaseEnvelopeStaysUnderThePayloadCeiling regresses the failure mode
// where the envelope is correct in every other respect and simply does not fit:
// marshalObservationPayload refuses an over-cap payload, so an envelope sized
// past the ceiling would silently turn every rich auto_decision fact into a
// counted drop, and the trail would lose exactly the facts it was extended for.
//
// A naive implementation checks a two-outcome envelope and concludes there is
// room. The bound that matters is the WORST case the ceilings allow — a full
// wire outcome vector, a full model outcome vector, both settings snapshots and
// every pointer present — so that is what is measured here.
func TestAWorstCaseEnvelopeStaysUnderThePayloadCeiling(t *testing.T) {
	env := fullDecisionEnvelope()
	env.AttemptID = math.MaxUint64
	env.Balance = envInt64(math.MaxInt64)
	env.BetTotalUsers = envInt64(math.MaxInt64)
	env.BetTotalPoints = envInt64(math.MaxInt64)
	env.ChoiceIndex = envInt(MaxObservationOutcomes - 1)
	env.ChoiceOutcomeID = "b7f1c2d3-4e5a-6b7c-8d9e-0f1a2b3c4d5e"
	env.ChoiceAmount = envInt64(math.MaxInt64)
	env.SkipCompared = envFloat(12.345678901234567)
	env.StakeAllowed = envInt64(math.MaxInt64)
	env.StakeLimit = envInt64(math.MaxInt64)
	env.FinalAmount = envInt64(math.MaxInt64)
	env.RiskMaxStakePercent = envInt(math.MaxInt32)
	env.RiskReservePoints = envInt(math.MaxInt32)
	env.Settings.Delay = 98765.43210987654
	env.Settings.FilterCondition.Value = 87654.32109876543
	env.Outcomes = nil
	for i := 0; i < MaxObservationOutcomes; i++ {
		env.Outcomes = append(env.Outcomes, ObservationModelOutcome{
			Present:         true,
			ID:              fmt.Sprintf("b7f1c2d3-4e5a-6b7c-8d9e-%012d", i),
			TotalUsers:      math.MaxInt64,
			TotalPoints:     math.MaxInt64,
			TopPoints:       math.MaxInt64,
			PercentageUsers: 12.345678901234567,
			Odds:            98765.43210987654,
			OddsPercentage:  87.65432109876543,
		})
	}

	admission := fullBetSettings()
	admission.Delay = 98765.43210987654
	admission.FilterCondition.Value = 76543.21098765432

	fact := envelopeFact(env, admission)
	// The wire outcome vector is bounded independently and can be full at the
	// same time, so the worst case carries both.
	for i := 0; i < MaxObservationOutcomes; i++ {
		fact.Payload.Outcomes = append(fact.Payload.Outcomes, ObservationOutcome{
			Color: "BLUE", ColorState: PresenceUnknownPresent,
			TotalPoints: math.MaxInt64, TotalUsers: math.MaxInt64,
			TopPredictorsExamined: MaxTopPredictorsExamined, TopPredictors: PresenceUnknownPresent,
		})
	}

	sanitized, ok := sanitizeObservation(fact, 1)
	if !ok {
		t.Fatal("the worst-case decision record was REFUSED: every rich auto_decision fact would " +
			"be dropped, and the trail would lose exactly the facts P1.5 added")
	}
	rendered, ok := marshalObservationPayload(sanitized.Payload)
	if !ok {
		t.Fatal("the worst-case decision record could not be rendered within the payload ceiling")
	}
	t.Logf("worst-case envelope payload renders %d bytes (ceiling %d)", len(rendered), MaxObservationPayloadBytes)
	if len(rendered) >= MaxObservationPayloadBytes {
		t.Fatalf("the worst-case decision record renders %d bytes against a %d ceiling: the fact "+
			"is refused whole and counted as a drop", len(rendered), MaxObservationPayloadBytes)
	}
	// Sanity: the fixture really is the worst case the ceilings allow.
	if len(sanitized.Payload.Outcomes) != MaxObservationOutcomes ||
		len(sanitized.Payload.DecisionEnvelope.Outcomes) != MaxObservationOutcomes {
		t.Fatalf("the fixture is not at the ceilings (%d wire / %d model outcomes); the margin it "+
			"reports is not the worst case", len(sanitized.Payload.Outcomes),
			len(sanitized.Payload.DecisionEnvelope.Outcomes))
	}
}

// TestAnOutcomeIDThatNormalizationWouldChangeRefusesTheFact regresses a
// substitution defect in the one field of the envelope that names something
// outside the record.
//
// The envelope's outcome ids used to go through boundedIdentifier, which trims
// before it measures and returns the TRIMMED id as valid. Nothing else on the
// path normalizes an outcome id: the model keeps what the frame carried and the
// placement mutation sends exactly that. So a padded id was stored, hashed,
// witnessed and counted as a complete record of a decision while naming an
// outcome the bet never named — the same class of defect as a truncated id,
// which this file already refuses in the loudest possible terms.
//
// The second half is the sharper one: an id that is over the frozen ceiling
// ONLY because of its padding was trimmed under the ceiling and accepted, so
// the ceiling that must refuse was turned into a ceiling that admits.
//
// Every case here is unreachable from real Twitch traffic. That is the point of
// a sanitizer: it is the boundary that must hold when the frame is not what the
// protocol says it is, and it is the last place the record's own claim about
// itself can still be made true.
func TestAnOutcomeIDThatNormalizationWouldChangeRefusesTheFact(t *testing.T) {
	atCeiling := strings.Repeat("A", MaxObservationString)

	for _, tc := range []struct {
		name   string
		build  func() *ObservationDecisionEnvelope
		wantOK bool
	}{
		{
			name: "a padded chosen outcome id refuses the fact",
			build: func() *ObservationDecisionEnvelope {
				e := fullDecisionEnvelope()
				e.ChoiceOutcomeID = " outcome-1 "
				return e
			},
		},
		{
			name: "a chosen outcome id with a trailing newline refuses the fact",
			build: func() *ObservationDecisionEnvelope {
				e := fullDecisionEnvelope()
				e.ChoiceOutcomeID = "outcome-1\n"
				return e
			},
		},
		{
			name: "a chosen outcome id over the ceiling only by padding still refuses the fact",
			build: func() *ObservationDecisionEnvelope {
				e := fullDecisionEnvelope()
				// Trimmed this fits exactly; raw it is over the frozen ceiling.
				e.ChoiceOutcomeID = " " + atCeiling + " "
				return e
			},
		},
		{
			name: "a padded model outcome id refuses the fact",
			build: func() *ObservationDecisionEnvelope {
				e := fullDecisionEnvelope()
				e.Outcomes = []ObservationModelOutcome{
					{Present: true, ID: "outcome-1"},
					{Present: true, ID: "\toutcome-2"},
				}
				e.ChoiceIndex = envInt(0)
				e.ChoiceOutcomeID = "outcome-1"
				return e
			},
		},
		{
			name: "a model outcome id over the ceiling only by padding still refuses the fact",
			build: func() *ObservationDecisionEnvelope {
				e := fullDecisionEnvelope()
				e.Outcomes = []ObservationModelOutcome{{Present: true, ID: atCeiling + " "}}
				e.ChoiceIndex = envInt(0)
				e.ChoiceOutcomeID = "outcome-1"
				return e
			},
		},
		{
			name: "an ordinary unpadded id is untouched",
			build: func() *ObservationDecisionEnvelope {
				e := fullDecisionEnvelope()
				e.ChoiceOutcomeID = "8b5a2e19-0c7f-4a6b-9d1e-2f3c4b5a6d7e"
				return e
			},
			wantOK: true,
		},
		{
			name: "an absent choice carries no id and is not a breach",
			build: func() *ObservationDecisionEnvelope {
				e := fullDecisionEnvelope()
				e.ChoiceOutcomeID = ""
				e.ChoiceIndex = envInt(-1)
				return e
			},
			wantOK: true,
		},
		{
			name: "an absent model outcome carries no id and is not a breach",
			build: func() *ObservationDecisionEnvelope {
				e := fullDecisionEnvelope()
				e.Outcomes = []ObservationModelOutcome{
					{Present: true, ID: "outcome-1"},
					{Present: false},
				}
				e.ChoiceIndex = envInt(0)
				e.ChoiceOutcomeID = "outcome-1"
				return e
			},
			wantOK: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.build()
			wantChoice := in.ChoiceOutcomeID
			wantOutcomes := append([]ObservationModelOutcome(nil), in.Outcomes...)

			out, ok := sanitizeObservationPayload(envelopeFact(in, nil).Payload)
			if ok != tc.wantOK {
				if tc.wantOK {
					t.Fatal("an id no normalization would change was refused: a legal decision " +
						"record is being lost and counted as a drop")
				}
				t.Fatal("an id that normalization ALTERED was accepted: the fact is now stored, " +
					"hashed and witnessed as a complete record while naming an outcome other " +
					"than the one the bet was placed on")
			}
			if !ok {
				if out.DecisionEnvelope != nil {
					t.Fatalf("a refused fact still produced an envelope %+v; a substituted "+
						"identifier must never escape the sanitizer", out.DecisionEnvelope)
				}
				return
			}
			// Accepted ids must survive byte for byte, so the guard cannot be
			// satisfied by quietly normalizing everything into agreement.
			if got := out.DecisionEnvelope.ChoiceOutcomeID; got != wantChoice {
				t.Fatalf("choiceOutcomeId stored as %q, want %q verbatim", got, wantChoice)
			}
			for i, o := range out.DecisionEnvelope.Outcomes {
				if o.ID != wantOutcomes[i].ID {
					t.Fatalf("model outcome %d stored id %q, want %q verbatim",
						i, o.ID, wantOutcomes[i].ID)
				}
			}
		})
	}
}
