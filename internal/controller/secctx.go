package controller

import corev1 "k8s.io/api/core/v1"

// defaultPodSecCtx returns a PodSecurityContext satisfying the restricted
// PodSecurity admission policy. When all containers in the pod run as the same
// UID, pass that UID; pass 0 to omit RunAsUser (e.g. when containers have
// different built-in UIDs and each container must override individually).
func defaultPodSecCtx(runAsUser int64) *corev1.PodSecurityContext {
	isNonRoot := true
	ctx := &corev1.PodSecurityContext{
		RunAsNonRoot:   &isNonRoot,
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if runAsUser > 0 {
		ctx.RunAsUser = &runAsUser
	}
	return ctx
}

// defaultContainerSecCtx returns a SecurityContext satisfying the restricted
// PodSecurity admission policy at the container level.
// ReadOnlyRootFilesystem is intentionally omitted — callers that need a writable
// root (e.g. PostgreSQL) must explicitly set it to false to prevent mutating
// admission webhooks from defaulting the field to true.
func defaultContainerSecCtx() *corev1.SecurityContext {
	allowPrivEsc := false
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &allowPrivEsc,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// writableContainerSecCtx is like defaultContainerSecCtx but explicitly sets
// ReadOnlyRootFilesystem=false for containers (e.g. PostgreSQL) that write to
// /tmp, sockets, or their data directory on the local filesystem.
func writableContainerSecCtx() *corev1.SecurityContext {
	ctx := defaultContainerSecCtx()
	roFS := false
	ctx.ReadOnlyRootFilesystem = &roFS
	return ctx
}
