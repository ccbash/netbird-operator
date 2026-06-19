# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

NetBird Kubernetes Operator: reconciles NetBird network state from Kubernetes CRDs and exposes in-cluster Services over the NetBird mesh via the Gateway API. Kubebuilder (`go.kubebuilder.io/v4`) + controller-runtime; talks to the NetBird Management API through the upstream `netbirdio/netbird` REST client.

## Commands

- `make test-unit` — full unit/integration suite (downloads envtest binaries, writes `coverage.txt`).
- Single Ginkgo spec: `KUBEBUILDER_ASSETS="$(bin/setup-envtest use <k8s-ver> --bin-dir bin -p path)" go test ./internal/controller/ -run TestControllers -args --ginkgo.focus="<text>"`.
- `make test-e2e` — builds the image, runs `test/e2e` (needs Docker).
- `make lint` — `golangci-lint run ./...`. **The linter is the source of truth for style** (`.golangci.yml`).
- `make generate` — **run after any change to `api/` structs.** Regenerates deepcopy, apply-configurations, CRDs, and `docs/api-reference.md`.
- `make build` — runs `generate`, then builds a static linux binary into `bin/linux-<arch>/`.
- `NB_API_KEY=${API_KEY} make run` — run locally against the current kube-context (webhooks disabled).

CONTRIBUTING expects single-commit PRs linked to an issue, with `lint` + `generate` + `test-unit` green.

## Architecture

See `docs/architecture.md` for the **target redesign** (mirror CRDs + Gateway-API translation) now in progress. The current code still reflects the v0.10.x model described below.

- **`cmd/main.go` → `setupControllers`** gates registration: no NetBird API key → Management-API controllers are skipped; `--gateway-api-enabled=false` (default) → Gateway-API controllers are skipped. The Pod webhook (`internal/webhook/v1`) is gated by `--enable-webhooks`. All writes use server-side apply with `FieldOwner: "netbird-operator"`.
- **`internal/controller/` — two groups.** (1) Direct NetBird resources: `setupkey`, `group`, `networkrouter`, `networkresource`, `clusterproxy`. (2) Gateway-API exposure: `gatewayclass`, `gateway`, `httproute`, `tcproute`, `nbservicepolicy`. One `netbird` GatewayClass; **route kind selects behavior** (HTTPRoute → L7 reverse-proxy service, TCPRoute → L4 resource).
- **`internal/` shared.** `netbirdutil/` (ID resolution, error classification), `k8sutil/` (finalizers, owner refs), `gatewayutil/` (route→Gateway resolution), `netbirdmock/` (httptest fake of the Management API — extend `addHandler(...)` for new endpoints), `version/`.
- **`api/v1alpha1/`.** Hand-edit only `*_types.go`; the rest is generated. **Read `docs/api-reference.md` for fields — do not re-derive from structs.**

## Reconciler conventions (match when editing)

- Finalizers via `k8sutil.Finalizer("<kind>")`; `DeletionTimestamp != zero` → `reconcileDelete` that cleans the NetBird object (tolerating `netbird.IsNotFound`) before removing the finalizer.
- Status via flux `patch.NewSerialPatcher` + `conditions.MarkTrue(obj, ReadyCondition, ...)`; final patch `patch.WithStatusObservedGeneration{}`.
- Child objects (Secrets/Deployments) via generated apply-configurations + `client.ForceOwnership`, owned via `k8sutil.ControllerReference`.
- Reconcilers self-requeue (~15m) to re-check drift against the Management API.

## Boundaries & gotchas

- **Generated files are off-limits to hand-edit:** `api/v1alpha1/zz_generated.deepcopy.go`, `pkg/applyconfigurations/`, `config/crd/bases/`, `docs/api-reference.md`. Change the source, then `make generate`.
- **CRDs are served from the chart, not `config/`.** `make generate` copies them to `charts/netbird-operator/crds`; envtest loads them from there, so a stale chart after an API change surfaces as test failures.
- **NetBird DNS zone ownership must be exclusive.** Never point external-dns (or any DNS sync) at a NetBird zone the operator manages — records flap (created-then-deleted each reconcile).
- **One owner per NetworkResource.** Never hand-write a `NetworkResource` for a Service a route already exposes; dual ownership flaps the resource and its DNS A-record (`identical record already exists` conflict).
- **`netbird` GatewayClass is cluster-scoped and chart-managed** (`helm.sh/resource-policy: keep`); consumers create only the `Gateway`. The name is validated (`netbird`) but no controller branches on it.

## Testing

- Controller/webhook suites use **envtest** (real apiserver + etcd, no kubelet), loading CRDs from `charts/netbird-operator/crds` — run `make generate` first after API changes.
- NetBird behavior is faked by `internal/netbirdmock`; no live Management API in unit tests.

## Releases

SemVer. Minor releases get a `release/v0.X.x` branch cut from `main`; tagging triggers a GitHub Action that publishes the image and Helm chart to GHCR.
