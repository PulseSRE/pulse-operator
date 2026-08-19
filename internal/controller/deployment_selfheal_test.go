package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// Pure-function coverage for isDeploymentPodStale, mirroring
// postgresql_reconciler_test.go's isPGPodStale Describe block: pod structs
// are built directly (not created via the API server) so CreationTimestamp
// can be freely backdated, which envtest's real API server would never
// allow.
var _ = Describe("isDeploymentPodStale", func() {
	newPod := func(phase corev1.PodPhase, age time.Duration, now time.Time, waitingReason string) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(now.Add(-age))},
			Status:     corev1.PodStatus{Phase: phase},
		}
		if waitingReason != "" {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{
				{Name: "main", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitingReason}}},
			}
		}
		return pod
	}

	It("is false for a Pending pod that just started (normal scheduling/volume-attach/image-pull)", func() {
		now := time.Now()
		Expect(isDeploymentPodStale(newPod(corev1.PodPending, 5*time.Second, now, ""), now)).To(BeFalse())
	})

	It("is false for a Pending pod just under the stale threshold", func() {
		now := time.Now()
		Expect(isDeploymentPodStale(newPod(corev1.PodPending, pgPendingPodStaleThreshold-time.Second, now, ""), now)).To(BeFalse())
	})

	It("is true for a Pending pod at or beyond the stale threshold", func() {
		now := time.Now()
		Expect(isDeploymentPodStale(newPod(corev1.PodPending, pgPendingPodStaleThreshold, now, ""), now)).To(BeTrue())
	})

	It("is false for a Running pod with no waiting container", func() {
		now := time.Now()
		Expect(isDeploymentPodStale(newPod(corev1.PodRunning, time.Hour, now, ""), now)).To(BeFalse())
	})

	It("is true immediately (no grace period) for a container in ImagePullBackOff, even a freshly-created pod", func() {
		now := time.Now()
		pod := newPod(corev1.PodPending, 0, now, "ImagePullBackOff")
		Expect(isDeploymentPodStale(pod, now)).To(BeTrue())
	})

	It("is true immediately (no grace period) for a container in CrashLoopBackOff", func() {
		now := time.Now()
		pod := newPod(corev1.PodRunning, 0, now, "CrashLoopBackOff")
		Expect(isDeploymentPodStale(pod, now)).To(BeTrue())
	})

	It("is false for a container waiting on an ordinary, non-stale reason (e.g. ContainerCreating)", func() {
		now := time.Now()
		pod := newPod(corev1.PodPending, 0, now, "ContainerCreating")
		Expect(isDeploymentPodStale(pod, now)).To(BeFalse())
	})
})

// Integration coverage: the actual reconcile call sites (AgentReconciler.
// reconcileDeployment, UIReconciler.reconcileUIDeployment) must delete a
// stale pod and emit a SelfHealed event via deleteStalePodsForDeployment.
// Uses the ImagePullBackOff/CrashLoopBackOff path (not Pending-too-long) —
// same reasoning as autonomy_test.go's self-heal Events Describe block:
// this is the one staleness path that doesn't depend on a real wall-clock
// CreationTimestamp envtest's API server won't let a test backdate.
var _ = Describe("Deployment self-heal (agent + UI)", func() {
	const namespace = "default"

	It("AgentReconciler.reconcileDeployment deletes an ImagePullBackOff agent pod and emits SelfHealed", func() {
		ctx := testCtx
		const crName = "selfheal-agent-pulse"
		name := agentResourceName(crName)

		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec:       pulsev1alpha1.OpenShiftPulseSpec{Agent: pulsev1alpha1.AgentConfig{Image: "quay.io/test/pulse-agent:v1"}},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, cr) }()

		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "quay.io/test/pulse-agent:v1"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deploy)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, deploy) }()

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-badpull", Namespace: namespace, Labels: map[string]string{"app": name}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "quay.io/test/does-not-exist:v1"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status = corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}},
			},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		rec := record.NewFakeRecorder(10)
		ar := &AgentReconciler{Client: k8sClient, Scheme: testScheme, Recorder: rec}

		_, err := ar.reconcileDeployment(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: namespace}, &corev1.Pod{}))).To(BeTrue(),
			"the ImagePullBackOff pod must actually have been deleted")

		event := drainEvent(rec)
		Expect(event).To(ContainSubstring("SelfHealed"))
		Expect(event).To(ContainSubstring("ImagePullBackOff"))
	})

	It("UIReconciler.reconcileUIDeployment deletes a CrashLoopBackOff UI pod and emits SelfHealed", func() {
		ctx := testCtx
		const crName = "selfheal-ui-pulse"
		name := uiResourceName(crName)

		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, cr) }()

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-crashy", Namespace: namespace, Labels: map[string]string{"app": name}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "openshiftpulse", Image: "quay.io/test/openshiftpulse:v1"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status = corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "openshiftpulse", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
			},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		rec := record.NewFakeRecorder(10)
		ui := &UIReconciler{Client: k8sClient, Scheme: testScheme, Recorder: rec}
		cr.Spec.UI.Replicas = 1

		Expect(ui.reconcileUIDeployment(ctx, cr, &ClusterInfo{}, "testhash")).To(Succeed())

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: namespace}, &corev1.Pod{}))).To(BeTrue(),
			"the CrashLoopBackOff pod must actually have been deleted")

		event := drainEvent(rec)
		Expect(event).To(ContainSubstring("SelfHealed"))
		Expect(event).To(ContainSubstring("CrashLoopBackOff"))

		_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}})
	})

	It("a nil Recorder does not panic when a Deployment self-heal fires (agent)", func() {
		ctx := testCtx
		const crName = "selfheal-agent-nilrec-pulse"
		name := agentResourceName(crName)

		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec:       pulsev1alpha1.OpenShiftPulseSpec{Agent: pulsev1alpha1.AgentConfig{Image: "quay.io/test/pulse-agent:v1"}},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, cr) }()

		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "quay.io/test/pulse-agent:v1"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, deploy)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, deploy) }()

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-badpull2", Namespace: namespace, Labels: map[string]string{"app": name}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "quay.io/test/does-not-exist:v1"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status = corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}},
			},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		ar := &AgentReconciler{Client: k8sClient, Scheme: testScheme} // Recorder deliberately left nil
		Expect(func() { _, _ = ar.reconcileDeployment(ctx, cr) }).NotTo(Panic())
	})
})
