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
| **PostgreSQL** | StatefulSet (pg-data PVC retained on delete), pg-auth Secret, ClusterIP + headless Services |
| **UI** | nginx ConfigMap, oauth-proxy Deployment (TLS on 8443), Service, Route, OAuthClient |
| **Monitoring** | ServiceMonitor (agent `/metrics`), PrometheusRule (`PulseAgentDown`, `PulsePostgreSQLDown`) |
| **MCP** | MCP server Deployment + Service (optional, `spec.agent.mcp.enabled: true`) |
| **Network** | UI ingress NetworkPolicy (OCP ingress + Prometheus only), PostgreSQL access-only NetworkPolicy |
| **Cluster detect** | Reads ingress domain, oauth-proxy image digest, ACM availability on first reconcile |

A `pulse.ai/cleanup` finalizer ensures ClusterRoles and OAuthClient are removed when the CR is deleted — no orphans on uninstall.

---

## Prerequisites

- OpenShift 4.12+ (uses Route, OAuthClient, ImageStream APIs)
- `oc` CLI with `cluster-admin`
- Prometheus Operator (for `ServiceMonitor` / `PrometheusRule` — ships with OpenShift Monitoring)
- One of: **Vertex AI** GCP service account key, or **Anthropic API key**

---

## Install via OLM

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

```bash
cat <<EOF | oc apply -f -
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: pulse-operator-group
  namespace: openshiftpulse
spec:
  targetNamespaces: []
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: pulse-operator
  namespace: openshiftpulse
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
oc get csv -n openshiftpulse -w
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

## CR Status

```yaml
status:
  phase: Running               # Pending | Installing | Running | Degraded
  agentHealthy: true           # agent Deployment has ≥1 ready replica
  databaseReady: true          # PostgreSQL StatefulSet has ≥1 ready replica
  uiAvailable: true            # Route hostname assigned by OCP router
  routeHost: pulse-openshiftpulse.apps.cluster.example.com
  observedGeneration: 3
  conditions:
  - type: Ready
    status: "True"
    reason: AllComponentsReady
    observedGeneration: 3
```

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

---

## Uninstall

```bash
# Delete CR — operator finalizer cleans up ClusterRoles and OAuthClient
oc delete openshiftpulse pulse -n openshiftpulse

# Remove operator (OLM install)
oc delete subscription pulse-operator -n openshiftpulse
oc delete csv pulse-operator.v0.1.0 -n openshiftpulse
oc delete catalogsource pulse-operator-catalog -n openshift-marketplace

# Or for manifest install
make undeploy

# Remove namespace
oc delete namespace openshiftpulse
```

> **Data retention:** PostgreSQL PVCs are retained by the StatefulSet `volumeClaimTemplate` lifecycle and survive operator deletion. Re-create the CR to reattach them.

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
go run ./cmd/main.go \
  --leader-elect=false \
  --metrics-bind-address=:9191 \
  --health-probe-bind-address=:9292

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

### Regenerate CRDs and RBAC after API changes

```bash
make manifests generate
```

---

## OLM / OperatorHub

### Build the catalog image

The catalog image is a File-Based Catalog (FBC) served by `opm`. To rebuild it after bundle changes:

```bash
# 1. Render the bundle into FBC YAML
podman run --rm -v $(pwd)/bundle:/bundle:z \
  quay.io/operator-framework/opm:latest render /bundle -o yaml > /tmp/catalog.yaml

# 2. Prepend package + channel declarations
cat - /tmp/catalog.yaml > /tmp/full-catalog.yaml <<'HEADER'
---
defaultChannel: alpha
name: pulse-operator
schema: olm.package
---
entries:
- name: pulse-operator.v0.1.0
name: alpha
package: pulse-operator
schema: olm.channel
HEADER

# 3. Build catalog image (linux/amd64 — cluster nodes are x86)
podman build --platform linux/amd64 \
  -f Dockerfile.catalog \
  -t quay.io/amobrem/pulse-operator-catalog:latest .
podman push quay.io/amobrem/pulse-operator-catalog:latest
```

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

CI builds and pushes:
- `quay.io/amobrem/pulse-operator:v0.2.0` + `:latest`
- `quay.io/amobrem/pulse-operator-catalog:v0.2.0` + `:latest`

Requires `QUAY_USERNAME` and `QUAY_TOKEN` as GitHub repository secrets.

---

## Architecture

```
pulse-operator-system/
└── pulse-operator-manager   (Deployment — 1 replica, leader-elected)
    └── OpenShiftPulseReconciler
        ├── pulse.ai/cleanup finalizer  ← removes cluster-scoped resources on CR delete
        │
        ├── AgentReconciler
        │   ├── {ns}/{name}-openshift-sre-agent  (ServiceAccount + ClusterRole + ClusterRoleBinding)
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
        │   ├── {ns}/{name}-openshiftpulse       (ServiceAccount + ClusterRole)
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
        │   └── {ns}/{name}-mcp-server           (Deployment + Service :8001)
        │
        └── NetworkPolicyReconciler
            ├── {ns}/{name}-openshiftpulse       (UI: ingress from OCP router + Prometheus only)
            └── {ns}/{name}-pg-access            (PG: ingress from agent pods only)
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

- All managed pods run as non-root with `AllowPrivilegeEscalation=false`, `Capabilities.Drop=ALL`, and `SeccompProfile=RuntimeDefault`.
- PostgreSQL sets `ReadOnlyRootFilesystem=false` (PG requires writable socket and temp paths).
- The operator's ClusterRole includes `escalate`+`bind` on RBAC resources — required to create ClusterRoles for managed instances, but represents a privilege escalation path. Restrict exec access to `pulse-operator-system` via NetworkPolicy.
- OAuthClient names are scoped to `{namespace}-{name}` to prevent collision when multiple CRs coexist on the same cluster.
- Secrets (pg-auth, ws-token, oauth cookie) are generated once and never rotated automatically. Rotate manually by deleting the Secret — the operator regenerates it on next reconcile.

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
