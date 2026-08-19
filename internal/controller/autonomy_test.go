package controller

// Tests for the operator-autonomy work: per-component status conditions and
// a distinct Upgrading phase (syncPhaseAndConditions), self-heal Events
// (recordEvent / the Recorder field now on every sub-reconciler), and
// automatic rollback of a stuck agent/UI image upgrade
// (reconcileAutoRollback). See status.go and events.go.

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// drainEvent reads one event string off a FakeRecorder's channel within a
// short timeout, or fails the test — used instead of a bare `<-rec.Events`
// so a missing event fails fast with a clear message instead of hanging.
func drainEvent(rec *record.FakeRecorder) string {
	select {
	case e := <-rec.Events:
		return e
	case <-time.After(time.Second):
		Fail("expected an event to be recorded, but none arrived within 1s")
		return ""
	}
}

// expectNoEvent fails the test if an event arrives within a short window —
// used to assert a self-heal/rollback action did NOT fire again (the
// anti-loop guards).
func expectNoEvent(rec *record.FakeRecorder) {
	select {
	case e := <-rec.Events:
		Fail("expected no event, but got: " + e)
	case <-time.After(200 * time.Millisecond):
	}
}

var _ = Describe("syncPhaseAndConditions", func() {
	const (
		crName    = "autonomy-phase-pulse"
		namespace = "default"
	)

	var (
		cr   *pulsev1alpha1.OpenShiftPulse
		root *OpenShiftPulseReconciler
	)

	BeforeEach(func() {
		root = &OpenShiftPulseReconciler{Client: k8sClient, Scheme: testScheme, Recorder: record.NewFakeRecorder(20)}
		cr = &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec: pulsev1alpha1.OpenShiftPulseSpec{
				Agent: pulsev1alpha1.AgentConfig{Image: "quay.io/test/pulse-agent:v1"},
				UI:    pulsev1alpha1.UIConfig{Image: "quay.io/test/openshiftpulse:v1"},
			},
		}
	})

	It("first install with nothing ready yet is Installing, not Upgrading", func() {
		_, _, agentUpgrading, uiUpgrading := root.syncPhaseAndConditions(cr, false, false)
		Expect(cr.Status.Phase).To(Equal("Installing"))
		Expect(agentUpgrading).To(BeFalse(), "no lastHealthy image recorded yet — nothing to be 'upgrading' from")
		Expect(uiUpgrading).To(BeFalse())
		Expect(cr.Status.LastHealthyAgentImage).To(BeEmpty())
	})

	It("all components ready -> Running, records lastHealthy images, Progressing=False/Stable", func() {
		cr.Status.UIAvailable = true // UIAvailable is read from status by syncPhaseAndConditions's caller convention
		root.syncPhaseAndConditions(cr, true, true)

		Expect(cr.Status.Phase).To(Equal("Running"))
		Expect(cr.Status.LastHealthyAgentImage).To(Equal("quay.io/test/pulse-agent:v1"))
		Expect(cr.Status.LastHealthyUIImage).To(Equal("quay.io/test/openshiftpulse:v1"))
		Expect(cr.Status.UpgradeStartedAt).To(BeNil())

		progressing := findCondition(cr.Status.Conditions, "Progressing")
		Expect(progressing).NotTo(BeNil())
		Expect(progressing.Status).To(Equal(metav1.ConditionFalse))
		Expect(progressing.Reason).To(Equal("Stable"))

		agentReady := findCondition(cr.Status.Conditions, "AgentReady")
		Expect(agentReady.Status).To(Equal(metav1.ConditionTrue))
	})

	It("a spec image bump while the component isn't ready yet is Upgrading, not Degraded", func() {
		// Reach Running with v1 first so LastHealthyAgentImage is populated.
		cr.Status.UIAvailable = true
		root.syncPhaseAndConditions(cr, true, true)
		Expect(cr.Status.Phase).To(Equal("Running"))

		// Bump the agent image and simulate the new pod not being ready yet
		// (Recreate strategy: old pod already torn down, new one still
		// starting) without touching the UI.
		cr.Spec.Agent.Image = "quay.io/test/pulse-agent:v2"
		_, _, agentUpgrading, uiUpgrading := root.syncPhaseAndConditions(cr, false, true)

		Expect(cr.Status.Phase).To(Equal("Upgrading"))
		Expect(agentUpgrading).To(BeTrue())
		Expect(uiUpgrading).To(BeFalse())
		Expect(cr.Status.UpgradeStartedAt).NotTo(BeNil())
		// The old image must still be remembered as the rollback target —
		// it is NOT overwritten just because a new image was requested.
		Expect(cr.Status.LastHealthyAgentImage).To(Equal("quay.io/test/pulse-agent:v1"))

		progressing := findCondition(cr.Status.Conditions, "Progressing")
		Expect(progressing.Status).To(Equal(metav1.ConditionTrue))
		Expect(progressing.Reason).To(Equal("Upgrading"))
	})

	It("the new image becoming ready completes the upgrade: Running again, lastHealthy advances, UpgradeStartedAt clears", func() {
		cr.Status.UIAvailable = true
		root.syncPhaseAndConditions(cr, true, true)

		cr.Spec.Agent.Image = "quay.io/test/pulse-agent:v2"
		root.syncPhaseAndConditions(cr, false, true)
		Expect(cr.Status.Phase).To(Equal("Upgrading"))

		// New image is now ready.
		root.syncPhaseAndConditions(cr, true, true)
		Expect(cr.Status.Phase).To(Equal("Running"))
		Expect(cr.Status.LastHealthyAgentImage).To(Equal("quay.io/test/pulse-agent:v2"))
		Expect(cr.Status.UpgradeStartedAt).To(BeNil())
	})

	It("an unplanned failure with no image change is Degraded, not Upgrading", func() {
		cr.Status.UIAvailable = true
		root.syncPhaseAndConditions(cr, true, true)
		Expect(cr.Status.Phase).To(Equal("Running"))

		// Agent goes unhealthy with the SAME image — not an upgrade in flight.
		_, _, agentUpgrading, _ := root.syncPhaseAndConditions(cr, false, true)
		Expect(agentUpgrading).To(BeFalse())
		Expect(cr.Status.Phase).To(Equal("Degraded"))
		Expect(cr.Status.UpgradeStartedAt).To(BeNil())
	})

	It("emits Running/Upgrading/Degraded events only on phase transitions, not every reconcile", func() {
		cr.Status.UIAvailable = true
		root.syncPhaseAndConditions(cr, true, true)
		Expect(drainEvent(root.Recorder.(*record.FakeRecorder))).To(ContainSubstring("Running"))

		// Second call, still healthy — must NOT re-emit "Running".
		root.syncPhaseAndConditions(cr, true, true)
		expectNoEvent(root.Recorder.(*record.FakeRecorder))
	})
})

var _ = Describe("reconcileAutoRollback", func() {
	const (
		crName    = "autonomy-rollback-pulse"
		namespace = "default"
	)

	var (
		ctx  context.Context
		cr   *pulsev1alpha1.OpenShiftPulse
		root *OpenShiftPulseReconciler
	)

	BeforeEach(func() {
		ctx = testCtx
		root = &OpenShiftPulseReconciler{Client: k8sClient, Scheme: testScheme, Recorder: record.NewFakeRecorder(20)}
		cr = &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec: pulsev1alpha1.OpenShiftPulseSpec{
				Agent: pulsev1alpha1.AgentConfig{Image: "quay.io/test/pulse-agent:bad"},
				UI:    pulsev1alpha1.UIConfig{Image: "quay.io/test/openshiftpulse:bad"},
			},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
	})

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, cr)
	})

	It("does nothing before upgradeHealthTimeout has elapsed", func() {
		started := metav1.NewTime(time.Now().Add(-time.Minute))
		cr.Status.Phase = "Upgrading"
		cr.Status.LastHealthyAgentImage = "quay.io/test/pulse-agent:good"
		cr.Status.UpgradeStartedAt = &started

		rolledBack, err := root.reconcileAutoRollback(ctx, cr, true, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(rolledBack).To(BeFalse())
		Expect(cr.Spec.Agent.Image).To(Equal("quay.io/test/pulse-agent:bad"), "must not touch spec before the timeout")
		expectNoEvent(root.Recorder.(*record.FakeRecorder))
	})

	It("reverts spec.agent.image to lastHealthy once past upgradeHealthTimeout, and emits AutoRolledBack", func() {
		started := metav1.NewTime(time.Now().Add(-upgradeHealthTimeout - time.Minute))
		cr.Status.Phase = "Upgrading"
		cr.Status.LastHealthyAgentImage = "quay.io/test/pulse-agent:good"
		cr.Status.UpgradeStartedAt = &started

		rolledBack, err := root.reconcileAutoRollback(ctx, cr, true, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(rolledBack).To(BeTrue())
		Expect(cr.Spec.Agent.Image).To(Equal("quay.io/test/pulse-agent:good"))

		event := drainEvent(root.Recorder.(*record.FakeRecorder))
		Expect(event).To(ContainSubstring("AutoRolledBack"))
		Expect(event).To(ContainSubstring("pulse-agent:bad"))

		// Persisted, not just mutated in-memory.
		fresh := &pulsev1alpha1.OpenShiftPulse{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: namespace}, fresh)).To(Succeed())
		Expect(fresh.Spec.Agent.Image).To(Equal("quay.io/test/pulse-agent:good"))
	})

	It("rolls back agent and UI independently in the same call when both are stuck", func() {
		started := metav1.NewTime(time.Now().Add(-upgradeHealthTimeout - time.Minute))
		cr.Status.Phase = "Upgrading"
		cr.Status.LastHealthyAgentImage = "quay.io/test/pulse-agent:good"
		cr.Status.LastHealthyUIImage = "quay.io/test/openshiftpulse:good"
		cr.Status.UpgradeStartedAt = &started

		rolledBack, err := root.reconcileAutoRollback(ctx, cr, true, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(rolledBack).To(BeTrue())
		Expect(cr.Spec.Agent.Image).To(Equal("quay.io/test/pulse-agent:good"))
		Expect(cr.Spec.UI.Image).To(Equal("quay.io/test/openshiftpulse:good"))

		rec := root.Recorder.(*record.FakeRecorder)
		Expect(drainEvent(rec)).To(ContainSubstring("AutoRolledBack"))
		Expect(drainEvent(rec)).To(ContainSubstring("AutoRolledBack"))
	})

	// Anti-loop guard: once spec already matches lastHealthy (e.g. this
	// function's own previous patch already landed), there is nothing left
	// to roll back to — reconcileAutoRollback must not keep firing on every
	// subsequent call just because the caller still passes upgrading=true.
	It("anti-loop guard: does not re-patch or re-emit once spec already equals lastHealthy", func() {
		cr.Spec.Agent.Image = "quay.io/test/pulse-agent:good" // already at the rollback target
		Expect(k8sClient.Update(ctx, cr)).To(Succeed())

		started := metav1.NewTime(time.Now().Add(-upgradeHealthTimeout - time.Minute))
		cr.Status.Phase = "Upgrading"
		cr.Status.LastHealthyAgentImage = "quay.io/test/pulse-agent:good"
		cr.Status.UpgradeStartedAt = &started

		rolledBack, err := root.reconcileAutoRollback(ctx, cr, true, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(rolledBack).To(BeFalse(), "nothing to roll back to — spec already matches lastHealthy")
		expectNoEvent(root.Recorder.(*record.FakeRecorder))
	})

	It("does nothing when Phase is not Upgrading, regardless of the upgrading flags", func() {
		cr.Status.Phase = "Degraded"
		started := metav1.NewTime(time.Now().Add(-upgradeHealthTimeout - time.Minute))
		cr.Status.LastHealthyAgentImage = "quay.io/test/pulse-agent:good"
		cr.Status.UpgradeStartedAt = &started

		rolledBack, err := root.reconcileAutoRollback(ctx, cr, true, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(rolledBack).To(BeFalse())
		expectNoEvent(root.Recorder.(*record.FakeRecorder))
	})
})

var _ = Describe("self-heal Events (Recorder wiring)", func() {
	const namespace = "default"

	// agentSelectorMismatchFixture creates a CR plus a pre-existing agent
	// Deployment whose selector deliberately doesn't match what the
	// operator manages (e.g. previously Helm-managed) — the same fixture
	// shape controller_behavior_test.go / agent_reconciler_test.go already
	// use for this exact migration path, chosen here because it's fully
	// deterministic (unlike the stale-pending-pod self-heal site, which
	// depends on a real wall-clock CreationTimestamp envtest's API server
	// won't let a test backdate).
	agentSelectorMismatchFixture := func(ctx context.Context, crName string) *pulsev1alpha1.OpenShiftPulse {
		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())

		name := agentResourceName(crName)
		existing := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "helm-managed-agent"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "helm-managed-agent"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "busybox"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, existing)).To(Succeed())
		return cr
	}

	It("AgentReconciler.reconcileDeployment emits a SelfHealed event when it recreates a selector-mismatched Deployment", func() {
		ctx := testCtx
		const crName = "autonomy-agent-selfheal-pulse"
		cr := agentSelectorMismatchFixture(ctx, crName)
		defer func() { _ = k8sClient.Delete(ctx, cr) }()
		defer func() {
			_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: agentResourceName(crName), Namespace: namespace}})
		}()

		rec := record.NewFakeRecorder(10)
		ar := &AgentReconciler{Client: k8sClient, Scheme: testScheme, Recorder: rec}

		result, err := ar.reconcileDeployment(ctx, cr)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		event := drainEvent(rec)
		Expect(event).To(ContainSubstring("SelfHealed"))
		Expect(event).To(ContainSubstring(agentResourceName(crName)))
	})

	It("a nil Recorder does not panic (sub-reconcilers built without one, as in most existing tests)", func() {
		ctx := testCtx
		const crName = "autonomy-nil-recorder-pulse"
		cr := agentSelectorMismatchFixture(ctx, crName)
		defer func() { _ = k8sClient.Delete(ctx, cr) }()
		defer func() {
			_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: agentResourceName(crName), Namespace: namespace}})
		}()

		ar := &AgentReconciler{Client: k8sClient, Scheme: testScheme} // Recorder deliberately left nil
		Expect(func() { _, _ = ar.reconcileDeployment(ctx, cr) }).NotTo(Panic())
	})

	It("UIReconciler.reconcileUIOAuthSecrets emits a SelfHealed event when it regenerates a malformed cookie-secret", func() {
		ctx := testCtx
		const crName = "autonomy-ui-selfheal-pulse"
		cr := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace}}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, cr) }()

		name := uiOAuthSecretsName(crName)
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Data: map[string][]byte{
				"client-secret": []byte("fine"),
				"cookie-secret": []byte("too-short"), // malformed: not 32 printable bytes
			},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, secret) }()

		rec := record.NewFakeRecorder(10)
		ui := &UIReconciler{Client: k8sClient, Scheme: testScheme, Recorder: rec}

		Expect(ui.reconcileUIOAuthSecrets(ctx, cr)).To(Succeed())

		event := drainEvent(rec)
		Expect(event).To(ContainSubstring("SelfHealed"))
		Expect(event).To(ContainSubstring(name))

		after := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, after)).To(Succeed())
		Expect(isValidCookieSecret(after.Data["cookie-secret"])).To(BeTrue())
		Expect(string(after.Data["client-secret"])).To(Equal("fine"), "client-secret must be left untouched")
	})
})
