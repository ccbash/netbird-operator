// SPDX-License-Identifier: BSD-3-Clause

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NetworkSpec defines the desired state of Network. It mirrors the NetBird
// Networks API (POST /api/networks) 1:1.
type NetworkSpec struct {
	// Name of the NetBird network.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Description of the network.
	// +optional
	Description string `json:"description,omitempty"`
}

// NetworkStatus defines the observed state of Network.
type NetworkStatus struct {
	// ObservedGeneration is the last reconciled generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the conditions for the Network.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// NetworkID is the id of the created NetBird network.
	// +optional
	NetworkID string `json:"networkID,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""

// Network is the Schema for the networks API. It is a thin mirror of a NetBird
// network.
type Network struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec NetworkSpec `json:"spec"`

	// +kubebuilder:default={"observedGeneration":-1}
	Status NetworkStatus `json:"status,omitempty"`
}

// GetConditions returns the status conditions of the object.
func (n *Network) GetConditions() []metav1.Condition {
	return n.Status.Conditions
}

// SetConditions sets the status conditions on the object.
func (n *Network) SetConditions(conditions []metav1.Condition) {
	n.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// NetworkList contains a list of Network.
type NetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Network `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Network{}, &NetworkList{})
}
