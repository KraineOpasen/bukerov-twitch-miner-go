"""The repo-owned provider registry that drives the update bot.

`docs/agents/skills-update-providers.json` records, for each provider, the one fact its manifest
deliberately does not: **which upstream branch was reviewed**. The manifest pins a commit; the
config names the ref that commit was taken from and that a future update may be taken from. Both
halves are required and they play different roles:

    manifest.upstream_commit    what we currently trust (a 40-hex SHA -- never a branch)
    config.upstream_ref         where the next candidate may come from (a reviewed branch name)

Keeping the ref out of the manifest is what stops a floating dependency entering the vendored
tree. Nothing in this package ever vendors "whatever `main` points at": the bot resolves the ref
to a concrete SHA, proves it, and then proposes *that SHA* for review. The config widens no
trust on its own -- it only says which branch a human already agreed is the right place to look.

`load()` cross-checks every entry against the provider manifest it names, so a config that
drifts from the manifests (a renamed upstream, a typo'd path) fails closed at load time rather
than producing a confidently wrong comparison later.
"""

import json
import os

from .errors import ConfigError
from .gitio import UPSTREAM_URL_RE, REF_NAME_RE, SHA1_RE

CONFIG_RELPATH = os.path.join("docs", "agents", "skills-update-providers.json")

SUPPORTED_SCHEMA_VERSION = 1

#: Keys a normal (vendored) provider entry may carry. An unknown key is an error, not something
#: to ignore: the config is the bot's authority surface, and silently tolerating an unrecognized
#: field is how a typo'd `monitor_only` ends up disabling a safety property nobody notices.
VENDORED_KEYS = {"key", "upstream_repo", "upstream_ref", "manifest", "policy", "patches",
                 "skills_root", "notes"}

#: Keys a monitor-only entry may carry. Monitor-only providers have no manifest and are never
#: vendored or updated -- they exist so that a licence or provenance change in a repository we
#: deliberately did NOT vendor still surfaces as a reviewable signal.
MONITOR_KEYS = {"key", "upstream_repo", "upstream_ref", "monitor_only", "baseline_commit",
                "watch_paths", "watch_baseline", "notes"}

def valid_provider_key(key):
    """Provider key shape: alphanumeric and dashes, starting with an alphanumeric.

    Used verbatim inside branch names and issue titles, so it is restricted to characters that
    are inert in a git ref, in a shell word, and in Markdown. The leading-character rule is not
    cosmetic: a key beginning with `-` could reach an argv position where a tool reads it as an
    option.

    This is THE definition, imported by `candidate.branch_name()` rather than re-implemented
    there. A second copy is how the two drift, and a looser copy at the branch-building site
    would silently undo the guarantee this one makes.
    """
    return (isinstance(key, str) and 1 <= len(key) <= 40
            and key[0].isalnum() and all(c.isalnum() or c == "-" for c in key))


#: Backwards-compatible private alias used by the validation helpers below.
_valid_key = valid_provider_key


class Provider:
    """One entry from the registry, cross-checked against its manifest.

    `monitor_only` providers carry `baseline_commit`/`watch_paths` and no manifest; vendored
    providers carry a manifest and no baseline. The two are kept in one class because the
    check phase treats them uniformly -- resolve the ref, compare to a known commit -- and
    diverge only at the point where a vendored provider would go on to prepare a candidate.
    """

    def __init__(self, raw, repo_root):
        self.raw = raw
        self.key = raw["key"]
        self.upstream_repo = raw["upstream_repo"]
        self.upstream_ref = raw["upstream_ref"]
        self.notes = raw.get("notes", "")
        self.monitor_only = bool(raw.get("monitor_only", False))
        self.repo_root = repo_root
        if self.monitor_only:
            self.manifest_path = None
            self.baseline_commit = raw["baseline_commit"]
            self.watch_paths = tuple(raw["watch_paths"])
            # path -> blob sha at the reviewed baseline, or None for "absent at baseline".
            # Recording absence explicitly is the point: for this repository the *absence* of a
            # root licence is the reviewed finding, so a licence file APPEARING is exactly the
            # signal worth waking a human for.
            self.watch_baseline = dict(raw["watch_baseline"])
            self.skills_root = None
        else:
            self.manifest_path = os.path.join(repo_root, raw["manifest"])
            self.policy_path = os.path.join(repo_root, raw["policy"])
            self.patches_path = os.path.join(repo_root, raw["patches"])
            self.skills_root = raw.get("skills_root", os.path.join(".claude", "skills"))
            self.baseline_commit = None
            self.watch_paths = ()

    @property
    def manifest_relpath(self):
        return self.raw.get("manifest")

    def __repr__(self):
        return "<Provider %s %s@%s>" % (self.key, self.upstream_repo, self.upstream_ref)


def _require(cond, message):
    if not cond:
        raise ConfigError(message)


def _validate_entry(raw, index):
    _require(isinstance(raw, dict), "providers[%d] is not an object" % index)
    key = raw.get("key")
    _require(_valid_key(key), "providers[%d].key %r is not a short alphanumeric-dash name"
             % (index, key))
    monitor = bool(raw.get("monitor_only", False))
    allowed = MONITOR_KEYS if monitor else VENDORED_KEYS
    unknown = sorted(set(raw) - allowed)
    _require(not unknown, "provider %r: unknown key(s) %s" % (key, unknown))
    _require(isinstance(raw.get("upstream_repo"), str)
             and UPSTREAM_URL_RE.match(raw["upstream_repo"]),
             "provider %r: upstream_repo is not an allowlisted https GitHub URL" % key)
    _require(isinstance(raw.get("upstream_ref"), str) and REF_NAME_RE.match(raw["upstream_ref"])
             and ".." not in raw["upstream_ref"],
             "provider %r: upstream_ref is not a plain branch name" % key)
    # A ref that is really a SHA would re-pin the bot to a commit and make drift undetectable
    # forever -- the config's whole job is to name a MOVING reviewed branch.
    _require(not SHA1_RE.match(raw["upstream_ref"]),
             "provider %r: upstream_ref must be a branch name, not a commit sha" % key)
    if monitor:
        _require(isinstance(raw.get("baseline_commit"), str)
                 and SHA1_RE.match(raw["baseline_commit"]),
                 "provider %r: monitor_only entry needs a 40-hex baseline_commit" % key)
        _require(isinstance(raw.get("watch_paths"), list) and raw["watch_paths"]
                 and all(isinstance(p, str) and p and not p.startswith("/")
                         and ".." not in p for p in raw["watch_paths"]),
                 "provider %r: watch_paths must be a non-empty list of relative paths" % key)
        baseline = raw.get("watch_baseline")
        _require(isinstance(baseline, dict)
                 and sorted(baseline) == sorted(raw["watch_paths"]),
                 "provider %r: watch_baseline must record exactly the watch_paths" % key)
        _require(all(v is None or (isinstance(v, str) and SHA1_RE.match(v))
                     for v in baseline.values()),
                 "provider %r: watch_baseline values must be a 40-hex blob sha or null" % key)
    else:
        for field in ("manifest", "policy", "patches"):
            value = raw.get(field)
            _require(isinstance(value, str) and value.startswith("docs/agents/")
                     and ".." not in value,
                     "provider %r: %s must be a path under docs/agents/" % (key, field))


def _cross_check(provider):
    """A vendored entry must agree with the manifest it names.

    Three agreements are checked, and each one prevents a specific class of confidently-wrong
    result: the manifest must exist and parse (otherwise "no drift" would just mean "could not
    read"); its `upstream_repo` must be the same repository (otherwise the bot would compare a
    pin against a different project's history); and its `upstream_commit` must be a full SHA
    (otherwise there is no fixed point to diff from).
    """
    path = provider.manifest_path
    _require(os.path.isfile(path), "provider %r: manifest not found: %s" % (provider.key, path))
    try:
        with open(path, encoding="utf-8") as handle:
            manifest = json.load(handle)
    except (OSError, ValueError) as exc:
        raise ConfigError("provider %r: manifest unreadable: %s" % (provider.key, exc))
    _require(isinstance(manifest, dict), "provider %r: manifest is not an object" % provider.key)
    declared = manifest.get("upstream_repo")
    _require(_same_repo(declared, provider.upstream_repo),
             "provider %r: config upstream_repo %r != manifest upstream_repo %r"
             % (provider.key, provider.upstream_repo, declared))
    pin = manifest.get("upstream_commit")
    _require(isinstance(pin, str) and SHA1_RE.match(pin),
             "provider %r: manifest upstream_commit is not a 40-hex sha: %r"
             % (provider.key, pin))
    for field in ("policy", "patches"):
        target = getattr(provider, field + "_path")
        _require(os.path.isfile(target),
                 "provider %r: %s not found: %s" % (provider.key, field, target))
    return manifest


def _same_repo(a, b):
    """Compare two GitHub URLs ignoring a trailing slash or `.git` suffix only.

    Case is NOT folded: GitHub owner/repo names are case-insensitive for routing but the
    manifests record them in their canonical upstream spelling, and quietly accepting a
    different casing would let a config entry point at a look-alike that a reader skims past.
    """
    def canon(url):
        if not isinstance(url, str):
            return None
        url = url.rstrip("/")
        return url[:-4] if url.endswith(".git") else url
    return canon(a) is not None and canon(a) == canon(b)


def load(repo_root, path=None):
    """Parse, validate and cross-check the registry. Returns a list of `Provider`.

    Order is preserved exactly as written in the file. Every downstream consumer iterates this
    list, so file order is the bot's canonical provider order -- reports, job summaries and
    multi-provider runs are all reproducible because of it.
    """
    path = path or os.path.join(repo_root, CONFIG_RELPATH)
    try:
        with open(path, encoding="utf-8") as handle:
            doc = json.load(handle)
    except OSError as exc:
        raise ConfigError("cannot read provider config %s: %s" % (path, exc))
    except ValueError as exc:
        raise ConfigError("provider config %s is not valid JSON: %s" % (path, exc))
    _require(isinstance(doc, dict), "provider config must be a JSON object")
    _require(doc.get("schema_version") == SUPPORTED_SCHEMA_VERSION,
             "provider config schema_version must be %d, got %r"
             % (SUPPORTED_SCHEMA_VERSION, doc.get("schema_version")))
    entries = doc.get("providers")
    _require(isinstance(entries, list) and entries,
             "provider config must carry a non-empty providers list")
    seen = set()
    providers = []
    for index, raw in enumerate(entries):
        _validate_entry(raw, index)
        _require(raw["key"] not in seen, "duplicate provider key %r" % raw["key"])
        seen.add(raw["key"])
        provider = Provider(raw, repo_root)
        if not provider.monitor_only:
            _cross_check(provider)
        providers.append(provider)
    return providers


def select(providers, key):
    """Return `providers` filtered to one key, or all of them for the literal "all".

    Raises on an unknown key rather than returning an empty list: a `workflow_dispatch` typo
    must fail the run loudly, not silently succeed having checked nothing.
    """
    if key in (None, "", "all"):
        return list(providers)
    matches = [p for p in providers if p.key == key]
    _require(matches, "unknown provider %r (known: %s)"
             % (key, ", ".join(p.key for p in providers)))
    return matches
