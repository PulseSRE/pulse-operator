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

## Existing cluster observations

Do not start a local operator or mutate a cluster as part of offline verification.
For an explicitly authorized existing test installation, reuse the acceptance
runner and its [guide](../../../docs/CLUSTER_ACCEPTANCE.md):

```bash
# Plan only; no cluster calls.
python3 scripts/cluster-acceptance.py \
  --context YOUR_CONTEXT --namespace YOUR_NAMESPACE --pulse YOUR_CR
# Add --execute only when the selected cluster is available and reads are authorized.
```

Live installation and reconciliation are separate from build/envtest verification.
Use the canonical README installation guide, select the cluster explicitly, and
retain exact images and observed identities. Do not infer provider access or
incident recovery from component readiness.

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

Use [examples/pulse.yaml](../../../examples/pulse.yaml) together with the
[README CR reference](../../../README.md#cr-spec). Replace example provider
credentials and mutable application tags with the approved matched image pair
for your test. Keep trust level 2 for approval acceptance tests.
