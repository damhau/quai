# ADR 0003: Definitions live next to the IaC

Status: Accepted · 2026-10-26

## Context

Quai has to know each repository's layout: where resources live, their shape,
how to edit them. That knowledge could live in Quai's database (configured in
the UI) or in the repository it describes.

## Decision

Resource types live in the repository they describe, under `.quai/`, reviewed
through the repository's own process (CODEOWNERS on `.quai/`). Blueprints that
span repositories live in a catalog repository. Repository connections
(credentials, delivery, runner) live in Quai, because they are about reaching
the repository, not about its content. Admin-owned floor rules on the
connection guarantee minimum approvals regardless of what types say.

## Consequences

- A layout change and its type change land in the same pull request, reviewed by the same people; `quai lint` in CI keeps them consistent.
- Definitions are versioned; every request records the definitions commit it was planned with.
- Whoever can merge to `.quai/` controls forms and rules, hence the CODEOWNERS check, floor rules and audit of definition changes.
- Quai must handle invalid definitions gracefully (keep the last valid version per type).

## Alternatives considered

- **Definitions in Quai's database.** Rejected: drifts from the repository, not reviewed with the code, lost if the database is lost.
- **A single central definitions repository for all IaC repositories.** Rejected for types (layout knowledge separated from the layout); kept for blueprints, which genuinely span repositories.
