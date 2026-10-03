#!/usr/bin/env python3
"""Opt-in OpenShift acceptance observations; never installs Pulse or changes context."""

from __future__ import annotations

import argparse
import base64
import json
import math
import re
import secrets
import shutil
import subprocess
import sys
import time
from pathlib import Path


class AcceptanceError(RuntimeError):
    pass


class Runner:
    def __init__(
        self, args, command=subprocess.run, sleep=time.sleep, clock=time.monotonic
    ):
        self.args, self.command, self.sleep, self.clock = args, command, sleep, clock
        self.checks = []
        self.cr = None
        self.image_evidence = []

    def oc(self, *argv, payload=None):
        command = [
            "oc",
            "--context",
            self.args.context,
            "--namespace",
            self.args.namespace,
            "--request-timeout=30s",
            *argv,
        ]
        result = self.command(
            command,
            input=json.dumps(payload) if payload is not None else None,
            capture_output=True,
            text=True,
            timeout=45,
        )
        if result.returncode:
            # oc may echo request payloads or Secrets. Never copy raw output into reports.
            raise AcceptanceError(
                f"oc {argv[0]} failed (exit {result.returncode}); inspect cluster separately"
            )
        try:
            return json.loads(result.stdout) if result.stdout.strip() else {}
        except json.JSONDecodeError as exc:
            raise AcceptanceError("oc returned invalid JSON") from exc

    def get(self, kind, name):
        return self.oc("get", kind, name, "-o", "json")

    def check(self, name, fn):
        try:
            detail = fn()
        except Exception as exc:
            self.checks.append(
                {
                    "name": name,
                    "status": "failed",
                    "detail": str(exc)
                    if isinstance(exc, AcceptanceError)
                    else f"{type(exc).__name__}; inspect prerequisite/cluster separately",
                }
            )
            raise
        self.checks.append({"name": name, "status": "passed", "detail": detail})

    def wait(self, predicate, description):
        deadline = self.clock() + self.args.timeout
        while True:
            result = predicate()
            if result:
                return result
            if self.clock() >= deadline:
                raise AcceptanceError(f"timed out: {description}")
            self.sleep(self.args.poll)

    def owned(self, obj):
        if not any(
            o.get("uid") == self.cr["metadata"]["uid"] and o.get("controller") is True
            for o in obj.get("metadata", {}).get("ownerReferences", [])
        ):
            raise AcceptanceError(
                "observed resource lacks expected Pulse controller owner"
            )

    def prerequisites(self):
        ns = self.get("namespace", self.args.namespace)
        self.cr = self.get("openshiftpulse.pulse.ai", self.args.pulse)
        if self.cr["metadata"].get("namespace") != self.args.namespace:
            raise AcceptanceError("CR namespace mismatch")
        if (
            self.args.rotate_token
            or self.args.exercise_temporal_toggle
            or self.args.network_probe_image
        ):
            if not self.args.allow_mutations:
                raise AcceptanceError("mutation checks require --allow-mutations")
            if (
                ns["metadata"].get("labels", {}).get("pulse.ai/acceptance")
                != "disposable"
            ):
                raise AcceptanceError(
                    "mutation checks require namespace label pulse.ai/acceptance=disposable"
                )
        return "explicit context, namespace and existing CR accessible; no installation performed"

    def readiness(self):
        names = [
            ("deployment", self.args.pulse + "-openshift-sre-agent"),
            ("deployment", self.args.pulse + "-openshiftpulse"),
            ("statefulset", self.args.pulse + "-openshift-sre-agent-postgresql"),
        ]
        if self.cr.get("spec", {}).get("agent", {}).get("mcp", {}).get("enabled"):
            names.append(("deployment", self.args.pulse + "-mcp-server"))
        if self.cr.get("spec", {}).get("temporal", {}).get("enabled", False):
            names.append(("deployment", self.args.pulse + "-temporal"))
            if self.cr["spec"]["temporal"].get("ui"):
                names.append(("deployment", self.args.pulse + "-temporal-ui"))
        workload_uids = {}
        for kind, name in names:

            def ready(kind=kind, name=name):
                obj = self.get(kind, name)
                self.owned(obj)
                workload_uids[name] = obj["metadata"]["uid"]
                desired, status = (
                    obj.get("spec", {}).get("replicas", 1),
                    obj.get("status", {}),
                )
                return (
                    desired > 0
                    and status.get("observedGeneration", 0)
                    >= obj["metadata"]["generation"]
                    and status.get("readyReplicas", 0) == desired
                    and status.get("updatedReplicas", 0) == desired
                )

            self.wait(ready, f"{kind}/{name} rollout ready")
        pods = self.oc("get", "pods", "-o", "json").get("items", [])
        evidence = []
        for kind, name in names:
            selected = [
                pod
                for pod in pods
                if pod.get("metadata", {}).get("labels", {}).get("app") == name
                and not pod.get("metadata", {}).get("deletionTimestamp")
            ]
            if not selected:
                raise AcceptanceError(f"no pods found for workload {name}")
            for pod in selected:
                refs = [
                    o
                    for o in pod["metadata"].get("ownerReferences", [])
                    if o.get("controller") is True
                ]
                if len(refs) != 1:
                    raise AcceptanceError(
                        f"pod lacks controller owner chain for {name}"
                    )
                owner = refs[0]
                if kind == "deployment":
                    if owner.get("kind") != "ReplicaSet":
                        raise AcceptanceError(
                            f"pod controller is not expected ReplicaSet for {name}"
                        )
                    rs = self.get("replicaset", owner["name"])
                    if rs["metadata"]["uid"] != owner["uid"] or not any(
                        o.get("uid") == workload_uids[name]
                        and o.get("controller") is True
                        for o in rs["metadata"].get("ownerReferences", [])
                    ):
                        raise AcceptanceError(
                            f"pod ReplicaSet does not belong to workload {name}"
                        )
                elif (
                    owner.get("uid") != workload_uids[name]
                    or owner.get("kind") != "StatefulSet"
                ):
                    raise AcceptanceError(
                        f"pod controller does not belong to workload {name}"
                    )
                statuses = pod.get("status", {}).get("containerStatuses", [])
                expected_containers = {c["name"] for c in pod["spec"]["containers"]}
                if {c["name"] for c in statuses} != expected_containers or any(
                    not c.get("imageID") for c in statuses
                ):
                    raise AcceptanceError(
                        f"missing exact imageID evidence for workload {name}"
                    )
                evidence.append(
                    {
                        "workload": name,
                        "pod": pod["metadata"]["name"],
                        "uid": pod["metadata"]["uid"],
                        "containers": [
                            {
                                "name": c["name"],
                                "image": c["image"],
                                "imageID": c["imageID"],
                            }
                            for c in statuses
                        ],
                    }
                )
        self.image_evidence = evidence
        return "owned workload rollouts ready; exact pod/container imageIDs captured (including sidecars)"

    def policy(self, enabled=None):
        if enabled is None:
            enabled = self.cr.get("spec", {}).get("temporal", {}).get("enabled", False)
        np = self.get("networkpolicy", self.args.pulse + "-pg-access")
        self.owned(np)
        spec = np.get("spec", {})
        expected = {self.args.pulse + "-openshift-sre-agent"}
        if enabled:
            expected.add(self.args.pulse + "-temporal")
        rules = spec.get("ingress", [])
        valid = (
            spec.get("podSelector")
            == {
                "matchLabels": {
                    "app": self.args.pulse + "-openshift-sre-agent-postgresql"
                }
            }
            and spec.get("policyTypes") == ["Ingress"]
            and len(rules) == 1
        )
        if valid:
            rule = rules[0]
            peers = rule.get("from", [])
            valid = (
                rule.get("ports") == [{"port": 5432, "protocol": "TCP"}]
                and len(peers) == len(expected)
                and {
                    p.get("podSelector", {}).get("matchLabels", {}).get("app")
                    for p in peers
                }
                == expected
                and all(
                    p
                    == {
                        "podSelector": {
                            "matchLabels": {
                                "app": p["podSelector"]["matchLabels"]["app"]
                            }
                        }
                    }
                    for p in peers
                )
            )
        if not valid:
            raise AcceptanceError(
                f"PostgreSQL policy is not scoped to expected TCP5432 peers (Temporal {enabled})"
            )
        return f"actual NetworkPolicy manifest admits agent and Temporal={enabled}; packet enforcement not tested"

    def idempotency(self):
        def snapshot():
            secret = self.get("secret", self.args.pulse + "-nginx")
            deploy = self.get("deployment", self.args.pulse + "-openshiftpulse")
            self.owned(secret)
            self.owned(deploy)
            return (
                secret["metadata"]["resourceVersion"],
                deploy["metadata"]["generation"],
                deploy["spec"]["template"]
                .get("metadata", {})
                .get("annotations", {})
                .get("pulse.ai/nginx-config-hash"),
            )

        before = snapshot()
        if not before[2]:
            raise AcceptanceError("UI template lacks nginx configuration hash")
        self.sleep(self.args.observe_seconds)
        if snapshot() != before:
            raise AcceptanceError(
                "nginx Secret or UI template changed during stability observation"
            )
        return "Secret resourceVersion and UI generation/hash stable; does not prove absence of no-op API UPDATE requests"

    def probe(self, label, should_connect):
        name = "pulse-acceptance-" + secrets.token_hex(5)
        host = self.args.pulse + "-openshift-sre-agent-postgresql"
        code = (
            "import socket,json; s=socket.socket(); s.settimeout(5); ok=s.connect_ex(("
            + repr(host)
            + ",5432))==0; print(json.dumps({'connected':ok})); s.close()"
        )
        pod = {
            "apiVersion": "v1",
            "kind": "Pod",
            "metadata": {"name": name, "labels": {"app": label}},
            "spec": {
                "restartPolicy": "Never",
                "automountServiceAccountToken": False,
                "securityContext": {
                    "runAsNonRoot": True,
                    "seccompProfile": {"type": "RuntimeDefault"},
                },
                "containers": [
                    {
                        "name": "probe",
                        "image": self.args.network_probe_image,
                        "command": ["python3", "-c", code],
                        "securityContext": {
                            "allowPrivilegeEscalation": False,
                            "readOnlyRootFilesystem": True,
                            "capabilities": {"drop": ["ALL"]},
                        },
                        "resources": {
                            "requests": {"cpu": "10m", "memory": "32Mi"},
                            "limits": {"cpu": "100m", "memory": "64Mi"},
                        },
                    }
                ],
            },
        }
        created = False
        try:
            obj = self.oc("create", "-f", "-", "-o", "json", payload=pod)
            created = True
            uid = obj["metadata"]["uid"]

            def finished():
                current = self.get("pod", name)
                if current["metadata"]["uid"] != uid:
                    raise AcceptanceError("probe pod identity changed")
                phase = current.get("status", {}).get("phase")
                if phase == "Failed":
                    raise AcceptanceError(
                        "probe pod failed; no network conclusion available"
                    )
                return phase == "Succeeded"

            self.wait(finished, "network probe pod completion")
            result = self.oc("logs", name, "-c", "probe")
            if (
                type(result.get("connected")) is not bool
                or result["connected"] != should_connect
            ):
                raise AcceptanceError(
                    f"TCP5432 probe result mismatched for label class {label}"
                )
        finally:
            if created:
                self.oc(
                    "delete",
                    "--raw",
                    f"/api/v1/namespaces/{self.args.namespace}/pods/{name}",
                    "-f",
                    "-",
                    payload={
                        "apiVersion": "v1",
                        "kind": "DeleteOptions",
                        "preconditions": {"uid": uid},
                    },
                )
                self.wait(
                    lambda: (
                        self.oc("get", "pod", name, "--ignore-not-found", "-o", "json")
                        .get("metadata", {})
                        .get("uid")
                        != uid
                    ),
                    "original probe pod removed",
                )
        return "new TCP connection observed; probe pod removed"

    def network(self, enabled=None):
        if enabled is None:
            enabled = self.cr.get("spec", {}).get("temporal", {}).get("enabled", False)
        self.probe(self.args.pulse + "-openshift-sre-agent", True)
        self.probe(self.args.pulse + "-temporal", enabled)
        self.probe("pulse-acceptance-untrusted", False)
        return f"fresh agent-label TCP connection allowed; Temporal-label allowed={enabled}; unrelated label denied"

    def patch(self, kind, name, operations):
        return self.oc(
            "patch",
            kind,
            name,
            "--type=json",
            "--patch-file",
            "/dev/stdin",
            "-o",
            "json",
            payload=operations,
        )

    def rotate(self):
        name = self.args.pulse + "-ws-token"
        old = self.get("secret", name)
        self.owned(old)
        original = old.get("data", {}).get("token")
        if not original:
            raise AcceptanceError("WS token Secret missing token data")
        before = self.get("deployment", self.args.pulse + "-openshiftpulse")["spec"][
            "template"
        ]["metadata"]["annotations"]["pulse.ai/nginx-config-hash"]
        token = base64.b64encode(secrets.token_urlsafe(32).encode()).decode()
        changed = False
        try:
            self.patch(
                "secret",
                name,
                [
                    {
                        "op": "test",
                        "path": "/metadata/uid",
                        "value": old["metadata"]["uid"],
                    },
                    {
                        "op": "test",
                        "path": "/metadata/resourceVersion",
                        "value": old["metadata"]["resourceVersion"],
                    },
                    {"op": "replace", "path": "/data/token", "value": token},
                ],
            )
            changed = True
            self.wait(
                lambda: (
                    self.get("deployment", self.args.pulse + "-openshiftpulse")["spec"][
                        "template"
                    ]["metadata"]["annotations"]["pulse.ai/nginx-config-hash"]
                    != before
                ),
                "token rotation propagates nginx hash",
            )
            self.readiness()
        finally:
            if changed:
                self.patch(
                    "secret",
                    name,
                    [
                        {
                            "op": "test",
                            "path": "/metadata/uid",
                            "value": old["metadata"]["uid"],
                        },
                        {"op": "test", "path": "/data/token", "value": token},
                        {"op": "replace", "path": "/data/token", "value": original},
                    ],
                )
                self.wait(
                    lambda: (
                        self.get("deployment", self.args.pulse + "-openshiftpulse")[
                            "spec"
                        ]["template"]["metadata"]["annotations"][
                            "pulse.ai/nginx-config-hash"
                        ]
                        == before
                    ),
                    "original token hash restored",
                )
                self.readiness()
        return "rotated token, observed new UI hash/ready rollout, restored original token/hash; authenticated traffic not tested"

    def toggle(self):
        if not self.cr.get("spec", {}).get("temporal", {}).get("enabled", False):
            raise AcceptanceError(
                "toggle check requires initially enabled Temporal; does not provision it"
            )
        changed = False
        try:
            current = self.get("openshiftpulse.pulse.ai", self.args.pulse)
            self.patch(
                "openshiftpulse.pulse.ai",
                self.args.pulse,
                [
                    {
                        "op": "test",
                        "path": "/metadata/resourceVersion",
                        "value": current["metadata"]["resourceVersion"],
                    },
                    {"op": "test", "path": "/spec/temporal/enabled", "value": True},
                    {"op": "replace", "path": "/spec/temporal/enabled", "value": False},
                ],
            )
            changed = True

            def revoked():
                try:
                    self.policy(False)
                    return True
                except AcceptanceError:
                    return False

            self.wait(revoked, "Temporal peer revoked")
            if self.args.network_probe_image:
                self.network(False)
        finally:
            if changed:
                self.patch(
                    "openshiftpulse.pulse.ai",
                    self.args.pulse,
                    [
                        {
                            "op": "test",
                            "path": "/metadata/uid",
                            "value": self.cr["metadata"]["uid"],
                        },
                        {
                            "op": "test",
                            "path": "/spec/temporal/enabled",
                            "value": False,
                        },
                        {
                            "op": "replace",
                            "path": "/spec/temporal/enabled",
                            "value": True,
                        },
                    ],
                )

                def restored():
                    try:
                        self.policy(True)
                        return True
                    except AcceptanceError:
                        return False

                self.wait(restored, "Temporal peer restored")
                if self.args.network_probe_image:
                    self.network(True)
                self.readiness()
        return "Temporal policy peer revoked while disabled and restored when enabled; workflow/packet behavior not tested"

    def run(self):
        for name, fn in [
            ("prerequisites", self.prerequisites),
            ("workload-readiness", self.readiness),
            ("postgresql-policy", self.policy),
            ("nginx-stability-observation", self.idempotency),
        ]:
            self.check(name, fn)
        if self.args.network_probe_image:
            self.check("postgresql-packet-probes", self.network)
        if self.args.rotate_token:
            self.check("token-rotation-and-restoration", self.rotate)
        if self.args.exercise_temporal_toggle:
            self.check("temporal-policy-toggle-and-restoration", self.toggle)


def parser():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--context", required=True)
    p.add_argument("--namespace", required=True)
    p.add_argument("--pulse", required=True)
    p.add_argument(
        "--execute", action="store_true", help="opt in to real cluster reads/checks"
    )
    p.add_argument("--allow-mutations", action="store_true")
    p.add_argument("--rotate-token", action="store_true")
    p.add_argument("--exercise-temporal-toggle", action="store_true")
    p.add_argument(
        "--network-probe-image",
        help="digest-pinned image containing python3; creates/deletes probe pods",
    )
    p.add_argument(
        "--report", type=Path, default=Path("cluster-acceptance-report.json")
    )
    p.add_argument("--timeout", type=float, default=600)
    p.add_argument("--poll", type=float, default=5)
    p.add_argument(
        "--observe-seconds",
        type=float,
        default=65,
        help="at least two 30s controller reconciliation intervals",
    )
    return p


def main(argv=None):
    args = parser().parse_args(argv)
    if any(
        not re.fullmatch(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?", v)
        for v in [args.namespace, args.pulse]
    ):
        raise SystemExit("namespace/pulse must be DNS labels")
    if args.network_probe_image and not re.fullmatch(
        r"[^\s]+@sha256:[0-9a-f]{64}", args.network_probe_image
    ):
        raise SystemExit("network probe image must be pinned by sha256 digest")
    if (
        not all(
            math.isfinite(v) for v in [args.timeout, args.poll, args.observe_seconds]
        )
        or min(args.timeout, args.poll) <= 0
        or args.observe_seconds < 65
    ):
        raise SystemExit("timeout/poll must be positive and observe-seconds >=65")
    if not args.execute:
        print(
            "Plan only. No cluster contacted. Add --execute for explicit cluster checks; mutation checks also require disposable label and --allow-mutations."
        )
        return 0
    runner = Runner(args)
    status = "failed"
    try:
        if not shutil.which("oc"):
            raise AcceptanceError("oc executable not found")
        runner.run()
        status = "passed"
    except Exception as exc:  # noqa: BLE001 -- every runner failure must produce a redacted failed report
        if not runner.checks or runner.checks[-1]["status"] != "failed":
            runner.checks.append(
                {
                    "name": "runner",
                    "status": "failed",
                    "detail": str(exc)
                    if isinstance(exc, AcceptanceError)
                    else type(exc).__name__,
                }
            )
    report = {
        "status": status,
        "acceptance": "incomplete" if status == "passed" else "failed",
        "cr_identity": (
            {
                k: runner.cr["metadata"].get(k)
                for k in ["name", "namespace", "uid", "generation"]
            }
            if runner.cr
            else None
        ),
        "images": runner.image_evidence,
        "context": args.context,
        "namespace": args.namespace,
        "pulse": args.pulse,
        "checks": runner.checks,
        "optional_checks": {
            "token_rotation": args.rotate_token,
            "temporal_toggle": args.exercise_temporal_toggle,
            "packet_probes": bool(args.network_probe_image),
        },
        "unverified": [
            *(
                ["token rotation propagation/restoration"]
                if not args.rotate_token
                else []
            ),
            *(
                ["Temporal enabled/disabled policy transition"]
                if not args.exercise_temporal_toggle
                else []
            ),
            *(
                ["actual NetworkPolicy packet enforcement"]
                if not args.network_probe_image
                else []
            ),
            "authenticated UI/API requests",
            "provider-backed agent behavior",
            "Temporal workflow persistence and approvals",
            "API audit proof of no-op reconciliation",
        ],
    }
    args.report.write_text(json.dumps(report, indent=2) + "\n")
    print(
        f"Operator checks {status}; full release acceptance remains unproven. Report: {args.report}"
    )
    return 0 if status == "passed" else 1


if __name__ == "__main__":
    sys.exit(main())
