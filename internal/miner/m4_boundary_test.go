package miner

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allowedNotificationsAccessors are the only functions in this package
// permitted to reference the Miner.notifications field directly by a raw
// `.notifications` selector: notificationManager() (the single accessor
// every other reader must go through) and initNotificationManager (the
// single write-once publisher). See miner.go's doc comments on both for the
// invariants this enforces (I1/I2 in the M4 design).
//
// Name alone is not enough to allowlist a function (see isMinerMethod):
// a method with the SAME name declared on a DIFFERENT receiver type (e.g. a
// hypothetical `func (n minerHealthNotifier) notificationManager() ...`)
// must NOT be silently allowlisted just because its name matches — only the
// two *Miner methods are exempt.
var allowedNotificationsAccessors = map[string]bool{
	"initNotificationManager": true,
	"notificationManager":     true,
}

// isMinerMethod reports whether fn is a method with exactly one receiver of
// type *Miner (the shape `func (m *Miner) name(...) ...`). A free function,
// a method on any other receiver type, or a value (non-pointer) receiver all
// return false. This is what makes the name-based allowedNotificationsAccessors
// lookup safe: allowlisting is name AND receiver-type, not name alone.
func isMinerMethod(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	return ok && ident.Name == "Miner"
}

// notificationsAssignSite records one source location where an AssignStmt's
// LHS is a `.notifications` selector — i.e. a WRITE to the field, as opposed
// to any other read-shaped access.
type notificationsAssignSite struct {
	file string
	line int
	fn   string
}

// TestNotificationsFieldAccessBoundary is an AST-based boundary test (M4
// design dossier §6.3): it parses every non-test .go file in this package
// and fails if a `.notifications` selector expression appears anywhere
// outside the struct field declaration and the two *Miner methods allowed to
// touch it directly. This is what makes "single write, single accessor" a
// build-test-checkable invariant instead of a convention nobody enforces —
// a raw `m.notifications` (or `n.m.notifications`, or any other nested
// selector ending in `.notifications`) creeping back into a THIRD function —
// or into a same-named method on the WRONG receiver type — fails this test
// with the offending file:line.
//
// It additionally asserts I1's "single write" half directly: across every
// non-test file in the package there must be EXACTLY ONE assignment whose
// LHS is a `.notifications` selector, and it must live inside
// initNotificationManager. This is what catches a mutant that adds a SECOND,
// earlier assignment while leaving the original (correctly-ordered) one in
// place — a pure ordering check on one located assignment cannot see an
// extra one appear beside it; a COUNT check can.
//
// Known limitation (documented, not fixed): this is a syntactic check, not a
// type-checked one. It flags any selector literally spelled `.notifications`
// regardless of the receiver's static type (except for the allowlist's own
// isMinerMethod check, which IS receiver-type-aware), and it cannot see
// through an alias captured into a closure (e.g. a local variable that
// itself aliases m.notifications via some indirection the accessor didn't
// produce). Neither gap is exploitable in this package today: nothing else
// exposes a field named `notifications`, and every current reader goes
// through the accessor directly rather than through a hand-rolled alias.
func TestNotificationsFieldAccessBoundary(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	var assignSites []notificationsAssignSite
	violations := 0

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				// Non-func decls (imports, the Miner struct's own field
				// declaration, consts/vars) can never contain a SelectorExpr
				// read of a struct field — only a func body can.
				continue
			}
			funcName := fn.Name.Name
			allowed := allowedNotificationsAccessors[funcName] && isMinerMethod(fn)

			// Collect every `.notifications` assignment site UNCONDITIONALLY
			// (even inside an allowlisted function): the "raw read/write"
			// violation check below is skipped for allowlisted functions,
			// but the "exactly one write, and it's in initNotificationManager"
			// invariant must still see it. A write inside a NON-allowlisted
			// function is caught twice — once here (recorded as an
			// out-of-place assignment site) and once by the raw-access scan
			// below (which still runs for non-allowlisted functions) — the
			// redundancy is intentional: either check alone is sufficient to
			// fail the test, and each pins a different, independently
			// useful invariant.
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range assign.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "notifications" {
						continue
					}
					pos := fset.Position(sel.Pos())
					assignSites = append(assignSites, notificationsAssignSite{
						file: filepath.Base(pos.Filename),
						line: pos.Line,
						fn:   funcName,
					})
				}
				return true
			})

			if allowed {
				continue
			}

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "notifications" {
					return true
				}
				pos := fset.Position(sel.Pos())
				violations++
				t.Errorf("%s:%d: raw `.notifications` field access outside notificationManager()/initNotificationManager() on *Miner (found in func %s) — use the notificationManager() accessor instead",
					filepath.Base(pos.Filename), pos.Line, funcName)
				return true
			})
		}
	}

	if len(assignSites) != 1 {
		t.Errorf("expected exactly 1 assignment to .notifications across the package, got %d: %v", len(assignSites), assignSites)
	} else if assignSites[0].fn != "initNotificationManager" {
		t.Errorf("the single .notifications assignment must be inside initNotificationManager, found in %s at %s:%d",
			assignSites[0].fn, assignSites[0].file, assignSites[0].line)
	}

	if violations == 0 && testing.Verbose() {
		t.Log("boundary test passed: no raw .notifications access found outside the *Miner accessor/publisher")
	}
}
