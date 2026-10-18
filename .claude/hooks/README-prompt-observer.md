# Prompt observer and guard explain CLI (G1)

Four files under `.claude/hooks/` let the owner see which permission prompts an
unattended session still raises, and why the PermissionRequest guard
(`README-permission-guard.md`) did or did not answer them.

| file | role |
| --- | --- |
| `prompt_observer.py` | log-only `Notification` hook for `permission_prompt`, and the offline `explain` CLI |
| `test_prompt_observer.py` | tests TO-1 to TO-9 (shape, value-leak property, transcript binding, hook end to end, explain parity) |
| `install_prompt_observer.py` | owner-run, hash-pinned, journalled installer: `install`, `status`, `uninstall`, `remove-entry` |
| `test_install_prompt_observer.py` | tests TI-1 to TI-10 (transitions, refusals, interruption recovery) |
| `README-prompt-observer.md` | this file |

## Purpose and trust boundary

* The observer **cannot decide anything**. A `Notification` hook cannot allow,
  deny or change a tool call; the observer also prints nothing and exits 0 on
  every path. The guard stays the only hook with ALLOW authority.
* The observer and `explain` execute guard code only when it is the one
  registered PermissionRequest runtime copy at its installer-defined path
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
  `registration_shape`, `guard_hash`, `guard_unknown`, `guard_import`,
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

`status` is read-only and prints `MATCH` (exit 0) when the journal says
installed, the settings file is byte-identical to the installed postimage, the
runtime copy has its hash and exactly one Notification command equals the
registration. Claude Code applies a `hooks` edit to a running session without
a restart.

Undo:

```sh
python3 -I /home/claude/bukerov-twitch-miner-go/.claude/hooks/install_prompt_observer.py uninstall --expect-sha256 INSTALLER_SHA
python3 -I /home/claude/bukerov-twitch-miner-go/.claude/hooks/install_prompt_observer.py remove-entry --expect-sha256 INSTALLER_SHA
```

* `uninstall` restores the exact preimage bytes and file mode, but only while
  the settings file is still byte-identical to the installed postimage (or
  already equals the preimage). If anything else changed the file since (a
  plugin toggle, another installer), it refuses with `SETTINGS_DRIFT` (exit 4)
  and writes nothing.
* Then run `remove-entry`: it saves the current bytes as
  `prompt-observer/settings-before-remove-<UTC>.json`, removes exactly the one
  observer hook (its entry too when it held no other hook, and an emptied
  `Notification` list), keeps every other key and value, and proves that by a
  JSON comparison. The file is re-serialised, so it is no longer byte-identical
  to any earlier version.
* Both rename the runtime directory to `<HASH12>.disabled-<UTC>` and the
  journal to `state.json.uninstalled-<UTC>`. Nothing is ever deleted.

Every refusal prints one JSON line and writes nothing. Receipts print hashes,
paths of the observer's own files, the observer's registration command and the
number of entries per hook event — never another hook command or any settings
value. Exit codes: 0 done (`status`: MATCH); 1 `status` DRIFT or PENDING_*;
2 usage, self-hash or source-hash mismatch; 3 precondition (`status`:
NOT_INSTALLED); 4 drift or conflict; 5 already installed; 6 write failed.

## Recovery after an interruption

`prompt-observer/state.json` is a journal (`pending_install`, `installed`,
`pending_uninstall`), written before every settings change it announces. If an
`install`, `uninstall` or `remove-entry` was interrupted, `status` prints
`PENDING_INSTALL` or `PENDING_UNINSTALL`: **run `uninstall`**. It completes the
same transition from wherever it stopped (it restores the preimage, or sees the
preimage or the removal result already in place and only renames), and running
it again is harmless (`NOT_INSTALLED`, exit 3, once finished). A failed
`install` that never replaced the settings file renames its runtime directory to
`<HASH12>.aborted-<UTC>-<pid>` and its journal to `state.json.aborted-<UTC>`.

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
one JSON line `{"decision","reason","guard_sha256","shape"}` (decision `ALLOW`,
`NO_DECISION` or `NOT_EVALUATED`) with the payload session id `explain`, and
writes nothing; every usage error prints one fixed line to stderr and exits 2.
`--test-root DIR` (tests only) replaces the hooks root by
`DIR/home/.claude/hooks` and adds `"test_root": true`.

## Tests

```sh
python3 -I .claude/hooks/test_prompt_observer.py -v [--observer PATH]
python3 -I .claude/hooks/test_install_prompt_observer.py -v [--installer PATH]
```

Both run as scripts, never through `python3 -I -m unittest`; zero collected
tests or any skipped test fails the run. They build a temporary HOME, project
and hooks root; the guard fixture is a byte copy of the clone's
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
