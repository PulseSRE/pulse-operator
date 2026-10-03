# Operator Verifier

## Build
```bash
go build -o /tmp/pulse-operator ./cmd/main.go
```

## envtest (local API server and etcd, without workload controllers)
```bash
# Install once
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
export KUBEBUILDER_ASSETS="$("$(go env GOPATH)/bin/setup-envtest" use 1.31 -p path)"

# Run
go test -count=1 ./...
```

## Local run against live OCP cluster
```bash
# Install CRD
oc apply -f config/crd/bases/pulse.ai_openshiftpulses.yaml

# Run operator (unique ports to avoid 8080/8081 conflicts)
# --zap-devel: human-readable console logs; omit for the production default (JSON).
/tmp/pulse-operator \
  --leader-elect=false \
  --metrics-bind-address=:9191 \
  --health-probe-bind-address=:9292 \
  --zap-devel

# Apply sample CR
oc apply -f examples/pulse.yaml
```

## Gotchas
- Cache warmup takes ~60-90s over WAN. Don't kill before the first reconcile log line appears.
- gp3-csi PVC provisioning takes 2-5min. The memory PVC is no longer a gate on the
  agent Deployment (removed — see agent_reconciler_test.go "Deployment is created even
  while memory PVC is Pending"); Kubernetes just holds the pod Pending until it binds.
- `--metrics-bind-address` defaults to `:8082`. Specify a different port if 8082 is in use.
- Inspect errors and process exit status when startup logs say "Stopping and waiting";
  do not treat a shutdown message as proof that initialization is healthy. Confirm
  readiness and successful reconciliation separately.
- Finalizer: delete CR only after operator is running so finalizer cleanup fires correctly.

## Sample CR
```yaml
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
```
