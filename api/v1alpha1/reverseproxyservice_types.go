// SPDX-License-Identifier: BSD-3-Clause

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CrowdsecMode selects how the proxy cluster's CrowdSec IP-reputation check is
// applied. Only effective when the proxy cluster supports CrowdSec.
// +kubebuilder:validation:Enum=off;observe;enforce
type CrowdsecMode string

const (
	CrowdsecModeOff     CrowdsecMode = "off"
	CrowdsecModeObserve CrowdsecMode = "observe"
	CrowdsecModeEnforce CrowdsecMode = "enforce"
)

// ReverseProxyMode selects the proxy mode. "http" is an L7 reverse proxy
// (path-based routing, TLS terminated at the edge). "tcp"/"tls"/"udp" are L4
// passthrough on a fixed ListenPort — used for non-HTTP backends such as mail
// (SMTP/IMAP/ManageSieve), where the backend terminates TLS itself. Maps to the
// NetBird API ServiceRequest.mode.
// +kubebuilder:validation:Enum=http;tcp;tls;udp
type ReverseProxyMode string

const (
	ReverseProxyModeHTTP ReverseProxyMode = "http"
	ReverseProxyModeTCP  ReverseProxyMode = "tcp"
	ReverseProxyModeTLS  ReverseProxyMode = "tls"
	ReverseProxyModeUDP  ReverseProxyMode = "udp"
)

// AccessRestrictions are connection-level restrictions based on IP address or
// geography, applied to the reverse-proxy service.
type AccessRestrictions struct {
	// AllowedCidrs is a CIDR allowlist. If non-empty, only matching source IPs
	// are allowed. Evaluated before BlockedCidrs.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=43
	// +kubebuilder:validation:XValidation:rule="self.all(c, isCIDR(c))",message="allowedCidrs entries must be valid CIDRs"
	AllowedCidrs []string `json:"allowedCidrs,omitempty"`

	// BlockedCidrs is a CIDR blocklist. Matching source IPs are rejected.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=43
	// +kubebuilder:validation:XValidation:rule="self.all(c, isCIDR(c))",message="blockedCidrs entries must be valid CIDRs"
	BlockedCidrs []string `json:"blockedCidrs,omitempty"`

	// AllowedCountries is an ISO 3166-1 alpha-2 country-code allowlist. If
	// non-empty, only these countries are permitted.
	// +optional
	// +kubebuilder:validation:MaxItems=250
	// +kubebuilder:validation:items:MaxLength=2
	// +kubebuilder:validation:XValidation:rule="self.all(c, c.matches('^[A-Za-z]{2}$'))",message="allowedCountries entries must be ISO 3166-1 alpha-2 codes"
	AllowedCountries []string `json:"allowedCountries,omitempty"`

	// BlockedCountries is an ISO 3166-1 alpha-2 country-code blocklist.
	// +optional
	// +kubebuilder:validation:MaxItems=250
	// +kubebuilder:validation:items:MaxLength=2
	// +kubebuilder:validation:XValidation:rule="self.all(c, c.matches('^[A-Za-z]{2}$'))",message="blockedCountries entries must be ISO 3166-1 alpha-2 codes"
	BlockedCountries []string `json:"blockedCountries,omitempty"`
}

// ReverseProxyBackend names a LoadBalancer Service this service proxies to. The
// Service must be advertised (have a DNSRecord); the proxy targets its dualstack
// FQDN, so IPv4/IPv6 is transparent.
type ReverseProxyBackend struct {
	// ServiceRef names the LoadBalancer Service to proxy to, in the same
	// namespace as the ReverseProxyService.
	ServiceRef corev1.LocalObjectReference `json:"serviceRef"`

	// Port the proxy dials on the backend. Defaults to the Service's first port.
	// +optional
	Port int `json:"port,omitempty"`

	// Path is the URL path prefix this backend serves (HTTP). Defaults to "/".
	// +optional
	Path string `json:"path,omitempty"`
}

// ReverseProxyServiceSpec defines the desired state of ReverseProxyService. It
// is admin-authored — creating one is the explicit decision to expose Services
// through the NetBird reverse proxy, internally or externally. It mirrors the
// NetBird reverse-proxy service API (POST /api/reverse-proxies/services),
// targeting the DNSRecord FQDN that belongs to each backend LoadBalancer Service.
// +kubebuilder:validation:XValidation:rule="!has(self.private) || !self.private || (has(self.accessGroups) && self.accessGroups.size() > 0)",message="accessGroups is required when private is true"
// +kubebuilder:validation:XValidation:rule="!has(self.private) || !self.private || !has(self.mode) || self.mode == 'http'",message="private requires mode http (the NetBird API only supports the auto-ACL on HTTP services)"
// +kubebuilder:validation:XValidation:rule="!has(self.listenPort) || (has(self.mode) && self.mode != 'http')",message="listenPort only applies to L4 modes (tcp/tls/udp)"
// +kubebuilder:validation:XValidation:rule="!has(self.mode) || self.mode == 'http' || (has(self.listenPort) && self.listenPort > 0)",message="tcp/tls/udp modes require a non-zero listenPort"
// +kubebuilder:validation:XValidation:rule="!has(self.proxyProtocol) || !self.proxyProtocol || (has(self.mode) && (self.mode == 'tcp' || self.mode == 'tls'))",message="proxyProtocol (PROXY protocol v2) only applies to tcp/tls modes"
type ReverseProxyServiceSpec struct {
	// Backends are the LoadBalancer Services this service proxies to, by path.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Backends []ReverseProxyBackend `json:"backends"`

	// Mode selects the proxy mode. "http" (default) is an L7 reverse proxy;
	// "tcp"/"tls"/"udp" are L4 passthrough on ListenPort. Expose several L4 ports
	// under one hostname with one CR per port (same Domain, distinct ListenPort).
	// +optional
	// +kubebuilder:default=http
	Mode ReverseProxyMode `json:"mode,omitempty"`

	// ListenPort is the public port the proxy listens on. Required for L4 modes
	// (tcp/tls/udp) — it both fixes the well-known port (e.g. 25/465/993 for
	// mail) and disambiguates the per-port service domain. Ignored for mode=http.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	ListenPort *int `json:"listenPort,omitempty"`

	// ProxyProtocol, when true, makes the proxy prepend a PROXY protocol v2
	// header to each backend connection so the backend sees the real client IP
	// and port instead of the proxy's. Applies to tcp/tls modes only (the
	// NetBird API rejects it elsewhere; HTTP conveys the client IP via
	// X-Forwarded-For). Required for mail backends that enforce SPF/DNSBL,
	// greylist, or log the client address — the backend must be configured to
	// accept PROXY protocol on the listening port.
	// +optional
	ProxyProtocol *bool `json:"proxyProtocol,omitempty"`

	// ProxyCluster is the address of the NetBird reverse-proxy cluster that
	// serves this service, e.g. "gate.example.com". The operator resolves it to
	// a proxy-cluster ID and points the service's targets at it.
	// +kubebuilder:validation:MinLength=1
	ProxyCluster string `json:"proxyCluster"`

	// Domain is the public hostname clients connect to. For mode=http/tls it is
	// the service domain verbatim (HTTP routing / TLS SNI). For mode=tcp/udp it
	// is the shared host: NetBird allows only one service per domain, and L4
	// connections route by listen port (no SNI), so the operator publishes each
	// port under a distinct per-port sibling subdomain
	// <first-label>-<portName>.<parent> — e.g. mail.example.com + the backend's
	// "smtp" port becomes mail-smtp.example.com (the backend Service port's name,
	// or its number when unnamed; shown in status.serviceDomain). Expose several
	// L4 ports under one hostname with one CR per port, all sharing this Domain.
	// For tcp/udp the registered NetBird custom domain (or cluster address) must
	// be the PARENT (e.g. example.com), since the per-port siblings derive the
	// cluster through it; public DNS for the host points at the cluster ingress.
	// +kubebuilder:validation:MinLength=1
	Domain string `json:"domain"`

	// Private, when true, makes the service NetBird-only: inbound peers
	// authenticate via their tunnel identity (no OIDC) and an ACL policy is
	// auto-generated from AccessGroups.
	// +optional
	Private *bool `json:"private,omitempty"`

	// AccessGroups are the NetBird groups whose peers may reach a private
	// service over the tunnel. Required when Private is true; ignored otherwise.
	// +optional
	AccessGroups []GroupReference `json:"accessGroups,omitempty"`

	// CrowdsecMode sets the CrowdSec IP-reputation handling for the service.
	// +optional
	CrowdsecMode *CrowdsecMode `json:"crowdsecMode,omitempty"`

	// AccessRestrictions sets IP/geo connection-level restrictions.
	// +optional
	AccessRestrictions *AccessRestrictions `json:"accessRestrictions,omitempty"`

	// PassHostHeader, when true, forwards the original client Host header to the
	// backend instead of rewriting it to the backend address.
	// +optional
	PassHostHeader *bool `json:"passHostHeader,omitempty"`

	// RewriteRedirects, when true, rewrites Location headers in backend
	// responses to replace the backend address with the public domain.
	// +optional
	RewriteRedirects *bool `json:"rewriteRedirects,omitempty"`
}

// ReverseProxyServiceStatus defines the observed state of ReverseProxyService.
type ReverseProxyServiceStatus struct {
	// ObservedGeneration is the last reconciled generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the conditions for the ReverseProxyService.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ServiceID is the id of the created NetBird reverse-proxy service.
	// +optional
	ServiceID string `json:"serviceID,omitempty"`

	// ServiceDomain is the domain actually registered with NetBird. It equals
	// spec.domain for http/tls, and the synthesized per-port sibling subdomain
	// (<first-label>-<portName>.<parent>, e.g. mail-smtp.example.com) for tcp/udp.
	// +optional
	ServiceDomain string `json:"serviceDomain,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource
// +kubebuilder:printcolumn:name="Domain",type="string",JSONPath=".spec.domain",description=""
// +kubebuilder:printcolumn:name="Service Domain",type="string",JSONPath=".status.serviceDomain",description="",priority=1
// +kubebuilder:printcolumn:name="Private",type="boolean",JSONPath=".spec.private",description=""
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""

// ReverseProxyService publishes LoadBalancer Services through the NetBird
// reverse proxy, internally or externally. It is the admin's expose-or-not
// decision.
type ReverseProxyService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec ReverseProxyServiceSpec `json:"spec"`

	// +kubebuilder:default={"observedGeneration":-1}
	Status ReverseProxyServiceStatus `json:"status,omitempty"`
}

// GetConditions returns the status conditions of the object.
func (s *ReverseProxyService) GetConditions() []metav1.Condition {
	return s.Status.Conditions
}

// SetConditions sets the status conditions on the object.
func (s *ReverseProxyService) SetConditions(conditions []metav1.Condition) {
	s.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// ReverseProxyServiceList contains a list of ReverseProxyService.
type ReverseProxyServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ReverseProxyService `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ReverseProxyService{}, &ReverseProxyServiceList{})
}
