package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

var _ = Describe("deleteClusterScopedResources finalizer", func() {
	const (
		crName    = "finalizer-test-pulse"
		namespace = "default"
	)

	var (
		ctx          context.Context
		cr           *pulsev1alpha1.OpenShiftPulse
		rootReconciler *OpenShiftPulseReconciler
	)

	BeforeEach(func() {
		ctx = testCtx

		rootReconciler = &OpenShiftPulseReconciler{
			Client:   k8sClient,
			Scheme:   testScheme,
			Recorder: record.NewFakeRecorder(10),
		}

		cr = &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec:       pulsev1alpha1.OpenShiftPulseSpec{},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
	})

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, cr)
	})

	It("ignores NotFound for RBAC resources; OAuthClient may fail in envtest (no oauth API)", func() {
		// ClusterRole/ClusterRoleBinding deletions return NotFound (ignored).
		// envtest does not register oauth.openshift.io, so the OAuthClient deletion
		// returns NoKindMatchError — which the production code does NOT suppress.
		// We therefore accept either nil OR an error that mentions only OAuthClient.
		err := rootReconciler.deleteClusterScopedResources(ctx, cr)
		if err != nil {
			Expect(err.Error()).To(ContainSubstring("OAuthClient"),
				"any error must be about the unregistered OAuthClient API, not RBAC")
		}
	})
})
