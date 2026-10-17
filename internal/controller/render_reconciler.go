// Render service: lets the agent's view designer see a draft dashboard.
//
// The agent's preview_dashboard tool (pulse-agent sre_agent/view_render.py)
// POSTs a draft to this service. The service loads the UI's /preview page in a
// headless browser with the requesting user's OpenShift token, then returns a
// screenshot (pulse-ui render/README.md).
//
// The security model is what this file mostly encodes:
//
//   - The service holds no cluster credentials. Its pod mounts no
//     ServiceAccount token, and widget data comes back with the user's token,
//     which the agent supplies per request.
//   - Only the agent may call it. Ingress is allowed from the agent pod only,
//     and requests need RENDER_TOKEN, which only the agent is given.
//   - It may reach only the UI's render listener (uiRenderPort). That
//     listener serves the static UI and the /api/kubernetes/, /api/prometheus/
//     and /api/alertmanager/ proxies, which authenticate the forwarded user
//     token upstream. It does not serve /api/agent/: the main listener's agent
//     route adds the agent's own credential and trusts X-Forwarded-User, so a
//     caller that reached it could act as any user against the agent. Egress
//     from the render pod is limited to that one port, plus DNS.
package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

const (
	defaultRenderImage = "quay.io/amobrem/openshiftpulse-render:latest"
	renderPort         = int32(8090)
	// uiRenderPort is the UI pod's render-only nginx listener (see
	// reconcileUINginxConfigMap). Present only while render is enabled.
	uiRenderPort = int32(8081)
	// The default the service uses too; set here so the two cannot drift.
	renderTimeoutMS = "30000"
)

type RenderReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func renderEnabled(pulse *pulsev1alpha1.OpenShiftPulse) bool {
	return pulse.Spec.Render.Enabled != nil && *pulse.Spec.Render.Enabled
}

func renderResourceName(crName string) string { return crName + "-render" }

func renderTokenSecretName(crName string) string { return crName + "-render-token" }

// RenderServiceURL is what the agent dials (PULSE_AGENT_RENDER_URL).
func RenderServiceURL(crName string) string {
	return fmt.Sprintf("http://%s:%d", renderResourceName(crName), renderPort)
}

// renderUIURL is the UI render listener, reached through the UI Service.
func renderUIURL(crName string) string {
	return fmt.Sprintf("http://%s:%d", uiResourceName(crName), uiRenderPort)
}

func resolvedRenderImage(pulse *pulsev1alpha1.OpenShiftPulse) string {
	if pulse.Spec.Render.Image != "" {
		return pulse.Spec.Render.Image
	}
	return defaultRenderImage
}

func renderResources(pulse *pulsev1alpha1.OpenShiftPulse) corev1.ResourceRequirements {
	res := pulse.Spec.Render.Resources
	if len(res.Requests) == 0 && len(res.Limits) == 0 {
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		}
	}
	return res
}

// reconcileRender creates the render service when enabled and removes it when
// not, so turning it off also takes the browser and its network path away.
func (r *RenderReconciler) reconcileRender(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	if !renderEnabled(pulse) {
		return r.removeRender(ctx, pulse)
	}
	// The token Secret first: both the render pod and the agent pod read it.
	if err := r.reconcileTokenSecret(ctx, pulse); err != nil {
		return fmt.Errorf("render token: %w", err)
	}
	if err := r.reconcileDeployment(ctx, pulse); err != nil {
		return fmt.Errorf("render deployment: %w", err)
	}
	if err := r.reconcileService(ctx, pulse); err != nil {
		return fmt.Errorf("render service: %w", err)
	}
	if err := r.reconcileNetworkPolicy(ctx, pulse); err != nil {
		return fmt.Errorf("render network policy: %w", err)
	}
	return nil
}

func (r *RenderReconciler) removeRender(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	meta := metav1.ObjectMeta{Name: renderResourceName(pulse.Name), Namespace: pulse.Namespace}
	for _, obj := range []client.Object{
		&appsv1.Deployment{ObjectMeta: meta},
		&corev1.Service{ObjectMeta: meta},
		&networkingv1.NetworkPolicy{ObjectMeta: meta},
		// Nothing else reads it once render is off; a later enable mints a new one.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: renderTokenSecretName(pulse.Name), Namespace: pulse.Namespace}},
	} {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("remove render %T: %w", obj, err)
		}
	}
	return nil
}

// reconcileTokenSecret creates the shared agent-to-render token once and then
// leaves it alone, like the WebSocket token (reconcileWSTokenSecret).
func (r *RenderReconciler) reconcileTokenSecret(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := renderTokenSecretName(pulse.Name)
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: pulse.Namespace}, existing)
	if err == nil || !apierrors.IsNotFound(err) {
		return err
	}
	token, err := generateToken()
	if err != nil {
		return err
	}
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pulse.Namespace},
		StringData: map[string]string{"token": token},
	}
	if err := controllerutil.SetControllerReference(pulse, desired, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, desired)
}

func (r *RenderReconciler) reconcileDeployment(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := renderResourceName(pulse.Name)
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pulse.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		if setErr := controllerutil.SetControllerReference(pulse, deploy, r.Scheme); setErr != nil {
			return setErr
		}
		one := int32(1)
		noToken := false
		deploy.Spec.Replicas = &one
		deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}
		deploy.Spec.Template.Labels = map[string]string{"app": name}
		spec := &deploy.Spec.Template.Spec
		spec.SecurityContext = defaultPodSecCtx(nil)
		// No cluster credentials in this pod, by construction: it renders
		// with the token each request carries and nothing else.
		spec.AutomountServiceAccountToken = &noToken
		spec.Volumes = []corev1.Volume{
			// Chromium writes its profile and crash dumps under HOME=/tmp, and
			// needs more shared memory than the default 64Mi /dev/shm.
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "dshm", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
		}
		probe := func(initial int32) *corev1.Probe {
			return &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(renderPort)},
				},
				InitialDelaySeconds: initial,
				PeriodSeconds:       10,
			}
		}
		spec.Containers = []corev1.Container{{
			Name:            "render",
			Image:           resolvedRenderImage(pulse),
			SecurityContext: defaultContainerSecCtx(),
			Resources:       renderResources(pulse),
			Env: []corev1.EnvVar{
				{Name: "PORT", Value: fmt.Sprintf("%d", renderPort)},
				{Name: "UI_URL", Value: renderUIURL(pulse.Name)},
				{Name: "MAX_CONCURRENT", Value: "2"},
				{Name: "RENDER_TIMEOUT_MS", Value: renderTimeoutMS},
				{
					Name: "RENDER_TOKEN",
					ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: renderTokenSecretName(pulse.Name)},
						Key:                  "token",
					}},
				},
			},
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: renderPort, Protocol: corev1.ProtocolTCP}},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "tmp", MountPath: "/tmp"},
				{Name: "dshm", MountPath: "/dev/shm"},
			},
			ReadinessProbe: probe(5),
			LivenessProbe:  probe(20),
		}}
		return nil
	})
	return err
}

func (r *RenderReconciler) reconcileService(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := renderResourceName(pulse.Name)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pulse.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if setErr := controllerutil.SetControllerReference(pulse, svc, r.Scheme); setErr != nil {
			return setErr
		}
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		svc.Spec.Selector = map[string]string{"app": name}
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: "http", Port: renderPort, TargetPort: intstr.FromInt32(renderPort), Protocol: corev1.ProtocolTCP},
		}
		return nil
	})
	return err
}

// reconcileNetworkPolicy: only the agent may call the render pod, and the
// render pod may call only the UI's render listener (and DNS to find it).
// Egress matters here as much as ingress: the pod runs a browser, and nothing
// it loads should be able to reach the agent, the database or the API server.
func (r *RenderReconciler) reconcileNetworkPolicy(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := renderResourceName(pulse.Name)
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	port := intstr.FromInt32(renderPort)
	uiPort := intstr.FromInt32(uiRenderPort)
	// 53 for clusters with standard DNS; 5353 is where OpenShift's dns-default
	// pods actually listen behind the 53 Service port.
	dns53, dns5353 := intstr.FromInt(53), intstr.FromInt(5353)

	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pulse.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Spec.PodSelector = metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}
		np.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}
		np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": agentResourceName(pulse.Name)}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
		}}
		np.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{
			{
				To: []networkingv1.NetworkPolicyPeer{{
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": uiResourceName(pulse.Name)}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &uiPort}},
			},
			{
				To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}},
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &udp, Port: &dns53}, {Protocol: &tcp, Port: &dns53},
					{Protocol: &udp, Port: &dns5353}, {Protocol: &tcp, Port: &dns5353},
				},
			},
		}
		return controllerutil.SetControllerReference(pulse, np, r.Scheme)
	})
	return err
}
