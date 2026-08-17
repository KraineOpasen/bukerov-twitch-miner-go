"""Builders for hermetic update-bot scenarios.

A scenario is three things that have to agree with each other:

    * a synthetic **upstream** git repository with a BASE commit and a TARGET commit,
    * a synthetic **working tree** holding the vendored copy of BASE (optionally patched),
    * a **manifest** whose provenance matches that vendored copy exactly.

`Scenario.build()` produces all three from one declarative description, so a test says what
changed upstream and what we patched locally, and never hand-maintains a hash. That matters:
a fixture with a hand-written hash tests the hash you typed, not the behaviour you meant.

Upstream is reached through `gitio.UpstreamRepo` pointed straight at the local bare repo,
deliberately bypassing `fetch_commits()` (whose URL allowlist rightly refuses local paths).
The allowlist itself is covered by direct unit assertions in
`test_security.py::TestUpstreamUrlAllowlist`, so nothing is
left untested by this arrangement -- it just keeps network policy out of the merge tests.
"""

import json
import os
import subprocess

from .. import manifest as M
from ..config import Provider
from ..gitio import UpstreamRepo

FIXTURE_URL = "https://github.com/fixture/skills"

#: Minimal but realistic MIT notice. Its bytes are irrelevant; what matters is that a test can
#: change them and assert the licence condition fires.
LICENSE_TEXT = "MIT License\n\nCopyright (c) 2026 Fixture\n\nPermission is hereby granted...\n"

SKILL_MD = """---
name: %(name)s
description: A fixture skill for testing.
---

# %(name)s

Body line one.
Body line two.
Body line three.
Body line four.
Body line five.
"""


def _run(args, cwd):
    proc = subprocess.run(["git"] + args, cwd=cwd, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, text=True)
    if proc.returncode != 0:
        raise AssertionError("git %s failed: %s" % (" ".join(args[:2]), proc.stderr))
    return proc.stdout


def _write(path, content, mode=0o644):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    data = content.encode("utf-8") if isinstance(content, str) else content
    with open(path, "wb") as handle:
        handle.write(data)
    os.chmod(path, mode)


class Scenario:
    """One built scenario: upstream repo + working tree + manifest + provider."""

    def __init__(self, root, upstream_repo, base_sha, target_sha, provider, manifest_path):
        self.root = root
        self.upstream = upstream_repo
        self.base_sha = base_sha
        self.target_sha = target_sha
        self.provider = provider
        self.manifest_path = manifest_path

    def manifest(self):
        return M.load(self.manifest_path)

    def vendored(self, relpath):
        with open(os.path.join(self.root, relpath), "rb") as handle:
            return handle.read()


def build(tmp, base_files=None, target_files=None, vendored_overrides=None,
          skill_name="fixture-skill", license_at_target=LICENSE_TEXT,
          target_modes=None, extra_skills=None, target_extra_skills=None,
          root_files_at_target=None, key="fixture"):
    """Build a scenario.

    `base_files`  upstream file contents at BASE, relative to the skill's upstream dir.
    `target_files` contents at TARGET; defaults to BASE (i.e. a provenance-only commit).
                  A value of ``None`` deletes the file at TARGET.
    `vendored_overrides` what we actually vendored, where it differs from BASE (local patches).
    `target_modes` per-path git modes at TARGET, for testing mode drift.
    `extra_skills` sibling skills present in BOTH commits -- upstream skills this project has
                  long declined. They must NOT read as discoveries.
    `target_extra_skills` sibling skills that appear only at TARGET -- genuinely new upstream,
                  and the only thing that should produce a `Discovery`.
    """
    base_files = dict(base_files or {"SKILL.md": SKILL_MD % {"name": skill_name}})
    target_files = base_files if target_files is None else dict(target_files)
    vendored_overrides = dict(vendored_overrides or {})
    target_modes = dict(target_modes or {})

    up_dir = os.path.join(tmp, "upstream")
    os.makedirs(up_dir, exist_ok=True)
    _run(["init", "--quiet", "-b", "main", "."], cwd=up_dir)
    _run(["config", "user.email", "fixture@example.invalid"], cwd=up_dir)
    _run(["config", "user.name", "Fixture"], cwd=up_dir)

    skill_up = "skills/%s" % skill_name

    def commit(files, message, license_text, modes, siblings, root_files=None):
        # Rebuild the skill subtree from scratch each commit so deletions are real deletions.
        subtree = os.path.join(up_dir, skill_up)
        if os.path.isdir(subtree):
            _run(["rm", "-r", "-q", "--ignore-unmatch", "--", skill_up], cwd=up_dir)
        for rel, content in sorted(files.items()):
            if content is None:
                continue
            _write(os.path.join(up_dir, skill_up, rel), content,
                   0o755 if modes.get(rel) == "100755" else 0o644)
        for rel, mode in sorted(modes.items()):
            if mode == "120000":
                link = os.path.join(up_dir, skill_up, rel)
                os.makedirs(os.path.dirname(link), exist_ok=True)
                if os.path.lexists(link):
                    os.remove(link)
                os.symlink("SKILL.md", link)
        for name, extra in sorted(siblings.items()):
            for rel, content in sorted(extra.items()):
                _write(os.path.join(up_dir, "skills", name, rel), content)
        # Repository-root files (package.json, .claude-plugin/plugin.json) -- where a provider
        # declares its own release version.
        for rel, content in sorted((root_files or {}).items()):
            _write(os.path.join(up_dir, rel), content)
        license_path = os.path.join(up_dir, "LICENSE")
        if license_text is not None:
            _write(license_path, license_text)
        elif os.path.exists(license_path):
            # `license_at_target=None` means "upstream deleted the licence". Files persist
            # across commits in a real repository, so the deletion has to be performed.
            os.remove(license_path)
        _run(["add", "-A", "."], cwd=up_dir)
        _run(["commit", "--quiet", "--allow-empty", "-m", message], cwd=up_dir)
        return _run(["rev-parse", "HEAD"], cwd=up_dir).strip()

    base_siblings = dict(extra_skills or {})
    target_siblings = dict(base_siblings)
    target_siblings.update(target_extra_skills or {})
    base_sha = commit(base_files, "base", LICENSE_TEXT, {}, base_siblings)
    target_sha = commit(target_files, "target", license_at_target, target_modes,
                        target_siblings, root_files_at_target)

    # --- working tree: the vendored copy of BASE, plus any local patches ------------------
    root = os.path.join(tmp, "repo")
    skills_dir = os.path.join(root, ".claude", "skills", skill_name)
    docs = os.path.join(root, "docs", "agents")
    os.makedirs(docs, exist_ok=True)

    upstream = UpstreamRepo(up_dir, FIXTURE_URL)
    base_entries = upstream.tree_entries(base_sha)

    files = []
    for rel, content in sorted(base_files.items()):
        if content is None:
            continue
        vendored_content = vendored_overrides.get(rel, content)
        vpath = ".claude/skills/%s/%s" % (skill_name, rel)
        _write(os.path.join(root, vpath), vendored_content)
        data = (vendored_content.encode("utf-8") if isinstance(vendored_content, str)
                else vendored_content)
        upstream_path = "%s/%s" % (skill_up, rel)
        meta = base_entries[upstream_path]
        files.append({
            "path": vpath,
            "origin": "upstream",
            "upstream_path": upstream_path,
            "upstream_blob_sha": meta[2],
            "upstream_mode": meta[0],
            "vendored_mode": "100644",
            "vendored_blob_sha": M.git_blob_sha(data),
            "locally_modified": M.git_blob_sha(data) != meta[2],
            "patch_ids": M.marker_ids(vpath, data),
        })

    _write(os.path.join(skills_dir, "LICENSE"), LICENSE_TEXT)

    manifest = {
        "upstream_repo": FIXTURE_URL,
        "upstream_commit": base_sha,
        "upstream_tree": {skill_name: upstream.path_tree_sha(base_sha, skill_up)},
        "reviewed_at": "2026-01-01T00:00:00Z",
        "reviewed_by": "fixture review",
        "installation_mode": "project-local-vendored-copy",
        "automatic_updates": False,
        "license": {
            "spdx": "MIT",
            "layout": "shared",
            "upstream_path": "LICENSE",
            "upstream_blob_sha": base_entries["LICENSE"][2],
        },
        "skills": [{
            "name": skill_name,
            "path": ".claude/skills/%s" % skill_name,
            "upstream_path": skill_up,
            "invocation": "model",
            "locally_modified": any(f["locally_modified"] for f in files),
            "patch_ids": sorted({p for f in files for p in f["patch_ids"]}),
            "files": files,
        }],
        "excluded_skills": [],
    }
    manifest_rel = "docs/agents/%s-skills-manifest.json" % key
    with open(os.path.join(root, manifest_rel), "w", encoding="utf-8") as handle:
        handle.write(M.dump(manifest))
    for suffix in ("policy", "patches"):
        _write(os.path.join(docs, "%s-skills-%s.md" % (key, suffix)),
               "# fixture %s\n\nlocal-patch\n" % suffix)

    provider = Provider({
        "key": key,
        "upstream_repo": FIXTURE_URL,
        "upstream_ref": "main",
        "manifest": manifest_rel,
        "policy": "docs/agents/%s-skills-policy.md" % key,
        "patches": "docs/agents/%s-skills-patches.md" % key,
    }, root)
    return Scenario(root, upstream, base_sha, target_sha, provider,
                    os.path.join(root, manifest_rel))


def init_work_repo(root):
    """Make `root` a git repo with a bare `origin`, so publish paths can be exercised."""
    bare = root + "-origin.git"
    _run(["init", "--quiet", "--bare", bare], cwd=os.path.dirname(root))
    _run(["init", "--quiet", "-b", "main", "."], cwd=root)
    _run(["config", "user.email", "fixture@example.invalid"], cwd=root)
    _run(["config", "user.name", "Fixture"], cwd=root)
    _run(["add", "-A", "."], cwd=root)
    _run(["commit", "--quiet", "-m", "initial"], cwd=root)
    _run(["remote", "add", "origin", bare], cwd=root)
    _run(["push", "--quiet", "-u", "origin", "main"], cwd=root)
    return bare
