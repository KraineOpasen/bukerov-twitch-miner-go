---
name: handoff
description: Compact the current conversation into a handoff document for another agent to pick up.
argument-hint: "What will the next session be used for?"
disable-model-invocation: true
---

Write a handoff document summarising the current conversation so a fresh agent can continue the work.
<!-- bukerov-local-patch: handoff-scratch-redact -->
Save it to a gitignored `.scratch/` directory inside the workspace, or the session scratchpad — not a
world-readable OS temp directory. Include the exact repo, branch, and HEAD SHA; the active task contract's
state; and the precise next step.
<!-- /bukerov-local-patch: handoff-scratch-redact -->

Include a "suggested skills" section in the document, which suggests skills that the agent should invoke.

Do not duplicate content already captured in other artifacts (specs, plans, ADRs, issues, commits, diffs). Reference them by path or URL instead.

<!-- bukerov-local-patch: handoff-scratch-redact -->
Redact any sensitive information — API keys, OAuth tokens, cookies, passwords, or personally identifiable
information — as `[REDACTED]`. When unsure whether something is sensitive, omit it rather than guess.
<!-- /bukerov-local-patch: handoff-scratch-redact -->

If the user passed arguments, treat them as a description of what the next session will focus on and tailor the doc accordingly.
