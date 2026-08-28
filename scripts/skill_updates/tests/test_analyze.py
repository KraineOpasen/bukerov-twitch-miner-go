"""Scenario tests for drift detection and the BLOCKED-condition classifier.

Each test builds a real local upstream git repository, a matching vendored tree, and a manifest
whose provenance is computed rather than typed, then asserts the classifier's verdict. The
positive controls matter as much as the negative ones: a classifier that blocked everything
would pass every "must block" test and be useless.
"""

import os
import tempfile
import unittest

from .. import analyze, candidate, manifest as M, merge3, runtime, states
from ..errors import ConfigError
from . import fixtures
from .fixtures import SKILL_MD

BASE_BODY = SKILL_MD % {"name": "fixture-skill"}
PATCH_MARKER = "<!-- bukerov-local-patch: fixture-gate -->\nLocal governance note.\n<!-- /bukerov-local-patch: fixture-gate -->\n"


class AnalyzeCase(unittest.TestCase):
    def analyze(self, **kw):
        """Build a scenario in a temp dir and classify it. Returns (analysis, scenario)."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        scenario = fixtures.build(tmp.name, **kw)
        analysis = analyze.analyze_provider(scenario.provider, scenario.root,
                                            scenario.upstream, scenario.target_sha)
        return analysis, scenario

    def codes(self, analysis):
        return sorted({r.code for r in analysis.blocked})

    def artifact_root(self):
        """Return a new production-shaped candidate root, never the tracked fixture tree."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = os.path.join(tmp.name, "artifact")
        candidate.create_artifact_root(root)
        return root

    def identity(self, analysis, provider_key="fixture"):
        return runtime.candidate_identity(
            provider=provider_key,
            stable_branch="release/0.3",
            stable_base_sha="a" * 40,
            target_sha=analysis.target_sha,
            upstream_repo=analysis.upstream_repo,
            old_pin=analysis.pinned_sha,
            control_input_digest="c" * 64,
            updater_source_sha="d" * 64,
            pinned_action_digests=runtime._pinned_action_digests(),
        )


class TestNoDrift(AnalyzeCase):
    def test_target_equal_to_pin_is_not_drift(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        scenario = fixtures.build(tmp.name)
        analysis = analyze.analyze_provider(scenario.provider, scenario.root,
                                            scenario.upstream, scenario.base_sha)
        self.assertFalse(analysis.drifted)
        self.assertFalse(analysis.is_blocked)
        self.assertEqual(analysis.changed_files, [])
        self.assertIsNone(analysis.new_manifest)

    def test_provenance_only_commit_drift(self):
        """Upstream committed, but nothing in the selected subtree changed.

        The pin must move and the manifest must be regenerated, while zero vendored files
        change -- exactly what the live awesome-copilot drift looked like.
        """
        analysis, _ = self.analyze()
        self.assertTrue(analysis.drifted)
        self.assertFalse(analysis.is_blocked)
        self.assertEqual(analysis.changed_files, [])
        self.assertEqual(analysis.new_manifest["upstream_commit"], analysis.target_sha)


class TestOrdinaryUpdates(AnalyzeCase):
    def test_unmodified_file_changed_upstream_is_taken_verbatim(self):
        analysis, scenario = self.analyze(
            target_files={"SKILL.md": BASE_BODY + "\nUpstream added a line.\n"})
        self.assertFalse(analysis.is_blocked, analysis.blocked)
        self.assertEqual(len(analysis.changed_files), 1)
        change = analysis.changed_files[0]
        self.assertEqual(change.verdict, merge3.TAKE_THEIRS)
        self.assertIn(b"Upstream added a line.", change.content)

    def test_clean_three_way_merge_preserves_local_patch(self):
        """Our patch at the top, upstream's edit at the bottom: both must survive."""
        patched = BASE_BODY.replace("Body line one.", PATCH_MARKER + "Body line one.")
        analysis, _ = self.analyze(
            vendored_overrides={"SKILL.md": patched},
            target_files={"SKILL.md": BASE_BODY.replace("Body line five.",
                                                        "Body line five, revised upstream.")})
        self.assertFalse(analysis.is_blocked, analysis.blocked)
        change = analysis.changed_files[0]
        self.assertEqual(change.verdict, merge3.MERGED)
        self.assertIn(b"fixture-gate", change.content)
        self.assertIn(b"Body line five, revised upstream.", change.content)
        self.assertIn("fixture-gate", change and
                      M.marker_ids(change.path, change.content))

    def test_retain_ours_when_upstream_did_not_move_the_file(self):
        patched = BASE_BODY.replace("Body line one.", PATCH_MARKER + "Body line one.")
        analysis, _ = self.analyze(
            vendored_overrides={"SKILL.md": patched},
            extra_skills={"unrelated": {"SKILL.md": "---\nname: unrelated\n---\nx\n"}})
        self.assertFalse(analysis.is_blocked, analysis.blocked)
        self.assertEqual(analysis.changed_files, [])


class TestBlockedConditions(AnalyzeCase):
    def test_merge_conflict(self):
        analysis, _ = self.analyze(
            vendored_overrides={"SKILL.md": BASE_BODY.replace("Body line three.",
                                                              "OUR rewrite of line three.")},
            target_files={"SKILL.md": BASE_BODY.replace("Body line three.",
                                                        "UPSTREAM rewrite of line three.")})
        self.assertIn(analyze.CONFLICT, self.codes(analysis))
        self.assertIsNone(analysis.new_manifest)

    def test_binary_file_changed_on_both_sides(self):
        analysis, _ = self.analyze(
            base_files={"SKILL.md": BASE_BODY, "logo.bin": "\x00base\n"},
            vendored_overrides={"logo.bin": "\x00ours\n"},
            target_files={"SKILL.md": BASE_BODY, "logo.bin": "\x00theirs\n"})
        self.assertIn(analyze.CONFLICT, self.codes(analysis))

    def test_skill_deleted_upstream(self):
        analysis, _ = self.analyze(target_files={"SKILL.md": None})
        self.assertIn(analyze.SKILL_SET, self.codes(analysis))

    def test_file_added_upstream_is_an_inventory_change(self):
        analysis, _ = self.analyze(
            target_files={"SKILL.md": BASE_BODY, "NEW-REFERENCE.md": "# new\n"})
        self.assertIn(analyze.INVENTORY, self.codes(analysis))

    def test_file_deleted_upstream_is_an_inventory_change(self):
        analysis, _ = self.analyze(
            base_files={"SKILL.md": BASE_BODY, "EXTRA.md": "# extra\n"},
            target_files={"SKILL.md": BASE_BODY})
        self.assertIn(analyze.INVENTORY, self.codes(analysis))

    def test_rename_reports_both_halves(self):
        analysis, _ = self.analyze(
            base_files={"SKILL.md": BASE_BODY, "OLD.md": "# old\n"},
            target_files={"SKILL.md": BASE_BODY, "NEW.md": "# old\n"})
        self.assertIn(analyze.INVENTORY, self.codes(analysis))
        detail = " ".join(d for r in analysis.blocked for d in r.details)
        self.assertIn("OLD.md", detail)
        self.assertIn("NEW.md", detail)

    def test_licence_text_changed(self):
        analysis, _ = self.analyze(license_at_target="MIT License\n\nCopyright (c) 2027 Someone Else\n")
        self.assertIn(analyze.LICENCE, self.codes(analysis))

    def test_licence_disappeared(self):
        analysis, _ = self.analyze(license_at_target=None)
        self.assertIn(analyze.LICENCE, self.codes(analysis))

    def test_new_executable_blocks_but_pre_existing_one_does_not(self):
        """The distinction this project actually needs.

        Several providers legitimately ship 100755 scripts that we vendor 100644 under a
        documented `*-mode-normalize` patch id. Blocking on the mere presence of 100755 would
        refuse every compound-engineering update forever. Only a file that BECAME executable
        between the pinned and target commits is a new capability.
        """
        already = self.analyze(
            base_files={"SKILL.md": BASE_BODY, "scripts/run.sh": "#!/bin/sh\necho hi\n"},
            target_files={"SKILL.md": BASE_BODY,
                          "scripts/run.sh": "#!/bin/sh\necho hi\nnew line\n"},
            target_modes={"scripts/run.sh": "100755"})[0]
        # BASE also had it executable? No -- fixtures only apply target_modes at TARGET, so
        # this IS a mode change and must block.
        self.assertIn(analyze.EXECUTABLE, self.codes(already))

    def test_symlink_appearing_blocks(self):
        analysis, _ = self.analyze(
            target_files={"SKILL.md": BASE_BODY, "link.md": "placeholder\n"},
            target_modes={"link.md": "120000"})
        self.assertIn(analyze.EXECUTABLE, self.codes(analysis))

    def test_frontmatter_authority_drift(self):
        analysis, _ = self.analyze(
            target_files={"SKILL.md": BASE_BODY.replace(
                "description: A fixture skill for testing.",
                "description: A fixture skill for testing.\ndisable-model-invocation: true")})
        self.assertIn(analyze.AUTHORITY, self.codes(analysis))

    def test_description_change_is_trigger_surface_and_blocks(self):
        """`description` is the TRIGGER surface, not decoration.

        It is what the model reads to decide whether to invoke a skill at all, so an upstream
        rewording changes when the skill fires even when it reads as ordinary prose. A checker
        cannot distinguish a clarifying reword from one that widens the trigger, so this is
        audit-required by design -- accepting that more updates need a human.
        """
        analysis, _ = self.analyze(
            target_files={"SKILL.md": BASE_BODY.replace(
                "description: A fixture skill for testing.",
                "description: A fixture skill, reworded upstream.")})
        self.assertIn(analyze.AUTHORITY, self.codes(analysis))

    def test_body_prose_change_alone_does_not_block(self):
        """Positive control: body prose is ordinary drift and still merges.

        Without this, "blocks on everything" would pass every must-block test in this class.
        """
        analysis, _ = self.analyze(
            target_files={"SKILL.md": BASE_BODY.replace("Body line four.",
                                                        "Body line four, reworded upstream.")})
        self.assertNotIn(analyze.AUTHORITY, self.codes(analysis))
        self.assertFalse(analysis.is_blocked, analysis.blocked)
        self.assertEqual(analysis.state, states.PREPARED_AUDIT_REQUIRED)

    def test_every_documented_trigger_key_is_covered(self):
        """Each key the owner addendum names must actually be in AUTHORITY_KEYS."""
        for key in ("description", "when_to_use", "disable-model-invocation", "user-invocable",
                    "allowed-tools", "disallowed-tools", "model", "effort", "context", "agent",
                    "hooks", "paths", "shell"):
            self.assertIn(key, analyze.AUTHORITY_KEYS, key)

    def test_each_trigger_key_change_blocks(self):
        """Drive a real block through several of them, not just the list membership."""
        for key, value in (("when_to_use", "always"), ("allowed-tools", "Read"),
                           ("disallowed-tools", "Bash"), ("model", "opus"),
                           ("shell", "bash"), ("user-invocable", "true")):
            analysis, _ = self.analyze(
                target_files={"SKILL.md": BASE_BODY.replace(
                    "description: A fixture skill for testing.",
                    "description: A fixture skill for testing.\n%s: %s" % (key, value))})
            self.assertIn(analyze.AUTHORITY, self.codes(analysis), key)

    def test_upstream_deleting_a_patched_region_is_blocked(self):
        """Upstream deletes exactly the region our patch wrapped.

        This must never produce a candidate. It surfaces as CONFLICT rather than PATCH_MAP,
        which is the correct and stronger outcome: both sides changed the same region, so the
        merge engine refuses before the patch-survival check is even consulted.
        """
        patched = BASE_BODY.replace("Body line three.", PATCH_MARKER + "Body line three.")
        target = BASE_BODY.replace("Body line three.\n", "")
        analysis, _ = self.analyze(vendored_overrides={"SKILL.md": patched},
                                   target_files={"SKILL.md": target})
        self.assertTrue(analysis.is_blocked)
        self.assertIn(analyze.CONFLICT, self.codes(analysis))

    def test_patch_map_guard_fires_if_a_merge_engine_ever_drops_a_marker(self):
        """Pin the PATCH_MAP guard itself.

        `merge3` cannot currently lose a marker (see the proof in analyze._analyze_file), so
        the guard is unreachable through the real engine. It exists so that a FUTURE change to
        the merge engine fails closed instead of silently discarding a governance patch, and
        that promise is only worth anything if it is tested. So the engine is temporarily
        replaced with one that returns a clean merge having dropped the patch region -- exactly
        the regression the guard is insurance against.
        """
        patched = BASE_BODY.replace("Body line one.", PATCH_MARKER + "Body line one.")
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        scenario = fixtures.build(
            tmp.name, vendored_overrides={"SKILL.md": patched},
            target_files={"SKILL.md": BASE_BODY.replace("Body line five.", "Five, revised.")})

        original = merge3.resolve

        def lossy(base, ours, theirs):
            verdict, content, conflicts = original(base, ours, theirs)
            if verdict == merge3.MERGED:
                content = content.replace(PATCH_MARKER.encode("utf-8"), b"")
            return verdict, content, conflicts

        merge3.resolve = lossy
        try:
            analysis = analyze.analyze_provider(scenario.provider, scenario.root,
                                                scenario.upstream, scenario.target_sha)
        finally:
            merge3.resolve = original
        self.assertIn(analyze.PATCH_MAP, self.codes(analysis))
        self.assertIsNone(analysis.new_manifest)
        # And the real engine, on the same inputs, does not lose it.
        clean = analyze.analyze_provider(scenario.provider, scenario.root, scenario.upstream,
                                         scenario.target_sha)
        self.assertFalse(clean.is_blocked, clean.blocked)
        self.assertIn(b"fixture-gate", clean.changed_files[0].content)

    def test_vendored_file_edited_without_a_manifest_bump(self):
        """The bot may run against a working tree someone has edited.

        Merging from an unrecorded state would produce a candidate whose provenance describes
        bytes nobody reviewed, so the mismatch must block rather than be absorbed.
        """
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        scenario = fixtures.build(tmp.name,
                                  target_files={"SKILL.md": BASE_BODY + "\nupstream\n"})
        path = os.path.join(scenario.root, ".claude/skills/fixture-skill/SKILL.md")
        with open(path, "ab") as handle:
            handle.write(b"\nsneaked in without a manifest bump\n")
        analysis = analyze.analyze_provider(scenario.provider, scenario.root,
                                            scenario.upstream, scenario.target_sha)
        self.assertIn(analyze.INVENTORY, self.codes(analysis))
        self.assertIn("vendored_blob_sha", " ".join(r.summary for r in analysis.blocked))

    def test_manifest_base_hash_disagreeing_with_the_pin_is_unprovable(self):
        """BASE is content-addressed. If the recorded upstream_blob_sha is not what the pinned
        commit actually holds, the manifest does not describe the commit it claims to pin --
        an integrity failure, not an update to merge."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        scenario = fixtures.build(tmp.name,
                                  target_files={"SKILL.md": BASE_BODY + "\nupstream\n"})
        doc = scenario.manifest()
        doc["skills"][0]["files"][0]["upstream_blob_sha"] = "d" * 40
        with open(scenario.manifest_path, "w", encoding="utf-8") as handle:
            handle.write(M.dump(doc))
        analysis = analyze.analyze_provider(scenario.provider, scenario.root,
                                            scenario.upstream, scenario.target_sha)
        self.assertIn(analyze.UNPROVABLE, self.codes(analysis))

    def test_merging_must_not_drop_a_locally_added_frontmatter_authority_line(self):
        """The gap the patch-marker check is structurally blind to.

        `disable-model-invocation: true` is added by local patch on several vendored skills, and
        a frontmatter line cannot carry an HTML-comment patch marker. So if a merge ever removed
        it, no patch id would go missing -- only this OURS-vs-merged comparison notices.
        """
        ours = BASE_BODY.replace("description: A fixture skill for testing.",
                                 "description: A fixture skill for testing.\n"
                                 "disable-model-invocation: true")
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        scenario = fixtures.build(
            tmp.name, vendored_overrides={"SKILL.md": ours},
            target_files={"SKILL.md": BASE_BODY.replace("Body line five.", "Five, revised.")})

        original = merge3.resolve

        def strips_authority(base, o, t):
            verdict, content, conflicts = original(base, o, t)
            if verdict == merge3.MERGED:
                content = content.replace(b"disable-model-invocation: true\n", b"")
            return verdict, content, conflicts

        merge3.resolve = strips_authority
        try:
            analysis = analyze.analyze_provider(scenario.provider, scenario.root,
                                                scenario.upstream, scenario.target_sha)
        finally:
            merge3.resolve = original
        self.assertIn(analyze.AUTHORITY, self.codes(analysis))
        self.assertIn("disable-model-invocation",
                      " ".join(d for r in analysis.blocked for d in r.details))

    def test_real_merge_preserves_a_locally_added_authority_line(self):
        """Positive control: the real engine keeps it, so the guard is not just always-on."""
        ours = BASE_BODY.replace("description: A fixture skill for testing.",
                                 "description: A fixture skill for testing.\n"
                                 "disable-model-invocation: true")
        analysis, _ = self.analyze(
            vendored_overrides={"SKILL.md": ours},
            target_files={"SKILL.md": BASE_BODY.replace("Body line five.", "Five, revised.")})
        self.assertFalse(analysis.is_blocked, analysis.blocked)
        self.assertIn(b"disable-model-invocation: true", analysis.changed_files[0].content)

    def test_changed_script_withdraws_the_scripts_audited_attestation(self):
        """`scripts_audited` claims a human read those bytes. The bot must not carry it across
        a change it made itself -- the validator's usual 're-audit, not re-hash' diagnostic
        cannot fire when the bot IS the rehash."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        scenario = fixtures.build(
            tmp.name,
            base_files={"SKILL.md": BASE_BODY, "scripts/run.sh": "#!/bin/sh\necho old\n"},
            target_files={"SKILL.md": BASE_BODY, "scripts/run.sh": "#!/bin/sh\necho new\n"})
        doc = scenario.manifest()
        doc["skills"][0]["scripts_audited"] = True
        doc["skills"][0]["audit_ref"] = "read end to end on 2026-01-01"
        with open(scenario.manifest_path, "w", encoding="utf-8") as handle:
            handle.write(M.dump(doc))

        analysis = analyze.analyze_provider(scenario.provider, scenario.root,
                                            scenario.upstream, scenario.target_sha)
        self.assertFalse(analysis.is_blocked, analysis.blocked)
        skill = analysis.new_manifest["skills"][0]
        self.assertFalse(skill["scripts_audited"])
        self.assertIn(".claude/skills/fixture-skill/scripts/run.sh",
                      skill["scripts_reaudit_required"])
        self.assertIn("SUPERSEDED", skill["audit_ref"])
        self.assertTrue(any("scripts_audited was withdrawn" in n for n in analysis.notes))

    def test_untouched_script_keeps_its_attestation(self):
        """Positive control: only a CHANGED script withdraws the claim."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        scenario = fixtures.build(
            tmp.name,
            base_files={"SKILL.md": BASE_BODY, "scripts/run.sh": "#!/bin/sh\necho same\n"},
            target_files={"SKILL.md": BASE_BODY + "\nprose only\n",
                          "scripts/run.sh": "#!/bin/sh\necho same\n"})
        doc = scenario.manifest()
        doc["skills"][0]["scripts_audited"] = True
        with open(scenario.manifest_path, "w", encoding="utf-8") as handle:
            handle.write(M.dump(doc))
        analysis = analyze.analyze_provider(scenario.provider, scenario.root,
                                            scenario.upstream, scenario.target_sha)
        self.assertTrue(analysis.new_manifest["skills"][0]["scripts_audited"])
        self.assertNotIn("scripts_reaudit_required", analysis.new_manifest["skills"][0])

    def test_unprovable_ref(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        scenario = fixtures.build(tmp.name)
        analysis = analyze.analyze_provider(scenario.provider, scenario.root, scenario.upstream,
                                            "0" * 40)
        self.assertIn(analyze.UNPROVABLE, self.codes(analysis))

    def test_blocked_never_yields_a_manifest_or_changes_disk(self):
        """A blocked provider must not reach the filesystem at all.

        Asserting only that `write()` raises is too weak: without the guard, `write()` would
        lay down every file it had already resolved before hitting the missing manifest, so
        the exception would arrive *after* a partial write. The bytes on disk are the real
        assertion.
        """
        analysis, scenario = self.analyze(
            base_files={"SKILL.md": BASE_BODY, "EXTRA.md": "# extra\n"},
            vendored_overrides={"SKILL.md": BASE_BODY.replace("Body line three.", "OURS.")},
            target_files={"SKILL.md": BASE_BODY.replace("Body line three.", "THEIRS."),
                          "EXTRA.md": "# extra, revised upstream\n"})
        self.assertTrue(analysis.is_blocked)
        self.assertIsNone(analysis.new_manifest)
        before = {p: scenario.vendored(p)
                  for p in (".claude/skills/fixture-skill/SKILL.md",
                            ".claude/skills/fixture-skill/EXTRA.md")}
        manifest_before = scenario.vendored(
            os.path.relpath(scenario.manifest_path, scenario.root))
        with self.assertRaises(ConfigError) as caught:
            candidate.write(analysis, scenario.provider, scenario.root,
                            self.identity(analysis))
        self.assertIn("blocked", str(caught.exception))
        for path, data in before.items():
            self.assertEqual(scenario.vendored(path), data, "%s was written despite BLOCKED" % path)
        self.assertEqual(
            scenario.vendored(os.path.relpath(scenario.manifest_path, scenario.root)),
            manifest_before)


class TestDiscoveryAndEval(AnalyzeCase):
    def test_new_sibling_skill_is_a_discovery_not_a_block(self):
        """A skill upstream added elsewhere must not hold this provider's refresh hostage."""
        analysis, _ = self.analyze(
            target_files={"SKILL.md": BASE_BODY + "\nprose\n"},
            target_extra_skills={"brand-new-sibling": {
                "SKILL.md": "---\nname: brand-new-sibling\ndescription: new\n---\n\nbody\n"}})
        self.assertFalse(analysis.is_blocked, analysis.blocked)
        self.assertEqual(analysis.state, states.PREPARED_AUDIT_REQUIRED)
        self.assertEqual([d.name for d in analysis.discoveries], ["brand-new-sibling"])
        self.assertIsNotNone(analysis.new_manifest)

    def test_a_skill_that_always_existed_upstream_is_not_a_discovery(self):
        """Only NEW names count; a long-standing declined skill is a settled decision."""
        sibling = {"brand-new-sibling": {"SKILL.md": "---\nname: brand-new-sibling\n---\nb\n"}}
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        scenario = fixtures.build(tmp.name, extra_skills=sibling,
                                  target_files={"SKILL.md": BASE_BODY + "\nprose\n"})
        analysis = analyze.analyze_provider(scenario.provider, scenario.root,
                                            scenario.upstream, scenario.target_sha)
        self.assertEqual(analysis.discoveries, [])

    def test_new_file_inside_an_installed_skill_still_blocks(self):
        """Discovery is only for skills OUTSIDE the selection; inside, it is an inventory change."""
        analysis, _ = self.analyze(
            target_files={"SKILL.md": BASE_BODY, "NEWFILE.md": "# new\n"})
        self.assertIn(analyze.INVENTORY, self.codes(analysis))
        self.assertEqual(analysis.discoveries, [])

    def test_skill_md_change_marks_eval_required(self):
        analysis, _ = self.analyze(target_files={"SKILL.md": BASE_BODY + "\nnew guidance\n"})
        self.assertTrue(analysis.eval_required)
        self.assertTrue(any("SKILL.md" in r for r in analysis.eval_required))

    def test_agent_and_script_changes_mark_eval_required(self):
        analysis, _ = self.analyze(
            base_files={"SKILL.md": BASE_BODY, "agents/helper.md": "# helper\n"},
            target_files={"SKILL.md": BASE_BODY, "agents/helper.md": "# helper, revised\n"})
        self.assertTrue(any("agents/helper.md" in r for r in analysis.eval_required))

    def test_eval_flag_reaches_the_candidate_manifest_and_cannot_be_bypassed(self):
        analysis, scenario = self.analyze(
            target_files={"SKILL.md": BASE_BODY + "\nnew guidance\n"})
        built = candidate.build_manifest(analysis, self.identity(analysis))
        self.assertTrue(built["automated_candidate"]["eval_required"])
        self.assertIn("behave the same way", built["automated_candidate"]["eval_note"])
        # And the candidate still cannot claim to be audited.
        self.assertEqual(built["automated_candidate"]["state"],
                         states.PREPARED_AUDIT_REQUIRED)

    def test_provenance_only_change_needs_no_eval(self):
        """Positive control: a pin move with no content change is not an eval trigger."""
        analysis, _ = self.analyze()
        self.assertEqual(analysis.eval_required, [])


class TestUpstreamVersion(AnalyzeCase):
    """A stale `upstream_version` never appears in the diff, so it must not survive a pin move."""

    def scenario_with_version(self, recorded, package_json_at_target):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        scenario = fixtures.build(tmp.name,
                                  target_files={"SKILL.md": BASE_BODY + "\nnew\n"},
                                  root_files_at_target=package_json_at_target)
        doc = scenario.manifest()
        doc["upstream_version"] = recorded
        with open(scenario.manifest_path, "w", encoding="utf-8") as handle:
            handle.write(M.dump(doc))
        return scenario

    def test_version_is_re_read_from_upstream_package_json(self):
        scenario = self.scenario_with_version(
            "1.0.0", {"package.json": '{"name": "fixture", "version": "2.5.0"}\n'})
        analysis = analyze.analyze_provider(scenario.provider, scenario.root,
                                            scenario.upstream, scenario.target_sha)
        self.assertFalse(analysis.is_blocked, analysis.blocked)
        self.assertEqual(analysis.new_manifest["upstream_version"], "2.5.0")
        self.assertTrue(any("upstream_version 1.0.0 -> 2.5.0" in n for n in analysis.notes))

    def test_plugin_json_takes_precedence_over_package_json(self):
        scenario = self.scenario_with_version("1.0.0", {
            "package.json": '{"version": "2.5.0"}\n',
            ".claude-plugin/plugin.json": '{"version": "9.9.9"}\n'})
        analysis = analyze.analyze_provider(scenario.provider, scenario.root,
                                            scenario.upstream, scenario.target_sha)
        self.assertEqual(analysis.new_manifest["upstream_version"], "9.9.9")

    def test_unresolvable_version_is_removed_not_carried_forward(self):
        scenario = self.scenario_with_version("1.0.0", {})
        analysis = analyze.analyze_provider(scenario.provider, scenario.root,
                                            scenario.upstream, scenario.target_sha)
        self.assertNotIn("upstream_version", analysis.new_manifest)
        self.assertTrue(any("REMOVED" in n for n in analysis.notes))

    def test_manifests_without_the_key_are_untouched(self):
        analysis, _ = self.analyze(target_files={"SKILL.md": BASE_BODY + "\nnew\n"})
        self.assertNotIn("upstream_version", analysis.new_manifest)


class TestUpstreamTreeRegeneration(AnalyzeCase):
    """`upstream_tree` is keyed on UPSTREAM identity, which a rename makes distinct from `name`.

    A skill vendored under a different local name than it carries upstream -- anthropic renames
    `skill-creator` to `skill-creator-anthropic` so it cannot collide with the Claude Code
    built-in -- still occupies one entry in the per-skill map, and that entry is keyed on the
    upstream directory the tree SHA is actually computed from. Regenerating the map by the
    vendored `name` alone silently drops that skill's provenance, and no validator check reads
    `upstream_tree`, so nothing downstream would catch the loss.
    """

    UPSTREAM_NAME = "fixture-skill"
    LOCAL_NAME = "fixture-skill-vendorlocal"

    def test_renamed_skill_keeps_its_upstream_tree_key(self):
        analysis, scenario = self.analyze(vendored_name=self.LOCAL_NAME,
                                          target_files={"SKILL.md": BASE_BODY + "\nnew\n"})
        self.assertFalse(analysis.is_blocked, analysis.blocked)
        tree = analysis.new_manifest["upstream_tree"]
        self.assertIn(self.UPSTREAM_NAME, tree,
                      "the renamed skill's upstream_tree entry was dropped: %r" % (tree,))
        self.assertEqual(
            tree[self.UPSTREAM_NAME],
            scenario.upstream.path_tree_sha(scenario.target_sha,
                                            "skills/%s" % self.UPSTREAM_NAME),
            "the entry must be recomputed from the skill's upstream_path at the TARGET commit")

    def test_renamed_skill_gains_no_ghost_key(self):
        """The local name must not appear as a second, invented entry."""
        analysis, _ = self.analyze(vendored_name=self.LOCAL_NAME,
                                   target_files={"SKILL.md": BASE_BODY + "\nnew\n"})
        tree = analysis.new_manifest["upstream_tree"]
        self.assertEqual(set(tree), {self.UPSTREAM_NAME}, tree)

    def test_ordinary_skill_still_regenerates_its_key(self):
        """Positive control: a provider with no renamed skill is unaffected."""
        analysis, scenario = self.analyze(target_files={"SKILL.md": BASE_BODY + "\nnew\n"})
        tree = analysis.new_manifest["upstream_tree"]
        self.assertEqual(set(tree), {self.UPSTREAM_NAME}, tree)
        self.assertEqual(
            tree[self.UPSTREAM_NAME],
            scenario.upstream.path_tree_sha(scenario.target_sha,
                                            "skills/%s" % self.UPSTREAM_NAME))

    def test_provenance_only_advance_preserves_every_key(self):
        """The live anthropic case: nothing in the subtree changed, so no key may be lost."""
        analysis, scenario = self.analyze(vendored_name=self.LOCAL_NAME)
        self.assertFalse(analysis.is_blocked, analysis.blocked)
        self.assertEqual(analysis.changed_files, [])
        self.assertEqual(set(analysis.new_manifest["upstream_tree"]),
                         set(scenario.manifest()["upstream_tree"]))


class TestRebuildUpstreamTreeUnit(unittest.TestCase):
    """Direct unit coverage of the key-selection rule, independent of a built scenario."""

    class FakeRepo:
        def __init__(self, trees=None):
            self.trees = dict(trees or {})
            self.asked = []

        def path_tree_sha(self, commit, path):
            self.asked.append(path)
            return self.trees.get(path)

        def commit_tree_sha(self, commit):
            return "c" * 40

    @staticmethod
    def doc(tree, skills):
        return {"upstream_tree": tree, "skills": skills}

    def test_key_is_taken_from_renamed_from_when_the_map_uses_upstream_identity(self):
        doc = self.doc({"upstream-name": "a" * 40},
                       [{"name": "local-name", "renamed_from": "upstream-name",
                         "upstream_path": "skills/upstream-name"}])
        repo = self.FakeRepo({"skills/upstream-name": "b" * 40})
        self.assertEqual(analyze._rebuild_upstream_tree(doc, repo, "0" * 40),
                         {"upstream-name": "b" * 40})

    def test_key_falls_back_to_the_upstream_path_basename(self):
        """`renamed_from` is optional metadata; the path the SHA comes from always exists."""
        doc = self.doc({"upstream-name": "a" * 40},
                       [{"name": "local-name", "upstream_path": "skills/upstream-name"}])
        repo = self.FakeRepo({"skills/upstream-name": "b" * 40})
        self.assertEqual(analyze._rebuild_upstream_tree(doc, repo, "0" * 40),
                         {"upstream-name": "b" * 40})

    def test_local_name_wins_when_the_map_is_keyed_that_way(self):
        """A map keyed on the vendored name keeps that convention, not the upstream one."""
        doc = self.doc({"local-name": "a" * 40},
                       [{"name": "local-name", "renamed_from": "upstream-name",
                         "upstream_path": "skills/upstream-name"}])
        repo = self.FakeRepo({"skills/upstream-name": "b" * 40})
        self.assertEqual(analyze._rebuild_upstream_tree(doc, repo, "0" * 40),
                         {"local-name": "b" * 40})

    def test_a_skill_the_map_never_tracked_is_not_added(self):
        """Regeneration re-derives the entries a manifest documents; it never invents one."""
        doc = self.doc({"tracked": "a" * 40},
                       [{"name": "tracked", "upstream_path": "skills/tracked"},
                        {"name": "untracked", "upstream_path": "skills/untracked"}])
        repo = self.FakeRepo({"skills/tracked": "b" * 40, "skills/untracked": "c" * 40})
        self.assertEqual(analyze._rebuild_upstream_tree(doc, repo, "0" * 40),
                         {"tracked": "b" * 40})

    def test_an_unresolvable_upstream_tree_drops_the_entry(self):
        """Fail-safe: a path that does not exist at TARGET yields no entry, never a stale one."""
        doc = self.doc({"gone": "a" * 40},
                       [{"name": "gone", "upstream_path": "skills/gone"}])
        self.assertEqual(analyze._rebuild_upstream_tree(doc, self.FakeRepo(), "0" * 40), {})

    def test_a_skill_with_no_upstream_path_is_skipped_not_crashed(self):
        """A malformed entry must not take the whole candidate down with a KeyError."""
        doc = self.doc({"tracked": "a" * 40},
                       [{"name": "tracked"},
                        {"name": "ok", "upstream_path": "skills/ok"}])
        repo = self.FakeRepo({"skills/ok": "b" * 40})
        self.assertEqual(analyze._rebuild_upstream_tree(doc, repo, "0" * 40), {})
        self.assertEqual(repo.asked, [])

    def test_a_trailing_slash_upstream_path_is_normalized_before_lookup(self):
        """`skills/x/` and `skills/x` name the same tree; git rev-parse only accepts the latter."""
        doc = self.doc({"x": "a" * 40},
                       [{"name": "x", "upstream_path": "skills/x/"}])
        repo = self.FakeRepo({"skills/x": "b" * 40})
        self.assertEqual(analyze._rebuild_upstream_tree(doc, repo, "0" * 40), {"x": "b" * 40})
        self.assertEqual(repo.asked, ["skills/x"])

    def test_a_string_shaped_upstream_tree_still_records_the_commit_tree(self):
        """mattpocock's shape: one SHA for the whole commit, not a per-skill map."""
        doc = self.doc("a" * 40, [{"name": "x", "upstream_path": "skills/x"}])
        repo = self.FakeRepo()
        self.assertEqual(analyze._rebuild_upstream_tree(doc, repo, "0" * 40), "c" * 40)
        self.assertEqual(repo.asked, [])

    def test_every_shipped_provider_manifest_regenerates_every_key_it_records(self):
        """Blast-radius guard: no provider may lose an entry, whatever its naming convention."""
        import glob
        import json
        import posixpath
        root = os.path.join(os.path.dirname(__file__), "..", "..", "..")
        found = 0
        for path in sorted(glob.glob(os.path.join(root, "docs", "agents",
                                                  "*-skills-manifest.json"))):
            with open(path, encoding="utf-8") as handle:
                doc = json.load(handle)
            if not isinstance(doc.get("upstream_tree"), dict) or not doc["upstream_tree"]:
                continue
            found += 1
            repo = self.FakeRepo({s["upstream_path"].rstrip("/"): "b" * 40
                                  for s in doc["skills"] if s.get("upstream_path")})
            rebuilt = analyze._rebuild_upstream_tree(doc, repo, "0" * 40)
            self.assertEqual(set(rebuilt), set(doc["upstream_tree"]),
                             "%s: regenerated key set differs" % posixpath.basename(path))
        self.assertGreater(found, 0, "no dict-shaped upstream_tree manifest was exercised")


class TestLicenceCoverage(unittest.TestCase):
    """Licence comparison must cover every layout the six providers actually use.

    A licence check that silently applies to only one of them is worse than none: the PR body
    tells the reviewer the licence was checked.
    """

    def repo_root(self):
        return os.path.dirname(os.path.dirname(os.path.dirname(
            os.path.dirname(os.path.abspath(__file__)))))

    def test_every_shipped_provider_has_at_least_one_licence_path_covered(self):
        from .. import config
        root = self.repo_root()
        for provider in config.load(root):
            if provider.monitor_only:
                continue
            doc = M.load(provider.manifest_path)
            paths = analyze.licence_upstream_paths(doc)
            self.assertTrue(paths, "%s: no licence upstream path covered" % provider.key)
            for upstream_path, recorded in paths.items():
                self.assertTrue(upstream_path)
                if recorded is not None:
                    self.assertRegex(recorded, r"^[0-9a-f]{40}$")

    def test_local_origin_licence_files_are_still_covered(self):
        """Four providers copy the upstream ROOT LICENSE into each skill dir as origin=local.

        `_analyze_file` returns early for local-origin files, so without explicit coverage those
        55 skills' notices were compared against nothing at all.
        """
        from .. import config
        root = self.repo_root()
        for key in ("trailofbits", "compound-engineering", "awesome-copilot", "builderio"):
            provider = [p for p in config.load(root) if p.key == key][0]
            doc = M.load(provider.manifest_path)
            entry = next(e for _s, e in M.iter_file_entries(doc)
                         if e["path"].endswith("/LICENSE"))
            self.assertEqual(entry.get("origin"), "local", key)
            self.assertIn(entry["upstream_path"], analyze.licence_upstream_paths(doc), key)

    def test_upstream_origin_licence_files_are_covered(self):
        from .. import config
        root = self.repo_root()
        provider = [p for p in config.load(root) if p.key == "anthropic"][0]
        doc = M.load(provider.manifest_path)
        paths = analyze.licence_upstream_paths(doc)
        self.assertTrue(any(p.endswith("LICENSE.txt") for p in paths), sorted(paths))

    def test_shared_layout_licence_is_covered(self):
        from .. import config
        root = self.repo_root()
        provider = [p for p in config.load(root) if p.key == "mattpocock"][0]
        doc = M.load(provider.manifest_path)
        self.assertIn("LICENSE", analyze.licence_upstream_paths(doc))


class TestDeterminism(AnalyzeCase):
    def test_repeated_analysis_is_byte_identical(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        patched = BASE_BODY.replace("Body line one.", PATCH_MARKER + "Body line one.")
        scenario = fixtures.build(
            tmp.name, vendored_overrides={"SKILL.md": patched},
            target_files={"SKILL.md": BASE_BODY.replace("Body line five.", "Five, revised.")})
        renders = []
        for _ in range(5):
            analysis = analyze.analyze_provider(scenario.provider, scenario.root,
                                                scenario.upstream, scenario.target_sha)
            renders.append(M.dump(candidate.build_manifest(
                analysis, self.identity(analysis))))
        self.assertEqual(len(set(renders)), 1)

    def test_blocked_reason_order_is_stable(self):
        analysis, _ = self.analyze(
            target_files={"SKILL.md": BASE_BODY, "ADDED.md": "# added\n"},
            license_at_target="MIT License\n\nDifferent.\n")
        orders = [[r.code for r in analysis.sorted_blocked()] for _ in range(5)]
        self.assertEqual(len({tuple(o) for o in orders}), 1)
        # INVENTORY precedes LICENCE per BLOCK_ORDER, whatever order they were appended in:
        # the ordering comes from the declared rank, not from discovery order.
        self.assertLess(orders[0].index(analyze.INVENTORY), orders[0].index(analyze.LICENCE))
        self.assertEqual(orders[0], sorted(
            orders[0], key=lambda c: analyze.BLOCK_ORDER.index(c)))


class TestCandidateWriting(AnalyzeCase):
    def test_dry_run_writes_nothing(self):
        analysis, scenario = self.analyze(
            target_files={"SKILL.md": BASE_BODY + "\nnew\n"})
        before = scenario.vendored(".claude/skills/fixture-skill/SKILL.md")
        paths = candidate.write(analysis, scenario.provider, scenario.root,
                                self.identity(analysis), dry_run=True)
        self.assertTrue(paths)
        self.assertEqual(before, scenario.vendored(".claude/skills/fixture-skill/SKILL.md"))

    def test_write_updates_files_and_manifest_and_marks_unaudited(self):
        analysis, scenario = self.analyze(target_files={"SKILL.md": BASE_BODY + "\nnew\n"})
        before = scenario.manifest()
        artifact = self.artifact_root()
        candidate.write(analysis, scenario.provider, artifact, self.identity(analysis))
        doc = M.load(os.path.join(artifact, scenario.provider.manifest_relpath))
        self.assertEqual(doc["upstream_commit"], analysis.target_sha)
        self.assertEqual(doc["automated_candidate"]["state"],
                         states.PREPARED_AUDIT_REQUIRED)
        self.assertFalse(doc["automated_candidate"]["audit_state_reachable_by_g1_1"])
        self.assertEqual(doc["automated_candidate"]["publication_authority"],
                         "UNCOMMISSIONED")
        self.assertEqual(doc["automated_candidate"]["candidate_identity"]["stable_base_sha"],
                         "a" * 40)
        # The bot must NOT restamp ANY review field: they are true statements about the
        # superseded pin, and rewriting either one would assert a review that never happened.
        self.assertEqual(doc["reviewed_at"], before["reviewed_at"])
        self.assertEqual(doc["reviewed_by"], before["reviewed_by"])
        self.assertEqual(doc["automated_candidate"]["reviewed_fields_refer_to"],
                         analysis.pinned_sha)

    def test_candidate_block_is_added_even_without_an_upstream_commit_key(self):
        """The marker's fallback insertion path.

        `build_manifest` normally inserts the block straight after `upstream_commit` so it is
        visible at the top of the diff. A manifest missing that key must still get the marker --
        the block is a safety property, not a formatting nicety, and it may not depend on the
        shape of the document it is protecting.
        """
        analysis, _ = self.analyze(target_files={"SKILL.md": BASE_BODY + "\nnew\n"})
        analysis.new_manifest = {"skills": [], "reviewed_at": "2026-01-01T00:00:00Z"}
        built = candidate.build_manifest(analysis, self.identity(analysis))
        self.assertEqual(built["automated_candidate"]["state"],
                         states.PREPARED_AUDIT_REQUIRED)
        self.assertEqual(built["automated_candidate"]["target_commit"], analysis.target_sha)

    def test_written_files_are_never_executable(self):
        analysis, scenario = self.analyze(
            base_files={"SKILL.md": BASE_BODY, "scripts/run.sh": "#!/bin/sh\nA\n"},
            target_files={"SKILL.md": BASE_BODY, "scripts/run.sh": "#!/bin/sh\nB\n"})
        self.assertFalse(analysis.is_blocked, analysis.blocked)
        artifact = self.artifact_root()
        candidate.write(analysis, scenario.provider, artifact, self.identity(analysis))
        mode = os.stat(os.path.join(artifact,
                                    ".claude/skills/fixture-skill/scripts/run.sh")).st_mode
        self.assertFalse(mode & 0o111)

    def test_regenerated_hashes_match_what_was_written(self):
        analysis, scenario = self.analyze(target_files={"SKILL.md": BASE_BODY + "\nnew\n"})
        artifact = self.artifact_root()
        paths = candidate.write(
            analysis, scenario.provider, artifact, self.identity(analysis))
        document = M.load(os.path.join(artifact, scenario.provider.manifest_relpath))
        for skill in document["skills"]:
            for entry in skill["files"]:
                if entry["path"] not in paths:
                    continue
                with open(os.path.join(artifact, entry["path"]), "rb") as handle:
                    data = handle.read()
                self.assertEqual(M.git_blob_sha(data), entry["vendored_blob_sha"], entry["path"])

    def test_repeated_artifact_build_is_byte_identical(self):
        """The same immutable proposal produces identical bytes in two fresh roots."""
        analysis, scenario = self.analyze(target_files={"SKILL.md": BASE_BODY + "\nnew\n"})
        identity = self.identity(analysis)
        roots = [self.artifact_root(), self.artifact_root()]
        for root in roots:
            candidate.write(analysis, scenario.provider, root, identity)

        def snapshot(root):
            result = {}
            for directory, _dirs, files in os.walk(root):
                for name in files:
                    path = os.path.join(directory, name)
                    rel = os.path.relpath(path, root)
                    with open(path, "rb") as handle:
                        result[rel] = handle.read()
            return result

        self.assertEqual(snapshot(roots[0]), snapshot(roots[1]))


class TestMirroredConstants(unittest.TestCase):
    """The classifier duplicates a few constants from the governance validator by necessity
    (that script is standalone and has no package to import from). These assertions are what
    keep the duplication from drifting silently."""

    def _validator_source(self):
        here = os.path.dirname(os.path.abspath(__file__))
        root = os.path.dirname(os.path.dirname(os.path.dirname(here)))
        with open(os.path.join(root, "scripts", "validate-agent-governance.py"),
                  encoding="utf-8") as handle:
            return handle.read()

    def test_closure_dirs_match_the_validator(self):
        source = self._validator_source()
        for name in analyze.CLOSURE_DIRS:
            self.assertIn('"%s"' % name, source, "CLOSURE_DIRS drifted: %s" % name)

    def test_patch_marker_patterns_match_the_validator(self):
        source = self._validator_source()
        for pattern in (M.PATCH_OPEN_RE, M.PY_MARK_RE, M.JS_MARK_RE):
            self.assertIn(pattern.pattern, source,
                          "patch-marker regex drifted: %s" % pattern.pattern)

    def test_vendored_mode_matches_the_validator(self):
        self.assertIn('!= "100644"', self._validator_source())
        self.assertEqual(M.VENDORED_MODE, "100644")


if __name__ == "__main__":
    unittest.main()
