package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

const (
	defaultUIImage  = "quay.io/amobrem/openshiftpulse:latest"
	uiHTTPPort = int32(8080)
	uiProxyPort = int32(8443)
)

// oauthClientName returns a CR-scoped OAuthClient name so multiple OpenShiftPulse
// CRs in different namespaces do not collide on the same cluster-scoped object.
func oauthClientName(crName, crNamespace string) string {
	return "openshiftpulse-" + crNamespace + "-" + crName
}

// UIReconciler reconciles the UI sub-resources of an OpenShiftPulse CR.
// It manages the nginx frontend Deployment, the OpenShift OAuth proxy sidecar,
// the Route, and the OAuthClient (cluster-scoped).
type UIReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pulse.ai,resources=openshiftpulses,verbs=get;list;watch
// +kubebuilder:rbac:groups=pulse.ai,resources=openshiftpulses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts;secrets;configmaps;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=oauth.openshift.io,resources=oauthclients,verbs=get;list;watch;create;update;patch;delete

// Reconcile implements reconcile.Reconciler for the UIReconciler.
// It can be registered as a standalone controller or called from the root reconciler.
func (r *UIReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	pulse := &pulsev1alpha1.OpenShiftPulse{}
	if err := r.Get(ctx, req.NamespacedName, pulse); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling UI sub-resources", "name", pulse.Name, "namespace", pulse.Namespace)

	result, err := r.reconcileUI(ctx, pulse)
	if err != nil {
		return ctrl.Result{}, err
	}

	if statusErr := r.Status().Update(ctx, pulse); statusErr != nil {
		logger.Error(statusErr, "failed to update UI status")
		return ctrl.Result{}, statusErr
	}

	return result, nil
}

// reconcileUI orchestrates all UI sub-resources in dependency order.
// Returns a requeue result when waiting for the OCP router to assign the Route hostname.
func (r *UIReconciler) reconcileUI(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	info := DetectClusterInfo(ctx, r.Client)

	type syncStep struct {
		name string
		fn   func(context.Context, *pulsev1alpha1.OpenShiftPulse) error
	}
	for _, s := range []syncStep{
		{"ServiceAccount", r.reconcileUIServiceAccount},
		{"ClusterRole", r.reconcileUIClusterRole},
		{"ClusterRoleBinding", r.reconcileUIClusterRoleBinding},
		{"OAuthSecrets", r.reconcileUIOAuthSecrets},
		{"NginxConfigMap", r.reconcileUINginxConfigMap},
		{"Deployment", r.reconcileUIDeployment},
		{"Service", r.reconcileUIService},
	} {
		if err := s.fn(ctx, pulse); err != nil {
			logger.Error(err, "UI reconcile step failed", "step", s.name)
			return ctrl.Result{}, fmt.Errorf("UI/%s: %w", s.name, err)
		}
	}

	// Route must be created before OAuthClient — OCP assigns the hostname asynchronously.
	routeHost, result, err := r.reconcileUIRoute(ctx, pulse, info)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("UI/Route: %w", err)
	}
	if result.RequeueAfter > 0 {
		// Route not yet admitted; requeue until hostname is assigned.
		logger.Info("Route hostname not yet assigned — requeuing", "name", pulse.Name)
		return result, nil
	}

	// Read the client-secret that was generated during OAuthSecrets reconcile.
	clientSecret, err := r.getOAuthClientSecret(ctx, pulse)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("UI/getOAuthClientSecret: %w", err)
	}

	// OAuthClient requires the Route hostname in its redirectURIs.
	if err := r.reconcileOAuthClient(ctx, pulse, routeHost, clientSecret); err != nil {
		return ctrl.Result{}, fmt.Errorf("UI/OAuthClient: %w", err)
	}

	// Reflect into status (caller is responsible for r.Status().Update).
	pulse.Status.RouteHost = routeHost
	pulse.Status.UIAvailable = true

	return ctrl.Result{}, nil
}

// ─── name helpers ────────────────────────────────────────────────────────────

func uiResourceName(crName string) string {
	return crName + "-openshiftpulse"
}

func uiClusterRoleName(crName string) string {
	return crName + "-openshiftpulse-reader"
}

func uiOAuthSecretsName(crName string) string {
	return crName + "-oauth-secrets"
}

func uiNginxConfigMapName(crName string) string {
	return crName + "-nginx"
}

func uiTLSSecretName(crName string) string {
	return crName + "-openshiftpulse-tls"
}

func resolvedUIImage(cr *pulsev1alpha1.OpenShiftPulse) string {
	if cr.Spec.UI.Image != "" {
		return cr.Spec.UI.Image
	}
	return defaultUIImage
}

func resolvedOAuthProxyImage(cr *pulsev1alpha1.OpenShiftPulse, info *ClusterInfo) string {
	oauthImage := cr.Spec.UI.OAuthProxyImage
	if oauthImage == "" && info != nil && info.OAuthProxyImage != "" {
		oauthImage = info.OAuthProxyImage
	}
	if oauthImage == "" {
		oauthImage = DefaultOAuthProxyImage
	}
	return oauthImage
}

// generateCookieSecret returns a base64-encoded string of 32 random bytes.
// The openshift oauth-proxy interprets --cookie-secret-file content as a base64 key.
func generateCookieSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// strMapToInterface converts map[string]string to map[string]interface{} for unstructured use.
func strMapToInterface(m map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ─── sub-reconcilers ─────────────────────────────────────────────────────────

// a. ServiceAccount
func (r *UIReconciler) reconcileUIServiceAccount(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	desired := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      uiResourceName(pulse.Name),
			Namespace: pulse.Namespace,
		},
	}
	if err := controllerutil.SetControllerReference(pulse, desired, r.Scheme); err != nil {
		return err
	}

	existing := &corev1.ServiceAccount{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	return err
}

// b. ClusterRole — read-only access to namespace and cluster resources needed by the UI.
func (r *UIReconciler) reconcileUIClusterRole(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := uiClusterRoleName(pulse.Name)
	desired := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: clusterScopedAnnotations(pulse),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{
					"namespaces", "pods", "services", "nodes", "events",
					"persistentvolumeclaims", "configmaps", "endpoints",
					"serviceaccounts",
				},
				Verbs: []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments", "daemonsets", "statefulsets", "replicasets"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"route.openshift.io"},
				Resources: []string{"routes"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"config.openshift.io"},
				Resources: []string{"clusterversions", "clusteroperators"},
				Verbs:     []string{"get", "list", "watch"},
			},
		},
	}

	existing := &rbacv1.ClusterRole{}
	err := r.Get(ctx, types.NamespacedName{Name: name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Rules = desired.Rules
	existing.Annotations = desired.Annotations
	return r.Update(ctx, existing)
}

// c. ClusterRoleBinding
func (r *UIReconciler) reconcileUIClusterRoleBinding(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := uiClusterRoleName(pulse.Name)
	desired := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: clusterScopedAnnotations(pulse),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     name,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      uiResourceName(pulse.Name),
				Namespace: pulse.Namespace,
			},
		},
	}

	existing := &rbacv1.ClusterRoleBinding{}
	err := r.Get(ctx, types.NamespacedName{Name: name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// RoleRef is immutable; only update Subjects and Annotations.
	existing.Subjects = desired.Subjects
	existing.Annotations = desired.Annotations
	return r.Update(ctx, existing)
}

// d. Secret — oauth-client-secret (hex) and cookie-secret (base64).
// Create-only: existing secrets are never overwritten to preserve live credentials.
func (r *UIReconciler) reconcileUIOAuthSecrets(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := uiOAuthSecretsName(pulse.Name)

	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: pulse.Namespace}, existing)
	if err == nil {
		return nil // already exists — preserve
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	// client-secret: 32 hex chars (16 random bytes hex-encoded).
	clientSecret, err := generatePassword(32)
	if err != nil {
		return fmt.Errorf("generate client-secret: %w", err)
	}

	// cookie-secret: base64(32 random bytes) as expected by oauth-proxy.
	cookieSecret, err := generateCookieSecret()
	if err != nil {
		return fmt.Errorf("generate cookie-secret: %w", err)
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pulse.Namespace,
		},
		StringData: map[string]string{
			"client-secret": clientSecret,
			"cookie-secret": cookieSecret,
		},
	}
	if err := controllerutil.SetControllerReference(pulse, desired, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, desired)
}

// e. ConfigMap — nginx.conf for the openshiftpulse SPA container.
func (r *UIReconciler) reconcileUINginxConfigMap(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := uiNginxConfigMapName(pulse.Name)

	nginxConf := `worker_processes auto;
events { worker_connections 1024; }
http {
  server {
    listen 8080;
    root /usr/share/nginx/html;
    index index.html;
    location / { try_files $uri $uri/ /index.html; }
    location /healthz { return 200 'OK\n'; add_header Content-Type text/plain; }
  }
}
`
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pulse.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if err := controllerutil.SetControllerReference(pulse, cm, r.Scheme); err != nil {
			return err
		}
		cm.Data = map[string]string{
			"nginx.conf": nginxConf,
		}
		return nil
	})
	return err
}

// f. Deployment — openshiftpulse (nginx) + oauth-proxy sidecar.
func (r *UIReconciler) reconcileUIDeployment(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := uiResourceName(pulse.Name)

	replicas := pulse.Spec.UI.Replicas
	if replicas == 0 {
		replicas = 2
	}

	saName := uiResourceName(pulse.Name)
	tlsSecretName := uiTLSSecretName(pulse.Name)
	oauthSecretsName := uiOAuthSecretsName(pulse.Name)
	nginxCMName := uiNginxConfigMapName(pulse.Name)

	maxSurge := intstr.FromInt(1)
	maxUnavailable := intstr.FromInt(0)

	uiImage := resolvedUIImage(pulse)
	oauthImage := resolvedOAuthProxyImage(pulse, DetectClusterInfo(ctx, r.Client))

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pulse.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		if err := controllerutil.SetControllerReference(pulse, deploy, r.Scheme); err != nil {
			return err
		}
		deploy.Spec.Replicas = &replicas
		deploy.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxSurge:       &maxSurge,
				MaxUnavailable: &maxUnavailable,
			},
		}
		deploy.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"app": name},
		}
		deploy.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": name},
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: saName,
				SecurityContext:    defaultPodSecCtx(1001),
				Containers: []corev1.Container{
					{
						// Container 1: nginx serving the React SPA.
						Name:  "openshiftpulse",
						Image: uiImage,
						SecurityContext: defaultContainerSecCtx(),
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: uiHTTPPort, Protocol: corev1.ProtocolTCP},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "nginx-conf",
								MountPath: "/etc/nginx/nginx.conf",
								SubPath:   "nginx.conf",
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromInt(int(uiHTTPPort)),
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromInt(int(uiHTTPPort)),
								},
							},
							InitialDelaySeconds: 15,
							PeriodSeconds:       20,
						},
					},
					{
						// Container 2: OpenShift OAuth proxy terminating TLS on 8443.
						Name:  "oauth-proxy",
						Image: oauthImage,
						SecurityContext: defaultContainerSecCtx(),
						Ports: []corev1.ContainerPort{
							{Name: "https", ContainerPort: uiProxyPort, Protocol: corev1.ProtocolTCP},
						},
						Args: []string{
							"--https-address=:8443",
							"--provider=openshift",
							"--upstream=http://localhost:8080",
							"--tls-cert=/etc/tls/private/tls.crt",
							"--tls-key=/etc/tls/private/tls.key",
							"--cookie-secret-file=/etc/proxy/secrets/cookie-secret",
							fmt.Sprintf("--openshift-service-account=%s", saName),
							"--skip-provider-button",
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "proxy-tls",
								MountPath: "/etc/tls/private",
								ReadOnly:  true,
							},
							{
								Name:      "proxy-secrets",
								MountPath: "/etc/proxy/secrets",
								ReadOnly:  true,
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path:   "/oauth/healthz",
									Port:   intstr.FromInt(int(uiProxyPort)),
									Scheme: corev1.URISchemeHTTPS,
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path:   "/oauth/healthz",
									Port:   intstr.FromInt(int(uiProxyPort)),
									Scheme: corev1.URISchemeHTTPS,
								},
							},
							InitialDelaySeconds: 15,
							PeriodSeconds:       20,
						},
					},
				},
				Volumes: []corev1.Volume{
					{
						Name: "nginx-conf",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: nginxCMName},
							},
						},
					},
					{
						// Serving cert secret auto-generated by OCP when the Service has
						// the "service.beta.openshift.io/serving-cert-secret-name" annotation.
						Name: "proxy-tls",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName: tlsSecretName,
							},
						},
					},
					{
						Name: "proxy-secrets",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName: oauthSecretsName,
							},
						},
					},
				},
			},
		}
		return nil
	})
	return err
}

// g. Service — ClusterIP on 8443; OCP serving-cert annotation generates the TLS secret.
func (r *UIReconciler) reconcileUIService(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := uiResourceName(pulse.Name)
	tlsSecretName := uiTLSSecretName(pulse.Name)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pulse.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if err := controllerutil.SetControllerReference(pulse, svc, r.Scheme); err != nil {
			return err
		}
		// Preserve existing annotations; always write the serving-cert key.
		if svc.Annotations == nil {
			svc.Annotations = make(map[string]string)
		}
		svc.Annotations["service.beta.openshift.io/serving-cert-secret-name"] = tlsSecretName
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		svc.Spec.Selector = map[string]string{"app": name}
		svc.Spec.Ports = []corev1.ServicePort{
			{
				Name:       "https",
				Port:       uiProxyPort,
				TargetPort: intstr.FromInt(int(uiProxyPort)),
				Protocol:   corev1.ProtocolTCP,
			},
		}
		return nil
	})
	return err
}

// h+i. Route — created without spec.host; waits for OCP to assign the hostname.
// Returns the hostname (non-empty) and an empty Result once the Route is admitted.
// Returns an empty host and a requeue Result while waiting.
// info is used to add an informational cluster-domain annotation (OCP still assigns spec.host).
func (r *UIReconciler) reconcileUIRoute(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse, info *ClusterInfo) (string, ctrl.Result, error) {
	name := uiResourceName(pulse.Name)

	routeGVK := schema.GroupVersionKind{
		Group:   "route.openshift.io",
		Version: "v1",
		Kind:    "Route",
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(routeGVK)

	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: pulse.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		// Create Route without spec.host — OCP assigns it.
		desired := &unstructured.Unstructured{}
		desired.SetGroupVersionKind(routeGVK)
		desired.SetName(name)
		desired.SetNamespace(pulse.Namespace)

		annotations := clusterScopedAnnotations(pulse)
		if info != nil && info.IngressDomain != "" {
			annotations["pulse.ai/cluster-domain"] = info.IngressDomain
		}
		desired.SetAnnotations(annotations)

		if setErr := unstructured.SetNestedField(desired.Object, map[string]interface{}{
			"kind":   "Service",
			"name":   name,
			"weight": int64(100),
		}, "spec", "to"); setErr != nil {
			return "", ctrl.Result{}, fmt.Errorf("set route spec.to: %w", setErr)
		}
		if setErr := unstructured.SetNestedField(desired.Object,
			int64(uiProxyPort), "spec", "port", "targetPort"); setErr != nil {
			return "", ctrl.Result{}, fmt.Errorf("set route spec.port.targetPort: %w", setErr)
		}
		if setErr := unstructured.SetNestedField(desired.Object, map[string]interface{}{
			"termination":                   "reencrypt",
			"insecureEdgeTerminationPolicy": "Redirect",
		}, "spec", "tls"); setErr != nil {
			return "", ctrl.Result{}, fmt.Errorf("set route spec.tls: %w", setErr)
		}

		if createErr := r.Create(ctx, desired); createErr != nil {
			return "", ctrl.Result{}, createErr
		}
		// Route created; requeue to wait for hostname.
		return "", ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if err != nil {
		return "", ctrl.Result{}, err
	}

	// Check spec.host — OCP populates this after the route is admitted by the router.
	host, _, _ := unstructured.NestedString(existing.Object, "spec", "host")
	if host == "" {
		// Not yet admitted; requeue.
		return "", ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	return host, ctrl.Result{}, nil
}

// j. OAuthClient — cluster-scoped; created/updated once the Route hostname is known.
// Uses unstructured because the oauth.openshift.io group may not be registered in the scheme.
func (r *UIReconciler) reconcileOAuthClient(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse, routeHost, clientSecret string) error {
	oauthGVK := schema.GroupVersionKind{
		Group:   "oauth.openshift.io",
		Version: "v1",
		Kind:    "OAuthClient",
	}

	redirectURI := "https://" + routeHost

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(oauthGVK)

	clientName := oauthClientName(pulse.Name, pulse.Namespace)
	err := r.Get(ctx, types.NamespacedName{Name: clientName}, existing)
	if apierrors.IsNotFound(err) {
		desired := &unstructured.Unstructured{}
		desired.SetGroupVersionKind(oauthGVK)
		desired.SetName(clientName)
		desired.SetAnnotations(clusterScopedAnnotations(pulse))

		if setErr := unstructured.SetNestedField(desired.Object, clientSecret, "secret"); setErr != nil {
			return fmt.Errorf("set oauthclient secret: %w", setErr)
		}
		if setErr := unstructured.SetNestedStringSlice(desired.Object,
			[]string{redirectURI}, "redirectURIs"); setErr != nil {
			return fmt.Errorf("set oauthclient redirectURIs: %w", setErr)
		}
		// grantMethod=auto skips the OCP consent screen. This is safe here because:
		// (a) redirectURIs is locked to the operator-controlled Route hostname — no
		//     third-party redirect target can be injected via the CR, and
		// (b) the OAuthClient name is CR-scoped (namespace+name) so two CRs cannot
		//     race to own the same client. Change to "prompt" if this operator is ever
		//     deployed in a multi-tenant environment where users are not fully trusted.
		if setErr := unstructured.SetNestedField(desired.Object, "auto", "grantMethod"); setErr != nil {
			return fmt.Errorf("set oauthclient grantMethod: %w", setErr)
		}
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// Update secret and redirectURIs in case the Route hostname changed.
	if setErr := unstructured.SetNestedField(existing.Object, clientSecret, "secret"); setErr != nil {
		return fmt.Errorf("update oauthclient secret: %w", setErr)
	}
	if setErr := unstructured.SetNestedStringSlice(existing.Object,
		[]string{redirectURI}, "redirectURIs"); setErr != nil {
		return fmt.Errorf("update oauthclient redirectURIs: %w", setErr)
	}
	if setErr := unstructured.SetNestedField(existing.Object, "auto", "grantMethod"); setErr != nil {
		return fmt.Errorf("update oauthclient grantMethod: %w", setErr)
	}
	return r.Update(ctx, existing)
}

// getOAuthClientSecret reads the client-secret value from the oauth secrets Secret.
func (r *UIReconciler) getOAuthClientSecret(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) (string, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      uiOAuthSecretsName(pulse.Name),
		Namespace: pulse.Namespace,
	}, secret); err != nil {
		return "", err
	}
	return string(secret.Data["client-secret"]), nil
}

// isReady returns true when at least one replica of the named Deployment is ready.
func (r *UIReconciler) isReady(ctx context.Context, name, ns string) bool {
	deploy := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, deploy); err != nil {
		return false
	}
	return deploy.Status.ReadyReplicas > 0
}

// SetupWithManager registers the UIReconciler with the controller manager.
// Watches: OpenShiftPulse CR (source), owned Deployments/Services/ConfigMaps/Secrets/ServiceAccounts.
// Also watches Routes (not owned) to trigger reconcile when OCP assigns the hostname.
func (r *UIReconciler) SetupWithManager(mgr ctrl.Manager) error {
	routeObj := &unstructured.Unstructured{}
	routeObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "route.openshift.io",
		Version: "v1",
		Kind:    "Route",
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&pulsev1alpha1.OpenShiftPulse{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ServiceAccount{}).
		Watches(
			routeObj,
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				annotations := obj.GetAnnotations()
				if annotations == nil {
					return nil
				}
				ownerName := annotations[annotationOwnerName]
				ownerNS := annotations[annotationOwnerNamespace]
				if ownerName == "" || ownerNS == "" {
					return nil
				}
				return []reconcile.Request{
					{NamespacedName: types.NamespacedName{Name: ownerName, Namespace: ownerNS}},
				}
			}),
		).
		Complete(r)
}
