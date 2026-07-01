# NetBird Kubernetes Operator

**Put your Kubernetes Services on your private [NetBird](https://netbird.io) mesh — declaratively.**

Drop a `type: LoadBalancer` on a Service and it's reachable over your overlay,
with DNS managed for you. Want HTTPS in front of it? The operator **deploys and
enrolls a NetBird reverse proxy in your cluster** and wires it up straight from
Gateway API manifests. No public edge to babysit, no ClusterIP routing to
untangle across clusters — just NetBird objects mirrored 1:1 into Kubernetes and
continuously reconciled. Architecture: [`docs/architecture.md`](docs/architecture.md).

## How this differs from the upstream NetBird operator

This started as a fork of the official
[`netbirdio/kubernetes-operator`](https://github.com/netbirdio/kubernetes-operator)
and has been re-architected enough to be, effectively, a different operator:

- **CRDs mirror the NetBird API 1:1.** Thin `Network`, `NetworkResource`,
  `DNSZone`, `DNSRecord`, `ReverseProxyService`, `Group`, `SetupKey` — one
  generic reconciler driving them all — instead of a few broad, opinionated CRDs.
- **It routes the LoadBalancer IP, never the ClusterIP.** ClusterIPs come from a
  large, identically-allocated CIDR that collides across clusters; an LB CIDR is
  small, deliberate and collision-free (see use case 1).
- **The reverse proxy is the operator's job.** It deploys + enrolls a NetBird
  reverse proxy in-cluster and drives it from the Gateway API — owning its own
  `GatewayClass` — so you don't stand one up or wire it yourself (see use case 2).
- **Routing peers are pluggable.** Reuse the NetBird group your nodes already
  join, or let the operator run a `hostNetwork` DaemonSet.

Want the upstream behaviour? Use the official operator instead.

## Two ways to expose a Service

| | **1 · Advertise the LoadBalancer IP** | **2 · NetBird reverse proxy** |
|---|---|---|
| What | The Service's **LB IP** becomes mesh-routable + gets a DNS name | The operator **deploys a NetBird reverse proxy** in your cluster that fronts your Services |
| You give it | `Service type=LoadBalancer` | A Gateway API `Gateway` + `HTTPRoute`s |
| Operator creates | `NetworkResource` + dualstack `DNSRecord` | A `ReverseProxyCluster` (proxy Deployment + LB Service + DNS) and a `ReverseProxyService` per route |
| Backend reached | Directly at the LB IP, over the mesh | Proxy dials the backend ClusterIP in-cluster |
| Use when | The workload owns a routable address (L3/L4, any protocol) | You want host/TLS-terminating HTTP exposure without a routable IP per app |

Both are internal-by-default (reachable over the NetBird mesh, not the public
internet) and can be combined in one cluster.

## Install

```shell
kubectl create namespace netbird
kubectl -n netbird create secret generic netbird-mgmt-api-key \
  --from-literal NB_API_KEY=${NETBIRD_API_KEY}
helm upgrade --install --create-namespace -n netbird netbird-operator \
  oci://ghcr.io/ccbash/helm-charts/netbird-operator
```

The chart reads the API key from the `netbird-mgmt-api-key` secret by default.
Self-hosted Management API? add `--set managementURL=https://netbird.example.com`.
Configure the rest (use cases below) through Helm values.

---

## Use case 1 — Advertise a `Service type=LoadBalancer`

The operator routes a Service's **LoadBalancer IP**, never its ClusterIP.
ClusterIPs come from a large, identically-allocated service CIDR that collides
across clusters; an LB CIDR is small, deliberately chosen and collision-free.
Your existing LB/IPAM allocates the address — a cloud LoadBalancer, or on-prem
[MetalLB](https://metallb.io/),
[Cilium LB-IPAM](https://docs.cilium.io/en/stable/network/lb-ipam/), or
[kube-vip](https://kube-vip.io/). (Without one, a `type: LoadBalancer` Service
stays `<pending>` and nothing is advertised.)

**1. Point the operator at a Network and a DNS zone** (Helm values):

```yaml
loadBalancer:
  network: kube                   # the Network CR advertised resources attach to
  resourceGroups: All             # NetBird groups the resource joins (for access policies)
  dnsZone:
    domain: kube.example.com      # the operator creates and owns this DNSZone
    distributionGroups: [ All ]   # peers that receive the zone (so they resolve it)
  advertise: true                 # default-on; opt out per ns/Service via netbird.io/advertise
```

**2. Author the Network and its routing peers** (the NetBird peers that carry
traffic to the advertised IPs):

```yaml
apiVersion: netbird.io/v1alpha1
kind: Network
metadata: { name: kube, namespace: netbird }
spec: { name: kube }
---
apiVersion: netbird.io/v1alpha1
kind: NetworkRouter
metadata: { name: kube, namespace: netbird }
spec:
  networkRef: { name: kube, namespace: netbird }
  peers:
    group: { name: kube-nodes }   # reuse the group your nodes' netbird clients join
    # deploy: {}                   # …or let the operator run a hostNetwork DaemonSet
```

**3. Give the workload a LoadBalancer Service.** That's it — the operator emits a
`NetworkResource` (the IP, routable via the router peers) and a `DNSRecord`
(`<svc>-<ns>.kube.example.com`, A + AAAA for one dualstack name) per LB ingress
IP family. Opt a Service or namespace out with `netbird.io/advertise: "false"`.

Advertising makes the Service reachable **over the mesh**. To also front it with
a reverse proxy, author a [`ReverseProxyService`](docs/api-reference.md#reverseproxyservice)
targeting it (`proxyCluster`) — and **if that reverse proxy is reachable from the
internet, the Service is exposed publicly** through it. Keep it mesh-only with
`private: true`.

Walkthrough: [`examples/expose`](examples/expose/README.md) ·
multi-port L4 (mail): [`examples/mail`](examples/mail/README.md).

---

## Use case 2 — Front Services with an in-cluster NetBird reverse proxy

Turn on the Gateway API controllers and the operator becomes a **Gateway API
controller** (`controllerName: netbird.io/gateway-controller`). It **creates and
owns its own `GatewayClass`** (default name `netbird`). Each `Gateway` of that
class becomes one NetBird reverse-proxy instance the operator **deploys and
enrolls in-cluster** (a `ReverseProxyCluster`: proxy `Deployment` + LoadBalancer
`Service` + token + DNS + a NetBird custom domain). `HTTPRoute`s attached to it
are translated into `ReverseProxyService`s whose backends are dialed **directly
at their ClusterIP** — no routable IP per app, TLS terminated at the proxy.

**1. Enable it** (Helm values):

```yaml
enableGatewayAPI: true            # the operator creates+owns the `netbird` GatewayClass
# gatewayClassName: netbird       # override the class name if you like
```

**2. Author a Gateway and its proxy parameters.** The proxy "flavor" lives in a
namespaced `ReverseProxyClusterParameters` the Gateway points at via
`spec.infrastructure.parametersRef` (same namespace). The wildcard listener cert
is issued by cert-manager's gateway-shim from the annotation:

```yaml
apiVersion: netbird.io/v1alpha1
kind: ReverseProxyClusterParameters
metadata: { name: netbird, namespace: netbird }
spec:
  private: false                  # centralised: the proxy dials ClusterIP backends directly
  logLevel: error                 # recommended when centralised — silences the embedded
                                  # client's unused P2P/ICE warnings (peers reach the proxy
                                  # at its LB IP, never via a WireGuard tunnel to it)
  groups: [ { name: All } ]       # groups the proxy's LB resource joins
  # image / replicas / serviceAnnotations: optional overrides
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: netbird
  namespace: netbird
  annotations:
    cert-manager.io/cluster-issuer: your-issuer   # gateway-shim issues the listener cert
spec:
  gatewayClassName: netbird
  infrastructure:
    parametersRef: { group: netbird.io, kind: ReverseProxyClusterParameters, name: netbird }
  listeners:
    - name: https
      protocol: HTTPS
      port: 443
      hostname: "*.example.com"   # → domain example.com, clusterAddress gate.example.com
      tls:
        mode: Terminate
        certificateRefs: [ { name: example-com-wildcard-tls } ]
      allowedRoutes: { namespaces: { from: All } }
```

**3. Expose a Service with an `HTTPRoute`** on that Gateway:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: { name: app, namespace: default }
spec:
  parentRefs: [ { name: netbird, namespace: netbird, sectionName: https } ]
  hostnames: [ app.example.com ]
  rules:
    - backendRefs: [ { name: app, port: 80 } ]    # a ClusterIP Service
```

`app.example.com` is now served by the in-cluster NetBird reverse proxy and
reachable over the mesh. Design + rationale:
[`docs/design/byop-gateway.md`](docs/design/byop-gateway.md).

> **Advanced:** to publish without a Gateway, author a `ReverseProxyService`
> directly (referencing an existing proxy cluster) — see
> [`docs/api-reference.md#reverseproxyservice`](docs/api-reference.md#reverseproxyservice).

---

## API

| Kind | Purpose |
|------|---------|
| [Network](docs/api-reference.md#network) | A NetBird network |
| [NetworkRouter](docs/api-reference.md#networkrouter) | Routing peers bound to a network (reuse a group or deploy a DaemonSet) |
| [NetworkResource](docs/api-reference.md#networkresource) | One address routed into a network |
| [DNSZone](docs/api-reference.md#dnszone) | A NetBird managed DNS zone |
| [DNSRecord](docs/api-reference.md#dnsrecord) | A record in a DNSZone |
| [ReverseProxyCluster](docs/api-reference.md#reverseproxycluster) | A NetBird reverse proxy the operator deploys + enrolls in-cluster |
| [ReverseProxyClusterParameters](docs/api-reference.md#reverseproxyclusterparameters) | Per-Gateway proxy flavor (image/replicas/groups/private) |
| [ReverseProxyService](docs/api-reference.md#reverseproxyservice) | Publish a Service through a reverse proxy |
| [Group](docs/api-reference.md#group) | A NetBird group |
| [SetupKey](docs/api-reference.md#setupkey) | A NetBird setup key |
| [SidecarProfile](docs/api-reference.md#sidecarprofile) | Sidecar peer injection profile |
| [ClusterProxy](docs/api-reference.md#clusterproxy) | Put the Kubernetes API server on the mesh; `kubectl` over NetBird ([flow](docs/architecture.md#cluster-api-proxy)) |

Full field reference: [`docs/api-reference.md`](docs/api-reference.md).

## Configuration

Set via Helm values; the underlying flags (see `--help`):

| Flag (Helm value) | Default | Purpose |
|------|---------|---------|
| `--advertise-loadbalancers` (`loadBalancer.advertise`) | `true` | Advertise `Service type=LoadBalancer` by default (annotation `netbird.io/advertise` overrides). |
| `--loadbalancer-network` (`loadBalancer.network`) | — | Name of the `Network` advertised Services attach to. |
| `--loadbalancer-dns-zone` (`loadBalancer.dnsZone.domain`) | — | Apex domain of the operator-owned `DNSZone` advertised records land in. |
| `--loadbalancer-dns-zone-groups` (`loadBalancer.dnsZone.distributionGroups`) | — | NetBird groups whose peers receive that zone. |
| `--default-resource-groups` (`loadBalancer.resourceGroups`) | — | Groups advertised resources join (annotation `netbird.io/groups` overrides). |
| `--enable-gateway-api` (`enableGatewayAPI`) | `false` | Run the Gateway API controllers; the operator creates+owns its GatewayClass. |
| `--gateway-class-name` (`gatewayClassName`) | `netbird` | Name of that operator-owned GatewayClass. |
| `--netbird-management-url` (`managementURL`) | `https://api.netbird.io` | NetBird Management API URL (self-hosted). |
| `--log-level` / `--log-format` (`operator.logging.*`) | `info` / `json` | Verbosity and output format. |
| `--metrics-bind-address` | `0` (off) | `:8080` HTTP or `:8443` HTTPS metrics. |

## Examples

Worked examples under [`examples/`](examples/), each with its own README:

| Example | What it shows |
|---------|---------------|
| [expose](examples/expose/README.md) | Advertise an HTTP `Service type=LoadBalancer` over the mesh and publish it through a reverse proxy (the end-to-end walkthrough). |
| [mail](examples/mail/README.md) | Many TCP ports (SMTP/IMAP/ManageSieve) under one hostname — L4 per-port services with PROXY protocol. |
| [gateway](examples/gateway/README.md) | Advertising a third-party gateway's (kgateway/Cilium/Istio) LoadBalancer Service — and how the operator's own Gateway controller differs. |
| [cluster-proxy](examples/cluster-proxy/README.md) | Put the Kubernetes API server on the mesh and reach it with `kubectl` over NetBird. |
| [sidecar](examples/sidecar/README.md) | Inject a NetBird sidecar peer into Pods via a `SidecarProfile`. |

## Observability

- An advertised `Service` gets a Kubernetes **`Advertised` event** (`kubectl
  describe svc`) carrying its NetBird FQDN; problems surface as `Warning` events.
- Mirror CRDs and the Gateway/HTTPRoute carry a **`Ready`/`Accepted` condition** —
  e.g. `kubectl get reverseproxycluster,reverseproxyservice,dnsrecord -A`.
- Set `--metrics-bind-address` (`:8080`/`:8443`) for the built-in
  controller-runtime **metrics** (per-controller reconcile rate, errors, latency).
