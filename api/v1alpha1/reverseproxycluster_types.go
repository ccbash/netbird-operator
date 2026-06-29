// SPDX-License-Identifier: BSD-3-Clause

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReverseProxyClusterSpec defines the desired state of ReverseProxyCluster. It
// is admin-authored: creating one deploys and enrolls a NetBird bring-your-own
// reverse proxy (the netbirdio/reverse-proxy image) and registers it as the
// account's own proxy cluster. The proxy runs behind a Service type=LoadBalancer
// (its ingress IP comes from the cluster's LB-IPAM) and is reached over the
// mesh; ReverseProxyService.proxyCluster targets its ClusterAddress.
type ReverseProxyClusterSpec struct {
	// ClusterAddress is the address the proxy registers under (the proxy's
	// NB_PROXY_DOMAIN), e.g. "gate.ccbash.cloud". ReverseProxyService.proxyCluster
	// references it. It must sit under Domain so service domains derive this
	// cluster.
	// +kubebuilder:validation:MinLength=1
	ClusterAddress string `json:"clusterAddress"`

	// Domain is the DNS zone the proxy fronts, e.g. "ccbash.cloud". The operator
	// ensures a DNSZone for it (unless ZoneRef is set) and creates the proxy's A
	// record (ClusterAddress -> the LoadBalancer IP) plus a catch-all
	// (*.Domain -> ClusterAddress) so any service hostname resolves to the proxy.
	// +kubebuilder:validation:MinLength=1
	Domain string `json:"domain"`

	// ZoneRef references an existing DNSZone for Domain instead of creating one.
	// +optional
	ZoneRef *CrossNamespaceReference `json:"zoneRef,omitempty"`

	// CertSecretName is a kubernetes.io/tls Secret (tls.crt/tls.key) in the same
	// namespace — typically a cert-manager wildcard for Domain — mounted into the
	// proxy as its static TLS certificate. The proxy does no ACME.
	// +optional
	CertSecretName string `json:"certSecretName,omitempty"`

	// Groups are NetBird groups the proxy's advertised LoadBalancer resource
	// joins, so access policies can target it.
	// +optional
	Groups []GroupReference `json:"groups,omitempty"`

	// Private enables NetBird-Only access for services on this cluster. The proxy
	// then runs an embedded netbird client (a mesh peer, userspace WireGuard — no
	// extra privileges), which the cluster needs to serve private (mesh-only)
	// services. Group-based services keep working regardless.
	// +optional
	Private bool `json:"private,omitempty"`

	// Replicas of the proxy Deployment. Defaults to 1.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Image overrides the netbird reverse-proxy image. Defaults to the operator's
	// pinned image.
	// +optional
	Image string `json:"image,omitempty"`

	// ServiceAnnotations are added to the proxy's LoadBalancer Service, e.g. to
	// pin an LB-IPAM pool or request a specific IP.
	// +optional
	ServiceAnnotations map[string]string `json:"serviceAnnotations,omitempty"`
}

// ReverseProxyClusterStatus defines the observed state of ReverseProxyCluster.
type ReverseProxyClusterStatus struct {
	// ObservedGeneration is the last reconciled generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the conditions for the ReverseProxyCluster.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ClusterAddress echoes the registered cluster address once the proxy has
	// enrolled (resolvable via the Management API).
	// +optional
	ClusterAddress string `json:"clusterAddress,omitempty"`

	// TokenID is the id of the minted proxy token (revoked when the cluster is
	// deleted).
	// +optional
	TokenID string `json:"tokenID,omitempty"`

	// DomainID is the id of the registered NetBird custom domain (Domain ->
	// ClusterAddress), so service domains under it derive this cluster. Removed
	// when the cluster is deleted.
	// +optional
	DomainID string `json:"domainID,omitempty"`

	// LoadBalancerIP is the proxy Service's assigned ingress IP — what the A
	// record points at.
	// +optional
	LoadBalancerIP string `json:"loadBalancerIP,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource
// +kubebuilder:printcolumn:name="Cluster Address",type="string",JSONPath=".spec.clusterAddress",description=""
// +kubebuilder:printcolumn:name="LB IP",type="string",JSONPath=".status.loadBalancerIP",description=""
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""

// ReverseProxyCluster deploys and enrolls a NetBird bring-your-own reverse proxy
// and registers it as the account's own proxy cluster.
type ReverseProxyCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec ReverseProxyClusterSpec `json:"spec"`

	// +kubebuilder:default={"observedGeneration":-1}
	Status ReverseProxyClusterStatus `json:"status,omitempty"`
}

// GetConditions returns the status conditions of the object.
func (c *ReverseProxyCluster) GetConditions() []metav1.Condition {
	return c.Status.Conditions
}

// SetConditions sets the status conditions on the object.
func (c *ReverseProxyCluster) SetConditions(conditions []metav1.Condition) {
	c.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// ReverseProxyClusterList contains a list of ReverseProxyCluster.
type ReverseProxyClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ReverseProxyCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ReverseProxyCluster{}, &ReverseProxyClusterList{})
}
