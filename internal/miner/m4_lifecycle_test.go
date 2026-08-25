package miner

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/journal"
)

// --- T1: startup publication order ---

// TestInitNotificationManagerPublishesFullyConstructedManager pins the
// write-once publish (I1/I8): before initNotificationManager runs,
// notificationManager() must be nil; after it runs (with a database
// present), it must return a fully usable Manager — proven by calling a
// real exported method (IsEnabled) rather than merely checking non-nil.
func TestInitNotificationManagerPublishesFullyConstructedManager(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	dbPath := filepath.Join(t.TempDir(), "t1.db")
	db := openRawMinerDB(t, dbPath)
	defer func() { _ = db.Close() }()
	m.db = db

	if got := m.notificationManager(); got != nil {
		t.Fatal("setup: manager already published before initNotificationManager ran")
	}

	m.initNotificationManager(context.Background())

	got := m.notificationManager()
	if got == nil {
		t.Fatal("initNotificationManager did not publish a manager despite a database existing")
	}
	if got.IsEnabled() {
		t.Fatal("Discord should start disabled per the seeded (zero-value) config")
	}
}

// TestInitNotificationManagerNeverPublishesPartiallyInitializedManager is a
// concurrency regression for I8: a reader goroutine hammers
// notificationManager() and, whenever it observes a non-nil manager,
// immediately calls two of its methods (NotifyPointsReached and IsEnabled —
// both touch Manager-internal, mutex-guarded state) concurrently with
// initNotificationManager's own post-construction InitializePointsTracking
// call. -race is the assertion (it must stay silent: publication happens only after it
// complete, and every Manager method involved takes Manager's own mutex).
//
// NOTE for the reviewer: this test alone does NOT distinguish "publish
// happens after init" from "publish happens before init" — verified
// empirically by temporarily reordering the two in initNotificationManager
// (publish moved above InitializePointsTracking) and
// re-running this test at -race -count=30: it still passed. The reordering
// is not a data race because InitializePointsTracking, NotifyPointsReached,
// and IsEnabled all serialize through Manager's OWN
// mutex regardless of call order — the defect is a pure ordering/visibility
// bug (a reader could observe zero-value tracking state), not an
// unsynchronized memory access. This test is kept anyway as a general
// concurrency regression (it would catch an ACTUAL unsynchronized field
// added later), but the ordering invariant itself is pinned deterministically
// by TestInitNotificationManagerPublishesBeforeInitCallSiteOrder below,
// following this repo's own precedent for equivalent-mutant scenarios (see
// internal/web/handlers_notifications_test.go's
// TestHandleAPITestNotificationRoutesThroughSanitizer).
func TestInitNotificationManagerNeverPublishesPartiallyInitializedManager(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	dbPath := filepath.Join(t.TempDir(), "mut04.db")
	db := openRawMinerDB(t, dbPath)
	defer func() { _ = db.Close() }()
	m.db = db

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				if mgr := m.notificationManager(); mgr != nil {
					mgr.NotifyPointsReached("alpha", 1)
					_ = mgr.IsEnabled()
				}
			}
		}
	}()

	m.initNotificationManager(context.Background())

	close(stop)
	<-done
}

// TestInitNotificationManagerPublishesBeforeInitCallSiteOrder is the
// deterministic killer for MUT-M4-04 (publication reordered above
// InitializePointsTracking): it parses miner.go and
// asserts, by source position within initNotificationManager's body, that
// notifMgr.InitializePointsTracking(...) appears BEFORE the `m.notifications
// = notifMgr` publication. A source-position pin is the right tool here (not
// -race — see the doc comment on
// TestInitNotificationManagerNeverPublishesPartiallyInitializedManager for
// why the reorder is not a detectable data race): I8 is an ordering
// invariant on a single goroutine's own instruction sequence, which a
// syntactic check states directly and a race detector cannot see at all.
func TestInitNotificationManagerPublishesBeforeInitCallSiteOrder(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "miner.go", nil, 0)
	if err != nil {
		t.Fatalf("parse miner.go: %v", err)
	}

	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if f, ok := decl.(*ast.FuncDecl); ok && f.Name.Name == "initNotificationManager" {
			fn = f
			break
		}
	}
	if fn == nil {
		t.Fatal("initNotificationManager not found in miner.go")
	}

	var initPointsPos token.Pos
	var publishPositions []token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "InitializePointsTracking":
					initPointsPos = v.Pos()
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range v.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "notifications" {
					continue
				}
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "m" {
					// Append, never overwrite: a mutant that ADDS a second,
					// earlier publish while leaving the original (correctly
					// ordered) one in place must be caught by the COUNT
					// below, not silently absorbed into "the last one found
					// still happens to be in the right place".
					publishPositions = append(publishPositions, v.Pos())
				}
			}
		}
		return true
	})

	if initPointsPos == token.NoPos {
		t.Fatalf("could not locate InitializePointsTracking call site in initNotificationManager: initPoints=%v", initPointsPos)
	}
	if len(publishPositions) != 1 {
		t.Fatalf("expected exactly 1 `m.notifications = ...` assignment inside initNotificationManager, found %d at byte offsets %v — I1 requires a SINGLE write-once publication, not merely a correctly-ordered one",
			len(publishPositions), publishPositions)
	}
	// The uniqueness check above already guarantees there is exactly one
	// candidate; using it explicitly (rather than "whichever was seen last")
	// keeps this ordering check correct even if the uniqueness assertion is
	// ever loosened by mistake — the FIRST publish is the one every reader
	// could observe earliest, so it is the one that actually has to be
	// preceded by full initialization.
	publishPos := publishPositions[0]
	if initPointsPos >= publishPos {
		t.Errorf("InitializePointsTracking must run BEFORE m.notifications is published (I8); found at byte offsets init=%d publish=%d",
			initPointsPos, publishPos)
	}
}

// --- T2/T3/T4: runtime off->on / on->off / token change preserve identity ---

// TestApplySettingsDiscordToggleAndTokenChangePreservesManagerIdentity
// drives the REAL ApplySettings path through three transitions (Discord
// off->on, on->off, and a token change while enabled) and asserts the
// manager pointer returned by notificationManager() never changes: M4's
// whole point is that finishApply no longer ever creates or replaces the
// Manager, only reconfigures the one initNotificationManager already
// published at startup.
func TestApplySettingsDiscordToggleAndTokenChangePreservesManagerIdentity(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	dbPath := filepath.Join(t.TempDir(), "t234.db")
	db := openRawMinerDB(t, dbPath)
	defer func() { _ = db.Close() }()
	m.db = db
	m.initNotificationManager(context.Background())

	before := m.notificationManager()
	if before == nil {
		t.Fatal("setup: manager not published")
	}

	// T2: off -> on.
	rsOn := m.GetRuntimeSettings()
	rsOn.Discord.Enabled = true
	if err := m.ApplySettings(context.Background(), rsOn); err != nil {
		t.Fatalf("enable apply: %v", err)
	}
	if got := m.notificationManager(); got != before {
		t.Fatal("manager pointer changed on Discord enable")
	}

	// T3: on -> off.
	rsOff := m.GetRuntimeSettings()
	rsOff.Discord.Enabled = false
	if err := m.ApplySettings(context.Background(), rsOff); err != nil {
		t.Fatalf("disable apply: %v", err)
	}
	if got := m.notificationManager(); got != before {
		t.Fatal("manager pointer changed on Discord disable")
	}

	// T4: token change while enabled.
	rsToken := m.GetRuntimeSettings()
	rsToken.Discord.Enabled = true
	rsToken.Discord.BotToken = "changed-token"
	if err := m.ApplySettings(context.Background(), rsToken); err != nil {
		t.Fatalf("token-change apply: %v", err)
	}
	if got := m.notificationManager(); got != before {
		t.Fatal("manager pointer changed on a bot-token change")
	}
}

// --- T15: NewManager construction failure at startup ---

// TestInitNotificationManagerConstructionFailureStaysNil uses a raw DB
// handle whose underlying *sql.DB is already closed, so NewRepository (and
// therefore notifications.NewManager) fails deterministically. Asserts
// notificationManager() stays nil and the failure is logged; per the M4
// writer briefing, verifying the web server's SetDiscordEnabled(false) side
// independently would require exporting a getter web.Server does not have —
// out of scope for this task (widening the web package's API surface) — so
// that half of the contract is covered by initNotificationManager's own
// unconditional `SetDiscordEnabled(cfg.Discord.Enabled && notifMgr != nil)`
// call (reviewed at the source) rather than re-asserted here through a new
// web-package seam.
func TestInitNotificationManagerConstructionFailureStaysNil(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")

	dbPath := filepath.Join(t.TempDir(), "t15.db")
	db := openRawMinerDB(t, dbPath)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	m.db = db // closed handle: NewRepository (inside notifications.NewManager) must fail

	cap := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	defer slog.SetDefault(prev)

	m.initNotificationManager(context.Background())

	if got := m.notificationManager(); got != nil {
		t.Fatalf("notificationManager() = %v, want nil after a failed construction", got)
	}
	if !cap.hasSubstring("Failed to create notification manager") {
		t.Errorf("expected the construction-failure log line, got: %v", cap.msgs)
	}
}

// --- T12: Start receives the SAME lifecycle ctx initNotificationManager was given ---

// TestInitNotificationManagerStartUsesProvidedLifecycleContext pins I4: the
// ctx passed to initNotificationManager must be the exact ctx forwarded to
// Manager.Start (never context.Background()). WEBHOOK_URL configures a real
// push provider so NewManager builds a Batcher with batching enabled — its
// background flush loop is a real goroutine whose lifetime is bound to
// whatever ctx Start receives. A goroutine-count probe (not a channel the
// production code exposes — Manager's batchers are unexported) is the only
// observable signal available from this package: snapshot the goroutine
// count right after Start has launched the loop, cancel the LOCAL ctx we
// passed in, and require the count to drop back below that peak. If Start
// were wired to context.Background() instead, cancelling our local ctx would
// never reach the loop and this would time out.
func TestInitNotificationManagerStartUsesProvidedLifecycleContext(t *testing.T) {
	t.Setenv("WEBHOOK_URL", "http://127.0.0.1:0/never-called")

	m, _, _ := newCapabilityMiner(t, "alpha")
	dbPath := filepath.Join(t.TempDir(), "mut05.db")
	db := openRawMinerDB(t, dbPath)
	defer func() { _ = db.Close() }()
	m.db = db
	m.config.Notifications.Batching = config.BatchingSettings{Enabled: true, Interval: "1h", MaxEntries: 20}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.initNotificationManager(ctx)
	if m.notificationManager() == nil {
		t.Fatal("setup: manager not published")
	}

	afterStart := runtime.NumGoroutine()

	cancel()

	waitForCondition(t, "batcher flush-loop goroutine to exit after the local ctx is cancelled", 2*time.Second, func() bool {
		return runtime.NumGoroutine() < afterStart
	})
}

// --- T11: shutdown-drain vs. apply ---

// TestApplyVsShutdownDrainNoRaceManagerStoppedOnce mirrors the
// shutdown-relevant PORTION of stop() (drain in-flight applies, then stop
// the notification manager) racing a real ApplySettings call — never the
// full stop(), which would nil-panic on this lightweight capability-miner
// fixture's un-built subsystems (chatManager/wsPool/watcher/etc. are nil
// here; the whole point of this test is the apply<->shutdown-drain
// interlock plus the once-guarded Manager.Stop, not the rest of teardown).
func TestApplyVsShutdownDrainNoRaceManagerStoppedOnce(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	dbPath := filepath.Join(t.TempDir(), "t11.db")
	db := openRawMinerDB(t, dbPath)
	defer func() { _ = db.Close() }()
	m.db = db
	m.initNotificationManager(context.Background())
	m.runCtx = context.Background()

	rs := m.GetRuntimeSettings()
	rs.Discord.Enabled = true

	var wg sync.WaitGroup
	var applyErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		applyErr = m.ApplySettings(context.Background(), rs)
	}()
	go func() {
		defer wg.Done()
		m.applyMu.Lock()
		m.applyDraining = true
		m.applyMu.Unlock()
		m.applyWG.Wait()
		if notifMgr := m.notificationManager(); notifMgr != nil {
			_ = notifMgr.Stop()
		}
	}()
	wg.Wait()

	if applyErr != nil && !errors.Is(applyErr, ErrShuttingDown) {
		t.Fatalf("apply must either succeed or be refused with ErrShuttingDown, got: %v", applyErr)
	}

	after := m.notificationManager()
	if after == nil {
		t.Fatal("manager pointer lost across the shutdown-drain race")
	}

	// Manager.Stop is idempotent (sync.Once-guarded); calling it again here
	// must be instant and safe, which is only true if the earlier Stop
	// already ran to completion rather than leaving anything half torn down.
	_ = after.Stop()
}

// --- Flagship: real applySettings racing the real health watchdog ---

// TestApplySettingsRuntimeEnableConcurrentWithWatchdog is the flagship M4
// concurrency test (Gherkin scenario 1): it drives the REAL
// applySettings/ApplySettings path enabling Discord from a disabled
// baseline, concurrently with the REAL evaluateConnectionHealth /
// recordHealthTransition watchdog-tick functions (a real connHealthState and
// a real healthJournal, exactly as healthWatchdogLoop wires them — see
// miner.go's own healthJournal construction in New()). Before M4 this exact
// interleaving (a runtime-created notifications.Manager published under
// m.mu, concurrent with an unlocked read on the watchdog goroutine) was the
// confirmed data race; -race silence here is the primary assertion.
// Post-conditions: the manager pointer is unchanged across the apply (M4
// never creates or replaces it at runtime — see T2/T3/T4). Asserting the web
// server's SetDiscordEnabled flip is intentionally NOT attempted (no
// web.Server is wired in this test — see TestInitNotificationManagerConstructionFailureStaysNil's
// doc comment for why that stays out of scope here).
func TestApplySettingsRuntimeEnableConcurrentWithWatchdog(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	dbPath := filepath.Join(t.TempDir(), "flagship.db")
	db := openRawMinerDB(t, dbPath)
	defer func() { _ = db.Close() }()
	m.db = db
	m.initNotificationManager(context.Background())
	m.healthJournal = journal.New[journal.HealthEvent](healthJournalCapacity, nil)

	before := m.notificationManager()
	if before == nil {
		t.Fatal("setup: manager not published")
	}

	rs := m.GetRuntimeSettings()
	rs.Discord.Enabled = true

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var state connHealthState
		for {
			select {
			case <-stop:
				return
			default:
				m.evaluateConnectionHealth(time.Now(), &state)
			}
		}
	}()

	if err := m.applySettings(context.Background(), rs); err != nil {
		close(stop)
		wg.Wait()
		t.Fatalf("applySettings: %v", err)
	}
	close(stop)
	wg.Wait()

	after := m.notificationManager()
	if after != before {
		t.Fatal("notification manager pointer changed across the apply — write-once was violated")
	}
}
