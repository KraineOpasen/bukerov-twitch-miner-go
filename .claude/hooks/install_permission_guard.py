#!/usr/bin/env python3
"""Installer for the PermissionRequest guard (s1h-contract-rev4, section 11).

    python3 -I install_permission_guard.py install  --manifest MANIFEST
    python3 -I install_permission_guard.py verify   --manifest MANIFEST
    python3 -I install_permission_guard.py rollback --manifest MANIFEST
    python3 -I install_permission_guard.py promote  --manifest MANIFEST

The manifest (JSON) names everything the installer may touch:
  source            absolute path of the guard source (the committed file)
  source_sha256     its SHA-256; a mismatch refuses every mode
  runtime_root      directory that receives <HASH12>/permission_guard.py (mode 0444);
                    its parent must already exist
  settings_target   the settings.local.json that receives the hook registration
  evidence_dir      existing writable directory for preimage backups, receipts and
                    permission-guard-state.json (the latest receipt that changed state)
  registration      the hook entry template, exactly
                    {"matcher": "Bash", "hooks": [{"type": "command",
                     "command": "python3 -I RUNTIME --expect-sha256 SHA", "timeout": 10}]}

Every run prints exactly one JSON receipt as its last stdout line and exits with
  0 installed / idempotent / MATCH          1 verify DRIFT
  2 precondition (manifest, source hash, settings malformed/symlink/directory, evidence
    dir, runtime root parent, state file missing/malformed/not matching the manifest)
  3 FOREIGN_HOOK (another PermissionRequest entry present)
  4 rollback or promote refused: settings drifted from the recorded postimage
  5 runtime directory for that hash already exists (or promote with the same hash)
  6 DRIFT_BEFORE_WRITE (settings changed between the preimage and the write)
  7 write failed or interrupted (the temporary file is named in the receipt), or a
    failure after a completed write (the receipt then says settings_written /
    settings_restored and the state file records the live postimage)

The installer never deletes anything: it renames (rollback, promote) and replaces
atomically through a temporary file in the settings directory. Every path it acts on
in rollback/promote/verify is re-checked against the manifest, never trusted from the
state file alone. `--test-barrier DIR` (DIR under evidence_dir), `--test-crash-before-replace`,
`--test-short-write` and `--test-fail-after-write` are seams for the offline tests only;
each is recorded in the receipt when used.
"""

import argparse
import hashlib
import json
import os
import shutil
import stat
import sys
import tempfile
import time

EXIT_OK = 0
EXIT_VERIFY_DRIFT = 1
EXIT_PRECONDITION = 2
EXIT_FOREIGN_HOOK = 3
EXIT_STATE_DRIFT = 4
EXIT_RUNTIME_EXISTS = 5
EXIT_DRIFT_BEFORE_WRITE = 6
EXIT_WRITE_FAILED = 7

STATE_FILE = "permission-guard-state.json"
RECEIPTS_DIR = "receipts"
GUARD_FILENAME = "permission_guard.py"
TEMPLATE_COMMAND = "python3 -I RUNTIME --expect-sha256 SHA"
REQUIRED_MANIFEST_KEYS = ("source", "source_sha256", "runtime_root", "settings_target",
                          "evidence_dir", "registration")
PATH_KEYS = ("source", "runtime_root", "settings_target", "evidence_dir")
BARRIER_TIMEOUT_SECONDS = 15


class Refusal(Exception):
    def __init__(self, code, reason, **extra):
        super().__init__(reason)
        self.code = code
        self.reason = reason
        self.extra = extra


def utc_stamp():
    return time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())


def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def sha256_file(path):
    with open(path, "rb") as handle:
        return sha256_bytes(handle.read())


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


def is_hex64(value):
    return isinstance(value, str) and len(value) == 64 and all(c in "0123456789abcdef" for c in value)


def under(path, root):
    return isinstance(path, str) and path.startswith(root.rstrip("/") + "/")


def lstat_components(path, code, reason):
    """lstat every component of an absolute path; a symlink anywhere refuses."""
    current = "/"
    for part in path.split("/"):
        if not part:
            continue
        current = os.path.join(current, part)
        try:
            info = os.lstat(current)
        except FileNotFoundError:
            return
        if stat.S_ISLNK(info.st_mode):
            raise Refusal(code, reason, component=current)


# --- manifest and preconditions (11.1) -------------------------------------------------

def load_manifest(path):
    try:
        info = os.lstat(path)
    except OSError as exc:
        raise Refusal(EXIT_PRECONDITION, "MANIFEST_MISSING", detail=str(exc))
    if not stat.S_ISREG(info.st_mode):
        raise Refusal(EXIT_PRECONDITION, "MANIFEST_NOT_REGULAR")
    try:
        with open(path, "rb") as handle:
            manifest = loads_strict(handle.read())
    except (ValueError, UnicodeDecodeError) as exc:
        raise Refusal(EXIT_PRECONDITION, "MANIFEST_MALFORMED", detail=str(exc))
    if not isinstance(manifest, dict):
        raise Refusal(EXIT_PRECONDITION, "MANIFEST_NOT_OBJECT")
    for key in REQUIRED_MANIFEST_KEYS:
        if key not in manifest:
            raise Refusal(EXIT_PRECONDITION, "MANIFEST_KEY_MISSING", key=key)
    for key in PATH_KEYS:
        if not isinstance(manifest[key], str) or not os.path.isabs(manifest[key]):
            raise Refusal(EXIT_PRECONDITION, "MANIFEST_PATH_NOT_ABSOLUTE", key=key)
    if not is_hex64(manifest["source_sha256"]):
        raise Refusal(EXIT_PRECONDITION, "MANIFEST_HASH_MALFORMED")
    reg = manifest["registration"]
    ok = (isinstance(reg, dict) and set(reg) == {"matcher", "hooks"} and reg["matcher"] == "Bash"
          and isinstance(reg["hooks"], list) and len(reg["hooks"]) == 1
          and isinstance(reg["hooks"][0], dict)
          and set(reg["hooks"][0]) <= {"type", "command", "timeout"}
          and reg["hooks"][0].get("type") == "command"
          and reg["hooks"][0].get("command") == TEMPLATE_COMMAND
          and isinstance(reg["hooks"][0].get("timeout", 10), int)
          and not isinstance(reg["hooks"][0].get("timeout", 10), bool))
    if not ok:
        raise Refusal(EXIT_PRECONDITION, "MANIFEST_REGISTRATION_MALFORMED")
    return manifest


def check_source(manifest):
    source = manifest["source"]
    try:
        info = os.lstat(source)
    except OSError as exc:
        raise Refusal(EXIT_PRECONDITION, "SOURCE_MISSING", detail=str(exc))
    if not stat.S_ISREG(info.st_mode):
        raise Refusal(EXIT_PRECONDITION, "SOURCE_NOT_REGULAR")
    actual = sha256_file(source)
    if actual != manifest["source_sha256"]:
        raise Refusal(EXIT_PRECONDITION, "SOURCE_HASH_MISMATCH", actual_source_sha256=actual)
    return actual


def check_evidence_dir(manifest):
    evidence = manifest["evidence_dir"]
    if not os.path.isdir(evidence) or os.path.islink(evidence) or not os.access(evidence, os.W_OK):
        raise Refusal(EXIT_PRECONDITION, "EVIDENCE_DIR_UNUSABLE")


def check_settings_path(target):
    lstat_components(os.path.dirname(target), EXIT_PRECONDITION, "SETTINGS_PATH_SYMLINK_COMPONENT")
    directory = os.path.dirname(target)
    if not os.path.isdir(directory):
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_DIRECTORY_MISSING", directory=directory)


def read_settings(target):
    """Returns (bytes, object, stat) or (None, None, None) when the file is absent."""
    check_settings_path(target)
    try:
        info = os.lstat(target)
    except FileNotFoundError:
        return None, None, None
    if stat.S_ISLNK(info.st_mode):
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_SYMLINK")
    if stat.S_ISDIR(info.st_mode):
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_DIRECTORY")
    if not stat.S_ISREG(info.st_mode):
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_NOT_REGULAR")
    with open(target, "rb") as handle:
        data = handle.read()
    try:
        obj = loads_strict(data)
    except (ValueError, UnicodeDecodeError) as exc:
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_MALFORMED", detail=str(exc))
    if not isinstance(obj, dict):
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_NOT_OBJECT")
    return data, obj, info


def permission_request_entries(obj):
    hooks = obj.get("hooks")
    if hooks is None:
        return []
    if not isinstance(hooks, dict):
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_HOOKS_NOT_OBJECT")
    entries = hooks.get("PermissionRequest")
    if entries is None:
        return []
    if not isinstance(entries, list):
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_PERMISSION_REQUEST_NOT_LIST")
    return entries


def registration_for(manifest, runtime_path, sha):
    entry = json.loads(json.dumps(manifest["registration"]))
    # SHA first, then RUNTIME: a hex digest can never contain "RUNTIME", a path might contain "SHA"
    command = TEMPLATE_COMMAND.replace("SHA", sha).replace("RUNTIME", runtime_path)
    if command.count(runtime_path) != 1 or command.count(sha) != 1:
        raise Refusal(EXIT_PRECONDITION, "REGISTRATION_SUBSTITUTION_AMBIGUOUS", command=command)
    entry["hooks"][0]["command"] = command
    return entry, command


def entry_command(entry):
    hooks = entry.get("hooks") if isinstance(entry, dict) else None
    if isinstance(hooks, list) and hooks and isinstance(hooks[0], dict):
        return hooks[0].get("command")
    return None


def current_settings_sha(target):
    try:
        info = os.lstat(target)
    except FileNotFoundError:
        return None
    if not stat.S_ISREG(info.st_mode):
        return None
    return sha256_file(target)


def recheck_preimage(target, preimage_bytes):
    current = current_settings_sha(target)
    expected = None if preimage_bytes is None else sha256_bytes(preimage_bytes)
    if current != expected:
        raise Refusal(EXIT_DRIFT_BEFORE_WRITE, "DRIFT_BEFORE_WRITE",
                      settings_sha256=current, expected_settings_sha256=expected)


def write_atomic(target, data, mode, seams=None):
    """Temporary file in the target's directory, full write, fsync, hash check, os.replace.
    Every failure names the temporary file. Returns the temp path (consumed by the replace)."""
    seams = seams or {}
    directory = os.path.dirname(target)
    try:
        fd, temp_path = tempfile.mkstemp(prefix=".settings.local.json.tmp-", dir=directory)
    except OSError as exc:
        raise Refusal(EXIT_WRITE_FAILED, "TEMP_CREATE_FAILED", detail=str(exc))
    try:
        view = memoryview(data)
        written = 0
        while written < len(view):
            chunk = view[written:]
            if seams.get("short_write"):  # test seam: behave like a device that took half and stopped
                chunk = view[written:written + max(1, len(view) // 2)]
            count = os.write(fd, chunk)
            if count <= 0:
                raise Refusal(EXIT_WRITE_FAILED, "SHORT_WRITE", temp_path=temp_path,
                              written=written, expected=len(data))
            written += count
            if seams.get("short_write"):
                break
        os.fsync(fd)
    except OSError as exc:
        raise Refusal(EXIT_WRITE_FAILED, "TEMP_WRITE_FAILED", temp_path=temp_path, detail=str(exc))
    finally:
        os.close(fd)
    if sha256_file(temp_path) != sha256_bytes(data):
        raise Refusal(EXIT_WRITE_FAILED, "TEMP_HASH_MISMATCH", temp_path=temp_path)
    try:
        os.chmod(temp_path, mode)
    except OSError as exc:
        raise Refusal(EXIT_WRITE_FAILED, "TEMP_CHMOD_FAILED", temp_path=temp_path, detail=str(exc))
    if seams.get("crash_before_replace"):
        raise Refusal(EXIT_WRITE_FAILED, "TEST_CRASH_BEFORE_REPLACE", temp_path=temp_path)
    try:
        os.replace(temp_path, target)
    except OSError as exc:
        raise Refusal(EXIT_WRITE_FAILED, "REPLACE_FAILED", temp_path=temp_path, detail=str(exc))
    try:
        dir_fd = os.open(directory, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except OSError:
        pass  # the replace already happened; the directory fsync is best effort
    return temp_path


def runtime_paths(manifest, sha):
    runtime_dir = os.path.join(manifest["runtime_root"], sha[:12])
    return runtime_dir, os.path.join(runtime_dir, GUARD_FILENAME)


def check_runtime_root(manifest, runtime_dir):
    runtime_root = manifest["runtime_root"]
    if os.path.lexists(runtime_dir):
        raise Refusal(EXIT_RUNTIME_EXISTS, "RUNTIME_DIR_EXISTS", runtime_dir=runtime_dir)
    parent = os.path.dirname(runtime_root)
    if not os.path.isdir(parent) or os.path.islink(parent):
        raise Refusal(EXIT_PRECONDITION, "RUNTIME_ROOT_PARENT_MISSING", runtime_root=runtime_root)
    if os.path.lexists(runtime_root) and (not os.path.isdir(runtime_root) or os.path.islink(runtime_root)):
        raise Refusal(EXIT_PRECONDITION, "RUNTIME_ROOT_NOT_DIRECTORY", runtime_root=runtime_root)


def create_runtime_copy(manifest, sha, receipt):
    runtime_dir, runtime_path = runtime_paths(manifest, sha)
    check_runtime_root(manifest, runtime_dir)
    os.makedirs(runtime_dir)  # creates only runtime_root and runtime_root/<HASH12>
    receipt["runtime_copy_created"] = True
    shutil.copyfile(manifest["source"], runtime_path)
    os.chmod(runtime_path, 0o444)
    copied = sha256_file(runtime_path)
    if copied != sha:
        raise Refusal(EXIT_WRITE_FAILED, "RUNTIME_COPY_HASH_MISMATCH", runtime_dir=runtime_dir,
                      runtime_sha256=copied)
    receipt["runtime_sha256"] = sha
    return runtime_dir, runtime_path


def wait_at_barrier(manifest, barrier_dir):
    if not under(barrier_dir, manifest["evidence_dir"]) or not os.path.isdir(barrier_dir):
        raise Refusal(EXIT_PRECONDITION, "TEST_BARRIER_OUTSIDE_EVIDENCE", barrier=barrier_dir)
    with open(os.path.join(barrier_dir, "reached"), "w") as handle:
        handle.write(utc_stamp() + "\n")
    deadline = time.monotonic() + BARRIER_TIMEOUT_SECONDS
    release = os.path.join(barrier_dir, "release")
    while not os.path.exists(release):
        if time.monotonic() > deadline:
            raise Refusal(EXIT_WRITE_FAILED, "TEST_BARRIER_TIMEOUT")
        time.sleep(0.05)


def fail_after_write(seams):
    if seams.get("fail_after_write"):
        raise OSError("TEST_FAIL_AFTER_WRITE")


# --- state and receipts ----------------------------------------------------------------

def state_path(manifest):
    return os.path.join(manifest["evidence_dir"], STATE_FILE)


def load_state(manifest):
    """The latest state-changing receipt; every path in it is re-checked against the manifest."""
    path = state_path(manifest)
    try:
        with open(path, "rb") as handle:
            state = loads_strict(handle.read())
    except OSError as exc:
        raise Refusal(EXIT_PRECONDITION, "STATE_MISSING", detail=str(exc), state_file=path)
    except (ValueError, UnicodeDecodeError) as exc:
        raise Refusal(EXIT_PRECONDITION, "STATE_MALFORMED", detail=str(exc), state_file=path)
    if not isinstance(state, dict):
        raise Refusal(EXIT_PRECONDITION, "STATE_MALFORMED", state_file=path)
    installed = state.get("result") in ("INSTALLED", "PROMOTED") or (
        state.get("result") == "FAILED" and state.get("settings_written") is True)
    if not installed:
        raise Refusal(EXIT_PRECONDITION, "STATE_NOT_INSTALLED", state_file=path,
                      state_result=state.get("result"))
    if state.get("settings_target") != manifest["settings_target"]:
        raise Refusal(EXIT_PRECONDITION, "STATE_SETTINGS_TARGET_MISMATCH", state_file=path)
    runtime_dir, runtime_path = state.get("runtime_dir"), state.get("runtime_path")
    if not (under(runtime_dir, manifest["runtime_root"]) and under(runtime_path, manifest["runtime_root"])
            and runtime_path == os.path.join(runtime_dir, GUARD_FILENAME)):
        raise Refusal(EXIT_PRECONDITION, "STATE_RUNTIME_OUTSIDE_ROOT", state_file=path)
    for key in ("postimage_sha256", "runtime_sha256"):
        if not is_hex64(state.get(key)):
            raise Refusal(EXIT_PRECONDITION, "STATE_HASH_MALFORMED", state_file=path, key=key)
    if not isinstance(state.get("registration_command"), str) or state["registration_command"] == "":
        raise Refusal(EXIT_PRECONDITION, "STATE_REGISTRATION_MISSING", state_file=path)
    preimage = state.get("preimage")
    if not isinstance(preimage, dict) or preimage.get("status") not in ("ABSENT", "PRESENT"):
        raise Refusal(EXIT_PRECONDITION, "STATE_PREIMAGE_MALFORMED", state_file=path)
    if preimage["status"] == "PRESENT":
        backup = preimage.get("backup_path")
        if not under(backup, manifest["evidence_dir"]) or not is_hex64(preimage.get("sha256")):
            raise Refusal(EXIT_PRECONDITION, "STATE_PREIMAGE_OUTSIDE_EVIDENCE", state_file=path)
        try:
            info = os.lstat(backup)
        except OSError as exc:
            raise Refusal(EXIT_PRECONDITION, "PREIMAGE_BACKUP_MISSING", backup_path=backup, detail=str(exc))
        if not stat.S_ISREG(info.st_mode):
            raise Refusal(EXIT_PRECONDITION, "PREIMAGE_BACKUP_NOT_REGULAR", backup_path=backup)
    return state


def save_receipt(manifest, receipt, update_state):
    evidence = manifest["evidence_dir"]
    if not os.path.isdir(evidence):
        return
    receipts = os.path.join(evidence, RECEIPTS_DIR)
    os.makedirs(receipts, exist_ok=True)
    name = f"{receipt['timestamp_utc']}-{receipt['mode']}-{os.getpid()}.json"
    with open(os.path.join(receipts, name), "x", encoding="utf-8") as handle:
        handle.write(dumps(receipt))
    if update_state:
        write_atomic(state_path(manifest), dumps(receipt).encode("utf-8"), 0o600)


# --- modes --------------------------------------------------------------------------

def do_install(manifest, receipt, seams):
    sha = check_source(manifest)
    check_evidence_dir(manifest)
    target = manifest["settings_target"]
    runtime_dir, runtime_path = runtime_paths(manifest, sha)
    entry, command = registration_for(manifest, runtime_path, sha)
    receipt.update(runtime_dir=runtime_dir, runtime_path=runtime_path, registration_command=command)

    pre_bytes, pre_obj, pre_info = read_settings(target)
    if pre_bytes is None:
        receipt["preimage"] = {"status": "ABSENT"}
        mode = 0o644
    else:
        backup = os.path.join(manifest["evidence_dir"],
                              f"settings.local.json.preimage-{receipt['timestamp_utc']}-{os.getpid()}")
        with open(backup, "xb") as handle:
            handle.write(pre_bytes)
        mode = stat.S_IMODE(pre_info.st_mode)
        receipt["preimage"] = {"status": "PRESENT", "sha256": sha256_bytes(pre_bytes),
                               "size": pre_info.st_size, "mode": oct(mode),
                               "mtime": pre_info.st_mtime, "backup_path": backup}
    if seams.get("barrier"):
        receipt["test_barrier"] = seams["barrier"]
        wait_at_barrier(manifest, seams["barrier"])

    existing = permission_request_entries(pre_obj or {})
    if existing:
        if existing == [entry]:
            receipt.update(result="IDEMPOTENT", exit_code=EXIT_OK, hooks=pre_obj.get("hooks"),
                           postimage_sha256=sha256_bytes(pre_bytes))
            return EXIT_OK
        raise Refusal(EXIT_FOREIGN_HOOK, "FOREIGN_HOOK", existing_entries=existing)

    # build and validate the new document before anything is created on disk
    new_obj = json.loads(json.dumps(pre_obj)) if pre_obj is not None else {}
    hooks = new_obj.setdefault("hooks", {})
    if not isinstance(hooks, dict):
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_HOOKS_NOT_OBJECT")
    hooks.setdefault("PermissionRequest", []).append(entry)
    data = dumps(new_obj).encode("utf-8")

    recheck_preimage(target, pre_bytes)
    check_runtime_root(manifest, runtime_dir)
    create_runtime_copy(manifest, sha, receipt)
    recheck_preimage(target, pre_bytes)
    for name in ("crash_before_replace", "short_write", "fail_after_write"):
        if seams.get(name):
            receipt["test_" + name] = True
    temp_path = write_atomic(target, data, mode, seams)
    receipt.update(settings_written=True, temp_path=temp_path, postimage_sha256=sha256_file(target),
                   hooks=new_obj["hooks"])
    if receipt["postimage_sha256"] != sha256_bytes(data):
        raise Refusal(EXIT_WRITE_FAILED, "POSTIMAGE_MISMATCH", expected_postimage_sha256=sha256_bytes(data))
    fail_after_write(seams)
    receipt.update(result="INSTALLED", exit_code=EXIT_OK)
    return EXIT_OK


def do_verify(manifest, receipt):
    state = load_state(manifest)
    target = manifest["settings_target"]
    settings_sha = current_settings_sha(target)
    runtime_path = state["runtime_path"]
    runtime_sha = sha256_file(runtime_path) if os.path.isfile(runtime_path) and not os.path.islink(runtime_path) else None
    registration_count = None
    try:
        _, obj, _ = read_settings(target)
        if obj is not None:
            registration_count = sum(1 for e in permission_request_entries(obj)
                                     if entry_command(e) == state["registration_command"])
    except Refusal:
        registration_count = None
    receipt.update(settings_sha256=settings_sha, expected_settings_sha256=state["postimage_sha256"],
                   runtime_path=runtime_path, runtime_sha256=runtime_sha,
                   expected_runtime_sha256=state["runtime_sha256"],
                   registration_command=state["registration_command"], registration_count=registration_count)
    if (settings_sha == state["postimage_sha256"] and runtime_sha == state["runtime_sha256"]
            and registration_count == 1):
        receipt.update(result="MATCH", exit_code=EXIT_OK)
        return EXIT_OK
    receipt.update(result="DRIFT", exit_code=EXIT_VERIFY_DRIFT)
    return EXIT_VERIFY_DRIFT


def do_rollback(manifest, receipt, seams):
    state = load_state(manifest)
    target = manifest["settings_target"]
    check_settings_path(target)
    current = current_settings_sha(target)
    receipt.update(settings_sha256=current, expected_settings_sha256=state["postimage_sha256"],
                   runtime_dir=state["runtime_dir"], runtime_path=state["runtime_path"],
                   registration_command=state["registration_command"], preimage=state["preimage"])
    if current != state["postimage_sha256"]:
        raise Refusal(EXIT_STATE_DRIFT, "SETTINGS_DRIFT", settings_sha256=current,
                      expected_settings_sha256=state["postimage_sha256"])
    preimage = state["preimage"]
    stamp = receipt["timestamp_utc"]
    if preimage["status"] == "ABSENT":
        renamed = f"{target}.rolled-back-{stamp}"
        if os.path.lexists(renamed):
            raise Refusal(EXIT_WRITE_FAILED, "ROLLBACK_TARGET_EXISTS", path=renamed)
        os.rename(target, renamed)
        receipt.update(settings_restored=True, settings_renamed_to=renamed, postimage_sha256=None)
    else:
        with open(preimage["backup_path"], "rb") as handle:
            data = handle.read()
        if sha256_bytes(data) != preimage["sha256"]:
            raise Refusal(EXIT_PRECONDITION, "PREIMAGE_BACKUP_CORRUPT", backup_path=preimage["backup_path"])
        mode = int(preimage.get("mode", "0o644"), 8)
        temp_path = write_atomic(target, data, mode, seams)
        receipt.update(settings_restored=True, temp_path=temp_path, postimage_sha256=sha256_file(target))
        if receipt["postimage_sha256"] != preimage["sha256"]:
            raise Refusal(EXIT_WRITE_FAILED, "POSTIMAGE_MISMATCH", expected_postimage_sha256=preimage["sha256"])
    fail_after_write(seams)
    runtime_dir = state["runtime_dir"]
    if os.path.isdir(runtime_dir) and not os.path.islink(runtime_dir):
        disabled = f"{runtime_dir}.disabled-{stamp}"
        os.rename(runtime_dir, disabled)
        receipt["runtime_renamed_to"] = disabled
    receipt.update(result="ROLLED_BACK", exit_code=EXIT_OK, previous_receipt=state)
    return EXIT_OK


def do_promote(manifest, receipt, seams):
    state = load_state(manifest)
    sha = check_source(manifest)
    check_evidence_dir(manifest)
    target = manifest["settings_target"]
    if sha == state["runtime_sha256"]:
        raise Refusal(EXIT_RUNTIME_EXISTS, "SAME_HASH_ALREADY_INSTALLED", runtime_sha256=sha)
    pre_bytes, pre_obj, pre_info = read_settings(target)
    current = None if pre_bytes is None else sha256_bytes(pre_bytes)
    receipt.update(settings_sha256=current, expected_settings_sha256=state["postimage_sha256"],
                   preimage=state["preimage"])
    if pre_bytes is None or current != state["postimage_sha256"]:
        raise Refusal(EXIT_STATE_DRIFT, "SETTINGS_DRIFT", settings_sha256=current,
                      expected_settings_sha256=state["postimage_sha256"])
    entries = permission_request_entries(pre_obj)
    matches = [i for i, e in enumerate(entries) if entry_command(e) == state["registration_command"]]
    if len(matches) != 1:
        raise Refusal(EXIT_STATE_DRIFT, "REGISTRATION_NOT_UNIQUE", matches=len(matches))
    new_runtime_dir, new_runtime_path = runtime_paths(manifest, sha)
    _, new_command = registration_for(manifest, new_runtime_path, sha)
    receipt.update(runtime_dir=new_runtime_dir, runtime_path=new_runtime_path,
                   registration_command=new_command)
    new_obj = json.loads(json.dumps(pre_obj))
    new_obj["hooks"]["PermissionRequest"][matches[0]]["hooks"][0]["command"] = new_command
    data = dumps(new_obj).encode("utf-8")
    recheck_preimage(target, pre_bytes)
    check_runtime_root(manifest, new_runtime_dir)
    create_runtime_copy(manifest, sha, receipt)
    recheck_preimage(target, pre_bytes)
    temp_path = write_atomic(target, data, stat.S_IMODE(pre_info.st_mode), seams)
    receipt.update(settings_written=True, temp_path=temp_path, postimage_sha256=sha256_file(target),
                   hooks=new_obj["hooks"])
    if receipt["postimage_sha256"] != sha256_bytes(data):
        raise Refusal(EXIT_WRITE_FAILED, "POSTIMAGE_MISMATCH", expected_postimage_sha256=sha256_bytes(data))
    fail_after_write(seams)
    old_runtime_dir = state["runtime_dir"]
    if os.path.isdir(old_runtime_dir) and not os.path.islink(old_runtime_dir):
        superseded = f"{old_runtime_dir}.superseded-{receipt['timestamp_utc']}"
        os.rename(old_runtime_dir, superseded)
        receipt["previous_runtime_renamed_to"] = superseded
    receipt.update(result="PROMOTED", exit_code=EXIT_OK, previous_receipt=state)
    return EXIT_OK


# --- entry point ------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(description="PermissionRequest guard installer")
    parser.add_argument("mode", choices=("install", "verify", "rollback", "promote"))
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--test-barrier", default=None, help="test seam: wait for DIR/release after the preimage (DIR under evidence_dir)")
    parser.add_argument("--test-crash-before-replace", action="store_true", help="test seam: fail before os.replace")
    parser.add_argument("--test-short-write", action="store_true", help="test seam: stop after the first os.write")
    parser.add_argument("--test-fail-after-write", action="store_true", help="test seam: fail right after the settings write")
    opts = parser.parse_args()
    seams = {"barrier": opts.test_barrier, "crash_before_replace": opts.test_crash_before_replace,
             "short_write": opts.test_short_write, "fail_after_write": opts.test_fail_after_write}
    manifest_path = os.path.abspath(opts.manifest)
    receipt = {
        "mode": opts.mode,
        "timestamp_utc": utc_stamp(),
        "manifest": manifest_path,
        "installer": os.path.abspath(__file__),
        "rollback_command": f"python3 -I {os.path.abspath(__file__)} rollback --manifest {manifest_path}",
    }
    manifest = None
    code = EXIT_PRECONDITION
    try:
        manifest = load_manifest(manifest_path)
        receipt["settings_target"] = manifest["settings_target"]
        if opts.mode == "install":
            code = do_install(manifest, receipt, seams)
        elif opts.mode == "verify":
            code = do_verify(manifest, receipt)
        elif opts.mode == "rollback":
            code = do_rollback(manifest, receipt, seams)
        else:
            code = do_promote(manifest, receipt, seams)
    except Refusal as refusal:
        code = refusal.code
        receipt.update(result="FAILED" if code == EXIT_WRITE_FAILED else "REFUSED",
                       reason=refusal.reason, exit_code=code)
        receipt.update(refusal.extra)
    except Exception as exc:  # unexpected: report what was already done, never pretend success
        code = EXIT_WRITE_FAILED
        receipt.update(result="FAILED", reason="UNEXPECTED_ERROR", exit_code=code,
                       detail=f"{type(exc).__name__}: {exc}")
    changed = bool(receipt.get("settings_written") or receipt.get("settings_restored"))
    if manifest is not None and opts.mode != "verify":
        try:
            save_receipt(manifest, receipt, update_state=changed)
        except (OSError, Refusal) as exc:
            receipt.update(reason="STATE_SAVE_FAILED", receipt_save_error=str(exc), exit_code=EXIT_WRITE_FAILED)
            code = EXIT_WRITE_FAILED
    try:
        sys.stdout.write(json.dumps(receipt, ensure_ascii=False, default=str) + "\n")
        sys.stdout.flush()
    except OSError:
        pass
    return code


if __name__ == "__main__":
    sys.exit(main())
