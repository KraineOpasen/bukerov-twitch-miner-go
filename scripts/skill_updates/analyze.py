"""Drift detection and the BLOCKED-condition classifier.

This module decides one thing per provider: *can the move from the pinned commit to the
resolved target commit be prepared mechanically, with no judgement call?* It answers with an
`Analysis` carrying either a set of file changes or a list of `BlockedReason`s -- never both as
a partial result. A partially-prepared update is the outcome this design exists to prevent: a
PR that refreshes eight files and quietly skips the ninth reads exactly like a complete one.

The eight blocked conditions, and the principle behind each
-----------------------------------------------------------
Every one of them is a case where the *right* answer depends on reading something -- new prose,
a new licence, a changed trigger surface -- and where a machine picking a side would be
manufacturing a review that never happened.

    conflict            our patch and upstream's edit touch the same region
    skill-set           a selected skill was added, deleted or renamed upstream
    inventory           the file set of a selected skill changed
    closure             merged content points at a file that is not vendored
    licence             the licence text or its presence changed
    executable          a symlink, submodule, or executable bit appeared
    authority           a skill's invocation/frontmatter authority surface changed
    patch-map           a local patch no longer maps onto the merged file
    unprovable          the upstream ref or commit could not be proven

Ordering is fixed by `BLOCK_ORDER` so two runs over the same inputs emit reasons in the same
sequence -- issue bodies are compared for deduplication, and an unstable order would make an
unchanged situation look like a new one on every run.

Nothing here writes to disk, clones anything, or talks to GitHub. It reads bytes from an
`UpstreamRepo` and from the working tree and returns a verdict, which is why the whole
classifier is exercisable from temporary local git repositories in the test suite.
"""

import json
import os
import posixpath

from . import ancestry as ancestry_mod
from . import manifest as M
from . import merge3
from . import states

CONFLICT = "conflict"
SKILL_SET = "skill-set"
INVENTORY = "inventory"
CLOSURE = "closure"
LICENCE = "licence"
EXECUTABLE = "executable"
AUTHORITY = "authority"
PATCH_MAP = "patch-map"
UNPROVABLE = "unprovable"
ANCESTRY = "ancestry"

#: Deterministic reporting order. Roughly "most structural first": a rewritten history or a
#: renamed skill explains a dozen downstream inventory complaints, so it comes first.
BLOCK_ORDER = (UNPROVABLE, ANCESTRY, SKILL_SET, INVENTORY, LICENCE, EXECUTABLE, AUTHORITY,
               CONFLICT, PATCH_MAP, CLOSURE)

#: Frontmatter keys that constitute a skill's *trigger and authority surface* -- everything that
#: determines whether the skill fires, when it fires, what it may touch, and how it runs.
#:
#: `description` and `when_to_use` ARE included. They are what the model reads to decide whether
#: to invoke a skill at all, so an upstream rewording is a change to the *trigger* surface even
#: when it reads as ordinary prose -- and this project's whole reason for vendoring is that a
#: skill's behaviour is reviewed at a specific commit. The cost is real (more updates need a
#: human) and is accepted deliberately: a mechanical checker cannot tell a clarifying reword from
#: one that widens when a skill fires, and only a reader can.
AUTHORITY_KEYS = (
    "name", "description", "when_to_use",
    "disable-model-invocation", "user-invocable",
    "allowed-tools", "disallowed-tools",
    "model", "effort", "context", "agent", "hooks", "paths", "shell",
    "argument-hint", "type",
)

#: Modes that are never acceptable in a vendored tree, no matter what the pinned commit looked
#: like. A symlink can point outside the skill directory and a submodule is an unpinned second
#: dependency; neither has ever appeared in these six providers' selected subtrees, and neither
#: could be vendored without a design decision.
NEVER_ALLOWED_MODES = ("120000", "160000")

#: The executable mode. NOT blocked on sight: several providers legitimately ship 100755
#: scripts, and this project's reviewed answer is to vendor them 100644 and record the
#: normalization with a patch id (see `ce-mode-normalize` in the compound-engineering manifest
#: and check_provider_vendored_modes in the governance validator). What is blocked is a file
#: becoming executable *between the pinned commit and the target* -- a capability appearing
#: where a reviewer last saw none. Blocking on the mere presence of 100755 would refuse every
#: compound-engineering update forever while telling the reader nothing new.
EXEC_MODE = "100755"

#: Directory names that make an inline-code path reference a dependency-closure claim. Mirrors
#: CLOSURE_DIRS in scripts/validate-agent-governance.py; the test suite asserts they agree.
CLOSURE_DIRS = ("scripts", "references", "assets", "examples", "agents", "templates", "evals",
                "hooks", "reference", "resources", "commands", "rules", "prompts")


class BlockedReason:
    """One refusal, with enough evidence to act on without re-running the tool."""

    def __init__(self, code, summary, details=None):
        self.code = code
        self.summary = summary
        self.details = list(details or [])

    def to_dict(self):
        return {"code": self.code, "summary": self.summary, "details": list(self.details)}

    def __repr__(self):
        return "<Blocked %s: %s>" % (self.code, self.summary)


class FileChange:
    """One vendored file's resolved three-way state."""

    def __init__(self, path, upstream_path, verdict, content, old_sha, new_sha):
        self.path = path
        self.upstream_path = upstream_path
        self.verdict = verdict
        self.content = content
        self.old_sha = old_sha
        self.new_sha = new_sha

    @property
    def changed(self):
        return self.old_sha != self.new_sha

    def to_dict(self):
        return {"path": self.path, "verdict": self.verdict,
                "old_vendored_blob_sha": self.old_sha, "new_vendored_blob_sha": self.new_sha}


class Discovery:
    """A new sibling skill upstream that this project has never selected.

    Deliberately NOT a blocked condition. Upstream adding an unrelated skill says nothing about
    the skills we vendor, and refusing an otherwise-clean refresh because of it would make every
    active provider permanently un-updatable. It is surfaced as its own deduplicated issue so the
    choice to review and adopt it stays a human one, taken on its own schedule.
    """

    def __init__(self, name, upstream_path, provider_key):
        self.name = name
        self.upstream_path = upstream_path
        self.provider_key = provider_key

    def to_dict(self):
        return {"name": self.name, "upstream_path": self.upstream_path,
                "provider": self.provider_key}


class Analysis:
    """The verdict for one provider."""

    def __init__(self, key, upstream_repo, upstream_ref, pinned_sha, target_sha,
                 monitor_only=False, ancestry=None):
        self.key = key
        self.upstream_repo = upstream_repo
        self.upstream_ref = upstream_ref
        self.pinned_sha = pinned_sha
        self.target_sha = target_sha
        self.monitor_only = monitor_only
        self.ancestry = ancestry
        self.blocked = []
        self.changes = []
        self.new_manifest = None
        self.notes = []
        #: New sibling skills upstream, outside the installed selection (see `Discovery`).
        self.discoveries = []
        #: Reasons this candidate cannot be argued equivalent by provenance alone. Static gates
        #: prove which BYTES changed; they cannot prove a skill still behaves the same way.
        self.eval_required = []

    @property
    def drifted(self):
        """True when the reviewed ref no longer points at the pinned commit.

        Compares FULL 40-hex SHAs -- never abbreviations. A short-SHA comparison can collide,
        and the collision would express itself as "no drift", which is the one wrong answer that
        produces silence instead of a report.

        `target_sha` is None only when the ref could not be resolved, which is itself an
        UNPROVABLE block, so "unknown" is never reported as "no drift".
        """
        return self.target_sha is not None and self.target_sha != self.pinned_sha

    @property
    def is_blocked(self):
        return bool(self.blocked)

    @property
    def changed_files(self):
        return [c for c in self.changes if c.changed]

    @property
    def state(self):
        """The candidate state machine value for this analysis (see `states.py`)."""
        return states.classify(self.drifted, self.is_blocked, self.new_manifest is not None)

    def block(self, code, summary, details=None):
        self.blocked.append(BlockedReason(code, summary, details))

    def needs_eval(self, reason):
        if reason not in self.eval_required:
            self.eval_required.append(reason)

    def sorted_blocked(self):
        """Blocked reasons in `BLOCK_ORDER`, then by summary -- stable across runs."""
        rank = {code: index for index, code in enumerate(BLOCK_ORDER)}
        return sorted(self.blocked, key=lambda r: (rank.get(r.code, len(rank)), r.summary))

    def to_dict(self):
        return {
            "provider": self.key,
            "state": self.state,
            "upstream_repo": self.upstream_repo,
            "upstream_ref": self.upstream_ref,
            "pinned_sha": self.pinned_sha,
            "target_sha": self.target_sha,
            "monitor_only": self.monitor_only,
            "drifted": self.drifted,
            "ancestry": self.ancestry.to_dict() if self.ancestry else None,
            "blocked": [r.to_dict() for r in self.sorted_blocked()],
            "discoveries": [d.to_dict() for d in self.discoveries],
            "eval_required": list(self.eval_required),
            "changed_files": [c.to_dict() for c in self.changed_files],
            "changed_file_count": len(self.changed_files),
            "notes": list(self.notes),
        }


def _subtree(entries, prefix):
    """Entries under `prefix`, keyed by path RELATIVE to it.

    The trailing-slash guard matters: without it, the prefix ``skills/engineering/tdd`` would
    also capture ``skills/engineering/tdd-extras/...`` and report a phantom inventory change.
    """
    root = prefix.rstrip("/") + "/"
    return {path[len(root):]: meta for path, meta in entries.items() if path.startswith(root)}


def _closure_refs(text):
    """Inline-code path references that count as dependency-closure claims."""
    import re
    pattern = re.compile(r"`(?:\{baseDir\}/)?([A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+)`")
    return sorted(set(pattern.findall(text)))


def analyze_monitor(provider, repo, target_sha):
    """Audit-only providers: report a change to any watched path, and nothing else.

    Deliberately narrow. This repository was reviewed and NOT vendored on provenance grounds,
    so ordinary commits to it are noise; the only signal worth a human's attention is the
    licence surface moving, because that is the fact the exclusion decision rests on. A commit
    that leaves every watched path identical produces no output at all.
    """
    analysis = Analysis(provider.key, provider.upstream_repo, provider.upstream_ref,
                        provider.baseline_commit, target_sha, monitor_only=True)
    if target_sha is None:
        analysis.block(UNPROVABLE, "could not resolve %s on %s"
                       % (provider.upstream_ref, provider.upstream_repo))
        return analysis
    if target_sha == provider.baseline_commit:
        return analysis
    # Verify the recorded baseline against the baseline COMMIT before trusting it as the thing
    # to compare against. Otherwise a stale `watch_baseline` would make a real licence change
    # look like no change at all -- the one failure mode this monitor exists to prevent.
    if repo.commit_exists(provider.baseline_commit):
        at_baseline = repo.tree_entries(provider.baseline_commit)
        stale = []
        for path in provider.watch_paths:
            recorded = provider.watch_baseline.get(path)
            actual = at_baseline[path][2] if path in at_baseline else None
            if recorded != actual:
                stale.append("%s: config records %s, baseline commit has %s"
                             % (path, recorded, actual))
        if stale:
            analysis.block(UNPROVABLE,
                           "watch_baseline does not match the recorded baseline commit",
                           stale)
            return analysis
    entries = repo.tree_entries(target_sha)
    moved = []
    for path in provider.watch_paths:
        recorded = provider.watch_baseline.get(path)
        current = entries[path][2] if path in entries else None
        if current == recorded:
            continue
        if recorded is None:
            moved.append("%s: ABSENT at baseline, now present as blob %s" % (path, current))
        elif current is None:
            moved.append("%s: blob %s at baseline, now ABSENT" % (path, recorded))
        else:
            moved.append("%s: blob %s -> %s" % (path, recorded, current))
    if moved:
        analysis.block(LICENCE,
                       "watched licence path(s) changed in audit-only upstream %s"
                       % provider.upstream_repo, moved)
    else:
        analysis.notes.append(
            "commit moved %s -> %s but every watched licence path is unchanged; audit-only "
            "monitor stays quiet by design" % (provider.baseline_commit[:8], target_sha[:8]))
    return analysis


def analyze_provider(provider, repo_root, repo, target_sha, default_ref=None):
    """Classify the move from the provider's pinned commit to `target_sha`.

    Returns an `Analysis`. When it is not blocked and `drifted` is true, `changes` holds every
    vendored file's resolved bytes and `new_manifest` holds the regenerated provenance, ready
    for `candidate.write()` to lay down.
    """
    doc = M.load(provider.manifest_path)
    pinned = doc["upstream_commit"]
    analysis = Analysis(provider.key, provider.upstream_repo, provider.upstream_ref,
                        pinned, target_sha)
    if target_sha is None:
        analysis.block(UNPROVABLE, "could not resolve ref %r on %s"
                       % (provider.upstream_ref, provider.upstream_repo))
        return analysis
    if target_sha == pinned:
        analysis.ancestry = ancestry_mod.Ancestry(
            ancestry_mod.EQUAL, pinned, target_sha, merge_base=pinned,
            configured_ref=provider.upstream_ref, default_ref=default_ref)
        return analysis

    for sha, label in ((pinned, "pinned"), (target_sha, "target")):
        if not repo.commit_exists(sha):
            analysis.block(UNPROVABLE,
                           "%s commit %s is not present in %s after fetch"
                           % (label, sha, provider.upstream_repo))
            return analysis

    # How upstream moved decides whether "the diff since the reviewed state" is even a
    # well-defined thing to audit. Only a fast-forward is.
    analysis.ancestry = ancestry_mod.classify(
        repo, pinned, target_sha, configured_ref=provider.upstream_ref, default_ref=default_ref)
    if not analysis.ancestry.advanceable:
        analysis.block(ANCESTRY,
                       "upstream history is %s, not a fast-forward from the reviewed commit"
                       % analysis.ancestry.relation,
                       [ancestry_mod.BLOCKING_REASON.get(analysis.ancestry.relation, ""),
                        analysis.ancestry.detail or "",
                        "pinned  %s" % pinned, "target  %s" % target_sha])
        return analysis
    if analysis.ancestry.ref_drifted:
        analysis.block(ANCESTRY,
                       "reviewed ref %r is no longer this repository's default branch (%r)"
                       % (provider.upstream_ref, analysis.ancestry.default_ref),
                       ["continuing to track %r may pin this project to an abandoned line of "
                        "development; confirm the intended branch and update "
                        "docs/agents/skills-update-providers.json deliberately"
                        % provider.upstream_ref])
        return analysis

    base_entries = repo.tree_entries(pinned)
    head_entries = repo.tree_entries(target_sha)
    _find_discoveries(analysis, doc, provider, base_entries, head_entries)

    _check_licence(analysis, doc, repo, base_entries, head_entries)

    new_doc = dict(doc)
    new_skills = []
    for skill in doc.get("skills", []):
        new_skills.append(_analyze_skill(analysis, provider, repo_root, repo, skill,
                                         base_entries, head_entries))
    if analysis.is_blocked:
        return analysis

    new_doc["upstream_commit"] = target_sha
    new_doc["upstream_tree"] = _rebuild_upstream_tree(doc, repo, target_sha)
    _rebuild_upstream_version(analysis, new_doc, repo, target_sha, head_entries)
    if "upstream_current_head" in new_doc:
        new_doc["upstream_current_head"] = target_sha
    if "drift" in new_doc:
        new_doc["drift"] = "none"
    new_doc["skills"] = [M.ordered_skill(s) for s in new_skills]
    analysis.new_manifest = new_doc
    _check_closure(analysis, provider, repo_root)
    if analysis.is_blocked:
        # A closure failure arrives after the manifest was assembled; drop it so a blocked
        # analysis can never hand a usable candidate to candidate.write().
        analysis.new_manifest = None
        return analysis
    _classify_eval(analysis)
    return analysis


def _find_discoveries(analysis, doc, provider, base_entries, head_entries):
    """Record new sibling skills upstream that this project has never selected.

    The search space is derived from the manifest rather than guessed: every installed skill's
    `upstream_path` has a parent directory (mattpocock uses `skills/<category>/<name>`, the
    others `skills/<name>`), and those parents are the directories where a *sibling* would
    appear. Anything new directly under one of them that carries a `SKILL.md`, and that is
    neither installed nor already recorded as excluded, is a discovery.

    Only names absent at the pinned commit count. A skill upstream has always had and this
    project deliberately never took is a settled decision, not news, and re-reporting it daily
    would bury the one case that matters.
    """
    installed = {s.get("name") for s in doc.get("skills", []) if isinstance(s, dict)}
    installed |= {posixpath.basename(s.get("upstream_path", "").rstrip("/"))
                  for s in doc.get("skills", []) if isinstance(s, dict)}
    excluded = set()
    for key in ("excluded", "excluded_skills"):
        for item in doc.get(key, []) or []:
            if isinstance(item, dict) and item.get("name"):
                excluded.add(item["name"])
            elif isinstance(item, str):
                excluded.add(item)

    parents = set()
    for skill in doc.get("skills", []):
        if isinstance(skill, dict) and skill.get("upstream_path"):
            parents.add(posixpath.dirname(skill["upstream_path"].rstrip("/")))
    parents.discard("")

    def skill_names_under(entries):
        found = {}
        for path in entries:
            if not path.endswith("/SKILL.md"):
                continue
            skill_dir = posixpath.dirname(path)
            if posixpath.dirname(skill_dir) in parents:
                found[posixpath.basename(skill_dir)] = skill_dir
        return found

    before = skill_names_under(base_entries)
    after = skill_names_under(head_entries)
    for name in sorted(set(after) - set(before)):
        if name in installed or name in excluded:
            continue
        analysis.discoveries.append(Discovery(name, after[name], provider.key))
    if analysis.discoveries:
        analysis.notes.append(
            "%d new sibling skill(s) appeared upstream outside the installed selection (%s); "
            "they are NOT installed automatically and do not block this refresh"
            % (len(analysis.discoveries),
               ", ".join(d.name for d in analysis.discoveries)))


#: Basenames whose content cannot change what a skill DOES. This is an EXEMPTION list, and the
#: direction matters: an earlier version listed the directories that DO trigger an eval, which
#: silently exempted 369 of the 602 vendored files -- including 268 Markdown files such as
#: `references/injection.md`, which is not documentation *about* a skill but the instruction text
#: the skill follows. Anything not exempted here triggers, so a new content directory upstream is
#: covered the day it appears rather than the day someone remembers to add it.
EVAL_EXEMPT_BASENAMES = ("LICENSE", "LICENSE.txt", "LICENSE.md", "LICENCE", "COPYING", "NOTICE")

#: Extensions that are assets rather than instructions.
EVAL_EXEMPT_EXTS = (".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico", ".webp", ".woff", ".woff2")


def _eval_exempt(path):
    """True when a changed file provably cannot alter behaviour."""
    name = posixpath.basename(path)
    return name in EVAL_EXEMPT_BASENAMES or name.lower().endswith(EVAL_EXEMPT_EXTS)


def _classify_eval(analysis):
    """Mark a candidate EVAL_REQUIRED when its changed bytes could change behaviour.

    Static provenance gates answer "which bytes changed, and were they reviewed at the old
    commit". They cannot answer "does this skill still do the same thing" -- a reworded
    instruction, a re-ordered agent topology, or a changed script can alter behaviour while every
    hash checks out. Recording that explicitly is the honest alternative to letting a green
    provenance run be mistaken for a behavioural guarantee.

    The rule is deliberately the inverse of a trigger list: **everything changed counts unless it
    is provably inert**. The module's own argument -- that provenance cannot establish
    behavioural equivalence -- applies to every instruction file a skill ships, not to a
    hand-picked set of directories, and a trigger list silently exempts whatever upstream invents
    next.

    Evals are never RUN here. They cost model time and money, and a scheduled GitHub Actions job
    is the wrong place to spend either; `report.eval_instructions()` emits what to run in a fresh
    Claude session instead.
    """
    for change in analysis.changed_files:
        path = change.path
        if _eval_exempt(path):
            continue
        name = posixpath.basename(path)
        if name == "SKILL.md":
            analysis.needs_eval(
                "%s changed: a skill's own instructions decide when it fires and what it does"
                % path)
        elif _is_scriptish(path, change.content):
            analysis.needs_eval("%s changed: executable content the skill runs" % path)
        else:
            analysis.needs_eval(
                "%s changed: content a skill reads and follows" % path)
    if analysis.eval_required:
        analysis.notes.append(
            "EVAL_REQUIRED: %d changed file(s) can alter behaviour; provenance checks do not "
            "establish behavioural equivalence" % len(analysis.eval_required))


def _rebuild_upstream_tree(doc, repo, target_sha):
    """Regenerate `upstream_tree` in whichever shape the manifest already uses.

    Two shapes exist across the six providers: a single tree SHA for the whole upstream commit
    (mattpocock) and a per-skill map of subdirectory tree SHAs (the other five). The shape is
    read from the existing document rather than chosen here, so regeneration never silently
    reformats a manifest into a different convention than the one its policy documents.
    """
    existing = doc.get("upstream_tree")
    if isinstance(existing, dict):
        rebuilt = {}
        for skill in doc.get("skills", []):
            if not isinstance(skill, dict) or "name" not in skill:
                continue
            if skill["name"] not in existing:
                continue
            tree = repo.path_tree_sha(target_sha, skill["upstream_path"])
            if tree:
                rebuilt[skill["name"]] = tree
        return rebuilt
    return repo.commit_tree_sha(target_sha)


#: Filenames that are a licence notice whatever a manifest calls them. Used as a backstop so a
#: provider that records its notice under an unexpected key is still compared.
LICENCE_BASENAMES = ("LICENSE", "LICENSE.txt", "LICENSE.md", "LICENCE", "COPYING", "NOTICE")


def licence_upstream_paths(doc):
    """Every UPSTREAM path this provider's licence notices are copied from.

    Three layouts exist across the six providers and all three must be covered, because a
    licence check that silently applies to one of them is worse than none -- the PR body tells
    the reviewer the licence was checked.

      * `license.upstream_path` -- mattpocock's single shared notice, which lives OUTSIDE every
        skill directory and therefore appears in no `files[]` array at all.
      * a `files[]` entry with `origin: "upstream"` whose basename is a licence filename --
        anthropic's per-skill `LICENSE.txt`, vendored straight from the skill's own subtree.
      * a `files[]` entry with `origin: "local"` that still records an `upstream_path` --
        trailofbits, compound-engineering, awesome-copilot and builderio copy the upstream ROOT
        `LICENSE` into each skill directory. `_analyze_file` returns early for local-origin
        files, so without this these 55 skills' notices were never compared against upstream at
        all.

    Returns ``{upstream_path: recorded_blob_sha_or_None}``.
    """
    paths = {}
    lic = doc.get("license") if isinstance(doc.get("license"), dict) else {}
    if lic.get("upstream_path"):
        paths[lic["upstream_path"]] = lic.get("upstream_blob_sha")
    per_skill = lic.get("per_skill_license_file")
    for skill in doc.get("skills", []):
        if not isinstance(skill, dict):
            continue
        for entry in skill.get("files", []) or []:
            if not isinstance(entry, dict) or not entry.get("upstream_path"):
                continue
            base = posixpath.basename(entry["path"])
            if base == per_skill or base in LICENCE_BASENAMES:
                # Keep the first recorded hash seen for a given upstream path; every skill that
                # copies the same root notice records the same value.
                paths.setdefault(entry["upstream_path"], entry.get("upstream_blob_sha"))
    return paths


#: Where a provider declares its own release version, most authoritative first. Mirrors the
#: precedence Claude Code itself uses for plugins (see `plugins.VERSION_SOURCES`).
VERSION_MANIFEST_PATHS = (".claude-plugin/plugin.json", "package.json")


def _rebuild_upstream_version(analysis, new_doc, repo, target_sha, head_entries):
    """Refresh `upstream_version`, or remove it rather than let a stale value survive.

    Two providers record the upstream project's own release version (mattpocock `1.2.3`,
    compound-engineering `3.22.0`). Carrying it forward unchanged across a pin move is the
    nastiest kind of stale field: because the line does not change, it never appears in the
    candidate diff at all, so a reviewer sees a manifest asserting a version that belongs to
    the *superseded* commit and has nothing to notice.

    So it is re-read from upstream at the target commit. When it cannot be resolved -- upstream
    dropped its `package.json`, or moved the version somewhere new -- the key is DELETED and a
    note is recorded, because "absent, and we said so" is honest where "unchanged" would be a
    false claim.
    """
    if "upstream_version" not in new_doc:
        return
    previous = new_doc.get("upstream_version")
    for path in VERSION_MANIFEST_PATHS:
        meta = head_entries.get(path)
        if not meta:
            continue
        blob = repo.blob(meta[2])
        if blob is None:
            continue
        try:
            version = json.loads(blob.decode("utf-8")).get("version")
        except (UnicodeDecodeError, ValueError, AttributeError):
            continue
        if isinstance(version, str) and version.strip():
            new_doc["upstream_version"] = version.strip()
            if version.strip() != previous:
                analysis.notes.append(
                    "upstream_version %s -> %s (read from %s at the target commit)"
                    % (previous, version.strip(), path))
            return
    del new_doc["upstream_version"]
    analysis.notes.append(
        "upstream_version (%s) could not be re-read at the target commit, so it was REMOVED "
        "rather than carried forward: it described the superseded commit, and an unchanged "
        "line would never have appeared in this diff for a reviewer to catch" % previous)


def _check_licence(analysis, doc, repo, base_entries, head_entries):
    """Refuse any change to a licence notice's presence or bytes, for EVERY layout.

    "Became unclear" is handled by the same rule as "changed": any delta at all is refused. A
    licence is the one artifact where a diff is never routine, and no automated reading of new
    licence text could be trusted anyway -- adopting a relicensing because the merge was clean
    is precisely the outcome a vendored, reviewed tree exists to prevent.
    """
    for upstream_path, recorded in sorted(licence_upstream_paths(doc).items()):
        base_meta = base_entries.get(upstream_path)
        head_meta = head_entries.get(upstream_path)
        if base_meta and recorded and base_meta[2] != recorded:
            analysis.block(LICENCE,
                           "recorded licence blob does not match the pinned commit",
                           ["%s: manifest records %s, pinned commit has %s"
                            % (upstream_path, recorded, base_meta[2])])
        if head_meta is None:
            analysis.block(LICENCE, "licence file disappeared upstream",
                           ["%s is absent at %s" % (upstream_path, analysis.target_sha)])
        elif base_meta and head_meta[2] != base_meta[2]:
            analysis.block(LICENCE, "licence text changed upstream",
                           ["%s: blob %s -> %s" % (upstream_path, base_meta[2], head_meta[2]),
                            "a relicensing is never adopted mechanically, however clean the "
                            "merge is"])


def _analyze_skill(analysis, provider, repo_root, repo, skill, base_entries, head_entries):
    """Resolve one skill; append blocked reasons to `analysis`. Returns the regenerated entry."""
    name = skill.get("name")
    upstream_dir = skill.get("upstream_path")
    new_skill = dict(skill)
    if not name or not upstream_dir:
        analysis.block(SKILL_SET, "manifest skill entry lacks name/upstream_path",
                       [repr(skill)[:200]])
        return new_skill

    base_sub = _subtree(base_entries, upstream_dir)
    head_sub = _subtree(head_entries, upstream_dir)
    if not head_sub:
        analysis.block(SKILL_SET, "selected skill %r no longer exists upstream" % name,
                       ["%s is empty or absent at %s" % (upstream_dir, analysis.target_sha)])
        return new_skill

    added = sorted(set(head_sub) - set(base_sub))
    removed = sorted(set(base_sub) - set(head_sub))
    if added or removed:
        detail = (["added upstream: %s/%s" % (upstream_dir, p) for p in added]
                  + ["removed upstream: %s/%s" % (upstream_dir, p) for p in removed])
        analysis.block(INVENTORY,
                       "file inventory of skill %r changed upstream" % name, detail)

    bad_modes = []
    for path, meta in sorted(head_sub.items()):
        mode = meta[0]
        was = base_sub.get(path, (None,))[0]
        if mode in NEVER_ALLOWED_MODES:
            bad_modes.append("%s/%s is mode %s (symlink/submodule)" % (upstream_dir, path, mode))
        elif mode == EXEC_MODE and was != EXEC_MODE:
            bad_modes.append("%s/%s became executable (%s -> %s)"
                             % (upstream_dir, path, was or "absent", mode))
    if bad_modes:
        analysis.block(EXECUTABLE,
                       "skill %r gained a symlink, submodule or newly executable file upstream"
                       % name, bad_modes)

    new_files = []
    for entry in skill.get("files", []) or []:
        new_files.append(_analyze_file(analysis, repo_root, repo, name, upstream_dir,
                                       entry, base_entries, head_entries))
    if new_files:
        new_skill["files"] = new_files
    _check_authority(analysis, repo, name, skill, base_entries, head_entries)
    _check_merged_authority(analysis, repo_root, name, skill, new_files)
    _carry_or_clear_script_audit(analysis, name, new_skill, new_files)

    modified = any(f.get("locally_modified") for f in new_files)
    new_skill["locally_modified"] = modified
    merged_ids = sorted({pid for f in new_files for pid in f.get("patch_ids", [])})
    if "patch_ids" in skill or merged_ids:
        new_skill["patch_ids"] = merged_ids
    return new_skill


def _analyze_file(analysis, repo_root, repo, skill_name, upstream_dir, entry,
                  base_entries, head_entries):
    """Resolve one file's three-way state and regenerate its provenance entry."""
    relpath = entry["path"]
    new_entry = dict(entry)
    origin = entry.get("origin", "upstream")

    kind = M.lstat_kind(repo_root, relpath)
    if kind != "file":
        analysis.block(EXECUTABLE if kind in ("symlink", "file-exec") else INVENTORY,
                       "vendored file %s is %s on disk" % (relpath, kind), [relpath])
        return new_entry
    ours = M.read_vendored(repo_root, relpath)

    if origin != "upstream":
        # Locally authored content: it has no upstream counterpart to merge against, so the
        # only thing to regenerate is its own hash. Recomputed rather than trusted, so an
        # edit to a local-origin file still shows up in the candidate's provenance.
        new_entry["vendored_blob_sha"] = M.git_blob_sha(ours)
        new_entry["vendored_mode"] = M.VENDORED_MODE
        analysis.changes.append(FileChange(relpath, None, "local-origin", ours,
                                           entry.get("vendored_blob_sha"),
                                           new_entry["vendored_blob_sha"]))
        return new_entry

    upstream_path = entry.get("upstream_path")
    if not upstream_path:
        analysis.block(INVENTORY, "upstream-origin file %s has no upstream_path" % relpath,
                       [relpath])
        return new_entry

    base_meta = base_entries.get(upstream_path)
    head_meta = head_entries.get(upstream_path)
    if head_meta is None:
        analysis.block(INVENTORY, "vendored file %s no longer exists upstream" % relpath,
                       ["%s absent at %s" % (upstream_path, analysis.target_sha)])
        return new_entry
    base_mode = base_meta[0] if base_meta else None
    if head_meta[0] in NEVER_ALLOWED_MODES or (
            head_meta[0] != base_mode and head_meta[0] == EXEC_MODE):
        analysis.block(EXECUTABLE,
                       "upstream mode of %s changed to %s" % (upstream_path, head_meta[0]),
                       ["%s: %s -> %s" % (upstream_path, base_mode or "absent", head_meta[0])])
        return new_entry

    # OURS must be the bytes the manifest says we vendored. The bot may run against a working
    # tree someone has edited; merging from an unrecorded state would produce a candidate whose
    # provenance describes bytes that were never reviewed. The governance validator catches
    # this too, but the bot must not depend on having been run after it.
    recorded_ours = entry.get("vendored_blob_sha")
    actual_ours = M.git_blob_sha(ours)
    if recorded_ours and actual_ours != recorded_ours:
        analysis.block(INVENTORY,
                       "vendored file %s does not match its recorded vendored_blob_sha" % relpath,
                       ["manifest records %s, on disk %s" % (recorded_ours, actual_ours)])
        return new_entry

    # BASE is taken from the manifest's recorded `upstream_blob_sha` -- a content address --
    # rather than by looking the path up at the pinned commit. The two must agree, and when
    # they do not the manifest does not describe the commit it claims to pin, which is an
    # integrity failure rather than an update to merge. Resolving BASE by path would also pair
    # OURS with the wrong file across an upstream rename.
    recorded_base = entry.get("upstream_blob_sha")
    if base_meta and recorded_base and base_meta[2] != recorded_base:
        analysis.block(UNPROVABLE,
                       "manifest upstream_blob_sha for %s does not match the pinned commit"
                       % relpath,
                       ["%s: manifest records %s, pinned commit has %s"
                        % (upstream_path, recorded_base, base_meta[2])])
        return new_entry

    theirs = repo.blob(head_meta[2])
    if theirs is None:
        analysis.block(UNPROVABLE, "could not read upstream blob for %s" % upstream_path,
                       [head_meta[2]])
        return new_entry
    base = repo.blob(recorded_base) if recorded_base else (
        repo.blob(base_meta[2]) if base_meta else None)
    if base is None:
        # No BASE means there is no common ancestor to reason from: either the pinned commit had
        # no such file, or the manifest records a blob this upstream does not contain. Either
        # way a three-way merge would be a guess.
        analysis.block(INVENTORY,
                       "vendored file %s has no provable counterpart at the pinned commit"
                       % relpath, [upstream_path, str(recorded_base)])
        return new_entry

    verdict, content, conflicts = merge3.resolve(base, ours, theirs)
    if verdict == merge3.CONFLICT:
        analysis.block(CONFLICT, "three-way merge conflict in %s" % relpath,
                       ["%d conflicting region(s); first ours=%r theirs=%r"
                        % (len(conflicts),
                           "".join(conflicts[0]["ours"])[:120],
                           "".join(conflicts[0]["theirs"])[:120])])
        return new_entry
    if verdict == merge3.BINARY_CONFLICT:
        analysis.block(CONFLICT,
                       "binary or undecodable file %s changed on both sides" % relpath,
                       [upstream_path])
        return new_entry

    # A local patch that no longer maps: every marker id present in OURS must survive into the
    # merged bytes. A clean merge that silently dropped a patch region would be the single most
    # dangerous "looks fine" outcome, because the governance intent the patch encoded is gone
    # while every hash still validates.
    #
    # With `merge3` as written this is provably unreachable, and that is worth stating rather
    # than leaving as a puzzle for the next reader: a marker only exists in OURS, so the chunk
    # containing it necessarily has ours != base; `merge_text` then resolves that chunk to OURS
    # (when theirs == base) or reports a CONFLICT (when theirs != base) -- there is no branch
    # that yields THEIRS for a chunk OURS changed. So the marker either survives or the file was
    # already blocked as a conflict.
    #
    # The check stays anyway, deliberately. It costs one set difference, and it is what turns
    # "the merge engine has this property today" into "any future change to the merge engine
    # that breaks the property fails closed instead of silently discarding a governance patch".
    # `test_patch_map_guard_fires_if_a_merge_engine_ever_drops_a_marker` pins that behaviour.
    before = set(M.marker_ids(relpath, ours))
    after = set(M.marker_ids(relpath, content))
    lost = sorted(before - after)
    if lost:
        analysis.block(PATCH_MAP,
                       "local patch id(s) no longer map onto merged %s" % relpath,
                       ["lost: %s" % ", ".join(lost)])
        return new_entry

    new_entry["origin"] = "upstream"
    new_entry["upstream_blob_sha"] = head_meta[2]
    new_entry["upstream_mode"] = head_meta[0]
    new_entry["vendored_mode"] = M.VENDORED_MODE
    new_entry["vendored_blob_sha"] = M.git_blob_sha(content)
    new_entry["locally_modified"] = content != theirs
    # Patch ids that are NOT in-file markers document something other than file content -- in
    # practice a mode normalization (`ce-mode-normalize`), which is exactly the case where
    # check_provider_vendored_modes REQUIRES an id to be present. Regenerating purely from
    # content markers would silently drop those and turn a documented normalization into an
    # undocumented one, so they are carried forward. Carrying a now-redundant id costs nothing:
    # the validator requires an id when the modes differ, it never forbids one when they match.
    non_content = sorted(set(entry.get("patch_ids", [])) - before)
    new_entry["patch_ids"] = sorted(after | set(non_content))
    analysis.changes.append(FileChange(relpath, upstream_path, verdict, content,
                                       entry.get("vendored_blob_sha"),
                                       new_entry["vendored_blob_sha"]))
    return new_entry


def _check_authority(analysis, repo, skill_name, skill, base_entries, head_entries):
    """Refuse an upstream change to a skill's invocation/authority surface.

    Compares BASE against THEIRS -- upstream's own change -- not OURS against THEIRS. Comparing
    against the vendored copy would fire on every skill this project has deliberately patched
    to be user-invoked, reporting our own reviewed decision as upstream drift.

    Both the *values* of `AUTHORITY_KEYS` and the *set* of frontmatter keys are compared: a new
    key appearing is a new surface, even when this tool does not yet know what it means.
    """
    skill_md = posixpath.join(skill.get("upstream_path", ""), "SKILL.md")
    base_meta = base_entries.get(skill_md)
    head_meta = head_entries.get(skill_md)
    if not base_meta or not head_meta or base_meta[2] == head_meta[2]:
        return
    base_fm, base_ok = M.parse_frontmatter(repo.blob(base_meta[2]) or b"")
    head_fm, head_ok = M.parse_frontmatter(repo.blob(head_meta[2]) or b"")
    if base_ok and not head_ok:
        analysis.block(AUTHORITY, "skill %r lost its frontmatter fence upstream" % skill_name,
                       [skill_md])
        return
    if not base_ok:
        return
    details = []
    for key in AUTHORITY_KEYS:
        if base_fm.get(key) != head_fm.get(key):
            details.append("%s: %r -> %r" % (key, base_fm.get(key), head_fm.get(key)))
    new_keys = sorted(set(head_fm) - set(base_fm))
    gone_keys = sorted(set(base_fm) - set(head_fm))
    if new_keys:
        details.append("new frontmatter key(s): %s" % ", ".join(new_keys))
    if gone_keys:
        details.append("removed frontmatter key(s): %s" % ", ".join(gone_keys))
    if details:
        analysis.block(AUTHORITY,
                       "skill %r frontmatter authority surface changed upstream" % skill_name,
                       details)


#: File extensions (plus the extensionless-with-shebang case) that make a vendored file
#: "executable-ish". Mirrors SCRIPT_EXTS in scripts/validate-agent-governance.py.
SCRIPT_EXTS = (".py", ".html", ".sh", ".mjs", ".js", ".bash", ".zsh")


def _is_scriptish(path, data):
    if path.endswith(SCRIPT_EXTS):
        return True
    if "." not in posixpath.basename(path):
        return data is not None and data.startswith(b"#!")
    return False


def _check_merged_authority(analysis, repo_root, skill_name, skill, new_files):
    """The MERGED file must still carry the authority surface we vendored.

    `_check_authority` compares BASE against THEIRS -- what upstream changed. This compares
    OURS against the merged result -- what we might have lost. They are different questions,
    and the second one has no other guard.

    It matters because this project's most important local patches are frontmatter lines
    (`disable-model-invocation: true` on wizard, resolving-merge-conflicts, writing-for-agents,
    skill-creator-anthropic and others), and a frontmatter line CANNOT carry a
    `bukerov-local-patch` marker -- an HTML comment inside a YAML fence would not parse. So the
    patch-id survival check in `_analyze_file` is structurally blind to exactly the patches that
    decide whether a skill can fire from model judgement alone. This closes that gap.
    """
    for entry in new_files:
        path = entry.get("path", "")
        if not path.endswith("SKILL.md"):
            continue
        change = next((c for c in analysis.changes if c.path == path), None)
        if change is None or change.content is None:
            continue
        ours = M.read_vendored(repo_root, path)
        if ours is None:
            continue
        ours_fm, ours_ok = M.parse_frontmatter(ours)
        merged_fm, merged_ok = M.parse_frontmatter(change.content)
        if not ours_ok:
            continue
        if not merged_ok:
            analysis.block(AUTHORITY,
                           "merged %s lost its frontmatter fence" % path, [path])
            continue
        lost = ["%s: %r -> %r" % (key, ours_fm.get(key), merged_fm.get(key))
                for key in AUTHORITY_KEYS if ours_fm.get(key) != merged_fm.get(key)]
        if lost:
            analysis.block(AUTHORITY,
                           "merging %s would change the vendored authority surface of %r"
                           % (path, skill_name), lost)


def _carry_or_clear_script_audit(analysis, skill_name, new_skill, new_files):
    """`scripts_audited` is a claim that a human read a script end to end. Do not let it stick.

    The manifest's judgement fields are carried through a regeneration untouched, which is right
    for `reviewed_by` and wrong for this one: if the candidate CHANGES a script's bytes, the
    attestation would survive while the bytes it referred to did not. The governance validator's
    normal defence -- a `vendored_blob_sha` mismatch meaning "re-audit, not just re-hash" --
    cannot fire here, because the bot is the thing doing the rehash.

    So when a candidate rewrites a script, the attestation is withdrawn rather than carried.
    `check_provider_scripts_audited` then FAILS while the skill still ships scripts, which is
    the correct outcome: a human has to read the new bytes and re-assert. This survives even
    after someone clears the `automated_candidate` block, so it cannot be waved through by
    deleting one field.
    """
    if not new_skill.get("scripts_audited"):
        return
    changed = sorted(
        c.path for c in analysis.changes
        if c.changed and c.content is not None
        and any(c.path == f.get("path") for f in new_files)
        and _is_scriptish(c.path, c.content))
    if not changed:
        return
    new_skill["scripts_audited"] = False
    new_skill["scripts_reaudit_required"] = changed
    new_skill["audit_ref"] = (
        "SUPERSEDED — the previous audit described bytes this candidate replaced. Re-read %s "
        "end to end and re-assert scripts_audited. (Previous note: %s)"
        % (", ".join(changed), new_skill.get("audit_ref") or "none recorded"))
    analysis.notes.append(
        "skill %r ships script(s) whose bytes changed (%s); scripts_audited was withdrawn and "
        "must be re-established by a human reading the new content"
        % (skill_name, ", ".join(changed)))


def _check_closure(analysis, provider, repo_root):
    """Every closure path a merged Markdown file points at must exist in the candidate tree.

    Run against the merged bytes rather than the on-disk tree, because the candidate has not
    been written yet: an upstream release that adds "run `scripts/new-thing.sh`" to a SKILL.md
    would otherwise produce a candidate whose prose references a file that was never vendored.
    """
    merged = {c.path: c.content for c in analysis.changes if c.content is not None}
    vendored_paths = set()
    for skill in analysis.new_manifest.get("skills", []):
        for entry in skill.get("files", []) or []:
            if isinstance(entry, dict) and entry.get("path"):
                vendored_paths.add(entry["path"])
    details = []
    for skill in analysis.new_manifest.get("skills", []):
        skill_dir = skill.get("path")
        if not skill_dir:
            continue
        present_dirs = {d for d in CLOSURE_DIRS
                        if any(p.startswith("%s/%s/" % (skill_dir, d)) for p in vendored_paths)}
        if not present_dirs:
            continue
        for entry in skill.get("files", []) or []:
            path = entry.get("path", "")
            if not path.endswith(".md"):
                continue
            data = merged.get(path)
            if data is None:
                data = M.read_vendored(repo_root, path)
            if data is None:
                continue
            try:
                text = data.decode("utf-8")
            except UnicodeDecodeError:
                continue
            for target in _closure_refs(text):
                head = target.split("/", 1)[0]
                if head not in present_dirs:
                    continue
                if posixpath.join(skill_dir, target) not in vendored_paths:
                    details.append("%s references %r, which is not vendored" % (path, target))
    if details:
        analysis.block(CLOSURE, "merged content references files that are not vendored",
                       sorted(set(details)))


# Ref resolution deliberately lives in `ancestry.resolve()`, not here. An earlier
# `resolve_target()` did the same `ls-remote` but returned only the SHA -- so a caller using it
# silently skipped the remote's default-branch lookup, and the reviewed ref quietly ceasing to be
# the default branch would go unreported. Keeping one resolver means that check cannot be
# bypassed by picking the wrong helper.
