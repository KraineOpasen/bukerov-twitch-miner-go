package util

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path with the given permissions via a temp
// file in the same directory followed by a rename, so readers never observe a
// partially written file and a crash mid-write cannot truncate the existing
// one. The temp file is fsynced before the rename and the containing directory
// is fsynced (best-effort) after it, so a completed swap survives a power loss
// with the new contents rather than a truncated or empty file. The temp file
// lives next to the target (same filesystem) to keep the rename atomic; it is
// removed on any failure before the rename. This is the same swap pattern the
// self-updater uses to replace the running binary.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	// Flush the temp file's contents to stable storage before the rename, so a
	// crash right after the swap cannot leave the target pointing at a rename
	// whose data never reached disk (a truncated or empty file).
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// CreateTemp opens 0600; apply the caller's permissions before the swap
	// so the file never transitions through a broader mode.
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}

	success = true

	// Persist the rename itself: without an fsync on the containing directory a
	// power loss can lose the directory-entry update even though the file's data
	// was synced. Best-effort — a directory fsync is meaningful on Unix (the
	// primary Linux/Docker target) and is harmlessly skipped or ignored
	// elsewhere; the atomic swap has already succeeded regardless.
	syncDir(dir)

	return nil
}

// syncDir best-effort fsyncs a directory so a completed rename survives a
// crash. Any error is ignored: on platforms where a directory cannot be
// opened or synced (e.g. Windows) this is a documented no-op, and the rename
// has already replaced the target atomically.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer func() { _ = d.Close() }()
	_ = d.Sync()
}
