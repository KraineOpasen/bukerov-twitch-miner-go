#!/usr/bin/env python3
"""Offline tests for install_permission_guard.py (s1h-contract-rev4, section 6.2).

Run as a script, never through ``-m unittest``::

    python3 -I .claude/hooks/test_install_permission_guard.py -v [--installer PATH]

Every test runs the installer as a subprocess against temporary settings,
runtime and evidence paths, and asserts the installer touched nothing outside
the paths its manifest names. Zero collected tests or any skipped test fails
the run.

Installer contract pinned here (section 11):
  modes install | verify | rollback | promote, each ``--manifest PATH``
  receipt = one JSON object on stdout (the last line)
  exit codes: 0 done/idempotent/MATCH, 1 verify DRIFT, 2 precondition,
              3 FOREIGN_HOOK, 4 rollback/promote drift, 5 runtime dir exists,
              6 DRIFT_BEFORE_WRITE, 7 write failed/interrupted
  state file  <evidence_dir>/permission-guard-state.json (latest receipt)
"""

import argparse
import hashlib
import json
import os
import stat
import subprocess
import sys
import tempfile
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_INSTALLER = os.path.join(HERE, "install_permission_guard.py")
INSTALLER = DEFAULT_INSTALLER

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


def snapshot(root):
    """path -> sha256 (or 'dir'/'symlink') for every entry under root."""
    out = {}
    for dirpath, dirnames, filenames in os.walk(root):
        for d in dirnames:
            p = os.path.join(dirpath, d)
            out[os.path.relpath(p, root)] = "symlink" if os.path.islink(p) else "dir"
        for f in filenames:
            p = os.path.join(dirpath, f)
            if os.path.islink(p):
                out[os.path.relpath(p, root)] = "symlink"
            else:
                out[os.path.relpath(p, root)] = sha256_file(p)
    return out


class Fixture:
    def __init__(self, tmp):
        self.tmp = tmp
        self.settings_dir = os.path.join(tmp, "proj", ".claude")
        os.makedirs(self.settings_dir)
        self.settings = os.path.join(self.settings_dir, "settings.local.json")
        self.runtime_root = os.path.join(tmp, "home", ".claude", "hooks", "permission-guard")
        os.makedirs(os.path.dirname(self.runtime_root))  # the parent exists, as ~/.claude/hooks does
        self.evidence = os.path.join(tmp, "evidence")
        os.makedirs(self.evidence)
        self.source = os.path.join(tmp, "src", "permission_guard.py")
        os.makedirs(os.path.dirname(self.source))
        self.write_source(GUARD_V1)
        self.manifest = os.path.join(tmp, "manifest.json")
        self.write_manifest()
        self.untouched = os.path.join(tmp, "untouched")
        os.makedirs(self.untouched)
        with open(os.path.join(self.untouched, "file.txt"), "w") as fh:
            fh.write("leave me\n")

    def write_source(self, data):
        with open(self.source, "wb") as fh:
            fh.write(data)
        self.source_sha = sha256_bytes(data)
        self.hash12 = self.source_sha[:12]

    def write_manifest(self, overrides=None, remove=(), path=None):
        man = {
            "source": self.source,
            "source_sha256": self.source_sha,
            "runtime_root": self.runtime_root,
            "settings_target": self.settings,
            "evidence_dir": self.evidence,
            "registration": REGISTRATION,
        }
        if overrides:
            man.update(overrides)
        for k in remove:
            man.pop(k, None)
        path = path or self.manifest
        with open(path, "w") as fh:
            json.dump(man, fh, indent=2)
        return path

    def write_settings(self, obj=None, raw=None):
        with open(self.settings, "wb") as fh:
            fh.write(raw if raw is not None else (json.dumps(obj, indent=2) + "\n").encode())

    def runtime_path(self, hash12=None):
        return os.path.join(self.runtime_root, hash12 or self.hash12, "permission_guard.py")

    def expected_command(self, hash12=None, sha=None):
        return TEMPLATE_COMMAND.replace("RUNTIME", self.runtime_path(hash12)).replace(
            "SHA", sha or self.source_sha)

    def expected_entry(self, hash12=None, sha=None):
        return {"matcher": "Bash", "hooks": [{"type": "command",
                                                "command": self.expected_command(hash12, sha),
                                                "timeout": 10}]}

    def run(self, mode, extra=(), manifest=None, installer=None, timeout=60):
        proc = subprocess.run([sys.executable, "-I", installer or INSTALLER, mode,
                               "--manifest", manifest or self.manifest] + list(extra),
                              capture_output=True, cwd=self.tmp, timeout=timeout)
        receipt = None
        lines = [ln for ln in proc.stdout.decode().splitlines() if ln.strip()]
        if lines:
            try:
                receipt = json.loads(lines[-1])
            except ValueError:
                receipt = None
        return proc.returncode, receipt, proc.stdout.decode(), proc.stderr.decode()

    def state(self):
        with open(os.path.join(self.evidence, "permission-guard-state.json")) as fh:
            return json.load(fh)


class InstallerTest(unittest.TestCase):
    def setUp(self):
        self.assertNotEqual(os.geteuid(), 0, "these tests must not run as root")
        self._tmp = tempfile.TemporaryDirectory(prefix="pginst-")
        self.fx = Fixture(self._tmp.name)

    def expected_doc_sha(self, obj):
        return sha256_bytes((json.dumps(obj, indent=2, ensure_ascii=False) + "\n").encode())

    def tearDown(self):
        for root, dirs, files in os.walk(self._tmp.name):
            for name in files + dirs:
                try:
                    os.chmod(os.path.join(root, name), 0o700)
                except OSError:
                    pass
        self._tmp.cleanup()

    def install_ok(self):
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 0, f"install failed rc={rc} out={out!r} err={err!r}")
        self.assertIsNotNone(receipt, f"no JSON receipt in {out!r}")
        self.assertEqual(receipt["result"], "INSTALLED")
        return receipt

    def settings_obj(self):
        return json.loads(read_bytes(self.fx.settings).decode())

    # 11.2 install ------------------------------------------------------------
    def test_11_2_absent_settings_created_with_hooks_key_only(self):
        before = snapshot(self.fx.tmp)
        receipt = self.install_ok()
        obj = self.settings_obj()
        self.assertEqual(list(obj.keys()), ["hooks"])
        self.assertEqual(obj["hooks"], {"PermissionRequest": [self.fx.expected_entry()]})
        self.assertEqual(receipt["preimage"]["status"], "ABSENT")
        self.assertEqual(receipt["postimage_sha256"], sha256_file(self.fx.settings))
        self.assertEqual(receipt["postimage_sha256"],
                         self.expected_doc_sha({"hooks": {"PermissionRequest": [self.fx.expected_entry()]}}))
        self.assertEqual(receipt["runtime_path"], self.fx.runtime_path())
        self.assertEqual(receipt["runtime_sha256"], self.fx.source_sha)
        self.assertEqual(receipt["registration_command"], self.fx.expected_command())
        self.assertIn("hooks", receipt)
        self.assertIn("rollback_command", receipt)
        after = snapshot(self.fx.tmp)
        changed = {k for k in set(before) | set(after) if before.get(k) != after.get(k)}
        for path in changed:
            self.assertTrue(path.startswith(("proj/.claude", "home/.claude/hooks/permission-guard",
                                             "evidence")), f"touched outside its arguments: {path}")
        self.assertEqual(after["untouched/file.txt"], before["untouched/file.txt"])

    def test_11_2_runtime_copy_bytes_and_mode(self):
        self.install_ok()
        self.assertEqual(read_bytes(self.fx.runtime_path()), GUARD_V1)
        mode = stat.S_IMODE(os.stat(self.fx.runtime_path()).st_mode)
        self.assertEqual(mode, 0o444)

    def test_11_2_existing_settings_merged_other_keys_preserved_in_order(self):
        self.fx.write_settings(OTHER_SETTINGS)
        preimage_bytes = read_bytes(self.fx.settings)
        receipt = self.install_ok()
        obj = self.settings_obj()
        self.assertEqual(list(obj.keys()), list(OTHER_SETTINGS.keys()))
        for key in OTHER_SETTINGS:
            if key != "hooks":
                self.assertEqual(obj[key], OTHER_SETTINGS[key])
        self.assertEqual(list(obj["hooks"].keys()), ["SessionStart", "PermissionRequest"])
        self.assertEqual(obj["hooks"]["SessionStart"], OTHER_SETTINGS["hooks"]["SessionStart"])
        self.assertEqual(obj["hooks"]["PermissionRequest"], [self.fx.expected_entry()])
        self.assertEqual(receipt["preimage"]["status"], "PRESENT")
        self.assertEqual(receipt["preimage"]["sha256"], sha256_bytes(preimage_bytes))
        self.assertEqual(read_bytes(receipt["preimage"]["backup_path"]), preimage_bytes)
        self.assertTrue(receipt["preimage"]["backup_path"].startswith(self.fx.evidence))

    def test_11_2_identical_registration_is_noop(self):
        self.install_ok()
        before = snapshot(self.fx.tmp)
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 0)
        self.assertEqual(receipt["result"], "IDEMPOTENT")
        after = snapshot(self.fx.tmp)
        self.assertEqual(before["proj/.claude/settings.local.json"], after["proj/.claude/settings.local.json"])
        self.assertEqual({k: v for k, v in before.items() if not k.startswith("evidence")},
                         {k: v for k, v in after.items() if not k.startswith("evidence")})

    def test_11_2_foreign_permission_request_entry_refused(self):
        foreign = {"hooks": {"PermissionRequest": [{"matcher": "Bash", "hooks": [
            {"type": "command", "command": "echo other"}]}]}}
        self.fx.write_settings(foreign)
        before = snapshot(self.fx.tmp)
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 3)
        self.assertEqual(receipt["result"], "REFUSED")
        self.assertEqual(receipt["reason"], "FOREIGN_HOOK")
        after = snapshot(self.fx.tmp)
        self.assertEqual({k: v for k, v in before.items() if not k.startswith("evidence")},
                         {k: v for k, v in after.items() if not k.startswith("evidence")})
        self.assertFalse(os.path.exists(self.fx.runtime_root))

    def test_11_2_malformed_settings_refused(self):
        self.fx.write_settings(raw=b'{"hooks": ')
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 2)
        self.assertEqual(receipt["result"], "REFUSED")
        self.assertEqual(read_bytes(self.fx.settings), b'{"hooks": ')
        self.assertFalse(os.path.exists(self.fx.runtime_root))

    def test_11_2_duplicate_keys_in_settings_refused(self):
        self.fx.write_settings(raw=b'{"a": 1, "a": 2}')
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 2)
        self.assertEqual(read_bytes(self.fx.settings), b'{"a": 1, "a": 2}')

    def test_11_2_settings_symlink_refused(self):
        real = os.path.join(self.fx.tmp, "real.json")
        with open(real, "w") as fh:
            fh.write("{}\n")
        os.symlink(real, self.fx.settings)
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 2)
        self.assertTrue(os.path.islink(self.fx.settings))
        self.assertEqual(read_bytes(real), b"{}\n")
        self.assertFalse(os.path.exists(self.fx.runtime_root))

    def test_11_2_settings_directory_refused(self):
        os.mkdir(self.fx.settings)
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 2)
        self.assertTrue(os.path.isdir(self.fx.settings))

    def test_11_2_runtime_dir_exists_refused(self):
        os.makedirs(os.path.dirname(self.fx.runtime_path()))
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 5)
        self.assertEqual(receipt["result"], "REFUSED")
        self.assertFalse(os.path.exists(self.fx.settings))

    def test_11_2_runtime_root_parent_missing_refused(self):
        self.fx.write_manifest(overrides={"runtime_root": os.path.join(self.fx.tmp, "nowhere", "permission-guard")})
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 2)
        self.assertEqual(receipt["reason"], "RUNTIME_ROOT_PARENT_MISSING")
        self.assertFalse(os.path.exists(os.path.join(self.fx.tmp, "nowhere")))
        self.assertFalse(os.path.exists(self.fx.settings))

    def test_11_2_source_hash_mismatch_refused(self):
        self.fx.write_manifest(overrides={"source_sha256": "0" * 64})
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 2)
        self.assertFalse(os.path.exists(self.fx.settings))
        self.assertFalse(os.path.exists(self.fx.runtime_root))

    def test_11_1_manifest_missing_or_malformed_refused(self):
        rc, receipt, out, err = self.fx.run("install", manifest=os.path.join(self.fx.tmp, "none.json"))
        self.assertEqual(rc, 2)
        bad = os.path.join(self.fx.tmp, "bad.json")
        with open(bad, "w") as fh:
            fh.write("{not json")
        rc, receipt, out, err = self.fx.run("install", manifest=bad)
        self.assertEqual(rc, 2)
        self.fx.write_manifest(remove=("settings_target",))
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 2)
        self.assertFalse(os.path.exists(self.fx.settings))

    def test_11_1_relative_manifest_paths_refused(self):
        self.fx.write_manifest(overrides={"settings_target": "proj/.claude/settings.local.json"})
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 2)

    def test_11_1_evidence_dir_missing_refused(self):
        self.fx.write_manifest(overrides={"evidence_dir": os.path.join(self.fx.tmp, "no-such")})
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 2)
        self.assertFalse(os.path.exists(self.fx.settings))

    def test_11_2_preimage_changed_before_write_refused(self):
        """The target is changed by another writer between the preimage copy and the
        write; the installer must notice (DRIFT_BEFORE_WRITE) and change nothing."""
        self.fx.write_settings(OTHER_SETTINGS)
        barrier = os.path.join(self.fx.evidence, "barrier")
        os.mkdir(barrier)
        proc = subprocess.Popen([sys.executable, "-I", INSTALLER, "install", "--manifest",
                                 self.fx.manifest, "--test-barrier", barrier],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.fx.tmp)
        deadline = time.time() + 20
        while not os.path.exists(os.path.join(barrier, "reached")):
            if time.time() > deadline or proc.poll() is not None:
                proc.kill()
                self.fail("installer never reached the test barrier")
            time.sleep(0.05)
        mutated = dict(OTHER_SETTINGS)
        mutated["permissions"] = {"allow": ["Bash(ls *)", "Bash(cat *)"]}
        self.fx.write_settings(mutated)
        mutated_bytes = read_bytes(self.fx.settings)
        with open(os.path.join(barrier, "release"), "w") as fh:
            fh.write("go\n")
        out, err = proc.communicate(timeout=30)
        receipt = json.loads([ln for ln in out.decode().splitlines() if ln.strip()][-1])
        self.assertEqual(proc.returncode, 6, err.decode())
        self.assertEqual(receipt["reason"], "DRIFT_BEFORE_WRITE")
        self.assertEqual(read_bytes(self.fx.settings), mutated_bytes)
        self.assertEqual(read_bytes(receipt["preimage"]["backup_path"]),
                         (json.dumps(OTHER_SETTINGS, indent=2) + "\n").encode())
        self.assertFalse(os.path.exists(self.fx.runtime_path()))

    def test_11_2_interrupted_before_replace_leaves_original_intact(self):
        self.fx.write_settings(OTHER_SETTINGS)
        original = read_bytes(self.fx.settings)
        rc, receipt, out, err = self.fx.run("install", extra=["--test-crash-before-replace"])
        self.assertEqual(rc, 7)
        self.assertEqual(receipt["result"], "FAILED")
        self.assertEqual(read_bytes(self.fx.settings), original)
        self.assertTrue(receipt.get("temp_path"))
        self.assertTrue(os.path.exists(receipt["temp_path"]))
        self.assertEqual(os.path.dirname(receipt["temp_path"]), self.fx.settings_dir)
        with open(receipt["temp_path"]) as fh:
            pending = json.load(fh)
        self.assertEqual(pending["hooks"]["PermissionRequest"], [self.fx.expected_entry()])

    def test_11_2_test_barrier_outside_evidence_refused(self):
        barrier = os.path.join(self.fx.tmp, "barrier")
        os.mkdir(barrier)
        rc, receipt, out, err = self.fx.run("install", extra=["--test-barrier", barrier])
        self.assertEqual(rc, 2)
        self.assertEqual(receipt["reason"], "TEST_BARRIER_OUTSIDE_EVIDENCE")
        self.assertEqual(os.listdir(barrier), [])
        self.assertFalse(os.path.exists(self.fx.settings))

    def test_11_2_short_write_refused_with_temp_named(self):
        self.fx.write_settings(OTHER_SETTINGS)
        original = read_bytes(self.fx.settings)
        rc, receipt, out, err = self.fx.run("install", extra=["--test-short-write"])
        self.assertEqual(rc, 7)
        self.assertEqual(receipt["result"], "FAILED")
        self.assertEqual(receipt["reason"], "TEMP_HASH_MISMATCH")
        self.assertEqual(read_bytes(self.fx.settings), original)
        self.assertTrue(os.path.exists(receipt["temp_path"]))
        self.assertNotIn("settings_written", receipt)

    def test_11_2_failure_after_write_is_reported_and_state_saved(self):
        self.fx.write_settings(OTHER_SETTINGS)
        rc, receipt, out, err = self.fx.run("install", extra=["--test-fail-after-write"])
        self.assertEqual(rc, 7)
        self.assertEqual(receipt["result"], "FAILED")
        self.assertTrue(receipt["settings_written"])
        self.assertEqual(receipt["postimage_sha256"], sha256_file(self.fx.settings))
        self.assertEqual(self.settings_obj()["hooks"]["PermissionRequest"], [self.fx.expected_entry()])
        self.assertTrue(self.fx.state()["settings_written"])
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual(rc, 0, err)
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual(rc, 0, err)
        self.assertEqual(read_bytes(self.fx.settings), (json.dumps(OTHER_SETTINGS, indent=2) + "\n").encode())

    def test_11_5_promote_failure_after_write_then_rollback(self):
        self.fx.write_settings(OTHER_SETTINGS)
        preimage = read_bytes(self.fx.settings)
        self.install_ok()
        old_hash12 = self.fx.hash12
        self.fx.write_source(GUARD_V2)
        self.fx.write_manifest()
        rc, receipt, out, err = self.fx.run("promote", extra=["--test-fail-after-write"])
        self.assertEqual(rc, 7)
        self.assertTrue(receipt["settings_written"])
        self.assertEqual(self.settings_obj()["hooks"]["PermissionRequest"], [self.fx.expected_entry()])
        self.assertTrue(os.path.isdir(os.path.join(self.fx.runtime_root, old_hash12)))  # not renamed yet
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual(rc, 0, err)
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual(rc, 0, err)
        self.assertEqual(read_bytes(self.fx.settings), preimage)

    def test_11_4_rollback_failure_after_restore_is_reported(self):
        self.fx.write_settings(OTHER_SETTINGS)
        preimage = read_bytes(self.fx.settings)
        self.install_ok()
        rc, receipt, out, err = self.fx.run("rollback", extra=["--test-fail-after-write"])
        self.assertEqual(rc, 7)
        self.assertTrue(receipt["settings_restored"])
        self.assertEqual(read_bytes(self.fx.settings), preimage)
        self.assertTrue(os.path.isdir(os.path.join(self.fx.runtime_root, self.fx.hash12)))
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual(rc, 2)
        self.assertEqual(receipt["reason"], "STATE_NOT_INSTALLED")

    def test_11_4_rollback_refuses_forged_runtime_dir_in_state(self):
        self.install_ok()
        unrelated = os.path.join(self.fx.tmp, "unrelated-dir")
        os.mkdir(unrelated)
        state_path = os.path.join(self.fx.evidence, "permission-guard-state.json")
        with open(state_path) as fh:
            state = json.load(fh)
        state["runtime_dir"] = unrelated
        state["runtime_path"] = os.path.join(unrelated, "permission_guard.py")
        with open(state_path, "w") as fh:
            json.dump(state, fh)
        before = snapshot(self.fx.tmp)
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual(rc, 2)
        self.assertEqual(receipt["reason"], "STATE_RUNTIME_OUTSIDE_ROOT")
        self.assertTrue(os.path.isdir(unrelated))
        after = snapshot(self.fx.tmp)
        self.assertEqual({k: v for k, v in before.items() if not k.startswith("evidence")},
                         {k: v for k, v in after.items() if not k.startswith("evidence")})

    def test_11_4_rollback_refuses_forged_backup_path_in_state(self):
        self.fx.write_settings(OTHER_SETTINGS)
        self.install_ok()
        installed = read_bytes(self.fx.settings)
        forged = os.path.join(self.fx.tmp, "forged.json")
        with open(forged, "w") as fh:
            fh.write('{"permissions": {"allow": ["Bash(*)"]}}\n')
        state_path = os.path.join(self.fx.evidence, "permission-guard-state.json")
        with open(state_path) as fh:
            state = json.load(fh)
        state["preimage"] = {"status": "PRESENT", "sha256": sha256_file(forged), "backup_path": forged, "mode": "0o644"}
        with open(state_path, "w") as fh:
            json.dump(state, fh)
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual(rc, 2)
        self.assertEqual(receipt["reason"], "STATE_PREIMAGE_OUTSIDE_EVIDENCE")
        self.assertEqual(read_bytes(self.fx.settings), installed)

    def test_11_4_rollback_refuses_state_with_other_settings_target(self):
        self.install_ok()
        state_path = os.path.join(self.fx.evidence, "permission-guard-state.json")
        with open(state_path) as fh:
            state = json.load(fh)
        state["settings_target"] = os.path.join(self.fx.tmp, "other.json")
        with open(state_path, "w") as fh:
            json.dump(state, fh)
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual(rc, 2)
        self.assertEqual(receipt["reason"], "STATE_SETTINGS_TARGET_MISMATCH")

    def test_11_3_verify_refuses_state_missing_hashes(self):
        self.install_ok()
        state_path = os.path.join(self.fx.evidence, "permission-guard-state.json")
        with open(state_path) as fh:
            state = json.load(fh)
        del state["postimage_sha256"]
        with open(state_path, "w") as fh:
            json.dump(state, fh)
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual(rc, 2)
        self.assertEqual(receipt["reason"], "STATE_HASH_MALFORMED")

    def test_11_1_registration_template_with_extra_key_refused(self):
        reg = json.loads(json.dumps(REGISTRATION))
        reg["hooks"][0]["onFailure"] = "block"
        self.fx.write_manifest(overrides={"registration": reg})
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 2)
        self.assertEqual(receipt["reason"], "MANIFEST_REGISTRATION_MALFORMED")
        self.assertFalse(os.path.exists(self.fx.settings))

    def test_11_1_registration_template_with_other_command_refused(self):
        reg = json.loads(json.dumps(REGISTRATION))
        reg["hooks"][0]["command"] = "python3 RUNTIME --expect-sha256 SHA"
        self.fx.write_manifest(overrides={"registration": reg})
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 2)

    def test_11_2_settings_directory_symlink_refused(self):
        elsewhere = os.path.join(self.fx.tmp, "elsewhere")
        os.mkdir(elsewhere)
        os.rmdir(self.fx.settings_dir)
        os.symlink(elsewhere, self.fx.settings_dir)
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 2)
        self.assertEqual(receipt["reason"], "SETTINGS_PATH_SYMLINK_COMPONENT")
        self.assertEqual(os.listdir(elsewhere), [])
        self.assertFalse(os.path.exists(self.fx.runtime_root))

    def test_11_2_runtime_root_containing_sha_substring(self):
        root = os.path.join(self.fx.tmp, "home", ".claude", "hooks", "SHA-guard")
        self.fx.write_manifest(overrides={"runtime_root": root})
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 0, err)
        expected_path = os.path.join(root, self.fx.hash12, "permission_guard.py")
        self.assertEqual(receipt["registration_command"], f"python3 -I {expected_path} --expect-sha256 {self.fx.source_sha}")
        self.assertTrue(os.path.isfile(expected_path))
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual(rc, 0, err)

    def test_11_2_hooks_null_refused_before_runtime_creation(self):
        self.fx.write_settings({"hooks": None})
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 2)
        self.assertEqual(receipt["reason"], "SETTINGS_HOOKS_NOT_OBJECT")
        self.assertFalse(os.path.exists(self.fx.runtime_root))
        rc, receipt, out, err = self.fx.run("install")
        self.assertEqual(rc, 2)  # a retry meets the same refusal, not RUNTIME_DIR_EXISTS

    def test_11_3_verify_detects_registration_removed(self):
        self.fx.write_settings(OTHER_SETTINGS)
        self.install_ok()
        obj = self.settings_obj()
        obj["hooks"]["PermissionRequest"] = []
        self.fx.write_settings(obj)
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual(rc, 1)
        self.assertEqual(receipt["registration_count"], 0)

    def test_11_2_receipt_names_rollback_command_in_every_case(self):
        for prep in (lambda: None, lambda: self.fx.write_settings(raw=b"{bad")):
            prep()
            rc, receipt, out, err = self.fx.run("install")
            self.assertIn("rollback_command", receipt)
            self.assertIn("rollback", receipt["rollback_command"])

    # 11.3 verify -------------------------------------------------------------
    def test_11_3_verify_match_then_drift(self):
        self.install_ok()
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual(rc, 0)
        self.assertEqual(receipt["result"], "MATCH")
        self.assertEqual(receipt["settings_sha256"], sha256_file(self.fx.settings))
        self.assertEqual(receipt["runtime_sha256"], self.fx.source_sha)
        before = snapshot(self.fx.tmp)
        with open(self.fx.settings, "a") as fh:
            fh.write("\n")
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual(rc, 1)
        self.assertEqual(receipt["result"], "DRIFT")
        self.assertEqual(receipt["settings_sha256"], sha256_file(self.fx.settings))
        self.assertEqual(receipt["expected_settings_sha256"], before["proj/.claude/settings.local.json"])
        self.assertIn("runtime_sha256", receipt)
        self.assertIn("expected_runtime_sha256", receipt)

    def test_11_3_verify_runtime_drift(self):
        self.install_ok()
        os.chmod(self.fx.runtime_path(), 0o644)
        with open(self.fx.runtime_path(), "ab") as fh:
            fh.write(b"# tampered\n")
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual(rc, 1)
        self.assertEqual(receipt["result"], "DRIFT")
        self.assertNotEqual(receipt["runtime_sha256"], receipt["expected_runtime_sha256"])

    def test_11_3_verify_never_writes(self):
        self.install_ok()
        before = snapshot(self.fx.tmp)
        self.fx.run("verify")
        after = snapshot(self.fx.tmp)
        self.assertEqual(before, after)  # the whole tree, evidence directory included

    # 11.4 rollback -----------------------------------------------------------
    def test_11_4_rollback_restores_present_preimage(self):
        self.fx.write_settings(OTHER_SETTINGS)
        os.chmod(self.fx.settings, 0o600)
        preimage = read_bytes(self.fx.settings)
        self.install_ok()
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual(rc, 0, err)
        self.assertEqual(receipt["result"], "ROLLED_BACK")
        self.assertEqual(read_bytes(self.fx.settings), preimage)
        self.assertEqual(stat.S_IMODE(os.stat(self.fx.settings).st_mode), 0o600)
        self.assertFalse(os.path.exists(self.fx.runtime_path()))
        disabled = [d for d in os.listdir(self.fx.runtime_root) if d.startswith(self.fx.hash12 + ".disabled-")]
        self.assertEqual(len(disabled), 1)
        self.assertEqual(read_bytes(os.path.join(self.fx.runtime_root, disabled[0], "permission_guard.py")), GUARD_V1)

    def test_11_4_rollback_absent_preimage_renames_never_deletes(self):
        self.install_ok()
        installed = read_bytes(self.fx.settings)
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual(rc, 0, err)
        self.assertFalse(os.path.exists(self.fx.settings))
        rolled = [f for f in os.listdir(self.fx.settings_dir) if f.startswith("settings.local.json.rolled-back-")]
        self.assertEqual(len(rolled), 1)
        self.assertEqual(read_bytes(os.path.join(self.fx.settings_dir, rolled[0])), installed)

    def test_11_4_rollback_with_drift_refused(self):
        self.fx.write_settings(OTHER_SETTINGS)
        self.install_ok()
        with open(self.fx.settings, "a") as fh:
            fh.write("\n")
        before = snapshot(self.fx.tmp)
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual(rc, 4)
        self.assertEqual(receipt["result"], "REFUSED")
        self.assertIn("settings_sha256", receipt)
        self.assertIn("expected_settings_sha256", receipt)
        after = snapshot(self.fx.tmp)
        self.assertEqual({k: v for k, v in before.items() if not k.startswith("evidence")},
                         {k: v for k, v in after.items() if not k.startswith("evidence")})

    def test_11_4_rollback_without_state_refused(self):
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual(rc, 2)

    # 11.5 promote ------------------------------------------------------------
    def test_11_5_promote_replaces_registration_once_and_chains(self):
        self.fx.write_settings(OTHER_SETTINGS)
        first = self.install_ok()
        old_hash12 = self.fx.hash12
        self.fx.write_source(GUARD_V2)
        self.fx.write_manifest()
        rc, receipt, out, err = self.fx.run("promote")
        self.assertEqual(rc, 0, err)
        self.assertEqual(receipt["result"], "PROMOTED")
        obj = self.settings_obj()
        self.assertEqual(obj["hooks"]["PermissionRequest"], [self.fx.expected_entry()])
        self.assertEqual(obj["hooks"]["SessionStart"], OTHER_SETTINGS["hooks"]["SessionStart"])
        self.assertEqual(read_bytes(self.fx.runtime_path()), GUARD_V2)
        self.assertEqual(stat.S_IMODE(os.stat(self.fx.runtime_path()).st_mode), 0o444)
        superseded = [d for d in os.listdir(self.fx.runtime_root) if d.startswith(old_hash12 + ".superseded-")]
        self.assertEqual(len(superseded), 1)
        self.assertEqual(receipt["previous_receipt"]["postimage_sha256"], first["postimage_sha256"])
        self.assertEqual(receipt["previous_receipt"]["runtime_sha256"], first["runtime_sha256"])
        self.assertEqual(self.fx.state()["postimage_sha256"], sha256_file(self.fx.settings))
        rc, receipt, out, err = self.fx.run("verify")
        self.assertEqual(rc, 0)

    def test_11_5_promote_with_drift_refused(self):
        self.install_ok()
        self.fx.write_source(GUARD_V2)
        self.fx.write_manifest()
        with open(self.fx.settings, "a") as fh:
            fh.write("\n")
        before = snapshot(self.fx.tmp)
        rc, receipt, out, err = self.fx.run("promote")
        self.assertEqual(rc, 4)
        after = snapshot(self.fx.tmp)
        self.assertEqual({k: v for k, v in before.items() if not k.startswith("evidence")},
                         {k: v for k, v in after.items() if not k.startswith("evidence")})

    def test_11_5_promote_same_hash_refused(self):
        self.install_ok()
        rc, receipt, out, err = self.fx.run("promote")
        self.assertEqual(rc, 5)

    def test_11_4_rollback_after_promote_restores_original_preimage(self):
        self.fx.write_settings(OTHER_SETTINGS)
        preimage = read_bytes(self.fx.settings)
        self.install_ok()
        self.fx.write_source(GUARD_V2)
        self.fx.write_manifest()
        rc, receipt, out, err = self.fx.run("promote")
        self.assertEqual(rc, 0)
        rc, receipt, out, err = self.fx.run("rollback")
        self.assertEqual(rc, 0, err)
        self.assertEqual(read_bytes(self.fx.settings), preimage)


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
