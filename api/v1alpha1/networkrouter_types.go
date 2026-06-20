// SPDX-License-Identifier: BSD-3-Clause

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NetworkRouterSpec mirrors the NetBird router API
// (POST /api/networks/{network}/routers) and adds the routing-peer source: an
// existing NetBird group, or a netbird-client DaemonSet the operator deploys.
type NetworkRouterSpec struct {
	// NetworkRef references the Network this router belongs to. The Network must
	// be Ready; its status.networkID identifies the NetBird network.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Value is immutable"
	NetworkRef CrossNamespaceReference `json:"networkRef"`

	// Peers selects the routing peers — exactly one of group or deploy.
	Peers NetworkRouterPeers `json:"peers"`

	// Masquerade makes the routing peers SNAT traffic to the routed resources.
	// +optional
	// +kubebuilder:default=true
	Masquerade bool `json:"masquerade"`

	// Metric is the route metric; the lowest number wins.
	// +optional
	// +kubebuilder:default=9999
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=9999
	Metric int `json:"metric"`

	// Enabled controls whether the router is active.
	// +optional
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
}

// NetworkRouterPeers selects the routing peers. Exactly one field must be set.
// +kubebuilder:validation:XValidation:rule="has(self.group) != has(self.deploy)",message="exactly one of peers.group or peers.deploy must be set"
type NetworkRouterPeers struct {
	// Group reuses an existing NetBird group as the routing peers (e.g. the group
	// the host-level netbird on the cluster nodes auto-joins). The operator
	// creates only the router and deploys nothing.
	// +optional
	Group *GroupReference `json:"group,omitempty"`

	// Deploy runs a hostNetwork DaemonSet of netbird-client as the routing peers;
	// the operator manages its Group, SetupKey and DaemonSet.
	// +optional
	Deploy *RouterDeploy `json:"deploy,omitempty"`
}

// RouterDeploy configures the netbird-client DaemonSet for peers.deploy.
type RouterDeploy struct {
	// NodeSelector limits the DaemonSet to matching nodes (default: all nodes).
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Image overrides the netbird-client image (default: the operator's
	// configured client image).
	// +optional
	Image string `json:"image,omitempty"`

	// LogLevel for the netbird client.
	// +optional
	// +kubebuilder:validation:Enum=error;warn;info;debug;trace
	LogLevel string `json:"logLevel,omitempty"`
}

// NetworkRouterStatus defines the observed state of NetworkRouter.
type NetworkRouterStatus struct {
	// ObservedGeneration is the last reconciled generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the conditions for the NetworkRouter.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// NetworkID is the id of the network the router is created in.
	// +optional
	NetworkID string `json:"networkID,omitempty"`

	// RouterID is the id of the created NetBird router.
	// +optional
	RouterID string `json:"routerID,omitempty"`

	// GroupID is the id of the peer group bound to the router.
	// +optional
	GroupID string `json:"groupID,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource
// +kubebuilder:printcolumn:name="Network",type="string",JSONPath=".spec.networkRef.name",description=""
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""

// NetworkRouter is the Schema for the networkrouters API: a NetBird router (a
// peer group bound to a network) plus its routing-peer source.
type NetworkRouter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec NetworkRouterSpec `json:"spec"`

	// +kubebuilder:default={"observedGeneration":-1}
	Status NetworkRouterStatus `json:"status,omitempty"`
}

// GetConditions returns the status conditions of the object.
func (n *NetworkRouter) GetConditions() []metav1.Condition {
	return n.Status.Conditions
}

// SetConditions sets the status conditions on the object.
func (n *NetworkRouter) SetConditions(conditions []metav1.Condition) {
	n.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// NetworkRouterList contains a list of NetworkRouter.
type NetworkRouterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NetworkRouter `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NetworkRouter{}, &NetworkRouterList{})
}
