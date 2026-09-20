package p4offline_test

// THE DEPENDENCY FENCE.
//
// This package claims to be a pure function of its arguments: no database,
// no network, no clock, no environment, no global RNG, no goroutines. Five
// rules enforce it; the first four work over the WHOLE transitive import
// graph of the production files, and the fifth over their syntax:
//
//	Rule A — the only first-party import is internal/predictioneval, whose
//	         own fence already excludes every capability package.
//	Rule B — direct imports come from an exact allowlist.
//	Rule C — the transitive closure contains no capability package.
//	Rule D — every reachable standard-library package is pinned, so a
//	         package nobody thought to deny cannot arrive unnoticed.
//	Rule E — no production file contains a `go` statement.
//
// RULE E EXISTS BECAUSE THE SENTENCE ABOVE WAS FIVE-SIXTHS TRUE. Five of the
// six clauses are import-borne, so Rules A–D really do decide them: two
// independent verifiers confirmed that adding `import "time"` or
// `import "sync"` to a production file fails Rule B in 0.04 s. The sixth is
// not. A `go` statement and a channel need NO import, so a goroutine
// launched on a hot path — probed by adding one to (*canonical).digest —
// passed all four rules, `go vet`, `gofmt` and the whole package suite under
// -race, with the fence's own log line byte-identical. The clause was a
// convention presented as a machine check.
//
// What Rule E is NOT: it is a syntactic check over THIS package's production
// files, not a proof that nothing in the closure ever starts a goroutine.
// That stronger claim is not made, and does not need to be — Rules B and D
// pin the closure, so anything that could start one would have to arrive as a
// reviewed import first.
//
// HONEST RESIDUE: crypto/sha256 transitively reaches internal/poll, io/fs,
// os, syscall and time through the FIPS-140 integrity check and the entropy
// source, and encoding/json — needed to bind the SUPPLIED raw ruleset bytes
// to their typed form — reaches reflect (and fmt, which reaches os again).
// None of these is called on any path this package executes; reflect is not
// a capability package. Both attributions are verified per root below rather
// than asserted.
//
// Rule D is EXPECTED to trip on a Go toolchain upgrade: a change to what the
// allowed imports drag in is exactly the event that should be reviewed rather
// than absorbed. Re-derive the set, confirm nothing capability-bearing
// appeared, and update the pin in the same commit as the upgrade.

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/KraineOpasen/bukerov-twitch-miner-go"

// allowedDirectImports is the complete set the production files may import.
var allowedDirectImports = map[string]string{
	"bytes":                                 "reading and trimming the supplied raw ruleset bytes",
	"crypto/hmac":                           "p4-hmac-sha256-u64/v1",
	"crypto/sha256":                         "every digest in the package",
	"encoding/json":                         "binding the supplied raw ruleset bytes to their typed config",
	"errors":                                "typed sentinel errors",
	"math":                                  "float bit patterns for exact comparison and digests",
	"sort":                                  "deterministic ordering, independent of map iteration",
	"strconv":                               "rendering numbers without fmt",
	"unicode/utf8":                          "refusing a supplied identity the declared JSON encoding cannot carry",
	modulePath + "/internal/predictioneval": "the P2 and P3b contracts this package adapts",
}

// deniedTransitiveImports are capability packages.
var deniedTransitiveImports = []string{
	"database/sql", "database/sql/driver",
	"net", "net/http", "net/url",
	"os/exec", "os/signal",
	"math/rand", "math/rand/v2",
	"context",
	"log", "log/slog",
	"io/ioutil",
	"testing",
	modulePath + "/internal/analytics",
	modulePath + "/internal/pubsub",
	modulePath + "/internal/database",
	modulePath + "/internal/twitch",
	modulePath + "/internal/settings",
	modulePath + "/internal/miner",
	modulePath + "/internal/web",
	modulePath + "/internal/predictioneval/reader",
	"modernc.org/sqlite",
}

// knownStdlibResidue is the reachable sensitive set, sorted.
var knownStdlibResidue = []string{"internal/poll", "io/fs", "os", "reflect", "syscall", "time"}

// sensitiveWatchlist is deliberately WIDER than the residue.
var sensitiveWatchlist = []string{
	"context",
	"database/sql", "database/sql/driver",
	"internal/poll",
	"io/fs", "io/ioutil",
	"log", "log/slog",
	"math/rand", "math/rand/v2",
	"net", "net/http", "net/url",
	"os", "os/exec", "os/signal", "os/user",
	"plugin",
	"reflect",
	"runtime/debug", "runtime/pprof",
	"syscall",
	"testing",
	"time",
}

// knownStdlibClosure is the COMPLETE transitive standard-library closure of
// the production files' direct imports, pinned exactly.
var knownStdlibClosure = []string{
	"bytes",
	"cmp",
	"crypto",
	"crypto/cipher",
	"crypto/fips140",
	"crypto/hmac",
	"crypto/internal/boring",
	"crypto/internal/boring/sig",
	"crypto/internal/constanttime",
	"crypto/internal/entropy/v1.0.0",
	"crypto/internal/fips140",
	"crypto/internal/fips140/aes",
	"crypto/internal/fips140/aes/gcm",
	"crypto/internal/fips140/alias",
	"crypto/internal/fips140/check",
	"crypto/internal/fips140/drbg",
	"crypto/internal/fips140/hmac",
	"crypto/internal/fips140/sha256",
	"crypto/internal/fips140/sha3",
	"crypto/internal/fips140/sha512",
	"crypto/internal/fips140/subtle",
	"crypto/internal/fips140deps/byteorder",
	"crypto/internal/fips140deps/cpu",
	"crypto/internal/fips140deps/godebug",
	"crypto/internal/fips140deps/time",
	"crypto/internal/fips140hash",
	"crypto/internal/fips140only",
	"crypto/internal/impl",
	"crypto/internal/sysrand",
	"crypto/sha256",
	"crypto/sha3",
	"crypto/subtle",
	"encoding",
	"encoding/base64",
	"encoding/json",
	"errors",
	"fmt",
	"hash",
	"internal/abi",
	"internal/asan",
	"internal/bisect",
	"internal/bytealg",
	"internal/byteorder",
	"internal/chacha8rand",
	"internal/coverage/rtcov",
	"internal/cpu",
	"internal/filepathlite",
	"internal/fmtsort",
	"internal/goarch",
	"internal/godebug",
	"internal/godebugs",
	"internal/goexperiment",
	"internal/goos",
	"internal/msan",
	"internal/oserror",
	"internal/poll",
	"internal/profilerecord",
	"internal/race",
	"internal/reflectlite",
	"internal/runtime/atomic",
	"internal/runtime/cgroup",
	"internal/runtime/exithook",
	"internal/runtime/gc",
	"internal/runtime/gc/scan",
	"internal/runtime/maps",
	"internal/runtime/math",
	"internal/runtime/pprof/label",
	"internal/runtime/sys",
	"internal/runtime/syscall/linux",
	"internal/strconv",
	"internal/stringslite",
	"internal/sync",
	"internal/synctest",
	"internal/syscall/execenv",
	"internal/syscall/unix",
	"internal/testlog",
	"internal/trace/tracev2",
	"internal/unsafeheader",
	"io",
	"io/fs",
	"iter",
	"math",
	"math/bits",
	"os",
	"path",
	"reflect",
	"runtime",
	"slices",
	"sort",
	"strconv",
	"strings",
	"sync",
	"sync/atomic",
	"syscall",
	"time",
	"unicode",
	"unicode/utf16",
	"unicode/utf8",
}

func TestP4OfflineDependencyFence(t *testing.T) {
	direct := directImportsOfProductionFiles(t, ".")

	// Rule E, before the import rules, because it is the one the header used
	// to assert and nothing checked.
	if sites, files := goStatementsInProductionFiles(t, "."); len(sites) != 0 {
		t.Errorf("the production files start goroutines at %v; this package claims to have none, and Rule E is what makes that claim a check", sites)
	} else if files == 0 {
		t.Fatal("no production files were parsed; Rule E would pass vacuously")
	} else {
		t.Logf("Rule E: %d production files carry no go statement", files)
	}

	for _, imp := range direct {
		if _, ok := allowedDirectImports[imp]; !ok {
			t.Errorf("the production files import %q, which is not on the purity allowlist", imp)
		}
		if strings.HasPrefix(imp, modulePath) && imp != modulePath+"/internal/predictioneval" {
			t.Errorf("the production files import the first-party package %q; only internal/predictioneval is admitted", imp)
		}
	}

	closure := transitiveClosure(t, direct)
	denied := map[string]bool{}
	for _, d := range deniedTransitiveImports {
		denied[d] = true
	}
	for pkg := range closure {
		if denied[pkg] {
			t.Errorf("the production files transitively reach %q", pkg)
		}
	}

	pinned := map[string]bool{}
	for _, p := range knownStdlibClosure {
		pinned[p] = true
	}
	var unexpected, vanished []string
	for pkg := range closure {
		if strings.HasPrefix(pkg, modulePath) {
			continue
		}
		if !pinned[pkg] {
			unexpected = append(unexpected, pkg)
		}
	}
	for _, p := range knownStdlibClosure {
		if !closure[p] {
			vanished = append(vanished, p)
		}
	}
	sort.Strings(unexpected)
	sort.Strings(vanished)
	if len(unexpected) > 0 {
		t.Errorf("the transitive closure gained %v; confirm none carries a capability, then pin them", unexpected)
	}
	if len(vanished) > 0 {
		t.Errorf("the transitive closure no longer reaches %v; drop them from the pin", vanished)
	}

	var residue []string
	for _, p := range sensitiveWatchlist {
		if closure[p] {
			residue = append(residue, p)
		}
	}
	sort.Strings(residue)
	want := append([]string(nil), knownStdlibResidue...)
	sort.Strings(want)
	if strings.Join(residue, ",") != strings.Join(want, ",") {
		t.Errorf("the reachable sensitive set changed.\n reachable  = %v\n documented = %v", residue, want)
	}
	t.Logf("direct imports (%d): %v; closure %d packages; residue %v", len(direct), direct, len(closure), residue)
}

// TestTheFenceWouldActuallyCatchAForbiddenImport proves the fence is not
// vacuous: the transitive walk finds a denied package when one is reachable.
func TestTheFenceWouldActuallyCatchAForbiddenImport(t *testing.T) {
	for _, forbidden := range []string{"database/sql", "net/http", "math/rand", "time", "context", modulePath + "/internal/analytics"} {
		if _, ok := allowedDirectImports[forbidden]; ok {
			t.Fatalf("%q is on the allowlist", forbidden)
		}
	}
	if !transitiveClosure(t, []string{"database/sql"})["context"] {
		t.Fatal("the transitive walk did not reach context from database/sql; Rule C is not inspecting the graph")
	}
}

// TestRuleEWouldActuallyCatchAGoroutine is Rule E's own control, and it is the
// reason Rule E is a check rather than a second assertion. A `go` statement
// needs no import, so nothing in Rules A–D, in `go vet`, in `gofmt` or in the
// -race suite observes one: that was measured, on a goroutine added to a hot
// path, before this rule existed.
//
// The scanner is run against a file written into a temporary directory rather
// than against this package, so the control never mutates the tree it guards.
func TestRuleEWouldActuallyCatchAGoroutine(t *testing.T) {
	dir := t.TempDir()
	const withGoroutine = `package probe

var sink = make(chan int, 1)

func probe() int {
	go func() { sink <- 1 }()
	return <-sink
}
`
	if err := os.WriteFile(filepath.Join(dir, "probe.go"), []byte(withGoroutine), 0o600); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	if sites, files := goStatementsInProductionFiles(t, dir); len(sites) != 1 || files != 1 {
		t.Fatalf("Rule E saw %d go statements in %d files, want 1 in 1; the goroutine clause is an assertion again", len(sites), files)
	}

	// And the negative half: a file with a channel, a package-level mutable
	// map and no `go` statement must pass, so Rule E is not merely failing on
	// everything.
	clean := t.TempDir()
	const withoutGoroutine = `package probe

var sink = make(chan int, 1)
var memo = map[string]string{}

func probe() int {
	sink <- 1
	memo["k"] = "v"
	return <-sink
}
`
	if err := os.WriteFile(filepath.Join(clean, "probe.go"), []byte(withoutGoroutine), 0o600); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	if sites, files := goStatementsInProductionFiles(t, clean); len(sites) != 0 || files != 1 {
		t.Fatalf("Rule E reported %v in %d files for a file with no go statement", sites, files)
	}
}

// TestTheDocumentedResidueIsAttributableToItsRoots checks the residue
// paragraph's claim about WHERE each entry comes from.
func TestTheDocumentedResidueIsAttributableToItsRoots(t *testing.T) {
	hash := transitiveClosure(t, []string{"crypto/sha256"})
	jsonc := transitiveClosure(t, []string{"encoding/json"})
	for _, p := range []string{"internal/poll", "io/fs", "os", "syscall", "time"} {
		if !hash[p] {
			t.Errorf("%q is documented as residue of crypto/sha256 but the hash does not reach it", p)
		}
	}
	if !jsonc["reflect"] {
		t.Error("reflect is documented as residue of encoding/json but json does not reach it")
	}
	for imp := range allowedDirectImports {
		if imp == "crypto/sha256" || imp == "crypto/hmac" || imp == "encoding/json" || strings.HasPrefix(imp, modulePath) {
			continue
		}
		closure := transitiveClosure(t, []string{imp})
		for _, p := range knownStdlibResidue {
			if closure[p] {
				t.Errorf("%q reaches %q, so that residue entry is not attributable to the hash or to json alone", imp, p)
			}
		}
	}
}

// goStatementsInProductionFiles is Rule E: it parses each production file in
// full -- not ImportsOnly, which is what let this escape -- and fails on any
// `go` statement. SkipObjectResolution keeps the parse cheap; a GoStmt is a
// syntactic node, so no type information is needed to find one.
func goStatementsInProductionFiles(t *testing.T, dir string) (sites []string, files int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if g, ok := n.(*ast.GoStmt); ok {
				sites = append(sites, name+":"+fset.Position(g.Go).String())
			}
			return true
		})
	}
	return sites, files
}

func directImportsOfProductionFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	set := map[string]bool{}
	files := 0
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", name, err)
			}
			set[path] = true
		}
	}
	if files == 0 {
		t.Fatal("no production files were parsed; the fence would pass vacuously")
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// transitiveClosure walks the import graph with go/build, following the
// first-party predictioneval package by source directory so its own stdlib
// imports are included.
func transitiveClosure(t *testing.T, roots []string) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	var walk func(string)
	walk = func(p string) {
		if seen[p] || p == "C" || p == "unsafe" {
			return
		}
		seen[p] = true
		var pkg *build.Package
		var err error
		if strings.HasPrefix(p, modulePath) {
			pkg, err = build.Default.ImportDir(filepath.Join("..", "..", "..", strings.TrimPrefix(p, modulePath+"/")), 0)
		} else {
			pkg, err = build.Default.Import(p, "", 0)
		}
		if err != nil {
			// An import the walk cannot resolve is not a leaf: its own
			// imports would be missing from the closure and the fence
			// would pass over them.
			t.Fatalf("resolve import %q: %v", p, err)
		}
		for _, imp := range pkg.Imports {
			walk(imp)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return seen
}

// extentNames is the family of helpers that write an extent sentence, both of
// which this census counts.
//
// THERE ARE TWO BECAUSE ONE EXTENT CANNOT BE MEASURED off a string in hand:
// decodeFault must say how wide a decoder's sentence is without building it,
// so it computes the width and calls suppliedExtent. Counting only the string
// spelling would leave a second way to write an extent that nothing counts,
// which is the whole failure mode this census exists to prevent.
var extentNames = map[string]bool{"suppliedTextExtent": true, "suppliedExtent": true}

// suppliedTextExtentCensus is how many production call sites of the extent
// helpers sit in each function.
//
// It lives here, executable, because a count maintained in prose drifts from
// the source it describes -- and prose ABOUT that drift drifts just as fast, so
// none is written here. This is Rule E's lesson applied to a census: a
// convention presented as a machine check should be a machine check.
//
// TestTheCensusWalkSeesWhatItClaimsTo holds the walk itself to synthetic
// sources, because this package's own files cannot: every real call site is a
// plain direct call in an ordinary func in a file no build tag excludes.
var suppliedTextExtentCensus = map[string]int{
	// THE DELEGATION IS COUNTED AND NOT EXEMPTED. suppliedTextExtent's body
	// calls suppliedExtent, which is a place this package writes an extent
	// like any other. Exempting the file a mechanism lives in is how the
	// comment register's first build hid five of its own instances, and that
	// lesson is cheaper to apply here than to relearn.
	"canonical.go/suppliedTextExtent":       1, // the string spelling delegating to the computed one
	"evidence.go/VerifySourceRoundRegistry": 2, // the Version clause and the digest-shape gate
	"factset.go/factsetAdmissionFault":      3, // contract, protocol, the digest's shape
	"factset.go/VerifyCommonFactset":        1, // the completeness vocabulary; the three above moved to the admission helper the evaluators hoist
	"factset.go/checkFactsetConsistency":    2, // the stealth-proof arm and the completeness arm
	"p3b.go/decodeFault":                    2, // the computed sentence width, and the rendered message
	"p3b.go/walkRulesetObject":              1, // the unknown-key gate
	"placement.go/decisionOf":               1, // the result's own factset digest
	// The local-error arm names the producer's error class by its EXTENT. The
	// class is supplier text with no vocabulary to match it against, so it is
	// withheld outright rather than recognized-or-withheld the way a policy
	// name is; see PlacementReasonLocalErrorClassWithheldPrefix.
	"placement.go/derivePlacement": 1,
	// The two rows below are the extents a REFUSED decision's artifact
	// reports in place of the identity it withholds: five fields both
	// evidence seams echo, and the derivation only the payout seam does.
	"placement.go/refusedDecisionIdentity":               5,
	"placement.go/refusedDecisionIdentityWithDerivation": 1,
	"resolution.go/VerifyResolutionArtifact":             5, // contract, obligations, the digest's shape, the hoisted outcome gate, the outcome arm
}

// firstGateTableRows is how many rows TestFirstGatesDoNotMaterializeSuppliedText
// carries. The test asserts its own length against this, so the two inventories
// that quote it cannot drift from it in silence.
const firstGateTableRows = 12

// censusOfDir walks the production files of one directory and returns how many
// times each function mentions suppliedTextExtent, plus how many files it read.
//
// It is a function rather than the body of the test below because the test
// below is where almost all of it is exercised: this package's call sites are
// every one of them plain direct calls inside ordinary funcs in files no build
// tag excludes, so the mechanisms that give the walk its coverage -- the
// build-excluded key, the test-file exclusion, the identifier match, the
// receiver in the key, the package-level bucket, the selector skip and the
// walk of its qualifier, the field skip and the walk of its type, the
// declaration skip and the walk of its body -- are invisible to a scan of this
// package's own files. All but TWO. The declaration skip is exercised by
// canonical.go's own suppliedTextExtent declaration, and reverting it makes the
// real census gain a row for the helper itself. The identifier match is
// exercised by every site there is, so DELETING it empties the census and the
// real scan fails -- though NARROWING it to a bare CallExpr does not, because
// every real site is already a bare direct call. Two mutants of one mechanism,
// and only one of them is visible from here: which is the whole reason the
// synthetic fixture exists. Remove the test below and the other nine survive.
// A machine check whose own mechanisms nothing asserts is a convention again,
// which is the thing this census exists to stop being.
func censusOfDir(t *testing.T, dir string) (map[string]int, int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	got, files := map[string]int{}, 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// A FILE THE BUILD EXCLUDES IS NOT A PRODUCTION CALL SITE OF THIS
		// BUILD, because parser.ParseFile applies no build constraints and a
		// `//go:build never` probe would otherwise count. It is not nothing
		// either: build.Default carries neither the tags the test binary was
		// built with nor the GOOS this project cross-compiles for, so DROPPING
		// an excluded file lets a real call site vanish from a census whose
		// claim is that none can. Excluded files are walked under their own
		// key instead. A probe must still be declared, and a site behind a tag
		// or a GOOS suffix appears as a row nobody wrote rather than as
		// silence.
		ok, err := build.Default.MatchFile(dir, name)
		if err != nil {
			t.Fatalf("match %s: %v", name, err)
		}
		prefix := name
		if ok {
			files++
		} else {
			prefix = name + " [build-excluded]"
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		// EVERY REFERENCE COUNTS, not every CallExpr, and the whole file is
		// walked rather than its FuncDecls -- a call in a package-level `var`,
		// a call through a function value and `(suppliedTextExtent)(x)` are all
		// invisible to the narrow form, and all three mention the name.
		// `where` is read by the walker below on every increment, so it is
		// declared beside it and reassigned per declaration rather than
		// rebuilding the closure for each one.
		var where string
		var count func(ast.Node) bool
		count = func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				// A SELECTED name is a different symbol -- a field or a method
				// called suppliedTextExtent is not this helper. The qualifier
				// is still walked, so a call nested inside one is not lost
				// with it.
				ast.Inspect(x.X, count)
				return false
			case *ast.Field:
				// The NAMES of a struct field, a parameter or a receiver are
				// DECLARATIONS, not references. Only the type can mention the
				// helper. The synthetic fixture in
				// TestTheCensusWalkSeesWhatItClaimsTo found this the moment it
				// was written: a `suppliedTextExtent string` field counted as
				// a call site.
				ast.Inspect(x.Type, count)
				return false
			case *ast.Ident:
				if extentNames[x.Name] {
					got[where]++
				}
			}
			return true
		}
		for _, d := range f.Decls {
			fd, isFunc := d.(*ast.FuncDecl)
			where = prefix + "/<package-level>"
			if isFunc {
				// THE RECEIVER IS PART OF THE KEY, or a method and a function
				// of the same name in one file merge into one row and the
				// breakdown stops being a breakdown.
				where = prefix + "/" + fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) > 0 {
					where = prefix + "/(" + types.ExprString(fd.Recv.List[0].Type) + ")." + fd.Name.Name
				}
				if extentNames[fd.Name.Name] {
					// A DECLARATION OF THAT NAME is not a reference to the
					// helper -- neither the helper's own, nor a METHOD's,
					// which is a different symbol by the same rule the
					// selector skip below states. The BODY is neither a
					// declaration nor a selected name, so it is walked like
					// any other: a reference inside it is a call site, and the
					// fixture in TestTheCensusWalkSeesWhatItClaimsTo carries
					// one, because a mechanism nothing exercises is a
					// mechanism that can be deleted with the suite green.
					if fd.Body != nil {
						ast.Inspect(fd.Body, count)
					}
					continue
				}
			}
			ast.Inspect(d, count)
		}
	}
	return got, files
}

// TestTheCensusWalkSeesWhatItClaimsTo holds censusOfDir's coverage mechanisms
// to synthetic sources, because this package's own files cannot: every real
// call site is a plain direct call in an ordinary func in a file no build tag
// excludes, so WITHOUT THIS TEST all but two of them could be reverted with
// the whole suite green. THE PRECONDITION IS LOAD-BEARING: with this test
// present, reverting any of the eleven fails, which is the entire reason it is
// here. The two a real scan does see are named at censusOfDir: the
// declaration skip, and the identifier match, whose DELETION empties the
// census even though NARROWING it to a bare CallExpr does not. Each mechanism
// appears below as a row it produces or as a row it keeps OUT, and the fixture
// is one directory so a single expected map pins them all at once.
func TestTheCensusWalkSeesWhatItClaimsTo(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// Excluded by a build tag: keyed apart, and not counted as a production file.
	write("excluded.go", "//go:build ignore\n\npackage p\n\nvar x = suppliedTextExtent(\"a\")\n")
	// A _test.go file is not production and is skipped before the tag check.
	write("skipped_test.go", "package p\n\nvar y = suppliedTextExtent(\"a\")\n")
	write("a.go", `package p

var pkgLevel = suppliedTextExtent("a")
var alias = suppliedTextExtent

type T struct{ suppliedTextExtent string }

func (t T) selected() string { return t.suppliedTextExtent }
func qualified() string      { return wrap(suppliedTextExtent("f")).s }
func inFieldType(p [suppliedTextExtent]int) {}
func (t T) suppliedTextExtent() string { return suppliedTextExtent("e") }
func (t T) n() string        { return suppliedTextExtent("b") }
func n() string              { return suppliedTextExtent("c") }
func parenthesised() string  { return (suppliedTextExtent)("d") }
func suppliedTextExtent(s string) string { return suppliedTextExtent(s) }
`)
	got, files := censusOfDir(t, dir)
	want := map[string]int{
		"a.go/<package-level>": 2, // the var initializer AND the function value
		"a.go/(T).n":           1, // the receiver keeps this apart from a.go/n
		"a.go/n":               1,
		"a.go/parenthesised":   1, // not a bare-identifier callee
		// THE TWO SKIPS DO NOT SWALLOW WHAT THEY STEP OVER. A selector's
		// QUALIFIER and a field's TYPE are still walked, so a reference nested
		// in either is a call site like any other. Without these rows both
		// recursions can be deleted with the whole suite green, and a census
		// that UNDERCOUNTS is the one failure it exists to rule out.
		"a.go/qualified":   1,
		"a.go/inFieldType": 1,
		// Walked, but under a key of its own, so a build-excluded site can
		// neither pass for a production one nor disappear.
		"excluded.go [build-excluded]/<package-level>": 1,
		// A DECLARATION of that name is not a reference, but its BODY is
		// walked like any other, so what each of these rows counts is the ONE
		// call inside the body and never the name above it. Without them the
		// fixture never exercises that walk and the line doing it can be
		// deleted with the whole suite green -- and the method row is what
		// tells a name skipped BECAUSE IT IS A DECLARATION from one skipped
		// only because it had no receiver.
		"a.go/suppliedTextExtent":     1,
		"a.go/(T).suppliedTextExtent": 1,
	}
	// a.go/(T).selected is absent: a FIELD of that name is a different symbol.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the walk does not see what it claims to.\n got: %v\nwant: %v", got, want)
	}
	if files != 1 {
		t.Errorf("counted %d production files; the build-excluded and _test.go ones must not be among them", files)
	}
}

// TestTheSuppliedTextExtentCensusMatchesAScan holds the census above to an AST
// walk of this package's own production files.
//
// It asserts the BREAKDOWN, not just the total, because a right total can hide
// two offsetting errors in the breakdown.
func TestTheSuppliedTextExtentCensusMatchesAScan(t *testing.T) {
	got, files := censusOfDir(t, ".")
	if files == 0 {
		t.Fatal("no production files were parsed; this census would pass vacuously")
	}
	if !reflect.DeepEqual(got, suppliedTextExtentCensus) {
		t.Errorf("the census does not match the source.\n got: %v\nwant: %v", got, suppliedTextExtentCensus)
	}
	// THE TOTAL IS LOGGED, NOT ASSERTED: a literal would be another
	// hand-maintained copy of a number the DeepEqual above already pins.
	total := 0
	for _, n := range suppliedTextExtentCensus {
		total += n
	}
	t.Logf("%d production call sites of suppliedTextExtent, scanned across %d production files", total, files)
}

// TestEveryWitnessBearingTypeCarriesAFramedWidth is the mechanical answer to
// the way the framed-width preflight was rolled out: by enumeration.
//
// THE ENUMERATION WAS WRONG AND SAID SO IN PROSE. doc.go named five sibling
// seams and asserted four of them took a caller-sized value; the fifth,
// VerifiedP3bRuleset, took one too, and a Q3 lane measured its identity at
// 1.007x the caller's edit on every use. Two earlier rounds had closed the
// seams a reviewer named and left the neighbour open. A sentence cannot stop
// that recurring; a census can.
//
// THE RULE. A type that carries an unexported `witness string` answers a
// question by re-framing its own fields, so it must also carry `framedLen int`
// and compare it first -- a rejection test that refuses a width-changing edit
// before any byte is materialized. This walks the production files and fails on
// any witness-bearing struct without a width, naming it. A NEW witness type is
// therefore a compile-and-fail, not a prose omission.
//
// IT DOES NOT CHECK THAT THE WIDTH IS USED WELL. That is
// TestTheRecordedFramedWidthIsTheWitnessOwn's job, and the two are deliberately
// separate: this one answers "is there one", that one answers "is it right".
func TestEveryWitnessBearingTypeCarriesAFramedWidth(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	type site struct{ name, pos string }
	var withWitness, missing []site
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			var witness, width bool
			for _, fld := range st.Fields.List {
				for _, id := range fld.Names {
					switch id.Name {
					case "witness":
						if x, ok := fld.Type.(*ast.Ident); ok && x.Name == "string" {
							witness = true
						}
					case "framedLen":
						if x, ok := fld.Type.(*ast.Ident); ok && x.Name == "int" {
							width = true
						}
					}
				}
			}
			if !witness {
				return true
			}
			s := site{ts.Name.Name, fset.Position(ts.Pos()).String()}
			withWitness = append(withWitness, s)
			if !width {
				missing = append(missing, s)
			}
			return true
		})
	}

	if len(missing) > 0 {
		for _, m := range missing {
			t.Errorf("%s (%s) carries a witness with no framedLen: it re-frames its own fields to answer a question, so a caller-sized edit is paid for before it is refused",
				m.name, m.pos)
		}
		t.Fatalf("%d of %d witness-bearing types carry no framed width", len(missing), len(withWitness))
	}

	// THE COUNT IS PINNED SO THE WALK CANNOT GO QUIETLY BLIND. A census that
	// stopped finding types would pass this test by finding nothing, which is
	// exactly the failure mode the prose enumeration had.
	const wantTypes = 7
	if len(withWitness) != wantTypes {
		names := make([]string, 0, len(withWitness))
		for _, s := range withWitness {
			names = append(names, s.name)
		}
		sort.Strings(names)
		t.Fatalf("the census found %d witness-bearing types, want %d: %v -- if a type was added or removed, update the count with its width, not the count alone",
			len(withWitness), wantTypes, names)
	}
}

// TestTheWitnessCensusWouldCatchAWidthlessType is that census's own control: a
// synthetic witness-bearing struct with no width must be reported. Without it
// the census above could pass by never recognising a witness field at all.
func TestTheWitnessCensusWouldCatchAWidthlessType(t *testing.T) {
	const src = `package p
type withWidth struct { A string; witness string; framedLen int }
type withoutWidth struct { B string; witness string }
type unrelated struct { C string }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse synthetic: %v", err)
	}
	var seen, missing []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		var witness, width bool
		for _, fld := range st.Fields.List {
			for _, id := range fld.Names {
				switch id.Name {
				case "witness":
					if x, ok := fld.Type.(*ast.Ident); ok && x.Name == "string" {
						witness = true
					}
				case "framedLen":
					if x, ok := fld.Type.(*ast.Ident); ok && x.Name == "int" {
						width = true
					}
				}
			}
		}
		if witness {
			seen = append(seen, ts.Name.Name)
			if !width {
				missing = append(missing, ts.Name.Name)
			}
		}
		return true
	})
	if want := []string{"withWidth", "withoutWidth"}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("the walk saw %v, want %v", seen, want)
	}
	if want := []string{"withoutWidth"}; !reflect.DeepEqual(missing, want) {
		t.Fatalf("the walk reported %v as widthless, want %v", missing, want)
	}
}
