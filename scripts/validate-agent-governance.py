#!/usr/bin/env python3
"""validate-agent-governance.py — offline, read-only consistency checks for the
Claude Code Governance v3 layer (CLAUDE.md, CONTEXT.md, README.md, .claude/**, docs/agents/**,
docs/adr/**), plus a repo-wide tracked-file scan for the forbidden host-OS token and the
application-path guard over go.mod/go.sum/Dockerfile/docker-compose/.github/workflows/.

Governance v3 is defined by the canonical GOVERNANCE_V3.md at the repository root (owner-approved
rev 3.1; adoption record: docs/adr/0002-canonical-governance-v3.md). The checks named
`governance-v3-*` below enforce that the prose
layer stays mechanically consistent with it: the v3 docs exist, `CLAUDE.md` declares v3, no
Governance-v2 orchestration mandate survives as current policy, `docs/agents/task-contract.md` reads
as an authority envelope rather than an orchestration recipe, no host operating-system name is
used as governance vocabulary anywhere in the tree (modulo HOST_OS_PREEXISTING_BUDGET -- a frozen,
counted exemption for pre-existing occurrences in application files this stable line's governance
authority cannot touch), and GOVERNANCE_V3.md section 7's approved skill inventory, the provider
manifests, the on-disk tree and docs/agents/skills-routing.md all agree exactly.

Stdlib only, no network access, deterministic. Exits 0 if every check passes,
1 if any check fails. Diagnostics print as `[FAIL] check-name: detail` /
`[PASS] check-name` lines.

This script covers every independently vendored skill provider through ONE registry (MANIFESTS,
below). Each entry names that provider's manifest/ledger/policy trio and its `schema`, which selects
the blob-hash granularity: "skill-level" (one upstream_blob_sha per skill's SKILL.md -- mattpocock's
original format) or "file-level" (one upstream_blob_sha plus vendored_blob_sha per vendored FILE).
Every provider added after anthropic uses "file-level": it is the only granularity that can prove a
complete file inventory in both directions. Adding a provider means appending a registry entry, not
adding a conditional inside a check -- the provider-aware checks all iterate the registry.

Usage:
    python3 scripts/validate-agent-governance.py [--application-scope generic]
    python3 scripts/validate-agent-governance.py --application-scope generic --self-test-hook
    GOVERNANCE_BASE_SHA=<full-base-commit> python3 scripts/validate-agent-governance.py \
        --application-scope g1-stable-skills [--self-test-hook]
    python3 scripts/validate-agent-governance.py --self-test
        Runs ONLY this script's own offline fixture matrix -- no network, no sleeps, fully
        deterministic. `_self_test_fixtures()` is the single source of truth for which fixtures
        exist and what each one covers; that list is expected to grow, so it is deliberately not
        duplicated as an id range here. Families, by id prefix: G* positive/structural cases,
        N* negative cases (one per distinct violation class in project-skills-policy.md's schema),
        V* the Governance-v3 prose checks, P* provider-registry cases, W* the stable workflow / CI
        / control-plane integration, and I*/R* the approved inventory / routing-universe contract.

        Fixtures come in two flavours, and BOTH are normal. Most build a synthetic, never-committed
        tree under tempfile.TemporaryDirectory. The rest read the REAL repository, read-only, to
        assert facts about actual repo state -- the vendored MANIFESTS registry's shape, the real
        (empty) project manifest, and each Governance-v3 prose invariant against the real documents.
        The guarantee this flag makes is "this run never WRITES to the repo", not "every fixture is
        synthetic-only".

        Every V* fixture calls the same production helper its corresponding check calls
        (`*_details()`), so a self-test pass means the production logic passed -- the fixtures never
        re-implement a check's conditions. Prints "N/N self-test fixtures passed" and exits 1 on any
        fixture failure, 0 otherwise. Independent of --self-test-hook and of the default run.

Env vars:
    GOVERNANCE_UPSTREAM_DIR_<LABEL>     path to a read-only clone of that provider's upstream at
                                        the pinned commit; when set, that provider's blob-hash check
                                        additionally verifies each unmodified file byte-for-byte
                                        against the clone instead of only against the recorded hash.
                                        <LABEL> is the registry label upper-cased with '-' -> '_':
                                        _MATTPOCOCK, _ANTHROPIC, _COMPOUND_ENGINEERING,
                                        _TRAILOFBITS, _AWESOME_COPILOT, _BUILDERIO.
    GOVERNANCE_UPSTREAM_DIR             legacy fallback for _MATTPOCOCK only, kept for compatibility
                                        with scripts/CI that pre-date the multi-provider registry.
    GOVERNANCE_BASE_SHA                 required only with
                                        `--application-scope g1-stable-skills`; it must name a
                                        full, non-zero lowercase commit object used as the diff
                                        base. Generic validation rejects this variable so an
                                        intended G1 scope check cannot silently downgrade.

A blob-hash mismatch on a file whose skill has `scripts_audited: true` in ANY provider's manifest
means more than "content drifted": it means a script that was read end-to-end during the last
review no longer matches what was reviewed, so re-audit (not just re-hash) is required before
trusting it again.
"""
import argparse
import contextlib
import datetime
import hashlib
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
# Seventh ownership class: project-owned first-party skills. Deliberately NOT added to the
# MANIFESTS registry below -- that registry drives vendored-only logic (excluded-skill keys,
# patch ledgers, upstream blob-hash schemas), none of which apply to first-party content. See
# docs/agents/project-skills-policy.md and check_project_manifest()/validate_project_manifest().
PROJECT_MANIFEST_PATH = os.path.join(DOCS_AGENTS_DIR, "project-skills-manifest.json")
PROJECT_OWNERSHIP_CLASS = "project-first-party"
SETTINGS_PATH = os.path.join(CLAUDE_DIR, "settings.json")
HOOK_PATH = os.path.join(CLAUDE_DIR, "hooks", "governance-policy.py")
STABLE_SKILLS_WORKFLOW_REL = ".github/workflows/stable-skills-maintenance.yml"
STABLE_CI_WORKFLOW_REL = ".github/workflows/ci.yml"
STABLE_RELEASE_WORKFLOW_REL = ".github/workflows/stable-release.yml"
STABLE_SKILLS_MAINTENANCE_DIR = os.path.join(DOCS_AGENTS_DIR, "skills-maintenance")
STABLE_SKILLS_CONTROL_PATHS = (
    "docs/agents/skills-maintenance/policy.json",
    "docs/agents/skills-maintenance/control-plane.json",
    "docs/agents/skills-maintenance/legacy-quarantine.json",
    "docs/agents/skills-maintenance/external-dependencies.json",
    "docs/agents/skills-maintenance/schemas/policy.schema.json",
    "docs/agents/skills-maintenance/schemas/control-plane.schema.json",
    "docs/agents/skills-maintenance/schemas/legacy-quarantine.schema.json",
    "docs/agents/skills-maintenance/schemas/external-dependencies.schema.json",
)
PINNED_CHECKOUT_ACTION = "actions/checkout@11d5960a326750d5838078e36cf38b85af677262"
STABLE_CI_BASELINE_SHA256 = "be5d141995b391da126648f095cd9321a1644ebeddd69b015766be6216129a18"
STABLE_SKILLS_WORKFLOW_SHA256 = (
    "60147affd0f00bbf6dc2e431cb7538ff84455ac1aa43ad066f6b515304971cbb"
)
STABLE_CI_GOVERNANCE_BLOCK_SHA256 = (
    "8a0170032264a1eeb90f3f6385874704eaeec92272de6b4ef6398330670c04d8"
)
STABLE_RELEASE_SHA256 = "de5e499ecb2a65230a37c6fb9bc74ff37a6155584038e4a1a529c92559b01a3a"
PRECHECKOUT_BOOTSTRAP_SHA256 = (
    "2d0c11afc6fd18b3141ee2a211e4dab4496465cfd6afb90112687e9c0ac33148"
)
_PRECHECKOUT_BOOTSTRAP_RE = re.compile(
    r"(?ms)^ {10}python3 -I -S -B <<'PY'\n(?P<body>.*?)^ {10}PY[ \t]*$"
)
_RESERVED_PROVIDER_ENV_PREFIXES = ("ACTIONS_", "GITHUB_", "RUNNER_")
_RESERVED_PROVIDER_ENV_NAMES = frozenset(("CI", "ImageOS", "ImageVersion"))

# --------------------------------------------------------------------------
# Provider registry
# --------------------------------------------------------------------------
#
# ONE registry describes every vendored-skill provider this script knows about, and every
# provider-aware check below is written against the registry rather than against a named provider.
# Adding a provider means appending an entry here (plus its manifest/ledger/policy documents) --
# not adding a conditional inside a check. The two original providers (mattpocock, anthropic) keep
# exactly the guarantees they had before the registry was generalized; the generic checks are the
# same logic with the provider hard-coding lifted out.
#
# Per-entry fields:
#   label                  short provider id, used as the prefix on every diagnostic and as the
#                          ownership-partition key.
#   manifest/patches/policy repo-absolute paths to the three documents every provider must ship.
#   upstream_repo          the canonical upstream URL, cross-checked against the manifest.
#   upstream_env           env var naming a read-only clone of the upstream at the pinned commit;
#                          when set, blob hashes are additionally verified against it directly.
#   legacy_upstream_env    older env-var spelling kept working for scripts that predate the rename.
#   schema                 blob-hash granularity. "skill-level" = one upstream_blob_sha per skill's
#                          SKILL.md (mattpocock's original format). "file-level" = one
#                          upstream_blob_sha + vendored_blob_sha per vendored FILE, in a per-skill
#                          files[] array. Every provider added after anthropic uses "file-level":
#                          it is the only granularity that can prove a complete file inventory.
#   excluded_key           manifest key holding the non-installed candidates' verdict list.
#   extra_frontmatter_keys frontmatter keys this provider's upstream legitimately uses on top of
#                          ALLOWED_SKILL_KEYS. Upstream frontmatter is PRESERVED, not deleted, so
#                          the allowlist is widened per provider rather than the skill being
#                          rewritten (see docs/agents/*-skills-policy.md).
#   license                {"spdx": ..., "layout": "per-skill"|"shared", ...}. "per-skill" requires
#                          `filename` present in every one of that provider's skill dirs; "shared"
#                          requires the single repo-relative `path` to exist.
LICENSE_SHARED_MATTPOCOCK = os.path.join(".claude", "skills", "LICENSE")

MANIFESTS = [
    {
        "label": "mattpocock",
        "manifest": MANIFEST_PATH,
        "patches": PATCHES_PATH,
        "policy": os.path.join(DOCS_AGENTS_DIR, "mattpocock-skills-policy.md"),
        "upstream_repo": "https://github.com/mattpocock/skills",
        "upstream_env": "GOVERNANCE_UPSTREAM_DIR_MATTPOCOCK",
        "legacy_upstream_env": "GOVERNANCE_UPSTREAM_DIR",
        # Migrated from "skill-level" to "file-level": every one of the 42 files in the 23
        # vendored directories now carries its own upstream/vendored hash pair, closing the
        # 17-files-with-no-recorded-hash gap this policy's "Known limitations" documented. All
        # six providers are now file-level, which check_all_providers_file_level enforces.
        "schema": "file-level",
        "excluded_key": "excluded",
        "extra_frontmatter_keys": frozenset(),
        "license": {"spdx": "MIT", "layout": "shared", "path": LICENSE_SHARED_MATTPOCOCK},
    },
    {
        "label": "anthropic",
        "manifest": ANTHROPIC_MANIFEST_PATH,
        "patches": ANTHROPIC_PATCHES_PATH,
        "policy": os.path.join(DOCS_AGENTS_DIR, "anthropic-skills-policy.md"),
        "upstream_repo": "https://github.com/anthropics/skills",
        "upstream_env": "GOVERNANCE_UPSTREAM_DIR_ANTHROPIC",
        "legacy_upstream_env": None,
        "schema": "file-level",
        "excluded_key": "excluded_skills",
        # frontend-design and webapp-testing both carry `license: Complete terms in LICENSE.txt`.
        "extra_frontmatter_keys": frozenset({"license"}),
        "license": {"spdx": "Apache-2.0", "layout": "per-skill", "filename": "LICENSE.txt"},
    },
    {
        "label": "compound-engineering",
        "manifest": os.path.join(DOCS_AGENTS_DIR, "compound-engineering-skills-manifest.json"),
        "patches": os.path.join(DOCS_AGENTS_DIR, "compound-engineering-skills-patches.md"),
        "policy": os.path.join(DOCS_AGENTS_DIR, "compound-engineering-skills-policy.md"),
        "upstream_repo": "https://github.com/EveryInc/compound-engineering-plugin",
        "upstream_env": "GOVERNANCE_UPSTREAM_DIR_COMPOUND_ENGINEERING",
        "legacy_upstream_env": None,
        "schema": "file-level",
        "excluded_key": "excluded_skills",
        # `allowed-tools` is a real Claude Code key that NARROWS a skill's tool surface, so it is
        # preserved rather than stripped (ce-resolve-pr-feedback uses it).
        "extra_frontmatter_keys": frozenset({"allowed-tools"}),
        # MIT requires the notice to accompany all copies, so each vendored skill dir carries its own.
        "license": {"spdx": "MIT", "layout": "per-skill", "filename": "LICENSE"},
    },
    {
        "label": "trailofbits",
        "manifest": os.path.join(DOCS_AGENTS_DIR, "trailofbits-skills-manifest.json"),
        "patches": os.path.join(DOCS_AGENTS_DIR, "trailofbits-skills-patches.md"),
        "policy": os.path.join(DOCS_AGENTS_DIR, "trailofbits-skills-policy.md"),
        "upstream_repo": "https://github.com/trailofbits/skills",
        "upstream_env": "GOVERNANCE_UPSTREAM_DIR_TRAILOFBITS",
        "legacy_upstream_env": None,
        "schema": "file-level",
        "excluded_key": "excluded_skills",
        # `allowed-tools` narrows the tool surface; `type` is Trail of Bits' own skill-kind marker.
        "extra_frontmatter_keys": frozenset({"allowed-tools", "type"}),
        # CC BY-SA 4.0 §3(a)(1) requires the licence notice with every distributed copy.
        "license": {"spdx": "CC-BY-SA-4.0", "layout": "per-skill", "filename": "LICENSE"},
    },
    {
        "label": "awesome-copilot",
        "manifest": os.path.join(DOCS_AGENTS_DIR, "awesome-copilot-skills-manifest.json"),
        "patches": os.path.join(DOCS_AGENTS_DIR, "awesome-copilot-skills-patches.md"),
        "policy": os.path.join(DOCS_AGENTS_DIR, "awesome-copilot-skills-policy.md"),
        "upstream_repo": "https://github.com/github/awesome-copilot",
        "upstream_env": "GOVERNANCE_UPSTREAM_DIR_AWESOME_COPILOT",
        "legacy_upstream_env": None,
        "schema": "file-level",
        "excluded_key": "excluded_skills",
        "extra_frontmatter_keys": frozenset(),
        "license": {"spdx": "MIT", "layout": "per-skill", "filename": "LICENSE"},
    },
    {
        "label": "builderio",
        "manifest": os.path.join(DOCS_AGENTS_DIR, "builderio-skills-manifest.json"),
        "patches": os.path.join(DOCS_AGENTS_DIR, "builderio-skills-patches.md"),
        "policy": os.path.join(DOCS_AGENTS_DIR, "builderio-skills-policy.md"),
        "upstream_repo": "https://github.com/BuilderIO/skills",
        "upstream_env": "GOVERNANCE_UPSTREAM_DIR_BUILDERIO",
        "legacy_upstream_env": None,
        "schema": "file-level",
        "excluded_key": "excluded_skills",
        "extra_frontmatter_keys": frozenset(),
        "license": {"spdx": "MIT", "layout": "per-skill", "filename": "LICENSE"},
    },
]

# Top-level manifest fields every provider manifest must carry, whatever its schema.
REQUIRED_MANIFEST_FIELDS = (
    "upstream_repo", "upstream_commit", "reviewed_at", "reviewed_by",
    "installation_mode", "automatic_updates", "skills",
)
REQUIRED_INSTALLATION_MODE = "project-local-vendored-copy"
SHA1_RE = re.compile(r"^[0-9a-f]{40}$")
# Verdicts an entry in a provider's excluded_key list may carry. HOLD means "valuable but blocked"
# (a mandatory hosted service, an unpinned CLI, a missing redistribution grant); EXCLUDE means
# "reviewed and not wanted". Both must state a reason -- a bare name with no verdict is exactly the
# silent omission the audit process exists to prevent.
ALLOWED_EXCLUSION_STATUSES = {"EXCLUDE", "HOLD"}

ALLOWED_SKILL_KEYS = {"name", "description", "disable-model-invocation", "argument-hint"}
# project-owned first-party skill dirs: an explicit, independent copy of the base set (not the
# same object -- set(...) so a future in-place mutation of one can never leak into the other),
# selected explicitly by ownership in check_frontmatter_keys/validate_project_manifest, never
# reached by an else-fallback.
ALLOWED_SKILL_KEYS_PROJECT = set(ALLOWED_SKILL_KEYS)
ALLOWED_RULE_KEYS = {"paths"}
FORBIDDEN_VENDOR_NAMES = {".github", ".claude-plugin", "package.json", "package-lock.json", "openai.yaml"}
APPLICATION_PATH_PREFIXES = ("internal/", "cmd/")
APPLICATION_PATH_EXACT_PREFIXES = ("go.mod", "go.sum", "Dockerfile", "docker-compose", ".github/workflows/")
APPLICATION_SCOPE_GENERIC = "generic"
APPLICATION_SCOPE_G1_STABLE_SKILLS = "g1-stable-skills"
APPLICATION_SCOPES = (APPLICATION_SCOPE_GENERIC, APPLICATION_SCOPE_G1_STABLE_SKILLS)

# Exact G1.1 workflow integration surfaces. This must remain a literal collection: the independent
# static runtime verifier parses the assignment without importing this validator. Every other
# `.github/workflows/**` path remains an application path. CI is admitted only together with the
# bounded-block check below, which proves that removing the block reproduces stable product CI.
GOVERNANCE_OWNED_WORKFLOWS = (
    ".github/workflows/ci.yml",
    ".github/workflows/stable-skills-maintenance.yml",
)

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


def _unique_json_object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("duplicate JSON key %r" % key)
        value[key] = item
    return value


def _reject_json_constant(value):
    raise ValueError("non-JSON numeric constant %r" % value)


def _strict_json_load(handle):
    return json.load(
        handle,
        object_pairs_hook=_unique_json_object,
        parse_constant=_reject_json_constant,
    )


def _strict_json_loads(raw):
    return json.loads(
        raw,
        object_pairs_hook=_unique_json_object,
        parse_constant=_reject_json_constant,
    )


def list_skill_dirs():
    if not os.path.isdir(SKILLS_DIR):
        return []
    return sorted(
        d for d in os.listdir(SKILLS_DIR)
        if os.path.isdir(os.path.join(SKILLS_DIR, d))
    )


def load_manifest(path=MANIFEST_PATH):
    with open(path) as f:
        return _strict_json_load(f)


def load_anthropic_manifest():
    return load_manifest(ANTHROPIC_MANIFEST_PATH)


def anthropic_skill_dir_names():
    """Directory names (under .claude/skills/) the anthropic manifest claims to own."""
    return {s["name"] for s in load_anthropic_manifest().get("skills", [])}


def mattpocock_skill_dir_names():
    """Directory names (under .claude/skills/) the mattpocock manifest claims to own."""
    return {s["name"] for s in load_manifest().get("skills", [])}


# --- generic provider accessors (every provider-aware check goes through these) --------------

def provider_manifest(entry):
    """The parsed manifest for one registry entry. Raises on unreadable/invalid JSON -- that is
    already reported by check_json_validity, and a check that swallowed it would report a
    misleading PASS."""
    return load_manifest(entry["manifest"])


def provider_skills(entry):
    """The skills[] list for one registry entry, defensively defaulted so a structurally broken
    manifest degrades to "claims nothing" instead of aborting the whole run (the same fail-closed
    shape project_skill_names() uses)."""
    try:
        manifest = provider_manifest(entry)
    except Exception:
        return []
    if not isinstance(manifest, dict):
        return []
    skills = manifest.get("skills", [])
    return skills if isinstance(skills, list) else []


def provider_skill_dir_names(entry):
    """Directory names (under .claude/skills/) one registry entry claims to own."""
    return {s["name"] for s in provider_skills(entry) if isinstance(s, dict) and "name" in s}


def file_level_providers():
    """Registry entries whose manifest records one hash per vendored FILE. These are the only
    providers whose complete on-disk inventory can be proven, so the inventory/mode/scripts-audited
    /patch-coverage checks all iterate exactly this subset."""
    return [e for e in MANIFESTS if e["schema"] == "file-level"]


def provider_upstream_dir(entry):
    """Path to a read-only upstream clone for this provider, from its env var (or the legacy
    spelling), or None when unset. Optional everywhere: setting it makes hash checks STRICTER
    (they additionally compare against the clone), never required."""
    val = os.environ.get(entry["upstream_env"])
    if not val and entry.get("legacy_upstream_env"):
        val = os.environ.get(entry["legacy_upstream_env"])
    return val or None


def allowed_frontmatter_keys_by_dir():
    """{skill dir name -> allowed frontmatter key set}, resolved by OWNERSHIP from the registry
    plus the project manifest. Upstream providers legitimately use keys the original two-provider
    allowlist never saw (`allowed-tools`, `type`, `effort`, `metadata`, `compatibility`, ...); the
    policy is to PRESERVE that frontmatter and widen the allowlist per provider, never to delete
    legitimate upstream frontmatter because an older allowlist did not recognise it. A dir claimed
    by no source is not in the map and falls back to the base set at the call site."""
    by_dir = {}
    for entry in MANIFESTS:
        allowed = ALLOWED_SKILL_KEYS | set(entry.get("extra_frontmatter_keys", ()))
        for name in provider_skill_dir_names(entry):
            by_dir[name] = allowed
    for name in project_skill_names():
        by_dir[name] = ALLOWED_SKILL_KEYS_PROJECT
    return by_dir


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
        "CLAUDE.md", "CONTEXT.md", ".gitignore",
        ".claude/settings.json", ".claude/hooks/governance-policy.py",
        "docs/agents/operation-modes.md", "docs/agents/task-contract.md", "docs/agents/quality-gates.md",
        "docs/agents/issue-tracker.md", "docs/agents/domain.md", "docs/agents/triage-labels.md",
        "docs/adr/0001-agent-governance-v2.md",
        "GOVERNANCE_V3.md",
        "docs/adr/0002-canonical-governance-v3.md",
        "docs/agents/skills-routing.md",
        "docs/agents/skills-update-providers.json",
        "docs/agents/skills-update-plugins.json",
        "scripts/validate-agent-governance.py",
        STABLE_SKILLS_WORKFLOW_REL,
        "scripts/check-skill-updates.py",
        "scripts/prepare-skill-update.py",
        "scripts/skill_updates/runtime.py",
        "scripts/skill_updates/tests/mutation_probe.py",
        "scripts/skill_updates/tests/test_runtime.py",
        "scripts/skill_updates/tests/test_external_dependencies.py",
        "docs/agents/skills-update-automation.md",
        "docs/agents/skills-maintenance/THIRD_PARTY_NOTICES.md",
        "docs/adr/0003-stable-native-deterministic-skill-updates.md",
        "docs/agents/project-skills-manifest.json", "docs/agents/project-skills-policy.md",
    ] + list(STABLE_SKILLS_CONTROL_PATHS)
    # Every registered provider's three documents are required by construction, so adding a
    # provider to the registry cannot leave its manifest/ledger/policy unlisted here.
    for entry in MANIFESTS:
        required += [rel(entry["manifest"]), rel(entry["patches"]), rel(entry["policy"])]
    missing = [p for p in required if not os.path.isfile(os.path.join(REPO_ROOT, p))]
    report("required-files-exist", not missing, ["missing %s" % p for p in missing])


def check_json_validity():
    details = []
    paths = ([SETTINGS_PATH, PROJECT_MANIFEST_PATH]
             + [e["manifest"] for e in MANIFESTS]
             + [os.path.join(REPO_ROOT, path) for path in STABLE_SKILLS_CONTROL_PATHS])
    for p in paths:
        try:
            with open(p) as f:
                _strict_json_load(f)
        except Exception as e:
            details.append("%s: %s" % (rel(p), e))
    report("json-validity", not details, details)


def provider_manifest_field_details(entry, manifest):
    """Pure core of check_provider_manifest_fields (no filesystem, no git): given a registry entry
    and a parsed manifest, return the list of provenance-contract violations. Shared by the
    production check and its self-test fixtures so both exercise identical logic."""
    label = entry["label"]
    details = []
    if not isinstance(manifest, dict):
        return ["%s: manifest top level is not an object" % label]
    for field in REQUIRED_MANIFEST_FIELDS:
        if field not in manifest:
            details.append("%s: manifest missing required field %r" % (label, field))
    # Note the `in ("installation_mode", ...)` presence check above only proves the KEY exists.
    # An explicit `"installation_mode": null` would satisfy that and then slip through a
    # `not in (None, REQUIRED)` value check -- so a manifest could declare no installation mode at
    # all and pass both halves. Compare against the required value directly.
    if manifest.get("installation_mode") != REQUIRED_INSTALLATION_MODE:
        details.append("%s: installation_mode %r != %r" % (
            label, manifest.get("installation_mode"), REQUIRED_INSTALLATION_MODE))
    # The excluded list is the audit trail for every reviewed-but-rejected candidate. A missing or
    # misspelled key would otherwise degrade silently to "this provider rejected nothing", which
    # reads as a complete audit rather than an absent one.
    if entry["excluded_key"] not in manifest:
        details.append("%s: manifest has no %r key -- rejected candidates would go unrecorded" % (
            label, entry["excluded_key"]))
    elif not isinstance(manifest[entry["excluded_key"]], list):
        details.append("%s: %s must be a list, got %s" % (
            label, entry["excluded_key"], type(manifest[entry["excluded_key"]]).__name__))
    if manifest.get("automatic_updates") is not False:
        details.append("%s: automatic_updates must be literally false, got %r" % (
            label, manifest.get("automatic_updates")))
    commit = manifest.get("upstream_commit")
    if not (isinstance(commit, str) and SHA1_RE.match(commit)):
        details.append("%s: upstream_commit %r is not a full 40-hex SHA" % (label, commit))
    declared_repo = manifest.get("upstream_repo")
    if isinstance(declared_repo, str) and _canon_repo(declared_repo) != _canon_repo(entry["upstream_repo"]):
        details.append("%s: manifest upstream_repo %r != registry %r" % (
            label, declared_repo, entry["upstream_repo"]))
    if not entry.get("license", {}).get("spdx"):
        details.append("%s: registry entry declares no license SPDX id" % label)
    return details


def _canon_repo(url):
    return url.rstrip("/").removesuffix(".git")


def check_provider_manifest_fields():
    """Every provider manifest must carry the same top-level provenance contract, whatever its
    blob-hash schema: the required fields exist, `installation_mode` is the project-local vendored
    copy (never a marketplace/live-plugin install), `automatic_updates` is literally false (nothing
    re-fetches upstream on its own), `upstream_commit` is a full 40-hex SHA (never a branch name or
    a short SHA -- a floating ref would make every recorded hash unfalsifiable), the declared
    `upstream_repo` matches the registry, and a license block names an SPDX id."""
    details = []
    for entry in MANIFESTS:
        try:
            manifest = provider_manifest(entry)
        except Exception as e:
            details.append("%s: manifest unreadable: %s" % (entry["label"], e))
            continue
        details += provider_manifest_field_details(entry, manifest)
    report("provider-manifest-fields", not details, details)


def provider_license_file_details(entry, skill_names, skills_dir, repo_root):
    """Pure-ish core of check_provider_license_files: root-parameterized so a fixture can point it
    at a synthetic tree."""
    lic = entry.get("license", {})
    layout = lic.get("layout")
    if layout == "shared":
        if not os.path.isfile(os.path.join(repo_root, lic["path"])):
            return ["%s: shared license file missing: %s" % (entry["label"], lic["path"])]
        return []
    if layout == "per-skill":
        fname = lic["filename"]
        return ["%s: %s/%s missing (per-skill %s notice)" % (entry["label"], name, fname, lic.get("spdx"))
                for name in sorted(skill_names)
                if not os.path.isfile(os.path.join(skills_dir, name, fname))]
    return ["%s: unknown license layout %r" % (entry["label"], layout)]


def check_provider_license_files():
    """The redistribution notice each provider's license actually requires must be present on disk.
    "per-skill" layouts (Apache-2.0 §4(a), CC BY-SA 4.0 §3(a)(1) attribution) need the license file
    inside EVERY one of that provider's skill directories; "shared" layouts need the single
    declared file. A vendored tree that lost its license file is a redistribution defect, not a
    cosmetic one."""
    details = []
    for entry in MANIFESTS:
        details += provider_license_file_details(
            entry, provider_skill_dir_names(entry), SKILLS_DIR, REPO_ROOT)
    report("provider-license-files", not details, details)


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
    # Union across ALL ownership sources (the six vendored manifests plus the project
    # manifest): a name duplicated within one source OR appearing in more than one source is
    # equally a collision -- two different skills must never share a name.
    names = []
    for entry in MANIFESTS:
        names.extend(s["name"] for s in provider_skills(entry)
                     if isinstance(s, dict) and isinstance(s.get("name"), str))
    names.extend(project_skill_names())
    dup_manifest = {n for n in names if names.count(n) > 1}
    details = ["duplicate dir: %s" % d for d in dup] + ["duplicate manifest entry (union of all manifests): %s" % n for n in dup_manifest]
    report("unique-skill-names", not details, details)


def check_frontmatter_keys():
    details = []
    # Allowlist selected explicitly BY OWNERSHIP, from the provider registry plus the project
    # manifest (see allowed_frontmatter_keys_by_dir). A dir claimed by no source falls back to the
    # base set -- which is the strictest of them, so an unclaimed dir can never gain keys by
    # accident (and check_manifest_ownership_partition fails on it independently anyway).
    allowed_by_dir = allowed_frontmatter_keys_by_dir()
    for name in list_skill_dirs():
        skillmd = os.path.join(SKILLS_DIR, name, "SKILL.md")
        if not os.path.isfile(skillmd):
            continue
        keys, ok = parse_frontmatter(skillmd)
        if not ok:
            details.append("%s/SKILL.md: no valid frontmatter fence" % name)
            continue
        allowed = allowed_by_dir.get(name, ALLOWED_SKILL_KEYS)
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
# Inline code spans. Deliberately single-line: a span is matched only within one line, so an odd
# number of backticks on some line (upstream really does write things like `](` as a code span)
# cannot swallow the rest of the file and expose unrelated text as if it were prose. Multi-line
# spans are legal CommonMark but vanishingly rare in these skills, and mis-pairing across lines
# produced exactly the phantom "dangling link" findings this form prevents.
INLINE_CODE_RE = re.compile(r"(`+)(?:(?!\1)[^\n])*?\1")


FENCE_LINE_RE = re.compile(r"^ {0,3}(`{3,}|~{3,})(.*)$")


def strip_fenced_blocks(text):
    """Drop ONLY fenced code blocks, keeping inline code spans intact.

    Line-based and fence-length aware, following CommonMark: a fence is closed only by a fence of
    the SAME character that is at least as long as the opener. The previous regex form
    (```` ```.*?``` ```` with DOTALL) could not express that, so a document containing a nested
    four-backtick fence -- the standard way to show a fenced block inside a fenced block, and
    exactly what these skills do when documenting their own output format -- had its fences paired
    wrongly. Everything after the mis-pair fell outside any recognised fence, and prose that merely
    QUOTED a Markdown link was then checked as if it were a real link. That produced phantom
    "dangling link" findings whose only cheap fix was editing correct upstream prose, which is the
    pressure the vendoring policies exist to remove.

    Used by the dependency-closure check, which deliberately reads backticked paths
    (`scripts/context.mjs`) as closure claims -- stripping inline code there would blind it.
    Link checking wants the stronger strip_code_fences() below."""
    out, fence = [], None
    for line in text.split("\n"):
        m = FENCE_LINE_RE.match(line)
        if fence is None:
            # An opening backtick fence may not carry a backtick in its info string (CommonMark);
            # that rule is what keeps a line like ``a `b` `` from being read as a fence opener.
            if m and not (m.group(1)[0] == "`" and "`" in m.group(2)):
                fence = m.group(1)
                continue
            out.append(line)
        else:
            if (m and m.group(1)[0] == fence[0] and len(m.group(1)) >= len(fence)
                    and not m.group(2).strip()):
                fence = None
    return "\n".join(out)


def strip_code_fences(text):
    """Drop fenced code blocks AND inline code spans, so text that merely *shows* link syntax is
    not checked as if it were a real link.

    Both halves matter. The fenced half keeps an example inside a ```md template from being read as
    a live link. The inline half is the same rule for `[T01.S](2-stride-analysis.md#anchor)` written
    in backticks: a skill documenting the link syntax its OUTPUT must use is quoting a string, not
    referencing a file that has to exist. Without this, every such quotation is a false "dangling
    link" -- and the pressure that creates is to edit correct upstream prose to satisfy the
    checker, which the vendoring policies exist to prevent."""
    return INLINE_CODE_RE.sub("", strip_fenced_blocks(text))


# Trail of Bits' skills write intra-skill paths as `{baseDir}/references/foo.md`. `{baseDir}` is
# their own documented convention for "this skill's own directory" (upstream AGENTS.md: "Use
# `{baseDir}` for paths, **never hardcode** absolute paths"), not a loader-substituted variable, so
# a vendored copy keeps the token verbatim and the file really does live at <skill>/references/foo.md.
#
# The validator resolves the token instead of the vendoring rewriting it. That is deliberate: the
# alternative -- editing hundreds of upstream lines to strip a prefix -- would be a large,
# content-touching patch made solely to satisfy an older link resolver, exactly the kind of change
# the vendoring policies forbid. Resolving it here keeps upstream bytes intact AND makes the check
# stronger, because these targets are now actually verified rather than skipped.
BASEDIR_TOKEN = "{baseDir}"


def resolve_skill_link(target, dirpath, skill_dir):
    """(kind, resolved_abs_path) for one Markdown link target inside a vendored skill.

    kind is "skip" (external/anchor-only/bare token), "absolute" (a `/`-rooted path, always a
    finding), or "path" (resolved_abs_path is what must exist). A `#fragment` suffix is stripped
    before resolution -- a link to a heading inside a real file is a valid link."""
    target = target.split(" ", 1)[0].strip()
    if not target or target.startswith(("http://", "https://", "mailto:", "#")):
        return "skip", None
    if target == BASEDIR_TOKEN:
        return "skip", None
    if target.startswith("/"):
        return "absolute", None
    if target.startswith(BASEDIR_TOKEN + "/"):
        base, rest = skill_dir, target[len(BASEDIR_TOKEN) + 1:]
    else:
        base, rest = dirpath, target
    rest = rest.split("#", 1)[0]
    if not rest:
        return "skip", None
    return "path", os.path.normpath(os.path.join(base, rest))


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
                for raw in LINK_RE.findall(text):
                    kind, resolved = resolve_skill_link(raw, dirpath, skill_dir)
                    if kind == "skip":
                        continue
                    if kind == "absolute":
                        details.append("%s: absolute-path link %r" % (rel(path), raw.strip()))
                        continue
                    # os.path.exists, not isfile: a link to a DIRECTORY inside the skill (e.g.
                    # `./references/skeletons/`) is a legitimate reference, and treating it as
                    # dangling was a bug in the original check that only surfaced once a provider
                    # shipping directory links was vendored.
                    if not os.path.exists(resolved):
                        details.append("%s: dangling link %r" % (rel(path), raw.strip()))
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
    name. With seven ownership sources now in play (the six vendored providers and the project
    manifest), a directory legitimately claimed by one source must not be flagged as "not in
    manifest" when checking another -- so each source's "dir but not in manifest" half only
    considers directories no OTHER source already claims. Directories claimed by no source at all
    are still caught here (via every source independently), and again, more directly, by
    check_manifest_ownership_partition below. The project manifest is read directly
    (project_skill_names()), never via the vendored MANIFESTS registry."""
    fs_names = set(list_skill_dirs())
    manifest_names_by_label = {}
    for entry in MANIFESTS:
        manifest_names_by_label[entry["label"]] = provider_skill_dir_names(entry)
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
    """Every .claude/skills/<dir> must be claimed by EXACTLY one of the registered ownership sources
    (the six vendored providers + project): the union of all their skill names must equal the set of
    directories on disk, and no name may appear in more than one source (a cross-source name
    collision would mean two different reviewed owners both think they own the same directory).
    The project manifest is read directly (project_skill_names()), never via the vendored
    MANIFESTS registry. Core logic lives in partition_details() above."""
    fs_names = set(list_skill_dirs())
    names_by_label = {}
    for entry in MANIFESTS:
        names_by_label[entry["label"]] = provider_skill_dir_names(entry)
    names_by_label["project"] = set(project_skill_names())  # set() needed: partition_details()
    # does set algebra (&, -), and project_skill_names() is deliberately a non-deduplicated list.

    details = partition_details(fs_names, names_by_label)
    report("manifest-ownership-partition", not details, details)


def exclusion_entry_details(label, key, excluded, fs_names):
    """Pure core of check_excluded_absent's per-entry validation: a rejected candidate must not
    also be installed, must carry a non-blank reason, and (when it states one) must use a
    recognised verdict. Shared with the self-test fixture so both exercise identical logic."""
    if not isinstance(excluded, list):
        return ["%s: %s must be a list" % (label, key)]
    details = []
    for item in excluded:
        if not isinstance(item, dict) or not isinstance(item.get("name"), str):
            details.append("%s: malformed exclusion entry %r" % (label, item))
            continue
        name = item["name"]
        if name in fs_names:
            details.append("%s: excluded but present: %s" % (label, name))
        if not (isinstance(item.get("reason"), str) and item["reason"].strip()):
            details.append("%s: exclusion %r has no reason" % (label, name))
        status = item.get("status")
        if status is not None and status not in ALLOWED_EXCLUSION_STATUSES:
            details.append("%s: exclusion %r has status %r, expected one of %s" % (
                label, name, status, sorted(ALLOWED_EXCLUSION_STATUSES)))
    return details


def check_excluded_absent():
    """A candidate the review REJECTED must not also be installed, and every rejection must carry
    a verdict a human can audit: a non-empty `reason`, and (when present) a `status` of EXCLUDE or
    HOLD. HOLD records "valuable but blocked" -- a mandatory hosted service, an unpinned CLI, a
    missing redistribution grant -- so that a blocker is documented rather than silently omitted."""
    fs_names = set(list_skill_dirs())
    details = []
    for entry in MANIFESTS:
        label = entry["label"]
        try:
            manifest = provider_manifest(entry)
        except Exception:
            continue  # reported by check_json_validity / check_provider_manifest_fields
        # Compare a provider's rejections against the dirs THAT PROVIDER owns, not against every
        # dir on disk. Two upstreams can ship same-named skills (github/awesome-copilot and
        # trailofbits both publish a `codeql`); rejecting one while installing the other is a
        # deliberate, recorded choice, not a contradiction. Global name uniqueness is still
        # enforced -- by check_unique_skill_names and check_manifest_ownership_partition, which is
        # where that invariant belongs.
        details += exclusion_entry_details(
            label, entry["excluded_key"], manifest.get(entry["excluded_key"], []),
            provider_skill_dir_names(entry))

        # A skill vendored under a different directory name than upstream's (to dodge a builtin
        # collision) must not ALSO leave the pre-rename name on disk -- that would shadow the
        # builtin the rename existed to protect. Generic across providers, not anthropic-only.
        renamed_froms = {
            s["renamed_from"] for s in provider_skills(entry)
            if isinstance(s, dict) and s.get("renamed_from")
        }
        for n in sorted(renamed_froms & fs_names):
            details.append("%s: renamed_from source present as dir: %s" % (label, n))

    report("excluded-skills-absent", not details, details)


def all_providers_file_level_details(manifests):
    """Root-parameterized core of check_all_providers_file_level."""
    return ["%s: schema is %r, not \"file-level\"" % (entry["label"], entry.get("schema"))
            for entry in manifests if entry.get("schema") != "file-level"]


def check_all_providers_file_level():
    """Every provider in the registry must record one hash pair per FILE.

    This replaces the old mattpocock-only `check_blob_hashes`, which verified a single
    SKILL.md hash per skill and skipped every locally-modified skill -- so of that provider's
    42 files it could speak for 4. Migrating mattpocock to the file-level schema made
    `check_provider_file_hashes` (which proves the inventory in BOTH directions, covers patched
    and local-origin files, and catches any undeclared file on disk) apply to all six
    providers, strictly subsuming what the old check did.

    Keeping this as a check rather than deleting the concept is the point: `file_level_providers()`
    silently skips any entry whose schema is something else, so a future provider added on a
    retired schema would get NO hash coverage and every hash check would still report PASS.
    This fails closed on that instead."""
    details = all_providers_file_level_details(MANIFESTS)
    report("all-providers-file-level", not details, details)


#: Key an automated update candidate carries in a provider manifest. See
#: scripts/skill_updates/candidate.py and docs/agents/skills-update-automation.md.
CANDIDATE_KEY = "automated_candidate"

#: The candidate state machine, mirrored from scripts/skill_updates/states.py. Only
#: PREPARED_AUDIT_REQUIRED may ever appear in a checked-in manifest written by automation;
#: AUDITED is what a human establishes, and it is established by DELETING the block, never by
#: writing the word.
CANDIDATE_STATE_PREPARED = "PREPARED_AUDIT_REQUIRED"
CANDIDATE_STATE_AUDITED = "AUDITED"


def unaudited_candidate_details(manifests):
    """Root-parameterized core of check_no_unaudited_candidate.

    Note what is checked: the PRESENCE of the block, not its `state` value. A candidate that
    rewrote its own state to AUDITED is caught by exactly the same rule as one that left it
    alone -- there is no string a machine can write into this block that makes it pass. The
    state value is still reported, and an AUDITED claim is called out separately, because a
    manifest asserting it is either a bug in the bot or a hand-edit that took the wrong route.
    """
    details = []
    for entry in manifests:
        try:
            manifest = provider_manifest(entry)
        except Exception:
            continue  # reported by check_json_validity
        block = manifest.get(CANDIDATE_KEY)
        if not isinstance(block, dict):
            continue
        state = block.get("state")
        if state == CANDIDATE_STATE_AUDITED:
            details.append(
                "%s: %s block claims state=%r. AUDITED is never written into this block -- it is "
                "established by auditing the diff, recording a fresh reviewed_at/reviewed_by, "
                "and DELETING the block. A manifest that writes the word instead is asserting a "
                "review through the one route that is not allowed to grant it."
                % (entry["label"], CANDIDATE_KEY, state))
            continue
        details.append(
            "%s: manifest carries an unaudited %s block (state=%r, %s -> %s). This is a "
            "machine-prepared candidate, not a reviewed pin: audit the diff, record a fresh "
            "reviewed_at/reviewed_by, and delete the block.%s" % (
                entry["label"], CANDIDATE_KEY, state,
                str(block.get("superseded_commit"))[:12], str(block.get("target_commit"))[:12],
                (" EVAL_REQUIRED: %d changed file(s) can alter behaviour, so provenance alone "
                 "does not establish equivalence -- run the behavioural comparison before "
                 "clearing." % len(block["eval_required"]))
                if isinstance(block.get("eval_required"), list) and block["eval_required"]
                else ""))
    return details


def check_no_unaudited_candidate():
    """No provider manifest may claim a pin that only a machine has ever looked at.

    The stable scheduler (.github/workflows/stable-skills-maintenance.yml) can prepare a mechanically
    clean refresh, but it has no standing to establish that the new upstream content is
    something this project wants to run. It marks every candidate with an `automated_candidate`
    block, and this check FAILS while that block is present.

    That is what makes "audited" unfakeable rather than merely asserted in a PR body: the only
    way to a passing governance run is for a human -- or an agent under a task contract -- to
    read the diff, record a fresh reviewed_at/reviewed_by, and remove the block deliberately.
    An automated candidate therefore cannot masquerade as a reviewed pin no matter how clean
    its merge was."""
    details = unaudited_candidate_details(MANIFESTS)
    report("no-unaudited-update-candidate", not details, details)


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


def _git_blob_sha(path):
    """(sha, error) for one file. Shells out to `git hash-object` exactly as the original
    per-provider checks did, so the recorded hashes stay directly reproducible by hand."""
    try:
        out = subprocess.run(["git", "hash-object", path], stdout=subprocess.PIPE,
                              stderr=subprocess.PIPE, timeout=5, text=True)
    except Exception as e:
        return None, "could not run git hash-object (%s)" % e
    if out.returncode != 0:
        return None, "git hash-object failed"
    return out.stdout.strip(), None


def check_provider_file_hashes():
    """File-level integrity, for EVERY file-level provider in the registry (this is the
    generalization of the original anthropic-only check; mattpocock keeps its skill-level
    check_blob_hashes above). For every file in every skill's files[]: the file must exist; its
    on-disk `git hash-object` must equal the recorded `vendored_blob_sha` (the integrity pin that
    covers patched and local-origin files too); an unmodified upstream-origin file must ALSO equal
    its `upstream_blob_sha`; a locally-modified file must name at least one patch id; a
    local-origin file must carry a reason. When the provider's upstream-clone env var is set, an
    unmodified file is additionally compared byte-for-byte against the clone.

    The inventory half is what makes the file list PROVABLY complete in both directions: a listed
    path missing on disk fails, and any on-disk file under a claimed skill directory that files[]
    does not mention fails. An unreviewed file with no manifest entry is exactly the drift this
    exists to catch.

    A `vendored_blob_sha` mismatch on a file whose skill has `scripts_audited: true` means a script
    that was read end-to-end during review no longer matches what was reviewed: re-audit is
    required, not just a manifest hash bump."""
    details = []
    verified_against_clone = []
    for entry in file_level_providers():
        upstream_dir = provider_upstream_dir(entry)
        if upstream_dir:
            verified_against_clone.append(entry["label"])
        details += provider_file_hash_details(
            entry, provider_skills(entry), REPO_ROOT, upstream_dir)
    if verified_against_clone:
        print("  (info) file hashes additionally verified against an upstream clone for: %s" %
              ", ".join(sorted(verified_against_clone)))
    report("provider-file-hashes", not details, details)


def provider_file_hash_details(entry, skills, repo_root, upstream_dir=None):
    """Root-parameterized core of check_provider_file_hashes, so a fixture can drive it against a
    synthetic tree and prove each failure mode really fails."""
    label = entry["label"]
    details = []
    for skill in skills:
        if not isinstance(skill, dict) or "name" not in skill:
            details.append("%s: malformed skills[] entry %r" % (label, skill))
            continue
        sname = skill["name"]
        listed_paths = set()
        for fentry in skill.get("files", []):
            if not isinstance(fentry, dict) or not isinstance(fentry.get("path"), str):
                details.append("%s: %s: malformed files[] entry %r" % (label, sname, fentry))
                continue
            rel_path = fentry["path"]
            listed_paths.add(rel_path)
            local_path = os.path.join(repo_root, rel_path)
            if not os.path.isfile(local_path):
                details.append("%s: %s: %s: listed in files[] but missing on disk" % (
                    label, sname, rel_path))
                continue

            local_sha, err = _git_blob_sha(local_path)
            if err:
                details.append("%s: %s: %s: %s" % (label, sname, rel_path, err))
                continue

            vendored_sha = fentry.get("vendored_blob_sha")
            if not vendored_sha:
                details.append("%s: %s: %s: missing vendored_blob_sha in manifest" % (
                    label, sname, rel_path))
            elif local_sha != vendored_sha:
                details.append("%s: %s: %s: local blob %s != manifest vendored_blob_sha %s"
                               " (if this file's skill has scripts_audited=true, re-audit is"
                               " required, not just re-hashing)" % (
                                   label, sname, rel_path, local_sha, vendored_sha))

            if fentry.get("origin") == "local":
                if not fentry.get("reason"):
                    details.append("%s: %s: %s: origin=local but no 'reason' given" % (
                        label, sname, rel_path))
                continue

            if fentry.get("locally_modified"):
                if not fentry.get("patch_ids"):
                    details.append("%s: %s: %s: locally_modified=true but patch_ids is empty" % (
                        label, sname, rel_path))
                continue

            if local_sha != fentry.get("upstream_blob_sha"):
                details.append("%s: %s: %s: local blob %s != manifest upstream_blob_sha %s"
                               " (if this file's skill has scripts_audited=true, re-audit is"
                               " required, not just re-hashing)" % (
                                   label, sname, rel_path, local_sha,
                                   fentry.get("upstream_blob_sha")))
            if upstream_dir and fentry.get("upstream_path"):
                upstream_path = os.path.join(upstream_dir, fentry["upstream_path"])
                if os.path.isfile(upstream_path):
                    up_sha, up_err = _git_blob_sha(upstream_path)
                    if up_err is None and up_sha != local_sha:
                        details.append("%s: %s: %s: local blob differs from %s" % (
                            label, sname, rel_path, upstream_path))

        skill_path = skill.get("path")
        skill_dir = os.path.join(repo_root, skill_path) if skill_path else None
        if skill_dir and os.path.isdir(skill_dir):
            for dirpath, dirnames, filenames in os.walk(skill_dir):
                # Running a vendored skill's own scripts (e.g. `python -m scripts.foo`) creates
                # __pycache__/*.pyc inside the skill dir; .gitignore covers __pycache__, so these
                # can never be committed and are not supply-chain drift. Skip narrowly by name
                # only, so this stays fail-closed for everything actually committable.
                dirnames[:] = [d for d in dirnames if d != "__pycache__"]
                for fname in filenames:
                    if fname.endswith(".pyc"):
                        continue
                    disk_rel = os.path.relpath(os.path.join(dirpath, fname), repo_root)
                    if disk_rel not in listed_paths:
                        details.append("%s: %s: %s: present on disk but not listed in files[]" % (
                            label, sname, disk_rel))
    return details


def provider_vendored_mode_details(entry, skills, repo_root):
    """Root-parameterized core of check_provider_vendored_modes."""
    label = entry["label"]
    details = []
    for skill in skills:
        if not isinstance(skill, dict):
            continue
        sname = skill.get("name")
        for fentry in skill.get("files", []):
            if not isinstance(fentry, dict) or not isinstance(fentry.get("path"), str):
                continue
            rel_path = fentry["path"]
            vendored_mode = fentry.get("vendored_mode")
            if vendored_mode != "100644":
                details.append("%s: %s: %s: manifest vendored_mode %r != \"100644\"" % (
                    label, sname, rel_path, vendored_mode))
            upstream_mode = fentry.get("upstream_mode")
            if (fentry.get("origin") == "upstream" and upstream_mode is not None
                    and upstream_mode != vendored_mode and not fentry.get("patch_ids")):
                details.append(
                    "%s: %s: %s: upstream_mode %r normalized to %r with no patch id documenting it"
                    % (label, sname, rel_path, upstream_mode, vendored_mode))
            local_path = os.path.join(repo_root, rel_path)
            if not os.path.isfile(local_path):
                continue  # already reported by check_provider_file_hashes
            if os.stat(local_path).st_mode & 0o111:
                details.append("%s: %s: %s: executable bit set on disk" % (label, sname, rel_path))
    return details


def check_provider_vendored_modes():
    """Vendored files are content an agent READS (and runs explicitly, e.g. `python3 <path>`),
    never a binary invoked straight off disk, so every file-level provider's files[] must record
    `vendored_mode: "100644"` and carry no executable bit on disk. Upstream frequently ships
    scripts 100755; normalizing the mode is a real change to the vendored artifact, so when
    `upstream_mode` differs from `vendored_mode` the entry must name the patch id that documents
    the normalization -- an undocumented mode change is exactly the "unexpected executable-bit
    drift" this check exists to make impossible in either direction."""
    details = []
    for entry in file_level_providers():
        details += provider_vendored_mode_details(entry, provider_skills(entry), REPO_ROOT)
    report("provider-vendored-modes", not details, details)


SCRIPT_EXTS = (".py", ".html", ".sh", ".mjs", ".js", ".bash", ".zsh")


def provider_scripts_audited_details(entry, skills, repo_root):
    """Root-parameterized core of check_provider_scripts_audited."""
    label = entry["label"]
    details = []
    for skill in skills:
        if not isinstance(skill, dict):
            continue
        scripts = []
        for fentry in skill.get("files", []):
            if not isinstance(fentry, dict) or not isinstance(fentry.get("path"), str):
                continue
            p = fentry["path"]
            if p.endswith(SCRIPT_EXTS):
                scripts.append(p)
            elif "." not in os.path.basename(p):
                # _has_shebang returns (is_shebang, error) -- unpack it. Testing the tuple itself
                # would be truthy for EVERY extensionless file (a LICENSE would read as a script).
                is_shebang, err = _has_shebang(os.path.join(repo_root, p))
                if err:
                    details.append("%s: %s: %s: could not read to classify: %s" % (
                        label, skill.get("name"), p, err))
                elif is_shebang:
                    scripts.append(p)
        if scripts and not skill.get("scripts_audited"):
            details.append("%s: %s: ships %d script file(s) (e.g. %s) but scripts_audited is not true"
                           % (label, skill.get("name"), len(scripts), sorted(scripts)[0]))
    return details


def check_provider_scripts_audited():
    """Any file-level provider skill whose files[] contains executable-ish content -- a script in
    any of the languages these providers actually ship, or an extensionless file with a shebang --
    must record `scripts_audited: true`, meaning the file was read END TO END during review rather
    than trusted from its SKILL.md description. Prose-only skills are exempt: there is nothing to
    audit. The extension list is deliberately wider than the original anthropic-only check's
    (.py/.html/.sh), because the newer providers ship .mjs/.js and extensionless shebang scripts."""
    details = []
    for entry in file_level_providers():
        details += provider_scripts_audited_details(entry, provider_skills(entry), REPO_ROOT)
    report("provider-scripts-audited", not details, details)


def check_builtin_collision_denylist():
    """No vendored or first-party skill directory name and no manifest skill name (across any of
    the registered ownership sources) may collide with a name Claude Code (or another first-party
    surface) already provides -- see BUILTIN_SKILL_NAMES above. This is the general form of the
    specific problem the skc-rename-vendored patch fixed for skill-creator. The project manifest
    is checked directly (project_skill_names()), never via the vendored MANIFESTS registry."""
    details = []
    for name in list_skill_dirs():
        if name in BUILTIN_SKILL_NAMES:
            details.append("skill dir name collides with a builtin: %s" % name)
    for entry in MANIFESTS:
        for name in sorted(provider_skill_dir_names(entry)):
            if name in BUILTIN_SKILL_NAMES:
                details.append("%s: manifest skill name collides with a builtin: %s" % (entry["label"], name))
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
#   - Shell scripts (.sh, and extensionless files with a shell shebang) use the same single-line
#     `#` form as Python -- `#` is a comment in both, so PY_MARK_RE covers them unchanged.
#   - JavaScript/ESM (.mjs, .js) has no `#` comment, so it uses the equivalent `//` form:
#     `// bukerov-local-patch: <id> — <note>`. Like the Python form it is single-line only and
#     has no balance concept.
PATCH_OPEN_RE = re.compile(r"<!--\s*bukerov-local-patch:\s*([\w-]+)\s*-->")
PATCH_CLOSE_RE = re.compile(r"<!--\s*/bukerov-local-patch:\s*([\w-]+)\s*-->")
PATCH_SELFCLOSING_RE = re.compile(r"<!--\s*bukerov-local-patch:\s*([\w-]+)\s*—[^>]*-->")
PY_MARK_RE = re.compile(r"#\s*bukerov-local-patch:\s*([\w-]+)")
JS_MARK_RE = re.compile(r"//\s*bukerov-local-patch:\s*([\w-]+)")

# Extensions carrying the single-line `#` marker form (no open/close balance).
HASH_MARK_EXTS = (".py", ".sh", ".bash", ".zsh", ".yaml", ".yml")
# Extensions carrying the single-line `//` marker form.
SLASH_MARK_EXTS = (".mjs", ".js", ".ts")
# Extensions carrying the wrapping HTML-comment marker pair.
WRAP_MARK_EXTS = (".md", ".html")


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
            if fname.endswith(HASH_MARK_EXTS) or fname.endswith(SLASH_MARK_EXTS):
                with open(path, encoding="utf-8", errors="replace") as f:
                    text = f.read()
                py_mark_count += len(PY_MARK_RE.findall(text)) + len(JS_MARK_RE.findall(text))
                continue
            if not fname.endswith(WRAP_MARK_EXTS):
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
            if fname.endswith(WRAP_MARK_EXTS):
                with open(path, encoding="utf-8", errors="replace") as f:
                    text = f.read()
                ids.update(PATCH_OPEN_RE.findall(text))
                ids.update(PATCH_SELFCLOSING_RE.findall(text))
                continue
            if fname.endswith(HASH_MARK_EXTS):
                with open(path, encoding="utf-8", errors="replace") as f:
                    ids.update(PY_MARK_RE.findall(f.read()))
                continue
            if fname.endswith(SLASH_MARK_EXTS):
                with open(path, encoding="utf-8", errors="replace") as f:
                    ids.update(JS_MARK_RE.findall(f.read()))
                continue
            # Extensionless bundled scripts (e.g. CE's `pr-snapshot`) carry the `#` form; detect
            # them by shebang rather than by name so a patched one can never go uncounted.
            # _has_shebang returns (is_shebang, error) -- unpack it; the bare tuple is always truthy.
            if "." not in fname and _has_shebang(path)[0]:
                with open(path, encoding="utf-8", errors="replace") as f:
                    ids.update(PY_MARK_RE.findall(f.read()))
    return ids


def check_provider_patch_coverage():
    """Bidirectional patch coverage for EVERY file-level provider (the generalization of the
    original anthropic-only check; mattpocock's skill-level equivalent is
    check_patch_ledger_coverage above, unchanged):
      - every marker id found anywhere inside a provider-owned skill directory must appear in that
        provider's ledger AND in some file's files[].patch_ids;
      - every files[].patch_ids id must appear in that provider's ledger -- a manifest entry
        claiming a patch id the ledger never documents is exactly the drift a ledger prevents.
    Each provider is checked against ITS OWN ledger: an id documented in a different provider's
    ledger does not count, or the ledgers would silently cross-cover each other."""
    details = []
    for entry in file_level_providers():
        try:
            with open(entry["patches"], encoding="utf-8") as f:
                ledger_text = f.read()
        except Exception as e:
            details.append("%s: patch ledger unreadable: %s" % (entry["label"], e))
            continue
        details += provider_patch_coverage_details(
            entry, provider_skills(entry), ledger_text, REPO_ROOT, rel(entry["patches"]))
    report("provider-patch-marker-coverage", not details, details)


def provider_patch_coverage_details(entry, skills, ledger_text, repo_root, ledger_name):
    """Root-parameterized core of check_provider_patch_coverage."""
    label = entry["label"]
    details = []
    manifest_patch_ids = set()
    for skill in skills:
        if not isinstance(skill, dict):
            continue
        for fentry in skill.get("files", []):
            if isinstance(fentry, dict):
                manifest_patch_ids.update(fentry.get("patch_ids", []))

    for skill in skills:
        if not isinstance(skill, dict) or not skill.get("path"):
            continue
        skill_dir = os.path.join(repo_root, skill["path"])
        if not os.path.isdir(skill_dir):
            continue  # already reported elsewhere
        for pid in sorted(find_all_patch_marker_ids(skill_dir)):
            if pid not in ledger_text:
                details.append("%s: %s: marker id %r found in-file but missing from %s" % (
                    label, skill["name"], pid, ledger_name))
            if pid not in manifest_patch_ids:
                details.append("%s: %s: marker id %r found in-file but not recorded in any"
                               " files[].patch_ids" % (label, skill["name"], pid))

    for pid in sorted(manifest_patch_ids):
        if pid not in ledger_text:
            details.append("%s: manifest patch_id %r not found in %s" % (label, pid, ledger_name))
    return details


# Directory names a vendored skill uses for its own dependency closure. A path reference in a
# skill's Markdown is only checked when its FIRST segment is one of these AND that directory
# actually exists in the skill -- which keeps illustrative prose ("put it in scripts/") from being
# read as a claim about a file, while still catching a real reference to a file that was not
# vendored.
CLOSURE_DIRS = ("scripts", "references", "assets", "examples", "agents", "templates", "evals",
                "hooks", "reference", "resources", "commands", "rules", "prompts")
CLOSURE_REF_RE = re.compile(
    r"`(?:\{baseDir\}/)?([A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+)`")


def invocation_consistency_details(entry, skills, skills_dir):
    """Pure core of check_provider_invocation_matches_frontmatter, root-parameterized for fixtures."""
    label = entry["label"]
    details = []
    for skill in skills:
        if not isinstance(skill, dict) or "name" not in skill:
            continue
        declared = skill.get("invocation")
        if declared is None:
            continue  # not every manifest schema records it
        skillmd = os.path.join(skills_dir, skill["name"], "SKILL.md")
        if not os.path.isfile(skillmd):
            continue  # reported elsewhere
        raw = parse_frontmatter_value(skillmd, "disable-model-invocation")
        user_only = isinstance(raw, str) and raw.strip().lower() == "true"
        expected = "user" if user_only else "model"
        if declared != expected:
            details.append(
                "%s: %s: manifest invocation %r but SKILL.md frontmatter says %s "
                "(disable-model-invocation: %s)" % (
                    label, skill["name"], declared, expected,
                    raw if raw is not None else "absent"))
    return details


def check_provider_invocation_matches_frontmatter():
    """A manifest's `invocation` must agree with the skill's own frontmatter: `user` exactly when the
    skill sets `disable-model-invocation: true`, `model` otherwise.

    This is not bookkeeping. `invocation` is how a reader learns whether a skill can fire from model
    judgment alone, and it is the field the vendoring policies cite when explaining why a
    high-authority skill was moved to explicit invocation. A manifest that says `model` for a skill
    whose frontmatter disables model invocation (or the reverse) misdescribes the installed system's
    actual trigger surface -- which is exactly the class of drift the manifests exist to prevent.
    Caught first by a reviewer on one real entry; now mechanical for every provider."""
    details = []
    for entry in MANIFESTS:
        details += invocation_consistency_details(entry, provider_skills(entry), SKILLS_DIR)
    report("provider-invocation-matches-frontmatter", not details, details)


def check_provider_dependency_closure():
    """Every file a vendored skill points at must actually have been vendored with it.

    check_relative_links_resolve above covers Markdown `[link](target)` syntax. This covers the
    other half, which is how these providers actually reference their closure: an inline-code path
    such as `scripts/context.mjs` or `references/anti-patterns.md`. A reference is only treated as
    a claim when its first path segment is a real closure directory inside that skill, so prose and
    example paths cannot produce false failures -- but a SKILL.md that tells the agent to run a
    script that was never copied in fails closed, which is the whole point of vendoring a complete
    dependency closure rather than a bare SKILL.md."""
    details = dependency_closure_details(SKILLS_DIR)
    report("provider-dependency-closure", not details, details)


def dependency_closure_details(skills_dir):
    """Root-parameterized core of check_provider_dependency_closure."""
    details = []
    if not os.path.isdir(skills_dir):
        return details
    for name in sorted(d for d in os.listdir(skills_dir)
                       if os.path.isdir(os.path.join(skills_dir, d))):
        skill_dir = os.path.join(skills_dir, name)
        present_dirs = {d for d in CLOSURE_DIRS if os.path.isdir(os.path.join(skill_dir, d))}
        if not present_dirs:
            continue
        for dirpath, _, filenames in os.walk(skill_dir):
            for fname in sorted(filenames):
                if not fname.endswith(".md"):
                    continue
                path = os.path.join(dirpath, fname)
                with open(path, encoding="utf-8") as f:
                    text = strip_fenced_blocks(f.read())
                for target in sorted(set(CLOSURE_REF_RE.findall(text))):
                    head = target.split("/", 1)[0]
                    if head not in present_dirs:
                        continue
                    if not os.path.exists(os.path.join(skill_dir, target)):
                        details.append("%s: %s references %r, which is not vendored" % (
                            name, os.path.relpath(path, skills_dir), target))
    return details


def check_application_paths_untouched():
    base_sha = os.environ.get("GOVERNANCE_BASE_SHA")
    details = governance_base_commit_details(base_sha, required=True)
    if details:
        report("application-paths-untouched", False, details)
        return
    try:
        paths = application_scope_changed_paths(base_sha, REPO_ROOT)
    except Exception as e:
        report("application-paths-untouched", False, ["could not run git: %s" % e])
        return
    details = application_path_violations(paths)
    report("application-paths-untouched", not details, details)


def governance_base_sha_details(value, required=False):
    """Validate the comparison object spelling before it can reach Git.

    Dedicated G1 scope validation requires an explicit base. Malformed, uppercase, abbreviated and
    branch-creation zero values fail closed instead of becoming ambiguous Git revisions.
    """
    if not value:
        if required:
            return ["GOVERNANCE_BASE_SHA is required for g1-stable-skills application scope"]
        return []
    if not re.fullmatch(r"[0-9a-f]{40}", value) or value == "0" * 40:
        return ["GOVERNANCE_BASE_SHA must be one non-zero lowercase full commit SHA"]
    return []


def governance_base_commit_details(value, repo_root=REPO_ROOT, required=False):
    """Require ``value`` to name an existing commit object, never a tree/blob/tag lookalike."""
    details = governance_base_sha_details(value, required=required)
    if details or not value:
        return details
    try:
        out = subprocess.run(
            ["git", "-C", repo_root, "cat-file", "-t", value],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10, text=True)
    except Exception as exc:
        return ["could not inspect GOVERNANCE_BASE_SHA object: %s" % exc]
    if out.returncode != 0:
        return ["GOVERNANCE_BASE_SHA does not name an existing commit object"]
    if out.stdout.strip() != "commit":
        return ["GOVERNANCE_BASE_SHA must name a commit object, got %s" %
                (out.stdout.strip() or "unknown")]
    return []


def application_scope_invocation_details(scope, environ=os.environ, repo_root=REPO_ROOT):
    """Validate the mode/environment ownership boundary before any repository check runs."""
    if scope == APPLICATION_SCOPE_GENERIC:
        if "GOVERNANCE_BASE_SHA" in environ:
            return ["GOVERNANCE_BASE_SHA is forbidden for generic application scope"]
        return []
    if scope == APPLICATION_SCOPE_G1_STABLE_SKILLS:
        return governance_base_commit_details(
            environ.get("GOVERNANCE_BASE_SHA"), repo_root=repo_root, required=True)
    return ["unknown application scope %r" % scope]


def application_scope_changed_paths(base_sha, repo_root=REPO_ROOT):
    """Return exact changed and untracked paths for a dedicated G1 comparison.

    Rename detection is disabled so an application file moved into governance produces both the
    deleted application source and added governance destination. HEAD, index and worktree are
    inspected independently: no layer can hide an application path by restoring another layer to
    the base version. NUL framing preserves unusual filenames without Git quoting or line splits.
    """
    commands = (
        ["git", "-C", repo_root, "diff", "--name-only", "--no-renames", "-z",
         base_sha, "HEAD", "--"],
        ["git", "-C", repo_root, "diff", "--cached", "--name-only", "--no-renames", "-z",
         base_sha, "--"],
        ["git", "-C", repo_root, "diff", "--name-only", "--no-renames", "-z", "--"],
        ["git", "-C", repo_root, "ls-files", "--others", "--exclude-standard", "-z", "--"],
    )
    paths = []
    seen = set()
    for command in commands:
        out = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
        if out.returncode != 0:
            error = os.fsdecode(out.stderr).strip()
            raise RuntimeError("git command failed: %s" % (error or "exit %d" % out.returncode))
        for raw in out.stdout.split(b"\0"):
            if not raw:
                continue
            path = os.fsdecode(raw)
            if path not in seen:
                seen.add(path)
                paths.append(path)
    return paths


def application_path_violations(paths):
    """Pure core of check_application_paths_untouched, so the allowlist is directly testable.

    The two exact workflow exceptions are paired with semantic checks below. No directory prefix
    is exempt, and the stable release workflow remains an application path."""
    details = []
    exact_workflow_exceptions = set(GOVERNANCE_OWNED_WORKFLOWS)
    for path in paths:
        if not path:
            continue
        if path in exact_workflow_exceptions:
            continue
        if path.startswith(APPLICATION_PATH_PREFIXES) or path.startswith(APPLICATION_PATH_EXACT_PREFIXES):
            # Git permits control characters in filenames. JSON string escaping keeps one path on
            # one diagnostic line, so a hostile name cannot forge PASS/FAIL records in CI logs.
            display_path = json.dumps(path, ensure_ascii=True)[1:-1]
            details.append("touched application path: %s" % display_path)
    return details


def _sha256_text(text):
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def _top_level_yaml_block(text, key):
    """Return one top-level YAML source block without pretending to be a general YAML parser."""
    lines = text.splitlines(True)
    starts = [i for i, line in enumerate(lines) if line.rstrip("\n") == key + ":"]
    if len(starts) != 1:
        return None
    start = starts[0]
    end = len(lines)
    for i in range(start + 1, len(lines)):
        if re.match(r"^[A-Za-z_][A-Za-z0-9_-]*(?::|\s*:)", lines[i]):
            end = i
            break
    return "".join(lines[start:end])


def _workflow_job_blocks(text):
    """Extract exact two-space-indented job blocks from the repository-owned workflow subset."""
    jobs = _top_level_yaml_block(text, "jobs")
    if jobs is None:
        return {}
    lines = jobs.splitlines(True)
    starts = []
    for i, line in enumerate(lines[1:], 1):
        match = re.match(r"^  ([A-Za-z_][A-Za-z0-9_-]*):\s*$", line)
        if match:
            starts.append((i, match.group(1)))
    out = {}
    for pos, (start, name) in enumerate(starts):
        end = starts[pos + 1][0] if pos + 1 < len(starts) else len(lines)
        out[name] = "".join(lines[start:end])
    return out


def _workflow_uses(text):
    return re.findall(r"^\s*(?:-\s*)?uses:\s*([^\s#]+)", text, re.MULTILINE)


def _checkout_input_maps(job_text):
    """Return each checkout step's literal input mapping from the closed workflow format."""
    lines = job_text.splitlines()
    maps = []
    for index, line in enumerate(lines):
        match = re.match(r"^\s*(?:-\s*)?uses:\s*([^\s#]+)", line)
        if not match or not match.group(1).startswith("actions/checkout@"):
            continue
        inputs = {}
        in_with = False
        for later in lines[index + 1:]:
            if re.match(r"^      - ", later):
                break
            if later.strip() == "with:":
                in_with = True
                continue
            if not in_with:
                continue
            item = re.match(r"^\s{10}([a-z][a-z0-9-]*):\s*(.*?)\s*$", later)
            if item:
                inputs[item.group(1)] = item.group(2)
        maps.append(inputs)
    return maps


def _checkout_contract_details(job_text, count, trusted_event_ref, fetch_depth=1):
    details = []
    uses = _workflow_uses(job_text)
    if uses != [PINNED_CHECKOUT_ACTION] * count:
        details.append("executable Actions must be exactly %d pinned checkout invocation(s), got %r"
                       % (count, uses))
    expected = {
        "clean": "true",
        "fetch-depth": str(fetch_depth),
        "fetch-tags": "false",
        "lfs": "false",
        "persist-credentials": "false",
        "set-safe-directory": "false",
        "show-progress": "false",
        "submodules": "false",
    }
    if trusted_event_ref:
        expected["ref"] = "${{ github.sha }}"
    maps = _checkout_input_maps(job_text)
    if len(maps) != count:
        details.append("expected %d checkout input map(s), got %d" % (count, len(maps)))
    for inputs in maps:
        if inputs != expected:
            details.append("checkout inputs differ from the closed envelope: %r" % inputs)
    return details


def _bootstrap_source_details(text, expected_count, label):
    bodies = [match.group("body").encode("utf-8")
              for match in _PRECHECKOUT_BOOTSTRAP_RE.finditer(text)]
    details = []
    if len(bodies) != expected_count:
        details.append("%s pre-checkout bootstrap count differs: %d"
                       % (label, len(bodies)))
        return details
    for body in bodies:
        if hashlib.sha256(body).hexdigest() != PRECHECKOUT_BOOTSTRAP_SHA256:
            details.append("%s pre-checkout bootstrap source digest mismatch" % label)
            break
    return details


def _reserved_provider_env_details(text, label):
    env_header = re.compile(
        r"^(?P<indent>[ ]*)(?P<quote>['\"]?)env(?P=quote)\s*:\s*(?P<tail>.*?)\s*$"
    )
    unsupported_yaml_key = re.compile(
        r'(?m)^[ ]*(?:[?!]|[&*][^:\n]*:|"[^"\n]*\\[^"\n]*"\s*:)'
    )
    assignment = re.compile(
        r"^(?P<indent>[ ]+)(?P<quote>['\"]?)(?P<key>[A-Za-z_][A-Za-z0-9_]*)(?P=quote)"
        r"\s*:\s*(?P<value>.*?)\s*$"
    )
    lines = text.splitlines()
    details = []
    shadowed = set()
    if unsupported_yaml_key.search(text):
        details.append(
            "%s contains an unsupported explicit, tagged, anchored, aliased, or escaped YAML key"
            % label
        )
    for index, line in enumerate(lines):
        header = env_header.match(line)
        if header is None:
            continue
        if header.group("tail"):
            details.append("%s contains a non-block or aliased env mapping" % label)
            continue
        parent_indent = len(header.group("indent"))
        child_indent = None
        keys = set()
        for child in lines[index + 1:]:
            if not child.strip() or child.lstrip().startswith("#"):
                continue
            indentation = len(child) - len(child.lstrip(" "))
            if indentation <= parent_indent:
                break
            if child_indent is None:
                child_indent = indentation
            if indentation != child_indent:
                continue
            stripped = child.strip()
            if stripped.startswith("<<:"):
                details.append("%s contains an env merge key" % label)
                break
            match = assignment.match(child)
            if match is None:
                details.append("%s contains an unsupported env key spelling" % label)
                break
            key = match.group("key")
            value = match.group("value")
            if key in keys:
                details.append("%s contains duplicate env key %s" % (label, key))
                break
            keys.add(key)
            if re.search(r"(?:^|[\s\[{,])(?:&|\*)[A-Za-z0-9_-]+", value):
                details.append("%s contains an env anchor or alias" % label)
                break
            if (key in _RESERVED_PROVIDER_ENV_NAMES
                    or key.startswith(_RESERVED_PROVIDER_ENV_PREFIXES)):
                shadowed.add(key)
    if shadowed:
        details.append("%s shadows provider-reserved environment: %r"
                       % (label, sorted(shadowed)))
    return details


def _isolated_python_details(text, label):
    details = []
    for line in text.splitlines():
        if not re.search(r"\bpython3\b", line):
            continue
        if "python3 -I -S -B" not in line:
            details.append("%s contains a non-isolated Python invocation" % label)
            break
        if re.search(r"\bpython3 -I -S -B\s+(?:-m|-c)\b", line):
            details.append(
                "%s contains an inline or module-mode production Python invocation" % label
            )
            break
    return details


def _privileged_expression_details(text, label):
    details = []
    patterns = (
        ("secret expression", r"\$\{\{[^}\n]*\bsecrets\s*(?:\.|\[)"),
        (
            "ambient github token expression",
            r"\$\{\{[^}\n]*\bgithub\s*(?:\.token|\[\s*['\"]token['\"]\s*\])",
        ),
    )
    for description, pattern in patterns:
        if re.search(pattern, text, flags=re.IGNORECASE):
            details.append("%s contains a forbidden %s" % (label, description))
    return details


def _exact_step_closure_details(job_text, expected_names, label):
    step_lines = re.findall(r"^      - (?P<body>.+?)\s*$", job_text, re.MULTILINE)
    names = []
    for body in step_lines:
        match = re.fullmatch(r"name:\s*(.+?)\s*", body)
        if match is None:
            return ["%s contains an unnamed or direct step" % label]
        names.append(match.group(1))
    if names != list(expected_names):
        return ["%s step closure mismatch: %r" % (label, names)]
    return []


def _runner_envelope_details(job_text, label):
    details = []
    required = (
        "runs-on: ubuntu-24.04",
        "GIT_ALLOW_PROTOCOL: https",
        "GIT_CONFIG_GLOBAL: /dev/null",
        'GIT_CONFIG_NOSYSTEM: "1"',
        'GIT_TERMINAL_PROMPT: "0"',
        'PYTHONDONTWRITEBYTECODE: "1"',
        '[[ "${ImageOS:-}" == "ubuntu24" ]]',
        '[[ "${RUNNER_ARCH:-}" == "X64" ]]',
        '[[ "${ImageVersion:-}" =~ ^[0-9]{8}\\.[0-9]+\\.[0-9]+$ ]]',
        "python3 -I -S -B <<'PY'",
        "sys.version_info[:2] != (3, 12)",
        "(2, 43, 0) <= parsed < (2, 60, 0)",
        "parsed_bash[:2] != (5, 2)",
        "urllib.request.ProxyHandler({})",
        '"SSL_CERT_DIR"',
        '"GIT_SSL_NO_VERIFY"',
        '"NODE_OPTIONS"',
        '"NODE_EXTRA_CA_CERTS"',
        '"NODE_TLS_REJECT_UNAUTHORIZED"',
        '"GIT_CONFIG_COUNT"',
        '"GIT_CONFIG_PARAMETERS"',
        '"GIT_EXEC_PATH"',
        "unexpected_git_env = sorted(",
        'repository_api_url = "https://api.github.com/repos/actions/runner-images"',
        'repository.get("id") != 190416463',
        'repository.get("full_name") != "actions/runner-images"',
        'source_sha = release.get("target_commitish")',
        'manifest.get("state") != "uploaded"',
        'f"{source_sha}/images/ubuntu/Ubuntu2404-Readme.md"',
        '"- SBOM availability: `UNAVAILABLE_OPTIONAL_HARDENING`',
    )
    for needle in required:
        if job_text.count(needle) != 1:
            details.append("%s must contain exactly one runner-envelope anchor %r" % (label, needle))
    details += _bootstrap_source_details(job_text, 1, label)
    preflight = job_text.find("Prove the pinned runner Python and Git envelope")
    checkout = job_text.find("uses: " + PINNED_CHECKOUT_ACTION)
    if preflight < 0 or checkout < 0 or preflight >= checkout:
        details.append("%s must prove the runner/Git envelope before checkout" % label)
    influence = [
        job_text.find("changed_git_env ="),
        job_text.find("unexpected_git_env ="),
        job_text.find("present_auth ="),
        job_text.find("present_proxy ="),
        job_text.find("present_trust ="),
        job_text.find("present_execution ="),
    ]
    first_rest = job_text.find("with opener.open(request")
    if first_rest < 0 or any(position < 0 or position >= first_rest for position in influence):
        details.append("%s must reject ambient Git/auth/proxy/TLS influence before REST" % label)
    return details


def stable_skills_workflow_details(text):
    """Semantic checks for the only stable-native workflow admitted by G1.1."""
    details = []
    if _sha256_text(text) != STABLE_SKILLS_WORKFLOW_SHA256:
        details.append("stable workflow source digest mismatch")
    details += _bootstrap_source_details(text, 2, "stable workflow")
    details += _reserved_provider_env_details(text, "stable workflow")
    details += _isolated_python_details(text, "stable workflow")
    details += _privileged_expression_details(text, "stable workflow")
    if text.count("name: Stable skills maintenance") != 1:
        details.append("stable workflow display name must be unique and exact")

    on_block = _top_level_yaml_block(text, "on")
    if on_block is None:
        details.append("stable workflow must contain one top-level on block")
    else:
        events = re.findall(r"^  ([A-Za-z_][A-Za-z0-9_-]*):", on_block, re.MULTILINE)
        if events != ["schedule", "workflow_dispatch"]:
            details.append("stable workflow events must be exactly schedule + workflow_dispatch: %r"
                           % events)
        if on_block.count('cron: "37 5 * * *"') != 1:
            details.append("stable workflow must carry the exact reviewed daily schedule")
        if "inputs:" in on_block or "provider:" in on_block:
            details.append("production workflow_dispatch must always run the complete provider set")

    if not re.search(r"^permissions: \{\}\s*$", text, re.MULTILINE):
        details.append("stable workflow top-level permissions must be empty")
    for needle in (
        "env:\n  BASH_ENV: /dev/null",
        'shell: "/usr/bin/bash --noprofile --norc -euo pipefail {0}"',
    ):
        if text.count(needle) != 1:
            details.append("stable workflow lacks exact shell bootstrap anchor %r" % needle)
    if text.count("group: stable-skills-maintenance") != 1 or \
            text.count("cancel-in-progress: false") != 1:
        details.append("stable workflow concurrency contract is missing or duplicated")

    jobs = _workflow_job_blocks(text)
    if list(jobs) != ["detect", "prepare", "reconcile-all"]:
        details.append("stable workflow jobs must be detect, prepare, reconcile-all in order: %r"
                       % list(jobs))
        return details
    detect = jobs["detect"]
    prepare = jobs["prepare"]
    reconcile = jobs["reconcile-all"]
    details += _exact_step_closure_details(
        detect,
        (
            "Prove the pinned runner Python and Git envelope",
            "Check out the trusted stable event head",
            "Prove stable workflow and control-plane identity",
            "Refuse detection when governance or updater invariants are red",
            "Detect and classify upstream drift",
            "Build the inert preparation matrix",
        ),
        "stable detect",
    )
    details += _exact_step_closure_details(
        prepare,
        (
            "Prove the pinned runner Python and Git envelope",
            "Check out the trusted stable event head",
            "Re-prove stable identity before preparation",
            "Prepare candidate content outside the repository checkout",
        ),
        "stable prepare",
    )
    details += _exact_step_closure_details(
        reconcile,
        ("Refuse detector-only liveness and re-prove the runner envelope",),
        "stable reconcile-all",
    )

    for label, block in (("detect", detect), ("prepare", prepare)):
        if block.count("permissions:\n      contents: read") != 1:
            details.append("%s must have only job-local contents: read" % label)
        details += _runner_envelope_details(block, label)
        details += _checkout_contract_details(block, 1, trusted_event_ref=True)
    if reconcile.count("permissions: {}") != 1:
        details.append("reconcile-all must have empty permissions")
    if reconcile.count("name: Reconcile complete G1.1 control plane") != 1:
        details.append(
            "reconcile-all YAML key must map to its exact externally observable API name"
        )
    if reconcile.count("runs-on: ubuntu-24.04") != 1:
        details.append("reconcile-all must use the pinned runner label")
    for needle in (
        "Refuse detector-only liveness and re-prove the runner envelope",
        '[[ "${ImageOS:-}" == "ubuntu24" ]]',
        '[[ "${RUNNER_ARCH:-}" == "X64" ]]',
        '[[ "${ImageVersion:-}" =~ ^[0-9]{8}\\.[0-9]+\\.[0-9]+$ ]]',
        '[[ "${BASH_VERSION:-}" =~ ^5\\.2\\.[0-9]+\\([0-9]+\\)-release$ ]]',
    ):
        if reconcile.count(needle) != 1:
            details.append("reconcile-all lacks exact runtime anchor %r" % needle)

    identity_needles = (
        "github.repository == 'KraineOpasen/bukerov-twitch-miner-go'",
        "github.repository_id == '1297795646'",
        "github.ref == 'refs/heads/release/0.3'",
        "github.workflow_ref == 'KraineOpasen/bukerov-twitch-miner-go/.github/workflows/"
        "stable-skills-maintenance.yml@refs/heads/release/0.3'",
    )
    for needle in identity_needles:
        if detect.count(needle) != 1:
            details.append("detect identity guard missing %r" % needle)

    verify = ("python3 -I -S -B scripts/skill_updates/runtime.py "
              "verify-workflow --repo-root .")
    detector = "python3 -I -S -B scripts/check-skill-updates.py"
    preparer = "python3 -I -S -B scripts/prepare-skill-update.py"
    if detect.count(verify) != 1 or detect.find(verify) >= detect.find(detector):
        details.append("runtime/control guard must run once before the detector")
    if prepare.count(verify) != 1 or prepare.find(verify) >= prepare.find(preparer):
        details.append("runtime/control guard must run once before the preparer")
    required_detect = (
        "verify-repository --repo-root .",
        "scripts/validate-agent-governance.py --self-test-hook",
        "python3 -I -S -B scripts/validate-agent-governance.py --self-test\n",
        "mutation_probe.py --check-anchors",
        "mutation_probe.py --suite-only",
        "--all",
        "--mode prepare",
        "any_prepare: ${{ steps.plan.outputs.any_prepare }}",
        "matrix: ${{ steps.plan.outputs.matrix }}",
    )
    for needle in required_detect:
        if detect.count(needle) != 1:
            details.append("detect job missing exact integration anchor %r" % needle)
    required_prepare = (
        '--provider "$PROVIDER"', '--target-sha "$TARGET_SHA"',
        '--artifact-root "$RUNNER_TEMP/stable-skills-$PROVIDER"',
        '--summary "$GITHUB_STEP_SUMMARY"',
    )
    for needle in required_prepare:
        if prepare.count(needle) != 1:
            details.append("prepare job missing exact artifact-only argument %r" % needle)

    if reconcile.count("needs: [detect, prepare]") != 1 or reconcile.count("if: always()") != 1:
        details.append("reconcile-all must run after both predecessor jobs even on failure")
    for needle in ('DETECT_RESULT: ${{ needs.detect.result }}',
                   'PREPARE_RESULT: ${{ needs.prepare.result }}',
                   '"$DETECT_RESULT" != "success"', 'success|skipped'):
        if reconcile.count(needle) != 1:
            details.append("reconcile-all missing liveness anchor %r" % needle)

    forbidden = (
        "--publish", "--base-branch", "contents: write", "pull-requests: write",
        "issues: write", "actions: write", "id-token: write", "secrets.",
        "${{ github.token", "Authorization:", "git push", "git commit", "git tag",
        "gh api", "curl ", "wget ", "actions/upload-artifact@",
    )
    for needle in forbidden:
        if needle in text:
            details.append("stable workflow contains forbidden G1.1 authority %r" % needle)
    if _workflow_uses(text) != [PINNED_CHECKOUT_ACTION, PINNED_CHECKOUT_ACTION]:
        details.append("stable workflow executable Action registry is not exactly two checkouts")
    return details


_CI_BLOCK_BEGIN = "  # BEGIN stable-skills-governance-job/v1\n"
_CI_BLOCK_END = "  # END stable-skills-governance-job/v1\n\n"


def ci_workflow_details(text):
    """Prove the G1.1 governance insertion without taking ownership of product CI."""
    details = []
    if text.count(_CI_BLOCK_BEGIN) != 1 or text.count(_CI_BLOCK_END) != 1:
        return ["CI must contain exactly one bounded stable-skills governance block"]
    start = text.index(_CI_BLOCK_BEGIN)
    end = text.index(_CI_BLOCK_END, start) + len(_CI_BLOCK_END)
    stripped = text[:start] + text[end:]
    if _sha256_text(stripped) != STABLE_CI_BASELINE_SHA256:
        details.append("removing the governance block does not reproduce exact stable product CI")
    block = text[start:end]
    if _sha256_text(block) != STABLE_CI_GOVERNANCE_BLOCK_SHA256:
        details.append("CI governance source digest mismatch")
    details += _bootstrap_source_details(block, 1, "CI governance")
    details += _reserved_provider_env_details(block, "CI governance")
    details += _isolated_python_details(block, "CI governance")
    details += _privileged_expression_details(block, "CI governance")
    details += _exact_step_closure_details(
        block,
        (
            "Prove the pinned runner Python and Git envelope",
            "Check out the event head and comparison history without persisted credentials",
            "Validate stable governance and updater integration",
            "Run validator fixture self-tests",
            "Verify mutation probe anchors",
            "Run updater regression suite",
        ),
        "CI governance",
    )
    if block.count("runs-on: ubuntu-24.04") != 1:
        details.append("CI governance job must use ubuntu-24.04")
    if block.count("permissions:\n      contents: read") != 1:
        details.append("CI governance job must have only job-local contents: read")
    for needle in (
        "BASH_ENV: /dev/null",
        'shell: "/usr/bin/bash --noprofile --norc -euo pipefail {0}"',
    ):
        if block.count(needle) != 1:
            details.append("CI governance job lacks exact shell bootstrap anchor %r" % needle)
    details += _runner_envelope_details(block, "CI governance")
    details += _checkout_contract_details(block, 1, trusted_event_ref=True, fetch_depth=0)
    if "GOVERNANCE_BASE_SHA" in block:
        details.append("generic CI governance must not export a G1 application-scope base")
    required = (
        "scripts/validate-agent-governance.py --application-scope generic --self-test-hook",
        "scripts/skill_updates/runtime.py verify-repository --repo-root .",
        "python3 -I -S -B scripts/validate-agent-governance.py --self-test\n",
        "mutation_probe.py --check-anchors",
        "mutation_probe.py --suite-only",
    )
    for needle in required:
        if block.count(needle) != 1:
            details.append("CI governance job missing exact command %r" % needle)
    for needle in ("contents: write", "pull-requests: write", "issues: write",
                   "id-token: write", "secrets.", "${{ github.token", "Authorization:",
                   "--publish"):
        if needle in block:
            details.append("CI governance job contains privileged surface %r" % needle)
    return details


def workflow_registry_details(repo_root):
    workflow_dir = os.path.join(repo_root, ".github", "workflows")
    try:
        names = sorted(name for name in os.listdir(workflow_dir)
                       if name.endswith((".yml", ".yaml")))
    except OSError as exc:
        return ["cannot enumerate workflow registry: %s" % exc]
    expected = sorted(("ci.yml", "stable-release.yml", "stable-skills-maintenance.yml"))
    details = []
    if names != expected:
        details.append("file-backed workflow registry differs: %r" % names)
    release_path = os.path.join(workflow_dir, "stable-release.yml")
    try:
        with io.open(release_path, encoding="utf-8") as handle:
            release_digest = _sha256_text(handle.read())
    except OSError as exc:
        details.append("cannot read stable release workflow: %s" % exc)
    else:
        if release_digest != STABLE_RELEASE_SHA256:
            details.append("stable-release.yml differs from stable-authoritative bytes")
    return details


def stable_skills_control_details(repo_root, runner=subprocess.run):
    """Delegate closed JSON/schema semantics to the updater's static, network-free owner."""
    scripts = os.path.join(repo_root, "scripts")
    env = os.environ.copy()
    env["PYTHONPATH"] = scripts + (os.pathsep + env["PYTHONPATH"]
                                   if env.get("PYTHONPATH") else "")
    command = [sys.executable, "-m", "skill_updates.runtime", "verify-repository",
               "--repo-root", repo_root]
    try:
        result = runner(command, cwd=repo_root, env=env, stdout=subprocess.PIPE,
                        stderr=subprocess.STDOUT, timeout=30, text=True)
    except Exception as exc:
        return ["static control verification could not run: %s" % exc]
    if result.returncode != 0:
        output = (result.stdout or "").strip()
        return ["static control verification failed: %s" % (output or "exit %d" % result.returncode)]
    return []


def check_stable_skills_workflow():
    path = os.path.join(REPO_ROOT, STABLE_SKILLS_WORKFLOW_REL)
    try:
        with io.open(path, encoding="utf-8") as handle:
            details = stable_skills_workflow_details(handle.read())
    except OSError as exc:
        details = ["cannot read %s: %s" % (STABLE_SKILLS_WORKFLOW_REL, exc)]
    report("stable-skills-workflow", not details, details)


def check_stable_ci_governance_integration():
    path = os.path.join(REPO_ROOT, STABLE_CI_WORKFLOW_REL)
    try:
        with io.open(path, encoding="utf-8") as handle:
            details = ci_workflow_details(handle.read())
    except OSError as exc:
        details = ["cannot read %s: %s" % (STABLE_CI_WORKFLOW_REL, exc)]
    report("stable-ci-governance-integration", not details, details)


def check_stable_workflow_registry():
    details = workflow_registry_details(REPO_ROOT)
    report("stable-workflow-registry", not details, details)


def check_stable_skills_control_contract():
    details = stable_skills_control_details(REPO_ROOT)
    report("stable-skills-control-contract", not details, details)


def check_settings_schema():
    details = []
    try:
        with open(SETTINGS_PATH) as f:
            settings = _strict_json_load(f)
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
            settings = _strict_json_load(f)
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
        # mechanical capability boundary. Actual mutation authority comes solely from Governance
        # v3's operation modes (docs/agents/operation-modes.md) and an active task contract (see
        # project-skills-policy.md); the
        # "read-only requires empty scripts/hooks" rule below is a deterministic proxy the
        # validator can check, not the thing that grants or denies write access.
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


# --------------------------------------------------------------------------
# Governance v3 consistency (ADR-0002)
# --------------------------------------------------------------------------
#
# These checks keep the prose governance layer mechanically consistent with
# Governance v3 (docs/adr/0002-governance-v3-skill-native-orchestration.md).
# They are deterministic, stdlib-only, and never touch the network — same
# contract as every other check in this file.
#
# GOVERNANCE_SURFACE is the set of tracked text this repository's governance
# owns. It deliberately EXCLUDES:
#   - .claude/skills/**     vendored upstream bodies, integrity-pinned by blob
#                           SHA; their content is governed by the vendoring
#                           policies + patch ledgers, not by these checks.
#   - .claude/hooks/**      the mechanical enforcement layer, edit-denied.
#   - Dockerfile, SPECIFICATIONS.md
#                           deployment/protocol reference text, not governance.
# A file is scanned only if it exists; the set is a fixed list plus two
# directory walks, so the check is stable regardless of repo state.

GOVERNANCE_SURFACE_FILES = ("CLAUDE.md", "CONTEXT.md", "README.md")
GOVERNANCE_SURFACE_DIRS = (
    os.path.join(".claude", "rules"),
    os.path.join("docs", "agents"),
    os.path.join("docs", "adr"),
)

# ADRs are the historical-decision layer: ADR-0001 preserves the Governance v2
# wording verbatim, and the stable line's ADR-0002 (the canonical-Governance-v3
# adoption record) must name the v2 mandates the adoption removed in order to
# record the decision at all. Both are exempt from the stale-language scan;
# policy documents are not.
V2_LANGUAGE_EXEMPT = (
    "docs/adr/0001-agent-governance-v2.md",
    "docs/adr/0002-canonical-governance-v3.md",
)

# Normative Governance-v2 orchestration wording that v3 removed. Matched
# case-insensitively as normalized substrings (see normalize_prose_line).
# Deliberately phrase-level, not the bare string "v2": describing what v2 did
# (in ADR-0002, in the vendoring policies' migration notes) is legitimate and
# must stay possible.
#
# Every phrase here is quoted from the Governance-v2 text ADR-0002 superseded --
# see `git show <pre-v3-sha>:CLAUDE.md` "### Agent orchestration" and ADR-0001's
# preserved wording. A phrase is only admissible if it produces ZERO hits against
# the current governance surface; the V3 fixture proves that continuously, and
# the V8/V9 fixtures prove each family is actually detected.
STALE_V2_ORCHESTRATION_PHRASES = (
    # --- v2's single-writer / ledger / spawning mandates ---
    # The identifier and the prose forms are listed separately on purpose:
    # normalize_prose_line preserves underscores (they are part of the field
    # name), so "single_writer" does not also cover "single-writer" or
    # "single writer per task". Note what is deliberately ABSENT: "one writer
    # per task" and "exactly one writer" would collide with the legitimate v3
    # sentences that reject them (GOVERNANCE_V3.md section 10 and
    # docs/agents/task-contract.md both say the invariant is 'not "one writer"'),
    # so needling them would fail the check against correct text.
    "single_writer",
    "single-writer",
    "single writer per task",
    "one production writer",
    "role ledger",
    "no recursive subagent",
    "no recursive spawning",
    "no recursive delegation",
    "claude code governance (v2)",
    "governance v2 precedence",
    # --- v2's UNIVERSAL reviewer-read-only mandate (removed by ADR-0002 §2) ---
    # v2: "One production writer per task; every other agent is read-only ..."
    #     "Reviewer/analysis agents never write to tracked files or push."
    # A skill may still choose read-only reviewers; what v3 removed is the
    # project imposing it on every skill, so only the universal phrasings match.
    "every other agent is read-only",
    "reviewer/analysis agents never write",
    "reviewer agents never write",
    "analysis agents never write",
    "reviewers are always read-only",
    "reviewer agents are always read-only",
    "reviewers must always be read-only",
    # --- v2's MANDATORY resource caps (v3 keeps both as optional, no default) ---
    # Phrased so that legitimately quoting the field name -- which
    # task-contract.md and the patch ledgers all do -- never matches; only an
    # assertion that a cap is obligatory does.
    "agent_cap is mandatory",
    "mandatory agent_cap",
    "agent_cap is required",
    "agent_cap must be set",
    "must set an agent_cap",
    "max_concurrency is mandatory",
    "mandatory max_concurrency",
    "max_concurrency is required",
    "max_concurrency must be set",
    "must set a max_concurrency",
    # NOT listed: "respect the task contract's agent_cap". Every occurrence of
    # that phrasing is legitimate. In vendored skill bodies it is upstream text
    # this scan cannot reach anyway (the surface excludes .claude/skills/**); in
    # the patch ledgers and in task-contract.md / ADR-0002 it is QUOTED,
    # precisely in order to state the v3 re-reading rule
    # for it. Needling it would fail the check against the documents that fix
    # the problem. Those texts are governed by that re-reading rule, not here.
)

# The host operating system of a machine that happens to run the miner's
# container is not a governance concept (ADR-0002 §8), and the acceptance
# condition is zero occurrences across ALL tracked files -- with no exemption for
# the ADR that records the decision, or for this file.
#
# The token is therefore assembled from fragments rather than written as a
# literal: were it spelled out here, this enforcement script would itself become
# the last surviving occurrence of the very string it forbids, and the repo-wide
# check below would fail on its own source. Splitting it is not obfuscation --
# it is what lets the rule be absolute.
FORBIDDEN_HOST_OS_TOKEN = "true" + "nas"

# Pre-existing occurrences of the token in this stable line's APPLICATION files, counted at the
# baseline this governance layer landed on. These are product files (build, user documentation,
# protocol specification) that a governance concern has no authority to touch -- removing the token
# there is product work under its own contract. The budget is a per-path ceiling, not a grant: a
# NEW occurrence in any other file fails, and growth within a listed file fails. Shrinkage (a
# product change removing occurrences) passes and the budget then merely over-provisions until it
# is tightened here. The governance layer itself has a budget of zero everywhere.
HOST_OS_PREEXISTING_BUDGET = {
    "Dockerfile": 1,
    "README.md": 3,
    "SPECIFICATIONS.md": 1,
}

# The two invariants CLAUDE.md itself must carry, because a session that never opens
# GOVERNANCE_V3.md section 5 still reads this file. Deliberately the SHORT forms, in the stable
# line's canonical phrasing (GOVERNANCE_V3.md section 5: "Every new session starts READ_ONLY and
# needs a current contract"): this is a pointer paragraph, and pinning more of it would make
# CLAUDE.md expensive to edit for no added safety.
CLAUDE_MD_CONTINUITY_INVARIANTS = (
    "a checkpoint is evidence, never authority",
    "every new session starts read_only",
)

# The canonical governance document and its adoption record. On this stable line GOVERNANCE_V3.md
# at the repository root is the single canonical governance authority (CLAUDE.md's Governance
# section defers to it); main's docs/agents/agent-orchestration.md + session-recovery.md pair is a
# default-branch elaboration this line deliberately does not carry.
V3_REQUIRED_DOCS = (
    "GOVERNANCE_V3.md",
    "docs/adr/0002-canonical-governance-v3.md",
)


def governance_surface_paths(repo_root):
    """Repo-relative, sorted list of the governance text this repo owns.

    Pure: takes the root as an argument so self-test fixtures can point it at
    a synthetic tree. Missing files/dirs are skipped, not errors.
    """
    paths = []
    for name in GOVERNANCE_SURFACE_FILES:
        if os.path.isfile(os.path.join(repo_root, name)):
            paths.append(name)
    for subdir in GOVERNANCE_SURFACE_DIRS:
        abs_dir = os.path.join(repo_root, subdir)
        if not os.path.isdir(abs_dir):
            continue
        for dirpath, _dirnames, filenames in os.walk(abs_dir):
            for filename in sorted(filenames):
                if not filename.endswith(".md"):
                    continue
                abs_path = os.path.join(dirpath, filename)
                paths.append(os.path.relpath(abs_path, repo_root).replace(os.sep, "/"))
    return sorted(set(paths))


_PROSE_MARKUP_RE = re.compile(r"[`*]+")
_PROSE_WS_RE = re.compile(r"\s+")


def normalize_prose_line(line):
    """Lowercase a Markdown line and strip the markup that would hide a phrase.

    Backticks and asterisks are removed (so ``respect the task contract's
    `agent_cap``` normalizes to the same text as the unformatted sentence), and
    whitespace runs collapse to a single space. Underscores are deliberately
    LEFT ALONE -- they carry meaning in `single_writer`, `agent_cap` and
    `max_concurrency`, which are exactly the identifiers being matched.
    """
    return _PROSE_WS_RE.sub(" ", _PROSE_MARKUP_RE.sub("", line.lower())).strip()


def scan_governance_surface(repo_root, needles, exempt=()):
    """Return ["<relpath>:<lineno>: <needle>", ...] for case-insensitive hits.

    `needles` is an iterable of lowercase substrings, matched against
    normalize_prose_line() output. `exempt` is an iterable of repo-relative
    paths to skip entirely.

    These documents hard-wrap at ~110 columns, so a reinstated mandate can land
    with its phrase split across two physical lines. Each line is therefore
    matched both on its own and joined to its successor, with the hit reported
    against the FIRST line of the pair. A phrase that spans a single wrap is
    caught; one deliberately spread over three lines is not, which is the
    accepted limit of a line-oriented scan.
    """
    exempt_set = set(exempt)
    hits = []
    for relpath in governance_surface_paths(repo_root):
        if relpath in exempt_set:
            continue
        try:
            with io.open(os.path.join(repo_root, relpath), encoding="utf-8") as f:
                lines = f.read().splitlines()
        except Exception as e:
            hits.append("%s: unreadable: %s" % (relpath, _safe(str(e))))
            continue
        normalized = [normalize_prose_line(line) for line in lines]
        for idx, current in enumerate(normalized):
            nxt = normalized[idx + 1] if idx + 1 < len(normalized) else ""
            window = (current + " " + nxt).strip() if nxt else current
            for needle in needles:
                if needle in current:
                    # Contained in one line: report it at that line.
                    hits.append("%s:%d: %s" % (relpath, idx + 1, _safe(needle)))
                elif nxt and needle in window and needle not in nxt:
                    # Only visible across the wrap. A phrase wholly inside `nxt`
                    # is skipped here and reported on its own line next pass, so
                    # no hit is ever counted twice.
                    hits.append("%s:%d: %s" % (relpath, idx + 1, _safe(needle)))
    return hits


def governance_v3_docs_details(repo_root):
    """Return [] if the v3 docs exist and CLAUDE.md defers to the canonical GOVERNANCE_V3.md.

    Pure in `repo_root` so the self-test fixture runs this exact function against the real
    repository rather than restating its conditions.
    """
    details = []
    for relpath in V3_REQUIRED_DOCS:
        if not os.path.isfile(os.path.join(repo_root, relpath)):
            details.append("missing required Governance v3 doc: %s" % relpath)

    claude_md = os.path.join(repo_root, "CLAUDE.md")
    if not os.path.isfile(claude_md):
        details.append("CLAUDE.md missing")
        return details

    with io.open(claude_md, encoding="utf-8") as f:
        text = f.read()
    if "## Governance" not in text:
        details.append("CLAUDE.md must declare the heading '## Governance'")
    if "## Claude Code Governance (v2)" in text:
        details.append("CLAUDE.md must not still declare the Governance v2 heading")
    if "GOVERNANCE_V3.md" not in text:
        details.append("CLAUDE.md must point at the canonical GOVERNANCE_V3.md")

    flat = normalize_prose_line(text.replace("\n", " "))
    # CLAUDE.md must say GOVERNANCE_V3.md is canonical, not merely link it.
    if "single canonical governance authority" not in flat:
        details.append(
            "CLAUDE.md must state that GOVERNANCE_V3.md is the single canonical governance authority")

    # The continuity invariants live in CLAUDE.md because it is the file every fresh session
    # actually loads; GOVERNANCE_V3.md section 5 is the canonical source (pinned separately by
    # check_governance_v3_recovery_contract) and CLAUDE.md restates the two sentences a session
    # must not be able to miss. Pinning them only in the pointed-to document would let this
    # paragraph be inverted -- telling a session a resume re-enters the recorded mode -- with
    # every other check still green.
    for needle in CLAUDE_MD_CONTINUITY_INVARIANTS:
        if needle not in flat:
            details.append("CLAUDE.md's session-continuity pointer omits: %r" % needle)
    if CHECKPOINT_CONTRACT_ID not in flat:
        details.append(
            "CLAUDE.md must name the checkpoint contract id %r, not the YAML key" % CHECKPOINT_CONTRACT_ID
        )
    return details


def check_governance_v3_docs():
    """The v3 docs exist and CLAUDE.md declares v3, not v2."""
    details = governance_v3_docs_details(REPO_ROOT)
    report("governance-v3-docs", not details, details)


def stale_orchestration_details(repo_root):
    """Return [] if no Governance-v2 orchestration mandate survives, else the hits."""
    return scan_governance_surface(
        repo_root, STALE_V2_ORCHESTRATION_PHRASES, exempt=V2_LANGUAGE_EXEMPT
    )


def check_governance_v3_no_stale_orchestration():
    """No Governance-v2 orchestration mandates left in the governance surface."""
    details = stale_orchestration_details(REPO_ROOT)
    report("governance-v3-no-stale-orchestration", not details, details)


RESOURCE_CEILING_FIELDS = ("agent_cap", "max_concurrency")

_YAML_FENCE_RE = re.compile(r"```ya?ml\n(.*?)```", re.DOTALL)


def _extract_yaml_schema_block(text):
    """The contents of task-contract.md's `task_contract:` yaml fence, or None.

    Selected by the `task_contract:` key rather than by position: taking the
    FIRST yaml fence would silently rebind this whole check to an unrelated
    example if one were ever added above the schema, and every field lookup
    below would then pass against the wrong block while still reporting PASS.

    Returns None -- never a positional guess -- when no fence declares the key.
    Falling back to the first fence would reopen exactly the hole this function
    closes; None instead surfaces "no ```yaml schema block found", which is a
    real, actionable diagnostic.
    """
    for block in _YAML_FENCE_RE.findall(text):
        if re.search(r"^\s*task_contract\s*:", block, re.MULTILINE):
            return block
    return None


def _schema_field_line(schema_block, field):
    """The single schema line declaring `field`, or None.

    Matches the key at the start of a line (modulo indentation) so that a field
    merely *mentioned* inside another entry's comment is never mistaken for its
    declaration.
    """
    pattern = re.compile(r"^\s*%s\s*:" % re.escape(field))
    for line in schema_block.splitlines():
        if pattern.match(line):
            return line
    return None


def _field_note_bullet(text, field):
    """The '## Field notes' bullet that defines `field`, joined into one string.

    Field notes are hard-wrapped Markdown bullets: a `- ` line followed by
    indented continuation lines. Returns the whole logical bullet whose FIRST
    line names the field, so a passing-mention in a neighbouring bullet cannot
    satisfy a requirement about this field's own definition.
    """
    lines = text.splitlines()
    try:
        start = next(i for i, l in enumerate(lines) if l.strip().lower() == "## field notes")
    except StopIteration:
        return None

    bullets = []
    current = None
    for line in lines[start + 1:]:
        if line.startswith("## "):
            break
        if line.startswith("- "):
            # Only a TOP-LEVEL dash opens a new bullet; an indented "- " is a
            # sub-bullet and stays part of the logical bullet above it (handled
            # by the else-branch), which would otherwise be truncated
            # mid-definition. An indented sub-bullet with no open parent -- one
            # separated from it by a blank line -- has no bullet to belong to
            # and is skipped rather than promoted to a top-level definition.
            if current is not None:
                bullets.append(current)
            current = [line]
        elif current is not None:
            if not line.strip():
                bullets.append(current)
                current = None
            else:
                current.append(line)
    if current is not None:
        bullets.append(current)

    for bullet in bullets:
        if field in normalize_prose_line(bullet[0]):
            return normalize_prose_line(" ".join(bullet))
    return None


def contract_schema_details(text):
    """Return [] if `text` reads as a Governance-v3 authority envelope, else reasons.

    Pure: takes the document text so both the production check and the self-test
    fixtures exercise this exact function -- the fixtures never restate these
    conditions themselves.

    The v2 implementation of the resource-ceiling test asked only whether the
    word "optional" occurred ANYWHERE in the document. That is vacuous here: the
    schema's own preamble opens "Every field is optional except ...", so the
    substring is always present and the test could never fail, no matter how the
    fields were actually described. Each field is now checked against its OWN
    schema line and its OWN field-note bullet, and must positively establish all
    four v3 properties:

      1. optional -- not a required field;
      2. a resource ceiling -- not orchestration policy;
      3. absent => no cap, and no default value;
      4. no longer mandatory (the explicit v2 reversal).
    """
    details = []
    lowered = normalize_prose_line(text.replace("\n", " "))

    if "orchestration: skill_native | main_context_only" not in lowered:
        details.append("schema must offer 'orchestration: skill_native | main_context_only'")
    if "absent => skill_native" not in lowered:
        details.append("schema must state that an absent 'orchestration' field means skill_native")

    schema_block = _extract_yaml_schema_block(text)
    if schema_block is None:
        details.append("no ```yaml schema block found")
        schema_block = ""

    for field in RESOURCE_CEILING_FIELDS:
        schema_line = _schema_field_line(schema_block, field)
        if schema_line is None:
            details.append("%s: no declaration in the yaml schema block" % field)
        else:
            norm = normalize_prose_line(schema_line)
            if "optional" not in norm:
                details.append(
                    "%s: schema line must mark the field optional, got: %s" % (field, _safe(schema_line.strip()))
                )
            if not ("absent" in norm and "no cap" in norm):
                details.append(
                    "%s: schema line must state 'absent => no cap', got: %s" % (field, _safe(schema_line.strip()))
                )
            if "required" in norm or "mandatory" in norm:
                details.append(
                    "%s: schema line must not describe the field as required/mandatory, got: %s"
                    % (field, _safe(schema_line.strip()))
                )

        bullet = _field_note_bullet(text, field)
        if bullet is None:
            details.append("%s: no '## Field notes' bullet defines this field" % field)
            continue
        if "optional" not in bullet:
            details.append("%s: field note must call the field optional" % field)
        if "resource ceiling" not in bullet:
            details.append("%s: field note must describe the field as a resource ceiling" % field)
        if "not orchestration policy" not in bullet:
            details.append(
                "%s: field note must state the field is NOT orchestration policy" % field
            )
        if "no longer mandatory" not in bullet:
            details.append("%s: field note must record that the field is no longer mandatory" % field)
        if not ("absent" in bullet and "no cap" in bullet):
            details.append("%s: field note must state that absence means no cap" % field)
        if "no default" not in bullet and "have no default" not in bullet:
            details.append("%s: field note must state the field has no default" % field)

    # v2 made single_writer a required contract field and a global invariant.
    # v3 has no such field at any strength, so its presence anywhere in the
    # schema document is a regression -- checked structurally (required-field
    # sentence, yaml keys) and then document-wide.
    single_writer_details = []
    for line in text.splitlines():
        low = normalize_prose_line(line)
        if "except" in low and "authorized_by" in low and "single_writer" in low:
            single_writer_details.append(
                "single_writer must not be a required contract field: %s" % _safe(line.strip())
            )
    if _schema_field_line(schema_block, "single_writer") is not None:
        single_writer_details.append("single_writer must not appear as a key in the yaml schema block")
    if "single_writer" in lowered and not single_writer_details:
        # The two checks above are the specific, actionable diagnoses; the
        # document-wide sweep is the catch-all beneath them. Reported only when
        # neither fired, so one regression yields one finding, not three.
        single_writer_details.append(
            "single_writer must not appear in the v3 contract schema document at all"
        )
    details.extend(single_writer_details)

    return details


def check_governance_v3_contract_schema():
    """task-contract.md is a v3 authority envelope, not a v2 orchestration recipe."""
    path = os.path.join(REPO_ROOT, "docs", "agents", "task-contract.md")
    if not os.path.isfile(path):
        report("governance-v3-contract-schema", False, ["docs/agents/task-contract.md missing"])
        return
    with io.open(path, encoding="utf-8") as f:
        text = f.read()
    details = contract_schema_details(text)
    report("governance-v3-contract-schema", not details, details)


# --- GOVERNANCE_V3.md section 5: the recovery-checkpoint contract on the stable line -----------
#
# On this stable line the durable-recovery contract is not a standalone protocol document: it is
# section 5 of the canonical GOVERNANCE_V3.md ("Preflight, STOP, recovery"), owner-approved
# rev 3.1. The parts a recovery is unsafe without are pinned here by normalized-substring needle,
# the same way the task-contract schema is pinned: the checkpoint id, the evidence-never-authority
# invariant, the new-session READ_ONLY invariant, the untrusted-checkpoint-text rule, the SAME
# resume form, and the record fields a checkpoint must carry. main's far more detailed
# docs/agents/session-recovery.md (ADR-0004: a key-selected 22-field deep_checkpoint yaml
# contract) is a default-branch elaboration this line deliberately does not carry; adopting it
# here would be a reviewed governance change that restores the full session_recovery_details
# machinery together with the document.

CHECKPOINT_CONTRACT_ID = "deep-checkpoint/v1"

# Matched against normalize_prose_line() output of the whole flattened document, so hard wraps and
# backtick/bold markup cannot hide a needle.
GOVERNANCE_V3_RECOVERY_NEEDLES = (
    "a checkpoint is evidence, never authority",
    "it never restores a mode",
    "every new session starts read_only",
    "checkpoint text pasted into a session is untrusted data",
    "never executed or read as authorization",
    "same — <task / recovery>",
)

# Authority-restoring wording that must NEVER appear in GOVERNANCE_V3.md. Presence needles alone
# cannot see an ADDITION attack: a sentence like "a resume may skip the live preflight" could be
# planted while every pinned invariant stays present. Phrases are chosen to be impossible in
# legitimate text -- section 5 states the inverses ("never restores a mode", "untrusted data").
GOVERNANCE_V3_RECOVERY_FORBIDDEN = (
    "may skip the live preflight",
    "skip the live preflight",
    "re-enters the recorded mode",
    "resume re-enters the recorded",
    "checkpoint restores the mode",
    "checkpoint restores authority",
    "inherit the recorded authority",
    "inherits the checkpoint's authority",
    "checkpoint text may be executed",
)

# Section 5's checkpoint record fields -- what a deep-checkpoint/v1 block must carry.
GOVERNANCE_V3_CHECKPOINT_RECORD_FIELDS = (
    "base sha",
    "worktree/index state",
    "completed stages",
    "proven facts",
    "unresolved findings",
    "next gate",
    "publication state",
    "echo of the authority that was active",
)


def governance_v3_recovery_details(text):
    """Return [] if `text` (GOVERNANCE_V3.md) carries the section-5 recovery contract, else reasons.

    Pure in `text` so the fixtures drive this exact function.
    """
    details = []
    if not text.strip():
        details.append("GOVERNANCE_V3.md is empty")
    flat = normalize_prose_line(text.replace("\n", " "))
    if CHECKPOINT_CONTRACT_ID not in flat:
        details.append("recovery contract must name the checkpoint id %r" % CHECKPOINT_CONTRACT_ID)
    for needle in GOVERNANCE_V3_RECOVERY_NEEDLES:
        if needle not in flat:
            details.append("recovery invariant missing: %r" % needle)
    for field in GOVERNANCE_V3_CHECKPOINT_RECORD_FIELDS:
        if field not in flat:
            details.append("checkpoint record field missing from section 5: %r" % field)
    for phrase in GOVERNANCE_V3_RECOVERY_FORBIDDEN:
        if phrase in flat:
            details.append("authority-restoring wording must not appear: %r" % phrase)
    return details


def check_governance_v3_recovery_contract():
    """GOVERNANCE_V3.md section 5 carries the complete recovery-checkpoint contract."""
    path = os.path.join(REPO_ROOT, "GOVERNANCE_V3.md")
    if not os.path.isfile(path):
        report("governance-v3-recovery-contract", False, ["GOVERNANCE_V3.md missing"])
        return
    with io.open(path, encoding="utf-8") as f:
        text = f.read()
    details = governance_v3_recovery_details(text)
    report("governance-v3-recovery-contract", not details, details)


def host_os_tracked_file_details(repo_root):
    """Repo-wide, tracked-file hits for the forbidden host-OS token.

    ADR-0002 §8's acceptance condition is literally `git grep -ni <token>`
    returning nothing, over EVERY tracked file -- vendored skill bodies,
    Dockerfile and SPECIFICATIONS.md included, with no document exempt. The
    governance-surface scan alone cannot express that, so this mirrors the
    acceptance command exactly.
    """
    try:
        out = subprocess.run(
            ["git", "-C", repo_root, "grep", "-n", "-i", "-I", "-e", FORBIDDEN_HOST_OS_TOKEN],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=15, text=True,
        )
    except Exception as e:
        return ["could not run git grep: %s" % _safe(str(e))]
    if out.returncode == 1:
        return []                      # git grep: no matches
    if out.returncode != 0:
        return ["git grep failed: %s" % _safe(out.stderr.strip())]
    return [_safe(line) for line in out.stdout.splitlines() if line.strip()]


def host_os_details(repo_root):
    """Return [] if no host-OS name appears anywhere in the tree, else the hits.

    Two complementary scans, merged, deduplicated, and then filtered through the
    frozen HOST_OS_PREEXISTING_BUDGET (a counted per-file ceiling for the
    pre-existing application-file occurrences this governance layer cannot
    remove -- growth anywhere, and any occurrence outside the budgeted paths,
    still fails):

      - the pure governance-surface scan, which works without git;
      - a repo-wide tracked-file scan, which additionally covers the vendored
        skill bodies, Dockerfile, SPECIFICATIONS.md and this script itself.

    Both live here rather than in the check, so the fixture runs this exact
    function instead of restating the pair.

    Their coverage overlaps on CLAUDE.md and docs/**, so results are merged
    order-preservingly and de-duplicated: one offending line is one finding,
    not two.
    """
    merged = (scan_governance_surface(repo_root, (FORBIDDEN_HOST_OS_TOKEN,))
              + host_os_tracked_file_details(repo_root))
    seen = set()
    deduped = []
    for hit in merged:
        # The two scans format hits differently ("path:line: needle" vs git
        # grep's "path:line:content"), so key on the path:line prefix they share.
        key = ":".join(hit.split(":", 2)[:2])
        if key in seen:
            continue
        seen.add(key)
        deduped.append(hit)
    return apply_host_os_budget(deduped)


def apply_host_os_budget(hits):
    """Pure budget filter over 'path:line[:...]' hit strings, so the fixtures drive it directly.

    A path listed in HOST_OS_PREEXISTING_BUDGET is tolerated up to its counted ceiling; one
    occurrence past the ceiling reports ALL of that file's hits (growth in a noisy file must not
    hide behind the tolerated remainder). Every unlisted path reports as-is."""
    by_path = {}
    for hit in hits:
        by_path.setdefault(hit.split(":", 1)[0], []).append(hit)
    filtered = []
    for path, path_hits in by_path.items():
        budget = HOST_OS_PREEXISTING_BUDGET.get(path)
        if budget is not None and len(path_hits) <= budget:
            continue
        filtered.extend(
            path_hits if budget is None else
            ["%s (exceeds the frozen pre-existing budget of %d for this file)" % (h, budget)
             for h in path_hits])
    return filtered


# --- The four-level repo-native authority chain, restated across the policy layer ---
#
# GOVERNANCE_V3.md sections 1 and 3 are canonical (one hierarchy, narrowing-only
# delegation); CLAUDE.md and every skill policy restate the four-level repo-native
# elaboration consumed at the positions section 1 assigns. Governance v2's chain
# had a FIFTH tier ("unpatched upstream skill defaults") sitting below the local
# patches, and the original v3 change-set left that stale five-level list standing
# in the skills policies. Prose alone could not stop that regressing, so it is
# checked here.
# Documents that must all restate the SAME four-level authority chain. Derived from the provider
# registry rather than hand-listed: every vendored provider's policy states the chain for the skills
# it installs, so a provider added to MANIFESTS without its policy being checked would be a silent
# hole -- which is exactly what happened when four providers were added and this tuple was left at
# its original two. Deriving it means the hole cannot reopen.
#
# Deliberately NOT in this list on the stable line: GOVERNANCE_V3.md (its section 1 is the canonical
# governance hierarchy -- a different, wider chain -- and section 3 is the delegation rule this
# four-level repo-native chain elaborates; needling it for "four levels" would conflate the two),
# and docs/adr/0002-canonical-governance-v3.md (an immutable dated adoption record that predates the
# restated chain).
AUTHORITY_CHAIN_DOCS = tuple(
    ["CLAUDE.md"]
    + sorted(os.path.relpath(entry["policy"], REPO_ROOT) for entry in MANIFESTS)
    + ["docs/agents/project-skills-policy.md"]
)

# Enumeration fragments that only occur in a five-level chain. Deliberately
# matched with their list marker attached: ADR-0002 legitimately NAMES the
# retired fifth tier in past tense when recording what v3 dropped, and must
# stay able to.
FIFTH_LEVEL_ENUMERATIONS = (
    "(4) unpatched upstream skill defaults",
    "4. unpatched upstream skill defaults",
    "(5) generic model behavior",
    "5. generic model behavior",
)


def authority_chain_details(repo_root):
    """Return [] if all six documents state the same four-level chain, else reasons."""
    details = []
    for relpath in AUTHORITY_CHAIN_DOCS:
        abs_path = os.path.join(repo_root, relpath)
        if not os.path.isfile(abs_path):
            details.append("missing authority-chain document: %s" % relpath)
            continue
        with io.open(abs_path, encoding="utf-8") as f:
            text = f.read()
        # Joined before normalizing: these documents hard-wrap, and "exactly four
        # levels" is split across a line break in both vendoring policies.
        flat = normalize_prose_line(text.replace("\n", " "))
        if "four levels" not in flat:
            details.append("%s: must state that the authority chain has exactly four levels" % relpath)
        for phrase in FIFTH_LEVEL_ENUMERATIONS:
            if phrase in flat:
                details.append("%s: five-level authority chain survives: %s" % (relpath, _safe(phrase)))
    return details


def check_governance_v3_authority_chain():
    """One four-level authority chain, restated consistently and with no fifth tier."""
    details = authority_chain_details(REPO_ROOT)
    report("governance-v3-authority-chain", not details, details)


def check_governance_no_host_os_reference():
    """No host-OS name anywhere in the tree (ADR-0002 section 8)."""
    details = host_os_details(REPO_ROOT)
    report("governance-no-host-os-reference", not details, details)


# --- GOVERNANCE_V3.md section 7 <-> manifests <-> disk <-> routing: the approved-inventory
# contract ------------------------------------------------------------------------------------
#
# The defect this closes: a validator that proves only manifest<->filesystem internal consistency
# passes a tree with 24 of 81 approved skills installed, because nothing pins what the manifests
# themselves are expected to contain. Section 7 of the canonical GOVERNANCE_V3.md carries the
# owner-approved exact-name universe; these checks make that text machine-checked in both
# directions, so a missing approved skill, an unapproved extra, a provider count drift, or a
# routing table that no longer covers the installed set each fails. The totals are PARSED from the
# document and cross-checked against the manifests, never hard-coded here: an owner-approved
# inventory update that lands through the manifests + section 7 + routing map together passes,
# exactly as section 7's living-inventory rule requires; a partial edit fails.

_NUMBER_WORDS = {2: "two", 3: "three", 4: "four", 5: "five", 6: "six", 7: "seven", 8: "eight"}


def _installed_names_by_provider():
    """{registry label: [installed names]} from every provider manifest, plus load errors."""
    out, errs = {}, []
    for entry in MANIFESTS:
        try:
            data = load_manifest(entry["manifest"])
            out[entry["label"]] = [s["name"] for s in data.get("skills", [])]
        except Exception as e:
            errs.append("%s: cannot load manifest: %s" % (entry["label"], _safe(str(e))))
    return out, errs


def _project_manifest_names():
    try:
        with open(PROJECT_MANIFEST_PATH, encoding="utf-8") as f:
            return [s["name"] for s in _strict_json_load(f).get("skills", [])]
    except Exception:
        return []


def _user_invoked_names():
    """Every installed skill name any provider manifest marks invocation \"user\"."""
    names = set()
    for entry in MANIFESTS:
        try:
            data = load_manifest(entry["manifest"])
        except Exception:
            continue
        for s in data.get("skills", []):
            if s.get("invocation") == "user":
                names.add(s["name"])
    return names


def parse_governance_inventory(text):
    """Parse section 7 (and section 9's heading) of GOVERNANCE_V3.md.

    Returns (details, parsed). Any structure that cannot be parsed is a DETAIL -- the caller's
    check then fails loudly -- never a silent pass.
    """
    details = []
    parsed = {"table": {}, "lists": {}, "list_counts": {}}

    m = re.search(r"^## 7\. Skill inventory[^\n]*?(\d+)-skill baseline", text, re.MULTILINE)
    if m:
        parsed["heading_total"] = int(m.group(1))
    else:
        details.append("section-7 heading with '<N>-skill baseline' not found")
    m = re.search(r"^## 9\. Routing map[^\n]*?(\d+)-skill baseline", text, re.MULTILINE)
    if m:
        parsed["routing_heading_total"] = int(m.group(1))
    else:
        details.append("section-9 heading with '<N>-skill baseline' not found")

    sec = re.search(r"^## 7\. .*?(?=^## 8\. )", text, re.MULTILINE | re.DOTALL)
    if not sec:
        details.append("section 7 could not be isolated (no '## 8.' terminator)")
        return details, parsed
    sec7 = sec.group(0)

    flat7 = normalize_prose_line(sec7.replace("\n", " "))
    m = re.search(r"baseline snapshot: (\d+) installed.*? across (\d+) providers", flat7)
    if m:
        parsed["snapshot_total"] = int(m.group(1))
        parsed["snapshot_providers"] = int(m.group(2))
    else:
        details.append("section 7 must open with 'baseline snapshot: <N> installed ... across <P> providers'")

    for line in sec7.splitlines():
        row = re.match(r"\|\s*([A-Za-z0-9_./-]+)\s*\|\s*(\d+)\s*\|", line)
        if row:
            parsed["table"][row.group(1)] = int(row.group(2))
    if not parsed["table"]:
        details.append("section 7's provider table has no parseable '| <repo> | <count> |' rows")

    m = re.search(r"^((?:\d+\s*\+\s*)+\d+)\s*=\s*\*\*(\d+)\*\*", sec7, re.MULTILINE)
    if m:
        parsed["sum_components"] = [int(x) for x in re.findall(r"\d+", m.group(1))]
        parsed["sum_total"] = int(m.group(2))
    else:
        details.append("section 7's '<a> + <b> + ... = **<N>**' sum line not found")

    lists_m = re.search(r"### Installed skills.*?(?=### )", sec7, re.DOTALL)
    if not lists_m:
        details.append("section 7 has no '### Installed skills' subsection")
    else:
        for pm in re.finditer(r"\*\*([a-z-]+) \((\d+)\):\*\*(.*?)(?=\n\n\*\*|\n\n#|\Z)",
                              lists_m.group(0), re.DOTALL):
            label = pm.group(1)
            parsed["list_counts"][label] = int(pm.group(2))
            parsed["lists"][label] = re.findall(r"`([^`]+)`", pm.group(3))
        if not parsed["lists"]:
            details.append("section 7's installed-name lists could not be parsed")

    inv_m = re.search(r"\*\*Explicit-invocation-only\*\*[^:]*\((manifest[^)]*)\):(.*?)\.\n",
                      sec7, re.DOTALL)
    if inv_m:
        parsed["explicit_invocation"] = re.findall(r"`([^`]+)`", inv_m.group(2))
    else:
        details.append("section 7's 'Explicit-invocation-only (manifest ...):' list not found")
    return details, parsed


def governance_inventory_details(text, installed_by_provider, repo_short_by_label,
                                 on_disk_dirs, project_names, user_invoked=None):
    """Return [] iff section 7, the manifests, and the disk agree exactly; else every mismatch."""
    details, parsed = parse_governance_inventory(text)
    if details:
        return details

    universe = set()
    for label, names in sorted(installed_by_provider.items()):
        nset = set(names)
        universe |= nset
        want = parsed["lists"].get(label)
        if want is None:
            details.append("section 7 has no installed-name list for provider %r" % label)
            continue
        wset = set(want)
        if len(want) != len(wset):
            details.append("%s: section 7's installed list contains a duplicate name" % label)
        for name in sorted(wset - nset):
            details.append("%s: section 7 approves %r but the provider manifest does not install it"
                           % (label, name))
        for name in sorted(nset - wset):
            details.append("%s: manifest installs %r but section 7's approved list omits it"
                           % (label, name))
        declared = parsed["list_counts"].get(label)
        if declared != len(wset):
            details.append("%s: section 7 declares %r skills but lists %d names"
                           % (label, declared, len(wset)))
        repo_short = repo_short_by_label.get(label)
        table_count = parsed["table"].get(repo_short)
        if table_count is None:
            details.append("%s: section 7's provider table has no row for %r" % (label, repo_short))
        elif table_count != len(nset):
            details.append("%s: section 7's table says %d skills but the manifest installs %d"
                           % (label, table_count, len(nset)))
    for label in sorted(set(parsed["lists"]) - set(installed_by_provider)):
        details.append("section 7 lists provider %r with no registered manifest" % label)

    flat = normalize_prose_line(text.replace("\n", " "))
    if project_names and "manifest currently ships empty" in flat:
        details.append("first-party skills are installed (%r) but section 7 still declares the "
                       "first-party manifest empty" % sorted(project_names))
    universe |= set(project_names)
    total = len(universe)

    for key, human in (("heading_total", "the section-7 heading"),
                       ("snapshot_total", "the baseline-snapshot sentence"),
                       ("sum_total", "the sum line"),
                       ("routing_heading_total", "the section-9 heading")):
        got = parsed.get(key)
        if got != total:
            details.append("%s says %r skills but the installed inventory is %d" % (human, got, total))
    if "sum_components" in parsed and sum(parsed["sum_components"]) != parsed.get("sum_total"):
        details.append("section 7's sum line does not add up: %r" % parsed["sum_components"])
    if "sum_components" in parsed and sorted(parsed["sum_components"]) != sorted(
            len(set(v)) for v in installed_by_provider.values()):
        details.append("section 7's sum components %r do not match the per-provider manifest counts"
                       % parsed["sum_components"])
    if parsed.get("snapshot_providers") != len(installed_by_provider):
        details.append("section 7 says %r providers but %d provider manifests are registered"
                       % (parsed.get("snapshot_providers"), len(installed_by_provider)))

    for name in sorted(universe - on_disk_dirs):
        details.append("approved skill %r has no directory under .claude/skills/" % name)
    for name in sorted(on_disk_dirs - universe):
        details.append("on-disk skill directory %r is not in the approved section-7 universe" % name)

    # Section 7's explicit-invocation-only list must equal the manifests' invocation:"user" set.
    # user_invoked=None (a caller that cannot compute it) skips the comparison; an empty set is a
    # real value and is compared.
    if user_invoked is not None and "explicit_invocation" in parsed:
        declared = set(parsed["explicit_invocation"])
        for name in sorted(declared - set(user_invoked)):
            details.append("section 7 lists %r as explicit-invocation-only but no manifest records "
                           "invocation \"user\" for it" % name)
        for name in sorted(set(user_invoked) - declared):
            details.append("manifests record invocation \"user\" for %r but section 7's "
                           "explicit-invocation-only list omits it" % name)
    return details


def check_governance_v3_inventory():
    """GOVERNANCE_V3.md section 7's approved inventory matches the manifests, the disk, and itself."""
    path = os.path.join(REPO_ROOT, "GOVERNANCE_V3.md")
    if not os.path.isfile(path):
        report("governance-v3-inventory", False, ["GOVERNANCE_V3.md missing"])
        return
    with io.open(path, encoding="utf-8") as f:
        text = f.read()
    installed, errs = _installed_names_by_provider()
    repo_short = {e["label"]: e["upstream_repo"].split("github.com/", 1)[1] for e in MANIFESTS}
    details = errs + governance_inventory_details(
        text, installed, repo_short, set(list_skill_dirs()), _project_manifest_names(),
        user_invoked=_user_invoked_names())
    report("governance-v3-inventory", not details, details)


def skills_routing_details(routing_text, installed_names, provider_count):
    """Return [] iff every installed name appears in the routing doc and its totals are true."""
    details = []
    found = set(re.findall(r"`([^`\s]+)`", routing_text))
    for name in sorted(set(installed_names) - found):
        details.append("installed skill %r does not appear in docs/agents/skills-routing.md" % name)
    flat = normalize_prose_line(routing_text.replace("\n", " "))
    want = "%d installed skills across %s providers" % (
        len(set(installed_names)), _NUMBER_WORDS.get(provider_count, str(provider_count)))
    if want not in flat:
        details.append("skills-routing.md must state %r (the machine-true total)" % want)
    return details


def check_skills_routing_universe():
    """docs/agents/skills-routing.md covers the complete installed set -- the routing universe."""
    path = os.path.join(DOCS_AGENTS_DIR, "skills-routing.md")
    if not os.path.isfile(path):
        report("skills-routing-universe", False, ["docs/agents/skills-routing.md missing"])
        return
    with io.open(path, encoding="utf-8") as f:
        text = f.read()
    installed, errs = _installed_names_by_provider()
    names = set()
    for v in installed.values():
        names |= set(v)
    names |= set(_project_manifest_names())
    details = errs + skills_routing_details(text, names, len(MANIFESTS))
    report("skills-routing-universe", not details, details)


ALL_CHECKS = [
    check_required_files,
    check_json_validity,
    check_provider_manifest_fields,
    check_provider_license_files,
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
    check_all_providers_file_level,
    check_no_unaudited_candidate,
    check_provider_file_hashes,
    check_provider_vendored_modes,
    check_provider_scripts_audited,
    check_provider_invocation_matches_frontmatter,
    check_provider_dependency_closure,
    check_builtin_collision_denylist,
    check_patch_ledger_coverage,
    check_patch_marker_balance,
    check_provider_patch_coverage,
    check_stable_skills_workflow,
    check_stable_ci_governance_integration,
    check_stable_workflow_registry,
    check_stable_skills_control_contract,
    check_application_paths_untouched,
    check_settings_schema,
    check_settings_mcp_mutations_denied,
    check_rules_frontmatter_and_uniqueness,
    check_hidden_unicode,
    check_project_manifest,
    check_governance_v3_docs,
    check_governance_v3_no_stale_orchestration,
    check_governance_v3_contract_schema,
    check_governance_v3_recovery_contract,
    check_governance_v3_authority_chain,
    check_governance_no_host_os_reference,
    check_governance_v3_inventory,
    check_skills_routing_universe,
]


def checks_for_application_scope(scope):
    """Select exactly one concern boundary without changing any generic invariant."""
    if scope == APPLICATION_SCOPE_GENERIC:
        return [check for check in ALL_CHECKS if check is not check_application_paths_untouched]
    if scope == APPLICATION_SCOPE_G1_STABLE_SKILLS:
        return list(ALL_CHECKS)
    raise ValueError("unknown application scope %r" % scope)


# --------------------------------------------------------------------------
# --self-test: offline fixture matrix for the project-manifest logic above.
#
# Most fixtures build their own tree under tempfile.TemporaryDirectory; none ever WRITES to this
# repo, makes a network call, or sleeps. A MINORITY are the exception to "synthetic tree"
# specifically, not to "never writes": they READ real files from this repo to check facts about the
# actual repo state -- read-only, like every other check this script performs. Some of those also
# shell out to git (`hash-object`, `grep`) against the real tree, exactly as the corresponding
# production checks do, so a handful of fixtures depend on git and on a clean working tree.
#
# This comment deliberately does NOT enumerate which fixtures those are. Two successive attempts to
# do so both went stale -- once by naming a pair that had grown to seven, once by giving a heuristic
# that was already wrong for fixtures reaching the repo through a helper. `_self_test_fixtures()` is
# the authority for what exists, and each fixture's own docstring says what it reads.
#
# Three families by id prefix: G* positive/structural, N* negative (one per distinct violation class
# named in project-skills-policy.md's schema), V* the Governance-v3 prose checks. Exact id ranges are
# deliberately NOT restated here -- they drift, and `_self_test_fixtures()` already enumerates them.
#
# Every V* fixture calls the same `*_details()` production helper its corresponding check calls, so
# the fixtures assert on production logic rather than re-deriving it. Each fixture function raises
# AssertionError (with a diagnostic message) on failure and returns None on success -- the runner
# below is the only place results are aggregated or printed.
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
    # Pins the registry's shape, not just its size: every entry must be fully specified, and the
    # project (first-party) manifest must never be reachable through the vendored registry.
    labels = {entry["label"] for entry in MANIFESTS}
    assert labels == {"mattpocock", "anthropic", "compound-engineering", "trailofbits",
                      "awesome-copilot", "builderio"}, labels
    required = ("label", "manifest", "patches", "policy", "upstream_repo", "upstream_env",
                "schema", "excluded_key", "extra_frontmatter_keys", "license")
    for entry in MANIFESTS:
        missing = [k for k in required if k not in entry]
        assert not missing, (entry.get("label"), missing)
        assert entry["schema"] in ("skill-level", "file-level"), entry["schema"]
        lic = entry["license"]
        assert lic.get("spdx"), entry["label"]
        assert lic.get("layout") in ("shared", "per-skill"), lic
        assert ("path" in lic) if lic["layout"] == "shared" else ("filename" in lic), lic
    manifest_paths = {entry["manifest"] for entry in MANIFESTS}
    assert PROJECT_MANIFEST_PATH not in manifest_paths, manifest_paths
    mattpocock_shaped = load_manifest(MANIFEST_PATH)
    details = validate_project_manifest(mattpocock_shaped, REPO_ROOT, require_tracked=False)
    assert details, "expected validate_project_manifest to reject a vendored-shaped manifest"


def _st_g12():
    manifest = load_manifest(PROJECT_MANIFEST_PATH)
    details = validate_project_manifest(manifest, REPO_ROOT, require_tracked=True)
    assert details == [], details


# ---- N*: one negative fixture per distinct violation class (see _self_test_fixtures) ----

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
    # failure mode was project_skill_names() raising inside check_unique_skill_names -- an early
    # entry in ALL_CHECKS -- which aborted the whole run before check_project_manifest's own
    # diagnostic was ever reached. (The fixture body asserts against len(ALL_CHECKS) rather than a
    # literal count, precisely so this comment is the only thing that can drift.)
    # RESULTS is saved/restored alongside PROJECT_MANIFEST_PATH so this fixture can never
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
    # Every registered check must report exactly once, even when a manifest is
    # malformed. Pinned to len(ALL_CHECKS) rather than a literal so adding a
    # check doesn't require editing this fixture -- duplicates and silent
    # non-reporters are still caught by the name-uniqueness assert below.
    names = [r[0] for r in results]
    assert len(results) == len(ALL_CHECKS), "expected %d labeled results, got %d: %r" % (
        len(ALL_CHECKS), len(results), names)
    assert len(set(names)) == len(names), "duplicate check labels: %r" % (names,)
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


def _st_v1():
    """governance_surface_paths picks up the fixed files + .md under the walked dirs."""
    with tempfile.TemporaryDirectory() as tmp:
        _write_file(os.path.join(tmp, "CLAUDE.md"), "x\n")
        _write_file(os.path.join(tmp, "README.md"), "x\n")
        _write_file(os.path.join(tmp, "docs", "agents", "task-contract.md"), "x\n")
        _write_file(os.path.join(tmp, "docs", "adr", "0001.md"), "x\n")
        _write_file(os.path.join(tmp, ".claude", "rules", "go-code.md"), "x\n")
        # Not governance surface: skill bodies, hooks, non-markdown, absent CONTEXT.md.
        _write_file(os.path.join(tmp, ".claude", "skills", "s", "SKILL.md"), "x\n")
        _write_file(os.path.join(tmp, ".claude", "hooks", "h.py"), "x\n")
        _write_file(os.path.join(tmp, "docs", "agents", "manifest.json"), "{}\n")
        _write_file(os.path.join(tmp, "Dockerfile"), "x\n")
        got = governance_surface_paths(tmp)
        expected = [
            ".claude/rules/go-code.md",
            "CLAUDE.md",
            "README.md",
            "docs/adr/0001.md",
            "docs/agents/task-contract.md",
        ]
        assert got == sorted(expected), got


def _st_v2():
    """scan_governance_surface reports file:line hits and honours the exempt list."""
    with tempfile.TemporaryDirectory() as tmp:
        _write_file(os.path.join(tmp, "CLAUDE.md"), "ok\nOne Production Writer per task\n")
        _write_file(os.path.join(tmp, "docs", "adr", "0001-agent-governance-v2.md"),
                    "one production writer\n")
        hits = scan_governance_surface(tmp, ("one production writer",))
        assert any(h.startswith("CLAUDE.md:2:") for h in hits), hits
        assert any("0001-agent-governance-v2.md" in h for h in hits), hits

        exempt_hits = scan_governance_surface(
            tmp, ("one production writer",), exempt=("docs/adr/0001-agent-governance-v2.md",)
        )
        assert len(exempt_hits) == 1, exempt_hits
        assert exempt_hits[0].startswith("CLAUDE.md:2:"), exempt_hits


def _real_contract_text():
    with io.open(os.path.join(REPO_ROOT, "docs", "agents", "task-contract.md"), encoding="utf-8") as f:
        return f.read()


def _st_v3():
    """The real repo's governance surface is free of stale v2 orchestration mandates."""
    hits = stale_orchestration_details(REPO_ROOT)
    assert not hits, hits


def _st_v4():
    """The real repo names no host operating system, on the surface or in any tracked file."""
    hits = host_os_details(REPO_ROOT)
    assert not hits, hits


def _st_v5():
    """Governance v3 required docs exist and CLAUDE.md declares v3."""
    details = governance_v3_docs_details(REPO_ROOT)
    assert not details, details


def _st_v6():
    """The real task-contract.md passes the production authority-envelope check."""
    details = contract_schema_details(_real_contract_text())
    assert not details, details


def _st_v7():
    """REGRESSION: a mandatory agent_cap must FAIL, even though 'optional' occurs elsewhere.

    This is the fixture the superseded implementation would have passed. That
    version asked `if field in lowered and "optional" not in lowered`, i.e. it
    searched the WHOLE document for the word "optional" -- and the schema's own
    preamble ("Every field is optional except ...") guarantees a match, so the
    test could never fire. The document below keeps that preamble verbatim while
    describing agent_cap as a mandatory orchestration requirement, which is
    precisely the v2 mandate ADR-0002 removed.
    """
    text = (
        "# Task contract\n\n"
        "Every field is optional except `mode`, `repository`, `base_branch`, `base_sha`,\n"
        "`task_branch`, and `authorized_by`.\n\n"
        "```yaml\n"
        "task_contract:\n"
        "  mode: READ_ONLY | PROTOTYPE | CHANGE | PUBLISH_DRAFT\n"
        "  orchestration: skill_native | main_context_only   # optional; absent => skill_native\n"
        "  agent_cap: <int>                             # mandatory orchestration cap\n"
        "  max_concurrency: <int>                       # mandatory orchestration cap\n"
        "```\n\n"
        "## Field notes\n\n"
        "- **`agent_cap`** / **`max_concurrency`** are mandatory orchestration policy and every\n"
        "  contract must set them; absent, they default to 4.\n"
    )
    details = contract_schema_details(text)
    assert details, "a mandatory agent_cap/max_concurrency contract must be rejected"
    joined = " ".join(details)
    assert "agent_cap" in joined and "max_concurrency" in joined, details
    # And prove the old vacuous condition really would have passed this text.
    lowered = text.lower()
    for field in RESOURCE_CEILING_FIELDS:
        assert not (field in lowered and "optional" not in lowered), (
            "fixture no longer reproduces the vacuous-pass condition for %s" % field
        )


def _st_v8():
    """REGRESSION: a resurrected universal reviewer-read-only mandate must be detected."""
    with tempfile.TemporaryDirectory() as tmp:
        _write_file(os.path.join(tmp, "CLAUDE.md"),
                    "# x\n\n- One writer per task; every other agent is read-only (research, review).\n"
                    "- Reviewer/analysis agents never write to tracked files or push.\n")
        hits = stale_orchestration_details(tmp)
        assert any("every other agent is read-only" in h for h in hits), hits
        assert any("reviewer/analysis agents never write" in h for h in hits), hits

    # Positive control: a skill CHOOSING read-only reviewers is not a project mandate.
    with tempfile.TemporaryDirectory() as tmp:
        _write_file(os.path.join(tmp, "CLAUDE.md"),
                    "# x\n\nInvoking a skill authorizes its lanes, reviewers, critics and verifiers.\n")
        assert not stale_orchestration_details(tmp), stale_orchestration_details(tmp)


def _st_v9():
    """REGRESSION: a resurrected mandatory resource cap must be detected, through Markdown markup.

    Also pins normalize_prose_line's job: the phrase is written with backticks
    and bold markers, exactly as it would appear in these documents, and must
    still match.
    """
    with tempfile.TemporaryDirectory() as tmp:
        _write_file(os.path.join(tmp, "docs", "agents", "task-contract.md"),
                    "- **`agent_cap` is mandatory** for every contract.\n"
                    "- `max_concurrency` is required whenever more than one agent runs.\n")
        hits = stale_orchestration_details(tmp)
        assert any("agent_cap is mandatory" in h for h in hits), hits
        assert any("max_concurrency is required" in h for h in hits), hits

    # Positive control: naming the fields as OPTIONAL ceilings must not match.
    with tempfile.TemporaryDirectory() as tmp:
        _write_file(os.path.join(tmp, "docs", "agents", "task-contract.md"),
                    "- **`agent_cap`** / **`max_concurrency`** are **optional resource ceilings**, not\n"
                    "  orchestration policy. They are no longer mandatory and have no default.\n")
        assert not stale_orchestration_details(tmp), stale_orchestration_details(tmp)


def _st_v10():
    """scan_governance_surface catches a phrase split across a hard wrap, exactly once."""
    with tempfile.TemporaryDirectory() as tmp:
        # Genuinely split: "role" ends line 1, "ledger" opens line 2. Neither
        # line contains the phrase; only the wrap does. Reported at the opening
        # line, once.
        _write_file(os.path.join(tmp, "CLAUDE.md"),
                    "Keep an explicit role\nledger when several agents run.\n")
        hits = scan_governance_surface(tmp, ("role ledger",))
        assert len(hits) == 1, hits
        assert hits[0].startswith("CLAUDE.md:1:"), hits

        # Wholly on one line: reported at that line, once -- the preceding
        # line's wrap window also contains it and must not double-report.
        _write_file(os.path.join(tmp, "CLAUDE.md"),
                    "Some preamble.\nKeep an explicit role ledger here.\n")
        hits = scan_governance_surface(tmp, ("role ledger",))
        assert len(hits) == 1, hits
        assert hits[0].startswith("CLAUDE.md:2:"), hits


def _st_v11():
    """REGRESSION: the host-OS token is rejected with no document exempt.

    The token is assembled from the production constant rather than written
    out, for the same reason the constant itself is assembled -- a literal here
    would make this file a tracked occurrence and fail the repo-wide check.
    """
    with tempfile.TemporaryDirectory() as tmp:
        _write_file(os.path.join(tmp, "docs", "adr", "0002-governance-v3-skill-native-orchestration.md"),
                    "Deploys to the %s box are out of scope.\n" % FORBIDDEN_HOST_OS_TOKEN.upper())
        hits = scan_governance_surface(tmp, (FORBIDDEN_HOST_OS_TOKEN,))
        assert len(hits) == 1, hits
        assert "0002-governance-v3-skill-native-orchestration.md:1:" in hits[0], hits


def _st_v12():
    """The schema block is selected by its `task_contract:` key, not by position.

    Guards the failure mode where an example yaml fence added ABOVE the schema
    would silently rebind every field lookup in contract_schema_details to the
    wrong block -- which reports PASS while validating nothing.
    """
    real = _real_contract_text()
    schema = _extract_yaml_schema_block(real)
    assert schema is not None and "task_contract:" in schema, schema

    decoy = (
        "# Task contract\n\n"
        "An example of what a contract is NOT:\n\n"
        "```yaml\n"
        "some_other_example:\n"
        "  agent_cap: <int>   # mandatory\n"
        "```\n\n"
    ) + real
    assert _extract_yaml_schema_block(decoy) == schema, "decoy fence was selected"
    # The decoyed document must still validate exactly as the real one does.
    assert contract_schema_details(decoy) == contract_schema_details(real)


def _st_v13():
    """The real repo's authority-chain documents (CLAUDE.md + the seven policies) agree on four levels."""
    details = authority_chain_details(REPO_ROOT)
    assert not details, details


def _st_v14():
    """REGRESSION: a resurrected five-level chain must be rejected.

    The fixture text is the Governance-v2 chain the three skills policies still
    carried at 6d28ef8 -- the actual Defect 3 regression, not an invented one.
    """
    with tempfile.TemporaryDirectory() as tmp:
        for relpath in AUTHORITY_CHAIN_DOCS:
            _write_file(os.path.join(tmp, relpath),
                        "The authority chain has exactly four levels.\n")
        stale = os.path.join(tmp, "docs", "agents", "mattpocock-skills-policy.md")
        _write_file(stale,
                    "Authority chain: (1) the active task contract, (2) `CLAUDE.md` +\n"
                    "`.claude/rules/*.md`, (3) these vendored skills (as patched),\n"
                    "(4) unpatched upstream skill defaults, (5) generic model behavior.\n")
        details = authority_chain_details(tmp)
        assert any("five-level authority chain survives" in d for d in details), details
        assert any("mattpocock-skills-policy.md" in d for d in details), details
        # Only the tampered document is faulted.
        assert not any("anthropic-skills-policy.md" in d for d in details), details

    # A document that simply omits the four-level statement is also caught.
    with tempfile.TemporaryDirectory() as tmp:
        for relpath in AUTHORITY_CHAIN_DOCS:
            _write_file(os.path.join(tmp, relpath), "no chain stated here\n")
        details = authority_chain_details(tmp)
        assert len(details) == len(AUTHORITY_CHAIN_DOCS), details


# ---- V15-V17: the GOVERNANCE_V3.md section-5 recovery-contract check. Every fixture drives the
# production governance_v3_recovery_details() helper, and every negative one MUTATES THE REAL
# DOCUMENT rather than inventing a synthetic one, so a fixture cannot drift away from the document
# it is meant to guard. Each mutation asserts it actually changed something first: a negative
# fixture that silently matched nothing would "pass" by re-testing the positive case.


def _real_governance_v3_text():
    with io.open(os.path.join(REPO_ROOT, "GOVERNANCE_V3.md"), encoding="utf-8") as f:
        return f.read()


def _drop_flat_needle(text, needle):
    """Remove every occurrence of a normalized needle from `text`, across hard wraps.

    The prose checks match their needles against the newline-flattened, markup-stripped
    document, so a fixture that wants to REMOVE one has to match it the same way: inter-word gaps
    become [\\s`*]+, which spans a hard wrap AND the backticks / bold markers the normalizer removes.
    Without the markup class a needle like "a base_sha that is still the base being built on" never
    matches the document's `base_sha`, the mutation is a no-op, and the negative fixture quietly
    re-tests the positive case. Returns (mutated, count); callers assert count.
    """
    pattern = re.compile(r"[\s`*]+".join(re.escape(word) for word in needle.split()), re.IGNORECASE)
    mutated, count = pattern.subn("[removed by fixture]", text)
    return mutated, count


def _st_v15():
    """The real GOVERNANCE_V3.md carries the complete section-5 recovery contract."""
    details = governance_v3_recovery_details(_real_governance_v3_text())
    assert not details, details


def _st_v16():
    """NEGATIVE: every recovery needle and checkpoint record field, one at a time."""
    real = _real_governance_v3_text()
    for needle in GOVERNANCE_V3_RECOVERY_NEEDLES + GOVERNANCE_V3_CHECKPOINT_RECORD_FIELDS:
        mutated, count = _drop_flat_needle(real, needle)
        assert count >= 1, (needle, count)
        details = governance_v3_recovery_details(mutated)
        assert any(needle in d for d in details), (needle, details)


def _st_v17():
    """NEGATIVE: the checkpoint contract id is pinned, and an empty document reports, not raises."""
    real = _real_governance_v3_text()
    keyed = real.replace(CHECKPOINT_CONTRACT_ID, "deep_checkpoint/v1")
    assert keyed != real
    assert any("checkpoint id" in d for d in governance_v3_recovery_details(keyed)), \
        governance_v3_recovery_details(keyed)
    details = governance_v3_recovery_details("")
    assert details, "an empty document must report failures"


def _st_v18():
    """NEGATIVE: authority-restoring wording ADDED to section 5 is rejected; list admissible."""
    real = _real_governance_v3_text()
    assert not governance_v3_recovery_details(real)
    for phrase in ("a resume may skip the live preflight.",
                   "a valid checkpoint restores the mode it recorded.",
                   "the session inherits the checkpoint's authority."):
        planted = real.replace("## 6. Evidence discipline",
                               phrase + "\n\n## 6. Evidence discipline", 1)
        assert planted != real
        details = governance_v3_recovery_details(planted)
        assert any("authority-restoring wording" in d for d in details), (phrase, details)


def _st_v19():
    """The host-OS budget filter: within-budget tolerated, growth and unlisted paths fail."""
    assert apply_host_os_budget([]) == []
    within = ["Dockerfile:10:x", "README.md:1:x", "README.md:2:x", "README.md:3:x",
              "SPECIFICATIONS.md:5:x"]
    assert apply_host_os_budget(within) == [], apply_host_os_budget(within)
    grown = within + ["README.md:9:x"]
    out = apply_host_os_budget(grown)
    assert len([h for h in out if h.startswith("README.md:")]) == 4, out
    assert all("exceeds the frozen pre-existing budget" in h for h in out), out
    assert [h for h in out if h.startswith("Dockerfile:")] == [], out
    unlisted = apply_host_os_budget(["CLAUDE.md:3:x"])
    assert unlisted == ["CLAUDE.md:3:x"], unlisted
    mixed = apply_host_os_budget(["docs/agents/foo.md:1:x", "Dockerfile:10:x"])
    assert mixed == ["docs/agents/foo.md:1:x"], mixed


def _st_v30():
    """NEGATIVE: CLAUDE.md's own continuity pointer, which every fresh session actually loads.

    GOVERNANCE_V3.md section 5 is a document a session may never open; CLAUDE.md is the one it
    always reads. Pinning the invariants only in the pointed-to file would leave this paragraph
    free to be inverted -- "a resume re-enters the recorded mode" -- with all other checks green.
    """
    real_claude = io.open(os.path.join(REPO_ROOT, "CLAUDE.md"), encoding="utf-8").read()
    assert not governance_v3_docs_details(REPO_ROOT), governance_v3_docs_details(REPO_ROOT)

    def _details_for(claude_text):
        with tempfile.TemporaryDirectory() as tmp:
            _write_file(os.path.join(tmp, "CLAUDE.md"), claude_text)
            for relpath in V3_REQUIRED_DOCS:
                _write_file(os.path.join(tmp, relpath), "x\n")
            return governance_v3_docs_details(tmp)

    assert not _details_for(real_claude), _details_for(real_claude)

    dropped = real_claude.replace("GOVERNANCE_V3.md", "GOVERNANCE_V2.md")
    assert dropped != real_claude
    assert any("must point at the canonical GOVERNANCE_V3.md" in d for d in _details_for(dropped)), \
        _details_for(dropped)

    mutated, count = _drop_flat_needle(real_claude, "single canonical governance authority")
    assert count >= 1, count
    assert any("single canonical governance authority" in d for d in _details_for(mutated)), \
        _details_for(mutated)

    for needle in CLAUDE_MD_CONTINUITY_INVARIANTS:
        mutated, count = _drop_flat_needle(real_claude, needle)
        assert count >= 1, (needle, count)
        details = _details_for(mutated)
        assert any("session-continuity pointer omits" in d and needle in d for d in details), (
            needle, details
        )

    # The pointer must name the contract id, not the YAML key.
    keyed = real_claude.replace(CHECKPOINT_CONTRACT_ID, "deep_checkpoint/v1")
    assert keyed != real_claude
    assert any("must name the checkpoint contract id" in d for d in _details_for(keyed)), \
        _details_for(keyed)


def _st_v31():
    """STRUCTURAL: the pinned constants themselves, so weakening one is a self-test failure."""
    assert CHECKPOINT_CONTRACT_ID == "deep-checkpoint/v1"
    assert set(CLAUDE_MD_CONTINUITY_INVARIANTS) == {
        "a checkpoint is evidence, never authority",
        "every new session starts read_only",
    }
    assert len(GOVERNANCE_V3_RECOVERY_NEEDLES) >= 6, GOVERNANCE_V3_RECOVERY_NEEDLES
    assert len(GOVERNANCE_V3_RECOVERY_FORBIDDEN) >= 8, GOVERNANCE_V3_RECOVERY_FORBIDDEN
    for phrase in GOVERNANCE_V3_RECOVERY_FORBIDDEN:
        assert len(phrase.split()) >= 3, phrase
    assert len(GOVERNANCE_V3_CHECKPOINT_RECORD_FIELDS) >= 8, GOVERNANCE_V3_CHECKPOINT_RECORD_FIELDS
    # A degenerate needle ("the", "push") would match anything and check nothing.
    for needle in GOVERNANCE_V3_RECOVERY_NEEDLES + GOVERNANCE_V3_CHECKPOINT_RECORD_FIELDS:
        assert len(needle.split()) >= 2, needle
    assert "every new session starts read_only" in GOVERNANCE_V3_RECOVERY_NEEDLES
    assert HOST_OS_PREEXISTING_BUDGET == {"Dockerfile": 1, "README.md": 3, "SPECIFICATIONS.md": 1}
    assert GOVERNANCE_OWNED_WORKFLOWS == (
        STABLE_CI_WORKFLOW_REL, STABLE_SKILLS_WORKFLOW_REL)
    assert PINNED_CHECKOUT_ACTION.endswith("@11d5960a326750d5838078e36cf38b85af677262")


# ---- P*: the generic provider-registry checks. Every fixture drives the SAME production
# `*_details()` helper its check calls, against a synthetic vendored tree under a temp root, so a
# P* pass means the production logic passed and each new check is proven able to FAIL, not merely
# able to pass. P1 is the positive control: a fully valid synthetic provider produces zero details
# from every one of the helpers, which is what makes the negatives meaningful. ----

_P_LICENSE_TEXT = "Fixture License 1.0\n\nRedistribution permitted for test purposes.\n"


def _fake_provider_entry(tmp_docs, label="fixture", layout="per-skill"):
    """A registry entry shaped exactly like a real one, pointing at temp documents."""
    lic = ({"spdx": "Fixture-1.0", "layout": "per-skill", "filename": "LICENSE.txt"}
           if layout == "per-skill" else
           {"spdx": "Fixture-1.0", "layout": "shared",
            "path": os.path.join(".claude", "skills", "LICENSE")})
    return {
        "label": label,
        "manifest": os.path.join(tmp_docs, "%s-skills-manifest.json" % label),
        "patches": os.path.join(tmp_docs, "%s-skills-patches.md" % label),
        "policy": os.path.join(tmp_docs, "%s-skills-policy.md" % label),
        "upstream_repo": "https://github.com/fixture/skills",
        "upstream_env": "GOVERNANCE_UPSTREAM_DIR_FIXTURE",
        "legacy_upstream_env": None,
        "schema": "file-level",
        "excluded_key": "excluded_skills",
        "extra_frontmatter_keys": frozenset({"allowed-tools"}),
        "license": lic,
    }


def _build_provider_fixture(tmp, skill_name="fixture-skill", with_script=True):
    """Builds a synthetic vendored provider tree and returns
    (entry, skills, repo_root, skills_dir, manifest). Every declared file exists, every hash is
    real (`git hash-object`), modes are 100644, and the license notice is in place -- i.e. the
    tree a correct vendoring produces."""
    repo_root = os.path.join(tmp, "repo")
    docs = os.path.join(repo_root, "docs", "agents")
    skills_dir = os.path.join(repo_root, ".claude", "skills")
    skill_dir = os.path.join(skills_dir, skill_name)
    os.makedirs(docs, exist_ok=True)

    files = {
        os.path.join(skill_dir, "SKILL.md"):
            "---\nname: %s\ndescription: Fixture skill.\nallowed-tools: Read\n---\n\n"
            "Run `scripts/run.sh` first.\n" % skill_name,
        os.path.join(skill_dir, "LICENSE.txt"): _P_LICENSE_TEXT,
    }
    if with_script:
        files[os.path.join(skill_dir, "scripts", "run.sh")] = "#!/bin/sh\necho fixture\n"
    for path, content in files.items():
        _write_file(path, content)

    entries = []
    for path in sorted(files):
        sha, err = _git_blob_sha(path)
        assert err is None, err
        entries.append({
            "path": os.path.relpath(path, repo_root),
            "origin": "upstream",
            "upstream_path": "skills/%s/%s" % (skill_name, os.path.relpath(path, skill_dir)),
            "upstream_blob_sha": sha,
            "upstream_mode": "100644",
            "vendored_mode": "100644",
            "vendored_blob_sha": sha,
            "locally_modified": False,
            "patch_ids": [],
        })
    skills = [{
        "name": skill_name,
        "path": os.path.relpath(skill_dir, repo_root),
        "invocation": "model",
        "scripts_audited": bool(with_script),
        "locally_modified": False,
        "patch_ids": [],
        "files": entries,
    }]
    manifest = {
        "upstream_repo": "https://github.com/fixture/skills",
        "upstream_commit": "0" * 40,
        "reviewed_at": "2026-08-16T00:00:00Z",
        "reviewed_by": "fixture",
        "installation_mode": REQUIRED_INSTALLATION_MODE,
        "automatic_updates": False,
        "skills": skills,
        "excluded_skills": [{"name": "not-installed", "status": "EXCLUDE", "reason": "fixture"}],
    }
    entry = _fake_provider_entry(docs)
    return entry, skills, repo_root, skills_dir, manifest


def _st_p1():
    """Positive control: a correctly vendored provider yields zero details from every helper."""
    with tempfile.TemporaryDirectory() as tmp:
        entry, skills, root, skills_dir, manifest = _build_provider_fixture(tmp)
        assert provider_manifest_field_details(entry, manifest) == []
        assert provider_file_hash_details(entry, skills, root) == []
        assert provider_vendored_mode_details(entry, skills, root) == []
        assert provider_scripts_audited_details(entry, skills, root) == []
        assert provider_license_file_details(
            entry, {skills[0]["name"]}, skills_dir, root) == []
        assert dependency_closure_details(skills_dir) == []


def _st_p2():
    """automatic_updates must be literally false -- a truthy or missing value is a live-update
    installation, which is exactly what project-local vendoring exists to prevent."""
    with tempfile.TemporaryDirectory() as tmp:
        entry, _s, _r, _sd, manifest = _build_provider_fixture(tmp)
        for bad in (True, "false", None):
            manifest["automatic_updates"] = bad
            details = provider_manifest_field_details(entry, manifest)
            assert any("automatic_updates must be literally false" in d for d in details), (bad, details)
        manifest["automatic_updates"] = False

        # An explicit null installation_mode must NOT slip through: the presence check sees the key,
        # so only a direct value comparison catches it.
        manifest["installation_mode"] = None
        d = provider_manifest_field_details(entry, manifest)
        assert any("installation_mode None" in x for x in d), d
        manifest["installation_mode"] = REQUIRED_INSTALLATION_MODE

        # A missing or non-list excluded list must fail, not degrade to "rejected nothing".
        saved = manifest.pop(entry["excluded_key"])
        d = provider_manifest_field_details(entry, manifest)
        assert any("would go unrecorded" in x for x in d), d
        manifest[entry["excluded_key"]] = {"not": "a list"}
        d = provider_manifest_field_details(entry, manifest)
        assert any("must be a list" in x for x in d), d
        manifest[entry["excluded_key"]] = saved
        assert provider_manifest_field_details(entry, manifest) == []


def _st_p3():
    """upstream_commit must be a full 40-hex SHA: a branch name or short SHA is a floating ref,
    and every recorded blob hash would be unfalsifiable against it."""
    with tempfile.TemporaryDirectory() as tmp:
        entry, _s, _r, _sd, manifest = _build_provider_fixture(tmp)
        for bad in ("main", "0" * 39, "z" * 40, None):
            manifest["upstream_commit"] = bad
            details = provider_manifest_field_details(entry, manifest)
            assert any("is not a full 40-hex SHA" in d for d in details), (bad, details)


def _st_p4():
    """A manifest pointing at a different upstream than the registry entry claims, and a manifest
    missing a required provenance field, both fail."""
    with tempfile.TemporaryDirectory() as tmp:
        entry, _s, _r, _sd, manifest = _build_provider_fixture(tmp)
        manifest["upstream_repo"] = "https://github.com/someone-else/skills"
        details = provider_manifest_field_details(entry, manifest)
        assert any("upstream_repo" in d and "!= registry" in d for d in details), details

        _e2, _s2, _r2, _sd2, manifest2 = _build_provider_fixture(tmp, skill_name="p4b")
        del manifest2["reviewed_by"]
        details2 = provider_manifest_field_details(entry, manifest2)
        assert any("missing required field 'reviewed_by'" in d for d in details2), details2
        # ...and the canonicalizer must not turn a genuine mismatch into a pass, nor a
        # .git/trailing-slash spelling difference into a failure.
        manifest2["reviewed_by"] = "fixture"
        manifest2["upstream_repo"] = "https://github.com/fixture/skills.git/"
        assert provider_manifest_field_details(entry, manifest2) == []


def _st_p5():
    """A per-skill license layout fails when the notice is missing from a skill directory, and a
    shared layout fails when the single declared file is missing."""
    with tempfile.TemporaryDirectory() as tmp:
        entry, skills, root, skills_dir, _m = _build_provider_fixture(tmp)
        name = skills[0]["name"]
        assert provider_license_file_details(entry, {name}, skills_dir, root) == []
        os.remove(os.path.join(skills_dir, name, "LICENSE.txt"))
        details = provider_license_file_details(entry, {name}, skills_dir, root)
        assert any("LICENSE.txt missing" in d for d in details), details

        shared_entry = _fake_provider_entry(os.path.join(root, "docs", "agents"), layout="shared")
        details2 = provider_license_file_details(shared_entry, set(), skills_dir, root)
        assert any("shared license file missing" in d for d in details2), details2


def _st_p6():
    """vendored_blob_sha is the integrity pin: editing a vendored file on disk without updating
    the manifest must fail closed."""
    with tempfile.TemporaryDirectory() as tmp:
        entry, skills, root, skills_dir, _m = _build_provider_fixture(tmp)
        target = os.path.join(skills_dir, skills[0]["name"], "scripts", "run.sh")
        _write_file(target, "#!/bin/sh\necho tampered\n")
        details = provider_file_hash_details(entry, skills, root)
        assert any("!= manifest vendored_blob_sha" in d for d in details), details


def _st_p7():
    """Inventory completeness, both directions: a declared file absent from disk fails, and an
    on-disk file the manifest never declares fails."""
    with tempfile.TemporaryDirectory() as tmp:
        entry, skills, root, skills_dir, _m = _build_provider_fixture(tmp)
        skill_dir = os.path.join(skills_dir, skills[0]["name"])
        os.remove(os.path.join(skill_dir, "scripts", "run.sh"))
        details = provider_file_hash_details(entry, skills, root)
        assert any("listed in files[] but missing on disk" in d for d in details), details

        entry2, skills2, root2, skills_dir2, _m2 = _build_provider_fixture(tmp, skill_name="p7b")
        _write_file(os.path.join(skills_dir2, "p7b", "scripts", "undeclared.sh"), "#!/bin/sh\n")
        details2 = provider_file_hash_details(entry2, skills2, root2)
        assert any("present on disk but not listed in files[]" in d for d in details2), details2


def _st_p8():
    """A file marked locally_modified must name a patch id, and a local-origin file must carry a
    reason -- otherwise a change to a vendored artifact has no documented justification."""
    with tempfile.TemporaryDirectory() as tmp:
        entry, skills, root, _sd, _m = _build_provider_fixture(tmp)
        skills[0]["files"][0]["locally_modified"] = True
        skills[0]["files"][0]["patch_ids"] = []
        details = provider_file_hash_details(entry, skills, root)
        assert any("locally_modified=true but patch_ids is empty" in d for d in details), details

        skills[0]["files"][0]["locally_modified"] = False
        skills[0]["files"][0]["origin"] = "local"
        skills[0]["files"][0].pop("reason", None)
        details2 = provider_file_hash_details(entry, skills, root)
        assert any("origin=local but no 'reason' given" in d for d in details2), details2


def _st_p9():
    """Mode drift in both directions: a manifest claiming a non-100644 vendored_mode, an
    executable bit actually set on disk, and an undocumented 100755 -> 100644 normalization."""
    with tempfile.TemporaryDirectory() as tmp:
        entry, skills, root, skills_dir, _m = _build_provider_fixture(tmp)
        assert provider_vendored_mode_details(entry, skills, root) == []

        skills[0]["files"][0]["vendored_mode"] = "100755"
        details = provider_vendored_mode_details(entry, skills, root)
        assert any('vendored_mode' in d and '!= "100644"' in d for d in details), details
        skills[0]["files"][0]["vendored_mode"] = "100644"

        script = os.path.join(skills_dir, skills[0]["name"], "scripts", "run.sh")
        os.chmod(script, 0o755)
        details2 = provider_vendored_mode_details(entry, skills, root)
        assert any("executable bit set on disk" in d for d in details2), details2
        os.chmod(script, 0o644)

        skills[0]["files"][0]["upstream_mode"] = "100755"
        skills[0]["files"][0]["patch_ids"] = []
        details3 = provider_vendored_mode_details(entry, skills, root)
        assert any("with no patch id documenting it" in d for d in details3), details3
        # Documenting the normalization with a patch id clears it.
        skills[0]["files"][0]["patch_ids"] = ["fixture-mode-normalize"]
        assert provider_vendored_mode_details(entry, skills, root) == []


def _st_p10():
    """A skill shipping a script must record scripts_audited: true; the detector covers the
    extensions these providers actually ship AND extensionless shebang files."""
    with tempfile.TemporaryDirectory() as tmp:
        entry, skills, root, skills_dir, _m = _build_provider_fixture(tmp)
        skills[0]["scripts_audited"] = False
        details = provider_scripts_audited_details(entry, skills, root)
        assert any("scripts_audited is not true" in d for d in details), details

        # Extensionless shebang file: caught by content, not by name.
        entry2, skills2, root2, skills_dir2, _m2 = _build_provider_fixture(
            tmp, skill_name="p10b", with_script=False)
        hookpath = os.path.join(skills_dir2, "p10b", "scripts", "pr-snapshot")
        _write_file(hookpath, "#!/usr/bin/env bash\necho snapshot\n")
        skills2[0]["scripts_audited"] = False
        skills2[0]["files"].append({
            "path": os.path.relpath(hookpath, root2), "origin": "upstream",
            "upstream_blob_sha": "x", "vendored_blob_sha": "x",
            "upstream_mode": "100644", "vendored_mode": "100644",
            "locally_modified": False, "patch_ids": [],
        })
        details2 = provider_scripts_audited_details(entry2, skills2, root2)
        assert any("scripts_audited is not true" in d for d in details2), details2

        # ...and an extensionless file that is NOT a script (a LICENSE) must NOT be counted as one.
        # Without this half the shebang branch passes vacuously: `_has_shebang` returns a
        # (bool, error) TUPLE, and testing the tuple itself is truthy for every extensionless file,
        # which is exactly the bug this assertion exists to catch.
        entry3, skills3, root3, skills_dir3, _m3 = _build_provider_fixture(
            tmp, skill_name="p10c", with_script=False)
        licpath = os.path.join(skills_dir3, "p10c", "LICENSE")
        _write_file(licpath, "MIT License\n\nCopyright (c) 2026 Someone\n")
        skills3[0]["scripts_audited"] = False
        skills3[0]["files"].append({
            "path": os.path.relpath(licpath, root3), "origin": "local", "reason": "license notice",
            "upstream_blob_sha": "x", "vendored_blob_sha": "x",
            "upstream_mode": "100644", "vendored_mode": "100644",
            "locally_modified": False, "patch_ids": [],
        })
        assert provider_scripts_audited_details(entry3, skills3, root3) == [], \
            "a prose-only skill with an extensionless LICENSE must not be flagged as shipping a script"


def _st_p11():
    """Patch coverage is bidirectional: an in-file marker missing from the ledger fails, an
    in-file marker not recorded in files[].patch_ids fails, and a manifest patch id the ledger
    never documents fails."""
    with tempfile.TemporaryDirectory() as tmp:
        entry, skills, root, skills_dir, _m = _build_provider_fixture(tmp)
        skillmd = os.path.join(skills_dir, skills[0]["name"], "SKILL.md")
        with open(skillmd, "a", encoding="utf-8") as f:
            f.write("\n<!-- bukerov-local-patch: fx-1 -->tweak<!-- /bukerov-local-patch: fx-1 -->\n")

        details = provider_patch_coverage_details(entry, skills, "", root, "ledger.md")
        assert any("found in-file but missing from ledger.md" in d for d in details), details
        assert any("not recorded in any files[].patch_ids" in d for d in details), details

        skills[0]["files"][0]["locally_modified"] = True
        skills[0]["files"][0]["patch_ids"] = ["fx-1"]
        details2 = provider_patch_coverage_details(entry, skills, "", root, "ledger.md")
        assert any("manifest patch_id 'fx-1' not found in ledger.md" in d for d in details2), details2

        assert provider_patch_coverage_details(
            entry, skills, "row for `fx-1`", root, "ledger.md") == []


def _st_p12():
    """Dependency closure: a SKILL.md pointing at a closure file that was never vendored fails,
    while a path whose head directory does not exist in the skill stays prose and does not."""
    with tempfile.TemporaryDirectory() as tmp:
        _e, skills, _r, skills_dir, _m = _build_provider_fixture(tmp)
        assert dependency_closure_details(skills_dir) == []

        os.remove(os.path.join(skills_dir, skills[0]["name"], "scripts", "run.sh"))
        _write_file(os.path.join(skills_dir, skills[0]["name"], "scripts", "other.sh"), "#!/bin/sh\n")
        details = dependency_closure_details(skills_dir)
        assert any("references 'scripts/run.sh', which is not vendored" in d for d in details), details

        # No `references/` directory in this skill -> a prose mention is not a closure claim.
        skillmd = os.path.join(skills_dir, skills[0]["name"], "SKILL.md")
        _write_file(skillmd, "---\nname: x\ndescription: y\n---\n\nPut notes in `references/notes.md`.\n")
        assert dependency_closure_details(skills_dir) == []


def _st_p13():
    """Every rejected candidate must carry an auditable verdict: a missing/blank reason fails, and
    a status outside {EXCLUDE, HOLD} fails. HOLD is accepted so a blocked-but-valuable candidate is
    recorded rather than silently omitted."""
    def d(excluded, fs=()):
        return exclusion_entry_details("fx", "excluded_skills", excluded, set(fs))
    assert any("no reason" in x for x in d([{"name": "a", "reason": "   "}])), "blank reason must fail"
    assert any("no reason" in x for x in d([{"name": "a"}])), "missing reason must fail"
    assert any("status" in x for x in d([{"name": "a", "reason": "r", "status": "MAYBE"}])), "bad status must fail"
    assert any("excluded but present" in x
               for x in d([{"name": "a", "reason": "r"}], fs=["a"])), "installed-and-excluded must fail"
    assert d([{"name": "a", "reason": "r", "status": "HOLD"}]) == []
    assert d([{"name": "a", "reason": "r", "status": "EXCLUDE"}]) == []
    assert d([{"name": "a", "reason": "r"}]) == []
    assert d("not-a-list") == ["fx: excluded_skills must be a list"]
    assert any("malformed" in x for x in d([{"no-name": 1}]))


def _st_p14():
    """The frontmatter allowlist is widened PER PROVIDER from the registry, so a provider that
    legitimately uses `allowed-tools` keeps it -- upstream frontmatter is preserved, not deleted --
    while a key no provider declares is still rejected."""
    by_dir = allowed_frontmatter_keys_by_dir()
    # Every on-disk skill dir is claimed by exactly one source, so every one has an entry.
    for name in list_skill_dirs():
        assert name in by_dir, "no allowlist resolved for %s" % name
    for entry in MANIFESTS:
        allowed = ALLOWED_SKILL_KEYS | set(entry.get("extra_frontmatter_keys", ()))
        for name in provider_skill_dir_names(entry):
            assert by_dir[name] == allowed, (entry["label"], name, by_dir[name])
    # A registry entry declaring an extra key widens only ITS OWN dirs, never another provider's.
    fixture = _fake_provider_entry("/nonexistent")
    assert "allowed-tools" in (ALLOWED_SKILL_KEYS | set(fixture["extra_frontmatter_keys"]))
    assert "allowed-tools" not in ALLOWED_SKILL_KEYS
    assert "totally-made-up-key" not in ALLOWED_SKILL_KEYS


def _st_p15():
    """`{baseDir}/x` resolves against the SKILL ROOT, plain relatives against the containing
    directory, `#fragment` suffixes are stripped, a bare `{baseDir}` is not a path claim, and a
    `/`-rooted target is still reported absolute. Resolution must be real: a `{baseDir}` target
    that does not exist has to be reachable as a dangling link, or adapting the resolver would
    have turned a check into a rubber stamp."""
    with tempfile.TemporaryDirectory() as tmp:
        skill_dir = os.path.join(tmp, "tob-skill")
        nested = os.path.join(skill_dir, "references")
        _write_file(os.path.join(nested, "foundations.md"), "x\n")
        _write_file(os.path.join(nested, "sibling.md"), "y\n")

        kind, res = resolve_skill_link("{baseDir}/references/foundations.md", nested, skill_dir)
        assert kind == "path" and os.path.isfile(res), (kind, res)
        kind, res = resolve_skill_link("sibling.md", nested, skill_dir)
        assert kind == "path" and os.path.isfile(res), (kind, res)
        kind, res = resolve_skill_link("{baseDir}/references/foundations.md#anchor", nested, skill_dir)
        assert kind == "path" and os.path.isfile(res), (kind, res)
        # Not a rubber stamp: a missing {baseDir} target still resolves to a real, absent path.
        kind, res = resolve_skill_link("{baseDir}/references/never-vendored.md", nested, skill_dir)
        assert kind == "path" and not os.path.isfile(res), (kind, res)
        for skipped in ("{baseDir}", "https://example.com/x.md", "#heading", ""):
            assert resolve_skill_link(skipped, nested, skill_dir)[0] == "skip", skipped
        assert resolve_skill_link("/etc/passwd", nested, skill_dir)[0] == "absolute"
        # Plain relatives are NOT silently rebased onto the skill root.
        kind, res = resolve_skill_link("references/foundations.md", nested, skill_dir)
        assert kind == "path" and not os.path.isfile(res), (kind, res)


def _st_p16():
    """Fence pairing is CommonMark-correct: a fence closes only on the SAME character at LEAST as
    long as the opener. The nested four-backtick case is the one that matters -- it is how these
    skills document a fenced block inside a fenced block, and getting it wrong let everything after
    the mis-pair escape the stripper and be checked as live Markdown. Also asserts the stripper does
    not over-reach: text after a properly closed fence is still returned, inline spans survive
    strip_fenced_blocks (the closure check needs them), and both are gone from strip_code_fences."""
    nested = "\n".join([
        "before",
        "````markdown",
        "| [0-assessment.md](0-assessment.md) | generated |",
        "```language",
        "inner",
        "```",
        "````",
        "after [real](target.md)",
    ])
    stripped = strip_fenced_blocks(nested)
    assert "0-assessment.md" not in stripped, stripped
    assert "inner" not in stripped, stripped
    assert "before" in stripped and "after [real](target.md)" in stripped, stripped

    # A three-backtick fence must NOT be closed by a longer run belonging to an outer block.
    assert strip_fenced_blocks("```\na\n```\nkept") .strip().endswith("kept")
    # Tilde fences work on their own...
    assert "hidden" not in strip_fenced_blocks("~~~\nhidden\n~~~\nkept")
    assert "kept" in strip_fenced_blocks("~~~\nhidden\n~~~\nkept")
    # ...and a fence is NOT closed by a fence of the OTHER character. A `~~~` inside a ``` block is
    # block content, so everything up to the real ``` closer stays stripped and only `kept` returns.
    mixed = strip_fenced_blocks("```\nhidden\n~~~\nstill-hidden\n```\nkept")
    assert "still-hidden" not in mixed, mixed
    assert "hidden" not in mixed, mixed
    assert "kept" in mixed, mixed
    # An info string containing a backtick is not a fence opener (CommonMark). Without this rule a
    # prose line that merely starts with three backticks and then quotes something would open a
    # fence that never closes, swallowing the rest of the document.
    unopened = strip_fenced_blocks("```js `foo`\nkept\nalso-kept")
    assert "kept" in unopened and "also-kept" in unopened, unopened

    # A CLOSING fence carries nothing after it. A same-length run followed by text is block
    # content, so the block continues to the real closer -- otherwise a code sample containing
    # ``` mid-line would end the block early and leak the rest of it into the checked text.
    trailing = strip_fenced_blocks("```\nhidden\n``` trailing\nstill-hidden\n```\nkept")
    assert "still-hidden" not in trailing, trailing
    assert "kept" in trailing, trailing

    # Division of labour between the two strippers.
    doc = "see `scripts/run.sh` here"
    assert "scripts/run.sh" in strip_fenced_blocks(doc), "closure check must still see inline paths"
    assert "scripts/run.sh" not in strip_code_fences(doc), "link check must not see inline code"


def _st_p17():
    """`invocation` must agree with the skill's own frontmatter in BOTH directions, and a manifest
    that simply omits the field is not fabricated into a finding."""
    with tempfile.TemporaryDirectory() as tmp:
        entry, skills, _root, skills_dir, _m = _build_provider_fixture(tmp, skill_name="p17-skill")
        name, skillmd = skills[0]["name"], os.path.join(skills_dir, "p17-skill", "SKILL.md")

        # Fixture skill is model-invoked and the manifest says so.
        assert invocation_consistency_details(entry, skills, skills_dir) == []

        # Manifest claims user-invoked, frontmatter does not disable model invocation.
        skills[0]["invocation"] = "user"
        d = invocation_consistency_details(entry, skills, skills_dir)
        assert any("manifest invocation 'user'" in x and "says model" in x for x in d), d

        # Frontmatter disables model invocation but the manifest still says model.
        _write_file(skillmd, "---\nname: %s\ndescription: d\ndisable-model-invocation: true\n---\n" % name)
        skills[0]["invocation"] = "model"
        d = invocation_consistency_details(entry, skills, skills_dir)
        assert any("manifest invocation 'model'" in x and "says user" in x for x in d), d

        # Correcting the manifest clears it.
        skills[0]["invocation"] = "user"
        assert invocation_consistency_details(entry, skills, skills_dir) == []

        # A manifest schema that does not record invocation is not a finding.
        skills[0].pop("invocation")
        assert invocation_consistency_details(entry, skills, skills_dir) == []


def _st_p18():
    """Every registry entry must be file-level; a retired-schema entry is rejected.

    The real registry is asserted clean too, so this fixture fails the moment a provider is
    added on a schema that would silently receive no hash coverage."""
    assert all_providers_file_level_details(MANIFESTS) == [], (
        "the real registry has a non-file-level provider: %r"
        % all_providers_file_level_details(MANIFESTS))
    stale = [{"label": "legacy", "schema": "skill-level"},
             {"label": "ok", "schema": "file-level"},
             {"label": "missing"}]
    details = all_providers_file_level_details(stale)
    assert len(details) == 2, details
    assert any("legacy" in d for d in details), details
    assert any("missing" in d for d in details), details


def _st_p19():
    """An automated update candidate must fail the governance gate.

    This is the anti-masquerade guarantee: the bot can prepare a mechanically clean refresh,
    but the manifest it writes carries `automated_candidate`, and that block failing here is
    what makes "audited" impossible to fake. Cleared only by a deliberate human edit."""
    with tempfile.TemporaryDirectory() as tmp:
        docs = os.path.join(tmp, "docs", "agents")
        os.makedirs(docs)
        path = os.path.join(docs, "fixture-skills-manifest.json")
        entry = dict(_fake_provider_entry(docs))
        base = {"upstream_repo": "https://github.com/fixture/skills",
                "upstream_commit": "a" * 40, "reviewed_at": "2026-01-01T00:00:00Z",
                "reviewed_by": "human", "installation_mode": REQUIRED_INSTALLATION_MODE,
                "automatic_updates": False, "skills": []}

        with open(path, "w", encoding="utf-8") as f:
            json.dump(base, f)
        assert unaudited_candidate_details([entry]) == [], "a reviewed manifest must pass"

        candidate = dict(base)
        candidate[CANDIDATE_KEY] = {"state": CANDIDATE_STATE_PREPARED,
                                    "superseded_commit": "a" * 40,
                                    "target_commit": "b" * 40}
        with open(path, "w", encoding="utf-8") as f:
            json.dump(candidate, f)
        details = unaudited_candidate_details([entry])
        assert len(details) == 1, details
        assert CANDIDATE_STATE_PREPARED in details[0], details
        assert "reviewed_at/reviewed_by" in details[0], details

        # A candidate cannot hide by renaming its own state: the KEY is what fails, not the
        # value, so "state": "AUDITED" is still caught -- and is called out specifically, because
        # a manifest that WRITES the word is taking the one route that cannot grant a review.
        candidate[CANDIDATE_KEY]["state"] = CANDIDATE_STATE_AUDITED
        with open(path, "w", encoding="utf-8") as f:
            json.dump(candidate, f)
        audited = unaudited_candidate_details([entry])
        assert len(audited) == 1, "renaming the state must not help: %r" % (audited,)
        assert "AUDITED is never written into this block" in audited[0], audited
        assert "DELETING the block" in audited[0], audited

        # An EVAL_REQUIRED candidate says so in the diagnostic, so a reader clearing the block
        # learns that provenance alone did not establish behavioural equivalence.
        candidate[CANDIDATE_KEY]["state"] = CANDIDATE_STATE_PREPARED
        candidate[CANDIDATE_KEY]["eval_required"] = ["SKILL.md changed"]
        with open(path, "w", encoding="utf-8") as f:
            json.dump(candidate, f)
        evald = unaudited_candidate_details([entry])
        assert len(evald) == 1 and "EVAL_REQUIRED" in evald[0], evald


def _st_p21():
    """The native-plugin inventory ships empty, parses, and documents all three surfaces.

    Surface B (marketplace plugins) is monitored, never mutated, and nothing is installed by the
    task that added the schema. If a plugin ever IS adopted, that is a reviewed data change and
    this fixture is where the "empty" assumption stops holding -- deliberately, so the change
    cannot pass unnoticed."""
    path = os.path.join(DOCS_AGENTS_DIR, "skills-update-plugins.json")
    assert os.path.isfile(path), "plugin inventory missing: %s" % path
    with open(path, encoding="utf-8") as f:
        doc = _strict_json_load(f)
    assert doc.get("schema_version") == 1, doc.get("schema_version")
    assert doc.get("plugins") == [], (
        "the plugin inventory is expected to ship EMPTY; adopting a plugin is a reviewed change "
        "that must update this fixture too, got: %r" % (doc.get("plugins"),))
    surfaces = doc.get("surfaces") or {}
    assert sorted(surfaces) == ["A_project_skills", "B_native_plugins",
                                "C_claudeai_zip_skills"], sorted(surfaces)
    assert doc["version_precedence"][0] == "plugin.json version", doc["version_precedence"]
    assert doc["version_precedence"][-1] == "unknown", doc["version_precedence"]


def _st_p22():
    """The provider registry owns the reviewed REF; manifests own the pin. No floating pins.

    A registry entry whose `upstream_ref` were a SHA would freeze drift detection permanently,
    and a manifest whose `upstream_commit` were a branch name would be the floating dependency
    vendoring exists to avoid. Both directions are asserted against the real files."""
    with open(os.path.join(DOCS_AGENTS_DIR, "skills-update-providers.json"),
              encoding="utf-8") as f:
        registry = _strict_json_load(f)
    keys = set()
    for entry in registry["providers"]:
        assert not SHA1_RE.match(entry["upstream_ref"]), (
            "%s: upstream_ref must be a branch name, not a pin" % entry["key"])
        assert entry["key"] not in keys, "duplicate provider key %s" % entry["key"]
        keys.add(entry["key"])
        if entry.get("monitor_only"):
            assert SHA1_RE.match(entry["baseline_commit"]), entry["key"]
            continue
        with open(os.path.join(REPO_ROOT, entry["manifest"]), encoding="utf-8") as mf:
            manifest = _strict_json_load(mf)
        assert SHA1_RE.match(manifest["upstream_commit"]), (
            "%s: manifest upstream_commit must be a full sha" % entry["key"])
        assert _canon_repo(manifest["upstream_repo"]) == _canon_repo(entry["upstream_repo"]), (
            "%s: registry and manifest disagree about the upstream repo" % entry["key"])
    # Every vendored provider in the MANIFESTS registry must be represented.
    assert {e["label"] for e in MANIFESTS} <= keys, (
        "a validated provider is missing from the update registry: %r"
        % ({e["label"] for e in MANIFESTS} - keys))


def _st_p20():
    """Only the two exact G1.1 workflow paths bypass the application-path guard."""
    assert GOVERNANCE_OWNED_WORKFLOWS == (
        STABLE_CI_WORKFLOW_REL, STABLE_SKILLS_WORKFLOW_REL)
    assert application_path_violations([
        STABLE_SKILLS_WORKFLOW_REL, STABLE_CI_WORKFLOW_REL]) == []
    caught = application_path_violations([
        STABLE_RELEASE_WORKFLOW_REL, ".github/workflows/release.yml",
        ".github/workflows/skills-update.yml", ".github/workflows/anything-else.yml",
        "internal/web/server.go", "cmd/miner/main.go",
        "go.mod", "go.sum", "Dockerfile", "docker-compose.yml"])
    assert len(caught) == 10, caught
    # Governance-layer paths this validator owns are not application paths and never were.
    assert application_path_violations([
        "scripts/validate-agent-governance.py", "GOVERNANCE_V3.md",
        "docs/agents/skills-update-providers.json", "CLAUDE.md"]) == []
    assert governance_base_sha_details(None) == []
    assert governance_base_sha_details(None, required=True)
    assert governance_base_sha_details("a" * 40) == []
    for invalid in ("0" * 40, "a" * 39, "A" * 40, "release/0.3"):
        assert governance_base_sha_details(invalid), invalid


def _st_p23():
    """Generic and dedicated G1 invocations select disjoint, fail-closed boundaries."""
    generic = checks_for_application_scope(APPLICATION_SCOPE_GENERIC)
    dedicated = checks_for_application_scope(APPLICATION_SCOPE_G1_STABLE_SKILLS)
    assert dedicated == ALL_CHECKS
    assert len(generic) == len(ALL_CHECKS) - 1
    assert check_application_paths_untouched not in generic
    assert [check for check in ALL_CHECKS if check is not check_application_paths_untouched] == generic
    try:
        checks_for_application_scope("unknown")
    except ValueError:
        pass
    else:
        raise AssertionError("unknown programmatic scope did not fail closed")

    assert application_scope_invocation_details(APPLICATION_SCOPE_GENERIC, {}) == []
    assert application_scope_invocation_details(
        APPLICATION_SCOPE_GENERIC, {"GOVERNANCE_BASE_SHA": "a" * 40})
    assert application_scope_invocation_details(APPLICATION_SCOPE_G1_STABLE_SKILLS, {})

    head = subprocess.check_output(
        ["git", "-C", REPO_ROOT, "rev-parse", "HEAD"], text=True).strip()
    tree = subprocess.check_output(
        ["git", "-C", REPO_ROOT, "rev-parse", "HEAD^{tree}"], text=True).strip()
    blob = subprocess.check_output(
        ["git", "-C", REPO_ROOT, "rev-parse", "HEAD:scripts/validate-agent-governance.py"],
        text=True).strip()
    assert not governance_base_commit_details(head, required=True)
    tree_details = governance_base_commit_details(tree, required=True)
    assert any("commit object" in detail for detail in tree_details), tree_details
    blob_details = governance_base_commit_details(blob, required=True)
    assert any("commit object" in detail for detail in blob_details), blob_details
    assert governance_base_commit_details("a" * 40, required=True)

    assert parse_cli_args([]).application_scope == APPLICATION_SCOPE_GENERIC
    assert parse_cli_args([
        "--application-scope", APPLICATION_SCOPE_G1_STABLE_SKILLS,
        "--self-test-hook",
    ]).application_scope == APPLICATION_SCOPE_G1_STABLE_SKILLS
    malformed = (
        ["--unknown-governance-mode"],
        ["--application-sc", APPLICATION_SCOPE_GENERIC],
        ["--application-scope"],
        ["--application-scope", "unknown"],
        ["--application-scope", APPLICATION_SCOPE_G1_STABLE_SKILLS,
         "--application-scope", APPLICATION_SCOPE_GENERIC],
        ["--self-test", "--application-scope", APPLICATION_SCOPE_GENERIC],
        ["--self-test", "--self-test-hook"],
    )
    for argv in malformed:
        with contextlib.redirect_stderr(io.StringIO()):
            try:
                parse_cli_args(argv)
            except SystemExit as exc:
                assert exc.code != 0, argv
            else:
                raise AssertionError("malformed invocation passed: %r" % (argv,))


def _st_p24():
    """G1 path collection catches every Git layer, renames and unusual filenames."""
    git = shutil.which("git")
    assert git, "git not found on PATH; required for the P24 fixture"
    with tempfile.TemporaryDirectory() as tmp:
        os.makedirs(os.path.join(tmp, "internal"))
        os.makedirs(os.path.join(tmp, "docs", "agents"))
        source = os.path.join(tmp, "internal", "runtime.go")
        destination = os.path.join(tmp, "docs", "agents", "runtime.go")
        _write_file(source, "package internal\n")
        _write_file(os.path.join(tmp, "internal", "index-only.go"), "package internal\n")
        _write_file(os.path.join(tmp, "internal", "head-only.go"), "package internal\n")

        def _git(*args):
            out = subprocess.run([git] + list(args), cwd=tmp, stdout=subprocess.PIPE,
                                 stderr=subprocess.PIPE, timeout=5, text=True)
            assert out.returncode == 0, "git %s failed: %s" % (" ".join(args), out.stderr)
            return out.stdout.strip()

        author_flags = ("-c", "user.name=governance-self-test",
                        "-c", "user.email=governance-self-test@example.invalid")
        _git("init", "-q")
        _git(*(author_flags + ("add", "-A")))
        _git(*(author_flags + ("commit", "-q", "-m", "p24: base")))
        base = _git("rev-parse", "HEAD")

        # Commit an application change to HEAD, then hide it from both index and worktree by
        # restoring those layers from base. Only the explicit base-to-HEAD comparison sees it.
        _write_file(os.path.join(tmp, "internal", "head-only.go"),
                    "package internal\n\n// changed in HEAD\n")
        _git(*(author_flags + ("add", "-A")))
        _git(*(author_flags + ("commit", "-q", "-m", "p24: head-only")))
        _git("restore", "--source", base, "--staged", "--worktree", "--",
             "internal/head-only.go")

        os.replace(source, destination)
        _git("add", "-A")

        # Stage an application change, then restore only its worktree bytes from base. Only the
        # explicit base-to-index comparison is guaranteed to see the staged candidate. This must
        # happen after staging the rename so a later broad add cannot overwrite the index layer.
        _write_file(os.path.join(tmp, "internal", "index-only.go"),
                    "package internal\n\n// changed in index\n")
        _git("add", "--", "internal/index-only.go")
        _git("restore", "--source", base, "--worktree", "--", "internal/index-only.go")

        unusual = "internal/odd\nname.go"
        _write_file(os.path.join(tmp, unusual), "package internal\n")

        paths = application_scope_changed_paths(base, tmp)
        assert "internal/runtime.go" in paths, paths
        assert "docs/agents/runtime.go" in paths, paths
        assert "internal/head-only.go" in paths, paths
        assert "internal/index-only.go" in paths, paths
        assert unusual in paths, paths
        assert len(paths) == len(set(paths)), paths
        violations = application_path_violations(paths)
        assert any("internal/runtime.go" in detail for detail in violations), violations
        assert any("internal/odd\\nname.go" in detail for detail in violations), violations
        assert not any("\n" in detail for detail in violations), violations


def _st_p25():
    """Production CLI routes generic runtime and dedicated G1 diffs end to end."""
    git = shutil.which("git")
    assert git, "git not found on PATH; required for the P25 fixture"
    with tempfile.TemporaryDirectory() as tmp:
        repo = os.path.join(tmp, "repo")
        os.makedirs(repo)

        tracked = subprocess.check_output(
            [git, "-C", REPO_ROOT, "ls-files", "-z"], timeout=10)
        for raw_path in tracked.split(b"\0"):
            if not raw_path:
                continue
            path = os.fsdecode(raw_path)
            source = os.path.join(REPO_ROOT, path)
            target = os.path.join(repo, path)
            os.makedirs(os.path.dirname(target), exist_ok=True)
            if os.path.islink(source):
                os.symlink(os.readlink(source), target)
            else:
                shutil.copy2(source, target)

        def _git(*args):
            out = subprocess.run([git] + list(args), cwd=repo, stdout=subprocess.PIPE,
                                 stderr=subprocess.PIPE, timeout=10, text=True)
            assert out.returncode == 0, "git %s failed: %s" % (" ".join(args), out.stderr)
            return out.stdout.strip()

        author_flags = ("-c", "user.name=governance-self-test",
                        "-c", "user.email=governance-self-test@example.invalid")
        _git("init", "-q")
        _git(*(author_flags + ("add", "-A")))
        _git(*(author_flags + ("commit", "-q", "-m", "p25: base")))
        base = _git("rev-parse", "HEAD")

        validator = os.path.join(repo, "scripts", "validate-agent-governance.py")
        clean_env = os.environ.copy()
        clean_env.pop("GOVERNANCE_BASE_SHA", None)
        # The fixture's commits belong to its disposable repository, never to an outer Actions
        # checkout. Hosted event identity would make the nested runtime verifier compare this
        # temporary HEAD with the outer PR merge SHA, so remove the entire GitHub event namespace.
        for name in tuple(clean_env):
            if name.startswith("GITHUB_"):
                clean_env.pop(name)
        clean_env["PYTHONDONTWRITEBYTECODE"] = "1"

        def _validate(scope, base_sha=None):
            env = clean_env.copy()
            if base_sha is not None:
                env["GOVERNANCE_BASE_SHA"] = base_sha
            return subprocess.run(
                [sys.executable, "-I", "-S", "-B", validator,
                 "--application-scope", scope, "--self-test-hook"],
                cwd=repo, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                timeout=30, text=True)

        # A governance-only candidate is a valid dedicated G1 comparison.
        _write_file(os.path.join(repo, "docs", "agents", "g1-scope-probe.md"),
                    "# G1 scope fixture\n")
        _git(*(author_flags + ("add", "-A")))
        _git(*(author_flags + ("commit", "-q", "-m", "p25: governance-only")))
        dedicated_clean = _validate(APPLICATION_SCOPE_G1_STABLE_SKILLS, base)
        assert dedicated_clean.returncode == 0, dedicated_clean.stdout + dedicated_clean.stderr
        assert "44/44 checks passed" in dedicated_clean.stdout, dedicated_clean.stdout

        # A committed base-to-head application change must pass generic validation and fail only
        # the dedicated guard. Committing the probe catches collectors that inspect only the index.
        _write_file(os.path.join(repo, "internal", "g1-scope-probe.go"),
                    "package internal\n")
        _git(*(author_flags + ("add", "-A")))
        _git(*(author_flags + ("commit", "-q", "-m", "p25: application change")))

        generic = _validate(APPLICATION_SCOPE_GENERIC)
        assert generic.returncode == 0, generic.stdout + generic.stderr
        assert "43/43 checks passed" in generic.stdout, generic.stdout

        dedicated_mixed = _validate(APPLICATION_SCOPE_G1_STABLE_SKILLS, base)
        assert dedicated_mixed.returncode == 1, dedicated_mixed.stdout + dedicated_mixed.stderr
        fail_lines = [line for line in dedicated_mixed.stdout.splitlines()
                      if line.startswith("[FAIL]")]
        assert fail_lines == ["[FAIL] application-paths-untouched"], fail_lines
        assert "43/44 checks passed" in dedicated_mixed.stdout, dedicated_mixed.stdout


def _st_w1():
    """The real stable maintenance workflow satisfies the closed G1.1 authority envelope."""
    path = os.path.join(REPO_ROOT, STABLE_SKILLS_WORKFLOW_REL)
    with io.open(path, encoding="utf-8") as handle:
        details = stable_skills_workflow_details(handle.read())
    assert not details, details


def _st_w2():
    """Stable-workflow mutations cannot widen authority or bypass identity and liveness."""
    path = os.path.join(REPO_ROOT, STABLE_SKILLS_WORKFLOW_REL)
    with io.open(path, encoding="utf-8") as handle:
        real = handle.read()
    mutations = (
        ("default branch", "github.ref == 'refs/heads/release/0.3'",
         "github.ref == 'refs/heads/main'"),
        ("write permission", "permissions:\n      contents: read",
         "permissions:\n      contents: write"),
        ("floating checkout", PINNED_CHECKOUT_ACTION, "actions/checkout@v4"),
        ("persisted credential", "persist-credentials: false", "persist-credentials: true"),
        ("missing runtime guard",
         "python3 -I -S -B scripts/skill_updates/runtime.py verify-workflow --repo-root .",
         "python3 -c 'raise SystemExit(1)'"),
        ("missing terminal reconciliation", "  reconcile-all:\n",
         "  detector-only-success:\n"),
        ("publication flag", '--summary "$GITHUB_STEP_SUMMARY"',
         '--publish \\\n            --summary "$GITHUB_STEP_SUMMARY"'),
        ("untrusted runner", "runs-on: ubuntu-24.04", "runs-on: ubuntu-latest"),
        ("extra executable action", "      - name: Detect and classify upstream drift\n",
         "      - uses: example/unreviewed-action@v1\n\n"
         "      - name: Detect and classify upstream drift\n"),
        ("partial provider dispatch", "  workflow_dispatch:\n",
         "  workflow_dispatch:\n    inputs:\n      provider:\n        type: string\n"),
        ("reserved provider env", "      PYTHONDONTWRITEBYTECODE: \"1\"",
         "      ImageVersion: attacker-controlled"),
        ("flow-style reserved provider env", "    env:\n      GIT_ALLOW_PROTOCOL: https",
         "    env: {ImageVersion: attacker-controlled}"),
        ("indexed secret expression", "      PYTHONDONTWRITEBYTECODE: \"1\"",
         "      TOKEN: ${{ secrets['PROD_TOKEN'] }}"),
        ("indexed github token expression", "      PYTHONDONTWRITEBYTECODE: \"1\"",
         "      TOKEN: ${{ github['token'] }}"),
        ("inline Python step", "      - name: Detect and classify upstream drift\n",
         "      - run: python3 -I -S -B -c 'raise SystemExit(0)'\n\n"
         "      - name: Detect and classify upstream drift\n"),
        ("ambient Git trace", '"GIT_CONFIG_PARAMETERS",',
         '"GIT_CONFIG_PARAMETERS", "GIT_TRACE",'),
    )
    for label, old, new in mutations:
        mutated = real.replace(old, new, 1)
        assert mutated != real, "mutation anchor missing: %s" % label
        details = stable_skills_workflow_details(mutated)
        assert details, "%s mutation passed the production helper" % label

    verify = ("python3 -I -S -B scripts/skill_updates/runtime.py "
              "verify-workflow --repo-root .")
    detector = "python3 -I -S -B scripts/check-skill-updates.py"
    reordered = real.replace(verify, "", 1).replace(detector, detector + "\n        " + verify, 1)
    assert reordered != real
    details = stable_skills_workflow_details(reordered)
    assert any("before the detector" in detail for detail in details), details


def _st_w3():
    """The real CI insertion is bounded and stable product CI remains byte-identical."""
    path = os.path.join(REPO_ROOT, STABLE_CI_WORKFLOW_REL)
    with io.open(path, encoding="utf-8") as handle:
        details = ci_workflow_details(handle.read())
    assert not details, details


def _st_w4():
    """CI trigger, checkout, runner and updater-command mutations all fail closed."""
    path = os.path.join(REPO_ROOT, STABLE_CI_WORKFLOW_REL)
    with io.open(path, encoding="utf-8") as handle:
        real = handle.read()
    mutations = (
        ("product trigger", 'branches: ["release/**"]', 'branches: ["main"]'),
        ("floating checkout", PINNED_CHECKOUT_ACTION, "actions/checkout@v4"),
        ("shallow comparison checkout", "fetch-depth: 0", "fetch-depth: 1"),
        ("generic scope downgraded", "--application-scope generic", "--application-scope g1-stable-skills"),
        ("ambient G1 comparison SHA", "      BASH_ENV: /dev/null",
         "      BASH_ENV: /dev/null\n      GOVERNANCE_BASE_SHA: " + "a" * 40),
        ("persisted credential", "persist-credentials: false", "persist-credentials: true"),
        ("missing validator self-test",
         "python3 -I -S -B scripts/validate-agent-governance.py --self-test\n", "true\n"),
        ("missing static verifier",
         "python3 -I -S -B scripts/skill_updates/runtime.py verify-repository --repo-root .",
         "python3 -c 'raise SystemExit(1)'"),
        ("untrusted runner", "runs-on: ubuntu-24.04", "runs-on: ubuntu-latest"),
        ("write permission", "  governance:\n", "  governance:\n    permissions:\n      contents: write\n"),
        ("bootstrap semantic drift", "if present_auth:", "if False and present_auth:") ,
        ("reserved provider env", "      PYTHONDONTWRITEBYTECODE: \"1\"",
         "      RUNNER_ARCH: attacker-controlled"),
        ("flow-style reserved provider env", "    env:\n      BASH_ENV: /dev/null",
         "    env: {ImageVersion: attacker-controlled}"),
        ("indexed secret expression", "      PYTHONDONTWRITEBYTECODE: \"1\"",
         "      TOKEN: ${{ secrets['PROD_TOKEN'] }}"),
        ("indexed github token expression", "      PYTHONDONTWRITEBYTECODE: \"1\"",
         "      TOKEN: ${{ github['token'] }}"),
        ("inline Python step", "      - name: Run validator fixture self-tests\n",
         "      - run: python3 -I -S -B -c 'raise SystemExit(0)'\n\n"
         "      - name: Run validator fixture self-tests\n"),
    )
    for label, old, new in mutations:
        mutated = real.replace(old, new, 1)
        assert mutated != real, "mutation anchor missing: %s" % label
        details = ci_workflow_details(mutated)
        assert details, "%s mutation passed the production helper" % label


def _st_w5():
    """The file-backed workflow registry admits exactly CI, release and G1.1 maintenance."""
    with tempfile.TemporaryDirectory() as tmp:
        workflows = os.path.join(tmp, ".github", "workflows")
        os.makedirs(workflows)
        for name in ("ci.yml", "stable-release.yml", "stable-skills-maintenance.yml"):
            shutil.copyfile(os.path.join(REPO_ROOT, ".github", "workflows", name),
                            os.path.join(workflows, name))
        details = workflow_registry_details(tmp)
        assert not details, details

        _write_file(os.path.join(workflows, "skills-update.yml"), "name: unreviewed\n")
        details = workflow_registry_details(tmp)
        assert any("registry differs" in detail for detail in details), details
        os.unlink(os.path.join(workflows, "skills-update.yml"))

        with io.open(os.path.join(workflows, "stable-release.yml"), "a", encoding="utf-8") as out:
            out.write("# drift\n")
        details = workflow_registry_details(tmp)
        assert any("stable-authoritative bytes" in detail for detail in details), details


def _st_w6():
    """The validator delegates exact control semantics to the static, network-free runtime API."""
    calls = []

    class Result:
        returncode = 0
        stdout = ""

    def ok_runner(command, **kwargs):
        calls.append((command, kwargs))
        return Result()

    with tempfile.TemporaryDirectory() as tmp:
        details = stable_skills_control_details(tmp, runner=ok_runner)
        assert not details, details
        assert len(calls) == 1, calls
        command, kwargs = calls[0]
        assert command == [sys.executable, "-m", "skill_updates.runtime", "verify-repository",
                           "--repo-root", tmp], command
        assert kwargs["cwd"] == tmp
        assert kwargs["timeout"] == 30 and kwargs["text"] is True
        assert kwargs["stderr"] is subprocess.STDOUT
        assert kwargs["env"]["PYTHONPATH"].split(os.pathsep)[0] == os.path.join(tmp, "scripts")

        class Failed:
            returncode = 23
            stdout = "control mutation rejected"

        details = stable_skills_control_details(tmp, runner=lambda *args, **kwargs: Failed())
        assert details and "control mutation rejected" in details[0], details


def _st_w7():
    """JSON and env source grammars reject ambiguous alternate spellings."""
    assert _strict_json_loads('{"outer":{"value":1}}') == {"outer": {"value": 1}}
    for raw in (
            '{"state":"BLOCKED","state":"NO_DRIFT"}',
            '{"outer":{"state":"BLOCKED","state":"NO_DRIFT"}}',
            '{"value":NaN}', '{"value":Infinity}', '{"value":-Infinity}'):
        try:
            _strict_json_loads(raw)
        except ValueError:
            pass
        else:
            raise AssertionError("duplicate JSON key passed: %s" % raw)
    for raw in (
            "? env\n:\n  ImageVersion: attacker-controlled\n",
            "!!str env:\n  ImageVersion: attacker-controlled\n",
            "? !!str env\n:\n  ImageVersion: attacker-controlled\n",
            "? !<tag:yaml.org,2002:str> env\n:\n  ImageVersion: attacker-controlled\n",
            '"\\u0065nv":\n  ImageVersion: attacker-controlled\n',
            "&envkey env:\n  ImageVersion: attacker-controlled\n",
            "*envkey:\n  ImageVersion: attacker-controlled\n"):
        details = _reserved_provider_env_details(raw, "fixture")
        assert any("unsupported explicit" in detail for detail in details), details


def _real_inventory_inputs():
    """The real repo's inventory inputs, shared by the I*/R* fixtures and the checks."""
    installed, errs = _installed_names_by_provider()
    assert not errs, errs
    project_names = _project_manifest_names()
    repo_short = {e["label"]: e["upstream_repo"].split("github.com/", 1)[1] for e in MANIFESTS}
    return installed, repo_short, set(list_skill_dirs()), project_names


def _st_i1():
    """The real GOVERNANCE_V3.md section 7 reconciles with the manifests, the disk, and itself."""
    installed, repo_short, disk, proj = _real_inventory_inputs()
    details = governance_inventory_details(
        _real_governance_v3_text(), installed, repo_short, disk, proj)
    assert not details, details


def _st_i2():
    """NEGATIVE: a missing installed skill (80-of-81) and a section-7 name swap are both caught."""
    text = _real_governance_v3_text()
    installed, repo_short, disk, proj = _real_inventory_inputs()
    mut = {k: list(v) for k, v in installed.items()}
    removed = mut["builderio"].pop()
    details = governance_inventory_details(text, mut, repo_short, disk, proj)
    assert any(removed in d for d in details), (removed, details)
    swapped = text.replace("`%s`" % removed, "`not-actually-installed`", 1)
    assert swapped != text
    details = governance_inventory_details(swapped, installed, repo_short, disk, proj)
    assert any(removed in d for d in details), (removed, details)
    assert any("not-actually-installed" in d for d in details), details


def _st_i3():
    """NEGATIVE: an unapproved 82nd directory, a stale total, and table drift are each caught."""
    text = _real_governance_v3_text()
    installed, repo_short, disk, proj = _real_inventory_inputs()
    details = governance_inventory_details(
        text, installed, repo_short, disk | {"rogue-skill"}, proj)
    assert any("rogue-skill" in d for d in details), details
    total = sum(len(v) for v in installed.values()) + len(proj)
    stale = text.replace("= **%d**" % total, "= **%d**" % (total - 1), 1)
    assert stale != text
    details = governance_inventory_details(stale, installed, repo_short, disk, proj)
    assert any(str(total - 1) in d for d in details), details
    drift = text.replace("| mattpocock/skills | 23 |", "| mattpocock/skills | 22 |", 1)
    assert drift != text
    details = governance_inventory_details(drift, installed, repo_short, disk, proj)
    assert any("mattpocock" in d and "22" in d for d in details), details


def _st_i5():
    """NEGATIVE: section 7's explicit-invocation list is machine-tied to the manifests."""
    text = _real_governance_v3_text()
    installed, repo_short, disk, proj = _real_inventory_inputs()
    user = _user_invoked_names()
    assert user, "expected a non-empty explicit-invocation set"
    assert not governance_inventory_details(text, installed, repo_short, disk, proj,
                                            user_invoked=user)
    victim = "wizard"
    assert victim in user
    details = governance_inventory_details(text, installed, repo_short, disk, proj,
                                           user_invoked=user - {victim})
    assert any(victim in d and "explicit-invocation-only" in d for d in details), details
    details = governance_inventory_details(text, installed, repo_short, disk, proj,
                                           user_invoked=user | {"tdd"})
    assert any("'tdd'" in d and "omits it" in d for d in details), details


def _st_i4():
    """NEGATIVE: an unparseable section 7 fails loudly instead of passing silently."""
    installed, repo_short, disk, proj = _real_inventory_inputs()
    details = governance_inventory_details(
        "# not governance at all\n", installed, repo_short, disk, proj)
    assert details, "an unparseable document must fail"


def _st_r1():
    """The real skills-routing.md routes every installed skill and states the machine-true total."""
    installed, _repo_short, _disk, proj = _real_inventory_inputs()
    names = set().union(*[set(v) for v in installed.values()]) | set(proj)
    text = io.open(os.path.join(DOCS_AGENTS_DIR, "skills-routing.md"), encoding="utf-8").read()
    details = skills_routing_details(text, names, len(MANIFESTS))
    assert not details, details


def _st_r2():
    """NEGATIVE: an unrouted installed skill and a stale routing total are both caught."""
    installed, _repo_short, _disk, proj = _real_inventory_inputs()
    names = set().union(*[set(v) for v in installed.values()]) | set(proj)
    text = io.open(os.path.join(DOCS_AGENTS_DIR, "skills-routing.md"), encoding="utf-8").read()
    victim = "vulnerability-triage-brocards"
    assert victim in names
    dropped = text.replace("`%s`" % victim, "`[removed]`")
    assert dropped != text
    details = skills_routing_details(dropped, names, len(MANIFESTS))
    assert any(victim in d for d in details), details
    stale = text.replace("%d installed skills" % len(names), "%d installed skills" % (len(names) - 1))
    assert stale != text
    details = skills_routing_details(stale, names, len(MANIFESTS))
    assert any("installed skills across" in d for d in details), details



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
        ("V1", "governance_surface_paths selects exactly the owned governance text", _st_v1),
        ("V2", "scan_governance_surface reports file:line hits and honours exemptions", _st_v2),
        ("V3", "repo governance surface has no stale v2 orchestration mandates", _st_v3),
        ("V4", "no tracked file names a host operating system", _st_v4),
        ("V5", "Governance v3 docs exist and CLAUDE.md declares v3", _st_v5),
        ("V6", "the real task-contract.md is a v3 authority envelope", _st_v6),
        ("V7", "mandatory agent_cap/max_concurrency rejected (old vacuous check passed it)", _st_v7),
        ("V8", "universal reviewer-read-only mandate detected; skill-chosen reviewers are not", _st_v8),
        ("V9", "mandatory resource caps detected through Markdown markup; optional ones are not", _st_v9),
        ("V10", "surface scan catches a hard-wrapped phrase exactly once", _st_v10),
        ("V11", "host-OS token rejected with no document exempt", _st_v11),
        ("V12", "schema block selected by task_contract: key, not by fence position", _st_v12),
        ("V13", "the authority-chain documents agree on four levels", _st_v13),
        ("V14", "a resurrected five-level authority chain is rejected", _st_v14),
        ("V15", "the real GOVERNANCE_V3.md carries the complete section-5 recovery contract", _st_v15),
        ("V16", "every recovery needle and checkpoint record field detected when dropped", _st_v16),
        ("V17", "checkpoint contract id pinned; empty document reports rather than raises", _st_v17),
        ("V18", "authority-restoring wording added to section 5 is rejected", _st_v18),
        ("V19", "host-OS budget filter: within-budget tolerated, growth and unlisted paths fail", _st_v19),
        ("V30", "CLAUDE.md continuity pointer: canonical link, invariants, contract id", _st_v30),
        ("V31", "the pinned constants themselves; no degenerate needle", _st_v31),
        ("P1", "a correctly vendored provider passes every generic provider check", _st_p1),
        ("P2", "automatic_updates not literally false", _st_p2),
        ("P3", "upstream_commit is a floating ref / not a 40-hex SHA", _st_p3),
        ("P4", "manifest upstream_repo mismatch and missing provenance field", _st_p4),
        ("P5", "missing per-skill and shared license notices", _st_p5),
        ("P6", "vendored file edited on disk without a manifest hash bump", _st_p6),
        ("P7", "file inventory incomplete in both directions", _st_p7),
        ("P8", "locally_modified with no patch id; local origin with no reason", _st_p8),
        ("P9", "vendored-mode drift: bad mode, on-disk exec bit, undocumented normalization", _st_p9),
        ("P10", "script shipped without scripts_audited (incl. extensionless shebang)", _st_p10),
        ("P11", "patch coverage broken in each of its three directions", _st_p11),
        ("P12", "skill references a closure file that was never vendored", _st_p12),
        ("P13", "exclusion entry with no reason / an invalid status", _st_p13),
        ("P14", "frontmatter allowlist widens per provider, not globally", _st_p14),
        ("P15", "{baseDir} link resolution is real, not a rubber stamp", _st_p15),
        ("P16", "fence pairing handles nested/longer fences; strippers divide labour", _st_p16),
        ("P17", "manifest invocation must agree with the skill's own frontmatter", _st_p17),
        ("P18", "every registry provider is file-level; a retired schema is rejected", _st_p18),
        ("P19", "an automated update candidate cannot masquerade as audited", _st_p19),
        ("P20", "only exact G1.1 workflow paths bypass the application-path guard", _st_p20),
        ("P23", "generic and G1 application-scope invocations are explicit and fail closed", _st_p23),
        ("P24", "G1 path collection covers HEAD, index, worktree, renames and unusual paths", _st_p24),
        ("P25", "production CLI separates generic runtime and dedicated G1 diffs", _st_p25),
        ("I1", "real section-7 inventory reconciles with manifests, disk, and itself", _st_i1),
        ("I2", "a missing installed skill and a section-7 name swap are caught", _st_i2),
        ("I3", "an unapproved 82nd directory, a stale total, and table drift are caught", _st_i3),
        ("I4", "an unparseable section 7 fails loudly", _st_i4),
        ("I5", "section 7's explicit-invocation list is machine-tied to the manifests", _st_i5),
        ("R1", "real routing doc covers every installed skill with true totals", _st_r1),
        ("R2", "an unrouted skill and a stale routing total are caught", _st_r2),
        ("P21", "native-plugin inventory ships empty and documents all three surfaces", _st_p21),
        ("P22", "registry owns the reviewed ref, manifests own the pin (no floating pin)", _st_p22),
        ("W1", "real stable maintenance workflow satisfies the closed envelope", _st_w1),
        ("W2", "stable workflow authority, identity and liveness mutations fail", _st_w2),
        ("W3", "real CI insertion is bounded and preserves exact product CI", _st_w3),
        ("W4", "CI trigger, checkout, runner and command mutations fail", _st_w4),
        ("W5", "file-backed workflow registry and stable release bytes are exact", _st_w5),
        ("W6", "control semantics use the exact static runtime verifier API", _st_w6),
        ("W7", "JSON and env source grammars reject ambiguous spellings", _st_w7),
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


def _argument_parser():
    parser = argparse.ArgumentParser(
        description="Validate repository governance with an explicit application concern scope.",
        allow_abbrev=False,
    )
    parser.add_argument(
        "--application-scope",
        choices=APPLICATION_SCOPES,
        default=APPLICATION_SCOPE_GENERIC,
        help="generic repository checks (default) or the dedicated G1 stable-skills scope guard",
    )
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--self-test", action="store_true",
                      help="run only the offline validator fixture matrix")
    mode.add_argument("--self-test-hook", action="store_true",
                      help="run repository validation plus the hook self-test")
    return parser


def parse_cli_args(argv):
    parser = _argument_parser()
    scope_count = sum(
        arg == "--application-scope" or arg.startswith("--application-scope=")
        for arg in argv
    )
    if scope_count > 1:
        parser.error("--application-scope may be specified exactly once")
    args = parser.parse_args(argv)
    if args.self_test and scope_count:
        parser.error("--self-test cannot be combined with --application-scope")
    return args


def main(argv=None):
    args = parse_cli_args(sys.argv[1:] if argv is None else argv)

    # Keep the callable entry point deterministic when embedded or exercised more than once in
    # one interpreter. Ordinary CLI execution also starts from the same explicit empty state.
    RESULTS.clear()

    if args.self_test:
        passed, total, failures = run_self_test()
        for fid, desc, err in failures:
            print("[FAIL] self-test %s (%s): %s" % (fid, desc, err))
        print("\n%d/%d self-test fixtures passed" % (passed, total))
        return 0 if not failures else 1

    invocation_details = application_scope_invocation_details(args.application_scope)
    if invocation_details:
        for detail in invocation_details:
            print("[FAIL] validation-invocation: %s" % detail)
        print("\n0/0 checks passed")
        print("Failed checks: validation-invocation")
        return 2

    for check in checks_for_application_scope(args.application_scope):
        check()
    if args.self_test_hook:
        check_hook_self_test()
    failed = [name for name, ok, _ in RESULTS if not ok]
    print("\n%d/%d checks passed" % (len(RESULTS) - len(failed), len(RESULTS)))
    if failed:
        print("Failed checks: %s" % ", ".join(failed))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
