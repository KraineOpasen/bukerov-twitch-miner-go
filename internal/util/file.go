package util

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path with the given permissions via a temp
// file in the same directory followed by a rename, so readers never observe a
// partially written file and a crash mid-write cannot truncate the existing
// one. The temp file lives next to the target (same filesystem) to keep the
// rename atomic; it is removed on any failure before the rename. This is the
// same swap pattern the self-updater uses to replace the running binary.
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
	return nil
}
