# Changelog

All notable changes to the Pulse Operator are documented in this file.

Reconstructed from the tagged release history; entries describe what each tag
actually contains, taken from its commits.

## Unreleased

- **MCP server reads a TOML config instead of flags.** `kubernetes-mcp-server` removed `--port` and `--toolsets` in favour of `--config`/`--config-dir`, but the operator kept passing the old flags, so the server exited at startup with `unknown flag: --port`. Because the published image is referenced by the mutable tag `pulse-agent:mcp-server`, and Kubernetes gives any non-`latest` tag `imagePullPolicy: IfNotPresent`, a running pod kept working off its node's cached copy and only broke the first time it was rescheduled onto a node without one — which is exactly what a node drain during a cluster upgrade does. The operator now renders the port and toolset list into a `<name>-mcp-server-config` ConfigMap, mounts it at `/etc/mcp`, and passes `--config`. A hash of the rendered file is annotated on the pod template so changing `spec.agent.mcp.toolsets` rolls the Deployment instead of leaving the running server on the old list. `spec.agent.mcp.toolsets` is unchanged for users.
- **Operand images are digest-pinned and actually wired to `spec.relatedImages`.** The CSV listed related images that nothing consumed: the manager container had no matching env, so the operator used the mutable tags compiled into each reconciler and the list was documentation that could drift — and had, with render and both Temporal images missing entirely. All eight operands are now pinned by digest in `spec.relatedImages`, set as `RELATED_IMAGE_*` env on the manager (and in `deploy/operator.yaml`, so a non-OLM install deploys the same operands), and read by the operator, with the compiled tags kept only as a `make run` fallback. A release therefore deploys the operands it was tested against, `oc adm catalog mirror` can see every image for disconnected installs, and a tag moving under a running deployment can no longer change what a pod runs. Precedence is unchanged where a user has an opinion: an explicit image in the CR still wins over both. Two tests enforce the invariants — every `RELATED_IMAGE_*` the code reads must be present in the CSV with a matching `relatedImages` entry, and every entry must be a digest.

## v0.12.0 (2026-10-30)

Pairs with pulse-agent and pulse-ui v2.36.0 (and runs v2.34.0–v2.35.0). No CRD or RBAC changes. **Behaviour change on upgrade:** stuck agent and UI pods are no longer deleted; the reason is reported on the `AgentReady`/`UIReady` condition and a `PodNotHealthy` event instead (#109). Pull requests #101–#110. Tagging this release also publishes a GitHub Release from this section (new in the release workflow; tags v0.6.0–v0.11.0 have no Release page).

- Docs: v0.11.0 also runs pulse-agent and pulse-ui v2.36.0 with no operator change. The agent applies migration 041 (`investigations.evidence_detail`, forward-only) on start. Background investigation plans are no longer offered write tools, which the operator's read-only default RBAC already implied.
- **Self-heal only deletes a pod when that can fix it.** Agent and UI pods were deleted on every reconcile when `Pending` for 2 minutes or in `ImagePullBackOff`/`CrashLoopBackOff`. None of these is fixed by recreating the pod, and the deletion did harm: a large image still pulling was cancelled and restarted every 2 minutes, a crashlooping container lost `oc logs --previous` and its restart backoff, a bad tag re-pulled every 30 seconds, and every pass logged a `SelfHealed` event for nothing healed. These pods are now left alone and diagnosed: the `AgentReady`/`UIReady` condition carries the pod's reason and message (exit code and termination message for a crashloop, the image for a pull failure, the scheduler's message for an unschedulable pod), with a `Warning PodNotHealthy` event. A bad image during an upgrade is still reverted by automatic rollback. The PostgreSQL pod-0 is deleted only when it is `Pending` on an outdated StatefulSet revision (the blocked-rollout case the rule was written for), not when it is unschedulable on the current spec, which deleted it every 2 minutes forever.
- Docs: v0.11.0 runs pulse-agent and pulse-ui v2.35.0 with no operator change. The agent applies migrations 038–040 on start (forward-only), and `adminUsers` now also gates monitor resume and memory import, correction and deletion.

## v0.11.0 (2026-10-22)

Paired with pulse-agent v2.34.0 and pulse-ui v2.34.0. No CRD or RBAC changes. **Behaviour changes on upgrade:** the PostgreSQL NetworkPolicy now applies (its selector used to match no pod), admitting only the agent and, when enabled, the Temporal server; and turning off `spec.agent.mcp.enabled`, `spec.temporal.enabled` or `spec.temporal.ui` now removes those components instead of leaving them running. Pull requests #89–#99.

### Security
- Built with Go 1.26.10 for the `crypto/x509` advisory GO-2026-6809, flagged by govulncheck and the image scan in 1.26.9

### Fixed
- The PostgreSQL NetworkPolicy selected pods by an `app` label the StatefulSet never sets, so it isolated nothing. It now selects the StatefulSet's own `app.kubernetes.io/*` labels, and a test checks the selector against the pod template the operator actually renders
- The UI NetworkPolicy no longer admits `openshift-user-workload-monitoring` on 8080/8443; nothing there scrapes the UI
- The UI NetworkPolicy admits a router running on the host network (`policy-group.network.openshift.io/host-network`), the default IngressController setup on bare metal, vSphere and single-node clusters. Only the pod-network router namespace was admitted, so on those clusters the Route reached the UI pod and was dropped
- Scaling the UI to one replica deletes the PodDisruptionBudget it no longer needs (a one-replica PDB with `minAvailable: 1` blocks node drains)
- Turning `spec.agent.mcp.enabled` off removes the MCP server's Deployment, Service, NetworkPolicy, ServiceAccount, ClusterRole and ClusterRoleBinding. They previously kept running, with cluster-wide read still bound
- Turning `spec.temporal.enabled` off removes the Temporal server and Web UI; turning `spec.temporal.ui` off removes the Web UI. History is in PostgreSQL and survives
- CR deletion no longer hangs on clusters without the OAuthClient API: "no such kind" is treated as already gone instead of holding the finalizer
- The finalizer no longer deletes a same-named cluster-scoped object whose owner annotation names a different CR

## v0.10.0 (2026-10-18)

Paired with pulse-agent v2.33.0 and pulse-ui v2.33.0. RBAC-only release: no CRD changes.

### Fixed
- The agent's ClusterRole also grants read-only `get`/`list` on ControllerRevisions, Argo CD Applications and MachineConfigPools: StatefulSet/DaemonSet rollouts and GitOps syncs in "what changed before this incident" (pulse-agent #164), and the maintenance-blocker scanner (pulse-agent #165)
- The agent's ClusterRole grants read-only `get`/`list` on ClusterOperators and ClusterVersions (`config.openshift.io`), CustomResourceDefinitions and Routes. Without them the monitor's degraded-operators scanner, stuck-CRD check and upgrade and route-certificate inbox generators got 403 on every scan of a default install. The operator's own role gains read on CRDs so it can grant it without `escalate`. Secrets stay opt-in (`spec.agent.allowSecretAccess`)

## v0.9.0 (2026-10-17)

### Added
- `spec.render` (off by default) deploys the dashboard preview service from pulse-ui v2.32.0. The agent's view designer uses it to see a draft dashboard, rendered with the requesting user's own token, before saving it (#92). It creates:
  - a `{name}-render` Deployment and Service, with no ServiceAccount token mounted
  - a `{name}-render-token` Secret shared with the agent (`PULSE_AGENT_RENDER_URL`/`PULSE_AGENT_RENDER_TOKEN`)
  - a render-only nginx listener on the UI pod (port 8081): static UI and user-token proxies only, with `/api/agent/` returning 404
  - NetworkPolicies limiting the render pod to agent ingress and UI-render-port plus DNS egress

  Turning it off removes all of it. With render off the generated nginx config is unchanged, so upgrading does not roll the UI pods.

### Changed

- Require Go 1.26.9 and update `golang.org/x/net` to 0.60.0 with its required dependencies to address the October HTTP/TLS security findings. Source changes and CI are not evidence of a published image or a deployed upgrade.
- Integrate controller-runtime 0.25.2 and coordinated dependency/image updates; group Kubernetes module updates in Dependabot.
- Permit enabled Temporal pods through the scoped PostgreSQL ingress policy and avoid nginx Secret updates for unchanged configuration.
- Add an explicit-context cluster acceptance runner with optional disposable-namespace probes and mocked runner tests. Real cluster acceptance remains a separate required observation.

- The Temporal config init container copies with `cp -r` rather than `cp -a`. An arbitrary UID cannot chown the copies, so `-a` fails to preserve ownership on every file; busybox only warns, but GNU coreutils exits non-zero — which would hard-fail the init container under a `spec.temporal.image` override built on a coreutils base
- The envtest for that Deployment now asserts the two halves are wired *to each other* — the server container mounts the same volume the init container populates, over the path it reads — instead of checking each half in isolation, which is how a disconnected pair passed CI
- The same by-name assertion applied to the Temporal UI Deployment (v0.8.3), which was pinned by index and length: a volume renamed on one side alone leaves the mount pointing at nothing and the pod unschedulable, and "one volume, one mount, right path" is satisfied by two halves that never refer to each other

## v0.8.3 (2026-09-01)

### Fixed
- The Temporal UI pod crash-looped on OpenShift with `unable to create open ./config/docker.yaml: permission denied` — the image renders its config into a directory owned by its own UID while OpenShift assigns an arbitrary one, the same first-boot failure the server had in v0.6.0. The directory ships empty (the server's holds templates), so a writable emptyDir at `/home/ui-server/config` is the whole fix; no init container needed. Found by enabling `ui: true` on dev05, which is where all three of this family's boot failures have been found

## v0.8.2 (2026-09-01)

### The catalog now carries the whole upgrade graph
- The rendered catalog held a single entry — this release, with one `replaces` edge to the previous tag. That works only if every cluster installs every release: dev05 sat on v0.7.0, was offered a v0.8.1-only catalog whose one edge said "replaces v0.8.0", and OLM correctly reported `AtLatestKnown` at v0.7.0 forever. An upgrade graph with one edge is a graph for exactly one cluster state
- The release workflow now renders every released bundle that exists in the registry, oldest first, and chains `replaces` between consecutive rendered entries (`--print-chain` on the bump script). Bundles predating bundle publishing are skipped with a notice; failing to render the release's own bundle fails the job

## v0.8.1 (2026-09-01)

### Fixed
- `OperatorVersion` in compat.go said `0.7.0` inside the v0.8.0 image — the tag shipped before the constant was bumped, the second time this exact drift has happened (v0.4.0 was the first). The in-repo guard test can only compare against tags that already exist, so by construction it fires one release too late
- The release workflow now refuses to build any image when the constant does not match the tag. The guard test still catches drift on main between releases; the workflow gate catches it at the moment that matters

## v0.8.0 (2026-09-01)

### `spec.temporal.ui` — the workflow audit trail, visible
- `ui: true` under `spec.temporal` deploys the Temporal Web UI (`{name}-temporal-ui`, pinned image) pointed at the CR's own server. Every run's full history — each activity attempt, retry with its backoff, durable timer, approval signal — becomes inspectable without a CLI. Off by default: it is an extra surface, and the agent does not need it to run workflows
- A Service but deliberately **no Route**, with a test asserting none is created. The Temporal UI ships with no authentication and offers a terminate button to anyone who can load it; on a cluster with public Routes that would be an unauthed kill switch for every in-flight fix. `oc port-forward svc/{name}-temporal-ui 8080:8080`, or front it with an oauth-proxy first

### Fixed
- `durableAutoFix` moved to its generated (alphabetical) position in the CRD — hand-placed after the resources block, `make generate manifests` produced a diff and the CRD-sync CI job went red. The generator's output is the spec, not a starting point

## v0.7.0 (2026-09-01)

### `spec.agent.durableAutoFix` — route auto-fix through the durable workflow
- Sets `PULSE_AGENT_DURABLE_AUTOFIX` on the agent Deployment, routing the monitor's autonomous fixes through the agent's Temporal incident workflow (agent v2.29.0). Off by default; requires `spec.temporal.enabled`, and the agent falls back to inline execution when the server is unreachable
- Also resolves the v0.6.0 known-issue note: the "mounts no volumes" symptom was version skew between the deployed operator and a live prototype, not a reconciler bug

## v0.6.1 (2026-09-01)

### `spec.temporal` boots on OpenShift
- Codifies the two first-boot fixes discovered live on dev05 (see v0.6.0's known issue): the config-template init container with its emptyDir mount is now set by the reconciler as one atomic container-list write, and the `CREATEDB` grant ships as a `postgresql-start` script for fresh installs

## v0.6.0 (2026-09-01)

### `spec.temporal` — an operator-managed Temporal for durable plan runs
- The agent gained a Temporal-backed plan execution path (its `docs/TEMPORAL.md`): runs survive pod restarts and `approval_required` phases genuinely wait for a human. That path is inert without a server, and a hand-deployed server is exactly the unmanaged state this operator exists to remove
- `spec.temporal.enabled` provisions `temporalio/auto-setup` (pinned — an unpinned server image would run schema migrations on a reschedule at a moment nobody chose) against the operator's own PostgreSQL: same service, credentials from the same `{name}-pg-auth` secret, `temporal` and `temporal_visibility` databases created on first start
- The agent Deployment receives `PULSE_AGENT_TEMPORAL_HOST={name}-temporal:7233` only when enabled, so the default stays fully inert
- Dev-grade topology by design: one replica, one container, TCP probes with a generous first-boot window for cold schema setup

### Two first-boot failures, found on a live cluster
- The auto-setup image renders config into `/etc/temporal/config`, owned by UID 1000 in the image, while OpenShift assigns an arbitrary UID — the pod crash-looped on "permission denied". A `config-template` init container copies the shipped templates into an `emptyDir` any UID can write
- The app PostgreSQL user has no `CREATEDB`, so schema setup died with `pq: permission denied to create database`. Fixed with a `postgresql-start` script
- Both were prototyped live on the Deployment before being codified, and both are now covered rather than left as tribal knowledge
- Verified end to end on dev05 on 2026-09-01: `pulse-temporal` reaches `1/1 Running`, auto-setup creates `temporal` and `temporal_visibility`, and the frontend, matching and worker services start
- The `CREATEDB` grant rides the PostgreSQL pod template, which is create-only. **Enabling Temporal on an install that already has a PostgreSQL pod needs the one-time grant from the README** — without it the server clears the config-permission error and then crash-loops on `permission denied to create database` instead, which reads like the same bug and is not

> **Known issue (resolved in v0.6.1):** the symptom above was observed on dev05
> while the *live prototype* Deployment patch coexisted with the deployed
> v0.6.0 operator: the operator's reconcile owns the container list and kept
> rewriting it, wiping the prototype's main-container mount while leaving the
> init container and volume — hence "mounts no volumes at all" on the cluster.
> The codified reconciler (10ca82a, shipped in v0.6.1) sets all three pieces:
> the volume, the init container, and the server mount at
> `/etc/temporal/config`, so the deployed operator produces the complete spec.

## v0.5.1 (2026-08-25)

### Two healthy Deployments reporting different versions to users
- The agent and the UI are released as a coordinated pair carrying one version number — pulse-agent v2.26.1 ships alongside pulse-ui v2.26.1 — and nothing enforced that on a cluster. The operator faithfully reconciles whatever `spec.agent.image` and `spec.ui.image` say and had no opinion about whether they agree
- Measured on dev05: `spec.agent.image` v2.25.0 ran against `spec.ui.image` v2.24.0 for weeks. Both Deployments healthy, CI green, CR `Ready`, two surfaces reporting different versions. Nothing was wrong enough to notice
- New `AgentUIVersionsMatch` condition (`internal/controller/skew.go`) compares the two pinned tags. Deliberately a comparison *between the two images*, not against the newest published release: asking "is this the latest?" would put a registry call in the reconcile path, make the answer depend on network reachability, and flag clusters that have legitimately stayed on an older version. Asking "do these two agree?" needs no network and encodes the actual project invariant
- It reports; it does not block. A rollout patches the two images at slightly different moments, so a transient mismatch is normal and clears itself — blocking would wedge the upgrade that resolves the condition. Digest-pinned and untagged references are skipped rather than guessed at, and a registry port (`registry.local:5000/pulse-agent`) is not mistaken for a tag

## v0.5.0 (2026-08-25)

### The CR's trust level never reached the agent
- `spec.agent.trustLevel` had been a no-op on every deployment since the knob shipped: the operator injected `PULSE_AGENT_TRUST_LEVEL`, the agent's settings only bound `PULSE_AGENT_MAX_TRUST_LEVEL`, and both sides defaulting to 2 kept the mismatch invisible. Verified live: CR patched to `trustLevel: 3`, new pod running with `PULSE_AGENT_TRUST_LEVEL=3` in its environment, agent still gating every fix at trust 2
- The operator now emits **both** names from the same CR field, so an older agent image keeps seeing the name it always saw and a fixed agent (>= v2.24.0, which accepts either) finally sees the CR's value
- `OperatorVersion` synced to the released tag; the version-guard test catches the drift on the next push rather than at the next release

## v0.4.2 (2026-08-22)

- Let the agent authenticate the caller so admin endpoints work (#59)

## v0.4.1 (2026-08-21)

- CI builds the OLM catalog, so a tag is a release people can actually install (#57), with the `opm` signature-policy fix that made pulls work (#58)
- `spec.agent.adminUsers`: the CR can name who may change how the agent behaves (#55)
- Base image pinned to a digest predating two curl CVEs (#56)
- Docs: state plainly that this is OpenShift-only, and name one canonical install path (#54)
- `OperatorVersion` constant synced to the released v0.4.0 (#53)

## v0.4.0 (2026-08-19)

- Deployment-lifecycle autonomy: self-heal, agent-version compatibility gate, memory metrics, and upgrade-duration tracking (#49)
- `operator-sdk scorecard` actually runs in CI; CSV spec/status descriptors added (#48)
- `alm-examples` status block no longer shows values the controller never emits (#50)

## v0.3.0 (2026-08-19)

- Per-component status conditions (`AgentReady`, `DatabaseReady`, `UIReady`), self-heal Events, and automatic rollback to the last known-healthy image (#46)
- CI guards for the two live-cluster incidents from that day's OLM rollout (#44)
- `pulse-operator-bundle` and `-catalog` published as public quay.io repos (#45)

## v0.2.0 (2026-08-19)

- Fixed the root cause of every WebSocket connection failure: oauth-proxy request logging (#39)
- Fixed the PostgreSQL pod-0 create/delete storm and the StatefulSet reconcile-conflict loop (#38)
- The agent WS token was readable cluster-wide via the nginx ConfigMap — moved into a Secret
- Leader election disabled: a single-replica Deployment gets no benefit from it (#40)
- `scripts/olm-uninstall.py` for properly uninstalling OLM operators (#41)

## v0.1.0 (2026-08-14)

Initial release: one `OpenShiftPulse` custom resource reconciles the full stack —
AI SRE agent, React UI, and PostgreSQL — on OpenShift 4.12+.
