# Security Policy

## Supported versions

This is a community-maintained fork. Security fixes target the **latest minor
release** (the current `release/v0.X.x` branch) and `main`. Older minors are not
maintained — please upgrade to the latest release before reporting.

| Version            | Supported          |
| ------------------ | ------------------ |
| latest `release/*` | :white_check_mark: |
| `main`             | :white_check_mark: |
| older releases     | :x:                |

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues,
Discussions, or pull requests.**

Report privately through one of:

- **GitHub Private Vulnerability Reporting** (preferred) — on the repository's
  **Security** tab → *Report a vulnerability*. This keeps the report private and
  tracked.
- **Email** — **kgr@ccbash.de**. Encrypt if you can; otherwise send a minimal
  report and we'll arrange a secure channel.

Please include, as far as you can:

- the affected version / commit and component,
- a description of the issue and its impact,
- steps to reproduce or a proof of concept,
- any suggested remediation.

## What to expect

- We aim to acknowledge a report within a few days.
- We'll confirm the issue, determine its severity, and keep you updated on
  remediation progress.
- We follow coordinated disclosure: please give us reasonable time to ship a fix
  before any public disclosure, and we'll credit you (if you wish) in the
  release notes.

## Scope

In scope: the operator code in this repository and the Helm chart shipped from
it. Out of scope: the upstream NetBird Management API and the
[upstream operator](https://github.com/netbirdio/kubernetes-operator), your own
cluster configuration, and third-party dependencies (report those upstream,
though we welcome a heads-up so we can bump them).
