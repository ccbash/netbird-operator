# NetBird Kubernetes Operator.

The NetBird Kubernetes Operator brings NetBird network access into the Kubernetes
API. NetBird API objects — networks, routers, resources, DNS zones and records,
groups, setup keys, reverse-proxy services — are mirrored 1:1 as custom
resources, and the operator translates Kubernetes **`Service type=LoadBalancer`**
addresses into NetBird reachability and exposure. Everything is declarative and
reconciled, so NetBird access is managed the same way as the rest of your
cluster.

See [`docs/architecture.md`](docs/architecture.md) for the design.

## Why LoadBalancer addresses

The operator makes a Service's **LoadBalancer IP** reachable over NetBird — never
its ClusterIP. ClusterIPs come from a huge, unpredictably-allocated service CIDR
that is identical on every default cluster, so routing them across your
infrastructure invites collisions; an LB CIDR is small, deliberately chosen and
collision-free. IP allocation is left to your existing load balancer (Cilium
LB-IPAM, MetalLB, kgateway, a cloud LB); the operator owns only the NetBird
overlay and DNS.

## Features

**NetBird-mirror CRDs**

* Thin, 1:1 mirrors of NetBird API objects — `Network`, `NetworkRouter`,
  `NetworkResource`, `DNSZone`, `DNSRecord`, `ReverseProxyService`, `Group`,
  `SetupKey`. Each reconciles its spec straight to the NetBird Management API.
* **Routing peers, your way** — a `NetworkRouter` either reuses an existing
  NetBird group (e.g. host-level netbird on your nodes) or deploys a
  `hostNetwork` netbird-client DaemonSet.

**Translation & exposure**

* **Automatic reachability** — every `Service type=LoadBalancer` is advertised
  into a NetBird network (default-on; opt out per namespace/Service with the
  `netbird.io/advertise` annotation). Per LB ingress IP family the operator
  creates a `NetworkResource` (the IP) and a `DNSRecord` (`<svc>-<ns>.<zone>`,
  A + AAAA) — one **dualstack** name per Service.
* **Reverse-proxy exposure** — a `ReverseProxyService` publishes Services
  through NetBird's reverse proxy, **internally or externally**, targeting each
  backend Service's dualstack DNS name with path awareness.

## How it works

1. A **`Network`** mirrors a NetBird network. A **`NetworkRouter`** binds the
   routing peers to it — `peers.group` reuses an existing group, or
   `peers.deploy` runs a DaemonSet.
2. A **`DNSZone`** (admin-authored or adopted by name) holds the per-service
   records.
3. Give a Service `type: LoadBalancer` (any LB / IPAM allocates the IP) and the
   operator advertises it: a `NetworkResource` + `DNSRecord` per IP family.
4. To publish a Service through the reverse proxy, author a
   **`ReverseProxyService`** referencing it.

## Quick start

Create the API-key secret and install the operator:

```shell
kubectl create namespace netbird
kubectl -n netbird create secret generic netbird-mgmt-api-key --from-literal NB_API_KEY=${NETBIRD_API_KEY}
helm upgrade --install --create-namespace -n netbird netbird-operator \
  oci://ghcr.io/netbirdio/helm-charts/netbird-operator
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
  crowdsecMode: observe
```

A full walkthrough is in [`examples/expose`](examples/expose/README.md). See the
[NetBird Kubernetes docs](https://docs.netbird.io/manage/integrations/kubernetes)
for management-side setup.

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
| [ClusterProxy](docs/api-reference.md#clusterproxy) | Cluster-API proxy |

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
