// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"

	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

// TCPRouteReconciler translates a TCPRoute into reachability objects
// (NetworkResource + DNSRecord per backend), reachable over the mesh. Exposing
// it through the reverse proxy (L4) is a separate admin decision (a
// ReverseProxyService referencing the route).
type TCPRouteReconciler struct {
	client.Client

	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=tcproutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=tcproutes/status,verbs=get;update;patch

func (r *TCPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	tr := &gwv1alpha2.TCPRoute{}
	if err := r.Get(ctx, req.NamespacedName, tr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !tr.DeletionTimestamp.IsZero() {
		// Children carry an owner reference to the route, so they are
		// garbage-collected (and their finalizers clean up NetBird) on delete.
		return ctrl.Result{}, nil
	}

	ctrl.LoggerFrom(ctx).V(1).Info("reconciling TCPRoute")
	for _, parent := range tr.Spec.ParentRefs {
		res, err := reconcileRouteParent(ctx, r.Client, r.Scheme(), r.Recorder, tr, "TCPRoute", parent, tr.Namespace, tcpRouteBackendNames(tr))
		if err != nil || !res.IsZero() {
			return res, err
		}
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

func tcpRouteBackendNames(tr *gwv1alpha2.TCPRoute) []string {
	var names []string
	for _, rule := range tr.Spec.Rules {
		for _, ref := range rule.BackendRefs {
			names = append(names, string(ref.Name))
		}
	}
	return names
}

func (r *TCPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gwv1alpha2.TCPRoute{}).
		WithLogConstructor(logConstructor(mgr, "TCPRoute")).
		Complete(r)
}
