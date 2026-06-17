// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"time"

	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/patch"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/gatewayutil"
	"github.com/netbirdio/kubernetes-operator/internal/k8sutil"
	nbv1alpha1ac "github.com/netbirdio/kubernetes-operator/pkg/applyconfigurations/api/v1alpha1"
)

const (
	HTTPRouteFinalizer = "gateway.netbird.io/httproute"
)

type HTTPRouteReconciler struct {
	client.Client

	Netbird *netbird.Client
}

func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.Log.WithName("HTTPRoute").WithValues("namespace", req.Namespace, "name", req.Name)

	hr := &gwv1.HTTPRoute{}
	err := r.Get(ctx, req.NamespacedName, hr)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	sp := patch.NewSerialPatcher(hr, r.Client)

	if !hr.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, sp, hr)
	}

	for _, parent := range hr.Spec.ParentRefs {
		res, err := r.reconcileParent(ctx, logger, sp, hr, parent)
		if err != nil {
			return ctrl.Result{}, err
		}
		// A non-zero result is a requeue request (a backend resource is not
		// ready yet); stop and let the controller retry rather than pressing
		// on with the remaining parents.
		if !res.IsZero() {
			return res, nil
		}
	}

	return ctrl.Result{}, nil
}

// reconcileParent reconciles the route against a single parent Gateway: it
// ensures a NetworkResource per backend Service and an up-to-date reverse-proxy
// service per hostname. A zero Result means "this parent is done, continue";
// a non-zero Result asks the caller to requeue.
func (r *HTTPRouteReconciler) reconcileParent(ctx context.Context, logger logr.Logger, sp *patch.SerialPatcher, hr *gwv1.HTTPRoute, parent gwv1.ParentReference) (ctrl.Result, error) {
	gw, err := gatewayutil.GetParentGateway(ctx, r.Client, parent, hr.Namespace, GatewayControllerName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if gw == nil {
		return ctrl.Result{}, nil
	}
	if !meta.IsStatusConditionTrue(gw.Status.Conditions, string(gwv1.GatewayConditionProgrammed)) {
		logger.Info("gateway is not ready", "name", gw.ObjectMeta.Name)
		return ctrl.Result{}, nil
	}
	netRouter, err := gatewayutil.GetGatewayNetworkRouter(ctx, r.Client, gw)
	if err != nil {
		return ctrl.Result{}, err
	}

	controllerutil.AddFinalizer(hr, k8sutil.Finalizer("httproute"))
	if err := sp.Patch(ctx, hr); err != nil {
		return ctrl.Result{}, err
	}

	svcIdx, err := r.indexBackendServices(ctx, hr, false)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Resolve the attached NBServicePolicies up front. routingMode decides
	// whether each backend is exposed as a host (ClusterIP) or domain (FQDN)
	// resource + matching proxy target; it defaults to ip (oldest policy
	// wins, matching applyServicePolicies).
	policies, err := r.servicePoliciesFor(ctx, hr)
	if err != nil {
		return ctrl.Result{}, err
	}
	routingMode, targetType := resolveRoutingMode(policies)

	if err := r.ensureNetworkResources(ctx, hr, netRouter, svcIdx, routingMode); err != nil {
		return ctrl.Result{}, err
	}

	resourceID, ready, err := r.resolveResourceIDs(ctx, hr, svcIdx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	targets := buildTargets(logger, hr, svcIdx, resourceID, targetType)
	if err := r.reconcileProxyServices(ctx, hr, targets, policies); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// indexBackendServices returns the Services referenced by the route's
// backendRefs, deduplicated by name (one NetworkResource is created per
// Service). When tolerateMissing is set, Services that no longer exist are
// skipped instead of erroring — used on the delete path.
func (r *HTTPRouteReconciler) indexBackendServices(ctx context.Context, hr *gwv1.HTTPRoute, tolerateMissing bool) (map[string]corev1.Service, error) {
	svcIdx := map[string]corev1.Service{}
	for _, rule := range hr.Spec.Rules {
		for _, ref := range rule.BackendRefs {
			key := client.ObjectKey{Namespace: hr.Namespace, Name: string(ref.Name)}
			var svc corev1.Service
			err := r.Client.Get(ctx, key, &svc)
			if tolerateMissing && kerrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			svcIdx[svc.Name] = svc
		}
	}
	return svcIdx, nil
}

// resolveRoutingMode folds the attached policies down to a single routing mode
// (oldest non-empty policy wins) and the matching proxy target type.
func resolveRoutingMode(policies []nbv1alpha1.NBServicePolicy) (nbv1alpha1.RoutingMode, api.ServiceTargetTargetType) {
	routingMode := nbv1alpha1.RoutingModeIP
	for i := range policies {
		if policies[i].Spec.RoutingMode != "" {
			routingMode = policies[i].Spec.RoutingMode
		}
	}
	if routingMode == nbv1alpha1.RoutingModeDomain {
		return routingMode, api.ServiceTargetTargetTypeDomain
	}
	return routingMode, api.ServiceTargetTargetTypeHost
}

// ensureNetworkResources applies one NetworkResource per backend Service, owned
// by both the Service (controller ref) and the route.
func (r *HTTPRouteReconciler) ensureNetworkResources(ctx context.Context, hr *gwv1.HTTPRoute, netRouter *nbv1alpha1.NetworkRouter, svcIdx map[string]corev1.Service, routingMode nbv1alpha1.RoutingMode) error {
	for _, svc := range svcIdx {
		controllerRef, err := k8sutil.ControllerReference(&svc, r.Scheme())
		if err != nil {
			return err
		}
		controllerRef = controllerRef.WithBlockOwnerDeletion(false)
		ownerRef, err := k8sutil.OwnerReference(hr, r.Scheme())
		if err != nil {
			return err
		}
		netResourceAC := nbv1alpha1ac.NetworkResource(svc.Name, svc.Namespace).
			WithOwnerReferences(controllerRef, ownerRef).
			WithSpec(
				nbv1alpha1ac.NetworkResourceSpec().
					WithNetworkRouterRef(nbv1alpha1ac.CrossNamespaceReference().WithName(netRouter.Name).WithNamespace(netRouter.Namespace)).
					WithServiceRef(corev1.LocalObjectReference{Name: svc.Name}).
					WithRoutingMode(routingMode),
			)
		if err := r.Client.Apply(ctx, netResourceAC, client.ForceOwnership); err != nil {
			return err
		}
	}
	return nil
}

// resolveResourceIDs maps each Service to its NetworkResource ID. ready is false
// if any resource has not reconciled yet, signalling the caller to requeue.
func (r *HTTPRouteReconciler) resolveResourceIDs(ctx context.Context, hr *gwv1.HTTPRoute, svcIdx map[string]corev1.Service) (map[string]string, bool, error) {
	resourceID := map[string]string{}
	for name := range svcIdx {
		netResource := &nbv1alpha1.NetworkResource{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: hr.Namespace},
		}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(netResource), netResource); err != nil {
			return nil, false, err
		}
		if !conditions.Has(netResource, nbv1alpha1.ReadyCondition) {
			return nil, false, nil
		}
		resourceID[name] = netResource.Status.ResourceID
	}
	return resourceID, true, nil
}

// buildTargets builds one proxy target per backendRef, carrying the rule's path
// prefix so path-based routes (e.g. /push/ -> notify-push, / -> app) resolve to
// the right backend instead of being flattened to pathless targets.
func buildTargets(logger logr.Logger, hr *gwv1.HTTPRoute, svcIdx map[string]corev1.Service, resourceID map[string]string, targetType api.ServiceTargetTargetType) []api.ServiceTarget {
	targets := []api.ServiceTarget{}
	for _, rule := range hr.Spec.Rules {
		path := pathPrefixFor(rule)
		for _, ref := range rule.BackendRefs {
			svc, ok := svcIdx[string(ref.Name)]
			if !ok {
				continue
			}
			var refPort int32
			if ref.Port != nil {
				refPort = *ref.Port
			} else if len(svc.Spec.Ports) > 0 {
				logger.Info("backendRef omits port; using the Service's first port",
					"service", svc.Name, "port", svc.Spec.Ports[0].Port)
			}
			targets = append(targets, api.ServiceTarget{
				Enabled:  true,
				Path:     path,
				Port:     backendPortFor(svc, refPort),
				TargetId: resourceID[svc.Name],
				Protocol: api.ServiceTargetProtocolHttp,
				// Must match the backend resource's type, which follows the
				// route's routing mode (host for ip, domain for domain).
				TargetType: targetType,
			})
		}
	}
	sortTargets(targets)
	return targets
}

// reconcileProxyServices upserts the reverse-proxy service for each hostname.
// Per-service config (private, access groups, CrowdSec, header behaviour) comes
// from the attached policies.
func (r *HTTPRouteReconciler) reconcileProxyServices(ctx context.Context, hr *gwv1.HTTPRoute, targets []api.ServiceTarget, policies []nbv1alpha1.NBServicePolicy) error {
	proxyServices, err := r.Netbird.ReverseProxyServices.List(ctx)
	if err != nil {
		return err
	}

	for _, hostname := range hr.Spec.Hostnames {
		proxyReq := api.ServiceRequest{
			Domain:           string(hostname),
			Enabled:          true,
			Name:             string(hostname),
			Mode:             new(api.ServiceRequestModeHttp),
			PassHostHeader:   new(false),
			RewriteRedirects: new(false),
			Targets:          &targets,
		}
		applyServicePolicies(policies, &proxyReq)

		// Upsert by domain: update the existing service if one already serves
		// this hostname, otherwise create it. Falling through to Create after
		// an Update would re-submit the same domain and the API rejects it with
		// "domain already taken".
		if err := r.upsertProxyService(ctx, proxyServices, string(hostname), proxyReq); err != nil {
			return err
		}
	}
	return nil
}

func (r *HTTPRouteReconciler) upsertProxyService(ctx context.Context, proxyServices []api.Service, hostname string, proxyReq api.ServiceRequest) error {
	for _, proxyService := range proxyServices {
		if proxyService.Domain != hostname {
			continue
		}
		if proxyServiceUpToDate(proxyService, proxyReq) {
			return nil
		}
		_, err := r.Netbird.ReverseProxyServices.Update(ctx, proxyService.Id, proxyReq)
		return err
	}
	_, err := r.Netbird.ReverseProxyServices.Create(ctx, proxyReq)
	return err
}

func (r *HTTPRouteReconciler) reconcileDelete(ctx context.Context, sp *patch.SerialPatcher, hr *gwv1.HTTPRoute) (ctrl.Result, error) {
	// Index all proxy services.
	proxyServices, err := r.Netbird.ReverseProxyServices.List(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	proxyIdx := map[string]string{}
	for _, proxyService := range proxyServices {
		proxyIdx[proxyService.Domain] = proxyService.Id
	}

	for _, parent := range hr.Spec.ParentRefs {
		gw, err := gatewayutil.GetParentGateway(ctx, r.Client, parent, hr.Namespace, GatewayControllerName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if gw == nil {
			continue
		}

		// Detach the route from each backend Service's NetworkResource.
		svcIdx, err := r.indexBackendServices(ctx, hr, true)
		if err != nil {
			return ctrl.Result{}, err
		}
		for _, svc := range svcIdx {
			netResource := &nbv1alpha1.NetworkResource{
				ObjectMeta: metav1.ObjectMeta{
					Name:      svc.Name,
					Namespace: svc.Namespace,
				},
			}
			err = r.Client.Get(ctx, client.ObjectKeyFromObject(netResource), netResource)
			if err != nil {
				return ctrl.Result{}, err
			}
			err = controllerutil.RemoveOwnerReference(hr, netResource, r.Scheme())
			if err != nil {
				return ctrl.Result{}, err
			}

			if len(netResource.OwnerReferences) > 1 {
				err = r.Client.Update(ctx, netResource)
				if err != nil {
					return ctrl.Result{}, err
				}
			} else {
				// TODO: Precondition that nothing has changed.
				err := r.Client.Delete(ctx, netResource)
				if err != nil {
					return ctrl.Result{}, err
				}
			}
		}

		// Remove the target from the proxy service.
		for _, hostname := range hr.Spec.Hostnames {
			id, ok := proxyIdx[string(hostname)]
			if !ok {
				continue
			}
			err = r.Netbird.ReverseProxyServices.Delete(ctx, id)
			if err != nil && !netbird.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}

	controllerutil.RemoveFinalizer(hr, k8sutil.Finalizer("httproute"))
	err = sp.Patch(ctx, hr)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// backendPortFor resolves the port a proxy target should connect to: the
// HTTPRoute backendRef port (port, or 0 if it was unset), falling back to the
// Service's first declared port.
func backendPortFor(svc corev1.Service, port int32) int {
	if port != 0 {
		return int(port)
	}
	if len(svc.Spec.Ports) > 0 {
		return int(svc.Spec.Ports[0].Port)
	}
	return 0
}

// +kubebuilder:rbac:groups=netbird.io,resources=nbservicepolicies,verbs=get;list;watch

// SetupWithManager sets up the controller with the Manager.
func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gwv1.HTTPRoute{}).
		Watches(&nbv1alpha1.NBServicePolicy{},
			handler.EnqueueRequestsFromMapFunc(routesForServicePolicy),
			// Only spec changes (and create/delete) should re-reconcile the
			// route; ignore the status-only writes from the policy controller.
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
