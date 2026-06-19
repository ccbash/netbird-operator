// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-openapi/testify/v2/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/netbirdio/netbird/shared/management/http/api"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
)

func TestResolveProxyClusterAndUpstream(t *testing.T) {
	t.Parallel()

	// Defaults: no cluster, hostname upstream.
	require.Equal(t, "", resolveProxyCluster(nil))
	require.Equal(t, nbv1alpha1.UpstreamModeHostname, resolveUpstream(nil))

	// Policies are newest-first; the oldest (last) non-empty value wins.
	policies := []nbv1alpha1.NBServicePolicy{
		{Spec: nbv1alpha1.NBServicePolicySpec{ProxyCluster: "newer.example", Upstream: nbv1alpha1.UpstreamModeIP}},
		{Spec: nbv1alpha1.NBServicePolicySpec{ProxyCluster: "gate.ccbash.de", Upstream: nbv1alpha1.UpstreamModeHostname}},
	}
	require.Equal(t, "gate.ccbash.de", resolveProxyCluster(policies))
	require.Equal(t, nbv1alpha1.UpstreamModeHostname, resolveUpstream(policies))

	// An empty field doesn't override an earlier non-empty value.
	require.Equal(t, nbv1alpha1.UpstreamModeIP, resolveUpstream([]nbv1alpha1.NBServicePolicy{
		{Spec: nbv1alpha1.NBServicePolicySpec{Upstream: nbv1alpha1.UpstreamModeIP}},
		{},
	}))
}

func TestBuildClusterTargets(t *testing.T) {
	t.Parallel()

	prefix := gwv1.PathMatchPathPrefix
	pushPath := "/push/"
	port80 := gwv1.PortNumber(80)

	backend := func(name string, port *gwv1.PortNumber) gwv1.HTTPBackendRef {
		return gwv1.HTTPBackendRef{BackendRef: gwv1.BackendRef{
			BackendObjectReference: gwv1.BackendObjectReference{Name: gwv1.ObjectName(name), Port: port},
		}}
	}
	hr := &gwv1.HTTPRoute{Spec: gwv1.HTTPRouteSpec{Rules: []gwv1.HTTPRouteRule{
		{
			Matches:     []gwv1.HTTPRouteMatch{{Path: &gwv1.HTTPPathMatch{Type: &prefix, Value: &pushPath}}},
			BackendRefs: []gwv1.HTTPBackendRef{backend("notify", &port80)},
		},
		{
			// no path match -> catch-all; no port -> fall back to Service port
			BackendRefs: []gwv1.HTTPBackendRef{backend("app", nil)},
		},
	}}}
	svcIdx := map[string]corev1.Service{
		"notify": {ObjectMeta: metav1.ObjectMeta{Name: "notify"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}},
		"app":    {ObjectMeta: metav1.ObjectMeta{Name: "app"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}}},
	}
	hostByService := map[string]string{"notify": "notify-ns.zone", "app": "app-ns.zone"}

	targets := buildClusterTargets(logr.Discard(), hr, svcIdx, hostByService, "cluster-1")
	require.Len(t, targets, 2)

	byHost := map[string]api.ServiceTarget{}
	for _, tgt := range targets {
		byHost[derefStr(tgt.Host)] = tgt
		// Every target is a cluster target pointing at the proxy cluster.
		require.Equal(t, api.ServiceTargetTargetTypeCluster, tgt.TargetType)
		require.Equal(t, "cluster-1", tgt.TargetId)
	}

	// Path prefix + port carried onto the matching backend's target.
	require.Equal(t, "/push/", derefStr(byHost["notify-ns.zone"].Path))
	require.Equal(t, 80, byHost["notify-ns.zone"].Port)

	// Missing backendRef port falls back to the Service's first port; catch-all
	// rule has no path.
	require.Nil(t, byHost["app-ns.zone"].Path)
	require.Equal(t, 8080, byHost["app-ns.zone"].Port)
}

func TestResourceAddressFor(t *testing.T) {
	t.Parallel()

	svc := &corev1.Service{Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.10"}}
	fqdn := "app-default.cluster.local"

	// ip mode (and empty default) -> host resource at the ClusterIP.
	addr, rType := resourceAddressFor(svc, fqdn, nbv1alpha1.RoutingModeIP)
	require.Equal(t, "10.96.0.10", addr)
	require.Equal(t, api.NetworkResourceTypeHost, rType)

	addr, rType = resourceAddressFor(svc, fqdn, "")
	require.Equal(t, "10.96.0.10", addr)
	require.Equal(t, api.NetworkResourceTypeHost, rType)

	// domain mode -> domain resource at the FQDN.
	addr, rType = resourceAddressFor(svc, fqdn, nbv1alpha1.RoutingModeDomain)
	require.Equal(t, fqdn, addr)
	require.Equal(t, api.NetworkResourceTypeDomain, rType)
}

func TestDNSRecordTypeFor(t *testing.T) {
	t.Parallel()

	rType, ok := dnsRecordTypeFor("10.96.0.10")
	require.True(t, ok)
	require.Equal(t, api.DNSRecordTypeA, rType)

	rType, ok = dnsRecordTypeFor("2001:db8::1")
	require.True(t, ok)
	require.Equal(t, api.DNSRecordTypeAAAA, rType)

	// IPv4-mapped IPv6 is still an A record (To4 != nil).
	rType, ok = dnsRecordTypeFor("::ffff:10.0.0.1")
	require.True(t, ok)
	require.Equal(t, api.DNSRecordTypeA, rType)

	_, ok = dnsRecordTypeFor("not-an-ip")
	require.False(t, ok)
}

func TestClusterIPsOf(t *testing.T) {
	t.Parallel()

	// Dualstack: both ClusterIPs are returned.
	dual := &corev1.Service{Spec: corev1.ServiceSpec{
		ClusterIP:  "10.96.0.10",
		ClusterIPs: []string{"10.96.0.10", "2001:db8::1"},
	}}
	require.Equal(t, []string{"10.96.0.10", "2001:db8::1"}, clusterIPsOf(dual))

	// Legacy object with only the singular field falls back to it.
	single := &corev1.Service{Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.10"}}
	require.Equal(t, []string{"10.96.0.10"}, clusterIPsOf(single))
}

func TestCIDRResourceSuffix(t *testing.T) {
	t.Parallel()

	require.Equal(t, "10-96-0-0-12", cidrResourceSuffix("10.96.0.0/12"))
	require.Equal(t, "fd00--12-64", cidrResourceSuffix("fd00::12/64"))
}
