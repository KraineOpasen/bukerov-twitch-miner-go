"""Automated update-candidate tooling for this project's vendored Claude skill providers.

This package answers two questions for each vendored provider, and nothing else:

    check    has the reviewed upstream ref moved away from the pinned commit, and if so, can a
             refresh be prepared mechanically without any judgement call?
    prepare  produce that refresh -- vendored bytes plus regenerated provenance -- as a
             candidate for human audit.

What it deliberately does NOT do is decide that an update is *safe*. Every candidate it emits
is explicitly marked unaudited (see `candidate.py`), and a candidate can only become a normal
reviewed pin when a human or an agent under a task contract clears that state and records a new
`reviewed_at`/`reviewed_by`. The whole point of vendoring these providers is that trust is
established by review at a specific SHA; automation can do the mechanical work of proposing the
next SHA, but it cannot manufacture the review.

Layering, innermost first -- each layer depends only on the ones above it:

    errors      exception types
    merge3      pure three-way text merge over bytes; no git, no filesystem
    gitio       read-only, checkout-free git access to upstream repositories
    config      the repo-owned provider registry (docs/agents/skills-update-providers.json)
    manifest    provider-manifest reading and provenance regeneration
    analyze     drift detection and the BLOCKED-condition classifier
    candidate   preparing an update candidate on disk
    report      deterministic human- and machine-readable output
    ghadapter   the only component that talks to GitHub; trivially replaceable by a fake
    cli         argument parsing and process exit codes

Everything above `ghadapter` is a pure function of (repo bytes, upstream bytes, config). That
is what makes the test suite able to drive real scenarios against temporary local git
repositories with no network and no GitHub.
"""

__all__ = [
    "analyze", "candidate", "config", "errors", "ghadapter", "gitio", "manifest", "merge3",
    "report",
]
