"""Runner behavior tests; all oc commands mocked, no cluster contacted."""

import importlib.util
import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

spec = importlib.util.spec_from_file_location(
    "cluster_acceptance", Path(__file__).with_name("cluster-acceptance.py")
)
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


def args(**changes):
    values = {
        "context": "test-context",
        "namespace": "pulse-test",
        "pulse": "pulse",
        "allow_mutations": False,
        "rotate_token": False,
        "exercise_temporal_toggle": False,
        "network_probe_image": None,
        "timeout": 10,
        "poll": 1,
        "observe_seconds": 65,
    }
    values.update(changes)
    return SimpleNamespace(**values)


def owned(**fields):
    return dict(
        metadata={
            "uid": "resource",
            "ownerReferences": [{"uid": "cr-uid", "controller": True}],
        },
        **fields,
    )


class RunnerTests(unittest.TestCase):
    def runner(self, **changes):
        runner = m.Runner(args(**changes), sleep=Mock())
        runner.cr = {"metadata": {"uid": "cr-uid"}, "spec": {}}
        return runner

    def test_every_command_pins_context_namespace_and_bounded_request(self):
        command = Mock(return_value=SimpleNamespace(returncode=0, stdout="{}"))
        runner = m.Runner(args(), command=command)
        runner.get("secret", "pulse-nginx")
        self.assertEqual(
            command.call_args.args[0][:7],
            [
                "oc",
                "--context",
                "test-context",
                "--namespace",
                "pulse-test",
                "--request-timeout=30s",
                "get",
            ],
        )
        self.assertEqual(command.call_args.kwargs["timeout"], 45)

    def test_command_failure_does_not_leak_secret_response(self):
        command = Mock(
            return_value=SimpleNamespace(
                returncode=1, stdout="private-token", stderr="secret-password"
            )
        )
        with self.assertRaises(m.AcceptanceError) as error:
            m.Runner(args(), command=command).get("secret", "pulse-ws-token")
        self.assertNotIn("private-token", str(error.exception))
        self.assertNotIn("secret-password", str(error.exception))

    def test_mutations_require_both_opt_in_and_disposable_label(self):
        for allow, label in [(False, "disposable"), (True, None)]:
            runner = self.runner(rotate_token=True, allow_mutations=allow)
            runner.get = Mock(
                side_effect=[
                    {"metadata": {"labels": {"pulse.ai/acceptance": label}}},
                    {"metadata": {"namespace": "pulse-test", "uid": "cr-uid"}},
                ]
            )
            with self.assertRaises(m.AcceptanceError):
                runner.prerequisites()

    def test_namespace_mismatch_fails(self):
        runner = self.runner()
        runner.get = Mock(
            side_effect=[{"metadata": {}}, {"metadata": {"namespace": "other"}}]
        )
        with self.assertRaises(m.AcceptanceError):
            runner.prerequisites()

    def test_unscoped_policy_or_other_port_fails(self):
        runner = self.runner()
        for peers, port in [
            ([{}], 5432),
            (
                [
                    {
                        "podSelector": {
                            "matchLabels": {"app": "pulse-openshift-sre-agent"}
                        }
                    }
                ],
                443,
            ),
        ]:
            runner.get = Mock(
                return_value=owned(
                    spec={
                        "podSelector": {
                            "matchLabels": {
                                "app": "pulse-openshift-sre-agent-postgresql"
                            }
                        },
                        "policyTypes": ["Ingress"],
                        "ingress": [
                            {
                                "from": peers,
                                "ports": [{"protocol": "TCP", "port": port}],
                            }
                        ],
                    }
                )
            )
            with self.assertRaises(m.AcceptanceError):
                runner.policy(False)

    def test_temporal_enabled_disabled_policy_matrix(self):
        runner = self.runner()
        for enabled in [False, True]:
            peers = [
                {"podSelector": {"matchLabels": {"app": "pulse-openshift-sre-agent"}}}
            ]
            if enabled:
                peers.append(
                    {"podSelector": {"matchLabels": {"app": "pulse-temporal"}}}
                )
            runner.get = Mock(
                return_value=owned(
                    spec={
                        "podSelector": {
                            "matchLabels": {
                                "app": "pulse-openshift-sre-agent-postgresql"
                            }
                        },
                        "policyTypes": ["Ingress"],
                        "ingress": [
                            {
                                "from": peers,
                                "ports": [{"protocol": "TCP", "port": 5432}],
                            }
                        ],
                    }
                )
            )
            runner.policy(enabled)
            with self.assertRaises(m.AcceptanceError):
                runner.policy(not enabled)

    def test_noop_update_observation_detects_secret_resourceversion_churn(self):
        runner = self.runner()

        def secret(version):
            obj = owned()
            obj["metadata"]["resourceVersion"] = version
            return obj

        deployment = owned(
            spec={
                "template": {
                    "metadata": {"annotations": {"pulse.ai/nginx-config-hash": "hash"}}
                }
            }
        )
        deployment["metadata"]["generation"] = 3
        runner.get = Mock(
            side_effect=[secret("1"), deployment, secret("2"), deployment]
        )
        with self.assertRaises(m.AcceptanceError):
            runner.idempotency()

    def test_missing_image_evidence_fails_instead_of_claiming_readiness(self):
        runner = self.runner()
        obj = owned(
            spec={"replicas": 1},
            status={"observedGeneration": 1, "readyReplicas": 1, "updatedReplicas": 1},
        )
        obj["metadata"]["generation"] = 1
        runner.get = Mock(return_value=obj)
        runner.oc = Mock(return_value={"items": []})
        with self.assertRaises(m.AcceptanceError):
            runner.readiness()

    def test_token_restored_when_rotation_observation_fails(self):
        runner = self.runner()
        secret = owned(data={"token": "old-token"})
        secret["metadata"].update(resourceVersion="10")
        deploy = {
            "spec": {
                "template": {
                    "metadata": {"annotations": {"pulse.ai/nginx-config-hash": "hash"}}
                }
            }
        }
        runner.get = Mock(side_effect=[secret, deploy])
        runner.patch = Mock()
        runner.wait = Mock(side_effect=[m.AcceptanceError("rollout failed"), True])
        runner.readiness = Mock()
        with self.assertRaises(m.AcceptanceError):
            runner.rotate()
        restoration = runner.patch.call_args_list[-1].args[2]
        self.assertEqual(
            restoration[-1],
            {"op": "replace", "path": "/data/token", "value": "old-token"},
        )
        self.assertEqual(restoration[0]["value"], "resource")

    def test_probe_failure_still_removes_temporary_pod(self):
        runner = self.runner(network_probe_image="python@sha256:" + "a" * 64)
        runner.oc = Mock(return_value={"metadata": {"uid": "probe-uid"}})
        runner.wait = Mock(side_effect=[m.AcceptanceError("probe failed"), True])
        with self.assertRaises(m.AcceptanceError):
            runner.probe("pulse-temporal", True)
        self.assertEqual(runner.oc.call_args.args[0:2], ("delete", "--raw"))
        self.assertEqual(
            runner.oc.call_args.kwargs["payload"]["preconditions"]["uid"], "probe-uid"
        )

    def test_patch_payload_is_stdin_not_process_arguments(self):
        runner = self.runner()
        runner.oc = Mock()
        operations = [
            {"op": "replace", "path": "/data/token", "value": "private-token"}
        ]
        runner.patch("secret", "pulse-ws-token", operations)
        self.assertNotIn("private-token", str(runner.oc.call_args.args))
        self.assertEqual(runner.oc.call_args.kwargs["payload"], operations)
        self.assertIn("/dev/stdin", runner.oc.call_args.args)

    def test_nonfinite_time_values_rejected_before_execution(self):
        for flag in ["--timeout", "--poll", "--observe-seconds"]:
            for value in ["nan", "inf"]:
                with self.assertRaises(SystemExit):
                    m.main(
                        [
                            "--context",
                            "test",
                            "--namespace",
                            "pulse-test",
                            "--pulse",
                            "pulse",
                            flag,
                            value,
                        ]
                    )

    def test_exact_pod_image_ids_and_owner_chains_recorded(self):
        runner = self.runner()
        names = [
            "pulse-openshift-sre-agent",
            "pulse-openshiftpulse",
            "pulse-openshift-sre-agent-postgresql",
        ]
        pods = []
        for index, name in enumerate(names):
            owner = {
                "kind": "ReplicaSet" if index < 2 else "StatefulSet",
                "name": name + "-rs",
                "uid": name + "-rs" if index < 2 else name,
                "controller": True,
            }
            pods.append(
                {
                    "metadata": {
                        "name": name + "-pod",
                        "uid": name + "-pod",
                        "labels": {"app": name},
                        "ownerReferences": [owner],
                    },
                    "spec": {"containers": [{"name": "app"}]},
                    "status": {
                        "containerStatuses": [
                            {
                                "name": "app",
                                "image": "repo:v1",
                                "imageID": "repo@sha256:digest",
                            }
                        ]
                    },
                }
            )

        def get(kind, name):
            if kind == "replicaset":
                return {
                    "metadata": {
                        "uid": name,
                        "ownerReferences": [{"uid": name[:-3], "controller": True}],
                    }
                }
            obj = owned(
                spec={"replicas": 1},
                status={
                    "observedGeneration": 1,
                    "readyReplicas": 1,
                    "updatedReplicas": 1,
                },
            )
            obj["metadata"].update(uid=name, generation=1)
            return obj

        runner.get = Mock(side_effect=get)
        runner.oc = Mock(return_value={"items": pods})
        runner.readiness()
        self.assertEqual(len(runner.image_evidence), 3)
        self.assertEqual(
            runner.image_evidence[0]["containers"][0]["imageID"], "repo@sha256:digest"
        )
        pods[0]["metadata"]["ownerReferences"][0]["uid"] = "wrong-rs"
        with self.assertRaises(m.AcceptanceError):
            runner.readiness()

    def test_plan_only_does_not_contact_cluster(self):
        with patch.object(m.Runner, "run") as run:
            self.assertEqual(
                m.main(
                    [
                        "--context",
                        "test",
                        "--namespace",
                        "pulse-test",
                        "--pulse",
                        "pulse",
                    ]
                ),
                0,
            )
            run.assert_not_called()

    def test_missing_oc_writes_failed_report(self):
        with (
            tempfile.TemporaryDirectory() as folder,
            patch.object(m.shutil, "which", return_value=None),
        ):
            report = Path(folder) / "report.json"
            rc = m.main(
                [
                    "--context",
                    "test",
                    "--namespace",
                    "pulse-test",
                    "--pulse",
                    "pulse",
                    "--execute",
                    "--report",
                    str(report),
                ]
            )
            self.assertEqual(rc, 1)
            evidence = json.loads(report.read_text())
            self.assertEqual(evidence["status"], "failed")
            self.assertIn("oc executable not found", evidence["checks"][0]["detail"])
            self.assertEqual(evidence["acceptance"], "failed")

    def test_failure_aborts_later_checks(self):
        runner = self.runner()
        runner.prerequisites = Mock(side_effect=m.AcceptanceError("forbidden"))
        runner.readiness = Mock()
        with self.assertRaises(m.AcceptanceError):
            runner.run()
        runner.readiness.assert_not_called()
        self.assertEqual(runner.checks[0]["status"], "failed")


if __name__ == "__main__":
    unittest.main()
