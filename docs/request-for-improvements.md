# Request for Improvements

A prioritized backlog of features and usability work for the operator. This is a
*roadmap*, not a settled design — each item states the gap, a proposed shape, the
impact, and rough scope. Anything that would cross an architecture guard rail
says so; see [`architecture.md`](architecture.md) for the settled scope and the
guard rails a change must respect.

Ordering is by value, not effort. The two headline items — **access/policies**
and **validating webhooks** — are the ones that most change how the operator
feels to use.

## Current state (what these build on)

- 13 CRDs, driven by the generic mirror reconciler plus the composite Gateway /
  ReverseProxyCluster / ClusterProxy controllers (see the Implementation map in
  `architecture.md`).
- The only admission webhook is the Pod sidecar mutating webhook
  (`internal/webhook/v1/pod_webhook.go`); there are **no validating webhooks** on
  the `netbird.io` CRDs.
- The upstream NetBird client (`netbird@v0.73.1`) already exposes API surfaces
  the operator does **not** yet mirror: `policies`, `peers`, `ingress`,
  `posturechecks`, `event_streaming`.

---

## 1. Access, not just reachability — a Policy/access capability

**Priority: highest.** This is the biggest workflow gap.

**Gap.** A guard rail states it plainly: *advertising grants reachability, not
access — access stays gated by NetBird policies, which are admin-managed.* So a
user does the declarative half in Kubernetes (advertise a Service / expose a
route) and then hand-writes the matching NetBird **Policy** in the dashboard
before anyone can actually reach it. The operator owns half the workflow.

**Proposal.** The NetBird client already has `policies`. Two shapes, ideally both:

- A thin **`Policy` mirror CRD** (spec ≈ the NetBird policy body) for admins who
  want full control — same `mirror[T]` pattern as the other mirrors.
- **Inline access on the exposure objects** — an optional `access:` block on
  `ReverseProxyService`, `Network`, and/or the LoadBalancer advertise path
  ("these groups may reach this"), from which the operator synthesizes and owns
  a Policy. A Service then goes from "resolvable but blocked" to "usable" in one
  manifest.

**Impact.** Turns the operator from "half the workflow" into the whole workflow —
the most-requested-shaped gap.

**Scope / guard rails.** New CRD + reconciler (mirror pattern is established).
The synthesized-policy variant must follow the shared-ownership and
truthful-status Development guard rails (identity-keyed delete guards,
conflict-as-not-ready). Keep "reachability ≠ access" intact: access is *opt-in*,
never implied by advertising.

## 2. Validating & defaulting admission webhooks

**Priority: high — biggest usability win per line of code.**

**Gap.** With no validating webhook, a bad CR fails silently mid-reconcile and
only surfaces as a `Ready=False` condition minutes later. The feedback loop for
a typo is "apply, wait, go read a condition."

**Proposal.**
- **Validating webhooks** that reject obvious errors at `kubectl apply` with a
  clear message: a `ReverseProxyService` whose `domain` isn't under a registered
  proxy cluster; a `private: true` service missing `accessGroups`; a
  cross-namespace `zoneRef`/`networkRef` that the target hasn't consented to
  (see item 6); a `NetworkRouter` declaring both `peers.group` and
  `peers.deploy`.
- **Defaulting (mutating) webhooks** to keep specs terse: fill `port` from the
  backend Service's first port, default `mode`, etc.

**Impact.** Fast, local, obvious feedback instead of delayed reconcile-time
failures — the biggest *felt* quality improvement.

**Scope / guard rails.** Reuses the existing webhook scaffolding
(`internal/webhook/v1`, gated by `--enable-webhooks`). Validation must not
duplicate business logic that the reconciler already enforces — keep it to
statically-checkable invariants; the reconciler stays the source of truth for
anything requiring a NetBird API lookup.

## 3. Peers as first-class objects

**Priority: medium.**

**Gap.** The mesh is invisible from `kubectl`; peers live only in the dashboard.

**Proposal.** Mirror `peers` — at minimum read-only CRs / status surfacing
peers and their groups; optionally actions (approval, group assignment). Pairs
naturally with the access work (item 1) and posture checks (item 7).

**Impact.** Makes the mesh observable and, eventually, manageable from Kubernetes.

## 4. Gateway-API as a DNS source

**Priority: medium.**

**Gap.** The operator already runs the reverse proxy for HTTPRoutes but does not
publish those hostnames into the internal NetBird zone — deployments still run
external-dns for that job.

**Proposal.** Publish admitted HTTPRoute hostnames into the internal NetBird
zone, folding external-dns's role into the operator for the single-owner
NetBird-only zone case. Reuses the existing DNSRecord machinery.

**Impact.** Removes a moving part from NetBird-only deployments.

**Scope / guard rails.** Only for a zone the operator single-owns (the DNS
single-owner guard rail). Do not touch externally-owned zones — no split-horizon
logic.

## 5. Native NetBird Ingress

**Priority: medium (exploratory).**

**Gap.** The BYOP reverse-proxy path is the only HTTP exposure mechanism.

**Proposal.** Evaluate mirroring the NetBird `ingress` API as a lighter path for
simple HTTP exposure that doesn't need the full in-cluster proxy. May or may not
subsume part of the Gateway path — needs a spike to compare capabilities.

## 6. Cross-namespace reference consent (ReferenceGrant-style)

**Priority: medium — security fix that unlocks multi-tenancy.**

**Gap.** The mirror-CRD path resolves a `CrossNamespaceReference.Namespace` with
no boundary check (`internal/controller/mirror.go` `resolveZoneID`/
`resolveNetworkID`), so any namespace can reference any zone/network — while the
Gateway path already scopes cross-namespace attachment via `allowedRoutes`.
(Raised in the security review as a confused-deputy risk under delegated RBAC.)

**Proposal.** Bring the mirror path up to the Gateway path's model: same-namespace
by default, with explicit **target-side opt-in** (an allowed-consumer-namespaces
list on `DNSZone`/`Network`, or a ReferenceGrant-style object) before a foreign
namespace may attach.

**Impact.** Both a security hardening and a *feature* — it's what makes it safe
to delegate these CRDs to teams, unlocking self-service.

**Related.** Pin/allowlist `NetworkRouter.peers.deploy.image` (today an arbitrary
image runs privileged + hostNetwork) so tenant-delegated router deployment is
safe — same multi-tenancy theme.

## 7. Posture checks

**Priority: low (composes with access).**

**Proposal.** Mirror `posturechecks` and let them feed the access story from
item 1 (only compliant peers reach a service).

## 8. Operations & observability

**Priority: low — polish, high perceived quality.**

- Richer per-controller metrics and a consolidated "what did I create in
  NetBird" status (NetBird IDs, validation state) so operators can trust the
  translation at a glance.
- Explore the client's `event_streaming` surface for a NetBird event feed.
- A `kubectl netbird` plugin / aggregated view: every exposed service, its FQDN,
  its access groups, its health, in one command.

---

## Suggested sequencing

1. **Access / Policy** (item 1) — the workflow gap.
2. **Validating webhooks** (item 2) — usability per effort.
3. **Cross-namespace consent** (item 6) — safe delegation; also converts a known
   security finding into a multi-tenancy feature.
4. **Peers** (item 3) and **Gateway-API DNS source** (item 4) — reach.

Items 5, 7, 8 are opportunistic. Each item should land as its own change with a
design note under [`docs/design/`](design/) when it crosses a guard rail or adds
a CRD, per the contribution rules in [`CLAUDE.md`](../CLAUDE.md).
