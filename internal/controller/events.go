// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
)

// Event reasons used in Kubernetes Events emitted by the controllers. They are
// CamelCase per API convention and surface in `kubectl describe`.
const (
	reasonDependencyNotReady = "DependencyNotReady"
	reasonRoutingModeSwitch  = "RoutingModeSwitch"
	reasonAwaitingRelease    = "AwaitingProxyRelease"
	reasonBackendNotFound    = "BackendNotFound"
	reasonProxyTargetMissing = "ProxyTargetNotFound"
	reasonInUse              = "InUse"
)

// recordEvent emits a Kubernetes Event for obj, tolerating a nil recorder so
// reconcilers constructed without one (e.g. in unit tests) don't panic.
func recordEvent(rec record.EventRecorder, obj runtime.Object, eventType, reason, messageFmt string, args ...any) {
	if rec == nil {
		return
	}
	rec.Eventf(obj, eventType, reason, messageFmt, args...)
}
