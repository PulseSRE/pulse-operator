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
does) and sets spec.replaces to the caller-supplied previous release.

spec.replaces is derived from the repo's git tags, NOT from the CSV's own
metadata.name. release.yml runs this against a fresh checkout and never
commits the result, so the file's version stays permanently at whatever is
committed on main (today v0.1.0). Reading the previous version out of it
emitted `replaces: v0.1.0` for v0.2.0, v0.3.0, v0.4.0 and so on — leaving
every cluster past the first release with no upgrade edge at all, because OLM
builds its upgrade path from `replaces`. The released tags are the only source
of truth that survives a fresh checkout.

The tag lookup lives in this script rather than in release.yml so that fixing
this needs no workflow change at all: release.yml already invokes the script
with the pushed tag, and that call site keeps working unchanged.

Usage:
    scripts/bump-bundle-version.py 0.2.0           # replaces := highest tag below 0.2.0
    scripts/bump-bundle-version.py v0.2.0          # "v" prefix optional
    scripts/bump-bundle-version.py 0.2.0 --replaces 0.1.5   # explicit override
    scripts/bump-bundle-version.py 0.2.0 --replaces ""      # force no replaces

Intended to run from release.yml, invoked with the pushed tag, before
Dockerfile.bundle is built.
"""
import argparse
import re
import subprocess
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


def released_versions() -> list:
    """Every vX.Y.Z tag in the repo, as bare X.Y.Z strings.

    Fetches tags first: actions/checkout does a shallow clone with no tags by
    default, and this must not depend on the workflow being configured with
    fetch-depth: 0 (the whole point of keeping the lookup here is that the
    workflow needs no edit). Only tag *names* are read, so a shallow object
    graph is fine. A failed fetch is non-fatal — fall back to local refs.
    """
    subprocess.run(["git", "fetch", "--tags", "--quiet"], check=False)
    result = subprocess.run(
        ["git", "tag", "--list", "v*"], capture_output=True, text=True, check=False
    )
    return [
        candidate
        for candidate in (line.lstrip("v") for line in result.stdout.split())
        if re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", candidate)
    ]


def version_key(v: str) -> tuple:
    return tuple(int(part) for part in v.split("."))


def previous_release(version: str) -> str:
    """Highest released version strictly below `version`, or "" if there is none.

    Selecting "highest below the one being released" rather than "the entry
    before this one in tag order" keeps re-tagging an older patch release
    correct, and does not require the tag being released to exist yet.
    """
    target = version_key(version)
    lower = sorted({v for v in released_versions() if version_key(v) < target}, key=version_key)
    return lower[-1] if lower else ""


def semver(raw: str, what: str) -> str:
    """Normalise a vX.Y.Z / X.Y.Z string, rejecting anything else."""
    value = raw.lstrip("v")
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", value):
        raise SystemExit(f"{what} must be semver (X.Y.Z), got: {raw!r}")
    return value


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("version", help="release version to bump to, e.g. 0.2.0 or v0.2.0")
    parser.add_argument(
        "--replaces",
        default=None,
        help="override the previous released version this one upgrades from, e.g. 0.1.0. "
        "Omitted, it is derived from the repo's git tags. Pass an empty string to force "
        "no replaces at all (the very first release).",
    )
    args = parser.parse_args()

    version = semver(args.version, "version")
    if args.replaces is None:
        previous = previous_release(version)
    elif args.replaces:
        previous = semver(args.replaces, "--replaces")
    else:
        previous = ""

    if previous == version:
        raise SystemExit(f"--replaces {args.replaces!r} is the version being released — refusing to self-replace")

    text = CSV_PATH.read_text()

    name_match = re.search(r"^  name: (pulse-operator\.v[0-9]+\.[0-9]+\.[0-9]+)$", text, re.MULTILINE)
    if not name_match:
        raise SystemExit(f"could not find metadata.name in {CSV_PATH}")
    new_name = f"pulse-operator.v{version}"

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

    # Drop any committed replaces line, then set spec.version and re-insert a
    # fresh one pointing at the caller-supplied previous release. This is what
    # builds the upgrade graph one release at a time; a release with no
    # predecessor (the first one) correctly carries no replaces at all.
    text = REPLACES_RE.sub("", text)
    if previous:
        text = VERSION_RE.sub(f"  version: {version}\n  replaces: pulse-operator.v{previous}", text, count=1)
    else:
        text = VERSION_RE.sub(f"  version: {version}", text, count=1)

    CSV_PATH.write_text(text)
    replaces_note = f"pulse-operator.v{previous}" if previous else "<none — first release>"
    print(f"Bumped {CSV_PATH} to {new_name} (replaces: {replaces_note})")


if __name__ == "__main__":
    main()
