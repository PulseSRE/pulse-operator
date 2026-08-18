package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	goerrors "errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

	"github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

const (
	defaultPGImage       = "registry.redhat.io/rhel9/postgresql-15:latest"
	defaultPGStorageSize = "5Gi"
	pgPort               = int32(5432)
	pgUser               = "pulse"
	pgDB                 = "pulse"

	// pgRequeueDelay is used when a reconcile step needs a short requeue
	// rather than an error (e.g. waiting for a selector-mismatched
	// StatefulSet to finish terminating before it can be recreated).
	pgRequeueDelay = 5 * time.Second
)

// PostgreSQLReconciler handles all PostgreSQL sub-resources for an OpenShiftPulse CR.
type PostgreSQLReconciler struct {
	client.Client
	// Scheme is required to set a real OwnerReference on every PostgreSQL
	// sub-resource so Kubernetes garbage-collects them when the CR is deleted.
	Scheme *runtime.Scheme
}

// reconcilePostgres ensures the Secret, StatefulSet, ClusterIP Service, and headless
// Service exist for the PostgreSQL instance backing the given pulse CR.
// It returns the DATABASE_URL string. The caller (OpenShiftPulseReconciler) is
// responsible for setting pulse.Status.DatabaseReady — PostgreSQLReconciler does
// not touch the status field to avoid double-writes with the root reconciler.
// Returns a non-zero ctrl.Result.RequeueAfter (with a nil error) when a step
// needs a short requeue rather than a failure — see reconcilePGStatefulSet.
func (r *PostgreSQLReconciler) reconcilePostgres(
	ctx context.Context,
	pulse *v1alpha1.OpenShiftPulse,
) (string, ctrl.Result, error) {
	logger := log.FromContext(ctx)

	secretName := pulse.Name + "-pg-auth"
	svcName := pulse.Name + "-openshift-sre-agent-postgresql"
	stsName := svcName

	// 1. Secret
	password, err := r.reconcilePGSecret(ctx, pulse, secretName)
	if err != nil {
		return "", ctrl.Result{}, fmt.Errorf("pg secret: %w", err)
	}
	logger.V(1).Info("pg secret ready", "secret", secretName)

	// 2. StatefulSet
	stsResult, err := r.reconcilePGStatefulSet(ctx, pulse, stsName, secretName)
	if err != nil {
		return "", ctrl.Result{}, fmt.Errorf("pg statefulset: %w", err)
	}
	if stsResult.RequeueAfter > 0 {
		// Waiting for a terminating/being-recreated StatefulSet — every step
		// below depends on it existing, so stop here rather than erroring.
		return "", stsResult, nil
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
		return "", ctrl.Result{}, fmt.Errorf("pg service: %w", err)
	}

	// 4. Headless Service
	headlessSvcName := svcName + "-headless"
	if err := r.reconcilePGService(ctx, pulse, headlessSvcName, true); err != nil {
		return "", ctrl.Result{}, fmt.Errorf("pg headless service: %w", err)
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
		if setErr := controllerutil.SetControllerReference(pulse, connSecret, r.Scheme); setErr != nil {
			return "", ctrl.Result{}, fmt.Errorf("set owner on pg connection secret: %w", setErr)
		}
		if createErr := r.Create(ctx, connSecret); createErr != nil {
			return "", ctrl.Result{}, fmt.Errorf("create pg connection secret: %w", createErr)
		}
	} else if err != nil {
		return "", ctrl.Result{}, err
	} else if string(connSecret.Data["database-url"]) != dbURL {
		// The underlying pg-auth credentials changed (e.g. the legacy
		// POSTGRES_*->POSTGRESQL_* migration path in reconcilePGSecret ran)
		// but this connection secret was create-only, so database-url never
		// got refreshed to match — the agent would keep using a stale URL
		// indefinitely. Patch just this key; nothing else in the secret changes.
		connSecret.StringData = map[string]string{"database-url": dbURL}
		if updateErr := r.Update(ctx, connSecret); updateErr != nil {
			return "", ctrl.Result{}, fmt.Errorf("update pg connection secret: %w", updateErr)
		}
	}

	return dbURL, ctrl.Result{}, nil
}

// reconcilePGSecret creates the pg-auth Secret if it does not exist and returns the password.
//
// Deliberately NOT given an OwnerReference to pulse, unlike every other
// PostgreSQL sub-resource. The PG data PVC (from the StatefulSet's
// volumeClaimTemplates, see reconcilePGStatefulSet) has no retention policy
// set and is intentionally retained across CR deletion — see this package's
// README row for PostgreSQL. postgres only runs initdb (which is what bakes
// a password into PGDATA) on an empty data directory, so if this Secret were
// owned/GC'd and a fresh one got generated on CR recreation, the new random
// password would never match what's already on the retained volume: the
// agent would get permanent authentication failures with no self-heal short
// of an operator manually deleting the PVC. Leaving this Secret unowned
// means reconcilePGSecret's "already exists — return stored password" branch
// above naturally reuses the matching credentials whenever the CR is
// recreated with the same name, keeping data and credentials as a pair with
// the same lifetime. See deletePGDataOnRequest for the explicit opt-in path
// that deletes both together when a real teardown is actually wanted.
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
	if err := r.Create(ctx, secret); err != nil {
		return "", err
	}
	return password, nil
}

// reconcilePGStatefulSet creates or updates the PostgreSQL StatefulSet.
// VolumeClaimTemplates are immutable after creation and are only set on the create path.
// Returns a non-zero ctrl.Result.RequeueAfter (with a nil error), not an
// error, when it is waiting for a selector-mismatched StatefulSet to finish
// terminating before it can be recreated: this is an expected step of a
// normal migration (e.g. adopting a previously Helm-managed instance), and
// returning it as an error used to surface as a Warning ReconcileFailed
// event with exponential backoff, making a healthy self-heal look like a
// broken operator.
func (r *PostgreSQLReconciler) reconcilePGStatefulSet(
	ctx context.Context,
	pulse *v1alpha1.OpenShiftPulse,
	stsName, secretName string,
) (ctrl.Result, error) {
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
		return ctrl.Result{}, fmt.Errorf("parse storageSize %q: %w", storageSize, err)
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
		// If the STS is already terminating, requeue shortly — the watch will
		// also re-trigger when deletion completes, but a short explicit
		// requeue avoids relying on that alone.
		if sts.DeletionTimestamp != nil {
			return ctrl.Result{RequeueAfter: pgRequeueDelay}, nil
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
					return ctrl.Result{}, fmt.Errorf("delete mismatched statefulset: %w", delErr)
				}
				return ctrl.Result{RequeueAfter: pgRequeueDelay}, nil
			}
		}
		sts = &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: pulse.Namespace}}
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Labels = pgLabels(pulse.Name)
		if setErr := controllerutil.SetControllerReference(pulse, sts, r.Scheme); setErr != nil {
			return setErr
		}

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

		// Sync mutable fields on both create and update — but only rebuild
		// Template when something we actually manage has drifted (in
		// practice, just the image; everything else here is a hardcoded
		// constant). Unconditionally reassigning the whole PodTemplateSpec
		// on every reconcile — even when the desired state is byte-for-byte
		// identical to last time — discards fields the API server had
		// defaulted onto the stored object (imagePullPolicy,
		// terminationMessagePath, probe defaults, etc.), since those are
		// zero-valued in this literal. CreateOrUpdate's DeepEqual check then
		// always sees a diff and always issues an Update, forever, which (a)
		// repeatedly races the StatefulSet controller's own concurrent
		// status writes — the "object has been modified" conflict loop —
		// and (b) was observed to make the StatefulSet controller treat
		// every single one of those Updates as a genuine template change,
		// continuously rolling pod-0.
		existingImage := ""
		if len(sts.Spec.Template.Spec.Containers) > 0 {
			existingImage = sts.Spec.Template.Spec.Containers[0].Image
		}
		if sts.CreationTimestamp.IsZero() || existingImage != image {
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
		}
		return nil
	})
	return ctrl.Result{}, err
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
		if setErr := controllerutil.SetControllerReference(pulse, svc, r.Scheme); setErr != nil {
			return setErr
		}
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

// generatePassword returns a hex-encoded random string exactly n characters
// long. byteLen is sized (rounding up for odd n) so the hex encoding is
// already exactly n chars — no wasted entropy from over-generating and
// truncating. The final [:n] is a no-op for even n; it only trims the one
// extra hex digit that rounding up produces for odd n.
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

// annotationDeleteData is an explicit, deliberate opt-in: set to "true" on
// the OpenShiftPulse CR *before* deleting it to also delete the pg-auth
// Secret and the PostgreSQL data PVC, which otherwise survive CR deletion by
// design (see reconcilePGSecret's doc comment). Without this annotation,
// deleting and recreating a CR with the same name reuses the existing data
// and matching credentials; with it, deletion is a real, full teardown.
const annotationDeleteData = "pulse.ai/delete-data"

// deletePGDataOnRequest deletes the pg-auth Secret and the PostgreSQL data
// PVC when pulse carries annotationDeleteData=="true" — the explicit opt-in
// path for a real teardown (see annotationDeleteData's doc comment). A no-op
// otherwise. Called from the finalizer, before the StatefulSet's own
// OwnerReference-driven garbage collection removes everything else.
func (r *PostgreSQLReconciler) deletePGDataOnRequest(ctx context.Context, pulse *v1alpha1.OpenShiftPulse) error {
	if pulse.Annotations[annotationDeleteData] != "true" {
		return nil
	}

	stsName := pulse.Name + "-openshift-sre-agent-postgresql"
	secretName := pulse.Name + "-pg-auth"
	// StatefulSet volumeClaimTemplate PVCs are named
	// "<claimTemplateName>-<statefulSetName>-<ordinal>" — see the "pg-data"
	// claim template in reconcilePGStatefulSet; replicas is always 1, so
	// there is only ever ordinal 0.
	pvcName := "pg-data-" + stsName + "-0"

	var errs []error
	if err := r.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: pulse.Namespace}}); err != nil && !errors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete pg-auth secret: %w", err))
	}
	if err := r.Delete(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: pulse.Namespace}}); err != nil && !errors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete pg-data PVC: %w", err))
	}
	return goerrors.Join(errs...)
}

// pgPendingPodStaleThreshold is how long pod-0 must have been Pending before
// isPGPodStale considers it genuinely stuck rather than just mid-startup.
// See isPGPodStale's doc comment for why this grace period is required, not
// optional.
const pgPendingPodStaleThreshold = 2 * time.Minute

// isPGPodStale reports whether pod is a Pending pod that has been Pending
// for at least pgPendingPodStaleThreshold as of now. now is taken as a
// parameter (rather than calling time.Now() internally) so this can be unit
// tested without needing to fake a Pod's server-assigned, otherwise
// immutable CreationTimestamp.
//
// The grace period is load-bearing, not cosmetic: every pod is transiently
// Pending for a few seconds to tens of seconds during entirely normal
// scheduling/volume-attach/image-pull, and the caller runs on every
// reconcile of the owning CR. Without this grace period, on a cluster where
// reconciles happen more often than a pod's startup takes (e.g. because
// something else is churning watches), every fresh pod-0 got deleted before
// it ever reached Running — a self-inflicted, indefinite create-delete loop
// indistinguishable from the node/CNI instability it produces as a side
// effect (addLogicalPort/FailedMount errors racing a pod that's deleted
// mid-setup), rather than the one genuinely-stuck-rollout case this was
// written for.
func isPGPodStale(pod *corev1.Pod, now time.Time) bool {
	return pod.Status.Phase == corev1.PodPending &&
		now.Sub(pod.CreationTimestamp.Time) >= pgPendingPodStaleThreshold
}

// deletePendingPGPodIfStale deletes the ordinal-0 PostgreSQL pod when
// isPGPodStale reports it stuck. The StatefulSet RollingUpdate controller
// waits for pod-0 to be Running before rolling it — a pod stuck Pending
// (e.g. due to stale resource requests that were later updated) blocks the
// rollout indefinitely.
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
	if !isPGPodStale(pod, time.Now()) {
		return nil
	}
	return r.Delete(ctx, pod)
}
