# Prompt observer and guard explain CLI (G1)

Four files under `.claude/hooks/` let the owner see which permission prompts an
unattended session still raises, and why the PermissionRequest guard
(`README-permission-guard.md`) did or did not answer them.

| file | role |
| --- | --- |
| `prompt_observer.py` | log-only `Notification` hook for `permission_prompt`, and the offline `explain` CLI |
| `test_prompt_observer.py` | tests TO-1 to TO-9 (shape, value-leak property, transcript binding, hook end to end, explain parity) and TR-B2 (registration shape, hooks switch) |
| `install_prompt_observer.py` | owner-run, hash-pinned, journalled, locked installer: `install`, `status`, `uninstall`, `remove-entry` |
| `test_install_prompt_observer.py` | tests TI-1 to TI-10 (transitions, refusals, interruption recovery) and the G1 repair tests TR-B1, TR-B3, TR-B4, TR-P1, TR-S |
| `README-prompt-observer.md` | this file |

## Purpose and trust boundary

* The observer **cannot decide anything**. A `Notification` hook cannot allow,
  deny or change a tool call; the observer also prints nothing and exits 0 on
  every path. The guard stays the only hook with ALLOW authority.
* The observer and `explain` execute guard code only when it is the one
  registered PermissionRequest runtime copy — an entry with matcher exactly
  `Bash` whose inner hook has type exactly `command`, with no guard-shaped
  command under any other matcher or hook type, and `disableAllHooks` not true
  in the local or project settings — at its installer-defined path
  `/home/claude/.claude/hooks/permission-guard/<HASH12>/permission_guard.py`,
  owned by the current user, not group or other writable, whose bytes hash to
  the registered SHA-256 **and** whose SHA-256 is in `KNOWN_GUARD_SHA256` inside
  `prompt_observer.py`. The bytes are read once, hashed, compiled and executed
  from memory: no import system, no `sys.modules` entry, no bytecode file. No
  option names a guard file or a hash, so neither mode ever runs a caller-chosen
  file.
* The observer's runtime copy lives outside the clone, mode 0444, pinned by its
  hash in the registration, so a branch change cannot change hook code (the
  same model as the guard).
* Deny rules, Draft-only publication and the owner's merge stay the hard
  boundary. Neither the observer nor `explain` is a sandbox against code that
  deliberately subverts them under the same OS account.

## What is stored, and what never is (GC-OD-2)

One JSON line per permission prompt that waited for the owner, appended to
`<project>/.scratch/prompt-observer/prompts.jsonl` (directory 0700, file 0600):

```json
{"event":"permission_prompt","guard":"NO_DECISION","mode":"given","pending":1,"reason":"record not active/disarmed","session":"0123abcd","shape":{"expansion":false,"more_segments":0,"ops":[],"segments":[{"assign":0,"flags":["--get"],"operands":1,"program":"git","sub":["config"]}],"unparsed":false},"source":"pending_tool_use","tool":"Bash","ts":"2026-10-18T12:00:00Z","v":1}
```

(that is `git config --get user.email` while the task record is closed).

* `session` — the first 8 hex characters of the session id, else `unknown`.
* `source` — `pending_tool_use` (exactly one tool call of this session has no
  result yet), `no_pending_tool_use`, `ambiguous` (two or more; `pending` is
  the count, no tool name and no shape are recorded and no candidate is chosen)
  or `transcript_unreadable` (`reason` = `binding`, `truncated` or `no_command`).
* `tool` — a member of `TOOLS`, `mcp` for an `mcp__…` tool, else `OTHER`.
* `shape` — for Bash only (else `null`): `segments` (at most 16; the rest in
  `more_segments`), `ops` (operator tokens, `?op` for an unknown one),
  `unparsed` (shlex failed), `expansion` (the text contains `$` or a backtick).
  A segment holds `program` (a `PROGRAMS` member or `OTHER`), `sub` (git, gh
  and go subcommand words that are members of `GIT_SUBS`, `GH_TOP`/`GH_SECOND`,
  `GO_SUBS`, else `?`), `flags` (at most 32: a `FLAG_VOCAB` member, a
  single-character short flag, `-N` for a numeric flag, else `-?`),
  `operands` (a count of tokens: flag values, positionals, redirection
  targets) and `assign` (a count of leading `NAME=VALUE` tokens). A
  `VALUE_FLAGS` member written without `=` takes the next token as its value,
  whatever it looks like (`--title --draft` records `--title` and one operand).
  For `OTHER` programs no flag name is kept: `flags` is `[]` and `flag_count`
  counts them.
* `guard` / `reason` — `ALLOW` (reason `null`), `NO_DECISION` (a guard reason
  literal, `no matching shape`, `error:<ExceptionClass>`, or `unlisted_reason`
  when a reason is not short plain text) or `NOT_EVALUATED` (`no_registration`,
  `registration_shape`, `hooks_disabled`, `hooks_switch_invalid`,
  `hooks_switch_unknown`, `guard_hash`, `guard_unknown`, `guard_import`,
  `not_bash`, `no_command`, `ambiguous`, `binding`, `truncated`).
* `mode` — `given` when the notification carried a known `permission_mode`,
  else `assumed_default` (the guard is then asked with `default`).

**Never stored or printed:** any operand, flag value, assignment, body, title,
path, branch name, PR number, token, hash or number taken from the command, a
program or flag name outside the constant sets, the raw command, and the
notification's `message` and `title`.

The constant sets (`PROGRAMS`, `GIT_SUBS`, `GH_TOP`, `GH_SECOND`, `GO_SUBS`,
`FLAG_VOCAB` — the 53 flag literals of the base guard grammar plus the listed
common flags —, `VALUE_FLAGS`, `TOOLS`, `OPS`) are written literally in
`prompt_observer.py`. `KNOWN_GUARD_SHA256` holds only the reviewed base guard
`5b16b120defa408b80047dd144c0ee1cc1dfbee519c9f51b2b92a6d322ffc0d1`. When G2
promotes a new guard, its PR must add the promoted hash, and the owner must
reinstall the observer from that PR's bytes; until then the observer and
`explain` report `NOT_EVALUATED guard_unknown` for the new guard.

## Registration (written only by the installer)

```json
{"hooks": {"Notification": [{"matcher": "permission_prompt", "hooks": [
  {"type": "command", "command": "python3 -I RUNTIME --expect-sha256 SHA", "timeout": 10}]}]}}
```

`RUNTIME` = `/home/claude/.claude/hooks/prompt-observer/<HASH12>/prompt_observer.py`
(mode 0444; `HASH12` = the first 12 hex of its SHA-256), `SHA` = its full
SHA-256. The entry is appended to the git-ignored `.claude/settings.local.json`;
every other key and value stays as it was. A copy whose hash differs from `SHA`
writes nothing.

## Owner commands (a plain terminal on the NUC, OS user `claude`)

`INSTALLER_SHA` below stands for the SHA-256 that `sha256sum` prints for
`install_prompt_observer.py`, and `OBSERVER_SHA` for the one it prints for
`prompt_observer.py` — both from the reviewed commit, compared with the values
the contract run printed.

```sh
sha256sum /home/claude/bukerov-twitch-miner-go/.claude/hooks/install_prompt_observer.py /home/claude/bukerov-twitch-miner-go/.claude/hooks/prompt_observer.py
python3 -I /home/claude/bukerov-twitch-miner-go/.claude/hooks/install_prompt_observer.py install --expect-sha256 INSTALLER_SHA --source-sha256 OBSERVER_SHA
python3 -I /home/claude/bukerov-twitch-miner-go/.claude/hooks/install_prompt_observer.py status --expect-sha256 INSTALLER_SHA
```

`status` is read-only (it takes no lock and creates nothing) and prints `MATCH`
(exit 0) when the journal says installed, the settings file is byte-identical
to the installed postimage, the runtime copy has its hash and exactly one
Notification command equals the registration. Claude Code applies a `hooks`
edit to a running session without a restart.

INSTALL PROTOCOL:

To install, run install and then status. MATCH means the observer is installed, even if install printed nothing. NOT_INSTALLED means install left no registration and no journal: run install once more. PENDING_INSTALL, PENDING_UNINSTALL or DRIFT: complete the removal protocol, then run install once more. next_step owner-review: stop. Run install at most twice.

Undo:

```sh
python3 -I /home/claude/bukerov-twitch-miner-go/.claude/hooks/install_prompt_observer.py uninstall --expect-sha256 INSTALLER_SHA
python3 -I /home/claude/bukerov-twitch-miner-go/.claude/hooks/install_prompt_observer.py remove-entry --expect-sha256 INSTALLER_SHA
```

Which of the two to run is what `status` prints as `next_step` (see the
REMOVAL PROTOCOL below).

* `uninstall` restores the exact preimage bytes and file mode, but only while
  the settings file is still byte-identical to the installed postimage (or
  already equals the preimage or the result of an earlier `remove-entry`). If
  anything else changed the file since (a plugin toggle, another installer), it
  refuses with `SETTINGS_DRIFT` (exit 4) and writes nothing.
* `remove-entry` saves the current bytes as
  `prompt-observer/settings-before-remove-<UTC>.json`, removes exactly the one
  observer hook (its entry too when it held no other hook, and an emptied
  `Notification` list), keeps every other key and value, and proves that by a
  JSON comparison. The file is re-serialised, so it is no longer byte-identical
  to any earlier version. When the observer hook is already gone (removed by
  hand or by an interrupted run) it writes no settings and only finishes
  (`ENTRY_ALREADY_ABSENT`, exit 0); when it is registered twice or more it
  refuses (`DUPLICATE_ENTRY`, exit 4, next_step owner-review).
* Both rename the runtime directory to `<HASH12>.disabled-<UTC>` and the
  journal to `state.json.uninstalled-<UTC>`.

Every run prints exactly one JSON line with `result` and `next_step`
(`none`, `status`, `uninstall`, `remove-entry` or `owner-review`). Receipts
print hashes, paths of the observer's own files, the observer's registration
command and the number of entries per hook event — never another hook command
or any settings value. Exit codes: 0 done (`status`: MATCH); 1 `status` DRIFT
or PENDING_*; 2 usage, self-hash or source-hash mismatch (nothing read or
written); 3 precondition or unsafe directory (`status`: NOT_INSTALLED); 4 drift,
conflict or lock (`status`: BLOCKED); 5 already installed; 6 write failed
(`{"error", "next_step": "status", "result": "WRITE_FAILED"}`, or a failure
while printing the receipt of an operation that completed; `status` shows the
final state).

A refusal writes nothing, with exactly these exceptions: after the directory
chain check a writing mode may create the empty lock file; a refusal that
happens after this run began its own change rolls back only its own work — an
`install` renames the journal it wrote (only while its bytes are still the
ones it wrote) to `state.json.aborted-<UTC>` and the runtime directory it
created (only while it is still that directory) to `<HASH12>.aborted-<UTC>-<pid>`
and prints `"foreign_state": true` when it found anything else in their place;
an `uninstall` or `remove-entry` that refuses `DRIFT_BEFORE_WRITE` puts the
journal back to its exact earlier bytes, and `remove-entry` renames its backup
copy to `<name>.aborted-<UTC>`. An `install` that finds a runtime directory
with no journal and no observer entry (left by an older interrupted run)
renames it to `<HASH12>.orphaned-<UTC>`. The only deletion the installer ever
makes is of its own temporary files (`.settings.local.json.tmp-…`,
`.state.json.tmp-…`), identified by the device and inode of its own descriptor.
Journals written by the earlier G1 installer are accepted, including copy
names with its `-N` collision suffix (for example
`settings-preimage-<UTC>.json-1`).

## Recovery after an interruption

`prompt-observer/state.json` is a journal (`pending_install`, `installed`,
`pending_uninstall`), validated before use (lowercase-hex hashes, the runtime
path and registration built from the runtime hash, copy names inside
`prompt-observer/`, a private regular file of mode 0600 with one link) and
written before every change it announces; a journal that fails validation is
never repaired automatically (`STATE_CORRUPT` or `STATE_UNSAFE`, owner-review).

REMOVAL PROTOCOL:

To remove the observer, run status and act on what it prints: MATCH, run uninstall; next_step uninstall, run uninstall; next_step remove-entry, run remove-entry; NOT_INSTALLED, removal is complete; next_step owner-review, stop and run no installer mode until the printed reason has been reviewed. Run status again after every uninstall or remove-entry. Run at most three uninstall or remove-entry commands in total; if status still does not print NOT_INSTALLED after the third, stop as for owner-review.

Every `LOCKED`, `DRIFT_BEFORE_WRITE`, `SETTINGS_DRIFT` or `WRITE_FAILED` result
counts against these bounds. The `status` rows, evaluated in order (S = the
settings hash, n = how many Notification commands equal the registration, P =
the preimage copy is intact):

| row | condition | result | next_step | exit |
| --- | --- | --- | --- | --- |
| 1 | directory chain unsafe | BLOCKED `ROOTS_UNSAFE` | owner-review | 4 |
| 2 | settings file fails a precondition | BLOCKED `SETTINGS_INVALID` | owner-review | 4 |
| 3 | no journal, but a Notification command contains `prompt_observer.py` | BLOCKED `ORPHAN_ENTRY` | owner-review | 4 |
| 4 | no journal | NOT_INSTALLED | none | 3 |
| 5 | journal unsafe or invalid | BLOCKED `STATE_CORRUPT` | owner-review | 4 |
| 5a | runtime directory present and unsafe | BLOCKED `ROOTS_UNSAFE` (`runtime_dir`) | owner-review | 4 |
| 6 | installed, S = postimage, runtime hash ok, n = 1 | MATCH | none | 0 |
| 7 | S = postimage and P | PENDING_INSTALL, PENDING_UNINSTALL or DRIFT | uninstall | 1 |
| 8 | S = preimage, or S = the removal result | PENDING_INSTALL, PENDING_UNINSTALL or DRIFT | uninstall | 1 |
| 9 | n = 1 or n = 0 | PENDING_INSTALL, PENDING_UNINSTALL or DRIFT | remove-entry | 1 |
| 10 | n ≥ 2 | BLOCKED `DUPLICATE_ENTRY` | owner-review | 4 |

`uninstall` and `remove-entry` decide with the same rows, so the command
`status` names makes progress unless a concurrent change intervenes.

**owner-review** means: the command wrote nothing; the installer found a
directory, journal, copy, lock or settings state it may not trust or
interpret; no installer mode may be used to force past it, and permissions,
files and the settings are not changed by hand to make it pass until the
reason has been reviewed. A refusal that names owner-review is a safe terminal
state, not a successful removal. While an entry stays registered the observer
only logs and cannot allow, deny or change any tool call.

**Residual window.** Every settings replacement re-reads the settings file
immediately before the atomic rename and aborts with `DRIFT_BEFORE_WRITE` when
it changed; a writer that does not take the installer lock and changes the
file inside the remaining interval between that re-read and the rename — a
logical interval of a few system calls, not bounded in time — can still be
overwritten, and its change is then recoverable only from the preimage copy
`settings-preimage-<UTC>.json` or the backup copy
`settings-before-remove-<UTC>.json` in `prompt-observer/`.

## Installer lock and directory chain

`install`, `uninstall` and `remove-entry` hold the advisory lock
`/home/claude/.claude/hooks/.prompt-observer-installer.lock` (`flock`, at most
50 attempts 100 ms apart, else `LOCKED`, exit 4) for exactly one invocation; the
descriptor is closed on every exit, and the empty 0600 lock file stays for later
runs. It serialises installer runs only: it does not stop any other writer of
`.claude/settings.local.json`. A lock file that is a symlink, has another owner,
mode or a second link is refused (`LOCK_UNSAFE`, owner-review). `status` takes
no lock.

Before anything else is read, every mode checks the directory chain with one
`lstat` per component: every directory above your home directory a real
directory owned by root without group or other write (unless sticky, like
`/tmp`); your home directory and every directory down to
`/home/claude/.claude/hooks` a real directory owned by you without group or
other write; `prompt-observer/`, when it exists, owned by you with mode exactly
0700; a runtime directory `<HASH12>/` owned by you without group or other
write. An unsafe chain is refused without writing anything (`ANCESTOR_UNSAFE`,
`HOOKS_ROOT_UNSAFE`, `OBSERVER_ROOT_UNSAFE`, `RUNTIME_DIR_UNSAFE`; exit 3,
owner-review; `status`: BLOCKED `ROOTS_UNSAFE` with `chain_component` and
`chain_fault`). A mildly over-permissive directory is refused like a symlinked
or foreign-owned one, because once another account can write there no journal,
copy or runtime below it can be trusted; the installer never chmods, chowns,
moves or cleans up a directory to make the check pass. The clone's own
directories (where `settings.local.json` and the observer source live) are not
part of this walk.

## Interaction with the guard installer (GC-OD-4)

`install_permission_guard.py verify`, `rollback` and `promote` compare the
WHOLE settings file with the postimage hash they recorded. From the observer
install on, `verify` reports drift and `rollback` and `promote` refuse
(SETTINGS_DRIFT).

* Before any guard `rollback` or `promote`, run the observer `uninstall`; the
  guard installer works again only if nothing else changed the file.
* After `remove-entry` the guard installer stays in drift until G2 re-baselines
  it (G2 makes it tolerate or manage the observer entry).
* The guard's emergency off-switch does not depend on any of this: a closed or
  missing task record makes the guard answer nothing.

## Log rotation and refused locations

The hook never rotates, chmods or replaces anything. When `prompts.jsonl` is
over 4 MiB it stops appending; rotate by renaming the file (for example to
`prompts.jsonl.1`) — the next prompt starts a new one. The hook writes nothing
when `.scratch` is not a real directory owned by you, when
`.scratch/prompt-observer` is a symlink or has any mode other than 0700, or
when `prompts.jsonl` is a symlink, a hard link, not a regular file, not owned
by you or has any mode other than 0600; fix such a path yourself. Every open
also uses `O_NONBLOCK`, so a FIFO planted at a path never blocks the hook.

## Explain

```sh
python3 -I .claude/hooks/prompt_observer.py explain --command-file FILE [--project DIR] [--cwd DIR] [--permission-mode MODE] [--subagent]
```

`FILE` holds the command (a regular file, not a symlink, at most 8192 bytes,
UTF-8; one trailing newline is removed). The project defaults to
`CLAUDE_PROJECT_DIR`, then the current directory; `cwd` to the project; the
mode to `auto` (one of `default`, `auto`, `plan`, `acceptEdits`, `dontAsk`,
`bypassPermissions`); `--subagent` adds `"agent_id":"explain-subagent"` to the
payload. The guard is found only through the registration, as above. It prints
one JSON line `{"decision","reason","guard_sha256","shape","scope","switch"}`
(decision `ALLOW`, `NO_DECISION` or `NOT_EVALUATED`) with the payload session
id `explain`, and writes nothing; every usage error prints one fixed line to
stderr and exits 2. `--test-root DIR` (tests only) replaces the hooks root by
`DIR/home/.claude/hooks` and adds `"test_root": true`.

* `scope` is always `guard_function`: the decision is what the registered
  guard function answers **if** Claude Code invokes it.
* `switch` names the readable settings file that decided `disableAllHooks`:
  `local` (`.claude/settings.local.json` has the key), else `project`
  (`.claude/settings.json` has it), else `unset` (also printed when the result
  was decided before the switch was read). `true` gives `NOT_EVALUATED
  hooks_disabled`, a non-boolean `hooks_switch_invalid`, an unreadable,
  symlinked or invalid project settings file `hooks_switch_unknown`.
* What this does not prove: the user (`~/.claude/settings.json`), flag
  (`--settings`) and managed policy settings are never read, so their effect
  is not verified; that Claude Code really invokes the guard live is shown
  only by a live canary, and a proof bound to one tool call needs the G2 guard
  decision receipt.

## Tests

```sh
python3 -I .claude/hooks/test_prompt_observer.py -v [--observer PATH]
python3 -I .claude/hooks/test_install_prompt_observer.py -v [--installer PATH]
```

Both run as scripts, never through `python3 -I -m unittest`; zero collected
tests or any skipped test fails the run. They build a temporary HOME, project
and hooks root (every directory with an explicit mode, so the chain check never
depends on the umask); the guard fixture is a byte copy of the clone's
`permission_guard.py` (it must hash to a `KNOWN_GUARD_SHA256` member). Hook-mode
tests run a copy of the observer in which only the `HOOKS_ROOT` constant line
points at the test layout, because hook mode has no test option.

## Known limits

* The transcript format is not a documented interface. The notification is
  bound to its transcript only by its session id and file name
  (`~/.claude/projects/<project-dir>/<session_id>.jsonl`, entries with another
  `sessionId` skipped); if a build differs, the line says
  `transcript_unreadable` with reason `binding`.
* Only the last 1 MiB of the transcript is read; two or more pending tool calls
  are logged as `ambiguous`, without a shape.
* Whether a Remote Control session emits `permission_prompt` is what the live
  canary measures. In sessions that send permission requests to a `canUseTool`
  host the notification fires about six seconds after Claude asks, and not at
  all when the owner or a PermissionRequest hook answers sooner;
  `CLAUDE_CODE_DISABLE_PERMISSION_PROMPT_NOTIFY_HOOKS=1` turns it off there.
  **An absent log line never proves that the guard answered.**
* `explain` evaluates the guard with the caller's environment; the live
  Bash-tool environment may differ (for example a `GIT_*` name makes the guard
  answer nothing for every git form).
