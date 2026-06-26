# Injecting a NetBird sidecar into Pods

A `SidecarProfile` adds a NetBird peer as a **sidecar container** to Pods that
match its selector, so individual workloads join the mesh directly (each Pod
becomes its own peer) without running netbird on the node. Enrollment uses a
`SetupKey`.

## Prerequisites

- The operator installed with a NetBird API key (see [`../expose`](../expose/README.md)).
- **The Pod webhook** — injection happens via a mutating webhook, gated by
  `--enable-webhooks` (on by default). If you disabled it, Pods are not modified.

## Deploy

```shell
kubectl apply -f ./examples/sidecar/sidecarprofile.yaml   # SetupKey + SidecarProfile
kubectl apply -f ./examples/sidecar/pod.yaml              # a Pod matching the selector
```

The `ubuntu` Pod matches `podSelector` (`app: ubuntu`), so the webhook injects
the netbird sidecar; the Pod joins the mesh as an ephemeral peer.

## How it works

- **`SetupKey`** mirrors a NetBird setup key. `ephemeral: true` means peers
  enrolled with it are cleaned up when they disconnect — right for Pods, which
  come and go.
- **`SidecarProfile`** injects the sidecar into Pods matching `podSelector`,
  enrolling them with `setupKeyRef`. Optional: `injectionMode`, `extraDNSLabels`,
  `containerOverride`.

## Notes

- Only Pods **created after** the profile exists are mutated — the webhook fires
  at admission, so restart/recreate existing Pods to inject them.
- Use an **ephemeral** setup key for Pods so disconnected peers don't accumulate.
- `extraDNSLabels` requires a setup key created with `allowExtraDnsLabels: true`.
