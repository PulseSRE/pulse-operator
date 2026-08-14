package controller

import (
	"context"
	"fmt"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

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

	// 4. Determine health of each component and sync CR status.
	agentHealthy := r.isAgentHealthy(ctx, pulse)
	pulse.Status.AgentHealthy = agentHealthy

	if pulse.Status.DatabaseReady && agentHealthy && pulse.Status.UIAvailable {
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
			agentHealthy, pulse.Status.DatabaseReady, pulse.Status.UIAvailable)
	}
	apimeta.SetStatusCondition(&pulse.Status.Conditions, condition)

	if err := r.Status().Update(ctx, pulse); err != nil {
		return ctrl.Result{}, fmt.Errorf("status update: %w", err)
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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

// isAgentHealthy returns true when the agent Deployment has at least one available replica.
func (r *OpenShiftPulseReconciler) isAgentHealthy(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) bool {
	deploy := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      agentResourceName(pulse.Name),
		Namespace: pulse.Namespace,
	}, deploy); err != nil {
		return false
	}
	return deploy.Status.AvailableReplicas > 0
}

// SetupWithManager registers this reconciler with the controller manager.
// It watches OpenShiftPulse CRs, all namespace-scoped sub-resources it owns,
// and the cluster-scoped configv1.Ingress "cluster" object so that a change to the
// ingress domain (e.g. base domain update) triggers re-detection and reconciliation.
func (r *OpenShiftPulseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pulsev1alpha1.OpenShiftPulse{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ServiceAccount{}).
		Watches(
			&configv1.Ingress{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
				// Any change to the cluster Ingress config should re-run all OpenShiftPulse reconciles.
				pulseList := &pulsev1alpha1.OpenShiftPulseList{}
				if err := r.List(ctx, pulseList); err != nil {
					return nil
				}
				reqs := make([]reconcile.Request, 0, len(pulseList.Items))
				for _, p := range pulseList.Items {
					reqs = append(reqs, reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name:      p.Name,
							Namespace: p.Namespace,
						},
					})
				}
				return reqs
			}),
		).
		Complete(r)
}
