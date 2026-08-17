"""Turning a prepared candidate into a branch, a Draft PR, or a blocked-update issue.

This is the only place local git *writes* happen, and it is bounded by three rules that the
code enforces structurally rather than by convention:

* **Never force.** No call here passes `--force`, `--force-with-lease`, `+refspec`, or
  `--delete`. A branch that already exists is a signal to stop, not to overwrite: it means this
  exact provider-and-target candidate was already published, possibly reviewed, possibly
  amended by a human.

* **Never the default branch.** Every push targets a `automated/skills-update/...` ref computed
  by `candidate.branch_name()` from an allowlisted provider key and a validated SHA. The base
  branch is only ever read.

* **Deduplicate before creating.** Branch, PR and issue are each checked for existence first,
  so a re-run over an unchanged situation performs no writes at all and reports that plainly.

Commit identity is passed per-invocation with `-c`, never written into repository config, so
the bot leaves no configuration residue in a checkout that other jobs may reuse.
"""

from . import report
from .candidate import (PR_BANNER, blocked_title_prefix, branch_name, discovery_title,
                        discovery_title_prefix, issue_title)
from .errors import AdapterError
from .gitio import run_git

#: Identity for bot commits. The `github-actions[bot]` address is GitHub's own no-reply for
#: workflow-authored commits, so the authorship is honest about being machine-made.
BOT_NAME = "github-actions[bot]"
BOT_EMAIL = "41898282+github-actions[bot]@users.noreply.github.com"

#: Label applied to blocked-update issues. Purely advisory; issue creation still succeeds if
#: the label does not exist, because GitHub creates unknown labels on demand.
BLOCKED_LABEL = "skills-update-blocked"

#: Label for new-sibling-skill issues. Distinct from BLOCKED_LABEL because the two mean opposite
#: things about whether the provider can still be updated.
DISCOVERY_LABEL = "skills-update-discovery"

COMMIT_SUBJECT = "chore(skills): automated update candidate for %s -> %s"


def _git(repo_root, args, **kw):
    """git against THIS repository (not an upstream), so the untrusted-transport ban is off."""
    kw.setdefault("untrusted", False)
    return run_git(["-C", repo_root] + list(args), **kw)


def current_branch(repo_root):
    return _git(repo_root, ["rev-parse", "--abbrev-ref", "HEAD"]).strip()


def commit_candidate(repo_root, provider, analysis, paths, base_branch):
    """Create the candidate branch and commit `paths` onto it.

    Returns the branch name. Assumes `candidate.write()` has already produced the files; this
    function only records them. `git switch -c` fails if the branch already exists locally,
    which is the desired behaviour -- the caller checks the remote first.
    """
    branch = branch_name(provider.key, analysis.target_sha)
    _git(repo_root, ["switch", "--quiet", "--create", branch])
    # `--` terminates option parsing, so a path can never be read as a flag. Paths here are
    # manifest-declared repo-relative strings, but the guard costs nothing and removes the
    # question entirely.
    _git(repo_root, ["add", "--"] + list(paths))
    body = (
        "%s\n\n"
        "Prepared by .github/workflows/skills-update.yml from the reviewed ref %r.\n"
        "Superseded pin: %s\nTarget commit:   %s\n\n"
        "The manifest carries an automated_candidate block; "
        "scripts/validate-agent-governance.py fails while it is present, so this cannot pass "
        "the governance gate without a deliberate human audit.\n"
        % (PR_BANNER, provider.upstream_ref, analysis.pinned_sha, analysis.target_sha))
    _git(repo_root, [
        "-c", "user.name=" + BOT_NAME, "-c", "user.email=" + BOT_EMAIL,
        "commit", "--quiet", "--no-verify",
        "-m", COMMIT_SUBJECT % (provider.key, analysis.target_sha[:12]),
        "-m", body])
    return branch


def push_branch(repo_root, branch):
    """Push the candidate branch. Never forced, never to the default branch."""
    _git(repo_root, ["push", "--quiet", "--set-upstream", "origin",
                     "refs/heads/%s:refs/heads/%s" % (branch, branch)])
    return branch


def publish_candidate(repo_root, provider, analysis, paths, adapter, base_branch,
                      dry_run=False):
    """Publish one prepared candidate. Returns a result dict describing what happened.

    `status` is one of:
        "duplicate"   a branch or PR for this provider/target already exists -- nothing done
        "dry-run"     everything checked, nothing written
        "published"   branch pushed and Draft PR opened
        "pushed"      branch pushed but the PR could not be created (see `remedy`)
    """
    branch = branch_name(provider.key, analysis.target_sha)
    if adapter.branch_exists(branch):
        return {"status": "duplicate", "branch": branch,
                "reason": "branch already exists on the remote"}
    existing = adapter.find_pull_request(branch)
    if existing:
        return {"status": "duplicate", "branch": branch, "pull_request": existing.get("number"),
                "reason": "a pull request for this branch already exists"}
    if dry_run:
        return {"status": "dry-run", "branch": branch, "files": list(paths)}

    commit_candidate(repo_root, provider, analysis, paths, base_branch)
    push_branch(repo_root, branch)
    title = "chore(skills): %s update candidate %s" % (provider.key, analysis.target_sha[:8])
    try:
        pull = adapter.create_pull_request(
            head=branch, base=base_branch, title=title,
            body=report.pr_body(analysis, provider), draft=True)
    except AdapterError as exc:
        if exc.status == 403:
            return {"status": "pushed", "branch": branch, "remedy": str(exc)}
        raise
    return {"status": "published", "branch": branch, "pull_request": pull.get("number"),
            "url": pull.get("html_url")}


def _supersede_stale(adapter, prefix, keep_title, label, target_sha, dry_run):
    """Close older open issues under `prefix` that are about a SUPERSEDED target commit.

    Without this the bot accumulates one issue per upstream head. The titles embed the target
    SHA -- which is what makes them deduplicate correctly for a single head -- so every new
    upstream commit under a persistent condition produces a *new* title and a *new* issue. Now
    that a reworded `description` blocks (it is trigger surface), an actively maintained
    provider would file one every day until someone acted.

    Superseding rather than reusing one title keeps each issue's evidence pinned to the exact
    commit it was written about, while leaving only the newest one open.
    """
    stale = [issue for issue in adapter.find_issues_by_title_prefix(prefix, [label])
             if issue.get("title") != keep_title]
    closed = []
    for issue in stale:
        if dry_run:
            closed.append(issue.get("number"))
            continue
        adapter.close_issue(issue["number"], comment=(
            "Superseded: upstream has moved on and the current state is tracked in **%s**. "
            "Closing this one so a persistent condition does not accumulate an issue per "
            "upstream commit. Nothing was resolved by this comment." % keep_title))
        closed.append(issue.get("number"))
    return closed


def publish_discovery(analysis, provider, adapter, dry_run=False):
    """Open or refresh the deduplicated `DISCOVERY_REQUIRED` issue for new sibling skills.

    Separate from `publish_blocked` on purpose: a discovery does NOT block the provider's other
    updates, so it must not share the blocked issue's title or lifecycle. Its own title keys on
    provider and target commit, so a run that finds the same new skills again leaves the issue
    untouched.
    """
    if not analysis.discoveries:
        return {"status": "none"}
    title = discovery_title(provider.key, analysis.target_sha)
    body = report.discovery_issue_body(analysis, provider)
    existing = adapter.find_issue_by_title(title, [DISCOVERY_LABEL])
    superseded = _supersede_stale(adapter, discovery_title_prefix(provider.key), title,
                                  DISCOVERY_LABEL, analysis.target_sha, dry_run)
    if dry_run:
        return {"status": "dry-run", "issue_title": title, "superseded": superseded,
                "would": "update" if existing else "create"}
    if existing:
        if (existing.get("body") or "").strip() == body.strip():
            return {"status": "unchanged", "issue": existing.get("number"),
                    "issue_title": title, "superseded": superseded}
        adapter.update_issue(existing["number"], body)
        return {"status": "updated", "issue": existing.get("number"), "issue_title": title,
                "superseded": superseded}
    issue = adapter.create_issue(title, body, labels=[DISCOVERY_LABEL])
    return {"status": "created", "issue": issue.get("number"), "issue_title": title,
            "superseded": superseded, "url": issue.get("html_url")}


def publish_blocked(analysis, provider, adapter, dry_run=False):
    """Open or refresh the single deduplicated issue for a blocked update.

    An existing issue whose body already matches is left completely alone -- not re-edited --
    so a persistent blocking condition does not re-notify subscribers on every daily run. That
    is why `report.issue_body()` is required to be free of timestamps.
    """
    title = issue_title(provider.key, analysis.target_sha) if analysis.target_sha else (
        "Skills update blocked: %s -> unresolved ref" % provider.key)
    body = report.issue_body(analysis, provider)
    existing = adapter.find_issue_by_title(title, [BLOCKED_LABEL])
    superseded = _supersede_stale(adapter, blocked_title_prefix(provider.key), title,
                                  BLOCKED_LABEL, analysis.target_sha, dry_run)
    if dry_run:
        return {"status": "dry-run", "issue_title": title, "superseded": superseded,
                "would": "update" if existing else "create"}
    if existing:
        if (existing.get("body") or "").strip() == body.strip():
            return {"status": "unchanged", "issue": existing.get("number"),
                    "issue_title": title, "superseded": superseded}
        adapter.update_issue(existing["number"], body)
        return {"status": "updated", "issue": existing.get("number"), "issue_title": title,
                "superseded": superseded}
    issue = adapter.create_issue(title, body, labels=[BLOCKED_LABEL])
    return {"status": "created", "issue": issue.get("number"), "issue_title": title,
            "superseded": superseded, "url": issue.get("html_url")}
