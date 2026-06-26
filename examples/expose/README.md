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

## Non-HTTP backends (L4) — e.g. mail on many ports

`mode: http` (the default) is an L7 reverse proxy. For non-HTTP backends —
mail (SMTP/IMAP/ManageSieve), databases, anything raw — use an L4 mode
(`tcp`/`tls`/`udp`) with an explicit `listenPort`; the proxy passes the
connection through and the backend terminates its own TLS.

### Two reachability paths (they coexist)

* **Mesh-internal** — any advertised `type=LoadBalancer` Service is already
  reachable by NetBird peers at its mesh name (`<svc>-<ns>.<zone>`) on **all**
  ports, with real client IPs and **no** `ReverseProxyService`. For internal-only
  mail this is all you need.
* **Public** (external clients / inbound `:25` from other mail servers) goes
  through the reverse proxy — one `ReverseProxyService` per port, below.

### Publishing many L4 ports under one hostname

NetBird allows **one service per domain**, and raw-TCP/UDP connections route by
**listen port** (no SNI). So to publish several ports under **one hostname**,
write **one `ReverseProxyService` per port**, all sharing the same `domain`,
`proxyCluster`, and distinct `listenPort`s. The operator registers each port
under a distinct per-port subdomain `<mode>-<listenPort>.<domain>` (shown in
`status.serviceDomain`); clients still connect to the shared host on each port.

```yaml
apiVersion: netbird.io/v1alpha1
kind: ReverseProxyService
metadata: { name: mail-smtps, namespace: mail }
spec:
  mode: tcp
  listenPort: 465              # required for L4; fixes the public port
  domain: mail.example.com     # shared public host (clients connect here)
  proxyCluster: gate.example.com
  proxyProtocol: true          # backend sees the real client IP (tcp/tls only)
  backends:
    - serviceRef: { name: mail }
      port: 465                # REQUIRED when the backend Service has >1 port
```

Repeat with `name: mail-smtp`/`listenPort: 25`, `name: mail-imaps`/`993`, etc.
The operator registers `tcp-465.mail.example.com`, `tcp-25.mail.example.com`,
`tcp-993.mail.example.com` … (distinct, so NetBird accepts them); clients keep
using `mail.example.com:465 / :25 / :993`.

Notes and prerequisites:

* `backends[].port` is **required** when the backend `Service` exposes more than
  one port (the usual mail case) — the operator refuses to guess rather than
  silently target the Service's first port.
* For backends that enforce SPF/DNSBL, greylist, or log the client address, set
  `proxyProtocol: true` (tcp/tls only): the proxy prepends a PROXY protocol v2
  header so the backend sees the real client IP. The backend must accept PROXY
  protocol on that port.
* The **proxy cluster must allow custom ports** and actually listen on them on
  its public ingress; otherwise NetBird rejects the service
  (`custom ports not supported on cluster …`).
* The `domain` host must resolve to the proxy cluster — be the cluster address,
  a subdomain of it, or a registered NetBird custom domain — with **public DNS**
  pointing at the cluster ingress. The synthesized per-port subdomains need no
  DNS of their own (clients never resolve them).
* Use `mode: tcp` (passthrough) even for implicit-TLS ports like 465/993 — the
  backend terminates TLS. `mode: tls` is SNI-terminated at the proxy and so
  cannot share one hostname across ports.

`private: true` and `accessGroups` are HTTP-only (the NetBird auto-ACL requires
`mode=http`). Gate L4 access with `accessRestrictions` (CIDR/geo) — e.g. limit
`allowedCidrs` to the NetBird range to keep an L4 service mesh-only.
