# Architecture

This document describes the **target architecture**. The principle is unchanged
from the v0.11.x redesign:

> **NetBird CRDs mirror NetBird API objects 1:1.** The operator is a thin
> translation layer between Kubernetes and the NetBird Management API.

What changed since v0.11.x is the *translation*: the operator no longer routes
**ClusterIPs**, owns a **Gateway**, or creates per-backend resources. It works
off **`Service type=LoadBalancer`** addresses and is driven by three CRDs —
`Network`, `NetworkRouter`, `ReverseProxyService`.

> **Implementation status.** The Layer-1 mirror CRDs and the generic reconciler
> (`internal/controller/mirror.go`) are implemented. The Layer-2 translation
> below (LoadBalancer-IP model, no Gateway) **supersedes** the v0.11.x
> Gateway/ClusterIP translation and is the next thing to build.

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
  rules:
    - path: /
      backend: { kind: Service, name: searxng }   # -> the Service's DNSRecord FQDN
  # or: routeRef: { kind: HTTPRoute, name: searxng }   # lift paths + backends from a kgateway route
```

Per rule the operator builds a proxy target: **`Host` = the backend Service's
DNSRecord FQDN** (resolves, via the zone, to the LB IP routed through the
`NetworkRouter` peers — dualstack-transparent), **`Path` = the rule path**,
`Options.DirectUpstream: true`. The proxy never sees an IP or an address family.

## Layer 2 — translation

The operator watches **`Service type=LoadBalancer`** (which includes
Gateway-provisioned LB Services; a Gateway's address is also in
`Gateway.status.addresses`). For each in-scope LB Service:

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

## Dropped relative to v0.11.x

- **The operator's own `Gateway` / `GatewayClass`** and the
  `gateway.netbird.io/Network` listener trick. The operator consumes *existing*
  LoadBalancer Services (Gateway-provisioned or bare); the opt-in is authoring a
  `ReverseProxyService` (and `Network` + `NetworkRouter`).
- **ClusterIP exposure** — per-backend ClusterIP `NetworkResource`s and the
  `<svc>-<ns>` records pointing at ClusterIPs. Replaced by LB-IP records.
- **The Gateway-owned DNSZone** — DNS is admin-managed.

Unchanged: the mirror CRDs, the generic reconciler, the dualstack/adopt-or-create
helpers, `Group`/`SetupKey`, `ClusterProxy`, the Pod sidecar webhook.

## Open details

- **Opt-in / scoping.** Which LoadBalancer Services get advertised, and into
  which `Network`/zone — a Service annotation/label selecting a `Network`, or
  implied by a `ReverseProxyService` reference. To finalize.
- **Bare-L4 path.** A `TCPRoute` or a non-HTTP LoadBalancer Service is reachable
  by its `DNSRecord` + `NetworkResource` directly (no reverse proxy); confirm
  whether that needs any CRD beyond the reachability layer.
