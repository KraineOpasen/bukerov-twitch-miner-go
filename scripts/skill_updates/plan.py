#!/usr/bin/env python3
"""Turn a `check --json-out` report into GitHub Actions job outputs.

Split out of the workflow YAML on purpose. Inline `run:` scripting is where Actions workflows
accumulate their subtlest bugs -- quoting, `set -e` interactions, JSON built with `echo` -- and
none of it is testable. This is a normal Python module with a normal test.

The output contract consumed by `.github/workflows/stable-skills-maintenance.yml`:

    mode=check|prepare
    any_prepare=true|false         at least one inert artifact must be prepared
    matrix={"include":[...]}       provider + exact target SHA for each artifact

Writing to `$GITHUB_OUTPUT` is itself an injection sink: a value containing a newline can forge
additional outputs, and one containing the heredoc delimiter can escape a multi-line block. Both
are made impossible here rather than trusted -- every emitted value is asserted to be a single
line drawn from an allowlisted character set, and provider keys have already been validated by
`config`. A value that fails those assertions aborts the run instead of being sanitized, because
a provider key that is not `[A-Za-z0-9-]` means the config was tampered with, not that it needs
escaping.
"""

import argparse
import json
import re
import sys

#: Everything this module is allowed to emit. Anything else means an upstream assumption broke.
SAFE_VALUE_RE = re.compile(r"^[A-Za-z0-9_.,:/{}\[\]\"-]*$")
PROVIDER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9-]{0,39}$")
SHA_RE = re.compile(r"^[0-9a-f]{40}$")

MODES = ("check", "prepare")
PRODUCTION_STATES = ("NO_DRIFT", "BLOCKED", "PREPARED_AUDIT_REQUIRED")
EXPECTED_PROVIDERS = (
    ("mattpocock", False),
    ("anthropic", False),
    ("compound-engineering", False),
    ("trailofbits", False),
    ("awesome-copilot", False),
    ("builderio", False),
    ("builder-agent-skills", True),
)
REPORT_KEYS = {
    "schema", "g1_1_mode", "publication_authority", "stable_identity", "providers",
    "plugins", "summary",
}
PROVIDER_KEYS = {
    "provider", "state", "upstream_repo", "upstream_ref", "pinned_sha", "target_sha",
    "monitor_only", "drifted", "ancestry", "blocked", "discoveries", "eval_required",
    "changed_files", "changed_file_count", "notes",
}
PLUGIN_KEYS = {"plugin", "kind", "summary", "details", "audit_required"}
SUMMARY_KEYS = {
    "checked", "drifted", "blocked", "prepared_audit_required", "discovery_required",
    "eval_required", "plugin_drifts", "states",
}


def validate_stable_identity(report):
    """Fail closed unless the detector report is bound to one live stable/control identity."""
    identity = report.get("stable_identity")
    required = {
        "subject_kind", "repository_id", "repository_full_name", "stable_branch",
        "stable_base_sha", "selected_ref", "live_default_branch", "fetched_default_sha",
        "control_input_digest", "workflow_path", "g1_1_mode", "publication_authority",
    }
    if not isinstance(identity, dict) or set(identity) != required:
        raise SystemExit("report lacks the closed stable/control identity")
    if (identity["subject_kind"] != "stable-run"
            or identity["repository_id"] != 1297795646
            or identity["repository_full_name"] != "KraineOpasen/bukerov-twitch-miner-go"
            or identity["stable_branch"] != "release/0.3"
            or identity["selected_ref"] != identity["stable_branch"]
            or identity["live_default_branch"] != identity["stable_branch"]
            or identity["workflow_path"]
            != ".github/workflows/stable-skills-maintenance.yml"
            or identity["g1_1_mode"] != "artifact-only"
            or identity["publication_authority"] != "UNCOMMISSIONED"):
        raise SystemExit("report stable/control identity mismatch")
    base = identity["stable_base_sha"]
    if not isinstance(base, str) or not SHA_RE.match(base):
        raise SystemExit("report stable base is not a full lowercase SHA")
    if identity["fetched_default_sha"] != base:
        raise SystemExit("report stable base is not the fetched live default head")
    digest = identity["control_input_digest"]
    if (not isinstance(digest, str) or len(digest) != 64
            or any(c not in "0123456789abcdef" for c in digest)):
        raise SystemExit("report control input digest is not full SHA-256")
    return identity


def _validate_provider_records(providers):
    """Return the closed, complete provider vector or stop on any truncation/ambiguity."""
    if not isinstance(providers, list):
        raise SystemExit("report providers must be a complete ordered array")
    actual = []
    for index, entry in enumerate(providers):
        if not isinstance(entry, dict) or set(entry) != PROVIDER_KEYS:
            raise SystemExit("provider record %d does not match the closed report schema" % index)
        provider = entry["provider"]
        if not isinstance(provider, str) or PROVIDER_RE.fullmatch(provider) is None:
            raise SystemExit("refusing unsafe provider key %r" % (provider,))
        if type(entry["monitor_only"]) is not bool or type(entry["drifted"]) is not bool:
            raise SystemExit("provider %r has non-boolean classification fields" % provider)
        actual.append((provider, entry["monitor_only"]))

        state = entry["state"]
        if state not in PRODUCTION_STATES:
            raise SystemExit("refusing report entry with non-G1.1 state %r" % (state,))
        pinned = entry["pinned_sha"]
        if not isinstance(pinned, str) or SHA_RE.fullmatch(pinned) is None:
            raise SystemExit("provider %r pin is not a full lowercase SHA" % provider)
        target = entry["target_sha"]
        if target is not None and (not isinstance(target, str) or SHA_RE.fullmatch(target) is None):
            raise SystemExit("provider %r target is not null or a full lowercase SHA" % provider)
        expected_drift = target is not None and target != pinned
        if entry["drifted"] != expected_drift:
            raise SystemExit("provider %r drift flag disagrees with full pin/target SHAs"
                             % provider)
        for key in ("upstream_repo", "upstream_ref"):
            if not isinstance(entry[key], str) or not entry[key]:
                raise SystemExit("provider %r has malformed %s" % (provider, key))
        if entry["ancestry"] is not None and not isinstance(entry["ancestry"], dict):
            raise SystemExit("provider %r has malformed ancestry" % provider)
        for key in ("blocked", "discoveries", "eval_required", "changed_files", "notes"):
            if not isinstance(entry[key], list):
                raise SystemExit("provider %r has malformed %s" % (provider, key))
        if (type(entry["changed_file_count"]) is not int
                or entry["changed_file_count"] != len(entry["changed_files"])):
            raise SystemExit("provider %r changed-file count mismatch" % provider)

        if state == "NO_DRIFT":
            if entry["drifted"] or entry["blocked"] or target is None:
                raise SystemExit("provider %r has inconsistent NO_DRIFT facts" % provider)
        elif state == "BLOCKED":
            if not entry["blocked"]:
                raise SystemExit("provider %r BLOCKED state lacks a blocking reason" % provider)
        else:
            if (not entry["drifted"] or entry["blocked"] or target is None
                    or entry["monitor_only"]):
                raise SystemExit(
                    "provider %r has inconsistent PREPARED_AUDIT_REQUIRED facts" % provider)

    if tuple(actual) != EXPECTED_PROVIDERS:
        raise SystemExit("report provider vector is missing, duplicated, reordered, or extra")
    return providers


def validate_report(report):
    """Validate the complete detector envelope before deriving any healthy/no-op output."""
    if not isinstance(report, dict) or set(report) != REPORT_KEYS:
        raise SystemExit("detector report does not match the closed top-level schema")
    if (report["schema"] != "skills-update-check/2"
            or report["g1_1_mode"] != "artifact-only"
            or report["publication_authority"] != "UNCOMMISSIONED"):
        raise SystemExit("detector report schema or G1.1 authority mismatch")
    validate_stable_identity(report)
    providers = _validate_provider_records(report["providers"])

    plugins = report["plugins"]
    if not isinstance(plugins, list):
        raise SystemExit("detector report plugins must be an array")
    for index, plugin in enumerate(plugins):
        if not isinstance(plugin, dict) or set(plugin) != PLUGIN_KEYS:
            raise SystemExit("plugin record %d does not match the closed report schema" % index)
        if (not all(isinstance(plugin[key], str) for key in ("plugin", "kind", "summary"))
                or not isinstance(plugin["details"], list)
                or type(plugin["audit_required"]) is not bool):
            raise SystemExit("plugin record %d has malformed fields" % index)

    expected_summary = {
        "checked": len(providers),
        "drifted": sum(1 for entry in providers if entry["drifted"]),
        "blocked": sum(1 for entry in providers if entry["state"] == "BLOCKED"),
        "prepared_audit_required": sum(
            1 for entry in providers if entry["state"] == "PREPARED_AUDIT_REQUIRED"),
        "discovery_required": sum(1 for entry in providers if entry["discoveries"]),
        "eval_required": sum(1 for entry in providers if entry["eval_required"]),
        "plugin_drifts": len(plugins),
        "states": {
            state: sum(1 for entry in providers if entry["state"] == state)
            for state in PRODUCTION_STATES
        },
    }
    summary = report["summary"]
    if (not isinstance(summary, dict) or set(summary) != SUMMARY_KEYS
            or summary != expected_summary):
        raise SystemExit("detector report summary does not match provider/plugin records")
    return providers


def preparations(report):
    """Return the exact inert preparation matrix.

    G1.1 has no issue or pull-request publication job.  Only a completed
    `PREPARED_AUDIT_REQUIRED` verdict may materialize an artifact.  Unknown, intermediate, or
    malformed states abort planning rather than becoming a silent no-op.
    """
    include = []
    for entry in validate_report(report):
        state = entry.get("state")
        if state != "PREPARED_AUDIT_REQUIRED":
            continue
        provider = entry.get("provider")
        target = entry.get("target_sha")
        include.append({"provider": provider, "target_sha": target})
    return include


def eval_required_providers(report):
    """Providers whose candidate cannot be argued behaviourally equivalent by provenance alone."""
    return [e["provider"] for e in validate_report(report) if e["eval_required"]]


def discovery_providers(report):
    """Providers with new sibling skills upstream that were not installed."""
    return [e["provider"] for e in validate_report(report) if e["discoveries"]]


def build_outputs(report, mode):
    if mode not in MODES:
        raise SystemExit("unknown mode %r (want one of %s)" % (mode, ", ".join(MODES)))
    include = preparations(report)
    return {
        "mode": mode,
        "any_prepare": "true" if include else "false",
        # `separators` keeps it on one line -- a multi-line output would need heredoc framing,
        # which is the part of the GITHUB_OUTPUT protocol worth not depending on.
        "matrix": json.dumps({"include": include}, separators=(",", ":")),
    }


def write_outputs(outputs, path):
    lines = []
    for key, value in sorted(outputs.items()):
        if "\n" in value or "\r" in value:
            raise SystemExit("refusing to emit multi-line output %r" % key)
        if not SAFE_VALUE_RE.match(value):
            raise SystemExit("refusing to emit output %r with unexpected characters: %r"
                             % (key, value))
        lines.append("%s=%s" % (key, value))
    text = "\n".join(lines) + "\n"
    if path:
        with open(path, "a", encoding="utf-8") as handle:
            handle.write(text)
    return text


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", required=True, help="path to check --json-out output")
    parser.add_argument("--mode", required=True, choices=MODES)
    parser.add_argument("--github-output", help="path to $GITHUB_OUTPUT")
    args = parser.parse_args(argv)
    with open(args.report, encoding="utf-8") as handle:
        report = json.load(handle)
    outputs = build_outputs(report, args.mode)
    sys.stdout.write(write_outputs(outputs, args.github_output))
    return 0


if __name__ == "__main__":
    sys.exit(main())
