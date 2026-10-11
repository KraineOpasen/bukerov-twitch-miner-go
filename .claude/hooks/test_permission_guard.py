#!/usr/bin/env python3
"""Offline adversarial tests for permission_guard.py (s1h-contract-rev4, section 6.1).

Run as a script, never through ``-m unittest``::

    python3 -I .claude/hooks/test_permission_guard.py -v [--guard PATH]

Every test runs the guard as a subprocess with a crafted PermissionRequest
JSON on stdin, inside a temporary CLAUDE_PROJECT_DIR that the test builds: a
real git repository on the governed branch, a task record, body files, and a
fake ``gh`` and ``git`` on a private PATH whose hashes the record carries.
The guard may answer exactly one of two ways: the ALLOW JSON, or nothing.
Exit code 0 in both cases. Zero collected tests or any skipped test fails the run.

Test names carry the contract requirement they pin (5.1 ... 5.7).
"""

import argparse
import hashlib
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_GUARD = os.path.join(HERE, "permission_guard.py")

ALLOW = ('{"hookSpecificOutput": {"hookEventName": "PermissionRequest", '
         '"decision": {"behavior": "allow"}}}\n')
REPO = "github.com/KraineOpasen/bukerov-twitch-miner-go"
API_PULLS = "repos/KraineOpasen/bukerov-twitch-miner-go/pulls"
BASE = "release/0.3"
BRANCH = "chore/permission-guard"
PR = 427
PR_HEAD = "a" * 40
T1 = "@coderabbitai full review"
T2 = "@codex review Full review of the entire PR. Review only; make no changes."
T3 = "@codex security review"
TITLE = '"chore(claude): PermissionRequest guard for unattended Draft-PR work"'
GUARD_TIMEOUT = 30

GUARD = DEFAULT_GUARD  # overridden by --guard in main()


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        h.update(fh.read())
    return h.hexdigest()


def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


class Fixture:
    """A temporary project the guard is pointed at."""

    def __init__(self, tmp):
        self.tmp = tmp
        self.proj = os.path.join(tmp, "proj")
        os.mkdir(self.proj)
        subprocess.run(["git", "init", "-q", "-b", BRANCH, self.proj], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.scratch = os.path.join(self.proj, ".scratch")
        os.mkdir(self.scratch)
        self.pg_dir = os.path.join(self.scratch, "permission-guard")
        os.mkdir(self.pg_dir)
        self.record_path = os.path.join(self.pg_dir, "task-record.json")
        self.evidence = os.path.join(self.scratch, "evidence")
        os.mkdir(self.evidence)
        # project content the read grammar can point at
        self.file = os.path.join(self.proj, "README.txt")
        with open(self.file, "w") as fh:
            fh.write("git is a tool\nsecond line\n")
        self.subdir = os.path.join(self.proj, "internal")
        os.mkdir(self.subdir)
        with open(os.path.join(self.subdir, "a.go"), "w") as fh:
            fh.write("package internal\n")
        # a nested .git directory (a vendored or fixture repository) under the project
        self.nested_git = os.path.join(self.proj, "sub", ".git")
        os.makedirs(self.nested_git)
        with open(os.path.join(self.nested_git, "config"), "w") as fh:
            fh.write("[core]\n")
        # a fake GOROOT with GOROOT/src/time/format.go and a .git under src
        self.goroot = os.path.join(tmp, "goroot")
        os.makedirs(os.path.join(self.goroot, "src", "time"))
        os.makedirs(os.path.join(self.goroot, "src", ".git"))
        with open(os.path.join(self.goroot, "src", ".git", "config"), "w") as fh:
            fh.write("[core]\n")
        self.time_file = os.path.join(self.goroot, "src", "time", "format.go")
        with open(self.time_file, "w") as fh:
            fh.write("package time\n")
        # fake executables on a private PATH
        self.bin = os.path.join(tmp, "bin")
        os.mkdir(self.bin)
        self.gh = self._exe(self.bin, "gh", "#!/bin/sh\necho fake-gh\n")
        self.git = self._exe(self.bin, "git", "#!/bin/sh\necho fake-git\n")
        self.gh_sha = sha256_file(self.gh)
        self.git_sha = sha256_file(self.git)
        # body snapshot
        self.body = os.path.join(self.scratch, "PR-BODY.md")
        with open(self.body, "w") as fh:
            fh.write("# PR body\n\nsnapshot\n")
        self.body_sha = sha256_file(self.body)
        self.set_record("pr")

    @staticmethod
    def _exe(directory, name, content):
        path = os.path.join(directory, name)
        with open(path, "w") as fh:
            fh.write(content)
        os.chmod(path, 0o755)
        return path

    def base_record(self, mode):
        rec = {
            "contract": "test-contract",
            "contract_sha256": "0" * 64,
            "state": "active",
            "repo": REPO,
            "branch": BRANCH,
            "evidence_root": self.evidence,
            "gh_path": self.gh,
            "gh_sha256": self.gh_sha,
            "git_path": self.git,
            "git_sha256": self.git_sha,
            "goroot": self.goroot,
            "body_sha256": self.body_sha,
        }
        if mode == "pr":
            rec["pr"] = PR
            rec["pr_head"] = PR_HEAD
        return rec

    def set_record(self, mode="pr", overrides=None, remove=()):
        rec = self.base_record(mode)
        if overrides:
            rec.update(overrides)
        for key in remove:
            rec.pop(key, None)
        if os.path.islink(self.record_path) or os.path.isfile(self.record_path):
            os.unlink(self.record_path)
        with open(self.record_path, "w") as fh:
            json.dump(rec, fh)
        return rec

    def write_body(self, data, name="PR-BODY.md", directory=None):
        path = os.path.join(directory or self.scratch, name)
        with open(path, "wb") as fh:
            fh.write(data)
        return path

    def env(self, extra=None):
        env = {"PATH": self.bin, "CLAUDE_PROJECT_DIR": self.proj, "LANG": "C.UTF-8"}
        if extra:
            env.update(extra)
        return env

    def hook_input(self, command, overrides=None, remove=()):
        data = {
            "session_id": "test-session",
            "transcript_path": "/nonexistent/transcript.jsonl",
            "cwd": self.proj,
            "permission_mode": "default",
            "hook_event_name": "PermissionRequest",
            "tool_name": "Bash",
            "tool_input": {"command": command, "description": "test"},
        }
        if overrides:
            data.update(overrides)
        for key in remove:
            data.pop(key, None)
        return data

    def run(self, command=None, raw=None, overrides=None, remove=(), env_extra=None,
            guard=None, sha=None, argv=None):
        guard = guard or GUARD
        if argv is None:
            argv = ["--expect-sha256", sha or sha256_file(guard)]
        payload = raw if raw is not None else json.dumps(
            self.hook_input(command, overrides, remove)).encode()
        proc = subprocess.run([sys.executable, "-I", guard] + argv, input=payload,
                              capture_output=True, env=self.env(env_extra),
                              cwd=self.tmp, timeout=GUARD_TIMEOUT)
        return proc.stdout.decode(), proc.stderr.decode(), proc.returncode

    # command builders -----------------------------------------------------
    def a_cmd(self, draft="--draft", repo=REPO, base=BASE, head=BRANCH, title=TITLE,
              body=None, extra=""):
        body = body or self.body
        return (f"gh pr create {draft} --repo {repo} --base {base} --head {head} "
                f"--title {title} --body-file {body}{extra}").replace("  ", " ")

    def b_cmd(self, n=PR, repo=REPO, body=T1, body_file=None, extra=""):
        if body_file:
            return f"gh pr comment {n} --repo {repo} --body-file {body_file}{extra}"
        return f'gh pr comment {n} --repo {repo} --body "{body}"{extra}'

    def c_cmd(self, n=PR, hostname="--hostname github.com", method="-X PATCH",
              endpoint=None, field=None, extra=""):
        endpoint = endpoint or f"{API_PULLS}/{n}"
        field = field or f"-F body=@{self.body}"
        return f"gh api {hostname} {method} {endpoint} {field}{extra}".replace("  ", " ")


class GuardTest(unittest.TestCase):
    def setUp(self):
        # the unreadable-file cases need a non-root runner; an assertion, never a skip
        self.assertNotEqual(os.geteuid(), 0, "these tests must not run as root")
        self._tmp = tempfile.TemporaryDirectory(prefix="pgtest-")
        self.fx = Fixture(self._tmp.name)

    def tearDown(self):
        # the FIFO and unreadable files must not block cleanup
        for root, dirs, files in os.walk(self._tmp.name):
            for name in files + dirs:
                try:
                    os.chmod(os.path.join(root, name), 0o700)
                except OSError:
                    pass
        self._tmp.cleanup()

    # assertion helpers --------------------------------------------------
    def expect_allow(self, command, **kw):
        out, err, rc = self.fx.run(command, **kw)
        self.assertEqual(rc, 0, f"exit {rc}, stderr={err!r}")
        self.assertEqual(out, ALLOW, f"expected ALLOW for {command!r}; stderr={err!r}")
        self.assertEqual(err, "", "the guard never logs")

    def expect_empty(self, command=None, **kw):
        out, err, rc = self.fx.run(command, **kw)
        self.assertEqual(rc, 0, f"exit {rc}, stderr={err!r}")
        self.assertEqual(out, "", f"expected NO DECISION for {command!r}; stderr={err!r}")
        self.assertEqual(err, "", "the guard never logs")

    # 5.1 input contract ---------------------------------------------------
    def test_5_1_wrong_event_no_output(self):
        self.expect_empty(self.fx.b_cmd(), overrides={"hook_event_name": "PreToolUse"})

    def test_5_1_wrong_tool_no_output(self):
        self.expect_empty(self.fx.b_cmd(), overrides={"tool_name": "Read"})

    def test_5_1_malformed_json(self):
        self.expect_empty(raw=b'{"hook_event_name": "PermissionRequest", ')

    def test_5_1_duplicate_json_keys(self):
        good = json.dumps(self.fx.hook_input(self.fx.b_cmd()))
        raw = good[:-1] + ', "tool_name": "Bash"}'
        self.expect_empty(raw=raw.encode())

    def test_5_1_non_object_input(self):
        self.expect_empty(raw=b'[1, 2, 3]')

    def test_5_1_missing_tool_input(self):
        self.expect_empty(self.fx.b_cmd(), remove=("tool_input",))

    def test_5_1_tool_input_wrong_type(self):
        self.expect_empty(None, overrides={"tool_input": "gh pr comment"})

    def test_5_1_command_wrong_type(self):
        self.expect_empty(None, overrides={"tool_input": {"command": ["gh"]}})

    def test_5_1_oversized_command(self):
        self.expect_empty(self.fx.b_cmd() + " " + "x" * 8192)

    def test_5_1_oversized_stdin(self):
        data = self.fx.hook_input(self.fx.b_cmd())
        data["tool_input"]["description"] = "y" * (1024 * 1024 + 1)
        self.expect_empty(raw=json.dumps(data).encode())

    def test_5_1_expect_sha256_missing(self):
        self.expect_empty(self.fx.b_cmd(), argv=[])

    def test_5_1_expect_sha256_malformed(self):
        self.expect_empty(self.fx.b_cmd(), argv=["--expect-sha256", "nothex"])

    def test_5_1_self_hash_mismatch_altered_copy_answers_nothing(self):
        altered = os.path.join(self.fx.tmp, "guard_altered.py")
        shutil.copyfile(GUARD, altered)
        with open(altered, "ab") as fh:
            fh.write(b"\n# one more byte\n")
        self.expect_empty(self.fx.b_cmd(), guard=altered, sha=sha256_file(GUARD))
        self.expect_empty("gh --version", guard=altered, sha=sha256_file(GUARD))

    def test_5_1_self_hash_matches_altered_copy_with_its_own_hash(self):
        altered = os.path.join(self.fx.tmp, "guard_altered.py")
        shutil.copyfile(GUARD, altered)
        with open(altered, "ab") as fh:
            fh.write(b"\n# one more byte\n")
        self.expect_allow(self.fx.b_cmd(), guard=altered, sha=sha256_file(altered))

    def test_5_1_internal_error_record_is_directory(self):
        os.unlink(self.fx.record_path)
        os.mkdir(self.fx.record_path)
        out, err, rc = self.fx.run(self.fx.b_cmd())
        self.assertEqual(rc, 0)
        self.assertEqual(out, "")

    def test_5_1_cwd_wrong_type(self):
        self.expect_empty(self.fx.b_cmd(), overrides={"cwd": 5})

    # 5.2 raw string and tokenization ---------------------------------------
    def test_5_2_non_printable_character(self):
        self.expect_empty(self.fx.b_cmd() + "\x01")

    def test_5_2_tab_character(self):
        self.expect_empty(self.fx.b_cmd().replace(" --repo", "\t--repo"))

    def test_5_2_dollar_in_command(self):
        self.expect_empty(self.fx.b_cmd(body="@coderabbitai full review$"))

    def test_5_2_dollar_subshell(self):
        self.expect_empty(self.fx.b_cmd() + " $(id)")

    def test_5_2_backtick(self):
        self.expect_empty(self.fx.b_cmd() + " `id`")

    # the raw-character rule placed in the least-validated slots, so only that rule can reject
    def test_5_2_dollar_in_title_slot(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(title='"fix $HOME handling"'))

    def test_5_2_backtick_in_title_slot(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(title='"fix `id` handling"'))

    def test_5_2_dollar_in_grep_pattern(self):
        self.expect_empty(f"grep -c 'end$' {self.fx.file}")

    def test_5_2_dollar_in_git_format(self):
        self.expect_empty("git log --format=%H$x -n 1")

    def test_5_2_newline(self):
        self.expect_empty(self.fx.b_cmd() + "\necho hi")

    def test_5_2_carriage_return(self):
        self.expect_empty(self.fx.b_cmd() + "\recho hi")

    def test_5_2_unrelated_echo(self):
        self.expect_empty("echo hi")

    def test_5_2_gh_pr_view_not_governed(self):
        self.expect_empty(f"gh pr view 1 --repo {REPO}")

    def test_5_2_compound_and(self):
        self.expect_empty("echo x && " + self.fx.b_cmd())

    def test_5_2_compound_semicolon(self):
        self.expect_empty(self.fx.b_cmd() + " ; echo")

    def test_5_2_compound_pipe(self):
        self.expect_empty(self.fx.b_cmd() + " | cat")

    def test_5_2_compound_redirect(self):
        self.expect_empty(self.fx.b_cmd() + " > /tmp/out")

    def test_5_2_compound_subshell_parens(self):
        self.expect_empty("(" + self.fx.b_cmd() + ")")

    def test_5_2_compound_pipe_on_git_read(self):
        self.expect_empty("git log --oneline | cat")

    def test_5_2_compound_semicolon_on_git_read(self):
        self.expect_empty("git log --oneline ; echo hi")

    def test_5_2_compound_redirect_on_git_read(self):
        self.expect_empty("git log --oneline > out.txt")

    def test_5_2_compound_and_on_git_read(self):
        self.expect_empty("git log --oneline && echo hi")

    # operators after `--`, where every other token is accepted as a repository path
    def test_5_2_compound_pipe_after_dashdash(self):
        self.expect_empty("git log --oneline -- internal | cat")

    def test_5_2_compound_semicolon_after_dashdash(self):
        self.expect_empty("git check-ignore -q -- .scratch/x ; echo hi")

    def test_5_2_leading_env_assignment_gh_repo(self):
        self.expect_empty("GH_REPO=x " + self.fx.b_cmd())

    def test_5_2_leading_env_assignment_gh_host(self):
        self.fx.set_record("create")
        self.expect_empty("GH_HOST=example.invalid " + self.fx.a_cmd())

    def test_5_2_unparsable_quoting(self):
        self.expect_empty(f'gh pr comment {PR} --repo {REPO} --body "unterminated')

    def test_5_2_glob_token_rejected(self):
        self.expect_empty(f"cat {self.fx.proj}/*.txt")

    def test_5_2_tilde_token_rejected(self):
        self.expect_empty("cat ~/README.txt")

    def test_5_2_path_form_of_gh_rejected(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd().replace("gh pr", f"{self.fx.gh} pr", 1))
        self.expect_empty(self.fx.a_cmd().replace("gh pr", "/usr/bin/gh pr", 1))

    def test_5_2_comment_token_rejected(self):
        self.expect_empty(self.fx.b_cmd() + " #note")

    def test_5_2_comment_token_rejected_on_git_read(self):
        self.expect_empty("git log --oneline #note")

    def test_5_2_tilde_inside_gh_token(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(title='"a~b"'))
        self.fx.set_record("pr")
        self.expect_empty(self.fx.c_cmd(field=f"-F body=@{self.fx.body}:~/x"))

    # 5.5 A — create ----------------------------------------------------------
    def test_5_5A_ok_long_flags(self):
        self.fx.set_record("create")
        self.expect_allow(self.fx.a_cmd())

    def test_5_5A_ok_short_flags(self):
        self.fx.set_record("create")
        self.expect_allow(f"gh pr create -d -R {REPO} -B {BASE} -H {BRANCH} -t {TITLE} -F {self.fx.body}")

    def test_5_5A_ok_equals_form(self):
        self.fx.set_record("create")
        self.expect_allow(f"gh pr create --draft --repo={REPO} --base={BASE} --head={BRANCH} "
                          f"--title={TITLE} --body-file={self.fx.body}")

    def test_5_5A_ok_any_flag_order(self):
        self.fx.set_record("create")
        self.expect_allow(f"gh pr create --body-file {self.fx.body} --title {TITLE} --head {BRANCH} "
                          f"--base {BASE} --repo {REPO} --draft")

    def test_5_5A_ok_permission_mode_auto(self):
        self.fx.set_record("create")
        self.expect_allow(self.fx.a_cmd(), overrides={"permission_mode": "auto"})

    def test_5_5A_missing_draft(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(draft=""))

    def test_5_5A_wrong_base(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(base="main"))

    def test_5_5A_head_release_branch(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(head="release/0.3"))

    def test_5_5A_head_main(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(head="main"))

    def test_5_5A_head_not_current_branch(self):
        """Only --head differs from the checked-out branch; the record still names the real branch,
        so this cell is reached by the head check alone."""
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(head="fix/other"))

    def test_5_5A_record_branch_mismatch(self):
        self.fx.set_record("create", overrides={"branch": "fix/other"})
        self.expect_empty(self.fx.a_cmd())

    def test_5_5A_missing_draft_short_form(self):
        self.fx.set_record("create")
        self.expect_empty(f"gh pr create -R {REPO} -B {BASE} -H {BRANCH} -t {TITLE} -F {self.fx.body}")

    def test_5_5A_wrong_base_similar_name(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(base="release/0.4"))

    def test_5_5A_web_flag(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(extra=" --web"))

    def test_5_5A_fill_flag(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(extra=" --fill"))

    def test_5_5A_unknown_flag(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(extra=" --label bug"))

    def test_5_5A_positional_operand(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(extra=" extra"))

    def test_5_5A_repeated_draft(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(extra=" --draft"))

    def test_5_5A_repeated_title(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(extra=f" --title {TITLE}"))

    def test_5_5A_inline_body(self):
        self.fx.set_record("create")
        cmd = self.fx.a_cmd().replace(f"--body-file {self.fx.body}", '--body "text"')
        self.expect_empty(cmd)

    def test_5_5A_repo_without_host(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(repo="KraineOpasen/bukerov-twitch-miner-go"))

    def test_5_5A_wrong_repo(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(repo="github.com/KraineOpasen/other-repo"))

    def test_5_5A_empty_title(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(title='""'))

    def test_5_5A_title_too_long(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(title='"' + "t" * 201 + '"'))

    def test_5_5A_body_file_outside_scratch(self):
        outside = self.fx.write_body(b"# PR body\n\nsnapshot\n", "PR-BODY.md", self.fx.tmp)
        self.fx.set_record("create", overrides={"body_sha256": sha256_file(outside)})
        self.expect_empty(self.fx.a_cmd(body=outside))

    def test_5_5A_body_file_relative_path(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(body=".scratch/PR-BODY.md"))

    def test_5_5A_body_file_symlink_component(self):
        real_dir = os.path.join(self.fx.scratch, "realdir")
        os.mkdir(real_dir)
        body = self.fx.write_body(b"# PR body\n\nsnapshot\n", "PR-BODY.md", real_dir)
        link = os.path.join(self.fx.scratch, "linkdir")
        os.symlink(real_dir, link)
        self.fx.set_record("create", overrides={"body_sha256": sha256_file(body)})
        self.expect_empty(self.fx.a_cmd(body=os.path.join(link, "PR-BODY.md")))
        self.expect_allow(self.fx.a_cmd(body=body))  # the same bytes by their real path

    def test_5_5A_body_file_symlink_leaf(self):
        link = os.path.join(self.fx.scratch, "body-link.md")
        os.symlink(self.fx.body, link)
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(body=link))

    def test_5_5A_body_file_fifo(self):
        fifo = os.path.join(self.fx.scratch, "fifo.md")
        os.mkfifo(fifo)
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(body=fifo))

    def test_5_5A_body_file_hardlinked(self):
        other = os.path.join(self.fx.scratch, "PR-BODY-2.md")
        os.link(self.fx.body, other)
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd())

    def test_5_5A_body_file_wrong_owner(self):
        """Ownership cannot be changed by an unprivileged test; when chown fails the
        contract's skip-free fallback applies: the case is simulated by a hash mismatch
        and this fact is written to stderr."""
        self.fx.set_record("create")
        try:
            os.chown(self.fx.body, os.getuid() + 1, -1)
            self.expect_empty(self.fx.a_cmd())
        except PermissionError:
            sys.stderr.write("NOTE: chown impossible for this uid; wrong-owner case simulated "
                             "by a body hash mismatch\n")
            self.fx.set_record("create", overrides={"body_sha256": "f" * 64})
            self.expect_empty(self.fx.a_cmd())

    def test_5_5A_body_file_too_large(self):
        body = self.fx.write_body(b"x" * 65537)
        self.fx.set_record("create", overrides={"body_sha256": sha256_file(body)})
        self.expect_empty(self.fx.a_cmd(body=body))

    def test_5_5A_body_file_exactly_max_size_ok(self):
        body = self.fx.write_body(b"x" * 65536)
        self.fx.set_record("create", overrides={"body_sha256": sha256_file(body)})
        self.expect_allow(self.fx.a_cmd(body=body))

    def test_5_5A_body_hash_mismatch(self):
        self.fx.set_record("create", overrides={"body_sha256": "f" * 64})
        self.expect_empty(self.fx.a_cmd())

    def test_5_5A_body_hash_missing_from_record(self):
        self.fx.set_record("create", remove=("body_sha256",))
        self.expect_empty(self.fx.a_cmd())

    def test_5_5A_pr_already_in_record(self):
        self.fx.set_record("pr")
        self.expect_empty(self.fx.a_cmd())

    def test_5_5A_pr_null_in_record_ok(self):
        self.fx.set_record("create", overrides={"pr": None})
        self.expect_allow(self.fx.a_cmd())

    def test_5_5A_body_file_unreadable(self):
        self.fx.set_record("create")
        os.chmod(self.fx.body, 0)
        self.expect_empty(self.fx.a_cmd())

    # 5.5 B — comment ---------------------------------------------------------
    def test_5_5B_t1_ok_body(self):
        self.expect_allow(self.fx.b_cmd(body=T1))

    def test_5_5B_t1_ok_short_flags(self):
        self.expect_allow(f'gh pr comment {PR} -R {REPO} -b "{T1}"')

    def test_5_5B_t1_ok_equals_form(self):
        self.expect_allow(f'gh pr comment {PR} --repo={REPO} --body="{T1}"')

    def test_5_5B_t2_ok_one_line_body(self):
        self.expect_allow(self.fx.b_cmd(body=T2))

    def test_5_5B_t2_ok_two_line_body_file(self):
        path = self.fx.write_body(b"@codex review\nFull review of the entire PR. Review only; make no changes.\n", "t2.md")
        self.expect_allow(self.fx.b_cmd(body_file=path))

    def test_5_5B_t2_ok_crlf_body_file_short_flag(self):
        path = self.fx.write_body(b"@codex review\r\nFull review of the entire PR. Review only; make no changes.\r\n", "t2.md")
        self.expect_allow(f"gh pr comment {PR} --repo {REPO} -F {path}")

    def test_5_5B_t2_ok_tab_separated_body_file(self):
        path = self.fx.write_body(b"@codex\treview\tFull review of the entire PR. Review only; make no changes.\n", "t2.md")
        self.expect_allow(self.fx.b_cmd(body_file=path))

    def test_5_5B_body_file_unicode_line_separator(self):
        path = self.fx.write_body("@coderabbitai full review\n".encode("utf-8"), "t1.md")
        self.expect_empty(self.fx.b_cmd(body_file=path))

    def test_5_5B_body_file_nbsp_separator(self):
        path = self.fx.write_body("@codex security review\n".encode("utf-8"), "t3.md")
        self.expect_empty(self.fx.b_cmd(body_file=path))

    def test_5_5B_repeated_body_flag(self):
        self.expect_empty(self.fx.b_cmd(extra=f' --body "{T1}"'))

    def test_5_5B_record_without_pr(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.b_cmd())

    def test_5_5B_permission_mode_other_values(self):
        for mode in ("acceptEdits", "bypassPermissions", "dontAsk"):
            self.expect_empty(self.fx.b_cmd(), overrides={"permission_mode": mode})

    def test_5_5B_t3_ok(self):
        self.expect_allow(self.fx.b_cmd(body=T3))

    def test_5_5B_t3_plus_extra_text(self):
        self.expect_empty(self.fx.b_cmd(body=T3 + " please"))

    def test_5_5B_t1_plus_trailing_note(self):
        self.expect_empty(self.fx.b_cmd(body=T1 + " (round 2)"))

    def test_5_5B_t1_prefix_only(self):
        self.expect_empty(self.fx.b_cmd(body="@coderabbitai full"))

    def test_5_5B_body_file_wrong_text(self):
        path = self.fx.write_body(b"@codex review\nsomething else\n", "t2.md")
        self.expect_empty(self.fx.b_cmd(body_file=path))

    def test_5_5B_number_not_record_pr(self):
        self.expect_empty(self.fx.b_cmd(n=PR + 1))

    def test_5_5B_number_not_integer(self):
        self.expect_empty(self.fx.b_cmd(n="427x"))

    def test_5_5B_record_state_closed(self):
        self.fx.set_record("pr", overrides={"state": "closed"})
        self.expect_empty(self.fx.b_cmd())

    def test_5_5B_record_state_disarmed(self):
        self.fx.set_record("pr", overrides={"state": "disarmed"})
        self.expect_empty(self.fx.b_cmd())

    def test_5_5B_record_contract_missing_or_empty(self):
        self.fx.set_record("pr", overrides={"contract": ""})
        self.expect_empty(self.fx.b_cmd())
        self.fx.set_record("pr", remove=("contract",))
        self.expect_empty(self.fx.b_cmd())

    def test_5_5B_record_repo_mismatch(self):
        self.fx.set_record("pr", overrides={"repo": "github.com/KraineOpasen/other"})
        self.expect_empty(self.fx.b_cmd())

    def test_5_5B_record_branch_mismatch(self):
        self.fx.set_record("pr", overrides={"branch": "fix/other"})
        self.expect_empty(self.fx.b_cmd())

    def test_5_5B_record_pr_head_malformed(self):
        self.fx.set_record("pr", overrides={"pr_head": "abc"})
        self.expect_empty(self.fx.b_cmd())

    def test_5_5B_missing_record(self):
        os.unlink(self.fx.record_path)
        self.expect_empty(self.fx.b_cmd())

    def test_5_5B_record_is_symlink(self):
        real = os.path.join(self.fx.scratch, "real-record.json")
        os.rename(self.fx.record_path, real)
        os.symlink(real, self.fx.record_path)
        self.expect_empty(self.fx.b_cmd())

    def test_5_5B_record_oversized(self):
        self.fx.set_record("pr", overrides={"pad": "p" * 17000})
        self.expect_empty(self.fx.b_cmd())

    def test_5_5B_record_duplicate_keys(self):
        with open(self.fx.record_path) as fh:
            text = fh.read()
        with open(self.fx.record_path, "w") as fh:
            fh.write(text[:-1] + ', "state": "active"}')
        self.expect_empty(self.fx.b_cmd())

    def test_5_5B_edit_last(self):
        self.expect_empty(self.fx.b_cmd(extra=" --edit-last"))

    def test_5_5B_missing_repo(self):
        self.expect_empty(f'gh pr comment {PR} --body "{T1}"')

    def test_5_5B_repo_without_host(self):
        self.expect_empty(self.fx.b_cmd(repo="KraineOpasen/bukerov-twitch-miner-go"))

    def test_5_5B_both_body_and_body_file(self):
        path = self.fx.write_body(T1.encode(), "t1.md")
        self.expect_empty(self.fx.b_cmd(extra=f" --body-file {path}"))

    def test_5_5B_body_file_unreadable(self):
        path = self.fx.write_body(T1.encode(), "t1.md")
        os.chmod(path, 0)
        self.expect_empty(self.fx.b_cmd(body_file=path))

    def test_5_5B_body_file_outside_scratch(self):
        path = self.fx.write_body(T1.encode(), "t1.md", self.fx.tmp)
        self.expect_empty(self.fx.b_cmd(body_file=path))

    def test_5_5B_detached_head(self):
        head = os.path.join(self.fx.proj, ".git", "HEAD")
        with open(head, "w") as fh:
            fh.write("0" * 40 + "\n")
        self.expect_empty(self.fx.b_cmd())

    def test_5_5B_gitdir_file_head(self):
        head = os.path.join(self.fx.proj, ".git", "HEAD")
        with open(head, "w") as fh:
            fh.write("gitdir: /elsewhere\n")
        self.expect_empty(self.fx.b_cmd())

    # 5.5 C — body PATCH ------------------------------------------------------
    def test_5_5C_ok(self):
        self.expect_allow(self.fx.c_cmd())

    def test_5_5C_ok_method_field_long_form(self):
        self.expect_allow(self.fx.c_cmd(method="--method PATCH", field=f"--field body=@{self.fx.body}"))

    def test_5_5C_ok_equals_form(self):
        self.expect_allow(f"gh api --hostname=github.com --method=PATCH {API_PULLS}/{PR} --field=body=@{self.fx.body}")

    def test_5_5C_ok_with_accept_header(self):
        self.expect_allow(self.fx.c_cmd(extra=' -H "Accept: application/vnd.github+json"'))

    def test_5_5C_other_header(self):
        self.expect_empty(self.fx.c_cmd(extra=' -H "X-Other: 1"'))

    def test_5_5C_missing_hostname(self):
        self.expect_empty(self.fx.c_cmd(hostname=""))

    def test_5_5C_other_hostname(self):
        self.expect_empty(self.fx.c_cmd(hostname="--hostname example.invalid"))

    def test_5_5C_other_method(self):
        self.expect_empty(self.fx.c_cmd(method="-X DELETE"))

    def test_5_5C_missing_method(self):
        self.expect_empty(self.fx.c_cmd(method=""))

    def test_5_5C_other_endpoint(self):
        self.expect_empty(self.fx.c_cmd(endpoint=f"repos/KraineOpasen/other/pulls/{PR}"))
        self.expect_empty(self.fx.c_cmd(endpoint=f"repos/KraineOpasen/bukerov-twitch-miner-go/issues/{PR}"))

    def test_5_5C_extra_field(self):
        self.expect_empty(self.fx.c_cmd(extra=" -F title=x"))

    def test_5_5C_input_flag(self):
        self.expect_empty(self.fx.c_cmd(field=f"--input {self.fx.body}"))

    def test_5_5C_raw_field(self):
        self.expect_empty(self.fx.c_cmd(field="-f body=x"))

    def test_5_5C_field_not_body(self):
        self.expect_empty(self.fx.c_cmd(field=f"-F title=@{self.fx.body}"))

    def test_5_5C_other_pr(self):
        self.expect_empty(self.fx.c_cmd(n=PR + 1))

    def test_5_5C_body_hash_mismatch(self):
        self.fx.set_record("pr", overrides={"body_sha256": "f" * 64})
        self.expect_empty(self.fx.c_cmd())

    def test_5_5C_record_without_pr(self):
        self.fx.set_record("create")
        self.expect_empty(self.fx.c_cmd())

    def test_5_5C_paginate_and_jq(self):
        self.expect_empty(self.fx.c_cmd(extra=" --paginate"))
        self.expect_empty(self.fx.c_cmd(extra=" --jq .id"))

    # 5.4 binding ---------------------------------------------------------------
    def test_5_4a_agent_id_present_rejects_writes(self):
        sub = {"agent_id": "agent-1", "agent_type": "Explore"}
        self.expect_empty(self.fx.b_cmd(), overrides=sub)
        self.expect_empty(self.fx.c_cmd(), overrides=sub)
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(), overrides=sub)

    def test_5_4a_agent_id_present_allows_reads(self):
        sub = {"agent_id": "agent-1", "agent_type": "Explore"}
        self.expect_allow("gh --version", overrides=sub)
        self.expect_allow("git status --porcelain", overrides=sub)

    def test_5_4a_permission_mode_plan(self):
        self.expect_empty(self.fx.b_cmd(), overrides={"permission_mode": "plan"})

    def test_5_4a_permission_mode_missing(self):
        self.expect_empty(self.fx.b_cmd(), remove=("permission_mode",))

    def test_5_4a_cwd_not_project(self):
        self.expect_empty(self.fx.b_cmd(), overrides={"cwd": self.fx.tmp})

    def test_5_4a_cwd_not_required_for_reads(self):
        self.expect_allow("gh --version", overrides={"cwd": self.fx.tmp})

    def test_5_4a_project_dir_env_missing(self):
        proc = subprocess.run(
            [sys.executable, "-I", GUARD, "--expect-sha256", sha256_file(GUARD)],
            input=json.dumps(self.fx.hook_input(self.fx.b_cmd())).encode(),
            capture_output=True, env={"PATH": self.fx.bin}, timeout=GUARD_TIMEOUT)
        self.assertEqual(proc.returncode, 0)
        self.assertEqual(proc.stdout.decode(), "")
        self.assertEqual(proc.stderr.decode(), "")

    def test_5_4b_env_gh_host_rejects_writes(self):
        self.expect_empty(self.fx.b_cmd(), env_extra={"GH_HOST": "example.invalid"})
        self.expect_empty(self.fx.c_cmd(), env_extra={"GH_HOST": "example.invalid"})
        self.fx.set_record("create")
        self.expect_empty(self.fx.a_cmd(), env_extra={"GH_HOST": "example.invalid"})

    def test_5_4b_env_pager_rejects_writes(self):
        self.expect_empty(self.fx.b_cmd(), env_extra={"PAGER": "less"})

    def test_5_4b_env_git_dir_rejects_writes_and_git_reads(self):
        self.expect_empty(self.fx.b_cmd(), env_extra={"GIT_DIR": "/tmp/x"})
        self.expect_empty("git status --porcelain", env_extra={"GIT_DIR": "/tmp/x"})
        self.expect_empty("git config --get user.name", env_extra={"GIT_CONFIG_GLOBAL": "/tmp/x"})

    def test_5_4b_env_gh_host_rejects_r1_not_r6(self):
        self.expect_empty("gh --version", env_extra={"GH_HOST": "example.invalid"})
        self.expect_allow("git status --porcelain", env_extra={"GH_HOST": "example.invalid"})

    def test_5_4b_env_git_dir_does_not_reject_r1(self):
        self.expect_allow("gh --version", env_extra={"GIT_DIR": "/tmp/x"})

    def test_5_4c_gh_resolves_to_another_path(self):
        bin2 = os.path.join(self.fx.tmp, "bin2")
        os.mkdir(bin2)
        shutil.copy2(self.fx.gh, os.path.join(bin2, "gh"))
        self.expect_empty(self.fx.b_cmd(), env_extra={"PATH": bin2})

    def test_5_4c_gh_hash_mismatch(self):
        with open(self.fx.gh, "a") as fh:
            fh.write("# changed\n")
        self.expect_empty(self.fx.b_cmd())
        self.expect_empty("gh --version")

    def test_5_4c_git_hash_mismatch_rejects_git_reads(self):
        with open(self.fx.git, "a") as fh:
            fh.write("# changed\n")
        self.expect_empty("git status --porcelain")

    def test_5_4c_gh_missing_from_path(self):
        os.unlink(self.fx.gh)
        self.expect_empty(self.fx.b_cmd())

    # 5.6 R1 ------------------------------------------------------------------
    def test_5_6R1_gh_version_flag_ok(self):
        self.expect_allow("gh --version")

    def test_5_6R1_gh_version_word_ok(self):
        self.expect_allow("gh version")

    def test_5_6R1_gh_help(self):
        self.expect_empty("gh --help")

    def test_5_6R1_gh_version_extra_arg(self):
        self.expect_empty("gh --version --json")

    def test_5_6R1_reads_allowed_with_disarmed_record(self):
        self.fx.set_record("pr", overrides={"state": "disarmed"})
        self.expect_allow("gh --version")

    def test_5_6R1_reads_rejected_with_closed_record(self):
        self.fx.set_record("pr", overrides={"state": "closed"})
        self.expect_empty("gh --version")

    def test_5_6R1_reads_rejected_without_record(self):
        os.unlink(self.fx.record_path)
        self.expect_empty("gh --version")

    def test_5_6R4_reads_rejected_without_record(self):
        os.unlink(self.fx.record_path)
        self.expect_empty(f"cat {self.fx.file}")

    # 5.6 R2 ------------------------------------------------------------------
    def test_5_6R2_config_local_get_user_name_ok(self):
        self.expect_allow("git config --local --get user.name")

    def test_5_6R2_config_get_user_email_ok(self):
        self.expect_allow("git config --get user.email")

    def test_5_6R2_config_with_C_root_ok(self):
        self.expect_allow(f"git -C {self.fx.proj} config --local --get user.name")

    def test_5_6R2_config_set_value(self):
        self.expect_empty("git config --local user.name X")

    def test_5_6R2_config_global_get(self):
        self.expect_empty("git config --global --get user.name")

    def test_5_6R2_config_list(self):
        self.expect_empty("git config --list")
        self.expect_empty("git config -l")

    def test_5_6R2_config_other_key(self):
        self.expect_empty("git config --get core.editor")

    def test_5_6R2_config_file_flag(self):
        self.expect_empty("git config --file /tmp/x --get user.name")

    # 5.6 R3 ------------------------------------------------------------------
    def test_5_6R3_grep_count_ok(self):
        self.expect_allow(f"grep -c git {self.fx.file}")

    def test_5_6R3_grep_recursive_dir_ok(self):
        self.expect_allow(f"grep -rn pat {self.fx.subdir}")

    def test_5_6R3_grep_e_pattern_and_context_ok(self):
        self.expect_allow(f"grep -n -A 2 -e pat -e other -- {self.fx.file}")

    def test_5_6R3_grep_goroot_file_ok(self):
        self.expect_allow(f"grep -n Parse {self.fx.time_file}")

    def test_5_6R3_grep_pattern_file_flag(self):
        self.expect_empty(f"grep -f {self.fx.file} {self.fx.file}")

    def test_5_6R3_grep_recursive_root(self):
        self.expect_empty("grep -r pat /")

    def test_5_6R3_grep_recursive_on_file_operand(self):
        self.expect_empty(f"grep -r pat {self.fx.file}")

    def test_5_6R3_grep_recursive_over_whole_root(self):
        self.expect_empty(f"grep -r pat {self.fx.proj}")
        self.expect_empty(f"grep -r pat {self.fx.goroot}/src")

    def test_5_6R3_grep_nested_git_under_goroot(self):
        self.expect_empty(f"grep -c core {self.fx.goroot}/src/.git/config")

    def test_5_6R3_grep_hardlinked_operand(self):
        other = os.path.join(self.fx.proj, "hardlink.txt")
        os.link(self.fx.file, other)
        self.expect_empty(f"grep -c git {other}")

    def test_5_6R3_grep_outside_roots(self):
        self.expect_empty("grep -c pat /home/claude/.claude/settings.json")

    def test_5_6R3_grep_relative_git_config(self):
        self.expect_empty("grep -c pat .git/config")

    def test_5_6R3_grep_absolute_git_config(self):
        self.expect_empty(f"grep -c pat {self.fx.proj}/.git/config")

    def test_5_6R3_grep_git_head_ok(self):
        self.expect_allow(f"grep -c ref {self.fx.proj}/.git/HEAD")

    def test_5_6R3_grep_include_flag(self):
        self.expect_empty(f"grep -r --include=*.go pat {self.fx.subdir}")

    def test_5_6R3_grep_perl_flag(self):
        self.expect_empty(f"grep -P pat {self.fx.file}")

    def test_5_6R3_grep_no_file(self):
        self.expect_empty("grep -c pat")

    def test_5_6R3_grep_context_not_integer(self):
        self.expect_empty(f"grep -A x pat {self.fx.file}")

    # 5.6 R4 ------------------------------------------------------------------
    def test_5_6R4_cat_goroot_time_ok(self):
        self.expect_allow(f"cat {self.fx.time_file}")

    def test_5_6R4_cat_n_ok(self):
        self.expect_allow(f"cat -n {self.fx.file}")

    def test_5_6R4_cat_etc_passwd(self):
        self.expect_empty("cat /etc/passwd")

    def test_5_6R4_cat_directory(self):
        self.expect_empty(f"cat {self.fx.subdir}")

    def test_5_6R4_cat_redirect(self):
        self.expect_empty(f"cat {self.fx.file} > g")

    def test_5_6R4_cat_leading_assignment(self):
        self.expect_empty(f"FOO=1 cat {self.fx.file}")

    def test_5_6R4_head_n_ok(self):
        self.expect_allow(f"head -n 20 {self.fx.file}")

    def test_5_6R4_tail_dash_n_ok(self):
        self.expect_allow(f"tail -5 {self.fx.file}")

    def test_5_6R4_head_other_flag(self):
        self.expect_empty(f"head -q -n 20 {self.fx.file}")

    def test_5_6R4_wc_l_ok(self):
        self.expect_allow(f"wc -l {self.fx.file}")

    def test_5_6R4_stat_format_ok(self):
        self.expect_allow(f"stat -c %s {self.fx.file}")

    def test_5_6R4_stat_follow_symlink_flag(self):
        self.expect_empty(f"stat -L -c %s {self.fx.file}")

    def test_5_6R4_ls_la_dir_ok(self):
        self.expect_allow(f"ls -la {self.fx.proj}")

    def test_5_6R4_ls_root_itself_ok(self):
        self.expect_allow(f"ls -1 {self.fx.goroot}/src")

    def test_5_6R4_ls_outside_roots(self):
        self.expect_empty(f"ls -la {self.fx.tmp}")

    def test_5_6R4_ls_git_dir(self):
        self.expect_empty(f"ls -la {self.fx.proj}/.git")

    def test_5_6R4_sha256sum_ok(self):
        self.expect_allow(f"sha256sum {self.fx.file}")

    def test_5_6R4_sha256sum_with_flag(self):
        self.expect_empty(f"sha256sum -c {self.fx.file}")

    def test_5_6R4_operand_with_symlink_component(self):
        link = os.path.join(self.fx.proj, "linkdir")
        os.symlink(self.fx.subdir, link)
        self.expect_empty(f"cat {link}/a.go")

    def test_5_6R4_nested_git_under_project(self):
        self.expect_empty(f"cat {self.fx.nested_git}/config")
        self.expect_empty(f"ls -la {self.fx.nested_git}")

    def test_5_6R4_cat_hardlinked_operand(self):
        other = os.path.join(self.fx.proj, "hardlink.txt")
        os.link(self.fx.file, other)
        self.expect_empty(f"cat {other}")

    # 5.6 R5 ------------------------------------------------------------------
    def test_5_6R5_sed_range_ok(self):
        self.expect_allow(f"sed -n '10,20p' {self.fx.file}")

    def test_5_6R5_sed_single_line_ok(self):
        self.expect_allow(f"sed -n 3p {self.fx.file}")

    def test_5_6R5_sed_write_command(self):
        self.expect_empty(f"sed -n '1w /tmp/x' {self.fx.file}")

    def test_5_6R5_sed_in_place(self):
        self.expect_empty(f"sed -i s/a/b/ {self.fx.file}")

    def test_5_6R5_sed_missing_n(self):
        self.expect_empty(f"sed 3p {self.fx.file}")

    def test_5_6R5_sed_two_files(self):
        self.expect_empty(f"sed -n 3p {self.fx.file} {self.fx.file}")

    # 5.6 R6 ------------------------------------------------------------------
    def test_5_6R6_log_with_C_root_ok(self):
        self.expect_allow(f"git -C {self.fx.proj} log --oneline -5")

    def test_5_6R6_log_format_ok(self):
        self.expect_allow("git log --format=%H -n 3 --no-merges")

    def test_5_6R6_log_path_after_dashdash_ok(self):
        self.expect_allow("git log --oneline -- internal/a.go")

    def test_5_6R6_log_c_flag(self):
        self.expect_empty("git log -c")

    def test_5_6R6_global_c_option(self):
        self.expect_empty("git -c user.name=x log")

    def test_5_6R6_global_c_option_before_status(self):
        self.expect_empty("git -c core.pager=cat status --porcelain")

    def test_5_6R6_log_output_flag(self):
        self.expect_empty("git log --output=/tmp/x")

    def test_5_6R6_diff_ext_diff(self):
        self.expect_empty("git diff --ext-diff")

    def test_5_6R6_paginate_option(self):
        self.expect_empty("git -p log")

    def test_5_6R6_git_dir_option(self):
        self.expect_empty("git --git-dir=/tmp/x log")

    def test_5_6R6_C_other_root(self):
        self.expect_empty("git -C /tmp/other log")

    def test_5_6R6_C_subdirectory_of_project(self):
        self.expect_empty(f"git -C {self.fx.subdir} log --oneline")

    def test_5_6R6_without_C_requires_project_cwd(self):
        self.expect_empty("git status --porcelain", overrides={"cwd": self.fx.tmp})
        self.expect_allow(f"git -C {self.fx.proj} status --porcelain", overrides={"cwd": self.fx.tmp})

    def test_5_6R6_repeated_flags(self):
        self.expect_empty("git log --oneline --oneline")
        self.expect_empty("git log -n 3 -n 5")
        self.expect_empty("git log --format=%H --format=%h")

    def test_5_6R6_grep_repeated_e_ok(self):
        self.expect_allow("git grep -c -e a -e b")

    def test_5_6R6_path_shaped_rev_outside_roots(self):
        self.expect_empty("git log /etc/passwd")
        self.expect_empty("git show /etc/passwd")
        self.expect_empty("git log ../../x")
        self.expect_empty("git rev-parse /etc/passwd")
        self.expect_empty(f"git log {self.fx.proj}/.git/config")

    def test_5_6R6_branch_like_rev_ok(self):
        self.expect_allow("git log --oneline origin/release/0.3")
        self.expect_allow("git log --oneline -- internal")

    def test_5_6R6_hash_object_relative_operand(self):
        self.expect_empty("git hash-object internal/a.go")

    def test_5_6R6_grep_count_ok(self):
        self.expect_allow("git grep -c pat")

    def test_5_6R6_grep_open_in_pager(self):
        self.expect_empty("git grep -O pat")

    def test_5_6R6_hash_object_ok(self):
        self.expect_allow(f"git hash-object {self.fx.file}")

    def test_5_6R6_hash_object_write(self):
        self.expect_empty(f"git hash-object -w {self.fx.file}")

    def test_5_6R6_status_porcelain_ok(self):
        self.expect_allow("git status --porcelain")

    def test_5_6R6_status_untracked_ok(self):
        self.expect_allow("git status --porcelain --untracked-files=all")

    def test_5_6R6_ls_remote_ok(self):
        self.expect_allow("git ls-remote origin release/0.3")

    def test_5_6R6_push(self):
        self.expect_empty("git push origin x")

    def test_5_6R6_rev_parse_ok(self):
        self.expect_allow("git rev-parse --verify HEAD")

    def test_5_6R6_rev_parse_brace_rev(self):
        self.expect_empty("git rev-parse origin/release/0.3^{tree}")

    def test_5_6R6_var_ok(self):
        self.expect_allow("git var GIT_AUTHOR_IDENT")

    def test_5_6R6_var_other(self):
        self.expect_empty("git var GIT_EDITOR")

    def test_5_6R6_show_stat_ok(self):
        self.expect_allow("git show --stat --format=%H HEAD")

    def test_5_6R6_show_rev_path_ok(self):
        self.expect_allow("git show HEAD:internal/a.go")

    def test_5_6R6_diff_check_ok(self):
        self.expect_allow("git diff --check")

    def test_5_6R6_branch_show_current_ok(self):
        self.expect_allow("git branch --show-current")

    def test_5_6R6_branch_delete(self):
        self.expect_empty("git branch -D x")

    def test_5_6R6_worktree_list_ok(self):
        self.expect_allow("git worktree list")

    def test_5_6R6_worktree_add(self):
        self.expect_empty("git worktree add /tmp/x")

    def test_5_6R6_check_ignore_ok(self):
        self.expect_allow("git check-ignore -q -- .scratch/x")

    def test_5_6R6_cat_file_ok(self):
        self.expect_allow("git cat-file -t HEAD")

    def test_5_6R6_describe_ok(self):
        self.expect_allow("git describe --tags --always HEAD")

    def test_5_6R6_remote_v_ok(self):
        self.expect_allow("git remote -v")

    def test_5_6R6_remote_add(self):
        self.expect_empty("git remote add x https://example.invalid/x")

    def test_5_6R6_ls_files_ok(self):
        self.expect_allow("git ls-files -s -- internal")

    def test_5_6R6_path_with_dotdot(self):
        self.expect_empty("git log --oneline -- ../other")

    def test_5_6R6_rev_starting_with_dash(self):
        self.expect_empty("git log --oneline -- -x")
        self.expect_empty("git show -x")

    def test_5_6R6_reads_allowed_with_disarmed_record(self):
        self.fx.set_record("pr", overrides={"state": "disarmed"})
        self.expect_allow("git status --porcelain")

    def test_5_6R6_reads_rejected_on_record_branch_mismatch(self):
        self.fx.set_record("pr", overrides={"branch": "fix/other"})
        self.expect_empty("git status --porcelain")

    # 5.7 everything else -----------------------------------------------------
    def test_5_7_sort_output_flag(self):
        self.expect_empty(f"sort -o out {self.fx.file}")

    def test_5_7_uniq_two_operands(self):
        self.expect_empty(f"uniq {self.fx.file} out")

    def test_5_7_go_env_write(self):
        self.expect_empty("go env -w X=1")

    def test_5_7_rg_pre(self):
        self.expect_empty("rg --pre cmd pat")

    def test_5_7_find_delete(self):
        self.expect_empty(f"find {self.fx.subdir} -delete")

    def test_5_7_python(self):
        self.expect_empty("python3 -c 1")

    def test_5_7_gh_pr_edit_not_governed(self):
        self.expect_empty(f"gh pr edit {PR} --repo {REPO} --body x")


def main():
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--guard", default=DEFAULT_GUARD)
    opts, rest = parser.parse_known_args()
    global GUARD
    GUARD = os.path.abspath(opts.guard)
    if not os.path.isfile(GUARD):
        sys.stderr.write(f"guard not found: {GUARD}\n")
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
