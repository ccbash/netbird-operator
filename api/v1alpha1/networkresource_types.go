// SPDX-License-Identifier: BSD-3-Clause

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RoutingMode selects how a Service is exposed as a NetBird network resource.
// +kubebuilder:validation:Enum=ip;domain
type RoutingMode string

const (
	// RoutingModeIP routes the Service's ClusterIP directly: a host resource +
	// host proxy target. DNS-independent; effectively IPv4 (the primary
	// ClusterIP). This is the conservative default.
	RoutingModeIP RoutingMode = "ip"
	// RoutingModeDomain routes via the Service FQDN: a domain resource + domain
	// proxy target, resolved through NetBird DNS (the A/AAAA records). Supports
	// dualstack but depends on NetBird DNS resolution.
	RoutingModeDomain RoutingMode = "domain"
)

// NetworkResourceSpec defines the desired state of NetworkResource.
type NetworkResourceSpec struct {
	// NetworkRouterRef is a reference to the network and router where the resource will be created.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Value is immutable"
	NetworkRouterRef CrossNamespaceReference `json:"networkRouterRef"`

	// ServiceRef is a reference to the service to expose in the Network.
	ServiceRef corev1.LocalObjectReference `json:"serviceRef"`

	// Groups are references to groups that the resource will be a part of.
	// +optional
	Groups []GroupReference `json:"groups,omitempty"`

	// RoutingMode selects ip (host resource at the ClusterIP) or domain (FQDN
	// domain resource). Defaults to ip.
	// +kubebuilder:default=ip
	// +optional
	RoutingMode RoutingMode `json:"routingMode,omitempty"`
}

// NetworkResourceStatus defines the observed state of NetworkResource.
type NetworkResourceStatus struct {
	// ObservedGeneration is the last reconciled generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the conditions for the NetworkResource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// NetworkID is the id of the network the resource is created in.
	// +optional
	NetworkID string `json:"networkID,omitempty"`

	// ResourceID is the id of the created resource.
	// +optional
	ResourceID string `json:"resourceID,omitempty"`

	// StaleResourceIDs are previous NetBird resource IDs left over by a
	// routing-mode change: switching host<->domain recreates the resource under a
	// new type, but the old one cannot be deleted while a reverse-proxy service
	// still targets it. The new resource is created first (it has a different
	// address and name, so the two coexist) and the old IDs are drained here on
	// later reconciles, once the proxy has been repointed at the new resource.
	// +optional
	StaleResourceIDs []string `json:"staleResourceIDs,omitempty"`

	// DNSZoneID is the id of the zone the DNS record is created in.
	// +optional
	DNSZoneID string `json:"dnsZoneID,omitempty"`

	// DNSRecordID is the id of the legacy single A record created before
	// dualstack support. Retained only so it can be cleaned up on upgrade;
	// records are now tracked in DNSRecords.
	// +optional
	DNSRecordID string `json:"dnsRecordID,omitempty"`

	// DNSRecords are the DNS records created for the resource — one A record
	// per IPv4 ClusterIP and one AAAA per IPv6 ClusterIP.
	// +optional
	DNSRecords []DNSRecordStatus `json:"dnsRecords,omitempty"`
}

// DNSRecordStatus tracks a single DNS record managed for a NetworkResource.
type DNSRecordStatus struct {
	// Type is the record type (A or AAAA).
	Type string `json:"type"`

	// Content is the record content (the ClusterIP).
	Content string `json:"content"`

	// ID is the Netbird DNS record id.
	ID string `json:"id"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""

// NetworkResource is the Schema for the networkresources API.
type NetworkResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec NetworkResourceSpec `json:"spec"`

	// +kubebuilder:default={"observedGeneration":-1}
	Status NetworkResourceStatus `json:"status,omitempty"`
}

// GetConditions returns the status conditions of the object.
func (n *NetworkResource) GetConditions() []metav1.Condition {
	return n.Status.Conditions
}

// SetConditions sets the status conditions on the object.
func (n *NetworkResource) SetConditions(conditions []metav1.Condition) {
	n.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// NetworkResourceList contains a list of NetworkResource.
type NetworkResourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NetworkResource `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NetworkResource{}, &NetworkResourceList{})
}
