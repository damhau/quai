# ADR 0010: Per-platform Git connectors, Azure DevOps first

Status: Accepted · 2026-10-26 · Amended 2026-10-29

## Context

Quai must work with the Git platforms customers already use. They differ in
everything beyond Git itself: authentication, pull request models, how a
required status is enforced, ownership rules, webhook security, pipelines and
API limits. The pilot customer uses Azure DevOps Server on-premises; other
prospects use Azure DevOps Services.

## Decision

- Each platform gets its own **connector**: a Go package in `internal/connectors/<platform>` implementing one capability-based interface. Connectors are compiled in, not loaded as plugins, because they hold credentials and call external APIs.
- Git transport is shared (the `git` CLI); connectors only supply credentials for it.
- Every connector must pass a shared **conformance suite** against a real instance of its platform before it ships.
- **Order:** Azure DevOps first, as one connector for both editions: **Azure DevOps Server (on-premises) first**, the M1 pilot's acceptance target, with **Azure DevOps Services** in the same phase or right after. Gitea is the container-based reference connector for tests. Then GitHub, then GitLab, then Bitbucket.
- The Azure DevOps connector is written against the lowest supported REST API version (7.0, Server 2022) and detects newer capabilities per instance.

Details: [Git connectors](../design/12-git-connectors.md).

## Consequences

- The core never branches on platform names; differences are expressed as capabilities (`enforced-status`, `auto-complete`, `signed-webhooks`…), and the UI explains what a missing capability means.
- Azure DevOps shapes the first implementation: PR statuses on iterations, status-check branch policies with an authorized identity, auto-complete, path-based required reviewers instead of `CODEOWNERS`, unsigned service hooks (handled by re-reading every object from the API).
- Server first means PAT authentication of a service account, internal CA certificates, collection-based URLs, possibly no network path for service hooks (polling mode), and API versions older than the Go SDK expects (the connector controls API versions itself).
- Conformance testing needs an Azure DevOps Server 2022 instance (lab VM or a test project on the pilot's Server) and an Azure DevOps Services sandbox organization.

## Amendment, 2026-10-29

The owner's decision: Phase 1 and the M1 pilot are accepted on **Azure DevOps Services**, and
running on a real Azure DevOps Server 2022 is a
[future improvement](../plan/roadmap.md#future-improvements) (#98), with no phase or milestone.
The rest stands: one connector for both editions, written against API 7.0 with Server's URL forms
and PAT authentication, so adding Server means verifying it on a real instance and fixing what
that finds. Where this record says Server first, Services now comes first.

## Alternatives considered

- **A generic Git-only integration with platform features added later.** Rejected: without pull requests and enforced statuses there is no binding approval.
- **Connectors as plugins (WASM or separate processes).** Rejected for now: they need credentials and network access, which is exactly what the plugin sandbox forbids. Revisit if third parties need to add platforms.
