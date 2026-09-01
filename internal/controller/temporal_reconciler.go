// Temporal server for durable plan execution.
//
// The agent's plan engine gained a Temporal-backed execution path
// (pulse-agent docs/TEMPORAL.md): runs survive pod restarts and
// approval_required phases can genuinely wait for a human. That path is inert
// without a Temporal server, and standing one up by hand is exactly the kind
// of unmanaged state this operator exists to remove.
//
// spec.temporal.enabled provisions the temporalio/auto-setup image — server
// plus schema setup in one container — pointed at the operator's own
// PostgreSQL. auto-setup creates the `temporal` and `temporal_visibility`
// databases in that instance on first start, so Temporal shares the
// StatefulSet the operator already manages instead of bringing its own
// stateful service. This is the dev-grade topology: one replica, one
// container. A production topology (separated services, ES visibility) is a
// deliberate later step, not a default.
//
// When enabled, the agent Deployment gets PULSE_AGENT_TEMPORAL_HOST pointing
// at the Service below (see agent_reconciler.go), which is the only wiring
// the agent needs.
package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// defaultTemporalImage is pinned rather than :latest — an unpinned server
// image would upgrade Temporal (and run schema migrations) on every pod
// reschedule, at a moment nobody chose.
const defaultTemporalImage = "temporalio/auto-setup:1.25.2"

const temporalFrontendPort = 7233

type TemporalReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder kept consistent with the other sub-reconcilers (see
	// MCPReconciler's comment) even though no event site exists here yet.
	Recorder record.EventRecorder
}

func temporalResourceName(crName string) string { return crName + "-temporal" }

// TemporalHostFor is what the agent should dial when Temporal is enabled.
func TemporalHostFor(crName string) string {
	return fmt.Sprintf("%s:%d", temporalResourceName(crName), temporalFrontendPort)
}

func temporalEnabled(pulse *pulsev1alpha1.OpenShiftPulse) bool {
	return pulse.Spec.Temporal.Enabled != nil && *pulse.Spec.Temporal.Enabled
}

func (r *TemporalReconciler) reconcileTemporal(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	if !temporalEnabled(pulse) {
		return nil
	}
	if err := r.reconcileDeployment(ctx, pulse); err != nil {
		return fmt.Errorf("temporal deployment: %w", err)
	}
	if err := r.reconcileService(ctx, pulse); err != nil {
		return fmt.Errorf("temporal service: %w", err)
	}
	return nil
}

func (r *TemporalReconciler) reconcileDeployment(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := temporalResourceName(pulse.Name)
	image := pulse.Spec.Temporal.Image
	if image == "" {
		image = defaultTemporalImage
	}

	// Reuses the operator's PostgreSQL: same service, same credentials secret
	// the agent's database wiring already depends on.
	pgService := pulse.Name + "-openshift-sre-agent-postgresql"
	pgAuthSecret := pulse.Name + "-pg-auth"

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pulse.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		if setErr := controllerutil.SetControllerReference(pulse, deploy, r.Scheme); setErr != nil {
			return setErr
		}
		one := int32(1)
		deploy.Spec.Replicas = &one
		deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}
		deploy.Spec.Template.Labels = map[string]string{"app": name}
		deploy.Spec.Template.Spec.SecurityContext = defaultPodSecCtx(nil)
		// auto-setup renders its config into /etc/temporal/config at startup.
		// In the image that directory is owned by UID 1000; OpenShift runs the
		// pod as an arbitrary UID, so the write fails (verified on dev05:
		// "unable to create open /etc/temporal/config/docker.yaml: permission
		// denied"). An init container copies the shipped templates into an
		// emptyDir, which any UID can write, and the server reads from there.
		deploy.Spec.Template.Spec.Volumes = []corev1.Volume{
			{Name: "temporal-config", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		}
		deploy.Spec.Template.Spec.InitContainers = []corev1.Container{
			{
				Name:            "config-template",
				Image:           image,
				Command:         []string{"sh", "-c", "cp -a /etc/temporal/config/. /config-work/"},
				SecurityContext: defaultContainerSecCtx(),
				VolumeMounts:    []corev1.VolumeMount{{Name: "temporal-config", MountPath: "/config-work"}},
			},
		}
		deploy.Spec.Template.Spec.Containers = []corev1.Container{
			{
				Name:  "temporal",
				Image: image,
				// auto-setup's entrypoint renders config under /etc/temporal at
				// startup, so the root filesystem must stay writable — the same
				// exemption PostgreSQL gets, for the same reason.
				SecurityContext: writableContainerSecCtx(),
				Env: []corev1.EnvVar{
					{Name: "DB", Value: "postgres12"},
					{Name: "DB_PORT", Value: "5432"},
					{Name: "POSTGRES_SEEDS", Value: pgService},
					{Name: "DBNAME", Value: "temporal"},
					{Name: "VISIBILITY_DBNAME", Value: "temporal_visibility"},
					{
						Name: "POSTGRES_USER",
						ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: pgAuthSecret},
							Key:                  "POSTGRESQL_USER",
						}},
					},
					{
						Name: "POSTGRES_PWD",
						ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: pgAuthSecret},
							Key:                  "POSTGRESQL_PASSWORD",
						}},
					},
					// Bind all services in the one container to the pod IP.
					{Name: "BIND_ON_IP", Value: "0.0.0.0"},
					{Name: "TEMPORAL_BROADCAST_ADDRESS", ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
					}},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "temporal-config", MountPath: "/etc/temporal/config"}},
				Ports:        []corev1.ContainerPort{{Name: "frontend", ContainerPort: temporalFrontendPort}},
				// Schema setup against a cold PostgreSQL can take a while on
				// first boot; a TCP check on the frontend is the honest signal
				// that the server actually came up.
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(temporalFrontendPort)},
					},
					InitialDelaySeconds: 10,
					PeriodSeconds:       10,
					FailureThreshold:    30,
				},
				LivenessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(temporalFrontendPort)},
					},
					InitialDelaySeconds: 120,
					PeriodSeconds:       30,
					FailureThreshold:    6,
				},
			},
		}
		return nil
	})
	return err
}

func (r *TemporalReconciler) reconcileService(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := temporalResourceName(pulse.Name)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pulse.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if setErr := controllerutil.SetControllerReference(pulse, svc, r.Scheme); setErr != nil {
			return setErr
		}
		svc.Spec.Selector = map[string]string{"app": name}
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: "frontend", Port: temporalFrontendPort, TargetPort: intstr.FromInt32(temporalFrontendPort)},
		}
		return nil
	})
	return err
}
