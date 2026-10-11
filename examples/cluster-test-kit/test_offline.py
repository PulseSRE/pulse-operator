import contextlib
import io
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
import fixtures
import collect

IMAGE = "registry.example/test-python@sha256:" + "a" * 64

class OfflineTests(unittest.TestCase):
    def test_manifests_are_scoped_digest_pinned_and_restricted(self):
        objects = fixtures.generate("disposable", "r01", IMAGE)
        self.assertEqual(len(objects), 7)
        for obj in objects.values():
            if obj.get("kind") != "Deployment":
                continue
            self.assertEqual(obj["metadata"]["namespace"], "disposable")
            self.assertEqual(obj["spec"]["selector"]["matchLabels"]["app"], obj["spec"]["template"]["metadata"]["labels"]["app"])
            spec = obj["spec"]["template"]["spec"]
            self.assertFalse(spec["automountServiceAccountToken"])
            c = spec["containers"][0]
            self.assertEqual(c["image"], IMAGE)
            self.assertNotIn("runAsUser", c["securityContext"])
            self.assertTrue(c["securityContext"]["readOnlyRootFilesystem"])
            self.assertTrue(c["securityContext"]["runAsNonRoot"])
            self.assertEqual(c["securityContext"]["capabilities"]["drop"], ["ALL"])

    def test_fault_modes_and_repair_baselines(self):
        obj = fixtures.generate("disposable", "r01", IMAGE)
        for case, expected in [("crashloop", "bad-config"), ("rollout", "app-fault")]:
            self.assertEqual(obj[f"{case}-fault.json"]["spec"]["template"]["spec"]["containers"][0]["env"][0]["value"], expected)
        self.assertEqual(obj["pending-fault.json"]["spec"]["template"]["spec"]["nodeSelector"], {"pulse.ai/acceptance-node": "r01"})
        self.assertNotIn("nodeSelector", obj["pending-healthy.json"]["spec"]["template"]["spec"])

    def test_invalid_input(self):
        for ns, run, image in [("prod;delete", "r1", IMAGE), ("a", "../../x", IMAGE), ("a", "r1", "python:latest")]:
            with self.assertRaises(ValueError):
                fixtures.generate(ns, run, image)

    def test_configuration_fault_is_deterministic(self):
        import os
        for mode, code, text in [("bad-config", 78, "configuration error"), ("app-fault", 1, "nontransient application fault")]:
            p = subprocess.run([sys.executable, "-c", fixtures.PROGRAM], env={**os.environ, "PULSE_TEST_MODE": mode}, capture_output=True, text=True, timeout=5)
            self.assertEqual(p.returncode, code)
            self.assertIn(text, p.stdout + p.stderr)

    def test_default_collection_is_offline(self):
        with patch.object(sys, "argv", ["collect.py", "--context", "chosen", "--namespace", "disposable", "--run-id", "r1"]), patch.object(collect.subprocess, "run") as call, contextlib.redirect_stdout(io.StringIO()):
            collect.main()
            call.assert_not_called()

    def test_execute_targets_context_and_records_failure(self):
        pod = {"kind": "Pod", "metadata": {"name": "pod", "uid": "uid1"}, "spec": {"containers": [{"name": "workload"}]}}
        def fake(cmd, **kwargs):
            self.assertEqual(cmd[:6], ["oc", "--context", "chosen", "--namespace", "disposable", "--request-timeout=20s"])
            self.assertIn(cmd[6], ["get", "logs"])
            if cmd[6] == "logs":
                return subprocess.CompletedProcess(cmd, 1, "", "logs unavailable")
            return subprocess.CompletedProcess(cmd, 0, json.dumps({"items": [pod] if "deployments,replicasets,pods" in cmd else []}), "")
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp) / "evidence"
            with patch.object(sys, "argv", ["collect.py", "--context", "chosen", "--namespace", "disposable", "--run-id", "r1", "--execute", "--output", str(output)]), patch.object(collect.subprocess, "run", side_effect=fake), self.assertRaises(SystemExit) as end:
                collect.main()
            self.assertEqual(end.exception.code, 0)
            data = json.loads((output / "sample-000.json").read_text())
            self.assertEqual(data["acceptance"], "unassessed")
            self.assertEqual(len(data["logs"]), 2)
            self.assertEqual(data["logs"][0]["returncode"], 1)

    def test_read_timeout_is_explicit_failure(self):
        with patch.object(collect.subprocess, "run", side_effect=subprocess.TimeoutExpired(["oc"], 25)):
            self.assertIsNone(collect.run(["oc"])["returncode"])

if __name__ == "__main__":
    unittest.main()
