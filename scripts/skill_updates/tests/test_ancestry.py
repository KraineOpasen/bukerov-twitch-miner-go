"""Tests for ancestry classification, driven against real local git repositories.

Force-pushes and rebases are the failure modes a tree-content comparison cannot see: upstream can
replace the reviewed history with different commits whose files look plausible, and every hash
check downstream would still pass because it only ever compares the NEW tree against the NEW
manifest. Classifying the relation between the pinned commit and the target is the only thing
that catches it, so every branch of it is exercised here on genuine git history.
"""

import os
import subprocess
import tempfile
import unittest

from .. import analyze, ancestry
from ..gitio import UpstreamRepo
from . import fixtures


def git(args, cwd):
    proc = subprocess.run(["git"] + args, cwd=cwd, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, text=True)
    if proc.returncode != 0:
        raise AssertionError("git %s: %s" % (" ".join(args[:2]), proc.stderr))
    return proc.stdout.strip()


class AncestryCase(unittest.TestCase):
    def repo(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        path = os.path.join(tmp.name, "up")
        os.makedirs(path)
        git(["init", "--quiet", "-b", "main", "."], path)
        git(["config", "user.email", "f@example.invalid"], path)
        git(["config", "user.name", "F"], path)
        return path

    def commit(self, path, name, content):
        with open(os.path.join(path, name), "w", encoding="utf-8") as handle:
            handle.write(content)
        git(["add", "-A", "."], path)
        git(["commit", "--quiet", "-m", name], path)
        return git(["rev-parse", "HEAD"], path)

    def up(self, path):
        return UpstreamRepo(path, fixtures.FIXTURE_URL)


class TestRelations(AncestryCase):
    def test_equal(self):
        path = self.repo()
        sha = self.commit(path, "a", "1")
        result = ancestry.classify(self.up(path), sha, sha)
        self.assertEqual(result.relation, ancestry.EQUAL)
        self.assertFalse(result.drifted)
        self.assertEqual(result.merge_base, sha)
        # EQUAL is not "advanceable": there is nothing to advance to. Only FAST_FORWARD is,
        # and a no-drift provider must never enter the prepare path.
        self.assertFalse(result.advanceable)

    def test_fast_forward(self):
        path = self.repo()
        first = self.commit(path, "a", "1")
        second = self.commit(path, "b", "2")
        result = ancestry.classify(self.up(path), first, second)
        self.assertEqual(result.relation, ancestry.FAST_FORWARD)
        self.assertTrue(result.advanceable)
        self.assertEqual(result.merge_base, first)

    def test_diverged(self):
        path = self.repo()
        base = self.commit(path, "a", "1")
        ours = self.commit(path, "b", "2")
        git(["checkout", "--quiet", "-b", "other", base], path)
        theirs = self.commit(path, "c", "3")
        result = ancestry.classify(self.up(path), ours, theirs)
        self.assertEqual(result.relation, ancestry.DIVERGED)
        self.assertFalse(result.advanceable)
        self.assertEqual(result.merge_base, base)

    def test_ref_moved_backwards_is_diverged_not_fast_forward(self):
        """A ref reset to an earlier commit must never look like an ordinary advance."""
        path = self.repo()
        first = self.commit(path, "a", "1")
        second = self.commit(path, "b", "2")
        result = ancestry.classify(self.up(path), second, first)
        self.assertEqual(result.relation, ancestry.DIVERGED)
        self.assertIn("BACKWARDS", result.detail)

    def test_rewritten_history_has_no_common_ancestor(self):
        """The force-push case: history REPLACED, not extended."""
        path = self.repo()
        original = self.commit(path, "a", "1")
        git(["checkout", "--quiet", "--orphan", "rewritten"], path)
        git(["rm", "-rf", "-q", "--cached", "."], path)
        replacement = self.commit(path, "a", "1-but-from-nowhere")
        result = ancestry.classify(self.up(path), original, replacement)
        self.assertEqual(result.relation, ancestry.REWRITTEN)
        self.assertFalse(result.advanceable)
        self.assertIsNone(result.merge_base)
        self.assertIn("supply-chain", ancestry.BLOCKING_REASON[ancestry.REWRITTEN])

    def test_unreachable_when_the_pinned_commit_is_gone(self):
        path = self.repo()
        present = self.commit(path, "a", "1")
        result = ancestry.classify(self.up(path), "0" * 40, present)
        self.assertEqual(result.relation, ancestry.UNREACHABLE)
        self.assertFalse(result.advanceable)

    def test_only_fast_forward_is_advanceable(self):
        self.assertEqual(ancestry.ADVANCEABLE, frozenset({ancestry.FAST_FORWARD}))
        for relation in (ancestry.DIVERGED, ancestry.REWRITTEN, ancestry.UNREACHABLE):
            self.assertIn(relation, ancestry.BLOCKING_REASON)


class TestRefDrift(AncestryCase):
    def test_ref_drift_detected_when_default_branch_differs(self):
        result = ancestry.Ancestry(ancestry.FAST_FORWARD, "a" * 40, "b" * 40,
                                   configured_ref="master", default_ref="main")
        self.assertTrue(result.ref_drifted)

    def test_no_ref_drift_when_they_match(self):
        result = ancestry.Ancestry(ancestry.FAST_FORWARD, "a" * 40, "b" * 40,
                                   configured_ref="main", default_ref="main")
        self.assertFalse(result.ref_drifted)

    def test_unknown_default_branch_never_reports_drift(self):
        """Best-effort: a remote that will not report HEAD must not produce a spurious block."""
        result = ancestry.Ancestry(ancestry.FAST_FORWARD, "a" * 40, "b" * 40,
                                   configured_ref="main", default_ref=None)
        self.assertFalse(result.ref_drifted)


class TestAncestryBlocksTheAnalyzer(unittest.TestCase):
    """The classifier must actually refuse a non-fast-forward, not merely label it."""

    def scenario(self, **kw):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        return fixtures.build(tmp.name, **kw)

    def test_diverged_history_blocks_and_produces_no_candidate(self):
        scenario = self.scenario(
            target_files={"SKILL.md": fixtures.SKILL_MD % {"name": "fixture-skill"} + "\nx\n"})
        # Re-point the manifest pin at a sibling commit so pinned and target diverge.
        path = scenario.upstream.path
        git(["checkout", "--quiet", "-b", "sidebranch", scenario.base_sha], path)
        with open(os.path.join(path, "unrelated.txt"), "w", encoding="utf-8") as handle:
            handle.write("side\n")
        git(["add", "-A", "."], path)
        git(["commit", "--quiet", "-m", "side"], path)
        side = git(["rev-parse", "HEAD"], path)

        import json
        with open(scenario.manifest_path, encoding="utf-8") as handle:
            doc = json.load(handle)
        doc["upstream_commit"] = side
        with open(scenario.manifest_path, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(doc, indent=2) + "\n")

        analysis = analyze.analyze_provider(scenario.provider, scenario.root, scenario.upstream,
                                            scenario.target_sha)
        self.assertIn(analyze.ANCESTRY, {r.code for r in analysis.blocked})
        self.assertIsNone(analysis.new_manifest)
        self.assertEqual(analysis.ancestry.relation, ancestry.DIVERGED)

    def test_ref_drift_blocks(self):
        scenario = self.scenario(
            target_files={"SKILL.md": fixtures.SKILL_MD % {"name": "fixture-skill"} + "\nx\n"})
        analysis = analyze.analyze_provider(
            scenario.provider, scenario.root, scenario.upstream, scenario.target_sha,
            default_ref="some-other-default")
        self.assertIn(analyze.ANCESTRY, {r.code for r in analysis.blocked})
        self.assertIsNone(analysis.new_manifest)

    def test_matching_default_ref_does_not_block(self):
        scenario = self.scenario(
            target_files={"SKILL.md": fixtures.SKILL_MD % {"name": "fixture-skill"} + "\nx\n"})
        analysis = analyze.analyze_provider(
            scenario.provider, scenario.root, scenario.upstream, scenario.target_sha,
            default_ref="main")
        self.assertFalse(analysis.is_blocked, analysis.blocked)


class TestFullShaComparison(unittest.TestCase):
    def test_drift_compares_full_shas_not_prefixes(self):
        """Two distinct commits sharing a 12-hex prefix must still read as drift."""
        item = analyze.Analysis("p", "https://github.com/a/b", "main",
                                "abcdef012345" + "0" * 28, "abcdef012345" + "1" * 28)
        self.assertTrue(item.drifted)


if __name__ == "__main__":
    unittest.main()
