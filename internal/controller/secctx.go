package controller

import corev1 "k8s.io/api/core/v1"

// defaultPodSecCtx returns a PodSecurityContext satisfying the restricted
// PodSecurity admission policy. All containers in the pod inherit the
// seccomp profile; individual containers can override with a narrower profile.
func defaultPodSecCtx(runAsUser int64) *corev1.PodSecurityContext {
	isNonRoot := true
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   &isNonRoot,
		RunAsUser:      &runAsUser,
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// defaultContainerSecCtx returns a SecurityContext satisfying the restricted
// PodSecurity admission policy at the container level.
func defaultContainerSecCtx() *corev1.SecurityContext {
	allowPrivEsc := false
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &allowPrivEsc,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}
