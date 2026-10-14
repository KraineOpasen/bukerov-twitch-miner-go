#!/usr/bin/env python3
"""Installer for the PermissionRequest guard (s1h-contract-rev12, section 11; run 20261011T173421Z).

Owner-run only (S1H-OD-1 = OPERATOR). Every mode requires BOTH integrity pins:

    python3 -I install_permission_guard.py install  --manifest EVIDENCE_ROOT/r9-install-manifest.json \
        --expect-sha256 <sha256 of this file> --expect-manifest-sha256 <sha256 of that manifest>
    python3 -I install_permission_guard.py verify   (same manifest and pins; never writes)
    python3 -I install_permission_guard.py rollback (same manifest and pins)
    python3 -I install_permission_guard.py promote  --manifest EVIDENCE_ROOT/r9-promote-manifest.json ...

Authorization boundary (11.0). Every destination is a constant in this file; the writable manifest can
only repeat these exact values and can never widen them:
  CLONE           /home/claude/bukerov-twitch-miner-go
  SOURCE          CLONE/.claude/hooks/permission_guard.py
  SETTINGS        CLONE/.claude/settings.local.json
  HOOKS_DIR       /home/claude/.claude/hooks                (must be an existing real directory)
  RUNTIME_PARENT  HOOKS_DIR/permission-guard                (created with a plain mkdir when absent;
                                                             the manifest key `runtime_root` names it)
  runtime dir     RUNTIME_PARENT/<first 12 hex of the manifest's source_sha256>
  EVIDENCE_DIR    CLONE/.scratch/s1h-20261011T173421Z/r9-install
                  (holds settings.local.json.preimage-<UTC>-<pid>, permission-guard-state.json, receipts/)
  manifest paths  CLONE/.scratch/s1h-20261011T173421Z/r9-install-manifest.json (install, verify, rollback)
                  CLONE/.scratch/s1h-20261011T173421Z/r9-promote-manifest.json (promote)
The registration is the fixed section-4 template; a manifest `registration` must equal it exactly.
The installer hashes its own bytes first (--expect-sha256) and the manifest bytes before parsing them
(--expect-manifest-sha256); a manifest-owned source_sha256 is never independent authority.

Containment (CT1-CT6). Roots are pinned above (CT1) and used only when they are existing real
directories whose realpath equals their own normalized path (CT2). Every derived path is absolute,
normalized, without `.`, `..`, empty components or NUL (CT3); every existing component from its root
down to its parent is lstat'ed and must be a real directory, realpath(parent) must equal or lie below
the root, the basename is a plain name, and a non-existent parent is a failure (CT4); an existing
candidate is lstat'ed and must be a regular file (or, for the installer's directories, a real directory)
and not a symlink (CT5). The checks are repeated immediately before each write or rename; temporary files
are created in the verified parent with O_EXCL|O_NOFOLLOW and moved with os.replace (CT6). This narrows
the check-to-use window; it does not eliminate it (a process of the same OS account can still race it).

Write outcomes (rev15, 7.11b (2)):
  W  once os.replace of the settings target (or, for an ABSENT rollback, its rename) has returned, the
     write is recorded at once (settings_written / settings_restored, temp_path,
     intended_postimage_sha256), with SIGINT/SIGTERM blocked across the call and that record; the
     read-back of the postimage is a later step, and its failure is exit 7 POSTIMAGE_UNREADABLE with
     postimage_sha256 null. Any refusal after a committed write is FAILED / exit 7 (original_code
     kept), never a refusal of input. A state whose postimage is null or differs from the intended
     one is never acted on: rollback and promote refuse POSTIMAGE_UNKNOWN (exit 4) and verify reports
     DRIFT.
  A  before the replacement has returned, every handled failure or interrupt after this run's mkdir of
     <HASH12> renames that directory to <HASH12>.aborted-<UTC>-<pid> exactly once (failed_step and
     runtime_renamed_to in the receipt).
  F3 after the replacement has returned, no failure, interrupt or SIGTERM touches the runtime directory:
     the write is recorded, failed_step and interrupted are set, state and receipt are saved, exit 7.
     Only the first interrupt raises; every later one is recorded as late_interrupt (never exit 0).
  S  the state file is saved before the receipt; when that save fails the result is
     <RESULT>_STATE_NOT_SAVED with state_saved false and original_result / original_reason kept (exit 7);
     a receipt that cannot be written says receipt_saved false (exit 7 unless a refusal keeps its code).

Tests pass --test-root DIR (an existing real directory directly under the system temporary directory):
every pinned constant, including the manifest paths, is then rebased under DIR, every receipt carries
`test_root`, and the test seams --test-barrier DIR (contained in DIR), --test-crash-before-replace,
--test-short-write, --test-fail-after-write and --test-fault POINT (POINT from TEST_FAULT_POINTS) are
accepted only together with it.

Every run prints one JSON receipt as its last stdout line; once the manifest is verified the receipt
also names the rollback command with both pins. Exit codes:
  0 installed / idempotent / MATCH / rolled back / promoted
  1 verify DRIFT, INCOMPLETE_INSTALL (no trustworthy state but a registration, runtime dir or .aborted-
    archive), NOT_INSTALLED or STATE_UNREADABLE
  2 refused input: a pin, a manifest value, a flag, a path that fails containment, malformed JSON, an
    unreadable state file or runtime parent (nothing written)
  3 FOREIGN_HOOK (another PermissionRequest entry present)
  4 rollback or promote refused: settings drifted from the recorded postimage, or POSTIMAGE_UNKNOWN;
    install refused RUNTIME_DRIFT (registration present but the runtime file is not intact)
  5 runtime directory for that hash exists, or promote with the same hash
  6 DRIFT_BEFORE_WRITE (settings changed between the preimage and the write)
  7 write failed or interrupted (a temporary settings file, once created, is named in the receipt),
    or a failure after a completed write, or a state or receipt that could not be saved
verify precedence: STATE_UNREADABLE (unreadable, symlinked, irregular or malformed state); a readable
state with a committed write (MATCH/DRIFT; DRIFT when the postimage is unknown or a file cannot be read);
INCOMPLETE_INSTALL (no trustworthy state, but a registration, runtime dir or .aborted- archive remains);
NOT_INSTALLED. Receipts and the state file are ASCII-escaped JSON.
A refusal before the evidence directory is verified is printed only (never written to an
unverified path). The installer never deletes anything: it renames (rollback, promote, a failed
install's runtime dir -> <HASH12>.aborted-<UTC>-<pid>) and replaces atomically.
"""

import argparse
import errno
import hashlib
import json
import os
import secrets
import signal
import stat
import sys
import tempfile
import time

CLONE = "/home/claude/bukerov-twitch-miner-go"
HOOKS_DIR = "/home/claude/.claude/hooks"
RUNID_DIR = "s1h-20261011T173421Z"
GUARD_FILENAME = "permission_guard.py"
STATE_FILE = "permission-guard-state.json"
RECEIPTS_DIR = "receipts"
PREIMAGE_PREFIX = "settings.local.json.preimage-"
TEMPLATE_COMMAND = "python3 -I RUNTIME --expect-sha256 SHA"
REGISTRATION_TEMPLATE = {"matcher": "Bash",
                         "hooks": [{"type": "command", "command": TEMPLATE_COMMAND, "timeout": 10}]}
MANIFEST_KEYS_REQUIRED = ("source", "source_sha256", "runtime_root", "settings_target", "evidence_dir")
MANIFEST_KEYS_ALLOWED = MANIFEST_KEYS_REQUIRED + ("registration",)

EXIT_OK = 0
EXIT_VERIFY_DRIFT = 1
EXIT_PRECONDITION = 2
EXIT_FOREIGN_HOOK = 3
EXIT_STATE_DRIFT = 4
EXIT_RUNTIME_EXISTS = 5
EXIT_DRIFT_BEFORE_WRITE = 6
EXIT_WRITE_FAILED = 7

BARRIER_TIMEOUT_SECONDS = 15
OPEN_NOFOLLOW = getattr(os, "O_NOFOLLOW", 0)
OPEN_CLOEXEC = getattr(os, "O_CLOEXEC", 0)
# --test-fault POINT (tests only, with --test-root): the fixed failure-injection points of 7.11b (2)
TEST_FAULT_POINTS = ("postimage-read", "runtime-copy-write", "interrupt-before-replace", "interrupt-after-replace",
                     "state-save", "receipt-save", "dir-fsync", "temp-readback", "state-read")
STEP_AFTER_REPLACE = "after the settings replacement"


class Interrupted(BaseException):
    """SIGINT/SIGTERM during a transaction (or the interrupt test faults); never an Exception."""


def inject_fault(seams, point):
    """Raise the injected failure for POINT when the test asked for it."""
    if point in (seams or {}).get("faults", ()):
        if point.startswith("interrupt-"):
            raise Interrupted("TEST_FAULT " + point)
        raise OSError(errno.EIO, "TEST_FAULT " + point)


class Refusal(Exception):
    def __init__(self, code, reason, **extra):
        super().__init__(reason)
        self.code = code
        self.reason = reason
        self.extra = extra


def refuse(reason, code=EXIT_PRECONDITION, **extra):
    raise Refusal(code, reason, **extra)


class Layout:
    """The pinned destinations of 11.0, optionally rebased under a test root."""

    def __init__(self, test_root=None):
        prefix = test_root or ""
        self.test_root = test_root
        self.clone = prefix + CLONE
        self.source_dir = self.clone + "/.claude/hooks"
        self.source = self.source_dir + "/" + GUARD_FILENAME
        self.settings_dir = self.clone + "/.claude"
        self.settings = self.settings_dir + "/settings.local.json"
        self.hooks_dir = prefix + HOOKS_DIR
        self.runtime_parent = self.hooks_dir + "/permission-guard"
        self.evidence_parent = self.clone + "/.scratch"
        self.evidence_root = self.evidence_parent + "/" + RUNID_DIR
        self.evidence_dir = self.evidence_root + "/r9-install"
        self.install_manifest = self.evidence_root + "/r9-install-manifest.json"
        self.promote_manifest = self.evidence_root + "/r9-promote-manifest.json"
        self.state_path = self.evidence_dir + "/" + STATE_FILE

    def manifest_for(self, mode):
        return self.promote_manifest if mode == "promote" else self.install_manifest


def utc_stamp():
    return time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())


def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def is_hex64(value):
    return isinstance(value, str) and len(value) == 64 and all(c in "0123456789abcdef" for c in value)


def _reject_duplicate_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate key: " + key)
        result[key] = value
    return result


def loads_strict(data):
    return json.loads(data.decode("utf-8"), object_pairs_hook=_reject_duplicate_keys)


def dumps(obj):
    return json.dumps(obj, indent=2, ensure_ascii=False) + "\n"


def dumps_record(obj):
    """Receipts, the state file and the printed receipt: ASCII-escaped, so no text read from the settings (for example
    a lone-surrogate escape) can make serialization fail after the outcome is decided."""
    return json.dumps(obj, indent=2, ensure_ascii=True, default=str) + "\n"


INTERRUPT_SIGNALS = (signal.SIGINT, signal.SIGTERM)
LATE_INTERRUPTS = []


class interrupts_blocked:
    """Block SIGINT/SIGTERM across a write and the statement that records it (W, A, F3): a signal that arrives in between
    is delivered when the block ends, after the record exists."""

    def __enter__(self):
        self.previous = signal.pthread_sigmask(signal.SIG_BLOCK, INTERRUPT_SIGNALS)
        return self

    def __exit__(self, *exc):
        signal.pthread_sigmask(signal.SIG_SETMASK, self.previous)
        return False


# --- containment (CT2-CT5) ----------------------------------------------------------------

def path_syntax_ok(path):
    """CT3: absolute, normalized, no `.`/`..`/empty component, no NUL."""
    if not isinstance(path, str) or not path.startswith("/") or "\0" in path:
        return False
    if os.path.normpath(path) != path:
        return False
    return all(part not in ("", ".", "..") for part in path.split("/")[1:])


def require_real_dir(path, reason):
    """CT2: an existing real directory (lstat: a directory, not a symlink) whose realpath equals itself."""
    if not path_syntax_ok(path):
        refuse(reason, path=path)
    try:
        info = os.lstat(path)
    except OSError as exc:
        refuse(reason, path=path, detail=str(exc))
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        refuse(reason, path=path)
    if os.path.realpath(path) != path:
        refuse(reason, path=path, detail="realpath differs")
    return path


def require_contained(candidate, root, kind, reason, must_exist=False):
    """CT3-CT5: `candidate` lies under the real directory `root`; every existing component from root
    to its parent is a real directory; realpath(parent) is root or below; plain basename; an existing
    candidate is of `kind` ("file" or "dir") and not a symlink."""
    if not path_syntax_ok(candidate):
        refuse(reason, path=candidate, detail="path syntax")
    require_real_dir(root, reason)
    parent, base = os.path.split(candidate)
    if base in ("", ".", "..") or "/" in base:
        refuse(reason, path=candidate, detail="basename")
    if parent != root and not parent.startswith(root + "/"):
        refuse(reason, path=candidate, detail="outside root")
    current = root
    rel = parent[len(root):].strip("/")
    for component in (rel.split("/") if rel else []):
        current = current + "/" + component
        try:
            info = os.lstat(current)
        except OSError:
            refuse(reason, path=candidate, detail="non-existent parent")
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            refuse(reason, path=candidate, detail="parent component not a real directory")
    real_parent = os.path.realpath(parent)
    if real_parent != root and not real_parent.startswith(root + os.sep):
        refuse(reason, path=candidate, detail="realpath outside root")
    try:
        info = os.lstat(candidate)
    except FileNotFoundError:
        info = None
    if info is None:
        if must_exist:
            refuse(reason, path=candidate, detail="missing")
        return candidate
    if stat.S_ISLNK(info.st_mode):
        refuse(reason, path=candidate, detail="symlink")
    if kind == "file" and not stat.S_ISREG(info.st_mode):
        refuse(reason, path=candidate, detail="not a regular file")
    if kind == "dir" and not stat.S_ISDIR(info.st_mode):
        refuse(reason, path=candidate, detail="not a directory")
    return candidate


def read_regular(path, reason):
    """Read a regular file without following a final symlink."""
    try:
        fd = os.open(path, os.O_RDONLY | OPEN_NOFOLLOW | OPEN_CLOEXEC)
    except OSError as exc:
        refuse(reason, path=path, detail=str(exc))
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            refuse(reason, path=path, detail="not a regular file")
        chunks = []
        while True:
            chunk = os.read(fd, 1024 * 1024)
            if not chunk:
                break
            chunks.append(chunk)
        return b"".join(chunks)
    finally:
        os.close(fd)


def write_new_file(path, root, data, mode=0o600):
    """Create a new file (O_EXCL|O_NOFOLLOW) inside a contained, verified parent."""
    require_contained(path, root, "file", "WRITE_TARGET_NOT_CONTAINED")
    if os.path.lexists(path):
        refuse("WRITE_TARGET_EXISTS", EXIT_WRITE_FAILED, path=path)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | OPEN_NOFOLLOW | OPEN_CLOEXEC, mode)
    try:
        view = memoryview(data)
        written = 0
        while written < len(view):
            written += os.write(fd, view[written:])
        os.fsync(fd)
    finally:
        os.close(fd)
    return path


def rename_contained(src, dst, root, kind):
    """Rename an installer-owned entry to a sibling built from pinned roots; never overwrite."""
    require_contained(src, root, kind, "RENAME_SOURCE_NOT_CONTAINED", must_exist=True)
    require_contained(dst, root, kind, "RENAME_TARGET_NOT_CONTAINED")
    if os.path.lexists(dst):
        refuse("RENAME_TARGET_EXISTS", EXIT_WRITE_FAILED, path=dst)
    os.rename(src, dst)
    return dst


def write_atomic(target, root, data, mode, seams=None, progress=None, on_replaced=None):
    """Temporary file in the verified parent (O_EXCL|O_NOFOLLOW), full write, fsync, hash check,
    containment re-check (CT6), os.replace. For the settings target, `progress` (the receipt) gets the
    temporary file name as soon as it exists, every later refusal exits 7 and names it (F6), and
    `on_replaced` records the completed write immediately after os.replace returns (invariant W)."""
    seams = seams or {}
    require_contained(target, root, "file", "WRITE_TARGET_NOT_CONTAINED")
    directory = os.path.dirname(target)
    temp_path = f"{directory}/.{os.path.basename(target)}.tmp-{os.getpid()}-{secrets.token_hex(6)}"
    try:
        fd = os.open(temp_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | OPEN_NOFOLLOW | OPEN_CLOEXEC, 0o600)
    except OSError as exc:
        refuse("TEMP_CREATE_FAILED", EXIT_WRITE_FAILED, detail=str(exc))
    if progress is not None:
        progress["temp_path"] = temp_path
    try:
        try:
            view = memoryview(data)
            written = 0
            while written < len(view):
                chunk = view[written:]
                if seams.get("short_write"):  # test seam: behave like a device that took half and stopped
                    chunk = view[written:written + max(1, len(view) // 2)]
                count = os.write(fd, chunk)
                if count <= 0:
                    refuse("SHORT_WRITE", EXIT_WRITE_FAILED, written=written, expected=len(data))
                written += count
                if seams.get("short_write"):
                    break
            os.fsync(fd)
        except OSError as exc:
            refuse("TEMP_WRITE_FAILED", EXIT_WRITE_FAILED, detail=str(exc))
        finally:
            os.close(fd)
        try:
            inject_fault(seams, "temp-readback")
            readback = read_regular(temp_path, "TEMP_READBACK_FAILED")
        except OSError as exc:
            refuse("TEMP_READBACK_FAILED", EXIT_WRITE_FAILED, detail=str(exc))
        if sha256_bytes(readback) != sha256_bytes(data):
            refuse("TEMP_HASH_MISMATCH", EXIT_WRITE_FAILED)
        try:
            os.chmod(temp_path, mode)
        except OSError as exc:
            refuse("TEMP_CHMOD_FAILED", EXIT_WRITE_FAILED, detail=str(exc))
        if seams.get("crash_before_replace"):
            refuse("TEST_CRASH_BEFORE_REPLACE", EXIT_WRITE_FAILED)
        inject_fault(seams, "interrupt-before-replace")
        require_contained(target, root, "file", "WRITE_TARGET_NOT_CONTAINED")  # CT6: immediately before use
        require_contained(temp_path, root, "file", "TEMP_NOT_CONTAINED", must_exist=True)
    except Refusal as refusal:
        # the temporary file exists: writing had begun, so this is never "refused input" (F6)
        refusal.code = EXIT_WRITE_FAILED
        refusal.extra.setdefault("temp_path", temp_path)
        raise
    with interrupts_blocked():  # the replace and its record are one step for SIGINT/SIGTERM (W, F3)
        try:
            os.replace(temp_path, target)
        except OSError as exc:
            refuse("REPLACE_FAILED", EXIT_WRITE_FAILED, temp_path=temp_path, detail=str(exc))
        if on_replaced is not None:
            on_replaced(temp_path)
    inject_fault(seams, "interrupt-after-replace")
    try:
        inject_fault(seams, "dir-fsync")
        dir_fd = os.open(directory, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except OSError as exc:  # the replace already happened; recorded, never swallowed (F5)
        if progress is not None:
            progress["dir_fsync_error"] = f"{type(exc).__name__}: {exc}"
    return temp_path


# --- manifest and preconditions (11.0, 11.1, 11.7) ------------------------------------------

def validate_test_root(path):
    if path is None:
        return None
    if not path_syntax_ok(path):
        refuse("TEST_ROOT_INVALID", path=path)
    try:
        info = os.lstat(path)
    except OSError:
        refuse("TEST_ROOT_INVALID", path=path, detail="missing")
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode) or os.path.realpath(path) != path:
        refuse("TEST_ROOT_INVALID", path=path, detail="not a real directory")
    if os.path.dirname(path) != os.path.realpath(tempfile.gettempdir()):
        refuse("TEST_ROOT_INVALID", path=path, detail="not directly under the system temporary directory")
    return path


def load_manifest(layout, mode, manifest_path, expected_sha, receipt):
    if not is_hex64(expected_sha):
        refuse("MANIFEST_HASH_MISSING")
    if manifest_path != layout.manifest_for(mode):
        refuse("MANIFEST_PATH_NOT_PINNED", path=manifest_path, pinned=layout.manifest_for(mode))
    require_real_dir(layout.evidence_parent, "EVIDENCE_PARENT_NOT_REAL_DIRECTORY")
    require_contained(layout.evidence_root, layout.evidence_parent, "dir", "EVIDENCE_ROOT_NOT_REAL_DIRECTORY",
                      must_exist=True)
    require_real_dir(layout.evidence_root, "EVIDENCE_ROOT_NOT_REAL_DIRECTORY")
    try:
        info = os.lstat(manifest_path)
    except OSError as exc:
        refuse("MANIFEST_MISSING", detail=str(exc))
    if not stat.S_ISREG(info.st_mode):
        refuse("MANIFEST_NOT_REGULAR")
    require_contained(manifest_path, layout.evidence_root, "file", "MANIFEST_NOT_CONTAINED", must_exist=True)
    data = read_regular(manifest_path, "MANIFEST_MISSING")
    actual = sha256_bytes(data)
    if actual != expected_sha:
        refuse("MANIFEST_HASH_MISMATCH", actual_manifest_sha256=actual)
    receipt["manifest_sha256_verified"] = actual
    try:
        manifest = loads_strict(data)
    except (ValueError, UnicodeDecodeError) as exc:
        refuse("MANIFEST_MALFORMED", detail=str(exc))
    if not isinstance(manifest, dict):
        refuse("MANIFEST_NOT_OBJECT")
    for key in manifest:
        if key not in MANIFEST_KEYS_ALLOWED:
            refuse("MANIFEST_UNKNOWN_KEY", key=key)
    for key in MANIFEST_KEYS_REQUIRED:
        if key not in manifest:
            refuse("MANIFEST_KEY_MISSING", key=key)
    if manifest["source"] != layout.source:
        refuse("MANIFEST_SOURCE_NOT_PINNED", value=manifest["source"])
    if not is_hex64(manifest["source_sha256"]):
        refuse("MANIFEST_SOURCE_SHA_MALFORMED")
    if manifest["runtime_root"] != layout.runtime_parent:
        refuse("MANIFEST_RUNTIME_ROOT_NOT_PINNED", value=manifest["runtime_root"])
    if manifest["settings_target"] != layout.settings:
        refuse("MANIFEST_SETTINGS_TARGET_NOT_PINNED", value=manifest["settings_target"])
    if manifest["evidence_dir"] != layout.evidence_dir:
        refuse("MANIFEST_EVIDENCE_DIR_NOT_PINNED", value=manifest["evidence_dir"])
    if "registration" in manifest and manifest["registration"] != REGISTRATION_TEMPLATE:
        refuse("MANIFEST_REGISTRATION_NOT_TEMPLATE")
    require_contained(layout.source, layout.source_dir, "file", "SOURCE_NOT_CONTAINED", must_exist=True)
    source_sha = sha256_bytes(read_regular(layout.source, "SOURCE_NOT_CONTAINED"))
    if source_sha != manifest["source_sha256"]:
        refuse("SOURCE_HASH_MISMATCH", actual_source_sha256=source_sha)
    require_real_dir(layout.hooks_dir, "HOOKS_DIR_NOT_REAL_DIRECTORY")
    if os.path.lexists(layout.runtime_parent):
        require_real_dir(layout.runtime_parent, "RUNTIME_PARENT_NOT_REAL_DIRECTORY")
    require_real_dir(layout.settings_dir, "SETTINGS_DIR_NOT_REAL_DIRECTORY")
    require_contained(layout.evidence_dir, layout.evidence_root, "dir", "EVIDENCE_DIR_NOT_REAL_DIRECTORY",
                      must_exist=True)
    require_real_dir(layout.evidence_dir, "EVIDENCE_DIR_NOT_REAL_DIRECTORY")
    if not os.access(layout.evidence_dir, os.W_OK):
        refuse("EVIDENCE_DIR_NOT_WRITABLE")
    receipt["evidence_verified"] = True
    return manifest


# --- settings ----------------------------------------------------------------------------

def read_settings(layout):
    """Returns (bytes, object, stat) or (None, None, None) when the file is absent."""
    require_real_dir(layout.settings_dir, "SETTINGS_DIR_NOT_REAL_DIRECTORY")
    try:
        info = os.lstat(layout.settings)
    except FileNotFoundError:
        return None, None, None
    if stat.S_ISLNK(info.st_mode):
        refuse("SETTINGS_SYMLINK")
    if stat.S_ISDIR(info.st_mode):
        refuse("SETTINGS_DIRECTORY")
    if not stat.S_ISREG(info.st_mode):
        refuse("SETTINGS_NOT_REGULAR")
    data = read_regular(layout.settings, "SETTINGS_NOT_REGULAR")
    try:
        obj = loads_strict(data)
    except (ValueError, UnicodeDecodeError) as exc:
        refuse("SETTINGS_MALFORMED", detail=str(exc))
    if not isinstance(obj, dict):
        refuse("SETTINGS_NOT_OBJECT")
    return data, obj, info


def permission_request_entries(obj):
    hooks = obj.get("hooks")
    if hooks is None:
        return []
    if not isinstance(hooks, dict):
        refuse("SETTINGS_HOOKS_NOT_OBJECT")
    entries = hooks.get("PermissionRequest")
    if entries is None:
        return []
    if not isinstance(entries, list):
        refuse("SETTINGS_PERMISSION_REQUEST_NOT_LIST")
    return entries


def registration_for(runtime_path, sha):
    # SHA first, then RUNTIME: a hex digest can never contain "RUNTIME", a path might contain "SHA"
    command = TEMPLATE_COMMAND.replace("SHA", sha).replace("RUNTIME", runtime_path)
    if command.count(runtime_path) != 1 or command.count(sha) != 1:
        refuse("REGISTRATION_SUBSTITUTION_AMBIGUOUS", command=command)
    entry = {"matcher": "Bash", "hooks": [{"type": "command", "command": command, "timeout": 10}]}
    return entry, command


def entry_command(entry):
    hooks = entry.get("hooks") if isinstance(entry, dict) else None
    if isinstance(hooks, list) and hooks and isinstance(hooks[0], dict):
        return hooks[0].get("command")
    return None


def current_settings_sha(layout):
    try:
        info = os.lstat(layout.settings)
    except FileNotFoundError:
        return None
    if not stat.S_ISREG(info.st_mode):
        return None
    return sha256_bytes(read_regular(layout.settings, "SETTINGS_NOT_REGULAR"))


def recheck_preimage(layout, preimage_bytes):
    current = current_settings_sha(layout)
    expected = None if preimage_bytes is None else sha256_bytes(preimage_bytes)
    if current != expected:
        refuse("DRIFT_BEFORE_WRITE", EXIT_DRIFT_BEFORE_WRITE, settings_sha256=current,
               expected_settings_sha256=expected)


# --- runtime directory --------------------------------------------------------------------

def runtime_entries(layout, hash12):
    """(runtime dir exists, list of aborted archives) for one hash; RUNTIME_PARENT verified when present."""
    if not os.path.lexists(layout.runtime_parent):
        return False, []
    require_real_dir(layout.runtime_parent, "RUNTIME_PARENT_NOT_REAL_DIRECTORY")
    try:
        names = os.listdir(layout.runtime_parent)
    except OSError as exc:  # a read failure, never reported as a write failure (F7)
        refuse("RUNTIME_PARENT_UNREADABLE", detail=str(exc))
    aborted = sorted(n for n in names if n.startswith(hash12 + ".aborted-"))
    return hash12 in names, aborted


def require_intact_runtime(layout, runtime_dir, runtime_path, sha):
    """IDEMPOTENT only when the registered runtime file exists as a regular, non-symlink 0444 file with
    the registered SHA-256; otherwise RUNTIME_DRIFT (exit 4), nothing written."""
    try:
        require_real_dir(layout.runtime_parent, "RUNTIME_DRIFT")
        require_contained(runtime_dir, layout.runtime_parent, "dir", "RUNTIME_DRIFT", must_exist=True)
        require_real_dir(runtime_dir, "RUNTIME_DRIFT")
        require_contained(runtime_path, runtime_dir, "file", "RUNTIME_DRIFT", must_exist=True)
        info = os.lstat(runtime_path)
        intact = (stat.S_IMODE(info.st_mode) == 0o444
                  and sha256_bytes(read_regular(runtime_path, "RUNTIME_DRIFT")) == sha)
    except (OSError, Refusal):
        intact = False
    if not intact:
        refuse("RUNTIME_DRIFT", EXIT_STATE_DRIFT, runtime_path=runtime_path)


def create_runtime_copy(layout, sha, receipt, seams=None):
    """Create RUNTIME_PARENT (plain mkdir, only below a real HOOKS_DIR) and RUNTIME_PARENT/<HASH12>,
    copy the source there (0444) and verify the copy. Ownership of <HASH12> is recorded in the receipt
    the moment its mkdir returns, so every later failure archives it (invariant A)."""
    if not os.path.lexists(layout.runtime_parent):
        require_real_dir(layout.hooks_dir, "HOOKS_DIR_NOT_REAL_DIRECTORY")
        os.mkdir(layout.runtime_parent)
        receipt["runtime_parent_created"] = True
    require_real_dir(layout.runtime_parent, "RUNTIME_PARENT_NOT_REAL_DIRECTORY")
    runtime_dir = layout.runtime_parent + "/" + sha[:12]
    require_contained(runtime_dir, layout.runtime_parent, "dir", "RUNTIME_DIR_NOT_CONTAINED")
    if os.path.lexists(runtime_dir):
        refuse("RUNTIME_DIR_EXISTS", EXIT_RUNTIME_EXISTS, runtime_dir=runtime_dir)
    receipt["step"] = "runtime copy"
    with interrupts_blocked():  # the mkdir and its ownership record are one step for SIGINT/SIGTERM (A)
        os.mkdir(runtime_dir)
        receipt["runtime_dir_owned"] = runtime_dir
    receipt["runtime_copy_created"] = True
    require_real_dir(runtime_dir, "RUNTIME_DIR_NOT_CONTAINED")
    data = read_regular(layout.source, "SOURCE_NOT_CONTAINED")
    if sha256_bytes(data) != sha:
        refuse("SOURCE_HASH_MISMATCH", EXIT_WRITE_FAILED, actual_source_sha256=sha256_bytes(data))
    try:
        inject_fault(seams, "runtime-copy-write")
        runtime_path = write_new_file(runtime_dir + "/" + GUARD_FILENAME, runtime_dir, data, 0o600)
        os.chmod(runtime_path, 0o444)
    except OSError as exc:
        refuse("RUNTIME_COPY_FAILED", EXIT_WRITE_FAILED, detail=str(exc))
    copied = sha256_bytes(read_regular(runtime_path, "RUNTIME_COPY_UNREADABLE"))
    if copied != sha:
        refuse("RUNTIME_COPY_HASH_MISMATCH", EXIT_WRITE_FAILED, runtime_dir=runtime_dir, runtime_sha256=copied)
    receipt["runtime_sha256"] = sha
    return runtime_dir


def archive_failed_runtime(layout, receipt):
    """Invariant A: a failure before the settings replacement returned renames the runtime dir this run
    created to <HASH12>.aborted-<UTC>-<pid> exactly once (never deleted, never overwritten). After the
    replacement (settings_written) the runtime dir is never touched (F3)."""
    runtime_dir = receipt.get("runtime_dir_owned")
    if not runtime_dir or receipt.get("settings_written") or "runtime_renamed_to" in receipt:
        return
    receipt["failed_step"] = receipt.get("step")
    archive = f"{runtime_dir}.aborted-{utc_stamp()}-{os.getpid()}"
    try:
        rename_contained(runtime_dir, archive, layout.runtime_parent, "dir")
        receipt["runtime_renamed_to"] = archive
    except Exception as exc:  # report, never hide: the orphan is then left for the owner (INCOMPLETE_INSTALL)
        receipt["runtime_renamed_to"] = None
        receipt["runtime_archive_error"] = f"{type(exc).__name__}: {exc}"


def record_commit(receipt, flag, data):
    """Invariant W: the settings write is recorded the moment its os.replace has returned."""
    def on_replaced(temp_path):
        receipt.update({flag: True, "temp_path": temp_path, "intended_postimage_sha256": sha256_bytes(data),
                        "postimage_sha256": None, "step": STEP_AFTER_REPLACE})
    return on_replaced


def read_postimage(layout, receipt, seams):
    """The read-back after a recorded write; a failure is exit 7, never a refusal of input (W)."""
    try:
        inject_fault(seams, "postimage-read")
        post = current_settings_sha(layout)
    except (OSError, Refusal) as exc:
        receipt["postimage_read_error"] = getattr(exc, "reason", None) or f"{type(exc).__name__}: {exc}"
        refuse("POSTIMAGE_UNREADABLE", EXIT_WRITE_FAILED)
    receipt["postimage_sha256"] = post
    return post


def fail_after_write(seams):
    if seams.get("fail_after_write"):
        raise OSError("TEST_FAIL_AFTER_WRITE")


# --- state and receipts ---------------------------------------------------------------------

def load_state(layout, seams=None):
    """The latest state-changing receipt; every path in it must equal the pinned derivation. Only a
    missing file means "no state" (STATE_MISSING); any other read error is STATE_UNREADABLE (F7). A
    state that records a committed write (settings_written / settings_restored) with a null postimage
    is returned with postimage_unknown set (W): callers never act on an assumed hash."""
    path = layout.state_path
    try:
        inject_fault(seams, "state-read")
        info = os.lstat(path)
    except FileNotFoundError as exc:
        refuse("STATE_MISSING", detail=str(exc))
    except OSError as exc:
        refuse("STATE_UNREADABLE", detail=str(exc))
    if not stat.S_ISREG(info.st_mode):
        refuse("STATE_NOT_REGULAR")
    data = read_regular(path, "STATE_UNREADABLE")
    try:
        state = loads_strict(data)
    except (ValueError, UnicodeDecodeError) as exc:
        refuse("STATE_MALFORMED", detail=str(exc))
    if not isinstance(state, dict):
        refuse("STATE_MALFORMED")
    committed = state.get("settings_written") is True or state.get("settings_restored") is True
    installed = state.get("result") in ("INSTALLED", "PROMOTED") or (state.get("result") == "FAILED" and committed)
    if not installed:
        refuse("STATE_NOT_INSTALLED", state_result=state.get("result"))
    if state.get("settings_target") != layout.settings:
        refuse("STATE_SETTINGS_TARGET_MISMATCH")
    if not is_hex64(state.get("runtime_sha256")):
        refuse("STATE_HASH_MALFORMED", key="runtime_sha256")
    if state.get("postimage_sha256") is None and committed and state.get("result") == "FAILED":
        state["postimage_unknown"] = True
    elif not is_hex64(state.get("postimage_sha256")):
        refuse("STATE_HASH_MALFORMED", key="postimage_sha256")
    intended = state.get("intended_postimage_sha256")
    if committed and is_hex64(intended) and state.get("postimage_sha256") != intended:
        # read back but not the bytes this run wrote (POSTIMAGE_MISMATCH, a concurrent writer): unverified (W, 11.3)
        state["postimage_unknown"] = True
    expected_dir = layout.runtime_parent + "/" + state["runtime_sha256"][:12]
    if state.get("runtime_dir") != expected_dir or state.get("runtime_path") != expected_dir + "/" + GUARD_FILENAME:
        refuse("STATE_RUNTIME_NOT_PINNED")
    if not isinstance(state.get("registration_command"), str) or state["registration_command"] == "":
        refuse("STATE_REGISTRATION_MISSING")
    preimage = state.get("preimage")
    if not isinstance(preimage, dict) or preimage.get("status") not in ("ABSENT", "PRESENT"):
        refuse("STATE_PREIMAGE_MALFORMED")
    if preimage["status"] == "PRESENT":
        backup = preimage.get("backup_path")
        if (not path_syntax_ok(backup) or os.path.dirname(backup) != layout.evidence_dir
                or not os.path.basename(backup).startswith(PREIMAGE_PREFIX) or not is_hex64(preimage.get("sha256"))):
            refuse("STATE_PREIMAGE_NOT_PINNED")
        require_contained(backup, layout.evidence_dir, "file", "STATE_PREIMAGE_NOT_PINNED", must_exist=True)
    return state


def save_receipt(layout, receipt, update_state, seams=None):
    """Outcome S: after a completed write the state file is saved FIRST; a receipt with a success result
    is persisted only after that save succeeded. A failed state save turns the result into
    <RESULT>_STATE_NOT_SAVED (exit 7, state_saved false, original_result / original_reason kept); a
    receipt that cannot be written says receipt_saved false (exit 7, except that a refusal keeps its
    own code). Never raises."""
    if not receipt.get("evidence_verified"):
        return  # a refusal before the evidence directory is verified is printed only
    if update_state:
        progress = {}
        try:
            inject_fault(seams, "state-save")
            write_atomic(layout.state_path, layout.evidence_dir, dumps_record(dict(receipt, state_saved=True)).encode("ascii"),
                         0o600, progress=progress)
            receipt["state_saved"] = True
            if "dir_fsync_error" in progress:  # F5 for the state write too
                receipt["state_dir_fsync_error"] = progress["dir_fsync_error"]
        except Exception as exc:  # outcome S: a failed state save is an outcome, never a crash
            temp = (getattr(exc, "extra", None) or {}).get("temp_path") or progress.get("temp_path")
            if temp:
                receipt["state_save_temp_path"] = temp  # F6 for the state write: the leftover temp file is named
            receipt.update(state_saved=False, state_save_error=getattr(exc, "reason", None) or f"{type(exc).__name__}: {exc}",
                           original_result=receipt.get("result"), original_reason=receipt.get("reason"),
                           result=f"{receipt.get('result')}_STATE_NOT_SAVED", exit_code=EXIT_WRITE_FAILED)
    try:
        receipts = layout.evidence_dir + "/" + RECEIPTS_DIR
        if not os.path.lexists(receipts):
            require_contained(receipts, layout.evidence_dir, "dir", "RECEIPTS_DIR_NOT_CONTAINED")
            os.mkdir(receipts)
        require_contained(receipts, layout.evidence_dir, "dir", "RECEIPTS_DIR_NOT_CONTAINED", must_exist=True)
        require_real_dir(receipts, "RECEIPTS_DIR_NOT_CONTAINED")
        inject_fault(seams, "receipt-save")
        name = f"{receipt['timestamp_utc']}-{receipt['mode']}-{os.getpid()}.json"
        write_new_file(receipts + "/" + name, receipts, dumps_record(dict(receipt, receipt_saved=True)).encode("ascii"))
        receipt["receipt_saved"] = True
    except Exception as exc:  # never raises: the printed receipt says receipt_saved false
        receipt.update(receipt_saved=False,
                       receipt_save_error=getattr(exc, "reason", None) or f"{type(exc).__name__}: {exc}")
        if receipt.get("exit_code") in (None, EXIT_OK) or update_state:
            receipt["exit_code"] = EXIT_WRITE_FAILED


# --- modes ----------------------------------------------------------------------------

def do_install(layout, manifest, receipt, seams):
    sha = manifest["source_sha256"]
    runtime_dir = layout.runtime_parent + "/" + sha[:12]
    runtime_path = runtime_dir + "/" + GUARD_FILENAME
    entry, command = registration_for(runtime_path, sha)
    receipt.update(runtime_dir=runtime_dir, runtime_path=runtime_path, registration_command=command)

    receipt["step"] = "preimage"
    pre_bytes, pre_obj, pre_info = read_settings(layout)
    existing = permission_request_entries(pre_obj or {})
    if existing:
        if existing == [entry]:
            require_intact_runtime(layout, runtime_dir, runtime_path, sha)
            receipt.update(result="IDEMPOTENT", exit_code=EXIT_OK, hooks=pre_obj.get("hooks"),
                           postimage_sha256=sha256_bytes(pre_bytes))
            return EXIT_OK
        refuse("FOREIGN_HOOK", EXIT_FOREIGN_HOOK, existing_entries=existing)
    exists, aborted = runtime_entries(layout, sha[:12])
    if exists:
        refuse("RUNTIME_DIR_EXISTS", EXIT_RUNTIME_EXISTS, runtime_dir=runtime_dir)
    if aborted:  # installer-owned archives of handled failures never block a later install (13.2 (e))
        receipt["aborted_archives_present"] = aborted

    if pre_bytes is None:
        receipt["preimage"] = {"status": "ABSENT"}
        mode = 0o644
    else:
        backup = f"{layout.evidence_dir}/{PREIMAGE_PREFIX}{receipt['timestamp_utc']}-{os.getpid()}"
        write_new_file(backup, layout.evidence_dir, pre_bytes)
        mode = stat.S_IMODE(pre_info.st_mode)
        receipt["preimage"] = {"status": "PRESENT", "sha256": sha256_bytes(pre_bytes), "size": pre_info.st_size,
                               "mode": oct(mode), "mtime": pre_info.st_mtime, "backup_path": backup}
    if seams.get("barrier"):
        receipt["test_barrier"] = seams["barrier"]
        wait_at_barrier(seams["barrier"])

    # build and validate the new document before anything is created on disk
    new_obj = json.loads(json.dumps(pre_obj)) if pre_obj is not None else {}
    hooks = new_obj.setdefault("hooks", {})
    if not isinstance(hooks, dict):
        refuse("SETTINGS_HOOKS_NOT_OBJECT")
    hooks.setdefault("PermissionRequest", []).append(entry)
    data = dumps(new_obj).encode("utf-8")
    for name in ("crash_before_replace", "short_write", "fail_after_write"):
        if seams.get(name):
            receipt["test_" + name] = True

    receipt["hooks"] = new_obj["hooks"]
    recheck_preimage(layout, pre_bytes)
    try:
        create_runtime_copy(layout, sha, receipt, seams)
        receipt["step"] = "settings replacement"
        recheck_preimage(layout, pre_bytes)
        write_atomic(layout.settings, layout.settings_dir, data, mode, seams, progress=receipt,
                     on_replaced=record_commit(receipt, "settings_written", data))
    except BaseException:
        archive_failed_runtime(layout, receipt)  # pre-commit only (A); a no-op once settings_written (F3)
        raise
    if read_postimage(layout, receipt, seams) != sha256_bytes(data):
        refuse("POSTIMAGE_MISMATCH", EXIT_WRITE_FAILED, expected_postimage_sha256=sha256_bytes(data))
    fail_after_write(seams)
    receipt.update(result="INSTALLED", exit_code=EXIT_OK)
    return EXIT_OK


def registration_present(layout, sha, receipt):
    """Whether the settings target holds this hash's registration (read-only; a read error is recorded)."""
    runtime_path = layout.runtime_parent + "/" + sha[:12] + "/" + GUARD_FILENAME
    _, command = registration_for(runtime_path, sha)
    try:
        _, obj, _ = read_settings(layout)
    except Refusal as refusal:
        receipt["registration_error"] = refusal.reason
        return None
    return obj is not None and any(entry_command(e) == command for e in permission_request_entries(obj))


def do_verify(layout, manifest, receipt, seams=None):
    """Precedence (11.3, binding; the first matching step decides): (1) a state file that exists but
    cannot be read: STATE_UNREADABLE; (2) a readable state that records a committed write: MATCH when
    the settings target, the runtime file and the registration match it, otherwise DRIFT (always DRIFT
    when its postimage is unknown, W); (3) no trustworthy state but a registration, runtime dir or
    .aborted- archive for HASH12: INCOMPLETE_INSTALL; (4) NOT_INSTALLED. Writes nothing."""
    sha = manifest["source_sha256"]
    untrusted = None
    try:
        state = load_state(layout, seams)
    except Refusal as refusal:
        if refusal.reason in ("STATE_UNREADABLE", "STATE_NOT_REGULAR", "STATE_MALFORMED"):  # step 1
            receipt.update(result="STATE_UNREADABLE", exit_code=EXIT_VERIFY_DRIFT, state_refusal=refusal.reason,
                           detail=refusal.extra.get("detail"))
            return EXIT_VERIFY_DRIFT
        if refusal.reason not in ("STATE_MISSING", "STATE_NOT_INSTALLED"):
            untrusted = refusal.reason  # readable, but its paths or hashes are not trustworthy: steps 3 and 4
        state = None
    if state is None:
        exists, aborted = runtime_entries(layout, sha[:12])
        registered = registration_present(layout, sha, receipt)
        if untrusted:
            receipt["state_refusal"] = untrusted
        if exists or aborted or registered:
            receipt.update(result="INCOMPLETE_INSTALL", exit_code=EXIT_VERIFY_DRIFT, runtime_dir_present=exists,
                           aborted=aborted, registration_present=registered, state_present=untrusted is not None)
            return EXIT_VERIFY_DRIFT
        receipt.update(result="NOT_INSTALLED", exit_code=EXIT_VERIFY_DRIFT)
        return EXIT_VERIFY_DRIFT
    try:  # step 2: a read error is DRIFT, never a refusal
        settings_sha = current_settings_sha(layout)
    except (OSError, Refusal) as exc:
        settings_sha = None
        receipt["settings_read_error"] = getattr(exc, "reason", None) or f"{type(exc).__name__}: {exc}"
    runtime_path = state["runtime_path"]
    runtime_sha = None
    try:
        if os.path.lexists(runtime_path):
            require_contained(runtime_path, state["runtime_dir"], "file", "STATE_RUNTIME_NOT_PINNED", must_exist=True)
            runtime_sha = sha256_bytes(read_regular(runtime_path, "STATE_RUNTIME_NOT_PINNED"))
    except (OSError, Refusal) as exc:
        runtime_sha = None
        receipt["runtime_read_error"] = getattr(exc, "reason", None) or f"{type(exc).__name__}: {exc}"
    registration_count = None
    try:
        _, obj, _ = read_settings(layout)
        if obj is not None:
            registration_count = sum(1 for e in permission_request_entries(obj)
                                     if entry_command(e) == state["registration_command"])
    except Refusal as refusal:
        receipt["registration_error"] = refusal.reason
    receipt.update(settings_sha256=settings_sha, expected_settings_sha256=state["postimage_sha256"],
                   intended_postimage_sha256=state.get("intended_postimage_sha256"),
                   postimage_unknown=bool(state.get("postimage_unknown")),
                   runtime_path=runtime_path, runtime_sha256=runtime_sha,
                   expected_runtime_sha256=state["runtime_sha256"],
                   registration_command=state["registration_command"], registration_count=registration_count,
                   state_result=state.get("result"), state_reason=state.get("reason"))
    if (not state.get("postimage_unknown") and settings_sha == state["postimage_sha256"]
            and runtime_sha == state["runtime_sha256"] and registration_count == 1):
        receipt.update(result="MATCH", exit_code=EXIT_OK)
        return EXIT_OK
    receipt.update(result="DRIFT", exit_code=EXIT_VERIFY_DRIFT)
    return EXIT_VERIFY_DRIFT


def refuse_unknown_postimage(state):
    """W: a state whose committed write has no verified postimage is never acted on."""
    if state.get("postimage_unknown"):
        refuse("POSTIMAGE_UNKNOWN", EXIT_STATE_DRIFT, intended_postimage_sha256=state.get("intended_postimage_sha256"),
               state_reason=state.get("reason"))


def do_rollback(layout, manifest, receipt, seams):
    state = load_state(layout, seams)
    receipt.update(runtime_dir=state["runtime_dir"], runtime_path=state["runtime_path"],
                   runtime_sha256=state["runtime_sha256"], registration_command=state["registration_command"],
                   preimage=state["preimage"], expected_settings_sha256=state["postimage_sha256"])
    refuse_unknown_postimage(state)
    current = current_settings_sha(layout)
    receipt["settings_sha256"] = current
    if current != state["postimage_sha256"]:
        refuse("SETTINGS_DRIFT", EXIT_STATE_DRIFT, settings_sha256=current,
               expected_settings_sha256=state["postimage_sha256"])
    preimage = state["preimage"]
    stamp = receipt["timestamp_utc"]
    receipt["step"] = "settings restore"
    if preimage["status"] == "ABSENT":
        with interrupts_blocked():  # W: the rename and its record are one step; nothing is read back after a rename
            renamed = rename_contained(layout.settings, f"{layout.settings}.rolled-back-{stamp}", layout.settings_dir,
                                       "file")
            receipt.update(settings_restored=True, settings_renamed_to=renamed, postimage_sha256=None,
                           step=STEP_AFTER_REPLACE)
    else:
        data = read_regular(preimage["backup_path"], "STATE_PREIMAGE_NOT_PINNED")
        if sha256_bytes(data) != preimage["sha256"]:
            refuse("PREIMAGE_BACKUP_CORRUPT", backup_path=preimage["backup_path"])
        mode = int(preimage.get("mode", "0o644"), 8)
        write_atomic(layout.settings, layout.settings_dir, data, mode, seams, progress=receipt,
                     on_replaced=record_commit(receipt, "settings_restored", data))
        if read_postimage(layout, receipt, seams) != preimage["sha256"]:
            refuse("POSTIMAGE_MISMATCH", EXIT_WRITE_FAILED, expected_postimage_sha256=preimage["sha256"])
    fail_after_write(seams)
    runtime_dir = state["runtime_dir"]
    if os.path.lexists(runtime_dir):
        receipt["runtime_renamed_to"] = rename_contained(runtime_dir, f"{runtime_dir}.disabled-{stamp}",
                                                         layout.runtime_parent, "dir")
    receipt.update(result="ROLLED_BACK", exit_code=EXIT_OK, previous_receipt=state)
    return EXIT_OK


def do_promote(layout, manifest, receipt, seams):
    state = load_state(layout, seams)
    refuse_unknown_postimage(state)
    sha = manifest["source_sha256"]
    if sha == state["runtime_sha256"]:
        refuse("SAME_HASH_ALREADY_INSTALLED", EXIT_RUNTIME_EXISTS, runtime_sha256=sha)
    pre_bytes, pre_obj, pre_info = read_settings(layout)
    current = None if pre_bytes is None else sha256_bytes(pre_bytes)
    receipt.update(settings_sha256=current, expected_settings_sha256=state["postimage_sha256"],
                   preimage=state["preimage"])
    if pre_bytes is None or current != state["postimage_sha256"]:
        refuse("SETTINGS_DRIFT", EXIT_STATE_DRIFT, settings_sha256=current,
               expected_settings_sha256=state["postimage_sha256"])
    entries = permission_request_entries(pre_obj)
    matches = [i for i, e in enumerate(entries) if entry_command(e) == state["registration_command"]]
    if len(matches) != 1:
        refuse("REGISTRATION_NOT_UNIQUE", EXIT_STATE_DRIFT, matches=len(matches))
    exists, aborted = runtime_entries(layout, sha[:12])
    if exists:
        refuse("RUNTIME_DIR_EXISTS", EXIT_RUNTIME_EXISTS)
    if aborted:  # installer-owned archives never block (13.2 (e))
        receipt["aborted_archives_present"] = aborted
    new_runtime_dir = layout.runtime_parent + "/" + sha[:12]
    new_runtime_path = new_runtime_dir + "/" + GUARD_FILENAME
    _, new_command = registration_for(new_runtime_path, sha)
    receipt.update(runtime_dir=new_runtime_dir, runtime_path=new_runtime_path, registration_command=new_command)
    new_obj = json.loads(json.dumps(pre_obj))
    new_obj["hooks"]["PermissionRequest"][matches[0]]["hooks"][0]["command"] = new_command
    data = dumps(new_obj).encode("utf-8")
    receipt["hooks"] = new_obj["hooks"]
    recheck_preimage(layout, pre_bytes)
    if seams.get("barrier"):
        receipt["test_barrier"] = seams["barrier"]
        wait_at_barrier(seams["barrier"])
    try:
        create_runtime_copy(layout, sha, receipt, seams)
        receipt["step"] = "settings replacement"
        recheck_preimage(layout, pre_bytes)
        write_atomic(layout.settings, layout.settings_dir, data, stat.S_IMODE(pre_info.st_mode), seams, progress=receipt,
                     on_replaced=record_commit(receipt, "settings_written", data))
    except BaseException:
        archive_failed_runtime(layout, receipt)  # pre-commit only (A); a no-op once settings_written (F3)
        raise
    if read_postimage(layout, receipt, seams) != sha256_bytes(data):
        refuse("POSTIMAGE_MISMATCH", EXIT_WRITE_FAILED, expected_postimage_sha256=sha256_bytes(data))
    fail_after_write(seams)
    old_runtime_dir = state["runtime_dir"]
    if os.path.lexists(old_runtime_dir):
        receipt["previous_runtime_renamed_to"] = rename_contained(
            old_runtime_dir, f"{old_runtime_dir}.superseded-{receipt['timestamp_utc']}", layout.runtime_parent, "dir")
    receipt.update(result="PROMOTED", exit_code=EXIT_OK, previous_receipt=state)
    return EXIT_OK


def wait_at_barrier(barrier_dir):
    write_new_file(barrier_dir + "/reached", barrier_dir, (utc_stamp() + "\n").encode())
    deadline = time.monotonic() + BARRIER_TIMEOUT_SECONDS
    while not os.path.exists(barrier_dir + "/release"):
        if time.monotonic() > deadline:
            refuse("TEST_BARRIER_TIMEOUT", EXIT_WRITE_FAILED)
        time.sleep(0.05)


# --- entry point ------------------------------------------------------------------------

def record_late_interrupt(signum, frame):
    LATE_INTERRUPTS.append(signal.Signals(signum).name)


def raise_on_interrupt(signum, frame):
    """The first SIGINT/SIGTERM of the transaction raises Interrupted once; the handler is switched to recording first,
    so a second signal during the unwinding (an archive, main's handlers) is recorded, never raised (F3)."""
    set_interrupt_handlers(record_late_interrupt)
    raise Interrupted(signal.Signals(signum).name)


def set_interrupt_handlers(handler):
    for sig in INTERRUPT_SIGNALS:
        signal.signal(sig, handler)


def main():
    parser = argparse.ArgumentParser(description="PermissionRequest guard installer (owner-run)")
    parser.add_argument("mode", choices=("install", "verify", "rollback", "promote"))
    parser.add_argument("--manifest", default=None)
    parser.add_argument("--expect-sha256", default=None, help="SHA-256 of this installer file")
    parser.add_argument("--expect-manifest-sha256", default=None, help="SHA-256 of the frozen manifest")
    parser.add_argument("--test-root", default=None, help="tests only: rebase every pinned destination under DIR")
    parser.add_argument("--test-barrier", default=None, help="test seam (with --test-root)")
    parser.add_argument("--test-crash-before-replace", action="store_true", help="test seam (with --test-root)")
    parser.add_argument("--test-short-write", action="store_true", help="test seam (with --test-root)")
    parser.add_argument("--test-fail-after-write", action="store_true", help="test seam (with --test-root)")
    parser.add_argument("--test-fault", action="append", default=[], metavar="POINT",
                        help="test seam (with --test-root): one of " + ", ".join(TEST_FAULT_POINTS))
    opts = parser.parse_args()
    installer_path = os.path.abspath(__file__)
    receipt = {"mode": opts.mode, "timestamp_utc": utc_stamp(), "installer": installer_path,
               "manifest": opts.manifest}
    seams = {"barrier": opts.test_barrier, "crash_before_replace": opts.test_crash_before_replace,
             "short_write": opts.test_short_write, "fail_after_write": opts.test_fail_after_write,
             "faults": tuple(opts.test_fault)}
    layout = None
    try:
        try:
            set_interrupt_handlers(raise_on_interrupt)  # SIGTERM/SIGINT become Interrupted for the transaction (F3)
            test_root = validate_test_root(opts.test_root)
            if test_root is None and any(seams.values()):
                refuse("TEST_OPTION_WITHOUT_TEST_ROOT")
            unknown = [p for p in seams["faults"] if p not in TEST_FAULT_POINTS]
            if unknown:
                refuse("TEST_FAULT_UNKNOWN", points=unknown)
            if test_root is not None:
                receipt["test_root"] = test_root
                if seams["faults"]:
                    receipt["test_faults"] = list(seams["faults"])
                if seams["barrier"] is not None:
                    require_contained(seams["barrier"], test_root, "dir", "TEST_BARRIER_NOT_CONTAINED", must_exist=True)
            layout = Layout(test_root)
            if not is_hex64(opts.expect_sha256):
                refuse("INSTALLER_HASH_MISSING")
            own_sha = sha256_bytes(read_regular(installer_path, "INSTALLER_UNREADABLE"))
            if own_sha != opts.expect_sha256:
                refuse("INSTALLER_HASH_MISMATCH", actual_installer_sha256=own_sha)
            receipt["installer_sha256_verified"] = own_sha
            manifest = load_manifest(layout, opts.mode, opts.manifest, opts.expect_manifest_sha256, receipt)
            receipt["settings_target"] = layout.settings
            receipt["evidence_dir"] = layout.evidence_dir
            install_manifest_sha = (receipt["manifest_sha256_verified"] if opts.mode != "promote"
                                    else (sha256_bytes(read_regular(layout.install_manifest, "MANIFEST_MISSING"))
                                          if os.path.isfile(layout.install_manifest) else None))
            receipt["rollback_command"] = (
                f"python3 -I {installer_path} rollback --manifest {layout.install_manifest} --expect-sha256 {own_sha} "
                f"--expect-manifest-sha256 {install_manifest_sha}" + (f" --test-root {test_root}" if test_root else ""))
            if opts.mode == "install":
                do_install(layout, manifest, receipt, seams)
            elif opts.mode == "verify":
                do_verify(layout, manifest, receipt, seams)
            elif opts.mode == "rollback":
                do_rollback(layout, manifest, receipt, seams)
            else:
                do_promote(layout, manifest, receipt, seams)
        finally:
            set_interrupt_handlers(record_late_interrupt)  # before any handler below runs: from here only recorded
    except Refusal as refusal:
        code = refusal.code
        if (receipt.get("settings_written") or receipt.get("settings_restored")) and code != EXIT_WRITE_FAILED:
            receipt["original_code"] = code  # a failure after a committed write is exit 7, never "refused input" (W)
            code = EXIT_WRITE_FAILED
        receipt.update(refusal.extra)
        receipt.update(result="FAILED" if code == EXIT_WRITE_FAILED else "REFUSED", reason=refusal.reason,
                       exit_code=code)
    except BaseException as exc:  # unexpected error, KeyboardInterrupt, SystemExit, SIGTERM: never a success
        interrupted = not isinstance(exc, Exception)
        receipt.update(result="FAILED", reason="INTERRUPTED" if interrupted else "UNEXPECTED_ERROR",
                       exit_code=EXIT_WRITE_FAILED, detail=f"{type(exc).__name__}: {exc}")
        if interrupted:
            receipt["interrupted"] = True
    if receipt.get("exit_code") == EXIT_WRITE_FAILED and "step" in receipt:
        receipt.setdefault("failed_step", receipt["step"])
    if LATE_INTERRUPTS:
        receipt["late_interrupt"] = list(LATE_INTERRUPTS)
    changed = bool(receipt.get("settings_written") or receipt.get("settings_restored"))
    if layout is not None and opts.mode != "verify":
        save_receipt(layout, receipt, update_state=changed, seams=seams)  # outcome S; never raises
    if LATE_INTERRUPTS:
        receipt["late_interrupt"] = list(LATE_INTERRUPTS)
        if receipt.get("exit_code") in (None, EXIT_OK):
            receipt["exit_code"] = EXIT_WRITE_FAILED  # a recorded interrupt never ends in exit 0
    try:
        sys.stdout.write(json.dumps(receipt, ensure_ascii=True, default=str) + "\n")
        sys.stdout.flush()
    except Exception:
        pass
    return receipt.get("exit_code", EXIT_WRITE_FAILED)


if __name__ == "__main__":
    sys.exit(main())
