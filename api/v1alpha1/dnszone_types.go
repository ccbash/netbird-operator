// SPDX-License-Identifier: BSD-3-Clause

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DNSZoneSpec defines the desired state of DNSZone. It mirrors the NetBird
// DNS-zones API (POST /api/dns/zones) 1:1. The controller adopts an existing
// zone with the same domain rather than failing, so a zone provisioned out of
// band is taken over.
type DNSZoneSpec struct {
	// Name of the managed zone.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Domain is the zone apex, e.g. "kube.example.com".
	// +kubebuilder:validation:MinLength=1
	Domain string `json:"domain"`

	// DistributionGroups are the NetBird groups whose peers receive the zone, so
	// they can resolve records in it. The reverse-proxy cluster that fronts a
	// service must be in one of these groups for hostname upstreams to resolve.
	// +optional
	DistributionGroups []GroupReference `json:"distributionGroups,omitempty"`

	// EnableSearchDomain adds the zone as a search domain on distributed peers.
	// +optional
	EnableSearchDomain *bool `json:"enableSearchDomain,omitempty"`

	// Enabled controls whether the zone is active. Defaults to true.
	// +optional
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
}

// DNSZoneStatus defines the observed state of DNSZone.
type DNSZoneStatus struct {
	// ObservedGeneration is the last reconciled generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the conditions for the DNSZone.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ZoneID is the id of the managed NetBird zone.
	// +optional
	ZoneID string `json:"zoneID,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource
// +kubebuilder:printcolumn:name="Domain",type="string",JSONPath=".spec.domain",description=""
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""

// DNSZone is the Schema for the dnszones API. It is a thin mirror of a NetBird
// managed DNS zone.
type DNSZone struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec DNSZoneSpec `json:"spec"`

	// +kubebuilder:default={"observedGeneration":-1}
	Status DNSZoneStatus `json:"status,omitempty"`
}

// GetConditions returns the status conditions of the object.
func (z *DNSZone) GetConditions() []metav1.Condition {
	return z.Status.Conditions
}

// SetConditions sets the status conditions on the object.
func (z *DNSZone) SetConditions(conditions []metav1.Condition) {
	z.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// DNSZoneList contains a list of DNSZone.
type DNSZoneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DNSZone `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DNSZone{}, &DNSZoneList{})
}
