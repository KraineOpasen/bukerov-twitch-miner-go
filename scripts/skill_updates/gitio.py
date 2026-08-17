"""Read-only git access to upstream provider repositories.

Design rule for this whole module: **an upstream repository is untrusted data, never executable
input.** Two structural properties enforce that, and both are worth stating because neither is
obvious from the call sites:

1. *No working tree is ever created.* Upstream is fetched into a bare repository and every byte
   is read through `git cat-file`. Upstream content therefore never lands on disk as a file with
   a name, a mode, or a path -- so there is nothing for a later step to accidentally execute,
   no symlink can be materialized, and a hostile path such as ``.git/hooks/pre-commit`` or
   ``../../etc/x`` is just a string in a listing rather than a write to that location.

2. *Every git invocation is an argument list with a hardened environment.* `shell=False` (the
   `subprocess.run` default with a list argv) means no upstream-controlled string -- a skill
   name, a path, a ref -- can ever be reinterpreted as shell syntax. `_git_env()` neutralizes
   ambient configuration, hooks, credential prompting, alternate transports and LFS smudging,
   so the result does not depend on the machine the bot happens to run on.

The module also draws the line between "we know the upstream ref" and "we are guessing": a
target SHA is always *proved* by `ls_remote()` against the configured branch before anything is
fetched. `upstream ref cannot be proven` is a first-class BLOCKED condition, and this is where
it is detected.
"""

import os
import re
import subprocess

from .errors import GitError

#: Only plain HTTPS GitHub repository URLs are accepted. This is an allowlist, not a sanitizer:
#: it rejects `ext::`/`file://`/`ssh://` transports outright, and -- because the pattern is
#: anchored -- a URL cannot begin with `-` and be smuggled into an argv position where git would
#: read it as an option (the `--upload-pack=` class of attack).
UPSTREAM_URL_RE = re.compile(r"^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")

#: A full 40-hex object name. Short SHAs and symbolic refs are rejected everywhere a commit is
#: expected: an abbreviated SHA is ambiguous by construction, and a floating ref is exactly the
#: dependency this project vendors upstream to avoid.
SHA1_RE = re.compile(r"^[0-9a-f]{40}$")

#: Branch names the bot will read. Deliberately narrow -- these are read from a repo-owned
#: config file, and a name outside this shape signals a config error, not an exotic branch.
REF_NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]*$")

#: Default timeout, in seconds, for any single git invocation. Network calls in CI must fail
#: rather than hang: a wedged updater run holds the concurrency group and blocks every later
#: scheduled run behind it.
DEFAULT_TIMEOUT = 300


def _git_env(untrusted=True):
    """A minimal, reproducible environment for every git invocation.

    `untrusted` selects the transport allowlist. Against a provider repository it is `https`
    alone. Against *this* repository it additionally permits `file`, because `origin` may
    legitimately be a local path -- that is how the test suite exercises the real push code
    path instead of mocking it. `ext::`, the only transport that executes a command, is refused
    in both cases by `_HARDENING`.

    Each entry closes a specific hole rather than being defensive boilerplate:

    * ``GIT_CONFIG_GLOBAL`` / ``GIT_CONFIG_SYSTEM`` -> /dev/null: ambient `~/.gitconfig` and
      `/etc/gitconfig` cannot inject aliases, `url.*.insteadOf` rewrites, or filters. This is
      also what makes results identical between CI and a laptop.
    * ``GIT_TERMINAL_PROMPT=0``, ``GIT_ASKPASS``, ``SSH_ASKPASS``: a private or deleted upstream
      fails fast instead of blocking on a credential prompt that will never be answered.
    * ``GIT_ALLOW_PROTOCOL=https``: even if a URL check were somehow bypassed, git itself will
      refuse any other transport.
    * ``GIT_LFS_SKIP_SMUDGE=1``: belt and braces. Nothing is checked out, so no smudge filter
      should run, but an LFS-enabled upstream must not be able to trigger a network fetch
      through a filter either.
    * ``GIT_PROTOCOL_FROM_USER=0``: treats the URL as if it came from a config file rather than
      a user, which disables the protocols git considers unsafe from untrusted sources.

    `PATH` is inherited because git itself must remain findable; nothing else is.
    """
    env = {
        "PATH": os.environ.get("PATH", "/usr/local/bin:/usr/bin:/bin"),
        "HOME": os.environ.get("HOME", "/tmp"),
        "GIT_CONFIG_GLOBAL": os.devnull,
        "GIT_CONFIG_SYSTEM": os.devnull,
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_TERMINAL_PROMPT": "0",
        "GIT_ASKPASS": "",
        "SSH_ASKPASS": "",
        "GIT_ALLOW_PROTOCOL": "https" if untrusted else "https:file",
        "GIT_PROTOCOL_FROM_USER": "0",
        "GIT_LFS_SKIP_SMUDGE": "1",
        "LC_ALL": "C",
        "TZ": "UTC",
    }
    # Proxy settings must survive: a sandboxed runner may only reach GitHub through one.
    # These carry no authority of their own -- they select a route, not a credential.
    for key in ("https_proxy", "HTTPS_PROXY", "http_proxy", "HTTP_PROXY", "no_proxy", "NO_PROXY",
                "GIT_SSL_CAINFO", "SSL_CERT_FILE", "GIT_PROXY_SSL_CAINFO", "CURL_CA_BUNDLE"):
        if key in os.environ:
            env[key] = os.environ[key]
    return env


#: git config forced on for every invocation. `core.hooksPath` to a non-existent directory is
#: the important one: it guarantees that no hook from a fetched repository -- or from the
#: bot's own working repo -- can execute, even if a future change here does create a work tree.
_HARDENING = [
    "-c", "core.hooksPath=/nonexistent",
    "-c", "core.symlinks=false",
    "-c", "core.autocrlf=false",
    "-c", "core.fsmonitor=false",
    # `ext::` lets a URL name a command for git to execute. It is the one transport that turns a
    # string into code, so it is banned unconditionally -- for our own repository too.
    "-c", "protocol.ext.allow=never",
    "-c", "advice.detachedHead=false",
    "-c", "gc.auto=0",
]

#: Extra hardening for operations against an UNTRUSTED upstream. Banning the `file` transport
#: matters there (a hostile repository could reference a local path through a submodule or an
#: alternate) and only there: our own `origin` is an https remote in production, and a local
#: bare remote is exactly what the test suite needs to exercise the real push path. Applying
#: this globally would mean the push code path could never be tested against a real git remote,
#: which is a worse trade than the residual risk of allowing a local transport on our own repo.
_UNTRUSTED_HARDENING = [
    "-c", "protocol.file.allow=never",
]


def run_git(args, cwd=None, timeout=DEFAULT_TIMEOUT, binary=False, check=True, untrusted=True):
    """Invoke git with hardening flags and a clean environment.

    `args` is the argv *after* the hardening flags. Always a list -- never a string -- so no
    caller can introduce shell metacharacter handling by accident.

    `untrusted` (default True) adds the extra transport restrictions appropriate for talking to
    a provider repository. Operations on *this* repository pass `untrusted=False`; the default
    is the strict one so a new call site is hardened unless it deliberately opts out.

    Returns stdout (bytes when `binary`, else str). Raises `GitError` on non-zero exit unless
    `check=False`, in which case `(returncode, stdout)` is returned so a caller can treat a
    specific failure as data (an absent path in a tree, say) rather than an error.
    """
    if isinstance(args, (str, bytes)):
        # `list("status; whoami")` would silently become one argument per CHARACTER, which git
        # then rejects with an unrelated-looking error. Refuse the type outright so a future
        # caller that passes a command string gets told what it did wrong.
        raise GitError("run_git() takes an argv list, not a string: %r" % (args,))
    argv = ["git"] + _HARDENING + (_UNTRUSTED_HARDENING if untrusted else []) + list(args)
    try:
        proc = subprocess.run(argv, cwd=cwd, env=_git_env(untrusted), stdout=subprocess.PIPE,
                              stderr=subprocess.PIPE, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise GitError("git timed out after %ds: %s" % (timeout, " ".join(args[:3])))
    except OSError as exc:
        raise GitError("could not run git: %s" % exc)
    out = proc.stdout if binary else proc.stdout.decode("utf-8", "replace")
    if check and proc.returncode != 0:
        raise GitError("git %s failed (%d): %s" % (
            " ".join(args[:3]), proc.returncode,
            proc.stderr.decode("utf-8", "replace").strip()[:400]))
    if check:
        return out
    return proc.returncode, out


def validate_url(url):
    """Return `url` if it is an acceptable upstream, else raise. See UPSTREAM_URL_RE."""
    if not isinstance(url, str) or not UPSTREAM_URL_RE.match(url):
        raise GitError("refusing non-allowlisted upstream URL: %r" % (url,))
    return url


def validate_sha(sha, what="commit"):
    """Return `sha` if it is a full 40-hex object name, else raise."""
    if not isinstance(sha, str) or not SHA1_RE.match(sha):
        raise GitError("%s is not a full 40-hex sha: %r" % (what, sha))
    return sha


def validate_ref(ref):
    """Return `ref` if it is an acceptable branch name, else raise."""
    if not isinstance(ref, str) or not REF_NAME_RE.match(ref) or ".." in ref:
        raise GitError("refusing malformed upstream ref: %r" % (ref,))
    return ref


def ls_remote(url, ref):
    """Resolve `refs/heads/<ref>` on `url` to a commit SHA, without fetching anything.

    This is the proof step for "the upstream ref can be proven". It answers one question --
    *what does the reviewed branch point at right now* -- and it answers it before a single
    object is transferred, so an unreachable, renamed or deleted branch is reported as a
    BLOCKED condition rather than discovered halfway through preparing a candidate.
    """
    validate_url(url)
    validate_ref(ref)
    out = run_git(["ls-remote", "--exit-code", "--", url, "refs/heads/" + ref])
    lines = [ln for ln in out.splitlines() if ln.strip()]
    if len(lines) != 1:
        raise GitError("expected exactly one ref match for %s on %s, got %d"
                       % (ref, url, len(lines)))
    sha = lines[0].split("\t", 1)[0].strip()
    return validate_sha(sha, "resolved ref %s" % ref)


def default_branch(url):
    """The branch `HEAD` points at on the remote, or None when it cannot be determined.

    Read from `ls-remote --symref`, so it is upstream's own answer rather than an assumption.
    Nothing in this package hardcodes "main": a provider's reviewed ref comes from the registry,
    and this exists so that the reviewed ref silently ceasing to be the default branch is a
    reportable fact instead of an invisible one.
    """
    validate_url(url)
    out = run_git(["ls-remote", "--symref", "--", url, "HEAD"])
    for line in out.splitlines():
        if line.startswith("ref:"):
            parts = line.split()
            if len(parts) >= 2 and parts[1].startswith("refs/heads/"):
                return parts[1][len("refs/heads/"):]
    return None


class UpstreamRepo:
    """A bare, checkout-free view of one upstream repository at one or more commits.

    Created by `fetch_commits()`. Every accessor reads objects out of the object database; no
    method on this class writes a file, so there is no point at which upstream content becomes
    an executable artifact.
    """

    def __init__(self, path, url):
        self.path = path
        self.url = url

    def _run(self, args, **kw):
        return run_git(args, cwd=self.path, **kw)

    def commit_exists(self, sha):
        """True when `sha` names a commit object present locally."""
        validate_sha(sha)
        rc, out = self._run(["cat-file", "-t", sha], check=False)
        return rc == 0 and out.strip() == "commit"

    def tree_entries(self, commit, prefix=None):
        """Full recursive inventory at `commit`, as ``{path: (mode, type, blob_sha)}``.

        Uses ``-z`` so a path containing a newline or quote is impossible to misparse -- git's
        default output quotes such paths, and a quoted path silently mismatching a manifest
        entry is precisely the kind of "looks clean, is wrong" failure this bot must not have.
        Symlinks (mode 120000), submodules (160000) and executables (100755) all appear here
        with their real mode, which is what lets the classifier flag them; nothing is filtered
        out at this layer.
        """
        validate_sha(commit)
        args = ["ls-tree", "-r", "-z", "--full-tree", commit]
        if prefix is not None:
            args += ["--", prefix]
        raw = self._run(args, binary=True)
        entries = {}
        for record in raw.split(b"\x00"):
            if not record:
                continue
            meta, _, path = record.partition(b"\t")
            fields = meta.decode("utf-8").split()
            if len(fields) != 3:
                raise GitError("unparsable ls-tree record: %r" % (record[:120],))
            mode, obj_type, sha = fields
            entries[path.decode("utf-8", "surrogateescape")] = (mode, obj_type, sha)
        return entries

    def blob(self, sha):
        """Raw bytes of one blob. Returns None when the object is absent or not a blob."""
        validate_sha(sha, "blob")
        rc, _ = self._run(["cat-file", "-e", sha], check=False)
        if rc != 0:
            return None
        return self._run(["cat-file", "blob", sha], binary=True)

    def is_ancestor(self, maybe_ancestor, descendant):
        """True when `maybe_ancestor` is reachable from `descendant`.

        This is the fast-forward test. `git merge-base --is-ancestor` communicates through its
        exit status (0 yes, 1 no, anything else an error), so `check=False` is required -- a
        raising call here would turn "not an ancestor", the single most interesting answer, into
        a crash.
        """
        validate_sha(maybe_ancestor)
        validate_sha(descendant)
        rc, _ = self._run(["merge-base", "--is-ancestor", maybe_ancestor, descendant],
                          check=False)
        if rc not in (0, 1):
            raise GitError("merge-base --is-ancestor failed with status %d" % rc)
        return rc == 0

    def merge_base(self, a, b):
        """Best common ancestor of two commits, or None when the histories are unrelated.

        None is the signal that upstream's history was *replaced* rather than advanced -- a
        different situation from diverging, and one no automated refresh should paper over.
        """
        validate_sha(a)
        validate_sha(b)
        rc, out = self._run(["merge-base", a, b], check=False)
        if rc != 0:
            return None
        return out.strip() or None

    def commit_tree_sha(self, commit):
        """The tree object a commit points at -- recorded in manifests as `upstream_tree`."""
        validate_sha(commit)
        return self._run(["rev-parse", commit + "^{tree}"]).strip()

    def path_tree_sha(self, commit, path):
        """Tree SHA of one subdirectory at `commit`, or None when it does not exist.

        Provider manifests record `upstream_tree` as a per-skill map of subdirectory tree SHAs,
        so this is how a single skill's whole subtree gets one comparable fingerprint.
        """
        validate_sha(commit)
        rc, out = self._run(["rev-parse", "%s:%s" % (commit, path)], check=False)
        if rc != 0:
            return None
        return out.strip() or None


def fetch_commits(url, shas, workdir):
    """Create a bare repo under `workdir` holding every commit in `shas`.

    `--filter=blob:none` would save bandwidth, but is deliberately NOT used: this bot reads
    essentially every blob in the selected subtrees, so a partial clone would just convert one
    fetch into hundreds of lazy round-trips, each a new chance to fail halfway through and
    leave the analysis in a state where "file absent" and "fetch failed" look the same.

    `--no-tags` keeps the object set to what was asked for. Fetching by explicit SHA (rather
    than by branch) is what makes the fetched content match the SHA that `ls_remote` already
    proved, with no window in which upstream could move underneath the run.
    """
    validate_url(url)
    for sha in shas:
        validate_sha(sha)
    run_git(["init", "--bare", "--quiet", "--", workdir])
    repo = UpstreamRepo(workdir, url)
    repo._run(["remote", "add", "origin", "--", url])
    wanted = [s for s in dict.fromkeys(shas)]
    if wanted:
        repo._run(["fetch", "--quiet", "--no-tags", "--no-write-fetch-head",
                   "origin"] + wanted)
    for sha in wanted:
        if not repo.commit_exists(sha):
            raise GitError("upstream %s does not contain commit %s (ref unprovable)"
                           % (url, sha))
    return repo
