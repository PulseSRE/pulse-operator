package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// podMetricsFixture builds an unstructured object shaped like a real
// metrics.k8s.io/v1beta1 PodMetrics item, so podMemoryBytes/
// maxPodMemoryBytes can be tested against realistic data without a live
// metrics-server (envtest does not run one — see resource_metrics.go's
// doc comment).
func podMetricsFixture(containerMemory ...string) unstructured.Unstructured {
	containers := make([]interface{}, 0, len(containerMemory))
	for _, mem := range containerMemory {
		usage := map[string]interface{}{"cpu": "10m"}
		if mem != "" {
			usage["memory"] = mem
		}
		containers = append(containers, map[string]interface{}{"name": "container", "usage": usage})
	}
	return unstructured.Unstructured{Object: map[string]interface{}{
		"containers": containers,
	}}
}

var _ = Describe("podMemoryBytes / maxPodMemoryBytes", func() {
	It("sums memory across all containers in one pod", func() {
		item := podMetricsFixture("100Mi", "28Mi")
		bytes, ok := podMemoryBytes(item)
		Expect(ok).To(BeTrue())
		// 100Mi + 28Mi = 128Mi = 134217728 bytes
		Expect(bytes).To(Equal(int64(134217728)))
	})

	It("reports not-ok when a PodMetrics item has no containers field", func() {
		item := unstructured.Unstructured{Object: map[string]interface{}{}}
		_, ok := podMemoryBytes(item)
		Expect(ok).To(BeFalse())
	})

	It("skips containers with an unparseable or missing usage.memory rather than erroring", func() {
		item := podMetricsFixture("", "50Mi")
		bytes, ok := podMemoryBytes(item)
		Expect(ok).To(BeTrue(), "the one parseable container is still counted")
		Expect(bytes).To(Equal(int64(52428800))) // 50Mi
	})

	It("maxPodMemoryBytes takes the highest per-pod total, not the sum or average across pods", func() {
		items := []unstructured.Unstructured{
			podMetricsFixture("50Mi"),
			podMetricsFixture("200Mi"),
			podMetricsFixture("10Mi"),
		}
		maxBytes, any := maxPodMemoryBytes(items)
		Expect(any).To(BeTrue())
		Expect(maxBytes).To(Equal(int64(209715200))) // 200Mi, not the sum (260Mi) or average
	})

	It("maxPodMemoryBytes reports not-any when there are no items or none have parseable usage", func() {
		maxBytes, any := maxPodMemoryBytes(nil)
		Expect(any).To(BeFalse())
		Expect(maxBytes).To(BeZero())

		maxBytes, any = maxPodMemoryBytes([]unstructured.Unstructured{{Object: map[string]interface{}{}}})
		Expect(any).To(BeFalse())
		Expect(maxBytes).To(BeZero())
	})
})

var _ = Describe("dueForObservedMemoryCheck", func() {
	BeforeEach(func() {
		resetObservedMemoryCheckCache()
	})

	It("is due the first time a key is checked", func() {
		Expect(dueForObservedMemoryCheck("ns/name/agent", time.Now())).To(BeTrue())
	})

	It("is not due again immediately after — avoids hammering the metrics API every reconcile", func() {
		now := time.Now()
		Expect(dueForObservedMemoryCheck("ns/name/agent", now)).To(BeTrue())
		Expect(dueForObservedMemoryCheck("ns/name/agent", now.Add(time.Second))).To(BeFalse())
	})

	It("is due again once observedMemoryCheckInterval has elapsed", func() {
		now := time.Now()
		Expect(dueForObservedMemoryCheck("ns/name/agent", now)).To(BeTrue())
		Expect(dueForObservedMemoryCheck("ns/name/agent", now.Add(observedMemoryCheckInterval+time.Second))).To(BeTrue())
	})

	It("tracks each key independently", func() {
		now := time.Now()
		Expect(dueForObservedMemoryCheck("ns/name/agent", now)).To(BeTrue())
		Expect(dueForObservedMemoryCheck("ns/name/ui", now)).To(BeTrue(), "a different component key must not be throttled by the first")
	})
})

// Integration coverage: reconcileObservedMemoryMetrics must never error or
// panic when metrics.k8s.io isn't served (envtest's real API server has no
// metrics-server aggregated API registered) — it must gracefully skip
// publishing an observed sample, exactly like DetectClusterInfo's ACM/
// oauth-proxy checks handle an absent, optional cluster capability.
var _ = Describe("reconcileObservedMemoryMetrics", func() {
	const namespace = "default"

	BeforeEach(func() {
		resetObservedMemoryCheckCache()
	})

	It("does not panic or error when metrics.k8s.io is unavailable, and still sets the cheap requested-memory gauge", func() {
		ctx := testCtx
		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: "obsmem-pulse", Namespace: namespace},
		}
		Expect(func() {
			reconcileObservedMemoryMetrics(ctx, k8sClient, cr, agentResourceName(cr.Name), "agent", agentRequestedMemoryBytes(cr))
		}).NotTo(Panic())

		// requestedMemoryBytes needs no cluster API call, so it must always
		// be set regardless of whether metrics.k8s.io is reachable.
		metric, err := requestedMemoryBytes.GetMetricWithLabelValues(namespace, cr.Name, "agent")
		Expect(err).NotTo(HaveOccurred())
		Expect(testutil.ToFloat64(metric)).To(BeNumerically(">", 0))
	})

	It("agentRequestedMemoryBytes/uiRequestedMemoryBytes reflect spec overrides, and the built-in defaults when unset", func() {
		defaultCR := &pulsev1alpha1.OpenShiftPulse{}
		Expect(agentRequestedMemoryBytes(defaultCR)).To(BeNumerically(">", 0))
		Expect(uiRequestedMemoryBytes(defaultCR)).To(BeNumerically(">", 0))

		overrideCR := &pulsev1alpha1.OpenShiftPulse{
			Spec: pulsev1alpha1.OpenShiftPulseSpec{
				Agent: pulsev1alpha1.AgentConfig{Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("321Mi")},
				}},
			},
		}
		Expect(agentRequestedMemoryBytes(overrideCR)).To(Equal(int64(336592896))) // 321Mi
	})
})
