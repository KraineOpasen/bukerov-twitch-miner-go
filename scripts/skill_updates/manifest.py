"""Provider-manifest reading and provenance regeneration.

A provider manifest is the record of *what we reviewed*. This module can read one, and can
regenerate the mechanical half of it -- blob hashes, modes, `locally_modified`, `patch_ids`,
`upstream_commit`, `upstream_tree` -- from bytes that are actually on disk and actually in the
upstream object database. It never regenerates the *judgement* half: `reviewed_at`,
`reviewed_by`, `classification`, `scripts_audited`, `audit_ref`, exclusion verdicts and prose
notes are carried through untouched, because no automated run has the standing to change them.

Two deliberate properties:

* **Every derived field is derived, never copied forward.** A regenerated entry's
  `vendored_blob_sha` is hashed from the bytes this run wrote, and its `locally_modified` is
  recomputed by comparing those bytes to the new upstream blob. Copying an old value forward
  would let a stale "false" survive a change that made it true -- the single most dangerous
  failure mode for a provenance file, because it reads as reviewed fact.

* **Hashes are computed in-process, not shelled out.** `git_blob_sha()` implements git's blob
  object hash directly. It agrees with `git hash-object` by construction (same preimage), and
  the validator continues to shell out so the recorded hashes stay hand-reproducible; doing it
  in-process here just avoids thousands of subprocess spawns per run.

Field ordering is fixed by `FILE_KEY_ORDER` / `SKILL_KEY_ORDER` so that re-running the bot on
an unchanged tree produces a byte-identical manifest. That is what makes "no drift -> no diff"
a mechanical property rather than a hope.
"""

import hashlib
import json
import os
import re

from .errors import ConfigError

#: Patch-marker forms, mirrored EXACTLY from scripts/validate-agent-governance.py. They are
#: duplicated rather than imported because that script is a standalone stdlib validator with no
#: package structure to import from. `tests/test_analyze.py::TestMirroredConstants` asserts the
#: two sets of patterns stay in agreement, so the duplication cannot drift silently.
PATCH_OPEN_RE = re.compile(r"<!--\s*bukerov-local-patch:\s*([\w-]+)\s*-->")
PATCH_SELFCLOSING_RE = re.compile(r"<!--\s*bukerov-local-patch:\s*([\w-]+)\s*—[^>]*-->")
PY_MARK_RE = re.compile(r"#\s*bukerov-local-patch:\s*([\w-]+)")
JS_MARK_RE = re.compile(r"//\s*bukerov-local-patch:\s*([\w-]+)")

HASH_MARK_EXTS = (".py", ".sh", ".bash", ".zsh", ".yaml", ".yml")
SLASH_MARK_EXTS = (".mjs", ".js", ".ts")
WRAP_MARK_EXTS = (".md", ".html")

#: Canonical key order for a files[] entry, matching the five providers already on the
#: file-level schema. Fixed order == reproducible output.
FILE_KEY_ORDER = ("path", "origin", "upstream_path", "upstream_blob_sha", "upstream_mode",
                  "vendored_mode", "vendored_blob_sha", "locally_modified", "patch_ids",
                  "reason")

#: Canonical key order for a skills[] entry. Unlisted keys are appended in their existing order,
#: so a provider-specific field (mattpocock's `category`, a per-skill `notes`) survives a
#: regeneration in a stable position instead of being dropped or reshuffled.
SKILL_KEY_ORDER = ("name", "category", "path", "upstream_path", "renamed_from", "invocation",
                   "classification", "locally_modified", "patch_ids", "scripts_audited",
                   "audit_ref", "notes", "files")

#: The mode every vendored file carries on disk. Upstream frequently ships scripts 100755;
#: this project normalizes to 100644 because vendored files are content an agent READS, never a
#: binary invoked off disk. See check_provider_vendored_modes in the governance validator.
VENDORED_MODE = "100644"


def git_blob_sha(data):
    """git's object name for a blob holding `data`.

    git hashes ``b"blob " + str(len(data)) + b"\\0" + data``. Implemented here so a run that
    touches thousands of files does not spawn thousands of processes; identical by construction
    to `git hash-object`, which is what the governance validator uses.
    """
    header = b"blob %d\x00" % len(data)
    return hashlib.sha1(header + data).hexdigest()


def load(path):
    """Read and minimally shape-check a provider manifest."""
    try:
        with open(path, encoding="utf-8") as handle:
            doc = json.load(handle)
    except OSError as exc:
        raise ConfigError("cannot read manifest %s: %s" % (path, exc))
    except ValueError as exc:
        raise ConfigError("manifest %s is not valid JSON: %s" % (path, exc))
    if not isinstance(doc, dict) or not isinstance(doc.get("skills"), list):
        raise ConfigError("manifest %s must be an object with a skills list" % path)
    return doc


def dump(doc):
    """Serialize a manifest exactly the way the checked-in files are formatted.

    Two-space indent, no key sorting (order is already canonical and carries meaning), and a
    trailing newline. `ensure_ascii=False` preserves the non-ASCII characters the existing
    manifests contain -- em dashes in prose notes, most visibly -- so a regeneration does not
    produce a diff consisting entirely of escape sequences.
    """
    return json.dumps(doc, indent=2, ensure_ascii=False) + "\n"


def marker_ids(path, data):
    """Patch-marker ids inside one file, selected by extension exactly as the validator does.

    `data` is passed in rather than re-read so this works on bytes a run has computed but not
    yet written -- which is how a prepared candidate's `patch_ids` are regenerated from merged
    content before that content lands on disk.
    """
    name = os.path.basename(path)
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError:
        return []
    ids = set()
    if name.endswith(WRAP_MARK_EXTS):
        ids.update(PATCH_OPEN_RE.findall(text))
        ids.update(PATCH_SELFCLOSING_RE.findall(text))
    elif name.endswith(HASH_MARK_EXTS):
        ids.update(PY_MARK_RE.findall(text))
    elif name.endswith(SLASH_MARK_EXTS):
        ids.update(JS_MARK_RE.findall(text))
    elif "." not in name and text.startswith("#!"):
        ids.update(PY_MARK_RE.findall(text))
    return sorted(ids)


def parse_frontmatter(data):
    """Return ``(mapping, ok)`` for a SKILL.md's ``---`` fenced frontmatter.

    Mirrors the validator's parser, including its tolerance rules: the fence must open on line
    one, the first later ``---`` closes it, blank lines are skipped, and a single layer of
    matching quotes is stripped from a value. `ok` is False when there is no valid fence pair --
    which is itself a reportable authority-surface change, so it is returned rather than raised.
    """
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError:
        return {}, False
    lines = text.splitlines()
    if not lines or lines[0].strip() != "---":
        return {}, False
    end = None
    for index in range(1, len(lines)):
        if lines[index].strip() == "---":
            end = index
            break
    if end is None:
        return {}, False
    fields = {}
    for line in lines[1:end]:
        if not line.strip():
            continue
        match = re.match(r"^([A-Za-z0-9_-]+):\s*(.*)$", line)
        if match:
            value = match.group(2).strip()
            if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
                value = value[1:-1]
            fields[match.group(1)] = value
    return fields, True


def iter_file_entries(doc):
    """Yield ``(skill, file_entry)`` for every declared file, in manifest order."""
    for skill in doc.get("skills", []):
        if not isinstance(skill, dict):
            continue
        for entry in skill.get("files", []) or []:
            if isinstance(entry, dict) and isinstance(entry.get("path"), str):
                yield skill, entry


def ordered_file_entry(entry):
    """Return `entry` with keys in canonical order, dropping keys whose value is absent."""
    out = {}
    for key in FILE_KEY_ORDER:
        if key in entry:
            out[key] = entry[key]
    for key in entry:
        if key not in out:
            out[key] = entry[key]
    return out


def ordered_skill(skill):
    """Return `skill` with keys in canonical order; unknown keys keep their relative order."""
    out = {}
    for key in SKILL_KEY_ORDER:
        if key in skill:
            out[key] = skill[key]
    for key in skill:
        if key not in out:
            out[key] = skill[key]
    if isinstance(out.get("files"), list):
        out["files"] = [ordered_file_entry(f) if isinstance(f, dict) else f
                        for f in out["files"]]
    return out


def read_vendored(repo_root, relpath):
    """Bytes of one vendored file, or None when it is absent.

    A symlink is reported as absent-with-a-flag by `lstat_kind`; this function follows nothing,
    so a symlinked vendored file can never be silently read as though it were a regular file.
    """
    full = os.path.join(repo_root, relpath)
    if os.path.islink(full) or not os.path.isfile(full):
        return None
    with open(full, "rb") as handle:
        return handle.read()


def lstat_kind(repo_root, relpath):
    """Classify an on-disk path without following symlinks.

    Returns one of "absent", "symlink", "dir", "file-exec", "file". The distinction between
    "file" and "file-exec" matters because an executable bit appearing on a vendored file is a
    reportable condition, and "symlink" matters because a symlink is never an acceptable
    vendored artifact under this project's governance.
    """
    full = os.path.join(repo_root, relpath)
    if not os.path.lexists(full):
        return "absent"
    if os.path.islink(full):
        return "symlink"
    if os.path.isdir(full):
        return "dir"
    return "file-exec" if os.stat(full).st_mode & 0o111 else "file"


def on_disk_inventory(repo_root, skill_relpath):
    """Every regular file actually present under one vendored skill directory, sorted.

    Walks with `topdown` sorting so the result is stable across filesystems -- `os.walk` yields
    directory entries in arbitrary order, and an unsorted inventory would make the regenerated
    manifest differ run to run on the same tree.
    """
    root = os.path.join(repo_root, skill_relpath)
    found = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames.sort()
        for name in sorted(filenames):
            full = os.path.join(dirpath, name)
            found.append(os.path.relpath(full, repo_root).replace(os.sep, "/"))
    return sorted(found)
