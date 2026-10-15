#!/usr/bin/env python3
"""Installer for the PermissionRequest guard (s1h-contract-rev12, section 11; run 20261011T173421Z).

Owner-run only (S1H-OD-1 = OPERATOR). Every mode requires BOTH integrity pins:

    python3 -I install_permission_guard.py install  --manifest EVIDENCE_ROOT/r9-install-manifest.json \
        --expect-sha256 <sha256 of this file> --expect-manifest-sha256 <sha256 of that manifest>
    python3 -I install_permission_guard.py verify   (same manifest and pins; never writes)
    python3 -I install_permission_guard.py rollback (same manifest and pins)
    python3 -I install_permission_guard.py promote  --manifest EVIDENCE_ROOT/r9-promote-manifest.json ...

Authorization boundary (11.0). Every destination is a constant in this file; the writable manifest can
only repeat these exact values and can never widen them:
  CLONE           /home/claude/bukerov-twitch-miner-go
  SOURCE          CLONE/.claude/hooks/permission_guard.py
  SETTINGS        CLONE/.claude/settings.local.json
  HOOKS_DIR       /home/claude/.claude/hooks                (must be an existing real directory)
  RUNTIME_PARENT  HOOKS_DIR/permission-guard                (created with a plain mkdir when absent;
                                                             the manifest key `runtime_root` names it)
  runtime dir     RUNTIME_PARENT/<first 12 hex of the manifest's source_sha256>
  EVIDENCE_DIR    CLONE/.scratch/s1h-20261011T173421Z/r9-install
                  (holds settings.local.json.preimage-<UTC>-<pid>, permission-guard-state.json, receipts/)
  manifest paths  CLONE/.scratch/s1h-20261011T173421Z/r9-install-manifest.json (install, verify, rollback)
                  CLONE/.scratch/s1h-20261011T173421Z/r9-promote-manifest.json (promote)
The registration is the fixed section-4 template; a manifest `registration` must equal it exactly.
The installer hashes its own bytes first (--expect-sha256) and the manifest bytes before parsing them
(--expect-manifest-sha256); a manifest-owned source_sha256 is never independent authority.

Containment (CT1-CT6). Roots are pinned above (CT1) and used only when they are existing real
directories whose realpath equals their own normalized path (CT2). Every derived path is absolute,
normalized, without `.`, `..`, empty components or NUL (CT3); every existing component from its root
down to its parent is lstat'ed and must be a real directory, realpath(parent) must equal or lie below
the root, the basename is a plain name, and a non-existent parent is a failure (CT4); an existing
candidate is lstat'ed and must be a regular file (or, for the installer's directories, a real directory)
and not a symlink (CT5). The checks are repeated immediately before each write or rename; temporary files
are created in the verified parent with O_EXCL|O_NOFOLLOW and moved with os.replace (CT6). This narrows
the check-to-use window; it does not eliminate it (a process of the same OS account can still race it).

Write outcomes (rev15, 7.11b (2)):
  W  once os.replace of the settings target (or, for an ABSENT rollback, its rename) has returned, the
     write is recorded in the next statement (settings_written / settings_restored, temp_path,
     intended_postimage_sha256); the read-back of the postimage is a later step, and its failure is
     exit 7 POSTIMAGE_UNREADABLE with postimage_sha256 null. Any refusal after a committed write is
     FAILED / exit 7 (original_code kept), never a refusal of input. A state whose postimage is null or
     differs from the intended one is never acted on: rollback and promote refuse POSTIMAGE_UNKNOWN
     (exit 4) and verify reports DRIFT.
  A  before the replacement has returned, every handled failure after this run's mkdir of <HASH12>
     renames that directory to <HASH12>.aborted-<UTC>-<pid> exactly once (failed_step and
     runtime_renamed_to in the receipt).
  F3 after the replacement has returned, no failure touches the runtime directory: the write is
     recorded, failed_step is set, state and receipt are saved, exit 7.
  S  the state file is saved before the receipt; when that save fails the result is
     <RESULT>_STATE_NOT_SAVED with state_saved false and original_result / original_reason kept (exit 7);
     a receipt that cannot be written says receipt_saved false (exit 7 unless a refusal keeps its code).

Validation before the first write (rev21, 7.11d (2)):
  V1 every check that can end a run with a refusal runs in phase P, before the handlers and before any write:
     the full structure of the existing settings (a `hooks` or `PermissionRequest` value that is null or of
     another type, or an entry that is not an object, is refused), the new document is built and encoded,
     the foreign-hook, registration, manifest and source checks, and the prospective containment of every
     path the run will create, write or rename (require_prospective: existing components as CT4/CT5, missing
     ones only installer-owned names). After each directory the installer creates (RUNTIME_PARENT, <HASH12>,
     receipts/) the strict CT2-CT5 checks run again (logged in post_creation_checks); the CT6 rechecks
     before each write or rename are unchanged.
  V2 before the first TRANSACTION write (the preimage copy, RUNTIME_PARENT, <HASH12> and anything inside it,
     the settings target, the state file, an archive or sibling name of them) every outcome keeps its code:
     0, 2, 3, 4, 5 or 6, with nothing but the refusal receipt written. After one, a refusal is FAILED /
     exit 7 with original_code, and every write the run made is named (preimage_backup_created,
     runtime_parent_created, runtime_dir_owned, runtime_renamed_to, temp_path, ...). The creation of
     RUNTIME_PARENT and the creation of <HASH12> are each, on their own, a transaction write: with an ABSENT
     preimage (no preimage copy) a failure right after the RUNTIME_PARENT mkdir is already FAILED / exit 7
     (rev23 X1). failed_step is set only after a transaction write (null otherwise); runtime_renamed_to is null
     whenever this run archived no <HASH12>, including a failure of the preimage copy itself (rev25 Y3).
  V3 verify reads a malformed hooks structure as no registration (registration_error) and continues the
     11.3 precedence; it never exits 2 for it. A document nested deeper than the JSON decoder can follow
     (RecursionError) is malformed JSON like any other, in every mode and for every file read (rev23 X2): the
     settings (SETTINGS_MALFORMED), a manifest (MANIFEST_MALFORMED) and the state (STATE_MALFORMED).

Untrusted state and truthful records (rev25, 7.11f (2)):
  Y1 every path read from the state (settings_target, runtime_dir, runtime_path, preimage.backup_path) must be
     absolute and normalized, without `.`, `..`, empty components, NUL or any surrogate code point U+D800-U+DFFF,
     encodable, at most 255 bytes per component and 4096 bytes in all, before it is compared or used; the
     containment helpers turn every OSError (other than a missing file where that is allowed) and ValueError into
     their refusal, a failed rename is RENAME_FAILED (exit 7), and an existence check of the state's runtime directory
     (entry_exists) that fails for another reason than "absent" is a refusal. A failure before the first transaction write is
     an untrustworthy state: rollback and promote refuse it in phase P (exit 2), verify follows 11.3. A recheck that
     fails after a transaction write follows V2, W, A and S (FAILED, exit 7, original_code, the writes named).
  Y2 every other value read from the state has the exact type and format the installer writes (results and reasons
     are strings, settings_written / settings_restored booleans, the hashes 64 lowercase hex, preimage.mode the
     oct() text of a permission mode, preimage.size a non-negative int, preimage.mtime a finite number, never a bool
     where a number is written; postimage_unknown is never in a state file); a failure is an untrustworthy state as in Y1.
  Y3 one place (complete_receipt, RECEIPT_FIELDS) gives every receipt the documented key set of its kind, null or
     false where a value does not apply; the receipt file and the state file are that receipt without the keys Z4 keeps
     out of them.
  Y4 an operation boolean is true only after its operation has completed: runtime_copy_created once the runtime file
     is written in full, 0444 and verified; settings_written / settings_restored once the replacement or rename
     returned; state_saved and receipt_saved once the file is in place.
  Y5 every JSON document read rejects NaN, Infinity, -Infinity and any number that becomes a non-finite float (such
     as 1e400) as malformed; every JSON document written or printed is serialized with allow_nan=False, so a
     non-finite value can never reach a file (a settings document that would need one is SETTINGS_NOT_ENCODABLE).

Integers, existence checks and post-write facts (rev27, 7.11g (2)):
  Z1/Z2 every JSON integer literal read (settings, state, manifests, the in-memory document copies) goes through
     _finite_int: more than 309 digits is rejected before any conversion, a shorter one becomes an exact int (never a
     float), and one whose conversion to float overflows (309 nines) is rejected; the rejection is the reader's
     ValueError, so it gets the malformed-document outcome of Y5, whatever the interpreter's digit limit.
  Z3 every existence check whose result enters an outcome, a receipt, a state field or a console line uses path_state
     (lstat): present, absent (FileNotFoundError only) or unreadable; os.path.exists/lexists/isfile/isdir/islink are
     never used for one. verify reports an unreadable runtime file in runtime_read_error (the failed operation and the
     errno name, e.g. "lstat: EACCES (Permission denied)") with runtime_sha256 null; elsewhere an unreadable entry is
     the caller's refusal (entry_exists).
  Z4 a written file states only facts complete before its own bytes were written: the state file holds no
     STATE_SAVE_KEYS (state_saved, state_save_error, state_save_temp_path, state_dir_fsync_error, the last known only
     after the fsync of its directory) and no receipt-only key; a receipt file holds no receipt-only key
     (receipt_saved, receipt_save_error, receipt_save_temp_path). The printed receipt carries all of them.

Interrupts (rev18, 7.11c (2), S1H-OD-7; rev21 7.11d (1)):
  P  the precondition checks write nothing and keep the default signal behaviour: Ctrl+C there raises
     KeyboardInterrupt, which ends the run as FAILED / INTERRUPTED (interrupted true, exit 7), also when it
     lands while the outcome is being taken over, before the handlers are installed (interrupted_outcome
     names what it overtook); one that lands before the arguments are parsed, or in the few instructions
     of that take-over itself, can still end the process with Python's KeyboardInterrupt and no receipt.
     SIGTERM and SIGHUP there end the process with no receipt. Nothing is written in phase P.
  E  each writing mode (install, rollback, promote) installs one handler for SIGINT, then SIGTERM, then
     SIGHUP before its first write, and in every case before it reports its outcome; verify never does and
     stays plainly interruptible. The handler stores the first signal's number and does nothing else: it
     raises nothing, performs no I/O and is never removed, so from then until the process exits these
     signals end nothing.
  W  every write follows E. Each transaction write the receipt names is recorded in the statement after it
     (D5): the RUNTIME_PARENT and <HASH12> mkdirs, the preimage copy (the moment os.open returns), the
     settings replacement or ABSENT rename, and the .aborted-, .disabled-, .superseded- and rolled-back-
     renames. The runtime copy file is covered by its recorded <HASH12>; receipts/ and the receipt file are
     evidence-only (a failure there is receipt_saved false).
  D3 the state and the final receipt each record interrupt_deferred (bool) and interrupt_signal (the first
     signal's name or null) as observed at the record's snapshot, taken immediately before it is
     serialized. The printed receipt carries the final receipt's snapshot. A signal that arrives after that
     snapshot is reported once on stderr at the final console-observation point, saying whether the final
     receipt was saved to disk, only printed, or neither; a signal first observed after that point, or not
     processed before the process ends, leaves no trace.
  D4 a deferred signal never changes the outcome or the exit code. A receipt that cannot be printed does not
     either: the failed stream is pointed at /dev/null so the interpreter's final flush cannot fail.
  Not covered: SIGKILL, SIGQUIT, SIGSTOP, power loss or a host crash; several signals are recorded as the
  first one only; a sequence of file operations is not atomic as a whole.

Tests pass --test-root DIR (an existing real directory directly under the system temporary directory):
every pinned constant, including the manifest paths, is then rebased under DIR, every receipt carries
`test_root`, and the test seams --test-barrier DIR (contained in DIR, waits at most 15 s),
--test-crash-before-replace, --test-short-write, --test-fail-after-write and --test-fault POINT (POINT
from TEST_FAULT_POINTS) are accepted only together with it. The signal-* points send this process
SIGINT at a fixed place; pause-in-scope waits at most 30 s inside the scope for DIR/pause-release; the
interrupt-* points raise KeyboardInterrupt from code, which a real signal no longer does; post-creation-check
fails the run's first strict check after a directory creation (V1); transaction-boundary records, at the run's
first post-creation boundary, whether the run is already in the transaction-write state, then fails there
(rev23 X1); insert-before-serializer puts the number written in DIR/insert-before-serializer into the validated
settings document right before the production serializer runs (rev25 Y5); state-dir-fsync fails the fsync of the
state file's directory, and nothing else of that write (rev27 Z4). --test-barrier waits after phase P in
install (after the preimage copy), promote and rollback (both before the first write).

Every run prints one JSON receipt as its last stdout line; once the manifest is verified the receipt
also names the rollback command with both pins (null, with rollback_command_unavailable, when a promote finds no
install manifest to pin). Exit codes:
  0 installed / idempotent / MATCH / rolled back / promoted
  1 verify DRIFT, INCOMPLETE_INSTALL (no trustworthy state but a registration, runtime dir or .aborted-
    archive), NOT_INSTALLED or STATE_UNREADABLE
  2 refused input: a pin, a manifest value, a flag, a path that fails containment, malformed JSON or a
    malformed hooks structure, an unreadable state file or runtime parent (nothing written but the
    refusal receipt)
  3 FOREIGN_HOOK (another PermissionRequest entry present)
  4 rollback or promote refused: settings drifted from the recorded postimage, or POSTIMAGE_UNKNOWN;
    install refused RUNTIME_DRIFT (registration present but the runtime file is not intact)
  5 runtime directory for that hash exists, or promote with the same hash
  6 DRIFT_BEFORE_WRITE (settings changed between the preimage and the write, before any transaction write)
  7 write failed or interrupted (a temporary settings file, once created, is named in the receipt),
    or any refusal or failure after a transaction write (original_code keeps a refusal's own code), or a
    state or receipt that could not be saved
  codes 2 to 6 are only ever reported before the run's first transaction write (V2)
verify precedence: STATE_UNREADABLE (unreadable, symlinked, irregular or malformed state); a readable
state with a committed write (MATCH/DRIFT; DRIFT when the postimage is unknown or a file cannot be read);
INCOMPLETE_INSTALL (no trustworthy state, but a registration, runtime dir or .aborted- archive remains);
NOT_INSTALLED. Receipts and the state file are ASCII-escaped JSON. A receipt file is written under a
temporary name in receipts/ and renamed to its final name only after a complete, fsynced write; a leftover
temporary file is named in receipt_save_temp_path and is not authoritative (its content is the bytes prepared
for the final name; the printed receipt, which says receipt_saved false, is the record of that run).
A refusal before the evidence directory is verified is printed only (never written to an
unverified path) and says receipt_saved false. The installer never deletes anything: it renames (rollback, promote, a failed
install's runtime dir -> <HASH12>.aborted-<UTC>-<pid>) and replaces atomically.
"""

import argparse
import errno
import hashlib
import json
import math
import os
import re
import secrets
import signal
import stat
import sys
import tempfile
import time

CLONE = "/home/claude/bukerov-twitch-miner-go"
HOOKS_DIR = "/home/claude/.claude/hooks"
RUNID_DIR = "s1h-20261011T173421Z"
GUARD_FILENAME = "permission_guard.py"
STATE_FILE = "permission-guard-state.json"
RECEIPTS_DIR = "receipts"
PREIMAGE_PREFIX = "settings.local.json.preimage-"
TEMPLATE_COMMAND = "python3 -I RUNTIME --expect-sha256 SHA"
REGISTRATION_TEMPLATE = {"matcher": "Bash",
                         "hooks": [{"type": "command", "command": TEMPLATE_COMMAND, "timeout": 10}]}
MANIFEST_KEYS_REQUIRED = ("source", "source_sha256", "runtime_root", "settings_target", "evidence_dir")
MANIFEST_KEYS_ALLOWED = MANIFEST_KEYS_REQUIRED + ("registration",)

EXIT_OK = 0
EXIT_VERIFY_DRIFT = 1
EXIT_PRECONDITION = 2
EXIT_FOREIGN_HOOK = 3
EXIT_STATE_DRIFT = 4
EXIT_RUNTIME_EXISTS = 5
EXIT_DRIFT_BEFORE_WRITE = 6
EXIT_WRITE_FAILED = 7

BARRIER_TIMEOUT_SECONDS = 15
OPEN_NOFOLLOW = getattr(os, "O_NOFOLLOW", 0)
OPEN_CLOEXEC = getattr(os, "O_CLOEXEC", 0)
# --test-fault POINT (tests only, with --test-root): the fixed failure-injection points of 7.11b (2), 7.11c (2) D7, the
# single point 7.11d (2) V4 adds (post-creation-check), the single point 7.11e (2) X1 adds (transaction-boundary), the
# single point 7.11f (2) U13-write adds (insert-before-serializer) and the single point 7.11g (2) U17 adds (state-dir-fsync)
TEST_FAULT_POINTS = ("postimage-read", "runtime-copy-write", "interrupt-before-replace", "interrupt-after-replace",
                     "state-save", "receipt-save", "dir-fsync", "temp-readback", "state-read",
                     "signal-before-scope", "signal-at-scope-entry", "signal-at-first-write",
                     "signal-after-archive-rename", "signal-after-disabled-rename", "signal-after-superseded-rename",
                     "signal-after-receipt-snapshot", "signal-after-final-observation", "pause-in-scope",
                     "post-creation-check", "transaction-boundary", "insert-before-serializer", "state-dir-fsync")
STEP_AFTER_REPLACE = "after the settings replacement"
PAUSE_SECONDS = 30  # pause-in-scope (test only) waits at most this long for its release file
# V2: the receipt records of the transaction writes (each set in the statement after its write, D5). Once one of them is
# set, a refusal is FAILED / exit 7 with its own code kept in original_code, never REFUSED.
TRANSACTION_RECORD_KEYS = ("preimage_backup_created", "runtime_parent_created", "runtime_dir_owned", "temp_path",
                           "settings_written", "settings_restored", "settings_renamed_to", "runtime_renamed_to",
                           "previous_runtime_renamed_to")
# Y1 (rev25): limits of a path read from the state, in bytes once encoded (Linux NAME_MAX and PATH_MAX)
NAME_MAX_BYTES = 255
PATH_MAX_BYTES = 4096
# Z1/Z2 (rev27): the most digits a JSON integer inside the finite float range can have (the float maximum, about
# 1.8e308, has 309); a longer literal is rejected before any conversion
MAX_INT_DIGITS = 309
# Y3 (rev25): the documented key set of each receipt kind (README "Receipt and state fields"), each with the value it has
# when it does not apply. complete_receipt() is the one place that fills them in; the state file is the receipt of its
# writing run without the receipt-only keys, plus state_saved.
_RECEIPT_COMMON = (("mode", None), ("timestamp_utc", None), ("installer", None), ("manifest", None), ("result", None),
                   ("reason", None), ("exit_code", None), ("detail", None), ("installer_sha256_verified", None),
                   ("manifest_sha256_verified", None), ("evidence_verified", False), ("settings_target", None),
                   ("evidence_dir", None), ("rollback_command", None), ("rollback_command_unavailable", None))
RECEIPT_ONLY_KEYS = ("receipt_saved", "receipt_save_error", "receipt_save_temp_path")
# Z4 (rev27): a written file states only facts complete before its own bytes were written. The results of saving the
# state (including the fsync of its directory, which follows the state write) are therefore never in the state file, and
# the results of saving a receipt are never in the receipt file: the printed receipt (the console) carries them all, the
# receipt file carries the state's.
STATE_SAVE_KEYS = ("state_saved", "state_save_error", "state_save_temp_path", "state_dir_fsync_error")
STATE_FILE_EXCLUDED_KEYS = RECEIPT_ONLY_KEYS + STATE_SAVE_KEYS
_RECEIPT_WRITING = (("step", None), ("failed_step", None), ("original_code", None), ("original_result", None),
                    ("original_reason", None), ("interrupted", False), ("interrupted_outcome", None),
                    ("state_saved", False), ("state_save_error", None), ("state_save_temp_path", None),
                    ("state_dir_fsync_error", None), ("temp_path", None), ("dir_fsync_error", None),
                    ("postimage_sha256", None), ("intended_postimage_sha256", None), ("postimage_read_error", None),
                    ("preimage", None), ("runtime_dir", None), ("runtime_path", None), ("runtime_sha256", None),
                    ("registration_command", None), ("runtime_renamed_to", None), ("post_creation_checks", []))
_RECEIPT_NEW_RUNTIME = (("runtime_parent_created", None), ("runtime_dir_owned", None), ("runtime_copy_created", False),
                        ("settings_written", False), ("hooks", None), ("aborted_archives_present", None),
                        ("runtime_archive_error", None))
RECEIPT_FIELDS = {
    "install": _RECEIPT_COMMON + _RECEIPT_WRITING + _RECEIPT_NEW_RUNTIME + (("preimage_backup_created", None),),
    "promote": _RECEIPT_COMMON + _RECEIPT_WRITING + _RECEIPT_NEW_RUNTIME + (
        ("previous_runtime_renamed_to", None), ("previous_receipt", None), ("settings_sha256", None),
        ("expected_settings_sha256", None)),
    "rollback": _RECEIPT_COMMON + _RECEIPT_WRITING + (
        ("settings_restored", False), ("settings_renamed_to", None), ("previous_receipt", None),
        ("settings_sha256", None), ("expected_settings_sha256", None)),
    "verify": _RECEIPT_COMMON + (
        ("state_refusal", None), ("registration_error", None), ("settings_read_error", None),
        ("runtime_read_error", None), ("settings_sha256", None), ("expected_settings_sha256", None),
        ("intended_postimage_sha256", None), ("postimage_unknown", None), ("runtime_path", None),
        ("runtime_sha256", None), ("expected_runtime_sha256", None), ("registration_command", None),
        ("registration_count", None), ("state_result", None), ("state_reason", None), ("runtime_dir_present", None),
        ("aborted", None), ("registration_present", None), ("state_present", None)),
}
# Y2 (rev25): the format in which the installer writes a recorded mode (oct() of stat.S_IMODE)
MODE_TEXT = re.compile(r"0o[0-7]{1,4}")


def inject_fault(seams, point):
    """Raise the injected failure for POINT when the test asked for it. The interrupt-* points simulate an unexpected
    exception inside the scope (a KeyboardInterrupt raised by code); real signals never take that path (D7)."""
    if point in (seams or {}).get("faults", ()):
        if point.startswith("interrupt-"):
            raise KeyboardInterrupt("TEST_FAULT " + point)
        raise OSError(errno.EIO, "TEST_FAULT " + point)


def signal_fault(seams, point):
    """Test only: send SIGINT to this process at POINT (7.11c (2) D7)."""
    if point in (seams or {}).get("faults", ()):
        os.kill(os.getpid(), signal.SIGINT)


# --- interrupt deferral (7.11c (2), S1H-OD-7) ---------------------------------------------------------------
# Phase P (precondition checks, nothing written) stays normally interruptible. Phase E installs DEFERRAL_SIGNALS in
# this order; a signal whose handler is installed is deferred from then until the process exits. Phase W (the first
# write onward) begins only after all three are installed. The handler never raises, performs no I/O and calls nothing.
DEFERRAL_SIGNALS = (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)
_deferred_signal = None  # the first deferred signal's number, or None
_scope_entered = False
_first_write_reached = False  # test seam bookkeeping for signal-at-first-write only
_post_creation_fault_used = False  # test seam bookkeeping for post-creation-check only
_boundary_fault_used = False  # test seam bookkeeping for transaction-boundary only


def defer_signal(signum, frame):
    global _deferred_signal
    if _deferred_signal is None:
        _deferred_signal = signum


def enter_deferral_scope(seams=None):
    """Phase E: install the deferring handler for SIGINT, then SIGTERM, then SIGHUP. Idempotent. The caller performs the
    mode's first write only after this returns."""
    global _scope_entered
    if _scope_entered:
        return
    signal.signal(signal.SIGINT, defer_signal)
    signal_fault(seams, "signal-at-scope-entry")
    signal.signal(signal.SIGTERM, defer_signal)
    signal.signal(signal.SIGHUP, defer_signal)
    _scope_entered = True


def before_first_write(seams):
    """Called immediately before each statement that can be the run's first write; only the first call counts. Its only
    effect is the D7 test point signal-at-first-write (inert without --test-fault)."""
    global _first_write_reached
    if not _first_write_reached:
        _first_write_reached = True
        signal_fault(seams, "signal-at-first-write")


def interrupt_snapshot():
    """ONE observation of the deferral variable, taken immediately before a record is serialized (D3)."""
    observed = _deferred_signal
    return {"interrupt_deferred": observed is not None,
            "interrupt_signal": signal.Signals(observed).name if observed is not None else None}


def pause_in_scope(layout, seams):
    """Test only (pause-in-scope): a bounded wait inside the scope during which the test sends real signals; it ends when
    <test root>/pause-release appears or after PAUSE_SECONDS (D6)."""
    if "pause-in-scope" not in (seams or {}).get("faults", ()) or not layout.test_root:
        return
    write_new_file(layout.test_root + "/pause-reached", layout.test_root, b"paused\n")
    deadline = time.monotonic() + PAUSE_SECONDS
    while not os.path.exists(layout.test_root + "/pause-release") and time.monotonic() < deadline:
        time.sleep(0.05)


def insert_before_serializer(layout, document, seams):
    """Test only (insert-before-serializer, Y5 U13-write): put the number written in <test root>/insert-before-serializer
    (for example inf, -inf, nan or 1.5) into the validated in-memory settings document as `test_inserted_value`,
    immediately before the production serializer runs. Input validation is untouched."""
    if "insert-before-serializer" not in (seams or {}).get("faults", ()) or not layout.test_root:
        return
    with open(layout.test_root + "/insert-before-serializer") as fh:
        document["test_inserted_value"] = float(fh.read().strip())


class Refusal(Exception):
    def __init__(self, code, reason, **extra):
        super().__init__(reason)
        self.code = code
        self.reason = reason
        self.extra = extra


def refuse(reason, code=EXIT_PRECONDITION, **extra):
    raise Refusal(code, reason, **extra)


class Layout:
    """The pinned destinations of 11.0, optionally rebased under a test root."""

    def __init__(self, test_root=None):
        prefix = test_root or ""
        self.test_root = test_root
        self.clone = prefix + CLONE
        self.source_dir = self.clone + "/.claude/hooks"
        self.source = self.source_dir + "/" + GUARD_FILENAME
        self.settings_dir = self.clone + "/.claude"
        self.settings = self.settings_dir + "/settings.local.json"
        self.hooks_dir = prefix + HOOKS_DIR
        self.runtime_parent = self.hooks_dir + "/permission-guard"
        self.evidence_parent = self.clone + "/.scratch"
        self.evidence_root = self.evidence_parent + "/" + RUNID_DIR
        self.evidence_dir = self.evidence_root + "/r9-install"
        self.install_manifest = self.evidence_root + "/r9-install-manifest.json"
        self.promote_manifest = self.evidence_root + "/r9-promote-manifest.json"
        self.state_path = self.evidence_dir + "/" + STATE_FILE

    def manifest_for(self, mode):
        return self.promote_manifest if mode == "promote" else self.install_manifest


def utc_stamp():
    return time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())


def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def is_hex64(value):
    return isinstance(value, str) and len(value) == 64 and all(c in "0123456789abcdef" for c in value)


def _reject_duplicate_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate key: " + key)
        result[key] = value
    return result


def _reject_constant(name):
    raise ValueError(f"non-finite JSON constant {name}")


def _finite_float(text):
    value = float(text)
    if not math.isfinite(value):
        raise ValueError(f"JSON number out of range: {text[:40]}")
    return value


def _finite_int(text):
    """Z1/Z2: a JSON integer literal is valid only when it lies in the finite float range. (i) More than
    MAX_INT_DIGITS digits (sign excluded) is rejected before any conversion; (ii) otherwise it is converted to int
    exactly, never through float; (iii) a value whose conversion to float overflows (309 nines) is rejected. Every
    rejection is the ValueError of a malformed document, never an OverflowError."""
    digits = text[1:] if text.startswith("-") else text
    if len(digits) > MAX_INT_DIGITS:
        raise ValueError(f"JSON integer outside the finite float range ({len(digits)} digits)")
    value = int(text)
    try:
        float(value)
    except OverflowError as exc:
        raise ValueError(f"JSON integer outside the finite float range ({len(digits)} digits)") from exc
    return value


def loads_strict(data):
    """X2: a document nested deeper than the decoder can follow is invalid JSON like any other (the decoder raises
    RecursionError for it, FACT from r23-x2/repro-on-head.log), never an unexpected error. Y5: NaN, Infinity, -Infinity
    and any number that becomes a non-finite float (such as 1e400) are invalid JSON too. Z1/Z2: so is an integer
    literal outside the finite float range (_finite_int), whatever the interpreter's integer-string digit limit."""
    try:
        return json.loads(data.decode("utf-8"), object_pairs_hook=_reject_duplicate_keys,
                          parse_constant=_reject_constant, parse_float=_finite_float, parse_int=_finite_int)
    except RecursionError as exc:
        raise ValueError(f"nesting too deep: {exc}") from exc


def json_text(obj, ensure_ascii, indent=2, default=None):
    """Y5: the one serializer of every JSON document the installer writes or prints; a non-finite number is never
    serialized (ValueError instead)."""
    return json.dumps(obj, indent=indent, ensure_ascii=ensure_ascii, default=default, allow_nan=False)


def dumps(obj):
    return json_text(obj, ensure_ascii=False) + "\n"


def dumps_record(obj):
    """Receipts, the state file and the printed receipt: ASCII-escaped, so no text read from the settings (for example
    a lone-surrogate escape) can make serialization fail after the outcome is decided."""
    return json_text(obj, ensure_ascii=True, default=str) + "\n"


# --- containment (CT2-CT5) ----------------------------------------------------------------

def path_syntax_ok(path):
    """CT3 and Y1: absolute, normalized, no `.`/`..`/empty component, no NUL; free of every surrogate code point
    U+D800-U+DFFF (checked on the string itself, because os.fsencode with surrogateescape would accept U+DC80-U+DCFF),
    encodable with os.fsencode, every component at most NAME_MAX_BYTES and the whole path at most PATH_MAX_BYTES once
    encoded. A path that fails this is never handed to a system call."""
    if not isinstance(path, str) or not path.startswith("/") or "\0" in path:
        return False
    if any("\ud800" <= char <= "\udfff" for char in path):
        return False
    try:
        encoded = os.fsencode(path)
    except (UnicodeError, ValueError):
        return False
    if len(encoded) > PATH_MAX_BYTES or any(len(part) > NAME_MAX_BYTES for part in encoded.split(b"/")):
        return False
    if os.path.normpath(path) != path:
        return False
    return all(part not in ("", ".", "..") for part in path.split("/")[1:])


def realpath_or_refuse(path, reason, report_path):
    """Y1: os.path.realpath with every OSError and ValueError turned into the caller's refusal."""
    try:
        return os.path.realpath(path)
    except (OSError, ValueError) as exc:
        refuse(reason, path=report_path, detail=str(exc))


def require_real_dir(path, reason):
    """CT2: an existing real directory (lstat: a directory, not a symlink) whose realpath equals itself."""
    if not path_syntax_ok(path):
        refuse(reason, path=path)
    try:
        info = os.lstat(path)
    except (OSError, ValueError) as exc:  # Y1: every lstat failure is this refusal
        refuse(reason, path=path, detail=str(exc))
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        refuse(reason, path=path)
    if realpath_or_refuse(path, reason, path) != path:
        refuse(reason, path=path, detail="realpath differs")
    return path


def require_contained(candidate, root, kind, reason, must_exist=False):
    """CT3-CT5: `candidate` lies under the real directory `root`; every existing component from root
    to its parent is a real directory; realpath(parent) is root or below; plain basename; an existing
    candidate is of `kind` ("file" or "dir") and not a symlink."""
    if not path_syntax_ok(candidate):
        refuse(reason, path=candidate, detail="path syntax")
    require_real_dir(root, reason)
    parent, base = os.path.split(candidate)
    if base in ("", ".", "..") or "/" in base:
        refuse(reason, path=candidate, detail="basename")
    if parent != root and not parent.startswith(root + "/"):
        refuse(reason, path=candidate, detail="outside root")
    current = root
    rel = parent[len(root):].strip("/")
    for component in (rel.split("/") if rel else []):
        current = current + "/" + component
        try:
            info = os.lstat(current)
        except FileNotFoundError:
            refuse(reason, path=candidate, detail="non-existent parent")
        except (OSError, ValueError) as exc:  # Z3: an unreadable parent is named as such, never as non-existent
            refuse(reason, path=candidate, detail="parent " + os_error_text("lstat", exc))
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            refuse(reason, path=candidate, detail="parent component not a real directory")
    real_parent = realpath_or_refuse(parent, reason, candidate)
    if real_parent != root and not real_parent.startswith(root + os.sep):
        refuse(reason, path=candidate, detail="realpath outside root")
    try:
        info = os.lstat(candidate)
    except FileNotFoundError:
        info = None
    except (OSError, ValueError) as exc:  # Y1: ENAMETOOLONG, ELOOP, ENOTDIR, EACCES, an unencodable name, ...
        refuse(reason, path=candidate, detail=str(exc))
    if info is None:
        if must_exist:
            refuse(reason, path=candidate, detail="missing")
        return candidate
    if stat.S_ISLNK(info.st_mode):
        refuse(reason, path=candidate, detail="symlink")
    if kind == "file" and not stat.S_ISREG(info.st_mode):
        refuse(reason, path=candidate, detail="not a regular file")
    if kind == "dir" and not stat.S_ISDIR(info.st_mode):
        refuse(reason, path=candidate, detail="not a directory")
    return candidate


def read_regular(path, reason):
    """Read a regular file without following a final symlink. Y1: every open or read failure is the caller's refusal."""
    try:
        fd = os.open(path, os.O_RDONLY | OPEN_NOFOLLOW | OPEN_CLOEXEC)
    except (OSError, ValueError) as exc:
        refuse(reason, path=path, detail=str(exc))
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            refuse(reason, path=path, detail="not a regular file")
        chunks = []
        while True:
            try:
                chunk = os.read(fd, 1024 * 1024)
            except OSError as exc:
                refuse(reason, path=path, detail=str(exc))
            if not chunk:
                break
            chunks.append(chunk)
        return b"".join(chunks)
    finally:
        os.close(fd)


def write_new_file(path, root, data, mode=0o600, on_created=None):
    """Create a new file (O_EXCL|O_NOFOLLOW) inside a contained, verified parent. `on_created` records the file the
    moment it exists, so a write that fails part-way leaves a file the receipt names (D5)."""
    require_contained(path, root, "file", "WRITE_TARGET_NOT_CONTAINED")
    if entry_exists(path, "WRITE_TARGET_NOT_CONTAINED"):  # Z3: an unreadable entry is never "absent"
        refuse("WRITE_TARGET_EXISTS", EXIT_WRITE_FAILED, path=path)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | OPEN_NOFOLLOW | OPEN_CLOEXEC, mode)
    if on_created is not None:  # D5: recorded in the statement after os.open returns
        on_created(path)
    try:
        view = memoryview(data)
        written = 0
        while written < len(view):
            written += os.write(fd, view[written:])
        os.fsync(fd)
    finally:
        os.close(fd)
    return path


def require_prospective(path, root, kind, reason, creatable=()):
    """V1, phase P, read-only: the containment a later creation, write or rename of `path` will need, checked before
    anything is written. The pinned root must be a real directory (CT1/CT2) and the path CT3-clean below it. Walking down
    from the root, every EXISTING component is checked as an existing parent is (CT4/CT5: lstat, a real directory, never
    a symlink, realpath inside the root); the first missing component and everything below it must be plain names the
    installer itself creates (the installer-owned directories in `creatable`, or the final name). An existing final
    component must be of `kind` ("file", "dir") and is refused for "new". This adds a phase-P refusal; the strict
    checks after each creation and the CT6 rechecks before each write still run."""
    if not path_syntax_ok(path):
        refuse(reason, path=path, detail="path syntax")
    require_real_dir(root, reason)
    if not path.startswith(root + "/"):
        refuse(reason, path=path, detail="outside root")
    names = path[len(root) + 1:].split("/")
    deepest = root
    current = root
    for index, name in enumerate(names):
        current = current + "/" + name
        final = index == len(names) - 1
        try:
            info = os.lstat(current)
        except FileNotFoundError:
            if any(missing not in creatable for missing in names[index:-1]):
                refuse(reason, path=path, detail="a missing component is not an installer-owned directory")
            break
        except (OSError, ValueError) as exc:  # Y1
            refuse(reason, path=path, detail=str(exc))
        if stat.S_ISLNK(info.st_mode):
            refuse(reason, path=path, detail="symlink")
        if not final:
            if not stat.S_ISDIR(info.st_mode):
                refuse(reason, path=path, detail="parent component not a real directory")
            deepest = current
        elif kind == "new":
            refuse(reason, path=path, detail="exists")
        elif kind == "file" and not stat.S_ISREG(info.st_mode):
            refuse(reason, path=path, detail="not a regular file")
        elif kind == "dir" and not stat.S_ISDIR(info.st_mode):
            refuse(reason, path=path, detail="not a directory")
    real = realpath_or_refuse(deepest, reason, path)
    if real != root and not real.startswith(root + "/"):
        refuse(reason, path=path, detail="realpath outside root")
    return path


def check_created_dir(path, root, reason, receipt, seams=None):
    """V1: the strict CT2-CT5 checks, run immediately after the installer created the directory `path` and before
    anything is written inside it. A failure there follows the post-write rule of V2 (FAILED, exit 7); a passed check is
    logged in post_creation_checks. The test point post-creation-check fails the first such check of a run."""
    global _post_creation_fault_used, _boundary_fault_used
    if "transaction-boundary" in (seams or {}).get("faults", ()) and not _boundary_fault_used:
        # X1 (test only): the REFUSED-versus-FAILED classification as it stands right after this creation and its
        # record, before any recovery, archive or other write; then the run halts here like a failed check
        _boundary_fault_used = True
        written = transaction_written(receipt)
        receipt["test_transaction_boundary"] = {"path": path, "transaction_written": written,
                                               "outcome_if_refused_here": "FAILED" if written else "REFUSED"}
        refuse(reason, path=path, detail="TEST_FAULT transaction-boundary")
    if "post-creation-check" in (seams or {}).get("faults", ()) and not _post_creation_fault_used:
        _post_creation_fault_used = True
        refuse(reason, path=path, detail="TEST_FAULT post-creation-check")
    require_contained(path, root, "dir", reason, must_exist=True)
    require_real_dir(path, reason)
    receipt.setdefault("post_creation_checks", []).append(path)


def recorder(receipt, key):
    """An on_created callback that names the created file in the receipt (D5)."""
    def record(path):
        receipt[key] = path
    return record


def rename_contained(src, dst, root, kind):
    """Rename an installer-owned entry to a sibling built from pinned roots; never overwrite."""
    require_contained(src, root, kind, "RENAME_SOURCE_NOT_CONTAINED", must_exist=True)
    require_contained(dst, root, kind, "RENAME_TARGET_NOT_CONTAINED")
    if entry_exists(dst, "RENAME_TARGET_NOT_CONTAINED"):  # Z3: an unreadable entry is never "absent"
        refuse("RENAME_TARGET_EXISTS", EXIT_WRITE_FAILED, path=dst)
    try:
        os.rename(src, dst)
    except OSError as exc:  # Y1: a failed rename is reported as such (exit 7), never as an unexpected error
        refuse("RENAME_FAILED", EXIT_WRITE_FAILED, path=dst, detail=str(exc))
    return dst


def os_error_text(operation, exc):
    """Z3: the failed operation and the errno name, e.g. "lstat: EACCES (Permission denied)"."""
    if isinstance(exc, OSError) and exc.errno is not None:
        return f"{operation}: {errno.errorcode.get(exc.errno, str(exc.errno))} ({exc.strerror})"
    return f"{operation}: {type(exc).__name__}: {exc}"


def path_state(path):
    """Z3: the one existence check behind every decision that enters an outcome, a receipt, a state field or a console
    line: ("present", None), ("absent", None) for FileNotFoundError only, or ("unreadable", text) for every other OSError
    or ValueError (EACCES, ELOOP, ENAMETOOLONG, EIO, an unencodable name). os.path.exists, lexists, isfile, isdir and
    islink are never used for such a decision: they return False on those errors and so hide them."""
    try:
        os.lstat(path)
        return "present", None
    except FileNotFoundError:
        return "absent", None
    except (OSError, ValueError) as exc:
        return "unreadable", os_error_text("lstat", exc)


def entry_exists(path, reason):
    """Y1/Z3: whether `path` exists (lstat). Only FileNotFoundError means absent; an unreadable entry is the caller's
    refusal, never read as "absent"."""
    status, error = path_state(path)
    if status == "unreadable":
        refuse(reason, path=path, detail=error)
    return status == "present"


def write_atomic(target, root, data, mode, seams=None, progress=None, on_replaced=None):
    """Temporary file in the verified parent (O_EXCL|O_NOFOLLOW), full write, fsync, hash check,
    containment re-check (CT6), os.replace. For the settings target, `progress` (the receipt) gets the
    temporary file name as soon as it exists, every later refusal exits 7 and names it (F6), and
    `on_replaced` records the completed write immediately after os.replace returns (invariant W)."""
    seams = seams or {}
    require_contained(target, root, "file", "WRITE_TARGET_NOT_CONTAINED")
    directory = os.path.dirname(target)
    temp_path = f"{directory}/.{os.path.basename(target)}.tmp-{os.getpid()}-{secrets.token_hex(6)}"
    try:
        fd = os.open(temp_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | OPEN_NOFOLLOW | OPEN_CLOEXEC, 0o600)
    except OSError as exc:
        refuse("TEMP_CREATE_FAILED", EXIT_WRITE_FAILED, detail=str(exc))
    if progress is not None:
        progress["temp_path"] = temp_path
    try:
        try:
            view = memoryview(data)
            written = 0
            while written < len(view):
                chunk = view[written:]
                if seams.get("short_write"):  # test seam: behave like a device that took half and stopped
                    chunk = view[written:written + max(1, len(view) // 2)]
                count = os.write(fd, chunk)
                if count <= 0:
                    refuse("SHORT_WRITE", EXIT_WRITE_FAILED, written=written, expected=len(data))
                written += count
                if seams.get("short_write"):
                    break
            os.fsync(fd)
        except OSError as exc:
            refuse("TEMP_WRITE_FAILED", EXIT_WRITE_FAILED, detail=str(exc))
        finally:
            os.close(fd)
        try:
            inject_fault(seams, "temp-readback")
            readback = read_regular(temp_path, "TEMP_READBACK_FAILED")
        except OSError as exc:
            refuse("TEMP_READBACK_FAILED", EXIT_WRITE_FAILED, detail=str(exc))
        if sha256_bytes(readback) != sha256_bytes(data):
            refuse("TEMP_HASH_MISMATCH", EXIT_WRITE_FAILED)
        try:
            os.chmod(temp_path, mode)
        except OSError as exc:
            refuse("TEMP_CHMOD_FAILED", EXIT_WRITE_FAILED, detail=str(exc))
        if seams.get("crash_before_replace"):
            refuse("TEST_CRASH_BEFORE_REPLACE", EXIT_WRITE_FAILED)
        inject_fault(seams, "interrupt-before-replace")
        require_contained(target, root, "file", "WRITE_TARGET_NOT_CONTAINED")  # CT6: immediately before use
        require_contained(temp_path, root, "file", "TEMP_NOT_CONTAINED", must_exist=True)
    except Refusal as refusal:
        # the temporary file exists: writing had begun, so this is never "refused input" (F6)
        refusal.code = EXIT_WRITE_FAILED
        refusal.extra.setdefault("temp_path", temp_path)
        raise
    try:
        os.replace(temp_path, target)
    except OSError as exc:
        refuse("REPLACE_FAILED", EXIT_WRITE_FAILED, temp_path=temp_path, detail=str(exc))
    if on_replaced is not None:  # D5: the record is the next statement after the replace returns
        on_replaced(temp_path)
    inject_fault(seams, "interrupt-after-replace")
    try:
        inject_fault(seams, "dir-fsync")
        dir_fd = os.open(directory, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except OSError as exc:  # the replace already happened; recorded, never swallowed (F5)
        if progress is not None:
            progress["dir_fsync_error"] = f"{type(exc).__name__}: {exc}"
    return temp_path


# --- manifest and preconditions (11.0, 11.1, 11.7) ------------------------------------------

def validate_test_root(path):
    if path is None:
        return None
    if not path_syntax_ok(path):
        refuse("TEST_ROOT_INVALID", path=path)
    try:
        info = os.lstat(path)
    except OSError:
        refuse("TEST_ROOT_INVALID", path=path, detail="missing")
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode) or os.path.realpath(path) != path:
        refuse("TEST_ROOT_INVALID", path=path, detail="not a real directory")
    if os.path.dirname(path) != os.path.realpath(tempfile.gettempdir()):
        refuse("TEST_ROOT_INVALID", path=path, detail="not directly under the system temporary directory")
    return path


def load_manifest(layout, mode, manifest_path, expected_sha, receipt):
    if not is_hex64(expected_sha):
        refuse("MANIFEST_HASH_MISSING")
    if manifest_path != layout.manifest_for(mode):
        refuse("MANIFEST_PATH_NOT_PINNED", path=manifest_path, pinned=layout.manifest_for(mode))
    require_real_dir(layout.evidence_parent, "EVIDENCE_PARENT_NOT_REAL_DIRECTORY")
    require_contained(layout.evidence_root, layout.evidence_parent, "dir", "EVIDENCE_ROOT_NOT_REAL_DIRECTORY",
                      must_exist=True)
    require_real_dir(layout.evidence_root, "EVIDENCE_ROOT_NOT_REAL_DIRECTORY")
    try:
        info = os.lstat(manifest_path)
    except OSError as exc:
        refuse("MANIFEST_MISSING", detail=str(exc))
    if not stat.S_ISREG(info.st_mode):
        refuse("MANIFEST_NOT_REGULAR")
    require_contained(manifest_path, layout.evidence_root, "file", "MANIFEST_NOT_CONTAINED", must_exist=True)
    data = read_regular(manifest_path, "MANIFEST_MISSING")
    actual = sha256_bytes(data)
    if actual != expected_sha:
        refuse("MANIFEST_HASH_MISMATCH", actual_manifest_sha256=actual)
    receipt["manifest_sha256_verified"] = actual
    try:
        manifest = loads_strict(data)
    except (ValueError, UnicodeDecodeError) as exc:
        refuse("MANIFEST_MALFORMED", detail=str(exc))
    if not isinstance(manifest, dict):
        refuse("MANIFEST_NOT_OBJECT")
    for key in manifest:
        if key not in MANIFEST_KEYS_ALLOWED:
            refuse("MANIFEST_UNKNOWN_KEY", key=key)
    for key in MANIFEST_KEYS_REQUIRED:
        if key not in manifest:
            refuse("MANIFEST_KEY_MISSING", key=key)
    if manifest["source"] != layout.source:
        refuse("MANIFEST_SOURCE_NOT_PINNED", value=manifest["source"])
    if not is_hex64(manifest["source_sha256"]):
        refuse("MANIFEST_SOURCE_SHA_MALFORMED")
    if manifest["runtime_root"] != layout.runtime_parent:
        refuse("MANIFEST_RUNTIME_ROOT_NOT_PINNED", value=manifest["runtime_root"])
    if manifest["settings_target"] != layout.settings:
        refuse("MANIFEST_SETTINGS_TARGET_NOT_PINNED", value=manifest["settings_target"])
    if manifest["evidence_dir"] != layout.evidence_dir:
        refuse("MANIFEST_EVIDENCE_DIR_NOT_PINNED", value=manifest["evidence_dir"])
    if "registration" in manifest and manifest["registration"] != REGISTRATION_TEMPLATE:
        refuse("MANIFEST_REGISTRATION_NOT_TEMPLATE")
    require_contained(layout.source, layout.source_dir, "file", "SOURCE_NOT_CONTAINED", must_exist=True)
    source_sha = sha256_bytes(read_regular(layout.source, "SOURCE_NOT_CONTAINED"))
    if source_sha != manifest["source_sha256"]:
        refuse("SOURCE_HASH_MISMATCH", actual_source_sha256=source_sha)
    require_real_dir(layout.hooks_dir, "HOOKS_DIR_NOT_REAL_DIRECTORY")
    if entry_exists(layout.runtime_parent, "RUNTIME_PARENT_NOT_REAL_DIRECTORY"):  # Z3
        require_real_dir(layout.runtime_parent, "RUNTIME_PARENT_NOT_REAL_DIRECTORY")
    require_real_dir(layout.settings_dir, "SETTINGS_DIR_NOT_REAL_DIRECTORY")
    require_contained(layout.evidence_dir, layout.evidence_root, "dir", "EVIDENCE_DIR_NOT_REAL_DIRECTORY",
                      must_exist=True)
    require_real_dir(layout.evidence_dir, "EVIDENCE_DIR_NOT_REAL_DIRECTORY")
    if not os.access(layout.evidence_dir, os.W_OK):
        refuse("EVIDENCE_DIR_NOT_WRITABLE")
    receipt["evidence_verified"] = True
    return manifest


# --- settings ----------------------------------------------------------------------------

def read_settings(layout):
    """Returns (bytes, object, stat) or (None, None, None) when the file is absent."""
    require_real_dir(layout.settings_dir, "SETTINGS_DIR_NOT_REAL_DIRECTORY")
    try:
        info = os.lstat(layout.settings)
    except FileNotFoundError:
        return None, None, None
    except (OSError, ValueError) as exc:  # Z3: unreadable is never "absent"
        refuse("SETTINGS_NOT_REGULAR", detail=os_error_text("lstat", exc))
    if stat.S_ISLNK(info.st_mode):
        refuse("SETTINGS_SYMLINK")
    if stat.S_ISDIR(info.st_mode):
        refuse("SETTINGS_DIRECTORY")
    if not stat.S_ISREG(info.st_mode):
        refuse("SETTINGS_NOT_REGULAR")
    data = read_regular(layout.settings, "SETTINGS_NOT_REGULAR")
    try:
        obj = loads_strict(data)
    except (ValueError, UnicodeDecodeError) as exc:
        refuse("SETTINGS_MALFORMED", detail=str(exc))
    if not isinstance(obj, dict):
        refuse("SETTINGS_NOT_OBJECT")
    return data, obj, info


def permission_request_entries(obj):
    """V1: the PermissionRequest entries, after validating every level of the hooks structure the installer reads or
    changes. A present key whose value is null or of another unexpected type is malformed (a refusal), never "absent"."""
    if "hooks" not in obj:
        return []
    hooks = obj["hooks"]
    if not isinstance(hooks, dict):
        refuse("SETTINGS_HOOKS_NOT_OBJECT")
    if "PermissionRequest" not in hooks:
        return []
    entries = hooks["PermissionRequest"]
    if not isinstance(entries, list):
        refuse("SETTINGS_PERMISSION_REQUEST_NOT_LIST")
    for entry in entries:
        if not isinstance(entry, dict):
            refuse("SETTINGS_PERMISSION_REQUEST_ENTRY_INVALID", detail="entry not an object")
        if "hooks" in entry:
            inner = entry["hooks"]
            if not isinstance(inner, list) or not all(isinstance(item, dict) for item in inner):
                refuse("SETTINGS_PERMISSION_REQUEST_ENTRY_INVALID", detail="entry hooks not a list of objects")
            if any("command" in item and not isinstance(item["command"], str) for item in inner):
                refuse("SETTINGS_PERMISSION_REQUEST_ENTRY_INVALID", detail="command not a string")
    return entries


def build_settings_document(obj):
    """V1, phase P: the new settings bytes; a document that cannot be encoded (a lone surrogate, or under Y5 a
    non-finite number) is refused before anything is written."""
    try:
        return dumps(obj).encode("utf-8")
    except ValueError as exc:  # UnicodeEncodeError is a ValueError
        refuse("SETTINGS_NOT_ENCODABLE", detail=str(exc))


def registration_for(runtime_path, sha):
    # SHA first, then RUNTIME: a hex digest can never contain "RUNTIME", a path might contain "SHA"
    command = TEMPLATE_COMMAND.replace("SHA", sha).replace("RUNTIME", runtime_path)
    if command.count(runtime_path) != 1 or command.count(sha) != 1:
        refuse("REGISTRATION_SUBSTITUTION_AMBIGUOUS", command=command)
    entry = {"matcher": "Bash", "hooks": [{"type": "command", "command": command, "timeout": 10}]}
    return entry, command


def entry_command(entry):
    hooks = entry.get("hooks") if isinstance(entry, dict) else None
    if isinstance(hooks, list) and hooks and isinstance(hooks[0], dict):
        return hooks[0].get("command")
    return None


def current_settings_sha(layout):
    """The settings target's hash; None when it is absent or (FACT, documented in r27-existence-checks.md) not a regular
    file, which every caller compares with a recorded hash. Z3: an unreadable target is a refusal, never None."""
    try:
        info = os.lstat(layout.settings)
    except FileNotFoundError:
        return None
    except (OSError, ValueError) as exc:
        refuse("SETTINGS_NOT_REGULAR", detail=os_error_text("lstat", exc))
    if not stat.S_ISREG(info.st_mode):
        return None
    return sha256_bytes(read_regular(layout.settings, "SETTINGS_NOT_REGULAR"))


def recheck_preimage(layout, preimage_bytes):
    current = current_settings_sha(layout)
    expected = None if preimage_bytes is None else sha256_bytes(preimage_bytes)
    if current != expected:
        refuse("DRIFT_BEFORE_WRITE", EXIT_DRIFT_BEFORE_WRITE, settings_sha256=current,
               expected_settings_sha256=expected)


# --- runtime directory --------------------------------------------------------------------

def runtime_entries(layout, hash12):
    """(runtime dir exists, list of aborted archives) for one hash; RUNTIME_PARENT verified when present."""
    if not entry_exists(layout.runtime_parent, "RUNTIME_PARENT_UNREADABLE"):  # Z3: unreadable is never "absent"
        return False, []
    require_real_dir(layout.runtime_parent, "RUNTIME_PARENT_NOT_REAL_DIRECTORY")
    try:
        names = os.listdir(layout.runtime_parent)
    except OSError as exc:  # a read failure, never reported as a write failure (F7)
        refuse("RUNTIME_PARENT_UNREADABLE", detail=str(exc))
    aborted = sorted(n for n in names if n.startswith(hash12 + ".aborted-"))
    return hash12 in names, aborted


def require_intact_runtime(layout, runtime_dir, runtime_path, sha):
    """IDEMPOTENT only when the registered runtime file exists as a regular, non-symlink 0444 file with
    the registered SHA-256; otherwise RUNTIME_DRIFT (exit 4), nothing written."""
    try:
        require_real_dir(layout.runtime_parent, "RUNTIME_DRIFT")
        require_contained(runtime_dir, layout.runtime_parent, "dir", "RUNTIME_DRIFT", must_exist=True)
        require_real_dir(runtime_dir, "RUNTIME_DRIFT")
        require_contained(runtime_path, runtime_dir, "file", "RUNTIME_DRIFT", must_exist=True)
        info = os.lstat(runtime_path)
        intact = (stat.S_IMODE(info.st_mode) == 0o444
                  and sha256_bytes(read_regular(runtime_path, "RUNTIME_DRIFT")) == sha)
    except (OSError, Refusal):
        intact = False
    if not intact:
        refuse("RUNTIME_DRIFT", EXIT_STATE_DRIFT, runtime_path=runtime_path)


def create_runtime_copy(layout, sha, receipt, seams=None):
    """Create RUNTIME_PARENT (plain mkdir, only below a real HOOKS_DIR) and RUNTIME_PARENT/<HASH12>,
    copy the source there (0444) and verify the copy. Ownership of <HASH12> is recorded in the receipt
    the moment its mkdir returns, so every later failure archives it (invariant A)."""
    if not entry_exists(layout.runtime_parent, "RUNTIME_PARENT_NOT_REAL_DIRECTORY"):  # Z3
        receipt["step"] = "runtime parent"
        require_real_dir(layout.hooks_dir, "HOOKS_DIR_NOT_REAL_DIRECTORY")
        before_first_write(seams)
        os.mkdir(layout.runtime_parent)
        receipt["runtime_parent_created"] = layout.runtime_parent  # D5: recorded in the statement after the mkdir returns
        check_created_dir(layout.runtime_parent, layout.hooks_dir, "RUNTIME_PARENT_NOT_REAL_DIRECTORY", receipt, seams)
    receipt["step"] = "runtime copy"
    require_real_dir(layout.runtime_parent, "RUNTIME_PARENT_NOT_REAL_DIRECTORY")
    runtime_dir = layout.runtime_parent + "/" + sha[:12]
    require_contained(runtime_dir, layout.runtime_parent, "dir", "RUNTIME_DIR_NOT_CONTAINED")
    if entry_exists(runtime_dir, "RUNTIME_DIR_NOT_CONTAINED"):  # Z3: an unreadable entry is never "absent"
        refuse("RUNTIME_DIR_EXISTS", EXIT_RUNTIME_EXISTS, runtime_dir=runtime_dir)
    before_first_write(seams)
    os.mkdir(runtime_dir)
    receipt["runtime_dir_owned"] = runtime_dir  # D5: recorded in the statement after the mkdir returns (A)
    check_created_dir(runtime_dir, layout.runtime_parent, "RUNTIME_DIR_NOT_CONTAINED", receipt, seams)
    data = read_regular(layout.source, "SOURCE_NOT_CONTAINED")
    if sha256_bytes(data) != sha:
        refuse("SOURCE_HASH_MISMATCH", EXIT_WRITE_FAILED, actual_source_sha256=sha256_bytes(data))
    try:
        inject_fault(seams, "runtime-copy-write")
        runtime_path = write_new_file(runtime_dir + "/" + GUARD_FILENAME, runtime_dir, data, 0o600)
        os.chmod(runtime_path, 0o444)
    except OSError as exc:
        refuse("RUNTIME_COPY_FAILED", EXIT_WRITE_FAILED, detail=str(exc))
    copied = sha256_bytes(read_regular(runtime_path, "RUNTIME_COPY_UNREADABLE"))
    if copied != sha:
        refuse("RUNTIME_COPY_HASH_MISMATCH", EXIT_WRITE_FAILED, runtime_dir=runtime_dir, runtime_sha256=copied)
    receipt["runtime_sha256"] = sha
    receipt["runtime_copy_created"] = True  # Y4: only now is the runtime copy written in full, 0444 and verified
    return runtime_dir


def archive_failed_runtime(layout, receipt, seams=None):
    """Invariant A: a failure before the settings replacement returned renames the runtime dir this run
    created to <HASH12>.aborted-<UTC>-<pid> exactly once (never deleted, never overwritten). After the
    replacement (settings_written) the runtime dir is never touched (F3)."""
    runtime_dir = receipt.get("runtime_dir_owned")
    if receipt.get("settings_written") or "runtime_renamed_to" in receipt:
        return
    if not runtime_dir:  # U4a: this run created no <HASH12>, so there is nothing to archive; the receipt says so
        receipt["runtime_renamed_to"] = None
        return
    receipt["failed_step"] = receipt.get("step")
    archive = f"{runtime_dir}.aborted-{utc_stamp()}-{os.getpid()}"
    try:
        rename_contained(runtime_dir, archive, layout.runtime_parent, "dir")
        signal_fault(seams, "signal-after-archive-rename")  # D7 test hook only; a deferred signal raises nothing
        receipt["runtime_renamed_to"] = archive  # D5: recorded in the statement after the rename returns
    except Exception as exc:  # report, never hide: the orphan is then left for the owner (INCOMPLETE_INSTALL)
        receipt["runtime_renamed_to"] = None
        receipt["runtime_archive_error"] = f"{type(exc).__name__}: {exc}"


def record_commit(receipt, flag, data):
    """Invariant W: the settings write is recorded the moment its os.replace has returned."""
    def on_replaced(temp_path):
        receipt.update({flag: True, "temp_path": temp_path, "intended_postimage_sha256": sha256_bytes(data),
                        "postimage_sha256": None, "step": STEP_AFTER_REPLACE})
    return on_replaced


def read_postimage(layout, receipt, seams):
    """The read-back after a recorded write; a failure is exit 7, never a refusal of input (W)."""
    try:
        inject_fault(seams, "postimage-read")
        post = current_settings_sha(layout)
    except (OSError, Refusal) as exc:
        receipt["postimage_read_error"] = getattr(exc, "reason", None) or f"{type(exc).__name__}: {exc}"
        refuse("POSTIMAGE_UNREADABLE", EXIT_WRITE_FAILED)
    receipt["postimage_sha256"] = post
    return post


def fail_after_write(seams):
    if seams.get("fail_after_write"):
        raise OSError("TEST_FAIL_AFTER_WRITE")


# --- state and receipts ---------------------------------------------------------------------

def load_state(layout, seams=None):
    """The latest state-changing receipt; every path in it must equal the pinned derivation. Only a
    missing file means "no state" (STATE_MISSING); any other read error is STATE_UNREADABLE (F7). A
    state that records a committed write (settings_written / settings_restored) with a null postimage
    is returned with postimage_unknown set (W): callers never act on an assumed hash."""
    path = layout.state_path
    try:
        inject_fault(seams, "state-read")
        info = os.lstat(path)
    except FileNotFoundError as exc:
        refuse("STATE_MISSING", detail=str(exc))
    except OSError as exc:
        refuse("STATE_UNREADABLE", detail=str(exc))
    if not stat.S_ISREG(info.st_mode):
        refuse("STATE_NOT_REGULAR")
    data = read_regular(path, "STATE_UNREADABLE")
    try:
        state = loads_strict(data)
    except (ValueError, UnicodeDecodeError) as exc:
        refuse("STATE_MALFORMED", detail=str(exc))
    if not isinstance(state, dict):
        refuse("STATE_MALFORMED")
    # Y2: every value read below has the exact type the installer writes (a bool is never an int); a value that fails
    # is an untrustworthy state, refused here in phase P (verify: 11.3 step 1 for STATE_MALFORMED, else steps 3 and 4)
    for key in ("settings_written", "settings_restored"):
        if key in state and not isinstance(state[key], bool):
            refuse("STATE_MALFORMED", key=key)
    if not isinstance(state.get("result"), str):
        refuse("STATE_MALFORMED", key="result")
    if state.get("reason") is not None and not isinstance(state["reason"], str):
        refuse("STATE_MALFORMED", key="reason")
    if "postimage_unknown" in state:  # computed here only, never written into a state by the installer
        refuse("STATE_MALFORMED", key="postimage_unknown")
    committed = state.get("settings_written") is True or state.get("settings_restored") is True
    installed = state.get("result") in ("INSTALLED", "PROMOTED") or (state.get("result") == "FAILED" and committed)
    if not installed:
        refuse("STATE_NOT_INSTALLED", state_result=state.get("result"))
    # Y1: a path read from the state is checked for its syntax (path_syntax_ok) before it is compared or used
    if not path_syntax_ok(state.get("settings_target")) or state.get("settings_target") != layout.settings:
        refuse("STATE_SETTINGS_TARGET_MISMATCH")
    if not is_hex64(state.get("runtime_sha256")):
        refuse("STATE_HASH_MALFORMED", key="runtime_sha256")
    if state.get("postimage_sha256") is None and committed and state.get("result") == "FAILED":
        state["postimage_unknown"] = True
    elif not is_hex64(state.get("postimage_sha256")):
        refuse("STATE_HASH_MALFORMED", key="postimage_sha256")
    intended = state.get("intended_postimage_sha256")
    if intended is not None and not is_hex64(intended):
        refuse("STATE_HASH_MALFORMED", key="intended_postimage_sha256")
    if committed and is_hex64(intended) and state.get("postimage_sha256") != intended:
        # read back but not the bytes this run wrote (POSTIMAGE_MISMATCH, a concurrent writer): unverified (W, 11.3)
        state["postimage_unknown"] = True
    expected_dir = layout.runtime_parent + "/" + state["runtime_sha256"][:12]
    for key, expected in (("runtime_dir", expected_dir), ("runtime_path", expected_dir + "/" + GUARD_FILENAME)):
        if not path_syntax_ok(state.get(key)) or state.get(key) != expected:
            refuse("STATE_RUNTIME_NOT_PINNED")
    if not isinstance(state.get("registration_command"), str) or state["registration_command"] == "":
        refuse("STATE_REGISTRATION_MISSING")
    preimage = state.get("preimage")
    if not isinstance(preimage, dict) or preimage.get("status") not in ("ABSENT", "PRESENT"):
        refuse("STATE_PREIMAGE_MALFORMED")
    if preimage["status"] == "PRESENT":
        backup = preimage.get("backup_path")
        if (not path_syntax_ok(backup) or os.path.dirname(backup) != layout.evidence_dir
                or not os.path.basename(backup).startswith(PREIMAGE_PREFIX) or not is_hex64(preimage.get("sha256"))):
            refuse("STATE_PREIMAGE_NOT_PINNED")
        mode, size, mtime = preimage.get("mode"), preimage.get("size"), preimage.get("mtime")
        if (not isinstance(mode, str) or not MODE_TEXT.fullmatch(mode) or oct(int(mode, 8)) != mode
                or int(mode, 8) != stat.S_IMODE(int(mode, 8))):
            refuse("STATE_PREIMAGE_MALFORMED", key="preimage.mode")
        if isinstance(size, bool) or not isinstance(size, int) or size < 0:
            refuse("STATE_PREIMAGE_MALFORMED", key="preimage.size")
        if (isinstance(mtime, bool) or not isinstance(mtime, (int, float))
                or (isinstance(mtime, float) and not math.isfinite(mtime))):  # Z1: isfinite only on a float
            refuse("STATE_PREIMAGE_MALFORMED", key="preimage.mtime")
        require_contained(backup, layout.evidence_dir, "file", "STATE_PREIMAGE_NOT_PINNED", must_exist=True)
    return state


def save_receipt(layout, receipt, update_state, seams=None):
    """Outcome S: after a completed write the state file is saved FIRST; a receipt with a success result
    is persisted only after that save succeeded. A failed state save turns the result into
    <RESULT>_STATE_NOT_SAVED (exit 7, state_saved false, original_result / original_reason kept); a
    receipt that cannot be written says receipt_saved false (exit 7, except that a refusal keeps its
    own code). Z4: the state file holds no result of saving itself (STATE_SAVE_KEYS) and no receipt-only key, and the
    receipt file holds no result of saving itself (RECEIPT_ONLY_KEYS); the printed receipt carries them. Never raises."""
    if not receipt.get("evidence_verified"):  # a refusal before the evidence directory is verified is printed only
        receipt.update(receipt_saved=False, receipt_save_error="EVIDENCE_DIR_NOT_VERIFIED", receipt_save_temp_path=None)
        return
    if update_state:
        progress = {}
        try:
            inject_fault(seams, "state-save")
            state_doc = {key: value for key, value in receipt.items() if key not in STATE_FILE_EXCLUDED_KEYS}
            state_doc.update(interrupt_snapshot())  # D3: the state's one snapshot, immediately before it is serialized
            before_first_write(seams)
            # test only (U17): state-dir-fsync fails the fsync of the state's directory, and nothing else of this write
            state_seams = {"faults": ("dir-fsync",) if "state-dir-fsync" in (seams or {}).get("faults", ()) else ()}
            write_atomic(layout.state_path, layout.evidence_dir, dumps_record(state_doc).encode("ascii"),
                         0o600, seams=state_seams, progress=progress)
            receipt["state_saved"] = True
            if "dir_fsync_error" in progress:  # F5 for the state write too
                receipt["state_dir_fsync_error"] = progress["dir_fsync_error"]
        except Exception as exc:  # outcome S: a failed state save is an outcome, never a crash
            temp = (getattr(exc, "extra", None) or {}).get("temp_path") or progress.get("temp_path")
            if temp:
                receipt["state_save_temp_path"] = temp  # F6 for the state write: the leftover temp file is named
            receipt.update(state_saved=False, state_save_error=getattr(exc, "reason", None) or f"{type(exc).__name__}: {exc}",
                           original_result=receipt.get("result"), original_reason=receipt.get("reason"),
                           result=f"{receipt.get('result')}_STATE_NOT_SAVED", exit_code=EXIT_WRITE_FAILED)
    receipt.update(receipt_save_error=None, receipt_save_temp_path=None)  # Y3: the receipt-only keys, not yet applicable
    try:
        receipts = layout.evidence_dir + "/" + RECEIPTS_DIR
        if not entry_exists(receipts, "RECEIPTS_DIR_NOT_CONTAINED"):  # Z3: an unreadable entry is never "absent"
            require_contained(receipts, layout.evidence_dir, "dir", "RECEIPTS_DIR_NOT_CONTAINED")
            before_first_write(seams)
            os.mkdir(receipts)
            check_created_dir(receipts, layout.evidence_dir, "RECEIPTS_DIR_NOT_CONTAINED", receipt, seams)
        require_contained(receipts, layout.evidence_dir, "dir", "RECEIPTS_DIR_NOT_CONTAINED", must_exist=True)
        require_real_dir(receipts, "RECEIPTS_DIR_NOT_CONTAINED")
        receipt.update(interrupt_snapshot())  # D3: the final receipt's one snapshot (the printed receipt uses it too)
        signal_fault(seams, "signal-after-receipt-snapshot")
        inject_fault(seams, "receipt-save")
        name = f"{receipt['timestamp_utc']}-{receipt['mode']}-{os.getpid()}.json"
        # Z4: the file holds what is known before its bytes are written; the save result goes to the printed receipt only
        data = dumps_record({key: value for key, value in receipt.items() if key not in RECEIPT_ONLY_KEYS}).encode("ascii")
        # the final name appears only after a complete, fsynced write; a leftover temporary file is named (R5a-3)
        temp = f"{receipts}/.{name}.tmp-{os.getpid()}"
        before_first_write(seams)
        write_new_file(temp, receipts, data, on_created=recorder(receipt, "receipt_save_temp_path"))
        rename_contained(temp, receipts + "/" + name, receipts, "file")
        receipt["receipt_save_temp_path"] = None  # Y3: no leftover temporary receipt (the key stays, null)
        receipt["receipt_saved"] = True
    except Exception as exc:  # never raises: the printed receipt says receipt_saved false
        receipt.update(receipt_saved=False,
                       receipt_save_error=getattr(exc, "reason", None) or f"{type(exc).__name__}: {exc}")
        if receipt.get("exit_code") in (None, EXIT_OK) or update_state:
            receipt["exit_code"] = EXIT_WRITE_FAILED


# --- modes ----------------------------------------------------------------------------

def require_prospective_records(layout):
    """V1, phase P: the paths every writing mode writes besides its own (the settings target and its temporary file in
    the settings directory, the state file, receipts/ and the receipt file in it)."""
    require_prospective(layout.settings, layout.settings_dir, "file", "WRITE_TARGET_NOT_CONTAINED")
    require_prospective(layout.state_path, layout.evidence_dir, "file", "WRITE_TARGET_NOT_CONTAINED")
    require_prospective(layout.evidence_dir + "/" + RECEIPTS_DIR, layout.evidence_dir, "dir",
                        "RECEIPTS_DIR_NOT_CONTAINED", creatable=(RECEIPTS_DIR,))


def do_install(layout, manifest, receipt, seams):
    sha = manifest["source_sha256"]
    runtime_dir = layout.runtime_parent + "/" + sha[:12]
    runtime_path = runtime_dir + "/" + GUARD_FILENAME
    entry, command = registration_for(runtime_path, sha)
    receipt.update(runtime_dir=runtime_dir, runtime_path=runtime_path, registration_command=command)

    receipt["step"] = "preimage"
    pre_bytes, pre_obj, pre_info = read_settings(layout)
    existing = permission_request_entries(pre_obj or {})  # V1: the whole hooks structure is validated here, in phase P
    if existing:
        if existing == [entry]:
            require_intact_runtime(layout, runtime_dir, runtime_path, sha)
            receipt.update(result="IDEMPOTENT", exit_code=EXIT_OK, hooks=pre_obj.get("hooks"),
                           postimage_sha256=sha256_bytes(pre_bytes))
            return EXIT_OK
        refuse("FOREIGN_HOOK", EXIT_FOREIGN_HOOK, existing_entries=existing)
    exists, aborted = runtime_entries(layout, sha[:12])
    if exists:
        refuse("RUNTIME_DIR_EXISTS", EXIT_RUNTIME_EXISTS, runtime_dir=runtime_dir)
    if aborted:  # installer-owned archives of handled failures never block a later install (13.2 (e))
        receipt["aborted_archives_present"] = aborted
    mode = 0o644 if pre_bytes is None else stat.S_IMODE(pre_info.st_mode)
    # V1: build and validate the new document before anything is written
    new_obj = loads_strict(json.dumps(pre_obj, allow_nan=False).encode("ascii")) if pre_obj is not None else {}
    new_obj.setdefault("hooks", {}).setdefault("PermissionRequest", []).append(entry)
    insert_before_serializer(layout, new_obj, seams)  # test only (U13-write); inert without --test-fault
    data = build_settings_document(new_obj)
    backup = (None if pre_bytes is None
              else f"{layout.evidence_dir}/{PREIMAGE_PREFIX}{receipt['timestamp_utc']}-{os.getpid()}")
    if backup is not None:
        require_prospective(backup, layout.evidence_dir, "new", "WRITE_TARGET_NOT_CONTAINED")
    require_prospective(runtime_path, layout.hooks_dir, "new", "RUNTIME_DIR_NOT_CONTAINED",
                        creatable=(os.path.basename(layout.runtime_parent), sha[:12]))
    require_prospective_records(layout)
    # end of phase P (the last precondition check above); phase E, then the first write (the preimage copy)
    signal_fault(seams, "signal-before-scope")
    enter_deferral_scope(seams)

    if pre_bytes is None:
        receipt["preimage"] = {"status": "ABSENT"}
    else:
        preimage = {"status": "PRESENT", "sha256": sha256_bytes(pre_bytes), "size": pre_info.st_size,
                    "mode": oct(mode), "mtime": pre_info.st_mtime, "backup_path": backup}
        before_first_write(seams)
        write_new_file(backup, layout.evidence_dir, pre_bytes, on_created=recorder(receipt, "preimage_backup_created"))
        receipt["preimage"] = preimage
    if seams.get("barrier"):
        receipt["test_barrier"] = seams["barrier"]
        wait_at_barrier(seams["barrier"])
    for name in ("crash_before_replace", "short_write", "fail_after_write"):
        if seams.get(name):
            receipt["test_" + name] = True

    receipt["hooks"] = new_obj["hooks"]
    try:  # every pre-commit failure from here on goes through the archive rule (A), which records runtime_renamed_to
        recheck_preimage(layout, pre_bytes)
        create_runtime_copy(layout, sha, receipt, seams)
        receipt["step"] = "settings replacement"
        recheck_preimage(layout, pre_bytes)
        write_atomic(layout.settings, layout.settings_dir, data, mode, seams, progress=receipt,
                     on_replaced=record_commit(receipt, "settings_written", data))
    except BaseException:
        archive_failed_runtime(layout, receipt, seams)  # pre-commit only (A); a no-op once settings_written (F3)
        raise
    if read_postimage(layout, receipt, seams) != sha256_bytes(data):
        refuse("POSTIMAGE_MISMATCH", EXIT_WRITE_FAILED, expected_postimage_sha256=sha256_bytes(data))
    fail_after_write(seams)
    receipt.update(result="INSTALLED", exit_code=EXIT_OK)
    return EXIT_OK


def registration_present(layout, sha, receipt):
    """Whether the settings target holds this hash's registration (read-only). V3: a read error or a malformed hooks
    structure is recorded in registration_error and counts as no registration, so the 11.3 precedence continues."""
    runtime_path = layout.runtime_parent + "/" + sha[:12] + "/" + GUARD_FILENAME
    _, command = registration_for(runtime_path, sha)
    try:
        _, obj, _ = read_settings(layout)
        return obj is not None and any(entry_command(e) == command for e in permission_request_entries(obj))
    except Refusal as refusal:
        receipt["registration_error"] = refusal.reason
        return None


def do_verify(layout, manifest, receipt, seams=None):
    """Precedence (11.3, binding; the first matching step decides): (1) a state file that exists but
    cannot be read: STATE_UNREADABLE; (2) a readable state that records a committed write: MATCH when
    the settings target, the runtime file and the registration match it, otherwise DRIFT (always DRIFT
    when its postimage is unknown, W); (3) no trustworthy state but a registration, runtime dir or
    .aborted- archive for HASH12: INCOMPLETE_INSTALL; (4) NOT_INSTALLED. Writes nothing."""
    sha = manifest["source_sha256"]
    untrusted = None
    try:
        state = load_state(layout, seams)
    except Refusal as refusal:
        if refusal.reason in ("STATE_UNREADABLE", "STATE_NOT_REGULAR", "STATE_MALFORMED"):  # step 1
            receipt.update(result="STATE_UNREADABLE", exit_code=EXIT_VERIFY_DRIFT, state_refusal=refusal.reason,
                           detail=refusal.extra.get("detail"))
            return EXIT_VERIFY_DRIFT
        if refusal.reason not in ("STATE_MISSING", "STATE_NOT_INSTALLED"):
            untrusted = refusal.reason  # readable, but its paths or hashes are not trustworthy: steps 3 and 4
        state = None
    if state is None:
        exists, aborted = runtime_entries(layout, sha[:12])
        registered = registration_present(layout, sha, receipt)
        if untrusted:
            receipt["state_refusal"] = untrusted
        if exists or aborted or registered:
            receipt.update(result="INCOMPLETE_INSTALL", exit_code=EXIT_VERIFY_DRIFT, runtime_dir_present=exists,
                           aborted=aborted, registration_present=registered, state_present=untrusted is not None)
            return EXIT_VERIFY_DRIFT
        receipt.update(result="NOT_INSTALLED", exit_code=EXIT_VERIFY_DRIFT)
        return EXIT_VERIFY_DRIFT
    try:  # step 2: a read error is DRIFT, never a refusal
        settings_sha = current_settings_sha(layout)
    except (OSError, ValueError, Refusal) as exc:
        settings_sha = None
        receipt["settings_read_error"] = getattr(exc, "reason", None) or f"{type(exc).__name__}: {exc}"
    runtime_path = state["runtime_path"]
    runtime_sha = None
    runtime_status, runtime_error = path_state(runtime_path)  # Z3: present, absent, or unreadable (never hidden)
    if runtime_status == "unreadable":
        receipt["runtime_read_error"] = runtime_error  # the failed operation and the errno name; runtime_sha256 null
    elif runtime_status == "present":
        try:
            require_contained(runtime_path, state["runtime_dir"], "file", "STATE_RUNTIME_NOT_PINNED", must_exist=True)
            runtime_sha = sha256_bytes(read_regular(runtime_path, "STATE_RUNTIME_NOT_PINNED"))
        except (OSError, ValueError, Refusal) as exc:
            runtime_sha = None
            receipt["runtime_read_error"] = getattr(exc, "reason", None) or f"{type(exc).__name__}: {exc}"
    registration_count = None
    try:
        _, obj, _ = read_settings(layout)
        if obj is not None:
            registration_count = sum(1 for e in permission_request_entries(obj)
                                     if entry_command(e) == state["registration_command"])
    except Refusal as refusal:
        receipt["registration_error"] = refusal.reason
    receipt.update(settings_sha256=settings_sha, expected_settings_sha256=state["postimage_sha256"],
                   intended_postimage_sha256=state.get("intended_postimage_sha256"),
                   postimage_unknown=bool(state.get("postimage_unknown")),
                   runtime_path=runtime_path, runtime_sha256=runtime_sha,
                   expected_runtime_sha256=state["runtime_sha256"],
                   registration_command=state["registration_command"], registration_count=registration_count,
                   state_result=state.get("result"), state_reason=state.get("reason"))
    if (not state.get("postimage_unknown") and settings_sha == state["postimage_sha256"]
            and runtime_sha == state["runtime_sha256"] and registration_count == 1):
        receipt.update(result="MATCH", exit_code=EXIT_OK)
        return EXIT_OK
    receipt.update(result="DRIFT", exit_code=EXIT_VERIFY_DRIFT)
    return EXIT_VERIFY_DRIFT


def refuse_unknown_postimage(state):
    """W: a state whose committed write has no verified postimage is never acted on."""
    if state.get("postimage_unknown"):
        refuse("POSTIMAGE_UNKNOWN", EXIT_STATE_DRIFT, intended_postimage_sha256=state.get("intended_postimage_sha256"),
               state_reason=state.get("reason"))


def do_rollback(layout, manifest, receipt, seams):
    state = load_state(layout, seams)
    receipt.update(runtime_dir=state["runtime_dir"], runtime_path=state["runtime_path"],
                   runtime_sha256=state["runtime_sha256"], registration_command=state["registration_command"],
                   preimage=state["preimage"], expected_settings_sha256=state["postimage_sha256"])
    refuse_unknown_postimage(state)
    current = current_settings_sha(layout)
    receipt["settings_sha256"] = current
    if current != state["postimage_sha256"]:
        refuse("SETTINGS_DRIFT", EXIT_STATE_DRIFT, settings_sha256=current,
               expected_settings_sha256=state["postimage_sha256"])
    preimage = state["preimage"]
    stamp = receipt["timestamp_utc"]
    if preimage["status"] != "ABSENT":
        data = read_regular(preimage["backup_path"], "STATE_PREIMAGE_NOT_PINNED")
        if sha256_bytes(data) != preimage["sha256"]:
            refuse("PREIMAGE_BACKUP_CORRUPT", backup_path=preimage["backup_path"])
        mode = int(preimage.get("mode", "0o644"), 8)
    else:  # V1: the sibling an ABSENT rollback renames the settings target to
        require_prospective(f"{layout.settings}.rolled-back-{stamp}", layout.settings_dir, "new",
                            "RENAME_TARGET_NOT_CONTAINED")
    if entry_exists(state["runtime_dir"], "RENAME_SOURCE_NOT_CONTAINED"):  # V1: the .disabled- rename (Y1: strict)
        require_prospective(state["runtime_dir"], layout.runtime_parent, "dir", "RENAME_SOURCE_NOT_CONTAINED")
        require_prospective(f"{state['runtime_dir']}.disabled-{stamp}", layout.runtime_parent, "new",
                            "RENAME_TARGET_NOT_CONTAINED")
    require_prospective_records(layout)
    # end of phase P (the last precondition check above); phase E, then the first write
    signal_fault(seams, "signal-before-scope")
    enter_deferral_scope(seams)
    if seams.get("barrier"):  # test only (U9b): the existing barrier, after phase P and before the first write
        receipt["test_barrier"] = seams["barrier"]
        wait_at_barrier(seams["barrier"])
    receipt["step"] = "settings restore"
    before_first_write(seams)
    if preimage["status"] == "ABSENT":
        renamed = rename_contained(layout.settings, f"{layout.settings}.rolled-back-{stamp}", layout.settings_dir, "file")
        # W, D5: recorded in the statement after the rename returns; nothing is read back after a rename
        receipt.update(settings_restored=True, settings_renamed_to=renamed, postimage_sha256=None,
                       step=STEP_AFTER_REPLACE)
    else:
        write_atomic(layout.settings, layout.settings_dir, data, mode, seams, progress=receipt,
                     on_replaced=record_commit(receipt, "settings_restored", data))
        if read_postimage(layout, receipt, seams) != preimage["sha256"]:
            refuse("POSTIMAGE_MISMATCH", EXIT_WRITE_FAILED, expected_postimage_sha256=preimage["sha256"])
    fail_after_write(seams)
    runtime_dir = state["runtime_dir"]
    if entry_exists(runtime_dir, "RENAME_SOURCE_NOT_CONTAINED"):  # Y1: an unreadable entry is never "absent"
        disabled = rename_contained(runtime_dir, f"{runtime_dir}.disabled-{stamp}", layout.runtime_parent, "dir")
        signal_fault(seams, "signal-after-disabled-rename")  # D7 test hook only; a deferred signal raises nothing
        receipt["runtime_renamed_to"] = disabled  # D5: recorded in the statement after the rename returns
    receipt.update(result="ROLLED_BACK", exit_code=EXIT_OK, previous_receipt=state)
    return EXIT_OK


def do_promote(layout, manifest, receipt, seams):
    state = load_state(layout, seams)
    refuse_unknown_postimage(state)
    sha = manifest["source_sha256"]
    if sha == state["runtime_sha256"]:
        refuse("SAME_HASH_ALREADY_INSTALLED", EXIT_RUNTIME_EXISTS, runtime_sha256=sha)
    pre_bytes, pre_obj, pre_info = read_settings(layout)
    current = None if pre_bytes is None else sha256_bytes(pre_bytes)
    receipt.update(settings_sha256=current, expected_settings_sha256=state["postimage_sha256"],
                   preimage=state["preimage"])
    if pre_bytes is None or current != state["postimage_sha256"]:
        refuse("SETTINGS_DRIFT", EXIT_STATE_DRIFT, settings_sha256=current,
               expected_settings_sha256=state["postimage_sha256"])
    entries = permission_request_entries(pre_obj)
    matches = [i for i, e in enumerate(entries) if entry_command(e) == state["registration_command"]]
    if len(matches) != 1:
        refuse("REGISTRATION_NOT_UNIQUE", EXIT_STATE_DRIFT, matches=len(matches))
    exists, aborted = runtime_entries(layout, sha[:12])
    if exists:
        refuse("RUNTIME_DIR_EXISTS", EXIT_RUNTIME_EXISTS)
    if aborted:  # installer-owned archives never block (13.2 (e))
        receipt["aborted_archives_present"] = aborted
    new_runtime_dir = layout.runtime_parent + "/" + sha[:12]
    new_runtime_path = new_runtime_dir + "/" + GUARD_FILENAME
    _, new_command = registration_for(new_runtime_path, sha)
    receipt.update(runtime_dir=new_runtime_dir, runtime_path=new_runtime_path, registration_command=new_command)
    new_obj = loads_strict(json.dumps(pre_obj, allow_nan=False).encode("ascii"))
    new_obj["hooks"]["PermissionRequest"][matches[0]]["hooks"][0]["command"] = new_command
    data = build_settings_document(new_obj)
    receipt["hooks"] = new_obj["hooks"]
    require_prospective(new_runtime_path, layout.hooks_dir, "new", "RUNTIME_DIR_NOT_CONTAINED",
                        creatable=(os.path.basename(layout.runtime_parent), sha[:12]))
    if entry_exists(state["runtime_dir"], "RENAME_SOURCE_NOT_CONTAINED"):  # V1: the .superseded- rename (Y1: strict)
        require_prospective(state["runtime_dir"], layout.runtime_parent, "dir", "RENAME_SOURCE_NOT_CONTAINED")
        require_prospective(f"{state['runtime_dir']}.superseded-{receipt['timestamp_utc']}", layout.runtime_parent,
                            "new", "RENAME_TARGET_NOT_CONTAINED")
    require_prospective_records(layout)
    recheck_preimage(layout, pre_bytes)
    # end of phase P (the last precondition check above); phase E, then the first write (RUNTIME_PARENT or <HASH12>)
    signal_fault(seams, "signal-before-scope")
    enter_deferral_scope(seams)
    if seams.get("barrier"):
        receipt["test_barrier"] = seams["barrier"]
        wait_at_barrier(seams["barrier"])
    try:
        create_runtime_copy(layout, sha, receipt, seams)
        receipt["step"] = "settings replacement"
        recheck_preimage(layout, pre_bytes)
        write_atomic(layout.settings, layout.settings_dir, data, stat.S_IMODE(pre_info.st_mode), seams, progress=receipt,
                     on_replaced=record_commit(receipt, "settings_written", data))
    except BaseException:
        archive_failed_runtime(layout, receipt, seams)  # pre-commit only (A); a no-op once settings_written (F3)
        raise
    if read_postimage(layout, receipt, seams) != sha256_bytes(data):
        refuse("POSTIMAGE_MISMATCH", EXIT_WRITE_FAILED, expected_postimage_sha256=sha256_bytes(data))
    fail_after_write(seams)
    old_runtime_dir = state["runtime_dir"]
    if entry_exists(old_runtime_dir, "RENAME_SOURCE_NOT_CONTAINED"):  # Y1: an unreadable entry is never "absent"
        superseded = rename_contained(old_runtime_dir, f"{old_runtime_dir}.superseded-{receipt['timestamp_utc']}",
                                      layout.runtime_parent, "dir")
        signal_fault(seams, "signal-after-superseded-rename")  # D7 test hook only; a deferred signal raises nothing
        receipt["previous_runtime_renamed_to"] = superseded  # D5: recorded in the statement after the rename returns
    receipt.update(result="PROMOTED", exit_code=EXIT_OK, previous_receipt=state)
    return EXIT_OK


def wait_at_barrier(barrier_dir):
    write_new_file(barrier_dir + "/reached", barrier_dir, (utc_stamp() + "\n").encode())
    deadline = time.monotonic() + BARRIER_TIMEOUT_SECONDS
    while not os.path.exists(barrier_dir + "/release"):
        if time.monotonic() > deadline:
            refuse("TEST_BARRIER_TIMEOUT", EXIT_WRITE_FAILED)
        time.sleep(0.05)


# --- entry point ------------------------------------------------------------------------

def complete_receipt(receipt):
    """Y3: the one place that gives a receipt the complete documented key set of its kind (RECEIPT_FIELDS), each missing
    key with the value it has when it does not apply (null, false for an operation that did not complete, [] for the
    list of passed checks). A key the run has set keeps its value. Called once, after the outcome is recorded."""
    for key, default in RECEIPT_FIELDS.get(receipt.get("mode"), ()):
        if key not in receipt:
            receipt[key] = list(default) if isinstance(default, list) else default


def transaction_written(receipt):
    """V2: whether this run has made a transaction write (each one is recorded in the statement after it, D5)."""
    return any(receipt.get(key) for key in TRANSACTION_RECORD_KEYS)


def record_outcome(receipt, outcome):
    """The outcome of phase P and the mode. Before any transaction write a refusal keeps its own result, reason and code
    (V1); after one it is FAILED / exit 7 with its code kept in original_code, never REFUSED (V2, W)."""
    if outcome is None:
        return
    if isinstance(outcome, Refusal):
        receipt.update(outcome.extra)
        code = outcome.code
        if transaction_written(receipt) and code != EXIT_WRITE_FAILED:
            receipt["original_code"] = code
            code = EXIT_WRITE_FAILED
        receipt.update(result="FAILED" if code == EXIT_WRITE_FAILED else "REFUSED", reason=outcome.reason,
                       exit_code=code)
        return
    interrupted = not isinstance(outcome, Exception)  # a test-fault KeyboardInterrupt, or SIGINT in phase P
    receipt.update(result="FAILED", reason="INTERRUPTED" if interrupted else "UNEXPECTED_ERROR",
                   exit_code=EXIT_WRITE_FAILED, detail=f"{type(outcome).__name__}: {outcome}")
    if interrupted:
        receipt["interrupted"] = True


def late_interrupt(receipt, outcome, late):
    """R5a-2: a Ctrl+C of phase P that lands after the checks ended and before the handlers are installed ends the run as
    an interrupt (FAILED / INTERRUPTED / exit 7) instead of a traceback; the outcome it overtook is kept in the receipt."""
    if outcome is not None and not isinstance(outcome, KeyboardInterrupt):
        receipt["interrupted_outcome"] = getattr(outcome, "reason", None) or type(outcome).__name__
    elif outcome is None and receipt.get("result"):
        receipt["interrupted_outcome"] = receipt["result"]
    return late


def silence_console(fd):
    """Point a console stream whose write failed at /dev/null, so that the interpreter's final flush cannot fail and
    turn the exit code into 120 (R5a-4)."""
    try:
        null = os.open(os.devnull, os.O_WRONLY)
        try:
            os.dup2(null, fd)
        finally:
            os.close(null)
    except OSError:
        pass


def print_receipt(receipt):
    """The receipt as the last stdout line; True when it was printed."""
    try:
        sys.stdout.write(json_text(receipt, ensure_ascii=True, indent=None, default=str) + "\n")
        sys.stdout.flush()
        return True
    except Exception:
        silence_console(1)
        return False


def report_late_interrupt(receipt, printed):
    """D3: the final console-observation point, after the final receipt's persistence and output attempts (including
    their handled failure paths). A deferred interrupt that the final receipt's snapshot did not include is reported on
    stderr in one line that says truthfully whether that receipt was saved to disk, only printed, or neither; nothing
    else happens because of it."""
    observed = _deferred_signal
    if observed is None or receipt.get("interrupt_deferred"):
        return
    if receipt.get("receipt_saved"):
        where = "the final receipt was saved to disk"
    elif printed:
        where = "the final receipt was only printed (receipt_saved false)"
    else:
        where = "the final receipt was neither written to disk nor printed (receipt_saved false)"
    try:
        sys.stderr.write(f"install_permission_guard: an interrupt ({signal.Signals(observed).name}) was observed after the "
                         f"final receipt's snapshot; it is not in that receipt and had no effect on the outcome; {where}\n")
        sys.stderr.flush()
    except Exception:
        silence_console(2)


def main():
    parser = argparse.ArgumentParser(description="PermissionRequest guard installer (owner-run)")
    parser.add_argument("mode", choices=("install", "verify", "rollback", "promote"))
    parser.add_argument("--manifest", default=None)
    parser.add_argument("--expect-sha256", default=None, help="SHA-256 of this installer file")
    parser.add_argument("--expect-manifest-sha256", default=None, help="SHA-256 of the frozen manifest")
    parser.add_argument("--test-root", default=None, help="tests only: rebase every pinned destination under DIR")
    parser.add_argument("--test-barrier", default=None, help="test seam (with --test-root)")
    parser.add_argument("--test-crash-before-replace", action="store_true", help="test seam (with --test-root)")
    parser.add_argument("--test-short-write", action="store_true", help="test seam (with --test-root)")
    parser.add_argument("--test-fail-after-write", action="store_true", help="test seam (with --test-root)")
    parser.add_argument("--test-fault", action="append", default=[], metavar="POINT",
                        help="test seam (with --test-root): one of " + ", ".join(TEST_FAULT_POINTS))
    opts = parser.parse_args()
    installer_path = os.path.abspath(__file__)
    receipt = {"mode": opts.mode, "timestamp_utc": utc_stamp(), "installer": installer_path,
               "manifest": opts.manifest}
    seams = {"barrier": opts.test_barrier, "crash_before_replace": opts.test_crash_before_replace,
             "short_write": opts.test_short_write, "fail_after_write": opts.test_fail_after_write,
             "faults": tuple(opts.test_fault)}
    writing = opts.mode != "verify"
    layout = None
    outcome = None
    try:
        try:  # phase P runs with the default signal behaviour; each writing mode enters the deferral scope itself
            test_root = validate_test_root(opts.test_root)
            if test_root is None and any(seams.values()):
                refuse("TEST_OPTION_WITHOUT_TEST_ROOT")
            unknown = [p for p in seams["faults"] if p not in TEST_FAULT_POINTS]
            if unknown:
                refuse("TEST_FAULT_UNKNOWN", points=unknown)
            if test_root is not None:
                receipt["test_root"] = test_root
                if seams["faults"]:
                    receipt["test_faults"] = list(seams["faults"])
                if seams["barrier"] is not None:
                    require_contained(seams["barrier"], test_root, "dir", "TEST_BARRIER_NOT_CONTAINED", must_exist=True)
            layout = Layout(test_root)
            if not is_hex64(opts.expect_sha256):
                refuse("INSTALLER_HASH_MISSING")
            own_sha = sha256_bytes(read_regular(installer_path, "INSTALLER_UNREADABLE"))
            if own_sha != opts.expect_sha256:
                refuse("INSTALLER_HASH_MISMATCH", actual_installer_sha256=own_sha)
            receipt["installer_sha256_verified"] = own_sha
            manifest = load_manifest(layout, opts.mode, opts.manifest, opts.expect_manifest_sha256, receipt)
            receipt["settings_target"] = layout.settings
            receipt["evidence_dir"] = layout.evidence_dir
            install_manifest_sha = (receipt["manifest_sha256_verified"] if opts.mode != "promote"
                                    else (sha256_bytes(read_regular(layout.install_manifest, "MANIFEST_MISSING"))
                                          if entry_exists(layout.install_manifest, "MANIFEST_MISSING") else None))
            if install_manifest_sha is None:  # R5b-c-5: no rollback command can be printed without its manifest pin
                receipt.update(rollback_command=None, rollback_command_unavailable="INSTALL_MANIFEST_MISSING")
            else:
                receipt["rollback_command"] = (
                    f"python3 -I {installer_path} rollback --manifest {layout.install_manifest} --expect-sha256 {own_sha} "
                    f"--expect-manifest-sha256 {install_manifest_sha}" + (f" --test-root {test_root}" if test_root else ""))
            if opts.mode == "install":
                do_install(layout, manifest, receipt, seams)
            elif opts.mode == "verify":
                do_verify(layout, manifest, receipt, seams)
            elif opts.mode == "rollback":
                do_rollback(layout, manifest, receipt, seams)
            else:
                do_promote(layout, manifest, receipt, seams)
        except BaseException as exc:  # a refusal, an unexpected error, a test-fault KeyboardInterrupt, or SIGINT in phase P
            outcome = exc
        if writing:
            enter_deferral_scope(seams)  # V2, D1: every writing mode reports its outcome inside the scope
    except KeyboardInterrupt as late:
        outcome = late_interrupt(receipt, outcome, late)
    while writing and not _scope_entered:  # completes a scope entry that a late phase-P Ctrl+C interrupted
        try:
            enter_deferral_scope(seams)
        except KeyboardInterrupt as late:
            outcome = late_interrupt(receipt, outcome, late)
    record_outcome(receipt, outcome)
    if receipt.get("exit_code") == EXIT_WRITE_FAILED and "step" in receipt and transaction_written(receipt):
        receipt.setdefault("failed_step", receipt["step"])  # R5b-c-3: a write step failed only if one was taken
    complete_receipt(receipt)  # Y3: every documented key of this mode's receipt kind, after the outcome is recorded
    changed = bool(receipt.get("settings_written") or receipt.get("settings_restored"))
    if writing:
        if layout is not None:
            try:
                pause_in_scope(layout, seams)  # test only; before the snapshots of the state and the final receipt
            except Exception as exc:  # a test seam never decides the outcome; its failure is reported
                receipt["test_pause_error"] = f"{type(exc).__name__}: {exc}"
            save_receipt(layout, receipt, update_state=changed, seams=seams)  # outcome S; never raises
        else:  # no trusted evidence directory: the receipt is printed only (V2)
            receipt.update(receipt_saved=False, receipt_save_error="EVIDENCE_DIR_NOT_VERIFIED", receipt_save_temp_path=None)
        if "interrupt_deferred" not in receipt:
            receipt.update(interrupt_snapshot())  # the printed receipt's snapshot when no receipt file was attempted
    else:  # verify writes nothing, its receipt included (Y3: the receipt-only keys say so)
        receipt.update(receipt_saved=False, receipt_save_error=None, receipt_save_temp_path=None)
    printed = print_receipt(receipt)
    report_late_interrupt(receipt, printed)  # the final console-observation point (D3)
    signal_fault(seams, "signal-after-final-observation")
    return receipt.get("exit_code", EXIT_WRITE_FAILED)


if __name__ == "__main__":
    sys.exit(main())
