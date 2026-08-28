#!/usr/bin/env python3
"""Controlled mutation probes for the update bot's load-bearing checks.

    python3 scripts/skill_updates/tests/mutation_probe.py

A passing test suite proves the code does what the tests say. It does not prove the tests would
*notice* if the code stopped doing it. These probes close that gap for the handful of decisions
where a silent failure would be worst: the merge rules, the licence guard, the candidate-state
marker, and publication deduplication. Each probe breaks one of them on purpose and asserts the
suite goes red. A probe that survives is a hole in the tests, reported as a failure here.

Safety properties, because this edits real source files:

* The exact bytes of every file it will touch are captured before anything changes, and restored
  in a `finally` -- including on KeyboardInterrupt or an exception inside a probe.
* Restoration is *verified by SHA-256*, not assumed, and a mismatch is reported loudly.
* Mutations are literal string substitutions that must match exactly once; a substitution that
  matches zero or several times aborts that probe instead of silently editing something else.
"""

import hashlib
import os
import signal
import subprocess
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
PKG = os.path.dirname(HERE)
REPO = os.path.dirname(os.path.dirname(PKG))

#: (name, relative-file, find, replace, why-this-must-be-caught)
PROBES = [
    ("merge-rule-take-theirs-inverted", "merge3.py",
     "    if ours == base:\n        return TAKE_THEIRS, theirs, []",
     "    if ours == base:\n        return TAKE_THEIRS, ours, []",
     "an unpatched file would silently keep the OLD bytes while reporting it adopted upstream"),

    ("merge-rule-retain-ours-inverted", "merge3.py",
     "    if theirs == base:\n        return RETAIN_OURS, ours, []",
     "    if theirs == base:\n        return RETAIN_OURS, theirs, []",
     "a local patch would be discarded whenever upstream did not touch the file"),

    ("merge-conflict-suppressed", "merge3.py",
     "        else:\n            conflicts.append({\"base\": region_base, \"ours\": region_ours,",
     "        elif False:\n            conflicts.append({\"base\": region_base, \"ours\": region_ours,",
     "overlapping edits would merge silently instead of blocking"),

    ("binary-conflict-suppressed", "merge3.py",
     "    if is_binary(base) or is_binary(ours) or is_binary(theirs):\n"
     "        return BINARY_CONFLICT, None, []",
     "    if False:\n        return BINARY_CONFLICT, None, []",
     "a binary file changed on both sides would be 'merged' as mangled text"),

    ("licence-change-ignored", "analyze.py",
     "        elif base_meta and head_meta[2] != base_meta[2]:\n"
     "            analysis.block(LICENCE, \"licence text changed upstream\",",
     "        elif False:\n            analysis.block(LICENCE, \"licence text changed upstream\",",
     "an upstream relicensing would be vendored without anyone reading it"),

    ("licence-deletion-ignored", "analyze.py",
     "        if head_meta is None:\n            analysis.block(LICENCE, \"licence file disappeared upstream\",",
     "        if False:\n            analysis.block(LICENCE, \"licence file disappeared upstream\",",
     "redistribution would continue after upstream withdrew the licence"),

    ("candidate-marker-omitted", "candidate.py",
     "    if \"automated_candidate\" not in out:\n        out[\"automated_candidate\"] = candidate_block(analysis, identity)",
     "    if False:\n        out[\"automated_candidate\"] = candidate_block(analysis, identity)",
     "a machine-prepared pin could pass the governance gate as if audited"),

    ("candidate-restamps-review-fields", "candidate.py",
     "    doc = analysis.new_manifest\n    if doc is None:",
     "    doc = dict(analysis.new_manifest or {})\n    doc[\"reviewed_by\"] = \"bot\"\n    if not doc:",
     "the bot would assert a review it never performed"),

    ("dedup-branch-check-removed", "publish.py",
     "    branch_present = adapter.branch_exists(branch)",
     "    branch_present = False",
     "a wedged branch would be re-committed and re-pushed instead of just getting its PR"),

    ("dedup-pr-check-removed", "publish.py",
     "    if existing:\n        return {\"status\": \"duplicate\", \"branch\": branch,"
     " \"pull_request\": existing.get(\"number\"),",
     "    if False:\n        return {\"status\": \"duplicate\", \"branch\": branch,"
     " \"pull_request\": existing.get(\"number\"),",
     "a closed-unmerged candidate would be reopened nightly"),

    ("blocked-candidate-write-allowed", "candidate.py",
     "    if analysis.is_blocked:\n        raise ConfigError(",
     "    if False:\n        raise ConfigError(",
     "a partially-classified provider could reach the filesystem"),

    ("script-audit-carried-across-a-change", "analyze.py",
     "    new_skill[\"scripts_audited\"] = False",
     "    new_skill[\"scripts_audited\"] = True",
     "a human's 'I read this script' would survive the bot rewriting that script"),

    ("patch-map-guard-removed", "analyze.py",
     "    lost = sorted(before - after)\n    if lost:",
     "    lost = sorted(before - after)\n    if False:",
     "a merge that dropped a governance patch would ship as clean"),

    # --- owner-addendum surfaces -------------------------------------------------------
    ("automation-may-set-audited", "states.py",
     "    return state == AUTOMATION_CREATABLE",
     "    return state in (AUTOMATION_CREATABLE, AUDITED)",
     "automation could write the AUDITED state, manufacturing a review"),

    ("automation-creatable-widened", "states.py",
     "AUTOMATION_CREATABLE = PREPARED_AUDIT_REQUIRED",
     "AUTOMATION_CREATABLE = AUDITED",
     "the single state automation may create would become the human-only one"),

    ("audited-reachable-from-blocked", "states.py",
     "    BLOCKED: frozenset({DRIFT_DETECTED, NO_DRIFT}),",
     "    BLOCKED: frozenset({DRIFT_DETECTED, NO_DRIFT, AUDITED}),",
     "a blocked provider could transition straight to audited"),

    ("blocked-loses-to-prepared", "states.py",
     "    if blocked:\n        return BLOCKED\n"
     "    if not drifted:\n        return NO_DRIFT\n"
     "    if prepared:\n        return PREPARED_AUDIT_REQUIRED",
     "    if prepared:\n        return PREPARED_AUDIT_REQUIRED\n"
     "    if not drifted:\n        return NO_DRIFT\n"
     "    if blocked:\n        return BLOCKED",
     "a provider that blocked after producing verdicts would report as a usable candidate"),

    ("diverged-history-treated-as-advanceable", "ancestry.py",
     "ADVANCEABLE = frozenset({FAST_FORWARD})",
     "ADVANCEABLE = frozenset({FAST_FORWARD, DIVERGED, REWRITTEN})",
     "a force-pushed or rewritten upstream history would be vendored as an ordinary update"),

    ("ancestry-always-fast-forward", "ancestry.py",
     "    if repo.is_ancestor(pinned, target):\n"
     "        return Ancestry(FAST_FORWARD, pinned, target, merge_base=pinned, **common)",
     "    if True:\n        return Ancestry(FAST_FORWARD, pinned, target, merge_base=pinned,"
     " **common)",
     "every history relation would be misreported as a clean fast-forward"),

    ("ancestry-block-removed", "analyze.py",
     "    if not analysis.ancestry.advanceable:",
     "    if False:",
     "a non-fast-forward would be classified and then acted on anyway"),

    ("ref-drift-ignored", "analyze.py",
     "    if analysis.ancestry.ref_drifted:",
     "    if False:",
     "tracking an abandoned branch would go unreported"),

    ("description-dropped-from-trigger-surface", "analyze.py",
     '    "name", "description", "when_to_use",',
     '    "name", "when_to_use",',
     "a reworded description changes when a skill fires and would pass as prose drift"),

    ("eval-classification-disabled", "analyze.py",
     "    _classify_eval(analysis)\n    return analysis",
     "    return analysis",
     "a behaviour-changing candidate would carry no EVAL_REQUIRED marker"),

    ("discovery-detection-disabled", "analyze.py",
     "    for name in sorted(set(after) - set(before)):",
     "    for name in []:",
     "new upstream skills would never be surfaced for review"),

    ("plugin-version-precedence-inverted", "plugins.py",
     'VERSION_SOURCES = ("plugin_json", "marketplace_json", "source_commit")',
     'VERSION_SOURCES = ("source_commit", "marketplace_json", "plugin_json")',
     "plugin versions would not resolve in Claude Code's documented order"),

    ("plugin-component-comparison-removed", "plugins.py",
     "        if rec_components[kind] != obs_components[kind]:",
     "        if False:",
     "a plugin gaining agents, hooks or MCP servers would go unreported"),

    ("licence-coverage-narrowed-to-one-layout", "analyze.py",
     "        base = posixpath.basename(entry[\"path\"])\n"
     "            if base == per_skill or base in LICENCE_BASENAMES:",
     "        base = posixpath.basename(entry[\"path\"])\n"
     "            if False:",
     "55 of 81 skills' licence notices would be compared against nothing"),

    ("licence-local-origin-skipped", "analyze.py",
     "            if base == per_skill or base in LICENCE_BASENAMES:",
     "            if base == per_skill and entry.get(\"origin\") == \"upstream\":",
     "the four providers that vendor the root LICENSE as origin=local lose licence coverage"),

    ("write-containment-removed", "candidate.py",
     "    if not normalized.startswith(WRITABLE_PREFIXES):",
     "    if False:",
     "a manifest path could write outside the vendored tree from a contents:write job"),

    ("issue-supersession-removed", "publish.py",
     "    stale = [issue for issue in adapter.find_issues_by_title_prefix(prefix, [label])\n"
     "             if issue.get(\"title\") != keep_title]",
     "    stale = []",
     "a persistent block would file one new issue per upstream commit"),

    ("blocked-admitted-to-artifact-plan", "plan.py",
     '        if state != "PREPARED_AUDIT_REQUIRED":\n            continue',
     '        if state == "NO_DRIFT":\n            continue',
     "a BLOCKED provider would be admitted to the artifact preparation matrix"),

    ("stable-report-identity-validation-removed", "plan.py",
     "    for entry in validate_report(report):",
     "    for entry in report[\"providers\"]:",
     "a missing or stale stable base/control digest would schedule artifact preparation"),

    ("prewrite-live-reverification-removed", "cli.py",
     "    verified = runtime.verify_workflow_environment(repo_root)\n"
     "    contract = verified.contract",
     "    verified = None\n    contract = verified.contract",
     "artifact bytes could be keyed to stable/default facts that drifted during analysis"),

    ("blocked-loses-to-not-drifted", "states.py",
     "    if blocked:\n        return BLOCKED\n    if not drifted:\n        return NO_DRIFT",
     "    if not drifted:\n        return NO_DRIFT\n    if blocked:\n        return BLOCKED",
     "an unresolvable upstream ref would report as NO_DRIFT -- silence instead of a report"),

    ("stale-upstream-version-carried-forward", "analyze.py",
     "    if \"upstream_version\" not in new_doc:\n        return",
     "    if True:\n        return",
     "a candidate would assert a version belonging to the superseded commit, invisibly"),

    # --- hosted runtime / managed-service executable closure --------------------------
    ("runtime-envelope-digest-check-removed", "runtime.py",
     "    if _canonical_json_sha256(runtime) != RUNTIME_ENVELOPE_SHA256:",
     "    if False:",
     "owner, licence, retention or executable drift in a runtime record would be accepted"),

    ("managed-service-digest-check-removed", "runtime.py",
     "    if _canonical_json_sha256(services) != SERVICE_DEPENDENCIES_SHA256:",
     "    if False:",
     "an unreviewed REST service retention or transitive field would be accepted"),

    ("runner-images-numeric-repository-id-check-removed", "runtime.py",
     "        payload.get(\"id\") != RUNNER_IMAGES_REPOSITORY_ID",
     "        False",
     "a recreated owner/name locator could masquerade as the reviewed runner-images repository"),

    ("node-execution-override-check-removed", "runtime.py",
     "_EXECUTION_OVERRIDE_ENV_KEYS = frozenset(\n    {\n        \"NODE_OPTIONS\",",
     "_EXECUTION_OVERRIDE_ENV_KEYS = frozenset(\n    {\n        \"IGNORED_NODE_OPTIONS\",",
     "ambient Node module injection could alter checkout before repository verification"),

    # --- final exact-HEAD Q3 repairs -----------------------------------------------------
    ("prepare-drops-the-resolved-default-ref", "cli.py",
     "                analysis = _analyze_one(provider, repo_root, workdir,\n"
     "                                        target_override=args.target_sha)",
     "                analysis = analyze.analyze_provider(\n"
     "                    provider, repo_root,\n"
     "                    gitio.fetch_commits(provider.upstream_repo,\n"
     "                                        [_pinned_of(provider),\n"
     "                                         ancestry.resolve(provider)[0]],\n"
     "                                        os.path.join(workdir, 'x-' + provider.key)),\n"
     "                    ancestry.resolve(provider)[0])",
     "check would report BLOCKED on ref drift while publish opened a Draft PR anyway"),

    ("target-override-trusted-without-proof", "cli.py",
     "        if target is not None and target_override != target:",
     "        if False:",
     "a dispatch input could vendor any commit in upstream's history"),

    ("nonterminal-analysis-not-closed", "cli.py",
     "    if states.terminal(item.state):\n        return item",
     "    if True:\n        return item",
     "a benign monitor-only advance would escape production as DRIFT_DETECTED"),

    ("equal-pin-default-ref-drift-ignored", "cli.py",
     "    if default_ref != provider.upstream_ref:",
     "    if False:",
     "an abandoned configured ref could report NO_DRIFT when its pin stayed equal"),

    ("branch-presence-short-circuits-pr-lookup", "publish.py",
     "    existing = adapter.find_pull_request(branch)\n"
     "    branch_present = adapter.branch_exists(branch)",
     "    branch_present = adapter.branch_exists(branch)\n"
     "    existing = {'number': 0} if branch_present else adapter.find_pull_request(branch)",
     "a pushed branch whose PR failed would report duplicate and exit 0 forever"),

    ("failed-publish-treated-as-success", "publish.py",
     'FAILED_PUBLISH_STATUSES = ("pushed", "pushed-no-pr")',
     'FAILED_PUBLISH_STATUSES = ()',
     "a branch on the remote with no pull request would exit the job green"),

    ("eval-exemption-inverted-to-a-trigger-list", "analyze.py",
     "        if _eval_exempt(path):\n            continue",
     "        if not path.endswith('SKILL.md'):\n            continue",
     "369 of 602 vendored files, including the instruction Markdown, would stop triggering an eval"),

    ("merged-authority-guard-removed", "analyze.py",
     "        if lost:\n            analysis.block(AUTHORITY,\n"
     "                           \"merging %s would change the vendored authority surface of %r\"",
     "        if False:\n            analysis.block(AUTHORITY,\n"
     "                           \"merging %s would change the vendored authority surface of %r\"",
     "a lost disable-model-invocation line would make a skill model-invocable again"),
]


def digest(path):
    with open(path, "rb") as handle:
        return hashlib.sha256(handle.read()).hexdigest()


def _run_suite_child():
    """Run the suite in this already-isolated direct-entrypoint process."""
    # The suite's deterministic repositories are local temporary fixtures.  Give only this
    # test-owned process boundary the one transport they need, even when the enclosing CI job
    # correctly exports an HTTPS-only production envelope.  Production updater/runtime Git
    # calls build their own closed environments and therefore do not inherit this allowance.
    outer_allow_protocol = os.environ.get("GIT_ALLOW_PROTOCOL")
    os.environ["GIT_ALLOW_PROTOCOL"] = "file"
    try:
        scripts = os.path.join(REPO, "scripts")
        # Do not depend on an ambient import path.  The reviewed repository scripts
        # directory is the one explicit non-stdlib package root admitted by the child.
        if scripts not in sys.path:
            sys.path.insert(0, scripts)
        suite = unittest.defaultTestLoader.discover(
            os.path.join(scripts, "skill_updates", "tests"),
            top_level_dir=scripts,
        )
        result = unittest.TextTestRunner(verbosity=1).run(suite)
        return 0 if result.wasSuccessful() else 1
    finally:
        if outer_allow_protocol is None:
            os.environ.pop("GIT_ALLOW_PROTOCOL", None)
        else:
            os.environ["GIT_ALLOW_PROTOCOL"] = outer_allow_protocol


def run_suite():
    """Run the unit suite in a fresh ``-I -S -B`` child. Returns True on PASS."""
    proc = subprocess.run(
        [sys.executable, "-I", "-S", "-B", os.path.abspath(__file__), "--suite-child"],
        cwd=REPO, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    return proc.returncode == 0


def _install_restore_handlers(original):
    """Restore on SIGTERM/SIGINT too, not just on a normal exit or an exception.

    Learned the hard way: a `finally` does not run when the process is killed, and a probe run
    that is killed mid-mutation leaves a deliberately broken source file in the working tree.
    That is the worst possible failure for this script -- a sabotaged file that looks like
    ordinary work. These handlers restore every target before re-raising the default action, so
    a timeout or a Ctrl-C leaves the tree exactly as it was found.
    """
    def restore_and_exit(signum, _frame):
        for path, data in original.items():
            with open(path, "wb") as handle:
                handle.write(data)
        sys.stderr.write("\nmutation_probe: signal %d -- restored %d file(s) before exiting\n"
                         % (signum, len(original)))
        signal.signal(signum, signal.SIG_DFL)
        os.kill(os.getpid(), signum)

    for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        try:
            signal.signal(sig, restore_and_exit)
        except (ValueError, OSError, AttributeError):
            pass  # not all signals exist or are settable everywhere; best effort


def check_anchors():
    """Assert every probe's anchor still matches its file exactly once.

    A stale anchor makes a probe SKIP, and a skipped probe is a silently missing guarantee --
    the run still reports "0 survived" while one safety property went unchecked. This runs in
    under a second, so there is no excuse for finding out the slow way.
    """
    problems = []
    for name, relfile, find, _replace, _why in PROBES:
        path = os.path.join(PKG, relfile)
        with open(path, encoding="utf-8") as handle:
            count = handle.read().count(find)
        if count != 1:
            problems.append("%-44s anchor matched %d times in %s" % (name, count, relfile))
    for line in problems:
        print("  STALE " + line)
    print("\n%d/%d probe anchors match exactly once" % (len(PROBES) - len(problems), len(PROBES)))
    return 1 if problems else 0


def main():
    if "--suite-child" in sys.argv:
        return _run_suite_child()
    if "--suite-only" in sys.argv:
        return _run_suite_child()
    if "--check-anchors" in sys.argv:
        return check_anchors()
    only = None
    if "--only" in sys.argv:
        only = sys.argv[sys.argv.index("--only") + 1]
    targets = sorted({os.path.join(PKG, probe[1]) for probe in PROBES})
    original = {path: open(path, "rb").read() for path in targets}
    before = {path: digest(path) for path in targets}
    _install_restore_handlers(original)

    print("baseline: verifying the suite is green before mutating anything")
    if not run_suite():
        print("ABORT: the suite is already failing; fix that before running probes")
        return 1

    killed, survived, skipped = [], [], []
    try:
        for name, relfile, find, replace, why in PROBES:
            if only and name != only:
                continue
            path = os.path.join(PKG, relfile)
            source = original[path].decode("utf-8")
            if source.count(find) != 1:
                skipped.append((name, "anchor matched %d times" % source.count(find)))
                print("  SKIP  %-40s anchor did not match exactly once" % name)
                continue
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(source.replace(find, replace, 1))
            passed = run_suite()
            with open(path, "wb") as handle:
                handle.write(original[path])
            if passed:
                survived.append((name, why))
                print("  SURVIVED %-37s <-- TEST GAP: %s" % (name, why))
            else:
                killed.append(name)
                print("  killed   %-37s" % name)
    finally:
        for path, data in original.items():
            with open(path, "wb") as handle:
                handle.write(data)

    print("\nrestore verification (byte-identical):")
    drift = [path for path in targets if digest(path) != before[path]]
    for path in targets:
        print("  %s  %s" % ("OK  " if digest(path) == before[path] else "DRIFT",
                            os.path.relpath(path, REPO)))
    print("\n%d killed, %d survived, %d skipped" % (len(killed), len(survived), len(skipped)))
    if drift:
        print("FAILED: files not restored byte-identically: %s" % drift)
        return 1
    if survived or skipped:
        return 1
    print("every probe was killed and every file restored byte-identically")
    return 0


if __name__ == "__main__":
    sys.exit(main())
