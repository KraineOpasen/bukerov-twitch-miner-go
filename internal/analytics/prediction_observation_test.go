package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// readPackageFile reads one of this package's own source files. Several
// invariants below are STRUCTURAL — "no mutating statement against the fact
// table", "exactly these eleven callers claim the gate" — and the source is
// the only place they can be asserted. `go test` runs with the package
// directory as the working directory.
func readPackageFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func sortStrings(in []string) { sort.Strings(in) }

// newObservationService stands up a Service over a private database with the
// collector started and bootstrapped, so a test can offer facts and read them
// back deterministically. The per-fact deadline is relaxed because a commit
// count must not depend on the host's fsync latency; the production value is
// pinned separately by TestObservationWriteDeadlineIsFiveMilliseconds.
func newObservationService(t *testing.T) (*Service, *SQLiteRepository) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	svc, err := NewService(db, filepath.Dir(path), 0)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	svc.observations.writeDeadline = 5 * time.Second
	t.Cleanup(func() { _ = svc.Close() })
	if err := svc.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	<-svc.observations.bootstrapped
	if svc.observations.disabled.Load() {
		t.Fatal("collector bootstrap failed")
	}
	return svc, svc.repo.(*SQLiteRepository)
}

// awaitCommitted waits until the collector has committed n facts.
func awaitCommitted(t *testing.T, svc *Service, n int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if svc.observations.committed.Load() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("only %d of %d facts committed (dropped=%d)",
		svc.observations.committed.Load(), n, svc.observations.dropped.Load())
}

func channelObservation(pool, channel, login, event string, phase string) PredictionObservation {
	return PredictionObservation{
		PoolInstanceID:               pool,
		RoutedChannelID:              channel,
		RoutedLogin:                  login,
		RetentionGroupOwnerChannelID: channel,
		RetentionGroupOwnerLogin:     login,
		// The producer supplies the local admission identity; the store never
		// derives one, so a fixture that wants a round has to name it.
		RoundIncarnationID:   "round:" + pool + ":" + event,
		EventID:              event,
		Kind:                 KindChannelEvent,
		SourceTopicType:      TopicTypePredictionsChannel,
		SourceMessageType:    MessageTypeEventCreated,
		ProducerAtMS:         1_700_000_000_000,
		ProducerTimeSource:   TimeSourceProducer,
		ReceivedAtMS:         1_700_000_000_001,
		ConnectionIndex:      2,
		ConnectionGeneration: 7,
		ConnectionSequence:   11,
		ConnectionKnown:      true,
		Payload:              ObservationPayload{Phase: phase, RoundState: "ACTIVE"},
	}
}

// TestObservationWriteDeadlineIsFiveMilliseconds pins the production per-fact
// budget: the only constructor sets exactly ObservationWriteDeadline, and that
// constant is 5 ms. A test may relax the field; production has no way to.
func TestObservationWriteDeadlineIsFiveMilliseconds(t *testing.T) {
	if ObservationWriteDeadline != 5*time.Millisecond {
		t.Fatalf("ObservationWriteDeadline = %v, want 5ms", ObservationWriteDeadline)
	}
	c := newObservationCollector(nil, nil, 0, time.Now)
	if c.writeDeadline != ObservationWriteDeadline {
		t.Fatalf("constructed collector deadline = %v, want %v", c.writeDeadline, ObservationWriteDeadline)
	}
	if cap(c.queue) != ObservationQueueCapacity || ObservationQueueCapacity != 512 {
		t.Fatalf("queue capacity = %d (const %d), want 512", cap(c.queue), ObservationQueueCapacity)
	}
}

// TestObservationRoundTrip is the behavioral seam: a fact offered to the
// Service is persisted with every column the schema promises, and reads back
// as the same typed projection.
func TestObservationRoundTrip(t *testing.T) {
	svc, repo := newObservationService(t)
	if err := repo.RecordPoints("streamer-a", 100, "WATCH"); err != nil {
		t.Fatal(err)
	}

	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "streamer-a", "event-1", "ROUND_CREATED"))
	awaitCommitted(t, svc, 1)

	got, err := repo.ObservationsBySession(context.Background(), svc.observations.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("read back %d facts, want 1", len(got))
	}
	rec := got[0]
	if rec.PoolInstanceID != "pool-1" || rec.RoutedChannelID != "chan-a" || rec.EventID != "event-1" {
		t.Fatalf("identity columns = %+v", rec)
	}
	if rec.RoutedStreamerID == 0 {
		t.Fatal("routed parent id was not resolved from the existing analytics row")
	}
	if rec.Kind != KindChannelEvent || rec.SourceTopicType != TopicTypePredictionsChannel || rec.SourceMessageType != MessageTypeEventCreated {
		t.Fatalf("source columns = %+v", rec)
	}
	if rec.ProducerTimeSource != TimeSourceProducer || rec.ProducerAtMS != 1_700_000_000_000 {
		t.Fatalf("producer time = %d/%s", rec.ProducerAtMS, rec.ProducerTimeSource)
	}
	if rec.ConnectionIndex != 2 || rec.ConnectionGeneration != 7 || rec.ConnectionSequence != 11 {
		t.Fatalf("connection provenance = %d/%d/%d", rec.ConnectionIndex, rec.ConnectionGeneration, rec.ConnectionSequence)
	}
	if rec.RoundIncarnationID == "" {
		t.Fatal("a fact with a retention-group owner must carry a round incarnation")
	}
	if rec.Payload.Phase != "ROUND_CREATED" || rec.Payload.RoundState != "ACTIVE" {
		t.Fatalf("payload = %+v", rec.Payload)
	}
	if rec.PayloadVersion != ObservationPayloadVersion {
		t.Fatalf("payload version = %d, want %d", rec.PayloadVersion, ObservationPayloadVersion)
	}
	if !strings.HasPrefix(rec.ObservationSHA256, "sha256:") || !strings.HasPrefix(rec.SourceFingerprint, "sha256:") {
		t.Fatalf("digests = %q / %q", rec.ObservationSHA256, rec.SourceFingerprint)
	}
	if rec.CollectorSequence != 1 || rec.CollectorEpoch != svc.observations.epoch.Load() {
		t.Fatalf("causal position = epoch %d seq %d", rec.CollectorEpoch, rec.CollectorSequence)
	}
}

// TestObservationsAreInsertOnlyAndOrdered proves the trail is immutable and
// causally ordered: sequences are strictly increasing in offer order, and the
// production code contains no UPDATE/REPLACE/upsert against the fact table.
func TestObservationsAreInsertOnlyAndOrdered(t *testing.T) {
	svc, repo := newObservationService(t)
	const n = 25
	for i := 0; i < n; i++ {
		svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "streamer-a", "event-"+itoa(int64(i)), "ROUND_CREATED"))
	}
	awaitCommitted(t, svc, n)

	got, err := repo.ObservationsBySession(context.Background(), svc.observations.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("read %d facts, want %d", len(got), n)
	}
	for i, rec := range got {
		if rec.CollectorSequence != int64(i+1) {
			t.Fatalf("fact %d has sequence %d, want %d — causal order is not total", i, rec.CollectorSequence, i+1)
		}
		if rec.EventID != "event-"+itoa(int64(i)) {
			t.Fatalf("fact %d is %s, want event-%d — offers were reordered", i, rec.EventID, i)
		}
	}

	// Immutability is structural: the fact path never issues a mutating
	// statement. Assert it against the source of the whole package.
	assertNoFactMutations(t)
}

func assertNoFactMutations(t *testing.T) {
	t.Helper()
	for _, file := range []string{"prediction_observation.go", "repository.go", "service.go"} {
		src := readPackageFile(t, file)
		lower := strings.ToLower(src)
		for _, forbidden := range []string{
			"update prediction_observations",
			"replace into prediction_observations",
			"insert or replace into prediction_observations",
			"on conflict(observation_id)",
		} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s mutates persisted observation facts (%q): the trail must be INSERT-only", file, forbidden)
			}
		}
	}
}

// TestObservationExactPairCollisionIsRejected proves the store refuses a
// second fact at the same causal position even if it is offered directly at
// the SQL level: the trail cannot silently gain two "first" facts.
func TestObservationExactPairCollisionIsRejected(t *testing.T) {
	svc, repo := newObservationService(t)
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "streamer-a", "event-1", "ROUND_CREATED"))
	awaitCommitted(t, svc, 1)

	epoch := svc.observations.epoch.Load()
	err := repo.AppendObservation(context.Background(),
		mustSanitize(t, channelObservation("pool-1", "chan-a", "streamer-a", "event-2", "ROUND_UPDATED")),
		svc.observations.sessionID, epoch, 1)
	if err == nil {
		t.Fatal("a colliding (epoch, sequence) pair was accepted")
	}
}

func mustSanitize(t *testing.T, in PredictionObservation) PredictionObservation {
	t.Helper()
	out, ok := sanitizeObservation(in, time.Now().UnixMilli())
	if !ok {
		t.Fatal("observation was rejected by sanitization")
	}
	return out
}

// TestObservationDigestIsStableAndCoversContent proves observation_sha256 is
// reproducible for identical content and changes when any covered field
// changes — which is what makes it usable as an integrity witness.
func TestObservationDigestIsStableAndCoversContent(t *testing.T) {
	base := mustSanitize(t, channelObservation("pool-1", "chan-a", "streamer-a", "event-1", "ROUND_CREATED"))
	payload, _ := marshalObservationPayload(base.Payload)
	inc := base.RoundIncarnationID

	digest := func(o PredictionObservation, seq int64, inc, pl string, routed, owner, retention interface{}) string {
		return observationDigest(o, "o:1", "s", 1, seq, inc, pl, routed, owner, retention)
	}

	d1 := digest(base, 1, inc, payload, int64(7), nil, int64(7))
	d2 := digest(base, 1, inc, payload, int64(7), nil, int64(7))
	if d1 != d2 {
		t.Fatal("digest is not reproducible for identical content")
	}

	changed := base
	changed.EventID = "event-2"
	if digest(changed, 1, inc, payload, int64(7), nil, int64(7)) == d1 {
		t.Fatal("digest ignores the event identity")
	}
	otherPayload, _ := marshalObservationPayload(ObservationPayload{Phase: "ROUND_UPDATED"})
	if digest(base, 1, inc, otherPayload, int64(7), nil, int64(7)) == d1 {
		t.Fatal("digest ignores the payload")
	}
	if digest(base, 2, inc, payload, int64(7), nil, int64(7)) == d1 {
		t.Fatal("digest ignores the causal position")
	}

	// Every OTHER persisted column is covered too — the review found the
	// digest silently omitted the resolved parent ids and the connection
	// provenance while its comment claimed to cover every column.
	if digest(base, 1, inc, payload, int64(8), nil, int64(7)) == d1 {
		t.Fatal("digest ignores the resolved routed parent id")
	}
	if digest(base, 1, inc, payload, int64(7), int64(9), int64(7)) == d1 {
		t.Fatal("digest ignores the resolved round-owner parent id")
	}
	if digest(base, 1, inc, payload, int64(7), nil, int64(8)) == d1 {
		t.Fatal("digest ignores the resolved retention-group parent id")
	}
	if digest(base, 1, inc, payload, nil, nil, int64(7)) == d1 {
		t.Fatal("digest cannot distinguish a NULL parent from a real one")
	}
	for _, mutate := range []func(o *PredictionObservation){
		func(o *PredictionObservation) { o.ConnectionIndex = 99 },
		func(o *PredictionObservation) { o.ConnectionGeneration = 99 },
		func(o *PredictionObservation) { o.ConnectionSequence = 99 },
		func(o *PredictionObservation) { o.ConnectionKnown = false },
	} {
		other := base
		mutate(&other)
		if digest(other, 1, inc, payload, int64(7), nil, int64(7)) == d1 {
			t.Fatal("digest ignores a persisted connection-provenance column")
		}
	}
	if digest(base, 1, "", payload, int64(7), nil, int64(7)) == d1 {
		t.Fatal("digest ignores the round incarnation")
	}
}

// TestObservationSanitizationIsClosedAndPrivate is the privacy proof. Every
// vocabulary is closed: an unrecognized value becomes UNKNOWN and the raw text
// is nowhere in the persisted projection — not in a column, not in the
// payload, not in the hash input.
func TestObservationSanitizationIsClosedAndPrivate(t *testing.T) {
	const secret = "oauth:abcdef0123456789-SECRET-TOKEN"
	in := PredictionObservation{
		PoolInstanceID:     "pool-1",
		Kind:               secret,
		SourceTopicType:    "community-points-user-v1." + secret,
		SourceMessageType:  secret,
		ProducerTimeSource: secret,
		// A producer time IS present, so the time source is a real enum
		// decision rather than the absent-time RECEIVER default.
		ProducerAtMS: 1_700_000_000_000,
		ReceivedAtMS: 10,
		Payload: ObservationPayload{
			Phase:      secret,
			RoundState: secret,
			Decision:   secret,
			ReasonCode: secret,
			ErrorClass: "graphql: " + secret,
			Outcomes: []ObservationOutcome{
				// Within every ceiling: this test is about the closed
				// vocabulary, and a ceiling breach would refuse the fact
				// before any of it could be examined (see
				// TestObservationPayloadCapsRefuseRatherThanShorten).
				{Color: secret, TotalPoints: 5, TotalUsers: 2, TopPredictorsExamined: 9},
			},
			Counters: map[string]int64{"stake": 10, "authToken": 42},
			Presence: map[string]string{"event": secret, "cookie": "PRESENT"},
		},
	}
	out, ok := sanitizeObservation(in, 10)
	if !ok {
		t.Fatal("a sanitizable observation was rejected")
	}
	if out.Kind != KindSourceUnknown {
		t.Fatalf("unrecognized kind became %q, want %q", out.Kind, KindSourceUnknown)
	}
	for name, got := range map[string]string{
		"topic":      out.SourceTopicType,
		"message":    out.SourceMessageType,
		"timeSource": out.ProducerTimeSource,
		"phase":      out.Payload.Phase,
		"roundState": out.Payload.RoundState,
		"decision":   out.Payload.Decision,
		"reasonCode": out.Payload.ReasonCode,
		"errorClass": out.Payload.ErrorClass,
		"color":      out.Payload.Outcomes[0].Color,
	} {
		if got != ValueUnknown {
			t.Fatalf("%s survived sanitization as %q, want %q", name, got, ValueUnknown)
		}
	}
	if out.Payload.Outcomes[0].TopPredictorsExamined != 9 {
		t.Fatalf("examined predictors = %d, want the count the producer reported",
			out.Payload.Outcomes[0].TopPredictorsExamined)
	}
	if _, ok := out.Payload.Counters["authToken"]; ok {
		t.Fatal("a counter outside the closed key set survived")
	}
	if out.Payload.Counters["stake"] != 10 {
		t.Fatal("an allowed counter was dropped")
	}
	if _, ok := out.Payload.Presence["cookie"]; ok {
		t.Fatal("a presence key outside the closed set survived")
	}
	if out.Payload.Presence["event"] != ValueUnknown {
		t.Fatalf("presence value = %q, want %q", out.Payload.Presence["event"], ValueUnknown)
	}

	// The secret appears nowhere in the persisted projection or its hashes.
	payload, renderable := marshalObservationPayload(out.Payload)
	if !renderable {
		t.Fatal("sanitized payload is not renderable")
	}
	blob := strings.Join([]string{
		payload,
		out.Kind, out.SourceTopicType, out.SourceMessageType, out.ProducerTimeSource,
		out.SourceFingerprint,
		observationDigest(out, "o:1", "s", 1, 1, "", payload, nil, nil, nil),
	}, "\x00")
	if strings.Contains(blob, "SECRET") || strings.Contains(blob, "oauth") {
		t.Fatalf("raw input survived into the persisted projection: %s", blob)
	}
}

// TestObservationPrivacyDeniedInputs pins the specific artefacts the contract
// forbids: a raw PubSub body, a Topic.String(), a transport EventFingerprint,
// a raw error and a predictor identity can never reach a persisted column,
// because no field of the typed producer contract accepts free text.
func TestObservationPrivacyDeniedInputs(t *testing.T) {
	// The typed producer contract has no free-text field at all: every string
	// field of ObservationPayload is validated against a closed vocabulary.
	forbidden := []string{
		`{"type":"event-created","data":{"event":{"id":"x"}}}`, // raw body
		"predictions-user-v1.123456789",                        // Topic.String()
		"sha256:deadbeef",                                      // transport EventFingerprint
		"Post \"https://gql.twitch.tv\": dial tcp: timeout",    // raw error
		"top_predictor_login",                                  // predictor identity
	}
	for _, raw := range forbidden {
		out, ok := sanitizeObservationPayload(ObservationPayload{
			Phase:      raw,
			RoundState: raw,
			Decision:   raw,
			ReasonCode: raw,
			ErrorClass: raw,
		})
		if !ok {
			t.Fatalf("a closed-vocabulary projection refused input %q; an unrecognized value "+
				"becomes UNKNOWN, it is not a ceiling breach", raw)
		}
		rendered, _ := marshalObservationPayload(out)
		if strings.Contains(rendered, raw) {
			t.Fatalf("forbidden input %q reached the persisted payload %s", raw, rendered)
		}
	}
}

// TestObservationPayloadCapsRefuseRatherThanShorten proves each payload
// ceiling AT the limit and at limit+1.
//
// The previous behaviour kept the first 64 of 70 outcomes and clamped an
// oversized predictor cohort, then committed the result as a successful fact.
// That fact is not a shortened truth, it is a falsehood: a reader sees a round
// whose aggregate cohort looks complete, and nothing distinguishes it from one
// that really had 64 outcomes. At the ceiling the fact is stored whole; past
// it, it is refused whole and counted as a loss.
func TestObservationPayloadCapsRefuseRatherThanShorten(t *testing.T) {
	outcomes := func(n int) []ObservationOutcome {
		out := make([]ObservationOutcome, n)
		for i := range out {
			out[i] = ObservationOutcome{Color: "BLUE", TotalPoints: int64(i)}
		}
		return out
	}

	t.Run("outcomes at the limit are kept whole", func(t *testing.T) {
		out, ok := sanitizeObservationPayload(ObservationPayload{
			Phase: "ROUND_UPDATED", Outcomes: outcomes(MaxObservationOutcomes)})
		if !ok {
			t.Fatalf("a round with exactly %d outcomes was refused", MaxObservationOutcomes)
		}
		if len(out.Outcomes) != MaxObservationOutcomes {
			t.Fatalf("outcomes = %d, want %d", len(out.Outcomes), MaxObservationOutcomes)
		}
		for i, o := range out.Outcomes {
			if o.Slot != i {
				t.Fatalf("outcome %d has slot %d — slots must be positional", i, o.Slot)
			}
		}
	})

	t.Run("one outcome past the limit refuses the fact", func(t *testing.T) {
		if _, ok := sanitizeObservationPayload(ObservationPayload{
			Phase: "ROUND_UPDATED", Outcomes: outcomes(MaxObservationOutcomes + 1)}); ok {
			t.Fatalf("a round with %d outcomes was accepted; the first %d would be stored as if "+
				"they were the whole cohort", MaxObservationOutcomes+1, MaxObservationOutcomes)
		}
	})

	t.Run("predictor cohort at the limit is kept", func(t *testing.T) {
		out, ok := sanitizeObservationPayload(ObservationPayload{Phase: "ROUND_UPDATED",
			Outcomes: []ObservationOutcome{{TopPredictorsExamined: MaxTopPredictorsExamined}}})
		if !ok {
			t.Fatalf("a cohort of exactly %d was refused", MaxTopPredictorsExamined)
		}
		if out.Outcomes[0].TopPredictorsExamined != MaxTopPredictorsExamined {
			t.Fatalf("cohort = %d, want %d", out.Outcomes[0].TopPredictorsExamined, MaxTopPredictorsExamined)
		}
	})

	t.Run("predictor cohort past the limit refuses the fact", func(t *testing.T) {
		if _, ok := sanitizeObservationPayload(ObservationPayload{Phase: "ROUND_UPDATED",
			Outcomes: []ObservationOutcome{{TopPredictorsExamined: MaxTopPredictorsExamined + 1}}}); ok {
			t.Fatalf("a cohort of %d was accepted and would be reported as %d",
				MaxTopPredictorsExamined+1, MaxTopPredictorsExamined)
		}
	})

	t.Run("an outcome slot past the ceiling refuses the fact", func(t *testing.T) {
		over := MaxObservationOutcomes
		if _, ok := sanitizeObservationPayload(ObservationPayload{
			Phase: "CALL_STARTED", OutcomeSlot: &over}); ok {
			t.Fatal("a slot past the outcome ceiling was accepted; it names an outcome that could " +
				"never have been stored")
		}
		// A negative slot is the ABSENCE of a chosen outcome, not a breach.
		absent := -1
		out, ok := sanitizeObservationPayload(ObservationPayload{
			Phase: "CALL_STARTED", OutcomeSlot: &absent})
		if !ok {
			t.Fatal("an absent outcome slot was treated as a ceiling breach")
		}
		if out.OutcomeSlot != nil {
			t.Fatalf("absent slot became %d", *out.OutcomeSlot)
		}
	})
}

// TestObservationStringCapIsEnforced proves an over-long enum candidate is
// rejected outright (it becomes UNKNOWN) rather than being truncated into
// something that might collide with a real member.
func TestObservationStringCapIsEnforced(t *testing.T) {
	long := strings.Repeat("A", MaxObservationString+1)
	if got := closedValue(long, observationPhases); got != ValueUnknown {
		t.Fatalf("over-long value became %q, want %q", got, ValueUnknown)
	}
	// An identifier is not shortened to fit. A truncated channel id, event id
	// or pool id names something else, and would then be stored, hashed and
	// erased as if it were the real one.
	if _, ok := boundedIdentifier(strings.Repeat("A", MaxObservationString)); !ok {
		t.Fatal("an identifier of exactly the cap was refused")
	}
	if got, ok := boundedIdentifier(long); ok {
		t.Fatalf("an over-long identifier was accepted as %q", got)
	}
	// And the whole fact goes with it.
	over := channelObservation("pool-1", strings.Repeat("c", MaxObservationString+1), "s", "e", "ROUND_CREATED")
	if _, ok := sanitizeObservation(over, 1); ok {
		t.Fatal("a fact with an over-long channel id was accepted")
	}
}

// TestObservationRejectsFactWithoutPoolProvenance proves pool_instance_id is
// genuinely required: a fact with no pool provenance is refused by
// sanitization, never stored with a NULL.
func TestObservationRejectsFactWithoutPoolProvenance(t *testing.T) {
	if _, ok := sanitizeObservation(PredictionObservation{Kind: KindChannelEvent, ReceivedAtMS: 1}, 1); ok {
		t.Fatal("a fact with no pool provenance was accepted")
	}
}

// TestObservationDropsHalfIdentity proves a parent login without its channel
// id is dropped rather than stored, so no row can exist that a channel-scoped
// privacy erasure cannot reach.
func TestObservationDropsHalfIdentity(t *testing.T) {
	out := mustSanitize(t, PredictionObservation{
		PoolInstanceID:     "pool-1",
		RoutedLogin:        "someone",
		Kind:               KindChannelEvent,
		ProducerTimeSource: TimeSourceReceiver,
		ReceivedAtMS:       1,
		Payload:            ObservationPayload{Phase: "ROUND_CREATED"},
	})
	if out.RoutedLogin != "" {
		t.Fatalf("routed login %q survived without its channel id", out.RoutedLogin)
	}
}

// TestObservationSessionReadings proves the four reader classifications: a
// live session is UNFINALIZED, a coherent finalized session is AS_FINALIZED, a
// session whose facts were removed afterwards is ADMINISTRATIVELY_TRUNCATED,
// and a self-contradicting row is an INTEGRITY_ERROR.
func TestObservationSessionReadings(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()
	epoch := svc.observations.epoch.Load()

	reading, found, err := repo.ReadObservationSession(ctx, epoch)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if reading.Reading != ReadingUnfinalized {
		t.Fatalf("live session reads %q, want %q", reading.Reading, ReadingUnfinalized)
	}

	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "streamer-a", "event-1", "ROUND_CREATED"))
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "streamer-a", "event-2", "ROUND_UPDATED"))
	awaitCommitted(t, svc, 2)
	svc.observations.Close()

	reading, _, err = repo.ReadObservationSession(ctx, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if reading.Reading != ReadingAsFinalized {
		t.Fatalf("finalized session reads %q (%s), want %q", reading.Reading, reading.Detail, ReadingAsFinalized)
	}
	if reading.Session.CloseState != SessionComplete {
		t.Fatalf("close state = %q, want %q", reading.Session.CloseState, SessionComplete)
	}
	if reading.FactsPresent != 2 || reading.Session.CommittedCount != 2 {
		t.Fatalf("present=%d committed=%d, want 2/2", reading.FactsPresent, reading.Session.CommittedCount)
	}

	// Remove a fact the way retention or an erasure would.
	if _, err := repo.db.Exec(`DELETE FROM prediction_observations WHERE collector_sequence = 1`); err != nil {
		t.Fatal(err)
	}
	reading, _, err = repo.ReadObservationSession(ctx, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if reading.Reading != ReadingAdministrativelyTruncated {
		t.Fatalf("truncated session reads %q, want %q", reading.Reading, ReadingAdministrativelyTruncated)
	}

	// A self-contradicting row is never treated as authoritative.
	if _, err := repo.db.Exec(`UPDATE prediction_observation_sessions SET committed_count = 0 WHERE collector_epoch = ?`, epoch); err != nil {
		t.Fatal(err)
	}
	reading, _, err = repo.ReadObservationSession(ctx, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if reading.Reading != ReadingIntegrityError {
		t.Fatalf("contradictory session reads %q, want %q", reading.Reading, ReadingIntegrityError)
	}
}

// TestObservationSessionIncompleteOnDrop proves the honesty contract: a
// session that dropped anything finalizes INCOMPLETE, so a reader can never
// mistake a lossy run for a whole one.
func TestObservationSessionIncompleteOnDrop(t *testing.T) {
	svc, repo := newObservationService(t)
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "streamer-a", "event-1", "ROUND_CREATED"))
	awaitCommitted(t, svc, 1)
	// A REAL drop: a fact with no pool provenance reserves a causal position
	// on the producer's call and is then refused by the writer. A bare
	// counter bump would leave committed+dropped disagreeing with the
	// positions actually reserved, which is itself an integrity failure.
	svc.RecordPredictionObservation(PredictionObservation{
		Kind: KindChannelEvent, ReceivedAtMS: 1, Payload: ObservationPayload{Phase: "ROUND_CREATED"}})
	awaitDropped(t, svc, 1)
	svc.observations.Close()

	reading, _, err := repo.ReadObservationSession(context.Background(), svc.observations.epoch.Load())
	if err != nil {
		t.Fatal(err)
	}
	if reading.Session.CloseState != SessionIncomplete {
		t.Fatalf("close state after a drop = %q, want %q", reading.Session.CloseState, SessionIncomplete)
	}
	if reading.Session.DroppedCount != 1 {
		t.Fatalf("dropped count = %d, want 1", reading.Session.DroppedCount)
	}
	// The facts it DID commit are still exactly present and authoritative.
	if reading.Reading != ReadingAsFinalized {
		t.Fatalf("reading = %q, want %q — an incomplete session's own facts are still exact", reading.Reading, ReadingAsFinalized)
	}
}

// TestObservationUnsettledObligationForcesIncomplete proves a privacy erasure
// permanently marks the session incomplete, because facts it had accepted were
// deliberately never written.
func TestObservationUnsettledObligationForcesIncomplete(t *testing.T) {
	svc, repo := newObservationService(t)
	svc.InvalidatePredictionObservationIdentity()
	svc.observations.Close()

	reading, _, err := repo.ReadObservationSession(context.Background(), svc.observations.epoch.Load())
	if err != nil {
		t.Fatal(err)
	}
	// An erasure forces INCOMPLETE — after it, the session's facts are
	// deliberately no longer the whole set it observed — but it must NOT be
	// reported as an unsettled obligation, which means "offered and never
	// written". Conflating the two made every ordinary streamer removal carry
	// a false explanation of why its session was incomplete.
	if reading.Session.CloseState != SessionIncomplete {
		t.Fatalf("session after an erasure = %q, want INCOMPLETE", reading.Session.CloseState)
	}
	if reading.Session.UnsettledObligationCount != 0 {
		t.Fatalf("an erasure was reported as %d unsettled obligations; it settled everything it dropped",
			reading.Session.UnsettledObligationCount)
	}
}

// TestObservationProducerShutdownUncertaintyForcesIncomplete proves a pool
// whose Close returned an error forces INCOMPLETE without altering anything
// else.
func TestObservationProducerShutdownUncertaintyForcesIncomplete(t *testing.T) {
	svc, repo := newObservationService(t)
	svc.NotePredictionProducerShutdownUncertain()
	svc.observations.Close()

	reading, _, err := repo.ReadObservationSession(context.Background(), svc.observations.epoch.Load())
	if err != nil {
		t.Fatal(err)
	}
	if reading.Session.CloseState != SessionIncomplete || reading.Session.ProducerShutdownUncertainCount != 1 {
		t.Fatalf("session = %q/%d, want INCOMPLETE/1",
			reading.Session.CloseState, reading.Session.ProducerShutdownUncertainCount)
	}
}

// TestObservationGenerationFenceDropsQueuedFacts proves a fact queued before a
// privacy erasure is DROPPED rather than committed: a queued observation can
// never resurrect an erased identity.
func TestObservationGenerationFenceDropsQueuedFacts(t *testing.T) {
	svc, repo := newObservationService(t)
	c := svc.observations

	// Stop the writer draining so a fact can sit in the queue across the fence.
	c.phase.Store(phaseRunning)
	stale := mustSanitize(t, channelObservation("pool-1", "chan-a", "streamer-a", "event-stale", "ROUND_CREATED"))
	stale.generation = c.generation.Load()
	svc.InvalidatePredictionObservationIdentity()

	before := c.dropped.Load()
	c.write(context.Background(), stale)
	if c.dropped.Load() != before+1 {
		t.Fatal("a fact stamped with a superseded generation was not dropped")
	}
	got, err := repo.ObservationsBySession(context.Background(), c.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("%d facts survived the generation fence", len(got))
	}
}

// TestObservationCaptureOffBeforeStartAndAfterClose proves the fence at both
// ends: a fact offered before the collector is running, or after Close has
// fenced intake, is refused, ACCOUNTED, and never written.
//
// Each end has its own counter, and neither is dropped_count. A fact refused
// before intake took no causal position, so counting it as a drop would make
// committed + dropped exceed the positions the session actually reserved —
// the exact shape a reader must treat as an integrity failure. Pre-intake
// losses and post-fence producers are separate reasons the session is not
// COMPLETE.
func TestObservationCaptureOffBeforeStartAndAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	svc, err := NewService(db, filepath.Dir(path), 0)
	if err != nil {
		t.Fatal(err)
	}
	svc.observations.writeDeadline = 5 * time.Second

	// Before Start: NewService constructs and migrates only.
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "streamer-a", "early", "ROUND_CREATED"))
	if svc.observations.preIntakeLosses.Load() != 1 {
		t.Fatal("a fact offered before Start was not accounted as a pre-intake loss")
	}
	if svc.observations.dropped.Load() != 0 {
		t.Fatal("a fact that never took a causal position was counted as a drop")
	}
	if svc.observations.epoch.Load() != 0 {
		t.Fatal("NewService allocated a collector session; Start must own that")
	}

	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	<-svc.observations.bootstrapped
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "streamer-a", "live", "ROUND_CREATED"))
	awaitCommitted(t, svc, 1)

	repo := svc.repo.(*SQLiteRepository)
	sessionID := svc.observations.sessionID
	if err := svc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// After Close the shared database is still open in this test, so a late
	// offer must be refused by the fence, not by the driver.
	postFence := svc.observations.postFenceProducers.Load()
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "streamer-a", "late", "ROUND_CREATED"))
	if svc.observations.postFenceProducers.Load() != postFence+1 {
		t.Fatal("a fact offered after Close was not recorded as a post-fence producer")
	}

	db2 := openPrivateDB(t, path)
	defer func() { _ = db2.Close() }()
	repo2, err := NewSQLiteRepository(db2, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo2.ObservationsBySession(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].EventID != "live" {
		t.Fatalf("persisted facts = %+v, want only the one offered while running", got)
	}
	_ = repo
}

// TestObservationNeverCreatesAStreamerRow proves parent resolution is
// LOOKUP-ONLY: an observation for an unknown login stores a NULL parent and
// does not invent a streamers row, so it can never resurrect a purged
// streamer.
func TestObservationNeverCreatesAStreamerRow(t *testing.T) {
	svc, repo := newObservationService(t)
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-x", "never-seen", "event-1", "ROUND_CREATED"))
	awaitCommitted(t, svc, 1)

	var n int
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM streamers WHERE name = ?`, "never-seen").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("recording an observation created a streamers row")
	}
	got, err := repo.ObservationsBySession(context.Background(), svc.observations.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RoutedStreamerID != 0 || got[0].RoutedChannelID != "chan-x" {
		t.Fatalf("fact = %+v, want a NULL parent with its channel identity intact", got)
	}
}

// TestObservationRoundIdentityIsTheProducersLocalAdmission proves the store
// CARRIES the producer's round identity and never derives one of its own.
//
// This replaces an assertion that a round incarnation is a hash of the channel
// and event id, and therefore identical for every fact of one Twitch event
// across every pool and every collector session. That is the wrong unit: two
// separate local admissions of one event — a re-admission after cleanup, a
// rebuilt pool, or two admissions racing — are two local rounds, and only the
// producer that admitted them can tell them apart. Deriving the id collapsed
// them into one retention unit and one erasure group.
//
// The store's remaining job is the schema invariant: a round incarnation and a
// retention-group owner exist together or not at all, in BOTH directions.
func TestObservationRoundIdentityIsTheProducersLocalAdmission(t *testing.T) {
	first := channelObservation("pool-1", "chan-a", "s", "event-1", "ROUND_CREATED")
	first.RoundIncarnationID = "round:pool-1:1"
	// The SAME Twitch event, admitted a second time by the same pool.
	second := channelObservation("pool-1", "chan-a", "s", "event-1", "ROUND_UPDATED")
	second.RoundIncarnationID = "round:pool-1:2"

	a := mustSanitize(t, first)
	b := mustSanitize(t, second)
	if a.RoundIncarnationID != "round:pool-1:1" || b.RoundIncarnationID != "round:pool-1:2" {
		t.Fatalf("the store rewrote the producer's round identity: %q, %q",
			a.RoundIncarnationID, b.RoundIncarnationID)
	}
	if a.RoundIncarnationID == b.RoundIncarnationID {
		t.Fatal("two local admissions of one event collapsed into one round")
	}
	// Companions of ONE admission agree because the producer gives them the
	// same id, not because anything here recomputes it.
	companion := channelObservation("pool-1", "chan-a", "s", "event-1", "ROUND_UPDATED")
	companion.RoundIncarnationID = "round:pool-1:1"
	if got := mustSanitize(t, companion).RoundIncarnationID; got != a.RoundIncarnationID {
		t.Fatalf("two facts of one admission disagree: %q vs %q", got, a.RoundIncarnationID)
	}

	// The biconditional, both ways.
	noOwner := first
	noOwner.RetentionGroupOwnerChannelID = ""
	noOwner.RetentionGroupOwnerLogin = ""
	if got := mustSanitize(t, noOwner); got.RoundIncarnationID != "" {
		t.Fatalf("a fact with no retention-group owner kept round incarnation %q", got.RoundIncarnationID)
	}
	noRound := first
	noRound.RoundIncarnationID = ""
	got := mustSanitize(t, noRound)
	if got.RetentionGroupOwnerChannelID != "" || got.RetentionGroupOwnerLogin != "" {
		t.Fatalf("a fact with no round kept retention-group owner %q/%q",
			got.RetentionGroupOwnerChannelID, got.RetentionGroupOwnerLogin)
	}
	if got.RoutedChannelID == "" {
		t.Fatal("dropping the retention group also dropped the routed identity; " +
			"a privacy erasure would no longer reach this fact")
	}
}

// TestObservationSourceFingerprintIsNotTheTransportFingerprint proves P1 uses
// its OWN digest over the sanitized source identity — it is derived only from
// closed, sanitized fields, so it can never carry raw frame content the way
// the transport EventFingerprint does.
func TestObservationSourceFingerprintIsNotTheTransportFingerprint(t *testing.T) {
	a := mustSanitize(t, channelObservation("pool-1", "chan-a", "s", "event-1", "ROUND_CREATED"))
	b := a
	b.PoolInstanceID = "pool-9"
	b.ReceivedAtMS = a.ReceivedAtMS + 5000
	b.ConnectionSequence = 999
	if observationSourceFingerprint(a) != observationSourceFingerprint(b) {
		t.Fatal("the source fingerprint depends on transport-local detail; it must digest the SOURCE identity only")
	}
	c := a
	c.EventID = "event-2"
	if observationSourceFingerprint(a) == observationSourceFingerprint(c) {
		t.Fatal("the source fingerprint ignores the event identity")
	}
	// Two deliveries of one source event share a fingerprint — which is why
	// the column is deliberately not unique.
	if observationSourceFingerprint(a) != observationSourceFingerprint(b) {
		t.Fatal("a re-delivery must be recognizable by fingerprint")
	}
}

// TestObservationStoreStatsMeasuresCaps proves the store's shape is
// measurable without reading any fact content, which is how the collector
// enforces the store caps at bootstrap.
func TestObservationStoreStatsMeasuresCaps(t *testing.T) {
	svc, repo := newObservationService(t)
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "s", "event-1", "ROUND_CREATED"))
	awaitCommitted(t, svc, 1)

	st, err := repo.ObservationStoreStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Rows != 1 || st.Sessions != 1 || st.Bytes <= 0 {
		t.Fatalf("store stats = %+v, want 1 row / 1 session / non-zero bytes", st)
	}
	if MaxStoreRows != 262144 || MaxStoreBytes != 1<<30 || MaxStoreSessions != 4096 {
		t.Fatalf("store caps drifted: rows=%d bytes=%d sessions=%d", MaxStoreRows, MaxStoreBytes, MaxStoreSessions)
	}
	if MaxRoundRows != 128 || MaxRoundBytes != 1<<20 ||
		MaxSessionRows != 65536 || MaxSessionBytes != 256<<20 ||
		MaxDeletionIdentityRows != 4096 || MaxDeletionIdentityBytes != 16<<20 ||
		MaxProvedIdentityUnionRows != 8192 || MaxProvedIdentityUnionBytes != 32<<20 {
		t.Fatal("a frozen bound drifted from the contract")
	}
}

// TestObservationProducerRevisionIsPinned proves every session is stamped with
// the exact producer contract the rows were written under, and that a session
// written under a different contract is never read as authoritative.
func TestObservationProducerRevisionIsPinned(t *testing.T) {
	if ObservationProducerRevision != "obs-v1|policy-0f98c316a8bcc24e055e2a0006ca6f96d1ff3a42" {
		t.Fatalf("producer revision drifted: %q", ObservationProducerRevision)
	}
	svc, repo := newObservationService(t)
	svc.observations.Close()

	var rev string
	if err := repo.db.QueryRow(`SELECT producer_revision FROM prediction_observation_sessions WHERE collector_epoch = ?`,
		svc.observations.epoch.Load()).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	if rev != ObservationProducerRevision {
		t.Fatalf("stored revision = %q, want %q", rev, ObservationProducerRevision)
	}

	// A session written under a DIFFERENT producer contract is NOT an
	// integrity failure: its rows are exactly what that contract wrote.
	// Treating it as one (as this originally did) would make every session
	// unreadable the moment the revision is bumped, which destroys the whole
	// point of an append-only trail across an upgrade. The reading stands and
	// the caller is told which contract's invariants apply.
	if _, err := repo.db.Exec(`UPDATE prediction_observation_sessions SET producer_revision = 'obs-v0|other' WHERE collector_epoch = ?`,
		svc.observations.epoch.Load()); err != nil {
		t.Fatal(err)
	}
	reading, _, err := repo.ReadObservationSession(context.Background(), svc.observations.epoch.Load())
	if err != nil {
		t.Fatal(err)
	}
	if reading.Reading != ReadingAsFinalized {
		t.Fatalf("foreign-contract session reads %q, want %q", reading.Reading, ReadingAsFinalized)
	}
	if !strings.Contains(reading.Detail, "different observation contract") {
		t.Fatalf("a foreign-contract session must say so; detail = %q", reading.Detail)
	}
	if reading.Session.ProducerRevision != "obs-v0|other" {
		t.Fatalf("the reading lost the contract it was written under: %q", reading.Session.ProducerRevision)
	}

	// A genuinely self-contradicting row IS still an integrity error. (A
	// negative counter cannot be used to demonstrate it — the schema's own
	// CHECK already refuses that — so use a session claiming to have
	// committed fewer facts than are actually present.)
	if _, err := repo.db.Exec(`INSERT INTO prediction_observations
		(observation_id, collector_session_id, collector_epoch, collector_sequence,
		 pool_instance_id, kind, producer_time_source, received_at_ms,
		 payload_version, payload_json, observation_sha256)
		VALUES ('o-extra', ?, ?, 9999, 'pool', ?, ?, 1, 1, '{"phase":"UNKNOWN"}', 'sha256:x')`,
		svc.observations.sessionID, svc.observations.epoch.Load(), KindChannelEvent, TimeSourceReceiver); err != nil {
		t.Fatal(err)
	}
	reading, _, err = repo.ReadObservationSession(context.Background(), svc.observations.epoch.Load())
	if err != nil {
		t.Fatal(err)
	}
	if reading.Reading != ReadingIntegrityError {
		t.Fatalf("more facts present than committed reads %q, want %q", reading.Reading, ReadingIntegrityError)
	}
}

// TestObservationKindsAreExactlyNine pins the closed kind set to the nine
// families the contract defines, in schema order.
func TestObservationKindsAreExactlyNine(t *testing.T) {
	want := []string{
		"source_unknown", "channel_event", "schedule_decision", "auto_decision",
		"manual_control", "placement", "user_prediction_made", "user_terminal",
		"round_cleanup",
	}
	if !reflect.DeepEqual(observationKinds, want) {
		t.Fatalf("observation kinds = %v, want %v", observationKinds, want)
	}
	// Every kind is accepted by the schema's CHECK.
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	if _, err := NewSQLiteRepository(db, filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	for i, kind := range observationKinds {
		if _, err := db.Exec(`INSERT INTO prediction_observations
			(observation_id, collector_session_id, collector_epoch, collector_sequence,
			 pool_instance_id, kind, producer_time_source, received_at_ms,
			 payload_version, payload_json, observation_sha256)
			VALUES (?, 's', 1, ?, 'pool', ?, ?, 1, 1, '{"phase":"UNKNOWN"}', 'sha256:x')`,
			"k"+itoa(int64(i)), int64(i), kind, TimeSourceReceiver); err != nil {
			t.Fatalf("kind %q rejected by the schema: %v", kind, err)
		}
	}
}

// TestObservationEraseByIdentity proves the asymmetric erasure contract: a
// retention-group-owner match removes the WHOLE round, a routed-only match
// removes ONLY the matching fact, and a round_owner match never expands
// deletion.
func TestObservationEraseByIdentity(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()

	// A round owned by chan-victim, with two facts.
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-victim", "victim", "round-1", "ROUND_CREATED"))
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-victim", "victim", "round-1", "ROUND_UPDATED"))
	// A THIRD fact of that same round, routed through a DIFFERENT channel.
	// Only whole-round expansion can reach it: the routed selector never
	// names chan-victim for this row. Without it the test cannot tell
	// whole-round erasure from routed-only erasure.
	roundMemberRoutedElsewhere := channelObservation("pool-1", "chan-bystander", "bystander", "round-1", "ROUND_UPDATED")
	roundMemberRoutedElsewhere.RetentionGroupOwnerChannelID = "chan-victim"
	roundMemberRoutedElsewhere.RetentionGroupOwnerLogin = "victim"
	svc.RecordPredictionObservation(roundMemberRoutedElsewhere)
	// A fact merely ROUTED through chan-victim, belonging to another channel's round.
	crossRouted := channelObservation("pool-1", "chan-victim", "victim", "round-2", "ROUND_CREATED")
	crossRouted.RetentionGroupOwnerChannelID = "chan-other"
	crossRouted.RetentionGroupOwnerLogin = "other"
	svc.RecordPredictionObservation(crossRouted)
	// A fact that only NAMES chan-victim as the round owner: provenance only.
	provenance := channelObservation("pool-1", "chan-third", "third", "round-3", "ROUND_CREATED")
	provenance.RoundOwnerChannelID = "chan-victim"
	provenance.RoundOwnerLogin = "victim"
	provenance.RetentionGroupOwnerChannelID = "chan-third"
	provenance.RetentionGroupOwnerLogin = "third"
	svc.RecordPredictionObservation(provenance)
	awaitCommitted(t, svc, 5)

	var removed int64
	if err := repo.db.WithTx(ctx, func(tx *sql.Tx) error {
		var e error
		removed, e = repo.EraseObservationsForIdentityTx(tx, ObservationIdentity{ChannelID: "chan-victim", Login: "victim"})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// Three whole-round facts (including the one routed through a different
	// channel, reachable ONLY by whole-round expansion) plus the one
	// routed-only fact = 4. The provenance fact survives: round_owner never
	// expands deletion.
	if removed != 4 {
		t.Fatalf("erased %d facts, want 4", removed)
	}
	left, err := repo.ObservationsBySession(ctx, svc.observations.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].EventID != "round-3" {
		t.Fatalf("surviving facts = %+v, want only the round_owner-provenance fact", left)
	}
	if left[0].RoundOwnerChannelID != "chan-victim" {
		t.Fatalf("the surviving fact lost its provenance: %+v", left[0])
	}
}

// TestObservationEraseByProvedParentFallback proves the empty-channel case:
// with no channel id, erasure falls back to the PROVED analytics parent id of
// the login — never to a guess.
func TestObservationEraseByProvedParentFallback(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()
	if err := repo.RecordPoints("proved", 10, "WATCH"); err != nil {
		t.Fatal(err)
	}
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-p", "proved", "round-1", "ROUND_CREATED"))
	awaitCommitted(t, svc, 1)

	var removed int64
	if err := repo.db.WithTx(ctx, func(tx *sql.Tx) error {
		var e error
		removed, e = repo.EraseObservationsForIdentityTx(tx, ObservationIdentity{Login: "proved"})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("erased %d facts via the proved parent, want 1", removed)
	}

	// A login with no analytics row and no channel proves nothing, so nothing
	// is erased — the erasure never guesses.
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-q", "unproved", "round-2", "ROUND_CREATED"))
	awaitCommitted(t, svc, 2)
	if err := repo.db.WithTx(ctx, func(tx *sql.Tx) error {
		var e error
		removed, e = repo.EraseObservationsForIdentityTx(tx, ObservationIdentity{Login: "unproved"})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("erased %d facts for an unprovable identity, want 0", removed)
	}
}

// TestObservationPruneUnitIsBounded proves retention removes exactly one
// bounded unit per transaction, never touches the active epoch, and never
// automatically removes a crash-left OPEN session.
func TestObservationPruneUnitIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	repo, err := NewSQLiteRepository(db, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// An old, crash-left OPEN session with a whole round, plus a NULL-round
	// batch, plus a fact in the "active" epoch.
	oldEpoch, err := repo.OpenObservationSession(ctx, "old-session", 1)
	if err != nil {
		t.Fatal(err)
	}
	activeEpoch, err := repo.OpenObservationSession(ctx, "active-session", 2)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(epoch, seq int64, id, incarnation, channel string, at int64) {
		t.Helper()
		var inc, ch interface{}
		if incarnation != "" {
			inc, ch = incarnation, channel
		}
		if _, err := db.Exec(`INSERT INTO prediction_observations
			(observation_id, collector_session_id, collector_epoch, collector_sequence,
			 pool_instance_id, round_incarnation_id, retention_group_owner_channel_id,
			 kind, producer_time_source, received_at_ms, payload_version, payload_json, observation_sha256)
			VALUES (?, 's', ?, ?, 'pool', ?, ?, ?, ?, ?, 1, '{"phase":"UNKNOWN"}', 'sha256:x')`,
			id, epoch, seq, inc, ch, KindChannelEvent, TimeSourceReceiver, at); err != nil {
			t.Fatal(err)
		}
	}
	insert(oldEpoch, 1, "r1", "round:aaa", "chan-a", 100)
	insert(oldEpoch, 2, "r2", "round:aaa", "chan-a", 150)
	for i := 0; i < observationPruneUnit+20; i++ {
		insert(oldEpoch, int64(100+i), "n"+itoa(int64(i)), "", "", 120)
	}
	insert(activeEpoch, 1, "active", "round:bbb", "chan-b", 100)
	// A crash-left OPEN session's FACTS are protected too, so finalize the old
	// session: this test is about the bounded prune, not about that guard
	// (TestObservationPruneSparesOpenSessionFacts covers it).
	if applied, err := repo.FinalizeObservationSession(ctx, oldEpoch,
		ObservationAccounting{Committed: int64(observationPruneUnit + 22)}, 200); err != nil || !applied {
		t.Fatalf("finalize old session: applied=%v err=%v", applied, err)
	}

	// Unit 1: the whole eligible round.
	n, err := repo.PruneObservationUnit(ctx, 1000, activeEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("first prune removed %d rows, want the whole 2-fact round", n)
	}
	// Unit 2: a bounded NULL-round batch, never more than the unit size.
	n, err = repo.PruneObservationUnit(ctx, 1000, activeEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if n != observationPruneUnit {
		t.Fatalf("second prune removed %d rows, want the %d-row bound", n, observationPruneUnit)
	}

	// The active epoch is never touched.
	var active int
	if err := db.QueryRow(`SELECT COUNT(*) FROM prediction_observations WHERE collector_epoch = ?`, activeEpoch).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("the active epoch lost %d facts to retention", 1-active)
	}

	// Drain the rest, then confirm the crash-left OPEN session still exists.
	for i := 0; i < 10; i++ {
		if n, err := repo.PruneObservationUnit(ctx, 1000, activeEpoch); err != nil {
			t.Fatal(err)
		} else if n == 0 {
			break
		}
	}
	// The ACTIVE session is still never swept, even once it is factless.
	var activeSessions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM prediction_observation_sessions WHERE collector_epoch = ?`, activeEpoch).Scan(&activeSessions); err != nil {
		t.Fatal(err)
	}
	if activeSessions != 1 {
		t.Fatal("the active session row was pruned")
	}
}

// TestObservationPruneSparesOpenSessionFacts proves a crash-left OPEN session
// is protected WHOLE — its row and its facts. An independent review found the
// facts were prunable while only the session row was guarded, which would
// leave a session claiming committed facts that are gone: that reads as an
// integrity error rather than as the unclean shutdown it actually was.
func TestObservationPruneSparesOpenSessionFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	repo, err := NewSQLiteRepository(db, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	crashed, err := repo.OpenObservationSession(ctx, "crashed", 1) // never finalized
	if err != nil {
		t.Fatal(err)
	}
	activeEpoch, err := repo.OpenObservationSession(ctx, "active", 2)
	if err != nil {
		t.Fatal(err)
	}
	// One whole round and one NULL-round fact, both long past any cutoff.
	for _, f := range []struct {
		id, inc, ch string
		seq         int64
	}{
		{"c1", "round:crashed", "chan-c", 1},
		{"c2", "round:crashed", "chan-c", 2},
		{"c3", "", "", 3},
	} {
		var inc, ch interface{}
		if f.inc != "" {
			inc, ch = f.inc, f.ch
		}
		if _, err := db.Exec(`INSERT INTO prediction_observations
			(observation_id, collector_session_id, collector_epoch, collector_sequence,
			 pool_instance_id, round_incarnation_id, retention_group_owner_channel_id,
			 kind, producer_time_source, received_at_ms, payload_version, payload_json, observation_sha256)
			VALUES (?, 'crashed', ?, ?, 'pool', ?, ?, ?, ?, 1, 1, '{"phase":"UNKNOWN"}', 'sha256:x')`,
			f.id, crashed, f.seq, inc, ch, KindChannelEvent, TimeSourceReceiver); err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < 5; i++ {
		if n, err := repo.PruneObservationUnit(ctx, 1_000_000, activeEpoch); err != nil {
			t.Fatal(err)
		} else if n != 0 {
			t.Fatalf("retention removed %d facts of a crash-left OPEN session", n)
		}
	}
	if got := countRows(t, repo, `SELECT COUNT(*) FROM prediction_observations`); got != 3 {
		t.Fatalf("%d of the crashed session's 3 facts survived", got)
	}
	var open int
	if err := db.QueryRow(`SELECT COUNT(*) FROM prediction_observation_sessions WHERE collector_epoch = ? AND close_state = 'OPEN'`, crashed).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 1 {
		t.Fatal("the crash-left OPEN session row was pruned; it is the only evidence of an unclean shutdown")
	}

	// Once it is finalized, its facts become ordinary retention candidates.
	if applied, err := repo.FinalizeObservationSession(ctx, crashed, ObservationAccounting{Committed: 3, Dropped: 1}, 2); err != nil || !applied {
		t.Fatalf("finalize: applied=%v err=%v", applied, err)
	}
	removed := int64(0)
	for i := 0; i < 5; i++ {
		n, err := repo.PruneObservationUnit(ctx, 1_000_000, activeEpoch)
		if err != nil {
			t.Fatal(err)
		}
		removed += n
		if n == 0 {
			break
		}
	}
	if removed < 3 {
		t.Fatalf("after finalization retention removed %d rows, want at least the 3 facts", removed)
	}
	if got := countRows(t, repo, `SELECT COUNT(*) FROM prediction_observations WHERE collector_epoch = ?`, crashed); got != 0 {
		t.Fatalf("%d facts of the finalized session survived retention", got)
	}
}

// insertObservationRow writes one fact straight through the driver, bypassing
// the collector, so a retention test can build an exact table state.
func insertObservationRow(t *testing.T, db *database.DB, epoch, seq int64, obsID, pool, incarnation, channel string, receivedAt int64) {
	t.Helper()
	var inc, ch interface{}
	if incarnation != "" {
		inc, ch = incarnation, channel
	}
	if _, err := db.Exec(`INSERT INTO prediction_observations
		(observation_id, collector_session_id, collector_epoch, collector_sequence,
		 pool_instance_id, round_incarnation_id, retention_group_owner_channel_id,
		 kind, producer_time_source, received_at_ms, payload_version, payload_json, observation_sha256)
		VALUES (?, 's', ?, ?, ?, ?, ?, ?, ?, ?, 1, '{"phase":"UNKNOWN"}', 'sha256:x')`,
		obsID, epoch, seq, pool, inc, ch, KindChannelEvent, TimeSourceReceiver, receivedAt); err != nil {
		t.Fatal(err)
	}
}

// TestObservationPruneSparesARoundThatSpansTheActiveSession is the real-driver
// reproduction of an independent review's F1.
//
// A prediction round that outlives a miner restart is the ORDINARY case, not an
// exotic one: the round keeps running on Twitch while the collector opens a new
// session. Its facts therefore land in two collector epochs, and the retention
// unit has to span both.
//
// The prune SELECT excluded OPEN-session rows in its WHERE clause — that is,
// BEFORE the GROUP BY — so the active session's facts were invisible to the
// HAVING guard that existed to protect them: MAX(received_at_ms) saw only the
// old rows and the active-epoch SUM was trivially zero. The DELETE then removed
// the whole incarnation, the live rows included. The guard read as if it worked
// and did nothing.
//
// Every eligibility question about a round has to be asked of ALL of the round's
// rows, because the DELETE acts on all of them.
func TestObservationPruneSparesARoundThatSpansTheActiveSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	repo, err := NewSQLiteRepository(db, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	previous, err := repo.OpenObservationSession(ctx, "previous", 1)
	if err != nil {
		t.Fatal(err)
	}
	active, err := repo.OpenObservationSession(ctx, "active", 2)
	if err != nil {
		t.Fatal(err)
	}
	// The round is admitted in the previous session and still live in the
	// active one, so both epochs carry facts of the SAME retention unit.
	const spanning = "round:spans-the-restart"
	insertObservationRow(t, db, previous, 1, "old-1", "pool-a", spanning, "chan-a", 1_000)
	insertObservationRow(t, db, previous, 2, "old-2", "pool-a", spanning, "chan-a", 1_500)
	insertObservationRow(t, db, active, 1, "live-1", "pool-b", spanning, "chan-a", 20_000)
	// A round entirely inside the finalized session, to prove the guard does
	// not simply disable retention.
	const finished = "round:entirely-in-the-past"
	insertObservationRow(t, db, previous, 3, "old-3", "pool-a", finished, "chan-b", 1_200)

	if applied, err := repo.FinalizeObservationSession(ctx, previous,
		ObservationAccounting{Committed: 3}, 2_000); err != nil || !applied {
		t.Fatalf("finalize previous session: applied=%v err=%v", applied, err)
	}

	// Drain retention completely at a cutoff that is past every old fact but
	// well short of the live one.
	for i := 0; i < 8; i++ {
		n, err := repo.PruneObservationUnit(ctx, 10_000, active)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}

	if got := countRows(t, repo,
		`SELECT COUNT(*) FROM prediction_observations WHERE collector_epoch = ?`, active); got != 1 {
		t.Fatalf("the active session kept %d of its 1 fact: retention deleted a live round's facts "+
			"because they shared an incarnation with the finalized session's rows", got)
	}
	// The unit is the LOCAL round — (collector_epoch, pool_instance_id,
	// round_incarnation_id) — so the previous session's slice of the same
	// round ages out on its own. That boundary is the point: ageing one
	// session's slice must never reach into another session's.
	if got := countRows(t, repo,
		`SELECT COUNT(*) FROM prediction_observations
		  WHERE round_incarnation_id = ? AND collector_epoch = ?`, spanning, previous); got != 0 {
		t.Fatalf("the finalized session's slice of the spanning round kept %d facts; a finalized, "+
			"fully-elapsed slice is an ordinary retention unit", got)
	}
	if got := countRows(t, repo,
		`SELECT COUNT(*) FROM prediction_observations
		  WHERE round_incarnation_id = ? AND collector_epoch = ?`, spanning, active); got != 1 {
		t.Fatalf("the active session's slice of the spanning round kept %d of its 1 fact", got)
	}
	if got := countRows(t, repo,
		`SELECT COUNT(*) FROM prediction_observations WHERE round_incarnation_id = ?`, finished); got != 0 {
		t.Fatalf("the fully-elapsed round kept %d facts; the guard must protect live rounds, "+
			"not stop retention", got)
	}
}

// TestObservationPruneSparesARoundThatSpansACrashLeftSession is the same defect
// reached through the other protected session state. A crash-left OPEN session
// is the only durable evidence of an unclean shutdown, and its facts are part of
// that evidence. When such a session shares a round with a finalized one, the
// finalized rows made the whole incarnation look eligible and the DELETE took
// the crashed session's facts with it — leaving an OPEN session that claims
// committed facts which are gone, which reads as corruption rather than as the
// crash it actually was.
func TestObservationPruneSparesARoundThatSpansACrashLeftSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	repo, err := NewSQLiteRepository(db, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	crashed, err := repo.OpenObservationSession(ctx, "crashed", 1) // never finalized
	if err != nil {
		t.Fatal(err)
	}
	finalized, err := repo.OpenObservationSession(ctx, "finalized", 2)
	if err != nil {
		t.Fatal(err)
	}
	active, err := repo.OpenObservationSession(ctx, "active", 3)
	if err != nil {
		t.Fatal(err)
	}

	const shared = "round:spans-the-crash"
	insertObservationRow(t, db, crashed, 1, "crashed-1", "pool-a", shared, "chan-a", 1_000)
	insertObservationRow(t, db, finalized, 1, "finalized-1", "pool-b", shared, "chan-a", 1_100)
	insertObservationRow(t, db, active, 1, "active-1", "pool-c", "", "", 30_000)

	if applied, err := repo.FinalizeObservationSession(ctx, finalized,
		ObservationAccounting{Committed: 1}, 2_000); err != nil || !applied {
		t.Fatalf("finalize: applied=%v err=%v", applied, err)
	}

	for i := 0; i < 8; i++ {
		n, err := repo.PruneObservationUnit(ctx, 10_000, active)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}

	if got := countRows(t, repo,
		`SELECT COUNT(*) FROM prediction_observations WHERE collector_epoch = ?`, crashed); got != 1 {
		t.Fatalf("the crash-left OPEN session kept %d of its 1 fact; its facts are the evidence of "+
			"the unclean shutdown and are never pruned automatically", got)
	}
	// The finalized session's own slice is a separate unit and ages out
	// normally; the crash-left slice is what must be untouchable.
	if got := countRows(t, repo,
		`SELECT COUNT(*) FROM prediction_observations
		  WHERE round_incarnation_id = ? AND collector_epoch = ?`, shared, finalized); got != 0 {
		t.Fatalf("the finalized session's slice kept %d facts; it is an ordinary retention unit", got)
	}
}

// TestObservationRetentionReusesAnalyticsSetting proves P1 adds no setting of
// its own: it reads the same Analytics.RetentionDays the rest of the package
// does, and adds nothing to the generic PruneBefore sweep.
func TestObservationRetentionReusesAnalyticsSetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	svc, err := NewService(db, filepath.Dir(path), 42)
	if err != nil {
		t.Fatal(err)
	}
	if svc.observations.retentionDays != 42 {
		t.Fatalf("collector retention = %d, want the Analytics.RetentionDays value 42", svc.observations.retentionDays)
	}

	// The generic sweep must not mention the observation tables.
	src := readPackageFile(t, "repository.go")
	start := strings.Index(src, "func (r *SQLiteRepository) PruneBefore(")
	if start < 0 {
		t.Fatal("PruneBefore not found")
	}
	end := strings.Index(src[start:], "\n}\n")
	body := src[start : start+end]
	if strings.Contains(body, "prediction_observation") {
		t.Fatal("PruneBefore was widened to the observation tables; P1 retention is worker-owned")
	}
}

// TestObservationPriorityParticipantsAreExactlyEleven is the source invariant:
// exactly the eleven documented repository entries claim the low-priority
// gate, and no Tx helper that runs inside a caller-owned *sql.Tx claims.
func TestObservationPriorityParticipantsAreExactlyEleven(t *testing.T) {
	src := readPackageFile(t, "repository.go")
	lines := strings.Split(src, "\n")

	var participants []string
	current := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "func (r *SQLiteRepository) ") {
			rest := strings.TrimPrefix(line, "func (r *SQLiteRepository) ")
			if i := strings.Index(rest, "("); i > 0 {
				current = rest[:i]
			}
		}
		if strings.Contains(line, "r.priority.Claim()") {
			participants = append(participants, current)
		}
	}
	want := []string{
		"Tombstone", "Reinstate", "RecordPoints", "RecordPointEvent",
		"RecordPointMarker", "RecordAnnotation", "RecordChatMessage", "RecordBet",
		"PruneBefore", "RenameStreamer", "DeleteStreamer",
	}
	sortStrings(participants)
	sortStrings(want)
	if !reflect.DeepEqual(participants, want) {
		t.Fatalf("priority participants = %v, want exactly %v", participants, want)
	}

	// No Tx helper claims: the claim already happened at the top of the call
	// that owns the transaction, and claiming inside one would invert the
	// lock order.
	for _, helper := range []string{"RenameStreamerTx", "DeleteStreamerTx", "EraseObservationsForIdentityTx"} {
		for _, p := range participants {
			if p == helper {
				t.Fatalf("%s claims the gate inside a caller-owned transaction", helper)
			}
		}
	}
}

// TestObservationSnapshotStaysUngated proves the coherent read snapshot is
// never gated: #303's snapshot semantics are untouched by P1.
func TestObservationSnapshotStaysUngated(t *testing.T) {
	src := readPackageFile(t, "repository.go")
	start := strings.Index(src, "func (r *SQLiteRepository) PointsSnapshotBetween(")
	if start < 0 {
		t.Fatal("PointsSnapshotBetween not found")
	}
	end := strings.Index(src[start:], "\n}\n")
	body := src[start : start+end]
	if strings.Contains(body, "priority") || strings.Contains(body, "r.mu") {
		t.Fatal("PointsSnapshotBetween took the gate or the repository mutex; it must stay ungated and lock-free")
	}
}

// TestObservationRemoveReAddDoesNotInheritFacts proves the identity lifecycle:
// after a streamer is erased and then re-added under the SAME login and
// channel, it starts clean — it inherits no observation from its previous
// life, and the facts it records afterwards are its own.
func TestObservationRemoveReAddDoesNotInheritFacts(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()

	if err := repo.RecordPoints("recycled", 100, "WATCH"); err != nil {
		t.Fatal(err)
	}
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-recycled", "recycled", "old-round", "ROUND_CREATED"))
	awaitCommitted(t, svc, 1)

	// Erase the identity exactly as the lifecycle purge does.
	if err := repo.db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := repo.EraseObservationsForIdentityTx(tx, ObservationIdentity{ChannelID: "chan-recycled", Login: "recycled"}); err != nil {
			return err
		}
		_, err := repo.DeleteStreamerTx(tx, "recycled")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	repo.Tombstone("recycled")

	// Re-add: the fence lifts and fresh history is allowed again.
	repo.Reinstate("recycled")
	if err := repo.RecordPoints("recycled", 200, "WATCH"); err != nil {
		t.Fatal(err)
	}
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-recycled", "recycled", "new-round", "ROUND_CREATED"))
	awaitCommitted(t, svc, 2)

	got, err := repo.ObservationsBySession(ctx, svc.observations.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].EventID != "new-round" {
		t.Fatalf("facts after re-add = %+v, want only the new round's fact", got)
	}
	// The re-added streamer got a FRESH parent row; the new fact points at it,
	// never at the purged one.
	var freshID int64
	if err := repo.db.QueryRow(`SELECT id FROM streamers WHERE name = ?`, "recycled").Scan(&freshID); err != nil {
		t.Fatal(err)
	}
	if got[0].RoutedStreamerID != freshID {
		t.Fatalf("re-added fact points at parent %d, want the fresh %d", got[0].RoutedStreamerID, freshID)
	}
}

// TestObservationErasureIsFencedAgainstQueuedFacts proves the ordering that
// makes an erasure trustworthy: a fact accepted BEFORE the erasure but not yet
// written can never commit afterwards, so it cannot resurrect the identity.
func TestObservationErasureIsFencedAgainstQueuedFacts(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()
	c := svc.observations

	// A fact accepted under the CURRENT generation, held back from the writer.
	queued := mustSanitize(t, channelObservation("pool-1", "chan-erased", "erased", "round-1", "ROUND_CREATED"))
	queued.generation = c.generation.Load()

	// The erasure fences capture BEFORE its transaction opens — exactly what
	// the lifecycle coordinator does via InvalidateIdentity.
	repo.InvalidateIdentity("chan-erased", "erased")
	if err := repo.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := repo.EraseObservationsForIdentityTx(tx, ObservationIdentity{ChannelID: "chan-erased", Login: "erased"})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// The held-back fact now reaches the writer and must be refused.
	c.write(ctx, queued)
	left, err := repo.ObservationsBySession(ctx, c.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("a fact queued before the erasure resurrected the identity: %+v", left)
	}
	// ...and the session is permanently INCOMPLETE, which is how a reader
	// learns the trail is not whole for this run.
	c.Close()
	reading, _, err := repo.ReadObservationSession(ctx, c.epoch.Load())
	if err != nil {
		t.Fatal(err)
	}
	if reading.Session.CloseState != SessionIncomplete {
		t.Fatalf("session after an erasure = %q, want INCOMPLETE", reading.Session.CloseState)
	}
}

// TestObservationFenceRefusesPostErasureProduction is the regression for the
// defect an independent review found: a generation bump alone is a ONE-SHOT.
// It drops facts already queued, but a producer still live for the erased
// channel stamps its NEXT fact with the new generation, so the erased identity
// comes straight back. The identity fence is what makes an erasure hold.
func TestObservationFenceRefusesPostErasureProduction(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()

	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-gone", "gone", "round-1", "ROUND_CREATED"))
	awaitCommitted(t, svc, 1)

	// Erase exactly as the lifecycle coordinator does.
	repo.InvalidateIdentity("chan-gone", "gone")
	if err := repo.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := repo.EraseObservationsForIdentityTx(tx, ObservationIdentity{ChannelID: "chan-gone", Login: "gone"})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// A producer that is STILL LIVE offers a fresh fact for the erased channel.
	// It carries the CURRENT generation, so only the identity fence can refuse
	// it.
	before := svc.observations.dropped.Load()
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-gone", "gone", "round-2", "ROUND_CREATED"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && svc.observations.dropped.Load() == before {
		time.Sleep(time.Millisecond)
	}

	left, err := repo.ObservationsBySession(ctx, svc.observations.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("a live producer resurrected the erased identity after the purge: %+v", left)
	}
	if svc.observations.dropped.Load() == before {
		t.Fatal("the post-erasure fact was neither written nor counted as a drop")
	}

	// A DIFFERENT identity is unaffected — the fence is identity-scoped, not a
	// global kill switch.
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-other", "other", "round-3", "ROUND_CREATED"))
	awaitCommitted(t, svc, 2)
	left, err = repo.ObservationsBySession(ctx, svc.observations.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].RoutedChannelID != "chan-other" {
		t.Fatalf("facts = %+v, want only the unaffected identity's", left)
	}
}

// TestObservationTombstoneFencesObservations proves the trail honours the SAME
// resurrection barrier the rest of the analytics store does: while a login is
// tombstoned, an observation naming it is refused, and Reinstate lifts both.
func TestObservationTombstoneFencesObservations(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()

	repo.Tombstone("fenced-login")
	before := svc.observations.dropped.Load()
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-f", "fenced-login", "r1", "ROUND_CREATED"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && svc.observations.dropped.Load() == before {
		time.Sleep(time.Millisecond)
	}
	got, err := repo.ObservationsBySession(ctx, svc.observations.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("an observation was written for a tombstoned login: %+v", got)
	}

	repo.Reinstate("fenced-login")
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-f", "fenced-login", "r2", "ROUND_CREATED"))
	awaitCommitted(t, svc, 1)
	got, err = repo.ObservationsBySession(ctx, svc.observations.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].EventID != "r2" {
		t.Fatalf("facts after reinstate = %+v, want the one recorded afterwards", got)
	}
}

// TestObservationUnfenceLiftsTheErasedChannelToo proves Reinstate lifts the
// channel that was erased alongside the login, so a re-added streamer is not
// permanently unobservable.
func TestObservationUnfenceLiftsTheErasedChannelToo(t *testing.T) {
	svc, repo := newObservationService(t)
	repo.InvalidateIdentity("chan-pair", "pair-login")

	if !svc.observations.isFenced(mustSanitize(t, channelObservation("pool-1", "chan-pair", "someone-else", "r", "ROUND_CREATED"))) {
		t.Fatal("the erased CHANNEL is not fenced")
	}
	repo.Reinstate("pair-login")
	// A fact captured AFTER the reinstate: its causal position is later than
	// the erasure's, so nothing but the fence itself can refuse it.
	fresh := mustSanitize(t, channelObservation("pool-1", "chan-pair", "pair-login", "r", "ROUND_CREATED"))
	fresh.sequence = svc.observations.sequence.Load() + 1
	if svc.observations.isFenced(fresh) {
		t.Fatal("Reinstate did not lift the channel erased alongside the login")
	}
}

// TestObservationOfferStampsTheCurrentGeneration closes a gap an independent
// review found: both generation tests called the unexported write() directly,
// so offer()'s stamping was never verified. Losing that stamp would silently
// kill capture after the first erasure — every later fact would carry a zero
// generation and be dropped — with the suite still green.
func TestObservationOfferStampsTheCurrentGeneration(t *testing.T) {
	// An UNSTARTED collector: nothing drains the queue, so the stamped value
	// can be read back directly instead of inferred from a side effect.
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	unstarted, err := NewService(db, filepath.Dir(path), 0)
	if err != nil {
		t.Fatal(err)
	}
	c := unstarted.observations
	c.phase.Store(phaseRunning) // publish capture WITHOUT a writer
	c.generation.Add(7)
	want := c.generation.Load()

	unstarted.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "s", "e1", "ROUND_CREATED"))

	select {
	case got := <-c.queue:
		if got.generation != want {
			t.Fatalf("offer stamped generation %d, want the current %d", got.generation, want)
		}
	default:
		t.Fatal("offer enqueued nothing")
	}

	// And capture still WORKS after a generation bump on a running collector:
	// a fact offered through the public entry point is committed, not
	// silently dropped by a stale stamp.
	svc, _ := newObservationService(t)
	svc.observations.generation.Add(3)
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "s", "e2", "ROUND_CREATED"))
	awaitCommitted(t, svc, 1)
}

// TestObservationPayloadOverCapIsRefusedWhole replaces a test that asserted an
// oversized payload "falls back to a minimal projection".
//
// That fallback wrote a phase/reason stub, hashed the stub as though it were
// what happened, and counted the row as a fact faithfully recorded. The
// session then reported it among its committed facts, so nothing at any layer
// could tell that the content had been discarded. An over-cap payload is
// refused, and the refusal is a counted drop.
func TestObservationPayloadOverCapIsRefusedWhole(t *testing.T) {
	huge := ObservationPayload{Phase: "ROUND_UPDATED", ReasonCode: "OK"}
	filler := strings.Repeat("x", 4096)
	for i := 0; i < MaxObservationOutcomes; i++ {
		huge.Outcomes = append(huge.Outcomes, ObservationOutcome{Slot: i, Color: filler})
	}
	raw, err := json.Marshal(huge)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= MaxObservationPayloadBytes {
		t.Fatalf("fixture is %d bytes, need more than the %d cap to exercise the branch",
			len(raw), MaxObservationPayloadBytes)
	}

	if rendered, ok := marshalObservationPayload(huge); ok {
		t.Fatalf("an over-cap payload rendered %d bytes instead of being refused", len(rendered))
	}

	// The byte ceiling is defence in depth, and this says why it has to be
	// tested directly: the closed vocabulary makes a SANITIZED payload far
	// smaller than the ceiling by construction, so no producer input can reach
	// the branch through sanitization. The 4 KiB filler above is exactly what
	// the vocabulary discards.
	over := channelObservation("pool-1", "chan-a", "s", "event-1", "ROUND_UPDATED")
	over.Payload = huge
	out, ok := sanitizeObservation(over, 1)
	if !ok {
		t.Fatal("the fixture was refused for some other reason; it must reach the size check")
	}
	sanitized, ok := marshalObservationPayload(out.Payload)
	if !ok {
		t.Fatal("a sanitized payload exceeded the byte ceiling")
	}
	if strings.Contains(sanitized, filler) {
		t.Fatal("the raw filler survived sanitization")
	}
	if len(sanitized) > MaxObservationPayloadBytes/8 {
		t.Fatalf("sanitized payload is %d bytes; the closed vocabulary is supposed to keep it far "+
			"below the %d ceiling", len(sanitized), MaxObservationPayloadBytes)
	}
}

// TestOverCapFactIsDroppedAndCounted is the end-to-end half: an over-cap fact
// is not merely refused by the sanitizer, it is accounted as a LOSS. That is
// what makes the session INCOMPLETE and stops a silently-shortened trail from
// finalizing as a complete one.
func TestOverCapFactIsDroppedAndCounted(t *testing.T) {
	svc, repo := newObservationService(t)

	over := channelObservation("pool-1", "chan-a", "over", "event-1", "ROUND_UPDATED")
	over.Payload.Outcomes = make([]ObservationOutcome, MaxObservationOutcomes+1)
	svc.RecordPredictionObservation(over)
	awaitDropped(t, svc, 1)

	got, err := repo.ObservationsBySession(context.Background(), svc.observations.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("an over-cap fact was stored anyway: %+v", got)
	}
	if c := svc.observations.committed.Load(); c != 0 {
		t.Fatalf("committed = %d, want 0: a refused fact must not count as recorded", c)
	}

	// The session cannot finalize COMPLETE while it has dropped a fact.
	svc.observations.Close()
	reading, _, err := repo.ReadObservationSession(context.Background(), svc.observations.epoch.Load())
	if err != nil {
		t.Fatal(err)
	}
	if reading.Session.CloseState != SessionIncomplete {
		t.Fatalf("close state = %q, want %q after a dropped fact",
			reading.Session.CloseState, SessionIncomplete)
	}
}

// awaitDropped waits until the collector has dropped n facts.
func awaitDropped(t *testing.T, svc *Service, n int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if svc.observations.dropped.Load() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("only %d of %d facts dropped (committed=%d)",
		svc.observations.dropped.Load(), n, svc.observations.committed.Load())
}

// TestALostFactLeavesACausalGap is the second half of an independent review's
// F2: the causal position must be RESERVED when the fact is captured, not
// handed out later by the writer to whatever survived.
//
// Assigning it at the writer made the stored sequence dense by construction.
// Every fact lost on the way — a full queue, a stale generation, an erasure
// fence, a session ceiling — consumed no number, so the trail read as a
// complete, gapless history of a session that had in fact lost facts, and a
// reader could not tell "nothing happened between these two" from "something
// happened and we dropped it". The session's dropped counter said facts were
// lost; the sequence said none were. Only one of those can be true.
//
// A reserved-then-unused number is the evidence, and the dropped counter is
// its explanation.
func TestALostFactLeavesACausalGap(t *testing.T) {
	svc, repo := newObservationService(t)

	// Arm the identity fence, then offer a fact naming the fenced identity.
	// The writer refuses it — but its position was already reserved when the
	// producer handed it over.
	repo.Tombstone("fenced")
	svc.RecordPredictionObservation(
		channelObservation("pool-1", "chan-fenced", "fenced", "e-lost", "ROUND_CREATED"))
	awaitDropped(t, svc, 1)

	svc.RecordPredictionObservation(
		channelObservation("pool-1", "chan-kept", "kept", "e-kept", "ROUND_CREATED"))
	awaitCommitted(t, svc, 1)

	got, err := repo.ObservationsBySession(context.Background(), svc.observations.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].EventID != "e-kept" {
		t.Fatalf("facts = %+v, want only the unfenced one", got)
	}
	if got[0].CollectorSequence != 2 {
		t.Fatalf("the surviving fact is at causal position %d, want 2: the lost fact's position "+
			"was reused, so the trail claims a completeness it does not have",
			got[0].CollectorSequence)
	}
	if assigned := svc.observations.sequence.Load(); assigned != 2 {
		t.Fatalf("last assigned sequence = %d, want 2 (one lost, one committed)", assigned)
	}

	// The session's accounting explains the gap rather than contradicting it.
	if c, d := svc.observations.committed.Load(), svc.observations.dropped.Load(); c != 1 || d != 1 {
		t.Fatalf("accounting = %d committed / %d dropped, want 1/1", c, d)
	}
}

// TestIdenticalRetryIsIdempotentSuccess is an independent review's F7: an
// identical retry must be RECOGNIZED, not merely rejected by a UNIQUE
// constraint.
//
// The difference matters to the caller. A constraint violation is
// indistinguishable from a real conflict, so a retry after an ambiguous
// failure -- a cancelled statement, a lost connection -- had to be treated as
// an error and counted as a loss, even though the fact was already safely
// recorded. Recognizing it makes the write idempotent: the same fact offered
// twice is one row and one success.
func TestIdenticalRetryIsIdempotentSuccess(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()
	epoch := svc.observations.epoch.Load()
	sessionID := svc.observations.sessionID

	fact := mustSanitize(t, channelObservation("pool-1", "chan-a", "streamer-a", "event-1", "ROUND_CREATED"))
	if err := repo.AppendObservation(ctx, fact, sessionID, epoch, 1); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := repo.AppendObservation(ctx, fact, sessionID, epoch, 1); err != nil {
		t.Fatalf("an identical retry was refused: %v", err)
	}
	got, err := repo.ObservationsBySession(ctx, sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("an identical retry produced %d rows, want 1", len(got))
	}

	// A LATER-created parent must not turn a retry into a conflict: the
	// resolved parent ids are insert metadata, not part of what the producer
	// captured, and the first row's are never recomputed.
	if err := repo.RecordPoints("streamer-a", 100, "WATCH"); err != nil {
		t.Fatal(err)
	}
	if err := repo.AppendObservation(ctx, fact, sessionID, epoch, 1); err != nil {
		t.Fatalf("a retry after the parent appeared was refused: %v", err)
	}
	got, err = repo.ObservationsBySession(ctx, sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RoutedStreamerID != 0 {
		t.Fatalf("the stored row was rewritten by a retry: %+v", got)
	}
}

// TestSameCausalPositionWithDifferentContentFailsClosed proves the other half:
// the same causal position carrying DIFFERENT content is an integrity failure,
// and the row already there is never overwritten.
func TestSameCausalPositionWithDifferentContentFailsClosed(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()
	epoch := svc.observations.epoch.Load()
	sessionID := svc.observations.sessionID

	first := mustSanitize(t, channelObservation("pool-1", "chan-a", "streamer-a", "event-1", "ROUND_CREATED"))
	if err := repo.AppendObservation(ctx, first, sessionID, epoch, 1); err != nil {
		t.Fatal(err)
	}
	second := mustSanitize(t, channelObservation("pool-1", "chan-a", "streamer-a", "event-2", "ROUND_UPDATED"))
	err := repo.AppendObservation(ctx, second, sessionID, epoch, 1)
	if !errors.Is(err, errObservationCollision) {
		t.Fatalf("colliding write returned %v, want a typed collision", err)
	}
	got, e := repo.ObservationsBySession(ctx, sessionID, 0)
	if e != nil {
		t.Fatal(e)
	}
	if len(got) != 1 || got[0].EventID != "event-1" {
		t.Fatalf("the first fact was overwritten: %+v", got)
	}
}

// TestAFinalizedSessionNeverAcceptsAnotherFact proves the published-session
// check. A finalized session's counters are fixed; a crash-left one is the
// durable evidence of an unclean shutdown. A late row in either makes those
// numbers a lie, and it is refused inside the same transaction that would
// have written it.
func TestAFinalizedSessionNeverAcceptsAnotherFact(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()
	epoch := svc.observations.epoch.Load()
	sessionID := svc.observations.sessionID
	fact := mustSanitize(t, channelObservation("pool-1", "chan-a", "streamer-a", "event-1", "ROUND_CREATED"))

	if applied, err := repo.FinalizeObservationSession(ctx, epoch,
		ObservationAccounting{LastAssignedSequence: 0}, 5); err != nil || !applied {
		t.Fatalf("finalize: applied=%v err=%v", applied, err)
	}
	if err := repo.AppendObservation(ctx, fact, sessionID, epoch, 1); !errors.Is(err, errObservationSessionNotOpen) {
		t.Fatalf("write into a finalized session returned %v, want a typed refusal", err)
	}
	// An epoch/session pair that does not exist at all is refused the same way.
	if err := repo.AppendObservation(ctx, fact, "no-such-session", epoch, 2); !errors.Is(err, errObservationSessionNotOpen) {
		t.Fatalf("write into an unknown session returned %v, want a typed refusal", err)
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM prediction_observations`); n != 0 {
		t.Fatalf("%d facts were written into a session that could not accept them", n)
	}
}

// TestARoundGroupOwnerIsFrozenByItsFirstFact proves a round's retention-group
// owner is decided ONCE. Resolving it per row would let a parent created
// mid-round, or a rename, give two facts of one round two different owners --
// and the owner is exactly what whole-round retention and whole-round erasure
// act on, so the round would stop being one unit.
func TestARoundGroupOwnerIsFrozenByItsFirstFact(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()
	epoch := svc.observations.epoch.Load()
	sessionID := svc.observations.sessionID

	// The first fact of the round is written while the streamer has no
	// analytics row, so the group's owner parent freezes as NULL.
	first := mustSanitize(t, channelObservation("pool-1", "chan-a", "late-parent", "event-1", "ROUND_CREATED"))
	if err := repo.AppendObservation(ctx, first, sessionID, epoch, 1); err != nil {
		t.Fatal(err)
	}
	// The parent appears mid-round.
	if err := repo.RecordPoints("late-parent", 100, "WATCH"); err != nil {
		t.Fatal(err)
	}
	companion := mustSanitize(t, channelObservation("pool-1", "chan-a", "late-parent", "event-1", "ROUND_UPDATED"))
	if err := repo.AppendObservation(ctx, companion, sessionID, epoch, 2); err != nil {
		t.Fatal(err)
	}

	got, err := repo.ObservationsByRound(ctx, first.RoundIncarnationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("round has %d facts, want 2", len(got))
	}
	if got[0].RetentionGroupOwnerStreamerID != got[1].RetentionGroupOwnerStreamerID {
		t.Fatalf("two facts of one round disagree about its owner: %d vs %d",
			got[0].RetentionGroupOwnerStreamerID, got[1].RetentionGroupOwnerStreamerID)
	}
	if got[1].RetentionGroupOwnerStreamerID != 0 {
		t.Fatalf("the companion resolved a parent of its own (%d); the group's owner is frozen "+
			"by its first committed fact", got[1].RetentionGroupOwnerStreamerID)
	}
	// The routed role is per-fact and DOES see the new parent: only the
	// retention group is frozen.
	if got[1].RoutedStreamerID == 0 {
		t.Fatal("the companion's own routed parent was frozen too; only the group owner is")
	}

	// A companion claiming a different owner for the same local round is
	// refused rather than quietly splitting the retention unit.
	conflicting := mustSanitize(t, channelObservation("pool-1", "chan-a", "late-parent", "event-1", "ROUND_UPDATED"))
	conflicting.RetentionGroupOwnerChannelID = "chan-someone-else"
	conflicting.RetentionGroupOwnerLogin = "someone-else"
	if err := repo.AppendObservation(ctx, conflicting, sessionID, epoch, 3); !errors.Is(err, errObservationGroupConflict) {
		t.Fatalf("a conflicting group owner returned %v, want a typed conflict", err)
	}
}

// TestSessionReadingEnforcesTheCounterForm proves the integrity predicates the
// reader contract requires. Each case is a store a reader must refuse to treat
// as authoritative -- and each was previously read as AS_FINALIZED, because the
// classifier checked only the fact count against committed_count.
func TestSessionReadingEnforcesTheCounterForm(t *testing.T) {
	base := ObservationSessionRecord{
		CollectorEpoch: 1, CollectorSessionID: "s", ProducerRevision: ObservationProducerRevision,
		StartedAtMS: 1, ClosedAtMS: 2, ClosedAtKnown: true, CloseState: SessionComplete,
		LastAssignedSequence: 2, LastAssignedSequenceKnown: true, CommittedCount: 2,
	}
	whole := observationSessionFacts{Present: 2, MinSequence: 1, MaxSequence: 2, DistinctSequences: 2}

	if got := classifyObservationSession(base, whole); got.Reading != ReadingAsFinalized {
		t.Fatalf("a coherent COMPLETE session reads %q (%s)", got.Reading, got.Detail)
	}

	for _, tc := range []struct {
		name    string
		session func(ObservationSessionRecord) ObservationSessionRecord
		facts   observationSessionFacts
		want    string
	}{
		{
			// ONLY the counter form is violated: the session is INCOMPLETE so
			// the exact-set rule does not apply, and its surviving facts
			// match committed_count exactly. Nothing but committed+dropped
			// failing to account for the reserved positions can catch this.
			name: "reserved positions are unaccounted for",
			session: func(s ObservationSessionRecord) ObservationSessionRecord {
				s.CloseState, s.CommittedCount, s.DroppedCount, s.LastAssignedSequence = SessionIncomplete, 2, 1, 9
				return s
			},
			facts: observationSessionFacts{Present: 2, MinSequence: 1, MaxSequence: 2, DistinctSequences: 2},
			want:  ReadingIntegrityError,
		},
		{
			name: "counters without any reservation",
			session: func(s ObservationSessionRecord) ObservationSessionRecord {
				s.LastAssignedSequenceKnown = false
				return s
			},
			facts: whole,
			want:  ReadingIntegrityError,
		},
		{
			name: "COMPLETE while reporting a loss",
			session: func(s ObservationSessionRecord) ObservationSessionRecord {
				s.DroppedCount, s.LastAssignedSequence = 1, 3
				return s
			},
			facts: whole,
			want:  ReadingIntegrityError,
		},
		{
			name:    "a fact outside the reserved range",
			session: func(s ObservationSessionRecord) ObservationSessionRecord { return s },
			facts:   observationSessionFacts{Present: 2, MinSequence: 1, MaxSequence: 9, DistinctSequences: 2},
			want:    ReadingIntegrityError,
		},
		{
			name:    "an orphan fact matching one half of the pair",
			session: func(s ObservationSessionRecord) ObservationSessionRecord { return s },
			facts:   observationSessionFacts{Present: 2, MinSequence: 1, MaxSequence: 2, DistinctSequences: 2, HalfPair: 1},
			want:    ReadingIntegrityError,
		},
		{
			name:    "COMPLETE with a gap in the positions it reserved",
			session: func(s ObservationSessionRecord) ObservationSessionRecord { return s },
			facts:   observationSessionFacts{Present: 2, MinSequence: 1, MaxSequence: 3, DistinctSequences: 2},
			want:    ReadingIntegrityError,
		},
		{
			name: "INCOMPLETE may legitimately have gaps",
			session: func(s ObservationSessionRecord) ObservationSessionRecord {
				s.CloseState, s.DroppedCount, s.CommittedCount, s.LastAssignedSequence = SessionIncomplete, 1, 2, 3
				return s
			},
			facts: observationSessionFacts{Present: 2, MinSequence: 1, MaxSequence: 3, DistinctSequences: 2},
			want:  ReadingAsFinalized,
		},
		{
			name: "facts removed after finalization",
			session: func(s ObservationSessionRecord) ObservationSessionRecord {
				s.CloseState = SessionIncomplete
				return s
			},
			facts: observationSessionFacts{Present: 1, MinSequence: 1, MaxSequence: 1, DistinctSequences: 1},
			want:  ReadingAdministrativelyTruncated,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyObservationSession(tc.session(base), tc.facts)
			if got.Reading != tc.want {
				t.Fatalf("reading = %q (%s), want %q", got.Reading, got.Detail, tc.want)
			}
		})
	}
}

// TestSessionReadingCountsOnTheExactPair proves the reader counts facts by the
// EXACT (collector_epoch, collector_session_id) pair.
//
// The fixture is built so that epoch-only counting gives the WRONG answer
// rather than merely a different route to the same one: the session committed
// two facts, one of its own was removed afterwards, and one row carrying this
// epoch under a different session id exists. Counted by epoch alone that is
// two facts -- exactly committed_count -- and the session reads as a coherent,
// fully intact AS_FINALIZED. Counted on the pair it is one fact, and the
// stray row is an orphan that makes the dataset unreadable.
func TestSessionReadingCountsOnTheExactPair(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()
	epoch := svc.observations.epoch.Load()

	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "streamer-a", "event-1", "ROUND_CREATED"))
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "streamer-a", "event-2", "ROUND_UPDATED"))
	awaitCommitted(t, svc, 2)
	svc.observations.Close()

	reading, _, err := repo.ReadObservationSession(ctx, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if reading.Reading != ReadingAsFinalized || reading.FactsPresent != 2 {
		t.Fatalf("baseline reads %q with %d facts", reading.Reading, reading.FactsPresent)
	}

	// Retention or an erasure removes one of the session's own facts...
	if _, err := repo.db.Exec(
		`DELETE FROM prediction_observations WHERE collector_sequence = 2 AND collector_epoch = ?`, epoch); err != nil {
		t.Fatal(err)
	}
	// ...and a row carrying this epoch under another session id takes its
	// place in an epoch-only count.
	insertObservationRow(t, repo.db, epoch, 2, "orphan", "pool-1", "", "", 10)
	if _, err := repo.db.Exec(
		`UPDATE prediction_observations SET collector_session_id = 'not-this-session' WHERE observation_id = 'orphan'`); err != nil {
		t.Fatal(err)
	}

	reading, _, err = repo.ReadObservationSession(ctx, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if reading.FactsPresent != 1 {
		t.Fatalf("the reader counted %d facts, want 1: a row under a different session id is not "+
			"one of this session's facts", reading.FactsPresent)
	}
	if reading.Reading != ReadingIntegrityError {
		t.Fatalf("a session with a cross-pair fact reads %q (%s), want %q",
			reading.Reading, reading.Detail, ReadingIntegrityError)
	}
}

// TestCloseBeforeStartLeavesNoWorker proves the lifecycle is one state
// machine rather than two independent guards.
//
// Close used to fence intake and return; Start's sync.Once was still unfired,
// so a later Start spawned the bootstrap goroutine anyway -- with a context
// nothing would ever cancel, because the Close that would have cancelled it
// had already run. That goroutine holds the shared database through its
// maintenance ticker, which is exactly what "no DB-capable P1 goroutine
// survives Service.Close" forbids, and it would then fail every DB call once
// App closed the database underneath it.
func TestCloseBeforeStartLeavesNoWorker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	svc, err := NewService(db, filepath.Dir(path), 0)
	if err != nil {
		t.Fatal(err)
	}
	c := svc.observations
	c.maintenanceInterval = time.Millisecond

	if err := svc.Close(); err != nil {
		t.Fatalf("close before start: %v", err)
	}
	// A Start after Close must be inert.
	if err := svc.Start(); err != nil {
		t.Fatalf("start after close: %v", err)
	}

	select {
	case <-c.bootstrapped:
		t.Fatal("a bootstrap ran after Close; the collector spawned a worker it can never cancel")
	case <-time.After(100 * time.Millisecond):
	}
	if c.capturing() {
		t.Fatal("capture opened after Close")
	}
	if got := c.phase.Load(); got != phaseClosed {
		t.Fatalf("phase = %d, want CLOSED", got)
	}
	if c.epoch.Load() != 0 {
		t.Fatal("a session was allocated after Close")
	}

	// And an offer afterwards is accounted, not silently queued.
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "s", "e", "ROUND_CREATED"))
	if c.postFenceProducers.Load() != 1 {
		t.Fatalf("post-fence producers = %d, want 1", c.postFenceProducers.Load())
	}
	if len(c.queue) != 0 {
		t.Fatalf("%d facts were queued to a collector with no writer", len(c.queue))
	}
}

// TestPublishingRunningLosesToTheShutdownFence proves the bootstrap's
// publication and the shutdown fence cannot interleave into a state neither
// intended.
//
// With independent flags, a bootstrap could read "not closing", be overtaken
// by Close (which set closing and cleared running), and then store running
// itself -- leaving intake OPEN on a collector whose writer was already
// cancelled and joined. Later facts were queued and lost without being
// counted anywhere, and the session could still finalize COMPLETE. Both are
// now one compare-and-swap out of STARTING, so one of them simply wins.
func TestPublishingRunningLosesToTheShutdownFence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")
	db := openPrivateDB(t, path)
	defer func() { _ = db.Close() }()
	svc, err := NewService(db, filepath.Dir(path), 0)
	if err != nil {
		t.Fatal(err)
	}
	c := svc.observations

	// Drive the exact interleaving directly rather than hoping to hit it: the
	// bootstrap is mid-publication when the fence goes up.
	c.phase.Store(phaseStarting)
	if was := c.fencePhase(); was != phaseStarting {
		t.Fatalf("fence saw phase %d, want STARTING", was)
	}
	// The bootstrap resumes and performs its publication — the real one.
	c.publishRunning()

	if c.capturing() {
		t.Fatal("a bootstrap published RUNNING over the top of the shutdown fence")
	}
	if got := c.phase.Load(); got != phaseClosing {
		t.Fatalf("phase = %d, want CLOSING", got)
	}
	// The other order is equally decisive: once RUNNING is published, the
	// fence still closes intake.
	c.phase.Store(phaseRunning)
	if was := c.fencePhase(); was != phaseRunning {
		t.Fatalf("fence saw phase %d, want RUNNING", was)
	}
	if c.capturing() {
		t.Fatal("the fence did not close intake")
	}
}

// TestSnapshotAndObservationContendInBothDirections is the behavioural proof
// the acceptance item asks for. The existing coverage was a source scan
// asserting PointsSnapshotBetween mentions neither the gate nor the mutex,
// which says nothing about what either side does when they actually meet.
//
// The two directions have deliberately different outcomes, and that asymmetry
// is the contract: the observer yields, the business reader does not.
func TestSnapshotAndObservationContendInBothDirections(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()
	if err := repo.RecordPoints("streamer-a", 100, "WATCH"); err != nil {
		t.Fatal(err)
	}
	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)

	t.Run("snapshot first: the observation yields", func(t *testing.T) {
		// A reader holding the single shared connection. P1 must give up
		// inside its own deadline rather than making this wait.
		release := make(chan struct{})
		held := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- repo.db.WithTx(ctx, func(*sql.Tx) error {
				close(held)
				<-release
				return nil
			})
		}()
		<-held

		svc.observations.writeDeadline = 20 * time.Millisecond
		before := svc.observations.dropped.Load()
		start := time.Now()
		svc.RecordPredictionObservation(
			channelObservation("pool-1", "chan-a", "streamer-a", "yielding", "ROUND_CREATED"))
		awaitDropped(t, svc, before+1)
		yielded := time.Since(start)

		close(release)
		if err := <-done; err != nil {
			t.Fatalf("the reader's transaction was disturbed: %v", err)
		}
		if yielded > 2*time.Second {
			t.Fatalf("the observation took %v to yield; it must give up inside its own deadline", yielded)
		}
		svc.observations.writeDeadline = 5 * time.Second
	})

	t.Run("observation first: the snapshot neither cancels nor waits for it", func(t *testing.T) {
		// Hold a real observation lease, so there is unambiguously an
		// observer in flight when the snapshot runs. Relying on a background
		// write loop to be mid-lease at the right moment does not distinguish
		// anything: the window is microseconds wide and is almost never open.
		lease, settle, ok := svc.observations.gate.lease(context.Background())
		if !ok {
			t.Fatal("could not take an observation lease")
		}
		defer settle()

		done := make(chan error, 1)
		go func() {
			_, err := repo.PointsSnapshotBetween(ctx, "streamer-a", from, to, 0, false)
			done <- err
		}()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("snapshot failed while an observation held its lease: %v", err)
			}
			if lease.Err() != nil {
				t.Fatal("the snapshot cancelled the in-flight observation: it claimed the priority " +
					"gate, and a business READ must not do that")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("the snapshot waited for the observation's lease to settle; it must stay ungated " +
				"and never queue behind an observer")
		}
	})
}

// TestAFactCapturedBeforeAnErasureCannotSurviveTheReinstate is an independent
// review's F6, stated as the interleaving it turns on.
//
// A producer captures a fact about a streamer. Before it reaches the
// collector, the streamer is removed -- the erasure runs, the fence goes up --
// and then the streamer is added again, which lifts the fence. The producer
// resumes and hands its fact over. The fence is down, the generation it is
// stamped with is the current one, and the fact is committed: an observation
// of the life that was erased, filed under the life that replaced it.
//
// That is the one outcome an erasure exists to prevent, and neither the
// generation bump nor the fence could see it: the bump only invalidates work
// already queued, and the fence only covers the window it is armed for.
//
// The erasure's boundary is now the causal position it was reached at, and it
// is never lifted. A fact whose position was reserved at or before that point
// was captured during the erased life, however the fence has moved since.
func TestAFactCapturedBeforeAnErasureCannotSurviveTheReinstate(t *testing.T) {
	svc, repo := newObservationService(t)
	ctx := context.Background()
	c := svc.observations

	if err := repo.RecordPoints("recycled", 100, "WATCH"); err != nil {
		t.Fatal(err)
	}

	// The producer captures a fact and reserves its causal position -- and is
	// then descheduled before the collector ever sees it.
	inFlight := mustSanitize(t, channelObservation("pool-1", "chan-recycled", "recycled", "old-life", "ROUND_CREATED"))
	inFlight.sequence = c.sequence.Add(1)
	inFlight.generation = c.generation.Load()

	// Meanwhile: the streamer is removed, erased, and added again.
	if err := repo.db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := repo.EraseObservationsForIdentityTx(tx,
			ObservationIdentity{ChannelID: "chan-recycled", Login: "recycled"}); err != nil {
			return err
		}
		_, err := repo.DeleteStreamerTx(tx, "recycled")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	repo.Tombstone("recycled")
	repo.Reinstate("recycled")
	if err := repo.RecordPoints("recycled", 200, "WATCH"); err != nil {
		t.Fatal(err)
	}

	// The producer resumes. The fence is down and the generation it carries is
	// no longer distinguishable from a fresh one -- the erasure bumped the
	// generation, but this fact was stamped before that and the reinstate
	// makes the identity acceptable again.
	before := c.dropped.Load()
	c.write(ctx, inFlight)
	if c.dropped.Load() != before+1 {
		t.Fatal("a fact captured during the erased life was accepted after the re-add")
	}

	got, err := repo.ObservationsBySession(ctx, c.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range got {
		if rec.EventID == "old-life" {
			t.Fatalf("an observation of the erased life was filed under the re-added streamer: %+v", rec)
		}
	}

	// The re-added streamer is still fully observable: only facts captured
	// before the erasure are refused.
	fresh := mustSanitize(t, channelObservation("pool-1", "chan-recycled", "recycled", "new-life", "ROUND_CREATED"))
	fresh.sequence = c.sequence.Add(1)
	fresh.generation = c.generation.Load()
	committed := c.committed.Load()
	c.write(ctx, fresh)
	if c.committed.Load() != committed+1 {
		t.Fatal("the re-added streamer is permanently unobservable; only the erased life is refused")
	}
}
