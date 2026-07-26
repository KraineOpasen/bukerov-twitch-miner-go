---
name: resolving-merge-conflicts
description: "Use when you need to resolve an in-progress git merge/rebase conflict."
disable-model-invocation: true
---
<!-- bukerov-local-patch: merge-conflicts-read-only — frontmatter: added disable-model-invocation -->

1. **See the current state** of the merge/rebase. Check git history, and the conflicting files.

2. **Find the primary sources** for each conflict. Understand deeply why each change was made, and what the original intent was. Read the commit messages, check the PRs, check original issues/tickets.

3. **Resolve each hunk.** Preserve both intents where possible. Where incompatible, pick the one matching the merge's stated goal and note the trade-off. Do **not** invent new behaviour.
<!-- bukerov-local-patch: merge-conflicts-read-only -->
Resolve rather than silently reverting, but `--abort` may be proposed to the user when appropriate.
<!-- /bukerov-local-patch: merge-conflicts-read-only -->

4. Discover the project's **automated checks** and run them — typically typecheck, then tests, then format. Fix anything the merge broke.

5. **Finish the merge/rebase.**
<!-- bukerov-local-patch: merge-conflicts-read-only -->
Present the conflict map and the resolved diff, then STOP. Do not `git add -A`; do not commit, `merge --continue`,
`rebase --continue`, or push — those require a separate explicit envelope.
<!-- /bukerov-local-patch: merge-conflicts-read-only -->
