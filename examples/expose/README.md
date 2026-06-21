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
