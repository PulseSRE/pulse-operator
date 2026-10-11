# Disposable cluster incident acceptance

This kit prepares three controlled incidents for the next available cluster session. Nothing here has been deployed or established product acceptance. It reuses the operator acceptance runner for installation observations; the fixture generator and evidence collector serve only the incident experiments.

## Prepare offline

Use Python 3.10 or newer. Set actual values before any cluster session. The image must contain `python3`, support an arbitrary non-root UID with a read-only root filesystem, and be pullable by the test namespace. Choose a tested digest; the kit deliberately supplies no invented image.

```bash
# Run from the pulse-operator checkout root.
export OPERATOR="$(pwd)"
export KIT="$OPERATOR/examples/cluster-test-kit"
export CONTEXT='YOUR_EXPLICIT_CONTEXT'
export NAMESPACE='YOUR_EXISTING_DISPOSABLE_NAMESPACE'
export PULSE='YOUR_PULSE_CR'
export RUN_ID='r20261012a' # choose a fresh ID for each session
export PYTHON_IMAGE='YOUR_REGISTRY/PYTHON_IMAGE@sha256:YOUR_64_HEX_DIGEST'
export FIXTURES="$KIT/generated-$RUN_ID"
export EVIDENCE="$KIT/evidence-$RUN_ID"

python3 "$KIT/fixtures.py" --namespace "$NAMESPACE" --run-id "$RUN_ID" \
  --image "$PYTHON_IMAGE" --output "$FIXTURES"
python3 "$KIT/collect.py" --context "$CONTEXT" --namespace "$NAMESPACE" --run-id "$RUN_ID"
python3 "$OPERATOR/scripts/cluster-acceptance.py" \
  --context "$CONTEXT" --namespace "$NAMESPACE" --pulse "$PULSE"
python3 -m unittest discover -s "$KIT" -p test_offline.py -v
```

All four commands above are offline. Generation refuses to overwrite an existing output directory. JSON files are valid Kubernetes manifest input, not server-validated until the cluster is available. `identity.json` is evidence metadata, not a Kubernetes manifest: never apply the whole generated directory.

## Establish the session and prerequisite evidence

The following section is for the available, authorized cluster session only. Do not change the current kubeconfig context. Every command explicitly targets the chosen context and namespace. Verify the cluster identity with your administrator, user identity, namespace ownership and existing Pulse installation. Use trust level 2 with explicit affirmative approval, an actual configured provider, and the real authenticated browser; record the chosen approval path. Do not enable unattended remediation for these experiments.

```bash
mkdir -m 700 "$EVIDENCE"
oc --context "$CONTEXT" --namespace "$NAMESPACE" --request-timeout=20s whoami
oc --context "$CONTEXT" --namespace "$NAMESPACE" --request-timeout=20s get namespace "$NAMESPACE" -o json > "$EVIDENCE/namespace.json"
oc --context "$CONTEXT" --namespace "$NAMESPACE" --request-timeout=20s auth can-i patch deployments.apps
python3 "$OPERATOR/scripts/cluster-acceptance.py" \
  --context "$CONTEXT" --namespace "$NAMESPACE" --pulse "$PULSE" \
  --execute --report "$EVIDENCE/operator-before.json"
```

Require an existing namespace labeled `pulse.ai/acceptance=disposable`; do not relabel a production namespace. If Pulse is installed in a different namespace, pass that installation namespace to the operator runner and record both namespaces. The workloads remain in `NAMESPACE`. Stop if namespace, target identity, provider access, installation readiness, RBAC, or image readiness cannot be established. The runner's `acceptance: incomplete` is expected and must not be relabeled as end-to-end acceptance.

Validate each of the six workload files with `oc --context "$CONTEXT" --namespace "$NAMESPACE" --request-timeout=20s apply --dry-run=server -f "$FIXTURES/CASE-STATE.json"` before applying. Inspect admission/SCC results; do not weaken security settings to force a pass. Check no Deployment with the generated name already exists. A collision requires a fresh run ID, not an overwrite.

## Execute one case at a time

For each `CASE` in `crashloop`, `rollout`, and `pending`, run the sequence below. Ensure the previous case is complete before continuing.

```bash
export CASE=crashloop # then rollout, then pending
export TARGET="pulse-test-$RUN_ID-$CASE"
oc --context "$CONTEXT" --namespace "$NAMESPACE" --request-timeout=20s apply -f "$FIXTURES/$CASE-healthy.json"
oc --context "$CONTEXT" --namespace "$NAMESPACE" --request-timeout=20s rollout status "deployment/$TARGET" --timeout=120s
python3 "$KIT/collect.py" --context "$CONTEXT" --namespace "$NAMESPACE" --run-id "$RUN_ID" \
  --execute --output "$EVIDENCE/$CASE-baseline"
# Apply only after the baseline is affirmatively ready and its imageID is recorded.
oc --context "$CONTEXT" --namespace "$NAMESPACE" --request-timeout=20s apply -f "$FIXTURES/$CASE-fault.json"
python3 "$KIT/collect.py" --context "$CONTEXT" --namespace "$NAMESPACE" --run-id "$RUN_ID" \
  --execute --output "$EVIDENCE/$CASE-fault" --samples 5 --interval 30
```

For `pending`, first verify no node matches `pulse.ai/acceptance-node=$RUN_ID` using an explicitly targeted `oc get nodes -l "pulse.ai/acceptance-node=$RUN_ID" -o json`. Node reads are cluster-scoped and may require an administrator. Do not alter node labels. Any match invalidates the fixture; choose another ID and regenerate. Missing permission means this prerequisite is unverified.

| Case | Expected evidence and diagnosis | Appropriate proposed repair and recovery proof |
| --- | --- | --- |
| Crashloop configuration fault | The new Pod repeatedly exits 78; current/previous logs identify `PULSE_TEST_MODE=bad-config`; restarts increase. Capture failing container, owner UID, generation and events. | Change this container's environment back to `healthy` through the chosen Pulse approval flow. A restart alone retains the fault and must not count as repair. |
| Failed rollout | Baseline establishes a healthy revision; new ReplicaSet runs `app-fault`, exits 1 with the deterministic RuntimeError, and cannot become ready. After the 60s progress deadline, expect ProgressDeadlineExceeded, subject to controller timing. | Approve a specific rollback to the recorded healthy revision or correction of the template. Confirm current generation, intended environment/revision, updated/available/ready replicas. The old healthy replica retained by maxUnavailable=0 must not mask the failed new rollout. |
| Pending scheduling constraint | New Pod has PodScheduled=False and FailedScheduling events for the unsatisfied nodeSelector. It has no nodeName. Show requests, selectors and scheduler events rather than inferring capacity exhaustion. | Approve removal of the exact test selector, preserving other scheduling constraints. Require a scheduled ready replacement and controller convergence; deleting the Pod alone does not repair its template. |

All fixtures use RollingUpdate with one old healthy replica retained while a new replica fails. Inspect the failing/new ReplicaSet and generation, not aggregate availability alone. If admission, quota, image pull or insufficient capacity blocks the fixture before the intended symptom, record a failed prerequisite; do not misclassify it as the intended incident.

Use the real browser to discover the finding, inspect cited observations, investigate with the actual provider, and review the exact change. Save finding/action/workflow IDs, user identity, explicit target cluster/namespace/UID/resourceVersion, proposed change, risk/rollback, approval decision, execution result and timestamps. Do not use `oc apply ...healthy.json` as a substitute for the product repair. That file is a documented manual recovery baseline if the experiment must be aborted; manual recovery is not product success.

After approved execution, capture fresh evidence and a sustained window:

```bash
python3 "$KIT/collect.py" --context "$CONTEXT" --namespace "$NAMESPACE" --run-id "$RUN_ID" \
  --execute --output "$EVIDENCE/$CASE-recovery" --samples 11 --interval 30
```

This provides at least five minutes between first and last sample, plus collection time. Require owning workload presence; observedGeneration >= generation; desired, updated, ready and available replicas converged; all required containers ready; no increasing restart counts on the same Pod UID; and no new symptom. Record replacement UIDs separately rather than interpreting reset counters as stability. A failed read, missing health, API error or ambiguous observation is unverified, not recovered. Collector exit zero only means the requested object reads completed, never that an incident passed. Previous logs may legitimately be absent; all log failures are preserved for assessment. Evidence can contain application logs, environment configuration, and unrelated namespace events; keep it private and redact before sharing. Secrets are never queried.

## Required negative and durability checks

Run these in separate attempts with fresh proposal IDs. Preserve each before/after object JSON and audit record. Operator reconcile changes can alter resourceVersion independently: compare the exact proposed fields and attribute writes using API audit events where available.

1. **Denied approval:** reject the proposal in the UI. Verify no proposed mutation occurred, no execution was recorded as successful, and a later affirmative approval requires a valid new pending proposal.
2. **Stale proposal:** after generating a proposal, change only a benign annotation on that exact Deployment with explicit context/namespace and the last observed resourceVersion (`oc --context "$CONTEXT" --namespace "$NAMESPACE" --request-timeout=20s annotate deployment "$TARGET" pulse.ai/stale-test=... --resource-version=...`). Approve the stale proposal. It must be rejected or require refreshed inspection/reapproval, with no stale mutation. Capture UID and resourceVersion before/after.
3. **Permission denial:** use an administrator-provided test identity that can observe but cannot patch the fixture. Do not modify production RBAC. Confirm an attempted repair is denied, no service-account privilege bypass occurs, and UI reports denial rather than recovery.
4. **Missing health / failed repair:** revoke observation access for the test identity through an administrator-managed disposable role, or use a separately approved repair that leaves the fault intact. Missing evidence stays unverified; completed execution alone must not become success.
5. **Recurrence:** after sustained recovery, deliberately reapply the same case's fault manifest as a separately recorded fixture mutation. Verify the symptom recurs, the finding/result changes to recurrent/unresolved, and the earlier verified timestamp does not imply ongoing health. Repair again through a fresh approval flow.
6. **Persistence:** reload the UI and reconnect. Verify findings, action-linked evidence, approval decisions, execution and verification states survive. For cross-cluster safety, use a second explicitly authorized context and verify a proposal from context A cannot execute after selecting B; without that second cluster, mark this check untested.

## Separate capacity scenario

The deterministic selector fixture does not prove capacity diagnosis. For a separate capacity run, have the administrator provide node allocatable CPU, currently allocated requests, namespace quota/LimitRange, and PVC/taint/affinity constraints. Record these observations. Generate a fresh `pending` healthy baseline. Copy its fault manifest and remove the test nodeSelector; set the workload CPU **request and limit** above the largest eligible node's allocatable CPU using measured values. Keep replicas=1; never create a load generator or exhaust actual cluster resources. Require server dry-run/admission success and sufficient namespace quota to create the pending Pod.

Expected evidence is FailedScheduling/Insufficient cpu with no confounding selector, quota, taint or storage failure. If quota prevents Pod creation, that is a separate quota incident, not a capacity success. Approve a justified lower request that fits a measured eligible node, respecting application needs and limits; never claim that deleting/restarting alone adds capacity. Then require scheduled/container-ready replacement, replica convergence and the same five-minute observation. Do not scale nodes or alter cluster quotas as part of this kit. An administrator must select the values: this kit intentionally does not invent capacity numbers.

## Cleanup and results

Do not delete the namespace or use broad label deletion. Record the exact Deployment names/UIDs from this run. After all evidence is saved and experiments are stopped, inspect each named Deployment and delete only after confirming its UID still matches the recorded fixture; use a reviewed API DeleteOptions UID precondition if supported by your administrative workflow. Do not delete a replacement object on name alone. Retain all fixture files and image digests.

Complete one result record per case with: prerequisite status; hypothesis and cited evidence; proposal/action IDs; approval/audit; mutation/precondition result; health samples and observation duration; final outcome (`verified`, `failed`, or `unverifiable`); recurrence; reload persistence; and remaining gaps. Keep operator readiness report, product incident outcome and provider evaluation results separate. Passing offline tests below proves helper behavior only.
