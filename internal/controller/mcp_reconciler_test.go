package controller

import (
	"context"

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
