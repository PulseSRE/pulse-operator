package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
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

// mcpResourceName returns the MCP server Deployment/Service/ServiceAccount/
// NetworkPolicy name for a given CR name.
func mcpResourceName(crName string) string {
	return crName + "-mcp-server"
}

// mcpClusterRoleName returns a namespace-qualified name for the MCP server's
// cluster-scoped ClusterRole/ClusterRoleBinding — see
// deleteStaleUnqualifiedClusterScopedResource's doc comment for why this
// can't be mcpResourceName(crName) the way MCP's namespaced resources are named.
func mcpClusterRoleName(crName, crNamespace string) string {
	return crNamespace + "-" + mcpResourceName(crName)
}

// MCPReconciler reconciles the MCP server Deployment and Service for an OpenShiftPulse CR.
type MCPReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder is unused today (no self-heal site exists in this reconciler
	// yet) but kept consistent with the other three sub-reconcilers so a
	// future self-heal addition here doesn't need a separate wiring change.
	Recorder record.EventRecorder
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

	// RBAC before the Deployment: the ServiceAccount must exist before it can
	// be referenced by Spec.Template.Spec.ServiceAccountName.
	if err := r.reconcileMCPServiceAccount(ctx, pulse); err != nil {
		return fmt.Errorf("MCP ServiceAccount: %w", err)
	}
	if err := r.reconcileMCPClusterRole(ctx, pulse); err != nil {
		return fmt.Errorf("MCP ClusterRole: %w", err)
	}
	if err := r.reconcileMCPClusterRoleBinding(ctx, pulse); err != nil {
		return fmt.Errorf("MCP ClusterRoleBinding: %w", err)
	}
	if err := r.reconcileMCPDeployment(ctx, pulse); err != nil {
		return fmt.Errorf("MCP Deployment: %w", err)
	}
	if err := r.reconcileMCPService(ctx, pulse); err != nil {
		return fmt.Errorf("MCP Service: %w", err)
	}
	if err := r.reconcileMCPNetworkPolicy(ctx, pulse); err != nil {
		return fmt.Errorf("MCP NetworkPolicy: %w", err)
	}
	return nil
}

// reconcileMCPServiceAccount creates the dedicated ServiceAccount the MCP server
// runs as. Without this, the Deployment ran as the namespace's implicit
// "default" ServiceAccount, which normally carries zero permissions — leaving
// its cluster-diagnostics/observability/openshift toolsets unable to read
// anything even though they were configured and enabled.
func (r *MCPReconciler) reconcileMCPServiceAccount(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := mcpResourceName(pulse.Name)
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pulse.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		return controllerutil.SetControllerReference(pulse, sa, r.Scheme)
	})
	return err
}

// reconcileMCPClusterRole grants the MCP server's ServiceAccount the read-only
// cluster access its toolsets need (cluster-diagnostics, observability/*,
// openshift, etc. — see defaultMCPToolsets). Rules are a read-only extension
// of AgentReconciler.reconcileClusterRole's set (which the MCP toolsets
// overlap heavily with for cluster-diagnostics/observability) plus the
// OpenShift platform resources (routes, clusteroperators, clusterversions)
// the "openshift" toolset needs, which the agent's role does not carry.
func (r *MCPReconciler) reconcileMCPClusterRole(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := mcpClusterRoleName(pulse.Name, pulse.Namespace)
	if err := deleteStaleUnqualifiedClusterScopedResource(ctx, r.Client, &rbacv1.ClusterRole{}, mcpResourceName(pulse.Name), pulse); err != nil {
		return fmt.Errorf("migrate stale MCP ClusterRole: %w", err)
	}
	rules := []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{
				"pods", "pods/log", "nodes", "events",
				"services", "namespaces", "configmaps",
				"persistentvolumeclaims", "resourcequotas",
				"serviceaccounts", "endpoints", "limitranges",
			},
			Verbs: []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{"apps"},
			Resources: []string{"deployments", "replicasets", "statefulsets", "daemonsets"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{"batch"},
			Resources: []string{"jobs", "cronjobs"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{"metrics.k8s.io"},
			Resources: []string{"nodes", "pods"},
			Verbs:     []string{"get", "list"},
		},
		{
			APIGroups: []string{"route.openshift.io"},
			Resources: []string{"routes"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{"config.openshift.io"},
			Resources: []string{"clusterversions", "clusteroperators"},
			Verbs:     []string{"get", "list", "watch"},
		},
	}

	desired := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, desired, func() error {
		desired.Annotations = clusterScopedAnnotations(pulse)
		desired.Rules = rules
		return nil
	})
	return err
}

// reconcileMCPClusterRoleBinding binds the MCP ServiceAccount to its ClusterRole.
func (r *MCPReconciler) reconcileMCPClusterRoleBinding(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := mcpClusterRoleName(pulse.Name, pulse.Namespace)
	if err := deleteStaleUnqualifiedClusterScopedResource(ctx, r.Client, &rbacv1.ClusterRoleBinding{}, mcpResourceName(pulse.Name), pulse); err != nil {
		return fmt.Errorf("migrate stale MCP ClusterRoleBinding: %w", err)
	}
	// saName (not name): the ServiceAccount subject is a namespaced resource,
	// still named plainly by mcpResourceName — only this Binding and its
	// ClusterRole need the namespace-qualified name.
	saName := mcpResourceName(pulse.Name)
	desired := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, desired, func() error {
		desired.Annotations = clusterScopedAnnotations(pulse)
		desired.Subjects = []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      saName,
			Namespace: pulse.Namespace,
		}}
		// RoleRef is immutable — set only on creation.
		if desired.RoleRef.Name == "" {
			desired.RoleRef = rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     name,
			}
		}
		return nil
	})
	return err
}

// reconcileMCPNetworkPolicy restricts ingress to the MCP server pod (port
// mcpServerPort) to only the agent pod — the sole caller, via PULSE_MCP_URL
// (see MCPServiceURL and AgentReconciler.buildDeploymentSpec). Referenced by
// the comment on reconcileAgentNetworkPolicy in network_policy_reconciler.go.
func (r *MCPReconciler) reconcileMCPNetworkPolicy(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := mcpResourceName(pulse.Name)
	mcpApp := name
	agentApp := pulse.Name + "-openshift-sre-agent"

	tcpProto := corev1.ProtocolTCP
	port := intstr.FromInt(int(mcpServerPort))

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pulse.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Spec.PodSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{"app": mcpApp},
		}
		np.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
		np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{
			{
				From: []networkingv1.NetworkPolicyPeer{
					{
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": agentApp},
						},
					},
				},
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &tcpProto, Port: &port},
				},
			},
		}
		return controllerutil.SetControllerReference(pulse, np, r.Scheme)
	})
	return err
}

// reconcileMCPDeployment creates or updates the MCP server Deployment.
// Uses CreateOrUpdate and only mutates the fields the operator owns (selector,
// pod labels, container image/args/probes/resources) — anything else a user
// manually patched on the Deployment (e.g. Spec.Replicas via `kubectl scale`)
// is left untouched instead of being clobbered by a full spec replace.
func (r *MCPReconciler) reconcileMCPDeployment(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := mcpResourceName(pulse.Name)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pulse.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		if setErr := controllerutil.SetControllerReference(pulse, deploy, r.Scheme); setErr != nil {
			return setErr
		}
		deploy.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"app": name},
		}
		deploy.Spec.Template.Labels = map[string]string{"app": name}
		deploy.Spec.Template.Spec.ServiceAccountName = name
		deploy.Spec.Template.Spec.SecurityContext = defaultPodSecCtx(nil) // OCP assigns UID from namespace range via SCC
		deploy.Spec.Template.Spec.Containers = []corev1.Container{
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
		}
		return nil
	})
	return err
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
