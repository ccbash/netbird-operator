# Design: NetBird BYOP proxy as an internal Gateway-API gateway

**Status:** proposed · **Branch:** `release/v0.12.x`

## Goal

Let the operator run a NetBird **bring-your-own-proxy** (BYOP) reverse proxy as a
**drop-in, internal replacement for kgateway**, and translate **Gateway API**
objects onto the CRDs we already have. Concretely:

- The operator deploys + enrolls the `netbirdio/reverse-proxy` and registers it
  as the account's own NetBird proxy cluster (`ClusterType=account`).
- A `GatewayClass` whose `controllerName` points at this operator makes the
  operator pick up that class's `Gateway`s and their `HTTPRoute`s.
- Each `HTTPRoute` **auto-creates a `ReverseProxyService`** (operator-owned).
  End-user behaviour is unchanged vs kgateway; only this automation is new.

Everything here is **internal**: `*.ccbash.cloud` is a NetBird-resolved domain
reachable over the mesh, not the public internet. kgateway is already internal,
so "internal / no external exposure" is the status quo, not a new constraint.

## Topology (resolved)

```
NetBird peer
 → NetBird DNS: *.ccbash.cloud (catch-all) → proxy LB IP
 → over the mesh to the proxy LoadBalancer IP (Cilium LB-IPAM)
 → proxy terminates TLS with the cert-manager *.ccbash.cloud wildcard (static)
 → proxy dials the backend ClusterIP in-cluster (e.g. bulwark:80) — as kgateway
```

- The proxy gets a **`Service type=LoadBalancer`**; Cilium LB-IPAM assigns the IP
  (reachability). The operator's existing LoadBalancer controller advertises that
  Service (NetworkResource → the LB IP is mesh-routable; default-on).
- **No Service/exposure on the public internet.** The proxy is reached over the
  mesh via its LB IP.
- The proxy is itself a NetBird peer (embeds the client) and dials backends; for
  in-cluster ClusterIP backends its embedded client only intercepts NetBird-zone
  DNS, so other names fall through to kube-dns → the proxy reaches the ClusterIP
  directly, exactly like kgateway.

## Decisions

| Topic | Decision |
|-------|----------|
| Exposure | Internal. `Service type=LoadBalancer` (Cilium LB IP), reached over the mesh. No public edge, no ACME `tls-alpn-01`. |
| Backend reach | **Direct to ClusterIP** (drop-in). Requires a new ClusterIP/`serviceRef` backend target on `ReverseProxyService`. |
| TLS | cert-manager wildcard `*.ccbash.cloud` via **DNS-01** (works without public reachability), mounted **static** to the proxy (`-cert-dir`, auto-reload on renewal). No proxy ACME. |
| Enrollment | **Token-only**: `NB_PROXY_TOKEN` (Bearer), minted via `ReverseProxyTokens.Create`. Cluster address = `NB_PROXY_DOMAIN`. (The proxy README's OAuth-M2M prose is stale; `-oidc-*` is end-user edge auth, out of scope.) |
| DNS | NetBird-managed, via `DNSRecord` CRDs: (1) **A record** `<cluster address>` → proxy LB IP (NetBird verifies this to consider the proxy configured), (2) **catch-all** `*.<domain>` → the proxy. Per-service records are unnecessary (the catch-all covers all hostnames). |
| Networks | Reuse existing `kube01` / `kube.ccba.sh`; the feature creates no Networks/Routers/Zones. |

## Components

### 1. `ReverseProxyCluster` CRD — deploy + enroll (Phase 1)
Bespoke reconciler (owns child workloads, like `NetworkRouterReconciler` /
`ClusterProxyReconciler`), NOT the generic `MirrorReconciler`. Per reconcile:
1. Mint a proxy token (`ReverseProxyTokens.Create`) → store `PlainToken` in an
   operator-owned Secret (one-shot, like `ClusterProxy`'s API-key Secret).
2. `Deployment` of `netbirdio/reverse-proxy`: env `NB_PROXY_TOKEN` (Secret),
   `NB_PROXY_MANAGEMENT_ADDRESS`, `NB_PROXY_DOMAIN=<spec.clusterAddress>`,
   `NB_PROXY_CERTIFICATE_DIRECTORY=/certs`, `NB_PROXY_ACME_CERTIFICATES=false`,
   `NB_PROXY_CERT_LOCK_METHOD=k8s-lease`; cert-manager Secret mounted read-only
   at `/certs`; health probes on `:8080`; lease RBAC for the proxy SA.
3. `Service type=LoadBalancer` → proxy pods (advertised by the LB controller).
4. `DNSRecord`s: A `<clusterAddress>` → LB IP; catch-all `*.<domain>` → proxy.
   (Wait for `status.loadBalancer.ingress` before writing the A record, as the
   LoadBalancer controller does.)
5. Status: `clusterAddress`, `tokenID`, ready when the Deployment is Available
   **and** `GetProxyClusterByAddress` resolves (the proxy has enrolled).

Delete/GC: revoke the token, drop the account cluster (`ReverseProxyClusters.Delete`),
children GC by owner refs. Finalizer `reverseproxycluster`.

**Independently useful:** an admin can target it with manual `ReverseProxyService`
CRs — the whole path works without any Gateway API.

### 2. ClusterIP backend on `ReverseProxyService` (Phase 2)
Today `backends[]` resolve to an advertised LoadBalancer `DNSRecord` FQDN. Add a
target shape that resolves a `serviceRef` to its in-cluster DNS name
(`<svc>.<ns>.svc.cluster.local`) + port and emits `Host=<that>`,
`TargetType=Cluster`, `DirectUpstream=true`. This is the load-bearing "no
different than today" piece. Existing LB-backed path stays intact.

### 3. Gateway-API controllers (Phases 3–4)
Add `sigs.k8s.io/gateway-api` (Standard channel) to `go.mod`; register the
scheme; **vendor its CRDs into the chart** (envtest loads CRDs from the chart,
and `make generate` won't produce upstream CRDs); hand-add RBAC for
`gateway.networking.k8s.io` (gatewayclasses/gateways/httproutes + `/status`).

- **GatewayClass controller** — accept classes whose `controllerName ==
  netbird.io/byop-proxy`; `parametersRef` → a `ReverseProxyCluster`.
- **Gateway controller** — for accepted-class Gateways: validate listeners,
  bind/reference the `ReverseProxyCluster`, publish `status.addresses` from the
  proxy Service, set `Accepted`/`Programmed`. The listener `certificateRefs`
  Secret is the cert the proxy mounts.
- **HTTPRoute controller** — for routes whose `parentRefs` resolve to an operator
  Gateway: per `(hostname × rule)` emit an **owner-ref'd `ReverseProxyService`**
  child (prune/GC like `loadbalancer_controller.go`); set `RouteParentStatus`
  (`Accepted`, `ResolvedRefs`). hostname→`domain`, backendRefs→`backends`
  (ClusterIP target), PathPrefix→`backends[].path`.

**Unsupported HTTPRoute features** (filters/redirect/URLRewrite, non-prefix
matches, weights, header/method match, multiple weighted backends) are **not**
silently dropped — surface them as a Route condition (`PartiallyInvalid` /
`Accepted=False`), to keep "no different than today" honest.

**Coexistence:** auto `ReverseProxyService`s are owner-ref'd + labeled; manual
ones are untouched. NetBird allows one service per domain, so detect a
domain collision and refuse with a condition rather than flap.

## Phasing

- **P1 — `ReverseProxyCluster`** (deploy+enroll+Service+DNS). Shippable alone.
- **P2 — ClusterIP backend** on `ReverseProxyService`.
- **P3 — GatewayClass + Gateway** controllers (+ gateway-api dep, CRDs, RBAC).
- **P4 — HTTPRoute → ReverseProxyService** translator.
- **P5 — migration + docs.** Cut kgateway over (reuse the cert Secret, delete the
  kgateway Gateway). Amend `docs/architecture.md` — it currently states the
  operator ships no Gateway/GatewayClass; this feature deliberately reverses that.

## Open items / risks

- **Image:** confirm the exact `netbirdio/reverse-proxy` tag/digest for module
  v0.73.x; add it to `internal/version` (and a version guard if feasible).
- **Cluster derivation:** `secrets.ccbash.cloud` must derive the BYOP cluster
  server-side — register `ccbash.cloud` (or put `NB_PROXY_DOMAIN` under it) so the
  suffix match resolves. Handle in the `ReverseProxyCluster` reconciler.
- **BYOP API availability** on the target management (self-hosted assumed).
- **Cardinality:** one proxy Deployment per Gateway vs one shared cluster — affects
  address uniqueness (addresses are unique per account) and certs.
- **Wildcard HTTPRoute hostnames** vs concrete-host `ReverseProxyService`.

## Testing

envtest (CRDs from `charts/netbird-operator/crds`, `make generate` first) +
`internal/netbirdmock` for all NetBird REST — extend `addHandler` for
`/api/reverse-proxies/proxy-tokens`. Gateway-API CRDs must be vendored into the
chart for envtest. Replay the downstream fixtures
(`secrets.ccbash.cloud`/`webmail.ccbash.cloud`) and assert the generated
`ReverseProxyService` matches the manual equivalent, plus prune-on-delete and the
collision/unsupported-feature conditions.
