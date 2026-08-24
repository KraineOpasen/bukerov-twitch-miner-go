#!/usr/bin/env python3
"""validate-agent-governance.py — offline, read-only consistency checks for the
repo-native governance layer (.claude/**, docs/agents/**, docs/adr/**) that elaborates the canonical
GOVERNANCE_V3.md at the repo root.

Stdlib only, no network access, deterministic. Exits 0 if every check passes,
1 if any check fails. Diagnostics print as `[FAIL] check-name: detail` /
`[PASS] check-name` lines.

This script covers TWO independently vendored, non-overlapping skill sets, each with its own
manifest/ledger pair (see the MANIFESTS registry below):
  - mattpocock/skills  -> docs/agents/mattpocock-skills-manifest.json / -patches.md (skill-level:
    one upstream_blob_sha per skill's SKILL.md).
  - anthropics/skills  -> docs/agents/anthropic-skills-manifest.json / -patches.md (file-level:
    one upstream_blob_sha per vendored file, since two of its three skills ship real scripts).

Usage:
    python3 scripts/validate-agent-governance.py
    python3 scripts/validate-agent-governance.py --self-test-hook
    python3 scripts/validate-agent-governance.py --self-test
        Runs ONLY this script's own offline fixture matrix (G1-G12, N1-N16) -- no network, no
        sleeps, fully deterministic. Most fixtures build synthetic, never-committed trees under
        tempfile.TemporaryDirectory; a couple (G11, G12) instead read the real repository
        read-only, to check the vendored MANIFESTS registry's shape and to validate the real
        (empty) project manifest. The guarantee this flag makes is "this run never WRITES to the
        repo" -- not "every fixture is synthetic-only". Prints "N/N self-test fixtures passed"
        and exits 1 on any fixture failure, 0 otherwise. Independent of --self-test-hook and of
        the default run.

Env vars (all optional, all make specific checks stricter, never required):
    GOVERNANCE_UPSTREAM_DIR_MATTPOCOCK  path to a read-only clone of mattpocock/skills; when set,
                                        the mattpocock blob-hash check verifies against it directly.
    GOVERNANCE_UPSTREAM_DIR             legacy fallback for the above, kept for compatibility with
                                        scripts/CI that pre-date the anthropic skill set.
    GOVERNANCE_UPSTREAM_DIR_ANTHROPIC   path to a read-only clone of anthropics/skills; when set,
                                        the anthropic (file-level) blob-hash check verifies against
                                        it directly, for every file in every skill's files[].
    GOVERNANCE_BASE_SHA                 a git ref; when set, the "application paths untouched"
                                        check diffs against it instead of the working tree.

A blob-hash mismatch on a file whose skill has `scripts_audited: true` in the anthropic manifest
means more than "content drifted": it means a script that was read end-to-end during the last
review no longer matches what was reviewed, so re-audit (not just re-hash) is required before
trusting it again.
"""
import contextlib
import datetime
import io
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CLAUDE_DIR = os.path.join(REPO_ROOT, ".claude")
SKILLS_DIR = os.path.join(CLAUDE_DIR, "skills")
RULES_DIR = os.path.join(CLAUDE_DIR, "rules")
DOCS_AGENTS_DIR = os.path.join(REPO_ROOT, "docs", "agents")
MANIFEST_PATH = os.path.join(DOCS_AGENTS_DIR, "mattpocock-skills-manifest.json")
PATCHES_PATH = os.path.join(DOCS_AGENTS_DIR, "mattpocock-skills-patches.md")
ANTHROPIC_MANIFEST_PATH = os.path.join(DOCS_AGENTS_DIR, "anthropic-skills-manifest.json")
ANTHROPIC_PATCHES_PATH = os.path.join(DOCS_AGENTS_DIR, "anthropic-skills-patches.md")
# Third ownership class: project-owned first-party skills. Deliberately NOT added to the
# MANIFESTS registry below -- that registry drives vendored-only logic (excluded-skill keys,
# patch ledgers, upstream blob-hash schemas), none of which apply to first-party content. See
# docs/agents/project-skills-policy.md and check_project_manifest()/validate_project_manifest().
PROJECT_MANIFEST_PATH = os.path.join(DOCS_AGENTS_DIR, "project-skills-manifest.json")
PROJECT_OWNERSHIP_CLASS = "project-first-party"
SETTINGS_PATH = os.path.join(CLAUDE_DIR, "settings.json")
HOOK_PATH = os.path.join(CLAUDE_DIR, "hooks", "governance-policy.py")

# Registry of every vendored-skill manifest/ledger pair this script knows about. "schema" records
# which blob-hash granularity that manifest uses: "skill-level" (one upstream_blob_sha per skill's
# SKILL.md, mattpocock's format) or "file-level" (one upstream_blob_sha per vendored file, in a
# per-skill files[] array -- anthropic's format, since two of its three skills ship real scripts).
MANIFESTS = [
    {
        "label": "mattpocock",
        "manifest": MANIFEST_PATH,
        "patches": PATCHES_PATH,
        "upstream_env": "GOVERNANCE_UPSTREAM_DIR_MATTPOCOCK",
        "legacy_upstream_env": "GOVERNANCE_UPSTREAM_DIR",
        "schema": "skill-level",
        "excluded_key": "excluded",
    },
    {
        "label": "anthropic",
        "manifest": ANTHROPIC_MANIFEST_PATH,
        "patches": ANTHROPIC_PATCHES_PATH,
        "upstream_env": "GOVERNANCE_UPSTREAM_DIR_ANTHROPIC",
        "legacy_upstream_env": None,
        "schema": "file-level",
        "excluded_key": "excluded_skills",
    },
]

ALLOWED_SKILL_KEYS = {"name", "description", "disable-model-invocation", "argument-hint"}
# anthropic-owned skill dirs additionally allow "license" (skill-creator-anthropic doesn't use it,
# but frontend-design and webapp-testing both carry `license: Complete terms in LICENSE.txt`).
ALLOWED_SKILL_KEYS_ANTHROPIC = ALLOWED_SKILL_KEYS | {"license"}
# project-owned first-party skill dirs: an explicit, independent copy of the base set (not the
# same object -- set(...) so a future in-place mutation of one can never leak into the other),
# selected explicitly by ownership in check_frontmatter_keys/validate_project_manifest, never
# reached by an else-fallback.
ALLOWED_SKILL_KEYS_PROJECT = set(ALLOWED_SKILL_KEYS)
ALLOWED_RULE_KEYS = {"paths"}
FORBIDDEN_VENDOR_NAMES = {".github", ".claude-plugin", "package.json", "package-lock.json", "openai.yaml"}
APPLICATION_PATH_PREFIXES = ("internal/", "cmd/")
APPLICATION_PATH_EXACT_PREFIXES = ("go.mod", "go.sum", "Dockerfile", "docker-compose", ".github/workflows/")

# Skill names Claude Code (or another first-party surface) already provides. No vendored skill dir
# name and no manifest skill name may collide with one of these -- that's exactly the problem the
# skc-rename-vendored patch fixed for skill-creator -> skill-creator-anthropic; this denylist keeps
# a future re-vendor of any OTHER skill from silently reintroducing the same class of collision.
BUILTIN_SKILL_NAMES = {
    "skill-creator", "dataviz", "artifact-design", "artifact-capabilities", "run", "review",
    "security-review", "init", "morning", "loop", "claude-api", "update-config",
    "keybindings-help", "fewer-permission-prompts", "simplify", "session-start-hook", "xlsx",
    "pptx", "pdf", "docx",
}

# Bidi control + zero-width/invisible characters that have no legitimate reason
# to appear in this project's governance Markdown/JSON/Python.
HIDDEN_UNICODE = "".join(chr(c) for c in (
    0x200B, 0x200C, 0x200D, 0x200E, 0x200F, 0xFEFF,
    0x202A, 0x202B, 0x202C, 0x202D, 0x202E,
    0x2066, 0x2067, 0x2068, 0x2069,
))

RESULTS = []


def report(name, ok, details=None):
    RESULTS.append((name, ok, details or []))
    print("[%s] %s" % ("PASS" if ok else "FAIL", name))
    for d in details or []:
        print("  [FAIL] %s: %s" % (name, d))


def rel(path):
    return os.path.relpath(path, REPO_ROOT)


def list_skill_dirs():
    if not os.path.isdir(SKILLS_DIR):
        return []
    return sorted(
        d for d in os.listdir(SKILLS_DIR)
        if os.path.isdir(os.path.join(SKILLS_DIR, d))
    )


def load_manifest(path=MANIFEST_PATH):
    with open(path) as f:
        return json.load(f)


def load_anthropic_manifest():
    return load_manifest(ANTHROPIC_MANIFEST_PATH)


def anthropic_skill_dir_names():
    """Directory names (under .claude/skills/) the anthropic manifest claims to own."""
    return {s["name"] for s in load_anthropic_manifest().get("skills", [])}


def mattpocock_skill_dir_names():
    """Directory names (under .claude/skills/) the mattpocock manifest claims to own."""
    return {s["name"] for s in load_manifest().get("skills", [])}


def project_skill_names():
    """Skill names (under .claude/skills/) the project (first-party) manifest claims to own, as a
    LIST -- deliberately NOT deduplicated, mirroring how the vendored sources are read (each
    MANIFESTS entry's names.extend(...) in check_unique_skill_names also sees every raw entry).
    Returning a set here would silently collapse two same-named project entries into one before
    any duplicate-detection code ever saw them. Callers that need set semantics (membership,
    union/difference for partition/fs-consistency checks) must wrap the result in set(...)
    explicitly at the call site -- never rely on this function to dedupe for them. Never routes
    through the vendored MANIFESTS registry -- reads PROJECT_MANIFEST_PATH directly."""
    try:
        manifest = load_manifest(PROJECT_MANIFEST_PATH)
    except Exception:
        return []
    # A JSON-valid but structurally invalid manifest (top-level array, or a "skills" value that
    # isn't a list) must fail closed here, not raise: this function is the FIRST caller every other
    # consumer of the project manifest goes through (check_unique_skill_names, check_frontmatter_keys,
    # check_manifest_fs_consistency, check_manifest_ownership_partition,
    # check_builtin_collision_denylist), so an uncaught AttributeError/TypeError here would abort
    # the entire check run before check_project_manifest ever gets to report the real diagnostic.
    if not isinstance(manifest, dict):
        return []
    skills = manifest.get("skills", [])
    if not isinstance(skills, list):
        return []
    return [
        s["name"] for s in skills
        if isinstance(s, dict) and isinstance(s.get("name"), str)
    ]


def parse_frontmatter(path):
    """Returns (keys:set, ok:bool) — ok False if the file has no valid --- fence pair."""
    with open(path, encoding="utf-8") as f:
        lines = f.readlines()
    if not lines or lines[0].strip() != "---":
        return set(), False
    end = None
    for i in range(1, len(lines)):
        if lines[i].strip() == "---":
            end = i
            break
    if end is None:
        return set(), False
    keys = set()
    for line in lines[1:end]:
        line = line.rstrip("\n")
        if not line.strip():
            continue
        m = re.match(r"^([A-Za-z0-9_-]+):", line)
        if m:
            keys.add(m.group(1))
    return keys, True


def parse_frontmatter_value(path, key):
    """Returns the string value of `<key>:` inside a file's --- frontmatter fence, stripped of
    surrounding whitespace and a single layer of matching quotes -- or None if the file has no
    valid fence or the key is absent. Sibling helper to parse_frontmatter(), which returns keys
    only; this one is needed to cross-check a manifest's declared `name` against the skill's own
    frontmatter `name:` value."""
    with open(path, encoding="utf-8") as f:
        lines = f.readlines()
    if not lines or lines[0].strip() != "---":
        return None
    end = None
    for i in range(1, len(lines)):
        if lines[i].strip() == "---":
            end = i
            break
    if end is None:
        return None
    pattern = re.compile(r"^%s:\s*(.*)$" % re.escape(key))
    for line in lines[1:end]:
        line = line.rstrip("\n")
        m = pattern.match(line)
        if m:
            val = m.group(1).strip()
            if len(val) >= 2 and val[0] == val[-1] and val[0] in "\"'":
                val = val[1:-1]
            return val
    return None


# --------------------------------------------------------------------------
# Checks
# --------------------------------------------------------------------------


def check_required_files():
    required = [
        "CLAUDE.md", "GOVERNANCE_V3.md", "CONTEXT.md", ".gitignore",
        ".claude/settings.json", ".claude/hooks/governance-policy.py", ".claude/skills/LICENSE",
        "docs/agents/operation-modes.md", "docs/agents/task-contract.md", "docs/agents/quality-gates.md",
        "docs/agents/issue-tracker.md", "docs/agents/domain.md", "docs/agents/triage-labels.md",
        "docs/agents/mattpocock-skills-manifest.json", "docs/agents/mattpocock-skills-patches.md",
        "docs/agents/mattpocock-skills-policy.md", "docs/adr/0001-agent-governance-v2.md",
        "docs/adr/0002-canonical-governance-v3.md",
        "scripts/validate-agent-governance.py",
        "docs/agents/anthropic-skills-manifest.json", "docs/agents/anthropic-skills-patches.md",
        "docs/agents/anthropic-skills-policy.md",
        "docs/agents/project-skills-manifest.json", "docs/agents/project-skills-policy.md",
    ]
    missing = [p for p in required if not os.path.isfile(os.path.join(REPO_ROOT, p))]
    report("required-files-exist", not missing, ["missing %s" % p for p in missing])


def check_json_validity():
    details = []
    for p in (SETTINGS_PATH, MANIFEST_PATH, ANTHROPIC_MANIFEST_PATH, PROJECT_MANIFEST_PATH):
        try:
            with open(p) as f:
                json.load(f)
        except Exception as e:
            details.append("%s: %s" % (rel(p), e))
    report("json-validity", not details, details)


def check_no_symlinks_no_exec():
    details = []
    if os.path.isdir(CLAUDE_DIR):
        for dirpath, dirnames, filenames in os.walk(CLAUDE_DIR):
            for name in dirnames + filenames:
                full = os.path.join(dirpath, name)
                if os.path.islink(full):
                    details.append("symlink: %s" % rel(full))
            for name in filenames:
                full = os.path.join(dirpath, name)
                if os.path.islink(full):
                    continue
                st = os.stat(full)
                if st.st_mode & 0o111:
                    details.append("executable bit set: %s" % rel(full))
    report("no-symlinks-no-exec-under-claude", not details, details)


def check_skill_dirs_have_skillmd():
    details = []
    for name in list_skill_dirs():
        if not os.path.isfile(os.path.join(SKILLS_DIR, name, "SKILL.md")):
            details.append("%s has no SKILL.md" % name)
    report("every-skill-dir-has-skillmd", not details, details)


def check_unique_skill_names():
    dirs = list_skill_dirs()
    dup = {d for d in dirs if dirs.count(d) > 1}
    # Union across ALL THREE ownership sources (the two vendored manifests plus the project
    # manifest): a name duplicated within one source OR appearing in more than one source is
    # equally a collision -- two different skills must never share a name.
    names = []
    for entry in MANIFESTS:
        manifest = load_manifest(entry["manifest"])
        names.extend(s["name"] for s in manifest["skills"])
    names.extend(project_skill_names())
    dup_manifest = {n for n in names if names.count(n) > 1}
    details = ["duplicate dir: %s" % d for d in dup] + ["duplicate manifest entry (union of all manifests): %s" % n for n in dup_manifest]
    report("unique-skill-names", not details, details)


def check_frontmatter_keys():
    details = []
    anthropic_dirs = anthropic_skill_dir_names()
    project_dirs = set(project_skill_names())  # membership only -- project_skill_names() is a
    # list (not deduplicated, see its docstring), so callers needing set semantics wrap it here.
    for name in list_skill_dirs():
        skillmd = os.path.join(SKILLS_DIR, name, "SKILL.md")
        if not os.path.isfile(skillmd):
            continue
        keys, ok = parse_frontmatter(skillmd)
        if not ok:
            details.append("%s/SKILL.md: no valid frontmatter fence" % name)
            continue
        # Allowlist selected explicitly by ownership -- anthropic dirs get the anthropic set,
        # project dirs get the (currently identical, but independently named) project set,
        # everything else (mattpocock or unclaimed) falls through to the base set. Never an
        # accidental else-fallback: project dirs are checked by membership, not by exclusion.
        if name in anthropic_dirs:
            allowed = ALLOWED_SKILL_KEYS_ANTHROPIC
        elif name in project_dirs:
            allowed = ALLOWED_SKILL_KEYS_PROJECT
        else:
            allowed = ALLOWED_SKILL_KEYS
        extra = keys - allowed
        if extra:
            details.append("%s/SKILL.md: unexpected frontmatter keys %s" % (name, sorted(extra)))
        if "name" not in keys or "description" not in keys:
            details.append("%s/SKILL.md: missing required key(s) name/description" % name)
    if os.path.isdir(RULES_DIR):
        for fname in sorted(os.listdir(RULES_DIR)):
            if not fname.endswith(".md"):
                continue
            path = os.path.join(RULES_DIR, fname)
            keys, ok = parse_frontmatter(path)
            if not ok:
                details.append("rules/%s: no valid frontmatter fence" % fname)
                continue
            extra = keys - ALLOWED_RULE_KEYS
            if extra:
                details.append("rules/%s: unexpected frontmatter keys %s" % (fname, sorted(extra)))
            if "paths" not in keys:
                details.append("rules/%s: missing required key 'paths'" % fname)
    report("frontmatter-keys-allowed", not details, details)


LINK_RE = re.compile(r"\[[^\]]*\]\(([^)]+)\)")
FENCE_RE = re.compile(r"```.*?```", re.DOTALL)


def strip_code_fences(text):
    """Drop fenced code blocks so illustrative example links inside a ```md
    template (not real links in this file) aren't checked as real targets."""
    return FENCE_RE.sub("", text)


def check_relative_links_resolve():
    details = []
    for name in list_skill_dirs():
        skill_dir = os.path.join(SKILLS_DIR, name)
        for dirpath, _, filenames in os.walk(skill_dir):
            for fname in filenames:
                if not fname.endswith(".md"):
                    continue
                path = os.path.join(dirpath, fname)
                with open(path, encoding="utf-8") as f:
                    text = strip_code_fences(f.read())
                for target in LINK_RE.findall(text):
                    target = target.split(" ", 1)[0].strip()
                    if not target or target.startswith(("http://", "https://", "mailto:", "#")):
                        continue
                    if target.startswith("/"):
                        details.append("%s: absolute-path link %r" % (rel(path), target))
                        continue
                    resolved = os.path.normpath(os.path.join(dirpath, target))
                    if not os.path.isfile(resolved):
                        details.append("%s: dangling link %r" % (rel(path), target))
    report("relative-links-resolve", not details, details)


def check_no_absolute_path_imports():
    # A stricter alias of the absolute-link half of check_relative_links_resolve,
    # kept separate so a future change to link-checking can't silently drop this.
    details = []
    for name in list_skill_dirs():
        skill_dir = os.path.join(SKILLS_DIR, name)
        for dirpath, _, filenames in os.walk(skill_dir):
            for fname in filenames:
                if not fname.endswith(".md"):
                    continue
                path = os.path.join(dirpath, fname)
                with open(path, encoding="utf-8") as f:
                    text = strip_code_fences(f.read())
                for target in LINK_RE.findall(text):
                    target = target.split(" ", 1)[0].strip()
                    if target.startswith("/") and not target.startswith(("http://", "https://")):
                        details.append("%s: absolute-path reference %r" % (rel(path), target))
    report("no-absolute-path-imports-in-skills", not details, details)


def check_forbidden_vendor_files_absent():
    details = []
    if os.path.isdir(SKILLS_DIR):
        for dirpath, dirnames, filenames in os.walk(SKILLS_DIR):
            for name in list(dirnames) + filenames:
                if name in FORBIDDEN_VENDOR_NAMES:
                    details.append(rel(os.path.join(dirpath, name)))
    report("no-forbidden-vendor-files", not details, details)


def check_manifest_fs_consistency():
    """Per-source filesystem consistency, reported under the original single-manifest report
    name. With three ownership sources now in play (mattpocock, anthropic, and the project
    manifest), a directory legitimately claimed by one source must not be flagged as "not in
    manifest" when checking another -- so each source's "dir but not in manifest" half only
    considers directories no OTHER source already claims. Directories claimed by no source at all
    are still caught here (via every source independently), and again, more directly, by
    check_manifest_ownership_partition below. The project manifest is read directly
    (project_skill_names()), never via the vendored MANIFESTS registry."""
    fs_names = set(list_skill_dirs())
    manifest_names_by_label = {}
    for entry in MANIFESTS:
        manifest = load_manifest(entry["manifest"])
        manifest_names_by_label[entry["label"]] = {s["name"] for s in manifest["skills"]}
    manifest_names_by_label["project"] = set(project_skill_names())  # set() needed: this dict's
    # values are combined with |/- below, and project_skill_names() is deliberately a
    # non-deduplicated list (see its docstring).

    details = []
    for label, manifest_names in manifest_names_by_label.items():
        other_claimed = set()
        for other_label, other_names in manifest_names_by_label.items():
            if other_label != label:
                other_claimed |= other_names
        only_manifest = manifest_names - fs_names
        only_fs = (fs_names - other_claimed) - manifest_names
        details += ["%s: in manifest but no dir: %s" % (label, n) for n in sorted(only_manifest)]
        details += ["%s: dir but not in manifest: %s" % (label, n) for n in sorted(only_fs)]
    report("manifest-filesystem-consistency", not details, details)


def partition_details(fs_names, names_by_label):
    """Pure core of check_manifest_ownership_partition: given the set of on-disk skill dir names
    and a {label: set(names)} map of every ownership source, returns the list of partition
    violations (unclaimed dirs, phantom entries, cross-source name collisions) -- empty list means
    every dir is claimed by EXACTLY one source. Shared by the production check and the self-test
    fixtures (G1-G4) so both exercise the identical logic, never a parallel reimplementation."""
    details = []
    all_claimed = set()
    for names in names_by_label.values():
        all_claimed |= names

    unclaimed = fs_names - all_claimed
    details += ["dir claimed by no manifest: %s" % n for n in sorted(unclaimed)]

    phantom = all_claimed - fs_names
    details += ["manifest entry with no on-disk dir: %s" % n for n in sorted(phantom)]

    labels = list(names_by_label)
    for i in range(len(labels)):
        for j in range(i + 1, len(labels)):
            collision = names_by_label[labels[i]] & names_by_label[labels[j]]
            for n in sorted(collision):
                details.append("cross-manifest name collision: %s claimed by both %s and %s" % (
                    n, labels[i], labels[j]))

    return details


def check_manifest_ownership_partition():
    """Every .claude/skills/<dir> must be claimed by EXACTLY one of the three ownership sources
    (mattpocock, anthropic, project): the union of all their skill names must equal the set of
    directories on disk, and no name may appear in more than one source (a cross-source name
    collision would mean two different reviewed owners both think they own the same directory).
    The project manifest is read directly (project_skill_names()), never via the vendored
    MANIFESTS registry. Core logic lives in partition_details() above."""
    fs_names = set(list_skill_dirs())
    names_by_label = {}
    for entry in MANIFESTS:
        manifest = load_manifest(entry["manifest"])
        names_by_label[entry["label"]] = {s["name"] for s in manifest["skills"]}
    names_by_label["project"] = set(project_skill_names())  # set() needed: partition_details()
    # does set algebra (&, -), and project_skill_names() is deliberately a non-deduplicated list.

    details = partition_details(fs_names, names_by_label)
    report("manifest-ownership-partition", not details, details)


def check_excluded_absent():
    fs_names = set(list_skill_dirs())
    details = []
    for entry in MANIFESTS:
        manifest = load_manifest(entry["manifest"])
        excluded_names = {e["name"] for e in manifest.get(entry["excluded_key"], [])}
        present = excluded_names & fs_names
        details += ["%s: excluded but present: %s" % (entry["label"], n) for n in sorted(present)]

    # anthropic-specific: the pre-rename source name ("skill-creator") must not exist as a dir --
    # only the renamed skill-creator-anthropic may be present.
    anthropic_manifest = load_anthropic_manifest()
    renamed_froms = {
        s["renamed_from"] for s in anthropic_manifest.get("skills", []) if s.get("renamed_from")
    }
    present_renamed = renamed_froms & fs_names
    details += ["anthropic: renamed_from source present as dir: %s" % n for n in sorted(present_renamed)]

    report("excluded-skills-absent", not details, details)


def check_blob_hashes():
    """mattpocock branch: unchanged from before the anthropic set was added -- skill-level,
    SKILL.md-only, locally_modified skills are skipped (only unmodified ones can be verified
    byte-for-byte against a single recorded hash)."""
    manifest = load_manifest()
    details = []
    upstream_dir = os.environ.get("GOVERNANCE_UPSTREAM_DIR_MATTPOCOCK") or os.environ.get("GOVERNANCE_UPSTREAM_DIR")
    for skill in manifest["skills"]:
        if skill.get("locally_modified"):
            continue  # only unmodified skills can be hash-verified byte-for-byte
        local_path = os.path.join(REPO_ROOT, skill["path"], "SKILL.md")
        if not os.path.isfile(local_path):
            continue  # already reported by manifest-filesystem-consistency
        try:
            out = subprocess.run(["git", "hash-object", local_path], stdout=subprocess.PIPE,
                                  stderr=subprocess.PIPE, timeout=5, text=True)
        except Exception as e:
            details.append("%s: could not run git hash-object (%s)" % (skill["name"], e))
            continue
        if out.returncode != 0:
            details.append("%s: git hash-object failed" % skill["name"])
            continue
        local_sha = out.stdout.strip()
        if local_sha != skill.get("upstream_blob_sha"):
            details.append("%s: local blob %s != manifest upstream_blob_sha %s" % (
                skill["name"], local_sha, skill.get("upstream_blob_sha")))
        if upstream_dir:
            upstream_path = os.path.join(upstream_dir, skill["upstream_path"], "SKILL.md")
            if os.path.isfile(upstream_path):
                out2 = subprocess.run(["git", "hash-object", upstream_path], stdout=subprocess.PIPE,
                                       stderr=subprocess.PIPE, timeout=5, text=True)
                if out2.returncode == 0 and out2.stdout.strip() != local_sha:
                    details.append("%s: local blob differs from %s" % (skill["name"], upstream_path))
    label = "blob-hash-verified-against-upstream-clone" if upstream_dir else "blob-hash-verified-locally"
    report(label, not details, details)


def check_patch_ledger_coverage():
    manifest = load_manifest()
    with open(PATCHES_PATH, encoding="utf-8") as f:
        ledger_text = f.read()
    details = []
    for skill in manifest["skills"]:
        if not skill.get("locally_modified"):
            continue
        if skill["name"] not in ledger_text:
            details.append("%s: locally_modified=true but not mentioned in patches ledger" % skill["name"])
        for patch_id in skill.get("patch_ids", []):
            if patch_id not in ledger_text:
                details.append("%s: patch_id %r not found in patches ledger" % (skill["name"], patch_id))
    report("patch-ledger-covers-modified-skills", not details, details)


def check_anthropic_file_hashes():
    """anthropic branch (file-level, unlike mattpocock's skill-level check_blob_hashes above): for
    every file in every skill's files[], the file must exist; an unmodified upstream-origin file's
    on-disk git hash-object must equal its recorded upstream_blob_sha; a locally-modified file must
    have a non-empty patch_ids; a local-origin file must carry a reason. ALSO, for every entry
    regardless of origin/modification status, the on-disk git hash-object must equal the manifest's
    recorded vendored_blob_sha -- this is the integrity pin that covers patched and local-origin
    files too (upstream_blob_sha alone only pins the unmodified ones). A vendored_blob_sha mismatch
    on a script whose skill has scripts_audited=true means the file changed since it was last read
    end-to-end; re-audit (not just a manifest hash bump) is required before trusting it again. Also
    walks each anthropic skill's directory on disk and fails if any file there isn't listed in
    files[] at all (an unreviewed file with no manifest entry is exactly the kind of drift this
    check exists to catch)."""
    manifest = load_anthropic_manifest()
    details = []
    upstream_dir = os.environ.get("GOVERNANCE_UPSTREAM_DIR_ANTHROPIC")
    for skill in manifest["skills"]:
        listed_paths = set()
        for entry in skill.get("files", []):
            rel_path = entry["path"]
            listed_paths.add(rel_path)
            local_path = os.path.join(REPO_ROOT, rel_path)
            if not os.path.isfile(local_path):
                details.append("%s: %s: listed in files[] but missing on disk" % (skill["name"], rel_path))
                continue

            try:
                out = subprocess.run(["git", "hash-object", local_path], stdout=subprocess.PIPE,
                                      stderr=subprocess.PIPE, timeout=5, text=True)
            except Exception as e:
                details.append("%s: %s: could not run git hash-object (%s)" % (skill["name"], rel_path, e))
                continue
            if out.returncode != 0:
                details.append("%s: %s: git hash-object failed" % (skill["name"], rel_path))
                continue
            local_sha = out.stdout.strip()

            # Integrity pin, ALL entries regardless of origin/locally_modified: on-disk content
            # must equal the manifest's own recorded vendored_blob_sha.
            vendored_sha = entry.get("vendored_blob_sha")
            if not vendored_sha:
                details.append("%s: %s: missing vendored_blob_sha in manifest" % (skill["name"], rel_path))
            elif local_sha != vendored_sha:
                details.append("%s: %s: local blob %s != manifest vendored_blob_sha %s"
                                " (if this file's skill has scripts_audited=true, re-audit is required,"
                                " not just re-hashing)" % (
                                    skill["name"], rel_path, local_sha, vendored_sha))

            origin = entry.get("origin")
            if origin == "local":
                if not entry.get("reason"):
                    details.append("%s: %s: origin=local but no 'reason' given" % (skill["name"], rel_path))
                continue

            if entry.get("locally_modified"):
                if not entry.get("patch_ids"):
                    details.append("%s: %s: locally_modified=true but patch_ids is empty" % (skill["name"], rel_path))
                continue

            # origin == "upstream" and not locally_modified: also verify byte-for-byte against the
            # recorded upstream_blob_sha (unmodified files must match BOTH upstream_blob_sha and
            # vendored_blob_sha, which should themselves be equal to each other).
            if local_sha != entry.get("upstream_blob_sha"):
                details.append("%s: %s: local blob %s != manifest upstream_blob_sha %s"
                                " (if this file's skill has scripts_audited=true, re-audit is required,"
                                " not just re-hashing)" % (
                                    skill["name"], rel_path, local_sha, entry.get("upstream_blob_sha")))
            if upstream_dir and entry.get("upstream_path"):
                upstream_path = os.path.join(upstream_dir, entry["upstream_path"])
                if os.path.isfile(upstream_path):
                    out2 = subprocess.run(["git", "hash-object", upstream_path], stdout=subprocess.PIPE,
                                           stderr=subprocess.PIPE, timeout=5, text=True)
                    if out2.returncode == 0 and out2.stdout.strip() != local_sha:
                        details.append("%s: %s: local blob differs from %s" % (
                            skill["name"], rel_path, upstream_path))

        # Any on-disk file under this skill's directory that files[] doesn't mention at all.
        skill_dir = os.path.join(REPO_ROOT, skill["path"])
        if os.path.isdir(skill_dir):
            for dirpath, dirnames, filenames in os.walk(skill_dir):
                # Running a vendored skill's own scripts (e.g. `python -m scripts.aggregate_benchmark`)
                # creates __pycache__/*.pyc inside the skill dir; .gitignore covers __pycache__, so
                # these can never be committed and are not supply-chain drift. Skip narrowly by name
                # only (no git check-ignore shell-out, no broader pattern) so this stays fail-closed
                # for everything actually committable.
                dirnames[:] = [d for d in dirnames if d != "__pycache__"]
                for fname in filenames:
                    if fname.endswith(".pyc"):
                        continue
                    full = os.path.join(dirpath, fname)
                    rel_path = rel(full)
                    if rel_path not in listed_paths:
                        details.append("%s: %s: present on disk but not listed in files[]" % (
                            skill["name"], rel_path))

    label = "anthropic-file-hashes-verified-against-upstream-clone" if upstream_dir else "anthropic-file-hashes-verified-locally"
    report(label, not details, details)


def check_anthropic_vendored_modes():
    """anthropic files[]: every on-disk file must have no executable bit set, and its recorded
    vendored_mode must be "100644" -- these are vendored docs/scripts read by an agent, never
    executed as a standalone binary/shebang-invoked file directly off disk."""
    manifest = load_anthropic_manifest()
    details = []
    for skill in manifest["skills"]:
        for entry in skill.get("files", []):
            rel_path = entry["path"]
            local_path = os.path.join(REPO_ROOT, rel_path)
            if entry.get("vendored_mode") != "100644":
                details.append("%s: %s: manifest vendored_mode %r != \"100644\"" % (
                    skill["name"], rel_path, entry.get("vendored_mode")))
            if not os.path.isfile(local_path):
                continue  # already reported by check_anthropic_file_hashes
            st = os.stat(local_path)
            if st.st_mode & 0o111:
                details.append("%s: %s: executable bit set on disk" % (skill["name"], rel_path))
    report("anthropic-vendored-modes", not details, details)


def check_anthropic_scripts_audited():
    """Any anthropic skill whose files[] contains a .py/.html/.sh file must have scripts_audited
    true -- prose-only skills (frontend-design) are exempt since there's nothing to audit."""
    manifest = load_anthropic_manifest()
    details = []
    script_exts = (".py", ".html", ".sh")
    for skill in manifest["skills"]:
        has_script = any(entry["path"].endswith(script_exts) for entry in skill.get("files", []))
        if has_script and not skill.get("scripts_audited"):
            details.append("%s: ships a .py/.html/.sh file but scripts_audited is not true" % skill["name"])
    report("anthropic-scripts-audited", not details, details)


def check_builtin_collision_denylist():
    """No vendored or first-party skill directory name and no manifest skill name (across any of
    the three ownership sources) may collide with a name Claude Code (or another first-party
    surface) already provides -- see BUILTIN_SKILL_NAMES above. This is the general form of the
    specific problem the skc-rename-vendored patch fixed for skill-creator. The project manifest
    is checked directly (project_skill_names()), never via the vendored MANIFESTS registry."""
    details = []
    for name in list_skill_dirs():
        if name in BUILTIN_SKILL_NAMES:
            details.append("skill dir name collides with a builtin: %s" % name)
    for entry in MANIFESTS:
        manifest = load_manifest(entry["manifest"])
        for s in manifest["skills"]:
            if s["name"] in BUILTIN_SKILL_NAMES:
                details.append("%s: manifest skill name collides with a builtin: %s" % (entry["label"], s["name"]))
    for name in sorted(project_skill_names()):
        if name in BUILTIN_SKILL_NAMES:
            details.append("project: manifest skill name collides with a builtin: %s" % name)
    report("builtin-collision-denylist", not details, details)


# Patch-marker convention (deterministic, documented here because this is the
# only place it's enforced):
#   - A WRAPPING marker pair is exactly `<!-- bukerov-local-patch: <id> -->`
#     ... `<!-- /bukerov-local-patch: <id> -->` — nothing else inside the
#     opening comment besides the id. Every opening tag for a given <id> in a
#     given file must be matched by an equal number of closing tags for that
#     same <id> in that file.
#   - A SELF-CLOSING annotation is a single comment of the form
#     `<!-- bukerov-local-patch: <id> — <free text> -->` — an em dash (—)
#     followed by free text before the closing `-->`. It stands alone (e.g. a
#     one-line note about a frontmatter change with nothing to wrap) and is
#     NOT required to have a matching `<!-- /bukerov-local-patch: ... -->`.
#     The em dash is what distinguishes it from a wrapping opener: a wrapping
#     opener's regex requires only whitespace between the id and `-->`, so the
#     two forms can never be confused by construction.
#   - Python files use a different, inherently single-line form instead of the HTML comment pair:
#     `# bukerov-local-patch: <id> — <note>`. There is no wrapping/closing counterpart in Python
#     (a change region is usually a whole function or block, not cleanly bracketable by a
#     standalone comment line the way HTML/Markdown text is), so PY_MARK_RE matches are collected
#     for coverage purposes only -- they are never subject to an open/close balance check.
PATCH_OPEN_RE = re.compile(r"<!--\s*bukerov-local-patch:\s*([\w-]+)\s*-->")
PATCH_CLOSE_RE = re.compile(r"<!--\s*/bukerov-local-patch:\s*([\w-]+)\s*-->")
PATCH_SELFCLOSING_RE = re.compile(r"<!--\s*bukerov-local-patch:\s*([\w-]+)\s*—[^>]*-->")
PY_MARK_RE = re.compile(r"#\s*bukerov-local-patch:\s*([\w-]+)")


def check_patch_marker_balance():
    """Scans .md files anywhere under .claude/skills/ (unchanged from before the anthropic set was
    added) plus .html files (same wrapping-marker convention as .md, since both use HTML comments)
    for balanced open/close patch markers. .py files are also scanned (PY_MARK_RE) but only for the
    informational count -- Python's single-line marker form has no closing counterpart to balance."""
    details = []
    selfclosing_count = 0
    py_mark_count = 0
    if not os.path.isdir(SKILLS_DIR):
        report("patch-marker-balance", False, ["missing .claude/skills/"])
        return
    for dirpath, _, filenames in os.walk(SKILLS_DIR):
        for fname in filenames:
            path = os.path.join(dirpath, fname)
            if fname.endswith(".py"):
                with open(path, encoding="utf-8") as f:
                    text = f.read()
                py_mark_count += len(PY_MARK_RE.findall(text))
                continue
            if not (fname.endswith(".md") or fname.endswith(".html")):
                continue
            with open(path, encoding="utf-8") as f:
                text = f.read()
            selfclosing_count += len(PATCH_SELFCLOSING_RE.findall(text))
            opens, closes = {}, {}
            for m in PATCH_OPEN_RE.finditer(text):
                opens[m.group(1)] = opens.get(m.group(1), 0) + 1
            for m in PATCH_CLOSE_RE.finditer(text):
                closes[m.group(1)] = closes.get(m.group(1), 0) + 1
            for pid in sorted(set(opens) | set(closes)):
                o, c = opens.get(pid, 0), closes.get(pid, 0)
                if o != c:
                    details.append("%s: patch-id %r open=%d close=%d (unbalanced wrapping markers)" % (
                        rel(path), pid, o, c))
    print("  (info) %d self-closing single-line patch annotations found (em-dash form, not paired)" %
          selfclosing_count)
    print("  (info) %d Python # bukerov-local-patch markers found (no balance concept)" % py_mark_count)
    report("patch-marker-balance", not details, details)


def find_all_patch_marker_ids(root_dir):
    """Collect every distinct patch id referenced by ANY marker form (.md/.html wrapping open tags
    and self-closing annotations, .py single-line markers) anywhere under root_dir."""
    ids = set()
    for dirpath, _, filenames in os.walk(root_dir):
        for fname in filenames:
            path = os.path.join(dirpath, fname)
            if fname.endswith(".py"):
                with open(path, encoding="utf-8") as f:
                    text = f.read()
                ids.update(PY_MARK_RE.findall(text))
            elif fname.endswith(".md") or fname.endswith(".html"):
                with open(path, encoding="utf-8") as f:
                    text = f.read()
                ids.update(PATCH_OPEN_RE.findall(text))
                ids.update(PATCH_SELFCLOSING_RE.findall(text))
    return ids


def check_anthropic_patch_coverage():
    """Bidirectional coverage check for the anthropic skill set specifically (the mattpocock set's
    equivalent is check_patch_ledger_coverage above, which is skill-level and unchanged):
      - every marker id found anywhere in an anthropic-owned skill directory must appear in the
        anthropic ledger (anthropic-skills-patches.md) AND in some file's files[].patch_ids;
      - every files[].patch_ids id (for every file, in every anthropic skill) must appear in the
        ledger -- a manifest entry claiming a patch id the ledger never documents is exactly the
        kind of drift a ledger is supposed to make impossible."""
    manifest = load_anthropic_manifest()
    with open(ANTHROPIC_PATCHES_PATH, encoding="utf-8") as f:
        ledger_text = f.read()

    manifest_patch_ids = set()
    for skill in manifest["skills"]:
        for entry in skill.get("files", []):
            manifest_patch_ids.update(entry.get("patch_ids", []))

    details = []
    for skill in manifest["skills"]:
        skill_dir = os.path.join(REPO_ROOT, skill["path"])
        if not os.path.isdir(skill_dir):
            continue  # already reported elsewhere
        found_ids = find_all_patch_marker_ids(skill_dir)
        for pid in sorted(found_ids):
            if pid not in ledger_text:
                details.append("%s: marker id %r found in-file but missing from anthropic ledger" % (
                    skill["name"], pid))
            if pid not in manifest_patch_ids:
                details.append("%s: marker id %r found in-file but not recorded in any files[].patch_ids" % (
                    skill["name"], pid))

    for pid in sorted(manifest_patch_ids):
        if pid not in ledger_text:
            details.append("manifest patch_id %r not found in anthropic ledger" % pid)

    report("anthropic-patch-marker-coverage", not details, details)


def check_application_paths_untouched():
    base_sha = os.environ.get("GOVERNANCE_BASE_SHA")
    details = []
    try:
        if base_sha:
            out = subprocess.run(["git", "-C", REPO_ROOT, "diff", "--name-only", base_sha],
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10, text=True)
        else:
            out = subprocess.run(["git", "-C", REPO_ROOT, "status", "--porcelain"],
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10, text=True)
    except Exception as e:
        report("application-paths-untouched", False, ["could not run git: %s" % e])
        return
    if out.returncode != 0:
        report("application-paths-untouched", False, ["git command failed: %s" % out.stderr.strip()])
        return
    lines = out.stdout.splitlines()
    if not base_sha:
        # `git status --porcelain` lines are "XY path"; take the path portion.
        lines = [ln[3:] if len(ln) > 3 else ln for ln in lines]
    for path in lines:
        path = path.strip()
        if not path:
            continue
        if path.startswith(APPLICATION_PATH_PREFIXES) or path.startswith(APPLICATION_PATH_EXACT_PREFIXES):
            details.append("touched application path: %s" % path)
    report("application-paths-untouched", not details, details)


def check_settings_schema():
    details = []
    try:
        with open(SETTINGS_PATH) as f:
            settings = json.load(f)
    except Exception as e:
        report("settings-json-schema-sanity", False, ["could not parse: %s" % e])
        return
    perms = settings.get("permissions", {})
    for key in ("deny", "ask"):
        val = perms.get(key, [])
        if not isinstance(val, list) or not all(isinstance(x, str) for x in val):
            details.append("permissions.%s must be an array of strings" % key)
    hooks = settings.get("hooks", {})
    pre = hooks.get("PreToolUse", [])
    if not isinstance(pre, list):
        details.append("hooks.PreToolUse must be an array")
    else:
        for entry in pre:
            if not isinstance(entry, dict) or "matcher" not in entry or "hooks" not in entry:
                details.append("hooks.PreToolUse entry missing matcher/hooks: %r" % (entry,))
                continue
            for h in entry.get("hooks", []):
                if not isinstance(h, dict) or h.get("type") != "command" or "command" not in h:
                    details.append("hooks.PreToolUse hook entry malformed: %r" % (h,))
    report("settings-json-schema-sanity", not details, details)


def check_settings_mcp_mutations_denied():
    """Regression guard for the final-audit F-M1 fix: server-side GitHub write
    tools must be denied outright (they bypass the local branch gate), and
    update_pull_request must at least be gated to ask (a draft->ready flip is a
    non-delegable action that must never happen autonomously)."""
    details = []
    try:
        with open(SETTINGS_PATH) as f:
            settings = json.load(f)
    except Exception as e:
        report("settings-mcp-mutations-denied", False, ["could not parse: %s" % e])
        return
    perms = settings.get("permissions", {})
    deny = set(perms.get("deny", []))
    ask = set(perms.get("ask", []))
    must_deny = [
        "mcp__github__merge_pull_request",
        "mcp__github__enable_pr_auto_merge",
        "mcp__github__actions_run_trigger",
        "mcp__github__push_files",
        "mcp__github__create_or_update_file",
        "mcp__github__delete_file",
    ]
    for name in must_deny:
        if name not in deny:
            details.append("%s must be in permissions.deny" % name)
    if "mcp__github__update_pull_request" not in (deny | ask):
        details.append("mcp__github__update_pull_request must be denied or gated to ask "
                       "(draft->ready flip is non-delegable)")
    report("settings-mcp-mutations-denied", not details, details)


def check_rules_frontmatter_and_uniqueness():
    details = []
    if not os.path.isdir(RULES_DIR):
        report("rules-frontmatter-and-uniqueness", False, ["missing .claude/rules/"])
        return
    seen = set()
    for fname in sorted(os.listdir(RULES_DIR)):
        if not fname.endswith(".md"):
            continue
        if fname in seen:
            details.append("duplicate rule filename: %s" % fname)
        seen.add(fname)
        path = os.path.join(RULES_DIR, fname)
        with open(path, encoding="utf-8") as f:
            lines = f.readlines()
        if not lines or lines[0].strip() != "---":
            details.append("rules/%s: missing frontmatter fence" % fname)
            continue
        try:
            end = next(i for i in range(1, len(lines)) if lines[i].strip() == "---")
        except StopIteration:
            details.append("rules/%s: unterminated frontmatter fence" % fname)
            continue
        block = "".join(lines[1:end])
        if "paths:" not in block:
            details.append("rules/%s: frontmatter has no 'paths:' key" % fname)
    report("rules-frontmatter-and-uniqueness", not details, details)


def check_hidden_unicode():
    details = []
    scan_roots = [CLAUDE_DIR, DOCS_AGENTS_DIR]
    for root in scan_roots:
        if not os.path.isdir(root):
            continue
        for dirpath, _, filenames in os.walk(root):
            for fname in filenames:
                path = os.path.join(dirpath, fname)
                try:
                    with open(path, encoding="utf-8") as f:
                        text = f.read()
                except (UnicodeDecodeError, OSError):
                    continue
                for ch in text:
                    if ch in HIDDEN_UNICODE:
                        details.append("%s: hidden/bidi character U+%04X" % (rel(path), ord(ch)))
                        break
    report("no-hidden-unicode", not details, details)


def check_hook_self_test():
    try:
        proc = subprocess.run([sys.executable, HOOK_PATH, "--self-test"], stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, timeout=30, text=True)
    except Exception as e:
        report("hook-self-test", False, ["could not run hook: %s" % e])
        return
    ok = proc.returncode == 0
    details = [] if ok else [proc.stdout.strip().splitlines()[-1] if proc.stdout.strip() else "no output"]
    report("hook-self-test", ok, details)


# --------------------------------------------------------------------------
# Project (first-party) manifest validation
#
# docs/agents/project-skills-manifest.json is validated by validate_project_manifest(), a PURE
# function (no reporting side effects) so the exact same code path can be driven against synthetic
# fixture trees by --self-test AND against the real repo by check_project_manifest() below --
# never two parallel implementations of the same rules.
# --------------------------------------------------------------------------

ALLOWED_PROJECT_MANIFEST_TOP_KEYS = {"schema_version", "ownership_class", "notes", "skills"}
REQUIRED_PROJECT_MANIFEST_TOP_KEYS = {"schema_version", "ownership_class", "skills"}
ALLOWED_PROJECT_ENTRY_KEYS = {
    "name", "path", "origin", "invocation", "mutation_capability", "scripts", "hooks",
    "files", "eval_evidence", "review_status", "reviewed_base_sha", "reviewed_at",
}
ALLOWED_PROJECT_FILE_KEYS = {"path", "blob_sha", "mode"}
ALLOWED_PROJECT_EVAL_EVIDENCE_KEYS = {"path", "blob_sha"}

PROJECT_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9-]*$")
PROJECT_BLOB_SHA_RE = re.compile(r"^[0-9a-f]{40}$")
PROJECT_REVIEWED_AT_RE = re.compile(r"^\d{4}-\d{2}-\d{2}$")
# .ts/.bash/.zsh/.ps1 added on top of the original extension set; a leading "#!" shebang (checked
# separately, see _has_shebang()) also marks a file script-shaped regardless of extension.
PROJECT_SCRIPT_EXTS = (".py", ".sh", ".js", ".mjs", ".rb", ".pl", ".ts", ".bash", ".zsh", ".ps1")
GLOB_METACHARS = frozenset("*?[")


def _git_hash_object(path, cwd):
    """Runs `git hash-object <path>` with the given cwd and returns (sha_or_None, error_or_None).
    Works outside a git repository (it is a plumbing command with no repo dependency), which is
    what lets this be used against synthetic --self-test fixture trees as well as the real repo."""
    try:
        out = subprocess.run(["git", "hash-object", path], stdout=subprocess.PIPE,
                              stderr=subprocess.PIPE, timeout=5, text=True, cwd=cwd)
    except Exception as e:
        return None, "could not run git hash-object (%s)" % e
    if out.returncode != 0:
        return None, "git hash-object failed: %s" % out.stderr.strip()
    return out.stdout.strip(), None


def _has_glob_metachars(s):
    return any(c in GLOB_METACHARS for c in s)


def _has_shebang(path):
    """Returns (is_shebang: bool, error: str_or_None). A file that can't be read is reported as an
    error (surfaced by the caller as its own detail), never silently treated as "not a script" --
    an unreadable file is exactly the kind of thing that deserves a loud diagnostic, not a skip."""
    try:
        with open(path, "rb") as f:
            head = f.read(2)
        return head == b"#!", None
    except OSError as e:
        return False, str(e)


def _safe(value):
    """Returns an injection-safe rendering of a manifest- or filesystem-derived string for
    embedding in a printed detail. report() prints every detail verbatim to stdout alongside this
    script's own "[PASS] check-name" / "[FAIL] check-name" lines; a crafted value containing a
    newline (a manifest `name` that never matched PROJECT_NAME_RE, or -- in principle, since most
    filesystems allow it -- an on-disk filename) could otherwise forge extra stdout lines,
    including a fake "[PASS] ..." line. Values already free of control characters are returned
    unchanged (so ordinary names/paths keep reading naturally in messages); anything else is
    repr()'d, which escapes \\n/\\r/\\t and every other control character."""
    if not isinstance(value, str):
        return value
    if any(ord(c) < 0x20 or ord(c) == 0x7f for c in value):
        return repr(value)
    return value


def _validate_repo_relative_subpath(value, prefix_dir):
    """Shared shape validation for a manifest-declared path that must live under prefix_dir
    (already ending in '/'), or None if there is no known prefix to check against yet. Returns a
    list of violation reason strings (empty = valid shape). Used identically for files[].path,
    scripts[] entries, and hooks[] entries so none of the three can drift from the others."""
    reasons = []
    if not isinstance(value, str):
        reasons.append("must be a string")
        return reasons
    if ".." in value:
        reasons.append("must not contain '..'")
    if value.startswith("/"):
        reasons.append("must not be absolute")
    if _has_glob_metachars(value):
        reasons.append("must not contain glob metacharacters")
    if not reasons and prefix_dir is not None and not value.startswith(prefix_dir):
        reasons.append("must be under the entry's skill dir %r" % (prefix_dir,))
    return reasons


def _find_upstream_keys(obj, path=""):
    """Recursively collects every dict key containing the substring "upstream" (case-insensitive)
    anywhere inside obj (dicts and lists), returning dotted/bracketed paths like
    "files[0].upstream_blob_sha" / "files[0].Upstream_Blob_Sha". A first-party manifest entry must
    never carry one of these -- belt and braces on top of the strict per-level allowed-key checks,
    since an upstream* key could otherwise hide inside a nested dict that isn't itself
    schema-checked (there is no such nesting today, but the check is defensive against one being
    added without an accompanying schema update)."""
    found = []
    if isinstance(obj, dict):
        for k, v in obj.items():
            here = ("%s.%s" % (path, k)) if path else k
            if isinstance(k, str) and "upstream" in k.lower():
                found.append(here)
            found.extend(_find_upstream_keys(v, here))
    elif isinstance(obj, list):
        for i, item in enumerate(obj):
            found.extend(_find_upstream_keys(item, "%s[%d]" % (path, i)))
    return found


def validate_project_manifest(manifest, repo_root, require_tracked=True):
    """Pure validation core for docs/agents/project-skills-manifest.json's schema (see
    docs/agents/project-skills-policy.md). Returns a list of distinct, stable diagnostic strings
    -- empty means valid. Never reports or prints; callers (check_project_manifest() and the
    --self-test fixtures) decide what to do with the result.

    require_tracked gates ONLY the git-ls-files "is eval_evidence.path actually tracked" check --
    every path-SHAPE rule (no ".scratch/", no "tmp/", no URL, not absolute, no "..") is a pure
    string check and is ALWAYS enforced regardless of this flag. Production always calls this with
    require_tracked=True; --self-test fixture trees that aren't real git repos pass False so the
    (deliberately environment-dependent) tracked-ness check doesn't fail fixtures that aren't
    testing that specific rule.

    An entry's own `path` is validated for SHAPE before it is ever used to build a filesystem path
    -- skill_dir stays None (no directory-existence check, no directory walk) unless the shape
    check passed, so a traversal ("..") or absolute ("/...") `path` can never be used to actually
    walk outside repo_root or across the whole filesystem. For an invalid `path`, the shape
    diagnostic is the only diagnostic this function ever emits for that entry's directory.
    """
    details = []
    if not isinstance(manifest, dict):
        return ["top-level manifest must be a JSON object"]

    top_keys = set(manifest.keys())
    unknown_top = top_keys - ALLOWED_PROJECT_MANIFEST_TOP_KEYS
    if unknown_top:
        details.append("unknown top-level key(s): %s" % sorted(unknown_top))
    missing_top = REQUIRED_PROJECT_MANIFEST_TOP_KEYS - top_keys
    if missing_top:
        details.append("missing required top-level key(s): %s" % sorted(missing_top))

    schema_version = manifest.get("schema_version")
    if isinstance(schema_version, bool) or schema_version != 1:
        details.append("schema_version must be the integer 1, got %r" % (schema_version,))

    ownership_class = manifest.get("ownership_class")
    if ownership_class != PROJECT_OWNERSHIP_CLASS:
        details.append("ownership_class must equal %r, got %r" % (PROJECT_OWNERSHIP_CLASS, ownership_class))

    # notes is optional, but when present must be a string -- this closes off a "notes as a dict"
    # blind spot: _find_upstream_keys() above is only ever called on entry objects inside
    # skills[], never on the top-level manifest, so a dict-shaped `notes` could otherwise carry an
    # upstream_* key (or anything else) with no schema check ever looking at it.
    if "notes" in manifest and not isinstance(manifest.get("notes"), str):
        details.append("notes must be a string when present, got %s" % type(manifest.get("notes")).__name__)

    skills = manifest.get("skills")
    if not isinstance(skills, list):
        details.append("skills must be a list")
        return details  # nothing further to validate without a list of entries

    # Intra-manifest duplicate detection: two entries in THIS manifest claiming the same `name` or
    # the same `path` is a violation on its own, independent of the repo-wide cross-source
    # unique-skill-names check (check_unique_skill_names) -- this function is exercised standalone
    # (by --self-test, and conceptually by anyone hand-validating a draft manifest) against
    # manifests that were never routed through that check.
    entry_names = [e.get("name") for e in skills if isinstance(e, dict) and isinstance(e.get("name"), str)]
    for n in sorted({n for n in entry_names if entry_names.count(n) > 1}):
        details.append("duplicate skill name within manifest: %s" % _safe(n))
    entry_paths = [e.get("path") for e in skills if isinstance(e, dict) and isinstance(e.get("path"), str)]
    for p in sorted({p for p in entry_paths if entry_paths.count(p) > 1}):
        details.append("duplicate skill path within manifest: %s" % _safe(p))

    for idx, entry in enumerate(skills):
        prefix = "skills[%d]" % idx
        if not isinstance(entry, dict):
            details.append("%s: entry must be an object" % prefix)
            continue

        entry_keys = set(entry.keys())
        unknown_keys = entry_keys - ALLOWED_PROJECT_ENTRY_KEYS
        if unknown_keys:
            details.append("%s: unknown key(s): %s" % (prefix, sorted(unknown_keys)))

        upstream_hits = _find_upstream_keys(entry)
        if upstream_hits:
            details.append(
                "%s: first-party entries must not claim upstream provenance (key path(s): %s)" % (
                    prefix, sorted(upstream_hits)))

        name = entry.get("name")
        label = _safe(name) if isinstance(name, str) and name else prefix
        if not (isinstance(name, str) and PROJECT_NAME_RE.match(name)):
            details.append("%s: name must match ^[a-z0-9][a-z0-9-]*$, got %r" % (label, name))

        path = entry.get("path")
        expected_path = (".claude/skills/" + name) if isinstance(name, str) else None
        valid_path = False
        if not isinstance(path, str):
            details.append("%s: path must be a string" % label)
        else:
            path_ok = True
            if ".." in path:
                details.append("%s: path must not contain '..'" % label)
                path_ok = False
            if path.startswith("/"):
                details.append("%s: path must not be absolute" % label)
                path_ok = False
            if expected_path is not None and path != expected_path:
                details.append("%s: path must equal %r, got %r" % (label, expected_path, path))
                path_ok = False
            valid_path = path_ok

        origin = entry.get("origin")
        if origin != "project":
            details.append("%s: origin must equal \"project\", got %r" % (label, origin))

        invocation = entry.get("invocation")
        if invocation not in ("model+user", "user-only"):
            details.append("%s: invocation must be \"model+user\" or \"user-only\", got %r" % (label, invocation))

        mutation_capability = entry.get("mutation_capability")
        scripts = entry.get("scripts")
        hooks = entry.get("hooks")
        # mutation_capability is REVIEWED METADATA / review evidence only -- it is not a
        # mechanical capability boundary. Actual mutation authority comes solely from the
        # operation modes (GOVERNANCE_V3.md section 4) and an active task contract (see
        # project-skills-policy.md); the "read-only requires empty scripts/hooks" rule below is a
        # deterministic proxy the validator can check, not the thing that grants or denies write
        # access.
        if mutation_capability not in ("read-only", "mutation-capable"):
            details.append("%s: mutation_capability must be \"read-only\" or \"mutation-capable\", got %r" % (
                label, mutation_capability))
        elif mutation_capability == "read-only":
            if scripts != []:
                details.append("%s: mutation_capability read-only requires scripts == []" % label)
            if hooks != []:
                details.append("%s: mutation_capability read-only requires hooks == []" % label)

        if not isinstance(scripts, list):
            details.append("%s: scripts must be a list" % label)
        if not isinstance(hooks, list):
            details.append("%s: hooks must be a list" % label)

        # skill_dir is None unless `path` passed shape validation above -- an invalid path (e.g.
        # traversal, or absolute so os.path.join would discard repo_root and walk the whole
        # filesystem) is NEVER used to build a filesystem path or drive a directory walk.
        skill_dir = os.path.join(repo_root, path) if valid_path else None
        prefix_dir = (path + "/") if valid_path else None
        hooks_prefix = (path + "/hooks/") if valid_path else None

        # scripts[]/hooks[] get the SAME shape validation as files[].path (no absolute, no "..",
        # must live under the entry's own skill dir, no glob metachars); hooks[] additionally must
        # live under "<skill>/hooks/" specifically. Only shape-valid, correctly-placed entries
        # make it into declared_scripts/declared_hooks -- these are what the directory walk and
        # the "declared but not in files[]" checks below compare against.
        declared_scripts = []
        for p in (scripts if isinstance(scripts, list) else []):
            reasons = _validate_repo_relative_subpath(p, prefix_dir)
            if reasons:
                details.append("%s: scripts entry %s: %s" % (label, _safe(p) if isinstance(p, str) else repr(p),
                                                               "; ".join(reasons)))
            else:
                declared_scripts.append(p)

        declared_hooks = []
        for p in (hooks if isinstance(hooks, list) else []):
            reasons = _validate_repo_relative_subpath(p, prefix_dir)
            if reasons:
                details.append("%s: hooks entry %s: %s" % (label, _safe(p) if isinstance(p, str) else repr(p),
                                                             "; ".join(reasons)))
            elif hooks_prefix is not None and not p.startswith(hooks_prefix):
                details.append("%s: hooks entry %s must live under %r" % (label, _safe(p), hooks_prefix))
            else:
                declared_hooks.append(p)

        files = entry.get("files")
        if not isinstance(files, list) or not files:
            details.append("%s: files must be a non-empty list" % label)
            files = files if isinstance(files, list) else []

        listed_paths = set()
        for fidx, f in enumerate(files):
            fprefix = "%s.files[%d]" % (label, fidx)
            if not isinstance(f, dict):
                details.append("%s: file entry must be an object" % fprefix)
                continue
            fkeys = set(f.keys())
            unknown_fkeys = fkeys - ALLOWED_PROJECT_FILE_KEYS
            if unknown_fkeys:
                details.append("%s: unknown key(s): %s" % (fprefix, sorted(unknown_fkeys)))

            fpath = f.get("path")
            fsha = f.get("blob_sha")
            fmode = f.get("mode")
            fpath_reasons = _validate_repo_relative_subpath(fpath, prefix_dir)
            valid_fpath = not fpath_reasons
            for reason in fpath_reasons:
                details.append("%s: path %s" % (fprefix, reason))
            # Only a shape-VALID path is added to listed_paths -- a rejected files[].path (e.g. a
            # traversal path) must never be able to satisfy scripts[]/hooks[] membership or the
            # SKILL.md-is-listed check just because the same bad string happens to appear twice.
            if valid_fpath:
                listed_paths.add(fpath)

            if not (isinstance(fsha, str) and PROJECT_BLOB_SHA_RE.match(fsha)):
                details.append("%s: blob_sha must be 40-hex, got %r" % (fprefix, fsha))
            if fmode != "100644":
                details.append("%s: mode must be \"100644\", got %r" % (fprefix, fmode))

            if valid_fpath:
                local_path = os.path.join(repo_root, fpath)
                if os.path.islink(local_path):
                    details.append("%s: declared file must not be a symlink: %s" % (fprefix, _safe(fpath)))
                elif not os.path.isfile(local_path):
                    details.append("%s: declared file does not exist on disk: %s" % (fprefix, _safe(fpath)))
                else:
                    st = os.stat(local_path)
                    if st.st_mode & 0o111:
                        details.append("%s: declared file has an executable bit set: %s" % (fprefix, _safe(fpath)))
                    local_sha, err = _git_hash_object(local_path, repo_root)
                    if err:
                        details.append("%s: %s" % (fprefix, err))
                    elif isinstance(fsha, str) and local_sha != fsha:
                        details.append("%s: on-disk blob %s != declared blob_sha %s" % (
                            fprefix, local_sha, fsha))

        # Directory walk: only ever attempted when `path` passed shape validation AND the
        # resulting directory actually exists -- see the module-level note above on why an
        # invalid path never reaches here. The skill_dir ROOT itself must not be a symlink,
        # checked and rejected before os.path.isdir() (which follows links) ever runs: the
        # os.path.islink() guard further down (:1369 in the pre-fix file) only covers CHILD
        # dirnames inside an already-descended-into skill_dir, so a symlinked TOP would otherwise
        # be walked in full by this standalone entry point -- a full-repo run is saved only by the
        # separate no-symlinks-no-exec-under-claude check, which is a weaker contract than what
        # this function documents for itself.
        if skill_dir is not None and os.path.islink(skill_dir):
            details.append("%s: skill directory must not be a symlink: %s" % (label, _safe(path)))
        elif skill_dir is not None and os.path.isdir(skill_dir):
            skillmd_path = os.path.join(skill_dir, "SKILL.md")
            skillmd_rel = path + "/SKILL.md"
            if not os.path.isfile(skillmd_path):
                details.append("%s: skill dir has no SKILL.md" % label)
            elif skillmd_rel not in listed_paths:
                details.append("%s: SKILL.md must be listed in files[]" % label)

            # review_status must be "approved" once the skill directory actually has on-disk
            # content -- "draft" is schema-valid only for a staged entry with no on-disk dir yet
            # (a manifest entry naming a directory that doesn't exist is independently flagged as
            # a phantom entry by check_manifest_ownership_partition/manifest-filesystem-
            # consistency at the repo-wide level, and this function's own "entry with no on-disk
            # dir" diagnostic below covers the same case when called standalone).
            if entry.get("review_status") == "draft":
                details.append(
                    "%s: review_status must be \"approved\" once the skill directory exists on "
                    "disk (\"draft\" is only valid for a staged entry with no on-disk content "
                    "yet)" % label)

            def _walk_onerror(err, _label=label):
                details.append("%s: error walking skill dir: %s" % (_label, err))

            for dirpath, dirnames, filenames in os.walk(skill_dir, onerror=_walk_onerror):
                kept_dirnames = []
                for d in sorted(dirnames):
                    if d == "__pycache__":
                        continue
                    full_d = os.path.join(dirpath, d)
                    if os.path.islink(full_d):
                        rp_d = os.path.relpath(full_d, repo_root).replace(os.sep, "/")
                        details.append(
                            "%s: on-disk symlinked directory is not permitted: %s" % (label, _safe(rp_d)))
                        continue  # never descend into a symlinked directory
                    kept_dirnames.append(d)
                dirnames[:] = kept_dirnames

                for fname in sorted(filenames):
                    if fname.endswith(".pyc"):
                        continue
                    full = os.path.join(dirpath, fname)
                    rp = os.path.relpath(full, repo_root).replace(os.sep, "/")
                    if rp not in listed_paths:
                        details.append("%s: on-disk file not listed in files[]: %s" % (label, _safe(rp)))
                    rel_to_skill = os.path.relpath(full, skill_dir).replace(os.sep, "/")

                    is_script_shaped = rp.endswith(PROJECT_SCRIPT_EXTS) or rel_to_skill.startswith("scripts/")
                    if not is_script_shaped:
                        shebang, read_err = _has_shebang(full)
                        if read_err:
                            details.append(
                                "%s: could not read on-disk file to check for a shebang: %s: %s" % (
                                    label, _safe(rp), read_err))
                        elif shebang:
                            is_script_shaped = True
                    if is_script_shaped and rp not in declared_scripts:
                        details.append("%s: on-disk script not declared in scripts[]: %s" % (label, _safe(rp)))

                    if rel_to_skill.startswith("hooks/") and rp not in declared_hooks:
                        details.append("%s: on-disk hook not declared in hooks[]: %s" % (label, _safe(rp)))
        elif skill_dir is not None:
            details.append("%s: entry with no on-disk dir: %s" % (label, _safe(path)))
        # else: `path` failed shape validation -- its diagnostic was already emitted above, and
        # (by design) that is the ONLY diagnostic this function emits about this entry's directory.

        for p in declared_scripts:
            if p not in listed_paths:
                details.append("%s: declared script not present in files[]: %s" % (label, _safe(p)))
        for p in declared_hooks:
            if p not in listed_paths:
                details.append("%s: declared hook not present in files[]: %s" % (label, _safe(p)))

        eval_evidence = entry.get("eval_evidence")
        if not isinstance(eval_evidence, dict):
            details.append("%s: eval_evidence is required and must be an object" % label)
        else:
            ekeys = set(eval_evidence.keys())
            unknown_ekeys = ekeys - ALLOWED_PROJECT_EVAL_EVIDENCE_KEYS
            if unknown_ekeys:
                details.append("%s: eval_evidence unknown key(s): %s" % (label, sorted(unknown_ekeys)))

            epath_raw = eval_evidence.get("path")
            esha = eval_evidence.get("blob_sha")
            valid_epath = False
            epath = None
            if not isinstance(epath_raw, str):
                details.append("%s: eval_evidence.path must be a string" % label)
            else:
                # Normalize FIRST (collapses "./x" -> "x", "a/../b" -> "b") so a "./.scratch/x"
                # style bypass of the raw-string prefix checks below is closed. Absolute-ness is
                # still judged on the RAW string (normpath can turn a leading "/" into a
                # platform-specific absolute form on its own, so checking the raw value keeps the
                # absolute-vs-relative distinction unambiguous), and it is checked FIRST so any
                # absolute path -- including "/tmp/..." -- always reports "must not be absolute"
                # rather than being shadowed by a same-shaped relative-only rule.
                epath_norm = os.path.normpath(epath_raw)
                if epath_raw.startswith("/"):
                    details.append("%s: eval_evidence.path must not be absolute" % label)
                elif ".." in epath_norm.split(os.sep):
                    details.append("%s: eval_evidence.path must not contain '..'" % label)
                elif epath_norm == ".scratch" or epath_norm.startswith(".scratch" + os.sep):
                    details.append("%s: eval_evidence.path must not be under .scratch/ (not durable)" % label)
                elif epath_norm == "tmp" or epath_norm.startswith("tmp" + os.sep):
                    details.append("%s: eval_evidence.path must not be under tmp/ (not durable)" % label)
                elif "://" in epath_raw:
                    details.append("%s: eval_evidence.path must not be a URL (not durable/pinned)" % label)
                else:
                    epath = epath_norm
                    valid_epath = True

            if not (isinstance(esha, str) and PROJECT_BLOB_SHA_RE.match(esha)):
                details.append("%s: eval_evidence.blob_sha must be 40-hex, got %r" % (label, esha))

            if valid_epath:
                local_epath = os.path.join(repo_root, epath)
                # No-follow policy: a symlink is rejected on shape alone, BEFORE isfile/ls-files/
                # hash ever run. os.path.isfile() and git hash-object both silently follow a
                # symlink and would otherwise validate against the TARGET's bytes -- a committed
                # symlink is "tracked" too, so without this guard a symlinked eval_evidence.path
                # whose target happens to match the declared blob_sha would validate through an
                # external target that was never reviewed.
                if os.path.islink(local_epath):
                    details.append(
                        "%s: eval_evidence.path must not be a symlink: %s" % (label, _safe(epath)))
                elif not os.path.isfile(local_epath):
                    details.append("%s: eval_evidence.path does not exist on disk: %s" % (label, _safe(epath)))
                else:
                    if require_tracked:
                        try:
                            # `-s` (show staged mode) lets this reject a symlink committed to the
                            # index (mode 120000) even in the rare case the working-tree copy isn't
                            # an OS-level symlink (e.g. a core.symlinks=false checkout writes the
                            # target path as plain text) -- the os.path.islink() guard above alone
                            # would miss that. Requiring exactly 100644 also fail-closes an
                            # executable-bit evidence blob (100755) as a side effect.
                            out = subprocess.run(
                                ["git", "-C", repo_root, "ls-files", "-s", "--", epath],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5, text=True)
                            ls_line = out.stdout.strip() if out.returncode == 0 else ""
                        except Exception:
                            ls_line = ""
                        if not ls_line:
                            details.append(
                                "%s: eval_evidence.path must be tracked by git: %s" % (label, _safe(epath)))
                        else:
                            tracked_mode = ls_line.split()[0]
                            if tracked_mode != "100644":
                                details.append(
                                    "%s: eval_evidence.path must be a tracked regular file (mode "
                                    "100644), got mode %s: %s" % (label, tracked_mode, _safe(epath)))
                    local_sha, err = _git_hash_object(local_epath, repo_root)
                    if err:
                        details.append("%s: eval_evidence: %s" % (label, err))
                    elif isinstance(esha, str) and local_sha != esha:
                        details.append("%s: eval_evidence on-disk blob %s != declared blob_sha %s" % (
                            label, local_sha, esha))

        review_status = entry.get("review_status")
        if review_status not in ("draft", "approved"):
            details.append("%s: review_status must be \"draft\" or \"approved\", got %r" % (label, review_status))

        reviewed_base_sha = entry.get("reviewed_base_sha")
        if not (isinstance(reviewed_base_sha, str) and PROJECT_BLOB_SHA_RE.match(reviewed_base_sha)):
            details.append("%s: reviewed_base_sha must be 40-hex, got %r" % (label, reviewed_base_sha))

        reviewed_at = entry.get("reviewed_at")
        if not (isinstance(reviewed_at, str) and PROJECT_REVIEWED_AT_RE.match(reviewed_at)):
            details.append("%s: reviewed_at must match YYYY-MM-DD, got %r" % (label, reviewed_at))
        elif isinstance(reviewed_at, str):
            try:
                datetime.datetime.strptime(reviewed_at, "%Y-%m-%d")
            except ValueError:
                details.append("%s: reviewed_at is not a valid calendar date: %r" % (label, reviewed_at))

        # Frontmatter cross-check: only meaningful once the SKILL.md actually exists on disk.
        if skill_dir is not None and os.path.isfile(os.path.join(skill_dir, "SKILL.md")):
            skillmd_path = os.path.join(skill_dir, "SKILL.md")
            keys, ok = parse_frontmatter(skillmd_path)
            if not ok:
                details.append("%s: SKILL.md has no valid frontmatter fence" % label)
            else:
                extra = keys - ALLOWED_SKILL_KEYS_PROJECT
                if extra:
                    details.append("%s: SKILL.md frontmatter has unsupported key(s): %s" % (label, sorted(extra)))
                fm_name = parse_frontmatter_value(skillmd_path, "name")
                if isinstance(name, str) and fm_name != name:
                    details.append("%s: SKILL.md frontmatter name %r does not match manifest name %r" % (
                        label, fm_name, name))
                has_dmi = "disable-model-invocation" in keys
                if invocation == "user-only" and not has_dmi:
                    details.append(
                        "%s: invocation \"user-only\" requires disable-model-invocation in frontmatter" % label)
                if invocation == "model+user" and has_dmi:
                    details.append(
                        "%s: invocation \"model+user\" requires disable-model-invocation to be absent" % label)

    return details


def check_project_manifest():
    try:
        manifest = load_manifest(PROJECT_MANIFEST_PATH)
    except Exception as e:
        report("project-manifest-valid", False, ["could not parse %s: %s" % (rel(PROJECT_MANIFEST_PATH), e)])
        return
    details = validate_project_manifest(manifest, REPO_ROOT, require_tracked=True)
    report("project-manifest-valid", not details, details)


ALL_CHECKS = [
    check_required_files,
    check_json_validity,
    check_no_symlinks_no_exec,
    check_skill_dirs_have_skillmd,
    check_unique_skill_names,
    check_frontmatter_keys,
    check_relative_links_resolve,
    check_no_absolute_path_imports,
    check_forbidden_vendor_files_absent,
    check_manifest_fs_consistency,
    check_manifest_ownership_partition,
    check_excluded_absent,
    check_blob_hashes,
    check_anthropic_file_hashes,
    check_anthropic_vendored_modes,
    check_anthropic_scripts_audited,
    check_builtin_collision_denylist,
    check_patch_ledger_coverage,
    check_patch_marker_balance,
    check_anthropic_patch_coverage,
    check_application_paths_untouched,
    check_settings_schema,
    check_settings_mcp_mutations_denied,
    check_rules_frontmatter_and_uniqueness,
    check_hidden_unicode,
    check_project_manifest,
]


# --------------------------------------------------------------------------
# --self-test: offline fixture matrix for the project-manifest logic above.
#
# Most fixtures build their own tree under tempfile.TemporaryDirectory; none ever WRITES to this
# repo, makes a network call, or sleeps. Two fixtures (G11, G12) are the exception to "synthetic
# tree" specifically, not to "never writes": they READ real files from this repo (the vendored
# mattpocock manifest, and the real project manifest) to check facts about the actual repo state
# -- read-only, like every other check this script performs. G1-G12 exercise positive/structural
# cases; N1-N16 are negative cases, one per distinct violation class named in
# project-skills-policy.md's schema. Each fixture function raises AssertionError (with a
# diagnostic message) on failure and returns None on success -- the runner below is the only place
# results are aggregated or printed.
# --------------------------------------------------------------------------


def _write_file(path, content):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        f.write(content)


def _build_valid_entry_fixture(base_dir, name, invocation="model+user",
                                mutation_capability="read-only", extra_entry_overrides=None,
                                skip_eval=False):
    """Builds a minimal, fully valid on-disk project-skill fixture (prose-only SKILL.md, correct
    hashes, valid eval evidence) under base_dir and returns (manifest_dict, repo_root, skill_dir,
    files_list) -- files_list is the SAME list object embedded in the manifest entry, so callers
    can mutate it in place (append/corrupt/etc.) to build a negative-case fixture on top of a
    known-good baseline."""
    repo_root = base_dir
    skill_rel = ".claude/skills/%s" % name
    skill_dir = os.path.join(repo_root, skill_rel)

    dmi_line = "disable-model-invocation: true\n" if invocation == "user-only" else ""
    skillmd_content = (
        "---\n"
        "name: %s\n"
        "description: A minimal prose-only project skill used for a governance self-test fixture.\n"
        "%s"
        "---\n\n"
        "# %s\n\nFixture content, never installed for real.\n" % (name, dmi_line, name)
    )
    skillmd_path = os.path.join(skill_dir, "SKILL.md")
    _write_file(skillmd_path, skillmd_content)
    skillmd_sha, err = _git_hash_object(skillmd_path, repo_root)
    assert err is None, "fixture setup: %s" % err

    files = [{"path": skill_rel + "/SKILL.md", "blob_sha": skillmd_sha, "mode": "100644"}]

    eval_evidence = None
    if not skip_eval:
        eval_rel = "docs/agents/eval-evidence/%s.md" % name
        eval_path = os.path.join(repo_root, eval_rel)
        _write_file(eval_path, "# eval evidence for %s\n\nFixture content.\n" % name)
        eval_sha, err = _git_hash_object(eval_path, repo_root)
        assert err is None, "fixture setup: %s" % err
        eval_evidence = {"path": eval_rel, "blob_sha": eval_sha}

    entry = {
        "name": name,
        "path": skill_rel,
        "origin": "project",
        "invocation": invocation,
        "mutation_capability": mutation_capability,
        "scripts": [],
        "hooks": [],
        "files": files,
        "eval_evidence": eval_evidence,
        "review_status": "approved",
        "reviewed_base_sha": "a" * 40,
        "reviewed_at": "2026-07-27",
    }
    if extra_entry_overrides:
        entry.update(extra_entry_overrides)

    manifest = {
        "schema_version": 1,
        "ownership_class": PROJECT_OWNERSHIP_CLASS,
        "notes": "self-test fixture",
        "skills": [entry],
    }
    return manifest, repo_root, skill_dir, files


# ---- G1-G4: partition_details() pure helper, no filesystem involved ----

def _st_g1():
    fs_names = {"a", "b", "c"}
    names_by_label = {"mattpocock": {"a"}, "anthropic": {"b"}, "project": {"c"}}
    details = partition_details(fs_names, names_by_label)
    assert details == [], "expected no partition details, got %r" % (details,)


def _st_g2():
    fs_names = {"a", "b"}
    names_by_label = {"mattpocock": {"a"}, "anthropic": {"a"}, "project": {"b"}}
    details = partition_details(fs_names, names_by_label)
    assert any("cross-manifest name collision" in d for d in details), details


def _st_g3():
    fs_names = {"a", "b"}
    names_by_label = {"mattpocock": {"a"}, "anthropic": set(), "project": set()}
    details = partition_details(fs_names, names_by_label)
    assert any("dir claimed by no manifest: b" in d for d in details), details


def _st_g4():
    fs_names = {"a"}
    names_by_label = {"mattpocock": {"a", "ghost"}, "anthropic": set(), "project": set()}
    details = partition_details(fs_names, names_by_label)
    assert any("manifest entry with no on-disk dir: ghost" in d for d in details), details


# ---- G5-G10: validate_project_manifest() against synthetic fixture trees ----

def _st_g5():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, skill_dir, _files = _build_valid_entry_fixture(tmp, "g5-skill")
        os.remove(os.path.join(skill_dir, "SKILL.md"))
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("no SKILL.md" in d for d in details), details


def _st_g6():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, skill_dir, _files = _build_valid_entry_fixture(tmp, "g6-skill")
        with open(os.path.join(skill_dir, "SKILL.md"), "a", encoding="utf-8") as f:
            f.write("\nCORRUPTED AFTER HASHING\n")
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("on-disk blob" in d and "!=" in d for d in details), details


def _st_g7():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, skill_dir, files = _build_valid_entry_fixture(tmp, "g7-skill")
        skillmd_path = os.path.join(skill_dir, "SKILL.md")
        new_content = (
            "---\n"
            "name: g7-skill\n"
            "description: fixture\n"
            "unsupported-key: true\n"
            "---\n\nBody.\n"
        )
        _write_file(skillmd_path, new_content)
        new_sha, err = _git_hash_object(skillmd_path, repo_root)
        assert err is None, err
        files[0]["blob_sha"] = new_sha  # keep the hash pin valid so only frontmatter-keys fires
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("unsupported key" in d for d in details), details


def _st_g8():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, _skill_dir, _files = _build_valid_entry_fixture(
            tmp, "g8-skill", extra_entry_overrides={"upstream_blob_sha": "b" * 40})
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("must not claim upstream provenance" in d for d in details), details


def _st_g9():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, skill_dir, _files = _build_valid_entry_fixture(tmp, "g9-skill")
        _write_file(os.path.join(skill_dir, "scripts", "helper.py"), "# helper\n")
        _write_file(os.path.join(skill_dir, "hooks", "pre.py"), "# hook\n")
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("on-disk script not declared in scripts[]" in d for d in details), details
        assert any("on-disk hook not declared in hooks[]" in d for d in details), details


def _st_g10():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, _skill_dir, _files = _build_valid_entry_fixture(tmp, "g10-skill")
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert details == [], details


def _st_g11():
    labels = {entry["label"] for entry in MANIFESTS}
    assert labels == {"mattpocock", "anthropic"}, labels
    manifest_paths = {entry["manifest"] for entry in MANIFESTS}
    assert PROJECT_MANIFEST_PATH not in manifest_paths, manifest_paths
    mattpocock_shaped = load_manifest(MANIFEST_PATH)
    details = validate_project_manifest(mattpocock_shaped, REPO_ROOT, require_tracked=False)
    assert details, "expected validate_project_manifest to reject a vendored-shaped manifest"


def _st_g12():
    manifest = load_manifest(PROJECT_MANIFEST_PATH)
    details = validate_project_manifest(manifest, REPO_ROOT, require_tracked=True)
    assert details == [], details


# ---- N1-N13: one negative fixture per distinct violation class ----

def _st_n1():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, _skill_dir, _files = _build_valid_entry_fixture(tmp, "n1-skill")
        manifest["ownership_class"] = "something-else"
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("ownership_class must equal" in d for d in details), details


def _st_n2():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, _skill_dir, _files = _build_valid_entry_fixture(
            tmp, "n2-skill", extra_entry_overrides={"origin": "somewhere-else"})
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("origin must equal" in d for d in details), details


def _st_n3():
    # A rejected entry `path` must NEVER be used for filesystem traversal: skill_dir stays None,
    # so no directory-existence check and no directory walk are ever attempted for either variant
    # below -- the shape diagnostic is the ONLY diagnostic emitted for the entry's directory.
    walk_markers = ("entry with no on-disk dir", "on-disk file not listed in files[]",
                    "on-disk script not declared", "on-disk hook not declared",
                    "on-disk symlinked directory", "skill dir has no SKILL.md")
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, _skill_dir, _files = _build_valid_entry_fixture(
            tmp, "n3-skill", extra_entry_overrides={"path": "../etc/passwd"})
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("must not contain '..'" in d for d in details), details
        assert not any(any(m in d for m in walk_markers) for d in details), details

        manifest2, repo_root2, _skill_dir2, _files2 = _build_valid_entry_fixture(
            tmp, "n3b-skill", extra_entry_overrides={"path": "/etc/passwd"})
        details2 = validate_project_manifest(manifest2, repo_root2, require_tracked=False)
        assert any("must not be absolute" in d for d in details2), details2
        assert not any(any(m in d for m in walk_markers) for d in details2), details2


def _st_n4():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, _skill_dir, _files = _build_valid_entry_fixture(
            tmp, "n4-skill", extra_entry_overrides={"path": ".claude/skills/different-name"})
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("path must equal" in d for d in details), details


def _st_n5():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, _skill_dir, files = _build_valid_entry_fixture(tmp, "n5-skill")
        files.append({"path": ".claude/skills/n5-skill/ghost.md", "blob_sha": "c" * 40, "mode": "100644"})
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("declared file does not exist on disk" in d for d in details), details


def _st_n6():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, skill_dir, _files = _build_valid_entry_fixture(tmp, "n6-skill")
        _write_file(os.path.join(skill_dir, "extra.md"), "extra\n")
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("on-disk file not listed in files[]" in d for d in details), details


def _st_n7():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, _skill_dir, files = _build_valid_entry_fixture(tmp, "n7-skill")
        files[0]["mode"] = "100755"
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any('mode must be "100644"' in d for d in details), details

    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, skill_dir, _files = _build_valid_entry_fixture(tmp, "n7b-skill")
        skillmd_path = os.path.join(skill_dir, "SKILL.md")
        st = os.stat(skillmd_path)
        os.chmod(skillmd_path, st.st_mode | 0o111)
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("executable bit set" in d for d in details), details


def _st_n8():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, _skill_dir, _files = _build_valid_entry_fixture(tmp, "n8-skill")
        manifest["skills"][0]["eval_evidence"]["path"] = "docs/agents/eval-evidence/does-not-exist.md"
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("eval_evidence.path does not exist on disk" in d for d in details), details


def _st_n9():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, _skill_dir, _files = _build_valid_entry_fixture(tmp, "n9-skill")
        eval_path = os.path.join(repo_root, manifest["skills"][0]["eval_evidence"]["path"])
        with open(eval_path, "a", encoding="utf-8") as f:
            f.write("drift\n")
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("eval_evidence on-disk blob" in d for d in details), details


def _st_n10():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, _skill_dir, _files = _build_valid_entry_fixture(
            tmp, "n10-skill", extra_entry_overrides={"invocation": "model-only"})
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("invocation must be" in d for d in details), details


def _st_n11():
    with tempfile.TemporaryDirectory() as tmp:
        # Helper writes SKILL.md WITHOUT disable-model-invocation (default invocation="model+user"
        # passed to the helper controls frontmatter content); the manifest is then overridden to
        # claim "user-only" -- a direct manifest/frontmatter mismatch.
        manifest, repo_root, _skill_dir, _files = _build_valid_entry_fixture(
            tmp, "n11-skill", extra_entry_overrides={"invocation": "user-only"})
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("requires disable-model-invocation" in d for d in details), details


def _st_n12():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, _skill_dir, _files = _build_valid_entry_fixture(
            tmp, "n12-skill",
            extra_entry_overrides={"reviewed_base_sha": "not-a-sha", "reviewed_at": "07/27/2026"})
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("reviewed_base_sha must be 40-hex" in d for d in details), details
        assert any("reviewed_at must match" in d for d in details), details


def _st_n13():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, skill_dir, _files = _build_valid_entry_fixture(tmp, "n13-skill")
        skillmd_path = os.path.join(skill_dir, "SKILL.md")
        real_target = os.path.join(skill_dir, "SKILL.real.md")
        os.replace(skillmd_path, real_target)
        try:
            os.symlink(real_target, skillmd_path)
        except (OSError, NotImplementedError) as e:
            raise AssertionError("platform refused symlink creation: %s" % e)
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("must not be a symlink" in d for d in details), details


def _st_n14():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, _skill_dir, _files = _build_valid_entry_fixture(tmp, "n14-skill")
        # An independent, well-formed duplicate of the same entry (same name, same path, same
        # on-disk files it legitimately points at) -- the collision itself is the violation, not
        # any malformedness in either copy.
        duplicate_entry = json.loads(json.dumps(manifest["skills"][0]))
        manifest["skills"].append(duplicate_entry)
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("duplicate skill name within manifest" in d for d in details), details
        assert any("duplicate skill path within manifest" in d for d in details), details


def _st_n15():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, skill_dir, _files = _build_valid_entry_fixture(tmp, "n15-skill")
        # The payload lives OUTSIDE skill_dir entirely, reached only through a symlinked
        # subdirectory inside skill_dir -- this is what proves the walk never descends into it
        # (a payload placed directly inside skill_dir would be visited on its own merits and
        # wouldn't isolate "did the walk follow the symlink" from "did the walk see the dir at
        # all").
        outside_dir = os.path.join(tmp, "outside-payload")
        os.makedirs(outside_dir, exist_ok=True)
        _write_file(os.path.join(outside_dir, "payload.md"), "hidden payload\n")
        link_path = os.path.join(skill_dir, "linked-subdir")
        try:
            os.symlink(outside_dir, link_path, target_is_directory=True)
        except (OSError, NotImplementedError) as e:
            raise AssertionError("platform refused symlink creation: %s" % e)
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("on-disk symlinked directory is not permitted" in d for d in details), details
        assert not any("payload.md" in d for d in details), details


def _st_n16():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, _skill_dir, _files = _build_valid_entry_fixture(
            tmp, "n16-skill", extra_entry_overrides={"review_status": "draft"})
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any('review_status must be "approved"' in d for d in details), details


# ---- N17-N21: project_skill_names() / validate_project_manifest() against structurally
# malformed (but JSON-valid) manifests -- the MINOR-1 gap: these must never raise, and the
# fail-closed diagnostics must actually be reached. N17-N19 swap the module-level
# PROJECT_MANIFEST_PATH to point at a synthetic malformed file so project_skill_names() itself
# (the function that was raising) is exercised directly, not just validate_project_manifest()'s
# own (separately correct) top-level guard -- always restored in a finally so a fixture failure
# can never leak a bad path into any fixture that runs after it. ----

def _st_n17():
    global PROJECT_MANIFEST_PATH
    with tempfile.TemporaryDirectory() as tmp:
        bad_path = os.path.join(tmp, "manifest.json")
        _write_file(bad_path, json.dumps([{"name": "should-never-appear"}]))
        saved = PROJECT_MANIFEST_PATH
        PROJECT_MANIFEST_PATH = bad_path
        try:
            names = project_skill_names()
        finally:
            PROJECT_MANIFEST_PATH = saved
        assert names == [], "expected [] for a top-level-array manifest, got %r" % (names,)

    details = validate_project_manifest([{"name": "x"}], REPO_ROOT, require_tracked=False)
    assert details == ["top-level manifest must be a JSON object"], details
    details_empty = validate_project_manifest([], REPO_ROOT, require_tracked=False)
    assert details_empty == ["top-level manifest must be a JSON object"], details_empty


def _st_n18():
    global PROJECT_MANIFEST_PATH
    manifest = {"schema_version": 1, "ownership_class": PROJECT_OWNERSHIP_CLASS, "skills": 7}
    with tempfile.TemporaryDirectory() as tmp:
        bad_path = os.path.join(tmp, "manifest.json")
        _write_file(bad_path, json.dumps(manifest))
        saved = PROJECT_MANIFEST_PATH
        PROJECT_MANIFEST_PATH = bad_path
        try:
            names = project_skill_names()
        finally:
            PROJECT_MANIFEST_PATH = saved
        assert names == [], "expected [] when skills is not a list, got %r" % (names,)

    details = validate_project_manifest(manifest, REPO_ROOT, require_tracked=False)
    assert any("skills must be a list" in d for d in details), details


def _st_n19():
    global PROJECT_MANIFEST_PATH
    manifest = {"schema_version": 1, "ownership_class": PROJECT_OWNERSHIP_CLASS, "skills": [7]}
    with tempfile.TemporaryDirectory() as tmp:
        bad_path = os.path.join(tmp, "manifest.json")
        _write_file(bad_path, json.dumps(manifest))
        saved = PROJECT_MANIFEST_PATH
        PROJECT_MANIFEST_PATH = bad_path
        try:
            names = project_skill_names()
        finally:
            PROJECT_MANIFEST_PATH = saved
        assert names == [], "expected [] when a skills[] entry is not an object, got %r" % (names,)

    details = validate_project_manifest(manifest, REPO_ROOT, require_tracked=False)
    assert any("entry must be an object" in d for d in details), details


def _st_n20():
    # A skills[] entry that IS an object but is missing every required member must produce
    # per-field missing-key diagnostics, never an exception -- this path was already correct
    # before MINOR-1 (validate_project_manifest's per-field .get()-based checks), included here as
    # coverage for the malformed-input failure matrix rather than as a MINOR-1 regression probe.
    manifest = {"schema_version": 1, "ownership_class": PROJECT_OWNERSHIP_CLASS, "skills": [{}]}
    details = validate_project_manifest(manifest, REPO_ROOT, require_tracked=False)
    assert any("name must match" in d for d in details), details
    assert any("path must be a string" in d for d in details), details
    assert any("origin must equal" in d for d in details), details
    assert any("invocation must be" in d for d in details), details
    assert any("mutation_capability must be" in d for d in details), details
    assert any("eval_evidence is required" in d for d in details), details


def _st_n21():
    # Full-report continuation: point the real production path constant at a malformed temp
    # manifest and run the REAL ALL_CHECKS list end to end (not validate_project_manifest() in
    # isolation) -- this is what actually proves the MINOR-1 gap is closed, since the original
    # failure mode was project_skill_names() raising inside check_unique_skill_names (check 5 of
    # 26), which aborted the whole run before check_project_manifest's own diagnostic was ever
    # reached. RESULTS is saved/restored alongside PROJECT_MANIFEST_PATH so this fixture can never
    # leave stray report() entries for anything that inspects RESULTS afterward.
    global PROJECT_MANIFEST_PATH, RESULTS
    with tempfile.TemporaryDirectory() as tmp:
        bad_path = os.path.join(tmp, "manifest.json")
        _write_file(bad_path, json.dumps({"skills": 7}))
        saved_path = PROJECT_MANIFEST_PATH
        saved_results = RESULTS
        PROJECT_MANIFEST_PATH = bad_path
        RESULTS = []
        try:
            # Every other self-test fixture is silent (none of them route through report()); this
            # one uniquely does, since proving MINOR-1 fixed means driving the REAL ALL_CHECKS
            # list end to end. Redirect stdout for the duration so --self-test's output stays
            # fixture-results-only, matching every other fixture's shape.
            with contextlib.redirect_stdout(io.StringIO()):
                for check in ALL_CHECKS:
                    check()
            results = RESULTS
        except Exception as e:
            raise AssertionError("full check run raised with a malformed project manifest: %r" % (e,))
        finally:
            PROJECT_MANIFEST_PATH = saved_path
            RESULTS = saved_results
    assert len(results) == 26, "expected exactly 26 labeled results, got %d: %r" % (
        len(results), [r[0] for r in results])
    failing = [name for name, ok, _ in results if not ok]
    assert failing, "expected at least one failing check, got none"
    assert "project-manifest-valid" in failing, failing


# ---- N22-N23: eval_evidence.path symlink handling -- the MINOR-2 gap. N22 exercises the
# working-tree os.path.islink() guard (filesystem only, require_tracked=False); N23 exercises the
# independent git-index tracked-mode guard (require_tracked=True) in a real, disposable temp git
# repo, specifically for the case a working-tree os.path.islink() check alone would miss (a
# checked-out plain file whose git INDEX entry is still mode 120000 -- e.g. a core.symlinks=false
# checkout). ----

def _st_n22():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, _skill_dir, _files = _build_valid_entry_fixture(tmp, "n22-skill")
        eval_rel = manifest["skills"][0]["eval_evidence"]["path"]
        eval_path = os.path.join(repo_root, eval_rel)
        # Move (not copy) the real evidence file OUTSIDE the fixture's skill tree entirely, then
        # symlink the declared eval_evidence.path at it -- the target's bytes still match the
        # declared blob_sha exactly (same bytes, just relocated), which is precisely the bypass
        # MINOR-2 closes: without the no-follow guard, hashing the target would validate clean.
        external_target = os.path.join(tmp, "outside-eval-target-n22.md")
        os.replace(eval_path, external_target)
        try:
            os.symlink(external_target, eval_path)
        except (OSError, NotImplementedError) as e:
            raise AssertionError("platform refused symlink creation: %s" % e)
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("eval_evidence.path must not be a symlink" in d for d in details), details
        # No-follow proof: the hash-mismatch diagnostic must never fire, because the target is
        # never read at all once islink() has already failed the entry closed.
        assert not any("eval_evidence on-disk blob" in d for d in details), details

    # Positive control: an ordinary (non-symlinked) evidence file still passes clean.
    with tempfile.TemporaryDirectory() as tmp:
        manifest2, repo_root2, _skill_dir2, _files2 = _build_valid_entry_fixture(tmp, "n22b-skill")
        details2 = validate_project_manifest(manifest2, repo_root2, require_tracked=False)
        assert not any("eval_evidence" in d for d in details2), details2


def _st_n23():
    git = shutil.which("git")
    assert git, "git not found on PATH; required for the N23 fixture"
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, _skill_dir, _files = _build_valid_entry_fixture(tmp, "n23-skill")
        eval_rel = manifest["skills"][0]["eval_evidence"]["path"]
        eval_path = os.path.join(repo_root, eval_rel)

        def _git(*args):
            out = subprocess.run(["git"] + list(args), cwd=repo_root, stdout=subprocess.PIPE,
                                  stderr=subprocess.PIPE, timeout=5, text=True)
            assert out.returncode == 0, "git %s failed: %s" % (" ".join(args), out.stderr)

        # -c user.name/user.email flags (never os.environ mutation) scope the commit identity to
        # this one git invocation, so this disposable /tmp fixture repo never touches the calling
        # process's or the primary repo's git config.
        author_flags = ("-c", "user.name=governance-self-test",
                         "-c", "user.email=governance-self-test@example.invalid")
        _git("init", "-q")
        _git(*(author_flags + ("add", "-A")))
        _git(*(author_flags + ("commit", "-q", "-m", "n23: initial regular file")))

        # Positive control: a committed 100644 regular file passes the tracked-mode check
        # unchanged.
        details_regular = validate_project_manifest(manifest, repo_root, require_tracked=True)
        assert not any("eval_evidence" in d for d in details_regular), details_regular

        # Replace the tracked file with a real OS symlink and commit it (mode 120000 in the
        # index), then simulate a core.symlinks=false checkout by re-writing the working-tree path
        # as a PLAIN FILE containing the link target text -- this isolates the git-index
        # tracked-mode guard from the working-tree os.path.islink() guard above it: with a real OS
        # symlink still on disk, islink() alone would already catch this case, so it wouldn't
        # prove the second, independent guard exists.
        external_target = os.path.join(tmp, "outside-eval-target-n23.md")
        _write_file(external_target, "external, must never be read\n")
        os.remove(eval_path)
        os.symlink(external_target, eval_path)
        _git(*(author_flags + ("add", "-A")))
        _git(*(author_flags + ("commit", "-q", "-m", "n23: replace with a committed symlink")))

        os.remove(eval_path)
        _write_file(eval_path, os.path.relpath(external_target, os.path.dirname(eval_path)))
        assert not os.path.islink(eval_path), "fixture setup: expected a plain file, not a symlink"

        details_symlink = validate_project_manifest(manifest, repo_root, require_tracked=True)
        assert any("mode 120000" in d for d in details_symlink), details_symlink


# ---- N24: symlinked skill root (standalone validate_project_manifest, the MINOR-3 gap) ----

def _st_n24():
    with tempfile.TemporaryDirectory() as tmp:
        manifest, repo_root, skill_dir, _files = _build_valid_entry_fixture(tmp, "n24-skill")
        # The external target directory carries a marker file that would never legitimately be
        # produced by this fixture -- its presence in any diagnostic would mean the walk descended
        # into the symlinked root, which the fix must prevent entirely.
        external_dir = os.path.join(tmp, "external-skill-root-n24")
        os.makedirs(external_dir, exist_ok=True)
        _write_file(os.path.join(external_dir, "marker.md"), "external marker, must never be walked\n")
        shutil.rmtree(skill_dir)
        try:
            os.symlink(external_dir, skill_dir, target_is_directory=True)
        except (OSError, NotImplementedError) as e:
            raise AssertionError("platform refused symlink creation: %s" % e)
        details = validate_project_manifest(manifest, repo_root, require_tracked=False)
        assert any("skill directory must not be a symlink" in d for d in details), details
        assert not any("marker.md" in d for d in details), details

    # Positive control: an ordinary (non-symlinked) skill directory is unaffected.
    with tempfile.TemporaryDirectory() as tmp:
        manifest2, repo_root2, _skill_dir2, _files2 = _build_valid_entry_fixture(tmp, "n24b-skill")
        details2 = validate_project_manifest(manifest2, repo_root2, require_tracked=False)
        assert not any("skill directory must not be a symlink" in d for d in details2), details2


def _self_test_fixtures():
    return [
        ("G1", "three ownership sources exactly partition a fixture fs", _st_g1),
        ("G2", "same name claimed by two ownership sources", _st_g2),
        ("G3", "fs dir claimed by no ownership source", _st_g3),
        ("G4", "manifest name with no fs dir (phantom entry)", _st_g4),
        ("G5", "project entry dir exists but has no SKILL.md", _st_g5),
        ("G6", "declared file content differs from blob_sha", _st_g6),
        ("G7", "project SKILL.md frontmatter has an unsupported key", _st_g7),
        ("G8", "entry contains an upstream* key", _st_g8),
        ("G9", "undeclared on-disk scripts/ and hooks/ files", _st_g9),
        ("G10", "complete valid prose-only read-only entry passes clean", _st_g10),
        ("G11", "vendored MANIFESTS registry/logic untouched by project logic", _st_g11),
        ("G12", "real (empty) project manifest validates clean against the real repo", _st_g12),
        ("N1", "invalid ownership_class", _st_n1),
        ("N2", "invalid origin", _st_n2),
        ("N3", "path escaping .claude/skills (traversal and absolute)", _st_n3),
        ("N4", "name/path mismatch", _st_n4),
        ("N5", "phantom declared file (in files[], absent on disk)", _st_n5),
        ("N6", "undeclared ordinary on-disk file", _st_n6),
        ("N7", "incorrect file mode (manifest mode and on-disk exec bit)", _st_n7),
        ("N8", "missing eval evidence file", _st_n8),
        ("N9", "eval evidence hash drift", _st_n9),
        ("N10", "invalid invocation enum value", _st_n10),
        ("N11", "frontmatter/manifest invocation mismatch", _st_n11),
        ("N12", "malformed reviewed_base_sha and reviewed_at", _st_n12),
        ("N13", "symlink where a regular file is required", _st_n13),
        ("N14", "duplicate skill name/path within one manifest", _st_n14),
        ("N15", "symlinked subdirectory inside the skill dir", _st_n15),
        ("N16", "on-disk skill dir with review_status draft", _st_n16),
        ("N17", "top-level manifest is a JSON array", _st_n17),
        ("N18", "skills is not a list", _st_n18),
        ("N19", "a skills[] entry is not an object", _st_n19),
        ("N20", "skills[] entry missing required structural members", _st_n20),
        ("N21", "full ALL_CHECKS run against a malformed project manifest", _st_n21),
        ("N22", "eval_evidence.path is a working-tree symlink", _st_n22),
        ("N23", "eval_evidence.path is a git-index symlink (tracked-mode check)", _st_n23),
        ("N24", "symlinked skill root, standalone validate_project_manifest", _st_n24),
    ]


def run_self_test():
    """Runs every fixture, catching AssertionError/Exception per-fixture so one failure doesn't
    abort the rest. Returns (passed_count, total_count, failures) where failures is a list of
    (fixture_id, description, error_message) tuples, in fixture order."""
    failures = []
    fixtures = _self_test_fixtures()
    for fid, desc, fn in fixtures:
        try:
            fn()
        except AssertionError as e:
            failures.append((fid, desc, str(e)))
        except Exception as e:
            failures.append((fid, desc, "unexpected error: %r" % (e,)))
    return len(fixtures) - len(failures), len(fixtures), failures


def main():
    self_test = "--self-test" in sys.argv
    self_test_hook = "--self-test-hook" in sys.argv

    if self_test:
        passed, total, failures = run_self_test()
        for fid, desc, err in failures:
            print("[FAIL] self-test %s (%s): %s" % (fid, desc, err))
        print("\n%d/%d self-test fixtures passed" % (passed, total))
        return 0 if not failures else 1

    for check in ALL_CHECKS:
        check()
    if self_test_hook:
        check_hook_self_test()
    failed = [name for name, ok, _ in RESULTS if not ok]
    print("\n%d/%d checks passed" % (len(RESULTS) - len(failed), len(RESULTS)))
    if failed:
        print("Failed checks: %s" % ", ".join(failed))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
