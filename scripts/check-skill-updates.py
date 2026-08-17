#!/usr/bin/env python3
"""Report upstream drift for every vendored skill provider. Read-only.

    python3 scripts/check-skill-updates.py --all
    python3 scripts/check-skill-updates.py --provider trailofbits --json

Creates no branch, pull request, issue or comment, and never executes anything fetched from
upstream: provider repositories are read through `git cat-file` out of a bare clone that is
never checked out. See docs/agents/skills-update-automation.md.

Thin wrapper so the `check` subcommand has a stable path for the workflow and for humans; all
behaviour lives in scripts/skill_updates/cli.py.
"""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from skill_updates.cli import main  # noqa: E402  (path setup must precede the import)

if __name__ == "__main__":
    sys.exit(main(["check"] + sys.argv[1:]))
