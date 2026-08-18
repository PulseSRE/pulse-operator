package controller

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// reconcileNginxAndRead runs reconcileUINginxConfigMap and returns the generated nginx.conf.
func reconcileNginxAndRead(ctx context.Context, ui *UIReconciler, cr *pulsev1alpha1.OpenShiftPulse) string {
	_, err := ui.reconcileUINginxConfigMap(ctx, cr)
	Expect(err).NotTo(HaveOccurred())

	sec := &corev1.Secret{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name:      uiNginxSecretName(cr.Name),
		Namespace: cr.Namespace,
	}, sec)).To(Succeed())

	// Written via StringData; the API server surfaces it back under Data.
	conf, ok := sec.Data["nginx.conf"]
	Expect(ok).To(BeTrue(), "Secret must have nginx.conf key")
	return string(conf)
}

var _ = Describe("UIReconciler nginx config", func() {
	const (
		crName    = "nginx-test-pulse"
		namespace = "default"
		testToken = "deadbeef01234567deadbeef01234567"
	)

	var (
		cr  *pulsev1alpha1.OpenShiftPulse
		ui  *UIReconciler
		ctx context.Context
	)

	BeforeEach(func() {
		ctx = testCtx
		ui = &UIReconciler{Client: k8sClient, Scheme: testScheme}

		cr = &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec:       pulsev1alpha1.OpenShiftPulseSpec{},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
	})

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, cr)
		// Clean up the rendered nginx Secret
		cm := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: uiNginxSecretName(crName), Namespace: namespace}}
		_ = k8sClient.Delete(ctx, cm)
	})

	It("proxies /api/agent/ws/ with WebSocket upgrade headers", func() {
		conf := reconcileNginxAndRead(ctx, ui, cr)
		Expect(conf).To(ContainSubstring("proxy_set_header Upgrade $http_upgrade"))
		Expect(conf).To(ContainSubstring("proxy_set_header Connection $connection_upgrade"))
		Expect(conf).To(ContainSubstring("/api/agent/ws/"))
		Expect(conf).To(ContainSubstring("proxy_http_version 1.1"))
	})

	It("proxies /api/agent/ to the agent service REST endpoint", func() {
		conf := reconcileNginxAndRead(ctx, ui, cr)
		agentSvc := agentResourceName(crName) // e.g. "nginx-test-pulse-openshift-sre-agent"
		Expect(conf).To(ContainSubstring("location /api/agent/"))
		Expect(conf).To(ContainSubstring("proxy_pass http://" + agentSvc + ":8080/"))
	})

	It("proxies /api/kubernetes/ to the in-cluster Kubernetes API", func() {
		conf := reconcileNginxAndRead(ctx, ui, cr)
		Expect(conf).To(ContainSubstring("location /api/kubernetes/"))
		Expect(conf).To(ContainSubstring("proxy_pass https://kubernetes.default.svc/"))
	})

	// Regression: the UI's CPU/memory/alert charts call /api/prometheus/ (see
	// its React app), but nginx.conf never had a matching location block —
	// every one of those requests silently fell through to the SPA
	// catch-all and got back index.html (HTTP 200, so no error surfaced
	// anywhere), which the frontend then failed to parse as PromQL JSON.
	// Charts showed dashes / "metric unavailable" with no visible cause.
	It("proxies /api/prometheus/ to the cluster's thanos-querier, before it reaches the SPA catch-all", func() {
		conf := reconcileNginxAndRead(ctx, ui, cr)
		Expect(conf).To(ContainSubstring("location /api/prometheus/"))
		Expect(conf).To(ContainSubstring("proxy_pass https://thanos-querier.openshift-monitoring.svc:9091/"))
		Expect(conf).To(ContainSubstring(`proxy_set_header Authorization "Bearer $http_x_forwarded_access_token"`))

		promIdx := strings.Index(conf, "location /api/prometheus/")
		catchAllIdx := strings.Index(conf, "location / {")
		Expect(promIdx).To(BeNumerically(">", 0))
		Expect(catchAllIdx).To(BeNumerically(">", 0))
		Expect(promIdx).To(BeNumerically("<", catchAllIdx),
			"/api/prometheus/ must appear before the catch-all / in the config")
	})

	It("embeds the WS token when the ws-token secret exists", func() {
		// Create the ws-token secret that AgentReconciler would normally create.
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      wsTokenSecretName(crName),
				Namespace: namespace,
			},
			StringData: map[string]string{"token": testToken},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, secret) })

		conf := reconcileNginxAndRead(ctx, ui, cr)
		Expect(conf).To(ContainSubstring("token="+testToken),
			"WS proxy URL must contain the shared token as a query param")
		Expect(conf).To(ContainSubstring("Bearer "+testToken),
			"REST proxy must set Authorization: Bearer <token>")
	})

	It("returns an empty token (safe fallback) when the ws-token secret is absent", func() {
		// Secret doesn't exist — reconcile must not error, just embed empty token.
		conf := reconcileNginxAndRead(ctx, ui, cr)
		// Should contain token= (empty) — proxy rules are still present.
		Expect(conf).To(ContainSubstring("token="))
		Expect(conf).To(ContainSubstring("/api/agent/ws/"))
	})

	It("Secret is idempotent — second reconcile does not change hash", func() {
		hash1, err := ui.reconcileUINginxConfigMap(ctx, cr)
		Expect(err).NotTo(HaveOccurred())
		hash2, err := ui.reconcileUINginxConfigMap(ctx, cr)
		Expect(err).NotTo(HaveOccurred())
		Expect(hash1).To(Equal(hash2), "hash must be stable across reconciles")
	})

	It("nginx config has map block for WebSocket connection upgrade", func() {
		conf := reconcileNginxAndRead(ctx, ui, cr)
		Expect(conf).To(ContainSubstring("map $http_upgrade $connection_upgrade"))
	})

	It("nginx config writes temp paths under /tmp for non-root compatibility", func() {
		conf := reconcileNginxAndRead(ctx, ui, cr)
		Expect(conf).To(ContainSubstring("client_body_temp_path /tmp/"))
		Expect(conf).To(ContainSubstring("proxy_temp_path /tmp/"))
	})

	It("nginx config does NOT serve index.html for /api/ routes", func() {
		// The /api/agent/ location block must appear before the SPA catch-all
		// "location / {" so nginx evaluates the specific prefix first.
		conf := reconcileNginxAndRead(ctx, ui, cr)
		agentIdx := strings.Index(conf, "location /api/agent/")
		// Match exactly the catch-all block — "location / {" with trailing space+brace.
		catchAllIdx := strings.Index(conf, "location / {")
		Expect(agentIdx).To(BeNumerically(">", 0), "/api/agent/ block must be present")
		Expect(catchAllIdx).To(BeNumerically(">", 0), "SPA catch-all block must be present")
		Expect(agentIdx).To(BeNumerically("<", catchAllIdx),
			"/api/agent/ must appear before the catch-all / in the config")
	})
})

var _ = Describe("UIReconciler nginx object is a Secret, not a ConfigMap", func() {
	const (
		crName    = "nginx-secret-pulse"
		namespace = "default"
	)

	It("stores the rendered config in a Secret and removes any stale ConfigMap", func() {
		ctx := context.Background()
		ui := &UIReconciler{Client: k8sClient, Scheme: testScheme}
		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, cr) }()

		// Simulate an install from before this change: a ConfigMap of the same
		// name holding the token in plain sight.
		stale := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: uiNginxSecretName(crName), Namespace: namespace},
			Data:       map[string]string{"nginx.conf": "stale"},
		}
		Expect(k8sClient.Create(ctx, stale)).To(Succeed())

		_, err := ui.reconcileUINginxConfigMap(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		// The config now lives in a Secret...
		sec := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: uiNginxSecretName(crName), Namespace: namespace,
		}, sec)).To(Succeed())
		Expect(string(sec.Data["nginx.conf"])).To(ContainSubstring("location /api/agent/"))

		// ...and the old ConfigMap is gone, so the token is not left readable by
		// anything holding the cluster-wide configmaps read the agent and UI have.
		leftover := &corev1.ConfigMap{}
		getErr := k8sClient.Get(ctx, types.NamespacedName{
			Name: uiNginxSecretName(crName), Namespace: namespace,
		}, leftover)
		Expect(apierrors.IsNotFound(getErr)).To(BeTrue(),
			"the pre-Secret ConfigMap must be deleted, not left behind with the token in it")
	})
})
