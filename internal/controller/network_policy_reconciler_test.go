package controller

// NetworkPolicyReconciler had zero test coverage before this file — flagged in
// AUDIT.md and still true as of the independent review that added this file.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

var _ = Describe("NetworkPolicyReconciler", func() {
	const (
		crName    = "netpol-pulse"
		namespace = "default"
	)

	var (
		ctx       context.Context
		cr        *pulsev1alpha1.OpenShiftPulse
		rootRecon *OpenShiftPulseReconciler
	)

	BeforeEach(func() {
		ctx = testCtx
		rootRecon = &OpenShiftPulseReconciler{Client: k8sClient, Scheme: testScheme}
		cr = &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec:       pulsev1alpha1.OpenShiftPulseSpec{},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
	})

	AfterEach(func() { _ = k8sClient.Delete(ctx, cr) })

	It("creates a NetworkPolicy selecting the UI pods", func() {
		Expect(rootRecon.reconcileNetworkPolicies(ctx, cr)).To(Succeed())

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      crName + "-openshiftpulse",
			Namespace: namespace,
		}, np)).To(Succeed())

		Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("app", crName+"-openshiftpulse"))
		Expect(np.Spec.PolicyTypes).To(ContainElements(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))
	})

	It("UI NetworkPolicy allows ingress from the OCP router ingress namespace on 8443", func() {
		Expect(rootRecon.reconcileNetworkPolicies(ctx, cr)).To(Succeed())

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      crName + "-openshiftpulse",
			Namespace: namespace,
		}, np)).To(Succeed())

		var sawIngressNamespaceRule bool
		for _, rule := range np.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.NamespaceSelector != nil &&
					peer.NamespaceSelector.MatchLabels["network.openshift.io/policy-group"] == "ingress" {
					sawIngressNamespaceRule = true
				}
			}
		}
		Expect(sawIngressNamespaceRule).To(BeTrue(), "must allow the OCP router ingress namespace")
	})

	It("UI NetworkPolicy does not have an empty (match-all) PodSelector in any ingress rule", func() {
		// Regression test: an empty PodSelector{} matches every pod in the
		// namespace, not a scoped subset — this used to broaden the "protect
		// the UI pod" NetworkPolicy far past its intent.
		Expect(rootRecon.reconcileNetworkPolicies(ctx, cr)).To(Succeed())

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      crName + "-openshiftpulse",
			Namespace: namespace,
		}, np)).To(Succeed())

		for _, rule := range np.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.PodSelector != nil {
					Expect(peer.PodSelector.MatchLabels).NotTo(BeEmpty(),
						"an empty PodSelector matches ALL pods in the namespace, not a scoped subset")
				}
			}
		}
	})

	It("creates a NetworkPolicy restricting PostgreSQL ingress to the agent pods only", func() {
		Expect(rootRecon.reconcileNetworkPolicies(ctx, cr)).To(Succeed())

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      crName + "-pg-access",
			Namespace: namespace,
		}, np)).To(Succeed())

		Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("app", crName+"-openshift-sre-agent-postgresql"))
		Expect(np.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress))
		Expect(np.Spec.Ingress).To(HaveLen(1))
		Expect(np.Spec.Ingress[0].From).To(HaveLen(1))
		Expect(np.Spec.Ingress[0].From[0].PodSelector.MatchLabels).To(
			HaveKeyWithValue("app", crName+"-openshift-sre-agent"))
	})

	It("PostgreSQL NetworkPolicy does not open any port to pods other than the agent", func() {
		Expect(rootRecon.reconcileNetworkPolicies(ctx, cr)).To(Succeed())

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      crName + "-pg-access",
			Namespace: namespace,
		}, np)).To(Succeed())

		for _, rule := range np.Spec.Ingress {
			for _, peer := range rule.From {
				Expect(peer.PodSelector).NotTo(BeNil(), "every peer must be pod-scoped, not an open namespace rule")
				Expect(peer.PodSelector.MatchLabels).NotTo(BeEmpty(), "PodSelector must not be empty (empty selector matches ALL pods)")
			}
		}
	})

	It("both NetworkPolicies have an OwnerReference to the CR", func() {
		Expect(rootRecon.reconcileNetworkPolicies(ctx, cr)).To(Succeed())

		for _, name := range []string{crName + "-openshiftpulse", crName + "-pg-access"} {
			np := &networkingv1.NetworkPolicy{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, np)).To(Succeed())
			Expect(np.GetOwnerReferences()).NotTo(BeEmpty(), "%s must be owned by the CR", name)
		}
	})

	It("reconcileNetworkPolicies is idempotent", func() {
		Expect(rootRecon.reconcileNetworkPolicies(ctx, cr)).To(Succeed())
		Expect(rootRecon.reconcileNetworkPolicies(ctx, cr)).To(Succeed())
	})

	It("creates a NetworkPolicy restricting agent ingress to the UI pod and monitoring only", func() {
		// Regression test: the agent pod previously had no NetworkPolicy at all —
		// any pod in the namespace (or with network access to the pod IP) could
		// reach its WS/REST API on 8080, including the shared WS auth token
		// embedded in the UI's nginx ConfigMap.
		Expect(rootRecon.reconcileNetworkPolicies(ctx, cr)).To(Succeed())

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      crName + "-agent-access",
			Namespace: namespace,
		}, np)).To(Succeed())

		Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("app", crName+"-openshift-sre-agent"))
		Expect(np.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress))

		var sawUIPeer, sawMonitoringPeer bool
		for _, rule := range np.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.PodSelector != nil && peer.PodSelector.MatchLabels["app"] == crName+"-openshiftpulse" {
					sawUIPeer = true
				}
				if peer.NamespaceSelector != nil &&
					peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "openshift-user-workload-monitoring" {
					sawMonitoringPeer = true
				}
			}
		}
		Expect(sawUIPeer).To(BeTrue(), "must allow ingress from the UI pod")
		Expect(sawMonitoringPeer).To(BeTrue(), "must allow ingress from user-workload-monitoring for ServiceMonitor scrape")
	})

	It("agent NetworkPolicy does not open port 8080 to any pod other than the UI", func() {
		Expect(rootRecon.reconcileNetworkPolicies(ctx, cr)).To(Succeed())

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      crName + "-agent-access",
			Namespace: namespace,
		}, np)).To(Succeed())

		for _, rule := range np.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.PodSelector != nil {
					Expect(peer.PodSelector.MatchLabels).To(HaveKeyWithValue("app", crName+"-openshiftpulse"),
						"only the UI pod may be a pod-scoped ingress peer for the agent")
				}
			}
		}
	})

	It("agent NetworkPolicy has an OwnerReference to the CR", func() {
		Expect(rootRecon.reconcileNetworkPolicies(ctx, cr)).To(Succeed())

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      crName + "-agent-access",
			Namespace: namespace,
		}, np)).To(Succeed())
		Expect(np.GetOwnerReferences()).NotTo(BeEmpty())
	})
})
