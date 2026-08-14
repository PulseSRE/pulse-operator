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
// It returns the DATABASE_URL string and updates pulse.Status.DatabaseReady.
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

	// 3. ClusterIP Service
	if err := r.reconcilePGService(ctx, pulse, svcName, false); err != nil {
		return "", fmt.Errorf("pg service: %w", err)
	}

	// 4. Headless Service
	headlessSvcName := svcName + "-headless"
	if err := r.reconcilePGService(ctx, pulse, headlessSvcName, true); err != nil {
		return "", fmt.Errorf("pg headless service: %w", err)
	}

	// 5. Reflect readiness into status
	ready, err := r.isPGReady(ctx, pulse.Namespace, stsName)
	if err != nil {
		logger.V(1).Info("could not determine pg readiness", "err", err)
	}
	pulse.Status.DatabaseReady = ready

	dbURL := fmt.Sprintf("postgresql://%s:%s@%s:5432/%s", pgUser, password, svcName, pgDB)
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
		// Already exists — return stored password without overwriting.
		return string(existing.Data["POSTGRES_PASSWORD"]), nil
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
		StringData: map[string]string{
			"POSTGRES_USER":     pgUser,
			"POSTGRES_PASSWORD": password,
			"POSTGRES_DB":       pgDB,
		},
	}
	setOwner(pulse, secret)
	if err := r.Create(ctx, secret); err != nil {
		return "", err
	}
	return password, nil
}

// reconcilePGStatefulSet creates the PostgreSQL StatefulSet if it does not exist.
func (r *PostgreSQLReconciler) reconcilePGStatefulSet(
	ctx context.Context,
	pulse *v1alpha1.OpenShiftPulse,
	stsName, secretName string,
) error {
	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Namespace: pulse.Namespace, Name: stsName}, existing)
	if err == nil {
		return nil // Already exists — no patching; operator manages creation only.
	}
	if !errors.IsNotFound(err) {
		return err
	}

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
	isTrue := true
	isReadOnly := false

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      stsName,
			Namespace: pulse.Namespace,
			Labels:    pgLabels(pulse.Name),
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: stsName + "-headless",
			Selector: &metav1.LabelSelector{
				MatchLabels: pgLabels(pulse.Name),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: pgLabels(pulse.Name),
				},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &isTrue,
					},
					Containers: []corev1.Container{
						{
							Name:  "postgresql",
							Image: image,
							Ports: []corev1.ContainerPort{
								{Name: "postgresql", ContainerPort: pgPort, Protocol: corev1.ProtocolTCP},
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
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"pg_isready", "-U", pgUser, "-d", pgDB},
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
								FailureThreshold:    3,
							},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: &isReadOnly,
								ReadOnlyRootFilesystem:   &isReadOnly,
							},
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
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
			},
		},
	}

	if pulse.Spec.Database.StorageClass != "" {
		sc := pulse.Spec.Database.StorageClass
		sts.Spec.VolumeClaimTemplates[0].Spec.StorageClassName = &sc
	}

	setOwner(pulse, sts)
	return r.Create(ctx, sts)
}

// reconcilePGService creates either a ClusterIP or headless Service for PostgreSQL.
func (r *PostgreSQLReconciler) reconcilePGService(
	ctx context.Context,
	pulse *v1alpha1.OpenShiftPulse,
	svcName string,
	headless bool,
) error {
	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Namespace: pulse.Namespace, Name: svcName}, existing)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	clusterIP := ""
	if headless {
		clusterIP = "None"
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: pulse.Namespace,
			Labels:    pgLabels(pulse.Name),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: clusterIP,
			Selector:  pgLabels(pulse.Name),
			Ports: []corev1.ServicePort{
				{
					Name:       "postgresql",
					Port:       pgPort,
					TargetPort: intstr.FromInt(int(pgPort)),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}

	setOwner(pulse, svc)
	return r.Create(ctx, svc)
}

// isPGReady returns true when the StatefulSet has at least one ready replica.
func (r *PostgreSQLReconciler) isPGReady(ctx context.Context, namespace, stsName string) (bool, error) {
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: stsName}, sts); err != nil {
		return false, err
	}
	return sts.Status.ReadyReplicas > 0, nil
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
func setOwner(pulse *v1alpha1.OpenShiftPulse, obj metav1.Object) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations["pulse-operator.io/owner"] = pulse.Namespace + "/" + pulse.Name
	obj.SetAnnotations(annotations)
}
