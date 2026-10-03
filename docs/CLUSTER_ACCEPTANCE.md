# Operator cluster acceptance observations

This runner inspects an **existing** operator-managed Pulse installation on a user-selected OpenShift cluster. It does not create a cluster, install Pulse, change kubeconfig context, log in, or execute provider-backed agent prompts. Nothing in this guide has been run against a real cluster as part of the runner implementation.

## Plan and read-only observations

Requirements: Python 3.9+, `oc`, access to the explicit context/namespace/CR, and permission to read namespaces, OpenShiftPulse, Deployments, StatefulSets, ReplicaSets, Pods, NetworkPolicies, and relevant Secrets. Secret content stays in memory and is not written to the report. The cluster and CR must already exist.

```bash
# No cluster calls: inspect the planned scope.
python3 scripts/cluster-acceptance.py \
  --context YOUR_CONTEXT --namespace YOUR_NAMESPACE --pulse YOUR_CR

# Explicit opt-in to real reads. No mutations without additional flags.
python3 scripts/cluster-acceptance.py \
  --context YOUR_CONTEXT --namespace YOUR_NAMESPACE --pulse YOUR_CR \
  --execute --report /tmp/pulse-operator-observations.json
```

Every `oc` command carries the supplied context and namespace and a bounded request timeout. The runner verifies controller ownership, observed rollout generations/readiness, enabled optional workloads, exact pod image IDs (including UI sidecars), and the PostgreSQL NetworkPolicy's exact TCP 5432 peer selectors. Pod ownership is traced through ReplicaSets to Deployments, rather than trusting an `app` label alone.

A 65-second observation checks that the nginx Secret resourceVersion and UI template generation/hash stay stable over more than two normal 30-second reconciliation intervals. This detects observable churn. It does **not** prove the operator sent no redundant API UPDATE requests: unchanged writes may be suppressed by the API server. Use API audit evidence if that distinction matters.

Missing components, prerequisites, ownership, image IDs, expected policy, or readiness cause failure and a nonzero exit. JSON records context, CR name/UID/generation, checks, pod image IDs, and explicit unverified acceptance areas. `status: passed` refers only to requested operator observations; `acceptance: incomplete` remains explicit because these observations cannot establish full release readiness.

## Optional mutation checks in a disposable namespace

Mutation checks require `--allow-mutations` **and** an existing namespace labeled `pulse.ai/acceptance=disposable`. Do not label a production namespace to bypass this prerequisite. Stop active experiments/workflows before token rotation or Temporal changes. These checks briefly interrupt service access or Temporal's database access.

For a disposable test namespace, set the label through your normal reviewed cluster workflow:

```bash
oc --context YOUR_CONTEXT label namespace YOUR_NAMESPACE pulse.ai/acceptance=disposable
```

Then choose the checks explicitly:

```bash
python3 scripts/cluster-acceptance.py \
  --context YOUR_CONTEXT --namespace YOUR_NAMESPACE --pulse YOUR_CR \
  --execute --allow-mutations \
  --rotate-token --exercise-temporal-toggle \
  --network-probe-image YOUR_PYTHON_IMAGE@sha256:YOUR_64_HEX_DIGEST \
  --report /tmp/pulse-operator-mutation-observations.json
```

The image must contain `python3`, support an arbitrary non-root UID, and be available to the cluster. Use a tested digest; do not copy the placeholder literally. OpenShift SCC allocates the UID; the pod drops capabilities, disables privilege escalation, mounts no service-account token, and requests a read-only root filesystem. Image pull or scheduling failure is a failed prerequisite, never a successful denial test.

- **Packet probes:** creates short-lived probe pods with agent, Temporal, and unrelated label classes. Each opens a fresh TCP connection to PostgreSQL. Agent succeeds; Temporal succeeds only when enabled; unrelated labels must fail. This tests observed network behavior for label classes, not database authentication or actual worker workflow execution. Probe cleanup uses an atomic UID precondition in raw DeleteOptions so a replacement pod cannot be deleted by name.
- **Token rotation:** changes only the WS token key using UID/resourceVersion checks, observes a changed nginx hash and ready rollout, then restores the original token and waits for the original hash/rollout. Patch bodies travel over stdin, not process arguments. No token is logged or included in the report. This verifies propagation, not an authenticated browser session.
- **Temporal toggle:** requires Temporal initially enabled. Disables it, checks that its PostgreSQL policy peer is revoked, then restores enabled state and readiness. With the probe image it checks fresh Temporal-label connections in both states. It does not provision Temporal on an initially disabled installation or infer workflow durability from a ready Deployment.

Restoration runs in `finally`, but process termination, credential expiry, network failure, or concurrent configuration changes can prevent it. A restoration failure is a failed check. Inspect the supplied CR/token Secret and restore intended configuration through your normal operational procedure; the runner deliberately does not overwrite concurrent token or CR changes. Reports do not contain backup credentials.

## Runner verification and release evidence

```bash
python3 scripts/test_cluster_acceptance.py
```

These tests mock every `oc` operation. They verify fail-closed prerequisites, explicit targeting, policy matrix, exact-image prerequisites, safe reporting, patch stdin, UID cleanup, and restoration/error behavior. They prove runner behavior only; they are not cluster health evidence.

Before accepting a release, retain:

1. Exact matched agent/UI versions and pod `imageID`/digest evidence, operator version, cluster context and CR UID/generation.
2. UI isolated acceptance output from `pnpm run test:acceptance` in pulse-ui. This validates controlled frontend behavior; it does not replace browser/proxy tests on OpenShift.
3. Agent outcome/behavior gate output using the current pulse-agent acceptance instructions, including failed or unavailable provider runs. The provider-backed suite and runtime outcome checks are distinct from fixture dry-runs.
4. Actual operator runner reports, including packet probes in enabled and disabled Temporal states when testing that feature. Missing optional mutation checks remain missing evidence.
5. Authenticated UI/API, user RBAC/approval denial, verified writes/rollback in a disposable scope, Temporal approvals/persistence, and database recovery evidence. Those flows require separate end-to-end checks; this runner lists them as unverified.

Do not turn mocked tests, a policy manifest, pod readiness, or an `acceptance: incomplete` report into a claim that the full Pulse release is healthy.
