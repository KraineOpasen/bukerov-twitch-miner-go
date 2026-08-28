"""Tests for native-plugin monitoring.

No plugin is installed, and these tests install none. They exercise the schema and the
comparison logic against fixture data, which is exactly the arrangement the design requires:
plugin monitoring must be testable in CI with no credentials, no Claude Code install, and no
real plugin cache anywhere in the picture.
"""

import json
import os
import tempfile
import unittest

from .. import plugins
from ..errors import ConfigError

REPO = os.path.dirname(os.path.dirname(os.path.dirname(
    os.path.dirname(os.path.abspath(__file__)))))


class TestVersionPrecedence(unittest.TestCase):
    """Claude Code's documented order: plugin.json > marketplace.json > source commit > unknown."""

    def test_plugin_json_wins(self):
        self.assertEqual(
            plugins.resolve_version({"plugin_json_version": "1.2.3",
                                     "marketplace_version": "9.9.9",
                                     "source_commit": "a" * 40}),
            ("1.2.3", "plugin_json"))

    def test_marketplace_is_second(self):
        self.assertEqual(
            plugins.resolve_version({"marketplace_version": "2.0.0", "source_commit": "a" * 40}),
            ("2.0.0", "marketplace_json"))

    def test_source_commit_is_third(self):
        self.assertEqual(plugins.resolve_version({"source_commit": "a" * 40}),
                         ("a" * 40, "source_commit"))

    def test_unknown_is_last_and_explicit(self):
        self.assertEqual(plugins.resolve_version({}), (plugins.UNKNOWN_VERSION, None))
        self.assertEqual(plugins.resolve_version({"plugin_json_version": "  "}),
                         (plugins.UNKNOWN_VERSION, None))

    def test_precedence_order_is_declared_not_incidental(self):
        self.assertEqual(plugins.VERSION_SOURCES,
                         ("plugin_json", "marketplace_json", "source_commit"))


class TestComparison(unittest.TestCase):
    def base(self, **over):
        record = {"name": "demo", "plugin_json_version": "1.0.0", "source_commit": "a" * 40,
                  "source_url": "https://github.com/x/y", "source_ref": "main",
                  "marketplace": "third-party/mkt",
                  "components": {"skills": ["s1"], "agents": [], "hooks": []},
                  "projected_context_tokens": 1000}
        record.update(over)
        return record

    def kinds(self, drifts):
        return sorted(d.kind for d in drifts)

    def test_identical_records_produce_no_drift(self):
        self.assertEqual(plugins.compare(self.base(), self.base()), [])

    def test_unchanged_version_with_changed_source_is_flagged(self):
        """The dangerous direction: the label stands still while the bytes move."""
        drifts = plugins.compare(self.base(), self.base(source_commit="b" * 40))
        self.assertIn("version-source-disagreement", self.kinds(drifts))
        flagged = [d for d in drifts if d.kind == "version-source-disagreement"][0]
        self.assertTrue(flagged.audit_required)

    def test_version_bump_with_unchanged_bytes_is_reported_but_not_audit_required(self):
        drifts = plugins.compare(self.base(), self.base(plugin_json_version="1.0.1"))
        kinds = self.kinds(drifts)
        self.assertIn("version", kinds)
        self.assertIn("version-bump-without-content-change", kinds)
        bump = [d for d in drifts if d.kind == "version-bump-without-content-change"][0]
        self.assertFalse(bump.audit_required)

    def test_source_ref_drift_is_flagged(self):
        drifts = plugins.compare(self.base(), self.base(source_ref="next"))
        self.assertIn("source-drift", self.kinds(drifts))

    def test_marketplace_change_is_flagged(self):
        drifts = plugins.compare(self.base(), self.base(marketplace="other/mkt"))
        self.assertIn("source-drift", self.kinds(drifts))

    def test_component_surface_change_is_audit_required(self):
        drifts = plugins.compare(
            self.base(),
            self.base(components={"skills": ["s1"], "agents": ["a1"], "hooks": []}))
        surface = [d for d in drifts if d.kind == "component-surface"]
        self.assertTrue(surface)
        self.assertTrue(all(d.audit_required for d in surface))
        self.assertIn("added: a1", " ".join(surface[0].details))

    def test_component_removal_is_detected_too(self):
        drifts = plugins.compare(self.base(), self.base(components={"skills": [], "agents": []}))
        surface = [d for d in drifts if d.kind == "component-surface"]
        self.assertTrue(surface)
        self.assertIn("removed: s1", " ".join(surface[0].details))

    def test_every_component_kind_is_compared(self):
        for kind in plugins.COMPONENT_KINDS:
            drifts = plugins.compare(self.base(), self.base(components={kind: ["new"]}))
            self.assertTrue([d for d in drifts if d.kind == "component-surface"], kind)

    def test_context_cost_change_is_reported(self):
        drifts = plugins.compare(self.base(), self.base(projected_context_tokens=5000))
        self.assertIn("context-cost", self.kinds(drifts))

    def test_dependency_drift_is_a_component_surface_change(self):
        drifts = plugins.compare(
            self.base(), self.base(components={"skills": ["s1"], "dependencies": ["left-pad"]}))
        self.assertIn("component-surface", self.kinds(drifts))


class TestInventory(unittest.TestCase):
    def test_shipped_inventory_is_empty_and_checks_are_a_no_op(self):
        doc = plugins.load_inventory(REPO)
        self.assertEqual(doc["plugins"], [], "no plugin should be installed by this task")
        self.assertEqual(plugins.check_inventory(REPO), [])

    def test_shipped_inventory_documents_all_three_surfaces(self):
        doc = plugins.load_inventory(REPO)
        self.assertEqual(sorted(doc["surfaces"]),
                         ["A_project_skills", "B_native_plugins", "C_claudeai_zip_skills"])
        self.assertIn("no documented programmatic upload",
                      doc["surfaces"]["C_claudeai_zip_skills"])

    def test_shipped_inventory_records_the_documented_precedence(self):
        doc = plugins.load_inventory(REPO)
        self.assertEqual(doc["version_precedence"][0], "plugin.json version")
        self.assertEqual(doc["version_precedence"][-1], "unknown")

    def test_installed_but_unrecorded_plugin_is_reported(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "inv.json")
            with open(path, "w", encoding="utf-8") as handle:
                json.dump({"schema_version": 1, "plugins": []}, handle)
            drifts = plugins.check_inventory(tmp, {"ghost": {"name": "ghost"}}, path=path)
            self.assertEqual([d.kind for d in drifts], ["unrecorded"])

    def test_recorded_but_missing_plugin_is_reported(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "inv.json")
            with open(path, "w", encoding="utf-8") as handle:
                json.dump({"schema_version": 1, "plugins": [{"name": "gone"}]}, handle)
            drifts = plugins.check_inventory(tmp, {"other": {"name": "other"}}, path=path)
            self.assertIn("unobserved", [d.kind for d in drifts])

    def test_bad_schema_version_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "inv.json")
            with open(path, "w", encoding="utf-8") as handle:
                json.dump({"schema_version": 99, "plugins": []}, handle)
            with self.assertRaises(ConfigError):
                plugins.load_inventory(tmp, path)


class TestCapturedAdapter(unittest.TestCase):
    """The adapter reads committed fixtures. It never runs `claude`."""

    def test_reads_captured_list_output(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "list.json")
            with open(path, "w", encoding="utf-8") as handle:
                json.dump({"plugins": [{"name": "demo", "plugin_json_version": "1.0.0"}]},
                          handle)
            observed = plugins.CapturedPluginAdapter(list_path=path).load_captured()
            self.assertEqual(observed["demo"]["plugin_json_version"], "1.0.0")

    def test_details_merge_into_the_list_entry(self):
        with tempfile.TemporaryDirectory() as tmp:
            lst = os.path.join(tmp, "list.json")
            det = os.path.join(tmp, "details.json")
            with open(lst, "w", encoding="utf-8") as handle:
                json.dump([{"name": "demo", "plugin_json_version": "1.0.0"}], handle)
            with open(det, "w", encoding="utf-8") as handle:
                json.dump({"name": "demo", "components": {"skills": ["a"]}}, handle)
            observed = plugins.CapturedPluginAdapter(lst, [det]).load_captured()
            self.assertEqual(observed["demo"]["components"], {"skills": ["a"]})
            self.assertEqual(observed["demo"]["plugin_json_version"], "1.0.0")

    def test_malformed_capture_is_refused_not_ignored(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "list.json")
            with open(path, "w", encoding="utf-8") as handle:
                handle.write("not json")
            with self.assertRaises(ConfigError):
                plugins.CapturedPluginAdapter(list_path=path).load_captured()

    def test_module_never_invokes_the_claude_cli(self):
        """Structural: no `claude` invocation, no subprocess, anywhere in the module."""
        with open(os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                               "plugins.py"), encoding="utf-8") as handle:
            source = handle.read()
        self.assertNotIn("subprocess", source)
        self.assertNotIn("plugin update", source.replace(
            "never runs `claude plugin update`", ""))


class TestAutoUpdatePolicyNote(unittest.TestCase):
    def test_anthropic_marketplace_is_described_as_auto_updating(self):
        note = plugins.auto_update_policy_note("anthropics/claude-code")
        self.assertIn("AUTO-UPDATE", note)
        self.assertIn("turned off", note)

    def test_third_party_marketplace_defaults_off(self):
        note = plugins.auto_update_policy_note("someone/else")
        self.assertIn("defaults to OFF", note)


if __name__ == "__main__":
    unittest.main()
