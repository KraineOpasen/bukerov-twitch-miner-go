#!/usr/bin/env python3
"""Prepare an audit-required update candidate for one vendored skill provider.

    python3 scripts/prepare-skill-update.py --provider mattpocock --dry-run
    python3 scripts/prepare-skill-update.py --provider mattpocock --publish

What it produces is a *candidate*, never a reviewed pin: the regenerated manifest carries an
`automated_candidate` block, and scripts/validate-agent-governance.py fails while that block is
present. Clearing it -- reading the diff and recording a fresh reviewed_at/reviewed_by -- is the
one step automation must not perform for itself.

Refuses to write anything when any blocked condition fires (merge conflict, skill added/deleted/
renamed, inventory or closure change, licence change, new script/executable/symlink, frontmatter
authority drift, an unmappable local patch, an unprovable ref); with --publish it records a
single deduplicated issue instead. See docs/agents/skills-update-automation.md.

Thin wrapper; all behaviour lives in scripts/skill_updates/cli.py.
"""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from skill_updates.cli import main  # noqa: E402  (path setup must precede the import)

if __name__ == "__main__":
    sys.exit(main(["prepare"] + sys.argv[1:]))
