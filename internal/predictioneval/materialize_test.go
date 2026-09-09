package predictioneval

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"testing"
)

// Tests for seam 1 ([MaterializePairedKnowledge]) and seam 2
// ([ProjectDecisionCase]) — the two stages that fix WHAT a replay is allowed
// to look at, before any stage wants to look at it.
//
// The property the whole file exists to defend is the CAUSAL CUT. The producer
// emits an attempt's placement facts AFTER its terminal fact and stamps them
// with the SAME attempt id, so a grouping that keyed on identity alone and
// ignored order would quietly hand a bet's own outcome to the decision that
// made it. Every "self-consistent" replay after that would be measuring the
// record against itself.
//
// The secondary properties are the ones that make the first one durable: an
// attempt's identity is the minted counter plus the pool instance plus the
// collector session (never the Twitch event id, which a re-admitted round
// reuses); an attempt's inputs are digested at materialization time so a
// growing store cannot reach backwards into a finished case; and a fact that
// cannot be replayed is EXCLUDED BY NAME rather than dropped, defaulted or
// guessed at.
//
// Every fixture helper below is prefixed mz so it cannot collide with a
// fixture defined in a sibling test file of this package.

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	mzEpoch   = int64(1700000000)
	mzSession = "collector-session-A"
	mzPool    = "pool-instance-1"
	mzRound   = "round-incarnation-1"
	mzEvent   = "twitch-event-1"
)

// mzSource is the baseline provenance: a cleanly finalized session, written
// under exactly the producer contract this model binds to, every surviving
// fact's witness recomputed, nothing dropped.
//
// It is deliberately anomaly-free, so any anomaly a test observes was produced
// by that test's own records rather than inherited from the fixture.
func mzSource() SourceProvenance {
	return SourceProvenance{
		CollectorEpoch:     mzEpoch,
		CollectorSessionID: mzSession,
		ProducerRevision:   SupportedProducerRevision,
		SessionReading:     readingAsFinalized,
		CloseState:         "CLOSED",
		WitnessesVerified:  8,
		WitnessesUnchecked: 0,
		FactsPresent:       8,
		CommittedCount:     8,
	}
}

func mzPtrI64(v int64) *int64 { return &v }
func mzPtrInt(v int) *int     { return &v }
func mzPtrBool(v bool) *bool  { return &v }

// mzFact builds one persisted fact. attemptID <= 0 means the fact carries no
// attempt discriminator at all, which is a real recorded shape (the scheduled
// timer fact for a round that no longer exists) and not just a degenerate one.
func mzFact(seq int64, kind, phase string, attemptID int64) SourceRecord {
	r := SourceRecord{
		ObservationID:      "obs-" + kind + "-" + phase + "-" + strconv.FormatInt(seq, 10),
		CollectorSessionID: mzSession,
		CollectorEpoch:     mzEpoch,
		CollectorSequence:  seq,
		PoolInstanceID:     mzPool,
		RoundIncarnationID: mzRound,
		RoundCaptureOrigin: "LIVE_ADMISSION",
		EventID:            mzEvent,
		Kind:               kind,
		PayloadVersion:     SupportedPayloadVersion,
		ObservationSHA256:  "row-witness-" + strconv.FormatInt(seq, 10),
		Payload:            SourcePayload{Phase: phase},
	}
	if attemptID > 0 {
		r.Payload.Counters = map[string]int64{CounterAutoAttemptID: attemptID}
	}
	return r
}

// mzDue is an attempt's opening fact.
func mzDue(seq, attemptID int64) SourceRecord {
	r := mzFact(seq, KindAutoDecision, PhaseAutoDue, attemptID)
	r.Payload.ReasonCode = "OK"
	return r
}

// mzTerminal is the ONE fact that ends an attempt and carries its envelope.
func mzTerminal(seq, attemptID int64, phase, reason string, env *SourceDecisionEnvelope) SourceRecord {
	r := mzFact(seq, KindAutoDecision, phase, attemptID)
	r.Payload.ReasonCode = reason
	r.Payload.DecisionEnvelope = env
	return r
}

// mzDecided is the common terminal fact: the policy ran and a bet was placed.
func mzDecided(seq, attemptID int64, env *SourceDecisionEnvelope) SourceRecord {
	return mzTerminal(seq, attemptID, PhaseAutoDecided, "PLACED", env)
}

// mzPlacement is a post-decision fact. It carries the SAME attempt id as the
// decision it followed — that shared id is exactly why the causal cut has to
// be positional and not identity-based.
func mzPlacement(seq, attemptID int64, phase string) SourceRecord {
	r := mzFact(seq, KindPlacement, phase, attemptID)
	if r.Payload.Counters == nil {
		r.Payload.Counters = map[string]int64{}
	}
	switch phase {
	case PhaseCallStarted:
		r.Payload.Counters[CounterStake] = 50
		r.Payload.OutcomeSlot = mzPtrInt(0)
	case PhaseCallReturned:
		r.Payload.ReasonCode = "OK"
	}
	return r
}

// mzUserTerminal is the round's settlement fact.
func mzUserTerminal(seq, attemptID int64) SourceRecord {
	r := mzFact(seq, KindUserTerminal, PhaseTerminalDelivered, attemptID)
	r.Payload.ReasonCode = "WIN"
	if r.Payload.Counters == nil {
		r.Payload.Counters = map[string]int64{}
	}
	r.Payload.Counters[CounterPayout] = 120
	return r
}

// mzExecutedEnvelope is a WELL-FORMED envelope for an attempt whose policy ran
// all the way through the stake gate. It satisfies every structural invariant
// inconsistentStages checks, so a test that wants one invariant broken breaks
// exactly that one and nothing else.
func mzExecutedEnvelope() *SourceDecisionEnvelope {
	return &SourceDecisionEnvelope{
		AttemptID:     1,
		SettingsStage: StageExecuted,
		Settings: &SourceBetSettings{
			Strategy:      StrategyMostVoted,
			Percentage:    5,
			PercentageGap: 20,
			MaxPoints:     50000,
			MinimumPoints: 0,
			StealthMode:   false,
			Delay:         6,
			DelayMode:     "FROM_END",
		},
		CalculateStage: StageExecuted,
		Balance:        mzPtrI64(1000),
		Outcomes: []SourceModelOutcome{
			{Slot: 0, Present: true, ID: "outcome-blue", TotalUsers: 10, TotalPoints: 900, TopPoints: 500, PercentageUsers: 66.6, Odds: 1.11, OddsPercentage: 90},
			{Slot: 1, Present: true, ID: "outcome-pink", TotalUsers: 5, TotalPoints: 100, TopPoints: 60, PercentageUsers: 33.3, Odds: 10, OddsPercentage: 10},
		},
		BetTotalUsers:       mzPtrI64(15),
		BetTotalPoints:      mzPtrI64(1000),
		ChoiceIndex:         mzPtrInt(0),
		ChoiceOutcomeID:     "outcome-blue",
		ChoiceAmount:        mzPtrI64(50),
		SkipStage:           StageExecuted,
		SkipResult:          mzPtrBool(false),
		HealthStage:         HealthAllowed,
		StakeStage:          StageExecuted,
		RiskMaxStakePercent: mzPtrInt(100),
		RiskReservePoints:   mzPtrInt(0),
		StakeAllowed:        mzPtrI64(50),
		StakeReason:         GateNone,
		StakeLimit:          mzPtrI64(0),
		ClampApplied:        mzPtrBool(false),
		FinalAmount:         mzPtrI64(50),
	}
}

// mzNotReachedEnvelope is the shape the producer writes when an attempt exited
// BEFORE the policy ran: every stage not reached, no settings, no balance, no
// results. Nothing here may be turned into a zero downstream.
func mzNotReachedEnvelope() *SourceDecisionEnvelope {
	return &SourceDecisionEnvelope{
		AttemptID:      1,
		SettingsStage:  StageNotReached,
		CalculateStage: StageNotReached,
		SkipStage:      StageNotReached,
		HealthStage:    HealthNotReached,
		StakeStage:     StageNotReached,
	}
}

// mzEnvelopeWithOutcomes is the well-formed envelope carrying n outcomes, for
// the store's frozen ceiling boundary.
func mzEnvelopeWithOutcomes(n int) *SourceDecisionEnvelope {
	env := mzExecutedEnvelope()
	env.Outcomes = make([]SourceModelOutcome, 0, n)
	for i := 0; i < n; i++ {
		env.Outcomes = append(env.Outcomes, SourceModelOutcome{
			Slot: i, Present: true, ID: "outcome-" + strconv.Itoa(i),
			TotalUsers: 1, TotalPoints: 10, TopPoints: 5,
		})
	}
	return env
}

func mzDataset(records ...SourceRecord) SourceDataset {
	return SourceDataset{Source: mzSource(), Records: records}
}

// mzInPool restamps facts onto another pool instance / round admission.
func mzInPool(pool, round string, recs ...SourceRecord) []SourceRecord {
	out := make([]SourceRecord, 0, len(recs))
	for _, r := range recs {
		r.PoolInstanceID = pool
		r.RoundIncarnationID = round
		out = append(out, r)
	}
	return out
}

func mzMaterialize(t *testing.T, ds SourceDataset) PairedKnowledge {
	t.Helper()
	pk, err := MaterializePairedKnowledge(ds)
	if err != nil {
		t.Fatalf("MaterializePairedKnowledge: unexpected error: %v", err)
	}
	return pk
}

func mzOnlyAttempt(t *testing.T, pk PairedKnowledge) AttemptKnowledge {
	t.Helper()
	if len(pk.Attempts) != 1 {
		t.Fatalf("attempts = %d, want exactly 1 (exclusions: %v)", len(pk.Attempts), mzReasons(pk))
	}
	return pk.Attempts[0]
}

func mzReasons(pk PairedKnowledge) []string {
	out := make([]string, 0, len(pk.Excluded))
	for _, e := range pk.Excluded {
		out = append(out, e.Reason)
	}
	return out
}

func mzObservationIDs(recs []SourceRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.ObservationID)
	}
	return out
}

func mzHasString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// mzProject runs the two seams under test end to end over a single attempt:
// one due fact, one terminal fact carrying env.
func mzProject(t *testing.T, phase, reason string, env *SourceDecisionEnvelope) DecisionCase {
	t.Helper()
	pk := mzMaterialize(t, mzDataset(mzDue(1, 1), mzTerminal(2, 1, phase, reason, env)))
	c, err := ProjectDecisionCase(mzOnlyAttempt(t, pk))
	if err != nil {
		t.Fatalf("ProjectDecisionCase: unexpected error: %v", err)
	}
	return c
}

// ---------------------------------------------------------------------------
// A. Grouping
// ---------------------------------------------------------------------------

// TestAttemptsAreGroupedByTheMintedCounterAndNotByTheSharedEventID pins the
// discriminator. Twitch reuses an event id when a round is re-admitted, so two
// separate automatic attempts can carry the identical event id; grouping on it
// would merge two decisions into one case (and, because each carries its own
// terminal fact, would then throw the merged case away as MULTIPLE_TERMINAL_
// FACTS — losing both). The minted per-pool counter is what separates them.
func TestAttemptsAreGroupedByTheMintedCounterAndNotByTheSharedEventID(t *testing.T) {
	first := []SourceRecord{mzDue(1, 1), mzDecided(2, 1, mzExecutedEnvelope())}
	// The second admission of the SAME Twitch event: same event id, new local
	// round incarnation, new attempt counter.
	second := mzInPool(mzPool, "round-incarnation-2", mzDue(3, 2), mzDecided(4, 2, mzExecutedEnvelope()))

	pk := mzMaterialize(t, mzDataset(append(first, second...)...))

	if len(pk.Excluded) != 0 {
		t.Fatalf("exclusions = %v, want none", mzReasons(pk))
	}
	if len(pk.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (one per minted counter)", len(pk.Attempts))
	}
	for i, want := range []uint64{1, 2} {
		if got := pk.Attempts[i].Key.AttemptID; got != want {
			t.Errorf("attempts[%d].Key.AttemptID = %d, want %d", i, got, want)
		}
		if got := pk.Attempts[i].EventID; got != mzEvent {
			t.Errorf("attempts[%d].EventID = %q, want %q (both attempts describe one reused event)", i, got, mzEvent)
		}
		if got := len(pk.Attempts[i].CommonInputSlice); got != 2 {
			t.Errorf("attempts[%d] slice = %v, want exactly its own two facts",
				i, mzObservationIDs(pk.Attempts[i].CommonInputSlice))
		}
	}
	if got := pk.Attempts[0].RoundIncarnationID; got != mzRound {
		t.Errorf("attempts[0].RoundIncarnationID = %q, want %q", got, mzRound)
	}
	if got := pk.Attempts[1].RoundIncarnationID; got != "round-incarnation-2" {
		t.Errorf("attempts[1].RoundIncarnationID = %q, want the second admission", got)
	}
}

// TestTheSameAttemptCounterInADifferentPoolInstanceIsADifferentAttempt pins the
// other half of the identity. The counter is per-pool and restarts with the
// process, so attempt 1 exists in every pool instance that ever ran. Dropping
// the pool from the key would fuse two unrelated decisions — each with its own
// terminal fact — into one inconsistent group.
func TestTheSameAttemptCounterInADifferentPoolInstanceIsADifferentAttempt(t *testing.T) {
	a := mzInPool("pool-instance-a", "round-a", mzDue(1, 1), mzDecided(2, 1, mzExecutedEnvelope()))
	b := mzInPool("pool-instance-b", "round-b", mzDue(3, 1), mzDecided(4, 1, mzExecutedEnvelope()))

	pk := mzMaterialize(t, mzDataset(append(a, b...)...))

	if len(pk.Excluded) != 0 {
		t.Fatalf("exclusions = %v, want none (two pools, two attempts, one ending each)", mzReasons(pk))
	}
	if len(pk.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 — the pool instance is part of the identity", len(pk.Attempts))
	}
	for i, wantPool := range []string{"pool-instance-a", "pool-instance-b"} {
		if got := pk.Attempts[i].Key.PoolInstanceID; got != wantPool {
			t.Errorf("attempts[%d].Key.PoolInstanceID = %q, want %q", i, got, wantPool)
		}
		if got := pk.Attempts[i].Key.AttemptID; got != 1 {
			t.Errorf("attempts[%d].Key.AttemptID = %d, want 1 (the counter restarts per pool)", i, got)
		}
		if got := pk.Attempts[i].Key.CollectorSessionID; got != mzSession {
			t.Errorf("attempts[%d].Key.CollectorSessionID = %q, want %q", i, got, mzSession)
		}
		if got := pk.Attempts[i].Key.CollectorEpoch; got != mzEpoch {
			t.Errorf("attempts[%d].Key.CollectorEpoch = %d, want %d", i, got, mzEpoch)
		}
	}
}

// ---------------------------------------------------------------------------
// B. The causal cut
// ---------------------------------------------------------------------------

// TestPlacementFactsShareTheAttemptIDButLandStrictlyAfterTheCausalCut is the
// most important test in this file.
//
// The placement facts of an attempt carry that attempt's id and are written
// AFTER its terminal envelope. If they ever entered CommonInputSlice they would
// become projectable inputs, and the outcome of a bet would be feeding the
// decision that placed it — a replay that then "agreed" with the record would
// be proving nothing at all. The cut is positional: the slice ends at the
// terminal fact, and everything after it is reachable only by [Score].
func TestPlacementFactsShareTheAttemptIDButLandStrictlyAfterTheCausalCut(t *testing.T) {
	pk := mzMaterialize(t, mzDataset(
		mzDue(1, 1),
		mzDecided(2, 1, mzExecutedEnvelope()),
		mzPlacement(3, 1, PhaseCallStarted),
		mzPlacement(4, 1, PhaseCallReturned),
	))
	a := mzOnlyAttempt(t, pk)

	if len(a.CommonInputSlice) != 2 {
		t.Fatalf("CommonInputSlice = %v, want only the due and terminal facts",
			mzObservationIDs(a.CommonInputSlice))
	}
	for _, r := range a.CommonInputSlice {
		if r.Kind == KindPlacement {
			t.Fatalf("placement fact %q is inside the common-input slice: a bet's own "+
				"outcome would feed the decision that made it", r.ObservationID)
		}
	}
	if a.TerminalIndex != len(a.CommonInputSlice)-1 {
		t.Errorf("TerminalIndex = %d, want %d (the terminal fact is always last)",
			a.TerminalIndex, len(a.CommonInputSlice)-1)
	}
	terminal := a.CommonInputSlice[a.TerminalIndex]
	if terminal.Payload.Phase != PhaseAutoDecided || terminal.Payload.DecisionEnvelope == nil {
		t.Errorf("terminal fact = phase %q envelope %v, want an AUTO_DECIDED fact carrying an envelope",
			terminal.Payload.Phase, terminal.Payload.DecisionEnvelope != nil)
	}
	if !a.SawDueFact || a.DueReason != "OK" {
		t.Errorf("SawDueFact = %v DueReason = %q, want true/OK — the opening fact survives here",
			a.SawDueFact, a.DueReason)
	}

	// The same facts must be present on the far side of the cut, where Score
	// (and only Score) can read them.
	if got := mzObservationIDs(a.PostDecision); len(got) != 2 {
		t.Fatalf("PostDecision = %v, want the two placement facts", got)
	}
	if a.PostDecision[0].Payload.Phase != PhaseCallStarted ||
		a.PostDecision[1].Payload.Phase != PhaseCallReturned {
		t.Errorf("PostDecision phases = %q,%q, want CALL_STARTED then CALL_RETURNED (causal order preserved)",
			a.PostDecision[0].Payload.Phase, a.PostDecision[1].Payload.Phase)
	}
	facts := ProjectSettlementFacts(a)
	if !facts.PlacementCallStarted || !facts.PlacementCallReturned || facts.PostDecisionFacts != 2 {
		t.Errorf("settlement facts = %+v, want both placement calls visible to Score", facts)
	}

	// And the projected case must be readable without them: its inputs come
	// from the terminal envelope alone.
	c, err := ProjectDecisionCase(a)
	if err != nil {
		t.Fatalf("ProjectDecisionCase: %v", err)
	}
	if !c.Eligibility.Eligible || !c.Eligibility.ExercisesPolicy {
		t.Fatalf("eligibility = %+v, want an eligible case that exercises the policy", c.Eligibility)
	}
	if c.CommonInputDigest != a.CommonInputDigest || c.Inputs.CommonInputDigest != a.CommonInputDigest {
		t.Errorf("case digests = %q/%q, want both tied to the slice digest %q",
			c.CommonInputDigest, c.Inputs.CommonInputDigest, a.CommonInputDigest)
	}
}

// ---------------------------------------------------------------------------
// C. No look-ahead
// ---------------------------------------------------------------------------

// mzCommonHalf renders everything about an attempt EXCEPT its post-decision
// facts, which are the one half later facts are allowed to reach.
func mzCommonHalf(t *testing.T, a AttemptKnowledge) string {
	t.Helper()
	a.PostDecision = nil
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal attempt: %v", err)
	}
	return string(b)
}

func mzAttemptWithID(t *testing.T, pk PairedKnowledge, id uint64) AttemptKnowledge {
	t.Helper()
	for _, a := range pk.Attempts {
		if a.Key.AttemptID == id {
			return a
		}
	}
	t.Fatalf("no attempt %d in reading (attempts %d, exclusions %v)", id, len(pk.Attempts), mzReasons(pk))
	return AttemptKnowledge{}
}

// TestAppendingLaterFactsCannotChangeAnEarlierAttemptsInputsOrDigest is the
// store-growth guard.
//
// An observation store is append-only and keeps growing: placements land after
// the decision, the next attempt lands after that, the round settles later
// still. A replay of a finished attempt must therefore be a function of the
// prefix that existed when the attempt ended — otherwise re-running the same
// analysis a minute later would silently answer a different question, and a
// stored scorecard's digest would stop identifying the inputs it was computed
// from.
func TestAppendingLaterFactsCannotChangeAnEarlierAttemptsInputsOrDigest(t *testing.T) {
	base := []SourceRecord{mzDue(1, 1), mzDecided(2, 1, mzExecutedEnvelope())}

	// A whole second attempt, on the next admission of the same round.
	secondAttempt := mzInPool(mzPool, "round-incarnation-2",
		mzDue(5, 2), mzDecided(6, 2, mzExecutedEnvelope()))

	grown := append([]SourceRecord(nil), base...)
	grown = append(grown,
		mzPlacement(3, 1, PhaseCallStarted),
		mzPlacement(4, 1, PhaseCallReturned),
	)
	grown = append(grown, secondAttempt...)
	// And the round's settlement.
	grown = append(grown, mzUserTerminal(7, 1))

	before := mzOnlyAttempt(t, mzMaterialize(t, mzDataset(base...)))
	grownReading := mzMaterialize(t, mzDataset(grown...))
	after := mzAttemptWithID(t, grownReading, 1)

	if len(grownReading.Attempts) != 2 {
		t.Fatalf("grown reading has %d attempts, want 2 — the fixture must actually have grown", len(grownReading.Attempts))
	}
	if before.CommonInputDigest == "" || len(before.CommonInputDigest) != 64 {
		t.Fatalf("CommonInputDigest = %q, want a 64-character sha256 hex digest", before.CommonInputDigest)
	}
	if after.CommonInputDigest != before.CommonInputDigest {
		t.Errorf("CommonInputDigest changed when later facts were appended:\n  before %s\n  after  %s",
			before.CommonInputDigest, after.CommonInputDigest)
	}
	if !reflect.DeepEqual(after.CommonInputSlice, before.CommonInputSlice) {
		t.Errorf("CommonInputSlice changed when later facts were appended:\n  before %v\n  after  %v",
			mzObservationIDs(before.CommonInputSlice), mzObservationIDs(after.CommonInputSlice))
	}
	if got, want := mzCommonHalf(t, after), mzCommonHalf(t, before); got != want {
		t.Errorf("attempt 1 is not byte-identical across the growth:\n  before %s\n  after  %s", want, got)
	}

	// The later facts did not vanish — they went where only Score may read
	// them. (Asserted by membership rather than by length: which KINDS of
	// later fact are grouped into an attempt is a separate concern from the
	// cut this test defends.)
	post := mzObservationIDs(after.PostDecision)
	for _, want := range []string{
		"obs-placement-CALL_STARTED-3",
		"obs-placement-CALL_RETURNED-4",
	} {
		if !mzHasString(post, want) {
			t.Errorf("PostDecision = %v, want it to contain %q", post, want)
		}
	}
}

// TestChangingAFactInsideTheCommonInputSliceChangesItsDigest is the other half
// of the digest contract: it has to be sensitive to the prefix it witnesses,
// or its stability under appends would be the stability of a constant.
//
// The digest deliberately covers each fact's IDENTITY plus the store's own row
// witness (ObservationSHA256) rather than re-deriving the row's contents — see
// digest.go. So a payload edit with an unchanged row witness is NOT expected to
// move it: in the store, altered bytes move the witness, and the reader checks
// that separately. The final sub-case pins that documented boundary so a future
// change to the encoding is a visible decision rather than a silent one.
func TestChangingAFactInsideTheCommonInputSliceChangesItsDigest(t *testing.T) {
	baseline := mzOnlyAttempt(t, mzMaterialize(t, mzDataset(
		mzDue(1, 1), mzDecided(2, 1, mzExecutedEnvelope()),
	))).CommonInputDigest

	tests := []struct {
		name       string
		mutate     func(due, terminal *SourceRecord)
		wantChange bool
	}{
		{
			name:       "the row witness of a fact inside the slice",
			mutate:     func(due, _ *SourceRecord) { due.ObservationSHA256 = "row-witness-tampered" },
			wantChange: true,
		},
		{
			name:       "the identity of a fact inside the slice",
			mutate:     func(due, _ *SourceRecord) { due.ObservationID = "obs-substituted" },
			wantChange: true,
		},
		{
			name:       "the event id a fact inside the slice describes",
			mutate:     func(due, _ *SourceRecord) { due.EventID = "twitch-event-2" },
			wantChange: true,
		},
		{
			name:       "the admission provenance of a fact inside the slice",
			mutate:     func(_ *SourceRecord, term *SourceRecord) { term.RoundCaptureGapCause = "SUBSCRIBED_MID_ROUND" },
			wantChange: true,
		},
		{
			name: "the terminal fact's payload while its row witness stays put",
			mutate: func(_ *SourceRecord, term *SourceRecord) {
				term.Payload.DecisionEnvelope.ChoiceAmount = mzPtrI64(999999)
			},
			wantChange: false, // by design: the store's row witness covers the bytes.
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			due := mzDue(1, 1)
			term := mzDecided(2, 1, mzExecutedEnvelope())
			tc.mutate(&due, &term)

			got := mzOnlyAttempt(t, mzMaterialize(t, mzDataset(due, term))).CommonInputDigest
			if changed := got != baseline; changed != tc.wantChange {
				t.Errorf("digest changed = %v, want %v (baseline %s, got %s)",
					changed, tc.wantChange, baseline, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// D. Exclusions
// ---------------------------------------------------------------------------

// TestUnreplayableFactsAndAttemptsAreExcludedWithTheReasonThatNamesThem walks
// the closed exclusion vocabulary.
//
// The property is that nothing disappears QUIETLY and nothing is rehabilitated
// by a plausible default: a fact this model cannot replay produces exactly one
// named exclusion and no attempt, so a caller counting cases can never mistake
// "refused" for "agreed".
func TestUnreplayableFactsAndAttemptsAreExcludedWithTheReasonThatNamesThem(t *testing.T) {
	foreignSessionFact := mzDecided(1, 1, mzExecutedEnvelope())
	foreignSessionFact.CollectorSessionID = "collector-session-B"

	foreignEpochFact := mzDecided(1, 1, mzExecutedEnvelope())
	foreignEpochFact.CollectorEpoch = mzEpoch + 1

	undecodable := mzDue(1, 1)
	undecodable.PayloadUndecodable = true

	wrongVersion := mzDecided(1, 1, mzExecutedEnvelope())
	wrongVersion.PayloadVersion = SupportedPayloadVersion + 1

	noAttemptID := mzFact(1, KindAutoDecision, PhaseAutoSkipped, 0)
	noAttemptID.Payload.ReasonCode = "NO_ROUND"

	crossedIncarnation := mzDue(1, 1)
	crossedIncarnation.RoundIncarnationID = "round-incarnation-2"

	legacySource := mzSource()
	legacySource.ProducerRevision = LegacyProducerRevisionPrefix + "|policy-0000000000000000000000000000000000000000"

	corruptSource := mzSource()
	corruptSource.SessionReading = readingIntegrityError
	corruptSource.SessionDetail = "ROW_DIGEST_MISMATCH"

	tests := []struct {
		name         string
		source       SourceProvenance
		records      []SourceRecord
		wantReason   string // "" means: no exclusion at all
		wantDetail   string // "" means: not asserted
		wantAttempts int
	}{
		{
			name:       "the scheduled timer fact for a round that no longer exists carries no attempt counter",
			source:     mzSource(),
			records:    []SourceRecord{noAttemptID},
			wantReason: ExclusionNoAttemptID,
			wantDetail: PhaseAutoSkipped + "/NO_ROUND",
		},
		{
			name:       "a row whose stored payload did not decode is unreadable, not empty",
			source:     mzSource(),
			records:    []SourceRecord{undecodable},
			wantReason: ExclusionPayloadUndecodable,
		},
		{
			name:       "a payload claiming a version this model is not bound to",
			source:     mzSource(),
			records:    []SourceRecord{wrongVersion},
			wantReason: ExclusionUnsupportedPayloadVersion,
		},
		{
			name:       "a fact belonging to another collector session",
			source:     mzSource(),
			records:    []SourceRecord{foreignSessionFact},
			wantReason: ExclusionForeignSession,
		},
		{
			name:       "a fact belonging to another collector epoch",
			source:     mzSource(),
			records:    []SourceRecord{foreignEpochFact},
			wantReason: ExclusionForeignSession,
		},
		{
			name:       "an attempt whose facts stop before its ending",
			source:     mzSource(),
			records:    []SourceRecord{mzDue(1, 1)},
			wantReason: ExclusionNoTerminalFact,
		},
		{
			name:   "a terminal-phase fact that carries no envelope under the envelope contract",
			source: mzSource(),
			records: []SourceRecord{
				mzDue(1, 1),
				mzTerminal(2, 1, PhaseAutoDecided, "PLACED", nil),
			},
			wantReason: ExclusionTerminalWithoutEnvelope,
		},
		{
			name:   "the same envelope-less ending under the pre-envelope producer contract",
			source: legacySource,
			records: []SourceRecord{
				mzDue(1, 1),
				mzTerminal(2, 1, PhaseAutoSkipped, "NOT_ELIGIBLE", nil),
			},
			wantReason: ExclusionLegacyProducerNoEnvelope,
		},
		{
			name:   "two terminal facts for one attempt",
			source: mzSource(),
			records: []SourceRecord{
				mzDue(1, 1),
				mzDecided(2, 1, mzExecutedEnvelope()),
				mzDecided(3, 1, mzExecutedEnvelope()),
			},
			wantReason: ExclusionMultipleTerminalFacts,
		},
		{
			name:   "facts of one attempt disagreeing about which admission of the round they describe",
			source: mzSource(),
			records: []SourceRecord{
				crossedIncarnation,
				mzDecided(2, 1, mzExecutedEnvelope()),
			},
			wantReason: ExclusionInconsistentRoundIncarnation,
		},
		{
			name:   "an outcome vector larger than the store could ever have persisted whole",
			source: mzSource(),
			records: []SourceRecord{
				mzDue(1, 1),
				mzDecided(2, 1, mzEnvelopeWithOutcomes(PinnedMaxOutcomes+1)),
			},
			wantReason: ExclusionOutcomeVectorOverCeiling,
		},
		{
			name:   "an outcome vector exactly at the ceiling is still replayable",
			source: mzSource(),
			records: []SourceRecord{
				mzDue(1, 1),
				mzDecided(2, 1, mzEnvelopeWithOutcomes(PinnedMaxOutcomes)),
			},
			wantReason:   "",
			wantAttempts: 1,
		},
		{
			name:   "a session the store classified as corrupt yields no cases at all",
			source: corruptSource,
			records: []SourceRecord{
				mzDue(1, 1),
				mzDecided(2, 1, mzExecutedEnvelope()),
			},
			wantReason: ExclusionSessionIntegrityError,
			wantDetail: "ROW_DIGEST_MISMATCH",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			pk := mzMaterialize(t, SourceDataset{Source: tc.source, Records: tc.records})

			if len(pk.Attempts) != tc.wantAttempts {
				t.Fatalf("attempts = %d, want %d (exclusions %v)", len(pk.Attempts), tc.wantAttempts, mzReasons(pk))
			}
			if tc.wantReason == "" {
				if len(pk.Excluded) != 0 {
					t.Fatalf("exclusions = %v, want none", mzReasons(pk))
				}
				return
			}
			if len(pk.Excluded) != 1 {
				t.Fatalf("exclusions = %v, want exactly one %s", mzReasons(pk), tc.wantReason)
			}
			if got := pk.Excluded[0].Reason; got != tc.wantReason {
				t.Errorf("exclusion reason = %s, want %s", got, tc.wantReason)
			}
			if tc.wantDetail != "" && pk.Excluded[0].Detail != tc.wantDetail {
				t.Errorf("exclusion detail = %q, want %q", pk.Excluded[0].Detail, tc.wantDetail)
			}
		})
	}
}

// TestASessionClassifiedCorruptIsRefusedEvenWhenItsFactsLookSound states the
// integrity rule separately from the table because it is the one exclusion
// that overrules perfectly readable rows. The individual facts below would
// materialize into a clean case under any other classification; the store's
// verdict about the session as a whole is what refuses them, and the anomaly
// is reported beside the exclusion rather than instead of it.
func TestASessionClassifiedCorruptIsRefusedEvenWhenItsFactsLookSound(t *testing.T) {
	sound := []SourceRecord{mzDue(1, 1), mzDecided(2, 1, mzExecutedEnvelope())}

	clean := mzMaterialize(t, mzDataset(sound...))
	if len(clean.Attempts) != 1 {
		t.Fatalf("control reading has %d attempts, want 1 — the facts must be sound on their own", len(clean.Attempts))
	}

	src := mzSource()
	src.SessionReading = readingIntegrityError
	corrupt := mzMaterialize(t, SourceDataset{Source: src, Records: sound})

	if len(corrupt.Attempts) != 0 {
		t.Errorf("attempts = %d, want 0 — a corrupt session yields no replayable case", len(corrupt.Attempts))
	}
	if !mzHasString(mzReasons(corrupt), ExclusionSessionIntegrityError) {
		t.Errorf("exclusions = %v, want %s", mzReasons(corrupt), ExclusionSessionIntegrityError)
	}
	if !mzHasString(corrupt.Anomalies, AnomalySessionIntegrityError) {
		t.Errorf("anomalies = %v, want %s reported beside the exclusion", corrupt.Anomalies, AnomalySessionIntegrityError)
	}
}

// ---------------------------------------------------------------------------
// E. Ordering
// ---------------------------------------------------------------------------

// TestRecordsOutOfCausalOrderAreRefusedRatherThanReordered pins the caller
// contract. Bounding an attempt's prefix BY POSITION only means anything over
// an ascending sequence: hand this stage a shuffled dataset and a placement
// fact could sort before the decision it followed, putting a result inside the
// common-input slice. Sorting the records here instead would paper over a
// broken reader, so the dataset is refused whole — no partial reading is
// returned for a caller to use by accident.
func TestRecordsOutOfCausalOrderAreRefusedRatherThanReordered(t *testing.T) {
	tests := []struct {
		name    string
		records []SourceRecord
	}{
		{
			name: "a sequence that goes backwards within one epoch",
			records: func() []SourceRecord {
				return []SourceRecord{mzDue(5, 1), mzDecided(4, 1, mzExecutedEnvelope())}
			}(),
		},
		{
			name: "an epoch that goes backwards",
			records: func() []SourceRecord {
				first := mzDue(1, 1)
				first.CollectorEpoch = mzEpoch + 1
				return []SourceRecord{first, mzDecided(2, 1, mzExecutedEnvelope())}
			}(),
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			pk, err := MaterializePairedKnowledge(mzDataset(tc.records...))
			if !errors.Is(err, ErrRecordsOutOfOrder) {
				t.Fatalf("error = %v, want ErrRecordsOutOfOrder", err)
			}
			if !reflect.DeepEqual(pk, PairedKnowledge{}) {
				t.Errorf("reading = %+v, want the zero value — a refused dataset yields no partial reading", pk)
			}
		})
	}
}

// TestDuplicateCausalPositionsAreReportedAsAnAnomalyWithoutLosingTheAttempt
// separates "impossible" from "suspicious". Two facts sharing a collector
// sequence do not break prefix bounding — the order is still non-descending —
// but they do mean the collector's causal counter is not the unique position it
// claims to be, and a reader must be told. The attempt is still materialized:
// silently dropping it would hide the anomaly it exists to report.
func TestDuplicateCausalPositionsAreReportedAsAnAnomalyWithoutLosingTheAttempt(t *testing.T) {
	started := mzPlacement(2, 1, PhaseCallStarted)
	returned := mzPlacement(2, 1, PhaseCallReturned)

	pk := mzMaterialize(t, mzDataset(
		mzDue(1, 1),
		mzDecided(2, 1, mzExecutedEnvelope()),
		started,
		returned,
	))

	if !mzHasString(pk.Anomalies, AnomalyDuplicateCausalPosition) {
		t.Fatalf("anomalies = %v, want %s", pk.Anomalies, AnomalyDuplicateCausalPosition)
	}
	// Three facts collide on sequence 2, but the anomaly qualifies the reading
	// once — a repeated entry would be noise, not information.
	seen := 0
	for _, a := range pk.Anomalies {
		if a == AnomalyDuplicateCausalPosition {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("%s appears %d times, want exactly once", AnomalyDuplicateCausalPosition, seen)
	}
	a := mzOnlyAttempt(t, pk)
	if len(a.CommonInputSlice) != 2 || len(a.PostDecision) != 2 {
		t.Errorf("slice/post = %v / %v, want the attempt materialized unchanged",
			mzObservationIDs(a.CommonInputSlice), mzObservationIDs(a.PostDecision))
	}
}

// ---------------------------------------------------------------------------
// F. Determinism
// ---------------------------------------------------------------------------

// TestMaterializePairedKnowledgeIsDeterministicAcrossRepeatedRuns pins the
// output order. Attempts are accumulated through a map, and Go randomizes map
// iteration on purpose, so a stage that leaked that iteration into its result
// would produce a different attempt order — and therefore a different stored
// artifact — on every run of the same data.
//
// The fixture is built so the first-seen order (pool-b/7, pool-a/2, pool-a/1)
// differs from the sorted key order, which is what gives the ordering assertion
// teeth: dropping the sort would leave the run-to-run comparison passing and
// this assertion failing.
func TestMaterializePairedKnowledgeIsDeterministicAcrossRepeatedRuns(t *testing.T) {
	var records []SourceRecord
	records = append(records, mzInPool("pool-instance-b", "round-b7", mzDue(1, 7), mzDecided(2, 7, mzExecutedEnvelope()))...)
	records = append(records, mzInPool("pool-instance-a", "round-a2", mzDue(3, 2), mzDecided(4, 2, mzExecutedEnvelope()))...)
	records = append(records, mzInPool("pool-instance-a", "round-a1", mzDue(5, 1), mzDecided(6, 1, mzExecutedEnvelope()))...)
	ds := mzDataset(records...)

	wantOrder := []AttemptKey{
		{CollectorEpoch: mzEpoch, CollectorSessionID: mzSession, PoolInstanceID: "pool-instance-a", AttemptID: 1},
		{CollectorEpoch: mzEpoch, CollectorSessionID: mzSession, PoolInstanceID: "pool-instance-a", AttemptID: 2},
		{CollectorEpoch: mzEpoch, CollectorSessionID: mzSession, PoolInstanceID: "pool-instance-b", AttemptID: 7},
	}

	first := mzMaterialize(t, ds)
	gotOrder := make([]AttemptKey, 0, len(first.Attempts))
	for _, a := range first.Attempts {
		gotOrder = append(gotOrder, a.Key)
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("attempt order = %+v, want %+v (ascending epoch, session, pool, counter)", gotOrder, wantOrder)
	}

	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal reading: %v", err)
	}
	for i := 0; i < 50; i++ {
		again := mzMaterialize(t, ds)
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("run %d differs from run 0", i+1)
		}
		againJSON, err := json.Marshal(again)
		if err != nil {
			t.Fatalf("marshal reading: %v", err)
		}
		if string(againJSON) != string(firstJSON) {
			t.Fatalf("run %d is not byte-identical to run 0:\n  %s\n  %s", i+1, firstJSON, againJSON)
		}
	}
}

// ---------------------------------------------------------------------------
// G. The causal split in ProjectDecisionCase
// ---------------------------------------------------------------------------

// TestAnExecutedCalculateStageWithoutAStoredChoiceIndexReconstructsTheNegativeOne
// pins the one place where an ABSENT stored value is a value.
//
// The pinned policy leaves its choice at -1 when no strategy branch selected
// anything, and the store drops a negative index rather than persisting it. So
// under an EXECUTED calculate stage, "no index" is not missing data — it is the
// policy's own "chose nothing", and reading it as absence (or worse, as 0)
// would compare the replay against outcome zero instead of against no outcome
// at all.
func TestAnExecutedCalculateStageWithoutAStoredChoiceIndexReconstructsTheNegativeOne(t *testing.T) {
	tests := []struct {
		name              string
		phase             string
		reason            string
		env               func() *SourceDecisionEnvelope
		wantIndexRecorded bool
		wantWasNegative   bool
		wantIndex         int
		wantHas           bool
	}{
		{
			name:   "an executed stage with no stored index is the policy's -1",
			phase:  PhaseAutoSkipped,
			reason: "NO_OUTCOME_CHOSEN",
			env: func() *SourceDecisionEnvelope {
				env := mzExecutedEnvelope()
				env.ChoiceIndex = nil
				env.ChoiceOutcomeID = ""
				return env
			},
			wantIndexRecorded: false,
			wantWasNegative:   true,
			wantIndex:         -1,
			wantHas:           true,
		},
		{
			name:   "a stored index is carried verbatim",
			phase:  PhaseAutoDecided,
			reason: "PLACED",
			env: func() *SourceDecisionEnvelope {
				env := mzExecutedEnvelope()
				env.ChoiceIndex = mzPtrInt(1)
				env.ChoiceOutcomeID = "outcome-pink"
				return env
			},
			wantIndexRecorded: true,
			wantWasNegative:   false,
			wantIndex:         1,
			wantHas:           true,
		},
		{
			name:              "an attempt that never reached the policy has no recorded index at all",
			phase:             PhaseAutoSkipped,
			reason:            "NOT_ELIGIBLE",
			env:               mzNotReachedEnvelope,
			wantIndexRecorded: false,
			wantWasNegative:   false,
			wantIndex:         0,
			wantHas:           false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := mzProject(t, tc.phase, tc.reason, tc.env())

			if c.Recorded.ChoiceIndexRecorded != tc.wantIndexRecorded {
				t.Errorf("ChoiceIndexRecorded = %v, want %v", c.Recorded.ChoiceIndexRecorded, tc.wantIndexRecorded)
			}
			if c.Recorded.ChoiceWasNegative != tc.wantWasNegative {
				t.Errorf("ChoiceWasNegative = %v, want %v", c.Recorded.ChoiceWasNegative, tc.wantWasNegative)
			}
			idx, has := recordedChoiceIndex(c.Recorded)
			if has != tc.wantHas || (has && idx != tc.wantIndex) {
				t.Errorf("recordedChoiceIndex = (%d, %v), want (%d, %v)", idx, has, tc.wantIndex, tc.wantHas)
			}
		})
	}
}

// TestAnAttemptThatExitedBeforeTheCalculateStageExercisesNoPolicyAndCarriesItsWitnessedExit
// pins the pre-decision exit.
//
// Such an attempt is perfectly readable and perfectly useless as evidence about
// the betting policy: the policy never ran. It must therefore say so
// (ExercisesPolicy false) and must NOT acquire invented inputs — no default
// strategy, no zero balance, no empty-but-present outcome vector — while the
// witnessed reason it exited for is carried through verbatim, labelled as the
// external input it is.
func TestAnAttemptThatExitedBeforeTheCalculateStageExercisesNoPolicyAndCarriesItsWitnessedExit(t *testing.T) {
	c := mzProject(t, PhaseAutoSkipped, "ROUND_SUPPRESSED", mzNotReachedEnvelope())

	if c.Inputs.ReachedDecision {
		t.Errorf("ReachedDecision = true, want false — the calculate stage was NOT_REACHED")
	}
	if c.Eligibility.ExercisesPolicy {
		t.Errorf("ExercisesPolicy = true, want false — this case is no evidence about the policy")
	}
	if c.Inputs.PreDecisionExit != "ROUND_SUPPRESSED" {
		t.Errorf("PreDecisionExit = %q, want the terminal fact's witnessed reason code", c.Inputs.PreDecisionExit)
	}
	if !c.Eligibility.Eligible || len(c.Eligibility.Reasons) != 0 {
		t.Errorf("eligibility = %+v, want an eligible case: a pre-policy exit is readable, just not policy evidence", c.Eligibility)
	}
	// Nothing may be manufactured for the stages that never ran.
	if c.Inputs.Settings != nil {
		t.Errorf("Settings = %+v, want nil — no default strategy may be invented", *c.Inputs.Settings)
	}
	if c.Inputs.BalancePresent || c.Inputs.OutcomesPresent || c.Inputs.RiskPresent {
		t.Errorf("balance/outcomes/risk present = %v/%v/%v, want all false",
			c.Inputs.BalancePresent, c.Inputs.OutcomesPresent, c.Inputs.RiskPresent)
	}
	if c.Observed.StealthAmount != nil {
		t.Errorf("Observed.StealthAmount = %d, want nil", *c.Observed.StealthAmount)
	}
	if c.Inputs.HealthState != HealthNotReached {
		t.Errorf("HealthState = %q, want %q echoed verbatim", c.Inputs.HealthState, HealthNotReached)
	}
	if c.Inputs.MinimumStake != PinnedMinimumStake {
		t.Errorf("MinimumStake = %d, want the pinned floor %d even on a pre-policy exit",
			c.Inputs.MinimumStake, PinnedMinimumStake)
	}
	// The producer's own account of the ending is a RESULT, and lives there.
	if c.Recorded.TerminalPhase != PhaseAutoSkipped || c.Recorded.TerminalReason != "ROUND_SUPPRESSED" {
		t.Errorf("recorded terminal = %q/%q, want AUTO_SKIPPED/ROUND_SUPPRESSED",
			c.Recorded.TerminalPhase, c.Recorded.TerminalReason)
	}
	if c.Recorded.CalculateStage != StageNotReached {
		t.Errorf("recorded CalculateStage = %q, want %q", c.Recorded.CalculateStage, StageNotReached)
	}
}

// TestTheObservedStealthRealizationIsProjectedOnlyForAStealthModeDecision
// bounds the single observed value that is allowed past the causal cut.
//
// The recorded pre-risk stake is a RESULT. It reaches Evaluate only as
// ObservedRealization, only to pin down which of four integer reductions a
// random draw produced, and only when stealth mode could have consumed a draw
// at all. Handing it over for a non-stealth decision would let the evaluator
// read the recorded stake while claiming to have derived it.
func TestTheObservedStealthRealizationIsProjectedOnlyForAStealthModeDecision(t *testing.T) {
	t.Run("stealth mode off hands the evaluator nothing", func(t *testing.T) {
		env := mzExecutedEnvelope()
		env.Settings.StealthMode = false
		env.ChoiceAmount = mzPtrI64(437)

		c := mzProject(t, PhaseAutoDecided, "PLACED", env)

		if c.Observed.StealthAmount != nil {
			t.Fatalf("Observed.StealthAmount = %d, want nil — a non-stealth replay must derive the stake itself",
				*c.Observed.StealthAmount)
		}
		// The value still exists where it belongs: as a comparison target.
		if !c.Recorded.ChoiceAmountRecorded || c.Recorded.ChoiceAmount != 437 {
			t.Errorf("recorded ChoiceAmount = (%d, %v), want (437, true)",
				c.Recorded.ChoiceAmount, c.Recorded.ChoiceAmountRecorded)
		}
	})

	t.Run("stealth mode on hands over a copy of the recorded pre-risk stake", func(t *testing.T) {
		env := mzExecutedEnvelope()
		env.Settings.StealthMode = true
		env.ChoiceAmount = mzPtrI64(437)

		c := mzProject(t, PhaseAutoDecided, "PLACED", env)

		if c.Observed.StealthAmount == nil {
			t.Fatalf("Observed.StealthAmount = nil, want the recorded pre-risk stake")
		}
		if *c.Observed.StealthAmount != 437 {
			t.Errorf("Observed.StealthAmount = %d, want 437", *c.Observed.StealthAmount)
		}
		if c.Observed.StealthAmount == env.ChoiceAmount {
			t.Errorf("Observed.StealthAmount aliases the source envelope; a case must own its values")
		}
	})

	t.Run("stealth mode on with no recorded stake pins nothing down", func(t *testing.T) {
		env := mzExecutedEnvelope()
		env.Settings.StealthMode = true
		env.ChoiceAmount = nil

		c := mzProject(t, PhaseAutoDecided, "PLACED", env)

		if c.Observed.StealthAmount != nil {
			t.Fatalf("Observed.StealthAmount = %d, want nil — there is no realization to condition on",
				*c.Observed.StealthAmount)
		}
		if !mzHasString(c.Eligibility.Reasons, IneligibleMissingChoiceAmount) {
			t.Errorf("eligibility reasons = %v, want %s", c.Eligibility.Reasons, IneligibleMissingChoiceAmount)
		}
	})
}

// TestDecisionInputsExposesNoRecordedResultField is a structural guard rather
// than a behavioural one.
//
// The causal cut is enforced by the TYPE Evaluate accepts: it is handed a
// DecisionInputs and nothing else, so a recorded choice, stake or clamp is
// unreachable to it as long as DecisionInputs has no field to carry one. A
// future edit that "just carries the recorded amount along for convenience"
// would silently dissolve that guarantee while every behavioural test kept
// passing — so the field set itself is pinned here, as a literal.
func TestDecisionInputsExposesNoRecordedResultField(t *testing.T) {
	want := []string{
		"Balance",
		"BalancePresent",
		"BetTotalPoints",
		"BetTotalUsers",
		"CommonInputDigest",
		"HealthReason",
		"HealthState",
		"MinimumStake",
		"Outcomes",
		"OutcomesPresent",
		"PreDecisionExit",
		"ReachedDecision",
		"RiskMaxStakePercent",
		"RiskPresent",
		"RiskReservePoints",
		"Settings",
	}

	typ := reflect.TypeOf(DecisionInputs{})
	got := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	sort.Strings(got)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("DecisionInputs fields =\n  %v\nwant\n  %v\n"+
			"A NEW field here widens what Evaluate may read. If it carries a recorded "+
			"RESULT (a choice, a stake, a clamp, a settlement) the causal cut is gone; "+
			"if it is a genuine input, add it to this literal deliberately.", got, want)
	}

	// And the results those inputs deliberately exclude are present on the
	// other side of the cut, so this is a routing guarantee and not simply an
	// absence of data.
	env := mzExecutedEnvelope()
	env.ChoiceIndex = mzPtrInt(1)
	env.ChoiceAmount = mzPtrI64(777)
	env.FinalAmount = mzPtrI64(999)
	env.StakeAllowed = mzPtrI64(999)
	c := mzProject(t, PhaseAutoDecided, "PLACED", env)

	if c.Recorded.ChoiceIndex != 1 || c.Recorded.ChoiceAmount != 777 || c.Recorded.FinalAmount != 999 {
		t.Fatalf("recorded results = index %d amount %d final %d, want 1/777/999 — "+
			"the values must reach the comparison targets",
			c.Recorded.ChoiceIndex, c.Recorded.ChoiceAmount, c.Recorded.FinalAmount)
	}
}

// ---------------------------------------------------------------------------
// H. Structural invariants
// ---------------------------------------------------------------------------

// TestEnvelopesBreakingTheProducersStageInvariantsAreRefusedAsInconsistent
// walks the invariants the pinned decision path guarantees by construction.
//
// A snapshot that breaks one of them did not come from the code this model
// replays. Reading it anyway would mean replaying a decision under assumptions
// that provably do not hold for it — e.g. treating a stake gate that "ran"
// without recorded risk inputs as if the missing inputs were zeros — so the
// case is refused by name instead.
func TestEnvelopesBreakingTheProducersStageInvariantsAreRefusedAsInconsistent(t *testing.T) {
	executed := func(mutate func(env *SourceDecisionEnvelope)) func() *SourceDecisionEnvelope {
		return func() *SourceDecisionEnvelope {
			env := mzExecutedEnvelope()
			mutate(env)
			return env
		}
	}
	notReached := func(mutate func(env *SourceDecisionEnvelope)) func() *SourceDecisionEnvelope {
		return func() *SourceDecisionEnvelope {
			env := mzNotReachedEnvelope()
			mutate(env)
			return env
		}
	}

	tests := []struct {
		name             string
		phase            string
		reason           string
		env              func() *SourceDecisionEnvelope
		wantInconsistent bool
	}{
		{
			name:  "a well-formed executed envelope is coherent",
			phase: PhaseAutoDecided, reason: "PLACED",
			env:              mzExecutedEnvelope,
			wantInconsistent: false,
		},
		{
			name:  "a well-formed pre-policy exit is coherent",
			phase: PhaseAutoSkipped, reason: "NOT_ELIGIBLE",
			env:              mzNotReachedEnvelope,
			wantInconsistent: false,
		},
		{
			name:  "settings promoted without calculate",
			phase: PhaseAutoDecided, reason: "PLACED",
			env:              executed(func(e *SourceDecisionEnvelope) { e.SettingsStage = StageNotReached }),
			wantInconsistent: true,
		},
		{
			name:  "skip promoted apart from calculate",
			phase: PhaseAutoDecided, reason: "PLACED",
			env:              executed(func(e *SourceDecisionEnvelope) { e.SkipStage = StageNotReached }),
			wantInconsistent: true,
		},
		{
			name:  "calculate not reached but the health gate ran",
			phase: PhaseAutoSkipped, reason: "NOT_ELIGIBLE",
			env:              notReached(func(e *SourceDecisionEnvelope) { e.HealthStage = HealthAllowed }),
			wantInconsistent: true,
		},
		{
			name:  "calculate not reached but the stake gate ran",
			phase: PhaseAutoSkipped, reason: "NOT_ELIGIBLE",
			env:              notReached(func(e *SourceDecisionEnvelope) { e.StakeStage = StageExecuted }),
			wantInconsistent: true,
		},
		{
			name:  "calculate executed but the health gate was never reached",
			phase: PhaseAutoDecided, reason: "PLACED",
			env: executed(func(e *SourceDecisionEnvelope) {
				e.HealthStage = HealthNotReached
				e.StakeStage = StageNotReached
			}),
			wantInconsistent: true,
		},
		{
			name:  "health denied but the stake gate ran anyway",
			phase: PhaseAutoSkipped, reason: "HEALTH_GATED",
			env:              executed(func(e *SourceDecisionEnvelope) { e.HealthStage = HealthDenied }),
			wantInconsistent: true,
		},
		{
			name:  "health denied returning before the stake gate",
			phase: PhaseAutoSkipped, reason: "HEALTH_GATED",
			env: executed(func(e *SourceDecisionEnvelope) {
				e.HealthStage = HealthDenied
				e.HealthReason = "TRANSPORT_UNHEALTHY"
				e.StakeStage = StageNotReached
				e.RiskMaxStakePercent, e.RiskReservePoints = nil, nil
				e.StakeAllowed, e.StakeLimit, e.ClampApplied, e.FinalAmount = nil, nil, nil, nil
			}),
			wantInconsistent: false,
		},
		{
			name:  "health allowed but the stake gate never ran",
			phase: PhaseAutoDecided, reason: "PLACED",
			env:              executed(func(e *SourceDecisionEnvelope) { e.StakeStage = StageNotReached }),
			wantInconsistent: true,
		},
		{
			name:  "an executed stake gate with no recorded max-stake percent",
			phase: PhaseAutoDecided, reason: "PLACED",
			env:              executed(func(e *SourceDecisionEnvelope) { e.RiskMaxStakePercent = nil }),
			wantInconsistent: true,
		},
		{
			name:  "an executed stake gate with no recorded reserve",
			phase: PhaseAutoDecided, reason: "PLACED",
			env:              executed(func(e *SourceDecisionEnvelope) { e.RiskReservePoints = nil }),
			wantInconsistent: true,
		},
		{
			name:  "an executed stake gate with no recorded allowance",
			phase: PhaseAutoDecided, reason: "PLACED",
			env:              executed(func(e *SourceDecisionEnvelope) { e.StakeAllowed = nil }),
			wantInconsistent: true,
		},
		{
			name:  "an executed stake gate with no recorded limit",
			phase: PhaseAutoDecided, reason: "PLACED",
			env:              executed(func(e *SourceDecisionEnvelope) { e.StakeLimit = nil }),
			wantInconsistent: true,
		},
		{
			name:  "an executed stake gate with no recorded clamp assignment",
			phase: PhaseAutoDecided, reason: "PLACED",
			env:              executed(func(e *SourceDecisionEnvelope) { e.ClampApplied = nil }),
			wantInconsistent: true,
		},
		{
			name:  "a reserve violation that still carried a post-gate stake",
			phase: PhaseAutoSkipped, reason: "RESERVE_VIOLATION",
			env:              executed(func(e *SourceDecisionEnvelope) { e.StakeReason = GateReserveViolation }),
			wantInconsistent: true,
		},
		{
			name:  "a reserve violation returning from inside the gate block",
			phase: PhaseAutoSkipped, reason: "RESERVE_VIOLATION",
			env: executed(func(e *SourceDecisionEnvelope) {
				e.StakeReason = GateReserveViolation
				e.FinalAmount = nil
			}),
			wantInconsistent: false,
		},
		{
			name:  "a non-reserve executed gate with no post-gate stake",
			phase: PhaseAutoDecided, reason: "PLACED",
			env:              executed(func(e *SourceDecisionEnvelope) { e.FinalAmount = nil }),
			wantInconsistent: true,
		},
		{
			name:  "a clamp applied under a gate that never clamped",
			phase: PhaseAutoDecided, reason: "PLACED",
			env:              executed(func(e *SourceDecisionEnvelope) { e.ClampApplied = mzPtrBool(true) }),
			wantInconsistent: true,
		},
		{
			name:  "a clamp applied under the percent gate",
			phase: PhaseAutoDecided, reason: "PLACED",
			env: executed(func(e *SourceDecisionEnvelope) {
				e.ClampApplied = mzPtrBool(true)
				e.StakeReason = GatePercent
				e.StakeLimit = mzPtrI64(40)
				e.StakeAllowed = mzPtrI64(40)
				e.FinalAmount = mzPtrI64(40)
			}),
			wantInconsistent: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := mzProject(t, tc.phase, tc.reason, tc.env())
			got := mzHasString(c.Eligibility.Reasons, IneligibleInconsistentStageStates)
			if got != tc.wantInconsistent {
				t.Errorf("%s present = %v, want %v (reasons: %v)",
					IneligibleInconsistentStageStates, got, tc.wantInconsistent, c.Eligibility.Reasons)
			}
			if tc.wantInconsistent && c.Eligibility.Eligible {
				t.Errorf("Eligible = true, want false — an incoherent snapshot must not be evaluated")
			}
		})
	}
}

// TestAStageStateOutsideTheStoresClosedVocabularyIsRefusedRatherThanGuessed
// covers the store's own escape hatch. When a stage state falls outside the
// closed vocabulary the store writes UNKNOWN, and UNKNOWN must never be quietly
// read as NOT_REACHED (which would claim the stage did not run) or as EXECUTED
// (which would claim it did).
func TestAStageStateOutsideTheStoresClosedVocabularyIsRefusedRatherThanGuessed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(env *SourceDecisionEnvelope)
	}{
		{"an unknown health verdict", func(e *SourceDecisionEnvelope) { e.HealthStage = ValueUnknown }},
		{"an unknown stake stage", func(e *SourceDecisionEnvelope) { e.StakeStage = ValueUnknown }},
		{"an unknown settings stage", func(e *SourceDecisionEnvelope) { e.SettingsStage = ValueUnknown }},
		{"an empty calculate stage", func(e *SourceDecisionEnvelope) { e.CalculateStage = "" }},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			env := mzExecutedEnvelope()
			tc.mutate(env)

			c := mzProject(t, PhaseAutoDecided, "PLACED", env)

			if !mzHasString(c.Eligibility.Reasons, IneligibleUnknownStageState) {
				t.Errorf("reasons = %v, want %s", c.Eligibility.Reasons, IneligibleUnknownStageState)
			}
			if c.Eligibility.Eligible {
				t.Errorf("Eligible = true, want false")
			}
		})
	}
}
