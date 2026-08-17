package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// reconcileWithBoundPVC runs two reconcile passes for AgentReconciler:
// 1st pass creates the PVC; then we patch its status to Bound so
// 2nd pass proceeds past the PVC gate and creates the Deployment.
func reconcileWithBoundPVC(ctx context.Context, crName, namespace string) (ctrl.Result, error) {
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace}}

	// First pass — creates SA, ClusterRole, PVC etc. Stops before Deployment.
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		return ctrl.Result{}, err
	}

	// Patch PVC status to Bound (envtest has no storage provisioner).
	pvc := &corev1.PersistentVolumeClaim{}
	pvcName := crName + "-openshift-sre-agent-memory"
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: namespace}, pvc); err != nil {
		return ctrl.Result{}, err
	}
	pvc.Status.Phase = corev1.ClaimBound
	pvc.Status.Capacity = corev1.ResourceList{
		corev1.ResourceStorage: resource.MustParse("1Gi"),
	}
	if err := k8sClient.Status().Update(ctx, pvc); err != nil {
		return ctrl.Result{}, err
	}

	// Second pass — PVC is Bound, proceeds to create Deployment.
	return reconciler.Reconcile(ctx, req)
}

var _ = Describe("AgentReconciler", func() {
	const (
		crName    = "test-pulse"
		namespace = "default"
	)

	var (
		cr  *pulsev1alpha1.OpenShiftPulse
		ctx context.Context
	)

	BeforeEach(func() {
		ctx = testCtx

		cr = &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{
				Name:      crName,
				Namespace: namespace,
			},
			Spec: pulsev1alpha1.OpenShiftPulseSpec{
				Agent: pulsev1alpha1.AgentConfig{
					Image:      "quay.io/amobrem/pulse-agent:test",
					TrustLevel: 2,
				},
			},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
	})

	AfterEach(func() {
		// Best-effort cleanup — delete CR and PVC so tests don't bleed into each other.
		_ = k8sClient.Delete(ctx, cr)
		pvc := &corev1.PersistentVolumeClaim{}
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      crName + "-openshift-sre-agent-memory",
			Namespace: namespace,
		}, pvc); err == nil {
			_ = k8sClient.Delete(ctx, pvc)
		}
	})

	It("CR creation triggers Deployment creation", func() {
		_, err := reconcileWithBoundPVC(ctx, crName, namespace)
		Expect(err).NotTo(HaveOccurred())

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      agentResourceName(crName),
			Namespace: namespace,
		}, deploy)).To(Succeed())
	})

	It("Deployment has the image from spec", func() {
		_, err := reconcileWithBoundPVC(ctx, crName, namespace)
		Expect(err).NotTo(HaveOccurred())

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      agentResourceName(crName),
			Namespace: namespace,
		}, deploy)).To(Succeed())

		Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))
		Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("quay.io/amobrem/pulse-agent:test"))
	})

	It("Deployment defaults to the standard image when spec.agent.image is empty", func() {
		cr.Spec.Agent.Image = ""
		Expect(k8sClient.Update(ctx, cr)).To(Succeed())

		_, err := reconcileWithBoundPVC(ctx, crName, namespace)
		Expect(err).NotTo(HaveOccurred())

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      agentResourceName(crName),
			Namespace: namespace,
		}, deploy)).To(Succeed())
		Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal(defaultAgentImage))
	})

	It("WS token secret is created with a 32-char hex token", func() {
		// Token is created in the first pass — no need to reach Deployment
		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace}}
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      wsTokenSecretName(crName),
			Namespace: namespace,
		}, secret)).To(Succeed())

		token, ok := secret.Data["token"]
		Expect(ok).To(BeTrue(), "secret must contain key 'token'")
		Expect(string(token)).To(HaveLen(32), "token must be 32-char hex")
		Expect(string(token)).To(MatchRegexp(`^[0-9a-f]{32}$`), "token must be lowercase hex")
	})

	It("WS token is not rotated on subsequent reconciles", func() {
		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace}}
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		secret1 := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      wsTokenSecretName(crName),
			Namespace: namespace,
		}, secret1)).To(Succeed())
		token1 := string(secret1.Data["token"])

		_, err = reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		secret2 := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      wsTokenSecretName(crName),
			Namespace: namespace,
		}, secret2)).To(Succeed())
		Expect(string(secret2.Data["token"])).To(Equal(token1))
	})

	It("agent Service carries an app label matching the ServiceMonitor selector", func() {
		// Regression: the ServiceMonitor's spec.selector.matchLabels matches
		// against this Service's own labels (not its spec.selector, which only
		// targets pods). Without this label Prometheus discovers the Service
		// but silently drops it, and metrics scraping never happens.
		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace}}
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      agentResourceName(crName),
			Namespace: namespace,
		}, svc)).To(Succeed())
		Expect(svc.Labels).To(HaveKeyWithValue("app", agentResourceName(crName)))
	})

	It("Deployment is created even while memory PVC is Pending (WaitForFirstConsumer)", func() {
		// With WaitForFirstConsumer storage classes, the PVC only binds once a pod
		// is scheduled. The gate (requeue until Bound) created a deadlock — removed.
		// Now the Deployment is created immediately; Kubernetes holds the pod Pending
		// until the volume is provisioned.
		uniqueName := "test-pulse-pvcgate"
		uniqueCR := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: uniqueName, Namespace: namespace},
			Spec:       pulsev1alpha1.OpenShiftPulseSpec{Agent: pulsev1alpha1.AgentConfig{Image: "quay.io/test:latest"}},
		}
		Expect(k8sClient.Create(ctx, uniqueCR)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, uniqueCR) })

		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: uniqueName, Namespace: namespace}}
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      agentResourceName(uniqueName),
			Namespace: namespace,
		}, deploy)).To(Succeed(), "Deployment should exist even with Pending PVC")
	})

	// Regression: a selector-mismatched Deployment (e.g. previously
	// Helm-managed) used to be handled by deleting it and returning a
	// synthetic error purely to force a requeue. controller-runtime turns
	// that into a Warning ReconcileFailed event with exponential backoff, so
	// a normal, successful migration step looked like a failing operator.
	It("deletes a selector-mismatched Deployment and requeues cleanly, without an error", func() {
		name := agentResourceName(crName)

		// envtest does not run the garbage collector controller, so a real
		// Deployment left behind by another spec in this suite (owned by the
		// same CR name, never cascade-deleted) can still exist here. Clear it
		// first so the fake "pre-existing, mismatched" object below can be
		// created cleanly regardless of run order.
		_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}})
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &appsv1.Deployment{}))
		}).Should(BeTrue())

		preExisting := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "helm-managed-agent"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "helm-managed-agent"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "busybox"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, preExisting)).To(Succeed())

		result, err := reconciler.reconcileDeployment(ctx, cr)
		Expect(err).NotTo(HaveOccurred(), "a selector mismatch is an expected migration step, not a failure")
		Expect(result.RequeueAfter).To(BeNumerically(">", 0), "must signal a requeue instead of relying on an error")

		deploy := &appsv1.Deployment{}
		err = k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, deploy)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "mismatched Deployment must actually have been deleted")
	})

	It("Deployment has readiness and liveness probes configured", func() {
		_, err := reconcileWithBoundPVC(ctx, crName, namespace)
		Expect(err).NotTo(HaveOccurred())

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      agentResourceName(crName),
			Namespace: namespace,
		}, deploy)).To(Succeed())

		containers := deploy.Spec.Template.Spec.Containers
		Expect(containers).To(HaveLen(1))
		c := containers[0]

		Expect(c.ReadinessProbe).NotTo(BeNil(), "readiness probe must be set")
		Expect(c.ReadinessProbe.HTTPGet).NotTo(BeNil())
		Expect(c.ReadinessProbe.HTTPGet.Path).To(Equal("/healthz"))
		Expect(c.ReadinessProbe.InitialDelaySeconds).To(Equal(int32(5)))

		Expect(c.LivenessProbe).NotTo(BeNil(), "liveness probe must be set")
		Expect(c.LivenessProbe.HTTPGet).NotTo(BeNil())
		Expect(c.LivenessProbe.HTTPGet.Path).To(Equal("/healthz"))
		Expect(c.LivenessProbe.InitialDelaySeconds).To(Equal(int32(15)))
	})

	It("Deployment uses Recreate strategy", func() {
		_, err := reconcileWithBoundPVC(ctx, crName, namespace)
		Expect(err).NotTo(HaveOccurred())

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      agentResourceName(crName),
			Namespace: namespace,
		}, deploy)).To(Succeed())
		Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
	})
})
