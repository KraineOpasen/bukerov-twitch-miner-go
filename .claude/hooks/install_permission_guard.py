#!/usr/bin/env python3
"""Installer for the PermissionRequest guard (s1h-contract-rev4, section 11).

    python3 -I install_permission_guard.py install  --manifest MANIFEST
    python3 -I install_permission_guard.py verify   --manifest MANIFEST
    python3 -I install_permission_guard.py rollback --manifest MANIFEST
    python3 -I install_permission_guard.py promote  --manifest MANIFEST

The manifest (JSON) names everything the installer may touch:
  source            absolute path of the guard source (the committed file)
  source_sha256     its SHA-256; a mismatch refuses every mode
  runtime_root      directory that receives <HASH12>/permission_guard.py (mode 0444)
  settings_target   the settings.local.json that receives the hook registration
  evidence_dir      existing writable directory for preimage backups, receipts and
                    permission-guard-state.json (the latest successful receipt)
  registration      the hook entry template; "RUNTIME" and "SHA" in its command are
                    replaced by the runtime path and the guard hash

Every run prints exactly one JSON receipt as its last stdout line and exits with
  0 installed / idempotent / MATCH          1 verify DRIFT
  2 precondition (manifest, source hash, settings malformed/symlink/directory, evidence dir)
  3 FOREIGN_HOOK (another PermissionRequest entry present)
  4 rollback or promote refused: settings drifted from the recorded postimage
  5 runtime directory for that hash already exists (or promote with the same hash)
  6 DRIFT_BEFORE_WRITE (settings changed between the preimage and the write)
  7 write failed or interrupted (the temporary file is named in the receipt)

The installer never deletes anything: it renames (rollback, promote) and replaces
atomically through a temporary file in the settings directory. `--test-barrier DIR`
and `--test-crash-before-replace` are seams for the offline tests only; both are
recorded in the receipt when used.
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
    ok = (isinstance(reg, dict) and isinstance(reg.get("matcher"), str)
          and isinstance(reg.get("hooks"), list) and len(reg["hooks"]) == 1
          and isinstance(reg["hooks"][0], dict) and reg["hooks"][0].get("type") == "command"
          and isinstance(reg["hooks"][0].get("command"), str)
          and "RUNTIME" in reg["hooks"][0]["command"] and "SHA" in reg["hooks"][0]["command"])
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


def read_settings(target):
    """Returns (bytes, object, stat) or (None, None, None) when the file is absent."""
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
    command = entry["hooks"][0]["command"].replace("RUNTIME", runtime_path).replace("SHA", sha)
    entry["hooks"][0]["command"] = command
    return entry, command


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


def write_atomic(target, data, mode, crash_before_replace=False):
    """Temporary file in the target's directory, fsync, os.replace. Returns the temp path."""
    directory = os.path.dirname(target)
    fd, temp_path = tempfile.mkstemp(prefix=".settings.local.json.tmp-", dir=directory)
    try:
        os.write(fd, data)
        os.fsync(fd)
    finally:
        os.close(fd)
    os.chmod(temp_path, mode)
    if crash_before_replace:
        raise Refusal(EXIT_WRITE_FAILED, "TEST_CRASH_BEFORE_REPLACE", temp_path=temp_path)
    os.replace(temp_path, target)
    dir_fd = os.open(directory, os.O_RDONLY)
    try:
        os.fsync(dir_fd)
    finally:
        os.close(dir_fd)
    return temp_path


def create_runtime_copy(manifest, sha):
    runtime_root = manifest["runtime_root"]
    runtime_dir = os.path.join(runtime_root, sha[:12])
    runtime_path = os.path.join(runtime_dir, GUARD_FILENAME)
    if os.path.lexists(runtime_dir):
        raise Refusal(EXIT_RUNTIME_EXISTS, "RUNTIME_DIR_EXISTS", runtime_dir=runtime_dir)
    parent = os.path.dirname(runtime_root)
    if not os.path.isdir(parent) or os.path.islink(parent):
        raise Refusal(EXIT_PRECONDITION, "RUNTIME_ROOT_PARENT_MISSING", runtime_root=runtime_root)
    if os.path.lexists(runtime_root) and (not os.path.isdir(runtime_root) or os.path.islink(runtime_root)):
        raise Refusal(EXIT_PRECONDITION, "RUNTIME_ROOT_NOT_DIRECTORY", runtime_root=runtime_root)
    os.makedirs(runtime_dir)  # creates only runtime_root and runtime_root/<HASH12>
    shutil.copyfile(manifest["source"], runtime_path)
    os.chmod(runtime_path, 0o444)
    copied = sha256_file(runtime_path)
    if copied != sha:
        raise Refusal(EXIT_WRITE_FAILED, "RUNTIME_COPY_HASH_MISMATCH", runtime_dir=runtime_dir,
                      runtime_sha256=copied)
    return runtime_dir, runtime_path


def wait_at_barrier(barrier_dir):
    with open(os.path.join(barrier_dir, "reached"), "w") as handle:
        handle.write(utc_stamp() + "\n")
    deadline = time.monotonic() + BARRIER_TIMEOUT_SECONDS
    release = os.path.join(barrier_dir, "release")
    while not os.path.exists(release):
        if time.monotonic() > deadline:
            raise Refusal(EXIT_WRITE_FAILED, "TEST_BARRIER_TIMEOUT")
        time.sleep(0.05)


# --- state and receipts ----------------------------------------------------------------

def state_path(manifest):
    return os.path.join(manifest["evidence_dir"], STATE_FILE)


def load_state(manifest):
    path = state_path(manifest)
    try:
        with open(path, "rb") as handle:
            state = loads_strict(handle.read())
    except OSError as exc:
        raise Refusal(EXIT_PRECONDITION, "STATE_MISSING", detail=str(exc), state_file=path)
    except (ValueError, UnicodeDecodeError) as exc:
        raise Refusal(EXIT_PRECONDITION, "STATE_MALFORMED", detail=str(exc), state_file=path)
    if not isinstance(state, dict) or state.get("result") not in ("INSTALLED", "PROMOTED"):
        raise Refusal(EXIT_PRECONDITION, "STATE_NOT_INSTALLED", state_file=path)
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

def do_install(manifest, receipt, barrier_dir, crash_before_replace):
    sha = check_source(manifest)
    check_evidence_dir(manifest)
    target = manifest["settings_target"]
    runtime_dir = os.path.join(manifest["runtime_root"], sha[:12])
    runtime_path = os.path.join(runtime_dir, GUARD_FILENAME)
    entry, command = registration_for(manifest, runtime_path, sha)
    receipt.update(runtime_dir=runtime_dir, runtime_path=runtime_path, registration_command=command)

    pre_bytes, pre_obj, pre_info = read_settings(target)
    if pre_bytes is None:
        receipt["preimage"] = {"status": "ABSENT"}
        mode = 0o644
    else:
        backup = os.path.join(manifest["evidence_dir"], f"settings.local.json.preimage-{receipt['timestamp_utc']}-{os.getpid()}")
        with open(backup, "xb") as handle:
            handle.write(pre_bytes)
        mode = stat.S_IMODE(pre_info.st_mode)
        receipt["preimage"] = {"status": "PRESENT", "sha256": sha256_bytes(pre_bytes),
                               "size": pre_info.st_size, "mode": oct(mode),
                               "mtime": pre_info.st_mtime, "backup_path": backup}
    if barrier_dir:
        receipt["test_barrier"] = barrier_dir
        wait_at_barrier(barrier_dir)

    existing = permission_request_entries(pre_obj or {})
    if existing:
        if existing == [entry]:
            receipt.update(result="IDEMPOTENT", exit_code=EXIT_OK, hooks=pre_obj.get("hooks"),
                           postimage_sha256=sha256_bytes(pre_bytes))
            return EXIT_OK
        raise Refusal(EXIT_FOREIGN_HOOK, "FOREIGN_HOOK", existing_entries=existing)

    recheck_preimage(target, pre_bytes)
    runtime_dir, runtime_path = create_runtime_copy(manifest, sha)
    receipt["runtime_sha256"] = sha

    new_obj = json.loads(json.dumps(pre_obj)) if pre_obj is not None else {}
    hooks = new_obj.setdefault("hooks", {})
    if not isinstance(hooks, dict):
        raise Refusal(EXIT_PRECONDITION, "SETTINGS_HOOKS_NOT_OBJECT")
    hooks.setdefault("PermissionRequest", []).append(entry)
    data = dumps(new_obj).encode("utf-8")

    recheck_preimage(target, pre_bytes)
    if crash_before_replace:
        receipt["test_crash_before_replace"] = True
    temp_path = write_atomic(target, data, mode, crash_before_replace)
    receipt.update(result="INSTALLED", exit_code=EXIT_OK, temp_path=temp_path,
                   postimage_sha256=sha256_file(target), hooks=new_obj["hooks"])
    return EXIT_OK


def do_verify(manifest, receipt):
    state = load_state(manifest)
    target = manifest["settings_target"]
    settings_sha = current_settings_sha(target)
    runtime_path = state.get("runtime_path")
    runtime_sha = sha256_file(runtime_path) if runtime_path and os.path.isfile(runtime_path) else None
    receipt.update(settings_sha256=settings_sha, expected_settings_sha256=state.get("postimage_sha256"),
                   runtime_path=runtime_path, runtime_sha256=runtime_sha,
                   expected_runtime_sha256=state.get("runtime_sha256"),
                   registration_command=state.get("registration_command"))
    if settings_sha == state.get("postimage_sha256") and runtime_sha == state.get("runtime_sha256"):
        receipt.update(result="MATCH", exit_code=EXIT_OK)
        return EXIT_OK
    receipt.update(result="DRIFT", exit_code=EXIT_VERIFY_DRIFT)
    return EXIT_VERIFY_DRIFT


def do_rollback(manifest, receipt):
    state = load_state(manifest)
    target = manifest["settings_target"]
    current = current_settings_sha(target)
    receipt.update(settings_sha256=current, expected_settings_sha256=state.get("postimage_sha256"),
                   runtime_dir=state.get("runtime_dir"), runtime_path=state.get("runtime_path"))
    if current != state.get("postimage_sha256"):
        raise Refusal(EXIT_STATE_DRIFT, "SETTINGS_DRIFT", settings_sha256=current,
                      expected_settings_sha256=state.get("postimage_sha256"))
    preimage = state.get("preimage") or {}
    receipt["preimage"] = preimage
    stamp = receipt["timestamp_utc"]
    if preimage.get("status") == "ABSENT":
        renamed = f"{target}.rolled-back-{stamp}"
        os.rename(target, renamed)
        receipt["settings_renamed_to"] = renamed
    else:
        backup = preimage.get("backup_path")
        if not backup or not os.path.isfile(backup):
            raise Refusal(EXIT_PRECONDITION, "PREIMAGE_BACKUP_MISSING", backup_path=backup)
        with open(backup, "rb") as handle:
            data = handle.read()
        if sha256_bytes(data) != preimage.get("sha256"):
            raise Refusal(EXIT_PRECONDITION, "PREIMAGE_BACKUP_CORRUPT", backup_path=backup)
        mode = int(preimage.get("mode", "0o644"), 8)
        write_atomic(target, data, mode)
    runtime_dir = state.get("runtime_dir")
    if runtime_dir and os.path.isdir(runtime_dir):
        disabled = f"{runtime_dir}.disabled-{stamp}"
        os.rename(runtime_dir, disabled)
        receipt["runtime_renamed_to"] = disabled
    receipt.update(result="ROLLED_BACK", exit_code=EXIT_OK,
                   postimage_sha256=current_settings_sha(target), previous_receipt=state)
    return EXIT_OK


def do_promote(manifest, receipt):
    state = load_state(manifest)
    sha = check_source(manifest)
    check_evidence_dir(manifest)
    target = manifest["settings_target"]
    if sha == state.get("runtime_sha256"):
        raise Refusal(EXIT_RUNTIME_EXISTS, "SAME_HASH_ALREADY_INSTALLED", runtime_sha256=sha)
    pre_bytes, pre_obj, pre_info = read_settings(target)
    current = None if pre_bytes is None else sha256_bytes(pre_bytes)
    receipt.update(settings_sha256=current, expected_settings_sha256=state.get("postimage_sha256"))
    if pre_bytes is None or current != state.get("postimage_sha256"):
        raise Refusal(EXIT_STATE_DRIFT, "SETTINGS_DRIFT", settings_sha256=current,
                      expected_settings_sha256=state.get("postimage_sha256"))
    entries = permission_request_entries(pre_obj)
    old_command = state.get("registration_command")
    matches = [i for i, e in enumerate(entries)
               if isinstance(e, dict) and isinstance(e.get("hooks"), list) and e["hooks"]
               and isinstance(e["hooks"][0], dict) and e["hooks"][0].get("command") == old_command]
    if len(matches) != 1:
        raise Refusal(EXIT_STATE_DRIFT, "REGISTRATION_NOT_UNIQUE", matches=len(matches))
    new_runtime_dir = os.path.join(manifest["runtime_root"], sha[:12])
    new_runtime_path = os.path.join(new_runtime_dir, GUARD_FILENAME)
    _, new_command = registration_for(manifest, new_runtime_path, sha)
    receipt.update(runtime_dir=new_runtime_dir, runtime_path=new_runtime_path,
                   registration_command=new_command, preimage=state.get("preimage"))
    recheck_preimage(target, pre_bytes)
    create_runtime_copy(manifest, sha)
    receipt["runtime_sha256"] = sha
    new_obj = json.loads(json.dumps(pre_obj))
    new_obj["hooks"]["PermissionRequest"][matches[0]]["hooks"][0]["command"] = new_command
    data = dumps(new_obj).encode("utf-8")
    recheck_preimage(target, pre_bytes)
    temp_path = write_atomic(target, data, stat.S_IMODE(pre_info.st_mode))
    old_runtime_dir = state.get("runtime_dir")
    if old_runtime_dir and os.path.isdir(old_runtime_dir):
        superseded = f"{old_runtime_dir}.superseded-{receipt['timestamp_utc']}"
        os.rename(old_runtime_dir, superseded)
        receipt["previous_runtime_renamed_to"] = superseded
    receipt.update(result="PROMOTED", exit_code=EXIT_OK, temp_path=temp_path,
                   postimage_sha256=sha256_file(target), hooks=new_obj["hooks"],
                   previous_receipt=state)
    return EXIT_OK


# --- entry point ------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(description="PermissionRequest guard installer")
    parser.add_argument("mode", choices=("install", "verify", "rollback", "promote"))
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--test-barrier", default=None, help="test seam: wait for DIR/release after the preimage")
    parser.add_argument("--test-crash-before-replace", action="store_true", help="test seam: fail before os.replace")
    opts = parser.parse_args()
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
            code = do_install(manifest, receipt, opts.test_barrier, opts.test_crash_before_replace)
        elif opts.mode == "verify":
            code = do_verify(manifest, receipt)
        elif opts.mode == "rollback":
            code = do_rollback(manifest, receipt)
        else:
            code = do_promote(manifest, receipt)
    except Refusal as refusal:
        code = refusal.code
        receipt.update(result="FAILED" if code == EXIT_WRITE_FAILED else "REFUSED",
                       reason=refusal.reason, exit_code=code)
        receipt.update(refusal.extra)
    except Exception as exc:  # unexpected: report, never pretend success
        code = EXIT_WRITE_FAILED
        receipt.update(result="FAILED", reason="UNEXPECTED_ERROR", exit_code=code,
                       detail=f"{type(exc).__name__}: {exc}")
    if manifest is not None and opts.mode != "verify":
        try:
            save_receipt(manifest, receipt, update_state=receipt.get("result") in ("INSTALLED", "PROMOTED", "ROLLED_BACK"))
        except OSError as exc:
            receipt["receipt_save_error"] = str(exc)
    sys.stdout.write(json.dumps(receipt, ensure_ascii=False) + "\n")
    sys.stdout.flush()
    return code


if __name__ == "__main__":
    sys.exit(main())
