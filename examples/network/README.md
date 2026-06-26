# Routing an arbitrary address into a NetBird network

This example routes a hand-picked address (an IP, CIDR, or domain) into a NetBird
network with the raw mirror CRDs — no Kubernetes `Service` involved. Use it when
the target isn't a cluster `Service` the operator can advertise for you: an
on-prem host, a database, a printer, a whole CIDR behind the routing peers.

For exposing a cluster `Service type=LoadBalancer` instead, see
[`../expose`](../expose/README.md).

## Prerequisites

- The operator installed with a NetBird API key (see [`../expose`](../expose/README.md)).
- An existing NetBird group for the routing peers (`prod-nodes` below) — the
  group your nodes' netbird joins. Or switch the `NetworkRouter` to
  `peers.deploy: {}` to have the operator run a netbird-client DaemonSet.

## Deploy

```shell
kubectl apply -f ./examples/network/networkrouter.yaml    # Network + NetworkRouter
kubectl apply -f ./examples/network/networkresource.yaml  # the routed address
```

## How it works

- **`Network`** mirrors a NetBird network 1:1.
- **`NetworkRouter`** binds routing peers to the network (`peers.group` reuses an
  existing group and deploys nothing; `peers.deploy` runs a `hostNetwork`
  netbird-client DaemonSet). `masquerade: true` SNATs traffic to the resources.
- **`NetworkResource`** routes one `address` (IP / CIDR / domain) into the
  network. Peers reach it once a NetBird **policy** grants their group access to
  the resource's `groups`.

## Notes

- `networkRef` is immutable — to move a resource to another network, recreate it.
- A `NetworkResource` makes the address routable but grants no access by itself;
  write a NetBird policy from the consuming peers' group to the resource's group.
- Resources self-heal: the operator re-checks them against the Management API
  (~15m), so an out-of-band NetBird deletion is recreated.
