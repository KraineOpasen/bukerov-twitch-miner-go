package p4offline_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// retiredRegisters are comment shapes this package retires. FOUR of the five
// are shapes of the rule canonical.go states about a comment's own history;
// the fifth is not that rule at all and is described as its own thing below.
//
// THE FIRST FOUR ARE ANCHORED ON THE OBJECT, NOT ON THE VERB, and that is the
// whole design of those four. The rule has two halves: a comment MAY say what
// the CODE used to do, because published history can be checked, and may NOT
// say what a COMMENT used to say. "An earlier version interned instead" and
// "an earlier version of this paragraph said 29" share every word that matters
// to a verb-anchored pattern and fall on opposite sides of the rule. So each
// of those four requires a word for a piece of PROSE -- wording, sentence,
// paragraph, bullet, header, comment, note -- in the position the sentence is
// ABOUT. THE FIFTH IS A DIFFERENT RETIREMENT: it carries no prose noun and is
// not about a comment's history, but about a commit this package does not
// exist at, which canonical.go states as a separate practice rather than as
// part of that rule. It rides in this register because the mechanism -- a
// parsed scan with an asserted count -- is the same one, not because the rule
// is.
//
// WHAT EACH ONE IS FOR, one description per pattern, in order.
//
// The first is a comment's own phrasing named directly: "an earlier wording"
// cites an authoring state this repository never recorded.
//
// The second is the same thing at one remove -- "an earlier version OF this
// paragraph" -- which is the form the same sentence takes when it wants to
// sound like it is about the code.
//
// The third is that shape in the other grammatical direction: "this sentence
// no longer claims" is a statement about a comment's own history, and a repair
// written that way is a fresh instance of the class it repairs.
//
// The fourth is the rule's own subject stated plainly: what a comment used to
// say.
//
// The fifth names a commit where this package does not exist. Every file here
// is added on this branch, so "at the base commit" sends a reader to bd4d2727
// to check something that is not there.
var retiredRegisters = []struct {
	name string
	re   *regexp.Regexp
}{
	{"a comment's own earlier phrasing", regexp.MustCompile(`(?i)\ban earlier (?:wording|phrasing|sentence|paragraph|bullet|header|comment)\b`)},
	{"an earlier version OF a piece of prose", regexp.MustCompile(`(?i)\ban earlier (?:version|draft|revision) of (?:this|the) (?:comment|sentence|paragraph|bullet|wording|header|line|note)\b`)},
	{"what a piece of prose used to say", regexp.MustCompile(`(?i)\b(?:this|the) (?:sentence|comment|wording|header|paragraph|bullet|line|note) (?:no longer|used to|once|has already)\b`)},
	{"what a comment used to say", regexp.MustCompile(`(?i)\ba comment (?:in \S+ )?used to (?:say|assert|claim|read|describe|omit)\b`)},
	{"a commit where this package does not exist", regexp.MustCompile(`(?i)\bat the base commit\b`)},
}

// retiredRegisterInstances is how many matches of those shapes this package
// carries, counting every file including this one. IT IS A RATCHET AND NOT A
// TARGET: the assertion is an equality, so retiring one fails this test until
// the number is lowered, and writing a new one fails it until the sentence is
// cut.
//
// WHAT THIS NUMBER IS NOT. It is not a count of every violation of
// canonical.go's rule. These are five literal phrasings; the rule is a
// semantic class, and no regexp decides a semantic class. A fresh violation in
// words nobody has used yet -- "a previous draft of this paragraph said 29" is
// one -- passes this test, and the register grows a pattern when one is found
// rather than pretending it was covered. What the equality does hold is that
// none of these five shapes can be added in silence, and that retiring one is
// recorded.
//
// NO COUNT OF FILES IS WRITTEN BESIDE IT, because a file count here would be
// a hand-counted number sitting inside the mechanism that exists to stop
// hand-counted numbers. The match count is ASSERTED and therefore checked;
// anything beside it that is not asserted is the same hazard wearing the
// mechanism's clothes. A reader who wants the file count gets it from the
// failure message below, which prints every hit with its file.
//
// THIS FILE'S OWN HITS ARE COUNTED, NOT SUBTRACTED. canonical.go says a
// comment "may not state what a COMMENT used to say", and that sentence
// matches the shape it forbids; so does every description above. Exempting
// the file the mechanism lives in would make it the one place a real instance
// could hide, so the count includes it and the guard below requires it.
const retiredRegisterInstances = 33

type registerHit struct{ file, register, quote string }

var registerWhitespace = regexp.MustCompile(`\s+`)

// registerHitsIn returns every retired-register match in one parsed file.
//
// EACH COMMENT GROUP IS MATCHED ON ITS OWN, and its internal line breaks are
// collapsed first so a sentence wrapped across several `//` lines is seen.
// Separate groups are NOT joined: a sentence does not span two comments, and
// joining them invents matches present in neither.
// TestTheRegisterDoesNotJoinSeparateComments holds that, because on this
// package's own files joining every group changes nothing and the mechanism
// would otherwise be a mechanism nothing exercises.
func registerHitsIn(file string, f *ast.File) []registerHit {
	var hits []registerHit
	for _, group := range f.Comments {
		text := registerWhitespace.ReplaceAllString(group.Text(), " ")
		for _, r := range retiredRegisters {
			for _, m := range r.re.FindAllStringIndex(text, -1) {
				lo := m[0] - 60
				if lo < 0 {
					lo = 0
				}
				hi := m[1] + 40
				if hi > len(text) {
					hi = len(text)
				}
				hits = append(hits, registerHit{file, r.name, text[lo:hi]})
			}
		}
	}
	return hits
}

// TestTheRetiredCommentRegisterOnlyShrinks scans this package's own comments
// for the shapes canonical.go retires.
//
// IT PARSES THE COMMENTS RATHER THAN MATCHING LINES, which is the whole reason
// it exists as a test rather than as a grep anybody could run. A line-oriented
// search sees neither of the two forms that matter here: a sentence wrapped
// across several `//` lines, and a trailing comment after code, of which this
// package has dozens. A scanner here took only lines whose trimmed text began
// with `//`, and a review lane demonstrated the hole by adding three instances
// of the retired register as trailing comments and watching the suite stay
// green. go/parser hands back every comment group in the file, whatever its
// form and wherever it sits, so the blind spot is closed by construction
// rather than by a wider regexp.
func TestTheRetiredCommentRegisterOnlyShrinks(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var hits []registerHit
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		hits = append(hits, registerHitsIn(name, f)...)
	}
	// THE SCANNER MUST SEE ITSELF, PATTERN BY PATTERN -- AS A DIAGNOSIS AND
	// NOT AS A SECOND CHECK. The descriptions above state all five shapes in
	// prose, so each pattern must match in this file. What that buys is the
	// failure message: a broken pattern is already caught by the equality
	// below, because its hits vanish and the count falls, and reverting this
	// loop to a whole-file guard leaves the suite green for exactly that
	// reason. It is kept because "register X matched nothing in the file that
	// states it" is a diagnosis and "the count is three lower than it was" is
	// a puzzle, and it is described as a diagnosis because a second-order
	// guard argued as a check is how two guards in this package came to be
	// called unkillable when an operator flip killed both.
	for _, r := range retiredRegisters {
		seen := false
		for _, h := range hits {
			if h.file == "register_test.go" && h.register == r.name {
				seen = true
				break
			}
		}
		if !seen {
			t.Fatalf("register %q is stated in this file's own prose and matched nothing in it: "+
				"the parsing or that pattern is broken, and a low count would be an artifact rather than a result", r.name)
		}
	}
	if got := len(hits); got != retiredRegisterInstances {
		var lines []string
		for _, h := range hits {
			lines = append(lines, "  "+h.file+"  ["+h.register+"]  ..."+h.quote+"...")
		}
		t.Fatalf("this package carries %d matches of the retired comment register, retiredRegisterInstances says %d.\nThe count is a RATCHET: lower it when you retire one, and do not raise it.\n%s",
			got, retiredRegisterInstances, strings.Join(lines, "\n"))
	}
}

// TestTheRegisterDoesNotJoinSeparateComments holds the one mechanism of
// registerHitsIn that this package's own files cannot hold. Joining every
// comment group in any file here leaves the count at exactly what it is, so
// the separation could be dropped with the whole suite green -- the failure
// mode censusOfDir's synthetic fixture exists to rule out, one instrument
// over.
//
// The fixture is two adjacent groups that match nothing apart and match the
// third register when concatenated, plus a control that matches on its own so
// a fixture passing because matching is broken fails here instead.
func TestTheRegisterDoesNotJoinSeparateComments(t *testing.T) {
	const src = `package p

// A group that ends on the bare words the header
var a int

// used to assert something, says the group that follows it
var b int

// A control: an earlier wording, which matches on its own.
var c int
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	hits := registerHitsIn("fixture.go", f)
	if len(hits) != 1 || hits[0].register != retiredRegisters[0].name {
		var got []string
		for _, h := range hits {
			got = append(got, h.register+": ..."+h.quote+"...")
		}
		t.Fatalf("the fixture's two groups match nothing apart and the control matches once, so this must be exactly one hit of %q; got %d:\n  %s",
			retiredRegisters[0].name, len(hits), strings.Join(got, "\n  "))
	}
}
