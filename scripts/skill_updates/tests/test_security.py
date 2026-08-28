"""Input-hardening tests: URL/ref/SHA allowlists, branch-name safety, and output injection.

These cover the boundaries where an attacker-controlled or merely malformed string could change
what the bot *executes* or *emits*, as opposed to what it reports. Every one of them asserts a
refusal, because the design choice throughout is to fail closed rather than sanitize: a value
outside the allowlist means an assumption broke, and quietly escaping it would hide that.
"""

import json
import hashlib
import os
import stat
import tempfile
import unittest
from unittest import mock

from .. import analyze, candidate, config, gitio, plan, report, runtime
from ..errors import ConfigError, GitError
from . import fixtures


def candidate_identity(provider="trailofbits", target_sha="a" * 40,
                       stable_base_sha="b" * 40, control_input_digest="e" * 64,
                       upstream_repo="https://github.com/example/fixture",
                       old_pin="d" * 40):
    return runtime.candidate_identity(
        provider=provider, stable_branch="release/0.3",
        stable_base_sha=stable_base_sha, target_sha=target_sha,
        upstream_repo=upstream_repo, old_pin=old_pin,
        control_input_digest=control_input_digest, updater_source_sha="f" * 64,
        pinned_action_digests=runtime._pinned_action_digests())


_DEFAULT_TARGET = object()


def plan_entry(provider, state="NO_DRIFT", target_sha=_DEFAULT_TARGET, *, monitor_only=None,
               drifted=None):
    """Return one production-shaped closed provider record for planner tests."""
    expected = dict(plan.EXPECTED_PROVIDERS)
    if monitor_only is None:
        monitor_only = expected.get(provider, False)
    index = [key for key, _monitor in plan.EXPECTED_PROVIDERS].index(provider)
    pinned = ("%x" % (index + 1)) * 40
    if target_sha is _DEFAULT_TARGET:
        target_sha = (None if state == "BLOCKED"
                      else pinned if state == "NO_DRIFT" else "f" * 40)
    if drifted is None:
        drifted = target_sha is not None and target_sha != pinned
    return {
        "provider": provider,
        "state": state,
        "upstream_repo": "https://github.com/example/%s" % provider,
        "upstream_ref": "main",
        "pinned_sha": pinned,
        "target_sha": target_sha,
        "monitor_only": monitor_only,
        "drifted": drifted,
        "ancestry": None,
        "blocked": ([{"code": "unprovable", "summary": "blocked", "details": []}]
                    if state == "BLOCKED" else []),
        "discoveries": [],
        "eval_required": [],
        "changed_files": [],
        "changed_file_count": 0,
        "notes": [],
    }


def plan_report(overrides=None, **identity_overrides):
    identity = {
        "subject_kind": "stable-run",
        "repository_id": 1297795646,
        "repository_full_name": "KraineOpasen/bukerov-twitch-miner-go",
        "stable_branch": "release/0.3",
        "stable_base_sha": "d" * 40,
        "selected_ref": "release/0.3",
        "live_default_branch": "release/0.3",
        "fetched_default_sha": "d" * 40,
        "control_input_digest": "e" * 64,
        "workflow_path": ".github/workflows/stable-skills-maintenance.yml",
        "g1_1_mode": "artifact-only",
        "publication_authority": "UNCOMMISSIONED",
    }
    identity.update(identity_overrides)
    records = {key: plan_entry(key) for key, _monitor in plan.EXPECTED_PROVIDERS}
    extras = []
    for entry in overrides or []:
        if entry["provider"] in records:
            records[entry["provider"]] = entry
        else:
            extras.append(entry)
    providers = [records[key] for key, _monitor in plan.EXPECTED_PROVIDERS] + extras
    plugins = []
    summary = {
        "checked": len(providers),
        "drifted": sum(1 for entry in providers if entry["drifted"]),
        "blocked": sum(1 for entry in providers if entry["state"] == "BLOCKED"),
        "prepared_audit_required": sum(
            1 for entry in providers if entry["state"] == "PREPARED_AUDIT_REQUIRED"),
        "discovery_required": sum(1 for entry in providers if entry["discoveries"]),
        "eval_required": sum(1 for entry in providers if entry["eval_required"]),
        "plugin_drifts": 0,
        "states": {
            state: sum(1 for entry in providers if entry["state"] == state)
            for state in plan.PRODUCTION_STATES
        },
    }
    return {
        "schema": "skills-update-check/2",
        "g1_1_mode": "artifact-only",
        "publication_authority": "UNCOMMISSIONED",
        "stable_identity": identity,
        "providers": providers,
        "plugins": plugins,
        "summary": summary,
    }


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
    def test_branch_name_is_the_complete_identity_locator(self):
        identity = candidate_identity()
        self.assertEqual(candidate.branch_name(identity), identity.locator)
        self.assertIn("release-0.3", identity.locator)
        self.assertIn("b" * 12, identity.locator)

    def test_legacy_target_only_branch_api_is_absent(self):
        with self.assertRaises(TypeError):
            candidate.branch_name("trailofbits", "a" * 40)

    def test_same_target_cannot_collide_across_base_or_control_input(self):
        original = candidate_identity()
        other_base = candidate_identity(stable_base_sha="d" * 40)
        other_control = candidate_identity(control_input_digest="c" * 64)
        self.assertNotEqual(candidate.branch_name(original), candidate.branch_name(other_base))
        self.assertNotEqual(candidate.branch_name(original), candidate.branch_name(other_control))

    def test_hostile_provider_keys_are_refused(self):
        for key in ("a;rm -rf /", "a b", "../x", "-x", "$(id)", "`id`", "a\nb", "", "a/b",
                    "a|b", "a&b", "a>b", "a'b", 'a"b'):
            with self.assertRaises(runtime.RuntimeEnvelopeError, msg=repr(key)):
                candidate_identity(provider=key)

    def test_hostile_sha_is_refused(self):
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            candidate_identity(provider="ok", target_sha="--force")

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
        return config.load(tmp, path, require_stable_contract=False)

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
                config.load(tmp, path, require_stable_contract=False)

    def test_duplicate_keys_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = self.write(tmp, {"schema_version": 1,
                                    "providers": [self.base_entry(), self.base_entry()]})
            with self.assertRaises(ConfigError):
                config.load(tmp, path, require_stable_contract=False)

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

    def test_stable_load_accepts_only_the_canonical_registry_path(self):
        root = os.path.dirname(os.path.dirname(os.path.dirname(
            os.path.dirname(os.path.abspath(__file__)))))
        canonical = os.path.join(root, config.CONFIG_RELPATH)
        self.assertEqual(len(config.load(root, canonical)), len(config.load(root)))
        with tempfile.TemporaryDirectory() as tmp:
            alternate = os.path.join(tmp, "providers.json")
            with open(canonical, "rb") as source, open(alternate, "wb") as target:
                target.write(source.read())
            with self.assertRaises(ConfigError):
                config.load(root, alternate)

            alias = os.path.join(tmp, "providers-link.json")
            os.symlink(canonical, alias)
            with self.assertRaises(ConfigError):
                config.load(root, alias)


class TestPlanOutputSafety(unittest.TestCase):
    """`$GITHUB_OUTPUT` is an injection sink: a newline forges extra outputs."""

    def test_multiline_value_is_refused(self):
        with self.assertRaises(SystemExit):
            plan.write_outputs({"providers": "a\nb=evil"}, None)

    def test_unexpected_characters_are_refused(self):
        for bad in ("a;b", "a b", "$(id)", "a`b`", "a\rb", "a\\b"):
            with self.assertRaises(SystemExit, msg=bad):
                plan.write_outputs({"any_prepare": bad}, None)

    def test_normal_outputs_render_one_line_each(self):
        text = plan.write_outputs(
            {"mode": "prepare", "any_prepare": "true",
             "matrix": '{"include":[{"provider":"a","target_sha":"%s"}]}'
                       % ("a" * 40)}, None)
        self.assertEqual(text.strip().splitlines(),
                         ['any_prepare=true',
                          'matrix={"include":[{"provider":"a","target_sha":"%s"}]}'
                          % ("a" * 40),
                          'mode=prepare'])

    def test_only_prepared_providers_are_scheduled_with_full_target(self):
        report = plan_report([
            plan_entry("anthropic", "BLOCKED", "b" * 40),
            plan_entry("compound-engineering", "PREPARED_AUDIT_REQUIRED", "c" * 40),
        ])
        outputs = plan.build_outputs(report, "prepare")
        self.assertEqual(json.loads(outputs["matrix"]), {"include": [
            {"provider": "compound-engineering", "target_sha": "c" * 40}]})
        self.assertEqual(outputs["any_prepare"], "true")

    def test_monitor_cannot_masquerade_as_prepared(self):
        report = plan_report([
            plan_entry("builder-agent-skills", "PREPARED_AUDIT_REQUIRED", "a" * 40),
        ])
        with self.assertRaises(SystemExit):
            plan.build_outputs(report, "prepare")

    def test_blocked_and_unprovable_are_report_only(self):
        report_doc = plan_report([
            plan_entry("mattpocock", "BLOCKED", None, drifted=False),
        ])
        outputs = plan.build_outputs(report_doc, "prepare")
        self.assertEqual(json.loads(outputs["matrix"]), {"include": []})
        self.assertEqual(outputs["any_prepare"], "false")

    def test_no_drift_schedules_nothing(self):
        outputs = plan.build_outputs(plan_report(), "prepare")
        self.assertEqual(outputs["any_prepare"], "false")
        self.assertEqual(json.loads(outputs["matrix"]), {"include": []})

    def test_no_drift_target_must_equal_the_full_pin(self):
        document = plan_report()
        document["providers"][0]["target_sha"] = "f" * 40
        # Leave both the claimed drift flag and summary at false: the planner must derive the
        # relation from full SHAs rather than accepting the internally consistent false claim.
        with self.assertRaises(SystemExit):
            plan.build_outputs(document, "prepare")

    def test_malformed_provider_pin_or_target_fails_closed(self):
        mutations = (
            ("pinned_sha", "a" * 39),
            ("pinned_sha", "A" * 40),
            ("pinned_sha", "main"),
            ("target_sha", "b" * 39),
            ("target_sha", "B" * 40),
            ("target_sha", "release/0.3"),
        )
        for field, value in mutations:
            document = plan_report()
            document["providers"][0][field] = value
            with self.assertRaises(SystemExit, msg=(field, value)):
                plan.build_outputs(document, "prepare")

    def test_blocked_non_null_target_obeys_the_same_drift_equation(self):
        entry = plan_entry("mattpocock", "BLOCKED", "f" * 40, drifted=False)
        with self.assertRaises(SystemExit):
            plan.build_outputs(plan_report([entry]), "prepare")

    def test_unknown_or_intermediate_state_fails_closed(self):
        for state in (None, "UNKNOWN", "UNAVAILABLE", "DRIFT_DETECTED", "AUDITED", "READY"):
            with self.assertRaises(SystemExit, msg=state):
                plan.build_outputs(plan_report([
                    plan_entry("mattpocock", state, "a" * 40)]), "prepare")

    def test_prepared_requires_full_lowercase_target_sha(self):
        for target in (None, "a" * 39, "A" * 40, "main"):
            with self.assertRaises(SystemExit, msg=target):
                plan.build_outputs(plan_report([
                    plan_entry("mattpocock", "PREPARED_AUDIT_REQUIRED", target)]), "prepare")

    def test_missing_or_wrong_stable_control_identity_fails_closed(self):
        entry = plan_entry("mattpocock", "PREPARED_AUDIT_REQUIRED", "a" * 40)
        with self.assertRaises(SystemExit):
            plan.build_outputs({"providers": [entry]}, "prepare")
        for override in (
                {"stable_base_sha": "f" * 40},
                {"live_default_branch": "main"},
                {"control_input_digest": "0" * 63},
                {"publication_authority": "ENABLED"}):
            with self.assertRaises(SystemExit, msg=override):
                plan.build_outputs(plan_report([entry], **override), "prepare")

    def test_unknown_mode_refused(self):
        with self.assertRaises(SystemExit):
            plan.build_outputs(plan_report(), "publish-everything")

    def test_missing_duplicate_or_extra_provider_fails_closed(self):
        missing = plan_report()
        missing["providers"].pop()
        duplicate = plan_report()
        duplicate["providers"].insert(1, dict(duplicate["providers"][0]))
        extra = plan_report()
        unexpected = dict(extra["providers"][0])
        unexpected["provider"] = "unexpected"
        extra["providers"].append(unexpected)
        for document in (missing, duplicate, extra):
            with self.assertRaises(SystemExit):
                plan.build_outputs(document, "prepare")

    def test_missing_provider_array_or_top_level_extra_fails_closed(self):
        missing = plan_report()
        del missing["providers"]
        extra = plan_report()
        extra["healthy"] = True
        for document in (missing, extra):
            with self.assertRaises(SystemExit):
                plan.build_outputs(document, "prepare")

    def test_malformed_summary_and_open_provider_record_fail_closed(self):
        bad_summary = plan_report()
        bad_summary["summary"]["checked"] -= 1
        open_record = plan_report()
        open_record["providers"][0]["later_authority"] = "READY"
        for document in (bad_summary, open_record):
            with self.assertRaises(SystemExit):
                plan.build_outputs(document, "prepare")


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
        args = self.parse(["prepare", "--provider", "trailofbits",
                           "--target-sha", "a" * 40,
                           "--artifact-root", "/tmp/stable-artifact",
                           "--summary", "/tmp/s.md"])
        self.assertEqual(args.provider, "trailofbits")
        self.assertEqual(args.target_sha, "a" * 40)
        self.assertEqual(args.artifact_root, "/tmp/stable-artifact")
        self.assertEqual(args.summary, "/tmp/s.md")

    def test_historical_publish_and_base_options_are_not_registered(self):
        for option in ("--publish", "--base-branch"):
            with self.assertRaises(SystemExit, msg=option):
                self.parse(["prepare", "--provider", "trailofbits",
                            "--target-sha", "a" * 40,
                            "--artifact-root", "/tmp/stable-artifact", option])

    def test_alternate_config_option_is_not_registered(self):
        for command in (["check"], ["prepare", "--provider", "trailofbits",
                                    "--target-sha", "a" * 40,
                                    "--artifact-root", "/tmp/stable-artifact"]):
            with self.assertRaises(SystemExit):
                self.parse(command + ["--config", "/tmp/alternate-providers.json"])

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
                                    "--target-sha", "a" * 40,
                                    "--artifact-root", "/tmp/a",
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


class TestArtifactModes(unittest.TestCase):
    """Owned artifact bytes and modes must not depend on the runner's ambient umask."""

    def emit(self, parent, name, mask, analysis, provider, identity):
        root = os.path.join(parent, name)
        previous = os.umask(mask)
        try:
            candidate.create_artifact_root(root)
            candidate.write(analysis, provider, root, identity)
            candidate.write_new_artifact(
                os.path.join(root, "candidate-report.json"),
                report.to_json([analysis], stable_identity=identity),
            )
        finally:
            os.umask(previous)
        files = {}
        directories = {}
        for current, dirnames, filenames in os.walk(root):
            rel_current = os.path.relpath(current, root)
            directories[rel_current] = stat.S_IMODE(os.stat(current).st_mode)
            for dirname in dirnames:
                path = os.path.join(current, dirname)
                directories[os.path.relpath(path, root)] = stat.S_IMODE(os.stat(path).st_mode)
            for filename in filenames:
                path = os.path.join(current, filename)
                with open(path, "rb") as handle:
                    content = handle.read()
                files[os.path.relpath(path, root)] = (
                    stat.S_IMODE(os.stat(path).st_mode), content)
        return files, directories

    def test_umask_permutations_are_byte_and_mode_identical(self):
        with tempfile.TemporaryDirectory() as tmp:
            body = fixtures.SKILL_MD % {"name": "fixture-skill"}
            scenario = fixtures.build(
                os.path.join(tmp, "scenario"),
                target_files={"SKILL.md": body + "\nchanged upstream\n"},
            )
            analysis = analyze.analyze_provider(
                scenario.provider, scenario.root, scenario.upstream, scenario.target_sha)
            identity = candidate_identity(
                provider=scenario.provider.key,
                target_sha=analysis.target_sha,
                upstream_repo=analysis.upstream_repo,
                old_pin=analysis.pinned_sha,
            )
            permissive = self.emit(
                tmp, "permissive", 0o000, analysis, scenario.provider, identity)
            restrictive = self.emit(
                tmp, "restrictive", 0o777, analysis, scenario.provider, identity)
        self.assertEqual(permissive, restrictive)
        files, directories = permissive
        self.assertEqual(set(files), {
            ".claude/skills/fixture-skill/SKILL.md",
            scenario.provider.manifest_relpath,
            "candidate-report.json",
        })
        self.assertTrue(all(mode == 0o644 for mode, _content in files.values()))
        self.assertTrue(all(mode == 0o700 for mode in directories.values()))

    def test_exclusive_artifact_creation_refuses_overwrite(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "report.json")
            candidate.write_new_artifact(path, "first\n")
            with self.assertRaises(FileExistsError):
                candidate.write_new_artifact(path, "replacement\n")
            with open(path, encoding="utf-8") as handle:
                self.assertEqual(handle.read(), "first\n")


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
    def test_frozen_donor_gitio_keeps_its_provenance_hardening(self):
        env = gitio._git_env()
        self.assertEqual(env["GIT_CONFIG_GLOBAL"], os.devnull)
        self.assertEqual(env["GIT_CONFIG_SYSTEM"], os.devnull)
        self.assertEqual(env["GIT_TERMINAL_PROMPT"], "0")
        self.assertEqual(env["GIT_ALLOW_PROTOCOL"], "https")
        self.assertEqual(env["LC_ALL"], "C")
        joined = " ".join(gitio._HARDENING)
        self.assertIn("core.hooksPath=/nonexistent", joined)
        self.assertIn("core.symlinks=false", joined)
        self.assertIn("protocol.ext.allow=never", joined)
        self.assertIn("protocol.file.allow=never", " ".join(gitio._UNTRUSTED_HARDENING))

    def test_cli_runtime_rejects_proxy_and_ca_overrides_before_donor_git(self):
        from .. import cli
        root = os.path.dirname(os.path.dirname(os.path.dirname(
            os.path.dirname(os.path.abspath(__file__)))))
        keys = (
            "http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY",
            "all_proxy", "ALL_PROXY", "no_proxy", "NO_PROXY", "GIT_SSL_CAINFO",
            "SSL_CERT_FILE", "GIT_PROXY_SSL_CAINFO", "CURL_CA_BUNDLE",
        )
        for key in keys:
            with mock.patch.dict(os.environ, {key: "attacker-route"}, clear=True), \
                    mock.patch.object(gitio.subprocess, "run") as invocation:
                with self.assertRaises(runtime.RuntimeEnvelopeError, msg=key):
                    cli.main(["check", "--repo-root", root])
                invocation.assert_not_called()

    def test_run_git_rejects_a_string_command(self):
        """argv must be a list; a string would be iterated character-by-character rather than
        silently handed to a shell, but the failure should be loud either way."""
        with self.assertRaises(GitError):
            gitio.run_git("status; whoami")

    def test_production_facade_hardens_and_restores_on_success_and_exception(self):
        from .. import cli

        original_hardening = gitio._HARDENING
        original_git_env = gitio._git_env
        with cli._production_git_facade():
            self.assertIn("credential.helper=", gitio._HARDENING)
            self.assertIn("http.sslVerify=true", gitio._HARDENING)
            self.assertIn("http.followRedirects=false", gitio._HARDENING)
            env = gitio._git_env()
            home = env["HOME"]
            self.assertEqual(0o700, stat.S_IMODE(os.stat(home).st_mode))
            self.assertEqual([], os.listdir(home))
            for key in cli._DONOR_AMBIENT_ROUTE_KEYS:
                self.assertNotIn(key, env)
        self.assertIs(original_hardening, gitio._HARDENING)
        self.assertIs(original_git_env, gitio._git_env)

        with self.assertRaisesRegex(RuntimeError, "facade-test"):
            with cli._production_git_facade():
                raise RuntimeError("facade-test")
        self.assertIs(original_hardening, gitio._HARDENING)
        self.assertIs(original_git_env, gitio._git_env)

    def test_unknown_default_ref_is_unprovable_before_donor_git(self):
        from .. import cli

        provider = config.Provider(
            {
                "key": "fixture",
                "upstream_repo": "https://github.com/fixture/skills",
                "upstream_ref": "main",
                "monitor_only": True,
                "baseline_commit": "b" * 40,
                "watch_paths": [],
                "watch_baseline": {},
            },
            os.getcwd(),
        )
        with mock.patch.object(cli.ancestry, "resolve", return_value=("a" * 40, None, None)), \
                mock.patch.object(cli.gitio, "fetch_commits") as fetch:
            result = cli._analyze_one(provider, os.getcwd(), tempfile.gettempdir())
        self.assertTrue(result.is_blocked)
        self.assertEqual([analyze.UNPROVABLE], [reason.code for reason in result.blocked])
        fetch.assert_not_called()


class TestFrozenDonorProvenance(unittest.TestCase):
    def test_copy_verbatim_entrypoint_and_gitio_hashes(self):
        root = os.path.dirname(os.path.dirname(os.path.dirname(
            os.path.dirname(os.path.abspath(__file__)))))
        expected = {
            "scripts/check-skill-updates.py":
                "0e78c4b9a6e8d1085af905f283f2caf66fbbf01cbfd5194b0c67b9cd37b96775",
            "scripts/prepare-skill-update.py":
                "61448357b97cb9dfb76da85a25592a173fecaa274a61f430685642e04db6d121",
            "scripts/skill_updates/__init__.py":
                "cc83072252fe0da43ad99cf435d2b1f5b72216016dd2f51d14dda255059e7f8b",
            "scripts/skill_updates/errors.py":
                "adbff8a4789377cd932cf38f95cb03bf8b5d7b56a2268a691bba4af4e26c347e",
            "scripts/skill_updates/gitio.py":
                "42b003ce258d81143e0bcdff510f0797a1dc5049faae2616cbff07e1efd80ccf",
            "scripts/skill_updates/ancestry.py":
                "c789802fbaee250bb05ba4cb91f7224ff6a7ff28e6ddc6995c4094e90297e3fd",
            "scripts/skill_updates/manifest.py":
                "76c79864644b2034aeb5200887b299feac53d8ecde1e38314e6a2d858f9d89e7",
            "scripts/skill_updates/merge3.py":
                "b0ec628a3751ca6eed159401b35e8051bb04932297d9308fa6ff8d051567c060",
            "scripts/skill_updates/plugins.py":
                "204057f9d1e54c2d288b432d0619363a8f853e94f7e364afc27be3641e4262ad",
            "scripts/skill_updates/analyze.py":
                "c07fbe576ef5e6d3b78067e17fd900f6d39508a5f2213b35d671bb74b323f9e5",
            "scripts/skill_updates/candidate.py":
                "2109264d490ed333af9718f10c286f693bf11f87c1f2b5422493a7f864c7e62c",
            "scripts/skill_updates/tests/__init__.py":
                "1a4d6918fabbbf81cf0615d9930aab3e3570a674f57fe660e1012b2c2abb5418",
            "scripts/skill_updates/tests/fixtures.py":
                "6f9d496ac9263e3a8ae4a338f86c7de7d85d6e70c9853dcbb4a7ce16cc00bf3b",
            "scripts/skill_updates/tests/test_merge3.py":
                "fce89f1916baaec4a0982e2848de9b05d346128eb763ae5d8819bfa35de1f1de",
            "scripts/skill_updates/tests/test_ancestry.py":
                "10118dc14648b9d4b8a1bfca6f1c1395f03bf9be4890d06019c91587486e6029",
            "scripts/skill_updates/tests/test_plugins.py":
                "cd71befb5766b5c49d1801d258bf211eee1c177881cb08fa2f83ee5aab2c774f",
        }
        for relpath, digest in expected.items():
            with open(os.path.join(root, relpath), "rb") as handle:
                self.assertEqual(hashlib.sha256(handle.read()).hexdigest(), digest, relpath)


if __name__ == "__main__":
    unittest.main()
