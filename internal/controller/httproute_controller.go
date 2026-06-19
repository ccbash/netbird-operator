// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"errors"

	"github.com/fluxcd/pkg/runtime/patch"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/record"
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
	"github.com/netbirdio/kubernetes-operator/internal/netbirdutil"
)

type HTTPRouteReconciler struct {
	client.Client

	Netbird  *netbird.Client
	Recorder record.EventRecorder
}

func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

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
// upserts an up-to-date reverse-proxy service per hostname whose `cluster`
// targets dial the backend Services. A zero Result means "this parent is done,
// continue"; a non-zero Result asks the caller to requeue.
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

	logger.V(1).Info("reconciling HTTPRoute", "gateway", gw.Name)

	controllerutil.AddFinalizer(hr, k8sutil.Finalizer("httproute"))
	if err := sp.Patch(ctx, hr); err != nil {
		return ctrl.Result{}, err
	}

	// Tolerate missing backend Services: a referenced Service may not exist yet
	// (or at all). Wire up the resolvable backends and requeue for the rest
	// instead of failing the whole route with an error + stack trace.
	svcIdx, err := r.indexBackendServices(ctx, hr, true)
	if err != nil {
		return ctrl.Result{}, err
	}
	missing := missingBackendNames(hr, svcIdx)
	if len(missing) > 0 {
		logger.Info("backend Service(s) not found; routing the resolvable backends and retrying", "missing", missing)
		recordEvent(r.Recorder, hr, reasonBackendNotFound,
			"Backend Service(s) %v not found; routing resolvable backends and retrying", missing)
	}
	if len(svcIdx) == 0 {
		return ctrl.Result{RequeueAfter: backendRetry}, nil
	}

	// The attached NBServicePolicies carry the proxy cluster and the upstream
	// form (newest-first; the oldest non-empty wins, matching applyServicePolicies).
	policies, err := r.servicePoliciesFor(ctx, hr)
	if err != nil {
		return ctrl.Result{}, err
	}

	clusterAddr := resolveProxyCluster(policies)
	if clusterAddr == "" {
		recordEvent(r.Recorder, hr, reasonDependencyNotReady,
			"No proxyCluster set on an attached NBServicePolicy; cannot expose over HTTP")
		return ctrl.Result{RequeueAfter: dependencyRetry}, nil
	}
	cluster, err := netbirdutil.GetProxyClusterByAddress(ctx, r.Netbird, clusterAddr)
	if errors.Is(err, netbirdutil.ErrProxyClusterNotFound) {
		recordEvent(r.Recorder, hr, reasonDependencyNotReady,
			"Reverse-proxy cluster %q not found", clusterAddr)
		return ctrl.Result{RequeueAfter: dependencyRetry}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Resolve each backend's proxy-target Host: the Service FQDN (default — the
	// proxy resolves it via NetBird DNS, IPv4/IPv6 transparent) or the ClusterIP.
	hostByService, err := r.upstreamHosts(ctx, netRouter, svcIdx, resolveUpstream(policies))
	if err != nil {
		if errors.Is(err, netbirdutil.ErrZoneNotFound) {
			recordEvent(r.Recorder, hr, reasonDependencyNotReady,
				"Referenced NetworkRouter DNS zone does not exist yet")
			return ctrl.Result{RequeueAfter: dependencyRetry}, nil
		}
		return ctrl.Result{}, err
	}

	targets := buildClusterTargets(logger, hr, svcIdx, hostByService, cluster.Id)
	if err := r.reconcileProxyServices(ctx, hr, targets, policies); err != nil {
		return ctrl.Result{}, err
	}

	// Some backends weren't resolvable; retry so they're wired up once present.
	if len(missing) > 0 {
		return ctrl.Result{RequeueAfter: backendRetry}, nil
	}
	return ctrl.Result{}, nil
}

// resolveProxyCluster folds the attached policies to a single proxy-cluster
// address (newest-first; the oldest non-empty wins).
func resolveProxyCluster(policies []nbv1alpha1.NBServicePolicy) string {
	var addr string
	for i := range policies {
		if policies[i].Spec.ProxyCluster != "" {
			addr = policies[i].Spec.ProxyCluster
		}
	}
	return addr
}

// resolveUpstream folds the attached policies to a single upstream form,
// defaulting to hostname (newest-first; the oldest non-empty wins).
func resolveUpstream(policies []nbv1alpha1.NBServicePolicy) nbv1alpha1.UpstreamMode {
	mode := nbv1alpha1.UpstreamModeHostname
	for i := range policies {
		if policies[i].Spec.Upstream != "" {
			mode = policies[i].Spec.Upstream
		}
	}
	return mode
}

// upstreamHosts returns the reverse-proxy target Host for each backend Service.
// For the hostname form it publishes the Service's A/AAAA records under its FQDN
// (so the proxy resolves it via NetBird DNS, IPv4/IPv6 transparent) and uses the
// FQDN; for the ip form it uses the ClusterIP directly (no DNS).
func (r *HTTPRouteReconciler) upstreamHosts(ctx context.Context, netRouter *nbv1alpha1.NetworkRouter, svcIdx map[string]corev1.Service, upstream nbv1alpha1.UpstreamMode) (map[string]string, error) {
	hosts := map[string]string{}
	if upstream == nbv1alpha1.UpstreamModeIP {
		for name, svc := range svcIdx {
			hosts[name] = svc.Spec.ClusterIP
		}
		return hosts, nil
	}
	zone, err := netbirdutil.GetDNSZoneByName(ctx, r.Netbird, netRouter.Spec.DNSZoneRef.Name)
	if err != nil {
		return nil, err
	}
	for name := range svcIdx {
		svc := svcIdx[name]
		fqdn := serviceFQDN(svc.Name, svc.Namespace, zone.Domain)
		if _, err := reconcileZoneRecords(ctx, r.Netbird, zone.Id, fqdn, clusterIPsOf(&svc)); err != nil {
			return nil, err
		}
		hosts[name] = fqdn
	}
	return hosts, nil
}

// missingBackendNames returns the distinct backendRef Service names that are not
// present in the resolved set.
func missingBackendNames(hr *gwv1.HTTPRoute, found map[string]corev1.Service) []string {
	var missing []string
	seen := map[string]bool{}
	for _, rule := range hr.Spec.Rules {
		for _, ref := range rule.BackendRefs {
			name := string(ref.Name)
			if _, ok := found[name]; ok || seen[name] {
				continue
			}
			seen[name] = true
			missing = append(missing, name)
		}
	}
	return missing
}

// indexBackendServices returns the Services referenced by the route's
// backendRefs, deduplicated by name (one NetworkResource is created per
// Service). When tolerateMissing is set, Services that no longer exist are
// skipped instead of erroring — used on the delete path.
func (r *HTTPRouteReconciler) indexBackendServices(ctx context.Context, hr *gwv1.HTTPRoute, tolerateMissing bool) (map[string]corev1.Service, error) {
	var names []string
	for _, rule := range hr.Spec.Rules {
		for _, ref := range rule.BackendRefs {
			names = append(names, string(ref.Name))
		}
	}
	return collectBackendServices(ctx, r.Client, hr.Namespace, names, tolerateMissing)
}

// buildClusterTargets builds one reverse-proxy `cluster` target per backendRef:
// the proxy cluster (TargetId) dials the backend Host (FQDN or ClusterIP) via
// its embedded NetBird client. The rule's path prefix is carried so path-based
// routes (e.g. /push/ -> notify-push, / -> app) resolve to the right backend.
func buildClusterTargets(logger logr.Logger, hr *gwv1.HTTPRoute, svcIdx map[string]corev1.Service, hostByService map[string]string, clusterID string) []api.ServiceTarget {
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
			host := hostByService[svc.Name]
			targets = append(targets, api.ServiceTarget{
				Enabled:    true,
				Host:       &host,
				Path:       path,
				Port:       backendPortFor(svc, refPort),
				Protocol:   api.ServiceTargetProtocolHttp,
				TargetId:   clusterID,
				TargetType: api.ServiceTargetTargetTypeCluster,
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

	// Resolve the private-service access groups (by name/id/ref) to NetBird
	// group IDs once for the route; the same set applies to every hostname.
	var accessGroups []string
	if refs := accessGroupRefs(policies); len(refs) > 0 {
		accessGroups, err = netbirdutil.GetGroupIDs(ctx, r.Client, r.Netbird, refs, hr.Namespace)
		if err != nil {
			return err
		}
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
		if len(accessGroups) > 0 {
			proxyReq.AccessGroups = &accessGroups
		}

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

	// Delete the reverse-proxy service for each hostname this route owns. This
	// runs regardless of whether the parent Gateway still exists — otherwise a
	// Gateway deleted before its routes would orphan the proxy services on the
	// NetBird control plane (the finalizer is removed below either way).
	for _, hostname := range hr.Spec.Hostnames {
		id, ok := proxyIdx[string(hostname)]
		if !ok {
			continue
		}
		if err := r.Netbird.ReverseProxyServices.Delete(ctx, id); err != nil && !netbird.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// Delete the DNS records published for each backend (best-effort).
	if err := r.cleanupRouteDNS(ctx, hr); err != nil {
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(hr, k8sutil.Finalizer("httproute"))
	if err := sp.Patch(ctx, hr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// cleanupRouteDNS removes the A/AAAA records published for the route's backends.
// It resolves the zone via a parent Gateway's NetworkRouter; if the Gateway or
// router is already gone the records are left for manual cleanup rather than
// blocking deletion.
func (r *HTTPRouteReconciler) cleanupRouteDNS(ctx context.Context, hr *gwv1.HTTPRoute) error {
	svcIdx, err := r.indexBackendServices(ctx, hr, true)
	if err != nil {
		return err
	}
	if len(svcIdx) == 0 {
		return nil
	}
	for _, parent := range hr.Spec.ParentRefs {
		gw, err := gatewayutil.GetParentGateway(ctx, r.Client, parent, hr.Namespace, GatewayControllerName)
		if kerrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if gw == nil {
			continue
		}
		netRouter, err := gatewayutil.GetGatewayNetworkRouter(ctx, r.Client, gw)
		if kerrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		zone, err := netbirdutil.GetDNSZoneByName(ctx, r.Netbird, netRouter.Spec.DNSZoneRef.Name)
		if errors.Is(err, netbirdutil.ErrZoneNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		for _, svc := range svcIdx {
			fqdn := serviceFQDN(svc.Name, svc.Namespace, zone.Domain)
			if err := deleteZoneRecords(ctx, r.Netbird, zone.Id, fqdn); err != nil {
				return err
			}
		}
	}
	return nil
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
		WithLogConstructor(logConstructor(mgr, "HTTPRoute")).
		Watches(&nbv1alpha1.NBServicePolicy{},
			handler.EnqueueRequestsFromMapFunc(routesForServicePolicy),
			// Only spec changes (and create/delete) should re-reconcile the
			// route; ignore the status-only writes from the policy controller.
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
