"""Writing a prepared update candidate to an inert artifact tree.

A *candidate* is the mechanical half of an update: refreshed vendored bytes plus regenerated
provenance. It is explicitly **not** a reviewed pin, and this module is where that distinction
is made structural rather than merely stated in a PR body.

The anti-masquerade guarantee
-----------------------------
Every candidate manifest carries an `automated_candidate` block, and
`scripts/validate-agent-governance.py` **fails** while that block is present. So a candidate
cannot pass the repository's own governance gate no matter how clean its merge was, and the
only way to clear it is the thing automation must never do for itself: a human (or an agent
under a task contract) reads the diff, records a fresh `reviewed_at`/`reviewed_by`, and deletes
the block.

Note what is deliberately *not* done here: `reviewed_at` and `reviewed_by` are left exactly as
they were. They are true statements about the previous pin, and rewriting them to today's date
would be the bot asserting a review it did not perform -- the precise failure this design is
built to make impossible. The `automated_candidate` block says out loud that those fields refer
to the superseded commit.

Artifact identity is supplied by the stable runtime owner.  It binds the repository, stable
branch, full live stable base, provider/upstream pins, full target, aggregate governed-control
digest, updater source, and pinned Action closure.  Display locators may truncate those hashes;
authority never does.
"""

import os

from . import manifest as M
from . import runtime, states
from .config import valid_provider_key
from .errors import ConfigError
from .gitio import validate_sha

#: The banner every candidate PR body must open with.
PR_BANNER = "AUTOMATED UPDATE ARTIFACT — AUDIT REQUIRED — PUBLICATION UNCOMMISSIONED."

#: Length of the SHA fragment used in branch names and issue titles. Twelve hex characters is
#: far past the ambiguity threshold for these repositories while staying readable.
SHA_FRAGMENT = 12


def ensure_artifact_directory(path):
    """Create/normalize one owned artifact directory to exact mode 0700."""
    os.makedirs(path, mode=0o700, exist_ok=True)
    if not os.path.isdir(path) or os.path.islink(path):
        raise ConfigError("artifact directory is not a real directory: %r" % path)
    os.chmod(path, 0o700)
    return path


def create_artifact_root(path):
    """Exclusively create the owned artifact root and force exact mode 0700."""
    os.mkdir(path, 0o700)
    os.chmod(path, 0o700)
    return path


def _ensure_artifact_parents(root, target):
    """Create every owned directory from `root` to `target` with exact mode 0700."""
    root = os.path.realpath(root)
    directory = os.path.dirname(os.path.realpath(target))
    ensure_artifact_directory(root)
    relative = os.path.relpath(directory, root)
    if relative == os.curdir:
        return
    current = root
    for part in relative.split(os.sep):
        current = os.path.join(current, part)
        ensure_artifact_directory(current)


def write_new_artifact(path, content, *, binary=False):
    """Exclusively create one artifact and force exact mode 0644 after a complete write."""
    mode = "xb" if binary else "x"
    kwargs = {} if binary else {"encoding": "utf-8"}
    with open(path, mode, **kwargs) as handle:
        handle.write(content)
    os.chmod(path, 0o644)
    return path


def _identity_dict(identity, analysis=None, provider=None):
    """Validate and serialize the runtime-owned candidate identity."""
    if identity is None or not callable(getattr(identity, "to_dict", None)):
        raise ConfigError("candidate identity must be a verified runtime CandidateIdentity")
    doc = identity.to_dict()
    required = {
        "schema", "repo_id", "repo_full_name", "stable_branch", "stable_base_sha",
        "provider", "upstream_repo", "old_pin", "target_sha", "control_input_digest",
        "updater_source_sha", "pinned_action_digests", "proposal_id", "locator",
    }
    if not isinstance(doc, dict) or set(doc) != required:
        raise ConfigError("candidate identity does not match the closed G1.1 schema")
    if not valid_provider_key(doc["provider"]):
        raise ConfigError("candidate identity has unsafe provider %r" % doc["provider"])
    validate_sha(doc["stable_base_sha"], "stable base")
    validate_sha(doc["old_pin"], "old pin")
    validate_sha(doc["target_sha"], "target")
    for field in ("control_input_digest", "updater_source_sha"):
        if (not isinstance(doc[field], str) or len(doc[field]) != 64
                or any(c not in "0123456789abcdef" for c in doc[field])):
            raise ConfigError("candidate identity %s is not full lowercase SHA-256" % field)
    actions = doc["pinned_action_digests"]
    if (not isinstance(actions, dict) or not actions
            or any(not isinstance(key, str) or not key for key in actions)
            or any(not isinstance(value, str)
                   or len(value) != (40 if key.endswith("sha1") else 64)
                   or any(c not in "0123456789abcdef" for c in value)
                   for key, value in actions.items())):
        raise ConfigError("candidate identity pinned action digests are not closed SHA-256s")
    if (not isinstance(doc["proposal_id"], str) or len(doc["proposal_id"]) != 64
            or any(c not in "0123456789abcdef" for c in doc["proposal_id"])):
        raise ConfigError("candidate identity proposal id is not full lowercase SHA-256")
    try:
        canonical = runtime.candidate_identity(
            provider=doc["provider"],
            stable_branch=doc["stable_branch"],
            stable_base_sha=doc["stable_base_sha"],
            target_sha=doc["target_sha"],
            upstream_repo=doc["upstream_repo"],
            old_pin=doc["old_pin"],
            control_input_digest=doc["control_input_digest"],
            updater_source_sha=doc["updater_source_sha"],
            pinned_action_digests=doc["pinned_action_digests"],
        ).to_dict()
    except runtime.RuntimeEnvelopeError as exc:
        raise ConfigError("candidate identity failed runtime validation: %s" % exc) from exc
    if canonical != doc:
        raise ConfigError("candidate identity is not the canonical G1.1 proposal descriptor")
    if analysis is not None:
        if doc["target_sha"] != analysis.target_sha:
            raise ConfigError("candidate identity target does not match analyzed target")
        if doc["old_pin"] != analysis.pinned_sha:
            raise ConfigError("candidate identity old pin does not match analyzed pin")
        if doc["upstream_repo"] != analysis.upstream_repo:
            raise ConfigError("candidate identity upstream does not match analyzed upstream")
    if provider is not None:
        if doc["provider"] != provider.key:
            raise ConfigError("candidate identity provider does not match analyzed provider")
        if doc["upstream_repo"] != provider.upstream_repo:
            raise ConfigError("candidate identity upstream does not match provider config")
    return doc


def branch_name(identity):
    """Return the runtime-owned ref-safe locator for a complete stable-aware identity."""
    doc = _identity_dict(identity)
    locator = doc["locator"]
    if (not isinstance(locator, str) or not locator or locator.startswith("-")
            or any(c not in "abcdefghijklmnopqrstuvwxyz0123456789-/." for c in locator)
            or ".." in locator or "//" in locator):
        raise ConfigError("candidate identity locator is not a safe git ref")
    return locator


BLOCKED_TITLE = "Skills update blocked: %s -> %s"
DISCOVERY_TITLE = "Skills discovery required: %s -> %s"


def _checked_key(provider_key):
    if not valid_provider_key(provider_key):
        raise ConfigError("provider key %r is not safe for an issue title" % (provider_key,))
    return provider_key


def blocked_title_prefix(provider_key):
    """Title prefix identifying every blocked issue for one provider, across target commits.

    Used to supersede stale issues: the SHA in a title is what makes deduplication correct for
    a single upstream head, and is also what would otherwise accumulate one open issue per head.
    """
    return BLOCKED_TITLE % (_checked_key(provider_key), "")


def discovery_title_prefix(provider_key):
    """Title prefix identifying every discovery issue for one provider."""
    return DISCOVERY_TITLE % (_checked_key(provider_key), "")


def issue_title(provider_key, target_sha):
    """Deduplication key for a blocked update, used verbatim as the issue title."""
    validate_sha(target_sha, "target")
    return BLOCKED_TITLE % (_checked_key(provider_key), target_sha[:8])


def discovery_title(provider_key, target_sha):
    """Deduplication key for a `DISCOVERY_REQUIRED` issue.

    Deliberately a different title from `issue_title`: a discovery does not block the provider,
    and sharing a title would let one condition's resolution silently close the other.
    """
    validate_sha(target_sha, "target")
    return DISCOVERY_TITLE % (_checked_key(provider_key), target_sha[:8])


def candidate_block(analysis, identity):
    """The `automated_candidate` marker embedded in a prepared manifest.

    `state` is asserted through `states.assert_automation_may_set()` rather than written as a
    literal, so the one state automation is allowed to create is enforced by the state machine
    instead of by whatever string happens to be typed here. Any later-stage claim raises rather
    than shipping.

    Contains no timestamp. That is deliberate: a re-run over identical inputs must produce a
    byte-identical manifest, so that "nothing changed" is provable by comparing bytes rather
    than by diffing around a moving clock. Run identity lives in the PR body and the job
    summary, which are allowed to differ between runs.
    """
    state = states.assert_automation_may_set(states.PREPARED_AUDIT_REQUIRED)
    block = {
        "state": state,
        "prepared_by": "scripts/skill_updates (stable-native artifact preparer)",
        "artifact_only": True,
        "publication_authority": "UNCOMMISSIONED",
        "candidate_identity": _identity_dict(identity, analysis=analysis),
        "superseded_commit": analysis.pinned_sha,
        "target_commit": analysis.target_sha,
        "upstream_ref": analysis.upstream_ref,
        "reviewed_fields_refer_to": analysis.pinned_sha,
        "audit_state_reachable_by_g1_1": False,
        "clears_when": (
            "a separate owner-authorized audit stage consumes this immutable artifact. G1.1 "
            "cannot clear this marker, publish it, or grant review/promotion authority. "
            "scripts/validate-agent-governance.py fails while this block is present."
        ),
    }
    if analysis.ancestry is not None:
        block["ancestry"] = analysis.ancestry.relation
    if analysis.eval_required:
        block["eval_required"] = list(analysis.eval_required)
        block["eval_note"] = (
            "Provenance checks proved WHICH bytes changed and that each was reviewed at the "
            "superseded commit. They do NOT establish that these skills still behave the same "
            "way. Run the old-vs-candidate comparison in a fresh Claude session before clearing "
            "this block; see docs/agents/skills-update-automation.md.")
    return block


def build_manifest(analysis, identity):
    """Return the candidate manifest document, with the unaudited marker inserted.

    The marker is placed immediately after `upstream_commit` so it is visible in the first
    screenful of the diff rather than buried at the end of a large file.
    """
    doc = analysis.new_manifest
    if doc is None:
        raise ConfigError("provider %s has no prepared manifest" % analysis.key)
    out = {}
    for key, value in doc.items():
        out[key] = value
        if key == "upstream_commit":
            out["automated_candidate"] = candidate_block(analysis, identity)
    if "automated_candidate" not in out:
        out["automated_candidate"] = candidate_block(analysis, identity)
    return out


def write(analysis, provider, repo_root, identity, dry_run=False):
    """Lay the candidate down under an explicit artifact root.

    Returns the sorted list of repo-relative paths written (or that *would* be written, when
    `dry_run`). The production CLI proves this root is outside the tracked checkout before
    calling here. Writes are refused unless the analysis is both drifted and unblocked, so
    there is no code path in which a partially-classified provider reaches the filesystem.
    """
    if analysis.is_blocked:
        raise ConfigError("refusing to write a blocked candidate for %s" % analysis.key)
    if not analysis.drifted:
        return []
    _identity_dict(identity, analysis=analysis, provider=provider)

    written = []
    for change in analysis.changed_files:
        if change.content is None:
            continue
        written.append(change.path)
        if dry_run:
            continue
        target = _contained_path(repo_root, change.path)
        _ensure_artifact_parents(repo_root, target)
        # Vendored files are content an agent reads, never a binary it invokes: 0o644, always,
        # regardless of what upstream shipped. A candidate that silently carried an executable
        # bit through would defeat check_provider_vendored_modes in the governance validator.
        write_new_artifact(target, change.content, binary=True)

    manifest_rel = provider.manifest_relpath
    written.append(manifest_rel)
    if not dry_run:
        manifest_target = _contained_path(repo_root, manifest_rel)
        _ensure_artifact_parents(repo_root, manifest_target)
        write_new_artifact(manifest_target, M.dump(build_manifest(analysis, identity)))
    return sorted(written)


#: Directories a candidate may write inside its outside-checkout artifact root. Everything the
#: preparer legitimately produces is a vendored skill file or a provider manifest; nothing else
#: is ever a correct write target.
WRITABLE_PREFIXES = (os.path.join(".claude", "skills") + os.sep,
                     os.path.join("docs", "agents") + os.sep)


def _contained_path(repo_root, relpath):
    """Resolve `relpath` under an artifact root, refusing escapes or out-of-scope paths.

    `os.path.join` silently discards `repo_root` when handed an absolute path, and does nothing
    about `..`, so a manifest entry could otherwise name any file on the runner. The paths in
    every shipped manifest are already under
    `.claude/skills/`, so this is defence in depth rather than a live hole; it is here because
    the manifest is exactly the kind of data an attacker would target if they got that far, and
    a write outside the artifact tree should be impossible rather than merely unlikely.
    """
    root = os.path.realpath(repo_root)
    target = os.path.realpath(os.path.join(root, relpath))
    if os.path.isabs(relpath) or ".." in relpath.replace("\\", "/").split("/"):
        raise ConfigError("refusing to write a non-relative candidate path: %r" % (relpath,))
    if target != root and not target.startswith(root + os.sep):
        raise ConfigError("refusing to write outside the artifact root: %r" % (relpath,))
    normalized = os.path.normpath(relpath)
    if not normalized.startswith(WRITABLE_PREFIXES):
        raise ConfigError(
            "refusing to write outside the vendored-skill and manifest directories: %r"
            % (relpath,))
    return target
