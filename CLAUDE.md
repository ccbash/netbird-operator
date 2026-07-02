# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

NetBird Kubernetes Operator: mirrors NetBird API objects as Kubernetes CRDs and advertises `Service type=LoadBalancer` addresses over the NetBird mesh (with reverse-proxy exposure). Kubebuilder (`go.kubebuilder.io/v4`) + controller-runtime; talks to the NetBird Management API through the upstream `netbirdio/netbird` REST client.

## Commands

- `make test-unit` — full unit/integration suite (downloads envtest binaries, writes `coverage.txt`).
- Single Ginkgo spec: `KUBEBUILDER_ASSETS="$(bin/setup-envtest use <k8s-ver> --bin-dir bin -p path)" go test ./internal/controller/ -run TestControllers -args --ginkgo.focus="<text>"`.
- `make test-e2e` — builds the image, runs `test/e2e` (needs Docker).
- `make lint` — `golangci-lint run ./...`. **The linter is the source of truth for style** (`.golangci.yml`).
- `make generate` — **run after any change to `api/` structs.** Regenerates deepcopy, apply-configurations, CRDs (into `charts/netbird-operator/crds`), and `docs/api-reference.md`. controller-gen only writes — after **removing** a type, delete its stale CRD/applyconfig files by hand.
- `make build` — runs `generate`, then builds a static linux binary into `bin/linux-<arch>/`.
- `NB_API_KEY=${API_KEY} make run` — run locally against the current kube-context (webhooks disabled).

CONTRIBUTING expects single-commit PRs linked to an issue, with `lint` + `generate` + `test-unit` green.

## Architecture

See `docs/architecture.md` — **its "Guard rails" section is the review checklist; a PR crossing one needs an explicit rationale and a doc update in the same change.** Principle: **NetBird CRDs mirror NetBird API objects 1:1; the operator translates `Service type=LoadBalancer` addresses into NetBird reachability, and Gateway API manifests into reverse-proxy exposure.** The mesh routes the LB IP (allocated by Cilium LB-IPAM / MetalLB / a cloud LB), never ClusterIPs / the service CIDR; the only place a ClusterIP appears is as an in-cluster dial target of the in-cluster NetBird reverse proxy. Gateway API is the authoring layer only — the NetBird proxy is the data plane, never the operator.

- **`cmd/main.go` → `setupControllers`** registers everything when a NetBird API key is set (no key → skipped). `--advertise-loadbalancers` (default true) sets the LoadBalancer advertise default; `--enable-gateway-api` gates the Gateway controllers. The Pod webhook (`internal/webhook/v1`) is gated by `--enable-webhooks`. All writes use server-side apply with `FieldOwner: "netbird-operator"`.
- **`internal/controller/` — four controller shapes.** (1) **Mirror CRDs** via one generic `MirrorReconciler[T]` (`mirror.go`): `Network`, `NetworkResource`, `DNSZone`, `DNSRecord`, `ReverseProxyService` — each a per-kind `mirror[T]` adapter (`*_mirror.go`) supplying `apply` (upsert → record status id) and `del`. (2) **`NetworkRouter`** (`networkrouter_controller.go`): the NetBird router + routing peers — `peers.group` reuses an existing NetBird group (deploys nothing), `peers.deploy` runs a `hostNetwork` netbird-client DaemonSet. (3) **`LoadBalancer`** (`loadbalancer_controller.go`): watches `Service type=LoadBalancer` and (default-on, opt out via the `netbird.io/advertise` annotation on namespace/Service) emits a `NetworkResource` + dualstack `DNSRecord` per LB ingress IP family. (4) **Gateway API** (`gateway_controller.go` + `reverseproxycluster_controller.go`): GatewayClass (operator-owned, self-healing) / Gateway → owned `ReverseProxyCluster` (BYOP proxy Deployment + LB Service + DNS + NetBird custom domain) / HTTPRoute → owned `ReverseProxyService`s — fail-closed translation (Exact/Regex paths, weighted backends → `Accepted=False` with an accurate reason). `Group`/`SetupKey`/`ClusterProxy` keep bespoke controllers.
- **`internal/` shared.** `netbirdutil/` (group/zone/proxy-cluster resolution, error classification), `k8sutil/` (finalizers, owner refs), `netbirdmock/` (httptest fake of the Management API — extend `addHandler(...)` for new endpoints), `version/`. In the controller package, `serviceaddr.go` holds the IP-family fan-out + FQDN helpers.
- **`api/v1alpha1/`.** Hand-edit only `*_types.go`; the rest is generated. **Read `docs/api-reference.md` for fields — do not re-derive from structs.**

## Reconciler conventions (match when editing)

- A new mirror CRD = a `mirror[T]` adapter (`apply`/`del`) + a `NewXReconciler`; the generic reconciler handles finalizer, conditions, requeue and id bookkeeping.
- `apply` **GET-verifies** a recorded NetBird id before reusing it and recreates when it was deleted out of band (a plain `GET` returns a clean 404; `Update` may not). Reset a child's id when its parent's id changes.
- Finalizers via `k8sutil.Finalizer("<kind>")`; `DeletionTimestamp != zero` → `reconcileDelete` that cleans the NetBird object (tolerating `netbird.IsNotFound`) before removing the finalizer.
- Status via flux `patch.NewSerialPatcher` + `conditions.MarkTrue(obj, ReadyCondition, ...)`; final patch `patch.WithStatusObservedGeneration{}`. A not-ready dependency returns `errDependencyNotReady` → requeue, not a hard error. Not-ready branches reset the status fields they normally set (no stale printcolumns) but never the fields `reconcileDelete` deletes by.
- **Adoption implies shared ownership.** Delete guards for adopted NetBird objects compare the recorded status identity (`Status.ZoneID`/`ClusterAddress`/`DomainID` — the delete key), never a mutable spec field; the last CR standing deletes. An adopted object owned by another live CR is a conflict surfaced as `errDependencyNotReady`, never overwritten per reconcile (= flapping); unowned-stale state is repaired (delete+recreate — e.g. `ReverseProxyDomains` has no Update API).
- Child objects via generated apply-configurations + `client.ForceOwnership`, owned via `k8sutil.ControllerReference`. Child names must be collision-free by construction (hash-suffixed, see `routeChildName`); list-scanning validity checks scan the whole list (order-independent fail-close).
- Reconcilers self-requeue (~15m) to re-check drift against the Management API — so out-of-band NetBird deletions self-heal within that window.

## Boundaries & gotchas

- **Generated files are off-limits to hand-edit:** `api/v1alpha1/zz_generated.deepcopy.go`, `pkg/applyconfigurations/`, `config/crd/bases/`, `docs/api-reference.md`. Change the source, then `make generate`.
- **CRDs are served from the chart, not `config/`.** `make generate` copies them to `charts/netbird-operator/crds`; envtest loads them from there, so a stale chart after an API change surfaces as test failures.
- **NetBird DNS zone ownership must be exclusive.** Never point external-dns (or any DNS sync) at a NetBird zone the operator manages — records flap (created-then-deleted each reconcile).
- **LoadBalancer Service children are operator-owned.** The `LoadBalancer` controller owns the `NetworkResource`/`DNSRecord` for a Service (labeled `netbird.io/loadbalancer`), prunes them per IP family, and GC's them with the Service. Don't hand-write those for an advertised Service.
- **Cluster targets reference the cluster CNAME address**, not `ProxyCluster.Id` (a single proxy-node id → `netbird-proxy-<id>`). RBAC is hand-maintained in `charts/netbird-operator/templates/rbac.yaml` (no `controller-gen rbac`) — update it when a controller touches a new resource.

## Testing

- Controller/webhook suites use **envtest** (real apiserver + etcd, no kubelet), loading CRDs from `charts/netbird-operator/crds` — run `make generate` first after API changes. Set a LoadBalancer Service's `status.loadBalancer.ingress` by hand (no cloud controller in envtest).
- NetBird behavior is faked by `internal/netbirdmock`; no live Management API in unit tests.

## Releases

SemVer. Minor releases get a `release/v0.X.x` branch cut from `main`; tagging triggers a GitHub Action that publishes the image and Helm chart to GHCR.
