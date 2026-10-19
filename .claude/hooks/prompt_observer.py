#!/usr/bin/env python3
"""Prompt observer and guard explain CLI (guard-g1-contract-rev3, G1; README-prompt-observer.md).

Two entry modes, chosen by argv:

  Hook mode — registered by install_prompt_observer.py as a Notification hook (matcher
  permission_prompt) and run as `python3 -I RUNTIME --expect-sha256 SHA`.
    Log-only. For a permission prompt that is waiting for the owner it appends ONE JSON line to
    <project>/.scratch/prompt-observer/prompts.jsonl: the SHAPE of the pending tool call (program,
    subcommand, flag names, counts) and what the installed guard answers for it. It prints nothing,
    exits 0 on every path and cannot influence any decision (a Notification hook has no decision).

  Explain mode — offline, for one Bash command:
      python3 -I prompt_observer.py explain --command-file PATH [--project DIR] [--cwd DIR]
          [--permission-mode MODE] [--subagent] [--test-root DIR]
    Prints one JSON line {"decision", "reason", "guard_sha256", "shape", "scope", "switch"} and
    writes nothing. No option names a guard file or a hash. "scope" is always "guard_function"
    (what the registered guard function answers IF Claude Code invokes it); "switch" names the
    readable settings file that decided disableAllHooks ("local", "project" or "unset").

Neither mode emits a value of a command: every string that leaves the process is a member of a
constant set below, a single-character short flag, "-N", "-?", "?", "?op", "OTHER", "mcp", a fixed
field value, or a guard reason that passes sanitize().

The guard is executed only when it is the one registered PermissionRequest runtime copy (matcher
"Bash", hook type "command", no guard-shaped command anywhere else under PermissionRequest, and
disableAllHooks not true in the local or project settings) at its installer-defined path
(HOOKS_ROOT/permission-guard/<HASH12>/permission_guard.py), owned by the
effective uid and not group/other writable, whose bytes hash to the registered SHA-256 AND whose
SHA-256 is in KNOWN_GUARD_SHA256. Its bytes are read once, hashed, compiled and executed from
memory: no import system, no sys.modules entry, no bytecode file.
"""

import fcntl
import hashlib
import io
import json
import os
import re
import shlex
import stat
import sys
import time
import types

# The installer-defined root of the runtime hook copies (only explain's --test-root replaces it).
HOOKS_ROOT = "/home/claude/.claude/hooks"

# Reviewed guard bytes the observer and explain may execute. G2 adds its promoted hash in its own PR
# (the owner then reinstalls the observer).
KNOWN_GUARD_SHA256 = frozenset((
    "5b16b120defa408b80047dd144c0ee1cc1dfbee519c9f51b2b92a6d322ffc0d1",
))

# --- the constant sets (3.0); nothing outside them is ever emitted ------------------------------

PROGRAMS = frozenset((
    "git", "gh", "go", "gofmt", "golangci-lint", "make", "gcc", "python3", "python", "pip", "grep",
    "cat", "head", "tail", "wc", "stat", "sha256sum", "ls", "sed", "awk", "find", "sort", "uniq",
    "diff", "cut", "tr", "jq", "date", "mkdir", "cp", "mv", "rm", "ln", "touch", "chmod", "readlink",
    "realpath", "tee", "echo", "printf", "test", "true", "false", "sleep", "kill", "pkill", "ps",
    "whoami", "id", "pwd", "cd", "env", "command", "exec", "eval", "timeout", "nohup", "nice",
    "xargs", "time", "stdbuf", "builtin", "bash", "sh", "zsh", "dash", "sudo", "doas", "ssh", "scp",
    "rsync", "curl", "wget", "tar", "unzip", "zip", "tmux", "systemctl", "journalctl", "docker",
    "docker-compose", "npm", "node", "claude",
))

GIT_SUBS = frozenset((
    # the 16 read subcommands of the guard's GIT_SUBCOMMANDS
    "status", "log", "show", "diff", "rev-parse", "ls-files", "cat-file", "hash-object",
    "check-ignore", "var", "branch", "remote", "worktree", "describe", "grep", "ls-remote",
    # and the rest of 3.0
    "config", "add", "commit", "push", "pull", "fetch", "switch", "checkout", "restore", "reset",
    "rebase", "merge", "cherry-pick", "tag", "stash", "clean", "rm", "mv", "init", "clone",
    "update-ref", "symbolic-ref", "filter-branch", "send-pack", "gc", "apply", "am", "bisect",
    "blame", "reflog", "shortlog", "notes", "submodule", "sparse-checkout", "archive", "bundle",
    "format-patch", "range-diff", "whatchanged", "for-each-ref", "rev-list", "count-objects", "fsck",
    "prune", "repack", "maintenance",
))

GH_TOP = frozenset((
    "pr", "issue", "api", "run", "repo", "release", "workflow", "auth", "version", "secret",
    "variable", "label", "search", "browse", "cache", "gist", "ruleset", "project", "codespace",
    "extension", "alias", "config", "status", "attestation",
))

GH_SECOND = types.MappingProxyType({
    "pr": frozenset(("create", "new", "comment", "edit", "view", "list", "checks", "merge", "ready",
                     "close", "reopen", "diff", "status", "review", "lock", "unlock", "checkout",
                     "update-branch")),
    "issue": frozenset(("create", "new", "comment", "edit", "view", "list", "close", "reopen", "delete",
                        "lock", "unlock", "pin", "unpin", "transfer", "develop", "status")),
    "run": frozenset(("list", "view", "watch", "rerun", "cancel", "download", "delete")),
    "repo": frozenset(("view", "list", "clone", "create", "edit", "delete", "rename", "fork", "sync",
                       "archive", "set-default")),
    "release": frozenset(("list", "view", "create", "edit", "delete", "upload", "download")),
    "workflow": frozenset(("list", "view", "run", "enable", "disable")),
    "auth": frozenset(("status", "login", "logout", "refresh", "token", "setup-git", "switch")),
})

GO_SUBS = frozenset((
    "build", "test", "vet", "mod", "env", "version", "run", "list", "fmt", "generate", "install",
    "get", "tool", "work", "clean", "doc",
))

# Every flag literal of the guard grammar at the base (permission_guard.py 5b16b120…; the
# "="-suffixed ones without the "=", "--no-textconv" from "--no-textconv=false"): 53 strings.
GUARD_FLAGS = (
    "--abbrev-ref", "--always", "--cached", "--check", "--error-unmatch", "--expect-sha256",
    "--ext-diff", "--first-parent", "--follow", "--format", "--get", "--heads", "--list", "--local",
    "--max-count", "--name-only", "--name-status", "--no-merges", "--no-patch", "--no-textconv",
    "--oneline", "--open-files-in-pager", "--output", "--porcelain", "--pretty", "--printf",
    "--short", "--show-current", "--show-toplevel", "--stage", "--staged", "--stat", "--tags",
    "--textconv", "--untracked-files", "--verify", "--version",
    "-C", "-E", "-F", "-O", "-b", "-c", "-e", "-i", "-l", "-n", "-p", "-q", "-s", "-t", "-v", "-w",
)

FLAG_VOCAB = frozenset(GUARD_FLAGS + (
    "--help", "--repo", "--base", "--head", "--title", "--body", "--body-file", "--draft",
    "--hostname", "--method", "--field", "--raw-field", "--input", "--jq", "--json", "--state",
    "--author", "--limit", "--web", "--force", "--force-with-lease", "--no-verify", "--amend",
    "--all", "--quiet", "--verbose", "--dry-run", "--global", "--system", "--unset", "--add",
    "--replace-all", "--set-upstream", "--delete", "--create", "--push", "--race", "--count",
    "--run", "--timeout", "--tags", "--abbrev-ref", "--untracked-files", "--no-optional-locks",
    "--no-pager", "--git-dir", "--work-tree", "--permission-mode", "--remote-control", "--print",
    "--version", "--color", "--recursive", "--ignore-case", "--line-number", "--fixed-strings",
    "--extended-regexp", "--files-with-matches", "--max-count", "--lines", "--bytes", "--check",
    "--format",
))

# FLAG_VOCAB members (and single-character short flags) that take the next token as their value
# when written without "=". A short flag outside this set never consumes a token.
VALUE_FLAGS = frozenset((
    "-C", "-c", "-e", "-m", "-n", "-t", "-F", "-f", "-H", "-R", "-B", "-X", "-A",
    "--repo", "--base", "--head", "--title", "--body", "--body-file", "--hostname", "--method",
    "--field", "--raw-field", "--input", "--jq", "--json", "--state", "--author", "--limit",
    "--format", "--pretty", "--max-count", "--git-dir", "--work-tree", "--permission-mode",
    "--remote-control", "--untracked-files", "--timeout", "--run", "--count", "--lines", "--bytes",
    "--printf",
))

TOOLS = frozenset((
    "Bash", "Edit", "Write", "Read", "MultiEdit", "NotebookEdit", "Glob", "Grep", "WebFetch",
    "WebSearch", "Agent", "Task", "Skill", "Workflow", "Monitor", "TodoWrite",
))

OPS = frozenset((";", "&", "&&", "|", "||", "(", ")", "<", ">", ">>", "<<", "&>", ">&", "|&"))
REDIRECTS = frozenset(("<", ">", ">>", "<<", "&>", ">&"))

MODES = frozenset(("default", "auto", "plan", "acceptEdits", "dontAsk", "bypassPermissions"))

# explain's "scope" and "switch" values and the hooks-switch reasons (guard-g1-repair-rev6 3.1)
SCOPES = frozenset(("guard_function",))
SWITCH_SOURCES = frozenset(("local", "project", "unset"))
SWITCH_REASONS = frozenset(("hooks_disabled", "hooks_switch_invalid", "hooks_switch_unknown"))

EXPLAIN_SESSION_ID = "explain"
EXPLAIN_SUBAGENT_ID = "explain-subagent"
EXPLAIN_USAGE_LINE = "prompt_observer explain: usage error (see README-prompt-observer.md)"
EXPLAIN_INTERNAL_LINE = "prompt_observer explain: internal error"

MAX_STDIN = 1024 * 1024
TAIL_BYTES = 1024 * 1024
MAX_COMMAND_FILE = 8192
MAX_SETTINGS = 1024 * 1024
MAX_GUARD = 1024 * 1024
MAX_LOG = 4 * 1024 * 1024
MAX_RECORD = 4096
MAX_SEGMENTS = 16
MAX_FLAGS = 32
LOCK_RETRIES = 20
LOCK_SLEEP = 0.05

HEX64_RE = re.compile(r"[0-9a-f]{64}")
UUID_RE = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
SESSION8_RE = re.compile(r"[0-9a-f]{8}")
OPERATOR_RE = re.compile(r"[();<>|&]+")
ASSIGNMENT_RE = re.compile(r"[A-Za-z_][A-Za-z0-9_]*=")
SHORT_FLAG_RE = re.compile(r"-[A-Za-z0-9]")
NUMERIC_FLAG_RE = re.compile(r"-[0-9]+")
REGISTRATION_RE = re.compile(r"python3 -I (\S+) --expect-sha256 ([0-9a-f]{64})")
REASON_RE = re.compile(r"[A-Za-z0-9 _./:~-]{1,60}")
CLASS_RE = re.compile(r"[A-Za-z]{1,40}")


# --- SHAPE (3.2) ----------------------------------------------------------------------------------

def shape(command):
    """The only representation of a command that leaves the process (deterministic)."""
    result = {"segments": [], "ops": [], "more_segments": 0, "unparsed": False,
              "expansion": "$" in command or "`" in command}
    lexer = shlex.shlex(command, posix=True, punctuation_chars=True)
    lexer.whitespace_split = True
    lexer.commenters = ""
    try:
        tokens = list(lexer)
    except ValueError:
        result["unparsed"] = True
        return result
    segments = []
    current = []  # (is_redirect_target, token)
    expect_target = False
    for token in tokens:
        if OPERATOR_RE.fullmatch(token):
            result["ops"].append(token if token in OPS else "?op")
            if token in REDIRECTS:
                expect_target = True
                continue
            expect_target = False
            if current:
                segments.append(current)
            current = []
            continue
        current.append((expect_target, token))
        expect_target = False
    if current:
        segments.append(current)
    result["more_segments"] = max(0, len(segments) - MAX_SEGMENTS)
    result["segments"] = [_segment(items) for items in segments[:MAX_SEGMENTS]]
    return result


def _segment(items):
    assign = 0
    operands = 0
    program_token = None
    args = []
    for is_target, token in items:
        if is_target:
            operands += 1
        elif program_token is None and ASSIGNMENT_RE.match(token):
            assign += 1
        elif program_token is None:
            program_token = token
        else:
            args.append(token)
    base = os.path.basename(program_token) if program_token is not None else None
    program = base if base in PROGRAMS else "OTHER"
    sub, flags, flag_count, arg_operands = _parse_args(program, args)
    operands += arg_operands
    if program == "OTHER":
        return {"program": "OTHER", "sub": [], "flags": [], "flag_count": flag_count,
                "operands": operands, "assign": assign}
    return {"program": program, "sub": sub, "flags": flags, "operands": operands, "assign": assign}


def _flag_name(token):
    if SHORT_FLAG_RE.fullmatch(token):
        return token
    if NUMERIC_FLAG_RE.fullmatch(token):
        return "-N"
    name = token.split("=", 1)[0]
    return name if name in FLAG_VOCAB else "-?"


def _parse_args(program, args):
    """Flags by the flag rule (a VALUE_FLAGS member without "=" consumes the next token as an
    operand, whatever it looks like); git/gh/go subcommand words; everything else counted."""
    sub = []
    flags = []
    flag_count = 0
    operands = 0
    sub_wanted = 1 if program in ("git", "gh", "go") else 0
    after_dashdash = False
    i = 0
    while i < len(args):
        token = args[i]
        i += 1
        if not after_dashdash and token == "--":
            after_dashdash = True
            continue
        if not after_dashdash and token.startswith("-") and token != "-":
            flag_count += 1
            if len(flags) < MAX_FLAGS:
                flags.append(_flag_name(token))
            if "=" not in token and token in VALUE_FLAGS and i < len(args):
                operands += 1
                i += 1
            continue
        if sub_wanted and not after_dashdash:
            sub_wanted -= 1
            if program == "git":
                sub.append(token if token in GIT_SUBS else "?")
            elif program == "go":
                sub.append(token if token in GO_SUBS else "?")
            elif not sub:
                sub.append(token if token in GH_TOP else "?")
                if token in GH_SECOND:
                    sub_wanted = 1
            else:
                sub.append(token if token in GH_SECOND[sub[0]] else "?")
            continue
        operands += 1
    return sub, flags, flag_count, operands


def sanitize(reason):
    """A guard reason leaves the process unchanged only when it is short, plain text."""
    if isinstance(reason, str) and REASON_RE.fullmatch(reason):
        return reason
    return "unlisted_reason"


def tool_label(name):
    if isinstance(name, str) and name in TOOLS:
        return name
    if isinstance(name, str) and name.startswith("mcp__"):
        return "mcp"
    return "OTHER"


# --- strict reads ---------------------------------------------------------------------------------

def _reject_duplicates(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate key")
        result[key] = value
    return result


def _reject_constant(_name):
    raise ValueError("non-finite number")


def loads_strict(data):
    return json.loads(data.decode("utf-8"), object_pairs_hook=_reject_duplicates,
                      parse_constant=_reject_constant)


def _read_regular(path, limit):
    """Open without following a final symlink and without blocking on a FIFO; a regular file of at
    most `limit` bytes, else OSError. Returns (bytes, fstat of the opened descriptor)."""
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_size > limit:
            raise OSError("not a small regular file")
        chunks = []
        remaining = limit + 1
        while remaining > 0:
            chunk = os.read(fd, min(remaining, 1 << 20))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        data = b"".join(chunks)
        if len(data) > limit:
            raise OSError("too large")
        return data, info
    finally:
        os.close(fd)


def _sha256_path(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


# --- guard evaluation (3.3) -----------------------------------------------------------------------

class _NotEvaluated(Exception):
    def __init__(self, reason, sha=None):
        super().__init__(reason)
        self.reason = reason
        self.sha = sha


def _read_settings_doc(path):
    """A settings file: lstat regular (not a symlink), at most 1 MiB, strict JSON. Returns the parsed
    document; FileNotFoundError when absent; OSError/ValueError/RecursionError otherwise."""
    info = os.lstat(path)
    if not stat.S_ISREG(info.st_mode):
        raise OSError("not a regular file")
    data, _ = _read_regular(path, MAX_SETTINGS)
    return loads_strict(data)


def registered_guard(project, hooks_root):
    """The one PermissionRequest registration Claude Code would invoke for a Bash call: an entry
    with matcher exactly "Bash" whose inner hook has type exactly "command" and the command
    `python3 -I PATH --expect-sha256 SHA` with PATH exactly
    hooks_root/permission-guard/SHA[:12]/permission_guard.py. A command of that pattern under any
    other matcher (or none) or in a hook of another type makes the registration ambiguous.
    Returns (PATH, SHA, the parsed local settings document)."""
    path = os.path.join(project, ".claude", "settings.local.json")
    try:
        doc = _read_settings_doc(path)
    except (OSError, ValueError, RecursionError):
        raise _NotEvaluated("no_registration") from None
    seen = 0
    candidates = []
    misplaced = False
    hooks = doc.get("hooks") if isinstance(doc, dict) else None
    entries = hooks.get("PermissionRequest") if isinstance(hooks, dict) else None
    if isinstance(entries, list):
        for entry in entries:
            inner = entry.get("hooks") if isinstance(entry, dict) else None
            if not isinstance(inner, list):
                continue
            for hook in inner:
                if not (isinstance(hook, dict) and isinstance(hook.get("command"), str)):
                    continue
                seen += 1
                match = REGISTRATION_RE.fullmatch(hook["command"])
                if match is None:
                    continue
                if entry.get("matcher") != "Bash" or hook.get("type") != "command":
                    misplaced = True
                    continue
                guard_path, sha = match.group(1), match.group(2)
                if guard_path == hooks_root + "/permission-guard/" + sha[:12] + "/permission_guard.py":
                    candidates.append((guard_path, sha))
    if not seen:
        raise _NotEvaluated("no_registration")
    if misplaced or len(candidates) != 1:
        raise _NotEvaluated("registration_shape")
    return candidates[0][0], candidates[0][1], doc


def hooks_switch(project, local_doc):
    """disableAllHooks as the readable settings decide it: the local document when it has the
    key, else <project>/.claude/settings.json when it has the key, else unset. The user, flag and
    policy sources are never read. Returns (deciding source, None when hooks stay enabled, else
    the NOT_EVALUATED reason: hooks_switch_unknown, hooks_switch_invalid or hooks_disabled)."""
    try:
        project_doc = _read_settings_doc(os.path.join(project, ".claude", "settings.json"))
    except FileNotFoundError:
        project_doc = {}
    except (OSError, ValueError, RecursionError):
        return "unset", "hooks_switch_unknown"
    if not isinstance(project_doc, dict):
        return "unset", "hooks_switch_unknown"
    if isinstance(local_doc, dict) and "disableAllHooks" in local_doc:
        source, value = "local", local_doc["disableAllHooks"]
    elif "disableAllHooks" in project_doc:
        source, value = "project", project_doc["disableAllHooks"]
    else:
        return "unset", None
    if not isinstance(value, bool):
        return source, "hooks_switch_invalid"
    return source, ("hooks_disabled" if value else None)


def load_guard(path, sha):
    """Read the registered file ONCE, check owner, mode and hash of exactly those bytes, check the
    hash is reviewed, then compile and execute those bytes into a private module object."""
    euid = os.geteuid()
    unsafe = stat.S_IWGRP | stat.S_IWOTH
    try:
        info = os.lstat(path)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != euid or info.st_mode & unsafe:
            raise _NotEvaluated("guard_hash")
        data, opened = _read_regular(path, MAX_GUARD)
    except OSError:
        raise _NotEvaluated("guard_hash") from None
    if ((opened.st_dev, opened.st_ino) != (info.st_dev, info.st_ino) or opened.st_uid != euid
            or opened.st_mode & unsafe):
        raise _NotEvaluated("guard_hash")
    if hashlib.sha256(data).hexdigest() != sha:
        raise _NotEvaluated("guard_hash")
    if sha not in KNOWN_GUARD_SHA256:
        raise _NotEvaluated("guard_unknown", sha)
    try:
        code = compile(data, path, "exec", dont_inherit=True)
        module = types.ModuleType("_prompt_observer_private_guard")
        module.__file__ = path
        exec(code, module.__dict__)
        decide = module.__dict__["decide"]
        no_decision = module.__dict__["NoDecision"]
        if not callable(decide) or not (isinstance(no_decision, type) and issubclass(no_decision, Exception)):
            raise TypeError("guard interface")
    except Exception:
        raise _NotEvaluated("guard_import", sha) from None
    return decide, no_decision


def evaluate(command, project, cwd, mode, session_id, subagent, hooks_root, environ, switch_out=None):
    """What the registered known-hash guard answers for one Bash command, when the readable
    settings leave hooks enabled. Returns (decision, reason, guard_sha256); the deciding
    hooks-switch source ("local", "project", "unset") is appended to `switch_out` once read."""
    try:
        path, sha, local_doc = registered_guard(project, hooks_root)
        source, disabled = hooks_switch(project, local_doc)
        if switch_out is not None:
            switch_out.append(source)
        if disabled is not None:
            raise _NotEvaluated(disabled)
        decide, no_decision = load_guard(path, sha)
    except _NotEvaluated as exc:
        return "NOT_EVALUATED", exc.reason, exc.sha
    payload = {"session_id": session_id, "hook_event_name": "PermissionRequest", "tool_name": "Bash",
               "tool_input": {"command": command}, "permission_mode": mode}
    if cwd is not None:
        payload["cwd"] = cwd
    if subagent:
        payload["agent_id"] = EXPLAIN_SUBAGENT_ID
    env = dict(environ)
    env["CLAUDE_PROJECT_DIR"] = project
    try:
        result = decide(["--expect-sha256", sha], io.BytesIO(json.dumps(payload).encode("utf-8")), env)
    except no_decision as exc:
        return "NO_DECISION", sanitize(str(exc)), sha
    except Exception as exc:
        name = type(exc).__name__
        return "NO_DECISION", "error:" + name if CLASS_RE.fullmatch(name) else "error", sha
    if result:
        return "ALLOW", None, sha
    return "NO_DECISION", "no matching shape", sha


# --- transcript (3.4) -----------------------------------------------------------------------------

class _Unreadable(Exception):
    def __init__(self, reason):
        super().__init__(reason)
        self.reason = reason


def pending_calls(session_id, transcript_path, home):
    """The tool calls of this session's transcript tail that have no later tool_result, in order,
    as (name, input) pairs. The notification is bound to the transcript only by its session id
    and file name (projects/<project-dir>/<session_id>.jsonl under HOME/.claude)."""
    if not (isinstance(session_id, str) and UUID_RE.fullmatch(session_id)):
        raise _Unreadable("binding")
    if not (isinstance(transcript_path, str) and os.path.isabs(transcript_path)
            and isinstance(home, str) and os.path.isabs(home)):
        raise _Unreadable("binding")
    name = session_id + ".jsonl"
    root = os.path.realpath(os.path.join(home, ".claude", "projects"))
    real = os.path.realpath(transcript_path)
    parts = os.path.relpath(real, root).split(os.sep)
    if (len(parts) != 2 or parts[0] in ("", ".", "..") or parts[1] != name
            or os.path.basename(transcript_path) != name):
        raise _Unreadable("binding")
    try:
        info = os.lstat(transcript_path)
    except OSError:
        raise _Unreadable("no_command") from None
    if stat.S_ISLNK(info.st_mode):
        raise _Unreadable("binding")
    try:
        data, truncated = _read_tail(transcript_path, info)
    except OSError:
        raise _Unreadable("no_command") from None
    pending = {}
    saw_assistant = False
    for raw in data.split(b"\n"):
        if not raw.strip():
            continue
        try:
            entry = json.loads(raw.decode("utf-8"))
        except (ValueError, RecursionError):
            continue
        if not isinstance(entry, dict):
            continue
        if "sessionId" in entry and entry["sessionId"] != session_id:
            continue
        message = entry.get("message")
        content = message.get("content") if isinstance(message, dict) else None
        if entry.get("type") == "assistant":
            saw_assistant = True
            for block in content if isinstance(content, list) else ():
                if isinstance(block, dict) and block.get("type") == "tool_use" and isinstance(block.get("id"), str):
                    pending[block["id"]] = (block.get("name"), block.get("input"))
        elif entry.get("type") == "user":
            for block in content if isinstance(content, list) else ():
                if (isinstance(block, dict) and block.get("type") == "tool_result"
                        and isinstance(block.get("tool_use_id"), str)):
                    pending.pop(block["tool_use_id"], None)
    if truncated and not saw_assistant:
        raise _Unreadable("truncated")
    return list(pending.values())


def _read_tail(path, info):
    """The last TAIL_BYTES of the file; when the read starts past offset 0, a first line that the
    window cut is dropped. Returns (bytes, started_past_zero)."""
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
    try:
        opened = os.fstat(fd)
        if not stat.S_ISREG(opened.st_mode) or (opened.st_dev, opened.st_ino) != (info.st_dev, info.st_ino):
            raise OSError("transcript changed")
        start = max(0, opened.st_size - TAIL_BYTES)
        chunks = []
        offset = start
        while offset < opened.st_size:
            chunk = os.pread(fd, min(opened.st_size - offset, 1 << 20), offset)
            if not chunk:
                break
            chunks.append(chunk)
            offset += len(chunk)
        data = b"".join(chunks)
        cut = start > 0 and os.pread(fd, 1, start - 1) != b"\n"
    finally:
        os.close(fd)
    if cut:
        newline = data.find(b"\n")
        data = data[newline + 1:] if newline >= 0 else b""
    return data, start > 0


def _utc(now):
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() if now is None else now))


def build_record(notification, project, home, now=None, hooks_root=None, environ=None):
    """The one hook record (3.4) for a permission_prompt notification. Never stores the
    notification's message or title, or any value of the pending call."""
    session_id = notification.get("session_id")
    given = notification.get("permission_mode")
    mode = given if isinstance(given, str) and given in MODES else "default"
    prefix = session_id[:8] if isinstance(session_id, str) else ""
    record = {"v": 1, "ts": _utc(now), "event": "permission_prompt",
              "session": prefix if SESSION8_RE.fullmatch(prefix) else "unknown",
              "source": "transcript_unreadable", "pending": 0, "tool": "OTHER", "shape": None,
              "guard": "NOT_EVALUATED", "reason": "no_command",
              "mode": "given" if mode == given else "assumed_default"}
    try:
        calls = pending_calls(session_id, notification.get("transcript_path"), home)
    except _Unreadable as exc:
        record["reason"] = exc.reason
        return record
    if not calls:
        record["source"] = "no_pending_tool_use"
        return record
    if len(calls) > 1:
        record.update(source="ambiguous", pending=len(calls), reason="ambiguous")
        return record
    name, tool_input = calls[0]
    record.update(source="pending_tool_use", pending=1, tool=tool_label(name))
    if name != "Bash":
        record["reason"] = "not_bash"
        return record
    command = tool_input.get("command") if isinstance(tool_input, dict) else None
    if not isinstance(command, str):
        return record
    record["shape"] = shape(command)
    cwd = notification.get("cwd")
    decision, reason, _ = evaluate(command, project, cwd if isinstance(cwd, str) else None, mode,
                                   session_id, False, hooks_root or HOOKS_ROOT,
                                   os.environ if environ is None else environ)
    record["guard"] = decision
    record["reason"] = reason
    return record


# --- log append (3.5) -----------------------------------------------------------------------------

def append_record(project, record):
    """Append one line to <project>/.scratch/prompt-observer/prompts.jsonl, or nothing."""
    line = json.dumps(record, sort_keys=True, ensure_ascii=True, separators=(",", ":")) + "\n"
    if len(line) > MAX_RECORD:
        line = json.dumps({"v": 1, "ts": record["ts"], "event": "permission_prompt", "oversize": True},
                          sort_keys=True, separators=(",", ":")) + "\n"
    data = line.encode("ascii")
    euid = os.geteuid()
    scratch = os.path.join(project, ".scratch")
    info = os.lstat(scratch)
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != euid:
        return False
    directory = os.path.join(scratch, "prompt-observer")
    try:
        os.mkdir(directory, 0o700)
    except FileExistsError:
        pass
    before = os.lstat(directory)
    if not stat.S_ISDIR(before.st_mode) or before.st_uid != euid or stat.S_IMODE(before.st_mode) != 0o700:
        return False
    fd = os.open(os.path.join(directory, "prompts.jsonl"),
                 os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC, 0o600)
    try:
        opened = os.fstat(fd)
        if (not stat.S_ISREG(opened.st_mode) or opened.st_nlink != 1 or opened.st_uid != euid
                or stat.S_IMODE(opened.st_mode) != 0o600):
            return False
        after = os.lstat(directory)
        if ((after.st_dev, after.st_ino) != (before.st_dev, before.st_ino)
                or stat.S_IMODE(after.st_mode) != 0o700):
            return False
        if opened.st_size > MAX_LOG:
            return False
        for attempt in range(LOCK_RETRIES + 1):
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if attempt == LOCK_RETRIES:
                    return False
                time.sleep(LOCK_SLEEP)
        os.write(fd, data)
        return True
    finally:
        os.close(fd)


# --- entry points -----------------------------------------------------------------------------------

def hook_main(argv, stdin, environ):
    if len(argv) != 2 or argv[0] != "--expect-sha256" or not HEX64_RE.fullmatch(argv[1]):
        return
    if _sha256_path(os.path.abspath(__file__)) != argv[1]:
        return
    data = stdin.read(MAX_STDIN + 1)
    if len(data) > MAX_STDIN:
        return
    notification = loads_strict(data)
    if not isinstance(notification, dict):
        return
    if notification.get("hook_event_name") != "Notification":
        return
    if notification.get("notification_type") != "permission_prompt":
        return
    project = environ.get("CLAUDE_PROJECT_DIR")
    if not isinstance(project, str) or not os.path.isabs(project):
        return
    project = os.path.realpath(project)
    if not os.path.isdir(project):
        return
    append_record(project, build_record(notification, project, environ.get("HOME"), environ=environ))


class _Usage(Exception):
    pass


def _parse_explain(argv):
    valued = {"--command-file": "command_file", "--project": "project", "--cwd": "cwd",
              "--permission-mode": "mode", "--test-root": "test_root"}
    opts = {"command_file": None, "project": None, "cwd": None, "mode": "auto", "subagent": False,
            "test_root": None}
    seen = set()
    i = 0
    while i < len(argv):
        token = argv[i]
        if token in seen:
            raise _Usage()
        seen.add(token)
        if token == "--subagent":
            opts["subagent"] = True
            i += 1
        elif token in valued and i + 1 < len(argv):
            opts[valued[token]] = argv[i + 1]
            i += 2
        else:
            raise _Usage()
    if opts["command_file"] is None or opts["mode"] not in MODES:
        raise _Usage()
    return opts


def _real_dir(path):
    if not isinstance(path, str) or path == "":
        raise _Usage()
    real = os.path.realpath(path)
    if not os.path.isdir(real):
        raise _Usage()
    return real


def explain_main(argv, environ):
    try:
        opts = _parse_explain(argv)
        path = opts["command_file"]
        try:
            if not stat.S_ISREG(os.lstat(path).st_mode):
                raise _Usage()
            data, _ = _read_regular(path, MAX_COMMAND_FILE)
            command = data.decode("utf-8")
        except (OSError, ValueError):
            raise _Usage() from None
        if command.endswith("\n"):
            command = command[:-1]
        if opts["project"] is not None:
            project = _real_dir(opts["project"])
        elif environ.get("CLAUDE_PROJECT_DIR"):
            project = _real_dir(environ["CLAUDE_PROJECT_DIR"])
        else:
            project = _real_dir(os.getcwd())
        cwd = os.path.abspath(opts["cwd"]) if opts["cwd"] else project
        hooks_root = HOOKS_ROOT
        if opts["test_root"] is not None:
            hooks_root = os.path.join(_real_dir(opts["test_root"]), "home", ".claude", "hooks")
    except _Usage:
        sys.stderr.write(EXPLAIN_USAGE_LINE + "\n")
        return 2
    try:
        switch = []
        decision, reason, sha = evaluate(command, project, cwd, opts["mode"], EXPLAIN_SESSION_ID,
                                         opts["subagent"], hooks_root, environ, switch_out=switch)
        out = {"decision": decision, "reason": reason, "guard_sha256": sha, "shape": shape(command),
               "scope": "guard_function", "switch": switch[0] if switch else "unset"}
        if opts["test_root"] is not None:
            out["test_root"] = True
        line = json.dumps(out, sort_keys=True, ensure_ascii=True, separators=(",", ":")) + "\n"
    except Exception:
        sys.stderr.write(EXPLAIN_INTERNAL_LINE + "\n")
        return 2
    sys.stdout.write(line)
    sys.stdout.flush()
    return 0


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    if argv[:1] == ["explain"]:
        return explain_main(argv[1:], os.environ)
    try:
        hook_main(argv, sys.stdin.buffer, os.environ)
    except BaseException:  # any error at all, an interrupt included: write nothing, print nothing
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
