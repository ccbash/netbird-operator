// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/k8sutil"
	nbv1alpha1ac "github.com/netbirdio/kubernetes-operator/pkg/applyconfigurations/api/v1alpha1"
)

// byopGatewayController is the GatewayClass controllerName the operator claims.
// A GatewayClass set to it routes its Gateways' HTTPRoutes through the NetBird
// BYOP reverse proxy (a ReverseProxyCluster, referenced via the class's
// parametersRef).
const byopGatewayController gwv1.GatewayController = "netbird.io/byop-proxy"

// httpRouteLabel marks the ReverseProxyService children translated from an
// HTTPRoute (value = the route name), for prune and lookup.
const httpRouteLabel = "gateway.netbird.io/httproute"

// GatewayClassReconciler accepts GatewayClasses that point at the BYOP proxy.
type GatewayClassReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/status,verbs=get;update;patch

func (r *GatewayClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	gc := &gwv1.GatewayClass{}
	if err := r.Get(ctx, req.NamespacedName, gc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if gc.Spec.ControllerName != byopGatewayController {
		return ctrl.Result{}, nil
	}
	meta.SetStatusCondition(&gc.Status.Conditions, metav1.Condition{
		Type:               string(gwv1.GatewayClassConditionStatusAccepted),
		Status:             metav1.ConditionTrue,
		Reason:             string(gwv1.GatewayClassReasonAccepted),
		Message:            "Accepted by the NetBird BYOP reverse proxy",
		ObservedGeneration: gc.Generation,
	})
	return ctrl.Result{}, r.Status().Update(ctx, gc)
}

func (r *GatewayClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gwv1.GatewayClass{}).
		WithLogConstructor(logConstructor(mgr, "GatewayClass")).
		Complete(r)
}

// HTTPRouteReconciler translates HTTPRoutes attached to a BYOP Gateway into
// owned ReverseProxyService children — the same exposure the operator already
// reconciles, just authored from Gateway API instead of by hand.
type HTTPRouteReconciler struct {
	client.Client
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get;update;patch

func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	route := &gwv1.HTTPRoute{}
	if err := r.Get(ctx, req.NamespacedName, route); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clusterAddr, parent, ok, err := r.resolveCluster(ctx, route)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ok {
		// Not attached to one of our Gateways — drop anything we previously made.
		return ctrl.Result{}, r.prune(ctx, route, nil)
	}

	ownerRef, err := k8sutil.ControllerReference(route, r.Scheme())
	if err != nil {
		return ctrl.Result{}, err
	}

	desired := map[string]bool{}
	for _, hostname := range route.Spec.Hostnames {
		name := routeChildName(route.Name, string(hostname))
		desired[name] = true
		rpsAC := nbv1alpha1ac.ReverseProxyService(name, route.Namespace).
			WithOwnerReferences(ownerRef).
			WithLabels(map[string]string{httpRouteLabel: route.Name}).
			WithSpec(nbv1alpha1ac.ReverseProxyServiceSpec().
				WithDomain(string(hostname)).
				WithProxyCluster(clusterAddr).
				WithBackends(routeBackends(route)...))
		if err := r.Apply(ctx, rpsAC, client.ForceOwnership); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.prune(ctx, route, desired); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.setAccepted(ctx, route, parent)
}

// resolveCluster finds the first parent Gateway of a class the operator owns and
// returns the BYOP cluster address it routes to (from the GatewayClass's
// parametersRef -> ReverseProxyCluster), plus the matched parentRef.
func (r *HTTPRouteReconciler) resolveCluster(ctx context.Context, route *gwv1.HTTPRoute) (string, gwv1.ParentReference, bool, error) {
	for _, p := range route.Spec.ParentRefs {
		ns := route.Namespace
		if p.Namespace != nil {
			ns = string(*p.Namespace)
		}
		gw := &gwv1.Gateway{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: string(p.Name)}, gw); err != nil {
			if kerrors.IsNotFound(err) {
				continue
			}
			return "", p, false, err
		}
		gc := &gwv1.GatewayClass{}
		if err := r.Get(ctx, client.ObjectKey{Name: string(gw.Spec.GatewayClassName)}, gc); err != nil {
			if kerrors.IsNotFound(err) {
				continue
			}
			return "", p, false, err
		}
		if gc.Spec.ControllerName != byopGatewayController || gc.Spec.ParametersRef == nil {
			continue
		}
		ref := gc.Spec.ParametersRef
		refNS := ns
		if ref.Namespace != nil {
			refNS = string(*ref.Namespace)
		}
		rpc := &nbv1alpha1.ReverseProxyCluster{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: refNS, Name: ref.Name}, rpc); err != nil {
			if kerrors.IsNotFound(err) {
				continue
			}
			return "", p, false, err
		}
		return rpc.Spec.ClusterAddress, p, true, nil
	}
	return "", gwv1.ParentReference{}, false, nil
}

// prune deletes ReverseProxyService children of the route no longer desired
// (desired nil drops them all).
func (r *HTTPRouteReconciler) prune(ctx context.Context, route *gwv1.HTTPRoute, desired map[string]bool) error {
	var list nbv1alpha1.ReverseProxyServiceList
	if err := r.List(ctx, &list, client.InNamespace(route.Namespace), client.MatchingLabels{httpRouteLabel: route.Name}); err != nil {
		return err
	}
	for i := range list.Items {
		if !desired[list.Items[i].Name] {
			if err := r.Delete(ctx, &list.Items[i]); err != nil && !kerrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (r *HTTPRouteReconciler) setAccepted(ctx context.Context, route *gwv1.HTTPRoute, parent gwv1.ParentReference) error {
	cond := metav1.Condition{
		Type:               string(gwv1.RouteConditionAccepted),
		Status:             metav1.ConditionTrue,
		Reason:             string(gwv1.RouteReasonAccepted),
		Message:            "Translated to a NetBird ReverseProxyService",
		ObservedGeneration: route.Generation,
	}
	idx := -1
	for i := range route.Status.Parents {
		if route.Status.Parents[i].ControllerName == byopGatewayController {
			idx = i
			break
		}
	}
	if idx < 0 {
		route.Status.Parents = append(route.Status.Parents, gwv1.RouteParentStatus{
			ParentRef:      parent,
			ControllerName: byopGatewayController,
		})
		idx = len(route.Status.Parents) - 1
	}
	meta.SetStatusCondition(&route.Status.Parents[idx].Conditions, cond)
	return r.Status().Update(ctx, route)
}

func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gwv1.HTTPRoute{}).
		WithLogConstructor(logConstructor(mgr, "HTTPRoute")).
		Owns(&nbv1alpha1.ReverseProxyService{}).
		Complete(r)
}

// routeBackends maps an HTTPRoute's rules onto ReverseProxyService backends: one
// backend per (rule, backendRef), carrying the rule's PathPrefix as the backend
// path. Match types other than PathPrefix collapse to "/".
func routeBackends(route *gwv1.HTTPRoute) []*nbv1alpha1ac.ReverseProxyBackendApplyConfiguration {
	var out []*nbv1alpha1ac.ReverseProxyBackendApplyConfiguration
	for _, rule := range route.Spec.Rules {
		path := rulePath(rule)
		for _, br := range rule.BackendRefs {
			b := nbv1alpha1ac.ReverseProxyBackend().
				WithServiceRef(corev1.LocalObjectReference{Name: string(br.Name)}).
				WithPath(path)
			if br.Port != nil {
				b.WithPort(int(*br.Port))
			}
			out = append(out, b)
		}
	}
	return out
}

// rulePath returns the rule's first PathPrefix match value, or "/".
func rulePath(rule gwv1.HTTPRouteRule) string {
	for _, m := range rule.Matches {
		if m.Path != nil && m.Path.Type != nil && *m.Path.Type == gwv1.PathMatchPathPrefix && m.Path.Value != nil {
			return *m.Path.Value
		}
	}
	return "/"
}

// routeChildName derives a DNS-safe ReverseProxyService name from the route name
// and a hostname (which carries dots and may be a wildcard).
func routeChildName(routeName, hostname string) string {
	h := strings.ReplaceAll(hostname, "*", "wildcard")
	h = strings.ReplaceAll(h, ".", "-")
	name := routeName + "-" + h
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}
