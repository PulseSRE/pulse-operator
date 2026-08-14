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
	"sigs.k8s.io/controller-runtime/pkg/log"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

const (
	defaultAgentImage = "quay.io/amobrem/pulse-agent:latest"
	agentPort         = int32(8080)

	annotationOwnerName      = "pulse.ai/owner-name"
	annotationOwnerNamespace = "pulse.ai/owner-namespace"
	annotationOwnerUID       = "pulse.ai/owner-uid"
)

// AgentReconciler reconciles the agent sub-resources of an OpenShiftPulse CR.
type AgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pulse.ai,resources=openshiftpulses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pulse.ai,resources=openshiftpulses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts;secrets;persistentvolumeclaims;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete

// Reconcile creates or updates all agent sub-resources when an OpenShiftPulse CR exists.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cr := &pulsev1alpha1.OpenShiftPulse{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling OpenShiftPulse", "name", cr.Name, "namespace", cr.Namespace)

	type step struct {
		name string
		fn   func(context.Context, *pulsev1alpha1.OpenShiftPulse) error
	}

	steps := []step{
		{"ServiceAccount", r.reconcileServiceAccount},
		{"ClusterRole", r.reconcileClusterRole},
		{"ClusterRoleBinding", r.reconcileClusterRoleBinding},
		{"WSTokenSecret", r.reconcileWSTokenSecret},
		{"MemoryPVC", r.reconcileMemoryPVC},
	}

	for _, s := range steps {
		if err := s.fn(ctx, cr); err != nil {
			logger.Error(err, "reconcile step failed", "resource", s.name)
			return ctrl.Result{}, err
		}
	}

	// Wait for the memory PVC to be Bound before creating the Deployment.
	// The Deployment uses Recreate strategy and mounts the RWO PVC — it won't
	// schedule until the volume is provisioned.
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Name: memoryPVCName(cr.Name), Namespace: cr.Namespace}, pvc); err != nil {
		return ctrl.Result{}, err
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		logger.Info("Memory PVC not yet Bound — requeuing", "pvc", pvc.Name, "phase", pvc.Status.Phase)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	postPVCSteps := []step{
		{"Deployment", r.reconcileDeployment},
		{"Service", r.reconcileService},
	}
	for _, s := range postPVCSteps {
		if err := s.fn(ctx, cr); err != nil {
			logger.Error(err, "reconcile step failed", "resource", s.name)
			return ctrl.Result{}, err
		}
	}

	logger.Info("Reconcile complete", "name", cr.Name)
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pulsev1alpha1.OpenShiftPulse{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}

// ─── helpers ────────────────────────────────────────────────────────────────

func agentResourceName(crName string) string {
	return crName + "-openshift-sre-agent"
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

func clusterScopedAnnotations(cr *pulsev1alpha1.OpenShiftPulse) map[string]string {
	return map[string]string{
		annotationOwnerName:      cr.Name,
		annotationOwnerNamespace: cr.Namespace,
		annotationOwnerUID:       string(cr.UID),
	}
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
	name := agentResourceName(cr.Name)
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
					"services", "services/proxy", "namespaces", "configmaps",
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
	name := agentResourceName(cr.Name)
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
				Name:      name,
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
	isNonRoot := true
	runAsUser := int64(1001)

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
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot: &isNonRoot,
					RunAsUser:    &runAsUser,
				},
				Containers: []corev1.Container{
					{
						Name:      "agent",
						Image:     resolvedImage(cr),
						Resources: cr.Spec.Agent.Resources,
						Ports: []corev1.ContainerPort{
							{
								Name:          "http",
								ContainerPort: agentPort,
								Protocol:      corev1.ProtocolTCP,
							},
						},
						Env: envVars,
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "memory",
								MountPath: "/memory",
							},
						},
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
				Volumes: []corev1.Volume{
					{
						Name: "memory",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: memoryPVCName(cr.Name),
							},
						},
					},
				},
			},
		},
	}
}

func (r *AgentReconciler) reconcileDeployment(ctx context.Context, cr *pulsev1alpha1.OpenShiftPulse) error {
	info := DetectClusterInfo(ctx, r.Client)
	name := agentResourceName(cr.Name)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		if err := controllerutil.SetControllerReference(cr, deploy, r.Scheme); err != nil {
			return err
		}
		deploy.Spec = r.buildDeploymentSpec(cr, info)
		return nil
	})
	return err
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

// isReady returns true when the agent Deployment has at least one ready replica.
func (r *AgentReconciler) isReady(ctx context.Context, name, ns string) bool {
	deploy := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, deploy); err != nil {
		return false
	}
	return deploy.Status.ReadyReplicas > 0
}
