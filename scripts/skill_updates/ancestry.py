"""Classifying how the target commit relates to the pinned one.

"Upstream moved" is not one situation, and treating it as one is how an automated refresh
quietly adopts a history that was rewritten underneath it. Five outcomes, and only the first two
are ordinary:

    EQUAL         the reviewed ref still resolves to the pinned commit -- no drift
    FAST_FORWARD  the pinned commit is an ancestor of the target: upstream simply advanced
    DIVERGED      both sides hold commits the other does not, but they share history
    REWRITTEN     no common ancestor at all -- the history was replaced, not extended
    UNREACHABLE   the pinned commit is no longer present upstream (force-push, deleted branch)

Only `FAST_FORWARD` may produce a candidate. `DIVERGED`, `REWRITTEN` and `UNREACHABLE` are
BLOCKED: in each case the commit this project reviewed is no longer a prefix of what upstream is
now offering, so "the diff since the reviewed state" is not a well-defined thing to audit. A
force-push that replaces reviewed history with different content of the same shape is exactly the
supply-chain event a pinned vendoring model exists to catch, and it is invisible to any check
that only compares tree contents.

Two further facts are recorded rather than assumed:

* the ref is resolved to a **full 40-hex SHA**; nothing here compares abbreviated SHAs, because
  a short-SHA comparison can collide and, worse, can silently *match* across a rewrite;
* the remote's **default branch** is read from `ls-remote --symref`, so a reviewed ref that has
  quietly stopped being the default branch is reported instead of being invisible.
"""

from . import gitio
from .errors import GitError

EQUAL = "equal"
FAST_FORWARD = "fast-forward"
DIVERGED = "diverged"
REWRITTEN = "rewritten"
UNREACHABLE = "unreachable"

#: The only relation an automated refresh may act on.
ADVANCEABLE = frozenset({FAST_FORWARD})

#: Relations that must block, with the reason a reader needs.
BLOCKING_REASON = {
    DIVERGED: ("upstream history diverged from the reviewed commit: both sides now hold commits "
               "the other does not, so there is no single reviewed-to-target diff to audit"),
    REWRITTEN: ("upstream history was REPLACED, not extended: the reviewed commit and the target "
                "share no common ancestor. Treat this as a supply-chain event, not an update"),
    UNREACHABLE: ("the reviewed commit is no longer present upstream (force-push or deleted "
                  "branch), so the state this project audited can no longer be produced"),
}


class Ancestry:
    """The resolved relationship between the pinned commit and the reviewed ref's target."""

    def __init__(self, relation, pinned, target, merge_base=None, configured_ref=None,
                 default_ref=None, detail=None):
        self.relation = relation
        self.pinned = pinned
        self.target = target
        self.merge_base = merge_base
        self.configured_ref = configured_ref
        self.default_ref = default_ref
        self.detail = detail or ""

    @property
    def drifted(self):
        return self.relation != EQUAL

    @property
    def advanceable(self):
        return self.relation in ADVANCEABLE

    @property
    def ref_drifted(self):
        """True when the reviewed ref is no longer the remote's default branch.

        Reported, not silently tolerated. It is not on its own proof of anything wrong -- a
        project may deliberately review a non-default branch -- but when the *default* moves away
        from the reviewed ref it usually means upstream reorganized, and continuing to track the
        old branch would quietly pin this project to an abandoned line of development.
        """
        return (self.default_ref is not None and self.configured_ref is not None
                and self.default_ref != self.configured_ref)

    def to_dict(self):
        return {"relation": self.relation, "pinned": self.pinned, "target": self.target,
                "merge_base": self.merge_base, "configured_ref": self.configured_ref,
                "default_ref": self.default_ref, "ref_drifted": self.ref_drifted,
                "detail": self.detail}

    def __repr__(self):
        return "<Ancestry %s %s..%s>" % (self.relation, (self.pinned or "?")[:8],
                                         (self.target or "?")[:8])


def classify(repo, pinned, target, configured_ref=None, default_ref=None):
    """Classify `pinned -> target` inside an already-fetched `UpstreamRepo`.

    `repo` must contain both commits when they differ; `fetch_commits` guarantees that or raises.
    The pinned commit being absent is itself the UNREACHABLE answer, so it is checked rather than
    assumed.
    """
    common = dict(configured_ref=configured_ref, default_ref=default_ref)
    if pinned == target:
        return Ancestry(EQUAL, pinned, target, merge_base=pinned, **common)
    if not repo.commit_exists(pinned):
        return Ancestry(UNREACHABLE, pinned, target,
                        detail="pinned commit %s is absent from the upstream object database"
                               % pinned, **common)
    if not repo.commit_exists(target):
        return Ancestry(UNREACHABLE, pinned, target,
                        detail="target commit %s is absent from the upstream object database"
                               % target, **common)
    if repo.is_ancestor(pinned, target):
        return Ancestry(FAST_FORWARD, pinned, target, merge_base=pinned, **common)
    base = repo.merge_base(pinned, target)
    if base is None:
        return Ancestry(REWRITTEN, pinned, target,
                        detail="no common ancestor between %s and %s" % (pinned, target),
                        **common)
    if repo.is_ancestor(target, pinned):
        return Ancestry(DIVERGED, pinned, target, merge_base=base,
                        detail="the reviewed ref moved BACKWARDS: target %s is an ancestor of "
                               "the pinned commit %s" % (target, pinned), **common)
    return Ancestry(DIVERGED, pinned, target, merge_base=base,
                    detail="common ancestor is %s; each side holds commits the other does not"
                           % base, **common)


def resolve(provider):
    """Resolve a provider's configured ref to a full SHA and read the remote's default branch.

    Returns ``(target_sha, default_ref, error)``. Failures are returned rather than raised so one
    unreachable upstream cannot abort a whole multi-provider run. The default branch is
    best-effort: a remote that will not report it yields None and a suppressed ref-drift check,
    never a spurious block.
    """
    try:
        target = gitio.ls_remote(provider.upstream_repo, provider.upstream_ref)
    except GitError as exc:
        return None, None, str(exc)
    try:
        default_ref = gitio.default_branch(provider.upstream_repo)
    except GitError:
        default_ref = None
    return target, default_ref, None
