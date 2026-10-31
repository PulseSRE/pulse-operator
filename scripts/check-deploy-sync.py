#!/usr/bin/env python3
"""Check that deploy/operator.yaml installs what the OLM bundle installs.

deploy/operator.yaml (the `oc apply` / `make deploy` install) and the bundle
CSV (the OLM install) each carry their own copy of the manager Deployment.
Nothing kept the two in step:

- The operand images (`RELATED_IMAGE_*` env on the manager) are digest-pinned
  in both files by hand. Updating one and not the other makes a manifest
  install deploy different operands than an OLM install of the same commit.
- deploy/operator.yaml ran `pulse-operator:latest`, a moving tag, next to
  operand digests frozen at one release. Whatever release `:latest` pointed at
  that day ran operands pinned for another. It now names a version tag, and
  that tag must be the release this commit is (compat.go's OperatorVersion,
  which release.yml already requires to equal the pushed tag). Bumping
  OperatorVersion for a release therefore fails CI until the image line in
  deploy/operator.yaml is bumped with it.

Why compat.go and not the CSV for the operator version: the CSV in git stays
at v0.1.0 on purpose (scripts/bump-bundle-version.py rewrites it in the
release workflow and never commits the result), so it does not name the
current release. compat.go does.

Usage:
    scripts/check-deploy-sync.py          # exit 1 on any drift (run in CI)
"""
import re
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent
DEPLOY = ROOT / "deploy/operator.yaml"
CSV = ROOT / "bundle/manifests/pulse-operator.clusterserviceversion.yaml"
COMPAT = ROOT / "internal/controller/compat.go"
OPERATOR_IMAGE = "quay.io/amobrem/pulse-operator"


def manager_container(pod_spec: dict, where: str) -> dict:
    for container in pod_spec.get("containers") or []:
        if container.get("name") == "manager":
            return container
    raise SystemExit(f"no container named 'manager' in {where}")


def related_env(container: dict) -> dict:
    return {
        e["name"]: e.get("value")
        for e in container.get("env") or []
        if e.get("name", "").startswith("RELATED_IMAGE_")
    }


def deploy_manager() -> dict:
    for doc in yaml.safe_load_all(DEPLOY.read_text()):
        if doc and doc.get("kind") == "Deployment" and doc["metadata"]["name"] == "pulse-operator-manager":
            return manager_container(doc["spec"]["template"]["spec"], str(DEPLOY))
    raise SystemExit(f"no pulse-operator-manager Deployment in {DEPLOY}")


def csv_manager() -> dict:
    csv = yaml.safe_load(CSV.read_text())
    for dep in csv["spec"]["install"]["spec"]["deployments"]:
        return manager_container(dep["spec"]["template"]["spec"], str(CSV))
    raise SystemExit(f"no install deployment in {CSV}")


def operator_version() -> str:
    match = re.search(r'^const OperatorVersion = "([0-9]+\.[0-9]+\.[0-9]+)"$', COMPAT.read_text(), re.MULTILINE)
    if not match:
        raise SystemExit(f"could not find `const OperatorVersion = \"X.Y.Z\"` in {COMPAT}")
    return match.group(1)


def main() -> int:
    errors = []

    deploy, csv = deploy_manager(), csv_manager()
    deploy_env, csv_env = related_env(deploy), related_env(csv)
    if not csv_env:
        errors.append(f"{CSV.relative_to(ROOT)}: manager has no RELATED_IMAGE_* env at all")
    for name in sorted(set(deploy_env) | set(csv_env)):
        have, want = deploy_env.get(name), csv_env.get(name)
        if have != want:
            errors.append(
                f"{DEPLOY.relative_to(ROOT)}: {name} is {have or '<missing>'} "
                f"but the CSV pins {want or '<missing>'}"
            )

    want_image = f"{OPERATOR_IMAGE}:v{operator_version()}"
    if deploy.get("image") != want_image:
        errors.append(
            f"{DEPLOY.relative_to(ROOT)}: manager image is {deploy.get('image')!r}, expected {want_image!r} "
            f"(the release named by OperatorVersion in {COMPAT.relative_to(ROOT)}; bump both together)"
        )

    for err in errors:
        print(f"::error::{err}")
    if errors:
        return 1
    print(f"deploy/operator.yaml matches the CSV's {len(csv_env)} RELATED_IMAGE_* pins and runs {want_image}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
