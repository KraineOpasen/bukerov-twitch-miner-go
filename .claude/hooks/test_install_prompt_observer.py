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
import fcntl
import hashlib
import io
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import time
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
LOCK_NAME = ".prompt-observer-installer.lock"
README = os.path.join(HERE, "README-prompt-observer.md")
SUBPROCESS_ENV = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"}

# The protocol texts of guard-g1-repair-rev6 3.2, which the README states verbatim (TR-B3d).
REMOVAL_PROTOCOL = (
    "To remove the observer, run status and act on what it prints: MATCH, run uninstall; next_step uninstall, "
    "run uninstall; next_step remove-entry, run remove-entry; NOT_INSTALLED, removal is complete; next_step "
    "owner-review, stop and run no installer mode until the printed reason has been reviewed. Run status again "
    "after every uninstall or remove-entry. Run at most three uninstall or remove-entry commands in total; if "
    "status still does not print NOT_INSTALLED after the third, stop as for owner-review.")
INSTALL_PROTOCOL = (
    "To install, run install and then status. MATCH means the observer is installed, even if install printed "
    "nothing. NOT_INSTALLED means install left no registration and no journal: run install once more. "
    "PENDING_INSTALL, PENDING_UNINSTALL or DRIFT: complete the removal protocol, then run install once more. "
    "next_step owner-review: stop. Run install at most twice.")

# Subprocess driver (3.4): loads the installer bytes into a private module exactly as load_installer()
# does, applies the seams named in argv[1] (JSON) and calls main(argv[3:]). Markers are files the
# driver creates when a seam is reached; releases are files the test creates.
DRIVER = r'''
import json, os, sys, time, types
spec = json.loads(sys.argv[1])
installer = sys.argv[2]
with open(installer, "rb") as fh:
    data = fh.read()
module = types.ModuleType("_g1_installer_under_test")
module.__file__ = installer
exec(compile(data, installer, "exec"), module.__dict__)

def touch(path):
    open(path, "w").close()

def wait(path):
    deadline = time.time() + 30
    while not os.path.exists(path) and time.time() < deadline:
        time.sleep(0.05)

hold = spec.get("read_state_hold")
if hold:
    original_read_state = module.read_state
    def read_state(layout):
        result = original_read_state(layout)
        touch(hold["marker"])
        wait(hold["release"])
        return result
    module.read_state = read_state
barrier = spec.get("barrier")
if barrier:
    def at_barrier():
        touch(barrier["marker"])
        wait(barrier["release"])
    module.FAULTS[barrier["point"]] = at_barrier
exit_at = spec.get("exit_at")
if exit_at:
    def die():
        touch(exit_at["marker"])
        os._exit(exit_at["code"])
    module.FAULTS[exit_at["point"]] = die
emit_fail = spec.get("emit_fail")
if emit_fail:
    original_emit = module.emit
    def emit(obj):
        if obj.get("result") in emit_fail["results"]:
            touch(emit_fail["marker"])
            raise OSError("injected receipt failure")
        original_emit(obj)
    module.emit = emit
sys.exit(module.main(sys.argv[3:]))
'''


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
    def __init__(self, tmp, settings=None, raw_settings=None, mode=0o640, precreate_lock=True, driver=None):
        self.root = os.path.realpath(tmp)
        self.driver = driver
        self.project = os.path.join(self.root, "project")
        os.makedirs(os.path.join(self.project, ".claude", "hooks"))
        self.hooks_root = os.path.join(self.root, "home", ".claude", "hooks")
        os.makedirs(self.hooks_root)
        # every directory the harness creates gets an explicit mode, so the installer's directory
        # chain check (3.2) never depends on the umask
        for directory in (self.project, os.path.join(self.project, ".claude"),
                          os.path.join(self.project, ".claude", "hooks"), os.path.join(self.root, "home"),
                          os.path.join(self.root, "home", ".claude"), self.hooks_root):
            os.chmod(directory, 0o755)
        self.lock = os.path.join(self.hooks_root, LOCK_NAME)
        if precreate_lock:
            # the lock file a previous writing run leaves behind (3.2 Lock); present from the start so
            # that a writing run that only takes the lock leaves every snapshot unchanged
            os.close(os.open(self.lock, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600))
            os.chmod(self.lock, 0o600)
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
                              cwd=self.root, timeout=TIMEOUT, env=SUBPROCESS_ENV)
        lines = proc.stdout.decode().splitlines()
        out = json.loads(lines[0]) if len(lines) == 1 else None
        return proc.returncode, out, proc

    def status(self):
        code, out, _ = self.run("status")
        return code, out

    def run_driver(self, mode, spec, wait=True, **kw):
        args = [sys.executable, "-I", self.driver, json.dumps(spec), INSTALLER] + self.argv(mode, **kw)
        if not wait:
            return subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=self.root,
                                    env=SUBPROCESS_ENV)
        proc = subprocess.run(args, capture_output=True, cwd=self.root, timeout=TIMEOUT, env=SUBPROCESS_ENV)
        lines = proc.stdout.decode().splitlines()
        out = json.loads(lines[0]) if len(lines) == 1 else None
        return proc.returncode, out, proc

    def run_inproc(self, mode, faults=None, patch=None, **kw):
        module = load_installer()
        if patch is not None:
            patch(module)
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
        driver = os.path.join(self._tmp.name, "driver.py")
        if not os.path.exists(driver):
            with open(driver, "w") as fh:
                fh.write(DRIVER)
        return Root(sub, driver=driver, **kw)

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
        # 3.2: an orphaned runtime directory is renamed (TR-B3j) unless it fails the directory check
        r = self.root()
        os.mkdir(r.obs_root, 0o700)
        os.chmod(r.obs_root, 0o700)
        os.mkdir(r.runtime_dir)
        os.chmod(r.runtime_dir, 0o777)
        before = r.snapshot()
        code, out, _ = r.run("install")
        self.assertEqual(code, 3)
        self.assertEqual(out["reason"], "RUNTIME_DIR_UNSAFE")
        self.assertEqual(out["next_step"], "owner-review")
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
        self.assertEqual(out["result"], "WRITE_FAILED")
        self.assertEqual(out["next_step"], "status")
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


# --- G1 repair (guard-g1-repair-rev6 3.4): TR-B1 to TR-B4, TR-P1, TR-S -------------------------------
#
# Every injected callable records that it ran, and each test asserts that record FIRST (the stimulus
# was reached) before any behavioural assertion. Classes (RED, CONTROL, SCHEMA, NEW-SEAM) are in the
# test names' docstrings.

def read_bytes(path):
    with open(path, "rb") as fh:
        return fh.read()


def write_json(path, obj):
    with open(path, "w") as fh:
        json.dump(obj, fh, indent=2)
        fh.write("\n")


def replace_private_file(path, data):
    """A new regular file, mode 0600, link count 1 (as the installer writes its journal)."""
    if os.path.lexists(path):
        os.unlink(path)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        os.write(fd, data)
    finally:
        os.close(fd)
    os.chmod(path, 0o600)


def dumps_record(obj):
    return (json.dumps(obj, indent=2, ensure_ascii=True, allow_nan=False, sort_keys=True) + "\n").encode("ascii")


def crash_at(point, fired):
    def action():
        fired.append(point)
        raise Crash()
    return action


def raise_at(point, fired, exc):
    def action():
        fired.append(point)
        raise exc
    return action


def temp_files(r):
    return [n for n in os.listdir(os.path.dirname(r.settings)) if ".tmp-" in n]


def observer_commands(obj):
    out = []
    for entry in (obj.get("hooks") or {}).get("Notification") or []:
        for hook in entry.get("hooks") or []:
            if "prompt_observer.py" in hook.get("command", ""):
                out.append(hook["command"])
    return out


@contextlib.contextmanager
def held_lock(path):
    fd = os.open(path, os.O_RDWR | os.O_CREAT, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)
        yield
    finally:
        os.close(fd)


def wait_for(predicate, limit=30):
    deadline = time.time() + limit
    while not predicate():
        if time.time() > deadline:
            raise AssertionError("wait_for timed out")
        time.sleep(0.05)


class RepairCase(RootCase):
    def interrupt(self, r, mode, point):
        fired = []
        try:
            r.run_inproc(mode, faults={point: crash_at(point, fired)})
        except Crash:
            pass
        self.assertEqual(fired, [point], "stimulus not reached")

    def removal_protocol(self, r, inproc=False):
        """The REMOVAL PROTOCOL of 3.2 applied mechanically; returns the number of writing commands."""
        def run(mode):
            if inproc:
                return r.run_inproc(mode)
            code, out, _ = r.run(mode)
            return code, out

        writes = 0
        while True:
            code, out = run("status")
            self.assertIsNotNone(out)
            self.assertIn("next_step", out)
            if out["result"] == "NOT_INSTALLED":
                return writes
            if out["result"] == "MATCH":
                command = "uninstall"
            else:
                self.assertIn(out["next_step"], ("uninstall", "remove-entry"), out)
                command = out["next_step"]
            self.assertLess(writes, 3, "removal protocol bound exceeded")
            code, out = run(command)
            self.assertNotEqual((out or {}).get("reason"), "LOCKED", out)
            writes += 1

    def install_protocol(self, r, installs=0):
        """The INSTALL PROTOCOL of 3.2; `installs` counts install runs already made."""
        while True:
            code, out = r.status()
            self.assertIsNotNone(out)
            self.assertIn("next_step", out)
            if out["result"] == "MATCH":
                return installs
            self.assertNotEqual(out["next_step"], "owner-review", out)
            if out["result"] != "NOT_INSTALLED":
                self.removal_protocol(r)
            self.assertLess(installs, 2, "install protocol bound exceeded")
            r.run("install")
            installs += 1

    def rewrite_state(self, r, **changes):
        state = json.loads(read_bytes(r.state))
        state.update(changes)
        replace_private_file(r.state, dumps_record(state))
        return state

    def drifted(self, r, key="foreignKey"):
        obj = r.settings_obj()
        obj[key] = {"kept": [1, 2, "three"]}
        data = (json.dumps(obj, indent=2) + "\n").encode()
        with open(r.settings, "wb") as fh:
            fh.write(data)
        return obj, data


class TestRepairB1(RepairCase):
    def test_TR_B1a_uninstall_rechecks_before_replace(self):
        """RED: a foreign change at uninstall:replace-settings is never overwritten."""
        r = self.root(settings=base_settings(with_notification=True))
        self.install(r)
        foreign_obj = r.settings_obj()
        foreign_obj["enabledPlugins"]["pr-review-toolkit@synced"] = False
        foreign = (json.dumps(foreign_obj, indent=2) + "\n").encode()
        state_before = read_bytes(r.state)
        fired = []

        def change():
            fired.append(1)
            with open(r.settings, "wb") as fh:
                fh.write(foreign)

        code, out = r.run_inproc("uninstall", faults={"uninstall:replace-settings": change})
        self.assertEqual(fired, [1], "stimulus not reached")
        self.assertEqual(code, 4, out)
        self.assertEqual(out["reason"], "DRIFT_BEFORE_WRITE")
        self.assertEqual(out["next_step"], "status")
        self.assertEqual(r.pre_bytes(), foreign)
        self.assertEqual(read_bytes(r.state), state_before)
        self.assertEqual(sha256_file(r.runtime), r.source_sha)
        self.assertEqual(temp_files(r), [])
        code, out = r.status()
        self.assertEqual((code, out["result"], out["next_step"]), (1, "DRIFT", "remove-entry"))
        code, out, _ = r.run("remove-entry")
        self.assertEqual((code, out["result"]), (0, "ENTRY_REMOVED"))
        expected = json.loads(foreign)
        expected["hooks"]["Notification"] = [e for e in expected["hooks"]["Notification"]
                                             if e["hooks"][0]["command"] != r.command]
        self.assertEqual(r.settings_obj(), expected)

    def test_TR_B1b_remove_entry_rechecks_before_replace(self):
        """RED: a foreign change at remove-entry:replace-settings is never overwritten."""
        r = self.root()
        self.install(r)
        state_before = read_bytes(r.state)
        foreign = b'{"hooks": {}, "foreign": "change"}\n'
        fired = []

        def change():
            fired.append(1)
            with open(r.settings, "wb") as fh:
                fh.write(foreign)

        code, out = r.run_inproc("remove-entry", faults={"remove-entry:replace-settings": change})
        self.assertEqual(fired, [1], "stimulus not reached")
        self.assertEqual(code, 4, out)
        self.assertEqual(out["reason"], "DRIFT_BEFORE_WRITE")
        self.assertEqual(r.pre_bytes(), foreign)
        self.assertEqual(read_bytes(r.state), state_before)
        names = r.listing()
        backups = [n for n in names if n.startswith("settings-before-remove-")]
        self.assertEqual(len(backups), 1, names)
        self.assertIn(".aborted-", backups[0])
        self.assertEqual(temp_files(r), [])

    def test_TR_B1c_install_temp_complete_at_replace_point(self):
        """CONTROL: at install:replace-settings the temporary file already holds the whole postimage."""
        r = self.root()
        expected = json.loads(r.pre_bytes())
        expected["hooks"]["Notification"] = [{"matcher": "permission_prompt", "hooks": [
            {"type": "command", "command": r.command, "timeout": 10}]}]
        expected_bytes = (json.dumps(expected, indent=2, ensure_ascii=False) + "\n").encode()
        seen = []

        def look():
            names = temp_files(r)
            seen.append([read_bytes(os.path.join(os.path.dirname(r.settings), n)) for n in names])

        code, out = r.run_inproc("install", faults={"install:replace-settings": look})
        self.assertEqual(len(seen), 1, "stimulus not reached")
        self.assertEqual(seen[0], [expected_bytes])
        self.assertEqual(code, 0)

    def test_TR_B1c_install_rechecks_before_replace(self):
        """RED: a foreign change at install:replace-settings is never overwritten."""
        r = self.root()
        foreign = b'{"hooks": {}, "foreign": "change"}\n'
        fired = []

        def change():
            fired.append(1)
            with open(r.settings, "wb") as fh:
                fh.write(foreign)

        code, out = r.run_inproc("install", faults={"install:replace-settings": change})
        self.assertEqual(fired, [1], "stimulus not reached")
        self.assertEqual(code, 4, out)
        self.assertEqual(out["reason"], "DRIFT_BEFORE_WRITE")
        self.assertEqual(r.pre_bytes(), foreign)
        self.assertFalse(os.path.lexists(r.state))
        self.assertFalse(os.path.lexists(r.runtime_dir))
        names = r.listing()
        self.assertEqual(len([n for n in names if n.startswith("state.json.aborted-")]), 1)
        self.assertEqual(len([n for n in names if n.startswith(r.source_sha[:12] + ".aborted-")]), 1)
        self.assertEqual(temp_files(r), [])


C1_FAULT_POINTS = (
    ("install", "install:after-state-pending"), ("install", "install:before-settings-reread"),
    ("install", "install:replace-settings"), ("install", "install:after-settings-replace"),
    ("uninstall", "uninstall:after-state-pending"), ("uninstall", "uninstall:replace-settings"),
    ("uninstall", "uninstall:before-renames"), ("remove-entry", "remove-entry:after-state-pending"),
    ("remove-entry", "remove-entry:replace-settings"), ("remove-entry", "remove-entry:before-renames"),
    ("uninstall", "finish:between-renames"),
)
NEW_FAULT_POINTS = (("install", "install:after-runtime"), ("install", "install:after-installed"))


class TestRepairB3(RepairCase):
    def recover_from(self, mode, point, foreign):
        r = self.root()
        pre_obj = json.loads(r.pre_bytes())
        if mode != "install":
            self.install(r)
        self.interrupt(r, mode, point)
        if foreign:
            self.drifted(r)
        self.removal_protocol(r)
        obj = r.settings_obj()
        if foreign:
            self.assertEqual(obj["foreignKey"], {"kept": [1, 2, "three"]})
        self.assertEqual(obj["hooks"]["PermissionRequest"], pre_obj["hooks"]["PermissionRequest"])
        self.assertEqual(obj["enabledPlugins"], pre_obj["enabledPlugins"])
        self.assertEqual(observer_commands(obj), [])
        self.assertFalse(os.path.lexists(r.runtime_dir))
        self.assertFalse(os.path.lexists(r.state))
        self.install(r)

    def test_TR_B3c_recovery_table(self):
        """SCHEMA: from every C1 fault point the REMOVAL PROTOCOL reaches NOT_INSTALLED."""
        for mode, point in C1_FAULT_POINTS:
            for foreign in (False, True):
                with self.subTest(point=point, foreign=foreign):
                    self.recover_from(mode, point, foreign)

    def test_TR_B3c_recovery_table_new_points(self):
        """NEW-SEAM: the same from install:after-runtime and install:after-installed."""
        for mode, point in NEW_FAULT_POINTS:
            for foreign in (False, True):
                with self.subTest(point=point, foreign=foreign):
                    self.recover_from(mode, point, foreign)

    def test_TR_B3a_remove_entry_entry_already_absent(self):
        """RED: remove-entry completes when a foreign edit already removed the entry."""
        r = self.root()
        self.install(r)
        obj = r.settings_obj()
        del obj["hooks"]["Notification"]
        write_json(r.settings, obj)
        before = r.pre_bytes()
        code, out, _ = r.run("remove-entry")
        self.assertEqual(code, 0, out)
        self.assertEqual(out["result"], "ENTRY_ALREADY_ABSENT")
        self.assertEqual(r.pre_bytes(), before)
        self.assertFalse(os.path.lexists(r.runtime_dir))
        self.assertFalse(os.path.lexists(r.state))
        names = r.listing()
        self.assertEqual(len([n for n in names if n.startswith(r.source_sha[:12] + ".disabled-")]), 1)
        self.assertEqual(len([n for n in names if n.startswith("state.json.uninstalled-")]), 1)
        self.assertEqual(r.status()[1]["result"], "NOT_INSTALLED")
        self.install(r)

    def duplicated(self):
        r = self.root()
        self.install(r)
        obj = r.settings_obj()
        obj["hooks"]["Notification"].append(json.loads(json.dumps(obj["hooks"]["Notification"][0])))
        write_json(r.settings, obj)
        return r

    def test_TR_B3b_duplicate_entry_refused(self):
        """CONTROL: two observer entries make remove-entry refuse and write nothing."""
        r = self.duplicated()
        before = r.snapshot()
        code, out, _ = r.run("remove-entry")
        self.assertEqual(code, 4)
        self.assertEqual(out["observer_entries"], 2)
        self.assertEqual(r.snapshot(), before)

    def test_TR_B3b_duplicate_entry_schema(self):
        """SCHEMA: the refusal is DUPLICATE_ENTRY / owner-review, and status says BLOCKED."""
        r = self.duplicated()
        code, out, _ = r.run("remove-entry")
        self.assertEqual(out["reason"], "DUPLICATE_ENTRY")
        self.assertEqual(out["next_step"], "owner-review")
        code, out = r.status()
        self.assertEqual((code, out["result"], out["reason"], out["next_step"]),
                         (4, "BLOCKED", "DUPLICATE_ENTRY", "owner-review"))

    def test_TR_B3d_readme_protocols(self):
        """RED: the README states both protocols verbatim and no blanket 'run uninstall' rule."""
        with open(README, encoding="utf-8") as fh:
            text = fh.read()
        self.assertIn(REMOVAL_PROTOCOL, text)
        self.assertIn(INSTALL_PROTOCOL, text)
        self.assertIsNone(re.search(r"PENDING_UNINSTALL`?: \*\*run `uninstall`", text))
        self.assertIsNone(re.search(r"completes the\s+same transition from wherever it stopped", text))
        # Q3 CR-1: a change lost in the residual window is in neither copy; restoring one is unsafe
        self.assertIsNone(re.search(r"recoverable\s+only\s+from", text))
        self.assertIn("neither the preimage copy", text)
        self.assertIn("Never copy either\nback over the settings file", text)

    def test_TR_B3e_journal_precedes_runtime(self):
        """RED: an interruption at the first journal write leaves no runtime directory."""
        r = self.root()
        fired = []

        def patch(module):
            original = module.write_state

            def write_state(layout, state):
                fired.append(state.get("status"))
                if len(fired) == 1:
                    raise Crash()
                return original(layout, state)

            module.write_state = write_state

        try:
            r.run_inproc("install", patch=patch)
        except Crash:
            pass
        self.assertEqual(fired[:1], ["pending_install"], "stimulus not reached")
        self.assertFalse(os.path.lexists(r.runtime_dir))
        self.install_protocol(r, installs=1)

    def c1_collision_journal(self, removal):
        r = self.root()
        self.install(r)
        if removal:
            self.drifted(r)
            self.interrupt(r, "remove-entry", "remove-entry:before-renames")
            state = json.loads(read_bytes(r.state))
            renamed = state["removal_backup"] + "-1"
            os.rename(state["removal_backup"], renamed)
            self.rewrite_state(r, removal_backup=renamed)
        else:
            state = json.loads(read_bytes(r.state))
            renamed = state["preimage_copy"] + "-1"
            os.rename(state["preimage_copy"], renamed)
            self.rewrite_state(r, preimage_copy=renamed)
        return r

    def test_TR_B3f_c1_preimage_collision_name(self):
        """CONTROL: a C1 journal whose preimage copy has the '-1' collision name is accepted."""
        r = self.root()
        pre = r.pre_bytes()
        self.install(r)
        state = json.loads(read_bytes(r.state))
        renamed = state["preimage_copy"] + "-1"
        os.rename(state["preimage_copy"], renamed)
        self.rewrite_state(r, preimage_copy=renamed)
        code, out, _ = r.run("uninstall")
        self.assertEqual((code, out["result"]), (0, "UNINSTALLED"))
        self.assertEqual(r.pre_bytes(), pre)

    def test_TR_B3f_c1_removal_backup_collision_name(self):
        """CONTROL: a C1 remove-entry journal whose backup has the '-1' collision name is accepted."""
        r = self.c1_collision_journal(removal=True)
        removed = r.pre_bytes()
        code, out, _ = r.run("uninstall")
        self.assertEqual((code, out["result"]), (0, "UNINSTALLED"))
        self.assertEqual(r.pre_bytes(), removed)

    def test_TR_B3f_c1_collision_protocol(self):
        """SCHEMA: the REMOVAL PROTOCOL completes from the same C1 journal."""
        r = self.c1_collision_journal(removal=True)
        self.removal_protocol(r)

    def test_TR_B3g_receipt_print_failure(self):
        """RED: a failure while printing the final receipt is exit 6; status shows the final state."""
        r = self.root()
        marker = os.path.join(r.root, "MARK-emit-install")
        code, out, proc = r.run_driver("install", {"emit_fail": {"results": ["INSTALLED"], "marker": marker}})
        self.assertTrue(os.path.exists(marker), "stimulus not reached")
        self.assertEqual(code, 6, proc.stderr)
        self.assertEqual(r.status()[1]["result"], "MATCH")
        marker = os.path.join(r.root, "MARK-emit-uninstall")
        code, out, proc = r.run_driver("uninstall", {"emit_fail": {"results": ["UNINSTALLED"], "marker": marker}})
        self.assertTrue(os.path.exists(marker), "stimulus not reached")
        self.assertEqual(code, 6, proc.stderr)
        self.assertEqual(r.status()[1]["result"], "NOT_INSTALLED")

    def test_TR_B3h_write_failed_output(self):
        """SCHEMA: a handled write error prints exactly result, error and next_step status."""
        r = self.root()
        self.install(r)
        fired = []
        code, out = r.run_inproc("uninstall", faults={
            "uninstall:replace-settings": raise_at("uninstall:replace-settings", fired, OSError("injected"))})
        self.assertEqual(fired, ["uninstall:replace-settings"], "stimulus not reached")
        self.assertEqual((code, out), (6, {"error": "OSError", "next_step": "status", "result": "WRITE_FAILED"}))
        self.removal_protocol(r)

    def install_interrupted(self, point):
        r = self.root()
        self.interrupt(r, "install", point)
        self.install_protocol(r, installs=1)

    def test_TR_B3i_install_protocol(self):
        """SCHEMA: after an interrupted install the INSTALL PROTOCOL reaches MATCH."""
        for mode, point in C1_FAULT_POINTS:
            if mode == "install":
                with self.subTest(point=point):
                    self.install_interrupted(point)

    def test_TR_B3i_install_protocol_new_points(self):
        """NEW-SEAM: the same from install:after-runtime and install:after-installed."""
        for _, point in NEW_FAULT_POINTS:
            with self.subTest(point=point):
                self.install_interrupted(point)

    def test_TR_B3j_orphaned_runtime_renamed(self):
        """RED: a runtime directory without journal and entry is renamed .orphaned- and install succeeds."""
        r = self.root()
        os.mkdir(r.obs_root, 0o700)
        os.chmod(r.obs_root, 0o700)
        os.mkdir(r.runtime_dir, 0o755)
        os.chmod(r.runtime_dir, 0o755)
        code, out, _ = r.run("install")
        self.assertEqual((code, out["result"]), (0, "INSTALLED"), out)
        names = r.listing()
        self.assertEqual(len([n for n in names if n.startswith(r.source_sha[:12] + ".orphaned-")]), 1, names)
        self.assertEqual(sha256_file(r.runtime), r.source_sha)

    def orphan_entry_root(self):
        doc = base_settings()
        doc["hooks"]["Notification"] = [{"hooks": [{"type": "command",
                                                     "command": "python3 -I /x/prompt_observer.py --expect-sha256 x"}]}]
        return self.root(settings=doc)

    def test_TR_B3k_status_orphan_entry(self):
        """RED: a registration without a journal is BLOCKED ORPHAN_ENTRY."""
        r = self.orphan_entry_root()
        code, out = r.status()
        self.assertEqual((code, out["result"]), (4, "BLOCKED"))
        self.assertEqual(out["reason"], "ORPHAN_ENTRY")
        self.assertEqual(out["next_step"], "owner-review")

    def test_TR_B3k_install_refuses_orphan_entry(self):
        """CONTROL: install refuses OBSERVER_ENTRY_EXISTS and writes nothing."""
        r = self.orphan_entry_root()
        before = r.snapshot()
        code, out, _ = r.run("install")
        self.assertEqual((code, out.get("reason")), (4, "OBSERVER_ENTRY_EXISTS"), out)
        self.assertEqual(r.snapshot(), before)

    UNSERIALISABLE = (b"1e400", b'"\\ud800"')

    def with_foreign_fragment(self, r, fragment):
        raw = r.pre_bytes()
        marker = b'"permissions": {'
        self.assertEqual(raw.count(marker), 1)
        with open(r.settings, "wb") as fh:
            fh.write(raw.replace(marker, b'"q3Foreign": ' + fragment + b", " + marker))

    def test_TR_Q1_remove_entry_unserialisable_foreign_value(self):
        """RED (Q3 SF-1): a foreign value the installer cannot write back is an owner-review refusal, not a crash."""
        for fragment in self.UNSERIALISABLE:
            with self.subTest(fragment=fragment):
                r = self.root()
                self.install(r)
                self.with_foreign_fragment(r, fragment)
                # stimulus: the file still parses and the entry is still registered
                self.assertEqual(len(observer_commands(json.loads(r.pre_bytes()))), 1)
                before = r.snapshot()
                code, out, proc = r.run("remove-entry")
                self.assertNotIn(b"Traceback", proc.stderr)
                self.assertEqual(len(proc.stdout.decode().splitlines()), 1, proc.stderr)
                self.assertEqual((code, out["reason"], out["next_step"]),
                                 (3, "SETTINGS_NOT_SERIALISABLE", "owner-review"))
                self.assertEqual(r.snapshot(), before)
                code, out = r.status()
                self.assertEqual((code, out["result"], out["reason"], out["next_step"]),
                                 (4, "BLOCKED", "SETTINGS_INVALID", "owner-review"))

    def test_TR_Q1_install_unserialisable_preimage(self):
        """RED (Q3 CR-5): install refuses a preimage it could not write back, before any write."""
        for fragment in self.UNSERIALISABLE:
            with self.subTest(fragment=fragment):
                r = self.root()
                self.with_foreign_fragment(r, fragment)
                before = r.snapshot()
                code, out, proc = r.run("install")
                self.assertNotIn(b"Traceback", proc.stderr)
                self.assertEqual(len(proc.stdout.decode().splitlines()), 1, proc.stderr)
                self.assertEqual((code, out["reason"], out["next_step"]),
                                 (3, "SETTINGS_NOT_SERIALISABLE", "owner-review"))
                self.assertEqual(r.snapshot(), before)
                self.assertFalse(os.path.lexists(r.obs_root))


class TestRepairB4(RepairCase):
    def test_TR_B4a_locked(self):
        """RED: a held installer lock makes every writing mode exit LOCKED and write nothing."""
        fresh = self.root()
        installed = self.root()
        self.install(installed)
        for r, mode in ((fresh, "install"), (installed, "uninstall"), (installed, "remove-entry")):
            with self.subTest(mode=mode):
                before = r.snapshot()
                with held_lock(r.lock):
                    code, out, _ = r.run(mode)
                self.assertEqual(code, 4, out)
                self.assertEqual(out["reason"], "LOCKED")
                self.assertEqual(out["next_step"], "status")
                self.assertEqual(r.snapshot(), before)

    def test_TR_B4a_lock_file_created(self):
        """RED: a writing run leaves an empty 0600 lock file in the hooks root."""
        r = self.root(precreate_lock=False)
        self.install(r)
        self.assertTrue(os.path.lexists(r.lock), "no lock file")
        info = os.lstat(r.lock)
        self.assertTrue(stat.S_ISREG(info.st_mode))
        self.assertEqual(stat.S_IMODE(info.st_mode), 0o600)
        self.assertEqual(info.st_size, 0)

    def test_TR_B4b_two_installer_race(self):
        """RED: two concurrent installs never leave a registration without its journal."""
        r = self.root(settings=base_settings(with_notification=True))
        pre = r.pre_bytes()
        pre_obj = json.loads(pre)
        mb, rb, ma, ra = (os.path.join(r.root, name) for name in ("MB", "RB", "MA", "RA"))
        b = r.run_driver("install", {"read_state_hold": {"marker": mb, "release": rb}}, wait=False)
        try:
            wait_for(lambda: os.path.exists(mb) or b.poll() is not None)
            self.assertTrue(os.path.exists(mb), "stimulus not reached")
            source_a = r.source_bytes + b"# installer race: source A\n"
            with open(r.source, "wb") as fh:
                fh.write(source_a)
            a = r.run_driver("install", {"barrier": {"point": "install:after-state-pending", "marker": ma,
                                                     "release": ra}}, wait=False, source=sha256_bytes(source_a))
            try:
                wait_for(lambda: os.path.exists(ma) or a.poll() is not None)
                open(rb, "w").close()
                b.communicate(timeout=TIMEOUT)
                open(ra, "w").close()
                a.communicate(timeout=TIMEOUT)
            finally:
                if a.poll() is None:
                    a.kill()
                    a.communicate()
        finally:
            if b.poll() is None:
                b.kill()
                b.communicate()
        code, out = r.status()
        self.assertEqual(out["result"], "MATCH", out)
        obj = r.settings_obj()
        self.assertEqual(len(observer_commands(obj)), 1)
        self.assertEqual(obj["hooks"]["Notification"][0]["hooks"][0]["command"], FOREIGN_NOTIFY)
        for key in ("enabledPlugins", "permissions"):
            self.assertEqual(obj[key], pre_obj[key])
        self.assertEqual(obj["hooks"]["PermissionRequest"], pre_obj["hooks"]["PermissionRequest"])
        self.assertEqual(r.run("uninstall")[0], 0)
        self.assertEqual(r.status()[1]["result"], "NOT_INSTALLED")
        self.assertEqual(r.pre_bytes(), pre)

    def test_TR_B4c_abort_keeps_foreign_journal(self):
        """RED: a failing install never renames a journal it did not write."""
        r = self.root()
        foreign = b'{"foreign": "journal"}\n'
        fired = []

        def overwrite():
            fired.append(1)
            with open(r.state, "wb") as fh:
                fh.write(foreign)
            raise OSError("injected")

        code, out = r.run_inproc("install", faults={"install:after-state-pending": overwrite})
        self.assertEqual(fired, [1], "stimulus not reached")
        self.assertTrue(os.path.lexists(r.state), "the foreign journal was renamed")
        self.assertEqual(read_bytes(r.state), foreign)
        self.assertEqual(code, 6, out)
        self.assertTrue(out.get("foreign_state"), out)

    def test_TR_B4d_lock_released_in_process(self):
        """CONTROL: after an in-process interruption the next in-process uninstall completes."""
        r = self.root()
        self.install(r)
        self.interrupt(r, "uninstall", "uninstall:after-state-pending")
        code, out = r.run_inproc("uninstall")
        self.assertEqual((code, out["result"]), (0, "UNINSTALLED"), out)

    def test_TR_B4d_protocol_in_process(self):
        """SCHEMA: the REMOVAL PROTOCOL in-process reaches NOT_INSTALLED without LOCKED."""
        r = self.root()
        self.install(r)
        self.interrupt(r, "uninstall", "uninstall:after-state-pending")
        self.removal_protocol(r, inproc=True)

    def test_TR_B4e_lock_unsafe(self):
        """RED: a symlinked, 0644 or hard-linked lock file is refused LOCK_UNSAFE."""
        for case in ("symlink", "mode", "hardlink"):
            with self.subTest(case=case):
                r = self.root(precreate_lock=False)
                target = os.path.join(r.root, "lock-target")
                os.close(os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600))
                os.chmod(target, 0o600)
                if case == "symlink":
                    os.symlink(target, r.lock)
                elif case == "mode":
                    os.close(os.open(r.lock, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600))
                    os.chmod(r.lock, 0o644)
                else:
                    os.link(target, r.lock)
                before = r.snapshot()
                code, out, _ = r.run("install")
                self.assertEqual(code, 4, out)
                self.assertEqual(out["reason"], "LOCK_UNSAFE")
                self.assertEqual(out["next_step"], "owner-review")
                self.assertEqual(r.snapshot(), before)
                self.assertFalse(os.path.lexists(r.obs_root))

    def killed_install(self):
        r = self.root()
        marker = os.path.join(r.root, "MARK-exit")
        code, _, _ = r.run_driver("install", {"exit_at": {"point": "install:after-state-pending",
                                                          "marker": marker, "code": 9}})
        self.assertTrue(os.path.exists(marker), "stimulus not reached")
        self.assertEqual(code, 9)
        return r

    def test_TR_B4f_lock_released_by_process_exit(self):
        """CONTROL: a killed install leaves PENDING_INSTALL and the next uninstall completes."""
        r = self.killed_install()
        code, out = r.status()
        self.assertEqual((code, out["result"]), (1, "PENDING_INSTALL"))
        code, out, _ = r.run("uninstall")
        self.assertEqual((code, out["result"]), (0, "UNINSTALLED"), out)

    def test_TR_B4f_protocol_after_process_exit(self):
        """SCHEMA: the REMOVAL PROTOCOL in subprocesses reaches NOT_INSTALLED."""
        r = self.killed_install()
        self.removal_protocol(r)


class TestRepairP1(RepairCase):
    def test_TR_P1a_observer_root_mode(self):
        """RED: an existing observer root with any mode but 0700 is refused before any write."""
        for mode in (0o750, 0o770, 0o755, 0o707):
            with self.subTest(mode=oct(mode)):
                r = self.root(precreate_lock=False)
                os.mkdir(r.obs_root)
                os.chmod(r.obs_root, mode)
                before = r.snapshot()
                code, out, _ = r.run("install")
                self.assertEqual(code, 3, out)
                self.assertEqual(out["reason"], "OBSERVER_ROOT_UNSAFE")
                self.assertEqual(out["next_step"], "owner-review")
                self.assertEqual(r.snapshot(), before)
                self.assertFalse(os.path.lexists(r.lock))

    def test_TR_P1b_hooks_root_writable(self):
        """RED: a group- or world-writable hooks root is refused."""
        for mode in (0o775, 0o757):
            with self.subTest(mode=oct(mode)):
                r = self.root(precreate_lock=False)
                os.chmod(r.hooks_root, mode)
                before = r.snapshot()
                code, out, _ = r.run("install")
                self.assertEqual(code, 3, out)
                self.assertEqual(out["reason"], "HOOKS_ROOT_UNSAFE")
                self.assertEqual(r.snapshot(), before)

    def test_TR_P1c_ancestor_writable(self):
        """RED: a group-writable ancestor between the test root and the hooks root is refused."""
        r = self.root(precreate_lock=False)
        os.chmod(os.path.join(r.root, "home", ".claude"), 0o775)
        before = r.snapshot()
        code, out, _ = r.run("install")
        self.assertEqual(code, 3, out)
        self.assertEqual(out["reason"], "ANCESTOR_UNSAFE")
        self.assertEqual(r.snapshot(), before)

    def test_TR_P1d_unsafe_after_install(self):
        """RED: an observer root made 0770 after install blocks status and every removal mode."""
        r = self.root()
        self.install(r)
        os.chmod(r.obs_root, 0o770)
        code, out = r.status()
        self.assertEqual((code, out["result"]), (4, "BLOCKED"), out)
        self.assertEqual((out["roots"], out["chain_fault"], out["chain_component"]), ("unsafe", "mode", "obs_root"))
        before = r.snapshot()
        for mode in ("uninstall", "remove-entry"):
            with self.subTest(mode=mode):
                code, out, _ = r.run(mode)
                self.assertEqual(code, 3, out)
                self.assertEqual(r.snapshot(), before)

    def test_TR_P1e_i_symlinked_home(self):
        """RED: a symlinked component above the hooks root is refused by every writing mode."""
        r = self.root()
        self.install(r)
        home = os.path.join(r.root, "home")
        elsewhere = os.path.join(r.root, "elsewhere-home")
        os.rename(home, elsewhere)
        os.symlink(elsewhere, home)
        sentinel = os.path.join(r.root, "sentinel-dir")
        os.mkdir(sentinel)
        with open(os.path.join(sentinel, "keep"), "w") as fh:
            fh.write("sentinel\n")
        before = r.snapshot()
        for mode in ("install", "uninstall", "remove-entry"):
            with self.subTest(mode=mode):
                code, out, _ = r.run(mode)
                self.assertEqual(code, 3, out)
                self.assertEqual(out.get("chain_fault"), "symlink", out)
                self.assertEqual(r.snapshot(), before)

    def test_TR_P1e_ii_foreign_owner(self):
        """NEW-SEAM: an effective uid that owns nothing makes every writing mode refuse (owner)."""
        r = self.root()
        self.install(r)
        before = r.snapshot()
        for mode in ("install", "uninstall", "remove-entry"):
            with self.subTest(mode=mode):
                fired = []

                def patch(module):
                    def effective_uid():
                        fired.append(1)
                        return os.geteuid() + 4242
                    module.effective_uid = effective_uid

                code, out = r.run_inproc(mode, patch=patch)
                self.assertTrue(fired, "stimulus not reached")
                self.assertEqual(code, 3, out)
                self.assertEqual(out["chain_fault"], "owner")
                self.assertEqual(r.snapshot(), before)

    def test_TR_P1e_iii_symlinked_runtime_dir(self):
        """RED: a runtime directory replaced by a symlink is refused RUNTIME_DIR_UNSAFE."""
        r = self.root()
        self.install(r)
        os.rename(r.runtime_dir, os.path.join(r.root, "moved-runtime"))
        sentinel = os.path.join(r.root, "sentinel-runtime")
        os.mkdir(sentinel)
        with open(os.path.join(sentinel, "keep"), "w") as fh:
            fh.write("sentinel\n")
        os.symlink(sentinel, r.runtime_dir)
        before = r.snapshot()
        code, out, _ = r.run("uninstall")
        self.assertEqual(code, 3, out)
        self.assertEqual(out["reason"], "RUNTIME_DIR_UNSAFE")
        self.assertEqual(r.snapshot(), before)

    def test_TR_P1f_walk_chain_system_components(self):
        """NEW-SEAM: walk_chain with the real system ancestors (uid-0 rule, sticky exception)."""
        module = load_installer()
        self.assertTrue(hasattr(module, "walk_chain"), "stimulus not reached: no walk_chain")
        system_tmp = "/tmp"
        root_info = os.lstat("/")
        tmp_info = os.lstat(system_tmp)
        self.assertEqual(root_info.st_uid, 0)
        self.assertTrue(stat.S_ISDIR(tmp_info.st_mode))
        self.assertEqual(tmp_info.st_uid, 0)
        self.assertTrue(tmp_info.st_mode & stat.S_ISVTX)
        home = tempfile.mkdtemp(dir=system_tmp, prefix="g1r-walk-")
        try:
            def layout(base):
                hooks = os.path.join(base, ".claude", "hooks")
                os.makedirs(hooks)
                os.chmod(os.path.join(base, ".claude"), 0o755)
                os.chmod(hooks, 0o755)
                obs_root = os.path.join(hooks, "prompt-observer")
                os.mkdir(obs_root, 0o700)
                os.chmod(obs_root, 0o700)
                return hooks, obs_root

            hooks, obs_root = layout(home)
            euid = os.geteuid()
            self.assertIsNone(module.walk_chain(hooks, obs_root, home, euid))
            deeper = os.path.join(home, "inner")
            os.mkdir(deeper, 0o700)
            hooks2, obs_root2 = layout(deeper)
            self.assertEqual(module.walk_chain(hooks2, obs_root2, deeper, euid), ("system", "owner"))
            outside = module.walk_chain(hooks, obs_root, deeper, euid)
            self.assertIsNotNone(outside)
            self.assertIn(outside[0], ("system", "home", "ancestor"))
        finally:
            shutil.rmtree(home)


class TestRepairState(RepairCase):
    def test_TR_S1_state_paths_outside_layout(self):
        """CONTROL: a journal naming paths outside the observer root is STATE_CORRUPT."""
        for change in ({"runtime_path": "/elsewhere/prompt_observer.py"},
                       {"preimage_copy": "/elsewhere/settings-preimage-20261018T000000Z.json"}):
            for mode in ("uninstall", "remove-entry"):
                with self.subTest(change=change, mode=mode):
                    r = self.root()
                    self.install(r)
                    self.rewrite_state(r, **change)
                    before = r.snapshot()
                    code, out, _ = r.run(mode)
                    self.assertEqual((code, out.get("reason")), (4, "STATE_CORRUPT"), out)
                    self.assertEqual(r.snapshot(), before)

    def test_TR_S1_state_corrupt_schema(self):
        """SCHEMA: STATE_CORRUPT names next_step owner-review."""
        r = self.root()
        self.install(r)
        self.rewrite_state(r, runtime_path="/elsewhere/prompt_observer.py")
        code, out, _ = r.run("uninstall")
        self.assertEqual(out.get("next_step"), "owner-review", out)

    def test_TR_S2_runtime_hash_validated(self):
        """RED: a non-hex runtime_sha256 (upper case, or '../sentinelX…') is STATE_CORRUPT; nothing renamed."""
        for variant in ("upper", "traversal"):
            for mode in ("uninstall", "remove-entry"):
                with self.subTest(variant=variant, mode=mode):
                    r = self.root()
                    self.install(r)
                    sha = r.source_sha.upper() if variant == "upper" else "../sentinelX" + "a" * 52
                    self.assertEqual(len(sha), 64)
                    runtime_path = os.path.join(r.obs_root, sha[:12], "prompt_observer.py")
                    self.rewrite_state(r, runtime_sha256=sha, runtime_path=runtime_path,
                                       registration_command=f"python3 -I {runtime_path} --expect-sha256 {sha}")
                    sentinel = os.path.join(r.hooks_root, "sentinelX")
                    os.mkdir(sentinel)
                    with open(os.path.join(sentinel, "keep"), "w") as fh:
                        fh.write("sentinel\n")
                    before = r.snapshot()
                    code, out, _ = r.run(mode)
                    self.assertEqual((code, out.get("reason")), (4, "STATE_CORRUPT"), out)
                    self.assertEqual(r.snapshot(), before)

    def test_TR_S3_preimage_copy_name(self):
        """RED: a preimage copy name with the prefix but not the <UTC>.json form is STATE_CORRUPT."""
        r = self.root()
        self.install(r)
        state = json.loads(read_bytes(r.state))
        renamed = os.path.join(r.obs_root, "settings-preimage-x.json")
        os.rename(state["preimage_copy"], renamed)
        self.rewrite_state(r, preimage_copy=renamed)
        before = r.snapshot()
        code, out, _ = r.run("uninstall")
        self.assertEqual((code, out.get("reason")), (4, "STATE_CORRUPT"), out)
        self.assertEqual(r.snapshot(), before)

    def test_TR_S4_state_file_checks(self):
        """RED: a journal with mode 0644 or a second hard link is STATE_UNSAFE."""
        for case in ("mode", "hardlink"):
            with self.subTest(case=case):
                r = self.root()
                self.install(r)
                if case == "mode":
                    os.chmod(r.state, 0o644)
                else:
                    os.link(r.state, os.path.join(r.root, "state-second-link"))
                before = r.snapshot()
                code, out, _ = r.run("uninstall")
                self.assertEqual((code, out.get("reason")), (4, "STATE_UNSAFE"), out)
                self.assertEqual(r.snapshot(), before)


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
