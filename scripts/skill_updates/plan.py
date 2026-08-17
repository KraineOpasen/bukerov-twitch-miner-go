#!/usr/bin/env python3
"""Turn a `check --json-out` report into GitHub Actions job outputs.

Split out of the workflow YAML on purpose. Inline `run:` scripting is where Actions workflows
accumulate their subtlest bugs -- quoting, `set -e` interactions, JSON built with `echo` -- and
none of it is testable. This is a normal Python module with a normal test.

The output contract consumed by .github/workflows/skills-update.yml:

    mode=check|prepare
    any_action=true|false          any provider needs a PR or an issue
    providers=["a","b"]            compact JSON array, safe for `fromJSON()` in a matrix

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
SAFE_VALUE_RE = re.compile(r"^[A-Za-z0-9_.,:/\[\]\"-]*$")

MODES = ("check", "prepare")


def providers_needing_action(report):
    """Provider keys that require the publication job.

    Both ordinary outcomes need it, for different reasons: a clean candidate needs a branch and a
    Draft PR, and a blocked one needs its deduplicated issue opened or refreshed. A provider that
    is simply up to date needs nothing, which is the normal daily result.

    An audit-only monitor is the exception. Nothing from it is vendored, so a moved commit alone
    produces no candidate and no issue -- only a *watched licence path* changing does. Scheduling
    the publication job for an unblocked monitor would start a privileged job (contents/
    pull-requests/issues write) every single day for the rest of that repository's life, to do
    nothing. So a monitor is scheduled only when it is actually blocked.
    """
    keys = []
    for entry in report.get("providers", []):
        blocked = entry.get("blocked")
        # A provider whose ref could not be resolved is BLOCKED with `drifted` false -- there is
        # no target commit to compare against. Gating purely on `drifted` silently dropped that
        # whole class from the publication job, so an unreachable or renamed upstream produced a
        # green run and no issue, contradicting the documented "blocked -> one issue" contract.
        if not entry.get("drifted") and not blocked:
            continue
        if entry.get("monitor_only") and not blocked:
            continue
        keys.append(entry["provider"])
    return keys


def eval_required_providers(report):
    """Providers whose candidate cannot be argued behaviourally equivalent by provenance alone."""
    return [e["provider"] for e in report.get("providers", []) if e.get("eval_required")]


def discovery_providers(report):
    """Providers with new sibling skills upstream that were not installed."""
    return [e["provider"] for e in report.get("providers", []) if e.get("discoveries")]


def build_outputs(report, mode):
    if mode not in MODES:
        raise SystemExit("unknown mode %r (want one of %s)" % (mode, ", ".join(MODES)))
    keys = providers_needing_action(report)
    return {
        "mode": mode,
        "any_action": "true" if keys else "false",
        # `separators` keeps it on one line -- a multi-line output would need heredoc framing,
        # which is the part of the GITHUB_OUTPUT protocol worth not depending on.
        "providers": json.dumps(keys, separators=(",", ":")),
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
