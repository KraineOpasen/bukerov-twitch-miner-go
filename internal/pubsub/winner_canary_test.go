package pubsub

// Tests for the P4 winner-field canary (winner_canary.go).
//
// Oracles are independent of the implementation: the expected records are
// literals written from the owner contract into testdata/winner_canary and the
// tables below, and every event_key_hash literal was computed outside Go, with
// both `printf 'p4-winner-canary/v2\0event\0<id>' | sha256sum` and Python's
// hashlib, which agreed on every vector. referenceWinnerCanary restates the
// contract rule by rule for the generated-input properties; the fixtures and
// the literal admission cases keep it from being the only witness.
//
// Tests that replace the process-wide slog default, os.Stdout, the working
// directory or internal/version.Version never run in parallel (t.Setenv and
// t.Chdir enforce that), and they put back everything they change.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	_ "time/tzdata" // a fresh process can then load a named zone on any host
	"unsafe"
	"weak"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/logger"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

// --- Contract oracles -------------------------------------------------------

// winnerCanaryAttrKeys is the exact canary attribute set, in emission order, as
// the owner contract lists it.
var winnerCanaryAttrKeys = []string{
	"canary_schema", "build_version", "source_topic_type", "source_message_type",
	"round_status", "winner_field_name", "winner_field_presence", "winner_field_json_type",
	"winner_value_empty", "outcomes_presence", "outcome_count", "outcome_ids_valid",
	"winner_match_count", "winner_index", "event_key_hash", "observed_at_utc", "result",
}

// winnerCanaryConstants are the attributes every record carries verbatim.
var winnerCanaryConstants = map[string]string{
	"canary_schema":       "p4-winner-canary/v2",
	"source_topic_type":   "predictions-channel-v1",
	"source_message_type": "event-updated",
	"round_status":        "RESOLVED",
	"winner_field_name":   "winning_outcome_id",
}

// winnerCanaryFrameKeys are the ten attributes that depend on the frame; the
// testdata fixtures state exactly these.
var winnerCanaryFrameKeys = []string{
	"winner_field_presence", "winner_field_json_type", "winner_value_empty",
	"outcomes_presence", "outcome_count", "outcome_ids_valid",
	"winner_match_count", "winner_index", "event_key_hash", "result",
}

func winnerCanaryAttrKind(key string) slog.Kind {
	switch key {
	case "winner_value_empty", "outcome_ids_valid":
		return slog.KindBool
	case "outcome_count", "winner_match_count", "winner_index":
		return slog.KindInt64
	default:
		return slog.KindString
	}
}

// Independently computed event_key_hash vectors (see the file comment).
const (
	canaryHash0001 = "sha256:b691aded51cd9b59dc0a2634ee71304714dda4419daad8d965158fa75767c08d"
	canaryHash0002 = "sha256:481637c78b6ee02a5ddc5b1d461679a7b21427e07e25ad9cfaad9e23cf6de903"
	canaryHash0003 = "sha256:5a06dc7892a2a61dc42b50a5cf5bdcd0857e1d3116aa73c08fc880f89df8b7cc"
	canaryHash0064 = "sha256:6a1b501dc6e3371b2bfd568f0f542ba0876c5c2588bb3e98c14c26109a836bc9"
	canaryHashUTF8 = "sha256:5b3f35e3ba4f91b63368e2667e35550fa8f950c35a467e1e479b68c8e6085b05" // "synthetic-évènement-0003", 26 bytes
	canaryHashX4K  = "sha256:a5a182010fa2254ee2d850013ac8a58b7cd7002d0316eb5a54bca4af568ab234" // 4096 x 'x'
	canaryHashE4K  = "sha256:bfa3dd9e20b9678f006c8a0734efc48c86a5c497d516a290db91a41ef4737a3f" // 2048 x U+00E9, 4096 bytes
)

const (
	canaryObserved    = "WINNER_FIELD_OBSERVED"
	canaryNotObserved = "NOT_OBSERVED_IN_THIS_EVENT"
	canaryMalformed   = "MALFORMED_IN_THIS_EVENT"

	canaryTopicChan1  = "predictions-channel-v1.chan-1"
	canaryLogKey      = "winner-canary-test"
	canaryGlobalsEnv  = "P4_WINNER_CANARY_TEST_GLOBALS"
	canaryIsolatedEnv = "P4_WINNER_CANARY_TEST_ISOLATED"
	canaryTimingsEnv  = "P4_WINNER_CANARY_TEST_TIMINGS"
)

// canaryFrameAttrs is the literal expectation for the ten frame-dependent
// attributes, in the order winnerCanaryFrameKeys lists them.
type canaryFrameAttrs struct {
	winnerPresence   string
	winnerJSONType   string
	winnerValueEmpty bool
	outcomesPresence string
	outcomeCount     int64
	outcomeIDsValid  bool
	winnerMatchCount int64
	winnerIndex      int64
	eventKeyHash     string
	result           string
}

func (f canaryFrameAttrs) values() map[string]interface{} {
	return map[string]interface{}{
		"winner_field_presence":  f.winnerPresence,
		"winner_field_json_type": f.winnerJSONType,
		"winner_value_empty":     f.winnerValueEmpty,
		"outcomes_presence":      f.outcomesPresence,
		"outcome_count":          f.outcomeCount,
		"outcome_ids_valid":      f.outcomeIDsValid,
		"winner_match_count":     f.winnerMatchCount,
		"winner_index":           f.winnerIndex,
		"event_key_hash":         f.eventKeyHash,
		"result":                 f.result,
	}
}

// canaryPositive0001 is the record of a two-outcome frame for event
// synthetic-event-0001 whose winner names the second outcome.
var canaryPositive0001 = canaryFrameAttrs{
	winnerPresence: "PRESENT", winnerJSONType: "string", outcomesPresence: "PRESENT",
	outcomeCount: 2, outcomeIDsValid: true, winnerMatchCount: 1, winnerIndex: 1,
	eventKeyHash: canaryHash0001, result: canaryObserved,
}

// canaryUntracked0003 is the record of a two-outcome frame for event
// synthetic-event-0003, a round nobody tracks, that carries no winner.
var canaryUntracked0003 = canaryFrameAttrs{
	winnerPresence: "ABSENT_ON_WIRE", winnerJSONType: "absent", outcomesPresence: "PRESENT",
	outcomeCount: 2, outcomeIDsValid: true, winnerMatchCount: -1, winnerIndex: -1,
	eventKeyHash: canaryHash0003, result: canaryNotObserved,
}

// --- Record inspection --------------------------------------------------------

// canaryFields checks that attrs are exactly the contract's attribute set, in
// order, each of its scalar kind, and returns their values. A value of any other
// kind (Any, LogValuer, Group) would fail here, which is what keeps untrusted
// values away from every handler.
func canaryFields(t *testing.T, attrs []slog.Attr) map[string]interface{} {
	t.Helper()
	attrKeys := make([]string, 0, len(attrs))
	for _, a := range attrs {
		attrKeys = append(attrKeys, a.Key)
	}
	if !reflect.DeepEqual(attrKeys, winnerCanaryAttrKeys) {
		t.Fatalf("canary attribute keys =\n  %v\nwant exactly\n  %v", attrKeys, winnerCanaryAttrKeys)
	}
	out := make(map[string]interface{}, len(attrs))
	for _, a := range attrs {
		want := winnerCanaryAttrKind(a.Key)
		if a.Value.Kind() != want {
			t.Fatalf("attribute %s has kind %v, want %v", a.Key, a.Value.Kind(), want)
		}
		switch want {
		case slog.KindBool:
			out[a.Key] = a.Value.Bool()
		case slog.KindInt64:
			out[a.Key] = a.Value.Int64()
		default:
			out[a.Key] = a.Value.String()
		}
	}
	return out
}

func canaryRecordAttrs(r slog.Record) []slog.Attr {
	var attrs []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	return attrs
}

// assertCanaryFields compares one record's fields with the literal frame
// expectation, the contract constants, the build label and the receiver-time
// window [before, after].
func assertCanaryFields(t *testing.T, got, want map[string]interface{}, build string, before, after time.Time) {
	t.Helper()
	for k, v := range winnerCanaryConstants {
		if got[k] != v {
			t.Errorf("%s = %v, want %q", k, got[k], v)
		}
	}
	if got["build_version"] != build {
		t.Errorf("build_version = %v, want %q", got["build_version"], build)
	}
	for _, k := range winnerCanaryFrameKeys {
		if got[k] != want[k] {
			t.Errorf("%s = %#v, want %#v", k, got[k], want[k])
		}
	}
	stamp, _ := got["observed_at_utc"].(string)
	observed, err := time.Parse(time.RFC3339, stamp)
	if err != nil || !strings.HasSuffix(stamp, "Z") {
		t.Fatalf("observed_at_utc = %q: not an RFC3339 UTC time (%v)", stamp, err)
	}
	if observed.Before(before.UTC().Truncate(time.Second)) || observed.After(after.UTC()) {
		t.Errorf("observed_at_utc = %s, outside the receive window [%s, %s]", stamp,
			before.UTC().Format(time.RFC3339Nano), after.UTC().Format(time.RFC3339Nano))
	}
}

// renderCanary formats a record through the real slog Text and JSON handlers,
// which resolve every value the way a production sink would.
func renderCanary(t *testing.T, r slog.Record) string {
	t.Helper()
	var buf bytes.Buffer
	if err := slog.NewTextHandler(&buf, nil).Handle(context.Background(), r.Clone()); err != nil {
		t.Fatalf("text handler: %v", err)
	}
	if err := slog.NewJSONHandler(&buf, nil).Handle(context.Background(), r.Clone()); err != nil {
		t.Fatalf("json handler: %v", err)
	}
	return buf.String()
}

// assertCanaryPrivate screens rendered output for the frame values that carry a
// marker: every fixture id, winner, channel and login that is a string and not
// blank contains "synthetic-" in some letter case, every title, secret,
// predictor and unknown key or value carries "SENTINEL", and the in-package test
// streamer is "streamer" on "chan-1". Frame content without a marker — a blank,
// numeric or boolean id or winner, a known key name, a timestamp, a colour, a
// total — is kept out by the exact attribute checks and the receive-time window
// instead.
func assertCanaryPrivate(t *testing.T, rendered string) {
	t.Helper()
	lower := strings.ToLower(rendered)
	for _, forbidden := range []string{"synthetic-", "sentinel", "chan-1", "streamer"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("output leaks %q:\n%s", forbidden, rendered)
		}
	}
}

// --- Process-wide logging seams -----------------------------------------------

// canaryCapture is a lossless, level-honest slog.Handler that keeps every
// record it is offered at or above level. The application's console writer may
// drop a line when its queue is full; this one never does, so a missing record
// is the canary's doing, never transport loss.
type canaryCapture struct {
	level   slog.Level
	mu      sync.Mutex
	records []slog.Record
}

// canaryCaptureAll lies below every level this module logs at, so a record at
// any of them shows up.
const canaryCaptureAll = slog.LevelDebug - 8

func (c *canaryCapture) Enabled(_ context.Context, l slog.Level) bool { return l >= c.level }

func (c *canaryCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r.Clone())
	return nil
}

func (c *canaryCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *canaryCapture) WithGroup(string) slog.Handler      { return c }

// all returns every captured record, whatever its message or level.
func (c *canaryCapture) all() []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]slog.Record(nil), c.records...)
}

// take returns every record captured since the last take and forgets them.
func (c *canaryCapture) take() []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	taken := c.records
	c.records = nil
	return taken
}

// canaries returns the canary records captured so far, in arrival order.
func (c *canaryCapture) canaries() []slog.Record {
	var out []slog.Record
	for _, r := range c.all() {
		if r.Message == "p4_winner_canary" {
			out = append(out, r)
		}
	}
	return out
}

// canaryProbeHandler hands every record to onRecord on the emitting goroutine,
// holding no lock of its own, so a test can inspect the moment of emission.
type canaryProbeHandler struct{ onRecord func(slog.Record) }

func (h canaryProbeHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h canaryProbeHandler) Handle(_ context.Context, r slog.Record) error {
	h.onRecord(r.Clone())
	return nil
}

func (h canaryProbeHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h canaryProbeHandler) WithGroup(string) slog.Handler      { return h }

// failingCanaryHandler fails every record, standing in for a sink whose write
// errors. It reports each canary record it is offered to attempt, with the
// record's event_key_hash, at the moment the record is offered.
type failingCanaryHandler struct{ attempt func(hash string) }

func (f *failingCanaryHandler) Enabled(context.Context, slog.Level) bool { return true }

func (f *failingCanaryHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "p4_winner_canary" {
		f.attempt(canaryRecordHash(r))
	}
	return errors.New("synthetic log sink failure")
}

// canaryRecordHash returns a canary record's event_key_hash.
func canaryRecordHash(r slog.Record) string {
	var hash string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "event_key_hash" {
			hash = a.Value.String()
			return false
		}
		return true
	})
	return hash
}

func (f *failingCanaryHandler) WithAttrs([]slog.Attr) slog.Handler { return f }
func (f *failingCanaryHandler) WithGroup(string) slog.Handler      { return f }

// canaryPreserveProcessLogging puts back everything slog.SetDefault changes:
// the previous default and the log package's writer, flags and prefix
// (restoring the slog default alone does not undo the log package
// redirection). Its t.Setenv refuses a parallel test.
func canaryPreserveProcessLogging(t *testing.T) {
	t.Helper()
	t.Setenv(canaryGlobalsEnv, "1")
	prev := slog.Default()
	prevOut, prevFlags, prevPrefix := log.Writer(), log.Flags(), log.Prefix()
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})
}

// canaryUseProcessLogger makes h the process-wide slog default for one test.
func canaryUseProcessLogger(t *testing.T, h slog.Handler) {
	t.Helper()
	canaryPreserveProcessLogging(t)
	slog.SetDefault(slog.New(h))
}

// canaryInFreshProcess runs the calling top-level test again in a child process
// of its own and reports whether this is that child, where the test body runs.
// Tests that count every record or line the process logs need it: another test
// in this package can leave a client behind that logs a reconnect a minute
// later, and in a long multi-count run such a line could land in the window
// being counted. The child runs nothing but this test, so everything it counts
// is this test's own. env is added to the child's environment.
//
// The child runs with the parent's current GOMAXPROCS (so -cpu applies to it)
// and stops before the parent's deadline. It is started directly, not through a go
// test -exec wrapper; its coverage is not collected; and it does not linger
// after its test returns, so a race that could only complete after that would
// go unreported — none of these tests leaves a goroutine running.
func canaryInFreshProcess(t *testing.T, env ...string) bool {
	t.Helper()
	if canaryIsFreshChild(t) {
		return true
	}
	timeout := 5 * time.Minute
	if deadline, ok := t.Deadline(); ok {
		// Ten seconds early, so a hung child reports itself before the parent's
		// own timeout ends everything.
		timeout = max(time.Until(deadline)-10*time.Second, time.Second)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v",
		"-test.cpu="+strconv.Itoa(runtime.GOMAXPROCS(0)), "-test.timeout="+timeout.String())
	// Built with -race, the child would otherwise wait a second before exiting.
	cmd.Env = append(append(os.Environ(), canaryIsolatedEnv+"="+t.Name(),
		"GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0")), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s in a fresh process: %v\n%s", t.Name(), err, out)
	}
	if !bytes.Contains(out, []byte("--- PASS: "+t.Name()+" (")) {
		t.Fatalf("the fresh process did not run %s:\n%s", t.Name(), out)
	}
	return false
}

// canaryIsFreshChild reports whether this process is the fresh child that
// canaryInFreshProcess started for the calling top-level test.
func canaryIsFreshChild(t *testing.T) bool {
	return os.Getenv(canaryIsolatedEnv) == t.Name()
}

// canaryStdoutCapture swaps os.Stdout for a pipe that a goroutine drains for
// the whole test, so the application logger's console writer never blocks.
type canaryStdoutCapture struct {
	prev *os.File
	r, w *os.File
	done chan struct{}
	out  bytes.Buffer
	once sync.Once
}

func canaryCaptureStdout(t *testing.T) *canaryStdoutCapture {
	t.Helper()
	t.Setenv(canaryGlobalsEnv, "1")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	c := &canaryStdoutCapture{prev: os.Stdout, r: r, w: w, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		_, _ = io.Copy(&c.out, r)
	}()
	os.Stdout = w
	t.Cleanup(func() { c.finish() })
	return c
}

// finish restores os.Stdout, closes the write end and waits for the drain to
// reach EOF. Call it after Logger.Close has flushed the console queue.
func (c *canaryStdoutCapture) finish() string {
	c.once.Do(func() {
		os.Stdout = c.prev
		_ = c.w.Close()
		<-c.done
		_ = c.r.Close()
	})
	return c.out.String()
}

// canaryTextLineFields parses one slog TextHandler line into ordered pairs.
func canaryTextLineFields(t *testing.T, line string) ([]string, map[string]string) {
	t.Helper()
	var lineKeys []string
	vals := map[string]string{}
	rest := line
	for rest != "" {
		eq := strings.IndexByte(rest, '=')
		if eq <= 0 {
			t.Fatalf("unparseable log line %q", line)
		}
		key := rest[:eq]
		rest = rest[eq+1:]
		var val string
		if strings.HasPrefix(rest, `"`) {
			quoted, err := strconv.QuotedPrefix(rest)
			if err != nil {
				t.Fatalf("unparseable quoted value in %q: %v", line, err)
			}
			if val, err = strconv.Unquote(quoted); err != nil {
				t.Fatalf("unquote %q: %v", quoted, err)
			}
			rest = rest[len(quoted):]
		} else if sp := strings.IndexByte(rest, ' '); sp >= 0 {
			val, rest = rest[:sp], rest[sp:]
		} else {
			val, rest = rest, ""
		}
		rest = strings.TrimPrefix(rest, " ")
		lineKeys = append(lineKeys, key)
		vals[key] = val
	}
	return lineKeys, vals
}

// canaryLogLines returns the non-empty lines of logger output.
func canaryLogLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// assertCanaryTextLine checks one rendered line: standard logger metadata first
// (time, level, msg), then exactly the canary attributes with literal values.
func assertCanaryTextLine(t *testing.T, line string, want map[string]interface{}, before, after time.Time) {
	t.Helper()
	lineKeys, vals := canaryTextLineFields(t, line)
	wantKeys := append([]string{"time", "level", "msg"}, winnerCanaryAttrKeys...)
	if !reflect.DeepEqual(lineKeys, wantKeys) {
		t.Fatalf("line keys =\n  %v\nwant\n  %v\nline: %s", lineKeys, wantKeys, line)
	}
	if vals["level"] != "INFO" || vals["msg"] != "p4_winner_canary" {
		t.Fatalf("level/msg = %q/%q, want INFO/p4_winner_canary", vals["level"], vals["msg"])
	}
	got := map[string]interface{}{}
	for _, k := range winnerCanaryAttrKeys {
		switch winnerCanaryAttrKind(k) {
		case slog.KindBool:
			b, err := strconv.ParseBool(vals[k])
			if err != nil {
				t.Fatalf("%s = %q is not a bool", k, vals[k])
			}
			got[k] = b
		case slog.KindInt64:
			n, err := strconv.ParseInt(vals[k], 10, 64)
			if err != nil {
				t.Fatalf("%s = %q is not an integer", k, vals[k])
			}
			got[k] = n
		default:
			got[k] = vals[k]
		}
	}
	assertCanaryFields(t, got, want, version.Version, before, after)
}

// --- Frames and fixtures ------------------------------------------------------

type winnerCanaryFixtureFile struct {
	Description string             `json:"description"`
	Cases       []winnerCanaryCase `json:"cases"`
}

type winnerCanaryCase struct {
	Name    string                     `json:"name"`
	Topic   string                     `json:"topic"`
	Message json.RawMessage            `json:"message"`
	Record  *bool                      `json:"record"`
	Want    map[string]json.RawMessage `json:"want"`

	file string
}

// want decodes the fixture's literal expectation into typed values.
func (c winnerCanaryCase) want(tb testing.TB) map[string]interface{} {
	tb.Helper()
	out := map[string]interface{}{}
	for _, k := range winnerCanaryFrameKeys {
		raw, ok := c.Want[k]
		if !ok {
			tb.Fatalf("%s/%s: want lacks %s", c.file, c.Name, k)
		}
		var err error
		switch winnerCanaryAttrKind(k) {
		case slog.KindBool:
			var b bool
			err = json.Unmarshal(raw, &b)
			out[k] = b
		case slog.KindInt64:
			var n int64
			err = json.Unmarshal(raw, &n)
			out[k] = n
		default:
			var s string
			err = json.Unmarshal(raw, &s)
			out[k] = s
		}
		if err != nil {
			tb.Fatalf("%s/%s: %s: %v", c.file, c.Name, k, err)
		}
	}
	return out
}

// loadWinnerCanaryCases strictly loads every testdata/winner_canary fixture.
func loadWinnerCanaryCases(tb testing.TB) []winnerCanaryCase {
	tb.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "winner_canary", "*.json"))
	if err != nil || len(paths) == 0 {
		tb.Fatalf("no winner canary fixtures (%v)", err)
	}
	sort.Strings(paths)
	var all []winnerCanaryCase
	seen := map[string]bool{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			tb.Fatalf("read %s: %v", path, err)
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		var file winnerCanaryFixtureFile
		if err := dec.Decode(&file); err != nil {
			tb.Fatalf("decode %s: %v", path, err)
		}
		if err := dec.Decode(&json.RawMessage{}); err != io.EOF {
			tb.Fatalf("%s: content after the fixture object (%v)", path, err)
		}
		if file.Description == "" || len(file.Cases) == 0 {
			tb.Fatalf("%s: a fixture file needs a description and cases", path)
		}
		for _, c := range file.Cases {
			c.file = filepath.Base(path)
			id := c.file + "/" + c.Name
			switch {
			case c.Name == "" || seen[id]:
				tb.Fatalf("%s: empty or duplicate case name %q", path, c.Name)
			case c.Record == nil:
				tb.Fatalf("%s: record must be stated explicitly", id)
			case *c.Record && len(c.Want) != len(winnerCanaryFrameKeys):
				tb.Fatalf("%s: want must state exactly %d attributes, has %d", id, len(winnerCanaryFrameKeys), len(c.Want))
			case !*c.Record && c.Want != nil:
				tb.Fatalf("%s: a case without a record must not state want", id)
			}
			seen[id] = true
			all = append(all, c)
		}
	}
	return all
}

func parseCanaryFrame(tb testing.TB, topic string, message []byte) *PubSubMessage {
	tb.Helper()
	msg, err := ParsePubSubMessage(&WSData{Topic: topic, Message: string(message)})
	if err != nil {
		tb.Fatalf("parse frame: %v", err)
	}
	return msg
}

// canaryStreamer is a points-enabled online streamer whose identity is
// synthetic, so a leak of it into the record is detectable.
func canaryStreamer() *models.Streamer {
	return newNamedTestStreamer("synthetic-login-4417", "synthetic-channel-4417", 100000)
}

// canaryUpdateFrame is a predictions-channel-v1 event-updated inner message for
// a two-outcome round with ids synthetic-outcome-a/b; outcome a carries one top
// predictor staking 100 points.
func canaryUpdateFrame(eventID, status string, pointsA, pointsB int, winner string) []byte {
	winnerField := ""
	if winner != "" {
		winnerField = fmt.Sprintf(`,"winning_outcome_id":%q`, winner)
	}
	return []byte(fmt.Sprintf(`{"type":"event-updated","data":{"timestamp":"2026-09-23T12:10:00Z","event":{`+
		`"id":%q,"status":%q,"title":"SENTINEL-TITLE-3b8f","auth_token":"SENTINEL-SECRET-oauth-7d1f","outcomes":[`+
		`{"id":"synthetic-outcome-a","title":"SENTINEL-OUTCOME-TITLE-A","total_points":%d,"total_users":3,`+
		`"top_predictors":[{"user_display_name":"SENTINEL-PREDICTOR-51c0","points":100}]},`+
		`{"id":"synthetic-outcome-b","title":"SENTINEL-OUTCOME-TITLE-B","total_points":%d,"total_users":2}]%s}}}`,
		eventID, status, pointsA, pointsB, winnerField))
}

// canaryCreatedFrame is canaryUpdateFrame's frame announced as event-created.
func canaryCreatedFrame(eventID, status string) []byte {
	return bytes.Replace(canaryUpdateFrame(eventID, status, 300, 200, ""),
		[]byte(`"type":"event-updated"`), []byte(`"type":"event-created"`), 1)
}

// admitCanaryRound tracks a round the way the pool's admission path does, with
// a local incarnation, so event-updated frames reach its bookkeeping.
func admitCanaryRound(p *WebSocketPool, s *models.Streamer, eventID string) {
	addRoundWithOutcomes(p, s, eventID, "synthetic-outcome-a", "synthetic-outcome-b")
	p.mu.Lock()
	p.control[eventID].incarnation = p.newRoundIncarnation()
	p.mu.Unlock()
}

// canaryEventMsg is the envelope the classifier sees for a qualifying topic and
// message type.
func canaryEventMsg() *PubSubMessage {
	return &PubSubMessage{Topic: NewTopic(TopicPredictionsChannel, "synthetic-channel-4417"), Type: "event-updated"}
}

// canaryAbsentKey marks an event key that is left off the frame entirely.
type canaryAbsentKey struct{}

func isCanaryAbsentKey(v interface{}) bool {
	_, absent := v.(canaryAbsentKey)
	return absent
}

// canaryEvent builds a RESOLVED event object; canaryAbsentKey{} omits a key.
func canaryEvent(id, winner, outcomes interface{}) map[string]interface{} {
	ev := map[string]interface{}{"status": "RESOLVED", "title": "SENTINEL-TITLE-3b8f"}
	for key, v := range map[string]interface{}{"id": id, "winning_outcome_id": winner, "outcomes": outcomes} {
		if !isCanaryAbsentKey(v) {
			ev[key] = v
		}
	}
	return ev
}

// canaryOutcomes builds an outcomes array of objects carrying the given ids.
func canaryOutcomes(ids ...interface{}) []interface{} {
	out := make([]interface{}, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]interface{}{"id": id, "title": "SENTINEL-OUTCOME-TITLE"})
	}
	return out
}

func canaryPair() []interface{} {
	return canaryOutcomes("synthetic-outcome-a", "synthetic-outcome-b")
}

func canaryClassifiedFields(t *testing.T, rec winnerCanaryRecord) map[string]interface{} {
	t.Helper()
	return canaryFields(t, rec.attrs(time.Now()))
}

// canaryCloneMessage copies a message, deep-copying its JSON-shaped parts, so
// the copy can later show whether anything in the original changed.
func canaryCloneMessage(m *PubSubMessage) PubSubMessage {
	c := *m
	c.Data, _ = canaryCloneJSON(m.Data).(map[string]interface{})
	c.Message, _ = canaryCloneJSON(m.Message).(map[string]interface{})
	return c
}

// canaryStampConnection gives a parsed message the connection provenance a
// delivering connection stamps on it, so that every field of the message holds
// a value that overwriting it would change.
func canaryStampConnection(m *PubSubMessage) *PubSubMessage {
	m.ConnectionIndex, m.ConnectionGeneration, m.ConnectionSequence, m.ConnectionKnown = 2, 3, 7, true
	return m
}

// canaryCloneJSON deep-copies a JSON-shaped value.
func canaryCloneJSON(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		if x == nil {
			return x
		}
		out := make(map[string]interface{}, len(x))
		for k, e := range x {
			out[k] = canaryCloneJSON(e)
		}
		return out
	case []interface{}:
		if x == nil {
			return x
		}
		out := make([]interface{}, len(x))
		for i, e := range x {
			out[i] = canaryCloneJSON(e)
		}
		return out
	}
	return v
}

// --- Fixture table through the real handler entry -----------------------------

// TestWinnerCanaryFixturesThroughHandlePredictionChannel drives every fixture
// frame through the real predictions-channel handler, capturing every record at
// every level. A qualifying frame yields exactly one record of any kind — the
// canary's, matching the fixture's literal expectation — a non-candidate
// yields none, and no frame is modified. It counts every record the process
// logs, so it runs in a fresh process. The fixtures are loaded before that, in
// this process too, so go test's result cache notices when one changes.
func TestWinnerCanaryFixturesThroughHandlePredictionChannel(t *testing.T) {
	cases := loadWinnerCanaryCases(t)
	// The fixtures are the literal oracle; a silently shrunken table would
	// weaken every assertion below without failing any of them.
	records := 0
	for _, c := range cases {
		if *c.Record {
			records++
		}
	}
	if len(cases) != 66 || records != 43 {
		t.Fatalf("loaded %d fixture cases (%d with a record), want 66 (43)", len(cases), records)
	}
	if !canaryInFreshProcess(t) {
		return
	}
	capture := &canaryCapture{level: canaryCaptureAll}
	canaryUseProcessLogger(t, capture)

	for _, c := range cases {
		t.Run(c.file+"/"+c.Name, func(t *testing.T) {
			p := newTestPool(&fakePlacer{})
			s := canaryStreamer()
			p.streamers = []*models.Streamer{s}
			msg := parseCanaryFrame(t, c.Topic, c.Message)
			snapshot := canaryCloneMessage(msg)

			already := len(capture.all())
			before := time.Now()
			p.handlePredictionChannel(msg, s)
			after := time.Now()
			got := capture.all()[already:]

			if !reflect.DeepEqual(*msg, snapshot) {
				t.Fatalf("handling the frame modified it")
			}
			if !*c.Record {
				if len(got) != 0 {
					t.Fatalf("a frame that is not a canary candidate produced %d record(s), first %q", len(got), got[0].Message)
				}
				return
			}
			if len(got) != 1 || got[0].Message != "p4_winner_canary" || got[0].Level != slog.LevelInfo {
				t.Fatalf("a qualifying frame produced %d record(s), want exactly one INFO p4_winner_canary", len(got))
			}
			assertCanaryFields(t, canaryFields(t, canaryRecordAttrs(got[0])), c.want(t), version.Version, before, after)
			assertCanaryPrivate(t, renderCanary(t, got[0]))
		})
	}
}

// --- Classifier seam: bounds, byte counting and non-JSON values ---------------

func TestWinnerCanaryResourceAdmission(t *testing.T) {
	long := func(n int) string { return strings.Repeat("x", n) }
	e := func(n int) string { return strings.Repeat("\u00e9", n) } // two bytes per rune
	ids64 := make([]interface{}, 64)
	for i := range ids64 {
		ids64[i] = fmt.Sprintf("synthetic-outcome-%02d", i)
	}
	ids65 := append(append([]interface{}{}, ids64...), "synthetic-outcome-64")
	positiveAt := func(index int64) *canaryFrameAttrs {
		a := canaryPositive0001
		a.winnerIndex = index
		return &a
	}

	for _, tc := range []struct {
		name  string
		event map[string]interface{}
		build string
		want  *canaryFrameAttrs // nil: the attempt is refused and leaves no record
	}{
		{
			name:  "64 outcomes are admitted and the last one can win",
			event: canaryEvent("synthetic-event-0064", "synthetic-outcome-63", canaryOutcomes(ids64...)),
			want: &canaryFrameAttrs{winnerPresence: "PRESENT", winnerJSONType: "string", outcomesPresence: "PRESENT",
				outcomeCount: 64, outcomeIDsValid: true, winnerMatchCount: 1, winnerIndex: 63,
				eventKeyHash: canaryHash0064, result: canaryObserved},
		},
		{name: "65 outcomes refuse the attempt",
			event: canaryEvent("synthetic-event-0064", "synthetic-outcome-63", canaryOutcomes(ids65...))},
		{name: "65 outcomes refuse even a frame that would be malformed",
			event: canaryEvent(canaryAbsentKey{}, canaryAbsentKey{}, canaryOutcomes(ids65...))},
		{name: "65 outcomes refuse a frame whose winner is null",
			event: canaryEvent("synthetic-event-0064", nil, canaryOutcomes(ids65...))},
		{
			name:  "a 4096-byte event id is admitted and hashed",
			event: canaryEvent(long(4096), "synthetic-outcome-b", canaryPair()),
			want: &canaryFrameAttrs{winnerPresence: "PRESENT", winnerJSONType: "string", outcomesPresence: "PRESENT",
				outcomeCount: 2, outcomeIDsValid: true, winnerMatchCount: 1, winnerIndex: 1,
				eventKeyHash: canaryHashX4K, result: canaryObserved},
		},
		{name: "a 4097-byte event id refuses the attempt",
			event: canaryEvent(long(4097), "synthetic-outcome-b", canaryPair())},
		{name: "a 4097-byte event id refuses a frame whose winner is absent",
			event: canaryEvent(long(4097), canaryAbsentKey{}, canaryPair())},
		{
			name:  "bytes, not runes: a 2048-rune, 4096-byte event id is admitted",
			event: canaryEvent(e(2048), "synthetic-outcome-b", canaryPair()),
			want: &canaryFrameAttrs{winnerPresence: "PRESENT", winnerJSONType: "string", outcomesPresence: "PRESENT",
				outcomeCount: 2, outcomeIDsValid: true, winnerMatchCount: 1, winnerIndex: 1,
				eventKeyHash: canaryHashE4K, result: canaryObserved},
		},
		{name: "bytes, not runes: a 2049-rune, 4098-byte event id is refused",
			event: canaryEvent(e(2049), "synthetic-outcome-b", canaryPair())},
		{name: "a 4096-byte winner matching a 4096-byte outcome id is admitted",
			event: canaryEvent("synthetic-event-0001", long(4096), canaryOutcomes("synthetic-outcome-a", long(4096))),
			want:  positiveAt(1)},
		{name: "a 4097-byte winner refuses the attempt",
			event: canaryEvent("synthetic-event-0001", long(4097), canaryPair())},
		{name: "a 4097-byte winner refuses a frame whose outcomes are absent",
			event: canaryEvent("synthetic-event-0001", long(4097), canaryAbsentKey{})},
		{name: "bytes, not runes: a 2048-rune winner matching its outcome is admitted",
			event: canaryEvent("synthetic-event-0001", e(2048), canaryOutcomes("synthetic-outcome-a", e(2048))),
			want:  positiveAt(1)},
		{name: "bytes, not runes: a 2049-rune winner is refused",
			event: canaryEvent("synthetic-event-0001", e(2049), canaryPair())},
		{name: "resource refusal outranks the malformed event id",
			event: canaryEvent(canaryAbsentKey{}, long(4097), canaryPair())},
		{name: "a 4097-byte outcome id refuses the attempt",
			event: canaryEvent("synthetic-event-0001", "synthetic-outcome-a", canaryOutcomes("synthetic-outcome-a", long(4097)))},
		{name: "a 4097-byte outcome id refuses a frame whose event id is missing",
			event: canaryEvent(canaryAbsentKey{}, "synthetic-outcome-a", canaryOutcomes("synthetic-outcome-a", long(4097)))},
		{name: "bytes, not runes: a 2048-rune outcome id is admitted",
			event: canaryEvent("synthetic-event-0001", "synthetic-outcome-a", canaryOutcomes("synthetic-outcome-a", e(2048))),
			want:  positiveAt(0)},
		{name: "bytes, not runes: a 2049-rune outcome id is refused",
			event: canaryEvent("synthetic-event-0001", "synthetic-outcome-a", canaryOutcomes("synthetic-outcome-a", e(2049)))},
		{
			name: "a 4097-byte id in the last of 64 outcomes refuses an otherwise invalid vector",
			event: canaryEvent("synthetic-event-0001", canaryAbsentKey{},
				append(append([]interface{}{"not-an-object"}, canaryOutcomes(ids64[1:63]...)...),
					map[string]interface{}{"id": long(4097)})),
		},
		{name: "a 4097-byte id in the first of two outcomes refuses the attempt",
			event: canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryOutcomes(long(4097), "synthetic-outcome-b"))},
		{name: "a 4097-byte id in the middle of 64 outcomes refuses the attempt",
			event: canaryEvent("synthetic-event-0064", "synthetic-outcome-63",
				canaryOutcomes(append(append(append([]interface{}{}, ids64[:31]...), long(4097)), ids64[32:]...)...))},
		{name: "a 4096-byte build label is admitted",
			event: canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair()),
			build: long(4096), want: positiveAt(1)},
		{name: "a 4097-byte build label refuses the attempt",
			event: canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair()),
			build: long(4097)},
		{name: "bytes, not runes: a 2048-rune build label is admitted",
			event: canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair()),
			build: e(2048), want: positiveAt(1)},
		{name: "bytes, not runes: a 2049-rune build label is refused",
			event: canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair()),
			build: e(2049)},
		{
			name: "fields the canary never inspects are not bounded by it",
			event: func() map[string]interface{} {
				ev := canaryEvent("synthetic-event-0001", "synthetic-outcome-b", []interface{}{
					map[string]interface{}{"id": "synthetic-outcome-a", "title": long(1 << 20),
						"top_predictors": []interface{}{map[string]interface{}{"user_display_name": long(1 << 20)}}},
					map[string]interface{}{"id": "synthetic-outcome-b"},
				})
				ev["title"] = long(1 << 20)
				ev["SENTINEL_UNKNOWN_KEY"] = long(1 << 20)
				return ev
			}(),
			want: positiveAt(1),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build := tc.build
			if build == "" {
				build = "synthetic-build"
			}
			rec, ok := classifyWinnerCanary(canaryEventMsg(), "RESOLVED", tc.event, build)
			if tc.want == nil {
				if ok {
					t.Fatalf("an over-bound frame was admitted: %+v", canaryClassifiedFields(t, rec))
				}
				return
			}
			if !ok {
				t.Fatal("an in-bound frame was refused")
			}
			assertCanaryFields(t, canaryClassifiedFields(t, rec), tc.want.values(), build, time.Now().Add(-time.Minute), time.Now())
		})
	}
}

// TestWinnerCanaryRefusalTouchesNothing refuses frames far beyond every bound.
// Refusal must come first: a refused frame is neither copied nor hashed (no
// allocation at all), and no id in it is compared. Before admission, the
// pairwise id check over 8192 outcomes (33 million comparisons) or comparing a
// 4 MiB winner with 4096 same-length ids (16 GiB scanned) would take seconds
// for each call; refusing all 21 calls takes microseconds. A lighter scan of an
// over-long list is TestWinnerCanaryRefusesAnOverlongOutcomeListUnread's to
// catch.
func TestWinnerCanaryRefusalTouchesNothing(t *testing.T) {
	huge := strings.Repeat("x", 1<<20)
	bigWinner := strings.Repeat("x", 4<<20)
	bigOther := strings.Repeat("x", 4<<20-1) + "y" // same length, differs only at the end
	distinct := make([]interface{}, 1<<13)
	for i := range distinct {
		distinct[i] = map[string]interface{}{"id": "synthetic-outcome-" + strconv.Itoa(i)}
	}
	sameLength := make([]interface{}, 1<<12)
	for i := range sameLength {
		sameLength[i] = map[string]interface{}{"id": bigOther}
	}
	msg := canaryEventMsg()

	for _, tc := range []struct {
		name  string
		event map[string]interface{}
		build string
	}{
		{"a 1 MiB event id", canaryEvent(huge, "synthetic-outcome-b", canaryPair()), "synthetic-build"},
		{"a 1 MiB winner", canaryEvent("synthetic-event-0001", huge, canaryPair()), "synthetic-build"},
		{"a 1 MiB id in the last of 64 outcomes", canaryEvent("synthetic-event-0001", "synthetic-outcome-a",
			append(canaryOutcomes(make([]interface{}, 63)...), map[string]interface{}{"id": huge})), "synthetic-build"},
		{"8192 distinct outcome ids", canaryEvent("synthetic-event-0001", "synthetic-outcome-1", distinct), "synthetic-build"},
		{"a 4 MiB winner beside 4096 same-length outcome ids", canaryEvent("synthetic-event-0001", bigWinner, sameLength), "synthetic-build"},
		{"a 1 MiB build label", canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair()), huge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := classifyWinnerCanary(msg, "RESOLVED", tc.event, tc.build); ok {
				t.Fatal("an over-bound frame was admitted")
			}
			// AllocsPerRun makes 21 calls; refusing up front costs microseconds
			// for all of them together.
			started := time.Now()
			allocs := testing.AllocsPerRun(20, func() { classifyWinnerCanary(msg, "RESOLVED", tc.event, tc.build) })
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("21 refusals took %v: the frame was traversed or compared before admission", elapsed)
			}
			if allocs != 0 {
				t.Fatalf("refusing the frame allocated %v times per call: something was copied or hashed before admission", allocs)
			}
		})
	}
}

// TestWinnerCanaryRefusesAnOverlongOutcomeListUnread refuses a list beyond the
// 64-outcome bound on its length alone. Every outcome is an ordinary in-bound
// object, so only looking at the list costs anything: 21 refusals must take
// less than one pass that merely checks each outcome's type, the least any scan
// before admission would do. Both sides take the best of several runs, so a
// descheduled run cannot fail the test while a scan slows every run.
func TestWinnerCanaryRefusesAnOverlongOutcomeListUnread(t *testing.T) {
	outcome := map[string]interface{}{"id": "synthetic-outcome-a"}
	outcomes := make([]interface{}, 1<<20)
	for i := range outcomes {
		outcomes[i] = outcome
	}
	event := canaryEvent("synthetic-event-0001", "synthetic-outcome-a", outcomes)
	msg := canaryEventMsg()

	var onePass time.Duration
	for run := 0; run < 3; run++ {
		started := time.Now()
		objects := 0
		for _, o := range outcomes {
			if _, ok := o.(map[string]interface{}); ok {
				objects++
			}
		}
		elapsed := time.Since(started)
		if objects != len(outcomes) {
			t.Fatalf("found %d objects, want %d", objects, len(outcomes))
		}
		if run == 0 || elapsed < onePass {
			onePass = elapsed
		}
	}

	var refusals time.Duration
	for run := 0; run < 5; run++ {
		started := time.Now()
		for i := 0; i < 21; i++ {
			if _, ok := classifyWinnerCanary(msg, "RESOLVED", event, "synthetic-build"); ok {
				t.Fatal("an over-bound outcome list was admitted")
			}
		}
		elapsed := time.Since(started)
		if run == 0 || elapsed < refusals {
			refusals = elapsed
		}
	}
	if refusals >= onePass {
		t.Fatalf("21 refusals took %v, not less than one pass checking every outcome's type (%v): the list was read before admission", refusals, onePass)
	}
}

// TestWinnerCanaryRefusesOverlongIDsUncompared refuses 64 outcomes — within the
// outcome bound — whose ids are 256 KiB each, beyond the byte bound, all the
// same length and different only at the end, so that comparing two of them
// scans them whole. Lengths come before any comparison, so 21 refusals must
// take less than one pass comparing each id with the next, the least a
// pairwise check before admission would do. Both sides take the best of
// several runs.
func TestWinnerCanaryRefusesOverlongIDsUncompared(t *testing.T) {
	prefix := strings.Repeat("x", 256<<10-2)
	ids := make([]string, 64)
	outcomes := make([]interface{}, len(ids))
	for i := range ids {
		ids[i] = prefix + fmt.Sprintf("%02d", i)
		outcomes[i] = map[string]interface{}{"id": ids[i]}
	}
	event := canaryEvent("synthetic-event-0001", "synthetic-outcome-a", outcomes)
	msg := canaryEventMsg()

	var onePass time.Duration
	for run := 0; run < 3; run++ {
		started := time.Now()
		equal := 0
		for i := 1; i < len(ids); i++ {
			if ids[i] == ids[i-1] {
				equal++
			}
		}
		elapsed := time.Since(started)
		if equal != 0 {
			t.Fatalf("control: %d neighbouring ids are equal, want none", equal)
		}
		if run == 0 || elapsed < onePass {
			onePass = elapsed
		}
	}

	var refusals time.Duration
	for run := 0; run < 5; run++ {
		started := time.Now()
		for i := 0; i < 21; i++ {
			if _, ok := classifyWinnerCanary(msg, "RESOLVED", event, "synthetic-build"); ok {
				t.Fatal("outcome ids beyond the byte bound were admitted")
			}
		}
		elapsed := time.Since(started)
		if run == 0 || elapsed < refusals {
			refusals = elapsed
		}
	}
	if refusals >= onePass {
		t.Fatalf("21 refusals took %v, not less than one pass comparing neighbouring ids (%v): ids were compared before their lengths were checked", refusals, onePass)
	}
}

// canaryKindRecords holds the record of each kind of frame the timing tests
// build, from the contract's field rules: the positive record,
// canaryPositive0001, with the fields that kind changes. All carry
// synthetic-event-0001's hash, except noEventID, which has none.
type canaryKindRecords struct {
	positive, firstMatched, noWinner, nullWinner, emptyWinner, unmatched canaryFrameAttrs
	noEventID, invalidVector, noOutcomes, nullOutcomes, invalidOutcomes  canaryFrameAttrs
}

func newCanaryKindRecords() canaryKindRecords {
	k := canaryKindRecords{positive: canaryPositive0001}
	k.firstMatched = k.positive
	k.firstMatched.winnerIndex = 0
	k.noWinner = k.positive
	k.noWinner.winnerPresence, k.noWinner.winnerJSONType = "ABSENT_ON_WIRE", "absent"
	k.noWinner.winnerMatchCount, k.noWinner.winnerIndex, k.noWinner.result = -1, -1, canaryNotObserved
	k.nullWinner = k.noWinner
	k.nullWinner.winnerPresence, k.nullWinner.winnerJSONType = "NULL_ON_WIRE", "null"
	k.emptyWinner = k.positive
	k.emptyWinner.winnerValueEmpty = true
	k.emptyWinner.winnerMatchCount, k.emptyWinner.winnerIndex, k.emptyWinner.result = -1, -1, canaryMalformed
	k.unmatched = k.positive
	k.unmatched.winnerMatchCount, k.unmatched.winnerIndex, k.unmatched.result = 0, -1, canaryMalformed
	k.noEventID = k.positive
	k.noEventID.eventKeyHash, k.noEventID.result = "none", canaryMalformed
	k.invalidVector = k.positive
	k.invalidVector.outcomeIDsValid = false
	k.invalidVector.winnerMatchCount, k.invalidVector.winnerIndex, k.invalidVector.result = -1, -1, canaryMalformed
	k.noOutcomes = k.invalidVector
	k.noOutcomes.outcomesPresence, k.noOutcomes.outcomeCount = "ABSENT_ON_WIRE", -1
	k.nullOutcomes = k.noOutcomes
	k.nullOutcomes.outcomesPresence = "NULL_ON_WIRE"
	k.invalidOutcomes = k.noOutcomes
	k.invalidOutcomes.outcomesPresence = "INVALID"
	return k
}

// invalidWinner is the record of a winner of the given JSON type other than
// string or null.
func (k canaryKindRecords) invalidWinner(jsonType string) canaryFrameAttrs {
	w := k.positive
	w.winnerPresence, w.winnerJSONType = "INVALID", jsonType
	w.winnerMatchCount, w.winnerIndex, w.result = -1, -1, canaryMalformed
	return w
}

// canarySet sets key to v in m, or removes key when v is canaryAbsentKey{}.
func canarySet(m map[string]interface{}, key string, v interface{}) map[string]interface{} {
	if isCanaryAbsentKey(v) {
		delete(m, key)
	} else {
		m[key] = v
	}
	return m
}

// canaryFrame is one frame as the pool hands it to the canary.
type canaryFrame struct {
	msg    *PubSubMessage
	status string
	event  map[string]interface{}
}

// canaryParts supplies what a frame holds besides the values the canary reads,
// so that one table of frame kinds serves oracles that make those parts large
// and one that has other goroutines write into them.
type canaryParts struct {
	// inspected completes an object the canary reads by fixed key: the event
	// or an outcome.
	inspected func(map[string]interface{}) map[string]interface{}
	// object and array stand where the winner, the event id, the outcomes, an
	// outcome or an outcome's id belongs.
	object func(map[string]interface{}) map[string]interface{}
	array  func() []interface{}
	// predictors and nest are an outcome's predictors and nested object, and
	// envelope completes the message's data envelope and raw message.
	predictors func() []interface{}
	nest       func() map[string]interface{}
	envelope   func(map[string]interface{}) map[string]interface{}
}

// canaryFrameKind is one kind of frame: how to build one, and the record the
// canary must make of it, or that it must make none.
type canaryFrameKind struct {
	name  string
	build func(p canaryParts, n int) canaryFrame
	want  canaryFrameAttrs
	none  bool // not a candidate, or refused by a resource bound
}

// record is the record the canary must make of a frame of this kind, or nil
// when it must make none.
func (k canaryFrameKind) record() map[string]interface{} {
	if k.none {
		return nil
	}
	return k.want.values()
}

// canaryFrameKinds lists a frame of every kind the canary tells apart: one for
// each record it can make, one for each way a frame is not a candidate and one
// for each bound that refuses a frame. Frame n has its own ids —
// synthetic-event-000n and its outcomes' — so frames built for different n
// share none; the records carry the hash of synthetic-event-0001, the event id
// of n = 1.
func canaryFrameKinds() []canaryFrameKind {
	records := newCanaryKindRecords()
	absent := canaryAbsentKey{}
	id := func(n int) string { return fmt.Sprintf("synthetic-event-%04d", n) }
	a := func(n int) string { return fmt.Sprintf("synthetic-outcome-a-%04d", n) }
	b := func(n int) string { return fmt.Sprintf("synthetic-outcome-b-%04d", n) }
	// overlong is 4101 bytes, beyond the 4096-byte bound.
	overlong := func(n int) string { return fmt.Sprintf("%s-%04d", strings.Repeat("x", 4096), n) }
	outcome := func(p canaryParts, outcomeID interface{}) map[string]interface{} {
		return p.inspected(canarySet(map[string]interface{}{"top_predictors": p.predictors(), "SENTINEL_NEST": p.nest()}, "id", outcomeID))
	}
	// pair is two outcomes: the first with firstID (none for absent), the
	// second naming b(n).
	pair := func(p canaryParts, n int, firstID interface{}) []interface{} {
		return []interface{}{outcome(p, firstID), outcome(p, b(n))}
	}
	// frame is a qualifying frame of a RESOLVED event with the given id,
	// winner and outcomes.
	frame := func(p canaryParts, eventID, winner, outcomes interface{}) canaryFrame {
		event := p.inspected(canaryEvent(eventID, winner, outcomes))
		msg := canaryEventMsg()
		msg.Data = p.envelope(map[string]interface{}{"event": event})
		msg.Message = p.envelope(map[string]interface{}{"type": "MESSAGE"})
		return canaryFrame{msg: msg, status: "RESOLVED", event: event}
	}
	// positive is frame n of the positive kind, before its kind changes it.
	positive := func(p canaryParts, n int) canaryFrame { return frame(p, id(n), b(n), pair(p, n, a(n))) }
	// winner and eventID vary one value of the positive frame; outcomes
	// replaces its outcomes, and first its first outcome's id.
	winner := func(v func(p canaryParts, n int) interface{}) func(canaryParts, int) canaryFrame {
		return func(p canaryParts, n int) canaryFrame { return frame(p, id(n), v(p, n), pair(p, n, a(n))) }
	}
	eventID := func(v func(p canaryParts, n int) interface{}) func(canaryParts, int) canaryFrame {
		return func(p canaryParts, n int) canaryFrame { return frame(p, v(p, n), b(n), pair(p, n, a(n))) }
	}
	outcomes := func(v func(p canaryParts, n int) interface{}) func(canaryParts, int) canaryFrame {
		return func(p canaryParts, n int) canaryFrame { return frame(p, id(n), b(n), v(p, n)) }
	}
	first := func(v func(p canaryParts, n int) interface{}) func(canaryParts, int) canaryFrame {
		return outcomes(func(p canaryParts, n int) interface{} { return pair(p, n, v(p, n)) })
	}
	value := func(v interface{}) func(canaryParts, int) interface{} {
		return func(canaryParts, int) interface{} { return v }
	}
	of := func(v func(n int) string) func(canaryParts, int) interface{} {
		return func(_ canaryParts, n int) interface{} { return v(n) }
	}
	z := func(n int) string { return fmt.Sprintf("synthetic-outcome-z-%04d", n) }
	number := func(_ canaryParts, n int) interface{} { return float64(n) }
	array := func(p canaryParts, _ int) interface{} { return p.array() }
	// changed is the positive frame with one change made to it.
	changed := func(change func(f *canaryFrame)) func(canaryParts, int) canaryFrame {
		return func(p canaryParts, n int) canaryFrame {
			f := positive(p, n)
			change(&f)
			return f
		}
	}
	return []canaryFrameKind{
		{name: "positive", want: records.positive, build: positive},
		{name: "positive, the first outcome matched", want: records.firstMatched, build: winner(of(a))},
		{name: "no winner", want: records.noWinner, build: winner(value(absent))},
		{name: "null winner", want: records.nullWinner, build: winner(value(nil))},
		{name: "numeric winner", want: records.invalidWinner("number"), build: winner(number)},
		{name: "boolean winner", want: records.invalidWinner("bool"), build: winner(value(true))},
		{name: "empty winner", want: records.emptyWinner, build: winner(value(""))},
		{name: "winner naming no outcome", want: records.unmatched, build: winner(of(z))},
		{name: "object winner", want: records.invalidWinner("object"),
			build: winner(func(p canaryParts, n int) interface{} { return p.object(map[string]interface{}{"id": b(n)}) })},
		{name: "array winner", want: records.invalidWinner("array"), build: winner(array)},
		{name: "no event id", want: records.noEventID, build: eventID(value(absent))},
		{name: "null event id", want: records.noEventID, build: eventID(value(nil))},
		{name: "numeric event id", want: records.noEventID, build: eventID(number)},
		{name: "empty event id", want: records.noEventID, build: eventID(value(""))},
		{name: "object event id", want: records.noEventID,
			build: eventID(func(p canaryParts, n int) interface{} { return p.object(map[string]interface{}{"id": id(n)}) })},
		{name: "array event id", want: records.noEventID, build: eventID(array)},
		{name: "no outcomes", want: records.noOutcomes, build: outcomes(value(absent))},
		{name: "null outcomes", want: records.nullOutcomes, build: outcomes(value(nil))},
		{name: "object for the outcomes", want: records.invalidOutcomes, build: outcomes(func(p canaryParts, n int) interface{} {
			return p.object(map[string]interface{}{"0": map[string]interface{}{"id": a(n)}})
		})},
		{name: "duplicate outcome id", want: records.invalidVector, build: first(of(b))},
		{name: "outcome that is an array", want: records.invalidVector, build: outcomes(func(p canaryParts, n int) interface{} {
			return []interface{}{p.array(), outcome(p, b(n))}
		})},
		{name: "outcome with no id", want: records.invalidVector, build: first(value(absent))},
		{name: "outcome whose id is empty", want: records.invalidVector, build: first(value(""))},
		{name: "outcome whose id is a number", want: records.invalidVector, build: first(number)},
		{name: "outcome whose id is an object", want: records.invalidVector,
			build: first(func(p canaryParts, _ int) interface{} { return p.object(map[string]interface{}{}) })},
		{name: "outcome whose id is an array", want: records.invalidVector, build: first(array)},
		{name: "another topic", none: true, build: changed(func(f *canaryFrame) {
			f.msg.Topic = NewTopic(TopicPredictionsUser, "synthetic-channel-4417")
		})},
		{name: "another message type", none: true, build: changed(func(f *canaryFrame) { f.msg.Type = "event-created" })},
		{name: "an unresolved round", none: true, build: changed(func(f *canaryFrame) {
			f.status = "ACTIVE"
			f.event["status"] = "ACTIVE"
		})},
		{name: "no event object", none: true, build: changed(func(f *canaryFrame) { f.event = nil })},
		{name: "no message", none: true, build: changed(func(f *canaryFrame) { f.msg = nil })},
		{name: "an overlong event id", none: true, build: eventID(of(overlong))},
		{name: "an overlong winner", none: true, build: winner(of(overlong))},
		{name: "an overlong outcome id", none: true, build: first(of(overlong))},
		{name: "more than 64 outcomes", none: true, build: outcomes(func(p canaryParts, n int) interface{} {
			return slices.Repeat([]interface{}{outcome(p, a(n))}, 65)
		})},
	}
}

// canaryAssertClassified checks the classifier's answer for a frame: the
// record want, or, for a nil want, a refusal. A classified record has no
// receive time of its own, so it is given one here.
func canaryAssertClassified(t *testing.T, want map[string]interface{}, rec winnerCanaryRecord, ok bool) {
	t.Helper()
	switch {
	case ok && want == nil:
		t.Fatal("the frame was admitted")
	case !ok && want != nil:
		t.Fatal("the frame was refused")
	case ok:
		now := time.Now()
		assertCanaryFields(t, canaryFields(t, rec.attrs(now)), want, "synthetic-build", now, now)
	}
}

// canaryAssertEmitted checks what was logged during count emissions of a
// frame: count INFO p4_winner_canary records, each the record want, or, for a
// nil want, nothing at all.
func canaryAssertEmitted(t *testing.T, want map[string]interface{}, logged []slog.Record, count int, before, after time.Time) {
	t.Helper()
	if want == nil {
		count = 0
	}
	if len(logged) != count {
		t.Fatalf("the emitter logged %d record(s), want %d", len(logged), count)
	}
	for _, r := range logged {
		if r.Message != "p4_winner_canary" || r.Level != slog.LevelInfo {
			t.Fatalf("the emitter logged %s %q, want INFO p4_winner_canary", r.Level, r.Message)
		}
		assertCanaryFields(t, canaryFields(t, canaryRecordAttrs(r)), want, version.Version, before, after)
	}
}

// canaryBestOf times runs calls of f and returns the fastest, so that a run
// the scheduler or the collector slowed does not decide a comparison.
func canaryBestOf(runs int, f func()) time.Duration {
	var best time.Duration
	for run := 0; run < runs; run++ {
		started := time.Now()
		f()
		if elapsed := time.Since(started); run == 0 || elapsed < best {
			best = elapsed
		}
	}
	return best
}

// canaryWalk returns a timing of one plain walk over m's keys, which must
// number at least want.
func canaryWalk(t *testing.T, m map[string]interface{}, want int) func() {
	return func() {
		visited := 0
		for range m {
			visited++
		}
		if visited < want {
			t.Fatalf("the walk visited %d keys, want at least %d", visited, want)
		}
	}
}

// TestWinnerCanaryReadsFramesByFixedKey hands the canary one frame of every
// kind canaryFrameKinds lists — each record it can make, each way a frame is
// not a candidate and each bound that refuses one — through the classifier and
// through the emitter the pool calls. Each frame is large wherever the canary
// has no reason to read: 65,536 extra keys on the event, in every outcome
// object, in the data envelope and raw message and in every object standing
// where the winner, the event id, the outcomes or an outcome's id belongs;
// 1,048,576 elements in every array standing there and in every outcome's
// predictors; 65,536 levels of nesting in every outcome. A walk hidden in any
// branch therefore meets a large value there. The canary reads fixed keys
// only, so ten classifications, and ten emissions, must each take less than
// one plain walk over the extra keys, the least any enumeration, traversal or
// recursion would do. Timing sees walks over thousands of entries; one over a
// few hundred costs about as much as a call and escapes it. Every timing is
// the best of several runs, and each subtest logs them (go test -v). Each
// frame's record, classified and emitted, is first checked against its kind's
// literal expectation, or that there is none, so a case that stopped reaching
// its branch, or a record that depended on what the canary must not read,
// fails rather than passing on the timing of another kind of frame. This test
// times repeated calls on one frame; TestWinnerCanaryReadsFreshFramesByFixedKey
// times first ones.
func TestWinnerCanaryReadsFramesByFixedKey(t *testing.T) {
	capture := &canaryCapture{level: canaryCaptureAll}
	canaryUseProcessLogger(t, capture) // its t.Setenv also refuses a parallel test
	const extra = 1 << 16
	extraKeys := make(map[string]interface{}, extra)
	for i := 0; i < extra; i++ {
		extraKeys["SENTINEL_KEY_"+strconv.Itoa(i)] = float64(i)
	}
	// widen gives an object a copy of the extra keys of its own.
	widen := func(m map[string]interface{}) map[string]interface{} {
		wide := maps.Clone(extraKeys)
		maps.Copy(wide, m)
		return wide
	}
	big := slices.Repeat([]interface{}{true}, 1<<20)
	deep := map[string]interface{}{}
	for level, m := 0, deep; level < extra; level++ {
		next := map[string]interface{}{}
		m["SENTINEL_NEST"] = next
		m = next
	}
	parts := canaryParts{
		inspected:  widen,
		object:     widen,
		array:      func() []interface{} { return big },
		predictors: func() []interface{} { return big },
		nest:       func() map[string]interface{} { return deep },
		envelope:   widen,
	}
	for _, kind := range canaryFrameKinds() {
		t.Run(kind.name, func(t *testing.T) {
			f := kind.build(parts, 1)
			want := kind.record()
			rec, ok := classifyWinnerCanary(f.msg, f.status, f.event, "synthetic-build")
			canaryAssertClassified(t, want, rec, ok)
			before := time.Now()
			logWinnerCanary(f.msg, f.status, f.event)
			// Only the canary's records count here: in the main test process, a
			// client another test left behind can log at any time.
			logged := slices.DeleteFunc(capture.take(), func(r slog.Record) bool { return r.Message != "p4_winner_canary" })
			canaryAssertEmitted(t, want, logged, 1, before, time.Now())
			runtime.GC()
			onePass := canaryBestOf(3, canaryWalk(t, extraKeys, extra))
			classifying := canaryBestOf(5, func() {
				for i := 0; i < 10; i++ {
					classifyWinnerCanary(f.msg, f.status, f.event, "synthetic-build")
				}
			})
			emitting := canaryBestOf(5, func() {
				for i := 0; i < 10; i++ {
					logWinnerCanary(f.msg, f.status, f.event)
				}
			})
			capture.take()
			t.Logf("ten classifications %v, ten emissions %v, one walk %v (%.0fx, %.0fx)", classifying, emitting, onePass,
				float64(onePass)/float64(classifying), float64(onePass)/float64(emitting))
			if classifying >= onePass {
				t.Errorf("ten classifications took %v, not less than one walk over the extra keys (%v): the frame was enumerated or walked", classifying, onePass)
			}
			if emitting >= onePass {
				t.Errorf("ten emissions took %v, not less than one walk over the extra keys (%v): the frame was enumerated or walked", emitting, onePass)
			}
		})
	}
}

// canaryFreshTimings is what one fresh process of
// TestWinnerCanaryReadsFreshFramesByFixedKey reports: its walk over a frame's
// extra keys, its first call and then one call per kind, in the order
// canaryFrameKinds lists them.
type canaryFreshTimings struct {
	Walk, First time.Duration
	Calls       []time.Duration
}

// TestWinnerCanaryReadsFreshFramesByFixedKey times first calls. A canary that
// walked a frame once and then remembered doing so — by the address of a map
// in it, the message, an id, the winner, the frame's size, the branch the
// frame takes, or only that it had walked at all — would hide that walk behind
// any earlier call, and TestWinnerCanaryReadsFramesByFixedKey times repeated
// calls. Here the body runs in five fresh processes. Each builds one fresh
// frame of every kind canaryFrameKinds lists — its own message, maps, ids,
// winner and size, with extra keys on the event, in every outcome and in the
// case's object, and the case's array large too — and one more positive frame
// with eight times the extra keys. After a record of the test's own warms the
// logger (not the canary), the heap is collected and one plain walk over a
// separate map of the extra keys is timed, each process times the emitter the
// pool calls, once per frame: first on the wider frame, then on one frame of
// every kind in a fixed order, so the first call of the process, of every
// branch and on every frame is timed. For every kind, the fastest of its five
// calls must take less than a quarter of one walk: a looser bound than
// repeated calls get, since one call is timed alone, yet well below the walk a
// remembering canary makes. The first call of a process also runs cold code,
// so its fastest need only take less than two walks, while walking its frame
// would take eight. An outcome's predictors and nested object and the message's
// envelope stay small here; TestWinnerCanaryLeavesUninspectedValuesUnread
// watches them in every kind of frame. Each process checks every record
// against its kind's literal expectation, with the hash of the frame's own
// event id, or that there is none, and reports its timings; each kind then
// reports as a subtest. With P4_WINNER_CANARY_TEST_ISOLATED set to this test's
// name, the body runs once in-process and go test -v shows its timings,
// unjudged.
func TestWinnerCanaryReadsFreshFramesByFixedKey(t *testing.T) {
	const processes = 5
	kinds := canaryFrameKinds()
	if !canaryIsFreshChild(t) {
		dir := t.TempDir()
		runs := make([]canaryFreshTimings, processes)
		for p := range runs {
			path := filepath.Join(dir, strconv.Itoa(p)+".json")
			canaryInFreshProcess(t, canaryTimingsEnv+"="+path)
			raw, err := os.ReadFile(path)
			if err == nil {
				err = json.Unmarshal(raw, &runs[p])
			}
			if err != nil || len(runs[p].Calls) != len(kinds) {
				t.Fatalf("fresh process %d reported no timings: %v", p, err)
			}
		}
		// best is the largest ratio of a process's walk to one of its calls:
		// the fastest of the calls, each against its own process's walk.
		best := func(call func(canaryFreshTimings) time.Duration) float64 {
			ratio := 0.0
			for _, run := range runs {
				ratio = max(ratio, float64(run.Walk)/float64(call(run)))
			}
			return ratio
		}
		first := best(func(run canaryFreshTimings) time.Duration { return run.First })
		t.Logf("fastest first call of %d fresh processes: one walk takes %.1fx as long", processes, first)
		if first <= 0.5 {
			t.Errorf("the fastest first call of a fresh process took %.2f walks over a frame's extra keys, not less than two: the first frame of a process was walked", 1/first)
		}
		for k, kind := range kinds {
			t.Run(kind.name, func(t *testing.T) {
				ratio := best(func(run canaryFreshTimings) time.Duration { return run.Calls[k] })
				t.Logf("fastest first call in %d fresh processes: one walk takes %.0fx as long", processes, ratio)
				if ratio <= 4 {
					t.Errorf("the fastest first call on a fresh frame took %.2f of one walk over a frame's extra keys, not less than a quarter: fresh frames were walked", 1/ratio)
				}
			})
		}
		return
	}
	// 12,288 extra keys still fit the map size 8,192 would need (a Go map
	// fills to 7/8 of its slots), so the longer walk costs no memory. The
	// frames share the extra key strings and values, not a map, id, message or
	// size. Frame n takes the first extra+n of them, and the wider frame eight
	// times the extra keys.
	const extra = 3 << 12
	keys := make([]string, 8*extra)
	values := make([]interface{}, len(keys))
	for i := range keys {
		keys[i], values[i] = "SENTINEL_KEY_"+strconv.Itoa(i), float64(i)
	}
	// widen returns a map of m's keys and the first width extra keys, sized
	// for them up front.
	widen := func(m map[string]interface{}, width int) map[string]interface{} {
		wide := make(map[string]interface{}, len(m)+width)
		maps.Copy(wide, m)
		for i := 0; i < width; i++ {
			wide[keys[i]] = values[i]
		}
		return wide
	}
	// parts gives every object of frame n the first width extra keys, and
	// every array of it 196,608+n elements.
	parts := func(n, width int) canaryParts {
		wideObject := func(m map[string]interface{}) map[string]interface{} { return widen(m, width) }
		return canaryParts{
			inspected:  wideObject,
			object:     wideObject,
			array:      func() []interface{} { return slices.Repeat([]interface{}{true}, 16*extra+n) },
			predictors: func() []interface{} { return []interface{}{map[string]interface{}{"points": 500.0}} },
			nest:       func() map[string]interface{} { return map[string]interface{}{} },
			envelope:   func(m map[string]interface{}) map[string]interface{} { return m },
		}
	}
	// The opening frame is of the first kind, which must be the positive one.
	if kinds[0].none || kinds[0].want != newCanaryKindRecords().positive {
		t.Fatalf("the first kind is %q, want the positive one", kinds[0].name)
	}
	// Every frame stays alive until the end, so no two share an address.
	opening := kinds[0].build(parts(len(kinds), 8*extra), len(kinds))
	frames := make([]canaryFrame, len(kinds))
	for k, kind := range kinds {
		frames[k] = kind.build(parts(k, extra+k), k)
	}
	walked := widen(map[string]interface{}{}, extra)
	// Room for every record up front, so keeping one never grows the slice,
	// and no lock: only this goroutine emits.
	kept := make([]slog.Record, 0, len(kinds)+2)
	canaryUseProcessLogger(t, canaryProbeHandler{onRecord: func(r slog.Record) { kept = append(kept, r) }})
	warmUp := make([]slog.Attr, 0, len(winnerCanaryAttrKeys))
	for _, key := range winnerCanaryAttrKeys {
		warmUp = append(warmUp, slog.String(key, "synthetic"))
	}
	slog.LogAttrs(context.Background(), slog.LevelInfo, "synthetic warm-up", warmUp...)
	// emit times one emission and returns it with what it logged.
	emit := func(f canaryFrame) (time.Duration, []slog.Record) {
		n := len(kept)
		started := time.Now()
		logWinnerCanary(f.msg, f.status, f.event)
		elapsed := time.Since(started)
		return elapsed, kept[n:]
	}
	runtime.GC()
	onePass := canaryBestOf(3, canaryWalk(t, walked, extra))
	before := time.Now()
	firstCall, openingLogged := emit(opening)
	calls := make([]time.Duration, len(kinds))
	logged := make([][]slog.Record, len(kinds))
	for k, f := range frames {
		calls[k], logged[k] = emit(f)
	}
	after := time.Now()
	t.Logf("first call of the process %v, one walk %v (%.1fx)", firstCall, onePass, float64(onePass)/float64(firstCall))
	// Each record is its kind's, carrying the hash of the frame's own event
	// id, which the contract model computes.
	want := func(kind canaryFrameKind, f canaryFrame) map[string]interface{} {
		record := kind.record()
		if record != nil && kind.want.eventKeyHash != "none" {
			model, _ := referenceWinnerCanary(f.msg.Topic.Type, f.msg.Type, f.status, f.event, version.Version)
			record["event_key_hash"] = model["event_key_hash"]
		}
		return record
	}
	canaryAssertEmitted(t, want(kinds[0], opening), openingLogged, 1, before, after)
	for k, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			t.Logf("first call %v, one walk %v (%.0fx)", calls[k], onePass, float64(onePass)/float64(calls[k]))
			canaryAssertEmitted(t, want(kind, frames[k]), logged[k], 1, before, after)
		})
	}
	if path := os.Getenv(canaryTimingsEnv); path != "" {
		raw, err := json.Marshal(canaryFreshTimings{Walk: onePass, First: firstCall, Calls: calls})
		if err == nil {
			err = os.WriteFile(path, raw, 0o600)
		}
		if err != nil {
			t.Fatalf("reporting the timings: %v", err)
		}
	}
}

// TestWinnerCanaryLeavesUninspectedValuesUnread builds one frame of every kind
// canaryFrameKinds lists and lets other goroutines write into every value in
// it the canary has no reason to read — the message's data envelope and raw
// message, every outcome's predictors and nested object, and an object or
// array standing where the winner, the event id, the outcomes, an outcome or an
// outcome's id belongs — while the canary classifies and emits the frame,
// twice each. Nothing orders those writes against the canary, so under the
// race detector (go test -race, as CI and the quality gates run it) reading
// what any of those values holds — a map entry, the keys of a map in a walk,
// or an array element — is a reported data race, however quick; an array's
// length alone is not, since it sits in the slice header rather than in what
// the writers write. The event and the outcome objects, which the canary does
// read by key, are left to the timing tests. The test runs in a fresh process,
// so that no earlier test has already made the canary's first call of the
// process or of a branch. Each record, classified and emitted, is checked
// against its kind's literal expectation, or that there is none, so a frame
// that stopped reaching its case fails; without -race that is all this test
// checks, and the timing tests take the same kinds of frame.
func TestWinnerCanaryLeavesUninspectedValuesUnread(t *testing.T) {
	if !canaryInFreshProcess(t) {
		return
	}
	capture := &canaryCapture{level: canaryCaptureAll}
	canaryUseProcessLogger(t, capture) // its t.Setenv also refuses a parallel test
	for _, kind := range canaryFrameKinds() {
		t.Run(kind.name, func(t *testing.T) {
			var writes []func()
			watched := func(m map[string]interface{}) map[string]interface{} {
				writes = append(writes, func() { m["SENTINEL_WRITE"] = true })
				return m
			}
			list := func() []interface{} {
				l := []interface{}{"SENTINEL_ELEMENT", "SENTINEL_ELEMENT"}
				for i := range l {
					writes = append(writes, func() { l[i] = "SENTINEL_WRITE" })
				}
				return l
			}
			f := kind.build(canaryParts{
				inspected:  func(m map[string]interface{}) map[string]interface{} { return m },
				object:     watched,
				array:      list,
				predictors: list,
				nest:       func() map[string]interface{} { return watched(map[string]interface{}{}) },
				envelope:   watched,
			}, 1)
			var writers sync.WaitGroup
			for _, write := range writes {
				writers.Add(1)
				go func() {
					defer writers.Done()
					write()
				}()
			}
			before := time.Now()
			var recs [2]winnerCanaryRecord
			var oks [2]bool
			for i := range recs {
				recs[i], oks[i] = classifyWinnerCanary(f.msg, f.status, f.event, "synthetic-build")
				logWinnerCanary(f.msg, f.status, f.event)
			}
			writers.Wait()
			after := time.Now()
			want := kind.record()
			for i := range recs {
				canaryAssertClassified(t, want, recs[i], oks[i])
			}
			canaryAssertEmitted(t, want, capture.take(), len(recs), before, after)
		})
	}
}

// TestWinnerCanaryRecordsFollowTheFrameNotItsAddress changes one event object,
// and then one of its outcome objects, in place between calls. A canary that
// kept anything from one call to the next — a record, or a verdict keyed by the
// address of the event or of an outcome, the event id or the frame's size —
// would answer a later call with what the frame used to say; every record,
// classified and emitted, must match what the frame says at that moment. It
// runs in a fresh process, since it counts the records the process logs.
func TestWinnerCanaryRecordsFollowTheFrameNotItsAddress(t *testing.T) {
	if !canaryInFreshProcess(t) {
		return
	}
	capture := &canaryCapture{level: canaryCaptureAll}
	canaryUseProcessLogger(t, capture) // its t.Setenv also refuses a parallel test
	firstMatched := canaryPositive0001
	firstMatched.winnerIndex = 0
	noWinner := canaryUntracked0003
	noWinner.eventKeyHash = canaryHash0001
	positive0003 := canaryPositive0001
	positive0003.eventKeyHash = canaryHash0003
	collided := canaryFrameAttrs{
		winnerPresence: "PRESENT", winnerJSONType: "string", outcomesPresence: "PRESENT",
		outcomeCount: 2, outcomeIDsValid: false, winnerMatchCount: -1, winnerIndex: -1,
		eventKeyHash: canaryHash0003, result: canaryMalformed,
	}
	msg := canaryEventMsg()
	event := canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair())
	for _, step := range []struct {
		name   string
		change func()
		want   canaryFrameAttrs
	}{
		{"as built", func() {}, canaryPositive0001},
		{"the winner names the first outcome", func() { event["winning_outcome_id"] = "synthetic-outcome-a" }, firstMatched},
		{"the winner is gone", func() { delete(event, "winning_outcome_id") }, noWinner},
		{"another event names the second outcome", func() {
			event["id"] = "synthetic-event-0003"
			event["winning_outcome_id"] = "synthetic-outcome-b"
		}, positive0003},
		{"the outcome ids collide", func() {
			event["outcomes"] = canaryOutcomes("synthetic-outcome-b", "synthetic-outcome-b")
		}, collided},
		{"the first outcome's id changes in place", func() {
			event["outcomes"].([]interface{})[0].(map[string]interface{})["id"] = "synthetic-outcome-a"
		}, positive0003},
	} {
		step.change()
		before := time.Now()
		rec, ok := classifyWinnerCanary(msg, "RESOLVED", event, "synthetic-build")
		if !ok {
			t.Fatalf("%s: the frame was refused", step.name)
		}
		logWinnerCanary(msg, "RESOLVED", event)
		after := time.Now()
		emitted := capture.take()
		t.Run(step.name, func(t *testing.T) {
			assertCanaryFields(t, canaryClassifiedFields(t, rec), step.want.values(), "synthetic-build", before, time.Now())
			if len(emitted) != 1 || emitted[0].Message != "p4_winner_canary" || emitted[0].Level != slog.LevelInfo {
				t.Fatalf("the emitter logged %d record(s), want exactly one INFO p4_winner_canary", len(emitted))
			}
			assertCanaryFields(t, canaryFields(t, canaryRecordAttrs(emitted[0])), step.want.values(), version.Version, before, after)
		})
	}
}

// canaryTrap stands in for a frame value this build does not understand. The
// methods a logger, formatter or encoder calls to turn a value into text —
// String, GoString, Error, LogValue, MarshalJSON, MarshalText — count their
// calls, so a canary that rendered an unknown value through any of them is
// caught.
type canaryTrap struct{ calls *atomic.Int64 }

func (c canaryTrap) String() string   { c.calls.Add(1); return "SENTINEL-TRAP-STRING" }
func (c canaryTrap) GoString() string { c.calls.Add(1); return "SENTINEL-TRAP-GOSTRING" }
func (c canaryTrap) Error() string    { c.calls.Add(1); return "SENTINEL-TRAP-ERROR" }
func (c canaryTrap) LogValue() slog.Value {
	c.calls.Add(1)
	return slog.StringValue("SENTINEL-TRAP-LOGVALUE")
}
func (c canaryTrap) MarshalJSON() ([]byte, error) {
	c.calls.Add(1)
	return []byte(`"SENTINEL-TRAP-JSON"`), nil
}
func (c canaryTrap) MarshalText() ([]byte, error) {
	c.calls.Add(1)
	return []byte("SENTINEL-TRAP-TEXT"), nil
}

// TestWinnerCanaryNonJSONValuesAreClassifiedNotFormatted feeds values the JSON
// decoder never produces. They are classified by shape ("other" where they are
// not one of the six JSON types) and never stringified: each case's own trap
// count stays at zero through classification AND through rendering the record
// with the real Text and JSON handlers, and the emitter produces exactly one
// record of any kind. One ordinary multibyte event id rides along, to show it
// is hashed over its UTF-8 bytes. It counts every record the process logs, so
// it runs in a fresh process.
func TestWinnerCanaryNonJSONValuesAreClassifiedNotFormatted(t *testing.T) {
	if !canaryInFreshProcess(t) {
		return
	}
	var nilString *string
	invalidWinner := canaryFrameAttrs{winnerPresence: "INVALID", winnerJSONType: "other", outcomesPresence: "PRESENT",
		outcomeCount: 2, outcomeIDsValid: true, winnerMatchCount: -1, winnerIndex: -1,
		eventKeyHash: canaryHash0001, result: canaryMalformed}
	invalidVector := canaryFrameAttrs{winnerPresence: "PRESENT", winnerJSONType: "string", outcomesPresence: "PRESENT",
		outcomeCount: 2, winnerMatchCount: -1, winnerIndex: -1, eventKeyHash: canaryHash0001, result: canaryMalformed}
	invalidOutcomes := canaryFrameAttrs{winnerPresence: "PRESENT", winnerJSONType: "string", outcomesPresence: "INVALID",
		outcomeCount: -1, winnerMatchCount: -1, winnerIndex: -1, eventKeyHash: canaryHash0001, result: canaryMalformed}
	emptyVector := invalidVector
	emptyVector.outcomeCount = 0
	noEventID := canaryPositive0001
	noEventID.eventKeyHash, noEventID.result = "none", canaryMalformed
	utf8ID := canaryPositive0001
	utf8ID.eventKeyHash = canaryHashUTF8

	for _, tc := range []struct {
		name  string
		event func(trap canaryTrap) map[string]interface{}
		want  canaryFrameAttrs
	}{
		{"winner is a trap", func(trap canaryTrap) map[string]interface{} {
			return canaryEvent("synthetic-event-0001", trap, canaryPair())
		}, invalidWinner},
		{"winner is json.Number", func(canaryTrap) map[string]interface{} {
			return canaryEvent("synthetic-event-0001", json.Number("1"), canaryPair())
		}, invalidWinner},
		{"winner is a Go int", func(canaryTrap) map[string]interface{} {
			return canaryEvent("synthetic-event-0001", 1, canaryPair())
		}, invalidWinner},
		{"winner is a typed nil pointer", func(canaryTrap) map[string]interface{} {
			return canaryEvent("synthetic-event-0001", nilString, canaryPair())
		}, invalidWinner},
		{"event id is a trap", func(trap canaryTrap) map[string]interface{} {
			return canaryEvent(trap, "synthetic-outcome-b", canaryPair())
		}, noEventID},
		{"outcomes is a trap", func(trap canaryTrap) map[string]interface{} {
			return canaryEvent("synthetic-event-0001", "synthetic-outcome-b", trap)
		}, invalidOutcomes},
		{"outcomes is a []string", func(canaryTrap) map[string]interface{} {
			return canaryEvent("synthetic-event-0001", "synthetic-outcome-b", []string{"synthetic-outcome-a", "synthetic-outcome-b"})
		}, invalidOutcomes},
		{"outcomes is a nil []interface{}", func(canaryTrap) map[string]interface{} {
			return canaryEvent("synthetic-event-0001", "synthetic-outcome-b", []interface{}(nil))
		}, emptyVector},
		{"an outcome id is a trap", func(trap canaryTrap) map[string]interface{} {
			return canaryEvent("synthetic-event-0001", "synthetic-outcome-a", canaryOutcomes("synthetic-outcome-a", trap))
		}, invalidVector},
		{"an outcome is a map[string]string", func(canaryTrap) map[string]interface{} {
			return canaryEvent("synthetic-event-0001", "synthetic-outcome-a",
				[]interface{}{map[string]interface{}{"id": "synthetic-outcome-a"}, map[string]string{"id": "synthetic-outcome-b"}})
		}, invalidVector},
		{"an outcome is a nil object", func(canaryTrap) map[string]interface{} {
			return canaryEvent("synthetic-event-0001", "synthetic-outcome-a",
				[]interface{}{map[string]interface{}{"id": "synthetic-outcome-a"}, map[string]interface{}(nil)})
		}, invalidVector},
		{"an outcome is a trap", func(trap canaryTrap) map[string]interface{} {
			return canaryEvent("synthetic-event-0001", "synthetic-outcome-a",
				[]interface{}{map[string]interface{}{"id": "synthetic-outcome-a"}, trap})
		}, invalidVector},
		{"unknown keys, a secret and a title hold traps", func(trap canaryTrap) map[string]interface{} {
			ev := canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair())
			ev["SENTINEL_TRAP_KEY"] = trap
			ev["auth_token"] = trap
			ev["outcomes"].([]interface{})[0].(map[string]interface{})["title"] = trap
			return ev
		}, canaryPositive0001},
		{"a multibyte event id is hashed as its UTF-8 bytes", func(canaryTrap) map[string]interface{} {
			return canaryEvent("synthetic-évènement-0003", "synthetic-outcome-b", canaryPair())
		}, utf8ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := &atomic.Int64{}
			capture := &canaryCapture{level: canaryCaptureAll}
			canaryUseProcessLogger(t, capture)
			before := time.Now()
			logWinnerCanary(canaryEventMsg(), "RESOLVED", tc.event(canaryTrap{calls: calls}))
			after := time.Now()
			got := capture.all()
			if len(got) != 1 || got[0].Message != "p4_winner_canary" {
				t.Fatalf("records = %d, want exactly the one canary record", len(got))
			}
			assertCanaryFields(t, canaryFields(t, canaryRecordAttrs(got[0])), tc.want.values(), version.Version, before, after)
			assertCanaryPrivate(t, renderCanary(t, got[0]))
			if n := calls.Load(); n != 0 {
				t.Fatalf("the canary invoked %d String/LogValue/Marshal method(s) on frame values", n)
			}
		})
	}
}

// TestWinnerCanaryIgnoresNonCandidatesAtTheClassifier covers the envelope
// checks the handler cannot reach: a nil message and a nil event object. It
// counts every record the process logs, so it runs in a fresh process.
func TestWinnerCanaryIgnoresNonCandidatesAtTheClassifier(t *testing.T) {
	if !canaryInFreshProcess(t) {
		return
	}
	event := canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair())
	if _, ok := classifyWinnerCanary(canaryEventMsg(), "RESOLVED", event, "synthetic-build"); !ok {
		t.Fatal("control: the qualifying frame was not admitted")
	}
	if _, ok := classifyWinnerCanary(nil, "RESOLVED", event, "synthetic-build"); ok {
		t.Error("a nil message produced a record")
	}
	if _, ok := classifyWinnerCanary(canaryEventMsg(), "RESOLVED", nil, "synthetic-build"); ok {
		t.Error("a nil event object produced a record")
	}
	capture := &canaryCapture{level: canaryCaptureAll}
	canaryUseProcessLogger(t, capture)
	logWinnerCanary(nil, "RESOLVED", event)
	logWinnerCanary(canaryEventMsg(), "RESOLVED", nil)
	if n := len(capture.all()); n != 0 {
		t.Fatalf("non-candidates emitted %d record(s)", n)
	}
}

// TestWinnerCanaryRecordsCarryTheBuildVersionLabel pins build_version to the
// internal/version label: a distinctive label set for this test must be the one
// the record carries.
func TestWinnerCanaryRecordsCarryTheBuildVersionLabel(t *testing.T) {
	t.Setenv(canaryGlobalsEnv, "1") // version.Version is process-wide
	prev := version.Version
	version.Version = "synthetic-label-7f31"
	t.Cleanup(func() { version.Version = prev })

	capture := &canaryCapture{level: slog.LevelInfo}
	canaryUseProcessLogger(t, capture)
	logWinnerCanary(canaryEventMsg(), "RESOLVED", canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair()))
	got := capture.canaries()
	if len(got) != 1 {
		t.Fatalf("records = %d, want 1", len(got))
	}
	if label := canaryFields(t, canaryRecordAttrs(got[0]))["build_version"]; label != "synthetic-label-7f31" {
		t.Fatalf("build_version = %v, want the internal/version label synthetic-label-7f31", label)
	}
}

// TestWinnerCanaryObservedAtIsRenderedInUTC renders a record for an instant
// given in a non-UTC zone: observed_at_utc is that same instant, RFC3339, in
// UTC. That the instant is the receiver's clock is checked against the receive
// window wherever a record is emitted.
func TestWinnerCanaryObservedAtIsRenderedInUTC(t *testing.T) {
	rec, ok := classifyWinnerCanary(canaryEventMsg(), "RESOLVED",
		canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair()), "synthetic-build")
	if !ok {
		t.Fatal("control frame refused")
	}
	observed := time.Date(2026, 9, 23, 18, 4, 5, 987654321, time.FixedZone("UTC+3", 3*60*60))
	if got := canaryFields(t, rec.attrs(observed))["observed_at_utc"]; got != "2026-09-23T15:04:05Z" {
		t.Fatalf("observed_at_utc = %v, want 2026-09-23T15:04:05Z", got)
	}
}

// TestWinnerCanaryObservedAtIsUTCInANonUTCProcess emits a record in a fresh
// process whose local zone is Asia/Tokyo, where the receive time rendered in
// local time would differ from UTC; on a UTC host every other test would pass
// either way. observed_at_utc must still be the receive instant, in UTC.
func TestWinnerCanaryObservedAtIsUTCInANonUTCProcess(t *testing.T) {
	if !canaryInFreshProcess(t, "TZ=Asia/Tokyo") {
		return
	}
	if _, offset := time.Now().Zone(); offset == 0 {
		t.Fatal("the fresh process still keeps UTC as its local zone, so this check would prove nothing")
	}
	capture := &canaryCapture{level: slog.LevelInfo}
	canaryUseProcessLogger(t, capture)
	before := time.Now()
	logWinnerCanary(canaryEventMsg(), "RESOLVED", canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair()))
	after := time.Now()
	got := capture.canaries()
	if len(got) != 1 {
		t.Fatalf("records = %d, want 1", len(got))
	}
	assertCanaryFields(t, canaryFields(t, canaryRecordAttrs(got[0])), canaryPositive0001.values(), version.Version, before, after)
}

// --- Real handler wiring ------------------------------------------------------

// canaryRoundStatus reads a tracked round's status through the pool's exported
// snapshot, or "" when the round is not tracked.
func canaryRoundStatus(p *WebSocketPool, eventID string) string {
	for _, snap := range p.PredictionsSnapshot() {
		if snap.EventID == eventID {
			return snap.Status
		}
	}
	return ""
}

// TestWinnerCanaryRealHandlerEmitsForEveryQualifyingFrame drives parsed frames
// through the pool's real dispatcher. Tracked and untracked RESOLVED updates
// are both candidates (the canary runs before the tracked-round lookup), and
// neither sequential nor concurrent duplicates are suppressed.
func TestWinnerCanaryRealHandlerEmitsForEveryQualifyingFrame(t *testing.T) {
	capture := &canaryCapture{level: slog.LevelInfo}
	canaryUseProcessLogger(t, capture)
	p, sink := observedPool(t, &fakePlacer{})
	s := newTestStreamer(100000)
	p.streamers = []*models.Streamer{s}
	admitCanaryRound(p, s, "synthetic-event-0001")

	tracked := canaryUpdateFrame("synthetic-event-0001", "RESOLVED", 310, 210, "synthetic-outcome-b")
	untracked := canaryUpdateFrame("synthetic-event-0003", "RESOLVED", 10, 20, "")
	wantTracked := canaryPositive0001.values()
	wantUntracked := canaryUntracked0003.values()

	before := time.Now()
	p.handleMessage(parseCanaryFrame(t, canaryTopicChan1, tracked))
	p.handleMessage(parseCanaryFrame(t, canaryTopicChan1, untracked))
	for i := 0; i < 3; i++ {
		p.handleMessage(parseCanaryFrame(t, canaryTopicChan1, tracked))
	}
	const concurrent = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			msg, err := ParsePubSubMessage(&WSData{Topic: canaryTopicChan1, Message: string(tracked)})
			if err != nil {
				t.Errorf("parse: %v", err)
				return
			}
			p.handleMessage(msg)
		}()
	}
	close(start)
	wg.Wait()
	after := time.Now()

	got := capture.canaries()
	if len(got) != 1+1+3+concurrent {
		t.Fatalf("records = %d, want %d: one per qualifying frame, duplicates included", len(got), 1+1+3+concurrent)
	}
	for i, r := range got {
		want := wantTracked
		if i == 1 {
			want = wantUntracked
		}
		assertCanaryFields(t, canaryFields(t, canaryRecordAttrs(r)), want, version.Version, before, after)
		assertCanaryPrivate(t, renderCanary(t, r))
	}

	// The business path still ran for every frame: the tracked round is
	// resolved, the untracked one was never admitted, and each tracked
	// RESOLVED frame scheduled its cleanup.
	if status, untrackedStatus := canaryRoundStatus(p, "synthetic-event-0001"), canaryRoundStatus(p, "synthetic-event-0003"); status != "RESOLVED" || untrackedStatus != "" {
		t.Fatalf("round state: tracked=%q untracked=%q", status, untrackedStatus)
	}
	channelEvents, cleanups := 0, 0
	for _, o := range sink.all() {
		switch {
		case o.Kind == ObsKindChannelEvent:
			channelEvents++
		case o.Kind == ObsKindRoundCleanup && o.Payload.Phase == "CLEANUP_SCHEDULED":
			cleanups++
		}
	}
	if channelEvents != 1+1+3+concurrent || cleanups != 1+3+concurrent {
		t.Fatalf("channel events = %d, cleanups scheduled = %d; want %d and %d",
			channelEvents, cleanups, 1+1+3+concurrent, 1+3+concurrent)
	}
}

// TestWinnerCanaryEmitsInPlaceWithNoLockHeld dispatches frames through a real
// WebSocketClient into the pool and inspects the moment of emission from
// inside the log handler. The record is handled on the goroutine that
// dispatched the frame, the frame's channel_event observation has already been
// recorded, the tracked round has not yet been updated, and no connection,
// pool, round or observation-sink lock is held. It then emits a second record
// for the same event from that moment, which neither a lock or TryLock held
// across emission nor the suppression of an in-flight duplicate can let
// through. The frames are sequential: a concurrent handler could hold the pool
// lock legitimately.
func TestWinnerCanaryEmitsInPlaceWithNoLockHeld(t *testing.T) {
	p, sink := observedPool(t, &fakePlacer{})
	s := newTestStreamer(100000)
	p.streamers = []*models.Streamer{s}
	admitCanaryRound(p, s, "synthetic-event-0001")
	ws := NewWebSocketClient(0, nil, 3600, 1, p.handleMessage, nil)
	p.mu.RLock()
	round := p.control["synthetic-event-0001"]
	p.mu.RUnlock()

	var mu sync.Mutex
	var trail, problems []string
	note := func(list *[]string, entry string) {
		mu.Lock()
		*list = append(*list, entry)
		mu.Unlock()
	}
	sink.mu.Lock()
	sink.hook = func(o PredictionObservation) {
		note(&trail, "observation "+o.Kind+" "+o.Payload.Phase+" "+o.EventID)
	}
	sink.mu.Unlock()

	locks := map[string]interface {
		TryLock() bool
		Unlock()
	}{"pool": &p.mu, "connection": &ws.mu, "connection write": &ws.writeMu, "round placement": &round.placeMu, "observation sink": &sink.mu}
	var reentered atomic.Bool
	canaryUseProcessLogger(t, canaryProbeHandler{onRecord: func(r slog.Record) {
		if r.Message != "p4_winner_canary" {
			return // the connection's own debug lines are not under test here
		}
		hash := canaryRecordHash(r)
		where := "elsewhere"
		if stack := make([]byte, 64<<10); strings.Contains(string(stack[:runtime.Stack(stack, false)]), "handlePredictionChannel") {
			where = "on the dispatching goroutine"
		}
		note(&trail, "canary "+hash+" "+where)
		poolFree := true
		for name, l := range locks {
			if !l.TryLock() {
				note(&problems, name+" lock held during emission")
				poolFree = poolFree && name != "pool"
				continue
			}
			l.Unlock()
		}
		if hash != canaryHash0001 || !reentered.CompareAndSwap(false, true) {
			return
		}
		if poolFree {
			note(&trail, "round status at emission "+canaryRoundStatus(p, "synthetic-event-0001"))
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			logWinnerCanary(canaryEventMsg(), "RESOLVED", canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair()))
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			note(&problems, "a second emission started during the first did not finish within 5s: emission holds a lock")
		}
	}})

	for _, frame := range [][]byte{
		canaryUpdateFrame("synthetic-event-0001", "RESOLVED", 310, 210, "synthetic-outcome-b"),
		canaryUpdateFrame("synthetic-event-0003", "RESOLVED", 10, 20, ""),
	} {
		ws.handleMessage(WSMessage{Type: "MESSAGE", Data: &WSData{Topic: canaryTopicChan1, Message: string(frame)}})
	}

	mu.Lock()
	defer mu.Unlock()
	if len(problems) != 0 {
		t.Fatalf("problems at the moment of emission:\n  %s", strings.Join(problems, "\n  "))
	}
	wantTrail := []string{
		"observation channel_event ROUND_UPDATED synthetic-event-0001",
		"canary " + canaryHash0001 + " on the dispatching goroutine",
		"round status at emission ACTIVE",
		"canary " + canaryHash0001 + " elsewhere",
		"observation round_cleanup CLEANUP_SCHEDULED synthetic-event-0001",
		"observation channel_event ROUND_UPDATED synthetic-event-0003",
		"canary " + canaryHash0003 + " on the dispatching goroutine",
	}
	if !reflect.DeepEqual(trail, wantTrail) {
		t.Fatalf("emission order =\n  %s\nwant\n  %s", strings.Join(trail, "\n  "), strings.Join(wantTrail, "\n  "))
	}
	if status := canaryRoundStatus(p, "synthetic-event-0001"); status != "RESOLVED" {
		t.Fatalf("tracked round status after the frame = %q, want RESOLVED", status)
	}
}

// TestWinnerCanaryConcurrentDirectCallsShareNothing calls the emitter from many
// goroutines at once, with no pool lock to order them, on frames of their own:
// every call yields its record, and the race detector sees no shared state.
func TestWinnerCanaryConcurrentDirectCallsShareNothing(t *testing.T) {
	capture := &canaryCapture{level: slog.LevelInfo}
	canaryUseProcessLogger(t, capture)
	const workers, perWorker = 16, 40
	start := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perWorker; i++ {
				logWinnerCanary(canaryEventMsg(), "RESOLVED", canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair()))
			}
		}()
	}
	before := time.Now()
	close(start)
	wg.Wait()
	after := time.Now()
	got := capture.canaries()
	if len(got) != workers*perWorker {
		t.Fatalf("records = %d, want %d: every concurrent call must emit", len(got), workers*perWorker)
	}
	for _, r := range got {
		assertCanaryFields(t, canaryFields(t, canaryRecordAttrs(r)), canaryPositive0001.values(), version.Version, before, after)
	}
}

// TestWinnerCanaryConcurrentCallsLeaveASharedFrameAlone has many goroutines
// emit for ONE parsed frame while another keeps copying all of it: every field
// of the message and, deep, its data envelope and raw message — the event, each
// outcome and its predictors included. The canary only reads its input, so
// under -race any write to the frame — even one undone before the call returns
// — is a reported data race; and every call still emits its record.
func TestWinnerCanaryConcurrentCallsLeaveASharedFrameAlone(t *testing.T) {
	capture := &canaryCapture{level: slog.LevelInfo}
	canaryUseProcessLogger(t, capture)
	msg := canaryStampConnection(parseCanaryFrame(t, canaryTopicChan1, canaryUpdateFrame("synthetic-event-0001", "RESOLVED", 310, 210, "synthetic-outcome-b")))
	event := msg.Data["event"].(map[string]interface{})
	const workers, perWorker = 8, 50
	start, stop := make(chan struct{}), make(chan struct{})
	var reader, emitters sync.WaitGroup
	var seen PubSubMessage
	var seenData, seenRaw interface{}
	reader.Add(1)
	go func() {
		defer reader.Done()
		<-start
		for {
			seen = *msg
			seenData, seenRaw = canaryCloneJSON(msg.Data), canaryCloneJSON(msg.Message)
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	for w := 0; w < workers; w++ {
		emitters.Add(1)
		go func() {
			defer emitters.Done()
			<-start
			for i := 0; i < perWorker; i++ {
				logWinnerCanary(msg, "RESOLVED", event)
			}
		}()
	}
	before := time.Now()
	close(start)
	emitters.Wait()
	close(stop)
	reader.Wait()
	after := time.Now()
	if seen.Type != "event-updated" || !reflect.DeepEqual(seenData, msg.Data) || !reflect.DeepEqual(seenRaw, msg.Message) {
		t.Fatalf("the reader never saw the shared message whole")
	}
	got := capture.canaries()
	if len(got) != workers*perWorker {
		t.Fatalf("records = %d, want %d: every call on the shared frame must emit", len(got), workers*perWorker)
	}
	for _, r := range got {
		assertCanaryFields(t, canaryFields(t, canaryRecordAttrs(r)), canaryPositive0001.values(), version.Version, before, after)
	}
}

// --- Business behaviour, canary emission active vs inactive -------------------

// canaryBusinessTrail is the part of the pool's business output this test
// compares between runs: the observation trail, the placement calls and the
// tracked round state. Log lines are checked apart from it, and the
// process-wide dashboard event feed, which every test in the process shares, is
// not compared.
type canaryBusinessTrail struct {
	observations []PredictionObservation
	placerCalls  int
	placedID     string
	placedAmount int
	manualTitle  string
	manualErr    string
	rounds       []canaryRoundState
}

type canaryRoundState struct {
	eventID   string
	status    string
	betPlaced bool
	betAmount int
	outcomes  []canaryOutcomeState
}

type canaryOutcomeState struct {
	id          string
	totalUsers  int
	totalPoints int
	topPoints   int
	chosen      bool
}

// canaryRounds reads every tracked round through the exported snapshot, plus
// each outcome's TopPoints, which the snapshot does not expose.
func canaryRounds(p *WebSocketPool) []canaryRoundState {
	snaps := p.PredictionsSnapshot()
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].EventID < snaps[j].EventID })
	p.mu.RLock()
	defer p.mu.RUnlock()
	var rounds []canaryRoundState
	for _, snap := range snaps {
		top := map[string]int{}
		if ep := p.predictions[snap.EventID]; ep != nil {
			for _, o := range ep.Bet.Outcomes {
				if o != nil {
					top[o.ID] = o.TopPoints
				}
			}
		}
		rs := canaryRoundState{eventID: snap.EventID, status: snap.Status, betPlaced: snap.BetPlaced, betAmount: snap.BetAmount}
		for _, o := range snap.Outcomes {
			rs.outcomes = append(rs.outcomes, canaryOutcomeState{id: o.ID, totalUsers: o.TotalUsers,
				totalPoints: o.TotalPoints, topPoints: top[o.ID], chosen: o.Chosen})
		}
		rounds = append(rounds, rs)
	}
	return rounds
}

// runCanaryBusinessScenario plays one fixed sequence of frames and a manual bet
// through a fresh pool, with h as the process-wide slog default. A non-nil
// observed sees each observation as the pool records it.
func runCanaryBusinessScenario(t *testing.T, h slog.Handler, observed func(PredictionObservation)) canaryBusinessTrail {
	t.Helper()
	canaryUseProcessLogger(t, h)
	placer := &fakePlacer{}
	p, sink := observedPool(t, placer)
	sink.hook = observed
	s := newTestStreamer(100000)
	p.streamers = []*models.Streamer{s}
	admitCanaryRound(p, s, "synthetic-event-0001")
	admitCanaryRound(p, s, "synthetic-event-0002")
	deliver := func(frame []byte) { p.handleMessage(parseCanaryFrame(t, canaryTopicChan1, frame)) }

	deliver(canaryCreatedFrame("synthetic-event-0001", "ACTIVE"))
	deliver(canaryUpdateFrame("synthetic-event-0001", "ACTIVE", 300, 200, ""))
	deliver(canaryUpdateFrame("synthetic-event-0001", "RESOLVED", 310, 210, "synthetic-outcome-b"))
	deliver(canaryUpdateFrame("synthetic-event-0001", "RESOLVED", 310, 210, "synthetic-outcome-b"))
	deliver(canaryUpdateFrame("synthetic-event-0003", "RESOLVED", 10, 20, ""))
	title, err := p.PlaceManualBet("synthetic-event-0002", "synthetic-outcome-a", 500)
	deliver(canaryUpdateFrame("synthetic-event-0002", "LOCKED", 800, 200, ""))
	deliver(canaryUpdateFrame("synthetic-event-0002", "RESOLVE_PENDING", 800, 200, ""))
	deliver(canaryUpdateFrame("synthetic-event-0002", "CANCELED", 800, 200, ""))

	trail := canaryBusinessTrail{placerCalls: placer.callCount(), manualTitle: title}
	trail.placedID, trail.placedAmount = placer.lastID, placer.lastAmt
	if err != nil {
		trail.manualErr = err.Error()
	}
	// Wall-clock receive time is the only field normalized away: this test
	// claims identical behaviour, not identical timing.
	for _, o := range sink.all() {
		o.ReceivedAtMS = 0
		trail.observations = append(trail.observations, o)
	}
	trail.rounds = canaryRounds(p)
	return trail
}

// TestWinnerCanaryLeavesBusinessBehaviourUnchanged plays the same scenario with
// canary emission active (a lossless INFO capture) and inactive (a default
// logger that discards INFO). Both runs execute the canary's classification;
// only whether its record reaches a handler differs. The business trails —
// every observation, the placement calls and the tracked rounds' full outcome
// state — must be identical and match the literal expectation, which holds
// unchanged with the canary call removed from the pool. Log output is not part
// of the trail, since the inactive default discards every INFO record, business
// ones included; the scenario's one business log line, the manual bet's, is
// checked in the active run on its own.
func TestWinnerCanaryLeavesBusinessBehaviourUnchanged(t *testing.T) {
	activeCapture := &canaryCapture{level: slog.LevelInfo}
	active := runCanaryBusinessScenario(t, activeCapture, nil)
	inactiveCapture := &canaryCapture{level: slog.LevelWarn}
	inactive := runCanaryBusinessScenario(t, inactiveCapture, nil)

	t.Run("business", func(t *testing.T) {
		if !reflect.DeepEqual(active, inactive) {
			t.Fatalf("business trail differs with canary emission active vs inactive:\nactive:   %+v\ninactive: %+v", active, inactive)
		}
		var trail []string
		for _, o := range active.observations {
			trail = append(trail, strings.Join(strings.Fields(strings.Join([]string{o.Kind, o.EventID,
				o.Payload.Phase, o.Payload.RoundState, o.Payload.Decision, o.Payload.ReasonCode}, " ")), " "))
		}
		wantTrail := []string{
			"channel_event synthetic-event-0001 ROUND_CREATED ACTIVE",
			"schedule_decision synthetic-event-0001 SCHEDULE_SKIPPED ACTIVE SKIP ALREADY_TRACKED",
			"channel_event synthetic-event-0001 ROUND_UPDATED ACTIVE",
			"channel_event synthetic-event-0001 ROUND_UPDATED RESOLVED",
			"round_cleanup synthetic-event-0001 CLEANUP_SCHEDULED OK",
			"channel_event synthetic-event-0001 ROUND_UPDATED RESOLVED",
			"round_cleanup synthetic-event-0001 CLEANUP_SCHEDULED OK",
			"channel_event synthetic-event-0003 ROUND_UPDATED RESOLVED",
			"manual_control synthetic-event-0002 MANUAL_DIRECT_ROOT OK",
			"manual_control synthetic-event-0002 MANUAL_POOL_LOOKUP OK",
			"manual_control synthetic-event-0002 MANUAL_ELIGIBILITY OK",
			"manual_control synthetic-event-0002 MANUAL_ARGUMENTS OK",
			"manual_control synthetic-event-0002 MANUAL_RESERVATION OK",
			"manual_control synthetic-event-0002 MANUAL_VALIDATION OK",
			"placement synthetic-event-0002 CALL_STARTED OK",
			"placement synthetic-event-0002 CALL_RETURNED OK",
			"manual_control synthetic-event-0002 MANUAL_EXECUTION OK",
			"channel_event synthetic-event-0002 ROUND_UPDATED LOCKED",
			"channel_event synthetic-event-0002 ROUND_UPDATED RESOLVE_PENDING",
			"channel_event synthetic-event-0002 ROUND_UPDATED CANCELED",
			"round_cleanup synthetic-event-0002 CLEANUP_SCHEDULED OK",
		}
		if !reflect.DeepEqual(trail, wantTrail) {
			t.Fatalf("observation trail =\n  %s\nwant\n  %s", strings.Join(trail, "\n  "), strings.Join(wantTrail, "\n  "))
		}
		var manualLog []string
		for _, r := range activeCapture.all() {
			if r.Message == "Manual prediction bet placed" {
				line := r.Level.String()
				r.Attrs(func(a slog.Attr) bool {
					line += " " + a.Key + "=" + a.Value.String()
					return true
				})
				manualLog = append(manualLog, line)
			}
		}
		if want := []string{"INFO streamer=streamer event=Will they win? amount=500"}; !reflect.DeepEqual(manualLog, want) {
			t.Fatalf("manual bet log lines = %q, want %q", manualLog, want)
		}
		if active.placerCalls != 1 || active.placedID != "synthetic-outcome-a" || active.placedAmount != 500 ||
			active.manualErr != "" || active.manualTitle != "Yes" {
			t.Fatalf("placement = %d call(s) id=%q amount=%d title=%q err=%q",
				active.placerCalls, active.placedID, active.placedAmount, active.manualTitle, active.manualErr)
		}
		wantRounds := []canaryRoundState{
			{eventID: "synthetic-event-0001", status: "RESOLVED", outcomes: []canaryOutcomeState{
				{id: "synthetic-outcome-a", totalUsers: 3, totalPoints: 310, topPoints: 100},
				{id: "synthetic-outcome-b", totalUsers: 2, totalPoints: 210},
			}},
			{eventID: "synthetic-event-0002", status: "CANCELED", betPlaced: true, betAmount: 500, outcomes: []canaryOutcomeState{
				{id: "synthetic-outcome-a", totalUsers: 3, totalPoints: 300, chosen: true},
				{id: "synthetic-outcome-b", totalUsers: 2, totalPoints: 200},
			}},
		}
		if !reflect.DeepEqual(active.rounds, wantRounds) {
			t.Fatalf("rounds =\n  %+v\nwant\n  %+v", active.rounds, wantRounds)
		}
	})

	t.Run("diagnostics", func(t *testing.T) {
		if n := len(inactiveCapture.canaries()); n != 0 {
			t.Fatalf("inactive emission still delivered %d canary record(s)", n)
		}
		got := activeCapture.canaries()
		want := []map[string]interface{}{
			canaryPositive0001.values(),
			canaryPositive0001.values(),
			canaryUntracked0003.values(),
		}
		if len(got) != len(want) {
			t.Fatalf("active emission delivered %d canary record(s), want %d", len(got), len(want))
		}
		for i := range want {
			assertCanaryFields(t, canaryFields(t, canaryRecordAttrs(got[i])), want[i], version.Version, time.Now().Add(-time.Minute), time.Now())
		}
	})
}

// TestWinnerCanaryLogFailureIsNeitherRetriedNorSticky offers every record to a
// sink that always fails and lays each attempt beside the observation of the
// frame it came from. Each qualifying frame is attempted exactly once, right
// after the pool observes it — the tracked round's two RESOLVED frames, then the
// untracked one — so a failed record is neither retried nor allowed to silence
// a later frame, even one for the same event. A healthy sink straight after the
// failures gets every record again, and the business path is unaffected.
func TestWinnerCanaryLogFailureIsNeitherRetriedNorSticky(t *testing.T) {
	var mu sync.Mutex
	var trail []string
	note := func(entry string) {
		mu.Lock()
		defer mu.Unlock()
		trail = append(trail, entry)
	}
	failing := &failingCanaryHandler{attempt: func(hash string) { note("canary " + hash) }}
	failed := runCanaryBusinessScenario(t, failing, func(o PredictionObservation) {
		if o.Kind == ObsKindChannelEvent && o.Payload.RoundState == "RESOLVED" {
			note("resolved " + o.EventID)
		}
	})
	healthyCapture := &canaryCapture{level: slog.LevelInfo}
	healthy := runCanaryBusinessScenario(t, healthyCapture, nil)
	want := []string{
		"resolved synthetic-event-0001", "canary " + canaryHash0001,
		"resolved synthetic-event-0001", "canary " + canaryHash0001,
		"resolved synthetic-event-0003", "canary " + canaryHash0003,
	}
	mu.Lock()
	got := append([]string(nil), trail...)
	mu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canary attempts against a failing sink, beside the frames they came from =\n  %s\nwant exactly one per qualifying frame, right after it:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	var afterwards []string
	for _, r := range healthyCapture.canaries() {
		afterwards = append(afterwards, canaryRecordHash(r))
	}
	if want := []string{canaryHash0001, canaryHash0001, canaryHash0003}; !reflect.DeepEqual(afterwards, want) {
		t.Fatalf("records from a healthy sink right after the failures = %q, want %q: the failures left the canary silenced", afterwards, want)
	}
	if !reflect.DeepEqual(failed, healthy) {
		t.Fatalf("a failing log sink changed the business trail:\nfailing: %+v\nhealthy: %+v", failed, healthy)
	}
}

// --- The application logger --------------------------------------------------

// TestWinnerCanaryReachesTheApplicationLogger routes a qualifying frame through
// the real dispatcher into the real internal/logger configured at INFO — never
// DEBUG — with console only, console plus file, file only, and with INFO
// disabled. Everything the logger wrote is checked, not just canary lines:
// exactly the expected canary lines and nothing else, none of it leaking the
// frame. Setup writes logs/ relative to the working directory, so each case runs
// inside its own temporary directory, and the whole test runs in a fresh
// process, where no other test's goroutine can add a line.
func TestWinnerCanaryReachesTheApplicationLogger(t *testing.T) {
	if !canaryInFreshProcess(t) {
		return
	}
	frame := canaryUpdateFrame("synthetic-event-0001", "RESOLVED", 310, 210, "synthetic-outcome-b")
	want := canaryPositive0001.values()

	for _, tc := range []struct {
		name        string
		settings    config.LoggerSettings
		wantConsole int
		wantFile    int // -1: no log file is configured
	}{
		{"console at INFO, Save=false", config.LoggerSettings{ConsoleLevel: "INFO", FileLevel: "INFO"}, 1, -1},
		{"console and file at INFO, Save=true", config.LoggerSettings{Save: true, ConsoleLevel: "INFO", FileLevel: "INFO"}, 1, 1},
		{"file only at INFO, Save=true", config.LoggerSettings{Save: true, ConsoleLevel: "WARN", FileLevel: "INFO"}, 0, 1},
		{"INFO disabled, Save=false", config.LoggerSettings{ConsoleLevel: "WARN", FileLevel: "INFO"}, 0, -1},
		{"INFO disabled, Save=true", config.LoggerSettings{Save: true, ConsoleLevel: "ERROR", FileLevel: "WARN"}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			canaryPreserveProcessLogging(t)
			stdout := canaryCaptureStdout(t)
			l, err := logger.Setup(canaryLogKey, tc.settings)
			if err != nil {
				t.Fatalf("logger.Setup: %v", err)
			}
			t.Cleanup(l.Close)

			p, sink := observedPool(t, &fakePlacer{})
			p.streamers = []*models.Streamer{newTestStreamer(1000)}
			before := time.Now()
			p.handleMessage(parseCanaryFrame(t, canaryTopicChan1, frame))
			after := time.Now()

			// The only producer has returned; Close flushes the console queue
			// synchronously, and stdout has been drained all along.
			l.Close()
			console := stdout.finish()
			assertCanaryPrivate(t, console)
			lines := canaryLogLines(console)
			if len(lines) != tc.wantConsole {
				t.Fatalf("console lines = %d, want %d canary line(s) and nothing else:\n%s", len(lines), tc.wantConsole, console)
			}
			for _, line := range lines {
				assertCanaryTextLine(t, line, want, before, after)
			}
			if tc.wantFile >= 0 {
				data, err := os.ReadFile(logger.LogFilePath(canaryLogKey))
				if err != nil {
					t.Fatalf("read log file: %v", err)
				}
				assertCanaryPrivate(t, string(data))
				lines := canaryLogLines(string(data))
				if len(lines) != tc.wantFile {
					t.Fatalf("file lines = %d, want %d canary line(s) and nothing else:\n%s", len(lines), tc.wantFile, data)
				}
				for _, line := range lines {
					assertCanaryTextLine(t, line, want, before, after)
				}
			}
			if n := len(sink.all()); n != 1 {
				t.Fatalf("observations = %d, want the one channel_event whatever the log level", n)
			}
		})
	}
}

// TestWinnerCanaryAfterLoggerShutdownDropsQuietly shows the documented limit:
// once the application logger is closed, a later record is lost without a
// panic, and the business path still runs. (That a failed record is never
// retried is TestWinnerCanaryLogFailureIsNeitherRetriedNorSticky's to show.)
// It counts every line the logger wrote, so it runs in a fresh process.
func TestWinnerCanaryAfterLoggerShutdownDropsQuietly(t *testing.T) {
	if !canaryInFreshProcess(t) {
		return
	}
	t.Chdir(t.TempDir())
	canaryPreserveProcessLogging(t)
	stdout := canaryCaptureStdout(t)
	l, err := logger.Setup(canaryLogKey, config.LoggerSettings{Save: true, ConsoleLevel: "INFO", FileLevel: "INFO"})
	if err != nil {
		t.Fatalf("logger.Setup: %v", err)
	}
	t.Cleanup(l.Close)
	p, sink := observedPool(t, &fakePlacer{})
	p.streamers = []*models.Streamer{newTestStreamer(1000)}
	frame := canaryUpdateFrame("synthetic-event-0001", "RESOLVED", 310, 210, "synthetic-outcome-b")

	p.handleMessage(parseCanaryFrame(t, canaryTopicChan1, frame))
	l.Close()
	p.handleMessage(parseCanaryFrame(t, canaryTopicChan1, frame))

	if n := len(canaryLogLines(stdout.finish())); n != 1 {
		t.Fatalf("console lines = %d, want 1 (the record before shutdown)", n)
	}
	data, err := os.ReadFile(logger.LogFilePath(canaryLogKey))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if n := len(canaryLogLines(string(data))); n != 1 {
		t.Fatalf("file lines = %d, want 1 (the record before shutdown)", n)
	}
	if n := len(sink.all()); n != 2 {
		t.Fatalf("observations = %d, want 2: the business path ran for both frames", n)
	}
}

// --- Purity, statelessness, retention and bounded cost -------------------------

// TestWinnerCanaryNeitherMutatesNorRetainsItsInput checks that a parsed frame
// — its whole message, predictor data included — is left as it arrived, that
// nothing the record reports is read lazily from it, and that an earlier frame
// has no influence on a later one. Every field of the message holds a value,
// connection provenance included, so overwriting any of them would show.
func TestWinnerCanaryNeitherMutatesNorRetainsItsInput(t *testing.T) {
	canaryUseProcessLogger(t, &canaryCapture{level: slog.LevelInfo})
	malformed := canaryFrameAttrs{winnerPresence: "PRESENT", winnerJSONType: "string", outcomesPresence: "PRESENT",
		outcomeCount: 2, winnerMatchCount: -1, winnerIndex: -1, eventKeyHash: canaryHash0002, result: canaryMalformed}
	parsed := func() *PubSubMessage {
		return parseCanaryFrame(t, canaryTopicChan1, canaryUpdateFrame("synthetic-event-0001", "RESOLVED", 310, 210, "synthetic-outcome-b"))
	}
	frame := func() map[string]interface{} { return parsed().Data["event"].(map[string]interface{}) }
	msg := canaryStampConnection(parsed())
	for i, fields := 0, reflect.ValueOf(*msg); i < fields.NumField(); i++ {
		if fields.Field(i).IsZero() {
			t.Fatalf("control: message field %s is zero, so an overwrite with zero would go unseen", fields.Type().Field(i).Name)
		}
	}
	event := msg.Data["event"].(map[string]interface{})
	snapshot := canaryCloneMessage(msg)

	rec, ok := classifyWinnerCanary(msg, "RESOLVED", event, "synthetic-build")
	if !ok {
		t.Fatal("control frame refused")
	}
	logWinnerCanary(msg, "RESOLVED", event)
	if !reflect.DeepEqual(*msg, snapshot) || !reflect.DeepEqual(event, frame()) {
		t.Fatalf("the canary mutated its input")
	}

	// Rewrite the frame after the fact: a record that retained any part of it
	// would now report something else.
	event["id"] = "synthetic-event-0002"
	event["winning_outcome_id"] = ""
	event["outcomes"] = canaryOutcomes("synthetic-outcome-a", "synthetic-outcome-a")
	now := time.Now()
	assertCanaryFields(t, canaryFields(t, rec.attrs(now)), canaryPositive0001.values(), "synthetic-build", now, now)

	// Statelessness: A, then an unrelated malformed B, then A again.
	b := canaryEvent("synthetic-event-0002", "synthetic-outcome-a", canaryOutcomes("synthetic-outcome-a", "synthetic-outcome-a"))
	recB, _ := classifyWinnerCanary(msg, "RESOLVED", b, "synthetic-build")
	recA2, _ := classifyWinnerCanary(msg, "RESOLVED", frame(), "synthetic-build")
	assertCanaryFields(t, canaryFields(t, recB.attrs(now)), malformed.values(), "synthetic-build", now, now)
	assertCanaryFields(t, canaryFields(t, recA2.attrs(now)), canaryPositive0001.values(), "synthetic-build", now, now)
}

// canaryRetentionProbe is a heap object large enough to be tracked on its own
// by the garbage collector, so a weak pointer to it goes nil once nothing else
// holds it.
type canaryRetentionProbe struct {
	self *canaryRetentionProbe
	_    [64]byte
}

// canaryEmitWithProbes emits a record for one frame — a positive one, or one
// whose winner is an object — that holds a probe in its raw message, its data
// envelope, its event and every outcome, and inside an object winner; its event
// id, winner and outcome ids are strings in memory of their own. It returns
// only weak pointers: to the probes, and to the bytes of those strings.
//
//go:noinline
func canaryEmitWithProbes(objectWinner bool) ([]weak.Pointer[canaryRetentionProbe], []weak.Pointer[byte]) {
	var probes []weak.Pointer[canaryRetentionProbe]
	var texts []weak.Pointer[byte]
	probe := func() *canaryRetentionProbe {
		p := new(canaryRetentionProbe)
		p.self = p
		probes = append(probes, weak.Make(p))
		return p
	}
	// text copies s into an allocation long enough to be its own heap object.
	text := func(s string) string {
		c := strings.Clone(s + strings.Repeat("-", 64))
		texts = append(texts, weak.Make(unsafe.StringData(c)))
		return c
	}
	var winner interface{}
	if objectWinner {
		winner = map[string]interface{}{"id": text("synthetic-outcome-b"), "SENTINEL_PROBE": probe()}
	} else {
		winner = text("synthetic-outcome-b")
	}
	event := canaryEvent(text("synthetic-event-0001"), winner, []interface{}{
		map[string]interface{}{"id": text("synthetic-outcome-a"), "SENTINEL_PROBE": probe()},
		map[string]interface{}{"id": text("synthetic-outcome-b"), "SENTINEL_PROBE": probe()},
	})
	event["SENTINEL_PROBE"] = probe()
	msg := canaryEventMsg()
	msg.Data = map[string]interface{}{"event": event, "SENTINEL_PROBE": probe()}
	msg.Message = map[string]interface{}{"SENTINEL_PROBE": probe()}
	logWinnerCanary(msg, "RESOLVED", event)
	return probes, texts
}

// TestWinnerCanaryRetainsNothingAfterTheCall shows the canary keeps no
// reference into a frame. Each frame is checked on its own, before the next one
// is emitted, so keeping only the latest frame shows as well: once the emitter
// has returned and the caller has let the frame go, the garbage collector
// reclaims every probe it held — in the raw message, the data envelope, the
// event, each outcome and an object winner — and the bytes of every id and
// winner string. A copy of a string would not show here; that no call leaves
// state behind for the next is what the repeated and concurrent emission tests
// show.
func TestWinnerCanaryRetainsNothingAfterTheCall(t *testing.T) {
	capture := &canaryCapture{level: slog.LevelInfo}
	canaryUseProcessLogger(t, capture)
	for _, objectWinner := range []bool{false, true} {
		probes, texts := canaryEmitWithProbes(objectWinner)
		retained := func() (objectsLeft, textsLeft int) {
			for _, p := range probes {
				if p.Value() != nil {
					objectsLeft++
				}
			}
			for _, b := range texts {
				if b.Value() != nil {
					textsLeft++
				}
			}
			return objectsLeft, textsLeft
		}
		for i := 0; i < 5; i++ {
			if objectsLeft, textsLeft := retained(); objectsLeft+textsLeft == 0 {
				break
			}
			runtime.GC()
		}
		if objectsLeft, textsLeft := retained(); objectsLeft+textsLeft != 0 {
			t.Fatalf("the frame outlived the call (object winner: %v): %d of %d probes and %d of %d id strings are still reachable, so the canary kept a reference into it",
				objectWinner, objectsLeft, len(probes), textsLeft, len(texts))
		}
	}
	if n := len(capture.canaries()); n != 2 {
		t.Fatalf("records = %d, want 2", n)
	}
}

// TestWinnerCanaryCostIsBoundedByTheAdmissionLimits checks an admitted frame's
// cost against the contract's bounds: at every limit (64 outcomes of 4096-byte
// ids, a 4096-byte winner and event id) a call must stay far below what copying
// the admitted strings would cost (64 × 4096 bytes).
func TestWinnerCanaryCostIsBoundedByTheAdmissionLimits(t *testing.T) {
	ids := make([]interface{}, 64)
	for i := range ids {
		ids[i] = fmt.Sprintf("%02d", i) + strings.Repeat("y", 4094)
	}
	msg := canaryEventMsg()
	for _, event := range []map[string]interface{}{
		canaryEvent("synthetic-event-0001", ids[63], canaryOutcomes(ids...)),
		canaryEvent(strings.Repeat("e", 4096), ids[63], canaryOutcomes(ids...)),
	} {
		if rec, ok := classifyWinnerCanary(msg, "RESOLVED", event, "synthetic-build"); !ok || canaryClassifiedFields(t, rec)["result"] != canaryObserved {
			t.Fatal("control: every frame here must be admitted as a positive")
		}
		const runs = 200
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for i := 0; i < runs; i++ {
			classifyWinnerCanary(msg, "RESOLVED", event, "synthetic-build")
		}
		runtime.ReadMemStats(&after)
		if perCall := (after.TotalAlloc - before.TotalAlloc) / runs; perCall > 32<<10 {
			t.Fatalf("a frame at the limits allocates %d bytes per call; copying the admitted outcome ids alone would be %d", perCall, 64*4096)
		}
	}
}

// --- Properties over generated frames -----------------------------------------

// referenceWinnerCanary restates the owner contract rule by rule. It is the
// oracle for generated frames and shares no helper with the implementation.
func referenceWinnerCanary(topic TopicType, msgType, status string, event map[string]interface{}, build string) (map[string]interface{}, bool) {
	if topic != "predictions-channel-v1" || msgType != "event-updated" || status != "RESOLVED" || event == nil {
		return nil, false
	}
	// Resource admission, before anything else is decided.
	if len(build) > 4096 {
		return nil, false
	}
	if s, isStr := event["id"].(string); isStr && len(s) > 4096 {
		return nil, false
	}
	if s, isStr := event["winning_outcome_id"].(string); isStr && len(s) > 4096 {
		return nil, false
	}
	list, outcomesIsList := event["outcomes"].([]interface{})
	if outcomesIsList {
		if len(list) > 64 {
			return nil, false
		}
		for _, o := range list {
			if m, isObj := o.(map[string]interface{}); isObj {
				if s, isStr := m["id"].(string); isStr && len(s) > 4096 {
					return nil, false
				}
			}
		}
	}

	jsonType := func(v interface{}) string {
		switch v.(type) {
		case nil:
			return "null"
		case string:
			return "string"
		case float64:
			return "number"
		case bool:
			return "bool"
		case map[string]interface{}:
			return "object"
		case []interface{}:
			return "array"
		}
		return "other"
	}
	presence := func(key string, wellTyped bool) string {
		v, found := event[key]
		switch {
		case !found:
			return "ABSENT_ON_WIRE"
		case v == nil:
			return "NULL_ON_WIRE"
		case wellTyped:
			return "PRESENT"
		}
		return "INVALID"
	}

	out := map[string]interface{}{}
	winnerRaw, winnerFound := event["winning_outcome_id"]
	winner, winnerIsString := winnerRaw.(string)
	out["winner_field_presence"] = presence("winning_outcome_id", winnerIsString)
	if winnerFound {
		out["winner_field_json_type"] = jsonType(winnerRaw)
	} else {
		out["winner_field_json_type"] = "absent"
	}
	out["winner_value_empty"] = winnerIsString && winner == ""
	out["outcomes_presence"] = presence("outcomes", outcomesIsList)
	out["outcome_count"] = int64(-1)
	if outcomesIsList {
		out["outcome_count"] = int64(len(list))
	}

	valid := outcomesIsList && len(list) >= 2
	var ids []string
	for i := 0; valid && i < len(list); i++ {
		m, isObj := list[i].(map[string]interface{})
		id, isStr := m["id"].(string)
		if !isObj || !isStr || id == "" {
			valid = false
			break
		}
		for _, prev := range ids {
			if prev == id {
				valid = false
			}
		}
		ids = append(ids, id)
	}
	out["outcome_ids_valid"] = valid

	matches, index := int64(-1), int64(-1)
	if winnerIsString && winner != "" && valid {
		matches = 0
		for i, id := range ids {
			if id == winner {
				matches++
				index = int64(i)
			}
		}
		if matches != 1 {
			index = -1
		}
	}
	out["winner_match_count"], out["winner_index"] = matches, index

	eventID, idIsString := event["id"].(string)
	eventValid := idIsString && eventID != ""
	out["event_key_hash"] = "none"
	if eventValid {
		sum := sha256.Sum256([]byte("p4-winner-canary/v2\x00event\x00" + eventID))
		out["event_key_hash"] = "sha256:" + hex.EncodeToString(sum[:])
	}
	switch {
	case !eventValid:
		out["result"] = canaryMalformed
	case !winnerFound || winnerRaw == nil:
		out["result"] = canaryNotObserved
	case winnerIsString && winner != "" && valid && matches == 1:
		out["result"] = canaryObserved
	default:
		out["result"] = canaryMalformed
	}
	return out, true
}

// canaryGeneratedOutcome is an outcome object carrying the aggregate and
// predictor data a real frame has, so a canary that touched them is caught by
// the input snapshot.
func canaryGeneratedOutcome(r *rand.Rand, id interface{}) map[string]interface{} {
	m := map[string]interface{}{
		"title":        "SENTINEL-OUTCOME-TITLE",
		"total_points": float64(r.IntN(1000)),
		"total_users":  float64(r.IntN(50)),
		"top_predictors": []interface{}{
			map[string]interface{}{"user_display_name": "SENTINEL-PREDICTOR", "points": float64(r.IntN(500))},
		},
	}
	if !isCanaryAbsentKey(id) {
		m["id"] = id
	}
	return m
}

// genCanaryFrame draws one frame from a distribution concentrated on the
// candidate path and on every boundary the contract names. The envelope —
// topic, message type, status, build label — is perturbed rarely and
// independently; then half of the frames start as a well-formed round that
// receives at most one perturbation of its event, so a positive result is
// common rather than a rarity, and the other half draw every field from its
// boundary values.
func genCanaryFrame(r *rand.Rand) (topic TopicType, msgType, status string, event map[string]interface{}, build string) {
	pick := func(options ...interface{}) interface{} { return options[r.IntN(len(options))] }
	idPool := []string{"synthetic-outcome-a", "synthetic-outcome-b", "synthetic-outcome-c", "synthetic-outcome-d",
		"synthetic-outcome-e", "synthetic-outcome-f"}
	long := strings.Repeat("z", 4096)

	topic = TopicPredictionsChannel
	if r.IntN(12) == 0 {
		topic = pick(TopicPredictionsUser, TopicRaid).(TopicType)
	}
	msgType = "event-updated"
	if r.IntN(12) == 0 {
		msgType = pick("event-created", "", "EVENT-UPDATED").(string)
	}
	status = "RESOLVED"
	if r.IntN(8) == 0 {
		status = pick("RESOLVE_PENDING", "CANCELED", "ACTIVE", "LOCKED", "resolved", "").(string)
	}
	build = "synthetic-build"
	switch r.IntN(40) {
	case 0:
		build = strings.Repeat("v", 4096)
	case 1:
		build = strings.Repeat("v", 4097)
	}
	if r.IntN(40) == 0 {
		return topic, msgType, status, nil, build
	}

	if r.IntN(2) == 0 {
		n := 2 + r.IntN(5)
		perm := r.Perm(len(idPool))
		list := make([]interface{}, n)
		for i := range list {
			list[i] = canaryGeneratedOutcome(r, idPool[perm[i]])
		}
		event = canaryEvent("synthetic-event-0001", idPool[perm[r.IntN(n)]], list)
		switch r.IntN(6) {
		case 0:
			event["winning_outcome_id"] = pick(nil, "", " ", "synthetic-outcome-z", float64(1), long+"!")
		case 1:
			delete(event, "winning_outcome_id")
		case 2:
			list[r.IntN(n)] = pick(nil, "synthetic-outcome-a", map[string]interface{}{"id": pick(nil, "", float64(1), long+"!")})
		case 3:
			list[r.IntN(n)] = canaryGeneratedOutcome(r, list[0].(map[string]interface{})["id"])
		case 4:
			event["id"] = pick(nil, "", " ", float64(7), strings.Repeat("e", 4097))
		}
		return topic, msgType, status, event, build
	}

	outcomeID := func() interface{} {
		switch r.IntN(14) {
		case 0:
			return canaryAbsentKey{}
		case 1:
			return nil
		case 2:
			return ""
		case 3:
			return float64(r.IntN(3))
		case 4:
			return long
		case 5:
			if r.IntN(4) == 0 {
				return long + "!"
			}
			return idPool[0]
		}
		return idPool[r.IntN(len(idPool))]
	}
	outcome := func() interface{} {
		switch r.IntN(12) {
		case 0:
			return nil
		case 1:
			return "synthetic-outcome-a"
		case 2:
			return []interface{}{map[string]interface{}{"id": "synthetic-outcome-a"}}
		}
		return canaryGeneratedOutcome(r, outcomeID())
	}
	var outcomes interface{}
	switch r.IntN(10) {
	case 0:
		outcomes = canaryAbsentKey{}
	case 1:
		outcomes = nil
	case 2:
		outcomes = pick("synthetic-outcome-a", map[string]interface{}{"0": map[string]interface{}{"id": "synthetic-outcome-a"}}, float64(2))
	default:
		n := r.IntN(6)
		if r.IntN(10) == 0 {
			n = 62 + r.IntN(5) // 62..66 straddles the 64-outcome limit
		}
		list := make([]interface{}, n)
		for i := range list {
			list[i] = outcome()
		}
		outcomes = list
	}
	winner := pick(canaryAbsentKey{}, nil, "", " ", idPool[0], idPool[1], idPool[2], "synthetic-outcome-z",
		float64(1), true, map[string]interface{}{"id": idPool[0]}, []interface{}{idPool[0]}, long, long+"!")
	id := pick(canaryAbsentKey{}, nil, "", " ", "synthetic-event-0001", "synthetic-event-0002", float64(7),
		map[string]interface{}{"id": "synthetic-event-0001"}, strings.Repeat("e", 4096), strings.Repeat("e", 4097))
	return topic, msgType, status, canaryEvent(id, winner, outcomes), build
}

// TestWinnerCanaryMatchesTheContractModelOnGeneratedFrames compares the
// classifier with referenceWinnerCanary over seeded random frames, checks the
// cross-attribute invariants the contract implies, and checks every input —
// aggregates and predictor data included — is left unmodified.
func TestWinnerCanaryMatchesTheContractModelOnGeneratedFrames(t *testing.T) {
	counts := map[string]int{}
	for seed := uint64(1); seed <= 4; seed++ {
		r := rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))
		for i := 0; i < 5000; i++ {
			topic, msgType, status, event, build := genCanaryFrame(r)
			var snapshot interface{}
			if event != nil {
				snapshot = canaryCloneJSON(event)
			}
			msg := &PubSubMessage{Topic: NewTopic(topic, "synthetic-channel-4417"), Type: msgType}

			want, wantOK := referenceWinnerCanary(topic, msgType, status, event, build)
			rec, ok := classifyWinnerCanary(msg, status, event, build)
			if event != nil && !reflect.DeepEqual(event, snapshot) {
				t.Fatalf("seed %d frame %d: input mutated", seed, i)
			}
			if ok != wantOK {
				t.Fatalf("seed %d frame %d: admitted=%v, contract model says %v (event %.300v)", seed, i, ok, wantOK, event)
			}
			if !ok {
				if topic == TopicPredictionsChannel && msgType == "event-updated" && status == "RESOLVED" && event != nil {
					counts["refused by a bound"]++
				} else {
					counts["not a candidate"]++
				}
				continue
			}
			got := canaryFields(t, rec.attrs(time.Now()))
			for k, v := range want {
				if got[k] != v {
					t.Fatalf("seed %d frame %d: %s = %#v, contract model says %#v (event %.300v)", seed, i, k, got[k], v, event)
				}
			}
			if got["build_version"] != build {
				t.Fatalf("seed %d frame %d: build_version not carried", seed, i)
			}
			assertCanaryInvariants(t, got)
			counts[got["result"].(string)]++
		}
	}
	// Guard against a vacuous generator: every outcome class must be reached.
	t.Logf("classes reached: %v", counts)
	for _, class := range []string{"not a candidate", "refused by a bound", canaryObserved, canaryNotObserved, canaryMalformed} {
		if counts[class] < 100 {
			t.Fatalf("generator reached %q only %d times: %v", class, counts[class], counts)
		}
	}
}

// TestWinnerCanaryEmitterAgreesWithTheClassifierOnGeneratedFrames sends every
// generated frame through the emitter itself, with the frame's build label as
// internal/version.Version, into a lossless capture of every level. Each frame
// yields exactly what the classifier decides for it — one INFO record carrying
// the classifier's attributes, or nothing at all — so no frame shape the
// generator draws, with lists of up to 66 outcomes and ids of up to 4097 bytes,
// makes the emitter add, drop or alter a record. It counts every record the
// process logs, so it runs in a fresh process.
func TestWinnerCanaryEmitterAgreesWithTheClassifierOnGeneratedFrames(t *testing.T) {
	if !canaryInFreshProcess(t) {
		return
	}
	capture := &canaryCapture{level: canaryCaptureAll}
	canaryUseProcessLogger(t, capture) // its t.Setenv also refuses a parallel test
	prev := version.Version
	t.Cleanup(func() { version.Version = prev })

	emitted := 0
	for seed := uint64(1); seed <= 4; seed++ {
		r := rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))
		for i := 0; i < 5000; i++ {
			topic, msgType, status, event, build := genCanaryFrame(r)
			msg := &PubSubMessage{Topic: NewTopic(topic, "synthetic-channel-4417"), Type: msgType}
			rec, ok := classifyWinnerCanary(msg, status, event, build)
			if pending := capture.take(); len(pending) != 0 {
				t.Fatalf("seed %d frame %d: classifying alone logged %d record(s), first %q", seed, i, len(pending), pending[0].Message)
			}
			version.Version = build
			before := time.Now()
			logWinnerCanary(msg, status, event)
			after := time.Now()
			got := capture.take()
			if !ok {
				if len(got) != 0 {
					t.Fatalf("seed %d frame %d: the classifier refuses the frame, but the emitter logged %d record(s), first %q",
						seed, i, len(got), got[0].Message)
				}
				continue
			}
			if len(got) != 1 || got[0].Message != "p4_winner_canary" || got[0].Level != slog.LevelInfo {
				t.Fatalf("seed %d frame %d: %d record(s), want exactly one INFO p4_winner_canary", seed, i, len(got))
			}
			assertCanaryFields(t, canaryFields(t, canaryRecordAttrs(got[0])), canaryClassifiedFields(t, rec), build, before, after)
			emitted++
		}
	}
	if emitted < 5000 {
		t.Fatalf("only %d of 20000 generated frames were emitted", emitted)
	}
}

// assertCanaryInvariants are relations the contract implies between fields,
// independent of how any one of them is computed.
func assertCanaryInvariants(t *testing.T, f map[string]interface{}) {
	t.Helper()
	result := f["result"].(string)
	count, matches, index := f["outcome_count"].(int64), f["winner_match_count"].(int64), f["winner_index"].(int64)
	valid, empty := f["outcome_ids_valid"].(bool), f["winner_value_empty"].(bool)
	hash, presence := f["event_key_hash"].(string), f["winner_field_presence"].(string)
	fail := func(why string) { t.Fatalf("invariant violated (%s): %v", why, f) }
	if result == canaryObserved && (!valid || matches != 1 || index < 0 || index >= count || presence != "PRESENT" || empty || hash == "none") {
		fail("a positive needs a valid vector, one match, a present nonempty winner and a hashed event id")
	}
	if valid && count < 2 {
		fail("a valid vector has at least two outcomes")
	}
	if (count == -1) != (f["outcomes_presence"] != "PRESENT") {
		fail("outcome_count is -1 exactly when outcomes is not a present array")
	}
	if matches >= 0 && (!valid || presence != "PRESENT" || empty) {
		fail("a match count needs a valid vector and a present nonempty winner")
	}
	if (index >= 0) != (matches == 1) {
		fail("winner_index is set exactly when there is one match")
	}
	if empty && (presence != "PRESENT" || f["winner_field_json_type"] != "string") {
		fail("winner_value_empty only for a present string")
	}
	if hash != "none" && (!strings.HasPrefix(hash, "sha256:") || len(hash) != len("sha256:")+64) {
		fail("event_key_hash is none or sha256:<64 hex>")
	}
	if hash == "none" && result != canaryMalformed {
		fail("an unusable event id is always malformed")
	}
	if (presence == "ABSENT_ON_WIRE" || presence == "NULL_ON_WIRE") && hash != "none" && result != canaryNotObserved {
		fail("an absent or null winner on a usable event id is not observed")
	}
}

// FuzzWinnerCanaryClassifier runs arbitrary inner messages through the real
// parser and compares the classifier with the contract model. Its seed corpus
// is every testdata/winner_canary fixture, so plain `go test` replays them.
func FuzzWinnerCanaryClassifier(f *testing.F) {
	for _, c := range loadWinnerCanaryCases(f) {
		f.Add(c.Topic, []byte(c.Message))
	}
	f.Fuzz(func(t *testing.T, topic string, message []byte) {
		msg, err := ParsePubSubMessage(&WSData{Topic: topic, Message: string(message)})
		if err != nil {
			return
		}
		event, _ := msg.Data["event"].(map[string]interface{})
		status, _ := event["status"].(string)
		want, wantOK := referenceWinnerCanary(msg.Topic.Type, msg.Type, status, event, version.Version)
		rec, ok := classifyWinnerCanary(msg, status, event, version.Version)
		if ok != wantOK {
			t.Fatalf("admitted=%v, contract model says %v", ok, wantOK)
		}
		if !ok {
			return
		}
		got := canaryFields(t, rec.attrs(time.Now()))
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s = %#v, contract model says %#v", k, got[k], v)
			}
		}
		assertCanaryInvariants(t, got)
	})
}
