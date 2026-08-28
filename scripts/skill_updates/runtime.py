#!/usr/bin/env python3
"""Fail-closed runtime and control-plane envelope for G1.1.

This module is stable-authored.  It binds the repository, stable branch, workflow,
legacy quarantine, hosted runner/toolchain closure, and the only third-party Action
used by the stable-native detector.  G1.1 is deliberately artifact-only: none of the helpers in
this module can create or update a ref, pull request, issue, workflow, setting, or
secret.

The production workflow uses :func:`verify_workflow_environment` before the first
artifact write.  Live control identity is resolved without a token through a fixed,
committed HTTPS origin and ``git ls-remote --symref``.  Missing, ambiguous, or changed
facts are errors; UNKNOWN/UNAVAILABLE never becomes success.
"""

import argparse
import ast
from dataclasses import dataclass
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import ssl
import stat
import subprocess
import sys
from types import MappingProxyType
from typing import Any, Callable, Mapping, Sequence
from urllib import error as urlerror
from urllib import parse as urlparse
from urllib import request as urlrequest


REPOSITORY_ID = 1297795646
REPOSITORY_FULL_NAME = "KraineOpasen/bukerov-twitch-miner-go"
REPOSITORY_HTTPS_URL = "https://github.com/KraineOpasen/bukerov-twitch-miner-go"
CANDIDATE_SCHEMA = "provider-update/v1"
STABLE_BRANCH = "release/0.3"
G11_IMPLEMENTATION_PARENT_SHA = "22dac63fd34b4e869c93f62c669d763f45b37620"
# Compatibility name for tests and downstream readers.  This is an historical
# implementation parent, never the operational candidate base.
FOUNDATION_BASE_SHA = G11_IMPLEMENTATION_PARENT_SHA
HISTORICAL_ANCESTRY_MARKER = "1cf198aa4257a5f9ba250aec29bf027870f8dad7"
OBSERVED_DEFAULT_BRANCH = "main"
OBSERVED_DEFAULT_SHA = "9c2c11030dd34c34bd7812e5a18bfe52d897b2a7"
WORKFLOW_PATH = ".github/workflows/stable-skills-maintenance.yml"
STABLE_SKILLS_WORKFLOW_SHA256 = (
    "60147affd0f00bbf6dc2e431cb7538ff84455ac1aa43ad066f6b515304971cbb"
)
STABLE_CI_GOVERNANCE_BLOCK_SHA256 = (
    "012341e19dee452f6d0c504af31e4ed79129268e0dfb2fbec2f510e81d0e728e"
)
_CI_BLOCK_BEGIN = "  # BEGIN stable-skills-governance-job/v1\n"
_CI_BLOCK_END = "  # END stable-skills-governance-job/v1\n\n"
RECONCILE_JOB_KEY = "reconcile-all"
RECONCILE_JOB_NAME = "Reconcile complete G1.1 control plane"
EXPECTED_WORKFLOW_REF = (
    REPOSITORY_FULL_NAME + "/" + WORKFLOW_PATH + "@refs/heads/" + STABLE_BRANCH
)
DONOR_WORKFLOW_PATH = ".github/workflows/skills-update.yml"

CHECKOUT_COMMIT = "11d5960a326750d5838078e36cf38b85af677262"
CHECKOUT_TREE = "f8a7b72dc00648d050099727d25ca92a43ad1162"
CHECKOUT_LICENSE_SHA256 = "3e855ffa704114a51628ef8f0bf3aeb41728adf9d9070e263bf58aa5640b0eb5"
CHECKOUT_ACTION_SHA256 = "6188f6991491ed38977347cdaad0b0cd921a6d6232892363c91f433c22954f4f"
CHECKOUT_DIST_BLOB_SHA1 = "17a1749fafa5ad50ee5376ad39fe19386c37f5a3"
CHECKOUT_DIST_SHA256 = "ff743df9891fd49ffdf6e6bb11ad574bce26a13862ede59474201ef4c9ae029e"
CHECKOUT_PIN_KEY = (
    "actions/checkout/action.yml@"
    "11d5960a326750d5838078e36cf38b85af677262"
)

# Full, non-truncated recursive Git tree independently read from the audited tree.
# Every record is reduced to the four Git identity fields before JCS hashing.
CHECKOUT_TREE_TRUNCATED = False
CHECKOUT_TREE_ENTRY_COUNT = 117
CHECKOUT_TREE_MANIFEST_SHA256 = "4815ff3a74ae4f01971ccb58454ce373292dbac3e21394d23b48151df2feade7"
CHECKOUT_PACKAGE_LOCK_BLOB_SHA1 = "aef29d3bce5069236ea8ed92827834d6381f2685"
CHECKOUT_PACKAGE_LOCK_SHA256 = "678275cbeb52fecbacc620aed081473053f86b787f1b469e157016376a26cc70"
CHECKOUT_TREE_MANIFEST = {'schema': 'github-action-tree-manifest/v1',
 'repository': 'actions/checkout',
 'commit': '11d5960a326750d5838078e36cf38b85af677262',
 'tree': 'f8a7b72dc00648d050099727d25ca92a43ad1162',
 'members': [{'path': '.eslintignore',
              'mode': '100644',
              'type': 'blob',
              'sha': '6de9a76115d1983525536c96b7ba905e6b1d7467'},
             {'path': '.eslintrc.json',
              'mode': '100644',
              'type': 'blob',
              'sha': '14c084ef11f8cdd8d8b63b7aa21e8f336628aa26'},
             {'path': '.gitattributes',
              'mode': '100644',
              'type': 'blob',
              'sha': '541fd55ed6ef70904b3b1f6cd24d0c5c13c53c3e'},
             {'path': '.github',
              'mode': '040000',
              'type': 'tree',
              'sha': '3887b3c308be56ca43cdf4fc342430e2feeef958'},
             {'path': '.github/dependabot.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '4f6427b937726142b0a723649314c5f496e4926e'},
             {'path': '.github/workflows',
              'mode': '040000',
              'type': 'tree',
              'sha': '14d0bc05528cee48fff4148bbec20c4e63cfc4ff'},
             {'path': '.github/workflows/check-dist.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '53902eeb903886205402d7e8935f5905d1c370aa'},
             {'path': '.github/workflows/codeql-analysis.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '778d474d8d9d8945f0612d3cf3885c2d02c9030a'},
             {'path': '.github/workflows/licensed.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '1f71aa7494b631ce1f97419c962731f0632b3eff'},
             {'path': '.github/workflows/publish-immutable-actions.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '87c0207285703173ffd1c9c6ec07f6fd61bb2f60'},
             {'path': '.github/workflows/test.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': 'cde9f060eb9cc0ff0d0f319219b5fd5814a35e34'},
             {'path': '.github/workflows/update-main-version.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '7bec7d5a894c903cff08199b0832ee9dc5da2e76'},
             {'path': '.github/workflows/update-test-ubuntu-git.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '5c252b98de3e868314a2887a5fe805cc8173857b'},
             {'path': '.gitignore',
              'mode': '100644',
              'type': 'blob',
              'sha': 'cd1f03c78a0b30f7a654a3014c47884b48ffce06'},
             {'path': '.licensed.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '15f619829c198f08b52da7549f6602495f4eef27'},
             {'path': '.licenses',
              'mode': '040000',
              'type': 'tree',
              'sha': '129fc5e6f3b3d1041990c51c42b7c9f617d907ac'},
             {'path': '.licenses/npm',
              'mode': '040000',
              'type': 'tree',
              'sha': 'f565bc729df73f6d196989d535a9d62fbe0f5027'},
             {'path': '.licenses/npm/@actions',
              'mode': '040000',
              'type': 'tree',
              'sha': '62fc660b4f9e9c20b8197b5ef3583cc021da9980'},
             {'path': '.licenses/npm/@actions/core.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '06638c01d908037d75e2cc98a3047d7a97d0008d'},
             {'path': '.licenses/npm/@actions/exec.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': 'cbc5abd39e6391ec6e526e6536f1a3efd706ec05'},
             {'path': '.licenses/npm/@actions/github.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '423601cac95ec71f36cafb9263d0b2889c835a0f'},
             {'path': '.licenses/npm/@actions/http-client.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': 'cdccff4e12d03fd72ab43975a6fb77dd0764565e'},
             {'path': '.licenses/npm/@actions/io.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': 'd28465403a243506f508f965c1c0641dfd5e0673'},
             {'path': '.licenses/npm/@actions/tool-cache.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': 'fbf911fef93e283a9bea20fd65d59fe51dad7d22'},
             {'path': '.licenses/npm/@fastify',
              'mode': '040000',
              'type': 'tree',
              'sha': '6913476ec6c6151c7bc7839491acc810f643e4fd'},
             {'path': '.licenses/npm/@fastify/busboy.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '4344f3a43f34f7c98d5a7162dc2b060ec558e72f'},
             {'path': '.licenses/npm/@octokit',
              'mode': '040000',
              'type': 'tree',
              'sha': '0e7bc3683b3cb76303c27e4969500ed67899067f'},
             {'path': '.licenses/npm/@octokit/auth-token.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '2ffbd9eaf76d929968946ff59b8dac3c14608a7e'},
             {'path': '.licenses/npm/@octokit/core.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '55830bf1117ea08221245945e0b59b35240b5cfe'},
             {'path': '.licenses/npm/@octokit/endpoint.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '71234c6712bee8579721fd04135149f4cf4e6bca'},
             {'path': '.licenses/npm/@octokit/graphql.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '898286909537997d5d50ba0db0d3e8a30f43373e'},
             {'path': '.licenses/npm/@octokit/openapi-types-20.0.0.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': 'b94d58918928b46c4dfbd304cd2864bfe4adc6ca'},
             {'path': '.licenses/npm/@octokit/openapi-types-22.1.0.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': 'f9be342546e0883651bc8db650fdc8aed1356d8c'},
             {'path': '.licenses/npm/@octokit/plugin-paginate-rest.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': 'c1853a6f2abcfb7a58928fcb1e4d4ff44cd7018a'},
             {'path': '.licenses/npm/@octokit/plugin-rest-endpoint-methods.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '3f4728f4e145ff400ada982d0b066d6f961f328d'},
             {'path': '.licenses/npm/@octokit/request-error.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '9c9d70242a68452f0f7a0be6d088e992c83a615e'},
             {'path': '.licenses/npm/@octokit/request.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': 'ef1a5542bfc69f9b38febda71fa1078f43b36411'},
             {'path': '.licenses/npm/@octokit/types-12.6.0.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': 'ffc81c9fd1068d9bca7140b0140814726124af7e'},
             {'path': '.licenses/npm/@octokit/types-13.4.1.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '5d9ee98c132209491d434d6142bb600947548d36'},
             {'path': '.licenses/npm/before-after-hook.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': 'c147501643837a50c60bf2f898ec2adef5b84b80'},
             {'path': '.licenses/npm/deprecation.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '12fd7cec70b8bd94724ff65b854269269db02504'},
             {'path': '.licenses/npm/once.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '7cf525acbb095d6d79c345aea72c63747a2d23e1'},
             {'path': '.licenses/npm/semver.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '248cb030140ec2e555e4f01285814778b13cb00e'},
             {'path': '.licenses/npm/tunnel.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '9a7111da96a36c81dd1160453ab32caa08c8707f'},
             {'path': '.licenses/npm/undici.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': 'fadecf4a743b7ff972adff6fb838d0547a1c7578'},
             {'path': '.licenses/npm/universal-user-agent.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '708e8965f36e8890ba155a6b1d72275c6534e67f'},
             {'path': '.licenses/npm/uuid-3.4.0.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '461f159ddef8d5b617cc91c6b65d9517e00601a8'},
             {'path': '.licenses/npm/uuid-8.3.2.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': 'bf84da082324fb7972ccc5d1e813ecbc29df1c0b'},
             {'path': '.licenses/npm/uuid-9.0.1.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': 'c9efb0980bae25f214bd2143a1a6c84ba4d30c7d'},
             {'path': '.licenses/npm/wrappy.dep.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '2a532ec343ca5b72b9833d2a9fb53e8de07b61f1'},
             {'path': '.prettierignore',
              'mode': '100644',
              'type': 'blob',
              'sha': '2186947299cfa1b6f7cff87b03944518cfea920b'},
             {'path': '.prettierrc.json',
              'mode': '100644',
              'type': 'blob',
              'sha': '386485a7669de54ba6ac7c0de972aaf5f47c1d84'},
             {'path': 'CHANGELOG.md',
              'mode': '100644',
              'type': 'blob',
              'sha': 'baf5c2d7e95444ed8092794db58b9165a23f958f'},
             {'path': 'CODEOWNERS',
              'mode': '100644',
              'type': 'blob',
              'sha': '992d27f094ebee464fcf37f435775899f14f61d8'},
             {'path': 'CONTRIBUTING.md',
              'mode': '100644',
              'type': 'blob',
              'sha': '5c764651984ffde959cd3afde3a1afa706b6766b'},
             {'path': 'LICENSE',
              'mode': '100644',
              'type': 'blob',
              'sha': 'a67dca8b4f65d6bd351f6b1e333ce2cd84d843a5'},
             {'path': 'README.md',
              'mode': '100644',
              'type': 'blob',
              'sha': '28c91208b39f4eefced50d79b57f5794bf47e8c7'},
             {'path': '__test__',
              'mode': '040000',
              'type': 'tree',
              'sha': 'd4104db914318c92523ef756794329a4d46625f1'},
             {'path': '__test__/git-auth-helper.test.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '92a61fecc423310f325793ec914abf711dc8f88c'},
             {'path': '__test__/git-command-manager.test.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': 'cea73d4dd3ae8487038c9bf53e7d08a978ecf5ea'},
             {'path': '__test__/git-directory-helper.test.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '1627b842e29943f77b45271eb363dbc4ea7d16b4'},
             {'path': '__test__/git-version.test.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '27f702e169ef3b313c6521580b2f220c81f56285'},
             {'path': '__test__/input-helper.test.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': 'fc26c5150ff29c83e4e8e0f3c9ee1fd9c80603a8'},
             {'path': '__test__/modify-work-tree.sh',
              'mode': '100755',
              'type': 'blob',
              'sha': '89447eb5b09aeb25b040e9fea914d3628b751e09'},
             {'path': '__test__/override-git-version.cmd',
              'mode': '100755',
              'type': 'blob',
              'sha': '64c7f4dc0fd30185de0b867ad08b0d0840825834'},
             {'path': '__test__/override-git-version.sh',
              'mode': '100755',
              'type': 'blob',
              'sha': '7c3ca011a9970c6fc9c954d6f1a37d6836b29e3e'},
             {'path': '__test__/ref-helper.test.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '5c8d76b87e5fb954ee110828b471ba9885e68791'},
             {'path': '__test__/retry-helper.test.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': 'a5d3f790911f87091b3447950aa5b8f1c748ed18'},
             {'path': '__test__/unsafe-pr-checkout-helper.test.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '9efa246e8a9c421672f00409d9c254aae4f0d093'},
             {'path': '__test__/url-helper.test.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '57cb28f6d2777cc4e08c46fd37aae7e6f0da728d'},
             {'path': '__test__/verify-basic.sh',
              'mode': '100755',
              'type': 'blob',
              'sha': 'd084617f022b481e0494af94a236ae34337e0e19'},
             {'path': '__test__/verify-clean.sh',
              'mode': '100755',
              'type': 'blob',
              'sha': '86bf9d6ba4fc2859a585d9c1b22df0e8416698db'},
             {'path': '__test__/verify-fetch-filter.sh',
              'mode': '100755',
              'type': 'blob',
              'sha': '4fc9d9e38103de5dc5db27a001a1193f36a2e95e'},
             {'path': '__test__/verify-lfs.sh',
              'mode': '100755',
              'type': 'blob',
              'sha': 'b0463f1b963142548bb1cccf04127442bccf134d'},
             {'path': '__test__/verify-no-unstaged-changes.sh',
              'mode': '100755',
              'type': 'blob',
              'sha': '9b30471320c5fc2cc22aa14477b1d92239066d6b'},
             {'path': '__test__/verify-side-by-side.sh',
              'mode': '100755',
              'type': 'blob',
              'sha': '35de29abcafd44514ab027ad70ec69c65bf638d3'},
             {'path': '__test__/verify-sparse-checkout-non-cone-mode.sh',
              'mode': '100755',
              'type': 'blob',
              'sha': 'dae28ed7053ec443e1676704fb2273599acf7825'},
             {'path': '__test__/verify-sparse-checkout.sh',
              'mode': '100755',
              'type': 'blob',
              'sha': 'a9f7748b080d6766d3fe180b64757e4de7cb01c3'},
             {'path': '__test__/verify-submodules-false.sh',
              'mode': '100755',
              'type': 'blob',
              'sha': '733e24712013c34475f3aad4430dfc999f4feb65'},
             {'path': '__test__/verify-submodules-recursive.sh',
              'mode': '100755',
              'type': 'blob',
              'sha': '1b68f9b975fb457b1cfaecc43c211e0e2774721b'},
             {'path': '__test__/verify-submodules-true.sh',
              'mode': '100755',
              'type': 'blob',
              'sha': '43769fe060286425613215c4c25fdc57c2cf2b45'},
             {'path': 'action.yml',
              'mode': '100644',
              'type': 'blob',
              'sha': '24e73e5a12126edc2adb9e5cd1bf245ce85bde56'},
             {'path': 'adrs',
              'mode': '040000',
              'type': 'tree',
              'sha': 'de88f10e82e3cb587767bad6c23bdce26c5b332d'},
             {'path': 'adrs/0153-checkout-v2.md',
              'mode': '100644',
              'type': 'blob',
              'sha': 'c3312909e4cb1049f1f6259dcbe13dfb8d2a78b3'},
             {'path': 'dist',
              'mode': '040000',
              'type': 'tree',
              'sha': 'af01a1bcb5c9a1451443f2b26051f3e163cadc76'},
             {'path': 'dist/index.js',
              'mode': '100644',
              'type': 'blob',
              'sha': '17a1749fafa5ad50ee5376ad39fe19386c37f5a3'},
             {'path': 'dist/problem-matcher.json',
              'mode': '100644',
              'type': 'blob',
              'sha': '071f2cb4441a02d54d7e07af9145a8f4fb79ceca'},
             {'path': 'images',
              'mode': '040000',
              'type': 'tree',
              'sha': 'c3c052d63733f5b6aa0081caf91ee41cf6d2e3be'},
             {'path': 'images/test-ubuntu-git.Dockerfile',
              'mode': '100644',
              'type': 'blob',
              'sha': '8b464c3d75c819f61428adc8483d28338464f3ba'},
             {'path': 'images/test-ubuntu-git.md',
              'mode': '100644',
              'type': 'blob',
              'sha': 'adf4bf6d421b67a8d70278291dbbce683ac1c99a'},
             {'path': 'jest.config.js',
              'mode': '100644',
              'type': 'blob',
              'sha': 'f09fe5c009cdd4ff3b62773d85c37aed84a2baef'},
             {'path': 'package-lock.json',
              'mode': '100644',
              'type': 'blob',
              'sha': 'aef29d3bce5069236ea8ed92827834d6381f2685'},
             {'path': 'package.json',
              'mode': '100644',
              'type': 'blob',
              'sha': 'dbbaabbae229c1c7404f819eefeec49a8b27d9f0'},
             {'path': 'src',
              'mode': '040000',
              'type': 'tree',
              'sha': '4ab2b279f2a58767b7740dd8e99daf7ef06bb72f'},
             {'path': 'src/fs-helper.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '00ad941ea9f93a9a4bb7b00a6f21244efcb59e43'},
             {'path': 'src/git-auth-helper.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '0c82dddabf9704020ac33271cdb4eb36c54d7cbf'},
             {'path': 'src/git-command-manager.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '9c789ac803e4d3b99463037d0ada654b79d8b863'},
             {'path': 'src/git-directory-helper.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '9a0085f21bba0a6116b8cfd8e6fed413223977d0'},
             {'path': 'src/git-source-provider.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '2d3513897655e0ea2eb4efc2b2c9d6132a69549f'},
             {'path': 'src/git-source-settings.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '79041c43476ab09af7235e83bec0507ac9ea0abc'},
             {'path': 'src/git-version.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '44bee1ae5944aa099703b8eb01f2dbcbed0dd08f'},
             {'path': 'src/github-api-helper.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '1ff27c2c7c5c5e7784f1cac2a6d8b7ca6e664c46'},
             {'path': 'src/input-helper.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '2ded955533070ad5a1cd5fd4b3f29e0b15e13833'},
             {'path': 'src/main.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '0684c6f5e202d1aa4952fc83b791bbc903a9dbd3'},
             {'path': 'src/misc',
              'mode': '040000',
              'type': 'tree',
              'sha': '5a399f31ad441263deac8145a53359b2e3690ba1'},
             {'path': 'src/misc/generate-docs.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '4b3c8ff529afee28d69e5c8fddf6b048c3f97cea'},
             {'path': 'src/misc/licensed-check.sh',
              'mode': '100755',
              'type': 'blob',
              'sha': '81987b6ca6901bdf8f794dcc1f6ee055e621d568'},
             {'path': 'src/misc/licensed-download.sh',
              'mode': '100755',
              'type': 'blob',
              'sha': '973e8e217658fd8bc24856a5c64403ffd14c4f51'},
             {'path': 'src/misc/licensed-generate.sh',
              'mode': '100755',
              'type': 'blob',
              'sha': 'd2e18774de43b5a6a7b8a8c9eab719b448ac21d6'},
             {'path': 'src/ref-helper.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '925be86614ded8ed4057bbbed4b3d15170f6e445'},
             {'path': 'src/regexp-helper.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': 'ec76c3ad3e78a5e3fd417da2c1aa9b837442da60'},
             {'path': 'src/retry-helper.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '323e75da6dbd09244db21786aebaed8065cb6d24'},
             {'path': 'src/state-helper.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': 'aa3eecc75f4f4078f60278ea3350eace0a4fb4c9'},
             {'path': 'src/unsafe-pr-checkout-helper.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': 'f6bf389cfb95375b8518606d36570fb6100e8517'},
             {'path': 'src/url-helper.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '17a0842a42876edc46514a3c59b7cb8442863cc1'},
             {'path': 'src/workflow-context-helper.ts',
              'mode': '100644',
              'type': 'blob',
              'sha': '5b342e6d34cb03f93b6a14ff854ecd45afc20a8e'},
             {'path': 'tsconfig.json',
              'mode': '100644',
              'type': 'blob',
              'sha': 'b0ff5f7fb9bfd78b4c371d92ebac5a29fcf836bc'}]}

RUNNER_LABEL = "ubuntu-24.04"
RUNNER_IMAGE_OS = "ubuntu24"
RUNNER_ARCH = "X64"
RUNNER_IMAGE_VERSION_RE = re.compile(
    r"^(?P<date>[0-9]{8})\.(?P<build>[0-9]+)\.(?P<revision>[0-9]+)$"
)
PYTHON_MAJOR_MINOR = (3, 12)
BASH_MAJOR_MINOR = (5, 2)
GIT_MIN_INCLUSIVE = (2, 43, 0)
GIT_MAX_EXCLUSIVE = (2, 60, 0)
CHECKOUT_NODE_RUNTIME = "node20"
RUNNER_IMAGES_REPOSITORY_ID = 190416463
RUNNER_IMAGES_REPOSITORY = "actions/runner-images"
RUNNER_IMAGE_RELEASE_API_PREFIX = (
    "https://api.github.com/repos/actions/runner-images/releases/tags/ubuntu24%2F"
)
RUNNER_IMAGE_INCLUDED_SOFTWARE_PATH = "images/ubuntu/Ubuntu2404-Readme.md"
RUNNER_IMAGE_MANIFEST_ASSET = "internal.ubuntu24.json"
SERVICE_DEPENDENCIES_SHA256 = "bb818694be90b7657563e76ba1e30c7dc9d4905d4030772bb4181cf377207382"
RUNTIME_ENVELOPE_SHA256 = "1a92e3daf7517b4717235c76dc61f619fc10687d8d8052f045ad33ef99c2acf9"
TRANSPORT_ENVELOPE_SHA256 = "f34a46563652a9c4b6d13e469730e2aa0e146c059f2e61354f45201bc9f3b304"
PRECHECKOUT_BOOTSTRAP_SHA256 = (
    "2d0c11afc6fd18b3141ee2a211e4dab4496465cfd6afb90112687e9c0ac33148"
)

ALLOWED_STATES = ("NO_DRIFT", "BLOCKED", "PREPARED_AUDIT_REQUIRED")
FORBIDDEN_STATES = frozenset(
    {"AUDITED", "READY", "READY_FOR_REVIEW", "ARMED", "MERGED"}
)
ALLOWED_TRANSPORTS = frozenset(
    {
        "git-https",
        "github-rest-quarantine-read",
        "github-rest-runner-image-release-read",
    }
)
API_ENDPOINT = "https://api.github.com"
QUARANTINE_API_PATHS = (
    "/repos/KraineOpasen/bukerov-twitch-miner-go/pulls/223",
    "/repos/KraineOpasen/bukerov-twitch-miner-go/pulls/233",
    "/repos/KraineOpasen/bukerov-twitch-miner-go/pulls/241",
    "/repos/KraineOpasen/bukerov-twitch-miner-go/issues/230",
    "/repos/KraineOpasen/bukerov-twitch-miner-go/issues/238",
    "/repos/KraineOpasen/bukerov-twitch-miner-go/issues/239",
    "/repos/KraineOpasen/bukerov-twitch-miner-go/issues/240",
)

POLICY_REL = Path("docs/agents/skills-maintenance/policy.json")
CONTROL_REL = Path("docs/agents/skills-maintenance/control-plane.json")
QUARANTINE_REL = Path("docs/agents/skills-maintenance/legacy-quarantine.json")
DEPENDENCIES_REL = Path("docs/agents/skills-maintenance/external-dependencies.json")
SCHEMAS_REL = Path("docs/agents/skills-maintenance/schemas")

# Exact frozen-R0 control authority.  No glob or directory walk may silently add
# a future provider, rule, workflow, or G1.2 surface to a proposal identity.
CONTROL_INPUT_MEMBERS = (
    ".claude/rules/github-governance.md",
    ".github/workflows/ci.yml",
    ".github/workflows/stable-release.yml",
    ".github/workflows/stable-skills-maintenance.yml",
    "CLAUDE.md",
    "GOVERNANCE_V3.md",
    "docs/agents/anthropic-skills-manifest.json",
    "docs/agents/anthropic-skills-patches.md",
    "docs/agents/anthropic-skills-policy.md",
    "docs/agents/awesome-copilot-skills-manifest.json",
    "docs/agents/awesome-copilot-skills-patches.md",
    "docs/agents/awesome-copilot-skills-policy.md",
    "docs/agents/builderio-skills-manifest.json",
    "docs/agents/builderio-skills-patches.md",
    "docs/agents/builderio-skills-policy.md",
    "docs/agents/compound-engineering-skills-manifest.json",
    "docs/agents/compound-engineering-skills-patches.md",
    "docs/agents/compound-engineering-skills-policy.md",
    "docs/agents/mattpocock-skills-manifest.json",
    "docs/agents/mattpocock-skills-patches.md",
    "docs/agents/mattpocock-skills-policy.md",
    "docs/agents/skills-maintenance/THIRD_PARTY_NOTICES.md",
    "docs/agents/skills-maintenance/control-plane.json",
    "docs/agents/skills-maintenance/external-dependencies.json",
    "docs/agents/skills-maintenance/legacy-quarantine.json",
    "docs/agents/skills-maintenance/policy.json",
    "docs/agents/skills-maintenance/schemas/control-plane.schema.json",
    "docs/agents/skills-maintenance/schemas/external-dependencies.schema.json",
    "docs/agents/skills-maintenance/schemas/legacy-quarantine.schema.json",
    "docs/agents/skills-maintenance/schemas/policy.schema.json",
    "docs/agents/skills-routing.md",
    "docs/agents/skills-update-plugins.json",
    "docs/agents/skills-update-providers.json",
    "docs/agents/trailofbits-skills-manifest.json",
    "docs/agents/trailofbits-skills-patches.md",
    "docs/agents/trailofbits-skills-policy.md",
    "scripts/validate-agent-governance.py",
)

LIVENESS_FAILURE_CLASSES = (
    "WORKFLOW_DISABLED",
    "WORKFLOW_MISSING",
    "WORKFLOW_DIGEST_MISMATCH",
    "ORCHESTRATOR_FAILURE",
    "ORCHESTRATOR_CANCELLED",
    "ORCHESTRATOR_TIMED_OUT",
    "ORCHESTRATOR_ACTION_REQUIRED",
    "ORCHESTRATOR_STALE",
    "RECONCILE_ALL_MISSING_OR_FAILED",
    "WRONG_DEFAULT_HEAD",
    "CONTROL_INPUT_DIGEST_MISMATCH",
    "QUALIFYING_SUCCESS_ABSENT_AFTER_GRACE",
)


def _expected_liveness_contract() -> dict[str, Any]:
    unavailable_workflows = [
        {
            "path": path,
            "workflow_id": None,
            "trusted_file_blob_sha": None,
            "state": "UNAVAILABLE",
        }
        for path in (".github/workflows/ci.yml", WORKFLOW_PATH)
    ]
    return {
        "state": "UNAVAILABLE",
        "detector_only_is_healthy": False,
        "schedule": {"cron": "37 5 * * *", "grace_seconds": 172800},
        "expected_control": {
            "repository_id": REPOSITORY_ID,
            "repository_full_name": REPOSITORY_FULL_NAME,
            "default_branch": STABLE_BRANCH,
            "stable_branch": STABLE_BRANCH,
            "workflow": {
                "path": WORKFLOW_PATH,
                "workflow_id": None,
                "trusted_file_blob_sha": None,
                "state": "UNAVAILABLE",
            },
            "closed_workflow_set": {
                "schema": "closed-workflow-set/v1",
                "algorithm": "sha256-jcs",
                "workflows": unavailable_workflows,
                "digest": None,
                "readback_state": "UNAVAILABLE",
            },
            "aggregate_control_plane_identity": {
                "state": "UNAVAILABLE",
                "schema": "control-plane-identity/v1",
                "algorithm": "sha256-jcs",
                "digest": None,
                "binds": [
                    "control_input_digest",
                    "updater_source_sha",
                    "pinned_action_digests",
                    "closed_workflow_set",
                ],
            },
        },
        "qualifying_run": {
            "event": "schedule",
            "status": "completed",
            "conclusion": "success",
            "workflow": {
                "path": WORKFLOW_PATH,
                "workflow_id": "CURRENT_STABLE_ORCHESTRATOR_WORKFLOW_ID",
                "trusted_file_blob_sha": (
                    "CURRENT_STABLE_ORCHESTRATOR_TRUSTED_FILE_BLOB_SHA"
                ),
                "state": "active",
            },
            "head_sha": "CURRENT_LIVE_DEFAULT_HEAD",
            "control_input_digest": "CURRENT_CONTROL_INPUT_DIGEST",
            "aggregate_control_plane_identity": (
                "CURRENT_AGGREGATE_CONTROL_PLANE_IDENTITY"
            ),
            "required_job": {
                "yaml_key": RECONCILE_JOB_KEY,
                "api_name": RECONCILE_JOB_NAME,
                "conclusion": "success",
            },
            "max_age_seconds": 172800,
            "ancestor_exception": {
                "allowed": True,
                "requires": [
                    "PROVED_ANCESTOR_OF_CURRENT_LIVE_DEFAULT_HEAD",
                    "IDENTICAL_CLOSED_WORKFLOW_SET_DIGEST",
                    "IDENTICAL_AGGREGATE_CONTROL_PLANE_IDENTITY",
                ],
            },
        },
        "failure_classes": list(LIVENESS_FAILURE_CLASSES),
        "incident_policy": {
            "dedupe": {
                "schema": "liveness-incident/v1",
                "algorithm": "sha256-jcs",
                "binds": [
                    "monitor_policy_digest",
                    "workflow_id",
                    "failure_class",
                    "first_bad_run_or_grace_epoch",
                ],
            },
            "recovery": {
                "requires_later_qualifying_full_orchestrator_success": True,
                "requires_every_workflow_active_and_digest_correct": True,
                "detector_only_sufficient": False,
                "send_healthy_or_repeated_message": False,
                "inactivity_disabled_workflow_requires_manual_reenable": True,
            },
        },
        "external_heartbeat": {
            "state": "UNCOMMISSIONED",
            "provider": None,
            "account": None,
            "config": None,
            "owner_action_required": True,
        },
    }

SHA1_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
PROVIDER_RE = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)*$")
STABLE_BRANCH_RE = re.compile(r"^release/[0-9]+\.[0-9]+$")

_DONOR_VALIDATOR_SHA256 = (
    "88529252f9aec2ce088a975c6bb0422762560043f092d00916e1aad64378ee5c"
)
_EXPECTED_GOVERNANCE_WORKFLOWS = frozenset(
    {".github/workflows/ci.yml", WORKFLOW_PATH}
)
_AUTH_ENV_KEYS = frozenset(
    {
        "GITHUB_TOKEN",
        "GH_TOKEN",
        "GITHUB_PAT",
        "CONTROL_PLANE_TOKEN",
        "GIT_ASKPASS",
        "SSH_ASKPASS",
        "GIT_SSH",
        "GIT_SSH_COMMAND",
        "SSH_AUTH_SOCK",
    }
)
_PROXY_ENV_KEYS = frozenset(
    {"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"}
)
_TRUST_OVERRIDE_ENV_KEYS = frozenset(
    {
        "GIT_SSL_CAINFO",
        "GIT_SSL_NO_VERIFY",
        "SSL_CERT_FILE",
        "SSL_CERT_DIR",
        "OPENSSL_CONF",
        "OPENSSL_MODULES",
        "PYTHONHTTPSVERIFY",
        "SSLKEYLOGFILE",
        "GIT_PROXY_SSL_CAINFO",
        "CURL_CA_BUNDLE",
        "NO_PROXY",
        "no_proxy",
    }
)
_EXECUTION_OVERRIDE_ENV_KEYS = frozenset(
    {
        "NODE_OPTIONS",
        "NODE_EXTRA_CA_CERTS",
        "NODE_TLS_REJECT_UNAUTHORIZED",
        "GIT_CONFIG_COUNT",
        "GIT_CONFIG_KEY_0",
        "GIT_CONFIG_VALUE_0",
        "GIT_CONFIG_PARAMETERS",
        "GIT_EXEC_PATH",
        "GIT_TEMPLATE_DIR",
    }
)

# These are the only Git-prefixed variables authored by the workflow before
# checkout.  Checking the complete ``GIT_*`` namespace (rather than a growing
# denylist) also excludes every Git trace/output/config injection variable.
_CONTROLLED_PRECHECKOUT_GIT_ENV = MappingProxyType(
    {
        "GIT_ALLOW_PROTOCOL": "https",
        "GIT_CONFIG_GLOBAL": "/dev/null",
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_TERMINAL_PROMPT": "0",
    }
)
_RESERVED_PROVIDER_ENV_PREFIXES = ("ACTIONS_", "GITHUB_", "RUNNER_")
_RESERVED_PROVIDER_ENV_NAMES = frozenset({"CI", "ImageOS", "ImageVersion"})
_PRECHECKOUT_BOOTSTRAP_RE = re.compile(
    r"(?ms)^ {10}python3 -I -S -B <<'PY'\n(?P<body>.*?)^ {10}PY[ \t]*$"
)


class RuntimeEnvelopeError(RuntimeError):
    """A committed or live runtime/control fact did not match the G1.1 envelope."""


@dataclass(frozen=True)
class CandidateIdentity:
    """One discriminated provider-update proposal identity."""

    schema: str
    repo_id: int
    repo_full_name: str
    stable_branch: str
    stable_base_sha: str
    provider: str
    upstream_repo: str
    old_pin: str
    target_sha: str
    control_input_digest: str
    updater_source_sha: str
    pinned_action_digests: Mapping[str, str]
    proposal_id: str
    locator: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema": self.schema,
            "repo_id": self.repo_id,
            "repo_full_name": self.repo_full_name,
            "stable_branch": self.stable_branch,
            "stable_base_sha": self.stable_base_sha,
            "provider": self.provider,
            "upstream_repo": self.upstream_repo,
            "old_pin": self.old_pin,
            "target_sha": self.target_sha,
            "control_input_digest": self.control_input_digest,
            "updater_source_sha": self.updater_source_sha,
            "pinned_action_digests": dict(self.pinned_action_digests),
            "proposal_id": self.proposal_id,
            "locator": self.locator,
        }

    as_dict = to_dict


@dataclass(frozen=True)
class RuntimeContract:
    """Validated committed G1.1 contract set and its exact byte digests."""

    policy: Mapping[str, Any]
    control_plane: Mapping[str, Any]
    quarantine: Mapping[str, Any]
    external_dependencies: Mapping[str, Any]
    policy_sha256: str
    control_plane_sha256: str
    quarantine_sha256: str
    external_dependencies_sha256: str
    control_input_digest: str
    updater_source_sha: str
    pinned_action_digests: Mapping[str, str]

    @property
    def repository_id(self) -> int:
        return int(self.policy["repository"]["id"])

    @property
    def repository_full_name(self) -> str:
        return str(self.policy["repository"]["full_name"])

    @property
    def stable_branch(self) -> str:
        return str(self.policy["stable"]["branch"])

    @property
    def foundation_base_sha(self) -> str:
        """Historical implementation parent; never an operational live base."""

        return str(self.policy["stable"]["g11_implementation_parent_sha"])

    @property
    def g11_implementation_parent_sha(self) -> str:
        return self.foundation_base_sha

    @property
    def workflow_path(self) -> str:
        return str(self.policy["workflow"]["path"])

    @property
    def expected_workflow_ref(self) -> str:
        return str(self.control_plane["workflow"]["expected_workflow_ref"])


@dataclass(frozen=True)
class VerifiedControlIdentity:
    """Live tokenless workflow identity proved immediately before artifact writes."""

    contract: RuntimeContract
    selected_ref: str
    selected_sha: str
    live_default_branch: str
    fetched_default_sha: str


def _root(repo_root: os.PathLike[str] | str) -> Path:
    root = Path(repo_root).resolve()
    if not root.is_dir():
        raise RuntimeEnvelopeError("repository root is not a directory: %s" % root)
    return root


def _raw_sha256(path: Path) -> str:
    try:
        return hashlib.sha256(path.read_bytes()).hexdigest()
    except OSError as exc:
        raise RuntimeEnvelopeError("cannot read %s: %s" % (path, exc)) from exc


def _canonical_json_sha256(value: Any) -> str:
    """SHA-256 of JCS bytes for the closed string/integer G1.1 data model.

    All authoritative payload keys and values are ASCII (apart from no free-form text),
    contain no floats, and therefore Python's sorted compact UTF-8 JSON is byte-for-byte
    RFC 8785 JCS for this deliberately constrained domain.
    """

    payload = json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")
    return hashlib.sha256(payload).hexdigest()


def _unique_json_object(pairs: Sequence[tuple[str, Any]]) -> dict[str, Any]:
    value: dict[str, Any] = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("duplicate JSON key %r" % key)
        value[key] = item
    return value


def _reject_json_constant(value: str) -> None:
    raise ValueError("non-JSON numeric constant %r" % value)


def _parse_unique_json(raw: str, where: str) -> Any:
    try:
        return json.loads(
            raw,
            object_pairs_hook=_unique_json_object,
            parse_constant=_reject_json_constant,
        )
    except (TypeError, ValueError) as exc:
        raise RuntimeEnvelopeError("invalid JSON in %s: %s" % (where, exc)) from exc


def _read_json(root: Path, rel: Path) -> dict[str, Any]:
    path = root / rel
    try:
        raw = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise RuntimeEnvelopeError("missing or unreadable contract %s: %s" % (rel, exc)) from exc
    value = _parse_unique_json(raw, str(rel))
    if not isinstance(value, dict):
        raise RuntimeEnvelopeError("contract %s must be a JSON object" % rel)
    return value


def _require_exact_keys(value: Mapping[str, Any], keys: set[str], where: str) -> None:
    actual = set(value)
    if actual != keys:
        raise RuntimeEnvelopeError(
            "%s keys mismatch: missing=%s unexpected=%s"
            % (where, sorted(keys - actual), sorted(actual - keys))
        )


def _require_sha1(value: Any, where: str) -> str:
    if not isinstance(value, str) or SHA1_RE.fullmatch(value) is None:
        raise RuntimeEnvelopeError("%s must be a full lowercase 40-hex SHA" % where)
    return value


def _require_sha256(value: Any, where: str) -> str:
    if not isinstance(value, str) or SHA256_RE.fullmatch(value) is None:
        raise RuntimeEnvelopeError("%s must be a full lowercase SHA-256" % where)
    return value


def _freeze(value: Any) -> Any:
    if isinstance(value, dict):
        return MappingProxyType({key: _freeze(item) for key, item in value.items()})
    if isinstance(value, list):
        return tuple(_freeze(item) for item in value)
    return value


def _thaw(value: Any) -> Any:
    """Return a JSON-serializable copy of a recursively frozen contract value."""

    if isinstance(value, Mapping):
        return {key: _thaw(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_thaw(item) for item in value]
    return value


def _validate_policy(policy: Mapping[str, Any]) -> None:
    _require_exact_keys(
        policy,
        {
            "$schema",
            "schema_version",
            "policy_id",
            "repository",
            "stable",
            "workflow",
            "control_input",
            "candidate_identity",
            "allowed_production_states",
            "forbidden_capabilities",
            "publication_authority",
            "failure_policy",
        },
        "policy",
    )
    if policy["$schema"] != "./schemas/policy.schema.json" or policy["schema_version"] != 1:
        raise RuntimeEnvelopeError("policy schema identity mismatch")
    if policy["policy_id"] != "g1.1-stable-native-deterministic-foundation":
        raise RuntimeEnvelopeError("policy_id mismatch")
    repo = policy["repository"]
    if repo != {
        "id": REPOSITORY_ID,
        "full_name": REPOSITORY_FULL_NAME,
        "canonical_https_url": REPOSITORY_HTTPS_URL,
    }:
        raise RuntimeEnvelopeError("policy repository identity mismatch")
    stable = policy["stable"]
    if stable != {
        "branch": STABLE_BRANCH,
        "base_mode": "live-default-head",
        "g11_implementation_parent_sha": G11_IMPLEMENTATION_PARENT_SHA,
        "historical_ancestry_marker": HISTORICAL_ANCESTRY_MARKER,
    }:
        raise RuntimeEnvelopeError("policy stable identity mismatch")
    workflow = policy["workflow"]
    if workflow != {
        "path": WORKFLOW_PATH,
        "execution_mode": "ARTIFACT_ONLY",
        "selected_ref_must_equal_stable_branch": True,
    }:
        raise RuntimeEnvelopeError("policy workflow envelope mismatch")
    if policy["control_input"] != {
        "schema": "control-input/v1",
        "member_mode": "100644",
        "members": list(CONTROL_INPUT_MEMBERS),
    }:
        raise RuntimeEnvelopeError("control-input membership mismatch")
    identity = policy["candidate_identity"]
    if identity != {
        "schema": CANDIDATE_SCHEMA,
        "algorithm": "sha256-jcs",
        "binds": [
            "schema",
            "repo_id",
            "repo_full_name",
            "stable_branch",
            "stable_base_sha",
            "provider",
            "upstream_repo",
            "old_pin",
            "target_sha",
            "control_input_digest",
            "updater_source_sha",
            "pinned_action_digests",
        ],
    }:
        raise RuntimeEnvelopeError("candidate identity contract mismatch")
    if policy["allowed_production_states"] != list(ALLOWED_STATES):
        raise RuntimeEnvelopeError("allowed production states mismatch")
    forbidden = policy["forbidden_capabilities"]
    expected_forbidden = [
        "SEMANTIC_AUDIT",
        "AUDITED_STATE",
        "READY_FOR_REVIEW",
        "REF_PUBLICATION",
        "PULL_REQUEST_PUBLICATION",
        "ISSUE_PUBLICATION",
        "AUTO_MERGE",
        "MERGE",
        "SIBLING_INSTALLATION",
        "MODEL_INVOCATION",
    ]
    if forbidden != expected_forbidden:
        raise RuntimeEnvelopeError("forbidden capability set mismatch")
    if policy["publication_authority"] is not False:
        raise RuntimeEnvelopeError("G1.1 cannot have publication authority")
    if policy["failure_policy"] != "FAIL_CLOSED":
        raise RuntimeEnvelopeError("policy must fail closed")


def _validate_control(control: Mapping[str, Any]) -> None:
    _require_exact_keys(
        control,
        {
            "$schema",
            "schema_version",
            "control_plane_id",
            "repository",
            "stable",
            "workflow",
            "default_branch",
            "commissioning",
            "liveness",
            "contracts",
            "owner_actions_after_g1_1",
        },
        "control-plane",
    )
    if control["$schema"] != "./schemas/control-plane.schema.json" or control["schema_version"] != 1:
        raise RuntimeEnvelopeError("control-plane schema identity mismatch")
    if control["control_plane_id"] != "g1.1-stable-native-control-plane":
        raise RuntimeEnvelopeError("control_plane_id mismatch")
    if control["repository"] != {"id": REPOSITORY_ID, "full_name": REPOSITORY_FULL_NAME}:
        raise RuntimeEnvelopeError("control-plane repository identity mismatch")
    if control["stable"] != {
        "branch": STABLE_BRANCH,
        "base_mode": "live-default-head",
        "g11_implementation_parent_sha": G11_IMPLEMENTATION_PARENT_SHA,
    }:
        raise RuntimeEnvelopeError("control-plane stable identity mismatch")
    workflow = control["workflow"]
    if workflow != {
        "path": WORKFLOW_PATH,
        "expected_workflow_ref": EXPECTED_WORKFLOW_REF,
        "historical_donor_path": DONOR_WORKFLOW_PATH,
        "execution_mode": "ARTIFACT_ONLY",
        "publication_authority": False,
    }:
        raise RuntimeEnvelopeError("control-plane workflow identity mismatch")
    default = control["default_branch"]
    if default != {
        "observed_at": "2026-08-27T12:15:58Z",
        "observed_name": OBSERVED_DEFAULT_BRANCH,
        "observed_head_sha": OBSERVED_DEFAULT_SHA,
        "required_for_commissioning": STABLE_BRANCH,
        "stable_is_default_observed": False,
    }:
        raise RuntimeEnvelopeError("default-branch relation mismatch")
    commissioning = control["commissioning"]
    if commissioning != {
        "state": "UNCOMMISSIONED",
        "owner_action_required": True,
        "workflow_enabled_assumed": False,
        "workflow_id": None,
        "workflow_state": "UNAVAILABLE",
    }:
        raise RuntimeEnvelopeError("commissioning facts are not exact and inert")
    liveness = control["liveness"]
    if liveness != _expected_liveness_contract():
        raise RuntimeEnvelopeError("liveness contract mismatch")
    if control["contracts"] != {
        "policy": POLICY_REL.as_posix(),
        "legacy_quarantine": QUARANTINE_REL.as_posix(),
        "external_dependencies": DEPENDENCIES_REL.as_posix(),
    }:
        raise RuntimeEnvelopeError("control-plane contract paths mismatch")
    if control["owner_actions_after_g1_1"] != [
        "REVERIFY_MERGED_STABLE_AND_LIVE_MIGRATION_INPUTS",
        "SNAPSHOT_COMPLETE_WORKFLOW_RUN_SETTINGS_RULESET_INTEGRATION_AND_QUARANTINE_STATE",
        "DISABLE_DRAIN_AND_PROVE_ZERO_HISTORICAL_OR_DYNAMIC_MUTATION",
        "FREEZE_MAIN_AND_NORMALIZE_STABLE_PROTECTION_WITHOUT_A_GAP",
        "CHANGE_DEFAULT_BRANCH_TO_RELEASE_0_3_WITH_AUTO_MERGE_DISABLED",
        "VERIFY_POST_SWITCH_DEFAULT_RULES_PR_BASES_WORKFLOW_AND_INTEGRATION_STATE",
        "VERIFY_NEW_WORKFLOW_REGISTRATION_AND_ENABLEMENT",
        "SELECT_CONFIGURE_AND_TEST_EXTERNAL_FULL_ORCHESTRATOR_HEARTBEAT",
    ]:
        raise RuntimeEnvelopeError("owner-action boundary mismatch")


def _expected_quarantine_numbers() -> dict[str, set[int]]:
    return {"pull_request": {223, 233, 241}, "issue": {230, 238, 239, 240}}


def _validate_quarantine_document(
    quarantine: Mapping[str, Any], *, policy_sha256: str, control_sha256: str
) -> None:
    _require_exact_keys(
        quarantine,
        {
            "$schema",
            "schema_version",
            "quarantine_id",
            "repository",
            "observed_at",
            "binding",
            "authority",
            "forbidden_operations",
            "subjects",
        },
        "legacy-quarantine",
    )
    if quarantine["$schema"] != "./schemas/legacy-quarantine.schema.json" or quarantine["schema_version"] != 1:
        raise RuntimeEnvelopeError("legacy-quarantine schema identity mismatch")
    if quarantine["quarantine_id"] != "g1.1-legacy-default-main-quarantine":
        raise RuntimeEnvelopeError("legacy quarantine id mismatch")
    if quarantine["repository"] != {"id": REPOSITORY_ID, "full_name": REPOSITORY_FULL_NAME}:
        raise RuntimeEnvelopeError("quarantine repository identity mismatch")
    if quarantine["observed_at"] != "2026-08-27T17:00:03Z":
        raise RuntimeEnvelopeError("quarantine observation time mismatch")
    binding = quarantine["binding"]
    _require_exact_keys(
        binding,
        {
            "stable_branch",
            "g11_implementation_parent_sha",
            "observed_default_branch",
            "observed_default_sha",
            "policy_path",
            "policy_sha256",
            "control_plane_path",
            "control_plane_sha256",
            "subjects_sha256",
        },
        "legacy-quarantine.binding",
    )
    if (
        binding["stable_branch"] != STABLE_BRANCH
        or binding["g11_implementation_parent_sha"] != G11_IMPLEMENTATION_PARENT_SHA
    ):
        raise RuntimeEnvelopeError("quarantine stable base mismatch")
    if binding["observed_default_branch"] != OBSERVED_DEFAULT_BRANCH or binding["observed_default_sha"] != OBSERVED_DEFAULT_SHA:
        raise RuntimeEnvelopeError("quarantine default control mismatch")
    if binding["policy_path"] != POLICY_REL.as_posix() or binding["policy_sha256"] != policy_sha256:
        raise RuntimeEnvelopeError("quarantine policy digest mismatch")
    if binding["control_plane_path"] != CONTROL_REL.as_posix() or binding["control_plane_sha256"] != control_sha256:
        raise RuntimeEnvelopeError("quarantine control-plane digest mismatch")
    subjects = quarantine["subjects"]
    if not isinstance(subjects, list) or len(subjects) != 7:
        raise RuntimeEnvelopeError("legacy quarantine must contain exactly seven subjects")
    if binding["subjects_sha256"] != _canonical_json_sha256(subjects):
        raise RuntimeEnvelopeError("quarantine subjects digest mismatch")
    if quarantine["authority"] != "OBSERVE_AND_REPORT_ONLY":
        raise RuntimeEnvelopeError("quarantine authority mismatch")
    forbidden = quarantine["forbidden_operations"]
    if forbidden != [
        "UPDATE",
        "CLOSE",
        "REOPEN",
        "RETARGET",
        "SUPERSEDE",
        "DUPLICATE",
        "COMMENT",
        "RECREATE",
        "ADOPT_AS_AUTHORITY",
        "USE_AS_PROMOTION_EVIDENCE",
    ]:
        raise RuntimeEnvelopeError("quarantine operation boundary mismatch")

    expected = _expected_quarantine_numbers()
    seen = {"pull_request": set(), "issue": set()}
    nodes: set[str] = set()
    for index, subject in enumerate(subjects):
        where = "legacy-quarantine.subjects[%d]" % index
        if not isinstance(subject, dict):
            raise RuntimeEnvelopeError("%s must be an object" % where)
        common = {
            "kind",
            "number",
            "node_id",
            "title",
            "state",
            "author",
            "provider",
            "target_sha",
        }
        kind = subject.get("kind")
        if kind == "pull_request":
            allowed_states = {"OPEN"}
            _require_exact_keys(
                subject,
                common | {"draft", "head_ref", "head_sha", "base_ref", "base_sha"},
                where,
            )
            if subject["draft"] is not True:
                raise RuntimeEnvelopeError("%s must remain a draft" % where)
            if not isinstance(subject["head_ref"], str) or not subject["head_ref"].startswith("automated/skills-update/"):
                raise RuntimeEnvelopeError("%s head ref mismatch" % where)
            _require_sha1(subject["head_sha"], where + ".head_sha")
            if subject["base_ref"] != OBSERVED_DEFAULT_BRANCH or subject["base_sha"] != OBSERVED_DEFAULT_SHA:
                raise RuntimeEnvelopeError("%s base mismatch" % where)
        elif kind == "issue":
            allowed_states = {"OPEN", "CLOSED"}
            _require_exact_keys(subject, common | {"label"}, where)
            if subject["label"] not in {"skills-update-blocked", "skills-update-discovery"}:
                raise RuntimeEnvelopeError("%s label mismatch" % where)
        else:
            raise RuntimeEnvelopeError("%s has unknown subject kind" % where)
        number = subject["number"]
        if not isinstance(number, int) or number not in expected[kind]:
            raise RuntimeEnvelopeError("%s number mismatch" % where)
        seen[kind].add(number)
        node = subject["node_id"]
        if not isinstance(node, str) or not node or node in nodes:
            raise RuntimeEnvelopeError("%s node_id missing or duplicate" % where)
        nodes.add(node)
        if subject["state"] not in allowed_states or subject["author"] != "github-actions[bot]":
            raise RuntimeEnvelopeError("%s live identity fields mismatch" % where)
        if not isinstance(subject["title"], str) or not subject["title"]:
            raise RuntimeEnvelopeError("%s title missing" % where)
        if not isinstance(subject["provider"], str) or PROVIDER_RE.fullmatch(subject["provider"]) is None:
            raise RuntimeEnvelopeError("%s provider mismatch" % where)
        _require_sha1(subject["target_sha"], where + ".target_sha")
    if seen != expected:
        raise RuntimeEnvelopeError("legacy quarantine subject set mismatch")


def _checkout_input_envelope(fetch_depth: int, *, workflow_scalars: bool) -> dict[str, Any]:
    """Return the one audited checkout input map for a specific consumer profile."""
    if fetch_depth not in (0, 1):
        raise RuntimeEnvelopeError("checkout fetch depth is outside the audited profiles")
    if workflow_scalars:
        return {
            "clean": "true",
            "fetch-depth": str(fetch_depth),
            "fetch-tags": "false",
            "lfs": "false",
            "persist-credentials": "false",
            "ref": "${{ github.sha }}",
            "set-safe-directory": "false",
            "show-progress": "false",
            "submodules": "false",
        }
    return {
        "clean": True,
        "fetch-depth": fetch_depth,
        "fetch-tags": False,
        "lfs": False,
        "persist-credentials": False,
        "ref": "${{ github.sha }}",
        "set-safe-directory": False,
        "show-progress": False,
        "submodules": False,
    }


def _validate_dependencies_document(ledger: Mapping[str, Any]) -> None:
    _require_exact_keys(
        ledger,
        {
            "$schema",
            "schema_version",
            "ledger_id",
            "policy",
            "dependencies",
            "runtime",
            "transport",
        },
        "external-dependencies",
    )
    if ledger["$schema"] != "./schemas/external-dependencies.schema.json" or ledger["schema_version"] != 1:
        raise RuntimeEnvelopeError("external-dependencies schema identity mismatch")
    if ledger["ledger_id"] != "g1.1-external-runtime-envelope" or ledger["policy"] != "CLOSED_FAIL_CLOSED":
        raise RuntimeEnvelopeError("external dependency ledger identity mismatch")
    dependencies = ledger["dependencies"]
    if not isinstance(dependencies, list) or len(dependencies) != 3:
        raise RuntimeEnvelopeError("dependency closure must contain one Action and two services")
    dep = dependencies[0]
    expected_keys = {
        "kind", "id", "owning_concern", "consumers", "source", "commit",
        "tree", "license", "action", "dist", "package_lock", "tree_manifest",
        "inputs", "permissions",
        "network", "transitive_closure", "drift_detector", "applicability",
        "data_retention", "rollback_owner", "provenance",
    }
    _require_exact_keys(dep, expected_keys, "external-dependencies.dependencies[0]")
    if dep["kind"] != "github-action" or dep["id"] != "actions/checkout":
        raise RuntimeEnvelopeError("dependency kind/id mismatch")
    if dep["owning_concern"] != "G1.1_STABLE_NATIVE_RUNTIME":
        raise RuntimeEnvelopeError("dependency owner mismatch")
    if dep["consumers"] != [
        {"workflow": WORKFLOW_PATH, "job": "detect"},
        {"workflow": WORKFLOW_PATH, "job": "prepare"},
        {"workflow": ".github/workflows/ci.yml", "job": "governance"},
    ]:
        raise RuntimeEnvelopeError("dependency consumer closure mismatch")
    if dep["source"] != {
        "provider": "GitHub",
        "repository_id": 197814629,
        "repository_full_name": "actions/checkout",
        "canonical_https_url": "https://github.com/actions/checkout",
    }:
        raise RuntimeEnvelopeError("actions/checkout source identity mismatch")
    if dep["commit"] != CHECKOUT_COMMIT or dep["tree"] != CHECKOUT_TREE:
        raise RuntimeEnvelopeError("actions/checkout immutable identity mismatch")
    _require_sha1(dep["commit"], "actions/checkout commit")
    _require_sha1(dep["tree"], "actions/checkout tree")
    if dep["license"] != {
        "spdx": "MIT",
        "path": "LICENSE",
        "blob_sha1": "a67dca8b4f65d6bd351f6b1e333ce2cd84d843a5",
        "sha256": CHECKOUT_LICENSE_SHA256,
        "notice_path": "docs/agents/skills-maintenance/THIRD_PARTY_NOTICES.md",
    }:
        raise RuntimeEnvelopeError("actions/checkout licence identity mismatch")
    if dep["action"] != {
        "path": "action.yml",
        "blob_sha1": "24e73e5a12126edc2adb9e5cd1bf245ce85bde56",
        "sha256": CHECKOUT_ACTION_SHA256,
        "using": "node20",
        "main": "dist/index.js",
        "post": "dist/index.js",
    }:
        raise RuntimeEnvelopeError("actions/checkout action descriptor mismatch")
    if dep["dist"] != {
        "path": "dist/index.js",
        "blob_sha1": CHECKOUT_DIST_BLOB_SHA1,
        "sha256": CHECKOUT_DIST_SHA256,
    }:
        raise RuntimeEnvelopeError("actions/checkout executable bundle mismatch")
    if dep["package_lock"] != {
        "path": "package-lock.json",
        "blob_sha1": CHECKOUT_PACKAGE_LOCK_BLOB_SHA1,
        "sha256": CHECKOUT_PACKAGE_LOCK_SHA256,
    }:
        raise RuntimeEnvelopeError("actions/checkout package-lock evidence mismatch")
    if dep["tree_manifest"] != {
        "kind": "full-recursive-git-tree",
        "algorithm": "sha256-jcs",
        "formula": "sha256(JCS(manifest))",
        "source": {
            "root": CHECKOUT_TREE,
            "recursive": True,
            "truncated": CHECKOUT_TREE_TRUNCATED,
            "entry_count": CHECKOUT_TREE_ENTRY_COUNT,
        },
        "manifest": CHECKOUT_TREE_MANIFEST,
        "digest": CHECKOUT_TREE_MANIFEST_SHA256,
        "candidate_pin_key": CHECKOUT_PIN_KEY,
    }:
        raise RuntimeEnvelopeError("actions/checkout full-tree manifest mismatch")
    if _canonical_json_sha256(CHECKOUT_TREE_MANIFEST) != CHECKOUT_TREE_MANIFEST_SHA256:
        raise RuntimeEnvelopeError("compiled actions/checkout full-tree digest mismatch")
    members = CHECKOUT_TREE_MANIFEST["members"]
    if (
        CHECKOUT_TREE_TRUNCATED
        or len(members) != 117
        or sum(item["type"] == "blob" for item in members) != 104
        or sum(item["type"] == "tree" for item in members) != 13
        or members != sorted(members, key=lambda item: item["path"].encode("utf-8"))
    ):
        raise RuntimeEnvelopeError("actions/checkout recursive tree closure mismatch")
    stable_inputs = _checkout_input_envelope(1, workflow_scalars=False)
    ci_inputs = _checkout_input_envelope(0, workflow_scalars=False)
    if dep["inputs"] != {
        WORKFLOW_PATH + "#detect": stable_inputs,
        WORKFLOW_PATH + "#prepare": stable_inputs,
        ".github/workflows/ci.yml#governance": ci_inputs,
    }:
        raise RuntimeEnvelopeError("actions/checkout input envelope mismatch")
    if dep["permissions"] != ["contents:read"] or dep["network"] != ["https://github.com"]:
        raise RuntimeEnvelopeError("actions/checkout permission/network mismatch")
    if dep["transitive_closure"] != {
        "binding": "full-recursive-git-tree-manifest",
        "tree_sha1": CHECKOUT_TREE,
        "manifest_sha256": CHECKOUT_TREE_MANIFEST_SHA256,
        "member_count": 117,
        "blob_count": 104,
        "tree_count": 13,
        "truncated": False,
        "package_lock_blob_sha1": CHECKOUT_PACKAGE_LOCK_BLOB_SHA1,
        "package_lock_sha256": CHECKOUT_PACKAGE_LOCK_SHA256,
        "bundled_license_metadata": {
            "prefix": ".licenses/npm/",
            "entry_count": 33,
            "blob_count": 30,
            "tree_count": 3,
        },
        "package_install": False,
        "upstream_script_execution": False,
    }:
        raise RuntimeEnvelopeError("actions/checkout transitive closure mismatch")
    if dep["drift_detector"] != {
        "method": (
            "exact-full-tree-manifest-license-action-dist-lock-input-consumer-"
            "real-worktree-head"
        ),
        "on_mismatch": "BLOCKED",
    }:
        raise RuntimeEnvelopeError("actions/checkout drift detector mismatch")
    if dep["applicability"] != {
        "runner": RUNNER_LABEL,
        "stable_jobs_trusted_event_head": True,
        "ci_untrusted_pr_read_only": True,
        "secret_bearing": False,
    }:
        raise RuntimeEnvelopeError("actions/checkout applicability mismatch")
    if dep["data_retention"] != "GITHUB_HOSTED_RUNNER_EPHEMERAL" or dep["rollback_owner"] != "repository-owner":
        raise RuntimeEnvelopeError("actions/checkout retention/rollback mismatch")
    provenance = dep["provenance"]
    if provenance != {
        "observed_at": "2026-08-27T12:15:58Z",
        "commit_url": "https://github.com/actions/checkout/commit/" + CHECKOUT_COMMIT,
        "tree_url": "https://api.github.com/repos/actions/checkout/git/trees/" + CHECKOUT_TREE,
        "license_url": "https://github.com/actions/checkout/blob/" + CHECKOUT_COMMIT + "/LICENSE",
        "action_url": "https://github.com/actions/checkout/blob/" + CHECKOUT_COMMIT + "/action.yml",
        "commit_signature_verified": True,
        "tree_recursive_url": (
            "https://api.github.com/repos/actions/checkout/git/trees/"
            + CHECKOUT_TREE
            + "?recursive=1"
        ),
    }:
        raise RuntimeEnvelopeError("actions/checkout provenance mismatch")

    services = dependencies[1:]
    if _canonical_json_sha256(services) != SERVICE_DEPENDENCIES_SHA256:
        raise RuntimeEnvelopeError("exact managed-service dependency digest mismatch")
    service_keys = {
        "kind", "id", "owner", "consumers", "source", "license",
        "transitive_closure", "permissions", "network", "data_retention",
        "applicability", "drift_detector", "rollback",
    }
    for index, service in enumerate(services, start=1):
        if not isinstance(service, Mapping):
            raise RuntimeEnvelopeError("managed service %d must be an object" % index)
        _require_exact_keys(
            service, service_keys, "external-dependencies.dependencies[%d]" % index
        )
        if service["kind"] != "managed-service":
            raise RuntimeEnvelopeError("managed-service discriminator mismatch")
        if service["owner"] != {
            "operational": "GitHub, Inc.",
            "governance": "repository-owner",
        }:
            raise RuntimeEnvelopeError("managed-service owner mismatch")
        if service["license"] != {
            "spdx": "N/A",
            "disposition": "MANAGED_SERVICE_NO_EXECUTABLE_CODE_REDISTRIBUTED",
            "terms_url": (
                "https://docs.github.com/en/site-policy/github-terms/"
                "github-terms-of-service"
            ),
            "response_content": service["license"].get("response_content"),
        }:
            raise RuntimeEnvelopeError("managed-service licence disposition mismatch")
        if service["permissions"] != {
            "authentication": "NONE",
            "github_token": False,
            "secrets": False,
            "operation": "READ_ONLY_GET",
        }:
            raise RuntimeEnvelopeError("managed-service permission envelope mismatch")
        if service["rollback"].get("owner") != "repository-owner" or service[
            "rollback"
        ].get("automatic_fallback") is not False:
            raise RuntimeEnvelopeError("managed-service rollback ownership mismatch")
    quarantine_service, runner_release_service = services
    if [service["id"] for service in services] != [
        "github-rest-quarantine-read",
        "github-rest-runner-image-release-read",
    ]:
        raise RuntimeEnvelopeError("managed-service identity/order mismatch")
    if (
        quarantine_service["license"]["response_content"]
        != "PUBLIC_REPOSITORY_SUBJECT_METADATA_ONLY"
        or runner_release_service["license"]["response_content"]
        != "PUBLIC_RUNNER_IMAGE_RELEASE_METADATA_ONLY"
    ):
        raise RuntimeEnvelopeError("managed-service response licence disposition mismatch")
    if quarantine_service["consumers"] != [
        {
            "workflow": WORKFLOW_PATH,
            "job": "detect",
            "component": "scripts/skill_updates/runtime.py#verify_workflow_environment",
        },
        {
            "workflow": WORKFLOW_PATH,
            "job": "prepare",
            "component": "scripts/skill_updates/runtime.py#verify_workflow_environment",
        },
    ]:
        raise RuntimeEnvelopeError("quarantine service consumer closure mismatch")
    if runner_release_service["consumers"] != [
        {
            "workflow": WORKFLOW_PATH,
            "job": "detect",
            "component": "pre-checkout hosted-runner provenance gate",
        },
        {
            "workflow": WORKFLOW_PATH,
            "job": "prepare",
            "component": "pre-checkout hosted-runner provenance gate",
        },
        {
            "workflow": ".github/workflows/ci.yml",
            "job": "governance",
            "component": "pre-checkout hosted-runner provenance gate",
        },
    ]:
        raise RuntimeEnvelopeError("runner-image release service consumer closure mismatch")
    if quarantine_service["network"]["transport"] != "github-rest-quarantine-read":
        raise RuntimeEnvelopeError("quarantine service transport binding mismatch")
    if (
        runner_release_service["source"].get("repository_id")
        != RUNNER_IMAGES_REPOSITORY_ID
        or runner_release_service["source"].get("repository_full_name")
        != RUNNER_IMAGES_REPOSITORY
        or runner_release_service["network"]["transport"]
        != "github-rest-runner-image-release-read"
    ):
        raise RuntimeEnvelopeError("runner-image release service identity mismatch")

    runtime = ledger["runtime"]
    _require_exact_keys(
        runtime,
        {"identity_policy", "records", "installation"},
        "external-dependencies.runtime",
    )
    if _canonical_json_sha256(runtime) != RUNTIME_ENVELOPE_SHA256:
        raise RuntimeEnvelopeError("exact hosted runtime envelope digest mismatch")
    if runtime["identity_policy"] != "PROVIDER_ATTESTED_FAIL_CLOSED":
        raise RuntimeEnvelopeError("hosted runtime identity policy mismatch")
    records = runtime["records"]
    if not isinstance(records, list) or len(records) != 5:
        raise RuntimeEnvelopeError("runtime closure must contain exactly five records")
    record_keys = {
        "kind", "id", "owner", "consumers", "source", "license",
        "transitive_closure", "permissions", "network", "data_retention",
        "applicability", "drift_detector", "rollback",
    }
    for index, record in enumerate(records):
        if not isinstance(record, Mapping):
            raise RuntimeEnvelopeError("runtime record %d must be an object" % index)
        _require_exact_keys(
            record, record_keys, "external-dependencies.runtime.records[%d]" % index
        )
    if [(record["kind"], record["id"]) for record in records] != [
        ("hosted-runner-image", "github-hosted-ubuntu24-x64"),
        ("language-runtime", "cpython-3.12"),
        ("system-executable", "git"),
        ("command-shell", "gnu-bash"),
        ("action-handler-runtime", "checkout-node20"),
    ]:
        raise RuntimeEnvelopeError("runtime record identity/order mismatch")
    runner_record, python_record, git_record, bash_record, node_record = records
    if runner_record["source"]["identity_mode"] != "provider-attested-rolling":
        raise RuntimeEnvelopeError("hosted runner must remain provider-attested and rolling")
    if runner_record["source"]["required_evidence"] != {
        "image_os": RUNNER_IMAGE_OS,
        "image_version_pattern": r"^[0-9]{8}\.[0-9]+\.[0-9]+$",
        "release_tag_formula": "ubuntu24/{YYYYMMDD}.{BUILD}",
        "release_api_path_formula": (
            "/repos/actions/runner-images/releases/tags/{encoded_tag}"
        ),
        "release_target_commitish_pattern": r"^[0-9a-f]{40}$",
        "included_software_raw_url_formula": (
            "https://raw.githubusercontent.com/actions/runner-images/"
            "{SOURCE_SHA}/images/ubuntu/Ubuntu2404-Readme.md"
        ),
        "provider_manifest_asset_name": RUNNER_IMAGE_MANIFEST_ASSET,
        "provider_manifest_required_fields": ["id", "size", "digest"],
        "provider_manifest_digest_pattern": r"^sha256:[0-9a-f]{64}$",
        "sbom_disposition": "OPTIONAL_HARDENING_UNAVAILABLE_NOT_REQUIRED",
    }:
        raise RuntimeEnvelopeError("hosted runner evidence closure mismatch")
    if python_record["source"]["implementation"] != "CPython":
        raise RuntimeEnvelopeError("Python implementation binding mismatch")
    if (
        git_record["source"]["min_inclusive"] != "2.43.0"
        or git_record["source"]["max_exclusive"] != "2.60.0"
    ):
        raise RuntimeEnvelopeError("Git executable range mismatch")
    if bash_record["transitive_closure"]["invocation"] != (
        "/usr/bin/bash --noprofile --norc -e -o pipefail <script>"
    ):
        raise RuntimeEnvelopeError("Bash invocation closure mismatch")
    if node_record["source"]["runtime_family"] != CHECKOUT_NODE_RUNTIME:
        raise RuntimeEnvelopeError("checkout Node runtime selector mismatch")
    if (
        node_record["source"]["selection_binding"]
        != "actions/checkout action.yml using=node20"
        or dep["action"]["using"] != CHECKOUT_NODE_RUNTIME
    ):
        raise RuntimeEnvelopeError("checkout action/Node selector binding mismatch")
    if [
        {"workflow": item["workflow"], "job": item["job"]}
        for item in node_record["consumers"]
    ] != dep["consumers"]:
        raise RuntimeEnvelopeError("checkout Node consumer closure mismatch")
    if runtime["installation"] != {
        "silent_install": False,
        "pip": False,
        "npm": False,
        "npx": False,
        "curl_pipe_shell": False,
    }:
        raise RuntimeEnvelopeError("runtime installation envelope mismatch")

    transport = ledger["transport"]
    _require_exact_keys(
        transport,
        {
            "allowed", "git_endpoint", "upstream_host", "quarantine_read",
            "runner_image_release_read", "rest_checkout", "ssh", "file",
        },
        "external-dependencies.transport",
    )
    if _canonical_json_sha256(transport) != TRANSPORT_ENVELOPE_SHA256:
        raise RuntimeEnvelopeError("exact transport envelope digest mismatch")
    if transport["allowed"] != [
        "git-https",
        "github-rest-quarantine-read",
        "github-rest-runner-image-release-read",
    ]:
        raise RuntimeEnvelopeError("allowed transport closure mismatch")
    if (
        transport["git_endpoint"] != REPOSITORY_HTTPS_URL
        or transport["upstream_host"] != "github.com"
    ):
        raise RuntimeEnvelopeError("Git transport identity mismatch")
    if transport["quarantine_read"] != {
        "endpoint": API_ENDPOINT,
        "method": "GET",
        "paths": list(QUARANTINE_API_PATHS),
        "authentication": "NONE",
        "tls_verification": "REQUIRED",
        "redirects": "REJECT",
        "response_identity": "EXACT_SUBJECT_NUMBER_NODE_ID_STATE_AUTHOR_HEAD_AND_BASE",
    }:
        raise RuntimeEnvelopeError("quarantine transport envelope mismatch")
    if transport["runner_image_release_read"] != {
        "endpoint": API_ENDPOINT,
        "method": "GET",
        "repository_identity_path": "/repos/actions/runner-images",
        "path_template": (
            "/repos/actions/runner-images/releases/tags/ubuntu24%2F{YYYYMMDD}.{BUILD}"
        ),
        "authentication": "NONE",
        "tls_verification": "REQUIRED",
        "redirects": "REJECT",
        "response_constraints": {
            "repository_id": RUNNER_IMAGES_REPOSITORY_ID,
            "repository_full_name": RUNNER_IMAGES_REPOSITORY,
            "tag_name_formula": "ubuntu24/{YYYYMMDD}.{BUILD}",
            "target_commitish_pattern": r"^[0-9a-f]{40}$",
            "asset_name": RUNNER_IMAGE_MANIFEST_ASSET,
            "asset_required_fields": ["id", "size", "digest"],
            "asset_digest_pattern": r"^sha256:[0-9a-f]{64}$",
            "included_software_raw_url_formula": (
                "https://raw.githubusercontent.com/actions/runner-images/"
                "{SOURCE_SHA}/images/ubuntu/Ubuntu2404-Readme.md"
            ),
        },
    }:
        raise RuntimeEnvelopeError("runner-image release-read envelope mismatch")
    if (
        transport["rest_checkout"] != "FORBIDDEN"
        or transport["ssh"] != "FORBIDDEN"
        or transport["file"] != "FORBIDDEN_IN_PRODUCTION"
    ):
        raise RuntimeEnvelopeError("forbidden transport disposition mismatch")


def _validate_schema_files(root: Path) -> None:
    expected = {
        "policy.schema.json": ("urn:bukerov:skills-maintenance:policy:1", "policy_id"),
        "control-plane.schema.json": (
            "urn:bukerov:skills-maintenance:control-plane:1",
            "control_plane_id",
        ),
        "legacy-quarantine.schema.json": (
            "urn:bukerov:skills-maintenance:legacy-quarantine:1",
            "quarantine_id",
        ),
        "external-dependencies.schema.json": (
            "urn:bukerov:skills-maintenance:external-dependencies:1",
            "ledger_id",
        ),
    }
    for name, (schema_id, identity_key) in expected.items():
        schema = _read_json(root, SCHEMAS_REL / name)
        if schema.get("$schema") != "https://json-schema.org/draft/2020-12/schema":
            raise RuntimeEnvelopeError("%s must use JSON Schema draft 2020-12" % name)
        if schema.get("$id") != schema_id or schema.get("type") != "object":
            raise RuntimeEnvelopeError("%s identity mismatch" % name)
        if schema.get("additionalProperties") is not False:
            raise RuntimeEnvelopeError("%s must be closed" % name)
        required = schema.get("required")
        properties = schema.get("properties")
        if not isinstance(required, list) or not isinstance(properties, dict):
            raise RuntimeEnvelopeError("%s lacks required/properties" % name)
        if set(required) != set(properties):
            raise RuntimeEnvelopeError("%s must require every top-level property" % name)
        if identity_key not in properties:
            raise RuntimeEnvelopeError("%s lacks its identity discriminator" % name)


def _updater_source_sha(root: Path) -> str:
    """Aggregate every production Python updater owner by path and raw byte digest."""

    package = root / "scripts/skill_updates"
    paths = [
        root / "scripts/check-skill-updates.py",
        root / "scripts/prepare-skill-update.py",
    ]
    try:
        paths.extend(sorted(path for path in package.glob("*.py") if path.is_file()))
    except OSError as exc:
        raise RuntimeEnvelopeError("cannot enumerate updater sources: %s" % exc) from exc
    if not paths or any(not path.is_file() for path in paths):
        raise RuntimeEnvelopeError("production updater source closure is incomplete")
    manifest = {
        path.relative_to(root).as_posix(): _raw_sha256(path) for path in paths
    }
    return _canonical_json_sha256(manifest)


def _pinned_action_digests() -> dict[str, str]:
    return {CHECKOUT_PIN_KEY: CHECKOUT_TREE_MANIFEST_SHA256}


def compute_control_input_digest(
    repo_root: os.PathLike[str] | str,
    members: Sequence[str] | None = None,
) -> str:
    """Hash the exact policy-declared, 100644, non-symlink control closure.

    ``members`` is an explicit test seam.  Production omits it and the committed policy
    must declare the exact frozen-R0 membership tuple.
    """

    root = _root(repo_root)
    if members is None:
        policy = _read_json(root, POLICY_REL)
        _validate_policy(policy)
        declared = policy["control_input"]["members"]
    else:
        declared = list(members)
    if declared != list(CONTROL_INPUT_MEMBERS):
        raise RuntimeEnvelopeError("control-input membership is not the exact frozen closure")
    if declared != sorted(declared, key=lambda item: item.encode("utf-8")):
        raise RuntimeEnvelopeError("control-input paths are not UTF-8 byte sorted")
    records: list[dict[str, str]] = []
    for rel in declared:
        if not isinstance(rel, str) or not rel or rel.startswith("/") or ".." in rel.split("/"):
            raise RuntimeEnvelopeError("unsafe control-input path %r" % rel)
        path = root / rel
        try:
            info = path.lstat()
        except OSError as exc:
            raise RuntimeEnvelopeError("missing control-input member %s: %s" % (rel, exc)) from exc
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
            raise RuntimeEnvelopeError("control-input member %s must be a regular non-symlink" % rel)
        if stat.S_IMODE(info.st_mode) != 0o644:
            raise RuntimeEnvelopeError("control-input member %s mode is not 100644" % rel)
        try:
            resolved = path.resolve(strict=True)
        except OSError as exc:
            raise RuntimeEnvelopeError("cannot resolve control-input member %s" % rel) from exc
        if resolved != path.absolute():
            raise RuntimeEnvelopeError("control-input member %s traverses a symlink" % rel)
        records.append({"path": rel, "mode": "100644", "sha256": _raw_sha256(path)})
    payload = {"schema": "control-input/v1", "members": records}
    return _canonical_json_sha256(payload)


def load_contract(repo_root: os.PathLike[str] | str) -> RuntimeContract:
    """Load and cross-validate the complete committed G1.1 contract set."""

    root = _root(repo_root)
    policy = _read_json(root, POLICY_REL)
    control = _read_json(root, CONTROL_REL)
    quarantine = _read_json(root, QUARANTINE_REL)
    dependencies = _read_json(root, DEPENDENCIES_REL)
    _validate_policy(policy)
    _validate_control(control)
    policy_sha256 = _raw_sha256(root / POLICY_REL)
    control_sha256 = _raw_sha256(root / CONTROL_REL)
    quarantine_sha256 = _raw_sha256(root / QUARANTINE_REL)
    external_dependencies_sha256 = _raw_sha256(root / DEPENDENCIES_REL)
    _validate_quarantine_document(
        quarantine,
        policy_sha256=policy_sha256,
        control_sha256=control_sha256,
    )
    _validate_dependencies_document(dependencies)
    _validate_schema_files(root)
    control_input_digest = compute_control_input_digest(root)
    updater_source_sha = _updater_source_sha(root)
    return RuntimeContract(
        policy=_freeze(policy),
        control_plane=_freeze(control),
        quarantine=_freeze(quarantine),
        external_dependencies=_freeze(dependencies),
        policy_sha256=policy_sha256,
        control_plane_sha256=control_sha256,
        quarantine_sha256=quarantine_sha256,
        external_dependencies_sha256=external_dependencies_sha256,
        control_input_digest=control_input_digest,
        updater_source_sha=updater_source_sha,
        pinned_action_digests=_freeze(_pinned_action_digests()),
    )


def candidate_identity(
    *,
    provider: str,
    stable_branch: str,
    stable_base_sha: str,
    target_sha: str,
    upstream_repo: str,
    old_pin: str,
    control_input_digest: str,
    updater_source_sha: str,
    pinned_action_digests: Mapping[str, str],
) -> CandidateIdentity:
    """Return the JCS identity of one complete stable update proposal."""

    if not isinstance(provider, str) or PROVIDER_RE.fullmatch(provider) is None:
        raise RuntimeEnvelopeError("provider is not a canonical provider key")
    if not isinstance(stable_branch, str) or STABLE_BRANCH_RE.fullmatch(stable_branch) is None:
        raise RuntimeEnvelopeError("stable branch is not a canonical release/X.Y branch")
    _require_sha1(stable_base_sha, "stable_base_sha")
    _require_sha1(target_sha, "target_sha")
    if not isinstance(upstream_repo, str) or re.fullmatch(
        r"https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", upstream_repo
    ) is None:
        raise RuntimeEnvelopeError("upstream_repo is not a canonical GitHub HTTPS URL")
    _require_sha1(old_pin, "old_pin")
    _require_sha256(control_input_digest, "control_input_digest")
    _require_sha256(updater_source_sha, "updater_source_sha")
    expected_pins = _pinned_action_digests()
    if not isinstance(pinned_action_digests, Mapping) or dict(pinned_action_digests) != expected_pins:
        raise RuntimeEnvelopeError("pinned Action digest closure mismatch")
    if set(pinned_action_digests) != {CHECKOUT_PIN_KEY}:
        raise RuntimeEnvelopeError("pinned Action digest must have one canonical key")
    _require_sha256(pinned_action_digests[CHECKOUT_PIN_KEY], "pinned_action_digests")
    payload: dict[str, Any] = {
        "schema": CANDIDATE_SCHEMA,
        "repo_id": REPOSITORY_ID,
        "repo_full_name": REPOSITORY_FULL_NAME,
        "stable_branch": stable_branch,
        "stable_base_sha": stable_base_sha,
        "provider": provider,
        "upstream_repo": upstream_repo,
        "old_pin": old_pin,
        "target_sha": target_sha,
        "control_input_digest": control_input_digest,
        "updater_source_sha": updater_source_sha,
        "pinned_action_digests": dict(pinned_action_digests),
    }
    proposal_id = _canonical_json_sha256(payload)
    release_locator = stable_branch.replace("/", "-")
    locator = "stable-skills/%s/%s/%s-%s-%s" % (
        provider,
        release_locator,
        stable_base_sha[:12],
        target_sha[:12],
        proposal_id[:12],
    )
    return CandidateIdentity(
        schema=CANDIDATE_SCHEMA,
        repo_id=REPOSITORY_ID,
        repo_full_name=REPOSITORY_FULL_NAME,
        stable_branch=stable_branch,
        stable_base_sha=stable_base_sha,
        provider=provider,
        upstream_repo=upstream_repo,
        old_pin=old_pin,
        target_sha=target_sha,
        control_input_digest=control_input_digest,
        updater_source_sha=updater_source_sha,
        pinned_action_digests=_freeze(dict(pinned_action_digests)),
        proposal_id=proposal_id,
        locator=locator,
    )


def verify_control_identity(
    repo_root: os.PathLike[str] | str,
    *,
    selected_ref: str,
    workflow_ref: str,
    live_default_branch: str,
    fetched_default_sha: str,
    selected_sha: str,
    require_default: bool,
) -> RuntimeContract:
    """Verify committed, selected, workflow, and live-default control identity."""

    contract = load_contract(repo_root)
    _require_sha1(fetched_default_sha, "fetched_default_sha")
    _require_sha1(selected_sha, "selected_sha")
    if selected_ref != contract.stable_branch:
        raise RuntimeEnvelopeError("selected ref is not the committed stable branch")
    if workflow_ref != contract.expected_workflow_ref:
        raise RuntimeEnvelopeError("GITHUB_WORKFLOW_REF does not identify the stable workflow")
    if require_default:
        if live_default_branch != contract.stable_branch:
            raise RuntimeEnvelopeError("stable branch is not the live default branch")
        if fetched_default_sha != selected_sha:
            raise RuntimeEnvelopeError("live default head and selected SHA differ")
    elif live_default_branch == OBSERVED_DEFAULT_BRANCH:
        if fetched_default_sha != OBSERVED_DEFAULT_SHA:
            raise RuntimeEnvelopeError("observed main default head changed")
    elif live_default_branch == contract.stable_branch:
        if fetched_default_sha != selected_sha:
            raise RuntimeEnvelopeError("migrated stable default head and selected SHA differ")
    else:
        raise RuntimeEnvelopeError("live default branch relation is outside the contract")
    return contract


def _verify_precheckout_git_environment(env: Mapping[str, str]) -> None:
    """Reject every ambient ``GIT_*`` value outside the four workflow controls."""

    changed = sorted(
        key
        for key, expected in _CONTROLLED_PRECHECKOUT_GIT_ENV.items()
        if env.get(key) != expected
    )
    unexpected = sorted(
        key
        for key in env
        if key.startswith("GIT_") and key not in _CONTROLLED_PRECHECKOUT_GIT_ENV
    )
    if changed or unexpected:
        raise RuntimeEnvelopeError(
            "pre-checkout Git environment mismatch: changed=%s unexpected=%s"
            % (changed, unexpected)
        )


def controlled_git_environment(source: Mapping[str, str] | None = None) -> dict[str, str]:
    """Build the exact credential-free Git environment allowed in production."""

    source = os.environ if source is None else source
    path = source.get("PATH", "")
    if not isinstance(path, str) or not path or "\x00" in path or "\n" in path:
        raise RuntimeEnvelopeError("PATH is missing or malformed")
    return {
        "PATH": path,
        "GIT_CONFIG_GLOBAL": os.devnull,
        "GIT_CONFIG_SYSTEM": os.devnull,
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_CONFIG_COUNT": "2",
        "GIT_CONFIG_KEY_0": "core.hooksPath",
        "GIT_CONFIG_VALUE_0": os.devnull,
        "GIT_CONFIG_KEY_1": "credential.helper",
        "GIT_CONFIG_VALUE_1": "",
        "GIT_TERMINAL_PROMPT": "0",
        "GIT_ALLOW_PROTOCOL": "https",
        "GIT_PROTOCOL_FROM_USER": "0",
        "GIT_LFS_SKIP_SMUDGE": "1",
        "GCM_INTERACTIVE": "Never",
        "LC_ALL": "C",
        "TZ": "UTC",
    }


_CONTROLLED_GIT_ARGV_PREFIX = (
    "git",
    "-c",
    "core.hooksPath=/dev/null",
    "-c",
    "credential.helper=",
    "-c",
    "protocol.allow=never",
    "-c",
    "protocol.https.allow=always",
    "-c",
    "protocol.file.allow=never",
    "-c",
    "protocol.ext.allow=never",
    "-c",
    "http.sslVerify=true",
    "-c",
    "http.followRedirects=false",
)


def _controlled_git_output(
    repo_root: Path, arguments: Sequence[str], env: Mapping[str, str]
) -> str:
    """Run one closed, non-network Git query in the checked-out repository."""

    argv = [*_CONTROLLED_GIT_ARGV_PREFIX, *arguments]
    try:
        proc = subprocess.run(
            argv,
            cwd=str(repo_root),
            env=dict(env),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=10,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise RuntimeEnvelopeError("checked-out Git identity probe failed: %s" % exc) from exc
    if proc.returncode != 0:
        raise RuntimeEnvelopeError("checked-out Git identity probe failed closed")
    return proc.stdout.strip()


def verify_checkout_worktree_identity(
    repo_root: os.PathLike[str] | str,
    environ: Mapping[str, str] | None = None,
) -> None:
    """Prove checkout produced the expected real worktree and exact event commit."""

    root = _root(repo_root)
    env = os.environ if environ is None else environ
    expected_sha = env.get("GITHUB_SHA", "")
    _require_sha1(expected_sha, "GITHUB_SHA")
    controlled = controlled_git_environment({"PATH": env.get("PATH", "")})
    if _controlled_git_output(root, ("rev-parse", "--is-inside-work-tree"), controlled) != "true":
        raise RuntimeEnvelopeError("checkout path is not a Git worktree")
    top_level = _controlled_git_output(root, ("rev-parse", "--show-toplevel"), controlled)
    try:
        resolved_top = Path(top_level).resolve(strict=True)
    except (OSError, ValueError) as exc:
        raise RuntimeEnvelopeError("checkout worktree root is not real") from exc
    if resolved_top != root.resolve(strict=True):
        raise RuntimeEnvelopeError("checkout worktree root does not match repository root")
    head_sha = _controlled_git_output(
        root, ("rev-parse", "--verify", "HEAD^{commit}"), controlled
    )
    _require_sha1(head_sha, "checked-out HEAD")
    if head_sha != expected_sha:
        raise RuntimeEnvelopeError("checked-out HEAD does not equal GITHUB_SHA")


def _version_tuple(value: Sequence[int], where: str) -> tuple[int, int, int]:
    try:
        valid = (
            not isinstance(value, (str, bytes))
            and len(value) == 3
            and all(
                isinstance(part, int) and not isinstance(part, bool) and part >= 0
                for part in value
            )
        )
    except TypeError:
        valid = False
    if not valid:
        raise RuntimeEnvelopeError("%s must be a three-integer version" % where)
    return int(value[0]), int(value[1]), int(value[2])


def runner_image_release_identity(image_version: str, source_commit: str) -> dict[str, str]:
    """Derive the only accepted official evidence locators for one hosted image.

    ``ubuntu-24.04`` is a rolling provider selector, not an immutable image
    digest.  GitHub's exact ``ImageVersion`` attestation is therefore the input
    to this derivation.  No caller may substitute a branch, ``latest`` alias,
    redirect, or an independently constructed asset host.
    """

    if not isinstance(image_version, str):
        raise RuntimeEnvelopeError("ImageVersion must be text")
    match = RUNNER_IMAGE_VERSION_RE.fullmatch(image_version)
    if match is None:
        raise RuntimeEnvelopeError("ImageVersion is outside the ubuntu24 release envelope")
    try:
        datetime.datetime.strptime(match.group("date"), "%Y%m%d")
    except ValueError as exc:
        raise RuntimeEnvelopeError("ImageVersion contains an invalid calendar date") from exc
    _require_sha1(source_commit, "runner image release target_commitish")
    release_id = "%s.%s" % (match.group("date"), match.group("build"))
    tag = "ubuntu24/" + release_id
    return {
        "image_version": image_version,
        "release_tag": tag,
        "release_api_url": RUNNER_IMAGE_RELEASE_API_PREFIX + release_id,
        "release_url": (
            "https://github.com/actions/runner-images/releases/tag/" + tag
        ),
        "source_commit": source_commit,
        "included_software_url": (
            "https://raw.githubusercontent.com/actions/runner-images/"
            + source_commit
            + "/"
            + RUNNER_IMAGE_INCLUDED_SOFTWARE_PATH
        ),
        "manifest_asset_name": RUNNER_IMAGE_MANIFEST_ASSET,
    }


def validate_runner_images_repository_payload(payload: Mapping[str, Any]) -> None:
    """Bind the mutable owner/name locator to GitHub's immutable repository id."""

    if not isinstance(payload, Mapping):
        raise RuntimeEnvelopeError("runner-images repository response must be an object")
    owner = payload.get("owner")
    if (
        payload.get("id") != RUNNER_IMAGES_REPOSITORY_ID
        or isinstance(payload.get("id"), bool)
        or payload.get("full_name") != RUNNER_IMAGES_REPOSITORY
        or payload.get("url")
        != "https://api.github.com/repos/actions/runner-images"
        or payload.get("html_url") != "https://github.com/actions/runner-images"
        or not isinstance(owner, Mapping)
        or owner.get("login") != "actions"
    ):
        raise RuntimeEnvelopeError("runner-images numeric repository identity mismatch")


def validate_runner_image_release_payload(
    image_version: str, payload: Mapping[str, Any]
) -> dict[str, Any]:
    """Validate the bounded GitHub release response used by the bootstrap gate.

    The provider software manifest is a provenance/transitive-software record;
    it is not mislabelled as an SBOM.  SBOM download and retention are optional
    hardening outside the G1.1 acceptance requirement.
    """

    if not isinstance(payload, Mapping):
        raise RuntimeEnvelopeError("runner image release response must be an object")
    target = payload.get("target_commitish")
    if not isinstance(target, str):
        raise RuntimeEnvelopeError("runner image release target_commitish is missing")
    identity = runner_image_release_identity(image_version, target)
    if payload.get("tag_name") != identity["release_tag"]:
        raise RuntimeEnvelopeError("runner image release tag mismatch")
    if payload.get("draft") is not False:
        raise RuntimeEnvelopeError("runner image release must not be a draft")
    html_url = payload.get("html_url")
    if not isinstance(html_url, str):
        raise RuntimeEnvelopeError("runner image release canonical URL is missing")
    parsed_html = urlparse.urlsplit(html_url)
    html_prefix = "/actions/runner-images/releases/tag/"
    if (
        parsed_html.scheme != "https"
        or parsed_html.netloc != "github.com"
        or parsed_html.query
        or parsed_html.fragment
        or not parsed_html.path.startswith(html_prefix)
        or urlparse.unquote(parsed_html.path[len(html_prefix) :])
        != identity["release_tag"]
    ):
        raise RuntimeEnvelopeError("runner image release canonical URL mismatch")
    assets = payload.get("assets")
    if not isinstance(assets, list):
        raise RuntimeEnvelopeError("runner image release assets are missing")
    manifests = [
        asset
        for asset in assets
        if isinstance(asset, Mapping) and asset.get("name") == RUNNER_IMAGE_MANIFEST_ASSET
    ]
    if len(manifests) != 1:
        raise RuntimeEnvelopeError("runner image release must contain one provider manifest")
    asset = manifests[0]
    asset_id = asset.get("id")
    size = asset.get("size")
    digest = asset.get("digest")
    download_url = asset.get("browser_download_url")
    if not isinstance(asset_id, int) or isinstance(asset_id, bool) or asset_id <= 0:
        raise RuntimeEnvelopeError("runner image manifest asset id is invalid")
    if not isinstance(size, int) or isinstance(size, bool) or size <= 0:
        raise RuntimeEnvelopeError("runner image manifest asset size is invalid")
    if (
        not isinstance(digest, str)
        or re.fullmatch(r"sha256:[0-9a-f]{64}", digest) is None
    ):
        raise RuntimeEnvelopeError("runner image manifest asset digest is invalid")
    if not isinstance(download_url, str):
        raise RuntimeEnvelopeError("runner image manifest asset URL is invalid")
    parsed_download = urlparse.urlsplit(download_url)
    download_prefix = "/actions/runner-images/releases/download/"
    if (
        parsed_download.scheme != "https"
        or parsed_download.netloc != "github.com"
        or parsed_download.query
        or parsed_download.fragment
        or not parsed_download.path.startswith(download_prefix)
    ):
        raise RuntimeEnvelopeError("runner image manifest asset URL is invalid")
    download_suffix = parsed_download.path[len(download_prefix) :]
    if "/" not in download_suffix:
        raise RuntimeEnvelopeError("runner image manifest asset URL is invalid")
    download_tag, download_name = download_suffix.rsplit("/", 1)
    if (
        urlparse.unquote(download_tag) != identity["release_tag"]
        or urlparse.unquote(download_name) != RUNNER_IMAGE_MANIFEST_ASSET
    ):
        raise RuntimeEnvelopeError("runner image manifest asset URL is invalid")
    result: dict[str, Any] = dict(identity)
    result.update(
        {
            "manifest_asset_id": asset_id,
            "manifest_asset_size": size,
            "manifest_asset_digest": digest,
            "manifest_asset_url": download_url,
            "sbom_availability": "UNAVAILABLE_OPTIONAL_HARDENING",
        }
    )
    return result


def verify_runtime_envelope(
    repo_root: os.PathLike[str] | str,
    *,
    runner: str,
    image_os: str,
    image_version: str,
    runner_arch: str,
    python_implementation: str,
    python_version: Sequence[int],
    bash_version: Sequence[int],
    git_version: Sequence[int],
    node_runtime: str,
    env: Mapping[str, str],
) -> RuntimeContract:
    """Verify the closed runner/executable facts against the committed ledger."""

    contract = load_contract(repo_root)
    if runner != RUNNER_LABEL:
        raise RuntimeEnvelopeError("runner label mismatch")
    if image_os != RUNNER_IMAGE_OS:
        raise RuntimeEnvelopeError("ImageOS must be ubuntu24")
    if not isinstance(image_version, str) or RUNNER_IMAGE_VERSION_RE.fullmatch(image_version) is None:
        raise RuntimeEnvelopeError("ImageVersion is outside the recorded release envelope")
    try:
        datetime.datetime.strptime(image_version.split(".", 1)[0], "%Y%m%d")
    except ValueError as exc:
        raise RuntimeEnvelopeError("ImageVersion contains an invalid calendar date") from exc
    if runner_arch != RUNNER_ARCH:
        raise RuntimeEnvelopeError("runner architecture must be X64")
    if python_implementation != "cpython":
        raise RuntimeEnvelopeError("Python implementation must be CPython")
    py = _version_tuple(python_version, "python_version")
    if py[:2] != PYTHON_MAJOR_MINOR:
        raise RuntimeEnvelopeError("Python must be CPython 3.12.x")
    bash = _version_tuple(bash_version, "bash_version")
    if bash[:2] != BASH_MAJOR_MINOR:
        raise RuntimeEnvelopeError("Bash must be GNU Bash 5.2.x")
    git = _version_tuple(git_version, "git_version")
    if git < GIT_MIN_INCLUSIVE or git >= GIT_MAX_EXCLUSIVE:
        raise RuntimeEnvelopeError("Git version is outside [2.43.0, 2.60.0)")
    if node_runtime != CHECKOUT_NODE_RUNTIME:
        raise RuntimeEnvelopeError("checkout Action runtime selector must be node20")
    expected = controlled_git_environment({"PATH": env.get("PATH", "")})
    if dict(env) != expected:
        missing = sorted(set(expected) - set(env))
        unexpected = sorted(set(env) - set(expected))
        changed = sorted(
            key for key in set(expected) & set(env) if expected[key] != env[key]
        )
        raise RuntimeEnvelopeError(
            "Git environment mismatch: missing=%s unexpected=%s changed=%s"
            % (missing, unexpected, changed)
        )
    return contract


def _parse_git_version(text: str) -> tuple[int, int, int]:
    match = re.fullmatch(r"git version ([0-9]+)\.([0-9]+)\.([0-9]+)(?:\.[^ ]+)?\s*", text)
    if match is None:
        raise RuntimeEnvelopeError("unrecognized git --version output")
    return tuple(int(part) for part in match.groups())  # type: ignore[return-value]


def _actual_git_version(env: Mapping[str, str]) -> tuple[int, int, int]:
    try:
        proc = subprocess.run(
            ["git", "--version"],
            env=dict(env),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=10,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise RuntimeEnvelopeError("could not execute git --version: %s" % exc) from exc
    if proc.returncode != 0:
        raise RuntimeEnvelopeError("git --version failed")
    return _parse_git_version(proc.stdout)


def _parse_bash_version(text: str) -> tuple[int, int, int]:
    match = re.search(
        r"(?m)^GNU bash, version ([0-9]+)\.([0-9]+)\.([0-9]+)(?:\([^\n]+)?",
        text,
    )
    if match is None:
        raise RuntimeEnvelopeError("unrecognized bash --version output")
    return tuple(int(part) for part in match.groups())  # type: ignore[return-value]


def _actual_bash_version(source: Mapping[str, str] | None = None) -> tuple[int, int, int]:
    source = os.environ if source is None else source
    path = source.get("PATH", "")
    if not isinstance(path, str) or not path:
        raise RuntimeEnvelopeError("PATH is missing for Bash runtime proof")
    env = {
        "PATH": path,
        "BASH_ENV": os.devnull,
        "ENV": os.devnull,
        "LC_ALL": "C",
    }
    try:
        proc = subprocess.run(
            ["/usr/bin/bash", "--noprofile", "--norc", "--version"],
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=10,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise RuntimeEnvelopeError("could not execute bash --version: %s" % exc) from exc
    if proc.returncode != 0:
        raise RuntimeEnvelopeError("bash --version failed")
    return _parse_bash_version(proc.stdout)


def verify_hosted_runtime_environment(
    repo_root: os.PathLike[str] | str,
    environ: Mapping[str, str] | None = None,
) -> RuntimeContract:
    """Verify live GitHub-hosted image and executable facts after checkout.

    The inline bootstrap gate performs the same observation before checkout.
    This repository-owned check cannot run before its bytes exist, so it is a
    second independent refusal point before updater/provider access.
    """

    env = os.environ if environ is None else environ
    if env.get("GITHUB_ACTIONS") != "true":
        raise RuntimeEnvelopeError("live hosted runtime proof requires GitHub Actions")
    _verify_precheckout_git_environment(env)
    present_execution = sorted(
        key for key in _EXECUTION_OVERRIDE_ENV_KEYS if env.get(key)
    )
    if present_execution:
        raise RuntimeEnvelopeError(
            "ambient Node/Git execution override variables are forbidden: %s"
            % present_execution
        )
    if env.get("BASH_ENV") != os.devnull:
        raise RuntimeEnvelopeError("BASH_ENV must be /dev/null")
    controlled = controlled_git_environment({"PATH": env.get("PATH", "")})
    return verify_runtime_envelope(
        repo_root,
        runner=RUNNER_LABEL,
        image_os=env.get("ImageOS", ""),
        image_version=env.get("ImageVersion", ""),
        runner_arch=env.get("RUNNER_ARCH", ""),
        python_implementation=sys.implementation.name,
        python_version=sys.version_info[:3],
        bash_version=_actual_bash_version(env),
        git_version=_actual_git_version(controlled),
        node_runtime=CHECKOUT_NODE_RUNTIME,
        env=controlled,
    )


def _parse_ls_remote(text: str) -> tuple[str, str, str]:
    """Return ``(default_branch, head_sha, selected_branch_sha)``."""

    default_refs: list[str] = []
    refs: dict[str, list[str]] = {}
    for raw in text.splitlines():
        line = raw.strip()
        if not line:
            continue
        if line.startswith("ref:"):
            parts = line.split()
            if len(parts) != 3 or parts[2] != "HEAD" or not parts[1].startswith("refs/heads/"):
                raise RuntimeEnvelopeError("malformed ls-remote symref line")
            default_refs.append(parts[1][len("refs/heads/") :])
            continue
        fields = line.split("\t")
        if len(fields) != 2:
            raise RuntimeEnvelopeError("malformed ls-remote object line")
        sha, ref = fields
        _require_sha1(sha, "ls-remote object")
        refs.setdefault(ref, []).append(sha)
    selected_name = "refs/heads/" + STABLE_BRANCH
    if len(default_refs) != 1 or len(refs.get("HEAD", [])) != 1 or len(refs.get(selected_name, [])) != 1:
        raise RuntimeEnvelopeError("ls-remote control identity is missing or ambiguous")
    extra = set(refs) - {"HEAD", selected_name}
    if extra:
        raise RuntimeEnvelopeError("ls-remote returned unexpected refs: %s" % sorted(extra))
    return default_refs[0], refs["HEAD"][0], refs[selected_name][0]


def _live_ls_remote(repo_url: str, stable_branch: str) -> str:
    if repo_url != REPOSITORY_HTTPS_URL or stable_branch != STABLE_BRANCH:
        raise RuntimeEnvelopeError("live control lookup is not the fixed committed origin")
    env = controlled_git_environment()
    argv = [
        *_CONTROLLED_GIT_ARGV_PREFIX,
        "ls-remote",
        "--symref",
        "--exit-code",
        "--",
        repo_url,
        "HEAD",
        "refs/heads/" + stable_branch,
    ]
    try:
        proc = subprocess.run(
            argv,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=30,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise RuntimeEnvelopeError("live git identity lookup failed: %s" % exc) from exc
    if proc.returncode != 0:
        detail = proc.stderr.strip().splitlines()
        safe_detail = detail[-1][:200] if detail else "no diagnostic"
        raise RuntimeEnvelopeError("live git identity lookup failed: %s" % safe_detail)
    return proc.stdout


class _RejectRedirects(urlrequest.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):  # noqa: D401
        """Reject every redirect instead of silently changing the trusted endpoint."""

        return None


def _live_quarantine_get(path: str) -> Mapping[str, Any]:
    """Perform one bounded, unauthenticated GET on the committed quarantine allowlist."""

    if path not in QUARANTINE_API_PATHS:
        raise RuntimeEnvelopeError("quarantine API path is outside the closed allowlist")
    url = API_ENDPOINT + path
    request = urlrequest.Request(
        url,
        method="GET",
        headers={
            "Accept": "application/vnd.github+json",
            "User-Agent": "bukerov-stable-skills-g11",
            "X-GitHub-Api-Version": "2022-11-28",
        },
    )
    context = ssl.create_default_context()
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    opener = urlrequest.build_opener(
        urlrequest.ProxyHandler({}),
        urlrequest.HTTPSHandler(context=context),
        _RejectRedirects(),
    )
    try:
        with opener.open(request, timeout=15) as response:
            if response.status != 200 or response.geturl() != url:
                raise RuntimeEnvelopeError("quarantine GET returned unexpected status or URL")
            body = response.read(1_048_577)
    except (OSError, urlerror.URLError, urlerror.HTTPError) as exc:
        raise RuntimeEnvelopeError("quarantine GET failed closed for %s: %s" % (path, exc)) from exc
    if len(body) > 1_048_576:
        raise RuntimeEnvelopeError("quarantine GET response exceeds the fixed size bound")
    try:
        decoded = body.decode("utf-8")
    except UnicodeError as exc:
        raise RuntimeEnvelopeError("quarantine GET returned invalid JSON") from exc
    value = _parse_unique_json(decoded, "quarantine GET response")
    if not isinstance(value, dict):
        raise RuntimeEnvelopeError("quarantine GET response must be an object")
    return value


def _api_identity_matches(expected: Mapping[str, Any], live: Mapping[str, Any]) -> None:
    """Compare the exact immutable/native identity fields of one GitHub subject."""

    common = {
        "number": live.get("number"),
        "node_id": live.get("node_id"),
        "title": live.get("title"),
        "state": str(live.get("state", "")).upper(),
        "author": (live.get("user") or {}).get("login")
        if isinstance(live.get("user"), Mapping)
        else None,
    }
    for key, actual in common.items():
        if actual != expected[key]:
            raise RuntimeEnvelopeError(
                "live quarantine #%s %s mismatch" % (expected["number"], key)
            )
    # Provider/target are not first-class GitHub fields.  The legacy bot wrote
    # them as Markdown table rows, so bind that one exact structured projection.
    # Searching the whole JSON is unsafe: an old expected target in an unrelated
    # nested field could coexist with a different current target and still pass.
    body = live.get("body")
    if not isinstance(body, str):
        raise RuntimeEnvelopeError(
            "live quarantine #%s body is missing" % expected["number"]
        )
    lines = body.splitlines()
    provider_rows = [
        line for line in lines if re.match(r"^\|\s*provider\s*\|", line)
    ]
    expected_provider_row = "| provider | `%s` |" % expected["provider"]
    if provider_rows != [expected_provider_row]:
        raise RuntimeEnvelopeError(
            "live quarantine #%s provider projection mismatch or ambiguity"
            % expected["number"]
        )
    target_rows = [
        line
        for line in lines
        if re.match(r"^\|\s*(?:target commit|seen at commit)\s*\|", line)
    ]
    target_label = "seen at commit" if expected["number"] == 240 else "target commit"
    expected_target_row = "| %s | `%s` |" % (target_label, expected["target_sha"])
    if target_rows != [expected_target_row]:
        raise RuntimeEnvelopeError(
            "live quarantine #%s target projection mismatch or ambiguity"
            % expected["number"]
        )
    if expected["kind"] == "pull_request":
        if "pull_request" in live and not isinstance(live.get("head"), Mapping):
            raise RuntimeEnvelopeError("pull quarantine response is malformed")
        head = live.get("head")
        base = live.get("base")
        if not isinstance(head, Mapping) or not isinstance(base, Mapping):
            raise RuntimeEnvelopeError("pull quarantine response lacks head/base identity")
        actual = {
            "draft": live.get("draft"),
            "head_ref": head.get("ref"),
            "head_sha": head.get("sha"),
            "base_ref": base.get("ref"),
            "base_sha": base.get("sha"),
        }
        for key, value in actual.items():
            if value != expected[key]:
                raise RuntimeEnvelopeError(
                    "live quarantine #%s %s mismatch" % (expected["number"], key)
                )
    else:
        if "pull_request" in live:
            raise RuntimeEnvelopeError("issue quarantine endpoint resolved to a pull request")
        labels = live.get("labels")
        if not isinstance(labels, list):
            raise RuntimeEnvelopeError("issue quarantine labels are malformed")
        names = [
            item.get("name") for item in labels if isinstance(item, Mapping)
        ]
        if names != [expected["label"]]:
            raise RuntimeEnvelopeError(
                "live quarantine #%s label mismatch" % expected["number"]
            )


def _verify_live_quarantine(
    contract: RuntimeContract,
    getter: Callable[[str], Mapping[str, Any]],
) -> None:
    expected_subjects = _thaw(contract.quarantine["subjects"])
    by_number = {subject["number"]: subject for subject in expected_subjects}
    seen: list[int] = []
    for path in QUARANTINE_API_PATHS:
        try:
            number = int(path.rsplit("/", 1)[1])
            live = getter(path)
        except RuntimeEnvelopeError:
            raise
        except Exception as exc:
            raise RuntimeEnvelopeError(
                "quarantine resolver failed closed for %s: %s" % (path, exc)
            ) from exc
        if not isinstance(live, Mapping):
            raise RuntimeEnvelopeError("quarantine resolver returned a non-object")
        _api_identity_matches(by_number[number], live)
        seen.append(number)
    if seen != [223, 233, 241, 230, 238, 239, 240]:
        raise RuntimeEnvelopeError("quarantine GET closure is incomplete")


def verify_workflow_environment(
    repo_root: os.PathLike[str] | str,
    environ: Mapping[str, str] | None = None,
    ls_remote: Callable[[str, str], str] | None = None,
    quarantine_get: Callable[[str], Mapping[str, Any]] | None = None,
    quarantine_snapshot: Sequence[Mapping[str, Any]] | None = None,
) -> VerifiedControlIdentity:
    """Prove live Git/default/quarantine identity without any credential."""

    env = os.environ if environ is None else environ
    _verify_precheckout_git_environment(env)
    present_auth = sorted(key for key in _AUTH_ENV_KEYS if env.get(key))
    if present_auth:
        raise RuntimeEnvelopeError("ambient credential variables are forbidden: %s" % present_auth)
    present_proxy = sorted(key for key in _PROXY_ENV_KEYS if key in env)
    if present_proxy:
        raise RuntimeEnvelopeError("ambient proxy variables are forbidden: %s" % present_proxy)
    present_trust = sorted(key for key in _TRUST_OVERRIDE_ENV_KEYS if key in env)
    if present_trust:
        raise RuntimeEnvelopeError(
            "ambient TLS/proxy trust override variables are forbidden: %s" % present_trust
        )
    present_execution = sorted(
        key for key in _EXECUTION_OVERRIDE_ENV_KEYS if env.get(key)
    )
    if present_execution:
        raise RuntimeEnvelopeError(
            "ambient Node/Git execution override variables are forbidden: %s"
            % present_execution
        )
    contract = load_contract(repo_root)
    expected_env = {
        "GITHUB_REPOSITORY_ID": str(contract.repository_id),
        "GITHUB_REPOSITORY": contract.repository_full_name,
        "GITHUB_REF_NAME": contract.stable_branch,
        "GITHUB_WORKFLOW_REF": contract.expected_workflow_ref,
    }
    for key, expected in expected_env.items():
        if env.get(key) != expected:
            raise RuntimeEnvelopeError("%s does not match committed control identity" % key)
    selected_sha = env.get("GITHUB_SHA", "")
    _require_sha1(selected_sha, "GITHUB_SHA")
    resolver = _live_ls_remote if ls_remote is None else ls_remote
    output = resolver(REPOSITORY_HTTPS_URL, contract.stable_branch)
    if not isinstance(output, str):
        raise RuntimeEnvelopeError("ls_remote resolver returned non-text output")
    live_default, head_sha, stable_sha = _parse_ls_remote(output)
    if head_sha != stable_sha:
        raise RuntimeEnvelopeError("remote HEAD does not equal the stable branch head")
    verify_control_identity(
        repo_root,
        selected_ref=env["GITHUB_REF_NAME"],
        workflow_ref=env["GITHUB_WORKFLOW_REF"],
        live_default_branch=live_default,
        fetched_default_sha=head_sha,
        selected_sha=selected_sha,
        require_default=True,
    )
    if stable_sha != selected_sha:
        raise RuntimeEnvelopeError("selected workflow SHA is not the live stable head")
    if quarantine_snapshot is not None:
        if quarantine_get is not None:
            raise RuntimeEnvelopeError("quarantine test seams are mutually exclusive")
        validate_quarantine_snapshot(repo_root, quarantine_snapshot)
    else:
        _verify_live_quarantine(
            contract,
            _live_quarantine_get if quarantine_get is None else quarantine_get,
        )
    return VerifiedControlIdentity(
        contract=contract,
        selected_ref=env["GITHUB_REF_NAME"],
        selected_sha=selected_sha,
        live_default_branch=live_default,
        fetched_default_sha=head_sha,
    )


def _validated_live_workflow_set(
    value: Sequence[Mapping[str, Any]],
) -> list[dict[str, Any]]:
    """Return the exact active G1.1 workflow closure or fail closed."""

    if isinstance(value, (str, bytes)) or not isinstance(value, Sequence):
        raise RuntimeEnvelopeError("closed workflow set must be an ordered array")
    workflows = _thaw(value)
    expected_paths = [".github/workflows/ci.yml", WORKFLOW_PATH]
    if not isinstance(workflows, list) or len(workflows) != len(expected_paths):
        raise RuntimeEnvelopeError("closed workflow set is missing or has extra members")
    seen_ids: set[int] = set()
    for index, (workflow, expected_path) in enumerate(zip(workflows, expected_paths)):
        if not isinstance(workflow, dict) or set(workflow) != {
            "path",
            "workflow_id",
            "trusted_file_blob_sha",
            "state",
        }:
            raise RuntimeEnvelopeError("closed workflow member %d is malformed" % index)
        if workflow["path"] != expected_path:
            raise RuntimeEnvelopeError("closed workflow set path/order mismatch")
        workflow_id = workflow["workflow_id"]
        if (
            isinstance(workflow_id, bool)
            or not isinstance(workflow_id, int)
            or workflow_id <= 0
            or workflow_id in seen_ids
        ):
            raise RuntimeEnvelopeError("closed workflow id is invalid or duplicated")
        seen_ids.add(workflow_id)
        _require_sha1(
            workflow["trusted_file_blob_sha"],
            "closed workflow trusted_file_blob_sha",
        )
        if workflow["state"] != "active":
            raise RuntimeEnvelopeError("every closed workflow must be active")
    return workflows


def closed_workflow_set_identity(
    workflows: Sequence[Mapping[str, Any]],
) -> dict[str, Any]:
    """Build the canonical complete live workflow-set identity."""

    payload = {
        "schema": "closed-workflow-set/v1",
        "algorithm": "sha256-jcs",
        "workflows": _validated_live_workflow_set(workflows),
    }
    return {**payload, "digest": _canonical_json_sha256(payload)}


def aggregate_control_plane_identity(
    repo_root: os.PathLike[str] | str,
    closed_workflow_set: Mapping[str, Any],
) -> dict[str, Any]:
    """Bind updater source, Action identity, controls, and the full workflow set."""

    contract = load_contract(repo_root)
    if not isinstance(closed_workflow_set, Mapping) or set(closed_workflow_set) != {
        "schema",
        "algorithm",
        "workflows",
        "digest",
    }:
        raise RuntimeEnvelopeError("closed workflow-set identity is malformed")
    recomputed = closed_workflow_set_identity(closed_workflow_set["workflows"])
    if _thaw(closed_workflow_set) != recomputed:
        raise RuntimeEnvelopeError("closed workflow-set digest mismatch")
    payload = {
        "schema": "control-plane-identity/v1",
        "algorithm": "sha256-jcs",
        "control_input_digest": contract.control_input_digest,
        "updater_source_sha": contract.updater_source_sha,
        "pinned_action_digests": _thaw(contract.pinned_action_digests),
        "closed_workflow_set": recomputed,
    }
    return {**payload, "digest": _canonical_json_sha256(payload)}


def validate_qualifying_run_evidence(
    repo_root: os.PathLike[str] | str,
    evidence: Mapping[str, Any],
    *,
    current_live_default_sha: str,
    expected_closed_workflows: Sequence[Mapping[str, Any]],
    now_epoch: int,
    is_ancestor: Callable[[str, str], bool] | None = None,
) -> None:
    """Validate a future monitor sample without claiming it is commissioned."""

    contract = load_contract(repo_root)
    _require_sha1(current_live_default_sha, "current_live_default_sha")
    if isinstance(now_epoch, bool) or not isinstance(now_epoch, int) or now_epoch < 0:
        raise RuntimeEnvelopeError("now_epoch must be a non-negative integer")
    closed = closed_workflow_set_identity(expected_closed_workflows)
    aggregate = aggregate_control_plane_identity(repo_root, closed)
    required = {
        "event",
        "status",
        "conclusion",
        "workflow",
        "head_sha",
        "control_input_digest",
        "aggregate_control_plane_identity",
        "required_jobs",
        "closed_workflow_set",
        "scheduled_epoch",
        "completed_epoch",
    }
    if not isinstance(evidence, Mapping) or set(evidence) != required:
        raise RuntimeEnvelopeError("qualifying liveness evidence is not a closed record")
    exact = {
        "event": "schedule",
        "status": "completed",
        "conclusion": "success",
        "workflow": closed["workflows"][1],
        "control_input_digest": contract.control_input_digest,
        "aggregate_control_plane_identity": aggregate,
        "required_jobs": {RECONCILE_JOB_NAME: "success"},
        "closed_workflow_set": closed,
    }
    for key, expected in exact.items():
        if _thaw(evidence[key]) != expected:
            raise RuntimeEnvelopeError("qualifying liveness %s mismatch" % key)
    head_sha = evidence["head_sha"]
    _require_sha1(head_sha, "qualifying liveness head_sha")
    if head_sha != current_live_default_sha:
        if is_ancestor is None:
            raise RuntimeEnvelopeError(
                "qualifying liveness ancestor head lacks an ancestry proof"
            )
        try:
            proved_ancestor = is_ancestor(head_sha, current_live_default_sha)
        except Exception as exc:
            raise RuntimeEnvelopeError(
                "qualifying liveness ancestry resolver failed closed"
            ) from exc
        if proved_ancestor is not True:
            raise RuntimeEnvelopeError(
                "qualifying liveness head is not a proved ancestor of the live default"
            )
    scheduled = evidence["scheduled_epoch"]
    completed = evidence["completed_epoch"]
    if (
        isinstance(scheduled, bool)
        or isinstance(completed, bool)
        or not isinstance(scheduled, int)
        or not isinstance(completed, int)
        or scheduled < 0
        or completed < scheduled
        or completed > now_epoch
        or now_epoch - scheduled > 172800
    ):
        raise RuntimeEnvelopeError("qualifying liveness run is outside the 48-hour window")


def liveness_incident_id(
    *,
    monitor_policy_digest: str,
    workflow_id: int,
    failure_class: str,
    first_bad_run_or_grace_epoch: int,
) -> str:
    """Return the deterministic frozen-R0 incident dedupe key."""

    _require_sha256(monitor_policy_digest, "monitor_policy_digest")
    if isinstance(workflow_id, bool) or not isinstance(workflow_id, int) or workflow_id <= 0:
        raise RuntimeEnvelopeError("workflow_id must be a positive integer")
    if failure_class not in LIVENESS_FAILURE_CLASSES:
        raise RuntimeEnvelopeError("failure_class is outside the closed liveness set")
    if (
        isinstance(first_bad_run_or_grace_epoch, bool)
        or not isinstance(first_bad_run_or_grace_epoch, int)
        or first_bad_run_or_grace_epoch < 0
    ):
        raise RuntimeEnvelopeError("first bad run/grace epoch is invalid")
    return _canonical_json_sha256(
        {
            "monitor_policy_digest": monitor_policy_digest,
            "workflow_id": workflow_id,
            "failure_class": failure_class,
            "first_bad_run_or_grace_epoch": first_bad_run_or_grace_epoch,
        }
    )


def verify_liveness_evidence(
    repo_root: os.PathLike[str] | str,
    evidence: Mapping[str, Any],
    **live_facts: Any,
) -> None:
    """Validate evidence, then reject health until owner commissioning is recorded."""

    required_live_facts = {
        "current_live_default_sha",
        "expected_closed_workflows",
        "now_epoch",
    }
    supplied_live_facts = set(live_facts)
    if (
        not required_live_facts.issubset(supplied_live_facts)
        or supplied_live_facts - required_live_facts - {"is_ancestor"}
    ):
        raise RuntimeEnvelopeError("live liveness facts are incomplete")
    validate_qualifying_run_evidence(repo_root, evidence, **live_facts)
    contract = load_contract(repo_root)
    if contract.control_plane["commissioning"]["state"] != "COMMISSIONED":
        raise RuntimeEnvelopeError(
            "control plane is UNCOMMISSIONED; no healthy claim is possible"
        )


def assert_g11_state(repo_root: os.PathLike[str] | str, state: str) -> str:
    contract = load_contract(repo_root)
    allowed = tuple(contract.policy["allowed_production_states"])
    if state not in allowed or state in FORBIDDEN_STATES:
        raise RuntimeEnvelopeError("state %r is outside the G1.1 artifact-only state machine" % state)
    return state


def validate_quarantine_snapshot(
    repo_root: os.PathLike[str] | str, subjects: Sequence[Mapping[str, Any]]
) -> None:
    contract = load_contract(repo_root)
    expected_plain = _thaw(contract.quarantine["subjects"])
    provided = _thaw(subjects)
    if provided != expected_plain:
        raise RuntimeEnvelopeError("live legacy quarantine snapshot differs from committed identity")


def validate_external_dependencies(
    repo_root: os.PathLike[str] | str, ledger: Mapping[str, Any]
) -> None:
    root = _root(repo_root)
    committed = _read_json(root, DEPENDENCIES_REL)
    provided = _thaw(ledger)
    if provided != committed:
        raise RuntimeEnvelopeError("external dependency ledger differs from committed bytes")
    _validate_dependencies_document(provided)
    notice = root / "docs/agents/skills-maintenance/THIRD_PARTY_NOTICES.md"
    try:
        notice_text = notice.read_text(encoding="utf-8")
    except OSError as exc:
        raise RuntimeEnvelopeError("third-party notice is missing: %s" % exc) from exc
    for token in (
        "actions/checkout",
        CHECKOUT_COMMIT,
        CHECKOUT_TREE,
        CHECKOUT_LICENSE_SHA256,
        CHECKOUT_ACTION_SHA256,
        CHECKOUT_DIST_BLOB_SHA1,
        CHECKOUT_DIST_SHA256,
        CHECKOUT_TREE_MANIFEST_SHA256,
        CHECKOUT_PACKAGE_LOCK_BLOB_SHA1,
        CHECKOUT_PACKAGE_LOCK_SHA256,
        CHECKOUT_PIN_KEY,
        "MIT License",
        "actions/runner-images",
        "github-rest-quarantine-read",
        "github-rest-runner-image-release-read",
        "MANAGED_SERVICE_NO_EXECUTABLE_CODE_REDISTRIBUTED",
        "GitHub Terms of Service",
        str(RUNNER_IMAGES_REPOSITORY_ID),
        "provider-attested rolling `ImageVersion`, not a digest-pinned image",
        "internal.ubuntu24.json",
        "sha256:68c57165414e6868ea1b042b920640435daacf12eaa3bbdcaa85abbc4caac214",
        "OPTIONAL_HARDENING_UNAVAILABLE_NOT_REQUIRED",
        "CPython 3.12",
        "PSF-2.0",
        "GPL-2.0-only",
        "GNU Bash",
        "GPL-3.0-or-later",
        "GitHub Actions Node 20 handler",
        PRECHECKOUT_BOOTSTRAP_SHA256,
        "python3 -I -S -B",
        "GIT_CONFIG_COUNT",
        "GIT_CONFIG_PARAMETERS",
        "GIT_SSL_NO_VERIFY",
        "GIT_EXEC_PATH",
        "NODE_OPTIONS",
        "NODE_EXTRA_CA_CERTS",
        "NODE_TLS_REJECT_UNAUTHORIZED",
        "after checkout",
        "before updater or upstream",
        "HEAD^{commit}",
        "real Git worktree",
        "provider access",
        "The ledger does not claim that repository code can",
        "verify an Action before that Action is invoked",
    ):
        if token not in notice_text:
            raise RuntimeEnvelopeError("third-party notice lacks %s" % token)


def assert_transport(repo_root: os.PathLike[str] | str, transport: str) -> str:
    contract = load_contract(repo_root)
    allowed = frozenset(contract.external_dependencies["transport"]["allowed"])
    if transport not in ALLOWED_TRANSPORTS or transport not in allowed:
        raise RuntimeEnvelopeError("transport %r is outside the committed envelope" % transport)
    return transport


def _semantic_workflow_text(text: str) -> str:
    lines = []
    for raw in text.splitlines():
        stripped = raw.lstrip()
        if not stripped or stripped.startswith("#"):
            continue
        # Workflow policy tokens do not contain '#'.  Removing trailing comments makes
        # comments incapable of satisfying or violating the static authority checks.
        lines.append(raw.split("#", 1)[0].rstrip())
    return "\n".join(lines) + "\n"


def _block(text: str, top_key: str, next_top_keys: Sequence[str]) -> str:
    match = re.search(r"(?m)^%s:\s*$" % re.escape(top_key), text)
    if match is None:
        raise RuntimeEnvelopeError("missing workflow block %s" % top_key)
    end = len(text)
    for key in next_top_keys:
        candidate = re.search(r"(?m)^%s:\s*$" % re.escape(key), text[match.end() :])
        if candidate is not None:
            end = min(end, match.end() + candidate.start())
    return text[match.start() : end]


def _checkout_blocks(text: str) -> list[str]:
    lines = text.splitlines()
    blocks: list[str] = []
    for index, line in enumerate(lines):
        if "uses: actions/checkout@" not in line:
            continue
        indent = len(line) - len(line.lstrip())
        end = index + 1
        while end < len(lines):
            candidate = lines[end]
            if candidate.strip() and len(candidate) - len(candidate.lstrip()) <= indent and candidate.lstrip().startswith("-"):
                break
            end += 1
        blocks.append("\n".join(lines[index:end]))
    return blocks


def validate_checkout_contract(
    repo_root: os.PathLike[str] | str, text: str, *, fetch_depth: int = 1
) -> None:
    load_contract(repo_root)
    semantic = _semantic_workflow_text(text)
    blocks = _checkout_blocks(semantic)
    if not blocks:
        raise RuntimeEnvelopeError("workflow has no actions/checkout use")
    expected_uses = "uses: actions/checkout@" + CHECKOUT_COMMIT
    expected_inputs = _checkout_input_envelope(fetch_depth, workflow_scalars=True)
    for block in blocks:
        if block.count(expected_uses) != 1:
            raise RuntimeEnvelopeError("actions/checkout is not pinned to the audited commit")
        lines = block.splitlines()
        with_indexes = [i for i, line in enumerate(lines) if line.strip() == "with:"]
        if len(with_indexes) != 1:
            raise RuntimeEnvelopeError("checkout block must have one closed input map")
        start = with_indexes[0]
        with_indent = len(lines[start]) - len(lines[start].lstrip())
        inputs: dict[str, str] = {}
        for line in lines[start + 1 :]:
            if not line.strip():
                continue
            indent = len(line) - len(line.lstrip())
            if indent <= with_indent:
                break
            match = re.fullmatch(r"\s*([A-Za-z0-9-]+):\s*(.*?)\s*", line)
            if match is None or match.group(1) in inputs:
                raise RuntimeEnvelopeError("checkout input map is malformed or duplicated")
            inputs[match.group(1)] = match.group(2)
        if inputs != expected_inputs:
            raise RuntimeEnvelopeError(
                "checkout input envelope mismatch: expected=%s actual=%s"
                % (sorted(expected_inputs.items()), sorted(inputs.items()))
            )
        if re.search(r"(?m)^\s+(token|ssh-key):", block):
            raise RuntimeEnvelopeError("checkout block may not inject a credential")


def validate_registered_workflows(repo_root: os.PathLike[str] | str) -> None:
    root = _root(repo_root)
    workflow_dir = root / ".github/workflows"
    if not workflow_dir.is_dir():
        raise RuntimeEnvelopeError("workflow directory is missing")
    actual = {
        path.relative_to(root).as_posix()
        for path in workflow_dir.iterdir()
        if path.is_file() and path.suffix in {".yml", ".yaml"}
    }
    expected = {
        ".github/workflows/ci.yml",
        ".github/workflows/stable-release.yml",
        WORKFLOW_PATH,
    }
    if actual != expected:
        raise RuntimeEnvelopeError(
            "stable workflow registration mismatch: expected=%s actual=%s"
            % (sorted(expected), sorted(actual))
        )
    if (root / DONOR_WORKFLOW_PATH).exists():
        raise RuntimeEnvelopeError("historical donor workflow is forbidden on stable")


def _literal_string_collection(node: ast.AST) -> set[str] | None:
    if not isinstance(node, (ast.Tuple, ast.List, ast.Set)):
        return None
    values: set[str] = set()
    for item in node.elts:
        if not isinstance(item, ast.Constant) or not isinstance(item.value, str):
            return None
        values.add(item.value)
    return values


def _governance_workflow_allowlist(source: str) -> set[str]:
    try:
        tree = ast.parse(source)
    except SyntaxError as exc:
        raise RuntimeEnvelopeError("stable governance validator does not parse: %s" % exc) from exc
    values: set[str] | None = None
    for node in tree.body:
        if isinstance(node, ast.Assign) and any(
            isinstance(target, ast.Name) and target.id == "GOVERNANCE_OWNED_WORKFLOWS"
            for target in node.targets
        ):
            if values is not None:
                raise RuntimeEnvelopeError("GOVERNANCE_OWNED_WORKFLOWS assigned more than once")
            values = _literal_string_collection(node.value)
    if values is None:
        raise RuntimeEnvelopeError("GOVERNANCE_OWNED_WORKFLOWS must be one literal collection")
    return values


def validate_stable_authority(repo_root: os.PathLike[str] | str) -> None:
    root = _root(repo_root)
    ci_path = root / ".github/workflows/ci.yml"
    validator_path = root / "scripts/validate-agent-governance.py"
    try:
        ci = _semantic_workflow_text(ci_path.read_text(encoding="utf-8"))
        validator = validator_path.read_text(encoding="utf-8")
    except OSError as exc:
        raise RuntimeEnvelopeError("stable authority owner is unreadable: %s" % exc) from exc
    if ci.count('branches: ["release/**"]') != 2 or 'branches: ["main"]' in ci:
        raise RuntimeEnvelopeError("CI branch ownership is not stable release/**")
    if not re.search(r"(?m)^  governance:\s*$", ci):
        raise RuntimeEnvelopeError("CI lacks the stable governance job")
    if _raw_sha256(validator_path) == _DONOR_VALIDATOR_SHA256:
        raise RuntimeEnvelopeError("donor validator bytes replaced stable authority")
    required_stable_needles = (
        "GOVERNANCE_V3.md",
        "governance_v3_recovery_details",
        "check_governance_v3_recovery_contract",
        "CHECKPOINT_CONTRACT_ID",
    )
    for needle in required_stable_needles:
        if needle not in validator:
            raise RuntimeEnvelopeError("stable validator lost %s" % needle)
    if _governance_workflow_allowlist(validator) != set(_EXPECTED_GOVERNANCE_WORKFLOWS):
        raise RuntimeEnvelopeError("stable governance workflow allowlist mismatch")


def _reject_workflow_mutation_surface(text: str, where: str) -> None:
    prohibited_patterns = {
        "write permission": r"(?m)^\s*(?:contents|pull-requests|issues|actions|id-token):\s*write\s*$",
        "secret reference": r"\$\{\{[^}\n]*\bsecrets\s*(?:\.|\[)",
        "ambient token use": (
            r"(?:\$\{\{[^}\n]*(?:GITHUB_TOKEN|GH_TOKEN|GITHUB_PAT|CONTROL_PLANE_TOKEN|"
            r"github\s*(?:\.token|\[\s*['\"]token['\"]\s*\]))|"
            r"(?m:^\s*(?:GITHUB_TOKEN|GH_TOKEN|GITHUB_PAT|"
            r"CONTROL_PLANE_TOKEN)\s*:)|Authorization\s*:)"
        ),
        "publisher module": r"(?:--publish\b|skill_updates/(?:publish|ghadapter)\.py|skill_updates\.(?:publish|ghadapter))",
        "git push": r"\bgit\s+push\b",
        "GitHub mutation CLI": r"\bgh\s+(?:pr|issue|api|workflow)\b",
        "HTTP mutation": r"\bcurl\b[^\n]*(?:-X|--request)\s*(?:POST|PUT|PATCH|DELETE)\b",
        "privileged trigger": r"(?m)^\s*(?:pull_request_target|workflow_run):\s*$",
        "model invocation": r"\b(?:claude|codex|openai)\b",
    }
    for label, pattern in prohibited_patterns.items():
        if re.search(pattern, text, flags=re.IGNORECASE):
            raise RuntimeEnvelopeError("%s exposes forbidden %s" % (where, label))


def _validate_precheckout_bootstrap_bodies(
    raw: str, *, expected_count: int, where: str
) -> None:
    """Bind every pre-checkout program to the reviewed canonical source bytes."""

    bodies = [match.group("body").encode("utf-8") for match in _PRECHECKOUT_BOOTSTRAP_RE.finditer(raw)]
    if len(bodies) != expected_count:
        raise RuntimeEnvelopeError(
            "%s pre-checkout bootstrap count mismatch" % where
        )
    digests = [hashlib.sha256(body).hexdigest() for body in bodies]
    if any(digest != PRECHECKOUT_BOOTSTRAP_SHA256 for digest in digests):
        raise RuntimeEnvelopeError(
            "%s pre-checkout bootstrap source digest mismatch" % where
        )


def _reject_reserved_provider_env_assignments(raw: str, where: str) -> None:
    """Forbid any YAML spelling that can shadow provider-owned identity.

    This is deliberately a closed source grammar, not a permissive YAML parser.
    Production workflows use block-style ``env`` maps with simple scalar values;
    flow maps, aliases, anchors, merge keys and duplicate keys are all rejected.
    """

    env_header = re.compile(
        r"^(?P<indent>[ ]*)(?P<quote>['\"]?)env(?P=quote)\s*:\s*(?P<tail>.*?)\s*$"
    )
    unsupported_yaml_key = re.compile(
        r'(?m)^[ ]*(?:[?!]|[&*][^:\n]*:|"[^"\n]*\\[^"\n]*"\s*:)'
    )
    assignment = re.compile(
        r"^(?P<indent>[ ]+)(?P<quote>['\"]?)(?P<key>[A-Za-z_][A-Za-z0-9_]*)(?P=quote)"
        r"\s*:\s*(?P<value>.*?)\s*$"
    )
    lines = raw.splitlines()
    shadowed: set[str] = set()
    if unsupported_yaml_key.search(raw):
        raise RuntimeEnvelopeError(
            "%s contains an unsupported explicit, tagged, anchored, aliased, or escaped YAML key"
            % where
        )
    for index, line in enumerate(lines):
        header = env_header.match(line)
        if header is None:
            continue
        if header.group("tail"):
            raise RuntimeEnvelopeError(
                "%s contains a non-block or aliased env mapping" % where
            )
        parent_indent = len(header.group("indent"))
        child_indent: int | None = None
        keys: set[str] = set()
        for child in lines[index + 1 :]:
            if not child.strip() or child.lstrip().startswith("#"):
                continue
            indentation = len(child) - len(child.lstrip(" "))
            if indentation <= parent_indent:
                break
            if child_indent is None:
                child_indent = indentation
            if indentation != child_indent:
                continue
            stripped = child.strip()
            if stripped.startswith("<<:"):
                raise RuntimeEnvelopeError("%s contains an env merge key" % where)
            match = assignment.match(child)
            if match is None:
                raise RuntimeEnvelopeError(
                    "%s contains an unsupported env key spelling" % where
                )
            key = match.group("key")
            value = match.group("value")
            if key in keys:
                raise RuntimeEnvelopeError(
                    "%s contains duplicate env key %s" % (where, key)
                )
            keys.add(key)
            if re.search(r"(?:^|[\s\[{,])(?:&|\*)[A-Za-z0-9_-]+", value):
                raise RuntimeEnvelopeError("%s contains an env anchor or alias" % where)
            if key in _RESERVED_PROVIDER_ENV_NAMES or key.startswith(
                _RESERVED_PROVIDER_ENV_PREFIXES
            ):
                shadowed.add(key)
    if shadowed:
        raise RuntimeEnvelopeError(
            "%s shadows provider-reserved environment: %s"
            % (where, sorted(shadowed))
        )


def _validate_isolated_python_invocations(text: str, where: str) -> None:
    """Require isolated direct entrypoints for every production Python process."""

    for line in text.splitlines():
        if re.search(r"\bpython3\b", line) is None:
            continue
        if "python3 -I -S -B" not in line:
            raise RuntimeEnvelopeError(
                "%s contains a non-isolated Python invocation" % where
            )
        if re.search(r"\bpython3 -I -S -B\s+(?:-m|-c)\b", line):
            raise RuntimeEnvelopeError(
                "%s contains an inline or module-mode production Python invocation" % where
            )


def _validate_exact_step_closure(
    job_text: str, expected_names: Sequence[str], where: str
) -> None:
    """Admit only the reviewed ordered step set for one production job."""

    step_lines = re.findall(r"(?m)^      - (?P<body>.+?)\s*$", job_text)
    names: list[str] = []
    for body in step_lines:
        match = re.fullmatch(r"name:\s*(.+?)\s*", body)
        if match is None:
            raise RuntimeEnvelopeError("%s contains an unnamed or direct step" % where)
        names.append(match.group(1))
    if names != list(expected_names):
        raise RuntimeEnvelopeError("%s step closure mismatch" % where)


def validate_artifact_only_workflow(repo_root: os.PathLike[str] | str) -> None:
    root = _root(repo_root)
    path = root / WORKFLOW_PATH
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise RuntimeEnvelopeError("stable workflow missing: %s" % exc) from exc
    validate_artifact_only_workflow_text(root, text)


def validate_artifact_only_workflow_text(
    repo_root: os.PathLike[str] | str, text: str
) -> None:
    """Validate stable-workflow bytes through a mutation-testable source seam."""

    root = _root(repo_root)
    if hashlib.sha256(text.encode("utf-8")).hexdigest() != STABLE_SKILLS_WORKFLOW_SHA256:
        raise RuntimeEnvelopeError("stable workflow source digest mismatch")
    _validate_precheckout_bootstrap_bodies(
        text, expected_count=2, where=WORKFLOW_PATH
    )
    _reject_reserved_provider_env_assignments(text, WORKFLOW_PATH)
    semantic = _semantic_workflow_text(text)
    _validate_isolated_python_invocations(semantic, WORKFLOW_PATH)
    _reject_workflow_mutation_surface(semantic, WORKFLOW_PATH)
    if not re.search(r"(?m)^permissions:\s*\{\}\s*$", semantic):
        raise RuntimeEnvelopeError("stable workflow top-level permissions must be empty")
    if not re.search(r"(?m)^  detect:\s*$", semantic):
        raise RuntimeEnvelopeError("stable workflow must have exactly owned detect job")
    job_keys = re.findall(r"(?m)^  ([A-Za-z0-9_-]+):\s*$", _block(semantic, "jobs", ()))
    if job_keys != ["detect", "prepare", "reconcile-all"]:
        raise RuntimeEnvelopeError("stable workflow job closure mismatch")
    detect = _job_block(semantic, "detect")
    prepare = _job_block(semantic, "prepare")
    reconcile = _job_block(semantic, "reconcile-all")
    _validate_exact_step_closure(
        detect,
        (
            "Prove the pinned runner Python and Git envelope",
            "Check out the trusted stable event head",
            "Prove stable workflow and control-plane identity",
            "Refuse detection when governance or updater invariants are red",
            "Detect and classify upstream drift",
            "Build the inert preparation matrix",
        ),
        "stable detect",
    )
    _validate_exact_step_closure(
        prepare,
        (
            "Prove the pinned runner Python and Git envelope",
            "Check out the trusted stable event head",
            "Re-prove stable identity before preparation",
            "Prepare candidate content outside the repository checkout",
        ),
        "stable prepare",
    )
    _validate_exact_step_closure(
        reconcile,
        ("Refuse detector-only liveness and re-prove the runner envelope",),
        "stable reconcile-all",
    )
    for name, block in (("detect", detect), ("prepare", prepare)):
        if "runs-on: " + RUNNER_LABEL not in block:
            raise RuntimeEnvelopeError("%s runner is outside the envelope" % name)
        if not re.search(r"(?m)^    permissions:\s*$\n\s{6}contents:\s*read\s*$", block):
            raise RuntimeEnvelopeError("%s must have only contents: read" % name)
        if (
            "python3 -I -S -B scripts/skill_updates/runtime.py "
            "verify-workflow --repo-root ."
        ) not in block:
            raise RuntimeEnvelopeError("%s lacks the runtime control gate" % name)
    if not re.search(r"(?m)^    permissions:\s*\{\}\s*$", reconcile):
        raise RuntimeEnvelopeError("reconcile-all permissions must be empty")
    if reconcile.count("    name: " + RECONCILE_JOB_NAME) != 1:
        raise RuntimeEnvelopeError(
            "reconcile-all YAML key lacks the exact externally observable job name"
        )
    if "actions/checkout@" in reconcile:
        raise RuntimeEnvelopeError("reconcile-all may not check out repository bytes")
    guards = (
        "github.repository == '" + REPOSITORY_FULL_NAME + "'",
        "github.repository_id == '" + str(REPOSITORY_ID) + "'",
        "github.ref == 'refs/heads/" + STABLE_BRANCH + "'",
        "github.workflow_ref == '" + EXPECTED_WORKFLOW_REF + "'",
    )
    if any(guard not in detect for guard in guards):
        raise RuntimeEnvelopeError("detect job lacks the exact stable event guard")
    if "needs: detect" not in prepare or "needs.detect.result == 'success'" not in prepare:
        raise RuntimeEnvelopeError("prepare job is not gated on successful detection")
    if len(_checkout_blocks(semantic)) != 2:
        raise RuntimeEnvelopeError("stable workflow checkout consumer closure mismatch")
    if semantic.count('--artifact-root "$RUNNER_TEMP/stable-skills-$PROVIDER"') != 1:
        raise RuntimeEnvelopeError("prepare artifact root is not the flat runner-temp path")
    validate_checkout_contract(root, text)


def _job_block(text: str, job: str) -> str:
    lines = text.splitlines()
    start = None
    for index, line in enumerate(lines):
        if line == "  %s:" % job:
            start = index
            break
    if start is None:
        raise RuntimeEnvelopeError("workflow lacks job %s" % job)
    end = len(lines)
    for index in range(start + 1, len(lines)):
        if re.match(r"^  [A-Za-z0-9_-]+:\s*$", lines[index]):
            end = index
            break
    return "\n".join(lines[start:end]) + "\n"


def validate_ci_trust_boundary(repo_root: os.PathLike[str] | str) -> None:
    root = _root(repo_root)
    try:
        raw = (root / ".github/workflows/ci.yml").read_text(encoding="utf-8")
    except OSError as exc:
        raise RuntimeEnvelopeError("CI workflow missing: %s" % exc) from exc
    validate_ci_trust_boundary_text(root, raw)


def validate_ci_trust_boundary_text(
    repo_root: os.PathLike[str] | str, raw: str
) -> None:
    """Validate CI semantics with an explicit source seam for mutation tests."""
    root = _root(repo_root)
    if raw.count(_CI_BLOCK_BEGIN) != 1 or raw.count(_CI_BLOCK_END) != 1:
        raise RuntimeEnvelopeError("CI governance marker closure mismatch")
    start = raw.index(_CI_BLOCK_BEGIN)
    end = raw.index(_CI_BLOCK_END, start) + len(_CI_BLOCK_END)
    governance_source = raw[start:end]
    if (
        hashlib.sha256(governance_source.encode("utf-8")).hexdigest()
        != STABLE_CI_GOVERNANCE_BLOCK_SHA256
    ):
        raise RuntimeEnvelopeError("CI governance source digest mismatch")
    _validate_precheckout_bootstrap_bodies(raw, expected_count=1, where="CI")
    _reject_reserved_provider_env_assignments(raw, "CI")
    text = _semantic_workflow_text(raw)
    _validate_isolated_python_invocations(_job_block(text, "governance"), "CI governance")
    _reject_workflow_mutation_surface(text, "CI")
    if text.count('branches: ["release/**"]') != 2 or 'branches: ["main"]' in text:
        raise RuntimeEnvelopeError("CI must target release/** for push and pull_request")
    if not re.search(r"(?m)^permissions:\s*$\n\s{2}contents:\s*read\s*$", text):
        raise RuntimeEnvelopeError("CI top-level permissions must be contents: read")
    governance = _job_block(text, "governance")
    _validate_exact_step_closure(
        governance,
        (
            "Prove the pinned runner Python and Git envelope",
            "Check out the event head and comparison history without persisted credentials",
            "Validate stable governance and updater integration",
            "Run validator fixture self-tests",
            "Verify mutation probe anchors",
            "Run updater regression suite",
        ),
        "CI governance",
    )
    if "runs-on: " + RUNNER_LABEL not in governance:
        raise RuntimeEnvelopeError("governance job runner mismatch")
    if not re.search(r"(?m)^    permissions:\s*$\n\s{6}contents:\s*read\s*$", governance):
        raise RuntimeEnvelopeError("governance job permissions must be contents: read")
    if len(_checkout_blocks(governance)) != 1:
        raise RuntimeEnvelopeError("governance checkout consumer closure mismatch")
    validate_checkout_contract(root, governance, fetch_depth=0)
    comparison_sha = (
        "GOVERNANCE_BASE_SHA: ${{ github.event_name == 'pull_request' && "
        "github.event.pull_request.base.sha || github.event.before }}"
    )
    if governance.count(comparison_sha) != 1:
        raise RuntimeEnvelopeError("CI governance job lacks the exact PR/push comparison SHA")
    if (
        "python3 -I -S -B scripts/skill_updates/runtime.py "
        "verify-repository --repo-root ."
    ) not in governance:
        raise RuntimeEnvelopeError("CI governance job lacks static runtime verification")


def _verify_local_executable_envelope() -> None:
    """Canary the repository-invoked executables without claiming a runner fact."""

    if sys.implementation.name != "cpython" or sys.version_info[:2] != PYTHON_MAJOR_MINOR:
        raise RuntimeEnvelopeError("local Python must be CPython 3.12.x")
    bash = _actual_bash_version()
    if bash[:2] != BASH_MAJOR_MINOR:
        raise RuntimeEnvelopeError("local Bash must be GNU Bash 5.2.x")
    controlled = controlled_git_environment()
    git = _actual_git_version(controlled)
    if git < GIT_MIN_INCLUSIVE or git >= GIT_MAX_EXCLUSIVE:
        raise RuntimeEnvelopeError("local Git is outside [2.43.0, 2.60.0)")


def verify_repository(repo_root: os.PathLike[str] | str) -> RuntimeContract:
    """Run every static G1.1 repository/runtime authority check; no network."""

    root = _root(repo_root)
    contract = load_contract(root)
    validate_registered_workflows(root)
    validate_stable_authority(root)
    validate_artifact_only_workflow(root)
    validate_ci_trust_boundary(root)
    validate_quarantine_snapshot(root, contract.quarantine["subjects"])
    validate_external_dependencies(root, contract.external_dependencies)
    assert_transport(root, "git-https")
    assert_transport(root, "github-rest-runner-image-release-read")
    if os.environ.get("GITHUB_ACTIONS") == "true":
        verify_hosted_runtime_environment(root)
        verify_checkout_worktree_identity(root)
    else:
        _verify_local_executable_envelope()
    return contract


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    for name in ("verify-repository", "verify-workflow"):
        command = subparsers.add_parser(name)
        command.add_argument("--repo-root", default=".")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        if args.command == "verify-repository":
            verify_repository(args.repo_root)
        elif args.command == "verify-workflow":
            verify_repository(args.repo_root)
            verify_workflow_environment(args.repo_root)
        else:  # pragma: no cover - argparse owns this branch
            raise RuntimeEnvelopeError("unknown command")
    except RuntimeEnvelopeError as exc:
        print("[FAIL] stable-skills-runtime: %s" % exc, file=sys.stderr)
        return 1
    print("[PASS] stable-skills-runtime: %s" % args.command)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
