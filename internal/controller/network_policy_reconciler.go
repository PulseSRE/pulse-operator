package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	pulsev1alpha1 "github.com/PulseSRE/pulse-operator/api/v1alpha1"
)

// reconcileNetworkPolicies creates or updates two NetworkPolicies:
//   - {name}-openshiftpulse: protects the UI pod
//   - {name}-pg-access: protects PostgreSQL
func (r *OpenShiftPulseReconciler) reconcileNetworkPolicies(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	if err := r.reconcileUIPodNetworkPolicy(ctx, pulse); err != nil {
		return fmt.Errorf("ui network policy: %w", err)
	}
	if err := r.reconcilePGNetworkPolicy(ctx, pulse); err != nil {
		return fmt.Errorf("pg network policy: %w", err)
	}
	return nil
}

func (r *OpenShiftPulseReconciler) reconcileUIPodNetworkPolicy(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := pulse.Name + "-openshiftpulse"
	uiApp := pulse.Name + "-openshiftpulse"

	tcpProto := corev1.ProtocolTCP
	port8443 := intstr.FromInt(8443)
	port8080 := intstr.FromInt(8080)

	ingress := []networkingv1.NetworkPolicyIngressRule{
		// OCP router ingress namespace.
		{
			From: []networkingv1.NetworkPolicyPeer{
				{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"network.openshift.io/policy-group": "ingress",
						},
					},
				},
			},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcpProto, Port: &port8443},
			},
		},
		// NOTE: there used to be a "same-namespace pods" rule here using an
		// empty PodSelector{}, which matches ALL pods in the namespace rather
		// than a scoped subset. Nothing in this operator's traffic pattern
		// needs sibling pods to call into the UI (nginx/oauth-proxy only ever
		// makes outbound calls to the agent and the Kubernetes API), so it was
		// removed rather than narrowed. Re-add it scoped to a specific
		// PodSelector if a real caller shows up.
		// user-workload-monitoring scrape.
		{
			From: []networkingv1.NetworkPolicyPeer{
				{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"kubernetes.io/metadata.name": "openshift-user-workload-monitoring",
						},
					},
				},
			},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcpProto, Port: &port8080},
				{Protocol: &tcpProto, Port: &port8443},
			},
		},
	}
	egress := []networkingv1.NetworkPolicyEgressRule{{}}
	policyTypes := []networkingv1.PolicyType{
		networkingv1.PolicyTypeIngress,
		networkingv1.PolicyTypeEgress,
	}

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pulse.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Spec.PodSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{"app": uiApp},
		}
		np.Spec.PolicyTypes = policyTypes
		np.Spec.Ingress = ingress
		np.Spec.Egress = egress
		return controllerutil.SetControllerReference(pulse, np, r.Scheme)
	})
	return err
}

func (r *OpenShiftPulseReconciler) reconcilePGNetworkPolicy(ctx context.Context, pulse *pulsev1alpha1.OpenShiftPulse) error {
	name := pulse.Name + "-pg-access"
	pgApp := pulse.Name + "-openshift-sre-agent-postgresql"
	agentApp := pulse.Name + "-openshift-sre-agent"

	tcpProto := corev1.ProtocolTCP
	port5432 := intstr.FromInt(5432)

	ingress := []networkingv1.NetworkPolicyIngressRule{
		{
			From: []networkingv1.NetworkPolicyPeer{
				{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": agentApp},
					},
				},
			},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcpProto, Port: &port5432},
			},
		},
	}

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pulse.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Spec.PodSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{"app": pgApp},
		}
		np.Spec.PolicyTypes = []networkingv1.PolicyType{
			networkingv1.PolicyTypeIngress,
		}
		np.Spec.Ingress = ingress
		return controllerutil.SetControllerReference(pulse, np, r.Scheme)
	})
	return err
}
