"""G1.1 production-seam tests for the stable-native runtime/control envelope.

These tests are deliberately written against the public production ownership seams.  The
initial stable tree has none of them, which is the required RED.  They become GREEN only when
the committed workflow, policy ledgers, runtime guard, candidate identity, and state machine
agree on the same inert stable-native contract.
"""

import importlib.util
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[3]
RUNTIME_PATH = ROOT / "scripts" / "skill_updates" / "runtime.py"
WORKFLOW_PATH = ROOT / ".github" / "workflows" / "stable-skills-maintenance.yml"
POLICY_PATH = ROOT / "docs" / "agents" / "skills-maintenance" / "policy.json"
CONTROL_PATH = ROOT / "docs" / "agents" / "skills-maintenance" / "control-plane.json"
QUARANTINE_PATH = ROOT / "docs" / "agents" / "skills-maintenance" / "legacy-quarantine.json"
DEPENDENCIES_PATH = ROOT / "docs" / "agents" / "skills-maintenance" / "external-dependencies.json"


def _load_json(path):
    if not path.is_file():
        raise AssertionError("missing production contract: %s" % path.relative_to(ROOT))
    with path.open(encoding="utf-8") as handle:
        return json.load(handle)


def _runtime():
    if not RUNTIME_PATH.is_file():
        raise AssertionError("missing production owner: scripts/skill_updates/runtime.py")
    spec = importlib.util.spec_from_file_location("g11_runtime_red", RUNTIME_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class StableNativeFoundationRed(unittest.TestCase):
    def test_01_proposal_identity_is_the_exact_twelve_field_contract(self):
        runtime = _runtime()
        contract = runtime.load_contract(ROOT)
        inputs = dict(
            provider="awesome-copilot",
            stable_branch="release/0.3",
            stable_base_sha="a" * 40,
            target_sha="b" * 40,
            upstream_repo="https://github.com/github/awesome-copilot",
            old_pin="c" * 40,
            control_input_digest=contract.control_input_digest,
            updater_source_sha=contract.updater_source_sha,
            pinned_action_digests=contract.pinned_action_digests,
        )
        identity = runtime.candidate_identity(**inputs)
        document = identity.to_dict()
        payload_keys = (
            "schema", "repo_id", "repo_full_name", "stable_branch", "stable_base_sha",
            "provider", "upstream_repo", "old_pin", "target_sha", "control_input_digest",
            "updater_source_sha", "pinned_action_digests",
        )
        payload = {key: document[key] for key in payload_keys}
        expected = hashlib.sha256(json.dumps(
            payload, ensure_ascii=False, sort_keys=True, separators=(",", ":")
        ).encode("utf-8")).hexdigest()
        self.assertEqual(expected, identity.proposal_id)
        self.assertEqual(set(payload_keys) | {"proposal_id", "locator"}, set(document))
        self.assertNotIn("policy_digest", document)
        self.assertNotIn("subject_kind", document)
        self.assertIn("release-0.3", identity.locator)
        self.assertIn(("a" * 12), identity.locator)
        self.assertNotEqual(
            identity.proposal_id,
            runtime.candidate_identity(**dict(inputs, stable_base_sha="d" * 40)).proposal_id,
        )
        self.assertNotEqual(
            identity.proposal_id,
            runtime.candidate_identity(
                **dict(inputs, control_input_digest="d" * 64)
            ).proposal_id,
        )

    def test_02_wrong_default_or_committed_base_fails_closed(self):
        runtime = _runtime()
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            runtime.verify_control_identity(
                ROOT,
                selected_ref="release/0.3",
                workflow_ref="KraineOpasen/bukerov-twitch-miner-go/.github/workflows/"
                "stable-skills-maintenance.yml@refs/heads/release/0.3",
                live_default_branch="release/0.2",
                fetched_default_sha="a" * 40,
                selected_sha="a" * 40,
                require_default=True,
            )

    def test_03_dispatch_from_main_cannot_enter_artifact_path(self):
        runtime = _runtime()
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            runtime.verify_control_identity(
                ROOT,
                selected_ref="main",
                workflow_ref="KraineOpasen/bukerov-twitch-miner-go/.github/workflows/"
                "stable-skills-maintenance.yml@refs/heads/main",
                live_default_branch="main",
                fetched_default_sha="a" * 40,
                selected_sha="a" * 40,
                require_default=False,
            )

    def test_04_public_fork_workflow_is_not_assumed_enabled(self):
        control = _load_json(CONTROL_PATH)
        self.assertEqual("UNCOMMISSIONED", control["commissioning"]["state"])
        self.assertTrue(control["commissioning"]["owner_action_required"])
        self.assertFalse(control["commissioning"]["workflow_enabled_assumed"])

    def test_05_detector_success_is_not_full_control_plane_liveness(self):
        runtime = _runtime()
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            runtime.verify_liveness_evidence(ROOT, {"detector": "success"})

    def test_06_broad_donor_workflow_registration_is_rejected(self):
        runtime = _runtime()
        runtime.validate_registered_workflows(ROOT)
        self.assertFalse((ROOT / ".github" / "workflows" / "skills-update.yml").exists())

    def test_07_donor_ci_or_validator_authority_is_rejected(self):
        runtime = _runtime()
        runtime.validate_stable_authority(ROOT)

    def test_08_g11_states_cannot_claim_audit_ready_arm_or_merge(self):
        runtime = _runtime()
        policy = _load_json(POLICY_PATH)
        self.assertEqual(
            ["NO_DRIFT", "BLOCKED", "PREPARED_AUDIT_REQUIRED"],
            policy["allowed_production_states"],
        )
        for forbidden in ("AUDITED", "READY", "ARMED", "MERGED"):
            with self.assertRaises(runtime.RuntimeEnvelopeError):
                runtime.assert_g11_state(ROOT, forbidden)

    def test_09_workflow_has_zero_reachable_github_mutation(self):
        runtime = _runtime()
        runtime.validate_artifact_only_workflow(ROOT)

    def test_10_quarantine_exact_match_and_mismatch_fail_closed(self):
        runtime = _runtime()
        ledger = _load_json(QUARANTINE_PATH)
        runtime.validate_quarantine_snapshot(ROOT, ledger["subjects"])
        corrupt = json.loads(json.dumps(ledger["subjects"]))
        corrupt[0]["node_id"] += "-wrong"
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            runtime.validate_quarantine_snapshot(ROOT, corrupt)

    def test_10a_closed_pull_request_is_rejected_by_runtime_contract(self):
        runtime = _runtime()
        ledger = _load_json(QUARANTINE_PATH)
        ledger["subjects"][0]["state"] = "CLOSED"
        ledger["binding"]["subjects_sha256"] = runtime._canonical_json_sha256(
            ledger["subjects"]
        )
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            runtime._validate_quarantine_document(
                ledger,
                policy_sha256=runtime._raw_sha256(ROOT / runtime.POLICY_REL),
                control_sha256=runtime._raw_sha256(ROOT / runtime.CONTROL_REL),
            )

    def test_10b_live_quarantine_projection_is_unique_anchored_and_exact(self):
        runtime = _runtime()
        subjects = _load_json(QUARANTINE_PATH)["subjects"]

        def live_for(expected):
            target_label = (
                "seen at commit" if expected["number"] == 240 else "target commit"
            )
            live = {
                "number": expected["number"],
                "node_id": expected["node_id"],
                "title": expected["title"],
                "state": expected["state"].lower(),
                "user": {"login": expected["author"]},
                "body": (
                    "| field | value |\n"
                    "| --- | --- |\n"
                    "| provider | `%s` |\n"
                    "| %s | `%s` |\n"
                    % (expected["provider"], target_label, expected["target_sha"])
                ),
            }
            if expected["kind"] == "pull_request":
                live.update(
                    {
                        "draft": expected["draft"],
                        "head": {
                            "ref": expected["head_ref"],
                            "sha": expected["head_sha"],
                        },
                        "base": {
                            "ref": expected["base_ref"],
                            "sha": expected["base_sha"],
                        },
                    }
                )
            else:
                live["labels"] = [{"name": expected["label"]}]
            return live

        for expected in (subjects[0], subjects[-1]):
            with self.subTest(number=expected["number"], mutation="positive"):
                runtime._api_identity_matches(expected, live_for(expected))
            target_label = (
                "seen at commit" if expected["number"] == 240 else "target commit"
            )
            exact_target = "| %s | `%s` |" % (
                target_label,
                expected["target_sha"],
            )
            wrong_target = "| %s | `%s` |" % (target_label, "f" * 40)
            mutations = []

            wrong_only = live_for(expected)
            wrong_only["body"] = wrong_only["body"].replace(exact_target, wrong_target)
            mutations.append(("wrong-only", wrong_only))

            expected_plus_wrong = live_for(expected)
            expected_plus_wrong["body"] += wrong_target + "\n"
            mutations.append(("expected-plus-wrong", expected_plus_wrong))

            duplicate = live_for(expected)
            duplicate["body"] += exact_target + "\n"
            mutations.append(("duplicate", duplicate))

            incidental = live_for(expected)
            incidental["body"] = incidental["body"].replace(exact_target + "\n", "")
            incidental["repository"] = {"description": expected["target_sha"]}
            mutations.append(("incidental-nested-string", incidental))

            duplicate_provider = live_for(expected)
            duplicate_provider["body"] += (
                "| provider | `%s` |\n" % expected["provider"]
            )
            mutations.append(("duplicate-provider", duplicate_provider))

            for label, live in mutations:
                with self.subTest(number=expected["number"], mutation=label):
                    with self.assertRaises(runtime.RuntimeEnvelopeError):
                        runtime._api_identity_matches(expected, live)

    def test_11_floating_action_or_dependency_identity_fails(self):
        runtime = _runtime()
        ledger = _load_json(DEPENDENCIES_PATH)
        runtime.validate_external_dependencies(ROOT, ledger)
        corrupt = json.loads(json.dumps(ledger))
        corrupt["dependencies"][0]["commit"] = "v4"
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            runtime.validate_external_dependencies(ROOT, corrupt)

    def test_12_checkout_tree_licence_or_input_mismatch_fails(self):
        runtime = _runtime()
        ledger = _load_json(DEPENDENCIES_PATH)
        for field in ("tree", "license_sha256", "action_sha256"):
            corrupt = json.loads(json.dumps(ledger))
            corrupt["dependencies"][0][field] = "0" * 40 if field == "tree" else "0" * 64
            with self.assertRaises(runtime.RuntimeEnvelopeError, msg=field):
                runtime.validate_external_dependencies(ROOT, corrupt)

    def test_13_persisted_checkout_credentials_fail(self):
        runtime = _runtime()
        text = WORKFLOW_PATH.read_text(encoding="utf-8") if WORKFLOW_PATH.exists() else ""
        runtime.validate_checkout_contract(ROOT, text)
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            runtime.validate_checkout_contract(
                ROOT, text.replace("persist-credentials: false", "persist-credentials: true", 1)
            )

    def test_14_privileged_job_cannot_checkout_candidate_bytes(self):
        runtime = _runtime()
        text = WORKFLOW_PATH.read_text(encoding="utf-8") if WORKFLOW_PATH.exists() else ""
        runtime.validate_checkout_contract(ROOT, text)
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            runtime.validate_checkout_contract(
                ROOT,
                text.replace(
                    "ref: ${{ github.sha }}",
                    "ref: ${{ github.event.pull_request.head.sha }}",
                    1,
                ),
            )

    def test_15_untrusted_pr_ci_has_no_write_or_secret_authority(self):
        runtime = _runtime()
        runtime.validate_ci_trust_boundary(ROOT)
        ci = (ROOT / ".github" / "workflows" / "ci.yml").read_text(encoding="utf-8")
        for old, new in (
            ("fetch-depth: 0", "fetch-depth: 1"),
            ("--application-scope generic", "--application-scope g1-stable-skills"),
            ("      BASH_ENV: /dev/null",
             "      BASH_ENV: /dev/null\n      GOVERNANCE_BASE_SHA: " + "a" * 40),
        ):
            mutated = ci.replace(old, new, 1)
            self.assertNotEqual(ci, mutated)
            with self.assertRaises(runtime.RuntimeEnvelopeError):
                runtime.validate_ci_trust_boundary_text(ROOT, mutated)

    def test_15b_workflow_source_grammar_rejects_yaml_and_expression_bypasses(self):
        runtime = _runtime()
        runtime._reject_reserved_provider_env_assignments(
            "env:\n  SAFE_VALUE: inert\n", "fixture"
        )
        env_bypasses = (
            "env: {ImageVersion: attacker-controlled}\n",
            '"env":\n  "ImageVersion": attacker-controlled\n',
            "? env\n:\n  ImageVersion: attacker-controlled\n",
            "!!str env:\n  ImageVersion: attacker-controlled\n",
            "? !!str env\n:\n  ImageVersion: attacker-controlled\n",
            "? !<tag:yaml.org,2002:str> env\n:\n  ImageVersion: attacker-controlled\n",
            '"\\u0065nv":\n  ImageVersion: attacker-controlled\n',
            "&envkey env:\n  ImageVersion: attacker-controlled\n",
            "*envkey:\n  ImageVersion: attacker-controlled\n",
            "env:\n  <<: *provider_defaults\n",
            "env:\n  SAFE_VALUE: &shared inert\n",
            "env:\n  SAFE_VALUE: one\n  SAFE_VALUE: two\n",
            "env:\n  RUNNER_TEMP: attacker-controlled\n",
        )
        for source in env_bypasses:
            with self.subTest(source=source):
                with self.assertRaises(runtime.RuntimeEnvelopeError):
                    runtime._reject_reserved_provider_env_assignments(source, "fixture")

        for expression in (
            "TOKEN: ${{ secrets['PROD_TOKEN'] }}",
            'TOKEN: ${{ secrets["PROD_TOKEN"] }}',
            "TOKEN: ${{ github['token'] }}",
            'TOKEN: ${{ github["token"] }}',
        ):
            with self.subTest(expression=expression):
                with self.assertRaises(runtime.RuntimeEnvelopeError):
                    runtime._reject_workflow_mutation_surface(expression, "fixture")

        stable = WORKFLOW_PATH.read_text(encoding="utf-8")
        ci = (ROOT / ".github" / "workflows" / "ci.yml").read_text(encoding="utf-8")
        workflow_mutations = (
            (
                "stable indexed secret",
                stable,
                "      PYTHONDONTWRITEBYTECODE: \"1\"",
                "      TOKEN: ${{ secrets['PROD_TOKEN'] }}",
                runtime.validate_artifact_only_workflow_text,
            ),
            (
                "stable direct inline Python step",
                stable,
                "      - name: Detect and classify upstream drift",
                "      - run: python3 -I -S -B -c 'raise SystemExit(0)'",
                runtime.validate_artifact_only_workflow_text,
            ),
            (
                "CI indexed github token",
                ci,
                "      PYTHONDONTWRITEBYTECODE: \"1\"",
                "      TOKEN: ${{ github['token'] }}",
                runtime.validate_ci_trust_boundary_text,
            ),
            (
                "CI unnamed direct step",
                ci,
                "      - name: Run validator fixture self-tests",
                "      - run: python3 -I -S -B -c 'raise SystemExit(0)'",
                runtime.validate_ci_trust_boundary_text,
            ),
        )
        for label, original, anchor, replacement, validator in workflow_mutations:
            with self.subTest(label=label):
                mutated = original.replace(anchor, replacement, 1)
                self.assertNotEqual(original, mutated)
                with self.assertRaises(runtime.RuntimeEnvelopeError):
                    validator(ROOT, mutated)

    def test_15c_contract_and_live_json_reject_duplicate_keys_at_every_depth(self):
        runtime = _runtime()
        self.assertEqual({"key": {"nested": 1}}, runtime._parse_unique_json(
            '{"key":{"nested":1}}', "fixture"
        ))
        for raw in (
            '{"schema_version":1,"schema_version":2}',
            '{"outer":{"state":"BLOCKED","state":"NO_DRIFT"}}',
            '{"value":NaN}',
            '{"value":Infinity}',
            '{"value":-Infinity}',
        ):
            with self.subTest(raw=raw):
                with self.assertRaises(runtime.RuntimeEnvelopeError):
                    runtime._parse_unique_json(raw, "fixture")

        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "duplicate.json"
            path.write_text('{"root":1,"root":2}', encoding="utf-8")
            with self.assertRaises(runtime.RuntimeEnvelopeError):
                runtime._read_json(Path(tmp), Path("duplicate.json"))

    def test_16_runner_python_and_git_outside_envelope_fail(self):
        runtime = _runtime()
        clean = runtime.controlled_git_environment({"PATH": os.environ.get("PATH", "")})
        valid = {
            "runner": "ubuntu-24.04",
            "image_os": "ubuntu24",
            "image_version": "20260823.283.1",
            "runner_arch": "X64",
            "python_implementation": "cpython",
            "python_version": (3, 12, 1),
            "bash_version": (5, 2, 21),
            "git_version": (2, 51, 1),
            "node_runtime": "node20",
            "env": clean,
        }
        runtime.verify_runtime_envelope(ROOT, **valid)
        mutations = (
            ("runner", "ubuntu-latest"),
            ("image_os", "ubuntu22"),
            ("image_version", ""),
            ("image_version", "20260823.283"),
            ("image_version", "20260823.283.1-extra"),
            ("image_version", "20261340.283.1"),
            ("runner_arch", "ARM64"),
            ("python_implementation", "pypy"),
            ("python_version", (3, 11, 9)),
            ("python_version", (3, 13, 0)),
            ("python_version", (3, 12, True)),
            ("python_version", (3, 12)),
            ("bash_version", (5, 1, 16)),
            ("bash_version", (6, 0, 0)),
            ("git_version", (2, 42, 99)),
            ("git_version", (2, 60, 0)),
            ("node_runtime", "node16"),
            ("node_runtime", "node24"),
        )
        for key, value in mutations:
            with self.assertRaises(runtime.RuntimeEnvelopeError):
                runtime.verify_runtime_envelope(ROOT, **dict(valid, **{key: value}))

    def test_16b_runner_release_payload_and_every_identity_mutation(self):
        runtime = _runtime()
        repository = {
            "id": 190416463,
            "full_name": "actions/runner-images",
            "url": "https://api.github.com/repos/actions/runner-images",
            "html_url": "https://github.com/actions/runner-images",
            "owner": {"login": "actions"},
        }
        runtime.validate_runner_images_repository_payload(repository)
        for mutate in (
            lambda item: item.__setitem__("id", 190416464),
            lambda item: item.__setitem__("id", True),
            lambda item: item.__setitem__("full_name", "attacker/runner-images"),
            lambda item: item.__setitem__("url", "https://example.invalid/repository"),
            lambda item: item.__setitem__("html_url", "https://example.invalid/repository"),
            lambda item: item["owner"].__setitem__("login", "attacker"),
        ):
            changed_repository = json.loads(json.dumps(repository))
            mutate(changed_repository)
            with self.assertRaises(runtime.RuntimeEnvelopeError):
                runtime.validate_runner_images_repository_payload(changed_repository)
        payload = {
            "tag_name": "ubuntu24/20260823.283",
            "target_commitish": "73a898e845210ee1565a4bb3328897e152dd73ae",
            "draft": False,
            "html_url": (
                "https://github.com/actions/runner-images/releases/tag/"
                "ubuntu24/20260823.283"
            ),
            "assets": [
                {
                    "name": "internal.ubuntu24.json",
                    "id": 527689532,
                    "size": 30505,
                    "digest": (
                        "sha256:68c57165414e6868ea1b042b920640435daacf12"
                        "eaa3bbdcaa85abbc4caac214"
                    ),
                    "browser_download_url": (
                        "https://github.com/actions/runner-images/releases/download/"
                        "ubuntu24/20260823.283/internal.ubuntu24.json"
                    ),
                }
            ],
        }
        evidence = runtime.validate_runner_image_release_payload(
            "20260823.283.1", payload
        )
        self.assertEqual("ubuntu24/20260823.283", evidence["release_tag"])
        self.assertEqual(
            "UNAVAILABLE_OPTIONAL_HARDENING", evidence["sbom_availability"]
        )
        self.assertEqual(
            "https://raw.githubusercontent.com/actions/runner-images/"
            "73a898e845210ee1565a4bb3328897e152dd73ae/"
            "images/ubuntu/Ubuntu2404-Readme.md",
            evidence["included_software_url"],
        )
        encoded = json.loads(json.dumps(payload))
        encoded["html_url"] = encoded["html_url"].replace(
            "ubuntu24/20260823.283", "ubuntu24%2F20260823.283"
        )
        encoded["assets"][0]["browser_download_url"] = encoded["assets"][0][
            "browser_download_url"
        ].replace("ubuntu24/20260823.283", "ubuntu24%2F20260823.283")
        runtime.validate_runner_image_release_payload("20260823.283.1", encoded)

        def changed(mutator):
            document = json.loads(json.dumps(payload))
            mutator(document)
            with self.assertRaises(runtime.RuntimeEnvelopeError):
                runtime.validate_runner_image_release_payload(
                    "20260823.283.1", document
                )

        mutations = (
            lambda item: item.__setitem__("tag_name", "ubuntu24/latest"),
            lambda item: item.__setitem__("target_commitish", "main"),
            lambda item: item.__setitem__("draft", True),
            lambda item: item.__setitem__("html_url", "https://example.invalid/release"),
            lambda item: item.__setitem__("assets", []),
            lambda item: item["assets"].append(dict(item["assets"][0])),
            lambda item: item["assets"][0].__setitem__("id", 0),
            lambda item: item["assets"][0].__setitem__("size", 0),
            lambda item: item["assets"][0].__setitem__("digest", "sha256:" + "0" * 63),
            lambda item: item["assets"][0].__setitem__(
                "browser_download_url", "https://example.invalid/internal.ubuntu24.json"
            ),
            lambda item: item["assets"][0].__setitem__(
                "browser_download_url",
                "https://github.com/actions/runner-images/releases/download/"
                "ubuntu24/20260822.1/internal.ubuntu24.json",
            ),
        )
        for mutator in mutations:
            changed(mutator)
        for image_version in ("", "latest", "20260823.283", "20260823/283/1"):
            with self.assertRaises(runtime.RuntimeEnvelopeError):
                runtime.validate_runner_image_release_payload(image_version, payload)

    def test_17_ambient_git_config_hook_protocol_or_helper_fails(self):
        runtime = _runtime()
        for key, value in (
            ("GIT_CONFIG_COUNT", "1"),
            ("GIT_CONFIG_KEY_0", "credential.helper"),
            ("GIT_CONFIG_VALUE_0", "store"),
            ("GIT_SSH_COMMAND", "ssh -o ProxyCommand=evil"),
            ("GIT_ALLOW_PROTOCOL", "file:https"),
        ):
            env = runtime.controlled_git_environment({"PATH": os.environ.get("PATH", "")})
            env[key] = value
            with self.assertRaises(runtime.RuntimeEnvelopeError, msg=key):
                runtime.verify_runtime_envelope(
                    ROOT,
                    runner="ubuntu-24.04",
                    image_os="ubuntu24",
                    image_version="20260823.283.1",
                    runner_arch="X64",
                    python_implementation="cpython",
                    python_version=(3, 12, 1),
                    bash_version=(5, 2, 21),
                    git_version=(2, 51, 1),
                    node_runtime="node20",
                    env=env,
                )

    def test_18_unexpected_rest_or_git_fallback_fails(self):
        runtime = _runtime()
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            runtime.assert_transport(ROOT, "rest-checkout")
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            runtime.assert_transport(ROOT, "ssh")
        runtime.assert_transport(ROOT, "git-https")

        head = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            cwd=ROOT,
            check=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        ).stdout.strip()
        checkout_env = {
            "PATH": os.environ.get("PATH", ""),
            "GITHUB_SHA": head,
        }
        runtime.verify_checkout_worktree_identity(ROOT, checkout_env)
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            runtime.verify_checkout_worktree_identity(
                ROOT, dict(checkout_env, GITHUB_SHA="0" * 40)
            )
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(runtime.RuntimeEnvelopeError):
                runtime.verify_checkout_worktree_identity(tmp, checkout_env)


if __name__ == "__main__":
    unittest.main()
