package logger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTailLogFamilyReturnsChronologicalTailAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	activePath := filepath.Join(dir, "family.log")
	writeTestLines(t, completedSegmentPath(activePath, 1), "old", 1, 3)
	writeTestLines(t, completedSegmentPath(activePath, 2), "archive", 1, 3)
	writeTestLines(t, activePath, "active", 1, 2)

	// Ordering belongs to the collision-safe sequence, never mutable mtimes.
	newer := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	older := newer.Add(-time.Hour)
	if err := os.Chtimes(completedSegmentPath(activePath, 1), newer, newer); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(completedSegmentPath(activePath, 2), older, older); err != nil {
		t.Fatal(err)
	}

	got, err := TailLogFamily(activePath, 5, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	want := "archive-001\narchive-002\narchive-003\nactive-001\nactive-002\n"
	if string(got) != want {
		t.Fatalf("chronological family tail:\n got %q\nwant %q", got, want)
	}
}

func TestTailLogFamilySharesOneByteBudgetAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	activePath := filepath.Join(dir, "budget.log")
	writeTestLines(t, completedSegmentPath(activePath, 1), "old-sentinel-xxxxxxxx", 1, 20)
	writeTestLines(t, completedSegmentPath(activePath, 2), "recent-xxxxxxxxxxxx", 1, 20)
	writeTestLines(t, activePath, "active-xxxxxxxxxxxx", 1, 4)

	const budget = int64(180)
	ops := defaultRotationFileOps()
	oldPath := completedSegmentPath(activePath, 1)
	openedOld := false
	ops.openRead = func(path string) (*os.File, error) {
		if path == oldPath {
			openedOld = true
			return nil, errors.New("byte budget opened oldest archive")
		}
		return os.Open(path)
	}
	got, err := tailLogFamilyWithOps(activePath, 100, budget, ops)
	if err != nil {
		t.Fatal(err)
	}
	if openedOld {
		t.Fatal("reader opened an older archive after exhausting the aggregate byte budget")
	}
	if int64(len(got)) > budget {
		t.Fatalf("returned bytes=%d exceed aggregate budget=%d", len(got), budget)
	}
	if strings.Contains(string(got), "old-sentinel") {
		t.Fatalf("reader crossed aggregate budget into oldest archive:\n%s", got)
	}
	if len(got) > 0 && got[len(got)-1] != '\n' {
		t.Fatalf("tail ends with a partial record: %q", got)
	}
}

func TestReadTailLinesStopsAtLineLimitBeforeByteBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "many-lines.log")
	var content strings.Builder
	for i := 0; i < 180_000; i++ {
		fmt.Fprintf(&content, "line-%06d-padding\n", i)
	}
	if content.Len() < 2<<20 {
		t.Fatalf("fixture unexpectedly small: %d", content.Len())
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	lines, consumed, _, err := readTailLines(file, 1, 4<<20)
	closeErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if consumed > tailReadBlockSize {
		t.Fatalf("read %d bytes for one short line, want <= one %d-byte block", consumed, tailReadBlockSize)
	}
	if len(lines) != 1 || string(lines[0]) != "line-179999-padding\n" {
		t.Fatalf("last line=%q, want exact final record", lines)
	}
}

func TestTailLogFamilyStopsOpeningOlderSegmentsWhenSatisfied(t *testing.T) {
	dir := t.TempDir()
	activePath := filepath.Join(dir, "bounded.log")
	oldPath := completedSegmentPath(activePath, 1)
	newPath := completedSegmentPath(activePath, 2)
	writeTestLines(t, oldPath, "must-not-open", 1, 1)
	writeTestLines(t, newPath, "archive", 1, 1)
	writeTestLines(t, activePath, "active", 1, 1)

	defaults := defaultRotationFileOps()
	sentinel := errors.New("older archive was opened")
	openedOld := false
	defaults.openRead = func(path string) (*os.File, error) {
		if path == oldPath {
			openedOld = true
			return nil, sentinel
		}
		return os.Open(path)
	}
	got, err := tailLogFamilyWithOps(activePath, 2, 1<<20, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if openedOld {
		t.Fatal("reader opened an older archive after the line budget was satisfied")
	}
	if string(got) != "archive-001\nactive-001\n" {
		t.Fatalf("bounded tail=%q", got)
	}
}

func TestTailLogFamilyServesArchivesWithoutCanonicalActive(t *testing.T) {
	dir := t.TempDir()
	activePath := filepath.Join(dir, "missing-active.log")
	writeTestLines(t, completedSegmentPath(activePath, 1), "archive", 1, 2)

	got, err := TailLogFamily(activePath, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "archive-001\narchive-002\n" {
		t.Fatalf("archives-only family=%q", got)
	}
}

func TestRotationCompatibleReadAllowsRenameAndPruneWhileOpen(t *testing.T) {
	dir := t.TempDir()
	activePath := filepath.Join(dir, "share-delete.log")
	archivePath := completedSegmentPath(activePath, 1)
	if err := os.WriteFile(activePath, []byte("record\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reader, err := defaultRotationFileOps().openRead(activePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if err := os.Rename(activePath, archivePath); err != nil {
		t.Fatalf("rename while retained-family reader is open: %v", err)
	}
	if err := os.Remove(archivePath); err != nil {
		t.Fatalf("prune while retained-family reader is open: %v", err)
	}
}

func TestTailLogFamilyDeduplicatesActiveRenamedDuringArchiveScan(t *testing.T) {
	dir := t.TempDir()
	activePath := filepath.Join(dir, "rollover-race.log")
	archivePath := completedSegmentPath(activePath, 1)
	if err := os.WriteFile(activePath, []byte("before-rollover\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ops := defaultRotationFileOps()
	readDir := ops.readDir
	moved := false
	ops.readDir = func(path string) ([]os.DirEntry, error) {
		if !moved {
			moved = true
			if err := os.Rename(activePath, archivePath); err != nil {
				return nil, err
			}
			if err := os.WriteFile(activePath, []byte("trigger-after-rollover\n"), 0o644); err != nil {
				return nil, err
			}
		}
		return readDir(path)
	}
	got, err := tailLogFamilyWithOps(activePath, 10, 1<<20, ops)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "before-rollover\n" {
		t.Fatalf("active-to-archive identity was duplicated or retargeted: %q", got)
	}
}

func TestTailLogFamilyIgnoresNearNamesAndNonRegularEntries(t *testing.T) {
	dir := t.TempDir()
	activePath := filepath.Join(dir, "exact.log")
	writeTestLines(t, activePath, "active", 1, 1)
	writeTestLines(t, completedSegmentPath(activePath, 1), "owned", 1, 1)

	nearNames := []string{
		activePath + ".rotated-1",
		activePath + ".rotated-0000000000000000000",
		activePath + ".rotated-000000000000000000001",
		activePath + ".rotated-0000000000000000000x",
		activePath + ".rotated-00000000000000000002.gz",
		filepath.Join(dir, "other.log.rotated-00000000000000000001"),
	}
	for _, path := range nearNames {
		if err := os.WriteFile(path, []byte("foreign\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(completedSegmentPath(activePath, 2), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nearNames[0], completedSegmentPath(activePath, 3)); err != nil && !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}

	got, err := TailLogFamily(activePath, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "owned-001\nactive-001\n" {
		t.Fatalf("reader accepted a foreign/near/non-regular entry:\n%s", got)
	}
}

func TestTailLogFamilyOversizedIncompleteLineConsumesBudget(t *testing.T) {
	dir := t.TempDir()
	activePath := filepath.Join(dir, "oversized.log")
	writeTestLines(t, completedSegmentPath(activePath, 1), "older-sentinel", 1, 1)
	if err := os.WriteFile(activePath, []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := TailLogFamily(activePath, 10, 512)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("oversized incomplete line or older archive escaped budget: %q", got)
	}
}

func TestTailLogFamilyMissingReturnsNotExist(t *testing.T) {
	_, err := TailLogFamily(filepath.Join(t.TempDir(), "absent.log"), 10, 1024)
	if !os.IsNotExist(err) {
		t.Fatalf("error=%v, want os.IsNotExist", err)
	}
}

func TestTailLogFamilyEmptyPathDoesNotTouchFilesystem(t *testing.T) {
	ops := defaultRotationFileOps()
	openCalled := false
	readDirCalled := false
	ops.openRead = func(path string) (*os.File, error) {
		openCalled = true
		return nil, errors.New("unexpected open")
	}
	ops.readDir = func(path string) ([]os.DirEntry, error) {
		readDirCalled = true
		return nil, errors.New("unexpected readdir")
	}
	_, err := tailLogFamilyWithOps("", 10, 1024, ops)
	if !os.IsNotExist(err) {
		t.Fatalf("empty path error=%v, want os.IsNotExist", err)
	}
	if openCalled || readDirCalled {
		t.Fatalf("empty path touched filesystem: open=%v readDir=%v", openCalled, readDirCalled)
	}
}

func writeTestLines(t *testing.T, path, prefix string, first, last int) {
	t.Helper()
	var content strings.Builder
	for i := first; i <= last; i++ {
		fmt.Fprintf(&content, "%s-%03d\n", prefix, i)
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
