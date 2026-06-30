# Design: NetBird BYOP proxy as a first-class Gateway-API gateway

**Status:** implemented · **Branch:** `release/v0.12.x`

## Goal

Run a NetBird **bring-your-own-proxy** (BYOP) reverse proxy as a **drop-in,
internal replacement for kgateway**, driven by **Gateway API**. The operator is a
first-class **GatewayClass + Gateway controller** (`controllerName:
netbird.io/gateway-controller`):

- The operator **creates and owns its `GatewayClass`** (default name `netbird`,
  `--gateway-class-name`) whenever `--enable-gateway-api` is set, and self-heals
  it if deleted. No hand-authored class.
- Each **`Gateway`** of that class **is one proxy instance**: it points
  `spec.infrastructure.parametersRef` at a namespaced
  **`ReverseProxyClusterParameters`** (in the Gateway's namespace) for the
  "flavor" (image/replicas/groups/private/serviceAnnotations); the operator
  derives `domain`/`clusterAddress`/cert from its listeners and **creates an
  owned `ReverseProxyCluster`** (the data plane: proxy Deployment + LB Service +
  DNS + NetBird custom domain). The Gateway's `status` reflects that cluster.
- Each **`HTTPRoute`** on the Gateway **auto-creates a `ReverseProxyService`**
  (operator-owned). End-user behaviour matches kgateway; only the automation is new.

Everything here is **internal**: `*.ccbash.io` / `*.ccbash.cloud` are
NetBird-resolved domains reachable over the mesh, not the public internet.

## Topology

```
NetBird peer
 → NetBird DNS: catch-all *.<domain> → gate.<domain> (A) → proxy LB IP
 → over the mesh to the proxy LoadBalancer IP (Cilium LB-IPAM, advertised by the LB controller)
 → proxy terminates TLS with the cert-manager *.<domain> wildcard (mounted static)
 → proxy dials the backend ClusterIP in-cluster (direct_upstream) — as kgateway
```

- The proxy gets a `Service type=LoadBalancer`; the operator's LoadBalancer
  controller advertises it (its LB IP becomes mesh-routable). No public edge.
- The proxy is **centralised** (not embedded) — see Decisions; it dials k8s
  ClusterIP backends directly over the pod network via `direct_upstream` targets.

## Decisions

| Topic | Decision |
|-------|----------|
| Shape | **Gateway-first-class.** The operator owns its `GatewayClass`; each Gateway points `spec.infrastructure.parametersRef` at a namespaced `ReverseProxyClusterParameters` and creates an owned `ReverseProxyCluster`. The direct chart-rendered RPC option was removed (Gateway-only). |
| Addressing | **Listener + convention.** `domain` = listener hostname minus `*.`; `clusterAddress` = `gate.<domain>`; cert = listener `tls.certificateRefs[0]`. Single TLS listener per Gateway (v1); extras get an "Unused" condition. |
| Exposure | Internal `Service type=LoadBalancer` (Cilium LB IP), reached over the mesh. No public edge, no ACME. Proxy listens on non-privileged `:8443`; the LB Service maps public 80/443 → 8443 (single SNI listener detects TLS vs HTTP). |
| Backend reach | **Direct to ClusterIP** (drop-in). `ReverseProxyService` targets are `TargetType=cluster` + `direct_upstream=true` + `Host=<svc>.<ns>.svc.cluster.local`. Honored by management v0.73.2 on a centralised cluster — **DirectUpstream does NOT require a private/embedded cluster** (the dashboard only *shows* the toggle for private clusters). |
| private vs centralised | **Centralised (`private: false`).** `--private` makes the proxy an inbound mesh peer (embedded netbird client) and flips the cluster's dashboard `private` flag (forcing every service NetBird-Only). In-cluster the mesh fails (ICE gathering timeouts, ghost `proxy-*` peers) and it's unnecessary, since all backends are `direct_upstream`. |
| TLS | cert-manager wildcard via **DNS-01** (works without public reachability), mounted static (`/certs`). No proxy ACME. |
| Enrollment | **Token-only**: `NB_PROXY_TOKEN`, minted via `ReverseProxyTokens.Create`. Cluster address = `NB_PROXY_DOMAIN`. |
| DNS | NetBird-managed via `DNSZone`/`DNSRecord`: A `<clusterAddress>` → LB IP, catch-all `*.<domain>` → `<clusterAddress>`. Plus a **`ReverseProxyDomains` custom domain** (`<domain>` → cluster) so `*.<domain>` service hostnames derive the cluster. |
| Pod DNS | **`ndots:1`** on the proxy pod: the node publishes a search domain (e.g. `ccbash.de`) that kubelet appends to ClusterFirst pods; under ndots:5 external FQDNs (geo DB `pkgs.netbird.io`, ACME) get it glued on and resolve wrong (`tls: internal error`). ndots:1 queries them as-is; `svc.cluster.local` still resolves. |
| LoadBalancer zone | LB-advertised Services attach to an **explicit** `Network` (admin-authored, paired with its `NetworkRouter`). The `DNSZone` is **operator-owned**: the LoadBalancer controller creates it from `--loadbalancer-dns-zone` (apex domain) + `--loadbalancer-dns-zone-groups` (distribution groups) and self-heals it, so it never needs hand-authoring and is never the proxy's `ccbash.io` zone. |

## Components

### 1. `ReverseProxyCluster` — deploy + enroll (data plane)
Bespoke reconciler (owns workloads). Per reconcile: mint a token → owned Secret;
ensure a `DNSZone`; `Deployment` of `netbirdio/reverse-proxy` (`:8443`,
`NB_PROXY_PRIVATE`, `/certs` mount, health `/healthz/*` on `:8080`, `ndots:1`);
`Service type=LoadBalancer` (80/443 → 8443); wait for the LB IP → A + catch-all
`DNSRecord`s; once enrolled (`GetProxyClusterByAddress`) register the
`ReverseProxyDomains` custom domain and validate it; status `clusterAddress` /
`loadBalancerIP` / `tokenID` / `domainID`. Delete: revoke token, drop the account
cluster + custom domain; children GC by owner refs. It's usable **directly** (an
admin RPC) but the chart/supported path is via a Gateway.

### 2. ClusterIP backend on `ReverseProxyService`
`backends[]` resolve a `serviceRef`: a LoadBalancer Service → its advertised mesh
FQDN; any other (ClusterIP) → `<svc>.<ns>.svc.cluster.local`. Targets are
`TargetType=cluster`, `TargetId=<cluster address>`, `direct_upstream=true`.
`AccessGroups` are only sent for `private:true` services (the NetBird-Only ACL).

### 3. Gateway-API controllers
- **`GatewayClassReconciler`** — owns the operator's GatewayClass: ensures the
  managed class (`controllerName == netbird.io/gateway-controller`) exists at
  startup (a leader-elected `manager.Runnable`), recreates it if deleted, and
  marks any class of our controllerName `Accepted`.
- **`GatewayReconciler`** — for a Gateway of an accepted class: derive the proxy
  config from the first TLS listener (hostname → `domain`, `gate.<domain>` →
  `clusterAddress`, `certificateRefs[0]` → cert), merge the params from the
  Gateway's `spec.infrastructure.parametersRef`, and
  **server-side-apply an owned `ReverseProxyCluster`**. Reflect the cluster into
  Gateway `status`: `Accepted`, `Programmed` (tracks the RPC's `Ready`),
  `.addresses` (the proxy LB IP), per-listener conditions (Accepted /
  ResolvedRefs / Programmed) + `attachedRoutes`. Owner-ref GC + the RPC finalizer
  clean up NetBird on Gateway delete.
- **`HTTPRouteReconciler`** — for routes whose `parentRefs` resolve to a BYOP
  Gateway and whose hostname matches a listener (+ `allowedRoutes` namespace
  policy): per `(hostname × rule)` emit an owner-ref'd `ReverseProxyService`
  (`domain`=hostname, `proxyCluster`=`gate.<domain>`, backends from
  `backendRefs`/PathPrefix); prune on change; set `RouteParentStatus`.

**Watches:** the Gateway controller watches `Gateway` (For), `Owns`
`ReverseProxyCluster`, and watches `GatewayClass` + `ReverseProxyClusterParameters`;
the HTTPRoute controller watches `HTTPRoute`, `Owns` `ReverseProxyService`, and
watches `Gateway` — all with a periodic resync, so upstream changes re-reconcile.

## Resolved questions

- **Image:** `internal/version.ReverseProxyImage` (`netbirdio/reverse-proxy`).
- **Cluster derivation:** handled by registering `<domain>` as a NetBird custom
  domain (`ReverseProxyDomains`) targeting the cluster.
- **Cardinality:** **one `ReverseProxyCluster` (and LB address) per Gateway.**
- **DirectUpstream / private:** DirectUpstream works on a centralised cluster
  (mgmt ≥ v0.73.1); private mode is unnecessary and harmful in-cluster.
- **Browser resolution gotcha (client-side):** Chrome's built-in DNS client
  won't use the `systemd-resolved` `127.0.0.53` stub, so NetBird-internal names
  fail to resolve in the browser while `curl`/`getent` work. Fix is client-side
  (`BuiltInDnsClientEnabled:false`, or no `systemd-resolved` on the host).

## Remaining / non-goals (v1)

- Multi-listener / multi-hostname per Gateway; full HTTPRoute filter support
  (redirect/URLRewrite/header-method match, weighted backends) — surface as a
  Route condition rather than silently drop.
- Cross-namespace backend `ReferenceGrant`; non-HTTPS listener protocols.
- `gate.` cluster-address prefix is a hardcoded convention.

## Testing

envtest (CRDs from `charts/netbird-operator/crds`, `make generate` first; Gateway
API CRDs vendored into `test/gateway-api-crds`) + `internal/netbirdmock` for the
NetBird REST API. The Gateway-API spec asserts: a Gateway creates an owned
`ReverseProxyCluster` with the derived spec, the Gateway reports `Accepted`, and
an attached `HTTPRoute` becomes a `ReverseProxyService` targeting `gate.<domain>`
with the route Accepted.
