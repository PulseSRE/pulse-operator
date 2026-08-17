#!/usr/bin/env python3
"""Keep operator RBAC in sync across three hand-maintained files.

config/rbac/role.yaml is the single source of truth for the manager's
ClusterRole. deploy/operator.yaml (manifest install) and the OLM
ClusterServiceVersion (OLM install) each embed a copy inside
BEGIN/END GENERATED RBAC markers. This script keeps those copies honest.

Usage:
    scripts/sync-rbac.py --check   # exit 1 if any copy has drifted (used in CI)
    scripts/sync-rbac.py --fix     # rewrite the copies to match the source

Why not controller-gen: this operator's RBAC includes many OpenShift-specific
groups (route.openshift.io, oauth.openshift.io, config.openshift.io,
monitoring.coreos.com) that are hand-curated rather than derived purely from
+kubebuilder:rbac markers today. Until that migration happens, this script is
the drift guard.
"""
import argparse
import re
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent
SOURCE = ROOT / "config/rbac/role.yaml"
TARGETS = [
    (ROOT / "deploy/operator.yaml", 0),
    (ROOT / "bundle/manifests/pulse-operator.clusterserviceversion.yaml", 10),
]

BEGIN = "# BEGIN GENERATED RBAC (source: config/rbac/role.yaml — run scripts/sync-rbac.py --fix)"
END = "# END GENERATED RBAC"

BLOCK_RE_TEMPLATE = r"{begin}\n(.*?)\n[ \t]*{end}"


def load_source_rules():
    for doc in yaml.safe_load_all(SOURCE.read_text()):
        if doc and doc.get("kind") == "ClusterRole":
            return doc["rules"]
    raise SystemExit(f"no ClusterRole found in {SOURCE}")


def normalize(rules):
    """Order-independent representation so list/verb reordering isn't flagged as drift."""
    return {
        (
            tuple(sorted(r.get("apiGroups", []))),
            tuple(sorted(r.get("resources", []))),
            tuple(sorted(r.get("verbs", []))),
        )
        for r in rules
    }


def render_block(rules, indent):
    pad = " " * indent
    lines = []
    for r in rules:
        groups = ",".join(f'"{g}"' for g in r.get("apiGroups", []))
        resources = ",".join(f'"{x}"' for x in r.get("resources", []))
        verbs = ",".join(f'"{x}"' for x in r.get("verbs", []))
        lines.append(f"{pad}- apiGroups: [{groups}]")
        lines.append(f"{pad}  resources: [{resources}]")
        lines.append(f"{pad}  verbs: [{verbs}]")
    return "\n".join(lines)


def extract_block_rules(text):
    pattern = re.compile(BLOCK_RE_TEMPLATE.format(begin=re.escape(BEGIN), end=re.escape(END)), re.DOTALL)
    m = pattern.search(text)
    if not m:
        return None
    return yaml.safe_load(m.group(1))


def replace_block(text, indent, rules):
    pad = " " * indent
    new_block = f"{pad}{BEGIN}\n{render_block(rules, indent)}\n{pad}{END}"
    pattern = re.compile(
        rf"[ \t]*{re.escape(BEGIN)}\n.*?\n[ \t]*{re.escape(END)}",
        re.DOTALL,
    )
    return pattern.sub(new_block, text, count=1)


def main():
    ap = argparse.ArgumentParser()
    group = ap.add_mutually_exclusive_group(required=True)
    group.add_argument("--check", action="store_true", help="fail if any target has drifted")
    group.add_argument("--fix", action="store_true", help="rewrite targets to match the source")
    args = ap.parse_args()

    source_rules = load_source_rules()
    drift = False

    for path, indent in TARGETS:
        text = path.read_text()
        current = extract_block_rules(text)
        if current is None:
            print(f"ERROR: {path} has no '{BEGIN}' / '{END}' markers", file=sys.stderr)
            drift = True
            continue

        if normalize(current) == normalize(source_rules):
            print(f"OK: {path} matches {SOURCE}")
            continue

        drift = True
        if args.fix:
            path.write_text(replace_block(text, indent, source_rules))
            print(f"FIXED: {path}")
        else:
            print(f"DRIFT: {path} does not match {SOURCE}", file=sys.stderr)

    if drift and args.check:
        sys.exit(1)


if __name__ == "__main__":
    main()
