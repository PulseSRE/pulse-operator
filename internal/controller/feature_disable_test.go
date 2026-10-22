package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// Turning an optional component off must remove it. Before this, disabling
// MCP left its server running with a cluster-wide read ClusterRole still
// bound, and disabling Temporal (or its Web UI) left those pods up.
var _ = Describe("disabling an optional component", func() {
	const namespace = "default"

	gone := func(obj client.Object, name, ns string) {
		err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, obj)
		ExpectWithOffset(1, apierrors.IsNotFound(err)).To(BeTrue(), "%T %s should be removed, got err=%v", obj, name, err)
	}
	present := func(obj client.Object, name, ns string) {
		ExpectWithOffset(1, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, obj)).To(Succeed())
	}

	It("removes the MCP server and its cluster-wide RBAC", func() {
		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: "mcp-off-again", Namespace: namespace},
			Spec:       pulsev1alpha1.OpenShiftPulseSpec{Agent: pulsev1alpha1.AgentConfig{MCP: pulsev1alpha1.MCPConfig{Enabled: true}}},
		}
		Expect(k8sClient.Create(testCtx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, cr) })
		mcp := &MCPReconciler{Client: k8sClient, Scheme: testScheme}
		Expect(mcp.reconcileMCP(testCtx, cr, nil)).To(Succeed())

		name := mcpResourceName(cr.Name)
		crbName := mcpClusterRoleName(cr.Name, cr.Namespace)
		present(&appsv1.Deployment{}, name, namespace)
		present(&rbacv1.ClusterRoleBinding{}, crbName, "")

		Expect(mcp.removeMCP(testCtx, cr)).To(Succeed())
		gone(&appsv1.Deployment{}, name, namespace)
		gone(&corev1.Service{}, name, namespace)
		gone(&networkingv1.NetworkPolicy{}, name, namespace)
		gone(&corev1.ServiceAccount{}, name, namespace)
		gone(&rbacv1.ClusterRoleBinding{}, crbName, "")
		gone(&rbacv1.ClusterRole{}, crbName, "")

		// Idempotent: the next reconcile with MCP still off finds nothing to do.
		Expect(mcp.removeMCP(testCtx, cr)).To(Succeed())
	})

	It("leaves alone objects with the same names that this CR does not own", func() {
		cr := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: "mcp-foreign", Namespace: namespace}}
		Expect(k8sClient.Create(testCtx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, cr) })

		name := mcpResourceName(cr.Name)
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		}
		Expect(k8sClient.Create(testCtx, svc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, svc) })
		roleName := mcpClusterRoleName(cr.Name, cr.Namespace)
		role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
			Name:        roleName,
			Annotations: map[string]string{annotationOwnerUID: "someone-else"},
		}}
		Expect(k8sClient.Create(testCtx, role)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, role) })

		mcp := &MCPReconciler{Client: k8sClient, Scheme: testScheme}
		Expect(mcp.removeMCP(testCtx, cr)).To(Succeed())
		present(&corev1.Service{}, name, namespace)
		present(&rbacv1.ClusterRole{}, roleName, "")
	})

	It("removes the Temporal Web UI when ui is turned off, then the server when Temporal is", func() {
		on := true
		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: "temporal-off-again", Namespace: namespace},
			Spec:       pulsev1alpha1.OpenShiftPulseSpec{Temporal: pulsev1alpha1.TemporalConfig{Enabled: &on, UI: &on}},
		}
		Expect(k8sClient.Create(testCtx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, cr) })
		tr := &TemporalReconciler{Client: k8sClient, Scheme: testScheme}
		Expect(tr.reconcileTemporal(testCtx, cr)).To(Succeed())

		server := temporalResourceName(cr.Name)
		ui := server + "-ui"
		present(&appsv1.Deployment{}, ui, namespace)

		// Both on: nothing is removed.
		Expect(tr.removeTemporal(testCtx, cr)).To(Succeed())
		present(&appsv1.Deployment{}, ui, namespace)

		off := false
		cr.Spec.Temporal.UI = &off
		Expect(tr.removeTemporal(testCtx, cr)).To(Succeed())
		gone(&appsv1.Deployment{}, ui, namespace)
		gone(&corev1.Service{}, ui, namespace)
		present(&appsv1.Deployment{}, server, namespace)

		cr.Spec.Temporal.Enabled = &off
		Expect(tr.removeTemporal(testCtx, cr)).To(Succeed())
		gone(&appsv1.Deployment{}, server, namespace)
		gone(&corev1.Service{}, server, namespace)
	})
})
