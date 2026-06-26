// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/netbirdutil"
)

// +kubebuilder:rbac:groups=netbird.io,resources=reverseproxyservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=netbird.io,resources=reverseproxyservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=netbird.io,resources=reverseproxyservices/finalizers,verbs=update

// NewReverseProxyServiceReconciler builds the reconciler for the
// ReverseProxyService CRD. It reuses the generic mirror reconciler — its apply
// closure targets each backend LoadBalancer Service's DNSRecord FQDN.
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
	cluster, err := netbirdutil.GetProxyClusterByAddress(ctx, nb, rps.Spec.ProxyCluster)
	if errors.Is(err, netbirdutil.ErrProxyClusterNotFound) {
		return fmt.Errorf("%w: proxy cluster %s not found", errDependencyNotReady, rps.Spec.ProxyCluster)
	}
	if err != nil {
		return err
	}

	mode, proto := serviceMode(rps.Spec.Mode)
	isHTTP := mode == api.ServiceRequestModeHttp
	// PROXY protocol v2 is a tcp/tls-only backend option; the CRD CEL already
	// rejects it elsewhere, so this gate only ever skips it for http/udp.
	proxyProtocol := mode == api.ServiceRequestModeTcp || mode == api.ServiceRequestModeTls

	// NetBird allows only one service per domain. tcp/udp connections route by
	// listen port (no SNI), so to publish several ports under one hostname the
	// operator registers each port under a distinct per-port subdomain of the
	// shared public host. http (Host routing) and tls (SNI) must keep the domain
	// verbatim. The synthesized subdomain still derives the same proxy cluster
	// (suffix match) and needs no public DNS — clients reach the shared host.
	serviceDomain := serviceDomain(mode, rps.Spec.Domain, rps.Spec.ListenPort)

	targets := make([]api.ServiceTarget, 0, len(rps.Spec.Backends))
	for _, b := range rps.Spec.Backends {
		fqdn, err := backendFQDN(ctx, c, rps.Namespace, b.ServiceRef.Name)
		if err != nil {
			return err
		}
		port, err := backendPort(ctx, c, rps.Namespace, b.ServiceRef.Name, b.Port)
		if err != nil {
			return err
		}
		host := fqdn
		direct := true
		opts := &api.ServiceTargetOptions{DirectUpstream: &direct}
		// Mirror the CRD's proxyProtocol verbatim onto every target so the
		// translation is transparent: nil leaves the NetBird default, an
		// explicit true/false is sent as-is.
		if proxyProtocol && rps.Spec.ProxyProtocol != nil {
			opts.ProxyProtocol = rps.Spec.ProxyProtocol
		}
		target := api.ServiceTarget{
			Enabled:    true,
			Host:       &host,
			Port:       port,
			Protocol:   proto,
			TargetType: api.ServiceTargetTargetTypeCluster,
			// A cluster target references the cluster's CNAME address (e.g.
			// gate.example.com), not cluster.Id which is a single proxy node.
			TargetId: cluster.Address,
			Options:  opts,
		}
		// Path is HTTP-only; L4 targets route by listen port, not URL path.
		if isHTTP && b.Path != "" {
			path := b.Path
			target.Path = &path
		}
		targets = append(targets, target)
	}
	sortServiceTargets(targets)

	req := api.ServiceRequest{
		Domain:           serviceDomain,
		Enabled:          true,
		Mode:             &mode,
		Name:             rps.Name,
		Targets:          &targets,
		PassHostHeader:   rps.Spec.PassHostHeader,
		RewriteRedirects: rps.Spec.RewriteRedirects,
	}

	// Private + the access-group auto-ACL is an HTTP-only NetBird feature
	// (ServiceRequest.private requires mode=http). For L4 modes access is
	// governed by AccessRestrictions / proxy-cluster reachability, and the
	// public listen port is set explicitly.
	if isHTTP {
		accessGroups, err := netbirdutil.GetGroupIDs(ctx, c, nb, rps.Spec.AccessGroups, rps.Namespace)
		if err != nil {
			return err
		}
		req.Private = rps.Spec.Private
		if len(accessGroups) > 0 {
			req.AccessGroups = &accessGroups
		}
	} else if rps.Spec.ListenPort != nil {
		req.ListenPort = rps.Spec.ListenPort
	}

	if ar := accessRestrictionsFor(rps.Spec.CrowdsecMode, rps.Spec.AccessRestrictions); ar != nil {
		req.AccessRestrictions = ar
	}

	// Surface the registered domain so the per-port synthesis is transparent.
	rps.Status.ServiceDomain = serviceDomain

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

// backendFQDN returns the dualstack DNS name advertised for a LoadBalancer
// Service (the DNSRecord the LoadBalancer controller published), or
// errDependencyNotReady while the Service is not yet advertised.
func backendFQDN(ctx context.Context, c client.Client, namespace, svcName string) (string, error) {
	var records nbv1alpha1.DNSRecordList
	if err := c.List(ctx, &records, client.InNamespace(namespace), client.MatchingLabels{lbServiceLabel: svcName}); err != nil {
		return "", err
	}
	if len(records.Items) == 0 {
		return "", fmt.Errorf("%w: Service %s/%s not advertised (no DNSRecord)", errDependencyNotReady, namespace, svcName)
	}
	return records.Items[0].Spec.Name, nil
}

// backendPort returns want, or the Service's first port when want is 0. A
// multi-port backend defaults to the first port (usually the right one for an
// HTTP face) — set backends[].port to target a specific one, which you'll
// normally want for L4 services fanning several ports out across CRs.
func backendPort(ctx context.Context, c client.Client, namespace, svcName string, want int) (int, error) {
	if want != 0 {
		return want, nil
	}
	svc := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: svcName}, svc); err != nil {
		return 0, err
	}
	if len(svc.Spec.Ports) == 0 {
		return 0, fmt.Errorf("%w: Service %s/%s has no ports", errDependencyNotReady, namespace, svcName)
	}
	return int(svc.Spec.Ports[0].Port), nil
}

// sortServiceTargets orders targets deterministically so an unchanged reconcile
// renders an identical ServiceRequest.
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

// serviceDomain returns the domain to register with NetBird. http (Host
// routing) and tls (SNI) keep the public host verbatim. tcp/udp route by listen
// port and NetBird permits only one service per domain, so each port is
// published under a distinct subdomain "<mode>-<listenPort>.<host>" — unique
// across mode+port, and still a subdomain of host so it derives the same proxy
// cluster (suffix match) without needing its own public DNS.
func serviceDomain(mode api.ServiceRequestMode, host string, listenPort *int) string {
	if (mode == api.ServiceRequestModeTcp || mode == api.ServiceRequestModeUdp) && listenPort != nil {
		return fmt.Sprintf("%s-%d.%s", mode, *listenPort, host)
	}
	return host
}

// serviceMode maps the CRD's mode onto the NetBird API request mode and the
// protocol used to reach the backend. An empty mode defaults to HTTP, so
// existing HTTP services keep their behavior unchanged. TLS mode terminates at
// the proxy and reaches the backend over plain TCP.
func serviceMode(m nbv1alpha1.ReverseProxyMode) (api.ServiceRequestMode, api.ServiceTargetProtocol) {
	switch m {
	case nbv1alpha1.ReverseProxyModeTCP:
		return api.ServiceRequestModeTcp, api.ServiceTargetProtocolTcp
	case nbv1alpha1.ReverseProxyModeTLS:
		return api.ServiceRequestModeTls, api.ServiceTargetProtocolTcp
	case nbv1alpha1.ReverseProxyModeUDP:
		return api.ServiceRequestModeUdp, api.ServiceTargetProtocolUdp
	default:
		return api.ServiceRequestModeHttp, api.ServiceTargetProtocolHttp
	}
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
