package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	})
})
