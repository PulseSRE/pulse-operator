package controller

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
)

// recordEvent emits a Kubernetes Event via recorder, or does nothing when
// recorder is nil. Every sub-reconciler's Recorder field is optional: many
// unit tests construct a sub-reconciler directly (e.g. &PostgreSQLReconciler{
// Client: k8sClient, Scheme: testScheme}) without wiring one up, and
// record.EventRecorder is an interface — calling a method on a nil interface
// value panics. Routing every self-heal/rollback event through this helper
// means adding observability here can never be the thing that breaks an
// unrelated test.
func recordEvent(recorder record.EventRecorder, obj runtime.Object, eventType, reason, messageFmt string, args ...interface{}) {
	if recorder == nil {
		return
	}
	recorder.Eventf(obj, eventType, reason, messageFmt, args...)
}
