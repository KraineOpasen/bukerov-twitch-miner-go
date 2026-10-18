#!/usr/bin/env python3
"""Owner-run installer of the prompt observer (guard-g1-contract-rev3 3.6, repaired by
guard-g1-repair-rev6 3.2; README-prompt-observer.md).

    python3 -I install_prompt_observer.py install --expect-sha256 SELF --source-sha256 OBSERVER
    python3 -I install_prompt_observer.py status --expect-sha256 SELF
    python3 -I install_prompt_observer.py uninstall --expect-sha256 SELF
    python3 -I install_prompt_observer.py remove-entry --expect-sha256 SELF

SELF is this file's SHA-256, checked before anything else is read; OBSERVER is the SHA-256 of
.claude/hooks/prompt_observer.py in the clone. `install` copies those exact bytes to the immutable
runtime copy RUNTIME = /home/claude/.claude/hooks/prompt-observer/<HASH12>/prompt_observer.py
(mode 0444) and appends ONE Notification entry to the git-ignored .claude/settings.local.json:

    {"matcher": "permission_prompt", "hooks": [{"type": "command",
      "command": "python3 -I RUNTIME --expect-sha256 OBSERVER", "timeout": 10}]}

Every mode first walks the directory chain from "/" to the hooks root and the observer root (each
component a real directory with a safe owner and mode; the observer root exactly 0700) and a
writing mode refuses an unsafe chain before it writes anything. The writing modes then hold the
advisory lock HOOKS_ROOT/.prompt-observer-installer.lock (flock) for exactly one invocation.
prompt-observer/state.json is a journal ("pending_install", "installed", "pending_uninstall"),
validated before use and written BEFORE every change it announces. Every settings replacement
re-reads the settings file immediately before the rename and, when it changed, aborts with
DRIFT_BEFORE_WRITE, restoring the journal and leaving the settings file as it found it.
`uninstall` restores the exact preimage bytes and mode while the settings file is byte-identical
to the installed postimage; after a foreign change it refuses and `remove-entry` removes exactly
the observer entry (the file is then re-serialised), or only finishes when the entry is already
gone. `status` is read-only and prints the next_step of the README's REMOVAL and INSTALL
PROTOCOL. The only deletion is of this run's own temporary files: runtime copies and journals are
renamed .aborted-, .disabled-, .orphaned-, .uninstalled-. Every run prints one JSON line with
"result" and "next_step"; receipts print counts per hook event, never another hook command or any
settings value. `--test-root DIR` (tests only) maps the clone to DIR/project and the hooks root to
DIR/home/.claude/hooks, and DIR plays the home directory of the chain walk.

Exit codes: 0 done (status: MATCH); 1 status DRIFT or PENDING_*; 2 usage, self-hash or source-hash
mismatch (nothing read or written); 3 precondition or unsafe directory (status: NOT_INSTALLED);
4 drift, conflict or lock (status: BLOCKED); 5 already installed; 6 write failed.
"""

import copy
import errno
import fcntl
import hashlib
import json
import os
import pwd
import re
import stat
import sys
import time

CLONE = "/home/claude/bukerov-twitch-miner-go"
HOOKS_ROOT = "/home/claude/.claude/hooks"
MAX_SETTINGS = 1024 * 1024
MAX_STATE = 64 * 1024
MAX_SOURCE = 1024 * 1024
OBSERVER_MARK = "prompt_observer.py"
STATUSES = ("pending_install", "installed", "pending_uninstall")
MODES = ("install", "uninstall", "remove-entry", "status")

LOCK_NAME = ".prompt-observer-installer.lock"
LOCK_ATTEMPTS = 50
LOCK_SLEEP = 0.1
WRITE_BITS = stat.S_IWGRP | stat.S_IWOTH
HEX64_RE = re.compile(r"[0-9a-f]{64}")
PREIMAGE_NAME_RE = re.compile(r"settings-preimage-[0-9]{8}T[0-9]{6}Z\.json(-[1-9][0-9]{0,5})?")
BACKUP_NAME_RE = re.compile(r"settings-before-remove-[0-9]{8}T[0-9]{6}Z\.json(-[1-9][0-9]{0,5})?")

EXIT_OK, EXIT_DRIFT, EXIT_USAGE, EXIT_PRECONDITION, EXIT_CONFLICT, EXIT_INSTALLED, EXIT_WRITE = range(7)

# Refusals after which `status` tells what to do next; every other refusal is "owner-review".
NEXT_STEP_STATUS = frozenset(("LOCKED", "DRIFT_BEFORE_WRITE", "SETTINGS_DRIFT", "PENDING_STATE",
                              "PREIMAGE_COPY_CORRUPT", "ALREADY_INSTALLED", "NOT_INSTALLED", "WRITE_FAILED"))
CHAIN_REASONS = {"system": "ANCESTOR_UNSAFE", "home": "ANCESTOR_UNSAFE", "ancestor": "ANCESTOR_UNSAFE",
                 "hooks_root": "HOOKS_ROOT_UNSAFE", "obs_root": "OBSERVER_ROOT_UNSAFE",
                 "runtime_dir": "RUNTIME_DIR_UNSAFE"}

# Test-only fault injection (in-process tests set entries; empty in every real run): a named point
# maps to a callable to run there, or to an exception instance to raise there.
FAULTS = {}


def effective_uid():
    """The uid every ownership check compares with (a module-level seam the tests may replace)."""
    return os.geteuid()


def _checkpoint(name):
    action = FAULTS.get(name)
    if action is None:
        return
    if isinstance(action, BaseException):
        raise action
    action()


class Refusal(Exception):
    def __init__(self, code, reason, **extra):
        super().__init__(reason)
        self.code = code
        self.reason = reason
        self.extra = extra


class WriteFailed(Exception):
    pass


# --- layout -----------------------------------------------------------------------------------

class Layout:
    def __init__(self, test_root=None):
        clone = CLONE if test_root is None else os.path.join(test_root, "project")
        self.hooks_root = HOOKS_ROOT if test_root is None else os.path.join(test_root, "home", ".claude", "hooks")
        self.source = os.path.join(clone, ".claude", "hooks", "prompt_observer.py")
        self.settings = os.path.join(clone, ".claude", "settings.local.json")
        self.obs_root = os.path.join(self.hooks_root, "prompt-observer")
        self.state = os.path.join(self.obs_root, "state.json")
        self.lock = os.path.join(self.hooks_root, LOCK_NAME)

    def runtime_dir(self, sha):
        return os.path.join(self.obs_root, sha[:12])

    def runtime_path(self, sha):
        return os.path.join(self.runtime_dir(sha), "prompt_observer.py")


# --- small helpers ------------------------------------------------------------------------------

def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def utc_stamp():
    return time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())


def _reject_duplicates(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate key")
        result[key] = value
    return result


def _reject_constant(_name):
    raise ValueError("non-finite number")


def loads_strict(data):
    return json.loads(data.decode("utf-8"), object_pairs_hook=_reject_duplicates,
                      parse_constant=_reject_constant)


def dumps_settings(obj):
    """Serialised exactly as the guard installer serialises the settings document."""
    return (json.dumps(obj, indent=2, ensure_ascii=False, allow_nan=False) + "\n").encode("utf-8")


def dumps_record(obj):
    return (json.dumps(obj, indent=2, ensure_ascii=True, allow_nan=False, sort_keys=True) + "\n").encode("ascii")


def emit(obj):
    sys.stdout.write(json.dumps(obj, sort_keys=True, ensure_ascii=True, allow_nan=False) + "\n")
    sys.stdout.flush()


def read_regular(path, limit):
    """A regular file (never through a symlink, never blocking on a FIFO) of at most `limit` bytes."""
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            raise OSError("not a regular file")
        if info.st_size > limit:
            raise OSError("too large")
        chunks = []
        remaining = limit + 1
        while remaining > 0:
            chunk = os.read(fd, min(remaining, 1 << 20))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        data = b"".join(chunks)
        if len(data) > limit:
            raise OSError("too large")
        return data
    finally:
        os.close(fd)


class UnsafeFile(Exception):
    """A journal or settings copy that is not a private regular file of this user."""


def read_private(path, limit):
    """A regular file owned by the effective uid, link count 1, mode exactly 0600, at most `limit`
    bytes, opened without following a final symlink and without blocking on a FIFO; every check
    is made on the opened descriptor. FileNotFoundError when absent, UnsafeFile otherwise."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
    except FileNotFoundError:
        raise
    except OSError:
        raise UnsafeFile("cannot open") from None
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != effective_uid() or info.st_nlink != 1
                or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > limit):
            raise UnsafeFile("not a private regular file")
        chunks = []
        remaining = limit + 1
        while remaining > 0:
            chunk = os.read(fd, min(remaining, 1 << 20))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        data = b"".join(chunks)
        if len(data) > limit:
            raise UnsafeFile("too large")
        return data
    finally:
        os.close(fd)


def unique_path(path):
    candidate = path
    n = 0
    while os.path.lexists(candidate):
        n += 1
        candidate = f"{path}-{n}"
    return candidate


def fsync_dir(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def write_new_file(path, data, mode):
    """O_EXCL|O_NOFOLLOW create, full write, fsync, chmod, read-back hash check."""
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600)
    try:
        view = memoryview(data)
        while view:
            view = view[os.write(fd, view):]
        os.fsync(fd)
    finally:
        os.close(fd)
    os.chmod(path, mode)
    fsync_dir(os.path.dirname(path))
    if sha256_bytes(read_regular(path, max(len(data), 1))) != sha256_bytes(data):
        raise WriteFailed("read-back mismatch")


def _unlink_own(path, own_id):
    """The installer's only deletion: a temporary file this run created, identified by the device
    and inode of its own descriptor, checked against lstat of the path just before the unlink."""
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return
    if (info.st_dev, info.st_ino) == own_id and stat.S_ISREG(info.st_mode):
        os.unlink(path)


def atomic_write(target, data, mode, point, on_replaced=None, expect_sha=None):
    """Temporary file in the target's directory (O_EXCL|O_NOFOLLOW, 0600), full write, fsync, chmod
    to the target's mode, checkpoint `point`; with `expect_sha` the target is then re-read and must
    still hash to it (else DRIFT_BEFORE_WRITE, the temporary file removed, the target untouched);
    then os.replace as the next system call, fsync of the directory, read-back hash check."""
    directory = os.path.dirname(target)
    temp = os.path.join(directory, f".{os.path.basename(target)}.tmp-{os.getpid()}-{os.urandom(4).hex()}")
    fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600)
    opened = os.fstat(fd)
    own_id = (opened.st_dev, opened.st_ino)
    try:
        try:
            view = memoryview(data)
            while view:
                view = view[os.write(fd, view):]
            os.fsync(fd)
            os.fchmod(fd, mode)
        finally:
            os.close(fd)
        _checkpoint(point)
        if expect_sha is not None and settings_sha_at(target) != expect_sha:
            raise Refusal(EXIT_CONFLICT, "DRIFT_BEFORE_WRITE")
        os.replace(temp, target)
    except BaseException:
        _unlink_own(temp, own_id)
        raise
    if on_replaced is not None:
        on_replaced()
    fsync_dir(directory)
    if sha256_bytes(read_regular(target, max(len(data), 1))) != sha256_bytes(data):
        raise WriteFailed("read-back mismatch")


# --- the settings document ------------------------------------------------------------------------

def read_settings(layout):
    """Returns (bytes, object, mode); every problem is a precondition refusal (exit 3)."""
    try:
        info = os.lstat(layout.settings)
    except FileNotFoundError:
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_ABSENT") from None
    if stat.S_ISLNK(info.st_mode):
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_SYMLINK")
    if not stat.S_ISREG(info.st_mode):
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_NOT_REGULAR")
    if info.st_size > MAX_SETTINGS:
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_TOO_LARGE")
    try:
        data = read_regular(layout.settings, MAX_SETTINGS)
    except OSError:
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_UNREADABLE") from None
    try:
        obj = loads_strict(data)
    except ValueError as exc:
        reason = "SETTINGS_DUPLICATE_KEYS" if str(exc) == "duplicate key" else "SETTINGS_INVALID_JSON"
        raise Refusal(EXIT_PRECONDITION, reason) from None
    except RecursionError:
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_INVALID_JSON") from None
    if not isinstance(obj, dict):
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_NOT_OBJECT")
    if "hooks" in obj and not isinstance(obj["hooks"], dict):
        raise Refusal(EXIT_PRECONDITION, "HOOKS_NOT_OBJECT")
    if "Notification" in obj.get("hooks", {}) and not isinstance(obj["hooks"]["Notification"], list):
        raise Refusal(EXIT_PRECONDITION, "NOTIFICATION_NOT_LIST")
    return data, obj, stat.S_IMODE(info.st_mode)


def settings_sha_at(path):
    """SHA-256 of a regular (not symlinked) settings file, else None."""
    try:
        info = os.lstat(path)
        if not stat.S_ISREG(info.st_mode):
            return None
        return sha256_bytes(read_regular(path, MAX_SETTINGS))
    except OSError:
        return None


def settings_sha_or_none(layout):
    return settings_sha_at(layout.settings)


# --- the directory chain and the lock (3.2) ----------------------------------------------------------

def _dir_fault(path, owner, exact_mode=None, sticky_ok=False):
    """None for a real directory owned by `owner` with a safe mode, else the fault's name."""
    try:
        info = os.lstat(path)
    except OSError:
        return "missing"
    if stat.S_ISLNK(info.st_mode):
        return "symlink"
    if not stat.S_ISDIR(info.st_mode):
        return "not_directory"
    if info.st_uid != owner:
        return "owner"
    mode = stat.S_IMODE(info.st_mode)
    if exact_mode is not None:
        return None if mode == exact_mode else "mode"
    if mode & WRITE_BITS and not (sticky_ok and mode & stat.S_ISVTX):
        return "writable"
    return None


def walk_chain(hooks_root, obs_root, home, euid, system=True):
    """The directory chain of 3.2, one lstat per component, each looked up only after its parent
    passed. Components above `home` (walked from "/" when `system`): owned by uid 0, no group or
    other write unless sticky. `home` down to `hooks_root`: owned by `euid`, no group or other
    write. `obs_root`, when it exists: owned by `euid`, mode exactly 0700. Returns None when safe,
    else (component, fault)."""
    hooks_root = os.path.normpath(hooks_root)
    obs_root = os.path.normpath(obs_root)
    home = os.path.normpath(home)
    if home == "/" or not hooks_root.startswith(home + "/"):
        return ("ancestor", "missing")  # the hooks root does not lie below the home directory
    if system:
        path = "/"
        names = home.strip("/").split("/")[:-1]
        for name in [None] + names:
            path = "/" if name is None else os.path.join(path, name)
            fault = _dir_fault(path, 0, sticky_ok=True)
            if fault:
                return ("system", fault)
    fault = _dir_fault(home, euid)
    if fault:
        return ("home", fault)
    path = home
    for name in hooks_root[len(home) + 1:].split("/")[:-1]:
        path = os.path.join(path, name)
        fault = _dir_fault(path, euid)
        if fault:
            return ("ancestor", fault)
    fault = _dir_fault(hooks_root, euid)
    if fault:
        return ("hooks_root", fault)
    if os.path.lexists(obs_root):
        fault = _dir_fault(obs_root, euid, exact_mode=0o700)
        if fault:
            return ("obs_root", fault)
    return None


def runtime_dir_fault(path):
    """None when a runtime directory is absent or a real directory of this user without group or
    other write, else the fault's name."""
    if not os.path.lexists(path):
        return None
    return _dir_fault(path, effective_uid())


def chain_refusal(component, fault):
    return Refusal(EXIT_PRECONDITION, CHAIN_REASONS[component], chain_component=component, chain_fault=fault)


def check_runtime_dir(layout, sha):
    fault = runtime_dir_fault(layout.runtime_dir(sha))
    if fault:
        raise chain_refusal("runtime_dir", fault)


def acquire_lock(layout):
    """The advisory installer lock for one invocation; returns its descriptor, which main() closes
    in a finally block. Refusals: LOCK_UNSAFE (not a private regular file), LOCKED (held)."""
    try:
        fd = os.open(layout.lock, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600)
    except OSError:
        raise Refusal(EXIT_CONFLICT, "LOCK_UNSAFE") from None
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != effective_uid() or info.st_nlink != 1
                or stat.S_IMODE(info.st_mode) != 0o600):
            raise Refusal(EXIT_CONFLICT, "LOCK_UNSAFE")
        for attempt in range(LOCK_ATTEMPTS):
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                return fd
            except OSError as exc:
                if exc.errno not in (errno.EAGAIN, errno.EACCES):
                    raise
            if attempt + 1 < LOCK_ATTEMPTS:
                time.sleep(LOCK_SLEEP)
        raise Refusal(EXIT_CONFLICT, "LOCKED")
    except BaseException:
        os.close(fd)
        raise


def notification_commands(obj):
    commands = []
    hooks = obj.get("hooks") if isinstance(obj, dict) else None
    entries = hooks.get("Notification") if isinstance(hooks, dict) else None
    for entry in entries if isinstance(entries, list) else ():
        inner = entry.get("hooks") if isinstance(entry, dict) else None
        for hook in inner if isinstance(inner, list) else ():
            if isinstance(hook, dict) and isinstance(hook.get("command"), str):
                commands.append(hook["command"])
    return commands


def entry_counts(obj):
    hooks = obj.get("hooks") if isinstance(obj, dict) else None
    if not isinstance(hooks, dict):
        return {}
    return {event: (len(entries) if isinstance(entries, list) else None) for event, entries in hooks.items()}


def registration_command(layout, sha):
    return f"python3 -I {layout.runtime_path(sha)} --expect-sha256 {sha}"


def observer_entry(command):
    return {"matcher": "permission_prompt", "hooks": [{"type": "command", "command": command, "timeout": 10}]}


def build_postimage(pre_obj, command):
    post = copy.deepcopy(pre_obj)
    hooks = post.setdefault("hooks", {})
    hooks.setdefault("Notification", []).append(observer_entry(command))
    return post


def remove_observer(obj, command):
    """Exactly the one inner hook whose command is `command` removed (its entry too when it held no
    other hook; an emptied Notification list too). Returns (new object, how to undo it)."""
    positions = []
    entries = obj.get("hooks", {}).get("Notification", [])
    for ei, entry in enumerate(entries):
        inner = entry.get("hooks") if isinstance(entry, dict) else None
        for hi, hook in enumerate(inner if isinstance(inner, list) else ()):
            if isinstance(hook, dict) and hook.get("command") == command:
                positions.append((ei, hi))
    if len(positions) != 1:
        raise Refusal(EXIT_CONFLICT, "OBSERVER_ENTRY_COUNT", observer_entries=len(positions))
    ei, hi = positions[0]
    new = copy.deepcopy(obj)
    notification = new["hooks"]["Notification"]
    hook = notification[ei]["hooks"].pop(hi)
    shell = None
    if not notification[ei]["hooks"]:
        shell = notification.pop(ei)
    list_removed = not notification
    if list_removed:
        del new["hooks"]["Notification"]
    return new, (ei, hi, hook, shell, list_removed)


def undo_removal(new, undo):
    ei, hi, hook, shell, list_removed = undo
    obj = copy.deepcopy(new)
    if list_removed:
        obj["hooks"]["Notification"] = []
    notification = obj["hooks"]["Notification"]
    if shell is not None:
        notification.insert(ei, copy.deepcopy(shell))
    notification[ei]["hooks"].insert(hi, copy.deepcopy(hook))
    return obj


# --- the journal -------------------------------------------------------------------------------

STATE_KEYS = ("version", "status", "started_utc", "runtime_path", "runtime_sha256", "registration_command",
              "preimage_sha256", "preimage_copy", "preimage_mode", "postimage_sha256")
OPTIONAL_STATE_KEYS = ("removal_sha256", "removal_backup")


def _copy_name_ok(layout, path, pattern):
    prefix = layout.obs_root + "/"
    return isinstance(path, str) and path.startswith(prefix) and pattern.fullmatch(path[len(prefix):]) is not None


def valid_state(layout, state):
    """The journal schema of 3.2 (C1 journals included): exact keys, lowercase-hex hashes, the
    runtime path and registration command built from the runtime hash, copy names in OBS_ROOT."""
    if not isinstance(state, dict):
        return False
    keys = set(state)
    if not set(STATE_KEYS) <= keys or not keys <= set(STATE_KEYS + OPTIONAL_STATE_KEYS):
        return False
    if type(state["version"]) is not int or state["version"] != 1:
        return False
    if not isinstance(state["status"], str) or state["status"] not in STATUSES:
        return False
    hashes = ("runtime_sha256", "preimage_sha256", "postimage_sha256") + (
        ("removal_sha256",) if "removal_sha256" in state else ())
    for key in hashes:
        if not isinstance(state[key], str) or HEX64_RE.fullmatch(state[key]) is None:
            return False
    sha = state["runtime_sha256"]
    runtime_path = layout.obs_root + "/" + sha[:12] + "/prompt_observer.py"
    if state["runtime_path"] != runtime_path:
        return False
    if state["registration_command"] != "python3 -I " + runtime_path + " --expect-sha256 " + sha:
        return False
    if not _copy_name_ok(layout, state["preimage_copy"], PREIMAGE_NAME_RE):
        return False
    if "removal_backup" in state and not _copy_name_ok(layout, state["removal_backup"], BACKUP_NAME_RE):
        return False
    mode = state["preimage_mode"]
    if type(mode) is not int or not 0 <= mode <= 0o7777:
        return False
    return isinstance(state["started_utc"], str)


def read_state(layout):
    """The validated journal, or None when there is none. STATE_UNSAFE: not a private regular file;
    STATE_CORRUPT: unreadable or invalid. Corruption is never repaired."""
    try:
        data = read_private(layout.state, MAX_STATE)
    except FileNotFoundError:
        return None
    except UnsafeFile:
        raise Refusal(EXIT_CONFLICT, "STATE_UNSAFE") from None
    try:
        state = loads_strict(data)
    except (ValueError, RecursionError):
        raise Refusal(EXIT_CONFLICT, "STATE_CORRUPT") from None
    if not valid_state(layout, state):
        raise Refusal(EXIT_CONFLICT, "STATE_CORRUPT")
    return state


def read_state_bytes(layout):
    """The journal's exact bytes (read again under the lock, after read_state validated them)."""
    try:
        return read_private(layout.state, MAX_STATE)
    except (OSError, UnsafeFile):
        raise Refusal(EXIT_CONFLICT, "STATE_UNSAFE") from None


def write_state(layout, state):
    atomic_write(layout.state, dumps_record(state), 0o600, "state:replace")


def restore_state_bytes(layout, data):
    """Puts the journal back to the exact bytes it had before this run changed it."""
    atomic_write(layout.state, data, 0o600, "state:restore")


def read_copy(path, sha):
    """A preimage copy that passes the private-file checks and hashes to `sha`."""
    try:
        data = read_private(path, MAX_SETTINGS)
    except (OSError, UnsafeFile):
        raise Refusal(EXIT_CONFLICT, "PREIMAGE_COPY_CORRUPT") from None
    if sha256_bytes(data) != sha:
        raise Refusal(EXIT_CONFLICT, "PREIMAGE_COPY_CORRUPT")
    return data


def finish_removal(layout, state, runtime_tag):
    """Rename the runtime directory (when present) and then the journal; idempotent."""
    stamp = utc_stamp()
    runtime_dir = layout.runtime_dir(state["runtime_sha256"])
    renamed_runtime = None
    if os.path.lexists(runtime_dir):
        check_runtime_dir(layout, state["runtime_sha256"])
        renamed_runtime = unique_path(f"{runtime_dir}.{runtime_tag}-{stamp}")
        os.rename(runtime_dir, renamed_runtime)
    _checkpoint("finish:between-renames")
    renamed_state = unique_path(f"{layout.state}.uninstalled-{stamp}")
    os.rename(layout.state, renamed_state)
    fsync_dir(layout.obs_root)
    return renamed_runtime, renamed_state


def abort_install(layout, runtime_dir, runtime_id, state_bytes):
    """Rename only what THIS run created: the runtime directory when its device and inode are the
    ones this run created, the journal when its bytes are the ones this run wrote. Anything else is
    left in place and reported as "foreign_state"."""
    stamp = utc_stamp()
    renamed = {}
    foreign = False
    if runtime_id is not None and os.path.lexists(runtime_dir):
        info = os.lstat(runtime_dir)
        if (info.st_dev, info.st_ino) == runtime_id and stat.S_ISDIR(info.st_mode):
            renamed["runtime_renamed_to"] = unique_path(f"{runtime_dir}.aborted-{stamp}-{os.getpid()}")
            os.rename(runtime_dir, renamed["runtime_renamed_to"])
        else:
            foreign = True
    if os.path.lexists(layout.state):
        try:
            current = read_regular(layout.state, MAX_STATE)
        except OSError:
            current = None
        if state_bytes is not None and current == state_bytes:
            renamed["state_renamed_to"] = unique_path(f"{layout.state}.aborted-{stamp}")
            os.rename(layout.state, renamed["state_renamed_to"])
        else:
            foreign = True
    if foreign:
        renamed["foreign_state"] = True
    return renamed


# --- modes ---------------------------------------------------------------------------------------

def _mark_replaced(flag):
    """Runs the instant os.replace of the settings file has returned."""
    flag.append(True)
    _checkpoint("install:after-settings-replace")


def read_source(layout, source_sha):
    try:
        info = os.lstat(layout.source)
        if not stat.S_ISREG(info.st_mode):
            raise OSError("source not a regular file")
        source = read_regular(layout.source, MAX_SOURCE)
    except OSError:
        raise Refusal(EXIT_USAGE, "SOURCE_UNREADABLE") from None
    if sha256_bytes(source) != source_sha:
        raise Refusal(EXIT_USAGE, "SOURCE_HASH_MISMATCH")
    return source


def do_install(layout, self_sha, source, source_sha):
    pre_bytes, pre_obj, pre_mode = read_settings(layout)
    state = read_state(layout)
    if state is not None:
        if state["status"] == "installed":
            raise Refusal(EXIT_INSTALLED, "ALREADY_INSTALLED")
        raise Refusal(EXIT_CONFLICT, "PENDING_STATE", status=state["status"])
    if any(OBSERVER_MARK in command for command in notification_commands(pre_obj)):
        raise Refusal(EXIT_CONFLICT, "OBSERVER_ENTRY_EXISTS")
    runtime_dir = layout.runtime_dir(source_sha)
    pre_sha = sha256_bytes(pre_bytes)
    command = registration_command(layout, source_sha)
    post_obj = build_postimage(pre_obj, command)
    post_bytes = dumps_settings(post_obj)
    post_sha = sha256_bytes(post_bytes)

    created_runtime = None  # (st_dev, st_ino) of the runtime directory this run created
    own_state = None  # the journal bytes this run wrote
    orphaned = None
    replaced = []
    try:
        if not os.path.lexists(layout.obs_root):
            os.mkdir(layout.obs_root, 0o700)
            os.chmod(layout.obs_root, 0o700)
            fault = _dir_fault(layout.obs_root, effective_uid(), exact_mode=0o700)
            if fault:
                raise chain_refusal("obs_root", fault)
        if os.path.lexists(runtime_dir):  # left by an interrupted older run: no journal, no entry
            check_runtime_dir(layout, source_sha)
            orphaned = unique_path(f"{runtime_dir}.orphaned-{utc_stamp()}")
            os.rename(runtime_dir, orphaned)
        preimage_copy = unique_path(os.path.join(layout.obs_root, f"settings-preimage-{utc_stamp()}.json"))
        write_new_file(preimage_copy, pre_bytes, 0o600)
        state = {"version": 1, "status": "pending_install", "started_utc": utc_stamp(),
                 "runtime_path": layout.runtime_path(source_sha), "runtime_sha256": source_sha,
                 "registration_command": command, "preimage_sha256": pre_sha,
                 "preimage_copy": preimage_copy, "preimage_mode": pre_mode, "postimage_sha256": post_sha}
        own_state = dumps_record(state)
        write_state(layout, state)  # journal: pending_install, before the runtime and the settings change
        _checkpoint("install:after-state-pending")
        os.mkdir(runtime_dir, 0o755)
        info = os.lstat(runtime_dir)
        created_runtime = (info.st_dev, info.st_ino)
        os.chmod(runtime_dir, 0o755)
        write_new_file(layout.runtime_path(source_sha), source, 0o444)
        _checkpoint("install:after-runtime")
        _checkpoint("install:before-settings-reread")
        atomic_write(layout.settings, post_bytes, pre_mode, "install:replace-settings",
                     on_replaced=lambda: _mark_replaced(replaced), expect_sha=pre_sha)
    except Refusal as refusal:
        refusal.extra.update(abort_install(layout, runtime_dir, created_runtime, own_state))
        raise
    except Exception as exc:
        if replaced:
            raise WriteFailed(f"after the settings replacement: {type(exc).__name__}") from None
        renamed = abort_install(layout, runtime_dir, created_runtime, own_state)
        raise Refusal(EXIT_WRITE, "WRITE_FAILED", error=type(exc).__name__, **renamed) from None
    state["status"] = "installed"
    write_state(layout, state)
    _checkpoint("install:after-installed")
    return {"result": "INSTALLED", "installer_sha256": self_sha, "runtime_sha256": source_sha,
            "preimage_sha256": pre_sha, "postimage_sha256": post_sha, "runtime_path": state["runtime_path"],
            "registration_command": command, "preimage_copy": preimage_copy,
            "orphaned_runtime_renamed_to": orphaned, "hook_entry_counts": entry_counts(post_obj)}


def do_uninstall(layout):
    state = read_state(layout)
    if state is None:
        raise Refusal(EXIT_PRECONDITION, "NOT_INSTALLED")
    check_runtime_dir(layout, state["runtime_sha256"])
    state_bytes = read_state_bytes(layout)
    current = settings_sha_or_none(layout)
    written = False
    if current == state["postimage_sha256"]:
        preimage = read_copy(state["preimage_copy"], state["preimage_sha256"])
        pending = dict(state, status="pending_uninstall")
        write_state(layout, pending)  # journal: pending_uninstall, before the restore
        _checkpoint("uninstall:after-state-pending")
        try:
            atomic_write(layout.settings, preimage, state["preimage_mode"], "uninstall:replace-settings",
                         expect_sha=current)
        except Refusal:
            restore_state_bytes(layout, state_bytes)
            raise
        written = True
        state = pending
    elif current == state["preimage_sha256"] or (current is not None and current == state.get("removal_sha256")):
        pass  # an install interrupted before its replacement, or an uninstall/remove-entry after it
    else:
        raise Refusal(EXIT_CONFLICT, "SETTINGS_DRIFT", settings_sha256=current,
                      postimage_sha256=state["postimage_sha256"], preimage_sha256=state["preimage_sha256"])
    _checkpoint("uninstall:before-renames")
    renamed_runtime, renamed_state = finish_removal(layout, state, "disabled")
    return {"result": "UNINSTALLED", "settings_written": written, "settings_sha256": settings_sha_or_none(layout),
            "runtime_renamed_to": renamed_runtime, "state_renamed_to": renamed_state}


def do_remove_entry(layout):
    state = read_state(layout)
    if state is None:
        raise Refusal(EXIT_PRECONDITION, "NOT_INSTALLED")
    check_runtime_dir(layout, state["runtime_sha256"])
    state_bytes = read_state_bytes(layout)
    cur_bytes, cur_obj, cur_mode = read_settings(layout)
    cur_sha = sha256_bytes(cur_bytes)
    command = state["registration_command"]
    count = notification_commands(cur_obj).count(command)
    if count >= 2:
        raise Refusal(EXIT_CONFLICT, "DUPLICATE_ENTRY", observer_entries=count)
    if count == 0:  # the entry is already gone (a foreign edit, or an interrupted run): only finish
        renamed_runtime, renamed_state = finish_removal(layout, state, "disabled")
        return {"result": "ENTRY_ALREADY_ABSENT", "settings_written": False, "settings_sha256": cur_sha,
                "runtime_renamed_to": renamed_runtime, "state_renamed_to": renamed_state}
    new_obj, undo = remove_observer(cur_obj, command)
    if undo_removal(new_obj, undo) != cur_obj or command in notification_commands(new_obj):
        raise Refusal(EXIT_CONFLICT, "REMOVAL_NOT_EXACT")
    new_bytes = dumps_settings(new_obj)
    backup = unique_path(os.path.join(layout.obs_root, f"settings-before-remove-{utc_stamp()}.json"))
    write_new_file(backup, cur_bytes, 0o600)
    pending = dict(state, status="pending_uninstall", removal_sha256=sha256_bytes(new_bytes), removal_backup=backup)
    write_state(layout, pending)  # journal before the settings change
    _checkpoint("remove-entry:after-state-pending")
    try:
        atomic_write(layout.settings, new_bytes, cur_mode, "remove-entry:replace-settings", expect_sha=cur_sha)
    except Refusal as refusal:
        restore_state_bytes(layout, state_bytes)
        refusal.extra["backup_renamed_to"] = unique_path(f"{backup}.aborted-{utc_stamp()}")
        os.rename(backup, refusal.extra["backup_renamed_to"])
        raise
    if loads_strict(read_regular(layout.settings, MAX_SETTINGS)) != new_obj:
        raise WriteFailed("read-back differs")
    _checkpoint("remove-entry:before-renames")
    renamed_runtime, renamed_state = finish_removal(layout, pending, "disabled")
    return {"result": "ENTRY_REMOVED", "reserialized": True, "settings_written": True,
            "settings_sha256": pending["removal_sha256"], "backup_copy": backup, "backup_sha256": cur_sha,
            "runtime_renamed_to": renamed_runtime, "state_renamed_to": renamed_state}


def do_status(layout, chain):
    """Read-only; takes no lock and creates nothing. Rows 1 to 10 of 3.2, the first that applies."""
    out = {"result": None, "reason": None, "next_step": None, "roots": "safe", "chain_component": None,
           "chain_fault": None, "settings_sha256": None, "postimage_sha256": None, "preimage_sha256": None,
           "runtime_path": None, "runtime_sha256": None, "runtime_sha256_ok": None, "observer_entries": None,
           "permission_request_entries": None}

    def blocked(reason):
        out.update(result="BLOCKED", reason=reason, next_step="owner-review")
        return out, EXIT_CONFLICT

    if chain is not None:  # row 1
        out.update(roots="unsafe", chain_component=chain[0], chain_fault=chain[1])
        return blocked("ROOTS_UNSAFE")
    try:
        cur_bytes, current_obj, _ = read_settings(layout)
    except Refusal:  # row 2
        return blocked("SETTINGS_INVALID")
    current = sha256_bytes(cur_bytes)
    out["settings_sha256"] = current
    out["observer_entries"] = sum(OBSERVER_MARK in c for c in notification_commands(current_obj))
    out["permission_request_entries"] = entry_counts(current_obj).get("PermissionRequest", 0)
    try:
        state = read_state(layout)
    except Refusal:  # row 5
        return blocked("STATE_CORRUPT")
    if state is None:
        if out["observer_entries"]:  # row 3
            return blocked("ORPHAN_ENTRY")
        out.update(result="NOT_INSTALLED", next_step="none")  # row 4
        return out, EXIT_PRECONDITION
    out.update(postimage_sha256=state["postimage_sha256"], preimage_sha256=state["preimage_sha256"],
               runtime_path=state["runtime_path"], runtime_sha256=state["runtime_sha256"])
    fault = runtime_dir_fault(layout.runtime_dir(state["runtime_sha256"]))
    if fault:  # row 5a
        out.update(roots="unsafe", chain_component="runtime_dir", chain_fault=fault)
        return blocked("ROOTS_UNSAFE")
    try:
        runtime = read_regular(state["runtime_path"], MAX_SOURCE)
        out["runtime_sha256_ok"] = sha256_bytes(runtime) == state["runtime_sha256"]
    except OSError:
        out["runtime_sha256_ok"] = False
    count = notification_commands(current_obj).count(state["registration_command"])
    if (state["status"] == "installed" and current == state["postimage_sha256"] and out["runtime_sha256_ok"]
            and count == 1):  # row 6
        out.update(result="MATCH", next_step="none")
        return out, EXIT_OK
    out["result"] = {"pending_install": "PENDING_INSTALL", "pending_uninstall": "PENDING_UNINSTALL"}.get(
        state["status"], "DRIFT")
    if current == state["postimage_sha256"] and _copy_intact(state):  # row 7
        out["next_step"] = "uninstall"
    elif current == state["preimage_sha256"] or current == state.get("removal_sha256"):  # row 8
        out["next_step"] = "uninstall"
    elif count <= 1:  # row 9
        out["next_step"] = "remove-entry"
    else:  # row 10
        return blocked("DUPLICATE_ENTRY")
    return out, EXIT_DRIFT


def _copy_intact(state):
    try:
        read_copy(state["preimage_copy"], state["preimage_sha256"])
    except Refusal:
        return False
    return True


# --- entry point --------------------------------------------------------------------------------

def parse_args(argv):
    if not argv or argv[0] not in MODES:
        return None
    opts = {"mode": argv[0], "expect": None, "source": None, "test_root": None}
    names = {"--expect-sha256": "expect", "--source-sha256": "source", "--test-root": "test_root"}
    i = 1
    while i < len(argv):
        key = names.get(argv[i])
        if key is None or opts[key] is not None or i + 1 >= len(argv):
            return None
        opts[key] = argv[i + 1]
        i += 2
    hexdigits = set("0123456789abcdef")
    for key in ("expect", "source"):
        if opts[key] is not None and (len(opts[key]) != 64 or not set(opts[key]) <= hexdigits):
            return None
    if opts["expect"] is None or (opts["mode"] == "install") != (opts["source"] is not None):
        return None
    return opts


def refusal_output(refusal):
    if refusal.reason == "WRITE_FAILED":
        return dict(refusal.extra, result="WRITE_FAILED", next_step="status")
    next_step = "status" if refusal.reason in NEXT_STEP_STATUS else "owner-review"
    return dict(refusal.extra, result="REFUSED", reason=refusal.reason, next_step=next_step)


def main(argv=None):
    opts = parse_args(sys.argv[1:] if argv is None else argv)
    if opts is None:
        emit({"result": "REFUSED", "reason": "USAGE", "next_step": "owner-review"})
        return EXIT_USAGE
    with open(os.path.abspath(__file__), "rb") as handle:
        self_sha = sha256_bytes(handle.read())
    if self_sha != opts["expect"]:
        emit({"result": "REFUSED", "reason": "SELF_HASH_MISMATCH", "next_step": "owner-review"})
        return EXIT_USAGE
    test_root = None
    if opts["test_root"] is not None:
        test_root = os.path.realpath(opts["test_root"])
        if not os.path.isdir(test_root):
            emit({"result": "REFUSED", "reason": "USAGE", "next_step": "owner-review"})
            return EXIT_USAGE
    layout = Layout(test_root)
    euid = effective_uid()
    if test_root is not None:  # the test root plays the home directory; pwd is not consulted
        chain = walk_chain(layout.hooks_root, layout.obs_root, test_root, euid, system=False)
    else:
        try:
            home = pwd.getpwuid(euid).pw_dir
        except KeyError:
            home = None
        chain = ("home", "missing") if not home else walk_chain(layout.hooks_root, layout.obs_root, home, euid)
    if opts["mode"] == "status":
        out, code = do_status(layout, chain)
        emit(out)
        return code
    lock_fd = None
    try:
        try:
            if chain is not None:
                raise chain_refusal(*chain)
            source = read_source(layout, opts["source"]) if opts["mode"] == "install" else None
            lock_fd = acquire_lock(layout)
            if opts["mode"] == "install":
                receipt = do_install(layout, self_sha, source, opts["source"])
            elif opts["mode"] == "uninstall":
                receipt = do_uninstall(layout)
            else:
                receipt = do_remove_entry(layout)
        except Refusal as refusal:
            emit(refusal_output(refusal))
            return refusal.code
        except (WriteFailed, OSError) as exc:
            emit({"result": "WRITE_FAILED", "error": type(exc).__name__, "next_step": "status"})
            return EXIT_WRITE
        receipt["next_step"] = "none"
        try:
            emit(receipt)
        except OSError:  # the operation is complete and final; `status` shows it
            return EXIT_WRITE
        return EXIT_OK
    finally:
        if lock_fd is not None:
            os.close(lock_fd)


if __name__ == "__main__":
    sys.exit(main())
