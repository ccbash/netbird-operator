// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
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
// closure renders HTTP backends as cluster targets (dialed direct-upstream by
// the proxy) and L4 backends as the advertised NetworkResource (dialed over
// the mesh by the proxy's embedded peer).
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
	isPortRouted := mode == api.ServiceRequestModeTcp || mode == api.ServiceRequestModeUdp
	// PROXY protocol v2 is a tcp/tls-only backend option; the CRD CEL already
	// rejects it elsewhere, so this gate only ever skips it for http/udp.
	proxyProtocol := mode == api.ServiceRequestModeTcp || mode == api.ServiceRequestModeTls

	// L4 (tcp/udp/tls) relays dial their backend through the proxy's embedded
	// mesh peer — DirectUpstream is an HTTP-only transport branch, so an L4
	// cluster target leaves the proxy with nothing dialable. NetBird resolves
	// the paths[0] target only, so an L4 service is exactly one backend.
	if !isHTTP && len(rps.Spec.Backends) > 1 {
		return fmt.Errorf("mode %s supports exactly one backend (NetBird relays L4 connections to a single target)", mode)
	}

	targets := make([]api.ServiceTarget, 0, len(rps.Spec.Backends))
	portLabel := "" // backend port name (or number) for the L4 per-port domain
	for _, b := range rps.Spec.Backends {
		if !isHTTP {
			// The single L4 target references the backend's advertised
			// NetworkResource by id: NetBird itself resolves it to the routed
			// LoadBalancer IP, so the relay dials an IP literal over the mesh
			// (no per-connection DNS lookup in the embedded netstack).
			resourceID, port, name, err := resolveL4Backend(ctx, c, rps.Namespace, b.ServiceRef.Name, b.Port)
			if err != nil {
				return err
			}
			// Label the per-port domain by the backend port's name, falling
			// back to the number.
			if portLabel = name; portLabel == "" {
				portLabel = strconv.Itoa(port)
			}
			target := api.ServiceTarget{
				Enabled:    true,
				Port:       port,
				Protocol:   proto,
				TargetType: api.ServiceTargetTargetTypeHost,
				TargetId:   resourceID,
			}
			// Mirror the CRD's proxyProtocol verbatim so the translation is
			// transparent: nil leaves the NetBird default, an explicit
			// true/false is sent as-is.
			if proxyProtocol && rps.Spec.ProxyProtocol != nil {
				target.Options = &api.ServiceTargetOptions{ProxyProtocol: rps.Spec.ProxyProtocol}
			}
			targets = append(targets, target)
			continue
		}

		host, port, _, err := resolveBackend(ctx, c, rps.Namespace, b.ServiceRef.Name, b.Port)
		if err != nil {
			return err
		}
		direct := true
		target := api.ServiceTarget{
			Enabled:    true,
			Host:       &host,
			Port:       port,
			Protocol:   proto,
			TargetType: api.ServiceTargetTargetTypeCluster,
			// A cluster target references the cluster's CNAME address (e.g.
			// gate.example.com), not cluster.Id which is a single proxy node.
			TargetId: cluster.Address,
			Options:  &api.ServiceTargetOptions{DirectUpstream: &direct},
		}
		if b.Path != "" {
			path := b.Path
			target.Path = &path
		}
		targets = append(targets, target)
	}
	sortServiceTargets(targets)

	// NetBird allows only one service per domain. tcp/udp connections route by
	// listen port (no SNI), so to publish several ports under one hostname the
	// operator gives each port a distinct sibling subdomain
	// "<first-label>-<portLabel>.<parent>" (e.g. mail.example.com + smtp ->
	// mail-smtp.example.com). http (Host routing) and tls (SNI) keep the domain
	// verbatim. The synthesized name still derives the same proxy cluster (it
	// suffix-matches the parent) and needs no public DNS of its own.
	serviceDomain := rps.Spec.Domain
	if isPortRouted && rps.Spec.ListenPort != nil {
		serviceDomain = portLabelDomain(rps.Spec.Domain, portLabel)
	}

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
		req.Private = rps.Spec.Private
		// AccessGroups are the NetBird-Only ACL — they only apply to a private
		// service (the CRD documents them as ignored otherwise). Attaching them
		// to a non-private service flips it into the NetBird-Only state with no
		// effective ACL, so only resolve+send them when private is true.
		if rps.Spec.Private != nil && *rps.Spec.Private {
			accessGroups, err := netbirdutil.GetGroupIDs(ctx, c, nb, rps.Spec.AccessGroups, rps.Namespace)
			if err != nil {
				return err
			}
			if len(accessGroups) > 0 {
				req.AccessGroups = &accessGroups
			}
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

func deleteReverseProxyService(ctx context.Context, nb *netbird.Client, _ client.Client, rps *nbv1alpha1.ReverseProxyService) error {
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

// resolveBackend resolves an HTTP backend Service to the host the proxy
// dials, the port, and that port's name. The host depends on the Service
// type: a type=LoadBalancer Service uses its advertised dualstack mesh FQDN
// (over the NetBird overlay); any other Service (ClusterIP) is reached
// directly at its in-cluster DNS name — the drop-in path for backends fronted
// by an in-cluster proxy. The port name is "" when the port is unnamed or the
// requested port isn't a Service port.
func resolveBackend(ctx context.Context, c client.Client, namespace, svcName string, want int) (host string, port int, portName string, err error) {
	svc := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: svcName}, svc); err != nil {
		if kerrors.IsNotFound(err) {
			return "", 0, "", fmt.Errorf("%w: Service %s/%s not found", errDependencyNotReady, namespace, svcName)
		}
		return "", 0, "", err
	}
	if len(svc.Spec.Ports) == 0 {
		return "", 0, "", fmt.Errorf("%w: Service %s/%s has no ports", errDependencyNotReady, namespace, svcName)
	}

	p := backendPort(svc, want)

	if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
		host, err = backendFQDN(ctx, c, namespace, svcName)
		if err != nil {
			return "", 0, "", err
		}
	} else {
		host = fmt.Sprintf("%s.%s.svc.cluster.local", svcName, namespace)
	}
	return host, int(p.Port), p.Name, nil
}

// backendPort resolves the Service port a backend entry addresses: want
// (backends[].port) wins; 0 falls back to the Service's first port. The
// returned port carries no name when want isn't a declared Service port.
func backendPort(svc *corev1.Service, want int) corev1.ServicePort {
	p := svc.Spec.Ports[0]
	if want != 0 {
		p = corev1.ServicePort{Port: int32(want)} // unmatched explicit port: no name
		for _, sp := range svc.Spec.Ports {
			if int(sp.Port) == want {
				p = sp
				break
			}
		}
	}
	return p
}

// resolveL4Backend resolves an L4 (tcp/udp/tls) backend to the NetBird network
// resource the proxy dials, plus the backend port and that port's name. L4
// relays always run through the proxy's embedded mesh peer (there is no
// direct-upstream branch below HTTP), so the backend must be an advertised
// LoadBalancer Service: the mesh routes its LB IP (never a ClusterIP), and the
// target references the per-family NetworkResource by id so NetBird resolves
// it to that IP — the relay then dials an IP literal, skipping the embedded
// netstack's DNS resolver (single upstream, hard 5s query timeout) on every
// connection. IPv4 is preferred over IPv6 (every mesh peer has a v4 address;
// v6 needs a dualstack mesh).
func resolveL4Backend(ctx context.Context, c client.Client, namespace, svcName string, want int) (resourceID string, port int, portName string, err error) {
	svc := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: svcName}, svc); err != nil {
		if kerrors.IsNotFound(err) {
			return "", 0, "", fmt.Errorf("%w: Service %s/%s not found", errDependencyNotReady, namespace, svcName)
		}
		return "", 0, "", err
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return "", 0, "", fmt.Errorf("tcp/tls/udp modes require an advertised LoadBalancer backend: Service %s/%s is type %s (the proxy relays L4 connections over the NetBird mesh, which routes only LoadBalancer addresses)",
			namespace, svcName, svc.Spec.Type)
	}
	if len(svc.Spec.Ports) == 0 {
		return "", 0, "", fmt.Errorf("%w: Service %s/%s has no ports", errDependencyNotReady, namespace, svcName)
	}
	p := backendPort(svc, want)

	var resources nbv1alpha1.NetworkResourceList
	if err := c.List(ctx, &resources, client.InNamespace(namespace), client.MatchingLabels{lbServiceLabel: svcName}); err != nil {
		return "", 0, "", err
	}
	if len(resources.Items) == 0 {
		return "", 0, "", fmt.Errorf("%w: Service %s/%s not advertised (no NetworkResource)", errDependencyNotReady, namespace, svcName)
	}
	pick := preferL4Resource(resources.Items)
	if pick.Status.ResourceID == "" {
		return "", 0, "", fmt.Errorf("%w: NetworkResource %s/%s not ready (no NetBird resource id)", errDependencyNotReady, namespace, pick.Name)
	}
	return pick.Status.ResourceID, int(p.Port), p.Name, nil
}

// preferL4Resource picks the advertised NetworkResource an L4 target
// references: IPv4 before IPv6, name as the tiebreaker, so an unchanged spec
// keeps rendering the identical target.
func preferL4Resource(items []nbv1alpha1.NetworkResource) *nbv1alpha1.NetworkResource {
	sort.Slice(items, func(i, j int) bool {
		fi, fj := ipFamilyOf(items[i].Spec.Address), ipFamilyOf(items[j].Spec.Address)
		if fi != fj {
			return fi == corev1.IPv4Protocol
		}
		return items[i].Name < items[j].Name
	})
	return &items[0]
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

// portLabelDomain builds the L4 per-port domain by appending "-<label>" to the
// first label of host: portLabelDomain("mail.example.com", "smtp") =
// "mail-smtp.example.com". The result is a sibling under host's parent, so that
// parent (e.g. example.com) must be the registered NetBird custom domain — or a
// cluster address — for the synthesized name to derive the proxy cluster.
func portLabelDomain(host, label string) string {
	first, rest, found := strings.Cut(host, ".")
	if !found {
		return first + "-" + label
	}
	return first + "-" + label + "." + rest
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
