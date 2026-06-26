# NetBird Kubernetes Operator.

> **A divergent fork.** This project began as a fork of the official
> [`netbirdio/kubernetes-operator`](https://github.com/netbirdio/kubernetes-operator),
> but has since been re-architected and now differs substantially from it. The
> key differences:
>
> - **NetBird CRDs mirror the NetBird API 1:1** — thin `Network`,
>   `NetworkResource`, `DNSZone`, `DNSRecord`, `ReverseProxyService`, `Group`,
>   `SetupKey`, all driven by a single generic reconciler — instead of a few fat,
>   overloaded CRDs.
> - **Exposure is built on `Service type=LoadBalancer`, not the Gateway API.** It
>   advertises LB IPs (allocated by your existing LB/IPAM — Cilium LB-IPAM,
>   MetalLB, a cloud LB) and **never routes ClusterIPs**; the operator ships no
>   `Gateway`/`GatewayClass` of its own.
> - **Routing peers are pluggable** — reuse an existing NetBird group (e.g. the
>   host-level netbird already on your nodes) or let the operator deploy a
>   `hostNetwork` DaemonSet.
> - **One exposure primitive, `ReverseProxyService`**, for both internal and
>   external publishing, with transparent dualstack DNS.
>
> If you want the upstream behaviour (Gateway API, ClusterIP routing), use the
> official operator instead.

The NetBird Kubernetes Operator brings NetBird network access into the Kubernetes
API: NetBird API objects are managed as custom resources, and in-cluster
`Service type=LoadBalancer` addresses are advertised over the NetBird mesh.
Everything is declarative and reconciled. See
[`docs/architecture.md`](docs/architecture.md) for the design.

## Why LoadBalancer addresses

The operator routes a Service's **LoadBalancer IP**, never its ClusterIP.
ClusterIPs come from a huge, unpredictably-allocated service CIDR that is
identical on every default cluster, so routing them across your infrastructure
invites collisions; an LB CIDR is small, deliberately chosen and collision-free.
The operator owns only the NetBird overlay and DNS — your existing LB/IPAM
allocates the addresses. Managed clusters get a cloud LoadBalancer for free;
on-prem you supply one, e.g. [MetalLB](https://metallb.io/),
[Cilium LB-IPAM](https://docs.cilium.io/en/stable/network/lb-ipam/), or
[kube-vip](https://kube-vip.io/). Without a LoadBalancer implementation a
`type: LoadBalancer` Service stays `<pending>` and nothing is advertised.

## How it works

1. A **`Network`** mirrors a NetBird network. A **`NetworkRouter`** binds the
   **routing peers** to it — the NetBird peers that actually carry traffic to the
   advertised LoadBalancer IPs. You pick one of two ways to provide them, and the
   operator configures the `NetworkRouter` either way:

   - **Reuse existing peers** (`peers.group`) — point at the NetBird group the
     host-level netbird clients already running on your cluster nodes auto-join.
     The operator creates **only** the router and deploys nothing.
   - **Deploy peers** (`peers.deploy`) — the operator runs a `hostNetwork`
     `netbird-client` **DaemonSet** as the routing peers and manages its `Group`,
     `SetupKey`, and DaemonSet, wiring the router to it automatically. Use this
     when your nodes don't already run netbird.

2. A **`DNSZone`** (admin-authored or adopted by name) holds the per-service
   records.
3. Give a Service `type: LoadBalancer` (any LB / IPAM allocates the IP) and the
   operator advertises it: a `NetworkResource` + `DNSRecord` (A + AAAA — one
   dualstack name) per IP family. Default-on; opt out per namespace/Service with
   the `netbird.io/advertise` annotation.
4. To publish a Service through the reverse proxy (internal or external), author
   a **`ReverseProxyService`** referencing it.

## Quick start

Create the API-key secret and install the operator:

```shell
kubectl create namespace netbird
kubectl -n netbird create secret generic netbird-mgmt-api-key --from-literal NB_API_KEY=${NETBIRD_API_KEY}
helm upgrade --install --create-namespace -n netbird netbird-operator \
  oci://ghcr.io/ccbash/helm-charts/netbird-operator
```

Define the network, its routing peers, and the DNS zone:

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
    group: { name: kube-nodes }     # reuse the group your node netbird joins
    # deploy: {}                     # …or let the operator run a DaemonSet
---
apiVersion: netbird.io/v1alpha1
kind: DNSZone
metadata: { name: kube, namespace: netbird }
spec: { name: kube.example.com, domain: kube.example.com }
```

Expose a Service: give it `type: LoadBalancer` (advertised automatically), then
publish it through the proxy:

```yaml
apiVersion: netbird.io/v1alpha1
kind: ReverseProxyService
metadata: { name: app, namespace: default }
spec:
  backends:
    - serviceRef: { name: app }      # a type=LoadBalancer Service
  proxyCluster: gate.example.com     # your NetBird reverse-proxy cluster
  domain: app.example.com
  crowdsecMode: observe              # off | observe | enforce
  passHostHeader: true               # advanced toggles (optional)
  # rewriteRedirects: true
  # private: true                    # internal mesh-only instead of public
```

A full walkthrough is in [`examples/expose`](examples/expose/README.md). See the
[NetBird Kubernetes docs](https://docs.netbird.io/manage/integrations/kubernetes)
for management-side setup.

## HowTos

Worked examples under [`examples/`](examples/), each with its own README:

| Example | What it shows |
|---------|---------------|
| [expose](examples/expose/README.md) | Advertise an HTTP `Service type=LoadBalancer` over the mesh and publish it through the NetBird reverse proxy (the end-to-end walkthrough). |
| [mail](examples/mail/README.md) | Expose a mail server on many TCP ports (SMTP/IMAP/ManageSieve) under one hostname — L4 per-port services with PROXY protocol. |
| [gateway](examples/gateway/README.md) | How a Gateway API `Gateway`/`HTTPRoute` is handled — the operator advertises the gateway's LoadBalancer Service; routing stays the gateway's job. |
| [cluster-proxy](examples/cluster-proxy/README.md) | Put the Kubernetes API server on the mesh and reach it with `kubectl` over NetBird. |
| [sidecar](examples/sidecar/README.md) | Inject a NetBird sidecar peer into Pods via a `SidecarProfile`. |

## API

| Kind | Purpose |
|------|---------|
| [Network](docs/api-reference.md#network) | A NetBird network |
| [NetworkRouter](docs/api-reference.md#networkrouter) | Routing peers bound to a network (reuse a group or deploy a DaemonSet) |
| [NetworkResource](docs/api-reference.md#networkresource) | One address routed into a network |
| [DNSZone](docs/api-reference.md#dnszone) | A NetBird managed DNS zone |
| [DNSRecord](docs/api-reference.md#dnsrecord) | A record in a DNSZone |
| [ReverseProxyService](docs/api-reference.md#reverseproxyservice) | Publish Services through the NetBird reverse proxy |
| [Group](docs/api-reference.md#group) | A NetBird group |
| [SetupKey](docs/api-reference.md#setupkey) | A NetBird setup key |
| [SidecarProfile](docs/api-reference.md#sidecarprofile) | Sidecar peer injection profile |
| [ClusterProxy](docs/api-reference.md#clusterproxy) | Put the Kubernetes API server on the mesh; `kubectl` over NetBird with group→RBAC impersonation ([flow](docs/architecture.md#cluster-api-proxy)) |

Full field reference: [`docs/api-reference.md`](docs/api-reference.md).

## Configuration

Command-line flags (see `--help`); the most useful:

| Flag | Default | Purpose |
|------|---------|---------|
| `--log-level` | `info` | `debug`, `info`, `warn`, `error`, or an integer for higher debug verbosity. |
| `--log-format` | `json` | `json` (structured) or `console` (human-readable). |
| `--advertise-loadbalancers` | `true` | Advertise `Service type=LoadBalancer` by default (annotation `netbird.io/advertise` overrides per namespace/Service). |
| `--netbird-management-url` | `https://api.netbird.io` | NetBird Management API URL (self-hosted). |
| `--netbird-client-image` | (built-in) | Image for a `NetworkRouter`'s `peers.deploy` DaemonSet. |
| `--metrics-bind-address` | `0` (off) | `:8080` HTTP or `:8443` HTTPS metrics. |

With the Helm chart these are set through values (`operator.logging.*`,
`managementURL`, …).

## Observability

- An advertised `Service` gets a Kubernetes **`Advertised` event** (`kubectl
  describe svc`) carrying its NetBird FQDN; problems surface as `Warning` events.
- The mirror CRDs carry a **`Ready` condition** — e.g. `kubectl get
  reverseproxyservice,networkresource,dnsrecord -A`.
- Set `--metrics-bind-address` (`:8080`/`:8443`) to expose the built-in
  controller-runtime **metrics** — per-controller reconcile rate, error count
  and latency.
