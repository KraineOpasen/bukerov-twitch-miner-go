package predictioneval_test

// THE DEPENDENCY FENCE.
//
// The replay core claims to be pure: no database, no network, no Twitch, no
// PubSub, no live settings, no environment, no wall clock, no global RNG. A
// claim like that decays the moment someone adds a convenient import, and a
// code review will not reliably catch it — the dangerous import is usually one
// line in a file that is mostly about something else.
//
// So the claim is enforced mechanically, over the WHOLE production core, by
// three rules:
//
//	Rule A — no first-party package. The core imports nothing from this module
//	         but itself. This is the rule that actually keeps the database, the
//	         pool and the settings service out, because every one of them lives
//	         behind a first-party import.
//	Rule B — direct imports come from an exact allowlist. This is what keeps
//	         database/sql, net/http, os, time and math/rand out of the core's
//	         own source, and it is checked by parsing the files themselves.
//	Rule C — the transitive stdlib closure contains no capability package.
//
// HONEST RESIDUE, stated rather than hidden: crypto/sha256 transitively
// reaches os, syscall, time, io/fs and internal/poll through the FIPS-140
// integrity check and the entropy source. Those are unavoidable beneath ANY
// cryptographic hash in the standard library, they are not reachable from any
// code path this package executes, and no amount of fence design removes them.
//
// That list used to say "and reflect", which was WRONG, and wrong in the one
// direction that matters: reflect was not unavoidable at all. It came from
// encoding/binary, which this package used for eight bytes of length prefix and
// no longer does. An earlier version of this file also asserted the residue in
// a way that could only detect SHRINKAGE — the "exact" set was built by
// filtering the closure through the documented list, so a package nobody had
// listed could never appear in it. Both are fixed below: the residue is
// computed by intersecting the closure with a WATCHLIST that is deliberately
// wider than the documented set, so a newly reachable sensitive package fails
// this test instead of passing it unnoticed.
//
// Test files are excluded on purpose: the tests import internal/models to
// compare against the real policy, which is the whole point of the parity
// suite and is not part of the shipped import graph.

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/KraineOpasen/bukerov-twitch-miner-go"

// allowedDirectImports is the complete set the production core may import.
//
// Every entry is a pure computation package. Adding to this list is a decision
// about what "pure" means for this package, so it is deliberately a diff a
// reviewer cannot miss.
var allowedDirectImports = map[string]string{
	"crypto/sha256": "the common-input digest; collision resistance is the point",
	"errors":        "typed sentinel errors",
	"hash":          "the hash.Hash interface used by the digest helpers",
	"math":          "Abs/Round/IsNaN, mirroring the pinned policy's own arithmetic",
	"sort":          "deterministic attempt ordering, independent of map iteration",
	"strconv":       "rendering comparison values without fmt",
}

// deniedTransitiveImports are capability packages. None of them is reachable
// from a pure computation package, so any appearance means the core grew a
// capability it must not have.
var deniedTransitiveImports = []string{
	"database/sql", "database/sql/driver",
	"net", "net/http", "net/url",
	"os/exec", "os/signal",
	"math/rand", "math/rand/v2",
	"context",
	"log", "log/slog",
	"io/ioutil",
	"testing",
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics",
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub",
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database",
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch",
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings",
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/miner",
	"modernc.org/sqlite",
}

// knownStdlibResidue is what crypto/sha256 legitimately drags in, sorted.
//
// Every entry here is a package this core can reach but never calls, and each
// one is beneath the FIPS-140 integrity check or the entropy source. Verified
// per-root rather than assumed: crypto/sha256 reaches all five, and none of the
// other six allowed imports reaches any of them.
var knownStdlibResidue = []string{"internal/poll", "io/fs", "os", "syscall", "time"}

// sensitiveWatchlist is deliberately WIDER than knownStdlibResidue. It names
// the packages whose appearance in the closure would mean the core had gained a
// capability, whether or not anyone predicted it.
//
// The residue check intersects the closure with THIS list and requires the
// result to equal knownStdlibResidue exactly. That is what makes the check
// bidirectional: a package that shows up here and is not documented fails the
// test, which is precisely what the previous filter-through-the-documented-list
// formulation could not do.
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

func TestProductionCoreDependencyFence(t *testing.T) {
	direct := directImportsOfProductionFiles(t, "../predictioneval")

	// ---- Rule B: the allowlist. ----------------------------------------
	for _, imp := range direct {
		if _, ok := allowedDirectImports[imp]; !ok {
			t.Errorf("the production core imports %q, which is not on the purity allowlist.\n"+
				"If this import is genuinely pure, add it to allowedDirectImports with a reason. "+
				"If it is not, the core has grown a capability it must not have.", imp)
		}
	}

	// ---- Rule A: no first-party package. -------------------------------
	// This is what actually keeps SQLite, the pool and live settings out: they
	// are all reachable only through a first-party import.
	for _, imp := range direct {
		if strings.HasPrefix(imp, modulePath) {
			t.Errorf("the production core imports the first-party package %q; the replay core "+
				"must depend on no other package in this module", imp)
		}
	}

	// ---- Rule C: the transitive closure. -------------------------------
	closure := transitiveStdlibClosure(t, direct)
	denied := map[string]bool{}
	for _, d := range deniedTransitiveImports {
		denied[d] = true
	}
	for pkg := range closure {
		if denied[pkg] {
			t.Errorf("the production core transitively reaches %q", pkg)
		}
	}

	// ---- The residue, asserted exactly and in BOTH directions. ---------
	// Built from the WATCHLIST, not from the documented set, so a sensitive
	// package nobody predicted still lands here.
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
		t.Errorf("the reachable sensitive set changed.\n reachable  = %v\n documented = %v\n"+
			"The fence's honesty rests on these matching. If something was ADDED, the core "+
			"just gained reach it has to justify; if something was REMOVED, tighten the "+
			"documented set rather than leaving it overstated.",
			residue, want)
	}

	t.Logf("core direct imports (%d): %v", len(direct), direct)
	t.Logf("transitive stdlib closure: %d packages; reachable sensitive residue %v, all of it "+
		"beneath crypto/sha256", len(closure), residue)
}

// TestTheFenceWouldActuallyCatchAForbiddenImport proves the fence is not
// vacuous.
//
// A fence that passes because its checks never fire is worse than no fence, so
// this drives the same predicates over a deliberately forbidden import and
// requires them to reject it.
func TestTheFenceWouldActuallyCatchAForbiddenImport(t *testing.T) {
	for _, forbidden := range []string{
		"database/sql",
		"net/http",
		"math/rand",
		"time",
		modulePath + "/internal/analytics",
	} {
		if _, ok := allowedDirectImports[forbidden]; ok {
			t.Fatalf("%q is on the allowlist; the fence would not reject it", forbidden)
		}
	}
	// And the transitive walk must actually find a denied package when one is
	// genuinely reachable: database/sql reaches context.
	closure := transitiveStdlibClosure(t, []string{"database/sql"})
	if !closure["context"] {
		t.Fatal("the transitive walk did not reach context from database/sql, so Rule C is not " +
			"actually inspecting the import graph")
	}
}

// TestTheDocumentedResidueIsActuallyAttributableToTheHash checks the CLAIM the
// residue paragraph makes, not just the set it names.
//
// The paragraph's whole argument is "these are unavoidable beneath any stdlib
// cryptographic hash". That is a statement about WHERE they come from, and it
// was previously wrong about reflect — which came from encoding/binary, was
// entirely avoidable, and is now gone. So the attribution is verified per-root:
// crypto/sha256 must reach every documented residue entry, and none of the
// other allowed imports may reach any of them.
func TestTheDocumentedResidueIsActuallyAttributableToTheHash(t *testing.T) {
	hashClosure := transitiveStdlibClosure(t, []string{"crypto/sha256"})
	for _, p := range knownStdlibResidue {
		if !hashClosure[p] {
			t.Errorf("%q is documented as residue of crypto/sha256, but the hash does not reach it; "+
				"something else in the core is pulling it in and the justification does not apply", p)
		}
	}
	for imp := range allowedDirectImports {
		if imp == "crypto/sha256" {
			continue
		}
		closure := transitiveStdlibClosure(t, []string{imp})
		for _, p := range knownStdlibResidue {
			if closure[p] {
				t.Errorf("%q reaches %q, so that residue entry is NOT attributable to the hash alone. "+
					"Either drop %q from the core (reflect was dropped for exactly this reason) or "+
					"correct the residue paragraph.", imp, p, imp)
			}
		}
	}
}

// directImportsOfProductionFiles parses the package's NON-TEST .go files.
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

// transitiveStdlibClosure walks the standard library import graph with go/build.
//
// It deliberately does not shell out to `go list`: a fence that needs a
// subprocess would have to skip when the toolchain is unavailable, and a
// skipped fence proves nothing.
func transitiveStdlibClosure(t *testing.T, roots []string) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	var walk func(string)
	walk = func(p string) {
		if seen[p] || p == "C" || p == "unsafe" {
			return
		}
		seen[p] = true
		pkg, err := build.Default.Import(p, "", 0)
		if err != nil {
			// A path go/build cannot resolve without module context is not a
			// stdlib package; Rules A and B already cover those.
			return
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

// TestEveryTestNamedInACommentActuallyExists closes a drift this package kept
// producing.
//
// The comments here routinely say "this invariant is held by TestX" — that
// citation is the reader's only evidence that a claim is mechanically enforced
// rather than merely asserted. Four such citations pointed at tests that did
// not exist under those names, and one of them named a test whose real scope
// was narrower than the citation implied. Nothing catches that: the compiler
// does not read comments, and a reviewer following the citation finds nothing
// and reasonably concludes the invariant is unenforced.
//
// So the citations are checked. Any Test-shaped identifier mentioned in a
// non-test file of this package or its reader must be a test that exists
// somewhere in internal/.
func TestEveryTestNamedInACommentActuallyExists(t *testing.T) {
	cited := map[string][]string{}
	for _, dir := range []string{"../predictioneval", "../predictioneval/reader"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			for _, ident := range testIdentifierPattern.FindAllString(string(body), -1) {
				cited[ident] = append(cited[ident], path)
			}
		}
	}
	if len(cited) == 0 {
		t.Fatal("no test citations were found at all; this check would pass vacuously")
	}

	defined := definedTestNames(t, "..")
	for name, where := range cited {
		if !defined[name] {
			t.Errorf("%s is cited in %v but no such test exists. A citation is the only evidence "+
				"a reader has that an invariant is mechanically enforced; a dangling one is worse "+
				"than no comment, because it reads as proof.", name, where)
		}
	}
	t.Logf("%d cited test names, all defined", len(cited))
}

// testIdentifierPattern matches a Go test function name mentioned in prose.
var testIdentifierPattern = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]+`)

// definedTestNames collects every top-level Test function under root.
func definedTestNames(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			// A file this walk cannot parse is not evidence of a missing test.
			return nil
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				out[fn.Name.Name] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("no test functions found under %s; the check would pass vacuously", root)
	}
	return out
}
