package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"

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
	// defaultMCPServerImage is only the `make run` / test fallback; a bundle
	// install runs RELATED_IMAGE_MCP_SERVER (see images.go).
	defaultMCPServerImage = "quay.io/amobrem/pulse-agent:mcp-server"
	mcpServerPort         = int32(8081)
	// defaultMCPToolsets is the full SRE toolset matching the documented 36-tool count.
	// Matches: core(default)+config(default)+helm+observability/metrics+observability/logs+
	// openshift+ossm+netedge+tekton+kubevirt+kcp+cluster-diagnostics
	defaultMCPToolsets = "core,config,helm,observability/metrics,observability/logs,openshift,ossm,netedge,tekton,kubevirt,kcp,cluster-diagnostics"

	// Where the generated TOML is mounted and what it is called. The server
	// resolves a relative --config against its working directory, so this is
	// kept absolute.
	mcpConfigMountPath = "/etc/mcp"
	mcpConfigFileName  = "config.toml"

	mcpConfigHashAnnotation = "pulse.ai/mcp-config-hash"
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
	// Config before the Deployment: the pod mounts this ConfigMap, and the
	// hash it returns goes into the pod template.
	configHash, err := r.reconcileMCPConfig(ctx, pulse)
	if err != nil {
		return fmt.Errorf("MCP ConfigMap: %w", err)
	}
	if err := r.reconcileMCPDeployment(ctx, pulse, configHash); err != nil {
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
func (r *MCPReconciler) reconcileMCPDeployment(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse, configHash string) error {
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
					return imageFromEnv(envMCPServerImage, defaultMCPServerImage)
				}(),
				SecurityContext: defaultContainerSecCtx(),
				// kubernetes-mcp-server takes its settings from a TOML file, not
				// flags: --config and --config-dir are the only knobs it has
				// besides --help/--version. It previously accepted --port and
				// --toolsets, and this operator passed those, so when the
				// published image moved to the config file the server began
				// exiting immediately with "unknown flag: --port". See
				// reconcileMCPConfig for the file this points at.
				Args: []string{
					fmt.Sprintf("--config=%s/%s", mcpConfigMountPath, mcpConfigFileName),
				},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      "config",
						MountPath: mcpConfigMountPath,
						ReadOnly:  true,
					},
				},
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
		deploy.Spec.Template.Spec.Volumes = []corev1.Volume{
			{
				Name: "config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: mcpConfigName(pulse.Name)},
					},
				},
			},
		}
		// A mounted ConfigMap's contents update in place, but the server only
		// reads its config at startup, so changing spec.agent.mcp.toolsets
		// would otherwise leave the running pod on the old toolset list until
		// something unrelated restarted it. Rolling the hash into the pod
		// template makes the change itself trigger the rollout.
		if deploy.Spec.Template.Annotations == nil {
			deploy.Spec.Template.Annotations = map[string]string{}
		}
		deploy.Spec.Template.Annotations[mcpConfigHashAnnotation] = configHash
		return nil
	})
	return err
}

// mcpConfigName is the ConfigMap holding the server's TOML settings.
func mcpConfigName(crName string) string {
	return mcpResourceName(crName) + "-config"
}

// buildMCPConfigTOML renders the settings that used to be --port and
// --toolsets. The upstream keys are `port` (a string; empty means stdio) and
// `toolsets` (an array of strings) — see pkg/config.Config in
// openshift/openshift-mcp-server.
//
// This is written by hand rather than with a TOML library because it is two
// scalar keys and adding a dependency to emit them is not worth it; the values
// are a fixed int and a comma-separated list the CRD already constrains.
func buildMCPConfigTOML(pulse *pulsev1alpha1.OpenShiftPulse) string {
	toolsets := pulse.Spec.Agent.MCP.Toolsets
	if toolsets == "" {
		toolsets = defaultMCPToolsets
	}
	quoted := make([]string, 0, strings.Count(toolsets, ",")+1)
	for _, t := range strings.Split(toolsets, ",") {
		if t = strings.TrimSpace(t); t != "" {
			quoted = append(quoted, strconv.Quote(t))
		}
	}
	return fmt.Sprintf("# Managed by pulse-operator. Edits are reverted.\nport = %q\ntoolsets = [%s]\n",
		strconv.Itoa(int(mcpServerPort)), strings.Join(quoted, ", "))
}

// reconcileMCPConfig writes the TOML the server reads at startup and returns a
// short hash of it for the Deployment's pod-template annotation.
func (r *MCPReconciler) reconcileMCPConfig(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) (string, error) {
	toml := buildMCPConfigTOML(pulse)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(toml)))[:8]

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mcpConfigName(pulse.Name),
			Namespace: pulse.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if setErr := controllerutil.SetControllerReference(pulse, cm, r.Scheme); setErr != nil {
			return setErr
		}
		cm.Data = map[string]string{mcpConfigFileName: toml}
		return nil
	})
	return hash, err
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

// removeMCP deletes what reconcileMCP created once spec.agent.mcp.enabled is
// turned off. Without it, disabling MCP left the server running with its
// cluster-wide read ClusterRole bound, which is the opposite of what someone
// switching it off intends. Only objects this CR owns are touched.
func (r *MCPReconciler) removeMCP(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := mcpResourceName(pulse.Name)
	for _, obj := range []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: mcpConfigName(pulse.Name)}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name}},
	} {
		if err := deleteOwned(ctx, r.Client, obj, pulse); err != nil {
			return err
		}
	}
	crName := mcpClusterRoleName(pulse.Name, pulse.Namespace)
	if err := deleteOwnedClusterScoped(ctx, r.Client, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: crName}}, pulse); err != nil {
		return err
	}
	return deleteOwnedClusterScoped(ctx, r.Client, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: crName}}, pulse)
}
