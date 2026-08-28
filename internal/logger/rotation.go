package logger

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	segmentDuration      = 24 * time.Hour
	maxCompletedSegments = 7
	archiveSequenceWidth = 20
	firstRecordReadLimit = 64 << 10
)

const archiveMarker = ".rotated-"

type rotationFileOps struct {
	openFile func(string, int, os.FileMode) (*os.File, error)
	openRead func(string) (*os.File, error)
	readDir  func(string) ([]os.DirEntry, error)
	lstat    func(string) (os.FileInfo, error)
	rename   func(string, string) error
	remove   func(string) error
}

func defaultRotationFileOps() rotationFileOps {
	return rotationFileOps{
		openFile: os.OpenFile,
		openRead: func(path string) (*os.File, error) {
			return os.OpenInRoot(filepath.Dir(path), filepath.Base(path))
		},
		readDir: os.ReadDir,
		lstat:   os.Lstat,
		rename:  os.Rename,
		remove:  os.Remove,
	}
}

type rotationOptions struct {
	now    func() time.Time
	ops    rotationFileOps
	report func(error)
}

func (o rotationOptions) withDefaults() rotationOptions {
	defaults := defaultRotationFileOps()
	if o.now == nil {
		o.now = time.Now
	}
	if o.ops.openFile == nil {
		o.ops.openFile = defaults.openFile
	}
	if o.ops.openRead == nil {
		o.ops.openRead = defaults.openRead
	}
	if o.ops.readDir == nil {
		o.ops.readDir = defaults.readDir
	}
	if o.ops.lstat == nil {
		o.ops.lstat = defaults.lstat
	}
	if o.ops.rename == nil {
		o.ops.rename = defaults.rename
	}
	if o.ops.remove == nil {
		o.ops.remove = defaults.remove
	}
	if o.report == nil {
		o.report = func(err error) {
			_, _ = fmt.Fprintf(os.Stderr, "logger: on-disk rotation failed: %v\n", err)
		}
	}
	return o
}

type rotatingWriter struct {
	mu sync.Mutex

	file           *os.File
	activePath     string
	mode           os.FileMode
	segmentStarted time.Time
	autoClear      bool
	prunePending   bool
	closed         bool

	now    func() time.Time
	ops    rotationFileOps
	report func(error)
}

func newRotatingWriter(file *os.File, activePath string, mode os.FileMode, started time.Time, autoClear bool, opts rotationOptions) *rotatingWriter {
	return &rotatingWriter{
		file:           file,
		activePath:     activePath,
		mode:           mode,
		segmentStarted: started,
		autoClear:      autoClear,
		now:            opts.now,
		ops:            opts.ops,
		report:         opts.report,
	}
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.writeLocked(p)
	w.mu.Unlock()

	if err != nil && !errors.Is(err, os.ErrClosed) {
		w.report(err)
	}
	return n, err
}

func (w *rotatingWriter) writeLocked(p []byte) (int, error) {
	if w.closed {
		return 0, os.ErrClosed
	}
	if w.file == nil {
		if err := w.recoverActiveLocked(w.now()); err != nil {
			return 0, err
		}
	}

	if !w.autoClear {
		return w.writeCurrentLocked(p)
	}

	if w.prunePending {
		if err := pruneCompletedSegments(w.activePath, maxCompletedSegments, w.ops); err != nil {
			n, writeErr := w.writeCurrentLocked(p)
			return n, errors.Join(fmt.Errorf("retry prune completed log segments: %w", err), writeErr)
		}
		w.prunePending = false
	}

	now := w.now()
	if now.Before(w.segmentStarted.Add(segmentDuration)) {
		return w.writeCurrentLocked(p)
	}

	n, rotated, rotateErr := w.rotateAndWriteLocked(p, now)
	if !rotated {
		return n, rotateErr
	}

	if pruneErr := pruneCompletedSegments(w.activePath, maxCompletedSegments, w.ops); pruneErr != nil {
		w.prunePending = true
		rotateErr = errors.Join(rotateErr, fmt.Errorf("prune completed log segments: %w", pruneErr))
	}
	return n, rotateErr
}

func (w *rotatingWriter) writeCurrentLocked(p []byte) (int, error) {
	if w.file == nil {
		return 0, os.ErrClosed
	}
	n, err := w.file.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (w *rotatingWriter) rotateAndWriteLocked(p []byte, now time.Time) (int, bool, error) {
	_, maxSequence, err := scanCompletedSegments(w.activePath, w.ops)
	if err != nil {
		n, writeErr := w.writeCurrentLocked(p)
		return n, false, errors.Join(fmt.Errorf("list completed log segments: %w", err), writeErr)
	}
	if maxSequence == math.MaxUint64 {
		n, writeErr := w.writeCurrentLocked(p)
		return n, false, errors.Join(errors.New("completed log segment sequence exhausted"), writeErr)
	}

	sequence := maxSequence + 1
	var archivePath string
	for {
		archivePath = completedSegmentPath(w.activePath, sequence)
		if _, err := w.ops.lstat(archivePath); os.IsNotExist(err) {
			break
		} else if err != nil {
			n, writeErr := w.writeCurrentLocked(p)
			return n, false, errors.Join(fmt.Errorf("check completed log segment destination: %w", err), writeErr)
		}
		if sequence == math.MaxUint64 {
			n, writeErr := w.writeCurrentLocked(p)
			return n, false, errors.Join(errors.New("completed log segment sequence exhausted"), writeErr)
		}
		sequence++
	}

	if err := w.file.Sync(); err != nil {
		n, writeErr := w.writeCurrentLocked(p)
		return n, false, errors.Join(fmt.Errorf("sync active log before rollover: %w", err), writeErr)
	}
	if err := w.file.Close(); err != nil {
		w.file = nil
		reopenErr := w.reopenActiveLocked()
		if reopenErr != nil {
			return 0, false, errors.Join(fmt.Errorf("close active log before rollover: %w", err), reopenErr)
		}
		n, writeErr := w.writeCurrentLocked(p)
		return n, false, errors.Join(fmt.Errorf("close active log before rollover: %w", err), writeErr)
	}
	w.file = nil

	if err := w.ops.rename(w.activePath, archivePath); err != nil {
		reopenErr := w.reopenActiveLocked()
		if reopenErr != nil {
			return 0, false, errors.Join(fmt.Errorf("rename active log for rollover: %w", err), reopenErr)
		}
		n, writeErr := w.writeCurrentLocked(p)
		return n, false, errors.Join(fmt.Errorf("rename active log for rollover: %w", err), writeErr)
	}

	newFile, openErr := w.ops.openFile(w.activePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, w.mode)
	if openErr != nil {
		rollbackErr := w.ops.rename(archivePath, w.activePath)
		if rollbackErr == nil {
			reopenErr := w.reopenActiveLocked()
			if reopenErr != nil {
				return 0, false, errors.Join(fmt.Errorf("create new active log: %w", openErr), reopenErr)
			}
			n, writeErr := w.writeCurrentLocked(p)
			return n, false, errors.Join(fmt.Errorf("create new active log: %w", openErr), writeErr)
		}

		// The old segment is safely archived. One final create attempt can
		// restore a usable canonical path without rewriting that history.
		newFile, openErr = w.ops.openFile(w.activePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, w.mode)
		if openErr != nil {
			return 0, true, errors.Join(
				fmt.Errorf("create new active log: %w", openErr),
				fmt.Errorf("rollback completed segment: %w", rollbackErr),
			)
		}
		w.file = newFile
		w.segmentStarted = now
		n, writeErr := w.writeCurrentLocked(p)
		return n, true, errors.Join(fmt.Errorf("rollback completed segment: %w", rollbackErr), writeErr)
	}

	w.file = newFile
	w.segmentStarted = now
	n, writeErr := w.writeCurrentLocked(p)
	return n, true, writeErr
}

func (w *rotatingWriter) reopenActiveLocked() error {
	file, err := w.ops.openFile(w.activePath, os.O_WRONLY|os.O_APPEND, w.mode)
	if err != nil {
		w.file = nil
		return fmt.Errorf("reopen canonical active log: %w", err)
	}
	w.file = file
	return nil
}

func (w *rotatingWriter) recoverActiveLocked(now time.Time) error {
	file, openErr := w.ops.openFile(w.activePath, os.O_WRONLY|os.O_APPEND, w.mode)
	if openErr == nil {
		w.file = file
		return nil
	}
	if !os.IsNotExist(openErr) {
		return fmt.Errorf("recover canonical active log: %w", openErr)
	}

	file, createErr := w.ops.openFile(w.activePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, w.mode)
	if createErr != nil {
		return errors.Join(
			fmt.Errorf("recover missing canonical active log: %w", openErr),
			fmt.Errorf("recreate canonical active log: %w", createErr),
		)
	}
	w.file = file
	w.segmentStarted = now
	return nil
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	file := w.file
	w.file = nil
	w.mu.Unlock()

	if file == nil {
		return nil
	}
	err := file.Close()
	if err != nil {
		w.report(fmt.Errorf("close active log: %w", err))
	}
	return err
}

type completedSegment struct {
	path     string
	sequence uint64
	info     os.FileInfo
}

func completedSegmentPath(activePath string, sequence uint64) string {
	return fmt.Sprintf("%s%s%0*d", activePath, archiveMarker, archiveSequenceWidth, sequence)
}

func parseCompletedSegmentName(activeBase, name string) (uint64, bool) {
	prefix := activeBase + archiveMarker
	if !strings.HasPrefix(name, prefix) || len(name) != len(prefix)+archiveSequenceWidth {
		return 0, false
	}
	digits := name[len(prefix):]
	for _, char := range digits {
		if char < '0' || char > '9' {
			return 0, false
		}
	}
	sequence, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || sequence == 0 {
		return 0, false
	}
	return sequence, true
}

func scanCompletedSegments(activePath string, ops rotationFileOps) ([]completedSegment, uint64, error) {
	dir := filepath.Dir(activePath)
	activeBase := filepath.Base(activePath)
	entries, err := ops.readDir(dir)
	if err != nil {
		return nil, 0, err
	}

	segments := make([]completedSegment, 0, maxCompletedSegments+1)
	var maxSequence uint64
	for _, entry := range entries {
		sequence, ok := parseCompletedSegmentName(activeBase, entry.Name())
		if !ok {
			continue
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, 0, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if sequence > maxSequence {
			maxSequence = sequence
		}
		segments = append(segments, completedSegment{
			path:     filepath.Join(dir, entry.Name()),
			sequence: sequence,
			info:     info,
		})
	}

	sort.Slice(segments, func(i, j int) bool {
		return segments[i].sequence < segments[j].sequence
	})
	return segments, maxSequence, nil
}

func pruneCompletedSegments(activePath string, keep int, ops rotationFileOps) error {
	segments, _, err := scanCompletedSegments(activePath, ops)
	if err != nil {
		return fmt.Errorf("list completed log segments: %w", err)
	}
	if len(segments) <= keep {
		return nil
	}

	for _, segment := range segments[:len(segments)-keep] {
		current, err := ops.lstat(segment.path)
		if err != nil {
			return fmt.Errorf("verify oldest completed log segment: %w", err)
		}
		if !current.Mode().IsRegular() || !os.SameFile(segment.info, current) {
			return fmt.Errorf("oldest completed log segment changed before prune: %s", filepath.Base(segment.path))
		}
		if err := ops.remove(segment.path); err != nil {
			return fmt.Errorf("remove oldest completed log segment %s: %w", filepath.Base(segment.path), err)
		}
	}
	return nil
}

func inspectActiveSegment(path string, now time.Time, ops rotationFileOps) (time.Time, os.FileMode, error) {
	info, err := ops.lstat(path)
	if os.IsNotExist(err) {
		return now, 0o644, nil
	}
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("inspect active log: %w", err)
	}
	if !info.Mode().IsRegular() {
		return time.Time{}, 0, fmt.Errorf("active log is not a regular file: %s", filepath.Base(path))
	}
	mode := info.Mode().Perm()
	if info.Size() == 0 {
		return now, mode, nil
	}

	f, err := ops.openRead(path)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("open active log to inspect segment age: %w", err)
	}
	readSize := min(info.Size(), int64(firstRecordReadLimit))
	prefix := make([]byte, int(readSize))
	n, readErr := f.ReadAt(prefix, 0)
	closeErr := f.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return time.Time{}, 0, fmt.Errorf("read active log to inspect segment age: %w", readErr)
	}
	if n != len(prefix) {
		return time.Time{}, 0, fmt.Errorf("read active log to inspect segment age: %w", io.ErrUnexpectedEOF)
	}
	if closeErr != nil {
		return time.Time{}, 0, fmt.Errorf("close active log after inspecting segment age: %w", closeErr)
	}
	if started, ok := parseFirstRecordTime(prefix); ok {
		if started.After(now) {
			return now.Add(-segmentDuration), mode, nil
		}
		return started, mode, nil
	}
	// An unparseable non-empty active file has no durable segment-start
	// timestamp. Treat it as due on the next write so repeated restarts cannot
	// turn append-refreshed ModTime into unbounded retention. Rollover preserves
	// its bytes unchanged in a completed segment.
	return now.Add(-segmentDuration), mode, nil
}

func parseFirstRecordTime(line []byte) (time.Time, bool) {
	const prefix = "time="
	if !strings.HasPrefix(string(line), prefix) {
		return time.Time{}, false
	}
	value := string(line[len(prefix):])
	if end := strings.IndexByte(value, ' '); end >= 0 {
		value = value[:end]
	}
	value = strings.TrimSpace(value)
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed, err == nil
}
