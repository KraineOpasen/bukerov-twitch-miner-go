package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
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
		EventID:                      event,
		Kind:                         KindChannelEvent,
		SourceTopicType:              TopicTypePredictionsChannel,
		SourceMessageType:            MessageTypeEventCreated,
		ProducerAtMS:                 1_700_000_000_000,
		ProducerTimeSource:           TimeSourceProducer,
		ReceivedAtMS:                 1_700_000_000_001,
		ConnectionIndex:              2,
		ConnectionGeneration:         7,
		ConnectionSequence:           11,
		ConnectionKnown:              true,
		Payload:                      ObservationPayload{Phase: phase, RoundState: "ACTIVE"},
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
	inc := roundIncarnationID(base)

	d1 := observationDigest(base, "o:1", "s", 1, 1, inc, payload)
	d2 := observationDigest(base, "o:1", "s", 1, 1, inc, payload)
	if d1 != d2 {
		t.Fatal("digest is not reproducible for identical content")
	}

	changed := base
	changed.EventID = "event-2"
	if observationDigest(changed, "o:1", "s", 1, 1, inc, payload) == d1 {
		t.Fatal("digest ignores the event identity")
	}
	otherPayload, _ := marshalObservationPayload(ObservationPayload{Phase: "ROUND_UPDATED"})
	if observationDigest(base, "o:1", "s", 1, 1, inc, otherPayload) == d1 {
		t.Fatal("digest ignores the payload")
	}
	if observationDigest(base, "o:1", "s", 1, 2, inc, payload) == d1 {
		t.Fatal("digest ignores the causal position")
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
				{Color: secret, TotalPoints: 5, TotalUsers: 2, TopPredictorsExamined: 9999},
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
	if out.Payload.Outcomes[0].TopPredictorsExamined != MaxTopPredictorsExamined {
		t.Fatalf("examined predictors = %d, want the %d cap", out.Payload.Outcomes[0].TopPredictorsExamined, MaxTopPredictorsExamined)
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
		observationDigest(out, "o:1", "s", 1, 1, "", payload),
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
		out := sanitizeObservationPayload(ObservationPayload{
			Phase:      raw,
			RoundState: raw,
			Decision:   raw,
			ReasonCode: raw,
			ErrorClass: raw,
		})
		rendered, _ := marshalObservationPayload(out)
		if strings.Contains(rendered, raw) {
			t.Fatalf("forbidden input %q reached the persisted payload %s", raw, rendered)
		}
	}
}

// TestObservationPayloadCaps proves the bounded projection: outcomes are
// capped at 64 and a payload that would exceed 64 KiB is replaced by a
// minimal, still-valid projection rather than a truncated one.
func TestObservationPayloadCaps(t *testing.T) {
	many := make([]ObservationOutcome, MaxObservationOutcomes+50)
	for i := range many {
		many[i] = ObservationOutcome{Color: "BLUE", TotalPoints: int64(i)}
	}
	out := sanitizeObservationPayload(ObservationPayload{Phase: "ROUND_UPDATED", Outcomes: many})
	if len(out.Outcomes) != MaxObservationOutcomes {
		t.Fatalf("outcomes = %d, want the %d cap", len(out.Outcomes), MaxObservationOutcomes)
	}
	for i, o := range out.Outcomes {
		if o.Slot != i {
			t.Fatalf("outcome %d has slot %d — slots must be positional", i, o.Slot)
		}
	}

	// A payload that cannot fit falls back to a minimal projection that still
	// parses as the same type.
	huge := ObservationPayload{Phase: "ROUND_UPDATED", ReasonCode: "OK"}
	huge.Counters = map[string]int64{}
	rendered, ok := marshalObservationPayload(huge)
	if !ok {
		t.Fatal("an ordinary payload was rejected")
	}
	if len(rendered) > MaxObservationPayloadBytes {
		t.Fatalf("rendered payload is %d bytes, over the %d cap", len(rendered), MaxObservationPayloadBytes)
	}
	var back ObservationPayload
	if err := json.Unmarshal([]byte(rendered), &back); err != nil {
		t.Fatalf("rendered payload does not round-trip: %v", err)
	}
}

// TestObservationStringCapIsEnforced proves an over-long enum candidate is
// rejected outright (it becomes UNKNOWN) rather than being truncated into
// something that might collide with a real member.
func TestObservationStringCapIsEnforced(t *testing.T) {
	long := strings.Repeat("A", MaxObservationString+1)
	if got := closedValue(long, observationPhases); got != ValueUnknown {
		t.Fatalf("over-long value became %q, want %q", got, ValueUnknown)
	}
	if got := boundedIdentifier(long); len(got) != MaxObservationString {
		t.Fatalf("identifier length = %d, want the %d cap", len(got), MaxObservationString)
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
	svc.observations.dropped.Add(1) // stand in for a queue-full or refused write
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
	if reading.Session.CloseState != SessionIncomplete || reading.Session.UnsettledObligationCount != 1 {
		t.Fatalf("session = %q/%d, want INCOMPLETE/1", reading.Session.CloseState, reading.Session.UnsettledObligationCount)
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
	c.running.Store(false)
	c.running.Store(true)
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
// fenced intake, is counted as a drop and never written.
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
	if svc.observations.dropped.Load() != 1 {
		t.Fatal("a fact offered before Start was not counted as a drop")
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
	dropped := svc.observations.dropped.Load()
	svc.RecordPredictionObservation(channelObservation("pool-1", "chan-a", "streamer-a", "late", "ROUND_CREATED"))
	if svc.observations.dropped.Load() != dropped+1 {
		t.Fatal("a fact offered after Close was not fenced")
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

// TestObservationRoundGrouping proves every fact of one round shares a round
// incarnation, including across collector sessions, which is what makes
// whole-round retention and whole-round erasure well defined.
func TestObservationRoundGrouping(t *testing.T) {
	a := mustSanitize(t, channelObservation("pool-1", "chan-a", "s", "event-1", "ROUND_CREATED"))
	b := mustSanitize(t, channelObservation("pool-2", "chan-a", "s", "event-1", "ROUND_UPDATED"))
	other := mustSanitize(t, channelObservation("pool-1", "chan-a", "s", "event-2", "ROUND_CREATED"))
	otherChannel := mustSanitize(t, channelObservation("pool-1", "chan-b", "s", "event-1", "ROUND_CREATED"))

	if roundIncarnationID(a) != roundIncarnationID(b) {
		t.Fatal("two facts of one round disagree on their round incarnation")
	}
	if roundIncarnationID(a) == roundIncarnationID(other) {
		t.Fatal("two different rounds share a round incarnation")
	}
	if roundIncarnationID(a) == roundIncarnationID(otherChannel) {
		t.Fatal("two channels' rounds share a round incarnation")
	}
	noOwner := a
	noOwner.RetentionGroupOwnerChannelID = ""
	if roundIncarnationID(noOwner) != "" {
		t.Fatal("a fact with no retention-group owner must have no round incarnation")
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

	if _, err := repo.db.Exec(`UPDATE prediction_observation_sessions SET producer_revision = 'obs-v0|other' WHERE collector_epoch = ?`,
		svc.observations.epoch.Load()); err != nil {
		t.Fatal(err)
	}
	reading, _, err := repo.ReadObservationSession(context.Background(), svc.observations.epoch.Load())
	if err != nil {
		t.Fatal(err)
	}
	if reading.Reading != ReadingIntegrityError {
		t.Fatalf("foreign-contract session reads %q, want %q", reading.Reading, ReadingIntegrityError)
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
	var open int
	if err := db.QueryRow(`SELECT COUNT(*) FROM prediction_observation_sessions WHERE collector_epoch = ? AND close_state = 'OPEN'`, oldEpoch).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 1 {
		t.Fatal("a crash-left OPEN session was pruned automatically; it is the only evidence of an unclean shutdown")
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
	if svc.observations.isFenced(mustSanitize(t, channelObservation("pool-1", "chan-pair", "pair-login", "r", "ROUND_CREATED"))) {
		t.Fatal("Reinstate did not lift the channel erased alongside the login")
	}
}
