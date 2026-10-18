#!/usr/bin/env python3
"""Offline tests for install_prompt_observer.py (guard-g1-contract-rev3, section 3.8: TI-1 to TI-10).

Run as a script, never through ``-m unittest``::

    python3 -I .claude/hooks/test_install_prompt_observer.py -v [--installer PATH]

Every test builds a temporary test root (DIR/project with .claude/settings.local.json and a copy of
the clone's prompt_observer.py as the source, DIR/home/.claude/hooks as the hooks root) and runs the
installer only with ``--test-root DIR``: as a subprocess, or in-process for the fault-injection
tests (TI-5, TI-8 pending states, TI-9), where the installer file's bytes are compiled and executed
into a private module object (no import system, no bytecode file) and its test-only FAULTS table
names the point to interrupt. Zero collected tests or any skipped test fails the run.
Test names carry the requirement they pin (TI-1 ... TI-10).
"""

import argparse
import contextlib
import hashlib
import io
import json
import os
import stat
import subprocess
import sys
import tempfile
import types
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_INSTALLER = os.path.join(HERE, "install_prompt_observer.py")
OBSERVER_SOURCE = os.path.join(HERE, "prompt_observer.py")
INSTALLER = DEFAULT_INSTALLER  # overridden by --installer in main()

GUARD_COMMAND = ("python3 -I /home/claude/.claude/hooks/permission-guard/5b16b120defa/permission_guard.py "
                 "--expect-sha256 5b16b120defa408b80047dd144c0ee1cc1dfbee519c9f51b2b92a6d322ffc0d1")
FOREIGN = "sh /home/claude/.claude/hooks/SENTINEL-FOREIGN-7f3a9c.sh"
FOREIGN_NOTIFY = "sh /home/claude/.claude/hooks/SENTINEL-NOTIFY-1b2c3d.sh"
STATE_KEYS = ("version", "status", "started_utc", "runtime_path", "runtime_sha256", "registration_command",
              "preimage_sha256", "preimage_copy", "preimage_mode", "postimage_sha256")
TIMEOUT = 60


def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def sha256_file(path):
    with open(path, "rb") as fh:
        return sha256_bytes(fh.read())


def load_installer():
    with open(INSTALLER, "rb") as fh:
        data = fh.read()
    module = types.ModuleType("_g1_installer_under_test")
    module.__file__ = INSTALLER
    exec(compile(data, INSTALLER, "exec"), module.__dict__)
    return module


class Crash(BaseException):
    """A simulated interruption: no handler of the installer catches it."""


def base_settings(with_notification=False, with_hooks=True):
    doc = {
        "enabledPlugins": {"agent-skills@synced": True, "pr-review-toolkit@synced": True},
        "permissions": {"allow": ["Bash(git status)"]},
    }
    if with_hooks:
        doc["hooks"] = {
            "PermissionRequest": [{"matcher": "Bash", "hooks": [
                {"type": "command", "command": GUARD_COMMAND, "timeout": 10}]}],
            "SessionStart": [{"hooks": [{"type": "command", "command": FOREIGN}]}],
        }
        if with_notification:
            doc["hooks"]["Notification"] = [{"matcher": "idle_prompt", "hooks": [
                {"type": "command", "command": FOREIGN_NOTIFY}]}]
    return doc


def reg_check(path):
    """The reg_check.py logic of contract 4A.5, on a test settings file."""
    with open(path, encoding="utf-8") as fh:
        d = json.load(fh)
    pr = (d.get("hooks") or {}).get("PermissionRequest") or []
    ok = (len(pr) == 1 and pr[0].get("matcher") == "Bash" and len(pr[0].get("hooks") or []) == 1
          and pr[0]["hooks"][0].get("type") == "command" and pr[0]["hooks"][0].get("command") == GUARD_COMMAND
          and pr[0]["hooks"][0].get("timeout") == 10)
    return len(pr), ok


class Root:
    def __init__(self, tmp, settings=None, raw_settings=None, mode=0o640):
        self.root = os.path.realpath(tmp)
        self.project = os.path.join(self.root, "project")
        os.makedirs(os.path.join(self.project, ".claude", "hooks"))
        self.hooks_root = os.path.join(self.root, "home", ".claude", "hooks")
        os.makedirs(self.hooks_root)
        self.obs_root = os.path.join(self.hooks_root, "prompt-observer")
        self.state = os.path.join(self.obs_root, "state.json")
        self.source = os.path.join(self.project, ".claude", "hooks", "prompt_observer.py")
        with open(OBSERVER_SOURCE, "rb") as fh:
            self.source_bytes = fh.read()
        with open(self.source, "wb") as fh:
            fh.write(self.source_bytes)
        self.source_sha = sha256_bytes(self.source_bytes)
        self.runtime_dir = os.path.join(self.obs_root, self.source_sha[:12])
        self.runtime = os.path.join(self.runtime_dir, "prompt_observer.py")
        self.command = f"python3 -I {self.runtime} --expect-sha256 {self.source_sha}"
        self.settings = os.path.join(self.project, ".claude", "settings.local.json")
        if raw_settings is None:
            # deliberately not the installer's serialisation: an exact restore must keep these bytes
            raw_settings = (json.dumps(settings if settings is not None else base_settings(), indent=4)
                            + "\n\n").encode()
        if raw_settings is not False:
            with open(self.settings, "wb") as fh:
                fh.write(raw_settings)
            os.chmod(self.settings, mode)
        self.installer_sha = sha256_file(INSTALLER)

    def pre_bytes(self):
        with open(self.settings, "rb") as fh:
            return fh.read()

    def argv(self, mode, expect=None, source=None, with_source=None):
        argv = [mode, "--expect-sha256", expect or self.installer_sha]
        if (mode == "install") if with_source is None else with_source:
            argv += ["--source-sha256", source or self.source_sha]
        return argv + ["--test-root", self.root]

    def run(self, mode, **kw):
        proc = subprocess.run([sys.executable, "-I", INSTALLER] + self.argv(mode, **kw), capture_output=True,
                              cwd=self.root, timeout=TIMEOUT, env={"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"})
        lines = proc.stdout.decode().splitlines()
        out = json.loads(lines[0]) if len(lines) == 1 else None
        return proc.returncode, out, proc

    def run_inproc(self, mode, faults=None, **kw):
        module = load_installer()
        module.FAULTS.update(faults or {})
        buffer = io.StringIO()
        with contextlib.redirect_stdout(buffer):
            code = module.main(self.argv(mode, **kw))
        lines = buffer.getvalue().splitlines()
        return code, (json.loads(lines[0]) if len(lines) == 1 else None)

    def snapshot(self):
        out = {}
        for dirpath, dirnames, filenames in os.walk(self.root):
            for name in dirnames + filenames:
                path = os.path.join(dirpath, name)
                info = os.lstat(path)
                digest = None
                if stat.S_ISREG(info.st_mode):
                    digest = sha256_file(path)
                out[os.path.relpath(path, self.root)] = (stat.S_IFMT(info.st_mode), stat.S_IMODE(info.st_mode),
                                                         digest, info.st_mtime_ns)
        return out

    def settings_obj(self):
        with open(self.settings, "rb") as fh:
            return json.loads(fh.read())

    def listing(self):
        return sorted(os.listdir(self.obs_root)) if os.path.isdir(self.obs_root) else []


class RootCase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()

    def tearDown(self):
        self._tmp.cleanup()

    def root(self, **kw):
        sub = tempfile.mkdtemp(dir=self._tmp.name)
        return Root(sub, **kw)

    def install(self, r):
        code, out, proc = r.run("install")
        self.assertEqual(code, 0, proc.stdout + proc.stderr)
        self.assertEqual(out["result"], "INSTALLED")
        return out

    def assert_only_preimage_left(self, r, pre):
        self.assertEqual(r.pre_bytes(), pre)
        self.assertFalse(os.path.lexists(r.state))
        self.assertFalse(os.path.lexists(r.runtime_dir))


# --- TI-1 ------------------------------------------------------------------------------------------

class TestInstall(RootCase):
    def test_TI_1_install_success(self):
        r = self.root()
        pre = r.pre_bytes()
        pre_obj = json.loads(pre)
        code, out, proc = r.run("install")
        self.assertEqual(code, 0)
        self.assertEqual(proc.stderr, b"")
        self.assertEqual(len(proc.stdout.decode().splitlines()), 1)
        self.assertEqual(out["result"], "INSTALLED")
        expected = json.loads(pre)
        expected["hooks"]["Notification"] = [{"matcher": "permission_prompt", "hooks": [
            {"type": "command", "command": r.command, "timeout": 10}]}]
        post_obj = r.settings_obj()
        self.assertEqual(post_obj, expected)
        for key in pre_obj:
            if key != "hooks":
                self.assertEqual(post_obj[key], pre_obj[key])
        self.assertEqual(post_obj["hooks"]["PermissionRequest"], pre_obj["hooks"]["PermissionRequest"])
        self.assertEqual(post_obj["hooks"]["SessionStart"], pre_obj["hooks"]["SessionStart"])
        post_bytes = r.pre_bytes()
        self.assertEqual(post_bytes, (json.dumps(expected, indent=2, ensure_ascii=False) + "\n").encode())
        self.assertEqual(stat.S_IMODE(os.lstat(r.settings).st_mode), 0o640)
        self.assertEqual(stat.S_IMODE(os.lstat(r.runtime).st_mode), 0o444)
        self.assertEqual(sha256_file(r.runtime), r.source_sha)
        self.assertEqual(stat.S_IMODE(os.lstat(r.obs_root).st_mode), 0o700)
        with open(r.state, "rb") as fh:
            state_text = fh.read().decode()
        state = json.loads(state_text)
        for key in STATE_KEYS:
            self.assertIn(key, state)
        self.assertEqual(state["status"], "installed")
        self.assertEqual(state["runtime_path"], r.runtime)
        self.assertEqual(state["registration_command"], r.command)
        self.assertEqual(state["preimage_sha256"], sha256_bytes(pre))
        self.assertEqual(state["postimage_sha256"], sha256_bytes(post_bytes))
        self.assertEqual(state["preimage_mode"], 0o640)
        self.assertEqual(sha256_file(state["preimage_copy"]), sha256_bytes(pre))
        self.assertEqual(stat.S_IMODE(os.lstat(state["preimage_copy"]).st_mode), 0o600)
        for sha in (out["runtime_sha256"], out["preimage_sha256"], out["postimage_sha256"], out["installer_sha256"]):
            self.assertEqual(len(sha), 64)
        self.assertEqual(out["installer_sha256"], r.installer_sha)
        self.assertEqual(out["registration_command"], r.command)
        self.assertEqual(out["hook_entry_counts"], {"PermissionRequest": 1, "SessionStart": 1, "Notification": 1})
        for text in (proc.stdout.decode(), state_text):
            self.assertNotIn("SENTINEL-FOREIGN", text)
            self.assertNotIn("Bash(git status)", text)
            self.assertNotIn("agent-skills", text)

    def test_TI_1_install_creates_hooks_and_appends_after_foreign_notification(self):
        r = self.root(settings=base_settings(with_hooks=False))
        self.install(r)
        self.assertEqual(r.settings_obj()["hooks"], {"Notification": [
            {"matcher": "permission_prompt", "hooks": [{"type": "command", "command": r.command, "timeout": 10}]}]})
        r2 = self.root(settings=base_settings(with_notification=True))
        out = self.install(r2)
        entries = r2.settings_obj()["hooks"]["Notification"]
        self.assertEqual(entries[0]["hooks"][0]["command"], FOREIGN_NOTIFY)
        self.assertEqual(entries[1]["hooks"][0]["command"], r2.command)
        self.assertNotIn("SENTINEL-NOTIFY", json.dumps(out))


# --- TI-2 to TI-4 --------------------------------------------------------------------------------------

class TestRefusals(RootCase):
    def test_TI_2_already_installed(self):
        r = self.root()
        self.install(r)
        before = r.snapshot()
        code, out, _ = r.run("install")
        self.assertEqual(code, 5)
        self.assertEqual(out["reason"], "ALREADY_INSTALLED")
        self.assertEqual(r.snapshot(), before)

    def test_TI_2_observer_entry_exists(self):
        doc = base_settings()
        doc["hooks"]["Notification"] = [{"hooks": [{"type": "command",
                                                     "command": "python3 -I /x/prompt_observer.py --expect-sha256 x"}]}]
        r = self.root(settings=doc)
        before = r.snapshot()
        code, out, _ = r.run("install")
        self.assertEqual(code, 4)
        self.assertEqual(out["reason"], "OBSERVER_ENTRY_EXISTS")
        self.assertEqual(r.snapshot(), before)

    def test_TI_2_runtime_exists(self):
        r = self.root()
        os.makedirs(r.runtime_dir)
        before = r.snapshot()
        code, out, _ = r.run("install")
        self.assertEqual(code, 4)
        self.assertEqual(out["reason"], "RUNTIME_EXISTS")
        self.assertEqual(r.snapshot(), before)

    def test_TI_3_preconditions(self):
        cases = {
            "SETTINGS_ABSENT": dict(raw_settings=False),
            "SETTINGS_INVALID_JSON": dict(raw_settings=b"{not json"),
            "SETTINGS_DUPLICATE_KEYS": dict(raw_settings=b'{"hooks": {}, "hooks": {}}'),
            "HOOKS_NOT_OBJECT": dict(raw_settings=b'{"hooks": []}'),
            "NOTIFICATION_NOT_LIST": dict(raw_settings=b'{"hooks": {"Notification": {}}}'),
            "SETTINGS_NOT_OBJECT": dict(raw_settings=b"[1]"),
        }
        for reason, kw in cases.items():
            with self.subTest(reason=reason):
                r = self.root(**kw)
                before = r.snapshot()
                code, out, _ = r.run("install")
                self.assertEqual(code, 3)
                self.assertEqual(out["reason"], reason)
                self.assertEqual(r.snapshot(), before)
                self.assertFalse(os.path.lexists(r.obs_root))
        with self.subTest(reason="SETTINGS_SYMLINK"):
            r = self.root()
            target = os.path.join(r.root, "target.json")
            os.rename(r.settings, target)
            os.symlink(target, r.settings)
            before = r.snapshot()
            code, out, _ = r.run("install")
            self.assertEqual(code, 3)
            self.assertEqual(out["reason"], "SETTINGS_SYMLINK")
            self.assertEqual(r.snapshot(), before)

    def test_TI_4_self_and_source_hash_mismatch(self):
        r = self.root()
        before = r.snapshot()
        cases = [("install", dict(expect="0" * 64)), ("install", dict(source="1" * 64)),
                 ("install", dict(with_source=False))]
        for mode in ("uninstall", "remove-entry", "status"):
            cases += [(mode, dict(expect="0" * 64)), (mode, dict(with_source=True))]
        for mode, kw in cases:
            with self.subTest(mode=mode, kw=kw):
                code, out, _ = r.run(mode, **kw)
                self.assertEqual(code, 2)
                self.assertEqual(out["result"], "REFUSED")
                self.assertEqual(r.snapshot(), before)
        code, out, _ = r.run("install", expect="0" * 64)
        self.assertEqual(out["reason"], "SELF_HASH_MISMATCH")
        code, out, _ = r.run("install", source="1" * 64)
        self.assertEqual(out["reason"], "SOURCE_HASH_MISMATCH")


# --- TI-5 to TI-7 --------------------------------------------------------------------------------------

class TestTransitions(RootCase):
    def test_TI_5_drift_before_write(self):
        r = self.root()
        changed = b'{"hooks": {}, "changed": true}\n'

        def drift():
            with open(r.settings, "wb") as fh:
                fh.write(changed)

        code, out = r.run_inproc("install", faults={"install:before-settings-reread": drift})
        self.assertEqual(code, 4)
        self.assertEqual(out["reason"], "DRIFT_BEFORE_WRITE")
        self.assertEqual(r.pre_bytes(), changed)
        self.assertFalse(os.path.lexists(r.state))
        self.assertFalse(os.path.lexists(r.runtime_dir))
        names = r.listing()
        self.assertEqual(len([n for n in names if n.startswith(r.source_sha[:12] + ".aborted-")]), 1)
        self.assertEqual(len([n for n in names if n.startswith("state.json.aborted-")]), 1)

    def test_TI_6_uninstall_restores_exact_preimage(self):
        r = self.root()
        pre = r.pre_bytes()
        self.install(r)
        self.assertNotEqual(r.pre_bytes(), pre)
        code, out, _ = r.run("uninstall")
        self.assertEqual(code, 0)
        self.assertEqual(out["result"], "UNINSTALLED")
        self.assertTrue(out["settings_written"])
        self.assertEqual(r.pre_bytes(), pre)
        self.assertEqual(stat.S_IMODE(os.lstat(r.settings).st_mode), 0o640)
        names = r.listing()
        self.assertEqual(len([n for n in names if n.startswith(r.source_sha[:12] + ".disabled-")]), 1)
        self.assertEqual(len([n for n in names if n.startswith("state.json.uninstalled-")]), 1)
        self.assertFalse(os.path.lexists(r.state))
        self.assertFalse(os.path.lexists(r.runtime_dir))
        self.assertEqual(r.run("status")[0], 3)
        self.assertEqual(r.run("uninstall")[0], 3)

    def test_TI_7_foreign_drift_then_remove_entry(self):
        r = self.root(settings=base_settings(with_notification=True))
        self.install(r)
        drifted_obj = r.settings_obj()
        drifted_obj["enabledPlugins"]["pr-review-toolkit@synced"] = False
        drifted = (json.dumps(drifted_obj, indent=2) + "\n").encode()
        with open(r.settings, "wb") as fh:
            fh.write(drifted)
        before = r.snapshot()
        code, out, _ = r.run("uninstall")
        self.assertEqual(code, 4)
        self.assertEqual(out["reason"], "SETTINGS_DRIFT")
        self.assertEqual(out["settings_sha256"], sha256_bytes(drifted))
        self.assertEqual(r.snapshot(), before)
        code, out, _ = r.run("remove-entry")
        self.assertEqual(code, 0)
        self.assertEqual(out["result"], "ENTRY_REMOVED")
        self.assertTrue(out["reserialized"])
        expected = json.loads(drifted)
        expected["hooks"]["Notification"] = [e for e in expected["hooks"]["Notification"]
                                             if e["hooks"][0]["command"] != r.command]
        self.assertEqual(r.settings_obj(), expected)
        self.assertEqual(stat.S_IMODE(os.lstat(r.settings).st_mode), 0o640)
        self.assertEqual(sha256_file(out["backup_copy"]), sha256_bytes(drifted))
        self.assertNotIn("SENTINEL", json.dumps(out))
        names = r.listing()
        self.assertEqual(len([n for n in names if n.startswith(r.source_sha[:12] + ".disabled-")]), 1)
        self.assertEqual(len([n for n in names if n.startswith("state.json.uninstalled-")]), 1)
        self.assertEqual(r.run("status")[0], 3)

    def test_TI_7_remove_entry_drops_emptied_list(self):
        r = self.root()
        self.install(r)
        obj = r.settings_obj()
        obj["permissions"]["allow"].append("Bash(ls)")
        with open(r.settings, "w") as fh:
            json.dump(obj, fh)
        code, out, _ = r.run("remove-entry")
        self.assertEqual(code, 0)
        expected = dict(obj)
        expected["hooks"] = {k: v for k, v in obj["hooks"].items() if k != "Notification"}
        self.assertEqual(r.settings_obj(), expected)

    def test_TI_7_remove_entry_refusals(self):
        r = self.root()
        self.assertEqual(r.run("remove-entry")[0], 3)
        self.install(r)
        obj = r.settings_obj()
        obj["hooks"]["Notification"].append(json.loads(json.dumps(obj["hooks"]["Notification"][0])))
        with open(r.settings, "w") as fh:
            json.dump(obj, fh)
        before = r.snapshot()
        code, out, _ = r.run("remove-entry")
        self.assertEqual(code, 4)
        self.assertEqual(out["observer_entries"], 2)
        self.assertEqual(r.snapshot(), before)


# --- TI-8 ----------------------------------------------------------------------------------------

class TestStatus(RootCase):
    def status_unchanged(self, r):
        before = r.snapshot()
        code, out, proc = r.run("status")
        self.assertEqual(r.snapshot(), before)
        self.assertEqual(len(proc.stdout.decode().splitlines()), 1)
        return code, out

    def test_TI_8_status_results(self):
        r = self.root()
        code, out = self.status_unchanged(r)
        self.assertEqual((code, out["result"]), (3, "NOT_INSTALLED"))
        self.assertEqual(out["permission_request_entries"], 1)
        self.assertEqual(out["observer_entries"], 0)
        self.assertFalse(os.path.lexists(r.obs_root))
        self.install(r)
        code, out = self.status_unchanged(r)
        self.assertEqual((code, out["result"]), (0, "MATCH"))
        self.assertTrue(out["runtime_sha256_ok"])
        self.assertEqual(out["observer_entries"], 1)
        self.assertEqual(out["settings_sha256"], out["postimage_sha256"])
        obj = r.settings_obj()
        obj["enabledPlugins"]["agent-skills@synced"] = False
        with open(r.settings, "w") as fh:
            json.dump(obj, fh, indent=2)
        code, out = self.status_unchanged(r)
        self.assertEqual((code, out["result"]), (1, "DRIFT"))

    def test_TI_8_status_runtime_drift(self):
        r = self.root()
        self.install(r)
        os.chmod(r.runtime, 0o644)
        with open(r.runtime, "ab") as fh:
            fh.write(b"# changed\n")
        code, out = self.status_unchanged(r)
        self.assertEqual((code, out["result"]), (1, "DRIFT"))
        self.assertFalse(out["runtime_sha256_ok"])

    def test_TI_8_status_pending(self):
        r = self.root()
        with self.assertRaises(Crash):
            r.run_inproc("install", faults={"install:after-state-pending": Crash()})
        code, out = self.status_unchanged(r)
        self.assertEqual((code, out["result"]), (1, "PENDING_INSTALL"))
        self.assertEqual(r.run("uninstall")[0], 0)
        self.install(r)
        with self.assertRaises(Crash):
            r.run_inproc("uninstall", faults={"uninstall:after-state-pending": Crash()})
        code, out = self.status_unchanged(r)
        self.assertEqual((code, out["result"]), (1, "PENDING_UNINSTALL"))


# --- TI-9 ----------------------------------------------------------------------------------------

class TestRecovery(RootCase):
    def finish(self, r, pre):
        code, out, proc = r.run("uninstall")
        self.assertEqual(code, 0, proc.stdout)
        self.assertEqual(r.pre_bytes(), pre)
        self.assertEqual(r.run("uninstall")[0], 3)
        return out

    def test_TI_9_a_after_pending_before_replacement(self):
        r = self.root()
        pre = r.pre_bytes()
        with self.assertRaises(Crash):
            r.run_inproc("install", faults={"install:after-state-pending": Crash()})
        self.assertEqual(r.pre_bytes(), pre)
        self.assertEqual(r.run("status")[1]["result"], "PENDING_INSTALL")
        out = self.finish(r, pre)
        self.assertFalse(out["settings_written"])

    def test_TI_9_b_after_replacement_before_installed(self):
        r = self.root()
        pre = r.pre_bytes()
        with self.assertRaises(Crash):
            r.run_inproc("install", faults={"install:after-settings-replace": Crash()})
        self.assertNotEqual(r.pre_bytes(), pre)
        self.assertEqual(r.run("status")[1]["result"], "PENDING_INSTALL")
        out = self.finish(r, pre)
        self.assertTrue(out["settings_written"])

    def test_TI_9_c_after_pending_uninstall_before_restore(self):
        r = self.root()
        pre = r.pre_bytes()
        self.install(r)
        with self.assertRaises(Crash):
            r.run_inproc("uninstall", faults={"uninstall:after-state-pending": Crash()})
        self.assertNotEqual(r.pre_bytes(), pre)
        self.assertEqual(r.run("status")[1]["result"], "PENDING_UNINSTALL")
        out = self.finish(r, pre)
        self.assertTrue(out["settings_written"])

    def test_TI_9_d_after_restore_before_renames(self):
        r = self.root()
        pre = r.pre_bytes()
        self.install(r)
        with self.assertRaises(Crash):
            r.run_inproc("uninstall", faults={"uninstall:before-renames": Crash()})
        self.assertEqual(r.pre_bytes(), pre)
        self.assertEqual(r.run("status")[1]["result"], "PENDING_UNINSTALL")
        out = self.finish(r, pre)
        self.assertFalse(out["settings_written"])

    def test_TI_9_d_between_renames(self):
        r = self.root()
        pre = r.pre_bytes()
        self.install(r)
        with self.assertRaises(Crash):
            r.run_inproc("uninstall", faults={"finish:between-renames": Crash()})
        self.assertFalse(os.path.lexists(r.runtime_dir))
        self.assertTrue(os.path.lexists(r.state))
        out = self.finish(r, pre)
        self.assertIsNone(out["runtime_renamed_to"])

    def test_TI_9_e_replace_failure(self):
        r = self.root()
        pre = r.pre_bytes()
        code, out = r.run_inproc("install", faults={"install:replace-settings": OSError("injected")})
        self.assertEqual(code, 6)
        self.assertEqual(out["reason"], "WRITE_FAILED")
        self.assertEqual(r.pre_bytes(), pre)
        names = r.listing()
        self.assertEqual(len([n for n in names if n.startswith(r.source_sha[:12] + ".aborted-")]), 1)
        self.assertEqual(len([n for n in names if n.startswith("state.json.aborted-")]), 1)
        self.assertEqual([n for n in os.listdir(os.path.dirname(r.settings)) if ".tmp-" in n], [])
        self.assert_only_preimage_left(r, pre)
        self.assertEqual(r.run("uninstall")[0], 3)

    def test_TI_9_remove_entry_interrupted_after_replacement(self):
        r = self.root()
        self.install(r)
        obj = r.settings_obj()
        obj["enabledPlugins"]["agent-skills@synced"] = False
        with open(r.settings, "w") as fh:
            json.dump(obj, fh)
        with self.assertRaises(Crash):
            r.run_inproc("remove-entry", faults={"remove-entry:before-renames": Crash()})
        self.assertEqual(r.run("status")[1]["result"], "PENDING_UNINSTALL")
        removed = r.pre_bytes()
        code, out, _ = r.run("uninstall")
        self.assertEqual(code, 0)
        self.assertFalse(out["settings_written"])
        self.assertEqual(r.pre_bytes(), removed)
        self.assertEqual(r.run("uninstall")[0], 3)


# --- TI-10 ------------------------------------------------------------------------------------------

class TestGuardCompatibility(RootCase):
    def test_TI_10_guard_registration_untouched(self):
        r = self.root()
        original = sha256_file(r.settings)
        self.assertEqual(reg_check(r.settings), (1, True))
        self.install(r)
        self.assertEqual(reg_check(r.settings), (1, True))
        self.assertEqual(r.run("uninstall")[0], 0)
        self.assertEqual(sha256_file(r.settings), original)
        self.assertEqual(reg_check(r.settings), (1, True))


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
