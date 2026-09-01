# Pulse Operator

[![CI](https://github.com/PulseSRE/pulse-operator/actions/workflows/ci.yml/badge.svg)](https://github.com/PulseSRE/pulse-operator/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/PulseSRE/pulse-operator)](go.mod)
[![OpenShift](https://img.shields.io/badge/OpenShift-4.12%2B-red)](https://www.redhat.com/en/technologies/cloud-computing/openshift)
[![Docs](https://img.shields.io/badge/docs-pulsesre.github.io-blue)](https://pulsesre.github.io/pulse-operator/)

A Kubernetes Operator that deploys and manages the complete [OpenShift Pulse](https://github.com/PulseSRE/pulse-agent) stack — AI SRE agent, React UI, PostgreSQL — from a **single custom resource** on OpenShift.

```
oc apply -f pulse.yaml   →   Running cluster AI assistant in ~5 minutes
```

---

## Table of Contents

- [What it does](#what-it-does)
- [Prerequisites](#prerequisites)
- [Install via OLM](#install-via-olm) ← recommended
- [Install via manifest](#install-via-manifest)
- [Create your first Pulse instance](#create-your-first-pulse-instance)
- [CR Spec reference](#cr-spec)
- [CR Status reference](#cr-status)
- [Upgrading](#upgrading)
- [Uninstall](#uninstall)
- [Development](#development)
- [OLM bundle / OperatorHub](#olm--operatorhub)
- [Architecture](#architecture)
- [Troubleshooting](#troubleshooting)
- [Security](#security)
- [Contributing](#contributing)
- [License](#license)

---

## What it does

One `OpenShiftPulse` CR drives the full lifecycle:

| Reconciler | Resources managed |
|---|---|
| **Agent** | ClusterRole (read-only cluster access), WS token Secret, memory PVC, Deployment, Service |
| **PostgreSQL** | StatefulSet (pg-data PVC retained on delete), pg-auth Secret (also retained — see below), ClusterIP + headless Services |
| **UI** | nginx ConfigMap, oauth-proxy Deployment (TLS on 8443), Service, Route, OAuthClient |
| **Monitoring** | ServiceMonitor (agent `/metrics`), PrometheusRule (`PulseAgentDown`, `PulsePostgreSQLDown`) |
| **MCP** | MCP server ServiceAccount + ClusterRole (read-only) + ClusterRoleBinding, Deployment, Service (optional, `spec.agent.mcp.enabled: true`) |
| **Network** | Per-component ingress-only NetworkPolicies: UI (OCP ingress + Prometheus), PostgreSQL (agent pod only), agent (UI pod + Prometheus), MCP server (agent pod only) |
| **Cluster detect** | Reads ingress domain, oauth-proxy image digest, ACM availability on first reconcile |

A `pulse.ai/cleanup` finalizer ensures ClusterRoles and OAuthClient are removed when the CR is deleted — no orphans on uninstall.

### PostgreSQL data survives CR deletion by design

The pg-data PVC (from the StatefulSet's `volumeClaimTemplates`) has no retention policy and is never deleted automatically, and the `{name}-pg-auth` credentials Secret has no `OwnerReference` for the same reason — postgres only runs `initdb` (which bakes a password into `PGDATA`) on an empty data directory, so if the Secret were garbage-collected with the CR, recreating a CR with the same name would generate a fresh random password that could never match the already-initialized data on the retained volume, leaving the agent permanently unable to authenticate with no self-heal short of manually deleting the PVC. Retaining both together means recreating a CR with the same name transparently reuses the matching credentials.

If you actually want a full teardown (delete the data and credentials, not just the CR), annotate the CR **before** deleting it:

```bash
oc annotate openshiftpulse pulse -n openshiftpulse pulse.ai/delete-data=true
oc delete openshiftpulse pulse -n openshiftpulse
```

Without this annotation, `{name}-pg-auth` and the pg-data PVC are left behind after `oc delete openshiftpulse pulse` — this is intentional, not a leak.

---

## Versioning

### Agent/UI version skew

The agent and UI ship as a pair under one version number, so the operator
compares the two tags pinned in the CR and reports the verdict as an
`AgentUIVersionsMatch` status condition:

```bash
oc get openshiftpulse pulse -n openshiftpulse \
  -o jsonpath='{.status.conditions[?(@.type=="AgentUIVersionsMatch")]}{"\n"}'
```

This compares the two pinned tags against each other, not against the newest
published release — that needs no network call in the reconcile path, stays
deterministic, and does not nag clusters that have deliberately stayed on an
older version. It reports and never blocks: an upgrade patches the two images
moments apart, and blocking on the resulting transient mismatch would wedge the
very rollout that clears it. Digest pins, `latest`, and unset images all report
no skew rather than guessing.

### Why the operator's version differs

The operator versions independently of the Pulse application it deploys. As of
this release the operator is **v0.7.0** while the agent and UI ship **v2.27.0**
— that gap is deliberate, not drift:

- The operator's version tracks *its own* API and reconcile behaviour. The CRD
  is still `v1alpha1`, and a 0.x version says so honestly.
- OLM upgrade graphs are monotonic. Folding the operator into the application's
  2.x stream would be irreversible, and would force an operator release, bundle
  rebuild, catalog render and a cluster-wide OLM upgrade for every application
  patch — including releases that change nothing in the operator.
- The application version is already carried explicitly, per instance, in
  `spec.agent.image` on the OpenShiftPulse CR. That is the field to look at to
  answer "which Pulse am I running", and it is deliberately decoupled from the
  operator that reconciles it.

`OperatorVersion` in `internal/controller/compat.go` is the operator build's own
version and must be bumped with each release; `version_guard_test.go` fails the
build if it falls behind the latest git tag.

## Prerequisites

> **OpenShift only — this will not install on vanilla Kubernetes.** The operator
> creates `Route` objects for ingress, `OAuthClient` objects for single sign-on, and
> reads `config.openshift.io/v1 Ingress` to discover the cluster's application
> domain. There is no Ingress fallback and no capability detection, so on a cluster
> without those APIs the reconcile fails rather than degrading to something usable.
> Supporting plain Kubernetes would mean an Ingress path and a different auth story;
> that work has not been done.

- OpenShift 4.12+ (uses Route, OAuthClient, ImageStream APIs)
- `oc` CLI with `cluster-admin`
- Prometheus Operator (for `ServiceMonitor` / `PrometheusRule` — ships with OpenShift Monitoring)
- One of: **Vertex AI** GCP service account key, or **Anthropic API key**

---

## Install via OLM

**This README is the canonical install guide for all of Pulse.** The
[pulse-agent](https://github.com/PulseSRE/pulse-agent) and
[pulse-ui](https://github.com/PulseSRE/pulse-ui) repos link here rather than
restating the steps, so this is the only copy that has to stay correct.

This is the recommended path. It installs the operator through OLM so it appears in the OpenShift Installed Operators view and receives automatic upgrades.

### 1. Add the CatalogSource

```bash
cat <<EOF | oc apply -f -
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: pulse-operator-catalog
  namespace: openshift-marketplace
spec:
  sourceType: grpc
  image: quay.io/amobrem/pulse-operator-catalog:latest
  displayName: Pulse Operator
  publisher: Red Hat CoE
  updateStrategy:
    registryPoll:
      interval: 10m
EOF
```

Wait for it to become ready:
```bash
oc get catalogsource pulse-operator-catalog -n openshift-marketplace -w
# STATE should reach READY within ~30 seconds
```

> **Private quay.io namespaces:** `quay.io/amobrem/pulse-operator-catalog`
> and `pulse-operator-bundle` are public, so this doesn't apply to using this
> repo as-is — but if you fork this and publish to a *private* namespace
> instead, read on. A private catalog repository means the cluster's nodes
> cannot pull it directly and the catalog pod fails with `ImagePullBackOff`.
> Worse, even once the catalog image itself is reachable, its content still
> embeds a reference to the **bundle** image (`quay.io/.../pulse-operator-bundle:*`)
> as `bundlePath` — if *that* is also private, `oc get csv` shows
> `Succeeded` misleadingly quickly while the underlying Subscription hangs
> forever on `BundleUnpacking: UnpackingInProgress` with no error logged,
> because OLM can't pull the bundle image it's unpacking either. Simplest
> fix: make both repositories public, matching the operator/agent/UI images
> (Repository Settings → Visibility → Make Public in the quay.io UI). If
> that's not an option, the reliable fallback is to mirror **both** images
> into the cluster's own internal registry (via its public route) and
> re-render the catalog to reference the
> internal path instead of quay.io for `bundlePath` — see
> [Build the catalog image](#build-the-catalog-image) for the exact steps.

### 2. Create the target namespace

```bash
oc new-project openshiftpulse
```

### 3. Create your AI backend secret

**Vertex AI (GCP):**
```bash
oc create secret generic gcp-sa-key \
  --from-file=key.json=/path/to/sa-key.json \
  -n openshiftpulse
```

**Anthropic API:**
```bash
oc create secret generic anthropic-api-key \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-... \
  -n openshiftpulse
```

### 4. Create OperatorGroup and Subscription

The operator's own controller runs in `pulse-operator-system` (matching the
manifest-install path below) — separate from `openshiftpulse`, which holds
the CR and everything it manages. Since the CSV supports only the
`AllNamespaces` install mode, the operator watches `OpenShiftPulse` CRs
cluster-wide regardless of which namespace its own controller runs in.

```bash
oc new-project pulse-operator-system

cat <<EOF | oc apply -f -
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: pulse-operator-group
  namespace: pulse-operator-system
spec:
  targetNamespaces: []
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: pulse-operator
  namespace: pulse-operator-system
spec:
  channel: alpha
  name: pulse-operator
  source: pulse-operator-catalog
  sourceNamespace: openshift-marketplace
  installPlanApproval: Automatic
EOF
```

Watch the CSV reach `Succeeded`:
```bash
oc get csv -n pulse-operator-system -w
```

Then skip to [Create your first Pulse instance](#create-your-first-pulse-instance).

---

## Install via manifest

For environments without OLM or for quick testing:

```bash
# Install CRD
oc apply -f https://raw.githubusercontent.com/PulseSRE/pulse-operator/main/config/crd/bases/pulse.ai_openshiftpulses.yaml

# Deploy operator
oc apply -f https://raw.githubusercontent.com/PulseSRE/pulse-operator/main/deploy/operator.yaml
oc rollout status deployment/pulse-operator-manager -n pulse-operator-system
```

Or clone and use make:
```bash
git clone https://github.com/PulseSRE/pulse-operator
cd pulse-operator
make deploy
```

---

## Create your first Pulse instance

```bash
cat <<EOF | oc apply -f -
apiVersion: pulse.ai/v1alpha1
kind: OpenShiftPulse
metadata:
  name: pulse
  namespace: openshiftpulse
spec:
  vertexAI:
    projectId: my-gcp-project
    region: us-east5
    credentialSecret: gcp-sa-key
  agent:
    image: quay.io/amobrem/pulse-agent:latest
    trustLevel: 2
    mcp:
      enabled: true
  ui:
    image: quay.io/amobrem/openshiftpulse:latest
    replicas: 2
  database:
    storageSize: 5Gi
  monitoring:
    enabled: true
EOF
```

Watch the stack come up:
```bash
oc get openshiftpulse pulse -n openshiftpulse -w
```

```
NAME    PHASE        AGENTHEALTHY   DBREADY   UIAVAILABLE   ROUTEHOST
pulse   Installing   false          false     false
pulse   Installing   false          true      false
pulse   Installing   true           true      false
pulse   Running      true           true      true          pulse-openshiftpulse.apps.cluster.example.com
```

Open the Route host in a browser — you'll authenticate through OpenShift OAuth and land in the Pulse UI.

```bash
oc get route -n openshiftpulse -o jsonpath='{.items[0].spec.host}'
```

---

## CR Spec

```yaml
spec:
  # ── AI Backend ─────────────────────────────────────────────────────────────
  # Exactly one of vertexAI or anthropicApiKey is required.

  vertexAI:
    projectId: my-gcp-project          # GCP project ID
    region: us-east5                   # Vertex AI region
    credentialSecret: gcp-sa-key       # Secret with key.json

  anthropicApiKey:
    existingSecret: anthropic-api-key  # Secret with ANTHROPIC_API_KEY

  # ── Agent ───────────────────────────────────────────────────────────────────
  agent:
    image: quay.io/amobrem/pulse-agent:latest
    trustLevel: 2          # 0=observe · 1=suggest · 2=confirm · 3=batch · 4=autonomous
    allowWriteOperations: false   # adds delete(pods), patch(deployments) to agent ClusterRole
    allowSecretAccess: false      # adds get/list/watch(secrets) to agent ClusterRole
    resources: {}                 # corev1.ResourceRequirements
    mcp:
      enabled: false       # deploys MCP server sidecar for tool extension
    minOperatorVersion: ""  # optional semver floor for this operator build; unset (default) = inert — see "Agent-version compatibility gate" below

  # ── UI ──────────────────────────────────────────────────────────────────────
  ui:
    image: quay.io/amobrem/openshiftpulse:latest
    replicas: 2
    oauthProxyImage: ""    # auto-detected from cluster ImageStream when empty
    resources: {}

  # ── Database ─────────────────────────────────────────────────────────────────
  database:
    storageSize: 5Gi       # PVC size; cannot shrink after creation
    storageClass: ""       # cluster default if empty
    image: ""              # RHEL9 postgresql-15 if empty

  # ── Monitoring ───────────────────────────────────────────────────────────────
  monitoring:
    enabled: true          # creates ServiceMonitor + PrometheusRule alerts
```

### Trust levels

| Level | Behaviour |
|---|---|
| `0` — observe | Read-only. Agent answers questions but takes no action. |
| `1` — suggest | Proposes actions in the UI, user approves each one. |
| `2` — confirm | Default. Agent executes after a single user confirmation. |
| `3` — batch | Executes batches of low-risk actions with one confirmation. |
| `4` — autonomous | Executes without confirmation. Use with caution. |

---

### Durable plan execution (spec.temporal)

`spec.temporal.enabled: true` provisions a Temporal server (`{name}-temporal`,
`temporalio/auto-setup` pinned) backed by the operator's own PostgreSQL — the
`temporal` and `temporal_visibility` databases are created in the same
instance on first start — and injects `PULSE_AGENT_TEMPORAL_HOST` into the
agent, which enables its durable plan-run endpoints (agent docs/TEMPORAL.md).
This is the dev-grade single-container topology; a production topology
(separated services, dedicated visibility store) is a deliberate later step.

**Enabling on an existing install:** the CREATEDB grant that lets Temporal
create its databases ships as a postgresql-start script, but the PostgreSQL
pod template is create-only — fresh installs get it automatically, existing
installs need the one-time grant:

```bash
oc exec -n <ns> <name>-openshift-sre-agent-postgresql-0 -- \
  sh -c 'psql -U postgres -c "ALTER USER \"$POSTGRESQL_USER\" CREATEDB;"'
```

## CR Status

```yaml
status:
  phase: Running                       # Installing | Running | Upgrading | Degraded
  agentVersion: v2.27.0                # tag portion of the running spec.agent.image
  agentHealthy: true                   # agent Deployment has ≥1 ready replica
  databaseReady: true                  # PostgreSQL StatefulSet has ≥1 ready replica
  uiAvailable: true                    # Route hostname assigned by OCP router
  routeHost: pulse-openshiftpulse.apps.cluster.example.com
  observedGeneration: 3
  upgradeStartedAt: null               # set while an image differs from the last known-healthy one; cleared when healthy again or after auto-rollback
  lastHealthyAgentImage: quay.io/amobrem/pulse-agent:v2.9.0     # rollback target — see "Automatic rollback" below
  lastHealthyUIImage: quay.io/amobrem/openshiftpulse:e6169a4
  lastUpgradeDurationSeconds: 0   # how long the most recently completed agent/UI upgrade took to become healthy; 0/absent if none has happened yet — see "Agent / UI image upgrades" below
  conditions:
  - type: Ready              # aggregate — kept for backward compatibility
    status: "True"
    reason: AllComponentsHealthy
  - type: AgentReady
    status: "True"
    reason: Ready
  - type: DatabaseReady
    status: "True"
    reason: Ready
  - type: UIReady
    status: "True"
    reason: Ready
  - type: Progressing        # True while Installing/Upgrading; False (Stable/Degraded) otherwise
    status: "False"
    reason: Stable
  - type: AgentUIVersionsMatch  # agent and UI image tags agree — see "Agent / UI image skew" below
    status: "True"
    reason: VersionsMatch
  # - type: AgentVersionCompatible   # only present once spec.agent.minOperatorVersion is set — see "Agent-version compatibility gate" below
  #   status: "True"
  #   reason: Compatible
```

`Phase: Upgrading` is distinct from `Degraded`: it means `spec.agent.image`/`spec.ui.image` was
changed and the new image isn't ready *yet*, not that something broke. The operator only reports
`Degraded` for a component that was previously healthy and is no longer — not for an upgrade in
progress. See **Automatic rollback** below for what happens if an upgrade never becomes healthy.

### Self-heal Events and metrics

The operator emits `Normal SelfHealed` Events (`oc get events` / `oc describe openshiftpulse`)
whenever it takes one of its own corrective actions: deleting a PostgreSQL pod stuck `Pending` for
over 2 minutes, deleting an **agent or UI Deployment pod** stuck `Pending` too long or definitively
failing with `ImagePullBackOff`/`CrashLoopBackOff` (no grace period needed for those two — the
kubelet only ever reports them after a real failed pull or start-then-exit attempt, so there's no
"might just be mid-startup" ambiguity to wait out), recreating a selector-mismatched
StatefulSet/Deployment (e.g. adopting a previously Helm-managed instance), correcting Route drift,
or regenerating a malformed `cookie-secret`. The agent/UI case matters most for the agent
specifically, whose Deployment uses the `Recreate` strategy (see **Agent / UI image upgrades**
below): `Recreate` tears the old pod down before the new one starts, so a bad rollout has no old
pod left to fall back to without this — previously it stayed stuck until a human noticed and ran
`oc delete pod` by hand.

It also exposes five Prometheus metrics on the manager's `:8082/metrics` endpoint:

- `pulse_operator_self_heal_actions_total{component,action}` — counts of the actions above.
- `pulse_operator_component_ready{namespace,name,component}` — 1/0 mirror of the AgentReady/DatabaseReady/UIReady conditions.
- `pulse_operator_reconcile_errors_total{step}` — reconcile failures by step (e.g. `postgres`, `agent`, `ui`, `status_update`).
- `pulse_operator_observed_memory_bytes{namespace,name,component}` — real memory usage observed via `metrics.k8s.io` (max across replicas), for comparing against the request below. Advisory only — the operator never patches `resources.requests` from this. Absent (not zero) when `metrics.k8s.io` has no sample yet, e.g. no metrics-server, or the pod isn't `Running`; sampled at most once every 5 minutes per component to keep load off the metrics API.
- `pulse_operator_requested_memory_bytes{namespace,name,component}` — the effective `resources.requests.memory` currently applied to that component's container (spec override or built-in default).

### Agent-version compatibility gate

`spec.agent.minOperatorVersion` (see **CR Spec** above) is an optional, admin-set semver floor —
left unset, the default for every existing CR, this gate is completely inert. When set, the
operator compares it against its own running version on every agent reconcile. If this operator
build doesn't satisfy the constraint, it records an `AgentVersionCompatible` condition
(`status: "False"`, `reason: IncompatibleVersion`) on the CR, emits a `Warning IncompatibleVersion`
Event, and pins the agent Deployment to whichever image is already running rather than applying
the requested `spec.agent.image` change — or, if there's no Deployment yet to pin, refuses to
create one and requeues instead of erroring. A malformed version string on either side fails
*open* (treated as compatible) rather than blocking a real deployment on a typo the CRD doesn't
itself validate. This is checked against **this specific operator build's own version**, not the
agent's — it exists to catch an operator upgrade running ahead of (or behind) an agent image that
expects a newer API/DB schema, the axis the agent's own `/version` UI↔agent protocol check
(documented in `pulse-agent`) doesn't cover.

---

## Upgrading

### Operator upgrade (OLM)

OLM handles upgrades automatically when `installPlanApproval: Automatic`. To upgrade manually:

```bash
# Check available versions
oc get packagemanifest pulse-operator -n openshift-marketplace \
  -o jsonpath='{range .status.channels[*]}{.name}{": "}{.currentCSV}{"\n"}{end}'

# Approve a pending install plan
oc get installplan -n openshiftpulse
oc patch installplan <name> -n openshiftpulse \
  --type=merge -p '{"spec":{"approved":true}}'
```

### Agent / UI image upgrades

Update the image tag in the CR — the operator rolls out the new image immediately:

```bash
oc patch openshiftpulse pulse -n openshiftpulse --type=merge \
  -p '{"spec":{"agent":{"image":"quay.io/amobrem/pulse-agent:v2.0.0"}}}'
```

`status.phase` reports `Upgrading` while the new image is rolling out (see **CR Status** above).
Once the new image becomes healthy again, `status.lastUpgradeDurationSeconds` records how long
that outage window actually took, and holds its previous value between upgrades (`0` before the
first one ever completes).

The agent Deployment uses the `Recreate` strategy, not `RollingUpdate` — a deliberate choice, not
an unexamined default. Its memory-cache PVC is `ReadWriteOnce`, and `pulse-agent`'s own Helm chart
runs `Recreate` for the identical reason (`chart/values.yaml`: *"Required because the memory PVC
is ReadWriteOnce (RWO) and cannot be mounted by two pods simultaneously"*) — a `RollingUpdate`
overlap here would leave the surging pod's volume attach stuck `Pending`
(`FailedAttachVolume`), arguably a worse failure mode than today's brief, bounded stop-then-start
outage. The agent also runs forward-only, no-rollback DB migrations automatically on startup, so
two concurrent agent versions sharing one Postgres instance is a second, independent reason to
avoid the overlap. `lastUpgradeDurationSeconds` exists to make that outage window's size visible
and measured, not to eliminate it — see the self-heal coverage above for what happens if the new
pod gets stuck instead of just taking a while.

#### Automatic rollback

If the new agent/UI image doesn't become healthy within 5 minutes of `Phase` turning `Upgrading`,
the operator automatically reverts `spec.agent.image`/`spec.ui.image` back to
`status.lastHealthy{Agent,UI}Image` — the last image that was actually observed ready — and emits
a `Warning AutoRolledBack` Event explaining what happened. This only covers the agent/UI images
managed directly by this CR; it does **not** cover an operator upgrade via OLM (rolling back the
operator's own running binary needs a separate pre-upgrade gate outside this process, which isn't
implemented yet).

---

## Uninstall

```bash
# Delete CR — operator finalizer cleans up ClusterRoles and OAuthClient
oc delete openshiftpulse pulse -n openshiftpulse

# Remove operator (OLM install) — use scripts/olm-uninstall.py instead of just
# these two deletes if you skipped the CR deletion above; see that script's
# docstring for why (this order is safe specifically because the CR is
# already gone by this point).
oc delete subscription pulse-operator -n pulse-operator-system
oc delete csv -n pulse-operator-system -l operators.coreos.com/pulse-operator.pulse-operator-system=
oc delete catalogsource pulse-operator-catalog -n openshift-marketplace

# Or for manifest install
make undeploy

# Remove namespace
oc delete namespace openshiftpulse
```

> **Data retention:** PostgreSQL PVCs are retained by the StatefulSet `volumeClaimTemplate` lifecycle and survive operator deletion. Re-create the CR to reattach them.

### Uninstalling *other* OLM operators cleanly

The steps above work for pulse-operator specifically because deleting its CR
first (via the `pulse.ai/cleanup` finalizer) tears down everything it owns
before the operator itself goes away. Manually running just
`oc delete subscription` + `oc delete csv` for some *other* operator skips
that step entirely — it removes the controller but leaves any custom
resources (and everything those CRs caused to be created) running forever,
orphaned. OLM also never deletes CRDs on uninstall, by design.

[`scripts/olm-uninstall.py`](scripts/olm-uninstall.py) does this properly for
any OLM-installed operator: it reads the CSV's own
`spec.customresourcedefinitions.owned[]` to find every CR instance the
operator manages, deletes those first, then removes the Subscription, CSV,
and any stale InstallPlans.

```bash
scripts/olm-uninstall.py authorino            # dry run — shows what would be deleted
scripts/olm-uninstall.py authorino --yes      # actually uninstall
```

---

## Development

### Prerequisites

```bash
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
setup-envtest use 1.31 --bin-dir /tmp/kubebuilder-bin
```

### Run tests

```bash
KUBEBUILDER_ASSETS=/tmp/kubebuilder-bin/k8s/1.31.0-darwin-arm64 make test
```

### Run locally against a live cluster

```bash
# Install CRD
oc apply -f config/crd/bases/pulse.ai_openshiftpulses.yaml

# Run operator process (skip leader election)
# --zap-devel switches to human-readable console logs; the production default
# (used by the shipped Deployment) is structured JSON.
go run ./cmd/main.go \
  --leader-elect=false \
  --metrics-bind-address=:9191 \
  --health-probe-bind-address=:9292 \
  --zap-devel

# Apply a CR in another terminal
oc apply -f examples/pulse.yaml
```

**Gotchas:**
- Cache warmup takes 60–90 s over WAN. The first `Reconciling OpenShiftPulse` log line confirms the controller is active.
- gp3-csi PVC provisioning takes 2–5 min. The agent Deployment is intentionally gated behind a `Bound` PVC check.
- Build for `linux/amd64` explicitly when on Apple Silicon: `make docker-build` passes `--platform linux/amd64` automatically.

### Build and push the operator image

```bash
make docker-build docker-push OPERATOR_IMG=quay.io/yourorg/pulse-operator:dev
```

`CONTAINER_TOOL` defaults to `podman`. Override with `CONTAINER_TOOL=docker` if needed.

### Regenerate the CRD and deepcopy code after API changes

```bash
make manifests generate
```

`manifests` regenerates `config/crd/bases/pulse.ai_openshiftpulses.yaml` and `generate` regenerates `api/v1alpha1/zz_generated.deepcopy.go`, both via `controller-gen` (auto-installed into `bin/` on first run). After `manifests` changes the CRD, also run `make bundle` to re-sync `bundle/manifests/pulse.ai_openshiftpulses.yaml` — CI's `rbac-and-crd-sync` job fails the build if either copy drifts from what these targets would produce, or if `zz_generated.deepcopy.go` drifts from `api/v1alpha1/types.go`.

RBAC is *not* regenerated by these targets: `config/rbac/role.yaml` is hand-curated (it includes several OpenShift-specific API groups that don't come from `+kubebuilder:rbac` markers) and is the source of truth for the two generated copies in `deploy/operator.yaml` and the CSV — see [`scripts/sync-rbac.py`](scripts/sync-rbac.py)'s docstring, and run `scripts/sync-rbac.py --fix` after editing it.

---

## OLM / OperatorHub

### Build the catalog image

The catalog image is a File-Based Catalog (FBC) served by `opm`. This is **not**
automated by CI (see [Releasing a new version](#releasing-a-new-version) below) —
rebuild and push it manually after bundle changes:

```bash
# 1. Push the bundle image first (see the release process below) — the next
#    step needs to render FROM it, not from the local bundle/ directory.
#    Rendering from a bare local directory (`opm render /bundle`) produces an
#    olm.bundle entry with an EMPTY `image:`/bundlePath field, since there's
#    no registry reference for a directory that was never pushed anywhere —
#    OLM then can't ever unpack it. Always render from the pushed image.
BUNDLE_IMG=quay.io/amobrem/pulse-operator-bundle:v0.2.0

mkdir -p catalog
podman run --rm quay.io/operator-framework/opm:latest \
  render "$BUNDLE_IMG" -o yaml > /tmp/catalog.yaml

# 2. Prepend package + channel declarations (update the version to match).
cat - /tmp/catalog.yaml > catalog/full-catalog.yaml <<'HEADER'
---
defaultChannel: alpha
name: pulse-operator
schema: olm.package
---
entries:
- name: pulse-operator.v0.2.0
  replaces: pulse-operator.v0.1.0
name: alpha
package: pulse-operator
schema: olm.channel
HEADER

# 3. Build catalog image (linux/amd64 — cluster nodes are x86). Dockerfile.catalog
#    pre-populates opm's serve cache at build time — required, or the container
#    fails at startup with "integrity check failed: read existing cache digest".
podman build --platform linux/amd64 \
  -f Dockerfile.catalog \
  -t quay.io/amobrem/pulse-operator-catalog:latest .
podman push quay.io/amobrem/pulse-operator-catalog:latest

rm -rf catalog  # generated — not committed (see .gitignore)
```

> **No registry credentials for your quay.io namespace on the cluster?**
> Mirror both images into the cluster's own internal registry via its public
> route instead of relying on quay.io being reachable/public:
> ```bash
> REGISTRY=$(oc get route default-route -n openshift-image-registry -o jsonpath='{.spec.host}')
> TOKEN=$(oc whoami -t)
> podman login -u kubeadmin -p "$TOKEN" "$REGISTRY" --tls-verify=false
>
> for image in bundle catalog; do
>   podman tag "quay.io/amobrem/pulse-operator-$image:v0.2.0" \
>     "$REGISTRY/pulse-operator-catalog/$image:v0.2.0"
>   podman push --tls-verify=false "$REGISTRY/pulse-operator-catalog/$image:v0.2.0"
> done
> ```
> Then render step 1 from `$REGISTRY/pulse-operator-catalog/bundle:v0.2.0`
> instead (so `bundlePath` in the rendered YAML points somewhere the cluster
> can actually pull from), and use the internal `catalog` tag —
> `image-registry.openshift-image-registry.svc:5000/pulse-operator-catalog/catalog:v0.2.0`
> — as the CatalogSource's `spec.image` in [step 1 of Install via OLM](#1-add-the-catalogsource).
> This mirroring only needs cluster-admin `oc` access, not registry credentials.

### Validate the bundle

```bash
make bundle-validate
```

### Releasing a new version

Tag to trigger CI publishing:

```bash
git tag v0.2.0 -m "v0.2.0: ..."
git push origin v0.2.0
```

CI ([`.github/workflows/release.yml`](.github/workflows/release.yml)) builds and pushes:
- `quay.io/amobrem/pulse-operator:v0.2.0` + `:latest` (the operator image, from `Dockerfile`)
- `quay.io/amobrem/pulse-operator-bundle:v0.2.0` + `:latest` (the OLM bundle, from `Dockerfile.bundle`)

Before the bundle image is built, CI runs [`scripts/bump-bundle-version.py`](scripts/bump-bundle-version.py) against the pushed tag, which updates the CSV's `metadata.name`, `spec.version`, `metadata.annotations.containerImage`, and the manager Deployment's `image` to match — and sets `spec.replaces` to whatever CSV name was current before the bump, extending the upgrade graph by one step. This runs in CI only; the version bump is never committed back to `main`, so `bundle/manifests/pulse-operator.clusterserviceversion.yaml` on `main` always reflects the most recently *released* version, not a preview of the next one.

The **catalog** image (`pulse-operator-catalog`, used by the CatalogSource in
[Install via OLM](#install-via-olm)) is *not* built by this workflow — publish it
manually via [Build the catalog image](#build-the-catalog-image) above after
tagging a release.

Requires `QUAY_USERNAME` and `QUAY_TOKEN` as GitHub repository secrets.

---

## Architecture

```
pulse-operator-system/
└── pulse-operator-manager   (Deployment — 1 replica, no leader election)
    └── OpenShiftPulseReconciler
        ├── pulse.ai/cleanup finalizer  ← removes cluster-scoped resources on CR delete
        │
        ├── AgentReconciler
        │   ├── {ns}/{name}-openshift-sre-agent  (ServiceAccount)
        │   ├── {ns}-{name}-openshift-sre-agent  (ClusterRole + ClusterRoleBinding — cluster-scoped, namespace-qualified)
        │   ├── {ns}-{name}-openshift-sre-agent-monitoring-view  (ClusterRoleBinding → built-in cluster-monitoring-view, for the agent's own Thanos-querier reads)
        │   ├── {ns}/{name}-ws-token             (Secret — random 32-char hex, never rotated)
        │   ├── {ns}/{name}-openshift-sre-agent-memory  (PVC 1Gi RWO — gated before Deployment)
        │   └── {ns}/{name}-openshift-sre-agent  (Deployment + Service :8080)
        │
        ├── PostgreSQLReconciler
        │   ├── {ns}/{name}-pg-auth              (Secret — POSTGRESQL_* RHSCL env vars)
        │   ├── {ns}/{name}-openshift-sre-agent-postgresql  (StatefulSet + pg-data PVC)
        │   └── {ns}/{name}-openshift-sre-agent-postgresql[-headless]  (Services)
        │
        ├── UIReconciler
        │   ├── {ns}/{name}-openshiftpulse       (ServiceAccount)
        │   ├── {ns}-{name}-openshiftpulse-reader  (ClusterRole + ClusterRoleBinding [+ -auth-delegator] — cluster-scoped)
        │   ├── {ns}/{name}-oauth-secrets        (Secret — client-secret + cookie-secret)
        │   ├── {ns}/{name}-nginx                (ConfigMap — nginx.conf, root /opt/app-root/src)
        │   ├── {ns}/{name}-openshiftpulse       (Deployment: nginx + oauth-proxy sidecars)
        │   ├── {ns}/{name}-openshiftpulse       (Service :8443)
        │   ├── {ns}/{name}-openshiftpulse       (Route — reencrypt, OCP assigns hostname)
        │   └── openshiftpulse-{ns}-{name}       (OAuthClient — cluster-scoped, redirect URI auto-set)
        │
        ├── MonitoringReconciler  [spec.monitoring.enabled]
        │   ├── {ns}/{name}-openshift-sre-agent  (ServiceMonitor → /metrics)
        │   └── {ns}/{name}-openshiftpulse       (PrometheusRule — 3 alert rules)
        │
        ├── MCPReconciler  [spec.agent.mcp.enabled]
        │   ├── {ns}/{name}-mcp-server           (ServiceAccount + ClusterRole + ClusterRoleBinding, read-only)
        │   ├── {ns}/{name}-mcp-server           (Deployment + Service :8081)
        │   └── {ns}/{name}-mcp-server           (NetworkPolicy — ingress from the agent pod only)
        │
        └── NetworkPolicyReconciler
            ├── {ns}/{name}-openshiftpulse       (UI: ingress from OCP router + Prometheus only)
            ├── {ns}/{name}-pg-access            (PG: ingress from the agent pod only)
            └── {ns}/{name}-agent-access         (Agent: ingress from the UI pod + Prometheus only)
```

### Cluster auto-detection

On the first reconcile, `ClusterDetector` reads:
- `ingresses.config.openshift.io/cluster` → wildcard app domain for Route host
- `openshift/oauth-proxy` ImageStream → correct proxy image digest for this OCP version
- `open-cluster-management-observability` namespace → ACM multicluster availability

Results are cached (`sync.Once`) — zero API calls on subsequent reconciles.

---

## Troubleshooting

### OAuth error on first browser visit

**Symptom:** `The authorization server encountered an unexpected condition`

**Cause:** The ServiceAccount used by oauth-proxy needs an OCP-specific annotation declaring the valid redirect URI.

```bash
ROUTE=$(oc get route -n openshiftpulse -o jsonpath='{.items[0].spec.host}')
oc annotate sa pulse-openshiftpulse -n openshiftpulse \
  "serviceaccounts.openshift.io/oauth-redirecturi.pulse=https://$ROUTE" \
  --overwrite
oc delete pods -n openshiftpulse -l app=pulse-openshiftpulse
```

### UI shows nginx test page instead of Pulse

**Symptom:** Default nginx welcome page at the route URL.

**Cause:** Stale ConfigMap or nginx not pointing at `/opt/app-root/src`. Force reconcile:

```bash
oc delete configmap pulse-nginx -n openshiftpulse
# Operator recreates it within seconds
```

### Agent CrashLoopBackOff — Solr/MCP unreachable

**Symptom:** Agent pod crashes with `MCP not ready (attempt N/36)`

**Cause:** MCP server is enabled in spec but its Service doesn't exist yet (first deployment race).

```bash
oc get pods -n openshiftpulse
oc logs -n openshiftpulse <agent-pod> -c agent | tail -20
# Wait for mcp-server pod to become Ready, agent restarts automatically
```

### PostgreSQL pod not starting

**Cause:** The PVC may not be bound yet, or a previous Helm-managed StatefulSet had incompatible labels. The operator will detect the mismatch and delete/recreate the StatefulSet automatically. PostgreSQL data on the PVC is preserved.

```bash
oc get pvc -n openshiftpulse
oc describe statefulset -n openshiftpulse
```

### Agent restarting with exit code 137 and no errors in its own logs

**Symptom:** `oc get pod -o jsonpath='{...lastState}'` shows `"reason":"Error","exitCode":137`, and `oc get events` shows `Liveness probe failed: ... context deadline exceeded` — but the agent's own logs show nothing wrong, and `oc adm top pod` shows normal CPU/memory usage.

**Cause:** The agent deliberately has no CPU request (see `agentResources` in [`agent_reconciler.go`](internal/controller/agent_reconciler.go)), so it gets minimal scheduling priority. On a cluster where other pods on the same node are bursting CPU, the agent's process can occasionally take longer than the probe timeout to answer `/healthz` even though it's healthy — the probes now use a 5s timeout (was the Kubernetes default of 1s) to tolerate that. If restarts continue after upgrading, check node-level CPU contention (`oc adm top nodes`, `oc describe node <node>` → Allocated resources) rather than the agent itself.

### Agent inbox shows "Prometheus monitoring degraded" / "Trend monitoring degraded"

**Cause:** The agent's alert-scanning queries hit `thanos-querier` directly using its own ServiceAccount token; that read is gated by binding to the built-in `cluster-monitoring-view` ClusterRole, not by anything addable to the agent's own ClusterRole. If this binding is missing (e.g. a CR created before this was added), a normal reconcile creates it — force one with:

```bash
oc get clusterrolebinding | grep monitoring-view
# If absent, touch an annotation to trigger reconcile, or restart the operator pod
```

### UI Alerts view shows "Unable to reach alerting backend"

**Cause:** The Alerts view reads firing alerts/rules from `/api/prometheus/` (Thanos-querier) but silences from a separate `/api/alertmanager/` proxy — a pre-existing gap where that location didn't exist in nginx at all, so requests fell through to the SPA's own `index.html` (200 OK, `text/html`) instead of reaching Alertmanager. The UI correctly detected the non-JSON response and reported the backend as unreachable, even though Prometheus itself was fine.

```bash
# Confirm the proxy exists in the live ConfigMap
oc get configmap {name}-nginx -n <namespace> -o jsonpath='{.data.nginx\.conf}' | grep -A5 'location /api/alertmanager/'
```

If it's missing, the operator image predates this fix — upgrade and restart the UI pods. Also confirm the logged-in user holds `monitoring-alertmanager-view` (or `-edit`) in `openshift-monitoring`; `cluster-monitoring-view` alone (which covers the Thanos path) is not sufficient for silences.

### Migrating from Helm install

The operator cannot adopt Helm-managed Deployments/StatefulSets because their label selectors are immutable. The operator detects selector mismatches and replaces the resources automatically (StatefulSet with orphan cascade to preserve the PVC). If the agent Deployment is stuck:

```bash
# Manually delete the Helm-managed deployment; operator recreates it cleanly
oc delete deployment pulse-openshift-sre-agent -n openshiftpulse
```

### CatalogSource stuck in TRANSIENT_FAILURE

Despite showing `TRANSIENT_FAILURE`, if `PackageManifest` is visible the catalog is functional:

```bash
oc get packagemanifest pulse-operator -n openshift-marketplace
```

If the pod itself is crashing:
```bash
oc logs -n openshift-marketplace -l olm.catalogSource=pulse-operator-catalog
```

Common causes: wrong image architecture (build with `--platform linux/amd64`), missing `grpc_health_probe` binary in the catalog image.

---

## Security

See [SECURITY.md](SECURITY.md) to report a vulnerability.

- All managed pods run as non-root with `AllowPrivilegeEscalation=false`, `Capabilities.Drop=ALL`, and `SeccompProfile=RuntimeDefault`.
- PostgreSQL sets `ReadOnlyRootFilesystem=false` (PG requires writable socket and temp paths).
- The operator's own ClusterRole ([`config/rbac/role.yaml`](config/rbac/role.yaml)) does **not** include `escalate`/`bind` on RBAC resources — every rule it ever writes into a generated agent/UI/MCP ClusterRole is already a permission it holds itself, so Kubernetes' RBAC "you already have this" rule lets `create`/`update` succeed without those verbs. It's still a privilege-concentration point (it *creates* ClusterRoles/ClusterRoleBindings for every managed instance): restrict exec access to `pulse-operator-system` via NetworkPolicy.
- The agent, UI, PostgreSQL, and MCP server pods each get their own NetworkPolicy restricting ingress to only the pods/namespaces that legitimately call them (e.g. only the UI pod may reach the agent on :8080; only the agent pod may reach the MCP server on :8081) — no pod is reachable cluster-wide by default.
- Every cluster-scoped resource the operator creates — the agent/UI/MCP ClusterRoles and ClusterRoleBindings, the agent's `-monitoring-view` binding, and the OAuthClient — is named `{namespace}-{name}-…` to prevent collision when multiple CRs coexist on the same cluster. Namespaced resources keep plain `{name}-…` names; Kubernetes already scopes those.
- The agent's ServiceAccount is bound to OpenShift's built-in `cluster-monitoring-view` ClusterRole (read-only) so its own alert-scanning/trend-monitoring features can query `thanos-querier` — this is separate from, and in addition to, the agent's own scoped-down ClusterRole.
- The UI's nginx proxies both `/api/prometheus/` (Thanos-querier — firing alerts, rules, CPU/memory charts) and `/api/alertmanager/` (Alertmanager — silences: list/create/expire) to `openshift-monitoring`, forwarding the logged-in user's own OAuth token. Reading/writing silences requires the user to additionally hold `monitoring-alertmanager-view` (read) or `monitoring-alertmanager-edit` (read+write) in the `openshift-monitoring` project — a narrower grant than `cluster-monitoring-view`, which only covers the Thanos path.
- Secrets are generated once and never rotated automatically. `{name}-ws-token` and `{name}-oauth-secrets` can be rotated by deleting the Secret — the operator regenerates it on the next reconcile.
- **Do not rotate `{name}-pg-auth` this way.** Deleting it makes the operator generate a fresh random password, but postgres only bakes a password into `PGDATA` during `initdb` on an empty data directory — the retained pg-data PVC still holds the old one, so the agent is left permanently unable to authenticate ([see above](#postgresql-data-survives-cr-deletion-by-design)). Rotating the PostgreSQL password means changing it inside the running database and in the Secret together, or tearing both down with `pulse.ai/delete-data=true` and letting the stack reinitialise.
- Container images (`Dockerfile`, `Dockerfile.bundle`) are pinned by digest, not just tag, for reproducible builds; Dependabot (`.github/dependabot.yml`) opens a PR when a pinned digest moves.

---

## Contributing

Pull requests are welcome. For substantial changes, open an issue first to discuss the approach.

### Development workflow

```bash
# Fork and clone
git clone https://github.com/YOUR_USERNAME/pulse-operator
cd pulse-operator

# Create a feature branch
git checkout -b feat/my-change

# Make changes, run tests
make test

# Submit a PR against main
```

### Commit style

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat: add HPA support for agent Deployment
fix: correct nginx root path for UBI nginx-122
docs: expand troubleshooting section
refactor: extract cluster detection into standalone package
```

### Reporting bugs

Open a [GitHub issue](https://github.com/PulseSRE/pulse-operator/issues) with:
- OpenShift version (`oc version`)
- Operator version / commit
- `oc describe openshiftpulse <name> -n <ns>`
- Relevant pod logs

---

## License

[MIT](LICENSE) — Copyright (c) 2026 Ali Mobrem / Red Hat CoE
