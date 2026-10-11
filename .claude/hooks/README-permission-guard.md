# PermissionRequest guard for unattended Draft-PR work

Six files under `.claude/hooks/` implement, test, install and natively verify a
`PermissionRequest` hook that answers the routine permission prompts of an
unattended engineering session on this repository (contract s1h-contract-rev4).

| file | role |
| --- | --- |
| `permission_guard.py` | the hook: ALLOW for an exact grammar, otherwise no decision |
| `test_permission_guard.py` | offline adversarial tests of the guard (subprocess, real `git init`) |
| `install_permission_guard.py` | hash-bound installer: `install`, `verify`, `rollback`, `promote` |
| `test_install_permission_guard.py` | offline tests of the installer's state transitions |
| `native_harness.py` | inert harness on the real permission engine (`claude -p` in a throwaway project) |
| `README-permission-guard.md` | this file |

## Why

The tracked `.claude/settings.json` keeps its deny and ask rules. Several ask
rules prompt the owner's phone for routine work: `gh pr create *`,
`gh pr comment *`, `gh api *-X*`/`*-F*`, `gh -*` (so `gh --version`),
`git config *` (reads included), and the over-broad `*git* -c *` and `*/time*`
patterns that Claude Security sub-agents hit. Rules are evaluated deny → ask →
allow, so an allow rule cannot cancel an ask rule, and a `PreToolUse` hook cannot
either. A `PermissionRequest` hook runs when Claude Code is about to ask; it may
return `decision.behavior: "allow"`. Deny and ask rules are still evaluated, so a
hook allow never overrides a matching deny rule.

## What the guard never does

* It never returns `deny`, never emits `updatedInput` or `updatedPermissions`,
  never exits 2. Its only outputs are the exact ALLOW object or nothing.
* Nothing = no decision = Claude Code prompts exactly as today. Every error path
  (bad JSON, missing record, unreadable file, any exception) is "nothing".
* It opens no network connection, writes nothing, logs nothing. It reads only
  `.git/HEAD`, the task record, the named body file and the two executables it
  hashes, plus `CLAUDE_PROJECT_DIR` and the *names* (never values) of the
  environment variables listed below.
* It is pinned to its own bytes: the registration passes `--expect-sha256 SHA`,
  and a copy whose hash differs answers nothing for everything.

### Hook-failure analysis (from the Claude Code docs, read 2026-10-11)

* A hook that exits 2 without a `decision` object leaves the permission flow
  unchanged. A timed-out hook renders no decision. A hook that cannot start,
  crashes or prints invalid JSON is a non-blocking error with no decision object.
  In every failure mode the request falls through to the prompt (or, in a
  session that cannot prompt, is denied). `onFailure: "block"` is deliberately
  not used: no failure path may grant anything, and "block" would turn a guard
  failure into a hard block of every prompted command.
* Hooks from settings files also run inside subagents; the input then carries
  `agent_id`/`agent_type`. The guard uses that to refuse write shapes from
  sub-agents.
* In a `-p` run with `--permission-prompts none`, anything that would prompt is
  denied unless a PermissionRequest hook allows it. That is what the native
  harness relies on.

## Registration (written only by the installer)

```json
{"hooks": {"PermissionRequest": [{"matcher": "Bash", "hooks": [
  {"type": "command", "command": "python3 -I RUNTIME --expect-sha256 SHA", "timeout": 10}]}]}}
```

`RUNTIME` = `/home/claude/.claude/hooks/permission-guard/<HASH12>/permission_guard.py`
(an immutable, mode 0444 copy; `HASH12` = first 12 hex of the guard's SHA-256),
`SHA` = the full SHA-256 of that file. The registration lives in the git-ignored
`.claude/settings.local.json`; the tracked `.claude/settings.json` is not changed.
The runtime copy is bound by hash, not by branch: a merge, checkout or branch
change neither promotes nor removes it.

## Grammar

Classification order: the raw command is rejected if it contains a backtick,
`$`, a newline, a carriage return, a NUL or any non-printable character. It is
then tokenised with `shlex` (POSIX, `punctuation_chars=True`,
`whitespace_split=True`, no comment characters); any operator token (`;`, `&`,
`&&`, `|`, `||`, `<`, `>`, `>>`, `(`, `)`, a token starting with `#`), any
token containing `*`, `?`, `[`, `{` or starting with `~`, and a leading
`NAME=value` assignment all mean no decision. Then exactly one shape must match;
`--flag value`, `--flag=value` and the listed short flags are accepted; an
unknown, repeated or clustered flag, a conflicting pair or an unexpected
positional operand means no decision. Program names never imply anything.

### Write shapes (main controller only)

Binding for every write shape: `agent_id` absent; `permission_mode` is `default`
or `auto`; `cwd` is the project; none of `GH_HOST GH_REPO GH_CONFIG_DIR
GH_ENTERPRISE_TOKEN GITHUB_ENTERPRISE_TOKEN GH_PAGER PAGER GH_FORCE_TTY GIT_DIR
GIT_WORK_TREE GIT_CONFIG_GLOBAL GIT_CONFIG_SYSTEM GIT_CONFIG_PARAMETERS
GIT_CONFIG_COUNT GIT_EXTERNAL_DIFF GIT_PAGER GIT_EXEC_PATH` is present in the
hook's environment; `argv[0]` is exactly `gh`, `shutil.which("gh")` equals the
record's `gh_path` and that file hashes to `gh_sha256`; the task record is
`active`.

* **A create** — `gh pr create --draft --repo github.com/KraineOpasen/bukerov-twitch-miner-go --base release/0.3 --head BRANCH --title TITLE --body-file PATH`
  (any order; `-d -R -B -H -t -F`). BRANCH = the current branch and matches
  `^(fix|feat|chore|test|docs|ci|refactor|perf|build)/[A-Za-z0-9._-]+$`; TITLE
  non-empty, ≤ 200 printable characters; the record has no `pr`; PATH is a body
  snapshot (below) whose SHA-256 equals the record's `body_sha256`.
* **B comment** — `gh pr comment N --repo github.com/KraineOpasen/bukerov-twitch-miner-go` with exactly one of `--body TEXT`/`-b` or `--body-file PATH`/`-F`; N = the record's `pr`; the text (whitespace runs collapsed, ends stripped) is exactly one of the trigger texts.
* **C body PATCH** — `gh api --hostname github.com -X PATCH repos/KraineOpasen/bukerov-twitch-miner-go/pulls/N -F body=@PATH` (`--method`, `--field` accepted; one optional `-H "Accept: application/vnd.github+json"`); N = the record's `pr`; PATH as in A.

Body snapshot (5.4 f): absolute path, no symlink component, under
`<project>/.scratch/`, a regular file with link count 1, owned by the current
uid, ≤ 65536 bytes, opened with `O_NOFOLLOW|O_NONBLOCK` and re-checked after
opening.

Trigger texts (exact):

```
T1  @coderabbitai full review
T2  @codex review Full review of the entire PR. Review only; make no changes.
T3  @codex security review
```

### Read forms (sub-agents too; record `active` or `disarmed`)

ROOTS = the project directory and `<goroot>/src`. An operand must be absolute
(repository-relative without `..` only where noted), have no symlink component,
resolve under ROOTS and never under `.git/` except `.git/HEAD`.

* **R1** `gh --version` or `gh version` (GH_* names absent; gh identity checked).
* **R2** `git [-C <project>] config [--local] --get user.name|user.email`.
* **R3** `grep` with flags only from `-n -c -l -L -i -E -F -w -x -o -h -H -q -s -r -A N -B N -C N -m N -e PATTERN --`; one pattern; one or more files under ROOTS (`-r` only with directories).
* **R4** `cat (-n -A)`, `head`/`tail (-n N, -c N, -N)`, `wc (-l -c -w -m -L)`, `stat (-c FORMAT, --printf FORMAT; never -L)`, `sha256sum` (no flags), `ls (-l -a -A -1 -d -h -t -r -S --)`; files under ROOTS (`ls` may name directories).
* **R5** `sed -n N[,M]p FILE`.
* **R6** `git [-C <project>] <subcommand>` with no other global option, for: `status log show diff rev-parse ls-files cat-file hash-object check-ignore var branch remote worktree describe grep ls-remote`, each with the flag set listed in `permission_guard.py` (`GIT_SUBCOMMANDS`); REV matches `^[A-Za-z0-9._/^~-]{1,120}$`, formats match `^[%A-Za-z0-9 ()<>:,._-]{1,120}$`; any token containing `--output`, `--ext-diff`, `--textconv`, `-O`, `--open-files-in-pager` or `--no-textconv=false` means no decision. GIT_* names must be absent; git identity is checked.

Narrowings beyond the contract text (each answers "nothing" where the contract would
have allowed; none widens): a `.git` directory at any depth under either root is
refused, not only the project's top-level one; a regular-file operand must have link
count 1; `grep -r` over a whole root is refused; a rev-shaped token that contains `/`
or starts with `.` is also checked as a repository path, so no rev can name a file
elsewhere; `git` forms without `-C` require the hook's `cwd` to be the project;
`git hash-object` takes absolute operands only; a repeated git flag is refused
(`-e` may repeat for `git grep`); any `~` in a `gh` command is refused (bash
expands `~` after `:` and `=` inside words); a body file whose separators are
Unicode whitespace other than space, tab, CR and LF is not a trigger.

Everything else: no decision. Deny rules are never reasoned about — Claude Code
applies them before and regardless of the hook.

## Task record

`<project>/.scratch/permission-guard/task-record.json` — a regular file (not a
symlink), ≤ 16 KiB, written and updated only by the controller of the active
contract:

```json
{
  "contract": "s1h-contract-rev4",
  "contract_sha256": "<bookkeeping, not checked>",
  "state": "active",
  "repo": "github.com/KraineOpasen/bukerov-twitch-miner-go",
  "branch": "chore/permission-guard",
  "evidence_root": "/home/claude/bukerov-twitch-miner-go/.scratch/s1h-<RUNID>",
  "gh_path": "/usr/bin/gh",  "gh_sha256": "<sha256 of /usr/bin/gh>",
  "git_path": "/usr/bin/git", "git_sha256": "<sha256 of /usr/bin/git>",
  "goroot": "/usr/local/go",
  "pr": 427, "pr_head": "<40 hex>",          // after PR read-back; absent for create
  "body_sha256": "<sha256 of the snapshot>"   // for create and the body PATCH
}
```

`state` is `active` while a contract runs, `disarmed` while a guard repair is
in flight (reads still answered, writes not), `closed` at RESULT/STOP (nothing
answered). A missing, foreign (`repo`/`branch` mismatch) or closed record means
no decision for everything. Every later contract writes its own record before
expecting any auto-allow and closes it at its end.

## Installer

```
python3 -I /home/claude/bukerov-twitch-miner-go/.claude/hooks/install_permission_guard.py install  --manifest <evidence root>/install-manifest.json
python3 -I /home/claude/bukerov-twitch-miner-go/.claude/hooks/install_permission_guard.py verify   --manifest <evidence root>/install-manifest.json
python3 -I /home/claude/bukerov-twitch-miner-go/.claude/hooks/install_permission_guard.py rollback --manifest <evidence root>/install-manifest.json
python3 -I /home/claude/bukerov-twitch-miner-go/.claude/hooks/install_permission_guard.py promote  --manifest <evidence root>/install-manifest.json
```

The manifest names `source`, `source_sha256`, `runtime_root` (its parent must
exist), `settings_target`, `evidence_dir` and the `registration` template (exactly
the section-4 entry: matcher `Bash`, one `command` hook with the template command
and an integer `timeout`; any other key, `onFailure` included, is refused); the
installer touches nothing else and never deletes anything. `install` records the
preimage (ABSENT, or a byte copy under the evidence directory), refuses any
foreign `PermissionRequest` entry (exit 3) and is a no-op when the exact
registration is already present (exit 0), builds and validates the new document
before anything is created, re-reads the target (DRIFT_BEFORE_WRITE, exit 6),
creates the 0444 runtime copy (refusing an existing directory, exit 5), re-reads
once more, and replaces atomically through a temporary file in the same
directory: the file is written in full, fsynced and hashed before `os.replace`,
and the postimage on disk is compared with the intended bytes afterwards. Every
failure of that path names the temporary file (exit 7). `verify` compares the
settings hash with the recorded postimage, the runtime hash with the registered
SHA and requires the registration to be present exactly once (MATCH exit 0 /
DRIFT exit 1); it writes nothing. `rollback` restores the preimage bytes with the
recorded mode (or renames an ABSENT-preimage file to
`settings.local.json.rolled-back-<UTC>`) and renames the runtime directory to
`<HASH12>.disabled-<UTC>`; with drift it refuses (exit 4). `promote` installs a
repaired guard under its new hash, replaces the registration string once,
renames the old runtime directory `.superseded-<UTC>` and chains its receipt to
the previous one. Every path that `verify`, `rollback` and `promote` act on is
re-checked against the manifest (the settings target must match, runtime
directories must sit under `runtime_root`, a preimage backup must sit under
`evidence_dir`); a state file that disagrees is refused (exit 2), never acted on.

Every run prints one JSON receipt (last stdout line) that names the rollback
command. The receipt records progress as it happens: `runtime_copy_created`,
`settings_written` (install, promote) or `settings_restored` (rollback) with the
live postimage hash, so a failure after a completed write is never silent; any
run that changed the settings file updates `<evidence_dir>/permission-guard-state.json`
even when a later step failed, and `rollback` then works from that live state. A
failed install may leave `<runtime_root>/<HASH12>/` behind (the receipt names it);
the owner renames it before retrying — nothing is deleted. After a rollback the
state file records ROLLED_BACK and `verify`/`rollback`/`promote` refuse with
STATE_NOT_INSTALLED (exit 2) until a new `install`. A state-file write that fails
after a successful transaction keeps the transaction's result but exits 7 with
reason STATE_SAVE_FAILED.

The rollback command printed by the install receipt is valid while the settings
file still hashes to the recorded postimage; otherwise the installer reports
DRIFT and the owner decides.

## Native harness

```
python3 -I /home/claude/bukerov-twitch-miner-go/.claude/hooks/native_harness.py \
  --candidate /home/claude/bukerov-twitch-miner-go/.claude/hooks/permission_guard.py \
  --root /home/claude/s1h-harness-<RUNID> --evidence <evidence root>/harness-1
```

Builds `ROOT/proj` (a fresh repository on `chore/permission-guard`), registers
the candidate as the PermissionRequest hook in `proj/.claude/settings.json`
beside the ask rules `Bash(gh *)`, `Bash(git config *)`, `Bash(git -* config *)`,
the clone's tracked deny list plus Read denies for `~/.config/gh`, `~/.claude`
and `~/.gitconfig`, and the clone's all-false `enabledPlugins` map, and runs one
nested `claude -p … --permission-mode default --permission-prompts none
--max-turns 3 --output-format stream-json --verbose` per case. The real
repository is unreachable by construction: a fake `gh` first on PATH (prints
`FAKE_GH ARGV=…`), the record pins `gh_path` to that fake, `GH_TOKEN` is an
invalid literal, the PR number is 999999999 and the project has no remote. The
user's other credentials are protected by the permission engine (the deny rules,
`--permission-prompts none`, `--max-turns 3`), not by construction: the nested
session runs as the user and loads the user's MCP connectors and plugins, so the
harness copies each nested session's `init` inventory (tools, MCP servers,
plugins, model) into `meta.json` and `receipt.json` for review. The contract's
`GH_CONFIG_DIR` layer is not used because the guard refuses any hook environment
that carries that name (5.4 b) — the same check H6 verifies with `GH_HOST`.
Cases H1–H10 (H11 optional) must all PASS for `NATIVE_INERT_PASS`; a case whose
first Bash command differs from the requested one is INVALID and retried once.
Case H2 (create) runs with a record without `pr`, the others with `pr`
999999999, because shape A requires the record to carry no PR yet. H10's denial
proves that without the hook no allow source exists for `gh --version` under the
ask rules; it does not single out which rule fired. Not natively verified:
that the engine supplies `agent_id` inside subagents (the sub-agent refusal of
write shapes rests on the documented input fields). The throwaway project is
left in its canonical state (hook variant) when the run ends; nested sessions
also write their own transcripts under `~/.claude/projects/`.

## Trust boundary

The guard removes routine prompts and prevents accidental or malformed GitHub
writes by the executor. The deny rules, the Draft-only publication and the
owner's merge remain the hard boundary. The guard is not a sandbox against an
executor that deliberately subverts it with the same OS account.

## Tests

```
python3 -I .claude/hooks/test_permission_guard.py -v          [--guard PATH]
python3 -I .claude/hooks/test_install_permission_guard.py -v  [--installer PATH]
```

Run them as scripts (`python3 -I -m unittest <path>` fails for a path inside
`.claude`). Zero collected tests or any skipped test is a failure. `--guard` and
`--installer` point the suites at copies for falsifier probes.
