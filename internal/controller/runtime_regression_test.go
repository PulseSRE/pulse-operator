package controller

import (
	"context"
	"testing"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRuntimeReconciliationRegressions(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme, networkingv1.AddToScheme, pulsev1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	cr := &pulsev1alpha1.OpenShiftPulse{ObjectMeta: metav1.ObjectMeta{Name: "pulse", Namespace: "default", UID: "pulse-uid"}}

	t.Run("nginx does not issue updates for identical configuration", func(t *testing.T) {
		updates := 0
		// The fake client does not perform the API server's write-only
		// StringData conversion. Model it so this test catches the old
		// unconditional StringData assignment, not just the config hash.
		normalizeSecret := func(obj client.Object) {
			secret, ok := obj.(*corev1.Secret)
			if !ok || len(secret.StringData) == 0 {
				return
			}
			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}
			for key, value := range secret.StringData {
				secret.Data[key] = []byte(value)
			}
			secret.StringData = nil
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr.DeepCopy()).
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					normalizeSecret(obj)
					return c.Create(ctx, obj, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					updates++
					normalizeSecret(obj)
					return c.Update(ctx, obj, opts...)
				},
			}).Build()
		r := &UIReconciler{Client: c, Scheme: scheme}
		firstHash, err := r.reconcileUINginxConfigMap(ctx, cr)
		if err != nil {
			t.Fatal(err)
		}
		secondHash, err := r.reconcileUINginxConfigMap(ctx, cr)
		if err != nil {
			t.Fatal(err)
		}
		if firstHash != secondHash || updates != 0 {
			t.Fatalf("stable reconcile changed config or updated Secret: hashes %q/%q, updates %d", firstHash, secondHash, updates)
		}
		// A real token change must still update the rendered config.
		token := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: wsTokenSecretName(cr.Name), Namespace: cr.Namespace}, Data: map[string][]byte{"token": []byte("changed-token")}}
		if err := c.Create(ctx, token); err != nil {
			t.Fatal(err)
		}
		thirdHash, err := r.reconcileUINginxConfigMap(ctx, cr)
		if err != nil {
			t.Fatal(err)
		}
		if thirdHash == firstHash || updates != 1 {
			t.Fatalf("token change was not propagated: hash %q, updates %d", thirdHash, updates)
		}
	})

	t.Run("Temporal deployment matches scoped PostgreSQL ingress peer", func(t *testing.T) {
		pulse := cr.DeepCopy()
		enabled := true
		pulse.Spec.Temporal.Enabled = &enabled
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pulse.DeepCopy()).Build()
		r := &OpenShiftPulseReconciler{Client: c, Scheme: scheme}
		tr := &TemporalReconciler{Client: c, Scheme: scheme}
		if err := tr.reconcileDeployment(ctx, pulse); err != nil {
			t.Fatal(err)
		}
		if err := r.reconcilePGNetworkPolicy(ctx, pulse); err != nil {
			t.Fatal(err)
		}
		deploy := &appsv1.Deployment{}
		if err := c.Get(ctx, types.NamespacedName{Name: temporalResourceName(pulse.Name), Namespace: pulse.Namespace}, deploy); err != nil {
			t.Fatal(err)
		}
		np := &networkingv1.NetworkPolicy{}
		key := types.NamespacedName{Name: pulse.Name + "-pg-access", Namespace: pulse.Namespace}
		if err := c.Get(ctx, key, np); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, rule := range np.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.PodSelector == nil || peer.NamespaceSelector != nil || len(peer.PodSelector.MatchLabels) != 1 {
					t.Fatal("database ingress must stay scoped to one app in the same namespace")
				}
				if peer.PodSelector.MatchLabels["app"] == deploy.Spec.Template.Labels["app"] {
					found = len(rule.Ports) == 1 && rule.Ports[0].Port.IntVal == 5432
				}
			}
		}
		if !found {
			t.Fatal("Temporal deployment cannot reach PostgreSQL under the generated policy")
		}
		enabled = false
		if err := r.reconcilePGNetworkPolicy(ctx, pulse); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(ctx, key, np); err != nil {
			t.Fatal(err)
		}
		if len(np.Spec.Ingress[0].From) != 1 || np.Spec.Ingress[0].From[0].PodSelector.MatchLabels["app"] != agentResourceName(pulse.Name) {
			t.Fatal("disabling Temporal did not revoke its database ingress")
		}
	})
}
