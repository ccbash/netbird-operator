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
`NBServicePolicy` configures that proxy service (here: `routingMode` and a
CrowdSec mode).

```shell
kubectl apply -f ./examples/gateway-api/nginx.yaml
```

Expose the Kubernetes API server as a private (L4) network resource via a
`TCPRoute` on the private Gateway.

```shell
kubectl apply -f ./examples/gateway-api/kubernetes.yaml
```

## Routing modes

An `NBServicePolicy` attached to an `HTTPRoute` selects how its backends are
exposed via `spec.routingMode`:

* `ip` (default) — a host resource at the Service ClusterIP with a host proxy
  target. DNS-independent, IPv4. Robust; the recommended default.
* `domain` — a domain resource at the Service FQDN (`<svc>-<ns>.<zone>`) with a
  domain proxy target and A/AAAA records. Dualstack, but the routing peer / proxy
  must be able to resolve the zone via NetBird DNS, which requires the zone to be
  distributed to that peer and the Service CIDRs to be routed
  (`NetworkRouter.spec.serviceCIDRs`).

When no `NBServicePolicy` targets a route, it defaults to `ip`.
