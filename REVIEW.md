# Code Review — commits f54e86d + 69ae96d

Reviewed by Claude Code audit agent. Verified-correct items listed at bottom.

---

## BLOCKER

### `PulseAgentHighRestarts` alert will never fire — wrong container name
`monitoring_reconciler.go:136`

PromQL uses `container="openshift-sre-agent"` but the agent container is named
`"agent"` (`agent_reconciler.go:449`). Alert is silently dead. No test covers PromQL content.

**Fix:** Change to `container="agent"`.

---

## HIGH

### Status never flushed during Route-hostname requeue
`openshiftpulse_controller.go:108–110`

```go
if uiResult.RequeueAfter > 0 {
    return uiResult, nil  // no r.Status().Update() call
}
```
Agent and PG reconcile have already succeeded but their status mutations
are never written. CR shows empty status for the full Route-admission window
(several minutes on a fresh install).

**Fix:** Call `r.Status().Update(ctx, pulse)` before returning on the requeue path.

### PostgreSQL resources orphaned on CR deletion — OwnerReferences missing
`postgresql_reconciler.go:381–391`

`setOwner` only writes an annotation. `PostgreSQLReconciler` has no `Scheme`
field so `controllerutil.SetControllerReference` is never called for the pg-auth
Secret, StatefulSet, Services, or connection Secret. They survive CR deletion.
Finalizer only cleans up ClusterRole/ClusterRoleBinding/OAuthClient.

**Fix:** Add `Scheme *runtime.Scheme` to `PostgreSQLReconciler`, thread it through,
call `controllerutil.SetControllerReference` instead of `setOwner`.

### `UIAvailable = true` set on Route admission, not pod readiness
`ui_reconciler.go:144`

Set unconditionally once OCP assigns a hostname — UI pods can be in
`CrashLoopBackOff`. Root reconciler sets `Phase: Running` and emits
`AllComponentsHealthy` event while UI is broken.

**Fix:** Gate `UIAvailable` on `r.isDeploymentReady(...)` the same way
`AgentHealthy` and `DatabaseReady` are checked.

### Agent Deployment has no default resource limits
`agent_reconciler.go:451`

PG and MCP got hardcoded defaults in these commits; agent did not.
`cr.Spec.Agent.Resources` is empty by default — agent runs unbounded.

**Fix:** Apply defaults when `.Limits` is nil (e.g. `500m`/`256Mi` req,
`2`/`1Gi` limits), matching the PG/MCP pattern.

### `finalizer_test.go` does not actually test `errors.Join` aggregation
`finalizer_test.go:47–57`

No RBAC resources are pre-created; all deletes return `NotFound` (already
suppressed). Code could revert to stopping on first error and this test still
passes. The claimed fix is structurally correct but unverifiable from this test.

**Fix:** Pre-create a ClusterRole + ClusterRoleBinding, inject a client that
returns a transient error on the first delete, assert the second delete still
ran and the error contains both messages.

### Connection Secret `{name}-postgresql` has no update path
`postgresql_reconciler.go:82–95`

Create-or-noop. If `pg-auth` is migrated (POSTGRES_* → POSTGRESQL_*),
the `database-url` in the connection secret is never refreshed.

**Fix:** Add an update branch that rewrites `database-url` when it diverges
from the computed URL.

---

## MEDIUM

### `UIReconciler.SetupWithManager` is dead code
`ui_reconciler.go:793–826` / `cmd/main.go:75–86`

Fully implemented including Route watch → owner mapping but never called.
Route hostname watch is inactive; operator falls back to 30-second periodic
requeue. Also a footgun: if someone calls it alongside the root controller
the original dual-registration race re-emerges.

**Fix:** Either remove it (UIReconciler runs as a sub-reconciler only),
or add an explicit Route watch inside `OpenShiftPulseReconciler.SetupWithManager`.

### `pulse.Status.DatabaseReady` written twice — first write is dead
`postgresql_reconciler.go:74` + `openshiftpulse_controller.go:141–144`

`reconcilePostgres` sets it; root reconciler overwrites it unconditionally.
Remove the assignment from `reconcilePostgres`.

### `MCPServiceURL` comment names wrong env var
`mcp_reconciler.go:28`

Comment says `PULSE_AGENT_MCP_URL`; actual injected var is `PULSE_MCP_URL`.

### Stale comment in `reconcileWithBoundPVC` test helper
`agent_reconciler_test.go:26`

Comment says "Stops before Deployment." The PVC gate was removed; Deployment
is created unconditionally. Comment is wrong; the PVC-binding step is a no-op.

---

## Verified correct

- BLOCKER (dual-registration race): `AgentReconciler.SetupWithManager` not called in `main.go`. Fixed.
- `deleteClusterScopedResources` uses `errors.Join` and iterates all deletes unconditionally. Structurally correct.
- `reconcilePGService` uses `CreateOrUpdate`. Tested.
- MCP readiness + liveness probes exist and are tested.
- PG and MCP resource limits exist and are tested.
- `Recorder` wired in `main.go` and present on root reconciler.
