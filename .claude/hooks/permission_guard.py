#!/usr/bin/env python3
"""PermissionRequest guard for unattended Draft-PR work (s1h-contract-rev4, section 5).

Registered in `.claude/settings.local.json` as a PermissionRequest hook for the
Bash tool (section 4) and run as `python3 -I RUNTIME --expect-sha256 SHA`.

It answers ALLOW only for the exact grammar below; for everything else, and on
every error, it answers nothing, so Claude Code prompts exactly as it does
today. It never returns deny, never changes the tool input or the permissions,
never opens a network connection, and never writes anything.

  WRITE shapes (5.5; main controller only, task record `state: active`):
    A  gh pr create --draft --repo <REPO> --base release/0.3 --head <current branch>
       --title <TITLE> --body-file <snapshot under .scratch, hash in the record>
    B  gh pr comment <record pr> --repo <REPO> --body "<T1|T2|T3>"   (or --body-file)
    C  gh api --hostname github.com -X PATCH repos/<REPO>/pulls/<record pr> -F body=@<snapshot>
  READ forms (5.6; sub-agents too, record `active` or `disarmed`):
    R1 gh --version | gh version
    R2 git [-C <clone>] config [--local] --get user.name|user.email
    R3 grep (listed flags) PATTERN FILE... under ROOTS
    R4 cat/head/tail/wc/stat/sha256sum/ls (listed flags) FILE... under ROOTS
    R5 sed -n N[,M]p FILE under ROOTS
    R6 git [-C <clone>] <read subcommand> (listed flags) ...

Reads only `.git/HEAD`, the task record, the named body file and the two
executables it hashes; reads only `CLAUDE_PROJECT_DIR`, the value of `PATH` (for
the executable lookup) and the *names* of the environment variables listed below.
"""

import hashlib
import json
import os
import re
import shlex
import shutil
import stat
import sys

ALLOW_JSON = ('{"hookSpecificOutput": {"hookEventName": "PermissionRequest", '
              '"decision": {"behavior": "allow"}}}')

REPO = "github.com/KraineOpasen/bukerov-twitch-miner-go"
API_PULLS_PREFIX = "repos/KraineOpasen/bukerov-twitch-miner-go/pulls/"
BASE_BRANCH = "release/0.3"
ACCEPT_HEADER = "Accept: application/vnd.github+json"
TRIGGERS = frozenset((
    "@coderabbitai full review",
    "@codex review Full review of the entire PR. Review only; make no changes.",
    "@codex security review",
))
ENV_GH_NAMES = ("GH_HOST", "GH_REPO", "GH_CONFIG_DIR", "GH_ENTERPRISE_TOKEN",
                "GITHUB_ENTERPRISE_TOKEN", "GH_PAGER", "PAGER", "GH_FORCE_TTY",
                "BASH_FUNC_gh%%")
ENV_GIT_NAMES = ("GIT_DIR", "GIT_WORK_TREE", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM",
                 "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_EXTERNAL_DIFF",
                 "GIT_PAGER", "GIT_EXEC_PATH", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_ASKPASS",
                 "SSH_ASKPASS", "BASH_FUNC_git%%")
# shell-startup and option names: fail-closed for every form (rev12 5.4(b), 5.6)
ENV_SHELL_NAMES = ("BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS")
RECORD_REL = os.path.join(".scratch", "permission-guard", "task-record.json")
MAX_STDIN = 1024 * 1024
MAX_COMMAND = 8192
MAX_RECORD = 16 * 1024
MAX_BODY = 65536
MAX_TITLE = 200

BRANCH_RE = re.compile(r"^(fix|feat|chore|test|docs|ci|refactor|perf|build)/[A-Za-z0-9._-]+$")
REV_RE = re.compile(r"^[A-Za-z0-9._/^~-]{1,120}$")
FMT_RE = re.compile(r"^[%A-Za-z0-9 ()<>:,._-]{1,120}$")
HEX64_RE = re.compile(r"^[0-9a-f]{64}$")
HEX40_RE = re.compile(r"^[0-9a-f]{40}$")
POSINT_RE = re.compile(r"^[1-9][0-9]*$")
DASH_N_RE = re.compile(r"^-[1-9][0-9]*$")
SED_SCRIPT_RE = re.compile(r"^[0-9]+(,[0-9]+)?p$")
OPERATOR_RE = re.compile(r"^[();<>|&]+$")
ASSIGNMENT_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")
HEAD_RE = re.compile(r"^ref: refs/heads/(\S+)\n?$")
UNTRACKED_RE = re.compile(r"^[a-z]+$")
FORBIDDEN_RAW_CHARS = "`$\n\r\0"
GLOB_CHARS = "*?[{"
TRIGGER_WHITESPACE = " \t\r\n"
GIT_FORBIDDEN_SUBSTRINGS = ("--output", "--ext-diff", "--textconv", "-O",
                            "--open-files-in-pager", "--no-textconv=false")
READ_UTILITIES = ("cat", "head", "tail", "wc", "stat", "sha256sum", "ls")


class NoDecision(Exception):
    """Raised anywhere a check fails: the guard prints nothing."""


# --- small helpers ---------------------------------------------------------------

def sha256_file(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _reject_duplicate_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise NoDecision("duplicate key")
        result[key] = value
    return result


def loads_strict(data):
    try:
        value = json.loads(data.decode("utf-8"), object_pairs_hook=_reject_duplicate_keys)
    except (ValueError, UnicodeDecodeError) as exc:
        raise NoDecision("invalid json") from exc
    if not isinstance(value, dict):
        raise NoDecision("not an object")
    return value


def require(condition, why="check failed"):
    if not condition:
        raise NoDecision(why)


def is_str(value):
    return isinstance(value, str)


def normalize_trigger(text):
    """Collapse runs of ASCII whitespace (space, tab, CR, LF) to one space and strip the ends.
    Any other Unicode whitespace is not a separator the trigger texts may carry."""
    for ch in text:
        if ch.isspace() and ch not in TRIGGER_WHITESPACE:
            raise NoDecision("unusual whitespace")
    return " ".join(text.split())


def read_regular_file(path, limit):
    """Open without following a symlink, refuse anything but a regular file, read at most limit+1 bytes."""
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
    try:
        info = os.fstat(fd)
        require(stat.S_ISREG(info.st_mode), "not a regular file")
        require(info.st_size <= limit, "too large")
        chunks = []
        remaining = limit + 1
        while remaining > 0:
            chunk = os.read(fd, min(remaining, 1024 * 1024))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        data = b"".join(chunks)
        require(len(data) <= limit, "too large")
        return data, info
    finally:
        os.close(fd)


def lstat_components(path, must_exist):
    """lstat every component of an absolute path; a symlink anywhere = NoDecision.
    Returns True when the whole path exists."""
    current = "/"
    for part in path.split("/"):
        if not part:
            continue
        current = os.path.join(current, part)
        try:
            info = os.lstat(current)
        except FileNotFoundError:
            require(not must_exist, "missing path")
            return False
        require(not stat.S_ISLNK(info.st_mode), "symlink component")
    return True


# --- context: project, task record, HEAD ----------------------------------------------

class Context:
    def __init__(self, payload, environ):
        self.payload = payload
        self.environ = environ
        project = environ.get("CLAUDE_PROJECT_DIR")
        require(is_str(project) and os.path.isabs(project), "no project dir")
        self.project = os.path.realpath(project)
        require(os.path.isdir(self.project), "project dir missing")
        self.head_branch = self._read_head_branch()
        self.record = self._load_record()
        goroot = os.path.realpath(self.record["goroot"])
        self.roots = (self.project, os.path.join(goroot, "src"))

    def _read_head_branch(self):
        head = os.path.join(self.project, ".git", "HEAD")
        info = os.lstat(head)
        require(stat.S_ISREG(info.st_mode), "HEAD not a file")
        data, _ = read_regular_file(head, 4096)
        match = HEAD_RE.fullmatch(data.decode("utf-8"))
        require(match is not None, "detached or unusual HEAD")
        return match.group(1)

    def _load_record(self):
        path = os.path.join(self.project, RECORD_REL)
        info = os.lstat(path)
        require(stat.S_ISREG(info.st_mode), "record not a regular file")
        require(info.st_size <= MAX_RECORD, "record too large")
        data, _ = read_regular_file(path, MAX_RECORD)
        record = loads_strict(data)
        require(is_str(record.get("contract")) and record["contract"] != "", "no contract")
        require(record.get("state") in ("active", "disarmed"), "record not active/disarmed")
        require(record.get("repo") == REPO, "foreign repo")
        require(record.get("branch") == self.head_branch, "foreign branch")
        require(is_str(record.get("evidence_root")), "no evidence root")
        for key in ("gh_path", "git_path", "goroot"):
            require(is_str(record.get(key)) and os.path.isabs(record[key]), "bad path in record")
        for key in ("gh_sha256", "git_sha256"):
            require(is_str(record.get(key)) and HEX64_RE.fullmatch(record[key]), "bad hash in record")
        pr = record.get("pr")
        if pr is not None:
            require(isinstance(pr, int) and not isinstance(pr, bool) and pr > 0, "bad pr")
            head = record.get("pr_head")
            require(is_str(head) and HEX40_RE.fullmatch(head), "bad pr_head")
        body = record.get("body_sha256")
        if body is not None:
            require(is_str(body) and HEX64_RE.fullmatch(body), "bad body hash")
        return record

    # bindings ---------------------------------------------------------------------
    def require_write_context(self):
        require("agent_id" not in self.payload, "subagent")
        require(self.payload.get("permission_mode") in ("default", "auto"), "mode")
        cwd = self.payload.get("cwd")
        require(is_str(cwd) and os.path.realpath(cwd) == self.project, "cwd")
        self.require_env_absent(ENV_GH_NAMES + ENV_GIT_NAMES + ENV_SHELL_NAMES)
        require(self.record["state"] == "active", "record not active")

    def require_env_absent(self, names):
        for name in names:
            require(name not in self.environ, "environment override present")

    def require_executable(self, name):
        found = shutil.which(name, path=self.environ.get("PATH"))
        require(found is not None and found == self.record[name + "_path"], "executable path")
        require(sha256_file(found) == self.record[name + "_sha256"], "executable hash")

    def root_of(self, real):
        for root in self.roots:
            if real == root or real.startswith(root + "/"):
                return root
        raise NoDecision("outside roots")

    def resolve_operand(self, token, kind="any", must_exist=True, allow_relative=False):
        """A file-system operand: absolute (or repository-relative when allowed), no symlink
        component, under ROOTS, never under .git/ except .git/HEAD; kind = file|dir|any."""
        require(is_str(token) and token != "" and not token.startswith("-"), "bad operand")
        if os.path.isabs(token):
            path = token
        else:
            require(allow_relative, "relative operand")
            parts = token.split("/")
            require(".." not in parts and parts[0] != "", "relative escape")
            path = os.path.join(self.project, token)
        exists = lstat_components(path, must_exist)
        real = os.path.realpath(path)
        root = self.root_of(real)
        rel = os.path.relpath(real, root)
        if ".git" in rel.split("/"):
            # a .git directory at any depth under either root; only the project's own HEAD is readable
            require(root == self.project and rel == ".git/HEAD", "inside .git")
        if exists:
            info = os.lstat(path)
            if kind == "file":
                require(stat.S_ISREG(info.st_mode), "not a file")
            elif kind == "dir":
                require(stat.S_ISDIR(info.st_mode), "not a directory")
            else:
                require(stat.S_ISREG(info.st_mode) or stat.S_ISDIR(info.st_mode), "odd operand")
            if stat.S_ISREG(info.st_mode):
                require(info.st_nlink == 1, "hardlinked operand")
        else:
            require(kind == "any", "missing operand")
        return real

    def read_body_file(self, token):
        """5.4(f): absolute, no symlink component, under .scratch, regular, nlink 1, own uid,
        at most MAX_BODY bytes. Returns the bytes."""
        require(is_str(token) and os.path.isabs(token), "body path")
        lstat_components(token, must_exist=True)
        real = os.path.realpath(token)
        scratch = os.path.realpath(os.path.join(self.project, ".scratch"))
        require(real.startswith(scratch + "/"), "body outside .scratch")
        info = os.lstat(token)
        require(stat.S_ISREG(info.st_mode), "body not a regular file")
        require(info.st_nlink == 1, "body hardlinked")
        require(info.st_uid == os.getuid(), "body owner")
        require(info.st_size <= MAX_BODY, "body too large")
        data, finfo = read_regular_file(token, MAX_BODY)
        require(finfo.st_ino == info.st_ino and finfo.st_dev == info.st_dev, "body changed")
        require(finfo.st_nlink == 1, "body hardlinked")
        return data

    def require_body_snapshot(self, token):
        expected = self.record.get("body_sha256")
        require(is_str(expected), "no body hash in record")
        data = self.read_body_file(token)
        require(hashlib.sha256(data).hexdigest() == expected, "body hash")

    def require_body_trigger(self, token):
        data = self.read_body_file(token)
        try:
            text = data.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise NoDecision("body encoding") from exc
        require(normalize_trigger(text) in TRIGGERS, "body not a trigger")

    def record_pr(self):
        pr = self.record.get("pr")
        require(isinstance(pr, int) and not isinstance(pr, bool) and pr > 0, "no pr in record")
        return pr


# --- flag parsing ---------------------------------------------------------------------

def parse_flags(args, spec):
    """spec: {long: (short_or_None, takes_value)}. Accepts --flag value, --flag=value and
    the listed short flags with a separate value. Unknown, repeated or clustered flags and a
    missing value raise. Returns (flags, positionals)."""
    short_map = {short: long for long, (short, _) in spec.items() if short}
    flags = {}
    positionals = []
    i = 0
    while i < len(args):
        token = args[i]
        if token.startswith("--"):
            name, has_eq, value = token[2:].partition("=")
            require(name in spec, "unknown flag")
            if spec[name][1]:
                if not has_eq:
                    i += 1
                    require(i < len(args), "missing value")
                    value = args[i]
            else:
                require(not has_eq, "value on boolean flag")
                value = True
        elif token.startswith("-"):
            require(len(token) == 2 and token[1] in short_map, "unknown short flag")
            name = short_map[token[1]]
            if spec[name][1]:
                i += 1
                require(i < len(args), "missing value")
                value = args[i]
            else:
                value = True
        else:
            positionals.append(token)
            i += 1
            continue
        require(name not in flags, "repeated flag")
        flags[name] = value
        i += 1
    return flags, positionals


# --- gh shapes ------------------------------------------------------------------------

def match_gh(tokens, ctx):
    for token in tokens:
        require("~" not in token, "tilde in gh token")  # bash expands ~ after : and = inside words
    args = tokens[1:]
    if args == ["--version"] or args == ["version"]:
        ctx.require_env_absent(ENV_GH_NAMES + ENV_SHELL_NAMES)
        ctx.require_executable("gh")
        return True
    if len(args) >= 2 and args[0] == "pr" and args[1] == "create":
        return match_create(args[2:], ctx)
    if len(args) >= 2 and args[0] == "pr" and args[1] == "comment":
        return match_comment(args[2:], ctx)
    if len(args) >= 1 and args[0] == "api":
        return match_patch(args[1:], ctx)
    return False


def match_create(args, ctx):
    spec = {"draft": ("d", False), "repo": ("R", True), "base": ("B", True),
            "head": ("H", True), "title": ("t", True), "body-file": ("F", True)}
    flags, positionals = parse_flags(args, spec)
    require(not positionals, "positional operand")
    for name in spec:
        require(name in flags, "missing flag")
    require(flags["repo"] == REPO, "repo")
    require(flags["base"] == BASE_BRANCH, "base")
    head = flags["head"]
    require(BRANCH_RE.fullmatch(head) is not None and head == ctx.head_branch, "head")
    title = flags["title"]
    require(title != "" and len(title) <= MAX_TITLE and title.isprintable(), "title")
    ctx.require_write_context()
    ctx.require_executable("gh")
    require(ctx.record.get("pr") is None, "pr already recorded")
    ctx.require_body_snapshot(flags["body-file"])
    return True


def match_comment(args, ctx):
    spec = {"repo": ("R", True), "body": ("b", True), "body-file": ("F", True)}
    flags, positionals = parse_flags(args, spec)
    require(len(positionals) == 1 and POSINT_RE.fullmatch(positionals[0]) is not None, "pr number")
    require(flags.get("repo") == REPO, "repo")
    require(("body" in flags) != ("body-file" in flags), "exactly one body source")
    ctx.require_write_context()
    ctx.require_executable("gh")
    require(int(positionals[0]) == ctx.record_pr(), "pr mismatch")
    if "body" in flags:
        require(normalize_trigger(flags["body"]) in TRIGGERS, "body not a trigger")
    else:
        ctx.require_body_trigger(flags["body-file"])
    return True


def match_patch(args, ctx):
    spec = {"hostname": (None, True), "method": ("X", True), "field": ("F", True),
            "header": ("H", True)}
    flags, positionals = parse_flags(args, spec)
    require(len(positionals) == 1, "endpoint")
    require(flags.get("hostname") == "github.com", "hostname")
    require(flags.get("method") == "PATCH", "method")
    field = flags.get("field")
    require(is_str(field) and field.startswith("body=@"), "field")
    if "header" in flags:
        require(flags["header"] == ACCEPT_HEADER, "header")
    ctx.require_write_context()
    ctx.require_executable("gh")
    require(positionals[0] == API_PULLS_PREFIX + str(ctx.record_pr()), "endpoint")
    ctx.require_body_snapshot(field[len("body=@"):])
    return True


# --- git read forms -------------------------------------------------------------------

def parse_git_args(rest, flags0, flags_val, prefixes, dash_n, allow_dashdash):
    flags = []
    values = {}
    positionals = []
    after = None
    i = 0
    while i < len(rest):
        token = rest[i]
        if after is not None:
            after.append(token)
        elif token == "--":
            require(allow_dashdash, "-- not allowed")
            after = []
        elif token.startswith("-"):
            if token in flags0:
                require(token not in flags, "repeated flag")
                flags.append(token)
            elif token in flags_val:
                i += 1
                require(i < len(rest), "missing value")
                require(token == "-e" or token not in values, "repeated flag")
                values.setdefault(token, []).append(rest[i])
            else:
                for prefix in prefixes:
                    if token.startswith(prefix):
                        require(prefix not in values, "repeated flag")
                        values.setdefault(prefix, []).append(token[len(prefix):])
                        break
                else:
                    require(dash_n and DASH_N_RE.fullmatch(token) is not None, "unknown git option")
                    require("-n" not in values, "repeated flag")
                    values.setdefault("-n", []).append(token[1:])
        else:
            positionals.append(token)
        i += 1
    return flags, values, positionals, after


def check_git_values(values):
    for key, items in values.items():
        for item in items:
            if key in ("-n", "--max-count="):
                require(POSINT_RE.fullmatch(item) is not None, "count")
            elif key in ("--format=", "--pretty="):
                require(FMT_RE.fullmatch(item) is not None, "format")
            elif key == "--untracked-files=":
                require(UNTRACKED_RE.fullmatch(item) is not None, "untracked value")
            elif key == "-e":
                require(item != "", "empty pattern")
            else:
                raise NoDecision("unchecked git value")


def check_rev(ctx, token):
    """A revision: REV_RE shape; a path-shaped one (containing / or starting with .) must also
    resolve as a repository path under ROOTS, so a rev-looking token never names a file elsewhere."""
    require(REV_RE.fullmatch(token) is not None and not token.startswith("-"), "rev")
    if "/" in token or token.startswith("."):
        ctx.resolve_operand(token, kind="any", must_exist=False, allow_relative=True)


def check_rev_or_path(ctx, token):
    require(token != "" and not token.startswith("-"), "operand")
    if ":" in token:
        rev, _, path = token.partition(":")
        check_rev(ctx, rev)
        ctx.resolve_operand(path, kind="any", must_exist=False, allow_relative=True)
        return
    check_rev(ctx, token)


def check_paths(ctx, paths):
    for path in paths:
        ctx.resolve_operand(path, kind="any", must_exist=False, allow_relative=True)


def git_status(rest, ctx):
    _, values, positionals, _ = parse_git_args(
        rest, {"--porcelain", "--short", "-s", "-b"}, set(), ("--untracked-files=",), False, False)
    check_git_values(values)
    require(not positionals, "status operand")


def git_log(rest, ctx):
    _, values, positionals, after = parse_git_args(
        rest, {"--oneline", "--stat", "--name-only", "--name-status", "--no-merges",
               "--first-parent", "--follow"}, {"-n"},
        ("--max-count=", "--format=", "--pretty="), True, True)
    check_git_values(values)
    for token in positionals:
        check_rev_or_path(ctx, token)
    check_paths(ctx, after or [])


def git_show(rest, ctx):
    _, values, positionals, after = parse_git_args(
        rest, {"--stat", "--name-only", "--name-status", "-s", "--no-patch"}, set(),
        ("--format=", "--pretty="), False, True)
    check_git_values(values)
    for token in positionals:
        check_rev_or_path(ctx, token)
    check_paths(ctx, after or [])


def git_diff(rest, ctx):
    _, _, positionals, after = parse_git_args(
        rest, {"--check", "--stat", "--name-only", "--name-status", "--cached", "--staged"},
        set(), (), False, True)
    for token in positionals:
        check_rev_or_path(ctx, token)
    check_paths(ctx, after or [])


def git_rev_parse(rest, ctx):
    _, _, positionals, _ = parse_git_args(
        rest, {"--verify", "--short", "--abbrev-ref", "--show-toplevel"}, set(), (), False, False)
    for token in positionals:
        check_rev(ctx, token)


def git_ls_files(rest, ctx):
    _, _, positionals, after = parse_git_args(
        rest, {"-s", "--stage", "--error-unmatch"}, set(), (), False, True)
    check_paths(ctx, positionals + (after or []))


def git_cat_file(rest, ctx):
    _, _, positionals, _ = parse_git_args(rest, {"-e", "-p", "-t", "-s"}, set(), (), False, False)
    require(len(positionals) == 1, "one object")
    check_rev_or_path(ctx, positionals[0])


def git_hash_object(rest, ctx):
    _, _, positionals, _ = parse_git_args(rest, set(), set(), (), False, False)
    require(len(positionals) >= 1, "file operand")
    for token in positionals:
        # absolute only: git would resolve a relative operand against its own cwd, not the project
        ctx.resolve_operand(token, kind="file", must_exist=True, allow_relative=False)


def git_check_ignore(rest, ctx):
    _, _, positionals, after = parse_git_args(rest, {"-q"}, set(), (), False, True)
    paths = positionals + (after or [])
    require(len(paths) >= 1, "path operand")
    check_paths(ctx, paths)


def git_var(rest, ctx):
    require(rest in (["GIT_AUTHOR_IDENT"], ["GIT_COMMITTER_IDENT"]), "var name")


def git_branch(rest, ctx):
    _, _, positionals, _ = parse_git_args(rest, {"--list", "--show-current"}, set(), (), False, False)
    require(not positionals, "branch operand")


def git_remote(rest, ctx):
    _, _, positionals, _ = parse_git_args(rest, {"-v"}, set(), (), False, False)
    require(not positionals, "remote operand")


def git_worktree(rest, ctx):
    require(rest == ["list"], "worktree list only")


def git_describe(rest, ctx):
    _, _, positionals, _ = parse_git_args(rest, {"--tags", "--always"}, set(), (), False, False)
    require(len(positionals) <= 1, "describe operand")
    for token in positionals:
        check_rev(ctx, token)


def git_grep(rest, ctx):
    _, values, positionals, after = parse_git_args(
        rest, {"-n", "-c", "-l", "-i", "-E", "-F", "-w"}, {"-e"}, (), False, True)
    check_git_values(values)
    if "-e" not in values:
        require(len(positionals) >= 1, "pattern")
        positionals = positionals[1:]
    for token in positionals:
        check_rev_or_path(ctx, token)
    check_paths(ctx, after or [])


def git_ls_remote(rest, ctx):
    _, _, positionals, _ = parse_git_args(rest, {"--heads", "--tags"}, set(), (), False, False)
    require(len(positionals) >= 1 and positionals[0] == "origin", "origin only")
    for token in positionals[1:]:
        require(REV_RE.fullmatch(token) is not None and not token.startswith("-"), "refspec")


GIT_SUBCOMMANDS = {
    "status": git_status, "log": git_log, "show": git_show, "diff": git_diff,
    "rev-parse": git_rev_parse, "ls-files": git_ls_files, "cat-file": git_cat_file,
    "hash-object": git_hash_object, "check-ignore": git_check_ignore, "var": git_var,
    "branch": git_branch, "remote": git_remote, "worktree": git_worktree,
    "describe": git_describe, "grep": git_grep, "ls-remote": git_ls_remote,
}


def match_git(tokens, ctx):
    args = tokens[1:]
    if args and args[0] == "-C":
        require(len(args) >= 2, "-C value")
        require(ctx.resolve_operand(args[1], kind="dir", must_exist=True) == ctx.project, "-C root")
        args = args[2:]
    else:
        # without -C git works on the shell's cwd: bind it to the project
        cwd = ctx.payload.get("cwd")
        require(is_str(cwd) and os.path.realpath(cwd) == ctx.project, "git cwd")
    require(len(args) >= 1 and not args[0].startswith("-"), "git global option")
    for token in args:
        for forbidden in GIT_FORBIDDEN_SUBSTRINGS:
            require(forbidden not in token, "forbidden git token")
    sub, rest = args[0], args[1:]
    if sub == "config":
        require(rest in (["--local", "--get", "user.name"], ["--local", "--get", "user.email"],
                         ["--get", "user.name"], ["--get", "user.email"]), "config form")
    else:
        handler = GIT_SUBCOMMANDS.get(sub)
        require(handler is not None, "git subcommand")
        handler(rest, ctx)
    ctx.require_env_absent(ENV_GIT_NAMES + ENV_SHELL_NAMES)
    ctx.require_executable("git")
    return True


# --- grep, read utilities, sed --------------------------------------------------------

def parse_short_clusters(rest, flags0, flags_val, allow_dash_n, allow_dashdash, long_val=()):
    """POSIX-style clusters (-rn); a value-taking letter must end its cluster and takes the
    next token. Returns (letters, values, operands)."""
    letters = []
    values = []
    operands = []
    after = False
    i = 0
    while i < len(rest):
        token = rest[i]
        if after or not token.startswith("-") or token == "-":
            require(token != "-", "stdin operand")
            operands.append(token)
        elif token == "--":
            require(allow_dashdash, "-- not allowed")
            after = True
        elif token.startswith("--"):
            name, has_eq, value = token.partition("=")
            require(name in long_val, "long option")
            if not has_eq:
                i += 1
                require(i < len(rest), "missing value")
                value = rest[i]
            values.append((name, value))
        elif allow_dash_n and DASH_N_RE.fullmatch(token):
            values.append(("-n", token[1:]))
        else:
            cluster = token[1:]
            for position, letter in enumerate(cluster):
                if letter in flags0:
                    letters.append(letter)
                elif letter in flags_val:
                    require(position == len(cluster) - 1, "value flag inside cluster")
                    i += 1
                    require(i < len(rest), "missing value")
                    values.append((letter, rest[i]))
                else:
                    raise NoDecision("unknown flag")
        i += 1
    return letters, values, operands


def match_grep(tokens, ctx):
    letters, values, operands = parse_short_clusters(
        tokens[1:], set("nclLiEFwxohHqsr"), set("ABCme"), False, True)
    for key, value in values:
        if key in "ABCm":
            require(POSINT_RE.fullmatch(value) is not None, "context count")
        else:
            require(value != "", "empty pattern")
    if not any(key == "e" for key, _ in values):
        require(len(operands) >= 1, "pattern")
        operands = operands[1:]
    require(len(operands) >= 1, "file operand")
    kind = "dir" if "r" in letters else "file"
    for operand in operands:
        real = ctx.resolve_operand(operand, kind=kind, must_exist=True)
        if kind == "dir":
            require(real not in ctx.roots, "recursive grep over a whole root")
    return True


def match_read_utility(tokens, ctx):
    name = tokens[0]
    rest = tokens[1:]
    if name == "cat":
        _, _, operands = parse_short_clusters(rest, set("nA"), set(), False, False)
        kind = "file"
    elif name in ("head", "tail"):
        _, values, operands = parse_short_clusters(rest, set(), set("nc"), True, False)
        for _, value in values:
            require(POSINT_RE.fullmatch(value) is not None, "count")
        kind = "file"
    elif name == "wc":
        _, _, operands = parse_short_clusters(rest, set("lcwmL"), set(), False, False)
        kind = "file"
    elif name == "stat":
        _, values, operands = parse_short_clusters(rest, set(), {"c"}, False, False, ("--printf",))
        for _, value in values:
            require(value != "", "empty format")
        kind = "file"
    elif name == "sha256sum":
        _, _, operands = parse_short_clusters(rest, set(), set(), False, False)
        kind = "file"
    else:  # ls
        _, _, operands = parse_short_clusters(rest, set("laA1dhtrS"), set(), False, True)
        kind = "any"
    require(len(operands) >= 1, "operand")
    for operand in operands:
        ctx.resolve_operand(operand, kind=kind, must_exist=True)
    return True


def match_sed(tokens, ctx):
    require(len(tokens) == 4 and tokens[1] == "-n", "sed form")
    require(SED_SCRIPT_RE.fullmatch(tokens[2]) is not None, "sed script")
    ctx.resolve_operand(tokens[3], kind="file", must_exist=True)
    return True


# --- top level -----------------------------------------------------------------------

def tokenize(command):
    lexer = shlex.shlex(command, posix=True, punctuation_chars=True)
    lexer.whitespace_split = True
    lexer.commenters = ""
    try:
        tokens = list(lexer)
    except ValueError as exc:
        raise NoDecision("unparsable") from exc
    require(len(tokens) >= 1, "empty command")
    for token in tokens:
        require(OPERATOR_RE.fullmatch(token) is None and not token.startswith("#"), "operator")
        require(not any(ch in token for ch in GLOB_CHARS) and not token.startswith("~"), "glob")
    require(ASSIGNMENT_RE.match(tokens[0]) is None, "leading assignment")
    return tokens


def decide(argv, stdin, environ):
    require(len(argv) == 2 and argv[0] == "--expect-sha256", "argv")
    expected = argv[1]
    require(HEX64_RE.fullmatch(expected) is not None, "argv hash")
    require(sha256_file(os.path.abspath(__file__)) == expected, "self hash")
    data = stdin.read(MAX_STDIN + 1)
    require(len(data) <= MAX_STDIN, "stdin too large")
    payload = loads_strict(data)
    if payload.get("hook_event_name") != "PermissionRequest" or payload.get("tool_name") != "Bash":
        return False
    tool_input = payload.get("tool_input")
    require(isinstance(tool_input, dict), "tool_input")
    command = tool_input.get("command")
    require(is_str(command) and len(command) <= MAX_COMMAND, "command")
    require(command.isprintable() and not any(ch in command for ch in FORBIDDEN_RAW_CHARS), "raw")
    tokens = tokenize(command)
    ctx = Context(payload, environ)
    ctx.require_env_absent(ENV_SHELL_NAMES)  # every form, reads included
    program = tokens[0]
    if program == "gh":
        return match_gh(tokens, ctx)
    if program == "git":
        return match_git(tokens, ctx)
    if program == "grep":
        return match_grep(tokens, ctx)
    if program in READ_UTILITIES:
        return match_read_utility(tokens, ctx)
    if program == "sed":
        return match_sed(tokens, ctx)
    return False


def main():
    allow = False
    try:
        allow = bool(decide(sys.argv[1:], sys.stdin.buffer, os.environ))
    except BaseException:  # any error at all, an interrupt included = no decision
        allow = False
    if allow:
        try:
            sys.stdout.write(ALLOW_JSON + "\n")
            sys.stdout.flush()
        except Exception:
            pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
