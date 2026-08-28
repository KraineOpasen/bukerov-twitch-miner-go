package logger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

func TestSetupRotatesFreshMTimeMixedAgeActiveOnTrigger(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	activePath := LogFilePath("retention-red")
	before := strings.Join([]string{
		fmt.Sprintf("time=%s level=INFO msg=expired", now.Add(-25*time.Hour).Format(time.RFC3339)),
		fmt.Sprintf("time=%s level=INFO msg=fresh-before-setup", now.Add(-time.Minute).Format(time.RFC3339)),
	}, "\n") + "\n"
	if err := os.WriteFile(activePath, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	// Reproduce the stable defect: a fresh append refreshes whole-file mtime,
	// masking the expired first record from the old startup-only cleanup.
	if err := os.Chtimes(activePath, now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	l, err := Setup("retention-red", config.LoggerSettings{
		Save:         true,
		AutoClear:    true,
		ConsoleLevel: "ERROR",
		FileLevel:    "INFO",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	slog.Info("trigger-new-segment")
	l.Close()

	archivePath := activePath + ".rotated-00000000000000000001"
	archive, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("read completed segment: %v", err)
	}
	active, err := os.ReadFile(activePath)
	if err != nil {
		t.Fatalf("read canonical active: %v", err)
	}

	if !bytesContainAll(archive, "msg=expired", "msg=fresh-before-setup") {
		t.Fatalf("completed segment lost pre-rollover records:\n%s", archive)
	}
	if strings.Contains(string(archive), "trigger-new-segment") {
		t.Fatalf("triggering record landed in completed segment:\n%s", archive)
	}
	if !strings.Contains(string(active), "trigger-new-segment") {
		t.Fatalf("triggering record missing from new canonical active:\n%s", active)
	}
	if strings.Contains(string(active), "msg=expired") || strings.Contains(string(active), "fresh-before-setup") {
		t.Fatalf("pre-rollover history remained in canonical active:\n%s", active)
	}
}

func TestSetupAutoClearFalseNeverRotates(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}
	activePath := LogFilePath("retention-disabled")
	old := fmt.Sprintf("time=%s level=INFO msg=old\n", time.Now().UTC().Add(-30*time.Hour).Format(time.RFC3339))
	if err := os.WriteFile(activePath, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	archives := make(map[string]string, 8)
	for sequence := uint64(1); sequence <= 8; sequence++ {
		path := completedSegmentPath(activePath, sequence)
		content := fmt.Sprintf("preexisting-%d\n", sequence)
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		archives[path] = content
	}

	l, err := Setup("retention-disabled", config.LoggerSettings{
		Save:         true,
		AutoClear:    false,
		ConsoleLevel: "ERROR",
		FileLevel:    "INFO",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	slog.Info("still-same-active")
	l.Close()

	for path, content := range archives {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != content {
			t.Fatalf("autoClear=false pruned or changed %s: err=%v data=%q", path, err, data)
		}
	}
	if _, err := os.Stat(completedSegmentPath(activePath, 9)); !os.IsNotExist(err) {
		t.Fatalf("autoClear=false created a new completed segment: %v", err)
	}
	active, err := os.ReadFile(activePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesContainAll(active, "msg=old", "still-same-active") {
		t.Fatalf("autoClear=false did not preserve append behavior:\n%s", active)
	}
}

func bytesContainAll(data []byte, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(string(data), needle) {
			return false
		}
	}
	return true
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

type errorRecorder struct {
	mu   sync.Mutex
	errs []error
}

func (r *errorRecorder) add(err error) {
	r.mu.Lock()
	r.errs = append(r.errs, err)
	r.mu.Unlock()
}

func (r *errorRecorder) contains(target error) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, err := range r.errs {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func retentionSettings(autoClear bool) config.LoggerSettings {
	return config.LoggerSettings{
		Save:         true,
		AutoClear:    autoClear,
		ConsoleLevel: "ERROR",
		FileLevel:    "INFO",
	}
}

func TestSetupRejectsUnsafeStorageKeyBeforeFilesystemMutation(t *testing.T) {
	t.Chdir(t.TempDir())
	sentinelPath := "victim.log.rotated-00000000000000000001"
	sentinel := []byte("unrelated-outside-logs\n")
	if err := os.WriteFile(sentinelPath, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}

	unsafeKeys := []string{
		"",
		".",
		"..",
		"../victim",
		`..\victim`,
		"nested/victim",
		`C:\victim`,
		"C:victim",
		"victim.",
		"victim ",
		"CON",
		"LPT1.any",
		"CONIN$",
		"conout$.log",
		"COM¹",
		"lpt².any",
		"bad\x00key",
	}
	for _, storageKey := range unsafeKeys {
		if path := LogFilePath(storageKey); path != "" {
			t.Errorf("unsafe StorageKey %q produced locator %q", storageKey, path)
		}
		if _, err := setupWithOptions(storageKey, retentionSettings(true), rotationOptions{}); err == nil {
			t.Errorf("unsafe StorageKey %q was accepted", storageKey)
		}
	}
	data, err := os.ReadFile(sentinelPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(sentinel) {
		t.Fatalf("unsafe StorageKey changed outside sentinel: %q", data)
	}
	if _, err := os.Stat("logs"); !os.IsNotExist(err) {
		t.Fatalf("unsafe StorageKey mutated logs directory before rejection: %v", err)
	}
}

func emitAt(l *Logger, at time.Time, msg string) error {
	record := slog.NewRecord(at, slog.LevelInfo, msg, 0)
	return l.handler.Handle(context.Background(), record)
}

func TestSegmentedRotationOnWritePlacesTriggerInNewActive(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	t0 := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: t0}
	l, err := setupWithOptions("boundary", retentionSettings(true), rotationOptions{now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := emitAt(l, t0, "initial"); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(23 * time.Hour))
	if err := emitAt(l, clock.Now(), "before-boundary"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(completedSegmentPath(LogFilePath("boundary"), 1)); !os.IsNotExist(err) {
		t.Fatalf("rotation occurred before 24h: %v", err)
	}

	clock.Set(t0.Add(segmentDuration))
	if err := emitAt(l, clock.Now(), "trigger"); err != nil {
		t.Fatal(err)
	}
	l.Close()

	archive, err := os.ReadFile(completedSegmentPath(LogFilePath("boundary"), 1))
	if err != nil {
		t.Fatal(err)
	}
	active, err := os.ReadFile(LogFilePath("boundary"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytesContainAll(archive, "msg=initial", "msg=before-boundary") || strings.Contains(string(archive), "msg=trigger") {
		t.Fatalf("completed segment has wrong rollover boundary:\n%s", archive)
	}
	if !strings.Contains(string(active), "msg=trigger") || strings.Contains(string(active), "msg=initial") {
		t.Fatalf("trigger did not land exclusively in new active:\n%s", active)
	}
}

func TestSegmentedRotationPrunesOldestAtSeven(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	t0 := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: t0}
	activePath := LogFilePath("prune")
	l, err := setupWithOptions("prune", retentionSettings(true), rotationOptions{now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := emitAt(l, t0, "segment-0"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 8; i++ {
		clock.Set(t0.Add(time.Duration(i) * segmentDuration))
		if i == 8 {
			fresh := clock.Now()
			old := t0.Add(-time.Hour)
			if err := os.Chtimes(completedSegmentPath(activePath, 1), fresh, fresh); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(completedSegmentPath(activePath, 2), old, old); err != nil {
				t.Fatal(err)
			}
		}
		if err := emitAt(l, clock.Now(), fmt.Sprintf("segment-%d", i)); err != nil {
			t.Fatalf("rollover %d: %v", i, err)
		}
	}
	l.Close()

	if _, err := os.Stat(completedSegmentPath(activePath, 1)); !os.IsNotExist(err) {
		t.Fatalf("oldest sequence was not pruned first: %v", err)
	}
	for sequence := uint64(2); sequence <= 8; sequence++ {
		if _, err := os.Stat(completedSegmentPath(activePath, sequence)); err != nil {
			t.Fatalf("retained sequence %d: %v", sequence, err)
		}
	}
	data, err := TailLogFamily(activePath, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "msg=segment-0") {
		t.Fatalf("pruned oldest marker survived family:\n%s", data)
	}
	for i := 1; i <= 8; i++ {
		if got := strings.Count(string(data), fmt.Sprintf("msg=segment-%d\n", i)); got != 1 {
			t.Fatalf("segment-%d count=%d, want 1", i, got)
		}
	}
}

func TestSegmentedRotationRestartResumesOriginalSegmentAge(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	t0 := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: t0}
	l, err := setupWithOptions("restart", retentionSettings(true), rotationOptions{now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := emitAt(l, t0, "start"); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(20 * time.Hour))
	if err := emitAt(l, clock.Now(), "late-before-restart"); err != nil {
		t.Fatal(err)
	}
	l.Close()

	l, err = setupWithOptions("restart", retentionSettings(true), rotationOptions{now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(segmentDuration))
	if err := emitAt(l, clock.Now(), "trigger-after-restart"); err != nil {
		t.Fatal(err)
	}
	l.Close()

	archive1, err := os.ReadFile(completedSegmentPath(LogFilePath("restart"), 1))
	if err != nil {
		t.Fatal(err)
	}
	if !bytesContainAll(archive1, "msg=start", "msg=late-before-restart") || strings.Contains(string(archive1), "trigger-after-restart") {
		t.Fatalf("restart reset segment age or misplaced trigger:\n%s", archive1)
	}

	clock.Set(t0.Add(2 * segmentDuration))
	l, err = setupWithOptions("restart", retentionSettings(true), rotationOptions{now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := emitAt(l, clock.Now(), "second-restart-trigger"); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if _, err := os.Stat(completedSegmentPath(LogFilePath("restart"), 2)); err != nil {
		t.Fatalf("restart did not resume archive sequence: %v", err)
	}
	archive1After, err := os.ReadFile(completedSegmentPath(LogFilePath("restart"), 1))
	if err != nil {
		t.Fatal(err)
	}
	if string(archive1After) != string(archive1) {
		t.Fatalf("second restart changed the first retained segment:\n before=%s\n after=%s", archive1, archive1After)
	}
}

func TestSegmentedRotationTouchesOnlyExactOwnedRegularArchives(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}
	activePath := LogFilePath("owned")
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(activePath, []byte("time=2026-08-28T12:00:00Z level=INFO msg=active\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	for sequence := uint64(1); sequence <= 8; sequence++ {
		if err := os.WriteFile(completedSegmentPath(activePath, sequence), []byte(fmt.Sprintf("owned-%d\n", sequence)), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	unrelated := map[string]string{
		filepath.Join("logs", "other.log.rotated-00000000000000000001"): "other-profile\n",
		activePath + ".rotated-1":                       "short\n",
		activePath + ".rotated-0000000000000000000":     "nineteen\n",
		activePath + ".rotated-000000000000000000001":   "twenty-one\n",
		activePath + ".rotated-0000000000000000000x":    "nondigit\n",
		activePath + ".rotated-00000000000000000009.gz": "gzip-looking\n",
		activePath + ".tmp":                             "temp\n",
	}
	for path, content := range unrelated {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	directoryPath := completedSegmentPath(activePath, 9)
	if err := os.Mkdir(directoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkPath := completedSegmentPath(activePath, 10)
	symlinkMade := os.Symlink(filepath.Join("logs", "other.log.rotated-00000000000000000001"), symlinkPath) == nil

	clock := &fakeClock{now: now}
	l, err := setupWithOptions("owned", retentionSettings(true), rotationOptions{now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	clock.Set(now.Add(segmentDuration))
	if err := emitAt(l, clock.Now(), "skip-nonregular-collisions"); err != nil {
		t.Fatal(err)
	}
	l.Close()

	if _, err := os.Stat(completedSegmentPath(activePath, 1)); !os.IsNotExist(err) {
		t.Fatalf("oldest exact regular segment was not pruned: %v", err)
	}
	for path, content := range unrelated {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != content {
			t.Fatalf("unrelated file changed: path=%s err=%v data=%q", path, err, data)
		}
	}
	if info, err := os.Stat(directoryPath); err != nil || !info.IsDir() {
		t.Fatalf("exact-looking directory was touched: info=%v err=%v", info, err)
	}
	if symlinkMade {
		if info, err := os.Lstat(symlinkPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("exact-looking symlink was touched: info=%v err=%v", info, err)
		}
	}
	wantSequence := uint64(10)
	if symlinkMade {
		wantSequence = 11
	}
	if _, err := os.Stat(completedSegmentPath(activePath, wantSequence)); err != nil {
		t.Fatalf("non-regular name collision blocked safe rollover sequence %d: %v", wantSequence, err)
	}
}

func TestSegmentedRotationConcurrentWritesNoLossOrDuplication(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	t0 := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: t0}
	l, err := setupWithOptions("concurrent", retentionSettings(true), rotationOptions{now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := emitAt(l, t0, "pre-rollover"); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(segmentDuration))

	const goroutines, perGoroutine = 8, 100
	errCh := make(chan error, goroutines*perGoroutine)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				marker := fmt.Sprintf("marker-%03d-%03d", g, i)
				if err := emitAt(l, clock.Now(), marker); err != nil {
					errCh <- err
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent Handle: %v", err)
	}
	l.Close()

	data, err := TailLogFamily(LogFilePath("concurrent"), 2000, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	for g := 0; g < goroutines; g++ {
		for i := 0; i < perGoroutine; i++ {
			marker := fmt.Sprintf("msg=marker-%03d-%03d\n", g, i)
			if got := strings.Count(string(data), marker); got != 1 {
				t.Fatalf("%s count=%d, want 1", marker, got)
			}
		}
	}
	if got := strings.Count(string(data), "msg=pre-rollover\n"); got != 1 {
		t.Fatalf("pre-rollover count=%d, want 1", got)
	}
}

func TestSegmentedRotationWriteRacingCloseIsAccounted(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	t0 := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: t0}
	l, err := setupWithOptions("close-race", retentionSettings(true), rotationOptions{now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := emitAt(l, t0, "pre-close"); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(segmentDuration))

	start := make(chan struct{})
	var successMu sync.Mutex
	successful := make([]string, 0, 400)
	var writers sync.WaitGroup
	for g := 0; g < 4; g++ {
		writers.Add(1)
		go func(g int) {
			defer writers.Done()
			<-start
			for i := 0; i < 100; i++ {
				marker := fmt.Sprintf("close-%02d-%03d", g, i)
				if err := emitAt(l, clock.Now(), marker); err == nil {
					successMu.Lock()
					successful = append(successful, marker)
					successMu.Unlock()
				}
			}
		}(g)
	}
	closed := make(chan struct{})
	go func() {
		<-start
		l.Close()
		close(closed)
	}()
	close(start)
	writers.Wait()
	<-closed
	l.Close()

	data, err := TailLogFamily(LogFilePath("close-race"), 1000, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range successful {
		if got := strings.Count(string(data), "msg="+marker+"\n"); got != 1 {
			t.Fatalf("successful %s count=%d, want 1", marker, got)
		}
	}
}

func TestRolloverRenameFailurePreservesFreshRecordAndReports(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	t0 := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: t0}
	sentinel := errors.New("rename sentinel")
	reports := &errorRecorder{}
	ops := defaultRotationFileOps()
	failOnce := true
	ops.rename = func(oldPath, newPath string) error {
		if failOnce {
			failOnce = false
			return sentinel
		}
		return os.Rename(oldPath, newPath)
	}
	l, err := setupWithOptions("rename-failure", retentionSettings(true), rotationOptions{now: clock.Now, ops: ops, report: reports.add})
	if err != nil {
		t.Fatal(err)
	}
	if err := emitAt(l, t0, "initial"); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(segmentDuration))
	if err := emitAt(l, clock.Now(), "trigger-survives-failure"); !errors.Is(err, sentinel) {
		t.Fatalf("Handle error=%v, want sentinel", err)
	}
	if !reports.contains(sentinel) {
		t.Fatal("runtime reporter did not observe rename failure")
	}
	active, err := os.ReadFile(LogFilePath("rename-failure"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytesContainAll(active, "msg=initial", "msg=trigger-survives-failure") {
		t.Fatalf("rename failure lost fresh data:\n%s", active)
	}

	if err := emitAt(l, clock.Now(), "retry-new-active"); err != nil {
		t.Fatalf("retry rollover: %v", err)
	}
	l.Close()
	archive, err := os.ReadFile(completedSegmentPath(LogFilePath("rename-failure"), 1))
	if err != nil {
		t.Fatal(err)
	}
	active, err = os.ReadFile(LogFilePath("rename-failure"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytesContainAll(archive, "msg=initial", "msg=trigger-survives-failure") || !strings.Contains(string(active), "msg=retry-new-active") {
		t.Fatalf("retry did not preserve family: archive=%s active=%s", archive, active)
	}
}

func TestNewActiveCreateFailureRollsBackAndPreservesTrigger(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	t0 := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: t0}
	sentinel := errors.New("create new active sentinel")
	reports := &errorRecorder{}
	ops := defaultRotationFileOps()
	failCreate := false
	ops.openFile = func(path string, flag int, mode os.FileMode) (*os.File, error) {
		if failCreate && flag&os.O_EXCL != 0 {
			failCreate = false
			return nil, sentinel
		}
		return os.OpenFile(path, flag, mode)
	}
	l, err := setupWithOptions("create-failure", retentionSettings(true), rotationOptions{now: clock.Now, ops: ops, report: reports.add})
	if err != nil {
		t.Fatal(err)
	}
	if err := emitAt(l, t0, "initial"); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(segmentDuration))
	failCreate = true
	if err := emitAt(l, clock.Now(), "trigger-survives-create-failure"); !errors.Is(err, sentinel) {
		t.Fatalf("Handle error=%v, want create sentinel", err)
	}
	if !reports.contains(sentinel) {
		t.Fatal("runtime reporter did not observe new-active create failure")
	}
	activePath := LogFilePath("create-failure")
	active, err := os.ReadFile(activePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesContainAll(active, "msg=initial", "msg=trigger-survives-create-failure") {
		t.Fatalf("rollback lost triggering record:\n%s", active)
	}
	if _, err := os.Stat(completedSegmentPath(activePath, 1)); !os.IsNotExist(err) {
		t.Fatalf("successful rollback left a duplicate archive: %v", err)
	}
	if err := emitAt(l, clock.Now(), "retry-new-active"); err != nil {
		t.Fatal(err)
	}
	l.Close()
	archive, err := os.ReadFile(completedSegmentPath(activePath, 1))
	if err != nil {
		t.Fatal(err)
	}
	active, err = os.ReadFile(activePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesContainAll(archive, "msg=initial", "msg=trigger-survives-create-failure") || !strings.Contains(string(active), "msg=retry-new-active") {
		t.Fatalf("retry after rollback lost family: archive=%s active=%s", archive, active)
	}
}

func TestPruneFailureKeepsTriggerAndRetriesNextWrite(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	activePath := LogFilePath("prune-failure")
	if err := os.WriteFile(activePath, []byte("time=2026-08-28T12:00:00Z level=INFO msg=old-active\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for sequence := uint64(1); sequence <= 7; sequence++ {
		if err := os.WriteFile(completedSegmentPath(activePath, sequence), []byte(fmt.Sprintf("archive-%d\n", sequence)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	clock := &fakeClock{now: t0.Add(segmentDuration)}
	sentinel := errors.New("remove sentinel")
	reports := &errorRecorder{}
	ops := defaultRotationFileOps()
	failOnce := true
	ops.remove = func(path string) error {
		if failOnce && path == completedSegmentPath(activePath, 1) {
			failOnce = false
			return sentinel
		}
		return os.Remove(path)
	}
	l, err := setupWithOptions("prune-failure", retentionSettings(true), rotationOptions{now: clock.Now, ops: ops, report: reports.add})
	if err != nil {
		t.Fatal(err)
	}
	if err := emitAt(l, clock.Now(), "trigger-before-prune-failure"); !errors.Is(err, sentinel) {
		t.Fatalf("Handle error=%v, want prune sentinel", err)
	}
	if !reports.contains(sentinel) {
		t.Fatal("runtime reporter did not observe prune failure")
	}
	active, err := os.ReadFile(activePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(active), "msg=trigger-before-prune-failure") {
		t.Fatalf("prune failure lost triggering record:\n%s", active)
	}
	if err := emitAt(l, clock.Now(), "after-prune-retry"); err != nil {
		t.Fatalf("prune retry: %v", err)
	}
	l.Close()

	segments, _, err := scanCompletedSegments(activePath, defaultRotationFileOps())
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != maxCompletedSegments {
		t.Fatalf("completed segments=%d, want %d after retry", len(segments), maxCompletedSegments)
	}
	if _, err := os.Stat(completedSegmentPath(activePath, 1)); !os.IsNotExist(err) {
		t.Fatalf("retry did not prune oldest first: %v", err)
	}
}

func TestMalformedLegacyActiveIsDueAcrossRestartsAndPreserved(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	activePath := LogFilePath("legacy")
	legacy := []byte("legacy line without slog timestamp\n")
	if err := os.WriteFile(activePath, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(activePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(activePath, now, now); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: now}
	l, err := setupWithOptions("legacy", retentionSettings(true), rotationOptions{now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	l.Close()

	clock.Set(now.Add(time.Hour))
	l, err = setupWithOptions("legacy", retentionSettings(true), rotationOptions{now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := emitAt(l, clock.Now(), "new-active"); err != nil {
		t.Fatal(err)
	}
	l.Close()
	archive, err := os.ReadFile(completedSegmentPath(activePath, 1))
	if err != nil {
		t.Fatal(err)
	}
	if string(archive) != string(legacy) {
		t.Fatalf("legacy bytes changed during rollover: got=%q want=%q", archive, legacy)
	}
	info, err := os.Stat(activePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != beforeInfo.Mode().Perm() {
		t.Fatalf("new active mode=%#o, want preserved platform mode %#o", info.Mode().Perm(), beforeInfo.Mode().Perm())
	}
}

func TestFutureDatedActiveIsDueAndPreserved(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	activePath := LogFilePath("future")
	future := []byte("time=2026-09-04T12:00:00Z level=INFO msg=future-clock\n")
	if err := os.WriteFile(activePath, future, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(activePath, now, now); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: now}
	l, err := setupWithOptions("future", retentionSettings(true), rotationOptions{now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := emitAt(l, now, "new-active"); err != nil {
		t.Fatal(err)
	}
	l.Close()
	archive, err := os.ReadFile(completedSegmentPath(activePath, 1))
	if err != nil {
		t.Fatal(err)
	}
	if string(archive) != string(future) {
		t.Fatalf("future-dated active changed during protective rollover: %q", archive)
	}
}

func TestRestartAgeUsesTimestampPrefixFromLargeFirstRecord(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	activePath := LogFilePath("large-first-record")
	first := fmt.Sprintf("time=%s level=INFO msg=%s\n", t0.Format(time.RFC3339Nano), strings.Repeat("x", firstRecordReadLimit+1024))
	late := fmt.Sprintf("time=%s level=INFO msg=late\n", t0.Add(20*time.Hour).Format(time.RFC3339Nano))
	if err := os.WriteFile(activePath, []byte(first+late), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := t0.Add(20 * time.Hour)
	if err := os.Chtimes(activePath, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	clock := &fakeClock{now: mtime}
	l, err := setupWithOptions("large-first-record", retentionSettings(true), rotationOptions{now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(segmentDuration))
	if err := emitAt(l, clock.Now(), "trigger-after-large-first-record"); err != nil {
		t.Fatal(err)
	}
	l.Close()

	archive, err := os.ReadFile(completedSegmentPath(activePath, 1))
	if err != nil {
		t.Fatalf("large first record reset restart age: %v", err)
	}
	if string(archive) != first+late {
		t.Fatal("large first record was not preserved byte-for-byte")
	}
}

func TestActiveAgeInspectionFailureIsObservable(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("logs", 0o755); err != nil {
		t.Fatal(err)
	}
	activePath := LogFilePath("inspect-failure")
	if err := os.WriteFile(activePath, []byte("time=2026-08-28T12:00:00Z level=INFO msg=existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("inspect read sentinel")
	ops := defaultRotationFileOps()
	ops.openRead = func(path string) (*os.File, error) {
		if path == activePath {
			return nil, sentinel
		}
		return os.Open(path)
	}
	_, err := setupWithOptions("inspect-failure", retentionSettings(true), rotationOptions{ops: ops})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Setup error=%v, want observable inspection sentinel", err)
	}
}

func TestUnavailableWriterRetriesCanonicalRecovery(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	t0 := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: t0}
	renameSentinel := errors.New("rename recovery sentinel")
	reopenSentinel := errors.New("reopen recovery sentinel")
	reports := &errorRecorder{}
	ops := defaultRotationFileOps()
	failRename := true
	failReopen := false
	ops.rename = func(oldPath, newPath string) error {
		if failRename {
			failRename = false
			failReopen = true
			return renameSentinel
		}
		return os.Rename(oldPath, newPath)
	}
	ops.openFile = func(path string, flag int, mode os.FileMode) (*os.File, error) {
		if failReopen && flag == os.O_WRONLY|os.O_APPEND {
			failReopen = false
			return nil, reopenSentinel
		}
		return os.OpenFile(path, flag, mode)
	}

	l, err := setupWithOptions("recover", retentionSettings(true), rotationOptions{now: clock.Now, ops: ops, report: reports.add})
	if err != nil {
		t.Fatal(err)
	}
	if err := emitAt(l, t0, "initial"); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(segmentDuration))
	if err := emitAt(l, clock.Now(), "failed-record"); !errors.Is(err, renameSentinel) || !errors.Is(err, reopenSentinel) {
		t.Fatalf("first recovery error=%v, want both injected sentinels", err)
	}
	if !reports.contains(renameSentinel) || !reports.contains(reopenSentinel) {
		t.Fatal("runtime reporter did not observe the exhausted recovery")
	}
	if err := emitAt(l, clock.Now(), "recovered-record"); err != nil {
		t.Fatalf("next write did not recover canonical FD: %v", err)
	}
	l.Close()

	data, err := TailLogFamily(LogFilePath("recover"), 20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "msg=recovered-record\n") != 1 {
		t.Fatalf("recovered record count is not exactly one:\n%s", data)
	}
}

func TestDerivedSlogHandlerUsesNewFDThroughRollover(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	t0 := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: t0}
	l, err := setupWithOptions("derived", retentionSettings(true), rotationOptions{now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	derived := slog.New(l.handler).With("component", "retention").WithGroup("scope").With("id", 42)
	if err := emitAt(l, t0, "initial"); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(segmentDuration))
	derived.Info("derived-trigger")
	l.Close()

	archive, err := os.ReadFile(completedSegmentPath(LogFilePath("derived"), 1))
	if err != nil {
		t.Fatal(err)
	}
	active, err := os.ReadFile(LogFilePath("derived"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(archive), "derived-trigger") {
		t.Fatalf("derived handler wrote through stale completed FD:\n%s", archive)
	}
	for _, want := range []string{"msg=derived-trigger", "component=retention", "scope.id=42"} {
		if !strings.Contains(string(active), want) {
			t.Fatalf("new active missing %q from derived handler:\n%s", want, active)
		}
	}
}

func TestCloseWaitsForAdmittedRolloverWrite(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	t0 := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: t0}
	enteredRename := make(chan struct{})
	releaseRename := make(chan struct{})
	ops := defaultRotationFileOps()
	ops.rename = func(oldPath, newPath string) error {
		close(enteredRename)
		<-releaseRename
		return os.Rename(oldPath, newPath)
	}
	l, err := setupWithOptions("close-serialized", retentionSettings(true), rotationOptions{now: clock.Now, ops: ops})
	if err != nil {
		t.Fatal(err)
	}
	if err := emitAt(l, t0, "initial"); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(segmentDuration))
	writeDone := make(chan error, 1)
	go func() { writeDone <- emitAt(l, clock.Now(), "admitted-before-close") }()
	<-enteredRename
	closeDone := make(chan struct{})
	go func() {
		l.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("Close returned while an admitted rollover write held serialization")
	default:
	}
	close(releaseRename)
	if err := <-writeDone; err != nil {
		t.Fatalf("admitted write: %v", err)
	}
	<-closeDone
	if err := emitAt(l, clock.Now(), "after-close"); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("post-close write error=%v, want os.ErrClosed", err)
	}
	data, err := TailLogFamily(LogFilePath("close-serialized"), 20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "msg=admitted-before-close\n") != 1 || strings.Contains(string(data), "msg=after-close") {
		t.Fatalf("close/write accounting failed:\n%s", data)
	}
}

func TestSegmentedFileLogsRemainPlainSlogText(t *testing.T) {
	t.Chdir(t.TempDir())
	previous := slog.Default()
	defer slog.SetDefault(previous)

	t0 := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: t0}
	settings := retentionSettings(true)
	settings.Colored = true
	l, err := setupWithOptions("plain-segments", settings, rotationOptions{now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := emitAt(l, t0, "Streamer is online"); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(segmentDuration))
	if err := emitAt(l, clock.Now(), "trigger"); err != nil {
		t.Fatal(err)
	}
	l.Close()

	activePath := LogFilePath("plain-segments")
	for _, path := range []string{completedSegmentPath(activePath, 1), activePath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
			t.Fatalf("%s is gzip data", path)
		}
		for _, forbidden := range []string{"\x1b[", "🟢", "⚠️", "🔴"} {
			if strings.Contains(string(data), forbidden) {
				t.Fatalf("%s contains console decoration %q:\n%s", path, forbidden, data)
			}
		}
		if !strings.HasPrefix(string(data), "time=") || !bytesContainAll(data, " level=INFO ", "msg=") {
			t.Fatalf("%s is not plain slog text:\n%s", path, data)
		}
	}
	entries, err := os.ReadDir("logs")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".gz") {
			t.Fatalf("rotation created gzip archive %q", entry.Name())
		}
	}
}
