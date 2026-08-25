package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// Coverage for agentUIVersionSkew. The case that motivated it is the first
// one: dev05 ran agent v2.25.0 against UI v2.24.0 for weeks with both
// Deployments healthy and every pipeline green.
var _ = Describe("agentUIVersionSkew", func() {
	newCR := func(agentImage, uiImage string) *pulsev1alpha1.OpenShiftPulse {
		return &pulsev1alpha1.OpenShiftPulse{
			Spec: pulsev1alpha1.OpenShiftPulseSpec{
				Agent: pulsev1alpha1.AgentConfig{Image: agentImage},
				UI:    pulsev1alpha1.UIConfig{Image: uiImage},
			},
		}
	}

	It("detects the real dev05 skew", func() {
		skewed, msg := agentUIVersionSkew(newCR(
			"quay.io/amobrem/pulse-agent:v2.25.0",
			"quay.io/amobrem/openshiftpulse:v2.24.0",
		))
		Expect(skewed).To(BeTrue())
		Expect(msg).To(ContainSubstring("v2.25.0"))
		Expect(msg).To(ContainSubstring("v2.24.0"))
	})

	It("reports no skew when both are pinned at the same version", func() {
		skewed, msg := agentUIVersionSkew(newCR(
			"quay.io/amobrem/pulse-agent:v2.26.1",
			"quay.io/amobrem/openshiftpulse:v2.26.1",
		))
		Expect(skewed).To(BeFalse())
		Expect(msg).To(BeEmpty())
	})

	It("reports no skew when either image is unset — the operator supplies a default", func() {
		skewed, _ := agentUIVersionSkew(newCR("", "quay.io/amobrem/openshiftpulse:v2.26.1"))
		Expect(skewed).To(BeFalse())
		skewed, _ = agentUIVersionSkew(newCR("quay.io/amobrem/pulse-agent:v2.26.1", ""))
		Expect(skewed).To(BeFalse())
	})

	It("fails open on digest pins, which carry no comparable version", func() {
		skewed, _ := agentUIVersionSkew(newCR(
			"quay.io/amobrem/pulse-agent@sha256:abc123",
			"quay.io/amobrem/openshiftpulse:v2.26.1",
		))
		Expect(skewed).To(BeFalse())
	})

	It("fails open on a floating latest tag, which says nothing about what runs", func() {
		skewed, _ := agentUIVersionSkew(newCR(
			"quay.io/amobrem/pulse-agent:latest",
			"quay.io/amobrem/openshiftpulse:v2.26.1",
		))
		Expect(skewed).To(BeFalse())
	})

	It("does not mistake a registry port for a tag", func() {
		// registry.local:5000/pulse-agent has a colon but no tag. Reading the
		// port as a version would compare "5000" against a real tag and report
		// a skew that does not exist.
		Expect(imageTag("registry.local:5000/pulse-agent")).To(BeEmpty())
		Expect(imageTag("registry.local:5000/pulse-agent:v2.26.1")).To(Equal("v2.26.1"))

		skewed, _ := agentUIVersionSkew(newCR(
			"registry.local:5000/pulse-agent:v2.26.1",
			"registry.local:5000/openshiftpulse:v2.26.1",
		))
		Expect(skewed).To(BeFalse())
	})
})

var _ = Describe("setVersionSkewCondition", func() {
	It("records True when the versions match, so a healthy CR reads positively", func() {
		cr := &pulsev1alpha1.OpenShiftPulse{}
		setVersionSkewCondition(cr, false, "")
		cond := apimeta.FindStatusCondition(cr.Status.Conditions, "AgentUIVersionsMatch")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("VersionsMatch"))
	})

	It("records False with the explanation when they differ", func() {
		cr := &pulsev1alpha1.OpenShiftPulse{}
		setVersionSkewCondition(cr, true, "agent v2.25.0 vs ui v2.24.0")
		cond := apimeta.FindStatusCondition(cr.Status.Conditions, "AgentUIVersionsMatch")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("VersionSkew"))
		Expect(cond.Message).To(ContainSubstring("v2.24.0"))
	})
})
