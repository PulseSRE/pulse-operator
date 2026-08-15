# Pulse Operator

Kubernetes Operator for [OpenShift Pulse](https://github.com/PulseSRE/pulse-agent) — deploys and manages the full Pulse stack (AI agent + UI + PostgreSQL) from a single custom resource.

## Overview

The operator manages the complete lifecycle of an `OpenShiftPulse` CR:

- **AgentReconciler** — ServiceAccount, ClusterRole, WS token Secret, memory PVC, Deployment, Service
- **PostgreSQLReconciler** — StatefulSet, pg-auth Secret, ClusterIP + headless Services
- **UIReconciler** — nginx ConfigMap, oauth-proxy Deployment, Service, Route, OAuthClient (redirect URI auto-patched after Route is admitted)
- **MonitoringReconciler** — ServiceMonitor, PrometheusRule (when `spec.monitoring.enabled`)
- **MCPReconciler** — MCP server Deployment + Service (when `spec.agent.mcp.enabled`)
- **NetworkPolicyReconciler** — restricts UI ingress and isolates PostgreSQL to agent-only
- **ClusterDetector** — auto-detects ingress domain, oauth-proxy image digest, and ACM availability on first reconcile

Finalizer `pulse.ai/cleanup` removes cluster-scoped resources (ClusterRoles, OAuthClient) on CR deletion.

## Prerequisites

- OpenShift 4.12+ (uses Route, OAuthClient, image detection from OpenShift APIs)
- `oc` CLI logged in with cluster-admin
- Prometheus Operator (for ServiceMonitor/PrometheusRule support)

## Install

```bash
# Install CRD
oc apply -f config/crd/bases/pulse.ai_openshiftpulses.yaml

# Deploy operator
oc apply -f deploy/operator.yaml
oc rollout status deployment/pulse-operator-manager -n pulse-operator-system
```

Or with make:

```bash
make deploy
```

## Quick Start

```bash
oc new-project openshiftpulse

cat <<EOF | oc apply -f -
apiVersion: pulse.ai/v1alpha1
kind: OpenShiftPulse
metadata:
  name: pulse
  namespace: openshiftpulse
spec:
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

# Watch status
oc get openshiftpulse pulse -n openshiftpulse -w
```

The operator auto-detects the cluster's ingress domain and oauth-proxy image. Within 2–5 minutes (PVC provisioning is the slow step) the stack is Running:

```
NAME    PHASE     AGENTHEALTHY   DBREADY   ROUTEHOST
pulse   Running   true           true      pulse-openshiftpulse.apps.<cluster-domain>
```

## CR Spec Reference

```yaml
spec:
  # AI backend — exactly one of vertexAI or anthropicApiKey is required
  vertexAI:
    projectId: my-gcp-project          # required
    region: us-east5                   # default: us-east5
    credentialSecret: gcp-sa-key       # Secret name containing SA JSON key

  anthropicApiKey:
    existingSecret: anthropic-api-key  # Secret name containing ANTHROPIC_API_KEY

  agent:
    image: quay.io/amobrem/pulse-agent:latest
    trustLevel: 2                      # 0–4; controls auto-fix aggressiveness
    allowWriteOperations: false        # grants delete on pods, patch on deployments
    allowSecretAccess: false           # grants get;list;watch on secrets
    resources: {}                      # standard corev1.ResourceRequirements
    mcp:
      enabled: false                   # deploys MCP server sidecar

  ui:
    image: quay.io/amobrem/openshiftpulse:latest
    replicas: 2
    oauthProxyImage: ""                # auto-detected from ImageStream if empty
    resources: {}

  database:
    storageSize: 5Gi                   # PVC size for PostgreSQL data
    storageClass: ""                   # uses cluster default if empty
    image: ""                          # uses RHEL9 postgresql-15 if empty

  monitoring:
    enabled: true                      # creates ServiceMonitor + PrometheusRule
```

## Status Fields

```yaml
status:
  phase: Running               # Pending | Installing | Running | Degraded
  agentHealthy: true           # Deployment.ReadyReplicas > 0
  databaseReady: true          # StatefulSet.ReadyReplicas > 0
  uiAvailable: true            # Route hostname assigned
  routeHost: pulse-openshiftpulse.apps.example.com
  observedGeneration: 3
  conditions:
  - type: Ready
    status: "True"
    reason: AllComponentsReady
```

## Uninstall

```bash
# Delete CR (operator cleans up ClusterRoles and OAuthClient via finalizer)
oc delete openshiftpulse pulse -n openshiftpulse

# Remove operator
make undeploy

# Remove CRD (deletes all OpenShiftPulse CRs first)
oc delete crd openshiftpulses.pulse.ai
```

## Development

```bash
# Build
make build

# Run tests (includes envtest)
KUBEBUILDER_ASSETS=$(setup-envtest use 1.31 -p path) make test

# Build + push operator image
make docker-build docker-push OPERATOR_IMG=quay.io/yourorg/pulse-operator:dev

# Run locally against live cluster (--leader-elect=false skips leader election)
go run ./cmd/main.go \
  --leader-elect=false \
  --metrics-bind-address=:9191 \
  --health-probe-bind-address=:9292
```

See [`.claude/skills/verify/SKILL.md`](.claude/skills/verify/SKILL.md) for detailed local-run gotchas (cache warmup, PVC provisioning timing, cross-platform builds).

## OLM / OperatorHub

A bundle image is available at `quay.io/amobrem/pulse-operator-bundle:latest`.

```bash
# Test OLM install (requires operator-sdk)
operator-sdk run bundle quay.io/amobrem/pulse-operator-bundle:latest \
  --namespace operators

# Validate bundle
make bundle-validate
```

## Releases

Tagged releases push both `quay.io/amobrem/pulse-operator:<tag>` and `:latest` via GitHub Actions. To release:

```bash
git tag v0.2.0 -m "v0.2.0: ..."
git push origin v0.2.0
```

Requires `QUAY_USERNAME` and `QUAY_TOKEN` GitHub repository secrets.

## Architecture

```
pulse-operator-system/
└── pulse-operator-manager (Deployment)
    └── OpenShiftPulseReconciler
        ├── AgentReconciler       → {ns}/pulse-openshift-sre-agent (Deployment)
        ├── PostgreSQLReconciler  → {ns}/pulse-openshift-sre-agent-postgresql (StatefulSet)
        ├── UIReconciler          → {ns}/pulse-openshiftpulse (Deployment) + Route + OAuthClient
        ├── MonitoringReconciler  → {ns}/pulse-openshift-sre-agent (ServiceMonitor + PrometheusRule)
        ├── MCPReconciler         → {ns}/pulse-mcp-server (Deployment) [optional]
        └── NetworkPolicyReconciler → {ns}/*-pg-access + {ns}/*-openshiftpulse
```

## License

MIT
