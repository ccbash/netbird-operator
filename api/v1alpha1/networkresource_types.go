// SPDX-License-Identifier: BSD-3-Clause

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NetworkResourceSpec defines the desired state of NetworkResource. It mirrors
// the NetBird network-resource API (POST /api/networks/{network}/resources) 1:1:
// a single address routed into a network, with groups. DNS is handled
// separately by DNSRecord; IP-family fan-out is done by the translation layer
// (one NetworkResource per address family).
type NetworkResourceSpec struct {
	// NetworkRef references the Network this resource is created in. The Network
	// must be Ready; its status.networkID identifies the NetBird network.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Value is immutable"
	NetworkRef CrossNamespaceReference `json:"networkRef"`

	// Name of the resource.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Address is the single resource address — an IP, CIDR, or domain. NetBird
	// derives the resource type from it.
	// +kubebuilder:validation:MinLength=1
	Address string `json:"address"`

	// Description of the resource.
	// +optional
	Description string `json:"description,omitempty"`

	// Groups are the NetBird groups this resource is a part of, referenced by
	// name, id, or local Group reference.
	// +optional
	Groups []GroupReference `json:"groups,omitempty"`

	// Enabled controls whether the resource is active. Defaults to true.
	// +optional
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
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

	// ResourceID is the id of the created NetBird resource.
	// +optional
	ResourceID string `json:"resourceID,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource
// +kubebuilder:printcolumn:name="Address",type="string",JSONPath=".spec.address",description=""
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""

// NetworkResource is the Schema for the networkresources API. It is a thin
// mirror of a NetBird network resource (one address).
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
