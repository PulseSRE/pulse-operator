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
		cr           *pulsev1alpha1.OpenShiftPulse
		ctx          context.Context
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
		_, err := uiReconciler.reconcileUINginxConfigMap(ctx, cr)
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

		cookieSecret, hasCookieSecret := secret.Data["cookie-secret"]
		Expect(hasCookieSecret).To(BeTrue(), "secret must contain key 'cookie-secret'")

		// Cookie secret must pass isValidCookieSecret — exactly 32 printable ASCII bytes.
		// Raw random bytes risk nulls/newlines that truncate the AES key.
		// 44-char base64 fails oauth-proxy's "must be 16/24/32 bytes" check when
		// --pass-access-token=true is set. This test catches both regressions.
		Expect(isValidCookieSecret(cookieSecret)).To(BeTrue(),
			"cookie-secret must be 32 printable non-whitespace ASCII bytes for AES-256 compatibility")
	})

	It("reconcileUI replaces a malformed cookie-secret (raw bytes or wrong length)", func() {
		// Use a unique CR so we start with no existing secret.
		const badCRName = "test-ui-pulse-badcookie"
		badCR := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: badCRName, Namespace: namespace},
			Spec:       pulsev1alpha1.OpenShiftPulseSpec{},
		}
		Expect(k8sClient.Create(ctx, badCR)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, badCR) })

		// Pre-create the secret with a 44-byte base64 cookie secret (old format).
		badSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      uiOAuthSecretsName(badCRName),
				Namespace: namespace,
			},
			Data: map[string][]byte{
				"client-secret": []byte("some-client-secret"),
				// 44-byte base64: invalid for AES when pass-access-token is set.
				"cookie-secret": []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"),
			},
		}
		Expect(k8sClient.Create(ctx, badSecret)).To(Succeed())

		// Reconcile should detect the bad format and regenerate.
		err := uiReconciler.reconcileUIOAuthSecrets(ctx, badCR)
		Expect(err).NotTo(HaveOccurred())

		regenerated := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      uiOAuthSecretsName(badCRName),
			Namespace: namespace,
		}, regenerated)).To(Succeed())

		Expect(isValidCookieSecret(regenerated.Data["cookie-secret"])).To(BeTrue(),
			"operator must regenerate a valid 32-byte cookie secret after detecting a malformed one")

		// Regression guard: fixing an unrelated cookie-secret format bug must never
		// rotate client-secret — that invalidates the OAuthClient's live grant for
		// no reason connected to the actual bug being fixed.
		Expect(string(regenerated.Data["client-secret"])).To(Equal("some-client-secret"),
			"client-secret must be preserved untouched when only cookie-secret is regenerated")
	})

	It("oauth-proxy args never contain --openshift-delegate-urls", func() {
		// delegate-urls blocks unauthenticated requests before OAuth redirect fires,
		// trapping users in a 403 they cannot escape. This test prevents regression.
		cr.Spec.UI.Replicas = 1
		info := &ClusterInfo{
			IngressDomain:   "apps.example.com",
			OAuthProxyImage: DefaultOAuthProxyImage,
		}
		err := uiReconciler.reconcileUIDeployment(ctx, cr, info, "testhash")
		Expect(err).NotTo(HaveOccurred())

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      uiResourceName(uiCRName),
			Namespace: namespace,
		}, deploy)).To(Succeed())

		// Find the oauth-proxy container (index 1).
		Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(2))
		for _, arg := range deploy.Spec.Template.Spec.Containers[1].Args {
			Expect(arg).NotTo(ContainSubstring("openshift-delegate-urls"),
				"--openshift-delegate-urls must never appear — it hard-blocks login with 403")
		}
	})

	It("oauth-proxy requests user:full scope (required for --pass-access-token to work against the K8s API)", func() {
		// oauth-proxy's default scope (user:info user:check-access) can authenticate
		// a user but cannot be used by nginx's /api/kubernetes/ proxy to actually list
		// resources — that needs user:full requested explicitly. Regression guard for
		// the "nodes/resources not loading in UI" symptom this chain of fixes targets.
		cr.Spec.UI.Replicas = 1
		info := &ClusterInfo{
			IngressDomain:   "apps.example.com",
			OAuthProxyImage: DefaultOAuthProxyImage,
		}
		err := uiReconciler.reconcileUIDeployment(ctx, cr, info, "testhash")
		Expect(err).NotTo(HaveOccurred())

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      uiResourceName(uiCRName),
			Namespace: namespace,
		}, deploy)).To(Succeed())

		Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(2))
		Expect(deploy.Spec.Template.Spec.Containers[1].Args).To(ContainElement("--scope=user:full"))
	})

	It("applies spec.ui.resources to the openshiftpulse container, and defaults it when unset", func() {
		// Regression: spec.ui.resources was defined in the CRD and read by zero
		// code — the UI container ran unbounded regardless of what an operator
		// configured. Cover both the override path and the default path.
		cr.Spec.UI.Replicas = 1
		cr.Spec.UI.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("777Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("999Mi")},
		}
		info := &ClusterInfo{IngressDomain: "apps.example.com", OAuthProxyImage: DefaultOAuthProxyImage}
		Expect(uiReconciler.reconcileUIDeployment(ctx, cr, info, "testhash")).To(Succeed())

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      uiResourceName(uiCRName),
			Namespace: namespace,
		}, deploy)).To(Succeed())

		Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(2))
		got := deploy.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
		Expect(got.String()).To(Equal("777Mi"), "spec.ui.resources override must reach the openshiftpulse container")

		// Now the default path: a fresh CR that never sets spec.ui.resources.
		cr.Spec.UI.Resources = corev1.ResourceRequirements{}
		Expect(uiReconciler.reconcileUIDeployment(ctx, cr, info, "testhash2")).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      uiResourceName(uiCRName),
			Namespace: namespace,
		}, deploy)).To(Succeed())
		gotDefault := deploy.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
		Expect(gotDefault.IsZero()).To(BeFalse(), "openshiftpulse container must have a non-zero default request when spec.ui.resources is unset")
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
		getErr := k8sClient.Get(ctx, types.NamespacedName{Name: oauthClientName(uiCRName, namespace)}, oauthClient)
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

	// Regression: reconcileUIRoute used to only ever read spec.host on an
	// existing Route and never wrote anything back — any external change to
	// spec.to/spec.port.targetPort/spec.tls (e.g. a manual kubectl edit)
	// would persist forever instead of being corrected on the next reconcile.
	It("corrects drift on an already-existing Route's spec.to/spec.port/spec.tls, without touching spec.host", func() {
		routeGVK := schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"}

		_, _, err := uiReconciler.reconcileUIRoute(ctx, cr, &ClusterInfo{})
		if err != nil && (apierrors.IsNotFound(err) || isNoCRDError(err)) {
			Skip("Route CRD not installed in envtest — skipping Route drift check")
		}
		Expect(err).NotTo(HaveOccurred())

		route := &unstructured.Unstructured{}
		route.SetGroupVersionKind(routeGVK)
		getErr := k8sClient.Get(ctx, types.NamespacedName{Name: uiResourceName(uiCRName), Namespace: namespace}, route)
		if apierrors.IsNotFound(getErr) || isNoCRDError(getErr) {
			Skip("Route CRD not installed in envtest — skipping Route drift check")
		}
		Expect(getErr).NotTo(HaveOccurred())

		// Simulate external drift (e.g. a manual kubectl edit) and a
		// pre-existing spec.host, which must survive untouched.
		Expect(unstructured.SetNestedField(route.Object, "manually-set-host.apps.example.com", "spec", "host")).To(Succeed())
		Expect(unstructured.SetNestedField(route.Object, map[string]interface{}{
			"kind": "Service", "name": "some-other-service", "weight": int64(100),
		}, "spec", "to")).To(Succeed())
		Expect(unstructured.SetNestedField(route.Object, map[string]interface{}{
			"termination": "edge", "insecureEdgeTerminationPolicy": "Allow",
		}, "spec", "tls")).To(Succeed())
		Expect(k8sClient.Update(ctx, route)).To(Succeed())

		host, _, err := uiReconciler.reconcileUIRoute(ctx, cr, &ClusterInfo{})
		Expect(err).NotTo(HaveOccurred())
		Expect(host).To(Equal("manually-set-host.apps.example.com"), "spec.host must be preserved, not overwritten")

		corrected := &unstructured.Unstructured{}
		corrected.SetGroupVersionKind(routeGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: uiResourceName(uiCRName), Namespace: namespace}, corrected)).To(Succeed())

		to, _, _ := unstructured.NestedString(corrected.Object, "spec", "to", "name")
		Expect(to).To(Equal(uiResourceName(uiCRName)), "spec.to must be corrected back to the UI Service")

		termination, _, _ := unstructured.NestedString(corrected.Object, "spec", "tls", "termination")
		Expect(termination).To(Equal("reencrypt"), "spec.tls must be corrected back to reencrypt")
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
