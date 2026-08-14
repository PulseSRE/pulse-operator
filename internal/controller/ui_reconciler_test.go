package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

var _ = Describe("UIReconciler", func() {
	const (
		uiCRName  = "test-ui-pulse"
		namespace = "default"
	)

	var (
		cr          *pulsev1alpha1.OpenShiftPulse
		ctx         context.Context
		uiReconciler *UIReconciler
	)

	BeforeEach(func() {
		ctx = testCtx

		uiReconciler = &UIReconciler{
			Client: k8sClient,
			Scheme: testScheme,
		}

		cr = &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{
				Name:      uiCRName,
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
		_ = k8sClient.Delete(ctx, cr)
	})

	It("reconcileUI creates the nginx ConfigMap", func() {
		err := uiReconciler.reconcileUINginxConfigMap(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      uiNginxConfigMapName(uiCRName),
			Namespace: namespace,
		}, cm)).To(Succeed())

		Expect(cm.Data).To(HaveKey("nginx.conf"))
		Expect(cm.Data["nginx.conf"]).To(ContainSubstring("listen 8080"))
	})

	It("reconcileUI creates the Service on port 8443", func() {
		err := uiReconciler.reconcileUIService(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      uiResourceName(uiCRName),
			Namespace: namespace,
		}, svc)).To(Succeed())

		Expect(svc.Spec.Ports).To(HaveLen(1))
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8443)))
		Expect(svc.Spec.Ports[0].Name).To(Equal("https"))
	})

	It("reconcileUI creates the oauth-secrets Secret with client-secret and cookie-secret keys", func() {
		err := uiReconciler.reconcileUIOAuthSecrets(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      uiOAuthSecretsName(uiCRName),
			Namespace: namespace,
		}, secret)).To(Succeed())

		_, hasClientSecret := secret.Data["client-secret"]
		Expect(hasClientSecret).To(BeTrue(), "secret must contain key 'client-secret'")

		_, hasCookieSecret := secret.Data["cookie-secret"]
		Expect(hasCookieSecret).To(BeTrue(), "secret must contain key 'cookie-secret'")
	})

	It("OAuthClient is created with correct redirectURI after Route hostname is known", func() {
		// Simulate the post-route-ready path: call reconcileOAuthClient directly
		// with a known routeHost and a valid client secret.
		testRouteHost := "test-ui-pulse-default.apps.example.com"
		testClientSecret := "test-client-secret-abc123"

		err := uiReconciler.reconcileOAuthClient(ctx, cr, testRouteHost, testClientSecret)
		if err != nil {
			if isNoCRDError(err) {
				Skip("OAuthClient CRD not installed in envtest — skipping OAuthClient check")
			}
			Expect(err).NotTo(HaveOccurred())
		}

		// Verify OAuthClient was created with the correct redirectURI
		oauthGVK := schema.GroupVersionKind{
			Group:   "oauth.openshift.io",
			Version: "v1",
			Kind:    "OAuthClient",
		}
		oauthClient := &unstructured.Unstructured{}
		oauthClient.SetGroupVersionKind(oauthGVK)
		getErr := k8sClient.Get(ctx, types.NamespacedName{Name: oauthClientName}, oauthClient)
		if isNoCRDError(getErr) {
			Skip("OAuthClient CRD not installed in envtest — skipping")
		}
		Expect(getErr).NotTo(HaveOccurred())

		redirectURIs, _, _ := unstructured.NestedStringSlice(oauthClient.Object, "redirectURIs")
		Expect(redirectURIs).To(ContainElement("https://" + testRouteHost))
	})

	It("Route is created", func() {
		routeGVK := schema.GroupVersionKind{
			Group:   "route.openshift.io",
			Version: "v1",
			Kind:    "Route",
		}

		_, _, err := uiReconciler.reconcileUIRoute(ctx, cr, &ClusterInfo{})
		if err != nil {
			// Route CRD not installed in envtest — skip gracefully.
			if apierrors.IsNotFound(err) || isNoCRDError(err) {
				Skip("Route CRD not installed in envtest — skipping Route creation check")
			}
			// Any other error is a real failure.
			Expect(err).NotTo(HaveOccurred())
		}

		// If reconcileUIRoute succeeded (no error), verify the Route object exists.
		route := &unstructured.Unstructured{}
		route.SetGroupVersionKind(routeGVK)
		getErr := k8sClient.Get(ctx, types.NamespacedName{
			Name:      uiResourceName(uiCRName),
			Namespace: namespace,
		}, route)
		if apierrors.IsNotFound(getErr) || isNoCRDError(getErr) {
			Skip("Route CRD not installed in envtest — skipping Route existence check")
		}
		Expect(getErr).NotTo(HaveOccurred())
		Expect(route.GetName()).To(Equal(uiResourceName(uiCRName)))
	})
})

// isNoCRDError returns true for "no kind is registered" or "no matches for kind" errors
// that occur when a CRD is absent from the envtest API server.
func isNoCRDError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return contains(msg, "no kind is registered") ||
		contains(msg, "no matches for kind") ||
		contains(msg, "no match for kind") ||
		contains(msg, "the server could not find the requested resource")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
