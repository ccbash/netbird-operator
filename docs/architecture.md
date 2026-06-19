# Architecture

This document describes the **target architecture** the operator is being
redesigned toward (`release/v0.11.x`). It supersedes the v0.10.x model where a
single overloaded `NetworkResource` did resource creation, DNS publishing and
IP-family fan-out at once, and where HTTP exposure was tangled with subnet
routing.

The guiding principle:

> **NetBird CRDs mirror NetBird API objects 1:1. The Gateway API is the
> translation layer.** Kubernetes-native intent (`Gateway`, `HTTPRoute`,
> `TCPRoute`) is translated into thin NetBird-mirror CRDs, and a generic
> controller applies those straight to the NetBird Management API.

## Two layers

```
 Gateway API (intent)            NetBird-mirror CRDs (Layer 1)      NetBird API
 ───────────────────             ─────────────────────────         ───────────
 Gateway  ─────────────────────▶ Network                       ──▶ /networks
                          └─────▶ DNSZone                       ──▶ /dns/zones
                          └─────▶ (router pods + router)        ──▶ /networks/{}/routers
 HTTPRoute ────────────────────▶ NetworkResource (per backend) ──▶ /networks/{}/resources
                          └─────▶ DNSRecord     (per backend)   ──▶ /dns/zones/{}/records
 TCPRoute  ────────────────────▶ NetworkResource (per backend) ──▶ /networks/{}/resources
                          └─────▶ DNSRecord     (per backend)   ──▶ /dns/zones/{}/records

 ReverseProxyService (admin-authored, references a route)       ──▶ /reverse-proxies/services
```

- **Layer 1 — NetBird-mirror CRDs.** Each is a thin 1:1 mirror of one NetBird
  API object. Spec ≈ the request body; status carries the returned NetBird ID.
  No business logic beyond *upsert to the API, store the ID, delete on
  finalizer*.
- **Layer 2 — translation controllers.** The Gateway API kinds own no NetBird
  API calls of their own — they create/own Layer-1 CRDs. All "what does a
  TCPRoute *mean* in NetBird" logic lives here, in one obvious place.

## Layer 1 — NetBird-mirror CRDs

All `netbird.io/v1alpha1`. Spec mirrors the NetBird request body; `status`
holds the NetBird object ID(s).

| Kind | NetBird endpoint | Spec (≈ request) |
|------|------------------|------------------|
| `Network` | `POST /networks` | `name, description?` |
| `NetworkResource` | `POST /networks/{network}/resources` | `networkRef, name, address, groups, enabled` — **one address** |
| `DNSZone` | `POST /dns/zones` (adopt-or-create) | `name, domain, distributionGroups, enableSearchDomain?, enabled?` |
| `DNSRecord` | `POST /dns/zones/{zone}/records` | `zoneRef, name, type, content, ttl` |
| `ReverseProxyService` | `POST /reverse-proxies/services` | admin-authored — see below |
| `Group` | `POST /groups` | `name, peers` *(existing)* |
| `SetupKey` | `POST /setup-keys` | `name, autoGroups, …` *(existing)* |

Notes:

- **`NetworkResource` is now purely a NetBird resource** — one address, with
  groups and enabled. DNS moved out to `DNSRecord`; IP-family fan-out moves up
  to the translation layer (one `NetworkResource` per address family).
- **`DNSZone` owns `distributionGroups`.** This is what makes a zone resolvable
  by the reverse-proxy cluster — previously a manual dashboard step and a
  recurring "the proxy can't resolve the FQDN" failure mode. The operator now
  ensures the proxy cluster's group is distributed.
- **`DNSRecord.zoneRef`** points at a `DNSZone` CRD and reads its
  `status.zoneID` (no per-reconcile name→ID lookup).

### Generic mirror controller

Every mirror CRD implements one small interface:

```go
type NBObject interface {
    apply(ctx, *netbird.Client) (id string, err error)  // the one typed API call
    delete(ctx, *netbird.Client, id string) error
}
```

A single generic `MirrorReconciler` drives all of them (registered per type):
finalizer, status/conditions, requeue and ID bookkeeping are shared; the only
per-kind code is those two methods (the typed client call). This is the "one
controller for all" — *one reconcile implementation, N registrations*. Go's
typed client is why each kind keeps a ~5-line shim rather than full reflection.

## Layer 2 — translation controllers

### Gateway

- Links to a `Network` (which NetBird network this Gateway fronts).
- Deploys the **router-peer pods** (the netbird-client workload) and assigns
  them to the network (the NetBird `router`). The router is **not** a mirror
  CRD — it is owned by the Gateway because it is inseparable from the pods.
- From the Gateway's **hostname / wildcard domain**, creates a `DNSZone`.

`Network` replaces today's `NetworkRouter` CRD as the pure network mirror; the
orchestration (pods, router, zone) moves onto the Gateway.

### HTTPRoute / TCPRoute

Both translate a route + its `backendRefs` into **reachability**:

- one `NetworkResource` per backend Service address family (makes the ClusterIP
  routable in the network via the router pods), and
- one `DNSRecord` per backend in the Gateway's `DNSZone`
  (`<svc>-<ns>.<zone>` → ClusterIP, A/AAAA).

Routes do **not** create a `ReverseProxyService`. Whether a Service is exposed
through the public reverse proxy is an **administrator decision**, not an
automatic consequence of routing.

### ReverseProxyService (admin-authored)

Hand-written, per service, like the old `NBServicePolicy` — but it *is* the
NetBird reverse-proxy service, referencing a route to pick up its backends:

```yaml
apiVersion: netbird.io/v1alpha1
kind: ReverseProxyService
spec:
  routeRef: { kind: HTTPRoute, name: app }   # picks up backends + hostname
  proxyCluster: gate.example.com             # reverse-proxy cluster (resolved to ID)
  upstream: hostname                         # hostname (FQDN, default) | ip (ClusterIP)
  # optional exposure tuning:
  private?, accessGroups?, accessRestrictions?, passHostHeader?, rewriteRedirects?, crowdsecMode?
status: { serviceID }
```

Its controller reads `routeRef` → backend Services → their `DNSRecord` FQDNs (or
ClusterIPs for `upstream: ip`) and builds `cluster` targets
(`targetId` = proxy-cluster ID, `Host` = FQDN, `Options.DirectUpstream = true`,
which NetBird requires for cluster targets), then applies the service. A
`TCPRoute` is exposed through the proxy the same way (tcp/tls mode); without a
`ReverseProxyService` a `TCPRoute` is pure mesh (resource + DNS only).

## How a request flows (HTTP)

1. `NetworkResource` makes the backend ClusterIP routable via the router pods.
2. `DNSRecord` resolves `<svc>-<ns>.<zone>` → ClusterIP (A/AAAA).
3. `ReverseProxyService` cluster target dials that FQDN. With `DirectUpstream`,
   the proxy uses its host stack — which has the NetBird route (through the
   router pods) and the distributed `DNSZone` — so IPv4/IPv6 is transparent.

Every object has exactly one job; nothing is duplicated.

## Removed from the v0.10.x model

- **`NBServicePolicy`** → replaced by `ReverseProxyService` (which references the
  route directly instead of GEP-713 `targetRefs`).
- **`NetworkRouter.serviceCIDRs` / subnet routing** → removed. Per-backend
  `NetworkResource` is the only reachability mechanism (this is the CIDR-vs-host
  duplication that was flagged).
- **Overloaded `NetworkResource`** → split into `NetworkResource` (one address) +
  `DNSRecord`; IP-family fan-out moves to the translation layer.
- **Routing modes (`ip` / `domain`, `RoutingMode`)** → gone; replaced by
  `ReverseProxyService.upstream` (`hostname` / `ip`).

## Open points

- **`DNSZone.distributionGroups` wiring.** The Gateway distributes its zone to
  the built-in `All` group, so any reverse-proxy cluster fronting a route can
  resolve the per-service records (and so can mesh peers). This is broad but
  matches a single-owner internal zone; a tighter design — the
  `ReverseProxyService` adding only its proxy cluster's group to the zone — is a
  future refinement.
- **Migration.** Pre-1.0 — clean break, no automated migration (done by hand).
- **`SidecarProfile` / `ClusterProxy`** are out of scope for this redesign and
  unchanged.
