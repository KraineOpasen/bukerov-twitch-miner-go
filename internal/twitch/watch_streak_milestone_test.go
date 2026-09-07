package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
)

// fullRewardListResponse is a complete, well-formed RewardList response with
// every audited field present. Values are arbitrary observed data: this file
// asserts that they are REPORTED, never that they mean anything.
func fullRewardListResponse() map[string]interface{} {
	return map[string]interface{}{
		"data": map[string]interface{}{
			"channel": map[string]interface{}{
				"id": "12345",
				"self": map[string]interface{}{
					"watchStreakMilestone": map[string]interface{}{
						"watchStreakThreshold": float64(3),
						"watchStreakCopoBonus": float64(450),
						"state":                "ACTIVE",
						"expiresAt":            "2026-09-10T00:00:00Z",
						"missedStreams": []interface{}{
							map[string]interface{}{
								"broadcastIdentifiers": []interface{}{
									map[string]interface{}{"id": "b-1"},
									map[string]interface{}{"id": "b-2"},
								},
							},
						},
						"watchStreakMilestone": map[string]interface{}{
							"id": "milestone-7",
							// String-encoded on purpose: the donor evidence says this
							// is the form Twitch actually sends for this field, so the
							// suite's "well-formed live response" must model it.
							//
							// The raw-JSON response fixtures further down this file
							// deliberately keep "value":4. Converting them too would
							// just move the blind spot rather than close it: the
							// number form is real tolerance, and those bodies are
							// where it stays exercised inside a live-shaped response
							// rather than only in a dedicated table test.
							"value":                "4",
							"achievementTimestamp": "2026-09-05T12:00:00Z",
							"shareStatus":          "UNSHARED",
						},
					},
				},
			},
		},
	}
}

// selfNode reaches into a response fixture and returns data.channel.self so a
// test can mutate exactly one node.
func selfNode(t *testing.T, resp map[string]interface{}) map[string]interface{} {
	t.Helper()
	return resp["data"].(map[string]interface{})["channel"].(map[string]interface{})["self"].(map[string]interface{})
}

// sNode reaches into a response fixture and returns
// data.channel.self.watchStreakMilestone (S).
func sNode(t *testing.T, resp map[string]interface{}) map[string]interface{} {
	t.Helper()
	return selfNode(t, resp)["watchStreakMilestone"].(map[string]interface{})
}

func TestParseWatchStreakMilestoneFullSnapshot(t *testing.T) {
	snap := parseWatchStreakMilestone(fullRewardListResponse())

	for _, tc := range []struct {
		name string
		got  MilestoneFieldPresence
	}{
		{"data", snap.DataPresence},
		{"channel", snap.ChannelPresence},
		{"self", snap.SelfPresence},
		{"S", snap.SelfMilestonePresence},
		{"V", snap.MilestoneNodePresence},
	} {
		if tc.got != MilestoneFieldValid {
			t.Errorf("%s presence = %q, want VALID", tc.name, tc.got)
		}
	}

	if snap.ChannelID.Presence != MilestoneFieldValid || snap.ChannelID.Value != "12345" {
		t.Errorf("channel id = %+v, want VALID/12345", snap.ChannelID)
	}
	if snap.MilestoneID.Presence != MilestoneFieldValid || snap.MilestoneID.Value != "milestone-7" {
		t.Errorf("milestone id = %+v", snap.MilestoneID)
	}
	if snap.MilestoneValue.Presence != MilestoneFieldValid || snap.MilestoneValue.Value != 4 {
		t.Errorf("milestone value = %+v", snap.MilestoneValue)
	}
	if snap.AchievementTimestamp.Presence != MilestoneFieldValid ||
		snap.AchievementTimestamp.Value != "2026-09-05T12:00:00Z" {
		t.Errorf("achievementTimestamp = %+v", snap.AchievementTimestamp)
	}
	if snap.ShareStatus.Presence != MilestoneFieldValid || snap.ShareStatus.Value != "UNSHARED" {
		t.Errorf("shareStatus = %+v", snap.ShareStatus)
	}
	if snap.WatchStreakThreshold.Presence != MilestoneFieldValid || snap.WatchStreakThreshold.Value != 3 {
		t.Errorf("watchStreakThreshold = %+v", snap.WatchStreakThreshold)
	}
	if snap.WatchStreakCopoBonus.Presence != MilestoneFieldValid || snap.WatchStreakCopoBonus.Value != 450 {
		t.Errorf("watchStreakCopoBonus = %+v", snap.WatchStreakCopoBonus)
	}
	if snap.State.Presence != MilestoneFieldValid || snap.State.Value != "ACTIVE" {
		t.Errorf("state = %+v", snap.State)
	}
	if snap.ExpiresAt.Presence != MilestoneFieldValid || snap.ExpiresAt.Value != "2026-09-10T00:00:00Z" {
		t.Errorf("expiresAt = %+v", snap.ExpiresAt)
	}
	if snap.MissedStreams.Presence != MilestoneFieldValid || snap.MissedStreams.Count != 1 {
		t.Fatalf("missedStreams = %+v, want VALID/1", snap.MissedStreams)
	}
	ids := snap.MissedStreams.Entries[0].BroadcastIdentifiers
	if ids.Presence != MilestoneFieldValid || ids.Count != 2 || ids.MalformedCount != 0 ||
		strings.Join(ids.IDs, ",") != "b-1,b-2" {
		t.Errorf("broadcastIdentifiers = %+v", ids)
	}
}

// TestParseWatchStreakMilestoneNodeAbsence covers the required distinction that
// MISSING, NULL, VALID and MALFORMED are four different observations at every
// node on the path — including S itself and the nested achievement node V.
func TestParseWatchStreakMilestoneNodeAbsence(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(t *testing.T, resp map[string]interface{})
		wantS   MilestoneFieldPresence
		wantV   MilestoneFieldPresence
		wantSub MilestoneFieldPresence // presence every S/V child must report
	}{
		{
			name:    "S missing",
			mutate:  func(t *testing.T, r map[string]interface{}) { delete(selfNode(t, r), "watchStreakMilestone") },
			wantS:   MilestoneFieldMissing,
			wantV:   MilestoneFieldMissing,
			wantSub: MilestoneFieldMissing,
		},
		{
			name:    "S null",
			mutate:  func(t *testing.T, r map[string]interface{}) { selfNode(t, r)["watchStreakMilestone"] = nil },
			wantS:   MilestoneFieldNull,
			wantV:   MilestoneFieldMissing,
			wantSub: MilestoneFieldMissing,
		},
		{
			name:    "S malformed scalar",
			mutate:  func(t *testing.T, r map[string]interface{}) { selfNode(t, r)["watchStreakMilestone"] = "nope" },
			wantS:   MilestoneFieldMalformed,
			wantV:   MilestoneFieldMissing,
			wantSub: MilestoneFieldMissing,
		},
		{
			name:    "V missing",
			mutate:  func(t *testing.T, r map[string]interface{}) { delete(sNode(t, r), "watchStreakMilestone") },
			wantS:   MilestoneFieldValid,
			wantV:   MilestoneFieldMissing,
			wantSub: MilestoneFieldMissing,
		},
		{
			name:    "V null",
			mutate:  func(t *testing.T, r map[string]interface{}) { sNode(t, r)["watchStreakMilestone"] = nil },
			wantS:   MilestoneFieldValid,
			wantV:   MilestoneFieldNull,
			wantSub: MilestoneFieldMissing,
		},
		{
			name:    "V malformed array",
			mutate:  func(t *testing.T, r map[string]interface{}) { sNode(t, r)["watchStreakMilestone"] = []interface{}{} },
			wantS:   MilestoneFieldValid,
			wantV:   MilestoneFieldMalformed,
			wantSub: MilestoneFieldMissing,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := fullRewardListResponse()
			tc.mutate(t, resp)
			snap := parseWatchStreakMilestone(resp)

			if snap.SelfMilestonePresence != tc.wantS {
				t.Errorf("S presence = %q, want %q", snap.SelfMilestonePresence, tc.wantS)
			}
			if snap.MilestoneNodePresence != tc.wantV {
				t.Errorf("V presence = %q, want %q", snap.MilestoneNodePresence, tc.wantV)
			}
			// Every child of an absent node must report the child's own absence
			// classification and carry NO fabricated value.
			if tc.wantS != MilestoneFieldValid {
				assertNoFabricatedValues(t, snap)
				if snap.State.Presence != tc.wantSub {
					t.Errorf("state presence = %q, want %q", snap.State.Presence, tc.wantSub)
				}
			}
			if snap.MilestoneID.Presence != tc.wantSub {
				t.Errorf("milestone id presence = %q, want %q", snap.MilestoneID.Presence, tc.wantSub)
			}
			if snap.MilestoneValue.Presence != tc.wantSub {
				t.Errorf("milestone value presence = %q, want %q", snap.MilestoneValue.Presence, tc.wantSub)
			}
			if snap.MilestoneValue.Value != 0 && snap.MilestoneValue.Presence != MilestoneFieldValid {
				t.Errorf("non-valid milestone value carried a value: %+v", snap.MilestoneValue)
			}
		})
	}
}

// assertNoFabricatedValues proves the parser never coerces an unobserved node
// into a usable-looking zero/empty value: every non-VALID field must carry the
// zero value AND a presence that says it was not observed.
func assertNoFabricatedValues(t *testing.T, snap WatchStreakMilestoneSnapshot) {
	t.Helper()
	for name, f := range map[string]MilestoneStringField{
		"milestoneId":          snap.MilestoneID,
		"achievementTimestamp": snap.AchievementTimestamp,
		"shareStatus":          snap.ShareStatus,
		"state":                snap.State,
		"expiresAt":            snap.ExpiresAt,
	} {
		if f.Presence != MilestoneFieldValid && f.Value != "" {
			t.Errorf("%s: non-valid field carries value %q", name, f.Value)
		}
	}
	for name, f := range map[string]MilestoneIntField{
		"milestoneValue":       snap.MilestoneValue,
		"watchStreakThreshold": snap.WatchStreakThreshold,
		"watchStreakCopoBonus": snap.WatchStreakCopoBonus,
	} {
		if f.Presence != MilestoneFieldValid && f.Value != 0 {
			t.Errorf("%s: non-valid field carries value %d", name, f.Value)
		}
		// The wire kind is an observation of a value that arrived. A field that
		// did not validly arrive has none, and must not carry a defaulted one.
		if f.Presence != MilestoneFieldValid && f.WireKind != MilestoneWireKindUnset {
			t.Errorf("%s: non-valid field carries wire kind %q", name, f.WireKind)
		}
	}
}

// assertEmptySnapshot proves a non-parsing outcome carried NO snapshot at all:
// a failed observation must not leave behind a zero-valued record that could be
// misread as "Twitch said the milestone is empty".
func assertEmptySnapshot(t *testing.T, snap WatchStreakMilestoneSnapshot) {
	t.Helper()
	if !reflect.DeepEqual(snap, WatchStreakMilestoneSnapshot{}) {
		t.Fatalf("failed observation carried a snapshot: %+v", snap)
	}
}

// TestParseWatchStreakMilestoneMalformedScalars proves a wrong-typed or
// non-integral scalar is MALFORMED, never truncated, rounded or defaulted.
func TestParseWatchStreakMilestoneMalformedScalars(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value interface{}
	}{
		{"value as bool", "value", true},
		{"value as object", "value", map[string]interface{}{}},
		{"value as array", "value", []interface{}{}},
		{"value fractional", "value", 450.5},
		{"value NaN", "value", math.NaN()},
		{"value +Inf", "value", math.Inf(1)},
		{"value beyond exact integer range", "value", math.Pow(2, 53)},
		// The value node accepts a string of digits (see
		// TestParseWatchStreakMilestoneValueAcceptsBothObservedWireKinds), which
		// is exactly why a string that is NOT one must still be MALFORMED. The
		// tolerance is for one documented wire encoding, not a licence to guess
		// at a number.
		{"value as non-numeric string", "value", "four"},
		{"value as empty string", "value", ""},
		{"value as fractional string", "value", "4.0"},
		{"value as padded numeric string", "value", " 4"},
		{"value as hexadecimal string", "value", "0x4"},
		{"value as thousands-separated string", "value", "1,000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := fullRewardListResponse()
			sNode(t, resp)["watchStreakMilestone"].(map[string]interface{})[tc.key] = tc.value
			snap := parseWatchStreakMilestone(resp)
			// The whole field is compared, not just its presence: a
			// MALFORMED node must carry no value AND no wire kind. Reporting
			// the encoding of a value the parser refused would say Twitch sent
			// a well-formed number after all.
			if got := snap.MilestoneValue; got != (MilestoneIntField{Presence: MilestoneFieldMalformed}) {
				t.Fatalf("value %v observed as %+v, want MALFORMED with no value and no wire kind",
					tc.value, got)
			}
		})
	}

	// An explicitly NULL integer is NULL, not MISSING and not MALFORMED: the
	// three are different observations for every integer field.
	for _, key := range []string{"value"} {
		resp := fullRewardListResponse()
		sNode(t, resp)["watchStreakMilestone"].(map[string]interface{})[key] = nil
		if got := parseWatchStreakMilestone(resp).MilestoneValue; got != (MilestoneIntField{Presence: MilestoneFieldNull}) {
			t.Fatalf("null %s = %+v, want NULL with no value and no wire kind", key, got)
		}
	}
	for _, key := range []string{"watchStreakThreshold", "watchStreakCopoBonus"} {
		resp := fullRewardListResponse()
		sNode(t, resp)[key] = nil
		snap := parseWatchStreakMilestone(resp)
		got := snap.WatchStreakThreshold
		if key == "watchStreakCopoBonus" {
			got = snap.WatchStreakCopoBonus
		}
		if got != (MilestoneIntField{Presence: MilestoneFieldNull}) {
			t.Fatalf("null %s = %+v, want NULL with no value and no wire kind", key, got)
		}
		// And NULL must stay distinct from MISSING on the same field.
		missing := fullRewardListResponse()
		delete(sNode(t, missing), key)
		msnap := parseWatchStreakMilestone(missing)
		mgot := msnap.WatchStreakThreshold
		if key == "watchStreakCopoBonus" {
			mgot = msnap.WatchStreakCopoBonus
		}
		if mgot != (MilestoneIntField{Presence: MilestoneFieldMissing}) {
			t.Fatalf("missing %s = %+v, want MISSING with no value and no wire kind", key, mgot)
		}
	}

	// The value node's string tolerance must NOT leak to its siblings. Twitch
	// sends watchStreakThreshold and watchStreakCopoBonus as JSON numbers, so a
	// string there is a genuine shape surprise and has to keep reading as one.
	for _, key := range []string{"watchStreakThreshold", "watchStreakCopoBonus"} {
		resp := fullRewardListResponse()
		sNode(t, resp)[key] = "4"
		snap := parseWatchStreakMilestone(resp)
		got := snap.WatchStreakThreshold
		if key == "watchStreakCopoBonus" {
			got = snap.WatchStreakCopoBonus
		}
		if got != (MilestoneIntField{Presence: MilestoneFieldMalformed}) {
			t.Fatalf("string-encoded %s = %+v, want MALFORMED with no value and no wire kind", key, got)
		}
	}

	// A string field given a non-string is MALFORMED, not an empty observed
	// string.
	resp := fullRewardListResponse()
	sNode(t, resp)["watchStreakMilestone"].(map[string]interface{})["shareStatus"] = float64(1)
	if got := parseWatchStreakMilestone(resp).ShareStatus; got.Presence != MilestoneFieldMalformed || got.Value != "" {
		t.Fatalf("shareStatus = %+v, want MALFORMED/empty", got)
	}

	// A genuinely observed empty string stays VALID: it is data, not absence.
	resp = fullRewardListResponse()
	sNode(t, resp)["watchStreakMilestone"].(map[string]interface{})["shareStatus"] = ""
	if got := parseWatchStreakMilestone(resp).ShareStatus; got.Presence != MilestoneFieldValid || got.Value != "" {
		t.Fatalf("observed empty string = %+v, want VALID/empty", got)
	}
}

// captureClientLogs redirects the default slog logger into a buffer for the
// duration of one test.
func captureClientLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

// TestDiagnosticReadDoesNotRotateTheSharedClientIDDefault is the isolation
// proof for the one piece of shared transport state a diagnostic read could
// still move.
//
// c.defaultClientID is process-wide: candidateClientIDs feeds it to EVERY
// uncached operation, business ones included, and ActiveClientID surfaces it to
// the operator. Promotion happens whenever a fallback candidate answers
// anything that is not PersistedQueryNotFound - which includes a 401, so a
// diagnostic observation that FAILED can still move it and still raise the
// operator WARN saying the shipped hashes are stale, when nothing resolved
// anything.
//
// That matters here more than anywhere else: RewardList's live acceptance is
// PENDING, so "PQNF on the default, something else on a fallback" is this
// operation's EXPECTED shape, once per online streamer per cycle.
func TestDiagnosticReadDoesNotRotateTheSharedClientIDDefault(t *testing.T) {
	const pqnf = `{"errors":[{"message":"PersistedQueryNotFound","extensions":{"code":"PERSISTED_QUERY_NOT_FOUND"}}]}`
	const staleHashWarn = "persisted-query hashes in internal/constants/gql.go are likely stale"

	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		// The observation SUCCEEDS on the fallback.
		{"fallback answers", http.StatusOK,
			`{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":null}}}}`},
		// The observation FAILS on the fallback. Promotion must not happen here
		// either: nothing was resolved, so there is nothing to promote.
		{"fallback rejects", http.StatusUnauthorized, `{"message":"unauthorized"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureClientLogs(t)
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Client-Id") == constants.ClientIDTV {
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, pqnf)
					return
				}
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			})

			before := c.ActiveClientID()
			_ = c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

			if got := c.ActiveClientID(); got != before {
				t.Errorf("a diagnostic read rotated the shared client-ID default from %q to %q; "+
					"the next uncached BUSINESS operation would go out under it", before, got)
			}
			if strings.Contains(logs.String(), staleHashWarn) {
				t.Errorf("a diagnostic read raised the operator stale-hash rotation WARN; "+
					"RewardList acceptance is PENDING, so this alert is not actionable\nlogs:\n%s",
					logs.String())
			}
		})
	}

	// The other half: the rule applies ONLY to the diagnostic read. A business
	// operation that genuinely resolves on a fallback must still rotate the
	// default and still tell the operator, exactly as before.
	t.Run("business read still rotates and warns", func(t *testing.T) {
		logs := captureClientLogs(t)
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Client-Id") == constants.ClientIDTV {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, pqnf)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w,
				`{"data":{"community":{"channel":{"self":{"communityPoints":{"balance":7,"availableClaim":null}}}}}}`)
		})

		before := c.ActiveClientID()
		_ = c.LoadChannelPointsContext(newTestStreamer("somestreamer"))

		if got := c.ActiveClientID(); got == before {
			t.Errorf("a business read resolved on a fallback but did not rotate the default (still %q); "+
				"the diagnostic guard leaked onto the business path", got)
		}
		if !strings.Contains(logs.String(), staleHashWarn) {
			t.Error("a business rotation no longer raises the operator stale-hash WARN; " +
				"the diagnostic guard leaked onto the business path")
		}
	})
}

// TestObservationFailsClosedOnAMalformedErrorsNode covers a rejection wearing
// the wrong shape.
//
// gql.HasTopLevelErrors asks "is there a NON-EMPTY ARRAY at errors", so a
// present errors node that is an object, a string or null answers false. A
// GraphQL errors node is only ever valid as an array, so every other shape is
// a rejection this parser must not walk past: accepting it would turn a
// definitively refused request into an observed milestone.
func TestObservationFailsClosedOnAMalformedErrorsNode(t *testing.T) {
	for _, tc := range []struct {
		name string
		errs string
	}{
		{"object", `{"message":"denied"}`},
		{"string", `"denied"`},
		{"null", `null`},
		{"number", `7`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"errors":` + tc.errs + `,"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":null}}}}`
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, body)
			})

			obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")
			if obs.Outcome == MilestoneObserved {
				t.Fatalf("a present but malformed errors node (%s) was recorded as OBSERVED; "+
					"a refused request became evidence", tc.name)
			}
			assertEmptySnapshot(t, obs.Snapshot)
		})
	}

	// A present, well-formed, EMPTY array is not a rejection: it carries no
	// error, so the data alongside it is still a real observation.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w,
			`{"errors":[],"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":null}}}}`)
	})
	if obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "x"); obs.Outcome != MilestoneObserved {
		t.Errorf("an empty errors array made the observation %q; an empty array reports no error",
			obs.Outcome)
	}
}

// TestObservationRequiresAnHTTPSuccessStatus covers a rejection whose body
// merely LOOKS like a GraphQL response.
//
// The shared transport special-cases only 401 and 403; every other non-2xx body
// that happens to be JSON comes back verbatim. So a 400 or 404 whose payload
// carries an object-valued data key would satisfy the data-presence check and
// be recorded as an observation. A hostile endpoint or proxy could therefore
// manufacture diagnostic evidence out of a request the server refused.
func TestObservationRequiresAnHTTPSuccessStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound,
		http.StatusMovedPermanently, http.StatusTeapot} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w,
					`{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{"watchStreakMilestone":{"value":"9"}}}}}}`)
			})

			obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")
			if obs.Outcome == MilestoneObserved {
				t.Fatalf("HTTP %d carrying a data-shaped body was recorded as OBSERVED; "+
					"a refused request became evidence", status)
			}
			assertEmptySnapshot(t, obs.Snapshot)
		})
	}
}

// TestDiagnosticRetryTraceCarriesNoRawErrorText holds the diagnostic read to its
// own stated privacy bound.
//
// The retry trace is lowered to DEBUG for a diagnostic read, but FileLevel
// defaults to DEBUG, so a lowered record still reaches the retained log. If it
// still attaches the transport error verbatim, an unbounded attacker-shaped
// string - a redirect URL, a proxy banner - lands in that log unsanitized,
// which is exactly what "arbitrary raw error strings are never logged" rules
// out. A bounded status is enough to diagnose a retry.
func TestDiagnosticRetryTraceCarriesNoRawErrorText(t *testing.T) {
	const retryMsg = "GQL request failed, retrying"

	logs := captureClientLogs(t)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
	})
	_ = c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, retryMsg) {
			continue
		}
		if strings.Contains(line, "error=") {
			t.Errorf("a diagnostic retry trace carried a raw transport error string:\n%s", line)
		}
	}

	// The business half keeps its full trace: the raw error is actionable there
	// and the operator is meant to see it.
	logs.Reset()
	business := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
	})
	_ = business.LoadChannelPointsContext(newTestStreamer("somestreamer"))

	sawBusinessError := false
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, retryMsg) && strings.Contains(line, "error=") {
			sawBusinessError = true
		}
	}
	if !sawBusinessError {
		t.Error("a business retry trace no longer carries its error; the diagnostic bound leaked " +
			"onto the business path")
	}
}

// TestDiagnosticResponseBodyIsCapped proves a diagnostic read will not pull an
// unbounded body into memory, and will not record a partially-read response as a
// whole observation.
//
// The HTTP status is only known once the response arrives, so refusing a non-2xx
// on its status does not by itself stop the body being read first. A hostile
// endpoint or proxy could answer a rejected diagnostic with an enormous body and
// have it read in full, on the bonus poll goroutine, before anything classified
// it. A diagnostic read therefore refuses a body past maxDiagnosticResponseBytes
// rather than trusting it.
//
// Both cases serve VALID JSON that would parse into a real observation, which is
// what makes this a test of the bound: unbounded, each is read whole and the
// observation succeeds. A body of junk bytes would fail either way and would
// prove nothing.
//
// The second case is the one a plain io.LimitReader gets wrong. A limited reader
// signals a truncated stream and a complete one identically - both end in a
// clean EOF - so a document that is complete at exactly the limit, followed by
// arbitrary bytes, decodes from its prefix and is recorded as OBSERVED even
// though the response was never read whole. Reading limit+1 is what tells the
// two apart.
func TestDiagnosticResponseBodyIsCapped(t *testing.T) {
	const (
		prefix = `{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":null}}},"pad":"`
		suffix = `"}`
	)
	// A complete document of exactly n bytes.
	docOfSize := func(t *testing.T, n int) string {
		t.Helper()
		doc := prefix + strings.Repeat("A", n-len(prefix)-len(suffix)) + suffix
		if len(doc) != n {
			t.Fatalf("built a %d-byte document, want exactly %d", len(doc), n)
		}
		return doc
	}

	for _, tc := range []struct {
		name string
		body func(t *testing.T) string
	}{
		{
			// Far past the limit: truncation lands mid-document.
			name: "a document far past the limit",
			body: func(t *testing.T) string { return docOfSize(t, 3*maxDiagnosticResponseBytes) },
		},
		{
			// Exactly at the limit, then one more byte. The prefix is a complete,
			// decodable document, so only a reader that can SEE the overflow
			// refuses it.
			name: "a document ending exactly at the limit, plus one byte",
			body: func(t *testing.T) string { return docOfSize(t, maxDiagnosticResponseBytes) + "Z" },
		},
		{
			// One byte over, and still whole. This case pins the SIZE CHECK
			// rather than the extra byte of read: everything read decodes, so
			// refusing it requires comparing the length against the limit.
			name: "a whole document one byte over the limit",
			body: func(t *testing.T) string { return docOfSize(t, maxDiagnosticResponseBytes+1) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body(t)
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, body)
			})

			obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

			if obs.Outcome == MilestoneObserved {
				t.Fatalf("a %d-byte body (limit %d) was recorded as OBSERVED; a diagnostic read "+
					"accepted a response it did not read whole",
					len(body), maxDiagnosticResponseBytes)
			}
			assertEmptySnapshot(t, obs.Snapshot)
		})
	}
}

// TestParseWatchStreakMilestoneValueAcceptsBothObservedWireKinds is the MAJOR-3
// fidelity proof for the one field this observation exists to see.
//
// RewardList is not uniform about its integers: the donor protocol evidence has
// ViewerMilestone.value as a STRING, while its siblings watchStreakThreshold and
// watchStreakCopoBonus arrive as JSON integers. The string is therefore the form
// attested for THIS field, and a parser that accepts only the integer form does
// not report a shape problem with Twitch's response - it throws away the
// documented wire form: "value":"4" would classify MALFORMED and the observed 4
// would never reach the record.
//
// The number form is accepted as well, but as TOLERANCE, not as an observed
// wire fact: nothing attests it for this field. If Twitch ever does send it, the
// recorded wire kind is what makes that visible instead of silent.
//
// Accepting both encodings is protocol tolerance, not interpretation. Nothing
// here decides that the value IS the streak count; which field carries the
// authoritative count stays UNKNOWN.
//
// This test names only Presence and Value, so it compiles unchanged on the
// pre-repair candidate and fails there behaviourally.
func TestParseWatchStreakMilestoneValueAcceptsBothObservedWireKinds(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire interface{}
	}{
		{"json string", "4"},
		{"json integer", float64(4)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := fullRewardListResponse()
			sNode(t, resp)["watchStreakMilestone"].(map[string]interface{})["value"] = tc.wire
			got := parseWatchStreakMilestone(resp).MilestoneValue
			if got.Presence != MilestoneFieldValid || got.Value != 4 {
				t.Fatalf("value %#v observed as %+v, want VALID/4 - the observed value was lost", tc.wire, got)
			}
		})
	}
}

// TestParseWatchStreakMilestoneValueRecordsItsWireKind proves the two accepted
// encodings stay told apart after normalisation.
//
// Normalising "4" and 4 to the same integer is the point; losing WHICH form
// Twitch sent is not. The wire kind is protocol evidence in its own right — it
// is how a later reader can tell a schema change from a parser bug — so it gets
// its own slot rather than being inferred back out of the value.
func TestParseWatchStreakMilestoneValueRecordsItsWireKind(t *testing.T) {
	for _, tc := range []struct {
		name         string
		wire         interface{}
		wantValue    int
		wantWireKind MilestoneWireKind
	}{
		{"digits", "4", 4, MilestoneWireKindString},
		{"signed digits", "-4", -4, MilestoneWireKindString},
		{"zero digits", "0", 0, MilestoneWireKindString},
		{"leading zero digits", "04", 4, MilestoneWireKindString},
		{"json number", float64(4), 4, MilestoneWireKindNumber},
		{"json zero", float64(0), 0, MilestoneWireKindNumber},
		{"negative json number", float64(-4), -4, MilestoneWireKindNumber},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := fullRewardListResponse()
			sNode(t, resp)["watchStreakMilestone"].(map[string]interface{})["value"] = tc.wire
			got := parseWatchStreakMilestone(resp).MilestoneValue
			want := MilestoneIntField{
				Presence: MilestoneFieldValid,
				Value:    tc.wantValue,
				WireKind: tc.wantWireKind,
			}
			if got != want {
				t.Fatalf("value %#v observed as %+v, want %+v", tc.wire, got, want)
			}
		})
	}

	// A value of 0 is a genuinely observed 0 and stays distinguishable from the
	// three not-observed classifications, which carry no value and no wire kind.
	for _, tc := range []struct {
		name string
		mut  func(v map[string]interface{})
		want MilestoneIntField
	}{
		{"missing", func(v map[string]interface{}) { delete(v, "value") },
			MilestoneIntField{Presence: MilestoneFieldMissing}},
		{"null", func(v map[string]interface{}) { v["value"] = nil },
			MilestoneIntField{Presence: MilestoneFieldNull}},
		{"malformed", func(v map[string]interface{}) { v["value"] = true },
			MilestoneIntField{Presence: MilestoneFieldMalformed}},
	} {
		resp := fullRewardListResponse()
		tc.mut(sNode(t, resp)["watchStreakMilestone"].(map[string]interface{}))
		if got := parseWatchStreakMilestone(resp).MilestoneValue; got != tc.want {
			t.Fatalf("%s value = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestParseWatchStreakMilestoneValueExactnessFollowsTheEncoding pins the one
// deliberate asymmetry between the two accepted encodings.
//
// A JSON number decodes through float64, so at and above 2^53 it has already
// lost the identity of the value that was actually sent; the parser refuses it
// rather than reporting a number Twitch may never have written. A decimal
// STRING has lost nothing, so the same magnitude is exactly observable there.
// The asymmetry is a property of the encodings, not an oversight, and this test
// exists so a later reader cannot "fix" one side into agreement with the other.
func TestParseWatchStreakMilestoneValueExactnessFollowsTheEncoding(t *testing.T) {
	// The same magnitude as a JSON number is not exactly observable on any
	// platform: float64 has already lost its identity by the time it is decoded.
	resp := fullRewardListResponse()
	sNode(t, resp)["watchStreakMilestone"].(map[string]interface{})["value"] = float64(1 << 53)
	if got := parseWatchStreakMilestone(resp).MilestoneValue; got.Presence != MilestoneFieldMalformed {
		t.Fatalf("number-encoded 2^53 = %+v, want MALFORMED", got)
	}

	// The string path's ceiling is the PLATFORM int, not 2^53, so where the two
	// encodings disagree depends on the build. On a 64-bit int the string is
	// still exact at 2^53+1; on a 32-bit int the platform bound bites first and
	// both encodings refuse. Asserting the 64-bit case unconditionally would
	// fail a 32-bit build for doing the right thing.
	if strconv.IntSize < 54 {
		t.Skipf("int is %d bits: the platform bound refuses 2^53+1 before the "+
			"exactness difference can show", strconv.IntSize)
	}
	const beyondFloat64Exact = "9007199254740993" // 2^53 + 1
	resp = fullRewardListResponse()
	sNode(t, resp)["watchStreakMilestone"].(map[string]interface{})["value"] = beyondFloat64Exact
	got := parseWatchStreakMilestone(resp).MilestoneValue
	if got.Presence != MilestoneFieldValid || got.WireKind != MilestoneWireKindString {
		t.Fatalf("string-encoded %s = %+v, want VALID/STRING", beyondFloat64Exact, got)
	}
	if strconv.Itoa(got.Value) != beyondFloat64Exact {
		t.Fatalf("string-encoded %s round-tripped to %d; a decimal string must be read exactly",
			beyondFloat64Exact, got.Value)
	}
}

// TestParseWatchStreakMilestoneValueRefusesAnOutOfRangeDigitRun pins the
// string path's ceiling, on every platform.
//
// This is the one case where strconv.Atoi is actively dangerous: for a digit
// run too large for the platform int it returns the SATURATED magnitude
// alongside ErrRange, so dropping the error check would record MaxInt as an
// observed milestone value. "A signed run of decimal digits" is therefore NOT
// sufficient for VALID, and the parser must say MALFORMED rather than report a
// number Twitch never sent.
func TestParseWatchStreakMilestoneValueRefusesAnOutOfRangeDigitRun(t *testing.T) {
	for _, digits := range []string{
		strconv.FormatUint(uint64(math.MaxInt64)+1, 10), // 2^63, above a 64-bit int
		"-9223372036854775809",                          // below a 64-bit int
		"99999999999999999999999999999999",              // above any platform int
	} {
		resp := fullRewardListResponse()
		sNode(t, resp)["watchStreakMilestone"].(map[string]interface{})["value"] = digits
		got := parseWatchStreakMilestone(resp).MilestoneValue
		if got != (MilestoneIntField{Presence: MilestoneFieldMalformed}) {
			t.Fatalf("out-of-range digit run %s = %+v, want MALFORMED with no value "+
				"and no wire kind; strconv.Atoi saturates, so a dropped error check "+
				"would report a fabricated %d here", digits, got, got.Value)
		}
	}
}

// TestParseWatchStreakMilestoneSiblingsRecordTheNumberWireKind pins the wire
// kind the number-encoded siblings carry.
//
// They are not rendered into any record today, which is exactly why they need
// pinning: without this, a change that reported watchStreakThreshold as
// STRING - a factually false encoding for a JSON number - would pass the whole
// suite and only surface if someone later surfaced the field.
func TestParseWatchStreakMilestoneSiblingsRecordTheNumberWireKind(t *testing.T) {
	snap := parseWatchStreakMilestone(fullRewardListResponse())
	for name, got := range map[string]MilestoneIntField{
		"watchStreakThreshold": snap.WatchStreakThreshold,
		"watchStreakCopoBonus": snap.WatchStreakCopoBonus,
	} {
		if got.Presence != MilestoneFieldValid || got.WireKind != MilestoneWireKindNumber {
			t.Errorf("%s = %+v, want VALID with wire kind NUMBER", name, got)
		}
	}
	// And the value node in the same canonical response is the string form,
	// so the two encodings are exercised side by side in one live-shaped body.
	if got := snap.MilestoneValue; got.Presence != MilestoneFieldValid ||
		got.WireKind != MilestoneWireKindString || got.Value != 4 {
		t.Errorf("milestone value = %+v, want VALID/4/STRING", got)
	}
}

// TestParseWatchStreakMilestoneMissedStreams covers the required
// missing/null/empty/valid/malformed matrix for the container, keeping EMPTY
// distinct from MISSING and NULL.
func TestParseWatchStreakMilestoneMissedStreams(t *testing.T) {
	tests := []struct {
		name          string
		value         interface{}
		absent        bool
		wantPresence  MilestoneFieldPresence
		wantCount     int
		wantMalformed int
	}{
		{name: "missing", absent: true, wantPresence: MilestoneFieldMissing},
		{name: "null", value: nil, wantPresence: MilestoneFieldNull},
		{name: "empty", value: []interface{}{}, wantPresence: MilestoneFieldEmpty},
		{name: "not an array", value: "nope", wantPresence: MilestoneFieldMalformed},
		{
			name:          "malformed element",
			value:         []interface{}{"nope", map[string]interface{}{}},
			wantPresence:  MilestoneFieldMalformed,
			wantCount:     2,
			wantMalformed: 1,
		},
		{
			// A null ELEMENT is a null, not a shape error — the same rule the
			// nested broadcastIdentifiers loop applies. It must not make the
			// container MALFORMED.
			name:         "null element is not a shape error",
			value:        []interface{}{nil, map[string]interface{}{}},
			wantPresence: MilestoneFieldValid,
			wantCount:    2,
		},
		{
			name:          "null element alongside a malformed one",
			value:         []interface{}{nil, "nope"},
			wantPresence:  MilestoneFieldMalformed,
			wantCount:     2,
			wantMalformed: 1,
		},
		{
			name: "valid",
			value: []interface{}{map[string]interface{}{
				"broadcastIdentifiers": []interface{}{map[string]interface{}{"id": "x"}},
			}},
			wantPresence: MilestoneFieldValid,
			wantCount:    1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := fullRewardListResponse()
			if tc.absent {
				delete(sNode(t, resp), "missedStreams")
			} else {
				sNode(t, resp)["missedStreams"] = tc.value
			}
			got := parseWatchStreakMilestone(resp).MissedStreams
			if got.Presence != tc.wantPresence {
				t.Errorf("presence = %q, want %q", got.Presence, tc.wantPresence)
			}
			if got.Count != tc.wantCount {
				t.Errorf("count = %d, want %d", got.Count, tc.wantCount)
			}
			if got.MalformedCount != tc.wantMalformed {
				t.Errorf("malformed = %d, want %d", got.MalformedCount, tc.wantMalformed)
			}
		})
	}
}

// TestParseWatchStreakMilestoneBroadcastIdentifiers covers the nested array,
// including partial data: well-formed ids survive, malformed elements are
// counted, and the container is flagged MALFORMED so nobody reads a partial
// list as complete.
func TestParseWatchStreakMilestoneBroadcastIdentifiers(t *testing.T) {
	tests := []struct {
		name          string
		entry         map[string]interface{}
		wantPresence  MilestoneFieldPresence
		wantIDs       string
		wantCount     int
		wantMalformed int
	}{
		{
			name:         "missing",
			entry:        map[string]interface{}{},
			wantPresence: MilestoneFieldMissing,
		},
		{
			name:         "null",
			entry:        map[string]interface{}{"broadcastIdentifiers": nil},
			wantPresence: MilestoneFieldNull,
		},
		{
			name:         "empty",
			entry:        map[string]interface{}{"broadcastIdentifiers": []interface{}{}},
			wantPresence: MilestoneFieldEmpty,
		},
		{
			name:         "not an array",
			entry:        map[string]interface{}{"broadcastIdentifiers": float64(3)},
			wantPresence: MilestoneFieldMalformed,
		},
		{
			name: "partially malformed keeps the well-formed ids and counts the rest",
			entry: map[string]interface{}{"broadcastIdentifiers": []interface{}{
				map[string]interface{}{"id": "keep-1"},
				"not-an-object",
				map[string]interface{}{"id": float64(9)},
				map[string]interface{}{},
				map[string]interface{}{"id": "keep-2"},
			}},
			// MalformedCount is SHAPE errors only: the non-object element and
			// the wrong-typed id. The element with no id at all is a MISSING
			// observation, not a shape error, and is carried by Elements.
			wantPresence:  MilestoneFieldMalformed,
			wantIDs:       "keep-1,keep-2",
			wantCount:     5,
			wantMalformed: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := fullRewardListResponse()
			sNode(t, resp)["missedStreams"] = []interface{}{tc.entry}
			got := parseWatchStreakMilestone(resp).MissedStreams.Entries[0].BroadcastIdentifiers
			if got.Presence != tc.wantPresence {
				t.Errorf("presence = %q, want %q", got.Presence, tc.wantPresence)
			}
			if strings.Join(got.IDs, ",") != tc.wantIDs {
				t.Errorf("ids = %v, want %q", got.IDs, tc.wantIDs)
			}
			if got.Count != tc.wantCount {
				t.Errorf("count = %d, want %d", got.Count, tc.wantCount)
			}
			if got.MalformedCount != tc.wantMalformed {
				t.Errorf("malformed = %d, want %d", got.MalformedCount, tc.wantMalformed)
			}
		})
	}
}

// TestParseWatchStreakMilestoneStructuralAbsence covers a response with no data
// node at all and one whose channel node is absent/null — neither may produce a
// fabricated snapshot.
func TestParseWatchStreakMilestoneStructuralAbsence(t *testing.T) {
	tests := []struct {
		name        string
		resp        map[string]interface{}
		wantData    MilestoneFieldPresence
		wantChannel MilestoneFieldPresence
	}{
		{"nil response", nil, MilestoneFieldMissing, MilestoneFieldMissing},
		{"empty response", map[string]interface{}{}, MilestoneFieldMissing, MilestoneFieldMissing},
		{"data null", map[string]interface{}{"data": nil}, MilestoneFieldNull, MilestoneFieldMissing},
		{
			"channel null",
			map[string]interface{}{"data": map[string]interface{}{"channel": nil}},
			MilestoneFieldValid, MilestoneFieldNull,
		},
		{
			"channel malformed",
			map[string]interface{}{"data": map[string]interface{}{"channel": []interface{}{}}},
			MilestoneFieldValid, MilestoneFieldMalformed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap := parseWatchStreakMilestone(tc.resp)
			if snap.DataPresence != tc.wantData {
				t.Errorf("data presence = %q, want %q", snap.DataPresence, tc.wantData)
			}
			if snap.ChannelPresence != tc.wantChannel {
				t.Errorf("channel presence = %q, want %q", snap.ChannelPresence, tc.wantChannel)
			}
			if snap.SelfMilestonePresence != MilestoneFieldMissing {
				t.Errorf("S presence = %q, want MISSING", snap.SelfMilestonePresence)
			}
			assertNoFabricatedValues(t, snap)
		})
	}
}

// TestObserveWatchStreakMilestoneSendsAuditedRequest proves the observation
// sends exactly one RewardList request carrying the recorded persisted-query
// evidence and both audited variables, and parses the result.
func TestObserveWatchStreakMilestoneSendsAuditedRequest(t *testing.T) {
	var (
		mu       sync.Mutex
		requests int
		bodies   []string
	)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests++
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{`+
			`"state":"ACTIVE","watchStreakThreshold":3,"watchStreakCopoBonus":450,"missedStreams":[],`+
			`"watchStreakMilestone":{"id":"m-1","value":4,"achievementTimestamp":"2026-09-05T12:00:00Z","shareStatus":"UNSHARED"}}}}}}`)
	})

	obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

	if obs.Outcome != MilestoneObserved {
		t.Fatalf("outcome = %q, want OBSERVED (failure %q)", obs.Outcome, obs.FailureClass)
	}
	if requests != 1 {
		t.Fatalf("RewardList requests = %d, want exactly 1", requests)
	}
	body := bodies[0]
	for _, want := range []string{
		`"operationName":"RewardList"`,
		constants.RewardList.Extensions.PersistedQuery.SHA256Hash,
		`"channelID":"12345"`,
		`"shouldIncludeAllSuspendedStreaks":false`,
		`"version":1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("request body missing %q; body: %s", want, body)
		}
	}
	if obs.Snapshot.MilestoneValue.Value != 4 || obs.Snapshot.WatchStreakCopoBonus.Value != 450 {
		t.Errorf("parsed snapshot = %+v", obs.Snapshot)
	}
	if obs.Snapshot.MissedStreams.Presence != MilestoneFieldEmpty {
		t.Errorf("empty missedStreams presence = %q, want EMPTY", obs.Snapshot.MissedStreams.Presence)
	}
	if obs.RequestStart.IsZero() || obs.RequestEnd.Before(obs.RequestStart) {
		t.Errorf("request window is not ordered: %v -> %v", obs.RequestStart, obs.RequestEnd)
	}
	// achievementTimestamp is the ACHIEVEMENT's field, never the sampling time.
	achieved, err := time.Parse(time.RFC3339, obs.Snapshot.AchievementTimestamp.Value)
	if err != nil {
		t.Fatalf("achievementTimestamp not parseable: %v", err)
	}
	if !achieved.Before(obs.RequestStart) {
		t.Errorf("fixture achievement time %v should predate the sample window %v", achieved, obs.RequestStart)
	}
}

// TestObserveWatchStreakMilestoneTopLevelGraphQLError proves a top-level errors
// array yields GRAPHQL_ERROR with nothing parsed — a service error is never
// presented as an observed milestone.
func TestObserveWatchStreakMilestoneTopLevelGraphQLError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Data is present alongside the errors array on purpose: it still must
		// not be parsed.
		_, _ = io.WriteString(w, `{"errors":[{"message":"service error"}],`+
			`"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{"watchStreakMilestone":{"value":450}}}}}}`)
	})

	obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

	if obs.Outcome != MilestoneGraphQLError || obs.FailureClass != MilestoneFailureGraphQLTopLevel {
		t.Fatalf("outcome = %q/%q, want GRAPHQL_ERROR/GRAPHQL_TOP_LEVEL_ERRORS", obs.Outcome, obs.FailureClass)
	}
	// Nothing is parsed at all: the data node alongside the errors array must
	// not reach the snapshot, not even as a presence classification.
	assertEmptySnapshot(t, obs.Snapshot)
}

// TestObserveWatchStreakMilestoneUnsupportedQuery proves a stale persisted-query
// hash is reported as UNSUPPORTED_QUERY evidence and nothing else. This is the
// outcome that would falsify the RewardList protocol premise.
func TestObserveWatchStreakMilestoneUnsupportedQuery(t *testing.T) {
	var (
		mu       sync.Mutex
		attempts int
	)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, persistedQueryNotFoundBody)
	})

	obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

	if obs.Outcome != MilestoneUnsupported || obs.FailureClass != MilestoneFailureQueryNotFound {
		t.Fatalf("outcome = %q/%q, want UNSUPPORTED_QUERY/PERSISTED_QUERY_NOT_FOUND", obs.Outcome, obs.FailureClass)
	}
	assertEmptySnapshot(t, obs.Snapshot)

	// Amplification bound: the worst case is the SHARED read transport's
	// existing client-ID fallback (one HTTP attempt per candidate ID, since an
	// APQ-not-found answer is HTTP 200 and is never retried) and nothing this
	// observation adds on top.
	mu.Lock()
	got := attempts
	mu.Unlock()
	want := len(c.candidateClientIDs(constants.RewardList.OperationName))
	if got != want {
		t.Fatalf("HTTP attempts = %d, want exactly %d (one per candidate client ID); "+
			"the observation must add no retry of its own", got, want)
	}
}

// TestObserveWatchStreakMilestoneSingleRequestOnSuccess pins the steady-state
// amplification: one observation is exactly one HTTP round trip.
func TestObserveWatchStreakMilestoneSingleRequestOnSuccess(t *testing.T) {
	var (
		mu       sync.Mutex
		attempts int
	)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{`+
			`"watchStreakMilestone":{"value":4}}}}}}`)
	})

	if obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer"); obs.Outcome != MilestoneObserved {
		t.Fatalf("outcome = %q, want OBSERVED", obs.Outcome)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Fatalf("HTTP attempts = %d, want exactly 1", attempts)
	}
}

// TestObserveWatchStreakMilestoneRequestFailure proves a rejection is
// UNAVAILABLE evidence carrying only a bounded class — never the raw error
// text — and that the class names the STATUS when the status is what settled
// it.
//
// The 403 case reported TRANSPORT until the status was made authoritative.
// That was not false so much as vague: nothing about a 403 is a transport
// fault, and answering "transport" for an HTTP refusal is the same imprecision
// that let a 4xx whose body failed to parse arrive under a transport class.
// TRANSPORT now means what its name says, and this test pins which of the two
// a permission rejection is.
//
// Transient statuses are pinned the other way in the sibling case below: a 429
// or a 5xx here means the retry schedule ran and gave up, which is the more
// useful fact than the last status seen, so those keep TRANSPORT.
func TestObserveWatchStreakMilestoneRequestFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		wantClass MilestoneFailureClass
	}{
		{
			// A permission rejection: returned immediately, no retries.
			name:      "a permission rejection names the status",
			status:    http.StatusForbidden,
			wantClass: MilestoneFailureHTTPStatus,
		},
		{
			// Exhausted retries: the schedule ran, so the fact is the
			// transport, not the last status.
			name:      "an exhausted transient status stays TRANSPORT",
			status:    http.StatusBadGateway,
			wantClass: MilestoneFailureTransport,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"message":"rejected"}`)
			})

			obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

			if obs.Outcome != MilestoneUnavailable || obs.FailureClass != tc.wantClass {
				t.Fatalf("outcome = %q/%q, want UNAVAILABLE/%s", obs.Outcome, obs.FailureClass, tc.wantClass)
			}
			assertEmptySnapshot(t, obs.Snapshot)
		})
	}
}

// TestObserveWatchStreakMilestoneNoChannelIDSkips proves an unscopeable target
// produces a SKIPPED record and sends nothing.
func TestObserveWatchStreakMilestoneNoChannelIDSkips(t *testing.T) {
	var requests int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":{}}`)
	})

	obs := c.ObserveWatchStreakMilestone(context.Background(), "", "somestreamer")

	if obs.Outcome != MilestoneSkipped || obs.FailureClass != MilestoneFailureNoChannelID {
		t.Fatalf("outcome = %q/%q, want SKIPPED/NO_CHANNEL_ID", obs.Outcome, obs.FailureClass)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

// TestObserveWatchStreakMilestoneAlreadyCancelledSendsNothing proves a cancelled
// owner produces a SKIPPED record with no request at all.
func TestObserveWatchStreakMilestoneAlreadyCancelledSendsNothing(t *testing.T) {
	var requests int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":{}}`)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	obs := c.ObserveWatchStreakMilestone(ctx, "12345", "somestreamer")

	if obs.Outcome != MilestoneSkipped || obs.FailureClass != MilestoneFailureContextDone {
		t.Fatalf("outcome = %q/%q, want SKIPPED/CONTEXT_ALREADY_DONE", obs.Outcome, obs.FailureClass)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

// TestObserveWatchStreakMilestoneCancellationReleasesInFlightRequest proves
// cancelling the owner mid-request releases it deterministically — the handler
// is still blocked when the observation returns, so nothing here waits on a
// sleep or a timeout.
func TestObserveWatchStreakMilestoneCancellationReleasesInFlightRequest(t *testing.T) {
	var (
		once     sync.Once
		entered  = make(chan struct{})
		release  = make(chan struct{})
		requests int
		mu       sync.Mutex
	)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		once.Do(func() { close(entered) })
		<-release
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-entered
		cancel()
	}()

	obs := c.ObserveWatchStreakMilestone(ctx, "12345", "somestreamer")
	close(release) // let the (still-blocked) handler finish so the server can close

	if obs.Outcome != MilestoneCancelled || obs.FailureClass != MilestoneFailureCancelled {
		t.Fatalf("outcome = %q/%q, want CANCELLED/CANCELLED", obs.Outcome, obs.FailureClass)
	}
	mu.Lock()
	got := requests
	mu.Unlock()
	if got != 1 {
		t.Fatalf("requests = %d, want exactly 1 (cancellation must not retry)", got)
	}
	assertEmptySnapshot(t, obs.Snapshot)
}

// TestObserveWatchStreakMilestoneOrderingIsByRequestStart is the reverse-
// completion falsifier: an observation that STARTED first but FINISHED last
// must never be presentable as the newer evidence.
//
// Deterministic by construction — the older request is held open by a barrier
// until the newer one has completed. No sleeps.
func TestObserveWatchStreakMilestoneOrderingIsByRequestStart(t *testing.T) {
	var (
		once    sync.Once
		entered = make(chan struct{})
		release = make(chan struct{})
		mu      sync.Mutex
		seen    int
	)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen++
		first := seen == 1
		mu.Unlock()
		if first {
			once.Do(func() { close(entered) })
			<-release // the older request is held open
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{`+
			`"watchStreakMilestone":{"value":4}}}}}}`)
	})

	var older WatchStreakMilestoneObservation
	done := make(chan struct{})
	go func() {
		defer close(done)
		older = c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")
	}()

	<-entered // the older request is in flight and pinned
	newer := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")
	close(release)
	<-done

	if older.Sequence >= newer.Sequence {
		t.Fatalf("sequences = %d then %d; the earlier-started request must hold the lower sequence",
			older.Sequence, newer.Sequence)
	}
	if !older.RequestEnd.After(newer.RequestEnd) {
		t.Fatalf("fixture did not reverse completion: older ended %v, newer ended %v",
			older.RequestEnd, newer.RequestEnd)
	}
	// Ordering must follow REQUEST START, not completion. The sequence is the
	// only recency authority the record carries, and it is stamped before the
	// request is sent — so the late-completing older observation still sorts
	// older, and nothing in the record claims otherwise.
	if !older.RequestStart.Before(newer.RequestStart) && !older.RequestStart.Equal(newer.RequestStart) {
		t.Fatalf("older request started at %v, after the newer one at %v", older.RequestStart, newer.RequestStart)
	}
	byCompletion := []WatchStreakMilestoneObservation{newer, older} // completion order
	if byCompletion[0].Sequence < byCompletion[1].Sequence {
		t.Fatal("sorting by completion order reproduces the start order; the fixture no longer reverses completion")
	}
}

// TestObserveWatchStreakMilestoneRepeatedIdenticalValuesStayDistinct proves
// temporal evidence is never deduped away: two samples with identical milestone
// content are two distinct observations.
func TestObserveWatchStreakMilestoneRepeatedIdenticalValuesStayDistinct(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{`+
			`"watchStreakMilestone":{"id":"m-1","value":4,"achievementTimestamp":"2026-09-05T12:00:00Z"}}}}}}`)
	})

	first := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")
	second := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

	if !reflect.DeepEqual(first.Snapshot, second.Snapshot) {
		t.Fatalf("fixture did not produce identical content:\n%+v\n%+v", first.Snapshot, second.Snapshot)
	}
	if first.Sequence == second.Sequence {
		t.Fatal("identical observations collapsed onto one sequence")
	}
	if second.Sequence < first.Sequence {
		t.Fatal("the second identical observation is not ordered after the first")
	}
	// Identical content, identical achievement time, DIFFERENT sample windows.
	// The achievement time is a property of the achievement and does not move
	// with the sample; the sample window does.
	if first.Snapshot.AchievementTimestamp.Value != second.Snapshot.AchievementTimestamp.Value {
		t.Fatal("fixture achievement timestamps differ")
	}
	if second.RequestStart.Before(first.RequestStart) {
		t.Fatalf("second sample started before the first: %v < %v", second.RequestStart, first.RequestStart)
	}
	if second.RequestEnd.Before(first.RequestEnd) {
		t.Fatalf("second sample ended before the first: %v < %v", second.RequestEnd, first.RequestEnd)
	}
}

// TestObserveWatchStreakMilestoneDoesNotTouchConnectionHealth is the BLOCKER
// regression.
//
// The shared connectivity accounting feeds internal/miner.classifyAPI, which
// turns RecentFunctionalFailures >= 2 into health.SignalGQLAPI = DEGRADED,
// which minerBetHealthGate turns into "no automated prediction bets". A
// RewardList hash Twitch does not accept is the EXPECTED outcome here until
// live acceptance is separately evidenced, so without diagnostic isolation two
// online streamers would permanently close the auto-bet gate.
//
// The suppression must be SYMMETRIC: a failing observation may not invent an
// outage, and a succeeding one may not mask one.
func TestObserveWatchStreakMilestoneDoesNotTouchConnectionHealth(t *testing.T) {
	const window = time.Hour

	tests := []struct {
		name string
		body string
		code int
		want WatchStreakMilestoneOutcome
	}{
		{"unsupported hash", persistedQueryNotFoundBody, http.StatusOK, MilestoneUnsupported},
		{"top-level graphql errors", `{"errors":[{"message":"service error"}]}`, http.StatusOK, MilestoneGraphQLError},
		{"permission denied", `{"message":"forbidden"}`, http.StatusForbidden, MilestoneUnavailable},
		{"unauthorized", `{"message":"unauthorized"}`, http.StatusUnauthorized, MilestoneUnavailable},
		{
			"success",
			`{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{"watchStreakMilestone":{"value":4}}}}}}`,
			http.StatusOK, MilestoneObserved,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			})

			before := c.ConnHealth(time.Now(), window)

			// Several cycles' worth of targets: more than enough to cross the
			// miner's degrade threshold if any of this were being counted.
			for i := 0; i < 6; i++ {
				if got := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer"); got.Outcome != tc.want {
					t.Fatalf("observation %d outcome = %q, want %q (failure %q)", i, got.Outcome, tc.want, got.FailureClass)
				}
			}

			after := c.ConnHealth(time.Now(), window)

			if after.RecentFunctionalFailures != before.RecentFunctionalFailures {
				t.Errorf("functional failures %d -> %d: a diagnostic observation must not degrade GQL health "+
					"(this is what closes the auto-bet gate)",
					before.RecentFunctionalFailures, after.RecentFunctionalFailures)
			}
			if after.RecentTransportFailures != before.RecentTransportFailures {
				t.Errorf("transport failures %d -> %d: a diagnostic observation must not degrade GQL health",
					before.RecentTransportFailures, after.RecentTransportFailures)
			}
			if !after.LastAttempt.Equal(before.LastAttempt) {
				t.Errorf("last attempt %v -> %v: a diagnostic observation must not claim the link was exercised",
					before.LastAttempt, after.LastAttempt)
			}
			if !after.LastSuccess.Equal(before.LastSuccess) {
				t.Errorf("last success %v -> %v: a diagnostic observation must not mask a real business-path outage",
					before.LastSuccess, after.LastSuccess)
			}
		})
	}
}

// TestBusinessReadStillAccountsConnectionHealth is the other half of the
// regression: the isolation must apply ONLY to the diagnostic read. A business
// operation over the same client must still record exactly what it always did.
func TestBusinessReadStillAccountsConnectionHealth(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, persistedQueryNotFoundBody)
	})

	// A diagnostic observation first: it must leave the accounting untouched.
	c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")
	if h := c.ConnHealth(time.Now(), time.Hour); h.RecentFunctionalFailures != 0 || !h.LastAttempt.IsZero() {
		t.Fatalf("diagnostic observation polluted the accounting: %+v", h)
	}

	// The same failure on a BUSINESS read is still real evidence.
	s := newTestStreamer("somestreamer")
	if err := c.LoadChannelPointsContext(s); err == nil {
		t.Fatal("expected the business read to fail on a stale hash")
	}
	h := c.ConnHealth(time.Now(), time.Hour)
	if h.RecentFunctionalFailures == 0 {
		t.Error("business read no longer records a functional failure; the isolation leaked past the diagnostic path")
	}
	if h.LastAttempt.IsZero() {
		t.Error("business read no longer records an attempt; the isolation leaked past the diagnostic path")
	}
}

// TestObserveWatchStreakMilestoneUnauthorizedDoesNotEscalate proves a
// diagnostic 401 reports UNAUTHORIZED evidence and stops: it drives no
// credential recovery and no operator reauth escalation. Business operations
// own auth recovery; an observation must not be what declares the session dead.
//
// The recovery seam is stubbed to SUCCEED so the assertion discriminates. On
// the business path a successful recovery replays the identical body, which
// would show up as a second HTTP attempt; on the diagnostic path recovery must
// never be reached at all.
func TestObserveWatchStreakMilestoneUnauthorizedDoesNotEscalate(t *testing.T) {
	var (
		mu       sync.Mutex
		attempts int
	)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"unauthorized"}`)
	})

	recoveries := 0
	c.recoverFn = func(uint64) (auth.Snapshot, error) {
		recoveries++
		return auth.Snapshot{AccessToken: "recovered-token", Generation: 99}, nil
	}
	escalated := false
	c.SetAuthErrorHandler(func() { escalated = true })

	obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

	if obs.Outcome != MilestoneUnavailable || obs.FailureClass != MilestoneFailureUnauthorized {
		t.Fatalf("outcome = %q/%q, want UNAVAILABLE/UNAUTHORIZED", obs.Outcome, obs.FailureClass)
	}
	if recoveries != 0 {
		t.Errorf("credential recovery ran %d time(s) for a diagnostic observation; it must own none", recoveries)
	}
	if escalated {
		t.Error("a diagnostic observation escalated to the operator reauth path")
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Errorf("HTTP attempts = %d, want 1 (a diagnostic read owns no recovery replay)", attempts)
	}
}

// TestBusinessReadStillRecoversOnUnauthorized is the other half: the
// no-recovery rule applies ONLY to the diagnostic read. The identical 401 on a
// business operation must still drive the single-flight recovery and its one
// replay, exactly as before.
func TestBusinessReadStillRecoversOnUnauthorized(t *testing.T) {
	var (
		mu       sync.Mutex
		attempts int
	)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"unauthorized"}`)
	})

	recoveries := 0
	c.recoverFn = func(uint64) (auth.Snapshot, error) {
		recoveries++
		return auth.Snapshot{AccessToken: "recovered-token", Generation: 99}, nil
	}

	// The error is asserted rather than discarded: this fixture answers 401 to
	// every attempt, so a nil error here would mean the business read reported
	// success after two rejections, and the recovery/attempt counts below would
	// not notice.
	if err := c.LoadChannelPointsContext(newTestStreamer("somestreamer")); err == nil {
		t.Error("business read returned nil error although every attempt was rejected 401")
	}

	if recoveries != 1 {
		t.Errorf("business read ran recovery %d time(s), want 1; the diagnostic rule leaked", recoveries)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Errorf("business read HTTP attempts = %d, want 2 (original + one replay)", attempts)
	}
}

// TestObserveWatchStreakMilestoneCancelledThroughADerivedTimeoutContext proves
// cancellation is honoured through a derived (timeout-carrying) context, not
// only through a bare cancel.
//
// It does NOT produce a real deadline expiry — the parent is cancelled while
// the request is in flight, so the error is context.Canceled. Forcing a genuine
// in-flight deadline here would mean racing a wall-clock timer.
//
// The deadline classifications are proven deterministically elsewhere, and the
// distinction matters, so both halves are named:
//   - a CALLER-OWNED deadline (owner context already expired) classifies as
//     CANCELLED/DEADLINE_EXCEEDED — TestClassifyMilestoneRequestErrorSeparatesShutdownFromStall,
//     case "owner deadline expired", and end to end in
//     TestObserveWatchStreakMilestoneExpiredDeadlineIsSkippedNotSent;
//   - a TRANSPORT timeout with a still-live caller context (net/http's
//     Client.Timeout, whose error also satisfies errors.Is(err,
//     context.DeadlineExceeded)) classifies as UNAVAILABLE/TRANSPORT_TIMEOUT —
//     same test, cases "http client timeout with a live owner" and the wrapped
//     net/http shape.
//
// TestClassifyMilestoneRequestErrorVocabularyIsClosed does NOT prove either: it
// passes a live context.Background() and only checks that the returned class is
// inside the closed vocabulary.
func TestObserveWatchStreakMilestoneCancelledThroughADerivedTimeoutContext(t *testing.T) {
	var (
		once    sync.Once
		entered = make(chan struct{})
		release = make(chan struct{})
	)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-entered
		cancel()
	}()
	// A cancelled parent and an exceeded deadline share the classification
	// branch; drive the deadline form explicitly.
	deadlined, stop := context.WithTimeout(ctx, time.Hour)
	defer stop()

	obs := c.ObserveWatchStreakMilestone(deadlined, "12345", "somestreamer")
	close(release)

	if obs.Outcome != MilestoneCancelled {
		t.Fatalf("outcome = %q, want CANCELLED", obs.Outcome)
	}
	// Precisely what this fixture produces: a cancelled parent, so CANCELLED.
	// Accepting either class here would have made the assertion vacuous.
	if obs.FailureClass != MilestoneFailureCancelled {
		t.Fatalf("failureClass = %q, want CANCELLED", obs.FailureClass)
	}
}

// TestObserveWatchStreakMilestoneExpiredDeadlineIsSkippedNotSent proves an
// already-expired deadline is honoured before the request is built, and is
// classified as a context-done skip rather than a transport failure.
func TestObserveWatchStreakMilestoneExpiredDeadlineIsSkippedNotSent(t *testing.T) {
	var requests int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":{}}`)
	})

	// An already-past deadline: ctx.Err() is DeadlineExceeded immediately, with
	// no timer to race.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("fixture context error = %v, want DeadlineExceeded", ctx.Err())
	}

	obs := c.ObserveWatchStreakMilestone(ctx, "12345", "somestreamer")

	if obs.Outcome != MilestoneSkipped || obs.FailureClass != MilestoneFailureContextDone {
		t.Fatalf("outcome = %q/%q, want SKIPPED/CONTEXT_ALREADY_DONE", obs.Outcome, obs.FailureClass)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

// TestClassifyMilestoneRequestErrorVocabularyIsClosed proves every failure
// reaches a record as a bounded class, never as raw error text.
func TestClassifyMilestoneRequestErrorVocabularyIsClosed(t *testing.T) {
	allowed := map[MilestoneFailureClass]bool{
		MilestoneFailureCancelled:        true,
		MilestoneFailureDeadline:         true,
		MilestoneFailureQueryNotFound:    true,
		MilestoneFailureUnauthorized:     true,
		MilestoneFailureTransport:        true,
		MilestoneFailureTransportTimeout: true,
	}
	secret := "OAuth super-secret-token Authorization: Bearer abc"
	tests := []error{
		context.Canceled,
		context.DeadlineExceeded,
		fmt.Errorf("wrapped: %w", ErrPersistedQueryNotFound),
		fmt.Errorf("wrapped: %w", ErrUnauthorized),
		errors.New(secret),
		fmt.Errorf("request failed: %s", secret),
	}
	// A LIVE owner: nothing here may be reported as a cancellation.
	for _, err := range tests {
		outcome, class := classifyMilestoneRequestError(context.Background(), err)
		if !allowed[class] {
			t.Errorf("error %q produced class %q, outside the closed vocabulary", err, class)
		}
		if strings.Contains(string(class), "OAuth") || strings.Contains(string(class), secret) {
			t.Errorf("error text leaked into the failure class: %q", class)
		}
		if outcome == MilestoneObserved {
			t.Errorf("error %q classified as OBSERVED", err)
		}
	}
}

// TestClassifyMilestoneRequestErrorSeparatesShutdownFromStall pins the
// distinction the error alone cannot make.
//
// net/http's Client.Timeout produces an error satisfying
// errors.Is(err, context.DeadlineExceeded) even when the owner's context is
// perfectly healthy. Classifying on the error alone would report a stalled
// Twitch as CANCELLED — whose own definition is "the owning context was
// cancelled" — turning a remote fault into an apparent local shutdown.
func TestClassifyMilestoneRequestErrorSeparatesShutdownFromStall(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer stop()

	tests := []struct {
		name        string
		ctx         context.Context
		err         error
		wantOutcome WatchStreakMilestoneOutcome
		wantClass   MilestoneFailureClass
	}{
		{
			"owner cancelled", cancelled, context.Canceled,
			MilestoneCancelled, MilestoneFailureCancelled,
		},
		{
			"owner deadline expired", expired, context.DeadlineExceeded,
			MilestoneCancelled, MilestoneFailureDeadline,
		},
		{
			// The load-bearing case: a live owner and an http.Client timeout.
			"http client timeout with a live owner", context.Background(), context.DeadlineExceeded,
			MilestoneUnavailable, MilestoneFailureTransportTimeout,
		},
		{
			"inner cancellation with a live owner", context.Background(), context.Canceled,
			MilestoneUnavailable, MilestoneFailureTransport,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcome, class := classifyMilestoneRequestError(tc.ctx, tc.err)
			if outcome != tc.wantOutcome || class != tc.wantClass {
				t.Fatalf("classified as %q/%q, want %q/%q", outcome, class, tc.wantOutcome, tc.wantClass)
			}
		})
	}

	// And the real wrapped shape net/http actually produces.
	wrapped := fmt.Errorf(`request failed: Get "http://x": %w (Client.Timeout exceeded while awaiting headers)`,
		context.DeadlineExceeded)
	if outcome, class := classifyMilestoneRequestError(context.Background(), wrapped); outcome != MilestoneUnavailable ||
		class != MilestoneFailureTransportTimeout {
		t.Fatalf("a wrapped client timeout classified as %q/%q, want UNAVAILABLE/TRANSPORT_TIMEOUT", outcome, class)
	}
}

// TestParseWatchStreakMilestoneSelfNodeAbsence covers data.channel.self, whose
// MISSING / NULL / MALFORMED forms were otherwise never observed.
func TestParseWatchStreakMilestoneSelfNodeAbsence(t *testing.T) {
	channelOf := func(t *testing.T, r map[string]interface{}) map[string]interface{} {
		t.Helper()
		return r["data"].(map[string]interface{})["channel"].(map[string]interface{})
	}
	tests := []struct {
		name   string
		mutate func(t *testing.T, r map[string]interface{})
		want   MilestoneFieldPresence
	}{
		{"missing", func(t *testing.T, r map[string]interface{}) { delete(channelOf(t, r), "self") }, MilestoneFieldMissing},
		{"null", func(t *testing.T, r map[string]interface{}) { channelOf(t, r)["self"] = nil }, MilestoneFieldNull},
		{"malformed", func(t *testing.T, r map[string]interface{}) { channelOf(t, r)["self"] = "nope" }, MilestoneFieldMalformed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := fullRewardListResponse()
			tc.mutate(t, resp)
			snap := parseWatchStreakMilestone(resp)
			if snap.SelfPresence != tc.want {
				t.Errorf("self presence = %q, want %q", snap.SelfPresence, tc.want)
			}
			// The channel id above self is still observed; everything below it
			// is absent and carries no fabricated value.
			if snap.ChannelID.Presence != MilestoneFieldValid || snap.ChannelID.Value != "12345" {
				t.Errorf("channel id = %+v, want the observed id", snap.ChannelID)
			}
			if snap.SelfMilestonePresence != MilestoneFieldMissing {
				t.Errorf("S presence = %q, want MISSING", snap.SelfMilestonePresence)
			}
			assertNoFabricatedValues(t, snap)
		})
	}
}

// TestObserveWatchStreakMilestoneNonGraphQLBodyIsUnavailable proves an edge or
// proxy rejection body is reported as UNAVAILABLE, not as an observation in
// which every field happened to be missing.
//
// The shared transport special-cases only 401 and 403; any other non-2xx body
// that parses as JSON is returned verbatim as a result. Without the data-node
// check, a 404 or a gateway error page would be recorded as OBSERVED with every
// node MISSING — indistinguishable in the retained log from Twitch genuinely
// answering "this channel has no milestone", which is the exact confusion this
// feature exists to prevent.
func TestObserveWatchStreakMilestoneNonGraphQLBodyIsUnavailable(t *testing.T) {
	// The failure class differs per case on purpose. A non-2xx is refused on its
	// STATUS before the body is looked at, which is the sharper fact; the 200
	// cases are refused because the body is not a GraphQL data response.
	tests := []struct {
		name      string
		code      int
		body      string
		wantClass MilestoneFailureClass
	}{
		{"4xx rejection with a JSON body", http.StatusNotFound,
			`{"error":"Not Found","status":404}`, MilestoneFailureHTTPStatus},
		{"gateway body with no data node", http.StatusOK,
			`{"message":"upstream unavailable"}`, MilestoneFailureNoDataNode},
		{"explicit null data with no errors array", http.StatusOK,
			`{"data":null}`, MilestoneFailureNoDataNode},
		{"data of the wrong shape", http.StatusOK,
			`{"data":[]}`, MilestoneFailureNoDataNode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			})

			obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

			if obs.Outcome != MilestoneUnavailable {
				t.Fatalf("outcome = %q, want UNAVAILABLE — a rejected request must not read as an "+
					"observation with everything missing", obs.Outcome)
			}
			if obs.FailureClass != tc.wantClass {
				t.Errorf("failureClass = %q, want %q", obs.FailureClass, tc.wantClass)
			}
			assertEmptySnapshot(t, obs.Snapshot)
		})
	}

	// The counterpart: a real GraphQL response whose data node IS present stays
	// OBSERVED even when everything under it is absent — that is a genuine
	// observation of absence and must not be downgraded.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":{"channel":null}}`)
	})
	obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")
	if obs.Outcome != MilestoneObserved {
		t.Fatalf("outcome = %q, want OBSERVED for a real GraphQL response", obs.Outcome)
	}
	if obs.Snapshot.ChannelPresence != MilestoneFieldNull {
		t.Errorf("channel presence = %q, want NULL", obs.Snapshot.ChannelPresence)
	}
}

// TestObserveWatchStreakMilestoneTransientFailureLeavesTransportHealthAlone
// closes the second half of the connection-health isolation.
//
// The earlier isolation test asserts RecentTransportFailures is unchanged, but
// none of its fixtures uses a transient status, so it compares 0 to 0. Only an
// exhausted retry ladder reaches gqlFailures.mark, and that counter drives
// classifyAPI's transportFailing/degradedEvidence branches exactly as the
// functional counter does — so an unguarded diagnostic retry storm would close
// the auto-bet gate through the other door.
//
// This exercises the real ladder, so it pays the real backoff.
func TestObserveWatchStreakMilestoneTransientFailureLeavesTransportHealthAlone(t *testing.T) {
	var (
		mu       sync.Mutex
		attempts int
	)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		// Transient: retried, and the ladder is exhausted.
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	before := c.ConnHealth(time.Now(), time.Hour)

	obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")
	if obs.Outcome != MilestoneUnavailable || obs.FailureClass != MilestoneFailureTransport {
		t.Fatalf("outcome = %q/%q, want UNAVAILABLE/TRANSPORT", obs.Outcome, obs.FailureClass)
	}

	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != gqlMaxRetries+1 {
		t.Fatalf("attempts = %d, want %d — the fixture did not exhaust the retry ladder, so the "+
			"assertion below would be vacuous", got, gqlMaxRetries+1)
	}

	after := c.ConnHealth(time.Now(), time.Hour)
	if after.RecentTransportFailures != before.RecentTransportFailures {
		t.Errorf("transport failures %d -> %d: an exhausted DIAGNOSTIC retry ladder must not degrade "+
			"GQL health — that closes the auto-bet gate through classifyAPI's transport branch",
			before.RecentTransportFailures, after.RecentTransportFailures)
	}
	if !after.LastSuccess.Equal(before.LastSuccess) || !after.LastAttempt.Equal(before.LastAttempt) {
		t.Errorf("accounting moved: attempt %v -> %v, success %v -> %v",
			before.LastAttempt, after.LastAttempt, before.LastSuccess, after.LastSuccess)
	}
}

// TestBusinessTransientFailureStillRecordsTransportHealth is its counterpart:
// the isolation must not leak onto a business read.
func TestBusinessTransientFailureStillRecordsTransportHealth(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	if err := c.LoadChannelPointsContext(newTestStreamer("somestreamer")); err == nil {
		t.Fatal("expected the business read to fail")
	}
	if h := c.ConnHealth(time.Now(), time.Hour); h.RecentTransportFailures == 0 {
		t.Error("business read no longer records a transport failure; the diagnostic isolation leaked")
	}
}

// missedStreamsFrom decodes a missedStreams JSON array through the REAL JSON
// decoder and returns the parsed container, so fixtures are wire text rather
// than hand-built Go values.
func missedStreamsFrom(t *testing.T, arrayJSON string) MilestoneMissedStreams {
	t.Helper()
	var resp map[string]interface{}
	body := `{"data":{"channel":{"id":"1","self":{"watchStreakMilestone":{"missedStreams":` + arrayJSON + `}}}}}`
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	return parseWatchStreakMilestone(resp).MissedStreams
}

// elementPresences / childPresences project the per-node classifications in
// wire order so a test can assert each node separately.
func elementPresences(m MilestoneMissedStreams) []MilestoneFieldPresence {
	out := make([]MilestoneFieldPresence, 0, len(m.Entries))
	for _, e := range m.Entries {
		out = append(out, e.Presence)
	}
	return out
}

func childPresences(m MilestoneMissedStreams) []MilestoneFieldPresence {
	out := make([]MilestoneFieldPresence, 0, len(m.Entries))
	for _, e := range m.Entries {
		out = append(out, e.BroadcastIdentifiers.Presence)
	}
	return out
}

// TestMissedStreamsElementAndChildPresenceAreIndependentNodes is the R1/R3
// parser-level proof.
//
// Every expectation below is specified from the WIRE SHAPE alone — what Twitch
// sent — not read off the implementation. An element and its broadcastIdentifiers
// child are two different nodes and each carries its own classification; where
// no element object exists there is no child to classify, so the child stays
// unset ("") rather than borrowing the element's answer.
func TestMissedStreamsElementAndChildPresenceAreIndependentNodes(t *testing.T) {
	const unset = MilestoneFieldPresence("")

	tests := []struct {
		name          string
		array         string
		wantElements  []MilestoneFieldPresence
		wantChildren  []MilestoneFieldPresence
		wantContainer MilestoneFieldPresence
		wantCount     int
		wantMalformed int
	}{
		{
			name:          "null element: the ELEMENT is null and has no child to classify",
			array:         `[null]`,
			wantElements:  []MilestoneFieldPresence{MilestoneFieldNull},
			wantChildren:  []MilestoneFieldPresence{unset},
			wantContainer: MilestoneFieldValid, wantCount: 1, wantMalformed: 0,
		},
		{
			name:          "null child: the ELEMENT is valid and its child array is null",
			array:         `[{"broadcastIdentifiers":null}]`,
			wantElements:  []MilestoneFieldPresence{MilestoneFieldValid},
			wantChildren:  []MilestoneFieldPresence{MilestoneFieldNull},
			wantContainer: MilestoneFieldValid, wantCount: 1, wantMalformed: 0,
		},
		{
			name:          "absent child: the ELEMENT is valid and its child key is missing",
			array:         `[{}]`,
			wantElements:  []MilestoneFieldPresence{MilestoneFieldValid},
			wantChildren:  []MilestoneFieldPresence{MilestoneFieldMissing},
			wantContainer: MilestoneFieldValid, wantCount: 1, wantMalformed: 0,
		},
		{
			name:          "empty child: Twitch said there are none",
			array:         `[{"broadcastIdentifiers":[]}]`,
			wantElements:  []MilestoneFieldPresence{MilestoneFieldValid},
			wantChildren:  []MilestoneFieldPresence{MilestoneFieldEmpty},
			wantContainer: MilestoneFieldValid, wantCount: 1, wantMalformed: 0,
		},
		{
			name:          "string element is the wrong shape",
			array:         `["nope"]`,
			wantElements:  []MilestoneFieldPresence{MilestoneFieldMalformed},
			wantChildren:  []MilestoneFieldPresence{unset},
			wantContainer: MilestoneFieldMalformed, wantCount: 1, wantMalformed: 1,
		},
		{
			name:          "number element is the wrong shape",
			array:         `[7]`,
			wantElements:  []MilestoneFieldPresence{MilestoneFieldMalformed},
			wantChildren:  []MilestoneFieldPresence{unset},
			wantContainer: MilestoneFieldMalformed, wantCount: 1, wantMalformed: 1,
		},
		{
			name:          "array element is the wrong shape",
			array:         `[[]]`,
			wantElements:  []MilestoneFieldPresence{MilestoneFieldMalformed},
			wantChildren:  []MilestoneFieldPresence{unset},
			wantContainer: MilestoneFieldMalformed, wantCount: 1, wantMalformed: 1,
		},
		{
			name:          "valid entry from the existing fixture shape",
			array:         `[{"broadcastIdentifiers":[{"id":"b-1"},{"id":"b-2"}]}]`,
			wantElements:  []MilestoneFieldPresence{MilestoneFieldValid},
			wantChildren:  []MilestoneFieldPresence{MilestoneFieldValid},
			wantContainer: MilestoneFieldValid, wantCount: 1, wantMalformed: 0,
		},
		{
			name:  "mixed list: every element keeps its own answer, in position",
			array: `[null,{"broadcastIdentifiers":null},{"broadcastIdentifiers":[{"id":"x"}]},"nope",{}]`,
			wantElements: []MilestoneFieldPresence{
				MilestoneFieldNull, MilestoneFieldValid, MilestoneFieldValid,
				MilestoneFieldMalformed, MilestoneFieldValid,
			},
			wantChildren: []MilestoneFieldPresence{
				unset, MilestoneFieldNull, MilestoneFieldValid, unset, MilestoneFieldMissing,
			},
			wantContainer: MilestoneFieldMalformed, wantCount: 5, wantMalformed: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := missedStreamsFrom(t, tc.array)
			if !reflect.DeepEqual(elementPresences(got), tc.wantElements) {
				t.Errorf("element presences = %v, want %v", elementPresences(got), tc.wantElements)
			}
			if !reflect.DeepEqual(childPresences(got), tc.wantChildren) {
				t.Errorf("child presences = %v, want %v", childPresences(got), tc.wantChildren)
			}
			if got.Presence != tc.wantContainer {
				t.Errorf("container presence = %q, want %q", got.Presence, tc.wantContainer)
			}
			if got.Count != tc.wantCount {
				t.Errorf("count = %d, want %d", got.Count, tc.wantCount)
			}
			if got.MalformedCount != tc.wantMalformed {
				t.Errorf("malformed = %d, want %d", got.MalformedCount, tc.wantMalformed)
			}
			// A non-VALID element has no child object, so nothing may be
			// fabricated inside it.
			for i, e := range got.Entries {
				if e.Presence == MilestoneFieldValid {
					continue
				}
				if e.BroadcastIdentifiers.Count != 0 || e.BroadcastIdentifiers.MalformedCount != 0 ||
					len(e.BroadcastIdentifiers.IDs) != 0 || len(e.BroadcastIdentifiers.Elements) != 0 {
					t.Errorf("entry %d (%s) fabricated child data: %+v", i, e.Presence, e.BroadcastIdentifiers)
				}
			}
		})
	}

	// The independence claim, stated directly: the two fixtures the repair
	// exists for must differ at the ELEMENT node and agree nowhere that would
	// let them be confused.
	nullElement := missedStreamsFrom(t, `[null]`)
	nullChild := missedStreamsFrom(t, `[{"broadcastIdentifiers":null}]`)
	if nullElement.Entries[0].Presence == nullChild.Entries[0].Presence {
		t.Error("a null element and a null child share an element presence")
	}
	if reflect.DeepEqual(nullElement.Entries[0], nullChild.Entries[0]) {
		t.Error("a null element and a null child produced identical entries")
	}
}

// TestMissedStreamsClassificationStructuralProperties covers acceptance D:
// properties that must hold across equivalent or rearranged wire text, using
// only the standard library and fixed, reproducible inputs.
func TestMissedStreamsClassificationStructuralProperties(t *testing.T) {
	t.Run("whitespace and key order do not change classification", func(t *testing.T) {
		compact := `[{"broadcastIdentifiers":[{"id":"x"}]},null,"nope"]`
		spaced := "[ {\n \"broadcastIdentifiers\" : [ { \"id\" : \"x\" } ]\n } , null , \"nope\" ]"
		if !reflect.DeepEqual(missedStreamsFrom(t, compact), missedStreamsFrom(t, spaced)) {
			t.Error("whitespace changed the classification")
		}
		// Key order within an element object is likewise irrelevant.
		a := `[{"broadcastIdentifiers":[{"id":"x"}],"other":1}]`
		b := `[{"other":1,"broadcastIdentifiers":[{"id":"x"}]}]`
		if !reflect.DeepEqual(missedStreamsFrom(t, a), missedStreamsFrom(t, b)) {
			t.Error("key order changed the classification")
		}
	})

	t.Run("reordering moves each element's classification with that element", func(t *testing.T) {
		// Fixed, reproducible permutations — no randomness, no seed to drift.
		parts := []string{`null`, `{"broadcastIdentifiers":null}`, `{"broadcastIdentifiers":[{"id":"x"}]}`, `"nope"`}
		want := []MilestoneFieldPresence{
			MilestoneFieldNull, MilestoneFieldValid, MilestoneFieldValid, MilestoneFieldMalformed,
		}
		for _, perm := range [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {1, 3, 0, 2}, {2, 0, 3, 1}} {
			elems := make([]string, 0, len(perm))
			expect := make([]MilestoneFieldPresence, 0, len(perm))
			for _, i := range perm {
				elems = append(elems, parts[i])
				expect = append(expect, want[i])
			}
			got := elementPresences(missedStreamsFrom(t, "["+strings.Join(elems, ",")+"]"))
			if !reflect.DeepEqual(got, expect) {
				t.Errorf("permutation %v: element presences = %v, want %v", perm, got, expect)
			}
		}
	})

	t.Run("inserting a null element does not rewrite its neighbours", func(t *testing.T) {
		neighbours := `{"broadcastIdentifiers":[{"id":"a"}]},{"broadcastIdentifiers":null}`
		before := missedStreamsFrom(t, `[`+neighbours+`]`)
		for _, at := range []string{`[null,` + neighbours + `]`, `[{"broadcastIdentifiers":[{"id":"a"}]},null,{"broadcastIdentifiers":null}]`, `[` + neighbours + `,null]`} {
			after := missedStreamsFrom(t, at)
			// Pull out the non-null entries; they must be untouched.
			kept := make([]MilestoneMissedStream, 0, 2)
			for _, e := range after.Entries {
				if e.Presence != MilestoneFieldNull {
					kept = append(kept, e)
				}
			}
			if !reflect.DeepEqual(kept, before.Entries) {
				t.Errorf("inserting null at %s rewrote a neighbour:\n before %+v\n after  %+v", at, before.Entries, kept)
			}
			if after.MalformedCount != before.MalformedCount {
				t.Errorf("inserting a null element changed MalformedCount %d -> %d",
					before.MalformedCount, after.MalformedCount)
			}
		}
	})
}

// TestDiagnosticStopsAtANonSuccessStatusInsteadOfRotatingClientIDs pins the
// status as the authority on whether a body is worth reading.
//
// gql.IsPersistedQueryNotFound is a raw substring test over the whole response
// and never looks at the status. A rejected response that merely CONTAINS the
// marker therefore used to drive the client-ID candidate loop: the
// authenticated request went out under every client ID this project ships, and
// because each answer carried the marker the read ended as UNSUPPORTED_QUERY -
// the wrong cause, reached by three times the requests.
func TestDiagnosticStopsAtANonSuccessStatusInsteadOfRotatingClientIDs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantClass MilestoneFailureClass
	}{
		{
			// The amplification-and-masking case.
			name:      "a rejection whose body carries the APQ marker",
			status:    http.StatusForbidden,
			body:      `{"errors":[{"message":"PersistedQueryNotFound"}]}`,
			wantClass: MilestoneFailureHTTPStatus,
		},
		{
			// The classification case: refused on status, not on whether the
			// payload happened to parse.
			name:      "a rejection whose body is not JSON at all",
			status:    http.StatusBadRequest,
			body:      `<html>go away</html>`,
			wantClass: MilestoneFailureHTTPStatus,
		},
		{
			// 401 keeps its own, more specific class.
			name:      "a token rejection",
			status:    http.StatusUnauthorized,
			body:      `{"errors":[{"message":"PersistedQueryNotFound"}]}`,
			wantClass: MilestoneFailureUnauthorized,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests int
			clientIDs := map[string]struct{}{}
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				clientIDs[r.Header.Get("Client-Id")] = struct{}{}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})

			obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

			if obs.Outcome != MilestoneUnavailable {
				t.Errorf("outcome = %q, want %q", obs.Outcome, MilestoneUnavailable)
			}
			if obs.FailureClass != tc.wantClass {
				t.Errorf("failure class = %q, want %q", obs.FailureClass, tc.wantClass)
			}
			if requests != 1 {
				t.Errorf("sent %d requests under %d client IDs; a refused status must stop the "+
					"candidate loop, not rotate through it", requests, len(clientIDs))
			}
			assertEmptySnapshot(t, obs.Snapshot)
		})
	}
}

// TestDiagnosticRefusesAmbiguousJSON pins duplicate object members as
// non-evidence.
//
// encoding/json applies last-value-wins to duplicate members, so a response
// carrying a real rejection followed by a benign duplicate decodes with the
// rejection ERASED. The fail-closed checks on the errors node cannot catch it:
// by the time they run they are looking at the lossy decoded map, in which only
// one of the two readings survives. The mutation path already refuses raw
// bodies like this; an observation has the same need.
func TestDiagnosticRefusesAmbiguousJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			// The erasure case: an explicit rejection hidden behind an empty
			// duplicate. Without the guard this records OBSERVED.
			name: "a rejection erased by a duplicate errors member",
			body: `{"errors":[{"message":"denied"}],"errors":[],` +
				`"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":null}}}}`,
		},
		{
			// Duplicates nested inside data are just as ambiguous.
			name: "a duplicate member nested inside data",
			body: `{"data":{"channel":{"id":"12345","id":"99999",` +
				`"self":{"watchStreakMilestone":null}}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.body)
			})

			obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

			if obs.Outcome != MilestoneUnavailable {
				t.Errorf("outcome = %q, want %q", obs.Outcome, MilestoneUnavailable)
			}
			if obs.FailureClass != MilestoneFailureAmbiguousJSON {
				t.Errorf("failure class = %q, want %q", obs.FailureClass, MilestoneFailureAmbiguousJSON)
			}
			assertEmptySnapshot(t, obs.Snapshot)
		})
	}

	// The counterpart, so the guard cannot drift into refusing valid bodies: a
	// response with no duplicate members is still a real observation.
	t.Run("a unique-membered response is unaffected", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":{"channel":{"id":"12345",`+
				`"self":{"watchStreakMilestone":null}}}}`)
		})
		obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")
		if obs.Outcome != MilestoneObserved {
			t.Fatalf("outcome = %q (%q), want %q", obs.Outcome, obs.FailureClass, MilestoneObserved)
		}
	})
}

// TestAmbiguousJSONIsRefusedBeforeTheAPQDetector pins the ORDER of the two
// diagnostic refusals, which is load-bearing rather than incidental.
//
// gql.IsPersistedQueryNotFound is a raw substring test over the whole body. A
// response carrying duplicate object members AND the marker is ambiguous first
// and APQ evidence second, so checking uniqueness after the detector lets the
// ambiguous body drive the client-ID candidate loop and end as
// UNSUPPORTED_QUERY — never reaching the AMBIGUOUS_JSON refusal at all. Found
// independently by two reviewers on the commit that introduced the guard.
func TestAmbiguousJSONIsRefusedBeforeTheAPQDetector(t *testing.T) {
	var requests int
	clientIDs := map[string]struct{}{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		clientIDs[r.Header.Get("Client-Id")] = struct{}{}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"errors":[{"message":"PersistedQueryNotFound"}],"errors":[],`+
			`"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":null}}}}`)
	})

	obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

	if obs.Outcome != MilestoneUnavailable || obs.FailureClass != MilestoneFailureAmbiguousJSON {
		t.Errorf("outcome = %q/%q, want UNAVAILABLE/%s", obs.Outcome, obs.FailureClass,
			MilestoneFailureAmbiguousJSON)
	}
	if requests != 1 {
		t.Errorf("sent %d requests under %d client IDs; an ambiguous body is not APQ evidence and "+
			"must not drive the candidate loop", requests, len(clientIDs))
	}
	assertEmptySnapshot(t, obs.Snapshot)
}

// TestAnOversizedBodyCannotChangeATokenRejectionsClass pins that the recorded
// failure class does not depend on a value the peer chooses.
//
// A 401 is settled by the status alone and needs no body. But an oversized body
// fails the bounded read before the transport reaches its 401 branch, so the
// error lands on the generic transport fallback — and the status refinement
// would then report HTTP_STATUS, letting the peer pick which failure gets
// recorded simply by making its rejection large.
func TestAnOversizedBodyCannotChangeATokenRejectionsClass(t *testing.T) {
	oversized := bytes.Repeat([]byte("A"), maxDiagnosticResponseBytes+4096)

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"a small rejection body", []byte(`{"message":"unauthorized"}`)},
		{"an oversized rejection body", oversized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write(tc.body)
			})

			obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

			if obs.Outcome != MilestoneUnavailable || obs.FailureClass != MilestoneFailureUnauthorized {
				t.Fatalf("outcome = %q/%q, want UNAVAILABLE/%s — the body's SIZE changed the class",
					obs.Outcome, obs.FailureClass, MilestoneFailureUnauthorized)
			}
			assertEmptySnapshot(t, obs.Snapshot)
		})
	}
}

// TestOversizedCollectionsAreRefusedBeforeTheyAreBuilt pins the cardinality
// bound, which the byte limit does not give.
//
// A megabyte of wire is not a megabyte of memory: a dense array of tiny
// elements turns it into hundreds of thousands of Go structs, once per online
// target, on the bonus poll goroutine. The structs buy nothing — the record
// prints exact counts and a small identifier sample, so everything past the
// sample exists only to be tallied.
//
// Refusal rather than truncation is deliberate: a partly-walked array would
// report an exact Count beside a tally taken over only some of the elements,
// which reads as evidence while being none.
func TestOversizedCollectionsAreRefusedBeforeTheyAreBuilt(t *testing.T) {
	// Just under the BYTE limit, so this is the cardinality bound doing the
	// work and not the body cap.
	denseBody := func(t *testing.T) string {
		t.Helper()
		prefix := `{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{"missedStreams":[`
		suffix := `]}}}}}`
		room := maxDiagnosticResponseBytes - len(prefix) - len(suffix)
		n := room / len("0,")
		body := prefix + strings.Repeat("0,", n-1) + "0" + suffix
		if len(body) > maxDiagnosticResponseBytes {
			t.Fatalf("fixture is %d bytes, over the %d-byte body limit; this would test the wrong bound",
				len(body), maxDiagnosticResponseBytes)
		}
		return body
	}

	// The nested array counts toward the same budget, so the bound cannot be
	// walked around one level down.
	nestedBody := func(t *testing.T) string {
		t.Helper()
		var b strings.Builder
		b.WriteString(`{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{"missedStreams":[`)
		b.WriteString(`{"broadcastIdentifiers":[`)
		for i := 0; i <= maxMilestoneCollectionElements; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"id":"b"}`)
		}
		b.WriteString(`]}]}}}}}`)
		return b.String()
	}

	for _, tc := range []struct {
		name string
		body func(*testing.T) string
	}{
		{"a dense top-level collection", denseBody},
		{"a dense nested collection", nestedBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body(t)
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, body)
			})

			obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

			if obs.Outcome != MilestoneUnavailable ||
				obs.FailureClass != MilestoneFailureOversizedCollection {
				t.Fatalf("outcome = %q/%q, want UNAVAILABLE/%s", obs.Outcome, obs.FailureClass,
					MilestoneFailureOversizedCollection)
			}
			if got := len(obs.Snapshot.MissedStreams.Entries); got != 0 {
				t.Fatalf("%d entry structs were built for a refused response; the refusal must come "+
					"BEFORE the parse, or it does not bound anything", got)
			}
			assertEmptySnapshot(t, obs.Snapshot)
		})
	}

	// The counterpart, so the bound cannot drift into refusing real responses:
	// a credible milestone node is still a real observation.
	t.Run("a credible collection is unaffected", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{`+
				`"missedStreams":[{"broadcastIdentifiers":[{"id":"b-1"},{"id":"b-2"}]}],`+
				`"watchStreakMilestone":{"value":"4"}}}}}}`)
		})
		obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")
		if obs.Outcome != MilestoneObserved {
			t.Fatalf("outcome = %q (%q), want OBSERVED", obs.Outcome, obs.FailureClass)
		}
		if obs.Snapshot.MissedStreams.Count != 1 {
			t.Fatalf("missedStreams count = %d, want 1", obs.Snapshot.MissedStreams.Count)
		}
	})
}

// TestAnOversizedResponseIsRefusedBeforeItIsDecoded pins the bound that the
// byte limit and the collection limit both leave open.
//
// Refusing a response is only cheap if the refusal runs before the expensive
// part. The collection limit added earlier reads the DECODED response, so
// encoding/json had already materialised the whole document by the time it
// could say no: measured at ~97 MB and ~300 ms to refuse a 1 MiB body. The
// outcome was right and the cost was paid anyway.
//
// So this asserts the cost, not just the class. A test that only checked the
// failure class would have passed against the version this fixes — which is
// exactly what happened.
func TestAnOversizedResponseIsRefusedBeforeItIsDecoded(t *testing.T) {
	prefix := `{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{"missedStreams":[`
	suffix := `]}}}}}`
	room := maxDiagnosticResponseBytes - len(prefix) - len(suffix)
	elements := room / len("0,")
	body := prefix + strings.Repeat("0,", elements-1) + "0" + suffix

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	})

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")
	runtime.ReadMemStats(&after)

	if obs.Outcome != MilestoneUnavailable ||
		obs.FailureClass != MilestoneFailureOversizedCollection {
		t.Fatalf("outcome = %q/%q, want UNAVAILABLE/%s", obs.Outcome, obs.FailureClass,
			MilestoneFailureOversizedCollection)
	}
	assertEmptySnapshot(t, obs.Snapshot)

	// A coarse bound, on purpose. The point is the ORDER OF MAGNITUDE: decoding
	// this body costs ~97 MB, streaming past it costs a few. Anything in
	// between still means the decode was skipped. A tight threshold here would
	// be a flaky test about allocator behaviour rather than about the guard.
	const budgetMB = 20
	allocatedMB := float64(after.TotalAlloc-before.TotalAlloc) / (1 << 20)
	if allocatedMB > budgetMB {
		t.Fatalf("refusing a %d-byte body allocated %.1f MB (budget %d MB): the response was decoded "+
			"before it was refused, so the refusal bounded nothing",
			len(body), allocatedMB, budgetMB)
	}
	t.Logf("refused %d elements in %.1f MB", elements, allocatedMB)
}

// TestAContainerHeavyResponseIsAlsoRefusedBeforeDecoding pins the half of the
// pre-decode bound that its first version missed.
//
// The scan counts JSON values, and the first version counted only scalars — so
// a body made of nothing but delimiters counted as ZERO. That is the cheapest
// amplification available: 349,496 empty objects fit inside a just-under-1 MiB
// body, scanned as within limit, and still cost ~88 MB to decode. The eventual
// outcome was right, because the collection limit caught them after the fact,
// which is exactly why an outcome-only assertion proves nothing here.
func TestAContainerHeavyResponseIsAlsoRefusedBeforeDecoding(t *testing.T) {
	prefix := `{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{"missedStreams":[`
	suffix := `]}}}}}`
	room := maxDiagnosticResponseBytes - len(prefix) - len(suffix)
	objects := room / len("{},")
	body := prefix + strings.Repeat("{},", objects-1) + "{}" + suffix

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	})

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")
	runtime.ReadMemStats(&after)

	if obs.Outcome != MilestoneUnavailable ||
		obs.FailureClass != MilestoneFailureOversizedCollection {
		t.Fatalf("outcome = %q/%q, want UNAVAILABLE/%s", obs.Outcome, obs.FailureClass,
			MilestoneFailureOversizedCollection)
	}

	const budgetMB = 20
	allocatedMB := float64(after.TotalAlloc-before.TotalAlloc) / (1 << 20)
	if allocatedMB > budgetMB {
		t.Fatalf("refusing %d empty objects allocated %.1f MB (budget %d MB): container tokens are "+
			"not being counted, so the body was decoded before it was refused",
			objects, allocatedMB, budgetMB)
	}
	t.Logf("refused %d empty objects in %.1f MB", objects, allocatedMB)
}

// TestTheAPQMarkerIsOnlyHonouredWhereARejectionPutsIt pins that a peer cannot
// destroy a valid observation by writing a string.
//
// gql.IsPersistedQueryNotFound is bytes.Contains over the whole body. For a
// business read that is defensible — those responses are not attacker-shaped
// text. For this read the consequence is not a misclassification but a
// DESTROYED observation: a perfectly valid response with the marker in an
// unrelated field was resent under every candidate client ID and then reported
// as UNSUPPORTED_QUERY, discarding the real milestone data it carried.
//
// The genuine rejection must still be recognised, in both attested spellings,
// because it is the EXPECTED steady state for this operation until live hash
// acceptance is evidenced.
func TestTheAPQMarkerIsOnlyHonouredWhereARejectionPutsIt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		wantOutcome WatchStreakMilestoneOutcome
		wantClass   MilestoneFailureClass
		wantRequest int
	}{
		{
			// The destroyed-observation case.
			name: "the marker in an unrelated field is not a rejection",
			body: `{"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":{` +
				`"shareStatus":"PersistedQueryNotFound","watchStreakMilestone":{"value":"4"}}}}}}`,
			wantOutcome: MilestoneObserved,
			wantRequest: 1,
		},
		{
			// Genuine, message spelling.
			name:        "a structured rejection, message spelling",
			body:        `{"errors":[{"message":"PersistedQueryNotFound"}]}`,
			wantOutcome: MilestoneUnsupported,
			wantClass:   MilestoneFailureQueryNotFound,
			wantRequest: 3,
		},
		{
			// Genuine, extensions spelling.
			name: "a structured rejection, extensions spelling",
			body: `{"errors":[{"message":"whatever","extensions":` +
				`{"code":"PERSISTED_QUERY_NOT_FOUND"}}]}`,
			wantOutcome: MilestoneUnsupported,
			wantClass:   MilestoneFailureQueryNotFound,
			wantRequest: 3,
		},
		{
			// Usable data beside the rejection means it is not one.
			name: "the marker beside usable data is not a rejection",
			body: `{"errors":[{"message":"PersistedQueryNotFound"}],` +
				`"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":null}}}}`,
			wantOutcome: MilestoneGraphQLError,
			wantClass:   MilestoneFailureGraphQLTopLevel,
			wantRequest: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests int
			clientIDs := map[string]struct{}{}
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				clientIDs[r.Header.Get("Client-Id")] = struct{}{}
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.body)
			})

			obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

			if obs.Outcome != tc.wantOutcome || obs.FailureClass != tc.wantClass {
				t.Errorf("outcome = %q/%q, want %q/%q", obs.Outcome, obs.FailureClass,
					tc.wantOutcome, tc.wantClass)
			}
			if requests != tc.wantRequest {
				t.Errorf("sent %d requests under %d client IDs, want %d", requests,
					len(clientIDs), tc.wantRequest)
			}
		})
	}
}

// TestMalformedJSONKeepsItsOwnFailureClass pins that AMBIGUOUS_JSON means what
// it says.
//
// jsonObjectKeysAreUnique folds scanner errors into its "not unique" answer,
// which is the right shape for its own callers and the wrong one here, where
// the answer becomes a named class. Reporting a truncated body as
// AMBIGUOUS_JSON tells an operator that Twitch sent duplicate members when it
// sent no such thing — and lets a peer choose the recorded class with syntax
// alone, which is the same defect as a body's SIZE choosing it.
func TestMalformedJSONKeepsItsOwnFailureClass(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"a truncated document", `{"data":{"channel":`},
		{"trailing bytes after the document", `{"data":{"channel":{"id":"1"}}} NOPE`},
		{"an empty body", ``},
		{"not JSON at all", `<html>go away</html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.body)
			})

			obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")

			if obs.FailureClass == MilestoneFailureAmbiguousJSON {
				t.Fatalf("malformed input reported as %s, a class that means duplicate object members",
					MilestoneFailureAmbiguousJSON)
			}
			if obs.Outcome == MilestoneObserved {
				t.Fatalf("malformed input recorded as an observation")
			}
			assertEmptySnapshot(t, obs.Snapshot)
		})
	}

	// The counterpart: real duplicates still get the class that names them.
	t.Run("genuine duplicate members keep AMBIGUOUS_JSON", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"errors":[{"message":"denied"}],"errors":[],`+
				`"data":{"channel":{"id":"12345","self":{"watchStreakMilestone":null}}}}`)
		})
		obs := c.ObserveWatchStreakMilestone(context.Background(), "12345", "somestreamer")
		if obs.FailureClass != MilestoneFailureAmbiguousJSON {
			t.Fatalf("failure class = %q, want %s", obs.FailureClass, MilestoneFailureAmbiguousJSON)
		}
	})
}
