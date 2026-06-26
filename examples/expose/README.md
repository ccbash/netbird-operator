# Exposing a Service

This example sets up the NetBird overlay and exposes an Nginx
`Service type=LoadBalancer` over the mesh and through the NetBird reverse proxy.

Create the NetBird namespace and API-key secret.

```shell
kubectl create namespace netbird
kubectl -n netbird create secret generic netbird-mgmt-api-key --from-literal NB_API_KEY=${NETBIRD_API_KEY}
```

Install the operator.

```shell
helm upgrade --install --create-namespace -f ./examples/expose/values.yaml -n netbird netbird-operator ./charts/netbird-operator
```

Create the `Network`, its `NetworkRouter` and the `DNSZone`. The example reuses
an existing NetBird group as the routing peers (`peers.group`) — point it at the
group your nodes' netbird joins, or switch to `peers.deploy: {}` to have the
operator run a `hostNetwork` netbird-client DaemonSet instead.

```shell
kubectl apply -f ./examples/expose/network.yaml
```

Deploy Nginx as a `Service type=LoadBalancer` and publish it. Your cluster's LB
IPAM (Cilium LB-IPAM, MetalLB, …) allocates the ingress IP; the operator
advertises it automatically (a `NetworkResource` + a dualstack `DNSRecord` per
IP family). The `ReverseProxyService` then publishes it through the proxy.

```shell
kubectl apply -f ./examples/expose/nginx.yaml
```

## How it fits together

* The **LoadBalancer IP** — not the ClusterIP — is what gets routed into NetBird.
  Allocation is your LB's job; the operator owns the overlay and DNS.
* Advertising is **default-on**; opt a namespace or Service out with the
  annotation `netbird.io/advertise: "false"`. Advertising makes the name
  resolvable and the IP routable, but grants no access by itself.
* **For access, the resource needs a group and a policy.** Put advertised
  resources in NetBird groups with the `netbird.io/groups` annotation (or the
  operator's `--default-resource-groups`), then write a NetBird policy granting
  the consuming peers access to that group. Nothing is published through the
  proxy until you also author a `ReverseProxyService`.
* `ReverseProxyService` targets each backend Service's **dualstack DNS name**
  (`<svc>-<ns>.<zone>`), so IPv4/IPv6 is transparent. `private: true` makes the
  exposure NetBird-only instead of public.

## Non-HTTP backends (L4)

`mode: http` (the default) is an L7 reverse proxy. For non-HTTP backends —
mail (SMTP/IMAP/ManageSieve), databases, anything raw — use an L4 mode
(`tcp`/`tls`/`udp`) with an explicit `listenPort`; the proxy passes the
connection through and the backend terminates its own TLS.

A NetBird service has one `domain` and one `listenPort`, so to publish several
ports under **one hostname** write **one `ReverseProxyService` per port**, all
sharing the same `domain` and `proxyCluster` with distinct `listenPort`s:

```yaml
apiVersion: netbird.io/v1alpha1
kind: ReverseProxyService
metadata: { name: mail-smtps, namespace: mail }
spec:
  mode: tcp
  listenPort: 465
  domain: mail.example.com
  proxyCluster: gate.example.com
  proxyProtocol: true          # backend sees the real client IP (tcp/tls only)
  backends:
    - serviceRef: { name: mail }
      port: 465                # REQUIRED when the backend Service has >1 port
```

When the backend `Service` exposes more than one port (the usual mail case),
`backends[].port` is **required** — the operator refuses to guess rather than
silently target the Service's first port.

For mail and other backends that enforce SPF/DNSBL, greylist, or log the
client address, set `proxyProtocol: true` (tcp/tls only): the proxy prepends a
PROXY protocol v2 header so the backend sees the real client IP instead of the
proxy's. The backend must be configured to accept PROXY protocol on that port.

`private: true` and `accessGroups` are HTTP-only (the NetBird auto-ACL requires
`mode=http`). Gate L4 access with `accessRestrictions` (CIDR/geo) — e.g. limit
`allowedCidrs` to the NetBird range to keep an L4 service mesh-only.
