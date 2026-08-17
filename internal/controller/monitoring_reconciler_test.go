package controller

// MonitoringReconciler had zero test coverage before this file — flagged in
// AUDIT.md and still true as of the independent review that added this file.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

func boolPtr(b bool) *bool { return &b }

var _ = Describe("MonitoringReconciler", func() {
	const namespace = "default"

	var (
		ctx context.Context
		mr  *MonitoringReconciler
	)

	BeforeEach(func() {
		ctx = testCtx
		mr = &MonitoringReconciler{Client: k8sClient, Scheme: testScheme}
	})

	Describe("monitoring disabled", func() {
		const crName = "mon-disabled-pulse"
		var cr *pulsev1alpha1.OpenShiftPulse

		BeforeEach(func() {
			cr = &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					Monitoring: pulsev1alpha1.MonitoringConfig{Enabled: boolPtr(false)},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		})

		AfterEach(func() { _ = k8sClient.Delete(ctx, cr) })

		It("Enabled=false survives the API round-trip (regression: bool+omitempty+CRD-default used to silently flip this back to true)", func() {
			Expect(cr.Spec.Monitoring.Enabled).NotTo(BeNil())
			Expect(*cr.Spec.Monitoring.Enabled).To(BeFalse())
		})

		It("creates neither ServiceMonitor nor PrometheusRule", func() {
			Expect(mr.reconcileMonitoring(ctx, cr)).To(Succeed())

			sm := &unstructured.Unstructured{}
			sm.SetGroupVersionKind(serviceMonitorGVK)
			err := k8sClient.Get(ctx, types.NamespacedName{Name: agentResourceName(crName), Namespace: namespace}, sm)
			Expect(err).To(HaveOccurred(), "ServiceMonitor must NOT exist when monitoring is disabled")

			pr := &unstructured.Unstructured{}
			pr.SetGroupVersionKind(prometheusRuleGVK)
			err = k8sClient.Get(ctx, types.NamespacedName{Name: prometheusRuleName(crName), Namespace: namespace}, pr)
			Expect(err).To(HaveOccurred(), "PrometheusRule must NOT exist when monitoring is disabled")
		})
	})

	Describe("monitoring enabled", func() {
		const crName = "mon-enabled-pulse"
		var cr *pulsev1alpha1.OpenShiftPulse

		BeforeEach(func() {
			cr = &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					Monitoring: pulsev1alpha1.MonitoringConfig{Enabled: boolPtr(true)},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		})

		AfterEach(func() { _ = k8sClient.Delete(ctx, cr) })

		It("creates a ServiceMonitor scraping the agent's /metrics endpoint", func() {
			Expect(mr.reconcileMonitoring(ctx, cr)).To(Succeed())

			sm := &unstructured.Unstructured{}
			sm.SetGroupVersionKind(serviceMonitorGVK)
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      agentResourceName(crName),
				Namespace: namespace,
			}, sm)).To(Succeed())

			endpoints, found, err := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(endpoints).To(HaveLen(1))
			ep := endpoints[0].(map[string]interface{})
			// Regression: the agent's uvicorn server 307-redirects "/metrics" to
			// "/metrics/", and Prometheus does not follow redirects when scraping,
			// so a bare "/metrics" path here silently produces zero samples forever.
			Expect(ep["path"]).To(Equal("/metrics/"))

			selector, _, _ := unstructured.NestedStringMap(sm.Object, "spec", "selector", "matchLabels")
			Expect(selector).To(HaveKeyWithValue("app", agentResourceName(crName)))
		})

		It("ServiceMonitor has an OwnerReference to the CR (garbage-collected on delete)", func() {
			Expect(mr.reconcileMonitoring(ctx, cr)).To(Succeed())

			sm := &unstructured.Unstructured{}
			sm.SetGroupVersionKind(serviceMonitorGVK)
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      agentResourceName(crName),
				Namespace: namespace,
			}, sm)).To(Succeed())

			owners := sm.GetOwnerReferences()
			Expect(owners).NotTo(BeEmpty())
			Expect(owners[0].Name).To(Equal(crName))
			Expect(owners[0].Kind).To(Equal("OpenShiftPulse"))
		})

		It("creates a PrometheusRule with PulseAgentDown, PulseAgentHighRestarts, and PulsePostgreSQLDown alerts", func() {
			Expect(mr.reconcileMonitoring(ctx, cr)).To(Succeed())

			pr := &unstructured.Unstructured{}
			pr.SetGroupVersionKind(prometheusRuleGVK)
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      prometheusRuleName(crName),
				Namespace: namespace,
			}, pr)).To(Succeed())

			groups, found, err := unstructured.NestedSlice(pr.Object, "spec", "groups")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(groups).To(HaveLen(1))

			group := groups[0].(map[string]interface{})
			rules := group["rules"].([]interface{})
			Expect(rules).To(HaveLen(3))

			var alertNames []string
			for _, r := range rules {
				rule := r.(map[string]interface{})
				alertNames = append(alertNames, rule["alert"].(string))
			}
			Expect(alertNames).To(ContainElements("PulseAgentDown", "PulseAgentHighRestarts", "PulsePostgreSQLDown"))
		})

		It("PulseAgentHighRestarts PromQL matches the actual agent container name", func() {
			// Regression test for REVIEW.md's finding: the alert previously used
			// container="openshift-sre-agent" but the container is actually named "agent"
			// (agent_reconciler.go buildDeploymentSpec), so the alert never fired.
			Expect(mr.reconcileMonitoring(ctx, cr)).To(Succeed())

			pr := &unstructured.Unstructured{}
			pr.SetGroupVersionKind(prometheusRuleGVK)
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      prometheusRuleName(crName),
				Namespace: namespace,
			}, pr)).To(Succeed())

			groups, _, _ := unstructured.NestedSlice(pr.Object, "spec", "groups")
			group := groups[0].(map[string]interface{})
			rules := group["rules"].([]interface{})

			var found bool
			for _, r := range rules {
				rule := r.(map[string]interface{})
				if rule["alert"] == "PulseAgentHighRestarts" {
					found = true
					Expect(rule["expr"]).To(ContainSubstring(`container="agent"`),
						"PromQL must reference the real container name, not container=\"openshift-sre-agent\"")
				}
			}
			Expect(found).To(BeTrue(), "PulseAgentHighRestarts rule must exist")
		})

		It("reconcileMonitoring is idempotent", func() {
			Expect(mr.reconcileMonitoring(ctx, cr)).To(Succeed())
			Expect(mr.reconcileMonitoring(ctx, cr)).To(Succeed())

			sm := &unstructured.Unstructured{}
			sm.SetGroupVersionKind(serviceMonitorGVK)
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      agentResourceName(crName),
				Namespace: namespace,
			}, sm)).To(Succeed())
		})
	})
})
