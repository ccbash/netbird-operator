// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestSafeName(t *testing.T) {
	short := "app-route"
	if got := safeName(short, 63); got != short {
		t.Fatalf("short name changed: %q", got)
	}

	long := strings.Repeat("a", 300)
	got := safeName(long, objectNameMaxLen)
	if len(got) > objectNameMaxLen {
		t.Fatalf("len(%d) exceeds cap %d", len(got), objectNameMaxLen)
	}
	if last := got[len(got)-1]; last == '-' || last == '.' || last == '_' {
		t.Fatalf("name ends on invalid char: %q", got)
	}

	// Distinct inputs that share a long prefix must not collide.
	a := safeName(strings.Repeat("x", 70)+"-one", labelValueMaxLen)
	b := safeName(strings.Repeat("x", 70)+"-two", labelValueMaxLen)
	if a == b {
		t.Fatalf("distinct inputs collided: %q", a)
	}
	if len(a) > labelValueMaxLen {
		t.Fatalf("label value len(%d) exceeds %d", len(a), labelValueMaxLen)
	}
}

func TestRulePath(t *testing.T) {
	prefix := gwv1.PathMatchPathPrefix
	exact := gwv1.PathMatchExact
	rule := func(typ *gwv1.PathMatchType, val string) gwv1.HTTPRouteRule {
		if typ == nil {
			return gwv1.HTTPRouteRule{}
		}
		return gwv1.HTTPRouteRule{Matches: []gwv1.HTTPRouteMatch{{Path: &gwv1.HTTPPathMatch{Type: typ, Value: &val}}}}
	}

	cases := []struct {
		name     string
		rule     gwv1.HTTPRouteRule
		wantPath string
		wantOK   bool
	}{
		{"no match defaults to root", rule(nil, ""), "/", true},
		{"prefix passes through", rule(&prefix, "/admin"), "/admin", true},
		{"exact is unsupported", rule(&exact, "/admin"), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, ok := rulePath(tc.rule)
			if ok != tc.wantOK || path != tc.wantPath {
				t.Fatalf("got (%q,%v), want (%q,%v)", path, ok, tc.wantPath, tc.wantOK)
			}
		})
	}
}

func TestRouteBackendsFailsClosedOnUnsupportedMatch(t *testing.T) {
	exact := gwv1.PathMatchExact
	route := &gwv1.HTTPRoute{Spec: gwv1.HTTPRouteSpec{Rules: []gwv1.HTTPRouteRule{{
		Matches:     []gwv1.HTTPRouteMatch{{Path: &gwv1.HTTPPathMatch{Type: &exact, Value: ptrTo("/admin")}}},
		BackendRefs: []gwv1.HTTPBackendRef{{BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{Name: "app"}}}},
	}}}}
	if _, ok := routeBackends(route); ok {
		t.Fatal("expected routeBackends to fail closed on an Exact match")
	}
}

func TestGatewayAdmitsRouteOnlyProxyListener(t *testing.T) {
	gw := &gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: gwv1.GatewaySpec{Listeners: []gwv1.Listener{
			{Name: "foo", Hostname: ptrTo(gwv1.Hostname("*.foo.com"))},
			{Name: "bar", Hostname: ptrTo(gwv1.Hostname("*.bar.com"))},
		}},
	}
	route := &gwv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns"}}

	// A bar.com host must NOT be admitted when the proxy listener is foo (it would
	// otherwise be registered against the foo cluster).
	if gatewayAdmitsRoute(gw, route, "app.bar.com", "foo") {
		t.Fatal("admitted a route under a non-proxy listener's domain")
	}
	if !gatewayAdmitsRoute(gw, route, "app.foo.com", "foo") {
		t.Fatal("did not admit a route matching the proxy listener")
	}
}
