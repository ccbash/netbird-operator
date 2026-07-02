// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"slices"
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

func TestRulePaths(t *testing.T) {
	prefix := gwv1.PathMatchPathPrefix
	exact := gwv1.PathMatchExact
	pathMatch := func(typ *gwv1.PathMatchType, val string) gwv1.HTTPRouteMatch {
		return gwv1.HTTPRouteMatch{Path: &gwv1.HTTPPathMatch{Type: typ, Value: &val}}
	}

	cases := []struct {
		name      string
		matches   []gwv1.HTTPRouteMatch
		wantPaths []string
		wantOK    bool
	}{
		{"no matches default to root", nil, []string{"/"}, true},
		{"prefix passes through", []gwv1.HTTPRouteMatch{pathMatch(&prefix, "/admin")}, []string{"/admin"}, true},
		{"exact is unsupported", []gwv1.HTTPRouteMatch{pathMatch(&exact, "/admin")}, nil, false},
		// The fail-close must be order-independent: an unsupported match after a
		// valid prefix must still reject (previously it was silently dropped).
		{"prefix then exact still fails", []gwv1.HTTPRouteMatch{pathMatch(&prefix, "/a"), pathMatch(&exact, "/b")}, nil, false},
		{"exact then prefix fails", []gwv1.HTTPRouteMatch{pathMatch(&exact, "/b"), pathMatch(&prefix, "/a")}, nil, false},
		// ORed prefixes are all collected (previously only the first survived).
		{"ORed prefixes collected", []gwv1.HTTPRouteMatch{pathMatch(&prefix, "/a"), pathMatch(&prefix, "/b")}, []string{"/a", "/b"}, true},
		{"duplicate prefixes deduped", []gwv1.HTTPRouteMatch{pathMatch(&prefix, "/a"), pathMatch(&prefix, "/a")}, []string{"/a"}, true},
		{"header-only match contributes root", []gwv1.HTTPRouteMatch{{}}, []string{"/"}, true},
		{"nil type is treated as prefix", []gwv1.HTTPRouteMatch{pathMatch(nil, "/a")}, []string{"/a"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths, ok := rulePaths(gwv1.HTTPRouteRule{Matches: tc.matches})
			if ok != tc.wantOK || !slices.Equal(paths, tc.wantPaths) {
				t.Fatalf("got (%v,%v), want (%v,%v)", paths, ok, tc.wantPaths, tc.wantOK)
			}
		})
	}
}

func TestRouteBackends(t *testing.T) {
	prefix := gwv1.PathMatchPathPrefix
	exact := gwv1.PathMatchExact
	backendRef := func(name string, weight *int32) gwv1.HTTPBackendRef {
		return gwv1.HTTPBackendRef{BackendRef: gwv1.BackendRef{
			BackendObjectReference: gwv1.BackendObjectReference{Name: gwv1.ObjectName(name)},
			Weight:                 weight,
		}}
	}
	route := func(rules ...gwv1.HTTPRouteRule) *gwv1.HTTPRoute {
		return &gwv1.HTTPRoute{Spec: gwv1.HTTPRouteSpec{Rules: rules}}
	}

	t.Run("fails closed on an Exact match regardless of order", func(t *testing.T) {
		r := route(gwv1.HTTPRouteRule{
			Matches: []gwv1.HTTPRouteMatch{
				{Path: &gwv1.HTTPPathMatch{Type: &prefix, Value: ptrTo("/a")}},
				{Path: &gwv1.HTTPPathMatch{Type: &exact, Value: ptrTo("/b")}},
			},
			BackendRefs: []gwv1.HTTPBackendRef{backendRef("app", nil)},
		})
		if _, reason, ok := routeBackends(r); ok || !strings.Contains(reason, "PathPrefix") {
			t.Fatalf("expected PathPrefix fail-close, got ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("fans out paths x backendRefs", func(t *testing.T) {
		r := route(gwv1.HTTPRouteRule{
			Matches: []gwv1.HTTPRouteMatch{
				{Path: &gwv1.HTTPPathMatch{Type: &prefix, Value: ptrTo("/a")}},
				{Path: &gwv1.HTTPPathMatch{Type: &prefix, Value: ptrTo("/b")}},
			},
			BackendRefs: []gwv1.HTTPBackendRef{backendRef("one", nil), backendRef("two", nil)},
		})
		backends, reason, ok := routeBackends(r)
		if !ok {
			t.Fatalf("unexpected fail-close: %s", reason)
		}
		if len(backends) != 4 {
			t.Fatalf("want 4 backends (2 paths x 2 refs), got %d", len(backends))
		}
	})

	t.Run("equal weights ok, nil counts as one", func(t *testing.T) {
		r := route(gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{backendRef("one", nil), backendRef("two", ptrTo(int32(1)))}})
		if backends, reason, ok := routeBackends(r); !ok || len(backends) != 2 {
			t.Fatalf("want 2 backends, got %d (ok=%v reason=%q)", len(backends), ok, reason)
		}
	})

	t.Run("weight zero excludes the backend", func(t *testing.T) {
		r := route(gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{backendRef("live", ptrTo(int32(1))), backendRef("drained", ptrTo(int32(0)))}})
		backends, reason, ok := routeBackends(r)
		if !ok || len(backends) != 1 {
			t.Fatalf("want the drained backend skipped, got %d (ok=%v reason=%q)", len(backends), ok, reason)
		}
	})

	t.Run("differing weights fail closed", func(t *testing.T) {
		r := route(gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{backendRef("blue", ptrTo(int32(3))), backendRef("green", ptrTo(int32(1)))}})
		if _, reason, ok := routeBackends(r); ok || !strings.Contains(reason, "weight") {
			t.Fatalf("expected weights fail-close, got ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("all weight zero fails closed", func(t *testing.T) {
		r := route(gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{backendRef("a", ptrTo(int32(0)))}})
		if _, reason, ok := routeBackends(r); ok || !strings.Contains(reason, "routes no traffic") {
			t.Fatalf("expected no-traffic fail-close, got ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("a backend-less rule among healthy ones fails closed", func(t *testing.T) {
		// Its paths would otherwise silently fall through to the other rule's
		// backends — e.g. a redirect-only /admin rule served by the / backend.
		r := route(
			gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{backendRef("app", nil)}},
			gwv1.HTTPRouteRule{Matches: []gwv1.HTTPRouteMatch{{Path: &gwv1.HTTPPathMatch{Type: &prefix, Value: ptrTo("/admin")}}}},
		)
		if _, reason, ok := routeBackends(r); ok || !strings.Contains(reason, "routes no traffic") {
			t.Fatalf("expected per-rule fail-close, got ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("rule filters fail closed", func(t *testing.T) {
		r := route(gwv1.HTTPRouteRule{
			Filters:     []gwv1.HTTPRouteFilter{{Type: gwv1.HTTPRouteFilterRequestRedirect}},
			BackendRefs: []gwv1.HTTPBackendRef{backendRef("app", nil)},
		})
		if _, reason, ok := routeBackends(r); ok || !strings.Contains(reason, "filters") {
			t.Fatalf("expected filter fail-close, got ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("backendRef filters fail closed", func(t *testing.T) {
		br := backendRef("app", nil)
		br.Filters = []gwv1.HTTPRouteFilter{{Type: gwv1.HTTPRouteFilterURLRewrite}}
		r := route(gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{br}})
		if _, reason, ok := routeBackends(r); ok || !strings.Contains(reason, "filters") {
			t.Fatalf("expected filter fail-close, got ok=%v reason=%q", ok, reason)
		}
	})
}

func TestGatewayAdmitsRoute(t *testing.T) {
	// The common cert-manager shape: an apex listener first and a wildcard
	// second, both over the proxy's registered domain, plus an unrelated one.
	gw := &gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: gwv1.GatewaySpec{Listeners: []gwv1.Listener{
			{Name: "apex", Hostname: ptrTo(gwv1.Hostname("foo.com"))},
			{Name: "wildcard", Hostname: ptrTo(gwv1.Hostname("*.foo.com"))},
			{Name: "other", Hostname: ptrTo(gwv1.Hostname("*.bar.com"))},
		}},
	}
	cfg := gatewayProxy{domain: "foo.com"} // admission reads only the domain, not the derived listener
	route := &gwv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns"}}

	// Admission must consider every listener, not just the proxy listener the
	// domain was derived from — app.foo.com only matches the wildcard listener.
	if !gatewayAdmitsRoute(gw, route, "app.foo.com", cfg) {
		t.Fatal("did not admit a wildcard-listener hostname under the registered domain")
	}
	if !gatewayAdmitsRoute(gw, route, "foo.com", cfg) {
		t.Fatal("did not admit the apex hostname")
	}
	// A hostname outside the registered domain must NOT be admitted even though
	// the *.bar.com listener matches it (it would be registered against the
	// foo.com cluster).
	if gatewayAdmitsRoute(gw, route, "app.bar.com", cfg) {
		t.Fatal("admitted a hostname outside the registered domain")
	}
	// Both checks must hold on the same listener: a foreign-namespace route is
	// rejected when the only hostname-matching listener defaults to Same.
	foreign := &gwv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "elsewhere"}}
	if gatewayAdmitsRoute(gw, foreign, "app.foo.com", cfg) {
		t.Fatal("admitted a foreign-namespace route past the default Same policy")
	}

	// A hostname-less catch-all listener (a common :80 redirect listener) with a
	// permissive allowedRoutes must not leak admission onto the proxy: it matches
	// every hostname, but it is not under the registered domain.
	fromAll := gwv1.NamespacesFromAll
	gw.Spec.Listeners = append(gw.Spec.Listeners, gwv1.Listener{
		Name:          "http",
		AllowedRoutes: &gwv1.AllowedRoutes{Namespaces: &gwv1.RouteNamespaces{From: &fromAll}},
	})
	if gatewayAdmitsRoute(gw, foreign, "app.foo.com", cfg) {
		t.Fatal("a hostname-less From=All listener bypassed the proxy listeners' namespace scoping")
	}
	// The same-namespace route stays admitted via the under-domain listeners.
	if !gatewayAdmitsRoute(gw, route, "app.foo.com", cfg) {
		t.Fatal("under-domain admission broke when a catch-all listener is present")
	}
}

func TestAdmittedHostnames(t *testing.T) {
	fromAll := gwv1.NamespacesFromAll
	allowAll := &gwv1.AllowedRoutes{Namespaces: &gwv1.RouteNamespaces{From: &fromAll}}
	gw := func(listeners ...gwv1.Listener) *gwv1.Gateway {
		return &gwv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
			Spec:       gwv1.GatewaySpec{Listeners: listeners},
		}
	}
	cfg := gatewayProxy{domain: "foo.com"}
	route := &gwv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns"}}

	// Declared hostnames are filtered through admission.
	withHosts := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns"},
		Spec:       gwv1.HTTPRouteSpec{Hostnames: []gwv1.Hostname{"app.foo.com", "app.bar.com"}},
	}
	got := admittedHostnames(gw(gwv1.Listener{Name: "w", Hostname: ptrTo(gwv1.Hostname("*.foo.com"))}), withHosts, cfg)
	if !slices.Equal(got, []string{"app.foo.com"}) {
		t.Fatalf("declared hostnames: got %v", got)
	}

	// A hostname-less route inherits concrete listener hostnames (Gateway API
	// hostname inheritance) — but never synthesized wildcards.
	got = admittedHostnames(gw(
		gwv1.Listener{Name: "apex", Hostname: ptrTo(gwv1.Hostname("foo.com"))},
		gwv1.Listener{Name: "wild", Hostname: ptrTo(gwv1.Hostname("*.foo.com"))},
	), route, cfg)
	if !slices.Equal(got, []string{"foo.com"}) {
		t.Fatalf("mixed listeners: got %v, want the apex only", got)
	}
	got = admittedHostnames(gw(gwv1.Listener{Name: "wild", Hostname: ptrTo(gwv1.Hostname("*.foo.com"))}), route, cfg)
	if len(got) != 0 {
		t.Fatalf("wildcard-only listeners must yield nothing, got %v", got)
	}

	// The concrete listener must still permit the route's namespace.
	foreign := &gwv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "elsewhere"}}
	apex := gwv1.Listener{Name: "apex", Hostname: ptrTo(gwv1.Hostname("foo.com"))}
	if got := admittedHostnames(gw(apex), foreign, cfg); len(got) != 0 {
		t.Fatalf("foreign namespace inherited a hostname past Same: %v", got)
	}
	apex.AllowedRoutes = allowAll
	if got := admittedHostnames(gw(apex), foreign, cfg); !slices.Equal(got, []string{"foo.com"}) {
		t.Fatalf("From=All did not admit the foreign namespace: %v", got)
	}
}

func TestListenerAttachedRoutes(t *testing.T) {
	gw := &gwv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"}}
	apex := gwv1.Listener{Name: "apex", Hostname: ptrTo(gwv1.Hostname("foo.com"))}
	wild := gwv1.Listener{Name: "wild", Hostname: ptrTo(gwv1.Hostname("*.foo.com"))}
	route := func(ns string, hostnames ...gwv1.Hostname) gwv1.HTTPRoute {
		return gwv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: ns},
			Spec: gwv1.HTTPRouteSpec{
				CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Name: "gw"}}},
				Hostnames:       hostnames,
			},
		}
	}
	routes := []gwv1.HTTPRoute{
		route("ns", "app.foo.com"),      // wildcard listener only
		route("ns", "foo.com"),          // apex listener only
		route("ns"),                     // hostname-less: matches every listener
		route("elsewhere", "x.foo.com"), // denied by default Same policy
	}
	if n := listenerAttachedRoutes(gw, wild, routes); n != 2 {
		t.Fatalf("wildcard listener: want 2 attached, got %d", n)
	}
	if n := listenerAttachedRoutes(gw, apex, routes); n != 2 {
		t.Fatalf("apex listener: want 2 attached, got %d", n)
	}
}

func TestRouteChildName(t *testing.T) {
	// The readable head is ambiguous ('.' and '-' merge; the join point is
	// unmarked) — the hash suffix must keep these distinct.
	collisions := [][2][2]string{
		{{"a", "b-c.d"}, {"a-b", "c.d"}},
		{{"r", "x.a.b.com"}, {"r", "x-a.b.com"}},
	}
	for _, pair := range collisions {
		a := routeChildName(pair[0][0], pair[0][1])
		b := routeChildName(pair[1][0], pair[1][1])
		if a == b {
			t.Fatalf("%v and %v collided: %q", pair[0], pair[1], a)
		}
	}

	name := routeChildName("route", "app.example.com")
	if name != routeChildName("route", "app.example.com") {
		t.Fatal("routeChildName is not deterministic")
	}
	long := routeChildName(strings.Repeat("r", 250), strings.Repeat("h", 100)+".example.com")
	if len(long) > objectNameMaxLen {
		t.Fatalf("len(%d) exceeds %d", len(long), objectNameMaxLen)
	}
	if last := long[len(long)-1]; (last < 'a' || last > 'z') && (last < '0' || last > '9') {
		t.Fatalf("name ends on invalid char: %q", long)
	}
}
