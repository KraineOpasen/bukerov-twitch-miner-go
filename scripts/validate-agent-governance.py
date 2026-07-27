#!/usr/bin/env python3
"""validate-agent-governance.py — offline, read-only consistency checks for the
Claude Code Governance v2 layer (.claude/**, docs/agents/**, docs/adr/**).

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
import json
import os
import re
import subprocess
import sys

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CLAUDE_DIR = os.path.join(REPO_ROOT, ".claude")
SKILLS_DIR = os.path.join(CLAUDE_DIR, "skills")
RULES_DIR = os.path.join(CLAUDE_DIR, "rules")
DOCS_AGENTS_DIR = os.path.join(REPO_ROOT, "docs", "agents")
MANIFEST_PATH = os.path.join(DOCS_AGENTS_DIR, "mattpocock-skills-manifest.json")
PATCHES_PATH = os.path.join(DOCS_AGENTS_DIR, "mattpocock-skills-patches.md")
ANTHROPIC_MANIFEST_PATH = os.path.join(DOCS_AGENTS_DIR, "anthropic-skills-manifest.json")
ANTHROPIC_PATCHES_PATH = os.path.join(DOCS_AGENTS_DIR, "anthropic-skills-patches.md")
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


# --------------------------------------------------------------------------
# Checks
# --------------------------------------------------------------------------


def check_required_files():
    required = [
        "CLAUDE.md", "CONTEXT.md", ".gitignore",
        ".claude/settings.json", ".claude/hooks/governance-policy.py", ".claude/skills/LICENSE",
        "docs/agents/operation-modes.md", "docs/agents/task-contract.md", "docs/agents/quality-gates.md",
        "docs/agents/issue-tracker.md", "docs/agents/domain.md", "docs/agents/triage-labels.md",
        "docs/agents/mattpocock-skills-manifest.json", "docs/agents/mattpocock-skills-patches.md",
        "docs/agents/mattpocock-skills-policy.md", "docs/adr/0001-agent-governance-v2.md",
        "scripts/validate-agent-governance.py",
        "docs/agents/anthropic-skills-manifest.json", "docs/agents/anthropic-skills-patches.md",
        "docs/agents/anthropic-skills-policy.md",
    ]
    missing = [p for p in required if not os.path.isfile(os.path.join(REPO_ROOT, p))]
    report("required-files-exist", not missing, ["missing %s" % p for p in missing])


def check_json_validity():
    details = []
    for p in (SETTINGS_PATH, MANIFEST_PATH, ANTHROPIC_MANIFEST_PATH):
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
    # Union across ALL manifests: a name duplicated within one manifest OR appearing in both
    # manifests is equally a collision -- two different skills must never share a name.
    names = []
    for entry in MANIFESTS:
        manifest = load_manifest(entry["manifest"])
        names.extend(s["name"] for s in manifest["skills"])
    dup_manifest = {n for n in names if names.count(n) > 1}
    details = ["duplicate dir: %s" % d for d in dup] + ["duplicate manifest entry (union of all manifests): %s" % n for n in dup_manifest]
    report("unique-skill-names", not details, details)


def check_frontmatter_keys():
    details = []
    anthropic_dirs = anthropic_skill_dir_names()
    for name in list_skill_dirs():
        skillmd = os.path.join(SKILLS_DIR, name, "SKILL.md")
        if not os.path.isfile(skillmd):
            continue
        keys, ok = parse_frontmatter(skillmd)
        if not ok:
            details.append("%s/SKILL.md: no valid frontmatter fence" % name)
            continue
        allowed = ALLOWED_SKILL_KEYS_ANTHROPIC if name in anthropic_dirs else ALLOWED_SKILL_KEYS
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
    """Per-manifest filesystem consistency, reported under the original single-manifest report
    name. With two manifests now in play, a directory legitimately claimed by manifest B must not
    be flagged as "not in manifest" when checking manifest A -- so each manifest's "dir but not in
    manifest" half only considers directories no OTHER manifest already claims. Directories claimed
    by no manifest at all are still caught here (via every manifest independently), and again,
    more directly, by check_manifest_ownership_partition below."""
    fs_names = set(list_skill_dirs())
    manifest_names_by_label = {}
    for entry in MANIFESTS:
        manifest = load_manifest(entry["manifest"])
        manifest_names_by_label[entry["label"]] = {s["name"] for s in manifest["skills"]}

    details = []
    for entry in MANIFESTS:
        label = entry["label"]
        manifest_names = manifest_names_by_label[label]
        other_claimed = set()
        for other_label, other_names in manifest_names_by_label.items():
            if other_label != label:
                other_claimed |= other_names
        only_manifest = manifest_names - fs_names
        only_fs = (fs_names - other_claimed) - manifest_names
        details += ["%s: in manifest but no dir: %s" % (label, n) for n in sorted(only_manifest)]
        details += ["%s: dir but not in manifest: %s" % (label, n) for n in sorted(only_fs)]
    report("manifest-filesystem-consistency", not details, details)


def check_manifest_ownership_partition():
    """Every .claude/skills/<dir> must be claimed by EXACTLY one manifest: the union of all
    manifests' skill names must equal the set of directories on disk, and no name may appear in
    more than one manifest (a cross-manifest name collision would mean two different reviewed
    upstreams both think they own the same directory)."""
    fs_names = set(list_skill_dirs())
    names_by_label = {}
    for entry in MANIFESTS:
        manifest = load_manifest(entry["manifest"])
        names_by_label[entry["label"]] = {s["name"] for s in manifest["skills"]}

    details = []
    all_claimed = set()
    for label, names in names_by_label.items():
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
    """No vendored skill directory name and no manifest skill name (across either manifest) may
    collide with a name Claude Code (or another first-party surface) already provides -- see
    BUILTIN_SKILL_NAMES above. This is the general form of the specific problem the
    skc-rename-vendored patch fixed for skill-creator."""
    details = []
    for name in list_skill_dirs():
        if name in BUILTIN_SKILL_NAMES:
            details.append("skill dir name collides with a builtin: %s" % name)
    for entry in MANIFESTS:
        manifest = load_manifest(entry["manifest"])
        for s in manifest["skills"]:
            if s["name"] in BUILTIN_SKILL_NAMES:
                details.append("%s: manifest skill name collides with a builtin: %s" % (entry["label"], s["name"]))
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
]


def main():
    self_test_hook = "--self-test-hook" in sys.argv
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
