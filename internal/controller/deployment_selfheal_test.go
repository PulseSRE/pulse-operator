package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// The agent and UI used to delete any pod Pending for 2 minutes or in
// ImagePullBackOff/CrashLoopBackOff on every reconcile. These tests pin the
// replacement: the pod is diagnosed and reported, and left alone.
var _ = Describe("diagnosePod", func() {
	waiting := func(reason, message string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p"},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}}},
				},
			},
		}
	}

	It("treats a pod still pulling its image as progress, however long it has been Pending", func() {
		pod := waiting("ContainerCreating", "")
		pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
		Expect(diagnosePod(pod)).To(BeNil())
	})

	It("reports ImagePullBackOff with the kubelet's message", func() {
		p := diagnosePod(waiting("ImagePullBackOff", `Back-off pulling image "quay.io/x/agent:typo"`))
		Expect(p).NotTo(BeNil())
		Expect(p.Reason).To(Equal("ImagePullBackOff"))
		Expect(p.String()).To(ContainSubstring("quay.io/x/agent:typo"))
	})

	It("reports a crashloop with the exit code of the last crash, not the back-off text", func() {
		pod := waiting("CrashLoopBackOff", "back-off 5m0s restarting failed container")
		pod.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{
			ExitCode: 1, Reason: "Error", Message: "migration 041 failed",
		}
		p := diagnosePod(pod)
		Expect(p).NotTo(BeNil())
		Expect(p.Reason).To(Equal("CrashLoopBackOff"))
		Expect(p.Message).To(Equal("container agent exited with code 1 (Error): migration 041 failed"))
	})

	It("reports an unschedulable pod with the scheduler's reason", func() {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p"},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{{
					Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
					Message: "0/3 nodes are available: 3 Insufficient memory.",
				}},
			},
		}
		p := diagnosePod(pod)
		Expect(p).NotTo(BeNil())
		Expect(p.Reason).To(Equal("Unschedulable"))
		Expect(p.Message).To(ContainSubstring("Insufficient memory"))
	})

	It("reports a failing init container", func() {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p"},
			Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "init", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: `secret "x" not found`}}},
			}},
		}
		Expect(diagnosePod(pod).Reason).To(Equal("CreateContainerConfigError"))
	})

	It("is nil for a running pod", func() {
		Expect(diagnosePod(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}})).To(BeNil())
	})
})

var _ = Describe("Stuck agent and UI pods are reported, not deleted", func() {
	const namespace = "default"

	stuckPod := func(name, app, container, reason string) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app": app}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: container, Image: "quay.io/test/img:v1"}}},
		}
		Expect(k8sClient.Create(testCtx, pod)).To(Succeed())
		pod.Status = corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: container, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "detail from the kubelet"}}},
			},
		}
		Expect(k8sClient.Status().Update(testCtx, pod)).To(Succeed())
		return pod
	}

	It("AgentReconciler.reconcileDeployment leaves an ImagePullBackOff agent pod in place", func() {
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
		pod := stuckPod(name+"-badpull", name, "agent", "ImagePullBackOff")
		defer func() { _ = k8sClient.Delete(ctx, pod) }()

		ar := &AgentReconciler{Client: k8sClient, Scheme: testScheme, Recorder: record.NewFakeRecorder(10)}
		_, err := ar.reconcileDeployment(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: namespace}, &corev1.Pod{})).To(Succeed(),
			"deleting the pod cannot fix a bad image; it only restarted the pull every reconcile")
	})

	It("UIReconciler.reconcileUIDeployment leaves a CrashLoopBackOff UI pod in place, keeping its previous logs", func() {
		ctx := testCtx
		const crName = "selfheal-ui-pulse"
		name := uiResourceName(crName)
		cr := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace}}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, cr) }()
		pod := stuckPod(name+"-crashy", name, "openshiftpulse", "CrashLoopBackOff")
		defer func() { _ = k8sClient.Delete(ctx, pod) }()

		ui := &UIReconciler{Client: k8sClient, Scheme: testScheme, Recorder: record.NewFakeRecorder(10)}
		cr.Spec.UI.Replicas = 1
		Expect(ui.reconcileUIDeployment(ctx, cr, &ClusterInfo{}, "testhash")).To(Succeed())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: namespace}, &corev1.Pod{})).To(Succeed())
		_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}})
	})

	It("reportPodProblems puts the pod's reason in the not-ready condition and emits PodNotHealthy", func() {
		ctx := testCtx
		const crName = "diagnose-agent-pulse"
		name := agentResourceName(crName)
		pulse := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace}}
		pod := stuckPod(name+"-crashy", name, "agent", "CrashLoopBackOff")
		defer func() { _ = k8sClient.Delete(ctx, pod) }()

		rec := record.NewFakeRecorder(10)
		r := &OpenShiftPulseReconciler{Client: k8sClient, Scheme: testScheme, Recorder: rec}
		pulse.Status.UIAvailable = true
		r.reportPodProblems(ctx, pulse, false)

		cond := apimeta.FindStatusCondition(pulse.Status.Conditions, "AgentReady")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("CrashLoopBackOff"))
		Expect(cond.Message).To(ContainSubstring(pod.Name))
		event := drainEvent(rec)
		Expect(event).To(ContainSubstring("PodNotHealthy"))
		Expect(event).To(ContainSubstring("CrashLoopBackOff"))
		Expect(apimeta.FindStatusCondition(pulse.Status.Conditions, "UIReady")).To(BeNil(), "a ready UI is not diagnosed")
	})

	It("a nil Recorder does not panic when a problem is reported", func() {
		ctx := testCtx
		const crName = "diagnose-nilrec-pulse"
		name := agentResourceName(crName)
		pulse := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace}}
		pod := stuckPod(name+"-badpull", name, "agent", "ImagePullBackOff")
		defer func() { _ = k8sClient.Delete(ctx, pod) }()
		r := &OpenShiftPulseReconciler{Client: k8sClient, Scheme: testScheme}
		Expect(func() { r.reportPodProblems(ctx, pulse, false) }).NotTo(Panic())
	})
})
