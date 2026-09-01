package controller

// Tests for AgentReconciler.buildDeploymentSpec — verifies that the Deployment spec
// produced for each CR configuration contains the expected env vars, volumes, and
// ClusterRole rules. These are unit-level tests that drive reconcile end-to-end
// (create CR → reconcile → inspect Deployment) so they exercise the full code path.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// envVar returns the value of an env var in the first container of the given Deployment,
// or ("", false) if not found. Checks both direct Value and SecretKeyRef.name for source.
func envVar(deploy *appsv1.Deployment, name string) (string, bool) {
	for _, e := range deploy.Spec.Template.Spec.Containers[0].Env {
		if e.Name == name {
			if e.Value != "" {
				return e.Value, true
			}
			if e.ValueFrom != nil {
				return "<from-secret>", true
			}
			return "", true
		}
	}
	return "", false
}

// envVarSecretRef returns the secret name and key for a SecretKeyRef env var.
func envVarSecretRef(deploy *appsv1.Deployment, name string) (secretName, key string, ok bool) {
	for _, e := range deploy.Spec.Template.Spec.Containers[0].Env {
		if e.Name == name && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			return e.ValueFrom.SecretKeyRef.Name,
				e.ValueFrom.SecretKeyRef.Key,
				true
		}
	}
	return "", "", false
}

// reconcileAgent runs the AgentReconciler for the given CR and returns the resulting Deployment.
func reconcileAgent(ctx context.Context, crName, namespace string) *appsv1.Deployment {
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace}}
	_, err := reconciler.Reconcile(ctx, req)
	Expect(err).NotTo(HaveOccurred())

	deploy := &appsv1.Deployment{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name:      agentResourceName(crName),
		Namespace: namespace,
	}, deploy)).To(Succeed())
	return deploy
}

var _ = Describe("AgentReconciler — Deployment spec", func() {
	const namespace = "default"
	var ctx context.Context

	BeforeEach(func() { ctx = testCtx })

	// ── MCP ────────────────────────────────────────────────────────────────────

	Describe("MCP env var injection", func() {
		const crName = "spec-mcp-pulse"

		AfterEach(func() {
			cr := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace}}
			_ = k8sClient.Delete(ctx, cr)
		})

		It("sets PULSE_MCP_URL (not PULSE_AGENT_MCP_URL) when MCP is enabled", func() {
			// This exact env var name was the root cause of the Degraded state:
			// skills/sre/mcp.yaml reads ${PULSE_MCP_URL}, not PULSE_AGENT_MCP_URL.
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					Agent: pulsev1alpha1.AgentConfig{
						MCP: pulsev1alpha1.MCPConfig{Enabled: true},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			deploy := reconcileAgent(ctx, crName, namespace)

			_, wrongName := envVar(deploy, "PULSE_AGENT_MCP_URL")
			Expect(wrongName).To(BeFalse(), "PULSE_AGENT_MCP_URL must NOT be set (skills read PULSE_MCP_URL)")

			val, ok := envVar(deploy, "PULSE_MCP_URL")
			Expect(ok).To(BeTrue(), "PULSE_MCP_URL must be set when MCP is enabled")
			Expect(val).To(ContainSubstring(crName+"-mcp-server"),
				"PULSE_MCP_URL must reference the in-cluster MCP service")
			Expect(val).To(ContainSubstring(":8081"),
				"PULSE_MCP_URL must use port 8081 (matches skill mcp.yaml default)")
		})

		It("does NOT set PULSE_MCP_URL when MCP is disabled", func() {
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					Agent: pulsev1alpha1.AgentConfig{
						MCP: pulsev1alpha1.MCPConfig{Enabled: false},
					},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			deploy := reconcileAgent(ctx, crName, namespace)

			_, ok := envVar(deploy, "PULSE_MCP_URL")
			Expect(ok).To(BeFalse(), "PULSE_MCP_URL must not be injected when MCP is disabled")
		})
	})

	// ── Vertex AI credentials ──────────────────────────────────────────────────

	Describe("Vertex AI credential injection", func() {
		const crName = "spec-vertex-pulse"

		AfterEach(func() {
			cr := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace}}
			_ = k8sClient.Delete(ctx, cr)
		})

		It("injects ANTHROPIC_VERTEX_PROJECT_ID and CLOUD_ML_REGION", func() {
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					VertexAI: &pulsev1alpha1.VertexAIConfig{
						ProjectID: "my-gcp-project",
						Region:    "us-east5",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			deploy := reconcileAgent(ctx, crName, namespace)

			pid, ok := envVar(deploy, "ANTHROPIC_VERTEX_PROJECT_ID")
			Expect(ok).To(BeTrue())
			Expect(pid).To(Equal("my-gcp-project"))

			region, ok := envVar(deploy, "CLOUD_ML_REGION")
			Expect(ok).To(BeTrue())
			Expect(region).To(Equal("us-east5"))
		})

		It("defaults CLOUD_ML_REGION to us-east5 when region is empty", func() {
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					VertexAI: &pulsev1alpha1.VertexAIConfig{
						ProjectID: "my-gcp-project",
						Region:    "", // intentionally empty
					},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			deploy := reconcileAgent(ctx, crName, namespace)

			region, ok := envVar(deploy, "CLOUD_ML_REGION")
			Expect(ok).To(BeTrue())
			Expect(region).To(Equal("us-east5"), "should default to us-east5")
		})

		It("does NOT mount GCP SA key volume when credentialSecret is empty (ADC/workload identity)", func() {
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					VertexAI: &pulsev1alpha1.VertexAIConfig{
						ProjectID:        "my-gcp-project",
						CredentialSecret: "", // ADC — no key file
					},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			deploy := reconcileAgent(ctx, crName, namespace)

			for _, v := range deploy.Spec.Template.Spec.Volumes {
				Expect(v.Name).NotTo(Equal("gcp-sa-key"),
					"gcp-sa-key volume must not be mounted when credentialSecret is empty")
			}
			_, ok := envVar(deploy, "GOOGLE_APPLICATION_CREDENTIALS")
			Expect(ok).To(BeFalse(), "GOOGLE_APPLICATION_CREDENTIALS must not be set without credentialSecret")
		})

		It("mounts GCP SA key volume when credentialSecret is specified", func() {
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					VertexAI: &pulsev1alpha1.VertexAIConfig{
						ProjectID:        "my-gcp-project",
						CredentialSecret: "gcp-sa-key-secret",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			deploy := reconcileAgent(ctx, crName, namespace)

			found := false
			for _, v := range deploy.Spec.Template.Spec.Volumes {
				if v.Name == "gcp-sa-key" {
					found = true
					Expect(v.VolumeSource.Secret.SecretName).To(Equal("gcp-sa-key-secret"))
				}
			}
			Expect(found).To(BeTrue(), "gcp-sa-key volume must be present when credentialSecret is set")

			val, ok := envVar(deploy, "GOOGLE_APPLICATION_CREDENTIALS")
			Expect(ok).To(BeTrue())
			Expect(val).To(Equal("/var/secrets/google/key.json"))
		})
	})

	// ── Anthropic API key ──────────────────────────────────────────────────────

	Describe("Anthropic API key injection", func() {
		const crName = "spec-anthropic-pulse"

		AfterEach(func() {
			cr := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace}}
			_ = k8sClient.Delete(ctx, cr)
		})

		It("injects ANTHROPIC_API_KEY from the named secret", func() {
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					AnthropicAPIKey: &pulsev1alpha1.APIKeyConfig{
						ExistingSecret: "my-anthropic-secret",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			deploy := reconcileAgent(ctx, crName, namespace)

			secretName, key, ok := envVarSecretRef(deploy, "ANTHROPIC_API_KEY")
			Expect(ok).To(BeTrue(), "ANTHROPIC_API_KEY must be injected from a SecretKeyRef")
			Expect(secretName).To(Equal("my-anthropic-secret"))
			Expect(key).To(Equal("ANTHROPIC_API_KEY"))
		})
	})

	// ── Database URL ───────────────────────────────────────────────────────────

	Describe("Database URL injection", func() {
		It("injects PULSE_AGENT_DATABASE_URL from the postgresql secret when database is enabled", func() {
			const crName = "spec-db-enabled-pulse"
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					Database: pulsev1alpha1.DatabaseConfig{StorageSize: "5Gi"},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

			deploy := reconcileAgent(ctx, crName, namespace)

			secretName, key, ok := envVarSecretRef(deploy, "PULSE_AGENT_DATABASE_URL")
			Expect(ok).To(BeTrue(), "PULSE_AGENT_DATABASE_URL must be a SecretKeyRef when DB enabled")
			Expect(secretName).To(Equal(crName+"-postgresql"),
				"must reference the {name}-postgresql connection secret")
			Expect(key).To(Equal("database-url"))
		})

		It("PULSE_AGENT_DATABASE_URL is always injected because CRD default (5Gi) makes DB always enabled", func() {
			// DatabaseConfig.StorageSize has kubebuilder:default="5Gi" in the CRD schema.
			// The API server applies this default on every CR create, so databaseEnabled()
			// always returns true. This test documents that invariant.
			const crName = "spec-db-default-pulse"
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec:       pulsev1alpha1.OpenShiftPulseSpec{},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

			deploy := reconcileAgent(ctx, crName, namespace)

			_, _, ok := envVarSecretRef(deploy, "PULSE_AGENT_DATABASE_URL")
			Expect(ok).To(BeTrue(),
				"PULSE_AGENT_DATABASE_URL must always be injected — storageSize defaults to 5Gi via CRD schema")
		})
	})

	// ── ClusterRole rules ──────────────────────────────────────────────────────

	Describe("ClusterRole RBAC rules", func() {
		const crName = "spec-rbac-pulse"

		AfterEach(func() {
			cr := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace}}
			_ = k8sClient.Delete(ctx, cr)
		})

		hasVerb := func(cr *rbacv1.ClusterRole, resource, verb string) bool {
			for _, rule := range cr.Rules {
				for _, r := range rule.Resources {
					if r != resource {
						continue
					}
					for _, v := range rule.Verbs {
						if v == verb {
							return true
						}
					}
				}
			}
			return false
		}

		It("ClusterRole does not include write verbs by default", func() {
			pulseCR := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec:       pulsev1alpha1.OpenShiftPulseSpec{},
			}
			Expect(k8sClient.Create(ctx, pulseCR)).To(Succeed())
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace}}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			clusterRole := &rbacv1.ClusterRole{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentClusterRoleName(crName, namespace)}, clusterRole)).To(Succeed())
			Expect(hasVerb(clusterRole, "pods", "delete")).To(BeFalse(),
				"delete verb must NOT be in ClusterRole by default")
		})

		It("ClusterRole includes delete/patch when AllowWriteOperations is true", func() {
			pulseCR := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					Agent: pulsev1alpha1.AgentConfig{AllowWriteOperations: true},
				},
			}
			Expect(k8sClient.Create(ctx, pulseCR)).To(Succeed())
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace}}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			clusterRole := &rbacv1.ClusterRole{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentClusterRoleName(crName, namespace)}, clusterRole)).To(Succeed())
			Expect(hasVerb(clusterRole, "pods", "delete")).To(BeTrue(),
				"delete verb must be present when AllowWriteOperations=true")
			Expect(hasVerb(clusterRole, "deployments", "patch")).To(BeTrue(),
				"patch verb must be present when AllowWriteOperations=true")
		})

		It("ClusterRole includes secrets read when AllowSecretAccess is true", func() {
			pulseCR := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					Agent: pulsev1alpha1.AgentConfig{AllowSecretAccess: true},
				},
			}
			Expect(k8sClient.Create(ctx, pulseCR)).To(Succeed())
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: crName, Namespace: namespace}}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			clusterRole := &rbacv1.ClusterRole{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentClusterRoleName(crName, namespace)}, clusterRole)).To(Succeed())
			Expect(hasVerb(clusterRole, "secrets", "get")).To(BeTrue(),
				"get verb on secrets must be present when AllowSecretAccess=true")
		})
	})

	// ── Trust level ────────────────────────────────────────────────────────────

	Describe("Trust level injection", func() {
		const crName = "spec-trust-pulse"

		AfterEach(func() {
			cr := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace}}
			_ = k8sClient.Delete(ctx, cr)
		})

		It("injects PULSE_AGENT_TRUST_LEVEL from spec.agent.trustLevel", func() {
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					Agent: pulsev1alpha1.AgentConfig{TrustLevel: 3},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			deploy := reconcileAgent(ctx, crName, namespace)

			val, ok := envVar(deploy, "PULSE_AGENT_TRUST_LEVEL")
			Expect(ok).To(BeTrue())
			Expect(val).To(Equal("3"))

			// The name the agent's settings actually bind. Only the legacy name
			// was injected before, so spec.agent.trustLevel never reached the
			// agent — it gated at its built-in default of 2 regardless of the CR.
			val, ok = envVar(deploy, "PULSE_AGENT_MAX_TRUST_LEVEL")
			Expect(ok).To(BeTrue())
			Expect(val).To(Equal("3"))
		})

		It("injects PULSE_AGENT_ADMIN_USERS from spec.agent.adminUsers", func() {
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec: pulsev1alpha1.OpenShiftPulseSpec{
					Agent: pulsev1alpha1.AgentConfig{AdminUsers: "kube:admin,sre@example.com"},
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			deploy := reconcileAgent(ctx, crName, namespace)

			val, ok := envVar(deploy, "PULSE_AGENT_ADMIN_USERS")
			Expect(ok).To(BeTrue())
			Expect(val).To(Equal("kube:admin,sre@example.com"))
		})

		It("leaves PULSE_AGENT_ADMIN_USERS unset when no admins are configured", func() {
			// Unset and empty mean the same thing to the agent, but setting it
			// empty would make `oc set env --list` report an admin list that
			// does not exist.
			cr := &pulsev1alpha1.OpenShiftPulse{
				ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
				Spec:       pulsev1alpha1.OpenShiftPulseSpec{Agent: pulsev1alpha1.AgentConfig{}},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			deploy := reconcileAgent(ctx, crName, namespace)

			_, ok := envVar(deploy, "PULSE_AGENT_ADMIN_USERS")
			Expect(ok).To(BeFalse())
		})
	})
})

// ── Temporal host injection ─────────────────────────────────────────────────

var _ = Describe("Agent Temporal wiring", func() {
	const namespace = "default"

	It("injects PULSE_AGENT_TEMPORAL_HOST only when temporal is enabled", func() {
		enabled := true
		const crName = "spec-temporal-pulse"
		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec: pulsev1alpha1.OpenShiftPulseSpec{
				Temporal: pulsev1alpha1.TemporalConfig{Enabled: &enabled},
			},
		}
		Expect(k8sClient.Create(testCtx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, cr) })

		deploy := reconcileAgent(testCtx, crName, namespace)
		val, ok := envVar(deploy, "PULSE_AGENT_TEMPORAL_HOST")
		Expect(ok).To(BeTrue(), "agent must be pointed at the Temporal service when enabled")
		Expect(val).To(Equal(crName + "-temporal:7233"))
	})

	It("omits the variable when temporal is disabled, keeping the agent's durable path inert", func() {
		const crName = "spec-no-temporal-pulse"
		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
		}
		Expect(k8sClient.Create(testCtx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, cr) })

		deploy := reconcileAgent(testCtx, crName, namespace)
		_, ok := envVar(deploy, "PULSE_AGENT_TEMPORAL_HOST")
		Expect(ok).To(BeFalse(), "no host env when temporal is disabled")
	})
})

var _ = Describe("Agent durable auto-fix wiring", func() {
	const namespace = "default"

	It("injects PULSE_AGENT_DURABLE_AUTOFIX when enabled", func() {
		enabled := true
		const crName = "spec-durable-pulse"
		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
			Spec: pulsev1alpha1.OpenShiftPulseSpec{
				Agent: pulsev1alpha1.AgentConfig{DurableAutoFix: &enabled},
			},
		}
		Expect(k8sClient.Create(testCtx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, cr) })

		deploy := reconcileAgent(testCtx, crName, namespace)
		val, ok := envVar(deploy, "PULSE_AGENT_DURABLE_AUTOFIX")
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal("true"))
	})

	It("omits it by default, keeping auto-fix inline", func() {
		const crName = "spec-inline-pulse"
		cr := &pulsev1alpha1.OpenShiftPulse{
			ObjectMeta: metav1.ObjectMeta{Name: crName, Namespace: namespace},
		}
		Expect(k8sClient.Create(testCtx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(testCtx, cr) })

		deploy := reconcileAgent(testCtx, crName, namespace)
		_, ok := envVar(deploy, "PULSE_AGENT_DURABLE_AUTOFIX")
		Expect(ok).To(BeFalse(), "durable auto-fix must be opt-in")
	})
})
