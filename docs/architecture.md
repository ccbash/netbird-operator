# Architecture

This document describes the **target architecture**. The principle is unchanged
from the v0.11.x redesign:

> **NetBird CRDs mirror NetBird API objects 1:1.** The operator is a thin
> translation layer between Kubernetes and the NetBird Management API.

What changed since v0.11.x is the *translation*: the operator no longer routes
**ClusterIPs**, owns a **Gateway**, or creates per-backend resources. It works
off **`Service type=LoadBalancer`** addresses and is driven by three CRDs —
`Network`, `NetworkRouter`, `ReverseProxyService`.

> **Implementation status.** Both layers are implemented: the Layer-1 mirror CRDs
> and generic reconciler (`internal/controller/mirror.go`) and the Layer-2
> LoadBalancer-IP translation (`internal/controller/loadbalancer_controller.go`,
> `reverseproxyservice_mirror.go`), which **supersedes** the v0.11.x
> Gateway/ClusterIP translation.

## The model in one line

A Kubernetes `Service type=LoadBalancer` — bare, or provisioned by a Gateway-API
controller like kgateway (a Gateway *is* a LoadBalancer Service; its address is
in `Gateway.status.addresses`) — has an LB IP. The operator makes that LB IP
reachable over a NetBird network, gives it a dualstack DNS name, and optionally
exposes it through the NetBird reverse proxy. **ClusterIPs are never routed; the
service CIDR is never advertised.**

Why not ClusterIP: the service CIDR (`10.96.0.0/12`) is huge, allocated
unpredictably across its whole range, identical on every default cluster (so two
clusters collide), and is internal by design. An LB CIDR is small, deliberately
chosen, and collision-free — the right thing to make routable. IP allocation is
left to the existing LB (Cilium LB-IPAM, MetalLB, kgateway, a cloud LB); the
operator owns only the NetBird overlay.

## Layers

| layer | object(s) | one per |
|-------|-----------|---------|
| overlay | `Network` + `NetworkRouter` | network |
| reachability | `NetworkResource` (LB IP, per family) + `DNSRecord` (FQDN, A+AAAA) | LoadBalancer Service |
| exposure | `ReverseProxyService` (FQDN + paths, internal **or** external) | exposed app |

## Layer 1 — NetBird-mirror CRDs

All `netbird.io/v1alpha1`. Spec ≈ the NetBird request body; status carries the
NetBird id. One generic reconciler drives them (`MirrorReconciler[T]` — finalizer,
conditions, requeue, id bookkeeping shared; per-kind `apply`/`delete` closures
supply the one typed API call).

| Kind | NetBird endpoint | notes |
|------|------------------|-------|
| `Network` | `POST /networks` | the network |
| `NetworkRouter` | `POST /networks/{net}/routers` | **the routing peers — see below** |
| `NetworkResource` | `POST /networks/{net}/resources` | one address (an LB IP) |
| `DNSZone` | `POST /dns/zones` (adopt-or-create) | admin-authored |
| `DNSRecord` | `POST /dns/zones/{zone}/records` | A/AAAA/CNAME |
| `ReverseProxyService` | `POST /reverse-proxies/services` | **the exposure layer — see below** |
| `Group` / `SetupKey` | `groups` / `setup-keys` | unchanged |

### `NetworkRouter` — peers via reuse *or* DaemonSet

The router (a peer group bound to a network) is a thin mirror, plus a peer-source
switch so an operator-managed DaemonSet and a pre-existing host NetBird install
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
    #   image: ...             #  (operator defaults)
    #   logLevel: info
```

- **`peers.group`** → the operator creates only the NetBird router
  (`PeerGroups: [resolved group]`) and deploys nothing. The node↔peer mapping
  problem dissolves: you point at the *group* the node peers already belong to
  (their setup key's auto-group), never at individual nodes.
- **`peers.deploy`** → the operator creates a `Group` + `SetupKey` + a
  `hostNetwork` DaemonSet (so each peer shares the node datapath that reaches the
  LB IP), then the router pointing at that group.

**Routing-peer placement caveat (DaemonSet mode).** A routing peer can only serve
an LB IP it can deliver to a backend. With the LB Service's
`externalTrafficPolicy: Cluster` (default) any node works → peers can be sparse.
With `Local`, only nodes running a backend endpoint serve the IP → the DaemonSet
must co-locate with those endpoints. Default to a broad `nodeSelector` and assume
`Cluster`. Do not try to auto-discover Cilium's L2/BGP announcing nodes (brittle).

### `ReverseProxyService` — the one exposure primitive

Admin-authored. Exposes an app **internally or externally** through the NetBird
reverse proxy, referencing the **DNSRecord that belongs to a Service** (the
dualstack FQDN → LB IP) as the upstream, with path/host awareness:

```yaml
kind: ReverseProxyService
spec:
  domain: search.ccbash.de
  proxyCluster: gate.ccbash.de
  private: false                 # external; true = internal mesh-only (same CRD)
  passHostHeader: true           # advanced toggles, optional
  rewriteRedirects: false
  backends:
    - serviceRef: { name: searxng }   # a type=LoadBalancer Service
      path: /                         # optional path prefix
      # port: 80                      # optional; defaults to the Service's first port
```

Per backend the operator builds a proxy target: **`Host` = the backend Service's
DNSRecord FQDN** (resolves, via the zone, to the LB IP routed through the
`NetworkRouter` peers — dualstack-transparent), **`Port`** = the backend port,
**`Path`** = the backend path, `TargetId` = the cluster's **CNAME address** (not
a proxy-node id), `Options.DirectUpstream: true`. The proxy never sees a raw IP
or an address family.

## Layer 2 — translation

The operator watches **`Service type=LoadBalancer`** (which includes
Gateway-provisioned LB Services; a Gateway's address is also in
`Gateway.status.addresses`).

**Scoping — default-on, namespace opt-out.** A Service is advertised when it has
an allocated `status.loadBalancer.ingress` and the advertise decision resolves
to true, most-specific wins:

1. operator default — `advertiseLoadBalancers: true` (flip to `false` for a
   default-off / namespace-opt-in posture);
2. namespace annotation `netbird.io/advertise: "true"|"false"`;
3. Service annotation `netbird.io/advertise: "true"|"false"`.

Namespace-level is the primary lever because Gateway-provisioned LB Services are
generated and awkward to annotate individually. The target `Network` is the
cluster's single `Network` by default, or a namespace annotation
`netbird.io/network: <name>` for multi-network setups. Advertising is *automatic*
(no opt-in CRD); it makes the name resolvable and the IP routable, but grants no
access — that stays gated by NetBird policies, and nothing is published through
the proxy until a `ReverseProxyService` is authored.

For each advertised LB Service:

- **`DNSRecord`** `<svc>-<ns>.<zone>` with one **A** per IPv4 and one **AAAA**
  per IPv6 `status.loadBalancer.ingress` address — a single **dualstack name**,
  so whoever resolves it gets whichever family they speak.
- **`NetworkResource`** per LB-IP family (`/32`, `/128`) so both are routable.

The IP-family fan-out lives **only here** (reusing `familyAddresses` /
`dnsRecordTypeFor` from `serviceaddr.go`, now over LB ingress IPs instead of
ClusterIPs). Nothing above this layer — `ReverseProxyService`, the exposure model
— ever deals with families or raw IPs; it deals with the FQDN.

`ReverseProxyService` then translates into a NetBird reverse-proxy service whose
cluster targets point at those FQDNs with the route's paths.

## DNS

Zones are **admin-managed** (a `DNSZone` mirror, authored or adopted by name).
The operator only writes A/AAAA/CNAME records into the zone it is pointed at. How
internal vs public names are arranged (split-horizon, internal-only domains) is
out of scope — the operator does no horizon logic.

## Cluster API proxy

`ClusterProxy` is a standalone capability (independent of the LB-IP exposure
model): it puts the Kubernetes API server itself on the NetBird mesh, so
operators reach `kubectl` over the tunnel with their NetBird identity instead of
a public ingress or a VPN.

The controller (`clusterproxy_controller.go`) reconciles one `ClusterProxy`
(`clusterName`, `apiServer`, `serviceAccountName`, `groups`) into:

1. a **`SetupKey`** — ephemeral, `allowExtraDnsLabels: true`, `autoGroups` copied
   from `spec.groups`;
2. a **Secret** holding the operator's NetBird management **API key**;
3. a 3-replica **Deployment** of `netbird-kubeapi-proxy` (image pinned in
   `internal/version`), hostname-spread, running as `spec.serviceAccountName`.

**How a client connects (the netbird-cli link).** Each proxy pod joins the mesh
as a NetBird peer (`--setup-key` + `--management-url`) and — because the setup
key allows extra DNS labels — registers **`<cluster-name>.netbird-kubeapi-proxy`**.
A user on the same mesh (via the netbird CLI/daemon) points their kubeconfig
`server:` at that mesh-only name; the replicas share the label, so NetBird load-
balances across them. The proxy reads the caller's NetBird peer identity, uses
the management **API key** to resolve that peer's NetBird groups, and
**impersonates** a matching Kubernetes user/group — so the proxy itself holds
only `impersonate` rights and the *effective* permissions come from whatever
RBAC binds the impersonated group (e.g. a NetBird group `kubernetes-admin`
mapped to `cluster-admin`).

**Do not break (these are what the CLI link depends on):**

- **`spec.clusterName`** derives the DNS label in every client kubeconfig — it is
  immutable (CEL) for this reason. Renaming it orphans every client.
- **`--management-url`** must point at the self-hosted NetBird, or the setup key
  is rejected as invalid (this was the v0.6.0 regression that forced downstream
  to hand-roll the proxy; fixed — the controller forwards it).
- **`allowExtraDnsLabels: true`** on the setup key — without it the DNS label is
  never registered. Immutable.
- **the management API key** in the proxy Secret — required for peer→group
  resolution; it is a powerful credential by design.
- The impersonation RBAC (`serviceAccountName`'s `impersonate` ClusterRole, and
  the group→ClusterRole bindings) is **operator-external** — the controller
  references the ServiceAccount but does not create it or the RBAC.

These surfaces are pinned by `clusterproxy_controller_test.go`.

## Dropped relative to v0.11.x

- **The operator's own `Gateway` / `GatewayClass`** and the
  `gateway.netbird.io/Network` listener trick. The operator consumes *existing*
  LoadBalancer Services (Gateway-provisioned or bare); the opt-in is authoring a
  `ReverseProxyService` (and `Network` + `NetworkRouter`).
  - *Re-introduced as a first-class Gateway controller in v0.12 (opt-in,
    `--enable-gateway-api`):* the operator is a **GatewayClass + Gateway
    controller** (`controllerName: netbird.io/byop-proxy`). A `GatewayClass`
    points its `parametersRef` at a cluster-scoped **`ReverseProxyClusterParameters`**
    (the class "flavor": image/replicas/groups/private/serviceAnnotations). Each
    **`Gateway`** of that class becomes one NetBird bring-your-own reverse proxy:
    the operator derives `domain` (listener hostname minus `*.`), `clusterAddress`
    (`gate.<domain>`) and the cert (listener `tls.certificateRefs`) from the
    Gateway's listeners, and **creates an owned `ReverseProxyCluster`** (proxy
    Deployment + LB Service + DNS + custom domain). The Gateway's `status`
    (`Accepted`, `Programmed`, `.addresses` = the proxy LB IP, per-listener
    conditions) reflects that cluster. **`HTTPRoute`s** attached to the Gateway
    become **`ReverseProxyService`s** targeting it. The proxy — not the operator —
    is the data plane. See [`design/byop-gateway.md`](design/byop-gateway.md).
- **ClusterIP exposure** — per-backend ClusterIP `NetworkResource`s and the
  `<svc>-<ns>` records pointing at ClusterIPs. Replaced by LB-IP records.
- **The Gateway-owned DNSZone** — DNS is admin-managed.

Unchanged: the mirror CRDs, the generic reconciler, the dualstack/adopt-or-create
helpers, `Group`/`SetupKey`, `ClusterProxy`, the Pod sidecar webhook.

## Implemented vs this document

This document is the design; the build matches it, with these concrete shapes:
`ReverseProxyService.spec.backends[]` (`serviceRef` / `port` / `path`) — there is
no `routeRef`/`rules`; the cluster `TargetId` is the cluster's CNAME **address**;
the `--advertise-loadbalancers` flag (chart value `advertiseLoadBalancers`) sets
the operator default. The status helpers GET-verify a recorded NetBird id and
recreate when it was deleted out of band.

## Open details

- **Bare-L4 reachability.** A non-HTTP LoadBalancer Service is reachable by its
  `DNSRecord` + `NetworkResource` directly (no reverse proxy); confirm whether
  L4 exposure through the proxy needs anything beyond the reachability layer.
