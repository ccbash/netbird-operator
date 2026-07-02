# Architecture

This document describes the architecture as **settled in v0.12** and the guard
rails that keep it settled. The principle is unchanged since the v0.11 redesign:

> **NetBird CRDs mirror NetBird API objects 1:1.** The operator is a thin
> translation layer between Kubernetes and the NetBird Management API.

## Scope — what the operator is

Four capabilities, layered:

| capability | input | output | data plane |
|---|---|---|---|
| **Mirror** | a `netbird.io` CRD | the matching NetBird API object | — |
| **Reachability** | `Service type=LoadBalancer` | `NetworkResource` + dualstack `DNSRecord` per LB IP family | NetBird routing peers → LB IP |
| **Exposure** | Gateway API (`Gateway` + `HTTPRoute`) or a hand-authored `ReverseProxyService` | an in-cluster NetBird reverse proxy (`ReverseProxyCluster`) + `ReverseProxyService`s | the NetBird reverse proxy |
| **Cluster access** | `ClusterProxy` | kube-apiserver on the mesh, NetBird-identity impersonation | `netbird-kubeapi-proxy` |

And explicitly **not**: a general ingress controller, a DNS synchronizer for
externally-owned zones, a ClusterIP-CIDR router, or a policy engine (access is
always granted in NetBird, never implied by the operator).

## The model in one line

A `Service type=LoadBalancer` has a deliberately-allocated, collision-free IP —
the operator makes **that IP** mesh-routable and names it; HTTP(S)/L4 exposure
happens through a NetBird reverse proxy the operator deploys in-cluster, wired
from Gateway API manifests. **The mesh never routes ClusterIPs and the service
CIDR is never advertised** — the only place a ClusterIP appears is as an
in-cluster dial target of the in-cluster proxy.

Why not route ClusterIPs: the service CIDR (`10.96.0.0/12`) is huge, allocated
unpredictably across its whole range, identical on every default cluster (so two
clusters collide), and internal by design. An LB CIDR is small, deliberately
chosen, and collision-free. IP allocation stays with the existing LB (Cilium
LB-IPAM, MetalLB, kube-vip, a cloud LB); the operator owns only the NetBird
overlay.

## Layer 1 — NetBird-mirror CRDs

All `netbird.io/v1alpha1`. Spec ≈ the NetBird request body; status carries the
NetBird id. One generic reconciler drives them (`MirrorReconciler[T]`,
`internal/controller/mirror.go` — finalizer, conditions, requeue, id bookkeeping
shared; per-kind `apply`/`del` closures supply the typed API calls).

| Kind | NetBird endpoint | notes |
|------|------------------|-------|
| `Network` | `POST /networks` | the network |
| `NetworkRouter` | `POST /networks/{net}/routers` | the routing peers — see below |
| `NetworkResource` | `POST /networks/{net}/resources` | one address (an LB IP) |
| `DNSZone` | `POST /dns/zones` (adopt-or-create) | single-owner: admin-authored **or** owned by one controller (the LoadBalancer controller's LB zone, a ReverseProxyCluster's proxy zone) |
| `DNSRecord` | `POST /dns/zones/{zone}/records` | A/AAAA/CNAME |
| `ReverseProxyService` | `POST /reverse-proxies/services` | the exposure primitive — see below |
| `Group` / `SetupKey` | `groups` / `setup-keys` | unchanged since upstream |

Two composite (non-mirror) CRDs sit beside them:

- **`ReverseProxyCluster`** — deploys + enrolls a bring-your-own NetBird reverse
  proxy: token Secret, proxy Deployment (`:8443` single SNI/HTTP listener,
  `ndots:1`), LB Service (80/443 → 8443, PreferDualStack), DNSZone + A record +
  `*.domain` catch-all CNAME, the account cluster registration and the custom
  domain (Domain → clusterAddress) in NetBird.
- **`ReverseProxyClusterParameters`** — the proxy "flavor"
  (image/replicas/groups/private/serviceAnnotations/logLevel) a Gateway points
  at via `spec.infrastructure.parametersRef`.

### `NetworkRouter` — peers via reuse *or* DaemonSet

The router (a peer group bound to a network) is a thin mirror plus a peer-source
switch, so an operator-managed DaemonSet and a pre-existing host NetBird install
are both first-class:

```yaml
kind: NetworkRouter
spec:
  networkRef: { name: kube01 }
  masquerade: true
  metric: 9999
  peers:                       # exactly one:
    group: kube01-nodes        #  reuse — an existing NetBird group (e.g. host netbird on the nodes)
    # deploy:                  #  or let the operator run a hostNetwork DaemonSet
    #   nodeSelector: {...}
```

- **`peers.group`** → only the NetBird router is created (`PeerGroups:
  [resolved group]`); nothing is deployed. The node↔peer mapping problem
  dissolves: you point at the *group* the node peers already belong to.
- **`peers.deploy`** → the operator creates a `Group` + `SetupKey` + a
  `hostNetwork` DaemonSet (each peer shares the node datapath that reaches the
  LB IP), then the router pointing at that group.

**Placement caveat (DaemonSet mode).** With the LB Service's
`externalTrafficPolicy: Cluster` (default) any node delivers → peers can be
sparse. With `Local`, only endpoint-bearing nodes serve the IP → the DaemonSet
must co-locate with them. Default to a broad `nodeSelector` and assume
`Cluster`. Do not auto-discover Cilium's L2/BGP announcing nodes (brittle).

### `ReverseProxyService` — the one exposure primitive

Exposes an app through a NetBird reverse proxy — internal (`private: true`,
mesh-only) or external, HTTP (default), TLS-terminating, or raw TCP/UDP
(`mode`), authored by an admin or translated from an `HTTPRoute`:

```yaml
kind: ReverseProxyService
spec:
  domain: search.ccbash.de       # required, globally unique per NetBird service
  proxyCluster: gate.ccbash.de
  private: false
  backends:
    - serviceRef: { name: searxng }
      path: /                    # optional path prefix
      # port: 80                 # optional; defaults to the Service's first port
```

Backend resolution (`resolveBackend`) depends on the Service type:

- **`type=LoadBalancer`** → the proxy dials the Service's advertised dualstack
  mesh FQDN (its operator-published `DNSRecord`), over the overlay; not-yet
  advertised is a not-ready dependency, not an error.
- **any other type (ClusterIP)** → the proxy dials
  `<svc>.<ns>.svc.cluster.local` directly — it runs in-cluster, so no routable
  IP per app is needed. This is the Gateway/HTTPRoute default path.

Targets always carry `DirectUpstream: true` and reference the cluster's **CNAME
address** (never a proxy-node id). NetBird enforces **one service per domain**;
multi-port L4 under one hostname is solved by synthesizing per-port sibling
domains `<first-label>-<portName>.<parent>` (e.g. `mail-smtp.example.com`) —
the parent must be the registered custom domain.

## Layer 2a — LoadBalancer reachability

The operator watches `Service type=LoadBalancer` (which includes LB Services a
third-party Gateway controller provisions). A Service is advertised when it has
an allocated `status.loadBalancer.ingress` and the advertise decision resolves
true, most-specific wins:

1. operator default — `--advertise-loadbalancers` (default `true`);
2. namespace annotation `netbird.io/advertise: "true"|"false"`;
3. Service annotation `netbird.io/advertise: "true"|"false"`.

Per advertised Service: a **`DNSRecord`** `<svc>-<ns>.<zone>` (one A per IPv4,
one AAAA per IPv6 ingress — a single dualstack name) and a **`NetworkResource`**
per family (`/32`, `/128`). The IP-family fan-out lives **only here**
(`serviceaddr.go`); everything above deals in FQDNs. Children are labeled
`netbird.io/loadbalancer`, pruned per family, GC'd with the Service — never
hand-authored for an advertised Service.

Advertising grants **reachability, not access**: the name resolves and the IP
routes, but access stays gated by NetBird policies, and nothing is published
through a proxy until an exposure object exists.

## Layer 2b — Gateway API exposure (opt-in, `--enable-gateway-api`)

The operator is a **GatewayClass + Gateway + HTTPRoute controller**
(`controllerName: netbird.io/gateway-controller`,
`internal/controller/gateway_controller.go`). Gateway API is the *authoring
layer*; the NetBird reverse proxy is the data plane; `ReverseProxyCluster` /
`ReverseProxyService` are the translation targets.

- **GatewayClass** — created and owned by the operator (default `netbird`),
  self-healed if deleted; any class with our controllerName is Accepted.
- **Gateway → `ReverseProxyCluster`.** The proxy config derives from the
  **first TLS-terminating listener with a hostname and a certificateRef**:
  `domain` = hostname minus `*.`, `clusterAddress` = `gate.<domain>`, cert =
  `tls.certificateRefs[0]` (issued e.g. by cert-manager's gateway-shim). The
  flavor comes from the referenced `ReverseProxyClusterParameters`. Gateway
  status (`Accepted`/`Programmed`/`.addresses` = proxy LB IP, listener
  conditions) reflects the owned cluster.
- **HTTPRoute → `ReverseProxyService`s** — one child per admitted (route,
  hostname), labeled `gateway.netbird.io/httproute`, pruned when no longer
  desired, name = readable head + 8-hex hash of the raw (route, hostname) pair
  (collision-free by construction).
- **Admission**: a hostname is admitted iff it falls **under the registered
  domain** and **some listener** both hostname-matches it and permits the
  route's namespace (both checks on the same listener). Hostname-less routes
  inherit the concrete (non-wildcard) listener hostnames; wildcard-only
  Gateways fail them closed.
- **Fail closed, order-independent**: route semantics the proxy can't
  faithfully represent — Exact/Regex path matches, differing backend weights,
  zero surviving backends (all `weight: 0`), > 64 path×backend combinations —
  reject the whole route with `Accepted=False` and an accurate
  `UnsupportedValue` message, never a silently-widened or silently-dropped
  translation. ORed PathPrefixes all translate (path × backendRef fan-out);
  `weight: 0` backends receive no traffic.

Design + rationale: [`design/byop-gateway.md`](design/byop-gateway.md).

## DNS

Zones are **single-owner**. The owner is an admin (an authored/adopted
`DNSZone`) or exactly one operator controller (the LoadBalancer controller's LB
zone; a ReverseProxyCluster's proxy zone) — never two writers. The operator does
no split-horizon logic; how internal vs public names are arranged is out of
scope.

## Cluster API proxy

`ClusterProxy` is a standalone capability (independent of the exposure model):
it puts the Kubernetes API server itself on the NetBird mesh, so operators reach
`kubectl` over the tunnel with their NetBird identity instead of a public
ingress or a VPN.

The controller (`clusterproxy_controller.go`) reconciles one `ClusterProxy`
(`clusterName`, `apiServer`, `serviceAccountName`, `groups`) into:

1. a **`SetupKey`** — ephemeral, `allowExtraDnsLabels: true`, `autoGroups`
   copied from `spec.groups`;
2. a **Secret** holding the operator's NetBird management **API key**;
3. a 3-replica **Deployment** of `netbird-kubeapi-proxy` (image pinned in
   `internal/version`), hostname-spread, running as `spec.serviceAccountName`.

Each proxy pod joins the mesh as a peer and registers
**`<cluster-name>.netbird-kubeapi-proxy`** (extra DNS label). A mesh user points
their kubeconfig `server:` at that name; replicas share the label, NetBird
load-balances. The proxy reads the caller's NetBird peer identity, resolves the
peer's groups via the management API key, and **impersonates** a matching
Kubernetes user/group — the proxy holds only `impersonate` rights; effective
permissions come from whatever RBAC binds the impersonated group.

**Do not break (the CLI link depends on these):**

- **`spec.clusterName`** derives the DNS label in every client kubeconfig —
  immutable (CEL). Renaming it orphans every client.
- **`--management-url`** must point at the self-hosted NetBird, or the setup
  key is rejected (the v0.6.0 regression; fixed — the controller forwards it).
- **`allowExtraDnsLabels: true`** on the setup key — without it the DNS label
  is never registered. Immutable.
- **the management API key** in the proxy Secret — required for peer→group
  resolution; a powerful credential by design.
- The impersonation RBAC (`serviceAccountName`'s `impersonate` ClusterRole and
  the group→ClusterRole bindings) is **operator-external** — referenced, never
  created.

These surfaces are pinned by `clusterproxy_controller_test.go`.

---

## Guard rails

What keeps the architecture from regressing or flip-flopping. **A PR that
crosses one of these needs an explicit rationale in the PR description and an
update to this document in the same change** — silence is a review reject.

### Settled decisions (do not re-litigate)

Each of these was decided deliberately, some after building the alternative.
Reversing one is an architecture change, not a refactor:

1. **The mesh routes LB IPs, never ClusterIPs / the service CIDR.** The
   in-cluster proxy dialing a ClusterIP backend directly is not mesh routing
   and does not weaken this.
2. **Gateway API is the authoring layer, not the data plane.** The operator
   translates Gateway/HTTPRoute into `ReverseProxyCluster`/`ReverseProxyService`;
   the NetBird proxy serves traffic. v0.11 dropped an operator-owned Gateway
   data plane; v0.12 re-introduced Gateway API strictly as translation. Do not
   grow a data-plane component again.
3. **One exposure primitive.** `ReverseProxyService` — hand-authored or
   translated. No parallel exposure CRD, no `routeRef`/`rules` re-invention.
4. **DNS zones are single-owner; external-dns never writes a NetBird zone the
   operator manages** (records flap created-then-deleted each reconcile).
5. **One NetBird service per domain**; multi-port L4 = synthesized per-port
   sibling domains, not multiple services on one domain.
6. **Cluster targets reference the cluster CNAME address**, not a proxy-node id.
7. **Routing-peer placement is declared, not discovered** — no auto-detection
   of LB announcing nodes.
8. **Reachability ≠ access.** Advertising never creates NetBird policies.

### Development guard rails

Invariants the reconcilers must uphold — most were bugs once; the listed tests
pin them. When you add a controller, hold it to the same list.

- **Delete guards compare the recorded identity, never a mutable spec field.**
  A delete keyed on `Status.ZoneID` / `Status.ClusterAddress` / `Status.DomainID`
  must decide "shared?" on that same key (spec fields may drift via renames).
  Pinned by the `DNSZone sharing` and `ReverseProxyCluster` deletion specs in
  `redesign_test.go`.
- **Shared NetBird objects are deleted by the last CR standing.** Adoption
  (by name/domain/address) implies shared ownership; a deleting CR skips the
  NetBird delete while another live CR still uses the object.
- **Verify what you adopt; GET-verify what you recorded.** An adopted object
  must actually match the spec (e.g. a ReverseProxyDomain's `TargetCluster`);
  a recorded id is GET-verified before reuse and recreated when deleted out of
  band. Stale-but-unowned state is repaired; state owned by another live CR is
  a **conflict surfaced as not-ready (`errDependencyNotReady` → requeue), never
  fought over** — two controllers overwriting each other's NetBird object every
  reconcile is the definition of flapping.
- **Fail closed, order-independently.** Untranslatable semantics reject the
  whole object with an accurate condition message; a check that scans a list
  must scan the *whole* list (no first-match early return that hides a later
  unsupported entry). Pinned by `TestRulePaths` / `TestRouteBackends`.
- **Child names are collision-free by construction** (hash-suffixed), and
  renaming a child-name scheme is a breaking change (one-time churn on
  upgrade) — call it out in the PR. Pinned by `TestRouteChildName`.
- **Status tells the truth in failure branches.** Every not-ready path must
  reset the status fields it normally sets (`online`, `connectedProxies`, …) —
  printcolumns must never show last-good values during an outage. Never clear
  the fields `reconcileDelete` uses as delete keys.
- **Idempotent renders.** An unchanged spec must render a byte-identical
  NetBird request (sort targets, deterministic ordering) so reconciles are
  no-ops, not updates.
- **Mechanics**: new mirror CRD = a `mirror[T]` adapter + `NewXReconciler`;
  children via generated apply-configurations + `client.ForceOwnership` +
  `k8sutil.ControllerReference`; finalizers via `k8sutil.Finalizer`; status via
  `patch.NewSerialPatcher` + conditions; self-requeue ~15m. Generated files
  (`zz_generated.deepcopy.go`, `pkg/applyconfigurations/`, `config/crd/bases/`,
  `docs/api-reference.md`) are never hand-edited — change the source, run
  `make generate`. `make lint` + `make test-unit` green before merge; envtest
  loads CRDs from `charts/netbird-operator/crds`, so a stale chart after an API
  change surfaces as test failures.

### Security guard rails

- **Least-privilege RBAC, hand-maintained.** `charts/netbird-operator/templates/rbac.yaml`
  is curated (no `controller-gen rbac`); a controller touching a new resource
  updates it in the same PR — and nothing more than it needs.
- **The management API key is the crown jewel.** It reaches the operator via
  the `netbird-mgmt-api-key` Secret and is copied into the ClusterProxy proxy
  Secret by design. Never log it, never put it in a CR, never widen who can
  read those Secrets.
- **Proxy enrollment tokens are one-shot and revoked.** The plain token exists
  only in the owned Secret; `reconcileDelete` revokes it
  (`ReverseProxyTokens.Delete`). Don't persist it anywhere else.
- **TLS certs come from cert-manager Secrets; in-proxy ACME stays off**
  (`NB_PROXY_ACME_CERTIFICATES=false`). The proxy must not solve challenges or
  hold issuer credentials.
- **Exposure is explicit.** `private: false` on a reverse-proxy service means
  *internet-facing if the proxy is*. Defaults must never silently flip a
  service from mesh-only to public; the Gateway path inherits `private` from
  `ReverseProxyClusterParameters`, an admin-authored object.
- **Admission boundaries are security boundaries.** A route hostname outside
  the Gateway's registered domain, or from a namespace its listener doesn't
  allow, is rejected — cross-namespace exposure requires the Gateway owner's
  explicit `allowedRoutes.from: All`.
- **ClusterProxy impersonation stays external.** The operator never creates the
  `impersonate` ClusterRole or group bindings; granting cluster access is an
  admin act. Keep the immutables (clusterName, allowExtraDnsLabels) immutable.
- **All writes are server-side apply with `FieldOwner: "netbird-operator"`** —
  ownership conflicts surface instead of silently stealing fields.
- **The Pod webhook stays flag-gated** (`--enable-webhooks`); no NetBird API
  key configured → no controllers registered (fail inert, not open).

### Operations guard rails

- **Self-healing window is ~15 minutes.** Reconcilers self-requeue to re-check
  drift; out-of-band NetBird deletions (zones, registrations, services) repair
  within that window. Don't "fix" drift by editing NetBird objects the operator
  owns — it will win, and the interim is flapping.
- **Never point external-dns (or any DNS sync) at an operator-managed NetBird
  zone.** Two writers = records created-then-deleted every reconcile.
- **Don't hand-author `NetworkResource`/`DNSRecord` for an advertised Service**
  (label `netbird.io/loadbalancer`) or `ReverseProxyService` for a routed
  HTTPRoute (label `gateway.netbird.io/httproute`) — the owning controller
  prunes what it doesn't expect.
- **Deleting one of several CRs sharing a NetBird object is safe** — the
  registration/zone survives until the last CR standing, and a shared domain
  re-points its target when the target CR goes away. Expect the survivor to
  need up to one resync to converge.
- **Upgrades that change child-name schemes churn once**: children are created
  under the new name before the old ones are pruned; the NetBird
  one-service-per-domain rule can bounce the new child until the old one's
  finalizer clears — self-heals via backoff, budget a few error events.
- **Watch `Ready`/`Accepted` conditions and events, not just workloads**:
  `kubectl get reverseproxycluster` shows ONLINE/PROXIES (live, reset on
  outage); advertised Services carry an `Advertised` event; rejected routes say
  *why* in `Accepted=False` messages. `--metrics-bind-address` exposes
  controller-runtime reconcile metrics.
- **Proxy pods need `ndots:1`** (set by the operator): the NetBird search
  domain otherwise hijacks external FQDNs (ACME, geo DB) and breaks egress.
  Don't override the dnsConfig.
- **`externalTrafficPolicy: Local` on an advertised Service** requires routing
  peers on endpoint-bearing nodes (see NetworkRouter placement caveat).

## History (decisions already flipped once — don't flip back)

- **v0.11 → dropped** the operator-owned Gateway data plane
  (`gateway.netbird.io/Network` listener trick), per-backend ClusterIP
  `NetworkResource`s + ClusterIP DNS records, and the Gateway-owned DNSZone.
- **v0.12 → re-introduced Gateway API as a translation layer only** (BYOP
  `ReverseProxyCluster` + `HTTPRoute` → `ReverseProxyService`, proxy dials
  ClusterIPs in-cluster), added the composite `ReverseProxyCluster` /
  `ReverseProxyClusterParameters`, L4 modes with per-port sibling domains, and
  the shared-registration ownership rules under Development guard rails.

## Open details

- **Bare-L4 reachability.** A non-HTTP LoadBalancer Service is reachable by its
  `DNSRecord` + `NetworkResource` directly (no reverse proxy); L4 exposure
  *through the proxy* exists via `mode: tcp|udp` — confirm whether anything
  beyond the reachability layer is needed for proxy-less L4.
- **HTTP cluster targets.** NetBird `cluster` ServiceTargets could let the
  proxy path drop the per-service mesh FQDN hop entirely; tracked as a backlog
  item, would not change the exposure model.
