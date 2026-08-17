"""Wiring tests: the `check` and `prepare` orchestrations must reach the SAME verdict.

Every earlier test drove `analyze.analyze_provider` directly with an explicit `default_ref`, so
a whole class of bug was invisible: `prepare` resolved the remote default branch and bound it to
a throwaway, which made the ref-drift ANCESTRY block unreachable in the only job that writes
anything. `check` reported BLOCKED and `prepare` opened a Draft PR, from the same run, on the
same inputs.

These tests drive the CLI-level functions instead, so the wiring itself is under test.
"""

import os
import tempfile
import unittest

from .. import analyze, ancestry, cli, ghadapter, publish
from . import fixtures
from .fixtures import SKILL_MD

BASE_BODY = SKILL_MD % {"name": "fixture-skill"}


class WiringCase(unittest.TestCase):
    def scenario(self, **kw):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        return fixtures.build(tmp.name, **kw)

    def patch_resolve(self, scenario, target, default_ref):
        """Point the CLI's ref resolution and fetch at the local fixture repo.

        Both are substituted, not stubbed away: `_analyze_one` still runs its real control flow
        (resolve -> compare to pin -> fetch -> classify), which is precisely the wiring under
        test. Only the two network boundaries are replaced.
        """
        original_resolve = ancestry.resolve
        original_fetch = cli.gitio.fetch_commits
        ancestry.resolve = lambda provider: (target, default_ref, None)
        cli.gitio.fetch_commits = lambda url, shas, workdir: scenario.upstream

        def restore():
            ancestry.resolve = original_resolve
            cli.gitio.fetch_commits = original_fetch

        self.addCleanup(restore)
        # cli imports the modules, not the symbols, so patching module attributes suffices.
        self.assertIs(cli.ancestry, ancestry)


class TestCheckAndPrepareAgree(WiringCase):
    def test_ref_drift_blocks_on_the_prepare_path_too(self):
        """The regression: `check` BLOCKED but `prepare` published a candidate anyway."""
        scenario = self.scenario(target_files={"SKILL.md": BASE_BODY + "\nupstream\n"})
        self.patch_resolve(scenario, scenario.target_sha, "some-other-default")

        with tempfile.TemporaryDirectory() as workdir:
            analysis = cli._analyze_one(scenario.provider, scenario.root, workdir)

        self.assertTrue(analysis.is_blocked, "prepare path must apply the ref-drift block")
        self.assertIn(analyze.ANCESTRY, {r.code for r in analysis.blocked})
        self.assertIsNone(analysis.new_manifest, "no candidate may be produced when blocked")
        self.assertTrue(analysis.ancestry.ref_drifted)

    def test_matching_default_ref_still_prepares(self):
        """Positive control: the guard must not block the ordinary case."""
        scenario = self.scenario(target_files={"SKILL.md": BASE_BODY + "\nupstream\n"})
        self.patch_resolve(scenario, scenario.target_sha, "main")
        with tempfile.TemporaryDirectory() as workdir:
            analysis = cli._analyze_one(scenario.provider, scenario.root, workdir)
        self.assertFalse(analysis.is_blocked, analysis.blocked)
        self.assertIsNotNone(analysis.new_manifest)

    def test_default_ref_reaches_the_analysis_from_the_shared_path(self):
        scenario = self.scenario(target_files={"SKILL.md": BASE_BODY + "\nupstream\n"})
        self.patch_resolve(scenario, scenario.target_sha, "main")
        with tempfile.TemporaryDirectory() as workdir:
            analysis = cli._analyze_one(scenario.provider, scenario.root, workdir)
        self.assertEqual(analysis.ancestry.default_ref, "main",
                         "the resolved default branch must not be discarded")

    def test_target_override_is_proved_against_the_reviewed_ref(self):
        """`--target-sha` must not be a second bypass: it is checked, not trusted."""
        scenario = self.scenario(target_files={"SKILL.md": BASE_BODY + "\nupstream\n"})
        self.patch_resolve(scenario, scenario.target_sha, "main")
        with tempfile.TemporaryDirectory() as workdir:
            with self.assertRaises(cli._TargetRefused):
                cli._analyze_one(scenario.provider, scenario.root, workdir,
                                 target_override="b" * 40)

    def test_target_override_also_gets_the_ref_drift_check(self):
        """The `--target-sha` path used to skip the default-branch lookup entirely."""
        scenario = self.scenario(target_files={"SKILL.md": BASE_BODY + "\nupstream\n"})
        self.patch_resolve(scenario, scenario.target_sha, "some-other-default")
        with tempfile.TemporaryDirectory() as workdir:
            analysis = cli._analyze_one(scenario.provider, scenario.root, workdir,
                                        target_override=scenario.target_sha)
        self.assertIn(analyze.ANCESTRY, {r.code for r in analysis.blocked})

    def test_one_classification_function_serves_both_phases(self):
        """Structural: `cmd_prepare` must not re-implement classification.

        The bug was two code paths that had to agree and silently stopped agreeing. Asserting
        the single call site is what stops it coming back.
        """
        import inspect
        source = inspect.getsource(cli.cmd_prepare)
        self.assertIn("_analyze_one(", source)
        self.assertNotIn("analyze.analyze_provider(", source,
                         "cmd_prepare must delegate, not re-classify")


class TestEvalCoverage(unittest.TestCase):
    """`EVAL_REQUIRED` must not silently exempt the files that ARE a skill's instructions."""

    def scenario(self, **kw):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        return fixtures.build(tmp.name, **kw)

    def analysis_for(self, base_files, target_files):
        scenario = self.scenario(base_files=base_files, target_files=target_files)
        return analyze.analyze_provider(scenario.provider, scenario.root, scenario.upstream,
                                        scenario.target_sha)

    def test_reference_markdown_triggers_an_eval(self):
        """`references/*.md` is instruction text a skill follows, not documentation about it."""
        analysis = self.analysis_for(
            {"SKILL.md": BASE_BODY, "references/injection.md": "# rules\noriginal\n"},
            {"SKILL.md": BASE_BODY, "references/injection.md": "# rules\nrewritten\n"})
        self.assertFalse(analysis.is_blocked, analysis.blocked)
        self.assertTrue(analysis.eval_required)
        self.assertTrue(any("references/injection.md" in r for r in analysis.eval_required))

    def test_skill_root_companion_markdown_triggers_an_eval(self):
        analysis = self.analysis_for(
            {"SKILL.md": BASE_BODY, "DEEPENING.md": "# guide\nold\n"},
            {"SKILL.md": BASE_BODY, "DEEPENING.md": "# guide\nnew\n"})
        self.assertTrue(any("DEEPENING.md" in r for r in analysis.eval_required))

    def test_licence_change_alone_is_exempt_from_eval(self):
        """Licences cannot change behaviour. (They block for a different reason entirely.)"""
        self.assertTrue(analyze._eval_exempt(".claude/skills/x/LICENSE"))
        self.assertTrue(analyze._eval_exempt(".claude/skills/x/LICENSE.txt"))
        self.assertTrue(analyze._eval_exempt(".claude/skills/x/NOTICE"))

    def test_image_assets_are_exempt(self):
        for path in (".claude/skills/x/assets/mark.svg", ".claude/skills/x/a/logo.PNG"):
            self.assertTrue(analyze._eval_exempt(path), path)

    def test_nothing_else_is_exempt(self):
        for path in (".claude/skills/x/SKILL.md", ".claude/skills/x/references/a.md",
                     ".claude/skills/x/scripts/run.sh", ".claude/skills/x/templates/t.yaml",
                     ".claude/skills/x/evals/case.json", ".claude/skills/x/README.md"):
            self.assertFalse(analyze._eval_exempt(path), path)

    def test_real_manifests_have_almost_no_eval_exempt_files(self):
        """Pin the coverage against the SHIPPED manifests so the gap cannot silently reopen.

        A trigger-list rule exempted 369 of 602 declared files, 268 of them Markdown. The
        exemption rule must leave only licence notices and binary assets uncovered.
        """
        from .. import config, manifest as M
        root = os.path.dirname(os.path.dirname(os.path.dirname(
            os.path.dirname(os.path.abspath(__file__)))))
        total = exempt = 0
        for provider in config.load(root):
            if provider.monitor_only:
                continue
            doc = M.load(provider.manifest_path)
            for _skill, entry in M.iter_file_entries(doc):
                total += 1
                if analyze._eval_exempt(entry["path"]):
                    exempt += 1
        self.assertGreater(total, 500, "expected 500+ declared files across six manifests")
        covered = total - exempt
        self.assertGreater(covered / float(total), 0.85,
                           "only %d/%d declared files would trigger an eval" % (covered, total))


class TestPublishFailureStatuses(unittest.TestCase):
    def test_failed_statuses_are_declared_and_non_empty(self):
        self.assertIn("pushed", publish.FAILED_PUBLISH_STATUSES)
        self.assertIn("pushed-no-pr", publish.FAILED_PUBLISH_STATUSES)
        self.assertNotIn("duplicate", publish.FAILED_PUBLISH_STATUSES)
        self.assertNotIn("published", publish.FAILED_PUBLISH_STATUSES)

    def test_cli_treats_every_failed_status_as_failure(self):
        import inspect
        source = inspect.getsource(cli.cmd_prepare)
        self.assertIn("FAILED_PUBLISH_STATUSES", source,
                      "cmd_prepare must not hard-code a single failing status")


class TestAdapterRetry(unittest.TestCase):
    def test_transient_statuses_are_retried_and_real_answers_are_not(self):
        self.assertEqual(
            set(ghadapter.GitHubAdapter.RETRY_STATUSES), {429, 500, 502, 503, 504})
        for status in (403, 404, 422):
            self.assertNotIn(status, ghadapter.GitHubAdapter.RETRY_STATUSES,
                             "%d is a real answer, not a transient" % status)


if __name__ == "__main__":
    unittest.main()
