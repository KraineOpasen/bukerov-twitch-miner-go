package web

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file checks the Logs page classifier against the miner's REAL logging
// corpus rather than a hand-picked sample. It harvests every literal
// slog.Debug/Info/Warn/Error message in the tree and asserts the two
// invariants that must hold for messages nobody has thought about yet —
// including messages added long after this change.
//
// It is deliberately implemented in pure Go (no grep/bash) so it behaves the
// same on Linux, macOS and Windows.

// slogCall matches a package-level slog call whose first argument is a plain
// string literal. Calls whose msg is built at runtime (concatenation, a
// variable) are skipped: there is no literal to classify.
var slogCallRe = regexp.MustCompile(`slog\.(Debug|Info|Warn|Error)\(\s*("(?:[^"\\]|\\.)*")(\s*\+)?`)

type corpusMsg struct {
	level string // DEBUG | INFO | WARN | ERROR
	msg   string
	where string
}

// harvestSlogCorpus walks the module for non-test Go sources and returns every
// distinct (level, literal msg) pair the miner can emit.
func harvestSlogCorpus(t *testing.T) []corpusMsg {
	t.Helper()

	root := moduleRoot(t)
	seen := map[string]corpusMsg{}
	for _, dir := range []string{"internal", "cmd"} {
		walkRoot := filepath.Join(root, dir)
		if _, err := os.Stat(walkRoot); err != nil {
			continue
		}
		err := filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return fmt.Errorf("relative path of %s under %s: %w", path, root, err)
			}
			for _, m := range slogCallRe.FindAllSubmatch(src, -1) {
				// A literal immediately followed by "+" is only the first
				// fragment of a message assembled at runtime; the complete
				// message never exists as a literal, so recording the fragment
				// would make the dead-entry guard report nonsense.
				if len(m[3]) > 0 {
					continue
				}
				msg, err := strconv.Unquote(string(m[2]))
				if err != nil || msg == "" {
					continue
				}
				level := strings.ToUpper(string(m[1]))
				key := level + "\x00" + msg
				if _, ok := seen[key]; !ok {
					seen[key] = corpusMsg{level: level, msg: msg, where: filepath.ToSlash(rel)}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", walkRoot, err)
		}
	}

	out := make([]corpusMsg, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].level != out[j].level {
			return out[i].level < out[j].level
		}
		return out[i].msg < out[j].msg
	})
	return out
}

// moduleRoot locates the directory holding go.mod. It tries the working
// directory first — `go test` runs each test binary in its package directory,
// which works under -trimpath — and falls back to this file's compiled-in
// path for any runner that sets a different working directory.
func moduleRoot(t *testing.T) string {
	t.Helper()

	var starts []string
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	if _, self, _, ok := runtime.Caller(0); ok {
		starts = append(starts, filepath.Dir(self))
	}

	for _, dir := range starts {
		for i := 0; i < 12; i++ {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	t.Fatalf("could not locate go.mod from any of %v", starts)
	return ""
}

// corpusLine renders a harvested message as the slog TextHandler would write
// it to the retained file, so the classifier sees a realistic line.
func corpusLine(c corpusMsg) string {
	return fmt.Sprintf("time=2026-07-14T10:00:00.000+03:00 level=%s msg=%s", c.level, strconv.Quote(c.msg))
}

// TestCorpusNoWarnOrErrorIsEverHidden is the safety net for the whole
// suppression feature, checked against every WARN/ERROR the miner can
// actually emit rather than a sample. A future message added anywhere in the
// tree is covered automatically: if any suppression rule ever swallows a
// WARN or an ERROR, this fails and names it.
func TestCorpusNoWarnOrErrorIsEverHidden(t *testing.T) {
	corpus := harvestSlogCorpus(t)
	if len(corpus) < 200 {
		t.Fatalf("harvested only %d slog messages; the corpus scan is broken", len(corpus))
	}

	checked := 0
	for _, c := range corpus {
		if c.level != "WARN" && c.level != "ERROR" {
			continue
		}
		checked++
		got := classifyLogLine(corpusLine(c))
		if !got.DashboardVisible {
			t.Errorf("%s %s is hidden from the dashboard (%s) — a WARN/ERROR is never suppressed", c.level, strconv.Quote(c.msg), c.where)
		}
		wantLevel := levelWarning
		if c.level == "ERROR" {
			wantLevel = levelError
		}
		if got.Level != wantLevel {
			t.Errorf("%s %s classified as level %q, want %q (%s) — severity is never downgraded",
				c.level, strconv.Quote(c.msg), got.Level, wantLevel, c.where)
		}
	}
	if checked < 100 {
		t.Fatalf("only %d WARN/ERROR messages harvested; the corpus scan is broken", checked)
	}
	t.Logf("verified %d distinct production WARN/ERROR messages stay visible", checked)
}

// TestCorpusEverySubsystemIsSupported asserts the classifier can never emit a
// subsystem outside the taxonomy the Logs page filter offers, for any message
// the miner really logs.
func TestCorpusEverySubsystemIsSupported(t *testing.T) {
	supported := map[string]bool{}
	for _, s := range allLogSubsystems() {
		supported[s] = true
	}
	styled := map[string]bool{}
	for _, c := range allLogLineClasses() {
		styled[c] = true
	}

	for _, c := range harvestSlogCorpus(t) {
		got := classifyLogLine(corpusLine(c))
		if !supported[got.Subsystem] {
			t.Errorf("%s %s -> unsupported subsystem %q (%s)", c.level, strconv.Quote(c.msg), got.Subsystem, c.where)
		}
		if !styled[got.Class] {
			t.Errorf("%s %s -> class %q is not one allLogLineClasses() reports, so it may render unstyled (%s)",
				c.level, strconv.Quote(c.msg), got.Class, c.where)
		}
		if got.Level == "" {
			t.Errorf("%s %s -> empty level (%s)", c.level, strconv.Quote(c.msg), c.where)
		}
	}
}

// TestCorpusDropProgressShapeMatchesNothingElse guards the one rule that
// matches by SHAPE rather than by literal. A shape matcher is only safe while
// it is precise, so assert no other message the miner logs is caught by it.
func TestCorpusDropProgressShapeMatchesNothingElse(t *testing.T) {
	for _, c := range harvestSlogCorpus(t) {
		if isDropProgressLine(c.msg) {
			t.Errorf("the drop-progress shape matcher also captures %s %s (%s)", c.level, strconv.Quote(c.msg), c.where)
		}
	}

	// ...and it must still recognize the real shapes internal/drops builds
	// (with and without the game segment, and at both ends of the range).
	for _, real := range []string{
		"World of Tanks [cyganzor] AMD Summer Arena Drops#2: -----------> 55%",
		"[cyganzor] AMD Summer Arena Drops#2: > 0%",
		"World of Tanks [cyganzor] AMD Summer Arena Drops#2: -------------------> 100%",
	} {
		if !isDropProgressLine(real) {
			t.Errorf("the drop-progress shape matcher does not recognize %q", real)
		}
	}
}

// TestCorpusRuleTablesHaveNoDeadExactEntries guards a whole bug class. An
// `exact` rule whose literal has drifted (a clause appended to the message,
// or a message assembled by concatenation) silently stops matching, and the
// event quietly falls back to the "other" bucket — which is exactly how two
// streamer-deletion ERRORs were mis-bucketed before this test existed.
//
// Every `exact` entry must match a real production message. An entry that
// only ever appears as the START of a longer real message is the drift case
// and must be declared as a `prefix` instead.
func TestCorpusRuleTablesHaveNoDeadExactEntries(t *testing.T) {
	corpus := harvestSlogCorpus(t)
	literals := make(map[string]bool, len(corpus))
	for _, c := range corpus {
		literals[c.msg] = true
	}

	// Note: messages the miner assembles at runtime (e.g. the drops sync
	// summary, built in a variable) are declared as prefixes, never as exact
	// entries, so they are correctly out of scope here.
	check := func(table string, i int, exact []string) {
		for _, e := range exact {
			if literals[e] {
				continue
			}
			drifted := ""
			for lit := range literals {
				if strings.HasPrefix(lit, e) {
					drifted = lit
					break
				}
			}
			if drifted != "" {
				t.Errorf("%s[%d]: exact entry %q never matches — the real message is %q. Declare it as a prefix.",
					table, i, e, drifted)
				continue
			}
			// Not a prefix of anything real either: the message was removed
			// from the miner and the rule outlived it.
			t.Errorf("%s[%d]: exact entry %q matches no production message — the rule is dead and should be removed.", table, i, e)
		}
	}
	for i, r := range logMsgRules {
		check("logMsgRules", i, r.exact)
	}
	for i, r := range logSubsystemRules {
		check("logSubsystemRules", i, r.exact)
	}
}
