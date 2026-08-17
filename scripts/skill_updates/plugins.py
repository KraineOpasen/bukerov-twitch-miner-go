"""Monitoring schema for native Claude Code marketplace plugins.

**No plugin is installed by this task, and this module installs none.** The inventory at
`docs/agents/skills-update-plugins.json` ships empty, and every function here is a no-op over an
empty inventory. It exists so that adopting a plugin later is a reviewed data change rather than
a scramble to invent monitoring under time pressure.

Three surfaces, kept apart on purpose
-------------------------------------
A. **Repo-vendored project skills** under `.claude/skills/**` — what the rest of this package
   updates. Content lives in this git tree; provenance is the provider manifests.
B. **Native marketplace plugins** — installed by Claude Code from a marketplace, living in a user
   cache outside this repository. This module *monitors* them; it never installs, updates, or
   touches that cache.
C. **Claude.ai custom ZIP skills** — packaged and uploaded through the web interface. There is no
   documented programmatic upload, so this project treats C as documentation and package output
   only. Nothing here automates it.

What this module will not do
----------------------------
It never runs `claude plugin update` (or any `claude` subcommand), never installs Claude Code in
CI, never writes to a real plugin cache, and never executes plugin code. Its only inputs are this
repository's own inventory file and, optionally, *captured* CLI output committed as a fixture.
That keeps the security boundary identical to the vendored-skill path: upstream artifacts are
data, never executable input.

Version precedence
------------------
Claude Code resolves a plugin's version from the first of these that is present, and this module
mirrors that order exactly rather than inventing its own:

    1. `plugin.json`'s `version`
    2. the marketplace entry's `version`
    3. the git source commit SHA
    4. `unknown`

The interesting cases are the disagreements, and they are why a version string alone is never
sufficient evidence: an unchanged explicit `version` with a changed source commit means the
bytes moved while the label stood still, and a bumped `version` with unchanged bytes means the
label moved while nothing did. Both are reported; only the first is dangerous, and neither is
resolvable by comparing version strings.
"""

import json
import os

from .errors import ConfigError

INVENTORY_RELPATH = os.path.join("docs", "agents", "skills-update-plugins.json")

SUPPORTED_SCHEMA_VERSION = 1

#: Version-resolution sources, most authoritative first. Order IS the precedence rule.
VERSION_SOURCES = ("plugin_json", "marketplace_json", "source_commit")

UNKNOWN_VERSION = "unknown"

#: Every component kind a plugin may contribute. A change to ANY of them is audit-required:
#: each one either adds behaviour, adds authority, or adds context cost, and none of them can be
#: assessed by comparing a version string.
COMPONENT_KINDS = ("skills", "agents", "hooks", "mcp_servers", "lsp_servers", "monitors",
                   "bin", "settings", "dependencies")

#: Marketplaces Anthropic operates, which auto-update by default. Third-party marketplaces
#: default to auto-update OFF. Recorded so the policy note below can be checked mechanically
#: rather than remembered.
ANTHROPIC_MARKETPLACES = ("anthropics/claude-code", "anthropic")


class PluginDrift:
    """One reportable difference between a recorded plugin and its observed state."""

    def __init__(self, plugin, kind, summary, details=None, audit_required=True):
        self.plugin = plugin
        self.kind = kind
        self.summary = summary
        self.details = list(details or [])
        self.audit_required = audit_required

    def to_dict(self):
        return {"plugin": self.plugin, "kind": self.kind, "summary": self.summary,
                "details": list(self.details), "audit_required": self.audit_required}

    def __repr__(self):
        return "<PluginDrift %s %s>" % (self.plugin, self.kind)


def resolve_version(record):
    """Return ``(version, source)`` using Claude Code's documented precedence.

    `record` is a mapping that may carry `plugin_json_version`, `marketplace_version` and
    `source_commit`. Missing or empty values fall through to the next source; when none is
    present the answer is `(UNKNOWN_VERSION, None)` -- explicitly unknown, never guessed and
    never silently substituted with a commit prefix.
    """
    for source in VERSION_SOURCES:
        key = {"plugin_json": "plugin_json_version",
               "marketplace_json": "marketplace_version",
               "source_commit": "source_commit"}[source]
        value = record.get(key)
        if isinstance(value, str) and value.strip():
            return value.strip(), source
    return UNKNOWN_VERSION, None


def load_inventory(repo_root, path=None):
    """Read the plugin inventory. An empty `plugins` list is the normal, shipped state."""
    path = path or os.path.join(repo_root, INVENTORY_RELPATH)
    try:
        with open(path, encoding="utf-8") as handle:
            doc = json.load(handle)
    except OSError as exc:
        raise ConfigError("cannot read plugin inventory %s: %s" % (path, exc))
    except ValueError as exc:
        raise ConfigError("plugin inventory %s is not valid JSON: %s" % (path, exc))
    if not isinstance(doc, dict):
        raise ConfigError("plugin inventory must be a JSON object")
    if doc.get("schema_version") != SUPPORTED_SCHEMA_VERSION:
        raise ConfigError("plugin inventory schema_version must be %d, got %r"
                          % (SUPPORTED_SCHEMA_VERSION, doc.get("schema_version")))
    plugins = doc.get("plugins")
    if not isinstance(plugins, list):
        raise ConfigError("plugin inventory must carry a plugins list (may be empty)")
    for index, entry in enumerate(plugins):
        if not isinstance(entry, dict) or not entry.get("name"):
            raise ConfigError("plugins[%d] must be an object with a name" % index)
    return doc


def component_inventory(record):
    """Normalized component counts/identities for one plugin record.

    Every kind is present in the result even when absent from the record, so a component
    *disappearing* is a comparable value change rather than a missing key that a naive diff
    would skip.
    """
    components = record.get("components") or {}
    out = {}
    for kind in COMPONENT_KINDS:
        value = components.get(kind)
        if value is None:
            out[kind] = []
        elif isinstance(value, list):
            out[kind] = sorted(str(v) for v in value)
        elif isinstance(value, dict):
            out[kind] = sorted(str(k) for k in value)
        else:
            out[kind] = [str(value)]
    return out


def compare(recorded, observed):
    """Compare one recorded plugin against an observed state. Returns a list of `PluginDrift`.

    `observed` is whatever an adapter produced -- a captured `claude plugin list --json` entry,
    or a re-read of the plugin's source. Both arguments are plain mappings, so this is a pure
    function and the whole comparison is testable without Claude Code present.
    """
    name = recorded.get("name") or observed.get("name") or "<unnamed>"
    drifts = []

    rec_version, rec_source = resolve_version(recorded)
    obs_version, obs_source = resolve_version(observed)
    rec_commit = recorded.get("source_commit")
    obs_commit = observed.get("source_commit")

    if rec_version != obs_version:
        drifts.append(PluginDrift(
            name, "version",
            "version changed: %s -> %s" % (rec_version, obs_version),
            ["resolved from %s -> %s" % (rec_source or "unknown", obs_source or "unknown")]))

    # The two disagreements a version string cannot express on its own.
    if rec_version == obs_version and rec_commit and obs_commit and rec_commit != obs_commit:
        drifts.append(PluginDrift(
            name, "version-source-disagreement",
            "version is unchanged (%s) but the source commit moved" % rec_version,
            ["source_commit %s -> %s" % (rec_commit, obs_commit),
             "an unchanged version label over changed bytes is the dangerous direction: nothing "
             "about the label proves the content was reviewed"]))
    if rec_version != obs_version and rec_commit and obs_commit and rec_commit == obs_commit:
        drifts.append(PluginDrift(
            name, "version-bump-without-content-change",
            "version moved (%s -> %s) while the source commit stayed at %s"
            % (rec_version, obs_version, rec_commit),
            ["harmless in itself, but it means the version label is not a reliable change "
             "signal for this plugin"],
            audit_required=False))

    for field, label in (("source_url", "source repository"), ("source_ref", "source ref"),
                         ("marketplace", "marketplace")):
        if recorded.get(field) and observed.get(field) and \
                recorded[field] != observed[field]:
            drifts.append(PluginDrift(
                name, "source-drift",
                "%s changed: %r -> %r" % (label, recorded[field], observed[field])))

    rec_components = component_inventory(recorded)
    obs_components = component_inventory(observed)
    for kind in COMPONENT_KINDS:
        if rec_components[kind] != obs_components[kind]:
            added = sorted(set(obs_components[kind]) - set(rec_components[kind]))
            removed = sorted(set(rec_components[kind]) - set(obs_components[kind]))
            drifts.append(PluginDrift(
                name, "component-surface",
                "%s component surface changed" % kind,
                (["added: %s" % ", ".join(added)] if added else []) +
                (["removed: %s" % ", ".join(removed)] if removed else [])))

    rec_cost = recorded.get("projected_context_tokens")
    obs_cost = observed.get("projected_context_tokens")
    if isinstance(rec_cost, int) and isinstance(obs_cost, int) and rec_cost != obs_cost:
        drifts.append(PluginDrift(
            name, "context-cost",
            "projected context cost changed: %d -> %d tokens" % (rec_cost, obs_cost),
            ["every installed plugin spends context on every session; a growing surface is a "
             "real cost even when nothing behaves differently"],
            audit_required=abs(obs_cost - rec_cost) > 0))

    return drifts


def check_inventory(repo_root, observations=None, path=None):
    """Compare the whole recorded inventory against `observations`.

    `observations` maps plugin name -> observed record, typically produced by
    `adapter.load_captured()`. Absent, every recorded plugin is simply reported as unobserved,
    which is the correct answer for a repository that records plugins but has no captured CLI
    output committed.

    With the shipped (empty) inventory this returns `[]` and performs no I/O beyond reading one
    small JSON file -- the no-op that the daily workflow relies on.
    """
    doc = load_inventory(repo_root, path)
    observations = observations or {}
    drifts = []
    for record in doc["plugins"]:
        name = record["name"]
        observed = observations.get(name)
        if observed is None:
            if observations:
                drifts.append(PluginDrift(
                    name, "unobserved",
                    "recorded plugin %r does not appear in the captured plugin list" % name,
                    ["either it was uninstalled, or the capture is stale"]))
            continue
        drifts.extend(compare(record, observed))
    for name in sorted(set(observations) - {p["name"] for p in doc["plugins"]}):
        drifts.append(PluginDrift(
            name, "unrecorded",
            "plugin %r is installed but not recorded in the inventory" % name,
            ["an unreviewed plugin contributes skills, agents, hooks and context cost that no "
             "manifest describes"]))
    return drifts


def auto_update_policy_note(marketplace):
    """The documented native auto-update behaviour for a marketplace, as prose for a report.

    Accurate to Claude Code's documented behaviour: Anthropic's own marketplaces auto-update by
    default; third-party marketplaces do not. Either way an updated plugin is not live in a
    running session until `/reload-plugins` or a new session -- the session keeps whatever it
    loaded at startup.
    """
    if marketplace and any(marketplace.startswith(m) for m in ANTHROPIC_MARKETPLACES):
        return ("%s is an Anthropic marketplace: plugins from it AUTO-UPDATE by default. For a "
                "project-governed, audited plugin this should be turned off so updates arrive "
                "through the monitored candidate flow instead of silently." % marketplace)
    return ("%s is a third-party marketplace: auto-update defaults to OFF, which is the posture "
            "this project wants. Updates should arrive through the monitored candidate flow."
            % (marketplace or "this marketplace"))


class CapturedPluginAdapter:
    """Reads *captured* `claude plugin list --json` / `claude plugin details` output.

    Optional, and inert by default. It parses committed fixture files; it does not invoke the
    `claude` CLI, require a login, read a real plugin cache, or touch the network. That is what
    lets plugin monitoring be exercised in CI with no credentials and no Claude Code install --
    and what keeps a CI job from ever mutating a developer's actual plugin state.
    """

    def __init__(self, list_path=None, details_paths=None):
        self.list_path = list_path
        self.details_paths = list(details_paths or [])

    @staticmethod
    def _read_json(path):
        try:
            with open(path, encoding="utf-8") as handle:
                return json.load(handle)
        except OSError as exc:
            raise ConfigError("cannot read captured plugin output %s: %s" % (path, exc))
        except ValueError as exc:
            raise ConfigError("captured plugin output %s is not valid JSON: %s" % (path, exc))

    def load_captured(self):
        """Return ``{plugin_name: observed_record}`` from the captured files."""
        observed = {}
        if self.list_path:
            doc = self._read_json(self.list_path)
            entries = doc.get("plugins") if isinstance(doc, dict) else doc
            for entry in entries or []:
                if isinstance(entry, dict) and entry.get("name"):
                    observed[entry["name"]] = dict(entry)
        for path in self.details_paths:
            doc = self._read_json(path)
            if isinstance(doc, dict) and doc.get("name"):
                observed.setdefault(doc["name"], {}).update(doc)
        return observed
