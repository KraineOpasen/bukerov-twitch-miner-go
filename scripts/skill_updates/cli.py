"""Command-line surface for the skills update bot.

Two commands, matching the two things the workflow needs:

    check    resolve every provider's reviewed ref, classify the drift, report. Read-only:
             it creates no branch, PR, issue or comment, and writes to the working tree only
             if asked for a summary file.
    prepare  produce the candidate for one provider, and optionally publish it.

Exit-code contract, which the workflow depends on:

    0   the tool did its job. Includes "no drift" (the common case) and "blocked, issue
        recorded" -- a blocked provider is the bot working correctly, not a failure.
    1   the tool could not do its job: bad config, unreadable manifest, git or API failure.
    2   command-line misuse (argparse's own convention).

Blocked-but-handled deliberately does NOT exit non-zero. A red scheduled workflow trains people
to ignore it; the signal for "a human is needed" is the issue, which is durable and assignable.
`--fail-on-blocked` exists for anyone who wants the opposite in an ad-hoc run.
"""

import argparse
import os
import sys
import tempfile

from . import (analyze, ancestry, candidate, config, ghadapter, gitio, plugins, publish,
               report)
from .errors import SkillUpdateError

DEFAULT_BASE_BRANCH = "main"


def _repo_root(explicit=None):
    """Repository root: this file is scripts/skill_updates/cli.py, so climb three levels."""
    if explicit:
        return os.path.abspath(explicit)
    here = os.path.abspath(__file__)
    return os.path.dirname(os.path.dirname(os.path.dirname(here)))


def _write_if(path, text):
    """Write `text` to `path` when a path was given. Appends, so several steps can contribute
    to one GitHub step-summary file without clobbering each other."""
    if not path:
        return
    with open(path, "a", encoding="utf-8") as handle:
        handle.write(text)


def _analyze_all(providers, repo_root, workdir):
    """Resolve and classify every provider. One provider's failure never aborts the rest.

    Each provider gets its own bare upstream clone under `workdir`, keyed by provider key, so
    two providers can never share an object database and a fetch failure for one cannot
    silently satisfy a lookup for another.
    """
    analyses = []
    for provider in providers:
        try:
            analyses.append(_analyze_one(provider, repo_root, workdir))
        except SkillUpdateError as exc:
            # One unreachable or malformed provider must never cost the other six their report.
            # The failure becomes that provider's own UNPROVABLE block, which is exactly how a
            # human finds out about it -- an exception escaping here would produce no text
            # report, no job summary and no JSON for anybody.
            item = analyze.Analysis(provider.key, provider.upstream_repo, provider.upstream_ref,
                                    provider.baseline_commit or "?", None,
                                    monitor_only=provider.monitor_only)
            item.block(analyze.UNPROVABLE,
                       "could not analyze %s" % provider.key, [str(exc)])
            analyses.append(item)
    return analyses


class _TargetRefused(Exception):
    """An explicitly supplied --target-sha is not what the reviewed ref points at."""


def _analyze_one(provider, repo_root, workdir, target_override=None):
    """Resolve and classify exactly one provider. Raises on failure; the caller isolates it.

    This is the ONLY place a provider is classified. Both `check` and `prepare` call it, so the
    two phases cannot reach different verdicts for the same upstream state -- which is exactly
    what happened when `prepare` had its own copy and dropped the resolved default branch.

    `target_override` pins the target commit (the `--target-sha` dispatch input). It is still
    proved against the reviewed ref rather than trusted: accepting an arbitrary commit would let
    a dispatch input vendor any object in upstream's history -- an abandoned branch, an
    unreviewed fork merge. The default-branch lookup happens on this path too, so it is not a
    second bypass.
    """
    target, default_ref, error = ancestry.resolve(provider)
    if target_override is not None:
        gitio.validate_sha(target_override, "target")
        if target is not None and target_override != target:
            raise _TargetRefused(
                "refusing --target-sha %s: reviewed ref %r on %s currently points at %s"
                % (target_override, provider.upstream_ref, provider.upstream_repo, target))
    if target is None:
        item = analyze.Analysis(provider.key, provider.upstream_repo, provider.upstream_ref,
                                provider.baseline_commit or "?", None,
                                monitor_only=provider.monitor_only)
        item.block(analyze.UNPROVABLE,
                   "could not resolve ref %r on %s" % (provider.upstream_ref,
                                                       provider.upstream_repo),
                   [error or "no reason reported"])
        return item
    pinned = provider.baseline_commit if provider.monitor_only else _pinned_of(provider)
    if target == pinned:
        # Nothing moved: skip the fetch entirely. The common case must cost one ls-remote per
        # provider and nothing else -- a daily job that clones six repositories to discover
        # that none of them changed is a job people turn off.
        return analyze.Analysis(
            provider.key, provider.upstream_repo, provider.upstream_ref, pinned, target,
            monitor_only=provider.monitor_only,
            ancestry=ancestry.Ancestry(ancestry.EQUAL, pinned, target, merge_base=pinned,
                                       configured_ref=provider.upstream_ref,
                                       default_ref=default_ref))
    bare = os.path.join(workdir, "upstream-" + provider.key)
    repo = gitio.fetch_commits(provider.upstream_repo,
                               [sha for sha in (pinned, target) if sha], bare)
    if provider.monitor_only:
        return analyze.analyze_monitor(provider, repo, target)
    return analyze.analyze_provider(provider, repo_root, repo, target, default_ref=default_ref)


def _pinned_of(provider):
    from . import manifest as M
    return M.load(provider.manifest_path)["upstream_commit"]


def cmd_check(args):
    repo_root = _repo_root(args.repo_root)
    providers = config.select(config.load(repo_root, args.config), args.provider)
    with tempfile.TemporaryDirectory(prefix="skills-update-") as workdir:
        analyses = _analyze_all(providers, repo_root, workdir)
    # Native plugin monitoring. With the shipped (empty) inventory this reads one small JSON
    # file and returns nothing -- no Claude Code, no plugin cache, no network.
    plugin_drifts = plugins.check_inventory(repo_root)

    if args.json:
        sys.stdout.write(report.to_json(analyses, plugin_drifts))
    else:
        sys.stdout.write(report.text_report(analyses))
        sys.stdout.write(report.plugin_report(plugin_drifts))
    _write_if(args.summary, report.job_summary(analyses, "check")
              + report.plugin_report(plugin_drifts))
    if args.json_out:
        with open(args.json_out, "w", encoding="utf-8") as handle:
            handle.write(report.to_json(analyses, plugin_drifts))
    if args.fail_on_blocked and any(a.is_blocked for a in analyses):
        return 1
    return 0


def cmd_prepare(args):
    repo_root = _repo_root(args.repo_root)
    providers = config.select(config.load(repo_root, args.config), args.provider)
    if len(providers) != 1:
        sys.stderr.write("prepare needs exactly one --provider (got %d)\n" % len(providers))
        return 2
    provider = providers[0]
    adapter = None
    if args.publish:
        adapter = ghadapter.from_env()

    if provider.monitor_only:
        # An audit-only provider can never produce a candidate -- nothing from it is vendored --
        # but it CAN block: a licence appearing in a repository we rejected on provenance
        # grounds is exactly the finding worth an issue. So it takes the blocked path and only
        # the blocked path; there is no code route from here to a file write.
        return _prepare_monitor(args, provider, adapter)

    with tempfile.TemporaryDirectory(prefix="skills-update-") as workdir:
        # ONE classification path, shared with `check`. An earlier version re-implemented this
        # here and bound the resolved default branch to a throwaway, which silently disabled the
        # ref-drift ANCESTRY block in the only job that actually writes anything: `check` reported
        # BLOCKED and `publish` opened a Draft PR for the same upstream state, in the same run.
        # Two code paths that must agree is the bug; sharing the function is the fix, so a future
        # argument cannot be dropped on one side only.
        try:
            analysis = _analyze_one(provider, repo_root, workdir,
                                    target_override=args.target_sha)
        except _TargetRefused as exc:
            sys.stderr.write("%s\n" % exc)
            return 1

    if not analysis.drifted and not analysis.is_blocked:
        sys.stdout.write("%s is already at %s; nothing to prepare\n"
                         % (provider.key, analysis.pinned_sha))
        _write_if(args.summary, "`%s` is already at `%s`. No action taken.\n"
                  % (provider.key, analysis.pinned_sha[:12]))
        return 0

    sys.stdout.write(report.text_report([analysis]))

    # A discovery is reported whether or not the refresh itself proceeds: new sibling skills are
    # news in their own right, and deliberately do NOT gate the provider's other updates.
    if analysis.discoveries and adapter is not None:
        sys.stdout.write("discovery: %s\n" % publish.publish_discovery(
            analysis, provider, adapter, dry_run=args.dry_run))

    if analysis.is_blocked:
        result = {"status": "blocked-dry-run"}
        if adapter is not None:
            result = publish.publish_blocked(analysis, provider, adapter, dry_run=args.dry_run)
        sys.stdout.write("blocked: %s\n" % result)
        _write_if(args.summary, report.job_summary([analysis], "prepare"))
        return 1 if args.fail_on_blocked else 0

    paths = candidate.write(analysis, provider, repo_root, dry_run=args.dry_run)
    sys.stdout.write("candidate touches %d file(s)\n" % len(paths))
    for path in paths:
        sys.stdout.write("  %s\n" % path)

    if adapter is not None:
        result = publish.publish_candidate(repo_root, provider, analysis, paths, adapter,
                                           args.base_branch, dry_run=args.dry_run)
        sys.stdout.write("publish: %s\n" % result)
        if result.get("status") in publish.FAILED_PUBLISH_STATUSES:
            # The branch is safe on the remote; only PR creation failed. Fail LOUDLY every time
            # until a PR exists -- a candidate nobody can see is not published, and reporting
            # success here is exactly how the bot would go silently dark for a provider.
            sys.stderr.write(result["remedy"] + "\n")
            _write_if(args.summary,
                      "### `%s` branch pushed, pull request NOT created\n\n%s\n"
                      % (provider.key, result["remedy"]))
            return 1
    _write_if(args.summary, report.job_summary([analysis], "prepare"))
    return 0


def _prepare_monitor(args, provider, adapter):
    """The `prepare` path for an audit-only provider: report, and open an issue if blocked.

    Never writes to the working tree. `candidate.write()` is not reachable from here, which is
    the structural version of the promise that a monitored-but-not-vendored repository can
    never be vendored by automation.
    """
    with tempfile.TemporaryDirectory(prefix="skills-update-") as workdir:
        target, _default_ref, error = ancestry.resolve(provider)
        if target is None:
            # Same rule as a vendored provider: an unprovable ref is a BLOCKED outcome that gets
            # its issue, not a silent non-zero exit. A monitored repository going unreachable is
            # itself worth a human knowing about.
            item = analyze.Analysis(provider.key, provider.upstream_repo, provider.upstream_ref,
                                    provider.baseline_commit, None, monitor_only=True)
            item.block(analyze.UNPROVABLE,
                       "could not resolve ref %r on %s" % (provider.upstream_ref,
                                                           provider.upstream_repo),
                       [error or "no reason reported"])
            sys.stdout.write(report.text_report([item]))
            if adapter is not None:
                sys.stdout.write("blocked: %s\n" % publish.publish_blocked(
                    item, provider, adapter, dry_run=args.dry_run))
            _write_if(args.summary, report.job_summary([item], "prepare"))
            return 1 if args.fail_on_blocked else 0
        if target == provider.baseline_commit:
            sys.stdout.write("%s is at its reviewed baseline; nothing to report\n" % provider.key)
            return 0
        repo = gitio.fetch_commits(provider.upstream_repo,
                                   [provider.baseline_commit, target],
                                   os.path.join(workdir, "upstream-" + provider.key))
        analysis = analyze.analyze_monitor(provider, repo, target)

    sys.stdout.write(report.text_report([analysis]))
    if not analysis.is_blocked:
        _write_if(args.summary, report.job_summary([analysis], "prepare"))
        return 0
    result = {"status": "blocked-dry-run"}
    if adapter is not None:
        result = publish.publish_blocked(analysis, provider, adapter, dry_run=args.dry_run)
    sys.stdout.write("blocked: %s\n" % result)
    _write_if(args.summary, report.job_summary([analysis], "prepare"))
    return 1 if args.fail_on_blocked else 0


def _common_options():
    """Options every subcommand accepts, attached via `parents=`.

    They live on the SUBCOMMANDS, not on the top-level parser. argparse requires a parent-level
    option to appear BEFORE the subcommand, and both entry-point wrappers
    (`check-skill-updates.py`, `prepare-skill-update.py`) prepend the subcommand to whatever the
    user typed -- so `check-skill-updates.py --summary x` becomes `["check", "--summary", "x"]`,
    which a top-level-only option would reject.

    Attaching them to both would not fix it either: a subparser re-applies its own defaults over
    the namespace, so a value given before the subcommand is silently replaced by None. That is
    worse than an error -- `--summary` would appear to work and quietly write nothing. One
    definition site, after the subcommand, is the only shape with no silent failure mode.
    """
    common = argparse.ArgumentParser(add_help=False)
    common.add_argument("--repo-root", help="repository root (default: this file's repo)")
    common.add_argument("--config", help="path to skills-update-providers.json")
    common.add_argument("--summary", help="append a Markdown job summary to this file")
    common.add_argument("--fail-on-blocked", action="store_true",
                        help="exit non-zero when any provider is blocked (default: exit 0, "
                             "because a blocked provider is a recorded outcome, not a failure)")
    return common


def build_parser():
    common = _common_options()
    parser = argparse.ArgumentParser(
        prog="skill-updates",
        description="Detect upstream drift in vendored skill providers and prepare "
                    "audit-required update candidates. Shared options (--repo-root, --config, "
                    "--summary, --fail-on-blocked) go AFTER the subcommand.")
    sub = parser.add_subparsers(dest="command", required=True)

    check = sub.add_parser("check", parents=[common],
                           help="report drift; never writes to GitHub")
    group = check.add_mutually_exclusive_group()
    group.add_argument("--all", dest="provider", action="store_const", const="all",
                       help="check every configured provider (default)")
    group.add_argument("--provider", dest="provider", help="check one provider by key")
    check.add_argument("--json", action="store_true", help="emit JSON on stdout")
    check.add_argument("--json-out", help="also write JSON to this path")
    check.set_defaults(func=cmd_check, provider="all")

    prepare = sub.add_parser("prepare", parents=[common],
                             help="prepare (and optionally publish) one candidate")
    prepare.add_argument("--provider", required=True, help="provider key")
    prepare.add_argument("--target-sha",
                         help="target commit; must equal what the reviewed ref points at")
    prepare.add_argument("--dry-run", action="store_true",
                         help="classify and report without writing files or calling GitHub")
    prepare.add_argument("--publish", action="store_true",
                         help="push the branch and open a Draft PR, or record a blocked issue")
    prepare.add_argument("--base-branch", default=DEFAULT_BASE_BRANCH)
    prepare.set_defaults(func=cmd_prepare)
    return parser


def main(argv=None):
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except SkillUpdateError as exc:
        sys.stderr.write("error: %s\n" % exc)
        return 1


if __name__ == "__main__":
    sys.exit(main())
