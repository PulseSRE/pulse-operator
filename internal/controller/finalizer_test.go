package controller

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// failingDeleteClient wraps the real client and returns a fixed error for any
// Delete call whose object name matches failName, delegating everything else
// (including every other Delete) to the real client. Used to prove that
// deleteClusterScopedResources attempts every deletion instead of stopping at
// the first failure — REVIEW.md flagged the previous version of this test as
// "structurally correct but unverifiable from this test" because it never
// injected a real failure; this does.
type failingDeleteClient struct {
	client.Client
	failName    string
	failErr     error
	deleteCalls []string
}

func (c *failingDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.deleteCalls = append(c.deleteCalls, obj.GetName())
	// The agent ClusterRole and agent ClusterRoleBinding intentionally share
	// the same name (agentResourceName) — only fail the ClusterRole so exactly
	// one of the five deletes fails, proving the other four (including the
	// same-named Binding) are still attempted independently.
	if _, isClusterRole := obj.(*rbacv1.ClusterRole); isClusterRole && obj.GetName() == c.failName {
		return c.failErr
	}
	return c.Client.Delete(ctx, obj, opts...)
}

var _ = Describe("deleteClusterScopedResources finalizer", func() {
	const (
		crName    = "finalizer-test-pulse"
		namespace = "default"
	)

	var (
		ctx            context.Context
		cr             *pulsev1alpha1.OpenShiftPulse
		rootReconciler *OpenShiftPulseReconciler
	)

	BeforeEach(func() {
		ctx = testCtx

		rootReconciler = &OpenShiftPulseReconciler{
			Client:   k8sClient,
			Scheme:   testScheme,
			Recorder: record.NewFakeRecorder(10),
		}

		cr = &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec:       pulsev1alpha1.OpenShiftPulseSpec{},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
	})

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, cr)
		for _, name := range []string{agentClusterRoleName(crName, namespace), agentResourceName(crName)} {
			_ = k8sClient.Delete(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}})
			_ = k8sClient.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}})
		}
		_ = k8sClient.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: agentMonitoringViewBindingName(crName, namespace)}})
		for _, name := range []string{uiClusterRoleName(crName, namespace), uiClusterRoleNameUnqualified(crName)} {
			_ = k8sClient.Delete(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}})
			_ = k8sClient.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}})
			_ = k8sClient.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name + "-auth-delegator"}})
		}
		for _, name := range []string{mcpClusterRoleName(crName, namespace), mcpResourceName(crName)} {
			_ = k8sClient.Delete(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}})
			_ = k8sClient.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}})
		}
		oac := &unstructured.Unstructured{}
		oac.SetGroupVersionKind(schema.GroupVersionKind{Group: "oauth.openshift.io", Version: "v1", Kind: "OAuthClient"})
		oac.SetName(oauthClientName(crName, namespace))
		_ = k8sClient.Delete(ctx, oac)
	})

	It("ignores NotFound for every resource when nothing was ever created", func() {
		err := rootReconciler.deleteClusterScopedResources(ctx, cr)
		Expect(err).NotTo(HaveOccurred())
	})

	It("attempts every deletion and aggregates errors — one failure does not block the rest", func() {
		// Pre-create all eight cluster-scoped resources the finalizer is
		// responsible for, at their namespace-qualified names, so every
		// Delete call in deleteClusterScopedResources hits a real object
		// instead of NotFound.
		agentClusterRole := agentClusterRoleName(crName, namespace)
		uiClusterRole := uiClusterRoleName(crName, namespace)
		mcpClusterRole := mcpClusterRoleName(crName, namespace)

		agentCR := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: agentClusterRole}}
		Expect(k8sClient.Create(ctx, agentCR)).To(Succeed())

		agentCRB := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: agentClusterRole},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: agentClusterRole},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: agentResourceName(crName), Namespace: namespace}},
		}
		Expect(k8sClient.Create(ctx, agentCRB)).To(Succeed())

		uiCR := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: uiClusterRole}}
		Expect(k8sClient.Create(ctx, uiCR)).To(Succeed())

		uiCRB := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: uiClusterRole},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: uiClusterRole},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: uiResourceName(crName), Namespace: namespace}},
		}
		Expect(k8sClient.Create(ctx, uiCRB)).To(Succeed())

		authDelegatorCRB := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: uiClusterRole + "-auth-delegator"},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "system:auth-delegator"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: uiResourceName(crName), Namespace: namespace}},
		}
		Expect(k8sClient.Create(ctx, authDelegatorCRB)).To(Succeed())

		mcpCR := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: mcpClusterRole}}
		Expect(k8sClient.Create(ctx, mcpCR)).To(Succeed())

		mcpCRB := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: mcpClusterRole},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: mcpClusterRole},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: mcpResourceName(crName), Namespace: namespace}},
		}
		Expect(k8sClient.Create(ctx, mcpCRB)).To(Succeed())

		monitoringViewCRB := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: agentMonitoringViewBindingName(crName, namespace)},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-monitoring-view"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: agentResourceName(crName), Namespace: namespace}},
		}
		Expect(k8sClient.Create(ctx, monitoringViewCRB)).To(Succeed())

		oac := &unstructured.Unstructured{}
		oac.SetGroupVersionKind(schema.GroupVersionKind{Group: "oauth.openshift.io", Version: "v1", Kind: "OAuthClient"})
		oac.SetName(oauthClientName(crName, namespace))
		Expect(k8sClient.Create(ctx, oac)).To(Succeed())

		// Inject a failure on the agent ClusterRole's delete only. If the
		// implementation stopped at the first error (the pre-fix behaviour),
		// none of the other four deletes would ever be attempted.
		injectedErr := errors.New("injected: simulated transient API error")
		failingClient := &failingDeleteClient{
			Client:   k8sClient,
			failName: agentClusterRole,
			failErr:  injectedErr,
		}
		reconWithFailure := &OpenShiftPulseReconciler{
			Client:   failingClient,
			Scheme:   testScheme,
			Recorder: record.NewFakeRecorder(10),
		}

		err := reconWithFailure.deleteClusterScopedResources(ctx, cr)

		By("the aggregated error reports the injected failure")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("agent ClusterRole"))
		Expect(errors.Is(err, injectedErr)).To(BeTrue(), "errors.Join must preserve the underlying error for errors.Is")

		By("every resource's Delete was attempted, not just the first")
		Expect(failingClient.deleteCalls).To(ContainElements(
			agentClusterRole,                // agent ClusterRole — fails
			uiClusterRole,                   // UI ClusterRole
			agentClusterRole,                // agent ClusterRoleBinding
			uiClusterRole,                   // UI ClusterRoleBinding
			uiClusterRole+"-auth-delegator", // UI auth-delegator ClusterRoleBinding
			mcpClusterRole,                  // MCP ClusterRole
			mcpClusterRole,                  // MCP ClusterRoleBinding
			agentMonitoringViewBindingName(crName, namespace), // agent monitoring-view ClusterRoleBinding
			oauthClientName(crName, namespace),
		))

		By("the resources that did not fail were actually deleted")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: uiClusterRole}, &rbacv1.ClusterRole{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: agentClusterRole}, &rbacv1.ClusterRoleBinding{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: uiClusterRole}, &rbacv1.ClusterRoleBinding{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: uiClusterRole + "-auth-delegator"}, &rbacv1.ClusterRoleBinding{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: mcpClusterRole}, &rbacv1.ClusterRole{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: mcpClusterRole}, &rbacv1.ClusterRoleBinding{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: agentMonitoringViewBindingName(crName, namespace)}, &rbacv1.ClusterRoleBinding{}))).To(BeTrue())
		freshOAC := &unstructured.Unstructured{}
		freshOAC.SetGroupVersionKind(schema.GroupVersionKind{Group: "oauth.openshift.io", Version: "v1", Kind: "OAuthClient"})
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: oauthClientName(crName, namespace)}, freshOAC))).To(BeTrue())

		By("the resource that failed is still present (delete was attempted, not silently skipped)")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentClusterRole}, &rbacv1.ClusterRole{})).To(Succeed())
	})

	// Regression for the namespace-collision bug: before namespace
	// qualification, two OpenShiftPulse CRs sharing a name in different
	// namespaces would fight over one cluster-scoped ClusterRole/Binding.
	// This CR's finalizer must clean up any leftover old-named resource that
	// actually belongs to it (owner-uid matches) without ever touching a
	// same-named resource some other CR's owner-uid annotation claims.
	Describe("migrating stale pre-namespace-qualification resources", func() {
		It("deletes an old-named ClusterRole this CR owns, but leaves one owned by a different CR alone", func() {
			ours := &rbacv1.ClusterRole{
				ObjectMeta: metav1.ObjectMeta{
					Name:        agentResourceName(crName),
					Annotations: clusterScopedAnnotations(cr),
				},
			}
			Expect(k8sClient.Create(ctx, ours)).To(Succeed())

			otherCRName := "finalizer-test-other-pulse"
			otherCR := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: otherCRName, Namespace: namespace},
			}
			Expect(k8sClient.Create(ctx, otherCR)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, otherCR) }()

			notOurs := &rbacv1.ClusterRole{
				ObjectMeta: metav1.ObjectMeta{
					Name:        uiClusterRoleNameUnqualified(crName), // deliberately reuses crName's UI old-name slot
					Annotations: clusterScopedAnnotations(otherCR),    // but owned by a different CR
				},
			}
			Expect(k8sClient.Create(ctx, notOurs)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, notOurs) }()

			Expect(rootReconciler.deleteClusterScopedResources(ctx, cr)).To(Succeed())

			Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
				types.NamespacedName{Name: agentResourceName(crName)}, &rbacv1.ClusterRole{}))).To(BeTrue(),
				"the old-named ClusterRole this CR actually owns must be cleaned up")

			stillThere := &rbacv1.ClusterRole{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: uiClusterRoleNameUnqualified(crName)}, stillThere)).To(Succeed(),
				"a same-named resource owned by a different CR must never be deleted by this CR's finalizer")
		})
	})
})
