# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

NetBird Kubernetes Operator: mirrors NetBird API objects as Kubernetes CRDs, advertises `Service type=LoadBalancer` addresses over the NetBird mesh, and translates Gateway API manifests into an in-cluster NetBird reverse proxy. Kubebuilder (`go.kubebuilder.io/v4`) + controller-runtime; talks to the NetBird Management API through the upstream `netbirdio/netbird` REST client.

**`docs/architecture.md` is the single source of truth for architecture** — read it before any structural or controller change:

- *Scope / layers / Implementation map* — what the operator is, what it deliberately is not, and where each piece lives in the code.
- *Guard rails* — the review checklist. **Settled decisions are not up for re-litigation; a change crossing a guard rail needs an explicit rationale in the PR and an update to that document in the same change.** The *Development* guard rails double as the reconciler conventions to match when editing (identity-keyed delete guards, conflict-as-not-ready instead of reconcile wars, order-independent fail-close, collision-free child names, truthful failure-branch status, idempotent renders, and the shared mechanics for mirror adapters/finalizers/status patching).

## Commands

- `make test-unit` — full unit/integration suite (downloads envtest binaries, writes `coverage.txt`).
- Single Ginkgo spec: `KUBEBUILDER_ASSETS="$(bin/setup-envtest use <k8s-ver> --bin-dir bin -p path)" go test ./internal/controller/ -run TestControllers -args --ginkgo.focus="<text>"`.
- `make test-e2e` — builds the image, runs `test/e2e` (needs Docker).
- `make lint` — `golangci-lint run ./...`. **The linter is the source of truth for style** (`.golangci.yml`).
- `make generate` — **run after any change to `api/` structs.** Regenerates deepcopy, apply-configurations, CRDs (into `charts/netbird-operator/crds`), and `docs/api-reference.md`. controller-gen only writes — after **removing** a type, delete its stale CRD/applyconfig files by hand.
- `make build` — runs `generate`, then builds a static linux binary into `bin/linux-<arch>/`.
- `NB_API_KEY=${API_KEY} make run` — run locally against the current kube-context (webhooks disabled).

CONTRIBUTING expects single-commit PRs linked to an issue, with `lint` + `generate` + `test-unit` green.

## Editing rules

- **Generated files are off-limits to hand-edit:** `api/v1alpha1/zz_generated.deepcopy.go`, `pkg/applyconfigurations/`, `config/crd/bases/`, `docs/api-reference.md`. Change the source (`api/v1alpha1/*_types.go`), then `make generate`.
- **Read `docs/api-reference.md` for CRD fields — do not re-derive them from structs.**
- **RBAC is hand-maintained** in `charts/netbird-operator/templates/rbac.yaml` (no `controller-gen rbac`) — update it in the same PR when a controller touches a new resource, and grant nothing more than needed.
- **CRDs are served from the chart, not `config/`.** `make generate` copies them to `charts/netbird-operator/crds`; envtest loads them from there, so a stale chart after an API change surfaces as test failures.
- When adding or changing a controller, hold it to the Development guard rails and pin new invariants with a test (unit for pure helpers in `*_helpers_test.go`, envtest spec otherwise) — that is how the current invariants stay enforced.

## Testing

- Controller/webhook suites use **envtest** (real apiserver + etcd, no kubelet), loading CRDs from `charts/netbird-operator/crds` — run `make generate` first after API changes. Set a LoadBalancer Service's `status.loadBalancer.ingress` by hand (no cloud controller in envtest).
- NetBird behavior is faked by `internal/netbirdmock`; no live Management API in unit tests. A mock 404 must be written via `util.WriteErrorResponse` (a plain-text 404 is unparseable → `netbird.IsNotFound` is false and reconciles hard-error). The `netbirdutil` list caches are per-client (30s TTL) — use `Controls.NewClient()` when a test must observe out-of-band state changes immediately.

## Releases

SemVer. Minor releases get a `release/v0.X.x` branch cut from `main`; tagging triggers a GitHub Action that publishes the image and Helm chart to GHCR.
