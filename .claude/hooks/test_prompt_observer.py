#!/usr/bin/env python3
"""Offline tests for prompt_observer.py (guard-g1-contract-rev3, section 3.8: TO-1 to TO-9).

Run as a script, never through ``-m unittest``::

    python3 -I .claude/hooks/test_prompt_observer.py -v [--observer PATH]

Pure functions of the observer (shape, sanitize, the notification-to-record
builder) are tested in-process: the observer file's bytes are compiled and
executed into a private module object, never through the import system, so no
bytecode file is written. Everything else runs the observer as a subprocess:

* hook mode against a temporary HOME, project, transcript and guard fixture (a
  byte copy of the clone's permission_guard.py placed at the test HOOKS_ROOT
  layout with mode 0444). Hook mode has no test option, so these tests run a
  copy of the observer in which exactly one line, the HOOKS_ROOT constant, points
  at the test layout;
* explain mode with ``--test-root`` (HOOKS_ROOT = <test root>/home/.claude/hooks).

The guard decisions are compared with the guard fixture run as a subprocess
PermissionRequest hook. Zero collected tests or any skipped test fails the run.
Test names carry the requirement they pin (TO-1 ... TO-9).
"""

import argparse
import ast
import concurrent.futures
import hashlib
import json
import os
import random
import re
import shlex
import stat
import string
import subprocess
import sys
import tempfile
import types
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_OBSERVER = os.path.join(HERE, "prompt_observer.py")
GUARD_SOURCE = os.path.join(HERE, "permission_guard.py")
OBSERVER = DEFAULT_OBSERVER  # overridden by --observer in main()

BASE_GUARD_SHA = "5b16b120defa408b80047dd144c0ee1cc1dfbee519c9f51b2b92a6d322ffc0d1"
HOOKS_ROOT_LINE = 'HOOKS_ROOT = "/home/claude/.claude/hooks"'
REPO = "github.com/KraineOpasen/bukerov-twitch-miner-go"
API_PULLS = "repos/KraineOpasen/bukerov-twitch-miner-go/pulls"
BRANCH = "chore/guard-g1-observer"
PR = 431
PR_HEAD = "b" * 40
T1 = "@coderabbitai full review"
T2 = "@codex review Full review of the entire PR. Review only; make no changes."
T3 = "@codex security review"
ALLOW_JSON = ('{"hookSpecificOutput": {"hookEventName": "PermissionRequest", '
              '"decision": {"behavior": "allow"}}}\n')
SID = "0123abcd-1111-4222-8333-444455556666"
SID_B = "fedc9876-aaaa-4bbb-8ccc-ddddeeeeffff"
TAIL = 1 << 20
TIMEOUT = 60

FIXED_REASONS = frozenset((
    "no_registration", "registration_shape", "guard_hash", "guard_unknown", "guard_import",
    "not_bash", "no_command", "no matching shape", "unlisted_reason", "ambiguous", "binding",
    "truncated", "error",
))
ERROR_REASON_RE = re.compile(r"error:[A-Za-z]{1,40}")
TS_RE = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z")
SHORT_FLAG_RE = re.compile(r"-[A-Za-z0-9]")
FIELD_STRINGS = frozenset((
    # keys of the hook record, the explain line, SHAPE and its segments (3.1 (b), 3.2, 3.4)
    "v", "ts", "event", "session", "source", "pending", "tool", "shape", "guard", "reason", "mode",
    "decision", "guard_sha256", "test_root", "oversize",
    "segments", "ops", "more_segments", "unparsed", "expansion",
    "program", "sub", "flags", "operands", "assign", "flag_count",
    # fixed field values of 3.4
    "permission_prompt", "pending_tool_use", "ambiguous", "no_pending_tool_use", "transcript_unreadable",
    "ALLOW", "NO_DECISION", "NOT_EVALUATED", "given", "assumed_default", "unknown",
    "-N", "-?", "?", "?op", "OTHER", "mcp",
))

_MODULES = {}


def sha256_file(path):
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def load_file(path, name):
    """Compile and execute a file's bytes into a private module object (no import system,
    no sys.modules entry, no bytecode file)."""
    with open(path, "rb") as fh:
        data = fh.read()
    module = types.ModuleType(name)
    module.__file__ = path
    exec(compile(data, path, "exec"), module.__dict__)
    return module


def obs():
    if "obs" not in _MODULES:
        _MODULES["obs"] = load_file(OBSERVER, "_g1_observer_under_test")
    return _MODULES["obs"]


def guard_reason_literals(path=GUARD_SOURCE):
    """TO-7: every argument that becomes a NoDecision message, from the guard's source (ast only).
    Returns (set of str literals, list of non-constant descriptions)."""
    with open(path, "rb") as fh:
        tree = ast.parse(fh.read(), path)
    literals = set()
    bad = []

    def take(node, where):
        if isinstance(node, ast.Constant) and isinstance(node.value, str):
            literals.add(node.value)
        else:
            bad.append(f"{where} at line {getattr(node, 'lineno', '?')}")

    plumbing = set()  # require()'s own `raise NoDecision(<its message parameter>)`
    for node in ast.walk(tree):
        if isinstance(node, ast.FunctionDef) and node.name == "require":
            for default in node.args.defaults:
                take(default, "require default")
            param = node.args.args[1].arg if len(node.args.args) >= 2 else None
            for inner in ast.walk(node):
                if (isinstance(inner, ast.Call) and isinstance(inner.func, ast.Name)
                        and inner.func.id == "NoDecision" and len(inner.args) == 1
                        and isinstance(inner.args[0], ast.Name) and inner.args[0].id == param):
                    plumbing.add(id(inner))
    for node in ast.walk(tree):
        if isinstance(node, ast.Call) and isinstance(node.func, ast.Name):
            if id(node) in plumbing:
                continue
            if node.func.id == "NoDecision":
                if len(node.args) != 1 or node.keywords:
                    bad.append(f"NoDecision call shape at line {node.lineno}")
                else:
                    take(node.args[0], "NoDecision")
            elif node.func.id == "require":
                if len(node.args) >= 2:
                    take(node.args[1], "require")
                for keyword in node.keywords:
                    take(keyword.value, "require keyword")
    return literals, bad


def allowed_strings():
    m = obs()
    allowed = set(FIELD_STRINGS) | FIXED_REASONS
    for name in ("PROGRAMS", "GIT_SUBS", "GH_TOP", "GO_SUBS", "FLAG_VOCAB", "TOOLS", "OPS"):
        allowed |= set(getattr(m, name))
    for values in m.GH_SECOND.values():
        allowed |= set(values)
    allowed |= guard_reason_literals()[0]
    return allowed


def strings_in(value, out):
    if isinstance(value, str):
        out.append(value)
    elif isinstance(value, dict):
        for key, item in value.items():
            out.append(key)
            strings_in(item, out)
    elif isinstance(value, list):
        for item in value:
            strings_in(item, out)
    return out


def is_allowed(text, allowed, extra=()):
    return (text in allowed or text in extra or SHORT_FLAG_RE.fullmatch(text) is not None
            or TS_RE.fullmatch(text) is not None or ERROR_REASON_RE.fullmatch(text) is not None)


# --- expected-shape builders ---------------------------------------------------------------

def seg(program, sub=(), flags=(), operands=0, assign=0):
    return {"program": program, "sub": list(sub), "flags": list(flags), "operands": operands, "assign": assign}


def other(flag_count=0, operands=0, assign=0):
    return {"program": "OTHER", "sub": [], "flags": [], "flag_count": flag_count, "operands": operands,
            "assign": assign}


def shp(segments, ops=(), more=0, unparsed=False, expansion=False):
    return {"segments": list(segments), "ops": list(ops), "more_segments": more, "unparsed": unparsed,
            "expansion": expansion}


# --- the fixture world ----------------------------------------------------------------------

class World:
    """A temporary HOME, project, GOROOT, fake gh/git, guard fixture and patched observer copy."""

    def __init__(self, tmp):
        self.tmp = os.path.realpath(tmp)
        self.home = os.path.join(self.tmp, "home")
        self.hooks_root = os.path.join(self.home, ".claude", "hooks")  # = explain --test-root <tmp>
        self.projects = os.path.join(self.home, ".claude", "projects")
        self.tdir = os.path.join(self.projects, "-proj")
        os.makedirs(self.hooks_root)
        os.makedirs(self.tdir)
        self.proj = os.path.join(self.tmp, "proj")
        os.makedirs(os.path.join(self.proj, ".git"))
        with open(os.path.join(self.proj, ".git", "HEAD"), "w") as fh:
            fh.write(f"ref: refs/heads/{BRANCH}\n")
        with open(os.path.join(self.proj, ".git", "config"), "w") as fh:
            fh.write("[core]\n")
        os.makedirs(os.path.join(self.proj, ".claude"))
        self.scratch = os.path.join(self.proj, ".scratch")
        os.makedirs(os.path.join(self.scratch, "permission-guard"))
        self.record_path = os.path.join(self.scratch, "permission-guard", "task-record.json")
        os.makedirs(os.path.join(self.proj, "internal"))
        with open(os.path.join(self.proj, "internal", "a.go"), "w") as fh:
            fh.write("package internal\n\nfunc A() {}\n")
        self.goroot = os.path.join(self.tmp, "goroot")
        os.makedirs(os.path.join(self.goroot, "src", "time"))
        self.sleep_go = os.path.join(self.goroot, "src", "time", "sleep.go")
        with open(self.sleep_go, "w") as fh:
            fh.write("package time\n\nfunc Sleep(d Duration)\n")
        self.bin = os.path.join(self.tmp, "bin")
        os.mkdir(self.bin)
        self.gh = self._exe("gh", "#!/bin/sh\necho fake-gh\n")
        self.git = self._exe("git", "#!/bin/sh\necho fake-git\n")
        self.body = os.path.join(self.scratch, "PR-BODY.md")
        with open(self.body, "w") as fh:
            fh.write("# PR body\n\nsnapshot\n")
        self.body_sha = sha256_file(self.body)
        self.set_record()
        # guard fixture at the layout path
        with open(GUARD_SOURCE, "rb") as fh:
            self.guard_bytes = fh.read()
        self.guard_sha = sha256_bytes(self.guard_bytes)
        self.guard_path = self.place_guard(self.guard_bytes)
        self.settings = os.path.join(self.proj, ".claude", "settings.local.json")
        self.register_guard(self.guard_path, self.guard_sha)
        # the hook-mode observer copy: only the HOOKS_ROOT line differs
        with open(OBSERVER, "rb") as fh:
            text = fh.read().decode("utf-8")
        if text.count(HOOKS_ROOT_LINE) != 1:
            raise AssertionError("observer must define HOOKS_ROOT on exactly one line")
        patched = text.replace(HOOKS_ROOT_LINE, f'HOOKS_ROOT = "{self.hooks_root}"')
        self.obs_dir = os.path.join(self.tmp, "obs")
        os.mkdir(self.obs_dir)
        self.observer_copy = os.path.join(self.obs_dir, "prompt_observer.py")
        with open(self.observer_copy, "w", encoding="utf-8") as fh:
            fh.write(patched)
        self.log_dir = os.path.join(self.scratch, "prompt-observer")
        self.log = os.path.join(self.log_dir, "prompts.jsonl")
        self.cmd_dir = os.path.join(self.tmp, "cmds")
        os.mkdir(self.cmd_dir)
        self._cmd_n = 0

    def _exe(self, name, content):
        path = os.path.join(self.bin, name)
        with open(path, "w") as fh:
            fh.write(content)
        os.chmod(path, 0o755)
        return path

    def place_guard(self, data, sha=None, root=None):
        sha = sha or sha256_bytes(data)
        directory = os.path.join(root or self.hooks_root, "permission-guard", sha[:12])
        os.makedirs(directory, exist_ok=True)
        path = os.path.join(directory, "permission_guard.py")
        if os.path.exists(path):
            os.chmod(path, 0o644)
            os.unlink(path)
        with open(path, "wb") as fh:
            fh.write(data)
        os.chmod(path, 0o444)
        return path

    def register_guard(self, path, sha, extra_entries=()):
        doc = {"hooks": {"PermissionRequest": [
            {"matcher": "Bash", "hooks": [
                {"type": "command", "command": f"python3 -I {path} --expect-sha256 {sha}", "timeout": 10}]}]
            + list(extra_entries)}}
        self.write_settings(doc)

    def write_settings(self, doc):
        with open(self.settings, "w") as fh:
            json.dump(doc, fh, indent=2)

    def set_record(self, state="active", with_pr=True, **over):
        rec = {
            "contract": "test-g1", "contract_sha256": "0" * 64, "state": state, "repo": REPO,
            "branch": BRANCH, "evidence_root": os.path.join(self.scratch, "g1"),
            "gh_path": self.gh, "gh_sha256": sha256_file(self.gh),
            "git_path": self.git, "git_sha256": sha256_file(self.git),
            "goroot": self.goroot, "body_sha256": self.body_sha,
        }
        if with_pr:
            rec["pr"] = PR
            rec["pr_head"] = PR_HEAD
        rec.update(over)
        with open(self.record_path, "w") as fh:
            json.dump(rec, fh)

    def env(self, extra=None):
        env = {"PATH": self.bin, "HOME": self.home, "CLAUDE_PROJECT_DIR": self.proj, "LANG": "C.UTF-8"}
        if extra:
            env.update(extra)
        return env

    # transcripts -----------------------------------------------------------------
    def write_transcript(self, sid, entries, name=None, directory=None, raw_lines=None):
        directory = directory or self.tdir
        os.makedirs(directory, exist_ok=True)
        path = os.path.join(directory, name or (sid + ".jsonl"))
        with open(path, "w", encoding="utf-8") as fh:
            for entry in entries:
                fh.write(json.dumps(entry) + "\n")
            for line in raw_lines or ():
                fh.write(line + "\n")
        return path

    def notification(self, sid, path, remove=(), **over):
        data = {
            "session_id": sid, "transcript_path": path, "cwd": self.proj,
            "hook_event_name": "Notification", "message": "Claude needs your permission to use Bash",
            "title": "Permission needed", "notification_type": "permission_prompt",
            "permission_mode": "default",
        }
        data.update(over)
        for key in remove:
            data.pop(key, None)
        return data

    def pending_bash(self, command, sid=SID):
        return self.write_transcript(sid, [user_text(sid), tool_use(sid, "toolu_01", "Bash", {"command": command})])

    # runs ----------------------------------------------------------------------------
    def run_hook(self, payload, argv=None, observer=None, env_extra=None):
        observer = observer or self.observer_copy
        if argv is None:
            argv = ["--expect-sha256", sha256_file(observer)]
        data = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        return subprocess.run([sys.executable, "-I", observer] + argv, input=data, capture_output=True,
                              env=self.env(env_extra), cwd=self.tmp, timeout=TIMEOUT)

    def write_command(self, command, raw=None):
        self._cmd_n += 1
        path = os.path.join(self.cmd_dir, f"{self._cmd_n:04d}.cmd")
        with open(path, "wb") as fh:
            fh.write(raw if raw is not None else command.encode("utf-8"))
        return path

    def run_explain(self, command=None, extra=(), path=None, observer=None, env_extra=None, test_root=True):
        if path is None:
            path = self.write_command(command)
        args = [sys.executable, "-I", observer or OBSERVER, "explain", "--command-file", path,
                "--project", self.proj]
        if test_root:
            args += ["--test-root", self.tmp]
        args += list(extra)
        return subprocess.run(args, capture_output=True, env=self.env(env_extra), cwd=self.tmp, timeout=TIMEOUT)

    def run_guard(self, command, mode="auto", subagent=False):
        payload = {"session_id": "explain", "hook_event_name": "PermissionRequest", "tool_name": "Bash",
                   "tool_input": {"command": command}, "cwd": self.proj, "permission_mode": mode}
        if subagent:
            payload["agent_id"] = "explain-subagent"
        return subprocess.run([sys.executable, "-I", self.guard_path, "--expect-sha256", self.guard_sha],
                              input=json.dumps(payload).encode(), capture_output=True, env=self.env(),
                              cwd=self.tmp, timeout=TIMEOUT)

    def log_lines(self):
        if not os.path.exists(self.log):
            return []
        with open(self.log, "rb") as fh:
            return fh.read().decode("utf-8").splitlines()

    def canary(self):
        sleep = self.sleep_go
        return {
            "C01": "gh --version",
            "C02": "git config --get user.email",
            "C03": f"git -C {self.proj} config --get user.email",
            "C04": f"grep -c func {sleep}",
            "C05": f"wc -l {sleep}",
            "C06": f"head -n 3 {sleep}",
            "C07": f"sha256sum {sleep}",
            "C08": f"sed -n 1,3p {sleep}",
            "C09": f"git -C {self.proj} grep -c func -- internal",
        }

    def shape_a(self, head=BRANCH):
        return (f"gh pr create --draft --repo {REPO} --base release/0.3 --head {head} "
                f"--title 'feat(guard): prompt observer' --body-file {self.body}")

    def shape_b(self, text, n=PR):
        return f"gh pr comment {n} --repo {REPO} --body {shlex.quote(text)}"

    def shape_c(self, n=PR):
        return f"gh api --hostname github.com -X PATCH {API_PULLS}/{n} -F body=@{self.body}"


def user_text(sid):
    return {"type": "user", "sessionId": sid, "message": {"role": "user", "content": "please continue"}}


def tool_use(sid, tid, name, tool_input):
    entry = {"type": "assistant", "message": {"role": "assistant", "content": [
        {"type": "tool_use", "id": tid, "name": name, "input": tool_input}]}}
    if sid is not None:
        entry["sessionId"] = sid
    return entry


def tool_result(sid, tid):
    entry = {"type": "user", "message": {"role": "user", "content": [
        {"type": "tool_result", "tool_use_id": tid, "content": "done"}]}}
    if sid is not None:
        entry["sessionId"] = sid
    return entry


class WorldCase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.w = World(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def assert_quiet(self, proc):
        """TO-5 (f): the hook prints nothing and exits 0 on every path."""
        self.assertEqual(proc.returncode, 0)
        self.assertEqual(proc.stdout, b"")
        self.assertEqual(proc.stderr, b"")

    def hook_once(self, payload, **kw):
        before = len(self.w.log_lines())
        proc = self.w.run_hook(payload, **kw)
        self.assert_quiet(proc)
        lines = self.w.log_lines()
        return lines[before:]

    def one_record(self, payload, **kw):
        new = self.hook_once(payload, **kw)
        self.assertEqual(len(new), 1, new)
        return json.loads(new[0])


# --- TO-1 shape table --------------------------------------------------------------------------

GIT_HOME = "/home/claude/bukerov-twitch-miner-go"
GOSLEEP = "/usr/local/go/src/time/sleep.go"
SEVENTEEN = " ; ".join(["true"] * 17)

TO1_CASES = [
    ("C01", "gh --version", shp([seg("gh", [], ["--version"], 0)])),
    ("C02", "git config --get user.email", shp([seg("git", ["config"], ["--get"], 1)])),
    ("C03", f"git -C {GIT_HOME} config --get user.email", shp([seg("git", ["config"], ["-C", "--get"], 2)])),
    ("C04", f"grep -c func {GOSLEEP}", shp([seg("grep", [], ["-c"], 2)])),
    ("C05", f"wc -l {GOSLEEP}", shp([seg("wc", [], ["-l"], 1)])),
    ("C06", f"head -n 3 {GOSLEEP}", shp([seg("head", [], ["-n"], 2)])),
    ("C07", f"sha256sum {GOSLEEP}", shp([seg("sha256sum", [], [], 1)])),
    ("C08", f"sed -n 1,3p {GOSLEEP}", shp([seg("sed", [], ["-n"], 2)])),
    ("C09", f"git -C {GIT_HOME} grep -c func -- internal", shp([seg("git", ["grep"], ["-C", "-c"], 3)])),
    ("A", f"gh pr create --draft --repo {REPO} --base release/0.3 --head chore/guard-g1-observer "
          f"--title \"feat(guard): prompt observer and guard explain CLI (G1)\" "
          f"--body-file {GIT_HOME}/.scratch/g1-x/PR-BODY.md",
     shp([seg("gh", ["pr", "create"], ["--draft", "--repo", "--base", "--head", "--title", "--body-file"], 5)])),
    ("B-T1", f"gh pr comment 431 --repo {REPO} --body \"{T1}\"",
     shp([seg("gh", ["pr", "comment"], ["--repo", "--body"], 3)])),
    ("B-T2", f"gh pr comment 431 --repo {REPO} --body \"{T2}\"",
     shp([seg("gh", ["pr", "comment"], ["--repo", "--body"], 3)])),
    ("B-T3", f"gh pr comment 431 --repo {REPO} --body \"{T3}\"",
     shp([seg("gh", ["pr", "comment"], ["--repo", "--body"], 3)])),
    ("C-PATCH", f"gh api --hostname github.com -X PATCH {API_PULLS}/431 -F body=@{GIT_HOME}/.scratch/x/PR-BODY.md",
     shp([seg("gh", ["api"], ["--hostname", "-X", "-F"], 4)])),
    ("title-draft", "gh pr create --title --draft", shp([seg("gh", ["pr", "create"], ["--title"], 1)])),
    ("title-dash-value", "gh pr create --title -ghp_x", shp([seg("gh", ["pr", "create"], ["--title"], 1)])),
    ("m-amend", "git commit -m --amend", shp([seg("git", ["commit"], ["-m"], 1)])),
    ("unknown-program-flags", "frobnicate --secret=hunter2 -x value", shp([other(2, 1)])),
    ("program-path", "/tmp/privateabc778899 --help", shp([other(1, 0)])),
    ("program-abs-git", "/usr/bin/git status", shp([seg("git", ["status"], [], 0)])),
    ("and", "git status && git log --oneline",
     shp([seg("git", ["status"]), seg("git", ["log"], ["--oneline"])], ["&&"])),
    ("pipe", "git log | head -n 5", shp([seg("git", ["log"]), seg("head", [], ["-n"], 1)], ["|"])),
    ("semicolon", "ls -la ; pwd", shp([seg("ls", [], ["-?"]), seg("pwd")], [";"])),
    ("redirect", "git diff > /tmp/out.txt", shp([seg("git", ["diff"], [], 1)], [">"])),
    ("redirect-2>&1", "make lint > log.txt 2>&1", shp([seg("make", [], [], 4)], [">", ">&"])),
    ("assignment", "GIT_OPTIONAL_LOCKS=0 git -C /x status --porcelain",
     shp([seg("git", ["status"], ["-C", "--porcelain"], 1, assign=1)])),
    ("two-assignments", "A=1 B=2 make test", shp([seg("make", [], [], 1, assign=2)])),
    ("assignment-only", "FOO=bar", shp([other(0, 0, assign=1)])),
    ("flag=value", "git log --format=%H --max-count=5", shp([seg("git", ["log"], ["--format", "--max-count"], 0)])),
    ("short-cluster", "grep -rn pattern internal", shp([seg("grep", [], ["-?"], 2)])),
    ("numeric-short", "head -20 file.txt", shp([seg("head", [], ["-N"], 1)])),
    ("single-digit-short", "tail -5 f", shp([seg("tail", [], ["-5"], 1)])),
    ("bare-dashdash", "git log -- --not-a-flag path", shp([seg("git", ["log"], [], 2)])),
    ("git-globals", "git --no-pager -c core.pager=cat --git-dir=/x/.git log -n 3",
     shp([seg("git", ["log"], ["--no-pager", "-c", "--git-dir", "-n"], 2)])),
    ("gh-two-level", "gh run view 12345 --log", shp([seg("gh", ["run", "view"], ["-?"], 1)])),
    ("gh-unknown-second", "gh pr frobnicate 1", shp([seg("gh", ["pr", "?"], [], 1)])),
    ("gh-unknown-top", "gh xyzzy", shp([seg("gh", ["?"], [], 0)])),
    ("git-unknown-sub", "git frobnicate x", shp([seg("git", ["?"], [], 1)])),
    ("unknown-flag", "git status --frobnicate", shp([seg("git", ["status"], ["-?"], 0)])),
    ("go-test", "go test -race -count=1 ./...", shp([seg("go", ["test"], ["-?", "-?"], 1)])),
    ("unparsable", 'echo "unterminated', shp([], unparsed=True)),
    ("dollar", "echo $HOME", shp([seg("echo", [], [], 1)], expansion=True)),
    ("backtick", "echo `whoami`", shp([seg("echo", [], [], 1)], expansion=True)),
    ("subshell", "(cd /x && ls)", shp([seg("cd", [], [], 1), seg("ls")], ["(", "&&", ")"])),
    ("stdin-dash", "cat -", shp([seg("cat", [], [], 1)])),
    ("python", "python3 -I script.py --expect-sha256 abc",
     shp([seg("python3", [], ["-I", "--expect-sha256"], 2)])),
    ("more-segments", SEVENTEEN, shp([seg("true")] * 16, [";"] * 16, more=1)),
]


class TestShape(unittest.TestCase):
    def test_TO_1_shape_table(self):
        self.assertGreaterEqual(len(TO1_CASES), 34)
        for name, command, expected in TO1_CASES:
            with self.subTest(case=name):
                self.assertEqual(obs().shape(command), expected)

    def test_TO_1_shape_is_deterministic(self):
        for _, command, _ in TO1_CASES:
            self.assertEqual(json.dumps(obs().shape(command), sort_keys=True),
                             json.dumps(obs().shape(command), sort_keys=True))

    def test_TO_3_flag_mapping(self):
        cases = [
            ("-Z", "-Z"), ("-x", "-x"), ("-9", "-9"), ("-123", "-N"),
            ("--TITLE", "-?"), ("--Draft", "-?"), ("--body_file", "-?"), ("--" + "a" * 100, "-?"),
            ("--frobnicate", "-?"), ("--frobnicate=1", "-?"), ("-abc", "-?"), ("-rn", "-?"), ("-é", "-?"),
            ("--draft", "--draft"), ("--body-file=x", "--body-file"), ("--porcelain", "--porcelain"),
            ("--no-textconv=false", "--no-textconv"), ("--max-count=3", "--max-count"),
        ]
        for token, expected in cases:
            with self.subTest(token=token):
                got = obs().shape("git status " + shlex.quote(token))
                self.assertEqual(got["segments"][0]["flags"], [expected])


# --- TO-2 value-leak property ----------------------------------------------------------------------

VALUE_CHARS = string.ascii_letters + string.digits + "_./@:%+=-"
FIRST_CHARS = VALUE_CHARS.replace("-", "")
PROGRAM_CHARS = string.ascii_letters + string.digits + "_.@:%+-"


def rv(rng, first=FIRST_CHARS, chars=VALUE_CHARS):
    n = rng.randint(8, 40)
    return rng.choice(first) + "".join(rng.choice(chars) for _ in range(n - 1))


def rprog(rng):
    n = rng.randint(8, 40)
    return "." + "".join(rng.choice(PROGRAM_CHARS) for _ in range(n - 1))


def _t00(r):
    v = [rv(r) for _ in range(5)]
    return (f"gh pr create --draft --repo {v[0]} --base {v[1]} --head {v[2]} --title {v[3]} --body-file {v[4]}",
            v, shp([seg("gh", ["pr", "create"], ["--draft", "--repo", "--base", "--head", "--title", "--body-file"], 5)]))


def _t01(r):
    v = [rv(r) for _ in range(3)]
    return (f"gh pr comment {v[0]} --repo {v[1]} --body {v[2]}", v,
            shp([seg("gh", ["pr", "comment"], ["--repo", "--body"], 3)]))


def _t02(r):
    v = [rv(r) for _ in range(4)]
    return (f"gh api --hostname {v[0]} -X PATCH repos/{v[1]}/pulls/{v[2]} -F body=@{v[3]}", v,
            shp([seg("gh", ["api"], ["--hostname", "-X", "-F"], 4)]))


def _t03(r):
    v = [rv(r) for _ in range(3)]
    return (f"git -C {v[0]} -c {v[1]} config --get {v[2]}", v,
            shp([seg("git", ["config"], ["-C", "-c", "--get"], 3)]))


def _t04(r):
    name = "ZZ_" + "".join(r.choice(string.ascii_letters + string.digits) for _ in range(r.randint(6, 12)))
    v = [rv(r) for _ in range(3)]
    return (f"{name}={v[0]} git log --format={v[1]} -- {v[2]}", v + [name],
            shp([seg("git", ["log"], ["--format"], 1, assign=1)]))


def _t05(r):
    v = [rv(r) for _ in range(4)]
    return (f"grep -e {v[0]} -m {v[1]} {v[2]} > {v[3]}", v, shp([seg("grep", [], ["-e", "-m"], 4)], [">"]))


def _t06(r):
    p = rprog(r)
    name = "".join(r.choice(string.ascii_lowercase + string.digits) for _ in range(r.randint(8, 20)))
    v = [rv(r) for _ in range(2)]
    return (f"{p} --{name}={v[0]} {v[1]}", v + [p, name], shp([other(1, 1)]))


def _t07(r):
    p = rprog(r)[1:] + "x"
    v = rv(r)
    return (f"/tmp/{p} --help {v}", [p, v], shp([other(1, 1)]))


def _t08(r):
    v = [rv(r) for _ in range(3)]
    return (f"cat -- {v[0]} --{v[1]} -{v[2]}", v, shp([seg("cat", [], [], 3)]))


def _t09(r):
    v = "-ghp_" + rv(r, chars=string.ascii_letters + string.digits)
    return (f"git commit -m {v} --amend", [v], shp([seg("git", ["commit"], ["-m", "--amend"], 1)]))


def _t10(r):
    dd = "--" + "".join(r.choice("0123456789abcdef") for _ in range(r.randint(8, 30)))
    v = rv(r)
    return (f"gh pr create --title {dd} --body {v}", [dd, v],
            shp([seg("gh", ["pr", "create"], ["--title", "--body"], 2)]))


def _t11(r):
    v = [rv(r) for _ in range(3)]
    return (f"sed -n {v[0]} {v[1]} | head -n {v[2]}", v,
            shp([seg("sed", [], ["-n"], 2), seg("head", [], ["-n"], 1)], ["|"]))


def _t12(r):
    v = [rv(r) for _ in range(3)]
    return (f"go test -run {v[0]} -count {v[1]} ./{v[2]}", v, shp([seg("go", ["test"], ["-?", "-?"], 3)]))


def _t13(r):
    v = [rv(r) for _ in range(3)]
    return (f"gh pr view {v[0]} --json {v[1]} --jq {v[2]}", v,
            shp([seg("gh", ["pr", "view"], ["--json", "--jq"], 3)]))


def _t14(r):
    v = rv(r)
    return (f"git push origin {v}", [v], shp([seg("git", ["push"], [], 2)]))


def _t15(r):
    v = [rv(r) for _ in range(3)]
    return (f"echo {v[0]} >> {v[1]} ; ls {v[2]}", v,
            shp([seg("echo", [], [], 2), seg("ls", [], [], 1)], [">>", ";"]))


def _t16(r):
    v = [rv(r) for _ in range(3)]
    return (f"docker run --env {v[0]}={v[1]} {v[2]}", v, shp([seg("docker", [], ["-?"], 3)]))


def _t17(r):
    v = [rv(r) for _ in range(4)]
    return (f"curl -H {v[0]} -F {v[1]} -X {v[2]} {v[3]}", v, shp([seg("curl", [], ["-H", "-F", "-X"], 4)]))


def _t18(r):
    p = rprog(r)
    return (p, [p], shp([other(0, 0)]))


def _t19(r):
    v = [rv(r) for _ in range(2)]
    return (f"xargs -I {v[0]} rm {v[1]}", v, shp([seg("xargs", [], ["-I"], 3)]))


TEMPLATES = [_t00, _t01, _t02, _t03, _t04, _t05, _t06, _t07, _t08, _t09,
             _t10, _t11, _t12, _t13, _t14, _t15, _t16, _t17, _t18, _t19]


def generate_corpus(seed=20261017, count=500):
    rng = random.Random(seed)
    corpus = []
    sids = set()
    for i in range(count):
        command, values, expected = TEMPLATES[i % len(TEMPLATES)](rng)
        while True:
            sid = "%08x-%04x-4%03x-8%03x-%012x" % (rng.getrandbits(32), rng.getrandbits(16), rng.getrandbits(12),
                                                    rng.getrandbits(12), rng.getrandbits(48))
            if sid[:8] not in sids:
                sids.add(sid[:8])
                break
        corpus.append((command, values, expected, sid))
    usage = []
    for _ in range(20):
        usage.append(rv(rng))
    return corpus, usage


class TestValueLeak(WorldCase):
    def check_emitted(self, raw_text, values, allowed, extra, where):
        for value in values:
            self.assertGreaterEqual(len(value), 6)
            self.assertNotIn(value, raw_text, f"{where}: a value leaked")
        if raw_text.strip():
            for line in raw_text.splitlines():
                for text in strings_in(json.loads(line), []):
                    self.assertTrue(is_allowed(text, allowed, extra), f"{where}: {text!r} is not a constant")

    def test_TO_2_value_leak_property(self):
        corpus, usage_values = generate_corpus()
        self.assertEqual(len(corpus), 500)
        self.assertGreaterEqual(len(TEMPLATES), 10)
        allowed = allowed_strings()
        extra = {SID[:8], self.w.guard_sha} | {c[3][:8] for c in corpus}
        # every template's shape is invariant under value substitution
        for command, values, expected, _ in corpus:
            self.assertEqual(obs().shape(command), expected, command)

        def hook(item):
            command, values, expected, sid = item
            path = self.w.write_transcript(sid, [user_text(sid), tool_use(sid, "toolu_x", "Bash",
                                                                            {"command": command})])
            return self.w.run_hook(self.w.notification(sid, path))

        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            procs = list(pool.map(hook, corpus))
        for proc in procs:
            self.assert_quiet(proc)
        lines = self.w.log_lines()
        self.assertEqual(len(lines), 500)
        by_session = {json.loads(line)["session"]: line for line in lines}
        self.assertEqual(len(by_session), 500)
        for command, values, expected, sid in corpus:
            line = by_session[sid[:8]]
            self.check_emitted(line, values, allowed, extra, "hook")
            record = json.loads(line)
            self.assertEqual(record["source"], "pending_tool_use")
            self.assertEqual(record["shape"], expected)

        commands = [(c[0], self.w.write_command(c[0])) for c in corpus]

        def explain(item):
            return self.w.run_explain(path=item[1])

        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            results = list(pool.map(explain, commands))
        for (command, values, expected, _), proc in zip(corpus, results):
            self.assertEqual(proc.returncode, 0)
            self.assertEqual(proc.stderr, b"")
            out = proc.stdout.decode("utf-8")
            self.assertEqual(len(out.splitlines()), 1)
            self.check_emitted(out, values, allowed, extra, "explain stdout")
            self.assertEqual(json.loads(out)["shape"], expected)

        # usage-error runs carry values in their options; stderr is one fixed line
        stderr_lines = set()
        for value in usage_values:
            path = self.w.write_command("git status")
            for extra_args in (["--permission-mode", value], ["--" + value.lstrip("-")],
                               ["--cwd"], ["--project", os.path.join(self.w.tmp, value)]):
                if extra_args[0] == "--project":
                    proc = subprocess.run([sys.executable, "-I", OBSERVER, "explain", "--command-file", path]
                                          + extra_args, capture_output=True, env=self.w.env(), cwd=self.w.tmp,
                                          timeout=TIMEOUT)
                else:
                    proc = self.w.run_explain(path=path, extra=extra_args)
                self.assertEqual(proc.returncode, 2, extra_args)
                self.assertEqual(proc.stdout, b"")
                err = proc.stderr.decode("utf-8")
                self.assertNotIn(value, err)
                stderr_lines.add(err)
            missing = os.path.join(self.w.tmp, value + ".cmd")
            proc = self.w.run_explain(path=missing)
            self.assertEqual(proc.returncode, 2)
            self.assertNotIn(value, proc.stderr.decode("utf-8"))
            stderr_lines.add(proc.stderr.decode("utf-8"))
        self.assertEqual(len(stderr_lines), 1)
        self.assertEqual(len(next(iter(stderr_lines)).splitlines()), 1)


# --- TO-4 transcript ---------------------------------------------------------------------------------

class TestTranscript(WorldCase):
    def record(self, sid, path, **over):
        return obs().build_record(self.w.notification(sid, path, **over), self.w.proj, self.w.home, now=0)

    def assert_unreadable(self, rec, reason):
        self.assertEqual(rec["source"], "transcript_unreadable")
        self.assertEqual(rec["reason"], reason)
        self.assertEqual(rec["tool"], "OTHER")
        self.assertIsNone(rec["shape"])
        self.assertEqual(rec["guard"], "NOT_EVALUATED")
        self.assertEqual(rec["pending"], 0)

    def test_TO_4_one_pending_found(self):
        path = self.w.pending_bash("git status --porcelain")
        rec = self.record(SID, path)
        self.assertEqual(rec["source"], "pending_tool_use")
        self.assertEqual(rec["pending"], 1)
        self.assertEqual(rec["tool"], "Bash")
        self.assertEqual(rec["shape"], obs().shape("git status --porcelain"))
        self.assertEqual(rec["ts"], "1970-01-01T00:00:00Z")
        self.assertEqual(rec["session"], SID[:8])
        self.assertEqual(rec["v"], 1)
        self.assertEqual(rec["event"], "permission_prompt")
        self.assertEqual(rec["mode"], "given")

    def test_TO_4_record_never_stores_message_or_title(self):
        path = self.w.pending_bash("git status")
        rec = self.record(SID, path, message="SECRETMESSAGE77", title="SECRETTITLE88")
        text = json.dumps(rec)
        self.assertNotIn("SECRETMESSAGE77", text)
        self.assertNotIn("SECRETTITLE88", text)
        self.assertEqual(set(rec), {"v", "ts", "event", "session", "source", "pending", "tool", "shape",
                                    "guard", "reason", "mode"})

    def test_TO_4_resolved_ignored(self):
        path = self.w.write_transcript(SID, [tool_use(SID, "t1", "Bash", {"command": "ls"}), tool_result(SID, "t1")])
        rec = self.record(SID, path)
        self.assertEqual(rec["source"], "no_pending_tool_use")
        self.assertEqual(rec["pending"], 0)
        self.assertIsNone(rec["shape"])
        self.assertEqual(rec["tool"], "OTHER")

    def test_TO_4_mixed_resolved_and_one_pending(self):
        path = self.w.write_transcript(SID, [
            tool_use(SID, "t1", "Bash", {"command": "git log"}), tool_result(SID, "t1"),
            tool_use(SID, "t2", "Bash", {"command": "ls -la"}),
            tool_use(SID, "t3", "Read", {"file_path": "/x"}), tool_result(SID, "t3")])
        rec = self.record(SID, path)
        self.assertEqual(rec["source"], "pending_tool_use")
        self.assertEqual(rec["shape"], obs().shape("ls -la"))

    def test_TO_4_two_pending_ambiguous(self):
        path = self.w.write_transcript(SID, [
            tool_use(SID, "t1", "Bash", {"command": "cat /secretone/abcdef"}),
            tool_use(SID, "t2", "Bash", {"command": "git push origin secretbranch77"})])
        rec = self.record(SID, path)
        self.assertEqual(rec["source"], "ambiguous")
        self.assertEqual(rec["pending"], 2)
        self.assertIsNone(rec["shape"])
        self.assertEqual(rec["tool"], "OTHER")
        self.assertEqual(rec["guard"], "NOT_EVALUATED")
        self.assertEqual(rec["reason"], "ambiguous")
        self.assertNotIn("Bash", json.dumps(rec))

    def test_TO_4_three_pending_ambiguous(self):
        path = self.w.write_transcript(SID, [
            tool_use(SID, "t1", "Bash", {"command": "ls"}), tool_use(SID, "t2", "Edit", {"file_path": "/a"}),
            tool_use(SID, "t3", "Bash", {"command": "pwd"})])
        rec = self.record(SID, path)
        self.assertEqual(rec["source"], "ambiguous")
        self.assertEqual(rec["pending"], 3)
        self.assertIsNone(rec["shape"])

    def test_TO_4_partial_first_line_after_tail_seek(self):
        # The tail window starts exactly where a valid assistant object begins inside a longer,
        # invalid line; reading that fragment would add a second pending call.
        inner = json.dumps(tool_use(SID, "tfrag", "Bash", {"command": "rm -rf fragment"}))
        last = json.dumps(tool_use(SID, "treal", "Bash", {"command": "git status"})) + "\n"
        pad_entry = user_text(SID)
        pad_entry["message"]["content"] = ""
        pad_base = json.dumps(pad_entry) + "\n"
        pad_len = TAIL - len(inner) - 1 - len(last) - len(pad_base)
        self.assertGreater(pad_len, 0)
        pad_entry["message"]["content"] = "p" * pad_len
        pad = json.dumps(pad_entry) + "\n"
        tail = inner + "\n" + pad + last
        self.assertEqual(len(tail.encode()), TAIL)
        path = os.path.join(self.w.tdir, SID + ".jsonl")
        with open(path, "w") as fh:
            fh.write(json.dumps(user_text(SID)) + "\n")
            fh.write("garbage-prefix-" * 10 + tail)
        rec = self.record(SID, path)
        self.assertEqual(rec["source"], "pending_tool_use")
        self.assertEqual(rec["shape"], obs().shape("git status"))

    def test_TO_4_truncated_tail_without_assistant(self):
        path = os.path.join(self.w.tdir, SID + ".jsonl")
        with open(path, "w") as fh:
            fh.write(json.dumps(tool_use(SID, "t1", "Bash", {"command": "ls"})) + "\n")
            entry = user_text(SID)
            entry["message"]["content"] = "q" * 4096
            line = json.dumps(entry) + "\n"
            for _ in range(TAIL // len(line) + 8):
                fh.write(line)
        self.assert_unreadable(self.record(SID, path), "truncated")

    def test_TO_4_corrupt_lines_skipped(self):
        path = self.w.write_transcript(SID, [], raw_lines=[
            "not json", "[1, 2, 3]", '{"type": ', '"a string"',
            json.dumps(tool_use(SID, "t1", "Bash", {"command": "git diff"}))])
        rec = self.record(SID, path)
        self.assertEqual(rec["source"], "pending_tool_use")
        self.assertEqual(rec["shape"], obs().shape("git diff"))

    def test_TO_4_non_assistant_entries_ignored(self):
        system = tool_use(SID, "t0", "Bash", {"command": "ls"})
        system["type"] = "system"
        as_user = tool_use(SID, "t9", "Bash", {"command": "pwd"})
        as_user["type"] = "user"
        path = self.w.write_transcript(SID, [system, as_user, tool_use(SID, "t1", "Bash", {"command": "git log"})])
        rec = self.record(SID, path)
        self.assertEqual(rec["source"], "pending_tool_use")
        self.assertEqual(rec["shape"], obs().shape("git log"))

    def test_TO_4_foreign_session_entries_ignored(self):
        path = self.w.write_transcript(SID, [
            tool_use(SID_B, "tb", "Bash", {"command": "cat /foreign/secretfile"}),
            tool_use(SID, "t1", "Bash", {"command": "git status"})])
        rec = self.record(SID, path)
        self.assertEqual(rec["source"], "pending_tool_use")
        self.assertEqual(rec["shape"], obs().shape("git status"))

    def test_TO_4_two_session_substitution(self):
        for with_session_field in (True, False):
            with self.subTest(with_session_field=with_session_field):
                sid = SID_B if with_session_field else None
                path_b = self.w.write_transcript(SID_B, [tool_use(sid, "tb", "Bash",
                                                                  {"command": "cat /secretB/valueBBBB"})])
                self.w.pending_bash("git status")  # session A's own transcript exists too
                rec = self.record(SID, path_b)
                self.assert_unreadable(rec, "binding")
                self.assertNotIn("secretB", json.dumps(rec))

    def test_TO_4_binding_refusals(self):
        entries = [tool_use(SID, "t1", "Bash", {"command": "git status"})]
        outside = os.path.join(self.w.tmp, "elsewhere")
        cases = {
            "wrong name": self.w.write_transcript(SID, entries, name=SID + ".json"),
            "depth one": self.w.write_transcript(SID, entries, directory=self.w.projects),
            "depth three": self.w.write_transcript(SID, entries, directory=os.path.join(self.w.tdir, "deeper")),
            "outside root": self.w.write_transcript(SID, entries, directory=outside),
        }
        for name, path in cases.items():
            with self.subTest(case=name):
                self.assert_unreadable(self.record(SID, path), "binding")
        real = self.w.write_transcript(SID, entries, directory=os.path.join(self.w.tdir, "..", "-other"))
        link = os.path.join(self.w.tdir, SID + ".jsonl")
        os.symlink(real, link)
        with self.subTest(case="symlinked"):
            self.assert_unreadable(self.record(SID, link), "binding")
        with self.subTest(case="not a uuid"):
            path = self.w.write_transcript("not-a-uuid", entries)
            self.assert_unreadable(self.record("not-a-uuid", path), "binding")

    def test_TO_4_missing_file(self):
        rec = self.record(SID, os.path.join(self.w.tdir, SID + ".jsonl"))
        self.assertEqual(rec["source"], "transcript_unreadable")
        self.assertIsNone(rec["shape"])
        self.assertEqual(rec["guard"], "NOT_EVALUATED")


# --- TO-5 hook end to end ----------------------------------------------------------------------------

class TestHook(WorldCase):
    def test_TO_5_a_config_form(self):
        rec = self.one_record(self.w.notification(SID, self.w.pending_bash("git config --get core.bare")))
        self.assertEqual(rec["guard"], "NO_DECISION")
        self.assertEqual(rec["reason"], "config form")
        self.assertEqual(rec["shape"], obs().shape("git config --get core.bare"))

    def test_TO_5_b_allow(self):
        rec = self.one_record(self.w.notification(SID, self.w.pending_bash("gh --version")))
        self.assertEqual(rec["guard"], "ALLOW")
        self.assertIsNone(rec["reason"])

    def test_TO_5_c_idle_prompt_ignored(self):
        payload = self.w.notification(SID, self.w.pending_bash("gh --version"), notification_type="idle_prompt")
        self.assertEqual(self.hook_once(payload), [])

    def test_TO_5_d_other_event_ignored(self):
        payload = self.w.notification(SID, self.w.pending_bash("gh --version"), hook_event_name="PermissionRequest")
        self.assertEqual(self.hook_once(payload), [])

    def test_TO_5_e_wrong_self_hash(self):
        payload = self.w.notification(SID, self.w.pending_bash("gh --version"))
        self.assertEqual(self.hook_once(payload, argv=["--expect-sha256", "0" * 64]), [])
        self.assertEqual(self.hook_once(payload, argv=["--expect-sha256"]), [])
        self.assertEqual(self.hook_once(payload, argv=[]), [])

    def test_TO_5_f_quiet_on_every_path(self):
        payloads = [self.w.notification(SID, self.w.pending_bash("gh --version")), b"{bad", b"",
                    json.dumps([1]).encode(), self.w.notification(SID, "/nonexistent/x.jsonl")]
        for payload in payloads:
            self.assert_quiet(self.w.run_hook(payload))
        self.assertEqual(len(self.w.log_lines()), 2)  # the valid one and the unreadable-transcript one
        sha = sha256_file(self.w.observer_copy)
        for argv in (["--expect-sha256", "0" * 64], ["--expect-sha256", sha, "extra"],
                     ["--test-root", self.w.tmp], ["--expect-sha256", sha, "--test-root", self.w.tmp]):
            self.assert_quiet(self.w.run_hook(payloads[0], argv=argv))
        self.assertEqual(len(self.w.log_lines()), 2)

    def test_TO_5_g_symlinked_log(self):
        os.mkdir(self.w.log_dir, 0o700)
        target = os.path.join(self.w.tmp, "target.jsonl")
        with open(target, "wb") as fh:
            fh.write(b"sentinel\n")
        os.chmod(target, 0o600)
        os.symlink(target, self.w.log)
        self.hook_once(self.w.notification(SID, self.w.pending_bash("gh --version")))
        with open(target, "rb") as fh:
            self.assertEqual(fh.read(), b"sentinel\n")
        self.assertTrue(os.path.islink(self.w.log))

    def test_TO_5_h_hardlinked_log(self):
        os.mkdir(self.w.log_dir, 0o700)
        other_path = os.path.join(self.w.tmp, "other.jsonl")
        with open(other_path, "wb") as fh:
            fh.write(b"sentinel\n")
        os.chmod(other_path, 0o600)
        os.link(other_path, self.w.log)
        self.assert_quiet(self.w.run_hook(self.w.notification(SID, self.w.pending_bash("gh --version"))))
        with open(other_path, "rb") as fh:
            self.assertEqual(fh.read(), b"sentinel\n")

    def test_TO_5_i_symlinked_directory(self):
        real_dir = os.path.join(self.w.tmp, "realdir")
        os.mkdir(real_dir, 0o700)
        os.symlink(real_dir, self.w.log_dir)
        self.assert_quiet(self.w.run_hook(self.w.notification(SID, self.w.pending_bash("gh --version"))))
        self.assertEqual(os.listdir(real_dir), [])

    def test_TO_5_j_size_cap(self):
        os.mkdir(self.w.log_dir, 0o700)
        data = b"x" * (4 * 1024 * 1024) + b"\n"
        fd = os.open(self.w.log, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        os.write(fd, data)
        os.close(fd)
        self.assert_quiet(self.w.run_hook(self.w.notification(SID, self.w.pending_bash("gh --version"))))
        self.assertEqual(sha256_file(self.w.log), sha256_bytes(data))

    def test_TO_5_k_malformed_stdin(self):
        for raw in (b"{not json", b'{"a": 1, "a": 2}', b"\xff\xfe", b"[]"):
            with self.subTest(raw=raw):
                self.assertEqual(self.hook_once(raw), [])

    def test_TO_5_l_no_registration(self):
        self.w.write_settings({"hooks": {}})
        rec = self.one_record(self.w.notification(SID, self.w.pending_bash("gh --version")))
        self.assertEqual(rec["guard"], "NOT_EVALUATED")
        self.assertEqual(rec["reason"], "no_registration")

    def test_TO_5_m_guard_altered(self):
        altered = bytearray(self.w.guard_bytes)
        altered[-2] = ord("#") if altered[-2] != ord("#") else ord(" ")
        os.chmod(self.w.guard_path, 0o644)
        with open(self.w.guard_path, "wb") as fh:
            fh.write(bytes(altered))
        os.chmod(self.w.guard_path, 0o444)
        rec = self.one_record(self.w.notification(SID, self.w.pending_bash("gh --version")))
        self.assertEqual(rec["guard"], "NOT_EVALUATED")
        self.assertEqual(rec["reason"], "guard_hash")

    def test_TO_5_n_edit_tool(self):
        secret = "/tmp/secretpathxyz/notes-abc.txt"
        path = self.w.write_transcript(SID, [tool_use(SID, "t1", "Edit", {"file_path": secret,
                                                                          "old_string": "a", "new_string": "b"})])
        new = self.hook_once(self.w.notification(SID, path))
        self.assertEqual(len(new), 1)
        rec = json.loads(new[0])
        self.assertEqual(rec["tool"], "Edit")
        self.assertIsNone(rec["shape"])
        self.assertEqual(rec["guard"], "NOT_EVALUATED")
        self.assertEqual(rec["reason"], "not_bash")
        self.assertNotIn("secretpathxyz", new[0])
        self.assertNotIn("notes-abc", new[0])

    def test_TO_5_o_new_log_modes(self):
        self.one_record(self.w.notification(SID, self.w.pending_bash("gh --version")))
        self.assertEqual(stat.S_IMODE(os.lstat(self.w.log_dir).st_mode), 0o700)
        self.assertEqual(stat.S_IMODE(os.lstat(self.w.log).st_mode), 0o600)

    def test_TO_5_p_concurrent_hooks(self):
        payload = json.dumps(self.w.notification(SID, self.w.pending_bash("gh --version"))).encode()
        observer = self.w.observer_copy
        argv = [sys.executable, "-I", observer, "--expect-sha256", sha256_file(observer)]
        procs = [subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                  env=self.w.env(), cwd=self.w.tmp) for _ in range(20)]
        for proc in procs:
            out, err = proc.communicate(payload, timeout=TIMEOUT)
            self.assertEqual((proc.returncode, out, err), (0, b"", b""))
        lines = self.w.log_lines()
        self.assertEqual(len(lines), 20)
        for line in lines:
            self.assertEqual(json.loads(line)["guard"], "ALLOW")

    def test_TO_5_q_no_bytecode(self):
        rec = self.one_record(self.w.notification(SID, self.w.pending_bash("gh --version")))
        self.assertEqual(rec["guard"], "ALLOW")
        for root in (self.w.hooks_root, self.w.obs_dir):
            for dirpath, dirnames, filenames in os.walk(root):
                self.assertNotIn("__pycache__", dirnames, dirpath)
                self.assertEqual([f for f in filenames if f.endswith(".pyc")], [], dirpath)

    def test_TO_5_r_mode_assumed(self):
        rec = self.one_record(self.w.notification(SID, self.w.pending_bash("gh --version"),
                                                  remove=("permission_mode",)))
        self.assertEqual(rec["mode"], "assumed_default")
        rec = self.one_record(self.w.notification(SID, self.w.pending_bash("gh --version"),
                                                  permission_mode="weird"))
        self.assertEqual(rec["mode"], "assumed_default")

    def test_TO_5_u_directory_0755_refused(self):
        os.mkdir(self.w.log_dir)
        os.chmod(self.w.log_dir, 0o755)
        fd = os.open(self.w.log, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        os.write(fd, b"sentinel\n")
        os.close(fd)
        self.assert_quiet(self.w.run_hook(self.w.notification(SID, self.w.pending_bash("gh --version"))))
        self.assertEqual(stat.S_IMODE(os.lstat(self.w.log_dir).st_mode), 0o755)
        with open(self.w.log, "rb") as fh:
            self.assertEqual(fh.read(), b"sentinel\n")

    def test_TO_5_v_log_0644_refused(self):
        os.mkdir(self.w.log_dir, 0o700)
        fd = os.open(self.w.log, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        os.write(fd, b"sentinel\n")
        os.close(fd)
        os.chmod(self.w.log, 0o644)
        self.assert_quiet(self.w.run_hook(self.w.notification(SID, self.w.pending_bash("gh --version"))))
        self.assertEqual(stat.S_IMODE(os.lstat(self.w.log).st_mode), 0o644)
        with open(self.w.log, "rb") as fh:
            self.assertEqual(fh.read(), b"sentinel\n")

    def test_TO_5_s_guard_outside_layout(self):
        elsewhere = os.path.join(self.w.tmp, "elsewhere")
        os.mkdir(elsewhere)
        copy = os.path.join(elsewhere, "permission_guard.py")
        with open(copy, "wb") as fh:
            fh.write(self.w.guard_bytes)
        os.chmod(copy, 0o444)
        sentinel = os.path.join(self.w.tmp, "sentinel-s")
        for module in ("hashlib.py", "json.py", "shlex.py"):
            with open(os.path.join(elsewhere, module), "w") as fh:
                fh.write(f"open({sentinel!r}, 'w').close()\n")
        self.w.register_guard(copy, self.w.guard_sha)
        rec = self.one_record(self.w.notification(SID, self.w.pending_bash("gh --version")))
        self.assertEqual(rec["guard"], "NOT_EVALUATED")
        self.assertEqual(rec["reason"], "registration_shape")
        self.assertFalse(os.path.exists(sentinel))

    def test_TO_5_t_guard_unknown_hash(self):
        sentinel = os.path.join(self.w.tmp, "sentinel-t")
        data = f"open({sentinel!r}, 'w').close()\n".encode() + self.w.guard_bytes
        sha = sha256_bytes(data)
        path = self.w.place_guard(data, sha)
        self.w.register_guard(path, sha)
        rec = self.one_record(self.w.notification(SID, self.w.pending_bash("gh --version")))
        self.assertEqual(rec["guard"], "NOT_EVALUATED")
        self.assertEqual(rec["reason"], "guard_unknown")
        self.assertFalse(os.path.exists(sentinel))

    def test_TO_5_guard_fixture_is_the_reviewed_base_guard(self):
        self.assertEqual(self.w.guard_sha, BASE_GUARD_SHA)
        self.assertIn(BASE_GUARD_SHA, obs().KNOWN_GUARD_SHA256)


# --- TO-6 explain parity ---------------------------------------------------------------------------

class TestExplainParity(WorldCase):
    def corpus(self):
        w = self.w
        c = w.canary()
        sleep = w.sleep_go
        items = []
        for name, command in c.items():
            items.append((name, command, "active", True, False))
            items.append((name + "-closed", command, "closed", True, False))
        items += [
            ("A", w.shape_a(), "active", False, False),
            ("A-with-pr-recorded", w.shape_a(), "active", True, False),
            ("A-other-head", w.shape_a(head="chore/other-branch"), "active", False, False),
            ("A-subagent", w.shape_a(), "active", False, True),
            ("B-T1", w.shape_b(T1), "active", True, False),
            ("B-T2", w.shape_b(T2), "active", True, False),
            ("B-T3", w.shape_b(T3), "active", True, False),
            ("B-wrong-pr", w.shape_b(T1, n=999), "active", True, False),
            ("B-not-trigger", w.shape_b("hello there"), "active", True, False),
            ("C", w.shape_c(), "active", True, False),
            ("C-disarmed", w.shape_c(), "disarmed", True, False),
            ("C02-disarmed", c["C02"], "disarmed", True, False),
            ("gh-pr-edit", f"gh pr edit {PR} --repo {REPO} --body x", "active", True, False),
            ("gh-api-get", f"gh api {API_PULLS}/{PR}", "active", True, False),
            ("git-push", "git push origin main", "active", True, False),
            ("git-config-global", "git config --global user.name", "active", True, False),
            ("git-config-core", "git config --get core.bare", "active", True, False),
            ("grep-outside", "grep -c func /etc/hostname", "active", True, False),
            ("cat-outside", "cat /etc/passwd", "active", True, False),
            ("sed-inplace", f"sed -i s/a/b/ {sleep}", "active", True, False),
            ("ls-tilde", "ls ~", "active", True, False),
            ("rm", "rm -rf /", "active", True, False),
            ("python", "python3 -c 1", "active", True, False),
            ("dollar", "echo $HOME", "active", True, False),
            ("compound", "git status ; ls", "active", True, False),
            ("assignment", "FOO=1 git status", "active", True, False),
            ("git-dash-c", "git -c core.pager=cat log", "active", True, False),
            ("dot-git", f"head -n 3 {w.proj}/.git/config", "active", True, False),
            ("git-output", "git log --output=/tmp/x", "active", True, False),
            ("stat-L", f"stat -L {sleep}", "active", True, False),
            ("unparsable", "echo 'unterminated", "active", True, False),
        ]
        return items

    def test_TO_6_explain_matches_guard(self):
        items = self.corpus()
        self.assertGreaterEqual(len(items), 40)
        seen = set()
        for name, command, state, with_pr, subagent in items:
            with self.subTest(case=name):
                self.w.set_record(state=state, with_pr=with_pr)
                extra = ["--subagent"] if subagent else []
                proc = self.w.run_explain(command, extra=extra)
                self.assertEqual(proc.returncode, 0)
                self.assertEqual(proc.stderr, b"")
                out = json.loads(proc.stdout)
                self.assertTrue(out["test_root"])
                self.assertEqual(out["guard_sha256"], self.w.guard_sha)
                guard = self.w.run_guard(command, subagent=subagent)
                self.assertEqual(guard.returncode, 0)
                guard_allow = guard.stdout.decode() == ALLOW_JSON
                if not guard_allow:
                    self.assertEqual(guard.stdout, b"")
                self.assertEqual(out["decision"], "ALLOW" if guard_allow else "NO_DECISION")
                if out["decision"] == "NO_DECISION":
                    self.assertTrue(out["reason"])
                    self.assertEqual(obs().sanitize(out["reason"]), out["reason"])
                else:
                    self.assertIsNone(out["reason"])
                seen.add((name, out["decision"], out["reason"]))
        decisions = {d for _, d, _ in seen}
        self.assertEqual(decisions, {"ALLOW", "NO_DECISION"})
        reasons = {name: reason for name, _, reason in seen}
        for name in ("C01", "C02", "C03", "C04", "C05", "C06", "C07", "C08", "C09", "A", "B-T1", "B-T2",
                     "B-T3", "C", "C02-disarmed"):
            self.assertIsNone(reasons[name], name)
        for name in ("C01-closed", "C02-closed", "C04-closed", "C09-closed"):
            self.assertEqual(reasons[name], "record not active/disarmed")
        self.assertEqual(reasons["A-subagent"], "subagent")
        self.assertEqual(reasons["python"], "no matching shape")
        self.assertEqual(reasons["C-disarmed"], "record not active")

    def test_TO_6_explain_not_evaluated_without_registration(self):
        self.w.write_settings({})
        proc = self.w.run_explain("gh --version")
        out = json.loads(proc.stdout)
        self.assertEqual(out["decision"], "NOT_EVALUATED")
        self.assertEqual(out["reason"], "no_registration")
        self.assertIsNone(out["guard_sha256"])

    def test_TO_6_explain_without_test_root_never_uses_the_test_layout(self):
        proc = self.w.run_explain("gh --version", test_root=False)
        out = json.loads(proc.stdout)
        self.assertNotIn("test_root", out)
        self.assertEqual(out["decision"], "NOT_EVALUATED")
        self.assertEqual(out["reason"], "registration_shape")


# --- TO-7 reason literals ----------------------------------------------------------------------------

class TestReasonLiterals(unittest.TestCase):
    def test_TO_7_guard_reasons_are_constants(self):
        literals, bad = guard_reason_literals()
        self.assertEqual(bad, [])
        self.assertGreater(len(literals), 40)
        self.assertIn("record not active/disarmed", literals)
        self.assertIn("config form", literals)
        self.assertIn("check failed", literals)


# --- TO-8 explain CLI ------------------------------------------------------------------------------

class TestExplainCli(WorldCase):
    def usage_error(self, proc):
        self.assertEqual(proc.returncode, 2)
        self.assertEqual(proc.stdout, b"")
        self.assertEqual(proc.stderr.decode(), obs().EXPLAIN_USAGE_LINE + "\n")
        self.assertNotIn(self.w.tmp, proc.stderr.decode())

    def test_TO_8_bad_command_files(self):
        self.usage_error(self.w.run_explain(path=os.path.join(self.w.tmp, "missing-secretname.cmd")))
        real = self.w.write_command("git status")
        link = os.path.join(self.w.tmp, "link.cmd")
        os.symlink(real, link)
        self.usage_error(self.w.run_explain(path=link))
        self.usage_error(self.w.run_explain(path=self.w.write_command(None, raw=b"a" * 8193)))
        self.usage_error(self.w.run_explain(path=self.w.write_command(None, raw=b"git status \xff\xfe")))
        self.usage_error(self.w.run_explain(path=self.w.cmd_dir))

    def test_TO_8_limits_accepted(self):
        proc = self.w.run_explain(path=self.w.write_command(None, raw=b"a" * 8191 + b"\n"))
        self.assertEqual(proc.returncode, 0)
        proc = self.w.run_explain(path=self.w.write_command(None, raw=b"a" * 8192))
        self.assertEqual(proc.returncode, 0)

    def test_TO_8_no_guard_option(self):
        self.usage_error(self.w.run_explain("gh --version", extra=["--guard", self.w.guard_path]))
        self.usage_error(self.w.run_explain("gh --version", extra=["--guard-sha256", self.w.guard_sha]))
        self.usage_error(self.w.run_explain("gh --version", extra=["--permission-mode", "superuser"]))
        self.usage_error(self.w.run_explain("gh --version", extra=["--test-root"]))

    def test_TO_8_one_line_no_values(self):
        command = "gh pr comment 98765 --repo secretrepoXYZ --body 'tokenvalue-ghp_abcdef123456'"
        proc = self.w.run_explain(command)
        self.assertEqual(proc.returncode, 0)
        self.assertEqual(proc.stderr, b"")
        out = proc.stdout.decode()
        self.assertEqual(len(out.splitlines()), 1)
        self.assertTrue(out.endswith("\n"))
        for value in ("98765", "secretrepoXYZ", "tokenvalue", "ghp_abcdef123456"):
            self.assertNotIn(value, out)
        self.assertEqual(set(json.loads(out)), {"decision", "reason", "guard_sha256", "shape", "test_root"})

    def test_TO_8_writes_nothing(self):
        path = self.w.write_command("gh --version")

        def snapshot():
            out = {}
            for dirpath, dirnames, filenames in os.walk(self.w.tmp):
                for name in dirnames + filenames:
                    entry = os.path.join(dirpath, name)
                    info = os.lstat(entry)
                    out[entry] = (info.st_mode, info.st_size, info.st_mtime_ns)
            return out

        before = snapshot()
        proc = self.w.run_explain(path=path)
        self.assertEqual(proc.returncode, 0)
        self.assertEqual(snapshot(), before)
        self.assertFalse(os.path.exists(self.w.log_dir))


# --- TO-9 sanitize ----------------------------------------------------------------------------------

class TestSanitize(unittest.TestCase):
    def test_TO_9_sanitize(self):
        for reason in ('a"b', "a$b", "a=b", "a\nb", "café", "a" * 61, "", "tab\there", "x;y", "a'b"):
            with self.subTest(reason=reason):
                self.assertEqual(obs().sanitize(reason), "unlisted_reason")
        for reason in ("a" * 60, "record not active/disarmed", "-- not allowed", "error:FileNotFoundError"):
            self.assertEqual(obs().sanitize(reason), reason)
        literals, _ = guard_reason_literals()
        for literal in literals:
            with self.subTest(literal=literal):
                self.assertEqual(obs().sanitize(literal), literal)


def main():
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--observer", default=DEFAULT_OBSERVER)
    opts, rest = parser.parse_known_args()
    global OBSERVER
    OBSERVER = os.path.abspath(opts.observer)
    if not os.path.isfile(OBSERVER):
        sys.stderr.write(f"observer not found: {OBSERVER}\n")
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
