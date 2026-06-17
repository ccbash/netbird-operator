# NetBird Kubernetes Operator

The NetBird Kubernetes Operator brings NetBird network access into the Kubernetes
API. It manages NetBird networks, routers, resources, groups, setup keys and peers
as custom resources, and — through the Gateway API — exposes in-cluster Services
over the NetBird mesh, either as a public reverse-proxy endpoint or as a private,
peers-only resource. Everything is declarative and reconciled, so NetBird access
is managed the same way as the rest of your cluster.

## Features

**Core network management**

* Declarative NetBird **networks, routers, resources, groups and setup keys** as
  Kubernetes objects; the operator provisions and reconciles them against the
  NetBird management API.
* **Automatic secret management** — setup keys and credentials are stored and
  rotated as Kubernetes secrets.
* **Sidecar injection** (`SidecarProfile`) and **cluster-API proxy**
  (`ClusterProxy`) for peer-style connectivity.
* Namespace-scoped or cluster-wide; works with NetBird Cloud or self-hosted.

**Service exposure (Gateway API)**

* **Public reverse proxy** — an `HTTPRoute` on a `netbird-public` Gateway
  publishes a Service through NetBird's L7 reverse proxy under a public hostname.
  Path-based rules are honoured (one proxy target per backend, carrying its path
  prefix), and the proxy service is updated idempotently.
* **Private resource** — a `TCPRoute` on a `netbird-private` Gateway exposes a
  Service as an L4 network resource reachable only by mesh peers.
* **Per-route policy** (`NBServicePolicy`, GEP-713 direct attachment) configures
  the reverse-proxy service for the route(s) it targets — and keeps the settings
  applied, instead of them being reset on each reconcile:
  * `private` + `accessGroups`
  * `crowdsecMode` — `off` / `observe` / `enforce`
  * `accessRestrictions` — allowed/blocked CIDRs and ISO-3166 country codes
  * `passHostHeader`, `rewriteRedirects`
  * `routingMode` — see below

**Routing & DNS**

* **Selectable routing mode** per service (`NBServicePolicy.spec.routingMode`,
  default `ip`):

  | Mode | NetBird resource | Proxy target | DNS at request time | Address family |
  |------|------------------|--------------|---------------------|----------------|
  | `ip` (default) | host @ Service ClusterIP | `host` | none | IPv4 |
  | `domain` | domain @ Service FQDN | `domain` | resolved via NetBird DNS | dualstack (A + AAAA) |

  `ip` is robust and DNS-independent. `domain` enables dualstack but the proxy/peer
  must resolve the zone via NetBird DNS — which requires the Service CIDRs to be
  routed and the DNS zone to be distributed to that peer (see below).
* **Dualstack DNS** — per service, an **A** record per IPv4 and an **AAAA** per
  IPv6 `ClusterIP` are published under `<svc>-<ns>.<zone>` (a single label, which
  NetBird's managed zones serve). Records are reconciled against the live zone, so
  they're adopted rather than recreated, and the resource is recreated only when
  its type actually has to change.
* **Service-CIDR routing** — `NetworkRouter.spec.serviceCIDRs` routes the
  cluster's Service CIDRs into the NetBird network as subnet resources so
  ClusterIPs are reachable through the routing peers. `NetworkRouter.spec.resourceGroups`
  puts the network's resources into NetBird groups so access policies can target
  them.

## How it works

* A **`NetworkRouter`** owns a NetBird network: it deploys the routing-peer
  client workload, references the DNS zone (`dnsZoneRef`), routes the
  `serviceCIDRs`, and tags resources with `resourceGroups`.
* Two **`GatewayClass`es** — `netbird-public` and `netbird-private` — are
  provided by the operator. A `Gateway` of each class attaches to the router.
* A **`Service`** is exposed by attaching an **`HTTPRoute`** (public) or
  **`TCPRoute`** (private) to the matching `Gateway`. The operator creates a
  `NetworkResource` for each backend Service and, for HTTP routes, a reverse-proxy
  service whose targets point at those resources.
* An **`NBServicePolicy`** attached to an `HTTPRoute` (via `targetRefs`) tunes the
  reverse-proxy service and selects the routing mode.

## Quick start

Install the Gateway API CRDs, create the API-key secret, and install the operator
(enable `gatewayAPI`):

```shell
kubectl apply --server-side -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.0/experimental-install.yaml
kubectl create namespace netbird
kubectl -n netbird create secret generic netbird-mgmt-api-key --from-literal NB_API_KEY=${NETBIRD_API_KEY}
helm upgrade --install --create-namespace -n netbird netbird-operator \
  oci://ghcr.io/netbirdio/helm-charts/netbird-operator --set gatewayAPI.enabled=true
```

Set up a network router and public gateway:

```yaml
apiVersion: netbird.io/v1alpha1
kind: NetworkRouter
metadata: { name: kube, namespace: netbird }
spec:
  dnsZoneRef: { name: cluster.local }
  serviceCIDRs: ["10.96.0.0/12"]   # your cluster's Service CIDR(s)
  resourceGroups: [{ name: All }]
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: { name: public, namespace: netbird }
spec:
  gatewayClassName: netbird-public
  listeners:
    - { name: kube, protocol: gateway.netbird.io/NetworkRouter, port: 1 }
```

Expose a Service and configure it:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: { name: app, namespace: default }
spec:
  parentRefs: [{ name: public, namespace: netbird }]
  hostnames: ["app.example.com"]
  rules: [{ backendRefs: [{ name: app, port: 80 }] }]
---
apiVersion: netbird.io/v1alpha1
kind: NBServicePolicy
metadata: { name: app, namespace: default }
spec:
  targetRefs: [{ group: gateway.networking.k8s.io, kind: HTTPRoute, name: app }]
  routingMode: ip          # or "domain" for dualstack
  crowdsecMode: observe
```

A full walkthrough (including a private `TCPRoute`) is in
[`examples/gateway-api`](examples/gateway-api/README.md). See the
[NetBird Kubernetes docs](https://docs.netbird.io/manage/integrations/kubernetes)
for management-side setup.

## API

| Kind | API Version | Purpose |
|------|-------------|---------|
| [NetworkRouter](docs/api-reference.md#networkrouter) | `netbird.io/v1alpha1` | Network + routing peer, DNS zone, service CIDRs, resource groups |
| [NetworkResource](docs/api-reference.md#networkresource) | `netbird.io/v1alpha1` | A Service exposed in the network (host or domain) |
| [NBServicePolicy](docs/api-reference.md#nbservicepolicy) | `netbird.io/v1alpha1` | Per-route reverse-proxy config + routing mode |
| [Group](docs/api-reference.md#group) | `netbird.io/v1alpha1` | NetBird group |
| [SetupKey](docs/api-reference.md#setupkey) | `netbird.io/v1alpha1` | Setup key |
| [SidecarProfile](docs/api-reference.md#sidecarprofile) | `netbird.io/v1alpha1` | Sidecar peer injection profile |
| [ClusterProxy](docs/api-reference.md#clusterproxy) | `netbird.io/v1alpha1` | Cluster-API proxy |

Service exposure also consumes the upstream Gateway API kinds `HTTPRoute` and
`TCPRoute` (`gateway.networking.k8s.io`). Full field reference:
[`docs/api-reference.md`](docs/api-reference.md).
