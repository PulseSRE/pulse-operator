package controller

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// envValue is envVar without the found flag, for direct comparison.
func envValue(deploy *appsv1.Deployment, name string) string {
	v, _ := envVar(deploy, name)
	return v
}

// renderServerBlock returns the nginx server block that listens on the render
// port, or "" when there is none.
func renderServerBlock(conf string) string {
	i := strings.Index(conf, "listen 8081;")
	if i < 0 {
		return ""
	}
	return conf[i:]
}

var _ = Describe("RenderReconciler", func() {
	const namespace = "default"

	var (
		ctx context.Context
		rr  *RenderReconciler
	)

	BeforeEach(func() {
		ctx = testCtx
		rr = &RenderReconciler{Client: k8sClient, Scheme: testScheme}
	})

	newCR := func(name string, enabled *bool) *pulsev1alpha1.OpenShiftPulse {
		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: pulsev1alpha1.OpenShiftPulseSpec{
				Render: pulsev1alpha1.RenderConfig{Enabled: enabled},
			},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })
		return cr
	}

	It("creates nothing when render is not enabled", func() {
		cr := newCR("render-off", nil)
		Expect(rr.reconcileRender(ctx, cr)).To(Succeed())

		err := k8sClient.Get(ctx, types.NamespacedName{Name: renderResourceName(cr.Name), Namespace: namespace}, &appsv1.Deployment{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		err = k8sClient.Get(ctx, types.NamespacedName{Name: renderTokenSecretName(cr.Name), Namespace: namespace}, &corev1.Secret{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("leaves a user's own object with a render name alone while render is off", func() {
		cr := newCR("render-user-obj", nil)
		mine := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: renderTokenSecretName(cr.Name), Namespace: namespace},
			StringData: map[string]string{"k": "v"},
		}
		Expect(k8sClient.Create(ctx, mine)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, mine) })

		Expect(rr.reconcileRender(ctx, cr)).To(Succeed())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mine.Name, Namespace: namespace}, &corev1.Secret{})).To(Succeed(),
			"render removal must delete only what this CR created")
	})

	It("deploys a credential-free render pod that only the agent can reach", func() {
		on := true
		cr := newCR("render-on", &on)
		Expect(rr.reconcileRender(ctx, cr)).To(Succeed())
		key := types.NamespacedName{Name: renderResourceName(cr.Name), Namespace: namespace}

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: renderTokenSecretName(cr.Name), Namespace: namespace}, secret)).To(Succeed())
		token := string(secret.Data["token"])
		Expect(token).To(HaveLen(32))

		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, deploy)).To(Succeed())
		pod := deploy.Spec.Template.Spec
		Expect(pod.AutomountServiceAccountToken).NotTo(BeNil())
		Expect(*pod.AutomountServiceAccountToken).To(BeFalse(), "the render pod must hold no cluster credentials")
		c := pod.Containers[0]
		Expect(c.Image).To(Equal(defaultRenderImage))
		Expect(envValue(deploy, "UI_URL")).To(Equal("http://render-on-openshiftpulse:8081"))
		Expect(envValue(deploy, "RENDER_TOKEN")).To(Equal("<from-secret>"))

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		Expect(svc.Spec.Ports[0].Port).To(Equal(renderPort))

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, key, np)).To(Succeed())
		Expect(np.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))
		Expect(np.Spec.Ingress).To(HaveLen(1))
		Expect(np.Spec.Ingress[0].From[0].PodSelector.MatchLabels).To(Equal(map[string]string{"app": agentResourceName(cr.Name)}))
		// Egress: the UI render listener, and DNS. Nothing else.
		Expect(np.Spec.Egress).To(HaveLen(2))
		Expect(np.Spec.Egress[0].To[0].PodSelector.MatchLabels).To(Equal(map[string]string{"app": uiResourceName(cr.Name)}))
		Expect(np.Spec.Egress[0].Ports[0].Port.IntValue()).To(Equal(int(uiRenderPort)))
		for _, p := range np.Spec.Egress[1].Ports {
			Expect(p.Port.IntValue()).To(BeElementOf(53, 5353))
		}

		By("keeping the token across reconciles")
		Expect(rr.reconcileRender(ctx, cr)).To(Succeed())
		again := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: renderTokenSecretName(cr.Name), Namespace: namespace}, again)).To(Succeed())
		Expect(string(again.Data["token"])).To(Equal(token))

		By("removing everything when render is turned off")
		off := false
		cr.Spec.Render.Enabled = &off
		Expect(rr.reconcileRender(ctx, cr)).To(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &appsv1.Deployment{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &corev1.Service{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &networkingv1.NetworkPolicy{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: renderTokenSecretName(cr.Name), Namespace: namespace}, &corev1.Secret{}))).To(BeTrue())
	})

	It("gives the agent the render URL and token only when enabled", func() {
		ar := &AgentReconciler{Client: k8sClient, Scheme: testScheme}
		cr := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: "render-env", Namespace: namespace}}

		deploy := &appsv1.Deployment{Spec: ar.buildDeploymentSpec(cr, nil)}
		_, ok := envVar(deploy, "PULSE_AGENT_RENDER_URL")
		Expect(ok).To(BeFalse())

		on := true
		cr.Spec.Render.Enabled = &on
		deploy = &appsv1.Deployment{Spec: ar.buildDeploymentSpec(cr, nil)}
		Expect(envValue(deploy, "PULSE_AGENT_RENDER_URL")).To(Equal("http://render-env-render:8090"))
		Expect(envValue(deploy, "PULSE_AGENT_RENDER_TOKEN")).To(Equal("<from-secret>"))
	})
})

var _ = Describe("UI render listener", func() {
	const namespace = "default"

	var (
		ctx context.Context
		ui  *UIReconciler
	)

	BeforeEach(func() {
		ctx = testCtx
		ui = &UIReconciler{Client: k8sClient, Scheme: testScheme}
	})

	newCR := func(name string, enabled bool) *pulsev1alpha1.OpenShiftPulse {
		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       pulsev1alpha1.OpenShiftPulseSpec{Render: pulsev1alpha1.RenderConfig{Enabled: &enabled}},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, cr)
			_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: uiNginxSecretName(name), Namespace: namespace}})
		})
		return cr
	}

	It("has no render listener unless render is enabled", func() {
		conf := reconcileNginxAndRead(ctx, ui, newCR("listener-off", false))
		Expect(renderServerBlock(conf)).To(BeEmpty())
	})

	It("serves the user-token proxies and never the agent on the render listener", func() {
		conf := reconcileNginxAndRead(ctx, ui, newCR("listener-on", true))
		block := renderServerBlock(conf)
		Expect(block).NotTo(BeEmpty())
		Expect(block).To(ContainSubstring("location /api/kubernetes/"))
		Expect(block).To(ContainSubstring("location /api/prometheus/"))
		Expect(block).To(ContainSubstring("location /api/alertmanager/"))
		Expect(block).To(ContainSubstring("location /api/agent/ { return 404; }"))
		// The agent's credential and the trusted user header stay on the main listener.
		Expect(block).NotTo(ContainSubstring(agentResourceName("listener-on")))
		Expect(block).NotTo(ContainSubstring("X-Forwarded-User"))
		Expect(strings.Count(block, "Bearer $http_x_forwarded_access_token")).To(Equal(3))
	})

	It("lets only the render pod reach the render port", func() {
		r := &OpenShiftPulseReconciler{Client: k8sClient, Scheme: testScheme}
		cr := newCR("listener-np", true)
		Expect(r.reconcileUIPodNetworkPolicy(ctx, cr)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: uiResourceName(cr.Name), Namespace: namespace}})
		})
		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: uiResourceName(cr.Name), Namespace: namespace}, np)).To(Succeed())
		var renderRules []networkingv1.NetworkPolicyIngressRule
		for _, rule := range np.Spec.Ingress {
			for _, p := range rule.Ports {
				if p.Port.IntValue() == int(uiRenderPort) {
					renderRules = append(renderRules, rule)
				}
			}
		}
		Expect(renderRules).To(HaveLen(1))
		Expect(renderRules[0].From).To(HaveLen(1))
		Expect(renderRules[0].From[0].PodSelector.MatchLabels).To(Equal(map[string]string{"app": renderResourceName(cr.Name)}))
		Expect(renderRules[0].Ports).To(HaveLen(1), "the render pod must not be admitted to the main listener")
	})
})
