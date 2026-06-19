// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/gatewayutil"
	"github.com/netbirdio/kubernetes-operator/internal/netbirdutil"
)

// +kubebuilder:rbac:groups=netbird.io,resources=reverseproxyservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=netbird.io,resources=reverseproxyservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=netbird.io,resources=reverseproxyservices/finalizers,verbs=update

// NewReverseProxyServiceReconciler builds the reconciler for the
// ReverseProxyService CRD. It reuses the generic mirror reconciler — its apply
// closure derives the service's targets from the referenced route's backends.
func NewReverseProxyServiceReconciler(c client.Client, nb *netbird.Client, rec record.EventRecorder) *MirrorReconciler[*nbv1alpha1.ReverseProxyService] {
	return &MirrorReconciler[*nbv1alpha1.ReverseProxyService]{
		Client:   c,
		Netbird:  nb,
		Recorder: rec,
		m: mirror[*nbv1alpha1.ReverseProxyService]{
			kind:      "ReverseProxyService",
			finalizer: "reverseproxyservice",
			newObject: func() *nbv1alpha1.ReverseProxyService { return &nbv1alpha1.ReverseProxyService{} },
			apply:     applyReverseProxyService,
			del:       deleteReverseProxyService,
		},
	}
}

func applyReverseProxyService(ctx context.Context, nb *netbird.Client, c client.Client, rps *nbv1alpha1.ReverseProxyService) error {
	backends, zoneDomain, hostname, err := resolveRouteForProxy(ctx, c, rps.Namespace, rps.Spec.RouteRef)
	if err != nil {
		return err
	}

	cluster, err := netbirdutil.GetProxyClusterByAddress(ctx, nb, rps.Spec.ProxyCluster)
	if errors.Is(err, netbirdutil.ErrProxyClusterNotFound) {
		return fmt.Errorf("%w: proxy cluster %s not found", errDependencyNotReady, rps.Spec.ProxyCluster)
	}
	if err != nil {
		return err
	}

	domain := rps.Spec.Domain
	if domain == "" {
		domain = hostname
	}
	if domain == "" {
		return fmt.Errorf("spec.domain is required (route %s has no hostname)", rps.Spec.RouteRef.Name)
	}

	upstream := rps.Spec.Upstream
	if upstream == "" {
		upstream = nbv1alpha1.UpstreamModeHostname
	}

	mode := api.ServiceRequestModeHttp
	protocol := api.ServiceTargetProtocolHttp
	if rps.Spec.RouteRef.Kind == "TCPRoute" {
		mode = api.ServiceRequestModeTcp
		protocol = api.ServiceTargetProtocolTcp
	}

	targets := make([]api.ServiceTarget, 0, len(backends))
	for _, b := range backends {
		host := serviceFQDN(b.svc.Name, b.svc.Namespace, zoneDomain)
		if upstream == nbv1alpha1.UpstreamModeIP {
			host = b.svc.Spec.ClusterIP
		}
		hostVal := host
		direct := true
		targets = append(targets, api.ServiceTarget{
			Enabled:    true,
			Host:       &hostVal,
			Port:       b.port,
			Protocol:   protocol,
			TargetType: api.ServiceTargetTargetTypeCluster,
			TargetId:   cluster.Id,
			Options:    &api.ServiceTargetOptions{DirectUpstream: &direct},
		})
	}
	sortServiceTargets(targets)

	accessGroups, err := netbirdutil.GetGroupIDs(ctx, c, nb, rps.Spec.AccessGroups, rps.Namespace)
	if err != nil {
		return err
	}

	modeVal := mode
	req := api.ServiceRequest{
		Domain:           domain,
		Enabled:          true,
		Mode:             &modeVal,
		Name:             rps.Name,
		Targets:          &targets,
		Private:          rps.Spec.Private,
		PassHostHeader:   rps.Spec.PassHostHeader,
		RewriteRedirects: rps.Spec.RewriteRedirects,
	}
	if len(accessGroups) > 0 {
		req.AccessGroups = &accessGroups
	}
	if ar := accessRestrictionsFor(rps.Spec.CrowdsecMode, rps.Spec.AccessRestrictions); ar != nil {
		req.AccessRestrictions = ar
	}

	if rps.Status.ServiceID != "" {
		resp, err := nb.ReverseProxyServices.Update(ctx, rps.Status.ServiceID, req)
		if err == nil {
			rps.Status.ServiceID = resp.Id
			return nil
		}
		if !netbird.IsNotFound(err) {
			return err
		}
	}
	resp, err := nb.ReverseProxyServices.Create(ctx, req)
	if err != nil {
		return err
	}
	rps.Status.ServiceID = resp.Id
	return nil
}

func deleteReverseProxyService(ctx context.Context, nb *netbird.Client, rps *nbv1alpha1.ReverseProxyService) error {
	if rps.Status.ServiceID == "" {
		return nil
	}
	return nb.ReverseProxyServices.Delete(ctx, rps.Status.ServiceID)
}

// routeBackend pairs a resolved backend Service with its target port.
type routeBackend struct {
	svc  corev1.Service
	port int
}

// resolveRouteForProxy resolves the route named by ref, its parent netbird
// Gateway's zone domain and hostname, and its backend Services. Missing or
// not-programmed dependencies are returned as errDependencyNotReady.
func resolveRouteForProxy(ctx context.Context, c client.Client, namespace string, ref nbv1alpha1.RouteReference) ([]routeBackend, string, string, error) {
	var parents []gwv1.ParentReference
	type backendRef struct {
		name string
		port int
	}
	var backendRefs []backendRef
	var hostname string

	switch ref.Kind {
	case "HTTPRoute":
		hr := &gwv1.HTTPRoute{}
		err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, hr)
		if kerrors.IsNotFound(err) {
			return nil, "", "", fmt.Errorf("%w: HTTPRoute %s/%s not found", errDependencyNotReady, namespace, ref.Name)
		}
		if err != nil {
			return nil, "", "", err
		}
		parents = hr.Spec.ParentRefs
		if len(hr.Spec.Hostnames) > 0 {
			hostname = string(hr.Spec.Hostnames[0])
		}
		for _, rule := range hr.Spec.Rules {
			for _, b := range rule.BackendRefs {
				backendRefs = append(backendRefs, backendRef{string(b.Name), portNumber(b.Port)})
			}
		}
	case "TCPRoute":
		tr := &gwv1alpha2.TCPRoute{}
		err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, tr)
		if kerrors.IsNotFound(err) {
			return nil, "", "", fmt.Errorf("%w: TCPRoute %s/%s not found", errDependencyNotReady, namespace, ref.Name)
		}
		if err != nil {
			return nil, "", "", err
		}
		parents = tr.Spec.ParentRefs
		for _, rule := range tr.Spec.Rules {
			for _, b := range rule.BackendRefs {
				backendRefs = append(backendRefs, backendRef{string(b.Name), portNumber(b.Port)})
			}
		}
	default:
		return nil, "", "", fmt.Errorf("unsupported routeRef kind %q", ref.Kind)
	}

	var gw *gwv1.Gateway
	for _, p := range parents {
		g, err := gatewayutil.GetParentGateway(ctx, c, p, namespace, GatewayControllerName)
		if err != nil {
			return nil, "", "", err
		}
		if g != nil {
			gw = g
			break
		}
	}
	if gw == nil {
		return nil, "", "", fmt.Errorf("%w: route %s has no netbird Gateway parent", errDependencyNotReady, ref.Name)
	}
	if !meta.IsStatusConditionTrue(gw.Status.Conditions, string(gwv1.GatewayConditionProgrammed)) {
		return nil, "", "", fmt.Errorf("%w: Gateway %s is not programmed", errDependencyNotReady, gw.Name)
	}

	zone := &nbv1alpha1.DNSZone{}
	err := c.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: gatewayDNSZoneName(gw)}, zone)
	if kerrors.IsNotFound(err) {
		return nil, "", "", fmt.Errorf("%w: DNSZone for Gateway %s not found", errDependencyNotReady, gw.Name)
	}
	if err != nil {
		return nil, "", "", err
	}

	var backends []routeBackend
	for _, br := range backendRefs {
		svc := corev1.Service{}
		err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: br.name}, &svc)
		if kerrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, "", "", err
		}
		backends = append(backends, routeBackend{svc: svc, port: br.port})
	}
	return backends, zone.Spec.Domain, hostname, nil
}

func portNumber(p *gwv1.PortNumber) int {
	if p == nil {
		return 0
	}
	return int(*p)
}

// sortServiceTargets orders targets deterministically so an unchanged reconcile
// renders an identical ServiceRequest (cluster targets share a TargetId and
// differ by Host/Port).
func sortServiceTargets(targets []api.ServiceTarget) {
	sort.Slice(targets, func(i, j int) bool {
		hi, hj := "", ""
		if targets[i].Host != nil {
			hi = *targets[i].Host
		}
		if targets[j].Host != nil {
			hj = *targets[j].Host
		}
		if hi != hj {
			return hi < hj
		}
		return targets[i].Port < targets[j].Port
	})
}

// accessRestrictionsFor maps the CRD's restriction fields onto the NetBird API
// type, or returns nil when none are set.
func accessRestrictionsFor(mode *nbv1alpha1.CrowdsecMode, ar *nbv1alpha1.AccessRestrictions) *api.AccessRestrictions {
	if mode == nil && ar == nil {
		return nil
	}
	var out api.AccessRestrictions
	if mode != nil {
		m := api.AccessRestrictionsCrowdsecMode(*mode)
		out.CrowdsecMode = &m
	}
	if ar != nil {
		if len(ar.AllowedCidrs) > 0 {
			v := append([]string(nil), ar.AllowedCidrs...)
			out.AllowedCidrs = &v
		}
		if len(ar.BlockedCidrs) > 0 {
			v := append([]string(nil), ar.BlockedCidrs...)
			out.BlockedCidrs = &v
		}
		if len(ar.AllowedCountries) > 0 {
			v := append([]string(nil), ar.AllowedCountries...)
			out.AllowedCountries = &v
		}
		if len(ar.BlockedCountries) > 0 {
			v := append([]string(nil), ar.BlockedCountries...)
			out.BlockedCountries = &v
		}
	}
	return &out
}
