package controller

import corev1 "k8s.io/api/core/v1"

// defaultPodSecCtx returns a PodSecurityContext satisfying the restricted
// PodSecurity admission policy. Pass a non-nil uid when all containers share
// a single known UID; pass nil to omit RunAsUser so OCP's SCC admission
// assigns a UID from the namespace range (required when containers have
// different UIDs or the namespace enforces its own UID range).
// Never pass 0 — that would set RunAsUser=root.
func defaultPodSecCtx(uid *int64) *corev1.PodSecurityContext {
	isNonRoot := true
	ctx := &corev1.PodSecurityContext{
		RunAsNonRoot:   &isNonRoot,
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if uid != nil {
		ctx.RunAsUser = uid
	}
	return ctx
}

// podUID is a convenience helper to create an *int64 for defaultPodSecCtx.
func podUID(uid int64) *int64 { return &uid }

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
