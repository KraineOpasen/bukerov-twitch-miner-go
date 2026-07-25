package boundary

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// BKM-021 environment-read boundary enforcement.
//
// After BKM-021 the process resolves its environment exactly once, in the
// centralized startup resolver, and a small set of subsystem-local secret reads
// are deliberately deferred. This test walks every PRODUCTION Go file in the
// repository (skipping _test.go) and, using the Go AST — not a text grep, which
// would false-match comments and string literals — flags any os.Getenv /
// os.LookupEnv read that is not in the explicitly documented allowlist.
//
// A new, unapproved env read in cmd/, internal/app, internal/web,
// internal/miner, internal/watcher, internal/pubsub, internal/chat,
// internal/drops, internal/analytics, or any other runtime package fails the
// build here.

// envRead is one detected os.Getenv / os.LookupEnv reference.
type envRead struct {
	file string // module-relative, forward-slashed
	line int
	sel  string // e.g. "os.Getenv", "stdos.LookupEnv"
}

// allowedEnvReadFiles is the minimal, documented boundary. Keyed by exact
// module-relative path (not whole packages), each entry says WHY that file is
// permitted to read the process environment directly. Every entry is verified
// to still contain a real env read (see the stale-entry check), so the
// allowlist cannot rot into a blanket exemption.
func allowedEnvReadFiles() map[string]string {
	return map[string]string{
		"internal/runtimeconfig/runtimeconfig.go": "centralized startup env boundary: OSLookup wraps os.LookupEnv — THE sanctioned read point",
		"internal/config/config.go":               "config-file loader boundary: DISCORD_BOT_TOKEN overlay coupled to LoadConfig/SaveConfig",
		"internal/auth/crypto.go":                 "auth token-at-rest: TWITCH_AUTH_ENCRYPTION_KEY, a global secret read only in the encryption path",
		"internal/notifications/message.go":       "per-account push-provider secret resolution (documented deferral: binds to the runtime-reconciled username)",
	}
}

// isEnvReadFn reports whether a function name is one of the two process-env
// readers the boundary forbids outside the allowlist.
func isEnvReadFn(name string) bool {
	return name == "Getenv" || name == "LookupEnv"
}

// scanFileForEnvReads returns every os.Getenv / os.LookupEnv reference in the
// parsed file. It resolves the local name(s) bound to the standard "os" import
// (default name or alias) so a call is matched only when its selector base
// actually refers to std os — a lookalike package's Getenv is never flagged. A
// dot-import of os (which would let Getenv be called bare, evading a selector
// check) is itself reported as a boundary violation.
func scanFileForEnvReads(fset *token.FileSet, f *ast.File, relPath string) []envRead {
	aliases := map[string]bool{} // local names referring to std "os"
	dotImported := false
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != "os" {
			continue
		}
		switch {
		case imp.Name == nil:
			aliases["os"] = true
		case imp.Name.Name == "_":
			// blank import: no calls possible
		case imp.Name.Name == ".":
			dotImported = true
		default:
			aliases[imp.Name.Name] = true
		}
	}
	if len(aliases) == 0 && !dotImported {
		return nil
	}

	var reads []envRead
	if dotImported {
		reads = append(reads, mkRead(fset, relPath, f.Pos(),
			"os (dot-import — env reads would evade selector enforcement)"))
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		base, ok := sel.X.(*ast.Ident)
		if !ok || !aliases[base.Name] {
			return true
		}
		if isEnvReadFn(sel.Sel.Name) {
			reads = append(reads, mkRead(fset, relPath, sel.Pos(), base.Name+"."+sel.Sel.Name))
		}
		return true
	})
	return reads
}

func mkRead(fset *token.FileSet, relPath string, pos token.Pos, sel string) envRead {
	return envRead{file: relPath, line: fset.Position(pos).Line, sel: sel}
}

// TestNoUnapprovedEnvReads is the enforcement: the whole production tree is
// walked and any env read outside the allowlist fails with an exact file:line.
func TestNoUnapprovedEnvReads(t *testing.T) {
	root := moduleRoot(t)
	allow := allowedEnvReadFiles()
	seenAllowed := map[string]bool{}
	var violations []envRead

	walkErr := filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if de.IsDir() {
			switch de.Name() {
			case ".git", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(mustRel(t, root, path))

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Errorf("parse %s: %v", rel, err)
			return nil
		}
		reads := scanFileForEnvReads(fset, file, rel)
		if len(reads) == 0 {
			return nil
		}
		if reason, ok := allow[rel]; ok {
			seenAllowed[rel] = true
			for _, r := range reads {
				t.Logf("allowed env read %s:%d (%s) — %s", r.file, r.line, r.sel, reason)
			}
			return nil
		}
		violations = append(violations, reads...)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}

	for _, v := range violations {
		t.Errorf("unapproved env read: %s:%d calls %s — production runtime code must read the environment only through internal/runtimeconfig; add a documented allowlist entry ONLY if this is a genuine new config boundary", v.file, v.line, v.sel)
	}

	// Allowlist hygiene: an entry that no longer has a real env read is stale and
	// must be removed, so the allowlist can never silently over-permit.
	for f := range allow {
		if !seenAllowed[f] {
			t.Errorf("stale allowlist entry %q: no os.Getenv/os.LookupEnv found there anymore — remove it", f)
		}
	}
}

// --- Negative proof: the detector actually detects (and is precise). ---

func scanSrc(t *testing.T, src string) []envRead {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return scanFileForEnvReads(fset, file, "fixture.go")
}

// TestDetectorFlagsDirectCall proves a real os.Getenv call is detected — the
// negative proof that the enforcement above is not a no-op.
func TestDetectorFlagsDirectCall(t *testing.T) {
	reads := scanSrc(t, `package p
import "os"
func f() string { return os.Getenv("X") }
`)
	if len(reads) != 1 || reads[0].sel != "os.Getenv" || reads[0].line != 3 {
		t.Fatalf("must flag os.Getenv at line 3, got %+v", reads)
	}
}

func TestDetectorFlagsLookupEnv(t *testing.T) {
	reads := scanSrc(t, `package p
import "os"
func f() (string, bool) { return os.LookupEnv("X") }
`)
	if len(reads) != 1 || reads[0].sel != "os.LookupEnv" {
		t.Fatalf("must flag os.LookupEnv, got %+v", reads)
	}
}

// TestDetectorFollowsAlias proves an aliased os import is still resolved.
func TestDetectorFollowsAlias(t *testing.T) {
	reads := scanSrc(t, `package p
import stdos "os"
func f() string { return stdos.Getenv("X") }
`)
	if len(reads) != 1 || reads[0].sel != "stdos.Getenv" {
		t.Fatalf("must follow the aliased os import, got %+v", reads)
	}
}

// TestDetectorIgnoresCommentsStringsAndOtherOsCalls proves the AST approach does
// not false-match text (a grep would) and does not flag other os functions.
func TestDetectorIgnoresCommentsStringsAndOtherOsCalls(t *testing.T) {
	reads := scanSrc(t, `package p
import "os"
// os.Getenv("X") in a comment must be ignored
var s = "os.LookupEnv is fine inside a string literal"
func f() []string { return os.Environ() } // Environ is neither Getenv nor LookupEnv
`)
	if len(reads) != 0 {
		t.Fatalf("must ignore comments/strings/os.Environ, got %+v", reads)
	}
}

// TestDetectorIgnoresLookalikePackage proves a different package's Getenv is not
// flagged — matching is by resolved import path, not by the selector text.
func TestDetectorIgnoresLookalikePackage(t *testing.T) {
	reads := scanSrc(t, `package p
import "example.com/notos"
func f() string { return notos.Getenv("X") }
`)
	if len(reads) != 0 {
		t.Fatalf("must NOT flag a non-os package's Getenv, got %+v", reads)
	}
}

// TestDetectorFlagsDotImport proves the one selector-evading form — a dot-import
// of os — is still caught.
func TestDetectorFlagsDotImport(t *testing.T) {
	reads := scanSrc(t, `package p
import . "os"
func f() string { return Getenv("X") }
`)
	if len(reads) == 0 {
		t.Fatalf("must flag a dot-import of os")
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from the test directory")
		}
		dir = parent
	}
}

func mustRel(t *testing.T, base, target string) string {
	t.Helper()
	rel, err := filepath.Rel(base, target)
	if err != nil {
		t.Fatalf("rel(%q,%q): %v", base, target, err)
	}
	return rel
}
