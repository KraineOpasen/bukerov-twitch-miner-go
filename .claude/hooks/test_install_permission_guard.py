#!/usr/bin/env python3
"""Offline tests for install_permission_guard.py (s1h-contract-rev12, sections 6.2, 11 and 7.11a (2)).

Run as a script, never through ``-m unittest``::

    python3 -I .claude/hooks/test_install_permission_guard.py -v [--installer PATH]

Every test runs the installer as a subprocess with ``--test-root`` set to a fresh directory
directly under the system temporary directory. Under it the test builds the EXACT pinned
production layout of 11.0 (clone, ~/.claude/hooks, .scratch/<RUNID>/r9-install, the pinned
manifest paths), rebased, and passes both integrity pins (``--expect-sha256`` of the installer
under test and ``--expect-manifest-sha256`` of the manifest). No test touches a production path.
Every refusal test asserts the JSON receipt's ``reason``, never only an exit code. Zero
collected tests or any skipped test fails the run.

Installer contract pinned here:
  modes install | verify | rollback | promote; manifest paths pinned per mode (11.0)
  receipt = one JSON object on stdout (the last line)
  exit codes: 0 done/idempotent/MATCH, 1 verify DRIFT/INCOMPLETE_INSTALL/NOT_INSTALLED/STATE_UNREADABLE,
              2 refused input (pin, manifest value, flag, path, unreadable state), 3 FOREIGN_HOOK,
              4 rollback/promote drift, POSTIMAGE_UNKNOWN, RUNTIME_DRIFT, 5 runtime dir exists / same hash,
              6 DRIFT_BEFORE_WRITE (codes 2-6 only before the first transaction write, rev21 V2),
              7 write failed or interrupted, or any refusal or failure after a transaction write
  state file <EVIDENCE_DIR>/permission-guard-state.json; receipts <EVIDENCE_DIR>/receipts/
  rev15 (7.11b (2)): invariant W (a completed settings write is recorded at once and never reported as
  refused), invariant A (pre-commit failures archive the new runtime dir exactly once), F3 (after the
  replacement nothing touches the runtime dir), outcome S (state before receipt; a failed state save
  is the *_STATE_NOT_SAVED outcome), failure injection only through --test-fault POINT.
"""

import argparse
import hashlib
import json
import os
import signal
import stat
import subprocess
import sys
import tempfile
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_INSTALLER = os.path.join(HERE, "install_permission_guard.py")
INSTALLER = DEFAULT_INSTALLER

CLONE = "/home/claude/bukerov-twitch-miner-go"
HOOKS_DIR = "/home/claude/.claude/hooks"
RUNID_DIR = "s1h-20261011T173421Z"
TEMPLATE_COMMAND = "python3 -I RUNTIME --expect-sha256 SHA"
REGISTRATION = {"matcher": "Bash",
                "hooks": [{"type": "command", "command": TEMPLATE_COMMAND, "timeout": 10}]}
GUARD_V1 = b"#!/usr/bin/env python3\n# guard v1\nprint()\n"
GUARD_V2 = b"#!/usr/bin/env python3\n# guard v2\nprint()\n"
OTHER_SETTINGS = {
    "permissions": {"allow": ["Bash(ls *)"], "deny": ["Bash(rm *)"],
                    "additionalDirectories": ["/home/x/obs"]},
    "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "sh /x/start.sh"}]}]},
    "enabledPlugins": {"a@synced": False},
}

# rev25 7.11f (2) Y3 / U11: the documented key set of every receipt kind (README "Receipt and state fields"), kept here
# as an independent copy of the documentation, never imported from the installer. Every printed receipt, every receipt
# file and every state file the suite produces must carry each key of its kind (null or false where it does not apply).
RECEIPT_KEYS_COMMON = ("mode", "timestamp_utc", "installer", "manifest", "result", "reason", "exit_code", "detail",
                       "installer_sha256_verified", "manifest_sha256_verified", "evidence_verified", "settings_target",
                       "evidence_dir", "rollback_command", "rollback_command_unavailable", "receipt_saved",
                       "receipt_save_error", "receipt_save_temp_path")
RECEIPT_KEYS_WRITING = ("step", "failed_step", "original_code", "original_result", "original_reason", "interrupted",
                        "interrupted_outcome", "interrupt_deferred", "interrupt_signal", "state_saved", "state_save_error",
                        "state_save_temp_path", "state_dir_fsync_error", "temp_path", "dir_fsync_error",
                        "postimage_sha256", "intended_postimage_sha256", "postimage_read_error", "preimage",
                        "runtime_dir", "runtime_path", "runtime_sha256", "registration_command", "runtime_renamed_to",
                        "post_creation_checks")
RECEIPT_KEYS_BY_MODE = {
    "install": RECEIPT_KEYS_COMMON + RECEIPT_KEYS_WRITING + (
        "preimage_backup_created", "runtime_parent_created", "runtime_dir_owned", "runtime_copy_created",
        "settings_written", "hooks", "aborted_archives_present", "runtime_archive_error"),
    "promote": RECEIPT_KEYS_COMMON + RECEIPT_KEYS_WRITING + (
        "runtime_parent_created", "runtime_dir_owned", "runtime_copy_created", "settings_written", "hooks",
        "aborted_archives_present", "runtime_archive_error", "previous_runtime_renamed_to", "previous_receipt",
        "settings_sha256", "expected_settings_sha256"),
    "rollback": RECEIPT_KEYS_COMMON + RECEIPT_KEYS_WRITING + (
        "settings_restored", "settings_renamed_to", "previous_receipt", "settings_sha256", "expected_settings_sha256"),
    "verify": RECEIPT_KEYS_COMMON + (
        "state_refusal", "registration_error", "settings_read_error", "runtime_read_error", "settings_sha256",
        "expected_settings_sha256", "intended_postimage_sha256", "postimage_unknown", "runtime_path", "runtime_sha256",
        "expected_runtime_sha256", "registration_command", "registration_count", "state_result", "state_reason",
        "runtime_dir_present", "aborted", "registration_present", "state_present"),
}
RECEIPT_ONLY_KEYS = ("receipt_saved", "receipt_save_error", "receipt_save_temp_path")
# rev27 7.11g (2) Z4: a written file states only facts complete before its own bytes were written, so the results of
# saving a file (and the state directory's fsync) are never in that file: a receipt file lacks RECEIPT_ONLY_KEYS (the
# printed receipt carries them), a state file lacks RECEIPT_ONLY_KEYS and STATE_SAVE_KEYS (the receipts carry them)
STATE_SAVE_KEYS = ("state_saved", "state_save_error", "state_save_temp_path", "state_dir_fsync_error")


def missing_documented_keys(record, kind):
    """U11: the documented keys a printed receipt ("receipt"), a receipt file ("receipt-file") or a state file ("state")
    of its mode lacks, and (rev27 Z4) every key that must not be in that file but is."""
    if not isinstance(record, dict) or record.get("mode") not in RECEIPT_KEYS_BY_MODE:
        return ["<not a receipt of a known mode>"]
    excluded = {"receipt": (), "receipt-file": RECEIPT_ONLY_KEYS, "state": RECEIPT_ONLY_KEYS + STATE_SAVE_KEYS}[kind]
    problems = [k for k in RECEIPT_KEYS_BY_MODE[record["mode"]] if k not in excluded and k not in record]
    return problems + ["excluded but present: " + k for k in excluded if k in record]


def strict_json(data):
    """Y5 oracle: parse JSON text, rejecting NaN, Infinity, -Infinity and any number that becomes a non-finite float."""
    def reject(token):
        raise ValueError("non-finite JSON constant " + token)

    def finite(text):
        value = float(text)
        if value != value or value in (float("inf"), float("-inf")):
            raise ValueError("non-finite JSON number " + text)
        return value
    return json.loads(data.decode("utf-8"), parse_constant=reject, parse_float=finite)


def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def sha256_file(path):
    with open(path, "rb") as fh:
        return sha256_bytes(fh.read())


def read_bytes(path):
    with open(path, "rb") as fh:
        return fh.read()


def doc_bytes(obj):
    return (json.dumps(obj, indent=2, ensure_ascii=False) + "\n").encode()


def snapshot(root):
    """relative path -> sha256 / 'dir' / 'symlink' for every entry under root."""
    out = {}
    for dirpath, dirnames, filenames in os.walk(root):
        for name in dirnames + filenames:
            p = os.path.join(dirpath, name)
            rel = os.path.relpath(p, root)
            if os.path.islink(p):
                out[rel] = "symlink"
            elif os.path.isdir(p):
                out[rel] = "dir"
            else:
                try:
                    out[rel] = sha256_file(p)
                except PermissionError:  # a deliberately unreadable fixture file
                    out[rel] = "unreadable:" + oct(stat.S_IMODE(os.lstat(p).st_mode))
    return out


class Fixture:
    def __init__(self):
        self.root = os.path.realpath(tempfile.mkdtemp(prefix="pginst-"))
        r = self.root
        self.clone = r + CLONE
        self.hooks_src_dir = self.clone + "/.claude/hooks"
        self.source = self.hooks_src_dir + "/permission_guard.py"
        self.settings_dir = self.clone + "/.claude"
        self.settings = self.settings_dir + "/settings.local.json"
        self.hooks_dir = r + HOOKS_DIR
        self.runtime_parent = self.hooks_dir + "/permission-guard"
        self.evidence_parent = self.clone + "/.scratch"
        self.evidence_root = self.evidence_parent + "/" + RUNID_DIR
        self.evidence_dir = self.evidence_root + "/r9-install"
        self.manifest = self.evidence_root + "/r9-install-manifest.json"
        self.promote_manifest = self.evidence_root + "/r9-promote-manifest.json"
        os.makedirs(self.hooks_src_dir)
        os.makedirs(self.hooks_dir)
        os.makedirs(self.evidence_dir)
        self.outside = r + "/outside"
        os.makedirs(self.outside)
        with open(self.outside + "/sentinel.txt", "w") as fh:
            fh.write("leave me\n")
        self.write_source(GUARD_V1)
        self.manifest_sha = self.write_manifest()
        self.collected = []  # U11: ("receipt" | "state", record) for every printed receipt and every saved state

    def collect(self, receipt):
        """U11: keep a printed receipt and, when the run saved the state, the state file as written by that run."""
        if receipt is None:
            return
        self.collected.append(("receipt", receipt))
        if receipt.get("state_saved") is True:
            self.collect_state()

    def collect_state(self):
        """U11: keep the state file exactly as the installer wrote it (for runs whose printed receipt is unavailable)."""
        with open(self.state_path(), "rb") as fh:
            self.collected.append(("state", json.loads(fh.read().decode())))

    # layout helpers -------------------------------------------------------
    def write_source(self, data):
        if os.path.exists(self.source):
            os.chmod(self.source, 0o644)
        with open(self.source, "wb") as fh:
            fh.write(data)
        self.source_sha = sha256_bytes(data)
        self.hash12 = self.source_sha[:12]

    def manifest_doc(self, overrides=None, remove=()):
        doc = {
            "source": self.source,
            "source_sha256": self.source_sha,
            "runtime_root": self.runtime_parent,
            "settings_target": self.settings,
            "evidence_dir": self.evidence_dir,
            "registration": REGISTRATION,
        }
        if overrides:
            doc.update(overrides)
        for key in remove:
            doc.pop(key, None)
        return doc

    def write_manifest(self, overrides=None, remove=(), path=None, raw=None):
        path = path or self.manifest
        data = raw if raw is not None else (json.dumps(self.manifest_doc(overrides, remove), indent=2) + "\n").encode()
        with open(path, "wb") as fh:
            fh.write(data)
        sha = sha256_bytes(data)
        if path == self.manifest:
            self.manifest_sha = sha
        return sha

    def write_settings(self, obj=None, raw=None):
        with open(self.settings, "wb") as fh:
            fh.write(raw if raw is not None else doc_bytes(obj))

    def runtime_dir(self, hash12=None):
        return self.runtime_parent + "/" + (hash12 or self.hash12)

    def runtime_path(self, hash12=None):
        return self.runtime_dir(hash12) + "/permission_guard.py"

    def expected_command(self, hash12=None, sha=None):
        return f"python3 -I {self.runtime_path(hash12)} --expect-sha256 {sha or self.source_sha}"

    def expected_entry(self, hash12=None, sha=None):
        return {"matcher": "Bash", "hooks": [{"type": "command",
                                                "command": self.expected_command(hash12, sha),
                                                "timeout": 10}]}

    def state_path(self):
        return self.evidence_dir + "/permission-guard-state.json"

    def state(self):
        with open(self.state_path()) as fh:
            return json.load(fh)

    def edit_state(self, mutate):
        st = self.state()
        mutate(st)
        with open(self.state_path(), "w") as fh:
            json.dump(st, fh)

    # running ---------------------------------------------------------------
    def run(self, mode, manifest=None, manifest_sha="auto", installer_sha="auto", extra=(),
            test_root="auto", installer=None, timeout=60, python_flags=()):
        installer = installer or INSTALLER
        manifest = manifest or (self.promote_manifest if mode == "promote" else self.manifest)
        argv = [sys.executable, "-I"] + list(python_flags) + [installer, mode, "--manifest", manifest]
        if installer_sha == "auto":
            installer_sha = sha256_file(installer)
        if installer_sha is not None:
            argv += ["--expect-sha256", installer_sha]
        if manifest_sha == "auto":
            manifest_sha = sha256_file(manifest) if os.path.isfile(manifest) else "0" * 64
        if manifest_sha is not None:
            argv += ["--expect-manifest-sha256", manifest_sha]
        if test_root == "auto":
            test_root = self.root
        if test_root is not None:
            argv += ["--test-root", test_root]
        argv += list(extra)
        proc = subprocess.run(argv, capture_output=True, cwd=self.root, timeout=timeout)
        receipt = None
        lines = [ln for ln in proc.stdout.decode().splitlines() if ln.strip()]
        if lines:
            try:
                receipt = json.loads(lines[-1])
            except ValueError:
                receipt = None
        self.collect(receipt)
        return proc.returncode, receipt, proc.stdout.decode(), proc.stderr.decode()


class InstallerTest(unittest.TestCase):
    def setUp(self):
        self.assertNotEqual(os.geteuid(), 0, "these tests must not run as root")
        self.fx = Fixture()

    def tearDown(self):
        violations = getattr(self, "retired_violations", []) + self.retire_fixture()
        self.assertEqual(violations, [], "U11: a receipt or state lacks documented keys of its kind")

    def retire_fixture(self):
        """Restore permissions, check the fixture's receipts and states (U11), remove its tree; returns the violations."""
        for root, dirs, files in os.walk(self.fx.root):
            for name in dirs + files:
                p = os.path.join(root, name)
                if not os.path.islink(p):
                    try:
                        os.chmod(p, 0o700)
                    except OSError:
                        pass
        subprocess.run(["chmod", "-R", "u+w", self.fx.root], capture_output=True)
        try:
            return self.documented_key_violations()
        finally:
            import shutil
            shutil.rmtree(self.fx.root, ignore_errors=True)

    def next_fixture(self):
        """A fresh pinned layout for the next case of a grid test; the current one is checked (U11) and removed first."""
        self.retired_violations = getattr(self, "retired_violations", []) + self.retire_fixture()
        self.fx = Fixture()

    def documented_key_violations(self, fx=None):
        """U11 (rev25 Y3, rev27 Z4): every printed receipt and saved state this test produced, and every final-named
        receipt file under receipts/, carries the documented key set of its kind and no key excluded from it."""
        fx = fx or self.fx
        found = []
        for kind, record in fx.collected:
            missing = missing_documented_keys(record, kind)
            if missing:
                found.append((kind, (record or {}).get("mode"), (record or {}).get("result"), missing))
        directory = fx.evidence_dir + "/receipts"
        if os.path.isdir(directory):
            for name in sorted(os.listdir(directory)):
                if name.startswith("."):  # a leftover temporary receipt is not authoritative (README)
                    continue
                with open(directory + "/" + name, "rb") as fh:
                    record = json.loads(fh.read().decode())
                missing = missing_documented_keys(record, "receipt-file")
                if missing:
                    found.append(("receipt file " + name, record.get("mode"), record.get("result"), missing))
        return found

    def check_twin(self, twin):
        """U11 for a second fixture a test builds itself: its receipts and states join this test's check."""
        self.retired_violations = getattr(self, "retired_violations", []) + self.documented_key_violations(twin)

    @staticmethod
    def as_saved(receipt):
        """rev27 Z4: the printed receipt as its receipt file holds it (without the three receipt-save keys)."""
        return {k: v for k, v in receipt.items() if k not in RECEIPT_ONLY_KEYS}

    # helpers ---------------------------------------------------------------
    def install_ok(self):
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 0, f"install failed rc={rc} out={out!r} err={err!r}")
        self.assertIsNotNone(receipt, f"no JSON receipt in {out!r}")
        self.assertEqual(receipt["result"], "INSTALLED")
        return receipt

    def assert_refused(self, result, code, reason):
        rc, receipt, out, err = result
        self.assertIsNotNone(receipt, f"no JSON receipt: out={out!r} err={err!r}")
        self.assertEqual(receipt.get("reason"), reason, receipt)
        self.assertEqual(rc, code, receipt)
        return receipt

    def assert_verify(self, result, state_refusal=None):
        """verify writes nothing and answers by the 11.3 precedence; every non-MATCH outcome exits 1."""
        before = snapshot(self.fx.root)
        rc, receipt, out, err = self.fx.run("verify")
        self.assertIsNotNone(receipt, (out, err))
        self.assertEqual((rc, receipt.get("result")), (0 if result == "MATCH" else 1, result), receipt)
        if state_refusal is not None:
            self.assertEqual(receipt.get("state_refusal"), state_refusal, receipt)
        self.assertEqual(before, snapshot(self.fx.root))
        return receipt

    def settings_obj(self):
        return json.loads(read_bytes(self.fx.settings).decode())

    def unchanged_except_evidence(self, before):
        after = snapshot(self.fx.root)
        strip = lambda snap: {k: v for k, v in snap.items() if "/r9-install" not in "/" + k}
        self.assertEqual(strip(before), strip(after))

    def write_promote_manifest(self):
        return self.fx.write_manifest(path=self.fx.promote_manifest)

    # 6.2 install -----------------------------------------------------------
    def test_6_2_absent_settings_created_hooks_only_and_runtime_parent_created(self):
        self.assertFalse(os.path.exists(self.fx.runtime_parent))
        receipt = self.install_ok()
        self.assertEqual(list(self.settings_obj()), ["hooks"])
        self.assertEqual(self.settings_obj()["hooks"], {"PermissionRequest": [self.fx.expected_entry()]})
        self.assertEqual(receipt["preimage"]["status"], "ABSENT")
        self.assertEqual(receipt["postimage_sha256"], sha256_file(self.fx.settings))
        self.assertEqual(receipt["postimage_sha256"],
                         sha256_bytes(doc_bytes({"hooks": {"PermissionRequest": [self.fx.expected_entry()]}})))
        self.assertEqual(receipt["runtime_path"], self.fx.runtime_path())
        self.assertEqual(receipt["runtime_sha256"], self.fx.source_sha)
        self.assertEqual(receipt["registration_command"], self.fx.expected_command())
        self.assertTrue(os.path.isdir(self.fx.runtime_parent))
        self.assertEqual(receipt["test_root"], self.fx.root)

    def test_6_2_install_receipt_records_both_verified_pins(self):
        receipt = self.install_ok()
        self.assertEqual(receipt["installer_sha256_verified"], sha256_file(INSTALLER))
        self.assertEqual(receipt["manifest_sha256_verified"], self.fx.manifest_sha)

    def test_6_2_state_and_receipts_land_in_the_pinned_evidence_dir(self):
        self.assertFalse(os.path.exists(self.fx.evidence_dir + "/receipts"))
        self.install_ok()
        self.assertTrue(os.path.isfile(self.fx.state_path()))
        receipts = os.listdir(self.fx.evidence_dir + "/receipts")
        self.assertEqual(len([n for n in receipts if "-install-" in n]), 1)

    def test_6_2_runtime_copy_bytes_and_mode(self):
        self.install_ok()
        self.assertEqual(read_bytes(self.fx.runtime_path()), GUARD_V1)
        self.assertEqual(stat.S_IMODE(os.stat(self.fx.runtime_path()).st_mode), 0o444)

    def test_6_2_existing_settings_merged_other_keys_preserved_in_order(self):
        self.fx.write_settings(OTHER_SETTINGS)
        preimage = read_bytes(self.fx.settings)
        receipt = self.install_ok()
        obj = self.settings_obj()
        self.assertEqual(list(obj), list(OTHER_SETTINGS))
        for key in OTHER_SETTINGS:
            if key != "hooks":
                self.assertEqual(obj[key], OTHER_SETTINGS[key])
        self.assertEqual(list(obj["hooks"]), ["SessionStart", "PermissionRequest"])
        self.assertEqual(obj["hooks"]["PermissionRequest"], [self.fx.expected_entry()])
        self.assertEqual(receipt["preimage"]["status"], "PRESENT")
        self.assertEqual(receipt["preimage"]["sha256"], sha256_bytes(preimage))
        self.assertEqual(os.path.dirname(receipt["preimage"]["backup_path"]), self.fx.evidence_dir)
        self.assertEqual(read_bytes(receipt["preimage"]["backup_path"]), preimage)

    def test_6_2_identical_registration_is_noop(self):
        self.install_ok()
        before = snapshot(self.fx.root)
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual((rc, receipt["result"]), (0, "IDEMPOTENT"), receipt)
        self.unchanged_except_evidence(before)

    def test_6_2_foreign_permission_request_entry_refused(self):
        self.fx.write_settings({"hooks": {"PermissionRequest": [{"matcher": "Bash", "hooks": [
            {"type": "command", "command": "echo other"}]}]}})
        before = snapshot(self.fx.root)
        self.assert_refused(self.fx.run("install"), 3, "FOREIGN_HOOK")
        self.unchanged_except_evidence(before)
        self.assertFalse(os.path.exists(self.fx.runtime_parent))

    def test_6_2_malformed_settings_refused(self):
        self.fx.write_settings(raw=b'{"hooks": ')
        self.assert_refused(self.fx.run("install"), 2, "SETTINGS_MALFORMED")
        self.assertEqual(read_bytes(self.fx.settings), b'{"hooks": ')
        self.assertFalse(os.path.exists(self.fx.runtime_parent))

    def test_6_2_settings_symlink_refused(self):
        real = self.fx.outside + "/real.json"
        with open(real, "w") as fh:
            fh.write("{}\n")
        os.symlink(real, self.fx.settings)
        self.assert_refused(self.fx.run("install"), 2, "SETTINGS_SYMLINK")
        self.assertEqual(read_bytes(real), b"{}\n")

    def test_6_2_settings_directory_refused(self):
        os.mkdir(self.fx.settings)
        self.assert_refused(self.fx.run("install"), 2, "SETTINGS_DIRECTORY")

    def test_6_2_preimage_changed_before_write_is_a_failure_after_the_preimage_copy(self):
        """Rewritten for rev21 (7.11d (2) V2): the drift is injected at the barrier, after the preimage copy, which is a
        transaction write; a refusal after a transaction write is FAILED / exit 7 with its own code kept in original_code
        (it asserted REFUSED / exit 6 before). The backup is kept and nothing else is written."""
        self.fx.write_settings(OTHER_SETTINGS)
        barrier = self.fx.root + "/barrier"
        os.mkdir(barrier)
        argv = [sys.executable, "-I", INSTALLER, "install", "--manifest", self.fx.manifest,
                "--expect-sha256", sha256_file(INSTALLER), "--expect-manifest-sha256", self.fx.manifest_sha,
                "--test-root", self.fx.root, "--test-barrier", barrier]
        proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.fx.root)
        deadline = time.time() + 20
        while not os.path.exists(barrier + "/reached"):
            if time.time() > deadline or proc.poll() is not None:
                proc.kill()
                self.fail("installer never reached the test barrier")
            time.sleep(0.05)
        mutated = dict(OTHER_SETTINGS, permissions={"allow": ["Bash(cat *)"]})
        self.fx.write_settings(mutated)
        mutated_bytes = read_bytes(self.fx.settings)
        with open(barrier + "/release", "w") as fh:
            fh.write("go\n")
        out, err = proc.communicate(timeout=30)
        receipt = json.loads([ln for ln in out.decode().splitlines() if ln.strip()][-1])
        self.fx.collect(receipt)  # U11 (R5d-b-6)
        self.assertEqual(receipt["reason"], "DRIFT_BEFORE_WRITE", err.decode())
        self.assertEqual((proc.returncode, receipt["result"], receipt["original_code"]), (7, "FAILED", 6), receipt)
        self.assertEqual(receipt["preimage_backup_created"], receipt["preimage"]["backup_path"])
        self.assertIn("runtime_renamed_to", receipt)  # R5b-c-3: README says null when no <HASH12> was created
        self.assertIsNone(receipt["runtime_renamed_to"])
        self.assertEqual(read_bytes(self.fx.settings), mutated_bytes)
        self.assertEqual(read_bytes(receipt["preimage"]["backup_path"]), doc_bytes(OTHER_SETTINGS))
        self.assertFalse(os.path.exists(self.fx.runtime_dir()))

    def test_6_2_crash_before_replace_keeps_settings_and_archives_runtime(self):
        self.fx.write_settings(OTHER_SETTINGS)
        original = read_bytes(self.fx.settings)
        receipt = self.assert_refused(self.fx.run("install", extra=["--test-crash-before-replace"]),
                                      7, "TEST_CRASH_BEFORE_REPLACE")
        self.assertEqual(receipt["result"], "FAILED")
        self.assertEqual(read_bytes(self.fx.settings), original)
        self.assertTrue(os.path.exists(receipt["temp_path"]))
        self.assertEqual(os.path.dirname(receipt["temp_path"]), self.fx.settings_dir)
        self.assertFalse(os.path.exists(self.fx.runtime_dir()))
        archived = receipt["runtime_renamed_to"]
        self.assertEqual(os.path.dirname(archived), self.fx.runtime_parent)
        self.assertTrue(os.path.basename(archived).startswith(self.fx.hash12 + ".aborted-"))
        self.assertEqual(read_bytes(archived + "/permission_guard.py"), GUARD_V1)

    def test_6_2_short_write_refused_with_temp_named(self):
        self.fx.write_settings(OTHER_SETTINGS)
        original = read_bytes(self.fx.settings)
        receipt = self.assert_refused(self.fx.run("install", extra=["--test-short-write"]), 7, "TEMP_HASH_MISMATCH")
        self.assertEqual(read_bytes(self.fx.settings), original)
        self.assertTrue(os.path.exists(receipt["temp_path"]))
        self.assertIs(receipt["settings_written"], False)  # rev25 Y3/Y4: documented, false until the write completed

    def test_6_2_simulated_abrupt_kill_orphan_is_incomplete_install(self):
        os.makedirs(self.fx.runtime_dir())
        with open(self.fx.runtime_path(), "wb") as fh:
            fh.write(GUARD_V1)
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual(rc, 1, receipt)
        self.assertEqual(receipt["result"], "INCOMPLETE_INSTALL")
        self.assert_refused(self.fx.run("install"), 5, "RUNTIME_DIR_EXISTS")
        self.assertFalse(os.path.exists(self.fx.settings))

    def test_6_2_aborted_archive_without_receipt_is_incomplete_install(self):
        archive = self.fx.runtime_parent + "/" + self.fx.hash12 + ".aborted-20261014T000000Z-1"
        os.makedirs(archive)
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual(rc, 1, receipt)
        self.assertEqual(receipt["result"], "INCOMPLETE_INSTALL")
        # rev15 R3-A2 / 13.2 (e): an installer-owned archive never blocks a later install, and a valid
        # state decides verify (precedence step 2) even while the archive remains
        self.install_ok()
        self.assertTrue(os.path.isdir(archive))
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (0, "MATCH"), receipt)

    def test_6_2_verify_without_state_is_not_installed(self):
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual(rc, 1, (out, err))
        self.assertEqual(receipt["result"], "NOT_INSTALLED")
        self.assertNotIn("Traceback", err)

    def test_6_2_source_hash_mismatch_refused(self):
        self.fx.write_manifest(overrides={"source_sha256": "0" * 64})
        self.assert_refused(self.fx.run("install"), 2, "SOURCE_HASH_MISMATCH")
        self.assertFalse(os.path.exists(self.fx.settings))

    def test_6_2_manifest_missing_or_malformed_refused(self):
        os.unlink(self.fx.manifest)
        self.assert_refused(self.fx.run("install", manifest_sha="0" * 64), 2, "MANIFEST_MISSING")
        sha = self.fx.write_manifest(raw=b"{not json")
        self.assert_refused(self.fx.run("install", manifest_sha=sha), 2, "MANIFEST_MALFORMED")

    def test_6_2_manifest_unknown_key_refused(self):
        self.fx.write_manifest(overrides={"extra": 1})
        self.assert_refused(self.fx.run("install"), 2, "MANIFEST_UNKNOWN_KEY")

    # 11.3 verify -----------------------------------------------------------
    def test_11_3_verify_match_then_drift(self):
        installed = self.install_ok()
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (0, "MATCH"), receipt)
        with open(self.fx.settings, "a") as fh:
            fh.write("\n")
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (1, "DRIFT"), receipt)
        self.assertEqual(receipt["settings_sha256"], sha256_file(self.fx.settings))
        self.assertEqual(receipt["expected_settings_sha256"], installed["postimage_sha256"])
        self.assertNotEqual(receipt["settings_sha256"], receipt["expected_settings_sha256"])

    def test_11_3_verify_runtime_drift(self):
        self.install_ok()
        os.chmod(self.fx.runtime_path(), 0o644)
        with open(self.fx.runtime_path(), "ab") as fh:
            fh.write(b"# tampered\n")
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (1, "DRIFT"), receipt)
        self.assertEqual(receipt["runtime_sha256"], sha256_file(self.fx.runtime_path()))
        self.assertEqual(receipt["expected_runtime_sha256"], self.fx.source_sha)
        self.assertEqual(receipt["settings_sha256"], receipt["expected_settings_sha256"])

    def test_11_3_verify_never_writes(self):
        self.install_ok()
        before = snapshot(self.fx.root)
        self.fx.run("verify")
        self.assertEqual(before, snapshot(self.fx.root))

    # 11.4 rollback -----------------------------------------------------------
    def test_11_4_rollback_restores_present_preimage_and_mode(self):
        self.fx.write_settings(OTHER_SETTINGS)
        os.chmod(self.fx.settings, 0o600)
        preimage = read_bytes(self.fx.settings)
        self.install_ok()
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual((rc, receipt["result"]), (0, "ROLLED_BACK"), receipt)
        self.assertEqual(read_bytes(self.fx.settings), preimage)
        self.assertEqual(stat.S_IMODE(os.stat(self.fx.settings).st_mode), 0o600)
        disabled = [d for d in os.listdir(self.fx.runtime_parent) if d.startswith(self.fx.hash12 + ".disabled-")]
        self.assertEqual(len(disabled), 1)

    def test_11_4_rollback_absent_preimage_renames_never_deletes(self):
        self.install_ok()
        installed = read_bytes(self.fx.settings)
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual(rc, 0, receipt)
        self.assertFalse(os.path.exists(self.fx.settings))
        rolled = [f for f in os.listdir(self.fx.settings_dir) if f.startswith("settings.local.json.rolled-back-")]
        self.assertEqual(len(rolled), 1)
        self.assertEqual(read_bytes(self.fx.settings_dir + "/" + rolled[0]), installed)

    def test_11_4_rollback_with_drift_refused(self):
        self.fx.write_settings(OTHER_SETTINGS)
        self.install_ok()
        with open(self.fx.settings, "a") as fh:
            fh.write("\n")
        before = snapshot(self.fx.root)
        self.assert_refused(self.fx.run("rollback"), 4, "SETTINGS_DRIFT")
        self.unchanged_except_evidence(before)

    def test_11_4_fail_after_write_is_reported_and_rollback_still_works(self):
        self.fx.write_settings(OTHER_SETTINGS)
        rc, receipt, out, err = self.fx.run("install", extra=["--test-fail-after-write"])
        self.assertEqual(rc, 7, receipt)
        self.assertTrue(receipt["settings_written"])
        self.assertTrue(self.fx.state()["settings_written"])
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (0, "MATCH"), receipt)
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual(rc, 0, receipt)
        self.assertEqual(read_bytes(self.fx.settings), doc_bytes(OTHER_SETTINGS))

    # 11.5 promote ------------------------------------------------------------
    def test_11_5_promote_replaces_registration_once_and_chains(self):
        self.fx.write_settings(OTHER_SETTINGS)
        first = self.install_ok()
        old_hash12 = self.fx.hash12
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        rc, receipt, out, err = self.fx.run("promote")
        self.assertEqual((rc, receipt["result"]), (0, "PROMOTED"), receipt)
        self.assertEqual(self.settings_obj()["hooks"]["PermissionRequest"], [self.fx.expected_entry()])
        self.assertEqual(read_bytes(self.fx.runtime_path()), GUARD_V2)
        superseded = [d for d in os.listdir(self.fx.runtime_parent) if d.startswith(old_hash12 + ".superseded-")]
        self.assertEqual(len(superseded), 1)
        self.assertEqual(receipt["previous_receipt"]["postimage_sha256"], first["postimage_sha256"])

    def test_11_5_promote_same_hash_refused_nothing_written(self):
        self.install_ok()
        self.write_promote_manifest()
        before = snapshot(self.fx.root)
        self.assert_refused(self.fx.run("promote"), 5, "SAME_HASH_ALREADY_INSTALLED")
        self.unchanged_except_evidence(before)

    def test_11_5_promote_refuses_the_install_manifest_path(self):
        self.install_ok()
        self.assert_refused(self.fx.run("promote", manifest=self.fx.manifest), 2, "MANIFEST_PATH_NOT_PINNED")

    def test_11_0_install_refuses_the_promote_manifest_path(self):
        self.write_promote_manifest()
        self.assert_refused(self.fx.run("install", manifest=self.fx.promote_manifest), 2, "MANIFEST_PATH_NOT_PINNED")
        self.assertFalse(os.path.exists(self.fx.settings))

    # 11.7 integrity pins -------------------------------------------------------
    def test_11_7_correct_pins_accepted_in_verify(self):
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (1, "NOT_INSTALLED"), receipt)
        self.assertEqual(receipt["installer_sha256_verified"], sha256_file(INSTALLER))
        self.assertEqual(receipt["manifest_sha256_verified"], self.fx.manifest_sha)

    def test_11_7_missing_or_wrong_installer_pin_refused_in_every_mode(self):
        self.write_promote_manifest()
        for mode in ("install", "verify", "rollback", "promote"):
            self.assert_refused(self.fx.run(mode, installer_sha=None), 2, "INSTALLER_HASH_MISSING")
            self.assert_refused(self.fx.run(mode, installer_sha="f" * 64), 2, "INSTALLER_HASH_MISMATCH")
        self.assertFalse(os.path.exists(self.fx.settings))

    def test_11_7_missing_or_wrong_manifest_pin_refused_in_every_mode(self):
        self.write_promote_manifest()
        for mode in ("install", "verify", "rollback", "promote"):
            self.assert_refused(self.fx.run(mode, manifest_sha=None), 2, "MANIFEST_HASH_MISSING")
            self.assert_refused(self.fx.run(mode, manifest_sha="f" * 64), 2, "MANIFEST_HASH_MISMATCH")
        self.assertFalse(os.path.exists(self.fx.settings))

    def test_11_7_wrong_installer_pin_refused_before_the_manifest_is_read(self):
        os.unlink(self.fx.manifest)
        self.assert_refused(self.fx.run("install", installer_sha="f" * 64, manifest_sha="0" * 64), 2,
                            "INSTALLER_HASH_MISMATCH")

    def test_11_7_wrong_manifest_pin_refused_before_json_parse(self):
        self.fx.write_manifest(raw=b"{not json")
        self.assert_refused(self.fx.run("install", manifest_sha="f" * 64), 2, "MANIFEST_HASH_MISMATCH")

    def test_11_7_tampered_self_consistent_manifest_refused_by_manifest_pin(self):
        frozen_sha = self.fx.manifest_sha
        self.fx.write_source(b"#!/usr/bin/env python3\n# altered guard\n")
        self.fx.write_manifest()  # source_sha256 now matches the altered guard
        self.assert_refused(self.fx.run("install", manifest_sha=frozen_sha), 2, "MANIFEST_HASH_MISMATCH")
        self.assertFalse(os.path.exists(self.fx.runtime_parent))

    def test_13_2e_correct_install_after_a_refused_one_succeeds(self):
        self.assert_refused(self.fx.run("install", manifest_sha="f" * 64), 2, "MANIFEST_HASH_MISMATCH")
        self.install_ok()

    # 11.0 boundary: manifest values pinned in code -------------------------------
    def test_11_0_manifest_value_outside_each_pinned_destination_refused(self):
        cases = {
            "source": (self.fx.outside + "/permission_guard.py", "MANIFEST_SOURCE_NOT_PINNED"),
            "runtime_root": (self.fx.outside, "MANIFEST_RUNTIME_ROOT_NOT_PINNED"),
            "settings_target": (self.fx.outside + "/settings.local.json", "MANIFEST_SETTINGS_TARGET_NOT_PINNED"),
            "evidence_dir": (self.fx.outside, "MANIFEST_EVIDENCE_DIR_NOT_PINNED"),
        }
        with open(self.fx.outside + "/permission_guard.py", "wb") as fh:
            fh.write(GUARD_V1)
        for key, (value, reason) in cases.items():
            before = snapshot(self.fx.root)
            self.fx.write_manifest(overrides={key: value})
            self.assert_refused(self.fx.run("install"), 2, reason)
            after = snapshot(self.fx.root)
            changed = {k for k in set(before) | set(after) if before.get(k) != after.get(k)}
            self.assertTrue(changed <= {os.path.relpath(self.fx.manifest, self.fx.root)}, (key, changed))

    def test_11_0_evidence_dir_other_runid_sibling_prefix_and_other_child_refused(self):
        other_runid = self.fx.evidence_parent + "/s1h-20991231T000000Z/r9-install"
        sibling = self.fx.evidence_root + "-evil/r9-install"
        other_child = self.fx.evidence_root + "/other"
        for d in (other_runid, sibling, other_child):
            os.makedirs(d)
            with open(d + "/sentinel.txt", "w") as fh:
                fh.write("sentinel\n")
        for d in (other_runid, sibling, other_child):
            self.fx.write_manifest(overrides={"evidence_dir": d})
            self.assert_refused(self.fx.run("install"), 2, "MANIFEST_EVIDENCE_DIR_NOT_PINNED")
            self.assertEqual(sorted(os.listdir(d)), ["sentinel.txt"])
            self.assertEqual(read_bytes(d + "/sentinel.txt"), b"sentinel\n")
        self.assertFalse(os.path.exists(self.fx.settings))

    def test_11_0_evidence_dir_with_dotdot_refused_in_every_mode(self):
        self.write_promote_manifest()
        dotted = self.fx.evidence_dir + "/../r9-install"
        for mode in ("install", "verify", "rollback", "promote"):
            path = self.fx.promote_manifest if mode == "promote" else self.fx.manifest
            self.fx.write_manifest(overrides={"evidence_dir": dotted}, path=path)
            before = snapshot(self.fx.root)
            self.assert_refused(self.fx.run(mode), 2, "MANIFEST_EVIDENCE_DIR_NOT_PINNED")
            self.assertEqual(before, snapshot(self.fx.root), mode)

    def test_11_0_registration_differing_from_template_refused(self):
        reg = json.loads(json.dumps(REGISTRATION))
        reg["hooks"][0]["onFailure"] = "block"
        self.fx.write_manifest(overrides={"registration": reg})
        self.assert_refused(self.fx.run("install"), 2, "MANIFEST_REGISTRATION_NOT_TEMPLATE")
        reg = json.loads(json.dumps(REGISTRATION))
        reg["hooks"][0]["command"] = "python3 RUNTIME --expect-sha256 SHA"
        self.fx.write_manifest(overrides={"registration": reg})
        self.assert_refused(self.fx.run("install"), 2, "MANIFEST_REGISTRATION_NOT_TEMPLATE")

    def test_11_0_runtime_root_sibling_prefix_refused(self):
        os.makedirs(self.fx.runtime_parent + "-evil")
        self.fx.write_manifest(overrides={"runtime_root": self.fx.runtime_parent + "-evil"})
        before = snapshot(self.fx.root)
        self.assert_refused(self.fx.run("install"), 2, "MANIFEST_RUNTIME_ROOT_NOT_PINNED")
        self.assertEqual(before, snapshot(self.fx.root))

    # 7.11a (2) containment -------------------------------------------------------
    def test_ct_hooks_dir_absent_refused(self):
        os.rmdir(self.fx.hooks_dir)
        self.assert_refused(self.fx.run("install"), 2, "HOOKS_DIR_NOT_REAL_DIRECTORY")
        self.assertFalse(os.path.exists(self.fx.settings))

    def test_ct_hooks_dir_symlink_refused(self):
        os.rmdir(self.fx.hooks_dir)
        os.symlink(self.fx.outside, self.fx.hooks_dir)
        self.assert_refused(self.fx.run("install"), 2, "HOOKS_DIR_NOT_REAL_DIRECTORY")
        self.assertEqual(sorted(os.listdir(self.fx.outside)), ["sentinel.txt"])

    def test_ct_hooks_dir_is_a_file_refused(self):
        os.rmdir(self.fx.hooks_dir)
        with open(self.fx.hooks_dir, "w") as fh:
            fh.write("x")
        self.assert_refused(self.fx.run("install"), 2, "HOOKS_DIR_NOT_REAL_DIRECTORY")

    def test_ct_runtime_parent_symlink_refused(self):
        os.symlink(self.fx.outside, self.fx.runtime_parent)
        self.assert_refused(self.fx.run("install"), 2, "RUNTIME_PARENT_NOT_REAL_DIRECTORY")
        self.assertEqual(sorted(os.listdir(self.fx.outside)), ["sentinel.txt"])

    def test_ct_symlinked_intermediate_evidence_directory_refused(self):
        """EVIDENCE_ROOT (between EVIDENCE_PARENT and EVIDENCE_DIR) replaced by a symlink to a look-alike tree."""
        elsewhere = self.fx.outside + "/evidence-root"
        os.rename(self.fx.evidence_root, elsewhere)
        os.symlink(elsewhere, self.fx.evidence_root)
        before = snapshot(elsewhere)
        self.assert_refused(self.fx.run("install"), 2, "EVIDENCE_ROOT_NOT_REAL_DIRECTORY")
        self.assertEqual(before, snapshot(elsewhere))
        self.assertFalse(os.path.exists(self.fx.settings))

    def test_ct_symlinked_settings_directory_refused(self):
        """CLONE/.claude replaced by a symlink to a copy: the source and settings roots are not real directories."""
        elsewhere = self.fx.outside + "/claude-dir"
        os.rename(self.fx.settings_dir, elsewhere)
        os.symlink(elsewhere, self.fx.settings_dir)
        before = snapshot(elsewhere)
        self.assert_refused(self.fx.run("install"), 2, "SOURCE_NOT_CONTAINED")
        self.assertEqual(before, snapshot(elsewhere))

    def test_ct_evidence_dir_missing_refused(self):
        os.rmdir(self.fx.evidence_dir)
        self.assert_refused(self.fx.run("install"), 2, "EVIDENCE_DIR_NOT_REAL_DIRECTORY")

    def test_ct_evidence_dir_symlink_refused(self):
        os.rmdir(self.fx.evidence_dir)
        os.symlink(self.fx.outside, self.fx.evidence_dir)
        self.assert_refused(self.fx.run("install"), 2, "EVIDENCE_DIR_NOT_REAL_DIRECTORY")
        self.assertEqual(sorted(os.listdir(self.fx.outside)), ["sentinel.txt"])

    def test_ct_symlinked_state_file_refused(self):
        self.install_ok()
        elsewhere = self.fx.outside + "/state.json"
        os.rename(self.fx.state_path(), elsewhere)
        os.symlink(elsewhere, self.fx.state_path())
        installed = read_bytes(self.fx.settings)
        # verify: 11.3 step 1, a state file that exists but cannot be used is STATE_UNREADABLE (never acted on)
        self.assert_verify("STATE_UNREADABLE", state_refusal="STATE_NOT_REGULAR")
        for mode in ("rollback", "promote"):
            if mode == "promote":
                self.fx.write_source(GUARD_V2)
                self.write_promote_manifest()
            self.assert_refused(self.fx.run(mode), 2, "STATE_NOT_REGULAR")
            self.assertEqual(read_bytes(self.fx.settings), installed, mode)
        self.assertFalse(os.path.exists(self.fx.runtime_dir()))  # no v2 runtime dir was created

    def test_ct_state_runtime_dir_traversal_refused_in_verify_rollback_promote(self):
        self.install_ok()
        victim = self.fx.hooks_dir + "/victim"
        os.mkdir(victim)
        for forged in (self.fx.runtime_parent + "/../victim",
                       self.fx.runtime_parent + "/" + self.fx.hash12 + "/../../victim",
                       self.fx.runtime_parent + "-evil/" + self.fx.hash12):
            def mutate(st, forged=forged):
                st["runtime_dir"] = forged
                st["runtime_path"] = forged + "/permission_guard.py"
            self.fx.edit_state(mutate)
            # verify: 11.3 step 3, no trustworthy state but a registration and runtime remain
            self.assert_verify("INCOMPLETE_INSTALL", state_refusal="STATE_RUNTIME_NOT_PINNED")
            self.assert_refused(self.fx.run("rollback"), 2, "STATE_RUNTIME_NOT_PINNED")
            self.fx.write_source(GUARD_V2)
            self.write_promote_manifest()
            self.assert_refused(self.fx.run("promote"), 2, "STATE_RUNTIME_NOT_PINNED")
            self.fx.write_source(GUARD_V1)
            self.assertTrue(os.path.isdir(victim))

    def test_ct_state_preimage_backup_traversal_refused(self):
        self.fx.write_settings(OTHER_SETTINGS)
        self.install_ok()
        installed = read_bytes(self.fx.settings)
        forged = self.fx.evidence_root + "/settings.local.json.preimage-forged"
        with open(forged, "w") as fh:
            fh.write('{"permissions": {"allow": ["Bash(*)"]}}\n')
        for path in (self.fx.evidence_dir + "/../settings.local.json.preimage-forged", forged,
                     self.fx.evidence_dir + "/forged.json"):
            def mutate(st, path=path):
                st["preimage"] = {"status": "PRESENT", "sha256": sha256_file(forged), "backup_path": path,
                                  "mode": "0o644"}
            self.fx.edit_state(mutate)
            self.assert_refused(self.fx.run("rollback"), 2, "STATE_PREIMAGE_NOT_PINNED")
            self.assertEqual(read_bytes(self.fx.settings), installed)

    def test_ct_verify_runtime_path_traversal_refused(self):
        self.install_ok()
        outside_guard = self.fx.outside + "/permission_guard.py"
        with open(outside_guard, "wb") as fh:
            fh.write(b"# elsewhere\n")

        def mutate(st):
            st["runtime_path"] = self.fx.runtime_dir() + "/../../../../../outside/permission_guard.py"
        self.fx.edit_state(mutate)
        self.assert_verify("INCOMPLETE_INSTALL", state_refusal="STATE_RUNTIME_NOT_PINNED")
        self.assert_refused(self.fx.run("rollback"), 2, "STATE_RUNTIME_NOT_PINNED")

    def test_ct_state_settings_target_mismatch_refused(self):
        self.install_ok()
        self.fx.edit_state(lambda st: st.__setitem__("settings_target", self.fx.outside + "/s.json"))
        self.assert_refused(self.fx.run("rollback"), 2, "STATE_SETTINGS_TARGET_MISMATCH")

    # test options ---------------------------------------------------------------
    def test_test_options_refused_without_test_root(self):
        before = snapshot(self.fx.root)
        for flag in (["--test-crash-before-replace"], ["--test-short-write"], ["--test-fail-after-write"],
                     ["--test-barrier", self.fx.root], ["--test-fault", "state-save"]):
            self.assert_refused(self.fx.run("install", test_root=None, extra=flag), 2, "TEST_OPTION_WITHOUT_TEST_ROOT")
        self.assertEqual(before, snapshot(self.fx.root))

    def test_test_barrier_outside_or_traversing_the_test_root_refused(self):
        for barrier in (self.fx.outside + "/../../", tempfile.gettempdir(), self.fx.root + "/x/../outside"):
            self.assert_refused(self.fx.run("install", extra=["--test-barrier", barrier]), 2, "TEST_BARRIER_NOT_CONTAINED")
        self.assertFalse(os.path.exists(self.fx.settings))

    def test_ct_barrier_with_a_missing_intermediate_parent_refused(self):
        """D-8: CT4 — a non-existent parent below an existing root is a failure, not a created path."""
        before = snapshot(self.fx.root)
        self.assert_refused(self.fx.run("install", extra=["--test-barrier", self.fx.root + "/missing/b"]), 2,
                            "TEST_BARRIER_NOT_CONTAINED")
        self.assertEqual(before, snapshot(self.fx.root))

    def test_test_fault_unknown_point_refused(self):
        before = snapshot(self.fx.root)
        self.assert_refused(self.fx.run("install", extra=["--test-fault", "no-such-point"]), 2, "TEST_FAULT_UNKNOWN")
        self.assertEqual(before, snapshot(self.fx.root))

    def test_ct_barrier_through_a_symlinked_intermediate_directory_refused(self):
        """CT4 per-component lstat walk: an intermediate symlink is refused even when its target stays
        inside the test root (realpath alone would accept it) and when it points outside."""
        real = self.fx.root + "/barrier-real"
        os.makedirs(real + "/b")
        os.symlink(real, self.fx.root + "/barrier-alias")
        os.makedirs(self.fx.outside + "/b")
        os.symlink(self.fx.outside, self.fx.root + "/barrier-out")
        for barrier in (self.fx.root + "/barrier-alias/b", self.fx.root + "/barrier-out/b"):
            self.assert_refused(self.fx.run("install", extra=["--test-barrier", barrier]), 2, "TEST_BARRIER_NOT_CONTAINED")
        self.assertEqual(os.listdir(real + "/b"), [])
        self.assertEqual(os.listdir(self.fx.outside + "/b"), [])
        self.assertFalse(os.path.exists(self.fx.settings))

    def test_test_root_must_be_a_real_directory_directly_under_the_temp_dir(self):
        nested = self.fx.root + "/nested"
        os.mkdir(nested)
        link = os.path.realpath(tempfile.gettempdir()) + "/pginst-link-" + os.path.basename(self.fx.root)
        os.symlink(self.fx.root, link)
        try:
            for bad in (nested, link, self.fx.root + "-missing", self.fx.root + "/../" + os.path.basename(self.fx.root)):
                self.assert_refused(self.fx.run("verify", test_root=bad), 2, "TEST_ROOT_INVALID")
        finally:
            os.unlink(link)

    # rev15 7.11b (2) helpers ---------------------------------------------------------
    def fault(self, *points):
        out = []
        for point in points:
            out += ["--test-fault", point]
        return out

    def receipts(self):
        directory = self.fx.evidence_dir + "/receipts"
        if not os.path.isdir(directory):
            return []
        out = []
        for name in sorted(os.listdir(directory)):
            with open(directory + "/" + name) as fh:
                out.append((name, json.load(fh)))
        return out

    def aborted(self, hash12=None):
        if not os.path.isdir(self.fx.runtime_parent):
            return []
        return [d for d in os.listdir(self.fx.runtime_parent) if d.startswith((hash12 or self.fx.hash12) + ".aborted-")]

    def run_with_barrier(self, mode, at_barrier, manifest=None):
        barrier = self.fx.root + "/barrier"
        os.mkdir(barrier)
        manifest = manifest or (self.fx.promote_manifest if mode == "promote" else self.fx.manifest)
        argv = [sys.executable, "-I", INSTALLER, mode, "--manifest", manifest,
                "--expect-sha256", sha256_file(INSTALLER), "--expect-manifest-sha256", sha256_file(manifest),
                "--test-root", self.fx.root, "--test-barrier", barrier]
        proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.fx.root)
        deadline = time.time() + 20
        while not os.path.exists(barrier + "/reached"):
            if time.time() > deadline or proc.poll() is not None:
                proc.kill()
                self.fail("installer never reached the test barrier")
            time.sleep(0.05)
        at_barrier(proc)
        if proc.poll() is None:
            with open(barrier + "/release", "w") as fh:
                fh.write("go\n")
        out, err = proc.communicate(timeout=30)
        lines = [ln for ln in out.decode().splitlines() if ln.strip()]
        self.assertTrue(lines, err.decode())
        receipt = json.loads(lines[-1])
        self.fx.collect(receipt)
        return proc.returncode, receipt, err.decode()

    # rev15 F1, invariant W ---------------------------------------------------------
    def test_w_install_postimage_read_failure_is_a_recorded_write(self):
        self.fx.write_settings(OTHER_SETTINGS)
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("postimage-read"))
        self.assertEqual((rc, receipt["reason"], receipt["result"]), (7, "POSTIMAGE_UNREADABLE", "FAILED"), receipt)
        self.assertTrue(receipt["settings_written"])
        self.assertEqual(os.path.dirname(receipt["temp_path"]), self.fx.settings_dir)
        self.assertIsNone(receipt["postimage_sha256"])
        self.assertEqual(receipt["intended_postimage_sha256"], sha256_file(self.fx.settings))
        self.assertEqual(self.settings_obj()["hooks"]["PermissionRequest"], [self.fx.expected_entry()])
        self.assertTrue(receipt["state_saved"])
        state = self.fx.state()
        self.assertTrue(state["settings_written"])
        self.assertEqual(state["reason"], "POSTIMAGE_UNREADABLE")
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (1, "DRIFT"), receipt)
        self.assertEqual(receipt["intended_postimage_sha256"], sha256_file(self.fx.settings))
        self.assertEqual(receipt["settings_sha256"], sha256_file(self.fx.settings))
        before = snapshot(self.fx.root)
        self.assert_refused(self.fx.run("rollback"), 4, "POSTIMAGE_UNKNOWN")
        self.unchanged_except_evidence(before)
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        before = snapshot(self.fx.root)
        self.assert_refused(self.fx.run("promote"), 4, "POSTIMAGE_UNKNOWN")
        self.unchanged_except_evidence(before)

    def test_w_promote_postimage_read_failure_is_a_recorded_write(self):
        self.install_ok()
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        rc, receipt, out, err = self.fx.run("promote", extra=self.fault("postimage-read"))
        self.assertEqual((rc, receipt["reason"]), (7, "POSTIMAGE_UNREADABLE"), receipt)
        self.assertTrue(receipt["settings_written"])
        self.assertEqual(receipt["intended_postimage_sha256"], sha256_file(self.fx.settings))
        self.assertEqual(self.settings_obj()["hooks"]["PermissionRequest"], [self.fx.expected_entry()])
        self.assertTrue(self.fx.state()["settings_written"])
        self.assertIsNone(self.fx.state()["postimage_sha256"])
        self.fx.write_manifest()  # verify and rollback read the install manifest: re-frozen for the v2 source
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (1, "DRIFT"), receipt)
        before = snapshot(self.fx.root)
        self.assert_refused(self.fx.run("rollback"), 4, "POSTIMAGE_UNKNOWN")
        self.unchanged_except_evidence(before)

    def test_w_rollback_present_preimage_postimage_read_failure(self):
        self.fx.write_settings(OTHER_SETTINGS)
        preimage = read_bytes(self.fx.settings)
        self.install_ok()
        rc, receipt, out, err = self.fx.run("rollback", extra=self.fault("postimage-read"))
        self.assertEqual((rc, receipt["reason"]), (7, "POSTIMAGE_UNREADABLE"), receipt)
        self.assertTrue(receipt["settings_restored"])
        self.assertEqual(read_bytes(self.fx.settings), preimage)
        state = self.fx.state()
        self.assertNotIn(state["result"], ("INSTALLED", "PROMOTED"))
        self.assertTrue(state["settings_restored"])
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (1, "DRIFT"), receipt)

    def test_w_rollback_absent_preimage_has_no_read_back(self):
        """Recorded behaviour: an ABSENT-preimage rollback renames the target and reads nothing back,
        so the postimage-read fault point is never reached and the rollback completes."""
        self.install_ok()
        rc, receipt, out, err = self.fx.run("rollback", extra=self.fault("postimage-read"))
        self.assertEqual((rc, receipt["result"]), (0, "ROLLED_BACK"), receipt)
        self.assertTrue(receipt["settings_restored"])
        self.assertFalse(os.path.exists(self.fx.settings))

    # rev15 R3-A2, invariant A ---------------------------------------------------------
    def test_a_install_source_changed_at_barrier_archives_runtime_once(self):
        self.fx.write_settings(OTHER_SETTINGS)
        original = read_bytes(self.fx.settings)

        def change_source(proc):
            with open(self.fx.source, "wb") as fh:  # fixture hashes deliberately left at v1
                fh.write(GUARD_V2)
        rc, receipt, err = self.run_with_barrier("install", change_source)
        self.assertEqual((rc, receipt["reason"]), (7, "SOURCE_HASH_MISMATCH"), (receipt, err))
        self.assertEqual(receipt["failed_step"], "runtime copy")
        self.assertEqual(os.path.dirname(receipt["runtime_renamed_to"]), self.fx.runtime_parent)
        self.assertEqual(len(self.aborted()), 1)
        self.assertFalse(os.path.exists(self.fx.runtime_dir()))
        self.assertEqual(read_bytes(self.fx.settings), original)
        self.fx.write_source(GUARD_V1)  # the manifest pins v1: every mode refuses a different source
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (1, "INCOMPLETE_INSTALL"), receipt)
        self.install_ok()
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (0, "MATCH"), receipt)
        self.assertEqual(len(self.aborted()), 1)

    def test_a_promote_source_changed_at_barrier_archives_new_runtime_once(self):
        self.install_ok()
        old_dir = self.fx.runtime_dir()
        old_snapshot = snapshot(old_dir)
        installed = read_bytes(self.fx.settings)
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        new_hash12 = self.fx.hash12

        def change_source(proc):
            with open(self.fx.source, "wb") as fh:
                fh.write(b"#!/usr/bin/env python3\n# guard v3\n")
        rc, receipt, err = self.run_with_barrier("promote", change_source)
        self.assertEqual((rc, receipt["reason"]), (7, "SOURCE_HASH_MISMATCH"), (receipt, err))
        self.assertEqual(receipt["failed_step"], "runtime copy")
        self.assertTrue(os.path.basename(receipt["runtime_renamed_to"]).startswith(new_hash12 + ".aborted-"))
        self.assertEqual(len(self.aborted(new_hash12)), 1)
        self.assertFalse(os.path.exists(self.fx.runtime_parent + "/" + new_hash12))
        self.assertEqual(read_bytes(self.fx.settings), installed)
        self.assertEqual(snapshot(old_dir), old_snapshot)

    def test_a_runtime_copy_write_fault_archives_runtime_once(self):
        self.fx.write_settings(OTHER_SETTINGS)
        original = read_bytes(self.fx.settings)
        receipt = self.assert_refused(self.fx.run("install", extra=self.fault("runtime-copy-write")), 7,
                                      "RUNTIME_COPY_FAILED")
        self.assertEqual(receipt["failed_step"], "runtime copy")
        self.assertEqual(len(self.aborted()), 1)
        self.assertEqual(receipt["runtime_renamed_to"], self.fx.runtime_parent + "/" + self.aborted()[0])
        self.assertFalse(os.path.exists(self.fx.runtime_dir()))
        self.assertEqual(read_bytes(self.fx.settings), original)

    # rev15 F3, the post-commit boundary --------------------------------------------------
    def test_f3_install_interrupt_after_replace_never_touches_the_runtime(self):
        self.fx.write_settings(OTHER_SETTINGS)
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("interrupt-after-replace"))
        self.assertEqual(rc, 7, receipt)
        self.assertTrue(receipt["interrupted"])
        self.assertTrue(receipt["settings_written"])
        self.assertEqual(receipt["failed_step"], "after the settings replacement")
        self.assertTrue(os.path.isdir(self.fx.runtime_dir()))
        self.assertEqual(stat.S_IMODE(os.stat(self.fx.runtime_path()).st_mode), 0o444)
        self.assertEqual(sha256_file(self.fx.runtime_path()), self.fx.source_sha)
        self.assertEqual(self.settings_obj()["hooks"]["PermissionRequest"], [self.fx.expected_entry()])
        self.assertEqual(self.aborted(), [])
        state = self.fx.state()
        self.assertTrue(state["settings_written"])
        self.assertTrue(state["interrupted"])
        # the interrupt fires before the read-back, so no postimage was recorded: DRIFT, never MATCH (W)
        self.assertIsNone(state["postimage_sha256"])
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (1, "DRIFT"), receipt)

    def test_f3_promote_interrupt_after_replace_never_touches_the_runtime(self):
        self.install_ok()
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        rc, receipt, out, err = self.fx.run("promote", extra=self.fault("interrupt-after-replace"))
        self.assertEqual(rc, 7, receipt)
        self.assertTrue(receipt["interrupted"])
        self.assertTrue(receipt["settings_written"])
        self.assertEqual(stat.S_IMODE(os.stat(self.fx.runtime_path()).st_mode), 0o444)
        self.assertEqual(sha256_file(self.fx.runtime_path()), self.fx.source_sha)
        self.assertEqual(self.settings_obj()["hooks"]["PermissionRequest"], [self.fx.expected_entry()])
        self.assertEqual(self.aborted(), [])
        self.assertTrue(self.fx.state()["interrupted"])
        self.fx.write_manifest()  # verify reads the install manifest: re-frozen for the v2 source
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (1, "DRIFT"), receipt)

    def test_f3_interrupt_before_replace_archives_once_and_keeps_settings(self):
        self.fx.write_settings(OTHER_SETTINGS)
        original = read_bytes(self.fx.settings)
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("interrupt-before-replace"))
        self.assertEqual(rc, 7, receipt)
        self.assertTrue(receipt["interrupted"])
        self.assertIs(receipt["settings_written"], False)  # rev25 Y3/Y4
        self.assertEqual(read_bytes(self.fx.settings), original)
        self.assertEqual(len(self.aborted()), 1)
        self.assertFalse(os.path.exists(self.fx.runtime_dir()))
        self.assertTrue(os.path.exists(receipt["temp_path"]))

    def test_f3_sigterm_during_the_transaction_is_deferred(self):
        """Rewritten for rev18 (S1H-OD-7): the install barrier sits after the first write (the preimage copy), inside the
        deferral scope, so a real SIGTERM there no longer aborts the run; it is recorded and the install completes."""
        self.fx.write_settings(OTHER_SETTINGS)
        rc, receipt, err = self.run_with_barrier("install", lambda proc: proc.terminate())
        self.assertEqual((rc, receipt["result"]), (0, "INSTALLED"), (receipt, err))
        self.assertEqual((receipt["interrupt_deferred"], receipt["interrupt_signal"]), (True, "SIGTERM"))
        self.assertIs(receipt["interrupted"], False)  # rev25 Y3/Y4
        self.assert_verify("MATCH")

    # rev15 F4, outcome S ----------------------------------------------------------------
    def test_s_install_state_save_failure_is_never_a_success(self):
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("state-save"))
        self.assertEqual((rc, receipt["result"]), (7, "INSTALLED_STATE_NOT_SAVED"), receipt)
        self.assertFalse(receipt["state_saved"])
        self.assertEqual(receipt["original_result"], "INSTALLED")
        self.assertIn("original_reason", receipt)
        self.assertFalse(os.path.exists(self.fx.state_path()))
        for name, saved in self.receipts():
            self.assertFalse(saved.get("result") == "INSTALLED" and saved.get("exit_code") == 0, name)
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (1, "INCOMPLETE_INSTALL"), receipt)
        before = snapshot(self.fx.root)
        self.assert_refused(self.fx.run("rollback"), 2, "STATE_MISSING")
        self.unchanged_except_evidence(before)

    def test_s_postimage_and_state_save_faults_together(self):
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("postimage-read", "state-save"))
        self.assertEqual((rc, receipt["result"]), (7, "FAILED_STATE_NOT_SAVED"), receipt)
        self.assertTrue(receipt["settings_written"])
        self.assertFalse(receipt["state_saved"])
        self.assertEqual(receipt["original_reason"], "POSTIMAGE_UNREADABLE")

    def test_s_state_save_fault_on_a_foreign_hook_refusal(self):
        """Recorded behaviour: a refusal with no write saves no state, so the state-save fault point
        is never reached; the refusal keeps its own exit code and reason."""
        self.fx.write_settings({"hooks": {"PermissionRequest": [{"matcher": "Bash", "hooks": [
            {"type": "command", "command": "echo other"}]}]}})
        receipt = self.assert_refused(self.fx.run("install", extra=self.fault("state-save")), 3, "FOREIGN_HOOK")
        self.assertIsNone(receipt["state_save_error"])  # rev25 Y3: documented, null when no state save was attempted
        self.assertIs(receipt["state_saved"], False)
        self.assertFalse(os.path.exists(self.fx.state_path()))

    def test_s_receipt_save_failure_after_the_state_was_saved(self):
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("receipt-save"))
        self.assertEqual(rc, 7, receipt)
        self.assertFalse(receipt["receipt_saved"])
        self.assertTrue(receipt["state_saved"])
        self.assertEqual(self.fx.state()["result"], "INSTALLED")
        self.assertEqual(self.receipts(), [])

    def test_s_rollback_state_save_failure_leaves_a_detected_stale_state(self):
        self.fx.write_settings(OTHER_SETTINGS)
        preimage = read_bytes(self.fx.settings)
        self.install_ok()
        rc, receipt, out, err = self.fx.run("rollback", extra=self.fault("state-save"))
        self.assertEqual((rc, receipt["result"]), (7, "ROLLED_BACK_STATE_NOT_SAVED"), receipt)
        self.assertEqual(read_bytes(self.fx.settings), preimage)
        self.assertEqual(self.fx.state()["result"], "INSTALLED")  # stale, not missing
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (1, "DRIFT"), receipt)
        before = snapshot(self.fx.root)
        self.assert_refused(self.fx.run("rollback"), 4, "SETTINGS_DRIFT")
        self.unchanged_except_evidence(before)

    # rev15 F5, F6, F7 and IDEMPOTENT -----------------------------------------------------
    def test_f5_dir_fsync_failure_is_recorded(self):
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("dir-fsync"))
        self.assertEqual((rc, receipt["result"]), (0, "INSTALLED"), receipt)
        self.assertIn("TEST_FAULT", receipt["dir_fsync_error"])

    def test_f6_temp_readback_failure_names_the_temp_file_and_exits_7(self):
        self.fx.write_settings(OTHER_SETTINGS)
        original = read_bytes(self.fx.settings)
        receipt = self.assert_refused(self.fx.run("install", extra=self.fault("temp-readback")), 7,
                                      "TEMP_READBACK_FAILED")
        self.assertTrue(os.path.exists(receipt["temp_path"]))
        self.assertEqual(os.path.dirname(receipt["temp_path"]), self.fx.settings_dir)
        self.assertEqual(read_bytes(self.fx.settings), original)
        self.assertEqual(len(self.aborted()), 1)

    def test_f7_state_read_error_is_state_unreadable(self):
        self.install_ok()
        rc, receipt, out, err = self.fx.run("verify", extra=self.fault("state-read"))
        self.assertEqual((rc, receipt["result"]), (1, "STATE_UNREADABLE"), receipt)
        before = snapshot(self.fx.root)
        self.assert_refused(self.fx.run("rollback", extra=self.fault("state-read")), 2, "STATE_UNREADABLE")
        self.unchanged_except_evidence(before)

    def test_idempotent_only_with_an_intact_runtime_file(self):
        self.install_ok()
        path = self.fx.runtime_path()
        for damage in ("moved", "bytes", "mode"):
            if damage == "moved":
                os.rename(path, path + ".moved")
            elif damage == "bytes":
                os.chmod(path, 0o644)
                with open(path, "ab") as fh:
                    fh.write(b"# other\n")
                os.chmod(path, 0o444)
            else:
                os.chmod(path, 0o644)
            before = snapshot(self.fx.root)
            self.assert_refused(self.fx.run("install"), 4, "RUNTIME_DRIFT")
            self.unchanged_except_evidence(before)
            # restore the intact copy for the next case
            if os.path.exists(path):
                os.chmod(path, 0o644)
                os.unlink(path)
            if os.path.exists(path + ".moved"):
                os.rename(path + ".moved", path)
            else:
                with open(path, "wb") as fh:
                    fh.write(GUARD_V1)
            os.chmod(path, 0o444)
            rc, receipt, out, err = self.fx.run("install")
            self.assertEqual((rc, receipt["result"]), (0, "IDEMPOTENT"), (damage, receipt))

    # rev15 R-2: the production-layout sequence ----------------------------------------------
    def test_r2_production_layout_sequence_all_four_modes(self):
        self.assertTrue(self.fx.evidence_dir.endswith("/.scratch/" + RUNID_DIR + "/r9-install"))
        self.assertFalse(os.path.exists(self.fx.evidence_dir + "/receipts"))
        self.fx.write_settings(OTHER_SETTINGS)
        preimage = read_bytes(self.fx.settings)
        start = snapshot(self.fx.root)

        def count(kind):
            return len([n for n, _ in self.receipts() if f"-{kind}-" in n])

        first = self.install_ok()
        self.assertEqual((count("install"), self.fx.state()["result"]), (1, "INSTALLED"))
        listing, state_bytes = self.receipts(), read_bytes(self.fx.state_path())
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (0, "MATCH"), receipt)
        self.assertEqual((self.receipts(), read_bytes(self.fx.state_path())), (listing, state_bytes))
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual((rc, receipt["result"]), (0, "ROLLED_BACK"), receipt)
        self.assertEqual((count("rollback"), self.fx.state()["result"]), (1, "ROLLED_BACK"))
        self.assertEqual(read_bytes(self.fx.settings), preimage)
        self.install_ok()
        self.assertEqual(count("install"), 2)
        old_dir = self.fx.runtime_dir()
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        rc, receipt, out, err = self.fx.run("promote")
        self.assertEqual((rc, receipt["result"]), (0, "PROMOTED"), receipt)
        self.assertEqual(count("promote"), 1)
        state = self.fx.state()
        self.assertEqual((state["result"], state["runtime_dir"]), ("PROMOTED", self.fx.runtime_dir()))
        self.assertEqual(state["previous_receipt"]["result"], "INSTALLED")
        self.assertNotEqual(old_dir, self.fx.runtime_dir())
        self.fx.write_manifest()  # verify and rollback read the install manifest: re-frozen for the v2 source
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"]), (0, "MATCH"), receipt)
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual((rc, receipt["result"]), (0, "ROLLED_BACK"), receipt)
        self.assertEqual(read_bytes(self.fx.settings), preimage)
        self.assertEqual(len([d for d in os.listdir(self.fx.runtime_parent)
                              if d.startswith(self.fx.hash12 + ".disabled-")]), 1)
        self.assertEqual(count("rollback"), 2)
        for _, saved in self.receipts():
            self.assertEqual(saved["test_root"], self.fx.root)
            self.assertEqual(saved["evidence_dir"], self.fx.evidence_dir)
            self.assertEqual(saved["settings_target"], self.fx.settings)
        allowed = (os.path.relpath(self.fx.evidence_dir, self.fx.root), os.path.relpath(self.fx.settings_dir, self.fx.root),
                   os.path.relpath(self.fx.runtime_parent, self.fx.root), os.path.relpath(self.fx.source, self.fx.root),
                   os.path.relpath(self.fx.promote_manifest, self.fx.root), os.path.relpath(self.fx.manifest, self.fx.root))
        end = snapshot(self.fx.root)
        changed = {k for k in set(start) | set(end) if start.get(k) != end.get(k)}
        for path in changed:
            self.assertTrue(any(path == a or path.startswith(a + "/") for a in allowed), path)
        self.assertEqual(first["evidence_dir"], self.fx.evidence_dir)

    # rev15 Q3 round 4 repairs ---------------------------------------------------------------
    def replace_with_symlink(self, directory):
        moved = directory + ".moved"
        os.rename(directory, moved)
        os.symlink(moved, directory)

    def test_r4_symlinked_runtime_dir_is_refused_before_the_rollback_writes(self):
        """Rewritten for rev21 (7.11d (2) V1): a runtime dir that is already a symlink when rollback starts is now refused
        by the prospective validation of phase P (REFUSED / exit 2, nothing written). It asserted, as R4-4, a refusal of
        the post-commit rename (FAILED / exit 7); that path still exists only for a change between phase P and the
        rename, which no seam reaches; the conversion itself is the one U4a-U4c prove (record_outcome, V2/W)."""
        self.fx.write_settings(OTHER_SETTINGS)
        self.install_ok()
        installed = read_bytes(self.fx.settings)
        self.replace_with_symlink(self.fx.runtime_dir())
        before = snapshot(self.fx.root)
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual((rc, receipt["result"], receipt["reason"]), (2, "REFUSED", "RENAME_SOURCE_NOT_CONTAINED"), receipt)
        self.assertIs(receipt["settings_restored"], False)  # rev25 Y3/Y4
        self.assertEqual(read_bytes(self.fx.settings), installed)
        self.assert_only_receipts_changed(before)

    def test_r4_symlinked_runtime_dir_is_refused_before_the_promote_writes(self):
        """Rewritten for rev21 (V1), as the rollback case above: the promote refuses in phase P, nothing written."""
        self.install_ok()
        installed = read_bytes(self.fx.settings)
        old_dir = self.fx.runtime_dir()
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        self.replace_with_symlink(old_dir)
        before = snapshot(self.fx.root)
        rc, receipt, out, err = self.fx.run("promote")
        self.assertEqual((rc, receipt["result"], receipt["reason"]), (2, "REFUSED", "RENAME_SOURCE_NOT_CONTAINED"), receipt)
        self.assertIs(receipt["settings_written"], False)  # rev25 Y3/Y4
        self.assertEqual(read_bytes(self.fx.settings), installed)
        self.assertFalse(os.path.exists(self.fx.runtime_dir()))
        self.assert_only_receipts_changed(before)

    def test_r4_postimage_mismatch_is_an_unverified_postimage(self):
        """R4-5: a state whose recorded postimage differs from the intended postimage (POSTIMAGE_MISMATCH, a concurrent
        writer) is never MATCH and never acted on, even when the settings now equal that recorded hash."""
        self.install_ok()
        with open(self.fx.settings, "a") as fh:
            fh.write("\n")  # the concurrent writer's bytes
        foreign = sha256_file(self.fx.settings)

        def mutate(st):
            st.update(result="FAILED", reason="POSTIMAGE_MISMATCH", exit_code=7, postimage_sha256=foreign)
        self.fx.edit_state(mutate)
        self.assertNotEqual(self.fx.state()["intended_postimage_sha256"], foreign)
        self.assert_verify("DRIFT")
        before = snapshot(self.fx.root)
        self.assert_refused(self.fx.run("rollback"), 4, "POSTIMAGE_UNKNOWN")
        self.unchanged_except_evidence(before)

    def test_r4_unencodable_settings_text_keeps_the_refusal_outcome(self):
        """R4-6: a lone-surrogate escape in the settings never turns a refusal into a traceback: the receipt is printed
        and persisted and the refusal keeps its code."""
        raw = (b'{"hooks": {"PermissionRequest": [{"matcher": "Bash", "hooks": '
               b'[{"type": "command", "command": "echo \\ud800"}]}]}}\n')
        self.fx.write_settings(raw=raw)
        receipt = self.assert_refused(self.fx.run("install"), 3, "FOREIGN_HOOK")
        self.assertTrue(receipt["receipt_saved"])
        self.assertEqual([saved["reason"] for _, saved in self.receipts()], ["FOREIGN_HOOK"])
        self.assertEqual(read_bytes(self.fx.settings), raw)

    def test_r4_verify_step2_read_error_is_drift_and_malformed_state_is_unreadable(self):
        """R4-8: 11.3 is binding — a read error on a committed state's runtime file is DRIFT (step 2, never a refusal),
        and a state file that exists but cannot be parsed is STATE_UNREADABLE (step 1)."""
        self.install_ok()
        os.chmod(self.fx.runtime_path(), 0o000)
        try:
            receipt = self.assert_verify("DRIFT")
            self.assertIn("runtime_read_error", receipt)
        finally:
            os.chmod(self.fx.runtime_path(), 0o444)
        self.assert_verify("MATCH")
        with open(self.fx.state_path(), "w") as fh:
            fh.write("{not json")
        self.assert_verify("STATE_UNREADABLE", state_refusal="STATE_MALFORMED")

    # rev18 7.11c (2): interrupt deferral (S1H-OD-7), real signals in a separate process -------------------------------
    def run_with_pause(self, mode, signals, extra=(), manifest=None):
        """Run MODE with --test-fault pause-in-scope; once the installer reports it is paused inside the deferral scope,
        send each real signal in SIGNALS to it, then release the pause and collect the outcome."""
        reached, release = self.fx.root + "/pause-reached", self.fx.root + "/pause-release"
        manifest = manifest or (self.fx.promote_manifest if mode == "promote" else self.fx.manifest)
        argv = [sys.executable, "-I", INSTALLER, mode, "--manifest", manifest,
                "--expect-sha256", sha256_file(INSTALLER), "--expect-manifest-sha256", sha256_file(manifest),
                "--test-root", self.fx.root, "--test-fault", "pause-in-scope"] + list(extra)
        proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.fx.root)
        deadline = time.time() + 20
        while not os.path.exists(reached):
            if time.time() > deadline or proc.poll() is not None:
                proc.kill()
                self.fail("installer never reached pause-in-scope")
            time.sleep(0.05)
        for sig in signals:
            proc.send_signal(sig)
            time.sleep(0.2)
        with open(release, "w") as fh:
            fh.write("go\n")
        out, err = proc.communicate(timeout=60)
        lines = [ln for ln in out.decode().splitlines() if ln.strip()]
        self.assertTrue(lines, err.decode())
        receipt = json.loads(lines[-1])
        self.fx.collect(receipt)
        return proc.returncode, receipt, err.decode()

    def assert_deferred(self, receipt, name):
        self.assertEqual((receipt["interrupt_deferred"], receipt["interrupt_signal"]), (True, name), receipt)
        state = self.fx.state()
        self.assertEqual((state["interrupt_deferred"], state["interrupt_signal"]), (True, name), state)
        self.assertIs(receipt["interrupted"], False)  # rev25 Y3/Y4
        # the receipt file on disk carries the same snapshot as the printed receipt (D3)
        self.assertIn(self.as_saved(receipt), [saved for _, saved in self.receipts()])  # rev27 Z4

    def test_t1_install_completes_and_records_a_deferred_sigint(self):
        rc, receipt, err = self.run_with_pause("install", [signal.SIGINT])
        self.assertEqual((rc, receipt["result"]), (0, "INSTALLED"), (receipt, err))
        self.assert_deferred(receipt, "SIGINT")
        self.assertEqual(stat.S_IMODE(os.stat(self.fx.runtime_path()).st_mode), 0o444)
        self.assertEqual(sha256_file(self.fx.runtime_path()), self.fx.source_sha)
        self.assertNotIn("Traceback", err)
        self.assert_verify("MATCH")

    def test_t2_install_completes_and_records_a_deferred_sigterm_or_sighup(self):
        for sig, name in ((signal.SIGTERM, "SIGTERM"), (signal.SIGHUP, "SIGHUP")):
            with self.subTest(signal=name):
                if os.path.exists(self.fx.settings):  # second round: start again from a rolled-back layout
                    rc, receipt, out, err = self.fx.run("rollback")
                    self.assertEqual(rc, 0, receipt)
                    os.unlink(self.fx.root + "/pause-reached")
                    os.unlink(self.fx.root + "/pause-release")
                rc, receipt, err = self.run_with_pause("install", [sig])
                self.assertEqual((rc, receipt["result"]), (0, "INSTALLED"), (receipt, err))
                self.assert_deferred(receipt, name)
                self.assert_verify("MATCH")

    def test_t3_rollback_and_promote_complete_and_record_a_deferred_sigint(self):
        self.fx.write_settings(OTHER_SETTINGS)
        preimage = read_bytes(self.fx.settings)
        self.install_ok()
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        # signal-at-first-write also puts a SIGINT at the mode's own first write, before main's receipt handling (D1)
        rc, receipt, err = self.run_with_pause("promote", [signal.SIGINT], extra=self.fault("signal-at-first-write"))
        self.assertEqual((rc, receipt["result"]), (0, "PROMOTED"), (receipt, err))
        self.assert_deferred(receipt, "SIGINT")
        self.fx.write_manifest()  # verify and rollback read the install manifest: re-frozen for the v2 source
        os.unlink(self.fx.root + "/pause-reached")
        os.unlink(self.fx.root + "/pause-release")
        rc, receipt, err = self.run_with_pause("rollback", [signal.SIGINT], extra=self.fault("signal-at-first-write"))
        self.assertEqual((rc, receipt["result"]), (0, "ROLLED_BACK"), (receipt, err))
        self.assertEqual((receipt["interrupt_deferred"], receipt["interrupt_signal"]), (True, "SIGINT"))
        self.assertEqual(self.fx.state()["interrupt_deferred"], True)
        self.assertEqual(read_bytes(self.fx.settings), preimage)

    def test_t4_signal_after_archive_rename_keeps_the_archive_record(self):
        """U7 (7.11d (1) ruling on R5b-2): the archive is named in the receipt; and in the state only when the run saves a
        state. A pre-commit failure saves none (7.11b), so the test asserts the receipt naming and that this run wrote no
        state file."""
        self.fx.write_settings(OTHER_SETTINGS)
        original = read_bytes(self.fx.settings)
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("runtime-copy-write", "signal-after-archive-rename"))
        self.assertEqual((rc, receipt["reason"]), (7, "RUNTIME_COPY_FAILED"), (receipt, err))
        self.assertEqual(len(self.aborted()), 1)
        self.assertEqual(receipt["runtime_renamed_to"], self.fx.runtime_parent + "/" + self.aborted()[0])
        self.assertEqual((receipt["interrupt_deferred"], receipt["interrupt_signal"]), (True, "SIGINT"))
        self.assertEqual(read_bytes(self.fx.settings), original)
        # a pre-commit failure saves no state (7.11b W/S): "the state" names the archive only when a state is saved
        self.assertFalse(os.path.exists(self.fx.state_path()))
        saved = [s for _, s in self.receipts()]
        self.assertEqual([s["runtime_renamed_to"] for s in saved], [receipt["runtime_renamed_to"]])

    def test_t5_signal_after_disabled_and_superseded_renames_keeps_their_records(self):
        self.install_ok()
        old_dir = self.fx.runtime_dir()
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        rc, receipt, out, err = self.fx.run("promote", extra=self.fault("signal-after-superseded-rename"))
        self.assertEqual((rc, receipt["result"]), (0, "PROMOTED"), (receipt, err))
        self.assertTrue(os.path.basename(receipt["previous_runtime_renamed_to"]).startswith(os.path.basename(old_dir) + ".superseded-"))
        self.assertEqual(self.fx.state()["previous_runtime_renamed_to"], receipt["previous_runtime_renamed_to"])
        self.assertEqual(receipt["interrupt_deferred"], True)
        self.fx.write_manifest()  # re-frozen for the v2 source
        new_dir = self.fx.runtime_dir()
        rc, receipt, out, err = self.fx.run("rollback", extra=self.fault("signal-after-disabled-rename"))
        self.assertEqual((rc, receipt["result"]), (0, "ROLLED_BACK"), (receipt, err))
        self.assertTrue(os.path.basename(receipt["runtime_renamed_to"]).startswith(os.path.basename(new_dir) + ".disabled-"))
        self.assertEqual(self.fx.state()["runtime_renamed_to"], receipt["runtime_renamed_to"])
        self.assertTrue(os.path.isdir(receipt["runtime_renamed_to"]))
        # R5b-4: the rollback half proves that its own signal point fired, so its adjacency proof is never vacuous
        self.assertEqual((receipt["interrupt_deferred"], receipt["interrupt_signal"]), (True, "SIGINT"), receipt)
        self.assertEqual((self.fx.state()["interrupt_deferred"], self.fx.state()["interrupt_signal"]), (True, "SIGINT"))

    def test_t6_signal_at_scope_entry_is_deferred_and_no_signal_mask_is_used(self):
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("signal-at-scope-entry"))
        self.assertEqual((rc, receipt["result"]), (0, "INSTALLED"), (receipt, err))
        self.assertEqual((receipt["interrupt_deferred"], receipt["interrupt_signal"]), (True, "SIGINT"))
        self.assertNotIn("Traceback", err)
        with open(INSTALLER) as fh:
            source = fh.read()
        self.assertNotIn("pthread_sigmask", source)
        self.assertNotIn("sigprocmask", source)

    def test_t7_signal_before_scope_ends_the_run_before_any_write(self):
        self.fx.write_settings(OTHER_SETTINGS)
        before = snapshot(self.fx.root)
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("signal-before-scope"))
        self.assertEqual(rc, 7, (out, err))  # R5b-5 (1): the exit code the README states for a Ctrl+C in phase P
        # phase P stays interruptible: the default SIGINT action (KeyboardInterrupt) ended the run, not a refusal
        self.assertEqual((receipt["result"], receipt["reason"], receipt.get("interrupted")), ("FAILED", "INTERRUPTED", True), receipt)
        self.assertIs(receipt["settings_written"], False)  # rev25 Y3/Y4: documented keys, no write recorded
        self.assertIsNone(receipt["runtime_dir_owned"])
        self.assertIsNone(receipt["failed_step"])  # R5b-c-3: nothing was written, so no write step failed
        after = snapshot(self.fx.root)
        for path in (self.fx.settings, self.fx.runtime_parent, self.fx.runtime_dir(), self.fx.state_path()):
            rel = os.path.relpath(path, self.fx.root)
            self.assertEqual(before.get(rel), after.get(rel), rel)
        self.assertFalse(os.path.exists(self.fx.runtime_parent))
        changed = {k for k in set(before) | set(after) if before.get(k) != after.get(k)}
        receipts = os.path.relpath(self.fx.evidence_dir + "/receipts", self.fx.root)
        self.assertTrue(all(k == receipts or k.startswith(receipts + "/") for k in changed), changed)

    def test_t8_signal_at_first_write_is_deferred(self):
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("signal-at-first-write"))
        self.assertEqual((rc, receipt["result"]), (0, "INSTALLED"), (receipt, err))
        self.assertEqual((receipt["interrupt_deferred"], receipt["interrupt_signal"]), (True, "SIGINT"))
        self.assert_verify("MATCH")

    def test_t9_two_signals_are_one_recorded_fact_without_a_count(self):
        rc, receipt, err = self.run_with_pause("install", [signal.SIGINT, signal.SIGTERM])
        self.assertEqual((rc, receipt["result"]), (0, "INSTALLED"), (receipt, err))
        self.assert_deferred(receipt, "SIGINT")
        self.assertFalse([k for k in receipt if "count" in k], receipt)

    def test_t10_deferred_signal_with_a_failed_state_save(self):
        rc, receipt, err = self.run_with_pause("install", [signal.SIGINT], extra=self.fault("state-save"))
        self.assertEqual((rc, receipt["result"]), (7, "INSTALLED_STATE_NOT_SAVED"), (receipt, err))
        self.assertEqual((receipt["interrupt_deferred"], receipt["interrupt_signal"]), (True, "SIGINT"))
        self.assertFalse(os.path.exists(self.fx.state_path()))

    def test_t11_signal_after_the_final_receipt_snapshot_is_reported_on_the_console(self):
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("signal-after-receipt-snapshot"))
        self.assertEqual((rc, receipt["result"]), (0, "INSTALLED"), (receipt, err))
        self.assertFalse(receipt["interrupt_deferred"])
        [(_, saved)] = self.receipts()
        self.assertFalse(saved["interrupt_deferred"])
        self.assertTrue(receipt["receipt_saved"])  # rev27 Z4: the save result is printed, never inside the saved file
        self.assertNotIn("receipt_saved", saved)
        notes = [ln for ln in err.splitlines() if "after the final receipt's snapshot" in ln]
        self.assertEqual(len(notes), 1, err)
        self.assertIn("SIGINT", notes[0])
        self.assertIn("saved to disk", notes[0])

    def test_t11b_signal_after_the_final_receipt_snapshot_with_a_failed_receipt_save(self):
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("signal-after-receipt-snapshot", "receipt-save"))
        self.assertEqual(rc, 7, (receipt, err))
        self.assertFalse(receipt["receipt_saved"])
        self.assertTrue(receipt["state_saved"])
        self.assertEqual(self.receipts(), [])
        notes = [ln for ln in err.splitlines() if "after the final receipt's snapshot" in ln]
        self.assertEqual(len(notes), 1, err)
        self.assertIn("only printed", notes[0])
        self.assertNotIn("saved to disk", notes[0])
        # U6 (R5b-1): the deferred signal added no write, retry or recovery beyond the F4 outcome
        state = self.fx.state()
        self.assertEqual((state["result"], state["interrupt_deferred"]), ("INSTALLED", False), state)
        self.assertEqual(self.settings_obj()["hooks"]["PermissionRequest"], [self.fx.expected_entry()])
        self.assertEqual(stat.S_IMODE(os.stat(self.fx.runtime_path()).st_mode), 0o444)
        self.assertEqual(sha256_file(self.fx.runtime_path()), self.fx.source_sha)
        self.assertEqual(self.aborted(), [])
        self.assertEqual([d for d in os.listdir(self.fx.runtime_parent) if ".disabled-" in d], [])
        paths = set(snapshot(self.fx.root))
        twin = Fixture()  # the same run with the receipt-save fault alone, in its own test root
        try:
            rc2, receipt2, out2, err2 = twin.run("install", extra=self.fault("receipt-save"))
            self.assertEqual((rc2, receipt2["receipt_saved"]), (7, False), receipt2)
            self.assertEqual(paths, set(snapshot(twin.root)))
            self.check_twin(twin)  # U11 (R5d-b-6): the twin's receipts and state are checked too
        finally:
            subprocess.run(["chmod", "-R", "u+w", twin.root], capture_output=True)
            import shutil
            shutil.rmtree(twin.root, ignore_errors=True)
        self.assert_verify("MATCH")

    def test_t12_signal_after_the_final_observation_changes_nothing(self):
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("signal-after-final-observation"))
        self.assertEqual((rc, receipt["result"]), (0, "INSTALLED"), (receipt, err))
        [(_, saved)] = self.receipts()
        self.assertFalse(saved["interrupt_deferred"])
        # the saved bytes and the printed receipt are the same snapshot; the printed receipt adds only the save result (Z4)
        self.assertEqual(saved, self.as_saved(receipt))
        self.assertNotIn("after the final receipt's snapshot", err)
        self.assertNotIn("Traceback", err)
        self.assert_verify("MATCH")

    # rev21 7.11d (2): validate before the first write (V1), no exit 2 after a transaction write (V2), verify (V3) -----
    MALFORMED_HOOKS = ({"hooks": None}, {"hooks": {"PermissionRequest": None}}, {"hooks": []}, {"hooks": "x"},
                       {"hooks": {"PermissionRequest": {}}}, {"hooks": {"PermissionRequest": [None]}})

    def changed_paths(self, before):
        after = snapshot(self.fx.root)
        return {k for k in set(before) | set(after) if before.get(k) != after.get(k)}

    def normalized_changes(self, fixture, before):
        """Changed relative paths, with the per-run receipt file name replaced (it carries a timestamp and a pid)."""
        after = snapshot(fixture.root)
        receipts = os.path.relpath(fixture.evidence_dir + "/receipts", fixture.root)
        out = set()
        for k in set(before) | set(after):
            if before.get(k) != after.get(k):
                out.add(receipts + "/<receipt>" if k.startswith(receipts + "/") else k)
        return out

    def assert_only_receipts_changed(self, before):
        receipts = os.path.relpath(self.fx.evidence_dir + "/receipts", self.fx.root)
        changed = self.changed_paths(before)
        self.assertTrue(all(k == receipts or k.startswith(receipts + "/") for k in changed), changed)

    def make_receipts_unavailable(self):
        """The receipt destination is unusable: receipts/ is a regular file (the evidence directory stays verified)."""
        receipts = self.fx.evidence_dir + "/receipts"
        if os.path.isdir(receipts):
            os.rename(receipts, receipts + ".moved")
        with open(receipts, "w") as fh:
            fh.write("not a directory\n")

    def restore_receipts(self):
        receipts = self.fx.evidence_dir + "/receipts"
        os.unlink(receipts)
        if os.path.isdir(receipts + ".moved"):
            os.rename(receipts + ".moved", receipts)

    def assert_code_kept_both_ways(self, run, code, reason):
        """U3e: before any transaction write a refusal keeps its own result, reason and code, with the refusal receipt
        persisted and with the receipt destination unavailable (printed and marked not saved)."""
        before = snapshot(self.fx.root)
        rc, receipt, out, err = run()
        self.assertEqual((rc, receipt["result"], receipt["reason"]), (code, "REFUSED", reason), receipt)
        self.assertTrue(receipt["receipt_saved"], receipt)
        self.assertIn(self.as_saved(receipt), [saved for _, saved in self.receipts()])  # rev27 Z4
        self.assert_only_receipts_changed(before)
        self.make_receipts_unavailable()
        try:
            before = snapshot(self.fx.root)
            rc, receipt, out, err = run()
            self.assertEqual((rc, receipt["result"], receipt["reason"]), (code, "REFUSED", reason), receipt)
            self.assertFalse(receipt["receipt_saved"], receipt)
            self.assertEqual(before, snapshot(self.fx.root))
            self.assertNotIn("Traceback", err)
        finally:
            self.restore_receipts()

    def test_u1_hooks_null_is_refused_in_phase_p_like_malformed_json(self):
        self.fx.write_settings({"hooks": None})
        before = snapshot(self.fx.root)
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual((rc, receipt["result"], receipt["reason"]), (2, "REFUSED", "SETTINGS_HOOKS_NOT_OBJECT"), (receipt, err))
        self.assertEqual(read_bytes(self.fx.settings), doc_bytes({"hooks": None}))
        self.assertFalse(os.path.exists(self.fx.runtime_parent))
        self.assertFalse(os.path.exists(self.fx.state_path()))
        self.assertFalse([f for f in os.listdir(self.fx.evidence_dir) if f.startswith("settings.local.json.preimage-")])
        self.assertIsNone(receipt["preimage_backup_created"])  # rev25 Y3: documented, null when nothing was copied
        # exactly what the existing pre-write refusal (malformed JSON, 6.2) writes: one refusal receipt, nothing else
        twin = Fixture()
        try:
            twin.write_settings(raw=b'{"hooks": ')
            twin_before = snapshot(twin.root)
            rc2, receipt2, out2, err2 = twin.run("install")
            self.assertEqual((rc2, receipt2["reason"]), (2, "SETTINGS_MALFORMED"), receipt2)
            self.assertEqual(self.normalized_changes(self.fx, before), self.normalized_changes(twin, twin_before))
            self.check_twin(twin)  # U11 (R5d-b-6)
        finally:
            import shutil
            shutil.rmtree(twin.root, ignore_errors=True)

    MALFORMED_HOOKS_REASONS = ("SETTINGS_HOOKS_NOT_OBJECT", "SETTINGS_PERMISSION_REQUEST_NOT_LIST", "SETTINGS_HOOKS_NOT_OBJECT",
                               "SETTINGS_HOOKS_NOT_OBJECT", "SETTINGS_PERMISSION_REQUEST_NOT_LIST",
                               "SETTINGS_PERMISSION_REQUEST_ENTRY_INVALID")

    def test_u2_every_malformed_hooks_shape_is_a_phase_p_refusal(self):
        for doc, reason in zip(self.MALFORMED_HOOKS, self.MALFORMED_HOOKS_REASONS):
            with self.subTest(settings=doc):
                self.fx.write_settings(doc)
                before = snapshot(self.fx.root)
                rc, receipt, out, err = self.fx.run("install")
                self.assertEqual((rc, receipt["result"]), (2, "REFUSED"), (receipt, err))
                self.assertEqual(receipt["reason"], reason, receipt)  # R5b-b-5: the exact reason per shape
                self.assertNotIn("Traceback", err)
                self.assert_only_receipts_changed(before)
                self.assertFalse(os.path.exists(self.fx.runtime_parent))

    def test_u3_source_and_write_target_containment_refused_in_phase_p(self):
        self.fx.write_settings(OTHER_SETTINGS)
        # WRITE_TARGET_NOT_CONTAINED: the state file the run would write is a symlink (prospective validation, V1)
        os.symlink(self.fx.outside + "/sentinel.txt", self.fx.state_path())
        before = snapshot(self.fx.root)
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual((rc, receipt["result"], receipt["reason"]), (2, "REFUSED", "WRITE_TARGET_NOT_CONTAINED"), receipt)
        self.assert_only_receipts_changed(before)
        self.assertFalse(os.path.exists(self.fx.runtime_parent))
        self.assertEqual(read_bytes(self.fx.outside + "/sentinel.txt"), b"leave me\n")
        os.unlink(self.fx.state_path())
        # SOURCE_NOT_CONTAINED: the guard source is a symlink
        real = self.fx.outside + "/guard.py"
        with open(real, "wb") as fh:
            fh.write(GUARD_V1)
        os.unlink(self.fx.source)
        os.symlink(real, self.fx.source)
        before = snapshot(self.fx.root)
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual((rc, receipt["result"], receipt["reason"]), (2, "REFUSED", "SOURCE_NOT_CONTAINED"), receipt)
        self.assert_only_receipts_changed(before)
        self.assertFalse(os.path.exists(self.fx.runtime_parent))

    def test_u3b_fresh_install_passes_prospective_validation_and_logs_the_strict_checks(self):
        self.assertFalse(os.path.exists(self.fx.runtime_parent))
        self.assertFalse(os.path.exists(self.fx.evidence_dir + "/receipts"))
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual((rc, receipt["result"]), (0, "INSTALLED"), (receipt, err))
        self.assertEqual(receipt["post_creation_checks"],
                         [self.fx.runtime_parent, self.fx.runtime_dir(), self.fx.evidence_dir + "/receipts"])
        self.assertEqual(receipt["runtime_parent_created"], self.fx.runtime_parent)
        self.assertIn(self.as_saved(receipt), [saved for _, saved in self.receipts()])  # rev27 Z4
        self.assert_verify("MATCH")

    def test_u3c_malformed_settings_refusal_receipt_is_persisted(self):
        self.fx.write_settings({"hooks": {"PermissionRequest": None}})
        before = snapshot(self.fx.root)
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual((rc, receipt["reason"]), (2, "SETTINGS_PERMISSION_REQUEST_NOT_LIST"), receipt)
        self.assertTrue(receipt["receipt_saved"])
        [(_, saved)] = self.receipts()
        self.assertEqual(saved, self.as_saved(receipt))  # rev27 Z4: the save result is printed, not saved
        self.assert_only_receipts_changed(before)

    def test_u3d_malformed_settings_refusal_with_the_receipt_destination_unavailable(self):
        self.fx.write_settings({"hooks": {"PermissionRequest": None}})
        self.make_receipts_unavailable()
        before = snapshot(self.fx.root)
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual((rc, receipt["reason"]), (2, "SETTINGS_PERMISSION_REQUEST_NOT_LIST"), receipt)
        self.assertFalse(receipt["receipt_saved"])
        self.assertEqual(before, snapshot(self.fx.root))
        self.restore_receipts()
        # the evidence directory itself unavailable: the refusal is printed only and marked not saved
        os.rename(self.fx.evidence_dir, self.fx.evidence_dir + ".away")
        try:
            before = snapshot(self.fx.root)
            rc, receipt, out, err = self.fx.run("install")
            self.assertEqual((rc, receipt["result"], receipt["reason"]), (2, "REFUSED", "EVIDENCE_DIR_NOT_REAL_DIRECTORY"), receipt)
            self.assertFalse(receipt["receipt_saved"])
            self.assertEqual(before, snapshot(self.fx.root))
        finally:
            os.rename(self.fx.evidence_dir + ".away", self.fx.evidence_dir)

    def test_u3e_codes_3_4_5_and_0_are_kept_before_any_transaction_write(self):
        # FOREIGN_HOOK, exit 3 (6.2)
        self.fx.write_settings({"hooks": {"PermissionRequest": [{"matcher": "Bash", "hooks": [
            {"type": "command", "command": "echo other"}]}]}})
        self.assert_code_kept_both_ways(lambda: self.fx.run("install"), 3, "FOREIGN_HOOK")
        os.unlink(self.fx.settings)
        # IDEMPOTENT, exit 0, no write but its receipt (11.2)
        self.install_ok()
        before = snapshot(self.fx.root)
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual((rc, receipt["result"]), (0, "IDEMPOTENT"), receipt)
        self.assert_only_receipts_changed(before)
        # RUNTIME_DRIFT, exit 4 (11.2)
        os.chmod(self.fx.runtime_path(), 0o644)
        self.assert_code_kept_both_ways(lambda: self.fx.run("install"), 4, "RUNTIME_DRIFT")
        # R5b-b-6: drifted runtime BYTES, as U3e names it (the persisted case is also in the unchanged
        # test_idempotent_only_with_an_intact_runtime_file)
        intact = read_bytes(self.fx.runtime_path())
        with open(self.fx.runtime_path(), "ab") as fh:
            fh.write(b"# drifted\n")
        os.chmod(self.fx.runtime_path(), 0o444)
        self.assert_code_kept_both_ways(lambda: self.fx.run("install"), 4, "RUNTIME_DRIFT")
        os.chmod(self.fx.runtime_path(), 0o644)
        with open(self.fx.runtime_path(), "wb") as fh:
            fh.write(intact)
        os.chmod(self.fx.runtime_path(), 0o444)
        # SAME_HASH_ALREADY_INSTALLED, exit 5 (11.5)
        self.write_promote_manifest()
        self.assert_code_kept_both_ways(lambda: self.fx.run("promote"), 5, "SAME_HASH_ALREADY_INSTALLED")
        # rollback with drift, exit 4 (11.4)
        installed = read_bytes(self.fx.settings)
        with open(self.fx.settings, "ab") as fh:
            fh.write(b"\n")
        self.assert_code_kept_both_ways(lambda: self.fx.run("rollback"), 4, "SETTINGS_DRIFT")
        with open(self.fx.settings, "wb") as fh:
            fh.write(installed)

    def test_u3e_postimage_unknown_keeps_exit_4_for_rollback_and_promote(self):
        self.fx.write_settings(OTHER_SETTINGS)
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("postimage-read"))
        self.assertEqual((rc, receipt["reason"]), (7, "POSTIMAGE_UNREADABLE"), receipt)
        self.assert_code_kept_both_ways(lambda: self.fx.run("rollback"), 4, "POSTIMAGE_UNKNOWN")
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        self.assert_code_kept_both_ways(lambda: self.fx.run("promote"), 4, "POSTIMAGE_UNKNOWN")

    def test_u3e_drift_before_write_keeps_exit_6_when_nothing_was_written(self):
        """ABSENT preimage: nothing is written before the barrier, so a drift there keeps REFUSED / exit 6."""
        barrier = self.fx.root + "/barrier"
        os.mkdir(barrier)
        argv = [sys.executable, "-I", INSTALLER, "install", "--manifest", self.fx.manifest,
                "--expect-sha256", sha256_file(INSTALLER), "--expect-manifest-sha256", self.fx.manifest_sha,
                "--test-root", self.fx.root, "--test-barrier", barrier]
        proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.fx.root)
        deadline = time.time() + 20
        while not os.path.exists(barrier + "/reached"):
            if time.time() > deadline or proc.poll() is not None:
                proc.kill()
                self.fail("installer never reached the test barrier")
            time.sleep(0.05)
        self.fx.write_settings(OTHER_SETTINGS)
        with open(barrier + "/release", "w") as fh:
            fh.write("go\n")
        out, err = proc.communicate(timeout=30)
        receipt = json.loads([ln for ln in out.decode().splitlines() if ln.strip()][-1])
        self.fx.collect(receipt)  # U11 (R5d-b-6)
        self.assertEqual((proc.returncode, receipt["result"], receipt["reason"]), (6, "REFUSED", "DRIFT_BEFORE_WRITE"), receipt)
        self.assertFalse(os.path.exists(self.fx.runtime_parent))

    def test_u4a_post_creation_check_failure_after_runtime_parent_is_exit_7(self):
        self.fx.write_settings(OTHER_SETTINGS)
        original = read_bytes(self.fx.settings)
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("post-creation-check"))
        self.assertEqual((rc, receipt["result"], receipt["original_code"]), (7, "FAILED", 2), (receipt, err))
        self.assertEqual(receipt["reason"], "RUNTIME_PARENT_NOT_REAL_DIRECTORY")
        self.assertEqual(receipt["runtime_parent_created"], self.fx.runtime_parent)
        self.assertIn("runtime_renamed_to", receipt)
        self.assertIsNone(receipt["runtime_renamed_to"])  # there was no runtime directory to archive
        self.assertTrue(os.path.isdir(self.fx.runtime_parent))  # nothing is deleted
        self.assertEqual(os.listdir(self.fx.runtime_parent), [])
        self.assertEqual(read_bytes(self.fx.settings), original)
        self.assertIn(self.as_saved(receipt), [saved for _, saved in self.receipts()])  # rev27 Z4

    def test_u4b_post_creation_check_failure_after_the_runtime_dir_is_exit_7_and_archived(self):
        self.fx.write_settings(OTHER_SETTINGS)
        original = read_bytes(self.fx.settings)
        os.mkdir(self.fx.runtime_parent)
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("post-creation-check"))
        self.assertEqual((rc, receipt["result"], receipt["original_code"]), (7, "FAILED", 2), (receipt, err))
        self.assertEqual(receipt["reason"], "RUNTIME_DIR_NOT_CONTAINED")
        self.assertEqual(len(self.aborted()), 1)
        self.assertEqual(receipt["runtime_renamed_to"], self.fx.runtime_parent + "/" + self.aborted()[0])
        self.assertFalse(os.path.exists(self.fx.runtime_dir()))
        self.assertEqual(read_bytes(self.fx.settings), original)

    def test_u4c_source_replaced_after_the_preimage_copy_is_exit_7_with_the_archive_named(self):
        """R5c-1 (b) with the existing barrier seam: the source becomes a symlink after the preimage copy; the copy step's
        SOURCE_NOT_CONTAINED (code 2) after the <HASH12> mkdir is FAILED / exit 7, never REFUSED / exit 2."""
        self.fx.write_settings(OTHER_SETTINGS)
        rc, receipt, err = self.run_with_barrier_action("install", self.replace_source_by_symlink)
        self.assertEqual((rc, receipt["result"], receipt["reason"], receipt["original_code"]),
                         (7, "FAILED", "SOURCE_NOT_CONTAINED", 2), (receipt, err))
        self.assertEqual(len(self.aborted()), 1)
        self.assertEqual(receipt["runtime_renamed_to"], self.fx.runtime_parent + "/" + self.aborted()[0])
        self.assertEqual(receipt["preimage_backup_created"], receipt["preimage"]["backup_path"])
        self.assertEqual(read_bytes(self.fx.settings), doc_bytes(OTHER_SETTINGS))

    def replace_source_by_symlink(self):
        real = self.fx.outside + "/guard.py"
        with open(real, "wb") as fh:
            fh.write(GUARD_V1)
        os.unlink(self.fx.source)
        os.symlink(real, self.fx.source)

    def run_with_barrier_action(self, mode, action):
        barrier = self.fx.root + "/barrier"
        os.mkdir(barrier)
        manifest = self.fx.promote_manifest if mode == "promote" else self.fx.manifest
        argv = [sys.executable, "-I", INSTALLER, mode, "--manifest", manifest,
                "--expect-sha256", sha256_file(INSTALLER), "--expect-manifest-sha256", sha256_file(manifest),
                "--test-root", self.fx.root, "--test-barrier", barrier]
        proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.fx.root)
        deadline = time.time() + 20
        while not os.path.exists(barrier + "/reached"):
            if time.time() > deadline or proc.poll() is not None:
                proc.kill()
                self.fail("installer never reached the test barrier")
            time.sleep(0.05)
        action()
        with open(barrier + "/release", "w") as fh:
            fh.write("go\n")
        out, err = proc.communicate(timeout=30)
        receipt = json.loads([ln for ln in out.decode().splitlines() if ln.strip()][-1])
        self.fx.collect(receipt)
        return proc.returncode, receipt, err.decode()

    def test_u5_verify_treats_a_malformed_hooks_structure_as_no_registration(self):
        self.fx.write_settings({"hooks": []})
        os.makedirs(self.fx.runtime_dir())
        receipt = self.assert_verify("INCOMPLETE_INSTALL")
        self.assertEqual(receipt["registration_error"], "SETTINGS_HOOKS_NOT_OBJECT")
        os.rename(self.fx.runtime_dir(), self.fx.runtime_dir() + ".aborted-20261014T000000Z-1")
        self.fx.write_settings({"hooks": None})
        self.assert_verify("INCOMPLETE_INSTALL")
        os.rename(self.fx.runtime_parent, self.fx.root + "/runtime-parent-away")
        for doc in self.MALFORMED_HOOKS:
            with self.subTest(settings=doc):
                self.fx.write_settings(doc)
                rc, receipt, out, err = self.fx.run("verify")
                self.assertEqual((rc, receipt["result"]), (1, "NOT_INSTALLED"), (receipt, err))
                self.assertNotIn("Traceback", err)

    def test_r5a2_phase_p_refusal_with_unverified_evidence_still_enters_the_scope(self):
        """R5a-2 / R5c-4: every writing mode enters the deferral scope after phase P, also when the evidence directory is
        not verified, so a Ctrl+C while the refusal is reported is deferred instead of ending in a traceback."""
        os.rename(self.fx.evidence_dir, self.fx.evidence_dir + ".away")
        try:
            rc, receipt, err = self.run_with_pause("install", [signal.SIGINT])
            self.assertEqual((rc, receipt["result"], receipt["reason"]), (2, "REFUSED", "EVIDENCE_DIR_NOT_REAL_DIRECTORY"), receipt)
            self.assertEqual((receipt["interrupt_deferred"], receipt["interrupt_signal"]), (True, "SIGINT"))
            self.assertFalse(receipt["receipt_saved"])
            self.assertNotIn("Traceback", err)
        finally:
            os.rename(self.fx.evidence_dir + ".away", self.fx.evidence_dir)

    def test_r5b3_refusal_receipt_is_written_inside_the_scope(self):
        """R5b-3: main enters the scope before a refusal receipt is written; the refusal keeps its code (D4)."""
        self.fx.write_settings({"hooks": {"PermissionRequest": [{"matcher": "Bash", "hooks": [
            {"type": "command", "command": "echo other"}]}]}})
        original = read_bytes(self.fx.settings)
        rc, receipt, err = self.run_with_pause("install", [signal.SIGINT])
        self.assertEqual((rc, receipt["result"], receipt["reason"]), (3, "REFUSED", "FOREIGN_HOOK"), (receipt, err))
        self.assertEqual((receipt["interrupt_deferred"], receipt["interrupt_signal"]), (True, "SIGINT"))
        self.assertIn(self.as_saved(receipt), [saved for _, saved in self.receipts()])  # rev27 Z4
        self.assertEqual(read_bytes(self.fx.settings), original)

    def test_r5a3_partial_preimage_copy_and_receipt_are_named(self):
        """R5a-3 / R5c-5: a write that fails after its file was created leaves a file the receipt names (here the file
        size limit makes the preimage copy and the receipt file fail part-way)."""
        import resource
        self.fx.write_settings(dict(OTHER_SETTINGS, padding="x" * 4096))
        argv = [sys.executable, "-I", INSTALLER, "install", "--manifest", self.fx.manifest,
                "--expect-sha256", sha256_file(INSTALLER), "--expect-manifest-sha256", self.fx.manifest_sha,
                "--test-root", self.fx.root]
        limit = lambda: resource.setrlimit(resource.RLIMIT_FSIZE, (1024, 1024))
        proc = subprocess.run(argv, capture_output=True, cwd=self.fx.root, timeout=60, preexec_fn=limit)
        receipt = json.loads([ln for ln in proc.stdout.decode().splitlines() if ln.strip()][-1])
        self.fx.collect(receipt)  # U11 covers this part-way preimage copy failure too
        self.assertEqual((proc.returncode, receipt["result"]), (7, "FAILED"), receipt)
        self.assertIn("runtime_renamed_to", receipt)  # rev25 Y3 (R5c-c-3): documented, null: nothing to archive
        self.assertIsNone(receipt["runtime_renamed_to"])
        partial = receipt["preimage_backup_created"]
        self.assertEqual(os.path.dirname(partial), self.fx.evidence_dir)
        self.assertTrue(os.path.basename(partial).startswith("settings.local.json.preimage-"))
        self.assertTrue(os.path.isfile(partial))
        self.assertFalse(os.path.exists(self.fx.runtime_parent))
        self.assertFalse(receipt["receipt_saved"])
        leftover = receipt["receipt_save_temp_path"]
        self.assertTrue(os.path.isfile(leftover))
        self.assertEqual(self.receipts_with_final_names(), [])

    def receipts_with_final_names(self):
        return [name for name, _ in self.receipts_listing() if not name.startswith(".")]

    def receipts_listing(self):
        directory = self.fx.evidence_dir + "/receipts"
        return [(name, None) for name in sorted(os.listdir(directory))] if os.path.isdir(directory) else []

    def test_r5a4_a_failed_print_never_changes_the_exit_code(self):
        """R5a-4 / R5c-3: the receipt cannot be printed (the reader closed stdout); the exit code stays the outcome's (no
        signal is sent, so no D3 line is expected) and the receipt file on disk is the complete INSTALLED receipt."""
        rc, err = self.run_with_closed_stdout(self.fault("pause-in-scope"))
        self.assertEqual(rc, 0, err)
        self.assertNotIn("Exception ignored", err)
        self.assertNotIn("Traceback", err)
        [(_, saved)] = self.receipts()
        self.assertEqual((saved["result"], saved["exit_code"]), ("INSTALLED", 0))

    def run_with_closed_stdout(self, faults):
        """Install with pause-in-scope; while the installer is paused the test closes its end of the stdout pipe, then
        releases the pause; returns (exit code, stderr). Every wait is bounded."""
        reached, release = self.fx.root + "/pause-reached", self.fx.root + "/pause-release"
        argv = [sys.executable, "-I", INSTALLER, "install", "--manifest", self.fx.manifest,
                "--expect-sha256", sha256_file(INSTALLER), "--expect-manifest-sha256", self.fx.manifest_sha,
                "--test-root", self.fx.root] + list(faults)
        proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.fx.root)
        deadline = time.time() + 20
        while not os.path.exists(reached):
            if time.time() > deadline or proc.poll() is not None:
                proc.kill()
                proc.communicate(timeout=10)
                self.fail("installer never reached pause-in-scope")
            time.sleep(0.05)
        proc.stdout.close()
        proc.stdout = None  # communicate() then reads stderr only
        with open(release, "w") as fh:
            fh.write("go\n")
        _, err = proc.communicate(timeout=60)
        if os.path.isfile(self.fx.state_path()):
            self.fx.collect_state()  # U11 (R5d-b-6): no printed receipt, but the state the run wrote is checked
        return proc.returncode, err.decode()

    def test_r5a4_d3_line_says_neither_saved_nor_printed(self):
        rc, err = self.run_with_closed_stdout(self.fault("pause-in-scope", "signal-after-receipt-snapshot", "receipt-save"))
        self.assertEqual(rc, 7, err)
        notes = [ln for ln in err.splitlines() if "after the final receipt's snapshot" in ln]
        self.assertEqual(len(notes), 1, err)
        self.assertIn("neither written to disk nor printed", notes[0])
        self.assertNotIn("only printed", notes[0])
        self.assertNotIn("Exception ignored", err)

    # rev23 7.11e (2): X1 (U4c-U4e), X2 (U8) and the closures of the round-5b re-examination ---------------------------
    DEEP_SETTINGS = b'{"hooks": ' + b"[" * 100000 + b"]" * 100000 + b"}\n"

    def test_u4c_parent_only_the_runtime_parent_creation_alone_is_a_transaction_write(self):
        """X1, parent only: the settings target and RUNTIME_PARENT are absent before the run, so no preimage copy or other
        transaction write precedes the RUNTIME_PARENT mkdir; its strict check fails -> FAILED / exit 7."""
        self.assertFalse(os.path.exists(self.fx.settings))
        self.assertFalse(os.path.exists(self.fx.runtime_parent))
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("post-creation-check"))
        self.assertEqual((rc, receipt["result"], receipt["reason"], receipt["original_code"]),
                         (7, "FAILED", "RUNTIME_PARENT_NOT_REAL_DIRECTORY", 2), (receipt, err))
        self.assertIsNone(receipt["preimage_backup_created"])  # nothing else can stand in for the parent creation
        self.assertEqual(receipt["runtime_parent_created"], self.fx.runtime_parent)
        self.assertIsNone(receipt["runtime_renamed_to"])
        self.assertEqual(receipt["failed_step"], "runtime parent")  # R5b-c-3
        self.assertTrue(os.path.isdir(self.fx.runtime_parent))
        self.assertEqual(os.listdir(self.fx.runtime_parent), [])
        self.assertFalse(os.path.exists(self.fx.settings))
        self.assertFalse(os.path.exists(self.fx.state_path()))
        self.assertIn(self.as_saved(receipt), [saved for _, saved in self.receipts()])  # rev27 Z4

    def test_u4d_child_only_the_runtime_dir_creation_alone_is_a_transaction_write(self):
        """X1, child only: RUNTIME_PARENT is created by the fixture (never by the run), the settings target and <HASH12>
        are absent; the run's only transaction write before the boundary is the <HASH12> mkdir. transaction-boundary
        records the REFUSED-versus-FAILED classification at the post-mkdir boundary, before any archive, and halts."""
        os.mkdir(self.fx.runtime_parent)
        self.assertFalse(os.path.exists(self.fx.settings))
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("transaction-boundary"))
        boundary = receipt["test_transaction_boundary"]
        self.assertEqual((boundary["path"], boundary["transaction_written"], boundary["outcome_if_refused_here"]),
                         (self.fx.runtime_dir(), True, "FAILED"), (receipt, err))
        self.assertIsNone(receipt["runtime_parent_created"])  # the parent is not attributed to this run
        self.assertIsNone(receipt["preimage_backup_created"])

    def test_u4e_child_only_failure_archives_the_runtime_dir_once(self):
        """X1, archive integration: the U4d fixture run to its end; the archive assertions here never stand in for U4d."""
        os.mkdir(self.fx.runtime_parent)
        self.assertFalse(os.path.exists(self.fx.settings))
        before = snapshot(self.fx.root)
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("post-creation-check"))
        self.assertEqual((rc, receipt["result"], receipt["reason"], receipt["original_code"]),
                         (7, "FAILED", "RUNTIME_DIR_NOT_CONTAINED", 2), (receipt, err))
        self.assertEqual(len(self.aborted()), 1)
        self.assertEqual(receipt["runtime_renamed_to"], self.fx.runtime_parent + "/" + self.aborted()[0])
        self.assertIsNone(receipt["runtime_parent_created"])  # rev25 Y3: documented, null (not created by this run)
        self.assertTrue(os.path.isdir(self.fx.runtime_parent))
        self.assertFalse(os.path.exists(self.fx.runtime_dir()))
        self.assertEqual(receipt["failed_step"], "runtime copy")  # R5b-c-3
        # R5c-a-5: nothing deleted and nothing else created: apart from receipts/, the only change is the <HASH12>
        # archive (an empty directory), and no settings target or state file appeared
        self.assertFalse(os.path.exists(self.fx.settings))
        self.assertFalse(os.path.exists(self.fx.state_path()))
        after = snapshot(self.fx.root)
        self.assertEqual([k for k in before if k not in after], [])  # nothing deleted
        archive = os.path.relpath(receipt["runtime_renamed_to"], self.fx.root)
        added = sorted(k for k in after if k not in before)
        self.assertEqual([k for k in added if not k.startswith(os.path.relpath(self.fx.evidence_dir, self.fx.root)
                                                               + "/receipts")], [archive])
        self.assertEqual(after[archive], "dir")
        self.assertEqual({k: v for k, v in before.items()}, {k: after[k] for k in before})

    def test_u8_deep_settings_in_verify_keeps_the_precedence(self):
        """X2: a settings document nested deeper than the recursion limit is unparseable JSON, never an unexpected error."""
        self.fx.write_settings(raw=self.DEEP_SETTINGS)
        receipt = self.assert_verify("NOT_INSTALLED")  # (i) nothing on disk
        self.assertEqual(receipt["registration_error"], "SETTINGS_MALFORMED")
        os.makedirs(self.fx.runtime_parent)
        os.mkdir(self.fx.runtime_dir() + ".aborted-20261014T000000Z-1")
        self.assert_verify("INCOMPLETE_INSTALL")  # (ii) an orphan .aborted- archive, no state

    def test_u8_deep_settings_with_a_committed_or_unreadable_state(self):
        self.install_ok()
        self.fx.write_settings(raw=self.DEEP_SETTINGS)
        receipt = self.assert_verify("DRIFT")  # (iii) a committed state
        self.assertEqual(receipt["registration_error"], "SETTINGS_MALFORMED")
        os.chmod(self.fx.state_path(), 0o000)
        try:
            self.assert_verify("STATE_UNREADABLE", state_refusal="STATE_UNREADABLE")  # (iv) an unreadable state file
        finally:
            os.chmod(self.fx.state_path(), 0o600)
        with open(self.fx.state_path(), "wb") as fh:  # a state file nested too deep is a malformed state (F7)
            fh.write(b'{"a": ' + b"[" * 100000 + b"]" * 100000 + b"}\n")
        self.assert_verify("STATE_UNREADABLE", state_refusal="STATE_MALFORMED")

    def test_u8_deep_settings_in_install_rollback_and_promote(self):
        self.fx.write_settings(raw=self.DEEP_SETTINGS)
        before = snapshot(self.fx.root)
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual((rc, receipt["result"], receipt["reason"]), (2, "REFUSED", "SETTINGS_MALFORMED"), (receipt, err))
        self.assert_only_receipts_changed(before)
        os.unlink(self.fx.settings)
        self.install_ok()
        self.fx.write_settings(raw=self.DEEP_SETTINGS)
        # rollback hashes the settings target and never parses it: its drift refusal is reached unchanged
        self.assert_refused(self.fx.run("rollback"), 4, "SETTINGS_DRIFT")
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        before = snapshot(self.fx.root)
        rc, receipt, out, err = self.fx.run("promote")
        self.assertEqual((rc, receipt["result"], receipt["reason"]), (2, "REFUSED", "SETTINGS_MALFORMED"), (receipt, err))
        self.assertNotIn("Traceback", err)
        self.assert_only_receipts_changed(before)

    def test_r5b_b3_unencodable_document_and_invalid_entries_are_phase_p_refusals(self):
        cases = ((b'{"x": "\\ud800"}\n', "SETTINGS_NOT_ENCODABLE"),
                 (doc_bytes({"hooks": {"PermissionRequest": [{"hooks": "x"}]}}), "SETTINGS_PERMISSION_REQUEST_ENTRY_INVALID"),
                 (doc_bytes({"hooks": {"PermissionRequest": [{"hooks": [{"command": 1}]}]}}),
                  "SETTINGS_PERMISSION_REQUEST_ENTRY_INVALID"))
        for raw, reason in cases:
            with self.subTest(reason=reason, raw=raw[:40]):
                self.fx.write_settings(raw=raw)
                before = snapshot(self.fx.root)
                rc, receipt, out, err = self.fx.run("install")
                self.assertEqual((rc, receipt["result"], receipt["reason"]), (2, "REFUSED", reason), (receipt, err))
                self.assert_only_receipts_changed(before)

    def test_r5b_b4_verify_step_2_reads_a_malformed_hooks_structure_as_no_registration(self):
        self.install_ok()
        self.fx.write_settings({"hooks": None})
        receipt = self.assert_verify("DRIFT")
        self.assertEqual(receipt["registration_error"], "SETTINGS_HOOKS_NOT_OBJECT")

    def test_r5b_a1_promote_refusal_after_the_commit_is_exit_7(self):
        """R5b-a-1: the existing barrier reaches the post-commit rename in promote: the old runtime dir becomes a symlink
        after phase P, the settings replacement commits, and the rename's refusal (code 2) is FAILED / exit 7."""
        self.install_ok()
        old_dir = self.fx.runtime_dir()
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        rc, receipt, err = self.run_with_barrier_action("promote", lambda: self.replace_with_symlink(old_dir))
        self.assertEqual((rc, receipt["result"], receipt["reason"], receipt["original_code"]),
                         (7, "FAILED", "RENAME_SOURCE_NOT_CONTAINED", 2), (receipt, err))
        self.assertTrue(receipt["settings_written"])
        state = self.fx.state()
        self.assertEqual((state["result"], state["settings_written"], state["runtime_dir"]), ("FAILED", True, self.fx.runtime_dir()))
        self.fx.write_manifest()  # verify reads the install manifest: re-frozen for the v2 source
        rc, receipt, out, err = self.fx.run("verify")
        self.assertNotEqual(receipt["result"], "NOT_INSTALLED", receipt)

    def test_r5b_c3_promote_pre_commit_failure_names_its_step(self):
        self.install_ok()
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        rc, receipt, out, err = self.fx.run("promote", extra=self.fault("post-creation-check"))
        self.assertEqual((rc, receipt["result"], receipt["original_code"]), (7, "FAILED", 2), (receipt, err))
        self.assertEqual(receipt["failed_step"], "runtime copy")

    def test_r5b_c5_promote_without_the_install_manifest_prints_no_unusable_rollback_command(self):
        self.install_ok()
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        os.rename(self.fx.manifest, self.fx.manifest + ".away")
        rc, receipt, out, err = self.fx.run("promote")
        self.assertEqual((rc, receipt["result"]), (0, "PROMOTED"), (receipt, err))
        self.assertIsNone(receipt["rollback_command"])
        self.assertEqual(receipt["rollback_command_unavailable"], "INSTALL_MANIFEST_MISSING")

    # rev25 7.11f (2): Y1 to Y5 (U9 to U13) and the closures of the round-5c re-examination ---------------------------
    BAD_PATH_SHAPES = (
        ("314-character basename", lambda p: os.path.dirname(p) + "/" + (os.path.basename(p) + "x" * 314)[:314]),
        ("lone high surrogate U+D800", lambda p: p + "\ud800"),
        ("lone low surrogate U+DC80", lambda p: p + "\udc80"),
        ("NUL", lambda p: p + "\x00"),
        ("relative path", lambda p: p.lstrip("/")),
        ("`..` component", lambda p: (os.path.dirname(p) + "/../" + os.path.basename(os.path.dirname(p)) + "/"
                                      + os.path.basename(p))),
    )

    def install_then_damage_state(self, keys, damage):
        """A PRESENT preimage install, then DAMAGE applied to the state field at the key path KEYS."""
        self.fx.write_settings(OTHER_SETTINGS)
        self.install_ok()

        def mutate(state):
            holder = state
            for key in keys[:-1]:
                holder = holder[key]
            holder[keys[-1]] = damage(holder.get(keys[-1]))
        self.fx.edit_state(mutate)

    def assert_untrustworthy_state_everywhere(self, reason, label):
        """Y1/Y2: verify gives an untrustworthy-state outcome (exit 1, the cause in state_refusal), rollback and promote
        refuse in phase P with REASON (exit 2); never UNEXPECTED_ERROR, never a traceback, nothing written but receipts."""
        before = snapshot(self.fx.root)
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["state_refusal"]), (1, reason), (label, receipt, err))
        self.assertIn(receipt["result"], ("STATE_UNREADABLE", "INCOMPLETE_INSTALL"), (label, receipt))
        self.assertNotIn("Traceback", err)
        self.assertEqual(before, snapshot(self.fx.root), label)  # verify writes nothing
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual((rc, receipt["result"], receipt["reason"]), (2, "REFUSED", reason), (label, receipt, err))
        self.assertNotIn("Traceback", err)
        self.assert_only_receipts_changed(before)
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        before = snapshot(self.fx.root)
        rc, receipt, out, err = self.fx.run("promote")
        self.assertEqual((rc, receipt["result"], receipt["reason"]), (2, "REFUSED", reason), (label, receipt, err))
        self.assertNotIn("Traceback", err)
        self.assert_only_receipts_changed(before)

    def test_u9_untrusted_state_paths_are_an_untrustworthy_state_in_every_mode(self):
        """Y1: every path read from the state, damaged in each adversarial shape, is an untrustworthy state in verify,
        rollback and promote before any transaction write."""
        fields = ((("settings_target",), "STATE_SETTINGS_TARGET_MISMATCH"),
                  (("runtime_dir",), "STATE_RUNTIME_NOT_PINNED"),
                  (("runtime_path",), "STATE_RUNTIME_NOT_PINNED"),
                  (("preimage", "backup_path"), "STATE_PREIMAGE_NOT_PINNED"))
        for keys, reason in fields:
            for shape, damage in self.BAD_PATH_SHAPES:
                label = ".".join(keys) + " / " + shape
                with self.subTest(case=label):
                    self.next_fixture()
                    self.install_then_damage_state(keys, damage)
                    self.assert_untrustworthy_state_everywhere(reason, label)

    def test_u9b_a_state_path_recheck_failing_after_the_restore_is_failed_exit_7(self):
        """Y1 (R24-B01): the state-derived runtime directory passes phase P; at the barrier (after phase P, before the
        restore) it becomes a symlink; the restore commits, then the rename's recheck refuses (code 2) -> FAILED / exit 7
        with original_code 2 and the completed restore recorded in the receipt and the state, never a phase-P refusal."""
        self.fx.write_settings(OTHER_SETTINGS)
        original = read_bytes(self.fx.settings)
        self.install_ok()
        runtime_dir = self.fx.runtime_dir()
        rc, receipt, err = self.run_with_barrier_action("rollback", lambda: self.replace_with_symlink(runtime_dir))
        self.assertEqual((rc, receipt["result"], receipt["reason"], receipt["original_code"]),
                         (7, "FAILED", "RENAME_SOURCE_NOT_CONTAINED", 2), (receipt, err))
        self.assertIs(receipt["settings_restored"], True)
        self.assertEqual(receipt["postimage_sha256"], sha256_bytes(original))
        self.assertIsNone(receipt["runtime_renamed_to"])  # the rename did not happen
        self.assertEqual(read_bytes(self.fx.settings), original)  # the restore stands
        state = self.fx.state()
        self.assertEqual((state["mode"], state["result"], state["settings_restored"]), ("rollback", "FAILED", True))
        self.assertTrue(os.path.islink(runtime_dir))  # nothing deleted or renamed after the refusal

    def test_u9c_a_lone_low_surrogate_naming_an_existing_file_is_still_untrustworthy(self):
        """Y1, the surrogate check itself: U+DC80 encodes under surrogateescape to the byte 0x80, so a state can name a
        real file (here a byte-identical copy of the preimage backup); the path is refused for its code point alone."""
        self.fx.write_settings(OTHER_SETTINGS)
        self.install_ok()
        backup = self.fx.state()["preimage"]["backup_path"]
        twin = backup + "\udc80"
        with open(os.fsencode(twin), "wb") as fh:
            fh.write(read_bytes(backup))
        self.fx.edit_state(lambda st: st["preimage"].update(backup_path=twin))
        self.assert_untrustworthy_state_everywhere("STATE_PREIMAGE_NOT_PINNED", "existing file named with U+DC80")

    def test_u9d_an_unsearchable_evidence_parent_is_a_refusal_never_an_unexpected_error(self):
        """Y1, every lstat failure handled: with .scratch readable but not searchable, the evidence root cannot be lstat'ed
        (EACCES) -> a refusal (exit 2), in every mode, never FAILED / UNEXPECTED_ERROR / exit 7."""
        self.install_ok()
        os.chmod(self.fx.evidence_parent, 0o600)
        try:
            for mode in ("verify", "install", "rollback"):
                rc, receipt, out, err = self.fx.run(mode, manifest_sha=self.fx.manifest_sha)
                self.assertEqual((rc, receipt["result"], receipt["reason"]),
                                 (2, "REFUSED", "EVIDENCE_ROOT_NOT_REAL_DIRECTORY"), (mode, receipt, err))
        finally:
            os.chmod(self.fx.evidence_parent, 0o700)

    def test_u9e_an_unsearchable_runtime_parent_never_lets_rollback_skip_the_runtime_rename(self):
        """Y1: an existence check of the state's runtime directory that fails for another reason than "absent" (EACCES) is
        a refusal before anything is written, never read as "absent" (which would roll back the settings, keep the runtime
        directory active-named and report ROLLED_BACK, exit 0)."""
        self.fx.write_settings(OTHER_SETTINGS)
        self.install_ok()
        installed = read_bytes(self.fx.settings)
        os.chmod(self.fx.runtime_parent, 0o600)
        try:
            rc, receipt, out, err = self.fx.run("rollback")
        finally:
            os.chmod(self.fx.runtime_parent, 0o755)
        self.assertEqual((rc, receipt["result"], receipt["reason"]), (2, "REFUSED", "RENAME_SOURCE_NOT_CONTAINED"),
                         (receipt, err))
        self.assertEqual(read_bytes(self.fx.settings), installed)
        self.assertTrue(os.path.isdir(self.fx.runtime_dir()))
        self.assert_verify("MATCH")

    def test_u10_malformed_state_values_are_an_untrustworthy_state(self):
        """Y2: every non-path value read from the state must have the exact type and format the installer writes."""
        cases = [(("preimage", "mode"), value, "STATE_PREIMAGE_MALFORMED")
                 for value in ("rw", 420, None, "0o77777777777", True, "0o0644", "644", "0o10000")]
        cases += [(("preimage", "size"), value, "STATE_PREIMAGE_MALFORMED") for value in (True, -1, "12", 1.5)]
        cases += [(("preimage", "mtime"), value, "STATE_PREIMAGE_MALFORMED") for value in (True, "x", None)]
        cases += [(("preimage", "status"), "X", "STATE_PREIMAGE_MALFORMED"),
                  (("preimage", "sha256"), 1, "STATE_PREIMAGE_NOT_PINNED"),
                  (("settings_written",), "yes", "STATE_MALFORMED"),
                  (("settings_restored",), 1, "STATE_MALFORMED"),
                  (("result",), 5, "STATE_MALFORMED"),
                  (("reason",), 5, "STATE_MALFORMED"),
                  (("postimage_unknown",), True, "STATE_MALFORMED"),
                  (("postimage_sha256",), 123, "STATE_HASH_MALFORMED"),
                  (("intended_postimage_sha256",), True, "STATE_HASH_MALFORMED"),
                  (("runtime_sha256",), None, "STATE_HASH_MALFORMED"),
                  (("registration_command",), 5, "STATE_REGISTRATION_MISSING")]
        for keys, value, reason in cases:
            label = ".".join(keys) + " = " + repr(value)
            with self.subTest(case=label):
                self.next_fixture()
                self.install_then_damage_state(keys, lambda _old, value=value: value)
                self.assert_untrustworthy_state_everywhere(reason, label)

    def test_u12_runtime_copy_created_is_true_only_for_a_complete_verified_copy(self):
        """Y4: runtime_copy_created is true only once the runtime file is written in full and verified, and the archived
        <HASH12> holds exactly what the receipt says."""
        def no_copy(fault=None, parent=False, barrier_action=None):
            if parent:
                os.mkdir(self.fx.runtime_parent)
            if barrier_action is not None:
                rc, receipt, err = self.run_with_barrier_action("install", barrier_action)
            else:
                rc, receipt, out, err = self.fx.run("install", extra=self.fault(fault))
            return rc, receipt, err
        cases = (("strict check after the <HASH12> mkdir", dict(fault="post-creation-check", parent=True)),
                 ("runtime copy write fault", dict(fault="runtime-copy-write")),
                 ("source changed before the copy", dict(barrier_action=lambda: self.fx.write_source(GUARD_V2))))
        for label, kwargs in cases:
            with self.subTest(case=label):
                self.next_fixture()
                self.fx.write_settings(OTHER_SETTINGS)
                rc, receipt, err = no_copy(**kwargs)
                self.assertEqual((rc, receipt["result"]), (7, "FAILED"), (label, receipt, err))
                self.assertIs(receipt["runtime_copy_created"], False, (label, receipt))
                self.assertIsNone(receipt["runtime_sha256"], (label, receipt))
                self.assertEqual(os.listdir(receipt["runtime_renamed_to"]), [], (label, receipt))
        # positive controls: a failure after the verified copy, and a success
        self.next_fixture()
        self.fx.write_settings(OTHER_SETTINGS)
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("interrupt-before-replace"))
        self.assertEqual((rc, receipt["result"], receipt["runtime_copy_created"]), (7, "FAILED", True), (receipt, err))
        archived = receipt["runtime_renamed_to"] + "/permission_guard.py"
        self.assertEqual(os.listdir(receipt["runtime_renamed_to"]), ["permission_guard.py"])
        self.assertEqual((sha256_file(archived), stat.S_IMODE(os.lstat(archived).st_mode)), (self.fx.source_sha, 0o444))
        self.next_fixture()
        receipt = self.install_ok()
        self.assertIs(receipt["runtime_copy_created"], True)
        self.assertEqual(sha256_file(self.fx.runtime_path()), self.fx.source_sha)

    NONFINITE_SETTINGS = (b'{"x": 1e400}\n', b'{"x": -1e400}\n', b'{"x": NaN}\n', b'{"x": Infinity}\n',
                          b'{"x": -Infinity}\n')

    def test_u13_read_non_finite_numbers_are_malformed_settings(self):
        """Y5 on read: NaN, Infinity, -Infinity and numbers that become non-finite floats are malformed JSON."""
        for raw in self.NONFINITE_SETTINGS:
            with self.subTest(document=raw):
                self.next_fixture()
                self.fx.write_settings(raw=raw)
                before = snapshot(self.fx.root)
                rc, receipt, out, err = self.fx.run("install")
                self.assertEqual((rc, receipt["result"], receipt["reason"]), (2, "REFUSED", "SETTINGS_MALFORMED"),
                                 (raw, receipt, err))
                self.assertEqual(read_bytes(self.fx.settings), raw)
                self.assert_only_receipts_changed(before)
                receipt = self.assert_verify("NOT_INSTALLED")  # 11.3 step 4: nothing on disk
                self.assertEqual(receipt["registration_error"], "SETTINGS_MALFORMED")
                os.unlink(self.fx.settings)
                self.install_ok()
                self.fx.write_settings(raw=raw)
                receipt = self.assert_verify("DRIFT")  # 11.3 step 2: a committed state
                self.assertEqual(receipt["registration_error"], "SETTINGS_MALFORMED")

    def test_u13_read_non_finite_numbers_in_the_state_and_the_manifest(self):
        """Y5 on read for the state file and the manifests (with R5c-a-4 / R5c-b-8: every parsed file)."""
        for token in (b"NaN", b"Infinity", b"-Infinity", b"1e400", b"-1e400"):
            with self.subTest(state_token=token):
                self.next_fixture()
                self.install_ok()
                data = read_bytes(self.fx.state_path()).rstrip()
                self.assertTrue(data.endswith(b"}"))
                with open(self.fx.state_path(), "wb") as fh:
                    fh.write(data[:-1] + b', "x": ' + token + b"}\n")
                self.assert_untrustworthy_state_everywhere("STATE_MALFORMED", "state " + token.decode())
            with self.subTest(manifest_token=token):
                self.next_fixture()
                doc = json.dumps(self.fx.manifest_doc(), indent=2).encode().rstrip()
                self.fx.write_manifest(raw=doc[:-1] + b', "x": ' + token + b"}\n")
                for mode in ("install", "verify", "rollback"):
                    self.assert_refused(self.fx.run(mode), 2, "MANIFEST_MALFORMED")
                self.fx.write_manifest(raw=doc[:-1] + b', "x": ' + token + b"}\n", path=self.fx.promote_manifest)
                self.assert_refused(self.fx.run("promote"), 2, "MANIFEST_MALFORMED")

    def assert_no_non_finite_token_anywhere(self):
        """U13-write: no file under the test root holds a NaN or Infinity token, and every JSON file there is strict."""
        for root, dirs, files in os.walk(self.fx.root):
            for name in files:
                path = os.path.join(root, name)
                if os.path.islink(path) or name == "insert-before-serializer":
                    continue
                data = read_bytes(path)
                self.assertNotIn(b"Infinity", data, path)
                self.assertNotIn(b"NaN", data, path)
                if name.endswith(".json") and not name.startswith("."):
                    strict_json(data)

    def test_u13_write_the_serializer_refuses_a_non_finite_value_on_the_production_path(self):
        """Y5 on write (R24-C01): the test point insert-before-serializer puts the value read from
        <test root>/insert-before-serializer into the validated in-memory settings document immediately before the
        production serializer runs; input validation stays in force."""
        for text in ("inf", "-inf", "nan"):
            with self.subTest(value=text):
                self.next_fixture()
                self.fx.write_settings(OTHER_SETTINGS)
                with open(self.fx.root + "/insert-before-serializer", "w") as fh:
                    fh.write(text + "\n")
                before = snapshot(self.fx.root)
                rc, receipt, out, err = self.fx.run("install", extra=self.fault("insert-before-serializer"))
                self.assertEqual((rc, receipt["result"], receipt["reason"]), (2, "REFUSED", "SETTINGS_NOT_ENCODABLE"),
                                 (text, receipt, err))
                self.assert_only_receipts_changed(before)
                self.assert_no_non_finite_token_anywhere()
        self.next_fixture()  # a finite control value through the same path is written
        self.fx.write_settings(OTHER_SETTINGS)
        with open(self.fx.root + "/insert-before-serializer", "w") as fh:
            fh.write("1.5\n")
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("insert-before-serializer"))
        self.assertEqual((rc, receipt["result"]), (0, "INSTALLED"), (receipt, err))
        self.assertEqual(strict_json(read_bytes(self.fx.settings))["test_inserted_value"], 1.5)
        self.assert_no_non_finite_token_anywhere()
        self.assert_verify("MATCH")

    def test_r5c_b1_depth_between_the_recursion_limit_and_the_decoder_limit_installs(self):
        """R5c-b-1: a document deeper than the interpreter's recursion limit (1000) that the decoder can follow is valid
        JSON: install re-encodes it and writes it, verify reads it back (MATCH)."""
        self.fx.write_settings(raw=b'{"x": ' + b"[" * 5000 + b"]" * 5000 + b"}\n")
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual((rc, receipt["result"]), (0, "INSTALLED"), (receipt, err))
        self.assert_verify("MATCH")

    def test_r5c_b3_promote_failure_after_it_created_the_runtime_parent_names_that_step(self):
        """R5c-b-3 (R5b-c-3 scenario (b)): promote creates RUNTIME_PARENT itself and its strict check fails."""
        self.install_ok()
        os.rename(self.fx.runtime_parent, self.fx.root + "/parent-away")
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        installed = read_bytes(self.fx.settings)
        rc, receipt, out, err = self.fx.run("promote", extra=self.fault("post-creation-check"))
        self.assertEqual((rc, receipt["result"], receipt["reason"], receipt["original_code"]),
                         (7, "FAILED", "RUNTIME_PARENT_NOT_REAL_DIRECTORY", 2), (receipt, err))
        self.assertEqual(receipt["failed_step"], "runtime parent")
        self.assertEqual(receipt["runtime_parent_created"], self.fx.runtime_parent)
        self.assertIsNone(receipt["runtime_renamed_to"])
        self.assertEqual(read_bytes(self.fx.settings), installed)

    def test_r5c_a4_deep_nesting_in_the_manifest_and_the_state_is_refused_in_every_mode(self):
        """R5c-a-4 / R5c-b-8: the deep-nesting rule of X2 for the manifests (every mode) and the state (rollback, promote)."""
        deep = b'{"a": ' + b"[" * 100000 + b"]" * 100000 + b"}\n"
        self.fx.write_manifest(raw=deep)
        for mode in ("install", "verify", "rollback"):
            self.assert_refused(self.fx.run(mode), 2, "MANIFEST_MALFORMED")
        self.fx.write_manifest(raw=deep, path=self.fx.promote_manifest)
        self.assert_refused(self.fx.run("promote"), 2, "MANIFEST_MALFORMED")
        self.next_fixture()
        self.install_ok()
        with open(self.fx.state_path(), "wb") as fh:
            fh.write(deep)
        self.assert_untrustworthy_state_everywhere("STATE_MALFORMED", "deep state")

    # rev27 7.11g (2): Z1 to Z4 (U14 to U18) -----------------------------------------------------------------------------
    BIG_INTS = (10 ** 400, -(10 ** 400))  # JSON integer literals of 401 digits, beyond the finite float range

    def append_to_state(self, token):
        """Add the raw JSON value TOKEN under a new key to the state file the installer wrote."""
        data = read_bytes(self.fx.state_path()).rstrip()
        self.assertTrue(data.endswith(b"}"))
        with open(self.fx.state_path(), "wb") as fh:
            fh.write(data[:-1] + b', "x": ' + token + b"}\n")

    def test_u14_huge_integers_in_the_state_are_an_untrustworthy_state(self):
        """Z1: +-10**400 as the JSON integer of preimage.mtime and of every other numeric state field -> the
        untrustworthy-state outcome (verify by 11.3; rollback and promote refuse in phase P), never UNEXPECTED_ERROR."""
        for keys in (("preimage", "mtime"), ("preimage", "size"), ("exit_code",), ("original_code",)):
            for value in self.BIG_INTS:
                label = ".".join(keys) + (" = 10**400" if value > 0 else " = -10**400")
                with self.subTest(case=label):
                    self.next_fixture()
                    self.install_then_damage_state(keys, lambda _old, value=value: value)
                    self.assertIn(str(value).encode(), read_bytes(self.fx.state_path()))
                    self.assert_untrustworthy_state_everywhere("STATE_MALFORMED", label)

    def test_u15_integers_beyond_the_finite_float_range_are_malformed_settings(self):
        """Z2: 401 digits (both signs) and 309 nines (309 digits whose conversion to float overflows) are malformed JSON in
        install and verify; so is a 5001-digit literal with the interpreter's digit limit disabled (-X
        int_max_str_digits=0). Controls: exact integers inside the float range are written back byte-identical."""
        cases = ((b"1" + b"0" * 400, ()), (b"-1" + b"0" * 400, ()), (b"9" * 309, ()),
                 (b"1" + b"0" * 5000, ("-X", "int_max_str_digits=0")))
        for literal, flags in cases:
            with self.subTest(digits=len(literal.lstrip(b"-")), sign=literal[:1], flags=flags):
                self.next_fixture()
                raw = b'{"x": ' + literal + b"}\n"
                self.fx.write_settings(raw=raw)
                before = snapshot(self.fx.root)
                rc, receipt, out, err = self.fx.run("install", python_flags=flags)
                self.assertEqual((rc, receipt["result"], receipt["reason"]), (2, "REFUSED", "SETTINGS_MALFORMED"),
                                 (receipt, err))
                self.assertEqual(read_bytes(self.fx.settings), raw)
                self.assert_only_receipts_changed(before)
                rc, receipt, out, err = self.fx.run("verify", python_flags=flags)
                self.assertEqual((rc, receipt["result"], receipt["registration_error"]),
                                 (1, "NOT_INSTALLED", "SETTINGS_MALFORMED"), (receipt, err))
        with self.subTest(case="rollback with 309 nines in the state"):
            self.next_fixture()
            self.fx.write_settings(OTHER_SETTINGS)
            self.install_ok()
            self.append_to_state(b"9" * 309)
            before = snapshot(self.fx.root)
            self.assert_refused(self.fx.run("rollback"), 2, "STATE_MALFORMED")
            self.assert_only_receipts_changed(before)

    def test_u15b_integers_inside_the_finite_float_range_keep_their_exact_literal(self):
        """Z2 controls (a test of their own, so that a mutant of the conversion is killed here and not earlier): 2**53 + 1
        (both signs) and a 308-digit integer below the float maximum install, are written back byte-identical (never
        through a float) and verify MATCH."""
        literals = (b"9007199254740993", b"-9007199254740993", b"1" + b"0" * 307)
        self.fx.write_settings(raw=b'{"a": ' + literals[0] + b', "b": ' + literals[1] + b', "c": ' + literals[2] + b"}\n")
        self.install_ok()
        written = read_bytes(self.fx.settings)
        for literal in literals:
            self.assertIn(b": " + literal + b",", written + b",")  # the exact literal, byte-identical
        self.assert_verify("MATCH")

    def test_u16_verify_reports_an_unreadable_runtime_path_instead_of_reading_it_as_absent(self):
        """Z3: with RUNTIME_PARENT at mode 000 the runtime file cannot be checked: DRIFT with runtime_read_error naming the
        failed operation and EACCES, runtime_sha256 null. Control: an absent runtime file is DRIFT with no read error."""
        self.assertNotEqual(os.geteuid(), 0, "run as an unprivileged user")
        self.install_ok()
        os.chmod(self.fx.runtime_parent, 0o000)
        try:
            rc, receipt, out, err = self.fx.run("verify")
        finally:
            os.chmod(self.fx.runtime_parent, 0o755)
        self.assertEqual((rc, receipt["result"]), (1, "DRIFT"), (receipt, err))
        self.assertIsNone(receipt["runtime_sha256"])
        self.assertIsNotNone(receipt["runtime_read_error"], receipt)
        self.assertIn("lstat", receipt["runtime_read_error"])
        self.assertIn("EACCES", receipt["runtime_read_error"])
        os.rename(self.fx.runtime_path(), self.fx.runtime_path() + ".away")  # control: the runtime file is absent
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual((rc, receipt["result"], receipt["runtime_sha256"], receipt["runtime_read_error"]),
                         (1, "DRIFT", None, None), receipt)

    def test_u17_the_state_directory_fsync_result_is_reported_but_never_written_into_the_state(self):
        """Z4: the fsync of the state file's directory runs after the state is written, so its result is in the receipt
        and on the console, never in the state file, which holds exactly the documented state key set."""
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("state-dir-fsync"))
        self.assertEqual((rc, receipt["result"]), (0, "INSTALLED"), (receipt, err))
        self.assertIn("TEST_FAULT", receipt["state_dir_fsync_error"])
        [(_, saved)] = self.receipts()
        self.assertEqual(saved["state_dir_fsync_error"], receipt["state_dir_fsync_error"])  # written after the fsync
        state = self.fx.state()
        self.assertNotIn("state_dir_fsync_error", state)
        documented = set(RECEIPT_KEYS_BY_MODE["install"]) - set(RECEIPT_ONLY_KEYS) - set(STATE_SAVE_KEYS)
        self.assertEqual({k for k in state if not k.startswith("test_")}, documented)
        self.assert_verify("MATCH")

    def test_u18_every_json_read_site_rejects_a_401_digit_integer_and_nan(self):
        """Z1/Z2 for every read site of r27-json-readers.md that a file reaches: the settings, both manifests and the
        state, each with a 401-digit integer literal and with NaN (the in-memory document copies are bound by code)."""
        for token in (b"1" + b"0" * 400, b"NaN"):
            with self.subTest(site="settings", token=token[:4]):
                self.next_fixture()
                self.fx.write_settings(raw=b'{"x": ' + token + b"}\n")
                self.assert_refused(self.fx.run("install"), 2, "SETTINGS_MALFORMED")
            with self.subTest(site="manifests", token=token[:4]):
                self.next_fixture()
                doc = json.dumps(self.fx.manifest_doc(), indent=2).encode().rstrip()
                self.fx.write_manifest(raw=doc[:-1] + b', "x": ' + token + b"}\n")
                for mode in ("install", "verify", "rollback"):
                    self.assert_refused(self.fx.run(mode), 2, "MANIFEST_MALFORMED")
                self.fx.write_manifest(raw=doc[:-1] + b', "x": ' + token + b"}\n", path=self.fx.promote_manifest)
                self.assert_refused(self.fx.run("promote"), 2, "MANIFEST_MALFORMED")
            with self.subTest(site="state", token=token[:4]):
                self.next_fixture()
                self.install_ok()
                self.append_to_state(token)
                self.assert_untrustworthy_state_everywhere("STATE_MALFORMED", "state " + token[:4].decode())


def main():
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--installer", default=DEFAULT_INSTALLER)
    opts, rest = parser.parse_known_args()
    global INSTALLER
    INSTALLER = os.path.abspath(opts.installer)
    if not os.path.isfile(INSTALLER):
        sys.stderr.write(f"installer not found: {INSTALLER}\n")
        return 2
    program = unittest.main(argv=[sys.argv[0]] + rest, exit=False)
    result = program.result
    if result.testsRun == 0:
        sys.stderr.write("FAIL: zero tests collected\n")
        return 1
    if result.skipped:
        sys.stderr.write(f"FAIL: {len(result.skipped)} test(s) skipped\n")
        return 1
    return 0 if result.wasSuccessful() else 1


if __name__ == "__main__":
    sys.exit(main())
