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
