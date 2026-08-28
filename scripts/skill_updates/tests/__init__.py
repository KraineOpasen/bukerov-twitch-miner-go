"""Test suite for the skills update bot.

Run from the repository root:

    python3 -m unittest discover -t scripts -s scripts/skill_updates/tests

Every test is offline and hermetic. Upstream repositories are real local git repositories built
in a `tempfile.TemporaryDirectory`, and GitHub is `ghadapter.FakeGitHub`. Nothing here reaches
the network, reads the real `.claude/skills/` tree for anything it then mutates, or depends on
wall-clock time.
"""
