# Changelog

All notable changes to the Pulse Operator are documented in this file.

Reconstructed from the tagged release history; entries describe what each tag
actually contains, taken from its commits.

## Unreleased

- The Temporal config init container copies with `cp -r` rather than `cp -a`. An arbitrary UID cannot chown the copies, so `-a` fails to preserve ownership on every file; busybox only warns, but GNU coreutils exits non-zero — which would hard-fail the init container under a `spec.temporal.image` override built on a coreutils base
- The envtest for that Deployment now asserts the two halves are wired *to each other* — the server container mounts the same volume the init container populates, over the path it reads — instead of checking each half in isolation, which is how a disconnected pair passed CI

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
