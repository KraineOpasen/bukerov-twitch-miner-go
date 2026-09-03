package models

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- The minute-watched WIRE CONTRACT -------------------------------------
//
// The payload's field set AND the JSON type of every field are protocol.
// Twitch transport-accepts a payload with the wrong shape and simply does not
// credit the watch, so these are the only tests that can catch a regression
// before it silently stops earning points in production.

// contractClock is a deterministic clock at a fixed, non-UTC-midnight instant
// with a non-zero millisecond component, so a formatting regression (wrong
// precision, local zone, missing Z) cannot pass by accident.
func contractClock() Clock {
	fixed := time.Date(2026, 9, 3, 16, 24, 44, 123456789, time.FixedZone("MSK", 3*60*60))
	return func() time.Time { return fixed }
}

const (
	contractUserID    = "44322889"
	contractClientNow = "2026-09-03T13:24:44.123Z" // the fixed instant, in UTC
)

func mustBuild(t *testing.T, game *Game) []MinuteWatchedEvent {
	t.Helper()
	events, err := BuildMinuteWatchedPayload("123456", "987654321", contractUserID, "cyganzor", game, contractClock())
	if err != nil {
		t.Fatalf("building the payload must succeed: %v", err)
	}
	return events
}

// TestPayloadExactWireShape pins the ENTIRE serialized payload byte for byte:
// the exact field set (no more, no less), every JSON type, and every value.
// Adding, removing, retyping or omitting any field fails here.
func TestPayloadExactWireShape(t *testing.T) {
	events := mustBuild(t, &Game{ID: "509658", Name: "Just Chatting"})

	got, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"event":"minute-watched","properties":{` +
		`"broadcast_id":"987654321",` +
		`"channel_id":"123456",` +
		`"channel":"cyganzor",` +
		`"client_time":"` + contractClientNow + `",` +
		`"game":"Just Chatting",` +
		`"game_id":"509658",` +
		`"hidden":false,` +
		`"is_live":true,` +
		`"live":true,` +
		`"logged_in":true,` +
		`"minutes_logged":1,` +
		`"muted":false,` +
		`"player":"site",` +
		`"user_id":44322889` +
		`}}]`
	if string(got) != want {
		t.Fatalf("wire payload mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestPayloadFieldSetIsExact proves no field is silently added or dropped,
// independently of ordering.
func TestPayloadFieldSetIsExact(t *testing.T) {
	events := mustBuild(t, &Game{ID: "509658", Name: "Just Chatting"})
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded []struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]bool{
		"broadcast_id": true, "channel_id": true, "channel": true, "client_time": true,
		"game": true, "game_id": true, "hidden": true, "is_live": true, "live": true,
		"logged_in": true, "minutes_logged": true, "muted": true, "player": true, "user_id": true,
	}
	for k := range decoded[0].Properties {
		if !want[k] {
			t.Errorf("unexpected field %q on the wire", k)
		}
		delete(want, k)
	}
	for k := range want {
		t.Errorf("missing field %q", k)
	}
	// location is a secondary-implementation-only field the primary reference
	// does not send; it must not appear.
	if _, bad := decoded[0].Properties["location"]; bad {
		t.Errorf("payload must not carry a \"location\" field")
	}
}

// TestPayloadMissingGameSendsEmptyStrings pins the audited contract for an
// unknown game: game and game_id are still PRESENT, as empty strings — never
// omitted (the legacy payload dropped both keys entirely).
func TestPayloadMissingGameSendsEmptyStrings(t *testing.T) {
	for name, game := range map[string]*Game{
		"nil game":        nil,
		"empty game":      {},
		"name without id": {Name: "Just Chatting"},
		"id without name": {ID: "509658"},
	} {
		t.Run(name, func(t *testing.T) {
			events := mustBuild(t, game)
			raw, err := json.Marshal(events)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded []struct {
				Properties map[string]any `json:"properties"`
			}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			for _, k := range []string{"game", "game_id"} {
				v, ok := decoded[0].Properties[k]
				if !ok {
					t.Fatalf("field %q must always be present, even without a game", k)
				}
				if _, isStr := v.(string); !isStr {
					t.Fatalf("field %q must be a JSON string, got %T", k, v)
				}
			}
		})
	}
}

// TestPayloadIDTypesAreNotInterchangeable pins the deliberate asymmetry: the
// three id-ish fields stay JSON strings while user_id is a JSON number.
func TestPayloadIDTypesAreNotInterchangeable(t *testing.T) {
	events := mustBuild(t, &Game{ID: "509658", Name: "Just Chatting"})
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var decoded []struct {
		Properties map[string]any `json:"properties"`
	}
	if err := dec.Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	props := decoded[0].Properties
	for _, k := range []string{"broadcast_id", "channel_id", "game_id"} {
		if _, ok := props[k].(string); !ok {
			t.Errorf("field %q must remain a JSON string, got %T (%v)", k, props[k], props[k])
		}
	}
	num, ok := props["user_id"].(json.Number)
	if !ok {
		t.Fatalf("user_id must be a JSON number, got %T (%v)", props["user_id"], props["user_id"])
	}
	if num.String() != contractUserID {
		t.Errorf("user_id = %s, want %s", num.String(), contractUserID)
	}
	ml, ok := props["minutes_logged"].(json.Number)
	if !ok {
		t.Fatalf("minutes_logged must be a JSON number, got %T", props["minutes_logged"])
	}
	if ml.String() != "1" {
		t.Errorf("minutes_logged = %s, want exactly 1", ml.String())
	}
}

// TestPayloadFailsClosedOnInvalidUserID: an unusable viewer identity yields NO
// payload and a bounded sentinel error — never a payload with user_id coerced
// to 0, which Twitch would transport-accept and never credit.
func TestPayloadFailsClosedOnInvalidUserID(t *testing.T) {
	for _, userID := range []string{"", " ", "uid", "account", "uid-cyganzor", "0", "-1", "12.5", "1e5", "0x10", "44322889 ", "+44322889", "٤٤٣٢٢٨٨٩"} {
		t.Run("userID="+userID, func(t *testing.T) {
			events, err := BuildMinuteWatchedPayload("123456", "987654321", userID, "cyganzor", nil, contractClock())
			if err == nil {
				t.Fatalf("an invalid user id must fail closed, got payload %+v", events)
			}
			if !errors.Is(err, ErrInvalidUserID) {
				t.Fatalf("want ErrInvalidUserID, got %v", err)
			}
			if events != nil {
				t.Fatalf("a failed build must return no payload, got %+v", events)
			}
		})
	}
}

// TestSetPayloadFailsClosedWithoutTouchingTheSession: a refused payload must not
// publish anything, and must not bump the playback-session generation.
func TestSetPayloadFailsClosedWithoutTouchingTheSession(t *testing.T) {
	s := NewStream()
	s.Update("987654321", "t", nil, nil, 1)
	before := s.SessionGeneration()

	if err := s.SetPayload("123456", "987654321", "uid", "cyganzor", nil, contractClock()); !errors.Is(err, ErrInvalidUserID) {
		t.Fatalf("want ErrInvalidUserID, got %v", err)
	}
	if snap := s.SessionSnapshot(); snap.HasPayload() {
		t.Fatal("a refused payload must never be published")
	}
	if after := s.SessionGeneration(); after != before {
		t.Fatalf("a refused payload must not bump the session generation: %d -> %d", before, after)
	}
}

// TestFaultedCandidateClearsAnExistingPayload: once the session proves it cannot
// build a creditable payload, the OLD payload is dropped rather than replayed.
func TestFaultedCandidateClearsAnExistingPayload(t *testing.T) {
	s := NewStream()
	obs := s.BeginSessionObservation()
	good, err := PlaybackSessionCandidate{BroadcastID: "b1"}.
		WithSpadeURL("https://spade.twitch.tv/track").
		WithPayload("123456", "b1", contractUserID, "cyganzor", nil, contractClock())
	if err != nil {
		t.Fatalf("valid build must succeed: %v", err)
	}
	if r := s.ApplyPlaybackSessionIfCurrent(obs, good, ExpectedSession{}); !r.Applied {
		t.Fatalf("the valid session must apply, got %+v", r)
	}
	if !s.SessionSnapshot().HasPayload() {
		t.Fatal("the valid payload must be published")
	}

	obs2 := s.BeginSessionObservation()
	bad, err := PlaybackSessionCandidate{BroadcastID: "b1"}.
		WithPayload("123456", "b1", "uid", "cyganzor", nil, contractClock())
	if !errors.Is(err, ErrInvalidUserID) {
		t.Fatalf("want ErrInvalidUserID, got %v", err)
	}
	if r := s.ApplyPlaybackSessionIfCurrent(obs2, bad, ExpectedSession{}); !r.Applied {
		t.Fatalf("the faulted candidate must still apply (to clear), got %+v", r)
	}
	if s.SessionSnapshot().HasPayload() {
		t.Fatal("a faulted refresh must CLEAR the payload, not keep replaying the old one")
	}
}

// TestClientTimeIsDeterministicAndUTC pins the exact format against an injected
// clock: ISO-8601, UTC, exactly three fractional digits, literal Z — and proves
// a non-UTC source instant is converted rather than emitted with an offset.
func TestClientTimeIsDeterministicAndUTC(t *testing.T) {
	events := mustBuild(t, nil)
	got := events[0].Properties.ClientTime
	if got != contractClientNow {
		t.Fatalf("client_time = %q, want %q", got, contractClientNow)
	}
	if _, err := time.Parse(MinuteWatchedClientTimeLayout, got); err != nil {
		t.Fatalf("client_time must parse as %s: %v", MinuteWatchedClientTimeLayout, err)
	}
	// Two builds from the same clock are byte-identical: nothing else varies.
	again := mustBuild(t, nil)
	if again[0].Properties != events[0].Properties {
		t.Fatal("two builds from the same clock must be identical")
	}
}

// TestClientTimeIsSessionBound is the falsification test for the per-send
// timestamp alternative. client_time belongs to the PAYLOAD: it is stamped once
// when the payload is built and replayed unchanged by every beacon from that
// session, exactly as the primary reference does (@cached_property).
//
// The clock is ADVANCED between reads, which is what makes this a real guard: if
// the timestamp were derived at snapshot or encode time instead of at build time,
// the encoded bytes would change as the clock moves. Reading the session must
// also not churn the playback-session generation — postBeacon suppresses any send
// whose captured generation drifted, so a per-send stamp (which would require
// re-publishing the payload) would turn every beacon into a stale-session no-op
// that earns nothing while logging nothing alarming.
func TestClientTimeIsSessionBound(t *testing.T) {
	// A clock that moves a full minute on every single call, so any re-derivation
	// of client_time anywhere downstream of the build is impossible to miss.
	tick := time.Date(2026, 9, 3, 16, 24, 44, 123000000, time.UTC)
	moving := Clock(func() time.Time {
		tick = tick.Add(time.Minute)
		return tick
	})

	s := NewStream()
	s.Update("987654321", "t", nil, nil, 1)
	if err := s.SetPayload("123456", "987654321", contractUserID, "cyganzor", nil, moving); err != nil {
		t.Fatalf("payload must build: %v", err)
	}

	gen := s.SessionGeneration()
	first, err := s.SessionSnapshot().EncodePayload()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	stamped := s.SessionSnapshot().payload[0].Properties.ClientTime

	for i := 0; i < 5; i++ {
		moving.Now() // the wall clock moves a minute between beacons
		snap := s.SessionSnapshot()
		body, err := snap.EncodePayload()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if body != first {
			t.Fatalf("beacon %d re-stamped the payload; client_time must be session-bound", i)
		}
		if got := snap.payload[0].Properties.ClientTime; got != stamped {
			t.Fatalf("beacon %d client_time drifted %q -> %q; it must be frozen at build time", i, stamped, got)
		}
		if snap.Generation != gen {
			t.Fatalf("reading the session must not churn the generation: %d -> %d", gen, snap.Generation)
		}
	}
	if after := s.SessionGeneration(); after != gen {
		t.Fatalf("session generation churned without a real session change: %d -> %d", gen, after)
	}

	// The complement: BUILDING a new payload after the clock moved must produce a
	// NEW client_time, proving the clock is genuinely consulted at build time and
	// the value is not a frozen constant.
	if err := s.SetPayload("123456", "987654321", contractUserID, "cyganzor", nil, moving); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got := s.SessionSnapshot().payload[0].Properties.ClientTime; got == stamped {
		t.Fatalf("a rebuilt payload must carry a fresh client_time, still %q", got)
	}
}

// TestPayloadSurvivesBase64RoundTrip proves the exact bytes the spade endpoint
// decodes are the payload under test.
func TestPayloadSurvivesBase64RoundTrip(t *testing.T) {
	s := NewStream()
	s.Update("987654321", "t", nil, nil, 1)
	if err := s.SetPayload("123456", "987654321", contractUserID, "cyganzor", &Game{ID: "509658", Name: "Just Chatting"}, contractClock()); err != nil {
		t.Fatalf("payload must build: %v", err)
	}
	encoded, err := s.SessionSnapshot().EncodePayload()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("standard base64 decode must succeed: %v", err)
	}
	var events []MinuteWatchedEvent
	if err := json.Unmarshal(decoded, &events); err != nil {
		t.Fatalf("decoded bytes must be the event array: %v", err)
	}
	if len(events) != 1 || events[0].Event != "minute-watched" {
		t.Fatalf("unexpected decoded payload: %+v", events)
	}
	if events[0].Properties.UserID != 44322889 || events[0].Properties.MinutesLogged != 1 {
		t.Fatalf("payload did not survive the round trip: %+v", events[0].Properties)
	}
}

// TestPayloadCarriesNoCredentialMaterial: the beacon body is public telemetry.
func TestPayloadCarriesNoCredentialMaterial(t *testing.T) {
	events := mustBuild(t, &Game{ID: "509658", Name: "Just Chatting"})
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, banned := range []string{"token", "sig", "Authorization", "OAuth", "cookie", "Cookie", "auth"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("payload must not mention %q: %s", banned, raw)
		}
	}
}

// TestConcurrentPayloadBuildsAreRaceFree runs under -race: the builder must stay
// safe to call off the Stream lock from concurrent refreshes.
func TestConcurrentPayloadBuildsAreRaceFree(t *testing.T) {
	s := NewStream()
	s.Update("987654321", "t", nil, nil, 1)
	if err := s.SetPayload("123456", "987654321", contractUserID, "cyganzor", nil, contractClock()); err != nil {
		t.Fatalf("payload must build: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := BuildMinuteWatchedPayload("123456", "987654321", contractUserID, "cyganzor", nil, nil); err != nil {
				t.Errorf("concurrent build failed: %v", err)
			}
			if _, err := s.SessionSnapshot().EncodePayload(); err != nil {
				t.Errorf("concurrent encode failed: %v", err)
			}
		}()
	}
	wg.Wait()
}

// TestNilClockUsesTheSystemClock keeps the nil-Clock fallback honest, so callers
// that do not inject a clock still emit a well-formed client_time.
func TestNilClockUsesTheSystemClock(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)
	events, err := BuildMinuteWatchedPayload("123456", "987654321", contractUserID, "cyganzor", nil, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, err := time.Parse(MinuteWatchedClientTimeLayout, events[0].Properties.ClientTime)
	if err != nil {
		t.Fatalf("client_time must parse as %s: %v", MinuteWatchedClientTimeLayout, err)
	}
	if got.Before(before) || got.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("client_time %s is not a plausible now", events[0].Properties.ClientTime)
	}
}

// TestPayloadFailsClosedOnIncompleteIdentity: broadcast_id, channel_id and
// channel name WHAT is being watched. A beacon missing any of them is
// transport-accepted (204) and never credited, so the strict-204 transport rule
// cannot catch it — the payload must be refused at build time instead, exactly
// like an unusable viewer identity.
func TestPayloadFailsClosedOnIncompleteIdentity(t *testing.T) {
	cases := map[string]struct{ channelID, broadcastID, channel string }{
		"empty broadcast id": {"123456", "", "cyganzor"},
		"empty channel id":   {"", "987654321", "cyganzor"},
		"empty channel":      {"123456", "987654321", ""},
		"all empty":          {"", "", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			events, err := BuildMinuteWatchedPayload(c.channelID, c.broadcastID, contractUserID, c.channel, nil, contractClock())
			if !errors.Is(err, ErrIncompleteIdentity) {
				t.Fatalf("want ErrIncompleteIdentity, got %v (payload %+v)", err, events)
			}
			if events != nil {
				t.Fatalf("a refused build must return no payload, got %+v", events)
			}
		})
	}
}

// TestIncompleteIdentityCandidateClearsThePayload: the same fault path as an
// unusable viewer identity — the session stops beaconing rather than replaying
// an older payload against an identity the refresh could not confirm.
func TestIncompleteIdentityCandidateClearsThePayload(t *testing.T) {
	s := NewStream()
	obs := s.BeginSessionObservation()
	good, err := PlaybackSessionCandidate{BroadcastID: "b1"}.
		WithSpadeURL("https://spade.twitch.tv/track").
		WithPayload("123456", "b1", contractUserID, "cyganzor", nil, contractClock())
	if err != nil {
		t.Fatalf("valid build must succeed: %v", err)
	}
	if r := s.ApplyPlaybackSessionIfCurrent(obs, good, ExpectedSession{}); !r.Applied {
		t.Fatalf("valid session must apply, got %+v", r)
	}

	obs2 := s.BeginSessionObservation()
	bad, err := PlaybackSessionCandidate{}.WithPayload("123456", "", contractUserID, "cyganzor", nil, contractClock())
	if !errors.Is(err, ErrIncompleteIdentity) {
		t.Fatalf("want ErrIncompleteIdentity, got %v", err)
	}
	if r := s.ApplyPlaybackSessionIfCurrent(obs2, bad, ExpectedSession{}); !r.Applied {
		t.Fatalf("the faulted candidate must apply to clear, got %+v", r)
	}
	if s.SessionSnapshot().HasPayload() {
		t.Fatal("an unconfirmable watch identity must clear the payload, not keep replaying it")
	}
}

// TestBeaconBytesDoNotHTMLEscape: encoding/json escapes '&', '<' and '>' by
// default; both reference implementations put them through literally, and real
// game names contain them ("Dungeons & Dragons"). The encoded beacon must carry
// the literal characters.
func TestBeaconBytesDoNotHTMLEscape(t *testing.T) {
	s := NewStream()
	s.Update("987654321", "t", nil, nil, 1)
	if err := s.SetPayload("123456", "987654321", contractUserID, "cyganzor", &Game{ID: "509658", Name: "Dungeons & Dragons <Online>"}, contractClock()); err != nil {
		t.Fatalf("payload must build: %v", err)
	}
	encoded, err := s.SessionSnapshot().EncodePayload()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	body := string(decoded)
	for _, escaped := range []string{`\u0026`, `\u003c`, `\u003e`} {
		if strings.Contains(body, escaped) {
			t.Errorf("beacon bytes must not HTML-escape (%s found): %s", escaped, body)
		}
	}
	if !strings.Contains(body, `"game":"Dungeons & Dragons <Online>"`) {
		t.Errorf("game name must appear literally: %s", body)
	}
	// The bytes must still be valid JSON that decodes to the original string.
	var events []MinuteWatchedEvent
	if err := json.Unmarshal(decoded, &events); err != nil {
		t.Fatalf("beacon bytes must remain valid JSON: %v", err)
	}
	if events[0].Properties.Game != "Dungeons & Dragons <Online>" {
		t.Errorf("game round-trip mismatch: %q", events[0].Properties.Game)
	}
	// And no trailing newline from the encoder.
	if strings.HasSuffix(body, "\n") {
		t.Error("encoded beacon must not carry a trailing newline")
	}
}
