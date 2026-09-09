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
// reaches os, syscall, time and reflect through the FIPS-140 integrity check
// and the entropy source. Those are unavoidable beneath ANY cryptographic hash
// in the standard library, they are not reachable from any code path this
// package executes, and no amount of fence design removes them. Rule C
// therefore denies the packages that indicate real capability and would NOT be
// dragged in by hashing — which is exactly the set that matters. The residue is
// asserted explicitly below so it can never grow silently.
//
// Test files are excluded on purpose: the tests import internal/models to
// compare against the real policy, which is the whole point of the parity
// suite and is not part of the shipped import graph.

import (
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

// allowedDirectImports is the complete set the production core may import.
//
// Every entry is a pure computation package. Adding to this list is a decision
// about what "pure" means for this package, so it is deliberately a diff a
// reviewer cannot miss.
var allowedDirectImports = map[string]string{
	"crypto/sha256":   "the common-input digest; collision resistance is the point",
	"encoding/binary": "length prefixes in the digest",
	"errors":          "typed sentinel errors",
	"hash":            "the hash.Hash interface used by the digest helpers",
	"math":            "Abs/Round/IsNaN, mirroring the pinned policy's own arithmetic",
	"sort":            "deterministic attempt ordering, independent of map iteration",
	"strconv":         "rendering comparison values without fmt",
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

// knownStdlibResidue is what crypto/sha256 legitimately drags in. Asserting the
// set EXACTLY means a future stdlib change that widened it would fail here
// instead of quietly enlarging the core's reach.
var knownStdlibResidue = []string{"os", "reflect", "syscall", "time"}

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

	// ---- The residue, asserted exactly. --------------------------------
	var residue []string
	for _, p := range knownStdlibResidue {
		if closure[p] {
			residue = append(residue, p)
		}
	}
	sort.Strings(residue)
	if strings.Join(residue, ",") != strings.Join(knownStdlibResidue, ",") {
		t.Errorf("the documented stdlib residue changed: reachable = %v, documented = %v.\n"+
			"This is not necessarily a defect, but the fence's honesty depends on the "+
			"documented set matching reality, so update the comment and this list together.",
			residue, knownStdlibResidue)
	}

	t.Logf("core direct imports (%d): %v", len(direct), direct)
	t.Logf("transitive stdlib closure: %d packages; documented residue %v is reachable only "+
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
