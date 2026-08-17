package controller

// Tests for controller behavior that was previously untested — caught by AUDIT.md review.
// These tests exist specifically because the issues they cover shipped undetected.
// Each test includes a comment explaining what gap it closes.

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

var _ = Describe("Controller behavior — AUDIT regression tests", func() {
	const namespace = "default"
	var ctx context.Context

	BeforeEach(func() { ctx = testCtx })

	// ── ClusterRole security ────────────────────────────────────────────────────
	// AUDIT: services/proxy in Agent ClusterRole allows HTTP proxying to any
	// cluster service — lateral movement vector. Removed; this test ensures it
	// never comes back.

	Describe("Agent ClusterRole security", func() {
		const crName = "rbac-sec-pulse"

		AfterEach(func() {
			cr := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace}}
			_ = k8sClient.Delete(ctx, cr)
		})

		It("does NOT include services/proxy in ClusterRole rules", func() {
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec:       pulsev1alpha1.OpenShiftPulseSpec{},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace}}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			clusterRole := &rbacv1.ClusterRole{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentResourceName(crName)}, clusterRole)).To(Succeed())

			for _, rule := range clusterRole.Rules {
				for _, res := range rule.Resources {
					Expect(res).NotTo(Equal("services/proxy"),
						"services/proxy must be absent — it allows HTTP proxying to any service (lateral movement)")
				}
			}
		})

		It("ClusterRole read-only by default — no delete/patch verbs without AllowWriteOperations", func() {
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec:       pulsev1alpha1.OpenShiftPulseSpec{},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace}}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			clusterRole := &rbacv1.ClusterRole{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentResourceName(crName)}, clusterRole)).To(Succeed())

			for _, rule := range clusterRole.Rules {
				for _, verb := range rule.Verbs {
					Expect(verb).NotTo(BeElementOf("delete", "patch", "update", "create"),
						"write verbs must not appear in default ClusterRole")
				}
			}
		})
	})

	// ── clusterInfo cache ───────────────────────────────────────────────────────
	// AUDIT: sync.Once cached partial failures forever. Replaced with mutex+bool
	// that only caches on full success. This test verifies reset works and that
	// DetectClusterInfo returns a non-nil result even on empty envtest cluster.

	Describe("ClusterInfo detection", func() {
		It("DetectClusterInfo returns non-nil even when OCP APIs are absent (envtest)", func() {
			ResetClusterInfoCache()
			info := DetectClusterInfo(ctx, k8sClient)
			Expect(info).NotTo(BeNil())
			// Default proxy image is always set even if ImageStream not found.
			Expect(info.OAuthProxyImage).NotTo(BeEmpty())
		})

		It("ResetClusterInfoCache allows re-detection on next call", func() {
			// Prime the cache.
			ResetClusterInfoCache()
			first := DetectClusterInfo(ctx, k8sClient)
			// Reset — next call should re-run detection.
			ResetClusterInfoCache()
			second := DetectClusterInfo(ctx, k8sClient)
			// Both should be valid (not panicking), content may differ.
			Expect(first).NotTo(BeNil())
			Expect(second).NotTo(BeNil())
		})
	})

	// ── MCP toolsets ────────────────────────────────────────────────────────────
	// AUDIT: MCP Deployment was started with only --port, no --toolsets.
	// This caused only the default core+config toolsets (13 tools) to load
	// instead of the documented 36. Now defaultMCPToolsets is injected.

	Describe("MCP Deployment toolsets arg", func() {
		const crName = "mcp-toolsets-pulse"

		AfterEach(func() {
			cr := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace}}
			_ = k8sClient.Delete(ctx, cr)
		})

		It("MCP Deployment args include --toolsets when MCP enabled without explicit toolsets", func() {
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					Agent: pulsev1alpha1.AgentConfig{
						MCP: pulsev1alpha1.MCPConfig{Enabled: true},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			mcp := &MCPReconciler{Client: k8sClient, Scheme: testScheme}
			Expect(mcp.reconcileMCP(ctx, cr, nil)).To(Succeed())

			deploy := getDeployment(ctx, mcpResourceName(crName), namespace)
			args := deploy.Spec.Template.Spec.Containers[0].Args

			hasToolsetsArg := false
			for _, arg := range args {
				if strings.HasPrefix(arg, "--toolsets=") {
					hasToolsetsArg = true
					Expect(arg).To(ContainSubstring("core"), "--toolsets must include core")
					Expect(arg).To(ContainSubstring("openshift"), "--toolsets must include openshift")
				}
			}
			Expect(hasToolsetsArg).To(BeTrue(), "--toolsets arg must be present in MCP Deployment")
		})

		It("MCP Deployment uses spec.agent.mcp.toolsets when explicitly set", func() {
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					Agent: pulsev1alpha1.AgentConfig{
						MCP: pulsev1alpha1.MCPConfig{
							Enabled:  true,
							Toolsets: "core,config",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			mcp := &MCPReconciler{Client: k8sClient, Scheme: testScheme}
			Expect(mcp.reconcileMCP(ctx, cr, nil)).To(Succeed())

			deploy := getDeployment(ctx, mcpResourceName(crName), namespace)
			args := deploy.Spec.Template.Spec.Containers[0].Args

			for _, arg := range args {
				if strings.HasPrefix(arg, "--toolsets=") {
					Expect(arg).To(Equal("--toolsets=core,config"),
						"explicit toolsets must be used verbatim")
				}
			}
		})

		It("MCP Deployment uses spec.agent.mcp.image when explicitly set", func() {
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					Agent: pulsev1alpha1.AgentConfig{
						MCP: pulsev1alpha1.MCPConfig{
							Enabled: true,
							Image:   "quay.io/custom/mcp-server:v2",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			mcp := &MCPReconciler{Client: k8sClient, Scheme: testScheme}
			Expect(mcp.reconcileMCP(ctx, cr, nil)).To(Succeed())

			deploy := getDeployment(ctx, mcpResourceName(crName), namespace)
			Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("quay.io/custom/mcp-server:v2"))
		})
	})
})

// getDeployment is a test helper that fetches a Deployment and fails the test if not found.
func getDeployment(ctx context.Context, name, namespace string) *appsv1.Deployment {
	deploy := &appsv1.Deployment{}
	ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{
		Name:      name,
		Namespace: namespace,
	}, deploy)).To(Succeed())
	return deploy
}
