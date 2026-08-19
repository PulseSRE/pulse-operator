#!/usr/bin/env python3
"""Properly uninstall an OLM-installed operator, including its owned resources.

Motivation: `oc delete subscription` + `oc delete csv` — the usual way people
uninstall an operator — only removes the operator's own controller. It does
NOT remove the custom resources that controller was managing, so whatever
Deployments/Services/etc. those CRs caused to be created keep running forever,
orphaned, with nothing left to reconcile them. OLM also never removes CRDs on
uninstall (by design, to avoid destroying data for any remaining CRs) — but
that means a "clean" uninstall still leaves both the CRDs AND any CR instances
sitting around, invisible to `oc get csv`/`oc get subscription` alone. This
script showed exactly that gap in practice: uninstalling the Authorino
Operator via CSV+Subscription deletion left a fully running `authorino`
Deployment behind, because the `Authorino` CR that Deployment came from was
never deleted.

This script closes that gap by reading the CSV's own
`spec.customresourcedefinitions.owned[]` — the operator's own manifest of
which CRDs it manages — to find every CR instance the operator owns, and
deletes those FIRST (letting their owned Deployments/Services/etc. cascade
away via ownerReferences) before removing the Subscription, the CSV, and any
stale InstallPlans left over from the original install.

Usage:
    scripts/olm-uninstall.py authorino                     # dry run (default)
    scripts/olm-uninstall.py authorino --yes               # actually uninstall
    scripts/olm-uninstall.py authorino --yes --keep-crs    # leave CR instances alone
    scripts/olm-uninstall.py authorino --yes --delete-crds # also remove the CRDs

Requires: `oc` on PATH, already logged in with sufficient RBAC
(cluster-admin is the simplest way to guarantee that).
"""
import argparse
import json
import subprocess
import sys
import time


def oc_json(*args):
    result = subprocess.run(["oc", *args, "-o", "json"], capture_output=True, text=True)
    if result.returncode != 0:
        return None
    try:
        return json.loads(result.stdout)
    except json.JSONDecodeError:
        return None


def oc(*args, check=True):
    print(f"  $ oc {' '.join(args)}")
    result = subprocess.run(["oc", *args], capture_output=True, text=True)
    if result.stdout.strip():
        print(f"    {result.stdout.strip()}")
    if result.returncode != 0:
        print(f"    stderr: {result.stderr.strip()}", file=sys.stderr)
        if check:
            raise SystemExit(1)
    return result


def find_csvs(match):
    data = oc_json("get", "csv", "-A")
    if not data:
        raise SystemExit("ERROR: could not list ClusterServiceVersions — check `oc whoami`")
    return [
        item
        for item in data.get("items", [])
        if match.lower() in item["metadata"]["name"].lower()
    ]


def find_subscriptions(package_name):
    data = oc_json("get", "subscriptions.operators.coreos.com", "-A")
    if not data:
        return []
    return [
        item
        for item in data.get("items", [])
        if item.get("spec", {}).get("name") == package_name
    ]


def find_installplans(csv_name, namespace):
    data = oc_json("get", "installplan", "-n", namespace)
    if not data:
        return []
    return [
        item
        for item in data.get("items", [])
        if csv_name in item.get("spec", {}).get("clusterServiceVersionNames", [])
    ]


def owned_crds(csv):
    return csv.get("spec", {}).get("customresourcedefinitions", {}).get("owned", [])


def find_cr_instances(crd_full_name):
    """crd_full_name is the CRD's own resource name, e.g.
    'authorinos.operator.authorino.kuadrant.io' — using that exact string as
    the resource type with `oc get` disambiguates group/version for us."""
    data = oc_json("get", crd_full_name, "-A")
    if not data:
        return []
    return data.get("items", [])


def wait_for_gone(resource_type, namespace, name, timeout=30):
    get_args = ["oc", "get", resource_type, name]
    if namespace:
        get_args += ["-n", namespace]
    deadline = time.time() + timeout
    while time.time() < deadline:
        result = subprocess.run(get_args, capture_output=True)
        if result.returncode != 0:
            return True
        time.sleep(2)
    return False


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("match", help="substring to match against CSV names (case-insensitive), e.g. 'authorino'")
    ap.add_argument("--yes", action="store_true", help="actually perform deletions (default: dry run / plan only)")
    ap.add_argument("--keep-crs", action="store_true", help="don't delete CR instances — only remove Subscription/CSV/InstallPlans")
    ap.add_argument("--delete-crds", action="store_true", help="also delete the owned CRDs themselves (more destructive — off by default)")
    args = ap.parse_args()

    csvs = find_csvs(args.match)
    if not csvs:
        print(f"No CSV found matching '{args.match}'. Nothing to do.")
        print("(If you already deleted the CSV/Subscription, this script can't find owned-CRD info anymore —")
        print(" run it BEFORE deleting the CSV next time, or clean up remaining CRs/CRDs manually.)")
        return

    for csv in csvs:
        csv_name = csv["metadata"]["name"]
        namespace = csv["metadata"]["namespace"]

        print(f"\n=== {csv_name} (namespace: {namespace}) ===")

        crds = owned_crds(csv)
        print(f"Owned CRDs: {[c['name'] for c in crds] or '(none)'}")

        cr_plan = []
        if not args.keep_crs:
            for crd in crds:
                instances = find_cr_instances(crd["name"])
                for inst in instances:
                    cr_plan.append((crd["name"], inst["metadata"].get("namespace"), inst["metadata"]["name"]))
            if cr_plan:
                print(f"CR instances that will be deleted ({len(cr_plan)}):")
                for crd_name, ns, name in cr_plan:
                    where = f"{ns}/{name}" if ns else name
                    print(f"  - {crd_name}: {where}")
            else:
                print("CR instances: (none found)")

        # A Subscription's spec.name is the OLM package name, not the CSV's
        # displayName — and CSV names follow "<package-name>.v<version>", so
        # the package name is everything before the last ".v".
        package_name = csv_name.rsplit(".v", 1)[0]
        subs = find_subscriptions(package_name)
        print(f"Subscriptions to delete: {[s['metadata']['name'] for s in subs] or '(none found)'}")

        plans = find_installplans(csv_name, namespace)
        print(f"Stale InstallPlans to delete: {[p['metadata']['name'] for p in plans] or '(none found)'}")

        if not args.yes:
            continue

        print(f"\n--- Deleting resources for {csv_name} ---")

        for crd_name, ns, name in cr_plan:
            del_args = ["delete", crd_name, name]
            if ns:
                del_args += ["-n", ns]
            oc(*del_args, check=False)
            if not wait_for_gone(crd_name, ns, name):
                print(f"  WARNING: {crd_name}/{name} did not disappear within 30s — "
                      "it may have a finalizer stuck waiting on something else. Check it manually.")

        for sub in subs:
            oc("delete", "subscription", sub["metadata"]["name"], "-n", sub["metadata"]["namespace"], check=False)

        oc("delete", "csv", csv_name, "-n", namespace, check=False)

        for plan in plans:
            oc("delete", "installplan", plan["metadata"]["name"], "-n", namespace, check=False)

        if args.delete_crds:
            for crd in crds:
                oc("delete", "crd", crd["name"], check=False)
        elif crds:
            print(f"\nCRDs left in place (use --delete-crds to remove): {[c['name'] for c in crds]}")

    if not args.yes:
        print("\nDry run only — nothing was deleted. Re-run with --yes to execute this plan.")


if __name__ == "__main__":
    main()
