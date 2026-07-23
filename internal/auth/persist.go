package auth

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// SaveAuth persists the current auth for this user with an atomic replace, so
// the final file is never observed partially written and a crash mid-save
// leaves the previous record intact. When TWITCH_AUTH_ENCRYPTION_KEY is set,
// the record is AES-256-GCM encrypted at rest; otherwise it is written in
// plaintext (with a one-time warning). The file is always mode 0600 regardless
// of format. Concurrent saves are serialized; each save snapshots the state at
// its own start, so the last completed save always carries the newest pair.
func (a *TwitchAuth) SaveAuth() error {
	a.saveMu.Lock()
	defer a.saveMu.Unlock()

	if err := os.MkdirAll("cookies", 0755); err != nil {
		return err
	}

	a.mu.Lock()
	stored := StoredAuth{
		AuthToken:    a.token,
		UserID:       a.userID,
		Username:     a.username,
		RefreshToken: a.refreshToken,
		TokenType:    a.tokenType,
		Scopes:       slices.Clone(a.scopes),
	}
	if !a.expiresAt.IsZero() {
		stored.ExpiresAt = a.expiresAt.UTC().Format(time.RFC3339)
	}
	snapshotGen := a.generation
	a.mu.Unlock()

	inner, err := json.Marshal(stored)
	if err != nil {
		return err
	}

	secret := encryptionSecret()

	var data []byte
	if secret != "" {
		env, err := encryptBlob(inner, secret)
		if err != nil {
			return err
		}
		data, err = json.MarshalIndent(env, "", "  ")
		if err != nil {
			return err
		}
	} else {
		warnPlaintextOnce()
		// Preserve the historical human-readable plaintext layout.
		data, err = json.MarshalIndent(stored, "", "  ")
		if err != nil {
			return err
		}
	}

	if err := a.atomicWriteFile(a.cookiesPath(), data); err != nil {
		return err
	}

	// The snapshot that just landed on disk clears the persist-pending flag —
	// but only if no newer generation was published mid-save (that one still
	// needs its own checkpoint).
	a.mu.Lock()
	if a.generation == snapshotGen {
		a.persistDirty = false
	}
	a.mu.Unlock()
	return nil
}

// ownTempPrefix is the profile-scoped temp-file prefix for the given final
// auth path (e.g. ".tester.json.auth-"). Binding temps to the exact final
// filename is what lets one profile's startup sweep remove only its OWN
// orphans — never another profile's (possibly live) temp in the shared
// cookies directory. It carries the profile basename only, never any secret.
// Glob/temp-pattern metacharacters in the basename are neutralized so a
// hostile or malformed username can neither break os.CreateTemp's pattern nor
// widen a match onto another profile's files.
func ownTempPrefix(finalPath string) string {
	base := filepath.Base(finalPath)
	base = strings.NewReplacer("*", "_", "?", "_", "[", "_", "]", "_").Replace(base)
	return "." + base + ".auth-"
}

// atomicWriteFile replaces path with data safely: a same-directory,
// profile-scoped temp file (created mode 0600, before any secret byte exists
// in it) is fully written, fsynced, closed, and renamed over the final path;
// the directory is then best-effort synced. On any failure the temp file is
// removed and the previous final file is untouched.
//
// Platform guarantee: on Unix/Linux — the production Docker target — the
// rename is an atomic same-filesystem replacement (POSIX rename(2)
// semantics), so a crash never leaves a partial or missing final file. On
// non-Unix platforms (the shipped Windows binary) the official os.Rename
// documentation states the rename is NOT atomic even within one directory:
// the write is still safe against partial content (full temp write + sync
// before the replace) but a crash exactly during the replace step may lose
// the previous file. No stronger Windows claim is made.
// The temp name carries no secret material and neither do the returned errors
// (only paths and OS error text).
func (a *TwitchAuth) atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)

	f, err := os.CreateTemp(dir, ownTempPrefix(path)+"*.tmp")
	if err != nil {
		return err
	}
	tmpPath := f.Name()

	// os.CreateTemp creates 0600 already; this keeps the guarantee explicit
	// even if that default ever changes.
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := a.fsWrite(f, data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := a.fsSync(f); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := a.fsRename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	syncDir(dir)
	return nil
}

// sweepStaleTempFiles removes THIS profile's orphaned temp files (crash
// between temp creation and rename/removal). Called from Login (startup); it
// additionally serializes on saveMu so it can never observe — let alone
// delete — a live temp of this instance's own in-flight SaveAuth. The glob is
// bound to this profile's exact temp prefix, so another profile's (possibly
// LIVE) temp in the shared cookies directory is never touched; legacy generic
// .auth-*.tmp orphans from older builds are deliberately left alone rather
// than deleted blindly — they may belong to another process.
func (a *TwitchAuth) sweepStaleTempFiles() {
	a.saveMu.Lock()
	defer a.saveMu.Unlock()

	final := a.cookiesPath()
	dir := filepath.Dir(final)
	prefix := ownTempPrefix(final)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		// Literal prefix/suffix matching — deliberately NOT a glob, so no
		// filename metacharacter can ever widen the match beyond this
		// profile's own temps.
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err == nil {
			slog.Debug("Removed stale auth temp file left by a previous crash")
		}
	}
}

// syncDir best-effort fsyncs a directory so a just-completed rename survives a
// crash. Not every platform/filesystem supports it (e.g. directories on
// Windows); failures are downgraded to debug because the rename itself already
// happened.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		slog.Debug("Directory sync after auth save not supported or failed", "error", err)
	}
}
