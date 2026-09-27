package pubsub

// Tests for the P4 winner-field canary (winner_canary.go).
//
// Oracles are independent of the implementation: the expected records are
// literals written from the owner contract into testdata/winner_canary and the
// tables below, and every event_key_hash literal was computed outside Go, with
// both `printf 'p4-winner-canary/v2\0event\0<id>' | sha256sum` and Python's
// hashlib, which agreed on every vector. referenceWinnerCanary restates the
// contract rule by rule for the generated-input properties; the fixtures and
// the literal admission cases keep it from being the only witness. One test is
// a deliberate exception:
// TestWinnerCanaryEmitterAgreesWithTheClassifierOnGeneratedFrames expects the
// record the classifier returns for the same frame, so it checks only that the
// emitter and the classifier agree; the oracles above judge the classifier.
//
// No test here judges the canary on elapsed time; the only time limits are
// watchdogs, reported as TIMEOUT / HARNESS_FAILURE. What the canary costs is
// characterized by the Benchmark functions at the end of this file, which go
// test runs only with -bench. The contract's cost-shaped rules — fixed-key
// reads, admission before any work on what is admitted, no state kept between
// calls — are properties of winner_canary.go's source, not of these tests.
// The two allocation checks, that refusing a frame allocates nothing and what
// classifying a frame at the limits allocates, are supplemental data about
// this implementation, not proofs of those rules.
//
// Tests that replace the process-wide slog default, os.Stdout, the working
// directory or internal/version.Version never run in parallel (t.Setenv and
// t.Chdir enforce that), and they put back everything they change.

import (
	"bytes"
	"cmp"
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

	// canaryWatchdog labels what a watchdog reports: a harness failure, never a
	// verdict on the canary.
	canaryWatchdog = "TIMEOUT / HARNESS_FAILURE"
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
func canaryFields(t testing.TB, attrs []slog.Attr) map[string]interface{} {
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
func assertCanaryFields(t testing.TB, got, want map[string]interface{}, build string, before, after time.Time) {
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

// canaryProbeHandler hands every record at or above level to onRecord on the
// emitting goroutine, holding no lock of its own, so a test can inspect the
// moment of emission.
type canaryProbeHandler struct {
	level    slog.Level
	onRecord func(slog.Record)
}

func (h canaryProbeHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

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
func canaryPreserveProcessLogging(t testing.TB) {
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
func canaryUseProcessLogger(t testing.TB, h slog.Handler) {
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
// is this test's own. Tests that call the canary from many goroutines at once
// need it too: a fatal concurrent map access, which no recover catches, then
// ends only the child, and the parent still reports the child's races under
// the test. env is added to the child's environment.
//
// The child runs with the parent's current GOMAXPROCS (so -cpu applies to it)
// and stops before the parent's deadline: its -test.timeout is a watchdog, and
// a child it stops is reported as TIMEOUT / HARNESS_FAILURE, not as a verdict.
// It is started directly, not through a go test -exec wrapper; its coverage is
// not collected; and it does not linger after its test returns, so a race that
// could only complete after that would go unreported — none of these tests
// leaves a goroutine running that reaches the canary (the real-handler test's
// round cleanups sleep for terminalCleanupGrace and then only remove the round).
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
		if bytes.Contains(out, []byte("panic: test timed out")) {
			t.Fatalf("%s: the fresh process for %s hit its watchdog deadline, which is no verdict on the canary: %v\n%s", canaryWatchdog, t.Name(), err, out)
		}
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

// canarySwitchedOffStreamer is a streamer with every setting off, the
// predictions toggle among them, whose status was never confirmed and which
// has no channel points; its identity is synthetic, as canaryStreamer's is.
func canarySwitchedOffStreamer() *models.Streamer {
	s := models.NewStreamer("synthetic-login-4418", models.StreamerSettings{})
	s.ChannelID = "synthetic-channel-4418"
	return s
}

// canaryHandlerStreamers are the streamers a frame is handled for: one in the
// state canaryStreamer describes, and one switched off
// (canarySwitchedOffStreamer). What the canary records follows from the frame
// alone, whatever the streamer's settings and state; a frame's subtest for the
// second streamer is named as for the first, followed by the suffix.
var canaryHandlerStreamers = []struct {
	suffix string
	make   func() *models.Streamer
}{
	{"", canaryStreamer},
	{" for a switched-off streamer", canarySwitchedOffStreamer},
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

// canaryWireMsg is the envelope of a frame of the given topic and message
// type, sent at timestamp on channelID, made the way the application makes
// one: parsed off the wire, then stamped with the provenance a connection
// gives every frame it delivers, so that each of its fields is set as a
// delivered frame's is.
func canaryWireMsg(topic TopicType, msgType, channelID, timestamp string) *PubSubMessage {
	message, _ := json.Marshal(map[string]interface{}{"type": msgType, // strings always marshal
		"data": map[string]interface{}{"timestamp": timestamp}})
	msg, err := ParsePubSubMessage(&WSData{Topic: NewTopic(topic, channelID).String(), Message: string(message)})
	if err != nil {
		panic(err) // a frame the parser accepts
	}
	return canaryStampConnection(msg)
}

// canaryMarkedMsg is the envelope of a frame of the given topic and message
// type sent at a fixed time on a marked channel id, for the tests that screen
// records for frame values.
func canaryMarkedMsg(topic TopicType, msgType string) *PubSubMessage {
	return canaryWireMsg(topic, msgType, "synthetic-channel-4417", "2026-09-23T12:10:00Z")
}

// canaryEventMsg is the marked envelope the classifier sees for a qualifying
// topic and message type.
func canaryEventMsg() *PubSubMessage {
	return canaryMarkedMsg(TopicPredictionsChannel, "event-updated")
}

// canaryLiveChannelID is a synthetic channel id of digits only. It carries no
// marker, so only the exact attribute checks keep it out of a record.
const canaryLiveChannelID = "44170001"

// canaryLiveMsg is the envelope of a frame of the given topic and message type
// sent now, on a synthetic channel id of digits only, stamped (canaryWireMsg);
// it claims nothing about the frames Twitch sends.
func canaryLiveMsg(topic TopicType, msgType string) *PubSubMessage {
	return canaryWireMsg(topic, msgType, canaryLiveChannelID, time.Now().UTC().Format(time.RFC3339Nano))
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

// canaryLiveEvent is event with more keys besides those the canary reads: a
// channel id, times (all now), a number and objects. The names and values are
// synthetic, not a record of what Twitch sends; they only give the frame more
// keys, of more value types. event's own keys win.
func canaryLiveEvent(event map[string]interface{}) map[string]interface{} {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	by := func() map[string]interface{} {
		return map[string]interface{}{"type": "USER", "user_id": canaryLiveChannelID,
			"user_display_name": "SENTINEL-BROADCASTER", "extension_client_id": nil}
	}
	live := map[string]interface{}{"channel_id": canaryLiveChannelID, "created_at": now, "created_by": by(),
		"ended_at": now, "ended_by": by(), "locked_at": now, "locked_by": by(), "prediction_window_seconds": 120.0}
	maps.Copy(live, event)
	return live
}

// canaryLiveOutcome is outcome with more keys besides those the canary reads,
// synthetic as canaryLiveEvent's are; outcome's own keys win.
func canaryLiveOutcome(outcome map[string]interface{}) map[string]interface{} {
	live := map[string]interface{}{"color": "BLUE", "total_points": 5000.0, "total_users": 7.0,
		"top_predictors": []interface{}{}, "badge": map[string]interface{}{"version": "blue-1", "set_id": "predictions"}}
	maps.Copy(live, outcome)
	return live
}

// canaryShape is how a frame is shaped besides the values the canary reads. A
// live-shaped frame is sent now, on a channel id of digits only
// (canaryLiveMsg), with more keys on the event and on each outcome
// (canaryLiveEvent, canaryLiveOutcome) — synthetic shapes that claim nothing
// about the frames Twitch sends. A minimal one has none of those keys, as most
// fixture frames have none, and is sent at a fixed time on a marked channel id
// (canaryMarkedMsg). Either way it is stamped by a connection, with its data
// and raw message as the parser leaves them. The tests that record frames of
// every kind, count what refusing allocates, watch what the canary reads or
// change a frame under it take frames of both shapes, so that a difference in
// what the canary does with frames that have, or lack, a live-shaped frame's
// keys is seen; a minimal frame's subtest is named as the live-shaped one's,
// followed by the shape's suffix.
type canaryShape struct{ live bool }

// canaryShapes are the two shapes, live first.
var canaryShapes = []canaryShape{{live: true}, {live: false}}

// suffix follows the name of a subtest of a frame of the shape: nothing for a
// live frame, ", minimal" for a minimal one.
func (s canaryShape) suffix() string {
	if s.live {
		return ""
	}
	return ", minimal"
}

// event is event of the shape: with the keys of a live one if the shape has
// them.
func (s canaryShape) event(event map[string]interface{}) map[string]interface{} {
	if s.live {
		return canaryLiveEvent(event)
	}
	return event
}

// outcome is outcome of the shape: with the keys of a live one if the shape
// has them.
func (s canaryShape) outcome(outcome map[string]interface{}) map[string]interface{} {
	if s.live {
		return canaryLiveOutcome(outcome)
	}
	return outcome
}

// outcomes is each of outcomes, which are all objects, of the shape.
func (s canaryShape) outcomes(outcomes []interface{}) []interface{} {
	shaped := make([]interface{}, len(outcomes))
	for i, outcome := range outcomes {
		shaped[i] = s.outcome(outcome.(map[string]interface{}))
	}
	return shaped
}

// frame is a frame of the given topic and message type about event, a
// RESOLVED event of the shape, in the shape's envelope, with its data and raw
// message as the parser leaves them — the raw message holding the data, the
// data the event — each completed by envelope.
func (s canaryShape) frame(topic TopicType, msgType string, event map[string]interface{}, envelope func(map[string]interface{}) map[string]interface{}) canaryFrame {
	var msg *PubSubMessage
	if s.live {
		msg = canaryLiveMsg(topic, msgType)
	} else {
		msg = canaryMarkedMsg(topic, msgType)
	}
	msg.Data = envelope(map[string]interface{}{"timestamp": msg.Data["timestamp"], "event": event})
	msg.Message = envelope(map[string]interface{}{"type": msg.Type, "data": msg.Data})
	return canaryFrame{msg: msg, status: "RESOLVED", event: event}
}

// qualifying is the qualifying frame about event, a RESOLVED event made of the
// shape here, with nothing added to its data envelope or raw message.
func (s canaryShape) qualifying(event map[string]interface{}) canaryFrame {
	return s.frame(TopicPredictionsChannel, "event-updated", s.event(event), canaryAsParsed)
}

// canaryAsParsed completes an envelope with nothing, leaving it as the parser
// does.
func canaryAsParsed(m map[string]interface{}) map[string]interface{} { return m }

func canaryClassifiedFields(t *testing.T, rec winnerCanaryRecord) map[string]interface{} {
	t.Helper()
	return canaryClassifiedFieldsAt(t, rec, time.Now())
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
// frame, stamped with the provenance a connection gives every frame it
// delivers, through the real predictions-channel handler for each of
// canaryHandlerStreamers, capturing every record at every level. A qualifying
// frame yields exactly one record of any kind — the canary's, matching the
// fixture's literal expectation — a non-candidate yields none, and no frame is
// modified. It counts every record the process logs, so it runs in a fresh
// process. The fixtures are loaded before that, in this process too, so go
// test's result cache notices when one changes.
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
	if len(cases) != 69 || records != 46 {
		t.Fatalf("loaded %d fixture cases (%d with a record), want 69 (46)", len(cases), records)
	}
	if !canaryInFreshProcess(t) {
		return
	}
	capture := &canaryCapture{level: canaryCaptureAll}
	canaryUseProcessLogger(t, capture)

	for _, c := range cases {
		for _, streamer := range canaryHandlerStreamers {
			t.Run(c.file+"/"+c.Name+streamer.suffix, func(t *testing.T) {
				p := newTestPool(&fakePlacer{})
				s := streamer.make()
				p.streamers = []*models.Streamer{s}
				msg := canaryStampConnection(parseCanaryFrame(t, c.Topic, c.Message))
				snapshot := canaryCloneMessage(msg)

				already := len(capture.all())
				before := time.Now()
				if panicked := canaryPanicOf(func() { p.handlePredictionChannel(msg, s) }); panicked != nil {
					t.Fatalf("handling the frame panicked: %v", panicked)
				}
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
			rec, ok, p := canaryClassifyCatching(canaryEventMsg(), "RESOLVED", tc.event, build)
			if p != nil {
				t.Fatalf("the classifier panicked: %v", p)
			}
			if tc.want == nil {
				if ok {
					t.Fatalf("an over-bound frame was admitted: %+v", canaryClassifiedFields(t, rec))
				}
				return
			}
			if !ok {
				t.Fatal("an in-bound frame was refused")
			}
			canaryAssertClassified(t, tc.want.values(), rec, ok, build)
		})
	}
}

// TestWinnerCanaryJudgesEveryOutcomeOfALongVector classifies admitted vectors
// of every length the contract admits, from two outcomes to sixty-four, one
// subtest per length. A flawless vector whose winner names the outcome at each
// position in turn is a positive at that index. A vector with one flaw at each
// position in turn — a null outcome, an outcome with no id, an empty id, an id
// repeating the first outcome's or the previous outcome's — is malformed
// whatever the winner (here the flawless vector's second id, wherever the flaw
// falls), since a vector is valid only when every outcome is an object with a
// nonempty id and no two ids are equal. A failure names the vector it judged
// and ends that length's subtest.
func TestWinnerCanaryJudgesEveryOutcomeOfALongVector(t *testing.T) {
	ids := make([]interface{}, 64)
	for i := range ids {
		ids[i] = fmt.Sprintf("synthetic-outcome-%02d", i)
	}
	records := newCanaryKindRecords()
	judge := func(t *testing.T, at int, what string, winner interface{}, outcomes []interface{}, want canaryFrameAttrs) {
		t.Helper()
		defer func() {
			if t.Failed() {
				t.Logf("the vector judged: outcome %d %s", at+1, what)
			}
		}()
		event := canaryEvent("synthetic-event-0001", winner, outcomes)
		rec, ok, p := canaryClassifyCatching(canaryEventMsg(), "RESOLVED", event, "synthetic-build")
		if p != nil {
			t.Fatalf("the classifier panicked: %v", p)
		}
		canaryAssertClassified(t, want.values(), rec, ok, "synthetic-build")
		if t.Failed() {
			t.FailNow() // a failing vector ends its length's subtest, so every failure reported is named
		}
	}
	type flaw struct {
		name    string
		outcome interface{}
	}
	for n := 2; n <= 64; n++ {
		t.Run(fmt.Sprintf("%d outcomes", n), func(t *testing.T) {
			invalid := records.invalidVector
			invalid.outcomeCount = int64(n)
			for at := 0; at < n; at++ {
				positive := records.positive
				positive.outcomeCount, positive.winnerIndex = int64(n), int64(at)
				judge(t, at, "the winner", ids[at], canaryOutcomes(ids[:n]...), positive)
				flaws := []flaw{
					{"null", nil},
					{"with no id", map[string]interface{}{"title": "SENTINEL-OUTCOME-TITLE"}},
					{"with an empty id", map[string]interface{}{"id": ""}},
				}
				if at >= 1 {
					flaws = append(flaws, flaw{"repeating the first", map[string]interface{}{"id": ids[0]}})
				}
				if at >= 2 {
					flaws = append(flaws, flaw{"repeating the previous", map[string]interface{}{"id": ids[at-1]}})
				}
				for _, f := range flaws {
					outcomes := canaryOutcomes(ids[:n]...)
					outcomes[at] = f.outcome
					judge(t, at, f.name, ids[1], outcomes, invalid)
				}
			}
		})
	}
}

// canaryRefusalCase is a frame far beyond a bound, and the build label to
// classify it with ("" for the environment's).
type canaryRefusalCase struct {
	name  string
	event func() map[string]interface{}
	build string
}

// label is the build label to classify tc's frame with in env.
func (tc canaryRefusalCase) label(env canaryEnvironment) string { return cmp.Or(tc.build, env.label) }

// canaryRefusalCases are the frames of one shape that
// TestWinnerCanaryRefusalTouchesNothing refuses and
// BenchmarkWinnerCanaryRefusals times, each built afresh by its event function.
func canaryRefusalCases(shape canaryShape) []canaryRefusalCase {
	huge := strings.Repeat("x", 1<<20)
	bigWinner := strings.Repeat("x", 4<<20)
	bigOther := strings.Repeat("x", 4<<20-1) + "y" // same length, differs only at the end
	longPrefix := strings.Repeat("x", 256<<10-2)
	longIDs := make([]string, 64) // differing only in their last two bytes
	for i := range longIDs {
		longIDs[i] = longPrefix + fmt.Sprintf("%02d", i)
	}
	// numbered are n outcome objects of the shape, the ith carrying id(i).
	numbered := func(n int, id func(i int) interface{}) []interface{} {
		out := make([]interface{}, n)
		for i := range out {
			out[i] = shape.outcome(map[string]interface{}{"id": id(i)})
		}
		return out
	}
	pair := func() []interface{} { return shape.outcomes(canaryPair()) }
	return []canaryRefusalCase{
		{"a 1 MiB event id", func() map[string]interface{} { return canaryEvent(huge, "synthetic-outcome-b", pair()) }, ""},
		{"a 1 MiB winner", func() map[string]interface{} { return canaryEvent("synthetic-event-0001", huge, pair()) }, ""},
		{"a 1 MiB id in the last of 64 outcomes", func() map[string]interface{} {
			return canaryEvent("synthetic-event-0001", "synthetic-outcome-a",
				shape.outcomes(append(canaryOutcomes(make([]interface{}, 63)...), map[string]interface{}{"id": huge})))
		}, ""},
		{"8192 distinct outcome ids", func() map[string]interface{} {
			return canaryEvent("synthetic-event-0001", "synthetic-outcome-1",
				numbered(1<<13, func(i int) interface{} { return "synthetic-outcome-" + strconv.Itoa(i) }))
		}, ""},
		{"1,048,576 in-bound outcome objects", func() map[string]interface{} {
			outcome := shape.outcome(map[string]interface{}{"id": "synthetic-outcome-a"})
			return canaryEvent("synthetic-event-0001", "synthetic-outcome-a", slices.Repeat([]interface{}{outcome}, 1<<20))
		}, ""},
		{"a 4 MiB winner beside 4096 same-length outcome ids", func() map[string]interface{} {
			return canaryEvent("synthetic-event-0001", bigWinner, numbered(1<<12, func(int) interface{} { return bigOther }))
		}, ""},
		{"64 outcomes whose 256 KiB ids differ only at the end", func() map[string]interface{} {
			return canaryEvent("synthetic-event-0001", "synthetic-outcome-a",
				numbered(64, func(i int) interface{} { return longIDs[i] }))
		}, ""},
		{"a 1 MiB build label", func() map[string]interface{} {
			return canaryEvent("synthetic-event-0001", "synthetic-outcome-b", pair())
		}, huge},
	}
}

// TestWinnerCanaryRefusalTouchesNothing refuses frames far beyond every bound,
// of both shapes (canaryShape), in every environment canaryEnvironments lists,
// each frame built afresh (canaryRefusalCases): an over-long event id, winner,
// outcome id or build label; more outcomes than the bound, as 8192 distinct
// ids or as 1,048,576 in-bound objects; a 4 MiB winner beside 4096
// same-length ids; and 64 outcomes whose 256 KiB ids differ only at the end.
// Each frame must be refused, and refusing it must allocate nothing
// (testing.AllocsPerRun): supplemental data about this implementation, which
// refuses without allocating, not a proof of what happens before admission. A
// case that fails in one environment is not tried in those after it. That the
// refusal comes before any copy, hash, comparison or traversal of what it
// refuses is a property of winner_canary.go's source, which no call here
// measures; BenchmarkWinnerCanaryRefusals characterizes what refusing each
// frame costs.
func TestWinnerCanaryRefusalTouchesNothing(t *testing.T) {
	for _, shape := range canaryShapes {
		for _, tc := range canaryRefusalCases(shape) {
			t.Run(tc.name+shape.suffix(), func(t *testing.T) {
				canaryInEachEnvironment(t, "", func(t *testing.T, env canaryEnvironment) {
					env.use(t, &canaryCapture{level: env.floor})
					build := tc.label(env)
					f := shape.qualifying(tc.event())
					if _, ok, p := canaryClassifyCatching(f.msg, f.status, f.event, build); p != nil {
						t.Fatalf("the classifier panicked: %v", p)
					} else if ok {
						t.Fatal("an over-bound frame was admitted")
					}
					var allocs float64
					if p := canaryPanicOf(func() {
						allocs = testing.AllocsPerRun(20, func() { classifyWinnerCanary(f.msg, f.status, f.event, build) })
					}); p != nil {
						t.Fatalf("the classifier panicked refusing the frame again: %v", p)
					}
					if allocs != 0 {
						t.Fatalf("refusing the frame allocated %v times per call; this implementation refuses without allocating", allocs)
					}
				})
			})
		}
	}
}

// canaryKindRecords holds the record of each kind of frame the tests build,
// from the contract's field rules: the positive record,
// canaryPositive0001, with the fields that kind changes. All carry
// synthetic-event-0001's hash, except noEventID and noEventIDNoWinner, which
// have none.
type canaryKindRecords struct {
	positive, firstMatched, noWinner, nullWinner, emptyWinner, unmatched canaryFrameAttrs
	noEventID, invalidVector, noOutcomes, nullOutcomes, invalidOutcomes  canaryFrameAttrs
	singleOutcome, emptyOutcomes, noEventIDNoWinner                      canaryFrameAttrs
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
	k.noEventIDNoWinner = k.noWinner
	k.noEventIDNoWinner.eventKeyHash, k.noEventIDNoWinner.result = "none", canaryMalformed
	k.invalidVector = k.positive
	k.invalidVector.outcomeIDsValid = false
	k.invalidVector.winnerMatchCount, k.invalidVector.winnerIndex, k.invalidVector.result = -1, -1, canaryMalformed
	k.singleOutcome = k.invalidVector
	k.singleOutcome.outcomeCount = 1
	k.emptyOutcomes = k.invalidVector
	k.emptyOutcomes.outcomeCount = 0
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
	// predictors, nest and ownOutcomes are an outcome's predictors, nested
	// object and own list under "outcomes", the key the event keeps its
	// outcomes under, and envelope completes the message's data envelope and
	// raw message.
	predictors  func() []interface{}
	nest        func() map[string]interface{}
	ownOutcomes func() []interface{}
	envelope    func(map[string]interface{}) map[string]interface{}
	// overlong is frame n's value beyond the 4096-byte bound where a refused
	// frame's event id, winner or outcome id belongs, and uncounted is the
	// outcomes of a frame refused for their count, count slots all holding
	// outcome, of which the canary may read the length alone.
	overlong  func(n int) string
	uncounted func(outcome map[string]interface{}, count int) []interface{}
}

// canaryRepeated is count slots, all holding outcome.
func canaryRepeated(outcome map[string]interface{}, count int) []interface{} {
	return slices.Repeat([]interface{}{outcome}, count)
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
// each branch it can take on the fields of a decoded frame — the winner, the
// event id, the outcomes, how many there are (fewer than two is a branch of
// its own) and each outcome's id — one for each way a frame is not a candidate
// and one for each bound that refuses a frame; the event id comes in every
// JSON type, and as an array beside no winner. Two branches are not kinds of
// frame: the build label's bound, since the label is no part of a frame (the
// tests that take this table refuse frames with a label beyond it), and a
// value of a type json.Unmarshal never produces, which no frame the pool
// decodes holds (TestWinnerCanaryNonJSONValuesAreClassifiedNotFormatted).
// Every frame is on its own topic and message type, and every kind comes in
// both shapes (canaryShape): first each live, then each again minimal, named
// with ", minimal". Frame n has its own ids — synthetic-event-000n and its
// outcomes' — so frames built for different n share none; the records carry
// the hash of synthetic-event-0001, the event id of n = 1.
func canaryFrameKinds() []canaryFrameKind {
	var kinds []canaryFrameKind
	for _, shape := range canaryShapes {
		for _, kind := range canaryShapedFrameKinds(shape) {
			kind.name += shape.suffix()
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// canaryShapedFrameKinds is canaryFrameKinds' table for frames of one shape.
func canaryShapedFrameKinds(shape canaryShape) []canaryFrameKind {
	records := newCanaryKindRecords()
	absent := canaryAbsentKey{}
	id := func(n int) string { return fmt.Sprintf("synthetic-event-%04d", n) }
	a := func(n int) string { return fmt.Sprintf("synthetic-outcome-a-%04d", n) }
	b := func(n int) string { return fmt.Sprintf("synthetic-outcome-b-%04d", n) }
	outcome := func(p canaryParts, outcomeID interface{}) map[string]interface{} {
		uninspected := map[string]interface{}{"top_predictors": p.predictors(), "SENTINEL_NEST": p.nest(), "outcomes": p.ownOutcomes()}
		return p.inspected(canarySet(shape.outcome(uninspected), "id", outcomeID))
	}
	// pair is two outcomes: the first with firstID (none for absent), the
	// second naming b(n).
	pair := func(p canaryParts, n int, firstID interface{}) []interface{} {
		return []interface{}{outcome(p, firstID), outcome(p, b(n))}
	}
	// frameOn is a frame of the given topic and message type about a RESOLVED
	// event with the given id, winner and outcomes; frame is a qualifying one.
	frameOn := func(topic TopicType, msgType string) func(p canaryParts, eventID, winner, outcomes interface{}) canaryFrame {
		return func(p canaryParts, eventID, winner, outcomes interface{}) canaryFrame {
			return shape.frame(topic, msgType, p.inspected(shape.event(canaryEvent(eventID, winner, outcomes))), p.envelope)
		}
	}
	frame := frameOn(TopicPredictionsChannel, "event-updated")
	// positiveOn is frame n of the positive kind on the given topic and
	// message type; positive, on the qualifying ones, is before its kind
	// changes it.
	positiveOn := func(topic TopicType, msgType string) func(canaryParts, int) canaryFrame {
		on := frameOn(topic, msgType)
		return func(p canaryParts, n int) canaryFrame { return on(p, id(n), b(n), pair(p, n, a(n))) }
	}
	positive := positiveOn(TopicPredictionsChannel, "event-updated")
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
	overlong := func(p canaryParts, n int) interface{} { return p.overlong(n) }
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
		{name: "boolean event id", want: records.noEventID, build: eventID(value(true))},
		{name: "array event id and no winner", want: records.noEventIDNoWinner,
			build: func(p canaryParts, n int) canaryFrame { return frame(p, p.array(), absent, pair(p, n, a(n))) }},
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
		{name: "a single outcome", want: records.singleOutcome, build: outcomes(func(p canaryParts, n int) interface{} {
			return []interface{}{outcome(p, b(n))}
		})},
		{name: "empty outcomes", want: records.emptyOutcomes, build: outcomes(value([]interface{}{}))},
		{name: "another topic", none: true, build: positiveOn(TopicPredictionsUser, "event-updated")},
		{name: "another message type", none: true, build: positiveOn(TopicPredictionsChannel, "event-created")},
		{name: "an unresolved round", none: true, build: changed(func(f *canaryFrame) {
			f.status = "ACTIVE"
			f.event["status"] = "ACTIVE"
		})},
		{name: "no event object", none: true, build: changed(func(f *canaryFrame) { f.event = nil })},
		{name: "no message", none: true, build: changed(func(f *canaryFrame) { f.msg = nil })},
		{name: "an overlong event id", none: true, build: eventID(overlong)},
		{name: "an overlong winner", none: true, build: winner(overlong)},
		{name: "an overlong outcome id", none: true, build: first(overlong)},
		{name: "more than 64 outcomes", none: true, build: outcomes(func(p canaryParts, n int) interface{} {
			return p.uncounted(outcome(p, a(n)), 65)
		})},
	}
}

// canaryAssertClassified checks the classifier's answer for a frame classified
// with the build label label: the record want, or, for a nil want, a refusal.
// A classified record has no receive time of its own, so it is given one here.
func canaryAssertClassified(t *testing.T, want map[string]interface{}, rec winnerCanaryRecord, ok bool, label string) {
	t.Helper()
	switch {
	case ok && want == nil:
		t.Fatal("the frame was admitted")
	case !ok && want != nil:
		t.Fatal("the frame was refused")
	case ok:
		now := time.Now()
		assertCanaryFields(t, canaryClassifiedFieldsAt(t, rec, now), want, label, now, now)
	}
}

// canaryAssertEmitted checks what was logged during count emissions of a
// frame: count INFO p4_winner_canary records, each the record want, or, for a
// nil want, nothing at all.
func canaryAssertEmitted(t testing.TB, want map[string]interface{}, logged []slog.Record, count int, before, after time.Time) {
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

// canaryBestOf times runs calls of f and returns the fastest, for a
// benchmark's baseline, which no test judges.
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

// canaryLogFloors are the floors the application's logger can have, a floor
// being the lowest level it lets a record through at: each level
// internal/logger reads from its settings, since the fanout of its console and
// file handlers lets a record through when either would. DEBUG is the default
// (Save=true, the file at DEBUG); INFO is the console alone at its default
// level (Save=false), with DEBUG off; at WARN or ERROR the canary's records,
// which it logs at INFO, are dropped once it has classified the frame.
var canaryLogFloors = []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError}

// canaryBuildLabels are build labels of four of the shapes the build paths give
// internal/version — a stable release's canonical version, the Makefile's git
// describe of a dirty tree after a tag and of a tree with no tag to describe
// from, and dev, which a build without ldflags keeps — the labels
// canaryEnvironments pairs with every floor. canaryBuildPathLabels has every
// shape.
var canaryBuildLabels = []string{"0.3.1", "v0.3.1-7-g1a2b3c4-dirty", "1a2b3c4", "dev"}

// canaryBuildPathLabels are a build label of every shape the repository's build
// paths give internal/version: a stable release's canonical X.Y.Z (the
// stable-release workflow's validated version), what the Makefile's
// `git describe --tags --always --dirty` gives for a tagged commit, for its
// dirty tree, for a clean and a dirty tree after a tag and for a clean and a
// dirty tree with no tag to describe from, CI's ci-<commit>, and dev, which a
// build without ldflags, the Makefile's fallback and the Dockerfile's default
// give. The tag and commit names are synthetic; only the shapes are the build
// paths'. A Docker build argument can set any label at all, so no list is
// complete: that what the canary does cannot depend on a label's content is a
// property of winner_canary.go's source.
var canaryBuildPathLabels = []string{
	"0.3.1", "v0.3.1", "v0.3.1-dirty", "v0.3.1-7-g1a2b3c4", "v0.3.1-7-g1a2b3c4-dirty", "1a2b3c4", "1a2b3c4-dirty",
	"ci-0123456789abcdef0123456789abcdef01234567", "dev",
}

// canaryEnvironment is what the application gives the canary besides a frame:
// its logger's floor and its build label. The tests that call
// canaryEnvironments or canaryInEachEnvironment run it in each of the sixteen
// environments canaryEnvironments lists, so that a difference in what it does
// in some of them is seen; TestWinnerCanaryRecordsCarryEveryBuildPathLabel
// runs it with the other label shapes of canaryBuildPathLabels.
type canaryEnvironment struct {
	floor slog.Level
	label string
}

// canaryEnvironments pairs every floor with every label.
func canaryEnvironments() []canaryEnvironment {
	var envs []canaryEnvironment
	for _, floor := range canaryLogFloors {
		for _, label := range canaryBuildLabels {
			envs = append(envs, canaryEnvironment{floor, label})
		}
	}
	return envs
}

func (env canaryEnvironment) name() string { return env.floor.String() + " " + env.label }

// use makes env the process's for the rest of t: h, a handler that lets
// through the records at or above env's floor, becomes the slog default, and
// env's label internal/version's, which the emitter passes the classifier.
func (env canaryEnvironment) use(t testing.TB, h slog.Handler) {
	t.Helper()
	canaryUseProcessLogger(t, h) // its t.Setenv also refuses a parallel test
	prev := version.Version
	version.Version = env.label
	t.Cleanup(func() { version.Version = prev })
}

// wantLogged is the record the emitter must log in env for a frame whose
// record is want: want itself, or none when INFO, the canary's level, is below
// the floor.
func (env canaryEnvironment) wantLogged(want map[string]interface{}) map[string]interface{} {
	if env.floor > slog.LevelInfo {
		return nil
	}
	return want
}

// canaryInEachEnvironment runs body as a subtest in each environment
// canaryEnvironments lists, named as the environment followed by suffix, and
// stops at the first that fails: the environments after it would add time,
// not a verdict.
func canaryInEachEnvironment(t *testing.T, suffix string, body func(t *testing.T, env canaryEnvironment)) {
	for _, env := range canaryEnvironments() {
		if !t.Run(env.name()+suffix, func(t *testing.T) { body(t, env) }) {
			return
		}
	}
}

// canaryLargeParts are frame parts large wherever the canary has no reason to
// read: 65,536 extra keys on the event, in every outcome object, in the data
// envelope and raw message and in every object standing where the winner, the
// event id, the outcomes or an outcome's id belongs; 1,048,576 elements in
// every array standing there and in every outcome's predictors and own
// outcomes list; 65,536 levels of nesting in every outcome; a 32 MiB title on
// the event and on every outcome; and a 32 MiB value, with frame n's suffix,
// where a refused frame is beyond its bound. It also returns the extra keys
// and the 32 MiB text, which is the long build label too.
func canaryLargeParts() (canaryParts, map[string]interface{}, string) {
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
	longText := strings.Repeat("x", 1<<25)
	deep := map[string]interface{}{}
	for level, m := 0, deep; level < extra; level++ {
		next := map[string]interface{}{}
		m["SENTINEL_NEST"] = next
		m = next
	}
	return canaryParts{
		inspected:   func(m map[string]interface{}) map[string]interface{} { m["title"] = longText; return widen(m) },
		object:      widen,
		array:       func() []interface{} { return big },
		predictors:  func() []interface{} { return big },
		nest:        func() map[string]interface{} { return deep },
		ownOutcomes: func() []interface{} { return big },
		envelope:    widen,
		overlong:    func(n int) string { return longText + fmt.Sprintf("-%04d", n) },
		uncounted:   canaryRepeated,
	}, extraKeys, longText
}

// TestWinnerCanaryRecordsLargeFramesOfEveryKind hands the canary one frame of
// every kind canaryFrameKinds lists — each branch it can take, each way a frame
// is not a candidate and each bound that refuses one — large wherever the
// canary has no reason to read (canaryLargeParts), in every environment
// canaryEnvironments lists: through the classifier, with the environment's
// build label, through the classifier once more with a build label beyond its
// bound, which refuses a frame of any kind (one that is not a candidate before
// the label is read), and through the emitter the pool calls. Each record,
// classified and emitted, must be its kind's literal expectation, or there
// must be none, whatever the frame holds beside what the canary reads. The
// environments are taken in canaryEnvironments' order, and a kind that fails
// in one is not tried in those after it. BenchmarkWinnerCanaryFixedKeyCalls
// characterizes what these calls cost beside a walk over the extra keys.
func TestWinnerCanaryRecordsLargeFramesOfEveryKind(t *testing.T) {
	parts, _, longText := canaryLargeParts()
	for _, kind := range canaryFrameKinds() {
		t.Run(kind.name, func(t *testing.T) {
			// Earlier kinds' frames are garbage now: collecting them before this
			// kind's frame is built bounds the peak memory.
			runtime.GC()
			f := kind.build(parts, 1)
			want := kind.record()
			canaryInEachEnvironment(t, "", func(t *testing.T, env canaryEnvironment) {
				capture := &canaryCapture{level: env.floor}
				env.use(t, capture)
				canaryAssertRecorded(t, env, capture, f, want, longText)
			})
		})
	}
}

// canaryAssertRecorded hands f to the classifier with env's build label, which
// must give want (canaryAssertClassified), and with beyond, a build label
// beyond its bound, which must refuse it; then to the emitter, logging into
// capture, which must log want in env (canaryAssertEmitted). None of the calls
// may panic.
func canaryAssertRecorded(t *testing.T, env canaryEnvironment, capture *canaryCapture, f canaryFrame, want map[string]interface{}, beyond string) {
	t.Helper()
	rec, ok, p := canaryClassifyCatching(f.msg, f.status, f.event, env.label)
	if p != nil {
		t.Fatalf("the classifier panicked: %v", p)
	}
	canaryAssertClassified(t, want, rec, ok, env.label)
	_, admitted, p := canaryClassifyCatching(f.msg, f.status, f.event, beyond)
	if p != nil {
		t.Fatalf("the classifier panicked on a build label beyond its bound: %v", p)
	}
	if admitted {
		t.Error("the frame was admitted with a build label beyond its bound")
	}
	before := time.Now()
	if p := canaryPanicOf(func() { logWinnerCanary(f.msg, f.status, f.event) }); p != nil {
		t.Fatalf("the emitter panicked: %v", p)
	}
	// Only the canary's records count here: in the main test process, a
	// client another test left behind can log at any time.
	logged := slices.DeleteFunc(capture.take(), func(r slog.Record) bool { return r.Message != "p4_winner_canary" })
	canaryAssertEmitted(t, env.wantLogged(want), logged, 1, before, time.Now())
}

// canarySmallParts are frame parts as small as a frame of the kind can be:
// a short title, an empty predictors list, own outcomes list and nested
// object, a two-element array and the object as given where one stands in,
// the envelope as parsed, and a value just beyond the 4096-byte bound, with
// frame n's suffix, where a refused frame is beyond it.
func canarySmallParts() canaryParts {
	return canaryParts{
		inspected:   func(m map[string]interface{}) map[string]interface{} { m["title"] = "SENTINEL-TITLE"; return m },
		object:      func(m map[string]interface{}) map[string]interface{} { return m },
		array:       func() []interface{} { return []interface{}{true, 1.0} },
		predictors:  func() []interface{} { return []interface{}{} },
		nest:        func() map[string]interface{} { return map[string]interface{}{} },
		ownOutcomes: func() []interface{} { return []interface{}{} },
		envelope:    canaryAsParsed,
		overlong:    func(n int) string { return strings.Repeat("x", 4097) + fmt.Sprintf("-%04d", n) },
		uncounted:   canaryRepeated,
	}
}

// canaryWantOf is the record frame f of kind must yield — its kind's literal
// expectation with the hash of the frame's own event id, which the contract
// model computes — or nil when it must yield none.
func canaryWantOf(kind canaryFrameKind, f canaryFrame) map[string]interface{} {
	record := kind.record()
	if record != nil && kind.want.eventKeyHash != "none" {
		model, _ := referenceWinnerCanary(f.msg.Topic.Type, f.msg.Type, f.status, f.event, "synthetic-build")
		record["event_key_hash"] = model["event_key_hash"]
	}
	return record
}

// canaryOutcomesHeader is the address, length and capacity of event's
// outcomes list.
func canaryOutcomesHeader(event map[string]interface{}) [3]uintptr {
	list, _ := event["outcomes"].([]interface{})
	return [3]uintptr{uintptr(unsafe.Pointer(unsafe.SliceData(list))), uintptr(len(list)), uintptr(cap(list))}
}

// canaryAssertInputUnchanged runs calls on f and checks that they left f as it
// was: its message and event equal deep copies taken before, and its outcomes
// list keeps its address, length and capacity.
func canaryAssertInputUnchanged(t *testing.T, f canaryFrame, calls func()) {
	t.Helper()
	var msg *PubSubMessage // nil for a nil message
	if f.msg != nil {
		m := canaryCloneMessage(f.msg)
		msg = &m
	}
	event, outcomes := canaryCloneJSON(f.event), canaryOutcomesHeader(f.event)
	calls()
	if !reflect.DeepEqual(msg, f.msg) {
		t.Error("the canary changed the message it was handed")
	}
	if !reflect.DeepEqual(event, f.event) {
		t.Error("the canary changed the event it was handed")
	}
	if after := canaryOutcomesHeader(f.event); after != outcomes {
		t.Errorf("the event's outcomes list was %v (address, length, capacity) and is now %v", outcomes, after)
	}
}

// canaryRecordFramesIn hands the classifier and the emitter a small frame
// (canarySmallParts) of every kind canaryFrameKinds lists in each environment
// of envs, every frame built afresh with ids of its own. Each record,
// classified and emitted, must be its kind's literal expectation with the
// hash of the frame's own event id, or there must be none (none at all logged
// above INFO); a build label beyond its bound must refuse the frame; and the
// calls must leave what they are handed unchanged (canaryAssertInputUnchanged).
func canaryRecordFramesIn(t *testing.T, envs []canaryEnvironment) {
	kinds := canaryFrameKinds()
	parts := canarySmallParts()
	beyond := strings.Repeat("x", 4097) // a build label beyond its bound
	for e, env := range envs {
		t.Run(env.name(), func(t *testing.T) {
			capture := &canaryCapture{level: env.floor}
			env.use(t, capture)
			for k, kind := range kinds {
				t.Run(kind.name, func(t *testing.T) {
					f := kind.build(parts, 1+e*len(kinds)+k)
					canaryAssertInputUnchanged(t, f, func() {
						canaryAssertRecorded(t, env, capture, f, canaryWantOf(kind, f), beyond)
					})
				})
			}
		})
	}
}

// TestWinnerCanaryRecordsFramesInEveryLoggerFloorAndBuildLabel runs
// canaryRecordFramesIn in each of the sixteen environments canaryEnvironments
// lists: every floor the application's logger can have with every label of
// canaryBuildLabels.
func TestWinnerCanaryRecordsFramesInEveryLoggerFloorAndBuildLabel(t *testing.T) {
	canaryRecordFramesIn(t, canaryEnvironments())
}

// TestWinnerCanaryRecordsCarryEveryBuildPathLabel runs canaryRecordFramesIn at
// every floor with each label shape of canaryBuildPathLabels that
// canaryBuildLabels leaves out.
func TestWinnerCanaryRecordsCarryEveryBuildPathLabel(t *testing.T) {
	var envs []canaryEnvironment
	for _, floor := range canaryLogFloors {
		for _, label := range canaryBuildPathLabels {
			if !slices.Contains(canaryBuildLabels, label) {
				envs = append(envs, canaryEnvironment{floor, label})
			}
		}
	}
	canaryRecordFramesIn(t, envs)
}

// TestWinnerCanaryLeavesUninspectedValuesUnread builds one frame of every kind
// canaryFrameKinds lists, in every environment canaryEnvironments lists, and
// lets other goroutines write into every value in it the canary has no reason
// to read — the message's data envelope and raw message, every outcome's
// predictors, nested object and own outcomes list, an object or array standing
// where the winner, the event id, the outcomes, an outcome or an outcome's id
// belongs, and every outcome and every slot of the outcomes of a frame refused
// for their count — while the canary classifies and emits the frame, twice
// each, and refuses it once for a build label beyond its bound. Nothing orders
// those writes against the canary, so under the race detector (go test -race,
// as CI and the quality gates run it) reading what any of those values holds —
// a map entry, the keys of a map in a walk, or an array element — is a reported
// data race, however quick; an array's length alone is not, since it sits in
// the slice header rather than in what the writers write. The event and the
// other frames' outcome objects, which the canary does read by key, are not
// watched: that it reads them by fixed keys only is a property of
// winner_canary.go's source. The test runs in a fresh process, so that no
// earlier test has already made the canary's first call of the process or of
// a branch, and every frame stays alive until the end, so that no two, in any
// environment, share an address. Each record, classified and emitted, is
// checked against its kind's literal expectation, or that there is none, so a
// frame that stopped reaching its case fails; without -race that is all this
// test checks.
func TestWinnerCanaryLeavesUninspectedValuesUnread(t *testing.T) {
	if !canaryInFreshProcess(t) {
		return
	}
	beyond := strings.Repeat("x", 4097) // a build label beyond its bound
	var alive []canaryFrame
	for _, env := range canaryEnvironments() {
		t.Run(env.name(), func(t *testing.T) {
			capture := &canaryCapture{level: env.floor}
			env.use(t, capture)
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
						inspected:   func(m map[string]interface{}) map[string]interface{} { m["title"] = "SENTINEL-TITLE-3b8f"; return m },
						object:      watched,
						array:       list,
						predictors:  list,
						nest:        func() map[string]interface{} { return watched(map[string]interface{}{}) },
						ownOutcomes: list,
						envelope:    watched,
						overlong:    func(n int) string { return strings.Repeat("x", 4096) + fmt.Sprintf("-%04d", n) },
						uncounted: func(outcome map[string]interface{}, count int) []interface{} {
							// Refused for its count, the frame has nothing in its
							// outcomes for the canary to read, not even the one
							// outcome every slot holds.
							l := canaryRepeated(watched(outcome), count)
							for i := range l {
								writes = append(writes, func() { l[i] = "SENTINEL_WRITE" })
							}
							return l
						},
					}, 1)
					alive = append(alive, f)
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
					var admitted bool
					panicked := canaryPanicOf(func() {
						for i := range recs {
							recs[i], oks[i] = classifyWinnerCanary(f.msg, f.status, f.event, env.label)
							logWinnerCanary(f.msg, f.status, f.event)
						}
						_, admitted = classifyWinnerCanary(f.msg, f.status, f.event, beyond)
					})
					writers.Wait()
					after := time.Now()
					if panicked != nil {
						t.Fatalf("the canary panicked: %v", panicked)
					}
					want := kind.record()
					for i := range recs {
						canaryAssertClassified(t, want, recs[i], oks[i], env.label)
					}
					if admitted {
						t.Error("the frame was admitted with a build label beyond its bound")
					}
					canaryAssertEmitted(t, env.wantLogged(want), capture.take(), len(recs), before, after)
				})
			}
		})
	}
	runtime.KeepAlive(alive)
}

// TestWinnerCanaryRecordsFollowTheFrameNotItsAddress changes one event object,
// and then one of its outcome objects, in place between calls, in every
// environment canaryEnvironments lists, on a frame of each shape. A canary
// that kept anything from one call to the next — a record, or a verdict keyed
// by the address of the event or of an outcome, the event id or the frame's
// size — would answer a later call with what the frame used to say; every
// record, classified and emitted (none is emitted with INFO disabled), must
// match what the frame says at that moment. It runs in a fresh process, since
// it counts the records the process logs.
func TestWinnerCanaryRecordsFollowTheFrameNotItsAddress(t *testing.T) {
	if !canaryInFreshProcess(t) {
		return
	}
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
	for _, env := range canaryEnvironments() {
		t.Run(env.name(), func(t *testing.T) {
			capture := &canaryCapture{level: env.floor}
			env.use(t, capture)
			for _, shape := range canaryShapes {
				// outcomes are outcome objects of the shape carrying the given ids.
				outcomes := func(ids ...interface{}) []interface{} { return shape.outcomes(canaryOutcomes(ids...)) }
				f := shape.qualifying(canaryEvent("synthetic-event-0001", "synthetic-outcome-b", outcomes("synthetic-outcome-a", "synthetic-outcome-b")))
				event := f.event
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
						event["outcomes"] = outcomes("synthetic-outcome-b", "synthetic-outcome-b")
					}, collided},
					{"the first outcome's id changes in place", func() {
						event["outcomes"].([]interface{})[0].(map[string]interface{})["id"] = "synthetic-outcome-a"
					}, positive0003},
				} {
					step.change()
					before := time.Now()
					rec, ok, p := canaryClassifyCatching(f.msg, f.status, event, env.label)
					if p != nil {
						t.Fatalf("%s%s: the classifier panicked: %v", step.name, shape.suffix(), p)
					}
					if p := canaryPanicOf(func() { logWinnerCanary(f.msg, f.status, event) }); p != nil {
						t.Fatalf("%s%s: the emitter panicked: %v", step.name, shape.suffix(), p)
					}
					after := time.Now()
					emitted := capture.take()
					t.Run(step.name+shape.suffix(), func(t *testing.T) {
						canaryAssertClassified(t, step.want.values(), rec, ok, env.label)
						canaryAssertEmitted(t, env.wantLogged(step.want.values()), emitted, 1, before, after)
					})
				}
			}
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
			msg, event := canaryEventMsg(), tc.event(canaryTrap{calls: calls})
			before := time.Now()
			if p := canaryPanicOf(func() { logWinnerCanary(msg, "RESOLVED", event) }); p != nil {
				t.Fatalf("the emitter panicked: %v", p)
			}
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

// canaryPanicOf calls f and returns what it panicked with, or nil, so that a
// panic fails the calling test by name instead of ending the process. Every
// call a test in this file makes into the canary — the classifier, the
// emitter, a classified record's rendering, or the pool's dispatcher that
// reaches them — goes through it, canaryClassifyCatching or
// canaryClassifiedFieldsAt, on whatever goroutine makes the call: a goroutine
// the test waits for reports with t.Errorf, one the test may stop waiting for
// hands its panic back (canarySecondEmission, the real-handler test's
// dispatchers), and a measured loop is caught as a whole, outside its
// measured window. A dispatcher that panicked may still hold a pool or
// connection lock, which parks the next frame dispatched on that pool, so a
// test dispatches nothing more on that pool after a panic (t.Fatalf, or a
// break where it still has to judge what it noted), whether it runs in the
// test process or in a fresh one: a fresh process that went on would wait for
// that lock until its watchdog, near the end of the run's deadline, and leave
// the fresh processes after it too little time to run.
// The call's inputs are built before it, so a panic building them is never
// counted as the canary's; the test code the canary calls back into (a log
// handler, a trap, a sink hook) runs inside the catch, and a panic there is.
// The benchmarks, which judge nothing, call the canary directly.
func canaryPanicOf(f func()) (panicked interface{}) {
	defer func() { panicked = recover() }()
	f()
	return nil
}

// canaryClassifyCatching calls the classifier and returns its answer and what
// it panicked with, or nil (canaryPanicOf).
func canaryClassifyCatching(msg *PubSubMessage, status string, event map[string]interface{}, build string) (rec winnerCanaryRecord, ok bool, panicked interface{}) {
	panicked = canaryPanicOf(func() { rec, ok = classifyWinnerCanary(msg, status, event, build) })
	return rec, ok, panicked
}

// canaryClassifiedFieldsAt renders a classified record for the instant
// observedAt, failing the test by name if rendering it panics (canaryPanicOf),
// and returns its fields (canaryFields).
func canaryClassifiedFieldsAt(t testing.TB, rec winnerCanaryRecord, observedAt time.Time) map[string]interface{} {
	t.Helper()
	var attrs []slog.Attr
	if p := canaryPanicOf(func() { attrs = rec.attrs(observedAt) }); p != nil {
		t.Fatalf("rendering the classified record panicked: %v", p)
	}
	return canaryFields(t, attrs)
}

// TestWinnerCanaryIgnoresNonCandidatesAtTheClassifier covers the envelope
// checks the handler cannot reach: a nil message and a nil event object, which
// must neither panic the classifier or the emitter nor yield a record. It
// counts every record the process logs, so it runs in a fresh process.
func TestWinnerCanaryIgnoresNonCandidatesAtTheClassifier(t *testing.T) {
	if !canaryInFreshProcess(t) {
		return
	}
	event := canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair())
	if _, ok, p := canaryClassifyCatching(canaryEventMsg(), "RESOLVED", event, "synthetic-build"); p != nil {
		t.Fatalf("the classifier panicked on the qualifying frame: %v", p)
	} else if !ok {
		t.Fatal("control: the qualifying frame was not admitted")
	}
	nilInputs := []struct {
		name  string
		msg   *PubSubMessage
		event map[string]interface{}
	}{{"a nil message", nil, event}, {"a nil event object", canaryEventMsg(), nil}}
	for _, in := range nilInputs {
		if _, ok, p := canaryClassifyCatching(in.msg, "RESOLVED", in.event, "synthetic-build"); p != nil {
			t.Errorf("%s panicked the classifier: %v", in.name, p)
		} else if ok {
			t.Errorf("%s produced a record", in.name)
		}
	}
	capture := &canaryCapture{level: canaryCaptureAll}
	canaryUseProcessLogger(t, capture)
	for _, in := range nilInputs {
		if p := canaryPanicOf(func() { logWinnerCanary(in.msg, "RESOLVED", in.event) }); p != nil {
			t.Errorf("%s panicked the emitter: %v", in.name, p)
		}
	}
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
	msg, event := canaryEventMsg(), canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair())
	if p := canaryPanicOf(func() { logWinnerCanary(msg, "RESOLVED", event) }); p != nil {
		t.Fatalf("the emitter panicked: %v", p)
	}
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
	event := canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair())
	rec, ok, p := canaryClassifyCatching(canaryEventMsg(), "RESOLVED", event, "synthetic-build")
	if p != nil {
		t.Fatalf("the classifier panicked: %v", p)
	}
	if !ok {
		t.Fatal("control frame refused")
	}
	observed := time.Date(2026, 9, 23, 18, 4, 5, 987654321, time.FixedZone("UTC+3", 3*60*60))
	if got := canaryClassifiedFieldsAt(t, rec, observed)["observed_at_utc"]; got != "2026-09-23T15:04:05Z" {
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
	msg, event := canaryEventMsg(), canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair())
	before := time.Now()
	if p := canaryPanicOf(func() { logWinnerCanary(msg, "RESOLVED", event) }); p != nil {
		t.Fatalf("the emitter panicked: %v", p)
	}
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
// neither sequential nor concurrent duplicates are suppressed. It calls the
// canary from many goroutines at once, so it runs in a fresh process
// (canaryInFreshProcess). The first frame whose handling panics, on the test's
// goroutine or on any of the concurrent ones, ends it, since the dispatcher
// may have panicked holding the pool lock the next frame would wait for.
func TestWinnerCanaryRealHandlerEmitsForEveryQualifyingFrame(t *testing.T) {
	if !canaryInFreshProcess(t) {
		return
	}
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

	// handle dispatches one parsed frame on the test's goroutine and ends the
	// test if the dispatch panicked.
	handle := func(msg *PubSubMessage) {
		if panicked := canaryPanicOf(func() { p.handleMessage(msg) }); panicked != nil {
			t.Fatalf("handling a frame panicked: %v", panicked)
		}
	}
	before := time.Now()
	handle(parseCanaryFrame(t, canaryTopicChan1, tracked))
	handle(parseCanaryFrame(t, canaryTopicChan1, untracked))
	for i := 0; i < 3; i++ {
		handle(parseCanaryFrame(t, canaryTopicChan1, tracked))
	}
	// The concurrent dispatchers hand what they panicked with back to the
	// test's goroutine, which stops waiting for the others at the first one;
	// panics is closed once they have all returned, each after its send.
	const concurrent = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	panics := make(chan interface{}, concurrent)
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
			if panicked := canaryPanicOf(func() { p.handleMessage(msg) }); panicked != nil {
				panics <- panicked
			}
		}()
	}
	close(start)
	go func() {
		wg.Wait()
		close(panics)
	}()
	if panicked, ok := <-panics; ok {
		t.Fatalf("handling a frame panicked: %v", panicked)
	}
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
// WebSocketClient into the pool and inspects the moment of emission from inside
// the log handler. The record is handled on the goroutine that dispatched the
// frame, the frame's channel_event observation has already been recorded, the
// tracked round has not yet been updated, and no connection, pool, round or
// observation-sink lock is held. It then emits a second record for the same
// event from that moment, on another goroutine, and waits for it
// (canaryAwaitEmission). A lock the second emission must take and the first
// holds — a sync.Mutex, sync.RWMutex's Lock, a condition — or a channel held
// across emission parks that emission inside the canary, which its goroutine's
// stack shows; a TryLock held across emission, or the suppression of an
// in-flight duplicate, keeps the second record from being made, which the
// emission order shows. What this cannot see is left to winner_canary.go's
// source, which takes no lock, keeps no package state, goroutine or channel and
// carries no compiler directive: a lock both emissions can hold at once
// (sync.RWMutex's RLock), one the second emission does not take, a lock or
// channel the second emission waits on outside the module's code (a
// log.Logger's mutex, say), a wait in the module's code whose stack frames a
// //line directive attributes to a test file, a timed wait the second emission
// gives up before canaryAwaitEmission looks again, a spin, or a wait on I/O. A
// second emission that neither returns nor parks in the canary within a minute
// is reported by a watchdog as TIMEOUT / HARNESS_FAILURE, never as a verdict.
// The frames are sequential — a concurrent handler could hold the pool lock
// legitimately — and the first frame whose handling panics ends them, since the
// dispatcher may have panicked holding a lock the next frame would wait for.
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
	var second chan struct{}       // closed when the second emission has returned
	var secondPanicked interface{} // what the second emission panicked with, set before second is closed
	canaryUseProcessLogger(t, canaryProbeHandler{level: canaryCaptureAll, onRecord: func(r slog.Record) {
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
		second = make(chan struct{})
		go canarySecondEmission(second, &secondPanicked)
		switch reason, returned := canaryAwaitEmission(second); {
		case returned:
		case reason != "":
			note(&problems, "a second emission started during the first is parked in the canary ("+reason+"): what it waits for is held across emission")
		default:
			note(&problems, canaryWatchdog+": a second emission started during the first had neither returned nor parked in the canary after a minute (a watchdog, not a verdict)")
		}
	}})

	for _, frame := range [][]byte{
		canaryUpdateFrame("synthetic-event-0001", "RESOLVED", 310, 210, "synthetic-outcome-b"),
		canaryUpdateFrame("synthetic-event-0003", "RESOLVED", 10, 20, ""),
	} {
		msg := WSMessage{Type: "MESSAGE", Data: &WSData{Topic: canaryTopicChan1, Message: string(frame)}}
		if panicked := canaryPanicOf(func() { ws.handleMessage(msg) }); panicked != nil {
			note(&problems, fmt.Sprintf("handling a frame panicked: %v", panicked))
			break
		}
	}
	// A parked second emission goes on once the first has returned; let it
	// finish before judging, so it cannot outlive the test.
	if second != nil {
		select {
		case <-second:
			if secondPanicked != nil {
				note(&problems, fmt.Sprintf("a second emission started during the first panicked: %v", secondPanicked))
			}
		case <-time.After(time.Minute):
			note(&problems, canaryWatchdog+": the second emission had not returned a minute after the frames were handled (a watchdog, not a verdict)")
		}
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

// canarySecondEmission is the emission TestWinnerCanaryEmitsInPlaceWithNoLockHeld
// starts while the first is being handled; it sets *panicked to what the
// emission panicked with, or nil (canaryPanicOf), and then closes done. It
// does not call t.Errorf: the test stops waiting for it when its watchdog
// fires, and failing a test that has returned panics the process. It is a
// function of its own so that canaryAwaitEmission can tell its goroutine by
// name.
//
//go:noinline
func canarySecondEmission(done chan<- struct{}, panicked *interface{}) {
	defer close(done)
	msg, event := canaryEventMsg(), canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair())
	*panicked = canaryPanicOf(func() { logWinnerCanary(msg, "RESOLVED", event) })
}

// canaryAwaitEmission waits until canarySecondEmission has returned (returned)
// or its goroutine is parked inside the canary (canaryParkedInCanary; reason is
// its wait reason), which only something held across the first emission can
// cause. It looks at most every 10 ms, and less often when reading the
// goroutines' stacks takes longer. The first emission is inside this call, so
// what it holds stays held and a second emission waiting for it stays parked;
// only a wait the second emission gives up before the next look can go unseen.
// A watchdog ends the wait after a minute with neither.
func canaryAwaitEmission(done <-chan struct{}) (reason string, returned bool) {
	watchdog := time.After(time.Minute)
	pace := time.NewTicker(10 * time.Millisecond)
	defer pace.Stop()
	marker := []byte(".canarySecondEmission(")
	buf := make([]byte, 1<<20)
	for {
		select {
		case <-done:
			return "", true
		case <-watchdog:
			return "", false
		case <-pace.C:
		}
		n := runtime.Stack(buf, true)
		for n == len(buf) {
			buf = make([]byte, 2*len(buf)) // truncated: look again with more room
			n = runtime.Stack(buf, true)
		}
		for g := range bytes.SplitSeq(buf[:n], []byte("\n\n")) {
			if bytes.Contains(g, marker) {
				if reason := canaryParkedInCanary(string(g)); reason != "" {
					return reason, false
				}
			}
		}
	}
}

// canaryParkedInCanary returns the wait reason of the goroutine whose stack g
// is, when that goroutine is parked on a lock or a channel — a wait reason of
// package sync, semacquire, a channel operation or select — with the innermost
// frame outside the runtime, package sync and the standard library's internal
// packages (internal/sync holds sync.Mutex's slow path) in the module's own
// code outside its tests: the canary or what it reaches; otherwise "". It knows
// a test file by the file name the stack shows, which a //line directive can
// change.
func canaryParkedInCanary(g string) string {
	module := strings.TrimSuffix(reflect.TypeOf(PubSubMessage{}).PkgPath(), "/internal/pubsub")
	lines := strings.Split(g, "\n")
	from, to := strings.IndexByte(lines[0], '['), strings.IndexByte(lines[0], ']')
	if from < 0 || to < from {
		return ""
	}
	reason, _, _ := strings.Cut(lines[0][from+1:to], ",")
	if !strings.HasPrefix(reason, "sync.") && reason != "semacquire" && !strings.HasPrefix(reason, "chan ") && !strings.HasPrefix(reason, "select") {
		return ""
	}
	for i := 1; i+1 < len(lines); i += 2 {
		fn, file := lines[i], lines[i+1]
		if strings.HasPrefix(fn, "runtime.") || strings.HasPrefix(fn, "internal/") || strings.HasPrefix(fn, "sync.") {
			continue
		}
		if strings.HasPrefix(fn, module+"/") && !strings.Contains(file, "_test.go:") {
			return reason
		}
		return ""
	}
	return ""
}

// TestWinnerCanaryConcurrentDirectCallsShareNothing calls the emitter from many
// goroutines at once, with no pool lock to order them, on frames of their own:
// every call yields its record, and the race detector sees no shared state. It
// runs in a fresh process (canaryInFreshProcess).
func TestWinnerCanaryConcurrentDirectCallsShareNothing(t *testing.T) {
	if !canaryInFreshProcess(t) {
		return
	}
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
				msg, event := canaryEventMsg(), canaryEvent("synthetic-event-0001", "synthetic-outcome-b", canaryPair())
				if p := canaryPanicOf(func() { logWinnerCanary(msg, "RESOLVED", event) }); p != nil {
					t.Errorf("an emission panicked: %v", p)
					return
				}
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
// — is a reported data race; and every call still emits its record. It runs
// in a fresh process (canaryInFreshProcess).
func TestWinnerCanaryConcurrentCallsLeaveASharedFrameAlone(t *testing.T) {
	if !canaryInFreshProcess(t) {
		return
	}
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
				if p := canaryPanicOf(func() { logWinnerCanary(msg, "RESOLVED", event) }); p != nil {
					t.Errorf("an emission panicked: %v", p)
					return
				}
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
	deliver := func(frame []byte) {
		msg := parseCanaryFrame(t, canaryTopicChan1, frame)
		if panicked := canaryPanicOf(func() { p.handleMessage(msg) }); panicked != nil {
			t.Fatalf("handling a frame panicked: %v", panicked)
		}
	}

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
// only whether its record reaches a handler differs, so the test compares
// emission on and off, not the canary present and absent. The business trails
// — every observation, the placement calls and the tracked rounds' full
// outcome state — must be identical and match the literal expectation. Log
// output is not part of the trail, since the inactive default discards every
// INFO record, business ones included; the scenario's one business log line,
// the manual bet's, is checked in the active run on its own, and the active
// run's canary records must carry an observed_at_utc within that run.
func TestWinnerCanaryLeavesBusinessBehaviourUnchanged(t *testing.T) {
	activeCapture := &canaryCapture{level: slog.LevelInfo}
	before := time.Now()
	active := runCanaryBusinessScenario(t, activeCapture, nil)
	after := time.Now()
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
			assertCanaryFields(t, canaryFields(t, canaryRecordAttrs(got[i])), want[i], version.Version, before, after)
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
			msg := parseCanaryFrame(t, canaryTopicChan1, frame)
			before := time.Now()
			if panicked := canaryPanicOf(func() { p.handleMessage(msg) }); panicked != nil {
				t.Fatalf("handling the frame panicked: %v", panicked)
			}
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

	msg := parseCanaryFrame(t, canaryTopicChan1, frame)
	if panicked := canaryPanicOf(func() { p.handleMessage(msg) }); panicked != nil {
		t.Fatalf("handling the frame before shutdown panicked: %v", panicked)
	}
	l.Close()
	msg = parseCanaryFrame(t, canaryTopicChan1, frame)
	if panicked := canaryPanicOf(func() { p.handleMessage(msg) }); panicked != nil {
		t.Fatalf("handling the frame after shutdown panicked: %v", panicked)
	}

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

	rec, ok, p := canaryClassifyCatching(msg, "RESOLVED", event, "synthetic-build")
	if p != nil {
		t.Fatalf("the classifier panicked: %v", p)
	}
	if !ok {
		t.Fatal("control frame refused")
	}
	if p := canaryPanicOf(func() { logWinnerCanary(msg, "RESOLVED", event) }); p != nil {
		t.Fatalf("the emitter panicked: %v", p)
	}
	if !reflect.DeepEqual(*msg, snapshot) || !reflect.DeepEqual(event, frame()) {
		t.Fatalf("the canary mutated its input")
	}

	// Rewrite the frame after the fact: a record that retained any part of it
	// would now report something else.
	event["id"] = "synthetic-event-0002"
	event["winning_outcome_id"] = ""
	event["outcomes"] = canaryOutcomes("synthetic-outcome-a", "synthetic-outcome-a")
	now := time.Now()
	assertCanaryFields(t, canaryClassifiedFieldsAt(t, rec, now), canaryPositive0001.values(), "synthetic-build", now, now)

	// Statelessness: A, then an unrelated malformed B, then A again.
	b := canaryEvent("synthetic-event-0002", "synthetic-outcome-a", canaryOutcomes("synthetic-outcome-a", "synthetic-outcome-a"))
	recB, _, p := canaryClassifyCatching(msg, "RESOLVED", b, "synthetic-build")
	if p != nil {
		t.Fatalf("the classifier panicked on the malformed frame: %v", p)
	}
	recA2, _, p := canaryClassifyCatching(msg, "RESOLVED", frame(), "synthetic-build")
	if p != nil {
		t.Fatalf("the classifier panicked on the first frame again: %v", p)
	}
	assertCanaryFields(t, canaryClassifiedFieldsAt(t, recB, now), malformed.values(), "synthetic-build", now, now)
	assertCanaryFields(t, canaryClassifiedFieldsAt(t, recA2, now), canaryPositive0001.values(), "synthetic-build", now, now)
}

// canaryRetentionProbe is a heap object large enough to be tracked on its own
// by the garbage collector, so a weak pointer to it goes nil once nothing else
// holds it.
type canaryRetentionProbe struct {
	self *canaryRetentionProbe
	_    [64]byte
}

// canaryProbeMakers make what canaryEmitWithProbes tracks: text puts a string
// in memory of its own, and probe makes a probe.
type canaryProbeMakers struct {
	text  func(string) string
	probe func() *canaryRetentionProbe
}

// outcomes are outcome objects with the given ids, each id made by text and
// each object holding a probe.
func (m canaryProbeMakers) outcomes(ids ...string) []interface{} {
	out := make([]interface{}, len(ids))
	for i, id := range ids {
		out[i] = map[string]interface{}{"id": m.text(id), "SENTINEL_PROBE": m.probe()}
	}
	return out
}

// canaryProbedEvent builds an event whose strings and probes m makes.
type canaryProbedEvent func(m canaryProbeMakers) map[string]interface{}

// canaryRetentionFrames are the frames TestWinnerCanaryRetainsNothingAfterTheCall
// emits, with the number of records each yields: a positive one, one whose
// winner is an object, one with no winner, and three a resource bound refuses —
// for 65 outcomes, for an outcome id beyond 4096 bytes and for an event id
// beyond 4096 bytes.
var canaryRetentionFrames = []struct {
	name    string
	records int
	event   canaryProbedEvent
}{
	{"a positive frame", 1, func(m canaryProbeMakers) map[string]interface{} {
		return canaryEvent(m.text("synthetic-event-0001"), m.text("synthetic-outcome-b"),
			m.outcomes("synthetic-outcome-a", "synthetic-outcome-b"))
	}},
	{"an object winner", 1, func(m canaryProbeMakers) map[string]interface{} {
		return canaryEvent(m.text("synthetic-event-0001"), map[string]interface{}{"id": m.text("synthetic-outcome-b"), "SENTINEL_PROBE": m.probe()},
			m.outcomes("synthetic-outcome-a", "synthetic-outcome-b"))
	}},
	{"no winner", 1, func(m canaryProbeMakers) map[string]interface{} {
		return canaryEvent(m.text("synthetic-event-0001"), canaryAbsentKey{},
			m.outcomes("synthetic-outcome-a", "synthetic-outcome-b"))
	}},
	{"refused for 65 outcomes", 0, func(m canaryProbeMakers) map[string]interface{} {
		ids := make([]string, 65)
		for i := range ids {
			ids[i] = fmt.Sprintf("synthetic-outcome-%02d", i)
		}
		return canaryEvent(m.text("synthetic-event-0001"), m.text("synthetic-outcome-01"), m.outcomes(ids...))
	}},
	{"refused for an outcome id beyond its bound", 0, func(m canaryProbeMakers) map[string]interface{} {
		return canaryEvent(m.text("synthetic-event-0001"), m.text("synthetic-outcome-a"),
			m.outcomes("synthetic-outcome-a", strings.Repeat("x", 4097)))
	}},
	{"refused for an event id beyond its bound", 0, func(m canaryProbeMakers) map[string]interface{} {
		return canaryEvent(m.text(strings.Repeat("e", 4097)), m.text("synthetic-outcome-b"),
			m.outcomes("synthetic-outcome-a", "synthetic-outcome-b"))
	}},
}

// canaryEmitWithProbes hands the emitter the frame event builds, which also
// holds a probe in its raw message, its data envelope and its event. Every
// string text made is in memory of its own. It returns only weak pointers: to
// the probes, and to the bytes of those strings; and what the emitter panicked
// with, or nil (canaryPanicOf).
//
//go:noinline
func canaryEmitWithProbes(event canaryProbedEvent) ([]weak.Pointer[canaryRetentionProbe], []weak.Pointer[byte], interface{}) {
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
	ev := event(canaryProbeMakers{text, probe})
	ev["SENTINEL_PROBE"] = probe()
	msg := canaryEventMsg()
	msg.Data = map[string]interface{}{"event": ev, "SENTINEL_PROBE": probe()}
	msg.Message = map[string]interface{}{"SENTINEL_PROBE": probe()}
	panicked := canaryPanicOf(func() { logWinnerCanary(msg, "RESOLVED", ev) })
	return probes, texts, panicked
}

// canaryReachable counts the weak pointers of ps whose object the garbage
// collector has not reclaimed.
func canaryReachable[T any](ps []weak.Pointer[T]) int {
	n := 0
	for _, p := range ps {
		if p.Value() != nil {
			n++
		}
	}
	return n
}

// TestWinnerCanaryRetainsNothingAfterTheCall shows the canary keeps no
// reference into a frame it records or refuses (canaryRetentionFrames). Each
// frame is checked on its own, before the next one is emitted, so keeping only
// the latest frame shows as well: once the emitter has returned and the caller
// has let the frame go, the garbage collector reclaims every probe it held — in
// the raw message, the data envelope, the event, each outcome and an object
// winner — and the bytes of every id and winner string. Each frame must first
// yield its number of records, so a frame that stopped reaching its branch
// fails. A copy of a string would not show here, nor would state a call
// leaves behind that changes no record: the repeated and concurrent emission
// tests show state that changes one, and the rest is left to winner_canary.go's
// source, which keeps no package state.
func TestWinnerCanaryRetainsNothingAfterTheCall(t *testing.T) {
	capture := &canaryCapture{level: slog.LevelInfo}
	canaryUseProcessLogger(t, capture)
	for _, frame := range canaryRetentionFrames {
		t.Run(frame.name, func(t *testing.T) {
			recorded := len(capture.canaries())
			probes, texts, p := canaryEmitWithProbes(frame.event)
			if p != nil {
				t.Fatalf("the emitter panicked: %v", p)
			}
			if n := len(capture.canaries()) - recorded; n != frame.records {
				t.Fatalf("control: the frame yielded %d record(s), want %d", n, frame.records)
			}
			for i := 0; i < 5 && canaryReachable(probes)+canaryReachable(texts) != 0; i++ {
				runtime.GC()
			}
			if objectsLeft, textsLeft := canaryReachable(probes), canaryReachable(texts); objectsLeft+textsLeft != 0 {
				t.Fatalf("the frame outlived the call: %d of %d probes and %d of %d id strings are still reachable, so the canary kept a reference into it",
					objectsLeft, len(probes), textsLeft, len(texts))
			}
		})
	}
}

// TestWinnerCanaryCostIsBoundedByTheAdmissionLimits counts what classifying a
// frame at every limit (64 outcomes of 4096-byte ids, a 4096-byte winner and
// event id) allocates, on frames of both shapes (canaryShape), in every
// environment canaryEnvironments lists, the frames built afresh in each; a
// shape that fails in one environment is not tried in those after it. Over
// 200 calls, the process's allocations must average at most 32 KiB a call, far
// below what copying the admitted strings would cost (64 × 4096 bytes). That
// bound is this test's own, and lower than the per-call temporary data the
// contract allows within its limits: the count is supplemental data about this
// implementation, not the contract's bounded-work rule, which is a property of
// winner_canary.go's source.
func TestWinnerCanaryCostIsBoundedByTheAdmissionLimits(t *testing.T) {
	ids := make([]interface{}, 64)
	for i := range ids {
		ids[i] = fmt.Sprintf("%02d", i) + strings.Repeat("y", 4094)
	}
	for _, shape := range canaryShapes {
		canaryInEachEnvironment(t, shape.suffix(), func(t *testing.T, env canaryEnvironment) {
			env.use(t, &canaryCapture{level: env.floor})
			for _, event := range []map[string]interface{}{
				canaryEvent("synthetic-event-0001", ids[63], shape.outcomes(canaryOutcomes(ids...))),
				canaryEvent(strings.Repeat("e", 4096), ids[63], shape.outcomes(canaryOutcomes(ids...))),
			} {
				f := shape.qualifying(event)
				if rec, ok, p := canaryClassifyCatching(f.msg, f.status, f.event, env.label); p != nil {
					t.Fatalf("the classifier panicked: %v", p)
				} else if !ok || canaryClassifiedFields(t, rec)["result"] != canaryObserved {
					t.Fatal("control: every frame here must be admitted as a positive")
				}
				const runs = 200
				var before, after runtime.MemStats
				if p := canaryPanicOf(func() {
					runtime.GC()
					runtime.ReadMemStats(&before)
					for i := 0; i < runs; i++ {
						classifyWinnerCanary(f.msg, f.status, f.event, env.label)
					}
					runtime.ReadMemStats(&after)
				}); p != nil {
					t.Fatalf("the classifier panicked on a repeated call: %v", p)
				}
				if perCall := (after.TotalAlloc - before.TotalAlloc) / runs; perCall > 32<<10 {
					t.Fatalf("a frame at the limits allocates %d bytes per call; copying the admitted outcome ids alone would be %d", perCall, 64*4096)
				}
			}
		})
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
// topic (every other topic type, and the candidate's upper-cased, with a
// trailing space, or empty), message type, status, build label — is perturbed
// rarely and independently; then half of the frames start as a well-formed
// round of up to 64 outcomes that receives at most one perturbation of its
// event, so a positive result is common rather than a rarity, and the other
// half draw every field from its boundary values, where each value the canary
// reads — the event id, the winner, the outcomes, an outcome and an outcome's
// id — takes every JSON type. In both halves the event id is sometimes one of
// the outcomes' ids, the winner's or another's.
func genCanaryFrame(r *rand.Rand) (topic TopicType, msgType, status string, event map[string]interface{}, build string) {
	pick := func(options ...interface{}) interface{} { return options[r.IntN(len(options))] }
	idPool := []string{"synthetic-outcome-a", "synthetic-outcome-b", "synthetic-outcome-c", "synthetic-outcome-d",
		"synthetic-outcome-e", "synthetic-outcome-f"}
	long := strings.Repeat("z", 4096)

	topic = TopicPredictionsChannel
	if r.IntN(12) == 0 {
		topic = pick(TopicCommunityPointsUser, TopicPredictionsUser, TopicVideoPlaybackByID, TopicRaid,
			TopicCommunityMomentsChannel, TopicCommunityPointsChannel,
			TopicType("PREDICTIONS-CHANNEL-V1"), TopicType("predictions-channel-v1 "), TopicType("")).(TopicType)
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
		// Two to six outcomes or, one time in three, seven to sixty-four, so
		// that the winner, an invalid outcome and a repeated id each come at
		// every position of a vector the contract admits.
		n := 2 + r.IntN(5)
		if r.IntN(3) == 0 {
			n = 7 + r.IntN(58)
		}
		perm := r.Perm(64)
		name := func(i int) string { return fmt.Sprintf("synthetic-outcome-%02d", perm[i]) }
		list := make([]interface{}, n)
		for i := range list {
			list[i] = canaryGeneratedOutcome(r, name(i))
		}
		// The event id is sometimes an outcome's id, the winner's or another's.
		event = canaryEvent(pick("synthetic-event-0001", name(r.IntN(n))), name(r.IntN(n)), list)
		switch r.IntN(6) {
		case 0:
			event["winning_outcome_id"] = pick(nil, "", " ", "synthetic-outcome-z", float64(1), long+"!")
		case 1:
			delete(event, "winning_outcome_id")
		case 2:
			list[r.IntN(n)] = pick(nil, "synthetic-outcome-a", false, float64(3), map[string]interface{}{"id": pick(nil, "", float64(1), true, long+"!")})
		case 3:
			later := 1 + r.IntN(n-1) // repeats the id of an outcome anywhere before it
			list[later] = canaryGeneratedOutcome(r, name(r.IntN(later)))
		case 4:
			event["id"] = pick(nil, "", " ", float64(7), true, []interface{}{"synthetic-event-0001"}, strings.Repeat("e", 4097))
		}
		return topic, msgType, status, event, build
	}

	outcomeID := func() interface{} {
		switch r.IntN(17) {
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
		case 6:
			return r.IntN(2) == 0
		case 7:
			return map[string]interface{}{"id": idPool[0]}
		case 8:
			return []interface{}{idPool[0]}
		}
		return idPool[r.IntN(len(idPool))]
	}
	outcome := func() interface{} {
		switch r.IntN(14) {
		case 0:
			return nil
		case 1:
			return "synthetic-outcome-a"
		case 2:
			return []interface{}{map[string]interface{}{"id": "synthetic-outcome-a"}}
		case 3:
			return r.IntN(2) == 0
		case 4:
			return float64(r.IntN(3))
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
		outcomes = pick("synthetic-outcome-a", map[string]interface{}{"0": map[string]interface{}{"id": "synthetic-outcome-a"}}, float64(2), true)
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
	id := pick(canaryAbsentKey{}, nil, "", " ", "synthetic-event-0001", "synthetic-event-0002", idPool[0], float64(7), true,
		map[string]interface{}{"id": "synthetic-event-0001"}, []interface{}{"synthetic-event-0001"},
		strings.Repeat("e", 4096), strings.Repeat("e", 4097))
	return topic, msgType, status, canaryEvent(id, winner, outcomes), build
}

// canaryChannelIDs are channel ids of the shapes an envelope's topic can
// carry: a marked one, and ids of digits only, from one digit to twenty.
var canaryChannelIDs = []string{"synthetic-channel-4417", canaryLiveChannelID, "441700012", "4417000123", "7", "44170001234567890123"}

// canaryEnvelopes are the two envelopes generated frame n is tried in: bare,
// with only the topic and message type the classifier is given, and stamped,
// sent now (canaryWireMsg), each on a channel id of canaryChannelIDs that n
// picks. The record must not depend on anything else the envelope carries, the
// channel id included.
func canaryEnvelopes(topic TopicType, msgType string, n int) []*PubSubMessage {
	bare, live := canaryChannelIDs[n%len(canaryChannelIDs)], canaryChannelIDs[(n+1)%len(canaryChannelIDs)]
	return []*PubSubMessage{
		{Topic: NewTopic(topic, bare), Type: msgType},
		canaryWireMsg(topic, msgType, live, time.Now().UTC().Format(time.RFC3339Nano)),
	}
}

// TestWinnerCanaryMatchesTheContractModelOnGeneratedFrames compares the
// classifier with referenceWinnerCanary over seeded random frames, each in
// both of canaryEnvelopes, checks the cross-attribute invariants the contract
// implies, and checks every input — aggregates and predictor data included —
// is left unmodified. A call that panics fails the test by name
// (canaryPanicOf).
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
			want, wantOK := referenceWinnerCanary(topic, msgType, status, event, build)
			for _, msg := range canaryEnvelopes(topic, msgType, i) {
				rec, ok, p := canaryClassifyCatching(msg, status, event, build)
				if p != nil {
					t.Fatalf("seed %d frame %d, stamped %v: the classifier panicked: %v (event %.300v)", seed, i, msg.ConnectionKnown, p, event)
				}
				if event != nil && !reflect.DeepEqual(event, snapshot) {
					t.Fatalf("seed %d frame %d, stamped %v: input mutated", seed, i, msg.ConnectionKnown)
				}
				if ok != wantOK {
					t.Fatalf("seed %d frame %d, stamped %v: admitted=%v, contract model says %v (event %.300v)",
						seed, i, msg.ConnectionKnown, ok, wantOK, event)
				}
				if !ok {
					continue
				}
				got := canaryClassifiedFields(t, rec)
				for k, v := range want {
					if got[k] != v {
						t.Fatalf("seed %d frame %d, stamped %v: %s = %#v, contract model says %#v (event %.300v)",
							seed, i, msg.ConnectionKnown, k, got[k], v, event)
					}
				}
				if got["build_version"] != build {
					t.Fatalf("seed %d frame %d, stamped %v: build_version not carried", seed, i, msg.ConnectionKnown)
				}
				assertCanaryInvariants(t, got)
			}
			switch {
			case wantOK:
				counts[want["result"].(string)]++
			case topic == TopicPredictionsChannel && msgType == "event-updated" && status == "RESOLVED" && event != nil:
				counts["refused by a bound"]++
			default:
				counts["not a candidate"]++
			}
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
// generated frame, in both of canaryEnvelopes, through the emitter itself, with
// the frame's build label as internal/version.Version, into a lossless capture
// of every level. Each frame yields exactly what the classifier decides for it
// — one INFO record carrying the classifier's attributes, or nothing at all —
// so no frame shape the generator draws, with lists of up to 66 outcomes and
// ids of up to 4097 bytes, makes the emitter add, drop or alter a record, and a
// call that panics fails the test by name (canaryPanicOf). It counts every
// record the process logs, so it runs in a fresh process.
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
			version.Version = build
			for _, msg := range canaryEnvelopes(topic, msgType, i) {
				rec, ok, p := canaryClassifyCatching(msg, status, event, build)
				if p != nil {
					t.Fatalf("seed %d frame %d, stamped %v: the classifier panicked: %v", seed, i, msg.ConnectionKnown, p)
				}
				if pending := capture.take(); len(pending) != 0 {
					t.Fatalf("seed %d frame %d, stamped %v: classifying alone logged %d record(s), first %q",
						seed, i, msg.ConnectionKnown, len(pending), pending[0].Message)
				}
				before := time.Now()
				if p := canaryPanicOf(func() { logWinnerCanary(msg, status, event) }); p != nil {
					t.Fatalf("seed %d frame %d, stamped %v: the emitter panicked: %v", seed, i, msg.ConnectionKnown, p)
				}
				after := time.Now()
				got := capture.take()
				if !ok {
					if len(got) != 0 {
						t.Fatalf("seed %d frame %d, stamped %v: the classifier refuses the frame, but the emitter logged %d record(s), first %q",
							seed, i, msg.ConnectionKnown, len(got), got[0].Message)
					}
					continue
				}
				if len(got) != 1 || got[0].Message != "p4_winner_canary" || got[0].Level != slog.LevelInfo {
					t.Fatalf("seed %d frame %d, stamped %v: %d record(s), want exactly one INFO p4_winner_canary", seed, i, msg.ConnectionKnown, len(got))
				}
				assertCanaryFields(t, canaryFields(t, canaryRecordAttrs(got[0])), canaryClassifiedFields(t, rec), build, before, after)
				if msg.ConnectionKnown {
					emitted++
				}
			}
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
// parser and compares the classifier with the contract model, on the message
// as parsed and once more stamped with the provenance a connection gives every
// frame it delivers. Its seed corpus is every testdata/winner_canary fixture,
// so plain `go test` replays them.
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
		stamped := *msg
		for _, envelope := range []*PubSubMessage{msg, canaryStampConnection(&stamped)} {
			rec, ok, p := canaryClassifyCatching(envelope, status, event, version.Version)
			if p != nil {
				t.Fatalf("stamped %v: the classifier panicked: %v", envelope.ConnectionKnown, p)
			}
			if ok != wantOK {
				t.Fatalf("stamped %v: admitted=%v, contract model says %v", envelope.ConnectionKnown, ok, wantOK)
			}
			if !ok {
				continue
			}
			got := canaryClassifiedFields(t, rec)
			for k, v := range want {
				if got[k] != v {
					t.Fatalf("stamped %v: %s = %#v, contract model says %#v", envelope.ConnectionKnown, k, got[k], v)
				}
			}
			assertCanaryInvariants(t, got)
		}
	})
}

// --- Cost characterization: benchmarks, never a verdict ---------------------

// canaryBenchSink keeps the benchmarks' baseline work from being optimized
// away.
var canaryBenchSink int

// canaryCountKeys walks m's keys once, counting them: the least any
// enumeration of m does.
func canaryCountKeys(m map[string]interface{}) int {
	n := 0
	for range m {
		n++
	}
	return n
}

// canaryTypePass checks every outcome's type: the least any scan of a list
// does.
func canaryTypePass(outcomes []interface{}) int {
	objects := 0
	for _, o := range outcomes {
		if _, ok := o.(map[string]interface{}); ok {
			objects++
		}
	}
	return objects
}

// canaryComparePass compares each outcome's id with the next one's: the least
// a pairwise check of the ids does.
func canaryComparePass(outcomes []interface{}) int {
	equal, prev := 0, ""
	for i, o := range outcomes {
		obj, _ := o.(map[string]interface{})
		id, _ := obj["id"].(string)
		if i > 0 && id == prev {
			equal++
		}
		prev = id
	}
	return equal
}

// canaryBenchHandler lets through the records at or above floor and keeps
// nothing.
func canaryBenchHandler(floor slog.Level) slog.Handler {
	return canaryProbeHandler{level: floor, onRecord: func(slog.Record) {}}
}

// BenchmarkWinnerCanaryFixedKeyCalls characterizes one classification, one
// emission and one refusal for a build label beyond its bound, on a frame of
// every kind as TestWinnerCanaryRecordsLargeFramesOfEveryKind builds it, in
// every environment canaryEnvironments lists, beside one plain walk over the
// frame's 65,536 extra keys, the least any enumeration costs (reported as
// walk-ns). It judges nothing.
func BenchmarkWinnerCanaryFixedKeyCalls(b *testing.B) {
	parts, extraKeys, longText := canaryLargeParts()
	walk := canaryBestOf(3, func() { canaryBenchSink += canaryCountKeys(extraKeys) })
	for _, kind := range canaryFrameKinds() {
		b.Run(kind.name, func(b *testing.B) {
			// Earlier kinds' frames are garbage now: collecting them before this
			// kind's frame is built bounds the peak memory.
			runtime.GC()
			f := kind.build(parts, 1)
			for _, env := range canaryEnvironments() {
				b.Run(env.name(), func(b *testing.B) {
					env.use(b, canaryBenchHandler(env.floor))
					for _, op := range []struct {
						name string
						call func()
					}{
						{"classify", func() { classifyWinnerCanary(f.msg, f.status, f.event, env.label) }},
						{"emit", func() { logWinnerCanary(f.msg, f.status, f.event) }},
						{"refuse a long build label", func() { classifyWinnerCanary(f.msg, f.status, f.event, longText) }},
					} {
						b.Run(op.name, func(b *testing.B) {
							for i := 0; i < b.N; i++ {
								op.call()
							}
							b.ReportMetric(float64(walk.Nanoseconds()), "walk-ns")
						})
					}
				})
			}
		})
	}
}

// canaryFreshTimings is what one fresh process of
// BenchmarkWinnerCanaryFirstCallOnFreshFrames reports: its walk over a frame's
// extra keys, its first call, one call per kind, in the order canaryFrameKinds
// lists them, and its refusal of a frame for a build label beyond its bound.
type canaryFreshTimings struct {
	Walk, First, Refusal time.Duration
	Calls                []time.Duration
}

// BenchmarkWinnerCanaryFirstCallOnFreshFrames characterizes first calls, which
// a canary that walked a frame once and then remembered doing so — by the
// address of a map in it, the message, an id, the winner, the frame's size, the
// branch the frame takes, or only that it had walked at all — would hide behind
// any earlier call. It runs for each floor of canaryLogFloors, with the label
// of canaryBuildLabels in the same place, and each operation is one fresh
// process (canaryBenchInFreshProcess) that runs canaryTimeFreshFrames, so its
// ns/op is a whole process; with -benchtime=5x the testing package runs a
// round of one, then the round of five it reports. It reports the fastest of
// its processes' walks (walk-ns), first calls of the process (first-call-ns)
// and refusals (refusal-ns), and the slowest kind's fastest first call
// (kind-first-call-ns), and logs each kind's (go test -v). It judges nothing:
// each process checks only that every record is its kind's.
func BenchmarkWinnerCanaryFirstCallOnFreshFrames(b *testing.B) {
	kinds := canaryFrameKinds()
	for i, floor := range canaryLogFloors {
		env := canaryEnvironment{floor, canaryBuildLabels[i]}
		b.Run(env.name(), func(b *testing.B) {
			if os.Getenv(canaryIsolatedEnv) == b.Name() {
				canaryTimeFreshFrames(b, kinds, env)
				return
			}
			var fastest canaryFreshTimings
			least := func(best *time.Duration, d time.Duration) {
				if d < *best {
					*best = d
				}
			}
			for n := 0; n < b.N; n++ {
				run := canaryBenchInFreshProcess(b)
				if len(run.Calls) != len(kinds) {
					b.Fatalf("a fresh process reported %d calls, want %d", len(run.Calls), len(kinds))
				}
				if n == 0 {
					fastest = run
					continue
				}
				least(&fastest.Walk, run.Walk)
				least(&fastest.First, run.First)
				least(&fastest.Refusal, run.Refusal)
				for k, call := range run.Calls {
					least(&fastest.Calls[k], call)
				}
			}
			for k, kind := range kinds {
				b.Logf("%s: fastest first call %v", kind.name, fastest.Calls[k])
			}
			b.Logf("a build label beyond its bound: fastest first refusal %v; fastest walk %v", fastest.Refusal, fastest.Walk)
			b.ReportMetric(float64(fastest.Walk.Nanoseconds()), "walk-ns")
			b.ReportMetric(float64(fastest.First.Nanoseconds()), "first-call-ns")
			b.ReportMetric(float64(slices.Max(fastest.Calls).Nanoseconds()), "kind-first-call-ns")
			b.ReportMetric(float64(fastest.Refusal.Nanoseconds()), "refusal-ns")
		})
	}
}

// canaryBenchInFreshProcess runs the calling sub-benchmark once more, in a
// child process of its own that reports its timings in a file of its own, and
// returns them. As canaryInFreshProcess's children do, the child runs with the
// parent's current GOMAXPROCS; its watchdog deadline is a fixed five minutes,
// and hitting it is reported as TIMEOUT / HARNESS_FAILURE.
func canaryBenchInFreshProcess(b *testing.B) canaryFreshTimings {
	b.Helper()
	path := filepath.Join(b.TempDir(), "timings.json")
	top, sub, _ := strings.Cut(b.Name(), "/")
	cmd := exec.Command(os.Args[0], "-test.run=^$", "-test.bench=^"+regexp.QuoteMeta(top)+"$/^"+regexp.QuoteMeta(sub)+"$",
		"-test.benchtime=1x", "-test.count=1", "-test.cpu="+strconv.Itoa(runtime.GOMAXPROCS(0)), "-test.timeout=5m")
	// Built with -race, the child would otherwise wait a second before exiting.
	cmd.Env = append(os.Environ(), canaryIsolatedEnv+"="+b.Name(), canaryTimingsEnv+"="+path,
		"GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		if bytes.Contains(out, []byte("panic: test timed out")) {
			b.Fatalf("%s: the fresh process for %s hit its watchdog deadline, which is no verdict on the canary: %v\n%s", canaryWatchdog, b.Name(), err, out)
		}
		b.Fatalf("%s in a fresh process: %v\n%s", b.Name(), err, out)
	}
	var run canaryFreshTimings
	raw, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(raw, &run)
	}
	if err != nil {
		b.Fatalf("the fresh process for %s reported no timings: %v\n%s", b.Name(), err, out)
	}
	return run
}

// canaryTimeFreshFrames is one fresh process of
// BenchmarkWinnerCanaryFirstCallOnFreshFrames, in env. It builds one fresh
// frame of every kind canaryFrameKinds lists — its own message, maps, ids,
// winner and size, with extra keys on the event, in every outcome and in the
// case's object, and the case's array large too — one more positive frame, the
// opening one, with eight times the extra keys, and one more to refuse. Each
// frame's titles and over-bound value, and the build label the last is refused
// for, are a string of its own over 8 MiB long: no two frames' strings share an
// address, a length or their last bytes. After a record of its own warms the
// logger (not the canary), it collects the heap and times one plain walk over a
// separate map of the extra keys, then the emitter the pool calls, once per
// frame: first on the opening frame, then on one frame of every kind in a fixed
// order; then the classifier refusing the last frame for its label. So the
// first call of the process, of every branch and on every frame is timed. Every
// record must be its kind's literal expectation with the hash of the frame's
// own event id, or there must be none (none at all with INFO disabled), and the
// last frame must be refused. The timings go to the file canaryTimingsEnv
// names.
func canaryTimeFreshFrames(b *testing.B, kinds []canaryFrameKind, env canaryEnvironment) {
	// 12,288 extra keys still fit the map size 8,192 would need (a Go map
	// fills to 7/8 of its slots), so the longer walk costs no memory. The
	// frames share the extra key strings and values, not a map, id, message or
	// size. Frame n takes the first extra+n of them, the opening frame eight
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
	// frameText(n) is frame n's titles and over-bound value, and the build
	// label frame n is refused for: the 8 MiB and 4n+5 bytes of texts that
	// start n bytes in and end in n's -%04d, one string for all of them.
	openingN, refusedN := len(kinds), len(kinds)+1
	var tail strings.Builder
	for n := 0; n <= refusedN; n++ {
		fmt.Fprintf(&tail, "-%04d", n)
	}
	texts := strings.Repeat("x", 1<<23) + tail.String()
	frameText := func(n int) string { return texts[n : 1<<23+5*(n+1)] }
	// parts gives every object of frame n the first width extra keys, every
	// array of it 196,608+n elements, and frameText(n) for its titles and its
	// over-bound value.
	parts := func(n, width int) canaryParts {
		wideObject := func(m map[string]interface{}) map[string]interface{} { return widen(m, width) }
		return canaryParts{
			inspected: func(m map[string]interface{}) map[string]interface{} {
				m["title"] = frameText(n)
				return wideObject(m)
			},
			object:      wideObject,
			array:       func() []interface{} { return slices.Repeat([]interface{}{true}, 16*extra+n) },
			predictors:  func() []interface{} { return []interface{}{map[string]interface{}{"points": 500.0}} },
			nest:        func() map[string]interface{} { return map[string]interface{}{} },
			ownOutcomes: func() []interface{} { return []interface{}{} },
			envelope:    canaryAsParsed,
			overlong:    frameText,
			uncounted:   canaryRepeated,
		}
	}
	// The opening frame and the refused one are of the first kind, which must
	// be the positive one.
	if kinds[0].none || kinds[0].want != canaryPositive0001 {
		b.Fatalf("the first kind is %q, want the positive one", kinds[0].name)
	}
	// Every frame stays alive until the end, so no two share an address.
	opening := kinds[0].build(parts(openingN, 8*extra), openingN)
	frames := make([]canaryFrame, len(kinds))
	for k, kind := range kinds {
		frames[k] = kind.build(parts(k, extra+k), k)
	}
	refused := kinds[0].build(parts(refusedN, extra+refusedN), refusedN)
	walked := widen(map[string]interface{}{}, extra)
	// Room for every record up front, so keeping one never grows the slice,
	// and no lock: only this goroutine emits.
	kept := make([]slog.Record, 0, len(kinds)+2)
	env.use(b, canaryProbeHandler{level: env.floor, onRecord: func(r slog.Record) { kept = append(kept, r) }})
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
	walk := canaryBestOf(3, func() { canaryBenchSink += canaryCountKeys(walked) })
	before := time.Now()
	firstCall, openingLogged := emit(opening)
	calls := make([]time.Duration, len(kinds))
	logged := make([][]slog.Record, len(kinds))
	for k, f := range frames {
		calls[k], logged[k] = emit(f)
	}
	label := frameText(refusedN)
	started := time.Now()
	_, admitted := classifyWinnerCanary(refused.msg, refused.status, refused.event, label)
	refusal := time.Since(started)
	after := time.Now()
	canaryAssertEmitted(b, env.wantLogged(canaryWantOf(kinds[0], opening)), openingLogged, 1, before, after)
	for k, kind := range kinds {
		canaryAssertEmitted(b, env.wantLogged(canaryWantOf(kind, frames[k])), logged[k], 1, before, after)
	}
	if admitted {
		b.Fatal("the last frame was admitted with a build label beyond its bound")
	}
	raw, err := json.Marshal(canaryFreshTimings{Walk: walk, First: firstCall, Refusal: refusal, Calls: calls})
	if err == nil {
		err = os.WriteFile(os.Getenv(canaryTimingsEnv), raw, 0o600)
	}
	if err != nil {
		b.Fatalf("reporting the timings: %v", err)
	}
}

// BenchmarkWinnerCanaryRefusals characterizes refusing each frame
// TestWinnerCanaryRefusalTouchesNothing refuses, in every environment
// canaryEnvironments lists, the frame built afresh in each, one refusal per
// operation, beside two passes no refusal needs: one that checks every
// outcome's type (type-pass-ns) and one that compares each outcome id with the
// next (compare-pass-ns). Its timings judge nothing.
func BenchmarkWinnerCanaryRefusals(b *testing.B) {
	for _, shape := range canaryShapes {
		for _, tc := range canaryRefusalCases(shape) {
			b.Run(tc.name+shape.suffix(), func(b *testing.B) {
				for _, env := range canaryEnvironments() {
					b.Run(env.name(), func(b *testing.B) {
						env.use(b, canaryBenchHandler(env.floor))
						build := tc.label(env)
						f := shape.qualifying(tc.event())
						outcomes, _ := f.event["outcomes"].([]interface{})
						typePass := canaryBestOf(3, func() { canaryBenchSink += canaryTypePass(outcomes) })
						comparePass := canaryBestOf(3, func() { canaryBenchSink += canaryComparePass(outcomes) })
						b.ReportAllocs()
						b.ResetTimer()
						for i := 0; i < b.N; i++ {
							if _, ok := classifyWinnerCanary(f.msg, f.status, f.event, build); ok {
								b.Fatal("an over-bound frame was admitted")
							}
						}
						b.ReportMetric(float64(typePass.Nanoseconds()), "type-pass-ns")
						b.ReportMetric(float64(comparePass.Nanoseconds()), "compare-pass-ns")
					})
				}
			})
		}
	}
}
