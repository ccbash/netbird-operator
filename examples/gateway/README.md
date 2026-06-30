# Exposing a Gateway API Gateway over NetBird

This example covers the **default** mode: the operator has no Gateway API
integration and treats a third-party gateway as just another LoadBalancer
Service. A second, **opt-in** mode (the operator *is* the gateway, backed by a
NetBird BYOP proxy) is summarised at the end.

## Default: gateway as a LoadBalancer Service

A Gateway's data plane is a `Service type=LoadBalancer` that your gateway
controller (kgateway, Cilium, Istio, …) provisions. The operator advertises that
Service exactly like any other LoadBalancer Service. **HTTPRoutes** do all the
L7 host/path routing behind that single advertised frontend — that stays the
gateway's job; the operator reads no `Gateway` or `HTTPRoute` objects.

So: mesh peers reach the Gateway at its advertised dualstack name on the listener
ports, and the Gateway routes to backends per its HTTPRoutes.

## Prerequisites

- The operator installed with a NetBird API key, and a `Network`/`NetworkRouter`/
  `DNSZone` (see [`../expose`](../expose/README.md)).
- A Gateway API implementation installed (its CRDs + a `GatewayClass`).

## Deploy

```shell
kubectl apply -f ./examples/gateway/gateway.yaml
```

## How it works

1. The gateway controller sees the `Gateway` and provisions a data-plane
   `Service type=LoadBalancer` (its name is implementation-specific), and your
   LB/IPAM gives it an ingress IP.
2. That Service is advertised by the operator — **default-on**, picking up the
   `netbird.io/network` / `dns-zone` / `groups` annotations from the `edge`
   namespace here — so the Gateway gets a `NetworkResource` + dualstack
   `DNSRecord` at `<generated-service>-edge.<zone>`.
3. `HTTPRoute`s attached to the Gateway route by host/path to backends. Mesh
   peers hit the Gateway's advertised name on `:80`/`:443`; the Gateway does the
   rest. The operator never looks at the routes.

## Stable DNS for the route hostnames

The operator auto-advertises the gateway's data-plane Service as
`<generated-service>-edge.<zone>` — an implementation-specific name, **not** the
hostnames your `HTTPRoute`s match (`app.kube.example.com`). To make the route
hostnames resolve over the mesh to the gateway, add records in the NetBird zone,
two ways:

- **Explicit — a `DNSRecord` CR.** Point the route hostname at the gateway's
  advertised name (or its LB IP):

  ```yaml
  apiVersion: netbird.io/v1alpha1
  kind: DNSRecord
  metadata: { name: app, namespace: edge }
  spec:
    zoneRef: { name: kube, namespace: netbird }
    name: app.kube.example.com                           # the HTTPRoute hostname
    type: CNAME
    content: <generated-service>-edge.kube.example.com   # the gateway's advertised name
  ```

- **Implicit — external-dns.** Run [external-dns](https://kubernetes-sigs.github.io/external-dns/)
  with the [external-dns-netbird-webhook](https://codeberg.org/ccbash-oss/external-dns-netbird-webhook);
  it reads `Gateway`/`HTTPRoute` hostnames and publishes them into the NetBird
  zone for you — no per-route `DNSRecord`.

> A NetBird zone must have a single owner. If external-dns manages a zone the
> operator also writes to (its LoadBalancer auto-records), run external-dns
> `policy: upsert-only` or give it its own zone, or the two fight and records
> flap.

## Notes

- **Controlling advertising:** annotate the gateway's namespace (shown here) or
  the generated data-plane Service directly. The mesh DNS label derives from that
  Service's name, which is implementation-specific.
- **Publishing publicly:** to put the Gateway behind the NetBird reverse proxy,
  point a `ReverseProxyService` (`mode: http`) at the gateway's data-plane
  Service — or expose individual backends as in [`../expose`](../expose/README.md).
- This is L7 only. For raw TCP (mail, databases) use an L4
  `ReverseProxyService` ([`../mail`](../mail/README.md)) — Gateway API L4
  (`TCPRoute`) is likewise just a LoadBalancer Service the operator advertises.

## Opt-in: the operator as the gateway (NetBird BYOP proxy)

Run the operator with `--enable-gateway-api` and it becomes a **Gateway API
controller** (`controllerName: netbird.io/gateway-controller`). It **creates and
owns its own `GatewayClass`** (default name `netbird`). Instead of standing aside,
it:

1. Deploys + enrols a NetBird **bring-your-own reverse proxy** via a
   `ReverseProxyCluster` (its own Deployment + `Service type=LoadBalancer` + token
   + DNS; cert-manager supplies the TLS cert).
2. **Translates every `HTTPRoute`** attached to a `Gateway` of that class into a
   `ReverseProxyService` (one per hostname; `backendRefs` → backends, reached
   directly at their in-cluster ClusterIP).

This replaces an in-cluster gateway like kgateway with a mesh-native proxy while
keeping your `Gateway`/`HTTPRoute` manifests. Author the `Gateway` with a
`spec.infrastructure.parametersRef` pointing at a `ReverseProxyClusterParameters`
in the Gateway's namespace (image/replicas/groups/private/serviceAnnotations).
Design + phasing:
[`../../docs/design/byop-gateway.md`](../../docs/design/byop-gateway.md). The
Gateway API CRDs are a cluster prerequisite (not shipped in the chart).
