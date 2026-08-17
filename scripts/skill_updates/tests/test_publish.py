"""Publication tests: deduplication, idempotence, blocked-issue handling, PR permission.

Driven entirely through `ghadapter.FakeGitHub` and real local git repositories, so the branch/
commit/push path is genuinely exercised rather than mocked away.
"""

import os
import tempfile
import unittest

from .. import analyze, candidate, ghadapter, publish, report
from ..errors import AdapterError
from . import fixtures
from .fixtures import SKILL_MD

BASE_BODY = SKILL_MD % {"name": "fixture-skill"}


class PublishCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.scenario = fixtures.build(
            self.tmp.name, target_files={"SKILL.md": BASE_BODY + "\nupstream line\n"})
        self.analysis = analyze.analyze_provider(
            self.scenario.provider, self.scenario.root, self.scenario.upstream,
            self.scenario.target_sha)
        self.assertFalse(self.analysis.is_blocked, self.analysis.blocked)
        self.gh = ghadapter.FakeGitHub()

    def blocked_scenario(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        scenario = fixtures.build(
            tmp.name,
            vendored_overrides={"SKILL.md": BASE_BODY.replace("Body line three.", "OURS.")},
            target_files={"SKILL.md": BASE_BODY.replace("Body line three.", "THEIRS.")})
        analysis = analyze.analyze_provider(scenario.provider, scenario.root,
                                            scenario.upstream, scenario.target_sha)
        self.assertTrue(analysis.is_blocked)
        return scenario, analysis


class TestDeduplication(PublishCase):
    def test_existing_branch_suppresses_everything(self):
        branch = candidate.branch_name(self.scenario.provider.key, self.analysis.target_sha)
        self.gh.branches.add(branch)
        result = publish.publish_candidate(
            self.scenario.root, self.scenario.provider, self.analysis, [], self.gh, "main")
        self.assertEqual(result["status"], "duplicate")
        self.assertEqual(self.gh.pulls, [])

    def test_existing_pull_request_suppresses_creation(self):
        branch = candidate.branch_name(self.scenario.provider.key, self.analysis.target_sha)
        self.gh.pulls.append({"number": 7, "head": branch, "state": "open"})
        result = publish.publish_candidate(
            self.scenario.root, self.scenario.provider, self.analysis, [], self.gh, "main")
        self.assertEqual(result["status"], "duplicate")
        self.assertEqual(result["pull_request"], 7)
        self.assertEqual(len(self.gh.pulls), 1)

    def test_closed_pull_request_still_suppresses(self):
        """A candidate a human closed unmerged must not be reopened nightly."""
        branch = candidate.branch_name(self.scenario.provider.key, self.analysis.target_sha)
        self.gh.pulls.append({"number": 9, "head": branch, "state": "closed"})
        result = publish.publish_candidate(
            self.scenario.root, self.scenario.provider, self.analysis, [], self.gh, "main")
        self.assertEqual(result["status"], "duplicate")
        self.assertEqual(len(self.gh.pulls), 1)

    def test_branch_name_is_stable_across_runs(self):
        first = candidate.branch_name("p", "c" * 40)
        second = candidate.branch_name("p", "c" * 40)
        self.assertEqual(first, second)

    def test_different_targets_get_different_branches(self):
        self.assertNotEqual(candidate.branch_name("p", "a" * 40),
                            candidate.branch_name("p", "b" * 40))

    def test_no_force_flag_reaches_any_git_invocation(self):
        """Structural guarantee, asserted rather than trusted to review.

        Checks the module's real string *literals* via the AST rather than grepping the file:
        the prose that explains this rule necessarily names the flags it forbids, and a grep
        cannot tell an explanation from an argument.
        """
        import ast
        here = os.path.dirname(os.path.abspath(__file__))
        path = os.path.join(os.path.dirname(here), "publish.py")
        with open(path, encoding="utf-8") as handle:
            tree = ast.parse(handle.read())
        docstrings = set()
        for node in ast.walk(tree):
            if isinstance(node, (ast.Module, ast.FunctionDef, ast.ClassDef)):
                doc = ast.get_docstring(node, clean=False)
                if doc:
                    docstrings.add(doc)
        literals = [n.value for n in ast.walk(tree)
                    if isinstance(n, ast.Constant) and isinstance(n.value, str)
                    and n.value not in docstrings]
        for forbidden in ("--force", "--force-with-lease", "+refs/", "--delete", "--mirror",
                          "--all", "-f"):
            for literal in literals:
                self.assertNotIn(forbidden, literal,
                                 "%r appears in a publish.py string literal" % forbidden)


class TestDryRun(PublishCase):
    def test_dry_run_creates_nothing(self):
        result = publish.publish_candidate(
            self.scenario.root, self.scenario.provider, self.analysis, [], self.gh, "main",
            dry_run=True)
        self.assertEqual(result["status"], "dry-run")
        self.assertEqual(self.gh.pulls, [])
        self.assertEqual(self.gh.issues, [])

    def test_blocked_dry_run_creates_no_issue(self):
        scenario, analysis = self.blocked_scenario()
        result = publish.publish_blocked(analysis, scenario.provider, self.gh, dry_run=True)
        self.assertEqual(result["status"], "dry-run")
        self.assertEqual(self.gh.issues, [])


class TestBlockedIssues(PublishCase):
    def test_issue_created_once_then_left_alone(self):
        scenario, analysis = self.blocked_scenario()
        first = publish.publish_blocked(analysis, scenario.provider, self.gh)
        self.assertEqual(first["status"], "created")
        self.assertEqual(len(self.gh.issues), 1)

        second = publish.publish_blocked(analysis, scenario.provider, self.gh)
        self.assertEqual(second["status"], "unchanged")
        self.assertEqual(len(self.gh.issues), 1)
        self.assertNotIn(("update_issue", 1), self.gh.calls)

    def test_issue_updated_when_the_reason_changes(self):
        scenario, analysis = self.blocked_scenario()
        publish.publish_blocked(analysis, scenario.provider, self.gh)
        self.gh.issues[0]["body"] = "something else entirely"
        result = publish.publish_blocked(analysis, scenario.provider, self.gh)
        self.assertEqual(result["status"], "updated")
        self.assertEqual(len(self.gh.issues), 1)

    def test_issue_body_is_free_of_timestamps_so_it_is_comparable(self):
        scenario, analysis = self.blocked_scenario()
        bodies = {report.issue_body(analysis, scenario.provider) for _ in range(5)}
        self.assertEqual(len(bodies), 1)

    def test_issue_title_encodes_provider_and_target(self):
        scenario, analysis = self.blocked_scenario()
        result = publish.publish_blocked(analysis, scenario.provider, self.gh)
        self.assertTrue(result["issue_title"].startswith("Skills update blocked: fixture -> "))

    def test_upstream_details_cannot_break_out_of_the_issue_fence(self):
        """Blocked-reason details are upstream-controlled text, and git preserves newlines in
        paths, so a crafted path could otherwise close the fence and continue as Markdown.

        Asserts the structural property rather than the absence of one substring: the rendered
        body must contain exactly two fence lines -- the opener and the closer -- with nothing
        in between able to act as a third.
        """
        scenario, analysis = self.blocked_scenario()
        analysis.blocked = []
        analysis.block("inventory", "hostile",
                       ["````\n## FORGED HEADING\n[click](https://evil.example)\n````"])
        body = report.issue_body(analysis, scenario.provider)
        fence_lines = [ln for ln in body.splitlines() if ln.strip() == report.FENCE]
        self.assertEqual(len(fence_lines), 2, body)
        self.assertNotIn("\n## FORGED HEADING", body)
        self.assertNotIn("\n````\n## ", body)


class TestIssueSupersession(PublishCase):
    """A persistent block must not accumulate one open issue per upstream commit.

    The title embeds the target SHA -- which is what makes deduplication correct for a single
    head, and is also what would otherwise file a fresh issue every time upstream moves. Now
    that a reworded `description` blocks (it is trigger surface), an actively maintained
    provider would produce one a day.
    """

    def test_older_blocked_issues_are_closed_when_a_newer_head_blocks(self):
        scenario, analysis = self.blocked_scenario()
        first = publish.publish_blocked(analysis, scenario.provider, self.gh)
        self.assertEqual(first["status"], "created")

        analysis.target_sha = "b" * 40
        second = publish.publish_blocked(analysis, scenario.provider, self.gh)
        self.assertEqual(second["status"], "created")
        self.assertIn(first["issue"], second["superseded"])

        open_titles = [i["title"] for i in self.gh.issues if i["state"] == "open"]
        self.assertEqual(len(open_titles), 1, open_titles)
        closed = [i for i in self.gh.issues if i["state"] == "closed"]
        self.assertIn("Superseded", closed[0]["close_comment"])

    def test_the_current_head_issue_is_never_superseded_by_itself(self):
        scenario, analysis = self.blocked_scenario()
        publish.publish_blocked(analysis, scenario.provider, self.gh)
        again = publish.publish_blocked(analysis, scenario.provider, self.gh)
        self.assertEqual(again["status"], "unchanged")
        self.assertEqual(again["superseded"], [])
        self.assertEqual([i["state"] for i in self.gh.issues], ["open"])

    def test_discovery_issues_supersede_independently_of_blocked_issues(self):
        """The two conditions mean opposite things; one must never close the other."""
        scenario, analysis = self.blocked_scenario()
        publish.publish_blocked(analysis, scenario.provider, self.gh)
        analysis.discoveries.append(
            analyze.Discovery("new-skill", "skills/new-skill", scenario.provider.key))
        result = publish.publish_discovery(analysis, scenario.provider, self.gh)
        self.assertEqual(result["status"], "created")
        self.assertEqual(result["superseded"], [])
        self.assertEqual(sorted(i["state"] for i in self.gh.issues), ["open", "open"])


class TestUnpaginatedIssueLookup(PublishCase):
    def test_dedup_finds_an_issue_past_the_first_page(self):
        """>100 open issues (PRs count too) must not defeat deduplication."""
        scenario, analysis = self.blocked_scenario()
        created = publish.publish_blocked(analysis, scenario.provider, self.gh)
        for index in range(150):
            self.gh.issues.append({"number": 1000 + index, "title": "noise %d" % index,
                                   "body": "", "labels": [], "state": "open"})
        again = publish.publish_blocked(analysis, scenario.provider, self.gh)
        self.assertEqual(again["status"], "unchanged")
        self.assertEqual(again["issue"], created["issue"])


class TestPullRequestPermission(PublishCase):
    def test_403_reports_the_exact_setting_and_keeps_the_branch(self):
        self.gh.fail_pr_permission = True
        fixtures.init_work_repo(self.scenario.root)
        paths = candidate.write(self.analysis, self.scenario.provider, self.scenario.root)
        result = publish.publish_candidate(
            self.scenario.root, self.scenario.provider, self.analysis, paths, self.gh, "main")
        self.assertEqual(result["status"], "pushed")
        self.assertIn("Allow GitHub Actions to create and approve pull requests",
                      result["remedy"])
        self.assertIn("does not change repository settings", result["remedy"])

    def test_other_errors_are_not_swallowed(self):
        class Boom(ghadapter.FakeGitHub):
            def create_pull_request(self, *a, **kw):
                raise AdapterError("upstream is on fire", status=500)

        gh = Boom()
        fixtures.init_work_repo(self.scenario.root)
        paths = candidate.write(self.analysis, self.scenario.provider, self.scenario.root)
        with self.assertRaises(AdapterError):
            publish.publish_candidate(self.scenario.root, self.scenario.provider,
                                      self.analysis, paths, gh, "main")


class TestEndToEndPublish(PublishCase):
    def test_branch_is_created_committed_pushed_and_pr_opened_as_draft(self):
        bare = fixtures.init_work_repo(self.scenario.root)
        paths = candidate.write(self.analysis, self.scenario.provider, self.scenario.root)
        result = publish.publish_candidate(
            self.scenario.root, self.scenario.provider, self.analysis, paths, self.gh, "main")
        self.assertEqual(result["status"], "published")
        pull = self.gh.pulls[0]
        self.assertTrue(pull["draft"], "candidate PRs must be drafts")
        self.assertTrue(pull["body"].startswith(candidate.PR_BANNER))
        self.assertEqual(pull["base"], "main")
        self.assertTrue(pull["head"].startswith("automated/skills-update/"))
        # The branch really reached the remote, and main was not touched.
        refs = fixtures._run(["for-each-ref", "--format=%(refname)"], cwd=bare)
        self.assertIn("refs/heads/" + result["branch"], refs)
        main_before = fixtures._run(["rev-parse", "refs/heads/main"], cwd=bare).strip()
        self.assertTrue(main_before)

    def test_pr_body_states_it_is_unaudited(self):
        body = report.pr_body(self.analysis, self.scenario.provider)
        self.assertIn("AUDIT REQUIRED", body)
        self.assertIn("DO NOT MERGE YET", body)
        self.assertIn("No audit has been performed", body)
        self.assertIn("automated_candidate", body)


if __name__ == "__main__":
    unittest.main()
