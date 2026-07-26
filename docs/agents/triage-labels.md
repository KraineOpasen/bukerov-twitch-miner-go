# Triage Labels

The skills speak in terms of five canonical triage roles. This file maps those roles to the label strings this
repo's issue tracker (GitHub Issues on `KraineOpasen/bukerov-twitch-miner-go`) would use.

| Canonical role     | Label in our tracker | Meaning                                    |
| ------------------ | --------------------- | ------------------------------------------ |
| `needs-triage`      | `needs-triage`         | Maintainer needs to evaluate this issue    |
| `needs-info`        | `needs-info`           | Waiting on reporter for more information   |
| `ready-for-agent`   | `ready-for-agent`      | Fully specified, ready for an AFK agent    |
| `ready-for-human`   | `ready-for-human`      | Requires human implementation              |
| `wontfix`           | `wontfix`               | Will not be actioned                       |

When a skill mentions a role (e.g. "apply the AFK-ready triage label"), use the corresponding label string
from the right-hand column above.

## Important: documentation only

This file **documents** the intended label vocabulary. It does **not** create these labels on GitHub, and no
part of this governance task has created or modified any label. Applying a label to an issue/PR, or creating a
label that doesn't yet exist in the repo, is a tracker mutation and requires an explicit task contract per
`docs/agents/issue-tracker.md` and `docs/agents/task-contract.md`.
