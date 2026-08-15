# Operator Verifier

## Build
```bash
go build -o /tmp/pulse-operator ./cmd/main.go
```

## envtest (unit tests with fake cluster)
```bash
# Install once
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
$(go env GOPATH)/bin/setup-envtest use 1.31 --bin-dir /tmp/kubebuilder-bin

# Run
KUBEBUILDER_ASSETS=/tmp/kubebuilder-bin/k8s/1.31.0-darwin-arm64 go test ./...
```

## Local run against live OCP cluster
```bash
# Install CRD
oc apply -f config/crd/bases/pulse.ai_openshiftpulses.yaml

# Run operator (unique ports to avoid 8080/8081 conflicts)
/tmp/pulse-operator \
  --leader-elect=false \
  --metrics-bind-address=:9191 \
  --health-probe-bind-address=:9292

# Apply sample CR
oc apply -f examples/pulse.yaml
```

## Gotchas
- Cache warmup takes ~60-90s over WAN. Don't kill before the first reconcile log line appears.
- gp3-csi PVC provisioning takes 2-5min — agent Deployment is gated on Bound PVC.
  Run operator for 5+ minutes to see the full stack created.
- `--metrics-bind-address` defaults to `:8082`. Specify a different port if 8082 is in use.
- controller-runtime v0.24.1: startup sequence logs "Stopping and waiting..." during init,
  not during shutdown — ignore these; wait for "Reconciling OpenShiftPulse" log line.
- Finalizer: delete CR only after operator is running so finalizer cleanup fires correctly.

## Sample CR
```yaml
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
```
