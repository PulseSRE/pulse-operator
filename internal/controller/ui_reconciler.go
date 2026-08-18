package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

const (
	defaultUIImage = "quay.io/amobrem/openshiftpulse:latest"
	uiHTTPPort     = int32(8080)
	uiProxyPort    = int32(8443)
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
		{"AuthDelegatorBinding", r.reconcileUIAuthDelegatorBinding},
		{"OAuthSecrets", r.reconcileUIOAuthSecrets},
		{"ServiceCABundle", r.reconcileUIServiceCABundle},
		{"Service", r.reconcileUIService},
	} {
		if err := s.fn(ctx, pulse); err != nil {
			logger.Error(err, "UI reconcile step failed", "step", s.name)
			return ctrl.Result{}, fmt.Errorf("UI/%s: %w", s.name, err)
		}
	}

	// NginxConfigMap before Deployment: the hash drives automatic rollout when config changes.
	nginxHash, err := r.reconcileUINginxConfigMap(ctx, pulse)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("UI/NginxConfigMap: %w", err)
	}
	if err := r.reconcileUIDeployment(ctx, pulse, info, nginxHash); err != nil {
		return ctrl.Result{}, fmt.Errorf("UI/Deployment: %w", err)
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
	// UIAvailable must reflect actual pod readiness, not just Route admission —
	// the Route can be admitted by the OCP router while the UI Deployment's
	// pods are still CrashLoopBackOff. Gate on the same readiness check used
	// for the agent/database so `Phase: Running` is never a false positive.
	pulse.Status.RouteHost = routeHost
	pulse.Status.UIAvailable = r.isReady(ctx, uiResourceName(pulse.Name), pulse.Namespace)

	return ctrl.Result{}, nil
}

// ─── name helpers ────────────────────────────────────────────────────────────

func uiResourceName(crName string) string {
	return crName + "-openshiftpulse"
}

// uiClusterRoleNameUnqualified is the pre-namespace-qualification form of the
// UI's cluster-scoped ClusterRole/ClusterRoleBinding name — kept only as the
// "old" side of deleteStaleUnqualifiedClusterScopedResource's migration.
func uiClusterRoleNameUnqualified(crName string) string {
	return crName + "-openshiftpulse-reader"
}

// uiClusterRoleName returns a namespace-qualified name for the UI's
// cluster-scoped ClusterRole/ClusterRoleBinding (and the auth-delegator
// binding derived from it) — see deleteStaleUnqualifiedClusterScopedResource's
// doc comment for why this can't be crName alone the way the UI's namespaced
// resources (ServiceAccount, Deployment, Service, Route, ...) are named.
func uiClusterRoleName(crName, crNamespace string) string {
	return crNamespace + "-" + uiClusterRoleNameUnqualified(crName)
}

func uiOAuthSecretsName(crName string) string {
	return crName + "-oauth-secrets"
}

// uiNginxSecretName names the rendered nginx.conf object. It is a Secret, not
// a ConfigMap: the config embeds the agent's shared WS token twice — as a query
// param on the WebSocket proxy_pass, and as a static Authorization bearer on
// the REST proxy — while both generated ClusterRoles grant cluster-wide
// configmaps get/list/watch. As a ConfigMap the token was therefore readable by
// anything holding those rights, which includes the agent and UI themselves.
// The object name is unchanged so the Deployment's mount path and subPath stay
// exactly as they were.
func uiNginxSecretName(crName string) string {
	return crName + "-nginx"
}

func uiServiceCABundleName(crName string) string {
	return crName + "-service-ca"
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

// resolvedUIReplicas returns the effective UI Deployment replica count, applying
// the same "0 means unset, default to 2" resolution the Deployment reconciler
// uses. Shared with the PDB reconciler so the two never disagree about whether
// a CR that omits spec.ui.replicas ends up with more than one pod.
func resolvedUIReplicas(cr *pulsev1alpha1.OpenShiftPulse) int32 {
	if cr.Spec.UI.Replicas == 0 {
		return 2
	}
	return cr.Spec.UI.Replicas
}

// uiResources returns the effective resource requirements for the UI's main
// (nginx/openshiftpulse) container: the spec override when set, or a modest
// default otherwise. Mirrors agentResources' shape (memory-only, no CPU
// request — Kubernetes auto-sets Requests=Limits for CPU when only a limit is
// given, which is deliberately avoided here too).
func uiResources(cr *pulsev1alpha1.OpenShiftPulse) corev1.ResourceRequirements {
	if cr.Spec.UI.Resources.Requests != nil || cr.Spec.UI.Resources.Limits != nil {
		return cr.Spec.UI.Resources
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
}

// oauthProxyResources returns fixed resource requirements for the oauth-proxy
// sidecar. Not user-configurable via the CRD (no spec field exists for it) —
// the sidecar's footprint is small and stable regardless of workload, unlike
// the main container which spec.ui.resources exists to let operators tune.
func oauthProxyResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
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

// generateCookieSecret returns a 32-character base64-encoded string derived from
// 24 random bytes. 24 bytes encode to exactly 32 base64 chars (no padding needed),
// all printable ASCII with no whitespace or control characters. oauth-proxy uses
// the file content as an AES-256 key when --pass-access-token is set; 32 printable
// bytes satisfy the "must be 16/24/32 bytes" requirement reliably.
func generateCookieSecret() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return base64.RawStdEncoding.EncodeToString(b), nil
}

// isValidCookieSecret returns true when the stored cookie-secret bytes are safe for
// use as an AES-256 key with --pass-access-token=true. Requirements:
//   - exactly 32 bytes (oauth-proxy AES requirement)
//   - all bytes are printable ASCII with no whitespace/control characters
//     (raw random bytes risk nulls or newlines that truncate the effective key)
func isValidCookieSecret(b []byte) bool {
	if len(b) != 32 {
		return false
	}
	for _, c := range b {
		if c < 33 || c > 126 { // printable non-space ASCII range
			return false
		}
	}
	return true
}

// ─── sub-reconcilers ─────────────────────────────────────────────────────────

// a. ServiceAccount
func (r *UIReconciler) reconcileUIServiceAccount(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	saName := uiResourceName(pulse.Name)
	routeName := uiResourceName(pulse.Name)
	// oauth-redirectreference is required by the OpenShift OAuth proxy so that
	// OCP's OAuth server accepts the service account as an OAuth client and
	// issues tokens for the Route's redirect URI.
	redirectRef := fmt.Sprintf(
		`{"kind":"OAuthRedirectReference","apiVersion":"v1","reference":{"kind":"Route","name":"%s"}}`,
		routeName,
	)
	desired := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: pulse.Namespace,
			Annotations: map[string]string{
				"serviceaccounts.openshift.io/oauth-redirectreference.primary": redirectRef,
			},
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
	if err != nil {
		return err
	}
	// Always sync the annotation in case the route name changed.
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	existing.Annotations["serviceaccounts.openshift.io/oauth-redirectreference.primary"] = redirectRef
	return r.Update(ctx, existing)
}

// b. ClusterRole — read-only access to namespace and cluster resources needed by the UI.
func (r *UIReconciler) reconcileUIClusterRole(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := uiClusterRoleName(pulse.Name, pulse.Namespace)
	if err := deleteStaleUnqualifiedClusterScopedResource(ctx, r.Client, &rbacv1.ClusterRole{}, uiClusterRoleNameUnqualified(pulse.Name), pulse); err != nil {
		return fmt.Errorf("migrate stale UI ClusterRole: %w", err)
	}
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
	name := uiClusterRoleName(pulse.Name, pulse.Namespace)
	if err := deleteStaleUnqualifiedClusterScopedResource(ctx, r.Client, &rbacv1.ClusterRoleBinding{}, uiClusterRoleNameUnqualified(pulse.Name), pulse); err != nil {
		return fmt.Errorf("migrate stale UI ClusterRoleBinding: %w", err)
	}
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

// c2. ClusterRoleBinding: system:auth-delegator — required when oauth-proxy uses
// --pass-access-token=true or --openshift-delegate-urls. Without it, the proxy cannot
// create TokenReview or SubjectAccessReview resources and logs "is forbidden" errors.
func (r *UIReconciler) reconcileUIAuthDelegatorBinding(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	bindingName := uiClusterRoleName(pulse.Name, pulse.Namespace) + "-auth-delegator"
	if err := deleteStaleUnqualifiedClusterScopedResource(ctx, r.Client,
		&rbacv1.ClusterRoleBinding{}, uiClusterRoleNameUnqualified(pulse.Name)+"-auth-delegator", pulse); err != nil {
		return fmt.Errorf("migrate stale UI auth-delegator ClusterRoleBinding: %w", err)
	}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:        bindingName,
			Annotations: clusterScopedAnnotations(pulse),
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, crb, func() error {
		crb.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "system:auth-delegator",
		}
		crb.Subjects = []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      uiResourceName(pulse.Name),
			Namespace: pulse.Namespace,
		}}
		return nil
	})
	return err
}

// d. Secret — oauth-client-secret (hex) and cookie-secret (base64, 32 printable chars).
// Validates the cookie-secret on every reconcile and regenerates if the format is
// wrong (e.g. raw bytes with nulls, or 44-byte base64 from an older operator version).
// client-secret is never rotated — rotating it invalidates all active OAuth grants.
func (r *UIReconciler) reconcileUIOAuthSecrets(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := uiOAuthSecretsName(pulse.Name)

	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: pulse.Namespace}, existing)
	if err == nil {
		if isValidCookieSecret(existing.Data["cookie-secret"]) {
			return nil // preserve valid secret
		}
		// Cookie-secret is malformed (raw bytes, or 44-byte base64 from an older
		// operator version). Patch just that key in place rather than deleting
		// and recreating the whole Secret — client-secret must stay untouched
		// here, or fixing an unrelated cookie-secret format bug would silently
		// rotate it too and invalidate the OAuthClient's live grant for no reason.
		newCookie, genErr := generateCookieSecret()
		if genErr != nil {
			return fmt.Errorf("generate cookie-secret: %w", genErr)
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data["cookie-secret"] = []byte(newCookie)
		return r.Update(ctx, existing)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	// client-secret: 32 hex chars (16 random bytes hex-encoded).
	clientSecret, err := generatePassword(32)
	if err != nil {
		return fmt.Errorf("generate client-secret: %w", err)
	}

	// cookie-secret: base64(24 random bytes) = 32 printable chars, as generateCookieSecret documents.
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

// d2. ConfigMap — empty, annotated so the service-ca operator populates it
// with the cluster's service-serving-certificate CA bundle. Needed to trust
// thanos-querier's serving certificate (signed by that CA, not by the
// kube-apiserver CA the SA token's ca.crt verifies) for the /api/prometheus/
// nginx proxy location. Same mechanism this operator already uses in
// reverse for the UI's own TLS cert (service.beta.openshift.io/serving-cert-secret-name).
func (r *UIReconciler) reconcileUIServiceCABundle(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := uiServiceCABundleName(pulse.Name)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pulse.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Annotations == nil {
			cm.Annotations = map[string]string{}
		}
		cm.Annotations["service.beta.openshift.io/inject-cabundle"] = "true"
		return controllerutil.SetControllerReference(pulse, cm, r.Scheme)
	})
	return err
}

// e. ConfigMap — nginx.conf for the openshiftpulse SPA container.
// reconcileUINginxConfigMap creates/updates the nginx ConfigMap and returns a
// short hash of the config content so callers can embed it as a Deployment
// pod-template annotation — Kubernetes then rolls out new pods automatically
// whenever the nginx config changes (subPath mounts never live-update).
func (r *UIReconciler) reconcileUINginxConfigMap(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) (string, error) {
	name := uiNginxSecretName(pulse.Name)

	// Read WS token from the agent's ws-token secret so it can be embedded in
	// the nginx WebSocket proxy rules. Token is created by AgentReconciler on first
	// reconcile and is stable across upgrades — fall back to empty string on lookup
	// failure (agent not yet created) and the configmap will update on the next
	// reconcile after the token secret appears.
	wsToken := ""
	tokenSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      wsTokenSecretName(pulse.Name),
		Namespace: pulse.Namespace,
	}, tokenSecret); err == nil {
		wsToken = string(tokenSecret.Data["token"])
	}

	agentSvc := agentResourceName(pulse.Name) // e.g. "pulse-openshift-sre-agent"

	nginxConf := fmt.Sprintf(`worker_processes auto;
error_log /dev/stderr warn;
pid /tmp/nginx.pid;
events { worker_connections 1024; }
http {
  include /etc/nginx/mime.types;
  default_type application/octet-stream;
  access_log /dev/stdout;
  sendfile on;
  keepalive_timeout 65;
  client_body_temp_path /tmp/client_temp;
  proxy_temp_path /tmp/proxy_temp;
  fastcgi_temp_path /tmp/fastcgi_temp;
  uwsgi_temp_path /tmp/uwsgi_temp;
  scgi_temp_path /tmp/scgi_temp;
  map $http_upgrade $connection_upgrade {
    default upgrade;
    '' close;
  }
  server {
    listen 8080;
    server_name _;
    root /opt/app-root/src;
    index index.html;

    add_header X-Frame-Options "DENY" always;
    add_header X-Content-Type-Options "nosniff" always;
    add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;
    add_header Referrer-Policy "strict-origin-when-cross-origin" always;

    # Kubernetes API proxy — UI reads cluster resources directly.
    #
    # proxy_set_header Origin below is load-bearing, not cosmetic: the
    # frontend watches resources (pods, deployments, etc.) over a raw
    # WebSocket-upgraded connection to kube-apiserver's own watch endpoint
    # (?watch=1), and kube-apiserver's WebSocket watch handler rejects the
    # upgrade with a bare 403 (no body) unless it sees a same-origin-looking
    # Origin header — its own CSRF guard, since a WS handshake bypasses
    # normal CORS preflight. nginx forwards unset headers through by
    # default, so this "worked" implicitly before too, but only as long as
    # nothing between the browser and here (Route, oauth-proxy) ever drops
    # or rewrites Origin — pinning it explicitly here removes that
    # assumption and documents why it must never be removed.
    location /api/kubernetes/ {
      proxy_pass https://kubernetes.default.svc/;
      proxy_ssl_verify on;
      proxy_ssl_trusted_certificate /var/run/secrets/kubernetes.io/serviceaccount/ca.crt;
      proxy_set_header Authorization "Bearer $http_x_forwarded_access_token";
      proxy_set_header Origin $http_origin;
      proxy_set_header Upgrade $http_upgrade;
      proxy_set_header Connection $connection_upgrade;
      proxy_http_version 1.1;
      proxy_read_timeout 3600s;
    }

    # Thanos-querier proxy — UI's CPU/memory/alert charts query PromQL
    # directly against the cluster's own Prometheus/Thanos stack. Forwards
    # the same OAuth access token as the Kubernetes API proxy above; reading
    # this endpoint requires the logged-in user to hold (or be bound to) the
    # cluster-monitoring-view ClusterRole, same as the OpenShift console's
    # own monitoring dashboards. proxy_ssl_trusted_certificate points at the
    # service-ca bundle (not the SA token's ca.crt, which only verifies the
    # kube-apiserver's own certificate, not service-ca-operator-issued ones
    # like thanos-querier's) — see reconcileUIServiceCABundle.
    location /api/prometheus/ {
      proxy_pass https://thanos-querier.openshift-monitoring.svc:9091/;
      proxy_ssl_verify on;
      proxy_ssl_trusted_certificate /etc/pki/service-ca/service-ca.crt;
      proxy_set_header Authorization "Bearer $http_x_forwarded_access_token";
      proxy_read_timeout 60s;
    }

    # Alertmanager proxy — UI's Alerts view reads firing alerts/rules via
    # /api/prometheus/ above, but silences (list/create/expire) are an
    # Alertmanager-only API with no Thanos equivalent. Without this location
    # /api/alertmanager/* fell through to the SPA catch-all below, silently
    # returning index.html (200, text/html) instead of proxying anywhere —
    # the UI correctly treated the non-JSON response as "backend down", but
    # the real cause was a missing proxy, not a misconfigured/absent
    # Prometheus. Same serving-ca bundle as Thanos (both are signed by the
    # cluster's own service-ca). Reading/writing this endpoint requires the
    # logged-in user to hold (or be bound to) monitoring-alertmanager-view
    # (read) or monitoring-alertmanager-edit (read+write silences) in the
    # openshift-monitoring project — see that Service's own
    # openshift.io/description annotation.
    location /api/alertmanager/ {
      proxy_pass https://alertmanager-main.openshift-monitoring.svc:9094/;
      proxy_ssl_verify on;
      proxy_ssl_trusted_certificate /etc/pki/service-ca/service-ca.crt;
      proxy_set_header Authorization "Bearer $http_x_forwarded_access_token";
      proxy_read_timeout 60s;
    }

    # Agent WebSocket — appends the shared token as a query param.
    # SECURITY NOTE: unlike the REST proxy below (Authorization: Bearer header),
    # this passes the token via the upstream URL, which risks it appearing in
    # the agent's own request logs. It is NOT switched to a header here because
    # the agent's WebSocket handler (github.com/PulseSRE/pulse-agent, a separate
    # repo) is what actually authenticates the connection — changing the wire
    # format on this side alone, without confirming the handler reads a header
    # too, would silently break every WebSocket connection. Coordinate a
    # protocol change with pulse-agent before changing this.
    location ~ ^/api/agent/ws/(sre|security|monitor|agent)$ {
      proxy_pass http://%s:8080/ws/$1?token=%s;
      proxy_set_header Upgrade $http_upgrade;
      proxy_set_header Connection $connection_upgrade;
      proxy_set_header X-Forwarded-Access-Token $http_x_forwarded_access_token;
      proxy_set_header X-Forwarded-User $http_x_forwarded_user;
      proxy_http_version 1.1;
      proxy_read_timeout 3600s;
    }

    # Agent REST API (inbox, views, briefing, metrics, etc.).
    location /api/agent/ {
      proxy_pass http://%s:8080/;
      proxy_set_header Authorization "Bearer %s";
      proxy_set_header X-Forwarded-Access-Token $http_x_forwarded_access_token;
      proxy_set_header X-Forwarded-User $http_x_forwarded_user;
      proxy_read_timeout 60s;
    }

    # Runtime config served by nginx (no disk file needed).
    location = /config.js {
      add_header Content-Type application/javascript;
      return 200 'window.__PULSE_CONFIG__={};';
    }
    location /healthz { return 200 'OK\n'; add_header Content-Type text/plain; }
    location ~* \.(js|css|png|svg|ico|woff2?|ttf|eot|map)$ {
      try_files $uri =404;
      expires 1y;
      add_header Cache-Control "public, immutable";
    }
    location / { try_files $uri $uri/ /index.html; }
  }
}
`, agentSvc, wsToken, agentSvc, wsToken)
	cm := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pulse.Namespace,
		},
	}

	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(nginxConf)))[:8]

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if err := controllerutil.SetControllerReference(pulse, cm, r.Scheme); err != nil {
			return err
		}
		cm.StringData = map[string]string{
			"nginx.conf": nginxConf,
		}
		return nil
	})
	if err != nil {
		return hash, err
	}

	// Upgrade path: installs from before this was a Secret left a ConfigMap of
	// the same name holding the token in plain sight. It is no longer read by
	// anything, so remove it rather than leaving the exposure behind.
	staleCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pulse.Namespace}}
	if delErr := r.Delete(ctx, staleCM); delErr != nil && !apierrors.IsNotFound(delErr) {
		return hash, fmt.Errorf("delete stale nginx ConfigMap: %w", delErr)
	}

	return hash, nil
}

// f. Deployment — openshiftpulse (nginx) + oauth-proxy sidecar.
func (r *UIReconciler) reconcileUIDeployment(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse, info *ClusterInfo, nginxHash string) error {
	name := uiResourceName(pulse.Name)

	replicas := resolvedUIReplicas(pulse)

	saName := uiResourceName(pulse.Name)
	tlsSecretName := uiTLSSecretName(pulse.Name)
	oauthSecretsName := uiOAuthSecretsName(pulse.Name)
	nginxCMName := uiNginxSecretName(pulse.Name)

	maxSurge := intstr.FromInt(1)
	maxUnavailable := intstr.FromInt(0)

	uiImage := resolvedUIImage(pulse)
	oauthImage := resolvedOAuthProxyImage(pulse, info)

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
				Labels:      map[string]string{"app": name},
				Annotations: map[string]string{"pulse.ai/nginx-config-hash": nginxHash},
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: saName,
				SecurityContext:    defaultPodSecCtx(nil),
				Containers: []corev1.Container{
					{
						// Container 1: nginx serving the React SPA.
						Name:            "openshiftpulse",
						Image:           uiImage,
						Resources:       uiResources(pulse),
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
							{
								// service-ca bundle for the /api/prometheus/ proxy
								// to trust thanos-querier's serving certificate.
								Name:      "service-ca",
								MountPath: "/etc/pki/service-ca",
								ReadOnly:  true,
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
						Name:            "oauth-proxy",
						Image:           oauthImage,
						Resources:       oauthProxyResources(),
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
							// Use the explicit OAuthClient (not SA-implicit mode) so the
							// operator-managed client-secret is used for token redemption.
							// Without these two flags the proxy uses the SA's implicit client
							// while the redirect URI resolves to the explicit OAuthClient,
							// causing "unauthorized_client" 400 on token exchange.
							fmt.Sprintf("--client-id=%s", oauthClientName(pulse.Name, pulse.Namespace)),
							"--client-secret-file=/etc/proxy/secrets/client-secret",
							"--skip-provider-button",
							// Forward the user's OAuth token so nginx can proxy /api/kubernetes/.
							// Requires cookie-secret to be exactly 16/24/32 bytes (AES key).
							"--pass-access-token=true",
							// oauth-proxy's default scope is "user:info user:check-access"
							// (providers/openshift/provider.go — unconditional, not raised by
							// any client mode). That scope can authenticate a user and answer
							// "can this user do X" SubjectAccessReview checks, but a token
							// scoped to it is rejected by the Kubernetes API for general
							// resource reads — which is exactly what --pass-access-token above
							// forwards the token for (nginx proxies /api/kubernetes/ using it
							// as a Bearer token). Without an explicit user:full request here,
							// that proxy path 403s and resources silently fail to load in the
							// UI — the same symptom the very first oauth-proxy fix in this
							// chain was written to resolve. SA-implicit OAuth clients can never
							// be granted user:full at all (OpenShift restricts SA-derived
							// clients to user:info/user:check-access/role:*), so this was not
							// safely addable until the explicit-OAuthClient switch above.
							"--scope=user:full",
							"--cookie-expire=168h",
							// MUST stay false. oauth-proxy's LoggingHandler wraps every
							// ResponseWriter in a *responseLogger that implements only
							// Header/Write/WriteHeader — not http.Hijacker. Its WebSocket path
							// (yhat/wsutil, used for every /api/kubernetes/...watch=1 and
							// /api/agent/ws/* connection) type-asserts the ResponseWriter to
							// http.Hijacker to take over the raw TCP connection, and with
							// request-logging on that assertion always fails, so oauth-proxy
							// answers every single WebSocket upgrade with an instant
							// "Not a hijacker?" 500 — the root cause of every WebSocket
							// connection failure this UI exhibited. request-logging was
							// briefly enabled here to diagnose an unrelated proxy issue, which
							// is exactly how this got found: it was the diagnostic tool
							// breaking the very thing it was added to observe.
							"--request-logging=false",
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
							Secret: &corev1.SecretVolumeSource{
								SecretName: nginxCMName,
							},
						},
					},
					{
						Name: "service-ca",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: uiServiceCABundleName(pulse.Name)},
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

		// Without an OwnerReference the Route is never garbage-collected by
		// Kubernetes on CR deletion, and it isn't in the finalizer's cleanup
		// list either (that only covers cluster-scoped resources) — it would
		// leak forever. Route is namespace-scoped, so SetControllerReference works.
		if setErr := controllerutil.SetControllerReference(pulse, desired, r.Scheme); setErr != nil {
			return "", ctrl.Result{}, fmt.Errorf("set owner on route: %w", setErr)
		}

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

	// Correct drift on spec.to/spec.port.targetPort/spec.tls if something
	// external changed them (this used to only ever read spec.host and never
	// wrote anything back once the Route existed). spec.host is deliberately
	// never touched here: OCP assigns and owns it once admitted, and
	// overwriting it would either be a no-op or, worse, race the router.
	// spec.tls is reconciled key-by-key rather than as a whole map. Replacing
	// the map wholesale would strip any sibling key this operator does not
	// manage — destinationCACertificate, certificate, key, caCertificate are
	// all legitimate fields an admin (or a cert-management controller) may set
	// on a reencrypt route. Worse, the presence of any such key would make a
	// whole-map DeepEqual unequal on every single reconcile, so the operator
	// would issue an Update every 30s forever, fighting whoever set it.
	wantTLS := map[string]string{
		"termination":                   "reencrypt",
		"insecureEdgeTerminationPolicy": "Redirect",
	}
	wantTo := map[string]interface{}{"kind": "Service", "name": name, "weight": int64(100)}
	gotTo, _, _ := unstructured.NestedMap(existing.Object, "spec", "to")
	gotTargetPort, _, _ := unstructured.NestedFieldNoCopy(existing.Object, "spec", "port", "targetPort")

	tlsDrifted := false
	for key, want := range wantTLS {
		if got, _, _ := unstructured.NestedString(existing.Object, "spec", "tls", key); got != want {
			tlsDrifted = true
			break
		}
	}

	if !reflect.DeepEqual(gotTo, wantTo) ||
		!reflect.DeepEqual(gotTargetPort, int64(uiProxyPort)) ||
		tlsDrifted {
		if setErr := unstructured.SetNestedField(existing.Object, wantTo, "spec", "to"); setErr != nil {
			return "", ctrl.Result{}, fmt.Errorf("correct route spec.to: %w", setErr)
		}
		if setErr := unstructured.SetNestedField(existing.Object, int64(uiProxyPort), "spec", "port", "targetPort"); setErr != nil {
			return "", ctrl.Result{}, fmt.Errorf("correct route spec.port.targetPort: %w", setErr)
		}
		for key, want := range wantTLS {
			if setErr := unstructured.SetNestedField(existing.Object, want, "spec", "tls", key); setErr != nil {
				return "", ctrl.Result{}, fmt.Errorf("correct route spec.tls.%s: %w", key, setErr)
			}
		}
		if updateErr := r.Update(ctx, existing); updateErr != nil {
			return "", ctrl.Result{}, fmt.Errorf("correct route drift: %w", updateErr)
		}
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
