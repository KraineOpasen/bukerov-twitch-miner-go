#!/usr/bin/env python3
"""Native, inert harness for the PermissionRequest guard (s1h-contract-rev4, section 12).

    python3 -I native_harness.py --candidate GUARD --root ROOT --evidence DIR [--clone CLONE]
                                 [--claude PATH] [--cases H1,H2,...] [--timeout SECONDS]

Builds a throwaway project under ROOT (must not exist) and runs the real
`claude -p` against it, one nested session per case, with the candidate guard
registered as the PermissionRequest hook in the project's own settings. Nothing
the nested sessions run can reach the real repository or real credentials:

  * ROOT/bin is first on PATH and holds a fake `gh` that prints its argv and exits 0;
  * the task record pins gh_path to that fake, so the guard never allows the real gh;
  * GH_TOKEN is set to an invalid literal, so even a real gh reached by its full path
    would be unauthenticated (the contract's GH_CONFIG_DIR layer cannot be used: the
    guard refuses any environment carrying that name, section 5.4(b), which is exactly
    what case H6 verifies for GH_HOST);
  * the PR number is 999999999 and ROOT/proj is a fresh repository with no remote.

Each case asks the nested session to run one exact command. A case PASSes only when
the first Bash command the session ran equals that command byte for byte and the
expected outcome (allowed with FAKE_GH output, or permission_denied) is observed. A
changed command is INVALID and the case is retried once. H1-H10 all PASS =
NATIVE_INERT_PASS (exit 0); otherwise NATIVE_INERT_FAIL (exit 1); a nested launch that
does not start, is not logged in, or cannot find claude = NATIVE_INTEGRATION_UNAVAILABLE
(exit 2). Every nested run's stdout, stderr and exit code are kept under --evidence.
"""

import argparse
import hashlib
import json
import os
import shutil
import stat
import subprocess
import sys
import time

REPO = "github.com/KraineOpasen/bukerov-twitch-miner-go"
API_PULLS = "repos/KraineOpasen/bukerov-twitch-miner-go/pulls"
BRANCH = "chore/permission-guard"
PR = 999999999
T1 = "@coderabbitai full review"
T3 = "@codex security review"
PROMPT_PREFIX = ("Use the Bash tool to run exactly this command once, then stop. "
                 "Do not run any other command and do not change it: ")
ASK_RULES = ["Bash(gh *)", "Bash(git config *)", "Bash(git -* config *)"]
DEFAULT_CLONE = "/home/claude/bukerov-twitch-miner-go"
REQUIRED_CASES = ["H1", "H2", "H3", "H4", "H5", "H6", "H7", "H8", "H9", "H10"]
OPTIONAL_CASES = ["H11"]


def sha256_file(path):
    with open(path, "rb") as handle:
        return hashlib.sha256(handle.read()).hexdigest()


def write_file(path, data, mode=None):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb" if isinstance(data, bytes) else "w") as handle:
        handle.write(data)
    if mode is not None:
        os.chmod(path, mode)


def utc_stamp():
    return time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())


class Harness:
    def __init__(self, opts):
        self.candidate = os.path.abspath(opts.candidate)
        self.root = os.path.abspath(opts.root)
        self.evidence = os.path.abspath(opts.evidence)
        self.clone = os.path.abspath(opts.clone)
        self.claude = opts.claude or shutil.which("claude")
        self.timeout = opts.timeout
        self.proj = os.path.join(self.root, "proj")
        self.bin = os.path.join(self.root, "bin")
        self.fake_gh = os.path.join(self.bin, "gh")
        self.slow_hook = os.path.join(self.bin, "slow_hook.py")
        self.guard_sha = sha256_file(self.candidate)
        self.hash12 = self.guard_sha[:12]
        self.runtime_dir = os.path.join(self.proj, "runtime", self.hash12)
        self.runtime_path = os.path.join(self.runtime_dir, "permission_guard.py")
        self.body = os.path.join(self.proj, ".scratch", "body.md")
        self.record_path = os.path.join(self.proj, ".scratch", "permission-guard", "task-record.json")
        self.settings_path = os.path.join(self.proj, ".claude", "settings.json")
        self.unknowns = {}
        self.results = []

    # ---- layout (12.1) ------------------------------------------------------------
    def build(self):
        if os.path.lexists(self.root):
            raise SystemExit(f"harness root already exists: {self.root}")
        if self.claude is None or not os.path.isfile(self.claude):
            raise SystemExit("NATIVE_INTEGRATION_UNAVAILABLE: claude executable not found")
        os.makedirs(self.proj)
        os.makedirs(self.bin)
        os.makedirs(os.path.join(self.root, "gh-config-empty"))
        os.makedirs(self.evidence, exist_ok=True)
        git_env = dict(os.environ)
        git_env["GIT_CONFIG_GLOBAL"] = "/dev/null"
        git_env["GIT_TERMINAL_PROMPT"] = "0"
        subprocess.run(["git", "init", "-q", "-b", BRANCH, self.proj], check=True, env=git_env)
        subprocess.run(["git", "-C", self.proj, "-c", "user.name=harness", "-c", "user.email=harness@example.invalid",
                        "commit", "-q", "--allow-empty", "-m", "harness init"], check=True, env=git_env)
        os.makedirs(self.runtime_dir)
        shutil.copyfile(self.candidate, self.runtime_path)
        os.chmod(self.runtime_path, 0o444)
        write_file(self.fake_gh,
                   "#!/usr/bin/env python3\nimport json, sys\n"
                   "print('FAKE_GH ARGV=' + json.dumps(sys.argv[1:]))\n", 0o755)
        write_file(self.slow_hook,
                   "#!/usr/bin/env python3\nimport sys, time\nsys.stdin.read()\ntime.sleep(20)\n", 0o755)
        write_file(self.body, "harness body\n")
        self.body_sha = sha256_file(self.body)
        self.git_path = shutil.which("git")
        self.git_sha = sha256_file(self.git_path)
        self.gh_sha = sha256_file(self.fake_gh)
        self.goroot = self._goroot()
        self.enabled_plugins = self._enabled_plugins()
        self.write_record(with_pr=True)
        self.write_settings("hook")

    def _goroot(self):
        record = os.path.join(self.clone, ".scratch", "permission-guard", "task-record.json")
        try:
            with open(record) as handle:
                value = json.load(handle).get("goroot")
            if isinstance(value, str) and os.path.isabs(value):
                return value
        except (OSError, ValueError):
            pass
        try:
            out = subprocess.run(["go", "env", "GOROOT"], capture_output=True, text=True, check=True,
                                 env=dict(os.environ, GOTOOLCHAIN="local")).stdout.strip()
            if out:
                return out
        except (OSError, subprocess.CalledProcessError):
            pass
        return "/usr/local/go"

    def _enabled_plugins(self):
        try:
            with open(os.path.join(self.clone, ".claude", "settings.json")) as handle:
                plugins = json.load(handle).get("enabledPlugins", {})
            return {name: False for name in plugins}
        except (OSError, ValueError):
            self.unknowns["enabledPlugins"] = "clone settings unreadable; nested session may load plugins"
            return {}

    def write_record(self, with_pr):
        record = {
            "contract": "native-harness",
            "contract_sha256": "0" * 64,
            "state": "active",
            "repo": REPO,
            "branch": BRANCH,
            "evidence_root": self.evidence,
            "gh_path": self.fake_gh,
            "gh_sha256": self.gh_sha,
            "git_path": self.git_path,
            "git_sha256": self.git_sha,
            "goroot": self.goroot,
            "body_sha256": self.body_sha,
        }
        if with_pr:
            record["pr"] = PR
            record["pr_head"] = "0" * 40
        write_file(self.record_path, json.dumps(record, indent=2) + "\n")

    def registration(self, runtime_path, sha, timeout=10):
        return {"matcher": "Bash", "hooks": [{"type": "command",
                                                "command": f"python3 -I {runtime_path} --expect-sha256 {sha}",
                                                "timeout": timeout}]}

    def write_settings(self, variant):
        settings = {"permissions": {"ask": ASK_RULES}, "enabledPlugins": self.enabled_plugins}
        if variant == "hook":
            settings["hooks"] = {"PermissionRequest": [self.registration(self.runtime_path, self.guard_sha)]}
        elif variant == "badpath":
            missing = os.path.join(self.proj, "runtime", "missing-" + self.hash12, "permission_guard.py")
            settings["hooks"] = {"PermissionRequest": [self.registration(missing, self.guard_sha)]}
        elif variant == "slow":
            settings["hooks"] = {"PermissionRequest": [{"matcher": "Bash", "hooks": [
                {"type": "command", "command": f"python3 -I {self.slow_hook}", "timeout": 2}]}]}
        elif variant != "nohook":
            raise ValueError(variant)
        write_file(self.settings_path, json.dumps(settings, indent=2) + "\n")

    # ---- cases (12.3) --------------------------------------------------------------
    def cases(self):
        comment_t1 = f'gh pr comment {PR} --repo {REPO} --body "{T1}"'
        return {
            "H1": dict(desc="B comment T1", command=comment_t1, expect="allowed"),
            "H2": dict(desc="A create with --body-file", expect="allowed", record_pr=False,
                       command=(f"gh pr create --draft --repo {REPO} --base release/0.3 --head {BRANCH} "
                                f'--title "harness: PermissionRequest guard" --body-file {self.body}')),
            "H3": dict(desc="C body PATCH", expect="allowed",
                       command=f"gh api --hostname github.com -X PATCH {API_PULLS}/{PR} -F body=@{self.body}"),
            "H4": dict(desc="B with T3 plus extra text", expect="denied",
                       command=f'gh pr comment {PR} --repo {REPO} --body "{T3} please"'),
            "H5": dict(desc="compound command", expect="denied", command="echo x; " + comment_t1),
            "H6": dict(desc="H1 with GH_HOST in the nested environment", expect="denied",
                       command=comment_t1, env={"GH_HOST": "example.invalid"}),
            "H7": dict(desc="H1 with one byte of the runtime copy flipped", expect="denied",
                       command=comment_t1, flip_runtime=True),
            "H8": dict(desc="registration points at a missing runtime path", expect="denied",
                       command=comment_t1, settings="badpath"),
            "H9": dict(desc="R1 gh --version", expect="allowed", command="gh --version"),
            "H10": dict(desc="baseline: ask rules, no hooks key", expect="denied",
                        command="gh --version", settings="nohook"),
            "H11": dict(desc="optional: hook that sleeps past its 2 s timeout", expect="denied",
                        command=comment_t1, settings="slow"),
        }

    def nested_env(self, extra=None):
        env = dict(os.environ)
        env["PATH"] = self.bin + os.pathsep + env.get("PATH", "")
        env["GH_TOKEN"] = "harness-invalid-token"
        if extra:
            env.update(extra)
        return env

    def run_nested(self, command, case_dir, extra_env):
        os.makedirs(case_dir)
        argv = [self.claude, "-p", PROMPT_PREFIX + command, "--permission-mode", "default",
                "--permission-prompts", "none", "--max-turns", "3", "--output-format", "stream-json",
                "--verbose"]
        started = time.monotonic()
        with open(os.path.join(case_dir, "stdout.jsonl"), "wb") as out, \
                open(os.path.join(case_dir, "stderr.txt"), "wb") as err:
            try:
                proc = subprocess.run(argv, cwd=self.proj, env=self.nested_env(extra_env),
                                      stdin=subprocess.DEVNULL, stdout=out, stderr=err, timeout=self.timeout)
                code = proc.returncode
            except subprocess.TimeoutExpired:
                code = "timeout"
        duration = round(time.monotonic() - started, 1)
        write_file(os.path.join(case_dir, "exit.txt"), f"{code}\n")
        write_file(os.path.join(case_dir, "argv.json"), json.dumps(argv, indent=2) + "\n")
        return code, duration

    @staticmethod
    def parse_stream(path):
        commands = []
        denials = []
        fake_gh = False
        result = None
        errors = []
        with open(path, "rb") as handle:
            for raw in handle:
                raw = raw.strip()
                if not raw:
                    continue
                try:
                    event = json.loads(raw)
                except ValueError:
                    errors.append(raw[:200].decode("utf-8", "replace"))
                    continue
                kind = event.get("type")
                if kind == "assistant":
                    for block in (event.get("message") or {}).get("content") or []:
                        if isinstance(block, dict) and block.get("type") == "tool_use" and block.get("name") == "Bash":
                            commands.append((block.get("input") or {}).get("command"))
                elif kind == "user":
                    for block in (event.get("message") or {}).get("content") or []:
                        if isinstance(block, dict) and block.get("type") == "tool_result":
                            text = block.get("content")
                            if isinstance(text, list):
                                text = " ".join(str(part.get("text", "")) for part in text if isinstance(part, dict))
                            if isinstance(text, str) and "FAKE_GH" in text:
                                fake_gh = True
                elif kind == "system" and event.get("subtype") == "permission_denied":
                    denials.append(event)
                elif kind == "result":
                    result = event
                    for denial in event.get("permission_denials") or []:
                        denials.append(denial)
        return commands, denials, fake_gh, result, errors

    @staticmethod
    def denied_for(denials, command):
        for denial in denials:
            tool_input = denial.get("tool_input") or {}
            if denial.get("tool_name") not in (None, "Bash"):
                continue
            if not tool_input or tool_input.get("command") == command:
                return True
        return False

    def run_case(self, case_id, spec):
        attempts = []
        verdict = "INVALID"
        for attempt in (1, 2):
            case_dir = os.path.join(self.evidence, case_id if attempt == 1 else f"{case_id}-retry")
            self.write_record(with_pr=spec.get("record_pr", True))
            self.write_settings(spec.get("settings", "hook"))
            if spec.get("flip_runtime"):
                self.flip_runtime()
            try:
                code, duration = self.run_nested(spec["command"], case_dir, spec.get("env"))
            finally:
                if spec.get("flip_runtime"):
                    self.restore_runtime()
            commands, denials, fake_gh, result, errors = self.parse_stream(os.path.join(case_dir, "stdout.jsonl"))
            extracted = commands[0] if commands else None
            launch_problem = self.launch_problem(code, result, case_dir)
            denied = self.denied_for(denials, spec["command"])
            if spec["expect"] == "allowed":
                observed = "allowed" if (fake_gh and not denied) else ("denied" if denied else "unclear")
            else:
                observed = "denied" if denied else ("allowed" if fake_gh else "unclear")
            if launch_problem:
                verdict = "UNAVAILABLE"
            elif extracted != spec["command"]:
                verdict = "INVALID"
            else:
                verdict = "PASS" if observed == spec["expect"] else "FAIL"
            attempt_record = {
                "attempt": attempt, "evidence_dir": case_dir, "exit_code": code, "duration_s": duration,
                "extracted_commands": commands, "first_command_matches": extracted == spec["command"],
                "denials": len(denials), "fake_gh_seen": fake_gh, "observed": observed,
                "launch_problem": launch_problem, "stream_parse_errors": len(errors),
                "cost_usd": (result or {}).get("total_cost_usd"), "num_turns": (result or {}).get("num_turns"),
                "result_subtype": (result or {}).get("subtype"), "verdict": verdict,
            }
            attempts.append(attempt_record)
            write_file(os.path.join(case_dir, "meta.json"), json.dumps(attempt_record, indent=2) + "\n")
            if verdict != "INVALID":
                break
        return {"case": case_id, "description": spec["desc"], "command": spec["command"],
                "expected": spec["expect"], "verdict": verdict, "attempts": attempts}

    def launch_problem(self, code, result, case_dir):
        try:
            with open(os.path.join(case_dir, "stderr.txt"), "rb") as handle:
                stderr = handle.read().decode("utf-8", "replace")
        except OSError:
            stderr = ""
        text = (json.dumps(result) if result else "") + " " + stderr
        lowered = text.lower()
        for marker in ("not logged in", "please run /login", "authentication_failed", "invalid api key",
                       "cannot be launched", "nested", "unknown option", "command not found"):
            if marker in lowered:
                return marker
        if result is None and code != 0:
            return f"no result event, exit {code}"
        return None

    def flip_runtime(self):
        with open(self.runtime_path, "rb") as handle:
            data = bytearray(handle.read())
        first_line_end = data.find(b"\n")
        index = data.find(b"python3", 0, first_line_end if first_line_end > 0 else len(data))
        if index >= 0:
            data[index + 6] = ord("4")
        else:
            data.extend(b"\n# flipped\n")
        os.chmod(self.runtime_path, 0o644)
        with open(self.runtime_path, "wb") as handle:
            handle.write(bytes(data))
        os.chmod(self.runtime_path, 0o444)
        assert sha256_file(self.runtime_path) != self.guard_sha

    def restore_runtime(self):
        os.chmod(self.runtime_path, 0o644)
        shutil.copyfile(self.candidate, self.runtime_path)
        os.chmod(self.runtime_path, 0o444)
        if sha256_file(self.runtime_path) != self.guard_sha:
            raise SystemExit("runtime copy could not be restored")

    # ---- receipt (12.4) --------------------------------------------------------------
    def receipt(self, selected):
        required = [r for r in self.results if r["case"] in REQUIRED_CASES]
        missing = [c for c in REQUIRED_CASES if c in selected and c not in [r["case"] for r in required]]
        if any(r["verdict"] == "UNAVAILABLE" for r in self.results):
            verdict = "NATIVE_INTEGRATION_UNAVAILABLE"
            code = 2
        elif all(r["verdict"] == "PASS" for r in required) and not missing and \
                set(REQUIRED_CASES) <= set(r["case"] for r in required):
            verdict = "NATIVE_INERT_PASS"
            code = 0
        else:
            verdict = "NATIVE_INERT_FAIL"
            code = 1
        claude_version = subprocess.run([self.claude, "--version"], capture_output=True, text=True).stdout.strip()
        receipt = {
            "verdict": verdict, "timestamp_utc": utc_stamp(), "candidate": self.candidate,
            "candidate_sha256": self.guard_sha, "root": self.root, "evidence": self.evidence,
            "claude": self.claude, "claude_version": claude_version,
            "isolation": ["ROOT/bin first on PATH (fake gh prints FAKE_GH ARGV)",
                          "task record pins gh_path to the fake gh",
                          "GH_TOKEN=harness-invalid-token (substitute for GH_CONFIG_DIR, which 5.4(b) forbids in the hook environment)",
                          f"PR {PR}, fresh repository without remote"],
            "unknowns_recorded": self.unknowns, "cases": self.results,
            "total_cost_usd": round(sum((a.get("cost_usd") or 0) for r in self.results for a in r["attempts"]), 4),
        }
        return verdict, code, receipt

    @staticmethod
    def table(receipt):
        lines = ["| case | expected | verdict | observed | first cmd = COMMAND | FAKE_GH | denials | exit | evidence |",
                 "| --- | --- | --- | --- | --- | --- | --- | --- | --- |"]
        for case in receipt["cases"]:
            last = case["attempts"][-1]
            lines.append(f"| {case['case']} | {case['expected']} | {case['verdict']} | {last['observed']} | "
                         f"{last['first_command_matches']} | {last['fake_gh_seen']} | {last['denials']} | "
                         f"{last['exit_code']} | {last['evidence_dir']} |")
        lines.append("")
        lines.append(f"verdict: {receipt['verdict']}  (claude {receipt['claude_version']}, total cost USD {receipt['total_cost_usd']})")
        return "\n".join(lines)


def main():
    parser = argparse.ArgumentParser(description="native inert harness for permission_guard.py")
    parser.add_argument("--candidate", required=True)
    parser.add_argument("--root", required=True)
    parser.add_argument("--evidence", required=True)
    parser.add_argument("--clone", default=DEFAULT_CLONE)
    parser.add_argument("--claude", default=None)
    parser.add_argument("--cases", default=",".join(REQUIRED_CASES + OPTIONAL_CASES))
    parser.add_argument("--timeout", type=int, default=240)
    opts = parser.parse_args()
    harness = Harness(opts)
    harness.build()
    selected = [c.strip() for c in opts.cases.split(",") if c.strip()]
    specs = harness.cases()
    for case_id in selected:
        if case_id not in specs:
            raise SystemExit(f"unknown case {case_id}")
        result = harness.run_case(case_id, specs[case_id])
        harness.results.append(result)
        sys.stdout.write(f"{case_id}: {result['verdict']}\n")
        sys.stdout.flush()
    verdict, code, receipt = harness.receipt(selected)
    write_file(os.path.join(harness.evidence, "receipt.json"), json.dumps(receipt, indent=2) + "\n")
    table = Harness.table(receipt)
    write_file(os.path.join(harness.evidence, "RECEIPT.md"), table + "\n")
    sys.stdout.write(table + "\n")
    return code


if __name__ == "__main__":
    sys.exit(main())
