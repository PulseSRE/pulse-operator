package controller

// End-to-end lifecycle tests driving OpenShiftPulseReconciler.Reconcile directly
// — the actual production entrypoint. Before this file, Reconcile itself (finalizer
// handling, Phase computation, Degraded detection, UIAvailable gating) had 0% test
// coverage: every other test in this package drives individual sub-reconciler
// functions instead. This file exists specifically to close that gap, and to prove
// (rather than just assert by inspection) that every resource the operator creates
// either has a real OwnerReference back to the CR or is covered by the finalizer's
// cluster-scoped cleanup — the exact class of bug that let the PostgreSQL stack and
// the Route leak silently on CR deletion.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// mustHaveOwner fails the test unless obj has at least one OwnerReference
// pointing at the given CR name/UID.
func mustHaveOwner(obj metav1.Object, crName string, desc string) {
	owners := obj.GetOwnerReferences()
	ExpectWithOffset(1, owners).NotTo(BeEmpty(), "%s must have an OwnerReference (would leak on CR deletion otherwise)", desc)
	var found bool
	for _, o := range owners {
		if o.Name == crName && o.Kind == "OpenShiftPulse" {
			found = true
		}
	}
	ExpectWithOffset(1, found).To(BeTrue(), "%s must be owned by the OpenShiftPulse CR %q", desc, crName)
}

var _ = Describe("OpenShiftPulseReconciler.Reconcile — full lifecycle", func() {
	const (
		crName    = "lifecycle-pulse"
		namespace = "default"
	)

	var (
		ctx  context.Context
		root *OpenShiftPulseReconciler
		req  ctrl.Request
	)

	BeforeEach(func() {
		ctx = testCtx
		root = &OpenShiftPulseReconciler{
			Client:   k8sClient,
			Scheme:   testScheme,
			Recorder: record.NewFakeRecorder(100),
			UIReconciler: &UIReconciler{
				Client: k8sClient,
				Scheme: testScheme,
			},
		}
		req = ctrl.Request{NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace}}

		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec: pulsev1alpha1.OpenShiftPulseSpec{
				VertexAI:   &pulsev1alpha1.VertexAIConfig{ProjectID: "test-project"},
				Agent:      pulsev1alpha1.AgentConfig{Image: "quay.io/test/pulse-agent:test", TrustLevel: 2},
				UI:         pulsev1alpha1.UIConfig{Image: "quay.io/test/openshiftpulse:test", Replicas: 2},
				Database:   pulsev1alpha1.DatabaseConfig{StorageSize: "1Gi"},
				Monitoring: pulsev1alpha1.MonitoringConfig{Enabled: boolPtr(true)},
			},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
	})

	AfterEach(func() {
		cr := &pulsev1alpha1.OpenShiftPulse{}
		if err := k8sClient.Get(ctx, req.NamespacedName, cr); err == nil {
			controllerutilRemoveFinalizerAndDelete(ctx, cr)
		}
	})

	It("runs the full create -> healthy -> delete -> cleaned-up lifecycle", func() {
		By("first reconcile adds the finalizer and returns immediately")
		_, err := root.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		cr := &pulsev1alpha1.OpenShiftPulse{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, cr)).To(Succeed())
		Expect(controllerutilContainsFinalizer(cr)).To(BeTrue())

		By("second reconcile creates every managed resource and waits on the Route hostname")
		result, err := root.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0), "must requeue while waiting for the Route hostname")

		Expect(k8sClient.Get(ctx, req.NamespacedName, cr)).To(Succeed())
		Expect(cr.Status.Phase).To(Equal("Installing"))

		By("every namespaced resource the operator created has an OwnerReference back to the CR")
		// PostgreSQL — the exact class of bug this test guards against: these
		// resources previously only got a non-blocking annotation, never a real
		// OwnerReference, and silently outlived CR deletion forever.
		//
		// pg-auth is the deliberate exception: it must NOT have an
		// OwnerReference, so it (and the matching PGDATA on the retained PVC)
		// survive CR deletion together and get correctly reused if the CR is
		// recreated with the same name — see reconcilePGSecret's doc comment,
		// and postgresql_reconciler_test.go's "credentials and data survive a
		// CR delete+recreate cycle" test for the actual regression coverage.
		pgAuth := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName + "-pg-auth", Namespace: namespace}, pgAuth)).To(Succeed())
		Expect(pgAuth.GetOwnerReferences()).To(BeEmpty(),
			"pg-auth must have no OwnerReference — it's designed to outlive CR deletion, matching the retained PG data PVC")

		pgSTS := &appsv1.StatefulSet{}
		stsName := crName + "-openshift-sre-agent-postgresql"
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: stsName, Namespace: namespace}, pgSTS)).To(Succeed())
		mustHaveOwner(pgSTS, crName, "PostgreSQL StatefulSet")

		pgSvc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: stsName, Namespace: namespace}, pgSvc)).To(Succeed())
		mustHaveOwner(pgSvc, crName, "PostgreSQL Service")

		pgHeadlessSvc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: stsName + "-headless", Namespace: namespace}, pgHeadlessSvc)).To(Succeed())
		mustHaveOwner(pgHeadlessSvc, crName, "PostgreSQL headless Service")

		pgConnSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName + "-postgresql", Namespace: namespace}, pgConnSecret)).To(Succeed())
		mustHaveOwner(pgConnSecret, crName, "PostgreSQL connection Secret")

		// Agent
		agentDeploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentResourceName(crName), Namespace: namespace}, agentDeploy)).To(Succeed())
		mustHaveOwner(agentDeploy, crName, "agent Deployment")

		wsSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: wsTokenSecretName(crName), Namespace: namespace}, wsSecret)).To(Succeed())
		mustHaveOwner(wsSecret, crName, "ws-token Secret")

		// UI + Route (Route previously had NO OwnerReference at all and wasn't
		// covered by the finalizer either — silently orphaned on every delete).
		uiDeploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: uiResourceName(crName), Namespace: namespace}, uiDeploy)).To(Succeed())
		mustHaveOwner(uiDeploy, crName, "UI Deployment")

		route := &unstructured.Unstructured{}
		route.SetGroupVersionKind(schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"})
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: uiResourceName(crName), Namespace: namespace}, route)).To(Succeed())
		mustHaveOwner(route, crName, "Route")

		// NetworkPolicies + PDB
		uiNP := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName + "-openshiftpulse", Namespace: namespace}, uiNP)).To(Succeed())
		mustHaveOwner(uiNP, crName, "UI NetworkPolicy")

		agentNP := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName + "-agent-access", Namespace: namespace}, agentNP)).To(Succeed())
		mustHaveOwner(agentNP, crName, "agent NetworkPolicy")

		pdb := &policyv1.PodDisruptionBudget{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName + "-openshiftpulse", Namespace: namespace}, pdb)).To(Succeed())
		mustHaveOwner(pdb, crName, "PodDisruptionBudget")

		// Monitoring
		sm := &unstructured.Unstructured{}
		sm.SetGroupVersionKind(serviceMonitorGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentResourceName(crName), Namespace: namespace}, sm)).To(Succeed())
		mustHaveOwner(sm, crName, "ServiceMonitor")

		By("simulating the OCP router admitting the Route hostname")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: uiResourceName(crName), Namespace: namespace}, route)).To(Succeed())
		Expect(unstructured.SetNestedField(route.Object, "lifecycle-pulse-default.apps.example.com", "spec", "host")).To(Succeed())
		Expect(k8sClient.Update(ctx, route)).To(Succeed())

		By("simulating pods becoming Ready (envtest has no kubelet to run real containers)")
		patchDeploymentReady(ctx, agentDeploy, 1)
		patchStatefulSetReady(ctx, pgSTS, 1)
		patchDeploymentReady(ctx, uiDeploy, 2)

		By("third reconcile: UIAvailable must reflect real Deployment readiness, not just Route admission")
		_, err = root.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, req.NamespacedName, cr)).To(Succeed())
		Expect(cr.Status.AgentHealthy).To(BeTrue())
		Expect(cr.Status.DatabaseReady).To(BeTrue())
		Expect(cr.Status.UIAvailable).To(BeTrue(), "UIAvailable must be true once the UI Deployment is actually ready")
		Expect(cr.Status.Phase).To(Equal("Running"))
		Expect(cr.Status.RouteHost).To(Equal("lifecycle-pulse-default.apps.example.com"))

		readyCond := findCondition(cr.Status.Conditions, "Ready")
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))

		By("regression: UIAvailable must go false again if the UI Deployment stops being ready, without a full re-run")
		patchDeploymentReady(ctx, uiDeploy, 0)
		_, err = root.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, req.NamespacedName, cr)).To(Succeed())
		Expect(cr.Status.UIAvailable).To(BeFalse())
		Expect(cr.Status.Phase).To(Equal("Degraded"), "was Running, one component went unhealthy -> Degraded, not Installing")

		By("deleting the CR triggers finalizer cleanup of cluster-scoped resources")
		Expect(k8sClient.Delete(ctx, cr)).To(Succeed())

		_, err = root.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		By("the CR itself is gone once the finalizer is removed")
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, req.NamespacedName, &pulsev1alpha1.OpenShiftPulse{}))
		}).Should(BeTrue())

		By("cluster-scoped resources created for this CR are gone too")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: agentClusterRoleName(crName, namespace)}, &rbacv1.ClusterRole{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: agentClusterRoleName(crName, namespace)}, &rbacv1.ClusterRoleBinding{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: uiClusterRoleName(crName, namespace)}, &rbacv1.ClusterRole{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: uiClusterRoleName(crName, namespace)}, &rbacv1.ClusterRoleBinding{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: uiClusterRoleName(crName, namespace) + "-auth-delegator"}, &rbacv1.ClusterRoleBinding{}))).To(BeTrue())

		oac := &unstructured.Unstructured{}
		oac.SetGroupVersionKind(schema.GroupVersionKind{Group: "oauth.openshift.io", Version: "v1", Kind: "OAuthClient"})
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: oauthClientName(crName, namespace)}, oac))).To(BeTrue())
	})
})

// ── test helpers ─────────────────────────────────────────────────────────────

func controllerutilContainsFinalizer(cr *pulsev1alpha1.OpenShiftPulse) bool {
	for _, f := range cr.Finalizers {
		if f == finalizerName {
			return true
		}
	}
	return false
}

// controllerutilRemoveFinalizerAndDelete is a best-effort AfterEach cleanup that
// removes the finalizer directly so a failed assertion mid-test never leaves a
// CR stuck Terminating for the rest of the suite.
func controllerutilRemoveFinalizerAndDelete(ctx context.Context, cr *pulsev1alpha1.OpenShiftPulse) {
	_ = k8sClient.Delete(ctx, cr)
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}, cr); err == nil {
		var kept []string
		for _, f := range cr.Finalizers {
			if f != finalizerName {
				kept = append(kept, f)
			}
		}
		cr.Finalizers = kept
		_ = k8sClient.Update(ctx, cr)
	}
}

func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

func patchDeploymentReady(ctx context.Context, deploy *appsv1.Deployment, ready int32) {
	fresh := &appsv1.Deployment{}
	ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: deploy.Name, Namespace: deploy.Namespace}, fresh)).To(Succeed())
	fresh.Status.ReadyReplicas = ready
	fresh.Status.Replicas = ready
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, fresh)).To(Succeed())
}

func patchStatefulSetReady(ctx context.Context, sts *appsv1.StatefulSet, ready int32) {
	fresh := &appsv1.StatefulSet{}
	ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: sts.Name, Namespace: sts.Namespace}, fresh)).To(Succeed())
	fresh.Status.ReadyReplicas = ready
	fresh.Status.Replicas = ready
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, fresh)).To(Succeed())
}
