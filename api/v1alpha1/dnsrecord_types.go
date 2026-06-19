// SPDX-License-Identifier: BSD-3-Clause

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DNSRecordSpec defines the desired state of DNSRecord. It mirrors the NetBird
// DNS-record API (POST /api/dns/zones/{zone}/records) 1:1: a single record in a
// zone. The controller adopts an existing record matching name+type+content
// rather than recreating it (NetBird rejects duplicate records).
type DNSRecordSpec struct {
	// ZoneRef references the DNSZone this record is created in. The zone must be
	// Ready; its status.zoneID identifies the NetBird zone.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Value is immutable"
	ZoneRef CrossNamespaceReference `json:"zoneRef"`

	// Name is the record name (a fully qualified name under the zone).
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Type is the record type.
	// +kubebuilder:validation:Enum=A;AAAA;CNAME;TXT;MX;NS;SRV;CAA
	Type string `json:"type"`

	// Content is the record content (e.g. the IP for an A/AAAA record).
	// +kubebuilder:validation:MinLength=1
	Content string `json:"content"`

	// TTL is the record TTL in seconds. Defaults to 300.
	// +optional
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=1
	TTL int `json:"ttl"`
}

// DNSRecordStatus defines the observed state of DNSRecord.
type DNSRecordStatus struct {
	// ObservedGeneration is the last reconciled generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the conditions for the DNSRecord.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ZoneID is the id of the zone the record is created in.
	// +optional
	ZoneID string `json:"zoneID,omitempty"`

	// RecordID is the id of the created NetBird DNS record.
	// +optional
	RecordID string `json:"recordID,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type",description=""
// +kubebuilder:printcolumn:name="Content",type="string",JSONPath=".spec.content",description=""
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""

// DNSRecord is the Schema for the dnsrecords API. It is a thin mirror of a
// single NetBird DNS record.
type DNSRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec DNSRecordSpec `json:"spec"`

	// +kubebuilder:default={"observedGeneration":-1}
	Status DNSRecordStatus `json:"status,omitempty"`
}

// GetConditions returns the status conditions of the object.
func (r *DNSRecord) GetConditions() []metav1.Condition {
	return r.Status.Conditions
}

// SetConditions sets the status conditions on the object.
func (r *DNSRecord) SetConditions(conditions []metav1.Condition) {
	r.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// DNSRecordList contains a list of DNSRecord.
type DNSRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DNSRecord `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DNSRecord{}, &DNSRecordList{})
}
