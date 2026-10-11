#!/usr/bin/env python3
"""Generate digest-pinned incident fixtures offline. Never invokes Kubernetes."""
import argparse
import copy
import json
import re
from pathlib import Path

PROGRAM = '''import os, sys, time
mode = os.environ.get("PULSE_TEST_MODE", "healthy")
if mode == "bad-config":
    print("configuration error: PULSE_TEST_MODE=bad-config is invalid", flush=True)
    sys.exit(78)
if mode == "app-fault":
    raise RuntimeError("deterministic nontransient application fault")
print("healthy test workload ready", flush=True)
while True:
    time.sleep(10)
'''

def generate(namespace, run_id, image):
    for value in (namespace, run_id):
        if not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?", value):
            raise ValueError("namespace and run-id must be DNS labels of 1-40 characters")
    if not re.fullmatch(r"[^\s@]+@sha256:[0-9a-f]{64}", image):
        raise ValueError("image must have an explicit sha256 digest of 64 lowercase hex digits")
    result = {}
    for case in ("crashloop", "rollout", "pending"):
        name = f"pulse-test-{run_id}-{case}"
        labels = {"app": name, "pulse.ai/test-run": run_id, "pulse.ai/test-case": case}
        container = {"name": "workload", "image": image, "imagePullPolicy": "IfNotPresent",
                     "command": ["python3", "-u", "-c", PROGRAM],
                     "env": [{"name": "PULSE_TEST_MODE", "value": "healthy"}],
                     "resources": {"requests": {"cpu": "10m", "memory": "32Mi"}, "limits": {"cpu": "100m", "memory": "64Mi"}},
                     "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                         "runAsNonRoot": True, "capabilities": {"drop": ["ALL"]}},
                     "readinessProbe": {"exec": {"command": ["python3", "-c", "import os; assert os.getenv('PULSE_TEST_MODE') == 'healthy'"]},
                                        "initialDelaySeconds": 2, "periodSeconds": 3}}
        deployment = {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": name, "namespace": namespace, "labels": labels},
                      "spec": {"replicas": 1, "progressDeadlineSeconds": 60,
                               "strategy": {"type": "RollingUpdate", "rollingUpdate": {"maxUnavailable": 0, "maxSurge": 1}},
                               "selector": {"matchLabels": {"app": name}},
                               "template": {"metadata": {"labels": labels}, "spec": {"automountServiceAccountToken": False,
                                   "securityContext": {"seccompProfile": {"type": "RuntimeDefault"}}, "containers": [container]}}}}
        result[f"{case}-healthy.json"] = deployment
        fault = copy.deepcopy(deployment)
        if case == "pending":
            # Existing nodes MUST be checked for this unique key/value before applying.
            fault["spec"]["template"]["spec"]["nodeSelector"] = {"pulse.ai/acceptance-node": run_id}
        else:
            fault["spec"]["template"]["spec"]["containers"][0]["env"][0]["value"] = "bad-config" if case == "crashloop" else "app-fault"
        result[f"{case}-fault.json"] = fault
    result["identity.json"] = {"namespace": namespace, "run_id": run_id, "image": image,
                               "deployments": [result[f"{case}-healthy.json"]["metadata"]["name"] for case in ("crashloop", "rollout", "pending")]}
    return result

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    try:
        objects = generate(args.namespace, args.run_id, args.image)
        args.output.mkdir(parents=True, exist_ok=False)
    except (ValueError, FileExistsError) as exc:
        parser.error(str(exc))
    for filename, obj in objects.items():
        (args.output / filename).write_text(json.dumps(obj, indent=2) + "\n")
    print(f"Wrote {len(objects)} offline files to {args.output}; no cluster calls made")

if __name__ == "__main__":
    main()
