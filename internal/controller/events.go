// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
)

// Event reasons used in Kubernetes Events emitted by the controllers. They are
// CamelCase per API convention and surface in `kubectl describe`.
const (
	reasonDependencyNotReady = "DependencyNotReady"
	reasonInUse              = "InUse"
	reasonAdvertised         = "Advertised"
)

// recordEvent emits a Warning Kubernetes Event for obj, tolerating a nil
// recorder so reconcilers constructed without one (e.g. in unit tests) don't
// panic.
func recordEvent(rec record.EventRecorder, obj runtime.Object, reason, messageFmt string, args ...any) {
	recordEventType(rec, obj, corev1.EventTypeWarning, reason, messageFmt, args...)
}

// recordNormalEvent emits a Normal Kubernetes Event for obj. Repeated identical
// events are aggregated by the recorder (count++), so it's safe to call every
// reconcile to surface steady state.
func recordNormalEvent(rec record.EventRecorder, obj runtime.Object, reason, messageFmt string, args ...any) {
	recordEventType(rec, obj, corev1.EventTypeNormal, reason, messageFmt, args...)
}

func recordEventType(rec record.EventRecorder, obj runtime.Object, eventType, reason, messageFmt string, args ...any) {
	if rec == nil {
		return
	}
	rec.Eventf(obj, eventType, reason, messageFmt, args...)
}
