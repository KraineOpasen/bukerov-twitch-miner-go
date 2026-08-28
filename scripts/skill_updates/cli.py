"""Command-line surface for the skills update bot.

Two artifact-only commands, matching the two things the stable workflow needs:

    check    resolve every provider's reviewed ref, classify the drift, report. Read-only:
             it creates no branch, PR, issue or comment, and writes to the working tree only
             if asked for a summary file.
    prepare  produce one inert, audit-required artifact outside the tracked checkout.

Exit-code contract, which the workflow depends on:

    0   the tool did its job. Includes "no drift" (the common case) and a reported block.
    1   the tool could not do its job: bad config, unreadable manifest, git or API failure.
    2   command-line misuse (argparse's own convention).

`--fail-on-blocked` makes a reported block fail an ad-hoc run. G1.1 has no publication edge:
attempting the historical `--publish` option fails explicitly as UNCOMMISSIONED.
"""

import argparse
from contextlib import contextmanager
import hashlib
import os
import sys
import tempfile

from . import analyze, ancestry, candidate, config, gitio, plugins, report, runtime, states
from .errors import SkillUpdateError

CANDIDATE_REPORT = "candidate-report.json"

DONOR_GITIO_SHA256 = "42b003ce258d81143e0bcdff510f0797a1dc5049faae2616cbff07e1efd80ccf"
DONOR_GIT_HARDENING = (
    "-c", "core.hooksPath=/nonexistent",
    "-c", "core.symlinks=false",
    "-c", "core.autocrlf=false",
    "-c", "core.fsmonitor=false",
    "-c", "protocol.ext.allow=never",
    "-c", "advice.detachedHead=false",
    "-c", "gc.auto=0",
)
DONOR_UNTRUSTED_GIT_HARDENING = ("-c", "protocol.file.allow=never")
PRODUCTION_GIT_HARDENING = (
    "-c", "credential.helper=",
    "-c", "http.sslVerify=true",
    "-c", "http.followRedirects=false",
)
_DONOR_AMBIENT_ROUTE_KEYS = (
    "http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY", "all_proxy", "ALL_PROXY",
    "no_proxy", "NO_PROXY", "GIT_SSL_CAINFO", "SSL_CERT_FILE", "GIT_PROXY_SSL_CAINFO",
    "CURL_CA_BUNDLE",
)


@contextmanager
def _production_git_facade():
    """Harden every donor Git call without changing the frozen donor module.

    R0 requires ``gitio.py`` to remain byte-identical.  G1.1 nevertheless needs
    stronger production transport guarantees than the donor supplied.  The two
    production commands enter this scope before ref resolution and leave it only
    after every upstream object read has completed.  The frozen source digest and
    both donor argv lists are verified before any runtime patch is installed.
    """

    try:
        with open(gitio.__file__, "rb") as handle:
            donor_digest = hashlib.sha256(handle.read()).hexdigest()
    except OSError as exc:
        raise SkillUpdateError("cannot verify frozen donor gitio.py: %s" % exc) from exc
    if donor_digest != DONOR_GITIO_SHA256:
        raise SkillUpdateError("frozen donor gitio.py digest mismatch")
    if tuple(gitio._HARDENING) != DONOR_GIT_HARDENING:
        raise SkillUpdateError("frozen donor Git hardening argv mismatch")
    if tuple(gitio._UNTRUSTED_HARDENING) != DONOR_UNTRUSTED_GIT_HARDENING:
        raise SkillUpdateError("frozen donor untrusted Git hardening argv mismatch")

    original_hardening = gitio._HARDENING
    original_git_env = gitio._git_env
    with tempfile.TemporaryDirectory(prefix="stable-skills-git-home-") as isolated_home:
        os.chmod(isolated_home, 0o700)

        def production_git_env(untrusted=True):
            env = original_git_env(untrusted)
            env["HOME"] = isolated_home
            for key in _DONOR_AMBIENT_ROUTE_KEYS:
                env.pop(key, None)
            return env

        gitio._HARDENING = list(DONOR_GIT_HARDENING + PRODUCTION_GIT_HARDENING)
        gitio._git_env = production_git_env
        try:
            yield
        finally:
            gitio._git_env = original_git_env
            gitio._HARDENING = original_hardening


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


def _stable_run_identity(verified):
    """The full dynamic stable identity carried by every production report."""
    contract = verified.contract
    return {
        "subject_kind": "stable-run",
        "repository_id": contract.repository_id,
        "repository_full_name": contract.repository_full_name,
        "stable_branch": contract.stable_branch,
        "stable_base_sha": verified.selected_sha,
        "selected_ref": verified.selected_ref,
        "live_default_branch": verified.live_default_branch,
        "fetched_default_sha": verified.fetched_default_sha,
        "control_input_digest": contract.control_input_digest,
        "workflow_path": contract.workflow_path,
        "g1_1_mode": "artifact-only",
        "publication_authority": "UNCOMMISSIONED",
    }


def _new_artifact_root(repo_root, requested):
    """Return a new outside-checkout artifact root, refusing overwrite or containment."""
    root = os.path.realpath(repo_root)
    artifact = os.path.realpath(os.path.abspath(requested))
    try:
        inside = os.path.commonpath((root, artifact)) == root
    except ValueError:
        inside = False
    if inside:
        raise SkillUpdateError("artifact root must be outside the tracked repository")
    if os.path.lexists(artifact):
        raise SkillUpdateError("artifact root already exists; refusing to overwrite: %s"
                               % artifact)
    return artifact


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
    """The required --target-sha is not exactly what the reviewed ref points at."""


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
    if target is not None and default_ref is None:
        item = analyze.Analysis(
            provider.key,
            provider.upstream_repo,
            provider.upstream_ref,
            provider.baseline_commit or _pinned_of(provider),
            target,
            monitor_only=provider.monitor_only,
        )
        item.block(
            analyze.UNPROVABLE,
            "could not prove the upstream default branch for %s"
            % provider.upstream_repo,
            ["UNKNOWN default-branch identity cannot become NO_DRIFT or a candidate"],
        )
        return item
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
        return _close_g11_analysis(analyze.Analysis(
            provider.key, provider.upstream_repo, provider.upstream_ref, pinned, target,
            monitor_only=provider.monitor_only,
            ancestry=ancestry.Ancestry(ancestry.EQUAL, pinned, target, merge_base=pinned,
                                       configured_ref=provider.upstream_ref,
                                       default_ref=default_ref)), provider, default_ref)
    bare = os.path.join(workdir, "upstream-" + provider.key)
    repo = gitio.fetch_commits(provider.upstream_repo,
                               [sha for sha in (pinned, target) if sha], bare)
    if provider.monitor_only:
        item = analyze.analyze_monitor(provider, repo, target)
    else:
        item = analyze.analyze_provider(
            provider, repo_root, repo, target, default_ref=default_ref
        )
    return _close_g11_analysis(item, provider, default_ref)


def _pinned_of(provider):
    from . import manifest as M
    return M.load(provider.manifest_path)["upstream_commit"]


def _close_g11_analysis(item, provider, default_ref):
    """Close donor results into the exact three-state stable production contract.

    The donor has two intentionally quiet cases that are not valid terminal G1.1
    records: an audit-only monitor whose watched licence bytes did not move, and
    an equal pin whose configured ref stopped being the upstream default.  Keep
    every full target identity intact, but turn those cases into truthful
    fail-closed blocks.  Any future incomplete classifier result is closed the
    same way instead of leaking the internal ``DRIFT_DETECTED`` state.
    """

    if default_ref != provider.upstream_ref:
        if not any(reason.code == analyze.ANCESTRY for reason in item.blocked):
            item.block(
                analyze.ANCESTRY,
                "reviewed ref %r is no longer this repository's default branch (%r)"
                % (provider.upstream_ref, default_ref),
                [
                    "continuing to track %r may pin this project to an abandoned line "
                    "of development; confirm the intended branch and update "
                    "docs/agents/skills-update-providers.json deliberately"
                    % provider.upstream_ref,
                ],
            )
        item.new_manifest = None
    if states.terminal(item.state):
        return item
    code = analyze.AUTHORITY if item.monitor_only else analyze.UNPROVABLE
    item.block(
        code,
        "G1.1 cannot terminally classify unprepared drift for %s" % item.key,
        [
            "target remains bound to the resolved full commit; G1.1 cannot silently "
            "advance a reviewed pin or monitor baseline",
        ],
    )
    if not states.terminal(item.state):
        raise SkillUpdateError("failed to close G1.1 terminal state for %s" % item.key)
    return item


def cmd_check(args):
    repo_root = _repo_root(args.repo_root)
    verified = runtime.verify_workflow_environment(repo_root)
    stable_identity = _stable_run_identity(verified)
    providers = config.select(config.load(repo_root), args.provider)
    with _production_git_facade():
        with tempfile.TemporaryDirectory(prefix="skills-update-") as workdir:
            analyses = _analyze_all(providers, repo_root, workdir)
    # Native plugin monitoring. With the shipped (empty) inventory this reads one small JSON
    # file and returns nothing -- no Claude Code, no plugin cache, no network.
    plugin_drifts = plugins.check_inventory(repo_root)

    if args.json:
        sys.stdout.write(report.to_json(analyses, plugin_drifts, stable_identity))
    else:
        sys.stdout.write(report.text_report(analyses))
        sys.stdout.write(report.plugin_report(plugin_drifts))
    _write_if(args.summary, report.job_summary(analyses, "check")
              + report.plugin_report(plugin_drifts))
    if args.json_out:
        candidate.write_new_artifact(
            args.json_out, report.to_json(analyses, plugin_drifts, stable_identity))
    if args.fail_on_blocked and any(a.is_blocked for a in analyses):
        return 1
    return 0


def cmd_prepare(args):
    repo_root = _repo_root(args.repo_root)
    artifact_root = _new_artifact_root(repo_root, args.artifact_root)
    runtime.verify_workflow_environment(repo_root)
    providers = config.select(config.load(repo_root), args.provider)
    if len(providers) != 1:
        sys.stderr.write("prepare needs exactly one --provider (got %d)\n" % len(providers))
        return 2
    provider = providers[0]
    if provider.monitor_only:
        sys.stderr.write("monitor-only provider %s cannot produce a G1.1 artifact\n"
                         % provider.key)
        return 1

    with _production_git_facade():
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

    if analysis.is_blocked:
        sys.stdout.write("blocked: report-only; G1.1 has no issue publication authority\n")
        _write_if(args.summary, report.job_summary([analysis], "prepare"))
        return 1 if args.fail_on_blocked else 0

    if analysis.state != states.PREPARED_AUDIT_REQUIRED:
        sys.stderr.write("refusing non-terminal preparation state %s\n" % analysis.state)
        return 1

    # This is deliberately immediately before the first mkdir/write. Upstream analysis can take
    # time; re-verifying now prevents a stable/default-head change during analysis from being
    # hidden inside an artifact keyed to stale base facts.
    verified = runtime.verify_workflow_environment(repo_root)
    contract = verified.contract
    identity = runtime.candidate_identity(
        provider=provider.key,
        stable_branch=contract.stable_branch,
        stable_base_sha=verified.selected_sha,
        target_sha=analysis.target_sha,
        upstream_repo=provider.upstream_repo,
        old_pin=analysis.pinned_sha,
        control_input_digest=contract.control_input_digest,
        updater_source_sha=contract.updater_source_sha,
        pinned_action_digests=contract.pinned_action_digests,
    )
    if args.dry_run:
        paths = candidate.write(
            analysis, provider, artifact_root, identity, dry_run=True)
    else:
        candidate.create_artifact_root(artifact_root)
        paths = candidate.write(analysis, provider, artifact_root, identity)
        report_path = os.path.join(artifact_root, CANDIDATE_REPORT)
        candidate.write_new_artifact(
            report_path, report.to_json([analysis], stable_identity=identity))
    sys.stdout.write("candidate touches %d file(s)\n" % len(paths))
    for path in paths:
        sys.stdout.write("  %s\n" % path)

    sys.stdout.write("artifact-only: %s\n" % identity.proposal_id)
    sys.stdout.write("publication: UNCOMMISSIONED\n")
    _write_if(args.summary, report.job_summary([analysis], "prepare"))
    return 0


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
                    "audit-required update candidates. Shared options (--repo-root, --summary, "
                    "--fail-on-blocked) go AFTER the subcommand.")
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
                             help="prepare one inert audit-required artifact outside the repo")
    prepare.add_argument("--provider", required=True, help="provider key")
    prepare.add_argument("--target-sha", required=True,
                         help="target commit; must equal what the reviewed ref points at")
    prepare.add_argument("--artifact-root", required=True,
                         help="new outside-repository directory for candidate bytes/report")
    prepare.add_argument("--dry-run", action="store_true",
                         help="classify and report without writing files or calling GitHub")
    prepare.set_defaults(func=cmd_prepare)
    return parser


def main(argv=None):
    supplied = list(sys.argv[1:] if argv is None else argv)
    if "--publish" in supplied:
        sys.stderr.write("UNCOMMISSIONED: G1.1 has no publication authority or CLI route\n")
        return 1
    parser = build_parser()
    args = parser.parse_args(supplied)
    try:
        return args.func(args)
    except SkillUpdateError as exc:
        sys.stderr.write("error: %s\n" % exc)
        return 1


if __name__ == "__main__":
    sys.exit(main())
