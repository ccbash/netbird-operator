# kubectl to the cluster API over NetBird

`ClusterProxy` puts the Kubernetes API server on the NetBird mesh, so you can
`kubectl` into the cluster over the tunnel with **no public API endpoint**. The
proxy authenticates the calling NetBird peer and impersonates a Kubernetes
user/group, so your existing **RBAC** decides what each peer may do — mapping
NetBird group membership onto cluster permissions.

See the [architecture notes](../../docs/architecture.md#cluster-api-proxy) for the
full flow.

## Prerequisites

- The operator installed with a NetBird API key (see [`../expose`](../expose/README.md)).
- A NetBird group whose members should get cluster access (`kubernetes-read` below).

## Deploy

```shell
kubectl apply -f ./examples/cluster-proxy/cluster-proxy.yaml
```

Then reach the cluster at the proxy's mesh name `<clusterName>.netbird-kubeapi-proxy`
(here `prod.netbird-kubeapi-proxy`) — point a kubeconfig server at it.

## How it works

- **`ClusterProxy`** runs a proxy Deployment (a NetBird peer, `replicas: 3` by
  default for HA) that fronts `apiServer`
  (`https://kubernetes.default.svc.cluster.local` by default).
- It uses the **`serviceAccountName`** ServiceAccount to **impersonate** the
  caller — so that SA is granted `impersonate` on `users`/`groups`/`userextras`/`uids`
  (the first `ClusterRole`/binding in the manifest).
- Authorization is plain RBAC: bind NetBird identities to roles with **`Group`**
  (or `User`) subjects. The example binds the NetBird group `kubernetes-read` to a
  read-only `ClusterRole`, so peers in that group get cluster-wide read.

## Notes

- `clusterName` is immutable — it forms the mesh DNS label.
- Grant more access by adding `ClusterRoleBinding`s with the relevant NetBird
  `Group`/`User` subjects; the impersonation `ClusterRole` itself stays read-only
  to the API (it only grants the right to impersonate).
- Set `spec.replicas` to scale the proxy peers (all share the one DNS label).
