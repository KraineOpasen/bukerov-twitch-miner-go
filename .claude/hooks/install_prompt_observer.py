#!/usr/bin/env python3
"""Owner-run installer of the prompt observer (guard-g1-contract-rev3 3.6; README-prompt-observer.md).

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

prompt-observer/state.json is a journal ("pending_install", "installed", "pending_uninstall"),
written BEFORE every settings change it announces, so an interrupted run is always recovered by
running `uninstall` again. `uninstall` restores the exact preimage bytes and mode while the
settings file is byte-identical to the installed postimage; after a foreign change it refuses and
`remove-entry` removes exactly the observer entry (the file is then re-serialised). `status` is
read-only. Nothing is ever deleted: runtime copies and journals are renamed .aborted-, .disabled-,
.uninstalled-. Receipts print counts per hook event, never another hook command or any settings
value. `--test-root DIR` (tests only) maps the clone to DIR/project and the hooks root to
DIR/home/.claude/hooks.

Exit codes: 0 done (status: MATCH); 1 status DRIFT or PENDING_*; 2 usage, self-hash or source-hash
mismatch (nothing read or written); 3 precondition (status: NOT_INSTALLED); 4 drift or conflict;
5 already installed; 6 write failed.
"""

import copy
import hashlib
import json
import os
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

EXIT_OK, EXIT_DRIFT, EXIT_USAGE, EXIT_PRECONDITION, EXIT_CONFLICT, EXIT_INSTALLED, EXIT_WRITE = range(7)

# Test-only fault injection (in-process tests set entries; empty in every real run): a named point
# maps to a callable to run there, or to an exception instance to raise there.
FAULTS = {}


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


def atomic_write(target, data, mode, point, on_replaced=None):
    """Temporary file in the target's directory (O_EXCL|O_NOFOLLOW, 0600), full write, fsync, chmod
    to the target's mode, os.replace, fsync of the directory, read-back hash check."""
    directory = os.path.dirname(target)
    temp = os.path.join(directory, f".{os.path.basename(target)}.tmp-{os.getpid()}-{os.urandom(4).hex()}")
    fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600)
    try:
        try:
            view = memoryview(data)
            while view:
                view = view[os.write(fd, view):]
            os.fsync(fd)
        finally:
            os.close(fd)
        os.chmod(temp, mode)
        _checkpoint(point)
        os.replace(temp, target)
    except BaseException:
        if os.path.lexists(temp):
            os.unlink(temp)
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


def settings_sha_or_none(layout):
    try:
        info = os.lstat(layout.settings)
        if not stat.S_ISREG(info.st_mode):
            return None
        return sha256_bytes(read_regular(layout.settings, MAX_SETTINGS))
    except OSError:
        return None


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


def read_state(layout):
    try:
        info = os.lstat(layout.state)
    except FileNotFoundError:
        return None
    if not stat.S_ISREG(info.st_mode):
        raise Refusal(EXIT_CONFLICT, "STATE_UNSAFE")
    try:
        state = loads_strict(read_regular(layout.state, MAX_STATE))
    except (OSError, ValueError, RecursionError):
        raise Refusal(EXIT_CONFLICT, "STATE_CORRUPT") from None
    ok = (isinstance(state, dict) and all(key in state for key in STATE_KEYS) and state["version"] == 1
          and state["status"] in STATUSES and isinstance(state["runtime_sha256"], str)
          and len(state["runtime_sha256"]) == 64
          and state["runtime_path"] == layout.runtime_path(state["runtime_sha256"])
          and state["registration_command"] == registration_command(layout, state["runtime_sha256"])
          and isinstance(state["preimage_copy"], str)
          and os.path.dirname(state["preimage_copy"]) == layout.obs_root
          and os.path.basename(state["preimage_copy"]).startswith("settings-preimage-")
          and isinstance(state["preimage_mode"], int) and 0 <= state["preimage_mode"] <= 0o7777)
    if not ok:
        raise Refusal(EXIT_CONFLICT, "STATE_CORRUPT")
    return state


def write_state(layout, state):
    atomic_write(layout.state, dumps_record(state), 0o600, "state:replace")


def finish_removal(layout, state, runtime_tag):
    """Rename the runtime directory (when present) and then the journal; idempotent."""
    stamp = utc_stamp()
    runtime_dir = layout.runtime_dir(state["runtime_sha256"])
    renamed_runtime = None
    if os.path.lexists(runtime_dir):
        renamed_runtime = unique_path(f"{runtime_dir}.{runtime_tag}-{stamp}")
        os.rename(runtime_dir, renamed_runtime)
    _checkpoint("finish:between-renames")
    renamed_state = unique_path(f"{layout.state}.uninstalled-{stamp}")
    os.rename(layout.state, renamed_state)
    fsync_dir(layout.obs_root)
    return renamed_runtime, renamed_state


def abort_install(layout, runtime_dir):
    """Install refused STATE when it existed, so a journal present now is this run's own."""
    stamp = utc_stamp()
    renamed = {}
    if runtime_dir is not None and os.path.lexists(runtime_dir):
        renamed["runtime_renamed_to"] = unique_path(f"{runtime_dir}.aborted-{stamp}-{os.getpid()}")
        os.rename(runtime_dir, renamed["runtime_renamed_to"])
    if os.path.lexists(layout.state):
        renamed["state_renamed_to"] = unique_path(f"{layout.state}.aborted-{stamp}")
        os.rename(layout.state, renamed["state_renamed_to"])
    return renamed


# --- modes ---------------------------------------------------------------------------------------

def _mark_replaced(flag):
    """Runs the instant os.replace of the settings file has returned."""
    flag.append(True)
    _checkpoint("install:after-settings-replace")


def do_install(layout, self_sha, source_sha):
    try:
        info = os.lstat(layout.source)
        if not stat.S_ISREG(info.st_mode):
            raise OSError("source not a regular file")
        source = read_regular(layout.source, MAX_SOURCE)
    except OSError:
        raise Refusal(EXIT_USAGE, "SOURCE_UNREADABLE") from None
    if sha256_bytes(source) != source_sha:
        raise Refusal(EXIT_USAGE, "SOURCE_HASH_MISMATCH")
    pre_bytes, pre_obj, pre_mode = read_settings(layout)
    state = read_state(layout)
    if state is not None:
        if state["status"] == "installed":
            raise Refusal(EXIT_INSTALLED, "ALREADY_INSTALLED")
        raise Refusal(EXIT_CONFLICT, "PENDING_STATE_RUN_UNINSTALL", status=state["status"])
    if any(OBSERVER_MARK in command for command in notification_commands(pre_obj)):
        raise Refusal(EXIT_CONFLICT, "OBSERVER_ENTRY_EXISTS")
    try:
        root_info = os.lstat(layout.hooks_root)
    except FileNotFoundError:
        raise Refusal(EXIT_PRECONDITION, "HOOKS_ROOT_MISSING") from None
    if not stat.S_ISDIR(root_info.st_mode):
        raise Refusal(EXIT_PRECONDITION, "HOOKS_ROOT_NOT_A_DIRECTORY")
    if os.path.lexists(layout.obs_root):
        obs_info = os.lstat(layout.obs_root)
        if not stat.S_ISDIR(obs_info.st_mode) or obs_info.st_uid != os.geteuid():
            raise Refusal(EXIT_PRECONDITION, "OBSERVER_ROOT_UNSAFE")
    runtime_dir = layout.runtime_dir(source_sha)
    if os.path.lexists(runtime_dir):
        raise Refusal(EXIT_CONFLICT, "RUNTIME_EXISTS")
    pre_sha = sha256_bytes(pre_bytes)
    command = registration_command(layout, source_sha)
    post_obj = build_postimage(pre_obj, command)
    post_bytes = dumps_settings(post_obj)
    post_sha = sha256_bytes(post_bytes)

    created_runtime = None
    replaced = []
    try:
        if not os.path.lexists(layout.obs_root):
            os.mkdir(layout.obs_root, 0o700)
            os.chmod(layout.obs_root, 0o700)
        os.mkdir(runtime_dir, 0o755)
        created_runtime = runtime_dir
        os.chmod(runtime_dir, 0o755)
        write_new_file(layout.runtime_path(source_sha), source, 0o444)
        preimage_copy = unique_path(os.path.join(layout.obs_root, f"settings-preimage-{utc_stamp()}.json"))
        write_new_file(preimage_copy, pre_bytes, 0o600)
        state = {"version": 1, "status": "pending_install", "started_utc": utc_stamp(),
                 "runtime_path": layout.runtime_path(source_sha), "runtime_sha256": source_sha,
                 "registration_command": command, "preimage_sha256": pre_sha,
                 "preimage_copy": preimage_copy, "preimage_mode": pre_mode, "postimage_sha256": post_sha}
        write_state(layout, state)  # journal: pending_install, before the settings change
        _checkpoint("install:after-state-pending")
        _checkpoint("install:before-settings-reread")
        if settings_sha_or_none(layout) != pre_sha:
            raise Refusal(EXIT_CONFLICT, "DRIFT_BEFORE_WRITE")
        atomic_write(layout.settings, post_bytes, pre_mode, "install:replace-settings",
                     on_replaced=lambda: _mark_replaced(replaced))
    except Refusal as refusal:
        refusal.extra.update(abort_install(layout, created_runtime))
        raise
    except Exception as exc:
        if replaced:
            raise WriteFailed(f"after the settings replacement: {type(exc).__name__}; run uninstall") from None
        renamed = abort_install(layout, created_runtime)
        raise Refusal(EXIT_WRITE, "WRITE_FAILED", error=type(exc).__name__, **renamed) from None
    state["status"] = "installed"
    write_state(layout, state)
    return {"result": "INSTALLED", "installer_sha256": self_sha, "runtime_sha256": source_sha,
            "preimage_sha256": pre_sha, "postimage_sha256": post_sha, "runtime_path": state["runtime_path"],
            "registration_command": command, "preimage_copy": preimage_copy,
            "hook_entry_counts": entry_counts(post_obj)}


def do_uninstall(layout):
    state = read_state(layout)
    if state is None:
        raise Refusal(EXIT_PRECONDITION, "NOT_INSTALLED")
    try:
        preimage = read_regular(state["preimage_copy"], MAX_SETTINGS)
    except OSError:
        raise Refusal(EXIT_CONFLICT, "PREIMAGE_COPY_UNREADABLE") from None
    if sha256_bytes(preimage) != state["preimage_sha256"]:
        raise Refusal(EXIT_CONFLICT, "PREIMAGE_COPY_CORRUPT")
    current = settings_sha_or_none(layout)
    written = False
    if current == state["postimage_sha256"]:
        state["status"] = "pending_uninstall"
        write_state(layout, state)  # journal: pending_uninstall, before the restore
        _checkpoint("uninstall:after-state-pending")
        atomic_write(layout.settings, preimage, state["preimage_mode"], "uninstall:replace-settings")
        written = True
    elif current == state["preimage_sha256"] or (current is not None and current == state.get("removal_sha256")):
        pass  # an install interrupted before its replacement, or an uninstall/remove-entry after it
    else:
        raise Refusal(EXIT_CONFLICT, "SETTINGS_DRIFT", settings_sha256=current,
                      postimage_sha256=state["postimage_sha256"], preimage_sha256=state["preimage_sha256"],
                      next_step="remove-entry")
    _checkpoint("uninstall:before-renames")
    renamed_runtime, renamed_state = finish_removal(layout, state, "disabled")
    return {"result": "UNINSTALLED", "settings_written": written, "settings_sha256": settings_sha_or_none(layout),
            "runtime_renamed_to": renamed_runtime, "state_renamed_to": renamed_state}


def do_remove_entry(layout):
    state = read_state(layout)
    if state is None:
        raise Refusal(EXIT_PRECONDITION, "NOT_INSTALLED")
    cur_bytes, cur_obj, cur_mode = read_settings(layout)
    cur_sha = sha256_bytes(cur_bytes)
    command = state["registration_command"]
    if (state["status"] == "pending_uninstall" and cur_sha == state.get("removal_sha256")
            and command not in notification_commands(cur_obj)):
        renamed_runtime, renamed_state = finish_removal(layout, state, "disabled")  # completes an interrupted run
        return {"result": "ENTRY_REMOVED", "reserialized": True, "settings_written": False,
                "settings_sha256": cur_sha, "runtime_renamed_to": renamed_runtime, "state_renamed_to": renamed_state}
    new_obj, undo = remove_observer(cur_obj, command)
    if undo_removal(new_obj, undo) != cur_obj or command in notification_commands(new_obj):
        raise Refusal(EXIT_CONFLICT, "REMOVAL_NOT_EXACT")
    new_bytes = dumps_settings(new_obj)
    backup = unique_path(os.path.join(layout.obs_root, f"settings-before-remove-{utc_stamp()}.json"))
    write_new_file(backup, cur_bytes, 0o600)
    state["status"] = "pending_uninstall"
    state["removal_sha256"] = sha256_bytes(new_bytes)
    state["removal_backup"] = backup
    write_state(layout, state)  # journal before the settings change
    _checkpoint("remove-entry:after-state-pending")
    atomic_write(layout.settings, new_bytes, cur_mode, "remove-entry:replace-settings")
    if loads_strict(read_regular(layout.settings, MAX_SETTINGS)) != new_obj:
        raise WriteFailed("read-back differs")
    _checkpoint("remove-entry:before-renames")
    renamed_runtime, renamed_state = finish_removal(layout, state, "disabled")
    return {"result": "ENTRY_REMOVED", "reserialized": True, "settings_written": True,
            "settings_sha256": state["removal_sha256"], "backup_copy": backup, "backup_sha256": cur_sha,
            "runtime_renamed_to": renamed_runtime, "state_renamed_to": renamed_state}


def do_status(layout):
    out = {"result": None, "settings_sha256": None, "postimage_sha256": None, "preimage_sha256": None,
           "runtime_sha256_ok": None, "observer_entries": None, "permission_request_entries": None}
    current_obj = None
    try:
        cur_bytes, current_obj, _ = read_settings(layout)
        out["settings_sha256"] = sha256_bytes(cur_bytes)
        out["observer_entries"] = sum(OBSERVER_MARK in c for c in notification_commands(current_obj))
        out["permission_request_entries"] = entry_counts(current_obj).get("PermissionRequest", 0)
    except Refusal:
        pass
    try:
        state = read_state(layout)
    except Refusal:
        out["result"] = "DRIFT"
        return out, EXIT_DRIFT
    if state is None:
        out["result"] = "NOT_INSTALLED"
        return out, EXIT_PRECONDITION
    out["postimage_sha256"] = state["postimage_sha256"]
    out["preimage_sha256"] = state["preimage_sha256"]
    try:
        runtime = read_regular(state["runtime_path"], MAX_SOURCE)
        out["runtime_sha256_ok"] = sha256_bytes(runtime) == state["runtime_sha256"]
    except OSError:
        out["runtime_sha256_ok"] = False
    if state["status"] == "pending_install":
        out["result"] = "PENDING_INSTALL"
        return out, EXIT_DRIFT
    if state["status"] == "pending_uninstall":
        out["result"] = "PENDING_UNINSTALL"
        return out, EXIT_DRIFT
    exact = current_obj is not None and notification_commands(current_obj).count(state["registration_command"]) == 1
    if out["settings_sha256"] == state["postimage_sha256"] and out["runtime_sha256_ok"] and exact:
        out["result"] = "MATCH"
        return out, EXIT_OK
    out["result"] = "DRIFT"
    return out, EXIT_DRIFT


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


def main(argv=None):
    opts = parse_args(sys.argv[1:] if argv is None else argv)
    if opts is None:
        emit({"result": "REFUSED", "reason": "USAGE"})
        return EXIT_USAGE
    with open(os.path.abspath(__file__), "rb") as handle:
        self_sha = sha256_bytes(handle.read())
    if self_sha != opts["expect"]:
        emit({"result": "REFUSED", "reason": "SELF_HASH_MISMATCH"})
        return EXIT_USAGE
    test_root = None
    if opts["test_root"] is not None:
        test_root = os.path.realpath(opts["test_root"])
        if not os.path.isdir(test_root):
            emit({"result": "REFUSED", "reason": "USAGE"})
            return EXIT_USAGE
    layout = Layout(test_root)
    try:
        if opts["mode"] == "status":
            out, code = do_status(layout)
            emit(out)
            return code
        if opts["mode"] == "install":
            receipt = do_install(layout, self_sha, opts["source"])
        elif opts["mode"] == "uninstall":
            receipt = do_uninstall(layout)
        else:
            receipt = do_remove_entry(layout)
    except Refusal as refusal:
        emit(dict(refusal.extra, result="REFUSED", reason=refusal.reason))
        return refusal.code
    except (WriteFailed, OSError) as exc:
        emit({"result": "WRITE_FAILED", "error": type(exc).__name__, "next_step": "status, then uninstall"})
        return EXIT_WRITE
    emit(receipt)
    return EXIT_OK


if __name__ == "__main__":
    sys.exit(main())
