# Gateway API

This example walks you through setting up the NetBird Gateway API integration and
exposing Nginx through the NetBird reverse proxy.

It shows the two exposure paths:

* **`HTTPRoute` → `public` Gateway** (class `netbird`) — publishes a Service
  through the NetBird reverse proxy (L7, public hostname). See `nginx.yaml`.
* **`TCPRoute` → `private` Gateway** (class `netbird`) — exposes a Service as a
  private network resource reachable only by mesh peers (L4). See
  `kubernetes.yaml`.

Build the image locally and load it into Kind.

```shell
make build-image
kind load docker-image ghcr.io/netbirdio/netbird-operator:dev
```

Install the Gateway API CRDs (the operator also reconciles the experimental
`TCPRoute`).

```shell
kubectl apply --server-side -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.0/experimental-install.yaml
```

Create the NetBird namespace and API-key secret.

```shell
kubectl create namespace netbird
kubectl -n netbird create secret generic netbird-mgmt-api-key --from-literal NB_API_KEY=${NETBIRD_API_KEY}
```

Install the operator. The values file enables Gateway API support.

```shell
helm upgrade --install --create-namespace -f ./examples/gateway-api/values.yaml -n netbird netbird-operator ./charts/netbird-operator
```

Create the GatewayClasses, Gateways and the `NetworkRouter` (which deploys the
NetBird routing-peer clients). The `NetworkRouter` also routes the cluster's
Service CIDRs into the network so ClusterIPs are reachable, and assigns the
resources to a NetBird group.

```shell
kubectl apply -f ./examples/gateway-api/gateway.yaml
```

Deploy the test Nginx app with an `HTTPRoute` and an `NBServicePolicy`. The
`HTTPRoute` publishes the Service through the public reverse proxy; the
`NBServicePolicy` configures that proxy service (here: the `proxyCluster`,
`upstream` form, and a CrowdSec mode).

```shell
kubectl apply -f ./examples/gateway-api/nginx.yaml
```

Expose the Kubernetes API server as a private (L4) network resource via a
`TCPRoute` on the private Gateway.

```shell
kubectl apply -f ./examples/gateway-api/kubernetes.yaml
```

## Upstream form

For HTTP exposure the `NBServicePolicy` names the reverse-proxy cluster
(`spec.proxyCluster`, required) and selects how the proxy reaches each backend
via `spec.upstream`:

* `hostname` (default) — the proxy dials the Service FQDN (`<svc>-<ns>.<zone>`)
  and resolves it via NetBird DNS (A/AAAA), so IPv4/IPv6 is transparent. The zone
  must be distributed to the proxy cluster, and the Service CIDRs routed
  (`NetworkRouter.spec.serviceCIDRs`), for the proxy to reach the ClusterIP.
* `ip` — the proxy dials the Service ClusterIP directly (single address family,
  DNS-independent).

Cluster targets dial the backend directly (NetBird requires direct-upstream), so
the proxy cluster itself must be able to reach the backend; no per-Service NetBird
resource is created for HTTP. (`TCPRoute` L4 exposure instead creates a host
`NetworkResource` per ClusterIP family — see `spec.ipFamilies`.)
