package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

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
		// Best-effort cleanup so tests don't bleed into each other.
		_ = k8sClient.Delete(ctx, cr)
	})

	It("CR creation triggers Deployment creation", func() {
		req := ctrl.Request{
			NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace},
		}
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      agentResourceName(crName),
			Namespace: namespace,
		}, deploy)).To(Succeed())
	})

	It("Deployment has the image from spec", func() {
		req := ctrl.Request{
			NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace},
		}
		_, err := reconciler.Reconcile(ctx, req)
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

		req := ctrl.Request{
			NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace},
		}
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      agentResourceName(crName),
			Namespace: namespace,
		}, deploy)).To(Succeed())
		Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal(defaultAgentImage))
	})

	It("WS token secret is created with a 32-char hex token", func() {
		req := ctrl.Request{
			NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace},
		}
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
		req := ctrl.Request{
			NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace},
		}

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

	It("Deployment uses Recreate strategy", func() {
		req := ctrl.Request{
			NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace},
		}
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      agentResourceName(crName),
			Namespace: namespace,
		}, deploy)).To(Succeed())
		Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
	})
})
