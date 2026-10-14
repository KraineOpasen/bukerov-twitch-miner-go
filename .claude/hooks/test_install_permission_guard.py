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
              6 DRIFT_BEFORE_WRITE, 7 write failed or interrupted, or a failure after a completed write
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
            test_root="auto", installer=None, timeout=60):
        installer = installer or INSTALLER
        manifest = manifest or (self.promote_manifest if mode == "promote" else self.manifest)
        argv = [sys.executable, "-I", installer, mode, "--manifest", manifest]
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
        return proc.returncode, receipt, proc.stdout.decode(), proc.stderr.decode()


class InstallerTest(unittest.TestCase):
    def setUp(self):
        self.assertNotEqual(os.geteuid(), 0, "these tests must not run as root")
        self.fx = Fixture()

    def tearDown(self):
        for root, dirs, files in os.walk(self.fx.root):
            for name in dirs + files:
                p = os.path.join(root, name)
                if not os.path.islink(p):
                    try:
                        os.chmod(p, 0o700)
                    except OSError:
                        pass
        subprocess.run(["chmod", "-R", "u+w", self.fx.root], capture_output=True)
        import shutil
        shutil.rmtree(self.fx.root, ignore_errors=True)

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

    def test_6_2_preimage_changed_before_write_refused(self):
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
        self.assertEqual(receipt["reason"], "DRIFT_BEFORE_WRITE", err.decode())
        self.assertEqual(proc.returncode, 6)
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
        self.assertNotIn("settings_written", receipt)

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
        return proc.returncode, json.loads(lines[-1]), err.decode()

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
        self.assertNotIn("settings_written", receipt)
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
        self.assertNotIn("interrupted", receipt)
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
        self.assertNotIn("state_save_error", receipt)
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

    def test_r4_post_commit_refusal_is_a_failed_write_exit_7(self):
        """R4-4: a refusal raised after the committed write (here the post-commit rename of a runtime dir that is now a
        symlink) is FAILED / exit 7 with the write recorded, never REFUSED / exit 2, and its state stays usable."""
        self.fx.write_settings(OTHER_SETTINGS)
        preimage = read_bytes(self.fx.settings)
        self.install_ok()
        self.replace_with_symlink(self.fx.runtime_dir())
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual((rc, receipt["result"], receipt["reason"]), (7, "FAILED", "RENAME_SOURCE_NOT_CONTAINED"), receipt)
        self.assertEqual(receipt["original_code"], 2)
        self.assertTrue(receipt["settings_restored"])
        self.assertEqual(read_bytes(self.fx.settings), preimage)
        state = self.fx.state()
        self.assertEqual((state["result"], state["settings_restored"]), ("FAILED", True))
        self.assert_verify("DRIFT")

    def test_r4_post_commit_refusal_in_promote_is_a_failed_write_exit_7(self):
        self.install_ok()
        old_dir = self.fx.runtime_dir()
        self.fx.write_source(GUARD_V2)
        self.write_promote_manifest()
        self.replace_with_symlink(old_dir)
        rc, receipt, out, err = self.fx.run("promote")
        self.assertEqual((rc, receipt["result"], receipt["reason"]), (7, "FAILED", "RENAME_SOURCE_NOT_CONTAINED"), receipt)
        self.assertTrue(receipt["settings_written"])
        self.assertEqual(self.settings_obj()["hooks"]["PermissionRequest"], [self.fx.expected_entry()])
        state = self.fx.state()
        self.assertEqual((state["result"], state["settings_written"], state["runtime_dir"]),
                         ("FAILED", True, self.fx.runtime_dir()))

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
        return proc.returncode, json.loads(lines[-1]), err.decode()

    def assert_deferred(self, receipt, name):
        self.assertEqual((receipt["interrupt_deferred"], receipt["interrupt_signal"]), (True, name), receipt)
        state = self.fx.state()
        self.assertEqual((state["interrupt_deferred"], state["interrupt_signal"]), (True, name), state)
        self.assertNotIn("interrupted", receipt)
        # the receipt file on disk carries the same snapshot as the printed receipt (D3)
        self.assertIn(receipt, [saved for _, saved in self.receipts()])

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
        self.assertNotEqual(rc, 0, (out, err))
        # phase P stays interruptible: the default SIGINT action (KeyboardInterrupt) ended the run, not a refusal
        self.assertEqual((receipt["result"], receipt["reason"], receipt.get("interrupted")), ("FAILED", "INTERRUPTED", True), receipt)
        self.assertNotIn("settings_written", receipt)
        self.assertNotIn("runtime_dir_owned", receipt)
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
        self.assertTrue(saved["receipt_saved"])
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

    def test_t12_signal_after_the_final_observation_changes_nothing(self):
        rc, receipt, out, err = self.fx.run("install", extra=self.fault("signal-after-final-observation"))
        self.assertEqual((rc, receipt["result"]), (0, "INSTALLED"), (receipt, err))
        [(_, saved)] = self.receipts()
        self.assertFalse(saved["interrupt_deferred"])
        self.assertEqual(saved, receipt)  # the saved bytes and the printed receipt are the same snapshot
        self.assertNotIn("after the final receipt's snapshot", err)
        self.assertNotIn("Traceback", err)
        self.assert_verify("MATCH")


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
