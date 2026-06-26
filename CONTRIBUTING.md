# Contributing

Thanks for your interest in contributing! This is a community-maintained,
**divergent fork** of `netbirdio/kubernetes-operator` (see the README for how it
differs). Contributions of all kinds are welcome — bug reports, fixes, docs, and
features.

Everyone participating is expected to follow our
[Code of Conduct](CODE_OF_CONDUCT.md).

## Where to start

- **Questions / ideas** — open a [Discussion](https://github.com/ccbash/kubernetes-operator/discussions).
- **Bugs** — open an [issue](https://github.com/ccbash/kubernetes-operator/issues/new/choose) with the bug template.
- **Features** — open a feature issue (or a Discussion) first; see the policy below.
- **Security vulnerabilities** — do **not** open a public issue. Follow the
  [Security Policy](SECURITY.md).

Good first contributions: improving docs, tightening tests, or fixing an issue
labelled `good first issue`.

## Acceptance policy

This project is **issue-first**: every PR should link to a bug or feature issue.
A PR that hasn't been discussed in an issue (or with a maintainer) may be closed
without further reason — please open the issue first so we can agree on the
approach before you invest time.

- Keep each PR to a **single, focused commit** (squash before review).
- **Test locally before submitting.** PRs from non-maintainers need maintainer
  approval to run CI, so a green local run is the fastest path to review.
- Update docs and tests alongside code.

## Development

The linter is the source of truth for style.

```bash
make lint        # golangci-lint
make generate    # regenerate deepcopy, CRDs, applyconfigs, docs/api-reference.md (after api/ changes)
make test-unit   # unit/integration suite (envtest)
make test-e2e    # e2e (needs Docker)
```

Run the operator locally against your current kube-context (webhooks disabled):

```bash
NB_API_KEY=${API_KEY} make run
```

A PR is ready when `lint`, `generate` (no diff), and `test-unit` are green.

## Sign-off and licensing

Contributions are accepted under the project's
[BSD-3-Clause license](LICENSE) — by contributing, you agree your contribution is
licensed under the same terms (inbound = outbound). There is **no CLA** and no
assignment of rights to any company.

We use the [Developer Certificate of Origin](https://developercertificate.org/)
(DCO): certify that you wrote the change (or have the right to submit it) by
signing off your commit:

```bash
git commit -s        # adds a "Signed-off-by: Your Name <you@example.com>" trailer
```

## Releases

SemVer. Each release with new features gets a new minor version; minor releases
are cut on a `release/v0.X.x` branch from `main` so bug fixes can be backported
and patch releases cut without pulling in new features.

1. For a new minor release, create the release branch from the latest `main`.
2. Create a [new release](https://github.com/ccbash/kubernetes-operator/releases/new)
   in GitHub, with a new tag and the **release branch** as the target.
3. Set the title to the release version and "Generate release notes".
4. Publish — the [release workflow](.github/workflows/release.yaml) then publishes
   the operator image and Helm chart to GHCR under `ghcr.io/ccbash`.
