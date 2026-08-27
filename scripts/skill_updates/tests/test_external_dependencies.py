"""Fail-closed tests for the G1.1 external/runtime control envelope."""

import json
import hashlib
import os
from pathlib import Path
import re
import shutil
import tempfile
import unittest

from .. import runtime


ROOT = Path(__file__).resolve().parents[3]
LEDGER = ROOT / "docs/agents/skills-maintenance/external-dependencies.json"
QUARANTINE = ROOT / "docs/agents/skills-maintenance/legacy-quarantine.json"
STABLE_WORKFLOW = ROOT / ".github/workflows/stable-skills-maintenance.yml"
CI_WORKFLOW = ROOT / ".github/workflows/ci.yml"


def _json(path):
    with path.open(encoding="utf-8") as handle:
        return json.load(handle)


class ExternalDependencyClosure(unittest.TestCase):
    def test_committed_ledger_and_notice_are_complete(self):
        ledger = _json(LEDGER)
        runtime.validate_external_dependencies(ROOT, ledger)
        checkout = ledger["dependencies"][0]
        self.assertEqual(
            ["detect", "prepare", "governance"],
            [consumer["job"] for consumer in checkout["consumers"]],
        )
        self.assertEqual(
            "4815ff3a74ae4f01971ccb58454ce373292dbac3e21394d23b48151df2feade7",
            checkout["tree_manifest"]["digest"],
        )
        self.assertFalse(checkout["tree_manifest"]["source"]["truncated"])
        self.assertEqual(117, len(checkout["tree_manifest"]["manifest"]["members"]))
        self.assertEqual(
            "678275cbeb52fecbacc620aed081473053f86b787f1b469e157016376a26cc70",
            checkout["package_lock"]["sha256"],
        )
        self.assertEqual(
            {
                ".github/workflows/stable-skills-maintenance.yml#detect",
                ".github/workflows/stable-skills-maintenance.yml#prepare",
                ".github/workflows/ci.yml#governance",
            },
            set(checkout["inputs"]),
        )
        self.assertEqual(
            [1, 1, 0],
            [
                checkout["inputs"][workflow + "#" + job]["fetch-depth"]
                for workflow, job in (
                    (".github/workflows/stable-skills-maintenance.yml", "detect"),
                    (".github/workflows/stable-skills-maintenance.yml", "prepare"),
                    (".github/workflows/ci.yml", "governance"),
                )
            ],
        )

    def test_runtime_records_are_exact_closed_and_consumer_complete(self):
        ledger = _json(LEDGER)
        records = ledger["runtime"]["records"]
        self.assertEqual(
            [
                "github-hosted-ubuntu24-x64",
                "cpython-3.12",
                "git",
                "gnu-bash",
                "checkout-node20",
            ],
            [record["id"] for record in records],
        )
        common = {
            "kind", "id", "owner", "consumers", "source", "license",
            "transitive_closure", "permissions", "network", "data_retention",
            "applicability", "drift_detector", "rollback",
        }
        self.assertTrue(all(set(record) == common for record in records))
        checkout_consumers = ledger["dependencies"][0]["consumers"]
        node_consumers = [
            {"workflow": item["workflow"], "job": item["job"]}
            for item in records[4]["consumers"]
        ]
        self.assertEqual(checkout_consumers, node_consumers)
        self.assertEqual(
            "provider-attested-rolling", records[0]["source"]["identity_mode"]
        )
        self.assertEqual(
            "OPTIONAL_HARDENING_UNAVAILABLE_NOT_REQUIRED",
            records[0]["source"]["required_evidence"]["sbom_disposition"],
        )
        self.assertEqual("PSF-2.0", records[1]["license"]["primary_spdx"])
        self.assertEqual("GPL-2.0-only", records[2]["license"]["spdx"])
        self.assertIn(
            "<runtime_control> ls-remote --symref --exit-code -- "
            "<repository-https-url> HEAD refs/heads/<stable-ref>",
            records[2]["transitive_closure"]["commands"],
        )
        self.assertIn(
            "protocol.file.allow=never",
            records[2]["transitive_closure"]["argv_prefixes"]["updater_untrusted"],
        )
        self.assertEqual("GPL-3.0-or-later", records[3]["license"]["spdx"])
        self.assertEqual("node20", records[4]["source"]["runtime_family"])

    def test_schema_consts_match_the_committed_runtime_and_transport(self):
        ledger = _json(LEDGER)
        schema = _json(
            ROOT / "docs/agents/skills-maintenance/schemas/external-dependencies.schema.json"
        )
        self.assertEqual(ledger["runtime"], schema["$defs"]["runtime"]["const"])
        self.assertEqual(ledger["transport"], schema["$defs"]["transport"]["const"])
        self.assertEqual(3, schema["properties"]["dependencies"]["minItems"])
        self.assertEqual(3, schema["properties"]["dependencies"]["maxItems"])
        self.assertFalse(schema["$defs"]["managedService"]["additionalProperties"])

    def test_managed_service_records_close_every_r0_field(self):
        services = _json(LEDGER)["dependencies"][1:]
        self.assertEqual(
            ["github-rest-quarantine-read", "github-rest-runner-image-release-read"],
            [item["id"] for item in services],
        )
        expected_keys = {
            "kind", "id", "owner", "consumers", "source", "license",
            "transitive_closure", "permissions", "network", "data_retention",
            "applicability", "drift_detector", "rollback",
        }
        for item in services:
            self.assertEqual(expected_keys, set(item))
            self.assertEqual("managed-service", item["kind"])
            self.assertEqual("repository-owner", item["owner"]["governance"])
            self.assertEqual("N/A", item["license"]["spdx"])
            self.assertIn("MANAGED_SERVICE", item["license"]["disposition"])
            self.assertEqual("NONE", item["permissions"]["authentication"])
            self.assertFalse(item["permissions"]["github_token"])
            self.assertEqual("REQUIRED", item["network"]["tls_verification"])
            self.assertEqual("REJECT", item["network"]["redirects"])
            self.assertEqual("repository-owner", item["rollback"]["owner"])
            self.assertFalse(item["rollback"]["automatic_fallback"])

    def test_every_runtime_record_surface_and_release_transport_fail_closed(self):
        mutations = (
            lambda item: item["runtime"].__setitem__("identity_policy", "DIGEST_PINNED"),
            lambda item: item["runtime"]["records"].reverse(),
            lambda item: item["runtime"]["records"][0].__setitem__("extra", True),
            lambda item: item["runtime"]["records"][0].pop("owner"),
            lambda item: item["runtime"]["records"][0]["owner"].__setitem__(
                "governance", "provider"
            ),
            lambda item: item["runtime"]["records"][0]["consumers"].pop(),
            lambda item: item["runtime"]["records"][0]["source"].__setitem__(
                "identity_mode", "digest-pinned"
            ),
            lambda item: item["runtime"]["records"][0]["license"].__setitem__(
                "source_repository_spdx", "UNKNOWN"
            ),
            lambda item: item["runtime"]["records"][0]["transitive_closure"].__setitem__(
                "provider_manifest_asset_required", False
            ),
            lambda item: item["runtime"]["records"][0]["permissions"].__setitem__(
                "secrets", True
            ),
            lambda item: item["runtime"]["records"][0]["network"]["allowed_hosts"].append(
                "example.invalid"
            ),
            lambda item: item["runtime"]["records"][0]["data_retention"].__setitem__(
                "uploaded_artifacts", True
            ),
            lambda item: item["runtime"]["records"][0]["applicability"].__setitem__(
                "runner_label", "ubuntu-latest"
            ),
            lambda item: item["runtime"]["records"][0]["drift_detector"].__setitem__(
                "on_missing_or_mismatch", "CONTINUE"
            ),
            lambda item: item["runtime"]["records"][0]["rollback"].__setitem__(
                "automatic_fallback", True
            ),
            lambda item: item["runtime"]["records"][4]["source"].__setitem__(
                "runtime_family", "node24"
            ),
            lambda item: item["transport"]["allowed"].pop(),
            lambda item: item["transport"]["runner_image_release_read"].__setitem__(
                "endpoint", "https://example.invalid"
            ),
            lambda item: item["transport"]["runner_image_release_read"].__setitem__(
                "redirects", "FOLLOW"
            ),
            lambda item: item["transport"]["runner_image_release_read"][
                "response_constraints"
            ].__setitem__("asset_digest_pattern", ".*"),
            lambda item: item["dependencies"][1]["owner"].__setitem__(
                "governance", "provider"
            ),
            lambda item: item["dependencies"][1]["license"].__setitem__(
                "spdx", "MIT"
            ),
            lambda item: item["dependencies"][1]["permissions"].__setitem__(
                "authentication", "TOKEN"
            ),
            lambda item: item["dependencies"][1]["network"].__setitem__(
                "redirects", "FOLLOW"
            ),
            lambda item: item["dependencies"][1]["rollback"].__setitem__(
                "automatic_fallback", True
            ),
            lambda item: item["dependencies"][1]["data_retention"].__setitem__(
                "response_bytes", "PERSISTENT"
            ),
            lambda item: item["dependencies"][2]["consumers"].pop(),
            lambda item: item["dependencies"][2]["transitive_closure"].__setitem__(
                "provider_manifest_download", True
            ),
        )
        for mutate in mutations:
            corrupt = _json(LEDGER)
            mutate(corrupt)
            with self.assertRaises(runtime.RuntimeEnvelopeError):
                runtime._validate_dependencies_document(corrupt)

    def test_workflow_precheckout_bootstraps_are_identical_and_match_consumers(self):
        stable = STABLE_WORKFLOW.read_text(encoding="utf-8")
        ci = CI_WORKFLOW.read_text(encoding="utf-8")
        pin = "actions/checkout@" + runtime.CHECKOUT_COMMIT
        bodies = []
        actual_consumers = []
        for path, text in ((runtime.WORKFLOW_PATH, stable), (".github/workflows/ci.yml", ci)):
            job = None
            preflight_by_job = {}
            for line_number, line in enumerate(text.splitlines()):
                match = re.match(r"^  ([A-Za-z_][A-Za-z0-9_-]*):\s*$", line)
                if match:
                    job = match.group(1)
                if "name: Prove the pinned runner Python and Git envelope" in line:
                    preflight_by_job[job] = line_number
                if "uses: " + pin in line:
                    self.assertIn(job, preflight_by_job)
                    self.assertLess(preflight_by_job[job], line_number)
                    actual_consumers.append({"workflow": path, "job": job})
            bodies.extend(
                re.findall(
                    r"(?ms)^\s{10}python3 -I -S -B <<'PY'\n(.*?)^\s{10}PY\s*$", text
                )
            )
        self.assertEqual(3, len(bodies))
        self.assertEqual([bodies[0], bodies[0], bodies[0]], bodies)
        self.assertTrue(all(
            hashlib.sha256(body.encode("utf-8")).hexdigest()
            == runtime.PRECHECKOUT_BOOTSTRAP_SHA256
            for body in bodies
        ))
        self.assertEqual(_json(LEDGER)["dependencies"][0]["consumers"], actual_consumers)
        for body in bodies:
            for anchor in (
                "urllib.request.ProxyHandler({})",
                "ssl.create_default_context",
                "RefuseRedirects",
                'repository.get("id") != 190416463',
                'repository.get("full_name") != "actions/runner-images"',
                "target_commitish",
                "internal.ubuntu24.json",
                "sha256:[0-9a-f]{64}",
                'f"{source_sha}/images/ubuntu/Ubuntu2404-Readme.md"',
                "UNAVAILABLE_OPTIONAL_HARDENING",
                "unexpected_git_env = sorted(",
            ):
                self.assertIn(anchor, body)
            for key in sorted(
                runtime._AUTH_ENV_KEYS
                | runtime._PROXY_ENV_KEYS
                | runtime._TRUST_OVERRIDE_ENV_KEYS
                | runtime._EXECUTION_OVERRIDE_ENV_KEYS
            ):
                self.assertIn('"' + key + '"', body)
            self.assertLess(body.index("present_auth ="), body.index("opener.open("))
            self.assertLess(body.index("present_proxy ="), body.index("opener.open("))
            self.assertLess(body.index("present_trust ="), body.index("opener.open("))
            self.assertLess(body.index("present_execution ="), body.index("opener.open("))
            self.assertLess(body.index("unexpected_git_env ="), body.index("opener.open("))
            self.assertNotIn("Authorization", body)
            self.assertNotIn("os.environ.items", body)

        for label, original, count in (
            ("stable", stable, 2),
            ("CI", ci, 1),
        ):
            for old, new in (
                ("if unexpected_git_env:", "if False and unexpected_git_env:"),
                ('"GIT_CONFIG_PARAMETERS",', '"IGNORED_GIT_CONFIG_PARAMETERS",'),
                ("ProxyHandler({})", "ProxyHandler()"),
            ):
                with self.subTest(workflow=label, mutation=old):
                    mutated = original.replace(old, new, 1)
                    self.assertNotEqual(original, mutated)
                    with self.assertRaises(runtime.RuntimeEnvelopeError):
                        runtime._validate_precheckout_bootstrap_bodies(
                            mutated, expected_count=count, where=label
                        )

    def test_notice_states_truthful_bootstrap_and_runtime_licences(self):
        notice = (
            ROOT / "docs/agents/skills-maintenance/THIRD_PARTY_NOTICES.md"
        ).read_text(encoding="utf-8")
        for token in (
            "bootstrap trust boundary",
            "after checkout",
            "before updater or upstream",
            "does not claim that repository code can",
            "provider-attested rolling `ImageVersion`, not a digest-pinned image",
            "OPTIONAL_HARDENING_UNAVAILABLE_NOT_REQUIRED",
            "PSF-2.0",
            "GPL-2.0-only",
            "GPL-3.0-or-later",
            "Node 20",
            runtime.PRECHECKOUT_BOOTSTRAP_SHA256,
            "python3 -I -S -B",
            "real Git worktree",
            "GIT_CONFIG_PARAMETERS",
        ):
            self.assertIn(token, notice)

    def test_ledger_corruption_fails_before_invocation(self):
        for mutate in (
            lambda item: item["dist"].__setitem__("sha256", "0" * 64),
            lambda item: item["tree_manifest"].__setitem__("digest", "0" * 64),
            lambda item: item["tree_manifest"]["manifest"]["members"][0].__setitem__(
                "mode", "100755"
            ),
            lambda item: item["tree_manifest"]["source"].__setitem__("truncated", True),
            lambda item: item["package_lock"].__setitem__("sha256", "0" * 64),
            lambda item: item["inputs"][
                ".github/workflows/ci.yml#governance"
            ].__setitem__("persist-credentials", True),
            lambda item: item["inputs"][
                ".github/workflows/ci.yml#governance"
            ].__setitem__("fetch-depth", 1),
            lambda item: item["consumers"].pop(),
            lambda item: item.__setitem__("rollback_owner", ""),
        ):
            corrupt = _json(LEDGER)
            mutate(corrupt["dependencies"][0])
            with self.assertRaises(runtime.RuntimeEnvelopeError):
                runtime.validate_external_dependencies(ROOT, corrupt)

    def test_only_declared_read_only_transports_are_accepted(self):
        self.assertEqual("git-https", runtime.assert_transport(ROOT, "git-https"))
        self.assertEqual(
            "github-rest-quarantine-read",
            runtime.assert_transport(ROOT, "github-rest-quarantine-read"),
        )
        self.assertEqual(
            "github-rest-runner-image-release-read",
            runtime.assert_transport(ROOT, "github-rest-runner-image-release-read"),
        )
        for bad in ("rest-checkout", "ssh", "file", "github-rest-write", ""):
            with self.assertRaises(runtime.RuntimeEnvelopeError, msg=bad):
                runtime.assert_transport(ROOT, bad)


class RuntimeAndLiveControl(unittest.TestCase):
    def setUp(self):
        self.precheckout = dict(runtime._CONTROLLED_PRECHECKOUT_GIT_ENV)
        self.clean = runtime.controlled_git_environment(
            {"PATH": os.environ.get("PATH", "")}
        )
        self.runtime = {
            "runner": "ubuntu-24.04",
            "image_os": "ubuntu24",
            "image_version": "20260823.283.1",
            "runner_arch": "X64",
            "python_implementation": "cpython",
            "python_version": (3, 12, 0),
            "bash_version": (5, 2, 21),
            "git_version": (2, 43, 0),
            "node_runtime": "node20",
            "env": self.clean,
        }

    def test_runtime_bounds_and_ambient_mutation_fail_closed(self):
        runtime.verify_runtime_envelope(ROOT, **self.runtime)
        for git_version in ((2, 42, 99), (2, 60, 0)):
            with self.assertRaises(runtime.RuntimeEnvelopeError):
                runtime.verify_runtime_envelope(
                    ROOT, **dict(self.runtime, git_version=git_version)
                )
        hostile = dict(self.clean)
        hostile["GIT_ALLOW_PROTOCOL"] = "https:file"
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            runtime.verify_runtime_envelope(
                ROOT, **dict(self.runtime, git_version=(2, 51, 1), env=hostile)
            )

    def test_live_hosted_runtime_requires_exact_image_and_bash_environment(self):
        hosted = {
            **self.precheckout,
            "GITHUB_ACTIONS": "true",
            "ImageOS": "ubuntu24",
            "ImageVersion": "20260823.283.1",
            "RUNNER_ARCH": "X64",
            "BASH_ENV": os.devnull,
            "PATH": os.environ.get("PATH", ""),
        }
        runtime.verify_hosted_runtime_environment(ROOT, hosted)
        for key, value in (
            ("GITHUB_ACTIONS", "false"),
            ("ImageOS", "ubuntu22"),
            ("ImageVersion", ""),
            ("RUNNER_ARCH", "ARM64"),
            ("BASH_ENV", "/tmp/untrusted-bash-env"),
            ("NODE_OPTIONS", "--require=/tmp/untrusted.js"),
            ("GIT_CONFIG_COUNT", "1"),
            ("GIT_CONFIG_PARAMETERS", "'credential.helper=store'"),
            ("GIT_SSL_NO_VERIFY", "1"),
            ("GIT_TRACE", "1"),
            ("GIT_ALLOW_PROTOCOL", "file:https"),
        ):
            with self.assertRaises(runtime.RuntimeEnvelopeError, msg=key):
                runtime.verify_hosted_runtime_environment(
                    ROOT, dict(hosted, **{key: value})
                )

    def test_live_identity_accepts_only_exact_git_and_injected_snapshot(self):
        sha = "a" * 40
        environ = {
            **self.precheckout,
            "GITHUB_REPOSITORY_ID": "1297795646",
            "GITHUB_REPOSITORY": "KraineOpasen/bukerov-twitch-miner-go",
            "GITHUB_REF_NAME": "release/0.3",
            "GITHUB_SHA": sha,
            "GITHUB_WORKFLOW_REF": (
                "KraineOpasen/bukerov-twitch-miner-go/.github/workflows/"
                "stable-skills-maintenance.yml@refs/heads/release/0.3"
            ),
        }
        output = "ref: refs/heads/release/0.3\tHEAD\n%s\tHEAD\n%s\trefs/heads/release/0.3\n" % (
            sha,
            sha,
        )
        calls = []

        def ls_remote(url, branch):
            calls.append((url, branch))
            return output

        subjects = _json(QUARANTINE)["subjects"]
        verified = runtime.verify_workflow_environment(
            ROOT,
            environ=environ,
            ls_remote=ls_remote,
            quarantine_snapshot=subjects,
        )
        self.assertEqual(sha, verified.fetched_default_sha)
        self.assertEqual(
            [("https://github.com/KraineOpasen/bukerov-twitch-miner-go", "release/0.3")],
            calls,
        )

        wrong = json.loads(json.dumps(subjects))
        wrong[-1]["state"] = "OPEN"
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            runtime.verify_workflow_environment(
                ROOT,
                environ=environ,
                ls_remote=ls_remote,
                quarantine_snapshot=wrong,
            )

    def test_workflow_identity_rejects_ambient_credentials_and_fallback(self):
        environ = {
            **self.precheckout,
            "GITHUB_REPOSITORY_ID": "1297795646",
            "GITHUB_REPOSITORY": "KraineOpasen/bukerov-twitch-miner-go",
            "GITHUB_REF_NAME": "release/0.3",
            "GITHUB_SHA": "a" * 40,
            "GITHUB_WORKFLOW_REF": runtime.EXPECTED_WORKFLOW_REF,
            "GITHUB_TOKEN": "must-not-be-read",
        }
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            runtime.verify_workflow_environment(
                ROOT,
                environ=environ,
                ls_remote=lambda *_: self.fail("network must not run with ambient auth"),
                quarantine_snapshot=_json(QUARANTINE)["subjects"],
            )

    def test_candidate_full_identity_binds_every_external_control(self):
        contract = runtime.load_contract(ROOT)
        identity = runtime.candidate_identity(
            provider="awesome-copilot",
            stable_branch=contract.stable_branch,
            stable_base_sha="a" * 40,
            target_sha="b" * 40,
            upstream_repo="https://github.com/github/awesome-copilot",
            old_pin="c" * 40,
            control_input_digest=contract.control_input_digest,
            updater_source_sha=contract.updater_source_sha,
            pinned_action_digests=contract.pinned_action_digests,
        )
        doc = identity.to_dict()
        payload = {
            key: doc[key]
            for key in (
                "schema", "repo_id", "repo_full_name", "stable_branch",
                "stable_base_sha", "provider", "upstream_repo", "old_pin",
                "target_sha", "control_input_digest", "updater_source_sha",
                "pinned_action_digests",
            )
        }
        expected = hashlib.sha256(
            json.dumps(
                payload,
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
            ).encode("utf-8")
        ).hexdigest()
        self.assertEqual(expected, identity.proposal_id)
        self.assertEqual(
            set(payload) | {"proposal_id", "locator"}, set(doc)
        )
        self.assertNotIn("subject_kind", doc)
        self.assertNotIn("policy_digest", doc)
        self.assertNotIn("repository_id", doc)
        self.assertEqual(contract.control_input_digest, doc["control_input_digest"])
        self.assertEqual(contract.updater_source_sha, doc["updater_source_sha"])
        self.assertEqual(
            {
                "actions/checkout/action.yml@11d5960a326750d5838078e36cf38b85af677262":
                "4815ff3a74ae4f01971ccb58454ce373292dbac3e21394d23b48151df2feade7"
            },
            doc["pinned_action_digests"],
        )

    def test_every_control_member_binds_path_mode_and_raw_bytes(self):
        members = runtime.CONTROL_INPUT_MEMBERS
        self.assertEqual(
            list(members), sorted(members, key=lambda value: value.encode("utf-8"))
        )
        with tempfile.TemporaryDirectory() as tmp:
            replica = Path(tmp)
            for rel in members:
                target = replica / rel
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(ROOT / rel, target)
                target.chmod(0o644)
            baseline = runtime.compute_control_input_digest(replica, members)
            self.assertRegex(baseline, r"^[0-9a-f]{64}$")
            for rel in members:
                with self.subTest(member=rel, mutation="bytes"):
                    target = replica / rel
                    original = target.read_bytes()
                    target.write_bytes(original + b"\nG1.1-mutation")
                    target.chmod(0o644)
                    self.assertNotEqual(
                        baseline, runtime.compute_control_input_digest(replica, members)
                    )
                    target.write_bytes(original)
                    target.chmod(0o644)
                with self.subTest(member=rel, mutation="mode"):
                    target.chmod(0o600)
                    with self.assertRaises(runtime.RuntimeEnvelopeError):
                        runtime.compute_control_input_digest(replica, members)
                    target.chmod(0o644)
                with self.subTest(member=rel, mutation="path"):
                    moved = target.with_name(target.name + ".moved")
                    target.rename(moved)
                    with self.assertRaises(runtime.RuntimeEnvelopeError):
                        runtime.compute_control_input_digest(replica, members)
                    moved.rename(target)
                with self.subTest(member=rel, mutation="symlink"):
                    original = target.read_bytes()
                    target.unlink()
                    target.symlink_to(ROOT / rel)
                    with self.assertRaises(runtime.RuntimeEnvelopeError):
                        runtime.compute_control_input_digest(replica, members)
                    target.unlink()
                    target.write_bytes(original)
                    target.chmod(0o644)

    def test_liveness_qualifier_binds_head_digest_job_and_time(self):
        contract = runtime.load_contract(ROOT)
        workflows = [
            {
                "path": ".github/workflows/ci.yml",
                "workflow_id": 122,
                "trusted_file_blob_sha": "b" * 40,
                "state": "active",
            },
            {
                "path": runtime.WORKFLOW_PATH,
                "workflow_id": 123,
                "trusted_file_blob_sha": "c" * 40,
                "state": "active",
            },
        ]
        facts = {
            "current_live_default_sha": "a" * 40,
            "expected_closed_workflows": workflows,
            "now_epoch": 200000,
        }
        closed = runtime.closed_workflow_set_identity(workflows)
        aggregate = runtime.aggregate_control_plane_identity(ROOT, closed)
        evidence = {
            "event": "schedule",
            "status": "completed",
            "conclusion": "success",
            "workflow": closed["workflows"][1],
            "head_sha": facts["current_live_default_sha"],
            "control_input_digest": contract.control_input_digest,
            "aggregate_control_plane_identity": aggregate,
            "required_jobs": {"Reconcile complete G1.1 control plane": "success"},
            "closed_workflow_set": closed,
            "scheduled_epoch": 100000,
            "completed_epoch": 100100,
        }
        runtime.validate_qualifying_run_evidence(ROOT, evidence, **facts)
        mutations = (
            ("workflow-id", lambda item: item["workflow"].__setitem__("workflow_id", 999)),
            ("workflow-path", lambda item: item["workflow"].__setitem__(
                "path", ".github/workflows/not-the-orchestrator.yml")),
            ("workflow-blob", lambda item: item["workflow"].__setitem__(
                "trusted_file_blob_sha", "d" * 40)),
            ("head", lambda item: item.__setitem__("head_sha", "d" * 40)),
            ("digest", lambda item: item.__setitem__("control_input_digest", "d" * 64)),
            ("job", lambda item: item["required_jobs"].__setitem__(
                "Reconcile complete G1.1 control plane", "failure")),
            ("updater", lambda item: item["aggregate_control_plane_identity"].__setitem__(
                "updater_source_sha", "d" * 64)),
            ("action", lambda item: item["aggregate_control_plane_identity"][
                "pinned_action_digests"
            ].__setitem__(next(iter(contract.pinned_action_digests)), "d" * 64)),
            ("closed-digest", lambda item: item["closed_workflow_set"].__setitem__(
                "digest", "d" * 64)),
            ("aggregate-digest", lambda item: item[
                "aggregate_control_plane_identity"
            ].__setitem__("digest", "d" * 64)),
            ("time", lambda item: item.__setitem__("scheduled_epoch", 1)),
        )
        for label, mutate in mutations:
            corrupt = json.loads(json.dumps(evidence))
            mutate(corrupt)
            with self.assertRaises(runtime.RuntimeEnvelopeError, msg=label):
                runtime.validate_qualifying_run_evidence(ROOT, corrupt, **facts)

        for index in range(2):
            with self.subTest(workflow=index, mutation="missing"):
                missing = json.loads(json.dumps(workflows))
                missing.pop(index)
                with self.assertRaises(runtime.RuntimeEnvelopeError):
                    runtime.validate_qualifying_run_evidence(
                        ROOT,
                        evidence,
                        **dict(facts, expected_closed_workflows=missing),
                    )
            with self.subTest(workflow=index, mutation="disabled"):
                disabled = json.loads(json.dumps(workflows))
                disabled[index]["state"] = "disabled_inactivity"
                with self.assertRaises(runtime.RuntimeEnvelopeError):
                    runtime.validate_qualifying_run_evidence(
                        ROOT,
                        evidence,
                        **dict(facts, expected_closed_workflows=disabled),
                    )
        ancestor = json.loads(json.dumps(evidence))
        ancestor["head_sha"] = "d" * 40
        runtime.validate_qualifying_run_evidence(
            ROOT,
            ancestor,
            **facts,
            is_ancestor=lambda old, current: (
                old == "d" * 40 and current == facts["current_live_default_sha"]
            ),
        )
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            runtime.validate_qualifying_run_evidence(
                ROOT, ancestor, **facts, is_ancestor=lambda *_: False
            )
        with self.assertRaisesRegex(runtime.RuntimeEnvelopeError, "UNCOMMISSIONED"):
            runtime.verify_liveness_evidence(
                ROOT,
                ancestor,
                **facts,
                is_ancestor=lambda old, current: (
                    old == "d" * 40 and current == facts["current_live_default_sha"]
                ),
            )
        with self.assertRaisesRegex(runtime.RuntimeEnvelopeError, "incomplete"):
            runtime.verify_liveness_evidence(
                ROOT, evidence, **facts, unreviewed_live_fact=True
            )
        with self.assertRaises(runtime.RuntimeEnvelopeError):
            runtime.verify_liveness_evidence(ROOT, evidence, **facts)

    def test_workflow_gate_rejects_tls_and_proxy_trust_overrides(self):
        base = {
            **self.precheckout,
            "GITHUB_REPOSITORY_ID": "1297795646",
            "GITHUB_REPOSITORY": "KraineOpasen/bukerov-twitch-miner-go",
            "GITHUB_REF_NAME": "release/0.3",
            "GITHUB_SHA": "a" * 40,
            "GITHUB_WORKFLOW_REF": runtime.EXPECTED_WORKFLOW_REF,
        }
        for key in sorted(runtime._PROXY_ENV_KEYS | runtime._TRUST_OVERRIDE_ENV_KEYS):
            hostile = dict(base)
            hostile[key] = ""
            with self.assertRaises(runtime.RuntimeEnvelopeError, msg=key):
                runtime.verify_workflow_environment(
                    ROOT,
                    environ=hostile,
                    ls_remote=lambda *_: self.fail("network reached with trust override"),
                    quarantine_snapshot=_json(QUARANTINE)["subjects"],
                )
        for key in sorted(runtime._EXECUTION_OVERRIDE_ENV_KEYS):
            hostile = dict(base)
            hostile[key] = "untrusted"
            with self.assertRaises(runtime.RuntimeEnvelopeError, msg=key):
                runtime.verify_workflow_environment(
                    ROOT,
                    environ=hostile,
                    ls_remote=lambda *_: self.fail("network reached with execution override"),
                    quarantine_snapshot=_json(QUARANTINE)["subjects"],
                )


if __name__ == "__main__":
    unittest.main()
