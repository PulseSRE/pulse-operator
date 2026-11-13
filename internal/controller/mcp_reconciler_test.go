package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

var _ = Describe("MCPReconciler", func() {
	const (
		namespace = "default"
	)

	var (
		ctx context.Context
		mcp *MCPReconciler
	)

	BeforeEach(func() {
		ctx = testCtx
		mcp = &MCPReconciler{
			Client: k8sClient,
			Scheme: testScheme,
		}
	})

	Describe("MCP disabled", func() {
		const crName = "mcp-disabled-pulse"

		var cr *pulsev1alpha1.OpenShiftPulse

		BeforeEach(func() {
			cr = &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					Agent: pulsev1alpha1.AgentConfig{
						MCP: pulsev1alpha1.MCPConfig{Enabled: false},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		})

		AfterEach(func() {
			_ = k8sClient.Delete(ctx, cr)
		})

		It("returns nil without creating a Deployment", func() {
			Expect(mcp.reconcileMCP(ctx, cr, nil)).To(Succeed())

			deploy := &appsv1.Deployment{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      mcpResourceName(crName),
				Namespace: namespace,
			}, deploy)
			Expect(err).To(HaveOccurred(), "Deployment must NOT exist when MCP is disabled")
		})
	})

	Describe("MCP enabled", func() {
		const crName = "mcp-enabled-pulse"

		var cr *pulsev1alpha1.OpenShiftPulse

		BeforeEach(func() {
			cr = &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					Agent: pulsev1alpha1.AgentConfig{
						MCP: pulsev1alpha1.MCPConfig{Enabled: true},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		})

		AfterEach(func() {
			_ = k8sClient.Delete(ctx, cr)
		})

		It("creates a Deployment with port 8081", func() {
			Expect(mcp.reconcileMCP(ctx, cr, nil)).To(Succeed())

			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      mcpResourceName(crName),
				Namespace: namespace,
			}, deploy)).To(Succeed())

			containers := deploy.Spec.Template.Spec.Containers
			Expect(containers).NotTo(BeEmpty())
			ports := containers[0].Ports
			Expect(ports).NotTo(BeEmpty())
			Expect(ports[0].ContainerPort).To(Equal(int32(8081)))
		})

		It("Deployment has both liveness and readiness probes", func() {
			Expect(mcp.reconcileMCP(ctx, cr, nil)).To(Succeed())

			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      mcpResourceName(crName),
				Namespace: namespace,
			}, deploy)).To(Succeed())

			c := deploy.Spec.Template.Spec.Containers[0]
			Expect(c.ReadinessProbe).NotTo(BeNil(), "readiness probe must be set")
			Expect(c.ReadinessProbe.HTTPGet).NotTo(BeNil())
			Expect(c.LivenessProbe).NotTo(BeNil(), "liveness probe must be set")
			Expect(c.LivenessProbe.HTTPGet).NotTo(BeNil())
		})

		It("Deployment has resource limits set", func() {
			Expect(mcp.reconcileMCP(ctx, cr, nil)).To(Succeed())

			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      mcpResourceName(crName),
				Namespace: namespace,
			}, deploy)).To(Succeed())

			limits := deploy.Spec.Template.Spec.Containers[0].Resources.Limits
			Expect(limits).NotTo(BeNil())
			mem, ok := limits[corev1.ResourceMemory]
			Expect(ok).To(BeTrue(), "memory limit must be set")
			Expect(mem.IsZero()).To(BeFalse(), "memory limit must be non-zero")
		})

		It("reconcileMCPService is idempotent", func() {
			Expect(mcp.reconcileMCPService(ctx, cr)).To(Succeed())
			Expect(mcp.reconcileMCPService(ctx, cr)).To(Succeed())

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      mcpResourceName(crName),
				Namespace: namespace,
			}, svc)).To(Succeed())
		})

		// Regression: the Deployment used to set no ServiceAccountName at all,
		// so the pod ran as the namespace's implicit "default" SA — normally
		// zero permissions — even though its configured toolsets need real
		// cluster read access.
		It("Deployment runs as its own dedicated ServiceAccount, not the namespace default", func() {
			Expect(mcp.reconcileMCP(ctx, cr, nil)).To(Succeed())

			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      mcpResourceName(crName),
				Namespace: namespace,
			}, deploy)).To(Succeed())

			saName := deploy.Spec.Template.Spec.ServiceAccountName
			Expect(saName).NotTo(BeEmpty())
			Expect(saName).NotTo(Equal("default"))
			Expect(saName).To(Equal(mcpResourceName(crName)))

			sa := &corev1.ServiceAccount{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: saName, Namespace: namespace}, sa)).To(Succeed())
		})

		It("creates a ClusterRole and ClusterRoleBinding granting the MCP ServiceAccount read access", func() {
			Expect(mcp.reconcileMCP(ctx, cr, nil)).To(Succeed())

			qualifiedName := mcpClusterRoleName(crName, namespace)
			cr2 := &rbacv1.ClusterRole{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: qualifiedName}, cr2)).To(Succeed())
			Expect(cr2.Rules).NotTo(BeEmpty())
			for _, rule := range cr2.Rules {
				for _, verb := range rule.Verbs {
					Expect(verb).To(BeElementOf("get", "list", "watch"),
						"MCP's ClusterRole must stay strictly read-only")
				}
			}

			crb := &rbacv1.ClusterRoleBinding{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: qualifiedName}, crb)).To(Succeed())
			Expect(crb.RoleRef.Name).To(Equal(qualifiedName))
			Expect(crb.Subjects).To(ConsistOf(rbacv1.Subject{
				Kind:      "ServiceAccount",
				Name:      mcpResourceName(crName),
				Namespace: namespace,
			}))
		})

		// Regression: two OpenShiftPulse CRs sharing a name in different
		// namespaces must not collide on one shared cluster-scoped
		// ClusterRole/ClusterRoleBinding — the CSV declares AllNamespaces as
		// the only supported install mode, so this is an expected shape, not
		// an edge case.
		It("namespace-qualifies the ClusterRole/Binding names so two CRs with the same name in different namespaces don't collide", func() {
			otherNamespace := "other-ns-" + crName
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: otherNamespace}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, ns) }()

			otherCR := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: otherNamespace},
				Spec:       pulsev1alpha1.OpenShiftPulseSpec{Agent: pulsev1alpha1.AgentConfig{MCP: pulsev1alpha1.MCPConfig{Enabled: true}}},
			}
			Expect(k8sClient.Create(ctx, otherCR)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, otherCR) }()

			Expect(mcp.reconcileMCP(ctx, cr, nil)).To(Succeed())
			Expect(mcp.reconcileMCP(ctx, otherCR, nil)).To(Succeed())

			ourCRB := &rbacv1.ClusterRoleBinding{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mcpClusterRoleName(crName, namespace)}, ourCRB)).To(Succeed())
			Expect(ourCRB.Subjects).To(ConsistOf(rbacv1.Subject{
				Kind: "ServiceAccount", Name: mcpResourceName(crName), Namespace: namespace,
			}), "our CR's binding must still point at our own ServiceAccount after the other CR reconciled")

			otherCRB := &rbacv1.ClusterRoleBinding{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mcpClusterRoleName(crName, otherNamespace)}, otherCRB)).To(Succeed())
			Expect(otherCRB.Subjects).To(ConsistOf(rbacv1.Subject{
				Kind: "ServiceAccount", Name: mcpResourceName(crName), Namespace: otherNamespace,
			}))
		})

		It("creates a NetworkPolicy restricting ingress to the agent pod only", func() {
			Expect(mcp.reconcileMCP(ctx, cr, nil)).To(Succeed())

			np := &networkingv1.NetworkPolicy{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      mcpResourceName(crName),
				Namespace: namespace,
			}, np)).To(Succeed())

			Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("app", mcpResourceName(crName)))
			Expect(np.Spec.Ingress).To(HaveLen(1))
			Expect(np.Spec.Ingress[0].From).To(HaveLen(1))
			Expect(np.Spec.Ingress[0].From[0].PodSelector.MatchLabels).To(
				HaveKeyWithValue("app", crName+"-openshift-sre-agent"),
				"only the agent pod (the sole caller of PULSE_MCP_URL) may reach the MCP server")
		})
	})
})

// TestBuildMCPConfigTOMLGolden pins the exact file the MCP server reads at
// startup. Keys are the ones openshift/openshift-mcp-server 0e36b24 defines in
// pkg/config (port, toolsets, read_only, disabled_tools).
func TestBuildMCPConfigTOMLGolden(t *testing.T) {
	const disabled = `disabled_tools = ["configuration_view", "pods_exec", "nodes_debug_exec", ` +
		`"get-conntrack", "get-ip", "get-iptables", "get-nft", "pwru", "tcpdump", ` +
		`"exec_dns_in_pod", "get_router_config", "get_router_info", "get_router_sessions", ` +
		`"ovn_get", "ovn_lflow_list", "ovn_show", "ovn_trace", "ovs_appctl", "ovs_ofctl", "ovs_vsctl"]` + "\n"

	for _, tc := range []struct {
		name, toolsets, want string
	}{
		{
			name:     "default toolsets",
			toolsets: "",
			want: "# Managed by pulse-operator. Edits are reverted.\n" +
				"port = \"8081\"\n" +
				`toolsets = ["core", "config", "helm", "observability/metrics", "observability/logs", ` +
				`"openshift", "ossm", "netedge", "tekton", "kubevirt", "kcp", "cluster-diagnostics"]` + "\n" +
				"read_only = true\n" + disabled,
		},
		{
			// An explicit toolset list must not drop the read-only settings:
			// enabling ovn-kubernetes or cni-diagnostics is exactly when the
			// exec tools would otherwise appear.
			name:     "explicit toolsets keep read_only and disabled_tools",
			toolsets: " core , ovn-kubernetes,cni-diagnostics,",
			want: "# Managed by pulse-operator. Edits are reverted.\n" +
				"port = \"8081\"\n" +
				`toolsets = ["core", "ovn-kubernetes", "cni-diagnostics"]` + "\n" +
				"read_only = true\n" + disabled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr := &pulsev1alpha1.OpenShiftPulse{}
			cr.Spec.Agent.MCP.Toolsets = tc.toolsets
			if got := buildMCPConfigTOML(cr); got != tc.want {
				t.Errorf("buildMCPConfigTOML mismatch\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestMCPDisabledToolsKeepsUpstreamDefault: setting disabled_tools replaces the
// downstream build's default (["configuration_view"]) instead of extending it,
// so dropping that entry would expose a tool the server hides today. Names
// must be exact; upstream does not expand globs.
func TestMCPDisabledToolsKeepsUpstreamDefault(t *testing.T) {
	if !slices.Contains(mcpDisabledTools, "configuration_view") {
		t.Error("mcpDisabledTools must keep the upstream default configuration_view")
	}
	seen := map[string]bool{}
	for _, name := range mcpDisabledTools {
		if strings.ContainsAny(name, "*?[") {
			t.Errorf("%q: disabled_tools takes exact names; upstream does not expand globs", name)
		}
		if seen[name] {
			t.Errorf("%q listed twice", name)
		}
		seen[name] = true
	}
}

// The config hash rolls the MCP Deployment, so a config change (like this
// one) restarts the server, which reads its config only at startup.
var _ = Describe("MCP config read-only settings", func() {
	const (
		crName    = "mcp-readonly-pulse"
		namespace = "default"
	)

	AfterEach(func() {
		cr := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace}}
		_ = k8sClient.Delete(testCtx, cr)
	})

	It("writes read_only and disabled_tools into the ConfigMap the server mounts", func() {
		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec: pulsev1alpha1.OpenShiftPulseSpec{
				Agent: pulsev1alpha1.AgentConfig{MCP: pulsev1alpha1.MCPConfig{Enabled: true}},
			},
		}
		Expect(k8sClient.Create(testCtx, cr)).To(Succeed())

		mcp := &MCPReconciler{Client: k8sClient, Scheme: testScheme}
		Expect(mcp.reconcileMCP(testCtx, cr, nil)).To(Succeed())

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: mcpConfigName(crName), Namespace: namespace}, cm)).To(Succeed())
		toml := cm.Data[mcpConfigFileName]
		Expect(toml).To(Equal(buildMCPConfigTOML(cr)))
		Expect(toml).To(ContainSubstring("\nread_only = true\n"))
		for _, name := range []string{"pods_exec", "nodes_debug_exec", "get_router_config", "ovn_trace", "tcpdump"} {
			Expect(toml).To(ContainSubstring(strconv.Quote(name)), "%s must be in disabled_tools", name)
		}

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: mcpResourceName(crName), Namespace: namespace}, deploy)).To(Succeed())
		want := fmt.Sprintf("%x", sha256.Sum256([]byte(toml)))[:8]
		Expect(deploy.Spec.Template.Annotations).To(HaveKeyWithValue(mcpConfigHashAnnotation, want),
			"the pod template must carry the hash of this config so the change rolls the server")
	})
})
