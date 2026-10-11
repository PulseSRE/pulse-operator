#!/usr/bin/env python3
"""Plan or collect bounded read-only incident evidence; never read Secrets."""
import argparse
import datetime
import json
from pathlib import Path
import subprocess
import time
from fixtures import generate


def commands(context, namespace, run_id):
    if not context or context.startswith("-"):
        raise ValueError("context must be a nonempty context name")
    generate(namespace, run_id, "example.invalid/python@sha256:" + "0" * 64)
    base = ["oc", "--context", context, "--namespace", namespace, "--request-timeout=20s"]
    selector = f"pulse.ai/test-run={run_id}"
    return {
        "workloads": base + ["get", "deployments,replicasets,pods", "-l", selector, "-o", "json"],
        "events": base + ["get", "events", "-o", "json"],
        "constraints": base + ["get", "resourcequotas,limitranges,persistentvolumeclaims", "-o", "json"],
    }


def run(command):
    try:
        proc = subprocess.run(command, capture_output=True, text=True, timeout=25)
        return {"command": command, "returncode": proc.returncode, "stdout": proc.stdout, "stderr": proc.stderr}
    except (subprocess.TimeoutExpired, OSError) as exc:
        return {"command": command, "returncode": None, "error": str(exc)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", required=True)
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--execute", action="store_true")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--samples", type=int, default=1)
    parser.add_argument("--interval", type=int, default=30)
    args = parser.parse_args()
    try:
        planned = commands(args.context, args.namespace, args.run_id)
        if not 1 <= args.samples <= 21 or not 1 <= args.interval <= 300:
            raise ValueError("samples must be 1-21 and interval 1-300 seconds")
        if args.execute and args.output is None:
            raise ValueError("--execute requires --output")
        if args.execute and args.output.exists():
            raise ValueError("output already exists; use a fresh directory")
    except ValueError as exc:
        parser.error(str(exc))
    if not args.execute:
        print(json.dumps({"context": args.context, "namespace": args.namespace, "commands": planned,
                          "note": "Plan only. Execution additionally reads current/previous logs for selected workload containers."}, indent=2))
        return
    args.output.mkdir(parents=True)
    failed = False
    for sample in range(args.samples):
        record = {"context": args.context, "namespace": args.namespace, "run_id": args.run_id,
                  "timestamp": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                  "acceptance": "unassessed", "observations": {k: run(v) for k, v in planned.items()}}
        workload = record["observations"]["workloads"]
        logs = []
        if workload["returncode"] == 0:
            try:
                items = json.loads(workload["stdout"])["items"]
                for pod in (obj for obj in items if obj.get("kind") == "Pod"):
                    name = pod["metadata"]["name"]
                    for container in pod["spec"]["containers"]:
                        for previous in (False, True):
                            cmd = planned["workloads"][:6] + ["logs", name, "-c", container["name"], "--tail=200", "--timestamps"]
                            if previous:
                                cmd.append("--previous")
                            entry = run(cmd)
                            entry.update({"pod_uid": pod["metadata"]["uid"], "previous": previous})
                            logs.append(entry)
            except (ValueError, KeyError, TypeError) as exc:
                record["parse_error"] = str(exc)
                failed = True
        record["logs"] = logs
        # Previous logs can legitimately be absent; preserve that error without claiming success.
        failed |= any(obs["returncode"] != 0 for obs in record["observations"].values())
        (args.output / f"sample-{sample:03}.json").write_text(json.dumps(record, indent=2) + "\n")
        if sample + 1 < args.samples:
            time.sleep(args.interval)
    raise SystemExit(1 if failed else 0)

if __name__ == "__main__":
    main()
