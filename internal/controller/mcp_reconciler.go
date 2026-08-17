package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

const (
	defaultMCPServerImage = "quay.io/amobrem/pulse-agent:mcp-server"
	mcpServerPort         = int32(8081)
	// defaultMCPToolsets is the full SRE toolset matching the documented 36-tool count.
	// Matches: core(default)+config(default)+helm+observability/metrics+observability/logs+
	// openshift+ossm+netedge+tekton+kubevirt+kcp+cluster-diagnostics
	defaultMCPToolsets = "core,config,helm,observability/metrics,observability/logs,openshift,ossm,netedge,tekton,kubevirt,kcp,cluster-diagnostics"
)

// MCPServiceURL returns the in-cluster URL of the MCP server for injection into the agent.
// Injected as PULSE_MCP_URL by AgentReconciler.buildDeploymentSpec when MCP is enabled.
// Skills read the env var ${PULSE_MCP_URL} (not PULSE_AGENT_MCP_URL).
func MCPServiceURL(name, ns string) string {
	return fmt.Sprintf("http://%s-mcp-server:%d", name, mcpServerPort)
}

// mcpResourceName returns the MCP server Deployment/Service name for a given CR name.
func mcpResourceName(crName string) string {
	return crName + "-mcp-server"
}

// MCPReconciler reconciles the MCP server Deployment and Service for an OpenShiftPulse CR.
type MCPReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// reconcileMCP creates/updates MCP server resources when spec.agent.mcp.enabled is true.
// info is accepted for future cluster-aware behaviour (e.g. ACM-scoped routing) but is
// not consumed today.
func (r *MCPReconciler) reconcileMCP(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse, info *ClusterInfo) error {
	_ = info // reserved for future cluster-aware routing

	if !pulse.Spec.Agent.MCP.Enabled {
		return nil
	}

	logger := log.FromContext(ctx)
	logger.Info("Reconciling MCP server resources", "name", pulse.Name, "namespace", pulse.Namespace)

	if err := r.reconcileMCPDeployment(ctx, pulse); err != nil {
		return fmt.Errorf("MCP Deployment: %w", err)
	}
	if err := r.reconcileMCPService(ctx, pulse); err != nil {
		return fmt.Errorf("MCP Service: %w", err)
	}
	return nil
}

// reconcileMCPDeployment creates or updates the MCP server Deployment.
// On update, only the container image and args are synced.
func (r *MCPReconciler) reconcileMCPDeployment(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := mcpResourceName(pulse.Name)

	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pulse.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					SecurityContext: defaultPodSecCtx(nil), // OCP assigns UID from namespace range via SCC
					Containers: []corev1.Container{
						{
							Name: "mcp-server",
							Image: func() string {
								if pulse.Spec.Agent.MCP.Image != "" {
									return pulse.Spec.Agent.MCP.Image
								}
								return defaultMCPServerImage
							}(),
							SecurityContext: defaultContainerSecCtx(),
							Args: func() []string {
								toolsets := pulse.Spec.Agent.MCP.Toolsets
								if toolsets == "" {
									toolsets = defaultMCPToolsets
								}
								return []string{
									fmt.Sprintf("--port=%d", mcpServerPort),
									fmt.Sprintf("--toolsets=%s", toolsets),
								}
							}(),
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: mcpServerPort,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt(int(mcpServerPort)),
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt(int(mcpServerPort)),
									},
								},
								InitialDelaySeconds: 15,
								PeriodSeconds:       20,
							},
						},
					},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(pulse, desired, r.Scheme); err != nil {
		return err
	}

	existing := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: pulse.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// Sync full spec; image tag and args are the primary fields that change across releases.
	existing.Spec = desired.Spec
	return r.Update(ctx, existing)
}

// reconcileMCPService creates or updates the ClusterIP Service fronting the MCP server.
func (r *MCPReconciler) reconcileMCPService(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := mcpResourceName(pulse.Name)

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pulse.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": name},
			Ports: []corev1.ServicePort{
				{
					Name:     "http",
					Port:     mcpServerPort,
					Protocol: corev1.ProtocolTCP,
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(pulse, desired, r.Scheme); err != nil {
		return err
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: pulse.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Ports = desired.Spec.Ports
	return r.Update(ctx, existing)
}
