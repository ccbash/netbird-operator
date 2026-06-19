// SPDX-License-Identifier: BSD-3-Clause

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NetworkResourceSpec defines the desired state of NetworkResource.
type NetworkResourceSpec struct {
	// NetworkRouterRef is a reference to the network and router where the resource will be created.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Value is immutable"
	NetworkRouterRef CrossNamespaceReference `json:"networkRouterRef"`

	// ServiceRef is a reference to the service to expose in the Network.
	// Immutable: re-pointing at a different Service would change the resource's
	// address in place — create a new NetworkResource instead.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Value is immutable"
	ServiceRef corev1.LocalObjectReference `json:"serviceRef"`

	// Groups are references to groups that the resource will be a part of.
	// +optional
	Groups []GroupReference `json:"groups,omitempty"`

	// IPFamilies selects which of the Service's ClusterIP families to expose.
	// Each selected family gets its own NetBird host resource at that ClusterIP,
	// so a dualstack Service is reachable over both. Defaults to all of the
	// Service's ClusterIP families.
	// +optional
	// +kubebuilder:validation:items:Enum=IPv4;IPv6
	IPFamilies []corev1.IPFamily `json:"ipFamilies,omitempty"`
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

	// NetworkID is the id of the network the resources are created in.
	// +optional
	NetworkID string `json:"networkID,omitempty"`

	// Resources are the NetBird host resources created for the Service, one per
	// exposed IP family.
	// +optional
	Resources []NetworkResourceEntry `json:"resources,omitempty"`

	// DNSZoneID is the id of the zone the DNS records are created in.
	// +optional
	DNSZoneID string `json:"dnsZoneID,omitempty"`

	// DNSRecords are the DNS records created for the resource — one A record
	// per IPv4 ClusterIP and one AAAA per IPv6 ClusterIP.
	// +optional
	DNSRecords []DNSRecordStatus `json:"dnsRecords,omitempty"`
}

// NetworkResourceEntry is a NetBird host resource created for one of a Service's
// ClusterIP families.
type NetworkResourceEntry struct {
	// Family is the IP family ("IPv4" or "IPv6").
	Family string `json:"family"`

	// Address is the ClusterIP the resource points at.
	Address string `json:"address"`

	// ResourceID is the NetBird resource id.
	ResourceID string `json:"resourceID"`
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
