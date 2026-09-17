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
	"os"
	"path/filepath"
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
