# Pulse Operator

[![CI](https://github.com/PulseSRE/pulse-operator/actions/workflows/ci.yml/badge.svg)](https://github.com/PulseSRE/pulse-operator/actions/workflows/ci.yml)

A Kubernetes Operator that deploys and manages the complete [OpenShift Pulse](https://github.com/PulseSRE/pulse-agent) stack — AI SRE agent, React UI, PostgreSQL — from a **single custom resource** on OpenShift.

---

## What it does

```
oc apply -f pulse.yaml   →   Running cluster AI assistant in ~5 minutes
```

One `OpenShiftPulse` CR drives the full lifecycle:

| Reconciler | Resources managed |
|---|---|
| **Agent** | ClusterRole (read-only cluster access), WS token Secret, memory PVC, Deployment, Service |
| **PostgreSQL** | StatefulSet (pg-data PVC retained on delete), pg-auth Secret, ClusterIP + headless Services |
| **UI** | nginx ConfigMap, oauth-proxy Deployment (TLS on 8443), Service, Route, OAuthClient |
| **Monitoring** | ServiceMonitor (agent `/metrics`), PrometheusRule (`PulseAgentDown`, `PulsePostgreSQLDown`) |
| **MCP** | MCP server Deployment + Service (optional) |
| **Network** | UI ingress NetworkPolicy (OCP ingress + Prometheus only), PostgreSQL access-only NetworkPolicy |
| **Cluster detect** | Reads ingress domain, oauth-proxy image digest, ACM availability on first reconcile |

A `pulse.ai/cleanup` finalizer ensures ClusterRoles and OAuthClient are removed when the CR is deleted — no orphans on uninstall.

---

## Prerequisites

- OpenShift 4.12+ (uses Route, OAuthClient, ImageStream APIs)
- `oc` with `cluster-admin`
- Prometheus Operator (for `ServiceMonitor`/`PrometheusRule`)
- One of: **Vertex AI** project or **Anthropic API key** (set as a Secret before install)

---

## Install

### 1. Set up your AI backend secret

**Option A — Vertex AI (GCP):**
```bash
oc new-project openshiftpulse
oc create secret generic gcp-sa-key \
  --from-file=key.json=/path/to/sa-key.json \
  -n openshiftpulse
```

**Option B — Anthropic API:**
```bash
oc new-project openshiftpulse
oc create secret generic anthropic-api-key \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-... \
  -n openshiftpulse
```

### 2. Deploy the operator

```bash
# Install CRD
oc apply -f https://raw.githubusercontent.com/PulseSRE/pulse-operator/main/config/crd/bases/pulse.ai_openshiftpulses.yaml

# Deploy operator into its own namespace
oc apply -f https://raw.githubusercontent.com/PulseSRE/pulse-operator/main/deploy/operator.yaml
oc rollout status deployment/pulse-operator-manager -n pulse-operator-system
```

Or clone and use make:
```bash
git clone https://github.com/PulseSRE/pulse-operator
cd pulse-operator
make deploy
```

### 3. Create your first Pulse instance

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
  ui:
    image: quay.io/amobrem/openshiftpulse:latest
    replicas: 2
  database:
    storageSize: 5Gi
  monitoring:
    enabled: true
EOF
```

Watch the operator bring up the stack:
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

Open the Route host in a browser — you'll be redirected through OCP OAuth and land in the Pulse UI.

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

## Uninstall

```bash
# Delete CR — operator finalizer cleans up ClusterRoles and OAuthClient
oc delete openshiftpulse pulse -n openshiftpulse
oc delete namespace openshiftpulse

# Remove operator and CRD
make undeploy
```

> **Data:** PostgreSQL PVCs are retained by the StatefulSet volumeClaimTemplate lifecycle and survive operator deletion. Re-create the CR to reattach them.

---

## Development

### Run tests

```bash
# Install envtest once
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
setup-envtest use 1.31 --bin-dir /tmp/kubebuilder-bin

# Run all tests
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
- Build for `linux/amd64` explicitly: `make docker-build` passes `--platform linux/amd64` automatically.

### Build and push the image

```bash
make docker-build docker-push OPERATOR_IMG=quay.io/yourorg/pulse-operator:dev
```

`CONTAINER_TOOL` defaults to `podman`. Override with `CONTAINER_TOOL=docker` if needed.

---

## OLM / OperatorHub

The bundle image is published at `quay.io/amobrem/pulse-operator-bundle:latest`.

```bash
# Test install via OLM (requires operator-sdk)
operator-sdk run bundle quay.io/amobrem/pulse-operator-bundle:latest \
  --namespace operators

# Validate
make bundle-validate
```

---

## Releasing

Tag a version to trigger CI publishing to Quay:

```bash
git tag v0.2.0 -m "v0.2.0: ..."
git push origin v0.2.0
```

This builds and pushes:
- `quay.io/amobrem/pulse-operator:v0.2.0` + `:latest`
- `quay.io/amobrem/pulse-operator-bundle:v0.2.0` + `:latest`

Requires `QUAY_USERNAME` and `QUAY_TOKEN` as GitHub repository secrets (Settings → Secrets → Actions).

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
        │   ├── {ns}/{name}-pg-auth              (Secret — user/password/db)
        │   ├── {ns}/{name}-openshift-sre-agent-postgresql  (StatefulSet + pg-data PVC)
        │   └── {ns}/{name}-openshift-sre-agent-postgresql[-headless]  (Services)
        │
        ├── UIReconciler
        │   ├── {ns}/{name}-openshiftpulse       (ServiceAccount + ClusterRole)
        │   ├── {ns}/{name}-oauth-secrets        (Secret — client-secret + cookie-secret)
        │   ├── {ns}/{name}-nginx                (ConfigMap — nginx.conf)
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
- `ingresses.config.openshift.io/cluster` → wildcard app domain
- `openshift/oauth-proxy` ImageStream → correct proxy image digest for this OCP version
- `open-cluster-management-observability` namespace → ACM multicluster availability

Results are cached (`sync.Once`) — zero API calls on subsequent reconciles.

---

## Security

- All managed pods run as non-root with `AllowPrivilegeEscalation=false`, `Capabilities.Drop=ALL`, and `SeccompProfile=RuntimeDefault`.
- PostgreSQL explicitly sets `ReadOnlyRootFilesystem=false` (PG requires writable `/tmp` and socket paths).
- The operator's own ClusterRole includes `escalate`+`bind` on RBAC resources — this is required to create ClusterRoles for managed instances, but represents a privilege escalation risk. Mitigate by restricting exec access to `pulse-operator-system` via NetworkPolicy.
- OAuthClient names are scoped to `{namespace}-{name}` to prevent collision when multiple CRs coexist.

---

## License

MIT
