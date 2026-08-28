"""The only component in this package that talks to GitHub.

Kept deliberately thin, for one reason: everything above it -- drift detection, merging, the
blocked-condition classifier, provenance regeneration, report rendering -- is a pure function of
bytes, and the test suite drives all of it against temporary local git repositories. The moment
GitHub knowledge leaks upward, that stops being true. So this module owns the entire surface
(five verbs), and `FakeGitHub` implements the same surface for tests.

Implemented on `urllib.request` rather than the `gh` CLI: stdlib-only means the bot has no
dependency to install, pin, or audit, and no external binary whose output format could change
underneath it.

Two behaviours are worth calling out because they encode requirements rather than taste:

* **Deduplication is a read, not a bookkeeping file.** Before creating anything, the adapter is
  asked whether a branch/PR/issue for this exact provider-and-target already exists. State
  lives in GitHub, so a re-run is naturally idempotent and there is nothing to get stale.

* **The pull-request-permission failure is diagnosed, not worked around.** When a repository has
  "Allow GitHub Actions to create and approve pull requests" disabled, PR creation returns 403.
  That is reported with the exact setting a human must change. The bot never mutates repository
  settings, and never falls back to some other way of publishing.
"""

import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request

from .errors import AdapterError

API_ROOT = "https://api.github.com"

#: Substrings GitHub uses in the 403 body when Actions is not allowed to open pull requests.
#: Matched case-insensitively against the message; the wording has changed before, so two
#: independent fragments are checked rather than one exact string.
PR_PERMISSION_MARKERS = (
    "not permitted to create or approve pull requests",
    "workflows are not allowed to create pull requests",
    "github actions is not permitted",
)

#: The exact remediation to print when that 403 is seen. Named in full because the person
#: reading a failed workflow log should not have to go looking for it.
PR_PERMISSION_REMEDY = (
    "GitHub refused to let this workflow open a pull request. A repository (or organisation) "
    "administrator must enable:\n"
    "  Settings -> Actions -> General -> Workflow permissions\n"
    "    [x] Allow GitHub Actions to create and approve pull requests\n"
    "This bot does not change repository settings. Until the setting is enabled, the update "
    "branch is still pushed and can be opened as a pull request by hand."
)


class GitHubAdapter:
    """REST client scoped to one repository, authenticated with GITHUB_TOKEN."""

    def __init__(self, owner, repo, token, api_root=API_ROOT, timeout=30):
        self.owner = owner
        self.repo = repo
        self.api_root = api_root.rstrip("/")
        self.timeout = timeout
        self._token = token if isinstance(token, str) else ""
        if not self._token:
            raise AdapterError("an explicit GitHub token is required by the uncommissioned "
                               "publication library")

    #: Statuses worth one more attempt: GitHub's transient 5xx and its rate limiters. A single
    #: blip on PR creation used to be enough to leave a branch on the remote with no pull
    #: request, so retrying cheaply here removes the common cause of that state.
    RETRY_STATUSES = (429, 500, 502, 503, 504)
    RETRY_ATTEMPTS = 3
    RETRY_BACKOFF_SECONDS = (1, 4)

    def _request(self, method, path, payload=None, params=None):
        last = None
        for attempt in range(self.RETRY_ATTEMPTS):
            try:
                return self._request_once(method, path, payload, params)
            except AdapterError as exc:
                # Only transport-level transients are retried. A 403/404/422 is a real answer
                # about authority or input and retrying it would just repeat the same mistake.
                if exc.status not in self.RETRY_STATUSES or attempt == self.RETRY_ATTEMPTS - 1:
                    raise
                last = exc
                time.sleep(self.RETRY_BACKOFF_SECONDS[
                    min(attempt, len(self.RETRY_BACKOFF_SECONDS) - 1)])
        raise last  # unreachable; kept so the loop has no implicit None return

    def _request_once(self, method, path, payload=None, params=None):
        url = "%s/repos/%s/%s%s" % (self.api_root, self.owner, self.repo, path)
        if params:
            url += "?" + urllib.parse.urlencode(params)
        data = json.dumps(payload).encode("utf-8") if payload is not None else None
        request = urllib.request.Request(url, data=data, method=method)
        request.add_header("Accept", "application/vnd.github+json")
        request.add_header("X-GitHub-Api-Version", "2022-11-28")
        request.add_header("User-Agent", "bukerov-skills-update-bot")
        request.add_header("Authorization", "Bearer %s" % self._token)
        if data is not None:
            request.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                body = response.read().decode("utf-8")
                return json.loads(body) if body.strip() else {}
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", "replace")[:600]
            # The token must never reach a log, an exception message, or a job summary. Only
            # the method, path and status are echoed -- never the request headers.
            if exc.code == 403 and any(m in detail.lower() for m in PR_PERMISSION_MARKERS):
                raise AdapterError(PR_PERMISSION_REMEDY, status=403)
            raise AdapterError("GitHub %s %s failed: HTTP %d: %s"
                               % (method, path, exc.code, detail), status=exc.code)
        except urllib.error.URLError as exc:
            raise AdapterError("GitHub %s %s failed: %s" % (method, path, exc.reason))

    def branch_exists(self, name):
        try:
            self._request("GET", "/git/ref/heads/" + urllib.parse.quote(name))
            return True
        except AdapterError as exc:
            if exc.status == 404:
                return False
            raise

    def find_pull_request(self, head_branch):
        """Any PR (open or closed) whose head is `head_branch`, else None.

        Closed and merged PRs count for deduplication. A candidate whose PR was closed
        unmerged was refused by a human; re-opening it every night would be the bot arguing
        with a decision it has no standing to revisit.
        """
        results = self._request("GET", "/pulls", params={
            "head": "%s:%s" % (self.owner, head_branch), "state": "all", "per_page": "5"})
        return results[0] if results else None

    def create_pull_request(self, head, base, title, body, draft=True):
        return self._request("POST", "/pulls", {
            "title": title, "head": head, "base": base, "body": body, "draft": bool(draft)})

    def open_issues(self, labels=None):
        """Every open issue, following pagination. PRs are filtered out.

        Pagination is not optional here even though this repository currently has no open
        issues: `/issues` returns open PULL REQUESTS too, so a busy repository can push a bot
        issue off page one, and the bot would then create a duplicate on every daily run while
        its own issue body promises it will not. Optionally narrowed by `labels`, which the bot
        always sets on its own issues.
        """
        collected = []
        page = 1
        while page <= 20:  # a hard stop; 2000 open issues means something else is wrong
            params = {"state": "open", "per_page": "100", "page": str(page)}
            if labels:
                params["labels"] = ",".join(labels)
            batch = self._request("GET", "/issues", params=params)
            if not batch:
                break
            collected.extend(item for item in batch if "pull_request" not in item)
            if len(batch) < 100:
                break
            page += 1
        return collected

    def find_issue_by_title(self, title, labels=None):
        """An open issue with exactly this title, else None.

        Title equality is the deduplication key, and the title embeds provider and target SHA,
        so it is stable for as long as the blocking condition is about the same update.
        """
        for item in self.open_issues(labels):
            if item.get("title") == title:
                return item
        return None

    def find_issues_by_title_prefix(self, prefix, labels=None):
        """Open issues whose title starts with `prefix` -- used to supersede stale ones."""
        return [item for item in self.open_issues(labels)
                if (item.get("title") or "").startswith(prefix)]

    def close_issue(self, number, comment=None):
        """Close an issue, optionally leaving a comment saying why."""
        if comment:
            self._request("POST", "/issues/%d/comments" % int(number), {"body": comment})
        return self._request("PATCH", "/issues/%d" % int(number),
                             {"state": "closed", "state_reason": "completed"})

    def create_issue(self, title, body, labels=None):
        payload = {"title": title, "body": body}
        if labels:
            payload["labels"] = list(labels)
        return self._request("POST", "/issues", payload)

    def update_issue(self, number, body):
        return self._request("PATCH", "/issues/%d" % int(number), {"body": body})


class FakeGitHub:
    """In-memory stand-in with identical semantics, for tests.

    Records every call in `self.calls` so a test can assert not just the end state but that the
    bot did not, for instance, create a second PR or force anything. Deliberately mirrors the
    real adapter's dedup rules (closed PRs still count, issues match on exact title) -- a fake
    that were more permissive than production would let a dedup regression pass.
    """

    def __init__(self, owner="o", repo="r"):
        self.owner = owner
        self.repo = repo
        self.branches = set()
        self.pulls = []
        self.issues = []
        self.calls = []
        self.fail_pr_permission = False

    def branch_exists(self, name):
        self.calls.append(("branch_exists", name))
        return name in self.branches

    def find_pull_request(self, head_branch):
        self.calls.append(("find_pull_request", head_branch))
        for pull in self.pulls:
            if pull["head"] == head_branch:
                return pull
        return None

    def create_pull_request(self, head, base, title, body, draft=True):
        self.calls.append(("create_pull_request", head))
        if self.fail_pr_permission:
            raise AdapterError(PR_PERMISSION_REMEDY, status=403)
        pull = {"number": len(self.pulls) + 1, "head": head, "base": base, "title": title,
                "body": body, "draft": bool(draft), "state": "open",
                "html_url": "https://example.invalid/%s/%s/pull/%d"
                            % (self.owner, self.repo, len(self.pulls) + 1)}
        self.pulls.append(pull)
        return pull

    def open_issues(self, labels=None):
        self.calls.append(("open_issues", tuple(labels or ())))
        return [i for i in self.issues if i["state"] == "open"
                and (not labels or set(labels) & set(i.get("labels") or []))]

    def find_issue_by_title(self, title, labels=None):
        self.calls.append(("find_issue_by_title", title))
        for issue in self.open_issues(labels):
            if issue["title"] == title:
                return issue
        return None

    def find_issues_by_title_prefix(self, prefix, labels=None):
        self.calls.append(("find_issues_by_title_prefix", prefix))
        return [i for i in self.open_issues(labels)
                if (i.get("title") or "").startswith(prefix)]

    def close_issue(self, number, comment=None):
        self.calls.append(("close_issue", number))
        for issue in self.issues:
            if issue["number"] == number:
                issue["state"] = "closed"
                issue["close_comment"] = comment
                return issue
        raise AdapterError("no such issue %r" % number)

    def create_issue(self, title, body, labels=None):
        self.calls.append(("create_issue", title))
        issue = {"number": len(self.issues) + 1, "title": title, "body": body,
                 "labels": list(labels or []), "state": "open",
                 "html_url": "https://example.invalid/%s/%s/issues/%d"
                             % (self.owner, self.repo, len(self.issues) + 1)}
        self.issues.append(issue)
        return issue

    def update_issue(self, number, body):
        self.calls.append(("update_issue", number))
        for issue in self.issues:
            if issue["number"] == number:
                issue["body"] = body
                return issue
        raise AdapterError("no such issue %r" % number)
