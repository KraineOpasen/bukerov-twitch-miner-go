"""Input-hardening tests: URL/ref/SHA allowlists, branch-name safety, and output injection.

These cover the boundaries where an attacker-controlled or merely malformed string could change
what the bot *executes* or *emits*, as opposed to what it reports. Every one of them asserts a
refusal, because the design choice throughout is to fail closed rather than sanitize: a value
outside the allowlist means an assumption broke, and quietly escaping it would hide that.
"""

import json
import os
import tempfile
import unittest

from .. import candidate, config, gitio, plan, report
from ..errors import ConfigError, GitError


class TestUpstreamUrlAllowlist(unittest.TestCase):
    HOSTILE = [
        "ext::sh -c whoami",
        "file:///etc/passwd",
        "ssh://git@github.com/a/b",
        "git://github.com/a/b",
        "https://evil.example.com/a/b",
        "https://github.com/a/b/../../c",
        "--upload-pack=/bin/sh",
        "-u/bin/sh",
        "https://github.com/a/b extra",
        "https://github.com/a/b;whoami",
        "https://github.com/a/b\nhttps://github.com/c/d",
        "https://user:pass@github.com/a/b",
        "",
        None,
        "https://github.com/a",
        "https://github.com/a/b/c",
    ]

    def test_hostile_urls_are_refused(self):
        for url in self.HOSTILE:
            with self.assertRaises(GitError, msg=repr(url)):
                gitio.validate_url(url)

    def test_legitimate_urls_are_accepted(self):
        for url in ("https://github.com/mattpocock/skills",
                    "https://github.com/EveryInc/compound-engineering-plugin",
                    "https://github.com/github/awesome-copilot"):
            self.assertEqual(gitio.validate_url(url), url)


class TestRefAndShaValidation(unittest.TestCase):
    def test_hostile_refs_are_refused(self):
        for ref in ("--upload-pack=x", "a/../b", "-x", "main;rm -rf /", "main\nmore",
                    "", None, "main space", "$(whoami)", "`id`", "ma..in"):
            with self.assertRaises(GitError, msg=repr(ref)):
                gitio.validate_ref(ref)

    def test_plain_branch_names_accepted(self):
        for ref in ("main", "master", "release/2.0", "v1.x", "a-b_c.d"):
            self.assertEqual(gitio.validate_ref(ref), ref)

    def test_sha_must_be_full_40_hex(self):
        for bad in ("068b6e0c", "0" * 39, "0" * 41, "g" * 40, "0" * 40 + " ", None,
                    "068b6e0c62393147daf03530149cdce209c93da8 ", "HEAD", "main"):
            with self.assertRaises(GitError, msg=repr(bad)):
                gitio.validate_sha(bad)
        self.assertTrue(gitio.validate_sha("0" * 40))


class TestBranchNameSafety(unittest.TestCase):
    def test_branch_name_is_derived_only_from_allowlisted_parts(self):
        name = candidate.branch_name("trailofbits", "a" * 40)
        self.assertEqual(name, "automated/skills-update/trailofbits-" + "a" * 12)

    def test_hostile_provider_keys_are_refused(self):
        for key in ("a;rm -rf /", "a b", "../x", "-x", "$(id)", "`id`", "a\nb", "", "a/b",
                    "a|b", "a&b", "a>b", "a'b", 'a"b'):
            with self.assertRaises(ConfigError, msg=repr(key)):
                candidate.branch_name(key, "a" * 40)

    def test_hostile_sha_is_refused(self):
        with self.assertRaises(GitError):
            candidate.branch_name("ok", "--force")

    def test_issue_title_is_stable_and_bounded(self):
        title = candidate.issue_title("builderio", "b" * 40)
        self.assertEqual(title, "Skills update blocked: builderio -> " + "b" * 8)
        self.assertEqual(title, candidate.issue_title("builderio", "b" * 40))


class TestProviderConfigValidation(unittest.TestCase):
    def write(self, tmp, doc):
        path = os.path.join(tmp, "providers.json")
        with open(path, "w", encoding="utf-8") as handle:
            json.dump(doc, handle)
        return path

    def base_entry(self, **over):
        entry = {"key": "p", "upstream_repo": "https://github.com/a/b", "upstream_ref": "main",
                 "manifest": "docs/agents/p-skills-manifest.json",
                 "policy": "docs/agents/p-skills-policy.md",
                 "patches": "docs/agents/p-skills-patches.md"}
        entry.update(over)
        return entry

    def load(self, tmp, entry):
        path = self.write(tmp, {"schema_version": 1, "providers": [entry]})
        return config.load(tmp, path)

    def test_ref_may_not_be_a_sha(self):
        """A SHA as the 'reviewed ref' would freeze drift detection permanently."""
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(ConfigError):
                self.load(tmp, self.base_entry(upstream_ref="0" * 40))

    def test_unknown_keys_are_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(ConfigError):
                self.load(tmp, self.base_entry(monitorr_only=True))

    def test_manifest_must_live_under_docs_agents(self):
        with tempfile.TemporaryDirectory() as tmp:
            for bad in ("/etc/passwd", "../../etc/passwd", "docs/agents/../../x"):
                with self.assertRaises(ConfigError):
                    self.load(tmp, self.base_entry(manifest=bad))

    def test_schema_version_is_pinned(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = self.write(tmp, {"schema_version": 2, "providers": [self.base_entry()]})
            with self.assertRaises(ConfigError):
                config.load(tmp, path)

    def test_duplicate_keys_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = self.write(tmp, {"schema_version": 1,
                                    "providers": [self.base_entry(), self.base_entry()]})
            with self.assertRaises(ConfigError):
                config.load(tmp, path)

    def test_real_repo_config_loads_and_matches_its_manifests(self):
        """The shipped config must agree with the shipped manifests, cross-checked."""
        root = os.path.dirname(os.path.dirname(os.path.dirname(
            os.path.dirname(os.path.abspath(__file__)))))
        providers = config.load(root)
        self.assertEqual([p.key for p in providers][:6],
                         ["mattpocock", "anthropic", "compound-engineering", "trailofbits",
                          "awesome-copilot", "builderio"])
        monitors = [p for p in providers if p.monitor_only]
        self.assertEqual(len(monitors), 1)
        self.assertEqual(monitors[0].key, "builder-agent-skills")
        # The audit-only monitor's reviewed finding is that there is NO root licence.
        self.assertIsNone(monitors[0].watch_baseline["LICENSE"])


class TestPlanOutputSafety(unittest.TestCase):
    """`$GITHUB_OUTPUT` is an injection sink: a newline forges extra outputs."""

    def test_multiline_value_is_refused(self):
        with self.assertRaises(SystemExit):
            plan.write_outputs({"providers": "a\nb=evil"}, None)

    def test_unexpected_characters_are_refused(self):
        for bad in ("a;b", "a b", "$(id)", "a`b`", "a\rb", "a\\b"):
            with self.assertRaises(SystemExit, msg=bad):
                plan.write_outputs({"any_action": bad}, None)

    def test_normal_outputs_render_one_line_each(self):
        text = plan.write_outputs(
            {"mode": "prepare", "any_action": "true", "providers": '["a","b-c"]'}, None)
        self.assertEqual(text.strip().splitlines(),
                         ['any_action=true', 'mode=prepare', 'providers=["a","b-c"]'])

    def test_only_drifted_providers_are_scheduled(self):
        report = {"providers": [
            {"provider": "a", "drifted": False},
            {"provider": "b", "drifted": True},
            {"provider": "c", "drifted": True},
        ]}
        outputs = plan.build_outputs(report, "prepare")
        self.assertEqual(json.loads(outputs["providers"]), ["b", "c"])
        self.assertEqual(outputs["any_action"], "true")

    def test_drifted_but_unblocked_monitor_is_not_scheduled(self):
        """An audit-only monitor whose commit moved but whose licence did not is not actionable.

        Scheduling it would start a privileged job daily to do nothing.
        """
        report = {"providers": [
            {"provider": "mon", "drifted": True, "monitor_only": True, "blocked": []},
            {"provider": "vendored", "drifted": True, "monitor_only": False, "blocked": []},
        ]}
        self.assertEqual(json.loads(plan.build_outputs(report, "prepare")["providers"]),
                         ["vendored"])

    def test_blocked_monitor_is_scheduled(self):
        report = {"providers": [
            {"provider": "mon", "drifted": True, "monitor_only": True,
             "blocked": [{"code": "licence", "summary": "licence appeared"}]},
        ]}
        self.assertEqual(json.loads(plan.build_outputs(report, "prepare")["providers"]), ["mon"])

    def test_unprovable_provider_is_still_scheduled(self):
        """An unresolvable ref is BLOCKED with drifted=false; it must still reach publication.

        Gating purely on `drifted` dropped the whole UNPROVABLE class, so an unreachable or
        renamed upstream produced a green run and no issue at all.
        """
        report_doc = {"providers": [
            {"provider": "gone", "drifted": False, "monitor_only": False,
             "blocked": [{"code": "unprovable", "summary": "could not resolve ref"}]},
            {"provider": "fine", "drifted": False, "monitor_only": False, "blocked": []},
        ]}
        outputs = plan.build_outputs(report_doc, "prepare")
        self.assertEqual(json.loads(outputs["providers"]), ["gone"])
        self.assertEqual(outputs["any_action"], "true")

    def test_no_drift_schedules_nothing(self):
        outputs = plan.build_outputs({"providers": [{"provider": "a", "drifted": False}]},
                                     "prepare")
        self.assertEqual(outputs["any_action"], "false")
        self.assertEqual(json.loads(outputs["providers"]), [])

    def test_unknown_mode_refused(self):
        with self.assertRaises(SystemExit):
            plan.build_outputs({"providers": []}, "publish-everything")


class TestEntryPointArgumentContract(unittest.TestCase):
    """The exact argv shapes .github/workflows/skills-update.yml uses must parse.

    Both wrapper scripts prepend their subcommand to whatever the caller typed, so a shared
    option defined only on the top-level parser would be rejected -- argparse requires those
    BEFORE the subcommand. That failure is invisible to every unit test that calls the parser
    directly, and would have surfaced as a broken first scheduled run.
    """

    def parse(self, argv):
        from .. import cli
        return cli.build_parser().parse_args(argv)

    def test_check_wrapper_shape(self):
        args = self.parse(["check", "--provider", "all",
                           "--json-out", "/tmp/r.json", "--summary", "/tmp/s.md"])
        self.assertEqual(args.provider, "all")
        self.assertEqual(args.summary, "/tmp/s.md")
        self.assertEqual(args.json_out, "/tmp/r.json")

    def test_prepare_wrapper_shape(self):
        args = self.parse(["prepare", "--provider", "trailofbits", "--base-branch", "main",
                           "--publish", "--summary", "/tmp/s.md"])
        self.assertEqual(args.provider, "trailofbits")
        self.assertTrue(args.publish)
        self.assertEqual(args.base_branch, "main")
        self.assertEqual(args.summary, "/tmp/s.md")

    def test_shared_options_belong_to_the_subcommand_not_the_parent(self):
        """Pinning the resolved shape: after the subcommand it binds, before it is an error.

        The failure mode this guards against is not the error -- it is the *silent* one. If the
        option were defined on both parser levels, `--summary x check` would parse happily and
        then be reset to None by the subparser's own default, so a job summary would be
        requested and never written.
        """
        self.assertEqual(self.parse(["check", "--summary", "/tmp/s.md"]).summary, "/tmp/s.md")
        with self.assertRaises(SystemExit):
            self.parse(["--summary", "/tmp/s.md", "check"])

    def test_check_defaults_to_all_providers(self):
        self.assertEqual(self.parse(["check"]).provider, "all")

    def test_fail_on_blocked_available_on_both_subcommands(self):
        self.assertTrue(self.parse(["check", "--fail-on-blocked"]).fail_on_blocked)
        self.assertTrue(self.parse(["prepare", "--provider", "x",
                                    "--fail-on-blocked"]).fail_on_blocked)


class TestCandidateWriteContainment(unittest.TestCase):
    """`files[].path` comes from a manifest. A write must never escape the vendored tree."""

    def test_absolute_and_traversal_paths_are_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            for bad in ("/etc/passwd", "../../etc/passwd",
                        ".claude/skills/../../../etc/x", "/tmp/evil"):
                with self.assertRaises(ConfigError, msg=bad):
                    candidate._contained_path(tmp, bad)

    def test_paths_outside_the_vendored_directories_are_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            for bad in (".github/workflows/ci.yml", "go.mod", "internal/web/server.go",
                        "CLAUDE.md", ".claude/settings.json", ".claude/hooks/x.py"):
                with self.assertRaises(ConfigError, msg=bad):
                    candidate._contained_path(tmp, bad)

    def test_legitimate_paths_are_allowed(self):
        with tempfile.TemporaryDirectory() as tmp:
            for good in (".claude/skills/foo/SKILL.md",
                         ".claude/skills/foo/scripts/run.sh",
                         "docs/agents/foo-skills-manifest.json"):
                resolved = candidate._contained_path(tmp, good)
                self.assertTrue(resolved.startswith(os.path.realpath(tmp) + os.sep), good)

    def test_every_shipped_manifest_path_is_writable(self):
        """Positive control against the real manifests: the guard must not reject real work."""
        root = os.path.dirname(os.path.dirname(os.path.dirname(
            os.path.dirname(os.path.abspath(__file__)))))
        from .. import manifest as M
        checked = 0
        for provider in config.load(root):
            if provider.monitor_only:
                continue
            doc = M.load(provider.manifest_path)
            for _skill, entry in M.iter_file_entries(doc):
                candidate._contained_path(root, entry["path"])
                checked += 1
            candidate._contained_path(root, provider.manifest_relpath)
        self.assertGreater(checked, 500, "expected the six manifests to declare 500+ files")


class TestFenceSanitization(unittest.TestCase):
    """Upstream paths reach issue bodies. `ls-tree -z` preserves newlines in them."""

    def test_a_path_containing_a_fence_cannot_close_the_block(self):
        hostile = "x\n````\n# attacker heading\n[link](https://evil.example)"
        rendered = report._fenced([hostile])
        body = rendered.split("\n")
        self.assertEqual(body[0], report.FENCE)
        self.assertEqual(body[-1], report.FENCE)
        # Exactly two fence lines: the opener and the closer, nothing in between.
        self.assertEqual(sum(1 for line in body if line.strip() == report.FENCE), 2)

    def test_newlines_are_folded_and_backticks_neutralized(self):
        rendered = report._fenced(["a\nb", "c`d`e"])
        self.assertNotIn("\na\n", rendered)
        self.assertNotIn("`d`", rendered)

    def test_empty_details_render_placeholder(self):
        self.assertIn("(none)", report._fenced([]))


class TestGitInvocationShape(unittest.TestCase):
    def test_git_env_neutralizes_ambient_config_and_transports(self):
        env = gitio._git_env()
        self.assertEqual(env["GIT_CONFIG_GLOBAL"], os.devnull)
        self.assertEqual(env["GIT_CONFIG_SYSTEM"], os.devnull)
        self.assertEqual(env["GIT_TERMINAL_PROMPT"], "0")
        self.assertEqual(env["GIT_ALLOW_PROTOCOL"], "https")
        self.assertEqual(env["LC_ALL"], "C")

    def test_hardening_flags_disable_hooks_and_symlinks(self):
        joined = " ".join(gitio._HARDENING)
        self.assertIn("core.hooksPath=/nonexistent", joined)
        self.assertIn("core.symlinks=false", joined)
        self.assertIn("protocol.ext.allow=never", joined)

    def test_run_git_rejects_a_string_command(self):
        """argv must be a list; a string would be iterated character-by-character rather than
        silently handed to a shell, but the failure should be loud either way."""
        with self.assertRaises(GitError):
            gitio.run_git("status; whoami")


if __name__ == "__main__":
    unittest.main()
