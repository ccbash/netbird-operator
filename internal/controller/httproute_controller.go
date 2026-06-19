// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"

	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// HTTPRouteReconciler translates an HTTPRoute into reachability objects
// (NetworkResource + DNSRecord per backend). Whether the route is exposed
// through the public reverse proxy is a separate, admin-authored decision (a
// ReverseProxyService referencing the route).
type HTTPRouteReconciler struct {
	client.Client

	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch

func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	hr := &gwv1.HTTPRoute{}
	if err := r.Get(ctx, req.NamespacedName, hr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !hr.DeletionTimestamp.IsZero() {
		// Children carry an owner reference to the route, so they are
		// garbage-collected (and their finalizers clean up NetBird) on delete.
		return ctrl.Result{}, nil
	}

	ctrl.LoggerFrom(ctx).V(1).Info("reconciling HTTPRoute")
	for _, parent := range hr.Spec.ParentRefs {
		res, err := reconcileRouteParent(ctx, r.Client, r.Scheme(), r.Recorder, hr, "HTTPRoute", parent, hr.Namespace, httpRouteBackendNames(hr))
		if err != nil || !res.IsZero() {
			return res, err
		}
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

func httpRouteBackendNames(hr *gwv1.HTTPRoute) []string {
	var names []string
	for _, rule := range hr.Spec.Rules {
		for _, ref := range rule.BackendRefs {
			names = append(names, string(ref.Name))
		}
	}
	return names
}

func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gwv1.HTTPRoute{}).
		WithLogConstructor(logConstructor(mgr, "HTTPRoute")).
		Complete(r)
}
