package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
				return ctrl.Result{}, err
			}
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

	// 1. Reconcile PostgreSQL sub-resources.
	dbURL, err := r.reconcilePostgres(ctx, pulse)
	if err != nil {
		logger.Error(err, "postgres reconcile failed")
		return ctrl.Result{}, fmt.Errorf("reconcilePostgres: %w", err)
	}

	// 2. Reconcile Agent sub-resources, providing the database URL.
	if err := r.reconcileAgent(ctx, pulse, dbURL); err != nil {
		logger.Error(err, "agent reconcile failed")
		return ctrl.Result{}, fmt.Errorf("reconcileAgent: %w", err)
	}

	// 2a. NetworkPolicies — protect UI and PostgreSQL pods.
	if err := r.reconcileNetworkPolicies(ctx, pulse); err != nil {
		logger.Error(err, "network policy reconcile failed")
		return ctrl.Result{}, fmt.Errorf("reconcileNetworkPolicies: %w", err)
	}

	// 3. Reconcile UI sub-resources (Route, OAuthClient, Deployment, Service, etc.).
	uiResult, err := r.UIReconciler.reconcileUI(ctx, pulse)
	if err != nil {
		logger.Error(err, "UI reconcile failed")
		return ctrl.Result{}, fmt.Errorf("reconcileUI: %w", err)
	}
	// reconcileUI sets pulse.Status.RouteHost and pulse.Status.UIAvailable directly.
	// If the Route hostname is not yet assigned, propagate the requeue.
	if uiResult.RequeueAfter > 0 {
		return uiResult, nil
	}

	// 3a. PodDisruptionBudget — protect UI when replicas > 1.
	if err := r.reconcileUIPodsDisruptionBudget(ctx, pulse); err != nil {
		logger.Error(err, "PDB reconcile failed")
		return ctrl.Result{}, fmt.Errorf("reconcileUIPodsDisruptionBudget: %w", err)
	}

	// 4. Detect cluster topology (used by Monitoring and MCP reconcilers).
	info := DetectClusterInfo(ctx, r.Client)

	// 5. Optional: Monitoring — ServiceMonitor + PrometheusRule.
	if pulse.Spec.Monitoring.Enabled {
		mr := &MonitoringReconciler{Client: r.Client, Scheme: r.Scheme}
		if err := mr.reconcileMonitoring(ctx, pulse); err != nil {
			logger.Error(err, "monitoring reconcile failed")
			return ctrl.Result{}, fmt.Errorf("reconcileMonitoring: %w", err)
		}
	}

	// 6. Optional: MCP server Deployment + Service.
	if pulse.Spec.Agent.MCP.Enabled {
		mcpr := &MCPReconciler{Client: r.Client, Scheme: r.Scheme}
		if err := mcpr.reconcileMCP(ctx, pulse, info); err != nil {
			logger.Error(err, "MCP reconcile failed")
			return ctrl.Result{}, fmt.Errorf("reconcileMCP: %w", err)
		}
	}

	// 7. Determine health of each component and sync CR status.
	agentReady := r.isDeploymentReady(ctx, agentResourceName(pulse.Name), pulse.Namespace)
	pgReady := r.isStatefulSetReady(ctx, pulse.Name+"-openshift-sre-agent-postgresql", pulse.Namespace)

	pulse.Status.AgentHealthy = agentReady
	pulse.Status.DatabaseReady = pgReady

	if agentReady && pgReady && pulse.Status.UIAvailable {
		pulse.Status.Phase = "Running"
	} else {
		pulse.Status.Phase = "Installing"
	}

	condition := metav1.Condition{
		Type:               "Ready",
		ObservedGeneration: pulse.Generation,
	}
	if pulse.Status.Phase == "Running" {
		condition.Status  = metav1.ConditionTrue
		condition.Reason  = "AllComponentsHealthy"
		condition.Message = "Agent, database, and UI are healthy"
	} else {
		condition.Status  = metav1.ConditionFalse
		condition.Reason  = "Installing"
		condition.Message = fmt.Sprintf("agentHealthy=%v databaseReady=%v uiAvailable=%v",
			agentReady, pgReady, pulse.Status.UIAvailable)
	}
	apimeta.SetStatusCondition(&pulse.Status.Conditions, condition)

	if err := r.Status().Update(ctx, pulse); err != nil {
		return ctrl.Result{}, fmt.Errorf("status update: %w", err)
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// deleteClusterScopedResources removes the cluster-scoped resources that are not
// owned by the CR (and therefore not garbage-collected by the K8s GC on deletion).
// Each deletion ignores NotFound so the method is idempotent.
func (r *OpenShiftPulseReconciler) deleteClusterScopedResources(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	// Agent ClusterRole
	agentCR := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: agentResourceName(pulse.Name)}}
	if err := r.Delete(ctx, agentCR); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete agent ClusterRole: %w", err)
	}

	// UI ClusterRole
	uiCR := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: uiClusterRoleName(pulse.Name)}}
	if err := r.Delete(ctx, uiCR); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete UI ClusterRole: %w", err)
	}

	// Agent ClusterRoleBinding
	agentCRB := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: agentResourceName(pulse.Name)}}
	if err := r.Delete(ctx, agentCRB); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete agent ClusterRoleBinding: %w", err)
	}

	// UI ClusterRoleBinding
	uiCRB := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: uiClusterRoleName(pulse.Name)}}
	if err := r.Delete(ctx, uiCRB); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete UI ClusterRoleBinding: %w", err)
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
	if err := r.Delete(ctx, oac); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete OAuthClient: %w", err)
	}

	return nil
}

// reconcilePostgres delegates to PostgreSQLReconciler and returns the database URL.
func (r *OpenShiftPulseReconciler) reconcilePostgres(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) (string, error) {
	pg := &PostgreSQLReconciler{Client: r.Client}
	return pg.reconcilePostgres(ctx, pulse)
}

// reconcileAgent runs all agent sub-reconciler steps in order.
// dbURL is available for future use (e.g. injecting directly rather than via Secret).
func (r *OpenShiftPulseReconciler) reconcileAgent(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse, dbURL string) error {
	logger := log.FromContext(ctx)
	_ = dbURL // consumed via K8s Secret by the agent Deployment today; reserved for future direct injection

	ar := &AgentReconciler{Client: r.Client, Scheme: r.Scheme}

	type step struct {
		name string
		fn   func(context.Context, *pulsev1alpha1.OpenShiftPulse) error
	}
	steps := []step{
		{"ServiceAccount", ar.reconcileServiceAccount},
		{"ClusterRole", ar.reconcileClusterRole},
		{"ClusterRoleBinding", ar.reconcileClusterRoleBinding},
		{"WSTokenSecret", ar.reconcileWSTokenSecret},
		{"MemoryPVC", ar.reconcileMemoryPVC},
		{"Deployment", ar.reconcileDeployment},
		{"Service", ar.reconcileService},
	}

	for _, s := range steps {
		if err := s.fn(ctx, pulse); err != nil {
			logger.Error(err, "agent reconcile step failed", "step", s.name)
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	return nil
}

// isDeploymentReady returns true when the named Deployment has at least one ready replica.
func (r *OpenShiftPulseReconciler) isDeploymentReady(ctx context.Context, name, ns string) bool {
	deploy := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, deploy); err != nil {
		return false
	}
	return deploy.Status.ReadyReplicas > 0
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
