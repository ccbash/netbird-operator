// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"testing"

	"github.com/go-openapi/testify/v2/require"
	corev1 "k8s.io/api/core/v1"
)

func TestChildName(t *testing.T) {
	t.Parallel()
	// Route children are suffixed with the IP family so the IPv4 and IPv6
	// resources/records for a Service coexist (names are unique within a kind).
	require.Equal(t, "app-ipv4", childName("app", corev1.IPv4Protocol))
	require.Equal(t, "app-ipv6", childName("app", corev1.IPv6Protocol))
}

func TestIPFamilyOf(t *testing.T) {
	t.Parallel()
	require.Equal(t, corev1.IPv4Protocol, ipFamilyOf("10.0.0.1"))
	require.Equal(t, corev1.IPv6Protocol, ipFamilyOf("2001:db8::1"))
	require.Equal(t, corev1.IPFamily(""), ipFamilyOf("not-an-ip"))
}

func TestFamilyAddresses(t *testing.T) {
	t.Parallel()
	dual := &corev1.Service{Spec: corev1.ServiceSpec{ClusterIP: "10.0.0.1", ClusterIPs: []string{"10.0.0.1", "2001:db8::1"}}}

	// No filter -> all of the Service's families, in order.
	all := familyAddresses(dual, nil)
	require.Equal(t, []string{"10.0.0.1", "2001:db8::1"}, addressList(all))

	// Filter to IPv6 only.
	v6 := familyAddresses(dual, []corev1.IPFamily{corev1.IPv6Protocol})
	require.Len(t, v6, 1)
	require.Equal(t, corev1.IPv6Protocol, v6[0].family)
	require.Equal(t, "2001:db8::1", v6[0].address)

	// A requested family the Service doesn't have -> nothing.
	v4only := &corev1.Service{Spec: corev1.ServiceSpec{ClusterIP: "10.0.0.1", ClusterIPs: []string{"10.0.0.1"}}}
	require.Empty(t, familyAddresses(v4only, []corev1.IPFamily{corev1.IPv6Protocol}))
}
