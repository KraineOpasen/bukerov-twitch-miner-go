"""Writing a prepared update candidate to the working tree.

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

Branch naming is derived only from values that have already passed an allowlist -- a provider
key (`[A-Za-z0-9-]`) and a 40-hex SHA -- so a branch name can never carry a shell metacharacter,
a git ref-spec character, or an option-looking leading dash, regardless of what upstream
contains.
"""

import os

from . import manifest as M
from . import states
from .config import valid_provider_key
from .errors import ConfigError
from .gitio import validate_sha

#: Namespace for every branch this bot creates. Distinct from `claude/*` (interactive agent
#: work) so a repository rule or a human scanning the branch list can tell machine-prepared
#: candidates apart at a glance.
BRANCH_PREFIX = "automated/skills-update"

#: The banner every candidate PR body must open with.
PR_BANNER = "AUTOMATED UPDATE CANDIDATE — AUDIT REQUIRED — DO NOT MERGE YET."

#: Length of the SHA fragment used in branch names and issue titles. Twelve hex characters is
#: far past the ambiguity threshold for these repositories while staying readable.
SHA_FRAGMENT = 12


def branch_name(provider_key, target_sha):
    """Deterministic branch name for one provider/target pair.

    Deterministic is the whole point: it is what makes deduplication work without any state.
    A second run against the same target computes the same name, finds the branch already
    exists, and stops -- no force-push, no duplicate PR, no bookkeeping file to get stale.
    """
    validate_sha(target_sha, "target")
    if not valid_provider_key(provider_key):
        raise ConfigError("provider key %r is not safe for a branch name" % (provider_key,))
    return "%s/%s-%s" % (BRANCH_PREFIX, provider_key, target_sha[:SHA_FRAGMENT])


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


def candidate_block(analysis):
    """The `automated_candidate` marker embedded in a prepared manifest.

    `state` is asserted through `states.assert_automation_may_set()` rather than written as a
    literal, so the one state automation is allowed to create is enforced by the state machine
    instead of by whatever string happens to be typed here. An edit that tried to write
    `AUDITED` raises rather than shipping.

    Contains no timestamp. That is deliberate: a re-run over identical inputs must produce a
    byte-identical manifest, so that "nothing changed" is provable by comparing bytes rather
    than by diffing around a moving clock. Run identity lives in the PR body and the job
    summary, which are allowed to differ between runs.
    """
    state = states.assert_automation_may_set(states.PREPARED_AUDIT_REQUIRED)
    block = {
        "state": state,
        "prepared_by": "scripts/skill_updates (automated skills update bot)",
        "superseded_commit": analysis.pinned_sha,
        "target_commit": analysis.target_sha,
        "upstream_ref": analysis.upstream_ref,
        "reviewed_fields_refer_to": analysis.pinned_sha,
        "audited_state_reachable_by_automation": False,
        "clears_when": (
            "a human or an agent under a task contract audits the diff, records a fresh "
            "reviewed_at/reviewed_by for the target commit, and deletes this block. Only that "
            "act reaches the %s state. scripts/validate-agent-governance.py fails while this "
            "block is present, so a candidate cannot pass the governance gate on automation "
            "alone." % states.AUDITED
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


def build_manifest(analysis):
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
            out["automated_candidate"] = candidate_block(analysis)
    if "automated_candidate" not in out:
        out["automated_candidate"] = candidate_block(analysis)
    return out


def write(analysis, provider, repo_root, dry_run=False):
    """Lay the candidate down in the working tree.

    Returns the sorted list of repo-relative paths written (or that *would* be written, when
    `dry_run`). Writes are refused unless the analysis is both drifted and unblocked, so there
    is no code path in which a partially-classified provider reaches the filesystem.
    """
    if analysis.is_blocked:
        raise ConfigError("refusing to write a blocked candidate for %s" % analysis.key)
    if not analysis.drifted:
        return []

    written = []
    for change in analysis.changed_files:
        if change.content is None:
            continue
        written.append(change.path)
        if dry_run:
            continue
        target = _contained_path(repo_root, change.path)
        # Vendored files are content an agent reads, never a binary it invokes: 0o644, always,
        # regardless of what upstream shipped. A candidate that silently carried an executable
        # bit through would defeat check_provider_vendored_modes in the governance validator.
        with open(target, "wb") as handle:
            handle.write(change.content)
        os.chmod(target, 0o644)

    manifest_rel = provider.manifest_relpath
    written.append(manifest_rel)
    if not dry_run:
        with open(_contained_path(repo_root, manifest_rel), "w", encoding="utf-8") as handle:
            handle.write(M.dump(build_manifest(analysis)))
    return sorted(written)


#: Directories a candidate may write into. Everything the bot legitimately produces is a vendored
#: skill file or a provider manifest; nothing else is ever a correct write target.
WRITABLE_PREFIXES = (os.path.join(".claude", "skills") + os.sep,
                     os.path.join("docs", "agents") + os.sep)


def _contained_path(repo_root, relpath):
    """Resolve `relpath` under `repo_root`, refusing anything that escapes or is out of scope.

    `os.path.join` silently discards `repo_root` when handed an absolute path, and does nothing
    about `..`, so a manifest entry could otherwise name any file on the runner -- in a job that
    holds `contents: write`. The paths in every shipped manifest are already under
    `.claude/skills/`, so this is defence in depth rather than a live hole; it is here because
    the manifest is exactly the kind of data an attacker would target if they got that far, and
    a write outside the tree should be impossible rather than merely unlikely.
    """
    root = os.path.realpath(repo_root)
    target = os.path.realpath(os.path.join(root, relpath))
    if os.path.isabs(relpath) or ".." in relpath.replace("\\", "/").split("/"):
        raise ConfigError("refusing to write a non-relative candidate path: %r" % (relpath,))
    if target != root and not target.startswith(root + os.sep):
        raise ConfigError("refusing to write outside the repository: %r" % (relpath,))
    normalized = os.path.normpath(relpath)
    if not normalized.startswith(WRITABLE_PREFIXES):
        raise ConfigError(
            "refusing to write outside the vendored-skill and manifest directories: %r"
            % (relpath,))
    return target
