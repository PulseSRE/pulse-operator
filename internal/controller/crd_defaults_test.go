package controller

// Regression coverage for CRD structural-schema defaulting when an entire
// object-typed spec field (spec.ui / spec.database / spec.monitoring) is
// omitted from the submitted CR. Kubernetes CRD defaulting only applies a
// field's default when that field is present in the submitted object; a
// child field's own `+kubebuilder:default` never causes the API server to
// synthesize its (missing) parent object. Without an object-level default on
// the parent field itself, submitting a CR with e.g. no `database:` key at
// all leaves `spec.database` as a Go zero-value struct — which looks
// identical to "explicitly set every child to its zero value" from the
// reconciler's point of view, but is NOT what `storageSize: 5Gi` etc. were
// meant to guarantee.
//
// These tests intentionally create the CR via an unstructured/raw object
// (not the typed pulsev1alpha1.OpenShiftPulse Go struct) — going through
// the typed client's JSON marshaling would always emit `"database":{}` for
// a struct-typed field regardless of `omitempty` (Go's encoding/json never
// treats a struct as "empty"), which would mask exactly the bug this test
// exists to catch. Only a request that truly omits the key from the wire
// payload (as `kubectl apply -f cr.yaml` does for an author who never wrote
// a `database:` section) reproduces the real-world scenario.
import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// createPulseWithOnlyAgent creates an OpenShiftPulse CR via an unstructured
// object whose spec contains only the required `agent` key — no `database`,
// `monitoring`, or `ui` key is present at all on the wire.
func createPulseWithOnlyAgent(ctx context.Context, name, namespace string) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "pulse.ai", Version: "v1alpha1", Kind: "OpenShiftPulse"})
	obj.SetName(name)
	obj.SetNamespace(namespace)
	Expect(unstructured.SetNestedMap(obj.Object, map[string]interface{}{
		"agent": map[string]interface{}{},
	}, "spec")).To(Succeed())
	ExpectWithOffset(1, k8sClient.Create(ctx, obj)).To(Succeed())
}

var _ = Describe("CRD object-level defaulting when a parent spec field is entirely omitted", func() {
	const namespace = "default"

	var ctx context.Context

	BeforeEach(func() {
		ctx = testCtx
	})

	Describe("spec.database omitted entirely", func() {
		// Name kept short: it gets suffixed with
		// "-openshift-sre-agent-postgresql-headless" for the PG headless
		// Service/StatefulSet.serviceName, which is capped at 63 chars total.
		const crName = "db-omit-pulse"

		AfterEach(func() {
			cr := &pulsev1alpha1.OpenShiftPulse{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: namespace}, cr); err == nil {
				controllerutilRemoveFinalizerAndDelete(ctx, cr)
			}
		})

		It("still defaults storageSize to 5Gi on the stored object", func() {
			createPulseWithOnlyAgent(ctx, crName, namespace)

			cr := &pulsev1alpha1.OpenShiftPulse{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: namespace}, cr)).To(Succeed())

			Expect(cr.Spec.Database.StorageSize).To(Equal("5Gi"),
				"spec.database.storageSize must default to 5Gi even when spec.database itself was never set")
		})

		It("still injects PULSE_AGENT_DATABASE_URL into the agent Deployment", func() {
			createPulseWithOnlyAgent(ctx, crName, namespace)

			root := &OpenShiftPulseReconciler{
				Client:   k8sClient,
				Scheme:   testScheme,
				Recorder: record.NewFakeRecorder(100),
				UIReconciler: &UIReconciler{
					Client: k8sClient,
					Scheme: testScheme,
				},
			}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace}}

			// First reconcile adds the finalizer; second does the real work.
			_, err := root.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			_, err = root.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      agentResourceName(crName),
				Namespace: namespace,
			}, deploy)).To(Succeed())

			var found bool
			for _, env := range deploy.Spec.Template.Spec.Containers[0].Env {
				if env.Name == "PULSE_AGENT_DATABASE_URL" {
					found = true
				}
			}
			Expect(found).To(BeTrue(),
				"PULSE_AGENT_DATABASE_URL must be injected — PostgreSQLReconciler provisions "+
					"PostgreSQL unconditionally, so the agent must always be told about it")
		})

		It("still applies storageSize: 5Gi to the PostgreSQL StatefulSet's PVC", func() {
			createPulseWithOnlyAgent(ctx, crName, namespace)

			pg := &PostgreSQLReconciler{Client: k8sClient, Scheme: testScheme}
			cr := &pulsev1alpha1.OpenShiftPulse{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: namespace}, cr)).To(Succeed())
			_, err := pg.reconcilePostgres(ctx, cr)
			Expect(err).NotTo(HaveOccurred())

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      crName + "-openshift-sre-agent-postgresql",
				Namespace: namespace,
			}, sts)).To(Succeed())

			Expect(sts.Spec.VolumeClaimTemplates).NotTo(BeEmpty())
			qty := sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests["storage"]
			Expect(qty.String()).To(Equal("5Gi"))
		})
	})

	Describe("spec.monitoring omitted entirely", func() {
		const crName = "mon-omit-pulse"

		AfterEach(func() {
			cr := &pulsev1alpha1.OpenShiftPulse{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: namespace}, cr); err == nil {
				controllerutilRemoveFinalizerAndDelete(ctx, cr)
			}
		})

		It("monitoringEnabled() still reports true (nil defaults to enabled)", func() {
			createPulseWithOnlyAgent(ctx, crName, namespace)

			cr := &pulsev1alpha1.OpenShiftPulse{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: namespace}, cr)).To(Succeed())

			Expect(monitoringEnabled(cr)).To(BeTrue(),
				"monitoring must stay enabled by default even when spec.monitoring was never set")
		})
	})

	Describe("spec.ui omitted entirely", func() {
		const crName = "ui-omit-pulse"

		AfterEach(func() {
			cr := &pulsev1alpha1.OpenShiftPulse{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: namespace}, cr); err == nil {
				controllerutilRemoveFinalizerAndDelete(ctx, cr)
			}
		})

		It("still defaults replicas to 2 on the stored object", func() {
			createPulseWithOnlyAgent(ctx, crName, namespace)

			cr := &pulsev1alpha1.OpenShiftPulse{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: namespace}, cr)).To(Succeed())

			Expect(cr.Spec.UI.Replicas).To(Equal(int32(2)),
				"spec.ui.replicas must default to 2 even when spec.ui itself was never set")
		})
	})
})
