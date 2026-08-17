package controller

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// reconcileNginxAndRead runs reconcileUINginxConfigMap and returns the generated nginx.conf.
func reconcileNginxAndRead(ctx context.Context, ui *UIReconciler, cr *pulsev1alpha1.OpenShiftPulse) string {
	_, err := ui.reconcileUINginxConfigMap(ctx, cr)
	Expect(err).NotTo(HaveOccurred())

	cm := &corev1.ConfigMap{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name:      uiNginxConfigMapName(cr.Name),
		Namespace: cr.Namespace,
	}, cm)).To(Succeed())

	conf, ok := cm.Data["nginx.conf"]
	Expect(ok).To(BeTrue(), "ConfigMap must have nginx.conf key")
	return conf
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
		// Clean up ConfigMap
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: uiNginxConfigMapName(crName), Namespace: namespace}}
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
		Expect(conf).To(ContainSubstring("token=" + testToken),
			"WS proxy URL must contain the shared token as a query param")
		Expect(conf).To(ContainSubstring("Bearer " + testToken),
			"REST proxy must set Authorization: Bearer <token>")
	})

	It("returns an empty token (safe fallback) when the ws-token secret is absent", func() {
		// Secret doesn't exist — reconcile must not error, just embed empty token.
		conf := reconcileNginxAndRead(ctx, ui, cr)
		// Should contain token= (empty) — proxy rules are still present.
		Expect(conf).To(ContainSubstring("token="))
		Expect(conf).To(ContainSubstring("/api/agent/ws/"))
	})

	It("ConfigMap is idempotent — second reconcile does not change hash", func() {
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
