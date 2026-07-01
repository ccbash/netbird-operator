// SPDX-License-Identifier: BSD-3-Clause

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReverseProxyClusterParametersSpec is the per-Gateway "flavor": the parts of a
// ReverseProxyCluster that aren't derived from a Gateway's own listeners. A
// Gateway derives its domain, clusterAddress and cert from its listeners and
// references these (in its own namespace) via spec.infrastructure.parametersRef
// to fill in the rest.
type ReverseProxyClusterParametersSpec struct {
	// Image overrides the netbird reverse-proxy image. Defaults to the operator's
	// pinned image.
	// +optional
	Image string `json:"image,omitempty"`

	// Replicas of the proxy Deployment. Defaults to 1.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// LogLevel sets the proxy's log level (and its embedded netbird client's),
	// e.g. "error" to silence the embedded client's unused P2P/ICE warnings on a
	// centralised cluster. Empty keeps the image default.
	// +optional
	// +kubebuilder:validation:Enum=error;warn;info;debug;trace
	LogLevel string `json:"logLevel,omitempty"`

	// Groups are NetBird groups the proxy's advertised LoadBalancer resource
	// joins, so access policies can target it.
	// +optional
	Groups []GroupReference `json:"groups,omitempty"`

	// Private enables NetBird-Only services: the proxy runs an embedded netbird
	// client (userspace WireGuard). Group-based services work regardless.
	// +optional
	Private bool `json:"private,omitempty"`

	// ServiceAnnotations are added to each Gateway's proxy LoadBalancer Service,
	// e.g. to pin an LB-IPAM pool.
	// +optional
	ServiceAnnotations map[string]string `json:"serviceAnnotations,omitempty"`
}

// +kubebuilder:object:root=true

// ReverseProxyClusterParameters is the implementation config a Gateway of the
// netbird.io/gateway-controller class points at via
// spec.infrastructure.parametersRef. It is namespaced and must live in the
// referencing Gateway's namespace.
type ReverseProxyClusterParameters struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec ReverseProxyClusterParametersSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// ReverseProxyClusterParametersList contains a list of ReverseProxyClusterParameters.
type ReverseProxyClusterParametersList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ReverseProxyClusterParameters `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ReverseProxyClusterParameters{}, &ReverseProxyClusterParametersList{})
}
