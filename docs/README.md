# Quai documentation

Status: Draft · 2026-10-26

The plan for turning the Quai prototype into an enterprise product: a governed
self-service portal in front of infrastructure that customers already manage
as code. Every request becomes a reviewed change in the customer's Git
repositories; their pipelines apply it.

## Reading order

1. [Architecture overview](architecture/overview.md): where the prototype stands, the target architecture, the request flow and the invariants.
2. [Domain model](architecture/domain-model.md): the vocabulary and lifecycles used everywhere else.
3. How Quai connects to repositories and writes to them:
   - [Repository connections](design/01-repository-connections.md): connections, credentials, definitions, and how catalog items bind to repositories.
   - [Resource types and generators](design/02-resource-types-and-generators.md): locators, generators, formats, ownership, validation.
   - [File editing](design/03-file-editing.md): minimal-diff editing of YAML, JSON and HCL in Go.
   - [Git connectors](design/12-git-connectors.md): one connector per platform; Azure DevOps Server first, then Services.
4. [Roadmap](plan/roadmap.md) and [open questions](plan/open-questions.md). Tasks and epics are [GitHub issues](https://github.com/damhau/quai/issues) by milestone (M1 Pilot, M2 Beta, M3 GA); how autonomous sessions work through them is in [Phase 0: how autonomous sessions work](plan/phase-0-backlog.md).

## Map

| Section | Documents |
| --- | --- |
| Architecture | [overview](architecture/overview.md) · [domain model](architecture/domain-model.md) |
| Design | [01 repository connections](design/01-repository-connections.md) · [02 resource types and generators](design/02-resource-types-and-generators.md) · [03 file editing](design/03-file-editing.md) · [04 blueprints and change sets](design/04-blueprints-and-change-sets.md) · [05 delivery and approvals](design/05-delivery-and-approvals.md) · [06 runs, status and drift](design/06-runs-status-and-drift.md) · [07 day-2 actions and executors](design/07-day2-actions-and-executors.md) · [08 identity, access and tenancy](design/08-identity-access-tenancy.md) · [09 security and compliance](design/09-security-and-compliance.md) · [10 onboarding](design/10-onboarding.md) · [11 integrations](design/11-integrations.md) · [12 Git connectors](design/12-git-connectors.md) |
| Operations | [deployment and operations](operations/deployment-and-operations.md) |
| Engineering | [backend (Go)](engineering/backend.md) · [frontend (React)](engineering/frontend.md) · [testing](engineering/testing.md) · [migration from the prototype](engineering/migration-from-prototype.md) |
| Decisions | [ADR index](#decisions) |
| Plan | [roadmap](plan/roadmap.md) · [autonomous sessions](plan/phase-0-backlog.md) · [open questions](plan/open-questions.md) · [GitHub milestones](https://github.com/damhau/quai/milestones) |

## Decisions

| ADR | Decision | Status |
| --- | --- | --- |
| [0001](adr/0001-git-is-the-system-of-record.md) | Git is the system of record | Accepted |
| [0002](adr/0002-quai-is-the-approval-authority.md) | Quai is the approval authority, enforced by a required check | Accepted |
| [0003](adr/0003-definitions-live-next-to-the-iac.md) | Definitions live next to the IaC | Accepted |
| [0004](adr/0004-structured-generators-no-text-templates.md) | Structured generators, no text templates; plugins in WASM | Accepted |
| [0005](adr/0005-minimal-diff-file-editing.md) | Minimal-diff editing of human-maintained files | Accepted |
| [0006](adr/0006-go-backend-react-frontend.md) | Go backend, React frontend | Accepted |
| [0007](adr/0007-postgresql-for-state-and-jobs.md) | PostgreSQL for state and jobs | Accepted |
| [0008](adr/0008-pull-requests-by-default.md) | Pull requests by default | Accepted |
| [0009](adr/0009-cel-expressions.md) | CEL as the expression language | Accepted |
| [0010](adr/0010-git-connectors-azure-devops-first.md) | Per-platform Git connectors; Azure DevOps Server first, then Services | Accepted |

## Conventions

- Every document starts with its status (`Draft`, `Accepted`, `Superseded`) and date.
- A change that breaks one of the [invariants](architecture/overview.md#invariants) or reverses an accepted decision needs a new ADR that supersedes the old one.
- Examples in the docs use `quai.dev/v1beta1`, the definition format. The prototype's `quai.dev/v1alpha1` is not read by Quai; its examples are rewritten by hand (see [migration from the prototype](engineering/migration-from-prototype.md)).
