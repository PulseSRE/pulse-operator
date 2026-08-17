// +groupName=pulse.ai
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion = schema.GroupVersion{Group: "pulse.ai", Version: "v1alpha1"}
	// scheme.Builder.Register accepts runtime.Object instances directly, which
	// is what types.go's init() uses; apimachinery's runtime.SchemeBuilder
	// (the suggested replacement) instead takes func(*runtime.Scheme) error
	// values, so swapping requires rewriting the Register call too — deferred
	// as a separate, deliberate change rather than bundled into a lint pass.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion} //nolint:staticcheck // SA1019: see comment above
	AddToScheme   = SchemeBuilder.AddToScheme
)
