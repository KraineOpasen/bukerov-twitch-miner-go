#!/usr/bin/env python3
"""validate-agent-governance.py — offline, read-only consistency checks for the
Claude Code Governance v2 layer (.claude/**, docs/agents/**, docs/adr/**).

Stdlib only, no network access, deterministic. Exits 0 if every check passes,
1 if any check fails. Diagnostics print as `[FAIL] check-name: detail` /
`[PASS] check-name` lines.

Usage:
    python3 scripts/validate-agent-governance.py
    python3 scripts/validate-agent-governance.py --self-test-hook

Env vars (both optional, both make specific checks stricter, never required):
    GOVERNANCE_UPSTREAM_DIR  path to a read-only clone of mattpocock/skills;
                             when set, blob-hash checks verify against it directly.
    GOVERNANCE_BASE_SHA      a git ref; when set, the "application paths untouched"
                             check diffs against it instead of the working tree.
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
SETTINGS_PATH = os.path.join(CLAUDE_DIR, "settings.json")
HOOK_PATH = os.path.join(CLAUDE_DIR, "hooks", "governance-policy.py")

ALLOWED_SKILL_KEYS = {"name", "description", "disable-model-invocation", "argument-hint"}
ALLOWED_RULE_KEYS = {"paths"}
FORBIDDEN_VENDOR_NAMES = {".github", ".claude-plugin", "package.json", "package-lock.json", "openai.yaml"}
APPLICATION_PATH_PREFIXES = ("internal/", "cmd/")
APPLICATION_PATH_EXACT_PREFIXES = ("go.mod", "go.sum", "Dockerfile", "docker-compose", ".github/workflows/")

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


def load_manifest():
    with open(MANIFEST_PATH) as f:
        return json.load(f)


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
    ]
    missing = [p for p in required if not os.path.isfile(os.path.join(REPO_ROOT, p))]
    report("required-files-exist", not missing, ["missing %s" % p for p in missing])


def check_json_validity():
    details = []
    for p in (SETTINGS_PATH, MANIFEST_PATH):
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
    manifest = load_manifest()
    names = [s["name"] for s in manifest["skills"]]
    dup_manifest = {n for n in names if names.count(n) > 1}
    details = ["duplicate dir: %s" % d for d in dup] + ["duplicate manifest entry: %s" % n for n in dup_manifest]
    report("unique-skill-names", not details, details)


def check_frontmatter_keys():
    details = []
    for name in list_skill_dirs():
        skillmd = os.path.join(SKILLS_DIR, name, "SKILL.md")
        if not os.path.isfile(skillmd):
            continue
        keys, ok = parse_frontmatter(skillmd)
        if not ok:
            details.append("%s/SKILL.md: no valid frontmatter fence" % name)
            continue
        extra = keys - ALLOWED_SKILL_KEYS
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
    manifest = load_manifest()
    manifest_names = {s["name"] for s in manifest["skills"]}
    fs_names = set(list_skill_dirs())
    only_manifest = manifest_names - fs_names
    only_fs = fs_names - manifest_names
    details = ["in manifest but no dir: %s" % n for n in sorted(only_manifest)]
    details += ["dir but not in manifest: %s" % n for n in sorted(only_fs)]
    report("manifest-filesystem-consistency", not details, details)


def check_excluded_absent():
    manifest = load_manifest()
    excluded_names = {e["name"] for e in manifest.get("excluded", [])}
    fs_names = set(list_skill_dirs())
    present = excluded_names & fs_names
    report("excluded-skills-absent", not present, ["excluded but present: %s" % n for n in sorted(present)])


def check_blob_hashes():
    manifest = load_manifest()
    details = []
    upstream_dir = os.environ.get("GOVERNANCE_UPSTREAM_DIR")
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
PATCH_OPEN_RE = re.compile(r"<!--\s*bukerov-local-patch:\s*([\w-]+)\s*-->")
PATCH_CLOSE_RE = re.compile(r"<!--\s*/bukerov-local-patch:\s*([\w-]+)\s*-->")
PATCH_SELFCLOSING_RE = re.compile(r"<!--\s*bukerov-local-patch:\s*([\w-]+)\s*—[^>]*-->")


def check_patch_marker_balance():
    details = []
    selfclosing_count = 0
    if not os.path.isdir(SKILLS_DIR):
        report("patch-marker-balance", False, ["missing .claude/skills/"])
        return
    for dirpath, _, filenames in os.walk(SKILLS_DIR):
        for fname in filenames:
            if not fname.endswith(".md"):
                continue
            path = os.path.join(dirpath, fname)
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
    report("patch-marker-balance", not details, details)


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
    check_excluded_absent,
    check_blob_hashes,
    check_patch_ledger_coverage,
    check_patch_marker_balance,
    check_application_paths_untouched,
    check_settings_schema,
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
