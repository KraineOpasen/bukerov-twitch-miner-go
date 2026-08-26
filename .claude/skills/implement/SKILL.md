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
Commit only when the active task contract allows commits, and only on the task branch — never on main.
Push only under a PUBLISH_DRAFT envelope. Open a Draft PR only when the contract authorizes it. Never
Ready-for-review, merge, release, or deploy — those require a separate explicit user command and are not
executed autonomously under this policy.
<!-- /bukerov-local-patch: implement-commit-envelope -->
