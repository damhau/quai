# ADR 0010: Per-platform Git connectors, Azure DevOps first

Status: Accepted · 2026-10-26

## Context

Quai must work with the Git platforms customers already use. They differ in
everything beyond Git itself: authentication, pull request models, how a
required status is enforced, ownership rules, webhook security, pipelines and
API limits. The pilot customer uses Azure DevOps.

## Decision

- Each platform gets its own **connector**: a Go package in `internal/connectors/<platform>` implementing one capability-based interface. Connectors are compiled in, not loaded as plugins, because they hold credentials and call external APIs.
- Git transport is shared (the `git` CLI); connectors only supply credentials for it.
- Every connector must pass a shared **conformance suite** against a real instance of its platform before it ships.
- **Order:** Azure DevOps first (Services and Server), with Gitea as the container-based reference connector for tests; then GitHub; then GitLab; then Bitbucket.

Details: [Git connectors](../design/12-git-connectors.md).

## Consequences

- The core never branches on platform names; differences are expressed as capabilities (`enforced-status`, `auto-complete`, `signed-webhooks`…), and the UI explains what a missing capability means.
- Azure DevOps shapes the first implementation: PR statuses on iterations, status-check branch policies with an authorized identity, auto-complete, path-based required reviewers instead of `CODEOWNERS`, unsigned service hooks (handled by re-reading every object from the API).
- Conformance testing needs access to real platform instances: an Azure DevOps Services sandbox organization, and an Azure DevOps Server instance if the pilot runs Server.

## Alternatives considered

- **A generic Git-only integration with platform features added later.** Rejected: without pull requests and enforced statuses there is no binding approval.
- **Connectors as plugins (WASM or separate processes).** Rejected for now: they need credentials and network access, which is exactly what the plugin sandbox forbids. Revisit if third parties need to add platforms.
