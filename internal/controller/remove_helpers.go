package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// deleteOwned deletes a namespaced object only when this CR controls it, so
// turning a feature off can never remove something the operator did not make.
// A missing object is not an error.
func deleteOwned(ctx context.Context, c client.Client, obj client.Object, pulse *pulsev1alpha1.OpenShiftPulse) error {
	key := types.NamespacedName{Name: obj.GetName(), Namespace: pulse.Namespace}
	if err := c.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !metav1.IsControlledBy(obj, pulse) {
		return nil
	}
	if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %T %s: %w", obj, key.Name, err)
	}
	return nil
}

// deleteOwnedClusterScoped deletes a cluster-scoped object only when its
// owner-UID annotation names this CR. Cluster-scoped objects cannot carry an
// OwnerReference to a namespaced CR, so the annotation is the ownership record.
func deleteOwnedClusterScoped(ctx context.Context, c client.Client, obj client.Object, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := obj.GetName()
	if err := c.Get(ctx, types.NamespacedName{Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if obj.GetAnnotations()[annotationOwnerUID] != string(pulse.UID) {
		return nil
	}
	if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %T %s: %w", obj, name, err)
	}
	return nil
}
