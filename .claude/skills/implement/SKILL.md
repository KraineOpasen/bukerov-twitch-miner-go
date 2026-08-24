---
name: implement
description: "Implement a piece of work based on a spec or set of tickets."
disable-model-invocation: true
---

Implement the work described by the user in the spec or tickets.

Use /tdd where possible, at pre-agreed seams.

Run typechecking regularly, single test files regularly, and the full test suite once at the end.

Once done, use /code-review to review the work.

<!-- bukerov-local-patch: implement-commit-envelope -->
Commit only when the active task contract allows commits, and only on the task branch — never on a
protected branch (main/master/release/*). Push only under a PUBLISH_DRAFT envelope (non-force). Open a
Draft PR only when the contract authorizes it. Ready-for-review, merge, release, and deploy are
owner-gated: forbidden without a separate, direct owner command, which authorizes exactly one specific
gated action after a fresh live preflight (GOVERNANCE_V3.md §4).
<!-- /bukerov-local-patch: implement-commit-envelope -->
