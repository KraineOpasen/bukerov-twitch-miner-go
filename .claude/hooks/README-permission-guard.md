# PermissionRequest guard for unattended Draft-PR work

Six files under `.claude/hooks/` implement, test, install and natively verify a
`PermissionRequest` hook that answers the routine permission prompts of an
unattended engineering session on this repository (the S1-H contract).

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
  hashes; from the environment it reads only the value of `CLAUDE_PROJECT_DIR`,
  the value of `PATH` (only to look up `gh` and `git`, whose path and SHA-256
  must equal the task record) and the *names* (never values) of the environment
  variables listed below.
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
or `auto`; `cwd` is the project; none of the gh names `GH_HOST GH_REPO
GH_CONFIG_DIR GH_ENTERPRISE_TOKEN GITHUB_ENTERPRISE_TOKEN GH_PAGER PAGER
GH_FORCE_TTY`, the git names `GIT_DIR GIT_WORK_TREE GIT_CONFIG_GLOBAL
GIT_CONFIG_SYSTEM GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT GIT_EXTERNAL_DIFF
GIT_PAGER GIT_EXEC_PATH GIT_SSH GIT_SSH_COMMAND GIT_ASKPASS SSH_ASKPASS` or the
six shell names `BASH_ENV ENV SHELLOPTS BASHOPTS BASH_FUNC_gh%% BASH_FUNC_git%%`
is present in the hook's environment (the hook sees the environment the Bash
tool's shell inherits; `BASH_FUNC_<name>%%` is how bash imports an exported
function that would shadow `gh` or `git`); `argv[0]` is exactly `gh`, `shutil.which("gh")` equals the
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
resolve under ROOTS and never under `.git/` except `.git/HEAD`. If any of the
six shell names `BASH_ENV ENV SHELLOPTS BASHOPTS BASH_FUNC_gh%% BASH_FUNC_git%%`
is present in the hook's environment, no form at all is answered (R1 to R6 and
the write shapes A, B, C): an exported shell function for `gh` or `git` is
refused for every form, not only for its own program.

* **R1** `gh --version` or `gh version` (the gh names above absent; gh identity checked).
* **R2** `git [-C <project>] config [--local] --get user.name|user.email`.
* **R3** `grep` with flags only from `-n -c -l -L -i -E -F -w -x -o -h -H -q -s -r -A N -B N -C N -m N -e PATTERN --`; one pattern; one or more files under ROOTS (`-r` only with directories).
* **R4** `cat (-n -A)`, `head`/`tail (-n N, -c N, -N)`, `wc (-l -c -w -m -L)`, `stat (-c FORMAT, --printf FORMAT; never -L)`, `sha256sum` (no flags), `ls (-l -a -A -1 -d -h -t -r -S --)`; files under ROOTS (`ls` may name directories).
* **R5** `sed -n N[,M]p FILE`.
* **R6** `git [-C <project>] <subcommand>` with no other global option, for: `status log show diff rev-parse ls-files cat-file hash-object check-ignore var branch remote worktree describe grep ls-remote`, each with the flag set listed in `permission_guard.py` (`GIT_SUBCOMMANDS`); REV matches `^[A-Za-z0-9._/^~-]{1,120}$`, formats match `^[%A-Za-z0-9 ()<>:,._-]{1,120}$`; any token containing `--output`, `--ext-diff`, `--textconv`, `-O`, `--open-files-in-pager` or `--no-textconv=false` means no decision. The git names above must be absent (R2 too); git identity is checked.

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

Owner-run only. Every mode needs both integrity pins: `--expect-sha256` (the
installer's own SHA-256, checked before anything else is read) and
`--expect-manifest-sha256` (the manifest's SHA-256, checked before it is parsed).

```
python3 -I /home/claude/bukerov-twitch-miner-go/.claude/hooks/install_permission_guard.py install \
  --manifest /home/claude/bukerov-twitch-miner-go/.scratch/s1h-20261011T173421Z/r9-install-manifest.json \
  --expect-sha256 <installer sha256> --expect-manifest-sha256 <manifest sha256>
# verify and rollback: the same manifest and pins; promote: r9-promote-manifest.json and its pin
```

Every destination is a constant in the installer (11.0); the manifest may only
repeat these exact values and can never widen them:

| name | path |
| --- | --- |
| source | `/home/claude/bukerov-twitch-miner-go/.claude/hooks/permission_guard.py` |
| settings target | `/home/claude/bukerov-twitch-miner-go/.claude/settings.local.json` |
| hooks directory | `/home/claude/.claude/hooks` (must already exist as a real directory) |
| runtime root | `/home/claude/.claude/hooks/permission-guard` (plain `mkdir` when absent) |
| runtime directory | `<runtime root>/<first 12 hex of source_sha256>/` |
| evidence directory | `/home/claude/bukerov-twitch-miner-go/.scratch/s1h-20261011T173421Z/r9-install` |
| manifests | `…/s1h-20261011T173421Z/r9-install-manifest.json` (install, verify, rollback), `…/r9-promote-manifest.json` (promote) |

The manifest names `source`, `source_sha256`, `runtime_root`, `settings_target`,
`evidence_dir` and optionally `registration`, which must equal the section-4
template exactly (any other key, `onFailure` included, is refused). The source
must hash to `source_sha256`; that manifest value is not independent authority,
the manifest pin is.

Containment (CT1–CT6): the roots are the pinned constants and are used only
when they are existing real directories whose realpath equals their own path;
every derived path is absolute and normalized, without `.`, `..`, empty
components or NUL; every existing component from the root down to the parent is
`lstat`-ed and must be a real directory, `realpath(parent)` must be the root or
lie below it, the basename is a plain name and a missing parent is a failure; an
existing target must be a regular file (or, for the installer's own
directories, a real directory), never a symlink. The checks are repeated
immediately before each write or rename; temporary files are created in the
verified parent with `O_EXCL|O_NOFOLLOW` and moved with `os.replace`. This
narrows the check-to-use window; it does not close it against another process
of the same OS account.

`install` records the preimage (ABSENT, or a byte copy in the evidence
directory). When the exact registration is already present it is a no-op
(IDEMPOTENT, exit 0) only if the registered runtime file is intact (a regular,
non-symlink 0444 file with the registered SHA-256); otherwise it refuses
RUNTIME_DRIFT (exit 4) and writes nothing. It refuses any foreign
`PermissionRequest` entry (exit 3) and an existing runtime directory for the hash
(exit 5); an installer-owned `.aborted-` archive of an earlier handled failure
never blocks it. Before anything is written it validates the whole `hooks`
structure, builds and encodes the new document and checks the containment of
every path it will create, write or rename (see "Before the first transaction
write" below). It then copies the preimage, re-reads the target
(DRIFT_BEFORE_WRITE: exit 6 while nothing has been written, which is the case for
an ABSENT preimage; after the preimage copy it is FAILED, exit 7, `original_code`
6), creates the 0444 runtime copy, re-reads
once more and replaces the settings atomically (written in full, fsynced and
hashed before `os.replace`; the postimage is read back and compared with the
intended bytes afterwards). `promote` installs a repaired guard under its new
hash the same way, replaces the registration string once, renames the old
runtime directory `.superseded-<UTC>` and chains its receipt to the previous one.
`rollback` restores the preimage bytes with the recorded mode (or renames an
ABSENT-preimage file to `settings.local.json.rolled-back-<UTC>`; nothing is read
back after that rename) and renames the runtime directory to
`<HASH12>.disabled-<UTC>`; with drift it refuses (exit 4). Every path read from
the state file is re-checked against the pinned layout; a state file that
disagrees is refused (exit 2), never acted on. Nothing is ever deleted.

Before the first transaction write (all modes that write):

* **Every refusal is checked first (phase P).** Every check that can end a run
  with a refusal runs before anything is written: the settings document is read
  and its `hooks` structure validated at every level the installer reads or
  changes (a `hooks` or `PermissionRequest` value that is null, a list where an
  object is expected, a string or a number, or a `PermissionRequest` entry that
  is not an object, is refused with exit 2; a document nested deeper than the JSON
  decoder can follow is malformed JSON like any other, SETTINGS_MALFORMED, exit 2,
  and this holds for every file the installer parses), the new document is built and
  encoded, the foreign-hook, registration, manifest and source checks run, and
  the containment of every path the run will create, write or rename is checked
  prospectively: the existing part of each path as an existing parent is
  checked, and the missing part may only be the directories the installer itself
  creates (RUNTIME_PARENT, `<HASH12>`, `receipts/`) and the final name. After the
  installer creates one of those directories it checks it again, strictly, before
  it writes anything inside it (each passed check is listed in
  `post_creation_checks`); the checks immediately before each write or rename
  stay as they were.
* **Codes before and after the first transaction write.** A transaction write
  is any creation, write or rename of the preimage copy, RUNTIME_PARENT,
  `<HASH12>` or anything inside it, the settings target, the state file, or an
  archive or sibling name of them; writing the run's own receipt is not one.
  Before the first transaction write every outcome keeps its own code: 0
  (IDEMPOTENT, nothing written), 2 (refused input), 3 (FOREIGN_HOOK), 4
  (RUNTIME_DRIFT, rollback drift, POSTIMAGE_UNKNOWN), 5 (SAME_HASH_ALREADY_INSTALLED,
  runtime directory exists) or 6 (DRIFT_BEFORE_WRITE), and only the refusal
  receipt is written. After it the installer never reports REFUSED: a refusal
  then is FAILED, exit 7, with its own code in `original_code`, and the receipt
  names every write the run made (`preimage_backup_created`,
  `runtime_parent_created`, `runtime_dir_owned`, `runtime_renamed_to`, which is
  null when a failure before the settings replacement found no `<HASH12>` of this
  run to archive, `temp_path`, `settings_written`, `settings_restored`), and
  `failed_step` names the step that failed. The creation of RUNTIME_PARENT and the
  creation of `<HASH12>` each count on their own: with no settings file before the
  run (no preimage copy), a failure right after the installer created
  RUNTIME_PARENT is already FAILED, exit 7, and RUNTIME_PARENT stays in place. A
  RUNTIME_PARENT that existed before the run is never claimed as created by it.

Write outcomes (all modes that write):

* **Invariant A (before the replacement).** Every handled failure after this
  run's `mkdir` of `<HASH12>` and before the settings replacement has returned,
  including failures inside the copy step, renames that directory to
  `<HASH12>.aborted-<UTC>-<pid>` exactly once; the receipt names `failed_step`
  and `runtime_renamed_to`, and the settings target stays byte-identical.
  RUNTIME_PARENT, once created, stays in place (nothing is deleted).
* **Invariant W (the write is recorded at once).** In the statement right after
  `os.replace` of the settings target (or the rename of an ABSENT rollback)
  returns, the receipt records `settings_written` (install, promote) or
  `settings_restored` (rollback), the temporary file and
  `intended_postimage_sha256`. Every other transaction write is likewise
  recorded in the statement after it (the `mkdir` of RUNTIME_PARENT as
  `runtime_parent_created` and of `<HASH12>` as `runtime_dir_owned`, the
  preimage copy as `preimage_backup_created` the moment the file exists, so a
  copy that fails part-way is named too, the `.aborted-`, `.disabled-` and
  `.superseded-` renames); the runtime copy file inside `<HASH12>` is covered by
  the record of its directory, and `receipts/` with the receipt file is
  evidence only; because no signal raises inside the write phase (see
  "Interrupts" below), nothing can come between an operation and its record. The
  read-back is a later step: when it fails the run ends with exit 7,
  POSTIMAGE_UNREADABLE and a null `postimage_sha256`, and the state records the
  write. Any refusal raised after a transaction write is reported as FAILED with
  exit 7 (its own code kept in `original_code`), never as refused input. A state whose postimage is null, or differs from the intended postimage
  (POSTIMAGE_MISMATCH, a concurrent writer), is never acted on: `rollback` and
  `promote` refuse POSTIMAGE_UNKNOWN (exit 4, nothing written) and `verify` reports
  DRIFT with the intended and the current hash.
* **F3 (after the replacement).** No failure touches the runtime directory the
  new registration points at; the receipt sets `failed_step`; state and receipt
  are saved; exit 7.
* **Outcome S (state before receipt).** After a completed write the state file
  `permission-guard-state.json` is saved first; a receipt with a success result
  and exit 0 is persisted only after that save succeeded. When the state save
  fails the result is `<RESULT>_STATE_NOT_SAVED` (for example
  INSTALLED_STATE_NOT_SAVED), exit 7, `state_saved` false, with the original
  result and reason in `original_result` and `original_reason` and any leftover
  temporary state file in `state_save_temp_path`. A refusal that wrote nothing
  saves no state and keeps its own exit code. A receipt file that cannot be
  written leaves `receipt_saved` false in the printed receipt (exit 7, unless a
  refusal keeps its own code). Receipts and the state are written ASCII-escaped,
  so no text read from the settings can break them. A directory fsync failure
  after the replacement is recorded as `dir_fsync_error` (for the state write,
  `state_dir_fsync_error`).

Interrupts (Ctrl+C, SIGTERM, SIGHUP):

* **Before the first write (phase P).** The precondition checks write nothing and
  keep Python's default signal behaviour. Ctrl+C (SIGINT) there normally ends the
  run as FAILED, reason INTERRUPTED, `interrupted` true, exit 7, with nothing
  written but the receipt: the receipt file is written when the evidence
  directory was already verified, otherwise the receipt is printed only
  (`receipt_saved` false). This also holds for a Ctrl+C that lands while the
  installer takes over the outcome of the checks, before its handlers are
  installed; the outcome it overtook is named in `interrupted_outcome`. A Ctrl+C
  before the arguments are parsed, or within the few instructions of that
  take-over itself, can still end the process with Python's KeyboardInterrupt
  message (exit 130) and no receipt. SIGTERM or SIGHUP in phase P end the process
  at once with no receipt. In every one of these cases nothing has been written
  to the settings target, RUNTIME_PARENT, `<HASH12>` or the state file.
* **From the first write on.** Before its first write, and in every case before
  it reports its outcome (a refusal, an IDEMPOTENT no-op or a failure), `install`,
  `rollback` and `promote` install one handler for SIGINT, then SIGTERM, then
  SIGHUP. `verify` never does: it writes nothing and Ctrl+C ends it at once,
  possibly without a receipt. The handler only notes the first signal it
  receives; it raises nothing and is never removed, so from then until the
  process exits Ctrl+C, SIGTERM and SIGHUP stop nothing: the run completes, and a
  deferred signal never changes the outcome or the exit code. No signal mask is
  used.
* **Where it is recorded.** The state file and the receipt file each record
  `interrupt_deferred` (true or false) and `interrupt_signal` (the first signal's
  name, such as `SIGINT`, or null) as observed at the record's snapshot, taken
  immediately before the record is written; a later signal is not in them. The
  printed receipt carries the same values as the receipt file. A signal that
  arrives after the receipt's snapshot is reported once on stderr (the JSON
  receipt stays the last stdout line), saying whether the receipt was saved to
  disk, only printed, or neither. A signal first observed after that report, or
  not processed before the process ends, leaves no trace.
* **A receipt that cannot be printed.** When stdout (or stderr, for that one
  line) can no longer be written, for example after the terminal or the reading
  pipe went away, the failed stream is pointed at /dev/null, so the run's exit
  code stays the outcome's and is never turned into Python's 120.
* **Limits.** SIGKILL, SIGQUIT, SIGSTOP, power loss and a host crash are not
  deferred and can stop the run between two file operations: the sequence of
  operations is not atomic as a whole, and `verify` then reports what is on disk
  (DRIFT or INCOMPLETE_INSTALL; nothing is guessed). Several signals are recorded
  as the first one only, without a count. A system call interrupted by a deferred
  signal is retried by Python where Python documents that (PEP 475); an
  interrupted rename or mkdir would surface as an ordinary write failure.
  The printed rollback command is itself a writing run and defers interrupts the
  same way.

`verify` writes nothing and decides by this precedence (the first matching step
wins): (1) a state file that exists but cannot be used — unreadable, a symlink or
not a regular file, or not valid JSON: STATE_UNREADABLE (exit 1, the cause in
`state_refusal`; only a missing file means "no state"); (2) a readable state that
records a committed write: MATCH (exit 0) when the settings hash equals the
recorded postimage, the runtime hash equals the registered SHA and the
registration is present exactly once, otherwise DRIFT (exit 1) with both hashes;
always DRIFT when the postimage is unknown or a file cannot be read
(`settings_read_error`, `runtime_read_error`); a state left stale by a failed
state save no longer matches the settings target and is DRIFT, never current and
never absent; (3) no trustworthy state (none, or one whose paths or hashes fail
the pinned layout, named in `state_refusal`) but a registration, a runtime
directory or an `.aborted-` archive for the hash: INCOMPLETE_INSTALL (exit 1;
owner recovery, no guessed rollback); (4) NOT_INSTALLED (exit 1). A settings
document whose `hooks` structure is malformed, or that cannot be parsed at all
(including one nested deeper than the JSON decoder can follow), counts as holding
no registration (the cause in `registration_error`) and the precedence continues;
a state file nested that deeply is STATE_UNREADABLE (`state_refusal`
STATE_MALFORMED); `verify` never refuses either with exit 2 and never ends in an
unexpected error for them. A transaction that ended nonzero stays nonzero in its
receipt even when a later `verify` returns MATCH.

Every run prints one JSON receipt (last stdout line); once the manifest has been
verified the receipt also names the rollback command with both pins (a refusal
before that point, such as a wrong installer or manifest pin, names none; a
`promote` that finds no install manifest to pin prints `rollback_command` null
with `rollback_command_unavailable` INSTALL_MANIFEST_MISSING). A
refusal before the evidence directory is verified is printed only, with
`receipt_saved` false; otherwise the receipt is also written under
`<evidence directory>/receipts/`, first under a temporary name that is renamed to
`<UTC>-<mode>-<pid>.json` only after the whole receipt is written and fsynced (a
leftover temporary receipt is named in `receipt_save_temp_path`; it is not
authoritative: its content is the bytes prepared for the final name, and the
printed receipt of that run, which says `receipt_saved` false, is the record).
Once the
temporary settings file has been created, every failure names it in `temp_path`
and exits 7; failures before that point (for example the runtime copy, the
preimage copy or a receipt file) have no temporary settings file to name. After a
rollback `verify` reports NOT_INSTALLED (INCOMPLETE_INSTALL while an `.aborted-`
archive for the hash remains, step 3) and `rollback`/`promote` refuse until a
new `install`.

The printed rollback command works only while all of these hold: the installer
at `/home/claude/bukerov-twitch-miner-go/.claude/hooks/install_permission_guard.py`
still hashes to its `--expect-sha256` value, the install manifest still hashes to
its `--expect-manifest-sha256` value, the guard source at
`.claude/hooks/permission_guard.py` still hashes to the manifest's
`source_sha256` (every mode checks all three before it reads the state), and the
settings file still hashes to the recorded postimage. After a checkout that
changes or removes those files (for example closing the PR and switching back to
`release/0.3`), check out `chore/permission-guard` at the installed head first;
otherwise the installer refuses (exit 2), and a settings change is reported as
DRIFT or POSTIMAGE_UNKNOWN for the owner to decide.

Tests pass `--test-root DIR` (an existing real directory directly under the
system temporary directory): every pinned constant, the manifest paths included,
is rebased under DIR, and the test seams (`--test-barrier DIR`,
`--test-crash-before-replace`, `--test-short-write`, `--test-fail-after-write`
and the single failure-injection option `--test-fault POINT`, POINT one of
`postimage-read runtime-copy-write interrupt-before-replace
interrupt-after-replace state-save receipt-save dir-fsync temp-readback
state-read signal-before-scope signal-at-scope-entry signal-at-first-write
signal-after-archive-rename signal-after-disabled-rename
signal-after-superseded-rename signal-after-receipt-snapshot
signal-after-final-observation pause-in-scope post-creation-check
transaction-boundary`) are accepted only together with it; an unknown POINT is
refused (exit 2) before anything is written. `post-creation-check` fails the
run's first strict check after a directory creation, to exercise the post-write
rule; `transaction-boundary` records at that first boundary whether the run is
already in the transaction-write state, then fails there. The
`interrupt-*` points raise KeyboardInterrupt from code, to exercise the failure
paths; a real signal no longer takes that path. The `signal-*` points send the
installer SIGINT at that fixed place. `pause-in-scope` waits inside the deferral
scope, before the state and receipt are written, until `DIR/pause-release`
exists (at most 30 s) so that a test can send real signals; `--test-barrier`
waits at most 15 s.

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
Cases H1–H10 (H11 optional) must all PASS for `NATIVE_INERT_PASS`. Verdict rules
(`evaluate_case`): the stream must hold exactly one Bash tool_use, whose command
equals the requested one byte for byte, otherwise INVALID (retried once). An
expected allow needs a non-error tool_result for that tool_use with a
`FAKE_GH ARGV=` line whose argv equals `shlex.split(COMMAND)[1:]`. An expected
denial needs no execution evidence, exactly one `is_error` tool_result, and a
`permission_denied` event or `permission_denials` entry naming the tool_use id;
without that structure a result text starting with "Permission for this tool use
was denied." counts and is recorded as DENIAL_BY_RESULT_TEXT; any other error
result is INVALID. The receipt carries the harness's own SHA-256 and the SHA-256
of the task record written for each attempt; `ROOT/evidence` (the record's
`evidence_root`) is created with the project.
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
