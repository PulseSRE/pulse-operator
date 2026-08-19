package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

const finalizerName = "pulse.ai/cleanup"

// OpenShiftPulseReconciler is the root reconciler for the OpenShiftPulse CRD.
// It orchestrates the PostgreSQL, Agent, and UI sub-reconcilers, then syncs status.
type OpenShiftPulseReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	UIReconciler *UIReconciler
	Recorder     record.EventRecorder
}

// +kubebuilder:rbac:groups=pulse.ai,resources=openshiftpulses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pulse.ai,resources=openshiftpulses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims;secrets;serviceaccounts,verbs=get;list;watch;create;update;patch;delete

func (r *OpenShiftPulseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	pulse := &pulsev1alpha1.OpenShiftPulse{}
	if err := r.Get(ctx, req.NamespacedName, pulse); err != nil {
		if apierrors.IsNotFound(err) {
			// CR was deleted — nothing to do.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Finalizer / deletion guard.
	if !pulse.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(pulse, finalizerName) {
			if err := r.deleteClusterScopedResources(ctx, pulse); err != nil {
				r.Recorder.Eventf(pulse, corev1.EventTypeWarning, "DeleteFailed",
					"Some cluster-scoped resources could not be deleted: %v", err)
				return ctrl.Result{}, err
			}
			// pg-auth Secret and the PG data PVC deliberately outlive CR
			// deletion by default (see PostgreSQLReconciler.reconcilePGSecret's
			// doc comment) — only remove them here if the CR explicitly opted
			// into a full teardown via annotationDeleteData.
			pg := &PostgreSQLReconciler{Client: r.Client, Scheme: r.Scheme, Recorder: r.Recorder}
			if err := pg.deletePGDataOnRequest(ctx, pulse); err != nil {
				r.Recorder.Eventf(pulse, corev1.EventTypeWarning, "DeleteFailed",
					"PostgreSQL data/credentials could not be deleted: %v", err)
				return ctrl.Result{}, err
			}
			r.Recorder.Event(pulse, corev1.EventTypeNormal, "Deleted", "Cluster-scoped resources cleaned up")
			controllerutil.RemoveFinalizer(pulse, finalizerName)
			if err := r.Update(ctx, pulse); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(pulse, finalizerName) {
		controllerutil.AddFinalizer(pulse, finalizerName)
		return ctrl.Result{}, r.Update(ctx, pulse)
	}

	logger.Info("Reconciling OpenShiftPulse", "name", pulse.Name, "namespace", pulse.Namespace)

	// The CRD does not require an AI backend (some tests/dev setups deploy the
	// stack without one), but a real deployment without either configured will
	// silently run an agent with no credentials. Surface that loudly instead of
	// letting it fail opaquely inside the agent container.
	if !hasAIBackend(pulse) {
		logger.Info("no AI backend configured — agent will start without AI credentials", "name", pulse.Name)
		r.Recorder.Event(pulse, corev1.EventTypeWarning, "NoAIBackendConfigured",
			"Neither spec.vertexAI nor spec.anthropicApiKey is set — the agent has no AI backend and will not function")
	}

	// 1. Reconcile PostgreSQL sub-resources.
	dbURL, pgResult, err := r.reconcilePostgres(ctx, pulse)
	if err != nil {
		logger.Error(err, "postgres reconcile failed")
		r.Recorder.Eventf(pulse, corev1.EventTypeWarning, "ReconcileFailed", "PostgreSQL reconcile failed: %v", err)
		reconcileErrorsTotal.WithLabelValues("postgres").Inc()
		return ctrl.Result{}, fmt.Errorf("reconcilePostgres: %w", err)
	}
	if pgResult.RequeueAfter > 0 {
		// A genuine "come back shortly" signal (e.g. waiting for a
		// selector-mismatched StatefulSet to finish terminating before it can
		// be recreated) — not a failure, so no Warning event.
		logger.Info("PostgreSQL reconcile requeued", "name", pulse.Name)
		r.flushInstallingStatus(ctx, pulse)
		return pgResult, nil
	}

	// 2. Reconcile Agent sub-resources, providing the database URL.
	if agentResult, err := r.reconcileAgent(ctx, pulse, dbURL); err != nil {
		logger.Error(err, "agent reconcile failed")
		r.Recorder.Eventf(pulse, corev1.EventTypeWarning, "ReconcileFailed", "Agent reconcile failed: %v", err)
		reconcileErrorsTotal.WithLabelValues("agent").Inc()
		return ctrl.Result{}, fmt.Errorf("reconcileAgent: %w", err)
	} else if agentResult.RequeueAfter > 0 {
		logger.Info("Agent reconcile requeued", "name", pulse.Name)
		r.flushInstallingStatus(ctx, pulse)
		return agentResult, nil
	}

	// 2a. NetworkPolicies — protect UI and PostgreSQL pods.
	if err := r.reconcileNetworkPolicies(ctx, pulse); err != nil {
		logger.Error(err, "network policy reconcile failed")
		reconcileErrorsTotal.WithLabelValues("network_policies").Inc()
		return ctrl.Result{}, fmt.Errorf("reconcileNetworkPolicies: %w", err)
	}

	// 2b. PodDisruptionBudget — protect UI when replicas > 1.
	if err := r.reconcileUIPodsDisruptionBudget(ctx, pulse); err != nil {
		logger.Error(err, "PDB reconcile failed")
		reconcileErrorsTotal.WithLabelValues("pdb").Inc()
		return ctrl.Result{}, fmt.Errorf("reconcileUIPodsDisruptionBudget: %w", err)
	}

	// 2c. Detect cluster topology (used by the MCP reconciler).
	info := DetectClusterInfo(ctx, r.Client)

	// 2d. Optional: Monitoring — ServiceMonitor + PrometheusRule. Deliberately
	// reconciled before the UI/Route step below: alerting (PulseAgentDown,
	// PulsePostgreSQLDown) must exist even while the Route is still being
	// admitted, or stuck for any reason — otherwise there'd be no alert to
	// fire about the very thing blocking a healthy install.
	if monitoringEnabled(pulse) {
		mr := &MonitoringReconciler{Client: r.Client, Scheme: r.Scheme}
		if err := mr.reconcileMonitoring(ctx, pulse); err != nil {
			logger.Error(err, "monitoring reconcile failed")
			reconcileErrorsTotal.WithLabelValues("monitoring").Inc()
			return ctrl.Result{}, fmt.Errorf("reconcileMonitoring: %w", err)
		}
	}

	// 2e. Optional: MCP server Deployment + Service. Also reconciled before the
	// UI/Route step — the agent depends on the MCP Service being reachable, and
	// neither should ever be blocked on the Route (an unrelated resource).
	if pulse.Spec.Agent.MCP.Enabled {
		mcpr := &MCPReconciler{Client: r.Client, Scheme: r.Scheme, Recorder: r.Recorder}
		if err := mcpr.reconcileMCP(ctx, pulse, info); err != nil {
			logger.Error(err, "MCP reconcile failed")
			reconcileErrorsTotal.WithLabelValues("mcp").Inc()
			return ctrl.Result{}, fmt.Errorf("reconcileMCP: %w", err)
		}
	}

	// 3. Reconcile UI sub-resources (Route, OAuthClient, Deployment, Service, etc.).
	uiResult, err := r.UIReconciler.reconcileUI(ctx, pulse)
	if err != nil {
		logger.Error(err, "UI reconcile failed")
		reconcileErrorsTotal.WithLabelValues("ui").Inc()
		return ctrl.Result{}, fmt.Errorf("reconcileUI: %w", err)
	}
	// reconcileUI sets pulse.Status.RouteHost and pulse.Status.UIAvailable directly.
	// If the Route hostname is not yet assigned, flush the current status and requeue
	// so the CR reflects the installing state rather than showing stale/empty status.
	if uiResult.RequeueAfter > 0 {
		r.flushInstallingStatus(ctx, pulse)
		return uiResult, nil
	}

	// 4. Determine health of each component and sync CR status.
	agentReady := r.isDeploymentReady(ctx, agentResourceName(pulse.Name), pulse.Namespace)
	pgReady := r.isStatefulSetReady(ctx, pulse.Name+"-openshift-sre-agent-postgresql", pulse.Namespace)

	pulse.Status.AgentHealthy = agentReady
	pulse.Status.DatabaseReady = pgReady

	// Populate agentVersion from the Deployment image tag.
	if deploy := r.agentDeployment(ctx, agentResourceName(pulse.Name), pulse.Namespace); deploy != nil {
		if len(deploy.Spec.Template.Spec.Containers) > 0 {
			img := deploy.Spec.Template.Spec.Containers[0].Image
			if idx := strings.LastIndex(img, ":"); idx >= 0 {
				pulse.Status.AgentVersion = img[idx+1:]
			} else {
				pulse.Status.AgentVersion = img
			}
		}
	}

	// Item 3: advisory-only observed-vs-requested memory metrics (see
	// resource_metrics.go). Never fatal, never blocks anything below —
	// gracefully skips publishing an observed sample when metrics.k8s.io
	// isn't reachable.
	reconcileObservedMemoryMetrics(ctx, r.Client, pulse, agentResourceName(pulse.Name), "agent", agentRequestedMemoryBytes(pulse))
	reconcileObservedMemoryMetrics(ctx, r.Client, pulse, uiResourceName(pulse.Name), "ui", uiRequestedMemoryBytes(pulse))

	_, _, agentUpgrading, uiUpgrading := r.syncPhaseAndConditions(pulse, agentReady, pgReady)

	// Auto-rollback: an agent/UI image change stuck Upgrading past
	// upgradeHealthTimeout gets reverted here. The spec patch itself
	// triggers a fresh reconcile (this reconciler watches OpenShiftPulse
	// directly), so return immediately rather than also writing status in
	// this same pass — the next pass recomputes everything from the
	// reverted spec.
	if rolledBack, err := r.reconcileAutoRollback(ctx, pulse, agentUpgrading, uiUpgrading); err != nil {
		reconcileErrorsTotal.WithLabelValues("auto_rollback").Inc()
		return ctrl.Result{}, fmt.Errorf("reconcileAutoRollback: %w", err)
	} else if rolledBack {
		return ctrl.Result{}, nil
	}

	pulse.Status.ObservedGeneration = pulse.Generation

	if err := r.Status().Update(ctx, pulse); err != nil {
		reconcileErrorsTotal.WithLabelValues("status_update").Inc()
		return ctrl.Result{}, fmt.Errorf("status update: %w", err)
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// flushInstallingStatus writes the "Installing" phase to the CR before an early
// requeue return. Every requeue path in Reconcile exits before the status sync at
// the bottom of the function, so without this the CR keeps reporting whatever
// phase it last had — including a stale "Running" — for the entire window a
// component is being torn down and recreated (a selector-mismatched StatefulSet
// or Deployment during Helm-to-operator migration, or a Route awaiting
// admission). That window is measured in minutes, and it is exactly when an
// operator is watching `oc get openshiftpulse` for a signal.
//
// The update error is deliberately not propagated: the caller is already
// returning a requeue, so a failed status write is retried on the next pass and
// must not turn a healthy "come back shortly" into a reconcile failure.
func (r *OpenShiftPulseReconciler) flushInstallingStatus(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) {
	pulse.Status.Phase = "Installing"
	if err := r.Status().Update(ctx, pulse); err != nil {
		log.FromContext(ctx).V(1).Info("could not flush Installing status before requeue", "error", err.Error())
	}
}

// deleteClusterScopedResources removes the cluster-scoped resources that are not
// owned by the CR (and therefore not garbage-collected by the K8s GC on deletion).
// All deletions are attempted regardless of individual failures; errors are aggregated
// so the caller sees the complete failure set rather than stopping at the first.
func (r *OpenShiftPulseReconciler) deleteClusterScopedResources(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	var errs []error

	del := func(obj client.Object, desc string) {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete %s: %w", desc, err))
		}
	}

	del(&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: agentClusterRoleName(pulse.Name, pulse.Namespace)}}, "agent ClusterRole")
	del(&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: uiClusterRoleName(pulse.Name, pulse.Namespace)}}, "UI ClusterRole")
	del(&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: agentClusterRoleName(pulse.Name, pulse.Namespace)}}, "agent ClusterRoleBinding")
	del(&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: agentMonitoringViewBindingName(pulse.Name, pulse.Namespace)}}, "agent monitoring-view ClusterRoleBinding")
	del(&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: uiClusterRoleName(pulse.Name, pulse.Namespace)}}, "UI ClusterRoleBinding")
	// system:auth-delegator binding (reconcileUIAuthDelegatorBinding) — same
	// orphan risk as the two bindings above: cluster-scoped, can't carry an
	// OwnerReference to a namespaced CR, so it must be cleaned up here too.
	del(&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: uiClusterRoleName(pulse.Name, pulse.Namespace) + "-auth-delegator"}}, "UI auth-delegator ClusterRoleBinding")
	// MCP server's ClusterRole/ClusterRoleBinding — same orphan risk: cluster-scoped,
	// reconciled unconditionally on delete regardless of whether MCP is (or ever
	// was) enabled, matching the pattern of the other cluster-scoped resources here.
	del(&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: mcpClusterRoleName(pulse.Name, pulse.Namespace)}}, "MCP ClusterRole")
	del(&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: mcpClusterRoleName(pulse.Name, pulse.Namespace)}}, "MCP ClusterRoleBinding")

	// Best-effort migration cleanup: these three reconcilers already migrate
	// away from the old, non-namespace-qualified names on every normal
	// reconcile (see deleteStaleUnqualifiedClusterScopedResource), but a CR
	// deleted before ever completing a full reconcile pass could still have
	// old-named resources around. Owner-uid-checked, so this can never touch
	// a same-named resource belonging to a different CR (the exact collision
	// this naming scheme exists to prevent).
	if err := deleteStaleUnqualifiedClusterScopedResource(ctx, r.Client, &rbacv1.ClusterRole{}, agentResourceName(pulse.Name), pulse); err != nil {
		errs = append(errs, fmt.Errorf("delete stale unqualified agent ClusterRole: %w", err))
	}
	if err := deleteStaleUnqualifiedClusterScopedResource(ctx, r.Client, &rbacv1.ClusterRoleBinding{}, agentResourceName(pulse.Name), pulse); err != nil {
		errs = append(errs, fmt.Errorf("delete stale unqualified agent ClusterRoleBinding: %w", err))
	}
	if err := deleteStaleUnqualifiedClusterScopedResource(ctx, r.Client, &rbacv1.ClusterRole{}, uiClusterRoleNameUnqualified(pulse.Name), pulse); err != nil {
		errs = append(errs, fmt.Errorf("delete stale unqualified UI ClusterRole: %w", err))
	}
	if err := deleteStaleUnqualifiedClusterScopedResource(ctx, r.Client, &rbacv1.ClusterRoleBinding{}, uiClusterRoleNameUnqualified(pulse.Name), pulse); err != nil {
		errs = append(errs, fmt.Errorf("delete stale unqualified UI ClusterRoleBinding: %w", err))
	}
	if err := deleteStaleUnqualifiedClusterScopedResource(ctx, r.Client,
		&rbacv1.ClusterRoleBinding{}, uiClusterRoleNameUnqualified(pulse.Name)+"-auth-delegator", pulse); err != nil {
		errs = append(errs, fmt.Errorf("delete stale unqualified UI auth-delegator ClusterRoleBinding: %w", err))
	}
	if err := deleteStaleUnqualifiedClusterScopedResource(ctx, r.Client, &rbacv1.ClusterRole{}, mcpResourceName(pulse.Name), pulse); err != nil {
		errs = append(errs, fmt.Errorf("delete stale unqualified MCP ClusterRole: %w", err))
	}
	if err := deleteStaleUnqualifiedClusterScopedResource(ctx, r.Client, &rbacv1.ClusterRoleBinding{}, mcpResourceName(pulse.Name), pulse); err != nil {
		errs = append(errs, fmt.Errorf("delete stale unqualified MCP ClusterRoleBinding: %w", err))
	}

	// OAuthClient — cluster-scoped OpenShift resource; use unstructured because the
	// oauth.openshift.io API group is not registered in the controller-runtime scheme.
	oac := &unstructured.Unstructured{}
	oac.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "oauth.openshift.io",
		Version: "v1",
		Kind:    "OAuthClient",
	})
	oac.SetName(oauthClientName(pulse.Name, pulse.Namespace))
	del(oac, "OAuthClient")

	return errors.Join(errs...)
}

// reconcilePostgres delegates to PostgreSQLReconciler and returns the database URL.
// Returns a non-zero ctrl.Result.RequeueAfter (with a nil error) when a step
// needs a short requeue rather than a failure — see
// PostgreSQLReconciler.reconcilePGStatefulSet.
func (r *OpenShiftPulseReconciler) reconcilePostgres(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) (string, ctrl.Result, error) {
	pg := &PostgreSQLReconciler{Client: r.Client, Scheme: r.Scheme, Recorder: r.Recorder}
	return pg.reconcilePostgres(ctx, pulse)
}

// reconcileAgent runs all agent sub-reconciler steps in order.
// dbURL is available for future use (e.g. injecting directly rather than via Secret).
// Returns a non-zero ctrl.Result.RequeueAfter (with a nil error) when a step
// needs a short requeue rather than a failure — see AgentReconciler.reconcileDeployment.
func (r *OpenShiftPulseReconciler) reconcileAgent(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse, dbURL string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	_ = dbURL // consumed via K8s Secret by the agent Deployment today; reserved for future direct injection

	ar := &AgentReconciler{Client: r.Client, Scheme: r.Scheme, Recorder: r.Recorder}

	type step struct {
		name string
		fn   func(context.Context, *pulsev1alpha1.OpenShiftPulse) error
	}
	steps := []step{
		{"ServiceAccount", ar.reconcileServiceAccount},
		{"ClusterRole", ar.reconcileClusterRole},
		{"ClusterRoleBinding", ar.reconcileClusterRoleBinding},
		{"MonitoringViewBinding", ar.reconcileAgentMonitoringViewBinding},
		{"WSTokenSecret", ar.reconcileWSTokenSecret},
		{"MemoryPVC", ar.reconcileMemoryPVC},
	}

	for _, s := range steps {
		if err := s.fn(ctx, pulse); err != nil {
			logger.Error(err, "agent reconcile step failed", "step", s.name)
			return ctrl.Result{}, fmt.Errorf("%s: %w", s.name, err)
		}
	}

	// Deployment is not in the generic loop above: it can return a genuine
	// (non-error) requeue signal instead of a failure.
	if result, err := ar.reconcileDeployment(ctx, pulse); err != nil {
		logger.Error(err, "agent reconcile step failed", "step", "Deployment")
		return ctrl.Result{}, fmt.Errorf("deployment: %w", err)
	} else if result.RequeueAfter > 0 {
		return result, nil
	}

	if err := ar.reconcileService(ctx, pulse); err != nil {
		logger.Error(err, "agent reconcile step failed", "step", "Service")
		return ctrl.Result{}, fmt.Errorf("service: %w", err)
	}
	return ctrl.Result{}, nil
}

// isDeploymentReady returns true when the named Deployment has at least one ready replica.
func (r *OpenShiftPulseReconciler) isDeploymentReady(ctx context.Context, name, ns string) bool {
	deploy := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, deploy); err != nil {
		return false
	}
	return deploy.Status.ReadyReplicas > 0
}

// agentDeployment returns the agent Deployment object, or nil if not found.
func (r *OpenShiftPulseReconciler) agentDeployment(ctx context.Context, name, ns string) *appsv1.Deployment {
	deploy := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, deploy); err != nil {
		return nil
	}
	return deploy
}

// isStatefulSetReady returns true when the named StatefulSet has at least one ready replica.
func (r *OpenShiftPulseReconciler) isStatefulSetReady(ctx context.Context, name, ns string) bool {
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, sts); err != nil {
		return false
	}
	return sts.Status.ReadyReplicas > 0
}

// SetupWithManager registers this reconciler with the controller manager.
// Watches OpenShiftPulse CRs and all namespace-scoped sub-resources it owns.
// Note: configv1.Ingress (cluster-scoped) is NOT watched here — ClusterDetector
// reads it lazily on first reconcile via sync.Once, which is sufficient since
// the cluster ingress domain almost never changes at runtime.
func (r *OpenShiftPulseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pulsev1alpha1.OpenShiftPulse{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ServiceAccount{}).
		Complete(r)
}
