package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

const (
	defaultAgentImage = "quay.io/amobrem/pulse-agent:latest"
	agentPort         = int32(8080)

	annotationOwnerName      = "pulse.ai/owner-name"
	annotationOwnerNamespace = "pulse.ai/owner-namespace"
	annotationOwnerUID       = "pulse.ai/owner-uid"

	// agentRequeueDelay is used when a reconcile step needs a short requeue
	// rather than an error (e.g. after deleting a selector-mismatched
	// Deployment so the next reconcile can recreate it cleanly).
	agentRequeueDelay = 5 * time.Second
)

// AgentReconciler reconciles the agent sub-resources of an OpenShiftPulse CR.
// It is NOT registered as a standalone controller — SetupWithManager must not be
// called on it because OpenShiftPulseReconciler already watches OpenShiftPulse
// and delegates here. Registering both would cause concurrent reconcile races on
// every watch event.
//
// Reconcile is kept for testing: envtest can drive the sub-reconcilers via the
// standard ctrl.Request interface without standing up the full root reconciler.
type AgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile runs all agent sub-resource reconcile steps in order.
// Used by envtest tests; in production it is called from OpenShiftPulseReconciler.reconcileAgent.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cr := &pulsev1alpha1.OpenShiftPulse{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	type step struct {
		name string
		fn   func(context.Context, *pulsev1alpha1.OpenShiftPulse) error
	}
	for _, s := range []step{
		{"ServiceAccount", r.reconcileServiceAccount},
		{"ClusterRole", r.reconcileClusterRole},
		{"ClusterRoleBinding", r.reconcileClusterRoleBinding},
		{"WSTokenSecret", r.reconcileWSTokenSecret},
		{"MemoryPVC", r.reconcileMemoryPVC},
	} {
		if err := s.fn(ctx, cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", s.name, err)
		}
	}

	// Deployment is not in the generic loop above: it can return a genuine
	// (non-error) requeue signal — see reconcileDeployment's doc comment.
	if result, err := r.reconcileDeployment(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("deployment: %w", err)
	} else if result.RequeueAfter > 0 {
		return result, nil
	}

	if err := r.reconcileService(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("service: %w", err)
	}
	return ctrl.Result{}, nil
}

// ─── helpers ────────────────────────────────────────────────────────────────

func agentResourceName(crName string) string {
	return crName + "-openshift-sre-agent"
}

// agentClusterRoleName returns a namespace-qualified name for the agent's
// cluster-scoped ClusterRole/ClusterRoleBinding — see
// deleteStaleUnqualifiedClusterScopedResource's doc comment for why this
// can't just be agentResourceName(crName) the way the agent's namespaced
// resources (ServiceAccount, Deployment, Service, ...) are named.
func agentClusterRoleName(crName, crNamespace string) string {
	return crNamespace + "-" + agentResourceName(crName)
}

func memoryPVCName(crName string) string {
	return crName + "-openshift-sre-agent-memory"
}

func wsTokenSecretName(crName string) string {
	return crName + "-ws-token"
}

func resolvedImage(cr *pulsev1alpha1.OpenShiftPulse) string {
	if cr.Spec.Agent.Image != "" {
		return cr.Spec.Agent.Image
	}
	return defaultAgentImage
}

func databaseEnabled(cr *pulsev1alpha1.OpenShiftPulse) bool {
	return cr.Spec.Database.StorageSize != "" || cr.Spec.Database.Image != ""
}

// monitoringEnabled reports whether monitoring should be reconciled. Enabled is
// a *bool defaulted to true by the CRD (+kubebuilder:default=true) — nil means
// "not yet defaulted" (e.g. an in-memory struct built by a test or client code
// that hasn't round-tripped through the API server), so nil is treated as true
// to match the CRD default rather than as false.
func monitoringEnabled(cr *pulsev1alpha1.OpenShiftPulse) bool {
	return cr.Spec.Monitoring.Enabled == nil || *cr.Spec.Monitoring.Enabled
}

// hasAIBackend reports whether the CR configures a usable AI backend. The CRD
// does not require one (see the mutual-exclusion XValidation on OpenShiftPulseSpec
// for why it isn't a hard "at least one" requirement), so callers that need to
// warn about a misconfigured instance should check this explicitly.
func hasAIBackend(cr *pulsev1alpha1.OpenShiftPulse) bool {
	if cr.Spec.VertexAI != nil && cr.Spec.VertexAI.ProjectID != "" {
		return true
	}
	if cr.Spec.AnthropicAPIKey != nil && cr.Spec.AnthropicAPIKey.ExistingSecret != "" {
		return true
	}
	return false
}

// agentResources returns the spec-provided resources when non-empty, or sensible
// defaults. CPU is omitted entirely from defaults: a CPU limit with no request
// causes Kubernetes to auto-set Requests.CPU = Limits.CPU, which blocks scheduling
// on nodes at 100% CPU request allocation (common in dev/shared clusters).
// Only memory is bounded by default; users set CPU via spec.agent.resources.
func agentResources(cr *pulsev1alpha1.OpenShiftPulse) corev1.ResourceRequirements {
	if cr.Spec.Agent.Resources.Requests != nil || cr.Spec.Agent.Resources.Limits != nil {
		return cr.Spec.Agent.Resources
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}
}

func clusterScopedAnnotations(cr *pulsev1alpha1.OpenShiftPulse) map[string]string {
	return map[string]string{
		annotationOwnerName:      cr.Name,
		annotationOwnerNamespace: cr.Namespace,
		annotationOwnerUID:       string(cr.UID),
	}
}

// deleteStaleUnqualifiedClusterScopedResource is a one-time migration helper.
// Before this fix, the agent/UI/MCP ClusterRole and ClusterRoleBinding names
// were derived from crName alone (e.g. "<crName>-openshift-sre-agent") with
// no namespace qualifier — a cluster-scoped resource with no namespace of
// its own. Two OpenShiftPulse CRs sharing the same name in different
// namespaces (an expected shape: the CSV declares AllNamespaces as the only
// supported install mode) would collide on that one name and silently
// overwrite each other's RBAC on every reconcile. Cluster-scoped resources
// now get a namespace-qualified name instead (see e.g. agentClusterRoleName),
// but any resource created under the old scheme before this fix needs to be
// cleaned up so it doesn't leak forever as an orphan. Deletes obj (looked up
// by oldName) only if its owner-uid annotation matches cr's UID — this is
// the exact case this bug caused, so if the annotation belongs to some other
// CR (the collision already happened), leave it alone rather than risk
// deleting a resource that CR still depends on.
func deleteStaleUnqualifiedClusterScopedResource(ctx context.Context, c client.Client, obj client.Object, oldName string, cr *pulsev1alpha1.OpenShiftPulse) error {
	obj.SetName(oldName)
	if err := c.Get(ctx, types.NamespacedName{Name: oldName}, obj); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if obj.GetAnnotations()[annotationOwnerUID] != string(cr.UID) {
		return nil
	}
	if err := c.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

// generateToken returns a 32-character lowercase hex string from 16 crypto-random bytes.
func generateToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand read: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ─── sub-reconcilers ────────────────────────────────────────────────────────

func (r *AgentReconciler) reconcileServiceAccount(ctx context.Context, cr *pulsev1alpha1.OpenShiftPulse) error {
	desired := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentResourceName(cr.Name),
			Namespace: cr.Namespace,
		},
	}
	if err := controllerutil.SetControllerReference(cr, desired, r.Scheme); err != nil {
		return err
	}

	existing := &corev1.ServiceAccount{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	return err // nil on success, non-nil on transient error
}

func (r *AgentReconciler) reconcileClusterRole(ctx context.Context, cr *pulsev1alpha1.OpenShiftPulse) error {
	name := agentClusterRoleName(cr.Name, cr.Namespace)
	if err := deleteStaleUnqualifiedClusterScopedResource(ctx, r.Client, &rbacv1.ClusterRole{}, agentResourceName(cr.Name), cr); err != nil {
		return fmt.Errorf("migrate stale agent ClusterRole: %w", err)
	}
	desired := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: clusterScopedAnnotations(cr),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{
					"pods", "pods/log", "nodes", "events",
					"services", "namespaces", "configmaps",
					"persistentvolumeclaims", "resourcequotas",
					"serviceaccounts", "endpoints", "limitranges",
				},
				Verbs: []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments", "replicasets", "statefulsets", "daemonsets"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"autoscaling"},
				Resources: []string{"horizontalpodautoscalers"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"batch"},
				Resources: []string{"jobs", "cronjobs"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"discovery.k8s.io"},
				Resources: []string{"endpointslices"},
				Verbs:     []string{"get", "list"},
			},
			{
				APIGroups: []string{"policy"},
				Resources: []string{"poddisruptionbudgets"},
				Verbs:     []string{"get", "list"},
			},
			{
				APIGroups: []string{"storage.k8s.io"},
				Resources: []string{"storageclasses"},
				Verbs:     []string{"get", "list"},
			},
			{
				APIGroups: []string{"networking.k8s.io"},
				Resources: []string{"ingresses", "networkpolicies"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"rbac.authorization.k8s.io"},
				Resources: []string{"clusterroles", "clusterrolebindings", "roles", "rolebindings"},
				Verbs:     []string{"get", "list"},
			},
			{
				APIGroups: []string{"metrics.k8s.io"},
				Resources: []string{"nodes", "pods"},
				Verbs:     []string{"get", "list"},
			},
		},
	}

	// AllowSecretAccess lets the agent read Secrets (e.g. to surface misconfigurations).
	// Disabled by default — cluster admins opt in via spec.agent.allowSecretAccess=true.
	if cr.Spec.Agent.AllowSecretAccess {
		desired.Rules = append(desired.Rules, rbacv1.PolicyRule{
			APIGroups: []string{""},
			Resources: []string{"secrets"},
			Verbs:     []string{"get", "list", "watch"},
		})
	}

	// AllowWriteOperations lets the agent perform remediations (restart pods, scale, etc.).
	// Disabled by default — cluster admins opt in via spec.agent.allowWriteOperations=true.
	if cr.Spec.Agent.AllowWriteOperations {
		desired.Rules = append(desired.Rules, rbacv1.PolicyRule{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"delete"},
		}, rbacv1.PolicyRule{
			APIGroups: []string{"apps"},
			Resources: []string{"deployments", "statefulsets"},
			Verbs:     []string{"patch", "update"},
		})
	}

	cr2 := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cr2, func() error {
		cr2.Annotations = desired.Annotations
		cr2.Rules = desired.Rules
		return nil
	})
	return err
}

func (r *AgentReconciler) reconcileClusterRoleBinding(ctx context.Context, cr *pulsev1alpha1.OpenShiftPulse) error {
	name := agentClusterRoleName(cr.Name, cr.Namespace)
	if err := deleteStaleUnqualifiedClusterScopedResource(ctx, r.Client, &rbacv1.ClusterRoleBinding{}, agentResourceName(cr.Name), cr); err != nil {
		return fmt.Errorf("migrate stale agent ClusterRoleBinding: %w", err)
	}
	// saName (not name): the ServiceAccount subject is a namespaced resource,
	// still named plainly by agentResourceName — only this Binding and its
	// ClusterRole need the namespace-qualified name.
	saName := agentResourceName(cr.Name)
	desired := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: clusterScopedAnnotations(cr),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     name,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: cr.Namespace,
			},
		},
	}

	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, crb, func() error {
		crb.Annotations = desired.Annotations
		crb.Subjects = desired.Subjects
		// RoleRef is immutable — set only on creation.
		if crb.RoleRef.Name == "" {
			crb.RoleRef = desired.RoleRef
		}
		return nil
	})
	return err
}

func (r *AgentReconciler) reconcileWSTokenSecret(ctx context.Context, cr *pulsev1alpha1.OpenShiftPulse) error {
	name := wsTokenSecretName(cr.Name)
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cr.Namespace}, existing)
	if err == nil {
		return nil // already exists — preserve the token
	}
	if !errors.IsNotFound(err) {
		return err
	}

	token, err := generateToken()
	if err != nil {
		return err
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
		},
		StringData: map[string]string{
			"token": token,
		},
	}
	if err := controllerutil.SetControllerReference(cr, desired, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, desired)
}

func (r *AgentReconciler) reconcileMemoryPVC(ctx context.Context, cr *pulsev1alpha1.OpenShiftPulse) error {
	name := memoryPVCName(cr.Name)
	existing := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cr.Namespace}, existing)
	if err == nil {
		return nil // already exists — don't resize RWO PVC
	}
	if !errors.IsNotFound(err) {
		return err
	}

	desired := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(cr, desired, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, desired)
}

func (r *AgentReconciler) buildDeploymentSpec(cr *pulsev1alpha1.OpenShiftPulse, info *ClusterInfo) appsv1.DeploymentSpec {
	name := agentResourceName(cr.Name)

	envVars := []corev1.EnvVar{
		{
			Name: "PULSE_AGENT_WS_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: wsTokenSecretName(cr.Name),
					},
					Key: "token",
				},
			},
		},
		{
			Name:  "PULSE_AGENT_TRUST_LEVEL",
			Value: strconv.Itoa(int(cr.Spec.Agent.TrustLevel)),
		},
	}

	// Inject AI backend credentials.
	if cr.Spec.VertexAI != nil && cr.Spec.VertexAI.ProjectID != "" {
		region := cr.Spec.VertexAI.Region
		if region == "" {
			region = "us-east5"
		}
		envVars = append(envVars,
			corev1.EnvVar{Name: "ANTHROPIC_VERTEX_PROJECT_ID", Value: cr.Spec.VertexAI.ProjectID},
			corev1.EnvVar{Name: "CLOUD_ML_REGION", Value: region},
		)
		// Only mount SA key if credentialSecret is specified — clusters using workload
		// identity or ADC don't need a key file, just the project ID and region.
		if cr.Spec.VertexAI.CredentialSecret != "" {
			envVars = append(envVars, corev1.EnvVar{
				Name:  "GOOGLE_APPLICATION_CREDENTIALS",
				Value: "/var/secrets/google/key.json",
			})
		}
	} else if cr.Spec.AnthropicAPIKey != nil && cr.Spec.AnthropicAPIKey.ExistingSecret != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: "ANTHROPIC_API_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cr.Spec.AnthropicAPIKey.ExistingSecret},
					Key:                  "ANTHROPIC_API_KEY",
				},
			},
		})
	}

	if databaseEnabled(cr) {
		envVars = append(envVars, corev1.EnvVar{
			Name: "PULSE_AGENT_DATABASE_URL",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: cr.Name + "-postgresql",
					},
					Key: "database-url",
				},
			},
		})
	}

	if cr.Spec.Agent.MCP.Enabled {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "PULSE_MCP_URL",
			Value: MCPServiceURL(cr.Name, cr.Namespace),
		})
	}

	if info != nil && info.ACMAvailable {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "PULSE_AGENT_ACM_THANOS_ENABLED",
			Value: "true",
		})
		if info.ACMThanosURL != "" {
			envVars = append(envVars, corev1.EnvVar{
				Name:  "PULSE_AGENT_ACM_THANOS_URL",
				Value: info.ACMThanosURL,
			})
		}
	}

	healthzProbeHandler := corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{
			Path: "/healthz",
			Port: intstr.FromInt(int(agentPort)),
		},
	}

	return appsv1.DeploymentSpec{
		Strategy: appsv1.DeploymentStrategy{
			Type: appsv1.RecreateDeploymentStrategyType,
		},
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"app": name},
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": name},
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: name,
				SecurityContext:    defaultPodSecCtx(nil), // OCP assigns UID from namespace range via SCC
				Containers: []corev1.Container{
					{
						Name:            "agent",
						Image:           resolvedImage(cr),
						Resources:       agentResources(cr),
						SecurityContext: defaultContainerSecCtx(),
						Ports: []corev1.ContainerPort{
							{
								Name:          "http",
								ContainerPort: agentPort,
								Protocol:      corev1.ProtocolTCP,
							},
						},
						Env: envVars,
						VolumeMounts: func() []corev1.VolumeMount {
							mounts := []corev1.VolumeMount{{Name: "memory", MountPath: "/memory"}}
							if cr.Spec.VertexAI != nil && cr.Spec.VertexAI.CredentialSecret != "" {
								mounts = append(mounts, corev1.VolumeMount{
									Name:      "gcp-sa-key",
									MountPath: "/var/secrets/google",
									ReadOnly:  true,
								})
							}
							return mounts
						}(),
						LivenessProbe: &corev1.Probe{
							ProbeHandler:        healthzProbeHandler,
							InitialDelaySeconds: 15,
							PeriodSeconds:       20,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:        healthzProbeHandler,
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
						},
					},
				},
				Volumes: func() []corev1.Volume {
					vols := []corev1.Volume{{
						Name: "memory",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: memoryPVCName(cr.Name),
							},
						},
					}}
					if cr.Spec.VertexAI != nil && cr.Spec.VertexAI.CredentialSecret != "" {
						vols = append(vols, corev1.Volume{
							Name: "gcp-sa-key",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: cr.Spec.VertexAI.CredentialSecret,
								},
							},
						})
					}
					return vols
				}(),
			},
		},
	}
}

// reconcileDeployment returns a non-zero ctrl.Result.RequeueAfter (with a nil
// error) when it deletes a Deployment whose immutable selector no longer
// matches (e.g. a Helm-managed Deployment being adopted). This is a normal,
// expected step of a successful migration, not a failure: returning it as an
// error here used to make controller-runtime log a Warning ReconcileFailed
// event and apply exponential backoff, so a healthy self-heal looked like a
// broken operator to anyone running `kubectl describe` or alerting on
// Warning events.
func (r *AgentReconciler) reconcileDeployment(ctx context.Context, cr *pulsev1alpha1.OpenShiftPulse) (ctrl.Result, error) {
	info := DetectClusterInfo(ctx, r.Client)
	name := agentResourceName(cr.Name)
	wantSelector := map[string]string{"app": name}

	existing := &appsv1.Deployment{}
	getErr := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cr.Namespace}, existing)
	if getErr != nil && !errors.IsNotFound(getErr) {
		return ctrl.Result{}, getErr
	}

	if getErr == nil {
		// Check for selector mismatch (e.g. Helm-managed deployment).
		// Deployment selectors are immutable — delete and let next reconcile recreate.
		mismatch := existing.Spec.Selector == nil
		for k, v := range wantSelector {
			if existing.Spec.Selector == nil || existing.Spec.Selector.MatchLabels[k] != v {
				mismatch = true
				break
			}
		}
		if mismatch {
			if delErr := r.Delete(ctx, existing); delErr != nil && !errors.IsNotFound(delErr) {
				return ctrl.Result{}, fmt.Errorf("delete mismatched deployment: %w", delErr)
			}
			return ctrl.Result{RequeueAfter: agentRequeueDelay}, nil
		}

		// Selector matches — patch mutable fields in place.
		updated := existing.DeepCopy()
		spec := r.buildDeploymentSpec(cr, info)
		spec.Selector = existing.Spec.Selector // selector is immutable, preserve it
		updated.Spec = spec
		if err := controllerutil.SetControllerReference(cr, updated, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.Update(ctx, updated)
	}

	// Deployment does not exist — create it fresh.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace},
		Spec:       r.buildDeploymentSpec(cr, info),
	}
	if err := controllerutil.SetControllerReference(cr, deploy, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.Create(ctx, deploy)
}

func (r *AgentReconciler) reconcileService(ctx context.Context, cr *pulsev1alpha1.OpenShiftPulse) error {
	name := agentResourceName(cr.Name)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if err := controllerutil.SetControllerReference(cr, svc, r.Scheme); err != nil {
			return err
		}
		// The ServiceMonitor's selector matches on this Service's own labels
		// (not the pod selector below) — without this, Prometheus discovers
		// the Service but silently drops it as a scrape target.
		svc.Labels = map[string]string{"app": name}
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		svc.Spec.Selector = map[string]string{"app": name}
		svc.Spec.Ports = []corev1.ServicePort{
			{
				Name:     "http",
				Port:     agentPort,
				Protocol: corev1.ProtocolTCP,
			},
		}
		return nil
	})
	return err
}
