package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// Pure-function coverage for agentVersionCompatible: the inert-by-default
// case, the pass case, the fail case, and the fail-open behavior on a
// malformed version string (see compat.go's doc comment for why malformed
// input must never be treated the same as "incompatible").
var _ = Describe("agentVersionCompatible", func() {
	newCR := func(minOperatorVersion string) *pulsev1alpha1.OpenShiftPulse {
		return &pulsev1alpha1.OpenShiftPulse{
			Spec: pulsev1alpha1.OpenShiftPulseSpec{
				Agent: pulsev1alpha1.AgentConfig{MinOperatorVersion: minOperatorVersion},
			},
		}
	}

	It("is compatible (inert) when minOperatorVersion is unset — the default for every existing CR", func() {
		compatible, msg := agentVersionCompatible(newCR(""))
		Expect(compatible).To(BeTrue())
		Expect(msg).To(BeEmpty())
	})

	It("is compatible when minOperatorVersion is at or below this operator's own build version", func() {
		compatible, _ := agentVersionCompatible(newCR("0.1.0"))
		Expect(compatible).To(BeTrue())

		compatible, _ = agentVersionCompatible(newCR(OperatorVersion))
		Expect(compatible).To(BeTrue(), "exactly equal to this build's own version must be compatible")
	})

	It("is incompatible, with an explanatory message, when minOperatorVersion exceeds this operator's own build version", func() {
		compatible, msg := agentVersionCompatible(newCR("99.0.0"))
		Expect(compatible).To(BeFalse())
		Expect(msg).To(ContainSubstring("99.0.0"))
		Expect(msg).To(ContainSubstring(OperatorVersion))
	})

	It("fails open (compatible) on a malformed minOperatorVersion string rather than blocking a real deployment on a typo", func() {
		compatible, msg := agentVersionCompatible(newCR("not-a-semver"))
		Expect(compatible).To(BeTrue())
		Expect(msg).To(BeEmpty())
	})
})

var _ = Describe("setAgentVersionCompatibleCondition", func() {
	It("sets AgentVersionCompatible=True/Compatible when compatible", func() {
		cr := &pulsev1alpha1.OpenShiftPulse{}
		setAgentVersionCompatibleCondition(cr, true, "")
		cond := findCondition(cr.Status.Conditions, "AgentVersionCompatible")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("Compatible"))
	})

	It("sets AgentVersionCompatible=False/IncompatibleVersion with the explanatory message when incompatible", func() {
		cr := &pulsev1alpha1.OpenShiftPulse{}
		setAgentVersionCompatibleCondition(cr, false, "some explanatory message")
		cond := findCondition(cr.Status.Conditions, "AgentVersionCompatible")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("IncompatibleVersion"))
		Expect(cond.Message).To(Equal("some explanatory message"))
	})

	It("never touches Progressing/AgentReady/DatabaseReady/UIReady — additive only", func() {
		cr := &pulsev1alpha1.OpenShiftPulse{}
		setReadyCondition(cr, "AgentReady", true, "ready", "not ready")
		setAgentVersionCompatibleCondition(cr, false, "boom")
		Expect(findCondition(cr.Status.Conditions, "AgentReady").Status).To(Equal(metav1.ConditionTrue),
			"an unrelated condition must survive untouched")
	})
})

// Integration coverage for the actual gate in AgentReconciler.reconcileDeployment:
// pass-through when compatible/unset, and the two real behaviors when
// incompatible — pin the existing image on an update, and refuse to
// create on a brand-new install.
var _ = Describe("agent version compatibility gate (AgentReconciler.reconcileDeployment)", func() {
	const namespace = "default"

	It("does not block or pin anything when minOperatorVersion is unset (the default)", func() {
		ctx := testCtx
		const crName = "compat-inert-pulse"
		name := agentResourceName(crName)

		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec:       pulsev1alpha1.OpenShiftPulseSpec{Agent: pulsev1alpha1.AgentConfig{Image: "quay.io/test/pulse-agent:v2"}},
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

		ar := &AgentReconciler{Client: k8sClient, Scheme: testScheme}
		_, err := ar.reconcileDeployment(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		after := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, after)).To(Succeed())
		Expect(after.Spec.Template.Spec.Containers[0].Image).To(Equal("quay.io/test/pulse-agent:v2"),
			"the new image must be applied normally when no compatibility constraint is set")
		Expect(findCondition(cr.Status.Conditions, "AgentVersionCompatible")).To(BeNil(),
			"the condition must not even be added when the field is unset")
	})

	It("pins the existing image and emits IncompatibleVersion when minOperatorVersion exceeds this build, instead of applying the new one", func() {
		ctx := testCtx
		const crName = "compat-blocked-update-pulse"
		name := agentResourceName(crName)

		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec: pulsev1alpha1.OpenShiftPulseSpec{
				Agent: pulsev1alpha1.AgentConfig{Image: "quay.io/test/pulse-agent:v2", MinOperatorVersion: "99.0.0"},
			},
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

		rec := record.NewFakeRecorder(10)
		ar := &AgentReconciler{Client: k8sClient, Scheme: testScheme, Recorder: rec}
		_, err := ar.reconcileDeployment(ctx, cr)
		Expect(err).NotTo(HaveOccurred())

		after := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, after)).To(Succeed())
		Expect(after.Spec.Template.Spec.Containers[0].Image).To(Equal("quay.io/test/pulse-agent:v1"),
			"the incompatible new image must NOT be applied — the existing running image must be pinned")

		event := drainEvent(rec)
		Expect(event).To(ContainSubstring("IncompatibleVersion"))

		cond := findCondition(cr.Status.Conditions, "AgentVersionCompatible")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("IncompatibleVersion"))
	})

	It("refuses to create a brand-new Deployment when minOperatorVersion is incompatible, and requeues instead of erroring", func() {
		ctx := testCtx
		const crName = "compat-blocked-create-pulse"
		name := agentResourceName(crName)

		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec: pulsev1alpha1.OpenShiftPulseSpec{
				Agent: pulsev1alpha1.AgentConfig{Image: "quay.io/test/pulse-agent:v1", MinOperatorVersion: "99.0.0"},
			},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, cr) }()

		ar := &AgentReconciler{Client: k8sClient, Scheme: testScheme}
		result, err := ar.reconcileDeployment(ctx, cr)
		Expect(err).NotTo(HaveOccurred(), "a blocked-by-policy create is an expected outcome, not a reconcile failure")
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &appsv1.Deployment{})).NotTo(Succeed(),
			"the Deployment must not have been created at all")
	})
})
