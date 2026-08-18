#!/usr/bin/env python3
"""Bump the OLM bundle's ClusterServiceVersion to a new release version.

Dockerfile.bundle copies bundle/manifests verbatim into the bundle image, but
nothing in this repo ever changed the CSV's hardcoded v0.1.0 values (name,
version, containerImage, and the manager Deployment's image) when cutting a
new release. Tagging v0.2.0 built and pushed a
pulse-operator-bundle:v0.2.0 image whose CSV still claimed to be v0.1.0 and
still pointed at the v0.1.0 operator image — OLM keys upgrades off CSV
name/version, so that release would never register as an upgrade.

This script performs the four required substitutions (via targeted regex on
specific, uniquely-anchored lines rather than a full YAML round-trip, to
preserve the file's comments/formatting exactly like scripts/sync-rbac.py
does) and sets spec.replaces to whatever CSV name was current before the
bump, so every release automatically extends the upgrade graph by one step.

Usage:
    scripts/bump-bundle-version.py 0.2.0
    scripts/bump-bundle-version.py v0.2.0   # "v" prefix is optional

Intended to run from release.yml, invoked with the pushed tag, before
Dockerfile.bundle is built.
"""
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CSV_PATH = ROOT / "bundle/manifests/pulse-operator.clusterserviceversion.yaml"
OPERATOR_IMAGE = "quay.io/amobrem/pulse-operator"

NAME_RE = re.compile(r"^  name: pulse-operator\.v[0-9]+\.[0-9]+\.[0-9]+$", re.MULTILINE)
VERSION_RE = re.compile(r"^  version: [0-9]+\.[0-9]+\.[0-9]+$", re.MULTILINE)
CONTAINER_IMAGE_ANNOTATION_RE = re.compile(
    rf"^    containerImage: {re.escape(OPERATOR_IMAGE)}:v[0-9]+\.[0-9]+\.[0-9]+$", re.MULTILINE
)
DEPLOYMENT_IMAGE_RE = re.compile(
    rf"^(\s+)image: {re.escape(OPERATOR_IMAGE)}:v[0-9]+\.[0-9]+\.[0-9]+$", re.MULTILINE
)
REPLACES_RE = re.compile(r"^  replaces: pulse-operator\.v[0-9]+\.[0-9]+\.[0-9]+\n", re.MULTILINE)


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit(f"usage: {sys.argv[0]} <version>  (e.g. 0.2.0 or v0.2.0)")

    version = sys.argv[1].lstrip("v")
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
        raise SystemExit(f"version must be semver (X.Y.Z), got: {sys.argv[1]!r}")

    text = CSV_PATH.read_text()

    name_match = re.search(r"^  name: (pulse-operator\.v[0-9]+\.[0-9]+\.[0-9]+)$", text, re.MULTILINE)
    if not name_match:
        raise SystemExit(f"could not find metadata.name in {CSV_PATH}")
    old_name = name_match.group(1)
    new_name = f"pulse-operator.v{version}"

    if old_name == new_name:
        print(f"{CSV_PATH} is already at {new_name} — nothing to do.")
        return

    for pattern, description in [
        (NAME_RE, "metadata.name"),
        (VERSION_RE, "spec.version"),
        (CONTAINER_IMAGE_ANNOTATION_RE, "metadata.annotations.containerImage"),
        (DEPLOYMENT_IMAGE_RE, "deployment container image"),
    ]:
        if not pattern.search(text):
            raise SystemExit(f"could not find expected {description} line in {CSV_PATH} — refusing to bump blind")

    text = NAME_RE.sub(f"  name: {new_name}", text)
    text = CONTAINER_IMAGE_ANNOTATION_RE.sub(f"    containerImage: {OPERATOR_IMAGE}:v{version}", text)
    text = DEPLOYMENT_IMAGE_RE.sub(rf"\1image: {OPERATOR_IMAGE}:v{version}", text)

    # Drop any previous replaces line, then set spec.version and re-insert a
    # fresh replaces line pointing at whatever CSV name was current before
    # this bump — this is what actually builds the upgrade graph one release
    # at a time, entirely automatically.
    text = REPLACES_RE.sub("", text)
    text = VERSION_RE.sub(f"  version: {version}\n  replaces: {old_name}", text, count=1)

    CSV_PATH.write_text(text)
    print(f"Bumped {CSV_PATH}: {old_name} -> {new_name} (replaces: {old_name})")


if __name__ == "__main__":
    main()
