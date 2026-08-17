package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

const (
	defaultPGImage       = "registry.redhat.io/rhel9/postgresql-15:latest"
	defaultPGStorageSize = "5Gi"
	pgPort               = int32(5432)
	pgUser               = "pulse"
	pgDB                 = "pulse"
)

// PostgreSQLReconciler handles all PostgreSQL sub-resources for an OpenShiftPulse CR.
type PostgreSQLReconciler struct {
	client.Client
}

// reconcilePostgres ensures the Secret, StatefulSet, ClusterIP Service, and headless
// Service exist for the PostgreSQL instance backing the given pulse CR.
// It returns the DATABASE_URL string. The caller (OpenShiftPulseReconciler) is
// responsible for setting pulse.Status.DatabaseReady — PostgreSQLReconciler does
// not touch the status field to avoid double-writes with the root reconciler.
func (r *PostgreSQLReconciler) reconcilePostgres(
	ctx context.Context,
	pulse *v1alpha1.OpenShiftPulse,
) (string, error) {
	logger := log.FromContext(ctx)

	secretName := pulse.Name + "-pg-auth"
	svcName := pulse.Name + "-openshift-sre-agent-postgresql"
	stsName := svcName

	// 1. Secret
	password, err := r.reconcilePGSecret(ctx, pulse, secretName)
	if err != nil {
		return "", fmt.Errorf("pg secret: %w", err)
	}
	logger.V(1).Info("pg secret ready", "secret", secretName)

	// 2. StatefulSet
	if err := r.reconcilePGStatefulSet(ctx, pulse, stsName, secretName); err != nil {
		return "", fmt.Errorf("pg statefulset: %w", err)
	}
	logger.V(1).Info("pg statefulset ready", "sts", stsName)

	// 2a. Unblock rolling update: the StatefulSet controller does not delete
	// Pending pods when the pod template changes — it waits for the pod to be
	// Running first, which never happens when scheduling fails. Delete the pod
	// so the StatefulSet controller creates a fresh pod with the updated spec.
	if err := r.deletePendingPGPodIfStale(ctx, pulse, stsName); err != nil {
		logger.Error(err, "failed to delete stale pending PG pod", "sts", stsName)
		// Non-fatal — next reconcile will retry.
	}

	// 3. ClusterIP Service
	if err := r.reconcilePGService(ctx, pulse, svcName, false); err != nil {
		return "", fmt.Errorf("pg service: %w", err)
	}

	// 4. Headless Service
	headlessSvcName := svcName + "-headless"
	if err := r.reconcilePGService(ctx, pulse, headlessSvcName, true); err != nil {
		return "", fmt.Errorf("pg headless service: %w", err)
	}

	dbURL := fmt.Sprintf("postgresql://%s:%s@%s:5432/%s", pgUser, password, svcName, pgDB)

	// Create/update the {name}-postgresql secret with the database-url key so the
	// agent Deployment can reference it via secretKeyRef without the URL in plain env vars.
	connSecret := &corev1.Secret{}
	connSecretName := pulse.Name + "-postgresql"
	err = r.Get(ctx, types.NamespacedName{Namespace: pulse.Namespace, Name: connSecretName}, connSecret)
	if errors.IsNotFound(err) {
		connSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: connSecretName, Namespace: pulse.Namespace, Labels: pgLabels(pulse.Name)},
			Type:       corev1.SecretTypeOpaque,
			StringData: map[string]string{"database-url": dbURL},
		}
		setOwner(pulse, connSecret)
		if createErr := r.Create(ctx, connSecret); createErr != nil {
			return "", fmt.Errorf("create pg connection secret: %w", createErr)
		}
	} else if err != nil {
		return "", err
	}

	return dbURL, nil
}

// reconcilePGSecret creates the pg-auth Secret if it does not exist and returns the password.
func (r *PostgreSQLReconciler) reconcilePGSecret(
	ctx context.Context,
	pulse *v1alpha1.OpenShiftPulse,
	secretName string,
) (string, error) {
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: pulse.Namespace, Name: secretName}, existing)
	if err == nil {
		// Already exists — return stored password. Migrate stale key names in-place
		// so the StatefulSet picks up the correct POSTGRESQL_* env vars on next restart.
		if _, ok := existing.Data["POSTGRESQL_PASSWORD"]; ok {
			return string(existing.Data["POSTGRESQL_PASSWORD"]), nil
		}
		// Legacy secret has POSTGRES_* keys — migrate to POSTGRESQL_* without rotating password.
		if pw, ok := existing.Data["POSTGRES_PASSWORD"]; ok {
			existing.Data["POSTGRESQL_USER"] = existing.Data["POSTGRES_USER"]
			existing.Data["POSTGRESQL_PASSWORD"] = pw
			existing.Data["POSTGRESQL_DATABASE"] = existing.Data["POSTGRES_DB"]
			delete(existing.Data, "POSTGRES_USER")
			delete(existing.Data, "POSTGRES_PASSWORD")
			delete(existing.Data, "POSTGRES_DB")
			if updateErr := r.Update(ctx, existing); updateErr != nil {
				return "", fmt.Errorf("migrate pg-auth secret keys: %w", updateErr)
			}
			return string(pw), nil
		}
		return string(existing.Data["POSTGRESQL_PASSWORD"]), nil
	}
	if !errors.IsNotFound(err) {
		return "", err
	}

	password, err := generatePassword(24)
	if err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: pulse.Namespace,
			Labels:    pgLabels(pulse.Name),
		},
		Type: corev1.SecretTypeOpaque,
		// UBI postgresql-15 reads POSTGRESQL_* (RHSCL convention), not POSTGRES_* (upstream Docker Hub).
		StringData: map[string]string{
			"POSTGRESQL_USER":     pgUser,
			"POSTGRESQL_PASSWORD": password,
			"POSTGRESQL_DATABASE": pgDB,
		},
	}
	setOwner(pulse, secret)
	if err := r.Create(ctx, secret); err != nil {
		return "", err
	}
	return password, nil
}

// reconcilePGStatefulSet creates or updates the PostgreSQL StatefulSet.
// VolumeClaimTemplates are immutable after creation and are only set on the create path.
func (r *PostgreSQLReconciler) reconcilePGStatefulSet(
	ctx context.Context,
	pulse *v1alpha1.OpenShiftPulse,
	stsName, secretName string,
) error {
	image := pulse.Spec.Database.Image
	if image == "" {
		image = defaultPGImage
	}
	storageSize := pulse.Spec.Database.StorageSize
	if storageSize == "" {
		storageSize = defaultPGStorageSize
	}
	storageQty, err := resource.ParseQuantity(storageSize)
	if err != nil {
		return fmt.Errorf("parse storageSize %q: %w", storageSize, err)
	}

	replicas := int32(1)

	pgProbeHandler := corev1.ProbeHandler{
		Exec: &corev1.ExecAction{
			Command: []string{"pg_isready", "-U", pgUser},
		},
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      stsName,
			Namespace: pulse.Namespace,
		},
	}

	// If the existing StatefulSet has a selector that doesn't match the operator's labels
	// (e.g. it was previously managed by Helm), delete it so the create path runs cleanly.
	// Use Background propagation so the pod is also deleted — Orphan leaves postgresql-0
	// running, which prevents the new STS from ever creating its pod (AlreadyExists).
	// PVCs survive because they are managed by the STS volumeClaimTemplate lifecycle.
	if err := r.Get(ctx, types.NamespacedName{Namespace: pulse.Namespace, Name: stsName}, sts); err == nil {
		// If the STS is already terminating, return an error so controller-runtime
		// retries shortly — the watch will also re-trigger when deletion completes.
		if sts.DeletionTimestamp != nil {
			return fmt.Errorf("postgresql StatefulSet %s is terminating; waiting for deletion", stsName)
		}
		wantLabels := pgLabels(pulse.Name)
		if sts.Spec.Selector != nil {
			mismatch := false
			for k, v := range wantLabels {
				existing, ok := sts.Spec.Selector.MatchLabels[k]
				if !ok || existing != v {
					mismatch = true
					break
				}
			}
			if mismatch {
				if delErr := r.Delete(ctx, sts, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !errors.IsNotFound(delErr) {
					return fmt.Errorf("delete mismatched statefulset: %w", delErr)
				}
				return fmt.Errorf("postgresql StatefulSet %s deleted (selector mismatch); waiting for removal", stsName)
			}
		}
		sts = &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: pulse.Namespace}}
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Labels = pgLabels(pulse.Name)
		setOwner(pulse, sts)

		// VolumeClaimTemplates is immutable — only populate on creation.
		if sts.CreationTimestamp.IsZero() {
			sts.Spec.Replicas = &replicas
			sts.Spec.ServiceName = stsName + "-headless"
			sts.Spec.Selector = &metav1.LabelSelector{
				MatchLabels: pgLabels(pulse.Name),
			}
			sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "pg-data",
						Labels: pgLabels(pulse.Name),
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: storageQty,
							},
						},
					},
				},
			}
			if pulse.Spec.Database.StorageClass != "" {
				sc := pulse.Spec.Database.StorageClass
				sts.Spec.VolumeClaimTemplates[0].Spec.StorageClassName = &sc
			}
		}

		// Sync mutable fields on both create and update.
		sts.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: pgLabels(pulse.Name),
			},
			Spec: corev1.PodSpec{
				// OCP assigns UIDs from the namespace range via the restricted SCC.
			// Do not set RunAsUser — hardcoding 26 (postgres uid) is rejected by
			// restricted-v2 SCC which enforces namespace-allocated UID ranges.
			SecurityContext: defaultPodSecCtx(nil),
				Containers: []corev1.Container{
					{
						Name:  "postgresql",
						Image: image,
						Ports: []corev1.ContainerPort{
							{Name: "postgresql", ContainerPort: pgPort, Protocol: corev1.ProtocolTCP},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
						EnvFrom: []corev1.EnvFromSource{
							{
								SecretRef: &corev1.SecretEnvSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
								},
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "pg-data", MountPath: "/var/lib/pgsql/data"},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:        pgProbeHandler,
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
							FailureThreshold:    3,
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler:        pgProbeHandler,
							InitialDelaySeconds: 30,
							PeriodSeconds:       30,
						},
						SecurityContext: writableContainerSecCtx(),
					},
				},
			},
		}
		return nil
	})
	return err
}

// isReady returns true when the PostgreSQL StatefulSet has at least one ready replica.
func (r *PostgreSQLReconciler) isReady(ctx context.Context, name, ns string) bool {
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sts); err != nil {
		return false
	}
	return sts.Status.ReadyReplicas > 0
}

// reconcilePGService creates or updates either a ClusterIP or headless Service for PostgreSQL.
func (r *PostgreSQLReconciler) reconcilePGService(
	ctx context.Context,
	pulse *v1alpha1.OpenShiftPulse,
	svcName string,
	headless bool,
) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: pulse.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = pgLabels(pulse.Name)
		setOwner(pulse, svc)
		svc.Spec.Selector = pgLabels(pulse.Name)
		svc.Spec.Ports = []corev1.ServicePort{
			{
				Name:       "postgresql",
				Port:       pgPort,
				TargetPort: intstr.FromInt(int(pgPort)),
				Protocol:   corev1.ProtocolTCP,
			},
		}
		// ClusterIP is immutable after creation — only set on creation.
		if svc.Spec.ClusterIP == "" && headless {
			svc.Spec.ClusterIP = "None"
		}
		return nil
	})
	return err
}

// generatePassword returns a hex-encoded random string of n bytes (produces 2n hex chars).
// Caller requests n=24 which yields a 48-char hex string; we truncate to n chars.
func generatePassword(n int) (string, error) {
	// Use n/2 bytes so the hex output is exactly n chars (round up for odd n).
	byteLen := (n + 1) / 2
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b)[:n], nil
}

// pgLabels returns the standard label set for all PostgreSQL sub-resources.
func pgLabels(crName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "postgresql",
		"app.kubernetes.io/instance":   crName,
		"app.kubernetes.io/component":  "database",
		"app.kubernetes.io/managed-by": "pulse-operator",
	}
}

// setOwner sets a non-blocking owner annotation (no controller OwnerReference since
// cross-namespace ownership is not supported and CR/GVK is not available here without scheme).
// Callers that have access to the scheme should call controllerutil.SetControllerReference instead.
// deletePendingPGPodIfStale deletes the ordinal-0 PostgreSQL pod when it is
// Pending. The StatefulSet RollingUpdate controller waits for pod-0 to be
// Running before rolling it — a pod stuck Pending (e.g. due to stale resource
// requests that were later updated) blocks the rollout indefinitely.
// Deleting a Pending pod is safe: it never served traffic, so there's no
// disruption. The StatefulSet controller recreates it with the current template.
func (r *PostgreSQLReconciler) deletePendingPGPodIfStale(
	ctx context.Context,
	pulse *v1alpha1.OpenShiftPulse,
	stsName string,
) error {
	podName := stsName + "-0"
	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Namespace: pulse.Namespace, Name: podName}, pod)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if pod.Status.Phase != corev1.PodPending {
		return nil
	}
	return r.Delete(ctx, pod)
}

func setOwner(pulse *v1alpha1.OpenShiftPulse, obj metav1.Object) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations["pulse-operator.io/owner"] = pulse.Namespace + "/" + pulse.Name
	obj.SetAnnotations(annotations)
}
