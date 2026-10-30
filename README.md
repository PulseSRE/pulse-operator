# Pulse Operator

[![CI](https://github.com/PulseSRE/pulse-operator/actions/workflows/ci.yml/badge.svg)](https://github.com/PulseSRE/pulse-operator/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/PulseSRE/pulse-operator)](go.mod)
[![OpenShift](https://img.shields.io/badge/OpenShift-4.12%2B-red)](https://www.redhat.com/en/technologies/cloud-computing/openshift)
[![Docs](https://img.shields.io/badge/docs-pulsesre.github.io-blue)](https://pulsesre.github.io/pulse-operator/)

A Kubernetes Operator that deploys and manages the complete [OpenShift Pulse](https://github.com/PulseSRE/pulse-agent) stack — AI SRE agent, React UI, PostgreSQL — from a **single custom resource** on OpenShift.

```
OpenShiftPulse CR → operator reconciliation → verify installation and provider access
```

[![Watch the 30-second overview of Pulse](https://pulsesre.github.io/pulse-agent/media/video-thumb.jpg)](https://pulsesre.github.io/pulse-agent/#film)

<sub>30-second overview of what the operator installs, no sound.</sub>

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
- [Cluster acceptance](#cluster-acceptance)
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
| **UI** | nginx configuration Secret, Deployment with nginx and oauth-proxy (TLS on 8443), Service, Route, OAuthClient |
| **Monitoring** | ServiceMonitor (agent `/metrics`), PrometheusRule (`PulseAgentDown`, `PulsePostgreSQLDown`) |
| **MCP** | MCP server ServiceAccount + ClusterRole (read-only) + ClusterRoleBinding, Deployment, Service, NetworkPolicy (optional, `spec.agent.mcp.enabled: true`; setting it back to `false` removes them all) |
| **Network** | Per-component ingress-only NetworkPolicies: UI (the OpenShift router on 8443, on the pod or host network), PostgreSQL (agent and enabled Temporal pods), agent (UI pod + Prometheus), MCP server (agent pod only); render service, when enabled (agent pod in, UI render port and DNS out) |
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

The operator versions independently of the Pulse application it deploys.
Read `OperatorVersion` in `internal/controller/compat.go` for this checkout's
operator version, and the CR's image fields for the deployed application versions:

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
    trustLevel: 2          # monitor: 0/1=no remediation, 2=propose, 3/4=execute
    adminUsers: "kube:admin" # restrict administrator endpoints (skill edits, fix approval, monitor resume, memory import and edits); empty allows any authenticated identity
    allowWriteOperations: false   # adds delete(pods), patch/update(deployments,statefulsets) to agent ClusterRole
    allowSecretAccess: false      # adds get/list/watch(secrets) to agent ClusterRole
    resources: {}                 # corev1.ResourceRequirements
    mcp:
      enabled: false       # deploys a separate MCP server Deployment for tool extension
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
| `0` / `1` | Background monitor does not enter remediation; investigations can still run. |
| `2` | Background monitor creates proposed actions for approval. |
| `3` / `4` | Background monitor executes eligible remediation without the level-2 approval gate. |

This table describes the background monitor, not a universal authorization
boundary for chat, MCP tools, or plans. Kubernetes RBAC still applies. The
current monitor uses the configured level as a floor and caps browser requests
at that same level, so browser trust selection cannot lower its authority.
Its enabled categories start with all server handlers; browser category
checkboxes currently cannot restrict that set. Configure the CR and agent
permissions deliberately rather than relying on browser preferences.

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

**Workflow history (spec.temporal.ui):** `ui: true` also deploys the Temporal
Web UI (`{name}-temporal-ui`), where every run's full history — each activity
attempt, retry, durable timer and approval signal — is inspectable without a
CLI. It is off by default, and it gets a Service but deliberately **no
Route**: the Temporal UI ships with no authentication of its own and offers a
"terminate workflow" button to anyone who can load it, so a public Route would
be an unauthed kill switch for every in-flight fix. Reach it locally:

```bash
oc port-forward -n <ns> svc/<name>-temporal-ui 8080:8080
```

Front it with an oauth-proxy before exposing it permanently.

**Turning it off:** since v0.11.0, `ui: false` removes the Web UI's Deployment and Service, and `enabled: false` also removes the Temporal server. Workflow history is in PostgreSQL, so re-enabling picks it back up. Earlier versions left both running.

### Dashboard preview (spec.render)

`spec.render.enabled: true` deploys the render service (`{name}-render`, image
`openshiftpulse-render`, built from pulse-ui's `render/`). It lets the agent's
view designer screenshot a draft dashboard and fix what it sees before saving.
The operator injects `PULSE_AGENT_RENDER_URL` and `PULSE_AGENT_RENDER_TOKEN`
into the agent. Without them, the agent reports preview as unavailable and
saves as before. Off by default: it runs a headless Chromium (256Mi requested,
1Gi limit; override with `spec.render.resources`).

The service renders with the requesting user's own token, so it shows nobody
more than they can already see. The operator enforces that with these pieces:

- **No credentials in the render pod.** `automountServiceAccountToken: false`.
  The only token it uses is the one each request carries.
- **Only the agent can call it.** The `{name}-render` NetworkPolicy admits the
  agent pod only. Requests also need `{name}-render-token`, which only the
  agent is given.
- **A render-only UI listener.** The UI pod gains a second nginx listener on
  8081 (Service port `render`). It serves the static UI and the
  `/api/kubernetes/`, `/api/prometheus/` and `/api/alertmanager/` proxies,
  which authenticate the forwarded user token upstream. `/api/agent/` returns
  404 there. The main listener's agent route adds the agent's own credential
  and trusts `X-Forwarded-User`, so it must never be reachable from the render
  pod.
- **Locked-down egress.** The render pod may reach only that UI port, plus
  DNS. The UI pod's NetworkPolicy admits only the render pod to 8081.

Turning it off removes the render Deployment, Service, NetworkPolicy and
token Secret, the agent env vars, and the UI listener.

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
an unexamined default. Its memory-cache PVC is `ReadWriteOnce`; overlapping pods on different nodes can fail volume attachment. The agent also runs forward-only, no-rollback DB migrations automatically on startup, so
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
# Install the Go version required by go.mod first.
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
export KUBEBUILDER_ASSETS="$("$(go env GOPATH)/bin/setup-envtest" use 1.31 -p path)"
```

### Run tests

```bash
make test
make vet
```

### Live cluster development

A local operator process reconciles real cluster resources. Use it only in an
explicitly authorized development cluster with a dedicated kubeconfig whose
current context has been verified. The operator watches Pulse CRs cluster-wide;
setting an `oc` namespace does not limit the controller's reconciliation scope.
Do not run a second controller against an installation already managed by the
in-cluster operator with leader election disabled.

For observation-only verification of an existing installation, use the
[explicit-context acceptance runner](docs/CLUSTER_ACCEPTANCE.md). Installation
and CR creation follow the canonical instructions above as a separate operation.

**Gotchas:**
- API/cache initialization time depends on cluster connectivity; a reconcile log alone does not prove readiness.
- Storage provisioning may delay Pod startup. The agent Deployment is created even while its memory PVC is Pending; Kubernetes holds the Pod until the claim binds.
- Build for `linux/amd64` explicitly when targeting x86 cluster nodes: `make docker-build` passes that platform automatically. Local emulated builds remain distinct from native CI validation.

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

The catalog image is a File-Based Catalog (FBC) served by `opm`. The
[tag release workflow](.github/workflows/release.yml) builds and publishes it
automatically alongside the operator and bundle images. It renders every
available released bundle and links consecutive entries into a complete upgrade
graph. Do not replace that catalog with a one-entry example: installed older
versions can lose their upgrade path.

For a custom registry or a manual rebuild, reproduce the workflow's
**Render the catalog from every released bundle** step using your published
bundle references. Preserve all available predecessors and require the current
bundle to render successfully. Use the digest-pinned `opm` image from
`Dockerfile.catalog`; inspect each rendered bundle's `image` reference and the
channel's `replaces` chain before building. Cluster nodes must be able to pull
both the catalog and every referenced bundle; mirroring the catalog alone does
not rewrite its bundle references.

Build the reviewed `catalog/full-catalog.yaml` for the target cluster
architecture with `Dockerfile.catalog`. Tag and publish it under your chosen
registry only as part of an explicitly approved release. Verify the resulting
image digest and upgrade graph before updating the CatalogSource. The workflow
is the executable source of truth for rendering and publication commands.

### Validate the bundle

```bash
make bundle-validate
```

### Releasing a new version

Tag to trigger CI publishing:

```bash
# Set an approved, unused version and update OperatorVersion first.
VERSION=vX.Y.Z
# Replace the placeholder before executing either command.
git tag -a "$VERSION" -m "$VERSION"
git push origin "$VERSION"
```

CI ([`.github/workflows/release.yml`](.github/workflows/release.yml)) builds and pushes:
- `quay.io/amobrem/pulse-operator:<tag>` + `:latest` (the operator image, from `Dockerfile`)
- `quay.io/amobrem/pulse-operator-bundle:<tag>` + `:latest` (the OLM bundle, from `Dockerfile.bundle`)
- `quay.io/amobrem/pulse-operator-catalog:<tag>` + `:latest` (the file-based OLM catalog, from `Dockerfile.catalog`)

Before the bundle image is built, CI runs [`scripts/bump-bundle-version.py`](scripts/bump-bundle-version.py) against the pushed tag, which updates the CSV's `metadata.name`, `spec.version`, `metadata.annotations.containerImage`, and the manager Deployment's `image` to match — and sets `spec.replaces` to whatever CSV name was current before the bump, extending the upgrade graph by one step. This runs in CI only; the version bump is never committed back to `main`, so the checked-in CSV may lag the release; verify the published bundle rather than inferring its version from `main`.

The workflow renders available released bundles into the **catalog** image
(`pulse-operator-catalog`, used by the CatalogSource in [Install via OLM](#install-via-olm)),
and publishes it with the operator and bundle. The current bundle
must render successfully; unavailable historical bundles are skipped and
`replaces` is rewired to available predecessors. Verify the resulting upgrade
graph and all three published image digests before deployment.

Before tagging, update `internal/controller/compat.go`
`OperatorVersion` to the tag without `v`; the release workflow rejects a mismatch.
These image publications do not themselves create a GitHub Release.

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
        │   ├── {ns}/{name}-openshift-sre-agent-memory  (PVC 1Gi RWO — Pod waits for binding)
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
        │   ├── {ns}/{name}-nginx                (Secret — nginx.conf, root /opt/app-root/src)
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
            ├── {ns}/{name}-pg-access            (PG: ingress from agent and enabled Temporal pods)
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

**Possible cause:** Stale nginx configuration or nginx not pointing at `/opt/app-root/src`. Force reconcile:

```bash
oc delete secret pulse-nginx -n openshiftpulse
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
# Confirm the proxy exists in the live configuration Secret
oc get secret <name>-nginx -n <namespace> -o jsonpath='{.data.nginx\.conf}' | base64 --decode | grep -A5 'location /api/alertmanager/'
```

If it's missing, the operator image predates this fix — upgrade and restart the UI pods. Also confirm the logged-in user holds `monitoring-alertmanager-view` (or `-edit`) in `openshift-monitoring`; `cluster-monitoring-view` alone (which covers the Thanos path) is not sufficient for silences.

### Migrating from Helm install

The operator cannot adopt Helm-managed Deployments/StatefulSets because their label selectors are immutable. The operator detects selector mismatches and replaces the resources automatically (StatefulSet with orphan cascade to preserve the PVC). If the agent Deployment is stuck:

```bash
# Manually delete the Helm-managed deployment; operator recreates it cleanly
oc delete deployment pulse-openshift-sre-agent -n openshiftpulse
```

### CatalogSource stuck in TRANSIENT_FAILURE

A visible `PackageManifest` alone does not prove the current catalog is healthy; it may reflect cached discovery. Inspect CatalogSource connection status, its Pod logs, and Subscription conditions:

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
- The operator's own ClusterRole ([`config/rbac/role.yaml`](config/rbac/role.yaml)) does **not** include `escalate`/`bind` on RBAC resources — every rule it ever writes into a generated agent/UI/MCP ClusterRole is already a permission it holds itself, so Kubernetes' RBAC "you already have this" rule lets `create`/`update` succeed without those verbs. It's still a privilege-concentration point (it *creates* ClusterRoles/ClusterRoleBindings for every managed instance): restrict `pods/exec` access in `pulse-operator-system` with RBAC. NetworkPolicy controls pod traffic and does not authorize Kubernetes exec requests.
- The agent, UI, PostgreSQL, and MCP server have ingress NetworkPolicies. UI ingress permits only the OpenShift router on 8443 (the `ingress` policy group, plus `policy-group.network.openshift.io/host-network` for routers on the host network, the default on bare metal, vSphere and SNO); agent ingress permits UI and Prometheus peers; MCP ingress permits the agent; PostgreSQL permits the agent and enabled Temporal peers. Before v0.11.0 the PostgreSQL policy selected no pod and so isolated nothing; on upgrade it starts applying. This does not imply all optional components are isolated, does not restrict egress, and requires an enforcing network plugin. Use the cluster acceptance packet probes to verify observed connectivity.
- Every cluster-scoped resource the operator creates — the agent/UI/MCP ClusterRoles and ClusterRoleBindings, the agent's `-monitoring-view` binding, and the OAuthClient — is named `{namespace}-{name}-…` to prevent collision when multiple CRs coexist on the same cluster. Namespaced resources keep plain `{name}-…` names; Kubernetes already scopes those.
- The agent's own ClusterRole is read-only unless `allowWriteOperations`/`allowSecretAccess` are set. Besides core workloads, it reads exactly what the monitor's scanners scan: `clusteroperators` and `clusterversions` (degraded operators, upgrades), `customresourcedefinitions` (stuck deletions), `routes` (route errors and certificates), `controllerrevisions` (StatefulSet/DaemonSet rollouts for "what changed"), Argo CD `applications` (GitOps syncs) and `machineconfigpools` (maintenance blockers). All are `get`/`list` only. Before v0.10.0 these reads were missing, and the agent reports the scanners that needed them as degraded ("To fix: …" in the UI); upgrading the operator updates the role on the next reconcile and those warnings clear. The Argo CD rule is harmless on a cluster without Argo CD.
- The agent's ServiceAccount is bound to OpenShift's built-in `cluster-monitoring-view` ClusterRole (read-only) so its own alert-scanning/trend-monitoring features can query `thanos-querier` — this is separate from, and in addition to, the agent's own scoped-down ClusterRole.
- The UI's nginx proxies both `/api/prometheus/` (Thanos-querier — firing alerts, rules, CPU/memory charts) and `/api/alertmanager/` (Alertmanager — silences: list/create/expire) to `openshift-monitoring`, forwarding the logged-in user's own OAuth token. Reading/writing silences requires the user to additionally hold `monitoring-alertmanager-view` (read) or `monitoring-alertmanager-edit` (read+write) in the `openshift-monitoring` project — a narrower grant than `cluster-monitoring-view`, which only covers the Thanos path.
- Secrets are generated once and never rotated automatically. `{name}-ws-token` and `{name}-oauth-secrets` can be rotated by deleting the Secret — the operator regenerates it on the next reconcile.
- **Do not rotate `{name}-pg-auth` this way.** Deleting it makes the operator generate a fresh random password, but postgres only bakes a password into `PGDATA` during `initdb` on an empty data directory — the retained pg-data PVC still holds the old one, so the agent is left permanently unable to authenticate ([see above](#postgresql-data-survives-cr-deletion-by-design)). Rotating the PostgreSQL password means changing it inside the running database and in the Secret together, or tearing both down with `pulse.ai/delete-data=true` and letting the stack reinitialise.
- Container images (`Dockerfile`, `Dockerfile.bundle`) are pinned by digest, not just tag, for reproducible builds; Dependabot (`.github/dependabot.yml`) opens a PR when a pinned digest moves.

---

## Cluster acceptance

Use the [explicit-context acceptance runner](docs/CLUSTER_ACCEPTANCE.md) against an existing disposable test installation. It records ownership, workload readiness, exact image IDs, policy selectors, stability, and opt-in token/Temporal/network observations. Runner unit tests are mocked; real cluster acceptance is separate and remains unproven until those checks execute.

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
