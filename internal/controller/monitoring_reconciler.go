package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

var (
	serviceMonitorGVK = schema.GroupVersionKind{
		Group:   "monitoring.coreos.com",
		Version: "v1",
		Kind:    "ServiceMonitor",
	}
	prometheusRuleGVK = schema.GroupVersionKind{
		Group:   "monitoring.coreos.com",
		Version: "v1",
		Kind:    "PrometheusRule",
	}
)

// MonitoringReconciler reconciles ServiceMonitor and PrometheusRule for an OpenShiftPulse CR.
// Uses unstructured because monitoring.coreos.com CRDs are not registered in the scheme.
type MonitoringReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// prometheusRuleName returns the PrometheusRule resource name for a given CR name.
func prometheusRuleName(crName string) string {
	return crName + "-openshiftpulse"
}

// reconcileMonitoring creates/updates ServiceMonitor and PrometheusRule when
// spec.monitoring.enabled is true. No-ops when monitoring is disabled.
func (r *MonitoringReconciler) reconcileMonitoring(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	if !pulse.Spec.Monitoring.Enabled {
		return nil
	}

	logger := log.FromContext(ctx)
	logger.Info("Reconciling monitoring resources", "name", pulse.Name, "namespace", pulse.Namespace)

	if err := r.reconcileServiceMonitor(ctx, pulse); err != nil {
		return fmt.Errorf("ServiceMonitor: %w", err)
	}
	if err := r.reconcilePrometheusRule(ctx, pulse); err != nil {
		return fmt.Errorf("PrometheusRule: %w", err)
	}
	return nil
}

// reconcileServiceMonitor creates or updates a ServiceMonitor that scrapes the agent's /metrics endpoint.
func (r *MonitoringReconciler) reconcileServiceMonitor(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := agentResourceName(pulse.Name)

	endpoints := []interface{}{
		map[string]interface{}{
			"port":     "http",
			"scheme":   "http",
			"path":     "/metrics",
			"interval": "30s",
		},
	}
	selectorSpec := map[string]interface{}{
		"matchLabels": map[string]interface{}{
			"app": name,
		},
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(serviceMonitorGVK)

	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: pulse.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		desired := &unstructured.Unstructured{}
		desired.SetGroupVersionKind(serviceMonitorGVK)
		desired.SetName(name)
		desired.SetNamespace(pulse.Namespace)

		if setErr := controllerutil.SetControllerReference(pulse, desired, r.Scheme); setErr != nil {
			return fmt.Errorf("SetControllerReference: %w", setErr)
		}
		if setErr := unstructured.SetNestedField(desired.Object, selectorSpec, "spec", "selector"); setErr != nil {
			return fmt.Errorf("set spec.selector: %w", setErr)
		}
		if setErr := unstructured.SetNestedSlice(desired.Object, endpoints, "spec", "endpoints"); setErr != nil {
			return fmt.Errorf("set spec.endpoints: %w", setErr)
		}
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	if setErr := unstructured.SetNestedField(existing.Object, selectorSpec, "spec", "selector"); setErr != nil {
		return fmt.Errorf("update spec.selector: %w", setErr)
	}
	if setErr := unstructured.SetNestedSlice(existing.Object, endpoints, "spec", "endpoints"); setErr != nil {
		return fmt.Errorf("update spec.endpoints: %w", setErr)
	}
	return r.Update(ctx, existing)
}

// reconcilePrometheusRule creates or updates a PrometheusRule with three alert rules:
// PulseAgentDown, PulseAgentHighRestarts, PulsePostgreSQLDown.
func (r *MonitoringReconciler) reconcilePrometheusRule(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := prometheusRuleName(pulse.Name)
	agentName := agentResourceName(pulse.Name)

	rules := []interface{}{
		map[string]interface{}{
			"alert": "PulseAgentDown",
			"expr":  fmt.Sprintf(`up{job="%s"} == 0`, agentName),
			"for":   "5m",
			"labels": map[string]interface{}{
				"severity": "critical",
			},
			"annotations": map[string]interface{}{
				"summary": "Pulse Agent is down",
			},
		},
		map[string]interface{}{
			"alert": "PulseAgentHighRestarts",
			"expr": fmt.Sprintf(
				`increase(kube_pod_container_status_restarts_total{namespace="%s", container="agent"}[1h]) > 3`,
				pulse.Namespace,
			),
			"for": "0m",
			"labels": map[string]interface{}{
				"severity": "warning",
			},
			"annotations": map[string]interface{}{
				"summary": "Pulse Agent restarting frequently",
			},
		},
		map[string]interface{}{
			"alert": "PulsePostgreSQLDown",
			"expr": fmt.Sprintf(
				`kube_statefulset_status_ready_replicas{namespace="%s", statefulset="%s-postgresql"} == 0`,
				pulse.Namespace,
				agentName,
			),
			"for": "3m",
			"labels": map[string]interface{}{
				"severity": "critical",
			},
			"annotations": map[string]interface{}{
				"summary": "Pulse PostgreSQL is down",
			},
		},
	}

	groups := []interface{}{
		map[string]interface{}{
			"name":  "pulse.rules",
			"rules": rules,
		},
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(prometheusRuleGVK)

	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: pulse.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		desired := &unstructured.Unstructured{}
		desired.SetGroupVersionKind(prometheusRuleGVK)
		desired.SetName(name)
		desired.SetNamespace(pulse.Namespace)
		// Owner annotation for tracking; SetControllerReference also sets ownerReferences.
		desired.SetAnnotations(clusterScopedAnnotations(pulse))

		if setErr := controllerutil.SetControllerReference(pulse, desired, r.Scheme); setErr != nil {
			return fmt.Errorf("SetControllerReference: %w", setErr)
		}
		if setErr := unstructured.SetNestedSlice(desired.Object, groups, "spec", "groups"); setErr != nil {
			return fmt.Errorf("set spec.groups: %w", setErr)
		}
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	if setErr := unstructured.SetNestedSlice(existing.Object, groups, "spec", "groups"); setErr != nil {
		return fmt.Errorf("update spec.groups: %w", setErr)
	}
	return r.Update(ctx, existing)
}
