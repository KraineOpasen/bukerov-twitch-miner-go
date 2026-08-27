# Third-party notices — stable skills maintenance

G1.1 installs no package, executes no script fetched from an upstream skills
repository, and selects no external heartbeat provider. Its closed executable
dependency set is the GitHub-hosted Ubuntu runner, CPython, Git, GNU Bash, the
pinned `actions/checkout` tree, and the GitHub Actions Node 20 handler used only
by that Action. The machine-readable source of truth is
`external-dependencies.json`.

## Bootstrap ordering and trust boundary

The workflow source contains the literal
`actions/checkout@11d5960a326750d5838078e36cf38b85af677262` pin, the literal
`${{ github.sha }}` ref, the complete consumer-specific checkout inputs, and a
pre-checkout hosted-runtime evidence check. Those reviewed workflow bytes are
bound by the canonical SHA-256
`2d0c11afc6fd18b3141ee2a211e4dab4496465cfd6afb90112687e9c0ac33148`;
the three pre-checkout Python bodies must remain byte-identical. They are the
bootstrap trust boundary. GitHub must start the provider-attested runner and
the checkout Node 20 handler before code from the checked-out repository can
execute.

The full ledger, Action-tree, input, workflow, runner-evidence, and control-plane
verification therefore runs **after checkout**, but **before updater or upstream
provider access**. A mismatch at that boundary is `BLOCKED`; it prevents all
subsequent detector and preparer work. The untrusted pull-request consumer has
read-only `contents:read`, no secrets, no persisted credentials, and no write or
publication authority. The ledger does not claim that repository code can
verify an Action before that Action is invoked.
The first repository-owned gate additionally requires a real Git worktree whose
resolved top level is the repository root and whose `HEAD^{commit}` equals
`GITHUB_SHA`. This detects and blocks checkout's non-Git REST fallback before
the updater or any provider is reached.

## Managed GitHub REST services

G1.1 has exactly two managed-service dependencies in addition to its Action and
runtime records: `github-rest-quarantine-read` and
`github-rest-runner-image-release-read`. Both use unauthenticated, read-only
`GET` requests to the exact `https://api.github.com` paths in the ledger, with
the GitHub REST API version `2022-11-28`, system-root TLS verification, ambient
authentication/proxy/trust overrides forbidden, redirects rejected, bounded
UTF-8 JSON, and exact response-field checks.

The service licence value is `N/A`, with disposition
`MANAGED_SERVICE_NO_EXECUTABLE_CODE_REDISTRIBUTED`: G1.1 consumes public metadata
under the [GitHub Terms of Service](https://docs.github.com/en/site-policy/github-terms/github-terms-of-service)
but downloads or redistributes no service executable code. The transitive
client is CPython 3.12 standard-library `urllib.request`, JSON, TLS/OpenSSL, and
their hosted-image provider-manifest/Included Software closure; no package is
installed. Responses are memory-only and job-scoped, except the runner gate's
enumerated safe fields written to the step summary. No raw quarantine response
or credential is logged or uploaded.

The quarantine service applies only to live identity verification in stable
`detect` and `prepare`. The runner-image release service applies only to the
pre-checkout gates in stable `detect`, stable `prepare`, and CI `governance`.
Missing, redirected, oversized, malformed, ambiguous, or identity-mismatched
responses block at their declared boundary. The rollback owner is the
repository owner; rollback means disabling the affected workflow or reverting
to the last reviewed service envelope, never silently following a new endpoint
or falling back to stale evidence.

## GitHub-hosted Ubuntu 24.04 x64 runner

- Source owner: GitHub Actions
- Source repository: `https://github.com/actions/runner-images`
- Repository ID: `190416463`
- Selector: `ubuntu-24.04`; required `ImageOS`: `ubuntu24`; required
  `RUNNER_ARCH`: `X64`
- Identity: provider-attested rolling `ImageVersion`, not a digest-pinned image
- Source-repository licence: MIT
- Image-package licences: multi-licence, as enumerated by the Included Software
  document and provider manifest for the exact release

Before reading a release, the verifier performs an unauthenticated, bounded
`GET /repos/actions/runner-images` and requires repository ID `190416463` and
full name `actions/runner-images` exactly. For
`ImageVersion=YYYYMMDD.BUILD.REVISION`, it then derives the release tag
`ubuntu24/YYYYMMDD.BUILD` and performs only
`GET /repos/actions/runner-images/releases/tags/ubuntu24%2FYYYYMMDD.BUILD`.
Both requests require verified TLS, reject proxies and redirects, and require an
exact final URL. The release target must be a full 40-hex commit. The exact
Included Software URL is derived as
`https://raw.githubusercontent.com/actions/runner-images/SOURCE_SHA/images/ubuntu/Ubuntu2404-Readme.md`,
where `SOURCE_SHA` is the validated full release `target_commitish`.
The release must contain asset `internal.ubuntu24.json` with API-provided `id`,
`size`, and `sha256:` digest. Missing or malformed evidence is `BLOCKED`.

The provider image release, Included Software document, and provider-manifest
asset close the installed executable/package inventory for the run. A separate
SBOM is classified
`OPTIONAL_HARDENING_UNAVAILABLE_NOT_REQUIRED`; no SBOM identity is invented.
The GitHub-hosted filesystem is job-scoped and ephemeral. Repository-owner
readback of the platform workflow-log retention value is unavailable, and G1.1
uploads no artifact.

The following is a non-authoritative audit sample only, never a runtime pin:

- `ImageVersion`: `20260823.283.1`
- Release tag: `ubuntu24/20260823.283`
- Release target: `73a898e845210ee1565a4bb3328897e152dd73ae`
- Included Software path:
  `73a898e845210ee1565a4bb3328897e152dd73ae/images/ubuntu/Ubuntu2404-Readme.md`
- Provider manifest asset: `internal.ubuntu24.json`, ID `527689532`, size
  `30505`, digest
  `sha256:68c57165414e6868ea1b042b920640435daacf12eaa3bbdcaa85abbc4caac214`

## CPython 3.12

- Upstream source: `https://github.com/python/cpython`
- Operational source: the exact provider-attested hosted-image release above
- Runtime envelope: CPython `>=3.12.0,<3.13.0`; standard library only
- Primary licence: PSF-2.0; bundled component terms are recorded in CPython's
  `LICENSE` and the hosted-image evidence
- Canonical licence: `https://github.com/python/cpython/blob/3.12/LICENSE`

Every production Python process uses a direct `python3 -I -S -B` entrypoint; the
workflow does not run `pip`, import ambient site packages, or write bytecode.
The hosted-image manifest and Included Software record close the installed
binary; isolated CPython execution closes imports to the standard library and
repository source. The verifier records the exact `platform.python_version`
evidence and blocks
unless the implementation is CPython 3.12.x. CPython makes no direct network
request and retains no state beyond the ephemeral job.

## Git

- Upstream source: `https://github.com/git/git`
- Operational source: the exact provider-attested hosted-image release above
- Runtime envelope: `>=2.43.0,<2.60.0`
- Licence: GPL-2.0-only
- Canonical licence: `https://github.com/git/git/blob/master/COPYING`

The updater-owned argv closure is exactly: `--version`; both `ls-remote`
variants; `init --bare`; `remote add`; `fetch`; `cat-file -t`, `cat-file -e`,
and `cat-file blob`; `ls-tree -r -z --full-tree`; `merge-base` and
`merge-base --is-ancestor`; and the two `rev-parse` forms recorded in the
ledger. Checkout's internal Git commands are not generalized into that closure;
they are owned by the exact Action tree and consumer-input closure.

The ledger also records the literal argv prefixes that precede those commands.
The updater prefix fixes hooks, symlinks, autocrlf, fsmonitor, `ext` and `file`
protocols, detached-head advice, automatic GC, an empty credential helper,
verified TLS, and redirect rejection. CPython selects each production temporary
directory through its standard `tempfile` rules (`TMPDIR`, `TEMP`, `TMP`, then the
platform temporary directory); the Git home is command-scoped, empty, exclusive,
and mode 0700, while the bare object database is scoped to the complete production
CLI command. On the hosted runner their final retention boundary is the ephemeral
job VM. The facade verifies the frozen donor source and argv bytes before use. The runtime-control prefix
fixes hooks, credentials, all protocols except HTTPS, TLS verification, and
redirect rejection before its combined `ls-remote --symref --exit-code` query.

Updater Git is argv-only, shell-free, HTTPS-only, unauthenticated, hook-disabled,
credential-helper-disabled, and redirect-rejecting. It creates only a bare,
checkout-free object database in that CPython-selected command-scoped temporary
directory. Checkout may
use the read-only job token, but `persist-credentials` is false. The hosted-image
evidence closes Git and its system-library dependencies.

Before checkout, the bootstrap permits exactly four `GIT_*` variables with exact
values: `GIT_ALLOW_PROTOCOL=https`, `GIT_CONFIG_GLOBAL=/dev/null`,
`GIT_CONFIG_NOSYSTEM=1`, and `GIT_TERMINAL_PROMPT=0`. Every other `GIT_*`
variable—including `GIT_CONFIG_COUNT`, `GIT_CONFIG_PARAMETERS`, `GIT_EXEC_PATH`,
`GIT_TEMPLATE_DIR`, `GIT_SSL_NO_VERIFY`, and all Git trace/output variables—is rejected. Workflow
validation also rejects assignments that shadow provider-reserved `ImageOS`,
`ImageVersion`, `GITHUB_*`, `RUNNER_*`, `ACTIONS_*`, or `CI` identity variables.
These controls prevent ambient execution/configuration overrides from altering
checkout or the repository-owned post-checkout guard.

## GNU Bash

- Upstream source: `https://savannah.gnu.org/projects/bash`
- Operational source: the exact provider-attested hosted-image release above
- Runtime envelope: GNU Bash 5.2.x at `/usr/bin/bash`
- Licence: GPL-3.0-or-later
- Canonical licence: `https://git.savannah.gnu.org/cgit/bash.git/tree/COPYING`

Bash is used by stable `detect`, `prepare`, and `reconcile-all`, and by CI
`governance`. The closed invocation uses no user or system profile and enables
errexit and pipefail. It downloads no script, installs no shell extension,
requires no elevation, and makes no direct network request. Its binary and
system-library closure is the exact hosted-image evidence. State is confined to
the ephemeral job; the platform log-retention value remains owner-readback
unavailable.

## GitHub Actions Node 20 handler

- Upstream source: `https://github.com/nodejs/node`
- Operational source: GitHub Actions' provider-attested rolling `node20` Action
  handler on the exact hosted-image release
- Licence: MIT, with bundled component terms in Node.js `LICENSE` and the
  hosted-image provider manifest
- Canonical licence: `https://github.com/nodejs/node/blob/main/LICENSE`

Node 20 has exactly three consumers: checkout in stable `detect`, stable
`prepare`, and CI `governance`. The runtime-family selection is bound by the
pinned Action's exact `action.yml` (`using: node20`). GitHub controls the rolling
Node 20 patch; the ledger does not claim a nonexistent repository-selected
runtime digest. The provider image evidence closes that runtime, while the exact
checkout tree manifest closes its invoked JavaScript bundle and bundled licence
metadata. No `npm`, `npx`, external module resolution, or package installation
occurs. Its checkout and post-handler state is job-scoped and credentials are
not persisted.

Before the Node 20 checkout handler starts, the bootstrap rejects nonempty
`NODE_OPTIONS`, `NODE_EXTRA_CA_CERTS`, and `NODE_TLS_REJECT_UNAUTHORIZED`.
Thus ambient module injection or TLS trust/verification overrides cannot alter
the handler before repository-owned verification is available.

## actions/checkout

- Canonical source: `https://github.com/actions/checkout`
- Repository ID: `197814629`
- Commit: `11d5960a326750d5838078e36cf38b85af677262`
- Commit signature: verified by GitHub at the recorded provenance check
- Tree: `f8a7b72dc00648d050099727d25ca92a43ad1162`
- Licence blob: `a67dca8b4f65d6bd351f6b1e333ce2cd84d843a5`
- Licence SHA-256: `3e855ffa704114a51628ef8f0bf3aeb41728adf9d9070e263bf58aa5640b0eb5`
- `action.yml` blob: `24e73e5a12126edc2adb9e5cd1bf245ce85bde56`
- `action.yml` SHA-256: `6188f6991491ed38977347cdaad0b0cd921a6d6232892363c91f433c22954f4f`
- `dist/index.js` blob: `17a1749fafa5ad50ee5376ad39fe19386c37f5a3`
- `dist/index.js` SHA-256: `ff743df9891fd49ffdf6e6bb11ad574bce26a13862ede59474201ef4c9ae029e`
- Full 117-entry recursive Git-tree manifest SHA-256 (JCS): `4815ff3a74ae4f01971ccb58454ce373292dbac3e21394d23b48151df2feade7`
- Recursive tree response: `truncated=false` (104 blobs, 13 trees)
- `package-lock.json` blob: `aef29d3bce5069236ea8ed92827834d6381f2685`
- `package-lock.json` SHA-256: `678275cbeb52fecbacc620aed081473053f86b787f1b469e157016376a26cc70`
- Candidate pin key: `actions/checkout/action.yml@11d5960a326750d5838078e36cf38b85af677262`
- Licence: MIT License

The immutable full recursive tree manifest records every path, mode, type and Git
object identity in the pinned Action tree. Its `.licenses/npm/**` closure contains
30 bundled dependency-licence metadata blobs under three trees; those notices and
the exact `package-lock.json` identity are part of the 117-entry manifest. The
separately recorded SHA-256 values bind the root licence, descriptor, invoked bundle,
and lockfile bytes independently. The workflows do not run `npm`, `npx`, `pip`, a
package installer, or an upstream setup script. The literal pin and inputs bind
the bootstrap invocation. The post-checkout verifier blocks any tree, licence,
descriptor, bundle, lockfile, input, permission, or runtime mismatch before the
updater or an upstream provider is accessed.

### MIT License text from the pinned tree

Copyright (c) 2018 GitHub, Inc. and contributors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
